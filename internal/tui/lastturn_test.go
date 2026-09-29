package tui

import (
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
)

// The last turn's ending after a restore (plan 030 §3.7, SF-57 in part,
// R2-7; lastturn.go). Every schedule is forced: the read's answer is a
// command the rig holds (gateRig.lastTurns) and runs exactly where the
// schedule says — before or after a turn the host runs, another restore, a
// backend that moved — so each ordering is the one written down, never one
// left to the scheduler. Every wait is bounded on its own (runWatched,
// waitHost).

// errTurnFailed is the failed turn's error in these tests.
var errTurnFailed = errors.New("the turn failed")

// hostWithAFailedTurn is a started host, served with o, over a scripted
// session with no primary, whose one turn so far — "go" — failed
// (errTurnFailed) before any TUI attached: its last ending is failed.
func hostWithAFailedTurn(t *testing.T, o control.Options) (*attachHost, *scriptedSession) {
	t.Helper()
	sess := &scriptedSession{Stub: NewStubNoPrimary(), cancels: make(chan struct{})}
	h := newAttachHostWith(t, sess, sess.Stub, true, o)
	sess.Script(scriptFailed(errTurnFailed))
	hostTurn(t, h, "go", engine.TurnFailed)
	return h, sess
}

// hostTurn runs one turn on h's engine, sent by a client of the host's own
// (another TUI, as far as the model is concerned), and waits for its ending
// to be the session's last, with outcome.
func hostTurn(t *testing.T, h *attachHost, text string, outcome engine.TurnOutcome) *engine.LastTurn {
	t.Helper()
	before := h.eng.State().LastTurn
	c := engine.Command{Client: h.eng.NewClientID(), ID: "1"}
	if _, err := h.eng.Submit(c, text, engine.SubmitQueue, ""); err != nil {
		t.Fatalf("the host's turn %q: %v", text, err)
	}
	var lt *engine.LastTurn
	waitHost(t, "the ending of "+text, func() bool {
		lt = h.eng.State().LastTurn
		return lt != nil && lt != before && (before == nil || lt.TurnID != before.TurnID)
	})
	if lt.Outcome != outcome {
		t.Fatalf("the host's turn %q ended %s (%q), want %s", text, lt.Outcome, lt.Err, outcome)
	}
	return lt
}

// attachedAfter is upOverTheSocket for a TUI attached to h — its session up,
// the attach's restore applied, and the read that restore sent held.
func attachedAfter(t *testing.T, h *attachHost, b func(backend.Backend) backend.Backend) *gateRig {
	t.Helper()
	ws := frameWorkspace(t)
	var be backend.Backend = dialHost(t, h, ws)
	if b != nil {
		be = b(be)
	}
	return upOverTheSocket(t, Config{Backend: be, Theme: "tokyo-night", Workspace: ws, Yolo: true}, nil)
}

// heldLastTurn runs the rig's oldest held read of the last ending, now, and
// hands back its answer, unapplied.
func heldLastTurn(t *testing.T, r *gateRig) lastTurnMsg {
	t.Helper()
	if len(r.lastTurns) == 0 {
		t.Fatal("no read of the last ending is held")
	}
	cmd := r.lastTurns[0]
	r.lastTurns = r.lastTurns[1:]
	msg, ok := runWatched(t, cmd).(lastTurnMsg)
	if !ok {
		t.Fatal("the read answered something that is not its answer")
	}
	return msg
}

// readUntilTurnEnded lands the rig's reads until the model has folded the
// ending of a turn — every read bounded by runWatched.
func readUntilTurnEnded(t *testing.T, r *gateRig) {
	t.Helper()
	for range 256 {
		ev := r.read()
		if ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.Phase == agent.TurnEnded {
			return
		}
	}
	t.Fatal("no turn's ending in 256 events")
}

// TestAFailedTurnShowsFailedAfterARestore (plan 030 §3.7, AC7): a turn that
// failed while no client watched — its terminal closed, its connection lost
// — restores as the failure it was: the transcript's error row is the
// snapshot's, and the read after the restore puts the model in the error
// state a live failure leaves (the failed status the tab title marks and a
// roost or herdr host reports), with the ending's error, drawing nothing.
func TestAFailedTurnShowsFailedAfterARestore(t *testing.T) {
	h, _ := hostWithAFailedTurn(t, control.Options{Workspace: "/work"})
	r := attachedAfter(t, h, nil)
	if r.m.status != statusIdle {
		t.Fatalf("fixture: the restore left the model %s; the snapshot alone keeps no ending", r.m.status)
	}
	rows := mainRows(r.m)
	if !slices.Contains(rows, "error:"+errTurnFailed.Error()) {
		t.Fatalf("the restored transcript has no error row: %q", rows)
	}
	msg := heldLastTurn(t, r)
	if msg.last == nil || msg.last.Outcome != engine.TurnFailed {
		t.Fatalf("the read answered %+v (%v), want the failed ending", msg.last, msg.err)
	}
	r.send(msg)
	if r.m.status != statusError || r.m.err != errTurnFailed.Error() {
		t.Fatalf("after the read the model is %s (%q), want the failure %q", r.m.status, r.m.err, errTurnFailed)
	}
	if got := mainRows(r.m); !slices.Equal(got, rows) {
		t.Fatalf("the read drew rows: %q, want %q", got, rows)
	}
	if mark := r.m.windowTitle(); mark[:len(titleMarkError)] != titleMarkError {
		t.Fatalf("the tab title %q is not marked failed", mark)
	}
}

// TestALaterEndingTakesTheFailureAway (§3.7): a model showing a failure a
// restore kept — one of the same incarnation keeps the status it found —
// while another turn ran and ended well on the host, unseen, reads that
// ending after the restore, and the failure is no longer the session's.
func TestALaterEndingTakesTheFailureAway(t *testing.T) {
	h, _ := hostWithAFailedTurn(t, control.Options{Workspace: "/work"})
	r := attachedAfter(t, h, nil)
	first := r.m.restores
	r.send(heldLastTurn(t, r))
	if r.m.status != statusError {
		t.Fatalf("fixture: the model is %s, want the failure shown", r.m.status)
	}
	// Unseen: the model reads none of the turn's events.
	hostTurn(t, h, "again", engine.TurnDone)
	snap, err := h.eng.TranscriptSnapshot("", matchSnapshotBytes)
	if err != nil {
		t.Fatal(err)
	}
	// Handed in as the stream's next item: the outstanding read lands with it.
	r.landed()
	r.send(restoreOf(r.m, snap, 1<<20))
	if r.m.restores != first+1 || r.m.status != statusError {
		t.Fatalf("fixture: after a same-incarnation restore the model is %s (restores %d)", r.m.status, r.m.restores)
	}
	r.send(heldLastTurn(t, r))
	if r.m.status != statusIdle || r.m.err != "" {
		t.Fatalf("after the done ending was read the model is %s (%q), want idle", r.m.status, r.m.err)
	}
}

// TestADelayedLastTurnReadIsDroppedWhenTheSessionMovedOn (§3.7, R2-7): the
// read's answer is applied only while what it was sent under stands — the
// backend's epoch, the restore it follows, and the turns the model had seen
// begin — so a delayed answer never puts back an ending the session has moved
// past. Each schedule holds the answer while the session moves, then lands
// it: dropped, the model as the move left it. The first is the control: the
// same answer, nothing moved, applied.
func TestADelayedLastTurnReadIsDroppedWhenTheSessionMovedOn(t *testing.T) {
	t.Run("nothing moved: applied", func(t *testing.T) {
		h, _ := hostWithAFailedTurn(t, control.Options{Workspace: "/work"})
		r := attachedAfter(t, h, nil)
		msg := heldLastTurn(t, r)
		r.send(msg)
		if r.m.status != statusError {
			t.Fatalf("the model is %s, want the failure", r.m.status)
		}
	})

	t.Run("a newer turn folded since the restore", func(t *testing.T) {
		h, _ := hostWithAFailedTurn(t, control.Options{Workspace: "/work"})
		r := attachedAfter(t, h, nil)
		// Answered before the next turn begins: the failure, then current.
		msg := heldLastTurn(t, r)
		if msg.last == nil || msg.last.Outcome != engine.TurnFailed {
			t.Fatalf("fixture: the read answered %+v", msg.last)
		}
		hostTurn(t, h, "again", engine.TurnDone)
		readUntilTurnEnded(t, r)
		if r.m.status != statusIdle {
			t.Fatalf("fixture: after the new turn the model is %s", r.m.status)
		}
		r.send(msg)
		if r.m.status != statusIdle || r.m.err != "" {
			t.Fatalf("the old failure was put back over the newer turn: %s (%q)", r.m.status, r.m.err)
		}
	})

	t.Run("another restore since", func(t *testing.T) {
		h, _ := hostWithAFailedTurn(t, control.Options{Workspace: "/work"})
		r := attachedAfter(t, h, nil)
		msg := heldLastTurn(t, r)
		snap, err := h.eng.TranscriptSnapshot("", matchSnapshotBytes)
		if err != nil {
			t.Fatal(err)
		}
		// Handed in as the stream's next item: the outstanding read lands with it.
		r.landed()
		r.send(restoreOf(r.m, snap, 1<<20))
		if len(r.lastTurns) != 1 {
			t.Fatalf("the second restore sent %d reads, want its own one", len(r.lastTurns))
		}
		r.send(msg)
		if r.m.status != statusIdle {
			t.Fatalf("the first restore's answer applied after a second restore: %s", r.m.status)
		}
		// The second restore's own read is current, and applies.
		r.send(heldLastTurn(t, r))
		if r.m.status != statusError {
			t.Fatalf("the second restore's own read did not apply: %s", r.m.status)
		}
	})

	t.Run("the backend's epoch moved", func(t *testing.T) {
		h, _ := hostWithAFailedTurn(t, control.Options{Workspace: "/work"})
		var moved *movableEpoch
		r := attachedAfter(t, h, func(b backend.Backend) backend.Backend {
			moved = &movableEpoch{Backend: b}
			return moved
		})
		msg := heldLastTurn(t, r)
		if msg.last == nil {
			t.Fatalf("fixture: the read answered nothing (%v)", msg.err)
		}
		// A reconnect that could not resume: the backend is bound to another
		// session identity before any restore of it lands.
		moved.by.Add(1)
		r.send(msg)
		if r.m.status != statusIdle {
			t.Fatalf("an answer from the epoch the backend left applied: %s", r.m.status)
		}
	})

	t.Run("the backend replaced", func(t *testing.T) {
		h, _ := hostWithAFailedTurn(t, control.Options{Workspace: "/work"})
		r := attachedAfter(t, h, nil)
		msg := heldLastTurn(t, r)
		r.m.setBackend(dialHost(t, h, "/work"))
		r.send(msg)
		if r.m.status != statusIdle {
			t.Fatalf("an answer about the backend the model left applied: %s", r.m.status)
		}
	})
}

// movableEpoch is a backend whose Epoch a test moves past its own, standing
// in for a socket's reconnect to another identity (backend.Backend.Epoch).
type movableEpoch struct {
	backend.Backend
	by atomic.Uint64
}

func (b *movableEpoch) Epoch() uint64 { return b.Backend.Epoch() + b.by.Load() }

// TestARestoreShowsTheFailedLastTurn is the golden of a restore whose
// session's last turn failed (plan 030 §3.7): a TUI attached, over the
// socket, to a host whose one turn failed before anyone watched. What it
// shows is what a live failure shows — the prompt and its error row, no
// spinner — held byte for byte against the same session failing live in
// process; the error state behind it, which no frame draws (the tab title
// and a roost or herdr host do), is TestAFailedTurnShowsFailedAfterARestore's.
//
// The frame is the runner's own transport's (a direct production): the frame
// harness's socket host attaches its model before its engine starts, so no
// turn can end before the attach there; this test's host is its own, served
// on its own socket, and its model's backend is that socket
// (golden_manifest_test.go lists it so).
func TestARestoreShowsTheFailedLastTurn(t *testing.T) {
	isolateSkillsHome(t)
	ws := frameWorkspace(t)
	opts := FrameOpts{Timeout: 10 * time.Second, Freeze: true}

	sess := &scriptedSession{Stub: NewStub(), cancels: make(chan struct{})}
	sess.Script(scriptFailed(errTurnFailed))
	live, _, err := RunFrameScript(Config{Session: sess, Theme: "tokyo-night", Workspace: ws, Model: "grok", Yolo: true},
		80, 24, "<wait:idle>go<enter><wait:text:"+errTurnFailed.Error()+">", opts)
	if err != nil {
		t.Fatalf("the live failure: %v", err)
	}

	h, _ := hostWithAFailedTurn(t, control.Options{Workspace: ws, PermissionMode: framePermissionMode(true), StartedAt: time.Now()})
	be := engineOnly{Backend: dialHost(t, h, ws), eng: h.eng}
	got, _, err := RunFrameScript(Config{Backend: be, Theme: "tokyo-night", Workspace: ws, Model: "grok", Yolo: true},
		80, 24, "<wait:text:"+errTurnFailed.Error()+">", opts)
	if err != nil {
		t.Fatalf("the restore: %v", err)
	}
	if got != live {
		t.Fatalf("the restored frame is not the live failure's (%s)\n--- live ---\n%s\n--- restored ---\n%s", frameLineDiff(live, got), live, got)
	}
	assertGolden(t, "restore-failed-80x24", 80, 24, got)
}
