package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
)

// TestTheOutboxHoldsItsBudgetAndItsReserve (§3.7, astra 10): the writer
// queue's own rules, below the socket. Every queued byte counts until the
// writer gives it back; an ordinary line waits while it would take the
// budget past WriterQueueBytes less ResetReserveBytes; a final reset
// (pushReserved) may use the reserve, so it is queued at once beside a full
// budget; a line that fits an empty queue always gets in; and every wait ends
// when the outbox closes.
func TestTheOutboxHoldsItsBudgetAndItsReserve(t *testing.T) {
	ctx := context.Background()
	o := newOutbox(nil)
	limit := protocol.WriterQueueBytes - protocol.ResetReserveBytes

	// A line the size of the whole ordinary budget fits an empty queue.
	if err := o.push(ctx, make([]byte, limit), nil); err != nil {
		t.Fatal(err)
	}
	// One more byte does not fit: it waits.
	waited := make(chan error, 1)
	go func() { waited <- o.push(ctx, []byte{'x'}, nil) }()
	select {
	case err := <-waited:
		t.Fatalf("a line past the budget was queued: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	// A final reset uses the reserve, and is queued at once: offered the whole
	// budget, it never waits.
	reset := make([]byte, protocol.ResetReserveBytes)
	if _, err := o.offer(reset, nil, protocol.WriterQueueBytes, ordinaryLine); err != nil {
		t.Fatalf("the reset did not fit the reserve: %v", err)
	}
	if o.highWater() != protocol.WriterQueueBytes {
		t.Fatalf("high water %d, want the whole budget", o.highWater())
	}
	// The writer gives the first line's bytes back: the waiting line gets in.
	ln, ok := o.next()
	if !ok || len(ln.b) != limit {
		t.Fatal("the head is not the first line")
	}
	o.written(ln)
	if err := <-waited; err != nil {
		t.Fatalf("the waiting line: %v", err)
	}

	// Every wait ends when the outbox closes.
	full := newOutbox(nil)
	if err := full.push(ctx, make([]byte, limit), nil); err != nil {
		t.Fatal(err)
	}
	go func() { waited <- full.push(ctx, []byte{'x'}, nil) }()
	time.Sleep(10 * time.Millisecond)
	full.close()
	select {
	case err := <-waited:
		if !errors.Is(err, errOutboxClosed) {
			t.Fatalf("a wait ended by the close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a wait for room outlived the close")
	}
	if _, ok := full.next(); ok {
		t.Fatal("a closed outbox handed the writer a line")
	}
}

// TestAReplacedOutboxAdmitsOnlyItsTerminalLine (§3.6, plan 027 X25; astra r8
// 7, sol r16): the outbox's own half of the terminal-only rule. When it is
// replaced, every ordinary line it holds is dropped — handed back, in order,
// for the caller to run their callbacks — while a terminal one it holds (a
// claimed detach's `{}`, queued before) is kept, and a line the writer had
// already taken is left to the writer; a push waiting for room is woken and
// refused, and so is every later ordinary line, whoever offers it. The first
// terminal line offered after it is admitted and seals it: nothing more is.
// The writer is handed the kept line and the sealing one, in order, and
// nothing else; owesTerminal holds until both are written, and every byte
// comes back to the budget.
func TestAReplacedOutboxAdmitsOnlyItsTerminalLine(t *testing.T) {
	ctx := context.Background()
	o := newOutbox(nil)
	var ran []string
	callback := func(name string) func() { return func() { ran = append(ran, name) } }

	// The writer has taken a first reply and is writing it.
	if err := o.push(ctx, []byte("reply 0"), callback("reply 0")); err != nil {
		t.Fatal(err)
	}
	taken, ok := o.next()
	if !ok {
		t.Fatal("the writer was handed nothing")
	}
	// Behind it: a reply filling most of the budget, a claimed detach's {}
	// (terminal: it does not seal an outbox that is not replaced), and a
	// reply after it.
	if err := o.push(ctx, make([]byte, ordinaryLimit-64), callback("reply 1")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.offer([]byte("{}"), callback("detach"), ordinaryLimit, terminalLine); err != nil {
		t.Fatal(err)
	}
	if err := o.push(ctx, []byte("reply 2"), callback("reply 2")); err != nil {
		t.Fatalf("a reply after a terminal line, not yet replaced: %v", err)
	}
	// A reply waiting for room.
	waited := make(chan error, 1)
	entered := make(chan struct{})
	o.full = func() { close(entered) }
	go func() { waited <- o.push(ctx, make([]byte, 128), callback("reply 3")) }()
	<-entered
	o.full = nil

	dropped := o.replace()
	select {
	case err := <-waited:
		if !errors.Is(err, errOutboxReplaced) {
			t.Fatalf("a push waiting across the replacement: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a push waiting for room outlived the replacement")
	}
	for _, ln := range dropped {
		ln.done()
	}
	if want := []string{"reply 1", "reply 2"}; !slices.Equal(ran, want) {
		t.Fatalf("the replacement dropped %v, want %v", ran, want)
	}
	if again := o.replace(); again != nil {
		t.Fatalf("a second replacement dropped %d lines", len(again))
	}

	// Every later ordinary line is refused: a reply, and a reset queued as an
	// ordinary line with the whole budget.
	if err := o.push(ctx, []byte("a later reply"), nil); !errors.Is(err, errOutboxReplaced) {
		t.Fatalf("a push after the replacement: %v", err)
	}
	if _, err := o.offer([]byte("reset{slow_consumer}"), nil, protocol.WriterQueueBytes, ordinaryLine); !errors.Is(err, errOutboxReplaced) {
		t.Fatalf("an ordinary offer after the replacement: %v", err)
	}
	// The terminal line is admitted, and seals it.
	if _, err := o.offer([]byte("reset{session_replaced}"), nil, protocol.WriterQueueBytes, terminalLine); err != nil {
		t.Fatalf("the terminal line: %v", err)
	}
	if _, err := o.offer([]byte("another terminal line"), nil, protocol.WriterQueueBytes, terminalLine); !errors.Is(err, errOutboxSealed) {
		t.Fatalf("a terminal line after the seal: %v", err)
	}
	if err := o.push(ctx, []byte("a reply after the seal"), nil); !errors.Is(err, errOutboxSealed) {
		t.Fatalf("a push after the seal: %v", err)
	}

	// The writer finishes the line it had taken, then is handed the kept {}
	// and the reset, and nothing more.
	o.written(taken)
	for _, want := range []string{"{}", "reset{session_replaced}"} {
		if !o.owesTerminal() {
			t.Fatalf("owes no terminal line with %q unwritten", want)
		}
		ln, ok := o.next()
		if !ok || string(ln.b) != want {
			t.Fatalf("the writer was handed %q (%v), want %q", ln.b, ok, want)
		}
		o.written(ln)
	}
	if o.owesTerminal() {
		t.Fatal("owes a terminal line with both written")
	}
	o.mu.Lock()
	queued, bytes := len(o.q), o.bytes
	o.mu.Unlock()
	if queued != 0 || bytes != 0 {
		t.Fatalf("%d lines and %d bytes left after the terminal lines", queued, bytes)
	}
}

// TestAReplacementOwingNothingClosesAtOnce (§3.6, plan 027 X25; sol r16 item
// 5): a connection with no attachment has two replies unwritten when its
// engine is replaced — the first taken by the writer and held there, off the
// socket (beforeWrite), the second queued behind it. It owes no terminal
// line, so it closes at once: the peer reads EOF, and nothing before it,
// while the writer still holds the first reply — no drain of what was queued
// — the second reply's slot is given back by the replacement that dropped
// it, and the first's once the writer, let go, fails to write it; the writer
// returns.
func TestAReplacementOwingNothingClosesAtOnce(t *testing.T) {
	s := New(Options{})
	held, release := make(chan struct{}), make(chan struct{})
	var holding atomic.Bool
	s.hooks.beforeWrite = func([]byte) {
		if holding.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	}
	nc, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	c := newConn(s, nc)
	s.transport.Add(1)
	go c.write()
	for i := range 2 {
		if !c.acquire() {
			t.Fatal("no admission slot")
		}
		c.reply(json.RawMessage(strconv.Itoa(i+1)), protocol.Empty{})
	}
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never took the first reply")
	}
	c.out.mu.Lock()
	queued := len(c.out.q)
	c.out.mu.Unlock()
	if queued != 1 {
		t.Fatalf("the premise: %d replies queued behind the held one, want 1", queued)
	}

	c.replace()
	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := peer.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("with the writer holding a reply, the peer read %d bytes, %v: want EOF at once", n, err)
	}
	if got := len(c.slots); got != 1 {
		t.Fatalf("%d slots held after the replacement, want only the writer's held reply's", got)
	}

	close(release)
	joined := make(chan struct{})
	go func() { s.transport.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer did not return once let go")
	}
	c.mu.Lock()
	inflight := c.inflight
	c.mu.Unlock()
	if got := len(c.slots); got != 0 || inflight != 0 {
		t.Fatalf("%d slots held, %d admitted, after the writer let go of the held reply", got, inflight)
	}
}

// TestAClosedOutboxReleasesWhatItDrops (§3.6; astra r18 item 3 on C7e
// a3fd956, pre-existing): a plain close — Server.Close, a write failure, the
// engine's end — must release what it drops, exactly as conn.replace's drop
// list already does. A connection with no attachment has two replies
// unwritten when it closes: the first taken by the writer and held there,
// off the socket (beforeWrite), the second still queued. conn.close hands
// that second reply back for its callback to run — giving back its
// admission slot and inflight count — at once, without waiting for the
// writer, which conn.close never joins.
func TestAClosedOutboxReleasesWhatItDrops(t *testing.T) {
	s := New(Options{})
	held, release := make(chan struct{}), make(chan struct{})
	var holding atomic.Bool
	s.hooks.beforeWrite = func([]byte) {
		if holding.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	}
	nc, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	c := newConn(s, nc)
	s.transport.Add(1)
	go c.write()
	for i := range 2 {
		if !c.acquire() {
			t.Fatal("no admission slot")
		}
		c.reply(json.RawMessage(strconv.Itoa(i+1)), protocol.Empty{})
	}
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never took the first reply")
	}
	c.out.mu.Lock()
	queued := len(c.out.q)
	c.out.mu.Unlock()
	if queued != 1 {
		t.Fatalf("the premise: %d replies queued behind the held one, want 1", queued)
	}

	c.close("connection closed")
	c.mu.Lock()
	inflight := c.inflight
	c.mu.Unlock()
	if got := len(c.slots); got != 1 || inflight != 1 {
		t.Fatalf("%d slots held, %d admitted, right after close: want the writer's held reply's alone (1, 1)", got, inflight)
	}

	close(release)
	joined := make(chan struct{})
	go func() { s.transport.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer did not return once let go")
	}
	c.mu.Lock()
	inflight = c.inflight
	c.mu.Unlock()
	if got := len(c.slots); got != 0 || inflight != 0 {
		t.Fatalf("%d slots held, %d admitted, after the writer let go of the held reply", got, inflight)
	}
}
