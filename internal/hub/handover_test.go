package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// The list's handover from the hub to its poller (r30 4, r34 2, 4, 5, r39 1,
// r42, X57): a session that answers with no row the list can draw — its row
// too large for the hub to forward, unreadable, or null — sends the list to
// its poller, which reads every host's whole row itself. Once the hub's rows
// were published, the hub's last whole Snapshot stands until the poller has
// read the registry and heard from every session the hub listed (or the cap
// has passed), and from then on the poller's Snapshots are shown whole. The
// hosts are internal/fakehost's, in a registry of the test's; the hub is a
// script (scriptHub) answering what a real hub would for them (Direct), or
// the poller is (scriptPoller). A host the test keeps the poller from asking
// is one whose registry entry names no session yet (gate): the poller lists
// it connecting and asks it nothing, so no attempt's budget runs while the
// test looks. Each test here is also run under a 5% CPU quota.

// handover is two real hosts in a registry of the test's, an index, and what
// a hub would say of them (handoverHosts).
type handover struct {
	x *listIndex
	// direct is the hub's roster of the two hosts as it builds it now
	// (Direct): host 1's row dropped over the row bound, host 2's forwarded.
	direct protocol.HubSessionsListResult
	// asking is host 1's row as the hub forwarded it before its catalog grew
	// past the bound: a real body, its permission ask open; summary is that
	// ask's summary as the body says it.
	asking  protocol.RosterRow
	summary string
	// hosts and regs are host 1's and host 2's, and their registry entries.
	hosts [2]*fakehost.Host
	regs  [2]*rundir.Host
}

// askPerm opens host h's permission ask, perm-1.
func askPerm(t *testing.T, h *fakehost.Host) {
	t.Helper()
	if err := h.Do(json.RawMessage(`{"name":"permission","id":"perm-1","tool":"Run make",` +
		`"options":[{"optionId":"a","name":"Allow","kind":"allow_once"}]}`)); err != nil {
		t.Fatal(err)
	}
}

// handoverHosts is a registry of two real hosts in env and an index: host
// 1's session waiting for the user — a permission ask open — with a row too
// large for the hub to forward (a model catalog of its own past the hub's
// row bound), host 2 an ordinary one; the index has host 1's session (running,
// so never saved) and a saved one, "five". What a hub says of them is read as
// a hub builds it (Direct) — and host 1's row before its catalog grew from a
// twin of it, the same host and session with no catalog of its own, in a
// registry of its own.
func handoverHosts(t *testing.T, env rundir.Env) handover {
	t.Helper()
	models := make([]agent.ModelInfo, 300)
	for i := range models {
		models[i] = agent.ModelInfo{ID: fmt.Sprintf("model-%03d", i), Name: strings.Repeat("a model of a long name ", 4)}
	}
	opts := fakehost.Options{HostID: hostOf(1), CrazeSessionID: sessionOf(1), Workspace: "/work/" + hostOf(1), RowFacts: true}
	var ho handover
	big := opts
	big.Models = models
	ho.hosts[0], ho.regs[0] = hostWith(t, env, big)
	askPerm(t, ho.hosts[0])
	ho.hosts[1], ho.regs[1] = hostWith(t, env, fakehost.Options{HostID: hostOf(2), CrazeSessionID: sessionOf(2),
		Workspace: "/work/" + hostOf(2), RowFacts: true})
	setVar(t, &directBudget, step)
	var err error
	if ho.direct, err = Direct(stepContext(t), env, listWho); err != nil {
		t.Fatal(err)
	}
	one, two := rowOf(ho.direct.Sessions, hostOf(1)), rowOf(ho.direct.Sessions, hostOf(2))
	if one == nil || two == nil || one.Status != protocol.RosterReachable || len(one.Row) != 0 || len(two.Row) == 0 {
		t.Fatalf("the hub's roster of the two hosts: %+v", ho.direct.Sessions)
	}

	twinEnv := testEnv(t)
	twinHost, _ := hostWith(t, twinEnv, opts)
	askPerm(t, twinHost)
	twin, err := Direct(stepContext(t), twinEnv, listWho)
	if err != nil {
		t.Fatal(err)
	}
	ho.asking = *rowOf(twin.Sessions, hostOf(1))
	row, ok, err := ho.asking.SessionRow()
	if !ok || err != nil || row.PendingAsks != 1 || row.HeadAsk == nil || row.HeadAsk.Summary == "" {
		t.Fatalf("host 1's row before its catalog grew: %s (%v)", ho.asking.Row, err)
	}
	ho.summary = row.HeadAsk.Summary
	ho.x = newListIndex(t, savedRow(1, "one, running", 9), savedRow(5, "five", 8))
	return ho
}

// hostWith serves an in-process fake host with o, listed in env's registry
// until the test ends: the host and its registry entry.
func hostWith(t *testing.T, env rundir.Env, o fakehost.Options) (*fakehost.Host, *rundir.Host) {
	t.Helper()
	h, err := fakehost.New(o)
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
	return h, reg
}

// gate keeps a poller from asking the host reg lists: its entry names no
// craze session — a host before its engine is up — so a poller lists it
// connecting, and asks it nothing (roster's launch), until ungate gives the
// session back. No attempt is in flight meanwhile, so no attempt's budget can
// run out while a test looks.
func gate(t *testing.T, reg *rundir.Host) {
	t.Helper()
	if err := reg.Update(func(e *rundir.Entry) { e.CrazeSessionID = "" }); err != nil {
		t.Fatal(err)
	}
}

// ungate gives reg's entry its session, id, back: the poller asks it at its
// next round.
func ungate(t *testing.T, reg *rundir.Host, id string) {
	t.Helper()
	if err := reg.Update(func(e *rundir.Entry) { e.CrazeSessionID = id }); err != nil {
		t.Fatal(err)
	}
}

// polledWhole is a predicate: a Snapshot of the list's poller — its rows'
// sockets the registry's — with both hosts answering, host 1 waiting for the
// user by the poller's own read of it (Polled), and "five" alone saved.
func polledWhole(s roster.Snapshot) bool {
	one, two := hostRow(s, hostOf(1)), hostRow(s, hostOf(2))
	return one != nil && two != nil && one.Host.Socket != "" && two.Host.Socket != "" &&
		one.Status == roster.Reachable && two.Status == roster.Reachable && one.Session != nil && one.Polled &&
		one.State() == roster.StateNeedsYou && slices.Equal(savedTitles(s), []string{"five"})
}

// asks says r is host 1 waiting for the user with its ask: needing you, the
// ask's summary summary.
func asks(r *roster.Row, summary string) bool {
	return r != nil && r.Session != nil && r.State() == roster.StateNeedsYou && r.Session.HeadAsk != nil &&
		r.Session.HeadAsk.Summary == summary
}

// fromHub says every running row of s is the hub's: no socket.
func fromHub(s roster.Snapshot) bool {
	for _, r := range s.Running {
		if r.Host.Socket != "" {
			return false
		}
	}
	return len(s.Running) > 0
}

// whole checks every Snapshot published from the from-th on is the list
// whole and of one source: both hosts running, "five" alone saved, no
// registry error; a Snapshot that lists the hub's rows lists only the hub's
// (host 1 by its own body, its ask open) and one of the poller's only the
// poller's — never the two mixed (X57). From the first Snapshot of the hub's
// rows on, host 1 is waiting for you with its ask in every one — never
// connecting — through the switch to the poller (r39 1).
func whole(t *testing.T, rg *listRig, from int, summary string) {
	t.Helper()
	hubbed := false
	for i, s := range rg.published(from) {
		one, two := hostRow(s, hostOf(1)), hostRow(s, hostOf(2))
		switch {
		case one == nil || two == nil || len(s.Running) != 2:
			t.Fatalf("Snapshot %d does not list both hosts: %s", from+i, listed(s))
		case (one.Host.Socket == "") != (two.Host.Socket == ""):
			t.Fatalf("Snapshot %d mixes the hub's rows and the poller's: %s", from+i, listed(s))
		case one.Host.Socket == "" && !asks(one, summary):
			t.Fatalf("Snapshot %d lists host 1 by a hub row that is not its own body: %s", from+i, listed(s))
		case !slices.Equal(savedTitles(s), []string{"five"}) || s.RegistryErr != nil:
			t.Fatalf("Snapshot %d is not the list whole: %s", from+i, listed(s))
		}
		hubbed = hubbed || fromHub(s)
		if hubbed && !asks(one, summary) {
			t.Fatalf("Snapshot %d, after the hub listed host 1 waiting for you, does not: %s (state %v)", from+i, listed(s), one.State())
		}
	}
}

// nullRow is host n's reachable roster row with a body of JSON null — what no
// hub of this build sends (the schema refuses it), a malformed one's.
func nullRow(n int) protocol.RosterRow {
	r := rosterRowOf(n, "")
	r.Row = json.RawMessage("null")
	return r
}

// TestAReachableRowWithNoUsableBodyIsThePollers (r30 4, r34 4, 5, r39 1, 5,
// X57): a row the hub reached but sent no body the list can draw for —
// dropped over the hub's row bound (host 1's genuinely is: its catalog),
// unreadable, or null — is a session that answers and that nothing the hub
// sent says the state of: here it waits for the user. Whether the row comes
// in the first reply, in a later notification or in a reseed's reply after a
// reset, the roster closes the subscription and runs its poller, and the
// poller's own fresh Snapshot lists both hosts with their sockets, host 1
// needing you, "five" alone saved; through the switch every Snapshot is the
// list whole (whole). Where the hub had listed host 1 — its real body, its
// ask open — both hosts are kept from the poller (gate) until the poller's
// Snapshot with both connecting has been handed to the list, and then let go:
// meanwhile the hub's last Snapshot stands, host 1 waiting for you with its
// ask, and once the poller has heard from both its own Snapshot replaces it,
// whole. The negative controls: the connecting and unreachable rows with no
// body are the hub's, and stay so (TestARowWithNoBodyIsNamedByItsRegistryEntry);
// a poll that published nothing would never show host 1 needing you; a
// handover that showed the poller's Snapshot at its first good read would
// show host 1 connecting.
func TestAReachableRowWithNoUsableBodyIsThePollers(t *testing.T) {
	odd := rosterRowOf(1, "")
	odd.Row = json.RawMessage(`{"sessionId":"session-1","title":7}`)
	oddTitle := rosterRowOf(1, "")
	oddTitle.Row = json.RawMessage(`{"title":7}`)
	for _, tc := range []struct {
		name string
		// bad is host 1's row the list cannot draw; how is where it comes.
		bad       func(direct protocol.RosterRow) protocol.RosterRow
		how       string
		offSchema bool
	}{
		{"dropped in the reply", func(d protocol.RosterRow) protocol.RosterRow { return d }, "reply", false},
		{"unreadable in the reply", func(protocol.RosterRow) protocol.RosterRow { return odd }, "reply", false},
		{"null in the reply", func(protocol.RosterRow) protocol.RosterRow { return nullRow(1) }, "reply", true},
		{"dropped in a notification", func(d protocol.RosterRow) protocol.RosterRow { return d }, "notification", false},
		{"null in a notification", func(protocol.RosterRow) protocol.RosterRow { return nullRow(1) }, "notification", true},
		{"dropped in a reseed", func(d protocol.RosterRow) protocol.RosterRow { return d }, "reseed", false},
		{"unreadable in a reseed", func(protocol.RosterRow) protocol.RosterRow { return oddTitle }, "reseed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			ho := handoverHosts(t, env)
			bad, two := tc.bad(*rowOf(ho.direct.Sessions, hostOf(1))), *rowOf(ho.direct.Sessions, hostOf(2))
			first := []protocol.RosterRow{ho.asking, two}
			if tc.how == "reply" {
				first = []protocol.RosterRow{bad, two}
			}
			h := newScriptHub(t, hostOf(0xa), first...)
			if tc.offSchema {
				h.writeOffSchema()
			}
			var (
				mu       sync.Mutex
				observed *observedPoller
			)
			rg := newListRig(t, env, ho.x, func(o *listOptions) {
				o.seedWait, o.reseedWait = step, step
				o.ensure = fixed(h.sock)
				o.holdCap = func() <-chan time.Time { return nil }
				poller := o.poller
				o.poller = func() listPoller {
					mu.Lock()
					defer mu.Unlock()
					observed = observe(poller())
					return observed
				}
			})
			sub := h.next()
			if tc.how != "reply" {
				rg.mode(ModeHub)
				rg.until("the hub's row of host 1, waiting for you", 0, func(s roster.Snapshot) bool {
					r := hostRow(s, hostOf(1))
					return r != nil && r.Host.Socket == "" && asks(r, ho.summary)
				})
				for _, reg := range ho.regs {
					gate(t, reg)
				}
			}
			switch tc.how {
			case "notification":
				sub.roster(100, []protocol.RosterRow{bad})
			case "reseed":
				h.set(hostOf(0xa), bad, two)
				sub.reset(protocol.ResetOmitted)
				sub.closedByClient("the reset")
				sub = h.next()
			}
			sub.closedByClient(tc.how)
			rg.mode(ModePoller)
			if tc.how != "reply" {
				mu.Lock()
				o := observed
				mu.Unlock()
			connecting:
				for {
					select {
					case s := <-o.seen:
						if r := hostRow(s, hostOf(1)); r != nil && r.Status == roster.Connecting && r.Session == nil && s.RegistryErr == nil {
							break connecting
						}
					case <-time.After(step):
						t.Fatal("the poller's Snapshot with both hosts connecting never came")
					}
				}
				ungate(t, ho.regs[0], sessionOf(1))
				ungate(t, ho.regs[1], sessionOf(2))
			}
			rg.until("the poller's own rows", 0, polledWhole)
			whole(t, rg, 0, ho.summary)
		})
	}
}

// scriptPoller is a poller whose Snapshots the test hands the list itself:
// its channel has no slot, so a hand returns once the list has taken it —
// and a second hand returns once the first has been dealt with.
type scriptPoller struct {
	out chan roster.Snapshot
}

func (p *scriptPoller) Updates() <-chan roster.Snapshot { return p.out }

func (p *scriptPoller) Close() {}

// hand gives the list s.
func (p *scriptPoller) hand(t *testing.T, s roster.Snapshot) {
	t.Helper()
	select {
	case p.out <- s:
	case <-time.After(step):
		t.Fatal("the list did not take the poller's Snapshot")
	}
}

// polledRow is host n's row as a poller has it: its registry entry's host,
// serving craze session session, at status st, with sess (nil: not answered).
func polledRow(n int, session string, st roster.Status, sess *roster.Session) roster.Row {
	return roster.Row{Host: roster.HostOf(regEntry(n, session)), Status: st, Session: sess}
}

// askingBody is host n's row body with an ask open, its summary summary.
func askingBody(n int, title, summary string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"sessionId":%q,"activity":"idle","title":%q,"pendingAsks":1,`+
		`"headAsk":{"id":"perm-1","kind":"permission","label":"permission %s","summary":%q},"capabilities":{"rowFacts":true}}`,
		sessionOf(n), title, summary, summary))
}

// TestTheHandoverHoldsUntilTheHubsSessionsAreHeardFrom (X57): the hub has
// listed host 1 waiting for you and host 2 idle when a row it cannot draw
// sends the list to a poller the test scripts, Snapshot by Snapshot. The
// hub's last Snapshot stands, whole, while the poller's latest still shows
// host 1 connecting with no session — host 2's answer alone does not end it —
// and from the poller's Snapshot that has heard from both on, each is shown
// whole as it comes, a host connecting again included: nothing of the hub's
// is mixed in. A Snapshot whose saved half lists a session its running rows
// serve is not taken either; the next, consistent, one is. A host the
// registry no longer lists is not waited for. A
// failed read shows the hub's rows with the poller's error, once per error,
// and a read that then succeeds clears the note while host 1 is still waited
// for. The cap ends the wait whatever host 1 says: the poller's latest is
// shown then. The negative controls: a handover that showed the poller's
// Snapshot at its first good read shows host 1 connecting; one with no cap
// never shows the latest when host 1 never answers.
func TestTheHandoverHoldsUntilTheHubsSessionsAreHeardFrom(t *testing.T) {
	one := rosterRowOf(1, "one")
	one.Row = askingBody(1, "one", "Run make")
	answered := func(n int, title string) roster.Row {
		return polledRow(n, sessionOf(n), roster.Reachable, &roster.Session{ID: sessionOf(n), Title: title})
	}
	connecting := polledRow(1, sessionOf(1), roster.Connecting, nil)
	// open is the roster on the hub's two rows, handed over to the scripted
	// poller: the list's Snapshots so far, and the cap's channel.
	open := func(t *testing.T) (*listRig, *scriptPoller, chan time.Time, int) {
		t.Helper()
		h := newScriptHub(t, hostOf(0xa), one, rosterRowOf(2, "two"))
		p := &scriptPoller{out: make(chan roster.Snapshot)}
		capC := make(chan time.Time)
		rg := newListRig(t, testEnv(t), newListIndex(t), func(o *listOptions) {
			o.seedWait, o.reseedWait = step, step
			o.ensure = fixed(h.sock)
			o.poller = func() listPoller { return p }
			o.holdCap = func() <-chan time.Time { return capC }
		})
		sub := h.next()
		rg.mode(ModeHub)
		rg.until("the hub's rows", 0, func(s roster.Snapshot) bool { return len(s.Running) == 2 && asks(hostRow(s, hostOf(1)), "Run make") })
		from := rg.mark()
		sub.roster(100, []protocol.RosterRow{bodiless(4, protocol.RosterReachable)})
		sub.closedByClient("the notification")
		rg.mode(ModePoller)
		return rg, p, capC, from
	}
	// nothingSince checks the list has published nothing since from: the
	// hub's last Snapshot stands.
	nothingSince := func(t *testing.T, rg *listRig, from int, what string) {
		t.Helper()
		if got := rg.published(from); len(got) != 0 {
			t.Fatalf("%s: the list published %s", what, listed(got[0]))
		}
	}
	title := func(s roster.Snapshot, n int) string {
		if r := hostRow(s, hostOf(n)); r != nil && r.Session != nil {
			return r.Session.Title
		}
		return "-"
	}

	t.Run("until host 1 is heard from", func(t *testing.T) {
		rg, p, _, from := open(t)
		waitingFor1 := roster.Snapshot{Run: 1, Running: []roster.Row{connecting, answered(2, "two, polled")}}
		p.hand(t, roster.Snapshot{Run: 1, Running: []roster.Row{connecting, polledRow(2, sessionOf(2), roster.Connecting, nil)}})
		p.hand(t, waitingFor1)
		p.hand(t, waitingFor1)
		nothingSince(t, rg, from, "the poller has not heard from host 1")
		p.hand(t, roster.Snapshot{Run: 1, Running: []roster.Row{answered(1, "one, polled"), answered(2, "two, polled")}})
		s := rg.until("the poller's own", from, func(s roster.Snapshot) bool { return len(s.Running) > 0 })
		if title(s, 1) != "one, polled" || title(s, 2) != "two, polled" {
			t.Fatalf("the first Snapshot after the hold: %s", listed(s))
		}
		from = rg.mark()
		p.hand(t, roster.Snapshot{Run: 1, Running: []roster.Row{connecting, answered(2, "two, polled")}})
		s = rg.until("host 1 connecting again", from, func(s roster.Snapshot) bool { return len(s.Running) > 0 })
		if r := hostRow(s, hostOf(1)); r.Status != roster.Connecting || r.Session != nil || title(s, 2) != "two, polled" {
			t.Fatalf("a Snapshot after the hold is not the poller's own: %s", listed(s))
		}
	})
	t.Run("a saved row of a running session", func(t *testing.T) {
		rg, p, _, from := open(t)
		both := []roster.Row{answered(1, "one, polled"), answered(2, "two, polled")}
		stale := roster.Snapshot{Run: 1, Running: both, Saved: []sessions.Row{savedRow(1, "one, saved", 9), savedRow(5, "five", 8)}}
		p.hand(t, stale)
		p.hand(t, stale)
		nothingSince(t, rg, from, "the poller's saved half lists host 1's session")
		p.hand(t, roster.Snapshot{Run: 1, Running: both, Saved: []sessions.Row{savedRow(5, "five", 8)}})
		s := rg.until("the poller's own", from, func(s roster.Snapshot) bool { return len(s.Running) > 0 })
		if title(s, 1) != "one, polled" || !slices.Equal(savedTitles(s), []string{"five"}) {
			t.Fatalf("the first Snapshot after the hold: %s", listed(s))
		}
	})
	t.Run("a host the registry no longer lists", func(t *testing.T) {
		rg, p, _, from := open(t)
		p.hand(t, roster.Snapshot{Run: 1, Running: []roster.Row{answered(2, "two, polled")}})
		s := rg.until("the poller's own", from, func(s roster.Snapshot) bool { return len(s.Running) > 0 })
		if len(s.Running) != 1 || title(s, 2) != "two, polled" {
			t.Fatalf("the poller's Snapshot without host 1: %s", listed(s))
		}
	})
	t.Run("a failed read, then a read", func(t *testing.T) {
		rg, p, _, from := open(t)
		failed := roster.Snapshot{Run: 1, RegistryErr: errors.New("the registry cannot be read")}
		p.hand(t, failed)
		p.hand(t, failed)
		p.hand(t, roster.Snapshot{Run: 1, Running: []roster.Row{connecting, answered(2, "two, polled")}})
		p.hand(t, roster.Snapshot{Run: 1, Running: []roster.Row{connecting, answered(2, "two, polled")}})
		got := rg.published(from)
		if len(got) != 2 || got[0].RegistryErr == nil || got[1].RegistryErr != nil ||
			!fromHub(got[0]) || !fromHub(got[1]) || !asks(hostRow(got[1], hostOf(1)), "Run make") {
			for _, s := range got {
				t.Logf("published: %s", listed(s))
			}
			t.Fatalf("%d Snapshots while the poller failed and then waited for host 1: want the hub's with the error, then without", len(got))
		}
	})
	t.Run("the cap", func(t *testing.T) {
		rg, p, capC, from := open(t)
		latest := roster.Snapshot{Run: 1, Running: []roster.Row{connecting, answered(2, "two, polled")}}
		p.hand(t, latest)
		p.hand(t, latest)
		nothingSince(t, rg, from, "before the cap")
		select {
		case capC <- time.Now():
		case <-time.After(step):
			t.Fatal("the list was not waiting on the cap")
		}
		s := rg.until("the poller's latest at the cap", from, func(s roster.Snapshot) bool { return len(s.Running) > 0 })
		if r := hostRow(s, hostOf(1)); r == nil || r.Status != roster.Connecting || title(s, 2) != "two, polled" {
			t.Fatalf("the Snapshot at the cap: %s", listed(s))
		}
	})
}

// TestALegacySavedRowIsNotListedTwiceAcrossAHandover (r42 3, X57): host 1's
// session is known to this CRAZE_HOME's index only by its provider session id
// (a legacy row, no craze id), and its registry entry does not carry that id
// — only its own row, which the hub forwarded, does. So the hub's Snapshot
// lists it running and not saved, and a poller that has read the registry
// but not yet heard from host 1 would list it running and saved both. Through
// the handover — host 1 kept from the poller (gate) until the poller's
// Snapshot of it connecting has been handed to the list — no Snapshot from the
// hub's on lists the legacy row saved beside host 1, and once the poller has
// heard from host 1 its own Snapshot lists it running and not saved. The
// seed, from the registry alone, lists it saved: the control that the legacy
// row is the session's, and that only a session's own row can take it out.
// The negative control: a handover that showed the poller's Snapshot at its
// first good read would list the legacy row saved beside host 1.
func TestALegacySavedRowIsNotListedTwiceAcrossAHandover(t *testing.T) {
	env := testEnv(t)
	_, reg := hostWith(t, env, fakehost.Options{HostID: hostOf(1), CrazeSessionID: sessionOf(1), Workspace: "/work/" + hostOf(1),
		RowFacts: true})
	setVar(t, &directBudget, step)
	direct, err := Direct(stepContext(t), env, listWho)
	if err != nil {
		t.Fatal(err)
	}
	body, ok, err := rowOf(direct.Sessions, hostOf(1)).SessionRow()
	if !ok || err != nil || body.ProviderSessionID == "" {
		t.Fatalf("host 1's row names no provider session: %v", err)
	}
	if err := reg.Update(func(e *rundir.Entry) { e.ProviderSessionID = "" }); err != nil {
		t.Fatal(err)
	}
	x := newListIndex(t,
		sessions.Row{SessionID: body.ProviderSessionID, Provider: body.Provider.Name, CWD: "/w", Title: "legacy",
			UpdatedAt: time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)},
		savedRow(5, "five", 8))
	h := newScriptHub(t, hostOf(0xa), *rowOf(direct.Sessions, hostOf(1)))
	var (
		mu       sync.Mutex
		observed *observedPoller
	)
	rg := newListRig(t, env, x, func(o *listOptions) {
		o.seedWait, o.reseedWait = step, step
		o.ensure = fixed(h.sock)
		o.holdCap = func() <-chan time.Time { return nil }
		poller := o.poller
		o.poller = func() listPoller {
			mu.Lock()
			defer mu.Unlock()
			observed = observe(poller())
			return observed
		}
	})
	sub := h.next()
	rg.mode(ModeHub)
	if s := rg.published(0)[0]; !slices.Equal(savedTitles(s), []string{"legacy", "five"}) {
		t.Fatalf("the seed, from the registry alone, saves %v: the legacy row is not the session's", savedTitles(s))
	}
	rg.until("the hub's row", 0, fromHub)
	start := slices.IndexFunc(rg.published(0), fromHub)
	if hub := rg.published(start)[0]; !slices.Equal(savedTitles(hub), []string{"five"}) {
		t.Fatalf("the hub's Snapshot saves %v with host 1 running", savedTitles(hub))
	}

	gate(t, reg)
	sub.roster(100, []protocol.RosterRow{bodiless(9, protocol.RosterReachable)})
	sub.closedByClient("the notification")
	rg.mode(ModePoller)
	mu.Lock()
	o := observed
	mu.Unlock()
	for {
		s, ok := <-o.seen
		if !ok {
			t.Fatal("the poller stopped")
		}
		if r := hostRow(s, hostOf(1)); r != nil && r.Status == roster.Connecting && s.RegistryErr == nil {
			if !slices.Contains(savedTitles(s), "legacy") {
				t.Fatalf("the poller, not yet heard from host 1, saves %v: not the case this test is of", savedTitles(s))
			}
			break
		}
	}
	ungate(t, reg, sessionOf(1))
	rg.until("the poller's own row of host 1", start, func(s roster.Snapshot) bool {
		r := hostRow(s, hostOf(1))
		return r != nil && r.Polled && r.Status == roster.Reachable && r.Session != nil
	})
	for i, s := range rg.published(start) {
		if hostRow(s, hostOf(1)) == nil || slices.Contains(savedTitles(s), "legacy") || !slices.Contains(savedTitles(s), "five") {
			t.Fatalf("Snapshot %d lists host 1's session running and saved, or not whole: %s", start+i, listed(s))
		}
	}
}

// two2 is host 2's forwarded row's title, as the hub's row of it shows.
func two2(t *testing.T, r protocol.RosterRow) string {
	t.Helper()
	row, ok, err := r.SessionRow()
	if !ok || err != nil {
		t.Fatalf("host 2's forwarded row: %v", err)
	}
	return row.Title
}

// observedPoller is a poller whose every Snapshot the test sees too: handed
// to the list — which has taken it when the hand returns, the channel having
// no slot — then noted (seen) while there is room. A Snapshot whose registry
// read failed is handed over twice, as a poller that published the same
// failure at every tick would.
type observedPoller struct {
	p    listPoller
	out  chan roster.Snapshot
	seen chan roster.Snapshot
	quit chan struct{}
	done chan struct{}
}

func observe(p listPoller) *observedPoller {
	o := &observedPoller{p: p, out: make(chan roster.Snapshot), seen: make(chan roster.Snapshot, 64),
		quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(o.done)
		defer close(o.out)
		for s := range p.Updates() {
			times := 1
			if s.RegistryErr != nil {
				times = 2
			}
			for range times {
				select {
				case o.out <- s:
				case <-o.quit:
					return
				}
				select {
				case o.seen <- s:
				default:
				}
			}
		}
	}()
	return o
}

func (o *observedPoller) Updates() <-chan roster.Snapshot { return o.out }

func (o *observedPoller) Close() {
	close(o.quit)
	o.p.Close()
	<-o.done
}

// TestAHandoverHoldsTheHubsRowsUntilTheRegistryIsRead (r34 2, X57): the hub
// has listed both hosts — host 1's session out of the saved rows — when it
// sends a row the list cannot draw, and the poller that takes over cannot
// read the registry at first (its directory's mode refused). That poller's
// Snapshot lists nothing running; the list takes it — twice, as a poller
// publishing the same failure at every tick would hand it (observe) — and in
// its place publishes the hub's last once, whole — both hosts running, "five"
// alone saved — with the poller's registry error on it, which the list draws
// as its note; once the poller has read the registry, and heard from both
// hosts, its own Snapshots are handed on. The negative controls: a list that
// handed the first one on would list host 1's session saved, and neither host
// running; one that dropped it in silence would publish no Snapshot with the
// error; one that published the held rows at every failure would publish two.
func TestAHandoverHoldsTheHubsRowsUntilTheRegistryIsRead(t *testing.T) {
	env := testEnv(t)
	ho := handoverHosts(t, env)
	direct := ho.direct
	two := *rowOf(direct.Sessions, hostOf(2))
	h := newScriptHub(t, hostOf(0xa), rosterRowOf(1, "one"), two)
	var (
		mu       sync.Mutex
		observed *observedPoller
	)
	rg := newListRig(t, env, ho.x, func(o *listOptions) {
		o.seedWait, o.reseedWait = step, step
		o.ensure = fixed(h.sock)
		o.holdCap = func() <-chan time.Time { return nil }
		poller := o.poller
		o.poller = func() listPoller {
			mu.Lock()
			defer mu.Unlock()
			observed = observe(poller())
			return observed
		}
	})
	sub := h.next()
	rg.mode(ModeHub)
	rg.until("the hub's rows", 0, hubRows(map[string]string{hostOf(1): "one", hostOf(2): two2(t, two)}))
	start := rg.mark() - 1

	hosts := filepath.Join(env.Home, ".cache", "craze", "hosts")
	if err := os.Chmod(hosts, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hosts, 0o700) })
	sub.roster(100, []protocol.RosterRow{*rowOf(direct.Sessions, hostOf(1))})
	sub.closedByClient("the notification")
	rg.mode(ModePoller)
	mu.Lock()
	o := observed
	mu.Unlock()
	for range 2 {
		select {
		case s := <-o.seen:
			if s.RegistryErr == nil || len(s.Running) != 0 {
				t.Fatalf("the poller's first Snapshot read the registry: %s", listed(s))
			}
		case <-time.After(step):
			t.Fatal("the poller's failed read was not handed to the list")
		}
	}

	if err := os.Chmod(hosts, 0o700); err != nil {
		t.Fatal(err)
	}
	// The list takes the poller's Snapshots in order: once it has published
	// the poller's own rows, it has done with both failures.
	rg.until("the poller's own rows", start, polledWhole)
	held := 0
	for i, s := range rg.published(start) {
		one, two := hostRow(s, hostOf(1)), hostRow(s, hostOf(2))
		switch {
		case one == nil || two == nil || len(s.Running) != 2 || !slices.Equal(savedTitles(s), []string{"five"}):
			t.Fatalf("Snapshot %d is not the list whole: %s", start+i, listed(s))
		case (one.Host.Socket == "") != (two.Host.Socket == ""):
			t.Fatalf("Snapshot %d mixes the hub's rows and the poller's: %s", start+i, listed(s))
		case s.RegistryErr == nil:
		case !fromHub(s) || !strings.Contains(s.RegistryErr.Error(), "0700 is required"):
			t.Fatalf("Snapshot %d carries the registry error on rows that are not the hub's: %s", start+i, listed(s))
		default:
			held++
		}
	}
	if held != 1 {
		t.Fatalf("%d Snapshots of the hub's rows with the poller's registry error, want 1", held)
	}
}
