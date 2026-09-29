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

// quitStopWait bounds the explicit quit's stop (plan 030 AC3): the receipt,
// and then the session's end, both within it. A variable only so that a test
// can shorten it.
var quitStopWait = 2 * time.Second

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
}

// begin records that the quit is asking for the stop.
func (x *exitState) begin() {
	x.mu.Lock()
	x.stopping = true
	x.mu.Unlock()
}

// answered records what the stop came to.
func (x *exitState) answered(err error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	switch {
	case err == nil:
	case errors.Is(err, backend.ErrStopUnsupported):
		x.unsupported = true
	default:
		x.err = err
	}
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
// comment). Its command id is the model's next, like every command's. What
// requestQuit does first for every quit — the composer's command ended, the
// host-status hub released — it does here too, and then the stop, bounded by
// quitStopWait with the session's end; the program quits once either is in.
// A second quit while the first waits quits at once: the host may still be
// closing, and that is fine.
func (m Model) stopQuit() (tea.Model, tea.Cmd) {
	if m.quitting {
		return m, tea.Quit
	}
	m.quitting = true
	eng, h, sh, x := m.eng, m.host, m.shell, m.exit
	c := m.nextCmd()
	x.begin()
	return m, func() tea.Msg {
		sh.shutdown()
		closeHost(h)
		ctx, cancel := context.WithTimeout(context.Background(), quitStopWait)
		defer cancel()
		err := eng.Stop(ctx, c)
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
