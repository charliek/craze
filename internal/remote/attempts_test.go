package remote_test

import (
	"bytes"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// The command rules of astra's r9 review (X18 1–3, 5, 7), each on a schedule
// the tap and the client's hooks pin.

// hold blocks until ch is closed, or fails the test's tap after the watchdog
// (it runs on the client's goroutines, where t.Fatal is not allowed).
func hold(tp *tap, ch <-chan struct{}, what string) {
	select {
	case <-ch:
	case <-time.After(watchdog):
		tp.fail("%s: not in %s", what, watchdog)
	}
}

// TestOverlappingAttemptsNeverRunACommandTwice (X18 1; astra r9 1, the
// blocker) drives the review's schedule: a command registered while the
// client reconnects, its caller paused before its first attempt, is sent by
// the reconnect (attempt 1) and runs; its reply is lost. The caller's own
// attempt, released then, sends nothing: one attempt at a time. So no attempt
// 2 exists to be refused unavailable/busy — the admission cap's "nothing ran",
// which the tap stands in for on any second send — and attempt 1's possible
// execution is never erased. Then:
//   - the connection goes, and the resume is lost (resumed: false): the
//     command resolves resume_lost and is never sent under the fresh id;
//   - or the client is closed: the command resolves outcome-unknown
//     (disconnected), never ErrNotRun.
//
// The host ran it once.
func TestOverlappingAttemptsNeverRunACommandTwice(t *testing.T) {
	for _, lost := range []bool{true, false} {
		name := "then Close"
		if lost {
			name = "then a resume that is lost"
		}
		t.Run(name, func(t *testing.T) {
			tp := newTap(t)
			t.Cleanup(tp.releaseDials)
			h := newHost(t)
			paused, resume, attempted := make(chan struct{}), make(chan struct{}), make(chan struct{})
			closed := make(chan struct{})
			t.Cleanup(func() { closeOnce(resume); closeOnce(closed) })
			var replied atomic.Int32
			c := dialHooked(t, h.path, tp, remote.Options{}, remote.TestHooks{
				Registered: func(id string) {
					if id == "1" {
						close(paused)
						hold(tp, resume, "the caller's release")
					}
				},
				Attempted: func(id string) {
					if id == "1" {
						close(attempted)
					}
				},
				Replied: func(id string) {
					if id == "1" {
						replied.Add(1)
					}
				},
				Retrying: func(string) {
					// A caller retrying a refused attempt waits for Close,
					// when the client is to be closed.
					if !lost {
						hold(tp, closed, "the Close")
					}
				},
			})
			first := c.Hello()

			// On the second connection, by request id — the host runs the
			// sends of one connection on goroutines of their own, so their
			// replies may come in either order — the reply to the command's
			// first send waits until the caller's own attempt has returned,
			// and is lost; the reply to any later send is refused
			// unavailable/busy. Once every send has had its reply read, the
			// connection goes (when the resume is to be lost), redials held.
			redial := make(chan (<-chan struct{}), 1)
			var seen, busy atomic.Int32
			var killed atomic.Bool
			tp.setRewriteIn(func(l wireLine) [][]byte {
				if l.conn != 1 || l.method != protocol.MethodQueueAdd || l.resp == nil {
					return nil
				}
				out := [][]byte{}
				if l.id == tp.sentOn(1, protocol.MethodQueueAdd)[0].id {
					hold(tp, attempted, "the caller's own attempt")
				} else {
					busy.Add(1)
					out = [][]byte{refusedLine(t, l, protocol.CodeUnavailable, protocol.ReasonBusy)}
				}
				if int(seen.Add(1)) == len(tp.sentOn(1, protocol.MethodQueueAdd)) && lost && killed.CompareAndSwap(false, true) {
					redial <- tp.holdDials()
					tp.killConn(1)
				}
				return out
			})

			held := tp.holdDials()
			tp.kill()
			await(t, held, "the redial")
			done := make(chan error, 1)
			go func() {
				_, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "once"},
					nil, remote.CommandOptions{Retry: true})
				done <- err
			}()
			await(t, paused, "the caller to pause before its first attempt")
			tp.releaseDials()
			tp.await(t, "the reconnect's send", func() bool { return len(tp.sentOn(1, protocol.MethodQueueAdd)) == 1 })
			close(resume)
			await(t, attempted, "the caller's own attempt")
			if n := len(tp.sentOn(1, protocol.MethodQueueAdd)); n != 1 {
				t.Fatalf("the command was sent %d times on one connection: overlapping attempts", n)
			}

			if lost {
				await(t, recv(t, redial), "the second redial")
				// The host released the client with its connection; past the
				// age bound it is retired, and the resume is lost.
				h.logs.wait(t, "conn 2 close", "client "+first.ClientID)
				h.advanceClock(pastRetirement)
				tp.releaseDials()
				outcomeUnknown(t, recv(t, done), protocol.ReasonResumeLost)
				if hc := c.Hello(); hc.Resumed || hc.ClientID == first.ClientID {
					t.Fatalf("the third hello: %+v, want a fresh client", hc)
				}
				if n := len(tp.sentOn(2, protocol.MethodQueueAdd)); n != 0 {
					t.Fatalf("the command was sent %d times under the fresh id", n)
				}
			} else {
				// Every reply the client has read is handled — a busy among
				// them, had an overlapping attempt been made — and then Close.
				waitFor(t, "the replies read", func() bool {
					return int(seen.Load()) == len(tp.sentOn(1, protocol.MethodQueueAdd))
				})
				waitFor(t, "the replies handled", func() bool { return replied.Load() >= busy.Load() })
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				close(closed)
				err := recv(t, done)
				if errors.Is(err, remote.ErrNotRun) {
					t.Fatalf("Close said a command that ran did not: %v", err)
				}
				outcomeUnknown(t, err, protocol.ReasonDisconnected)
			}
			if n := queued(h, "once"); n != 1 {
				t.Fatalf("the host ran the command %d times", n)
			}
		})
	}
}

// TestARefusedResendNeverErasesAnUnansweredAttempt (X18 1): a command whose
// first attempt ran and whose reply was lost with its connection is resent
// after a resume (resumed: true). A refusal of the resend never settles
// attempt 1:
//   - a gate refusal (not_accepting, the tap's) is the resend's own answer,
//     handed over, but "may have run" stays: the caller, retrying by code, is
//     closed before its retry, and Close says outcome-unknown, never ErrNotRun;
//   - unavailable/busy — the host's cap on commands in flight, which answers
//     before its receipts are asked — says nothing of attempt 1, so the client
//     waits it out and resends, as it does in_progress: the stored answer comes
//     back, and the caller never sees the busy.
func TestARefusedResendNeverErasesAnUnansweredAttempt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   protocol.Code
		reason protocol.Reason
	}{
		{"a gate refusal", protocol.CodeNotAccepting, protocol.ReasonNotAccepting},
		{"busy", protocol.CodeUnavailable, protocol.ReasonBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTap(t)
			t.Cleanup(tp.releaseDials)
			h := newHost(t)
			retrying, retry := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() { closeOnce(retry) })
			c := dialHooked(t, h.path, tp, remote.Options{}, remote.TestHooks{
				Retrying: func(string) {
					closeOnce(retrying)
					hold(tp, retry, "the retry's release")
				},
			})
			redial := make(chan (<-chan struct{}), 1)
			var once sync.Once
			tp.setRewriteIn(func(l wireLine) [][]byte {
				if l.method != protocol.MethodQueueAdd || l.resp == nil {
					return nil
				}
				var out [][]byte
				switch l.conn {
				case 0:
					// Attempt 1 ran; its reply is lost with the connection.
					redial <- tp.holdDials()
					tp.killConn(0)
					out = [][]byte{}
				case 1:
					once.Do(func() { out = [][]byte{refusedLine(t, l, tc.code, tc.reason)} })
				}
				return out
			})
			done := make(chan error, 1)
			go func() {
				_, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "once"},
					nil, remote.CommandOptions{Retry: tc.code != protocol.CodeUnavailable})
				done <- err
			}()
			await(t, recv(t, redial), "the redial")
			tp.releaseDials()
			if tc.code == protocol.CodeUnavailable {
				if err := recv(t, done); err != nil {
					t.Fatalf("the command, its resend refused busy: %v", err)
				}
				if sends := tp.sent(protocol.MethodQueueAdd); len(sends) != 3 || sends[2].conn != 1 {
					t.Fatalf("%d sends, want the first, a resend refused busy, and one more", len(sends))
				}
			} else {
				await(t, retrying, "the caller to retry the refused resend")
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				close(retry)
				err := recv(t, done)
				if errors.Is(err, remote.ErrNotRun) {
					t.Fatalf("Close said a command that ran did not: %v", err)
				}
				outcomeUnknown(t, err, protocol.ReasonDisconnected)
			}
			if n := queued(h, "once"); n != 1 {
				t.Fatalf("the host ran the command %d times", n)
			}
		})
	}
}

// TestAContradictoryResumeIsAResumeLoss (X18 2; astra r9 2): a command runs
// and its reply is lost with the connection; the reconnect's hello answers
// resumed: true with the client id asked for, but on another host, or with
// another token than the one sent (the tap rewrites the answer). The client
// trusts neither: it is a resume loss — the command resolves resume_lost and
// is not resent.
func TestAContradictoryResumeIsAResumeLoss(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		path  []string
	}{
		{"another host", `"fedcba987654"`, []string{"result", "endpoint", "hostId"}},
		{"another token", `"00000000000000000000000000000000"`, []string{"result", "token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTap(t)
			t.Cleanup(tp.releaseDials)
			h := newHost(t)
			c := dialClient(t, h.path, tp, remote.Options{})
			first := c.Hello()
			redial := make(chan (<-chan struct{}), 1)
			tp.setRewriteIn(func(l wireLine) [][]byte {
				switch {
				case l.resp == nil:
				case l.conn == 0 && l.method == protocol.MethodQueueAdd:
					redial <- tp.holdDials()
					tp.killConn(0)
					return [][]byte{}
				case l.conn == 1 && l.method == protocol.MethodHello && l.resp.Error == nil:
					return [][]byte{withMember(t, l.raw, tc.value, tc.path...)}
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
			outcomeUnknown(t, recv(t, done), protocol.ReasonResumeLost)
			if hc := c.Hello(); !hc.Resumed || hc.ClientID != first.ClientID {
				t.Fatalf("the premise: the second hello says it resumed %s: %+v", first.ClientID, hc)
			}
			if n := len(tp.sent(protocol.MethodQueueAdd)); n != 1 {
				t.Fatalf("the command was sent %d times: resent on a resume the client cannot trust", n)
			}
			if n := queued(h, "once"); n != 1 {
				t.Fatalf("the host ran the command %d times", n)
			}
		})
	}
}

// TestResendsGoInWireOrderBeforeNewCommands (X18 3; astra r9 3): A is
// registered and pauses before its first attempt; B is registered, sent, and
// runs; then A is sent — the wire order is B, A — and both replies are lost
// with the connection. C is issued while the client reconnects. After the resume the
// reconnect resends B, then A, in wire order, and only then sends C, before
// the connection is published: C's own caller, released the moment it is,
// finds C already sent. The host holds the rows B, A, C.
func TestResendsGoInWireOrderBeforeNewCommands(t *testing.T) {
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	h := newHost(t)
	pausedA, resumeA := make(chan struct{}), make(chan struct{})
	pausedC, resumeC, attemptedC := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { closeOnce(resumeA); closeOnce(resumeC) })
	c := dialHooked(t, h.path, tp, remote.Options{}, remote.TestHooks{
		Registered: func(id string) {
			switch id {
			case "1":
				close(pausedA)
				hold(tp, resumeA, "A's release")
			case "3":
				close(pausedC)
				hold(tp, resumeC, "C's release")
			}
		},
		Attempted: func(id string) {
			if id == "3" {
				close(attemptedC)
			}
		},
		Published: func() {
			// The connection is published: C's caller goes now.
			select {
			case <-pausedC:
				closeOnce(resumeC)
				hold(tp, attemptedC, "C's own attempt")
			default:
			}
		},
	})
	redial := make(chan (<-chan struct{}), 1)
	lost := 0
	tp.setRewriteIn(func(l wireLine) [][]byte {
		if l.conn != 0 || l.method != protocol.MethodQueueAdd || l.resp == nil {
			return nil
		}
		if lost++; lost == 2 {
			redial <- tp.holdDials()
			tp.killConn(0)
		}
		return [][]byte{}
	})
	type answer struct {
		text string
		err  error
	}
	answers := make(chan answer, 3)
	issue := func(text string) {
		go func() {
			_, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: text},
				nil, remote.CommandOptions{})
			answers <- answer{text, err}
		}()
	}
	issue("A")
	await(t, pausedA, "A to pause before its first attempt")
	issue("B")
	// B has run (its reply, lost, is on the wire) before A is sent: the host
	// runs the commands a connection admits on goroutines of their own, so
	// only then are its rows in wire order.
	tp.await(t, "B's reply", func() bool {
		return len(tp.received(func(l wireLine) bool {
			return l.conn == 0 && l.method == protocol.MethodQueueAdd && l.resp != nil
		})) == 1
	})
	close(resumeA)
	await(t, recv(t, redial), "the redial")
	issue("C")
	await(t, pausedC, "C to pause before its first attempt")
	tp.releaseDials()
	for range 3 {
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
	if got := ids(tp.sentOn(0, protocol.MethodQueueAdd)); !slices.Equal(got, []string{"2", "1"}) {
		t.Fatalf("the premise: the first connection's wire order %q, want B (2), A (1)", got)
	}
	if got := ids(tp.sentOn(1, protocol.MethodQueueAdd)); !slices.Equal(got, []string{"2", "1", "3"}) {
		t.Fatalf("the second connection's sends %q, want B (2) and A (1) resent in wire order, then C (3)", got)
	}
	var rows []string
	for _, q := range h.eng.State().Queue {
		rows = append(rows, q.Text)
	}
	if !slices.Equal(rows, []string{"B", "A", "C"}) {
		t.Fatalf("the host's rows %q, want B, A, C", rows)
	}
}

// TestAReconnectEpisodeIsBoundedInTime (X18 5; astra r9 5): the connection
// goes under a command that may have run (a Set parked in the session), and
// every redial connects to a host that never answers hello. The episode ends
// RedialWindow after the loss — each dial and handshake bounded by what is
// left of it, not by the 30 s handshake timeout — and the command resolves
// outcome-unknown, reason disconnected.
func TestAReconnectEpisodeIsBoundedInTime(t *testing.T) {
	h := newHost(t)
	held, release := h.stub.HoldNextSet()
	t.Cleanup(release)
	tp := newTap(t)
	const window = 300 * time.Millisecond
	c := dialClient(t, h.path, tp, remote.Options{RedialWindow: window})
	done := make(chan error, 1)
	go func() {
		_, err := c.Command(tctx(t), protocol.MethodSessionSet, protocol.SetParams{SessionID: h.sid(),
			Setting: protocol.Setting{Kind: protocol.SettingModel, Value: "fast"}}, nil, remote.CommandOptions{})
		done <- err
	}()
	await(t, held, "the Set to park")
	tp.redirect(blackHole(t))
	tp.kill()
	outcomeUnknown(t, recv(t, done), protocol.ReasonDisconnected)
	await(t, c.Done(), "the client to stop")
	if !errors.Is(c.Err(), remote.ErrDisconnected) {
		t.Fatalf("the client stopped with %v", c.Err())
	}
	if n := tp.dialCount(); n < 2 || n > 1+3 {
		t.Fatalf("%d dials, want the first and 1 to 3 redials", n)
	}
	if hellos := tp.sent(protocol.MethodHello); len(hellos) < 2 || hellos[1].conn != 1 {
		t.Fatal("the premise: the redial connected and said hello")
	}
}

// oversized is a line longer than the 16 MiB a host may write.
func oversized() []byte { return bytes.Repeat([]byte("x"), protocol.OutboundLineMax+1) }

// TestAnOversizedLineIsFatalEverywhere (X17 6, X18 7; astra r9 7): a line over
// the 16 MiB a host may write drops the connection wherever it comes — in the
// synchronous hello, where Dial fails, and in the reconnect's sessions.list,
// where the reconnect redials instead of reading on — never skipped for the
// line after it.
func TestAnOversizedLineIsFatalEverywhere(t *testing.T) {
	t.Run("hello", func(t *testing.T) {
		h := newHost(t)
		tp := newTap(t)
		tp.setRewriteIn(func(l wireLine) [][]byte {
			if l.method == protocol.MethodHello && l.resp != nil {
				return [][]byte{oversized(), l.raw}
			}
			return nil
		})
		if _, err := tryDial(t, h.path, tp, remote.Options{}); !errors.Is(err, protocol.ErrLineTooLong) {
			t.Fatalf("Dial with an oversized line before hello's answer: %v", err)
		}
	})
	t.Run("the reconnect's sessions.list", func(t *testing.T) {
		h := newHost(t)
		tp := newTap(t)
		var once sync.Once
		tp.setRewriteIn(func(l wireLine) [][]byte {
			var out [][]byte
			if l.conn == 1 && l.method == protocol.MethodSessionsList && l.resp != nil {
				once.Do(func() { out = [][]byte{oversized(), l.raw} })
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
			t.Fatalf("%d connections, want the first, the one whose sessions.list was oversized, and a redial", tp.connCount())
		}
		if n := len(tp.sentOn(1, protocol.MethodSessionAttach)); n != 0 {
			t.Fatalf("%d attaches on the connection whose sessions.list answer was oversized", n)
		}
	})
}

// TestAMalformedLineDropsTheConnection (X18 7; astra r9 7): a line from the
// host that breaks the protocol — not JSON, or a reply with neither result nor
// error — is never skipped nor taken as an answer: the connection is dropped,
// and the command whose reply it owed is settled by the resend rule — resent
// under its id after the resume, its stored answer coming back; the host ran
// it once.
func TestAMalformedLineDropsTheConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(l wireLine) [][]byte
	}{
		{"not JSON", func(l wireLine) [][]byte { return [][]byte{[]byte("not a message"), l.raw} }},
		{"a reply with neither result nor error", func(l wireLine) [][]byte {
			return [][]byte{[]byte(`{"jsonrpc":"2.0","id":` + l.id + `}`)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			tp := newTap(t)
			var once sync.Once
			tp.setRewriteIn(func(l wireLine) [][]byte {
				var out [][]byte
				if l.conn == 0 && l.method == protocol.MethodQueueAdd && l.resp != nil {
					once.Do(func() { out = tc.edit(l) })
				}
				return out
			})
			c := dialClient(t, h.path, tp, remote.Options{})
			first := c.Hello()
			var row protocol.QueueAddResult
			id, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "once"},
				&row, remote.CommandOptions{})
			if err != nil || len(row.Row) == 0 {
				t.Fatalf("the command: %+v, %v", row, err)
			}
			if tp.connCount() != 2 {
				t.Fatalf("%d connections: the malformed line did not drop the first", tp.connCount())
			}
			if hc := c.Hello(); !hc.Resumed || hc.ClientID != first.ClientID {
				t.Fatalf("the reconnect: %+v", hc)
			}
			adds := tp.sent(protocol.MethodQueueAdd)
			if len(adds) != 2 || commandID(t, adds[1]) != id || adds[1].conn != 1 {
				t.Fatalf("%d sends of the command, want it resent once under its id", len(adds))
			}
			if n := queued(h, "once"); n != 1 {
				t.Fatalf("the host ran the command %d times", n)
			}
		})
	}
}
