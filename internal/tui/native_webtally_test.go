package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/transcript"
)

// The web tally over the real engine and a real native session (plan 051
// F.4.2–F.4.3): the session's web deltas in the engine's stream between each
// turn's started and its ended, a queued successor's batch splitting the two
// turns' counts, the close's freeze ahead of the synthetic closing ending —
// also in the gap between a prompt's release and its settlement — and a
// client's fold stamping each delta with the turn running. The calls are the
// real websearch service's, against a fake MCP endpoint. It lives here, not in
// internal/engine, because the engine's tests may not import the harness
// (TestATurnsUsageReachesTheStreamBeforeItsEnd says why).

// webMCP is a fake MCP endpoint answering a search with resultText's text and
// a fetch with a page.
type webMCP struct {
	srv        *httptest.Server
	resultText string
}

func newWebMCP(t *testing.T, resultText string) *webMCP {
	t.Helper()
	m := &webMCP{resultText: resultText}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			ID     int64 `json:"id"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &call)
		text := "# Page\n\nthe page"
		if call.Params.Name == "web_search_exa" {
			text = m.resultText
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", out)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// webStepModel is a scripted model whose steps may wait: a step with a gate
// closes reached when its request arrives and answers only once the gate is
// released, or its context ends.
type webStepModel struct {
	mu    sync.Mutex
	steps []webStep
}

type webStep struct {
	parts   []fantasy.StreamPart
	reached chan struct{}
	gate    chan struct{}
}

func (m *webStepModel) push(steps ...webStep) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, steps...)
}

func (m *webStepModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	if len(m.steps) == 0 {
		m.mu.Unlock()
		return nil, errors.New("webStepModel: no step queued")
	}
	st := m.steps[0]
	m.steps = m.steps[1:]
	m.mu.Unlock()
	if st.gate != nil {
		close(st.reached)
		select {
		case <-st.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range st.parts {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (m *webStepModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not used")
}

func (m *webStepModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not used")
}

func (m *webStepModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not used")
}

func (m *webStepModel) Provider() string { return "test" }
func (m *webStepModel) Model() string    { return "wire-echo" }

// webCallStep calls the web tool name with args and finishes "tool-calls".
func webCallStep(t *testing.T, id, name string, args map[string]any) webStep {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return webStep{parts: nativeToolStep(id, name, string(b))}
}

// webAnswer is a step that answers text and stops.
func webAnswer(text string) webStep {
	return webStep{parts: cat(nativeTextParts(text), nativeFinishParts())}
}

// gated is s held at its request until the test releases it.
func gated(s webStep) webStep {
	s.reached, s.gate = make(chan struct{}), make(chan struct{})
	return s
}

// webExaKey is the web service's key in these sessions: the override is
// never sent it, but its scrub and the session's redactor both hold it.
const webExaKey = "exa key 0123456789abcdef"

// webNative is a native session over model whose web tools are on and reach
// mcp, with webExaKey in its environment.
func webNative(t *testing.T, model *webStepModel, mcp *webMCP) agent.Session {
	t.Helper()
	isolateSkillsHome(t)
	scripted := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	base := nativeSessionTweak(t.TempDir(), nativeOneModelTable(), scripted)
	return agent.NewNative(agent.Options{Workspace: frameWorkspace(t), ContentHome: t.TempDir(), WebFlag: agent.WebFlagOn},
		func(o *harness.Options) {
			base(o)
			getenv := o.Getenv
			o.Getenv = func(k string) string {
				if k == "EXA_API_KEY" {
					return webExaKey
				}
				return getenv(k)
			}
			o.NewModel = func(modeltable.Resolved) (fantasy.LanguageModel, error) { return model, nil }
			o.WebEndpoint = func() (string, error) { return mcp.srv.URL + "/mcp", nil }
		})
}

// startEngine is an engine over sess, started, closed at cleanup.
func startEngine(t *testing.T, sess agent.Session) *engine.Engine {
	t.Helper()
	e, err := engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return e
}

// readUntil reads the engine's stream until pred accepts an event, and
// returns what it read, that event included.
func readUntil(t *testing.T, e *engine.Engine, what string, pred func(agent.Event) bool) []agent.Event {
	t.Helper()
	var evs []agent.Event
	deadline := time.After(wakeWatchdog)
	for {
		select {
		case ev := <-e.Events():
			evs = append(evs, ev)
			if pred(ev) {
				return evs
			}
		case <-deadline:
			t.Fatalf("no %s within %s", what, wakeWatchdog)
		}
	}
}

func turnIs(id, phase string) func(agent.Event) bool {
	return func(ev agent.Event) bool {
		return ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.ID == id && ev.Turn.Phase == phase
	}
}

func webDelta(ev agent.Event) *agent.WebSearchState {
	if ev.Type == agent.EventMeta && ev.State != nil {
		return ev.State.WebSearch
	}
	return nil
}

// webDeltas is every web delta among evs, in order.
func webDeltas(evs []agent.Event) []agent.Event {
	var webs []agent.Event
	for _, ev := range evs {
		if webDelta(ev) != nil {
			webs = append(webs, ev)
		}
	}
	return webs
}

// noWebAfter drains what the engine's primary holds now: none of it, all
// after what, is a web delta.
func noWebAfter(t *testing.T, e *engine.Engine, what string) {
	t.Helper()
	for {
		select {
		case ev := <-e.Events():
			if webDelta(ev) != nil {
				t.Fatalf("a web delta after %s: %+v", what, webDelta(ev))
			}
		default:
			return
		}
	}
}

// foldAll is a client's model folded from evs.
func foldAll(evs []agent.Event) *transcript.Model {
	m := transcript.New(transcript.Options{})
	for _, ev := range evs {
		m.Fold(ev)
	}
	return m
}

// TestWebTallyOverTheEngine: a turn's search and fetch through the real
// service publish their tally only between the turn's started and its ended,
// the restatements last (the release's, then the settlement's); a client's fold stamps it with the turn's id and
// keeps it after the turn ends; the snapshot carries it — and no spelling of
// the service's key. A queued successor (its started in the settlement's
// batch, right behind the ended) shows none of its predecessor's calls.
func TestWebTallyOverTheEngine(t *testing.T) {
	leak := "https://leak.example/?k=" + url.QueryEscape(webExaKey)
	mcp := newWebMCP(t, "Title: A\nURL: https://a.example/\nText: x\n---\nTitle: Leak\nURL: "+leak+"\nText: y")
	model := &webStepModel{}
	held := gated(webAnswer("one done"))
	model.push(
		webCallStep(t, "w1", tool.WebSearchTool, map[string]any{"query": "q"}),
		webCallStep(t, "w2", tool.WebFetchTool, map[string]any{"url": "https://b.example/page"}),
		held,
		webCallStep(t, "w3", tool.WebFetchTool, map[string]any{"url": "https://c.example/"}),
		webAnswer("two done"),
	)
	e := startEngine(t, webNative(t, model, mcp))
	one, err := e.Submit(engine.Command{}, "first", engine.SubmitQueue, "")
	if err != nil || one.Turn == "" {
		t.Fatalf("submit = %+v, %v", one, err)
	}
	wakeAwait(t, held.reached, "the first turn's last step")
	if _, err := e.Submit(engine.Command{}, "second", engine.SubmitQueue, ""); err != nil {
		t.Fatalf("the second submit: %v", err)
	}
	close(held.gate)
	evs := readUntil(t, e, "the queued turn's ending", func(ev agent.Event) bool {
		return ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnEnded && ev.Turn.ID != one.Turn && ev.Turn.ID != ""
	})
	turn2 := evs[len(evs)-1].Turn.ID
	st1, en1 := slices.IndexFunc(evs, turnIs(one.Turn, agent.TurnStarted)), slices.IndexFunc(evs, turnIs(one.Turn, agent.TurnEnded))
	st2, en2 := slices.IndexFunc(evs, turnIs(turn2, agent.TurnStarted)), len(evs)-1
	if st2 != en1+1 {
		t.Fatalf("the successor's started (%d) is not right behind the ending (%d)", st2, en1)
	}
	var in1, in2 []int
	for i, ev := range evs {
		w := webDelta(ev)
		if w == nil {
			continue
		}
		for _, sp := range []string{webExaKey, url.QueryEscape(webExaKey), url.PathEscape(webExaKey), "0123456789abcdef"} {
			for _, u := range w.URLs {
				if strings.Contains(u, sp) {
					t.Fatalf("a delta's URL %q carries the key", u)
				}
			}
		}
		switch {
		case i > st1 && i < en1:
			in1 = append(in1, i)
		case i > st2 && i < en2:
			in2 = append(in2, i)
		default:
			t.Fatalf("a web delta at %d lies in no turn (turn one %d..%d, turn two %d..%d)", i, st1, en1, st2, en2)
		}
	}
	if len(in1) != 4 || len(in2) != 3 {
		t.Fatalf("web deltas: %d in turn one, %d in turn two; want 4 (two calls, the release's restatement, the settlement's) and 3",
			len(in1), len(in2))
	}
	final1 := webDelta(evs[in1[2]])
	if final1.Searches != 1 || final1.Fetches != 1 || len(final1.URLs) != 3 || final1.URLs[0] != "https://a.example/" || final1.URLs[2] != "https://b.example/page" {
		t.Fatalf("turn one's restatement is %+v", final1)
	}
	final2 := webDelta(evs[in2[1]])
	if final2.Searches != 0 || final2.Fetches != 1 || !slices.Equal(final2.URLs, []string{"https://c.example/"}) {
		t.Fatalf("turn two's restatement is %+v", final2)
	}
	// Each settlement restates the release's tally, the same value, last.
	for _, c := range []struct{ release, settle *agent.WebSearchState }{{final1, webDelta(evs[in1[3]])}, {final2, webDelta(evs[in2[2]])}} {
		if !reflect.DeepEqual(c.release, c.settle) {
			t.Fatalf("the settlement restated %+v; the release %+v", c.settle, c.release)
		}
	}

	// A client's fold, cut right after turn one ended: its tally, stamped.
	m := foldAll(evs[:en1+1])
	tw := m.State().Turn.WebSearch
	if tw == nil || tw.TurnID != one.Turn || tw.Searches != 1 || tw.Fetches != 1 {
		t.Fatalf("the fold after turn one's end holds %+v; want turn one's tally", tw)
	}
	snap, err := foldAll(evs).Snapshot(0)
	if err != nil {
		t.Fatal(err)
	}
	if tw := snap.Turn.WebSearch; tw == nil || tw.TurnID != turn2 || tw.Fetches != 1 || tw.Searches != 0 {
		t.Fatalf("the snapshot after turn two holds %+v; want turn two's", tw)
	}
	cut, err := foldAll(evs[:en1+1]).Snapshot(0)
	if err != nil {
		t.Fatal(err)
	}
	body, err := transcript.EncodeSnapshot(cut)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"webSearch"`) {
		t.Fatalf("the snapshot carries no turn.webSearch: %s", body)
	}
	for _, sp := range []string{webExaKey, url.QueryEscape(webExaKey), url.PathEscape(webExaKey), "0123456789abcdef"} {
		if strings.Contains(string(body), sp) {
			t.Fatalf("the snapshot carries the key (%q)", sp)
		}
	}
}

// TestWebFreezeOverTheEngine: Close with a turn current and a call counted:
// the freeze's delta is committed at a lower seq than the synthetic closing
// ending, and the fold holds that turn's tally once the ending is folded.
func TestWebFreezeOverTheEngine(t *testing.T) {
	mcp := newWebMCP(t, "Title: A\nURL: https://a.example/\nText: x")
	model := &webStepModel{}
	held := gated(webAnswer("never"))
	model.push(webCallStep(t, "w1", tool.WebSearchTool, map[string]any{"query": "q"}), held)
	e := startEngine(t, webNative(t, model, mcp))
	res, err := e.Submit(engine.Command{}, "go", engine.SubmitQueue, "")
	if err != nil {
		t.Fatal(err)
	}
	wakeAwait(t, held.reached, "the turn's second step")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	evs := readUntil(t, e, "the closing ending", turnIs(res.Turn, agent.TurnEnded))
	ending := evs[len(evs)-1]
	if !ending.Turn.Synthetic || ending.Turn.StopReason != "closing" {
		t.Fatalf("the ending is %+v", ending.Turn)
	}
	var freeze *agent.Event
	for i := range evs {
		if webDelta(evs[i]) != nil {
			freeze = &evs[i]
		}
	}
	if freeze == nil || freeze.Seq >= ending.Seq || webDelta(*freeze).Searches != 1 {
		t.Fatalf("the last web delta before the closing ending: %+v", freeze)
	}
	if tw := foldAll(evs).State().Turn.WebSearch; tw == nil || tw.TurnID != res.Turn || tw.Searches != 1 {
		t.Fatalf("the fold holds %+v", tw)
	}
	noWebAfter(t, e, "the closing ending")
}

// gapNative is what the engine finds on the native session, and gapSession
// passes on.
type gapNative interface {
	agent.Session
	agent.LogOwner
	agent.AskSource
	agent.AdmissionFence
	agent.Clocked
	agent.PromptAdmitter
	agent.OwedWork
	agent.SeedAnnouncer
	agent.WebTallyFreezer
	agent.WebSearcher
}

// gapRig is an engine over a native session held in the gap: one search,
// then an answer, submitted; it answers once the continuation has returned to
// the gap (g.returned), held there until g.hold closes, with the turn's id.
func gapRig(t *testing.T) (*gapSession, *engine.Engine, string) {
	t.Helper()
	mcp := newWebMCP(t, "Title: A\nURL: https://a.example/\nText: x")
	model := &webStepModel{}
	model.push(webCallStep(t, "w1", tool.WebSearchTool, map[string]any{"query": "q"}), webAnswer("done"))
	inner, ok := webNative(t, model, mcp).(gapNative)
	if !ok {
		t.Fatal("fixture: the native session is not every interface the engine looks for")
	}
	g := &gapSession{gapNative: inner, returned: make(chan struct{}), hold: make(chan struct{})}
	e := startEngine(t, g)
	res, err := e.Submit(engine.Command{}, "go", engine.SubmitQueue, "")
	if err != nil {
		t.Fatal(err)
	}
	wakeAwait(t, g.returned, "the continuation's return")
	return g, e, res.Turn
}

// gapSession holds each continuation after the native session's own has
// returned — its window released, its restatement enqueued — and before it
// returns to the engine: the gap between a prompt's release and its
// settlement.
type gapSession struct {
	gapNative
	returned chan struct{}
	hold     chan struct{}
}

func (g *gapSession) Begin(text string) func(context.Context) (agent.Result, error) {
	run := g.gapNative.Begin(text)
	return func(ctx context.Context) (agent.Result, error) {
		res, err := run(ctx)
		close(g.returned)
		<-g.hold
		return res, err
	}
}

func (g *gapSession) Prompt(ctx context.Context, text string) (agent.Result, error) {
	return g.Begin(text)(ctx)
}

// TestWebFreezeInTheGapOverTheEngine (Codex's gap barrier): the window has
// released and its restatement is committed; the continuation is held before
// it returns to the engine; the engine closes. The freeze restates the
// released prompt's tally (webLast) — the same value, replacing — ahead of
// the closing ending, and the fold stamps it with that turn's id.
func TestWebFreezeInTheGapOverTheEngine(t *testing.T) {
	g, e, turn := gapRig(t)
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	evs := readUntil(t, e, "the closing ending", turnIs(turn, agent.TurnEnded))
	close(g.hold)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	ending := evs[len(evs)-1]
	if !ending.Turn.Synthetic || ending.Turn.StopReason != "closing" {
		t.Fatalf("the turn ended %+v; want the close's synthetic ending (the gap was not held)", ending.Turn)
	}
	webs := webDeltas(evs)
	if len(webs) != 3 {
		t.Fatalf("%d web deltas; want the call's, the restatement and the freeze's restated webLast", len(webs))
	}
	restate, dup := webDelta(webs[1]), webDelta(webs[2])
	if dup.Searches != 1 || dup.Searches != restate.Searches || !slices.Equal(dup.URLs, restate.URLs) || webs[2].Seq >= ending.Seq {
		t.Fatalf("the freeze's delta %+v (seq %d), the restatement %+v, the ending at seq %d", dup, webs[2].Seq, restate, ending.Seq)
	}
	// A waiter whose cursor fell after the restatement folds the freeze's
	// delta and the ending: the turn's whole count.
	at := slices.IndexFunc(evs, func(ev agent.Event) bool { return ev.Seq == webs[1].Seq })
	m := foldAll(evs[:at+1])
	if tw := m.State().Turn.WebSearch; tw == nil || tw.TurnID != turn {
		t.Fatalf("fixture: the fold up to the restatement holds %+v", tw)
	}
	tw := foldAll(evs).State().Turn.WebSearch
	if tw == nil || tw.TurnID != turn || tw.Searches != 1 {
		t.Fatalf("the fold after the closing ending holds %+v; want the turn's tally, stamped with %s", tw, turn)
	}
}

// TestWebSettleRestatesInTheGapOverTheEngine: the same gap with no close —
// the window released, its restatement committed, the continuation held —
// and then the continuation returns. The settlement restates the released
// prompt's tally (webLast) — the same value, replacing — ahead of the turn's
// own ended, so a waiter whose cursor fell after the release's restatement
// folds the whole count with the ending; and a close afterwards restates
// nothing more (webLast is spent, no turn current).
func TestWebSettleRestatesInTheGapOverTheEngine(t *testing.T) {
	g, e, turn := gapRig(t)
	close(g.hold)
	evs := readUntil(t, e, "the turn's ended", turnIs(turn, agent.TurnEnded))
	ending := evs[len(evs)-1]
	if ending.Turn.Synthetic || ending.Turn.StopReason != "end_turn" {
		t.Fatalf("the turn ended %+v; want its own end_turn", ending.Turn)
	}
	webs := webDeltas(evs)
	if len(webs) != 3 {
		t.Fatalf("%d web deltas; want the call's, the restatement and the settlement's restated webLast", len(webs))
	}
	restate, dup := webDelta(webs[1]), webDelta(webs[2])
	if dup.Searches != 1 || dup.Searches != restate.Searches || !slices.Equal(dup.URLs, restate.URLs) || webs[2].Seq >= ending.Seq {
		t.Fatalf("the settlement's delta %+v (seq %d), the restatement %+v, the ended at seq %d", dup, webs[2].Seq, restate, ending.Seq)
	}
	// A waiter whose cursor fell after the release's restatement folds the
	// settlement's delta and the ended: the turn's whole count.
	// Its model knows the turn running (its started) and nothing of the tally.
	at := slices.IndexFunc(evs, func(ev agent.Event) bool { return ev.Seq == webs[1].Seq })
	started := evs[slices.IndexFunc(evs, turnIs(turn, agent.TurnStarted))]
	after := foldAll(append([]agent.Event{started}, evs[at+1:]...))
	if tw := after.State().Turn.WebSearch; tw == nil || tw.TurnID != turn || tw.Searches != 1 {
		t.Fatalf("the fold after the cursor holds %+v; want the turn's tally, stamped with %s", tw, turn)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	noWebAfter(t, e, "the settlement")
}
