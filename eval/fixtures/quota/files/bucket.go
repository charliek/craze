// Package quota is a token-bucket rate limiter for the API gateway.
package quota

import (
	"sync"
	"time"
)

// Bucket is a token bucket. It is safe for concurrent use.
type Bucket struct {
	mu     sync.Mutex
	limits Limits
	tokens float64
	last   time.Time
}

// Option changes a new Bucket's limits.
type Option func(*Limits)

// WithBurst overrides the burst size.
func WithBurst(n int) Option { return func(l *Limits) { l.Burst = n } }

// WithRate overrides the refill rate.
func WithRate(perMinute int) Option { return func(l *Limits) { l.PerMinute = perMinute } }

// New returns a full bucket with Defaults, as changed by opts.
func New(opts ...Option) *Bucket {
	l := Defaults
	for _, o := range opts {
		o(&l)
	}
	return &Bucket{limits: l, tokens: float64(l.Burst)}
}

// Limits returns the bucket's limits.
func (b *Bucket) Limits() Limits { return b.limits }

// Allow takes one token if there is one, refilling first for the time since
// the last call.
func (b *Bucket) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.last.IsZero() && now.After(b.last) {
		b.tokens += now.Sub(b.last).Minutes() * float64(b.limits.PerMinute)
		if max := float64(b.limits.Burst); b.tokens > max {
			b.tokens = max
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
