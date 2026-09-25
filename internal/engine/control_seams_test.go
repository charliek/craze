package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/journal"
)

// The three things the socket server needs of Control that no in-process
// client did (plan 027 §3.6, §3.7): the barrier's seq, readiness, and a way into
// the journal.

// readyNow reports whether e's Ready has closed, without waiting.
func readyNow(e *Engine) bool {
	select {
	case <-e.Ready():
		return true
	default:
		return false
	}
}

// newUnstarted is an engine over s that nothing has started, closed when the
// test ends.
func newUnstarted(t *testing.T, s *fakeSession) *Engine {
	t.Helper()
	e, err := New(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// TestReadyClosesOnceTheStartHasRunOrTheEngineCloses: Ready is open until the
// start has run — whatever it came to — or the engine has closed, whichever is
// first; a waiter woken by it reads the start's outcome in State; and nothing
// that follows, a second Started, a Started after Close or a Close after
// Started, closes it twice.
func TestReadyClosesOnceTheStartHasRunOrTheEngineCloses(t *testing.T) {
	// woken reads State the moment Ready closes, on a goroutine of its own, as
	// an attach waiting for readiness does.
	woken := func(e *Engine) <-chan State {
		out := make(chan State, 1)
		go func() {
			<-e.Ready()
			out <- e.State()
		}()
		return out
	}
	stateWithin := func(t *testing.T, ch <-chan State) State {
		t.Helper()
		select {
		case st := <-ch:
			return st
		case <-time.After(watchdog):
			t.Fatalf("Ready did not close within %s", watchdog)
			panic("unreachable")
		}
	}

	t.Run("a start that succeeded", func(t *testing.T) {
		e := newUnstarted(t, newFake(t, agent.EventLogOptions{NoPrimary: true}))
		st := woken(e)
		if readyNow(e) {
			t.Fatal("Ready is closed before the start has run")
		}
		if err := e.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := stateWithin(t, st); got.Activity != ActivityIdle {
			t.Fatalf("the waiter woke to activity %s, want idle", got.Activity)
		}
	})

	t.Run("a start that failed", func(t *testing.T) {
		s := newFake(t, agent.EventLogOptions{NoPrimary: true})
		boom := errors.New("the agent would not start")
		s.startErr = boom
		e := newUnstarted(t, s)
		st := woken(e)
		if readyNow(e) {
			t.Fatal("Ready is closed before the start has run")
		}
		if err := e.Start(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("start: %v", err)
		}
		if got := stateWithin(t, st); !got.StartFailed || got.Activity != ActivityError {
			t.Fatalf("the waiter woke to %+v, want the failed start", got)
		}
	})

	t.Run("an engine closed before it started", func(t *testing.T) {
		e := newUnstarted(t, newFake(t, agent.EventLogOptions{NoPrimary: true}))
		st := woken(e)
		if readyNow(e) {
			t.Fatal("Ready is closed before the start has run")
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		if got := stateWithin(t, st); got.Activity == ActivityStarting || got.Activity == ActivityIdle {
			t.Fatalf("the waiter woke to activity %s on a closed engine", got.Activity)
		}
		// The start the closed engine never had arrives after all: the gate stays
		// shut, and Ready is not closed a second time.
		e.Started(nil)
		if got := e.State(); got.Activity == ActivityIdle {
			t.Fatal("a Started after Close opened the gate")
		}
	})

	t.Run("a second Started and a Close after it", func(t *testing.T) {
		e := newUnstarted(t, newFake(t, agent.EventLogOptions{NoPrimary: true}))
		// The TUI's order: Start on a command's goroutine, then Started again from
		// the model that heard it.
		if err := e.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		e.Started(nil)
		e.Started(errors.New("a later failure changes nothing"))
		if !readyNow(e) {
			t.Fatal("Ready is open after the start")
		}
		if got := e.State(); got.Activity != ActivityIdle || got.StartFailed {
			t.Fatalf("a second Started moved the engine to %+v", got)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		if !readyNow(e) {
			t.Fatal("Ready is open after Close")
		}
	})
}

// TestSyncSeqCoversEveryEventACommandCaused is the reply barrier at the
// engine (plan 027 §3.6): several clients each queue a row behind a running turn
// and then call SyncSeq, as a handler does before it replies, and each answer is
// at or past the seq its own row's queued event was committed with. On a closed
// engine it is ErrLogClosing with seq 0.
func TestSyncSeqCoversEveryEventACommandCaused(t *testing.T) {
	const clients = 6
	r := newRig(t, Options{})
	turn := r.s.script(held())
	r.submit("one")
	await(t, turn.opened, "the turn to open")

	answers := make([]uint64, clients)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			if _, err := r.e.Queue(Command{}, fmt.Sprintf("row %d", i)); err != nil {
				t.Errorf("client %d: queue: %v", i, err)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), watchdog)
			defer cancel()
			seq, err := r.e.SyncSeq(ctx)
			if err != nil {
				t.Errorf("client %d: SyncSeq: %v", i, err)
				return
			}
			answers[i] = seq
		})
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	queued := map[string]uint64{}
	for len(queued) < clients {
		if ev := r.next(); ev.Type == agent.EventQueue && ev.QueueChange == agent.QueueQueued {
			queued[ev.Queue.Text] = ev.Seq
		}
	}
	for i, answer := range answers {
		text := fmt.Sprintf("row %d", i)
		seq, ok := queued[text]
		if !ok {
			t.Fatalf("%q was never queued: %v", text, queued)
		}
		if seq > answer {
			t.Fatalf("%q was committed as seq %d, past the %d SyncSeq answered its client", text, seq, answer)
		}
	}

	turn.release()
	if err := r.e.Close(); err != nil {
		t.Fatal(err)
	}
	if seq, err := r.e.SyncSeq(context.Background()); !errors.Is(err, agent.ErrLogClosing) || seq != 0 {
		t.Fatalf("SyncSeq on a closed engine: (%d, %v), want (0, ErrLogClosing)", seq, err)
	}
}

// TestNoteReachesTheSessionsJournal: Control.Note is the log's Note, so the
// server's connection diags land in the session's own journal ahead of its
// closing diag, and one written after Close is counted and dropped like any
// other late note.
func TestNoteReachesTheSessionsJournal(t *testing.T) {
	w, lo := testJournal(t, nil)
	r := newRigOn(t, Options{}, lo)
	r.e.Note(journal.DiagNote{Kind: "control_conn", Fields: map[string]any{"event": "open", "pid": 42}})
	if err := r.e.Close(); err != nil {
		t.Fatal(err)
	}
	r.e.Note(journal.DiagNote{Kind: "control_conn", Fields: map[string]any{"event": "late"}})
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	if err := w.WaitFlushed(ctx, journal.MaxSeq); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("waiting for the journal writer to finish: %v", err)
	}

	raw, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	var notes []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("journal line %q: %v", line, err)
		}
		if obj["type"] == "diag" && obj["kind"] == "control_conn" {
			notes = append(notes, obj)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("%d control_conn notes, want the one written before Close:\n%s", len(notes), raw)
	}
	if fields, _ := notes[0]["fields"].(map[string]any); fields["event"] != "open" {
		t.Fatalf("the note carries %v", notes[0]["fields"])
	}
	if n := r.s.log.Health().NotesDropped; n != 1 {
		t.Fatalf("%d notes dropped, want the one written after Close", n)
	}
}
