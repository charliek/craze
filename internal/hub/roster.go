package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
)

// The hub's roster (plan 032 §3.6, P1, P4): every live host in the HOME
// registry that has a craze session id, as the hub knows it — what its
// registry entry and its hello say, whether the hub reaches it, and its own
// sessions.list row, forwarded as the JSON value it sent — keyed by host id
// and listed in host-id order.
//
// # Polling (P1)
//
// The hosts are internal/roster's poller's (roster.OpenHub): the list's
// poller — a round of one attempt per host at most every second, eight in
// flight, 500 ms each (connect + hello + list), kept connections, backoff —
// keeping each row's JSON value and with no index. Its registry read is the
// hub's own (hub.pollHosts: rundir.Hosts, which sweeps the dead, so a crashed
// host's row leaves at the next round; the idle rule takes the same read). It
// polls only while a run is open, and one is open while there is demand: a
// subscription, or a sessions.list or sessions.subscribe waiting for its
// answer. The first demand after none opens a new run (Resume), the last
// one's end closes it (Pause), its connections kept for the next.
//
// Every Snapshot the poller publishes is applied here, in order, on the
// poller's goroutine (apply): rows are built for the hosts that can be listed
// — a host id of 12 hex digits, a craze session id of the schema's token
// form, a protocol from 1 — every row whose content moved (its approximate
// flag included) bumps the cursor and is marked pending for every
// subscription, and every waiting answer is woken.
//
// # A row (P4) and its bounds
//
// A roster row is reachable once its host answered and unreachable once an
// attempt failed — its last row kept — and connecting before; it is fresh
// while its last successful read is at most freshFor old, and approximate
// when it is not fresh, not reachable, or cut. protocol's size contract
// (limits.go) is kept in its order, each step marking the row approximate:
// host strings over their bounds cut at a character, a host row over
// RosterRowBytesMax dropped, then the row of a roster row whose encoding is
// still over RosterEntryBytesMax dropped. A row is never null: a host row that
// is not an object is not forwarded. At most RosterRowsMax rows are listed —
// the first in host-id order, the view — and truncated says there were more.
//
// # Answers
//
// sessions.list and sessions.subscribe first wait, bounded by listWait, for
// the open run to have polled every host it lists (Row.Polled: its attempt
// came back in the run, or it is waiting out a backoff), so a cold hub, or a
// hub whose poll was paused, answers what the hosts say now and not a roster
// of connecting rows; hosts not yet polled at the bound are listed as they
// stand. The answer is built under the roster's lock, freshness evaluated
// then too.
//
// A subscription is registered under the same lock as its reply is built,
// with the reply's view as what its client holds (has). From then each change
// marks its host id pending; its flusher (subscription.run) — the
// subscription's one writer, which writes nothing before its reply is
// written — sends the
// net change since what the client last had: each pending id's current row
// if it is in the view, its removal if it is not and the client holds it,
// nothing otherwise (a host that came and went between two flushes). Flushes
// are at least flushEvery apart, one at a time. A notification whose write
// blocks for slowWait ends the subscription: the rest of the line and a
// reset{slow_consumer} follow it, and the connection stays, for a new
// subscription. The teardown ends every subscription with
// reset{hub_closing}, its writes capped at resetWait.
//
// truncated is a reply's alone: a roster notification has no such member. So
// a change that takes the roster over RosterRowsMax rows, or back to them,
// ends every subscription with reset{omitted} — the roster's completeness
// changed — and each subscriber subscribes again, its reply saying which.
//
// On one connection the lines that carry the cursor — a sessions.list reply
// and its subscription's notifications — are written in the order their
// roster was taken: each takes the connection's write lock before the
// roster's, and holds it from its snapshot (list) or its take (take) through
// its write (server.go's conn.list and conn.flush).

// The roster's periods and bounds: variables only so a test can shorten them
// (never in parallel).
var (
	// listWait bounds a sessions.list's or sessions.subscribe's wait for the
	// open run to have polled every host.
	listWait = time.Second
	// flushEvery is the least time between two roster notifications of one
	// subscription.
	flushEvery = 250 * time.Millisecond
	// slowWait is how long a roster notification's write may block before
	// the subscription ends, reset slow_consumer.
	slowWait = 10 * time.Second
	// resetWait bounds the teardown's reset{hub_closing}: the notification in
	// flight, if any, and the reset's own write.
	resetWait = 5 * time.Second
)

// freshFor is how old a row's last successful read may be while it is fresh.
const freshFor = 3 * time.Second

// rosterState is the hub's roster: its rows, its cursor, its subscriptions
// and its demand on the poller, under one lock — the roster lock.
type rosterState struct {
	h *hub
	p *roster.Roster
	// epoch is the hub's id; now the freshness clock (the poller's too).
	epoch string
	now   func() time.Time
	// flushTimer arms a subscription's flush spacing (a test's), nil for a
	// timer; applied is told each Snapshot taken (a test's).
	flushTimer func(time.Duration) (<-chan time.Time, func())
	applied    func(roster.Snapshot)

	// quit is closed as the teardown starts: no answer waits any more.
	quit     chan struct{}
	quitOnce sync.Once

	mu sync.Mutex
	// cursor is the roster sequence: bumped by every change to a row.
	cursor uint64
	// rows is every listed host's, and order their ids, sorted: the view is
	// its first RosterRowsMax.
	rows  map[string]*rosterEntry
	order []string
	subs  map[*subscription]struct{}
	// nextSub numbers the subscriptions (their ids).
	nextSub uint64
	// demand is how many subscriptions and waiting answers want the poll,
	// run the last run asked of it, seenRun the run of the last Snapshot
	// applied and complete whether that Snapshot had every listed host
	// polled.
	demand   int
	run      uint64
	seenRun  uint64
	complete bool
	// regErr is why the last Snapshot's registry read failed (nil: it did
	// not), and readRun the run of the last Snapshot whose read succeeded: a
	// run with no read that succeeded answers no roster (unread).
	regErr  error
	readRun uint64
	// changed is closed, and replaced, by every Snapshot applied.
	changed chan struct{}
	// closed is the teardown's: no answer, subscription or Snapshot more.
	closed bool
}

// rosterEntry is one listed host's row.
type rosterEntry struct {
	// in is the poller's row it was last built from.
	in roster.Row
	// base is the roster row in's bounds leave (the contract's steps 1–2),
	// its approximate false; cut says a step changed it; size is its
	// encoding's length, row kept, approximate false.
	base protocol.RosterRow
	cut  bool
	size int
	// out is the row as listed now.
	out protocol.RosterRow
}

// hubClient is who the roster's poll says it is to a host.
func hubClient(version string) protocol.ClientInfo {
	return protocol.ClientInfo{Kind: "hub", Name: "craze hub", Version: version}
}

// newRoster is the hub's roster, its poll opened paused.
func newRoster(h *hub) *rosterState {
	rs := &rosterState{h: h, epoch: h.id, now: time.Now, quit: make(chan struct{}),
		rows: map[string]*rosterEntry{}, subs: map[*subscription]struct{}{}, changed: make(chan struct{}),
		flushTimer: h.hk.flushTimer, applied: h.hk.rosterApplied}
	if h.hk.rosterNow != nil {
		rs.now = h.hk.rosterNow
	}
	rs.p = roster.OpenHub(roster.HubOptions{
		Hosts:   h.pollHosts,
		Publish: rs.apply,
		Client:  hubClient(h.version),
		Ticks:   h.hk.rosterTicks,
		Now:     h.hk.rosterNow,
		Dial:    h.hk.rosterDial,
		Check:   h.hk.rosterCheck,
		Budget:  h.hk.rosterBudget,
		Paused:  h.hk.rosterPaused,
	})
	return rs
}

// ----------------------------------------------------------------- demand

// acquire is one more demand on the poll: the first after none opens a new
// run. It answers the run the demand waits on.
func (rs *rosterState) acquire() uint64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.demand++
	if rs.demand == 1 && !rs.closed {
		rs.run++
		rs.p.Resume(rs.run)
	}
	return rs.run
}

// release ends one demand: the last closes the run.
func (rs *rosterState) release() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.releaseLocked()
}

func (rs *rosterState) releaseLocked() {
	rs.demand--
	if rs.demand == 0 && !rs.closed {
		rs.p.Pause()
	}
}

// await waits, at most listWait, for run to have polled every host it lists —
// or for the hub to start closing.
func (rs *rosterState) await(run uint64) {
	t := time.NewTimer(listWait)
	defer t.Stop()
	for {
		rs.mu.Lock()
		done := rs.closed || rs.seenRun == run && rs.complete
		ch := rs.changed
		rs.mu.Unlock()
		if done {
			return
		}
		select {
		case <-ch:
		case <-t.C:
			return
		case <-rs.quit:
			return
		}
	}
}

// ------------------------------------------------------------------ rows

// apply takes one Snapshot of the poller's (its Publish, on its goroutine):
// the rows rebuilt, every change counted and marked pending, every waiting
// answer woken.
func (rs *rosterState) apply(snap roster.Snapshot) {
	rs.mu.Lock()
	if rs.closed {
		rs.mu.Unlock()
		return
	}
	now := rs.now()
	seen := make(map[string]bool, len(snap.Running))
	complete, members := true, false
	for i := range snap.Running {
		in := snap.Running[i]
		if !listable(in.Host) {
			continue
		}
		id := in.Host.ID
		seen[id] = true
		if !in.Polled {
			complete = false
		}
		e, ok := rs.rows[id]
		if !ok {
			e = &rosterEntry{}
			rs.rows[id] = e
			members = true
		}
		if !ok || !sameInput(e.in, in) {
			e.build(in)
		}
		e.in = in
		rs.settleLocked(id, e, now, !ok)
	}
	var gone []string
	for id := range rs.rows {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	for _, id := range gone {
		delete(rs.rows, id)
		rs.cursor++
		rs.touchLocked(id)
		members = true
	}
	reset, truncated := 0, false
	if members {
		was := len(rs.order) > protocol.RosterRowsMax
		rs.reorderLocked()
		if truncated = len(rs.order) > protocol.RosterRowsMax; truncated != was {
			reset = rs.resetAllLocked(protocol.ResetOmitted)
		}
	}
	rs.seenRun, rs.complete = snap.Run, complete
	if rs.regErr = snap.RegistryErr; rs.regErr == nil {
		rs.readRun = snap.Run
	}
	close(rs.changed)
	rs.changed = make(chan struct{})
	rs.wakeLocked()
	rs.mu.Unlock()
	if reset > 0 {
		state := "whole again"
		if truncated {
			state = fmt.Sprintf("over %d rows, truncated", protocol.RosterRowsMax)
		}
		rs.h.logf("the roster is %s: %d roster subscription(s) reset omitted", state, reset)
	}
	if rs.applied != nil {
		rs.applied(snap)
	}
}

// resetAllLocked ends every subscription for why — its reset written by its
// flusher (subscription.final) — and answers how many there were. The
// roster's completeness (truncated) is the reply's alone, which a roster
// notification cannot carry: a subscriber holding a roster whose truncated
// has flipped is told to subscribe again, its new reply saying which (r19 3).
func (rs *rosterState) resetAllLocked(why protocol.ResetReason) int {
	n := 0
	for s := range rs.subs {
		if rs.endLocked(s, why) {
			n++
		}
	}
	return n
}

// settleLocked brings e's listed row to now: a new row, or one whose content
// moved, bumps the cursor and is pending for every subscription.
func (rs *rosterState) settleLocked(id string, e *rosterEntry, now time.Time, isNew bool) {
	out := e.at(now)
	if !isNew && sameRow(e.out, out) {
		return
	}
	e.out = out
	rs.cursor++
	rs.touchLocked(id)
}

// reevaluateLocked settles every row at now: freshness is a matter of time,
// so an answer reads it again as it is built.
func (rs *rosterState) reevaluateLocked(now time.Time) {
	for _, id := range rs.order {
		rs.settleLocked(id, rs.rows[id], now, false)
	}
	rs.wakeLocked()
}

// reorderLocked sorts the listed ids again after a host came or went, and
// marks pending every id that came into the view or left it beside them.
func (rs *rosterState) reorderLocked() {
	before := rs.order
	rs.order = make([]string, 0, len(rs.rows))
	for id := range rs.rows {
		rs.order = append(rs.order, id)
	}
	sort.Strings(rs.order)
	if len(before) <= protocol.RosterRowsMax && len(rs.order) <= protocol.RosterRowsMax {
		return // every row is in the view: a change is its own row's
	}
	was := make(map[string]bool, protocol.RosterRowsMax)
	for _, id := range before[:min(len(before), protocol.RosterRowsMax)] {
		was[id] = true
	}
	for _, id := range rs.order[:min(len(rs.order), protocol.RosterRowsMax)] {
		if !was[id] {
			rs.touchLocked(id)
		}
		delete(was, id)
	}
	for id := range was {
		rs.touchLocked(id)
	}
}

// inViewLocked says id's row is listed: one of the first RosterRowsMax.
func (rs *rosterState) inViewLocked(id string) bool {
	i := sort.SearchStrings(rs.order, id)
	return i < len(rs.order) && rs.order[i] == id && i < protocol.RosterRowsMax
}

// viewLocked is the listed rows, and whether there were more.
func (rs *rosterState) viewLocked() ([]protocol.RosterRow, bool) {
	n := min(len(rs.order), protocol.RosterRowsMax)
	rows := make([]protocol.RosterRow, 0, n)
	for _, id := range rs.order[:n] {
		rows = append(rows, rs.rows[id].out)
	}
	return rows, len(rs.order) > n
}

// listLine is the line answering a sessions.list of run's with id: the hub's
// roster now, or its refusal while run has read no registry (unreadLocked),
// or response_too_large for a reply over the line limit (encodeReply); false
// once the hub closes. sub, when not nil, is the subscription of the
// connection the line is written to: a client that is answered the roster
// takes its rows for its own, so they become what sub's client holds
// (rebaseLocked) — only then. A refused or too-large answer gives the client
// nothing in their place, so sub's membership and what is pending stay as
// they were (r25 1). The reply is encoded under the roster's lock, beside the
// snapshot it is of: the rebase and the snapshot stay one section.
func (rs *rosterState) listLine(run uint64, sub *subscription, id json.RawMessage) ([]byte, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.closed {
		return nil, false
	}
	if perr := rs.unreadLocked(run); perr != nil {
		return errorLine(id, perr), true
	}
	rs.reevaluateLocked(rs.now())
	rows, truncated := rs.viewLocked()
	line, fits := encodeReply(id, protocol.HubSessionsListResult{Epoch: rs.epoch, Cursor: rs.cursor, Sessions: rows,
		Truncated: truncated})
	if fits && sub != nil && !sub.ended {
		sub.rebaseLocked(rows)
	}
	return line, true
}

// unreadLocked is the refusal of an answer of run's when the hub knows no
// roster: the last registry read failed and none has succeeded in run — a
// hub whose registry cannot be read answers that, never an empty roster
// (r23 4). unavailable, reason host_unreachable — session.connect's own
// answer when the registry cannot be read — and a message with no path in
// it: the hub's log has the read's error.
func (rs *rosterState) unreadLocked(run uint64) *protocol.Error {
	if rs.regErr == nil || rs.readRun == run {
		return nil
	}
	return refused(protocol.CodeUnavailable, protocol.ReasonHostUnreachable,
		"the hub cannot read the registry of session hosts, so it knows no roster")
}

// listable says a host can be listed (limits.go): a host id of 12 lowercase
// hex digits, a craze session id of the schema's token form, and a protocol
// the schema allows.
func listable(h roster.Host) bool {
	return rundir.ValidHostID(h.ID) && validSessionID(h.CrazeSessionID) && h.Protocol >= 1
}

// validSessionID is the roster row's sessionId pattern,
// ^[A-Za-z0-9._-]{1,128}$.
func validSessionID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// build makes e's bounded row from in (the contract's steps 1 and 2) and
// measures it.
func (e *rosterEntry) build(in roster.Row) {
	r := protocol.RosterRow{HostID: in.Host.ID, SessionID: in.Host.CrazeSessionID, Status: statusOf(in.Status),
		Host: protocol.RosterHost{PID: in.Host.PID, Protocol: in.Host.Protocol, StartedAt: in.Host.StartedAt, Ready: in.Host.Ready}}
	var c1, c2, c3 bool
	r.Host.CrazeVersion, c1 = cutChars(in.Version, protocol.RosterCrazeVersionMax)
	r.Host.Provider, c2 = cutChars(in.Host.Provider, protocol.RosterProviderMax)
	r.Host.Workspace, c3 = cutChars(in.Host.Workspace, protocol.RosterWorkspaceMax)
	cut := c1 || c2 || c3
	switch raw := in.Raw; {
	case len(raw) == 0:
		// None read yet — or a host that answered with a row that is not an
		// object, which is never forwarded (a row is an object or absent).
		cut = cut || in.Status == roster.Reachable
	case !isObject(raw) || len(raw) > protocol.RosterRowBytesMax:
		cut = true
	default:
		r.Row = raw
	}
	e.base, e.cut, e.size = r, cut, encodedLen(r)
}

// at is e's row at now: approximate when it is cut, not reachable or not
// fresh, and its row dropped (the contract's step 3) when the whole would
// still be over RosterEntryBytesMax.
func (e *rosterEntry) at(now time.Time) protocol.RosterRow {
	r := e.base
	fresh := e.in.Status == roster.Reachable && !e.in.ReadAt.IsZero() && now.Sub(e.in.ReadAt) <= freshFor
	r.Approximate = e.cut || !fresh
	size := e.size
	if r.Approximate {
		size -= len("false") - len("true")
	}
	if size > protocol.RosterEntryBytesMax && r.Row != nil {
		r.Row, r.Approximate = nil, true
	}
	return r
}

// encodedLen is r's encoding's length — approximate false, the longer — as
// the hub writes it (protocol.MarshalLine, its newline not counted).
func encodedLen(r protocol.RosterRow) int {
	r.Approximate = false
	b, err := protocol.MarshalLine(r)
	if err != nil {
		// Nothing in a roster row fails to encode; were it to, the row is
		// never forwarded.
		return protocol.RosterEntryBytesMax + 1
	}
	return len(b) - 1
}

func statusOf(s roster.Status) protocol.RosterStatus {
	switch s {
	case roster.Reachable:
		return protocol.RosterReachable
	case roster.Unreachable:
		return protocol.RosterUnreachable
	}
	return protocol.RosterConnecting
}

// cutChars is s cut to at most max characters — as JSON Schema's maxLength
// counts them, one per code point, an invalid byte one (the encoder writes it
// as U+FFFD) — at a character boundary, and whether it was cut.
func cutChars(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i], true
		}
		n++
	}
	return s, false
}

// isObject says raw, one JSON value, is an object.
func isObject(raw json.RawMessage) bool {
	t := bytes.TrimLeft(raw, " \t\r\n")
	return len(t) > 0 && t[0] == '{'
}

// sameInput says two of the poller's rows build the same roster row: every
// member the row is made of, Raw by content. ReadAt is not one: it moves
// freshness only (at).
func sameInput(a, b roster.Row) bool {
	ah, bh := a.Host, b.Host
	return ah.ID == bh.ID && ah.PID == bh.PID && ah.Protocol == bh.Protocol && ah.Provider == bh.Provider &&
		ah.Workspace == bh.Workspace && ah.Ready == bh.Ready && ah.CrazeSessionID == bh.CrazeSessionID &&
		sameTime(ah.StartedAt, bh.StartedAt) && a.Status == b.Status && a.Version == b.Version && sameBytes(a.Raw, b.Raw)
}

// sameRow says two roster rows encode the same.
func sameRow(a, b protocol.RosterRow) bool {
	return a.HostID == b.HostID && a.SessionID == b.SessionID && a.Status == b.Status && a.Approximate == b.Approximate &&
		a.Host.PID == b.Host.PID && a.Host.CrazeVersion == b.Host.CrazeVersion && a.Host.Protocol == b.Host.Protocol &&
		a.Host.Provider == b.Host.Provider && a.Host.Workspace == b.Host.Workspace && a.Host.Ready == b.Host.Ready &&
		sameTime(a.Host.StartedAt, b.Host.StartedAt) && sameBytes(a.Row, b.Row)
}

// sameTime says two times are one instant written the same way: its zone's
// offset is in the encoding.
func sameTime(a, b time.Time) bool {
	if !a.Equal(b) {
		return false
	}
	_, ao := a.Zone()
	_, bo := b.Zone()
	return ao == bo
}

// sameBytes is bytes.Equal, with the same backing array — a row the poller
// handed on unchanged — found at once.
func sameBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 || &a[0] == &b[0] {
		return true
	}
	return bytes.Equal(a, b)
}

// --------------------------------------------------------- subscriptions

// subscription is one connection's roster subscription.
type subscription struct {
	id string
	c  *conn
	rs *rosterState

	// Under the roster lock: the host ids whose rows moved since the last
	// flush; the ids the client holds — the view as of its last flush, or of
	// the reply; and whether, and why, the subscription ended ("" for the
	// connection's close).
	pending map[string]struct{}
	has     map[string]bool
	ended   bool
	why     protocol.ResetReason

	// wake says pending is not empty (a slot of one); stop is closed as the
	// subscription ends; replied once the reply's write is over, written or
	// not; done once the flusher has returned.
	wake    chan struct{}
	stop    chan struct{}
	replied chan struct{}
	done    chan struct{}
}

// subscribe registers a subscription of c's for an answer of run's, under
// the roster lock that builds its reply — or answers its refusal while run
// has read no registry (unreadLocked), registering nothing: false once the
// hub closes. Its flusher, started by the caller, writes nothing before its
// reply is written (replied).
func (rs *rosterState) subscribe(c *conn, run uint64) (*subscription, protocol.SessionsSubscribeResult, *protocol.Error, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.closed {
		return nil, protocol.SessionsSubscribeResult{}, nil, false
	}
	if perr := rs.unreadLocked(run); perr != nil {
		return nil, protocol.SessionsSubscribeResult{}, perr, true
	}
	rs.reevaluateLocked(rs.now())
	rows, truncated := rs.viewLocked()
	rs.nextSub++
	s := &subscription{id: fmt.Sprintf("r-%d", rs.nextSub), c: c, rs: rs,
		pending: map[string]struct{}{}, has: make(map[string]bool, len(rows)),
		wake: make(chan struct{}, 1), stop: make(chan struct{}), replied: make(chan struct{}), done: make(chan struct{})}
	for _, r := range rows {
		s.has[r.HostID] = true
	}
	rs.subs[s] = struct{}{}
	return s, protocol.SessionsSubscribeResult{Subscription: s.id, Epoch: rs.epoch, Cursor: rs.cursor,
		Sessions: rows, Truncated: truncated}, nil, true
}

// rebaseLocked makes rows — a sessions.list reply's, written on s's
// connection — what s's client holds, and drops what was pending: the reply
// carries it. A change after the list's snapshot marks its row pending again,
// and s's flusher, which writes after the reply (the connection's write lock,
// held from the snapshot through the write), sends it as a change from these
// rows — a host the reply listed and that has gone since included (r23 1).
func (s *subscription) rebaseLocked(rows []protocol.RosterRow) {
	s.has = make(map[string]bool, len(rows))
	for _, r := range rows {
		s.has[r.HostID] = true
	}
	clear(s.pending)
}

// abandon ends s, which its connection never told its client of — its
// reply could not be written as a result (r23 5) — before its flusher was
// started: unregistered, its demand released, and done.
func (s *subscription) abandon() {
	s.rs.end(s, "")
	close(s.replied)
	close(s.done)
}

// live says s has not ended.
func (rs *rosterState) live(s *subscription) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return !s.ended
}

// reason is why s ended.
func (rs *rosterState) reason(s *subscription) protocol.ResetReason {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return s.why
}

// end ends s for why, once: unregistered, its demand released, its flusher
// told. False when it had ended already.
func (rs *rosterState) end(s *subscription, why protocol.ResetReason) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.endLocked(s, why)
}

func (rs *rosterState) endLocked(s *subscription, why protocol.ResetReason) bool {
	if s.ended {
		return false
	}
	s.ended, s.why = true, why
	// Neither is read again (take, rebaseLocked check ended): released now,
	// not when the connection closes (r23 3).
	s.pending, s.has = nil, nil
	delete(rs.subs, s)
	rs.releaseLocked()
	close(s.stop)
	return true
}

// touchLocked marks id pending for every subscription.
func (rs *rosterState) touchLocked(id string) {
	for s := range rs.subs {
		s.pending[id] = struct{}{}
	}
}

// wakeLocked wakes the flusher of every subscription with something pending.
func (rs *rosterState) wakeLocked() {
	for s := range rs.subs {
		if len(s.pending) > 0 {
			select {
			case s.wake <- struct{}{}:
			default:
			}
		}
	}
}

// take is s's next notification: the net change since what its client holds
// (the file's comment), and false when there is none.
func (rs *rosterState) take(s *subscription) (protocol.RosterParams, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if s.ended || len(s.pending) == 0 {
		return protocol.RosterParams{}, false
	}
	ids := make([]string, 0, len(s.pending))
	for id := range s.pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	clear(s.pending)
	p := protocol.RosterParams{Subscription: s.id, Epoch: rs.epoch, Cursor: rs.cursor,
		Upserts: []protocol.RosterRow{}, Removes: []string{}}
	for _, id := range ids {
		switch {
		case rs.inViewLocked(id):
			p.Upserts = append(p.Upserts, rs.rows[id].out)
			s.has[id] = true
		case s.has[id]:
			p.Removes = append(p.Removes, id)
			delete(s.has, id)
		}
	}
	return p, len(p.Upserts) > 0 || len(p.Removes) > 0
}

// run is s's flusher: its one writer once its reply is written, until it
// ends — the file's comment.
func (s *subscription) run() {
	defer close(s.done)
	defer s.final()
	select {
	case <-s.replied:
	case <-s.stop:
		// Its end is written after its reply, never before.
		<-s.replied
		return
	}
	var last time.Time
	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
		}
		if !last.IsZero() {
			if wait := flushEvery - time.Since(last); wait > 0 || s.rs.flushTimer != nil {
				fire, cancel := s.timer(wait)
				select {
				case <-s.stop:
					cancel()
					return
				case <-fire:
				}
			}
		}
		last = time.Now()
		if !s.c.flush(s) {
			return
		}
	}
}

func (s *subscription) timer(d time.Duration) (<-chan time.Time, func()) {
	if s.rs.flushTimer != nil {
		return s.rs.flushTimer(d)
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// final writes the subscription's reset as the flusher returns — after its
// reply and any notification in flight — for a subscription the teardown
// ended (hub_closing) or the roster's completeness did (omitted); a slow
// consumer's reset is written with the line it cut (conn.notifyLocked), and a
// closed connection's is nobody's.
func (s *subscription) final() {
	switch why := s.rs.reason(s); why {
	case protocol.ResetHubClosing, protocol.ResetOmitted:
		s.c.writeReset(s.id, why)
	}
}

// ---------------------------------------------------------------- teardown

// quiesce is the teardown's start: no answer waits for the poll any more.
func (rs *rosterState) quiesce() {
	rs.quitOnce.Do(func() { close(rs.quit) })
}

// closeSubscriptions ends every subscription with reset{hub_closing}: each
// connection's writes capped at resetWait from now (and the teardown's
// deadline), each flusher waited for until then. Nothing is admitted after.
func (rs *rosterState) closeSubscriptions(deadline time.Time) {
	rs.mu.Lock()
	rs.closed = true
	subs := make([]*subscription, 0, len(rs.subs))
	for s := range rs.subs {
		subs = append(subs, s)
	}
	close(rs.changed)
	rs.changed = make(chan struct{})
	rs.mu.Unlock()
	at := time.Now().Add(resetWait)
	if deadline.Before(at) {
		at = deadline
	}
	for _, s := range subs {
		s.c.capWrites(at)
		rs.end(s, protocol.ResetHubClosing)
	}
	for _, s := range subs {
		select {
		case <-s.done:
		case <-time.After(time.Until(at) + 100*time.Millisecond):
			rs.h.logf("roster subscription %s did not end within %v", s.id, resetWait)
		}
	}
	if len(subs) > 0 {
		rs.h.logf("ended %d roster subscription(s): hub_closing", len(subs))
	}
}

// stop stops the poll, joining every goroutine of its.
func (rs *rosterState) stop() { rs.p.Close() }
