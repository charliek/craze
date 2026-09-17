package host

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// newOKServer answers every request with herdr's ok reply, id echoed.
func newOKServer(t *testing.T) *fakeUDS {
	t.Helper()
	return newFakeUDS(t, func(conn net.Conn, _ int, req map[string]any, _ <-chan struct{}) {
		id, _ := req["id"].(string)
		writeLine(t, conn, okReply(id))
	})
}

// requestMethods decodes every recorded request and returns its "method".
func requestMethods(t *testing.T, srv *fakeUDS) []string {
	t.Helper()
	var out []string
	for _, r := range srv.requests() {
		m := decodeJSONObject(t, r.line)
		method, _ := m["method"].(string)
		out = append(out, method)
	}
	return out
}

// TestNewHerdrFromEnv: the documented gate (plan 015 §3.3) is all three of
// HERDR_ENV == "1", HERDR_SOCKET_PATH and HERDR_PANE_ID non-empty. Negative
// control checked: temporarily changing the full-set assertion to `ok ==
// false` failed as expected, then was reverted.
func TestNewHerdrFromEnv(t *testing.T) {
	full := map[string]string{
		"HERDR_ENV":         "1",
		"HERDR_SOCKET_PATH": "/tmp/herdr.sock",
		"HERDR_PANE_ID":     "w1:p1",
	}
	getenvOf := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	if h, ok := NewHerdrFromEnv(getenvOf(full)); !ok || h == nil {
		t.Fatalf("all three set: want active, got ok=%v h=%v", ok, h)
	}

	for _, missing := range []string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_PANE_ID"} {
		m := maps.Clone(full)
		delete(m, missing)
		if h, ok := NewHerdrFromEnv(getenvOf(m)); ok || h != nil {
			t.Fatalf("%s missing: want inactive, got ok=%v h=%v", missing, ok, h)
		}
	}

	for _, v := range []string{"true", "0", "", "2", " 1"} {
		m := maps.Clone(full)
		m["HERDR_ENV"] = v
		if h, ok := NewHerdrFromEnv(getenvOf(m)); ok || h != nil {
			t.Fatalf("HERDR_ENV=%q: want inactive, got ok=%v h=%v", v, ok, h)
		}
	}
}

// TestHerdrOrdinarySequence drives the reporter directly (no Hub) through
// Idle ready -> Working -> Idle stop -> Release, and checks the whole wire
// sequence and every object semantically. Negative control checked:
// swapping the want seq for the metadata-nulls line from 4 to 3 failed the
// comparison as expected, then was reverted.
func TestHerdrOrdinarySequence(t *testing.T) {
	srv := newOKServer(t)
	h := &Herdr{socket: srv.socket, pane: "w1:p1"}
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(h.Report(ctx, Status{Kind: Idle, Provider: "cursor", Model: "gpt-5"}, 1))
	must(h.Report(ctx, Status{Kind: Working, Provider: "cursor", Model: "gpt-5"}, 2))
	must(h.Report(ctx, Status{Kind: Idle, Provider: "cursor", Model: "gpt-5"}, 3))
	must(h.Release(ctx, 4))

	reqs := srv.requests()
	wantMethods := []string{
		"pane.report_agent", "pane.report_metadata",
		"pane.report_agent",
		"pane.report_agent", "pane.report_metadata",
		"pane.release_agent",
	}
	if len(reqs) != len(wantMethods) {
		t.Fatalf("got %d lines, want %d: %v", len(reqs), len(wantMethods), requestMethods(t, srv))
	}
	for i, r := range reqs {
		if !strings.HasSuffix(string(r.line), "\n") {
			t.Fatalf("line %d does not end in a newline: %q", i, r.line)
		}
	}

	decoded := make([]map[string]any, len(reqs))
	for i, r := range reqs {
		decoded[i] = decodeJSONObject(t, r.line)
		if decoded[i]["method"] != wantMethods[i] {
			t.Fatalf("line %d method %v, want %v", i, decoded[i]["method"], wantMethods[i])
		}
		if _, ok := decoded[i]["id"].(string); !ok {
			t.Fatalf("line %d id is not a JSON string: %v", i, decoded[i]["id"])
		}
	}

	assertJSONEqual(t, decoded[0], map[string]any{
		"id": "craze:1", "method": "pane.report_agent",
		"params": map[string]any{
			"pane_id": "w1:p1", "source": "custom:craze", "agent": "craze",
			"state": "idle", "seq": 1,
		},
	})
	assertJSONEqual(t, decoded[1], map[string]any{
		"id": "craze:1:metadata", "method": "pane.report_metadata",
		"params": map[string]any{
			"pane_id": "w1:p1", "source": "custom:craze", "agent": "craze",
			"tokens": map[string]any{"provider": "cursor", "model": "gpt-5"},
		},
	})
	assertJSONEqual(t, decoded[2], map[string]any{
		"id": "craze:2", "method": "pane.report_agent",
		"params": map[string]any{
			"pane_id": "w1:p1", "source": "custom:craze", "agent": "craze",
			"state": "working", "seq": 2,
		},
	})
	assertJSONEqual(t, decoded[3], map[string]any{
		"id": "craze:3", "method": "pane.report_agent",
		"params": map[string]any{
			"pane_id": "w1:p1", "source": "custom:craze", "agent": "craze",
			"state": "idle", "seq": 3,
		},
	})
	assertJSONEqual(t, decoded[4], map[string]any{
		"id": "craze:4:metadata", "method": "pane.report_metadata",
		"params": map[string]any{
			"pane_id": "w1:p1", "source": "custom:craze", "agent": "craze",
			"tokens": map[string]any{"provider": nil, "model": nil},
		},
	})
	assertJSONEqual(t, decoded[5], map[string]any{
		"id": "craze:4", "method": "pane.release_agent",
		"params": map[string]any{
			"pane_id": "w1:p1", "source": "custom:craze", "agent": "craze", "seq": 4,
		},
	})
}

// TestHerdrBlockedAndFailedCarryMessage: both map to herdr's "blocked" state
// and carry the message; Idle/Working never do (already pinned by the
// ordinary-sequence test above, which has no "message" key anywhere).
// Negative control checked: dropping the "message" field from the want
// object failed the whole-object comparison as expected (an extra field
// fails, not just a missing one), then was reverted.
func TestHerdrBlockedAndFailedCarryMessage(t *testing.T) {
	srv := newOKServer(t)
	h := &Herdr{socket: srv.socket, pane: "w1:p1"}
	ctx := context.Background()

	if err := h.Report(ctx, Status{Kind: Blocked, Message: "need permission"}, 1); err != nil {
		t.Fatalf("blocked: %v", err)
	}
	if err := h.Report(ctx, Status{Kind: Failed, Message: "boom"}, 2); err != nil {
		t.Fatalf("failed: %v", err)
	}

	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 report_agent lines (provider/model always empty, no metadata), got %v", requestMethods(t, srv))
	}
	assertJSONEqual(t, decodeJSONObject(t, reqs[0].line), map[string]any{
		"id": "craze:1", "method": "pane.report_agent",
		"params": map[string]any{
			"pane_id": "w1:p1", "source": "custom:craze", "agent": "craze",
			"state": "blocked", "message": "need permission", "seq": 1,
		},
	})
	assertJSONEqual(t, decodeJSONObject(t, reqs[1].line), map[string]any{
		"id": "craze:2", "method": "pane.report_agent",
		"params": map[string]any{
			"pane_id": "w1:p1", "source": "custom:craze", "agent": "craze",
			"state": "blocked", "message": "boom", "seq": 2,
		},
	})
}

// TestHerdrBlockedAndFailedEmptyMessageStillPresent: the pinned shape is
// "message" present for Blocked and Failed even when Message is "", and
// absent for Working/Idle regardless of Message. Using a *string keyed off
// Kind (not omitempty on the string value) is what makes this so. Negative
// control checked: reverting Message to a plain `string` with
// `json:"message,omitempty"` made the whole-object comparison fail because
// the "message" key was missing from both lines, then the fix was restored.
func TestHerdrBlockedAndFailedEmptyMessageStillPresent(t *testing.T) {
	srv := newOKServer(t)
	h := &Herdr{socket: srv.socket, pane: "w1:p1"}
	ctx := context.Background()

	if err := h.Report(ctx, Status{Kind: Blocked, Message: ""}, 1); err != nil {
		t.Fatalf("blocked: %v", err)
	}
	if err := h.Report(ctx, Status{Kind: Failed, Message: ""}, 2); err != nil {
		t.Fatalf("failed: %v", err)
	}

	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 report_agent lines, got %v", requestMethods(t, srv))
	}
	assertJSONEqual(t, decodeJSONObject(t, reqs[0].line), map[string]any{
		"id": "craze:1", "method": "pane.report_agent",
		"params": map[string]any{
			"pane_id": "w1:p1", "source": "custom:craze", "agent": "craze",
			"state": "blocked", "message": "", "seq": 1,
		},
	})
	assertJSONEqual(t, decodeJSONObject(t, reqs[1].line), map[string]any{
		"id": "craze:2", "method": "pane.report_agent",
		"params": map[string]any{
			"pane_id": "w1:p1", "source": "custom:craze", "agent": "craze",
			"state": "blocked", "message": "", "seq": 2,
		},
	})
}

// TestHerdrMetadataOnlyOnChange: metadata is sent only when Provider or
// Model differs from what was last sent successfully. Negative control
// checked: changing want to expect metadata on every Report failed as
// expected (extra pane.report_metadata entries), then was reverted.
func TestHerdrMetadataOnlyOnChange(t *testing.T) {
	srv := newOKServer(t)
	h := &Herdr{socket: srv.socket, pane: "w1:p1"}
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(h.Report(ctx, Status{Kind: Working, Provider: "cursor", Model: "gpt-5"}, 1))   // sends
	must(h.Report(ctx, Status{Kind: Idle, Provider: "cursor", Model: "gpt-5"}, 2))      // unchanged: none
	must(h.Report(ctx, Status{Kind: Working, Provider: "cursor", Model: "gpt-5.1"}, 3)) // model changed: sends

	want := []string{
		"pane.report_agent", "pane.report_metadata",
		"pane.report_agent",
		"pane.report_agent", "pane.report_metadata",
	}
	if got := requestMethods(t, srv); !slices.Equal(got, want) {
		t.Fatalf("methods\n got  %v\n want %v", got, want)
	}
}

// TestHerdrNoMetadataWhenAlwaysEmpty: provider and model empty the whole
// time means no report_metadata ever, including no nulls at Release.
// Negative control checked: asserting the opposite (want a nulls line at
// Release) failed as expected, then was reverted.
func TestHerdrNoMetadataWhenAlwaysEmpty(t *testing.T) {
	srv := newOKServer(t)
	h := &Herdr{socket: srv.socket, pane: "w1:p1"}
	ctx := context.Background()

	if err := h.Report(ctx, Status{Kind: Working}, 1); err != nil {
		t.Fatal(err)
	}
	if err := h.Report(ctx, Status{Kind: Idle}, 2); err != nil {
		t.Fatal(err)
	}
	if err := h.Release(ctx, 3); err != nil {
		t.Fatal(err)
	}

	want := []string{"pane.report_agent", "pane.report_agent", "pane.release_agent"}
	if got := requestMethods(t, srv); !slices.Equal(got, want) {
		t.Fatalf("methods\n got  %v\n want %v", got, want)
	}
}

// TestHerdrMetadataTimeoutDoesNotFailStateLine: a metadata send that never
// gets a reply fails Report, but only after the state line already landed,
// and the next Report retries the metadata (plan 015 §3.3's "a metadata
// failure never undoes the state line"). Negative control checked: asserting
// err == nil failed as expected (the metadata timeout must surface), then
// was reverted.
func TestHerdrMetadataTimeoutDoesNotFailStateLine(t *testing.T) {
	srv := newFakeUDS(t, func(conn net.Conn, _ int, req map[string]any, stop <-chan struct{}) {
		method, _ := req["method"].(string)
		id, _ := req["id"].(string)
		if method == "pane.report_metadata" {
			<-stop // never reply
			return
		}
		writeLine(t, conn, okReply(id))
	})
	h := &Herdr{socket: srv.socket, pane: "w1:p1"}

	err := h.Report(context.Background(), Status{Kind: Idle, Provider: "cursor", Model: "m1"}, 1)
	if err == nil {
		t.Fatal("want an error: metadata timed out")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want an error wrapping context.DeadlineExceeded, got %v", err)
	}
	if got := requestMethods(t, srv); !slices.Equal(got, []string{"pane.report_agent", "pane.report_metadata"}) {
		t.Fatalf("methods after the first Report: %v", got)
	}

	// The next Report still sends its own state line, and retries metadata
	// since lastProvider/lastModel were never updated (the send never got an
	// ok reply).
	err = h.Report(context.Background(), Status{Kind: Working, Provider: "cursor", Model: "m1"}, 2)
	if err == nil {
		t.Fatal("want an error: metadata still times out")
	}
	want := []string{
		"pane.report_agent", "pane.report_metadata",
		"pane.report_agent", "pane.report_metadata",
	}
	if got := requestMethods(t, srv); !slices.Equal(got, want) {
		t.Fatalf("methods\n got  %v\n want %v", got, want)
	}
}

// TestHerdrErrorReply: a reply with an "error" object and no "result" is an
// error whose text includes the code. Negative control checked: asserting
// the error text must NOT contain the code failed as expected, then was
// reverted.
func TestHerdrErrorReply(t *testing.T) {
	srv := newFakeUDS(t, func(conn net.Conn, _ int, req map[string]any, _ <-chan struct{}) {
		id, _ := req["id"].(string)
		writeLine(t, conn, errReply(id, -32600, "bad state"))
	})
	h := &Herdr{socket: srv.socket, pane: "w1:p1"}

	err := h.Report(context.Background(), Status{Kind: Working}, 1)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "-32600") || !strings.Contains(err.Error(), "bad state") {
		t.Fatalf("error %q must include the code and message", err)
	}
}

// TestHerdrReplyMismatchedID: a reply for a different id must never be
// accepted as this call's answer, even though its shape is an ordinary ok
// result — otherwise a stray or delayed reply on the wire could silently
// acknowledge the wrong request. Negative control checked: dropping the id
// check in Herdr.call (comparing only reply.Error/reply.Result, as before
// this fix) made the test fail because err was nil, then the check was
// restored.
func TestHerdrReplyMismatchedID(t *testing.T) {
	srv := newFakeUDS(t, func(conn net.Conn, _ int, _ map[string]any, _ <-chan struct{}) {
		writeLine(t, conn, okReply("some-other-id"))
	})
	h := &Herdr{socket: srv.socket, pane: "w1:p1"}

	err := h.Report(context.Background(), Status{Kind: Working}, 1)
	if err == nil {
		t.Fatal("want an error: the reply id does not match the request id")
	}
	if !strings.Contains(err.Error(), "some-other-id") {
		t.Fatalf("error %q should name the mismatched id", err)
	}
}

// TestHerdrReplyNonOkResultType: a reply whose result exists but is not
// {"type":"ok"} is an error, not a silent success — herdr's own
// ResponseResult::Ok{} always serialises with type "ok", so anything else is
// unexpected. Negative control checked: accepting any non-nil result (the
// pre-fix behaviour) made the test fail because err was nil, then the fix
// was restored.
func TestHerdrReplyNonOkResultType(t *testing.T) {
	srv := newFakeUDS(t, func(conn net.Conn, _ int, req map[string]any, _ <-chan struct{}) {
		id, _ := req["id"].(string)
		writeLine(t, conn, map[string]any{"id": id, "result": map[string]any{"type": "nope"}})
	})
	h := &Herdr{socket: srv.socket, pane: "w1:p1"}

	err := h.Report(context.Background(), Status{Kind: Working}, 1)
	if err == nil {
		t.Fatal("want an error: result.type is not \"ok\"")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error %q should name the unexpected result type", err)
	}
}

// TestHerdrCloseWithoutNewline: a reply that closes the connection before a
// trailing newline is not accepted. Negative control checked: asserting
// err == nil failed as expected, then was reverted.
func TestHerdrCloseWithoutNewline(t *testing.T) {
	srv := newFakeUDS(t, func(conn net.Conn, _ int, req map[string]any, _ <-chan struct{}) {
		id, _ := req["id"].(string)
		b, err := json.Marshal(okReply(id))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := conn.Write(b); err != nil {
			t.Logf("write: %v", err)
		}
		conn.Close() // no trailing '\n': close now, not after another (never-coming) request line
	})
	h := &Herdr{socket: srv.socket, pane: "w1:p1"}

	if err := h.Report(context.Background(), Status{Kind: Working}, 1); err == nil {
		t.Fatal("want an error for a reply closed without a newline")
	}
}

// TestHerdrAcceptNeverReplyHonoursDeadline: a peer that accepts and never
// replies cannot hold Report past ctx's deadline. Negative control checked:
// raising the elapsed bound below the ctx timeout (e.g. 10ms) failed as
// expected (Report legitimately takes close to the 50ms deadline), then was
// reverted.
func TestHerdrAcceptNeverReplyHonoursDeadline(t *testing.T) {
	srv := newFakeUDS(t, func(conn net.Conn, _ int, _ map[string]any, stop <-chan struct{}) {
		<-stop // never reply
	})
	h := &Herdr{socket: srv.socket, pane: "w1:p1"}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := h.Report(ctx, Status{Kind: Working}, 1)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want an error wrapping context.DeadlineExceeded, got %v", err)
	}
	// Generous upper bound so a loaded machine cannot flake this; it still
	// catches a Report that ignored ctx entirely.
	if elapsed > 2*time.Second {
		t.Fatalf("Report took %v against a 50ms ctx", elapsed)
	}
}

// TestHerdrAbsentSocketWarnsOnce drives the reporter through a real Hub
// against a socket path nothing is listening on: Publish two different
// statuses, then Close, and expect exactly one Warn line (the Hub's
// warn-once-per-reporter rule), prefixed as hub.go's warn() always prefixes
// it. Negative control checked: asserting zero Warn lines failed as
// expected, then was reverted.
func TestHerdrAbsentSocketWarnsOnce(t *testing.T) {
	socket := shortSocketPath(t) // nothing listening

	h := &Herdr{socket: socket, pane: "w1:p1"}
	g := newRig(t, nil, h)

	g.h.Publish(st(Working, "w"))
	g.h.Publish(st(Blocked, "b"))
	waitFor(t, "the first failure to warn", func() bool { return len(g.warns.snapshot()) >= 1 })
	g.h.Close(context.Background())

	warns := g.warns.snapshot()
	if len(warns) != 1 {
		t.Fatalf("want exactly one Warn line, got %v", warns)
	}
	if !strings.HasPrefix(warns[0], "host status: herdr: ") {
		t.Fatalf("warn %q missing the host status: herdr: prefix", warns[0])
	}
}

// TestHerdrHubReleaseIsLastUnderCloseRace drives Herdr through a real Hub
// (Commit 1) and holds the reply to the second report_agent line so that
// Hub.Close races it: Close cancels the Hub's base context, which closes the
// held connection (ndjson's context.AfterFunc), so the in-flight Report
// returns without ever reaching its metadata step. The recorded wire order
// must still end release_agent, immediately preceded by the metadata nulls
// line, with no metadata-carrying-values line after the held state line.
// Negative control checked: asserting release_agent was NOT last failed as
// expected, then was reverted.
func TestHerdrHubReleaseIsLastUnderCloseRace(t *testing.T) {
	var mu sync.Mutex
	reportAgentCount := 0
	entered := make(chan struct{})

	srv := newFakeUDS(t, func(conn net.Conn, _ int, req map[string]any, stop <-chan struct{}) {
		method, _ := req["method"].(string)
		id, _ := req["id"].(string)
		if method == "pane.report_agent" {
			mu.Lock()
			reportAgentCount++
			n := reportAgentCount
			mu.Unlock()
			if n == 2 {
				close(entered)
				<-stop // held; the client gives up once Close cancels its ctx
				return
			}
		}
		writeLine(t, conn, okReply(id))
	})

	h := &Herdr{socket: srv.socket, pane: "w1:p2"}
	g := newRig(t, nil, h)

	g.h.Publish(Status{Kind: Working, Provider: "cursor", Model: "gpt-5"})
	waitFor(t, "the first report_agent and its metadata", func() bool { return len(srv.requests()) >= 2 })

	g.h.Publish(Status{Kind: Blocked, Message: "b"})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the second report_agent line")
	}

	g.h.Close(context.Background())

	reqs := srv.requests()
	methods := requestMethods(t, srv)
	if len(methods) == 0 || methods[len(methods)-1] != "pane.release_agent" {
		t.Fatalf("release_agent must be the last line: %v", methods)
	}
	if methods[len(methods)-2] != "pane.report_metadata" {
		t.Fatalf("the nulls metadata line must immediately precede release_agent: %v", methods)
	}

	var metaWithValues []int
	for i, r := range reqs {
		m := decodeJSONObject(t, r.line)
		if m["method"] != "pane.report_metadata" {
			continue
		}
		params, _ := m["params"].(map[string]any)
		tokens, _ := params["tokens"].(map[string]any)
		if tokens["provider"] != nil || tokens["model"] != nil {
			metaWithValues = append(metaWithValues, i)
		}
	}
	if len(metaWithValues) != 1 || metaWithValues[0] != 1 {
		t.Fatalf("exactly one metadata-with-values line, right after the first report_agent (index 1): got indices %v in %v", metaWithValues, methods)
	}
	if metaWithValues[0] > 1 {
		t.Fatalf("a metadata-with-values line appeared after the held state line: %v", methods)
	}
}
