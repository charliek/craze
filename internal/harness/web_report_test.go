package harness

import (
	"context"
	"slices"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/tool"
)

// Options.OnWebCall (plan 051 F.4.1): the service a session builds at Open
// reports every call it handled there, the session's own and its children's
// alike; a call the tool refused in Prepare never reaches it, and a retried
// step's failed attempt ran no tool, so it reports nothing.

// webCalls records what OnWebCall is told.
type webCalls struct {
	mu  sync.Mutex
	got []WebCall
}

func (w *webCalls) on(c WebCall) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, c)
}

func (w *webCalls) all() []WebCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.got)
}

// TestOnWebCallReportsTheSessionAndItsChildren: one report per call, a
// child's through the parent's function (it shares the parent's service).
func TestOnWebCallReportsTheSessionAndItsChildren(t *testing.T) {
	mcp := newMCPServer(t)
	f := newRouted(t)
	rec := &webCalls{}
	opts := f.options()
	opts.WebEndpoint = mcp.endpoint()
	opts.Web = WebState{Mode: WebOn, Provider: "exa"}
	opts.OnWebCall = rec.on
	s := f.open(opts)
	a := f.routers["test/a"]
	a.route("fan out",
		webCall(t, "p1", "parent query", "https://example.com/parent"),
		callStep(agentPart(t, "a1", task("look", "child one", "subagent_type", "general-purpose"))),
		answerWith("done"))
	a.route("child one", webCall(t, "c1", "child query", "https://example.com/child"), answerWith("child done"))
	run(t, s, "fan out")
	got := rec.all()
	if len(got) != 4 || len(mcp.calls()) != 4 {
		t.Fatalf("%d reports for %d endpoint calls, want 4 each: %+v", len(got), len(mcp.calls()), got)
	}
	var searches, fetches int
	for _, c := range got {
		if c.Failed {
			t.Fatalf("a call reported failed: %+v", c)
		}
		if c.Fetch {
			fetches++
			continue
		}
		searches++
		if !slices.Equal(c.URLs, []string{"https://served.example/"}) {
			t.Fatalf("a search reported %v", c.URLs)
		}
	}
	if searches != 2 || fetches != 2 {
		t.Fatalf("%d searches and %d fetches reported, want 2 each", searches, fetches)
	}
}

// TestAPrepareRefusalIsNotReported: a webfetch the tool refuses — a URL that
// is not public, one that is not http — never reaches the service, so it is
// an ordinary failed tool call and no report.
func TestAPrepareRefusalIsNotReported(t *testing.T) {
	mcp := newMCPServer(t)
	f := newFixture(t, "http://127.0.0.1:1/v1")
	rec := &webCalls{}
	opts := f.options()
	opts.WebEndpoint = mcp.endpoint()
	opts.Web = WebState{Mode: WebOn, Provider: "exa"}
	opts.OnWebCall = rec.on
	s := f.open(opts)
	a := f.models["test/a"]
	a.push(callStep(
		callParts("w1", tool.WebFetchTool, input(t, map[string]any{"url": "http://127.0.0.1:8080/admin"})),
		callParts("w2", tool.WebFetchTool, input(t, map[string]any{"url": "file:///etc/passwd"})),
	), answerWith("done"))
	run(t, s, "fetch them")
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("refused calls were reported: %+v", got)
	}
	if got := mcp.calls(); len(got) != 0 {
		t.Fatalf("refused calls reached the endpoint: %v", got)
	}
}

// TestARetriedStepReportsNothingMore: a step whose stream is reset after a
// complete websearch call is sent again; the failed attempt's call never ran
// (StepRetryInfo's contract), so the turn reports one call, its
// replacement's.
func TestARetriedStepReportsNothingMore(t *testing.T) {
	mcp := newMCPServer(t)
	call := func(id string) string {
		return toolCallChunk(id, tool.WebSearchTool, input(t, map[string]any{"query": "golang"}))
	}
	w := newH2Wire(t,
		resetAfter(textChunk("abandoned "), call("call_1")),
		sseReply(call("call_1"), finishChunk("tool_calls", true)),
		sseReply(textChunk("done"), finishChunk("stop", true)),
	)
	f := newFixture(t, w.url)
	rec := &webCalls{}
	o := f.options()
	o.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
		lm, err := llm.New(r, llm.WithHTTPClient(w.client))
		return fastRetries{lm}, err
	}
	o.WebEndpoint = mcp.endpoint()
	o.Web = WebState{Mode: WebOn, Provider: "exa"}
	o.OnWebCall = rec.on
	s := f.open(o)
	var ev events
	res, err := s.Run(context.Background(), "go", ev.sink)
	if err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v; want end_turn", res, err)
	}
	if n := len(of[Retrying](ev.list())); n != 1 {
		t.Fatalf("%d Retrying, want 1: the step was not retried", n)
	}
	if got := rec.all(); len(got) != 1 || got[0].Fetch || got[0].Failed {
		t.Fatalf("reports = %+v; want the replacement's one search", got)
	}
	if got := mcp.calls(); len(got) != 1 {
		t.Fatalf("the endpoint was called %d times, want once", len(got))
	}
}
