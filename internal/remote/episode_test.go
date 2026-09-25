package remote_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// The reconnect episode's bound on the adoption's writes, and Close during
// an adoption (X21; astra r11 6 and 9).

// stalledAdoption is a client whose command ran and whose reply was lost with
// its connection, and whose reconnect reaches a host that answers hello and
// the stream's re-attach and then stops reading: the resend publication makes
// stalls, as a write to a full socket does (the tap's stall). It returns once
// that write has begun, with the command's answer to come on done, and a time
// just after the loss.
func stalledAdoption(t *testing.T, window time.Duration) (*tap, *remote.Client, *remote.Stream, <-chan error, time.Time) {
	t.Helper()
	h := newHost(t)
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	c := dialClient(t, h.path, tp, remote.Options{RedialWindow: window})
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	tp.setStallOut(func(l wireLine) bool { return l.conn == 1 && l.method == protocol.MethodQueueAdd })
	held := losesReply(tp, protocol.MethodQueueAdd)
	done := make(chan error, 1)
	go func() {
		_, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "once"},
			nil, remote.CommandOptions{})
		done <- err
	}()
	await(t, recv(t, held), "the redial")
	lost := time.Now()
	tp.releaseDials()
	tp.await(t, "the resend's write", func() bool { return len(tp.sentOn(1, protocol.MethodQueueAdd)) == 1 })
	if hc := c.Hello(); !hc.Resumed {
		t.Fatalf("the premise: the reconnect resumed: %+v", hc)
	}
	if n := len(tp.received(func(l wireLine) bool {
		return l.conn == 1 && l.method == protocol.MethodSessionAttach && l.resp != nil && l.resp.Error == nil
	})); n != 1 {
		t.Fatalf("the premise: the host answered the re-attach before the resend (%d replies)", n)
	}
	return tp, c, s, done, lost
}

// TestAnAdoptionsWritesEndWithTheEpisode (X21; astra r11 6): the reconnect's
// host answers hello and the re-attach, then stops reading, and publication's
// resend blocks. The write carries the episode's deadline: when the episode
// ends, it fails its attempt, the episode is spent, and the command — which
// ran — resolves outcome-unknown (disconnected), within the episode, never
// stranded; the client stops, its stream's last item ErrDisconnected.
func TestAnAdoptionsWritesEndWithTheEpisode(t *testing.T) {
	const window = time.Second
	tp, c, s, done, lost := stalledAdoption(t, window)
	outcomeUnknown(t, recv(t, done), protocol.ReasonDisconnected)
	if elapsed := time.Since(lost); elapsed > 3*window {
		t.Fatalf("the command resolved %s after the loss, past its episode of %s", elapsed, window)
	}
	await(t, c.Done(), "the client to stop")
	if !errors.Is(c.Err(), remote.ErrDisconnected) {
		t.Fatalf("the client stopped with %v", c.Err())
	}
	if last := until(t, s, ofKind(remote.KindError)); !errors.Is(last[len(last)-1].Err, remote.ErrDisconnected) {
		t.Fatalf("the stream's last item: %s", describe(last[len(last)-1]))
	}
	if n := tp.connCount(); n != 2 {
		t.Fatalf("%d connections: a redial after the episode was spent", n)
	}
}

// TestCloseEndsABlockedAdoptionWrite (X21; astra r11 9): the same stalled
// resend, in an episode far longer than the test: Close closes the connection
// being adopted — not yet the one calls go to — so the blocked write fails,
// every goroutine returns, and Close does too; the command, which ran,
// resolves outcome-unknown (disconnected).
func TestCloseEndsABlockedAdoptionWrite(t *testing.T) {
	_, c, _, done, _ := stalledAdoption(t, 30*time.Second)
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	if err := recv(t, closed); err != nil {
		t.Fatal(err)
	}
	outcomeUnknown(t, recv(t, done), protocol.ReasonDisconnected)
}

// lostReplyAndReattach is a client, dialled through tp, whose command ran and
// whose reply was lost with its connection, and whose reconnect reaches the
// host again (resumed) with its stream re-attaching: the host's answer to a
// re-attach is replaced by reattached's ([][]byte{}: lost), and a write on
// the new connection for which stall returns true stalls (tap.stallOut). It
// returns once the redial has begun and its dial is released, with the
// command's answer to come on done, and the time of the loss.
func lostReplyAndReattach(t *testing.T, tp *tap, window time.Duration, h remote.TestHooks,
	reattached func(wireLine) [][]byte, stall func(wireLine) bool) (*remote.Client, *remote.Stream, <-chan error, time.Time) {
	t.Helper()
	host := newHost(t)
	t.Cleanup(tp.releaseDials)
	c := dialHooked(t, host.path, tp, remote.Options{RedialWindow: window}, h)
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	redial := make(chan (<-chan struct{}), 1)
	var armed atomic.Bool
	armed.Store(true)
	tp.setRewriteIn(func(l wireLine) [][]byte {
		switch {
		case l.resp == nil:
		case l.conn == 0 && l.method == protocol.MethodQueueAdd && armed.CompareAndSwap(true, false):
			redial <- tp.holdDials()
			tp.killConn(0)
			return [][]byte{}
		case l.conn == 1 && l.method == protocol.MethodSessionAttach:
			return reattached(l)
		}
		return nil
	})
	if stall != nil {
		tp.setStallOut(func(l wireLine) bool { return l.conn == 1 && stall(l) })
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: host.sid(), Text: "once"},
			nil, remote.CommandOptions{})
		done <- err
	}()
	await(t, recv(t, redial), "the redial")
	lost := time.Now()
	tp.releaseDials()
	return c, s, done, lost
}

// givenUp checks that a reconnect gave up within its episode: the command,
// which ran, resolves outcome-unknown (disconnected) within bound of the loss —
// never stranded — the client stops ErrDisconnected, its stream's last item
// carrying it, and no connection was made after the reconnect's.
func givenUp(t *testing.T, tp *tap, c *remote.Client, s *remote.Stream, done <-chan error, lost time.Time, bound time.Duration, why string) {
	t.Helper()
	select {
	case err := <-done:
		outcomeUnknown(t, err, protocol.ReasonDisconnected)
	case <-time.After(time.Until(lost.Add(bound))):
		t.Fatalf("the command is stranded %s after the loss: %s", bound, why)
	}
	await(t, c.Done(), "the client to stop")
	if !errors.Is(c.Err(), remote.ErrDisconnected) {
		t.Fatalf("the client stopped with %v", c.Err())
	}
	if last := until(t, s, ofKind(remote.KindError)); !errors.Is(last[len(last)-1].Err, remote.ErrDisconnected) {
		t.Fatalf("the stream's last item: %s", describe(last[len(last)-1]))
	}
	if n := tp.connCount(); n != 2 {
		t.Fatalf("%d connections: a redial after the episode was spent", n)
	}
}

// TestAWriteDuringTheReattachWaitIsBoundedByTheEpisode (C8c; astra r13 2): the
// reconnect's re-attach is refused retryably (the tap's unavailable), so the
// stream writes it again while the adoption waits for its reply — the one wait
// whose clock is stopped — and that write stalls: a host that has stopped
// reading. Only the wait for a reply stops the clock; the write runs it, so
// when the episode ends the write fails, the episode is spent, and the
// reconnect gives up within it.
func TestAWriteDuringTheReattachWaitIsBoundedByTheEpisode(t *testing.T) {
	const window = time.Second
	tp := newTap(t)
	retries := func() int { return len(tp.sentOn(1, protocol.MethodSessionAttach)) - 1 }
	c, s, done, lost := lostReplyAndReattach(t, tp, window, remote.TestHooks{},
		func(l wireLine) [][]byte {
			return [][]byte{refusedLine(t, l, protocol.CodeUnavailable, protocol.ReasonNotReady)}
		},
		func(l wireLine) bool { return l.method == protocol.MethodSessionAttach && retries() == 1 })
	tp.await(t, "the retried re-attach's write", func() bool { return retries() == 1 })
	givenUp(t, tp, c, s, done, lost, 3*window, "the write made during the re-attach's wait is unbounded")
}

// TestAnEpisodeSpentBeforeItsWaitNeverPauses (C8c; astra r13, further 2): the
// reconnect's re-attach is written, and the episode runs out before the
// adoption begins its wait for the reply (the Pausing hook holds it until
// then); the host's answer is lost (the tap's), and the host, its socket open,
// says nothing more. An episode already spent never stops its clock: the
// wait ends at once, and the reconnect gives up.
func TestAnEpisodeSpentBeforeItsWaitNeverPauses(t *testing.T) {
	const window = time.Second
	tp := newTap(t)
	var paused atomic.Bool
	c, s, done, lost := lostReplyAndReattach(t, tp, window, remote.TestHooks{
		Pausing: func(spent <-chan struct{}) {
			paused.Store(true)
			hold(tp, spent, "the episode's end")
		},
	}, func(wireLine) [][]byte { return [][]byte{} }, nil)
	givenUp(t, tp, c, s, done, lost, 3*window, "an episode spent before its wait began waits for ever")
	if !paused.Load() {
		t.Fatal("the premise: the adoption never reached its wait for the re-attach's reply")
	}
}

// TestAReattachWaitDoesNotSpendTheEpisode (X20, X21): before readiness, a
// reconnect's re-attach is when: "ready", and its reply waits for the start —
// here until well past the episode's end — while a command issued during the
// reconnect is held. The episode's clock stops for that wait, which it does
// not bound:
//   - once the session has started, the re-attach is answered and
//     publication sends the command within what was left of the episode;
//   - the connection goes during the wait: the episode goes on with what was
//     left of it, and redials; the next connection's re-attach waits for the
//     start in turn, and publication then sends the command.
//
// Either way the command runs, once, and the client goes on.
func TestAReattachWaitDoesNotSpendTheEpisode(t *testing.T) {
	const window = 300 * time.Millisecond
	for _, lost := range []bool{false, true} {
		name := "answered"
		if lost {
			name = "the connection goes during it"
		}
		t.Run(name, func(t *testing.T) {
			h := newHost(t, withoutStart())
			tp := newTap(t)
			t.Cleanup(tp.releaseDials)
			c := dialClient(t, h.path, tp, remote.Options{RedialWindow: window})
			s := attach(t, c, remote.AttachOptions{When: protocol.WhenNow})
			if r := nextKind(t, s, remote.KindAttached).Reply; r.Ready {
				t.Fatalf("the premise: the first reply says ready: %+v", r)
			}
			nextKind(t, s, remote.KindSynchronized)
			held := tp.holdDials()
			tp.kill()
			await(t, held, "the redial")
			done := make(chan error, 1)
			go func() {
				_, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "held"},
					nil, remote.CommandOptions{})
				done <- err
			}()
			waitFor(t, "the command held", func() bool { return c.Waiting() == 1 })
			tp.releaseDials()
			tp.await(t, "the re-attach", func() bool { return len(tp.sentOn(1, protocol.MethodSessionAttach)) == 1 })
			// The episode's own end — RedialWindow after the loss — passes
			// while the re-attach waits for the start.
			<-time.After(3 * window)
			if n := len(tp.received(func(l wireLine) bool { return l.conn == 1 && l.method == protocol.MethodSessionAttach })); n != 0 {
				t.Fatalf("the premise: the re-attach was answered before the start (%d replies)", n)
			}
			conns := 2
			if lost {
				tp.kill()
				tp.await(t, "the next re-attach", func() bool { return len(tp.sentOn(2, protocol.MethodSessionAttach)) == 1 })
				conns = 3
			}
			if err := h.eng.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := recv(t, done); err != nil {
				t.Fatalf("the command held across the re-attach's wait: %v", err)
			}
			if err := c.Err(); err != nil {
				t.Fatalf("the client stopped: %v", err)
			}
			if n := tp.connCount(); n != conns {
				t.Fatalf("%d connections, want %d", n, conns)
			}
			if n := queued(h, "held"); n != 1 {
				t.Fatalf("the host ran the command %d times", n)
			}
			if k := kinds(until(t, s, ofKind(remote.KindSynchronized))); k[remote.KindRestore] != 1 || k[remote.KindReady] != 1 {
				t.Fatalf("the re-attach's items: %v", k)
			}
		})
	}
}
