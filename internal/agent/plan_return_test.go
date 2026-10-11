package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// The return policy (ApprovalReturn, plan 041 X28) against an agent the test
// plays over the wire (inProcess): the plan's turn is ended by a
// session/cancel that can reach no other turn, its record of a returned plan
// is written only when this handler's answer is the request's and the turn is
// live, and it cancels the plan turn's asks alone (C3 review r2, 2 and 3).

const returnPlanParams = `{"sessionId":"s1","name":"The plan","overview":"o","plan":"## Steps"}`

// returnRig is a cursor-dialect session the test is the agent of, its policy
// an unattended host's on an ACP provider: permissions parked, plans
// returned. Its handlers are the ones Start installs.
func returnRig(t *testing.T) (*inProcess, *eventLog) {
	t.Helper()
	p := newInProcess(t, acp.DialectCursor, nil)
	p.s.opts.Approval = &ApprovalPolicy{Permission: ApprovalAsk, Question: ApprovalFirstOption, Plan: ApprovalReturn}
	p.client.SetPermissionHandler(p.s.onPermission)
	p.client.SetPlanHandler(p.s.onCreatePlan)
	p.client.SetEarlyAnswerHandler(p.s.onEarlyAnswer)
	return p, collect(t, p.s)
}

// holdReturnAt parks returnPlan at stage (returnPlanSeam) once, and answers
// what is closed when it is parked and what lets it go; t.Cleanup lets it go.
func holdReturnAt(t *testing.T, stage string) (reached <-chan struct{}, release func()) {
	t.Helper()
	at, gate := make(chan struct{}), make(chan struct{})
	once, relOnce := false, false
	prev := returnPlanSeam
	returnPlanSeam = func(s string) {
		if s != stage || once {
			return
		}
		once = true
		close(at)
		<-gate
	}
	release = func() {
		if !relOnce {
			relOnce = true
			close(gate)
		}
	}
	t.Cleanup(func() { release(); returnPlanSeam = prev })
	return at, release
}

// request is a blocking request the agent sends craze.
func (p *inProcess) request(t *testing.T, id, method, params string) {
	t.Helper()
	if err := p.enc.WriteMessage(&acp.Message{ID: json.RawMessage(id), Method: method, Params: json.RawMessage(params)}); err != nil {
		t.Fatal(err)
	}
}

// drain is every frame craze writes until none comes within d.
func (p *inProcess) drain(t *testing.T, d time.Duration) []*acp.Message {
	t.Helper()
	var out []*acp.Message
	for {
		m := p.next(t, d)
		if m == nil {
			return out
		}
		out = append(out, m)
	}
}

// until is every frame craze writes up to and including the first that done
// accepts, within 5 s.
func (p *inProcess) until(t *testing.T, what string, done func(*acp.Message) bool) []*acp.Message {
	t.Helper()
	var out []*acp.Message
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m := p.next(t, time.Until(deadline))
		if m == nil {
			break
		}
		out = append(out, m)
		if done(m) {
			return out
		}
	}
	t.Fatalf("craze never wrote %s: %d frames", what, len(out))
	return nil
}

func isMethod(method string) func(*acp.Message) bool {
	return func(m *acp.Message) bool { return m.Method == method }
}

// returnedPlans counts the records of a returned plan the session published:
// automatic plan openings that did not accept.
func returnedPlans(log *eventLog) int {
	n := 0
	for _, ev := range log.snapshot() {
		if ev.Type == EventPlan && ev.Plan != nil && ev.Plan.Auto && !ev.Plan.Accepted {
			n++
		}
	}
	return n
}

// TestAReturnedPlansCancelNeverReachesTheNextTurn is the review's interleaving
// (r2, 2): turn A presents a plan and the return policy has decided — the plan
// recorded, its cancel not yet written — when a person's cancel comes; the
// agent ends A on that cancel; turn B is admitted as soon as A's Prompt
// returns, and asks for a permission. The return's session/cancel reaches the
// wire before B's prompt, never after it — A's claim is held until it is
// written — and B's ask is left open.
func TestAReturnedPlansCancelNeverReachesTheNextTurn(t *testing.T) {
	p, log := returnRig(t)
	decided, release := holdReturnAt(t, "decided")
	outA := promptOn(p.s, "A")
	reqA := p.expect(t, acp.MethodSessionPrompt)
	p.request(t, `"plan-1"`, acp.MethodCursorCreatePlan, returnPlanParams)
	select {
	case <-decided:
	case <-time.After(5 * time.Second):
		t.Fatal("the return policy never decided")
	}
	// A person's craze cancel, by the session's own Cancel.
	cancelled := cancelBounded(t, p.s)
	frames := p.until(t, "the person's session/cancel", isMethod(acp.MethodSessionCancel))
	// The agent ends A on it.
	p.reply(t, reqA.ID, map[string]string{"stopReason": "cancelled"})
	outB := make(chan error, 1)
	go func() {
		_ = promptReturnWait(outA)
		outB <- <-promptOn(p.s, "B")
	}()
	frames = append(frames, p.drain(t, 300*time.Millisecond)...)
	askB := false
	if hasMethod(frames, acp.MethodSessionPrompt) {
		p.request(t, `"perm-1"`, acp.MethodRequestPermission, permParams)
		askB = true
	}
	release()
	frames = append(frames, p.until(t, "B's session/prompt", func(m *acp.Message) bool {
		return m.Method == acp.MethodSessionPrompt || hasMethod(frames, acp.MethodSessionPrompt)
	})...)
	if !askB {
		p.request(t, `"perm-1"`, acp.MethodRequestPermission, permParams)
	}
	frames = append(frames, p.drain(t, 300*time.Millisecond)...)
	b := indexMethod(frames, acp.MethodSessionPrompt)
	for _, m := range frames[b+1:] {
		if m.Method == acp.MethodSessionCancel {
			t.Fatal("the returned plan's session/cancel reached the wire after turn B's prompt")
		}
		if string(m.ID) == `"perm-1"` {
			t.Fatalf("turn B's permission was answered: %s", m.Result)
		}
	}
	waitFor(t, "B's permission open", func() bool { return len(p.s.asks.Asks()) == 1 })
	if asks := p.s.asks.Asks(); len(asks) != 1 || asks[0].Kind != AskPermission {
		t.Fatalf("turn B's open asks: %+v", asks)
	}
	if err := cancelReturn(t, cancelled, 10*time.Second); err != nil {
		t.Fatalf("the person's cancel: %v", err)
	}
	waitFor(t, "A's returned plan", func() bool { return returnedPlans(log) >= 1 })
	if n := returnedPlans(log); n != 1 {
		t.Fatalf("%d returned plans recorded, want A's one", n)
	}
	select {
	case err := <-outB:
		t.Fatalf("turn B ended: %v", err)
	default:
	}
}

// TestAStalledReturnCancelHoldsTheNextTurn is the review's surviving write
// (r3, 1): the return's session/cancel stalls before it lands — held on its
// own goroutine, past the handler's bound, which is long gone (shrunk to a
// millisecond) — the handler answers the plan request, a person's cancel ends
// A, and turn B is queued behind A. A's claim stays held until the return's
// cancel has been written, so B's prompt never reaches the wire ahead of it.
func TestAStalledReturnCancelHoldsTheNextTurn(t *testing.T) {
	prev := returnPlanCancelWait
	returnPlanCancelWait = time.Millisecond
	t.Cleanup(func() { returnPlanCancelWait = prev })
	p, _ := returnRig(t)
	writing, release := holdReturnAt(t, "writing")
	outA := promptOn(p.s, "A")
	reqA := p.expect(t, acp.MethodSessionPrompt)
	p.request(t, `"plan-1"`, acp.MethodCursorCreatePlan, returnPlanParams)
	select {
	case <-writing:
	case <-time.After(5 * time.Second):
		t.Fatal("the return policy never began its cancel's write")
	}
	// The handler's wait has ended on its bound: it answers the request
	// while the cancel is still unwritten.
	frames := p.until(t, "the plan's answer", func(m *acp.Message) bool { return m.Method == "" && string(m.ID) == `"plan-1"` })
	cancelled := cancelBounded(t, p.s)
	frames = append(frames, p.until(t, "the person's session/cancel", isMethod(acp.MethodSessionCancel))...)
	p.reply(t, reqA.ID, map[string]string{"stopReason": "cancelled"})
	outB := make(chan error, 1)
	go func() {
		_ = promptReturnWait(outA)
		outB <- <-promptOn(p.s, "B")
	}()
	// Either A's release is parked on the unwritten cancel, or the claim was
	// given back and B's prompt is on the wire.
	parked := func() bool {
		p.s.mu.Lock()
		defer p.s.mu.Unlock()
		return p.s.releasing && p.s.planHold != nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for !parked() && !hasMethod(frames, acp.MethodSessionPrompt) {
		if time.Now().After(deadline) {
			t.Fatal("A's release neither parked on the return's cancel nor admitted B")
		}
		if m := p.next(t, 5*time.Millisecond); m != nil {
			frames = append(frames, m)
		}
	}
	release()
	frames = append(frames, p.until(t, "B's session/prompt", func(m *acp.Message) bool {
		return m.Method == acp.MethodSessionPrompt || hasMethod(frames, acp.MethodSessionPrompt)
	})...)
	b, cancels := indexMethod(frames, acp.MethodSessionPrompt), 0
	for _, m := range frames[:b] {
		if m.Method == acp.MethodSessionCancel {
			cancels++
		}
	}
	if cancels != 2 {
		t.Fatalf("turn B's prompt reached the wire after %d session/cancel frames, want the person's and the return's", cancels)
	}
	if err := cancelReturn(t, cancelled, 10*time.Second); err != nil {
		t.Fatalf("the person's cancel: %v", err)
	}
	select {
	case err := <-outB:
		t.Fatalf("turn B ended: %v", err)
	default:
	}
}

// TestAPlanAnsweredByACancelFirstIsNoReturnedPlan (r2, 3): a person's cancel
// answers the plan request before the return policy's handler takes its
// answer. The handler's answer is not the request's: it records the plan as
// a cancel's answer records one, never as returned, and writes no
// session/cancel of its own — the turn ends as the person's cancel.
func TestAPlanAnsweredByACancelFirstIsNoReturnedPlan(t *testing.T) {
	p, log := returnRig(t)
	started, release := holdReturnAt(t, "handler")
	outA := promptOn(p.s, "A")
	reqA := p.expect(t, acp.MethodSessionPrompt)
	p.request(t, `"plan-1"`, acp.MethodCursorCreatePlan, returnPlanParams)
	<-started
	cancelled := cancelBounded(t, p.s)
	frames := p.until(t, "the person's session/cancel", isMethod(acp.MethodSessionCancel))
	if !answered(frames, `"plan-1"`) {
		t.Fatalf("the person's cancel did not answer the plan request: %d frames", len(frames))
	}
	release()
	waitFor(t, "the plan's record", func() bool { return len(p.s.asks.Resolved()) >= 1 })
	if after := p.drain(t, 300*time.Millisecond); hasMethod(after, acp.MethodSessionCancel) {
		t.Fatal("the return policy wrote a session/cancel of its own")
	}
	p.reply(t, reqA.ID, map[string]string{"stopReason": "cancelled"})
	_ = promptReturn(t, outA, "A never returned")
	if err := cancelReturn(t, cancelled, 10*time.Second); err != nil {
		t.Fatalf("the person's cancel: %v", err)
	}
	if n := returnedPlans(log); n != 0 {
		t.Fatalf("%d returned plans recorded for a turn a person cancelled", n)
	}
	for _, rec := range p.s.asks.Resolved() {
		if rec.Kind != AskPlan || rec.Outcome != AskCancelled {
			t.Fatalf("the plan's records: %+v", p.s.asks.Resolved())
		}
	}
}

// TestAStalePlanRequestIsNoReturnedPlan (r2, 3): the plan's handler runs only
// after its turn A has ended and turn B has started. Its request is stale:
// recorded as any stale request, never as a returned plan in B's range, and B
// is not cancelled.
func TestAStalePlanRequestIsNoReturnedPlan(t *testing.T) {
	p, log := returnRig(t)
	started, release := holdReturnAt(t, "handler")
	outA := promptOn(p.s, "A")
	reqA := p.expect(t, acp.MethodSessionPrompt)
	p.request(t, `"plan-1"`, acp.MethodCursorCreatePlan, returnPlanParams)
	<-started
	p.reply(t, reqA.ID, map[string]string{"stopReason": "end_turn"})
	_ = promptReturn(t, outA, "A never returned")
	outB := promptOn(p.s, "B")
	p.expect(t, acp.MethodSessionPrompt)
	release()
	frames := p.until(t, "the stale plan's answer", func(m *acp.Message) bool { return string(m.ID) == `"plan-1"` })
	frames = append(frames, p.drain(t, 300*time.Millisecond)...)
	if hasMethod(frames, acp.MethodSessionCancel) {
		t.Fatal("a stale plan request cancelled turn B")
	}
	if n := returnedPlans(log); n != 0 {
		t.Fatalf("%d returned plans recorded from a stale request", n)
	}
	select {
	case err := <-outB:
		t.Fatalf("turn B ended: %v", err)
	default:
	}
}

const permParams = `{"sessionId":"s1","toolCall":{"toolCallId":"t1","title":"Shell"},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"}]}`

func hasMethod(frames []*acp.Message, method string) bool { return indexMethod(frames, method) >= 0 }

func indexMethod(frames []*acp.Message, method string) int {
	for i, m := range frames {
		if m.Method == method {
			return i
		}
	}
	return -1
}

// answered says frames hold craze's reply to the agent's request id.
func answered(frames []*acp.Message, id string) bool {
	for _, m := range frames {
		if m.Method == "" && string(m.ID) == id {
			return true
		}
	}
	return false
}

// promptReturnWait is a prompt's return, however long it takes.
func promptReturnWait(out <-chan error) error { return <-out }

// TestCancelOwnTurnCancelsThatTurnsAsksAlone (r2, 2): the return policy's
// ask cancel is its turn's: an ask open on that turn's token is answered
// cancelled, and one open on another turn's — or between turns — is left as
// it is (CancelTurn, a person's cancel, answers every one).
func TestCancelOwnTurnCancelsThatTurnsAsksAlone(t *testing.T) {
	r, _ := newAskRegistry(t)
	a, b := r.BeginTurn(), r.BeginTurn()
	mine := mustOpen(t, r, context.Background(), a, permReq())
	theirs := mustOpen(t, r, context.Background(), b, permReq())
	between := mustOpen(t, r, context.Background(), TurnToken{}, permReq())
	r.CancelOwnTurn(a)
	open := map[string]bool{}
	for _, rec := range r.Asks() {
		open[rec.ID] = true
	}
	if open[mine.ID()] || !open[theirs.ID()] || !open[between.ID()] {
		t.Fatalf("open after the plan turn's cancel: %v (mine %s, theirs %s, between %s)", open, mine.ID(), theirs.ID(), between.ID())
	}
}
