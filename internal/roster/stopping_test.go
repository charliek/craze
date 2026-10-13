package roster_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// The stopping mark as the poll reads it (plan 046 B4, #93): a host whose
// registry entry is marked stopping is not listed — whatever round reads it,
// and for good once seen — and a host whose row an attempt would move is read
// again first, gone in that round when its entry is missing or stopping. Every
// schedule is forced: the mark lands where the test's barrier (Attempting)
// puts it, after the round's registry read and before its attempt.

// settled is the latest Snapshot published by now — once a tick has run
// (rig.tick waits for it), whatever it published is in the slot: rg.last, or
// the one the slot holds.
func (rg *rig) settled() roster.Snapshot {
	select {
	case s, ok := <-rg.r.Updates():
		if ok {
			rg.last = s
		}
	default:
	}
	return rg.last
}

// newStepRig is newRig with each attempt's budget a step's, both shares, and
// tweak's seams over that: a host these schedules mean to answer always does,
// however a starved scheduler delays its attempt — it never goes unreachable,
// nor is its entry read again for a move the test did not make.
func newStepRig(t *testing.T, g *registry, index roster.Index, tweak func(*roster.TestOptions)) *rig {
	t.Helper()
	return newRig(t, g, index, func(o *roster.TestOptions) {
		o.DialBudget, o.ListBudget = step, step
		if tweak != nil {
			tweak(o)
		}
	})
}

// ctxStep is a context for one step's wait.
func ctxStep() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), step)
}

// entryOf is host id's entry in the registry, nil when it lists none.
func (g *registry) entryOf(id string) *rundir.Entry {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range g.entries {
		if e.HostID == id {
			return &e
		}
	}
	return nil
}

// gone is a predicate: id not listed.
func gone(id string) func(roster.Snapshot) bool {
	return func(s roster.Snapshot) bool { return row(s, id) == nil }
}

// moveRow makes h's next sessions.list row differ from its last: a foreign
// turn starts (fakehost's foreign_turn op, its log flushed).
func moveRow(t *testing.T, h *fakehost.Host) {
	t.Helper()
	if err := h.Do(json.RawMessage(`{"name":"foreign_turn","id":"wake-1","reason":"subagent_wake","running":true}`)); err != nil {
		t.Fatal(err)
	}
}

// TestAStoppingHostIsNotListed: a host whose entry is marked stopping when a
// round reads the registry is not listed — never dialled when it was so from
// the start, and gone in that round when it was listed before — and its
// session is a saved row in the same Snapshot. Once seen stopping it stays
// gone, even were its entry to say otherwise later (no host takes its mark
// back; host ids are never reused). The negative control: a new host with no
// mark, read in the same rounds, is listed.
func TestAStoppingHostIsNotListed(t *testing.T) {
	g := newRegistry(t)
	_, a := g.add(1, false)
	hb, b := g.add(2, false)
	g.markStopping(b.HostID, true)
	at := func(h int) time.Time { return time.Date(2026, 9, 29, h, 0, 0, 0, time.UTC) }
	idx := &memIndex{rows: []sessions.Row{
		{SessionID: "p-1", Provider: "cursor", CWD: "/w", Title: "one", CrazeID: sessionID(1), UpdatedAt: at(9)},
		{SessionID: "p-2", Provider: "cursor", CWD: "/w", Title: "two", CrazeID: sessionID(2), UpdatedAt: at(8)},
	}}
	rg := newStepRig(t, g, idx, nil)
	rg.waitApplied(1)
	s := rg.until("a reachable", status(roster.Reachable, a.HostID))
	if row(s, b.HostID) != nil {
		t.Fatalf("a host stopping at the first read is listed: %s", describe(s))
	}
	if got := titles(s.Saved); got != "two" {
		t.Fatalf("saved %q, want the stopping host's session", got)
	}
	if n := hb.OpenConns(); n != 0 {
		t.Fatalf("the stopping host was dialled: it holds %d connections", n)
	}

	g.markStopping(a.HostID, true)
	rg.tick()
	s = rg.until("a gone at the read after its mark", gone(a.HostID))
	if got := titles(s.Saved); got != "one|two" {
		t.Fatalf("saved %q once a is stopping, want both sessions", got)
	}

	g.markStopping(a.HostID, false)
	g.markStopping(b.HostID, false)
	_, c := g.add(3, false)
	rg.tick()
	rg.waitApplied(1)
	s = rg.until("the new host reachable", status(roster.Reachable, c.HostID))
	if row(s, a.HostID) != nil || row(s, b.HostID) != nil {
		t.Fatalf("a host seen stopping came back: %s", describe(s))
	}
}

// TestAHostWhoseRowMovesIsReadAgain: a round's registry read lists the host,
// serving; then, before the round's attempt (the test's barrier), the host
// changes. When the attempt's result would move the host's row — it answers
// another row, or no longer answers — its entry is read again, and a host
// whose entry is now stopping, or missing, is gone in that round; a stopping
// one stays gone. A host whose entry still says it serves keeps today's
// behaviour (the negative controls): its moved row is published, or its last
// row kept, unreachable. A result that moves nothing is taken without a read.
func TestAHostWhoseRowMovesIsReadAgain(t *testing.T) {
	type tc struct {
		name string
		// before is what happens between the round's read and its attempt.
		before func(t *testing.T, g *registry, h *fakehost.Host, id string)
		gone   bool
		status roster.Status
	}
	stop := func(t *testing.T, h *fakehost.Host) {
		t.Helper()
		ctx, cancel := ctxStep()
		defer cancel()
		_ = h.Close(ctx)
	}
	cases := []tc{
		{name: "stopping, its row moves", gone: true, before: func(t *testing.T, g *registry, h *fakehost.Host, id string) {
			g.markStopping(id, true)
			moveRow(t, h)
		}},
		{name: "stopping, it stops answering", gone: true, before: func(t *testing.T, g *registry, h *fakehost.Host, id string) {
			g.markStopping(id, true)
			stop(t, h)
		}},
		{name: "unregistered, it stops answering", gone: true, before: func(t *testing.T, g *registry, h *fakehost.Host, id string) {
			g.unlist(id)
			stop(t, h)
		}},
		{name: "serving, its row moves", status: roster.Reachable, before: func(t *testing.T, _ *registry, h *fakehost.Host, _ string) {
			moveRow(t, h)
		}},
		{name: "serving, it stops answering", status: roster.Unreachable, before: func(t *testing.T, _ *registry, h *fakehost.Host, _ string) {
			stop(t, h)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newRegistry(t)
			h, e := g.add(1, true)
			var mu sync.Mutex
			var before func()
			rg := newStepRig(t, g, nil, func(o *roster.TestOptions) {
				o.Attempting = func(string) {
					mu.Lock()
					f := before
					before = nil
					mu.Unlock()
					if f != nil {
						f()
					}
				}
			})
			rg.waitApplied(1)
			first := rg.until("reachable", status(roster.Reachable, e.HostID))
			if first.Running[0].Session.ForeignTurn {
				t.Fatal("setup: the host's first row has a foreign turn")
			}
			reads := g.rereadCount()
			if reads != 1 {
				t.Fatalf("setup: the first answer — Connecting to Reachable, a move — read the entry %d times, want once", reads)
			}

			// A round whose answer moves nothing reads nothing again.
			rg.tick()
			rg.waitApplied(1)
			if n := g.rereadCount(); n != reads {
				t.Fatalf("an unchanged answer read the entry again (%d reads, want %d)", n, reads)
			}

			mu.Lock()
			before = func() { c.before(t, g, h, e.HostID) }
			mu.Unlock()
			rg.tick()
			rg.waitApplied(1)
			if n := g.rereadCount(); n != reads+1 {
				t.Fatalf("a moved answer read the entry %d times, want once", n-reads)
			}
			// The result is published once taken (the applied barrier runs
			// as it is taken, before the Snapshot): the first Snapshot whose
			// row is not the round's before is the round's.
			s := rg.until("the round's result", func(s roster.Snapshot) bool {
				r := row(s, e.HostID)
				return r == nil || r.Status != roster.Reachable || r.Session.ForeignTurn
			})
			r := row(s, e.HostID)
			switch {
			case c.gone && r != nil:
				t.Fatalf("the host is still listed in the round of its change: %s", describe(s))
			case c.gone:
			case r == nil:
				t.Fatalf("a serving host left the roster: %s", describe(s))
			case r.Status != c.status:
				t.Fatalf("the serving host is %s, want %s", r.Status, c.status)
			case c.status == roster.Reachable && !r.Session.ForeignTurn:
				t.Fatalf("the serving host's moved row was not taken: %+v", *r.Session)
			case c.status == roster.Unreachable && r.Session == nil:
				t.Fatal("the unreachable host lost its last row")
			}
			if !c.gone {
				return
			}
			// Gone — seen stopping, or its entry missing — it is latched: a
			// read that lists it serving again — which no host does — does
			// not bring it back.
			if g.entryOf(e.HostID) == nil {
				g.list(e)
			}
			g.markStopping(e.HostID, false)
			rg.tick()
			if s := rg.settled(); row(s, e.HostID) != nil {
				t.Fatalf("a host gone for good came back: %s", describe(s))
			}
		})
	}
}

// TestAGoneHostIsForgottenOnceLongUnlisted: a host gone for good is
// remembered — and kept out of the roster — while the registry lists it and
// for a minute after it last did, then forgotten, so the poll's memory is
// bounded by the hosts that left in the last minute. (No host ever comes back
// under its id; one listed again here, after that, shows the memory went.)
func TestAGoneHostIsForgottenOnceLongUnlisted(t *testing.T) {
	g := newRegistry(t)
	_, e := g.add(1, false)
	g.markStopping(e.HostID, true)
	rg := newStepRig(t, g, nil, nil)
	if s := rg.settled(); row(s, e.HostID) != nil {
		t.Fatalf("a stopping host was listed: %s", describe(s))
	}
	g.unlist(e.HostID)
	rg.clk.advance(30 * time.Second)
	rg.tick()
	g.list(rundir.Entry{Protocol: 1, HostID: e.HostID, PID: e.PID, Socket: e.Socket, CrazeSessionID: e.CrazeSessionID,
		Provider: e.Provider, Workspace: e.Workspace, Ready: true})
	rg.tick()
	if s := rg.settled(); row(s, e.HostID) != nil {
		t.Fatalf("a host gone half a minute ago came back: %s", describe(s))
	}
	// The minute counts from the last read that listed it.
	g.unlist(e.HostID)
	rg.clk.advance(time.Minute)
	rg.tick()
	g.list(rundir.Entry{Protocol: 1, HostID: e.HostID, PID: e.PID, Socket: e.Socket, CrazeSessionID: e.CrazeSessionID,
		Provider: e.Provider, Workspace: e.Workspace, Ready: true})
	rg.tick()
	if s := rg.settled(); row(s, e.HostID) != nil {
		t.Fatalf("a host unlisted for just a minute came back: %s", describe(s))
	}
	g.unlist(e.HostID)
	rg.clk.advance(time.Minute + time.Nanosecond)
	rg.tick()
	g.list(rundir.Entry{Protocol: 1, HostID: e.HostID, PID: e.PID, Socket: e.Socket, CrazeSessionID: e.CrazeSessionID,
		Provider: e.Provider, Workspace: e.Workspace, Ready: true})
	rg.tick()
	rg.until("listed again once forgotten", status(roster.Reachable, e.HostID))
}

// TestAReadAgainThatFailsKeepsTheHost: an entry that cannot be read again
// says nothing — the host's moved row is taken, as before plan 046.
func TestAReadAgainThatFailsKeepsTheHost(t *testing.T) {
	g := newRegistry(t)
	h, e := g.add(1, true)
	rg := newStepRig(t, g, nil, func(o *roster.TestOptions) {
		o.Entry = func(string) (rundir.Entry, bool, error) { return rundir.Entry{}, false, errors.New("unreadable") }
	})
	rg.waitApplied(1)
	rg.until("reachable", status(roster.Reachable, e.HostID))
	moveRow(t, h)
	rg.tick()
	rg.waitApplied(1)
	rg.until("the moved row taken", func(s roster.Snapshot) bool {
		r := row(s, e.HostID)
		return r != nil && r.Session != nil && r.Session.ForeignTurn
	})
}
