package protocol_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
)

// withMember is doc, a JSON object, with one more member.
func withMember(t *testing.T, doc, member, value string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	m[member] = json.RawMessage(value)
	return jsonOf(t, m)
}

// withNestedMember is doc with one more member in its object member obj.
func withNestedMember(t *testing.T, doc, obj, member, value string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	m[obj] = json.RawMessage(withMember(t, string(m[obj]), member, value))
	return jsonOf(t, m)
}

// generic is b decoded with no Go type of its own: what a JSON value is,
// whatever its member order and spacing.
func generic(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return v
}

// TestARosterRowKeepsTheHostsRowWhole (plan 032 §3.6, P4): the hub's roster
// row carries a host's own sessions.list row as the JSON value the host sent,
// so a newer host's members this build does not know pass through a decode
// and a re-encode untouched — the hub's own trip, a host's row in, the roster
// out — and a client reads the row tolerantly, those members ignored; the
// hub's list, its subscription's reply and its roster notification carrying
// that row are all on their schemas. A row not yet read is absent, and says
// so. The negative controls: the host's own sessionRow schema refuses the
// row, the row does carry a member this build refuses when it decodes
// strictly, and a roster row that held the host's row as a SessionRow would
// drop it on the way through.
func TestARosterRowKeepsTheHostsRowWhole(t *testing.T) {
	info := sessionInfo()
	info.Capabilities.RowFacts = true
	host := protocol.SessionRow{SessionInfo: info, Title: "fix it", Activity: protocol.ActivityWorking, PendingAsks: 1,
		HeadAsk: &protocol.HeadAsk{ID: "perm-1", Kind: "permission", Label: "permission Shell", Summary: "Run tests"},
		Doing:   "Run tests", Prompted: true}
	// A newer host's row: members this build has never heard of, at the top
	// (one of them an object of its own) and nested inside objects this build
	// does know.
	newer := withMember(t, withMember(t, jsonOf(t, host), "attached", "2"), "preview", `{"lines":["a","b"],"cut":false}`)
	newer = withNestedMember(t, withNestedMember(t, newer, "headAsk", "urgency", `"high"`), "capabilities", "presence", "true")
	in := `{"hostId":"0190ab12cd34","sessionId":"` + info.SessionID + `","host":{"pid":4242,"crazeVersion":"0.9.0","protocol":1,` +
		`"provider":"grok","workspace":"/work","startedAt":"2026-09-25T10:30:45Z","ready":true},` +
		`"status":"reachable","approximate":false,"row":` + newer + `}`

	var rr protocol.RosterRow
	if err := json.Unmarshal([]byte(in), &rr); err != nil {
		t.Fatal(err)
	}
	out, err := protocol.MarshalLine(rr)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Row json.RawMessage `json:"row"`
	}
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(generic(t, back.Row), generic(t, []byte(newer))) {
		t.Fatalf("the row came through the roster as\n%s\nwant the host's own\n%s", back.Row, newer)
	}
	if !reflect.DeepEqual(generic(t, out), generic(t, []byte(in))) {
		t.Fatalf("the roster row came back as\n%s\nwant\n%s", out, in)
	}

	got, ok, err := rr.SessionRow()
	if err != nil || !ok {
		t.Fatalf("SessionRow = _, %v, %v; want the row, read tolerantly", ok, err)
	}
	if !reflect.DeepEqual(got, host) {
		t.Fatalf("SessionRow =\n%+v\nwant the host's own row\n%+v", got, host)
	}
	if _, ok, err := (protocol.RosterRow{HostID: "0190ab12cd35", Status: protocol.RosterConnecting}).SessionRow(); ok || err != nil {
		t.Fatalf("a row not yet read: SessionRow = _, %v, %v; want absent", ok, err)
	}
	if _, ok, err := (protocol.RosterRow{Row: json.RawMessage(`["not","a","row"]`)}).SessionRow(); !ok || err == nil {
		t.Fatalf("a row that is no row: SessionRow = _, %v, %v; want present and an error", ok, err)
	}

	// What the hub then writes is on its schema, wherever it puts the row:
	// its sessions.list result, its subscription's reply and a roster
	// notification — the row any object (forwardedRow), unknown members and
	// all.
	c := newCompiler()
	for _, tc := range []struct{ file, def, doc string }{
		{"sessions.list.json", "result", jsonOf(t, protocol.HubSessionsListResult{Epoch: "0a1b2c3d4e5f", Cursor: 3, Sessions: []protocol.RosterRow{rr}})},
		{"sessions.subscribe.json", "result",
			jsonOf(t, protocol.SessionsSubscribeResult{Subscription: "r-1", Epoch: "0a1b2c3d4e5f", Cursor: 3, Sessions: []protocol.RosterRow{rr}})},
		{"notification.roster.json", "params",
			jsonOf(t, protocol.RosterParams{Subscription: "r-1", Epoch: "0a1b2c3d4e5f", Cursor: 4, Upserts: []protocol.RosterRow{rr}, Removes: []string{}})},
	} {
		if r := compiled(t, c, protocol.SchemaURI(tc.file, "#/$defs/"+tc.def)).Validate([]byte(tc.doc)); !r.IsValid() {
			t.Errorf("%s#%s refuses the hub's own output carrying a newer host's row: %v\n%s", tc.file, tc.def, r.Errors, tc.doc)
		}
	}
	// Negative control 0: the host's own row schema stays strict — the very
	// row the hub carries is not this build's sessionRow, so the hub's schemas
	// above took it only because they describe it as the host's, whatever its
	// build.
	if compiled(t, c, protocol.SchemaURI(protocol.SchemaInfo, "#/$defs/sessionRow")).Validate([]byte(newer)).IsValid() {
		t.Fatal("this build's sessionRow took the newer host's row: the test's row carries nothing this build refuses")
	}
	if !compiled(t, c, protocol.SchemaURI(protocol.SchemaInfo, "#/$defs/sessionRow")).Validate([]byte(jsonOf(t, host))).IsValid() {
		t.Fatal("this build's sessionRow refuses this build's own row")
	}

	// Negative control 1: the members really are ones this build does not
	// know — a strict decode refuses the row.
	dec := json.NewDecoder(bytes.NewReader([]byte(newer)))
	dec.DisallowUnknownFields()
	var strict protocol.SessionRow
	if err := dec.Decode(&strict); err == nil {
		t.Fatal("a strict decode took the newer host's row: the test's row carries nothing unknown")
	}
	// Negative control 2: a roster row holding the host's row as this build's
	// SessionRow loses them on the way through.
	var typed struct {
		protocol.RosterRow
		Row *protocol.SessionRow `json:"row,omitempty"`
	}
	if err := json.Unmarshal([]byte(in), &typed); err != nil {
		t.Fatal(err)
	}
	lossy, err := protocol.MarshalLine(typed)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(lossy, []byte(`"preview"`)) || bytes.Contains(lossy, []byte(`"attached"`)) {
		t.Fatalf("a typed row kept the unknown members (%s): the control cannot tell the preserved row from a re-encoded one", lossy)
	}
}

// TestTheHubsCursorsFitAUint64 (plan 032 §3.6): the cursor of the hub's list,
// of its subscription's reply and of a roster notification is a Go uint64, so
// each schema takes 2^64−1 and refuses 2^64, which no Go decode could hold.
// The negative control is the one cursor that does not say so, a host's list's
// (its schema predates the hub, and is left as it is): it takes 2^64.
func TestTheHubsCursorsFitAUint64(t *testing.T) {
	const most, over = "18446744073709551615", "18446744073709551616"
	var u uint64
	if err := json.Unmarshal([]byte(most), &u); err != nil || u != math.MaxUint64 {
		t.Fatalf("2^64-1 decodes to %d, %v", u, err)
	}
	if err := json.Unmarshal([]byte(over), &u); err == nil {
		t.Fatal("2^64 decoded into a uint64")
	}
	c := newCompiler()
	for _, tc := range []struct{ name, file, def, doc string }{
		{"the hub's list", "sessions.list.json", "result", `{"epoch":"e","cursor":%s,"sessions":[]}`},
		{"the subscription's reply", "sessions.subscribe.json", "result", `{"subscription":"r-1","epoch":"e","cursor":%s,"sessions":[]}`},
		{"a roster notification", "notification.roster.json", "params", `{"subscription":"r-1","epoch":"e","cursor":%s,"upserts":[],"removes":[]}`},
	} {
		s := compiled(t, c, protocol.SchemaURI(tc.file, "#/$defs/"+tc.def))
		if r := s.Validate([]byte(fmt.Sprintf(tc.doc, most))); !r.IsValid() {
			t.Errorf("%s: cursor 2^64-1 refused: %v", tc.name, r.Errors)
		}
		// An empty list's result is a host's as well as the hub's (the
		// anyOf), so the hub's list is held at its own branch here.
		if tc.file == "sessions.list.json" {
			s = compiled(t, c, protocol.SchemaURI(tc.file, "#/$defs/hubResult"))
		}
		if s.Validate([]byte(fmt.Sprintf(tc.doc, over))).IsValid() {
			t.Errorf("%s: cursor 2^64 taken", tc.name)
		}
	}
	host := compiled(t, c, protocol.SchemaURI("sessions.list.json", "#/$defs/hostResult"))
	if !host.Validate([]byte(`{"epoch":"h","cursor":` + over + `,"sessions":[]}`)).IsValid() {
		t.Fatal("a host's list refuses cursor 2^64: the control no longer shows the bound is the hub schemas' own")
	}
}

// worstRosterRow is roster row i at every bound at once, in the encoding that
// costs most: its host strings at their lengths in characters, each a control
// character JSON writes as six bytes; the widest pid, protocol and time; the
// longest status; a 128-character session id. It carries no row.
func worstRosterRow(i int) protocol.RosterRow {
	esc := func(n int) string { return strings.Repeat("\x01", n) }
	return protocol.RosterRow{
		HostID:    fmt.Sprintf("%012x", i),
		SessionID: fmt.Sprintf("%0128d", i),
		Host: protocol.RosterHost{
			PID:          math.MinInt64,
			CrazeVersion: esc(protocol.RosterCrazeVersionMax),
			Protocol:     math.MaxInt64,
			Provider:     esc(protocol.RosterProviderMax),
			Workspace:    esc(protocol.RosterWorkspaceMax),
			StartedAt:    time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.FixedZone("", -(23*3600+59*60))),
			Ready:        true,
		},
		Status:      protocol.RosterUnreachable,
		Approximate: true,
	}
}

// encodedLen is v's encoding on the wire, without its newline.
func encodedLen(t *testing.T, v any) int {
	t.Helper()
	b, err := protocol.MarshalLine(v)
	if err != nil {
		t.Fatal(err)
	}
	return len(b) - 1
}

// TestAFullRosterFitsOneLine (plan 032 §3.6): limits.go's roster bounds keep a
// whole roster in one line, by its encoding and not by arithmetic about it.
// A roster row at every bound of its host's, encoded the costliest way, fits
// RosterEntryBytesMax without its row — so the hub's last resort, dropping the
// row, always works; RosterRowsMax roster rows of exactly RosterEntryBytesMax
// each, in the hub's list reply and its subscription's reply (a request id of
// RosterRequestIDBytesMax, the hub's epoch and subscription id at 64 bytes,
// the widest cursor, truncated) and in a roster notification (RosterRowsMax
// upserts and RosterRowsMax removes besides), are each one line under
// OutboundLineMax. Such a row is on the schema. The negative control is the
// budget without its last resort: every roster row at its host's bounds and
// carrying a host row of RosterRowBytesMax overflows the line — the review's
// counterexample, and why RosterEntryBytesMax is a bound of its own.
func TestAFullRosterFitsOneLine(t *testing.T) {
	bare := worstRosterRow(0)
	if n := encodedLen(t, bare); n > protocol.RosterEntryBytesMax {
		t.Fatalf("a roster row at its host's bounds, with no row, encodes to %d bytes, over RosterEntryBytesMax (%d): dropping the row is no last resort",
			n, protocol.RosterEntryBytesMax)
	}
	// rowOf is a host row (any object, forwardedRow) of exactly n bytes.
	rowOf := func(n int) json.RawMessage {
		const shell = len(`{"p":""}`)
		return json.RawMessage(`{"p":"` + strings.Repeat("x", n-shell) + `"}`)
	}
	// atBudget is roster row i padded with a row to exactly
	// RosterEntryBytesMax.
	atBudget := func(i int) protocol.RosterRow {
		r := worstRosterRow(i)
		pad := protocol.RosterEntryBytesMax - encodedLen(t, r) - len(`,"row":`)
		if pad > protocol.RosterRowBytesMax {
			t.Fatalf("the padding row would be %d bytes, over RosterRowBytesMax", pad)
		}
		r.Row = rowOf(pad)
		if n := encodedLen(t, r); n != protocol.RosterEntryBytesMax {
			t.Fatalf("roster row %d encodes to %d bytes, want exactly %d", i, n, protocol.RosterEntryBytesMax)
		}
		return r
	}
	rows := make([]protocol.RosterRow, protocol.RosterRowsMax)
	removes := make([]string, protocol.RosterRowsMax)
	for i := range rows {
		rows[i] = atBudget(i)
		removes[i] = fmt.Sprintf("%012x", protocol.RosterRowsMax+i)
	}
	if r := compiled(t, newCompiler(), protocol.SchemaURI("sessions.list.json", "#/$defs/result")).Validate(
		[]byte(jsonOf(t, protocol.HubSessionsListResult{Epoch: "e", Sessions: rows[:1]}))); !r.IsValid() {
		t.Fatalf("a roster row at its bounds is off the schema: %v", r.Errors)
	}

	id := json.RawMessage(`"` + strings.Repeat("i", protocol.RosterRequestIDBytesMax-2) + `"`)
	epoch, sub := strings.Repeat("e", 64), strings.Repeat("r", 64)
	reply := func(result any) protocol.Response {
		b, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: id, Result: b}
	}
	for _, l := range []struct {
		name string
		v    any
	}{
		{"the hub's list reply", reply(protocol.HubSessionsListResult{Epoch: epoch, Cursor: math.MaxUint64, Sessions: rows, Truncated: true})},
		{"the subscription's reply", reply(protocol.SessionsSubscribeResult{Subscription: sub, Epoch: epoch, Cursor: math.MaxUint64,
			Sessions: rows, Truncated: true})},
		{"a roster notification", protocol.Notification{JSONRPC: protocol.JSONRPCVersion, Method: protocol.NotifyRoster,
			Params: json.RawMessage(jsonOf(t, protocol.RosterParams{Subscription: sub, Epoch: epoch, Cursor: math.MaxUint64,
				Upserts: rows, Removes: removes}))}},
	} {
		b, err := protocol.MarshalLine(l.v)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > protocol.OutboundLineMax {
			t.Errorf("%s at the roster's bounds is %d bytes, over OutboundLineMax (%d)", l.name, len(b), protocol.OutboundLineMax)
		}
		t.Logf("%s at the roster's bounds: %d bytes of %d", l.name, len(b), protocol.OutboundLineMax)
	}

	// Negative control: without RosterEntryBytesMax — every roster row at its
	// host's bounds and carrying a host row of RosterRowBytesMax — the list
	// overflows the line.
	over := make([]protocol.RosterRow, protocol.RosterRowsMax)
	for i := range over {
		over[i] = worstRosterRow(i)
		over[i].Row = rowOf(protocol.RosterRowBytesMax)
	}
	if n := encodedLen(t, reply(protocol.HubSessionsListResult{Epoch: epoch, Sessions: over})); n <= protocol.OutboundLineMax {
		t.Fatalf("every bound but RosterEntryBytesMax still fits a line (%d bytes): the control cannot tell the entry bound matters", n)
	}
}

// TestTheRosterBoundsAreTheSchemas: the schema states limits.go's roster
// bounds, number for number — each host string's maxLength and every roster
// list's maxItems — so what the hub enforces and what a client may assume are
// one set.
func TestTheRosterBoundsAreTheSchemas(t *testing.T) {
	for _, tc := range []struct {
		file, pointer string
		want          int
	}{
		{"info.json", "/$defs/rosterHost/properties/crazeVersion/maxLength", protocol.RosterCrazeVersionMax},
		{"info.json", "/$defs/rosterHost/properties/provider/maxLength", protocol.RosterProviderMax},
		{"info.json", "/$defs/rosterHost/properties/workspace/maxLength", protocol.RosterWorkspaceMax},
		{"sessions.list.json", "/$defs/hubResult/properties/sessions/maxItems", protocol.RosterRowsMax},
		{"sessions.subscribe.json", "/$defs/result/properties/sessions/maxItems", protocol.RosterRowsMax},
		{"notification.roster.json", "/$defs/params/properties/upserts/maxItems", protocol.RosterRowsMax},
		{"notification.roster.json", "/$defs/params/properties/removes/maxItems", protocol.RosterRowsMax},
	} {
		if got := schemaValue(t, tc.file, tc.pointer); got != float64(tc.want) {
			t.Errorf("%s#%s is %v, limits.go says %d", tc.file, tc.pointer, got, tc.want)
		}
	}
}
