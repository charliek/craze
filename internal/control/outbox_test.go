package control

import (
	"context"
	"errors"
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
	if _, err := o.offer(reset, nil, protocol.WriterQueueBytes, false); err != nil {
		t.Fatalf("the reset did not fit the reserve: %v", err)
	}
	if o.highWater() != protocol.WriterQueueBytes {
		t.Fatalf("high water %d, want the whole budget", o.highWater())
	}
	// The writer gives the first line's bytes back: the waiting line gets in.
	if ln, ok := o.next(); !ok || len(ln.b) != limit {
		t.Fatal("the head is not the first line")
	}
	o.written(limit)
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

// TestASealedOutboxAdmitsNothingMore (§3.6, astra r8 7): the line that seals
// the outbox — a replacement's reset — is the last it admits. Every later
// push or offer is refused, errOutboxSealed, whoever makes it; a push already
// waiting for room is woken and refused; and what was queued before the seal,
// the sealing line last, is still handed to the writer in order.
func TestASealedOutboxAdmitsNothingMore(t *testing.T) {
	ctx := context.Background()
	o := newOutbox(nil)
	if err := o.push(ctx, make([]byte, ordinaryLimit), nil); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	entered := make(chan struct{})
	o.full = func() { close(entered) }
	go func() { waited <- o.push(ctx, []byte("a reply waiting for room"), nil) }()
	<-entered
	if _, err := o.offer([]byte("reset"), nil, protocol.WriterQueueBytes, true); err != nil {
		t.Fatalf("the sealing reset: %v", err)
	}
	select {
	case err := <-waited:
		if !errors.Is(err, errOutboxSealed) {
			t.Fatalf("a push waiting across the seal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a push waiting for room outlived the seal")
	}
	o.full = nil
	if err := o.push(ctx, []byte("a later reply"), nil); !errors.Is(err, errOutboxSealed) {
		t.Fatalf("a push after the seal: %v", err)
	}
	if _, err := o.offer([]byte("another reset"), nil, protocol.WriterQueueBytes, false); !errors.Is(err, errOutboxSealed) {
		t.Fatalf("an offer after the seal: %v", err)
	}
	for _, want := range []int{ordinaryLimit, len("reset")} {
		ln, ok := o.next()
		if !ok || len(ln.b) != want {
			t.Fatalf("the writer was handed %d bytes (%v), want %d", len(ln.b), ok, want)
		}
		o.written(len(ln.b))
	}
	o.mu.Lock()
	queued := len(o.q)
	o.mu.Unlock()
	if queued != 0 {
		t.Fatalf("%d lines queued after the sealing one", queued)
	}
}
