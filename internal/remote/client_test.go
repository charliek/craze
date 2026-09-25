package remote_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// withMember is raw — a JSON object — with member name set to value at the
// object path names (each a member of the one before).
func withMember(t *testing.T, raw json.RawMessage, value string, path ...string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(path) == 1 {
		m[path[0]] = json.RawMessage(value)
	} else {
		m[path[0]] = withMember(t, m[path[0]], value, path[1:]...)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestUnknownNotificationsAndFieldsAreIgnored (§3.2, tolerant inbound):
// unknown fields in a result (hello's, the attach reply's, session.state's)
// and in a notification's params, an unknown notification method — for the
// stream's own subscription too — and an unknown event kind are ignored, never
// an error: the client goes on, and the stream hands every event up, the
// unknown kind's body verbatim.
func TestUnknownNotificationsAndFieldsAreIgnored(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	const future = `{"kind":"from a later host","n":[1,2]}`
	var sub string
	tp.setRewriteIn(func(l wireLine) [][]byte {
		switch {
		case l.resp != nil && l.resp.Error == nil && (l.method == protocol.MethodHello ||
			l.method == protocol.MethodSessionAttach || l.method == protocol.MethodSessionState):
			if l.method == protocol.MethodSessionAttach {
				var r protocol.AttachResult
				_ = json.Unmarshal(l.resp.Result, &r)
				sub = r.Subscription
			}
			return [][]byte{withMember(t, l.raw, future, "result", "futureMember")}
		case l.resp == nil && l.method == protocol.NotifyEvent:
			note := `{"jsonrpc":"2.0","method":"future.notice","params":{"subscription":"` + sub + `","seq":999}}`
			ev := withMember(t, l.raw, future, "params", "futureParam")
			var p protocol.EventParams
			_ = json.Unmarshal(l.params, &p)
			if strings.Contains(string(p.Event), "an unknown kind") {
				ev = withMember(t, ev, `{"type":"future_kind","text":"an unknown kind"}`, "params", "event")
			}
			return [][]byte{[]byte(note), ev}
		}
		return nil
	})
	c := dialClient(t, h.path, tp, remote.Options{})
	s := attach(t, c, remote.AttachOptions{})
	after := nextKind(t, s, remote.KindAttached).Reply.After
	nextKind(t, s, remote.KindSynchronized)
	h.text("known")
	h.text("an unknown kind")
	h.text("known again")
	items := until(t, s, isText(t, "known again"))
	contiguous(t, after.Seq+1, items)
	var bodies []string
	for _, it := range items {
		if it.Kind == remote.KindEvent {
			bodies = append(bodies, string(it.Body))
		}
	}
	if !slices.Contains(bodies, `{"type":"future_kind","text":"an unknown kind"}`) {
		t.Fatalf("the unknown kind's body was not handed up verbatim: %q", bodies)
	}
	var st protocol.StateResult
	if err := c.Call(tctx(t), protocol.MethodSessionState, protocol.StateParams{SessionID: h.sid()}, &st); err != nil || st.Activity != protocol.ActivityIdle {
		t.Fatalf("session.state with an unknown field: %+v, %v", st, err)
	}
}

// TestAHelloWithNoCommonVersionSurfacesSupported (§3.2, §3.3): hello says
// protocols [1]; a host that shares none refuses bad_request, reason
// protocol_version, and Dial surfaces the versions it speaks.
func TestAHelloWithNoCommonVersionSurfacesSupported(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	tp.setRewriteOut(func(l wireLine) []byte {
		var p protocol.HelloParams
		if l.method != protocol.MethodHello || json.Unmarshal(l.params, &p) != nil || !slices.Equal(p.Protocols, []int{1}) {
			t.Errorf("the client's hello: %s", l.raw)
			return nil
		}
		return append(withMember(t, l.raw, `[7]`, "params", "protocols"), '\n')
	})
	_, err := tryDial(t, h.path, tp, remote.Options{})
	var ve *remote.VersionError
	if !errors.As(err, &ve) || !slices.Equal(ve.Supported, []int{1}) {
		t.Fatalf("Dial: %v, want a VersionError naming [1]", err)
	}
	if ve.Err.Code != protocol.CodeBadRequest || ve.Err.Reason != protocol.ReasonProtocolVersion {
		t.Fatalf("the refusal: %+v", ve.Err)
	}
}

// TestAHubIsAcceptedOnlyForTheHubHop (X5, §3.3): a hub's hello — no client
// id, no token — is accepted only on the hub hop (Options.Connect); a hub
// where a session's host belongs, and a host where the hub hop expects a hub,
// are ErrEndpoint.
func TestAHubIsAcceptedOnlyForTheHubHop(t *testing.T) {
	h := newHost(t)
	hb := newHub(t, h.path)
	tp := newTap(t)
	if _, err := tryDial(t, hb.path, tp, remote.Options{}); !errors.Is(err, remote.ErrEndpoint) {
		t.Fatalf("a hub dialled as a host: %v", err)
	}
	if _, err := tryDial(t, h.path, tp, remote.Options{Connect: h.sid()}); !errors.Is(err, remote.ErrEndpoint) {
		t.Fatalf("a host dialled as a hub: %v", err)
	}
}

// TestRetryByCodeResendsTheSameID (§3.6, SF-12): a command refused with a
// code protocol.Retry allows — not_accepting while the session starts — comes
// back as the refusal without CommandOptions.Retry; with it the SAME command
// id is resent until the start has run, and the host runs it once; a code
// Retry does not allow (unknown_row) is never resent, Retry or not.
func TestRetryByCodeResendsTheSameID(t *testing.T) {
	h := newHost(t, withoutStart())
	tp := newTap(t)
	c := dialClient(t, h.path, tp, remote.Options{})
	_, err := c.Command(tctx(t), protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: h.sid(), Text: "early", Mode: protocol.PromptQueue}, nil, remote.CommandOptions{})
	var e *remote.Error
	if !errors.As(err, &e) || e.Code != protocol.CodeNotAccepting {
		t.Fatalf("a prompt while starting: %v", err)
	}
	if n := len(tp.sent(protocol.MethodSessionPrompt)); n != 1 {
		t.Fatalf("resent without Retry: %d sends", n)
	}

	type answer struct {
		id  string
		err error
	}
	done := make(chan answer, 1)
	go func() {
		id, err := c.Command(tctx(t), protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: h.sid(), Text: "when up", Mode: protocol.PromptQueue},
			nil, remote.CommandOptions{Retry: true})
		done <- answer{id, err}
	}()
	tp.await(t, "two refused attempts", func() bool { return len(tp.sent(protocol.MethodSessionPrompt)) >= 3 })
	if err := h.eng.Start(tctx(t)); err != nil {
		t.Fatal(err)
	}
	a := recv(t, done)
	if a.err != nil {
		t.Fatalf("the retried prompt: %v", a.err)
	}
	for _, l := range tp.sent(protocol.MethodSessionPrompt)[1:] {
		if commandID(t, l) != a.id {
			t.Fatalf("a retry under another id: %s", l.raw)
		}
	}
	if got := h.stub.Prompts(); !slices.Equal(got, []string{"when up"}) {
		t.Fatalf("the host ran %q", got)
	}

	_, err = c.Command(tctx(t), protocol.MethodQueueRemove, protocol.QueueRemoveParams{SessionID: h.sid(), RowID: "no-such-row"}, nil, remote.CommandOptions{Retry: true})
	if !errors.As(err, &e) || e.Code != protocol.CodeUnknownRow || protocol.Retry(e.Code) {
		t.Fatalf("a remove of no row: %v", err)
	}
	if n := len(tp.sent(protocol.MethodQueueRemove)); n != 1 {
		t.Fatalf("a code Retry does not allow was resent: %d sends", n)
	}
}

// TestARefusalCarriesItsCodeReasonMessageAndCause (§3.2): an error reply is
// an *Error carrying the JSON-RPC integer, data.code, data.reason, the
// host's message verbatim, and data.cause; params the host refuses are
// -32602 with their reason; and the client keeps commands and reads apart.
func TestARefusalCarriesItsCodeReasonMessageAndCause(t *testing.T) {
	h := newHost(t, withoutStart())
	tp := newTap(t)
	c := dialClient(t, h.path, tp, remote.Options{})
	h.eng.Started(errors.New("the agent would not start"))
	_, err := c.Attach(tctx(t), remote.AttachOptions{})
	var e *remote.Error
	switch {
	case !errors.As(err, &e):
		t.Fatalf("an attach after a failed start: %v", err)
	case e.RPC != protocol.RPCRefused || e.Code != protocol.CodeNotAccepting || e.Reason != protocol.ReasonStartFailed:
		t.Fatalf("the refusal: %+v", e)
	case e.Cause != "the agent would not start" || !strings.Contains(e.Error(), "the agent would not start"):
		t.Fatalf("the cause and message: %+v", e)
	}
	// The stream it would have been is gone: the client may attach again.
	if _, err := c.Attach(tctx(t), remote.AttachOptions{}); errors.Is(err, remote.ErrAlreadyAttached) {
		t.Fatal("a refused attach left its stream behind")
	}

	// Params the host refuses: the tap adds a member session.state does not
	// define to one request on its way out.
	var once sync.Once
	tp.setRewriteOut(func(l wireLine) []byte {
		var out []byte
		if l.method == protocol.MethodSessionState {
			once.Do(func() { out = append(withMember(t, l.raw, `1`, "params", "later"), '\n') })
		}
		return out
	})
	err = c.Call(tctx(t), protocol.MethodSessionState, protocol.StateParams{SessionID: h.sid()}, nil)
	if !errors.As(err, &e) || e.RPC != protocol.RPCInvalidParams || e.Code != protocol.CodeBadRequest || e.Reason != protocol.ReasonUnknownField {
		t.Fatalf("an unknown params field: %v", err)
	}
	err = c.Call(tctx(t), protocol.MethodSessionState, protocol.StateParams{SessionID: "no-such-session"}, nil)
	if !errors.As(err, &e) || e.Code != protocol.CodeUnknownSession {
		t.Fatalf("an unknown session: %v", err)
	}
	if err := c.Call(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "x"}, nil); err == nil || errors.As(err, &e) {
		t.Fatalf("Call took a command: %v", err)
	}
	if _, err := c.Command(tctx(t), protocol.MethodSessionState, protocol.StateParams{SessionID: h.sid()}, nil, remote.CommandOptions{}); err == nil || errors.As(err, &e) {
		t.Fatalf("Command took a read: %v", err)
	}
	if n := len(tp.sent(protocol.MethodQueueAdd)) + len(tp.sent(protocol.MethodSessionState)); n != 2 {
		t.Fatalf("%d requests: a misrouted call reached the host", n)
	}
}

// TestACommandWaitingForAConnectionIsSentOnce (§3.14): a command issued while
// the client reconnects is held — never sent on a lost connection — and sent,
// as a first attempt, once the new connection is adopted; one issued while the
// redials run out resolves ErrNotRun: it never left the client.
func TestACommandWaitingForAConnectionIsSentOnce(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	c := dialClient(t, h.path, tp, remote.Options{})
	held := tp.holdDials()
	tp.kill()
	await(t, held, "the redial")
	done := make(chan error, 1)
	go func() {
		_, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "held"}, nil, remote.CommandOptions{})
		done <- err
	}()
	waitFor(t, "the command to wait", func() bool { return c.Waiting() == 1 })
	if n := len(tp.sent(protocol.MethodQueueAdd)); n != 0 {
		t.Fatalf("sent while disconnected: %d", n)
	}
	tp.releaseDials()
	if err := recv(t, done); err != nil {
		t.Fatalf("the held command: %v", err)
	}
	if adds := tp.sent(protocol.MethodQueueAdd); len(adds) != 1 || adds[0].conn != 1 || queued(h, "held") != 1 {
		t.Fatalf("the held command: %d sends", len(adds))
	}

	held = tp.holdDials()
	tp.kill()
	await(t, held, "the redial")
	go func() {
		_, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "never"}, nil, remote.CommandOptions{})
		done <- err
	}()
	waitFor(t, "the command to wait", func() bool { return c.Waiting() == 1 })
	tp.failDials(errors.New("the host is gone"))
	tp.releaseDials()
	if err := recv(t, done); !errors.Is(err, remote.ErrNotRun) || errors.Is(err, remote.ErrOutcomeUnknown) {
		t.Fatalf("a command the client never sent: %v", err)
	}
	if n := len(tp.sent(protocol.MethodQueueAdd)); n != 1 {
		t.Fatalf("%d sends", n)
	}
}

// TestCloseResolvesWhatIsWaiting: Close resolves a command that may have run
// ErrOutcomeUnknown (disconnected), hands the stream an Error item carrying
// ErrClosed, and everything after it is ErrClosed.
func TestCloseResolvesWhatIsWaiting(t *testing.T) {
	h := newHost(t)
	held, release := h.stub.HoldNextSet()
	t.Cleanup(release)
	tp := newTap(t)
	c := dialClient(t, h.path, tp, remote.Options{})
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	done := make(chan error, 1)
	go func() {
		_, err := c.Command(tctx(t), protocol.MethodSessionSet, protocol.SetParams{SessionID: h.sid(),
			Setting: protocol.Setting{Kind: protocol.SettingModel, Value: "fast"}}, nil, remote.CommandOptions{})
		done <- err
	}()
	await(t, held, "the Set to park")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	outcomeUnknown(t, recv(t, done), protocol.ReasonDisconnected)
	if last := until(t, s, ofKind(remote.KindError)); !errors.Is(last[len(last)-1].Err, remote.ErrClosed) {
		t.Fatalf("the stream's last item: %s", describe(last[len(last)-1]))
	}
	if err := c.Call(tctx(t), protocol.MethodSessionState, protocol.StateParams{SessionID: h.sid()}, nil); !errors.Is(err, remote.ErrClosed) {
		t.Fatalf("a call after Close: %v", err)
	}
	if _, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "late"}, nil, remote.CommandOptions{}); !errors.Is(err, remote.ErrClosed) {
		t.Fatalf("a command after Close: %v", err)
	}
	if n := tp.dialCount(); n != 1 {
		t.Fatalf("%d dials: a closed client redialled", n)
	}
}
