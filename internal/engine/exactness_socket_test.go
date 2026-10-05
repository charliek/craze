package engine_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
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

// stepCtx bounds a single blocking step to budget, fresh off parent: a real
// hang at that step still fails within budget regardless of how many steps a
// loop around it has already run, and regardless of parent's own deadline
// (which a whole loop must not share — that budget shrinks with every step
// and times out steps that are each individually healthy).
func stepCtx(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, budget)
}

// dialAttachBudget bounds dialWire's own dial and attach: a hang bound,
// with room for a starved run, since every cut's dial+attach is sub-second
// uncontended. Neither of the trace's two large events is this test's
// business. Its one over-8-MiB event is replaced with a short string before
// replay, and the test asserts exactly one restore at each client's own cut
// — an omitted reset would fail that assertion. Its edit report's eight
// 64 KiB old/new diff pairs are cut to a byte a side (SF-133): that ~1 MiB
// event, fanned out to the 118 clients already attached under -race, cost
// the next cut's dial+attach minutes at a 5% CPU quota. The large payload
// is held in process by A2 (exactness_test.go's
// TestASecondSubscriberAttachedMidTurnReproducesTheFirst, every cut of the
// same trace, its edit report whole) and over the wire by
// TestAFullSizeEditReportReachesEachSocketClientWhole (three clients around
// that report, whole).
const dialAttachBudget = 12 * watchdog

// dialWire attaches a new client to the host at path — a snapshot attach,
// once the session is ready — and starts its fold. Each of the dial and the
// attach gets its own fresh, dialAttachBudget-bounded step off ctx, not a
// share of whatever budget ctx has left: dialWire is called once per cut of
// a loop that can run well past a single watchdog window under load.
func dialWire(t *testing.T, ctx context.Context, path string) *wireClient {
	t.Helper()
	dialCtx, cancel := stepCtx(ctx, dialAttachBudget)
	defer cancel()
	s, err := remote.DialSession(dialCtx, path, remote.SessionOptions{
		Client: remote.Options{Client: protocol.ClientInfo{Kind: "test", Name: "exactness"}, StreamBytes: 1 << 30},
		Budget: &protocol.AttachBudget{MaxItems: wireBudget.MaxItems, MaxBytes: wireBudget.MaxBytes, SnapshotBytes: protocol.SnapshotBytesMax},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	attachCtx, cancel2 := stepCtx(ctx, dialAttachBudget)
	defer cancel2()
	if err := s.Attach(attachCtx); err != nil {
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
		trace := wireTrace(t)
		// The edit report's eight 64 KiB diff pairs cut to a byte a side
		// (SF-133): fanned out to every client already attached, that one
		// ~1 MiB event cost the next cut's attach minutes under -race at a
		// 5% CPU quota. The large payload is A2's in process and
		// TestAFullSizeEditReportReachesEachSocketClientWhole's over the
		// wire; this subtest is about the cuts. The trace is the source of
		// full, the host's model and every client's, so all of them see the
		// same small report.
		for i := range trace {
			if tl := trace[i].Tool; tl != nil && len(tl.Diffs) > 0 {
				small := *tl
				small.Diffs = slices.Clone(tl.Diffs)
				for j := range small.Diffs {
					small.Diffs[j].OldText, small.Diffs[j].NewText = "-", "+"
				}
				trace[i].Tool = &small
			}
		}

		// ctx carries no deadline of its own: the loop below is 145 cuts
		// deep, each holding one more attached client folding the rest of
		// the trace live, and under -race that whole loop can run well past
		// a single watchdog window (measured with the report cut: ~3s at
		// full CPU, ~2m at CPUQuota=5%). A single ctx sized for the whole
		// loop times out later cuts even when each step is individually
		// healthy. Every blocking step below instead gets its own
		// watchdog-bounded stepCtx, fresh off ctx, so a real hang at any
		// one step still fails within watchdog.
		ctx := context.Background()
		stub, e, path := stubHost(t, ctx)

		start := time.Now()
		clients := make([]*wireClient, 0, len(trace)+1)
		for k := 0; k <= len(trace); k++ {
			publishCut(t, ctx, stub, e, trace, k)
			clients = append(clients, dialWire(t, ctx, path))
		}
		n := uint64(len(trace))
		full := hostIsTheTrace(t, ctx, e, trace)

		first := clients[0]
		first.foldThrough(t, n)
		if d := engine.DiffModels(full, first.model, true); d != "" {
			t.Fatalf("the first client, folding every event from the wire, is not the trace folded whole: %s", d)
		}
		for k, c := range clients {
			model := c.attachedAt(t, k, trace)
			if d := engine.DiffModels(first.model, model, true); d != "" {
				t.Fatalf("cut %d (attached at %d, compared at %d): %s", k, k, n, d)
			}
		}
		t.Logf("%d events, %d cuts over the wire in %v", len(trace), len(clients), time.Since(start).Round(time.Millisecond))
	})

	t.Run("eight cuts over the fake agent", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		// Same shape as the trace loop above, checked: eight cuts sharing
		// one ctx is milder (no O(n^2) fold matrix), but a whole-loop
		// deadline still squeezes later cuts. ctx itself carries no
		// deadline; fakeAgentGate's release() closure is created once and
		// reused across all eight cuts (both sides reopen the FIFO fresh
		// each turn — the fake agent's own awaitGate included — the
		// closure is just made once here), but it now takes its own fresh,
		// watchdog-sized stepCtx off ctx on every call rather than sharing
		// one deadline — CodeRabbit found the previous
		// gateCtx (4*watchdog, fixed at this subtest's start) was exactly
		// the whole-loop shape C29f fixed elsewhere, just not yet here
		// (measured: this subtest ran up to 38.10s under a 25% cgroup
		// quota, well past a single 40s budget's remaining slack by its
		// last cut). Every other blocking step below gets its own fresh
		// stepCtx too.
		ctx := context.Background()
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
		startCtx, startCancel := stepCtx(ctx, watchdog)
		err = e.Start(startCtx)
		startCancel()
		if err != nil {
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
			cutoffCtx, cutoffCancel := stepCtx(ctx, watchdog)
			n := cutoff(t, cutoffCtx, e)
			cutoffCancel()
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

// wireTrace is recordStubSession's trace with its one event over the log's
// record bound cut to fit: a subscriber is handed that one Omitted, which the
// wire answers with reset{omitted} and a fresh snapshot — the reset path, not
// these tests' business (A2 in process and the remote's own tests hold it).
func wireTrace(t *testing.T) []agent.Event {
	t.Helper()
	trace, _, oversized, _ := recordStubSession(t)
	if oversized == 0 {
		t.Fatal("fixture: the trace has no oversized event")
	}
	trace[oversized-1].Text = "an oversized reply, cut to fit a record on the wire"
	return trace
}

// stubHost is a started engine over a Stub with no primary, on the trace's
// clock, served on the wire: the Stub, whose log a test publishes a trace
// into (publishCut), the engine and its socket's path.
func stubHost(t *testing.T, ctx context.Context) (*tui.Stub, *engine.Engine, string) {
	t.Helper()
	stub := tui.NewStubNoPrimary()
	stub.Clock = traceClock()
	e, err := engine.New(stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	startCtx, startCancel := stepCtx(ctx, watchdog)
	err = e.Start(startCtx)
	startCancel()
	if err != nil {
		t.Fatal(err)
	}
	return stub, e, serveWire(t, e)
}

// publishCut moves the host to cut k: it publishes the trace's k-th event
// (none at cut 0), and fails unless the host then holds exactly the trace's
// first k events — nothing of its own is published between them.
func publishCut(t *testing.T, ctx context.Context, stub *tui.Stub, e *engine.Engine, trace []agent.Event, k int) {
	t.Helper()
	if k > 0 {
		ev := trace[k-1]
		ev.Seq = 0
		pubCtx, pubCancel := stepCtx(ctx, watchdog)
		ok := stub.EventLog().Publish(pubCtx, nil, ev)
		pubCancel()
		if !ok {
			t.Fatalf("cut %d: the host did not publish the trace's event", k)
		}
	}
	syncCtx, syncCancel := stepCtx(ctx, watchdog)
	head, err := e.SyncSeq(syncCtx)
	syncCancel()
	if err != nil || head != uint64(k) {
		t.Fatalf("cut %d: the host's head is %d (%v)", k, head, err)
	}
}

// hostIsTheTrace is the trace folded whole, once the host holds all of it,
// and fails unless that is the host engine's own model (its snapshot,
// restored).
func hostIsTheTrace(t *testing.T, ctx context.Context, e *engine.Engine, trace []agent.Event) *transcript.Model {
	t.Helper()
	n := uint64(len(trace))
	full := transcript.New(transcript.Options{})
	for _, ev := range trace {
		full.Fold(ev)
	}
	attachCtx, attachCancel := stepCtx(ctx, watchdog)
	a, err := e.Attach(attachCtx, engine.AttachOptions{SnapshotBytes: 1 << 30})
	attachCancel()
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
	return full
}

// attachedAt waits for c, attached at cut k, to fold the whole trace, and
// fails unless it restored one snapshot, at k, and folded the trace's suffix
// after it: its model at the trace's end.
func (c *wireClient) attachedAt(t *testing.T, k int, trace []agent.Event) *transcript.Model {
	t.Helper()
	c.foldThrough(t, uint64(len(trace)))
	c.mu.Lock()
	restored, folded, model := c.restoredAt, c.folded, c.model
	c.mu.Unlock()
	if len(restored) != 1 || restored[0] != uint64(k) {
		t.Fatalf("cut %d: the client restored snapshots at %v, want one at %d", k, restored, k)
	}
	if folded != len(trace)-k {
		t.Fatalf("cut %d: the client folded %d events after its snapshot, want the %d of the suffix", k, folded, len(trace)-k)
	}
	return model
}

// TestAFullSizeEditReportReachesEachSocketClientWhole (SF-133's large
// payload over the wire): the trace's edit report at its caps — eight 64 KiB
// old/new diff pairs, ~1 MiB, which the every-cut subtest of
// TestAttachMidTurnOverTheSocketReproducesTheFirst cuts to a byte a side —
// reaches four socket clients whole: two attached at the two cuts before it
// (the report comes to both live, fanned out to more than one subscriber),
// one at the cut that carries it (in its snapshot) and one at the cut after
// (in its snapshot, the rest of the trace live). Each folds to the trace
// folded whole, which is the host's own model, and holds the report's diffs
// exactly. Four clients, not one per cut: bounded under -race at a 5% CPU
// quota.
func TestAFullSizeEditReportReachesEachSocketClientWhole(t *testing.T) {
	trace := wireTrace(t)
	edit := -1
	for i := range trace {
		if tl := trace[i].Tool; tl != nil && len(tl.Diffs) > 0 {
			if edit >= 0 {
				t.Fatalf("fixture: edit reports at seq %d and %d, want one", edit+1, i+1)
			}
			edit = i
		}
	}
	if edit < 1 || edit+2 > len(trace) {
		t.Fatalf("fixture: the edit report is at index %d of %d events", edit, len(trace))
	}
	report := trace[edit].Tool
	size := 0
	for _, d := range report.Diffs {
		size += len(d.OldText) + len(d.NewText)
	}
	if size < 1<<20 {
		t.Fatalf("fixture: the edit report's diffs are %d bytes, want its full ~1 MiB", size)
	}

	ctx := context.Background()
	stub, e, path := stubHost(t, ctx)
	start := time.Now()
	// Cut k is the host holding the trace's first k events: the report is
	// the edit+1-th.
	cuts := []int{edit - 1, edit, edit + 1, edit + 2}
	clients := make([]*wireClient, 0, len(cuts))
	for k := 0; k <= len(trace); k++ {
		publishCut(t, ctx, stub, e, trace, k)
		if slices.Contains(cuts, k) {
			clients = append(clients, dialWire(t, ctx, path))
		}
	}
	full := hostIsTheTrace(t, ctx, e, trace)
	diffsOf := func(m *transcript.Model) []agent.ToolDiff {
		for _, tl := range m.Tools() {
			if tl.ID == report.ID {
				return tl.Diffs
			}
		}
		return nil
	}
	if !slices.Equal(diffsOf(full), report.Diffs) {
		t.Fatalf("fixture: the trace folded whole does not hold the edit report %s whole", report.ID)
	}
	for i, c := range clients {
		k := cuts[i]
		model := c.attachedAt(t, k, trace)
		if got := diffsOf(model); !slices.Equal(got, report.Diffs) {
			n := 0
			for _, d := range got {
				n += len(d.OldText) + len(d.NewText)
			}
			t.Fatalf("cut %d: the client holds %d diffs of %d bytes for %s, want the report's %d of %d bytes",
				k, len(got), n, report.ID, len(report.Diffs), size)
		}
		if d := engine.DiffModels(full, model, true); d != "" {
			t.Fatalf("cut %d (attached at %d, compared at %d): %s", k, k, len(trace), d)
		}
	}
	t.Logf("the %d-byte edit report at seq %d, clients at cuts %v, over the wire in %v",
		size, edit+1, cuts, time.Since(start).Round(time.Millisecond))
}
