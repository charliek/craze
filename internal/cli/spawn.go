package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// Spawning a detached host (plan 030 §3.4). The ordinary craze runs every
// session in a craze serve of its own and is that host's client (SD-33);
// spawnHost starts the host and waits for it to say it is there, through
// internal/hostspawn's process machinery (plan 032 §3.16). In order:
//
//  1. A host id is minted here and handed to the host (--host-id), with its
//     log, <host-logs>/<hostId>.log — named for the host, as the log sweep
//     requires (hostLogNamed) — so the spawner knows where the host's log and
//     its agents' process-group record are before the host has said a word.
//  2. os.Executable() is re-executed as `craze serve <session flags>`
//     (serveSpec, hostspawn.Args) detached, the write end of a pipe as its
//     fd 3, and one goroutine waits for it (hostspawn.Start).
//  3. The host's one ready line (hostspawn.ReadyLine) is read, bounded
//     (hostspawn.ReadReady). ok — the host is in the registry under its
//     session's craze id — is the ref the caller dials. Anything else ends
//     the host (failSpawn): a timeout, an EOF with no line, a line that is
//     not one or is too long, or the caller's context ending.
//  4. A host refused for a session another host holds answers held: the
//     spawner waits, bounded, for the holder to be in the registry with that
//     session (rendezvous) and hands back the holder for the caller to attach
//     to, and a holder gone meanwhile is spawned past once more. The spawner
//     never claims a session itself: which session a spawn is for is its
//     caller's resolving (loadArg), and the host claims it.
//  5. A host that answered ok and then cannot be dialled is not left running
//     unreached (dialHost): session.stop, or terminate (hostRef.abandon).
//
// Nothing here reports to roost or herdr, and nothing is written to the
// terminal: the caller — the TUI's launch (C4) — says what a failure means.

// spawnStopWait bounds the session.stop sent to a host whose caller could not
// use it (hostRef.abandon), its dial included: a variable only so a test can
// shorten it (never in parallel), as hostspawn's bounds are.
var spawnStopWait = 2 * time.Second

// spawnDial is how dialHost dials a spawned host: remote.DialSession, and a
// test's stand-in that fails.
var spawnDial = remote.DialSession

// spawnOptions is what a host is spawned for.
type spawnOptions struct {
	// env is the registry the rendezvous reads and the cache tree the host's
	// log is in: rundir.ProcessEnv() — the host's own, which it inherits.
	env rundir.Env
	// flags is the launching command line's session flags, settled
	// (tuiFlags.settle): each reaches the host as the flag it is
	// (serveSpec), and --no-host-status with them.
	flags tuiFlags
	// load is --load: loadArg's answer for the row to load, "" for none.
	load string
	// foreign is a host whose provider is not the launch's own (plan 032
	// §3.11, P7; ownsAgentBin): CRAZE_AGENT_BIN is left out of its
	// environment, as the launcher leaves --agent-bin out of flags.
	foreign bool
}

// hostRef is a host a spawn found for its caller: the one it started, which
// answered ok, or — held — the host that holds the session, from the
// registry, for the caller to attach to (plan 030 §3.5's attachHeld). Either
// way entry is what craze attach dials by: the host, its socket, the session.
type hostRef struct {
	entry rundir.Entry
	// held is a session another host holds: entry is that host's, and child
	// is nil — that host is not this spawn's to stop.
	held bool
	// version is the craze the started host said it is, "" when held.
	version string
	// log is the started host's log file, "" when held.
	log   string
	child *hostspawn.Child
}

// spawnFailure is how a spawn failed. A host that refused what it was asked
// to run (refused) and a session another host holds that could not be reached
// (held) are answers about the choice; a host that answered not ok for any
// other reason (notReady) could not come up, as the rest could not, which are
// hosts the spawner ended.
type spawnFailure int

const (
	// spawnNotReady: the host answered not ok — it could not come up, for a
	// reason of its own, its socket not bound among them — its reason in the
	// message, which names its log.
	spawnNotReady spawnFailure = iota + 1
	// spawnRefused: the host refused what it was asked to run
	// (hostspawn.ReadyLine.Refused), its refusal word for word in the message.
	spawnRefused
	// spawnExited: EOF with no line — the host exited, or closed its pipe
	// unwritten, before it was ready.
	spawnExited
	// spawnTimedOut: no line within hostspawn.ReadyWait.
	spawnTimedOut
	// spawnMalformed: a line that is not a ready line.
	spawnMalformed
	// spawnOversized: no line within hostspawn.ReadyLineMax bytes.
	spawnOversized
	// spawnCancelled: the caller's context ended first.
	spawnCancelled
	// spawnHeld: the session's holder could not be attached to.
	spawnHeld
	// spawnStartFailed: the host could not be started at all.
	spawnStartFailed
)

// spawnError is a spawn that found no host for its caller. msg is the whole
// line a caller shows (naming the host's log where there is one), and err
// what it is about: a *rundir.HeldError for held, the context's error for
// cancelled.
type spawnError struct {
	kind spawnFailure
	msg  string
	err  error
}

func (e *spawnError) Error() string { return e.msg }
func (e *spawnError) Unwrap() error { return e.err }

// spawnHost spawns a host for opts and answers the host its caller attaches
// to (the file's doc comment). A held answer is waited on (rendezvous); a
// holder gone meanwhile is spawned past once — the claim it released is the
// new host's to take — and a second holder gone the same way is a failure.
func spawnHost(ctx context.Context, opts spawnOptions) (hostRef, error) {
	var held *hostspawn.ReadyHeld
	for range 2 {
		ref, h, err := spawnOnce(ctx, opts)
		if err != nil || h == nil {
			return ref, err
		}
		held = h
		ref, err = rendezvous(ctx, opts.env, held)
		if !errors.Is(err, hostspawn.ErrHolderGone) {
			return ref, err
		}
	}
	herr := held.HeldError()
	return hostRef{}, &spawnError{kind: spawnHeld, msg: "craze: " + refusal(herr) + ", and its holder went while craze waited for it, twice — try again", err: herr}
}

// spawnOnce starts one host and reads its answer: its ref when ok, who holds
// the session when held (the host has exited, or is terminated), or why not.
func spawnOnce(ctx context.Context, opts spawnOptions) (hostRef, *hostspawn.ReadyHeld, error) {
	dir, err := rundir.HostLogDir(opts.env)
	if err != nil {
		return hostRef{}, nil, &spawnError{kind: spawnStartFailed, msg: "craze: the session host cannot be started: the host logs' directory: " + err.Error(), err: err}
	}
	hostID := rundir.NewHostID()
	logPath := filepath.Join(dir, hostID+".log")
	var unset []string
	if opts.foreign {
		unset = []string{envAgentBin}
	}
	child, r, err := hostspawn.Start(hostspawn.Args(serveSpec(&opts.flags, opts.load, hostID, logPath)), unset, filepath.Join(dir, hostspawn.AgentGroupsName(hostID)), logPath)
	if err != nil {
		return hostRef{}, nil, &spawnError{kind: spawnStartFailed, msg: "craze: the session host cannot be started: " + err.Error(), err: err}
	}
	line, failure, why := hostspawn.ReadReady(ctx, r)
	_ = r.Close()
	if failure != 0 {
		return hostRef{}, nil, failSpawn(ctx, child, readFailure(failure), why, logPath)
	}
	if !line.OK {
		// The host said why itself and is exiting; it is given the grace to,
		// and ended if it takes longer.
		child.Settle()
		if line.Held != nil {
			return hostRef{}, line.Held, nil
		}
		if line.Refused {
			return hostRef{}, nil, &spawnError{kind: spawnRefused, msg: line.Error, err: errors.New(line.Error)}
		}
		// A host that could not come up says why in the host's own words,
		// less its name for itself, and where its log is.
		why := strings.TrimPrefix(strings.TrimPrefix(line.Error, "craze serve: "), "craze: ")
		return hostRef{}, nil, &spawnError{kind: spawnNotReady, msg: "craze: the session host could not start: " + why + "; its log: " + logPath, err: errors.New(line.Error)}
	}
	// Its form is hostspawn.ParseReady's; whose it is, this spawn's.
	if line.HostID != hostID {
		return hostRef{}, nil, failSpawn(ctx, child, spawnMalformed, fmt.Sprintf("it names host %q, not %q", line.HostID, hostID), logPath)
	}
	entry := rundir.Entry{
		Protocol:       protocol.ProtocolVersion,
		HostID:         hostID,
		PID:            child.PID(),
		Socket:         line.Socket,
		CrazeSessionID: line.CrazeSessionID,
	}
	// The registry has the rest — the provider and the workspace a client's
	// fallback info shows before its attach is answered — since ok follows
	// the entry's identity write.
	if e, ok := hostEntry(opts.env, hostID); ok && e.CrazeSessionID == line.CrazeSessionID && e.Socket == line.Socket {
		entry = e
	}
	return hostRef{entry: entry, version: line.CrazeVersion, log: logPath, child: child}, nil, nil
}

// serveSpec is craze serve's command line for a spawn (hostspawn.Args): the
// host's id and log, then every session flag f carries — each passed only
// when it says something, so the host resolves what the launcher left to
// resolving (the provider, the workspace) exactly as the launcher would, from
// the same environment and config — and --load, and the launching TUI's
// --no-host-status.
func serveSpec(f *tuiFlags, load, hostID, logPath string) hostspawn.Spec {
	return hostspawn.Spec{
		HostID:       hostID,
		Log:          logPath,
		Workspace:    f.workspace,
		Provider:     f.provider,
		Model:        f.model,
		Effort:       strings.TrimSpace(f.effort),
		Fast:         f.fastSetting(),
		AgentBin:     f.agentBin,
		PluginDirs:   f.pluginDirs,
		NoForce:      !f.force || f.noForce,
		Ask:          f.ask,
		Plan:         f.plan,
		Continue:     f.cont,
		Load:         load,
		NoHostStatus: f.noHostStatus,
	}
}

// readFailure is the ready line's read failure (hostspawn.ReadReady) as a
// spawn's.
func readFailure(f hostspawn.Failure) spawnFailure {
	switch f {
	case hostspawn.Exited:
		return spawnExited
	case hostspawn.TimedOut:
		return spawnTimedOut
	case hostspawn.Oversized:
		return spawnOversized
	case hostspawn.Cancelled:
		return spawnCancelled
	}
	return spawnMalformed
}

// loadArg is the --load a spawn for row passes (plan 030 §3.3, §3.12, R2-6):
// its craze id, or for a legacy row that has none its provider and its
// provider's session id — the host gives it its craze id under its own claim.
func loadArg(row sessions.Row) string {
	if row.CrazeID != "" {
		return row.CrazeID
	}
	return row.Provider + ":" + row.SessionID
}

// hostEntry is host id's live registry entry.
func hostEntry(env rundir.Env, hostID string) (rundir.Entry, bool) {
	entries, err := rundir.Hosts(env)
	if err != nil {
		return rundir.Entry{}, false
	}
	for _, e := range entries {
		if e.HostID == hostID {
			return e, true
		}
	}
	return rundir.Entry{}, false
}

// failSpawn ends a host that gave no usable answer and says what happened,
// naming its log, where it said whatever it could. A host that closed its
// pipe unwritten is exiting, or has: it is given the grace to (Settle), and
// its exit status is part of the answer; any other is terminated. A cancelled
// spawn carries ctx's error.
func failSpawn(ctx context.Context, c *hostspawn.Child, failure spawnFailure, why, logPath string) error {
	if failure == spawnExited {
		c.Settle()
	} else {
		c.Terminate()
	}
	var msg string
	switch failure {
	case spawnExited:
		msg = "the session host exited before it was ready"
		if err := c.Err(); err != nil {
			msg += " (" + err.Error() + ")"
		}
	case spawnTimedOut:
		msg = "the session host was not ready within " + hostspawn.ReadyWait.String()
	case spawnOversized:
		msg = fmt.Sprintf("the session host's ready line is longer than %d bytes", hostspawn.ReadyLineMax)
	case spawnCancelled:
		msg = "the session host's start was abandoned"
	default:
		msg = "the session host's ready line cannot be read: " + why
	}
	err := errors.New(msg)
	if failure == spawnCancelled {
		err = ctx.Err()
	}
	return &spawnError{kind: failure, msg: "craze: " + msg + "; its log: " + logPath, err: err}
}

// rendezvous finds the host holding a session a spawn was refused
// (hostspawn.Rendezvous): the holder serving it is the ref its caller
// attaches to, held; a holder gone is hostspawn.ErrHolderGone, which spawnHost
// spawns past; a holder still there at the bound is refused as the picker
// refuses it — serving no socket, or another session — with the
// *rundir.HeldError; and a caller gone first is a cancelled spawn.
func rendezvous(ctx context.Context, env rundir.Env, held *hostspawn.ReadyHeld) (hostRef, error) {
	e, err := hostspawn.Rendezvous(ctx, env, held)
	herr := held.HeldError()
	switch {
	case err == nil:
		return hostRef{entry: e, held: true}, nil
	case errors.Is(err, hostspawn.ErrHolderGone):
		return hostRef{}, err
	case errors.Is(err, hostspawn.ErrHolderElsewhere):
		return hostRef{}, &spawnError{kind: spawnHeld, msg: "craze: " + refusal(herr) + holderElsewhere.refusalSuffix(), err: herr}
	case errors.Is(err, hostspawn.ErrHolderNoSocket):
		return hostRef{}, &spawnError{kind: spawnHeld, msg: "craze: " + refusal(herr) + holderNoSocket.refusalSuffix(), err: herr}
	}
	return hostRef{}, &spawnError{kind: spawnCancelled, msg: "craze: the session host's start was abandoned", err: err}
}

// dialHost dials the host a spawn found, as its caller's client (opts), and
// for a host the spawn started that cannot be dialled ends it (abandon): a
// host nobody reached is a host nobody would ever stop. A held ref's host is
// its holder's, and is left alone.
func dialHost(ctx context.Context, ref hostRef, opts remote.SessionOptions) (*remote.Session, error) {
	s, err := spawnDial(ctx, ref.entry.Socket, opts)
	if err != nil {
		ref.abandon()
		return nil, err
	}
	return s, nil
}

// abandon ends a host this spawn started that its caller could not use (plan
// 030 §3.4: a failed dial after the handshake): session.stop over a
// connection of its own, bounded by spawnStopWait, then hostspawn.TermGrace
// for the host's stop sequence; a stop that cannot be sent — the socket
// unreachable, or refused — or a host still there after the grace, is
// terminated. It returns once the host is gone. Nothing for a held ref.
func (r hostRef) abandon() {
	c := r.child
	if c == nil || r.held {
		return
	}
	if c.Exited() {
		c.KillAgents()
		return
	}
	if stopHost(r.entry) == nil && c.WaitExit(hostspawn.TermGrace) {
		c.KillAgents()
		return
	}
	c.Terminate()
}

// stopHost sends session.stop to the host e names, over a connection of its
// own, and waits for its receipt: nil once the host has taken it.
func stopHost(e rundir.Entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), spawnStopWait)
	defer cancel()
	c, err := remote.Dial(ctx, e.Socket, remote.Options{PeerCheck: rundir.DialCheck(os.Geteuid())})
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_, err = c.Command(ctx, protocol.MethodSessionStop, protocol.StopParams{SessionID: e.CrazeSessionID}, nil, remote.CommandOptions{})
	return err
}

// cancelHost is the session list's ctrl+x on a working or asking row (plan
// 030 §3.10): over a connection of its own, as stopHost's — it never
// attaches — the queue of the session e names is cleared, and then its
// running turn is cancelled, so the turn settles into an empty queue and
// nothing starts behind it (in the other order the settlement would start
// the queue's head in the same step). Both are the protocol's own commands;
// the connection's client mints their ids. The whole is bounded by
// spawnStopWait. A cancel the host refuses not_accepting found nothing left
// to cancel — the turn ended on its own between the two — and is not an
// error: the queue is cleared and nothing runs, which is what was asked.
func cancelHost(e rundir.Entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), spawnStopWait)
	defer cancel()
	c, err := remote.Dial(ctx, e.Socket, remote.Options{PeerCheck: rundir.DialCheck(os.Geteuid())})
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Command(ctx, protocol.MethodQueueClear, protocol.QueueClearParams{SessionID: e.CrazeSessionID}, nil, remote.CommandOptions{}); err != nil {
		return err
	}
	_, err = c.Command(ctx, protocol.MethodSessionCancel, protocol.CancelParams{SessionID: e.CrazeSessionID}, nil, remote.CommandOptions{})
	if errors.Is(err, engine.ErrNotAccepting) {
		return nil
	}
	return err
}
