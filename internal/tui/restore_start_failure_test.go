package tui

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// A start failure and the first restore, in either order (plan 030 C7r).
//
// Over a socket, Init — or the launch's adoption (spawned) — runs the start
// and the stream's reader side by side, and a remote Start answers as soon as
// its stream has queued the host's ready{startFailed}, whether or not the
// reader has handed up the attach's restore queued before it. So the start's
// failure (errMsg) can be applied before the first restore, and the restore's
// rebuild (pane.rebuild) takes every local row with it — the failure's among
// them — while the model stays failed (X56). Plan 030 C7's starvation runs
// found it (about 1 in 25 at a 2% CPU quota): the tab title marked failed,
// exit 1, the error printed after the screen, and no row on the screen for
// the ≥ 13 s it stayed up. Each order here is forced: the host's start fails
// after the client attached "now" (the launch's attach, X35), and the rig
// hands the model the start's answer and the stream's items in the order
// written down, each read bounded on its own (runWatched).

// failingStart is a session with no primary whose Start fails with err: a
// host whose agent would not start.
type failingStart struct {
	*Stub
	err error
}

func (s *failingStart) Start(context.Context) error { return s.err }

// errAuthFailed is the start failure here, as the fake agent's authfail
// script has cursor-agent say it.
var errAuthFailed = errors.New("json-rpc error -32000: authentication failed (run `agent login`)")

// startFailedOverTheSocket is a rig over a TUI attached "now" to a host whose
// start then failed, with the start's answer and the attach's restore both
// in hand and neither applied. The host's ready{startFailed} is the stream's
// next item after the restore. The TUI's backend counts the acknowledgements
// of its start (ackCount), as the launch flow's backend hears them.
func startFailedOverTheSocket(t *testing.T) (*gateRig, errMsg, restoreMsg) {
	t.Helper()
	stub := NewStubNoPrimary()
	h := newAttachHostWith(t, &failingStart{Stub: stub, err: errAuthFailed}, stub, false, control.Options{Workspace: "/work"})
	ws := frameWorkspace(t)
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	s, err := remote.DialSession(ctx, h.path, remote.SessionOptions{
		Client:    remote.Options{Client: protocol.ClientInfo{Kind: "test", Name: "tui_test"}},
		Workspace: ws,
		When:      protocol.WhenNow,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// Attached before the host's start has run, so the attach reply — the
	// first restore — is queued ahead of the start's outcome.
	if err := s.Attach(ctx); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := h.eng.Start(context.Background()); !errors.Is(err, errAuthFailed) {
		t.Fatalf("fixture: the host's start answered %v, want %v", err, errAuthFailed)
	}
	isolateSkillsHome(t)
	tm, _ := New(Config{Backend: &ackCount{Backend: s}, Theme: "tokyo-night", Workspace: ws, Yolo: true}).Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	r := newGateRig(t, asyncGate(t, tm.(Model)))
	failed, ok := runWatched(t, r.m.startCmd()).(errMsg)
	if !ok || failed.err == nil || failed.err.Error() != errAuthFailed.Error() {
		t.Fatalf("fixture: the start answered %#v, want the host's start failure", failed)
	}
	restore, ok := r.nextStreamMsg().(restoreMsg)
	if !ok {
		t.Fatal("fixture: the stream's first item is not the attach's restore")
	}
	return r, failed, restore
}

// TestAStartFailureKeepsItsRowThroughTheFirstRestore (plan 030 C7r): the start
// failure's error row is on screen however the start's answer and the first
// restore land — the ordinary order (the restore first) and the one C7's
// starvation runs found (the failure first) paint the same frame: one error
// row with the host's error, the model failed with it, the tab title marked
// failed. The rest of the stream (the ready{startFailed}) and the read of the
// last ending the restore sent change none of it.
func TestAStartFailureKeepsItsRowThroughTheFirstRestore(t *testing.T) {
	want := []string{"local error:" + errAuthFailed.Error()}
	finish := func(t *testing.T, r *gateRig) string {
		t.Helper()
		ready, ok := r.nextStreamMsg().(readyMsg)
		if !ok || ready.err == nil {
			t.Fatalf("fixture: the stream's item after the restore is %#v, want the host's ready{startFailed}", ready)
		}
		r.send(ready)
		r.send(heldLastTurn(t, r))
		if got := mainRows(r.m); !slices.Equal(got, want) {
			t.Fatalf("the transcript holds %q, want the start failure's row %q", got, want)
		}
		if r.m.status != statusError || r.m.err != errAuthFailed.Error() || r.m.startErr == nil {
			t.Fatalf("the model is %s (%q, start error %v), want the start failure", r.m.status, r.m.err, r.m.startErr)
		}
		if mark := r.m.windowTitle(); !strings.HasPrefix(mark, titleMarkError) {
			t.Fatalf("the tab title %q is not marked failed", mark)
		}
		if n := acks(t, r); n != 0 {
			t.Fatalf("a session whose start failed was acknowledged %d times as come up", n)
		}
		v := plainView(r.m)
		if !strings.Contains(v, "authentication failed") {
			t.Fatalf("the frame does not show the start failure:\n%s", v)
		}
		return v
	}

	var ordinary string
	t.Run("the restore, then the failure", func(t *testing.T) {
		r, failed, restore := startFailedOverTheSocket(t)
		r.send(restore)
		if got := mainRows(r.m); len(got) != 0 {
			t.Fatalf("fixture: the first restore of a session that never started drew %q", got)
		}
		r.send(failed)
		ordinary = finish(t, r)
	})

	t.Run("the failure, then the restore", func(t *testing.T) {
		r, failed, restore := startFailedOverTheSocket(t)
		r.send(failed)
		if got := mainRows(r.m); !slices.Equal(got, want) {
			t.Fatalf("fixture: the start failure drew %q, want %q", got, want)
		}
		r.send(restore)
		if got := mainRows(r.m); !slices.Equal(got, want) {
			t.Fatalf("the first restore left %q, want the start failure's row %q", got, want)
		}
		v := finish(t, r)
		if ordinary != "" && v != ordinary {
			t.Fatalf("the frame differs from the ordinary order's:\n%s\nwant:\n%s", v, ordinary)
		}
	})

	// A later restore — a re-attach of the same incarnation, after a slow
	// consumer's reset — replaces the transcript and says so; the start
	// failure's row is drawn again below that note.
	t.Run("a later restore", func(t *testing.T) {
		r, failed, restore := startFailedOverTheSocket(t)
		r.send(restore)
		r.send(failed)
		finish(t, r)
		again := restore
		again.gen++
		r.landed()
		r.send(again)
		later := []string{"local note:" + reloadedNote, want[0]}
		if got := mainRows(r.m); !slices.Equal(got, later) {
			t.Fatalf("the later restore left %q, want %q", got, later)
		}
		if r.m.status != statusError || r.m.startErr == nil {
			t.Fatalf("the later restore left the model %s (start error %v), want the start failure", r.m.status, r.m.startErr)
		}
	})
}

// acks is how many times the rig's model has told its backend that its
// session came up (startAcker).
func acks(t *testing.T, r *gateRig) int32 {
	t.Helper()
	b, ok := r.m.eng.(*ackCount)
	if !ok {
		t.Fatalf("the model's backend is %T, not the rig's", r.m.eng)
	}
	return b.acks.Load()
}

// TestAStartFailureIsItsIncarnations (plan 030 C7r2): a start failure is the
// failure of the incarnation it happened in. A restore of another — the host
// restarted, the engine replaced — is another session, and takes it away: no
// error row is drawn again, the error state goes, no failure is left for
// craze's exit, and the session the model now holds comes up, as the start
// has answered all it will (acknowledged, as a launch's would be). Over a
// socket the start's answer is read beside the stream, so it lands anywhere
// around the stream's items — A's restore, A's failed Ready, then B's restore
// (by hand: another incarnation's) — each place forced here; the last is a
// reply late for the incarnation it answered for, which fails nothing. Every
// order ends in the same model and paints the same frame.
func TestAStartFailureIsItsIncarnations(t *testing.T) {
	failure := []string{"local error:" + errAuthFailed.Error()}
	var frame string
	for _, tc := range []struct {
		name string
		// at is how many of the stream's items land before the start's
		// answer.
		at int
	}{
		{"the failure, A's restore, its Ready, B's restore", 0},
		{"A's restore, the failure, its Ready, B's restore", 1},
		{"A's restore, its Ready, the failure, B's restore", 2},
		{"A's restore, its Ready, B's restore, the late failure", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, failed, restore := startFailedOverTheSocket(t)
			stream := []func(){
				func() { r.send(restore) },
				func() {
					ready, ok := r.nextStreamMsg().(readyMsg)
					if !ok || ready.err == nil {
						t.Fatalf("fixture: the stream's item after the restore is %#v, want the host's ready{startFailed}", ready)
					}
					r.send(ready)
					if tc.at >= 2 && (r.m.startErr != nil || len(mainRows(r.m)) != 0) {
						t.Fatalf("fixture: the Ready alone applied a start failure (%v, %q)", r.m.startErr, mainRows(r.m))
					}
				},
				func() {
					if tc.at <= 2 {
						// Before the move, A's failure is on screen.
						if got := mainRows(r.m); !slices.Equal(got, failure) || r.m.status != statusError || r.m.startErr == nil {
							t.Fatalf("fixture: before the move the model is %s (start error %v) with %q, want the start failure", r.m.status, r.m.startErr, got)
						}
					}
					// The stream's next item, as another incarnation's
					// re-attach hands it up: its read is spent, never run.
					r.landed()
					r.send(restoreOf(r.m, snapshotFolded(t, "inc-b"), restore.gen+1))
					if r.m.shared.Incarnation() != "inc-b" {
						t.Fatalf("fixture: the model holds incarnation %q after B's restore", r.m.shared.Incarnation())
					}
				},
			}
			for i, land := range stream {
				if i == tc.at {
					r.send(failed)
				}
				land()
			}
			if tc.at == len(stream) {
				r.send(failed)
			}
			// The reads of the last ending both restores sent: A's is
			// another restore's, and B's host names no ending.
			for len(r.lastTurns) > 0 {
				r.send(heldLastTurn(t, r))
			}

			want := []string{"local note:" + reloadedNote}
			if got := mainRows(r.m); !slices.Equal(got, want) {
				t.Fatalf("B's transcript holds %q, want only the restore's note %q", got, want)
			}
			if r.m.startErr != nil || r.m.status != statusIdle || r.m.err != "" {
				t.Fatalf("on B the model is %s (%q, start error %v), want idle with no start failure", r.m.status, r.m.err, r.m.startErr)
			}
			if !r.m.sessionReady() {
				t.Fatal("B did not come up: the start has answered, and B's restore says nothing is loading")
			}
			if n := acks(t, r); n != 1 {
				t.Fatalf("B's coming up was acknowledged %d times, want once", n)
			}
			if mark := r.m.windowTitle(); strings.HasPrefix(mark, titleMarkError) {
				t.Fatalf("the tab title %q is marked failed on B", mark)
			}
			v := plainView(r.m)
			if strings.Contains(v, "authentication failed") {
				t.Fatalf("B's frame shows A's start failure:\n%s", v)
			}
			if frame == "" {
				frame = v
			} else if v != frame {
				t.Fatalf("the frame differs by order:\n%s\nwant:\n%s", v, frame)
			}
			// The quit: A's failure is not B's exit status.
			if failedRun, err := finishRun(io.Discard, r.m, r.m, nil); failedRun || err != nil {
				t.Fatalf("the quit on B reports the run failed %v, with %v", failedRun, err)
			}
		})
	}
}
