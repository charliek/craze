package messagesapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// page is a /v1/models page of entries, has_more and last_id as given.
func page(hasMore bool, lastID string, entries ...any) http.HandlerFunc {
	if entries == nil {
		entries = []any{}
	}
	b, err := json.Marshal(map[string]any{"data": entries, "has_more": hasMore, "first_id": nil, "last_id": lastID})
	if err != nil {
		panic(err)
	}
	return jsonReply(http.StatusOK, string(b))
}

func model(id string) map[string]any {
	return map[string]any{"type": "model", "id": id, "display_name": "Model " + id, "created_at": "2026-01-02T03:04:05Z"}
}

func ids(ms []ModelInfo) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func afterIDs(reqs []request) []string {
	var out []string
	for _, r := range reqs {
		q, _ := url.ParseQuery(r.query)
		out = append(out, q.Get("after_id"))
	}
	return out
}

// TestListModelsReadsTheRealList: the account's list as the API returned it
// (LF-1, testdata/models-list.json), one complete page of 13 models, read
// slim: the newest Haiku with every effort, images and adaptive thinking;
// Haiku 4.5 with no effort and no adaptive thinking — false, not unknown.
// The request is a GET of /v1/models?limit=100 with the key, the API
// version, JSON asked for and craze's user agent, and no beta or body type.
func TestListModelsReadsTheRealList(t *testing.T) {
	list, err := os.ReadFile("testdata/models-list.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(t, jsonReply(http.StatusOK, string(list)))
	ms, complete, err := srv.client(t).ListModels(context.Background(), testKey)
	if err != nil || !complete {
		t.Fatalf("complete %v, err %v", complete, err)
	}
	want := []string{"claude-haiku-5-5", "claude-sonnet-5-5", "claude-opus-5-5", "claude-fable-5-1", "claude-opus-5", "claude-sonnet-5",
		"claude-fable-5", "claude-opus-4-8", "claude-opus-4-7", "claude-sonnet-4-6", "claude-opus-4-6", "claude-opus-4-5-20251101", "claude-haiku-4-5-20251001"}
	if got := ids(ms); !slices.Equal(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	adaptive := true
	haiku := ModelInfo{ID: "claude-haiku-5-5", DisplayName: "Claude Haiku 5.5", CreatedAt: time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC),
		Line: "haiku", MaxInputTokens: 1000000, MaxTokens: 128000, Efforts: EffortLevels(), ImageInput: true,
		AdaptiveThinking: &adaptive, Lifecycle: "active"}
	if got := ms[0]; !sameModel(got, haiku) {
		t.Errorf("haiku 5.5 = %+v\nwant %+v", got, haiku)
	}
	old := ms[len(ms)-1]
	if old.Efforts != nil || old.AdaptiveThinking == nil || *old.AdaptiveThinking || !old.ImageInput || old.MaxTokens != 64000 || old.Line != "haiku" {
		t.Errorf("haiku 4.5 = %+v", old)
	}
	r := srv.requests()[0]
	if r.method != http.MethodGet || r.path != "/v1/models" || r.query != "limit=100" || len(r.body) != 0 {
		t.Errorf("request = %s %s?%s (%d bytes)", r.method, r.path, r.query, len(r.body))
	}
	for name, v := range map[string]string{"X-Api-Key": testKey, "Anthropic-Version": APIVersion, "Accept": "application/json",
		"User-Agent": "craze", "Anthropic-Beta": "", "Content-Type": ""} {
		if got := r.header.Get(name); got != v {
			t.Errorf("%s = %q, want %q", name, got, v)
		}
	}
}

func sameModel(a, b ModelInfo) bool {
	return a.ID == b.ID && a.DisplayName == b.DisplayName && a.CreatedAt.Equal(b.CreatedAt) && a.Line == b.Line &&
		a.MaxInputTokens == b.MaxInputTokens && a.MaxTokens == b.MaxTokens && slices.Equal(a.Efforts, b.Efforts) &&
		a.ImageInput == b.ImageInput && (a.AdaptiveThinking == nil) == (b.AdaptiveThinking == nil) &&
		(a.AdaptiveThinking == nil || *a.AdaptiveThinking == *b.AdaptiveThinking) && a.Lifecycle == b.Lifecycle && a.RetiresAt.Equal(b.RetiresAt)
}

// TestListModelsFollowsThePages: has_more and last_id lead to the next
// page (after_id), to the last; a model on two pages is kept once, as the
// first page had it.
func TestListModelsFollowsThePages(t *testing.T) {
	dup := model("m-b")
	dup["display_name"] = "Second copy"
	srv := newServer(t,
		page(true, "m-b", model("m-a"), model("m-b")),
		page(true, "m-d", dup, model("m-c"), model("m-d")),
		page(false, "m-e", model("m-e")),
	)
	ms, complete, err := srv.client(t).ListModels(context.Background(), testKey)
	if err != nil || !complete {
		t.Fatalf("complete %v, err %v", complete, err)
	}
	if got := ids(ms); !slices.Equal(got, []string{"m-a", "m-b", "m-c", "m-d", "m-e"}) {
		t.Fatalf("ids = %v", got)
	}
	if ms[1].DisplayName != "Model m-b" {
		t.Fatalf("the repeated id kept %q, want the first entry's", ms[1].DisplayName)
	}
	if got := afterIDs(srv.requests()); !slices.Equal(got, []string{"", "m-b", "m-d"}) {
		t.Fatalf("after_id = %q", got)
	}
}

// TestListModelsIncomplete (P10): a listing that cannot reach its end is
// incomplete — a cursor it already followed, a 10th page that still has
// more, a has_more with no cursor or one that is no model id — and keeps
// what it read; the caller never takes it as the whole list.
func TestListModelsIncomplete(t *testing.T) {
	t.Run("a repeated cursor", func(t *testing.T) {
		srv := newServer(t, page(true, "m-a", model("m-a")), page(true, "m-b", model("m-b")), page(true, "m-a", model("m-c")), page(false, "", model("m-z")))
		ms, complete, err := srv.client(t).ListModels(context.Background(), testKey)
		if err != nil || complete {
			t.Fatalf("complete %v, err %v; want incomplete", complete, err)
		}
		if got := ids(ms); !slices.Equal(got, []string{"m-a", "m-b", "m-c"}) || len(srv.requests()) != 3 {
			t.Fatalf("ids %v after %d requests", got, len(srv.requests()))
		}
	})
	t.Run("the page cap", func(t *testing.T) {
		var replies []http.HandlerFunc
		for i := range maxModelPages + 2 {
			replies = append(replies, page(true, fmt.Sprintf("m-%02d", i), model(fmt.Sprintf("m-%02d", i))))
		}
		srv := newServer(t, replies...)
		ms, complete, err := srv.client(t).ListModels(context.Background(), testKey)
		if err != nil || complete {
			t.Fatalf("complete %v, err %v; want incomplete", complete, err)
		}
		if n := len(srv.requests()); n != maxModelPages || len(ms) != maxModelPages {
			t.Fatalf("%d requests and %d models, want %d of each", n, len(ms), maxModelPages)
		}
	})
	t.Run("the 10th page ends it", func(t *testing.T) {
		var replies []http.HandlerFunc
		for i := range maxModelPages {
			replies = append(replies, page(i < maxModelPages-1, fmt.Sprintf("m-%02d", i), model(fmt.Sprintf("m-%02d", i))))
		}
		srv := newServer(t, replies...)
		if _, complete, err := srv.client(t).ListModels(context.Background(), testKey); err != nil || !complete {
			t.Fatalf("complete %v, err %v; ten pages that end are the whole list (the control)", complete, err)
		}
	})
	for name, cursor := range map[string]string{"no cursor": "", "a cursor that is no id": "m a/../b"} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t, page(true, cursor, model("m-a")), page(false, "", model("m-b")))
			ms, complete, err := srv.client(t).ListModels(context.Background(), testKey)
			if err != nil || complete || len(ms) != 1 || len(srv.requests()) != 1 {
				t.Fatalf("complete %v, err %v, %d models after %d requests", complete, err, len(ms), len(srv.requests()))
			}
		})
	}
}

// TestListModelsKeepsOnlyWhatItCanTrust: an entry whose id is no model id
// is skipped — a space, a path, a number, 101 bytes, no id, no object; the
// first of two with one id is kept; and a field of another shape costs that
// field alone: a name with a control character, a negative count, a date
// that is no date, a line in capitals, capabilities of other shapes.
func TestListModelsKeepsOnlyWhatItCanTrust(t *testing.T) {
	odd := map[string]any{
		"id": "m-odd", "display_name": "Bad\x1b[31m name", "created_at": "yesterday", "line": "Opus Family",
		"max_input_tokens": -5, "max_tokens": "lots", "lifecycle": 7, "retires_at": "2027-01-01T00:00:00Z",
		"capabilities": map[string]any{"effort": true, "image_input": 5, "thinking": "x"},
	}
	partial := map[string]any{
		"id": "m-partial",
		"capabilities": map[string]any{
			"effort":      map[string]any{"supported": true, "low": map[string]any{"supported": true}, "medium": true, "max": map[string]any{"supported": true}, "minimal": map[string]any{"supported": true}},
			"image_input": map[string]any{"supported": "yes"},
			"thinking":    map[string]any{"types": map[string]any{"adaptive": map[string]any{"supported": false}}},
		},
	}
	effortOff := map[string]any{"id": "m-effort-off", "capabilities": map[string]any{"effort": map[string]any{"supported": false, "low": map[string]any{"supported": true}}}}
	first := model("m-first")
	second := model("m-first")
	second["display_name"] = "Second"
	srv := newServer(t, page(false, "",
		model("m ok"), model("../m"), map[string]any{"id": 12}, model(strings.Repeat("m", 101)), map[string]any{"display_name": "no id"}, "not an object",
		first, second, odd, partial, effortOff, model(strings.Repeat("m", 100)),
	))
	ms, complete, err := srv.client(t).ListModels(context.Background(), testKey)
	if err != nil || !complete {
		t.Fatalf("complete %v, err %v", complete, err)
	}
	if got := ids(ms); !slices.Equal(got, []string{"m-first", "m-odd", "m-partial", "m-effort-off", strings.Repeat("m", 100)}) {
		t.Fatalf("ids = %v", got)
	}
	if ms[0].DisplayName != "Model m-first" {
		t.Errorf("the repeated id kept %q", ms[0].DisplayName)
	}
	o := ms[1]
	if o.DisplayName != "" || !o.CreatedAt.IsZero() || o.Line != "" || o.MaxInputTokens != 0 || o.MaxTokens != 0 || o.Lifecycle != "" ||
		!o.RetiresAt.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) || o.Efforts != nil || o.ImageInput || o.AdaptiveThinking != nil {
		t.Errorf("odd entry = %+v", o)
	}
	p := ms[2]
	if !slices.Equal(p.Efforts, []string{"low", "max"}) || p.ImageInput || p.AdaptiveThinking == nil || *p.AdaptiveThinking {
		t.Errorf("partial entry = %+v", p)
	}
	if ms[3].Efforts != nil {
		t.Errorf("effort off still lists %v", ms[3].Efforts)
	}
}

// TestListModelsFailures: a status is an *Error — a 401 the key's refusal —
// and so is any other way the listing cannot be read: a reply that is no
// model list, a page past its bound, a later page's failure (no list at
// all, not a partial one), and the listing's own deadline.
func TestListModelsFailures(t *testing.T) {
	t.Run("a refused key", func(t *testing.T) {
		srv := newServer(t, jsonReply(401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"},"request_id":"req_1"}`))
		_, _, err := srv.client(t).ListModels(context.Background(), testKey)
		if e := asError(t, err); !e.AuthError() || e.Type != "authentication_error" || e.RequestID != testRequestID {
			t.Fatalf("error = %+v", e)
		}
	})
	for name, body := range map[string]string{
		"another shape": `{"models":[{"id":"m-a"}]}`,
		"null data":     `{"data":null,"has_more":false}`,
		"an array":      `[{"id":"m-a"}]`,
		"not JSON":      `<html>`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t, jsonReply(200, body))
			ms, complete, err := srv.client(t).ListModels(context.Background(), testKey)
			if err == nil || ms != nil || complete {
				t.Fatalf("%d models, complete %v, err %v; want an error", len(ms), complete, err)
			}
		})
	}
	t.Run("an empty list is a list", func(t *testing.T) {
		srv := newServer(t, page(false, ""))
		ms, complete, err := srv.client(t).ListModels(context.Background(), testKey)
		if err != nil || !complete || len(ms) != 0 {
			t.Fatalf("%v, %v, %v", ms, complete, err)
		}
	})
	t.Run("a page past its bound", func(t *testing.T) {
		old := maxModelsPage
		maxModelsPage = 1 << 10
		defer func() { maxModelsPage = old }()
		big := model("m-big")
		big["display_name"] = strings.Repeat("x", 2<<10)
		srv := newServer(t, page(false, "", big), page(false, "", model("m-a")))
		if _, _, err := srv.client(t).ListModels(context.Background(), testKey); err == nil || !strings.Contains(err.Error(), "larger") {
			t.Fatalf("err = %v, want the page refused", err)
		}
		if _, _, err := srv.client(t).ListModels(context.Background(), testKey); err != nil {
			t.Fatalf("a page under the bound: %v (the control)", err)
		}
	})
	t.Run("a later page fails", func(t *testing.T) {
		srv := newServer(t, page(true, "m-a", model("m-a")), jsonReply(529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
		ms, complete, err := srv.client(t).ListModels(context.Background(), testKey)
		if e := asError(t, err); e.StatusCode != 529 || ms != nil || complete {
			t.Fatalf("%d models, complete %v, error %+v", len(ms), complete, e)
		}
	})
	t.Run("the deadline", func(t *testing.T) {
		old := modelsTimeout
		modelsTimeout = 50 * time.Millisecond
		defer func() { modelsTimeout = old }()
		release := make(chan struct{})
		defer close(release)
		srv := newServer(t, func(http.ResponseWriter, *http.Request) { <-release })
		_, _, err := srv.client(t).ListModels(context.Background(), testKey)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the listing's deadline", err)
		}
	})
}

// TestModelInfoJSON: the slim form a cache of the list keeps — the API's
// own names, an unknown adaptive thinking and unset times left out, and
// image_input always said.
func TestModelInfoJSON(t *testing.T) {
	on := true
	b, err := json.Marshal([]ModelInfo{
		{ID: "claude-x-1", DisplayName: "Claude X 1", CreatedAt: time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC), Line: "x",
			MaxInputTokens: 1000000, MaxTokens: 128000, Efforts: []string{"low", "max"}, ImageInput: true, AdaptiveThinking: &on,
			Lifecycle: "deprecated", RetiresAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{ID: "claude-y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"id":"claude-x-1","display_name":"Claude X 1","created_at":"2026-10-07T18:00:00Z","line":"x","max_input_tokens":1000000,` +
		`"max_tokens":128000,"efforts":["low","max"],"image_input":true,"adaptive_thinking":true,"lifecycle":"deprecated",` +
		`"retires_at":"2027-01-01T00:00:00Z"},{"id":"claude-y","image_input":false}]`
	if string(b) != want {
		t.Fatalf("json =\n%s\nwant\n%s", b, want)
	}
}

// TestListModelsNeedsABooleanHasMore (P10, review c1 F5): a page that does
// not say, as a JSON boolean, whether more follow — has_more absent, null,
// a string, a number — leaves the list incomplete after that page, never
// certified whole; its models are kept. The control: has_more false is a
// complete list.
func TestListModelsNeedsABooleanHasMore(t *testing.T) {
	for name, body := range map[string]string{
		"absent":   `{"data":[{"id":"m-a"}],"last_id":"m-a"}`,
		"null":     `{"data":[{"id":"m-a"}],"has_more":null,"last_id":"m-a"}`,
		"a string": `{"data":[{"id":"m-a"}],"has_more":"true","last_id":"m-a"}`,
		"a number": `{"data":[{"id":"m-a"}],"has_more":0,"last_id":"m-a"}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t, jsonReply(200, body), page(false, "", model("m-b")))
			ms, complete, err := srv.client(t).ListModels(context.Background(), testKey)
			if err != nil || complete || !slices.Equal(ids(ms), []string{"m-a"}) || len(srv.requests()) != 1 {
				t.Fatalf("%v, complete %v, err %v, after %d requests; want m-a, incomplete", ids(ms), complete, err, len(srv.requests()))
			}
		})
	}
	srv := newServer(t, jsonReply(200, `{"data":[{"id":"m-a"}],"has_more":false,"last_id":"m-a"}`))
	if _, complete, err := srv.client(t).ListModels(context.Background(), testKey); err != nil || !complete {
		t.Fatalf("has_more false: complete %v, err %v", complete, err)
	}
}

// TestEffortLevelsIsACopy (review c1 F6): what EffortLevels hands out is
// the caller's own — changing it changes neither the next copy nor how a
// later list is read.
func TestEffortLevelsIsACopy(t *testing.T) {
	levels := EffortLevels()
	levels[0] = "max"
	if got := EffortLevels(); !slices.Equal(got, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("EffortLevels = %v after a caller changed its copy", got)
	}
	entry := map[string]any{"id": "m-a", "capabilities": map[string]any{"effort": map[string]any{"supported": true,
		"low": map[string]any{"supported": true}, "max": map[string]any{"supported": true}}}}
	ms, _, err := newServer(t, page(false, "", entry)).client(t).ListModels(context.Background(), testKey)
	if err != nil || len(ms) != 1 || !slices.Equal(ms[0].Efforts, []string{"low", "max"}) {
		t.Fatalf("efforts = %v, err %v; the caller's change reached the parser", ms, err)
	}
}
