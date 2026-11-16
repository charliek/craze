package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/websearch"
)

// A key stored while a session runs is sent by its web search from the next
// turn on (issue #137), and only once the session's redactor holds it.

// exaKeyEcho is a fake of Exa's own endpoint: a server that records the
// x-api-key of every call and answers each search with one result whose text
// carries that key, as a careless provider might echo it.
type exaKeyEcho struct {
	srv  *httptest.Server
	mu   sync.Mutex
	keys []string
}

func newExaKeyEcho(t *testing.T) *exaKeyEcho {
	t.Helper()
	e := &exaKeyEcho{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(body, &call)
		key := r.Header.Get("X-Api-Key")
		e.mu.Lock()
		e.keys = append(e.keys, key)
		e.mu.Unlock()
		text := "Title: Echo\nURL: https://echo.example/\nHighlights:\nsent with [" + key + "]"
		res, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": false}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", res)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *exaKeyEcho) sent() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.keys...)
}

// toServer sends every request to srv, whatever its URL names: the service
// believes it calls Exa's own endpoint — where a key may go — and reaches
// the fake.
type toServer struct{ srv *url.URL }

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = t.srv.Scheme, t.srv.Host
	return http.DefaultTransport.RoundTrip(r)
}

// TestNativeTakesUpAWebKeyStoredWhileRunning: a keyless session's searches
// carry no key; once a key is stored, the next turn's search carries it, the
// key the provider echoes back reaches the model redacted — the redactor held
// it before the call went out — and the session's web status says the key is
// the stored one.
func TestNativeTakesUpAWebKeyStoredWhileRunning(t *testing.T) {
	const stored = "exa-stored-mid-session-0301"
	echo := newExaKeyEcho(t)
	u, _ := url.Parse(echo.srv.URL)
	f := newNativeFixture(t)
	f.edit = func(o *harness.Options) {
		// The session wires its report after this seam, into the same
		// options: read it at the call, as a service built at Open has it.
		svc, err := websearch.New(websearch.Config{HTTPClient: &http.Client{Transport: toServer{u}},
			Report: func(c tool.WebCall) {
				if o.OnWebCall != nil {
					o.OnWebCall(c)
				}
			}})
		if err != nil {
			t.Fatal(err)
		}
		o.Web = harness.WebState{Mode: harness.WebOn, Service: svc}
	}
	s := f.started(Options{})
	noReloads(s)
	m := f.models["test/a"]

	m.push(nativeCallStep("c1", "websearch", searchArgs(t, "before")), answer("one"))
	if _, err := s.Prompt(context.Background(), "search"); err != nil {
		t.Fatal(err)
	}
	if st := lastWebStatus(drained(s)); st == nil || st.Key != "keyless" || st.Searches != 1 {
		t.Fatalf("the first turn's web status = %+v; want keyless, one search", st)
	}

	storeWebKey(t, f.dir, stored)
	m.push(nativeCallStep("c2", "websearch", searchArgs(t, "after")), answer("two"))
	if _, err := s.Prompt(context.Background(), "search again"); err != nil {
		t.Fatal(err)
	}
	events := drained(s)
	if got := echo.sent(); len(got) != 2 || got[0] != "" || got[1] != stored {
		t.Fatalf("keys sent = %q; want none, then the stored key", got)
	}
	text := toolResultsOf(m.requests()[len(m.requests())-1])["c2"]
	if strings.Contains(text, stored) || !strings.Contains(text, "sent with ["+redact.Marker+"]") {
		t.Fatalf("the model was told %q; want the echoed key redacted", text)
	}
	if found := nativeLeaks(events, stored); len(found) > 0 {
		t.Fatalf("the key reached an event at %v", found)
	}
	if st := lastWebStatus(events); st == nil || st.Key != "stored" || st.Searches != 2 {
		t.Fatalf("the second turn's web status = %+v; want the stored key, two searches", st)
	}
}

// lastWebStatus is the last web status section among events, nil for none.
func lastWebStatus(events []Event) *WebStatusState {
	var last *WebStatusState
	for _, ev := range events {
		if ev.State != nil && ev.State.WebStatus != nil {
			last = ev.State.WebStatus
		}
	}
	return last
}
