package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
)

// A stopped session and the hub's roster (plan 046 B4, #93; B9 (i), (ix)):
// once a host's stop receipt is out, no roster notification upserts the host
// — its removal is the only word of it. The hosts are fake hosts that serve
// session.stop, registered in the test's registry, and each marks its entry
// stopping before its receipt as a real host does (fakehost's StoppingMark);
// each test's control is the same schedule against a host that writes no mark,
// a host from before plan 046, which the subscriber is told of after its
// receipt (the ghost #93 is about). Every schedule is forced: the poll's
// ticks, its clock and the flush spacing are the test's, and where the stop
// must land inside a round, the round's attempt is held (rosterAttempting).

// stoppingHost serves an in-process fake host listed in env's registry as
// host n — session.stop served, with the stopping mark when marked, and the
// row facts — and its socket; it is closed and unlisted when the test ends.
func stoppingHost(t *testing.T, env rundir.Env, n int, marked bool) (*fakehost.Host, string) {
	t.Helper()
	h, err := fakehost.New(fakehost.Options{HostID: hostOf(n), CrazeSessionID: sessionOf(n), Workspace: "/work/" + hostOf(n),
		Stop: true, StoppingMark: marked, RowFacts: true})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := h.Register(env)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- h.Serve(reg.Listener()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), step)
		defer cancel()
		_ = h.Close(ctx)
		<-served
		_ = reg.Close()
	})
	return h, reg.Socket()
}

// attemptGate holds one attempt of the poll's — the next for its armed host —
// on the poll's goroutine, after its round's registry read and before the
// attempt itself, until released (hooks.rosterAttempting).
type attemptGate struct {
	mu      sync.Mutex
	armed   string
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
}

func newAttemptGate() *attemptGate {
	return &attemptGate{arrived: make(chan struct{}), release: make(chan struct{})}
}

func (g *attemptGate) attempting(id string) {
	g.mu.Lock()
	if g.armed == "" || g.armed != id {
		g.mu.Unlock()
		return
	}
	g.armed = ""
	g.mu.Unlock()
	close(g.arrived)
	<-g.release
}

// arm holds the next attempt for host id.
func (g *attemptGate) arm(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = id
}

// open lets the held attempt go; idempotent.
func (g *attemptGate) open() { g.once.Do(func() { close(g.release) }) }

// await waits for the held attempt to arrive.
func (g *attemptGate) await(t *testing.T) {
	t.Helper()
	select {
	case <-g.arrived:
	case <-time.After(step):
		t.Fatal("the poll's attempt never came")
	}
}

// stopOver sends session.stop to the host at socket and reads its receipt;
// for a host that marks, its entry says stopping by then.
func stopOver(t *testing.T, env rundir.Env, socket string, n int, marked bool) {
	t.Helper()
	c := dialPeer(t, socket)
	if m := c.call(protocol.MethodSessionStop, protocol.StopParams{SessionID: sessionOf(n), CommandID: "1"}); m.Error != nil {
		t.Fatalf("session.stop: %+v", m.Error)
	}
	e, ok, err := rundir.HostEntry(env, hostOf(n))
	if err != nil || !ok || e.Stopping != marked {
		t.Fatalf("at the receipt the host's entry is %+v (ok %v, %v), want stopping %v", e, ok, err, marked)
	}
}

// rowsSaid is each upserted row's host, status and activity, for a log.
func rowsSaid(rows []protocol.RosterRow) []string {
	var out []string
	for _, r := range rows {
		var row struct {
			Activity string `json:"activity"`
		}
		_ = json.Unmarshal(r.Row, &row)
		out = append(out, fmt.Sprintf("%s %s %s approximate:%v", r.HostID, r.Status, row.Activity, r.Approximate))
	}
	return out
}

// upserted says n holds an upsert of host id.
func upserted(n protocol.RosterParams, id string) bool { return rowOf(n.Upserts, id) != nil }

// TestAStoppedHostIsNotUpsertedAfterItsReceipt is B9 (i): a round whose
// registry read precedes the stop — the host listed, serving — and whose
// attempt follows the stop's receipt and its engine's close. The attempt's
// result moves the row (the session is closing, or no longer answers), so the
// poll reads the host's entry again: marked stopping, the host is gone in that
// round, and the subscriber's next notification removes it — no upsert after
// the receipt. The control, a host that writes no mark: the same round
// upserts the stopped host after its receipt, as every host did before plan
// 046.
func TestAStoppedHostIsNotUpsertedAfterItsReceipt(t *testing.T) {
	for _, marked := range []bool{true, false} {
		name := "a host that marks"
		if !marked {
			name = "an older host, no mark (the control)"
		}
		t.Run(name, func(t *testing.T) {
			env := testEnv(t)
			gate := newAttemptGate()
			rg := newRosterRig(t, env, rigOpts{clock: true, gate: true, hk: func(hk *hooks) { hk.rosterAttempting = gate.attempting }})
			// After the rig's own cleanup is registered, so it runs first: a
			// held attempt would hold the poll's Close.
			t.Cleanup(gate.open)
			setVar(t, &listWait, step)
			h, socket := stoppingHost(t, env, 1, marked)
			id := hostOf(1)
			p := dialPeer(t, rg.sock)
			sub := p.subscribe()
			if r := rowOf(sub.Sessions, id); r == nil || r.Status != protocol.RosterReachable {
				t.Fatalf("subscribed to %+v, want the host reachable", sub.Sessions)
			}

			gate.arm(id)
			from := rg.snaps.mark()
			rg.tick()
			gate.await(t) // the round has read the registry: the host serving
			stopOver(t, env, socket, 1, marked)
			if err := h.RunStop(); err != nil { // the engine's close
				t.Fatal(err)
			}
			gate.open()
			rg.applied("the held attempt's result", from, func(s roster.Snapshot) bool {
				r := snapRow(s, id)
				return r == nil || r.Status != roster.Reachable || r.Session == nil || r.Session.Activity != "idle"
			})
			n := p.roster()
			t.Logf("after the receipt: upserts %v, removes %v", rowsSaid(n.Upserts), n.Removes)
			switch {
			case marked && (upserted(n, id) || !slices.Equal(n.Removes, []string{id})):
				t.Fatalf("after the receipt the subscriber was told %+v, want the host's removal alone", n)
			case !marked && !upserted(n, id):
				t.Fatalf("the control: after the receipt the subscriber was told %+v, want the old host upserted", n)
			}
			if !marked {
				return
			}
			// And nothing after: the next round's read lists it stopping.
			rg.tick()
			p.nothing("the round after the stopped host's removal")
		})
	}
}

// TestAStopMidTurnIsNotUpsertedAfterItsReceipt is B9 (ix): a session stopped
// while a turn runs, its subscriber holding the working row. The stop's
// receipt, then the engine's close — which ends the turn, closing — then the
// next round: its registry read follows the mark, so the host is not listed
// and the subscriber's next notification removes it; the turn's ending is
// never upserted. The control, a host that writes no mark: the round upserts
// its ended turn after the receipt.
func TestAStopMidTurnIsNotUpsertedAfterItsReceipt(t *testing.T) {
	for _, marked := range []bool{true, false} {
		name := "a host that marks"
		if !marked {
			name = "an older host, no mark (the control)"
		}
		t.Run(name, func(t *testing.T) {
			env := testEnv(t)
			rg := newRosterRig(t, env, rigOpts{clock: true, gate: true})
			setVar(t, &listWait, step)
			h, socket := stoppingHost(t, env, 1, marked)
			id := hostOf(1)
			p := dialPeer(t, rg.sock)
			p.subscribe()

			// A turn that runs until the stop ends it.
			h.HangNext()
			c := dialPeer(t, socket)
			if m := c.call(protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: sessionOf(1), CommandID: "1",
				Text: "hold on", Mode: protocol.PromptQueue}); m.Error != nil {
				t.Fatalf("session.prompt: %+v", m.Error)
			}
			from := rg.snaps.mark()
			rg.tick()
			rg.applied("the working row", from, func(s roster.Snapshot) bool {
				r := snapRow(s, id)
				return r != nil && r.Session != nil && r.Session.Activity == "working"
			})
			if n := p.roster(); !upserted(n, id) {
				t.Fatalf("the running turn: %+v, want the host upserted working", n)
			}

			stopOver(t, env, socket, 1, marked)
			if err := h.RunStop(); err != nil {
				t.Fatal(err)
			}
			var row json.RawMessage
			from = rg.snaps.mark()
			rg.tick()
			rg.applied("the round after the stop", from, func(s roster.Snapshot) bool {
				r := snapRow(s, id)
				if r != nil {
					row = r.Raw
				}
				return r == nil || r.Status != roster.Reachable || r.Session == nil || r.Session.Activity != "working"
			})
			rg.flush.open(t)
			n := p.roster()
			t.Logf("after the receipt: upserts %v, removes %v", rowsSaid(n.Upserts), n.Removes)
			switch {
			case marked && (upserted(n, id) || !slices.Equal(n.Removes, []string{id})):
				t.Fatalf("after the receipt the subscriber was told %+v, want the host's removal alone", n)
			case !marked && !upserted(n, id):
				t.Fatalf("the control: after the receipt the subscriber was told %+v (last row %s), want the old host upserted", n, row)
			}
		})
	}
}

// TestTheListSeedSkipsAStoppingHost: the session list's seed (listroster) —
// the registry's hosts as rows before the hub answers — leaves a host marked
// stopping out, and its session is a saved row from the first Snapshot; a
// serving host beside it is seeded. And hub.Direct (`craze ps --no-hub`),
// one run of the hub's poll, lists the serving host alone.
func TestTheListSeedSkipsAStoppingHost(t *testing.T) {
	env := testEnv(t)
	hostIn(t, env, 1)
	h2, _ := stoppingHost(t, env, 2, true)
	if err := h2.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	l := &listRows{hosts: func() ([]rundir.Entry, error) { return rundir.Hosts(env) }, rows: map[string]hubRow{},
		saved: roster.NewSaved(nil, nil)}
	l.seed()
	if _, ok := l.rows[hostOf(1)]; !ok || len(l.rows) != 1 {
		t.Fatalf("the seed's rows: %v, want the serving host alone", slices.Collect(maps.Keys(l.rows)))
	}

	setVar(t, &directBudget, step)
	res, err := Direct(stepContext(t), env, listWho)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res.Sessions); !slices.Equal(got, []string{hostOf(1)}) {
		t.Fatalf("Direct listed %v, want the serving host alone", got)
	}
}

// TestAStaleRowFreshenedAfterTheMarkIsNotUpserted (review c3 4): a row gone
// stale — its last read over freshFor old, the subscriber told it is
// approximate — and then a round whose registry read lists the host, serving,
// and whose attempt follows the host's mark, its row unchanged. The answer
// would make the row fresh again, which the hub publishes as a change; on the
// hub's roster every answer's entry is read again before it is taken, so it
// says stopping, and the host is removed — never upserted fresh after its
// mark. The control, the same schedule with no mark: the host is upserted
// fresh.
func TestAStaleRowFreshenedAfterTheMarkIsNotUpserted(t *testing.T) {
	for _, marked := range []bool{true, false} {
		name := "marked"
		if !marked {
			name = "no mark (the control)"
		}
		t.Run(name, func(t *testing.T) {
			m := newMemHosts(t)
			id := m.add(1, memRow(1, "one"))
			gate := newAttemptGate()
			rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, gate: true, hk: func(hk *hooks) {
				installMem(t, hk, m)
				hk.rosterAttempting = gate.attempting
			}})
			t.Cleanup(gate.open)
			// The subscription waits for the first round's answer, not the
			// production second: a starved attempt never answers connecting.
			setVar(t, &listWait, step)
			p := dialPeer(t, rg.sock)
			if sub := p.subscribe(); rowOf(sub.Sessions, id) == nil || rowOf(sub.Sessions, id).Approximate {
				t.Fatalf("subscribed to %+v, want the host fresh", sub.Sessions)
			}
			rg.clk.advance(freshFor + time.Nanosecond)
			dialPeer(t, rg.sock).list() // the hub reads its rows again: stale
			if stale := p.roster(); len(stale.Upserts) != 1 || !stale.Upserts[0].Approximate {
				t.Fatalf("the stale flip: %+v", stale)
			}

			gate.arm(id)
			from := rg.snaps.mark()
			rg.tick()
			gate.await(t) // the round has read the registry: the host serving
			if marked {
				m.mu.Lock()
				e := m.entries[id]
				e.Stopping = true
				m.entries[id] = e
				m.mu.Unlock()
			}
			gate.open()
			rg.applied("the attempt's result", from, func(s roster.Snapshot) bool {
				r := snapRow(s, id)
				return r == nil || r.ReadAt.Equal(rg.clk.now())
			})
			rg.flush.open(t)
			n := p.roster()
			t.Logf("after the mark: upserts %v, removes %v", rowsSaid(n.Upserts), n.Removes)
			switch {
			case marked && (upserted(n, id) || !slices.Equal(n.Removes, []string{id})):
				t.Fatalf("after the mark the subscriber was told %+v, want the host's removal alone", n)
			case !marked && (!upserted(n, id) || rowOf(n.Upserts, id).Approximate):
				t.Fatalf("the control: the subscriber was told %+v, want the host upserted fresh", n)
			}
		})
	}
}

// TestAnAnswerAfterTheMarkIsNeverTaken (review c3r2 1): the window between
// an answer's taking and the hub's publication of it. The host's last read
// is fresh; a round whose registry read lists it serving holds its attempt;
// the host marks; the attempt answers its row unchanged while that last read
// is still within freshFor; and then — after the poll has taken the answer,
// before it publishes — the clock passes freshFor of the last read and the
// hub reads its rows again, as a list does, so the row it holds is published
// approximate. Were the answer taken, its fresh read would be published as
// the row going fresh again: an upsert made from what the hub saw after the
// mark. Every answer's entry is read again before it is taken, so the answer
// is never taken: the host is removed. The control, the same schedule with
// no mark: the host is upserted fresh.
func TestAnAnswerAfterTheMarkIsNeverTaken(t *testing.T) {
	for _, marked := range []bool{true, false} {
		name := "marked"
		if !marked {
			name = "no mark (the control)"
		}
		t.Run(name, func(t *testing.T) {
			m := newMemHosts(t)
			id := m.add(1, memRow(1, "one"))
			gate := newAttemptGate()
			var (
				hookMu sync.Mutex
				onTake func()
			)
			rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, gate: true, hk: func(hk *hooks) {
				installMem(t, hk, m)
				hk.rosterAttempting = gate.attempting
				hk.rosterResult = func(string, error) {
					hookMu.Lock()
					f := onTake
					onTake = nil
					hookMu.Unlock()
					if f != nil {
						f()
					}
				}
			}})
			t.Cleanup(gate.open)
			setVar(t, &listWait, step) // as above: the first round's answer, not 1 s
			p := dialPeer(t, rg.sock)
			p.subscribe()
			// The subscriber's first notification is written at once; every
			// later one waits for the flush gate, as this test opens it.
			rg.clk.advance(freshFor + time.Nanosecond)
			dialPeer(t, rg.sock).list()
			if stale := p.roster(); len(stale.Upserts) != 1 || !stale.Upserts[0].Approximate {
				t.Fatalf("the stale flip: %+v", stale)
			}
			from := rg.snaps.mark()
			rg.tick()
			rg.applied("read again", from, readAt(rg.clk.now(), id))
			rg.flush.open(t)
			if fresh := p.roster(); len(fresh.Upserts) != 1 || fresh.Upserts[0].Approximate {
				t.Fatalf("read fresh again: %+v", fresh)
			}
			read := rg.clk.now()

			rg.clk.advance(freshFor - 200*time.Millisecond)
			answered := rg.clk.now()
			gate.arm(id)
			from = rg.snaps.mark()
			rg.tick()
			gate.await(t) // the round has read the registry: the host serving
			if marked {
				m.mu.Lock()
				e := m.entries[id]
				e.Stopping = true
				m.entries[id] = e
				m.mu.Unlock()
			}
			hookMu.Lock()
			onTake = func() {
				// Taken, not yet published: the last read goes stale, and the
				// hub reads its rows again (a list's reevaluation).
				rg.clk.advance(300 * time.Millisecond)
				rg.h.rs.mu.Lock()
				rg.h.rs.reevaluateLocked(rg.h.rs.now())
				rg.h.rs.mu.Unlock()
			}
			hookMu.Unlock()
			gate.open()
			rg.applied("the attempt's result", from, func(s roster.Snapshot) bool {
				r := snapRow(s, id)
				return r == nil || r.ReadAt.Equal(answered)
			})
			if rg.clk.now().Sub(read) <= freshFor || answered.Sub(read) > freshFor {
				t.Fatalf("setup: read at %v, answered at %v, published at %v", read, answered, rg.clk.now())
			}
			rg.flush.open(t)
			n := p.roster()
			t.Logf("after the mark: upserts %v, removes %v", rowsSaid(n.Upserts), n.Removes)
			switch {
			case marked && (upserted(n, id) || !slices.Equal(n.Removes, []string{id})):
				t.Fatalf("after the mark the subscriber was told %+v, want the host's removal alone", n)
			case !marked && (!upserted(n, id) || rowOf(n.Upserts, id).Approximate):
				t.Fatalf("the control: the subscriber was told %+v, want the host upserted fresh", n)
			}
		})
	}
}
