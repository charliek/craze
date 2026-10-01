package hub

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
)

// A roster subscription's membership and lifetime, and a roster the hub
// cannot read (plan 032 §3.6; the PR 3 branch review, r23 1, 3, 4, 5). The
// hosts live in memory (installMem); each test here is also run under a 5%
// CPU quota.

// onceCloser is a channel closed once, by the test or its cleanup.
func onceCloser(t *testing.T) (chan struct{}, func()) {
	ch := make(chan struct{})
	var once sync.Once
	closeIt := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(closeIt)
	return ch, closeIt
}

// TestAListRebasesItsConnectionsSubscription (r23 1): a sessions.list on a
// connection that holds a subscription is what its client holds from then
// on, so a host the list listed — one the subscription had not told its
// client of yet — that goes before the list's reply is written is removed
// by the next notification. The schedule is forced: subscribed before the
// host exists; the host appears and the flusher, woken for it, is held
// before the write lock; the list takes its snapshot, the host in it, and is
// held before its write; the host goes; the list is written, then the
// flusher runs. The negative control: a list that leaves the subscription's
// membership as it was lets the flusher fold the host's coming and going
// into nothing, and its client keeps a host that is gone.
func TestAListRebasesItsConnectionsSubscription(t *testing.T) {
	m := newMemHosts(t)
	var armed atomic.Bool
	flushing, snapped := make(chan struct{}, 8), make(chan struct{}, 1)
	flushGo, releaseFlush := onceCloser(t)
	listGo, releaseList := onceCloser(t)
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, hk: func(hk *hooks) {
		installMem(t, hk, m)
		hk.rosterLocking = func(_ *conn, what string) {
			if what == "flush" && armed.Load() {
				flushing <- struct{}{}
				<-flushGo
			}
		}
		hk.rosterTaken = func(_ *conn, what string) {
			if what == "list" && armed.Load() {
				snapped <- struct{}{}
				<-listGo
			}
		}
	}})
	p := dialPeer(t, rg.sock)
	if sub := p.subscribe(); len(sub.Sessions) != 0 {
		t.Fatalf("subscribed to %v, want an empty roster", ids(sub.Sessions))
	}
	armed.Store(true)
	wait := func(ch chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(step):
			t.Fatalf("%s: not within %v", what, step)
		}
	}

	h := m.add(1, memRow(1, "here"))
	rg.clk.advance(time.Second)
	from := rg.snaps.mark()
	rg.tick()
	rg.applied("the host listed", from, listedIn(true, h))
	wait(flushing, "the flusher woken for the host")

	listID := p.send(protocol.MethodSessionsList, nil)
	wait(snapped, "the list's snapshot")
	m.remove(h)
	rg.clk.advance(time.Second)
	from = rg.snaps.mark()
	rg.tick()
	rg.applied("the host gone", from, func(s roster.Snapshot) bool { return snapRow(s, h) == nil })

	releaseList()
	reply := p.read()
	var l protocol.HubSessionsListResult
	if string(reply.ID) != listID || reply.Error != nil || json.Unmarshal(reply.Result, &l) != nil || rowOf(l.Sessions, h) == nil {
		t.Fatalf("the list's reply: %s; want it to list the host, as snapshotted", clip(reply.raw))
	}
	releaseFlush()
	n := p.roster()
	if len(n.Upserts) != 0 || len(n.Removes) != 1 || n.Removes[0] != h || n.Cursor <= l.Cursor {
		t.Fatalf("the notification after the list: %+v; want the host's removal past cursor %d", n, l.Cursor)
	}
}

// TestARepeatedSubscriberHoldsOne (r23 3): a connection that subscribes, is
// reset and subscribes again, over and over — as the protocol lets it — holds
// one subscription: each subscribe joins and drops the ended ones first, and
// an ended subscription keeps no membership. The resets are the roster's
// completeness changing (omitted), every subscription ended at once. The
// negative control: a connection that kept every subscription it made would
// hold one more each round.
func TestARepeatedSubscriberHoldsOne(t *testing.T) {
	m := newMemHosts(t)
	m.add(1, memRow(1, "one"))
	held := make(chan int, 64)
	rg := newRosterRig(t, testEnv(t), rigOpts{hk: func(hk *hooks) {
		installMem(t, hk, m)
		hk.subscriptions = func(n int) { held <- n }
	}})
	p := dialPeer(t, rg.sock)
	for round := range 20 {
		sub := p.subscribe()
		select {
		case n := <-held:
			if n > 2 {
				t.Fatalf("round %d: the connection holds %d subscriptions", round, n)
			}
		case <-time.After(step):
			t.Fatal("no subscription registered")
		}
		rs := rg.h.rs
		rs.mu.Lock()
		var ended []*subscription
		for s := range rs.subs {
			ended = append(ended, s)
		}
		rs.resetAllLocked(protocol.ResetOmitted)
		for _, s := range ended {
			if s.has != nil || s.pending != nil {
				t.Errorf("round %d: ended subscription %s keeps its membership", round, s.id)
			}
		}
		rs.mu.Unlock()
		reset := p.read()
		var rp protocol.ResetParams
		if reset.Method != protocol.NotifyReset || json.Unmarshal(reset.Params, &rp) != nil ||
			rp.Subscription != sub.Subscription || rp.Reason != protocol.ResetOmitted {
			t.Fatalf("round %d: after the reset %s: %s", round, sub.Subscription, clip(reset.raw))
		}
	}
}

// TestAnUnreadableRegistryIsNoRoster (r23 4): a hub whose registry cannot be
// read answers sessions.list and sessions.subscribe unavailable, reason
// host_unreachable — an existing pair: session.connect's when the registry
// cannot be read — with no path in its message, never an empty roster, and
// registers no subscription. The negative control: once a read succeeds, in
// the next run, the roster is answered.
func TestAnUnreadableRegistryIsNoRoster(t *testing.T) {
	setVar(t, &listWait, step)
	m := newMemHosts(t)
	h := m.add(1, memRow(1, "one"))
	var unreadable atomic.Bool
	unreadable.Store(true)
	rg := newRosterRig(t, testEnv(t), rigOpts{hk: func(hk *hooks) {
		installMem(t, hk, m)
		// Set before the hub runs, so it is put back after the hub stops.
		setVar(t, &hostsRead, func(env rundir.Env) ([]rundir.Entry, error) {
			if unreadable.Load() {
				return nil, errors.New("rundir: /home/someone/.cache/craze/hosts: permission denied")
			}
			return m.read(env)
		})
	}})
	p := dialPeer(t, rg.sock)
	for _, method := range []string{protocol.MethodSessionsList, protocol.MethodSessionsSubscribe} {
		got := p.call(method, nil)
		refusal(t, got, protocol.CodeUnavailable, protocol.ReasonHostUnreachable)
		if strings.Contains(got.Error.Message, "/") {
			t.Fatalf("%s's refusal names a path: %q", method, got.Error.Message)
		}
	}
	rg.h.rs.mu.Lock()
	subs := len(rg.h.rs.subs)
	rg.h.rs.mu.Unlock()
	if subs != 0 {
		t.Fatalf("a refused subscribe left %d subscription(s)", subs)
	}

	unreadable.Store(false)
	if l := p.list(); len(l.Sessions) != 1 || l.Sessions[0].HostID != h {
		t.Fatalf("with the registry read: %v", ids(l.Sessions))
	}
	p.subscribe()
}

// TestASubscribeReplyTooLargeKeepsNoSubscription (r23 5): a subscribe whose
// reply cannot be written — a request id that takes a full roster's reply
// over the 16 MiB line, which C9's size contract allows — is answered
// response_too_large and keeps no subscription: no notification follows for
// it, and a subscribe with a short id on the same connection is answered.
// The roster: 512 rows near the 32,000-byte entry bound (a workspace of 4,096
// characters JSON escapes six bytes each, beside a row). The negative
// control: a subscription kept after its failed reply is already_subscribed
// on the next subscribe, and writes a notification for an id its client was
// never given.
func TestASubscribeReplyTooLargeKeepsNoSubscription(t *testing.T) {
	setVar(t, &listWait, step)
	m := bigRoster(t)
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, hk: func(hk *hooks) { installMem(t, hk, m) }})
	p := dialPeer(t, rg.sock)
	readRaw := func() msg { t.Helper(); return p.readRaw() }

	p.sendBigID(protocol.MethodSessionsSubscribe)
	failed := readRaw()
	if failed.Error == nil || failed.Error.Data.Reason != protocol.ReasonResponseTooLarge {
		t.Fatalf("the subscribe with a 1 MiB id: %s", clip(failed.raw))
	}

	// Nothing is left of it: no subscription, and no demand on the poll —
	// so a change and a tick start no round, and nothing is written.
	rs := rg.h.rs
	rs.mu.Lock()
	subs, demand := len(rs.subs), rs.demand
	rs.mu.Unlock()
	if subs != 0 || demand != 0 {
		t.Fatalf("after the failed subscribe: %d subscription(s), demand %d", subs, demand)
	}
	m.setRow(hostOf(1), memRow(1, "changed"))
	rg.clk.advance(time.Second)
	rg.tick()
	if m, ok := p.readWithin(300 * time.Millisecond); ok {
		t.Fatalf("after the failed subscribe the hub wrote %s", clip(m.raw))
	}

	id := p.send(protocol.MethodSessionsSubscribe, nil)
	ok := readRaw()
	var res protocol.SessionsSubscribeResult
	if string(ok.ID) != id || ok.Error != nil || json.Unmarshal(ok.Result, &res) != nil || len(res.Sessions) != protocol.RosterRowsMax {
		t.Fatalf("the subscribe with a short id: %s", clip(ok.raw))
	}
}

// bigRoster is RosterRowsMax hosts in memory whose roster rows are near the
// 32,000-byte entry bound — a workspace of 4,096 characters JSON escapes six
// bytes each, beside a row of about 6,500 — so a full roster's reply is
// about 16.1 MB: under the 16 MiB line with a short request id, over it with
// a 1 MiB one (sendBigID).
func bigRoster(t *testing.T) *memHosts {
	t.Helper()
	m := newMemHosts(t)
	ws := strings.Repeat("\x01", protocol.RosterWorkspaceMax)
	for n := 1; n <= protocol.RosterRowsMax; n++ {
		id := hostOf(n)
		m.put(rundir.Entry{Protocol: 1, HostID: id, PID: 1000 + n, Socket: memSocket(id), CrazeSessionID: sessionOf(n),
			Provider: "cursor", Workspace: ws, Ready: true, StartedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)},
			memRow(n, strings.Repeat("t", 6500)))
	}
	return m
}

// sendBigID writes method's request with a request id of 1 MiB.
func (p *peer) sendBigID(method string) {
	p.t.Helper()
	bigID, err := json.Marshal(strings.Repeat("i", 1<<20))
	if err != nil {
		p.t.Fatal(err)
	}
	_ = p.nc.SetWriteDeadline(time.Now().Add(step))
	if err := protocol.WriteLine(p.nc, map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(bigID), "method": method}); err != nil {
		p.t.Fatal(err)
	}
}

// readRaw is the next line, within step, its envelope only: a 16 MB line is
// not held to the schema here.
func (p *peer) readRaw() msg {
	p.t.Helper()
	_ = p.nc.SetReadDeadline(time.Now().Add(step))
	defer func() { _ = p.nc.SetReadDeadline(time.Time{}) }()
	line, err := p.lr.ReadLine()
	if err != nil {
		p.t.Fatalf("read: %v", err)
	}
	var out msg
	if err := json.Unmarshal(line, &out); err != nil {
		p.t.Fatal(err)
	}
	out.raw = line
	return out
}

// TestAListReplyTooLargeKeepsTheSubscriptionsView (r25 1): a sessions.list
// whose reply cannot be written — a 1 MiB request id over a full roster of
// large rows, answered response_too_large — on a connection that holds a
// subscription gives the client no roster in place of the one it holds, so
// the subscription's membership and what is pending stay as they were: a
// host that went just before the list is still removed by the next
// notification. The flusher, woken for the removal, is held before the
// write lock until the list has been answered. The negative control: a list
// that rebased the subscription though its reply failed drops the pending
// removal, and the client keeps a host that is gone.
func TestAListReplyTooLargeKeepsTheSubscriptionsView(t *testing.T) {
	setVar(t, &listWait, step)
	m := bigRoster(t)
	var armed atomic.Bool
	flushing := make(chan struct{}, 8)
	flushGo, releaseFlush := onceCloser(t)
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, hk: func(hk *hooks) {
		installMem(t, hk, m)
		hk.rosterLocking = func(_ *conn, what string) {
			if what == "flush" && armed.Load() {
				flushing <- struct{}{}
				<-flushGo
			}
		}
	}})
	p := dialPeer(t, rg.sock)
	id := p.send(protocol.MethodSessionsSubscribe, nil)
	if sub := p.readRaw(); string(sub.ID) != id || sub.Error != nil {
		t.Fatalf("the subscribe with a short id: %s", clip(sub.raw))
	}
	armed.Store(true)

	gone := hostOf(1)
	m.remove(gone)
	rg.clk.advance(time.Second)
	from := rg.snaps.mark()
	rg.tick()
	rg.applied("the host gone", from, func(s roster.Snapshot) bool { return snapRow(s, gone) == nil })
	select {
	case <-flushing:
	case <-time.After(step):
		t.Fatal("the flusher was not woken for the removal")
	}

	p.sendBigID(protocol.MethodSessionsList)
	if failed := p.readRaw(); failed.Error == nil || failed.Error.Data.Reason != protocol.ReasonResponseTooLarge {
		t.Fatalf("the list with a 1 MiB id: %s", clip(failed.raw))
	}
	releaseFlush()
	n := p.roster()
	if len(n.Upserts) != 0 || len(n.Removes) != 1 || n.Removes[0] != gone {
		t.Fatalf("the notification after the failed list: %+v; want the host's removal", n)
	}
}
