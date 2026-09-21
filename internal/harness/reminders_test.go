package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// Modes and their reminders (plan 023 §3.1, §3.3, §7 A1, A2, A8, A9). Every
// test here runs under the fixture's temp home, so the plan file it creates is
// there and never in the repository.

// planPathOf is the session's plan file.
func planPathOf(s *Session) string { return s.modes.planPath }

// reminderIn is the one reminder in request n of m, and the index it sits at,
// or ("", -1) when there is none. A request with more than one fails the
// test: the turn composes one, plus a transition notice the tests that want
// one look for themselves.
func reminderIn(t *testing.T, m *scripted, n int) (string, int) {
	t.Helper()
	calls := m.requests()
	if n >= len(calls) {
		t.Fatalf("the model saw %d requests, want at least %d", len(calls), n+1)
	}
	var text string
	at := -1
	for i, line := range promptOf(calls[n]) {
		if !strings.Contains(line, "<"+reminderTag+">") {
			continue
		}
		if at >= 0 {
			t.Fatalf("request %d carries two reminders: %v", n+1, promptOf(calls[n]))
		}
		text, at = line, i
	}
	return text, at
}

// remindersIn is every reminder in request n, in order.
func remindersIn(t *testing.T, m *scripted, n int) []string {
	t.Helper()
	calls := m.requests()
	if n >= len(calls) {
		t.Fatalf("the model saw %d requests, want at least %d", len(calls), n+1)
	}
	var out []string
	for _, line := range promptOf(calls[n]) {
		if strings.Contains(line, "<"+reminderTag+">") {
			out = append(out, line)
		}
	}
	return out
}

// planOptions are the fixture's options in mode.
func modeOptions(f *fixture, mode string) Options {
	opts := f.options()
	opts.Mode = mode
	return opts
}

// TestModeIsNotInTheFrozenPrompt is half of A2: the system prompt is the same
// bytes in every mode, so system_prompt.golden and its extras twin cannot
// move and every session shares one prefix (D-30). grok-build's template has
// no mode text either; the model learns the mode from the reminder.
func TestModeIsNotInTheFrozenPrompt(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	first := f.open(modeOptions(f, "")).system
	for _, mode := range []string{"agent", "plan", "ask"} {
		s := f.open(modeOptions(f, mode))
		if s.system != first {
			t.Fatalf("the system prompt differs in %s mode", mode)
		}
		if s.PromptSHA256() != f.open(modeOptions(f, "")).PromptSHA256() {
			t.Fatalf("the prompt's digest differs in %s mode", mode)
		}
	}
}

// The harness's own mode words are the tool framework's, restated across
// Seam 1 (reminders.go). Nothing but this test may compare them.
func TestModeWordsMatchTheToolFramework(t *testing.T) {
	if modeAgent != tool.ModeAgent || modePlan != tool.ModePlan || modeAsk != tool.ModeAsk {
		t.Fatalf("the harness says %q/%q/%q; the tool framework says %q/%q/%q",
			modeAgent, modePlan, modeAsk, tool.ModeAgent, tool.ModePlan, tool.ModeAsk)
	}
}

// SetMode mirrors SetModel's lifecycle: an unknown id is refused and changes
// nothing, a switch is effective at once whether or not a turn runs, and
// after Close it is ErrClosed. Current keeps its two results.
func TestSetModeLifecycle(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	if s.Mode() != modeAgent {
		t.Fatalf("a session with no mode is in %q", s.Mode())
	}
	if err := s.SetMode("architect"); !errors.Is(err, ErrUnknownMode) {
		t.Fatalf("SetMode(architect) = %v, want ErrUnknownMode", err)
	}
	if s.Mode() != modeAgent {
		t.Fatalf("a refused switch left the session in %q", s.Mode())
	}
	if _, err := Open(modeOptions(f, "architect")); !errors.Is(err, ErrUnknownMode) {
		t.Fatalf("Open with an unknown mode = %v, want ErrUnknownMode", err)
	}
	for _, mode := range []string{"plan", "ask", "agent", ""} {
		if err := s.SetMode(mode); err != nil {
			t.Fatalf("SetMode(%q): %v", mode, err)
		}
		want := mode
		if mode == "" {
			want = modeAgent
		}
		if s.Mode() != want {
			t.Fatalf("Mode() = %q after SetMode(%q)", s.Mode(), mode)
		}
	}
	// A turn is running: the switch lands, and the model and effort the turn
	// runs on are untouched.
	a := f.models["test/a"]
	g := newGate()
	a.push(g.hold(openText("thinking it over"), finishText()))
	out := start(context.Background(), s, "hi", nil)
	await(t, g.reached, "the turn's first step")
	if err := s.SetMode("plan"); err != nil {
		t.Fatalf("SetMode during a turn: %v", err)
	}
	if model, effort := s.Current(); model != "test/a" || effort != "high" {
		t.Fatalf("Current() = %q, %q; a mode switch must not touch it", model, effort)
	}
	close(g.release)
	if got := await(t, out, "the turn to end"); got.err != nil {
		t.Fatal(got.err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode("ask"); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetMode after Close = %v, want ErrClosed", err)
	}
}

// TestPlanModeRemindsEveryTurn is the alternation (plan 023 §3.3): the first
// turn in plan mode reads the full text, the next the sparse one, the next
// the full one again — each of them naming the absolute plan path, since the
// history the next turn replays has no reminder in it. A turn cancelled
// before its request goes out keeps the parity and carries no reminder at
// all, and SetMode resets it.
func TestPlanModeRemindsEveryTurn(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(modeOptions(f, "plan"))
	a := f.models["test/a"]
	a.push(answerWith("one"), answerWith("two"), answerWith("three"))
	plan := planPathOf(s)

	run(t, s, "plan a change")
	run(t, s, "and another")
	run(t, s, "and a third")

	want := []string{"Plan mode is active", "Plan mode is still active", "Plan mode is active"}
	for i, w := range want {
		text, at := reminderIn(t, a, i)
		lines := promptOf(a.requests()[i])
		switch {
		case at != len(lines)-1: // right after the user's prompt, whatever the history
			t.Fatalf("request %d's reminder is at %d of %d: %v", i+1, at, len(lines), lines)
		case !strings.Contains(text, w):
			t.Fatalf("request %d's reminder is %q, want the %q variant", i+1, text, w)
		case !strings.Contains(text, plan):
			t.Fatalf("request %d's reminder does not name the plan file: %q", i+1, text)
		}
	}

	// A turn whose context is already done never composes one, and the
	// alternation is where it was: the next turn reads the sparse text.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// What a provider answers a request cancelled as it went out.
	a.push(reply(errorPart(context.Canceled)))
	if res, err := s.Run(ctx, "withdrawn", nil); err != nil || res.StopReason != StopCancelled {
		t.Fatalf("the withdrawn turn = %+v, %v; want cancelled", res, err)
	}
	if _, at := reminderIn(t, a, 3); at != -1 {
		t.Fatalf("the withdrawn turn's request carried a reminder: %v", promptOf(a.requests()[3]))
	}
	a.push(answerWith("four"))
	run(t, s, "carry on")
	if text, _ := reminderIn(t, a, 4); !strings.Contains(text, "Plan mode is still active") {
		t.Fatalf("after a withdrawn turn the reminder is %q, want the sparse variant", text)
	}

	// SetMode starts the alternation again, whether or not the mode changes.
	if err := s.SetMode("plan"); err != nil {
		t.Fatal(err)
	}
	a.push(answerWith("five"))
	run(t, s, "again")
	if text, _ := reminderIn(t, a, 5); !strings.Contains(text, "Plan mode is active") {
		t.Fatalf("after SetMode the reminder is %q, want the full variant", text)
	}
}

// TestAgentModeSendsNoReminder is the other half of A2: a session with no
// mode composes nothing at all, so its requests are the bytes they were
// before modes existed — the prefix tests that have always passed still do.
func TestAgentModeSendsNoReminder(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	a := f.models["test/a"]
	f.put("a.txt", "alpha\n")
	a.push(callStep(callParts("c1", "read", input(t, map[string]any{"filePath": "a.txt"}))), answerWith("done"), answerWith("again"))
	run(t, s, "read it")
	run(t, s, "thanks")
	for n := range a.requests() {
		if got := remindersIn(t, a, n); len(got) != 0 {
			t.Fatalf("request %d carries %v", n+1, got)
		}
	}
}

// TestAskModeReminder: ask mode says its rule every turn, and leaving it for
// agent mode says so once. A plan-to-ask switch announces ask's restrictions
// rather than plan's exit — ask is where the model now is.
func TestAskModeReminder(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(modeOptions(f, "ask"))
	a := f.models["test/a"]
	a.push(answerWith("one"), answerWith("two"), answerWith("three"), answerWith("four"))

	run(t, s, "what does it do?")
	run(t, s, "and this?")
	for n := range 2 {
		text, at := reminderIn(t, a, n)
		if lines := promptOf(a.requests()[n]); at != len(lines)-1 || !strings.Contains(text, "Ask mode is active") {
			t.Fatalf("request %d's reminder is %q at %d of %d", n+1, text, at, len(lines))
		}
	}

	if err := s.SetMode("plan"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode("ask"); err != nil {
		t.Fatal(err)
	}
	run(t, s, "read-only again")
	if text, _ := reminderIn(t, a, 2); !strings.Contains(text, "Ask mode is active") || strings.Contains(text, "exited plan mode") {
		t.Fatalf("plan to ask told the model %q, want ask's own text", text)
	}

	if err := s.SetMode("agent"); err != nil {
		t.Fatal(err)
	}
	run(t, s, "now do it")
	if text, _ := reminderIn(t, a, 3); !strings.Contains(text, "You have exited ask mode") {
		t.Fatalf("leaving ask mode told the model %q", text)
	}
}

// TestPlanFileIsCreatedOnEntry is A9 and §3.2: the plan file appears under
// the harness home when plan mode is entered — at Open, or at the first
// SetMode — is never truncated, and a re-entry once it has content uses
// grok-build's returning text. Leaving plan mode names the file so the model
// can implement what it planned.
func TestPlanFileIsCreatedOnEntry(t *testing.T) {
	t.Run("at Open", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(modeOptions(f, "plan"))
		plan := planPathOf(s)
		if !strings.HasPrefix(plan, f.home) || !strings.HasSuffix(plan, ".plan.md") {
			t.Fatalf("the plan file is at %q, want <home>/…/<session>.plan.md", plan)
		}
		if info, err := os.Stat(plan); err != nil || info.Size() != 0 {
			t.Fatalf("stat(%s) = %v, %v; want an empty file", plan, info, err)
		}
		if got := filepath.Dir(plan); got != filepath.Dir(s.store.Path()) {
			t.Fatalf("the plan file is in %q, the transcript in %q", got, filepath.Dir(s.store.Path()))
		}
	})

	t.Run("at the first SetMode, and again after it has content", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(f.options())
		plan := planPathOf(s)
		if _, err := os.Stat(plan); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("agent mode made a plan file: %v", err)
		}
		if err := s.SetMode("plan"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(plan); err != nil {
			t.Fatalf("entering plan mode made no plan file: %v", err)
		}
		a := f.models["test/a"]
		a.push(answerWith("one"), answerWith("two"), answerWith("three"))
		run(t, s, "plan it")
		if text, _ := reminderIn(t, a, 0); !strings.Contains(text, "No plan written yet") {
			t.Fatalf("with an empty plan file the reminder says %q", text)
		}

		// The model writes the plan, the user leaves and comes back.
		if err := os.WriteFile(plan, []byte("## The plan\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.SetMode("agent"); err != nil {
			t.Fatal(err)
		}
		run(t, s, "never mind")
		if text, _ := reminderIn(t, a, 1); !strings.Contains(text, "You have exited plan mode") || !strings.Contains(text, plan) {
			t.Fatalf("leaving plan mode told the model %q", text)
		}
		if err := s.SetMode("plan"); err != nil {
			t.Fatal(err)
		}
		run(t, s, "back to planning")
		if text, _ := reminderIn(t, a, 2); !strings.Contains(text, "Returning to Plan Mode") || !strings.Contains(text, plan) {
			t.Fatalf("re-entering plan mode told the model %q", text)
		}
		if got, err := os.ReadFile(plan); err != nil || string(got) != "## The plan\n" {
			t.Fatalf("the plan file reads %q (%v); nothing may truncate it", got, err)
		}
	})
}

// TestPlanModeGate is A1 through a whole session: the gate the harness wires
// refuses an edit-kind call outside the plan file with grok-build's text, the
// model reads it, and a write of the plan file itself runs.
func TestPlanModeGate(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(modeOptions(f, "plan"))
	plan := planPathOf(s)
	a := f.models["test/a"]
	a.push(
		callStep(callParts("c1", "write", input(t, map[string]any{"filePath": "a.txt", "content": "nope"}))),
		callStep(callParts("c2", "write", input(t, map[string]any{"filePath": plan, "content": "## The plan\n"}))),
		answerWith("planned"),
	)
	run(t, s, "plan it")

	refused := promptOf(a.requests()[1])[len(promptOf(a.requests()[1]))-1]
	if !strings.Contains(refused, "file edits are not allowed in plan mode") || !strings.Contains(refused, plan) {
		t.Fatalf("the model read %q, want the plan-mode refusal", refused)
	}
	if exists(filepath.Join(f.workspace, "a.txt")) {
		t.Fatal("the refused write reached the workspace")
	}
	wrote := promptOf(a.requests()[2])[len(promptOf(a.requests()[2]))-1]
	if !strings.Contains(wrote, "Wrote file successfully") {
		t.Fatalf("writing the plan file read back %q", wrote)
	}
	if got, err := os.ReadFile(plan); err != nil || string(got) != "## The plan\n" {
		t.Fatalf("the plan file holds %q (%v)", got, err)
	}
}

// TestAskModeGate: in ask mode every call that is not read-only is refused,
// and the read-only ones run.
func TestAskModeGate(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(modeOptions(f, "ask"))
	f.put("a.txt", "alpha\n")
	a := f.models["test/a"]
	a.push(
		callStep(callParts("c1", "write", input(t, map[string]any{"filePath": "a.txt", "content": "nope"}))),
		callStep(callParts("c2", "read", input(t, map[string]any{"filePath": "a.txt"}))),
		answerWith("it says alpha"),
	)
	run(t, s, "what does a.txt say?")

	lines := promptOf(a.requests()[1])
	if got := lines[len(lines)-1]; !strings.Contains(got, "ask mode is read-only") {
		t.Fatalf("the model read %q, want ask mode's refusal", got)
	}
	if got, err := os.ReadFile(filepath.Join(f.workspace, "a.txt")); err != nil || string(got) != "alpha\n" {
		t.Fatalf("a.txt = %q (%v); the refused write changed it", got, err)
	}
	lines = promptOf(a.requests()[2])
	if got := lines[len(lines)-1]; !strings.Contains(got, "1: alpha") {
		t.Fatalf("the read was not allowed: %q", got)
	}
}

// TestModeChangeAtAStepBoundary is A8: a switch made between two steps of a
// running turn is announced at the next boundary and recorded there — held,
// like a model change, until that step's output is written — so the entry
// sits where the change became visible in the conversation.
func TestModeChangeAtAStepBoundary(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.put("a.txt", "alpha\n")
	a := f.models["test/a"]
	a.push(callStep(callParts("c1", "read", input(t, map[string]any{"filePath": "a.txt"}))), answerWith("done"))

	var switched bool
	_, err := s.Run(context.Background(), "read it", func(ev Event) {
		if d, ok := ev.(StepDone); ok && d.Step == 1 && !switched {
			switched = true
			if err := s.SetMode("plan"); err != nil {
				t.Errorf("SetMode between steps: %v", err)
			}
		}
	})
	if err != nil || !switched {
		t.Fatalf("Run = %v, switched = %v", err, switched)
	}
	if _, at := reminderIn(t, a, 0); at != -1 {
		t.Fatal("the first step's request carried a reminder")
	}
	text, at := reminderIn(t, a, 1)
	if at != len(promptOf(a.requests()[1]))-1 || !strings.Contains(text, "Plan mode is active") {
		t.Fatalf("the second step's notice is %q at %d", text, at)
	}
	// The entry is between the tool step and the step that went out under the
	// new mode, and it was held until that step's output was written.
	lines := entries(transcript(t, s))
	if len(lines) != 5 || lines[3] != "mode_change plan" || lines[4] != "assistant test/a high end_turn: done" {
		t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
	}
}

// A notice never moves or rewrites the reminder already in the turn (plan 023
// §3.3): a turn that starts in plan mode and leaves it mid-way shows the
// model both, the first at the index it has had since step 0 — so the prefix
// the provider cached is not rewritten — and the second at the boundary where
// the mode changed.
func TestATransitionNoticeLeavesTheEarlierReminderAlone(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(modeOptions(f, "plan"))
	f.put("a.txt", "alpha\n")
	a := f.models["test/a"]
	a.push(callStep(callParts("c1", "read", input(t, map[string]any{"filePath": "a.txt"}))), answerWith("done"))

	var switched bool
	if _, err := s.Run(context.Background(), "plan it", func(ev Event) {
		if d, ok := ev.(StepDone); ok && d.Step == 1 && !switched {
			switched = true
			if err := s.SetMode("agent"); err != nil {
				t.Errorf("SetMode between steps: %v", err)
			}
		}
	}); err != nil || !switched {
		t.Fatalf("Run = %v, switched = %v", err, switched)
	}

	first, second := promptOf(a.requests()[0]), promptOf(a.requests()[1])
	got := remindersIn(t, a, 1)
	if len(got) != 2 {
		t.Fatalf("the second request carries %d reminders, want the turn's and the notice: %v", len(got), second)
	}
	if got[0] != first[2] || second[2] != first[2] {
		t.Fatalf("the turn's reminder moved or changed:\n%q\n%q", first[2], second[2])
	}
	if !strings.Contains(got[1], "You have exited plan mode") || got[1] != second[len(second)-1] {
		t.Fatalf("the notice is %q, want the exit text at the end", got[1])
	}
	lines := entries(transcript(t, s))
	if len(lines) != 6 || lines[0] != "mode_change plan" || lines[4] != "mode_change agent" {
		t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
	}
}

// A switch the model is never told about — because no step's request went out
// under it — is not recorded; one it is told about is held, exactly like a
// model change, until a step produces output.
func TestModeChangeWithoutAnOutputStep(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(answerWith("one"), reply(reasoningParts("just thinking"), finish(fantasy.FinishReasonStop)), answerWith("two"))
	run(t, s, "hi")

	// Switched twice and back before any boundary: there is nothing to say.
	for _, mode := range []string{"plan", "agent"} {
		if err := s.SetMode(mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetMode("ask"); err != nil {
		t.Fatal(err)
	}
	// A turn that thinks and produces nothing: the change is held, as a model
	// change would be, and the next turn's write carries it.
	if res := run(t, s, "nothing to say"); res.StopReason != StopEndTurn {
		t.Fatalf("the thinking turn = %+v", res)
	}
	if lines := entries(transcript(t, s)); len(lines) != 2 {
		t.Fatalf("the no-output turn wrote something:\n%s", strings.Join(lines, "\n"))
	}
	run(t, s, "now answer")
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: hi",
		"assistant test/a high end_turn: one",
		"mode_change ask",
		"user test/a high: now answer",
		"assistant test/a high end_turn: two",
	})
}

// TestRemindersAreNotTheConversation is A2's provenance half: on the success
// path, on a cancelled turn and on one whose step could not be saved, no
// reminder text reaches the transcript's bytes, any event, or
// Result.Unanswered — while the model reads one in every request. A steer in
// the same turn is the control: it is reported, persisted and returned, and
// it keeps its own index.
func TestRemindersAreNotTheConversation(t *testing.T) {
	const canaryText = "<" + reminderTag + ">"

	t.Run("the success path", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(modeOptions(f, "plan"))
		a := f.models["test/a"]
		g := newGate()
		a.push(
			g.hold(callParts("c1", "read", input(t, map[string]any{"filePath": "a.txt"})), finish(fantasy.FinishReasonToolCalls)),
			answerWith("done"),
		)
		f.put("a.txt", "alpha\n")
		var ev events
		out := start(context.Background(), s, "plan it", ev.sink)
		await(t, g.reached, "the first step")
		if err := sendSteer(s, steerText); err != nil {
			t.Fatal(err)
		}
		close(g.release)
		got := await(t, out, "the turn to end")
		if got.err != nil || len(got.res.Unanswered) != 0 {
			t.Fatalf("Run = %+v, %v", got.res, got.err)
		}
		// The reminder is at its own index and the steer at its own, in the
		// step that took the steer up.
		text, at := reminderIn(t, a, 1)
		if at != 2 || !strings.Contains(text, "Plan mode is active") {
			t.Fatalf("the reminder is %q at %d", text, at)
		}
		if line := requestLine(t, a, 1, 5); line != "user: "+steerText {
			t.Fatalf("the steer is at %q, want index 5 after the reminder", line)
		}
		noReminder(t, s, ev.list(), got.res, canaryText)
		// The control: the steer, which is the conversation, is everywhere.
		if !strings.Contains(strings.Join(entries(transcript(t, s)), "\n"), steerText) {
			t.Fatal("control: the steer is not in the transcript")
		}
	})

	t.Run("a cancelled turn", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(modeOptions(f, "ask"))
		a := f.models["test/a"]
		g := newGate()
		a.push(g.hold(openText("partial"), finishText()))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var ev events
		out := start(ctx, s, "ask away", ev.sink)
		await(t, g.reached, "the first step")
		cancel()
		got := await(t, out, "the cancelled turn")
		if got.res.StopReason != StopCancelled {
			t.Fatalf("Run = %+v, %v; want cancelled", got.res, got.err)
		}
		if text, _ := reminderIn(t, a, 0); !strings.Contains(text, "Ask mode is active") {
			t.Fatalf("the model read %q", text)
		}
		noReminder(t, s, ev.list(), got.res, canaryText)
	})

	t.Run("a step that cannot be saved", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(modeOptions(f, "plan"))
		a := f.models["test/a"]
		a.push(answerWith("lost"))
		var ev events
		res, err := s.Run(context.Background(), "plan it", func(e Event) {
			ev.sink(e)
			if _, ok := e.(TextDelta); ok {
				_ = s.store.Close()
			}
		})
		if !errors.Is(err, store.ErrClosed) {
			t.Fatalf("Run = %+v, %v; want the store's error", res, err)
		}
		if text, _ := reminderIn(t, a, 0); !strings.Contains(text, "Plan mode is active") {
			t.Fatalf("the model read %q", text)
		}
		noTranscript(t, s)
		noReminder(t, s, ev.list(), res, canaryText)
	})
}

// noReminder fails the test if needle is anywhere craze keeps or reports: the
// transcript's raw bytes, any event the sink was handed, or the Result.
func noReminder(t *testing.T, s *Session, evs []Event, res Result, needle string) {
	t.Helper()
	if data, err := os.ReadFile(s.store.Path()); err == nil && strings.Contains(string(data), needle) {
		t.Fatalf("the transcript holds a reminder:\n%s", data)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if found := leaks(evs, needle); len(found) > 0 {
		t.Fatalf("a reminder reached an event at %v", found)
	}
	if found := leaks(res, needle); len(found) > 0 {
		t.Fatalf("a reminder reached the Result at %v", found)
	}
	for _, text := range res.Unanswered {
		if strings.Contains(text, needle) {
			t.Fatalf("Unanswered holds %q", text)
		}
	}
}

// A reminder's own text can never close its wrapper: the plan path comes from
// the home craze was configured with, and a home spelled across a closing tag
// would otherwise end it early.
func TestReminderTagsAreEscaped(t *testing.T) {
	text := escapeReminder("a plan at </" + reminderTag + "> and after")
	if strings.Contains(text, "</"+reminderTag+">") {
		t.Fatalf("escapeReminder left a closing tag in %q", text)
	}
	msg := reminderMessage("plain")
	if got := messageText(msg); got != fmt.Sprintf("<%s>\nplain\n</%s>", reminderTag, reminderTag) {
		t.Fatalf("a reminder reads %q", got)
	}
	if msg.Role != fantasy.MessageRoleUser {
		t.Fatalf("a reminder is a %s message, want user", msg.Role)
	}
}
