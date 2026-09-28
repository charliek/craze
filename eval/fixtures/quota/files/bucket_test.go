package quota

import (
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

func TestBurstAllowsTenRequests(t *testing.T) {
	b := New()
	for i := 1; i <= 10; i++ {
		if !b.Allow(t0) {
			t.Fatalf("request %d was refused, want allowed", i)
		}
	}
	if b.Allow(t0) {
		t.Fatal("request 11 was allowed, want refused")
	}
}

func TestRefillAfterAMinute(t *testing.T) {
	b := New(WithBurst(2), WithRate(60))
	b.Allow(t0)
	b.Allow(t0)
	if b.Allow(t0) {
		t.Fatal("empty bucket allowed a request")
	}
	if !b.Allow(t0.Add(time.Second)) {
		t.Fatal("one second at 60/min should refill one token")
	}
}

func TestConcurrentAllow(t *testing.T) {
	b := New(WithBurst(50))
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Allow(t0) {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 50 {
		t.Fatalf("allowed %d of 100 concurrent requests, want 50", allowed)
	}
}
