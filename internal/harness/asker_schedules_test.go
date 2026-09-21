package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/tool/opencode"
)

// An approved plan against the rest of its step, schedule by schedule (plan
// 023 §3.4): a call already running, more Parallel calls than Fantasy has
// slots, a second plan call, and the doom-loop guard.

// heldTool is a Parallel tool that blocks until the test lets it go: a call
// that is already running when the person approves the plan.
type heldTool struct {
	started chan string   // receives each call's title as it starts running
	release chan struct{} // closed to let every call return
	once    sync.Once
}

// letGo releases every held call, once.
func (h *heldTool) letGo() { h.once.Do(func() { close(h.release) }) }

func (h *heldTool) Spec() tool.Spec {
	return tool.Spec{
		ID: "held", Description: "Blocks until a test releases it.",
		Parameters: map[string]any{"id": map[string]any{"type": "string"}},
		Required:   []string{"id"},
		Kind:       tool.KindRead, ReadOnly: true, Parallel: true, Truncate: tool.None,
	}
}

func (h *heldTool) Prepare(_ tool.Env, c tool.Call) (tool.Prepared, error) {
	var in struct{ ID string }
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return nil, err
	}
	return heldCall{h, in.ID}, nil
}

type heldCall struct {
	h  *heldTool
	id string
}

func (c heldCall) Request() tool.Request { return tool.Request{Title: c.id} }

func (c heldCall) Run(context.Context, tool.Env) tool.Result {
	c.h.started <- c.id
	<-c.h.release
	return tool.Result{Text: "held " + c.id + " ran"}
}

// heldSession is planSession with a heldTool in the profile.
func heldSession(t *testing.T, asker tool.Asker) (*heldTool, *Session, *scripted) {
	t.Helper()
	h := &heldTool{started: make(chan string, 16), release: make(chan struct{})}
	f := newFixture(t, "http://127.0.0.1:1/v1")
	opts := modeOptions(f, "plan")
	opts.Asker = asker
	opts.tools.profiles = func() (*tool.Registry, error) {
		p, err := opencode.Profile()
		if err != nil {
			return nil, err
		}
		p.Tools = append(p.Tools, h)
		var reg tool.Registry
		return &reg, reg.Register(p)
	}
	s := f.open(opts)
	// Registered after the session's own cleanup, so it runs before it: Close
	// waits for the turn, and the turn for every held call.
	t.Cleanup(h.letGo)
	if err := os.WriteFile(planPathOf(s), []byte("# The plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return h, s, f.models["test/a"]
}

// finishes counts the ToolFinished events per call id.
func finishes(evs []Event) map[string]int {
	out := map[string]int{}
	for _, ev := range evs {
		if fin, ok := ev.(ToolFinished); ok {
			out[fin.ID]++
		}
	}
	return out
}

// A call already running when the plan is approved is not refused: the turn
// joins it, its own result stands, and only then does the turn end. With more
// Parallel calls than Fantasy has slots (five), the sixth holds the dispatch
// of everything after it, exit_plan_mode included, and nothing is lost: every
// call is answered once, and the dispatcher holds nothing at the end.
func TestAnApprovalJoinsTheCallsAlreadyRunning(t *testing.T) {
	for _, n := range []int{1, 6} {
		t.Run(fmt.Sprintf("%d held", n), func(t *testing.T) {
			a := newParkedAsker()
			h, s, m := heldSession(t, a)
			var calls [][]fantasy.StreamPart
			for i := range n {
				id := fmt.Sprintf("h%d", i+1)
				calls = append(calls, callParts(id, "held", fmt.Sprintf("{\"id\":%q}", id)))
			}
			calls = append(calls, callParts("x", "exit_plan_mode", "{}"))
			m.push(callStep(calls...))
			var evs events
			out := start(context.Background(), s, "plan it", evs.sink)

			for range min(n, 5) { // Fantasy's parallel slots
				await(t, h.started, "a held call to start")
			}
			if n <= 5 {
				// Every held call has a slot, so exit_plan_mode is dispatched
				// while they run: the person approves with them still running.
				await(t, a.asked, "the plan to be presented")
				a.plan <- tool.PlanApproved
				select {
				case got := <-out:
					t.Fatalf("Run returned %+v, %v with a call still running", got.res, got.err)
				case <-time.After(100 * time.Millisecond):
				}
				h.letGo()
			} else {
				// The sixth waits for a slot and holds the plan behind it.
				h.letGo()
				await(t, a.asked, "the plan to be presented")
				a.plan <- tool.PlanApproved
			}
			if got := await(t, out, "the turn"); got.err != nil || got.res.StopReason != StopEndTurn {
				t.Fatalf("Run = %+v, %v; want end_turn", got.res, got.err)
			}
			results := lastLines(t, s, 1)[0]
			for i := range n {
				if want := fmt.Sprintf("[result h%d: held h%d ran]", i+1, i+1); !strings.Contains(results, want) {
					t.Fatalf("the step's results lack %q:\n%s", want, results)
				}
			}
			if !strings.Contains(results, "[result x: "+opencode.PlanApprovedText+"]") {
				t.Fatalf("the step's results lack the approval:\n%s", results)
			}
			done := finishes(evs.list())
			if len(done) != n+1 {
				t.Fatalf("%d calls finished, want %d: %v", len(done), n+1, done)
			}
			for id, times := range done {
				if times != 1 {
					t.Fatalf("call %s finished %d times", id, times)
				}
			}
			if p := s.tools.d.Pending(); p != 0 {
				t.Fatalf("%d prepared calls leaked", p)
			}
		})
	}
}

// The rule itself, with no scheduler in it: a call the model placed before the
// asking one runs even if it had not started when the approval landed — it may
// still be waiting for its goroutine, or for one of Fantasy's five slots — and
// a call placed after it is refused. The turn's state is set up by hand so
// that the one schedule a real turn cannot be made to produce on demand, a
// dispatched call that has not reached runTool, is simply stated.
func TestTheVetoIsDecidedByPlace(t *testing.T) {
	waiting := &toolCall{order: 1}                // placed first, not yet running
	running := &toolCall{order: 2, running: true} // a Parallel call still running
	asking := &toolCall{order: 3, running: true}  // exit_plan_mode, blocked on the person
	after := &toolCall{order: 4}                  // placed after it
	tn := &turn{calls: calls{list: []*toolCall{waiting, running, asking, after}}}

	for _, c := range tn.list {
		if tn.refusedByApproval(c) {
			t.Fatalf("call %d is refused before any approval", c.order)
		}
	}
	tn.planWasApproved()
	if tn.approvedAt != asking.order {
		t.Fatalf("the asking call's place = %d, want %d: the last of the calls running", tn.approvedAt, asking.order)
	}
	for _, c := range []*toolCall{waiting, running, asking} {
		if tn.refusedByApproval(c) {
			t.Errorf("call %d, placed at or before the asking call, is refused", c.order)
		}
	}
	if !tn.refusedByApproval(after) {
		t.Error("the call placed after the asking one is not refused")
	}
	// A second approval in the step cannot move the place.
	asking.running = false
	tn.planWasApproved()
	if tn.approvedAt != asking.order {
		t.Fatalf("a second approval moved the place to %d", tn.approvedAt)
	}
	// With no call running — not reachable, since the asking call is always
	// one — everything is refused: the safe default.
	none := &turn{calls: calls{list: []*toolCall{{order: 1}}}}
	none.planWasApproved()
	if !none.refusedByApproval(none.list[0]) {
		t.Error("an approval that found no asking call refused nothing")
	}
}

// Two plan calls in one step: the person is asked once, and the second call
// is refused like any other that follows an approval.
func TestASecondPlanCallAfterAnApproval(t *testing.T) {
	a := newParkedAsker()
	_, s, m := planSession(t, a)
	m.push(callStep(callParts("c1", "exit_plan_mode", "{}"), callParts("c2", "exit_plan_mode", "{}")))
	out := start(context.Background(), s, "plan it", nil)
	await(t, a.asked, "the plan to be presented")
	a.plan <- tool.PlanApproved
	if got := await(t, out, "the turn"); got.err != nil || got.res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v; want end_turn", got.res, got.err)
	}
	if plans, _ := a.count(); plans != 1 {
		t.Fatalf("the plan was presented %d times", plans)
	}
	if got := lastLines(t, s, 1)[0]; !strings.Contains(got, "[error c2: "+planApprovedVeto+"]") {
		t.Fatalf("the step's results:\n%s", got)
	}
}

// The doom-loop guard outranks an approval: a step that tripped the guard and
// also had its plan approved is a turn that was stopped, not one that
// finished (plan 023 §3.4).
func TestTheGuardOutranksAnApproval(t *testing.T) {
	a := newParkedAsker()
	_, s, m := planSession(t, a)
	const same = "{\"todos\":[{\"id\":\"a\",\"status\":\"pending\"}]}"
	var calls [][]fantasy.StreamPart
	for i := range doomStopAt {
		calls = append(calls, callParts(fmt.Sprintf("t%d", i+1), "todo_write", same))
	}
	calls = append(calls, callParts("x", "exit_plan_mode", "{}"))
	m.push(callStep(calls...))
	out := start(context.Background(), s, "plan it", nil)
	await(t, a.asked, "the plan to be presented")
	a.plan <- tool.PlanApproved
	if got := await(t, out, "the turn"); got.err != nil || got.res.StopReason != StopMaxTurnRequests {
		t.Fatalf("Run = %+v, %v; want max_turn_requests", got.res, got.err)
	}
}
