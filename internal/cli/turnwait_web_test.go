package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/transcript"
)

// The web tally at the waiter (plan 051 F.4.4, F.4.5): offered from the info
// document, the turn's tally from its webSearch sections and from a snapshot
// naming it, newest by seq, on every result a native host answered and on no
// exit-1 envelope. The host's side — the windows, the restatement, the freeze —
// is the native session's (internal/agent); here a scripted session stands in
// for it (webStub), publishing the sections a native one would, and a close
// freezes it as the engine asks.

// webStub is the scripted session with the native session's two optional web
// interfaces: it says whether the tools were offered (agent.WebSearcher), and
// keeps a window's tally as the native session does — open, then released as
// webLast — so the engine's settlement restates it and its close freezes it
// (agent.WebTallyFreezer).
type webStub struct {
	*stubSession
	offered bool

	wmu        sync.Mutex
	open, last *agent.WebSearchState
	freezes    []bool
}

var (
	_ agent.WebSearcher     = (*webStub)(nil)
	_ agent.WebTallyFreezer = (*webStub)(nil)
)

func (w *webStub) WebSearch() (offered, known bool) { return w.offered, true }

// Begin forgets the released tally, as every native claim does.
func (w *webStub) Begin(text string) func(context.Context) (agent.Result, error) {
	w.wmu.Lock()
	w.last = nil
	w.wmu.Unlock()
	return w.stubSession.Begin(text)
}

// RestateWebTally is the native session's settlement restatement: the
// released tally, enqueued through the log's outbox ahead of the engine's
// ended, and then spent.
func (w *webStub) RestateWebTally() {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.last != nil {
		w.log.Enqueue(webEv(*w.last))
	}
	w.last = nil
}

// FreezeWebTally is the native session's freeze: the open window's tally, else
// — while the engine's turn is current — the released one's, enqueued through
// the log's outbox ahead of the engine's closing ending; then nothing more.
func (w *webStub) FreezeWebTally(turnCurrent bool) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	w.freezes = append(w.freezes, turnCurrent)
	switch {
	case w.open != nil:
		w.log.Enqueue(webEv(*w.open))
	case turnCurrent && w.last != nil:
		w.log.Enqueue(webEv(*w.last))
	}
	w.open, w.last = nil, nil
}

// report is a call counted in the open window: the whole tally published, as
// a native session's report enqueues it.
func (w *webStub) report(t agent.WebSearchState) {
	w.wmu.Lock()
	w.open, w.last = &t, nil
	w.wmu.Unlock()
	w.emit(webEv(t))
}

// release is the window's close: its tally restated, and kept as webLast.
func (w *webStub) release() {
	w.wmu.Lock()
	t := w.open
	w.last, w.open = t, nil
	w.wmu.Unlock()
	if t != nil {
		w.emit(webEv(*t))
	}
}

func (w *webStub) frozen() []bool {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	return slices.Clone(w.freezes)
}

// webEv is a state delta carrying t, its own copy.
func webEv(t agent.WebSearchState) agent.Event {
	return agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{WebSearch: t.Clone()}}
}

// tally is a web tally.
func tally(searches, fetches, failed int, urls ...string) agent.WebSearchState {
	return agent.WebSearchState{Searches: searches, Fetches: fetches, Failed: failed, URLs: urls}
}

// webRig is a waiting rig whose sessions are web stubs (offered as given), its
// registry naming native; stub is the current engine's.
type webRig struct {
	*waitRig
	mu   sync.Mutex
	last *webStub
}

func newWebRig(t *testing.T, offered bool, ring int, turns ...stubTurn) *webRig {
	t.Helper()
	return newWebRigChained(t, offered, ring, engine.ChainPolicy{}, turns...)
}

// newWebRigChained is newWebRig with every engine's chain policy chain's.
func newWebRigChained(t *testing.T, offered bool, ring int, chain engine.ChainPolicy, turns ...stubTurn) *webRig {
	t.Helper()
	wr := &webRig{}
	wr.waitRig = newWaitRigChained(t, "native", control.Options{Workspace: "/w"}, ring, func(s *stubSession) agent.Session {
		w := &webStub{stubSession: s, offered: offered}
		wr.mu.Lock()
		wr.last = w
		wr.mu.Unlock()
		return w
	}, chain, turns...)
	return wr
}

func (wr *webRig) stub() *webStub {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	return wr.last
}

// wantWeb is a result whose webSearch is want exactly.
func wantWeb(t *testing.T, res *turnResult, want resultWebSearch) {
	t.Helper()
	if res.WebSearch == nil {
		t.Fatalf("the result has no webSearch: %+v", res)
	}
	if !reflect.DeepEqual(*res.WebSearch, want) {
		t.Fatalf("webSearch = %+v, want %+v", *res.WebSearch, want)
	}
}

// webJSON is the result's JSON webSearch object, nil when it has none.
func webJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	w, ok := m["webSearch"]
	if !ok {
		return nil
	}
	o, _ := w.(map[string]any)
	if o == nil {
		t.Fatalf("webSearch is %v, not an object", w)
	}
	return o
}

// onTaken makes done close once the waiter has taken an item match accepts
// (waitTaken).
func onTaken(t *testing.T, match func(pumped) bool) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	see := func(p pumped) {
		if match(p) {
			once.Do(func() { close(done) })
		}
	}
	// Folded, or buffered while the submit's reply is out: the buffer is
	// folded, in order, once the reply is in, ahead of anything after it — a
	// timeout, a signal or a lost stream included.
	prevTaken, prevBuffered := waitTaken, waitBuffered
	waitTaken, waitBuffered = see, see
	t.Cleanup(func() { waitTaken, waitBuffered = prevTaken, prevBuffered })
	return done
}

// timedOutHandle is a plain wait that meets the running turn, looks, and
// times out: its running result, and the handle it names.
func timedOutHandle(t *testing.T, r *webRig) (*turnResult, turnHandle) {
	t.Helper()
	conn, tp := r.dialTapped()
	fire := make(chan time.Time)
	w := goWait(conn, waitStart{}, waitOptions{timeout: fire})
	waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
	close(fire)
	res, f := w.result(t)
	res = wantResult(t, res, f, statusRunning)
	h, err := parseHandle(res.Handle)
	if err != nil {
		t.Fatal(err)
	}
	return res, h
}

// encodedSnapshot is a model folded from evs, at seqs 1, 2, …, cut and
// encoded.
func encodedSnapshot(t *testing.T, evs ...agent.Event) (*transcript.Snapshot, []byte) {
	t.Helper()
	m := transcript.New(transcript.Options{})
	for i, ev := range evs {
		ev.Seq = uint64(i + 1)
		m.Fold(ev)
	}
	snap, err := m.Snapshot(0)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := transcript.EncodeSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	return snap, enc
}

// webItem is an event carrying a web section.
func webItem(p pumped) bool {
	return p.it.Kind == remote.KindEvent && strings.Contains(string(p.it.Body), `"webSearch"`)
}

var offeredInfo = &protocol.WebSearchInfo{Offered: true}

// webWaiter is a waiter from a handle at seq of incarnation inc on turn-1,
// attached with the cursor honoured, its info document's webSearch info: the
// stream from there is the test's (feedEv). Nothing it is fed may make it read
// the host (a synchronized, a partial end_turn's plan lookup).
func webWaiter(inc string, seq uint64, info *protocol.WebSearchInfo) *turnWaiter {
	h := &turnHandle{incarnation: inc, seq: seq, target: waitTarget{kind: targetTurn, id: "turn-1"}}
	w := newTurnWaiter(&waitConn{sessionID: waitRigID}, waitStart{from: h}, waitOptions{})
	w.item(context.Background(), pumped{it: remote.Item{Kind: remote.KindAttached, Reply: &protocol.AttachResult{
		After: protocol.Cursor{Incarnation: inc, Seq: seq}, Session: protocol.SessionInfo{SessionID: waitRigID, WebSearch: info}}}})
	return w
}

// feedEv hands w one event at seq, through the wire's codec.
func feedEv(t *testing.T, w *turnWaiter, seq uint64, ev agent.Event) {
	t.Helper()
	body, err := agent.EncodeEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	w.item(context.Background(), pumped{it: remote.Item{Kind: remote.KindEvent, Seq: seq, Body: []byte(body)}})
}

func startedEv(id string) agent.Event {
	return agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: id, Phase: agent.TurnStarted, Text: "go", Origin: agent.TurnOriginSubmit}}
}

func endedEv(id string) agent.Event {
	return agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: id, Phase: agent.TurnEnded, StopReason: "end_turn"}}
}

// TestWaitWebOfferedOnAStreamedCompletion (finding 6): a turn followed live
// whose model the stream named — so no session.state is read at the end —
// carries offered from the attach reply's info document and the turn's tally.
func TestWaitWebOfferedOnAStreamedCompletion(t *testing.T) {
	r := newWebRig(t, true, 0)
	model := "acme/sonic"
	turn := answerTurn(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Model: &model}}, textEv("found it"))
	turn.before = func() {
		r.stub().report(tally(1, 0, 0, "https://a.example/"))
		r.stub().report(tally(1, 1, 0, "https://a.example/", "https://b.example/"))
		r.stub().release()
	}
	r.script(turn)
	conn, tp := r.dialTapped()
	res, f := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{}).result(t)
	wantResult(t, res, f, statusCompleted)
	wantWeb(t, res, resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 1, Fetches: 1, URLs: []string{"https://a.example/", "https://b.example/"}}})
	if res.Model != model || len(tp.lines(protocol.MethodSessionState)) != 0 {
		t.Fatalf("model %q, state reads %q: the premise is a wait that never reads session.state", res.Model, tp.lines(protocol.MethodSessionState))
	}
}

// TestWaitWebOldHostNoKey: a host whose info document has no webSearch, and a
// stream with no web section — an older host, or a provider that is not
// native — gives a result with no webSearch key at all.
func TestWaitWebOldHostNoKey(t *testing.T) {
	for _, provider := range []string{"grok", "native"} {
		t.Run(provider, func(t *testing.T) {
			r := newWaitRigAs(t, provider, control.Options{Workspace: "/w"}, 0, answerTurn(textEv("an answer")))
			conn := r.dial(nil)
			res, f := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{}).result(t)
			wantResult(t, res, f, statusCompleted)
			if res.WebSearch != nil || webJSON(t, res) != nil {
				t.Fatalf("an older host's result has webSearch: %+v", res.WebSearch)
			}
		})
	}
}

// oldSessionInfo is protocol.SessionInfo as it was before plan 051: what an
// older waiter decodes the info document into.
type oldSessionInfo struct {
	SessionID         string                       `json:"sessionId"`
	ProviderSessionID string                       `json:"providerSessionId"`
	Incarnation       string                       `json:"incarnation"`
	HostID            string                       `json:"hostId"`
	Workspace         string                       `json:"workspace"`
	Provider          protocol.Provider            `json:"provider"`
	Catalogs          protocol.Catalogs            `json:"catalogs"`
	Capabilities      protocol.SessionCapabilities `json:"capabilities"`
	RetryHorizon      protocol.RetryHorizon        `json:"retryHorizon"`
	PermissionMode    protocol.PermissionMode      `json:"permissionMode,omitempty"`
	StartedAt         time.Time                    `json:"startedAt,omitzero"`
}

// stripWeb is body with every webSearch member taken out: the stream as a
// decoder that predates the section reads it (it ignores keys it does not
// know).
func stripWeb(t *testing.T, body string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	if st, ok := m["state"].(map[string]any); ok {
		delete(st, "webSearch")
	}
	if tu, ok := m["turn"].(map[string]any); ok {
		delete(tu, "webSearch")
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestWaitWebNewHostOldWaiter: the new info document, delta and snapshot read
// by a waiter that predates them decode with no error, and give today's
// result: the new waiter's, with nothing but webSearch removed.
func TestWaitWebNewHostOldWaiter(t *testing.T) {
	info := protocol.SessionInfo{SessionID: waitRigID, Incarnation: "inc", Provider: protocol.Provider{Name: "native", Label: "Native"},
		Catalogs: protocol.Catalogs{Models: []protocol.CatalogModel{}, Modes: []protocol.CatalogMode{}}, WebSearch: offeredInfo}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"webSearch":{"offered":true}`) {
		t.Fatalf("the premise: the document carries webSearch: %s", raw)
	}
	var old oldSessionInfo
	if err := json.Unmarshal(raw, &old); err != nil || old.SessionID != waitRigID || old.Provider.Name != "native" {
		t.Fatalf("an older waiter's read of the info document: %+v, %v", old, err)
	}

	// A snapshot with turn.webSearch, and one without: the same snapshot to
	// an older reader.
	snap, enc := encodedSnapshot(t, startedEv("turn-1"), webEv(tally(2, 0, 0, "https://a.example/")), textEv("so far"))
	if snap.Turn.WebSearch == nil {
		t.Fatalf("the premise: a snapshot with the turn's tally: %+v", snap)
	}
	full, err := transcript.DecodeSnapshot(enc)
	if err != nil {
		t.Fatal(err)
	}
	stripped, err := transcript.DecodeSnapshot([]byte(stripWeb(t, string(enc))))
	if err != nil {
		t.Fatalf("a snapshot without the member: %v", err)
	}
	full.Turn.WebSearch = nil
	if !reflect.DeepEqual(full, stripped) {
		t.Fatalf("the snapshot differs but for turn.webSearch:\n%+v\n%+v", full, stripped)
	}

	// The same stream to the new waiter and, stripped, to an older one.
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	evs := []agent.Event{startedEv("turn-1"), webEv(tally(1, 0, 0, "https://a.example/")), textEv("the "),
		usageEv(7, false), webEv(tally(1, 1, 1, "https://a.example/")), textEv("answer"), webEv(tally(1, 1, 1, "https://a.example/")), endedEv("turn-1")}
	run := func(old bool) *turnResult {
		info := offeredInfo
		if old {
			info = nil
		}
		w := webWaiter("inc", 0, info)
		for i, ev := range evs {
			ev.At = at.Add(time.Duration(i) * time.Second)
			body, err := agent.EncodeEvent(ev)
			if err != nil {
				t.Fatal(err)
			}
			if old {
				body = stripWeb(t, body)
			}
			w.item(context.Background(), pumped{it: remote.Item{Kind: remote.KindEvent, Seq: uint64(i + 1), Body: []byte(body)}})
		}
		if w.fail != nil || w.result == nil {
			t.Fatalf("the wait: %+v, %+v", w.result, w.fail)
		}
		return w.result
	}
	newRes, oldRes := run(false), run(true)
	wantWeb(t, newRes, resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 1, Fetches: 1, Failed: 1, URLs: []string{"https://a.example/"}}})
	if oldRes.WebSearch != nil {
		t.Fatalf("the older stream's result has webSearch: %+v", oldRes.WebSearch)
	}
	newRes.WebSearch = nil
	if !reflect.DeepEqual(newRes, oldRes) {
		t.Fatalf("the result differs but for webSearch:\n%+v\n%+v", newRes, oldRes)
	}
}

// TestWaitWebTimeoutRunningCarriesCounts: calls, then a model request that
// stalls, then --timeout: running, with the counts folded so far.
func TestWaitWebTimeoutRunningCarriesCounts(t *testing.T) {
	r := newWebRig(t, true, 0)
	g := r.gate()
	r.script(stubTurn{before: func() {
		r.stub().report(tally(2, 0, 0, "https://a.example/", "https://b.example/"))
		<-g.ch
	}, emit: endTurn().emit, res: endTurn().res})
	taken := onTaken(t, webItem)
	conn := r.dial(nil)
	fire := make(chan time.Time)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{timeout: fire})
	awaited(t, taken, "the tally folded")
	close(fire)
	res, f := w.result(t)
	res = wantResult(t, res, f, statusRunning)
	wantWeb(t, res, resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 2, URLs: []string{"https://a.example/", "https://b.example/"}}})
	if !strings.Contains(res.footer(), "web: 2 searches, 0 fetches") {
		t.Fatalf("the footer:\n%s", res.footer())
	}
}

// TestWaitWebConsentCardNeedsInputCarriesEarlierCounts: a later consent card
// (a question nobody answered) is needs_input with the calls before it; the
// call waiting on the card is not counted yet.
func TestWaitWebConsentCardNeedsInputCarriesEarlierCounts(t *testing.T) {
	r := newWebRig(t, true, 0)
	g := r.gate()
	r.script(stubTurn{before: func() {
		r.stub().report(tally(1, 0, 0, "https://a.example/"))
		q := &agent.QuestionEvent{Title: "Web search", Questions: []agent.Question{{ID: "q1", Prompt: "Allow craze to search the web?",
			Options: []agent.Option{{ID: "allow", Label: "Allow via Exa"}, {ID: "off", Label: "Disable web search"}}}}}
		if _, err := r.s.asks.Open(context.Background(), agent.TurnToken{}, agent.AskRequest{ID: "web-1", Kind: agent.AskQuestion,
			Body: agent.AskBody{Question: q}}); err != nil {
			t.Error(err)
		}
		<-g.ch
	}, emit: endTurn().emit, res: endTurn().res})
	conn := r.dial(nil)
	res, f := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{}).result(t)
	res = wantResult(t, res, f, statusNeedsInput)
	if res.Ask == nil || res.Ask.Kind != "question" || res.Turn != "turn-1" {
		t.Fatalf("the needs_input result: %+v, ask %+v", res, res.Ask)
	}
	wantWeb(t, res, resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 1, URLs: []string{"https://a.example/"}}})
}

// TestWaitWebReattachMidTurnIsTheHostsNumbers: a connection lost mid-turn,
// its cursor evicted, so the re-attach is a snapshot (holding the tally so
// far) and the stream after it (later tallies): the result is the host's
// numbers exactly — a section replaces, so snapshot and replay never double
// count.
func TestWaitWebReattachMidTurnIsTheHostsNumbers(t *testing.T) {
	r := newWebRig(t, true, 4)
	g1, g2 := r.gate(), r.gate()
	first, second := make(chan struct{}), make(chan struct{})
	r.script(stubTurn{before: func() {
		r.stub().report(tally(1, 0, 0, "https://a.example/"))
		close(first)
		<-g1.ch
		r.stub().report(tally(2, 0, 0, "https://a.example/", "https://b.example/"))
		for range 10 {
			r.s.emit(textEv("."))
		}
		close(second)
		<-g2.ch
		r.stub().report(tally(3, 1, 0, "https://a.example/", "https://b.example/", "https://c.example/"))
		r.stub().release()
	}, emit: endTurn().emit, res: endTurn().res})
	taken := onTaken(t, webItem)
	conn, tp := r.dialTapped()
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{})
	<-first
	awaited(t, taken, "the first tally folded")
	redial := r.gate()
	tp.holdRedials(redial)
	tp.drop()
	tp.awaitRedial(t)
	g1.open()
	<-second
	redial.open()
	waitFor(t, "the re-attach", func() bool { return len(tp.lines(protocol.MethodSessionAttach)) == 2 })
	g2.open()
	res, f := w.result(t)
	res = wantResult(t, res, f, statusCompleted)
	wantWeb(t, res, resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 3, Fetches: 1,
		URLs: []string{"https://a.example/", "https://b.example/", "https://c.example/"}}})
	if !res.Partial {
		t.Fatalf("the premise: the re-attach was a snapshot (partial): %+v", res)
	}
}

// TestWaitWebFromAHandleCutAtASnapshot: a plain wait that met the turn
// running — its handle cut at the snapshot, after the turn's calls — times
// out; `wait --from` that handle holds the whole count once synchronized (its
// look seeds it, nothing after the cursor carrying it), and the whole count
// at the end when the ending — and the restatement before it — arrives
// before synchronized.
func TestWaitWebFromAHandleCutAtASnapshot(t *testing.T) {
	r := newWebRig(t, true, 0)
	g := r.gate()
	reported := make(chan struct{})
	r.script(stubTurn{before: func() {
		r.stub().report(tally(2, 0, 0, "https://a.example/", "https://b.example/"))
		close(reported)
		<-g.ch
		r.stub().release()
	}, emit: endTurn().emit, res: endTurn().res})
	r.submit("someone else's")
	<-reported
	waitFor(t, "the tally in the snapshot", func() bool {
		snap, err := r.eng.TranscriptSnapshot("", 0)
		return err == nil && snap.Turn.ID == "turn-1" && snap.Turn.WebSearch != nil
	})
	want := resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 2, URLs: []string{"https://a.example/", "https://b.example/"}}}

	res, h := timedOutHandle(t, r)
	wantWeb(t, res, want)

	// Once synchronized: the cursor honoured, nothing after it yet.
	from, tp2 := r.dialTappedFrom(&protocol.Cursor{Incarnation: h.incarnation, Seq: h.seq}, &tap{})
	// The synchronized item taken is its look done (askCheck runs in it): a
	// timeout before that would cut the look short.
	synced := onTaken(t, func(p pumped) bool { return p.it.Kind == remote.KindSynchronized })
	fire := make(chan time.Time)
	w := goWait(from, waitStart{from: &h}, waitOptions{timeout: fire})
	awaited(t, synced, "the resumed waiter synchronized")
	if len(tp2.lines(protocol.MethodSessionSnapshot)) != 1 {
		t.Fatalf("the premise: one look at synchronized: %q", tp2.lines(protocol.MethodSessionSnapshot))
	}
	close(fire)
	res, f := w.result(t)
	res = wantResult(t, res, f, statusRunning)
	wantWeb(t, res, want)

	// The ending before synchronized.
	g.open()
	r.waitIdle()
	from = r.dial(&protocol.Cursor{Incarnation: h.incarnation, Seq: h.seq})
	res, f = goWait(from, waitStart{from: &h}, waitOptions{}).result(t)
	res = wantResult(t, res, f, statusCompleted)
	wantWeb(t, res, want)
}

// TestWebSeqRule: the newest tally by seq wins. A look ahead of the stream
// seeds at its seq M; the replayed deltas at seq < M and = M are skipped, one
// at > M is taken. A delta and a snapshot at the same seq leave the held
// tally. A seq of 0 always applies.
func TestWebSeqRule(t *testing.T) {
	snapAt := func(seq uint64, turn string, w agent.WebSearchState) *transcript.Snapshot {
		return &transcript.Snapshot{Seq: seq, Turn: transcript.Turn{ID: turn, WebSearch: &transcript.TurnWebSearch{TurnID: turn, WebSearchState: w}}}
	}
	at := func(seq uint64, w agent.WebSearchState) agent.Event {
		ev := webEv(w)
		ev.Seq = seq
		return ev
	}
	a := newTurnAccumulator()
	a.seedWeb(snapAt(10, "turn-1", tally(5, 0, 0)), "turn-1")
	a.fold(at(8, tally(3, 0, 0)))
	if a.web.Searches != 5 {
		t.Fatalf("an older delta lowered the look's tally: %+v", a.web)
	}
	a.fold(at(10, tally(4, 0, 0)))
	if a.web.Searches != 5 {
		t.Fatalf("a delta at the look's seq replaced it: %+v", a.web)
	}
	a.fold(at(11, tally(6, 0, 0)))
	if a.web.Searches != 6 || a.webSeq != 11 {
		t.Fatalf("a newer delta was not taken: %+v at %d", a.web, a.webSeq)
	}
	a.fold(at(12, tally(7, 0, 0)))
	a.seedWeb(snapAt(12, "turn-1", tally(99, 0, 0)), "turn-1")
	if a.web.Searches != 7 {
		t.Fatalf("a snapshot at the delta's seq replaced the held tally: %+v", a.web)
	}
	a.fold(at(0, tally(8, 0, 0)))
	if a.web.Searches != 8 {
		t.Fatalf("a seq of 0 did not apply: %+v", a.web)
	}
	// The accumulator's copy is its own.
	w := tally(1, 0, 0, "https://a.example/")
	b := newTurnAccumulator()
	b.takeWeb(&w, 3)
	w.URLs[0] = "scribbled"
	if b.web.URLs[0] != "https://a.example/" {
		t.Fatal("the accumulator shares the section's URL list")
	}
}

// TestWebSnapshotNamingAnotherTurnIsIgnored: a snapshot's tally is taken only
// when its turnId is the target's — settled from lastTurn (a partial
// settlement cut before the target's own start) or followed from — and the
// result then carries offered alone, never another turn's counts.
func TestWebSnapshotNamingAnotherTurnIsIgnored(t *testing.T) {
	other := &transcript.Snapshot{Seq: 9, Turn: transcript.Turn{WebSearch: &transcript.TurnWebSearch{TurnID: "turn-2",
		WebSearchState: tally(4, 0, 0, "https://other.example/")}}}
	w := waiterAt(nil)
	w.session.web = offeredInfo
	w.settleLastTurn(context.Background(), &protocol.LastTurn{TurnID: "turn-1", Outcome: protocol.TurnDone, StopReason: stopEndTurn}, other)
	res := wantResult(t, w.result, w.fail, statusCompleted)
	wantWeb(t, res, resultWebSearch{Offered: true})
	if !res.Partial {
		t.Fatalf("a settlement from lastTurn is partial: %+v", res)
	}
	w = waiterAt(nil)
	w.followFrom(other, 9, false)
	if w.acc.web != nil {
		t.Fatalf("followed from another turn's tally: %+v", w.acc.web)
	}
}

// TestWebCloseCutoffAtTheWaiter: the turn's calls are done and a first waiter's
// handle falls after their last delta; the session is then closed; `wait
// --from` that handle meets the closing ending before it has synchronized.
// The freeze's delta, ahead of the ending in the host's own stream, carries
// the whole count: the cancelled, stopped result has it. Without it — the
// control — the result would have none.
func TestWebCloseCutoffAtTheWaiter(t *testing.T) {
	stream := endAfterHandle(t, false, false)
	want := resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 2, Fetches: 1, URLs: []string{"https://a.example/", "https://b.example/"}}}
	res := replayFrom(t, stream, true)
	wantStopped(t, res, nil, agent.StopCauseStop)
	wantWeb(t, res, want)
	res = replayFrom(t, stream, false)
	wantStopped(t, res, nil, agent.StopCauseStop)
	wantWeb(t, res, resultWebSearch{Offered: true})
}

// TestWebGapBarrier is Codex's gap: the prompt's window releases and its
// restatement commits; its continuation is held before it returns, so the
// engine's turn is still current; a waiter attaches then, times out, and its
// handle is anchored after the restatement. The engine closes, the freeze
// restating the released window's tally (webLast) ahead of the closing
// ending; `wait --from` the handle with its cursor honoured meets the ending
// before synchronized: the result carries the tally.
func TestWebGapBarrier(t *testing.T) {
	stream := endAfterHandle(t, true, false)
	res := replayFrom(t, stream, true)
	wantStopped(t, res, nil, agent.StopCauseStop)
	wantWeb(t, res, resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 2, Fetches: 1, URLs: []string{"https://a.example/", "https://b.example/"}}})
}

// TestWebSettleGapBarrier is the same gap with no close: the prompt's window
// releases and its restatement commits, its continuation is held, a waiter
// times out with its handle anchored after the restatement, and the turn then
// ends on its own. The engine's settlement restates the released window's
// tally (webLast) ahead of the turn's ended, so `wait --from` the handle,
// meeting the ending before synchronized, still carries the tally. Without
// that delta — the control — the result would have none.
func TestWebSettleGapBarrier(t *testing.T) {
	stream := endAfterHandle(t, true, true)
	res := replayFrom(t, stream, true)
	res = wantResult(t, res, nil, statusCompleted)
	wantWeb(t, res, resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 2, Fetches: 1, URLs: []string{"https://a.example/", "https://b.example/"}}})
	res = replayFrom(t, stream, false)
	res = wantResult(t, res, nil, statusCompleted)
	wantWeb(t, res, resultWebSearch{Offered: true})
}

// handleStream is what the host committed after a handle's cursor, through
// the turn's ending.
type handleStream struct {
	h     turnHandle
	items []remote.Item
}

// endAfterHandle runs a turn that makes its calls and then holds — with its
// window open, or (gap) released and its continuation held before it returns
// — a plain waiter that meets it running and times out, and then the turn's
// end: the engine's close, or (settle) its continuation let go to return
// normally. It answers the timed-out waiter's handle and every event the host
// committed after it through the turn's ending, read by a client attached at
// that handle before the end: the bytes a `wait --from` of the handle would
// replay. It holds that a web section is among them, ahead of the ending.
func endAfterHandle(t *testing.T, gap, settle bool) handleStream {
	t.Helper()
	r := newWebRig(t, true, 0)
	held := make(chan struct{})
	finish := r.gate()
	want := tally(2, 1, 0, "https://a.example/", "https://b.example/")
	r.script(stubTurn{before: func() {
		r.stub().report(tally(1, 0, 0, "https://a.example/"))
		r.stub().report(want)
		if gap {
			r.stub().release()
		}
		close(held)
		select {
		case <-r.s.closed:
		case <-finish.ch:
		}
	}, emit: endTurn().emit, res: endTurn().res})
	r.submit("someone else's")
	<-held
	waitFor(t, "the tally in the snapshot", func() bool {
		snap, err := r.eng.TranscriptSnapshot("", 0)
		return err == nil && snap.Turn.ID == "turn-1" && snap.Turn.WebSearch != nil && snap.Turn.WebSearch.Fetches == 1
	})
	_, h := timedOutHandle(t, r)
	// The handle is past every delta the turn made: nothing after it says the
	// count but what the turn's end brings.
	snap, err := r.eng.TranscriptSnapshot("", 0)
	if err != nil || h.seq < snap.Seq {
		t.Fatalf("the handle %d is not past the turn's deltas (snapshot at %d): %v", h.seq, snap.Seq, err)
	}
	e := r.entry()
	c, err := remote.Dial(stepCtx(t), e.Socket, remote.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	st, err := c.Attach(stepCtx(t), remote.AttachOptions{SessionID: waitRigID, Cursor: &protocol.Cursor{Incarnation: h.incarnation, Seq: h.seq}, When: protocol.WhenNow})
	if err != nil {
		t.Fatal(err)
	}
	out := handleStream{h: h}
	synced := false
	ctx, cancel := context.WithTimeout(context.Background(), stubWatchdog)
	defer cancel()
	for !synced {
		it, err := st.Next(ctx)
		if err != nil {
			t.Fatalf("the capture before the close: %v", err)
		}
		switch it.Kind {
		case remote.KindAttached:
			if it.Reply == nil || it.Reply.Snapshot != nil {
				t.Fatal("the capture's cursor was not honoured")
			}
		case remote.KindEvent:
			t.Fatalf("an event after the handle before the close: %s", it.Body)
		case remote.KindSynchronized:
			synced = true
		}
	}
	endMark := `"stopReason":"closing"`
	if settle {
		endMark = `"phase":"ended"`
		finish.open()
	} else if err := r.eng.CloseFor(agent.StopCauseStop); err != nil {
		t.Fatal(err)
	}
	web, ending := -1, -1
	for ending < 0 {
		it, err := st.Next(ctx)
		if err != nil || it.Kind == remote.KindEnd || it.Kind == remote.KindError {
			break
		}
		if it.Kind != remote.KindEvent {
			continue
		}
		switch body := string(it.Body); {
		case strings.Contains(body, `"webSearch"`):
			web = len(out.items)
		case strings.Contains(body, endMark):
			ending = len(out.items)
		}
		out.items = append(out.items, it)
	}
	wantFreezes := []bool{true}
	if settle {
		wantFreezes = nil
	}
	if got := r.stub().frozen(); !slices.Equal(got, wantFreezes) {
		t.Fatalf("the freeze: %v, want %v", got, wantFreezes)
	}
	// The restating delta (the freeze's, or the settlement's), then the
	// ending, by seq.
	if web < 0 || ending < 0 || out.items[web].Seq >= out.items[ending].Seq {
		t.Fatalf("the host's stream after the handle: %d items, web delta at %d, ending at %d", len(out.items), web, ending)
	}
	return out
}

// replayFrom is a `wait --from` the stream's handle, its cursor honoured,
// that meets every committed event before it has synchronized; withWeb false
// leaves the web sections out (the control: no restating delta).
func replayFrom(t *testing.T, s handleStream, withWeb bool) *turnResult {
	t.Helper()
	w := webWaiter(s.h.incarnation, s.h.seq, offeredInfo)
	w.target = s.h.target
	for _, it := range s.items {
		if !withWeb && strings.Contains(string(it.Body), `"webSearch"`) {
			continue
		}
		w.item(context.Background(), pumped{it: it})
	}
	if w.fail != nil || w.result == nil {
		t.Fatalf("the replay: %+v, %+v", w.result, w.fail)
	}
	return w.result
}

// TestWaitWebIdle: plain craze wait on an idle session settles the last turn
// from the snapshot, its tally included — a turn of craze's, and a wake (the
// agent's own turn) — and with no last turn is idle with offered alone, true
// or false, always written.
func TestWaitWebIdle(t *testing.T) {
	t.Run("after a turn with calls", func(t *testing.T) {
		r := newWebRig(t, true, 0)
		turn := answerTurn(textEv("done"))
		turn.before = func() {
			r.stub().report(tally(1, 1, 0, "https://a.example/"))
			r.stub().release()
		}
		r.script(turn)
		r.submit("go")
		r.waitIdle()
		conn := r.dial(nil)
		res, f := goWait(conn, waitStart{}, waitOptions{}).result(t)
		res = wantResult(t, res, f, statusCompleted)
		if res.Turn != "turn-1" {
			t.Fatalf("the idle result: %+v", res)
		}
		wantWeb(t, res, resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 1, Fetches: 1, URLs: []string{"https://a.example/"}}})
	})
	t.Run("after a wake with calls", func(t *testing.T) {
		r := newWebRig(t, true, 0)
		r.s.setForeign(true)
		r.stub().report(tally(2, 0, 1))
		r.stub().release()
		r.s.setForeign(false)
		waitFor(t, "the wake's ending", func() bool {
			st := r.eng.State()
			return st.LastTurn != nil && st.LastTurn.TurnID == "interject-fallback-1"
		})
		conn := r.dial(nil)
		res, f := goWait(conn, waitStart{}, waitOptions{}).result(t)
		res = wantResult(t, res, f, statusCompleted)
		if res.Turn != "interject-fallback-1" {
			t.Fatalf("the idle result: %+v", res)
		}
		wantWeb(t, res, resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 2, Failed: 1}})
	})
	for _, offered := range []bool{true, false} {
		t.Run(map[bool]string{true: "no last turn, offered", false: "no last turn, not offered"}[offered], func(t *testing.T) {
			r := newWebRig(t, offered, 0)
			conn := r.dial(nil)
			res, f := goWait(conn, waitStart{}, waitOptions{}).result(t)
			res = wantResult(t, res, f, statusIdle)
			wantWeb(t, res, resultWebSearch{Offered: offered})
			if o := webJSON(t, res); len(o) != 1 || o["offered"] != offered {
				t.Fatalf("webSearch on the wire: %v", o)
			}
		})
	}
}

// TestWaitWebAnotherIncarnationsTurn1: an old turn-1, its tally folded, met
// by a re-attach whose snapshot is a reloaded session's (another incarnation,
// its own turn-1 with a tally): unobserved, exit 1, and no webSearch — an
// envelope carries none.
func TestWaitWebAnotherIncarnationsTurn1(t *testing.T) {
	w := webWaiter("inc-a", 0, offeredInfo)
	feedEv(t, w, 1, startedEv("turn-1"))
	feedEv(t, w, 2, webEv(tally(3, 0, 0)))
	_, enc := encodedSnapshot(t, startedEv("turn-1"), webEv(tally(9, 0, 0)))
	w.item(context.Background(), pumped{it: remote.Item{Kind: remote.KindRestore, Reply: &protocol.AttachResult{
		After: protocol.Cursor{Incarnation: "inc-b", Seq: 2}, Snapshot: enc, Session: protocol.SessionInfo{SessionID: waitRigID, WebSearch: offeredInfo}}}})
	if w.result != nil || w.fail == nil || w.fail.kind != errkind.Unobserved {
		t.Fatalf("another incarnation's turn-1: %+v, %+v", w.result, w.fail)
	}
	wantNoWebEnvelope(t, w.fail)
}

// wantNoWebEnvelope is an exit-1 failure whose --json object has no webSearch.
func wantNoWebEnvelope(t *testing.T, f *waitFailure) {
	t.Helper()
	var out bytes.Buffer
	_ = writeFailure(&out, resultOutput{what: waitWhat, json: true}, waitRigShort, f)
	if out.Len() == 0 || strings.Contains(out.String(), "webSearch") {
		t.Fatalf("the envelope: %s", out.String())
	}
}

// TestWaitWebMissedEndingThenANewTurn: the connection lost mid-turn (its
// calls folded), the turn ends and the next one starts before the waiter is
// back, its cursor evicted: the snapshot shows another turn running and no
// last turn, so the ending is unobserved — exit 1, no webSearch.
func TestWaitWebMissedEndingThenANewTurn(t *testing.T) {
	r := newWebRig(t, true, 4)
	g := r.gate()
	first := make(chan struct{})
	r.script(stubTurn{before: func() {
		r.stub().report(tally(1, 0, 0))
		close(first)
		<-g.ch
		for range 10 {
			r.s.emit(textEv("."))
		}
		r.stub().release()
	}, emit: endTurn().emit, res: endTurn().res}, heldTurn(r.gate()))
	taken := onTaken(t, webItem)
	conn, tp := r.dialTapped()
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{})
	<-first
	awaited(t, taken, "the tally folded")
	redial := r.gate()
	tp.holdRedials(redial)
	tp.drop()
	tp.awaitRedial(t)
	g.open()
	r.waitIdle()
	r.submit("the next")
	r.waitStartedInSnapshot("turn-2")
	redial.open()
	res, f := w.result(t)
	if res != nil || f == nil || f.kind != errkind.Unobserved {
		t.Fatalf("a missed ending: %+v, %+v", res, f)
	}
	wantNoWebEnvelope(t, f)
}

// TestWaitWebStatuses is F.4.5's table over the rig: an engine turn's
// completed, failed and cancelled results carry offered and its tally; a
// wake's result its own; a queued row's needs_input behind a turn with calls,
// its --timeout and its drop carry offered alone — never the blocker's.
func TestWaitWebStatuses(t *testing.T) {
	calls := tally(1, 1, 1, "https://a.example/")
	full := resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 1, Fetches: 1, Failed: 1, URLs: []string{"https://a.example/"}}}
	alone := resultWebSearch{Offered: true}
	t.Run("completed", func(t *testing.T) {
		r := newWebRig(t, true, 0)
		turn := answerTurn(textEv("ok"))
		turn.before = func() { r.stub().report(calls); r.stub().release() }
		r.script(turn)
		conn := r.dial(nil)
		res, f := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{}).result(t)
		wantWeb(t, wantResult(t, res, f, statusCompleted), full)
	})
	t.Run("failed", func(t *testing.T) {
		r := newWebRig(t, true, 0)
		r.script(stubTurn{before: func() { r.stub().report(calls); r.stub().release() }, err: errors.New("the provider failed")})
		conn := r.dial(nil)
		res, f := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{}).result(t)
		wantWeb(t, wantResult(t, res, f, statusFailed), full)
	})
	t.Run("cancelled", func(t *testing.T) {
		r := newWebRig(t, true, 0)
		cancelled := r.gate()
		r.s.mu.Lock()
		r.s.onCancel = cancelled.open
		r.s.mu.Unlock()
		r.script(stubTurn{before: func() { r.stub().report(calls); <-cancelled.ch; r.stub().release() },
			emit: []agent.Event{{Type: agent.EventDone, StopReason: "cancelled"}}, res: agent.Result{StopReason: "cancelled"}})
		taken := onTaken(t, webItem)
		conn := r.dial(nil)
		sigs := make(chan os.Signal, 2)
		w := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{sigs: sigs})
		awaited(t, taken, "the tally folded")
		sigs <- syscall.SIGINT
		res, f := w.result(t)
		wantWeb(t, wantResult(t, res, f, statusCancelled), full)
	})
	t.Run("a wake target", func(t *testing.T) {
		r := newWebRig(t, true, 0)
		r.s.setForeign(true)
		waitFor(t, "the wake in the snapshot", func() bool {
			snap, err := r.eng.TranscriptSnapshot("", 0)
			return err == nil && snap.Turn.Foreign != nil && snap.Turn.Foreign.Running
		})
		conn, tp := r.dialTapped()
		w := goWait(conn, waitStart{}, waitOptions{})
		waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
		r.stub().report(calls)
		r.stub().release()
		r.s.setForeign(false)
		res, f := w.result(t)
		res = wantResult(t, res, f, statusCompleted)
		if res.Turn != "interject-fallback-1" {
			t.Fatalf("the wake's result: %+v", res)
		}
		wantWeb(t, res, full)
	})
	// A turn ahead of the row that makes calls and then opens a permission.
	blocker := func(r *webRig) *gate {
		g := r.gate()
		r.script(stubTurn{before: func() {
			r.stub().report(calls)
			r.s.emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell: ls",
				Options: []agent.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}}}})
			<-g.ch
		}, emit: endTurn().emit, res: endTurn().res}, answerTurn(textEv("never")))
		r.submit("ahead")
		waitFor(r.t, "the blocker's open ask", func() bool {
			snap, err := r.eng.TranscriptSnapshot("", 0)
			return err == nil && len(snap.Asks) == 1 && snap.Turn.WebSearch != nil
		})
		return g
	}
	t.Run("a queued row's needs_input", func(t *testing.T) {
		r := newWebRig(t, true, 0)
		blocker(r)
		conn := r.dial(nil)
		res, f := goWait(conn, waitStart{submit: submitPrompt(conn, "behind it")}, waitOptions{}).result(t)
		res = wantResult(t, res, f, statusNeedsInput)
		if res.BlockedBy != "turn-1" || res.Row != "q-1" {
			t.Fatalf("the row's needs_input: %+v", res)
		}
		wantWeb(t, res, alone)
	})
	t.Run("a queued row's timeout", func(t *testing.T) {
		r := newWebRig(t, true, 0)
		g := r.gate()
		r.script(stubTurn{before: func() { r.stub().report(calls); <-g.ch }, emit: endTurn().emit, res: endTurn().res}, answerTurn())
		r.submit("ahead")
		conn, tp := r.dialTapped()
		fire := make(chan time.Time)
		w := goWait(conn, waitStart{submit: submitPrompt(conn, "behind it")}, waitOptions{timeout: fire})
		r.waitQueued(1)
		waitFor(t, "the waiter's target", func() bool { return len(tp.lines(protocol.MethodSessionPrompt)) == 1 })
		waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
		close(fire)
		res, f := w.result(t)
		res = wantResult(t, res, f, statusRunning)
		if res.Row != "q-1" || res.Turn != "" {
			t.Fatalf("the row's timeout: %+v", res)
		}
		wantWeb(t, res, alone)
	})
	t.Run("a dropped row", func(t *testing.T) {
		r := newWebRig(t, true, 0)
		g := r.gate()
		r.script(stubTurn{before: func() { r.stub().report(calls); <-g.ch }, emit: endTurn().emit, res: endTurn().res}, answerTurn())
		r.submit("ahead")
		sigs := make(chan os.Signal, 2)
		conn, tp := r.dialTapped()
		w := goWait(conn, waitStart{submit: submitPrompt(conn, "mine")}, waitOptions{sigs: sigs})
		r.waitQueued(1)
		waitFor(t, "the waiter's target", func() bool { return len(tp.lines(protocol.MethodSessionPrompt)) == 1 })
		sigs <- syscall.SIGTERM
		res, f := w.result(t)
		wantWeb(t, wantResult(t, res, f, statusDropped), alone)
	})
}

// TestWaitWebStopUnseen: a stop whose ending never reached the waiter. For an
// engine-turn target, a stop record naming it — and, with no record, the host
// marked stopping — is the partial cancelled, stopped result, and it keeps
// webSearch: offered and the tally the waiter held. A record naming another
// turn, a row target and a wake target are the exit-1 envelope, which has
// none.
func TestWaitWebStopUnseen(t *testing.T) {
	held := func() *turnWaiter {
		w := webWaiter("inc", 0, offeredInfo)
		feedEv(t, w, 1, startedEv("turn-1"))
		feedEv(t, w, 2, webEv(tally(2, 0, 0, "https://a.example/")))
		feedEv(t, w, 3, textEv("so far"))
		return w
	}
	want := resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 2, URLs: []string{"https://a.example/"}}}
	for _, tc := range []struct {
		name string
		rec  *rundir.StopRecord
	}{
		{"a stop record naming it", &rundir.StopRecord{Turn: "turn-1", Cause: agent.StopCauseStop, At: time.Now()}},
		{"no record, the host marked stopping", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := held()
			w.stopUnseen(tc.rec)
			cause := ""
			if tc.rec != nil {
				cause = tc.rec.Cause
			}
			res := wantStopped(t, w.result, w.fail, cause)
			if !res.Partial {
				t.Fatalf("not partial: %+v", res)
			}
			wantWeb(t, res, want)
			var out, errs bytes.Buffer
			_ = writeResult(&out, &errs, resultOutput{what: waitWhat, json: true}, res)
			if o := webJSON(t, json.RawMessage(bytes.TrimSpace(out.Bytes()))); o == nil || o["searches"] != float64(2) {
				t.Fatalf("the printed result: %s", out.String())
			}
		})
	}
	t.Run("a record naming another turn", func(t *testing.T) {
		w := held()
		w.stopUnseen(&rundir.StopRecord{Turn: "turn-2", Cause: agent.StopCauseStop})
		if w.result != nil || w.fail == nil {
			t.Fatalf("%+v, %+v", w.result, w.fail)
		}
		wantNoWebEnvelope(t, w.fail)
	})
	for _, kind := range []targetKind{targetRow, targetForeign} {
		t.Run("a "+map[targetKind]string{targetRow: "row", targetForeign: "wake"}[kind]+" target", func(t *testing.T) {
			w := held()
			w.target = waitTarget{kind: kind, id: "x-1"}
			w.stopUnseen(nil)
			if w.result != nil || w.fail == nil {
				t.Fatalf("%+v, %+v", w.result, w.fail)
			}
			wantNoWebEnvelope(t, w.fail)
		})
	}
}

// TestWaitWebEnvelopesCarryNone: every exit-1 envelope, of every kind, has no
// webSearch; nor does a result the command builds without a waiter (notSent's
// shape: a session it never had an info document for).
func TestWaitWebEnvelopesCarryNone(t *testing.T) {
	for _, k := range []errkind.Kind{errkind.Unobserved, errkind.Interrupted, errkind.HostExited, errkind.ConnectionLost, errkind.Stopped} {
		wantNoWebEnvelope(t, &waitFailure{msg: "gone", kind: k, handle: "h", usage: &agent.UsageState{}, inFlight: true})
	}
	res := newResult(resultSession{id: waitRigID, provider: "native"}, statusCancelled)
	res.fill(newTurnAccumulator())
	if webJSON(t, res) != nil {
		t.Fatal("a result with no info document has webSearch")
	}
}

// A wake inside the target's bracket (C7 review, finding 1). Under `craze
// prompt`'s chain policy (ChainPolicy.RetryForeignTurn) an engine turn can be
// admitted while a native wake holds the session: its started is on the
// stream, its claim is refused, and the wake's own web sections follow before
// the turn is claimed again. A section belongs to the turn running in the log
// as it arrives, a running foreign bracket's first — the transcript fold's
// rule, which stamps a snapshot's turn.webSearch — so none of the wake's is the
// engine turn's, on the stream as in a snapshot.

// wakeCalls is the wake's tally in these tests; ownCalls the engine turn's.
var (
	wakeCalls = tally(2, 0, 0, "https://wake.example/")
	ownCalls  = tally(1, 0, 0, "https://own.example/")
	ownResult = resultWebSearch{Offered: true, WireWebSearch: agent.WireWebSearch{Searches: 1, URLs: []string{"https://own.example/"}}}
)

func foreignEv(id string, running bool) agent.Event {
	return agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: id, Reason: agent.ReasonSubagentWake, Running: running}}
}

// closingEv is the synthetic ending a close authors for the current turn.
func closingEv(id string) agent.Event {
	return agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: id, Phase: agent.TurnEnded, Synthetic: true, StopReason: stopClosing, Cause: agent.StopCauseStop}}
}

// TestWaitWebAWakeInTheRefusedTurnIsNotItsTally is the review's schedule over
// the rig: a wake is running, `send --wait` submits under RetryForeignTurn, the
// turn starts and its claim is refused, and the wake makes its calls; then the
// wake ends and the turn, claimed again, completes with no call of its own (or
// with one) — or the wait times out, or the session is closed, while the wake
// still runs. The engine turn's result never carries the wake's calls.
func TestWaitWebAWakeInTheRefusedTurnIsNotItsTally(t *testing.T) {
	alone := resultWebSearch{Offered: true}
	for _, tc := range []struct {
		name  string
		calls bool
		end   string // "wake" (the wake ends, the turn completes), "timeout" or "close"
	}{
		{"completed with no call of its own", false, "wake"},
		{"completed with a call of its own", true, "wake"},
		{"timed out while the wake runs", false, "timeout"},
		{"closed while the wake runs", false, "close"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r *webRig
			turn := answerTurn(textEv("done"))
			if tc.calls {
				turn.before = func() { r.stub().report(ownCalls); r.stub().release() }
			}
			r = newWebRigChained(t, true, 0, engine.ChainPolicy{RetryForeignTurn: true}, turn)
			r.s.mu.Lock()
			r.s.refuseWhileForeign = true
			r.s.mu.Unlock()
			r.s.setForeign(true)
			waitFor(t, "the wake in the snapshot", func() bool {
				snap, err := r.eng.TranscriptSnapshot("", 0)
				return err == nil && snap.Turn.Foreign != nil && snap.Turn.Foreign.Running
			})
			taken := onTaken(t, webItem)
			conn := r.dial(nil)
			fire := make(chan time.Time)
			w := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{timeout: fire})
			waitFor(t, "the turn's refused claim", func() bool { return r.eng.State().Waiting })
			r.stub().report(wakeCalls)
			awaited(t, taken, "the wake's section taken")
			switch tc.end {
			case "wake":
				r.stub().release()
				r.s.setForeign(false)
				res, f := w.result(t)
				res = wantResult(t, res, f, statusCompleted)
				if res.Turn != "turn-1" {
					t.Fatalf("the result: %+v", res)
				}
				if tc.calls {
					wantWeb(t, res, ownResult)
				} else {
					wantWeb(t, res, alone)
				}
			case "timeout":
				close(fire)
				res, f := w.result(t)
				wantWeb(t, wantResult(t, res, f, statusRunning), alone)
			case "close":
				if err := r.eng.CloseFor(agent.StopCauseStop); err != nil {
					t.Fatal(err)
				}
				res, f := w.result(t)
				wantWeb(t, wantStopped(t, res, f, agent.StopCauseStop), alone)
			}
		})
	}
}

// fedWaiter is webWaiter from a handle at seq 0 fed evs at seqs 1, 2, ….
func fedWaiter(t *testing.T, evs ...agent.Event) *turnWaiter {
	t.Helper()
	w := webWaiter("inc", 0, offeredInfo)
	for i, ev := range evs {
		feedEv(t, w, uint64(i+1), ev)
	}
	return w
}

// waited is w's result, which it must have.
func waited(t *testing.T, w *turnWaiter) *turnResult {
	t.Helper()
	if w.fail != nil || w.result == nil {
		t.Fatalf("the wait: %+v, %+v", w.result, w.fail)
	}
	return w.result
}

// TestWaitWebStreamedSectionsFollowTheirOwner is the same ownership on a
// stream the waiter folds from a handle (its cursor honoured, so no snapshot
// says what runs): a wake bracket inside the target's, its start on the stream
// or before the handle — its ending then the first the waiter hears of it —
// and the turn ending after it, or closed or timed out while the wake runs.
func TestWaitWebStreamedSectionsFollowTheirOwner(t *testing.T) {
	alone := resultWebSearch{Offered: true}
	for _, tc := range []struct {
		name string
		evs  []agent.Event
		want resultWebSearch
	}{
		{"the wake starts after the turn, which makes no call",
			[]agent.Event{startedEv("turn-1"), foreignEv("wake-1", true), webEv(wakeCalls), webEv(wakeCalls), foreignEv("wake-1", false),
				textEv("done"), endedEv("turn-1")}, alone},
		{"the wake starts before the turn, which makes a call",
			[]agent.Event{foreignEv("wake-1", true), startedEv("turn-1"), webEv(wakeCalls), foreignEv("wake-1", false),
				webEv(ownCalls), textEv("done"), webEv(ownCalls), endedEv("turn-1")}, ownResult},
		{"the wake's start is before the handle, the turn makes no call",
			[]agent.Event{startedEv("turn-1"), webEv(wakeCalls), webEv(wakeCalls), foreignEv("wake-1", false),
				textEv("done"), endedEv("turn-1")}, alone},
		{"the wake's start is before the handle, the turn makes a call",
			[]agent.Event{startedEv("turn-1"), webEv(wakeCalls), foreignEv("wake-1", false),
				webEv(ownCalls), endedEv("turn-1")}, ownResult},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := waited(t, fedWaiter(t, tc.evs...))
			wantWeb(t, wantResult(t, res, nil, statusCompleted), tc.want)
		})
	}
	t.Run("closed while the wake runs", func(t *testing.T) {
		// The close's freeze restates the wake's open window ahead of the
		// closing ending.
		w := fedWaiter(t, startedEv("turn-1"), foreignEv("wake-1", true), webEv(wakeCalls), webEv(wakeCalls), closingEv("turn-1"))
		wantWeb(t, wantStopped(t, waited(t, w), nil, agent.StopCauseStop), alone)
	})
	t.Run("timed out while the wake runs", func(t *testing.T) {
		w := fedWaiter(t, startedEv("turn-1"), foreignEv("wake-1", true), webEv(wakeCalls))
		w.timedOut(nil)
		wantWeb(t, wantResult(t, waited(t, w), nil, statusRunning), alone)
	})
}

// TestWaitWebSnapshotAndStreamAgreeOnAWake: a snapshot cut while a wake runs
// inside the target's bracket stamps its tally with the wake's id, which the
// waiter does not take; followed on from, the wake's later sections are not
// taken either, and the turn's own are. A look standing at the target that
// shows a wake running the waiter never saw start (a handle inside it) says
// the same of the sections the waiter took before it.
func TestWaitWebSnapshotAndStreamAgreeOnAWake(t *testing.T) {
	t.Run("followed from a snapshot", func(t *testing.T) {
		snap, _ := encodedSnapshot(t, startedEv("turn-1"), foreignEv("wake-1", true), webEv(wakeCalls))
		if snap.Turn.WebSearch == nil || snap.Turn.WebSearch.TurnID != "wake-1" {
			t.Fatalf("the premise: the snapshot's tally is the wake's: %+v", snap.Turn.WebSearch)
		}
		w := webWaiter("inc", 0, offeredInfo)
		w.followFrom(snap, snap.Seq, false)
		if w.foreign != "wake-1" || w.acc.web != nil {
			t.Fatalf("followed from the snapshot: foreign %q, tally %+v", w.foreign, w.acc.web)
		}
		feedEv(t, w, 4, webEv(wakeCalls))
		if w.acc.web != nil {
			t.Fatalf("took the wake's section after the snapshot: %+v", w.acc.web)
		}
		feedEv(t, w, 5, foreignEv("wake-1", false))
		feedEv(t, w, 6, webEv(ownCalls))
		feedEv(t, w, 7, endedEv("turn-1"))
		wantWeb(t, waited(t, w), ownResult)
	})
	t.Run("a look shows a wake the waiter never saw start", func(t *testing.T) {
		snap, _ := encodedSnapshot(t, startedEv("turn-1"), foreignEv("wake-1", true), webEv(wakeCalls))
		w := fedWaiter(t, startedEv("turn-1"), webEv(wakeCalls))
		w.lookedAt(snap)
		if w.foreign != "wake-1" || w.acc.web != nil {
			t.Fatalf("after the look: foreign %q, tally %+v", w.foreign, w.acc.web)
		}
		feedEv(t, w, 4, webEv(wakeCalls))
		w.timedOut(nil)
		wantWeb(t, wantResult(t, waited(t, w), nil, statusRunning), resultWebSearch{Offered: true})
	})
}
