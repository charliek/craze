package modeltable

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// The ChatGPT plan in the model table (plan 033 §3.11, P22, P34).
//
// Its provider, "chatgpt" on the chatgpt driver, ships in the catalog like any
// other, but its models do not: they are the account's own list, which the
// sign-in (internal/chatgptauth) fetches from the plan's API and caches beside
// the model files, bound to the account that fetched it. A load adds each
// model of that list to the catalog it merges over (withDiscovered), so the
// user's models.toml can override one as it overrides a shipped model, and the
// catalog's [chatgpt_defaults] adds craze's own per-model settings to the
// list's.
//
// It is funded by the sign-in, never by a key (resolveKey): a model on it
// resolves while the sign-in's token file is there and its registration says
// the account granted plan usage, and it is ErrNotSignedIn or
// ErrPlanUsageDisabled otherwise. Neither reading touches a token: a stat of
// the token file, and the registration, which is not secret.
//
// The harness may not import internal/chatgptauth (D-02, P31), so the files'
// names and the two JSON shapes read here are a contract with it (plan 033
// X120): a test in internal/agent, which imports both, pins the names, and
// chatgptauth's Models type documents its shape as this package's to read.

const (
	// ChatGPTProvider is the ChatGPT plan's provider id: the one id a
	// provider on DriverChatGPT may have (validateProvider). A provider of the
	// user's own on that driver under any other id is dropped with a warning,
	// so the sign-in never funds an entry it was not made for.
	ChatGPTProvider = "chatgpt"

	// ChatGPTAliasPrefix begins every plan model's alias: chatgpt/<slug>, so
	// no plan model can collide with an OpenRouter GPT alias (P22).
	ChatGPTAliasPrefix = ChatGPTProvider + "/"

	// chatgptNameSuffix ends every plan model's name: the "Using ChatGPT
	// plan" indicator OpenAI's docs ask for, carried by the name so no picker
	// or protocol needs a field for it (P22).
	chatgptNameSuffix = " (ChatGPT plan)"

	// AuthSignIn is Resolved.Auth for a model the sign-in funds: the driver
	// takes its bearer from the process's token source, not from APIKey.
	AuthSignIn = "signin"

	// The sign-in's files under the native directory (chatgptauth's names).
	ChatGPTAuthDir    = "auth"                // <dir>/auth, 0700: every file of the sign-in but the model list
	ChatGPTTokenFile  = "chatgpt.json"        // <dir>/auth/chatgpt.json: the tokens (secret: only ever stat'ed here)
	ChatGPTClientFile = "chatgpt-client.json" // <dir>/auth/chatgpt-client.json: the registration (not secret)
	ChatGPTModelsFile = "chatgpt-models.json" // <dir>/chatgpt-models.json: the account's model list (not secret)

	// chatgptModelsVersion is the one model-list version this package reads.
	chatgptModelsVersion = 1

	// The most of each file read: the registration is a few hundred bytes and
	// the slim model list a few kilobytes (chatgptauth's own bounds).
	maxChatGPTClientFile = 64 << 10
	maxChatGPTModelsFile = 1 << 20
)

// chatgptEfforts are the efforts a ChatGPT plan request can carry, the
// Responses route's own list (plan 033 §3.9): never "ultra", which the
// account's list offers some models but which is codex's multi-agent mode,
// not a request value. A discovered model's efforts are its list's, less any
// other.
var chatgptEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// ChatGPTEfforts are the efforts a ChatGPT plan request can carry, in order:
// package llm's EffortOptions holds its own copy of the list, and a test there
// pins the two together.
func ChatGPTEfforts() []string { return slices.Clone(chatgptEfforts) }

// chatgptSlug is what a plan model's slug must look like to be named: an
// identifier, as chatgptauth keeps them (letters, digits and "_.:-", at most
// 64), starting with a letter or digit. Anything else is skipped, never
// shown.
var chatgptSlug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

var (
	// ErrNotSignedIn is Resolve's error for a model on the ChatGPT plan when
	// nobody is signed in: there is no token file in the table's directory, or
	// no registration beside it (plan 033 §3.11, P34). It unwraps to
	// ErrNoAPIKey — the sign-in is the plan's key — so every caller that
	// treats an unfunded model as unfunded does so here too.
	ErrNotSignedIn error = &fundingError{msg: "modeltable: not signed in to ChatGPT"}

	// ErrPlanUsageDisabled is Resolve's error for a model on the ChatGPT plan
	// when the account signed in without granting craze plan usage (the
	// chatgpt.tokens.use.direct scope): signed in, but not funded (P34). It
	// unwraps to ErrNoAPIKey too.
	ErrPlanUsageDisabled error = &fundingError{msg: "modeltable: ChatGPT plan usage is off for craze"}

	// ErrOtherAccount is Resolve's error for a model the table learned from
	// the ChatGPT plan's list of an account that is no longer the one signed
	// in (plan 033 C14r, r12 #4): the list was bound to the registration the
	// table loaded with, and the registration has changed since — another
	// account signed in while the process runs — so the model may be none the
	// account signed in now can use, and its list may order the plan's models
	// otherwise. It unwraps to ErrNoAPIKey: the model is not funded. A table
	// loaded anew offers the account's own list once it is fetched.
	//
	// It is the driver's error too, at each request on such a model
	// (Resolved.Account; plan 033 C14r2, review r13 d): a session, or a
	// sub-agent, opened on the model before the switch resolved it then, and
	// its every turn, summary and wake would otherwise send with whatever
	// account's token the process's sign-in holds now.
	ErrOtherAccount error = &fundingError{msg: "modeltable: that ChatGPT plan model is from another account's list"}
)

// fundingError is a sign-in's reason a model is not funded: a fixed text,
// and ErrNoAPIKey beneath it.
type fundingError struct{ msg string }

func (e *fundingError) Error() string { return e.msg }
func (e *fundingError) Unwrap() error { return ErrNoAPIKey }

// chatgptClient is what this package reads of chatgpt-client.json, the
// registration: the account and issued client the model list must be bound
// to, and whether plan usage was granted. Unknown keys are ignored, so the
// email and the notice flag never matter here.
type chatgptClient struct {
	ClientID  string `json:"client_id"`
	Subject   string `json:"subject"`
	PlanUsage bool   `json:"plan_usage"`
}

// readChatGPTClient is dir's registration, and false when there is none this
// package can read: no file, or one that is not a registration.
func readChatGPTClient(dir string) (chatgptClient, bool) {
	b, err := readSmallFile(filepath.Join(dir, ChatGPTAuthDir, ChatGPTClientFile), maxChatGPTClientFile)
	if err != nil {
		return chatgptClient{}, false
	}
	var c chatgptClient
	if json.Unmarshal(b, &c) != nil {
		return chatgptClient{}, false
	}
	return c, true
}

// errNotSmallFile is readSmallFile's refusal of anything but a regular file
// within its bound.
var errNotSmallFile = errors.New("not a regular file of the expected size")

// readSmallFile reads path, a regular file of at most max bytes, without
// following a symlink (the sign-in never writes one, and refuses one) and
// without waiting on a FIFO (O_NONBLOCK, as readRecentBytes).
func readSmallFile(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > max {
		return nil, errNotSmallFile
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errNotSmallFile
	}
	return b, nil
}

// signInVia is how dir's ChatGPT sign-in funds the plan now (P34): signed in
// — the token file a non-empty regular file, and the registration granting
// plan usage; plan usage disabled — a registration that says the account
// declined it; and otherwise nothing (KeyNone): no registration, one that
// cannot be read, or no token file. Only the registration is read and the
// token file stat'ed: no token is ever read here.
func signInVia(dir string) KeySource {
	via, _ := signInState(dir)
	return via
}

// signInState is signInVia with the registration it read: the account that
// is signed in, which Resolve holds a discovered model's list to (r12 #4).
func signInState(dir string) (KeySource, Account) {
	if dir == "" {
		return KeyNone, Account{}
	}
	c, ok := readChatGPTClient(dir)
	if !ok {
		return KeyNone, Account{}
	}
	account := Account{Subject: c.Subject, ClientID: c.ClientID}
	info, err := os.Lstat(filepath.Join(dir, ChatGPTAuthDir, ChatGPTTokenFile))
	tokens := err == nil && info.Mode().IsRegular() && info.Size() > 0
	switch {
	case tokens && c.PlanUsage:
		return KeySignedIn, account
	case !c.PlanUsage && (tokens || c.ClientID != "" || c.Subject != ""):
		return KeyPlanDisabled, account
	}
	return KeyNone, account
}

// Account names a ChatGPT account's registration as the model list is bound
// to it: the subject and the issued client id (P34). A model the table
// learned from that list carries it (Resolved.Account), and its driver sends
// no request with another account's token (plan 033 C14r2, review r13 d).
// The zero Account binds nothing.
type Account struct {
	Subject, ClientID string
}

// signInError is the reason a model on provider id is not funded when the
// sign-in says via (not KeySignedIn).
func signInError(id string, via KeySource) error {
	if via == KeyPlanDisabled {
		return fmt.Errorf("%w (provider %q)", ErrPlanUsageDisabled, id)
	}
	return fmt.Errorf("%w (provider %q)", ErrNotSignedIn, id)
}

// chatgptModelsDoc is chatgpt-models.json as this package reads it
// (chatgptauth.Models): the account it is bound to and the list, each entry
// decoded on its own so one that does not decode costs only itself.
type chatgptModelsDoc struct {
	Version  int               `json:"version"`
	Subject  string            `json:"subject"`
	ClientID string            `json:"client_id"`
	Models   []json.RawMessage `json:"models"`
}

// chatgptModelEntry is one model of the list (chatgptauth.Model).
type chatgptModelEntry struct {
	Slug              string   `json:"slug"`
	DisplayName       string   `json:"display_name"`
	ContextWindow     int      `json:"context_window"`
	Efforts           []string `json:"efforts"`
	DefaultEffort     string   `json:"default_effort"`
	InputModalities   []string `json:"input_modalities"`
	Priority          int      `json:"priority"`
	ParallelToolCalls *bool    `json:"parallel_tool_calls"`
	// MinClientVersion is the least client_version the server says the model
	// needs (plan 034 §3.1); "" when the list does not say.
	MinClientVersion string `json:"minimal_client_version"`
}

// maxMinVersionParts is how many dotted parts a model's minimum may have
// before it is read as malformed. The catalog's own pin is held to at most
// three (validPinVersion); a server's minimum is read more leniently, since a
// four-part one would still compare correctly and refusing it would only
// offer a model the pin may not meet.
const maxMinVersionParts = 4

// parseVersion reads a dotted numeric version: one to maxParts parts of one
// to six ASCII digits each (leading zeros allowed), the parts as numbers. A
// part over six digits, any other character, an empty part or an empty
// string is malformed (ok false).
func parseVersion(s string, maxParts int) (parts []int, ok bool) {
	if s == "" {
		return nil, false
	}
	for _, p := range strings.Split(s, ".") {
		if len(p) == 0 || len(p) > 6 || len(parts) == maxParts {
			return nil, false
		}
		n := 0
		for _, c := range []byte(p) {
			if c < '0' || c > '9' {
				return nil, false
			}
			n = n*10 + int(c-'0')
		}
		parts = append(parts, n)
	}
	return parts, true
}

// validPinVersion says s is a catalog pin: dotted numeric, one to three
// parts of one to six digits (plan 034 Q1).
func validPinVersion(s string) bool {
	_, ok := parseVersion(s, 3)
	return ok
}

// minimumMet says a model whose minimum client version is min may be offered
// to a client at pin (plan 034 Q3, the downgrade-safe read): equal passes,
// missing parts are 0 (0.160 is 0.160.0), and a missing or malformed min, or
// a malformed pin, passes — fail-open, because the server already filtered
// the list for the version it was fetched with and a value this package
// cannot read is no reason to hide a model the server offered.
func minimumMet(min, pin string) bool {
	m, ok := parseVersion(min, maxMinVersionParts)
	if !ok {
		return true
	}
	p, ok := parseVersion(pin, maxMinVersionParts)
	if !ok {
		return true
	}
	for i := 0; i < max(len(m), len(p)); i++ {
		var a, b int
		if i < len(m) {
			a = m[i]
		}
		if i < len(p) {
			b = p[i]
		}
		if a != b {
			return a < b
		}
	}
	return true
}

// discovered is what withDiscovered added to a catalog: the alias of each
// model it learned from the account's list, ranked by its place in that list
// (priority order), which is the order Choices lists them in; and the account
// the list is bound to, which the table keeps so Resolve funds those models
// only while that account is the one signed in (r12 #4).
type discovered struct {
	rank    map[string]int
	account Account
}

// withDiscovered adds the ChatGPT plan's models to cat, the merge's own copy
// of the catalog, before the user's files are laid over it (plan 033 §3.11):
// only when cat has the chatgpt provider on its driver, dir has a
// registration, and dir's model list is readable and bound to that
// registration's account — the same subject and issued client id (P34), so a
// later sign-in never inherits another account's list. A list that is
// missing, or bound to another account, adds nothing and says nothing (the
// next session's open fetches the account's own in the background); one that
// cannot be read as a list is one warning, and so is each entry skipped.
//
// Each entry becomes the model chatgpt/<slug> on the chatgpt provider: wire
// model the slug; name its display name (or [chatgpt_defaults]' name for the
// slug) and " (ChatGPT plan)"; its context window; its efforts — the list's,
// or the defaults' that the list also offers — less any a request cannot
// carry (never "ultra"); its default effort craze's, else the list's, when it
// is one of those; vision when the list takes image input; parallel tool calls
// as the list says (nil when it does not); no cost — the plan is not billed
// per token; and the defaults' tool profile. An entry whose
// minimal_client_version is above the catalog's models_client_version is left
// out without a warning (a downgrade-safe read, plan 034 Q3); a missing or
// unreadable minimum keeps it.
func withDiscovered(cat *Catalog, dir string) (*discovered, []string) {
	if p, ok := cat.Providers[ChatGPTProvider]; !ok || p.Driver != DriverChatGPT || dir == "" {
		return nil, nil
	}
	client, ok := readChatGPTClient(dir)
	if !ok || client.ClientID == "" {
		return nil, nil
	}
	path := filepath.Join(dir, ChatGPTModelsFile)
	b, err := readSmallFile(path, maxChatGPTModelsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	unreadable := fmt.Sprintf("modeltable: %s is not a ChatGPT plan model list this craze reads; the plan's models are not offered until it is fetched again", path)
	if err != nil {
		return nil, []string{unreadable}
	}
	var doc chatgptModelsDoc
	if json.Unmarshal(b, &doc) != nil || doc.Version != chatgptModelsVersion {
		return nil, []string{unreadable}
	}
	if doc.Subject != client.Subject || doc.ClientID != client.ClientID {
		return nil, nil // another account's list: ignored (P34)
	}

	type entry struct {
		i int
		m chatgptModelEntry
	}
	var entries []entry
	var warnings []string
	skip := func(i int, slug, reason string) {
		what := fmt.Sprintf("models[%d]", i)
		if chatgptSlug.MatchString(slug) {
			what += fmt.Sprintf(" (%q)", slug)
		}
		warnings = append(warnings, fmt.Sprintf("modeltable: %s: %s is skipped: %s", path, what, reason))
	}
	for i, raw := range doc.Models {
		var m chatgptModelEntry
		if err := json.Unmarshal(raw, &m); err != nil {
			skip(i, "", "it is not a model entry")
			continue
		}
		entries = append(entries, entry{i, m})
	}
	// The list's own order is its priority (chatgptauth sorts it so); the
	// file is sorted again, stably, in case anything else wrote it.
	sort.SliceStable(entries, func(a, b int) bool { return entries[a].m.Priority < entries[b].m.Priority })

	d := &discovered{rank: map[string]int{}, account: Account{Subject: client.Subject, ClientID: client.ClientID}}
	for _, e := range entries {
		m := e.m
		alias := ChatGPTAliasPrefix + m.Slug
		switch {
		case !chatgptSlug.MatchString(m.Slug):
			skip(e.i, "", "its slug is not one craze can name")
			continue
		case m.DisplayName != "" && !shownText(m.DisplayName, 128):
			skip(e.i, m.Slug, "its display name is not one line of text")
			continue
		case m.ContextWindow < 0:
			skip(e.i, m.Slug, "its context window is negative")
			continue
		case hasModel(cat.Models, alias):
			skip(e.i, m.Slug, "the model is listed twice, or its alias is already a model")
			continue
		case !minimumMet(m.MinClientVersion, cat.ChatGPT.ModelsClientVersion):
			// A list fetched by a newer craze whose pin is higher than this
			// one's: an older binary never offers what it has not verified
			// (plan 034 Q3). Silent, as a model the account does not list is.
			continue
		}
		model := discoveredModel(m, cat.ChatGPT.Models[m.Slug])
		if err := validateModel(path, alias, model, cat.Providers); err != nil {
			skip(e.i, m.Slug, err.Error())
			continue
		}
		cat.Models[alias] = model
		d.rank[alias] = len(d.rank)
	}
	return d, warnings
}

// discoveredModel is the model a list entry is, with craze's defaults for its
// slug (withDiscovered's rules).
func discoveredModel(m chatgptModelEntry, def ChatGPTModelDefaults) Model {
	offered := m.Efforts
	if def.Efforts != nil {
		offered = slices.DeleteFunc(slices.Clone(def.Efforts), func(e string) bool { return !slices.Contains(m.Efforts, e) })
	}
	var efforts []string
	for _, e := range offered {
		if slices.Contains(chatgptEfforts, e) && !slices.Contains(efforts, e) {
			efforts = append(efforts, e)
		}
	}
	effort := ""
	for _, e := range []string{def.DefaultEffort, m.DefaultEffort} {
		if e != "" && slices.Contains(efforts, e) {
			effort = e
			break
		}
	}
	return Model{
		Provider:          ChatGPTProvider,
		WireModel:         m.Slug,
		Name:              cmp.Or(def.Name, m.DisplayName, m.Slug) + chatgptNameSuffix,
		ContextWindow:     m.ContextWindow,
		Efforts:           efforts,
		DefaultEffort:     effort,
		Vision:            slices.Contains(m.InputModalities, "image"),
		ToolProfile:       def.ToolProfile,
		ParallelToolCalls: clonePtr(m.ParallelToolCalls),
	}
}

// shownText says s is one line of at most max bytes of UTF-8 with no control
// character: a picker row, never an escape sequence (chatgptauth's own rule
// for a model's name).
func shownText(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}
