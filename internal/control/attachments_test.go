package control_test

import (
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/protocol"
)

// OnAttachments and the count it reports (plan 030 §3.6; attach.go's
// "Counting attachments"): every attachment from its reservation to its close,
// reported in order; a connection that only lists is not attached; and a
// peer's read EOF takes its attachment out of the count — the half-close
// hazard C1 found — without changing what the half-closed peer is delivered.

// attachLog is every count a server's OnAttachments reported, in order.
type attachLog struct {
	mu     sync.Mutex
	counts []int
	grew   chan struct{}
}

// watchAttachments starts recording h's counts.
func watchAttachments(h *host) *attachLog {
	l := &attachLog{grew: make(chan struct{})}
	h.srv.OnAttachments(func(n int) {
		l.mu.Lock()
		l.counts = append(l.counts, n)
		close(l.grew)
		l.grew = make(chan struct{})
		l.mu.Unlock()
	})
	return l
}

// all is every count so far.
func (l *attachLog) all() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int(nil), l.counts...)
}

// waitLen waits, within the watchdog, until n counts have been reported, and
// answers them.
func (l *attachLog) waitLen(t *testing.T, n int) []int {
	t.Helper()
	deadline := time.After(watchdog)
	for {
		l.mu.Lock()
		got := append([]int(nil), l.counts...)
		grew := l.grew
		l.mu.Unlock()
		if len(got) >= n {
			return got
		}
		select {
		case <-grew:
		case <-deadline:
			t.Fatalf("OnAttachments reported %v; want %d reports within %s", got, n, watchdog)
		}
	}
}

// wantCounts fails unless the reports so far are exactly want.
func (l *attachLog) wantCounts(t *testing.T, what string, want ...int) {
	t.Helper()
	got := l.waitLen(t, len(want))
	if len(got) != len(want) {
		t.Fatalf("%s: OnAttachments reported %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: OnAttachments reported %v, want %v", what, got, want)
		}
	}
}

// TestOnAttachmentsHearsEveryChangeInOrder: eight clients attach and detach
// three times each, all at once. Every change is reported, one call at a time
// and in the order the count moved — each report one more or one less than
// the last, from 0 — and the count ends at 0 with every client detached.
func TestOnAttachmentsHearsEveryChangeInOrder(t *testing.T) {
	h := newHost(t)
	l := watchAttachments(h)
	const clients, rounds = 8, 3
	var wg sync.WaitGroup
	for range clients {
		c := h.dial()
		c.sayHello(nil)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				res := c.attach(protocol.AttachParams{SessionID: sid(h)})
				_, resp := c.until(c.send(protocol.MethodSessionDetach, protocol.DetachParams{SessionID: sid(h), Subscription: res.Subscription}))
				ok[protocol.Empty](t, resp)
			}
		}()
	}
	wg.Wait()
	got := l.waitLen(t, 2*clients*rounds)
	prev := 0
	for i, n := range got {
		if n != prev+1 && n != prev-1 {
			t.Fatalf("report %d is %d after %d: the calls are out of order: %v", i, n, prev, got)
		}
		if n < 0 || n > clients {
			t.Fatalf("report %d is %d: %v", i, n, got)
		}
		prev = n
	}
	if prev != 0 || h.srv.Attached() != 0 {
		t.Fatalf("the count ends at %d (Attached %d), want 0: %v", prev, h.srv.Attached(), got)
	}
}

// TestAListPollerIsNotAttached: a connection kept open that only says hello
// and lists — the agent view's poller — moves nothing; its own attach then
// counts, and its detach uncounts.
func TestAListPollerIsNotAttached(t *testing.T) {
	h := newHost(t)
	l := watchAttachments(h)
	c := h.dial()
	c.sayHello(nil)
	for range 3 {
		ok[protocol.SessionsListResult](t, c.call(protocol.MethodSessionsList, protocol.SessionsListParams{}))
	}
	if got := l.all(); len(got) != 0 || h.srv.Attached() != 0 {
		t.Fatalf("a poller moved the count: %v, Attached %d", got, h.srv.Attached())
	}
	res := c.attach(protocol.AttachParams{SessionID: sid(h)})
	l.wantCounts(t, "attached", 1)
	_, resp := c.until(c.send(protocol.MethodSessionDetach, protocol.DetachParams{SessionID: sid(h), Subscription: res.Subscription}))
	ok[protocol.Empty](t, resp)
	l.wantCounts(t, "detached", 1, 0)
}

// TestAPendingAttachIsCounted: an attach counts from its reservation, before
// it is answered — held there (TestHooks.Reserved), it is already reported —
// and a close fence raised meanwhile reads it.
func TestAPendingAttachIsCounted(t *testing.T) {
	reserved := make(chan struct{})
	resume := make(chan struct{})
	h := newHost(t, withHooks(control.TestHooks{Reserved: func(string) {
		close(reserved)
		<-resume
	}}))
	l := watchAttachments(h)
	c := h.dial()
	c.sayHello(nil)
	id := c.send(protocol.MethodSessionAttach, protocol.AttachParams{SessionID: sid(h)})
	await(t, reserved, "the attach to reserve")
	l.wantCounts(t, "pending", 1)
	n, release := h.srv.FenceAttaches()
	release()
	if n != 1 {
		t.Fatalf("a close fence counts %d attachments with one pending", n)
	}
	close(resume)
	ok[protocol.AttachResult](t, c.reply(id))
	if got := l.all(); len(got) != 1 {
		t.Fatalf("answering a pending attach moved the count: %v", got)
	}
}

// TestAHalfClosedPeerLeavesTheCount: an attached client that half-closes its
// socket leaves the count at once — and a close fence reads 0 — while its
// subscription still delivers what the session says afterwards (S2's
// half-close, unchanged). A second client's attach counts from there.
func TestAHalfClosedPeerLeavesTheCount(t *testing.T) {
	h := newHost(t)
	l := watchAttachments(h)
	a := h.dial()
	a.sayHello(nil)
	a.attach(protocol.AttachParams{SessionID: sid(h)})
	l.wantCounts(t, "attached", 1)
	if err := a.nc.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	l.wantCounts(t, "half-closed", 1, 0)
	if n, release := h.srv.FenceAttaches(); n != 0 {
		release()
		t.Fatalf("a close fence counts %d attachments with the only one half-closed", n)
	} else {
		release()
	}
	h.stub.Emit(agent.Event{Type: agent.EventText, Text: "still delivered"})
	for {
		n := a.anyNote()
		if n.Method != protocol.NotifyEvent {
			continue
		}
		if _, ev := eventOf(t, n); ev.Type == agent.EventText && ev.Text == "still delivered" {
			break
		}
	}
	b := h.dial()
	b.sayHello(nil)
	b.attach(protocol.AttachParams{SessionID: sid(h)})
	l.wantCounts(t, "a second client", 1, 0, 1)
}

// TestAKilledClientLeavesTheCountOfAnIdleSession is C1's hazard itself: a
// client killed while attached to an idle session — its socket closed whole,
// nothing written to it since — leaves the count as soon as the host reads
// its EOF, without waiting for a write that would fail.
func TestAKilledClientLeavesTheCountOfAnIdleSession(t *testing.T) {
	h := newHost(t)
	l := watchAttachments(h)
	a := h.dial()
	a.sayHello(nil)
	a.attach(protocol.AttachParams{SessionID: sid(h)})
	l.wantCounts(t, "attached", 1)
	a.close()
	l.wantCounts(t, "killed", 1, 0)
	if h.srv.Attached() != 0 {
		t.Fatalf("Attached is %d after the only client was killed", h.srv.Attached())
	}
}
