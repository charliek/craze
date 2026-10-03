package chatgptauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charliek/craze/internal/atomicfile"
)

// modelsVersion is chatgpt-models.json's schema version.
const modelsVersion = 1

// maxModelsReply bounds the model list's reply: the spike's was 374 KB, most
// of it each model's codex prompt, which craze does not keep.
const maxModelsReply = 8 << 20

// maxModelsFile bounds what ReadModels reads: the slim cache is a few KB.
const maxModelsFile = 1 << 20

// ModelsMaxAge is how old the model list may be before a session's open
// fetches it again in the background (plan 033 §3.10).
const ModelsMaxAge = 24 * time.Hour

// modelsTimeout bounds one fetch of the model list.
const modelsTimeout = 30 * time.Second

// Model is one model of the plan's list as craze keeps it (P34): the slug a
// request names, the name shown, the context window, the reasoning efforts
// as the server lists them (modeltable keeps the ones a request may send),
// the default effort, the input modalities (vision), the server's ordering
// priority, whether it takes parallel tool calls (nil: not said), and the
// least client_version the server says it needs (plan 034 §3.1; "" when the
// reply gave none or gave one that is not short printable text).
type Model struct {
	Slug              string   `json:"slug"`
	DisplayName       string   `json:"display_name"`
	ContextWindow     int      `json:"context_window"`
	Efforts           []string `json:"efforts"`
	DefaultEffort     string   `json:"default_effort,omitempty"`
	InputModalities   []string `json:"input_modalities"`
	Priority          int      `json:"priority"`
	ParallelToolCalls *bool    `json:"parallel_tool_calls,omitempty"`
	MinClientVersion  string   `json:"minimal_client_version,omitempty"`
}

// Models is chatgpt-models.json (plan 033 §3.10, P34): the list of models
// the signed-in account may use, bound to that account by its subject and
// issued client id — modeltable ignores a list whose account is not the one
// in chatgpt-client.json, so a later sign-in never inherits another's — with
// the server's X-Models-Etag and when it was fetched. Only models the server
// lists for display (visibility "list") are kept, in priority order.
//
// ClientVersion is the client_version the list was fetched with (plan 034
// §3.1, D-82): the server gates the list on it, so a list fetched with another
// version, or none (a legacy file), is stale (modelsCurrent) and fetched
// again. Each model keeps the server's minimal_client_version, which
// modeltable's reader compares with its own pin, so a binary with an older pin
// never offers a model a newer one fetched. Both fields are optional and
// modelsVersion stays 1: a legacy file stays readable.
//
// modeltable reads this file itself (the harness may not import this
// package, D-02), so its shape is a contract: a change here is a change
// there.
type Models struct {
	Version       int       `json:"version"`
	Subject       string    `json:"subject"`
	ClientID      string    `json:"client_id"`
	ModelsEtag    string    `json:"models_etag,omitempty"`
	ClientVersion string    `json:"client_version,omitempty"`
	FetchedAt     time.Time `json:"fetched_at"`
	Models        []Model   `json:"models"`
}

// FetchOptions are what a caller says about one model-list fetch. A later
// field (plan 034 C2b's event observer) is added here, so FetchModels and
// RefreshModels keep their signatures.
type FetchOptions struct {
	// ClientVersion is the client_version the request carries (plan 034 Q1):
	// the caller passes the shipped catalog's pin
	// (modeltable.ChatGPTModelsClientVersion), since this package may not
	// import the harness. Empty is ErrClientVersionRequired, before any
	// request.
	ClientVersion string
}

var (
	// ErrClientVersionRequired is FetchModels' and RefreshModels' error for
	// an empty FetchOptions.ClientVersion: the server's list without one is
	// the short one (plan 034 Q1), so craze never sends it.
	ErrClientVersionRequired = errors.New("chatgptauth: the model list is fetched with a client_version")

	// ErrModelsEmpty is FetchModels' error for a reply with no visible model
	// when the account's cache has some: the cache is kept (plan 034 Q3), as a
	// server-side minimum above the pin would otherwise wipe the list. The
	// wrapped message names the pin.
	ErrModelsEmpty = errors.New("chatgptauth: the plan's model list came back empty")
)

// FetchModels fetches the plan's model list with src's token (GET
// /v1/models?client_version=…; a 401 renews the token once, as a request
// does) and writes it to dir's chatgpt-models.json, 0600, bound to the
// account the token belongs to and to the client version it was fetched with.
// The server's ordering is its priority field; If-None-Match is not relied on
// (the spike saw no 304).
//
// A reply with no visible model, when the account's cache has some, writes
// nothing and is ErrModelsEmpty (plan 034 Q3); with no usable cache the empty
// list is written as it was. The cache check and the write are one critical
// section under the list's own lock (chatgpt-models.json.lock, the one
// RefreshModels takes), taken after the reply — never held across the
// request — so a concurrent host's non-empty list cannot land between them
// and be overwritten by an empty one (plan 034 r1 #2).
func FetchModels(ctx context.Context, src *TokenSource, opts FetchOptions) (*Models, error) {
	f, err := fetchModelList(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	if afterModelsReply != nil {
		afterModelsReply()
	}
	unlock, err := lockModels(ctx, src.dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return storeModels(src.dir, opts, f)
}

// fetchedModels is one reply of the model list, parsed, with the account the
// token that fetched it belongs to.
type fetchedModels struct {
	list []Model
	etag string
	rec  *record
}

// lockModels takes dir's model-list lock: at once when it is free, else —
// after onLockBusy — waiting for it within lockWait, giving up when ctx is
// done. The directory is made first.
func lockModels(ctx context.Context, dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := ModelsFile(dir) + ".lock"
	unlock, err := atomicfile.LockWithin(path, 0)
	if errors.Is(err, atomicfile.ErrLockBusy) {
		if onLockBusy != nil {
			onLockBusy()
		}
		unlock, err = atomicfile.LockContext(ctx, path, lockWait)
	}
	if err != nil {
		return nil, fmt.Errorf("chatgptauth: taking the model list's lock: %w", err)
	}
	return unlock, nil
}

// fetchModelList is the request half of FetchModels: the list, parsed, and
// the account that fetched it. It touches no file of the model list.
func fetchModelList(ctx context.Context, src *TokenSource, opts FetchOptions) (*fetchedModels, error) {
	if opts.ClientVersion == "" {
		return nil, ErrClientVersionRequired
	}
	ends, err := currentEndpoints()
	if err != nil {
		return nil, err
	}
	listURL := ends.api + "/models?" + url.Values{"client_version": {opts.ClientVersion}}.Encode()
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()
	for retried := false; ; retried = true {
		tok, gen, rec, err := src.token(ctx)
		if err != nil {
			return nil, err
		}
		status, hdr, body, err := ends.apiGET(ctx, listURL, tok)
		if err != nil {
			return nil, err
		}
		if status == http.StatusUnauthorized && !retried {
			if err := src.Invalidate(ctx, gen); err != nil {
				return nil, err
			}
			continue
		}
		if status != http.StatusOK {
			return nil, oauthError("models", status, body)
		}
		list, err := parseModels(body)
		if err != nil {
			return nil, err
		}
		return &fetchedModels{list: list, etag: etagOf(hdr), rec: rec}, nil
	}
}

// storeModels is the write half of FetchModels: the empty-reply rule, then
// the file. The caller holds dir's model-list lock.
func storeModels(dir string, opts FetchOptions, f *fetchedModels) (*Models, error) {
	if len(f.list) == 0 {
		if old, err := ReadModels(dir); err == nil && len(old.Models) > 0 &&
			old.Subject == f.rec.Subject && old.ClientID == f.rec.ClientID {
			return nil, fmt.Errorf("%w (client_version %s)", ErrModelsEmpty, opts.ClientVersion)
		}
	}
	if inModelsLock != nil {
		inModelsLock()
	}
	m := &Models{
		Version:       modelsVersion,
		Subject:       f.rec.Subject,
		ClientID:      f.rec.ClientID,
		ModelsEtag:    f.etag,
		ClientVersion: opts.ClientVersion,
		FetchedAt:     now().UTC(),
		Models:        f.list,
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := atomicfile.Write(ModelsFile(dir), append(b, '\n'), 0o600); err != nil {
		return nil, err
	}
	return m, nil
}

// afterModelsReply and inModelsLock are test seams: the first runs between
// FetchModels' reply and its lock, the second inside the critical section
// between the cache check and the write.
var (
	afterModelsReply func()
	inModelsLock     func()
)

// errOffAPI is a bearer request to anywhere but the API's origin.
var errOffAPI = errors.New("chatgptauth: a request with the ChatGPT token may go only to the API's host")

// apiGET sends GET rawURL with the bearer token, after checking rawURL is on
// the API's origin (P37: the token goes to api.openai.com, or a test's
// loopback API, and nowhere else). Only the bearer and Accept are set: no
// OpenAI-Organization or -Project header ever goes with it.
func (e endpoints) apiGET(ctx context.Context, rawURL, token string) (int, http.Header, []byte, error) {
	if !sameOrigin(rawURL, e.api) {
		return 0, nil, nil, errOffAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	return do(ctx, "models", req, maxModelsReply)
}

// parseModels reads the list's {"models":[…]} (codex's catalog shape, not
// OpenAI's {"data":[…]}), keeping the models shown for display. An entry
// that does not decode, or whose slug or name could not be shown safely, is
// skipped; the rest are sorted by priority, ties in the server's order.
func parseModels(body []byte) ([]Model, error) {
	var doc struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Models == nil {
		return nil, errors.New("chatgptauth: models: the reply is not a model list")
	}
	var out []Model
	for _, raw := range doc.Models {
		var m struct {
			Slug                     string   `json:"slug"`
			DisplayName              string   `json:"display_name"`
			Visibility               string   `json:"visibility"`
			ContextWindow            int      `json:"context_window"`
			InputModalities          []string `json:"input_modalities"`
			SupportedReasoningLevels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
			DefaultReasoningLevel     string `json:"default_reasoning_level"`
			SupportsParallelToolCalls *bool  `json:"supports_parallel_tool_calls"`
			Priority                  int    `json:"priority"`
			// Decoded apart: a minimum that is not a string (a number, an
			// object, null) costs the minimum, never the model (r1 #4).
			MinimalClientVersion json.RawMessage `json:"minimal_client_version"`
		}
		if json.Unmarshal(raw, &m) != nil || m.Visibility != "list" || !identifier(m.Slug, 64) || m.ContextWindow < 0 {
			continue
		}
		name := m.DisplayName
		if name == "" {
			name = m.Slug
		}
		if !shownText(name, 128) {
			continue
		}
		model := Model{
			Slug:              m.Slug,
			DisplayName:       name,
			ContextWindow:     m.ContextWindow,
			Priority:          m.Priority,
			ParallelToolCalls: m.SupportsParallelToolCalls,
		}
		var minVersion string
		if json.Unmarshal(m.MinimalClientVersion, &minVersion) == nil && identifier(minVersion, 32) {
			model.MinClientVersion = minVersion
		}
		for _, l := range m.SupportedReasoningLevels {
			if identifier(l.Effort, 32) && !slices.Contains(model.Efforts, l.Effort) {
				model.Efforts = append(model.Efforts, l.Effort)
			}
		}
		if identifier(m.DefaultReasoningLevel, 32) {
			model.DefaultEffort = m.DefaultReasoningLevel
		}
		for _, mod := range m.InputModalities {
			if identifier(mod, 32) && !slices.Contains(model.InputModalities, mod) {
				model.InputModalities = append(model.InputModalities, mod)
			}
		}
		out = append(out, model)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, nil
}

// shownText says s is one line of at most max bytes of UTF-8 with no control
// character: a picker row, never an escape sequence.
func shownText(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// etagOf is the reply's X-Models-Etag (the spike: on every reply), else its
// Etag, kept when it is short printable text.
func etagOf(h http.Header) string {
	for _, k := range []string{"X-Models-Etag", "Etag"} {
		if v := h.Get(k); v != "" && shownText(v, 256) {
			return v
		}
	}
	return ""
}

// ReadModels is dir's model list, as FetchModels wrote it. No file is
// fs.ErrNotExist; one this package did not write is an error.
func ReadModels(dir string) (*Models, error) {
	b, _, err := readFileMax(ModelsFile(dir), maxModelsFile)
	if err != nil {
		if errors.Is(err, errCorrupt) {
			return nil, errors.New("chatgptauth: chatgpt-models.json is not a model list craze wrote")
		}
		return nil, err
	}
	var m Models
	if err := json.Unmarshal(b, &m); err != nil || m.Version != modelsVersion {
		return nil, errors.New("chatgptauth: chatgpt-models.json is not a model list craze wrote")
	}
	return &m, nil
}

// RefreshModels fetches the model list again when dir's is missing, older
// than maxAge, or another account's than chatgpt-client.json's — the
// background refresh a session's open runs (plan 033 §3.10). The check is
// made again under the list's own lock (chatgpt-models.json.lock), so
// several hosts opening at once fetch it once. A list fetched with another
// client_version than opts', or none, is not current (plan 034 Q3). It
// answers whether it fetched.
func RefreshModels(ctx context.Context, src *TokenSource, maxAge time.Duration, opts FetchOptions) (bool, error) {
	if opts.ClientVersion == "" {
		return false, ErrClientVersionRequired
	}
	if modelsCurrent(src.dir, maxAge, opts.ClientVersion) {
		return false, nil
	}
	unlock, err := lockModels(ctx, src.dir)
	if err != nil {
		return false, err
	}
	defer unlock()
	if modelsCurrent(src.dir, maxAge, opts.ClientVersion) {
		return false, nil
	}
	f, err := fetchModelList(ctx, src, opts)
	if err != nil {
		return false, err
	}
	if _, err := storeModels(src.dir, opts, f); err != nil {
		return false, err
	}
	return true, nil
}

// modelsCurrent says dir's model list is the registered account's and was
// fetched less than maxAge ago (and not in the future) with clientVersion.
func modelsCurrent(dir string, maxAge time.Duration, clientVersion string) bool {
	m, err := ReadModels(dir)
	if err != nil {
		return false
	}
	c, err := ReadClient(dir)
	if err != nil || c.ClientID == "" || m.ClientID != c.ClientID || m.Subject != c.Subject || m.ClientVersion != clientVersion {
		return false
	}
	age := now().Sub(m.FetchedAt)
	return age >= -clockSkew && age < maxAge
}
