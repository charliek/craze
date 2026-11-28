package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/llm/responsesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/coder/websocket"
)

type wsHarnessAPI struct {
	base     string
	upgrades chan *wsHarnessPeer
	requests chan wsHarnessRequest
	posts    chan chan http.HandlerFunc
}

type wsHarnessPeer struct {
	conn    *websocket.Conn
	session string
	closed  chan struct{}
}

type wsHarnessRequest struct {
	peer *wsHarnessPeer
	body struct {
		Type               string           `json:"type"`
		Model              string           `json:"model"`
		PreviousResponseID string           `json:"previous_response_id"`
		Input              []map[string]any `json:"input"`
	}
}

// The handlers keep reading while the test writes response frames. This lets
// the test see a client close an idle or active connection.
func newWSHarnessAPI(t *testing.T) *wsHarnessAPI {
	t.Helper()
	api := &wsHarnessAPI{
		upgrades: make(chan *wsHarnessPeer, 8),
		requests: make(chan wsHarnessRequest, 8),
		posts:    make(chan chan http.HandlerFunc, 8),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("request path = %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			reply := make(chan http.HandlerFunc, 1)
			select {
			case api.posts <- reply:
			case <-r.Context().Done():
				return
			}
			select {
			case send := <-reply:
				send(w, r)
			case <-r.Context().Done():
			}
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			// Close can cancel an upgrade before the handshake is finished.
			return
		}
		conn.SetReadLimit(128 << 20)
		peer := &wsHarnessPeer{conn: conn, session: r.Header.Get("Session-Id"), closed: make(chan struct{})}
		defer close(peer.closed)
		defer func() { _ = conn.CloseNow() }()
		select {
		case api.upgrades <- peer:
		case <-t.Context().Done():
			return
		}
		for {
			typ, body, err := conn.Read(t.Context())
			if err != nil {
				return
			}
			req := wsHarnessRequest{peer: peer}
			if err := json.Unmarshal(body, &req.body); err != nil || typ != websocket.MessageText || req.body.Type != "response.create" {
				t.Errorf("expected response.create, got %s (%v)", body, err)
				return
			}
			select {
			case api.requests <- req:
			case <-t.Context().Done():
				return
			}
		}
	}))
	api.base = srv.URL + "/v1"
	t.Cleanup(srv.Close)
	return api
}

func (p *wsHarnessPeer) send(t *testing.T, frames ...map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	for _, f := range frames {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.conn.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatalf("write %v: %v", f["type"], err)
		}
	}
}

func wsHarnessOptions(t *testing.T, api *wsHarnessAPI, route responsesapi.Route) (*fixture, Options) {
	t.Helper()
	f := newFixture(t, api.base)
	if route == responsesapi.RouteChatGPT {
		onChatGPT(f)
	} else {
		onShippedOpenAI(f, api.base)
		f.table = f.load()
	}
	o := f.options()
	o.NewModel = nil
	o.OpenAITransport = "websocket"
	o.Auth, o.AuthAPIBase = newPlanAuth(), func() (string, error) { return api.base, nil }
	return f, o
}

func wsHarnessStats(t *testing.T, lm fantasy.LanguageModel) responsesapi.Stats {
	t.Helper()
	st, ok := llm.TransportStats(lm)
	if !ok {
		t.Fatal("Responses transport stats unavailable")
	}
	return st
}

func wsEndTurn(t *testing.T, done <-chan outcome) {
	t.Helper()
	if o := await(t, done, "the turn to finish"); o.err != nil || o.res.StopReason != StopEndTurn {
		t.Fatalf("turn = %+v, %v, want end_turn", o.res, o.err)
	}
}

func wsToolFrames(t *testing.T, id string) []map[string]any {
	t.Helper()
	capture, err := os.ReadFile("llm/testdata/openai_sol_commentary_call.sse")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(capture), "\n") {
		data, ok := strings.CutPrefix(line, "data: {")
		if !ok {
			continue
		}
		var ev struct {
			Type string         `json:"type"`
			Item map[string]any `json:"item"`
		}
		if err := json.Unmarshal([]byte("{"+data), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type != "response.output_item.done" || ev.Item["type"] != "reasoning" {
			continue
		}
		if sealed, ok := ev.Item["encrypted_content"].(string); !ok || sealed == "" {
			t.Fatal("captured reasoning has no encrypted content")
		}
		msg := rsMessage("msg_1", "Checking.")
		frames := rsFrames(id, ev.Item, msg, rsCall("call_1", "glob", `{"pattern":"*.go"}`))
		// Exercise the phase-on-added fallback used by both replay paths.
		delete(msg, "phase")
		return frames
	}
	t.Fatal("capture has no reasoning item")
	return nil
}

func TestWSHarnessIncrementalToolLoop(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		route      responsesapi.Route
	}{
		{"ChatGPT", "websocket", responsesapi.RouteChatGPT},
		{"OpenAI key", "auto", responsesapi.RouteKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newWSHarnessAPI(t)
			f, opts := wsHarnessOptions(t, api, tc.route)
			opts.OpenAITransport = tc.mode
			s := f.open(opts)
			peer := await(t, api.upgrades, "session pre-dial")
			if tc.route == responsesapi.RouteChatGPT && peer.session != s.ID() {
				t.Fatalf("pre-dial session = %q, want %q", peer.session, s.ID())
			}
			select {
			case req := <-api.requests:
				t.Fatalf("pre-dial sent a generation request: %+v", req.body)
			default:
			}
			var ev events
			done := start(t.Context(), s, "find Go files", ev.sink)
			first := await(t, api.requests, "first step")
			first.peer.send(t, wsToolFrames(t, "resp_1")...)
			second := await(t, api.requests, "step after the tool")
			if second.peer != first.peer || second.body.PreviousResponseID != "resp_1" {
				t.Fatalf("tool step did not chain on the same socket: %+v", second.body)
			}
			in := second.body.Input
			if len(in) != 1 || in[0]["type"] != "function_call_output" || in[0]["call_id"] != "call_1" {
				t.Fatalf("tool step delta = %+v", in)
			}
			second.peer.send(t, rsFrames("resp_2", rsAnswer("msg_2", "Done.", "final_answer"))...)
			wsEndTurn(t, done)
			if got := of[ToolFinished](ev.list()); len(got) != 1 || got[0].Result.IsError {
				t.Fatalf("tool results = %+v", got)
			}
			if st := wsHarnessStats(t, s.cur.lm); st.WSIncremental == 0 || st.SSERequests != 0 {
				t.Fatalf("transport stats = %+v", st)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			await(t, first.peer.closed, "main socket to close")
		})
	}
}

func TestWSHarnessMidStreamRetry(t *testing.T) {
	for _, budget := range []int{0, 1} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			api := newWSHarnessAPI(t)
			f, opts := wsHarnessOptions(t, api, responsesapi.RouteChatGPT)
			opts.ProviderRetries = &budget
			s := f.open(opts)
			var ev events
			done := start(t.Context(), s, "go", ev.sink)
			req := await(t, api.requests, "first attempt")
			observed := rsFrames("", rsMessage("msg_1", "Partial."))
			req.peer.send(t, observed[:2]...)
			if err := req.peer.conn.CloseNow(); err != nil {
				t.Fatal(err)
			}
			if budget == 1 {
				retry := await(t, api.requests, "replayed attempt")
				if retry.peer == req.peer || retry.body.PreviousResponseID != "" {
					t.Fatal("retry did not start with full input on a fresh socket")
				}
				equal(t, "retry input", retry.body.Input, req.body.Input)
				retry.peer.send(t, rsFrames("resp_retry", rsMessage("msg_retry", "Recovered."))...)
				wsEndTurn(t, done)
				got := of[Retrying](ev.list())
				if len(got) != 1 || !got[0].Discarded || got[0].Attempt != 1 || got[0].Step != 1 {
					t.Fatalf("retry events = %+v, want one that retracts the first attempt", got)
				}
				if got := of[TextDelta](ev.list()); len(got) != 2 || got[0].Text != "Partial." || got[1].Text != "Recovered." {
					t.Fatalf("deltas = %+v", got)
				}
			} else {
				if o := await(t, done, "failed attempt"); o.err == nil || len(of[Retrying](ev.list())) != 0 {
					t.Fatalf("turn = %+v, events = %+v, want failure without retry", o, ev.list())
				}
			}
			if st := wsHarnessStats(t, s.cur.lm); st.WSRequests != uint64(1+budget) || st.SSERequests != 0 {
				t.Fatalf("transport stats = %+v", st)
			}
		})
	}
}

func TestWSHarnessAsidePreservesChain(t *testing.T) {
	for _, where := range []string{"idle", "inter-step retry", "active stream"} {
		t.Run(where, func(t *testing.T) {
			api := newWSHarnessAPI(t)
			f, opts := wsHarnessOptions(t, api, responsesapi.RouteChatGPT)
			s := f.open(opts)
			s.sleep = func(context.Context, time.Duration) {}

			var prev string
			if where == "idle" {
				seed := start(t.Context(), s, "hello", nil)
				first := await(t, api.requests, "seed turn")
				first.peer.send(t, rsFrames("resp_seed", rsMessage("msg_seed", "Hello."))...)
				wsEndTurn(t, seed)
				wsHarnessAside(t, s, api, false)
				prev = "resp_seed"
			}

			between, release := make(chan struct{}), make(chan struct{})
			releaseStep := sync.OnceFunc(func() { close(release) })
			defer releaseStep()
			var seen int
			done := start(t.Context(), s, "find Go files", func(ev Event) {
				if _, ok := ev.(StepDone); ok && where == "inter-step retry" {
					seen++
					if seen == 1 {
						close(between)
						<-release
					}
				}
			})
			first := await(t, api.requests, "main step")
			if first.body.PreviousResponseID != prev {
				t.Fatalf("main step previous ID = %q, want %q", first.body.PreviousResponseID, prev)
			}
			frames := wsToolFrames(t, "resp_tool")
			if where == "active stream" {
				first.peer.send(t, frames[:1]...)
				wsHarnessAside(t, s, api, false)
				frames = frames[1:]
			}
			first.peer.send(t, frames...)
			if where == "inter-step retry" {
				await(t, between, "main step saved between requests")
				wsHarnessAside(t, s, api, true)
				releaseStep()
			}
			next := await(t, api.requests, "next main step")
			if next.peer != first.peer || next.body.PreviousResponseID != "resp_tool" {
				t.Fatalf("main chain after Aside = %+v", next.body)
			}
			next.peer.send(t, rsFrames("resp_done", rsMessage("msg_done", "Done."))...)
			wsEndTurn(t, done)
			wantPosts := uint64(1)
			if where == "inter-step retry" {
				wantPosts = 2
			}
			if st := wsHarnessStats(t, s.cur.lm); st.WSIncremental == 0 || st.SSERequests != wantPosts || st.SSEFallbacks != 0 {
				t.Fatalf("transport stats = %+v, want incremental WS and %d stateless SSE requests", st, wantPosts)
			}
		})
	}
}

func wsHarnessAside(t *testing.T, s *Session, api *wsHarnessAPI, retry bool) {
	t.Helper()
	done := startAside(t.Context(), s, nil, "a side question")
	if retry {
		await(t, api.posts, "first Aside SSE request") <- func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "try again", http.StatusServiceUnavailable)
		}
	}
	await(t, api.posts, "Aside SSE request") <- planText("Side answer.")
	if o := await(t, done, "Aside answer"); o.err != nil || o.a.Text != "Side answer." {
		t.Fatalf("Aside = %+v, %v", o.a, o.err)
	}
}

func TestWSHarnessSetModelDrainsActiveTurnAndAside(t *testing.T) {
	api := newWSHarnessAPI(t)
	f, opts := wsHarnessOptions(t, api, responsesapi.RouteChatGPT)
	s := f.open(opts)
	old := s.cur.lm
	done := start(t.Context(), s, "main question", nil)
	main := await(t, api.requests, "main WS request")
	frames := rsFrames("resp_1", rsMessage("msg_1", "Main answer."))
	main.peer.send(t, frames[:2]...)

	sideDone := startAside(t.Context(), s, nil, "side question")
	side := await(t, api.posts, "active Aside SSE request")
	if err := s.SetModel("test/a"); err != nil {
		t.Fatal(err)
	}
	side <- planText("Side answer.")
	if o := await(t, sideDone, "old model's Aside"); o.err != nil || o.a.Text != "Side answer." {
		t.Fatalf("Aside = %+v, %v", o.a, o.err)
	}
	main.peer.send(t, frames[2:]...)
	wsEndTurn(t, done)
	await(t, main.peer.closed, "old model's drained socket")

	done = start(t.Context(), s, "next question", nil)
	next := await(t, api.requests, "replacement model's request")
	if next.peer == main.peer || next.body.PreviousResponseID != "" {
		t.Fatalf("replacement model reused the prior chain: %+v", next.body)
	}
	next.peer.send(t, rsFrames("resp_2", rsMessage("msg_2", "New answer."))...)
	wsEndTurn(t, done)
	if st := wsHarnessStats(t, old); st.WSRequests != 1 || st.SSERequests != 1 {
		t.Fatalf("old transport stats = %+v", st)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	await(t, next.peer.closed, "replacement model's socket")
}

func TestWSHarnessPreviousModelCompactionClosesTemporaryClient(t *testing.T) {
	api := newWSHarnessAPI(t)
	f, opts := wsHarnessOptions(t, api, responsesapi.RouteChatGPT)
	b := f.table.Models["test/b"]
	b.Provider = modeltable.ChatGPTProvider
	f.table.Models["test/b"] = b
	windowed(f, "test/a", 1_000_000, 4096)
	windowed(f, "test/b", 100_000, 4096)
	s := f.open(opts)
	old := await(t, api.upgrades, "original model's pre-dial")
	seed := start(t.Context(), s, "first turn", nil)
	first := await(t, api.requests, "first model request")
	frames := rsFrames("resp_1", rsMessage("msg_1", "One."))
	usage := frames[len(frames)-1]["response"].(map[string]any)["usage"].(map[string]any)
	usage["input_tokens"], usage["total_tokens"] = 90_000, 90_008
	first.peer.send(t, frames...)
	wsEndTurn(t, seed)
	if err := s.SetModel("test/b"); err != nil {
		t.Fatal(err)
	}
	await(t, old.closed, "old session model's socket")
	next := await(t, api.upgrades, "replacement model's pre-dial")

	turn := start(t.Context(), s, "second turn", nil)
	summary := await(t, api.requests, "previous model's compaction request")
	if summary.peer == first.peer || summary.peer == next || summary.body.Model != "wire-a" || summary.body.PreviousResponseID != "" {
		t.Fatalf("compaction did not use a dedicated previous-model client: %+v", summary.body)
	}
	summary.peer.send(t, rsFrames("resp_summary", rsMessage("msg_summary", longSummary("1. Request and intent\nCarry on.")))...)
	main := await(t, api.requests, "new model's request after compaction")
	await(t, summary.peer.closed, "temporary previous-model socket")
	if main.peer != next || main.body.Model != "wire-b" ||
		!strings.Contains(fmt.Sprint(main.body.Input), "<compacted_context>") {
		t.Fatalf("new request did not use compacted history: %+v", main.body)
	}
	main.peer.send(t, rsFrames("resp_2", rsMessage("msg_2", "Two."))...)
	wsEndTurn(t, turn)
	if rows := compactionEntries(t, s); len(rows) != 1 || !rows[0].Compaction.Succeeded() || rows[0].Model.Alias != "test/a" {
		t.Fatalf("compaction entries = %+v", rows)
	}
}

func TestWSHarnessSubagentOwnsSocket(t *testing.T) {
	for _, tc := range []struct {
		name             string
		background, stop bool
	}{
		{name: "foreground completion"},
		{name: "foreground stop", stop: true},
		{name: "background stop", background: true, stop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			background := tc.background
			api := newWSHarnessAPI(t)
			f, opts := wsHarnessOptions(t, api, responsesapi.RouteChatGPT)
			opts.Background = background
			var own, ev events
			opts.Sink = own.sink
			pending := make(chan struct{}, 4)
			opts.OnPending = func() { pending <- struct{}{} }
			s := f.open(opts)
			children := watchKids(s)
			if inherited := s.childOpenOptions(tableView{table: opts.Table}, "test/a", "high", &ChildOptions{}).OpenAITransport; inherited != opts.OpenAITransport {
				t.Fatalf("child transport = %q, want %q", inherited, opts.OpenAITransport)
			}
			turn := start(t.Context(), s, "ask a child", ev.sink)
			parent := await(t, api.requests, "parent request")
			args := task("inspect project", "child task")
			if background {
				args["run_in_background"] = true
			}
			parent.peer.send(t, rsFrames("resp_parent", rsCall("call_child", "agent", input(t, args)))...)

			var child, parentNext wsHarnessRequest
			if background {
				// A background child and the parent's tool continuation may
				// send their requests in either order.
				for range 2 {
					req := await(t, api.requests, "child or parent continuation")
					if req.peer.session == s.ID() {
						parentNext = req
					} else {
						child = req
					}
				}
			} else {
				child = await(t, api.requests, "foreground child request")
			}
			if child.peer == nil || child.peer == parent.peer || child.peer.session == "" || child.peer.session == s.ID() || child.body.PreviousResponseID != "" {
				t.Fatalf("expected an independent child socket, got %+v", child)
			}
			frames := rsFrames("resp_child", rsMessage("msg_child", "Child answer."))
			if tc.stop {
				child.peer.send(t, frames[:2]...)
				if !background {
					if err := s.CancelSubagent(child.peer.session); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				child.peer.send(t, frames...)
			}
			if !background {
				await(t, child.peer.closed, "foreground child socket after join")
				parentNext = await(t, api.requests, "parent continuation")
			}
			if parentNext.peer != parent.peer || parentNext.body.PreviousResponseID != "resp_parent" {
				t.Fatalf("parent continuation lost its own chain: %+v", parentNext)
			}
			parentNext.peer.send(t, rsFrames("resp_done", rsMessage("msg_done", "Parent done."))...)
			wsEndTurn(t, turn)

			id := child.peer.session
			opened := children.get(id)
			if opened == nil {
				t.Fatalf("no child session for socket session ID %q", id)
			}
			if st := wsHarnessStats(t, opened.cur.lm); st.WSDials != 1 || st.WSRequests != 1 || st.SSERequests != 0 {
				t.Fatalf("child transport stats = %+v", st)
			}
			if background {
				if err := s.CancelSubagent(id); err != nil {
					t.Fatal(err)
				}
				await(t, pending, "stopped child's result")
				await(t, child.peer.closed, "background child socket after stop")
				if fin := finishedOf(t, own.list(), id); fin.Status != SubagentCancelled {
					t.Fatalf("stopped child = %+v", fin)
				}
			}
			if tc.stop && !background {
				if fin := finishedOf(t, ev.list(), id); fin.Status != SubagentCancelled {
					t.Fatalf("stopped foreground child = %+v", fin)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			await(t, parent.peer.closed, "parent socket after join")
			if !storeClosed(opened.store) {
				t.Fatal("child store still open after join")
			}
		})
	}
}
