package remote_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// The command rules of astra's r11 review (X21), each on a schedule the tap
// and the client's hooks pin.

// holdUntil blocks until cond holds, or fails the test's tap after the
// watchdog (it runs on the client's goroutines, where t.Fatal is not allowed).
func holdUntil(tp *tap, what string, cond func() bool) {
	deadline := time.Now().Add(watchdog)
	for !cond() {
		if time.Now().After(deadline) {
			tp.fail("%s: not in %s", what, watchdog)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// TestABusyResendWaitsOutItsBackoff (X21; astra r11 2): a command runs and
// its reply is lost with the connection; after the resume, publication
// resends it, and the host's answer — busy, twice (the tap's) — is handled
// before publication looks for more (the Resent hook holds it until then).
// Publication never picks a command whose backoff is running: it resends it
// once and publishes the connection; each busy is followed by one resend, when
// its backoff has run; and the stored answer comes back — the host ran it
// once.
func TestABusyResendWaitsOutItsBackoff(t *testing.T) {
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	h := newHost(t)
	resends := func() int { return len(tp.sentOn(1, protocol.MethodQueueAdd)) }
	var replied atomic.Int32
	atPublish := make(chan int, 1)
	c := dialHooked(t, h.path, tp, remote.Options{}, remote.TestHooks{
		Replied: func(id string) {
			if id == "1" {
				replied.Add(1)
			}
		},
		Resent: func(id string) {
			if id == "1" {
				holdUntil(tp, "the resend's answer handled", func() bool { return int(replied.Load()) >= resends() })
			}
		},
		Published: func() { atPublish <- resends() },
	})
	redial := make(chan (<-chan struct{}), 1)
	var busy atomic.Int32
	tp.setRewriteIn(func(l wireLine) [][]byte {
		switch {
		case l.method != protocol.MethodQueueAdd || l.resp == nil:
		case l.conn == 0:
			// It ran; its reply is lost with the connection.
			redial <- tp.holdDials()
			tp.killConn(0)
			return [][]byte{}
		case busy.Add(1) <= 2:
			return [][]byte{refusedLine(t, l, protocol.CodeUnavailable, protocol.ReasonBusy)}
		}
		return nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "once"},
			nil, remote.CommandOptions{})
		done <- err
	}()
	await(t, recv(t, redial), "the redial")
	tp.releaseDials()
	if err := recv(t, done); err != nil {
		t.Fatalf("the command, its resends answered busy: %v", err)
	}
	if n := recv(t, atPublish); n != 1 {
		t.Fatalf("publication sent the command %d times before it published the connection, want once: a busy's backoff was not waited out", n)
	}
	if n := resends(); n != 3 {
		t.Fatalf("%d sends on the second connection, want 3: one per busy's backoff, and the one answered", n)
	}
	if n := queued(h, "once"); n != 1 {
		t.Fatalf("the host ran the command %d times", n)
	}
}

// TestWireOrderIsTheOrderOfWrites (X21; astra r11 4): A claims its first
// attempt and pauses before a byte of it is written (the Claimed hook); B is
// sent meanwhile, and runs. A command's place in wire order is taken as its
// first byte is written, not as it is claimed:
//   - A is then written, after B: the wire order is B, A; both replies are
//     lost with the connection, and the resume resends B, then A;
//   - A's write writes nothing, its connection gone under it: A takes no
//     place, and after the resume B's resend goes first, then A — a command
//     never written, after every one that was.
//
// Each ran once, B first.
func TestWireOrderIsTheOrderOfWrites(t *testing.T) {
	for _, written := range []bool{true, false} {
		name := "A written after B"
		if !written {
			name = "A's write writes nothing"
		}
		t.Run(name, func(t *testing.T) {
			tp := newTap(t)
			t.Cleanup(tp.releaseDials)
			h := newHost(t)
			claimed, resume, attempted := make(chan struct{}), make(chan struct{}), make(chan struct{})
			t.Cleanup(func() { closeOnce(resume) })
			var first sync.Once
			c := dialHooked(t, h.path, tp, remote.Options{}, remote.TestHooks{
				Claimed: func(id string) {
					if id == "1" {
						first.Do(func() {
							close(claimed)
							hold(tp, resume, "A's write")
						})
					}
				},
				Attempted: func(id string) {
					if id == "1" {
						close(attempted)
					}
				},
			})
			// Every reply on the first connection is lost; the connection goes
			// with the last one it is to see — A's, or B's when A is never
			// written.
			last := int32(2)
			if !written {
				last = 1
			}
			redial := make(chan (<-chan struct{}), 1)
			var lost atomic.Int32
			tp.setRewriteIn(func(l wireLine) [][]byte {
				if l.conn != 0 || l.method != protocol.MethodQueueAdd || l.resp == nil {
					return nil
				}
				if lost.Add(1) == last {
					redial <- tp.holdDials()
					tp.killConn(0)
				}
				return [][]byte{}
			})
			type answer struct {
				text string
				err  error
			}
			answers := make(chan answer, 2)
			issue := func(text string) {
				go func() {
					_, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: text},
						nil, remote.CommandOptions{})
					answers <- answer{text, err}
				}()
			}
			issue("A")
			await(t, claimed, "A to claim its first attempt")
			issue("B")
			tp.await(t, "B's reply", func() bool {
				return len(tp.received(func(l wireLine) bool {
					return l.conn == 0 && l.method == protocol.MethodQueueAdd && l.resp != nil
				})) == 1
			})
			if written {
				close(resume)
				await(t, recv(t, redial), "the redial")
			} else {
				// The connection is gone — its reader has handed it to the
				// reconnect — before A's write: not a byte of it is written.
				await(t, recv(t, redial), "the redial")
				close(resume)
				await(t, attempted, "A's attempt")
			}
			tp.releaseDials()
			for range 2 {
				if a := recv(t, answers); a.err != nil {
					t.Fatalf("%s: %v", a.text, a.err)
				}
			}
			ids := func(ls []wireLine) []string {
				var out []string
				for _, l := range ls {
					out = append(out, commandID(t, l))
				}
				return out
			}
			want := []string{"2", "1"}
			if !written {
				want = []string{"2"}
			}
			if got := ids(tp.sentOn(0, protocol.MethodQueueAdd)); !slices.Equal(got, want) {
				t.Fatalf("the premise: the first connection's wire order %q, want %q", got, want)
			}
			if got := ids(tp.sentOn(1, protocol.MethodQueueAdd)); !slices.Equal(got, []string{"2", "1"}) {
				t.Fatalf("the second connection's sends %q, want B (2), then A (1): the order of their writes", got)
			}
			var rows []string
			for _, q := range h.eng.State().Queue {
				rows = append(rows, q.Text)
			}
			if !slices.Equal(rows, []string{"B", "A"}) {
				t.Fatalf("the host's rows %q, want B, A", rows)
			}
		})
	}
}

// TestACommandTooLargeForTheNewHostIsNeverSent (X21; astra r11, further 4): a
// command sized against the first host's inbound limit is held while the
// client reconnects, and the reconnect's hello says a smaller limit (the
// tap's). The command is never claimed, and not a byte of it is written:
//   - one that never left the client resolves not run, saying why —
//     ErrNotRun and ErrRequestTooLarge, never outcome-unknown — at once,
//     never stranded;
//   - one that ran on the first connection, its reply lost, resolves
//     outcome-unknown (resume_lost): the resumed connection cannot carry its
//     resend.
//
// Either way the connection is published, and the client goes on.
func TestACommandTooLargeForTheNewHostIsNeverSent(t *testing.T) {
	const limit = 2 << 10
	text := strings.Repeat("x", 2*limit)
	for _, ran := range []bool{false, true} {
		name := "never sent"
		if ran {
			name = "run, its reply lost"
		}
		t.Run(name, func(t *testing.T) {
			tp := newTap(t)
			t.Cleanup(tp.releaseDials)
			h := newHost(t)
			c := dialClient(t, h.path, tp, remote.Options{})
			redial := make(chan (<-chan struct{}), 1)
			tp.setRewriteIn(func(l wireLine) [][]byte {
				switch {
				case l.resp == nil:
				case l.conn == 0 && l.method == protocol.MethodQueueAdd:
					redial <- tp.holdDials()
					tp.killConn(0)
					return [][]byte{}
				case l.conn == 1 && l.method == protocol.MethodHello && l.resp.Error == nil:
					return [][]byte{withMember(t, l.raw, strconv.Itoa(limit), "result", "limits", "inboundLine")}
				}
				return nil
			})
			if !ran {
				// Issued once the reconnect has begun — the lost connection
				// no longer the one calls go to — so it is held.
				redial <- tp.holdDials()
				tp.kill()
				await(t, recv(t, redial), "the redial")
			}
			done := make(chan error, 1)
			go func() {
				_, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: text},
					nil, remote.CommandOptions{})
				done <- err
			}()
			if ran {
				await(t, recv(t, redial), "the redial")
			} else {
				waitFor(t, "the command held", func() bool { return c.Waiting() == 1 })
			}
			tp.releaseDials()
			err := recv(t, done)
			if ran {
				outcomeUnknown(t, err, protocol.ReasonResumeLost)
			} else {
				var te *remote.TooLargeError
				if !errors.Is(err, remote.ErrNotRun) || !errors.Is(err, remote.ErrRequestTooLarge) ||
					errors.Is(err, remote.ErrOutcomeUnknown) || !errors.As(err, &te) || te.Limit != limit {
					t.Fatalf("a command the new host cannot read, never sent: %v", err)
				}
			}
			if hc := c.Hello(); !hc.Resumed || hc.Limits.InboundLine != limit {
				t.Fatalf("the premise: the reconnect's hello resumed with a limit of %d: %+v", limit, hc)
			}
			sends := 0
			if ran {
				sends = 1
			}
			if n := len(tp.sent(protocol.MethodQueueAdd)); n != sends {
				t.Fatalf("%d sends of the command, want %d", n, sends)
			}
			if n := queued(h, text); n != sends {
				t.Fatalf("the host ran the command %d times, want %d", n, sends)
			}
			command(t, c, protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "small"}, nil)
		})
	}
}
