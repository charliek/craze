package remote_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// Presence on the client (plan 032 §3.14, SF-64): the stream hands up the
// host's presence notification for its own subscription as a Presence item —
// never sequenced, never a reason to fall behind, folded at the queue's tail
// — and a Presence of 0 when that subscription ends, so a caller clears the
// count until the next attachment's arrives; the Session maps it to
// backend.ItemPresence.

// presenceNotes is how many presence notifications the host wrote to the
// client, and the last one's count.
func presenceNotes(tp *tap) (int, uint) {
	lines := tp.received(func(l wireLine) bool { return l.method == protocol.NotifyPresence })
	if len(lines) == 0 {
		return 0, 0
	}
	var p protocol.PresenceParams
	_ = json.Unmarshal(lines[len(lines)-1].params, &p)
	return len(lines), p.Attached
}

// otherClient is a second client of h, attached; its Close is the test's to
// call (or the cleanup's).
func otherClient(t *testing.T, h *host) (*remote.Client, *remote.Stream) {
	t.Helper()
	c := dialClient(t, h.path, newTap(t), remote.Options{})
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	return c, s
}

// TestTheStreamHandsUpPresence: on a presence host the stream's synchronized
// is followed by a Presence item counting this client; a second client
// attaching makes it 2, leaving makes it 1 again. The negative control: the
// same client of a host without presence is handed up no Presence item — the
// next item after synchronized is the next event.
func TestTheStreamHandsUpPresence(t *testing.T) {
	h := newHost(t, withPresence())
	c := dialClient(t, h.path, newTap(t), remote.Options{})
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	if it := nextKind(t, s, remote.KindPresence); it.Attached != 1 {
		t.Fatalf("the first presence: %d, want 1", it.Attached)
	}
	other, os := otherClient(t, h)
	if it := nextKind(t, os, remote.KindPresence); it.Attached != 2 {
		t.Fatalf("the second client's presence: %d, want 2", it.Attached)
	}
	if it := nextKind(t, s, remote.KindPresence); it.Attached != 2 {
		t.Fatalf("the first client's presence once another attached: %d, want 2", it.Attached)
	}
	other.Close()
	if it := nextKind(t, s, remote.KindPresence); it.Attached != 1 {
		t.Fatalf("the first client's presence once the other left: %d, want 1", it.Attached)
	}

	old := newHost(t)
	oc := dialClient(t, old.path, newTap(t), remote.Options{})
	ostream := attach(t, oc, remote.AttachOptions{})
	nextKind(t, ostream, remote.KindAttached)
	nextKind(t, ostream, remote.KindSynchronized)
	otherClient(t, old)
	old.text("after both attached")
	if it := next(t, ostream); it.Kind != remote.KindEvent {
		t.Fatalf("a host without presence: the item after synchronized is %s, want the next event", describe(it))
	}
}

// TestASessionHandsUpPresence: remote.Session maps the Presence item to
// backend.ItemPresence, in the stream generation it came in, a client of a
// presence host reading it after the first Restore.
func TestASessionHandsUpPresence(t *testing.T) {
	h := newHost(t, withPresence())
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	sess, err := remote.DialSession(ctx, h.path, remote.SessionOptions{
		Client: remote.Options{Client: protocol.ClientInfo{Kind: "test", Name: "presence"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if err := sess.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	var gen uint64
	for {
		it, err := sess.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if it.Kind == backend.ItemRestore {
			gen = it.Gen
			continue
		}
		if it.Kind != backend.ItemPresence {
			continue
		}
		if it.Attached != 1 || it.Gen != gen || gen == 0 {
			t.Fatalf("the presence item: %+v, want 1 in generation %d", it, gen)
		}
		return
	}
}

// TestPresenceNeverMakesTheStreamFallBehind: a caller that has stopped
// reading, its queue past its bound, is handed every count all the same —
// past the bound, each folded into the one still waiting at the queue's tail
// — and the stream never falls behind for one (no detach goes out). Read at
// last, the queue holds the event and one Presence, the latest. The negative
// controls: the queue was past its bound (the next event does fall behind),
// and the host wrote the client several counts, so it was the stream that
// folded them.
func TestPresenceNeverMakesTheStreamFallBehind(t *testing.T) {
	h := newHost(t, withPresence())
	tp := newTap(t)
	const bound = 64 << 10
	c := dialClient(t, h.path, tp, remote.Options{StreamBytes: bound})
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	nextKind(t, s, remote.KindPresence)
	// One event over the bound: an empty queue takes it, and from then on
	// the queue is past its bound — anything but a Presence finds no room.
	h.text(strings.Repeat("o", 2*bound))
	awaitQueued(t, s, "the big event queued", func(items []remote.Item) bool {
		return len(items) == 1 && items[0].Kind == remote.KindEvent
	})
	if s.QueuedBytes() <= bound {
		t.Fatalf("the premise: the queue holds %d bytes, not past its bound %d", s.QueuedBytes(), bound)
	}
	var others []*remote.Client
	for range 3 {
		oc, _ := otherClient(t, h)
		others = append(others, oc)
	}
	tp.await(t, "the host's count of four", func() bool { _, last := presenceNotes(tp); return last == 4 })
	for _, oc := range others[1:] {
		oc.Close()
	}
	tp.await(t, "the host's count of two", func() bool { _, last := presenceNotes(tp); return last == 2 })
	if n, _ := presenceNotes(tp); n < 3 {
		t.Fatalf("the premise: the host wrote %d counts", n)
	}
	// What the stream did with them, read off its queue once it has applied
	// the last (the tap records a line before the client reads it).
	awaitQueued(t, s, "the count of two queued", func(items []remote.Item) bool {
		last := len(items) - 1
		return last >= 0 && items[last].Kind == remote.KindPresence && items[last].Attached == 2
	})
	if d := tp.sent(protocol.MethodSessionDetach); len(d) != 0 {
		t.Fatalf("the stream fell behind for a count: %d detaches", len(d))
	}
	if n := s.QueuedItems(); n != 2 {
		t.Fatalf("the queue holds %d items, want the event and one Presence", n)
	}
	nextKind(t, s, remote.KindEvent)
	if it := nextKind(t, s, remote.KindPresence); it.Attached != 2 {
		t.Fatalf("the queue's presence: %d, want the latest, 2", it.Attached)
	}

	// And past the bound, an event is what makes it fall behind.
	h.text(strings.Repeat("o", 2*bound))
	h.text(strings.Repeat("p", 2*bound))
	tp.await(t, "the fall behind's detach", func() bool { return len(tp.sent(protocol.MethodSessionDetach)) == 1 })
}

// TestAResetClearsPresence: a subscription that ends — here reset by an
// engine replaced under it — hands up a Presence of 0 before what follows,
// and the new attachment's count after its own synchronized. The negative
// control: the count before the reset was 1, not 0.
func TestAResetClearsPresence(t *testing.T) {
	h := newHost(t, withPresence())
	c := dialClient(t, h.path, newTap(t), remote.Options{})
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	if it := nextKind(t, s, remote.KindPresence); it.Attached != 1 {
		t.Fatalf("the count before the reset: %d", it.Attached)
	}
	h.srv.SetEngine(startedEngine(t))
	items := until(t, s, ofKind(remote.KindRestore))
	if len(items) < 2 || items[len(items)-2].Kind != remote.KindPresence || items[len(items)-2].Attached != 0 {
		t.Fatalf("before the restore: %v, want a Presence of 0 just before it", described(items))
	}
	nextKind(t, s, remote.KindSynchronized)
	if it := nextKind(t, s, remote.KindPresence); it.Attached != 1 {
		t.Fatalf("the new attachment's count: %d, want 1", it.Attached)
	}
}

// TestALostConnectionClearsPresenceAtOnce (review r29 item 4): the
// connection a counted subscription lives on goes (EOF), and the reconnect's
// handshake is held — its hello behind a barrier — so no re-attach can be
// made: the stream hands up a Presence of 0 all the same, before the
// reconnect, not once it has adopted a connection. Released, the stream
// re-attaches and the count comes back. The negative control is the premise,
// checked while the clear is read: the hello is held and only the first
// attach was ever sent, so the clear cannot be the re-attach's.
func TestALostConnectionClearsPresenceAtOnce(t *testing.T) {
	h := newHost(t, withPresence())
	tp := newTap(t)
	helloHeld, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	tp.setHoldOut(func(l wireLine) <-chan struct{} {
		if l.method == protocol.MethodHello && l.conn > 0 {
			once.Do(func() { close(helloHeld) })
			return release
		}
		return nil
	})
	c := dialClient(t, h.path, tp, remote.Options{})
	// Registered after the client, so it runs before the client's Close.
	t.Cleanup(func() { closeOnce(release) })
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	nextKind(t, s, remote.KindPresence)
	otherClient(t, h)
	if it := nextKind(t, s, remote.KindPresence); it.Attached != 2 {
		t.Fatalf("the count before the loss: %d, want 2", it.Attached)
	}

	tp.kill()
	await(t, helloHeld, "the reconnect's hello to be held")
	awaitQueued(t, s, "the clear with the handshake held", func(items []remote.Item) bool {
		last := len(items) - 1
		return last >= 0 && items[last].Kind == remote.KindPresence && items[last].Attached == 0
	})
	if n := len(tp.sent(protocol.MethodSessionAttach)); n != 1 {
		t.Fatalf("the premise: %d attaches sent while the hello is held, want the first alone", n)
	}
	if it := nextKind(t, s, remote.KindPresence); it.Attached != 0 {
		t.Fatalf("the clear: %d", it.Attached)
	}

	closeOnce(release)
	items := until(t, s, func(it remote.Item) bool { return it.Kind == remote.KindPresence && it.Attached == 2 })
	if n := len(tp.sent(protocol.MethodSessionAttach)); n != 2 {
		t.Fatalf("the count came back with %d attaches sent (%v), want the re-attach's", n, described(items))
	}
}

// awaitQueued waits, within the watchdog, for cond over what s's queue holds
// (Stream.AwaitQueued: the queue's own change signal).
func awaitQueued(t *testing.T, s *remote.Stream, what string, cond func([]remote.Item) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	if err := s.AwaitQueued(ctx, cond); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// described is each item described, for a failure.
func described(items []remote.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = describe(it)
	}
	return out
}

// TestAClientThatDoesNotKnowPresenceIgnoresIt (§3.15, an older client): a
// presence host's notification, renamed to a method this client does not
// know — what "presence" is to a client from before it — is read past: the
// stream hands up exactly what a host without presence gives, and goes on. The
// negative control: unrenamed, the same host's count is handed up.
func TestAClientThatDoesNotKnowPresenceIgnoresIt(t *testing.T) {
	h := newHost(t, withPresence())
	tp := newTap(t)
	var renamed atomic.Int32
	tp.setRewriteIn(func(l wireLine) [][]byte {
		if l.method != protocol.NotifyPresence {
			return nil
		}
		renamed.Add(1)
		return [][]byte{bytes.Replace(l.raw, []byte(`"method":"presence"`), []byte(`"method":"presence.from.a.later.host"`), 1)}
	})
	c := dialClient(t, h.path, tp, remote.Options{})
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	// The first count written before the second client attaches, so the
	// second is a change of its own the host must send — not one it may
	// fold into the first.
	tp.await(t, "the first count", func() bool { n, _ := presenceNotes(tp); return n == 1 })
	otherClient(t, h)
	tp.await(t, "two counts", func() bool { n, _ := presenceNotes(tp); return n == 2 })
	h.text("after the counts")
	if it := next(t, s); it.Kind != remote.KindEvent {
		t.Fatalf("after the renamed counts: %s, want the next event", describe(it))
	}
	if n := renamed.Load(); n != 2 {
		t.Fatalf("renamed %d counts", n)
	}

	plain := newHost(t, withPresence())
	pc := dialClient(t, plain.path, newTap(t), remote.Options{})
	ps := attach(t, pc, remote.AttachOptions{})
	nextKind(t, ps, remote.KindAttached)
	nextKind(t, ps, remote.KindSynchronized)
	nextKind(t, ps, remote.KindPresence)
}
