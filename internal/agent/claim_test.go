package agent

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// These tests pin the second half of issue #18: the window before a prompt's
// turn is open. The TUI says "working" inside Update and runs the prompt on a
// Cmd goroutine, so an Esc can reach Cancel before that goroutine has opened
// anything. Begin closes it by claiming the prompt slot synchronously; a Cancel
// that finds the claim waits on the claimed prompt's wire, and the prompt,
// finding itself cancelled before its turn opens, withdraws.

// claimState is what a claim leaves behind, read where it is kept.
func claimState(s *session) (claimed, in bool, wire *turnWire, turn int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimed, s.inPrompt, s.wire, s.turn
}

// TestBeginThenCancelWithdrawsThePrompt is Esc straight after Enter, against
// the real fake agent: the claim is taken, Cancel lands on it, and only then
// does the continuation run. The prompt withdraws and the cancel writes nothing,
// so nothing reached the agent at all — and the callorder receipt of the next
// prompt, which names every prompt and cancel the fake ever read, says exactly
// that: one prompt, and no cancel.
func TestBeginThenCancelWithdrawsThePrompt(t *testing.T) {
	s := startScript(t, "callorder", true)
	log := collect(t, s)
	_, _, _, turnBefore := claimState(s)

	run := s.Begin("first")
	cancelled := cancelBounded(t, s)
	waitCancelling(t, s)
	cancelStillWaiting(t, cancelled, "before the claimed prompt had run at all")

	if err := promptReturn(t, runOn(run), "the claimed prompt never came back"); !errors.Is(err, ErrPromptCancelled) {
		t.Fatalf("the claimed prompt returned %v", err)
	}
	if err := cancelReturn(t, cancelled, 15*time.Second); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// Withdrawn, not rolled back: no turn was opened and no number spent, and
	// the claim is gone with its wire.
	if claimed, in, wire, turn := claimState(s); claimed || in || wire != nil || turn != turnBefore {
		t.Fatalf("claimed=%v inPrompt=%v wire=%v turn=%d (was %d) after a withdraw", claimed, in, wire != nil, turn, turnBefore)
	}

	if _, err := s.Prompt(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	log.waitTexts(t, "echo: second\ncalls: prompt\n")
}

// TestBeginWithNoCancelIsPrompt: a claim that nobody cancels is a prompt, and
// Prompt is exactly Begin and its continuation back to back. The claim alone
// writes nothing; the continuation, however late, writes the same request
// Prompt writes and returns the same result for the same reply; and each claim
// is released with its turn, so the next one is not refused.
func TestBeginWithNoCancelIsPrompt(t *testing.T) {
	p := newInProcess(t, acp.DialectCursor, nil)
	type outcome struct {
		res Result
		err error
	}
	drive := func(prompt func() (Result, error)) ([]byte, outcome) {
		t.Helper()
		out := make(chan outcome, 1)
		go func() {
			res, err := prompt()
			out <- outcome{res, err}
		}()
		req := p.expect(t, acp.MethodSessionPrompt)
		p.reply(t, req.ID, map[string]string{"stopReason": acp.StopEndTurn})
		select {
		case o := <-out:
			return req.Params, o
		case <-time.After(5 * time.Second):
			t.Fatal("the prompt never came back")
			return nil, outcome{}
		}
	}

	run := p.s.Begin("hi")
	p.expectNothing(t, "a claim on its own")
	viaBegin, begun := drive(func() (Result, error) { return run(t.Context()) })
	viaPrompt, prompted := drive(func() (Result, error) { return p.s.Prompt(t.Context(), "hi") })

	if !bytes.Equal(viaBegin, viaPrompt) {
		t.Fatalf("Begin wrote %s, Prompt wrote %s", viaBegin, viaPrompt)
	}
	if begun != prompted || begun.err != nil || begun.res.StopReason != acp.StopEndTurn {
		t.Fatalf("Begin returned %+v, Prompt returned %+v", begun, prompted)
	}
	if claimed, in, wire, _ := claimState(p.s); claimed || in || wire != nil {
		t.Fatalf("claimed=%v inPrompt=%v wire=%v after both turns", claimed, in, wire != nil)
	}
}

// TestACancelBeforeTheClaimIsNotForIt pins where the cancel mark is cleared.
// Begin clears it, because a cancel asked before the claim was for whatever
// ran then; the opening does not, because a cancel asked after the claim is
// this prompt's and the opening is where the prompt reads it. Neither prompt
// here would wait for a catalog, so the opening is the only place either could
// be withdrawn.
func TestACancelBeforeTheClaimIsNotForIt(t *testing.T) {
	t.Run("a cancel before the claim", func(t *testing.T) {
		p := newInProcess(t, acp.DialectCursor, nil)
		// Nothing is running, so it goes out at once — and leaves the mark.
		cancelled := cancelBounded(t, p.s)
		p.expectCancel(t)
		if err := cancelReturn(t, cancelled, 5*time.Second); err != nil {
			t.Fatalf("cancel: %v", err)
		}

		out := runOn(p.s.Begin("after"))
		req := p.expect(t, acp.MethodSessionPrompt)
		p.reply(t, req.ID, map[string]string{"stopReason": acp.StopEndTurn})
		if err := promptReturn(t, out, "the prompt claimed after the cancel never came back"); err != nil {
			t.Fatalf("a prompt claimed after an earlier cancel: %v", err)
		}
	})

	t.Run("a cancel after the claim", func(t *testing.T) {
		p := newInProcess(t, acp.DialectCursor, nil)
		run := p.s.Begin("claimed")
		cancelled := cancelBounded(t, p.s)
		waitCancelling(t, p.s)
		if err := promptReturn(t, runOn(run), "the cancelled claim never came back"); !errors.Is(err, ErrPromptCancelled) {
			t.Fatalf("the claimed prompt returned %v", err)
		}
		if err := cancelReturn(t, cancelled, 5*time.Second); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		p.expectNothing(t, "a withdrawn prompt, or a cancel for it")
	})
}

// TestACancelledClaimSkipsTheCatalogWait is a Cancel landing between the claim
// and the catalog wait. Cancel looks for a wait to abort and finds none — the
// prompt has not registered one yet — so it marks the prompt and waits on its
// wire. A prompt that parked now would hold that Cancel for the whole window,
// and the TUI's 2s cancel would fail long before it ended; it has to see the
// mark first and not park at all. The window is set far past promptReturn's
// deadline, so a prompt that parked fails there rather than passing slowly.
func TestACancelledClaimSkipsTheCatalogWait(t *testing.T) {
	shortCatalogWait(t, longCatalogWait)
	p := newInProcess(t, acp.DialectCursor, nil)
	s := p.s
	withProbePlugins(t, s)
	const text = "/probe-echo banana"
	// The wait itself, on a context already done, says whether this prompt
	// would park, so the test cannot pass because it would never have waited
	// anyway. It takes its registration back before returning, so it leaves
	// nothing behind for the claim below.
	dead, stop := context.WithCancel(context.Background())
	stop()
	if _, waited, _, err := awaitOnce(dead, s, text); err != nil || !waited {
		t.Fatalf("the prompt would not wait for the catalog: waited=%v err=%v", waited, err)
	}

	run := s.Begin(text)
	cancelled := cancelBounded(t, s)
	waitCancelling(t, s)
	if err := promptReturn(t, runOn(run), "a cancelled claim parked for the catalog"); !errors.Is(err, ErrPromptCancelled) {
		t.Fatalf("the claimed prompt returned %v", err)
	}
	if err := cancelReturn(t, cancelled, 5*time.Second); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	s.mu.Lock()
	registered := s.catalogAbort
	s.mu.Unlock()
	if registered != nil {
		t.Fatal("a cancelled claim left a catalog wait registered")
	}
	p.expectNothing(t, "a prompt cancelled before its catalog wait, or a cancel for it")
}

// TestASecondBeginIsRefusedAndTouchesNothing: a Begin while another prompt
// holds the slot claims nothing. Had it gone ahead it would have replaced the
// holder's wire and cleared its cancel mark, and a Cancel for the holder would
// then have decided on the wrong prompt. The holder is caught in both of its
// states: claimed and not yet open, and open with its prompt on the wire.
func TestASecondBeginIsRefusedAndTouchesNothing(t *testing.T) {
	t.Run("the holder is claimed, not open", func(t *testing.T) {
		p := newInProcess(t, acp.DialectCursor, nil)
		runA := p.s.Begin("a")
		_, _, wireA, _ := claimState(p.s)
		cancelled := cancelBounded(t, p.s)
		waitCancelling(t, p.s)

		if _, err := p.s.Begin("b")(t.Context()); !errors.Is(err, ErrPromptInFlight) {
			t.Fatalf("a second claim returned %v", err)
		}
		if _, _, wire, _ := claimState(p.s); wire != wireA {
			t.Fatal("the refused claim replaced the holder's wire")
		}
		// The holder's cancel survived the refused claim: it still withdraws.
		if err := promptReturn(t, runOn(runA), "the holder never came back"); !errors.Is(err, ErrPromptCancelled) {
			t.Fatalf("the holder returned %v", err)
		}
		if err := cancelReturn(t, cancelled, 5*time.Second); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		p.expectNothing(t, "a withdrawn prompt, a refused one, or a cancel for either")
	})

	t.Run("the holder's turn is open", func(t *testing.T) {
		p := newInProcess(t, acp.DialectCursor, nil)
		outA := promptOn(p.s, "a")
		req := p.expect(t, acp.MethodSessionPrompt)
		_, _, wireA, _ := claimState(p.s)

		if _, err := p.s.Begin("b")(t.Context()); !errors.Is(err, ErrPromptInFlight) {
			t.Fatalf("a second claim returned %v", err)
		}
		if _, _, wire, _ := claimState(p.s); wire != wireA {
			t.Fatal("the refused claim replaced the running turn's wire")
		}
		// Cancel still decides on the running turn's wire, which says sent.
		cancelled := cancelBounded(t, p.s)
		p.expectCancel(t)
		p.reply(t, req.ID, map[string]string{"stopReason": acp.StopCancelled})
		if err := promptReturn(t, outA, "the running turn never came back"); err != nil {
			t.Fatalf("the running turn: %v", err)
		}
		if err := cancelReturn(t, cancelled, 5*time.Second); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		p.expectNothing(t, "anything for the refused claim")
	})
}

// TestCancelAfterBeginDuringAForeignTurn is the corner the claim accepts. grok
// is running a turn of its own when the user's prompt is claimed, and Esc
// lands before the prompt's turn opens. The Esc is the claimed prompt's: the
// prompt withdraws and nothing is written, so the foreign turn keeps running —
// a cancel written for the withdrawn prompt would stop a turn the user never
// started. The next Cancel finds no claim and no turn of craze's own, which is
// the path that has always stopped a foreign turn, and writes at once.
func TestCancelAfterBeginDuringAForeignTurn(t *testing.T) {
	p := newInProcess(t, acp.DialectGrok, nil)
	p.startForeignTurn(t)

	run := p.s.Begin("mine")
	first := cancelBounded(t, p.s)
	waitCancelling(t, p.s)
	p.expectNothing(t, "a cancel for a prompt claimed and not yet open")
	cancelStillWaiting(t, first, "before the claimed prompt had run at all")

	if err := promptReturn(t, runOn(run), "the claimed prompt never came back"); !errors.Is(err, ErrPromptCancelled) {
		t.Fatalf("the claimed prompt returned %v", err)
	}
	if err := cancelReturn(t, first, 5*time.Second); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	p.expectNothing(t, "the withdrawn prompt, or a cancel for it")
	if !p.client.ForeignTurnRunning() {
		t.Fatal("the foreign turn ended, yet nothing was written to end it")
	}

	second := cancelBounded(t, p.s)
	p.expectCancel(t)
	if err := cancelReturn(t, second, 5*time.Second); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	p.expectNothing(t, "a second cancel")
}
