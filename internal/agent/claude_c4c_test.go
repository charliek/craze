package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// barrier waits until everything pushed so far has reached the session: a
// mode update it can see, behind them on the wire.
func (p *pipeClaude) barrier(t *testing.T) {
	t.Helper()
	p.sync++
	mode := fmt.Sprintf("sync-%d", p.sync)
	p.push(t, "root", map[string]any{"sessionUpdate": acp.UpdateCurrentMode, "currentModeId": mode})
	waitFor(t, "the barrier "+mode, func() bool { return p.s.Snapshot().CurrentMode == mode })
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
	pc.barrier(t)
	found := false
	for _, ev := range pc.log.snapshot() {
		found = found || (ev.Type == EventText && ev.Agent == "B" && ev.Text == "late")
	}
	if !found {
		t.Fatal("B's late text did not land on its row")
	}
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
