package roster_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// The roster (plan 030 §3.9, R2-8; §3.18 PR 2): the poll's budgets — a
// stalled dial, a stalled hello, a stalled sessions.list, a kept connection
// ended late and redialled inside the one budget — the cap on attempts in
// flight and one per host, the backoff, unreachable against saved,
// simultaneous disconnects, and a Close that joins everything. Hosts are
// internal/fakehost's — or a socket the test answers by hand, where a host
// must misbehave — in process, each on a socket of its own; the registry is
// a list the test keeps (TestOptions.Hosts) — the one test of the
// real registry is TestTheRosterReadsTheRegistry — and the tick and the clock
// are the test's own, so every schedule is the test's. Every wait is bounded
// on its own (step).

// step bounds each wait a test makes: generous next to anything a tick
// takes, so hitting it means the poll is wedged.
const step = 10 * time.Second

// registry is the test's registry: fake hosts, each served on a socket of its
// own under one short directory, and entries with no host behind them.
type registry struct {
	t   *testing.T
	dir string

	mu      sync.Mutex
	entries []rundir.Entry
	hosts   map[string]*fakehost.Host
}

func newRegistry(t *testing.T) *registry {
	t.Helper()
	// Under /tmp itself, never t.TempDir() or $TMPDIR: sun_path on macOS.
	dir, err := os.MkdirTemp("/tmp", "czro-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return &registry{t: t, dir: dir, hosts: map[string]*fakehost.Host{}}
}

// hostID is the n-th host's id: 12 hex digits.
func hostID(n int) string { return fmt.Sprintf("%012x", n) }

// sessionID is the n-th host's craze session id.
func sessionID(n int) string { return fmt.Sprintf("session-%d", n) }

// add serves a fake host — with the row facts when rowFacts — and lists it.
func (g *registry) add(n int, rowFacts bool) (*fakehost.Host, rundir.Entry) {
	g.t.Helper()
	id := hostID(n)
	h, err := fakehost.New(fakehost.Options{HostID: id, CrazeSessionID: sessionID(n), RowFacts: rowFacts,
		Workspace: "/work/" + id, StartedAt: true})
	if err != nil {
		g.t.Fatal(err)
	}
	socket := filepath.Join(g.dir, id)
	l, err := net.Listen("unix", socket)
	if err != nil {
		g.t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- h.Serve(l) }()
	g.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), step)
		defer cancel()
		_ = h.Close(ctx)
		<-served
	})
	e := rundir.Entry{Protocol: 1, HostID: id, PID: 4242, Socket: socket, CrazeSessionID: sessionID(n),
		Provider: "cursor", Workspace: "/work/" + id, Ready: true}
	g.mu.Lock()
	g.hosts[id] = h
	g.entries = append(g.entries, e)
	g.mu.Unlock()
	return h, e
}

// list adds e as it is: a registry entry whatever is behind it.
func (g *registry) list(e rundir.Entry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.entries = append(g.entries, e)
}

// unlist takes host id's entry out of the registry.
func (g *registry) unlist(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.entries = slices.DeleteFunc(g.entries, func(e rundir.Entry) bool { return e.HostID == id })
}

func (g *registry) read() ([]rundir.Entry, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.entries), nil
}

// testClock is the roster's clock, moved by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// rig is one roster under test: its ticks, its clock and its barriers are
// the test's.
type rig struct {
	t     *testing.T
	r     *roster.Roster
	ticks chan time.Time
	clk   *testClock
	// ticked receives once per tick the poller has run; applied once per
	// result it has taken.
	ticked  chan struct{}
	applied chan applied
	last    roster.Snapshot
}

type applied struct {
	hostID string
	err    error
}

// newRig opens a roster over g and index with the production rules, but the
// test's tick, clock and barriers; tweak, when set, changes the seams before
// it opens.
func newRig(t *testing.T, g *registry, index roster.Index, tweak func(*roster.TestOptions)) *rig {
	t.Helper()
	rg := &rig{t: t, ticks: make(chan time.Time), clk: &testClock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)},
		ticked: make(chan struct{}, 64), applied: make(chan applied, 256)}
	o := roster.TestOptions{
		Ticks:   rg.ticks,
		Now:     rg.clk.now,
		Hosts:   g.read,
		Ticked:  func() { rg.ticked <- struct{}{} },
		Applied: func(id string, err error) { rg.applied <- applied{id, err} },
	}
	if tweak != nil {
		tweak(&o)
	}
	rg.r = roster.OpenForTest(index, o)
	t.Cleanup(rg.r.Close)
	rg.waitTicked() // the first tick, at once
	return rg
}

// tick runs one tick and waits for the poller to have run it.
func (rg *rig) tick() {
	rg.t.Helper()
	select {
	case rg.ticks <- rg.clk.now():
	case <-time.After(step):
		rg.t.Fatal("the poller took no tick")
	}
	rg.waitTicked()
}

func (rg *rig) waitTicked() {
	rg.t.Helper()
	select {
	case <-rg.ticked:
	case <-time.After(step):
		rg.t.Fatal("the poller never finished its tick")
	}
}

// waitApplied waits for n results, answering them in the order taken.
func (rg *rig) waitApplied(n int) []applied {
	rg.t.Helper()
	var out []applied
	for len(out) < n {
		select {
		case a := <-rg.applied:
			out = append(out, a)
		case <-time.After(step):
			rg.t.Fatalf("%d of %d results taken", len(out), n)
		}
	}
	return out
}

// waitEach waits for a result from each host of ids, and fails on any
// result that is a failure: taken one at a time, each wait bounded on its
// own. A host that answers again before the others have answered once —
// asked a tick later over the connection it kept, which is quicker than
// another host's dial — is counted once: the wait is for every host by
// name, never for a number of results (C12r2).
func (rg *rig) waitEach(ids []string) {
	rg.t.Helper()
	left := map[string]bool{}
	for _, id := range ids {
		left[id] = true
	}
	var taken []string
	for len(left) > 0 {
		select {
		case a := <-rg.applied:
			if a.err != nil {
				rg.t.Fatalf("host %s: %v", a.hostID, a.err)
			}
			taken = append(taken, a.hostID)
			delete(left, a.hostID)
		case <-time.After(step):
			rg.t.Fatalf("%d hosts never answered; the results taken: %v", len(left), taken)
		}
	}
}

// until waits for a Snapshot pred holds for: the last one taken, or the
// next ones published.
func (rg *rig) until(what string, pred func(roster.Snapshot) bool) roster.Snapshot {
	rg.t.Helper()
	if pred(rg.last) {
		return rg.last
	}
	deadline := time.After(step)
	for {
		select {
		case s, ok := <-rg.r.Updates():
			if !ok {
				rg.t.Fatalf("%s: the roster stopped", what)
			}
			rg.last = s
			if pred(s) {
				return s
			}
		case <-deadline:
			rg.t.Fatalf("%s: never; the last snapshot: %s", what, describe(rg.last))
		}
	}
}

// row is id's row in s, nil when s has none.
func row(s roster.Snapshot, id string) *roster.Row {
	for i := range s.Running {
		if s.Running[i].Host.ID == id {
			return &s.Running[i]
		}
	}
	return nil
}

// status is a predicate: every id listed, each in st.
func status(st roster.Status, ids ...string) func(roster.Snapshot) bool {
	return func(s roster.Snapshot) bool {
		for _, id := range ids {
			if r := row(s, id); r == nil || r.Status != st {
				return false
			}
		}
		return true
	}
}

func describe(s roster.Snapshot) string {
	var b strings.Builder
	for _, r := range s.Running {
		fmt.Fprintf(&b, "[%s %s answered:%v] ", r.Host.ID, r.Status, r.Session != nil)
	}
	fmt.Fprintf(&b, "saved:%d", len(s.Saved))
	return b.String()
}

// memIndex is an index in memory that counts its reads.
type memIndex struct {
	mu    sync.Mutex
	rows  []sessions.Row
	reads int
}

func (m *memIndex) All() ([]sessions.Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	return slices.Clone(m.rows), nil
}

func (m *memIndex) readCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reads
}

// TestTheRosterListsEveryHostAndWhatItSaid: each host's row — a host with
// the row facts, and an older one, which says what S2's row does and its
// craze version — reachable, in host-id order, with the index's title for a
// running session.
func TestTheRosterListsEveryHostAndWhatItSaid(t *testing.T) {
	g := newRegistry(t)
	g.add(2, true)
	g.add(1, false)
	idx := &memIndex{rows: []sessions.Row{{SessionID: "p-2", Provider: "cursor", CWD: "/work", Title: "fix the flake", CrazeID: sessionID(2)}}}
	rg := newRig(t, g, idx, nil)
	rg.waitApplied(2)
	s := rg.until("both reachable", status(roster.Reachable, hostID(1), hostID(2)))
	if len(s.Running) != 2 || s.Running[0].Host.ID != hostID(1) || s.Running[1].Host.ID != hostID(2) {
		t.Fatalf("the rows, in host-id order: %s", describe(s))
	}
	older, withFacts := s.Running[0], s.Running[1]
	switch o := older.Session; {
	case o == nil || o.RowFacts || !o.Since.IsZero() || o.Doing != "" || o.Prompted:
		t.Fatalf("an older host's row: %+v", o)
	case older.Version != "0.0.0-fakehost" || o.ID != sessionID(1) || o.Activity != engine.ActivityIdle || o.Workspace != "/work/"+hostID(1):
		t.Fatalf("an older host's row: version %q, %+v", older.Version, o)
	}
	switch w := withFacts.Session; {
	case w == nil || !w.RowFacts || w.Since.IsZero() || w.StartedAt.IsZero():
		t.Fatalf("a row-facts host's row: %+v", w)
	case withFacts.IndexTitle != "fix the flake" || withFacts.Host.CrazeSessionID != sessionID(2):
		t.Fatalf("the index's title %q for %q", withFacts.IndexTitle, withFacts.Host.CrazeSessionID)
	}
	if len(s.Saved) != 0 {
		t.Fatalf("a running session is saved: %+v", s.Saved)
	}
}

// TestAHostIsAskedOverTheConnectionItKeeps: one dial per host, whatever the
// number of ticks; what a host says between ticks is in the next Snapshot.
func TestAHostIsAskedOverTheConnectionItKeeps(t *testing.T) {
	g := newRegistry(t)
	h, e := g.add(1, true)
	var mu sync.Mutex
	dials := 0
	rg := newRig(t, g, nil, func(o *roster.TestOptions) {
		o.Dial = func(ctx context.Context, path string) (net.Conn, error) {
			mu.Lock()
			dials++
			mu.Unlock()
			return roster.DialUnix(ctx, path)
		}
	})
	rg.waitApplied(1)
	for range 3 {
		rg.tick()
		rg.waitApplied(1)
	}
	if err := h.Do([]byte(`{"name":"permission","id":"perm-1","tool":"Run ` + "`make`" + `","options":[{"optionId":"a","name":"Allow","kind":"allow_once"}]}`)); err != nil {
		t.Fatal(err)
	}
	rg.tick()
	s := rg.until("the ask", func(s roster.Snapshot) bool {
		r := row(s, e.HostID)
		return r != nil && r.Session != nil && r.Session.PendingAsks == 1
	})
	if a := row(s, e.HostID).Session.HeadAsk; a == nil || a.Summary != "Run `make`" {
		t.Fatalf("the head ask %+v", a)
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 1 || h.OpenConns() != 1 {
		t.Fatalf("%d dials, %d connections open for five polls: want one kept", dials, h.OpenConns())
	}
}

// TestAModelChangeIsPublished (plan 035 C11, SF-114): a host's row carrying
// its model is read into the session's Model, and a change of the model
// alone — every other member as it was — is a change the list's roster
// publishes, in the round that reads it (Session.equal), as it would a title
// or an ask.
func TestAModelChangeIsPublished(t *testing.T) {
	g := newRegistry(t)
	var mu sync.Mutex
	model := "grok-4.7-build-fast"
	setModel := func(m string) {
		mu.Lock()
		defer mu.Unlock()
		model = m
	}
	socket := filepath.Join(g.dir, "model")
	rawHostOf(t, socket, hostID(1), func() string {
		mu.Lock()
		defer mu.Unlock()
		return `[{"sessionId":"` + sessionID(1) + `","activity":"idle","title":"kept",` +
			`"capabilities":{"rowFacts":true},"prompted":true,"model":"` + model + `"}]`
	})
	g.list(rundir.Entry{Protocol: 1, HostID: hostID(1), Socket: socket, CrazeSessionID: sessionID(1), Ready: true})
	rg := newRig(t, g, nil, nil)
	modelIs := func(want string) func(roster.Snapshot) bool {
		return func(s roster.Snapshot) bool {
			r := row(s, hostID(1))
			return r != nil && r.Session != nil && r.Session.Model == want
		}
	}
	s := rg.until("the host's model", modelIs("grok-4.7-build-fast"))
	if sess := row(s, hostID(1)).Session; !sess.RowFacts || !sess.Prompted || sess.Title != "kept" {
		t.Fatalf("the row's session: %+v", sess)
	}
	setModel("fast")
	rg.tick()
	s = rg.until("the model's change", modelIs("fast"))
	if sess := row(s, hostID(1)).Session; sess.Title != "kept" || !sess.Prompted {
		t.Fatalf("the row's session after the model's change: %+v", sess)
	}
}

// TestAHostThatStopsAnsweringSpendsOneAttemptsBudget: a dial that never
// completes, a hello never answered and a sessions.list never answered each
// end the attempt at its budget — its connection closed — and the host is
// unreachable, while a healthy host beside it is asked all the same.
func TestAHostThatStopsAnsweringSpendsOneAttemptsBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		// serve answers one connection of the stalled host's; nil is a dial
		// that never completes.
		serve func(t *testing.T, c net.Conn)
	}{
		{"a stalled dial", nil},
		{"a stalled hello", func(t *testing.T, c net.Conn) {}},
		{"a stalled sessions.list", func(t *testing.T, c net.Conn) {
			lr := protocol.NewLineReader(c, 0)
			if _, err := lr.ReadLine(); err != nil {
				t.Errorf("reading hello: %v", err)
				return
			}
			hello := `{"jsonrpc":"2.0","id":"1","result":{"protocol":1,"endpoint":{"kind":"host","hostId":"000000000009","crazeVersion":"0.0.1","pid":9},"clientId":"c-1","token":"00","resumed":false,"capabilities":{},"codecs":{"event":1,"snapshot":1},"limits":{"inboundLine":4194304,"outboundLine":16777216},"retryHorizon":{"commands":1,"ageMs":1}}}` + "\n"
			if _, err := c.Write([]byte(hello)); err != nil {
				t.Errorf("answering hello: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRegistry(t)
			g.add(1, true)
			stalled := rundir.Entry{Protocol: 1, HostID: hostID(9), Socket: filepath.Join(g.dir, "stalled"), CrazeSessionID: sessionID(9)}
			closed := make(chan error, 1)
			if tc.serve != nil {
				l, err := net.Listen("unix", stalled.Socket)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Close() })
				go func() {
					c, err := l.Accept()
					if err != nil {
						closed <- err
						return
					}
					defer c.Close()
					tc.serve(t, c)
					// Nothing more is answered: the roster's budget is what
					// ends the connection, which reads as its end here.
					_, err = c.Read(make([]byte, 1<<20))
					for err == nil {
						_, err = c.Read(make([]byte, 1<<20))
					}
					closed <- err
				}()
			}
			g.list(stalled)
			type dialed struct{ budget time.Duration }
			dials := make(chan dialed, 8)
			rg := newRig(t, g, nil, func(o *roster.TestOptions) {
				o.Dial = func(ctx context.Context, path string) (net.Conn, error) {
					if path == stalled.Socket {
						dl, ok := ctx.Deadline()
						if !ok {
							t.Error("a dial with no deadline")
						}
						dials <- dialed{time.Until(dl)}
						if tc.serve == nil {
							<-ctx.Done()
							return nil, ctx.Err()
						}
					}
					return roster.DialUnix(ctx, path)
				}
			})
			started := time.Now()
			var d dialed
			select {
			case d = <-dials:
			case <-time.After(step):
				t.Fatal("the stalled host was never dialled")
			}
			if d.budget > roster.DialBudget {
				t.Fatalf("the dial's deadline is %s away, over the %s budget", d.budget, roster.DialBudget)
			}
			s := rg.until("the stalled host unreachable, the healthy one reachable", func(s roster.Snapshot) bool {
				return status(roster.Unreachable, hostID(9))(s) && status(roster.Reachable, hostID(1))(s)
			})
			if took := time.Since(started); took < roster.DialBudget/2 {
				t.Fatalf("the stalled attempt failed after %s: it did not wait for its budget", took)
			}
			if r := row(s, hostID(9)); r.Session != nil || r.Version != "" {
				t.Fatalf("the stalled host's row %+v", r)
			}
			if tc.serve != nil {
				select {
				case err := <-closed:
					if !errors.Is(err, net.ErrClosed) && !isEOF(err) {
						t.Fatalf("the stalled connection ended with %v, want the roster's close", err)
					}
				case <-time.After(step):
					t.Fatal("the roster never closed the stalled connection")
				}
			}
		})
	}
}

func isEOF(err error) bool { return err != nil && strings.Contains(err.Error(), "EOF") }

// TestAttemptsAreCappedAndNeverTwoForAHost: twenty hosts whose dials hang —
// eight attempts, and no more, however many ticks go by; none for a host
// already in one; and as those eight end, their places go to hosts the tick
// has not asked, never back to one just asked.
func TestAttemptsAreCappedAndNeverTwoForAHost(t *testing.T) {
	g := newRegistry(t)
	gates := map[string]chan struct{}{}
	for n := 1; n <= 20; n++ {
		g.list(rundir.Entry{Protocol: 1, HostID: hostID(n), Socket: filepath.Join(g.dir, hostID(n)), CrazeSessionID: sessionID(n)})
		gates[hostID(n)] = make(chan struct{})
	}
	entered := make(chan string, 64)
	rg := newRig(t, g, nil, func(o *roster.TestOptions) {
		o.DialBudget = time.Hour
		o.Dial = func(ctx context.Context, path string) (net.Conn, error) {
			id := filepath.Base(path)
			entered <- id
			select {
			case <-gates[id]:
			case <-ctx.Done():
			}
			return nil, errors.New("no host here")
		}
	})
	// take reads the next n hosts dialled, each once.
	take := func(n int, what string) map[string]bool {
		t.Helper()
		got := map[string]bool{}
		for len(got) < n {
			select {
			case id := <-entered:
				if got[id] {
					t.Fatalf("%s: host %s dialled twice", what, id)
				}
				got[id] = true
			case <-time.After(step):
				t.Fatalf("%s: %d attempts started, want %d", what, len(got), n)
			}
		}
		return got
	}
	first := take(roster.MaxInFlight, "the first tick")
	for range 3 {
		rg.clk.advance(roster.TickEvery)
		rg.tick()
	}
	select {
	case id := <-entered:
		t.Fatalf("host %s dialled with %d attempts in flight", id, roster.MaxInFlight)
	default:
	}
	for id := range first {
		close(gates[id])
	}
	for _, a := range rg.waitApplied(roster.MaxInFlight) {
		if !first[a.hostID] || a.err == nil {
			t.Fatalf("a result %+v", a)
		}
	}
	for id := range take(roster.MaxInFlight, "the places the first eight left") {
		if first[id] {
			t.Fatalf("host %s, just asked, went ahead of the hosts not asked yet", id)
		}
	}
	select {
	case id := <-entered:
		t.Fatalf("host %s dialled with %d attempts in flight again", id, roster.MaxInFlight)
	default:
	}
}

// TestAFailingHostBacksOffToThirtySeconds: after each failure the next
// attempt waits 1 s, 2 s, 4 s … at most 30 s, the ticks between asking
// nothing; the row stays unreachable throughout; an answer ends the backoff.
//
// "Asking nothing" is counted where it cannot be missed (sol r17-c9 5): an
// attempt is counted on the poller as it starts (Attempting), before the
// tick's own barrier (ticked), so once rg.tick has returned every attempt
// that tick started is in the count — however late its goroutine would run
// or report.
func TestAFailingHostBacksOffToThirtySeconds(t *testing.T) {
	g := newRegistry(t)
	_, e := g.add(1, true)
	var mu sync.Mutex
	failing := true
	var starts atomic.Int32
	rg := newRig(t, g, nil, func(o *roster.TestOptions) {
		o.Attempting = func(string) { starts.Add(1) }
		o.Dial = func(ctx context.Context, path string) (net.Conn, error) {
			mu.Lock()
			f := failing
			mu.Unlock()
			if f {
				return nil, errors.New("connection refused")
			}
			return roster.DialUnix(ctx, path)
		}
	})
	rg.waitApplied(1)
	rg.until("unreachable", status(roster.Unreachable, e.HostID))
	if n := starts.Load(); n != 1 {
		t.Fatalf("%d attempts started by the first tick, want 1", n)
	}
	// ticks runs one tick at the clock's now and answers how many attempts
	// it started: exact, at the tick's barrier.
	ticks := func() int32 {
		before := starts.Load()
		rg.tick()
		return starts.Load() - before
	}
	waits := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for _, wait := range waits {
		// Just short of the wait: nothing is asked.
		rg.clk.advance(wait - time.Millisecond)
		if n := ticks(); n != 0 {
			t.Fatalf("%d attempts started %s into a %s backoff", n, wait-time.Millisecond, wait)
		}
		rg.clk.advance(time.Millisecond)
		if n := ticks(); n != 1 {
			t.Fatalf("%d attempts started as a %s backoff ended, want 1", n, wait)
		}
		if a := rg.waitApplied(1)[0]; a.err == nil {
			t.Fatalf("the attempt after %s answered", wait)
		}
		if r := row(rg.until("still unreachable", status(roster.Unreachable, e.HostID)), e.HostID); r.Status != roster.Unreachable {
			t.Fatalf("the row %+v", r)
		}
	}
	mu.Lock()
	failing = false
	mu.Unlock()
	rg.clk.advance(30 * time.Second)
	if n := ticks(); n != 1 {
		t.Fatalf("%d attempts started after the last backoff, want 1", n)
	}
	if a := rg.waitApplied(1)[0]; a.err != nil {
		t.Fatalf("the host answering: %v", a.err)
	}
	rg.until("reachable", status(roster.Reachable, e.HostID))
	rg.clk.advance(roster.TickEvery)
	if n := ticks(); n != 1 {
		t.Fatalf("%d attempts started a tick after the answer, want 1", n)
	}
	if a := rg.waitApplied(1)[0]; a.err != nil {
		t.Fatalf("the next tick's attempt, the backoff over: %v", a.err)
	}
}

// TestUnreachableIsNeverSaved: a session the registry lists whose host does
// not answer is running and unreachable — never saved — and saved once the
// registry no longer lists it. Saved rows are the index's newest per craze
// id, a legacy row by its own id, of a provider craze can resume.
func TestUnreachableIsNeverSaved(t *testing.T) {
	g := newRegistry(t)
	dead := rundir.Entry{Protocol: 1, HostID: hostID(7), Socket: filepath.Join(g.dir, "nobody"), CrazeSessionID: sessionID(7), Provider: "cursor"}
	g.list(dead)
	at := func(h int) time.Time { return time.Date(2026, 9, 29, h, 0, 0, 0, time.UTC) }
	idx := &memIndex{rows: []sessions.Row{
		{SessionID: "p-7b", Provider: "cursor", CWD: "/w", Title: "newest of 7", CrazeID: sessionID(7), UpdatedAt: at(9)},
		{SessionID: "legacy", Provider: "grok", CWD: "/w", Title: "a legacy row", UpdatedAt: at(8)},
		{SessionID: "p-7a", Provider: "cursor", CWD: "/w", Title: "older of 7", CrazeID: sessionID(7), UpdatedAt: at(7)},
		{SessionID: "p-5", Provider: "cursor", CWD: "/w", Title: "five", CrazeID: sessionID(5), UpdatedAt: at(6)},
		{SessionID: "p-x", Provider: "nosuch", CWD: "/w", Title: "unknown provider", CrazeID: "x", UpdatedAt: at(5)},
	}}
	rg := newRig(t, g, idx, nil)
	rg.waitApplied(1)
	s := rg.until("the dead host unreachable", status(roster.Unreachable, dead.HostID))
	if got := titles(s.Saved); got != "a legacy row|five" {
		t.Fatalf("saved %q while its host is listed", got)
	}
	if r := row(s, dead.HostID); r.IndexTitle != "newest of 7" {
		t.Fatalf("the unreachable row's index title %q", r.IndexTitle)
	}
	g.unlist(dead.HostID)
	rg.tick()
	s = rg.until("saved once unlisted", func(s roster.Snapshot) bool { return row(s, dead.HostID) == nil })
	if got := titles(s.Saved); got != "newest of 7|a legacy row|five" {
		t.Fatalf("saved %q once its host is gone", got)
	}
}

func titles(rows []sessions.Row) string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Title)
	}
	return strings.Join(out, "|")
}

// TestTheIndexIsReadOnlyWhenItChanges: one read while the index file is as
// it was, another once it changes; running legacy rows are matched by their
// provider session id; at most fifty saved rows, newest first.
func TestTheIndexIsReadOnlyWhenItChanges(t *testing.T) {
	g := newRegistry(t)
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var rows []sessions.Row
	for n := range 60 {
		rows = append(rows, sessions.Row{SessionID: fmt.Sprintf("p-%02d", n), Provider: "cursor", CWD: "/w", Title: fmt.Sprintf("%02d", n)})
	}
	g.list(rundir.Entry{Protocol: 1, HostID: hostID(1), Socket: filepath.Join(g.dir, "nobody"), Provider: "cursor", ProviderSessionID: "p-00"})
	idx := &memIndex{rows: rows}
	rg := newRig(t, g, idx, func(o *roster.TestOptions) { o.IndexPath = func() string { return path } })
	for range 3 {
		rg.tick()
	}
	if n := idx.readCount(); n != 1 {
		t.Fatalf("%d reads of an unchanged index", n)
	}
	s := rg.until("the saved rows", func(s roster.Snapshot) bool { return len(s.Saved) > 0 })
	if len(s.Saved) != roster.SavedMax || s.Saved[0].Title != "01" || s.Saved[roster.SavedMax-1].Title != "50" {
		t.Fatalf("saved %d rows, %q…%q: want the fifty newest, the running legacy row out", len(s.Saved), s.Saved[0].Title, s.Saved[len(s.Saved)-1].Title)
	}
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rg.tick()
	if n := idx.readCount(); n != 2 {
		t.Fatalf("%d reads once the index changed, want 2", n)
	}
}

// TestSimultaneousDisconnectsAreDialledPast: every host drops its
// connection at once; the next tick's attempt finds each kept connection
// closed and dials a fresh one within its own budget — no row goes
// unreachable — and each host then holds exactly one connection again.
// Two ticks run back to back, so a host that has redialled may be asked
// again over its new connection while another's redial is still in flight:
// the test waits for every host by name (waitEach), not for ten results,
// which such second answers can fill first (C12r2: 1 in 100 at a 5 % quota).
func TestSimultaneousDisconnectsAreDialledPast(t *testing.T) {
	g := newRegistry(t)
	const n = 10
	var hosts []*fakehost.Host
	var ids []string
	for i := 1; i <= n; i++ {
		h, e := g.add(i, true)
		hosts = append(hosts, h)
		ids = append(ids, e.HostID)
	}
	// This test is about redialling past dropped connections, not about the
	// attempt budget (TestAHostThatStopsAnsweringSpendsOneAttemptsBudget and
	// TestAStaleRedialSpendsWhatIsLeftOfTheAttempt pin that): generous
	// budgets, because under -race at a 5% CPU quota ten hosts' fresh dials,
	// hellos and lists can outrun the real 500 ms (seen 4/100, C12r2).
	rg := newRig(t, g, nil, func(o *roster.TestOptions) {
		o.DialBudget = 10 * time.Second
		o.ListBudget = 10 * time.Second
	})
	rg.waitApplied(n) // every host answered once: eight at a time, in one tick
	rg.until("all reachable", status(roster.Reachable, ids...))
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Go(func() {
			if err := h.DropConnections(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	rg.clk.advance(roster.TickEvery)
	rg.tick()
	rg.clk.advance(roster.TickEvery)
	rg.tick()
	rg.waitEach(ids)
	s := rg.until("all reachable", status(roster.Reachable, ids...))
	for i, h := range hosts {
		if c := h.OpenConns(); c != 1 {
			t.Fatalf("host %s holds %d connections, want the one redialled (%s)", ids[i], c, describe(s))
		}
	}
}

// TestAHostGoneFromTheRegistryIsForgotten: its row goes, and its connection
// with it.
func TestAHostGoneFromTheRegistryIsForgotten(t *testing.T) {
	g := newRegistry(t)
	h, e := g.add(1, false)
	rg := newRig(t, g, nil, nil)
	rg.waitApplied(1)
	rg.until("reachable", status(roster.Reachable, e.HostID))
	g.unlist(e.HostID)
	rg.tick()
	rg.until("gone", func(s roster.Snapshot) bool { return len(s.Running) == 0 })
	waitConns(t, h, 0)
}

// waitConns waits, within a step, for h to hold n connections: a close the
// roster made reaches the host's reader asynchronously.
func waitConns(t *testing.T, h *fakehost.Host, n int) {
	t.Helper()
	deadline := time.Now().Add(step)
	for h.OpenConns() != n {
		if time.Now().After(deadline) {
			t.Fatalf("the host holds %d connections, want %d", h.OpenConns(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAHostWithNoSessionYetIsNotAsked: an entry that names no session — a
// host whose engine is not up — is listed Connecting and not dialled.
func TestAHostWithNoSessionYetIsNotAsked(t *testing.T) {
	g := newRegistry(t)
	g.list(rundir.Entry{Protocol: 1, HostID: hostID(3), Socket: filepath.Join(g.dir, "early")})
	var dials atomic.Int32
	rg := newRig(t, g, nil, func(o *roster.TestOptions) {
		o.Dial = func(context.Context, string) (net.Conn, error) { dials.Add(1); return nil, errors.New("no") }
	})
	rg.tick()
	s := rg.until("listed", func(s roster.Snapshot) bool { return row(s, hostID(3)) != nil })
	if r := row(s, hostID(3)); r.Status != roster.Connecting || dials.Load() != 0 {
		t.Fatalf("status %s after %d dials", r.Status, dials.Load())
	}
}

// TestAnEmptyRosterPublishesItsFirstSnapshot (C12r2, r27-pr2 2): a registry
// that lists nothing and an index that holds nothing — a fresh home, or one
// whose only session has just ended and left the registry — change nothing
// at the first tick, and it publishes anyway: an empty Snapshot, so the list
// can draw that nothing runs rather than nothing at all. Once: a tick that
// changes nothing still publishes nothing. So it goes with no index and with
// an empty one, and through the real registry of a fresh home.
func TestAnEmptyRosterPublishesItsFirstSnapshot(t *testing.T) {
	empty := func(t *testing.T, s roster.Snapshot, ok bool) {
		t.Helper()
		if !ok || len(s.Running) != 0 || len(s.Saved) != 0 || s.RegistryErr != nil || s.IndexErr != nil {
			t.Fatalf("the first snapshot: %s, registry %v, index %v (slot open %v)", describe(s), s.RegistryErr, s.IndexErr, ok)
		}
	}
	for _, tc := range []struct {
		name  string
		index roster.Index
	}{
		{"no index", nil},
		{"an empty index", &memIndex{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rg := newRig(t, newRegistry(t), tc.index, nil)
			// newRig has waited for the first tick, whose publish comes before
			// its barrier: the slot holds its Snapshot now, or never will.
			select {
			case s, ok := <-rg.r.Updates():
				empty(t, s, ok)
			default:
				t.Fatal("the first tick over an empty registry published nothing")
			}
			rg.clk.advance(roster.TickEvery)
			rg.tick()
			select {
			case s, ok := <-rg.r.Updates():
				t.Fatalf("a tick that changed nothing published %s (slot open %v)", describe(s), ok)
			default:
			}
		})
	}
	t.Run("the real registry of a fresh home", func(t *testing.T) {
		r := roster.Open(testEnv(t), nil)
		t.Cleanup(r.Close)
		select {
		case s, ok := <-r.Updates():
			empty(t, s, ok)
		case <-time.After(step):
			t.Fatal("no snapshot over an empty registry")
		}
	})
}

// deadlineConn is a connection whose deadlines the test sees: each one set
// (not the zero that clears it) is noted as it is set.
type deadlineConn struct {
	net.Conn
	note func(at time.Time)
}

func (c *deadlineConn) SetDeadline(t time.Time) error {
	if !t.IsZero() {
		c.note(t)
	}
	return c.Conn.SetDeadline(t)
}

// scriptedHost serves socket by hand: hello and sessions.list answered as a
// host answers them, for the session id, except where serve's caller says
// otherwise — lists, when it answers false for a connection's n-th list
// (from 1), ends that connection there instead.
func scriptedHost(t *testing.T, socket, id string, lists func(conn, n int) bool) {
	t.Helper()
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	hello, err := json.Marshal(protocol.HelloResult{Protocol: 1, Endpoint: protocol.Endpoint{Kind: protocol.EndpointHost, HostID: "000000000001", CrazeVersion: "0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	row, err := json.Marshal(protocol.SessionsListResult{Sessions: []protocol.SessionRow{{SessionInfo: protocol.SessionInfo{SessionID: id}, Activity: "idle"}}})
	if err != nil {
		t.Fatal(err)
	}
	serve := func(c net.Conn, conn int) {
		defer c.Close()
		lr := protocol.NewLineReader(c, 0)
		listed := 0
		for {
			line, err := lr.ReadLine()
			if err != nil {
				return
			}
			var req protocol.Request
			if err := json.Unmarshal(line, &req); err != nil {
				t.Errorf("the roster wrote %q: %v", line, err)
				return
			}
			result := hello
			if req.Method == protocol.MethodSessionsList {
				listed++
				if !lists(conn, listed) {
					return
				}
				result = row
			}
			out, err := protocol.MarshalLine(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: req.ID, Result: result})
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := c.Write(out); err != nil {
				return
			}
		}
	}
	go func() {
		for conn := 1; ; conn++ {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go serve(c, conn)
		}
	}()
}

// TestAStaleRedialSpendsWhatIsLeftOfTheAttempt (sol r17-c9 2): a kept
// connection that its host ends late in a sessions.list — a second before the
// list's deadline — is redialled within what is left of the attempt's one
// budget, dialBudget + listBudget from the attempt's start, never with fresh
// budgets of its own: every deadline the attempt sets, its dial's and each
// exchange's, lies inside that one budget, so the attempt ends within it
// whatever the host does. The redial is answered, so the row stays
// reachable (X66). Given fresh budgets, the redial's list would have run
// until at least a second past the budget's end: the host ends the list with
// resetLead of its own share left, and the fresh list share is longer than
// what remains after that by dialBudget.
func TestAStaleRedialSpendsWhatIsLeftOfTheAttempt(t *testing.T) {
	const (
		dialBudget = time.Second
		listBudget = 4 * time.Second
		// resetLead is how long before the kept list's deadline its host ends
		// the connection: the slack a starved scheduler has to run the host's
		// end before the roster's own deadline would.
		resetLead = 2 * time.Second
	)
	g := newRegistry(t)
	e := rundir.Entry{Protocol: 1, HostID: hostID(1), Socket: filepath.Join(g.dir, "resets"), CrazeSessionID: sessionID(1)}

	// marks is every attempt's start and every deadline it set, in order.
	type mark struct {
		start bool
		at    time.Time
		what  string
	}
	var mu sync.Mutex
	var marks []mark
	note := func(m mark) {
		mu.Lock()
		defer mu.Unlock()
		marks = append(marks, m)
	}
	lastDeadline := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		for i := len(marks) - 1; i >= 0; i-- {
			if !marks[i].start && marks[i].what != "dial" {
				return marks[i].at
			}
		}
		return time.Time{}
	}
	scriptedHost(t, e.Socket, e.CrazeSessionID, func(conn, n int) bool {
		if conn == 1 && n == 2 {
			// The kept connection's second list, the attempt under test: ended
			// resetLead before the deadline the roster set for it.
			time.Sleep(time.Until(lastDeadline().Add(-resetLead)))
			return false
		}
		return true
	})
	g.list(e)
	var dials atomic.Int32
	rg := newRig(t, g, nil, func(o *roster.TestOptions) {
		o.DialBudget, o.ListBudget = dialBudget, listBudget
		o.Attempting = func(string) { note(mark{start: true, at: time.Now()}) }
		o.Dial = func(ctx context.Context, path string) (net.Conn, error) {
			dials.Add(1)
			dl, ok := ctx.Deadline()
			if !ok {
				t.Error("a dial with no deadline")
			}
			note(mark{at: dl, what: "dial"})
			c, err := roster.DialUnix(ctx, path)
			if err != nil {
				return nil, err
			}
			return &deadlineConn{Conn: c, note: func(at time.Time) { note(mark{at: at, what: "exchange"}) }}, nil
		}
	})
	if a := rg.waitApplied(1)[0]; a.err != nil {
		t.Fatalf("the first attempt: %v", a.err)
	}
	rg.until("reachable", status(roster.Reachable, e.HostID))
	rg.clk.advance(roster.TickEvery)
	rg.tick()
	if a := rg.waitApplied(1)[0]; a.err != nil {
		t.Fatalf("the attempt whose kept connection the host ended: %v", a.err)
	}
	if r := row(rg.until("still reachable", status(roster.Reachable, e.HostID)), e.HostID); r.Session == nil {
		t.Fatalf("the row %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	attempts, dialled := 0, 0
	var start time.Time
	for _, m := range marks {
		if m.start {
			attempts++
			start = m.at
			continue
		}
		if m.what == "dial" {
			dialled++
		}
		if end := start.Add(dialBudget + listBudget); m.at.After(end) {
			t.Errorf("attempt %d set a %s deadline %s past the end of its one budget (%s from its start)", attempts, m.what, m.at.Sub(end), dialBudget+listBudget)
		}
	}
	if attempts != 2 || dialled != 2 || dials.Load() != 2 {
		t.Fatalf("%d attempts, %d dials: want the second attempt to have redialled its ended connection once", attempts, dialled)
	}
}

// holdingConn is a connection whose first Close is held, once the connection
// is closed, until release is closed: counted in held while it is, and
// announced on holds. The first call is claimed before the connection is
// closed, so no later one — the attempt's own, once the close has ended its
// read — can be the one held.
type holdingConn struct {
	net.Conn
	held    *atomic.Int32
	holds   chan<- string
	release <-chan struct{}
	once    atomic.Bool
}

func (c *holdingConn) Close() error {
	if !c.once.CompareAndSwap(false, true) {
		return c.Conn.Close()
	}
	c.held.Add(1)
	defer c.held.Add(-1)
	err := c.Conn.Close()
	c.holds <- "the close of a stalled hello's connection"
	<-c.release
	return err
}

// closeGrace is how long TestCloseJoinsEverything gives a Close that does not
// join to return while what it should join is held: a Close that waited for
// results alone returns within it at once, and one that joins never does
// until the hold is released.
const closeGrace = 500 * time.Millisecond

// TestCloseJoinsEverything (sol r17-c9 3): with connections kept and
// attempts in flight, Close cancels and joins every attempt, closes every
// connection and the slot.
//
// Joined is asserted where a Close that did not join would be caught, one
// thing held at a time — each alone is then all that can keep Close from
// returning: attempts whose dials hang, each held at its goroutine's last
// statement (Attempted), after it has sent its result — where a Close that
// waits for results alone returns — and an attempt whose hello is never
// answered, its connection held inside the close the roster's context makes
// of it — where an exchange that leaves that close running lets its attempt
// return. Close must not return until what is held is released, and at its
// return — read there, not polled for — no attempt and no close of theirs is
// still running.
func TestCloseJoinsEverything(t *testing.T) {
	t.Run("attempts held after their results", func(t *testing.T) { closeJoins(t, false) })
	t.Run("a close mid-exchange held", func(t *testing.T) { closeJoins(t, true) })
}

// closeJoins is TestCloseJoinsEverything with three live hosts, their
// connections kept, and in flight either three attempts whose dials hang or
// — midExchange — one whose hello is never answered.
func closeJoins(t *testing.T, midExchange bool) {
	g := newRegistry(t)
	var hosts []*fakehost.Host
	for i := 1; i <= 3; i++ {
		h, _ := g.add(i, true)
		hosts = append(hosts, h)
	}
	hangs := map[string]bool{}
	inFlight := make(chan struct{}, 8)
	want := 3
	if midExchange {
		want = 1
		stalled := rundir.Entry{Protocol: 1, HostID: hostID(7), Socket: filepath.Join(g.dir, "stalled"), CrazeSessionID: sessionID(7)}
		l, err := net.Listen("unix", stalled.Socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		go func() {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			if _, err := protocol.NewLineReader(c, 0).ReadLine(); err == nil {
				inFlight <- struct{}{}
			}
			// Never answered: the roster's close is what ends it.
			_, _ = io.Copy(io.Discard, c)
		}()
		g.list(stalled)
	} else {
		for i := 4; i <= 6; i++ {
			g.list(rundir.Entry{Protocol: 1, HostID: hostID(i), Socket: filepath.Join(g.dir, "hang"), CrazeSessionID: sessionID(i)})
			hangs[hostID(i)] = true
		}
	}

	// running counts the attempts between their start and their last
	// statement; closing, the stalled hello's close while it is held.
	var running, closing atomic.Int32
	holds := make(chan string, 16)
	release := make(chan struct{})
	rg := newRig(t, g, nil, func(o *roster.TestOptions) {
		o.DialBudget = time.Hour
		o.Attempting = func(string) { running.Add(1) }
		o.Attempted = func(id string) {
			defer running.Add(-1)
			if hangs[id] {
				holds <- "hanging attempt " + id
				<-release
			}
		}
		o.Dial = func(ctx context.Context, path string) (net.Conn, error) {
			switch filepath.Base(path) {
			case "hang":
				inFlight <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			case "stalled":
				c, err := roster.DialUnix(ctx, path)
				if err != nil {
					return nil, err
				}
				return &holdingConn{Conn: c, held: &closing, holds: holds, release: release}, nil
			}
			return roster.DialUnix(ctx, path)
		}
	})
	rg.until("the live hosts reachable", status(roster.Reachable, hostID(1), hostID(2), hostID(3)))
	for range want {
		select {
		case <-inFlight:
		case <-time.After(step):
			t.Fatal("the attempts to hold never got in flight")
		}
	}

	type atReturn struct{ running, closing int32 }
	closed := make(chan atReturn, 1)
	go func() {
		rg.r.Close()
		closed <- atReturn{running.Load(), closing.Load()}
	}()
	for range want {
		select {
		case <-holds:
		case <-time.After(step):
			t.Fatal("Close did not end the attempts in flight")
		}
	}
	select {
	case got := <-closed:
		t.Fatalf("Close returned with %d attempts and %d closes of theirs still running", got.running, got.closing)
	case <-time.After(closeGrace):
	}
	close(release)
	select {
	case got := <-closed:
		if got != (atReturn{}) {
			t.Fatalf("Close returned with %d attempts and %d closes of theirs still running", got.running, got.closing)
		}
	case <-time.After(step):
		t.Fatal("Close never returned")
	}
	if _, ok := <-rg.r.Updates(); ok {
		// A Snapshot left in the slot is still read; the channel is closed
		// after it.
		if _, ok := <-rg.r.Updates(); ok {
			t.Fatal("the slot is still open after Close")
		}
	}
	for _, h := range hosts {
		waitConns(t, h, 0)
	}
	rg.r.Close() // idempotent
}

// TestTheRosterReadsTheRegistry: through the real registry (rundir.Hosts)
// and the production dial and peer check, a bound host is listed and
// reachable.
func TestTheRosterReadsTheRegistry(t *testing.T) {
	env := testEnv(t)
	bound := bindFakeHost(t, env, 1)
	r := roster.Open(env, nil)
	t.Cleanup(r.Close)
	deadline := time.After(step)
	for {
		select {
		case s := <-r.Updates():
			if rw := row(s, bound); rw != nil && rw.Status == roster.Reachable && rw.Session != nil && rw.Session.RowFacts {
				return
			}
		case <-deadline:
			t.Fatal("the bound host never listed reachable")
		}
	}
}

// testEnv is an isolated registry: its own home, craze directory and short
// runtime directory.
func testEnv(t *testing.T) rundir.Env {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := os.MkdirTemp("/tmp", "czrr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rt) })
	return rundir.Env{Home: home, CrazeDir: filepath.Join(home, ".craze"), CrazeRuntimeDir: rt, EUID: os.Geteuid()}
}

// bindFakeHost binds host n in env's registry (rundir.Bind), serves a fake
// host with the row facts on its socket and publishes its session, as a
// craze host does once its engine is up. It answers the host id.
func bindFakeHost(t testing.TB, env rundir.Env, n int) string {
	t.Helper()
	id := hostID(n)
	bh, err := rundir.Bind(env, id, rundir.Entry{StartedAt: time.Now().UTC(), Workspace: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	fh, err := fakehost.New(fakehost.Options{HostID: id, CrazeSessionID: sessionID(n), RowFacts: true})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- fh.Serve(bh.Listener()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), step)
		defer cancel()
		_ = fh.Close(ctx)
		<-served
		_ = bh.Close()
	})
	if err := bh.Update(func(e *rundir.Entry) {
		e.CrazeSessionID, e.Provider, e.Ready = sessionID(n), "cursor", true
	}); err != nil {
		t.Fatal(err)
	}
	return id
}
