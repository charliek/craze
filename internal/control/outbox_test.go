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
	// A final reset uses the reserve, and is queued at once.
	reset := make([]byte, protocol.ResetReserveBytes)
	if err := pushWithDeadline(t, func(ctx context.Context) error { return o.pushReserved(ctx, reset, nil) }); err != nil {
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

func pushWithDeadline(t *testing.T, push func(context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return push(ctx)
}
