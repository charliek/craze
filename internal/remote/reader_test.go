package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// The reader and stream rules of astra's r11 review (X21), each on a schedule
// the tap pins.

// withoutMember is raw — a JSON object — without the member at the object
// path names (each a member of the one before).
func withoutMember(t *testing.T, raw json.RawMessage, path ...string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(path) == 1 {
		delete(m, path[0])
	} else {
		m[path[0]] = withoutMember(t, m[path[0]], path[1:]...)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTheReaderReadsOnWhileACallerWrites (X21; astra r11 7): a caller's write
// holds the connection — a session.state whose write the tap holds, as a full
// socket would — while the host writes more events than the stream's bound,
// and then a command's reply. The stream falls behind on an event that does
// not fit, and its detach cannot be written yet; the reader, which never
// writes, posts it and reads on, so the command's reply arrives while the
// caller's write is still held, and the detach goes out once it is not. The
// caller then reads every event, in order.
func TestTheReaderReadsOnWhileACallerWrites(t *testing.T) {
	h := newHost(t)
	parked, release := h.stub.HoldNextSet()
	t.Cleanup(release)
	tp := newTap(t)
	const bound = 4 << 10
	c := dialClient(t, h.path, tp, remote.Options{StreamBytes: bound})
	// Registered after the client, so it runs before the client's Close.
	writing := make(chan struct{})
	t.Cleanup(func() { closeOnce(writing) })
	s := attach(t, c, remote.AttachOptions{})
	after := nextKind(t, s, remote.KindAttached).Reply.After
	nextKind(t, s, remote.KindSynchronized)
	done := make(chan error, 1)
	go func() {
		_, err := c.Command(tctx(t), protocol.MethodSessionSet, protocol.SetParams{SessionID: h.sid(),
			Setting: protocol.Setting{Kind: protocol.SettingModel, Value: "fast"}}, nil, remote.CommandOptions{})
		done <- err
	}()
	await(t, parked, "the Set to park")
	tp.setHoldOut(func(l wireLine) <-chan struct{} {
		if l.method == protocol.MethodSessionState {
			return writing
		}
		return nil
	})
	called := make(chan error, 1)
	go func() {
		called <- c.Call(tctx(t), protocol.MethodSessionState, protocol.StateParams{SessionID: h.sid()}, nil)
	}()
	tp.await(t, "the caller's write", func() bool { return len(tp.sent(protocol.MethodSessionState)) == 1 })
	for i := range 64 {
		h.text(fmt.Sprintf("%02d%s", i, strings.Repeat("w", 256)))
	}
	release()
	if err := recv(t, done); err != nil {
		t.Fatalf("the command: %v", err)
	}
	select {
	case err := <-called:
		t.Fatalf("the premise: the caller's write was not held (%v)", err)
	default:
	}
	if n := len(tp.sent(protocol.MethodSessionDetach)); n != 0 {
		t.Fatalf("the premise: %d detaches went out while the caller's write held the connection", n)
	}
	closeOnce(writing)
	if err := recv(t, called); err != nil {
		t.Fatalf("the caller's call: %v", err)
	}
	tp.await(t, "the fall's detach", func() bool { return len(tp.sent(protocol.MethodSessionDetach)) == 1 })
	h.text("marker")
	contiguous(t, after.Seq+1, until(t, s, isText(t, "marker")))
}

// TestAnOwedReadyComesWithItsRestore (X21; astra r11, further 1): a stream
// attached when: "now" before the start, its queue one byte — it holds an
// item only when empty — falls behind at once (synchronized finds no room)
// and detaches; the session then starts, its ready notification going to no
// attachment. Each time the caller has drained, the stream re-attaches with no
// cursor, and each reply is a snapshot that brings the news of readiness; the
// caller reads only once each reply has been handled whole (the review's
// schedule). The Ready owed goes into the queue with its Restore, as one step:
// the caller gets the Restore and then the Ready — never Restore after
// Restore until ErrReattachBound — and the stream settles at synchronized.
func TestAnOwedReadyComesWithItsRestore(t *testing.T) {
	h := newHost(t, withoutStart())
	tp := newTap(t)
	c := dialClient(t, h.path, tp, remote.Options{StreamBytes: 1})
	s := attach(t, c, remote.AttachOptions{When: protocol.WhenNow})
	detaches := func() int { return len(tp.sent(protocol.MethodSessionDetach)) }
	tp.await(t, "the fall's detach answered", func() bool {
		return len(tp.received(func(l wireLine) bool { return l.method == protocol.MethodSessionDetach && l.resp != nil })) == 1
	})
	if err := h.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []remote.Item
	for {
		before := detaches()
		it := next(t, s)
		got = append(got, it)
		if it.Kind == remote.KindError || it.Kind == remote.KindEnd {
			t.Fatalf("the stream stopped after %d items: %s", len(got), describe(it))
		}
		if it.Kind == remote.KindReady {
			break
		}
		if s.QueuedItems() == 0 {
			// Drained: the stream re-attaches, and the reply is handled whole
			// before the caller reads again — it queued, or fell behind again
			// — unless the stream has stopped.
			waitFor(t, "the re-attach's reply handled", func() bool {
				return s.QueuedItems() >= 2 || detaches() > before || s.QueueClosed()
			})
		}
	}
	var ks []remote.Kind
	for _, it := range got {
		ks = append(ks, it.Kind)
	}
	if want := []remote.Kind{remote.KindAttached, remote.KindRestore, remote.KindReady}; !slices.Equal(ks, want) {
		t.Fatalf("the items %v, want %v", ks, want)
	}
	if got[0].Reply.Ready {
		t.Fatalf("the premise: the first reply says ready")
	}
	if r := got[2].Ready; r.StartFailed || len(r.Session.Catalogs.Models) == 0 {
		t.Fatalf("the ready owed: %s", describe(got[2]))
	}
	until(t, s, ofKind(remote.KindSynchronized))
}

// TestACursorlessFirstAttachMustCarryASnapshot (X21; astra r11, further 3):
// the host answers an attach with no cursor with a subscription and an after
// but no snapshot (the tap strips it). The stream could say neither what the
// caller holds nor where it goes on from: Attach returns ErrStreamGap — never
// a stream that begins after events the caller never had — and the
// connection, whose host holds an attachment the client will not use, is
// dropped; the client attaches again, with a snapshot.
func TestACursorlessFirstAttachMustCarryASnapshot(t *testing.T) {
	h := newHost(t)
	h.text("before the attach")
	tp := newTap(t)
	var once sync.Once
	tp.setRewriteIn(func(l wireLine) [][]byte {
		var out [][]byte
		if l.method == protocol.MethodSessionAttach && l.resp != nil && l.resp.Error == nil {
			once.Do(func() { out = [][]byte{withoutMember(t, l.raw, "result", "snapshot")} })
		}
		return out
	})
	c := dialClient(t, h.path, tp, remote.Options{})
	if _, err := c.Attach(tctx(t), remote.AttachOptions{}); !errors.Is(err, remote.ErrStreamGap) {
		t.Fatalf("Attach, its reply with no snapshot: %v", err)
	}
	s := attach(t, c, remote.AttachOptions{})
	if r := nextKind(t, s, remote.KindAttached).Reply; r.Snapshot == nil || r.After.Seq != h.head() {
		t.Fatalf("the attach after: %s", describe(remote.Item{Kind: remote.KindAttached, Reply: r}))
	}
	nextKind(t, s, remote.KindSynchronized)
	if n := tp.connCount(); n != 2 {
		t.Fatalf("%d connections, want the one the reply dropped and the next", n)
	}
}

// malformedEvent is an event notification whose params are not an object.
var malformedEvent = []byte(`{"jsonrpc":"2.0","method":"event","params":[1,2]}`)

// TestAMalformedLineIsFatalInTheOpeningExchange (X21; astra r11, further 2):
// before any reader runs — hello, and a reconnect's sessions.list — a line
// that breaks the protocol is as fatal as on a running connection, never
// passed over for the reply after it: a notification protocol 1 names whose
// params do not decode, or a reply whose id is neither a number nor a
// string. Dial fails; the reconnect drops the connection and redials.
func TestAMalformedLineIsFatalInTheOpeningExchange(t *testing.T) {
	for _, tc := range []struct {
		name string
		line []byte
	}{
		{"hello: an event whose params do not decode", malformedEvent},
		{"hello: a reply whose id is an array", []byte(`{"jsonrpc":"2.0","id":[1],"result":{}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			tp := newTap(t)
			tp.setRewriteIn(func(l wireLine) [][]byte {
				if l.method == protocol.MethodHello && l.resp != nil {
					return [][]byte{tc.line, l.raw}
				}
				return nil
			})
			if _, err := tryDial(t, h.path, tp, remote.Options{}); !errors.Is(err, remote.ErrMalformed) {
				t.Fatalf("Dial with a malformed line before hello's answer: %v", err)
			}
		})
	}
	t.Run("the reconnect's sessions.list: an event whose params do not decode", func(t *testing.T) {
		h := newHost(t)
		tp := newTap(t)
		var once sync.Once
		tp.setRewriteIn(func(l wireLine) [][]byte {
			var out [][]byte
			if l.conn == 1 && l.method == protocol.MethodSessionsList && l.resp != nil {
				once.Do(func() { out = [][]byte{malformedEvent, l.raw} })
			}
			return out
		})
		c := dialClient(t, h.path, tp, remote.Options{})
		s := attach(t, c, remote.AttachOptions{})
		nextKind(t, s, remote.KindAttached)
		nextKind(t, s, remote.KindSynchronized)
		// A replaced engine: the reconnect is a fresh hello, and asks
		// sessions.list which session the host serves now.
		next := startedEngine(t)
		h.srv.SetEngine(next)
		items := until(t, s, ofKind(remote.KindRestore))
		if r := items[len(items)-1].Reply; r.Session.SessionID != next.State().CrazeSessionID {
			t.Fatalf("the restore: %s", describe(items[len(items)-1]))
		}
		if tp.connCount() != 3 || len(tp.sentOn(1, protocol.MethodSessionsList)) != 1 {
			t.Fatalf("%d connections, want the first, the one whose sessions.list saw the malformed line, and a redial", tp.connCount())
		}
		if n := len(tp.sentOn(1, protocol.MethodSessionAttach)); n != 0 {
			t.Fatalf("%d attaches on the connection that saw the malformed line", n)
		}
	})
}
