package tool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// held is how many paths have an entry: none may outlive its holders.
func (l *PathLocks) held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.paths)
}

func TestPathLockSerializesOnePath(t *testing.T) {
	var l PathLocks
	unlock, err := l.Lock(context.Background(), "/ws/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	// The same file under another spelling waits; another file does not.
	got := make(chan struct{})
	go func() {
		u, err := l.Lock(context.Background(), "/ws/sub/../a.txt")
		if err == nil {
			close(got)
			u()
		}
	}()
	other, err := l.Lock(context.Background(), "/ws/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	other()
	select {
	case <-got:
		t.Fatal("a second holder took a held path")
	case <-time.After(50 * time.Millisecond):
	}
	if n := l.held(); n != 1 {
		t.Fatalf("held = %d with one path held and one waiting, want 1", n)
	}
	unlock()
	unlock() // a second call is a no-op
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter never got the lock")
	}
	waitFor(t, func() bool { return l.held() == 0 })
}

func TestPathLockCancelWhileWaiting(t *testing.T) {
	var l PathLocks
	unlock, err := l.Lock(context.Background(), "/ws/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := l.Lock(ctx, "/ws/a.txt")
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait = %v, want context.Canceled", err)
	}
	// The waiter's reference is gone; the holder's is not.
	if n := l.held(); n != 1 {
		t.Fatalf("held = %d, want 1", n)
	}
	unlock()
	if n := l.held(); n != 0 {
		t.Fatalf("held = %d after the last release, want 0", n)
	}
	// An already-cancelled context never takes even a free lock.
	if _, err := l.Lock(ctx, "/ws/free.txt"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Lock with a done context = %v", err)
	}
	if _, err := l.Lock(context.Background(), "relative.txt"); err == nil {
		t.Fatal("a relative path was locked")
	}
	if n := l.held(); n != 0 {
		t.Fatalf("held = %d, want 0", n)
	}
}

// TestPathLockExcludes: many goroutines on one path, never two inside at
// once; under -race, the lock is also the only thing guarding the counter.
func TestPathLockExcludes(t *testing.T) {
	var l PathLocks
	var inside, maxInside atomic.Int32
	counter := 0
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			unlock, err := l.Lock(context.Background(), "/ws/shared")
			if err != nil {
				t.Error(err)
				return
			}
			n := inside.Add(1)
			for {
				m := maxInside.Load()
				if n <= m || maxInside.CompareAndSwap(m, n) {
					break
				}
			}
			counter++
			inside.Add(-1)
			unlock()
		})
	}
	wg.Wait()
	if counter != 50 || maxInside.Load() != 1 || l.held() != 0 {
		t.Fatalf("counter %d, most inside at once %d, held %d; want 50, 1, 0", counter, maxInside.Load(), l.held())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(time.Millisecond)
	}
}
