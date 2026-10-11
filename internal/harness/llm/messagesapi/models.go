package messagesapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"
)

// The model list's bounds (plan 044 §3.2, P10): pages of 100, at most 10 of
// them, each at most 8 MiB, and 30 s for the whole listing.
const (
	modelsPageLimit = 100
	maxModelPages   = 10
)

// maxModelsPage bounds one page's body, and modelsTimeout one ListModels,
// every page. Variables only so a test can lower them.
var (
	maxModelsPage = 8 << 20
	modelsTimeout = 30 * time.Second
)

// ModelInfo is one model of the account's list, slim: the fields craze
// derives a model's settings from (§3.5), each kept only in the shape it
// should have — a value of another shape is left zero, never the entry's
// end. Its JSON is the slim form the cache file keeps.
type ModelInfo struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
	// Line is the model's family ("opus", "sonnet", "haiku", "fable").
	Line           string `json:"line,omitempty"`
	MaxInputTokens int64  `json:"max_input_tokens,omitempty"`
	MaxTokens      int64  `json:"max_tokens,omitempty"`
	// Efforts are the effort levels the model supports, in EffortLevels'
	// order; none for a model without effort.
	Efforts    []string `json:"efforts,omitempty"`
	ImageInput bool     `json:"image_input"`
	// AdaptiveThinking is whether the model takes adaptive thinking; nil
	// when the list does not say.
	AdaptiveThinking *bool     `json:"adaptive_thinking,omitempty"`
	Lifecycle        string    `json:"lifecycle,omitempty"`
	RetiresAt        time.Time `json:"retires_at,omitzero"`
}

// effortLevels are the effort levels craze reads from a model's
// capabilities, lowest first: the parser's own, never handed out (review c1
// F6), so no caller can change what every later list is read with.
var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// EffortLevels is a copy of the effort levels craze reads from a model's
// capabilities, lowest first.
func EffortLevels() []string { return slices.Clone(effortLevels) }

// modelIDPattern is what a model id must look like to be kept: the API's ids
// ("claude-opus-5-5", "claude-haiku-4-5-20251001") and nothing that could
// carry text or a path.
var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// wordPattern is what a model's line and lifecycle must look like to be
// kept.
var wordPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ListModels is the account's model list (GET /v1/models), with key, and
// whether it is complete: the pages are followed by has_more and last_id
// (after_id) for at most maxModelPages, and a page 10 that still has more,
// a page whose has_more is not a boolean, or a cursor the listing already
// followed, ends it incomplete — a list craze must not take as the whole
// (§3.6). An entry whose id is not a model id is skipped, and a repeated id
// keeps its first entry. Any failure — a status, a page over 8 MiB, a reply
// that is no model list, the 30 s bound — is an error, and no list; no error
// carries key (it runs outside the model wrapper's scrubber, P13).
func (c *Client) ListModels(ctx context.Context, key string) ([]ModelInfo, bool, error) {
	if key == "" {
		return nil, false, ErrNoKey
	}
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()
	sec := newSecret(key)
	var out []ModelInfo
	seen := map[string]bool{}
	followed := map[string]bool{}
	after := ""
	for page := 1; ; page++ {
		p, err := c.modelsPage(ctx, key, sec, after)
		if err != nil {
			return nil, false, err
		}
		for _, m := range p.models {
			if !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m)
			}
		}
		if p.hasMore == nil {
			return out, false, nil // the page did not say whether more follow
		}
		if !*p.hasMore {
			return out, true, nil
		}
		// A cursor that holds the key is not followed: it would go into a
		// URL (review c1r2 2).
		if page == maxModelPages || !modelIDPattern.MatchString(p.lastID) || followed[p.lastID] || sec.text(p.lastID) != p.lastID {
			return out, false, nil
		}
		followed[p.lastID] = true
		after = p.lastID
	}
}

type modelsPage struct {
	models []ModelInfo
	// hasMore is has_more; nil when the page did not say, as a boolean,
	// whether more follow (absent, null, or another type; review c1 F5).
	hasMore *bool
	lastID  string
}

// modelsPage is one page of the list, after the id after ("" for the
// first).
func (c *Client) modelsPage(ctx context.Context, key string, sec *secret, after string) (*modelsPage, error) {
	q := url.Values{"limit": {strconv.Itoa(modelsPageLimit)}}
	if after != "" {
		q.Set("after_id", after)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.models+"?"+q.Encode(), nil)
	if err != nil {
		return nil, sec.err(err)
	}
	c.setHeaders(hreq.Header, key, "application/json", nil)
	resp, err := c.do(ctx, hreq, sec)
	if err != nil {
		return nil, fmt.Errorf("messagesapi: listing the models: %w", err) // do scrubbed it
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp, sec)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxModelsPage)+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, sec.err(fmt.Errorf("messagesapi: reading the model list: %w", err))
	}
	if len(body) > maxModelsPage {
		return nil, errors.New("messagesapi: a page of the model list is larger than craze reads (8 MiB)")
	}
	return parseModelsPage(body)
}

// parseModelsPage reads a page, {"data":[…],"has_more":…,"last_id":…},
// keeping each entry that is a model with a model id.
func parseModelsPage(body []byte) (*modelsPage, error) {
	var doc struct {
		Data    []json.RawMessage `json:"data"`
		HasMore json.RawMessage   `json:"has_more"`
		LastID  json.RawMessage   `json:"last_id"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Data == nil {
		return nil, errors.New("messagesapi: the reply is not a model list")
	}
	p := &modelsPage{lastID: jsonString(doc.LastID)}
	if json.Unmarshal(doc.HasMore, &p.hasMore) != nil {
		p.hasMore = nil // not a boolean (an absent field is no JSON at all)
	}
	for _, raw := range doc.Data {
		if m, ok := parseModel(raw); ok {
			p.models = append(p.models, m)
		}
	}
	return p, nil
}

// parseModel reads one entry, every field raw first, so a field of another
// shape costs that field alone; an entry that is no object, or whose id is
// no model id, is no model.
func parseModel(raw json.RawMessage) (ModelInfo, bool) {
	var w struct {
		ID             json.RawMessage `json:"id"`
		DisplayName    json.RawMessage `json:"display_name"`
		CreatedAt      json.RawMessage `json:"created_at"`
		Line           json.RawMessage `json:"line"`
		MaxInputTokens json.RawMessage `json:"max_input_tokens"`
		MaxTokens      json.RawMessage `json:"max_tokens"`
		Capabilities   json.RawMessage `json:"capabilities"`
		Lifecycle      json.RawMessage `json:"lifecycle"`
		RetiresAt      json.RawMessage `json:"retires_at"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return ModelInfo{}, false
	}
	m := ModelInfo{ID: jsonString(w.ID)}
	if !modelIDPattern.MatchString(m.ID) {
		return ModelInfo{}, false
	}
	if name := jsonString(w.DisplayName); shownText(name, 128) {
		m.DisplayName = name
	}
	m.CreatedAt = jsonTime(w.CreatedAt)
	m.RetiresAt = jsonTime(w.RetiresAt)
	m.Line = keep(wordPattern, jsonString(w.Line))
	m.Lifecycle = keep(wordPattern, jsonString(w.Lifecycle))
	m.MaxInputTokens = jsonCount(w.MaxInputTokens)
	m.MaxTokens = jsonCount(w.MaxTokens)

	var caps struct {
		Effort     map[string]json.RawMessage `json:"effort"`
		ImageInput json.RawMessage            `json:"image_input"`
		Thinking   struct {
			Types struct {
				Adaptive json.RawMessage `json:"adaptive"`
			} `json:"types"`
		} `json:"thinking"`
	}
	// A capability of another shape is skipped and the rest are read
	// (encoding/json fills what it can and reports the first mismatch), so
	// one costs that capability alone.
	_ = json.Unmarshal(w.Capabilities, &caps)
	if on, _ := supported(caps.Effort["supported"], true); on {
		for _, level := range effortLevels {
			if on, _ := supported(caps.Effort[level], false); on {
				m.Efforts = append(m.Efforts, level)
			}
		}
	}
	m.ImageInput, _ = supported(caps.ImageInput, false)
	if on, ok := supported(caps.Thinking.Types.Adaptive, false); ok {
		m.AdaptiveThinking = &on
	}
	return m, true
}

// supported is a capability's {"supported": bool}, and whether raw says;
// bare, raw may be the boolean itself.
func supported(raw json.RawMessage, bare bool) (bool, bool) {
	var b bool
	if bare && json.Unmarshal(raw, &b) == nil {
		return b, true
	}
	var c struct {
		Supported *bool `json:"supported"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Supported == nil {
		return false, false
	}
	return *c.Supported, true
}

// jsonCount is raw as a non-negative integer, else 0.
func jsonCount(raw json.RawMessage) int64 {
	var n int64
	if json.Unmarshal(raw, &n) != nil || n < 0 {
		return 0
	}
	return n
}

// jsonTime is raw as an RFC 3339 time, in UTC, else the zero time.
func jsonTime(raw json.RawMessage) time.Time {
	t, err := time.Parse(time.RFC3339, jsonString(raw))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
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
