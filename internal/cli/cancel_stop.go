package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// craze cancel and craze stop (plan 041 §3.1, W1): the explicit, unowned
// verbs over a session the id names, each one command over a connection of its
// own that never attaches, as the session list's ctrl+x and its stop are
// (spawn.go's cancelHost, stopHost). Both are safe to send again: what they
// answer is a word for where the session stands (controlResult), not whether
// this call was the one that did it.
//
//   - craze cancel is session.cancel: with no --turn whatever turn is current, a
//     foreign one included — `cancelled <turn>`, or `nothing to cancel` (exit 0)
//     when nothing runs and no ask is open. --turn is session.cancel's turnId, a
//     turn of craze's own that the host holds against its current one in the
//     section that takes the cancel, so a retried cancel can never reach the
//     next queued turn: `turn_not_current`, exit 0. Its queue goes on after the
//     cancel (the host's zero chain policy, P8).
//   - craze stop is session.stop: the session ends, its host exits — `stopped
//     <short>` on the stop's receipt, or on finding that the host went under it.
//     A session the index says has ended is exit 0 too (`already_ended`).
//
// An id is resolved over the live hosts, then the session index (resolveControl),
// so an unknown id, an ambiguous one and an index that cannot be read stay apart.
// Plain craze cancel on an ended session stays exit 1: only stop's exit changed.

const (
	cancelWhat = "craze cancel"
	stopWhat   = "craze stop"
	// controlBudget bounds cancel's and stop's dial and command: a cancel
	// waits for the agent's own answer to it.
	controlBudget = 30 * time.Second
)

// What craze stop and craze cancel answer (controlResult.Result): exit 0 for
// where a session stands, exit 1 for what kept the command from finding out.
const (
	resultStopped         = "stopped"
	resultCancelled       = "cancelled"
	resultNothingToCancel = "nothing_to_cancel"
	resultTurnNotCurrent  = "turn_not_current"
	resultAlreadyEnded    = "already_ended"
	resultNotFound        = "not_found"
	resultAmbiguous       = "ambiguous"
	resultIndexError      = "index_error"
	// A session neither command could reach (its host running, or not started
	// yet), one whose host cannot stop it from here (a TUI's), and a command the
	// host refused.
	resultUnreachable = "unreachable"
	resultUnsupported = "unsupported"
	resultFailed      = "failed"
)

// controlResult is craze stop's and craze cancel's answer under --json.
type controlResult struct {
	Session string `json:"session"`
	Short   string `json:"short"`
	Turn    string `json:"turn,omitempty"`
	Result  string `json:"result"`
}

// controlFailure is their exit-1 answer under --json: the waiting commands'
// {"error":{…}} with the same result word beside it, and the session once the id
// was resolved to one.
type controlFailure struct {
	Error   *resultError `json:"error"`
	Session string       `json:"session,omitempty"`
	Short   string       `json:"short,omitempty"`
	Result  string       `json:"result"`
}

type controlFlags struct {
	json bool
	turn string
}

func newCancelCmd() *cobra.Command {
	f := &controlFlags{}
	cmd := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a session's current turn",
		Long: "craze cancel cancels the turn the running session <id> names (as craze ps shows it) is working " +
			"on, whoever started it, and prints which; with nothing running it prints nothing to cancel. " +
			"The session and its queue go on. With --turn it cancels that turn only while it is current, so " +
			"a cancel sent again never reaches the next queued turn.",
		Args: sessionArg,
		RunE: func(cmd *cobra.Command, args []string) error { return runCancel(cmd, f, args) },
	}
	cmd.Flags().BoolVar(&f.json, "json", false, "print the outcome as one JSON object: session, short, turn, result")
	cmd.Flags().StringVar(&f.turn, "turn", "", "cancel this turn only (a result's turn, turn-3), and only while it is the current one: anything else cancels nothing and exits 0")
	return cmd
}

func newStopCmd() *cobra.Command {
	f := &controlFlags{}
	cmd := &cobra.Command{
		Use:   "stop <id>",
		Short: "End a running session",
		Long: "craze stop ends the running session <id> names (as craze ps shows it): its turn is cancelled, " +
			"its queue cleared and its host exits. It stays in the session index, to be resumed. A session " +
			"that has ended already is exit 0.",
		Args: sessionArg,
		RunE: func(cmd *cobra.Command, args []string) error { return runStop(cmd, f, args) },
	}
	cmd.Flags().BoolVar(&f.json, "json", false, "print the outcome as one JSON object: session, short, result")
	return cmd
}

// controlOutput is how one craze stop or craze cancel answers.
type controlOutput struct {
	cmd  *cobra.Command
	what string
	json bool
}

// done is an answer of exit 0: result as one object, or line as itself.
func (o controlOutput) done(sid, turn, result, line string) error {
	out := o.cmd.OutOrStdout()
	if !o.json {
		_, err := fmt.Fprintln(out, line)
		return err
	}
	b, err := json.Marshal(controlResult{Session: sid, Short: psShort(sid), Turn: turn, Result: result})
	if err != nil {
		return kindf(errkind.Internal, "%s: the outcome: %v", o.what, err)
	}
	_, err = fmt.Fprintf(out, "%s\n", b)
	return err
}

// failed is an answer of exit 1, err's own line on stderr, and under --json its
// object first.
func (o controlOutput) failed(sid, result string, err error) error {
	if o.json {
		body := controlFailure{Error: kindError(o.exitMessage(err), failureKind(err)), Session: sid, Short: psShort(sid), Result: result}
		if b, merr := json.Marshal(body); merr == nil {
			fmt.Fprintf(o.cmd.OutOrStdout(), "%s\n", b)
		}
	}
	return err
}

// exitMessage is err's words without the command's name, as an object says them.
func (o controlOutput) exitMessage(err error) string {
	return strings.TrimPrefix(err.Error(), o.what+": ")
}

// controlTarget is what an id came to: a live host's entry, or — live false — the
// ended session the index holds a row for.
type controlTarget struct {
	entry rundir.Entry
	sid   string
	live  bool
}

// resolveControl is the session id names, for stop and cancel: the live hosts by
// matchSession's tiers, then the session index by matchCrazeID's. An id that is
// no token is a usage error; anything else that resolves to nothing says which it
// is (o.failed): an unknown id, an ambiguous one, or a registry or index that
// could not be read, or read whole, which places nothing. The live hosts come
// first, so a row the index still holds for a session a host now serves is that
// host's.
func (o controlOutput) resolveControl(id string) (controlTarget, error) {
	if !rundir.ValidToken(id) {
		return controlTarget{}, usagef("%s: that is not a session id (craze ps lists them)", o.what)
	}
	entries, unreadable, err := rundir.HostsScan(rundir.ProcessEnv())
	if err != nil {
		return controlTarget{}, o.failed("", resultIndexError, kindf(errkind.Internal, "%s: %s", o.what, sanitizeLine(err.Error())))
	}
	switch matches := matchSession(entries, id); len(matches) {
	case 1:
		sid, err := entrySession(o.what, matches[0])
		if err != nil {
			return controlTarget{}, o.failed("", resultUnreachable, err)
		}
		return controlTarget{entry: matches[0], sid: sid, live: true}, nil
	case 0:
	default:
		return controlTarget{}, o.failed("", resultAmbiguous, kindf(errkind.Usage, "%s: %d sessions match %s: %s", o.what, len(matches), id, sanitizeLine(formatEntries(matches))))
	}
	// A scan that missed a live host establishes nothing of an id it did not find:
	// that host may be the one serving it.
	if len(unreadable) > 0 {
		return controlTarget{}, o.failed("", resultIndexError, kindf(errkind.Internal, "%s: the registry entry of a running host cannot be read (%s), so session %s may be running",
			o.what, sanitizeLine(strings.Join(unreadable, ", ")), id))
	}
	rows, err := (&sessions.Store{}).ByCrazeID()
	if err != nil {
		return controlTarget{}, o.failed("", resultIndexError, kindf(errkind.Internal, "%s: the session index: %s", o.what, sanitizeLine(err.Error())))
	}
	ids := make([]string, 0, len(rows))
	for crazeID := range rows {
		ids = append(ids, crazeID)
	}
	switch matches := matchCrazeID(ids, id); len(matches) {
	case 1:
		return controlTarget{sid: matches[0]}, nil
	case 0:
		return controlTarget{}, o.failed("", resultNotFound, kindf(errkind.Config, "%s: no session %s", o.what, id))
	default:
		return controlTarget{}, o.failed("", resultAmbiguous, kindf(errkind.Usage, "%s: the session has ended: %d ended sessions match %s", o.what, len(matches), id))
	}
}

// controlDialling sees a live host about to be dialled, and controlDialled it
// dialled, its command about to be sent: no-ops, and the barriers a test ends that
// host at, between the lookup and the dial, and between the dial and the command.
var (
	controlDialling = func(rundir.Entry) {}
	controlDialled  = func(rundir.Entry) {}
)

// hostWent says a command that got no answer from e found a host that has gone or
// is going: its process exited, no registry entry for its session in a scan that
// missed none (sessionLive), or its entry marked stopping, which a host does before
// it closes any connection. None is a failure to report: the session ended under the
// command.
func hostWent(e rundir.Entry, sid string) bool {
	return readHost(e) == hostExited || !sessionLive(sid) || hostMarkedStopping(e)
}

// runCancel is craze cancel (the file's comment).
func runCancel(cmd *cobra.Command, f *controlFlags, args []string) error {
	o := controlOutput{cmd: cmd, what: cancelWhat, json: f.json}
	id, err := sessionID(cmd, args)
	if err != nil {
		return err
	}
	if cmd.Flags().Changed("turn") && strings.TrimSpace(f.turn) == "" {
		return usagef("%s: --turn needs a turn's id, as a result's turn says it (turn-3)", cancelWhat)
	}
	t, err := o.resolveControl(id)
	if err != nil {
		return err
	}
	ended := func() error {
		if !f.json {
			return kindf(errkind.Stopped, "%s: the session has ended", cancelWhat)
		}
		return o.done(t.sid, "", resultAlreadyEnded, "")
	}
	if !t.live {
		return ended()
	}
	controlDialling(t.entry)
	ctx, cancel := context.WithTimeout(context.Background(), controlBudget)
	defer cancel()
	c, err := dialCLI(ctx, t.entry, cancelWhat)
	if err != nil {
		if hostWent(t.entry, t.sid) {
			return ended()
		}
		return o.failed(t.sid, resultUnreachable, unreachable(cancelWhat, t.entry, err))
	}
	defer func() { _ = c.Close() }()
	controlDialled(t.entry)
	var res protocol.CancelResult
	_, err = c.Command(ctx, protocol.MethodSessionCancel, protocol.CancelParams{SessionID: t.sid, TurnID: f.turn}, &res, remote.CommandOptions{})
	var re *remote.Error
	switch {
	case f.turn != "" && (errors.Is(err, engine.ErrStaleTurn) || errors.Is(err, engine.ErrNotAccepting)):
		return o.done(t.sid, f.turn, resultTurnNotCurrent, psLine(f.turn)+" is not the current turn; nothing was cancelled")
	case errors.Is(err, engine.ErrNotAccepting):
		// Nothing of craze's runs, the agent runs no turn of its own and no
		// ask is open (engine.Cancel's refusal): nothing was written.
		return o.done(t.sid, "", resultNothingToCancel, "nothing to cancel")
	case err != nil && !errors.As(err, &re) && hostWent(t.entry, t.sid):
		return ended()
	case err != nil:
		return o.failed(t.sid, resultFailed, kindf(errkind.Internal, "%s: %s", cancelWhat, sanitizeLine(err.Error())))
	case res.Turn == "":
		// The agent's own turn, or an ask open between turns: the receipt
		// names no turn of craze's.
		return o.done(t.sid, "", resultCancelled, "cancelled")
	}
	return o.done(t.sid, res.Turn, resultCancelled, "cancelled "+psLine(res.Turn))
}

// runStop is craze stop (the file's comment). The session's end can reach the
// connection before the stop's receipt does (the host closes as the session ends,
// remote.Session.Stop's rule), and the host can go on its own between the lookup
// and the command: a stop that finds no host left to answer is the session ended,
// never a failure. stopped when the host took this stop or was going under it,
// already_ended when it had gone before anything was sent.
func runStop(cmd *cobra.Command, f *controlFlags, args []string) error {
	o := controlOutput{cmd: cmd, what: stopWhat, json: f.json}
	id, err := sessionID(cmd, args)
	if err != nil {
		return err
	}
	t, err := o.resolveControl(id)
	if err != nil {
		return err
	}
	short := psShort(t.sid)
	ended := func() error {
		return o.done(t.sid, "", resultAlreadyEnded, "session "+short+" has already ended")
	}
	if !t.live {
		return ended()
	}
	controlDialling(t.entry)
	ctx, cancel := context.WithTimeout(context.Background(), controlBudget)
	defer cancel()
	c, err := dialCLI(ctx, t.entry, stopWhat)
	if err != nil {
		if hostWent(t.entry, t.sid) {
			return ended()
		}
		return o.failed(t.sid, resultUnreachable, unreachable(stopWhat, t.entry, err))
	}
	defer func() { _ = c.Close() }()
	controlDialled(t.entry)
	_, err = c.Command(ctx, protocol.MethodSessionStop, protocol.StopParams{SessionID: t.sid}, nil, remote.CommandOptions{})
	var re *remote.Error
	switch {
	case err == nil, stopRacedItsEnd(err):
	case errors.Is(err, backend.ErrStopUnsupported):
		return o.failed(t.sid, resultUnsupported, kindf(errkind.Internal, "%s: session %s runs in a craze that cannot stop it from here (its TUI, or an older craze); close it there", stopWhat, short))
	case errors.As(err, &re), !hostWent(t.entry, t.sid):
		return o.failed(t.sid, resultFailed, kindf(errkind.Internal, "%s: %s", stopWhat, sanitizeLine(err.Error())))
	}
	return o.done(t.sid, "", resultStopped, "stopped "+short)
}

// stopRacedItsEnd says a host's refusal of a stop is its session ending under it.
func stopRacedItsEnd(err error) bool {
	return errors.Is(err, engine.ErrNotAccepting) || errors.Is(err, engine.ErrClosing) || errors.Is(err, remote.ErrSessionEnded)
}
