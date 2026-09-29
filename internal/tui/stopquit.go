package tui

import (
	"context"
	"errors"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/backend"
)

// The explicit quit over a backend served elsewhere (plan 030 §3.6, the
// owner's decision 11): /exit, Ctrl+D and the second Ctrl+C — the one quit
// path, requestQuit — end the session this TUI shows, in every client, craze
// attach's included. The TUI asks the session's host to stop it
// (backend.Backend.Stop: session.stop) and waits, up to quitStopWait, for the
// session's end, then quits; a host still closing after that is fine. Every
// client attached is sent the session's end; this one, which asked, says
// nothing about it (Result.Stopped).
//
// A host that cannot stop its session — an older craze, or a TUI-hosted one
// (the opt-out's socket) — refuses the stop, stop_unsupported
// (backend.ErrStopUnsupported): nothing stopped, and the quit detaches
// instead, as every quit did before, the command line saying so once the
// screen is back (Result.StopUnsupported).
//
// Only the explicit quit stops. A SIGTERM to this process, and its terminal
// closing (SIGHUP, which Run turns into SIGTERM's quit), reach finishRun
// without requestQuit, and close the backend — a view close: the session runs
// on. An engine this TUI built itself (the in-process opt-out) is closed by
// its quit exactly as before.

// quitStopWait bounds the explicit quit (plan 030 AC3; C5r): one deadline,
// taken when the quit is asked for, across the stop's receipt, the wait for
// the session's end and the backend's close after them (closeBackend) — and a
// second quit ends the wait and closes at once. A variable only so that a
// test can shorten it.
var quitStopWait = 2 * time.Second

// quitJoinWait bounds the wait for a stop the quit's deadline cut short
// (awaitStop), once the backend's transport is closed: a stop over a socket
// ends as soon as that close ends its write, so this only bites on a backend
// whose stop does not end with its transport — the quit goes on without it.
const quitJoinWait = time.Second

// exitState is what the explicit quit came to (stopQuit), shared by every copy
// of the model (Model.exit): written by the quit's own tea.Cmd, read by Run
// once the program has ended.
type exitState struct {
	mu sync.Mutex
	// stopping says the quit asked the host to stop the session;
	// unsupported that the host refused, stop_unsupported; err a stop
	// answered neither way.
	stopping    bool
	unsupported bool
	err         error
	// deadline is the quit's one bound (quitStopWait from the moment it was
	// asked for), and again says a second quit came while the first still
	// waited: the backend's close then waits for nothing (closeBackend), and
	// the stop's own answer, which that close cuts short, is not recorded —
	// the stop was on its way when the program ended.
	deadline time.Time
	again    bool
}

// begin records that the quit is asking for the stop, and starts its one
// deadline.
func (x *exitState) begin(now time.Time) time.Time {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.stopping = true
	x.deadline = now.Add(quitStopWait)
	return x.deadline
}

// quitAgain records a second quit while the first still waits.
func (x *exitState) quitAgain() {
	if x == nil {
		return
	}
	x.mu.Lock()
	x.again = true
	x.mu.Unlock()
}

// answered records what the stop came to — unless a second quit has ended
// the wait for it (quitAgain).
func (x *exitState) answered(err error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.again {
		return
	}
	switch {
	case err == nil:
	case errors.Is(err, backend.ErrStopUnsupported):
		x.unsupported = true
	default:
		x.err = err
	}
}

// quitCloser is a backend whose close can be bounded by a context
// (remote.Session.CloseWithin): the detach waits no longer than ctx allows,
// and none at all once it is done.
type quitCloser interface {
	CloseWithin(ctx context.Context) error
}

// closeBackend is finishRun's close of the backend the program ended with.
// After an explicit quit that asked its host to stop the session, the close
// shares the quit's one deadline (plan 030 C5r, astra r8-c5 4): a detach —
// after a stop the host refused, or one that failed — waits only for what is
// left of it; none once it has passed, or once a second quit came; and then
// the transport is closed. Without that, a host that withheld its stop's
// receipt used the quit's two seconds and then a fresh detach wait of the
// client's own. Every other exit closes as it always has.
func (x *exitState) closeBackend(eng backend.Backend) error {
	var deadline time.Time
	again := false
	if x != nil {
		x.mu.Lock()
		if x.stopping {
			deadline, again = x.deadline, x.again
		}
		x.mu.Unlock()
	}
	c, ok := eng.(quitCloser)
	if !ok || deadline.IsZero() {
		return eng.Close()
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if again {
		cancel()
	}
	return c.CloseWithin(ctx)
}

// outcome is Run's reading of it: stopped for a stop asked and not refused —
// taken, or still on its way when the program ended (the session's end that
// quit it then was this stop's) — unsupported for the refusal, and err for a
// stop that failed. A zero model's nil state is none of them.
func (x *exitState) outcome() (stopped, unsupported bool, err error) {
	if x == nil {
		return false, false, nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.stopping && !x.unsupported && x.err == nil, x.unsupported, x.err
}

// ender is a backend that says when its stream has ended (remote.Session's
// Ended): what the quit waits on after the stop's receipt.
type ender interface {
	Ended() <-chan struct{}
}

// stopQuit is requestQuit for a backend served elsewhere (the file's doc
// comment). Its command id is the model's next, like every command's. Its one
// deadline starts here, as the quit is asked for (exitState.begin), and bounds
// the stop's receipt, the wait for the session's end, and the backend's close
// once the program has ended (closeBackend): the stop is sent at once, on a
// goroutine of its own, beside what requestQuit does first for every quit —
// the composer's command ended, the host-status hub released — so neither
// spends the stop's time (plan 030 C5r); the program quits once the session
// has ended or the deadline has passed — the stop's answer included, which
// the deadline ends whether or not the stop can see it (awaitStop). The
// composer's command is the one wait past it: nothing it started may outlive
// craze, and its shutdown has a bound of its own (shellShutdownWait), spent
// only while a command runs. A second quit while the first waits quits at
// once, and the close that follows detaches nothing (quitAgain): the host may
// still be closing, and that is fine.
func (m Model) stopQuit() (tea.Model, tea.Cmd) {
	if m.quitting {
		m.exit.quitAgain()
		return m, tea.Quit
	}
	m.quitting = true
	eng, h, sh, x := m.eng, m.host, m.shell, m.exit
	c := m.nextCmd()
	deadline := x.begin(time.Now())
	return m, func() tea.Msg {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		stopped := make(chan error, 1)
		go func() { stopped <- eng.Stop(ctx, c) }()
		sh.shutdown()
		closeHost(h)
		err := awaitStop(ctx, eng, stopped)
		x.answered(err)
		if e, ok := eng.(ender); ok && err == nil {
			select {
			case <-e.Ended():
			case <-ctx.Done():
			}
		}
		return tea.Quit()
	}
}

// awaitStop is the stop's answer (stopQuit), waited for no longer than the
// quit's deadline, ctx's: an answer already there when the wait begins is
// taken whatever the clock says, since the quit's own cleanup may have run
// past the deadline while the stop was answered in time.
//
// A stop still out at the deadline is answered ctx.Err() — neither way, as a
// withheld receipt is — and is not left running (plan 030 C5r2, astra
// r9-fix45 1). The stop cannot be relied on to see its context: a host that
// has stopped reading lets an outstanding command's line fill the socket, and
// the stop's own request then waits for the connection's write lock, or in a
// write of its own, where no context reaches it. So the transport is closed
// — a close whose deadline has passed detaches nothing (quitCloser), the one
// finishRun would make — which ends every write on it, and every command
// waiting there with it; and the stop is joined, for at most quitJoinWait.
// finishRun's close after it is then a no-op. A backend with no such close
// has no transport of the TUI's to end: its stop is joined for the same
// bound, and the quit goes on without it.
func awaitStop(ctx context.Context, eng backend.Backend, stopped <-chan error) error {
	select {
	case err := <-stopped:
		return err
	default:
	}
	select {
	case err := <-stopped:
		return err
	case <-ctx.Done():
	}
	if c, ok := eng.(quitCloser); ok {
		_ = c.CloseWithin(ctx)
	}
	join := time.NewTimer(quitJoinWait)
	defer join.Stop()
	select {
	case <-stopped:
	case <-join.C:
	}
	return ctx.Err()
}
