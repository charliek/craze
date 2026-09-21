package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/tool/opencode"
)

// The asker seam from the turn's side (plan 023 §3.4, §3.5, §7 A3, A4): a
// tool that blocks on a person, what an approved plan does to its turn and to
// the rest of its step, and how a cancel and a close get a blocked tool back.
// The tools' own behaviour is tested beside them (tool/opencode).

// parkedAsker parks every ask until the test decides it. asked is the
// barrier: it receives once the ask is open, which is the one moment a test
// may cancel, close or answer without guessing at a schedule. With deaf set
// it ignores its context until the test lets go, as a tool that will not
// return does.
type parkedAsker struct {
	asked  chan string           // the plan's text, or the first question's
	plan   chan tool.PlanOutcome // the person's decision on a plan
	answer chan tool.Answers     // the person's answer to a question

	deaf    bool
	release chan struct{} // closed to let a deaf ask go

	mu    sync.Mutex
	plans int
	asks  int
}

func newParkedAsker() *parkedAsker {
	return &parkedAsker{
		asked:   make(chan string, 8),
		plan:    make(chan tool.PlanOutcome, 8),
		answer:  make(chan tool.Answers, 8),
		release: make(chan struct{}),
	}
}

func (a *parkedAsker) count() (plans, asks int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.plans, a.asks
}

func (a *parkedAsker) AskQuestion(ctx context.Context, qs []tool.Question) (tool.Answers, error) {
	a.mu.Lock()
	a.asks++
	a.mu.Unlock()
	a.asked <- qs[0].Question
	select {
	case ans := <-a.answer:
		return ans, nil
	case <-ctx.Done():
		return tool.Answers{}, nil
	}
}

func (a *parkedAsker) PresentPlan(ctx context.Context, p tool.PlanOffer) (tool.PlanOutcome, error) {
	a.mu.Lock()
	a.plans++
	a.mu.Unlock()
	a.asked <- p.Text
	if a.deaf {
		<-a.release
		return tool.PlanUnanswered, nil
	}
	select {
	case out := <-a.plan:
		return out, nil
	case <-ctx.Done():
		return tool.PlanUnanswered, nil
	}
}

// planSession is a session in plan mode with a written plan and asker wired
// in.
func planSession(t *testing.T, asker tool.Asker) (*fixture, *Session, *scripted) {
	t.Helper()
	f := newFixture(t, "http://127.0.0.1:1/v1")
	opts := modeOptions(f, "plan")
	opts.Asker = asker
	s := f.open(opts)
	if err := os.WriteFile(planPathOf(s), []byte("# The plan\n\n1. do it\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, s, f.models["test/a"]
}

// lastLines are the transcript's last n entries.
func lastLines(t *testing.T, s *Session, n int) []string {
	t.Helper()
	lines := entries(transcript(t, s))
	if len(lines) < n {
		t.Fatalf("the transcript has %d entries, want at least %d:\n%s", len(lines), n, strings.Join(lines, "\n"))
	}
	return lines[len(lines)-n:]
}

// An approved plan ends the turn, as end_turn, with the step that asked
// persisted — its own stop reason still tool_use — and no request after it;
// the mode is still plan, since leaving it is the person's (D-51).
func TestAnApprovedPlanEndsTheTurn(t *testing.T) {
	a := newParkedAsker()
	_, s, m := planSession(t, a)
	m.push(callStep(callParts("c1", "exit_plan_mode", "{}")), answerWith("never requested"))

	out := start(context.Background(), s, "plan it", nil)
	if text := await(t, a.asked, "the plan to be presented"); text != "# The plan\n\n1. do it\n" {
		t.Fatalf("the person was shown %q", text)
	}
	a.plan <- tool.PlanApproved
	got := await(t, out, "the turn")
	if got.err != nil || got.res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v; want end_turn", got.res, got.err)
	}
	if n := len(m.requests()); n != 1 {
		t.Fatalf("the model saw %d requests; an approved plan must not be followed by one", n)
	}
	equal(t, "transcript", lastLines(t, s, 2), []string{
		"assistant test/a high tool_use: [call c1 exit_plan_mode {}]",
		"tool test/a high: [result c1: " + opencode.PlanApprovedText + "]",
	})
	if s.Mode() != modePlan {
		t.Fatalf("the session is in %q mode; approving a plan does not change it", s.Mode())
	}

	// The next turn is an ordinary one: the approval was that turn's alone.
	m.push(answerWith("ok"))
	if res := run(t, s, "and now?"); res.StopReason != StopEndTurn || len(m.requests()) != 2 {
		t.Fatalf("the next turn = %+v after %d requests", res, len(m.requests()))
	}
}

// Every call of the step that starts after the approval is refused, so the
// model cannot change a plan the person has just approved, nor ask them
// something the turn will never hear the answer to; the results stay paired.
func TestAnApprovedPlanRefusesTheRestOfItsStep(t *testing.T) {
	a := newParkedAsker()
	_, s, m := planSession(t, a)
	plan := planPathOf(s)
	m.push(callStep(
		callParts("c1", "exit_plan_mode", "{}"),
		callParts("c2", "write", input(t, map[string]any{"filePath": plan, "content": "# A different plan\n"})),
		callParts("c3", "ask_user_question", input(t, map[string]any{"questions": []any{
			map[string]any{"question": "Shall I?", "options": []any{map[string]any{"label": "yes"}}}}})),
	))
	var evs events
	out := start(context.Background(), s, "plan it", evs.sink)
	await(t, a.asked, "the plan to be presented")
	a.plan <- tool.PlanApproved
	if got := await(t, out, "the turn"); got.err != nil || got.res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v; want end_turn", got.res, got.err)
	}

	if b, err := os.ReadFile(plan); err != nil || string(b) != "# The plan\n\n1. do it\n" {
		t.Fatalf("the approved plan is now %q (%v)", b, err)
	}
	if plans, asks := a.count(); plans != 1 || asks != 0 {
		t.Fatalf("the person was shown %d plans and %d questions, want 1 and 0", plans, asks)
	}
	tool3 := lastLines(t, s, 1)[0]
	for _, want := range []string{
		"[result c1: " + opencode.PlanApprovedText + "]",
		"[error c2: " + planApprovedVeto + "]",
		"[error c3: " + planApprovedVeto + "]",
	} {
		if !strings.Contains(tool3, want) {
			t.Fatalf("the step's results lack %q:\n%s", want, tool3)
		}
	}
	// Each refused call is finished for its card, once, as not executed.
	refused := 0
	for _, ev := range evs.list() {
		if fin, ok := ev.(ToolFinished); ok && fin.Result.Text == planApprovedVeto {
			refused++
			if !fin.Result.IsError || fin.Result.Class != tool.ClassNotExecuted {
				t.Fatalf("a refused call finished as %+v", fin.Result)
			}
		}
	}
	if refused != 2 {
		t.Fatalf("%d calls finished refused, want 2", refused)
	}
	if n := s.tools.d.Pending(); n != 0 {
		t.Fatalf("%d prepared calls leaked", n)
	}
}

// A rejected plan keeps the turn going, in plan mode, with grok-build's
// no-feedback text: the registry's plan answer carries none (D-51).
func TestARejectedPlanContinuesTheTurn(t *testing.T) {
	a := newParkedAsker()
	_, s, m := planSession(t, a)
	m.push(callStep(callParts("c1", "exit_plan_mode", "{}")), answerWith("What should change?"))

	out := start(context.Background(), s, "plan it", nil)
	await(t, a.asked, "the plan to be presented")
	a.plan <- tool.PlanRejected
	got := await(t, out, "the turn")
	if got.err != nil || got.res.StopReason != StopEndTurn || len(m.requests()) != 2 {
		t.Fatalf("Run = %+v, %v after %d requests; want the model's own end_turn after 2", got.res, got.err, len(m.requests()))
	}
	equal(t, "transcript", lastLines(t, s, 2), []string{
		"tool test/a high: [result c1: " + opencode.PlanRejectedText + "]",
		"assistant test/a high end_turn: What should change?",
	})
}

// A cancel reaches a tool blocked on the person through its context, the turn
// joins it, and no request follows. A cancel also outranks an approval that
// raced it: the person pressed Esc.
func TestACancelJoinsABlockedAsk(t *testing.T) {
	for _, tc := range []struct {
		name, toolName, args string
	}{
		{"plan", "exit_plan_mode", "{}"},
		{"question", "ask_user_question", `{"questions":[{"question":"Shall I?","options":[{"label":"yes"}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newParkedAsker()
			_, s, m := planSession(t, a)
			m.push(callStep(callParts("c1", tc.toolName, tc.args)), answerWith("never requested"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := start(ctx, s, "go", nil)
			await(t, a.asked, "the ask to open")
			cancel()
			got := await(t, out, "the cancelled turn")
			if got.err != nil || !only(got.res, StopCancelled) || len(m.requests()) != 1 {
				t.Fatalf("Run = %+v, %v after %d requests; want cancelled after 1", got.res, got.err, len(m.requests()))
			}
			// The step is persisted with the call answered, as aborted.
			last := lastLines(t, s, 1)[0]
			if !strings.HasPrefix(last, "tool test/a high: [error c1: ") {
				t.Fatalf("the blocked call's result is %q", last)
			}
		})
	}
	t.Run("an approval that raced the cancel", func(t *testing.T) {
		a := newParkedAsker()
		_, s, m := planSession(t, a)
		m.push(callStep(callParts("c1", "exit_plan_mode", "{}")))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := start(ctx, s, "go", nil)
		await(t, a.asked, "the plan to be presented")
		// Both are ready before the asker looks: whichever its select takes,
		// the turn was cancelled, and that is what it reports.
		a.plan <- tool.PlanApproved
		cancel()
		if got := await(t, out, "the turn"); got.err != nil || got.res.StopReason != StopCancelled {
			t.Fatalf("Run = %+v, %v; a cancel outranks an approval", got.res, got.err)
		}
	})
}

// A tool that will not return holds the turn — Fantasy joins every tool
// goroutine, and nothing times an ask out (D-52) — and the turn ends the
// moment it does. Nothing here depends on how long that is.
func TestATurnWaitsForAToolThatWillNotReturn(t *testing.T) {
	a := newParkedAsker()
	a.deaf = true
	_, s, m := planSession(t, a)
	m.push(callStep(callParts("c1", "exit_plan_mode", "{}")), answerWith("never requested"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := start(ctx, s, "go", nil)
	await(t, a.asked, "the plan to be presented")
	cancel()
	select {
	case got := <-out:
		t.Fatalf("Run returned %+v, %v while its tool was still running", got.res, got.err)
	default:
	}
	close(a.release)
	if got := await(t, out, "the turn, once its tool let go"); got.err != nil || got.res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
	}
	if n := len(m.requests()); n != 1 {
		t.Fatalf("the model saw %d requests", n)
	}
}

// Close during an ask: the turn's context is cancelled with the closing
// cause, the blocked tool returns, and no request leaves for a closing
// session (plan 023 §3.5).
func TestCloseJoinsABlockedAsk(t *testing.T) {
	a := newParkedAsker()
	_, s, m := planSession(t, a)
	m.push(callStep(callParts("c1", "exit_plan_mode", "{}")), answerWith("never requested"))
	out := start(context.Background(), s, "go", nil)
	await(t, a.asked, "the plan to be presented")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	got := await(t, out, "the turn")
	if got.err != nil || got.res.StopReason != StopCancelled || len(m.requests()) != 1 {
		t.Fatalf("Run = %+v, %v after %d requests; want cancelled after 1", got.res, got.err, len(m.requests()))
	}
}

// An empty plan asks nobody and the turn goes on; and the guard still counts
// exit_plan_mode, whose arguments never differ: the third call in a row is
// refused without the person being asked again.
func TestExitPlanModeInTheLoop(t *testing.T) {
	t.Run("an empty plan", func(t *testing.T) {
		a := newParkedAsker()
		_, s, m := planSession(t, a)
		if err := os.WriteFile(planPathOf(s), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		m.push(callStep(callParts("c1", "exit_plan_mode", "{}")), answerWith("I will write it first"))
		if res := run(t, s, "plan it"); res.StopReason != StopEndTurn || len(m.requests()) != 2 {
			t.Fatalf("Run = %+v after %d requests", res, len(m.requests()))
		}
		if plans, _ := a.count(); plans != 0 {
			t.Fatalf("an empty plan was presented %d times", plans)
		}
	})
	t.Run("the doom loop", func(t *testing.T) {
		a := newParkedAsker()
		_, s, m := planSession(t, a)
		for i := range doomNudgeAt {
			m.push(callStep(callParts("c"+string(rune('1'+i)), "exit_plan_mode", "{}")))
		}
		m.push(answerWith("I give up"))
		for range doomNudgeAt - 1 {
			a.plan <- tool.PlanRejected
		}
		if res := run(t, s, "plan it"); res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v", res)
		}
		if plans, _ := a.count(); plans != doomNudgeAt-1 {
			t.Fatalf("the plan was presented %d times, want %d: the guard refuses the next", plans, doomNudgeAt-1)
		}
	})
}

// Outside plan mode the gate refuses exit_plan_mode by name, before it can
// read a plan file or ask anyone; the other two run in every mode.
func TestExitPlanModeOutsidePlanMode(t *testing.T) {
	for _, mode := range []string{"agent", "ask"} {
		t.Run(mode, func(t *testing.T) {
			a := newParkedAsker()
			f := newFixture(t, "http://127.0.0.1:1/v1")
			opts := modeOptions(f, mode)
			opts.Asker = a
			s := f.open(opts)
			if err := os.MkdirAll(filepath.Dir(planPathOf(s)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(planPathOf(s), []byte("# a plan from earlier"), 0o600); err != nil {
				t.Fatal(err)
			}
			m := f.models["test/a"]
			m.push(callStep(
				callParts("c1", "exit_plan_mode", "{}"),
				callParts("c2", "todo_write", `{"todos":[{"id":"a","content":"first"}]}`),
			), answerWith("ok"))
			if res := run(t, s, "go"); res.StopReason != StopEndTurn {
				t.Fatalf("Run = %+v", res)
			}
			results := lastLines(t, s, 2)[0]
			if !strings.Contains(results, "[error c1: Plan mode has been disabled.") || !strings.Contains(results, "[result c2: ") {
				t.Fatalf("the step's results:\n%s", results)
			}
			if plans, _ := a.count(); plans != 0 {
				t.Fatalf("the plan was presented %d times outside plan mode", plans)
			}
		})
	}
}

// A question is answered and the turn goes on with the answer; with no asker
// wired, nobody is there and the model is told so at once.
func TestAQuestionInATurn(t *testing.T) {
	const args = `{"questions":[{"question":"Which one?","options":[{"label":"left"},{"label":"right"}]}]}`
	t.Run("answered", func(t *testing.T) {
		a := newParkedAsker()
		f := newFixture(t, "http://127.0.0.1:1/v1")
		opts := f.options()
		opts.Asker = a
		s := f.open(opts)
		m := f.models["test/a"]
		m.push(callStep(callParts("c1", "ask_user_question", args)), answerWith("right it is"))
		out := start(context.Background(), s, "go", nil)
		if q := await(t, a.asked, "the question"); q != "Which one?" {
			t.Fatalf("the person was asked %q", q)
		}
		a.answer <- tool.Answers{Answered: true, Picked: [][]int{{1}}}
		if got := await(t, out, "the turn"); got.err != nil || got.res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v", got.res, got.err)
		}
		if got := lastLines(t, s, 2)[0]; !strings.Contains(got, `"Which one?"="right"`) {
			t.Fatalf("the model was told %q", got)
		}
	})
	t.Run("nobody to ask", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(f.options())
		m := f.models["test/a"]
		m.push(callStep(callParts("c1", "ask_user_question", args)), answerWith("my best guess, then"))
		if res := run(t, s, "go"); res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v", res)
		}
		if got := lastLines(t, s, 2)[0]; !strings.Contains(got, "[result c1: "+opencode.UnansweredText) {
			t.Fatalf("the model was told %q", got)
		}
	})
}
