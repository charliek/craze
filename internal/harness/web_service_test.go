package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/websearch"
)

// The web service a session builds at Open (plan 051 §B, §C.3, §C.4): built
// only where the adapter injected the endpoint, shared with the session's
// children when it opened on, asking only where the adapter said a person
// can be asked, and remembering in the session's Home. The web is a fake MCP
// server on loopback.

// mcpServer is an MCP endpoint on loopback answering every call with one
// result, recording the tool each call named.
type mcpServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	names []string
	// keys is each call's x-api-key values, joined; "" for none.
	keys []string
}

func newMCPServer(t *testing.T) *mcpServer {
	t.Helper()
	m := &mcpServer{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			ID     int64 `json:"id"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &call)
		m.mu.Lock()
		m.names = append(m.names, call.Params.Name)
		m.keys = append(m.keys, strings.Join(r.Header.Values("X-Api-Key"), ","))
		m.mu.Unlock()
		res, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": "Title: Served\nURL: https://served.example/\nHighlights:\nfrom the fake"}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", res)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mcpServer) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.names)
}

func (m *mcpServer) endpoint() func() (string, error) {
	return func() (string, error) { return m.srv.URL + "/mcp", nil }
}

// consentAsker answers every question with picked, counting them.
type consentAsker struct {
	mu     sync.Mutex
	picked [][]int
	asked  int
}

func (a *consentAsker) AskQuestion(context.Context, []tool.Question) (tool.Answers, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked++
	return tool.Answers{Answered: true, Picked: a.picked}, nil
}

func (a *consentAsker) PresentPlan(context.Context, tool.PlanOffer) (tool.PlanOutcome, error) {
	return tool.PlanUnanswered, nil
}

func (a *consentAsker) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked
}

// searchOnce runs one turn on s whose model calls websearch, and returns
// what the call answered.
func searchOnce(t *testing.T, f *fixture, s *Session) string {
	t.Helper()
	a := f.models["test/a"]
	before := len(a.requests())
	a.push(callStep(callParts("w1", tool.WebSearchTool, input(t, map[string]any{"query": "golang"}))), answerWith("done"))
	run(t, s, "look it up")
	return strings.Join(promptOf(a.requests()[before+1]), "\n")
}

// TestWebServiceBuiltAtOpen: a state that offers the tools, with the
// endpoint injected, gets a websearch.Service at Open, and its calls reach
// that endpoint; the session's children are handed the same service when it
// opened on, and none when it was asking; a state the seam already gave a
// service keeps it; and with no endpoint injected nothing is built.
func TestWebServiceBuiltAtOpen(t *testing.T) {
	mcp := newMCPServer(t)
	f := newFixture(t, "http://127.0.0.1:1/v1")
	opts := f.options()
	opts.WebEndpoint = mcp.endpoint()
	opts.Web = WebState{Mode: WebOn, Provider: "exa"}
	s := f.open(opts)
	svc, ok := s.base.web.Service.(*websearch.Service)
	if !ok || s.tools.web.Service != tool.Web(svc) {
		t.Fatalf("the session's service %T, its tools' %T: want one websearch.Service", s.base.web.Service, s.tools.web.Service)
	}
	if got := s.childOpenOptions(tableView{}, "", "", &ChildOptions{}).Web; got.Mode != WebOn || got.Service != tool.Web(svc) {
		t.Fatalf("a child of an on session is handed %+v; want the parent's own service", got)
	}
	if history := searchOnce(t, f, s); !strings.Contains(history, "from the fake") {
		t.Fatalf("the call did not reach the endpoint:\n%s", history)
	}
	if got := mcp.calls(); !slices.Equal(got, []string{"web_search_exa"}) {
		t.Fatalf("the endpoint was sent %v", got)
	}

	asking := f.options()
	asking.WebEndpoint = mcp.endpoint()
	asking.Web = WebState{Mode: WebAsking, Provider: "exa", CanAsk: true}
	as := f.open(asking)
	if _, ok := as.base.web.Service.(*websearch.Service); !ok {
		t.Fatalf("an asking session's service is %T", as.base.web.Service)
	}
	if got := as.childOpenOptions(tableView{}, "", "", &ChildOptions{}).Web; got != (WebState{}) {
		t.Fatalf("a child of an asking session is handed %+v; want withheld", got)
	}

	own := &fakeWebService{}
	seamed := f.options()
	seamed.WebEndpoint = mcp.endpoint()
	seamed.Web = WebState{Mode: WebOn, Service: own}
	if got := f.open(seamed).tools.web.Service; got != tool.Web(own) {
		t.Fatalf("the seam's service was replaced by %T", got)
	}

	for _, mode := range []WebMode{WebOn, WebAsking} {
		bare := f.options()
		bare.Web = WebState{Mode: mode}
		if got := f.open(bare).tools.web.Service; got != nil {
			t.Fatalf("mode %v with no endpoint built %T", mode, got)
		}
	}
	withheld := f.options()
	withheld.WebEndpoint = mcp.endpoint()
	if got := f.open(withheld).tools.web.Service; got != nil {
		t.Fatalf("a withheld session built %T", got)
	}

	broken := f.options()
	broken.WebEndpoint = func() (string, error) { return "", errors.New("not loopback") }
	broken.Web = WebState{Mode: WebOn}
	if _, err := Open(broken); err == nil || !strings.Contains(err.Error(), "web search") {
		t.Fatalf("an endpoint that fails: Open = %v", err)
	}
}

// TestWebServiceConsent: the built service asks the session's asker only
// when the state says a person can be asked (CanAsk), and then remembers the
// answer in Home's websearch.json; asking but unable to ask, it asks nobody,
// declines, writes nothing and sends nothing, however willing the asker. A
// failed write of the answer reaches Options.Warn.
func TestWebServiceConsent(t *testing.T) {
	statePath := func(f *fixture) string { return filepath.Join(f.home, websearch.StateFile) }

	t.Run("can ask", func(t *testing.T) {
		mcp := newMCPServer(t)
		f := newFixture(t, "http://127.0.0.1:1/v1")
		a := &consentAsker{picked: [][]int{{0}}}
		opts := f.options()
		opts.Asker, opts.WebEndpoint = a, mcp.endpoint()
		opts.Web = WebState{Mode: WebAsking, Provider: "exa", CanAsk: true}
		s := f.open(opts)
		if history := searchOnce(t, f, s); !strings.Contains(history, "from the fake") {
			t.Fatalf("an allowed call did not reach the endpoint:\n%s", history)
		}
		b, err := os.ReadFile(statePath(f))
		if err != nil || string(b) != `{"version":1,"provider":"exa"}`+"\n" || a.count() != 1 {
			t.Fatalf("remembered %q (%v), asked %d", b, err, a.count())
		}
	})

	t.Run("cannot ask", func(t *testing.T) {
		mcp := newMCPServer(t)
		f := newFixture(t, "http://127.0.0.1:1/v1")
		a := &consentAsker{picked: [][]int{{0}}} // the first option, as a headless policy answers
		opts := f.options()
		opts.Asker, opts.WebEndpoint = a, mcp.endpoint()
		opts.Web = WebState{Mode: WebAsking, Provider: "exa"}
		s := f.open(opts)
		if history := searchOnce(t, f, s); !strings.Contains(history, websearch.DeclinedText) {
			t.Fatalf("the call was not declined:\n%s", history)
		}
		if _, err := os.Stat(statePath(f)); !os.IsNotExist(err) || a.count() != 0 || len(mcp.calls()) != 0 {
			t.Fatalf("state file %v, asked %d, endpoint sent %v", err, a.count(), mcp.calls())
		}
	})

	t.Run("a failed write", func(t *testing.T) {
		mcp := newMCPServer(t)
		f := newFixture(t, "http://127.0.0.1:1/v1")
		if err := os.Mkdir(statePath(f), 0o700); err != nil { // a directory where the file goes
			t.Fatal(err)
		}
		var mu sync.Mutex
		var warned []string
		opts := f.options()
		opts.Asker, opts.WebEndpoint = &consentAsker{picked: [][]int{{0}}}, mcp.endpoint()
		opts.Warn = func(s string) { mu.Lock(); warned = append(warned, s); mu.Unlock() }
		opts.Web = WebState{Mode: WebAsking, Provider: "exa", CanAsk: true}
		s := f.open(opts)
		if history := searchOnce(t, f, s); !strings.Contains(history, "from the fake") {
			t.Fatalf("the answer did not hold for the session:\n%s", history)
		}
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(warned, []string{websearch.WriteFailedText}) {
			t.Fatalf("warned %q", warned)
		}
	})
}

// toServer is a transport that sends every request to srv instead of where
// it was addressed, as it was otherwise: a service on the provider's own
// endpoint — so its key goes with each call — that reaches a fake.
type toServer struct{ srv *httptest.Server }

func (s toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host, r.Host = "http", s.srv.Listener.Addr().String(), ""
	return http.DefaultTransport.RoundTrip(r)
}

// TestWebServiceSendsTheStoredKey (plan 051 §F.5, R3): a key credentials.toml
// stores is the one a session's web service sends, in x-api-key, when
// EXA_API_KEY holds none; an exported key beats it; a key stored or rotated
// after the table was loaded is not sent (one snapshot: the redactor and the
// service are built from the same table, and the redactor holds what is
// sent); and through the test override no key is ever sent, stored or not.
// The service reaches a fake MCP server: on its own endpoint through a
// transport that redirects it (toServer), on the override directly.
func TestWebServiceSendsTheStoredKey(t *testing.T) {
	const (
		stored   = "exa-stored-in-credentials-0001"
		rotated  = "exa-rotated-after-load-0002"
		exported = "exa-exported-for-search-0003"
	)
	var built []websearch.Config
	viaFake := func(t *testing.T, mcp *mcpServer) {
		old := newWebService
		newWebService = func(cfg websearch.Config) (*websearch.Service, error) {
			built = append(built, cfg)
			if cfg.Endpoint == "" { // the provider's own: reach the fake instead
				cfg.HTTPClient = &http.Client{Transport: toServer{mcp.srv}}
			}
			return old(cfg)
		}
		t.Cleanup(func() { newWebService = old })
	}
	store := func(f *fixture, key string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.home, "credentials.toml"), []byte("[exa]\napi_key = \""+key+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name     string
		getenv   func(f *fixture) func(string) string
		override bool
		rotate   bool
		sent     string
	}{
		{"stored, nothing exported", func(f *fixture) func(string) string { return f.getenv }, false, false, stored},
		{"exported beats stored", func(*fixture) func(string) string { return keyEnv("EXA_API_KEY", exported) }, false, false, exported},
		{"an unusable export skips to the stored", func(*fixture) func(string) string { return keyEnv("EXA_API_KEY", "zq9") }, false, false, stored},
		{"rotated between load and Open", func(f *fixture) func(string) string { return f.getenv }, false, true, stored},
		{"the override sends none", func(f *fixture) func(string) string { return f.getenv }, true, false, ""},
		{"the override sends none, exported or not", func(*fixture) func(string) string { return keyEnv("EXA_API_KEY", exported) }, true, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			built = nil
			mcp := newMCPServer(t)
			viaFake(t, mcp)
			f := newFixture(t, "http://127.0.0.1:1/v1")
			store(f, stored)
			f.table = f.load()
			if c.rotate {
				store(f, rotated)
			}
			opts := f.options()
			opts.Getenv = c.getenv(f)
			opts.WebEndpoint = func() (string, error) { return "", nil }
			if c.override {
				opts.WebEndpoint = mcp.endpoint()
			}
			opts.Web = WebState{Mode: WebOn, Provider: "exa"}
			s := f.open(opts)
			if len(built) != 1 {
				t.Fatalf("%d services built; want one", len(built))
			}
			if history := searchOnce(t, f, s); !strings.Contains(history, "from the fake") {
				t.Fatal("the call did not reach the fake")
			}
			mcp.mu.Lock()
			keys := slices.Clone(mcp.keys)
			mcp.mu.Unlock()
			// Named, never printed: which of the test's keys a header held.
			name := func(k string) string {
				if n, ok := map[string]string{stored: "the stored key", rotated: "the rotated key", exported: "the exported key", "": "no key"}[k]; ok {
					return n
				}
				return "another value"
			}
			if len(keys) != 1 || keys[0] != c.sent {
				got := make([]string, len(keys))
				for i, k := range keys {
					got[i] = name(k)
				}
				t.Fatalf("the fake's calls carried x-api-key %v; want one call carrying %s", got, name(c.sent))
			}
			if c.sent != "" && s.Redact("k "+c.sent) != "k "+redact.Marker {
				t.Fatal("the key sent is not one the session's redactor holds")
			}
			if s.Redact("k "+stored) != "k "+redact.Marker {
				t.Fatal("the stored key is not redacted")
			}
			if c.rotate && s.Redact("k "+rotated) != "k "+rotated {
				t.Fatal("control: the rotated key, stored after the load, is redacted at Open: the snapshot is not the table's")
			}
		})
	}
}
