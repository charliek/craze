package modeltable

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The Anthropic account's model list in the model table (plan 044 §3.5,
// §3.6, P7, P10, P12).
//
// internal/anthropicmodels fetches the list (GET /v1/models, through
// llm/messagesapi) and caches it beside the model files as
// anthropic-models.json, bound to the key that fetched it by the key's id
// (AnthropicKeyID). A load reads it (withAnthropicList) only when that id is
// the id of the key the provider is funded by at load — so a list fetched
// with another key, which may be another account's, is never offered with
// this one — and then the list is authoritative: the seeds it names stand as
// the catalog ships them, the ones it does not name are hidden, and each id
// it names with no seed becomes a model of its own, unpriced, with settings
// derived from the entry. The fetcher may import this package (it is outside
// the harness), so the file's name, version, shape and the key's id are
// defined here, and a test there writes the file and reads it back through
// ReadAnthropicModels and a load (the shape pin, P19).

const (
	// AnthropicModelsFile is the account's model list, <dir>/
	// anthropic-models.json (not secret: it holds the key's id, never the
	// key).
	AnthropicModelsFile = "anthropic-models.json"

	// AnthropicModelsVersion is the one list version this package reads and
	// the fetcher writes.
	AnthropicModelsVersion = 1

	// MaxAnthropicModelsFile is the most of the list file read, and the most
	// the fetcher writes (P10): the slim list is a few kilobytes.
	MaxAnthropicModelsFile = 1 << 20

	// AnthropicAliasPrefix begins every Anthropic model's alias:
	// anthropic/<wire id> (O2), the seeds' and the list's alike.
	AnthropicAliasPrefix = AnthropicProvider + "/"

	// anthropicDeprecated ends the name of a model the account's list says is
	// deprecated (plan 044 §3.5).
	anthropicDeprecated = " (deprecated)"

	// anthropicMaxOutput is the most max_output_tokens craze derives from a
	// list entry's max_tokens: a request's ceiling, not the model's (the
	// seeds carry the same, min(64000, max_tokens)).
	anthropicMaxOutput = 64000

	// anthropicKeyIDLen is how many hex digits of the key's SHA-256 its id
	// keeps: enough to tell two keys apart, too few to say anything of one.
	anthropicKeyIDLen = 12
)

// anthropicModelID is what a list entry's id must look like to be a model
// craze names: the Messages API client's own rule for an id it keeps.
var anthropicModelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// anthropicGetenv is how a load reads the variables that fund the Anthropic
// provider, to bind the account's list to the key (P7): os.Getenv, which a
// test replaces. A key exported after the load is read at the next one.
var anthropicGetenv = os.Getenv

// AnthropicModels is anthropic-models.json (plan 044 §3.6): the account's
// model list, slim, the time it was fetched, and the id of the key that
// fetched it (AnthropicKeyID). The fetcher writes it whole, complete and
// non-empty only; Models in the API's own order.
type AnthropicModels struct {
	Version   int              `json:"version"`
	FetchedAt time.Time        `json:"fetched_at"`
	KeyID     string           `json:"key_id"`
	Models    []AnthropicModel `json:"models"`
}

// AnthropicModel is one model of the list as the file keeps it: the fields of
// the Messages API client's ModelInfo, in its JSON (the shape pin keeps the
// two together).
type AnthropicModel struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
	// Line is the model's family, as the API names it.
	Line           string `json:"line,omitempty"`
	MaxInputTokens int64  `json:"max_input_tokens,omitempty"`
	MaxTokens      int64  `json:"max_tokens,omitempty"`
	// Efforts are the effort levels the model supports; none for a model
	// without effort.
	Efforts    []string `json:"efforts,omitempty"`
	ImageInput bool     `json:"image_input"`
	// AdaptiveThinking is whether the model takes adaptive thinking; nil
	// when the list did not say.
	AdaptiveThinking *bool     `json:"adaptive_thinking,omitempty"`
	Lifecycle        string    `json:"lifecycle,omitempty"`
	RetiresAt        time.Time `json:"retires_at,omitzero"`
}

// AnthropicKeyID is the id the account's list is bound to for key (P7): the
// first 12 hex digits of the SHA-256 of the key, surrounding whitespace
// trimmed as every reader of a key trims it. It names the key without being
// it, so the list file holds no secret.
func AnthropicKeyID(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])[:anthropicKeyIDLen]
}

// errNotAnthropicList is a list file this craze cannot read as one.
var errNotAnthropicList = errors.New("not an Anthropic model list this craze reads")

// ReadAnthropicModels is dir's account model list as a load reads it: the
// file at most MaxAnthropicModelsFile, a regular file, never through a
// symlink, of AnthropicModelsVersion, and every entry that decodes as a
// model — an entry that does not is left out, as a load leaves it out. No
// file is fs.ErrNotExist. A list with no model left is read as it is: a load
// takes no such list as the account's (applyAnthropicList).
func ReadAnthropicModels(dir string) (*AnthropicModels, error) {
	doc, _, _, err := readAnthropicModels(filepath.Join(dir, AnthropicModelsFile))
	return doc, err
}

// readAnthropicModels is ReadAnthropicModels on path, with each kept entry's
// index in the file (index, beside doc.Models) and the index of each entry
// left out because it does not decode as a model (skipped).
func readAnthropicModels(path string) (doc *AnthropicModels, index, skipped []int, err error) {
	b, err := readSmallFile(path, MaxAnthropicModelsFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil, err
		}
		return nil, nil, nil, errNotAnthropicList
	}
	var raw struct {
		Version   int               `json:"version"`
		FetchedAt time.Time         `json:"fetched_at"`
		KeyID     string            `json:"key_id"`
		Models    []json.RawMessage `json:"models"`
	}
	if json.Unmarshal(b, &raw) != nil || raw.Version != AnthropicModelsVersion {
		return nil, nil, nil, errNotAnthropicList
	}
	doc = &AnthropicModels{Version: raw.Version, FetchedAt: raw.FetchedAt, KeyID: raw.KeyID}
	for i, r := range raw.Models {
		var m AnthropicModel
		if json.Unmarshal(r, &m) != nil {
			skipped = append(skipped, i)
			continue
		}
		doc.Models = append(doc.Models, m)
		index = append(index, i)
	}
	return doc, index, skipped, nil
}

// anthropicKeyAtLoad is the key the Anthropic provider is funded by as the
// load sees it (P7): the first variable of its env_keys — the user's, when
// their providers.toml writes them, else the catalog's — set to a usable key,
// read through anthropicGetenv, else the entry's inline api_key; "" for
// none. It is Resolve's rule (resolveKey) over the locked entry, which keeps
// only name, env_keys and api_key (lockAnthropic).
func anthropicKeyAtLoad(shipped Provider, pd *providersOverlay) string {
	o, written := pd.Providers[AnthropicProvider]
	names := shipped.EnvKeys
	if written && o.EnvKeys != nil {
		names = *o.EnvKeys
	}
	if _, k, _ := firstEnvKey(anthropicGetenv, names); k != "" {
		return k.Reveal()
	}
	if written && o.APIKey != nil {
		if v := strings.TrimSpace(*o.APIKey); keyProblem(v) == nil {
			return v
		}
	}
	return ""
}

// AnthropicListKey is the key the account's model list is fetched with for t
// (plan 044 §3.6): the key craze's own Anthropic provider is funded by
// through getenv (nil is os.Getenv) — the first of its env_keys set to a
// usable key, env-only keys included, else its stored key, Resolve's rule
// (resolveKey) and so the key a load binds the list to (anthropicKeyAtLoad)
// — and false when nothing funds it, or when t has no such provider: none at
// all, one WithholdFrozen took out, or a provider of the user's own under
// that id (a table with no catalog, or built in memory; plan 044 X1), whose
// load reads no list. A provider of the catalog's on another driver is
// unreachable — the lock (lockAnthropic) keeps the driver — and is refused
// here all the same: a list is Anthropic's, fetched from Anthropic.
func (t *Table) AnthropicListKey(getenv func(string) string) (Secret, bool) {
	p, ok := t.Providers[AnthropicProvider]
	if !ok || p.Driver != DriverAnthropic {
		return "", false
	}
	switch t.ProviderOrigin(AnthropicProvider) {
	case OriginShipped, OriginOverridden:
	default:
		return "", false
	}
	k, err := resolveKey(AnthropicProvider, p, t.envOf(getenv))
	if err != nil {
		return "", false
	}
	return k, true
}

// withAnthropicList makes the Anthropic models of cat, the merge's own copy
// of the catalog, the effective set before the user's files are laid over it
// (plan 044 §3.5), when cat ships the Anthropic provider on its driver:
//
//   - dir's account list is used only when its key id is the id of the key
//     the provider is funded by at load (anthropicKeyAtLoad, P7). With no key,
//     another key's list, or no file, the seeds stand alone, silently (a
//     refresh fetches the key's own); a file that is not a list this craze
//     reads is one warning, and so is each entry skipped.
//   - A list in use is authoritative: a seed it names stands as the catalog
//     ships it (craze's vetted knowledge, the price included), a seed it does
//     not name is hidden, and each id it names with no seed becomes the model
//     anthropic/<id> (anthropicListModel), unpriced — never one whose alias,
//     or identity, the catalog retired (P12). An entry that does not stand as
//     a model is skipped and costs nothing else. A model the list says is
//     deprecated, a seed included, has " (deprecated)" after its name. A list
//     with no entry that stands — none at all, or every one skipped or
//     retired — is not authoritative (C4 review 1): it is the one warning of
//     a file craze cannot read, and the seeds stand, so damage to the file
//     never takes every Anthropic model away.
//   - [anthropic_defaults] default_effort becomes the default effort of each
//     Anthropic model that offers it and has none of its own (D3).
//   - With [anthropic_defaults] list = "newest", only the newest model of each
//     family stays (O3's trim knob); a model whose family is not known is
//     kept, being no family's.
//
// A model taken out is kept in cat.listHidden. The warnings name the file
// and the entry, never a key.
func withAnthropicList(cat *Catalog, dir string, pd *providersOverlay) []string {
	if !cat.shipsAnthropic() {
		return nil
	}
	warnings := cat.applyAnthropicList(dir, pd)
	d := cat.Anthropic
	for alias, m := range cat.Models {
		if m.Provider == AnthropicProvider && m.DefaultEffort == "" && slices.Contains(m.Efforts, d.DefaultEffort) {
			m.DefaultEffort = d.DefaultEffort
			cat.Models[alias] = m
		}
	}
	if d.List == AnthropicListNewest {
		for _, aliases := range familiesNewestFirst(cat.Models, AnthropicProvider, factsOfModel) {
			for _, alias := range aliases[1:] {
				cat.hide(alias)
			}
		}
	}
	return warnings
}

// hide takes alias out of c's models into listHidden.
func (c *Catalog) hide(alias string) {
	if c.listHidden == nil {
		c.listHidden = make(map[string]Model)
	}
	c.listHidden[alias] = c.Models[alias]
	delete(c.Models, alias)
}

// applyAnthropicList is withAnthropicList's first step: dir's list, when it is
// bound to the key at load, made the effective set of c's Anthropic models.
func (c *Catalog) applyAnthropicList(dir string, pd *providersOverlay) []string {
	if dir == "" {
		return nil
	}
	key := anthropicKeyAtLoad(c.Providers[AnthropicProvider], pd)
	if key == "" {
		return nil // nothing funds the provider: the seeds stand alone
	}
	path := filepath.Join(dir, AnthropicModelsFile)
	doc, index, skipped, err := readAnthropicModels(path)
	unreadable := []string{fmt.Sprintf("modeltable: %s is not an Anthropic model list this craze reads; craze's own Anthropic models are offered until it is fetched again", path)}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return unreadable
	case doc.KeyID != AnthropicKeyID(key):
		return nil // another key's list (P7): ignored
	}

	var warnings []string
	skip := func(i int, id, reason string) {
		warnings = append(warnings, skippedEntry(path, i, id, anthropicModelID, reason))
	}
	for _, i := range skipped {
		skip(i, "", "it is not a model entry")
	}
	retiredAlias, retiredID := c.retiredAliases(), c.retiredIdentities()
	seeds := map[string]string{} // wire model → alias, of the catalog's own Anthropic models
	for alias, m := range c.Models {
		if m.Provider == AnthropicProvider {
			seeds[m.WireModel] = alias
		}
	}
	// What the list makes of c is gathered first and laid on c only once an
	// entry stands: the seeds it names (deprecated or not), and its own models.
	named := map[string]bool{}      // the seeds it names, and its own models
	deprecated := map[string]bool{} // the seeds it says are deprecated
	added := map[string]Model{}     // its own models, by alias
	for n, e := range doc.Models {
		i := index[n]
		alias := AnthropicAliasPrefix + e.ID
		switch seed, isSeed := seeds[e.ID]; {
		case !anthropicModelID.MatchString(e.ID):
			skip(i, "", "its id is not one craze can name")
		case retiredAlias[alias] || retiredID[identity{AnthropicProvider, e.ID}]:
			// Retired: never offered again, whatever the account lists (P12).
		case isSeed && named[seed]:
			skip(i, e.ID, "the model is listed twice")
		case isSeed:
			named[seed] = true
			deprecated[seed] = e.Lifecycle == "deprecated"
		case hasModel(c.Models, alias) || named[alias]:
			skip(i, e.ID, "the model is listed twice, or its alias is already a model")
		default:
			model := anthropicListModel(e)
			if err := validateModel(path, alias, model, c.Providers); err != nil {
				skip(i, e.ID, err.Error())
				continue
			}
			added[alias] = model
			named[alias] = true
		}
	}
	if len(named) == 0 {
		return unreadable // nothing stands: not the account's list (C4 review 1)
	}
	for seed, dep := range deprecated {
		if dep {
			m := c.Models[seed]
			m.Name = cmp.Or(m.Name, seed) + anthropicDeprecated
			c.Models[seed] = m
		}
	}
	maps.Copy(c.Models, added)
	for _, alias := range slices.Sorted(maps.Values(seeds)) {
		if !named[alias] {
			c.hide(alias)
		}
	}
	return warnings
}

// anthropicListModel is the model a list entry with no seed is (plan 044
// §3.5): wire model the id; its display name, when it is one line of text;
// its family, the entry's line when that is a family; released, the date it
// was created; its window, max_input_tokens; its ceiling, max_tokens up to
// 64,000; its efforts, the levels craze sends in low..max order; vision when
// it takes image input; thinking adaptive, none, or not known when the list
// does not say; and no cost (spend shows it unpriced until a release adds a
// seed).
func anthropicListModel(e AnthropicModel) Model {
	name := ""
	if shownText(e.DisplayName, 128) {
		name = e.DisplayName
	}
	if e.Lifecycle == "deprecated" {
		name = cmp.Or(name, AnthropicAliasPrefix+e.ID) + anthropicDeprecated
	}
	var efforts []string
	for _, level := range anthropicEfforts {
		if slices.Contains(e.Efforts, level) {
			efforts = append(efforts, level)
		}
	}
	family := ""
	if familyPattern.MatchString(e.Line) {
		family = e.Line
	}
	released := ""
	if !e.CreatedAt.IsZero() {
		released = e.CreatedAt.UTC().Format(time.DateOnly)
	}
	thinking := ""
	if a := e.AdaptiveThinking; a != nil {
		thinking = ThinkingNone
		if *a {
			thinking = ThinkingAdaptive
		}
	}
	return Model{
		Provider:        AnthropicProvider,
		WireModel:       e.ID,
		Name:            name,
		ContextWindow:   clampTokens(e.MaxInputTokens),
		MaxOutputTokens: clampTokens(min(e.MaxTokens, anthropicMaxOutput)),
		Efforts:         efforts,
		Vision:          e.ImageInput,
		Thinking:        thinking,
		Family:          family,
		Released:        released,
	}
}

// clampTokens is n as a token count a model holds: n itself on any count a
// real model has, the int32 limit above it (so a 32-bit build never wraps),
// and a negative n as it is, which validateModel refuses.
func clampTokens(n int64) int {
	return int(min(n, 1<<31-1))
}
