package engine_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/transcript"
	"github.com/charliek/craze/internal/tui"
)

// A2 over the wire (plan 027 §3.16, A1's automated half; PR 4, C29): Plan
// 024's A2 — a client that attaches mid-session from a snapshot reproduces the
// first client, at a common Seq — run through a real server (internal/control)
// and a remote.Session, the TUI's backend over the socket. Both sides are held
// here, in process: the clients' folds, the first client's and the engine's
// own model (no wire digest, astra r3 21).

// wireBudget is the matrix's subscription budget: the server's MaxBudget and
// what every client asks for, so no client is reset slow_consumer while the
// trace is published faster than it reads.
var wireBudget = control.Budget{MaxItems: 1 << 20, MaxBytes: 1 << 30}

// serveWire serves e on a short socket path under /tmp itself (t.TempDir()
// and $TMPDIR overflow sun_path on macOS) until the test ends, and answers
// the path.
func serveWire(t *testing.T, e *engine.Engine) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "czx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	srv := control.New(control.Options{Workspace: "/work", MaxBudget: wireBudget})
	srv.SetEngine(e)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), watchdog)
		defer cancel()
		if err := srv.Close(ctx); err != nil {
			t.Errorf("server close: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return path
}

// wireClient is a client of the wire as the TUI is one (remote.Session): it
// restores every Restore its stream hands up and folds every event after it,
// on a goroutine of its own, until its session closes.
type wireClient struct {
	s *remote.Session
	// restoredAt is the Seq of each snapshot it restored; folded counts the
	// events it folded after the last one.
	mu         sync.Mutex
	model      *transcript.Model
	restoredAt []uint64
	folded     int
	err        error
	moved      chan struct{}
	done       chan struct{}
}

// dialWire attaches a new client to the host at path — a snapshot attach,
// once the session is ready — and starts its fold.
func dialWire(t *testing.T, ctx context.Context, path string) *wireClient {
	t.Helper()
	s, err := remote.DialSession(ctx, path, remote.SessionOptions{
		Client: remote.Options{Client: protocol.ClientInfo{Kind: "test", Name: "exactness"}, StreamBytes: 1 << 30},
		Budget: &protocol.AttachBudget{MaxItems: wireBudget.MaxItems, MaxBytes: wireBudget.MaxBytes, SnapshotBytes: protocol.SnapshotBytesMax},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := s.Attach(ctx); err != nil {
		_ = s.Close()
		t.Fatalf("attach: %v", err)
	}
	c := &wireClient{s: s, moved: make(chan struct{}), done: make(chan struct{})}
	go c.run()
	t.Cleanup(c.close)
	return c
}

func (c *wireClient) run() {
	defer close(c.done)
	for {
		it, err := c.s.Read(context.Background())
		c.mu.Lock()
		switch {
		case err != nil:
			c.err = err
		case it.Kind == backend.ItemRestore:
			c.model = transcript.Restore(it.Snapshot, transcript.Options{})
			c.restoredAt = append(c.restoredAt, it.Snapshot.Seq)
			c.folded = 0
		case it.Kind == backend.ItemEvent && c.model != nil:
			c.model.Fold(it.Event)
			c.folded++
		case it.Kind == backend.ItemEnd:
			c.err = fmt.Errorf("the stream ended: %v", it.Err)
		}
		close(c.moved)
		c.moved = make(chan struct{})
		stop := c.err != nil
		c.mu.Unlock()
		if stop {
			return
		}
	}
}

// foldThrough waits until the client has folded seq, and fails if it has
// folded past it: the comparisons are at a common Seq.
func (c *wireClient) foldThrough(t *testing.T, seq uint64) {
	t.Helper()
	deadline := time.After(watchdog)
	for {
		c.mu.Lock()
		at, err, moved := uint64(0), c.err, c.moved
		if c.model != nil {
			at = c.model.Seq()
		}
		c.mu.Unlock()
		switch {
		case at > seq:
			t.Fatalf("the client is at %d, past the common seq %d", at, seq)
		case at == seq:
			return
		case err != nil:
			t.Fatalf("the client stopped at %d, waiting for %d: %v", at, seq, err)
		}
		select {
		case <-moved:
		case <-deadline:
			t.Fatalf("the client is at %d, waiting for %d", at, seq)
		}
	}
}

func (c *wireClient) close() {
	_ = c.s.Close()
	<-c.done
}

// TestAttachMidTurnOverTheSocketReproducesTheFirst (A2 over the wire, A1's
// automated half):
//
//   - Every cut of a recorded Stub trace (recordStubSession's: every kind of
//     event, asks, sub-agents, the queue, the settings, a send-now, a foreign
//     turn, a replay bracket), republished into a host one event at a time: a
//     client attaches at every cut k — its first item the snapshot at k, then
//     the live suffix — and every one folds to the first client's model (the
//     one attached at cut 0, which folded every event from the wire) at the
//     trace's end, which is itself the in-process fold of the trace and the
//     host engine's own model. The trace's one event over the log's record
//     bound is cut to fit: a subscriber is handed it Omitted, which the wire
//     answers with reset{omitted} and a fresh snapshot — the reset path, not
//     the cut this test is about (A2 in process and the remote's own tests
//     hold that one).
//   - Eight cuts over the fake agent, as TestAttachOverTheFakeAgentReproducesTheFirst
//     makes them in process: each attach mid-turn — the fixture held at its
//     gate until the attach has returned — and the client's fold compared
//     with the primary reader's at the turn's settled cutoff.
func TestAttachMidTurnOverTheSocketReproducesTheFirst(t *testing.T) {
	t.Run("every cut of a recorded Stub trace", func(t *testing.T) {
		trace, _, oversized, _ := recordStubSession(t)
		if oversized == 0 {
			t.Fatal("fixture: the trace has no oversized event")
		}
		trace[oversized-1].Text = "an oversized reply, cut to fit a record on the wire"

		ctx, cancel := context.WithTimeout(context.Background(), 4*watchdog)
		defer cancel()
		stub := tui.NewStubNoPrimary()
		stub.Clock = traceClock()
		e, err := engine.New(stub, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		if err := e.Start(ctx); err != nil {
			t.Fatal(err)
		}
		path := serveWire(t, e)

		start := time.Now()
		clients := make([]*wireClient, 0, len(trace)+1)
		for k := 0; k <= len(trace); k++ {
			if k > 0 {
				ev := trace[k-1]
				ev.Seq = 0
				if !stub.EventLog().Publish(ctx, nil, ev) {
					t.Fatalf("cut %d: the host did not publish the trace's event", k)
				}
			}
			// The host holds exactly the trace's first k events: nothing
			// of its own is published between them.
			if head, err := e.SyncSeq(ctx); err != nil || head != uint64(k) {
				t.Fatalf("cut %d: the host's head is %d (%v)", k, head, err)
			}
			clients = append(clients, dialWire(t, ctx, path))
		}
		n := uint64(len(trace))
		full := transcript.New(transcript.Options{})
		for _, ev := range trace {
			full.Fold(ev)
		}
		a, err := e.Attach(ctx, engine.AttachOptions{SnapshotBytes: 1 << 30})
		if err != nil {
			t.Fatal(err)
		}
		a.Sub.Close()
		if a.Snapshot.Seq != n {
			t.Fatalf("the host's model is at %d, the trace %d", a.Snapshot.Seq, n)
		}
		if d := engine.DiffModels(full, transcript.Restore(a.Snapshot, transcript.Options{}), true); d != "" {
			t.Fatalf("the trace folded whole is not the host's own model: %s", d)
		}

		first := clients[0]
		first.foldThrough(t, n)
		if d := engine.DiffModels(full, first.model, true); d != "" {
			t.Fatalf("the first client, folding every event from the wire, is not the trace folded whole: %s", d)
		}
		for k, c := range clients {
			c.foldThrough(t, n)
			c.mu.Lock()
			restored, folded, model := c.restoredAt, c.folded, c.model
			c.mu.Unlock()
			if len(restored) != 1 || restored[0] != uint64(k) {
				t.Fatalf("cut %d: the client restored snapshots at %v, want one at %d", k, restored, k)
			}
			if folded != len(trace)-k {
				t.Fatalf("cut %d: the client folded %d events after its snapshot, want the %d of the suffix", k, folded, len(trace)-k)
			}
			if d := engine.DiffModels(first.model, model, true); d != "" {
				t.Fatalf("cut %d (attached at %d, compared at %d): %s", k, restored[0], n, d)
			}
		}
		t.Logf("%d events, %d cuts over the wire in %v", len(trace), len(clients), time.Since(start).Round(time.Millisecond))
	})

	t.Run("eight cuts over the fake agent", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		ctx, cancel := context.WithTimeout(context.Background(), 4*watchdog)
		defer cancel()
		release := fakeAgentGate(t, ctx)
		sess := agent.New(agent.Options{
			Binary: fakeAgentBin(t), ExtraArgs: []string{"-script=tasks"},
			Workspace: t.TempDir(), Stderr: io.Discard,
		})
		e, err := engine.New(sess, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		first := readPrimary(t, e, true)
		if err := e.Start(ctx); err != nil {
			t.Fatal(err)
		}
		path := serveWire(t, e)
		from := 0
		for cut := range 8 {
			res, err := e.Submit(engine.Command{}, fmt.Sprintf("prompt %d", cut), engine.SubmitQueue, "")
			if err != nil || res.Turn == "" {
				t.Fatalf("cut %d: submit: %+v, %v", cut, res, err)
			}
			started := first.waitFor(t, from, startedTurn(res.Turn))
			at := first.waitFor(t, from, func(ev agent.Event) bool { return ev.Type == agent.EventTool })

			// The fixture is held after this tool until the attach has
			// returned.
			second := dialWire(t, ctx, path)
			release()
			ended := first.waitFor(t, at, endedTurn(res.Turn))
			from = ended + 1
			n := cutoff(t, ctx, e)
			first.waitSeq(t, n)
			second.foldThrough(t, n)

			second.mu.Lock()
			restored, folded, model := second.restoredAt, second.folded, second.model
			second.mu.Unlock()
			startedSeq, endedSeq := first.seqAt(started), first.seqAt(ended)
			if len(restored) != 1 || restored[0] < startedSeq || restored[0] >= endedSeq {
				t.Fatalf("cut %d: the client restored at %v, want one snapshot mid-turn: at or after the turn's start (%d), before its ending (%d)",
					cut, restored, startedSeq, endedSeq)
			}
			if folded < 1 {
				t.Fatalf("cut %d: the client folded nothing after its snapshot at %d", cut, restored[0])
			}
			first.mu.Lock()
			d := engine.DiffModels(first.model, model, true)
			first.mu.Unlock()
			if d != "" {
				t.Fatalf("cut %d (attached at %d, compared at %d): %s", cut, restored[0], n, d)
			}
			t.Logf("cut %d: attached at seq %d over the wire, compared at %d, the client folded %d events", cut, restored[0], n, folded)
			second.close()
		}
	})
}
