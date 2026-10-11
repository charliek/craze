package hostspawn

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/charliek/craze/internal/rundir"
)

// The reparenting start (plan 041 X32). A host Start or StartCmd starts is in
// a session and a process group of its own, which no terminal's signal and no
// group signal of its spawner's reaches — but setsid does not reparent: the
// host stays its spawner's child until one of them exits. A signal for the
// spawner's whole process tree, found by parent pid, reaches the host too:
// Claude Code's TaskStop on a Bash call that ran craze new --wait ended that
// command's session that way (live check L6: the host's log reads `craze
// serve: stopping: SIGTERM`). StartReparented starts the host through a
// short-lived intermediate instead, which starts the host and exits at once;
// the host is then its new parent's (init, launchd, or the nearest subreaper)
// and no descendant of the spawner's.
//
// The intermediate is craze itself, marked ReparentEnv, and Reparent is all
// it runs: craze's main calls it first, before the command line is parsed or
// anything is read, written or locked.
//
// A host the spawner never hears of is never left behind by either side (C6
// r1, 1): the intermediate ends a host it started and could not report, and
// the spawner, when no report comes, finds the host by the id it gave it
// (unreported) and ends it — unless the spawn is shared (a reload's), whose
// host neither side ever signals (C6 r2, 1, ReparentSharedEnv): it is left
// serving, as its spawner leaves it anyway.

// ReparentEnv marks a reparenting spawn's intermediate (StartReparented): the
// descriptor of the pipe it reports its host on, 4, beside the ready pipe's 3.
// The intermediate takes it out of its environment before anything else
// (Reparent), so neither its host nor anything the host runs sees it. Like
// ReadyFDEnv, an environment mark between craze and craze, never on the
// control protocol.
const ReparentEnv = "CRAZE_REPARENT_FD"

// ReparentSharedEnv=1, beside ReparentEnv, tells the intermediate its spawn
// is shared (ReparentSpawn.Shared: craze send's reload, its host started
// --outlive-spawner), so a host it cannot report is left serving rather
// than ended (C6 r2, 1): a reload never stops its host (X22), and another
// sender may have found and prompted it by then. Taken out of the
// environment with ReparentEnv.
const ReparentSharedEnv = "CRAZE_REPARENT_SHARED"

// ReparentWait bounds a reparenting spawn's intermediate: its report of the
// host, and then its exit. A variable only so a test can shorten it (never in
// parallel), as the handshake's bounds are.
var ReparentWait = 10 * time.Second

// ReparentTimer is the intermediate's waits' timer, time.After: a seam a test
// fires by hand, so an intermediate is timed out exactly when the test has
// seen what it waits for (as ReadyTimer is the ready wait's).
var ReparentTimer = time.After

// ReparentStarted is told the host's pid in the intermediate (Reparent) once
// it has started the host, before its report: a no-op, and the seam a test
// holds an intermediate at, its host started and unreported.
var ReparentStarted = func(pid int) {}

// ReparentAbandoning is told when a reparenting start fails after its
// intermediate ran (StartReparented's abandon), before that intermediate is
// killed — its report pipe's read end already closed when no report came: a
// no-op, and the seam a test lets a held intermediate go at, so it attempts
// its report and finds nobody to read it.
var ReparentAbandoning = func() {}

// reparentPoll is how often a reparented host, which no reaper here waits
// for, is looked for once gone (Child.gone).
const reparentPoll = 10 * time.Millisecond

// reportMax bounds the intermediate's report: one AgentGroup line, two
// decimal numbers and a newline.
const reportMax = 64

// ReparentSpawn is what a reparenting start knows of its host beyond
// StartCmd's arguments: where to find it should the intermediate never report
// it, and whether it is the spawn's to end then.
type ReparentSpawn struct {
	// Env is the registry the host lists itself in, and HostID the id the
	// spawner gave it (--host-id): how a host started and never reported is
	// found (unreported).
	Env    rundir.Env
	HostID string
	// Shared is a host for a session others may have found and prompted —
	// craze send's reload, --outlive-spawner — which a failed start leaves
	// serving rather than ends.
	Shared bool
	// Hurry, when set, is called as a failed start's clean-up begins
	// (abandon) and answers a channel that closes when the clean-up is to be
	// cut short (C8, 3) — the command's further signal: the host it ends is
	// then sent SIGTERM and not waited for, its grace and the SIGKILL past
	// it skipped, as C3's hurried undo sends session.stop and does not wait.
	Hurry func() <-chan struct{}
}

// StartReparented is StartCmd with the host left out of this process's tree
// (the file's comment). cmd — craze, as Command builds it — is started as
// StartCmd starts a host (a session of its own, stdin, stdout and stderr on
// /dev/null, the write end of a fresh ready pipe as its fd 3, ReadyFDEnv=3
// and marker=1, cmd.Env or this process's own), with the write end of a
// second pipe as its fd 4 and ReparentEnv=4 besides: so marked, craze is the
// intermediate (Reparent), which starts the host from that same command line
// and environment, reports it, and exits. Its report — the host's pid and
// start time, as one AgentGroup line, since the host leads a group of its own
// — is read within ReparentWait; the intermediate is then reaped, and killed
// first should it outstay a further ReparentWait, which leaves the host
// reparented all the same. The Child is the host: Terminate, Kill, Exited and
// the rest act on it, and nothing on the intermediate, whose exit means
// nothing once it has reported. The ready pipe is the host's, as StartCmd's.
//
// No report within the bound, one that is not a report, or ctx ending first
// — in either wait — is a start that failed (abandon): the intermediate is
// killed and reaped, the ready pipe closed, and the host, reported or found
// in the registry, ended — or, shared, left serving. A ctx that ended is the
// error's cause.
func StartReparented(ctx context.Context, cmd *exec.Cmd, marker, groups, log string, rs ReparentSpawn) (*Child, *os.File, error) {
	r, w, err := Pipe()
	if err != nil {
		return nil, nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, nil, err
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(withoutEnv(env, ReadyFDEnv, marker, ReparentEnv, ReparentSharedEnv), ReadyFDEnv+"=3", marker+"=1", ReparentEnv+"=4")
	if rs.Shared {
		cmd.Env = append(cmd.Env, ReparentSharedEnv+"=1")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.ExtraFiles = []*os.File{w, pw}
	// A session of its own for the intermediate too, so a terminal's signal
	// for its spawner's foreground group never reaches it in the instant it
	// lives.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	// Ours closed at once, whatever Start said, as StartCmd's: the
	// intermediate's copies — and then the host's ready pipe alone — are the
	// only write ends left.
	_ = w.Close()
	_ = pw.Close()
	if err != nil {
		_ = r.Close()
		_ = pr.Close()
		return nil, nil, err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	// endIntermediate kills the intermediate — its group, which setsid made
	// its own and the host, in a session of its own, is not in — unless it has
	// gone already, and waits for it to be reaped.
	endIntermediate := func() {
		select {
		case <-exited:
		default:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-exited
	}
	// abandon is a start that failed once the intermediate ran (C6 r1, 1).
	// The intermediate goes first, so it reports nothing more; then the ready
	// pipe's read end, so a host that announces itself from here on finds it
	// closed and stops (a shared one serves on); and only then is the host
	// looked for — the one reported, else the registry's, where a host whose
	// announcement went out before the close is listed already, its listing
	// coming first — and ended, its recorded agents with it: a host nobody
	// was told of is one nobody would ever stop. Found nowhere, only the
	// record is acted on (a host the intermediate killed may have left an
	// agent). A shared spawn's host is left as it is. The host's log says
	// which, where whoever reads it looks. The host's end is waited for
	// only until the spawner's hurry closes (rs.Hurry, C8, 3), armed before
	// anything else: SIGTERM has gone by then, and its grace, and the SIGKILL
	// past it, are left to a goroutine of their own.
	abandon := func(host *AgentGroup, cause error) (*Child, *os.File, error) {
		var hurry <-chan struct{}
		if rs.Hurry != nil {
			hurry = rs.Hurry()
		}
		ReparentAbandoning()
		endIntermediate()
		_ = r.Close()
		said := &Child{groups: groups, log: log}
		switch {
		case rs.Shared:
			said.note("its start failed (%v); left serving, as a shared spawn's host is", cause)
		default:
			var c *Child
			if host != nil {
				c = &Child{pid: host.PGID, start: host.Start, groups: groups, log: log, done: make(chan struct{})}
			} else {
				var why string
				if c, why = unreported(rs, groups, log); why != "" {
					said.note("its start failed (%v); not ended: %s", cause, why)
				}
			}
			if c == nil {
				said.KillAgents()
				break
			}
			said.note("its start failed (%v); ending this host", cause)
			if !c.Exited() {
				c.terminate()
			}
			// The grace is read here, not on the goroutine: a hurried
			// abandon leaves that goroutine running past this call, and
			// it must not read the package's tunables after (a test
			// restoring them raced it).
			grace := TermGrace
			ended := make(chan struct{})
			go func() {
				defer close(ended)
				c.escalate(grace)
				c.KillAgents()
			}()
			select {
			case <-ended:
			case <-hurry:
				said.note("hurried: SIGTERM sent, and this host's end not waited for")
			}
		}
		return nil, nil, cause
	}
	host, why, cancelled := readReport(ctx, pr)
	_ = pr.Close()
	switch {
	case cancelled:
		return abandon(nil, fmt.Errorf("its start was abandoned: %w", ctx.Err()))
	case why != "":
		return abandon(nil, fmt.Errorf("its intermediate reported no host (%s)", why))
	}
	select {
	case <-exited:
	case <-ctx.Done():
		return abandon(&host, fmt.Errorf("its start was abandoned: %w", ctx.Err()))
	case <-ReparentTimer(ReparentWait):
		endIntermediate()
	}
	return &Child{pid: host.PGID, start: host.Start, groups: groups, log: log, done: make(chan struct{})}, r, nil
}

// unreported is a host the intermediate started and never reported (C6 r1,
// 1), found by the id its spawner gave it in the registry, where a host lists
// itself before it writes its ready line; nil when it is not listed — never
// started, gone, or not listed yet, and so to find its ready pipe closed and
// stop. One registry scan, bounded by rundir's own budget for a contended
// entry.
//
// A listed entry is not proof that its pid is still the host's (C6 r2, 2): a
// sweeper holding a crashed host's lock while it removes the entry makes the
// entry look live, and by then the host's pid can be another process's. So
// the process with that pid is taken for the host only when its start, as
// its kernel keeps it, is exactly the start the host recorded of itself in
// its entry as it bound (rundir.Entry.PIDStart, C6 r3): no clock is read and
// nothing is allowed for — a later process given the pid started at another
// instant. That start then binds every signal to it (signalHost). An entry
// that records no start — a host that could not read its own, or an older
// craze — is never signalled, nor is a pid whose start differs or cannot be
// read: nil, and why says so; its spawner then ends only the agents its
// record names (Child.KillAgents), and a host it was after all serves on
// unattended until its idle exit or craze stop.
func unreported(rs ReparentSpawn, groups, log string) (*Child, string) {
	if rs.HostID == "" {
		return nil, ""
	}
	e, ok := listedHost(rs.Env, rs.HostID)
	if !ok {
		return nil, ""
	}
	if e.PIDStart == 0 {
		return nil, fmt.Sprintf("its registry entry records no start for its pid %d, so the process with that pid cannot be told for it", e.PID)
	}
	id, err := rundir.ProcessIdentity(e.PID)
	switch {
	case errors.Is(err, rundir.ErrNoProcess):
		return nil, ""
	case err != nil:
		return nil, fmt.Sprintf("its registry entry's pid %d cannot be read: %v", e.PID, err)
	case id.Start != e.PIDStart:
		return nil, fmt.Sprintf("its registry entry's pid %d is another process now (started at %d, the host at %d)", e.PID, id.Start, e.PIDStart)
	}
	return &Child{pid: e.PID, start: e.PIDStart, groups: groups, log: log, done: make(chan struct{})}, ""
}

// listedHost is host id's live registry entry.
func listedHost(env rundir.Env, hostID string) (rundir.Entry, bool) {
	entries, err := rundir.Hosts(env)
	if err != nil {
		return rundir.Entry{}, false
	}
	for _, e := range entries {
		if e.HostID == hostID && e.PID > 1 {
			return e, true
		}
	}
	return rundir.Entry{}, false
}

// readReport reads the intermediate's report from f, within ReparentWait: the
// host's pid (its group's id) and start time, or why there is none — a
// timeout, an EOF with no line, or a line that is not a report — or that ctx
// ended first (cancelled). A report already read when the wait ends is the
// answer. The read runs on a goroutine of its own, which closing f, or the
// intermediate's end, ends.
func readReport(ctx context.Context, f *os.File) (g AgentGroup, why string, cancelled bool) {
	type result struct {
		line string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(f, reportMax)).ReadString('\n')
		got <- result{line, err}
	}()
	var res result
	select {
	case res = <-got:
	case <-ReparentTimer(ReparentWait):
		select {
		case res = <-got:
		default:
			_ = f.Close()
			return AgentGroup{}, fmt.Sprintf("no report within %v", ReparentWait), false
		}
	case <-ctx.Done():
		select {
		case res = <-got:
		default:
			_ = f.Close()
			return AgentGroup{}, "", true
		}
	}
	switch {
	case errors.Is(res.err, io.EOF) && res.line == "":
		return AgentGroup{}, "it exited without a report", false
	case res.err != nil:
		return AgentGroup{}, fmt.Sprintf("a report with no end (%q)", res.line), false
	}
	g, ok := ParseAgentGroup(res.line)
	if !ok || g.PGID <= 1 || g.Start == 0 {
		return AgentGroup{}, fmt.Sprintf("%q is not a report", strings.TrimSuffix(res.line, "\n")), false
	}
	return g, "", false
}

// Reparent is a reparenting spawn's intermediate (StartReparented), which
// craze's main runs before anything else. In any process whose environment
// has no ReparentEnv — every craze but an intermediate — it returns at once.
// In one it never returns: it takes ReparentEnv out of its environment and
// starts the host — os.Executable(), with this process's own arguments and
// environment, so the command line and the marks StartReparented set — in a
// session of its own, this process's stdin, stdout and stderr (/dev/null)
// and its fd 3, the ready pipe, passed through; writes the host's pid and
// start time on the descriptor ReparentEnv named, which the host does not
// inherit; and exits — 0 once the report is written; 1 when the host could
// not be started — the descriptors past the ready pipe not all kept from it
// (closeOnExecInherited, C11) among the reasons, no host started then — or
// not reported: its start time not read, or the report
// not written whole — the host is ended then, its whole group, while it is
// still this process's child (C6 r1, 1), unless the spawn is shared
// (ReparentSharedEnv=1, C6 r2, 1): a reload's host is left serving, as its
// spawner would leave it; 2 for a ReparentEnv that names no pipe, or no ready
// pipe to pass on (ReadyFDEnv=3, a pipe), which StartReparented always hands
// it — so a stray ReparentEnv in a person's environment is refused, never
// acted on.
func Reparent() {
	raw, ok := os.LookupEnv(ReparentEnv)
	if !ok {
		return
	}
	shared := os.Getenv(ReparentSharedEnv) == "1"
	_ = os.Unsetenv(ReparentEnv)
	_ = os.Unsetenv(ReparentSharedEnv)
	// The exit system call itself: the intermediate owes no exit hook, and
	// under -race os.Exit first runs the race runtime's finalizer, which a
	// craze child was seen to hang in (plan 032 X67).
	syscall.Exit(reparent(raw, shared))
}

// reparent is Reparent's work once marked: its exit code. shared is a
// shared spawn's intermediate, which never signals its host.
func reparent(raw string, shared bool) int {
	fd, err := strconv.Atoi(raw)
	if err != nil || fd <= 3 || !isPipe(fd) {
		fmt.Fprintf(os.Stderr, "craze: %s=%q does not name a pipe\n", ReparentEnv, raw)
		return 2
	}
	if os.Getenv(ReadyFDEnv) != "3" || !isPipe(3) {
		fmt.Fprintf(os.Stderr, "craze: %s is set, and there is no ready pipe (%s=3) to pass on\n", ReparentEnv, ReadyFDEnv)
		return 2
	}
	// Before anything else, everything this process holds past the ready
	// pipe — the report pipe, and whatever its spawner was itself handed
	// open and not close-on-exec (C10) — stays behind when the host is
	// started (closeOnExecInherited). A host that would get them is not
	// started at all (C11): exit 1, as for a host that cannot be started,
	// and no report — its spawner's failed start, with nothing to end.
	if err := closeOnExecInherited(); err != nil {
		fmt.Fprintln(os.Stderr, "craze: the session host cannot be started: its spawner's descriptors cannot be kept from it:", err)
		return 1
	}
	// Only the ready pipe crosses to the host.
	syscall.CloseOnExec(fd)
	report := os.NewFile(uintptr(fd), "craze-reparent")
	// A signal for this process — a tree kill that finds it in the instant
	// it lives — waits until the report is written, so the spawner, which
	// that kill reaches too, knows the host it has to end. Caught, never
	// ignored: exec resets a caught signal to its default for the host, and
	// would hand on an ignored one.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "craze: the session host cannot be started:", err)
		return 1
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{os.NewFile(3, "craze-ready")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "craze: the session host cannot be started:", err)
		return 1
	}
	pid := cmd.Process.Pid
	// A host its spawner will not hear of — or could not tell from a later
	// process with its pid — is one it could never end: it is ended here, its
	// whole group (setsid made it the leader), while it is still this
	// process's child and its pid surely its own, and reaped. Not a shared
	// spawn's: another sender may already have found it in the registry and
	// prompted it, and it serves on (--outlive-spawner), as its spawner
	// leaves it.
	endHost := func() {
		if shared {
			return
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}
	ReparentStarted(pid)
	id, err := rundir.ProcessIdentity(pid)
	if err != nil {
		endHost()
		fmt.Fprintln(os.Stderr, "craze: the session host's identity:", err)
		return 1
	}
	// A short write is an error too (os.File writes the whole line or says
	// why not).
	if _, err := report.WriteString(AgentGroup{PGID: pid, Start: id.Start}.Line()); err != nil {
		endHost()
		fmt.Fprintln(os.Stderr, "craze: the session host's report:", err)
		return 1
	}
	return 0
}

// closeOnExecInherited marks every descriptor the intermediate holds past the
// ones it hands the host on purpose — stdin, stdout and stderr (/dev/null)
// and the ready pipe, 3 — close-on-exec (plan 041 C10): a descriptor its
// spawner was itself handed open, and not close-on-exec, would otherwise
// reach the host through the intermediate's exec and be held for the host's
// whole life — a caller's pipe never seeing its end, a Bash tool's output
// pipe keeping that call open while the host lives (seen on a CI runner,
// whose test process held two such pipes). A descriptor that cannot be
// marked — or descriptors that cannot be listed at all — is the error, which
// the intermediate refuses to start a host over (C11: the leak would
// otherwise go unseen, its stderr the spawner's /dev/null); one that went
// while it was marked (EBADF, the walk's own directory) is no error.
func closeOnExecInherited() error {
	return markCloseOnExec(4)
}

// markCloseOnExec is closeOnExecFrom: a test's seam, which a test's
// intermediate sets to fail (the marking refused).
var markCloseOnExec = closeOnExecFrom

// closeOnExecListed marks close-on-exec each descriptor from first up that
// dir — /proc/self/fd, /dev/fd — lists for this process. The directory's own
// descriptor is among them, closed once it is read: marking it then fails
// EBADF, which is no descriptor left to hand on, and is passed over. Any
// other failure is answered, the rest marked all the same.
func closeOnExecListed(dir string, first int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd < first {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err == nil {
			_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC)
		}
		if err != nil && !errors.Is(err, unix.EBADF) {
			errs = append(errs, fmt.Errorf("descriptor %d: %w", fd, err))
		}
	}
	return errors.Join(errs...)
}

// isPipe reports whether fd is an open pipe or FIFO.
func isPipe(fd int) bool {
	var st syscall.Stat_t
	return syscall.Fstat(fd, &st) == nil && uint32(st.Mode)&syscall.S_IFMT == syscall.S_IFIFO
}
