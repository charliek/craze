package cli

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// craze wait (plan 041 §3.1, W1) follows one turn of a running session and
// prints its result: plain `wait <id>` the session's current turn — craze's
// own or one the agent runs itself — or, with nothing running, its last turn
// at once (partial: what came before the attach was not seen); `wait <id>
// --from <handle>` exactly the turn or queued row a handle names, from the
// handle's cursor, and owns it: a signal cancels it (§3.1 "Signals"). The
// result is the turn's last assistant message and a footer, or one JSON
// object (--json), and the exit code is its status's (statusExit): 0
// completed or idle, 3 failed, cancelled or dropped, 4 needs_input, 5 running
// at --timeout; 1 is craze's own failure, 2 a usage error.

// waitWhat names craze wait in its lines.
const waitWhat = "craze wait"

type waitFlags struct {
	from       string
	timeout    time.Duration
	maxMessage int
	json       bool
}

// waitSignals is where a waiting command's signals come from: SIGINT, SIGTERM
// and SIGHUP, from signal.Notify (the channel and its stop). A seam: a test
// hands the command a channel of its own and sends on it.
var waitSignals = func() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	return ch, func() { signal.Stop(ch) }
}

func newWaitCmd() *cobra.Command {
	f := &waitFlags{}
	cmd := &cobra.Command{
		Use:   "wait <id>",
		Short: "Wait for a session's turn to end and print its result",
		Long: "craze wait follows the running session <id> names (as craze ps shows it) until its current " +
			"turn ends — or, with nothing running, reports its last turn at once — and prints the turn's last " +
			"message and a footer: its status, time, tool calls and the session. --from follows exactly the " +
			"turn or queued prompt a handle names (a running or needs_input result prints one), and a signal " +
			"then cancels it; plain craze wait never touches the session. The exit code is the status's: 0 " +
			"completed or idle, 3 failed, cancelled or dropped, 4 needs_input, 5 running at --timeout.",
		Args: sessionArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			sigs, stop := waitSignals()
			defer stop()
			return runWait(cmd, f, args, sigs)
		},
	}
	cmd.Flags().StringVar(&f.from, "from", "", "follow exactly the turn or queued prompt this handle names, from its place in the stream; a signal cancels it")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 0, "stop waiting after this long (from the command's start): the result is running, exit 5, with a handle to follow it (default: no limit)")
	cmd.Flags().IntVar(&f.maxMessage, "max-message", 0, "print at most this many characters of the message and the plan together, and write the whole result to a file named in the output (default: no limit)")
	cmd.Flags().BoolVar(&f.json, "json", false, "print the result as one JSON object")
	return cmd
}

// runWait is craze wait (the file's comment): the flags and the id checked
// before anything is dialled, the session resolved, attached — at the
// handle's cursor with --from — and followed (waitTurn), the result printed.
func runWait(cmd *cobra.Command, f *waitFlags, args []string, sigs <-chan os.Signal) error {
	started := time.Now()
	id, err := sessionID(cmd, args)
	if err != nil {
		return err
	}
	if f.timeout < 0 {
		return usagef("craze wait: --timeout must not be negative")
	}
	if f.maxMessage < 0 {
		return usagef("craze wait: --max-message must not be negative")
	}
	var from *turnHandle
	if cmd.Flags().Changed("from") {
		h, err := parseHandle(f.from)
		if err != nil {
			return usagef("craze wait: --from: %v", err)
		}
		from = &h
	}
	stdout := cmd.OutOrStdout()
	// A stdout whose reader has gone fails its write with EPIPE rather than
	// killing the command before its stderr line (craze new's rule).
	ignoreSIGPIPE()
	env := rundir.ProcessEnv()
	out := resultOutput{what: waitWhat, json: f.json, maxMessage: f.maxMessage, env: env}
	entry, err := resolveLive(env, waitWhat, id, true)
	if err == nil {
		id, err = entrySession(waitWhat, entry)
	}
	if err != nil {
		var ee *exitError
		if from != nil && errors.As(err, &ee) && ee.code == 1 {
			// The handle was given, and names the target still: say it, as
			// the attach's failure does, so it is not lost with the error.
			return writeFailure(stdout, out, "", &waitFailure{msg: out.exitMessage(err), handle: from.String()})
		}
		return failJSON(stdout, out, err)
	}
	short := psShort(id)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var deadline time.Time
	if f.timeout > 0 {
		deadline = started.Add(f.timeout)
		var cancelDeadline context.CancelFunc
		ctx, cancelDeadline = context.WithDeadline(ctx, deadline)
		defer cancelDeadline()
	}
	// The attach waits for the session's start, which may never come: a
	// signal ends it as --timeout's end does, and every signal goes on to
	// the waiter once it runs.
	relayed, signalled, stopRelay := relaySignals(sigs, cancel)
	defer stopRelay()
	var cursor *protocol.Cursor
	if from != nil {
		cursor = &protocol.Cursor{Incarnation: from.incarnation, Seq: from.seq}
	}
	conn, err := dialWait(ctx, entry, id, waitWhat, cursor)
	if err != nil {
		if signalled() {
			err = exitf(1, "%s: interrupted before session %s answered", waitWhat, short)
		}
		if from != nil {
			// The target is the handle's still, untouched: say which, so it
			// can be followed once the session answers.
			return writeFailure(stdout, out, short, &waitFailure{msg: out.exitMessage(err), handle: from.String(), follow: true})
		}
		return failJSON(stdout, out, err)
	}
	// The detach as the command exits ends at a signal (one the waiter did
	// not take: it has stopped reading them).
	defer conn.close(relayed)
	waitAttachedHook()
	timeout, stopTimer := timeoutAt(deadline)
	defer stopTimer()
	res, wf := waitTurn(context.Background(), conn, waitStart{from: from}, waitOptions{timeout: timeout, sigs: relayed})
	if wf != nil {
		return writeFailure(stdout, out, short, wf)
	}
	return writeResult(stdout, cmd.ErrOrStderr(), out, res)
}

// waitAttachedHook runs once craze wait's attach has been answered, before
// the wait: a no-op, and the barrier a test sends a signal after when it
// means the wait's, not the attach's.
var waitAttachedHook = func() {}

// relaySignals hands every signal sigs delivers on to the channel it answers
// (the waiter's), and cancels the command's attach (cancel) at the first: a
// session whose start never comes would otherwise keep the attach, and the
// signal, waiting for good. signalled says one came; stop ends the relay.
// The waiting commands' own (C3's new --wait and send --wait spawn and attach
// under it too).
func relaySignals(sigs <-chan os.Signal, cancel context.CancelFunc) (out <-chan os.Signal, signalled func() bool, stop func()) {
	ch := make(chan os.Signal, 4)
	done := make(chan struct{})
	var got atomic.Bool
	var once sync.Once
	go func() {
		for {
			select {
			case s := <-sigs:
				got.Store(true)
				cancel()
				select {
				case ch <- s:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	return ch, got.Load, func() { once.Do(func() { close(done) }) }
}
