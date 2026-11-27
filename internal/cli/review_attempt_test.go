package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/routing"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/transcript"
)

func TestReviewAttemptTimer(t *testing.T) {
	timer := newAttemptTimer(time.Now().Add(time.Minute), 60*time.Millisecond)
	defer timer.close()
	if !timer.expiry.IsZero() {
		t.Fatal("armed before submission")
	}
	timer.arm()
	timer.progress("inc", 1)
	timer.mu.Lock()
	stamp := timer.expiry
	timer.mu.Unlock()
	timer.progress("inc", 1)
	timer.progress("old", 2)
	timer.mu.Lock()
	same := timer.expiry.Equal(stamp)
	timer.mu.Unlock()
	if !same {
		t.Fatal("duplicate or unrelated event reset the watchdog")
	}
	select {
	case <-timer.timeout:
		if _, stalled := timer.expiration(); !stalled {
			t.Fatal("stall reported as cap")
		}
	case <-time.After(serveStep):
		t.Fatal("watchdog never fired")
	}
	capTimer := newAttemptTimer(time.Now().Add(25*time.Millisecond), 0)
	defer capTimer.close()
	capTimer.arm()
	select {
	case <-capTimer.timeout:
		if _, stalled := capTimer.expiration(); stalled {
			t.Fatal("disabled stall fired")
		}
	case <-time.After(serveStep):
		t.Fatal("cap never fired")
	}
}

func TestReviewAttemptStopsOwnedHost(t *testing.T) {
	env, ws, kids := unattendedHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	ctx, cancel := context.WithTimeout(t.Context(), serveStep)
	defer cancel()
	a := runUnattendedAttempt(ctx, ws, "review", routing.Choice{Provider: "cursor"}, false, 0)
	if a.cleanupErr != nil || a.turn.Status != statusCompleted || a.turn.message() != "echo: review" {
		t.Fatalf("review %+v cleanup %v", a.turn, a.cleanupErr)
	}
	checkArgv(t, childArgv(t, kids.cmd(t, 0)), []string{"--ask", "--no-force"}, []string{"--force"})
	entries, err := rundir.Hosts(env)
	if err != nil || len(entries) != 0 {
		t.Fatalf("hosts %+v, %v", entries, err)
	}
	cancel()
	a = runUnattendedAttempt(ctx, ws, "again", routing.Choice{Provider: "cursor"}, false, 0)
	if a.turn.Status != statusCancelled || kids.count() != 1 {
		t.Fatal(a, kids.count())
	}
}

func TestReviewAttemptSilentTurnStopsWithinBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cap, stall time.Duration
		status     string
		kind       errkind.Kind
	}{
		{"stall", serveStep, 180 * time.Millisecond, statusFailed, errkind.Stalled},
		{"cap", 800 * time.Millisecond, 0, reviewTimedOut, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws, _ := unattendedHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "hang")
			ctx, cancel := context.WithTimeout(t.Context(), tc.cap)
			defer cancel()
			a := runUnattendedAttempt(ctx, ws, "silent review", routing.Choice{Provider: "cursor"}, false, tc.stall)
			if a.cleanupErr != nil || a.turn.Status != tc.status || tc.kind != "" && (a.turn.Error == nil || a.turn.Error.Kind != tc.kind) {
				t.Fatalf("turn %+v, error %+v, cleanup %v", a.turn, a.turn.Error, a.cleanupErr)
			}
			entries, err := rundir.Hosts(env)
			if err != nil || len(entries) > 0 {
				t.Fatalf("hosts %+v, %v", entries, err)
			}
		})
	}
}

func TestReviewCleanupRejectsFIFOAfterStoppingHost(t *testing.T) {
	env, _, _ := unattendedHome(t)
	path := filepath.Join(t.TempDir(), "groups")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	child, ready, err := hostspawn.StartCmd(exec.Command("sleep", "30"), hostspawn.HostChildEnv, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	defer child.End(time.Second)
	done := make(chan error, 1)
	go func() { done <- reviewAfter(reviewHost{child: child}, env) }()
	select {
	case err := <-done:
		if err == nil || !child.Exited() {
			t.Fatalf("cleanup %v, exited %v", err, child.Exited())
		}
	case <-time.After(serveStep):
		t.Fatal("cleanup blocked on the FIFO")
	}
}

func TestReviewDeadlineDuringIdentityProofSendsNothing(t *testing.T) {
	r := newWaitRig(t, 0, answerTurn(textEv("unexpected")))
	conn, tp := r.dialTapped()
	started, release := make(chan struct{}), r.gate()
	tp.on(`"method":"`+protocol.MethodSessionsList+`"`, func() bool { close(started); <-release.ch; return true })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	g := submitGate{submit: submitPrompt(conn, "expired")}
	w := goWait(conn, waitStart{submit: g.run, submitContext: ctx}, waitOptions{})
	awaited(t, started, "identity proof")
	cancel()
	release.open()
	_, failure := w.result(t)
	if failure == nil || len(tp.lines(protocol.MethodSessionPrompt)) != 0 || len(r.s.sent()) != 0 {
		t.Fatal("a prompt reached the host after cancellation", failure)
	}
}

func TestReviewStartupBudgetAllowsFallback(t *testing.T) {
	_, ws, _ := unattendedHome(t)
	shorten(t, &reviewStartupBudget, 200*time.Millisecond)
	// A configured provider accepts the launch but never responds to ACP.
	script := filepath.Join(t.TempDir(), "silent-provider")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	writeCrazeConfig(t, fmt.Sprintf("[agents]\ncursor = %q\n", script))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	a := runUnattendedAttempt(ctx, ws, "review", routing.Choice{Provider: "cursor"}, false, 0)
	if a.cleanupErr != nil || !a.startFailure || a.turn.Error == nil || a.turn.Error.Kind != errkind.StartFailed || ctx.Err() != nil {
		t.Fatalf("turn %+v cleanup %v", a.turn, a.cleanupErr)
	}
}

func TestReviewProgressMatchesSubmittedTurn(t *testing.T) {
	p := reviewProgress{prompt: "mine"}
	start := func(id, text string) agent.Event {
		return agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: id, Text: text, Phase: agent.TurnStarted}}
	}
	foreign := func(active bool) agent.Event {
		return agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "wake", Running: active}}
	}
	mine := waitTarget{kind: targetTurn, id: "turn-2"}
	for _, c := range []struct {
		target waitTarget
		ev     agent.Event
		want   bool
	}{
		{waitTarget{}, start("turn-1", "theirs"), false}, {waitTarget{}, textEv("other"), false},
		{waitTarget{}, start("turn-2", "mine"), true}, {waitTarget{}, textEv("before reply"), true},
		{mine, agent.Event{Type: agent.EventMeta}, false}, {mine, foreign(true), false},
		{mine, textEv("wake"), false}, {mine, foreign(false), false}, {mine, textEv("after reply"), true},
		{mine, start("turn-3", "other"), false}, {mine, textEv("not ours"), false},
	} {
		if p.accepts(c.target, c.ev) != c.want {
			t.Fatalf("wrong progress for %+v", c)
		}
	}
}

func TestReviewRestoreKeepsCompleteAnswerOrFails(t *testing.T) {
	full := strings.Repeat("a", transcript.DefaultBounds().StreamText+1024)
	for _, seq := range []uint64{3, 5} {
		w := turnWaiter{opts: waitOptions{fullMessage: true}, target: waitTarget{kind: targetTurn, id: "turn-1"}, acc: newTurnAccumulator()}
		w.acc.full = true
		w.acc.seedRun(2, full, true, false)
		w.acc.seq = 3
		snap := transcript.Snapshot{Seq: seq, Main: transcript.TranscriptSnap{Entries: []transcript.Entry{{ID: transcript.EntryID{Seq: 2}, Kind: transcript.KindAssistant, Text: "…" + full[1024:], Cut: true}}}}
		w.followFrom(&snap, seq, true)
		res := &turnResult{Status: statusCompleted}
		res.fill(w.acc)
		finishReviewMessage(res)
		if seq == 3 && (res.Status != statusCompleted || res.message() != full) || seq == 5 && (res.Status != statusFailed || res.Error.Kind != errkind.Unobserved) {
			t.Fatal(seq, res.Status, res.Error)
		}
	}
}
