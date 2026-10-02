package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/control/wiretest"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// The session list's roster through the hub (plan 032 §3.13, A14): Roster
// returns at once; its seed, then the hub's rows — an in-process hub over
// hosts in memory, end to end — with the saved half kept on its own ticker;
// a lost subscription resubscribed, a new epoch reseeding; three losses in
// the window, a reseed that does not come, or no hub at all sending it to its
// poller; and a Close that joins everything. Where a hub must lose a
// subscription on cue — every reset reason, an end of the connection — the
// hub is a script of the test's (scriptHub). The saved half's ticker and the
// loss clock are the test's; every wait is bounded on its own (step). Each
// test here is also run under a 5% CPU quota.

// ------------------------------------------------------------- the rig

// listIndex is a session index in memory whose file the test moves by hand
// (set): the file's stamp is what the saved half reads it by.
type listIndex struct {
	t    *testing.T
	path string
	mu   sync.Mutex
	rows []sessions.Row
	n    int
}

func newListIndex(t *testing.T, rows ...sessions.Row) *listIndex {
	t.Helper()
	x := &listIndex{t: t, path: filepath.Join(t.TempDir(), "sessions.jsonl"), rows: rows}
	if err := os.WriteFile(x.path, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return x
}

func (x *listIndex) All() ([]sessions.Row, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.rows), nil
}

// set makes rows the index and moves its file's stamp: the file grows.
func (x *listIndex) set(rows ...sessions.Row) {
	x.t.Helper()
	x.mu.Lock()
	x.rows = rows
	x.n++
	n := x.n
	x.mu.Unlock()
	if err := os.WriteFile(x.path, []byte(strings.Repeat("x\n", n+1)), 0o600); err != nil {
		x.t.Fatal(err)
	}
}

// savedRow is an index row of craze session n, titled title, at hour h.
func savedRow(n int, title string, h int) sessions.Row {
	return sessions.Row{SessionID: fmt.Sprintf("p-%d", n), Provider: "cursor", CWD: "/w", Title: title,
		CrazeID: sessionOf(n), UpdatedAt: time.Date(2026, 9, 30, h, 0, 0, 0, time.UTC)}
}

// listRig is one list roster under test: every Snapshot it publishes kept,
// every mode it enters and epoch it seeds from noted, its poller's openings
// counted; the saved half's ticker and the loss clock are the rig's.
type listRig struct {
	t      *testing.T
	r      *ListRoster
	ticks  chan time.Time
	clk    *rigClock
	modes  chan Mode
	epochs chan string
	polls  atomic.Int32

	mu    sync.Mutex
	snaps []roster.Snapshot
	note  chan struct{}
}

// newListRig opens a list roster over env and x — production's rules but
// for tweak's — its ticker, clock and hooks the rig's.
func newListRig(t *testing.T, env rundir.Env, x *listIndex, tweak func(*listOptions)) *listRig {
	t.Helper()
	rg := &listRig{t: t, ticks: make(chan time.Time), clk: &rigClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)},
		modes: make(chan Mode, 16), epochs: make(chan string, 64), note: make(chan struct{}, 1)}
	o := listDefaults(env, x)
	o.indexPath = func() string { return x.path }
	o.savedTicks = rg.ticks
	o.now = rg.clk.now
	o.moved = func(m Mode) { rg.modes <- m }
	o.seeded = func(epoch string) { rg.epochs <- epoch }
	o.published = func(s roster.Snapshot) {
		rg.mu.Lock()
		rg.snaps = append(rg.snaps, s)
		rg.mu.Unlock()
		select {
		case rg.note <- struct{}{}:
		default:
		}
	}
	poller := o.poller
	o.poller = func() listPoller {
		rg.polls.Add(1)
		return poller()
	}
	if tweak != nil {
		tweak(&o)
	}
	rg.r = openList(env, x, o)
	t.Cleanup(rg.r.Close)
	return rg
}

// mode waits for the roster to enter want; entering another first fails.
func (rg *listRig) mode(want Mode) {
	rg.t.Helper()
	select {
	case m := <-rg.modes:
		if m != want {
			rg.t.Fatalf("the roster went %v, not %v", m, want)
		}
	case <-time.After(step):
		rg.t.Fatalf("the roster did not go %v within %v (it is %v)", want, step, rg.r.Mode())
	}
}

// noMode checks the roster has entered no mode since the last look.
func (rg *listRig) noMode() {
	rg.t.Helper()
	select {
	case m := <-rg.modes:
		rg.t.Fatalf("the roster went %v", m)
	default:
	}
}

// epoch is the epoch of the next reply that seeds the rows.
func (rg *listRig) epoch() string {
	rg.t.Helper()
	select {
	case e := <-rg.epochs:
		return e
	case <-time.After(step):
		rg.t.Fatalf("no reply seeded the rows within %v (mode %v)", step, rg.r.Mode())
	}
	return ""
}

// tick hands the saved half one tick: the roster has taken it once the send
// returns.
func (rg *listRig) tick() {
	rg.t.Helper()
	select {
	case rg.ticks <- time.Now():
	case <-time.After(step):
		rg.t.Fatal("the roster took no tick of its saved half")
	}
}

// mark is how many Snapshots were published so far: a later wait's from.
func (rg *listRig) mark() int {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return len(rg.snaps)
}

// published is every Snapshot published, from the from-th.
func (rg *listRig) published(from int) []roster.Snapshot {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return slices.Clone(rg.snaps[min(from, len(rg.snaps)):])
}

// until waits for a Snapshot published from the from-th on that pred holds
// for, and answers the first.
func (rg *listRig) until(what string, from int, pred func(roster.Snapshot) bool) roster.Snapshot {
	rg.t.Helper()
	deadline := time.After(step)
	for {
		snaps := rg.published(from)
		for _, s := range snaps {
			if pred(s) {
				return s
			}
		}
		select {
		case <-rg.note:
		case <-deadline:
			last := "none"
			if len(snaps) > 0 {
				last = listed(snaps[len(snaps)-1])
			}
			rg.t.Fatalf("%s: no such Snapshot within %v (mode %v); the last: %s", what, step, rg.r.Mode(), last)
		}
	}
}

// listed is a Snapshot in words: each running row's host, status, title and
// socket, then the saved rows' titles.
func listed(s roster.Snapshot) string {
	var b strings.Builder
	for _, r := range s.Running {
		title := "-"
		if r.Session != nil {
			title = r.Session.Title
		}
		fmt.Fprintf(&b, "[%s %s %q socket:%q index:%q] ", r.Host.ID, r.Status, title, r.Host.Socket, r.IndexTitle)
	}
	var saved []string
	for _, r := range s.Saved {
		saved = append(saved, r.Title)
	}
	fmt.Fprintf(&b, "saved:%v regErr:%v", saved, s.RegistryErr)
	return b.String()
}

// hostRow is id's running row in s, nil when it has none.
func hostRow(s roster.Snapshot, id string) *roster.Row {
	for i := range s.Running {
		if s.Running[i].Host.ID == id {
			return &s.Running[i]
		}
	}
	return nil
}

// savedTitles is s's saved rows' titles, in order.
func savedTitles(s roster.Snapshot) []string {
	var out []string
	for _, r := range s.Saved {
		out = append(out, r.Title)
	}
	return out
}

// hubRows is a predicate: every id listed as the hub lists it — no socket
// (a row the registry or a poll made carries one), reachable, titled as
// titles says — and exactly those.
func hubRows(titles map[string]string) func(roster.Snapshot) bool {
	return func(s roster.Snapshot) bool {
		if len(s.Running) != len(titles) {
			return false
		}
		for id, title := range titles {
			r := hostRow(s, id)
			if r == nil || r.Host.Socket != "" || r.Status != roster.Reachable || r.Session == nil || r.Session.Title != title {
				return false
			}
		}
		return true
	}
}

// fixed is an ensure that answers sock.
func fixed(sock string) func(context.Context, rundir.Env, protocol.ConnectionCapabilities) (string, error) {
	return func(context.Context, rundir.Env, protocol.ConnectionCapabilities) (string, error) { return sock, nil }
}

// ------------------------------------------------- a hub of the test's

// scriptHub is a hub of the test's own on a socket: it answers hello as a
// hub, and sessions.subscribe with the roster the test set — or holds that
// reply (hold) — and hands the test each subscription it answered, which the
// test then writes to: roster notifications, resets, the connection's end.
// Each line it writes is held to the protocol's schema.
type scriptHub struct {
	t    *testing.T
	sock string
	subs chan *scriptSub

	mu        sync.Mutex
	epoch     string
	rows      []protocol.RosterRow
	truncated bool
	n         int
	hold      chan struct{}
	conns     []net.Conn
	// offSchema: the hub writes what the schema refuses, on purpose (a
	// malformed hub's output), and its lines are not held to it.
	offSchema bool
}

// writeOffSchema lets the hub write lines the schema refuses: a test of a
// malformed hub.
func (h *scriptHub) writeOffSchema() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.offSchema = true
}

// scriptSub is one subscription a scriptHub answered.
type scriptSub struct {
	t   *testing.T
	nc  net.Conn
	id  string
	hub *scriptHub
	// gone is closed once the client has closed its end.
	gone chan struct{}
}

func newScriptHub(t *testing.T, epoch string, rows ...protocol.RosterRow) *scriptHub {
	t.Helper()
	h := &scriptHub{t: t, sock: filepath.Join(shortDir(t, "czs"), "hub.sock"), subs: make(chan *scriptSub, 16),
		epoch: epoch, rows: rows}
	ln, err := net.Listen("unix", h.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, c := range h.conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.conns = append(h.conns, c)
			h.mu.Unlock()
			go h.serve(c)
		}
	}()
	return h
}

// set makes the next subscriptions' replies epoch's and rows'.
func (h *scriptHub) set(epoch string, rows ...protocol.RosterRow) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.epoch, h.rows = epoch, rows
}

// setTruncated makes the next subscriptions' replies say the roster was cut
// at RosterRowsMax rows, or not.
func (h *scriptHub) setTruncated(truncated bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.truncated = truncated
}

// holdReplies makes every subscribe's reply wait until the returned func is
// called.
func (h *scriptHub) holdReplies() func() {
	ch := make(chan struct{})
	h.mu.Lock()
	h.hold = ch
	h.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	h.t.Cleanup(release)
	return release
}

// answered is how many subscriptions the hub answered.
func (h *scriptHub) answered() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// next is the next subscription the hub answered, within step.
func (h *scriptHub) next() *scriptSub {
	h.t.Helper()
	select {
	case s := <-h.subs:
		return s
	case <-time.After(step):
		h.t.Fatalf("no subscription within %v (%d answered)", step, h.answered())
	}
	return nil
}

func (h *scriptHub) serve(c net.Conn) {
	lr := protocol.NewLineReader(c, protocol.InboundLineMax)
	var sub *scriptSub
	defer func() {
		if sub != nil {
			close(sub.gone)
		}
	}()
	for {
		line, err := lr.ReadLine()
		if err != nil {
			return
		}
		var req protocol.Request
		if json.Unmarshal(line, &req) != nil {
			return
		}
		var result any
		switch req.Method {
		case protocol.MethodHello:
			h.mu.Lock()
			res := hubResult(h.epoch, "0.0.0-script", protocol.HubCapabilities())
			h.mu.Unlock()
			res.Codecs = protocol.Codecs{Event: 1, Snapshot: 1}
			result = res
		case protocol.MethodSessionsSubscribe:
			h.mu.Lock()
			h.n++
			id, hold := fmt.Sprintf("r-%d", h.n), h.hold
			res := protocol.SessionsSubscribeResult{Subscription: id, Epoch: h.epoch, Cursor: uint64(h.n),
				Sessions: append([]protocol.RosterRow{}, h.rows...), Truncated: h.truncated}
			h.mu.Unlock()
			if hold != nil {
				<-hold
			}
			result = res
			sub = &scriptSub{t: h.t, nc: c, id: id, hub: h, gone: make(chan struct{})}
		default:
			return
		}
		raw, _ := json.Marshal(result)
		h.write(c, req.Method, protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: req.ID, Result: raw})
		if sub != nil && req.Method == protocol.MethodSessionsSubscribe {
			h.subs <- sub
		}
	}
}

// write writes v, the answer to method or the notification method, held to
// the schema first.
func (h *scriptHub) write(c net.Conn, method string, v any) {
	line, err := protocol.MarshalLine(v)
	if err != nil {
		h.t.Errorf("a script line: %v", err)
		return
	}
	h.mu.Lock()
	offSchema := h.offSchema
	h.mu.Unlock()
	if err := wiretest.Default().Server(method, line[:len(line)-1]); err != nil && !offSchema {
		h.t.Errorf("the script's %s line is off the schema: %v (%s)", method, err, line)
	}
	_ = c.SetWriteDeadline(time.Now().Add(step))
	_, _ = c.Write(line)
}

// notify writes the notification method with params on s's connection.
func (s *scriptSub) notify(method string, params any) {
	raw, err := json.Marshal(params)
	if err != nil {
		s.t.Fatal(err)
	}
	s.hub.write(s.nc, method, protocol.Notification{JSONRPC: protocol.JSONRPCVersion, Method: method, Params: raw})
}

// unknown writes a notification protocol 1 does not name: a later hub's.
func (s *scriptSub) unknown() {
	_ = s.nc.SetWriteDeadline(time.Now().Add(step))
	_, _ = s.nc.Write([]byte(`{"jsonrpc":"2.0","method":"later","params":{"subscription":"` + s.id + `"}}` + "\n"))
}

// roster writes a roster notification of s's: upserts, removes.
func (s *scriptSub) roster(cursor uint64, upserts []protocol.RosterRow, removes ...string) {
	s.hub.mu.Lock()
	epoch := s.hub.epoch
	s.hub.mu.Unlock()
	if upserts == nil {
		upserts = []protocol.RosterRow{}
	}
	if removes == nil {
		removes = []string{}
	}
	s.notify(protocol.NotifyRoster, protocol.RosterParams{Subscription: s.id, Epoch: epoch, Cursor: cursor,
		Upserts: upserts, Removes: removes})
}

// reset ends s with why.
func (s *scriptSub) reset(why protocol.ResetReason) {
	s.notify(protocol.NotifyReset, protocol.ResetParams{Subscription: s.id, Reason: why})
}

// end closes s's connection: its client reads the end.
func (s *scriptSub) end() { _ = s.nc.Close() }

// closedByClient waits for the client to close s's connection.
func (s *scriptSub) closedByClient(what string) {
	s.t.Helper()
	select {
	case <-s.gone:
	case <-time.After(step):
		s.t.Fatalf("%s: the client did not close subscription %s's connection within %v", what, s.id, step)
	}
}

// rosterRowOf is host n's roster row, reachable, its session titled title.
func rosterRowOf(n int, title string) protocol.RosterRow {
	return protocol.RosterRow{HostID: hostOf(n), SessionID: sessionOf(n), Status: protocol.RosterReachable,
		Host: protocol.RosterHost{PID: 1000 + n, CrazeVersion: "0.0.0-script", Protocol: 1, Provider: "cursor",
			Workspace: "/work/" + hostOf(n), StartedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), Ready: true},
		Row: json.RawMessage(memRow(n, title))}
}

// ------------------------------------------------------------ the tests

// TestTheListRosterReturnsAtOnce (A14, V3's frames): Roster returns while
// the hub is still being reached — timed, well inside the seed's bound — and
// the first Snapshot follows with the hub still not reached: the registry's
// host connecting (its socket the registry's) and the index's saved rows
// less its session, the hub's mode still seeding. The negative control: a
// Roster that reached the hub before returning would wait out the held
// Ensure, the seed's bound, which this test sets to step.
func TestTheListRosterReturnsAtOnce(t *testing.T) {
	env := testEnv(t)
	hostIn(t, env, 1)
	x := newListIndex(t, savedRow(1, "one, running", 9), savedRow(2, "two", 8))
	entered := make(chan struct{}, 1)
	release, freed := onceCloser(t)
	o := listDefaults(env, x)
	o.indexPath = func() string { return x.path }
	o.seedWait = step
	o.ensure = func(ctx context.Context, _ rundir.Env, need protocol.ConnectionCapabilities) (string, error) {
		if !need.RosterSubscribe {
			t.Errorf("Ensure was asked for %+v, not the roster subscription", need)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("Ensure was given no deadline (X34)")
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "", errors.New("no hub for this test")
	}
	start := time.Now()
	r := openList(env, x, o)
	took := time.Since(start)
	t.Cleanup(r.Close)
	t.Logf("Roster returned in %v", took)
	if took >= time.Second {
		t.Fatalf("Roster took %v to return: it waited on the hub", took)
	}
	select {
	case <-entered:
	case <-time.After(step):
		t.Fatal("the roster never reached for the hub")
	}
	var s roster.Snapshot
	select {
	case s = <-r.Updates():
	case <-time.After(step):
		t.Fatal("no first Snapshot while the hub was being reached")
	}
	row := hostRow(s, hostOf(1))
	switch {
	case len(s.Running) != 1 || row == nil || row.Status != roster.Connecting || row.Host.Socket == "":
		t.Fatalf("the seed's running rows: %s", listed(s))
	case row.IndexTitle != "one, running" || !slices.Equal(savedTitles(s), []string{"two"}):
		t.Fatalf("the seed's index title %q, saved %v", row.IndexTitle, savedTitles(s))
	case r.Mode() != ModeSeeding:
		t.Fatalf("the roster is %v while the hub is being reached", r.Mode())
	}
	freed()
}

// TestTheListRosterFollowsTheHub (§3.13, A14) end to end against a hub in
// process over hosts in memory: the hub's rows — no socket, reachable, the
// index's title beside — once subscribed; a row that changes, one that
// leaves and one that appears as the hub polls; the saved half brought up to
// date by its own tick while the hub sends nothing; and, when the hub goes
// (its teardown's reset hub_closing), a new hub reached through Ensure whose
// reply — a new epoch — reseeds every row: a host the old hub still listed is
// gone. The roster never opens its poller. Negative controls: the saved row
// added to the index is not listed before the saved half's tick, and a
// roster that merged the new hub's reply into the old rows would keep the
// host the new hub does not list.
func TestTheListRosterFollowsTheHub(t *testing.T) {
	setVar(t, &listWait, step)
	m := newMemHosts(t)
	envA := testEnv(t)
	hubA := newRosterRig(t, envA, rigOpts{hk: func(hk *hooks) { installMem(t, hk, m) }})
	m.add(1, memRow(1, "one"))
	m.add(2, memRow(2, "two"))
	x := newListIndex(t, savedRow(1, "index one", 9), savedRow(5, "five", 8))
	var sock atomic.Value
	sock.Store(hubA.sock)
	rg := newListRig(t, envA, x, func(o *listOptions) {
		o.seedWait, o.reseedWait = step, step
		o.ensure = func(context.Context, rundir.Env, protocol.ConnectionCapabilities) (string, error) {
			return sock.Load().(string), nil
		}
	})
	if e := rg.epoch(); e != hubA.h.id {
		t.Fatalf("the rows were seeded from epoch %q, not hub A's %q", e, hubA.h.id)
	}
	rg.mode(ModeHub)
	s := rg.until("the hub's two rows", 0, hubRows(map[string]string{hostOf(1): "one", hostOf(2): "two"}))
	if r := hostRow(s, hostOf(1)); r.IndexTitle != "index one" || r.Version != "0.0.0-mem" || r.Host.CrazeSessionID != sessionOf(1) {
		t.Fatalf("host 1's row: %s", listed(s))
	}
	if !slices.Equal(savedTitles(s), []string{"five"}) {
		t.Fatalf("saved %v with session 1 running", savedTitles(s))
	}

	// A row changes, one leaves, one appears: each in a round of the hub's.
	from := rg.mark()
	m.setRow(hostOf(1), memRow(1, "renamed"))
	hubA.tick()
	rg.until("host 1 renamed", from, hubRows(map[string]string{hostOf(1): "renamed", hostOf(2): "two"}))
	from = rg.mark()
	m.remove(hostOf(2))
	hubA.tick()
	rg.until("host 2 gone", from, hubRows(map[string]string{hostOf(1): "renamed"}))
	from = rg.mark()
	m.add(3, memRow(3, "three"))
	hubA.tick()
	rg.until("host 3 come", from, hubRows(map[string]string{hostOf(1): "renamed", hostOf(3): "three"}))

	// The saved half moves on its own tick, the hub quiet.
	from = rg.mark()
	x.set(savedRow(6, "six", 10), savedRow(1, "index one", 9), savedRow(5, "five", 8))
	for _, s := range rg.published(from) {
		if slices.Contains(savedTitles(s), "six") {
			t.Fatalf("the index's new row was listed before the saved half's tick: %s", listed(s))
		}
	}
	rg.tick()
	s = rg.until("the index's new row saved", from, func(s roster.Snapshot) bool {
		return slices.Equal(savedTitles(s), []string{"six", "five"})
	})
	if !hubRows(map[string]string{hostOf(1): "renamed", hostOf(3): "three"})(s) {
		t.Fatalf("the saved half's tick moved the running rows: %s", listed(s))
	}

	// Hub A goes; hub B — a new epoch — answers Ensure next. Host 1 goes
	// meanwhile, which hub A never polls again.
	hubB := newRosterRig(t, testEnv(t), rigOpts{hk: func(hk *hooks) { installMem(t, hk, m) }})
	m.remove(hostOf(1))
	sock.Store(hubB.sock)
	from = rg.mark()
	hubA.rn.sigs <- syscall.SIGTERM
	if e := rg.epoch(); e != hubB.h.id {
		t.Fatalf("the rows were reseeded from epoch %q, not hub B's %q", e, hubB.h.id)
	}
	s = rg.until("hub B's rows", from, hubRows(map[string]string{hostOf(3): "three"}))
	if !slices.Equal(savedTitles(s), []string{"six", "index one", "five"}) {
		t.Fatalf("session 1 is not saved once its host is gone: %s", listed(s))
	}
	rg.noMode()
	if n := rg.polls.Load(); n != 0 || rg.r.Mode() != ModeHub {
		t.Fatalf("the roster opened its poller %d times and is %v", n, rg.r.Mode())
	}
}

// TestTheListRosterFallsBackWithNoHub (A14): in a Go test binary with no hub
// (Ensure is ErrNoHub) the roster runs its poller at once — well within the
// seed's two seconds — and nothing visible changes: every Snapshot it
// publishes lists the registry's one host and the same saved rows, from the
// seed's connecting row to the poll's reachable one, whose socket is the
// registry's. The negative control is the hub's own test above: there the
// poller is never opened.
func TestTheListRosterFallsBackWithNoHub(t *testing.T) {
	env := testEnv(t)
	hostIn(t, env, 1)
	x := newListIndex(t, savedRow(1, "one, running", 9), savedRow(2, "two", 8))
	start := time.Now()
	rg := newListRig(t, env, x, nil)
	rg.mode(ModePoller)
	if took := time.Since(start); took >= listSeedWait {
		t.Fatalf("the poller ran %v after the roster opened, not within %v", took, listSeedWait)
	} else {
		t.Logf("the poller ran %v after the roster opened", took)
	}
	rg.until("the poll's reachable row", 0, func(s roster.Snapshot) bool {
		r := hostRow(s, hostOf(1))
		return r != nil && r.Status == roster.Reachable && r.Session != nil
	})
	for i, s := range rg.published(0) {
		r := hostRow(s, hostOf(1))
		if len(s.Running) != 1 || r == nil || r.Host.Socket == "" || r.Status == roster.Unreachable ||
			r.IndexTitle != "one, running" || !slices.Equal(savedTitles(s), []string{"two"}) {
			t.Fatalf("Snapshot %d is not the list the others are: %s", i, listed(s))
		}
	}
	if n := rg.polls.Load(); n != 1 {
		t.Fatalf("the poller was opened %d times", n)
	}
}

// TestTheListRosterFallsBackWhenTheHubDoesNotAnswer (§3.13): a hub not
// reached within the seed's bound — an Ensure that does not return, or a hub
// that never answers sessions.subscribe — sends the roster to its poller,
// the reach given up at the bound: its context, which carries the bound as
// its deadline, ended — at that deadline, or cancelled by the list's own
// timer for the same bound, whichever fires first (r28 3) — and its
// connection closed by it. The negative control is
// TestTheListRosterFollowsTheHub, whose bound is not reached.
func TestTheListRosterFallsBackWhenTheHubDoesNotAnswer(t *testing.T) {
	t.Run("Ensure", func(t *testing.T) {
		const bound = 100 * time.Millisecond
		ended := make(chan error, 1)
		rg := newListRig(t, testEnv(t), newListIndex(t), func(o *listOptions) {
			o.seedWait = bound
			o.ensure = func(ctx context.Context, _ rundir.Env, _ protocol.ConnectionCapabilities) (string, error) {
				deadline, ok := ctx.Deadline()
				left := time.Until(deadline)
				<-ctx.Done()
				switch {
				case !ok:
					ended <- errors.New("no deadline")
				case left > bound:
					ended <- fmt.Errorf("a deadline %v off, past the bound", left)
				default:
					ended <- ctx.Err()
				}
				return "", ctx.Err()
			}
		})
		rg.mode(ModePoller)
		if err := <-ended; !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("Ensure's context: %v; want it ended at the bound", err)
		}
	})
	t.Run("subscribe", func(t *testing.T) {
		h := newScriptHub(t, hostOf(0xa), rosterRowOf(1, "one"))
		h.holdReplies()
		rg := newListRig(t, testEnv(t), newListIndex(t), func(o *listOptions) {
			o.seedWait = 100 * time.Millisecond
			o.ensure = fixed(h.sock)
		})
		rg.mode(ModePoller)
		if n := rg.polls.Load(); n != 1 {
			t.Fatalf("the poller was opened %d times", n)
		}
	})
}

// TestTheListRosterResubscribesAfterALoss (§3.13, X35): a subscription lost
// — reset slow_consumer, omitted or hub_closing, or its connection's end —
// is replaced through Ensure: the old connection closed by the roster, a new
// subscription made, its reply reseeding every row (a host listed only
// since); the new subscription's notifications are taken, another
// subscription's and an unknown notification passed over. The losses are
// spaced past the window, so none sends it to its poller. The negative
// control: a roster that ignored the reset, or the end, would subscribe no
// more, and the hub's next() would time out.
func TestTheListRosterResubscribesAfterALoss(t *testing.T) {
	losses := []struct {
		name string
		lose func(*scriptSub)
	}{
		{"slow_consumer", func(s *scriptSub) { s.reset(protocol.ResetSlowConsumer) }},
		{"omitted", func(s *scriptSub) { s.reset(protocol.ResetOmitted) }},
		{"hub_closing", func(s *scriptSub) { s.reset(protocol.ResetHubClosing); s.end() }},
		{"end of the connection", func(s *scriptSub) { s.end() }},
	}
	for _, loss := range losses {
		t.Run(loss.name, func(t *testing.T) {
			h := newScriptHub(t, hostOf(0xa), rosterRowOf(1, "one"))
			rg := newListRig(t, testEnv(t), newListIndex(t), func(o *listOptions) {
				o.seedWait, o.reseedWait = step, step
				o.ensure = fixed(h.sock)
			})
			sub := h.next()
			if e := rg.epoch(); e != hostOf(0xa) {
				t.Fatalf("the first reply's epoch %q", e)
			}
			rg.mode(ModeHub)
			rg.until("the first reply's row", 0, hubRows(map[string]string{hostOf(1): "one"}))
			for round := range 3 {
				epoch := hostOf(0xa)
				if loss.name == "hub_closing" {
					epoch = hostOf(0xb0 + round)
				}
				h.set(epoch, rosterRowOf(1, "one"), rosterRowOf(2+round, "new"))
				rg.clk.advance(listLossWindow + time.Second)
				from := rg.mark()
				loss.lose(sub)
				sub.closedByClient(loss.name)
				sub = h.next()
				if e := rg.epoch(); e != epoch {
					t.Fatalf("round %d reseeded from epoch %q, want %q", round, e, epoch)
				}
				rg.until("the reseed", from, hubRows(map[string]string{hostOf(1): "one", hostOf(2 + round): "new"}))

				// The new subscription is live: another's notification and
				// an unknown one passed over, its own taken.
				from = rg.mark()
				other := *sub
				other.id = "r-other"
				other.roster(100, []protocol.RosterRow{rosterRowOf(9, "not ours")})
				sub.unknown()
				sub.roster(101, []protocol.RosterRow{rosterRowOf(1, "changed")})
				s := rg.until("the new subscription's change", from, func(s roster.Snapshot) bool {
					r := hostRow(s, hostOf(1))
					return r != nil && r.Session != nil && r.Session.Title == "changed"
				})
				if hostRow(s, hostOf(9)) != nil {
					t.Fatalf("another subscription's notification was taken: %s", listed(s))
				}
				h.set(epoch, rosterRowOf(1, "one"))
			}
			rg.noMode()
			if n := rg.polls.Load(); n != 0 || h.answered() != 4 {
				t.Fatalf("the poller was opened %d times; %d subscriptions", n, h.answered())
			}
		})
	}
}

// TestThreeLossesInTenSecondsFallBackToThePoller (§3.13): losses at 0 s and
// 9 s, then a third at 10.5 s — the first out of the window by then — keep
// the roster on the hub, which is the window's negative control (a count
// that ignored it would have gone at the third); a fourth at 10.5 s is the
// third within ten seconds, and sends it to its poller, its last connection
// closed and no other subscription made.
func TestThreeLossesInTenSecondsFallBackToThePoller(t *testing.T) {
	h := newScriptHub(t, hostOf(0xa), rosterRowOf(1, "one"))
	rg := newListRig(t, testEnv(t), newListIndex(t), func(o *listOptions) {
		o.seedWait, o.reseedWait = step, step
		o.ensure = fixed(h.sock)
	})
	sub := h.next()
	rg.mode(ModeHub)
	lose := func(why string, s *scriptSub) {
		t.Helper()
		s.reset(protocol.ResetSlowConsumer)
		s.closedByClient(why)
	}
	lose("loss 1, at 0 s", sub)
	sub = h.next()
	rg.clk.advance(9 * time.Second)
	lose("loss 2, at 9 s", sub)
	sub = h.next()
	rg.clk.advance(1500 * time.Millisecond)
	lose("loss 3, at 10.5 s", sub)
	sub = h.next()
	rg.noMode()
	lose("loss 4, at 10.5 s", sub)
	rg.mode(ModePoller)
	if n, got := rg.polls.Load(), h.answered(); n != 1 || got != 4 {
		t.Fatalf("after the third loss in the window: the poller opened %d times, %d subscriptions", n, got)
	}
}

// TestAReseedThatDoesNotComeFallsBack (§3.13): after a loss, a hub not
// reached again within the reseed's bound — an Ensure that does not return,
// or a hub that holds its reply — sends the roster to its poller. The
// negative control is TestTheListRosterResubscribesAfterALoss, whose reseeds
// come.
func TestAReseedThatDoesNotComeFallsBack(t *testing.T) {
	for _, hold := range []string{"Ensure", "subscribe"} {
		t.Run(hold, func(t *testing.T) {
			h := newScriptHub(t, hostOf(0xa), rosterRowOf(1, "one"))
			var calls atomic.Int32
			rg := newListRig(t, testEnv(t), newListIndex(t), func(o *listOptions) {
				o.seedWait, o.reseedWait = step, 100*time.Millisecond
				o.ensure = func(ctx context.Context, _ rundir.Env, _ protocol.ConnectionCapabilities) (string, error) {
					if calls.Add(1) > 1 && hold == "Ensure" {
						<-ctx.Done()
						return "", ctx.Err()
					}
					return h.sock, nil
				}
			})
			sub := h.next()
			rg.mode(ModeHub)
			if hold == "subscribe" {
				h.holdReplies()
			}
			sub.reset(protocol.ResetHubClosing)
			sub.end()
			rg.mode(ModePoller)
			if n := rg.polls.Load(); n != 1 {
				t.Fatalf("the poller was opened %d times", n)
			}
		})
	}
}

// closeGrace is how long a Close that should be joining something held is
// given to return anyway: a Close that does not join returns within it.
const closeGrace = 500 * time.Millisecond

// TestTheListRosterCloseJoinsEverything: Close returns only once what the
// roster started has finished — a reach of the hub in flight (Ensure
// cancelled, and held after), the subscription's connection and reader, the
// poller — and a second Close returns at once, its Updates closed. Each is
// held in turn, alone, where a Close that did not join it would return
// within closeGrace.
func TestTheListRosterCloseJoinsEverything(t *testing.T) {
	closes := func(t *testing.T, r *ListRoster, held string, release func()) {
		t.Helper()
		closed := make(chan struct{})
		go func() {
			r.Close()
			close(closed)
		}()
		select {
		case <-closed:
			t.Fatalf("Close returned with %s held", held)
		case <-time.After(closeGrace):
		}
		release()
		select {
		case <-closed:
		case <-time.After(step):
			t.Fatalf("Close did not return within %v of %s's release", step, held)
		}
		again := make(chan struct{})
		go func() {
			r.Close()
			close(again)
		}()
		select {
		case <-again:
		case <-time.After(step):
			t.Fatal("a second Close did not return")
		}
		for range r.Updates() {
		}
	}

	t.Run("a reach of the hub", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		hold, release := onceCloser(t)
		rg := newListRig(t, testEnv(t), newListIndex(t), func(o *listOptions) {
			o.seedWait = step
			o.ensure = func(ctx context.Context, _ rundir.Env, _ protocol.ConnectionCapabilities) (string, error) {
				entered <- struct{}{}
				<-ctx.Done()
				<-hold
				return "", ctx.Err()
			}
		})
		select {
		case <-entered:
		case <-time.After(step):
			t.Fatal("the roster never reached for the hub")
		}
		closes(t, rg.r, "Ensure", release)
	})

	t.Run("the subscription", func(t *testing.T) {
		h := newScriptHub(t, hostOf(0xa), rosterRowOf(1, "one"))
		hold, release := onceCloser(t)
		var held atomic.Bool
		rg := newListRig(t, testEnv(t), newListIndex(t), func(o *listOptions) {
			o.seedWait = step
			o.ensure = fixed(h.sock)
			o.dial = func(ctx context.Context, socket string) (net.Conn, error) {
				nc, err := dialHub(ctx, socket)
				if err != nil {
					return nil, err
				}
				return &heldClose{Conn: nc, hold: hold, held: &held}, nil
			}
		})
		h.next()
		rg.mode(ModeHub)
		closes(t, rg.r, "the subscription's close", release)
		if !held.Load() {
			t.Fatal("the subscription's connection was never closed")
		}
	})

	t.Run("the poller", func(t *testing.T) {
		hold, release := onceCloser(t)
		var closed atomic.Bool
		rg := newListRig(t, testEnv(t), newListIndex(t), func(o *listOptions) {
			poller := o.poller
			o.poller = func() listPoller { return &heldPoller{listPoller: poller(), hold: hold, closed: &closed} }
		})
		rg.mode(ModePoller)
		closes(t, rg.r, "the poller's Close", release)
		if !closed.Load() {
			t.Fatal("the poller was never closed")
		}
	})
}

// heldClose is a connection whose first Close closes it and then waits for
// hold.
type heldClose struct {
	net.Conn
	hold <-chan struct{}
	held *atomic.Bool
	once sync.Once
}

func (c *heldClose) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.held.Store(true)
		<-c.hold
	})
	return err
}

// heldPoller is a poller whose Close closes it and then waits for hold.
type heldPoller struct {
	listPoller
	hold   <-chan struct{}
	closed *atomic.Bool
}

func (p *heldPoller) Close() {
	p.listPoller.Close()
	p.closed.Store(true)
	<-p.hold
}

// TestATruncatedRosterSendsTheListToItsPoller (r28 2): a subscription whose
// reply says the hub's roster was cut at RosterRowsMax rows — the first, or
// the reseed after reset omitted, when the roster's completeness changed — is
// no roster for the list: the roster closes that subscription and runs its
// poller, which lists every host, never having listed the cut roster's rows.
// The negative control: the same replies without truncated are the hub's
// (TestTheListRosterResubscribesAfterALoss).
func TestATruncatedRosterSendsTheListToItsPoller(t *testing.T) {
	t.Run("the first reply", func(t *testing.T) {
		h := newScriptHub(t, hostOf(0xa), rosterRowOf(1, "one"))
		h.setTruncated(true)
		rg := newListRig(t, testEnv(t), newListIndex(t), func(o *listOptions) {
			o.seedWait, o.reseedWait = step, step
			o.ensure = fixed(h.sock)
		})
		h.next().closedByClient("a truncated first reply")
		rg.mode(ModePoller)
		for _, s := range rg.published(0) {
			if hostRow(s, hostOf(1)) != nil {
				t.Fatalf("the cut roster's row was listed: %s", listed(s))
			}
		}
	})
	t.Run("the reseed after reset omitted", func(t *testing.T) {
		h := newScriptHub(t, hostOf(0xa), rosterRowOf(1, "one"))
		rg := newListRig(t, testEnv(t), newListIndex(t), func(o *listOptions) {
			o.seedWait, o.reseedWait = step, step
			o.ensure = fixed(h.sock)
		})
		sub := h.next()
		rg.mode(ModeHub)
		h.setTruncated(true)
		sub.reset(protocol.ResetOmitted)
		sub.closedByClient("the omitted reset")
		h.next().closedByClient("a truncated reseed")
		rg.mode(ModePoller)
		if n := rg.polls.Load(); n != 1 {
			t.Fatalf("the poller was opened %d times", n)
		}
	})
}

// bodiless is host n's roster row with no body, status st: one the hub has
// not read yet (connecting), lost (unreachable) or dropped over its size
// bound (reachable) — approximate, as the hub marks each.
func bodiless(n int, st protocol.RosterStatus) protocol.RosterRow {
	r := rosterRowOf(n, "")
	r.Row, r.Status, r.Approximate = nil, st, true
	r.Host.Provider = "grok"
	return r
}

// regEntry is host n's registry entry, serving craze session session as
// provider session p-<n>, incarnation inc-<n>, at a socket of its own.
func regEntry(n int, session string) rundir.Entry {
	return rundir.Entry{Protocol: 1, HostID: hostOf(n), PID: 1000 + n, Socket: fmt.Sprintf("/run/%d.sock", n),
		CrazeSessionID: session, Provider: "grok", ProviderSessionID: fmt.Sprintf("p-%d", n),
		Incarnation: fmt.Sprintf("inc-%d", n), Ready: true}
}

// registryOf is a registry read listing hosts ns, each serving the craze
// session session(n) (regEntry).
func registryOf(session func(int) string, ns ...int) func() ([]rundir.Entry, error) {
	return func() ([]rundir.Entry, error) {
		var out []rundir.Entry
		for _, n := range ns {
			out = append(out, regEntry(n, session(n)))
		}
		return out, nil
	}
}

// changingRegistry is a registry read the test changes as it goes.
type changingRegistry struct {
	mu      sync.Mutex
	entries []rundir.Entry
}

func (g *changingRegistry) read() ([]rundir.Entry, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.entries), nil
}

func (g *changingRegistry) set(entries ...rundir.Entry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.entries = entries
}

// legacyRow is an index row of no craze id — a legacy one, matched by its
// provider session id — for host n's provider session (regEntry), at hour h.
func legacyRow(n, h int) sessions.Row {
	return sessions.Row{SessionID: fmt.Sprintf("p-%d", n), Provider: "grok", CWD: "/w", Title: fmt.Sprintf("legacy %d", n),
		UpdatedAt: time.Date(2026, 9, 30, h, 0, 0, 0, time.UTC)}
}

// TestARowWithNoBodyIsNamedByItsRegistryEntry (r28 4): connecting and
// unreachable rows the hub sends with no body to decode — not read yet,
// lost, or lost with one this build cannot decode — name their sessions'
// provider session ids by their hosts' registry entries, when those name the
// same craze session: a legacy saved row (no craze id) of a session one of
// them serves is not listed saved, and no row takes its entry's socket. The
// negative control is the second case: entries that name other craze
// sessions say nothing of these rows, and the legacy rows are listed saved
// beside the hosts that run them — what a row that took nothing from the
// registry was listed with.
func TestARowWithNoBodyIsNamedByItsRegistryEntry(t *testing.T) {
	odd := bodiless(3, protocol.RosterUnreachable)
	odd.Row = json.RawMessage(`{"sessionId":"session-3","title":7}`)
	rows := []protocol.RosterRow{bodiless(1, protocol.RosterConnecting), bodiless(2, protocol.RosterUnreachable), odd}
	index := []sessions.Row{legacyRow(1, 9), legacyRow(2, 8), legacyRow(3, 7), savedRow(9, "other", 6)}
	for _, tc := range []struct {
		name    string
		session func(int) string
		saved   []string
	}{
		{"its own session", sessionOf, []string{"other"}},
		{"another session", func(n int) string { return sessionOf(n + 10) }, []string{"legacy 1", "legacy 2", "legacy 3", "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newScriptHub(t, hostOf(0xa), rows...)
			rg := newListRig(t, testEnv(t), newListIndex(t, index...), func(o *listOptions) {
				o.seedWait, o.reseedWait = step, step
				o.ensure = fixed(h.sock)
				o.hosts = registryOf(tc.session, 1, 2, 3)
			})
			if s := rg.until("the seed", 0, func(roster.Snapshot) bool { return true }); !slices.Equal(savedTitles(s), []string{"other"}) {
				t.Fatalf("the registry's seed lists saved %v", savedTitles(s))
			}
			rg.epoch()
			s := rg.until("the hub's rows", 0, func(s roster.Snapshot) bool {
				return len(s.Running) == 3 && s.Running[0].Host.Socket == "" && s.Running[2].Host.Socket == ""
			})
			if !slices.Equal(savedTitles(s), tc.saved) {
				t.Fatalf("saved %v, want %v: %s", savedTitles(s), tc.saved, listed(s))
			}
			for n := 1; n <= 3; n++ {
				r := hostRow(s, hostOf(n))
				want := fmt.Sprintf("p-%d", n)
				if tc.name == "another session" {
					want = ""
				}
				if r.Host.ProviderSessionID != want || r.Host.Socket != "" || r.Session != nil || r.Status == roster.Reachable {
					t.Fatalf("host %d's row: provider session %q (want %q), socket %q, status %v", n,
						r.Host.ProviderSessionID, want, r.Host.Socket, r.Status)
				}
			}
		})
	}
}

// TestARegistryRepairIsPublished (r30 3, 5): a row with no body whose host
// the registry did not list when the hub's reply came is named once the
// registry lists it — on the saved half's next tick, the hub and the index
// quiet — in a fresh Snapshot carrying its provider session id and
// incarnation, never its entry's socket, whether or not the saved rows move
// with it: the name alone (no index row of the session's), and a legacy saved
// row of that session, which the same tick takes out of the saved rows. The
// negative controls: nothing publishes between the registry's change and the
// tick (the wait starts before the change), so a tick that did not read the
// registry would leave both cases unnamed, and one that published only when
// the saved rows moved would leave the first.
func TestARegistryRepairIsPublished(t *testing.T) {
	for _, tc := range []struct {
		name          string
		index         []sessions.Row
		before, after []string
	}{
		{"the name alone", []sessions.Row{savedRow(9, "other", 6)}, []string{"other"}, []string{"other"}},
		{"a legacy saved row", []sessions.Row{legacyRow(1, 9), savedRow(9, "other", 6)},
			[]string{"legacy 1", "other"}, []string{"other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := &changingRegistry{}
			h := newScriptHub(t, hostOf(0xa), bodiless(1, protocol.RosterConnecting))
			rg := newListRig(t, testEnv(t), newListIndex(t, tc.index...), func(o *listOptions) {
				o.seedWait, o.reseedWait = step, step
				o.ensure = fixed(h.sock)
				o.hosts = reg.read
			})
			rg.epoch()
			s := rg.until("the hub's row", 0, func(s roster.Snapshot) bool { return hostRow(s, hostOf(1)) != nil })
			if r := hostRow(s, hostOf(1)); r.Host.ProviderSessionID != "" || r.Host.Incarnation != "" || !slices.Equal(savedTitles(s), tc.before) {
				t.Fatalf("before the registry lists the host: %+v, saved %v", r.Host, savedTitles(s))
			}

			from := rg.mark()
			reg.set(regEntry(1, sessionOf(1)))
			rg.tick()
			s = rg.until("the repaired name", from, func(s roster.Snapshot) bool {
				r := hostRow(s, hostOf(1))
				return r != nil && r.Host.ProviderSessionID == "p-1" && r.Host.Incarnation == "inc-1"
			})
			if r := hostRow(s, hostOf(1)); r.Host.Socket != "" || r.Status != roster.Connecting || !slices.Equal(savedTitles(s), tc.after) {
				t.Fatalf("after the registry lists the host: %+v (%v), saved %v", r.Host, r.Status, savedTitles(s))
			}
		})
	}
}

// TestARowThatDoesNotDecodeIsUnreachable: a roster row whose host row this
// build cannot decode — a member of a type it does not expect — is listed
// unreachable, with no session, as the poller lists a host whose answer does
// not decode; a row that decodes is the hub's, its socket none.
func TestARowThatDoesNotDecodeIsUnreachable(t *testing.T) {
	good := listRowOf(rosterRowOf(1, "one"))
	if r := good.row; good.bodiless || r.Status != roster.Reachable || r.Session == nil || r.Session.Title != "one" || r.Host.Socket != "" {
		t.Fatalf("a good row: %+v", good)
	}
	bad := rosterRowOf(2, "two")
	bad.Row = json.RawMessage(`{"sessionId":"session-2","title":7}`)
	got := listRowOf(bad)
	if r := got.row; !got.bodiless || r.Status != roster.Unreachable || r.Session != nil || r.Host.ID != hostOf(2) || r.Host.CrazeSessionID != sessionOf(2) {
		t.Fatalf("a row that does not decode: %+v", got)
	}
}
