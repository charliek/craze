package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
)

// craze cancel and craze stop (plan 041 §3.1, W1): the explicit, unowned
// verbs over a running session the id names (resolveLive), each one command
// over a connection of its own that never attaches, as the session list's
// ctrl+x and its stop are (spawn.go's cancelHost, stopHost).
//
//   - craze cancel is session.cancel with no turn id: whatever turn is
//     current, a foreign one included — `cancelled <turn>`, or `nothing to
//     cancel` (exit 0) when nothing runs and no ask is open. Its queue goes on
//     after the cancel (the host's zero chain policy, P8).
//   - craze stop is session.stop: the session ends, its host exits — `stopped
//     <short>` on the stop's receipt.

const (
	cancelWhat = "craze cancel"
	stopWhat   = "craze stop"
	// controlBudget bounds cancel's and stop's dial and command: a cancel
	// waits for the agent's own answer to it.
	controlBudget = 30 * time.Second
)

func newCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a session's current turn",
		Long: "craze cancel cancels the turn the running session <id> names (as craze ps shows it) is working " +
			"on, whoever started it, and prints which; with nothing running it prints nothing to cancel. " +
			"The session and its queue go on.",
		Args: sessionArg,
		RunE: runCancel,
	}
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <id>",
		Short: "End a running session",
		Long: "craze stop ends the running session <id> names (as craze ps shows it): its turn is cancelled, " +
			"its queue cleared and its host exits. It stays in the session index, to be resumed.",
		Args: sessionArg,
		RunE: runStop,
	}
}

// runCancel is craze cancel (the file's comment).
func runCancel(cmd *cobra.Command, args []string) error {
	c, sid, done, err := controlSession(cmd, args, cancelWhat)
	if err != nil {
		return err
	}
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), controlBudget)
	defer cancel()
	var res protocol.CancelResult
	_, err = c.Command(ctx, protocol.MethodSessionCancel, protocol.CancelParams{SessionID: sid}, &res, remote.CommandOptions{})
	out := cmd.OutOrStdout()
	switch {
	case errors.Is(err, engine.ErrNotAccepting):
		// Nothing of craze's runs, the agent runs no turn of its own and no
		// ask is open (engine.Cancel's refusal): nothing was written.
		_, err = fmt.Fprintln(out, "nothing to cancel")
		return err
	case err != nil:
		return exitf(1, "%s: %s", cancelWhat, sanitizeLine(err.Error()))
	case res.Turn == "":
		// The agent's own turn, or an ask open between turns: the receipt
		// names no turn of craze's.
		_, err = fmt.Fprintln(out, "cancelled")
		return err
	}
	_, err = fmt.Fprintf(out, "cancelled %s\n", psLine(res.Turn))
	return err
}

// runStop is craze stop (the file's comment). The session's end can reach
// the connection before the stop's receipt does (the host closes as the
// session ends, remote.Session.Stop's rule): a stop whose answer was lost is
// still `stopped` once no live host serves the session.
func runStop(cmd *cobra.Command, args []string) error {
	c, sid, done, err := controlSession(cmd, args, stopWhat)
	if err != nil {
		return err
	}
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), controlBudget)
	defer cancel()
	_, err = c.Command(ctx, protocol.MethodSessionStop, protocol.StopParams{SessionID: sid}, nil, remote.CommandOptions{})
	var re *remote.Error
	switch {
	case err == nil:
	case errors.Is(err, backend.ErrStopUnsupported):
		return exitf(1, "%s: session %s runs in a craze that cannot stop it from here (its TUI, or an older craze); close it there", stopWhat, psShort(sid))
	case errors.As(err, &re), sessionLive(sid):
		return exitf(1, "%s: %s", stopWhat, sanitizeLine(err.Error()))
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "stopped %s\n", psShort(sid))
	return err
}

// controlSession is cancel's and stop's way to their session: the id
// resolved over the live hosts (resolveLive), the session's full id, and a
// client of its host — dialled within dialTimeout, its peer checked — with
// what closes it.
func controlSession(cmd *cobra.Command, args []string, what string) (*remote.Client, string, func(), error) {
	id, err := sessionID(cmd, args)
	if err != nil {
		return nil, "", nil, err
	}
	entry, err := resolveLive(rundir.ProcessEnv(), what, id, false)
	if err != nil {
		return nil, "", nil, err
	}
	sid, err := entrySession(what, entry)
	if err != nil {
		return nil, "", nil, err
	}
	c, err := dialCLI(context.Background(), entry, what)
	if err != nil {
		return nil, "", nil, unreachable(what, entry, err)
	}
	return c, sid, func() { _ = c.Close() }, nil
}
