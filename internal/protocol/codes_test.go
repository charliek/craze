package protocol_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/charliek/craze/internal/protocol"
)

// TestRetryIsTheFourCodesNothingRan (§3.6, 05's retry table): the same
// commandId is resent on unavailable, not_accepting, in_progress and
// stale_model, and on nothing else.
func TestRetryIsTheFourCodesNothingRan(t *testing.T) {
	var got []protocol.Code
	for _, c := range protocol.Codes() {
		if protocol.Retry(c) {
			got = append(got, c)
		}
	}
	want := []protocol.Code{protocol.CodeNotAccepting, protocol.CodeUnavailable, protocol.CodeInProgress, protocol.CodeStaleModel}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Retry is true for %v, want %v", got, want)
	}
	if protocol.Retry("teapot") || protocol.Retry("") {
		t.Fatal("Retry is true for a code outside the closed set")
	}
}

// TestTheReasonTableIsExhaustive (§3.2): every reason appears once; a host's
// reason sits under one code of the closed set, and every code has at least
// one; a code shared by several sentinels lists exactly §3.2's reasons, and
// every other code's one reason is the code itself; the client-side reasons
// have no code; and the lookups answer from the table.
func TestTheReasonTableIsExhaustive(t *testing.T) {
	shared := map[protocol.Code][]protocol.Reason{
		protocol.CodeNotAccepting: {"not_accepting", "not_in_turn", "start_failed"},
		protocol.CodeAborted:      {"command_aborted", "set_outcome_unknown", "bad_catalog", "context"},
		protocol.CodeUnavailable:  {"log_backed_up", "ask_unavailable", "set_unavailable", "not_run", "attach_raced", "not_ready", "busy"},
		protocol.CodeFailed:       {"option_gone", "failed", "response_too_large", "snapshot_too_large"},
		protocol.CodeBadRequest: {"bad_request", "bad_answer", "hello_required", "unknown_field", "line_too_long",
			"protocol_version", "bad_token", "already_attached"},
		protocol.CodeUnsupported: {"unsupported", "unknown_method", "stop_unsupported", "roster_unsupported", "hub_only"},
	}
	byCode := map[protocol.Code][]protocol.Reason{}
	seen := map[protocol.Reason]bool{}
	var clientSide []protocol.Reason
	for _, r := range protocol.Reasons() {
		if seen[r.Reason] {
			t.Errorf("reason %s is listed twice", r.Reason)
		}
		seen[r.Reason] = true
		if r.ClientSide {
			if r.Code != "" || r.Engine {
				t.Errorf("client-side reason %s has code %q, engine %v", r.Reason, r.Code, r.Engine)
			}
			clientSide = append(clientSide, r.Reason)
			continue
		}
		if !r.Code.Known() {
			t.Errorf("reason %s is under %q, which is no code", r.Reason, r.Code)
		}
		byCode[r.Code] = append(byCode[r.Code], r.Reason)
		if got := r.Reason.Code(); got != r.Code {
			t.Errorf("%s.Code() = %q, want %q", r.Reason, got, r.Code)
		}
	}
	for _, c := range protocol.Codes() {
		want, ok := shared[c]
		if !ok {
			want = []protocol.Reason{protocol.Reason(c)}
		}
		if got := byCode[c]; !slices.Equal(got, want) {
			t.Errorf("code %s: reasons %v, want %v", c, got, want)
		}
	}
	if len(byCode) != len(protocol.Codes()) {
		t.Errorf("reasons sit under %d codes, the closed set has %d", len(byCode), len(protocol.Codes()))
	}
	if want := []protocol.Reason{protocol.ReasonResumeLost, protocol.ReasonDisconnected, protocol.ReasonNoAnswer}; !slices.Equal(clientSide, want) {
		t.Errorf("client-side reasons %v, want %v", clientSide, want)
	}
	for _, r := range clientSide {
		if !r.ClientSide() || r.Code() != "" {
			t.Errorf("%s: ClientSide %v, Code %q", r, r.ClientSide(), r.Code())
		}
	}
	if _, ok := protocol.Reason("teapot").Lookup(); ok {
		t.Fatal("Lookup found a reason the table does not hold")
	}
	if len(protocol.Codes()) != 23 {
		t.Fatalf("the closed set has %d codes: 05's twenty-two and unknown_subagent are 23", len(protocol.Codes()))
	}
}

// TestTheListsAreCopies: a caller that edits a list it was handed changes
// nothing the package holds.
func TestTheListsAreCopies(t *testing.T) {
	protocol.Codes()[0] = "x"
	protocol.Reasons()[0].Reason = "x"
	protocol.Methods()[0].Name = "x"
	protocol.Notifications()[0] = "x"
	protocol.SupportedProtocols()[0] = 99
	protocol.ResetReasons()[0] = "x"
	protocol.CursorReasons()[0] = "x"
	protocol.Activities()[0] = "x"
	if protocol.Codes()[0] == "x" || protocol.Reasons()[0].Reason == "x" || protocol.Methods()[0].Name == "x" ||
		protocol.Notifications()[0] == "x" || protocol.SupportedProtocols()[0] == 99 || protocol.ResetReasons()[0] == "x" ||
		protocol.CursorReasons()[0] == "x" || protocol.Activities()[0] == "x" {
		t.Fatal("a list handed out shares its storage")
	}
}

// TestTheLimits pins the numbers §3.2, §3.6 and §3.7 give, and that each
// holds the relations the plan relies on: the writer queue fits two of the
// longest line, a snapshot fits a line, the inbound limit is below the
// outbound one.
func TestTheLimits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want int
	}{
		{"InboundLineMax", protocol.InboundLineMax, 4 << 20},
		{"OutboundLineMax", protocol.OutboundLineMax, 16 << 20},
		{"SnapshotBytesMax", protocol.SnapshotBytesMax, 8 << 20},
		{"AskItemCap", protocol.AskItemCap, 256 << 10},
		{"WriterQueueBytes", protocol.WriterQueueBytes, 32 << 20},
		{"ResetReserveBytes", protocol.ResetReserveBytes, 1 << 10},
		{"RequestsPerConnection", protocol.RequestsPerConnection, 16},
		{"CommandsPerHost", protocol.CommandsPerHost, 64},
		{"ReattachesPerEpisode", protocol.ReattachesPerEpisode, 8},
		{"ProtocolVersion", protocol.ProtocolVersion, 1},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	if !slices.Equal(protocol.SupportedProtocols(), []int{1}) {
		t.Errorf("SupportedProtocols = %v", protocol.SupportedProtocols())
	}
	if protocol.WriterQueueBytes < 2*protocol.OutboundLineMax || protocol.SnapshotBytesMax >= protocol.OutboundLineMax ||
		protocol.InboundLineMax >= protocol.OutboundLineMax {
		t.Fatal("the limits no longer nest as §3.2 and §3.7 need")
	}
	if l := protocol.HostLimits(); l.InboundLine != protocol.InboundLineMax || l.OutboundLine != protocol.OutboundLineMax {
		t.Fatalf("HostLimits = %+v", l)
	}
	if c := protocol.HostCapabilities(); c.RosterSubscribe || c.SessionCreate || c.Multiplex || c.Connect || !c.Snapshot || !c.AttachWhenNow {
		t.Fatalf("HostCapabilities = %+v", c)
	}
}

// TestTheMethodTable pins §3.3's method list, the reserved session.create
// last, and which of them a TUI host answers unsupported, with which reason
// (X6): each an unsupported reason of the table.
func TestTheMethodTable(t *testing.T) {
	var names []string
	unsupported := map[string]protocol.Reason{}
	for _, m := range protocol.Methods() {
		names = append(names, m.Name)
		if m.HostUnsupported != "" {
			unsupported[m.Name] = m.HostUnsupported
			if m.HostUnsupported.Code() != protocol.CodeUnsupported {
				t.Errorf("%s: a host answers reason %q, which is not an unsupported reason", m.Name, m.HostUnsupported)
			}
		}
		if got, ok := protocol.Method(m.Name); !ok || got != m {
			t.Errorf("Method(%q) = %+v, %v", m.Name, got, ok)
		}
	}
	want := []string{"hello", "sessions.list", "sessions.subscribe", "session.connect", "session.attach", "session.detach",
		"session.state", "session.snapshot", "session.sync", "session.prompt", "session.cancel", "session.disarm",
		"session.queue.add", "session.queue.edit", "session.queue.remove", "session.queue.clear", "session.set",
		"session.setTitle", "session.subagent.cancel", "session.stop", "asks.list", "asks.get", "asks.answer",
		"session.create"}
	if !slices.Equal(names, want) {
		t.Fatalf("methods\n got %v\nwant %v", names, want)
	}
	wantUnsupported := map[string]protocol.Reason{
		"sessions.subscribe": protocol.ReasonRosterUnsupported,
		"session.connect":    protocol.ReasonHubOnly,
		"session.stop":       protocol.ReasonStopUnsupported,
		"session.create":     protocol.ReasonHubOnly,
	}
	if !maps.Equal(unsupported, wantUnsupported) {
		t.Fatalf("host-unsupported %v, want %v", unsupported, wantUnsupported)
	}
	if _, ok := protocol.Method("session.teleport"); ok {
		t.Fatal("Method found a method protocol 1 does not name")
	}
	if want := []string{"event", "synchronized", "ready", "reset"}; !slices.Equal(protocol.Notifications(), want) {
		t.Fatalf("notifications %v, want %v", protocol.Notifications(), want)
	}
}
