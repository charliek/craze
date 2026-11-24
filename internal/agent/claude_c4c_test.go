package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// The C4c fixes (plan 052, review C4b): each test parks the gap its finding
// names, and fails on the code before its fix.

// pipeClaude is a claude session over a real ACP client on a pipe, its
// session/new answered: the client routes what push writes as the adapter's
// frames, through the registry and the foreign-turn machine, into the session.
type pipeClaude struct {
	s      *session
	log    *eventLog
	client *acp.Client
	enc    *acp.Encoder
	sync   int
}

func newPipeClaude(t *testing.T, wrap func(*session, func(acp.SubagentNotification)) func(acp.SubagentNotification)) *pipeClaude {
	t.Helper()
	p := ClaudeProvider()
	s := newSession(Options{Provider: &p})
	s.sessionID = "root"
	t.Cleanup(func() { _ = s.Close() })
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	t.Cleanup(func() { _ = cr.Close(); _ = cw.Close(); _ = sr.Close(); _ = sw.Close() })
	client := acp.DialWithDialect(cr, cw, acp.DialectClaude)
	t.Cleanup(func() { _ = client.Close() })
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
	client.SetUpdateHandler(s.onUpdate)
	sub := s.onSubagent
	if wrap != nil {
		sub = wrap(s, sub)
	}
	client.SetSubagentHandler(sub)
	client.SetForeignTurnHandler(s.onACPForeignTurn)
	pc := &pipeClaude{s: s, log: collect(t, s), client: client, enc: acp.NewEncoder(sw)}
	dec := acp.NewDecoder(sr)
	done := make(chan error, 1)
	go func() {
		_, err := client.NewSession(context.Background(), t.TempDir(), acp.SessionParams{})
		done <- err
	}()
	req, err := dec.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.enc.WriteMessage(&acp.Message{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"sessionId":"root"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Whatever else the client writes is read and dropped, so it never blocks.
	go func() {
		for {
			if _, err := dec.ReadMessage(); err != nil {
				return
			}
		}
	}()
	return pc
}

func (p *pipeClaude) push(t *testing.T, sid string, update map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"sessionId": sid, "update": update})
	if err := p.enc.WriteMessage(&acp.Message{JSONRPC: "2.0", Method: acp.MethodSessionUpdate, Params: raw}); err != nil {
		t.Fatal(err)
	}
}

// barrier waits until everything pushed so far has reached the session and
// the test's collector: a mode update behind them on the wire. The snapshot
// alone is not enough — it holds the mode before the collector's goroutine
// has appended the events published ahead of it (a slow macOS -race runner
// read an empty log that way) — so the barrier also waits for the mode's own
// event, which the session publishes after every event the pushes before it
// produced: the subagent handler runs on the read goroutine in wire order.
func (p *pipeClaude) barrier(t *testing.T) {
	t.Helper()
	p.sync++
	mode := fmt.Sprintf("sync-%d", p.sync)
	p.push(t, "root", map[string]any{"sessionUpdate": acp.UpdateCurrentMode, "currentModeId": mode})
	waitFor(t, "the barrier "+mode, func() bool { return p.s.Snapshot().CurrentMode == mode })
	waitFor(t, "the collector at the barrier "+mode, func() bool {
		for _, ev := range p.log.snapshot() {
			if ev.Mode == mode {
				return true
			}
		}
		return false
	})
}

func announce(child, state string) map[string]any {
	return map[string]any{"sessionUpdate": "subagent_update", "sessionId": child, "title": "t-" + child, "description": "general-purpose",
		"capabilities": map[string]any{"cancel": map[string]any{}}, "state": map[string]any{"state": state}}
}

func idleUpdate(child string) map[string]any {
	return map[string]any{"sessionUpdate": "subagent_update", "sessionId": child, "state": map[string]any{"state": "idle", "stopReason": "end_turn"}}
}

// TestClaudeForeignAStartParkedBeforeItsEventYieldsToNoEnding (review C4b item
// 1): with a start parked between its state change and its event, an ending
// delivered meanwhile — the close's, without the dispatch lock — cannot be
// published ahead of it: the stream shows the start, then the ending, and the
// session is left with no foreign turn.
func TestClaudeForeignAStartParkedBeforeItsEventYieldsToNoEnding(t *testing.T) {
	s, log := claudeUnit(t)
	parked, release := make(chan struct{}), make(chan struct{})
	testForeignStartPublish = func() { close(parked); <-release }
	t.Cleanup(func() { testForeignStartPublish = nil })
	started := make(chan struct{})
	go func() {
		s.onForeignTurn(ForeignTurnInfo{ID: "claude-1", Reason: ReasonBackground, Running: true})
		close(started)
	}()
	<-parked
	if s.foreignMu.TryLock() {
		s.foreignMu.Unlock()
		t.Fatal("a start parked before its event does not hold the bracket lock")
	}
	ended := make(chan struct{})
	go func() {
		s.onForeignTurn(ForeignTurnInfo{ID: "claude-1", Reason: ReasonBackground, Err: &TurnError{Kind: "auth", Message: "stopped"}})
		close(ended)
	}()
	close(release)
	<-started
	<-ended
	waitFor(t, "both brackets", func() bool { return len(foreignBrackets(log.snapshot())) == 2 })
	b := foreignBrackets(log.snapshot())
	if !b[0].Running || b[1].Running || s.ForeignTurn() {
		t.Fatalf("brackets %+v, foreign %v; want the start, then its ending, and no turn running", b, s.ForeignTurn())
	}
}

// TestClaudeChildRetiredOnlyAfterTheLastFrame (review C4b item 2): the calls
// fail (Done) while the read loop is still inside the handler of a child's
// announce; the row that announce makes is retired after it, not missed, and
// nothing after the retirement makes a running row again.
func TestClaudeChildRetiredOnlyAfterTheLastFrame(t *testing.T) {
	parked, release := make(chan struct{}), make(chan struct{})
	once := false
	pc := newPipeClaude(t, func(_ *session, next func(acp.SubagentNotification)) func(acp.SubagentNotification) {
		return func(n acp.SubagentNotification) {
			if !once && n.ChildSessionID == "c1" {
				once = true
				close(parked)
				<-release
			}
			next(n)
		}
	})
	go pc.s.retireChildrenAtEnd(pc.client)
	pc.push(t, "root", announce("c1", "running"))
	<-parked
	closed := make(chan struct{})
	go func() { _ = pc.client.Close(); close(closed) }()
	<-pc.client.Conn().Done()
	close(release)
	<-closed
	row := pc.log.waitRow(t, "c1", rowDone)
	if row.Status != SubagentFailed || row.Error != agentStopped {
		t.Fatalf("the child announced as the transport ended: %+v", row)
	}
	pc.s.onSubagent(acp.SubagentNotification{Kind: acp.SubagentUpdate, SubagentID: "c1", ChildSessionID: "c1", State: acp.ChildRunning})
	pc.s.onSubagent(acp.SubagentNotification{Kind: acp.SubagentUpdate, SubagentID: "c2", ChildSessionID: "c2", State: acp.ChildRunning})
	for _, r := range pc.s.Snapshot().Subagents {
		if r.Status == SubagentRunning {
			t.Fatalf("a late update made %+v running after the retirement", r)
		}
	}
}

// TestClaudeChildOneEvictionOrder (review C4b item 7): A registered before B,
// B finished before A, then 62 more: when the registry evicts A for a 65th
// child, the session drops A's row and keeps B's, which the registry still
// routes — B's late text lands on it.
func TestClaudeChildOneEvictionOrder(t *testing.T) {
	pc := newPipeClaude(t, nil)
	pc.push(t, "root", announce("A", "running"))
	pc.push(t, "root", announce("B", "running"))
	pc.push(t, "root", idleUpdate("B"))
	pc.push(t, "root", idleUpdate("A"))
	for i := range 62 {
		id := fmt.Sprintf("c%d", i)
		pc.push(t, "root", announce(id, "running"))
		pc.push(t, "root", idleUpdate(id))
	}
	pc.push(t, "root", announce("D", "running"))
	pc.push(t, "root", idleUpdate("D"))
	pc.barrier(t)
	if _, ok := claudeRowOf(pc.s, "A"); ok {
		t.Fatal("the session kept the row of A, which the registry evicted")
	}
	if _, ok := claudeRowOf(pc.s, "B"); !ok {
		t.Fatal("the session dropped B, which the registry still routes")
	}
	pc.push(t, "B", map[string]any{"sessionUpdate": acp.UpdateAgentMessage, "content": map[string]string{"type": "text", "text": "late"}})
	// The barrier says the client dispatched it; the collector appends what
	// the session published on a goroutine of its own, so the test waits
	// for the event itself (CI run 44171562439 read the log too soon).
	waitFor(t, "B's late text on its row", func() bool {
		for _, ev := range pc.log.snapshot() {
			if ev.Type == EventText && ev.Agent == "B" && ev.Text == "late" {
				return true
			}
		}
		return false
	})
}

// TestClaudeForeignRefusalWaitsInTheQueue (review C4b item 3): claude's
// foreign-turn refusal is the requeueing one.
func TestClaudeForeignRefusalWaitsInTheQueue(t *testing.T) {
	shortForeign(t, 10*time.Second)
	r, log := startClaude(t, "claude-wake-nousage", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "BG go"); err != nil {
		t.Fatal(err)
	}
	log.waitBrackets(t, 1)
	waitFor(t, "the session to say foreign", r.s.ForeignTurn)
	if _, err := r.s.Prompt(t.Context(), "next"); !errors.Is(err, ErrForeignTurnRequeue) || !errors.Is(err, ErrForeignTurn) {
		t.Fatalf("a prompt during claude's own turn = %v; want ErrForeignTurnRequeue", err)
	}
}

// ----------------------------------------------------------------- C4d

// TestClaudeAStalledLoadCompletionFails (review C4c item 1): after a load's
// reply, the session's own waits end with the load: its context ending fails
// the hold, the stalled completion gives up, and Start returns ErrReplayHold
// instead of hanging.
func TestClaudeAStalledLoadCompletionFails(t *testing.T) {
	r := newClaudeRig(t, "claude-replay-load", Options{Interactive: true, LoadSessionID: "s-loaded"})
	collect(t, r.s)
	stalled := make(chan struct{})
	r.s.hooks.loadStall = func(ctx context.Context) {
		close(stalled)
		<-ctx.Done()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- r.s.Start(ctx) }()
	<-stalled
	cancel()
	select {
	case err := <-started:
		if !errors.Is(err, acp.ErrReplayHold) {
			t.Fatalf("Start = %v; want ErrReplayHold", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled load completion held Start after its context ended")
	}
}

// TestClaudeChildRetiredAfterTheDrain (review C4c item 4): with ReplayDone's
// drain parked between a child's running change and its publication, the
// agent's output ends: the retirement waits for the drain, so the running row
// is published before its retirement failure, never after it.
func TestClaudeChildRetiredAfterTheDrain(t *testing.T) {
	p := ClaudeProvider()
	s := newSession(Options{Provider: &p})
	t.Cleanup(func() { _ = s.Close() })
	// The seams, set before any goroutine of the session's reads them.
	parked, release := make(chan struct{}), make(chan struct{})
	s.hooks.subagentPublish = func(n acp.SubagentNotification) {
		if n.ChildSessionID == "c1" && n.State == acp.ChildRunning {
			close(parked)
			<-release
		}
	}
	joining := make(chan struct{})
	s.hooks.retireJoining = func() { close(joining) }
	log := collect(t, s)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	t.Cleanup(func() { _ = cr.Close(); _ = cw.Close(); _ = sr.Close(); _ = sw.Close() })
	client := acp.DialWithDialect(cr, cw, acp.DialectClaude)
	t.Cleanup(func() { _ = client.Close() })
	s.mu.Lock()
	s.client = client
	s.sessionID = "root"
	s.mu.Unlock()
	client.SetUpdateHandler(s.onUpdate)
	client.SetSubagentHandler(s.onSubagent)
	barrier := make(chan struct{}, 1)
	client.SetAuthStatusHandler(func(acp.AuthStatus) { barrier <- struct{}{} })
	enc, dec := acp.NewEncoder(sw), acp.NewDecoder(sr)
	loaded := make(chan error, 1)
	go func() {
		_, err := client.LoadSession(context.Background(), "root", t.TempDir(), acp.SessionParams{})
		loaded <- err
	}()
	req, err := dec.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteMessage(&acp.Message{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := <-loaded; err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if _, err := dec.ReadMessage(); err != nil {
				return
			}
		}
	}()
	raw, _ := json.Marshal(map[string]any{"sessionId": "root", "update": announce("c1", "running")})
	_ = enc.WriteMessage(&acp.Message{JSONRPC: "2.0", Method: acp.MethodSessionUpdate, Params: raw})
	_ = enc.WriteMessage(&acp.Message{JSONRPC: "2.0", Method: acp.MethodClaudeAuthStatus, Params: json.RawMessage(`{"authStatus":{"kind":"account"}}`)})
	<-barrier

	go s.retireChildrenAtEnd(client)
	drained := make(chan error, 1)
	go func() { drained <- client.ReplayDone() }()
	<-parked
	_ = sw.Close() // the agent's output ends
	<-client.Conn().ReadDone()
	<-joining
	retired := func() bool {
		for _, ev := range log.snapshot() {
			if ev.Type == EventSubagent && ev.Subagent != nil && ev.Subagent.ID == "c1" && ev.Subagent.Status == SubagentFailed {
				return true
			}
		}
		return false
	}
	// The one bounded look at non-progress: a retirement that did not wait
	// for the drain publishes its failure here, while the drain is parked.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && !retired() {
		time.Sleep(5 * time.Millisecond)
	}
	if retired() {
		t.Fatal("the retirement published while the drain still held the child's running row")
	}
	close(release)
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the retirement", retired)
	var order []SubagentStatus
	for _, ev := range log.snapshot() {
		if ev.Type == EventSubagent && ev.Subagent != nil && ev.Subagent.ID == "c1" {
			order = append(order, ev.Subagent.Status)
		}
	}
	if len(order) < 2 || order[len(order)-1] != SubagentFailed || order[0] != SubagentRunning {
		t.Fatalf("the child's rows were published %v; want running, then failed last", order)
	}
}

// TestClaudeSessionSaysWhichRowsItRemoves (review C4c item 5, the session's
// half; the transcript's is transcript.TestRetainedRowsFollowTheSession): in the
// A/B sequence every claude row is Retained, and the one row the session drops
// — A, which the registry evicted — is said to be removed; nothing else is.
func TestClaudeSessionSaysWhichRowsItRemoves(t *testing.T) {
	pc := newPipeClaude(t, nil)
	pc.push(t, "root", announce("A", "running"))
	pc.push(t, "root", announce("B", "running"))
	pc.push(t, "root", idleUpdate("B"))
	pc.push(t, "root", idleUpdate("A"))
	for i := range 62 {
		id := fmt.Sprintf("c%d", i)
		pc.push(t, "root", announce(id, "running"))
		pc.push(t, "root", idleUpdate(id))
	}
	pc.push(t, "root", announce("D", "running"))
	pc.push(t, "root", idleUpdate("D"))
	pc.barrier(t)
	var removed []string
	for _, ev := range pc.log.snapshot() {
		if ev.Type != EventSubagent || ev.Subagent == nil {
			continue
		}
		if !ev.Subagent.Retained {
			t.Fatalf("a claude row not retained: %+v", ev.Subagent)
		}
		if ev.SubagentChange == SubagentChangeRemoved {
			removed = append(removed, ev.Subagent.ID)
		}
	}
	if len(removed) != 1 || removed[0] != "A" {
		t.Fatalf("removed %v; want A alone", removed)
	}
}

// TestClaudeChildRetentionFollowsRouting (review C4c items 6, 7): a child the
// registry routes again after it was gone is the registry's to keep once more,
// and children the registry's cap refused — never routed — are capped like
// any gone row.
func TestClaudeChildRetentionFollowsRouting(t *testing.T) {
	s, _ := claudeUnit(t)
	upd := func(id, state string, unrouted, evicted bool) acp.SubagentNotification {
		n := claudeChildUpdate(id, state, map[string]any{"stop": acp.StopEndTurn})
		n.Unrouted, n.Evicted = unrouted, evicted
		return n
	}
	s.onSubagent(upd("G", acp.ChildRunning, false, false))
	s.onSubagent(upd("G", acp.ChildUnknown, false, true))
	s.onSubagent(upd("G", acp.ChildRunning, false, false))
	s.onSubagent(upd("G", acp.ChildIdle, false, false))
	for i := range 40 {
		id := fmt.Sprintf("refused%d", i)
		s.onSubagent(upd(id, acp.ChildRunning, true, false))
		s.onSubagent(upd(id, acp.ChildIdle, true, false))
	}
	if _, ok := claudeRowOf(s, "G"); !ok {
		t.Fatal("G, routed again, was evicted as a gone row")
	}
	refused := 0
	for _, r := range s.Snapshot().Subagents {
		if strings.HasPrefix(r.ID, "refused") {
			refused++
		}
	}
	if refused > subagentFinishedCap {
		t.Fatalf("%d refused rows kept; want at most %d", refused, subagentFinishedCap)
	}
}
