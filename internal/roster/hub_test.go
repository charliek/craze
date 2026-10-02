package roster_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
)

// The hub's roster (plan 032 §3.6, P1): the list's poller opened by OpenHub —
// no index, each row kept as the JSON value its host sent, every Snapshot
// handed to Publish in order, and a poll only while a run is open (Resume,
// Pause). Hosts are internal/fakehost's, or a socket the test answers by hand
// where a row must carry what no fakehost sends; the registry, the tick and
// the clock are the test's.

// hubRig is one OpenHub roster under test: every Snapshot it publishes is
// kept, the registry reads and the dials counted.
type hubRig struct {
	t     *testing.T
	r     *roster.Roster
	ticks chan time.Time
	clk   *testClock
	reads atomic.Int32
	dials atomic.Int32

	mu    sync.Mutex
	snaps []roster.Snapshot
	note  chan struct{}
}

func newHubRig(t *testing.T, g *registry) *hubRig {
	t.Helper()
	return newHubRigWith(t, g, nil)
}

// newHubRigWith is newHubRig with tweak's seams over the rig's.
func newHubRigWith(t *testing.T, g *registry, tweak func(*roster.HubOptions)) *hubRig {
	t.Helper()
	rg := &hubRig{t: t, ticks: make(chan time.Time), clk: &testClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)},
		note: make(chan struct{}, 1)}
	o := roster.HubOptions{
		Hosts: func() ([]rundir.Entry, error) {
			rg.reads.Add(1)
			return g.read()
		},
		Publish: func(s roster.Snapshot) {
			rg.mu.Lock()
			rg.snaps = append(rg.snaps, s)
			rg.mu.Unlock()
			select {
			case rg.note <- struct{}{}:
			default:
			}
		},
		Client: protocol.ClientInfo{Kind: "hub", Name: "roster test"},
		Ticks:  rg.ticks,
		Now:    rg.clk.now,
		Dial: func(ctx context.Context, path string) (net.Conn, error) {
			rg.dials.Add(1)
			return roster.DialUnix(ctx, path)
		},
	}
	if tweak != nil {
		tweak(&o)
	}
	rg.r = roster.OpenHub(o)
	t.Cleanup(rg.r.Close)
	return rg
}

// tick hands the poller one tick: it has taken it once the send returns.
func (rg *hubRig) tick() {
	rg.t.Helper()
	select {
	case rg.ticks <- rg.clk.now():
	case <-time.After(step):
		rg.t.Fatal("the poller took no tick")
	}
}

// published is every Snapshot so far.
func (rg *hubRig) published() []roster.Snapshot {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return append([]roster.Snapshot(nil), rg.snaps...)
}

// until waits for a published Snapshot pred holds for, the latest first; it
// answers it and its index.
func (rg *hubRig) until(what string, pred func(roster.Snapshot) bool) (roster.Snapshot, int) {
	rg.t.Helper()
	deadline := time.After(step)
	for {
		snaps := rg.published()
		for i := len(snaps) - 1; i >= 0; i-- {
			if pred(snaps[i]) {
				return snaps[i], i
			}
		}
		select {
		case <-rg.note:
		case <-deadline:
			last := roster.Snapshot{}
			if len(snaps) > 0 {
				last = snaps[len(snaps)-1]
			}
			rg.t.Fatalf("%s: never; the last snapshot (run %d): %s", what, last.Run, describe(last))
		}
	}
}

// first waits for a published Snapshot pred holds for and answers the
// earliest one: what the poller published first, however far it has gone
// since — until answers the latest, which an attempt that came back meanwhile
// may already have moved past.
func (rg *hubRig) first(what string, pred func(roster.Snapshot) bool) roster.Snapshot {
	rg.t.Helper()
	rg.until(what, pred)
	for _, s := range rg.published() {
		if pred(s) {
			return s
		}
	}
	rg.t.Fatalf("%s: the snapshot until found is gone", what)
	return roster.Snapshot{}
}

// polledIn is a predicate: a Snapshot of run with every id listed, reachable
// and polled.
func polledIn(run uint64, ids ...string) func(roster.Snapshot) bool {
	return func(s roster.Snapshot) bool {
		if s.Run != run {
			return false
		}
		for _, id := range ids {
			if r := row(s, id); r == nil || !r.Polled || r.Status != roster.Reachable {
				return false
			}
		}
		return true
	}
}

// TestAHubRosterPollsOnlyWhileARunIsOpen: a roster OpenHub opened polls
// nothing until Resume — a tick before reads no registry and publishes
// nothing — then reads the registry and asks every host at once, its
// Snapshots saying the run and each row polled once its answer is back, the
// row's JSON value kept and its read time on the roster's clock; a tick in
// the run polls again; after Pause a tick does nothing, and the next run asks
// every host again over the connections it kept, its first Snapshot's rows
// not polled in it until their answers come. The negative controls are the
// counts: a paused tick that polled would read the registry, a run that did
// not keep its connections would dial again, and a row's Polled that were not
// the run's would hold at the new run's start.
func TestAHubRosterPollsOnlyWhileARunIsOpen(t *testing.T) {
	g := newRegistry(t)
	g.add(1, false)
	g.add(2, true)
	// Pause never waits, so the poller can still be on its way to the wake
	// when the next tick is sent: with both ready its select may take the
	// tick first, while the run is still open, and poll (a -race CI run did).
	// The test waits for the Pause to take effect before it ticks.
	paused := make(chan struct{}, 1)
	rg := newHubRigWith(t, g, func(o *roster.HubOptions) { o.Paused = func() { paused <- struct{}{} } })
	a, b := hostID(1), hostID(2)

	rg.tick() // paused: taken, and nothing done
	rg.r.Resume(1)
	s, _ := rg.until("run 1 polled both", polledIn(1, a, b))
	if got := rg.published(); got[0].Run != 1 {
		t.Fatalf("a Snapshot of run %d was published before the run was opened", got[0].Run)
	}
	if n := rg.reads.Load(); n != 1 {
		t.Fatalf("the registry was read %d times by run 1's first round: the paused tick polled", n)
	}
	for _, id := range []string{a, b} {
		r := row(s, id)
		var decoded protocol.SessionRow
		if err := json.Unmarshal(r.Raw, &decoded); err != nil || decoded.SessionID != r.Host.CrazeSessionID {
			t.Fatalf("host %s's raw row %s does not decode to its session (%v)", id, r.Raw, err)
		}
		if !r.ReadAt.Equal(rg.clk.now()) || r.Version != "0.0.0-fakehost" || r.Host.Protocol != 1 {
			t.Fatalf("host %s's row: read at %v (the clock says %v), version %q, protocol %d", id, r.ReadAt, rg.clk.now(), r.Version, r.Host.Protocol)
		}
	}
	if !strings.Contains(string(row(s, b).Raw), `"doing"`) && !strings.Contains(string(row(s, b).Raw), `"since"`) {
		t.Fatalf("the row-facts host's raw row lost its row facts: %s", row(s, b).Raw)
	}

	// A tick in the run polls again, and publishes the reads.
	n := len(rg.published())
	rg.clk.advance(time.Second)
	rg.tick()
	rg.until("the tick's reads", func(s roster.Snapshot) bool {
		ra, rb := row(s, a), row(s, b)
		return ra != nil && rb != nil && ra.ReadAt.Equal(rg.clk.now()) && rb.ReadAt.Equal(rg.clk.now())
	})
	if rg.reads.Load() != 2 || len(rg.published()) <= n {
		t.Fatalf("a tick in the run: %d registry reads, %d snapshots (was %d)", rg.reads.Load(), len(rg.published()), n)
	}

	rg.r.Pause()
	select {
	case <-paused:
	case <-time.After(step):
		t.Fatal("the Pause never took effect")
	}
	rg.tick() // paused again
	rg.r.Resume(2)
	if got := rg.first("run 2's first snapshot", func(s roster.Snapshot) bool { return s.Run == 2 }); row(got, a) == nil || row(got, a).Polled || row(got, b).Polled {
		t.Fatalf("run 2's first snapshot already says its rows were polled in it: %+v", got.Running)
	}
	rg.until("run 2 polled both", polledIn(2, a, b))
	if n := rg.reads.Load(); n != 3 {
		t.Fatalf("%d registry reads after run 2's first round, want 3: a paused tick polled", n)
	}
	if n := rg.dials.Load(); n != 2 {
		t.Fatalf("%d dials over two runs and two hosts: the pause did not keep the connections", n)
	}
}

// rawHost serves socket by hand as host id: hello, and sessions.list
// answered with sessions exactly as given (raw JSON, written as it stands).
func rawHost(t *testing.T, socket, id, sessions string) {
	t.Helper()
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	hello, err := json.Marshal(protocol.HelloResult{Protocol: 1, Endpoint: protocol.Endpoint{Kind: protocol.EndpointHost, HostID: id, CrazeVersion: "9.9.9-raw"}})
	if err != nil {
		t.Fatal(err)
	}
	serve := func(c net.Conn) {
		defer c.Close()
		lr := protocol.NewLineReader(c, 0)
		for {
			line, err := lr.ReadLine()
			if err != nil {
				return
			}
			var req protocol.Request
			if err := json.Unmarshal(line, &req); err != nil {
				return
			}
			result := string(hello)
			if req.Method == protocol.MethodSessionsList {
				result = `{"epoch":"` + id + `","cursor":7,"sessions":` + sessions + `}`
			}
			if _, err := c.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":` + result + "}\n")); err != nil {
				return
			}
		}
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
}

// TestAHubRosterKeepsEachRowAsItsHostSentIt (P4): the row's JSON value is
// kept — members this build does not know, at any depth, and their numbers
// as written — compacted, beside its decoding; a host whose row is not an
// object has none kept. The negative control is the list's roster, which
// keeps no JSON value at all.
func TestAHubRosterKeepsEachRowAsItsHostSentIt(t *testing.T) {
	g := newRegistry(t)
	// White space, but no newline: the reply is one line.
	const sent = `{ "sessionId": "session-1", "activity": "idle",	"future": {"nested": [1, 2.50, {"deep": "é<&>"}],` +
		` "big": 12345678901234567890}, "title": "kept" }`
	future, nullRow := filepath.Join(g.dir, "future"), filepath.Join(g.dir, "null")
	rawHost(t, future, hostID(1), `[`+sent+`]`)
	rawHost(t, nullRow, hostID(2), `[null]`)
	g.list(rundir.Entry{Protocol: 1, HostID: hostID(1), Socket: future, CrazeSessionID: sessionID(1), Ready: true})
	g.list(rundir.Entry{Protocol: 1, HostID: hostID(2), Socket: nullRow, CrazeSessionID: sessionID(2), Ready: true})
	rg := newHubRig(t, g)
	rg.r.Resume(1)
	s, _ := rg.until("both polled", polledIn(1, hostID(1), hostID(2)))

	want := `{"sessionId":"session-1","activity":"idle","future":{"nested":[1,2.50,{"deep":"é<&>"}],"big":12345678901234567890},"title":"kept"}`
	if got := string(row(s, hostID(1)).Raw); got != want {
		t.Fatalf("the row kept:\n got  %s\n want %s", got, want)
	}
	if sess := row(s, hostID(1)).Session; sess == nil || sess.Title != "kept" || sess.ID != sessionID(1) {
		t.Fatalf("the row's decoding: %+v", sess)
	}
	if r := row(s, hostID(2)); r.Raw != nil || r.Session == nil {
		t.Fatalf("a null row: raw %s, session %+v; want no raw value and the (empty) decoding", r.Raw, r.Session)
	}

	// Negative control: the list's roster keeps no value.
	lr := newRig(t, g, nil, nil)
	ls := lr.until("the list's roster reached the host", status(roster.Reachable, hostID(1)))
	if r := row(ls, hostID(1)); r.Raw != nil {
		t.Fatalf("the list's roster kept the raw row %s", r.Raw)
	}
}

// TestAHostInBackoffIsPolled: a host whose attempt failed is polled in its
// run (its Unreachable is the run's), and in a later run while it waits out
// its backoff — no attempt is due, so none is waited for. Past the backoff,
// a new run's row is not polled until the host is asked again — the negative
// control: Polled is not simply "failed once".
func TestAHostInBackoffIsPolled(t *testing.T) {
	g := newRegistry(t)
	g.list(rundir.Entry{Protocol: 1, HostID: hostID(1), Socket: filepath.Join(g.dir, "nobody"), CrazeSessionID: sessionID(1), Ready: true})
	rg := newHubRig(t, g)
	id := hostID(1)
	unreachablePolled := func(run uint64) func(roster.Snapshot) bool {
		return func(s roster.Snapshot) bool {
			r := row(s, id)
			return s.Run == run && r != nil && r.Status == roster.Unreachable && r.Polled
		}
	}
	rg.r.Resume(1)
	rg.until("run 1: unreachable, polled", unreachablePolled(1))
	rg.r.Pause()
	rg.r.Resume(2)
	if r := row(rg.first("run 2's first snapshot", func(s roster.Snapshot) bool { return s.Run == 2 }), id); !r.Polled {
		t.Fatalf("a host waiting out its backoff is not polled at its run's start: %+v", r)
	}

	rg.clk.advance(2 * time.Second) // past the first backoff (1 s)
	rg.r.Pause()
	rg.r.Resume(3)
	if r := row(rg.first("run 3's first snapshot", func(s roster.Snapshot) bool { return s.Run == 3 }), id); r.Polled {
		t.Fatalf("a host past its backoff is polled at its run's start, before it was asked: %+v", r)
	}
	rg.until("run 3: asked again, unreachable, polled", unreachablePolled(3))
}

// TestAPausedPollStartsNothing: a run paused mid-round starts no attempt —
// not even the rest of its round, as the attempts in flight end and leave
// their places — until the next run, which asks the host the paused round
// did not. Nine hosts, eight in flight: the first eight's dials hang until
// the test lets them fail, once the pause has taken effect; the ninth waits
// for a place. The negative control is the next run, which starts the
// ninth's at once.
func TestAPausedPollStartsNothing(t *testing.T) {
	g := newRegistry(t)
	for n := 1; n <= roster.MaxInFlight+1; n++ {
		g.list(rundir.Entry{Protocol: 1, HostID: hostID(n), Socket: filepath.Join(g.dir, hostID(n)), CrazeSessionID: sessionID(n), Ready: true})
	}
	last := hostID(roster.MaxInFlight + 1)
	var mu sync.Mutex
	started := map[string]int{}
	startedNow := func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range started {
			out[k] = v
		}
		return out
	}
	release, paused := make(chan struct{}), make(chan struct{}, 4)
	rg := newHubRigWith(t, g, func(o *roster.HubOptions) {
		o.Budget = step
		o.Dial = func(ctx context.Context, path string) (net.Conn, error) {
			select { // a dial that hangs until the test lets it fail
			case <-release:
			case <-ctx.Done():
			}
			return nil, errors.New("refused by the test")
		}
		o.Attempting = func(id string) {
			mu.Lock()
			started[id]++
			mu.Unlock()
		}
		o.Paused = func() { paused <- struct{}{} }
	})
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(step)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not within %v", what, step)
			}
			time.Sleep(time.Millisecond)
		}
	}
	rg.r.Resume(1)
	waitFor("the first eight attempts", func() bool { return len(startedNow()) == roster.MaxInFlight })
	rg.r.Pause()
	select {
	case <-paused:
	case <-time.After(step):
		t.Fatal("the pause never took effect")
	}
	close(release)
	rg.until("the eight failed", func(s roster.Snapshot) bool {
		for n := 1; n <= roster.MaxInFlight; n++ {
			if r := row(s, hostID(n)); r == nil || r.Status != roster.Unreachable {
				return false
			}
		}
		return true
	})
	if got := startedNow(); got[last] != 0 || len(got) != roster.MaxInFlight {
		t.Fatalf("attempts started: %v; a paused poll started the rest of its round", got)
	}
	rg.r.Resume(2)
	waitFor("the ninth host asked in the next run", func() bool { return startedNow()[last] == 1 })
}

// queuedHost serves socket by hand as host id: hello at once, and each
// sessions.list answered with the next sessions the test sends (raw JSON, a
// list) — held until it does.
func queuedHost(t *testing.T, socket, id string) chan<- string {
	t.Helper()
	answers := make(chan string)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	hello, err := json.Marshal(protocol.HelloResult{Protocol: 1, Endpoint: protocol.Endpoint{Kind: protocol.EndpointHost, HostID: id, CrazeVersion: "9.9.9-queued"}})
	if err != nil {
		t.Fatal(err)
	}
	serve := func(c net.Conn) {
		defer c.Close()
		lr := protocol.NewLineReader(c, 0)
		for {
			line, err := lr.ReadLine()
			if err != nil {
				return
			}
			var req protocol.Request
			if err := json.Unmarshal(line, &req); err != nil {
				return
			}
			result := string(hello)
			if req.Method == protocol.MethodSessionsList {
				select {
				case sessions := <-answers:
					result = `{"epoch":"` + id + `","cursor":1,"sessions":` + sessions + `}`
				case <-done:
					return
				}
			}
			if _, err := c.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":` + result + "}\n")); err != nil {
				return
			}
		}
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	return answers
}

// sessionRows is a sessions.list's sessions: one row, session's, titled title.
func sessionRows(session, title string) string {
	return `[{"sessionId":"` + session + `","activity":"idle","title":"` + title + `"}]`
}

// polledAs is a predicate: id polled in run 1 and reachable, its entry
// naming session.
func polledAs(id, session string) func(roster.Snapshot) bool {
	return func(s roster.Snapshot) bool {
		r := row(s, id)
		return polledIn(1, id)(s) && r.Host.CrazeSessionID == session
	}
}

// send hands the host its next answer, within step.
func send(t *testing.T, answers chan<- string, sessions string) {
	t.Helper()
	select {
	case answers <- sessions:
	case <-time.After(step):
		t.Fatal("the host was not asked for its row")
	}
}

// TestAHostWhoseSessionChangesIsConnectingUntilRead (r19 2): a host whose
// registry entry comes to name another craze session is published
// Connecting, with no row, no read and not polled — not its last session's
// row under the new session's id — until an attempt for the new session
// comes back, which reads that session's row. The negative control is the
// read before the change: the same host, the same connection, reachable with
// its row.
func TestAHostWhoseSessionChangesIsConnectingUntilRead(t *testing.T) {
	g := newRegistry(t)
	id := hostID(1)
	sock := filepath.Join(g.dir, "queued")
	answers := queuedHost(t, sock, id)
	e := rundir.Entry{Protocol: 1, HostID: id, Socket: sock, CrazeSessionID: sessionID(1), Ready: true}
	g.list(e)
	rg := newHubRigWith(t, g, func(o *roster.HubOptions) { o.Budget = step })
	rg.r.Resume(1)
	send(t, answers, sessionRows(sessionID(1), "one"))
	s, _ := rg.until("the first session read", polledIn(1, id))
	if r := row(s, id); r.Session == nil || r.Session.ID != sessionID(1) || !strings.Contains(string(r.Raw), sessionID(1)) || r.ReadAt.IsZero() {
		t.Fatalf("the control: the first session's row: %+v (raw %s)", r, r.Raw)
	}

	e.CrazeSessionID = sessionID(2)
	g.unlist(id)
	g.list(e)
	from := len(rg.published())
	rg.clk.advance(time.Second)
	rg.tick()
	s, _ = rg.until("the new session's entry read", func(s roster.Snapshot) bool {
		r := row(s, id)
		return r != nil && r.Host.CrazeSessionID == sessionID(2)
	})
	if r := row(s, id); r.Status != roster.Connecting || r.Raw != nil || r.Session != nil || !r.ReadAt.IsZero() || r.Polled {
		t.Fatalf("a host whose session changed, before its read: status %s, raw %s, session %+v, read at %v, polled %v; "+
			"want connecting, no row, no read, not polled", r.Status, r.Raw, r.Session, r.ReadAt, r.Polled)
	}

	send(t, answers, sessionRows(sessionID(2), "two"))
	s, _ = rg.until("the new session read", polledAs(id, sessionID(2)))
	if r := row(s, id); r.Session == nil || r.Session.ID != sessionID(2) || !strings.Contains(string(r.Raw), sessionID(2)) || !r.ReadAt.Equal(rg.clk.now()) {
		t.Fatalf("the new session's row: %+v (raw %s)", r, r.Raw)
	}
	for _, s := range rg.published()[from:] {
		if r := row(s, id); r.Host.CrazeSessionID == sessionID(2) && strings.Contains(string(r.Raw), sessionID(1)) {
			t.Fatalf("a Snapshot carried the last session's row under the new one's id: %s", r.Raw)
		}
	}
}

// TestAnAnswerForTheLastSessionIsNotTaken (r19 2): an attempt in flight as
// the host's entry comes to name another session — made for the last one —
// is not taken when it comes back: the host stays Connecting with no row, is
// asked again at once over the connection the attempt kept, and that answer,
// the new session's, is taken. The negative control is that late answer's
// content: the last session's row, which a roster that took it would publish
// as the new session's, reachable and fresh.
func TestAnAnswerForTheLastSessionIsNotTaken(t *testing.T) {
	g := newRegistry(t)
	id := hostID(1)
	sock := filepath.Join(g.dir, "queued")
	answers := queuedHost(t, sock, id)
	e := rundir.Entry{Protocol: 1, HostID: id, Socket: sock, CrazeSessionID: sessionID(1), Ready: true}
	g.list(e)
	attempts := make(chan string, 16)
	rg := newHubRigWith(t, g, func(o *roster.HubOptions) {
		o.Budget = step
		o.Attempting = func(id string) { attempts <- id }
	})
	attempted := func(what string) {
		t.Helper()
		select {
		case <-attempts:
		case <-time.After(step):
			t.Fatalf("%s: no attempt started within %v", what, step)
		}
	}
	rg.r.Resume(1)
	attempted("the first round")
	send(t, answers, sessionRows(sessionID(1), "one"))
	rg.until("the first session read", polledIn(1, id))

	// A round's attempt is in flight, held at the host, when the next round
	// reads the entry naming another session.
	rg.clk.advance(time.Second)
	rg.tick()
	attempted("the second round")
	e.CrazeSessionID = sessionID(2)
	g.unlist(id)
	g.list(e)
	from := len(rg.published())
	rg.clk.advance(time.Second)
	rg.tick()
	s, _ := rg.until("the new session's entry read", func(s roster.Snapshot) bool {
		r := row(s, id)
		return r != nil && r.Host.CrazeSessionID == sessionID(2)
	})
	if r := row(s, id); r.Status != roster.Connecting || r.Raw != nil || r.Session != nil {
		t.Fatalf("before any answer for the new session: %+v (raw %s)", r, r.Raw)
	}

	// The held attempt answers, with the last session's row.
	send(t, answers, sessionRows(sessionID(1), "one, late"))
	attempted("the host asked again for the new session")
	send(t, answers, sessionRows(sessionID(2), "two"))
	s, _ = rg.until("the new session read", func(s roster.Snapshot) bool {
		r := row(s, id)
		return polledAs(id, sessionID(2))(s) && r.Session != nil && r.Session.Title == "two"
	})
	if r := row(s, id); r.Session.ID != sessionID(2) || !strings.Contains(string(r.Raw), `"title":"two"`) {
		t.Fatalf("the new session's row: %+v (raw %s)", r, r.Raw)
	}
	// Every Snapshot before that read — published in order — held no row.
	for _, s := range rg.published()[from:] {
		r := row(s, id)
		if r.Session != nil && r.Session.Title == "two" {
			break
		}
		if r.Status != roster.Connecting || r.Raw != nil || r.Session != nil {
			t.Fatalf("the late answer for the last session was taken: status %s, raw %s", r.Status, r.Raw)
		}
	}
	if n := rg.dials.Load(); n != 1 {
		t.Fatalf("%d dials: the late answer's connection was not kept", n)
	}
}
