package host

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/version"
)

const (
	roostTestTab = "5"
	sessA        = "sess-a"
	sessB        = "sess-b"
)

type obj = map[string]any

// roostOwner is who a fake roost tab belongs to: roost's ownership identity.
type roostOwner struct{ source, session string }

// fakeRoost models the part of roost's tab.agent_report craze relies on
// (crates/roost-ipc/src/agent.rs apply_report): a claim always takes the tab; a
// preserve or a release is accepted only from the owner, and an accepted
// release clears it. Every reply echoes the request id and carries the tab's
// ownership as it stands after the report. hook, if set, sees each request
// first and may answer it itself by returning true.
type fakeRoost struct {
	*fakeUDS
	mu    sync.Mutex
	owner *roostOwner
}

type roostHook func(f *fakeRoost, conn net.Conn, req obj, stop <-chan struct{}) (handled bool)

func newFakeRoost(t *testing.T, hook roostHook) *fakeRoost {
	t.Helper()
	f := &fakeRoost{}
	f.fakeUDS = newFakeUDS(t, func(conn net.Conn, _ int, req map[string]any, stop <-chan struct{}) {
		if hook != nil && hook(f, conn, req, stop) {
			return
		}
		writeLine(t, conn, f.apply(req))
	})
	return f
}

func (f *fakeRoost) apply(req obj) obj {
	params, _ := req["params"].(map[string]any)
	source, _ := params["source"].(string)
	session, _ := params["session_id"].(string)
	action, _ := params["ownership_action"].(string)

	f.mu.Lock()
	defer f.mu.Unlock()
	matches := f.owner != nil && f.owner.source == source && f.owner.session == session
	accepted := true
	switch action {
	case "claim":
		f.owner = &roostOwner{source, session}
	case "preserve":
		accepted = matches
	case "release":
		accepted = matches
		if matches {
			f.owner = nil
		}
	}
	return roostReplyObj(req["id"], accepted, f.owner)
}

// setOwner stands in for whatever else changes the tab's owner: nil for a
// roost that restarted, manual for `roostctl tab set-state`.
func (f *fakeRoost) setOwner(o *roostOwner) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owner = o
}

// roostReplyObj is roost's reply shape (tests/ipc-vectors/
// tab.agent_report.response.json), trimmed to the tab fields craze reads plus
// a few it must ignore.
func roostReplyObj(id any, accepted bool, owner *roostOwner) obj {
	tab := obj{"id": roostTestTab, "state": "none", "agent_lifecycle": "inactive"}
	if owner != nil {
		tab["ownership"] = obj{
			"source": owner.source, "session_id": owner.session,
			"last_event_at": 1700000060, "detail": "", "metadata": obj{},
		}
	}
	return obj{"id": id, "ok": true, "result": obj{"accepted": accepted, "tab": tab}}
}

func newTestRoost(socket string) *Roost { return &Roost{socket: socket, tabID: roostTestTab} }

// roostWant is one whole expected request line.
func roostWant(seq uint64, session, action string, extra obj) obj {
	p := obj{"tab_id": roostTestTab, "source": "craze", "session_id": session, "ownership_action": action}
	maps.Copy(p, extra)
	return obj{"id": strconv.FormatUint(seq, 10), "op": "tab.agent_report", "params": p}
}

func claimWant(seq uint64, session string, md obj) obj {
	return roostWant(seq, session, "claim", obj{
		"lifecycle": "inactive", "attention": "clear", "detail": "session_start", "metadata": md,
	})
}

func releaseWant(seq uint64, session string) obj {
	return roostWant(seq, session, "release", obj{
		"lifecycle": "inactive", "attention": "clear", "detail": "session_end",
	})
}

func workingWant(seq uint64, session string) obj {
	return roostWant(seq, session, "preserve", obj{"lifecycle": "working", "attention": "clear", "detail": "prompt"})
}

func turnCompleteWant(seq uint64, session string) obj {
	return roostWant(seq, session, "preserve", obj{
		"lifecycle": "finished", "attention": "set", "severity": "info",
		"title": "craze", "body": "Turn complete", "detail": "stop",
	})
}

func cancelledWant(seq uint64, session string) obj {
	return roostWant(seq, session, "preserve", obj{"lifecycle": "finished", "attention": "clear", "detail": "cancelled"})
}

// roost019Params is every params field roost-session 0.0.19 accepts, as its
// unknown-field error lists them. That roost denies unknown fields, so a line
// with any other key — lifecycle_if, which newer roost added — is rejected
// whole.
var roost019Params = []string{
	"tab_id", "source", "session_id", "ownership_action", "lifecycle",
	"attention", "severity", "title", "body", "detail", "metadata",
}

// assertRoost019Fields scans every recorded line: no params key outside what
// roost-session 0.0.19 accepts, and no lifecycle_if anywhere in the bytes.
func assertRoost019Fields(t *testing.T, srv *fakeUDS) {
	t.Helper()
	for i, r := range srv.requests() {
		if bytes.Contains(r.line, []byte("lifecycle_if")) {
			t.Fatalf("line %d carries lifecycle_if, which roost-session 0.0.19 rejects: %s", i, r.line)
		}
		for k := range decodeJSONObject(t, r.line)["params"].(map[string]any) {
			if !slices.Contains(roost019Params, k) {
				t.Fatalf("line %d: params key %q is not one roost-session 0.0.19 accepts: %s", i, k, r.line)
			}
		}
	}
}

// assertRoostFraming: every request is exactly one newline-terminated line on
// a connection of its own, with a string-wrapped int64 id and tab id, and
// only fields the oldest supported roost accepts.
func assertRoostFraming(t *testing.T, srv *fakeUDS) {
	t.Helper()
	assertRoost019Fields(t, srv)
	for i, r := range srv.requests() {
		if r.conn != i {
			t.Fatalf("line %d arrived on connection %d: want one line per connection", i, r.conn)
		}
		if !bytes.HasSuffix(r.line, []byte("\n")) || bytes.Count(r.line, []byte("\n")) != 1 {
			t.Fatalf("line %d is not exactly one newline-terminated line: %q", i, r.line)
		}
		m := decodeJSONObject(t, r.line)
		for _, v := range []any{m["id"], m["params"].(map[string]any)["tab_id"]} {
			s, ok := v.(string)
			if !ok || s == "" || strings.Trim(s, "0123456789") != "" {
				t.Fatalf("line %d: %v must be a string-wrapped int64: %s", i, v, r.line)
			}
		}
	}
}

// assertRoostLines compares every recorded line, whole, against want.
func assertRoostLines(t *testing.T, srv *fakeUDS, want ...obj) {
	t.Helper()
	reqs := srv.requests()
	if len(reqs) != len(want) {
		var got []string
		for _, r := range reqs {
			got = append(got, strings.TrimSpace(string(r.line)))
		}
		t.Fatalf("got %d lines, want %d:\n%s", len(reqs), len(want), strings.Join(got, "\n"))
	}
	for i, r := range reqs {
		assertJSONEqual(t, decodeJSONObject(t, r.line), want[i])
	}
	assertRoostFraming(t, srv)
}

// roostParamsLog is every recorded line's params, for a Hub-driven test whose
// ids come from the Hub's clock.
func roostParamsLog(t *testing.T, srv *fakeUDS) []obj {
	t.Helper()
	var out []obj
	for _, r := range srv.requests() {
		out = append(out, decodeJSONObject(t, r.line)["params"].(map[string]any))
	}
	return out
}

func actionsOf(params []obj) []string {
	var out []string
	for _, p := range params {
		out = append(out, p["ownership_action"].(string)+"/"+p["session_id"].(string))
	}
	return out
}

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestNewRoostFromEnv mirrors roost's own hook gate (crates/roost-agent/src/
// hook.rs parse_tab_id: str::parse::<i64>, then > 0). Rust's parse takes a
// leading '+' and leading zeros, and nothing else ParseInt(…, 10, 64) would
// not; the id goes out canonical, as roost's hook re-serialises it.
// Negative controls checked: trimming ROOST_TAB_ID before parsing failed the
// whitespace cases, and accepting 0 failed the zero case; both were reverted.
func TestNewRoostFromEnv(t *testing.T) {
	const sock = "/tmp/roost.sock"
	cases := []struct {
		name, socket, tab string
		wantTab           string // "" means inactive
	}{
		{"valid pair", sock, "5", "5"},
		{"max int64", sock, "9223372036854775807", "9223372036854775807"},
		{"leading plus, as Rust's parse", sock, "+5", "5"},
		{"leading zeros, as Rust's parse", sock, "007", "7"},
		{"zero", sock, "0", ""},
		{"negative", sock, "-1", ""},
		{"minus zero", sock, "-0", ""},
		{"leading space", sock, " 5", ""},
		{"trailing space", sock, "5 ", ""},
		{"trailing newline", sock, "5\n", ""},
		{"not a number", sock, "abc", ""},
		{"underscore", sock, "1_0", ""},
		{"sign alone", sock, "+", ""},
		{"int64 overflow", sock, "9223372036854775808", ""},
		{"empty tab id", sock, "", ""},
		{"empty socket", "", "5", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"ROOST_SOCKET": tc.socket, "ROOST_TAB_ID": tc.tab}
			r, ok := NewRoostFromEnv(func(k string) string { return env[k] })
			if tc.wantTab == "" {
				if ok || r != nil {
					t.Fatalf("want inactive, got ok=%v r=%+v", ok, r)
				}
				return
			}
			if !ok || r == nil {
				t.Fatalf("want active, got ok=%v", ok)
			}
			if r.tabID != tc.wantTab || r.socket != tc.socket || r.Name() != "roost" {
				t.Fatalf("got tab %q socket %q name %q, want tab %q socket %q name roost", r.tabID, r.socket, r.Name(), tc.wantTab, tc.socket)
			}
		})
	}
}

// TestRoostOrdinarySequence is plan 015 §3.4 for a plain turn: the ready idle
// is the claim and nothing else, then working, finished with the "Turn
// complete" banner, and the release. Negative
// control checked: sending a lifecycle line for Idle ready failed the line
// count after the first Report, then was reverted.
func TestRoostOrdinarySequence(t *testing.T) {
	srv := newFakeRoost(t, nil)
	r := newTestRoost(srv.socket)
	ctx := context.Background()
	ready := Status{Kind: Idle, Detail: DetailReady, SessionID: sessA, Provider: "cursor", Model: "gpt-5"}

	mustNil(t, r.Report(ctx, ready, 1))
	claim := claimWant(1, sessA, obj{"model": "gpt-5", "craze.provider": "cursor", "version": version.Version})
	assertRoostLines(t, srv.fakeUDS, claim)
	// The ready idle is recorded as stated: said again, it sends nothing.
	mustNil(t, r.Report(ctx, ready, 2))
	assertRoostLines(t, srv.fakeUDS, claim)

	working := ready
	working.Kind, working.Detail = Working, DetailPrompt
	mustNil(t, r.Report(ctx, working, 3))
	stop := ready
	stop.Detail = DetailStop
	mustNil(t, r.Report(ctx, stop, 4))
	mustNil(t, r.Release(ctx, 5))

	assertRoostLines(t, srv.fakeUDS,
		claim,
		workingWant(3, sessA),
		turnCompleteWant(4, sessA),
		releaseWant(5, sessA),
	)
}

// TestRoostStateLines is §3.4's table row by row, each after a claim — the two
// finished rows need a running turn first, so they are
// TestRoostFinishedOnlyAfterARunningTurn's — plus the
// empty-body fallbacks roost's validate_report needs (attention=set with an
// empty body is rejected outright). Negative control checked: dropping the
// Blocked fallback made the empty-message case fail with no "body" key, then
// was reverted.
func TestRoostStateLines(t *testing.T) {
	cases := []struct {
		name string
		s    Status
		want obj
	}{
		{"working", Status{Kind: Working, Detail: DetailPrompt},
			obj{"lifecycle": "working", "attention": "clear", "detail": "prompt"}},
		{"foreign turn", Status{Kind: Working, Detail: DetailForeignTurn},
			obj{"lifecycle": "working", "attention": "clear", "detail": "foreign_turn"}},
		{"permission card", Status{Kind: Blocked, Message: "permission Shell", Detail: DetailPermissionPrompt},
			obj{"lifecycle": "waiting", "attention": "set", "severity": "warn", "title": "craze", "body": "permission Shell", "detail": "permission_prompt"}},
		{"question card", Status{Kind: Blocked, Message: "question", Detail: DetailQuestion},
			obj{"lifecycle": "waiting", "attention": "set", "severity": "warn", "title": "craze", "body": "question", "detail": "question"}},
		{"blocked, empty message", Status{Kind: Blocked, Detail: DetailPlan},
			obj{"lifecycle": "waiting", "attention": "set", "severity": "warn", "title": "craze", "body": "needs input", "detail": "plan"}},
		{"failed", Status{Kind: Failed, Message: "json-rpc error -32000: boom", Detail: DetailError},
			obj{"lifecycle": "failed", "attention": "set", "severity": "error", "title": "craze", "body": "json-rpc error -32000: boom", "detail": "error"}},
		{"failed, empty message", Status{Kind: Failed, Detail: DetailError},
			obj{"lifecycle": "failed", "attention": "set", "severity": "error", "title": "craze", "body": "turn failed", "detail": "error"}},
		{"failed at start, with a session", Status{Kind: Failed, Message: "set mode failed", Detail: DetailStartFailed},
			obj{"lifecycle": "failed", "attention": "set", "severity": "error", "title": "craze", "body": "set mode failed", "detail": "error"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeRoost(t, nil)
			r := newTestRoost(srv.socket)
			ctx := context.Background()
			mustNil(t, r.Report(ctx, Status{Kind: Idle, Detail: DetailReady, SessionID: sessA}, 1))
			s := tc.s
			s.SessionID = sessA
			mustNil(t, r.Report(ctx, s, 2))
			assertRoostLines(t, srv.fakeUDS,
				claimWant(1, sessA, obj{"version": version.Version}),
				roostWant(2, sessA, "preserve", tc.want),
			)
		})
	}
}

// TestRoostModelMetadata: a model change with no state change is a preserve
// line with metadata alone; with a state change it is folded into the state
// line; a model that went empty, or a provider change, sends nothing (the
// claim is the only line that carries the provider). Negative control checked:
// always folding the model into the state line made the finished line carry
// metadata, then was reverted.
func TestRoostModelMetadata(t *testing.T) {
	srv := newFakeRoost(t, nil)
	r := newTestRoost(srv.socket)
	ctx := context.Background()
	s := Status{Kind: Idle, Detail: DetailReady, SessionID: sessA, Provider: "cursor", Model: "m1"}

	mustNil(t, r.Report(ctx, s, 1))
	s.Model = "m2" // the ready idle again, model changed
	mustNil(t, r.Report(ctx, s, 2))
	s.Kind, s.Detail, s.Model = Working, DetailPrompt, "m3"
	mustNil(t, r.Report(ctx, s, 3))
	s.Kind, s.Detail = Idle, DetailStop
	mustNil(t, r.Report(ctx, s, 4))
	s.Model = ""
	mustNil(t, r.Report(ctx, s, 5))
	s.Provider = "grok"
	mustNil(t, r.Report(ctx, s, 6))

	assertRoostLines(t, srv.fakeUDS,
		claimWant(1, sessA, obj{"model": "m1", "craze.provider": "cursor", "version": version.Version}),
		roostWant(2, sessA, "preserve", obj{"metadata": obj{"model": "m2"}}),
		roostWant(3, sessA, "preserve", obj{"lifecycle": "working", "attention": "clear", "detail": "prompt", "metadata": obj{"model": "m3"}}),
		turnCompleteWant(4, sessA),
	)
}

// TestRoostFinishedOnlyAfterARunningTurn is the client-side stand-in for
// roost's lifecycle_if guard (amendment A13): a stop or a cancel is a finished
// line only when the last lifecycle craze sent was working or waiting. A stop
// with nothing running before it sends no state line — a model change still
// goes out on its own — and a stop after a cancel does not banner a turn that
// already ended. Negative control checked: removing the guard sent a "Turn
// complete" for the stop right after the claim, then was reverted.
func TestRoostFinishedOnlyAfterARunningTurn(t *testing.T) {
	srv := newFakeRoost(t, nil)
	r := newTestRoost(srv.socket)
	ctx := context.Background()
	s := func(k Kind, detail, model string) Status {
		return Status{Kind: k, Detail: detail, SessionID: sessA, Model: model, Message: map[Kind]string{Blocked: "question"}[k]}
	}

	mustNil(t, r.Report(ctx, s(Idle, DetailReady, "m1"), 1))
	mustNil(t, r.Report(ctx, s(Idle, DetailStop, "m2"), 2)) // no turn was running
	mustNil(t, r.Report(ctx, s(Blocked, DetailQuestion, "m2"), 3))
	mustNil(t, r.Report(ctx, s(Idle, DetailCancelled, "m2"), 4))
	mustNil(t, r.Report(ctx, s(Idle, DetailStop, "m2"), 5)) // that turn already finished
	mustNil(t, r.Report(ctx, s(Working, DetailPrompt, "m2"), 6))
	mustNil(t, r.Report(ctx, s(Idle, DetailStop, "m2"), 7))
	mustNil(t, r.Release(ctx, 8))

	assertRoostLines(t, srv.fakeUDS,
		claimWant(1, sessA, obj{"model": "m1", "version": version.Version}),
		roostWant(2, sessA, "preserve", obj{"metadata": obj{"model": "m2"}}),
		roostWant(3, sessA, "preserve", obj{"lifecycle": "waiting", "attention": "set", "severity": "warn", "title": "craze", "body": "question", "detail": "question"}),
		cancelledWant(4, sessA),
		workingWant(6, sessA),
		turnCompleteWant(7, sessA),
		releaseWant(8, sessA),
	)
}

// TestRoostReclaimRaisesNoTurnComplete: roost restarts while a turn runs, and
// the next Report re-claims while a stop is the current status. The claim puts
// the tab at inactive, and re-stating the stop there would raise a "Turn
// complete" for a turn roost never saw running, so that Report is the claim
// alone — whether the line roost dropped was the stop's own finished line, or
// a metadata line sent while craze still had the turn running. Negative
// controls checked: removing the guard failed both cases, and not resetting the
// tracked lifecycle on a claim failed the second; both were reverted.
func TestRoostReclaimRaisesNoTurnComplete(t *testing.T) {
	cases := []struct {
		name    string
		dropped Status // answered accepted:false with no owner
		want    obj
	}{
		{"the stop's own line dropped",
			Status{Kind: Idle, Detail: DetailStop, Model: "m1"}, turnCompleteWant(2, sessA)},
		{"a metadata line dropped mid-turn",
			Status{Kind: Working, Detail: DetailPrompt, Model: "m1.5"}, roostWant(2, sessA, "preserve", obj{"metadata": obj{"model": "m1.5"}})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeRoost(t, nil)
			r := newTestRoost(srv.socket)
			ctx := context.Background()

			mustNil(t, r.Report(ctx, Status{Kind: Working, Detail: DetailPrompt, SessionID: sessA, Model: "m1"}, 1))
			srv.setOwner(nil)
			dropped := tc.dropped
			dropped.SessionID = sessA
			mustNil(t, r.Report(ctx, dropped, 2))
			mustNil(t, r.Report(ctx, Status{Kind: Idle, Detail: DetailStop, SessionID: sessA, Model: "m2"}, 3))
			mustNil(t, r.Release(ctx, 4))

			assertRoostLines(t, srv.fakeUDS,
				claimWant(1, sessA, obj{"model": "m1", "version": version.Version}),
				workingWant(1, sessA),
				tc.want,
				claimWant(3, sessA, obj{"model": "m2", "version": version.Version}),
				releaseWant(4, sessA),
			)
		})
	}
}

// TestRoostNeverSendsLifecycleIf drives every kind of line craze writes —
// claim, metadata alone, working, waiting, both finished lines, failed and
// release — and scans the bytes alone, with no whole-object comparison in
// front of it: nothing may carry a field roost-session 0.0.19 rejects.
// Negative control checked: putting a lifecycle_if field back on the finished
// lines failed the scan, then was reverted.
func TestRoostNeverSendsLifecycleIf(t *testing.T) {
	srv := newFakeRoost(t, nil)
	r := newTestRoost(srv.socket)
	ctx := context.Background()
	for i, st := range []Status{
		{Kind: Idle, Detail: DetailReady, Provider: "cursor", Model: "m1"},
		{Kind: Idle, Detail: DetailReady, Provider: "cursor", Model: "m2"},
		{Kind: Working, Detail: DetailPrompt, Model: "m2"},
		{Kind: Blocked, Message: "permission Shell", Detail: DetailPermissionPrompt, Model: "m2"},
		{Kind: Idle, Detail: DetailCancelled, Model: "m2"},
		{Kind: Working, Detail: DetailPrompt, Model: "m2"},
		{Kind: Idle, Detail: DetailStop, Model: "m2"},
		{Kind: Working, Detail: DetailPrompt, Model: "m2"},
		{Kind: Failed, Message: "boom", Detail: DetailError, Model: "m2"},
	} {
		st.SessionID = sessA
		mustNil(t, r.Report(ctx, st, uint64(i+1)))
	}
	mustNil(t, r.Release(ctx, 100))

	if n := len(srv.requests()); n != 10 {
		t.Fatalf("want all 10 lines to scan, got %d", n)
	}
	assertRoost019Fields(t, srv.fakeUDS)
}

// TestRoostClaimRetriedAfterTimeout: a claim that never got its reply is not
// taken as acknowledged, so the next Report claims again before its state
// line; the lost claim's Report fails on its deadline. Negative control
// checked: setting claimed before the send (on the attempt) made the second
// Report skip its claim, then was reverted.
func TestRoostClaimRetriedAfterTimeout(t *testing.T) {
	var first atomic.Bool
	srv := newFakeRoost(t, func(_ *fakeRoost, _ net.Conn, _ obj, stop <-chan struct{}) bool {
		if first.CompareAndSwap(false, true) {
			<-stop // never reply
			return true
		}
		return false
	})
	r := newTestRoost(srv.socket)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := r.Report(ctx, Status{Kind: Working, Detail: DetailPrompt, SessionID: sessA}, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want an error wrapping context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Report took %v against a 50ms ctx", elapsed)
	}

	ctx = context.Background()
	mustNil(t, r.Report(ctx, Status{Kind: Blocked, Message: "question", Detail: DetailQuestion, SessionID: sessA}, 2))
	mustNil(t, r.Report(ctx, Status{Kind: Working, Detail: DetailPrompt, SessionID: sessA}, 3))
	mustNil(t, r.Release(ctx, 4))

	md := obj{"version": version.Version}
	assertRoostLines(t, srv.fakeUDS,
		claimWant(1, sessA, md),
		claimWant(2, sessA, md),
		roostWant(2, sessA, "preserve", obj{"lifecycle": "waiting", "attention": "set", "severity": "warn", "title": "craze", "body": "question", "detail": "question"}),
		workingWant(3, sessA),
		releaseWant(4, sessA),
	)
}

// TestRoostAcceptedFalseWithoutOwnerReclaims: a tab with no owner at all (roost
// restarted) drops the report without an error, and the next Report claims
// again — and states its state again, even one unchanged since the dropped
// line, because the claim put roost's lifecycle back to inactive. Negative
// control checked: not resetting the stated state on a claim made the third
// Report send the claim alone, then was reverted.
func TestRoostAcceptedFalseWithoutOwnerReclaims(t *testing.T) {
	srv := newFakeRoost(t, nil)
	r := newTestRoost(srv.socket)
	ctx := context.Background()
	working := Status{Kind: Working, Detail: DetailPrompt, SessionID: sessA, Model: "m1"}

	mustNil(t, r.Report(ctx, working, 1))
	srv.setOwner(nil)
	working.Model = "m2"
	if err := r.Report(ctx, working, 2); err != nil {
		t.Fatalf("a report dropped for want of any owner is not an error: %v", err)
	}
	working.Model = "m3" // same state, so only the re-claim can bring the state back
	mustNil(t, r.Report(ctx, working, 3))
	mustNil(t, r.Release(ctx, 4))

	assertRoostLines(t, srv.fakeUDS,
		claimWant(1, sessA, obj{"model": "m1", "version": version.Version}),
		workingWant(1, sessA),
		roostWant(2, sessA, "preserve", obj{"metadata": obj{"model": "m2"}}),
		claimWant(3, sessA, obj{"model": "m3", "version": version.Version}),
		workingWant(3, sessA),
		releaseWant(4, sessA),
	)
}

// TestRoostManualOwnerWarnsOnceAndNeverReclaims drives the reporter through a
// real Hub: once `roostctl tab set-state` has claimed the tab as manual, every
// report is an error naming the owner — one Warn — and craze keeps sending
// preserve lines, never a claim, so the override stands; the release at Close
// is dropped too. Negative control checked: clearing claimed on any
// accepted:false made a second claim for sess-a appear, then was reverted.
func TestRoostManualOwnerWarnsOnceAndNeverReclaims(t *testing.T) {
	srv := newFakeRoost(t, nil)
	g := newRig(t, nil, newTestRoost(srv.socket))

	lines := func(n int) {
		t.Helper()
		waitFor(t, strconv.Itoa(n)+" roost lines", func() bool { return len(srv.requests()) >= n })
	}
	g.h.Publish(Status{Kind: Working, Detail: DetailPrompt, SessionID: sessA})
	lines(2)
	srv.setOwner(&roostOwner{source: "manual"})
	g.h.Publish(Status{Kind: Blocked, Message: "permission Shell", Detail: DetailPermissionPrompt, SessionID: sessA})
	lines(3)
	waitFor(t, "the warning", func() bool { return len(g.warns.snapshot()) >= 1 })
	g.h.Publish(Status{Kind: Working, Detail: DetailPrompt, SessionID: sessA})
	lines(4)
	g.h.Publish(Status{Kind: Failed, Message: "boom", Detail: DetailError, SessionID: sessA})
	lines(5)
	g.h.Close(context.Background())

	params := roostParamsLog(t, srv.fakeUDS)
	wantActions := []string{"claim/sess-a", "preserve/sess-a", "preserve/sess-a", "preserve/sess-a", "preserve/sess-a", "release/sess-a"}
	if got := actionsOf(params); !slices.Equal(got, wantActions) {
		t.Fatalf("actions\n got  %q\n want %q", got, wantActions)
	}
	warns := g.warns.snapshot()
	want := `host status: roost: report not accepted: tab 5 is owned by source "manual", session ""`
	if len(warns) != 1 || warns[0] != want {
		t.Fatalf("warns %q, want exactly [%q]", warns, want)
	}
	assertRoostFraming(t, srv.fakeUDS)
}

// TestRoostNewSessionClaimsAfterManualOverride: the override stands only for
// the session it displaced; a new session is a new claim. Negative control
// checked: claiming only while nothing had been claimed yet made the sess-b
// report a preserve that roost dropped as manual's, then was reverted.
func TestRoostNewSessionClaimsAfterManualOverride(t *testing.T) {
	srv := newFakeRoost(t, nil)
	r := newTestRoost(srv.socket)
	ctx := context.Background()

	mustNil(t, r.Report(ctx, Status{Kind: Idle, Detail: DetailReady, SessionID: sessA}, 1))
	srv.setOwner(&roostOwner{source: "manual"})
	if err := r.Report(ctx, Status{Kind: Working, Detail: DetailPrompt, SessionID: sessA}, 2); err == nil || !strings.Contains(err.Error(), `"manual"`) {
		t.Fatalf("want an error naming the manual owner, got %v", err)
	}
	mustNil(t, r.Report(ctx, Status{Kind: Working, Detail: DetailPrompt, SessionID: sessB}, 3))
	mustNil(t, r.Release(ctx, 4))

	md := obj{"version": version.Version}
	assertRoostLines(t, srv.fakeUDS,
		claimWant(1, sessA, md),
		workingWant(2, sessA),
		claimWant(3, sessB, md),
		workingWant(3, sessB),
		releaseWant(4, sessB),
	)
}

// TestRoostReleaseNamesTheLastAttemptedClaim: Release hands back the session
// of the last claim sent, even one whose reply never came — it may well have
// landed. Negative control checked: releasing the acknowledged session instead
// made the release carry sess-a, then was reverted.
func TestRoostReleaseNamesTheLastAttemptedClaim(t *testing.T) {
	var claims atomic.Int32
	srv := newFakeRoost(t, func(_ *fakeRoost, _ net.Conn, req obj, stop <-chan struct{}) bool {
		params := req["params"].(map[string]any)
		if params["ownership_action"] == "claim" && claims.Add(1) == 2 {
			<-stop // the second claim is never answered
			return true
		}
		return false
	})
	r := newTestRoost(srv.socket)

	mustNil(t, r.Report(context.Background(), Status{Kind: Idle, Detail: DetailReady, SessionID: sessA}, 1))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.Report(ctx, Status{Kind: Idle, Detail: DetailReady, SessionID: sessB}, 2); err == nil {
		t.Fatal("want the unanswered claim to fail")
	}
	releaseErr := r.Release(context.Background(), 3)

	md := obj{"version": version.Version}
	assertRoostLines(t, srv.fakeUDS, claimWant(1, sessA, md), claimWant(2, sessB, md), releaseWant(3, sessB))
	// The fake still holds sess-a as owner, so this release is dropped, and
	// says whose tab it is.
	if releaseErr == nil || !strings.Contains(releaseErr.Error(), `session "sess-a"`) {
		t.Fatalf("want the dropped release to name the owner, got %v", releaseErr)
	}
}

// TestRoostSendsNothingWithoutASession: before the ACP session id exists —
// including a session that failed to start — there is nothing a claim could
// be matched against later, so nothing is sent, and a reporter that never
// attempted a claim releases nothing. Negative control checked: dropping the
// empty-session guard sent a claim with session_id "", then was reverted.
func TestRoostSendsNothingWithoutASession(t *testing.T) {
	srv := newFakeRoost(t, nil)
	r := newTestRoost(srv.socket)
	ctx := context.Background()

	mustNil(t, r.Report(ctx, Status{Kind: Failed, Message: "authentication failed", Detail: DetailStartFailed, Provider: "cursor"}, 1))
	mustNil(t, r.Report(ctx, Status{Kind: Idle, Detail: DetailReady, Model: "m"}, 2))
	mustNil(t, r.Release(ctx, 3))
	if n := srv.connections(); n != 0 {
		t.Fatalf("want no connection at all, got %d: %v", n, srv.requests())
	}

	// Through a Hub too: the Report is attempted, so the Hub calls Release,
	// and Release has no claim to hand back.
	spy := &spyReporter{Reporter: newTestRoost(srv.socket)}
	g := newRig(t, nil, spy)
	g.h.Publish(Status{Kind: Failed, Message: "authentication failed", Detail: DetailStartFailed})
	waitFor(t, "the Report", func() bool { return spy.reports.Load() == 1 })
	g.h.Close(context.Background())
	if spy.releases.Load() != 1 {
		t.Fatalf("the Hub called Release %d times, want 1", spy.releases.Load())
	}
	if n := srv.connections(); n != 0 {
		t.Fatalf("want no connection through the Hub either, got %d", n)
	}
}

// spyReporter counts the calls a Hub makes on a real reporter, after they
// return.
type spyReporter struct {
	Reporter
	reports, releases atomic.Int32
}

func (s *spyReporter) Report(ctx context.Context, st Status, seq uint64) error {
	defer s.reports.Add(1)
	return s.Reporter.Report(ctx, st, seq)
}

func (s *spyReporter) Release(ctx context.Context, seq uint64) error {
	defer s.releases.Add(1)
	return s.Reporter.Release(ctx, seq)
}

// flagCtx is a context whose Err turns Canceled the moment flag is set while
// its Done never closes: a line already on the wire completes normally, so
// only a check before the next line can see the cancellation.
type flagCtx struct {
	context.Context
	flag *atomic.Bool
}

func (c flagCtx) Err() error {
	if c.flag.Load() {
		return context.Canceled
	}
	return nil
}

// TestRoostChecksCtxBeforeEachLine is the Reporter contract (hub.go): ctx is
// checked before each wire line, so a Close landing between the claim and the
// state line stops the state line from ever being written. Negative control
// checked: removing the check before the state line let the working line
// through, then was reverted.
func TestRoostChecksCtxBeforeEachLine(t *testing.T) {
	var flag atomic.Bool
	srv := newFakeRoost(t, func(_ *fakeRoost, _ net.Conn, _ obj, _ <-chan struct{}) bool {
		flag.Store(true) // before the claim's reply is written
		return false
	})
	r := newTestRoost(srv.socket)

	err := r.Report(flagCtx{context.Background(), &flag}, Status{Kind: Working, Detail: DetailPrompt, SessionID: sessA}, 1)
	assertRoostLines(t, srv.fakeUDS, claimWant(1, sessA, obj{"version": version.Version}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// TestRoostReplyErrors: anything but an ok reply to this request is an error,
// never an acknowledged claim — so each is followed by a fresh claim. Negative
// controls checked: treating ok:false as a success failed the first case;
// accepting a line without a trailing newline failed the EOF case; and taking
// accepted:true before checking for the tab failed the accepted-no-tab case,
// on its error and, with that check disabled, on the missing second claim. All
// were reverted.
func TestRoostReplyErrors(t *testing.T) {
	cases := []struct {
		name    string
		write   func(conn net.Conn, id any)
		wantErr string
	}{
		{"ok:false", func(conn net.Conn, id any) {
			writeLine(t, conn, obj{"id": id, "ok": false, "error": obj{"code": "unknown-field", "message": "unknown field `x`"}})
		}, "roost error unknown-field: unknown field `x`"},
		{"ok:false without an error", func(conn net.Conn, id any) {
			writeLine(t, conn, obj{"id": id, "ok": false})
		}, "ok:false with no error"},
		{"malformed line", func(conn net.Conn, _ any) {
			_, _ = conn.Write([]byte("this is not json\n"))
		}, "decode reply"},
		{"not an object", func(conn net.Conn, _ any) {
			_, _ = conn.Write([]byte("null\n"))
		}, "not a JSON object"},
		{"no ok field", func(conn net.Conn, id any) {
			writeLine(t, conn, obj{"id": id, "result": obj{"accepted": true}})
		}, "no ok field"},
		{"no accepted field", func(conn net.Conn, id any) {
			writeLine(t, conn, obj{"id": id, "ok": true, "result": obj{}})
		}, "no result.accepted"},
		{"not accepted, no tab", func(conn net.Conn, id any) {
			writeLine(t, conn, obj{"id": id, "ok": true, "result": obj{"accepted": false}})
		}, "no result.tab"},
		{"accepted, no tab", func(conn net.Conn, id any) {
			writeLine(t, conn, obj{"id": id, "ok": true, "result": obj{"accepted": true}})
		}, "no result.tab"},
		{"EOF without newline", func(conn net.Conn, id any) {
			_, _ = conn.Write([]byte(`{"id":"1","ok":true,"result":{"accepted":true,"tab":{}}}`))
			conn.Close()
		}, io.ErrUnexpectedEOF.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int32
			srv := newFakeRoost(t, func(_ *fakeRoost, conn net.Conn, req obj, _ <-chan struct{}) bool {
				if n.Add(1) > 1 {
					return false
				}
				tc.write(conn, req["id"])
				return true
			})
			r := newTestRoost(srv.socket)
			ctx := context.Background()
			ready := Status{Kind: Idle, Detail: DetailReady, SessionID: sessA}

			err := r.Report(ctx, ready, 1)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want an error containing %q, got %v", tc.wantErr, err)
			}
			mustNil(t, r.Report(ctx, ready, 2))
			md := obj{"version": version.Version}
			assertRoostLines(t, srv.fakeUDS, claimWant(1, sessA, md), claimWant(2, sessA, md))
		})
	}
}

// TestRoostSkipsEventFramesAndOtherIDs: unsolicited event frames are skipped
// (roost's own client, crates/roost-ipc/src/client.rs), even one that happens
// to carry this request's id, and so is a reply to some other id — even an
// error — until this request's own reply arrives. Negative controls checked:
// not skipping event frames failed with "no ok field", and not skipping other
// ids failed with the other id's roost error; both were reverted.
func TestRoostSkipsEventFramesAndOtherIDs(t *testing.T) {
	srv := newFakeRoost(t, func(f *fakeRoost, conn net.Conn, req obj, _ <-chan struct{}) bool {
		writeLine(t, conn, obj{"event": "agent_report_changed", "id": req["id"], "data": obj{"tab_id": roostTestTab}})
		writeLine(t, conn, obj{"event": "tab_state_changed", "data": obj{"tab_id": roostTestTab, "state": "running"}})
		writeLine(t, conn, obj{"id": "999", "ok": false, "error": obj{"code": "internal", "message": "not yours"}})
		writeLine(t, conn, f.apply(req))
		return true
	})
	r := newTestRoost(srv.socket)
	ctx := context.Background()

	mustNil(t, r.Report(ctx, Status{Kind: Idle, Detail: DetailReady, SessionID: sessA}, 1))
	mustNil(t, r.Report(ctx, Status{Kind: Working, Detail: DetailPrompt, SessionID: sessA}, 2))
	// The claim was acknowledged through the noise, so no second claim.
	assertRoostLines(t, srv.fakeUDS, claimWant(1, sessA, obj{"version": version.Version}), workingWant(2, sessA))
}

// TestRoostRejectsOversizedReply: a reply frame over roost's own 16 MiB cap is
// an error, not buffered. Negative control checked: raising the cap to 64 MiB
// left Report waiting out its deadline for a newline instead, then was
// reverted.
func TestRoostRejectsOversizedReply(t *testing.T) {
	srv := newFakeRoost(t, func(_ *fakeRoost, conn net.Conn, _ obj, stop <-chan struct{}) bool {
		// Well past the cap, with no newline: exactly one byte over would leave
		// a partial bufio chunk the reader waits on, as it should for a frame
		// still arriving.
		if _, err := conn.Write(bytes.Repeat([]byte("a"), 17<<20)); err == nil {
			<-stop
		}
		return true
	})
	r := newTestRoost(srv.socket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := r.Report(ctx, Status{Kind: Idle, Detail: DetailReady, SessionID: sessA}, 1)
	if !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("want an error wrapping errFrameTooLarge, got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the cap must trip, not the deadline: %v", err)
	}
}

// TestRoostAbsentSocketWarnsOnce: a socket nobody listens on is one Warn for
// the whole run, the release included. Negative control checked: dropping the
// Hub's warned-once check produced a line per failure, then was reverted.
func TestRoostAbsentSocketWarnsOnce(t *testing.T) {
	g := newRig(t, nil, newTestRoost(shortSocketPath(t))) // nothing listening

	g.h.Publish(Status{Kind: Working, Detail: DetailPrompt, SessionID: sessA})
	g.h.Publish(Status{Kind: Blocked, Message: "question", Detail: DetailQuestion, SessionID: sessA})
	waitFor(t, "the first failure to warn", func() bool { return len(g.warns.snapshot()) >= 1 })
	g.h.Close(context.Background())

	warns := g.warns.snapshot()
	if len(warns) != 1 || !strings.HasPrefix(warns[0], "host status: roost: claim: ") {
		t.Fatalf("want exactly one host status: roost: claim: warning, got %q", warns)
	}
}

// TestRoostHubReleaseIsLastUnderCloseRace: Close lands while the first claim
// is still waiting for its reply. The in-flight Report is cancelled and writes
// nothing more, and the release — for the session whose claim was attempted,
// acknowledged or not — is the last line. Negative control checked: carrying
// on past the failed claim on a fresh context wrote a working line before the
// release, then was reverted.
func TestRoostHubReleaseIsLastUnderCloseRace(t *testing.T) {
	entered := make(chan struct{})
	var first atomic.Bool
	srv := newFakeRoost(t, func(_ *fakeRoost, _ net.Conn, _ obj, stop <-chan struct{}) bool {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-stop // held; the client gives up once Close cancels its ctx
			return true
		}
		return false
	})
	g := newRig(t, nil, newTestRoost(srv.socket))

	g.h.Publish(Status{Kind: Working, Detail: DetailPrompt, SessionID: sessA, Model: "m"})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the claim")
	}
	g.h.Close(context.Background())

	params := roostParamsLog(t, srv.fakeUDS)
	if got, want := actionsOf(params), []string{"claim/sess-a", "release/sess-a"}; !slices.Equal(got, want) {
		t.Fatalf("actions\n got  %q\n want %q", got, want)
	}
	assertJSONEqual(t, params[1], releaseWant(0, sessA)["params"])
	assertRoostFraming(t, srv.fakeUDS)
	if w := g.warns.snapshot(); len(w) != 0 {
		// The held claim never landed, so the fake has no owner and drops the
		// release — which is recovery, not a failure.
		t.Fatalf("a send Close cancelled is not a failure: %q", w)
	}
}

// TestHubHerdrAndRoost runs both real reporters under one Hub against their own
// fake sockets: each host gets every status in its own vocabulary and its own
// release, and Close releases them in parallel — each fake holds its release
// until the other host's has arrived, so a Close that released one after the
// other would time the first out and warn. Negative control checked:
// serialising the Hub's releases behind one mutex timed the first release out
// with a warning, then was reverted.
func TestHubHerdrAndRoost(t *testing.T) {
	var mu sync.Mutex
	released := 0
	bothReleased := make(chan struct{})
	barrier := func(stop <-chan struct{}) {
		mu.Lock()
		if released++; released == 2 {
			close(bothReleased)
		}
		mu.Unlock()
		select {
		case <-bothReleased:
		case <-stop:
		}
	}

	herdr := newFakeUDS(t, func(conn net.Conn, _ int, req map[string]any, stop <-chan struct{}) {
		if req["method"] == "pane.release_agent" {
			barrier(stop)
		}
		writeLine(t, conn, okReply(req["id"].(string)))
	})
	roost := newFakeRoost(t, func(_ *fakeRoost, _ net.Conn, req obj, stop <-chan struct{}) bool {
		if req["params"].(map[string]any)["ownership_action"] == "release" {
			barrier(stop)
		}
		return false
	})
	const closeBudget = 5 * time.Second
	g := newRig(t, func(o *HubOptions) {
		o.SendTimeout = 2 * time.Second
		o.CloseTimeout = closeBudget
	}, &Herdr{socket: herdr.socket, pane: "w1:p1"}, newTestRoost(roost.socket))

	lines := func(what string, h, r int) {
		t.Helper()
		waitFor(t, what, func() bool { return len(herdr.requests()) >= h && len(roost.requests()) >= r })
	}
	fireIdle := func() {
		t.Helper()
		timers := g.timers.all()
		for _, tm := range timers[len(timers)-2:] {
			tm.fire()
		}
	}
	base := Status{SessionID: sessA, Provider: "cursor", Model: "m"}
	with := func(k Kind, msg, detail string) Status {
		s := base
		s.Kind, s.Message, s.Detail = k, msg, detail
		return s
	}

	g.h.Publish(with(Idle, "", DetailReady))
	fireIdle()
	lines("the ready idle", 2, 1)
	g.h.Publish(with(Working, "", DetailPrompt))
	lines("working", 3, 2)
	g.h.Publish(with(Blocked, "permission Shell", DetailPermissionPrompt))
	lines("blocked", 4, 3)
	g.h.Publish(with(Idle, "", DetailStop))
	fireIdle()
	lines("the turn's idle", 5, 4)

	start := time.Now()
	g.h.Close(context.Background())
	if elapsed := time.Since(start); elapsed >= closeBudget {
		t.Fatalf("Close took %v, over its %v budget", elapsed, closeBudget)
	}
	if w := g.warns.snapshot(); len(w) != 0 {
		t.Fatalf("no send may fail: %q", w)
	}

	if got, want := requestMethods(t, herdr), []string{
		"pane.report_agent", "pane.report_metadata", "pane.report_agent", "pane.report_agent",
		"pane.report_agent", "pane.report_metadata", "pane.release_agent",
	}; !slices.Equal(got, want) {
		t.Fatalf("herdr methods\n got  %v\n want %v", got, want)
	}
	var herdrStates []string
	for _, r := range herdr.requests() {
		m := decodeJSONObject(t, r.line)
		if p := m["params"].(map[string]any); m["method"] == "pane.report_agent" {
			herdrStates = append(herdrStates, p["state"].(string))
		}
	}
	if want := []string{"idle", "working", "blocked", "idle"}; !slices.Equal(herdrStates, want) {
		t.Fatalf("herdr states %v, want %v", herdrStates, want)
	}

	params := roostParamsLog(t, roost.fakeUDS)
	var roostLifecycles []string
	for _, p := range params {
		roostLifecycles = append(roostLifecycles, p["ownership_action"].(string)+"/"+p["lifecycle"].(string))
	}
	if want := []string{"claim/inactive", "preserve/working", "preserve/waiting", "preserve/finished", "release/inactive"}; !slices.Equal(roostLifecycles, want) {
		t.Fatalf("roost lines %v, want %v", roostLifecycles, want)
	}
	assertRoostFraming(t, roost.fakeUDS)
}
