package hub

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/control/wiretest"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
)

// The hub's roster (plan 032 §3.6, §3.18): hosts appearing, changing and
// dying; identity-less and malformed hosts omitted; a host dying between
// bind and identity; an unreachable host's last row; freshness; unknown
// members end to end; the size contract, step by step; more hosts than a
// roster holds; the net change; a slow subscriber; the reply before any
// notification; one subscription per connection; the teardown's reset; A9's
// bound; the poll's demand; the list's wait. The rig is
// roster_helpers_test.go's: every schedule — the poll's tick and clock, the
// flush spacing — is the test's, and each lifecycle test is also run under a
// 5% CPU quota.

// checkRow holds a listed row to what memHosts' host n says.
func checkRow(t *testing.T, r *protocol.RosterRow, n int, row string, status protocol.RosterStatus, approximate bool) {
	t.Helper()
	switch {
	case r == nil:
		t.Fatalf("host %s is not listed", hostOf(n))
	case r.SessionID != sessionOf(n) || r.Status != status || r.Approximate != approximate || string(r.Row) != row:
		t.Fatalf("host %s's row: %+v (row %s), want %s, approximate %v, row %s", hostOf(n), *r, r.Row, status, approximate, row)
	case r.Host != (protocol.RosterHost{PID: 1000 + n, CrazeVersion: "0.0.0-mem", Protocol: 1, Provider: "cursor",
		Workspace: "/work/" + hostOf(n), StartedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), Ready: true}):
		t.Fatalf("host %s's host: %+v", hostOf(n), r.Host)
	}
}

// TestTheRosterFollowsItsHosts (§3.6): a subscriber's reply lists every host
// reachable and fresh, its row the JSON value the host sent, in host-id
// order, at the roster's cursor; then a host appearing is upserted (connecting
// until its answer, reachable with its row after), a host's change is
// upserted, and a host leaving the registry is removed — each in the round
// that reads it, each notification's cursor past the last. The negative
// control: a round that changes nothing sends nothing, so each notification
// that follows is its change alone.
func TestTheRosterFollowsItsHosts(t *testing.T) {
	m := newMemHosts(t)
	a := m.add(1, memRow(1, "one"))
	b := m.add(2, memRow(2, "two"))
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, gate: true, hk: func(hk *hooks) { installMem(t, hk, m) }})
	p := dialPeer(t, rg.sock)
	sub := p.subscribe()
	if sub.Subscription != "r-1" || sub.Epoch != rg.h.id || sub.Cursor != rg.cursor() || sub.Truncated ||
		!slices.Equal(ids(sub.Sessions), []string{a, b}) {
		t.Fatalf("the reply: %+v", sub)
	}
	checkRow(t, rowOf(sub.Sessions, a), 1, memRow(1, "one"), protocol.RosterReachable, false)
	checkRow(t, rowOf(sub.Sessions, b), 2, memRow(2, "two"), protocol.RosterReachable, false)

	// A round that changes nothing sends nothing.
	from := rg.snaps.mark()
	rg.clk.advance(time.Second)
	rg.tick()
	rg.applied("the quiet round read both", from, readAt(rg.clk.now(), a, b))
	p.nothing("a round that changed nothing")

	// A host appears.
	c := m.add(3, memRow(3, "three"))
	rg.clk.advance(time.Second)
	rg.tick()
	up := p.roster()
	if len(up.Removes) != 0 || !slices.Equal(ids(up.Upserts), []string{c}) || up.Cursor <= sub.Cursor || up.Subscription != "r-1" || up.Epoch != rg.h.id {
		t.Fatalf("a host appearing: %+v", up)
	}
	if up.Upserts[0].Status == protocol.RosterConnecting {
		if r := up.Upserts[0]; !r.Approximate || r.Row != nil {
			t.Fatalf("a connecting row: %+v", r)
		}
		rg.applied("the appeared host answered", 0, listedIn(false, c))
		rg.flush.open(t)
		next := p.roster()
		if next.Cursor <= up.Cursor || !slices.Equal(ids(next.Upserts), []string{c}) {
			t.Fatalf("the appeared host answering: %+v", next)
		}
		up = next
	}
	checkRow(t, &up.Upserts[0], 3, memRow(3, "three"), protocol.RosterReachable, false)

	// A host changes.
	m.setRow(a, memRow(1, "one, changed"))
	rg.clk.advance(time.Second)
	from = rg.snaps.mark()
	rg.tick()
	rg.applied("the change read", from, titled(a, "one, changed"))
	rg.flush.open(t)
	ch := p.roster()
	if len(ch.Removes) != 0 || len(ch.Upserts) != 1 || ch.Cursor <= up.Cursor {
		t.Fatalf("a host changing: %+v", ch)
	}
	checkRow(t, &ch.Upserts[0], 1, memRow(1, "one, changed"), protocol.RosterReachable, false)

	// A host leaves the registry.
	m.remove(b)
	rg.clk.advance(time.Second)
	from = rg.snaps.mark()
	rg.tick()
	rg.applied("the host gone", from, func(s roster.Snapshot) bool { return snapRow(s, b) == nil })
	rg.flush.open(t)
	gone := p.roster()
	if len(gone.Upserts) != 0 || !slices.Equal(gone.Removes, []string{b}) || gone.Cursor <= ch.Cursor {
		t.Fatalf("a host leaving: %+v", gone)
	}
	l := dialPeer(t, rg.sock).list()
	if !slices.Equal(ids(l.Sessions), []string{a, c}) || l.Cursor != gone.Cursor || l.Epoch != rg.h.id {
		t.Fatalf("the list after: %+v", l)
	}
}

// titled is a Snapshot predicate: id listed, its row titled title.
func titled(id, title string) func(roster.Snapshot) bool {
	return func(s roster.Snapshot) bool {
		r := snapRow(s, id)
		return r != nil && strings.Contains(string(r.Raw), `"title":"`+title+`"`)
	}
}

// readAt is a Snapshot predicate: every id listed, last read at at.
func readAt(at time.Time, ids ...string) func(roster.Snapshot) bool {
	return func(s roster.Snapshot) bool {
		for _, id := range ids {
			if r := snapRow(s, id); r == nil || !r.ReadAt.Equal(at) {
				return false
			}
		}
		return true
	}
}

// TestHostsThatCannotBeListedAreOmitted (P4, limits.go): a host with no
// craze session id yet, one whose session id is not the schema's token, one
// whose entry names a protocol the schema refuses and one whose id is not a
// host id are not listed — the negative control: the registry lists every
// one of them, and the idle rule, fed by the poll's own read, counts them —
// and the identity-less host is listed once it has a session.
func TestHostsThatCannotBeListedAreOmitted(t *testing.T) {
	m := newMemHosts(t)
	looped := make(chan struct{})
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, hk: func(hk *hooks) {
		installMem(t, hk, m)
		hk.looping = func() { close(looped) }
	}})
	<-looped // the loop's own first read is done: the idle rule has seen no host
	a := m.add(1, memRow(1, "one"))
	odd := []rundir.Entry{
		{Protocol: 1, HostID: hostOf(2), Ready: false},
		{Protocol: 1, HostID: hostOf(3), CrazeSessionID: "not/a/token"},
		{Protocol: 1, HostID: hostOf(4), CrazeSessionID: strings.Repeat("s", 129)},
		{Protocol: 0, HostID: hostOf(5), CrazeSessionID: sessionOf(5)},
		{Protocol: 1, HostID: "00000000000G", CrazeSessionID: sessionOf(6)},
	}
	for _, e := range odd {
		e.Socket = memSocket(e.HostID)
		m.put(e, memRow(0, "odd"))
	}
	hostsLive := func() (bool, int) {
		rg.h.life.mu.Lock()
		defer rg.h.life.mu.Unlock()
		return rg.h.life.hostsLive, len(rg.h.life.hostIDs)
	}
	if live, _ := hostsLive(); live {
		t.Fatal("the idle rule saw a host before any read of the registry since it was listed")
	}
	q := dialPeer(t, rg.sock)
	if l := q.list(); !slices.Equal(ids(l.Sessions), []string{a}) {
		t.Fatalf("listed %v, want %s alone", ids(l.Sessions), a)
	}
	if live, n := hostsLive(); !live || n != 1+len(odd) {
		t.Fatalf("the idle rule, after the poll's read: live %v, %d hosts; want all %d", live, n, 1+len(odd))
	}

	m.put(rundir.Entry{Protocol: 1, HostID: hostOf(2), Socket: memSocket(hostOf(2)), CrazeSessionID: sessionOf(2), Ready: true},
		memRow(2, "two"))
	if l := q.list(); !slices.Equal(ids(l.Sessions), []string{a, hostOf(2)}) {
		t.Fatalf("after the identity: listed %v", ids(l.Sessions))
	}
}

// TestAHostDyingBetweenBindAndIdentity: a real host bound in the registry
// with no session yet, which crashes before it has one, is never listed —
// its subscriber is told nothing — and the round that reads the registry
// sweeps its entry. The negative control: a host with its identity, crashed
// alongside it, is removed in that same round.
func TestAHostDyingBetweenBindAndIdentity(t *testing.T) {
	env := testEnv(t)
	rg := newRosterRig(t, env, rigOpts{clock: true})
	early, known := hostOf(7), hostOf(8)
	hc := startHostChild(t, env, early, "")
	hk := startHostChild(t, env, known, sessionOf(8))
	setVar(t, &listWait, step)
	p := dialPeer(t, rg.sock)
	if sub := p.subscribe(); !slices.Equal(ids(sub.Sessions), []string{known}) {
		t.Fatalf("listed %v, want %s alone", ids(sub.Sessions), known)
	}
	hc.crash(t)
	hk.crash(t)
	rg.tick()
	n := p.roster()
	if len(n.Upserts) != 0 || !slices.Equal(n.Removes, []string{known}) {
		t.Fatalf("after both crashed: %+v, want the known host's removal alone", n)
	}
	hostsDir := filepath.Join(env.Home, ".cache", "craze", "hosts")
	for _, id := range []string{early, known} {
		if _, err := os.Lstat(filepath.Join(hostsDir, id+".json")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("host %s's entry was not swept by the round's read (%v)", id, err)
		}
	}
	p.nothing("after the removal")
}

// TestAnUnreachableHostKeepsItsLastRow (§3.6): a row is fresh while its last
// read is at most freshFor old — at the bound still fresh, the negative
// control, and a nanosecond past it approximate, which a list reads and its
// subscriber is told — and fresh again at the next read; a host that stops
// answering is unreachable and approximate, its last row kept.
func TestAnUnreachableHostKeepsItsLastRow(t *testing.T) {
	m := newMemHosts(t)
	a := m.add(1, memRow(1, "one"))
	b := m.add(2, memRow(2, "two"))
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, gate: true, hk: func(hk *hooks) { installMem(t, hk, m) }})
	p := dialPeer(t, rg.sock)
	sub := p.subscribe()
	q := dialPeer(t, rg.sock)

	rg.clk.advance(freshFor)
	if l := q.list(); l.Cursor != sub.Cursor || rowOf(l.Sessions, a).Approximate || rowOf(l.Sessions, b).Approximate {
		t.Fatalf("at the freshness bound: %+v, want both fresh at cursor %d", l, sub.Cursor)
	}
	p.nothing("at the freshness bound")
	rg.clk.advance(time.Nanosecond)
	l := q.list()
	if l.Cursor != sub.Cursor+2 {
		t.Fatalf("past the bound the cursor is %d, want %d (two flips)", l.Cursor, sub.Cursor+2)
	}
	checkRow(t, rowOf(l.Sessions, a), 1, memRow(1, "one"), protocol.RosterReachable, true)
	checkRow(t, rowOf(l.Sessions, b), 2, memRow(2, "two"), protocol.RosterReachable, true)
	stale := p.roster()
	if !slices.Equal(ids(stale.Upserts), []string{a, b}) || stale.Cursor != l.Cursor || !stale.Upserts[0].Approximate {
		t.Fatalf("the subscriber told of the flips: %+v", stale)
	}

	from := rg.snaps.mark()
	rg.tick()
	rg.applied("both read again", from, readAt(rg.clk.now(), a, b))
	rg.flush.open(t)
	if fresh := p.roster(); !slices.Equal(ids(fresh.Upserts), []string{a, b}) || fresh.Upserts[0].Approximate || fresh.Upserts[1].Approximate {
		t.Fatalf("read again: %+v, want both fresh", fresh)
	}

	m.fail(a)
	from = rg.snaps.mark()
	rg.tick()
	rg.applied("a unreachable", from, func(s roster.Snapshot) bool {
		r := snapRow(s, a)
		return r != nil && r.Status == roster.Unreachable
	})
	rg.flush.open(t)
	down := p.roster()
	if len(down.Upserts) != 1 || len(down.Removes) != 0 {
		t.Fatalf("a host that stopped answering: %+v", down)
	}
	checkRow(t, &down.Upserts[0], 1, memRow(1, "one"), protocol.RosterUnreachable, true)
}

// TestUnknownMembersPassThroughTheHub (P4, 02's rule): a newer host's row —
// members this build does not know, at any depth — reaches a list, a
// subscription's reply and a roster notification as the value the host
// sent, each line held to the hub's schema (peer.read). The negative control:
// the same row decoded and encoded again, as a hub that re-encoded it would,
// loses them.
func TestUnknownMembersPassThroughTheHub(t *testing.T) {
	const newer = `{"sessionId":"session-1","activity":"idle","title":"x","attached":2,` +
		`"future":{"nested":[1,{"deep":true,"n":1.50}],"big":12345678901234567890}}`
	newer2 := strings.Replace(newer, `"deep":true`, `"deep":false`, 1)
	m := newMemHosts(t)
	a := m.add(1, newer)
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, hk: func(hk *hooks) { installMem(t, hk, m) }})
	if r := rowOf(dialPeer(t, rg.sock).list().Sessions, a); r == nil || string(r.Row) != newer {
		t.Fatalf("the list's row: %+v", r)
	}
	p := dialPeer(t, rg.sock)
	if r := rowOf(p.subscribe().Sessions, a); r == nil || string(r.Row) != newer {
		t.Fatalf("the subscription's reply's row: %+v", r)
	}
	m.setRow(a, newer2)
	rg.clk.advance(time.Second)
	rg.tick()
	if n := p.roster(); len(n.Upserts) != 1 || string(n.Upserts[0].Row) != newer2 {
		t.Fatalf("the notification's row: %+v", n)
	}

	var decoded protocol.SessionRow
	if err := json.Unmarshal([]byte(newer), &decoded); err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(again), "future") {
		t.Fatalf("the control is no control: a decoded row keeps the unknown member: %s", again)
	}
}

// TestAModelChangeIsUpserted (plan 035 C11, SF-114): a host's row carrying
// its model reaches a subscriber as the host sent it, and a change of the
// model alone is upserted in the round that reads it, the row again as sent;
// a rowFacts host's row from before 035, which says no model, is forwarded
// with none — the hub adds nothing. The negative control: a round that
// changes nothing sends nothing, so the upsert that follows is the model's.
func TestAModelChangeIsUpserted(t *testing.T) {
	const facts = `"capabilities":{"rowFacts":true},"prompted":true`
	withModel := func(model string) string {
		return `{"sessionId":"` + sessionOf(1) + `","activity":"idle","title":"x",` + facts + `,"model":"` + model + `"}`
	}
	older := `{"sessionId":"` + sessionOf(2) + `","activity":"idle","title":"y",` + facts + `}`
	m := newMemHosts(t)
	a := m.add(1, withModel("grok-4.7-build-fast"))
	b := m.add(2, older)
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, hk: func(hk *hooks) { installMem(t, hk, m) }})
	p := dialPeer(t, rg.sock)
	sub := p.subscribe()
	if r := rowOf(sub.Sessions, a); r == nil || string(r.Row) != withModel("grok-4.7-build-fast") {
		t.Fatalf("the model's row in the reply: %+v", r)
	}
	if r := rowOf(sub.Sessions, b); r == nil || string(r.Row) != older {
		t.Fatalf("an older rowFacts host's row in the reply: %+v", r)
	}

	from := rg.snaps.mark()
	rg.clk.advance(time.Second)
	rg.tick()
	rg.applied("the quiet round read both", from, readAt(rg.clk.now(), a, b))
	p.nothing("a round that changed nothing")

	m.setRow(a, withModel("fast"))
	rg.clk.advance(time.Second)
	rg.tick()
	if n := p.roster(); len(n.Removes) != 0 || len(n.Upserts) != 1 || n.Upserts[0].HostID != a ||
		string(n.Upserts[0].Row) != withModel("fast") {
		t.Fatalf("the model's change: %+v", n)
	}
}

// TestTheSizeContract (limits.go, C9 r13 3): each step of the contract, in
// its order, each marking the row approximate — host strings cut at a
// character past their bounds (and not at them), a host row over 16 KiB
// dropped (and not at 16 KiB), the row of a roster row still over 32,000
// bytes dropped (and not at 32,000; and where approximate's shorter encoding
// brings it to 32,000, kept) — a row never null, and every roster row on the
// schema and within its bound.
func TestTheSizeContract(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	base := roster.Row{Host: roster.Host{ID: hostOf(1), PID: 9, Protocol: 1, CrazeSessionID: sessionOf(1), Provider: "cursor",
		Workspace: "/w", Ready: true, StartedAt: now}, Status: roster.Reachable, Version: "1.2.3", ReadAt: now,
		Raw: json.RawMessage(`{"sessionId":"session-1"}`)}
	at := func(in roster.Row) protocol.RosterRow {
		var e rosterEntry
		e.build(in)
		e.in = in
		return e.at(now)
	}
	// rowOfLen is a host row, an object, of exactly n bytes.
	rowOfLen := func(n int) json.RawMessage {
		return json.RawMessage(`{"p":"` + strings.Repeat("x", n-len(`{"p":""}`)) + `"}`)
	}
	ctl := strings.Repeat("\x01", protocol.RosterWorkspaceMax) // six bytes each, escaped
	// entryOf is in with a host row padding its roster row's encoding,
	// approximate false, to exactly n bytes.
	entryOf := func(in roster.Row, n int) roster.Row {
		in.Raw = rowOfLen(len(`{"p":""}`))
		var e rosterEntry
		e.build(in)
		in.Raw = rowOfLen(len(`{"p":""}`) + n - e.size)
		e.build(in)
		if e.size != n {
			t.Fatalf("padded to %d bytes, want %d", e.size, n)
		}
		return in
	}
	big := base
	big.Host.Workspace = ctl
	unreachable := big
	unreachable.Status = roster.Unreachable
	unreachable = entryOf(unreachable, protocol.RosterEntryBytesMax+1)

	for _, tc := range []struct {
		name       string
		in         roster.Row
		approx     bool
		row        bool
		check      func(protocol.RosterRow) string
		withRowLen int
	}{
		{name: "within every bound", in: base, row: true},
		{name: "host strings at their bounds", in: func() roster.Row {
			r := base
			r.Version, r.Host.Provider, r.Host.Workspace = strings.Repeat("é", 128), strings.Repeat("p", 64), strings.Repeat("w", 4096)
			return r
		}(), row: true, check: func(r protocol.RosterRow) string {
			if r.Host.CrazeVersion != strings.Repeat("é", 128) || len(r.Host.Workspace) != 4096 {
				return "cut at its bound"
			}
			return ""
		}},
		{name: "host strings over their bounds", in: func() roster.Row {
			r := base
			r.Version, r.Host.Provider, r.Host.Workspace = strings.Repeat("é", 129), strings.Repeat("p", 65), strings.Repeat("w", 4097)
			return r
		}(), approx: true, row: true, check: func(r protocol.RosterRow) string {
			if r.Host.CrazeVersion != strings.Repeat("é", 128) || r.Host.Provider != strings.Repeat("p", 64) || r.Host.Workspace != strings.Repeat("w", 4096) {
				return "not cut to its bound at a character"
			}
			return ""
		}},
		{name: "a host row of 16 KiB", in: func() roster.Row { r := base; r.Raw = rowOfLen(protocol.RosterRowBytesMax); return r }(),
			row: true, withRowLen: protocol.RosterRowBytesMax},
		{name: "a host row over 16 KiB", in: func() roster.Row { r := base; r.Raw = rowOfLen(protocol.RosterRowBytesMax + 1); return r }(),
			approx: true},
		{name: "a roster row of 32,000 bytes", in: entryOf(big, protocol.RosterEntryBytesMax), row: true},
		{name: "a roster row over 32,000 bytes", in: entryOf(big, protocol.RosterEntryBytesMax+1), approx: true},
		{name: "over by approximate's byte, approximate anyway", in: unreachable, approx: true, row: true},
		{name: "every host string at its costliest, and a 16 KiB row", in: func() roster.Row {
			r := base
			r.Version, r.Host.Provider, r.Host.Workspace = ctl[:128], ctl[:64], ctl
			r.Raw = rowOfLen(protocol.RosterRowBytesMax)
			return r
		}(), approx: true},
		{name: "a host row that is not an object", in: func() roster.Row { r := base; r.Raw = json.RawMessage(`null`); return r }(),
			approx: true},
		{name: "a reachable host with no row", in: func() roster.Row { r := base; r.Raw = nil; return r }(), approx: true},
		{name: "connecting", in: func() roster.Row {
			r := base
			r.Status, r.Raw, r.ReadAt = roster.Connecting, nil, time.Time{}
			return r
		}(),
			approx: true},
		{name: "read freshFor ago", in: func() roster.Row { r := base; r.ReadAt = now.Add(-freshFor); return r }(), row: true},
		{name: "read longer ago", in: func() roster.Row { r := base; r.ReadAt = now.Add(-freshFor - 1); return r }(),
			approx: true, row: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := at(tc.in)
			if r.Approximate != tc.approx || (r.Row != nil) != tc.row {
				t.Fatalf("approximate %v, row %v; want %v, %v", r.Approximate, r.Row != nil, tc.approx, tc.row)
			}
			if tc.withRowLen > 0 && len(r.Row) != tc.withRowLen {
				t.Fatalf("the row is %d bytes, want %d", len(r.Row), tc.withRowLen)
			}
			if tc.check != nil {
				if why := tc.check(r); why != "" {
					t.Fatal(why)
				}
			}
			b, err := protocol.MarshalLine(r)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(b) - 1; n > protocol.RosterEntryBytesMax {
				t.Fatalf("the roster row encodes to %d bytes, over %d", n, protocol.RosterEntryBytesMax)
			}
			if strings.Contains(string(b), `"row":null`) {
				t.Fatalf("a null row: %s", clip(b))
			}
			result, _ := json.Marshal(protocol.HubSessionsListResult{Epoch: "e", Sessions: []protocol.RosterRow{r}})
			line, _ := protocol.MarshalLine(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(`1`), Result: result})
			if err := wiretest.Default().Response(protocol.MethodSessionsList, line); err != nil {
				t.Fatalf("off the schema: %v", err)
			}
		})
	}

	for _, tc := range []struct {
		name string
		host roster.Host
		ok   bool
	}{
		{"a host", base.Host, true},
		{"a session id of the schema's dots", func() roster.Host { h := base.Host; h.CrazeSessionID = "."; return h }(), true},
		{"a session id of 128", func() roster.Host { h := base.Host; h.CrazeSessionID = strings.Repeat("s", 128); return h }(), true},
		{"no session id", func() roster.Host { h := base.Host; h.CrazeSessionID = ""; return h }(), false},
		{"a session id of 129", func() roster.Host { h := base.Host; h.CrazeSessionID = strings.Repeat("s", 129); return h }(), false},
		{"a session id not a token", func() roster.Host { h := base.Host; h.CrazeSessionID = "a b"; return h }(), false},
		{"an upper-case host id", func() roster.Host { h := base.Host; h.ID = "00000000000A"; return h }(), false},
		{"a short host id", func() roster.Host { h := base.Host; h.ID = "0001"; return h }(), false},
		{"protocol 0", func() roster.Host { h := base.Host; h.Protocol = 0; return h }(), false},
	} {
		if got := listable(tc.host); got != tc.ok {
			t.Errorf("%s: listable %v, want %v", tc.name, got, tc.ok)
		}
	}
}

// TestARosterOverItsRowLimit (§3.6): with more hosts than RosterRowsMax the
// list and the subscription's reply hold the first RosterRowsMax in host-id
// order, truncated; a host entering the view before them all pushes the last
// out — an upsert and a removal — and one leaving it lets the next in. The
// negative control: at RosterRowsMax hosts the roster is whole, not
// truncated.
func TestARosterOverItsRowLimit(t *testing.T) {
	const extra = 8
	m := newMemHosts(t)
	var all []string
	for n := 100; n < 100+protocol.RosterRowsMax+extra; n++ {
		all = append(all, m.add(n, memRow(n, "h")))
	}
	setVar(t, &listWait, step)
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, gate: true, hk: func(hk *hooks) { installMem(t, hk, m) }})
	view := all[:protocol.RosterRowsMax]
	l := dialPeer(t, rg.sock).list()
	if !l.Truncated || !slices.Equal(ids(l.Sessions), view) {
		t.Fatalf("over the limit: truncated %v, %d rows (first %v)", l.Truncated, len(l.Sessions), ids(l.Sessions)[:3])
	}
	p := dialPeer(t, rg.sock)
	if sub := p.subscribe(); !sub.Truncated || !slices.Equal(ids(sub.Sessions), view) {
		t.Fatalf("the subscription's reply: truncated %v, %d rows", sub.Truncated, len(sub.Sessions))
	}

	first := m.add(1, memRow(1, "first"))
	rg.clk.advance(time.Second)
	rg.tick()
	n := p.roster()
	if !slices.Equal(ids(n.Upserts), []string{first}) || !slices.Equal(n.Removes, []string{view[len(view)-1]}) {
		t.Fatalf("a host entering before them all: %+v", n)
	}
	if n.Upserts[0].Status != protocol.RosterReachable {
		rg.applied("the entered host answered", 0, listedIn(false, first))
		rg.flush.open(t)
		if n = p.roster(); !slices.Equal(ids(n.Upserts), []string{first}) || n.Upserts[0].Status != protocol.RosterReachable || len(n.Removes) != 0 {
			t.Fatalf("the entered host answering: %+v", n)
		}
	}

	m.remove(all[0])
	rg.clk.advance(time.Second)
	from := rg.snaps.mark()
	rg.tick()
	rg.applied("the first host gone", from, func(s roster.Snapshot) bool { return snapRow(s, all[0]) == nil })
	rg.flush.open(t)
	if n := p.roster(); !slices.Equal(n.Removes, []string{all[0]}) || !slices.Equal(ids(n.Upserts), []string{view[len(view)-1]}) {
		t.Fatalf("a host leaving the view: %+v", n)
	}

	// Negative control: RosterRowsMax hosts are a whole roster.
	m.remove(first)
	for _, id := range all[protocol.RosterRowsMax+1:] {
		m.remove(id)
	}
	from = rg.snaps.mark()
	rg.tick()
	rg.applied("the registry at the limit", from, func(s roster.Snapshot) bool { return len(s.Running) == protocol.RosterRowsMax })
	if l := dialPeer(t, rg.sock).list(); l.Truncated || len(l.Sessions) != protocol.RosterRowsMax {
		t.Fatalf("at the limit: truncated %v, %d rows", l.Truncated, len(l.Sessions))
	}
}

// TestNotificationsCarryTheNetChange (§3.6): what changes within one flush's
// spacing reaches the subscriber as one notification of the net change since
// the cursor it last had — a host changed three times once, at its last row;
// a host changed and then gone, a removal; a host gone and back, an upsert
// of its last row; a host that came and went while the subscriber held it
// never, nothing — at the roster's cursor, which jumped. The negative
// control: while the flush is spaced the subscriber is sent nothing, and
// after it nothing more.
func TestNotificationsCarryTheNetChange(t *testing.T) {
	m := newMemHosts(t)
	a, b, c := m.add(1, memRow(1, "1")), m.add(2, memRow(2, "2")), m.add(3, memRow(3, "3"))
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, gate: true, hk: func(hk *hooks) { installMem(t, hk, m) }})
	p := dialPeer(t, rg.sock)
	p.subscribe()

	// A first notification, which goes at once: the next is spaced.
	m.setRow(a, memRow(1, "1a"))
	rg.clk.advance(time.Second)
	rg.tick()
	first := p.roster()
	if !slices.Equal(ids(first.Upserts), []string{a}) {
		t.Fatalf("the first notification: %+v", first)
	}

	// round makes a change, ticks, and waits for the roster to have taken
	// what pred says of the round that read it.
	round := func(what string, change func(), pred func(now time.Time) func(roster.Snapshot) bool) {
		t.Helper()
		change()
		rg.clk.advance(time.Second)
		from := rg.snaps.mark()
		rg.tick()
		rg.applied(what, from, pred(rg.clk.now()))
	}
	both := func(ps ...func(roster.Snapshot) bool) func(roster.Snapshot) bool {
		return func(s roster.Snapshot) bool {
			for _, p := range ps {
				if !p(s) {
					return false
				}
			}
			return true
		}
	}
	d := hostOf(4)
	round("1b, 2b, 4 in", func() {
		m.setRow(a, memRow(1, "1b"))
		m.setRow(b, memRow(2, "2b"))
		m.add(4, memRow(4, "4"))
	}, func(now time.Time) func(roster.Snapshot) bool {
		return both(titled(a, "1b"), titled(b, "2b"), titled(d, "4"), readAt(now, a, b, c, d))
	})
	p.nothing("while the flush is spaced")
	round("1c, 2, 3 and 4 gone", func() {
		m.setRow(a, memRow(1, "1c"))
		m.remove(b)
		m.remove(c)
		m.remove(d)
	}, func(time.Time) func(roster.Snapshot) bool {
		return func(s roster.Snapshot) bool {
			return titled(a, "1c")(s) && snapRow(s, b) == nil && snapRow(s, c) == nil && snapRow(s, d) == nil
		}
	})
	round("3 back as 3c", func() { m.add(3, memRow(3, "3c")) }, func(now time.Time) func(roster.Snapshot) bool {
		return both(titled(c, "3c"), readAt(now, c))
	})
	p.nothing("while the flush is spaced")

	rg.flush.open(t)
	net := p.roster()
	if !slices.Equal(ids(net.Upserts), []string{a, c}) || !slices.Equal(net.Removes, []string{b}) || net.Cursor != rg.cursor() ||
		net.Cursor <= first.Cursor+1 {
		t.Fatalf("the net change: %+v (cursor %d, the roster's %d, the first's %d)", net, net.Cursor, rg.cursor(), first.Cursor)
	}
	if string(net.Upserts[0].Row) != memRow(1, "1c") || string(net.Upserts[1].Row) != memRow(3, "3c") {
		t.Fatalf("the net change's rows: %s, %s", net.Upserts[0].Row, net.Upserts[1].Row)
	}
	p.nothing("after the net change")
}

// subLive says the hub holds a live subscription named id.
func subLive(h *hub, id string) bool {
	h.rs.mu.Lock()
	defer h.rs.mu.Unlock()
	for s := range h.rs.subs {
		if s.id == id {
			return true
		}
	}
	return false
}

// TestASlowSubscriberIsReset (§3.6): a subscriber that stops reading has its
// subscription ended once a notification's write blocks for slowWait: the
// rest of that line and a reset{slow_consumer} follow what it was sent, the
// connection stays, and a new subscription on it is answered. Meanwhile
// another subscriber is sent every round's change and the poll goes on —
// neither stalls behind it. The negative control: the reader, sent the same
// notifications, is never reset.
func TestASlowSubscriberIsReset(t *testing.T) {
	setVar(t, &slowWait, 300*time.Millisecond)
	setVar(t, &flushEvery, time.Millisecond)
	const hosts = 4
	m := newMemHosts(t)
	pad := strings.Repeat("p", 4096)
	for n := 1; n <= hosts; n++ {
		m.add(n, memRow(n, "0"+pad))
	}
	var accepted atomic.Int32
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, hk: func(hk *hooks) {
		installMem(t, hk, m)
		hk.accepted = func(uc *net.UnixConn) {
			if accepted.Add(1) == 1 {
				_ = uc.SetWriteBuffer(1) // the slow subscriber's: a few notifications fill it
			}
		}
	}})
	slow := dialPeer(t, rg.sock)
	if sub := slow.subscribe(); sub.Subscription != "r-1" {
		t.Fatalf("the slow subscriber's subscription: %+v", sub)
	}
	fast := dialPeer(t, rg.sock)
	fast.subscribe()

	deadline := time.Now().Add(step)
	for i := 1; subLive(rg.h, "r-1"); i++ {
		if time.Now().After(deadline) {
			t.Fatalf("the slow subscriber was not reset within %v (%d rounds)", step, i)
		}
		title := strings.Repeat("x", i%7) + pad
		for n := 1; n <= hosts; n++ {
			m.setRow(hostOf(n), memRow(n, title))
		}
		rg.clk.advance(time.Second)
		rg.tick()
		// The reader is sent this round's change: nothing waits on the
		// subscriber that is not reading.
		for seen := map[string]bool{}; len(seen) < hosts; {
			n := fast.roster()
			for _, r := range n.Upserts {
				if strings.Contains(string(r.Row), `"title":"`+title+`"`) {
					seen[r.HostID] = true
				}
			}
		}
	}

	// The slow subscriber reads: notifications, then its reset, then nothing.
	var last msg
	for {
		last = slow.read()
		if last.Method != protocol.NotifyRoster {
			break
		}
	}
	var reset protocol.ResetParams
	if last.Method != protocol.NotifyReset || json.Unmarshal(last.Params, &reset) != nil ||
		reset != (protocol.ResetParams{Subscription: "r-1", Reason: protocol.ResetSlowConsumer}) {
		t.Fatalf("after its notifications the slow subscriber read %s, want reset{r-1, slow_consumer}", clip(last.raw))
	}
	slow.nothing("after the reset")
	if again := slow.subscribe(); again.Subscription == "" {
		t.Fatal("no new subscription after the reset")
	}
	// The negative control.
	fast.nothing("the reader, reset") // nothing at all is owed it now: no reset among what it read
}

// TestTheReplyIsWrittenBeforeAnyNotification (§3.6): a change the roster
// takes between a subscription's registration and its reply's write is
// pending at once — its flusher woken — stays pending while the reply is
// unwritten, and is written after the reply. The negative control: the reply
// does not hold the change (it was built before it), so the notification had
// something to say before the reply was written, and waited.
func TestTheReplyIsWrittenBeforeAnyNotification(t *testing.T) {
	m := newMemHosts(t)
	a := m.add(1, memRow(1, "before"))
	var rg *rosterRig
	var during atomic.Bool
	rg = newRosterRig(t, testEnv(t), rigOpts{clock: true, hk: func(hk *hooks) {
		installMem(t, hk, m)
		hk.subscribed = func() {
			if !during.Load() {
				return
			}
			m.setRow(a, memRow(1, "during"))
			rg.clk.advance(time.Second)
			from := rg.snaps.mark()
			rg.tick()
			rg.applied("the change taken", from, titled(a, "during"))
			// The change is pending and its flusher woken; for a while yet
			// the reply is unwritten, and the flusher must leave it so — a
			// flusher that did not wait for the reply would take it now, and
			// write it first.
			for end := time.Now().Add(200 * time.Millisecond); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
				rg.h.rs.mu.Lock()
				pending := 0
				for s := range rg.h.rs.subs {
					pending += len(s.pending)
				}
				rg.h.rs.mu.Unlock()
				if pending != 1 {
					t.Errorf("%d host ids pending before the reply was written, want the change's 1", pending)
					return
				}
			}
		}
	}})
	during.Store(true)
	p := dialPeer(t, rg.sock)
	id := p.send(protocol.MethodSessionsSubscribe, nil)
	first := p.read()
	if string(first.ID) != id || first.Error != nil {
		t.Fatalf("the first line is not the reply: %s", clip(first.raw))
	}
	var sub protocol.SessionsSubscribeResult
	if err := json.Unmarshal(first.Result, &sub); err != nil {
		t.Fatal(err)
	}
	if r := rowOf(sub.Sessions, a); r == nil || string(r.Row) != memRow(1, "before") {
		t.Fatalf("the reply holds %+v, want the row as it was before the change", r)
	}
	n := p.roster()
	if len(n.Upserts) != 1 || string(n.Upserts[0].Row) != memRow(1, "during") || n.Cursor <= sub.Cursor {
		t.Fatalf("the notification after the reply: %+v", n)
	}
}

// TestOneSubscriptionPerConnection (§3.6): a second sessions.subscribe on a
// connection that holds one is refused bad_request, reason
// already_subscribed — the negative control: another connection's is
// answered — and params must be {}: a member is unknown_field, null is
// bad_request, for both roster methods.
func TestOneSubscriptionPerConnection(t *testing.T) {
	m := newMemHosts(t)
	m.add(1, memRow(1, "one"))
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, hk: func(hk *hooks) { installMem(t, hk, m) }})
	p := dialPeer(t, rg.sock)
	p.subscribe()
	if r := p.call(protocol.MethodSessionsSubscribe, nil); r.Error == nil || r.Error.Data.Code != protocol.CodeBadRequest ||
		r.Error.Data.Reason != protocol.ReasonAlreadySubscribed {
		t.Fatalf("a second subscription: %s", clip(r.raw))
	}
	if sub := dialPeer(t, rg.sock).subscribe(); sub.Subscription != "r-2" {
		t.Fatalf("another connection's subscription: %+v", sub)
	}
	q := dialPeer(t, rg.sock)
	for _, method := range []string{protocol.MethodSessionsSubscribe, protocol.MethodSessionsList} {
		if r := q.call(method, map[string]any{"cursor": 1}); r.Error == nil || r.Error.Data.Reason != protocol.ReasonUnknownField ||
			!strings.Contains(r.Error.Message, "params.cursor") {
			t.Fatalf("%s with a member: %s", method, clip(r.raw))
		}
		id := q.send(method, nil)
		_ = id
		q.read()
		raw := `{"jsonrpc":"2.0","id":"null-params","method":"` + method + `","params":null}` + "\n"
		q.methods["\"null-params\""] = method
		if _, err := q.nc.Write([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		if r := q.read(); r.Error == nil || r.Error.Data.Reason != protocol.ReasonBadRequest || r.Error.Code != protocol.RPCInvalidParams {
			t.Fatalf("%s with null params: %s", method, clip(r.raw))
		}
	}
}

// TestTeardownEndsEverySubscription (§3.5): a hub that stops ends each
// subscription with reset{hub_closing} and closes the connection; a
// connection with none is closed with no reset — the negative control — and
// a subscriber that is not reading, a notification's write stuck in flight,
// holds the teardown up only within resetWait: the hub's files are gone and
// its lock free soon after.
func TestTeardownEndsEverySubscription(t *testing.T) {
	setVar(t, &resetWait, 500*time.Millisecond)
	m := newMemHosts(t)
	pad := strings.Repeat("p", 8192)
	m.add(1, memRow(1, "0"+pad))
	env := testEnv(t)
	var accepted atomic.Int32
	rg := newRosterRig(t, env, rigOpts{clock: true, hk: func(hk *hooks) {
		installMem(t, hk, m)
		hk.accepted = func(uc *net.UnixConn) {
			if accepted.Add(1) == 3 {
				_ = uc.SetWriteBuffer(1) // the stuck subscriber's
			}
		}
	}})
	p := dialPeer(t, rg.sock)
	sub := p.subscribe()
	q := dialPeer(t, rg.sock)
	stuck := dialPeer(t, rg.sock)
	stuck.subscribe()
	var stuckConn *conn
	rg.h.rs.mu.Lock()
	for s := range rg.h.rs.subs {
		if s.id == "r-2" {
			stuckConn = s.c
		}
	}
	rg.h.rs.mu.Unlock()
	if stuckConn == nil {
		t.Fatal("the stuck subscriber's connection is not found")
	}
	for i := 1; !stuckConn.writing.Load(); i++ {
		if i > 1000 {
			t.Fatal("the stuck subscriber's notifications never blocked")
		}
		m.setRow(hostOf(1), memRow(1, strings.Repeat("x", i%5)+pad))
		rg.clk.advance(time.Second)
		from := rg.snaps.mark()
		rg.tick()
		rg.applied("the change", from, titled(hostOf(1), strings.Repeat("x", i%5)+pad))
		for {
			n := p.roster()
			if strings.Contains(string(n.Upserts[0].Row), strings.Repeat("x", i%5)+pad) {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	rg.rn.sigs <- syscall.SIGTERM
	var last msg
	for {
		last = p.read()
		if last.Method != protocol.NotifyRoster {
			break
		}
	}
	var reset protocol.ResetParams
	if last.Method != protocol.NotifyReset || json.Unmarshal(last.Params, &reset) != nil ||
		reset != (protocol.ResetParams{Subscription: sub.Subscription, Reason: protocol.ResetHubClosing}) {
		t.Fatalf("the subscriber's last line: %s, want reset{%s, hub_closing}", clip(last.raw), sub.Subscription)
	}
	if !p.eof() {
		t.Fatal("the subscriber's connection was not closed after its reset")
	}
	if !q.eof() {
		t.Fatal("a connection with no subscription was not closed — or was written to")
	}
	if err := rg.rn.stopped(t); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > resetWait+teardownBound {
		t.Fatalf("the teardown took %v with a stuck subscriber", took)
	}
	absent(t, "the record", recordPath(t, env))
	absent(t, "the socket", rg.sock)
	lockFree(t, env)
}

// TestACrashedHostLeavesWithinOneRound (A9): ten real hosts behind a hub
// with a subscriber; one crashes — SIGKILL, its lock freed, its entry left —
// and the very next round's registry read sweeps it, its removal the next
// notification. Without that round the row stays: a list then still holds it,
// the negative control. With the production tick (1 s) and flush spacing
// (250 ms) that is within A9's 2 s.
func TestACrashedHostLeavesWithinOneRound(t *testing.T) {
	if roster.TickEvery != time.Second || flushEvery != 250*time.Millisecond {
		t.Fatalf("the poll ticks every %v and flushes every %v: A9's bound is no longer the tick and a flush", roster.TickEvery, flushEvery)
	}
	env := testEnv(t)
	rg := newRosterRig(t, env, rigOpts{clock: true})
	var want []string
	for n := 1; n <= 9; n++ {
		hostIn(t, env, n)
		want = append(want, hostOf(n))
	}
	crashed := hostOf(10)
	hc := startHostChild(t, env, crashed, sessionOf(10))
	want = append(want, crashed)
	setVar(t, &listWait, step)
	p := dialPeer(t, rg.sock)
	sub := p.subscribe()
	if !slices.Equal(ids(sub.Sessions), want) {
		t.Fatalf("subscribed to %v, want %v", ids(sub.Sessions), want)
	}
	for _, r := range sub.Sessions {
		if r.Status != protocol.RosterReachable || r.Approximate || r.Row == nil {
			t.Fatalf("host %s: %+v", r.HostID, r)
		}
	}

	hc.crash(t)
	if l := dialPeer(t, rg.sock).list(); rowOf(l.Sessions, crashed) == nil {
		t.Fatal("the crashed host left without a round: the control does not hold")
	}
	start := time.Now()
	rg.tick()
	n := p.roster()
	if len(n.Upserts) != 0 || !slices.Equal(n.Removes, []string{crashed}) {
		t.Fatalf("the round after the crash: %+v, want the crashed host's removal", n)
	}
	t.Logf("the crashed host's removal was written %v after its round's tick", time.Since(start))
	if entries, err := rundir.Hosts(env); err != nil || len(entries) != 9 {
		t.Fatalf("the registry after the round: %d hosts, %v", len(entries), err)
	}
}

// TestThePollRunsOnlyOnDemand (P1, §3.5): the hub's poll reads the registry
// and asks its hosts only while a subscription or an answer wants it — a
// list's round, a subscription's ticks — and not otherwise: ticks before the
// first list, between lists and after the subscription's end read nothing.
// Each run asks over the connections the last one kept, and the poll's reads
// feed the idle rule. The negative controls are the counts, and the idle
// rule's view before the poll first read the registry.
func TestThePollRunsOnlyOnDemand(t *testing.T) {
	m := newMemHosts(t)
	looped := make(chan struct{})
	// paused is told each time the poll has taken a Pause: the list's demand
	// is released before its answer is written, but the poll applies the
	// Pause on its own goroutine, and a tick it takes first still polls.
	paused := make(chan struct{}, 8)
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, hk: func(hk *hooks) {
		installMem(t, hk, m)
		hk.looping = func() { close(looped) }
		hk.rosterPaused = func() {
			select {
			case paused <- struct{}{}:
			default:
			}
		}
	}})
	<-looped
	a := m.add(1, memRow(1, "one"))
	reads := m.readCount()
	live := func() bool {
		rg.h.life.mu.Lock()
		defer rg.h.life.mu.Unlock()
		return rg.h.life.hostsLive
	}

	rg.tick() // no demand: nothing
	if live() {
		t.Fatal("the idle rule saw a host with no read since it was listed")
	}
	q := dialPeer(t, rg.sock)
	drain := func() { // only a Pause after this point counts for the wait below it
		for len(paused) > 0 {
			<-paused
		}
	}
	drain()
	if l := q.list(); !slices.Equal(ids(l.Sessions), []string{a}) {
		t.Fatalf("listed %v", ids(l.Sessions))
	}
	if got := m.readCount(); got != reads+1 {
		t.Fatalf("%d registry reads after a list, want %d: a tick with no demand read it", got, reads+1)
	}
	if !live() {
		t.Fatal("the poll's read did not reach the idle rule")
	}

	select {
	case <-paused: // the list's demand is released and the poll has paused
	case <-time.After(step):
		t.Fatalf("the poll did not pause after the list's answer within %v", step)
	}
	rg.tick() // the list has answered: no demand again
	p := dialPeer(t, rg.sock)
	p.subscribe()
	from := rg.snaps.mark()
	rg.clk.advance(time.Second)
	rg.tick() // a subscription's tick polls
	rg.applied("the subscription's tick", from, readAt(rg.clk.now(), a))
	if got := m.readCount(); got != reads+3 {
		t.Fatalf("%d registry reads after a list, a subscription and its tick, want %d", got, reads+3)
	}

	drain()
	_ = p.nc.Close()
	waitFor(t, "the subscription's end", func() bool {
		rg.h.rs.mu.Lock()
		defer rg.h.rs.mu.Unlock()
		return rg.h.rs.demand == 0
	})
	select {
	case <-paused: // the end's release has reached the poll
	case <-time.After(step):
		t.Fatalf("the poll did not pause after the subscription's end within %v", step)
	}
	rg.tick() // ended: nothing
	q.list()
	if got := m.readCount(); got != reads+4 {
		t.Fatalf("%d registry reads after the subscription ended and a list, want %d", got, reads+4)
	}
	if n := m.dialCount(); n != 1 {
		t.Fatalf("%d dials over four runs of one host: the runs did not keep its connection", n)
	}
}

// TestAListWaitsForItsRound (§3.6): a list on a cold hub waits for the
// round its demand opened — its hosts reachable, read — but no longer than
// listWait: a host whose answer has not come by then is listed connecting.
// The negative control is the host that answered: a list that did not wait
// would hold no row at all on a cold hub.
func TestAListWaitsForItsRound(t *testing.T) {
	setVar(t, &listWait, 300*time.Millisecond)
	m := newMemHosts(t)
	a := m.add(1, memRow(1, "one"))
	b := m.add(2, memRow(2, "two"))
	release := m.holdAnswers(b)
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, hk: func(hk *hooks) { installMem(t, hk, m) }})
	q := dialPeer(t, rg.sock)
	start := time.Now()
	l := q.list()
	if took := time.Since(start); took < listWait {
		t.Fatalf("the list answered after %v, before its bound %v, with a host unanswered", took, listWait)
	}
	checkRow(t, rowOf(l.Sessions, a), 1, memRow(1, "one"), protocol.RosterReachable, false)
	if r := rowOf(l.Sessions, b); r == nil || r.Status != protocol.RosterConnecting || !r.Approximate || r.Row != nil {
		t.Fatalf("the unanswered host: %+v", r)
	}
	release()
	rg.applied("the held host answered", 0, listedIn(false, b))
	if l := q.list(); rowOf(l.Sessions, b).Status != protocol.RosterReachable {
		t.Fatalf("after its answer: %+v", rowOf(l.Sessions, b))
	}
}

// cursorOf is the roster cursor a line carries — a roster notification's, or
// the sessions.list reply's to listID — and whether it carries one.
func cursorOf(t *testing.T, m msg, listID string) (uint64, bool) {
	t.Helper()
	switch {
	case m.Method == protocol.NotifyRoster:
		var n protocol.RosterParams
		if err := json.Unmarshal(m.Params, &n); err != nil {
			t.Fatal(err)
		}
		return n.Cursor, true
	case m.Method == "" && string(m.ID) == listID && m.Error == nil:
		var l protocol.HubSessionsListResult
		if err := json.Unmarshal(m.Result, &l); err != nil {
			t.Fatal(err)
		}
		return l.Cursor, true
	}
	return 0, false
}

// readThrough reads the connection's lines, after those read already, until
// the reply to listID: every line in the order written.
func readThrough(p *peer, read []msg, listID string) []msg {
	p.t.Helper()
	for _, m := range read {
		if string(m.ID) == listID {
			return read
		}
	}
	for {
		m := p.read()
		read = append(read, m)
		if string(m.ID) == listID {
			return read
		}
	}
}

// inOrder holds lines, in the order one connection was written them, to the
// roster's order: no line carries a cursor below one written before it. It
// answers the list reply and the notifications written before it.
func inOrder(t *testing.T, lines []msg, listID string) (protocol.HubSessionsListResult, []protocol.RosterParams) {
	t.Helper()
	var last uint64
	var lastRaw []byte
	var list *protocol.HubSessionsListResult
	var before []protocol.RosterParams
	for _, m := range lines {
		cur, ok := cursorOf(t, m, listID)
		if !ok {
			t.Fatalf("an unexpected line: %s", clip(m.raw))
		}
		if cur < last {
			t.Fatalf("a line at cursor %d was written after one at cursor %d:\n  %s\nthen\n  %s", cur, last, clip(lastRaw), clip(m.raw))
		}
		last, lastRaw = cur, m.raw
		switch {
		case list != nil:
		case m.Method == protocol.NotifyRoster:
			var n protocol.RosterParams
			_ = json.Unmarshal(m.Params, &n)
			before = append(before, n)
		default:
			list = &protocol.HubSessionsListResult{}
			_ = json.Unmarshal(m.Result, list)
		}
	}
	if list == nil {
		t.Fatal("no list reply")
	}
	return *list, before
}

// TestAListIsWrittenInItsSnapshotsOrder (r19 1): on a connection that holds a
// subscription, a change the roster takes — and its subscription's flush —
// between a sessions.list's snapshot and that reply's write is written after
// the reply, never before it: the reply's cursor is never below a
// notification the connection was written first, and its rows hold every
// change such a notification carried. The flusher, woken for the change,
// waits at the connection's write lock, which the list holds from its
// snapshot through its write. The negative control: the reply holds the row
// as it was before the change, at a lower cursor — had the notification gone
// first, the client would have been taken back.
func TestAListIsWrittenInItsSnapshotsOrder(t *testing.T) {
	m := newMemHosts(t)
	a := m.add(1, memRow(1, "one"))
	var armed atomic.Bool
	snapped, resume, locking := make(chan *conn, 1), make(chan struct{}), make(chan struct{}, 8)
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, gate: true, hk: func(hk *hooks) {
		installMem(t, hk, m)
		hk.rosterTaken = func(c *conn, what string) {
			if what == "list" && armed.Load() {
				snapped <- c
				<-resume
			}
		}
		hk.rosterLocking = func(_ *conn, what string) {
			if what == "flush" && armed.Load() {
				locking <- struct{}{}
			}
		}
	}})
	p := dialPeer(t, rg.sock)
	sub := p.subscribe()
	armed.Store(true)
	listID := p.send(protocol.MethodSessionsList, nil)
	var c *conn
	select {
	case c = <-snapped:
	case <-time.After(step):
		t.Fatal("the list took no snapshot")
	}

	// Between the list's snapshot and its write: a change, and its flush.
	m.setRow(a, memRow(1, "changed"))
	rg.clk.advance(time.Second)
	from := rg.snaps.mark()
	rg.tick()
	rg.applied("the change taken", from, titled(a, "changed"))
	select {
	case <-locking:
	case <-time.After(step):
		close(resume)
		t.Fatal("the flusher was not woken for the change")
	}
	var lines []msg
	if c.wmu.TryLock() {
		// The list does not hold the write lock: nothing keeps the
		// notification from going first, and it does.
		c.wmu.Unlock()
		lines = append(lines, p.read())
	}
	close(resume)

	lines = readThrough(p, lines, listID)
	l, before := inOrder(t, lines, listID)
	for _, n := range before {
		for _, u := range n.Upserts {
			if r := rowOf(l.Sessions, u.HostID); r == nil || string(r.Row) != string(u.Row) {
				t.Fatalf("the list misses a change written to the connection before it: %s's row %s", u.HostID, u.Row)
			}
		}
	}
	if len(before) == 0 {
		// The control: the reply is the roster as it was snapshotted.
		if r := rowOf(l.Sessions, a); r == nil || string(r.Row) != memRow(1, "one") || l.Cursor != sub.Cursor {
			t.Fatalf("the reply: cursor %d (subscribed at %d), row %+v; want the row before the change", l.Cursor, sub.Cursor, r)
		}
		n := p.roster()
		if n.Cursor <= l.Cursor || len(n.Upserts) != 1 || string(n.Upserts[0].Row) != memRow(1, "changed") {
			t.Fatalf("the change after the reply: %+v, want it at a cursor past %d", n, l.Cursor)
		}
	}
}

// TestANotificationIsWrittenInItsTakesOrder (r19 1): the flusher's take and
// its write are one section under the connection's write lock too, so a
// sessions.list on that connection — after a change the take did not see —
// is written after the notification, never before it with a later cursor.
// The negative control: the list's reply holds the change the notification
// does not, at a higher cursor — written first, the client would have taken
// the notification's older row over it.
func TestANotificationIsWrittenInItsTakesOrder(t *testing.T) {
	m := newMemHosts(t)
	a := m.add(1, memRow(1, "one"))
	var armed atomic.Bool
	took, resume, locking := make(chan *conn, 1), make(chan struct{}), make(chan struct{}, 8)
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, gate: true, hk: func(hk *hooks) {
		installMem(t, hk, m)
		hk.rosterTaken = func(c *conn, what string) {
			if what == "flush" && armed.Load() {
				took <- c
				<-resume
			}
		}
		hk.rosterLocking = func(_ *conn, what string) {
			if what == "list" && armed.Load() {
				locking <- struct{}{}
			}
		}
	}})
	p := dialPeer(t, rg.sock)
	p.subscribe()
	armed.Store(true)

	// The first change is taken by the flusher, held before its write.
	m.setRow(a, memRow(1, "first"))
	rg.clk.advance(time.Second)
	from := rg.snaps.mark()
	rg.tick()
	rg.applied("the first change taken", from, titled(a, "first"))
	var c *conn
	select {
	case c = <-took:
	case <-time.After(step):
		t.Fatal("the flusher took no notification")
	}
	// A second change, and a list, while the notification is unwritten.
	m.setRow(a, memRow(1, "second"))
	rg.clk.advance(time.Second)
	from = rg.snaps.mark()
	rg.tick()
	rg.applied("the second change taken", from, titled(a, "second"))
	listID := p.send(protocol.MethodSessionsList, nil)
	select {
	case <-locking:
	case <-time.After(step):
		close(resume)
		t.Fatal("the list never came to the write lock")
	}
	var lines []msg
	if c.wmu.TryLock() {
		// The flusher does not hold the write lock: nothing keeps the list
		// from going first, and it does.
		c.wmu.Unlock()
		lines = append(lines, p.read())
	}
	close(resume)
	if len(lines) > 0 {
		lines = append(lines, p.read()) // the held notification
	}

	l, before := inOrder(t, readThrough(p, lines, listID), listID)
	if len(before) != 1 || len(before[0].Upserts) != 1 || string(before[0].Upserts[0].Row) != memRow(1, "first") {
		t.Fatalf("written before the list: %+v, want the first change's notification", before)
	}
	if r := rowOf(l.Sessions, a); r == nil || string(r.Row) != memRow(1, "second") || l.Cursor <= before[0].Cursor {
		t.Fatalf("the list: cursor %d, row %+v; want the second change past cursor %d", l.Cursor, r, before[0].Cursor)
	}
}

// TestARosterCrossingItsRowLimitResetsItsSubscriptions (r19 3): truncated is
// a reply's alone — a roster notification has no such member — so a host
// that takes a whole roster of RosterRowsMax rows over the limit ends every
// subscription with reset{omitted}, though its row is not in the view; the
// subscriber subscribes again and is told truncated. Back to RosterRowsMax
// rows, another reset, and the new reply says whole. The negative control: a
// host added to a roster truncated already changes nothing a subscriber
// holds, and it is told nothing.
func TestARosterCrossingItsRowLimitResetsItsSubscriptions(t *testing.T) {
	m := newMemHosts(t)
	var all []string
	for n := 100; n < 100+protocol.RosterRowsMax; n++ {
		all = append(all, m.add(n, memRow(n, "h")))
	}
	setVar(t, &listWait, step)
	rg := newRosterRig(t, testEnv(t), rigOpts{clock: true, gate: true, hk: func(hk *hooks) { installMem(t, hk, m) }})
	p := dialPeer(t, rg.sock)
	if sub := p.subscribe(); sub.Truncated || !slices.Equal(ids(sub.Sessions), all) {
		t.Fatalf("a whole roster: truncated %v, %d rows", sub.Truncated, len(sub.Sessions))
	}
	// reset reads the subscriber's lines up to its reset, which must be
	// reset{sub, omitted}.
	reset := func(what, sub string) {
		t.Helper()
		var last msg
		for last = p.read(); last.Method == protocol.NotifyRoster; last = p.read() {
		}
		var rp protocol.ResetParams
		if last.Method != protocol.NotifyReset || json.Unmarshal(last.Params, &rp) != nil ||
			rp != (protocol.ResetParams{Subscription: sub, Reason: protocol.ResetOmitted}) {
			t.Fatalf("%s: the subscriber read %s, want reset{%s, omitted}", what, clip(last.raw), sub)
		}
	}
	round := func(what string, pred func(roster.Snapshot) bool) {
		t.Helper()
		rg.clk.advance(time.Second)
		from := rg.snaps.mark()
		rg.tick()
		rg.applied(what, from, pred)
	}
	count := func(n int) func(roster.Snapshot) bool {
		return func(s roster.Snapshot) bool { return len(s.Running) == n }
	}

	over := m.add(100+protocol.RosterRowsMax, memRow(100+protocol.RosterRowsMax, "h"))
	round("one host over the limit", count(protocol.RosterRowsMax+1))
	reset("over the limit", "r-1")
	again := p.subscribe()
	if again.Subscription != "r-2" || !again.Truncated || !slices.Equal(ids(again.Sessions), all) {
		t.Fatalf("subscribed again: %s, truncated %v, %d rows", again.Subscription, again.Truncated, len(again.Sessions))
	}

	// The negative control: truncated already, one more host is no news.
	further := m.add(101+protocol.RosterRowsMax, memRow(101+protocol.RosterRowsMax, "h"))
	round("two hosts over the limit", count(protocol.RosterRowsMax+2))
	p.nothing("a host added to a truncated roster")

	m.remove(over)
	m.remove(further)
	round("back at the limit", count(protocol.RosterRowsMax))
	reset("back at the limit", "r-2")
	if whole := p.subscribe(); whole.Subscription != "r-3" || whole.Truncated || !slices.Equal(ids(whole.Sessions), all) {
		t.Fatalf("subscribed again at the limit: %s, truncated %v, %d rows", whole.Subscription, whole.Truncated, len(whole.Sessions))
	}
}

// TestAResetThatCannotBeWrittenClosesTheConnection: a subscription's reset
// that its subscriber does not read within writeWait — reset{omitted} ends a
// subscription on a connection that otherwise stays — closes the connection,
// so the subscriber is never left holding a subscription that is over, nor a
// line cut part way. The negative control is the subscriber's read before
// the reset: the connection open, the bytes written so far all there.
func TestAResetThatCannotBeWrittenClosesTheConnection(t *testing.T) {
	setVar(t, &writeWait, 200*time.Millisecond)
	dir, err := os.MkdirTemp("/tmp", "czhr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	peer, err := net.DialTimeout("unix", ln.Addr().String(), step)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	uc, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	c := &conn{uc: uc}
	defer c.close()

	// The subscriber reads nothing: the socket's buffers fill.
	_ = uc.SetWriteBuffer(1)
	filled, chunk := 0, make([]byte, 4096)
	for {
		_ = uc.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
		n, err := uc.Write(chunk)
		filled += n
		if err != nil {
			break
		}
	}
	c.writeReset("r-1", protocol.ResetOmitted)

	// Negative control: the connection was open — everything written before
	// the reset is there to read.
	got := 0
	buf := make([]byte, 64<<10)
	for {
		_ = peer.SetReadDeadline(time.Now().Add(step))
		n, err := peer.Read(buf)
		got += n
		if err != nil {
			if isTimeout(err) {
				t.Fatalf("the connection was left open after its reset could not be written (%d of %d bytes read)", got, filled)
			}
			break
		}
	}
	if got < filled {
		t.Fatalf("read %d bytes, %d were written before the reset", got, filled)
	}
}
