package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/harness"
)

// The native adapter's modes (plan 023 §3.6): Options.Mode at Start, SetMode
// afterwards, and the state each of them publishes. The harness owns what a
// mode MEANS — the gate that judges a call, the reminder that tells the model
// (internal/harness) — so what is held here is the seam's half: the modes a
// snapshot advertises, the mode it is in, the delta that says so, and the
// value a setter answers with.

// nativeModeDeltas is the mode each EventMeta in evs reports, and it fails the
// caller's expectation loudly if one also filled Event.Mode or Event.Text:
// those two are what an *agent*-initiated update fills, a client retires a
// plan offer on Event.Mode, and no craze-initiated change has ever set either
// (plan 021 correction 20).
func nativeModeDeltas(evs []Event) []string {
	var out []string
	for _, ev := range evs {
		if ev.Type != EventMeta || ev.State == nil || ev.State.Mode == nil {
			continue
		}
		switch {
		case ev.Mode != "":
			out = append(out, "WITH Event.Mode: "+ev.Mode)
		case ev.Text != "":
			out = append(out, "WITH Event.Text: "+ev.Text)
		default:
			out = append(out, *ev.State.Mode)
		}
	}
	return out
}

// TestNativeSetMode is the setter itself: the snapshot moves, one delta says
// so and carries nothing else that could retire a plan offer, the confirmed
// value comes back, and the ticket names the delta the client can fold to.
func TestNativeSetMode(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	if got := s.Snapshot().CurrentMode; got != "agent" {
		t.Fatalf("a session with no Options.Mode starts in %q", got)
	}

	out, err := s.SetMode(context.Background(), "c-1/1", "plan")
	if err != nil {
		t.Fatalf("SetMode(plan): %v", err)
	}
	if out.Value != "plan" || out.Ticket == nil {
		t.Fatalf("SetMode(plan) = %+v, want plan with a ticket", out)
	}
	if got := s.Snapshot().CurrentMode; got != "plan" {
		t.Fatalf("the snapshot is in %q", got)
	}
	// The harness is what enforces it, and it is where the value came from.
	if got := harnessModeOf(s); got != "plan" {
		t.Fatalf("the harness is in %q", got)
	}
	// After the barrier, because a delta is enqueued under s.mu and numbered by
	// the log's drainer: the ticket answers 0 until that has happened.
	evs := deltaSettled(t, s)
	if got := nativeModeDeltas(evs); len(got) != 1 || got[0] != "plan" {
		t.Fatalf("SetMode published modes %q, want one plan", got)
	}
	// The ticket names that delta rather than the next event by luck.
	for _, ev := range evs {
		if ev.Type == EventMeta && ev.State != nil && ev.State.Mode != nil && ev.Seq != out.Ticket.Seq() {
			t.Fatalf("the mode delta is Seq %d and the ticket says %d", ev.Seq, out.Ticket.Seq())
		}
	}
	if out.Ticket.Seq() == 0 {
		t.Fatal("the ticket was never numbered")
	}
}

// TestNativeSetModeResolvesTheVocabulary: an id goes through craze's own mode
// words (ResolveMode) against the three this session advertises, exactly as it
// does for cursor — whose ids these are, so `/plan`, `/ask` and `/agent` land
// on the same three here and nothing translates. A word none of them answers
// to is refused with the list, and changes nothing.
//
// The vocabulary's other spellings — architect, code, default — are what
// another AGENT may advertise a mode as, not what a user's word resolves from
// (ResolveMode takes craze's canonical name and finds the agent's), so they are
// unknown ids here as they are on cursor.
func TestNativeSetModeResolvesTheVocabulary(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	for _, tc := range []struct{ asked, want string }{
		{"plan", "plan"},
		{"ask", "ask"},
		{"agent", "agent"},
		// Asking for the mode it is already in is accepted: the harness puts
		// the session in it afresh, which restarts the plan reminder's
		// alternation and nothing else (harness.Session.SetMode).
		{"agent", "agent"},
	} {
		got, err := setMode(s, tc.asked)
		if err != nil {
			t.Fatalf("SetMode(%q): %v", tc.asked, err)
		}
		if got != tc.want || s.Snapshot().CurrentMode != tc.want || harnessModeOf(s) != tc.want {
			t.Fatalf("SetMode(%q) = %q, snapshot %q, harness %q; want %q",
				tc.asked, got, s.Snapshot().CurrentMode, harnessModeOf(s), tc.want)
		}
	}
	deltaSettled(t, s)
	before := s.Snapshot().CurrentMode
	_, err := setMode(s, "architect")
	if err == nil || !errors.Is(err, harness.ErrUnknownMode) {
		t.Fatalf("SetMode on a mode the session does not advertise = %v", err)
	}
	for _, want := range []string{`"architect"`, "agent, plan, ask"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("SetMode error %q does not say %q", err, want)
		}
	}
	if got := s.Snapshot().CurrentMode; got != before {
		t.Fatalf("a refused SetMode moved the snapshot to %q", got)
	}
	if deltas := nativeModeDeltas(deltaSettled(t, s)); len(deltas) != 0 {
		t.Fatalf("a refused SetMode published %q", deltas)
	}
}

// TestNativeSetModeBeforeStartAndAfterClose: the setter answers the way
// SetModel answers in both states — there is no harness to ask before Start,
// and a closed one refuses — and neither leaves a mode behind.
func TestNativeSetModeBeforeStartAndAfterClose(t *testing.T) {
	f := newNativeFixture(t)
	s := f.session(Options{})
	if _, err := setMode(s, "plan"); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("SetMode before Start = %v, want the not-started refusal", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	takeStartDelta(t, s.log)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := setMode(s, "plan")
	if err == nil || !errors.Is(err, harness.ErrClosed) {
		t.Fatalf("SetMode after Close = %v, want the harness's ErrClosed", err)
	}
	if got := err.Error(); got != "agent: session closed" {
		t.Fatalf("SetMode after Close says %q", got)
	}
	if got := s.Snapshot().CurrentMode; got != "agent" {
		t.Fatalf("a refused SetMode left the session in %q", got)
	}
}

// TestNativeStartsInPlanMode is Options.Mode end to end: the CLI's --plan
// reaches the harness, the install delta says the session is in plan mode in
// the section that installed it, and the plan file — the only file an
// edit-kind call may touch there — is on disk under the harness home before
// any turn has run (plan 023 §3.2, A9).
func TestNativeStartsInPlanMode(t *testing.T) {
	f := newNativeFixture(t)
	s := f.session(Options{Mode: "plan"})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ev := takeStartDelta(t, s.log)
	if ev.State.Mode == nil || *ev.State.Mode != "plan" {
		t.Fatalf("the install delta's mode is %v, want plan", ev.State.Mode)
	}
	if ev.Mode != "" || ev.Text != "" {
		t.Fatalf("the install delta filled Mode %q / Text %q", ev.Mode, ev.Text)
	}
	snap := s.Snapshot()
	if snap.CurrentMode != "plan" || harnessModeOf(s) != "plan" {
		t.Fatalf("snapshot %q, harness %q; want plan", snap.CurrentMode, harnessModeOf(s))
	}
	if body, err := os.ReadFile(planFileIn(t, f)); err != nil || len(body) != 0 {
		t.Fatalf("the plan file reads %q, %v; want it empty and there", body, err)
	}
	// --ask is the other one the CLI can ask for; a word none of the three
	// answers to refuses Start (TestNativeStartRefusals).
	g := newNativeFixture(t)
	if got := g.started(Options{Mode: "ask"}).Snapshot().CurrentMode; got != "ask" {
		t.Fatalf("Options.Mode ask started in %q", got)
	}
}

// TestNativePlanModeTurnEndToEnd is A4 through the adapter's own mode switch
// rather than a harness seam: a session put in plan mode by SetMode writes its
// plan, offers it, has it accepted, and the turn ends there — with the mode
// still plan, because approving a plan is not leaving plan mode (D-51). The
// user leaves with /agent, which is the SetMode at the end.
func TestNativePlanModeTurnEndToEnd(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	if _, err := setMode(s, "plan"); err != nil {
		t.Fatalf("SetMode(plan): %v", err)
	}
	// The switch is what created it: entering plan mode makes the file, not the
	// turn that writes to it.
	plan := planFileIn(t, f)

	m := f.models["test/a"]
	m.push(
		nativeCallStep("c1", "write", nativeArgs(t, map[string]any{
			"filePath": plan,
			"content":  "# Ship it\n\n1. do the thing\n",
		})),
		nativeCallStep("c2", "exit_plan_mode", "{}"),
		answer("never requested"),
	)

	out := startPrompt(s, "plan it")
	p := w.waitType(EventPlan).Plan
	if p.Name != "Ship it" || !strings.Contains(p.Plan, "do the thing") {
		t.Fatalf("the plan opening is %+v", p)
	}
	if _, err := s.Asks().Answer("test/accept", p.ID, AskAnswer{Accept: true}); err != nil {
		t.Fatalf("accepting: %v", err)
	}
	got := await(t, out, "the prompt")
	if got.err != nil || got.res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want end_turn", got.res, got.err)
	}
	// Two requests: the one that wrote the plan and the one that offered it.
	// The approval's own result reaches no third, because the turn ended.
	if n := len(m.requests()); n != 2 {
		t.Fatalf("the model saw %d requests; an approved plan ends the turn", n)
	}
	if body, err := os.ReadFile(plan); err != nil || !strings.Contains(string(body), "do the thing") {
		t.Fatalf("the plan file reads %q, %v", body, err)
	}
	if mode := s.Snapshot().CurrentMode; mode != "plan" {
		t.Fatalf("the session is in %q after an approved plan", mode)
	}
	w.waitTerminal()

	// /agent, which is what the TUI's implement offer does before it sends
	// "Implement the plan above."
	if got, err := setMode(s, "agent"); err != nil || got != "agent" {
		t.Fatalf("SetMode(agent) = %q, %v", got, err)
	}
	if harnessModeOf(s) != "agent" {
		t.Fatalf("the harness is in %q", harnessModeOf(s))
	}
}

// TestNativeSetModeMidTurn: the switch is allowed while a turn runs, as
// SetModel's is — the harness decides which call the new mode judges, and
// nothing here waits for the turn to finish.
func TestNativeSetModeMidTurn(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	h := newHeld(t)
	m := f.models["test/a"]
	m.push(h.step(textParts("thinking"), append(textParts(" done"), finishParts(fantasy.FinishReasonStop)...)))

	out := startPrompt(s, "go")
	<-h.reached
	got, err := setMode(s, "ask")
	if err != nil {
		t.Fatalf("SetMode during a turn: %v", err)
	}
	if got != "ask" || s.Snapshot().CurrentMode != "ask" || harnessModeOf(s) != "ask" {
		t.Fatalf("SetMode = %q, snapshot %q, harness %q", got, s.Snapshot().CurrentMode, harnessModeOf(s))
	}
	close(h.release)
	if res := await(t, out, "the prompt"); res.err != nil {
		t.Fatalf("the turn failed: %v", res.err)
	}
	if mode := s.Snapshot().CurrentMode; mode != "ask" {
		t.Fatalf("the turn ending moved the mode to %q", mode)
	}
}

// TestNativeRacingSetModesEachAnswerWhatTheHarnessHeld is announceCurrent's
// rule for the mode (plan 023 §3.6): two setters racing each other must each
// answer with a value the HARNESS held, never with the one they asked for, and
// the last delta by Seq must be what the snapshot says.
//
// The interleaving is forced, not hoped for (r6 finding 3): the mode seam
// holds each setter after the harness took its switch and before it
// announces, until BOTH have got there. From then on the harness holds one
// mode — whichever switch landed second — and a setter that answered with
// what it asked for would answer "plan" or "ask" by its argument; one that
// reads the harness back inside the locked section answers the same word as
// its rival. Under -race it is also the lock check: the mutation and the
// enqueue are one locked section, so the deltas cannot be published in the
// other order from the one the snapshot ended up in.
func TestNativeRacingSetModesEachAnswerWhatTheHarnessHeld(t *testing.T) {
	for range 20 {
		f := newNativeFixture(t)
		s := f.started(Options{})
		var both sync.WaitGroup
		both.Add(2)
		s.mu.Lock()
		s.modeSeam = func() { both.Done(); both.Wait() }
		s.mu.Unlock()
		var wg sync.WaitGroup
		values := make([]string, 2)
		for i, id := range []string{"plan", "ask"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := setMode(s, id)
				if err != nil {
					t.Errorf("SetMode(%q): %v", id, err)
				}
				values[i] = got
			}()
		}
		wg.Wait()
		held := harnessModeOf(s)
		for i, got := range values {
			if got != held {
				t.Fatalf("setter %d answered %q; the harness held %q when both announced", i, got, held)
			}
		}
		last := ""
		for _, mode := range nativeModeDeltas(deltaSettled(t, s)) {
			last = mode
		}
		if last != held {
			t.Fatalf("the last mode delta is %q and the harness says %q", last, held)
		}
		if got := s.Snapshot().CurrentMode; got != last {
			t.Fatalf("the last mode delta is %q and the snapshot says %q", last, got)
		}
		_ = s.Close()
	}
}

// TestNativeSetModeRacingCloseIsRefused is r6 finding 1: a Close that lands
// between the harness taking the switch and the announcement must be seen by
// the announcement. Without the guard the setter rewrote a closed session's
// snapshot, enqueued into a log that had stopped admitting (a ticket of Seq
// 0), and reported success. The window is pinned from inside it with the mode
// seam, as the cancel window is with cancelSeam.
func TestNativeSetModeRacingCloseIsRefused(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	s.mu.Lock()
	s.modeSeam = func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
	s.mu.Unlock()
	out, err := s.SetMode(context.Background(), "", "plan")
	if err == nil || err.Error() != "agent: session closed" {
		t.Fatalf("SetMode racing Close = (%+v, %v), want the closed refusal", out, err)
	}
	if out.Ticket != nil || out.Value != "" {
		t.Fatalf("a refused SetMode answered %+v", out)
	}
	if got := s.Snapshot().CurrentMode; got != "agent" {
		t.Fatalf("a Close mid-SetMode left the closed snapshot in %q", got)
	}
}

// TestNativeAnnounceCurrentAfterCloseIsRefused is the same window for
// SetModel and SetConfig (plan 025 §1): a Close that lands between the harness
// taking a model or effort switch and announceCurrent must be seen by it. The
// announcement is made here with the Close already run, which is that window's
// far side: it answers "closed", rewrites nothing of the closed session's
// snapshot, and enqueues nothing into a log that has stopped admitting.
func TestNativeAnnounceCurrentAfterCloseIsRefused(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	before := s.Snapshot()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	model, effort, tk, err := s.announceCurrent("c-1/1")
	if err == nil || err.Error() != "agent: session closed" {
		t.Fatalf("announceCurrent after Close = %v, want the closed refusal", err)
	}
	if model != "" || effort != "" || tk != nil {
		t.Fatalf("a refused announcement answered %q/%q/%v", model, effort, tk)
	}
	after := s.Snapshot()
	if after.CurrentModel != before.CurrentModel || len(after.Config) != len(before.Config) {
		t.Fatalf("an announcement after Close rewrote the snapshot: %+v → %+v", before, after)
	}
}
