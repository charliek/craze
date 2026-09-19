package tool

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestClosing: a ctx cancelled with ErrClosing, directly, wrapped, or through
// a parent, is closing; a live ctx, a plain cancel, another cause, and a
// deadline are not — the negative controls.
func TestClosing(t *testing.T) {
	cancelled := func(cause error) context.Context {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		return ctx
	}
	parent, cancelParent := context.WithCancelCause(context.Background())
	child, cancelChild := context.WithCancel(parent)
	defer cancelChild()
	cancelParent(ErrClosing)
	deadline, cancelDeadline := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelDeadline()
	<-deadline.Done()

	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"ErrClosing", cancelled(ErrClosing), true},
		{"wrapped ErrClosing", cancelled(fmt.Errorf("session 3: %w", ErrClosing)), true},
		{"a child of a closing ctx", child, true},
		{"not done", context.Background(), false},
		{"a plain cancel", cancelled(nil), false},
		{"another cause", cancelled(fmt.Errorf("user pressed Esc")), false},
		{"a deadline", deadline, false},
	}
	for _, tc := range cases {
		if got := Closing(tc.ctx); got != tc.want {
			t.Errorf("%s: Closing = %v, want %v", tc.name, got, tc.want)
		}
		// With no channel, SessionClosing is Closing.
		if got := SessionClosing(tc.ctx, nil); got != tc.want {
			t.Errorf("%s: SessionClosing(nil) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSessionClosing: a closed Env.Closing means the session is closing,
// whatever ctx says — a ctx already cancelled for another reason included,
// which is the case the channel exists for. An open channel is the control.
func TestSessionClosing(t *testing.T) {
	cancelled, cancel := context.WithCancelCause(context.Background())
	cancel(fmt.Errorf("user pressed Esc"))
	open, closed := make(chan struct{}), make(chan struct{})
	close(closed)
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		closing <-chan struct{}
		want    bool
	}{
		{"live ctx, open channel", context.Background(), open, false},
		{"cancelled ctx, open channel", cancelled, open, false},
		{"live ctx, closed channel", context.Background(), closed, true},
		{"cancelled ctx, closed channel", cancelled, closed, true},
	} {
		if got := SessionClosing(tc.ctx, tc.closing); got != tc.want {
			t.Errorf("%s: SessionClosing = %v, want %v", tc.name, got, tc.want)
		}
	}
}
