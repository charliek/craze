package tool

import (
	"context"
	"errors"
)

// ErrClosing is the cause a session gives when it cancels a running call's
// context because the session itself is closing (context.CancelCauseFunc).
// A ctx cancelled with this cause means the session is closing: tools stop
// at once, with no grace — bash sends SIGKILL to its command, not SIGTERM
// (plan 019 §3.9). A ctx cancelled for any other reason, a user's cancel or
// a deadline, is an ordinary cancel, and a tool may take its grace.
//
// A ctx's cause is fixed by its first cancel, so a call already cancelled
// for another reason cannot be told this way that the session is now
// closing: Env.Closing is how it learns that.
//
// Either way the call still returns its result, class aborted, as Prepared
// promises. Tell a close from a cancel with Closing or SessionClosing, never
// by comparing ctx.Err(): that is context.Canceled for both.
var ErrClosing = errors.New("tool: the session is closing")

// Closing reports whether ctx was cancelled because the session is closing:
// its cause is, or wraps, ErrClosing. It is false for a ctx that is not done.
func Closing(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrClosing)
}

// SessionClosing reports whether the session is closing, by either signal:
// ctx was cancelled with ErrClosing, or closing — the call's Env.Closing —
// is closed. A nil closing never is.
func SessionClosing(ctx context.Context, closing <-chan struct{}) bool {
	if Closing(ctx) {
		return true
	}
	select {
	case <-closing:
		return true
	default:
		return false
	}
}
