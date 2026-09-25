package remote_test

import (
	"context"
	"errors"
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
