package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// TestClassifyIsTheOneTable is r28 finding 1: walks EVERY exported sentinel
// Code (control.go) can return — engine's own and the ones it takes from
// internal/agent — plus errNotRun (settings.go, unexported, r28's own new
// one) and a plain unclassified error standing for classify's "failed"
// default, and asserts the invariant classify exists to hold over the WHOLE
// closed set:
//
//	Code(err) ∈ {unavailable, not_accepting, in_progress} ⇔ the receipt was
//	NOT stored — gateRefusal(err) is true, the same id may be resent, and
//	classify(err).stored is false.
//
// Code and gateRefusal are both DERIVED from classify (control.go,
// receipts.go), so this one table is what pins them from drifting apart
// again the way r28 finding 1 found them apart over agent.ErrNotInTurn and a
// Set's dead ctx: Code already said not_accepting for ErrNotInTurn while
// gateRefusal did not know it, and a command the docs promise is never stored
// was stored anyway.
func TestClassifyIsTheOneTable(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name string
		err  error
		code string
		// forgot is gateRefusal(err): the answer is NOT stored, and the same id
		// may be resent for a genuine first attempt.
		forgot bool
	}{
		{"ErrIndexWrite", fmt.Errorf("%w: %w", ErrIndexWrite, boom), "index_write", false},
		{"ErrNotAccepting", ErrNotAccepting, "not_accepting", true},
		{"agent.ErrNotInTurn", agent.ErrNotInTurn, "not_accepting", true},
		{"ErrCommandInProgress", ErrCommandInProgress, "in_progress", true},
		{"ErrAlreadyPending", ErrAlreadyPending, "already_submitted", false},
		{"ErrStaleTurn", ErrStaleTurn, "stale_turn", false},
		{"ErrStaleVersion", ErrStaleVersion, "stale_version", false},
		{"ErrUnknownRow", ErrUnknownRow, "unknown_row", false},
		{"agent.ErrBadAnswer", agent.ErrBadAnswer, "bad_request", false},
		{"agent.ErrAlreadyResolved", agent.ErrAlreadyResolved, "already_resolved", false},
		{"agent.ErrUnknownAsk", agent.ErrUnknownAsk, "unknown_ask", false},
		{"ErrCommandAborted", ErrCommandAborted, "aborted", false},
		{"ErrSetOutcomeUnknown", setOutcomeUnknown(context.Canceled), "aborted", false},
		{"errNotRun (a Set that never ran)", notRun(context.Canceled), "unavailable", true},
		{"agent.ErrAskUnavailable", agent.ErrAskUnavailable, "unavailable", true},
		{"agent.ErrSetUnavailable", agent.ErrSetUnavailable, "unavailable", true},
		{"ErrUnavailable", ErrUnavailable, "unavailable", true},
		{"ErrBadRequest", ErrBadRequest, "bad_request", false},
		{"ErrUnknownCommand", ErrUnknownCommand, "unknown_command", false},
		{"agent.ErrQueueFull", agent.ErrQueueFull, "queue_full", false},
		{"agent.ErrQueueTextTooLong", agent.ErrQueueTextTooLong, "text_too_long", false},
		{"agent.ErrPromptInFlight", agent.ErrPromptInFlight, "prompt_in_flight", false},
		{"agent.ErrForeignTurn", agent.ErrForeignTurn, "foreign_turn", false},
		{"agent.ErrPromptCancelled", agent.ErrPromptCancelled, "prompt_cancelled", false},
		{"agent.ErrUnsupported", agent.ErrUnsupported, "unsupported", false},
		{"an unclassified provider/RPC error (classify's own default)", boom, "failed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Code(tc.err); got != tc.code {
				t.Fatalf("Code(%v) = %q, want %q", tc.err, got, tc.code)
			}
			if got := gateRefusal(tc.err); got != tc.forgot {
				t.Fatalf("gateRefusal(%v) = %v, want %v", tc.err, got, tc.forgot)
			}
			// The invariant itself, in both directions, over THIS row: forgotten
			// iff the code is one of the three codes never stored. A row that
			// fails this has a bad table, not a bad implementation — classify's
			// own switch is what both Code and gateRefusal read, so the two
			// cannot disagree with each other; they can still disagree with the
			// invariant, which is what this line catches.
			wantForgotten := tc.code == "unavailable" || tc.code == "not_accepting" || tc.code == "in_progress"
			if tc.forgot != wantForgotten {
				t.Fatalf("%s: table says forgotten=%v for code %q, want %v", tc.name, tc.forgot, tc.code, wantForgotten)
			}
		})
	}
	if got := Code(nil); got != "" {
		t.Fatalf("Code(nil) = %q, want \"\"", got)
	}
	if gateRefusal(nil) {
		t.Fatal("gateRefusal(nil) must be false: nil never reaches finish as a refusal")
	}
}

// TestClassifyThroughRealCommands drives three of the table above through an
// actual Control call, where the fake session makes that feasible: the
// dead-ctx Set (errNotRun), a Set the provider refuses, and a Cancel that
// fails — the two new "failed" schedules r28 finding 1 names by name
// ("a provider / RPC refusal of a Set... or a Cancel that failed"). Every
// other row already has its own dedicated, real-command coverage elsewhere
// (cancel_test.go, sendnow_test.go, settings_test.go, queue_test.go,
// receipts_test.go, lifecycle_test.go, driver_test.go, index_test.go), which
// is what the table above exists to pin together rather than duplicate.
func TestClassifyThroughRealCommands(t *testing.T) {
	t.Run("errNotRun: a Set made with an already-dead context", func(t *testing.T) {
		r := newRig(t, Options{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := r.e.Set(ctx, Command{}, modeSetting("plan"))
		if Code(err) != "unavailable" || !errors.Is(err, context.Canceled) {
			t.Fatalf("a Set made with a dead context: %v (%s), want context.Canceled coded unavailable", err, Code(err))
		}
		if n := r.s.setCalls(); n != 0 {
			t.Fatalf("%d settings reached the provider for a dead-ctx Set", n)
		}
	})
	t.Run("failed: a Set the provider refused", func(t *testing.T) {
		r := newRig(t, Options{})
		boom := errors.New("the agent refused")
		r.s.failSets(boom)
		_, err := r.e.Set(context.Background(), Command{}, modeSetting("plan"))
		if Code(err) != "failed" || !errors.Is(err, boom) {
			t.Fatalf("a Set the provider refused: %v (%s), want it wrapped errors.Is boom and coded failed", err, Code(err))
		}
	})
	t.Run("failed: a Cancel that failed", func(t *testing.T) {
		r := newRig(t, Options{})
		turn := r.s.script(held())
		res := r.submit("one")
		await(t, turn.opened, "the turn to open")
		boom := errors.New("the pipe is gone")
		r.s.mu.Lock()
		r.s.cancelErr = boom
		r.s.mu.Unlock()
		_, err := r.e.Cancel(context.Background(), Command{}, res.Turn)
		if Code(err) != "failed" || !errors.Is(err, boom) {
			t.Fatalf("a cancel that failed: %v (%s), want it wrapped errors.Is boom and coded failed", err, Code(err))
		}
		turn.release()
		r.until(lastEnding)
	})
}
