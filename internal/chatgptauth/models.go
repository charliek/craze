package chatgptauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
// priority, and whether it takes parallel tool calls (nil: not said).
type Model struct {
	Slug              string   `json:"slug"`
	DisplayName       string   `json:"display_name"`
	ContextWindow     int      `json:"context_window"`
	Efforts           []string `json:"efforts"`
	DefaultEffort     string   `json:"default_effort,omitempty"`
	InputModalities   []string `json:"input_modalities"`
	Priority          int      `json:"priority"`
	ParallelToolCalls *bool    `json:"parallel_tool_calls,omitempty"`
}

// Models is chatgpt-models.json (plan 033 §3.10, P34): the list of models
// the signed-in account may use, bound to that account by its subject and
// issued client id — modeltable ignores a list whose account is not the one
// in chatgpt-client.json, so a later sign-in never inherits another's — with
// the server's X-Models-Etag and when it was fetched. Only models the server
// lists for display (visibility "list") are kept, in priority order.
//
// modeltable reads this file itself (the harness may not import this
// package, D-02), so its shape is a contract: a change here is a change
// there.
type Models struct {
	Version    int       `json:"version"`
	Subject    string    `json:"subject"`
	ClientID   string    `json:"client_id"`
	ModelsEtag string    `json:"models_etag,omitempty"`
	FetchedAt  time.Time `json:"fetched_at"`
	Models     []Model   `json:"models"`
}

// FetchModels fetches the plan's model list with src's token (GET
// /v1/models; a 401 renews the token once, as a request does) and writes it
// to dir's chatgpt-models.json, 0600, bound to the account the token belongs
// to. The server's ordering is its priority field; If-None-Match is not
// relied on (the spike saw no 304).
func FetchModels(ctx context.Context, src *TokenSource) (*Models, error) {
	ends, err := currentEndpoints()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()
	for retried := false; ; retried = true {
		tok, gen, rec, err := src.token(ctx)
		if err != nil {
			return nil, err
		}
		status, hdr, body, err := ends.apiGET(ctx, ends.api+"/models", tok)
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
		m := &Models{
			Version:    modelsVersion,
			Subject:    rec.Subject,
			ClientID:   rec.ClientID,
			ModelsEtag: etagOf(hdr),
			FetchedAt:  now().UTC(),
			Models:     list,
		}
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(src.dir, 0o700); err != nil {
			return nil, err
		}
		if err := atomicfile.Write(ModelsFile(src.dir), append(b, '\n'), 0o600); err != nil {
			return nil, err
		}
		return m, nil
	}
}

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
// several hosts opening at once fetch it once. It answers whether it
// fetched.
func RefreshModels(ctx context.Context, src *TokenSource, maxAge time.Duration) (bool, error) {
	if modelsCurrent(src.dir, maxAge) {
		return false, nil
	}
	if err := os.MkdirAll(src.dir, 0o700); err != nil {
		return false, err
	}
	path := ModelsFile(src.dir) + ".lock"
	unlock, err := atomicfile.LockWithin(path, 0)
	if errors.Is(err, atomicfile.ErrLockBusy) {
		if onLockBusy != nil {
			onLockBusy()
		}
		unlock, err = atomicfile.LockContext(ctx, path, lockWait)
	}
	if err != nil {
		return false, fmt.Errorf("chatgptauth: taking the model list's lock: %w", err)
	}
	defer unlock()
	if modelsCurrent(src.dir, maxAge) {
		return false, nil
	}
	if _, err := FetchModels(ctx, src); err != nil {
		return false, err
	}
	return true, nil
}

// modelsCurrent says dir's model list is the registered account's and was
// fetched less than maxAge ago (and not in the future).
func modelsCurrent(dir string, maxAge time.Duration) bool {
	m, err := ReadModels(dir)
	if err != nil {
		return false
	}
	c, err := ReadClient(dir)
	if err != nil || c.ClientID == "" || m.ClientID != c.ClientID || m.Subject != c.Subject {
		return false
	}
	age := now().Sub(m.FetchedAt)
	return age >= -clockSkew && age < maxAge
}
