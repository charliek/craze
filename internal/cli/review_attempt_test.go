package cli

import (
	"context"
	"testing"
	"time"

	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/routing"
	"github.com/charliek/craze/internal/rundir"
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
