package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// Spawning a detached host (plan 030 §3.4). The ordinary craze runs every
// session in a craze serve of its own and is that host's client (SD-33);
// spawnHost starts the host and waits for it to say it is there. In order:
//
//  1. A host id is minted here and handed to the host (--host-id), with its
//     log, <host-logs>/<hostId>.log — named for the host, as the log sweep
//     requires (hostLogNamed) — so the spawner knows where the host's log and
//     its agents' process-group record are before the host has said a word.
//  2. os.Executable() is re-executed as `craze serve <session flags>` in a
//     session of its own (setsid), stdin, stdout and stderr on /dev/null, the
//     write end of a pipe as its fd 3 (CRAZE_READY_FD=3) and CRAZE_HOST_CHILD=1
//     marking it. The spawner closes its own copy of the write end at once, so
//     a host that dies before answering is EOF here at once, not a timeout.
//     One goroutine owns every started child's Wait from then on: setsid does
//     not reparent, so a host that exits while its launcher lives would
//     otherwise be a zombie until the launcher exits.
//  3. The host's one ready line (readyLine, ready.go) is read, bounded by
//     spawnReadyWait, at most readyLineMax bytes. ok — the host is in the
//     registry under its session's craze id — is the ref the caller dials.
//     Anything else ends the host (hostChild.terminate): a timeout, an EOF
//     with no line, a line that is not one or is too long, or the caller's
//     context ending.
//  4. A host refused for a session another host holds answers held: the
//     spawner waits, bounded, for the holder to be in the registry with that
//     session (rendezvous) and hands back the holder for the caller to attach
//     to, and a holder gone meanwhile is spawned past once more. The spawner
//     never claims a session itself: which session a spawn is for is its
//     caller's resolving (loadArg), and the host claims it.
//  5. A host that answered ok and then cannot be dialled is not left running
//     unreached (dialHost): session.stop, or terminate (hostRef.abandon).
//
// Linux and macOS alike: Setsid and ExtraFiles mean the same on both (the
// first extra file is fd 3 in the child), and so do close-on-exec and group
// signals. Nothing here reports to roost or herdr, and nothing is written to
// the terminal: the caller — the TUI's launch (C4) — says what a failure
// means.

// The handshake's bounds: variables only so a test can shorten them (never in
// parallel).
var (
	// spawnReadyWait is how long a host has to answer (plan 030 §3.4: 10 s).
	spawnReadyWait = 10 * time.Second
	// spawnTermGrace is how long a host sent SIGTERM has to run its stop
	// sequence — the engine's close ends its agent — before its process
	// group is killed, and the recorded agent groups with it; and how long
	// a host that answered not ok has to exit on its own.
	spawnTermGrace = 5 * time.Second
	// spawnRendezvousWait bounds the held rendezvous: the holder's registry
	// entry must carry the session within it.
	spawnRendezvousWait = 5 * time.Second
	// spawnRendezvousPoll is how often the rendezvous reads the registry.
	spawnRendezvousPoll = 20 * time.Millisecond
	// spawnStopWait bounds the session.stop sent to a host whose caller
	// could not use it (hostRef.abandon), its dial included.
	spawnStopWait = 2 * time.Second
)

// hostCommand is the command a host is spawned with, argv craze serve's (its
// first word "serve"): os.Executable() run with argv, in production. A test
// runs its own binary as craze instead (serve_child_test.go). The spawner sets
// the rest — the session, stdio, fd 3, the environment's marks — on what it
// returns; an Env left nil is craze's own environment.
var hostCommand = func(argv []string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(exe, argv...), nil
}

// spawnPipe is the ready pipe's constructor, os.Pipe: a seam for the test that
// checks no descendant of the host holds it.
var spawnPipe = os.Pipe

// spawnReadyTimer is the ready wait's timer, time.After: a seam a test fires
// by hand, so a host that never answers is timed out exactly when the test
// has seen it serving, however slowly it got there.
var spawnReadyTimer = time.After

// spawnPolled is told each read of the registry the held rendezvous makes: a
// seam that lets a test act while the rendezvous is known to be waiting; a
// no-op in production.
var spawnPolled = func() {}

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
	// (serveArgv), and --no-host-status with them.
	flags tuiFlags
	// load is --load: loadArg's answer for the row to load, "" for none.
	load string
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
	child *hostChild
}

// spawnFailure is how a spawn failed. A host that refused (notReady) and a
// session another host holds that could not be reached (held) are the host's
// answers; the rest are hosts the spawner ended.
type spawnFailure int

const (
	// spawnNotReady: the host answered not ok, its refusal in the message.
	spawnNotReady spawnFailure = iota + 1
	// spawnExited: EOF with no line — the host exited, or closed its pipe
	// unwritten, before it was ready.
	spawnExited
	// spawnTimedOut: no line within spawnReadyWait.
	spawnTimedOut
	// spawnMalformed: a line that is not a ready line.
	spawnMalformed
	// spawnOversized: no line within readyLineMax bytes.
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
	var held *readyHeld
	for range 2 {
		ref, h, err := spawnOnce(ctx, opts)
		if err != nil || h == nil {
			return ref, err
		}
		held = h
		ref, err = rendezvous(ctx, opts.env, held)
		if !errors.Is(err, errHolderGone) {
			return ref, err
		}
	}
	herr := held.heldError()
	return hostRef{}, &spawnError{kind: spawnHeld, msg: "craze: " + refusal(herr) + ", and its holder went while craze waited for it, twice — try again", err: herr}
}

// spawnOnce starts one host and reads its answer: its ref when ok, who holds
// the session when held (the host has exited, or is terminated), or why not.
func spawnOnce(ctx context.Context, opts spawnOptions) (hostRef, *readyHeld, error) {
	dir, err := rundir.HostLogDir(opts.env)
	if err != nil {
		return hostRef{}, nil, &spawnError{kind: spawnStartFailed, msg: "craze: the session host cannot be started: the host logs' directory: " + err.Error(), err: err}
	}
	hostID := rundir.NewHostID()
	logPath := filepath.Join(dir, hostID+".log")
	child, r, err := startHostChild(serveArgv(&opts.flags, opts.load, hostID, logPath), filepath.Join(dir, agentGroupsName(hostID)), logPath)
	if err != nil {
		return hostRef{}, nil, &spawnError{kind: spawnStartFailed, msg: "craze: the session host cannot be started: " + err.Error(), err: err}
	}
	line, failure, why := readReady(ctx, r)
	_ = r.Close()
	if failure != 0 {
		return hostRef{}, nil, child.fail(ctx, failure, why, logPath)
	}
	if !line.OK {
		// The host said why itself and is exiting; it is given the grace to,
		// and ended if it takes longer.
		child.settle()
		if line.Held != nil {
			return hostRef{}, line.Held, nil
		}
		return hostRef{}, nil, &spawnError{kind: spawnNotReady, msg: line.Error, err: errors.New(line.Error)}
	}
	// Its form is parseReady's; whose it is, this spawn's.
	if line.HostID != hostID {
		return hostRef{}, nil, child.fail(ctx, spawnMalformed, fmt.Sprintf("it names host %q, not %q", line.HostID, hostID), logPath)
	}
	entry := rundir.Entry{
		Protocol:       protocol.ProtocolVersion,
		HostID:         hostID,
		PID:            child.pid,
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

// serveArgv is craze serve's command line for a spawn: the host's id and log,
// then every session flag f carries — each only when it says something, so
// the host resolves what the launcher left to resolving (the provider, the
// workspace) exactly as the launcher would, from the same environment and
// config — and --load, and the launching TUI's --no-host-status. Every value
// is spelled --flag=value, so none is read as a flag of its own.
func serveArgv(f *tuiFlags, load, hostID, logPath string) []string {
	argv := []string{"serve", "--host-id=" + hostID, "--log=" + logPath}
	str := func(name, v string) {
		if v != "" {
			argv = append(argv, "--"+name+"="+v)
		}
	}
	str("workspace", f.workspace)
	str("provider", f.provider)
	str("model", f.model)
	str("agent-bin", f.agentBin)
	for _, d := range f.pluginDirs {
		argv = append(argv, "--plugin-dir="+d)
	}
	if !f.force || f.noForce {
		argv = append(argv, "--no-force")
	}
	if f.ask {
		argv = append(argv, "--ask")
	}
	if f.plan {
		argv = append(argv, "--plan")
	}
	if f.cont {
		argv = append(argv, "--continue")
	}
	str("load", load)
	if f.noHostStatus {
		argv = append(argv, "--no-host-status")
	}
	return argv
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

// hostChild is a host this process started, and the one goroutine that waits
// for it.
type hostChild struct {
	cmd *exec.Cmd
	pid int
	// groups is the host's record of its agents' process groups
	// (agentGroups, ready.go).
	groups string
	// log is the host's log, where the spawner notes a recorded group it
	// did not kill (note).
	log  string
	done chan struct{}
	err  error // cmd.Wait's, once done is closed
}

// startHostChild starts `craze serve argv…` detached (the file's doc comment,
// step 2) and answers it with the read end of its ready pipe. groups and log
// are the host's record of its agents and its log.
func startHostChild(argv []string, groups, log string) (*hostChild, *os.File, error) {
	cmd, err := hostCommand(argv)
	if err != nil {
		return nil, nil, err
	}
	r, w, err := spawnPipe()
	if err != nil {
		return nil, nil, err
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(withoutEnv(env, readyFDEnv, hostChildEnv), readyFDEnv+"=3", hostChildEnv+"=1")
	// stdin, stdout and stderr nil are /dev/null: the host writes to its log.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.ExtraFiles = []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	// Ours closed at once, whatever Start said: the host's copy is the only
	// write end left, so its exit is EOF here.
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	c := &hostChild{cmd: cmd, pid: cmd.Process.Pid, groups: groups, log: log, done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	return c, r, nil
}

// readReady reads the host's ready line from r: the line, or why there is
// none — a failure and what it was — within spawnReadyWait, or until ctx
// ends. The read runs on a goroutine of its own, which the caller's close of
// r ends.
func readReady(ctx context.Context, r *os.File) (readyLine, spawnFailure, string) {
	type result struct {
		line    readyLine
		failure spawnFailure
		why     string
	}
	got := make(chan result, 1)
	go func() {
		line, failure, why := parseReady(r)
		got <- result{line, failure, why}
	}()
	select {
	case res := <-got:
		return res.line, res.failure, res.why
	case <-spawnReadyTimer(spawnReadyWait):
		return readyLine{}, spawnTimedOut, ""
	case <-ctx.Done():
		return readyLine{}, spawnCancelled, ""
	}
}

// parseReady reads one ready line from r: every byte up to its newline, at
// most readyLineMax with it, decoded. EOF before any byte is a host that
// exited, or closed the pipe unwritten, before it was ready; EOF within a line
// is a malformed one.
func parseReady(r io.Reader) (readyLine, spawnFailure, string) {
	br := bufio.NewReaderSize(io.LimitReader(r, readyLineMax+1), 4096)
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > readyLineMax {
			return readyLine{}, spawnOversized, ""
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(buf) == 0 && errors.Is(err, io.EOF) {
			return readyLine{}, spawnExited, ""
		}
		if errors.Is(err, io.EOF) {
			return readyLine{}, spawnMalformed, "the line has no end"
		}
		return readyLine{}, spawnMalformed, err.Error()
	}
	var line readyLine
	if err := json.Unmarshal(bytes.TrimSuffix(buf, []byte("\n")), &line); err != nil {
		return readyLine{}, spawnMalformed, err.Error()
	}
	if why := line.invalid(); why != "" {
		return readyLine{}, spawnMalformed, why
	}
	return line, 0, ""
}

// invalid says why a decoded line is not a ready line, "" when it is one
// (astra r5-c3 4): each branch must carry everything its reader acts on, and
// nothing of the other's. ok names the host in a host id's form, the socket
// as an absolute path, the session by a craze id (a token: it names a lock),
// and the craze serving it. Not ok says why, and a holder, when there is one,
// names the session by its craze id — a held line naming none would have the
// rendezvous match any registry entry that carries no session yet — and its
// host by a host id's form, or by nothing at all with no pid either: a lock
// that names nobody yet (plan 030 X19, X21: any live host serving the session
// is then the holder). A member the line has that is not one of these is
// ignored — a host may be a newer craze than its launcher.
func (l readyLine) invalid() string {
	if l.OK {
		switch {
		case l.Error != "" || l.Held != nil:
			return "ok, and a refusal too"
		case !rundir.ValidHostID(l.HostID):
			return fmt.Sprintf("ok names no host id (%q)", l.HostID)
		case !filepath.IsAbs(l.Socket):
			return fmt.Sprintf("ok names no socket (%q)", l.Socket)
		case !rundir.ValidToken(l.CrazeSessionID):
			return fmt.Sprintf("ok names no session (%q)", l.CrazeSessionID)
		case l.CrazeVersion == "":
			return "ok names no craze version"
		}
		return ""
	}
	h := l.Held
	switch {
	case l.Error == "":
		return "not ok, and no reason"
	case h == nil:
		return ""
	case !rundir.ValidToken(h.CrazeSessionID):
		return fmt.Sprintf("held names no session (%q)", h.CrazeSessionID)
	case h.PID < 0 || h.PID > math.MaxInt32:
		return fmt.Sprintf("held names pid %d", h.PID)
	case h.HostID == "" && h.PID != 0:
		return fmt.Sprintf("held names pid %d and no host", h.PID)
	case h.HostID != "" && !rundir.ValidHostID(h.HostID):
		return fmt.Sprintf("held names no host id (%q)", h.HostID)
	}
	return ""
}

// fail ends a host that gave no usable answer and says what happened, naming
// its log, where it said whatever it could. A host that closed its pipe
// unwritten is exiting, or has: it is given the grace to (settle), and its
// exit status is part of the answer; any other is terminated. A cancelled
// spawn carries ctx's error.
func (c *hostChild) fail(ctx context.Context, failure spawnFailure, why, logPath string) error {
	if failure == spawnExited {
		c.settle()
	} else {
		c.terminate()
	}
	var msg string
	switch failure {
	case spawnExited:
		msg = "the session host exited before it was ready"
		if c.err != nil {
			msg += " (" + c.err.Error() + ")"
		}
	case spawnTimedOut:
		msg = "the session host was not ready within " + spawnReadyWait.String()
	case spawnOversized:
		msg = fmt.Sprintf("the session host's ready line is longer than %d bytes", readyLineMax)
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

// exited reports whether the host has exited and been reaped.
func (c *hostChild) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// waitExit waits up to d for the host to exit: whether it did.
func (c *hostChild) waitExit(d time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(d):
		return false
	}
}

// settle gives a host that is exiting of its own accord — it answered not ok,
// or closed its pipe unwritten — spawnTermGrace to, and terminates it past
// that. One that exited without its stop sequence (a crash) may have left an
// agent: its record is read either way (killAgents).
func (c *hostChild) settle() {
	if !c.waitExit(spawnTermGrace) {
		c.terminate()
		return
	}
	c.killAgents()
}

// terminate ends the host, and everything it spawned (plan 030 §3.4, R2-2):
// SIGTERM, which runs its stop sequence — the engine's close ends its agent,
// whose process group is the agent's own — and spawnTermGrace for that; past
// it, SIGKILL to the host's process group (setsid made it the leader of its
// own), a second grace for the reaper, and then, whichever way the host went,
// SIGKILL to every agent group it recorded that its agent still leads
// (killAgents): an agent that outlives its pipes is ended there. It returns
// once all of that is done; the host is reaped unless it could not be killed
// within the second grace (an uninterruptible wait), when the reaper still
// takes it whenever it goes.
func (c *hostChild) terminate() {
	if !c.exited() {
		// Signal on the Process, not the pid: once the reaper has waited for
		// it, this is refused rather than sent to whatever reused the pid.
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
		if !c.waitExit(spawnTermGrace) {
			if !c.exited() {
				_ = syscall.Kill(-c.pid, syscall.SIGKILL)
			}
			c.waitExit(spawnTermGrace)
		}
	}
	c.killAgents()
}

// killAgents kills every agent process group the host recorded and removes
// the record: nothing, for a host that stopped cleanly and removed it itself.
// A group is signalled only while it is provably the agent's (plan 030 X22,
// astra r5-c3 1): the process leading it — the agent, whose pid is the
// group's id — still there, and started at the instant the host recorded
// (agentGroup.stale). A number an agent had can be anyone's once the agent
// has gone and been reaped, and this runs seconds after its host has, or
// after a launcher slow to get here; a group whose leader has exited or is a
// stranger is left alone, and the host's log says so (note). Never 0 or 1 or
// this process's own group, and at most agentGroupsMax records.
func (c *hostChild) killAgents() {
	b, err := os.ReadFile(c.groups)
	if err != nil {
		return
	}
	own := syscall.Getpgrp()
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if n++; n > agentGroupsMax {
			c.note("agent process groups past the first %d in %s not killed", agentGroupsMax, c.groups)
			break
		}
		g, ok := parseAgentGroup(line)
		if !ok || g.pgid <= 1 || g.pgid == own {
			if len(line) > 64 {
				line = line[:64] + "…"
			}
			c.note("a record that names no agent's process group not acted on: %q", line)
			continue
		}
		if why := g.stale(); why != "" {
			c.note("agent process group %d not killed: %s", g.pgid, why)
			continue
		}
		_ = syscall.Kill(-g.pgid, syscall.SIGKILL)
	}
	_ = os.Remove(c.groups)
}

// note appends one line to the host's log: what the spawner did after the
// host, where whoever reads the host's log looks. Best effort — a log that
// cannot be opened loses it — and never through a link.
func (c *hostChild) note(format string, args ...any) {
	if c.log == "" {
		return
	}
	f, err := os.OpenFile(c.log, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "craze: "+format+"\n", args...)
	_ = f.Close()
}

// errHolderGone is a held session's holder that exited, or released the
// session, before the rendezvous found it serving: a spawn past it may claim
// the session now.
var errHolderGone = errors.New("the session's holder is gone")

// rendezvous finds the host holding a session a spawn was refused (plan 030
// §3.4): polled every spawnRendezvousPoll, up to spawnRendezvousWait, until
// the registry lists the holder's host id serving that session — its identity
// published, which a host writes once bound with the session's engine; never
// its ready flag, which waits for the provider's start. A lock that names no
// holder yet has no host id to look for, and any live host serving the
// session will do.
//
// The holder is gone (errHolderGone) when its process has died (the pid its
// lock names), or when its entry has been seen and is no longer listed; a
// holder still there at the bound is refused as the picker refuses it —
// serving no socket, or another session — with the *rundir.HeldError.
func rendezvous(ctx context.Context, env rundir.Env, held *readyHeld) (hostRef, error) {
	deadline := time.Now().Add(spawnRendezvousWait)
	seen := false
	herr := held.heldError()
	for {
		spawnPolled()
		entries, _ := rundir.Hosts(env)
		listed := false
		for _, e := range entries {
			switch {
			case held.HostID != "" && e.HostID != held.HostID:
			case e.CrazeSessionID == held.CrazeSessionID:
				return hostRef{entry: e, held: true}, nil
			case held.HostID == "":
			case e.CrazeSessionID == "":
				listed = true
			default:
				return hostRef{}, &spawnError{kind: spawnHeld, msg: "craze: " + refusal(herr) + holderElsewhere.refusalSuffix(), err: herr}
			}
		}
		if listed {
			seen = true
		}
		if (seen && !listed) || (held.PID > 0 && !processAlive(held.PID)) {
			return hostRef{}, errHolderGone
		}
		if time.Now().After(deadline) {
			return hostRef{}, &spawnError{kind: spawnHeld, msg: "craze: " + refusal(herr) + holderNoSocket.refusalSuffix(), err: herr}
		}
		select {
		case <-ctx.Done():
			return hostRef{}, &spawnError{kind: spawnCancelled, msg: "craze: the session host's start was abandoned", err: ctx.Err()}
		case <-time.After(spawnRendezvousPoll):
		}
	}
}

// heldError is the claim's refusal a held answer reports, as the host's own
// claim had it: what refusal words and a caller finds with errors.As.
func (h *readyHeld) heldError() *rundir.HeldError {
	return &rundir.HeldError{CrazeID: h.CrazeSessionID, Holder: rundir.Holder{PID: h.PID, HostID: h.HostID}}
}

// processAlive reports whether pid names a live process (a zombie counts:
// it has not been reaped).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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
// connection of its own, bounded by spawnStopWait, then spawnTermGrace for the
// host's stop sequence; a stop that cannot be sent — the socket unreachable,
// or refused — or a host still there after the grace, is terminated. It
// returns once the host is gone. Nothing for a held ref.
func (r hostRef) abandon() {
	c := r.child
	if c == nil || r.held {
		return
	}
	if c.exited() {
		c.killAgents()
		return
	}
	if stopHost(r.entry) == nil && c.waitExit(spawnTermGrace) {
		c.killAgents()
		return
	}
	c.terminate()
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
