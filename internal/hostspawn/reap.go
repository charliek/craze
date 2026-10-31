package hostspawn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/rundir"
)

// Ending what a dead host left running (AgentGroup). A shell command leads a
// session and a process group of its own, so a host killed outright takes none
// of its commands with it; a host writes each of them down the moment it starts,
// and removes it once the group has had its last signal. Three kinds of process
// act on what a dead host left: a waiter that has just seen its host gone
// (ReapHost), every craze command that reads the registry and the hub, on its
// tick (SweepDeadHosts), and a spawner (KillRecordedAgents). Any of them may run at
// once with any other: each holds the record's lock for as long as it acts on it.
//
// The order is what keeps a record whole: the host's death is established first,
// and only then is the record locked and read. A dead host writes nothing, so what
// is read is all there will be, and removing it loses nothing. A host's rewrite
// replaces the file, so the file is taken by its name again after the death is
// established (examine).
//
// What tells a group from one that merely has its number, in the order asked:
//
//   - A number that cannot be a group of ours: 0 and 1, and the dead host's own,
//     are a record that is wrong. This process's own group or pid is never
//     signalled, and keeps the record: the caller may be one of the host's own
//     commands, and a craze outside the group finishes the job.
//   - The leader alive with the start time written down. Its pid is taken, so the
//     group's number cannot be anyone else's. Signalled.
//   - A leader that is a zombie with that start time: still holding the number,
//     so what is in the group is the group. Signalled if anything in it can die.
//   - Another start time. The number was free for someone to take it, so the
//     group it named had emptied. Never signalled; the entry is dropped.
//   - No process at all, a shell command's entry. What is left of the group is
//     found by a scan, and a number in use as a group's is not handed to a new
//     process; but the group may have emptied since the host died and its number
//     gone to a stranger whose own leader has exited in turn, as any daemon's has
//     once it has forked twice, and that stranger may be another host's command.
//     So a member also has to be one of this host's (AgentGroup.shellMember): in
//     the group's own session, since a command's shell is a session leader; started
//     no earlier than the leader; and carrying CommandMark for this very host. A
//     member that cleared its environment, alone in its group with its leader gone,
//     is therefore left running: a sweep cannot tell it from a stranger, and the
//     docs say so. A member whose environment cannot be read at all, though it is still
//     there, is neither: nothing is signalled and the entry stays, to be read again.
//   - No process at all, an agent's entry: dropped. An agent's entry is not removed
//     when the agent exits, only when its host stops, so its number may have been
//     anyone's for hours, and an agent's environment is not craze's to mark. It
//     is signalled only while its leader lives, as ever.
//   - A line with no owner (an older host's) has no boot or namespace to check its
//     numbers against, so a sweep never signals it. Only a spawner whose own child
//     has just died, on the same boot and with the same numbers, may
//     (KillRecordedAgents).
//
// A record that belongs to a host still running is left alone, whatever it says
// (GroupOwner). A host's own stop removes its record: one mid-exit is alive until
// the record has gone. A host on another boot or in another PID namespace is one
// whose numbers mean nothing here: its record is never acted on, and goes once it
// has aged. Those files are this code's: the logs' sweep does not touch them,
// since an old record can still name something running.

const (
	// ReapGrace is how long a dead host's process groups have between SIGTERM
	// and SIGKILL.
	ReapGrace = 2 * time.Second
	// SweepHostsMax is how many dead hosts' records one SweepDeadHosts acts on.
	// A record it keeps counts, so each pass starts somewhere else in the listing
	// (sweepStart), and what was not reached is reached by a later one. A host
	// still running costs two small reads and does not count.
	SweepHostsMax = 20
	// sweepRead bounds the reading before anything is signalled, and each scan of
	// the machine's processes (a few microseconds a process): a record it did not
	// reach, or a scan it cut, waits for the next sweep.
	sweepRead = 500 * time.Millisecond
	// SweepBound is how long a sweep usually takes at most, once its listing is read:
	// selection and the first round have 2*sweepRead between them, past which they
	// stop and what they did not reach stays recorded, and then the grace. The
	// SIGKILL round is not cut by it: a group that has had its SIGTERM has its SIGKILL
	// unless the run's context ends, so a pass that is slow still gets there. A short
	// command does not wait past a bound of its own (hostSweepBound), and leaves the
	// record to the next craze; a hub or a longer command finishes it.
	SweepBound = 2*sweepRead + ReapGrace
	// RecordKeep is how long a record nothing here can act on is kept: another
	// boot's or PID namespace's, or one whose host or groups cannot be read.
	RecordKeep = 7 * 24 * time.Hour
	// groupsPoll is how often a grace looks for its groups to have gone; a poll
	// is a kill(2) a group. groupsScan is how often it scans the machine, to see a
	// group of nothing but zombies has nothing left to wait for.
	groupsPoll = 20 * time.Millisecond
	groupsScan = 250 * time.Millisecond
	// recordBytes bounds a record's read: AgentGroupsMax lines, far past what a
	// host writes.
	recordBytes = AgentGroupsMax * 256
	// bareRecordAge is how long a record with no owner in it (an older craze's)
	// must have been left before it may go, its host's lock free: a missing lock is
	// also a host mid-exit, whose record goes within seconds.
	bareRecordAge = rundir.OrphanAge
	// CommandMarkName is the environment variable that names the host a native
	// command was started by (CommandMark).
	CommandMarkName = "CRAZE_HOST_ID"
)

// CommandMark is the environment entry every command host hostID runs carries: what
// a member of a group whose leader has gone must have, so that nothing another host
// started can be taken for this one's.
func CommandMark(hostID string) string { return CommandMarkName + "=" + hostID }

// ClearCommandMark takes the mark out of this process's environment: the first thing
// every craze does. A craze a native command ran is not that command's host's to
// answer for, so nothing it starts (a hub, a host, an ACP agent, a command of its
// own) carries the mark it found. Only a native command has one, put in its exec's
// environment by its host; what that command runs that is not craze inherits it,
// and is that host's.
func ClearCommandMark() { _ = os.Unsetenv(CommandMarkName) }

// Seams, each its real value in production; a test replaces one (never in
// parallel) to force a schedule.
var (
	// sweepStart is where in a listing of n records a pass starts.
	sweepStart = func(n int) int { return rand.IntN(n) }
	// recordPeeked runs in examine once the record has been read for its owner,
	// before that owner's death is established.
	recordPeeked = func(path string) {}
	// waitForGroups is waitGroups.
	waitForGroups = waitGroups
	// reapLockWait is how long ReapHost waits out another sweep at the record: that
	// sweep's whole bound, and a read.
	reapLockWait = SweepBound + sweepRead
	// recordsListed runs in SweepDeadHosts once its listing has been read.
	recordsListed = func() {}
	// groupRead runs in signal between a group's read and the one straight before
	// its signal.
	groupRead = func(AgentGroup) {}
	// sendSignal is syscall.Kill, for signalGroup alone.
	sendSignal = syscall.Kill
	// processCarries is rundir.ProcessCarries, for shellMember alone.
	processCarries = rundir.ProcessCarries
)

// noteFunc says something on a log; never nil past the entry points.
type noteFunc func(format string, args ...any)

// memberScan is one look at the machine's processes (rundir.ProcessGroups), made
// when something first asks.
type memberScan struct {
	deadline time.Time
	groups   map[int][]rundir.GroupMember
	err      error
	done     bool
}

func (s *memberScan) of(pgid int) ([]rundir.GroupMember, error) {
	if !s.done {
		s.groups, s.err = rundir.ProcessGroups(s.deadline)
		s.done = true
	}
	return s.groups[pgid], s.err
}

// groupFate is what a read of a group came to.
type groupFate int

const (
	// groupOver: nothing of it is left to signal, or what has its number is not
	// it. Its entry is dropped.
	groupOver groupFate = iota
	// groupHere: provably the group, with something in it to signal.
	groupHere
	// groupUnknown: it cannot be told now. A shell command's entry stays.
	groupUnknown
	// groupOwn: it is this process's own. Every kind's entry stays.
	groupOwn
	// groupCut: the run's context ended, or its reading ran out of time, before it
	// could be told. The entry stays for the next run, however old.
	groupCut
)

// entry is one group a reap may signal, and the record it stands in.
type entry struct {
	g   AgentGroup
	rec *groupRecord
}

// reaper is one run over some locked records: the two rounds of signals and what
// bounds them.
type reaper struct {
	ctx context.Context
	// firstBy is when selection and the first round must be over: past it nothing
	// more has its SIGTERM. A group that has had one has its SIGKILL regardless.
	firstBy time.Time
	// killing is the second round, which only the context stops.
	killing bool
	// readBy bounds this round's reading: a scan of the machine, and a search of a
	// group's members.
	readBy time.Time
	grace  time.Duration
	// direct is a spawner whose own child has just died: a line with no owner may
	// be acted on, and is not otherwise.
	direct bool
	// agentsAtOnce is SIGKILL for everything but a shell command in the first round:
	// a spawner's, which has no grace to give an agent.
	agentsAtOnce bool
	scan         *memberScan
}

func (r *reaper) newScan() *memberScan { return &memberScan{deadline: r.readBy} }

// over reports whether the run must signal nothing more.
func (r *reaper) over() bool {
	return r.ctx.Err() != nil || (!r.killing && !time.Now().Before(r.firstBy))
}

// readsOut reports whether a search of a group's members must stop here.
func (r *reaper) readsOut() bool { return r.ctx.Err() != nil || !time.Now().Before(r.readBy) }

// fate reads e's group once: what it came to, why it is not here for a log, and for
// a group that is here, the read to make straight before its signal.
func (r *reaper) fate(e entry) (fate groupFate, why string, holds func() bool) {
	g := e.g
	switch {
	case g.PGID <= 1:
		return groupOver, "it names no process group", nil
	case g.Owner.PID != 0 && g.PGID == g.Owner.PID:
		return groupOver, "it is its host's own", nil
	case g.PGID == os.Getpid() || g.PGID == syscall.Getpgrp():
		return groupOwn, "it is this process's own", nil
	case g.Kind == "" && !r.direct:
		return groupOver, "its line has no boot or namespace to tell its number by", nil
	}
	leader := func() bool {
		id, err := rundir.ProcessIdentity(g.PGID)
		return err == nil && id.Start == g.Start
	}
	id, err := rundir.ProcessIdentity(g.PGID)
	switch {
	case errors.Is(err, rundir.ErrNoProcess):
		if g.Kind != GroupShell {
			return groupOver, "its agent has exited, and a group whose leader has gone cannot be told from another's", nil
		}
		return r.leaderGone(g, e.rec.hostID)
	case err != nil:
		return groupUnknown, "its leader cannot be told: " + err.Error(), nil
	case id.Start != g.Start:
		return groupOver, fmt.Sprintf("its number is another process's now (started at %d, its leader at %d)", id.Start, g.Start), nil
	case id.Zombie:
		// The zombie holds the number, so every member is the group's, and only a
		// live one is worth a signal. A scan that says nothing leaves the signal
		// to do no harm.
		members, err := r.scan.of(g.PGID)
		if err == nil && onlyZombies(members) {
			return groupOver, "only its zombies are left", nil
		}
	}
	return groupHere, "", leader
}

// leaderGone reads a shell command's group whose leader has gone and been reaped,
// by a member that is provably the host's. That member is what makes the number the
// group's while it lives, so it is what is read again before the signal.
func (r *reaper) leaderGone(g AgentGroup, hostID string) (groupFate, string, func() bool) {
	members, err := r.scan.of(g.PGID)
	if err != nil {
		return groupUnknown, "what is left of it cannot be told: " + err.Error(), nil
	}
	live, unread := 0, 0
	for _, m := range members {
		if m.Zombie {
			continue
		}
		if r.readsOut() {
			return groupCut, "this run ended before what is left of it could be told", nil
		}
		live++
		member, known := g.shellMember(m, hostID)
		if member {
			return groupHere, "", func() bool {
				now, err := rundir.ProcessMember(m.PID)
				return err == nil && now.PGID == g.PGID && now.Start == m.Start && !now.Zombie
			}
		}
		if !known {
			unread++
		}
	}
	if unread > 0 {
		if r.readsOut() {
			return groupCut, "this run ended before what is left of it could be told", nil
		}
		return groupUnknown, fmt.Sprintf("the environment of %d of the %d left in the group cannot be read", unread, live), nil
	}
	if live > 0 {
		return groupOver, fmt.Sprintf("its leader has gone, and none of the %d left in the group can be told from a stranger's", live), nil
	}
	return groupOver, "nothing is left of it", nil
}

// shellMember reports whether m, found in g's group with its leader gone, is one
// of the commands of host hostID and not a stranger's that has taken the group's
// number since. known is false when that hangs on an environment that cannot be read.
func (g AgentGroup) shellMember(m rundir.GroupMember, hostID string) (member, known bool) {
	if m.Session != g.PGID || m.Start < g.Start {
		return false, true
	}
	return processCarries(m.PID, CommandMark(hostID))
}

// signal reads e's group and sends it sigs, each straight after a read of what
// holds its number: no log, no scan and no other group's work comes between that
// read and its kill(2). What stays open is that instant: the leader, or the member
// that was read, would have to exit, the group empty, and its number go to a new
// group leader, which takes the kernel handing out every other pid first. A group
// whose read fails is looked at again, from a new scan, a few times. It reports
// whether the group was signalled; what it has to say goes on the record's notes. A
// run that must stop, found here or in that read, sends nothing more and keeps it.
func (r *reaper) signal(e entry, sigs ...syscall.Signal) bool {
	for range 3 {
		fate, why, holds := r.fate(e)
		switch fate {
		case groupOwn, groupCut:
			e.rec.keep, e.rec.stay = true, true
		case groupUnknown:
			// An agent's entry goes as it always has: only its leader's being the
			// agent ever had it signalled, and that is not going to be told later.
			e.rec.keep = e.rec.keep || e.g.Kind == GroupShell
		}
		if fate != groupHere {
			e.rec.say("%s process group %d not killed: %s", e.g.what(), e.g.PGID, why)
			return false
		}
		groupRead(e.g)
		signalled := false
		for _, sig := range sigs {
			if r.over() {
				e.rec.keep, e.rec.stay = true, true
				return signalled
			}
			if !holds() {
				break
			}
			signalGroup(e.g.PGID, sig)
			signalled = true
		}
		if signalled {
			return true
		}
		r.scan = r.newScan()
	}
	e.rec.keep = e.rec.keep || e.g.Kind == GroupShell
	e.rec.say("%s process group %d not killed: it kept changing as it was read", e.g.what(), e.g.PGID)
	return false
}

// signalGroup sends sig to the group. Nothing here acts on its error: ESRCH is a
// group that has gone, EPERM one of another user's. The check is that no number that
// could mean "the caller", "init" or "everything" can get this far by any path.
func signalGroup(pgid int, sig syscall.Signal) {
	if pgid <= 1 || pgid == syscall.Getpgrp() {
		return
	}
	_ = sendSignal(-pgid, sig)
}

// what names the kind of thing g is, for a log.
func (g AgentGroup) what() string {
	if g.Kind == GroupShell {
		return "shell command"
	}
	return "agent"
}

// first is the first round over recs: SIGTERM, with SIGCONT since a stopped process
// acts on it only once it runs again, or SIGKILL for what a spawner gives no grace.
// It answers the groups that were given a grace.
func (r *reaper) first(recs []*groupRecord) (termed []entry) {
	r.readBy = r.firstBy
	r.scan = r.newScan()
	for _, rec := range recs {
		for _, g := range rec.groups {
			e := entry{g, rec}
			switch {
			case r.over():
				rec.keep, rec.stay = true, true
			case r.agentsAtOnce && g.Kind != GroupShell:
				if r.signal(e, syscall.SIGKILL) {
					rec.say("%s process group %d killed", g.what(), g.PGID)
				}
			case r.signal(e, syscall.SIGTERM, syscall.SIGCONT):
				rec.say("%s process group %d sent SIGTERM", g.what(), g.PGID)
				termed = append(termed, e)
			}
		}
	}
	for _, rec := range recs {
		rec.flush()
	}
	return termed
}

// second is what comes after the first round: the grace, SIGKILL for what is still
// there, and each record let go, removed unless something in it stays. A run whose
// context ends sends nothing more and keeps what it did not finish, for the next. No
// deadline stops this round: a pass that took long over the first would otherwise
// send SIGTERM again every time and SIGKILL never.
func (r *reaper) second(recs []*groupRecord, termed []entry) {
	if len(termed) > 0 {
		waitForGroups(r.ctx, termed, time.Now().Add(r.grace))
		r.killing = true
		r.readBy = time.Now().Add(sweepRead)
		r.scan = r.newScan()
		for _, e := range termed {
			switch {
			case r.over():
				e.rec.keep, e.rec.stay = true, true
			case r.signal(e, syscall.SIGKILL):
				e.rec.say("%s process group %d sent SIGKILL", e.g.what(), e.g.PGID)
			}
		}
	}
	for _, rec := range recs {
		rec.flush()
		rec.finish()
	}
}

// waitGroups waits until every group has gone, or until until, or the context ends it
// first. An exit with something still running is SIGKILL's to deal
// with, and a zombie cannot be made to go at all: a scan sees a group holding only
// those, so no grace is spent on it.
func waitGroups(ctx context.Context, here []entry, until time.Time) {
	left := make(map[int]bool, len(here))
	for _, e := range here {
		left[e.g.PGID] = true
	}
	nextScan := time.Now().Add(groupsScan)
	for len(left) > 0 && time.Now().Before(until) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(groupsPoll):
		}
		for pgid := range left {
			if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
				delete(left, pgid)
			}
		}
		if len(left) == 0 || time.Now().Before(nextScan) {
			continue
		}
		nextScan = time.Now().Add(groupsScan)
		groups, err := rundir.ProcessGroups(until)
		if err != nil {
			continue
		}
		for pgid := range left {
			if onlyZombies(groups[pgid]) {
				delete(left, pgid)
			}
		}
	}
}

func onlyZombies(members []rundir.GroupMember) bool {
	for _, m := range members {
		if !m.Zombie {
			return false
		}
	}
	return true
}

// groupRecord is one host's record, open and locked by its reader, so that
// two sweeps cannot both be at it and a removal happens once. A lock on the file
// itself, not its name: a host never takes it, and a dead one writes nothing.
type groupRecord struct {
	path   string
	hostID string
	f      *os.File
	st     fs.FileInfo
	groups []AgentGroup
	owner  GroupOwner
	mixed  bool // lines with different owners: nothing about it is believed
	keep   bool
	stay   bool // kept whatever its age: the caller is in a group, or a run was cut short
	note   noteFunc
	notes  []func()
}

// say keeps something to say on the log for after the round: nothing is written
// between one group's read and its signal, and no group waits on a log.
func (r *groupRecord) say(format string, args ...any) {
	r.notes = append(r.notes, func() { r.note(format, args...) })
}

func (r *groupRecord) flush() {
	for _, n := range r.notes {
		n()
	}
	r.notes = nil
}

// parseRecord is a record's entries, with note told of what it does not act on. A
// last line with no end is a write that died, or one under way, and a blank line is
// what a host writes after one it could not take back: neither is an entry. Past
// AgentGroupsMax lines a file is not a host's, and the rest is not read.
func parseRecord(b []byte, path string, note noteFunc) (groups []AgentGroup) {
	s := string(b)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	} else {
		s = ""
	}
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if n++; n > AgentGroupsMax {
			note("agent process groups past the first %d in %s not killed", AgentGroupsMax, path)
			break
		}
		g, ok := ParseAgentGroup(line)
		if !ok || g.PGID <= 1 {
			if len(line) > 64 {
				line = line[:64] + "…"
			}
			note("a record that names no agent's process group not acted on: %q", line)
			continue
		}
		groups = append(groups, g)
	}
	return groups
}

// recordOwner is the owner a record's lines agree on, and whether they do not.
func recordOwner(groups []AgentGroup) (owner GroupOwner, mixed bool) {
	for _, g := range groups {
		switch {
		case g.Owner == (GroupOwner{}):
		case owner == (GroupOwner{}):
			owner = g.Owner
		case owner != g.Owner:
			mixed = true
		}
	}
	return owner, mixed
}

// openRecord opens the record at path and takes its lock, only if it is a file of
// this user's in the state it was named: nil when it has gone, another has the
// lock, or it is not a record at all. Removed by a reader just before the lock was
// taken, it is that reader's: nil too. What it reads is the file as it is once
// locked. busy says another has the lock.
func openRecord(path string, note noteFunc) (rec *groupRecord, busy bool) {
	f, st, _, err := readRecordFile(path)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.Is(err, syscall.EWOULDBLOCK)
	}
	if now, err := os.Lstat(path); err != nil || !os.SameFile(st, now) {
		_ = f.Close()
		return nil, false
	}
	// Read again now that it is held: another reader may have finished with it, and
	// the read that opened it may be of a line under way.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, recordBytes))
	if err != nil {
		_ = f.Close()
		return nil, false
	}
	rec = &groupRecord{path: path, hostID: strings.TrimSuffix(filepath.Base(path), groupsSuffix), f: f, st: st, note: note}
	rec.groups = parseRecord(b, path, note)
	rec.owner, rec.mixed = recordOwner(rec.groups)
	return rec, false
}

// readRecordFile opens path, never through a link, and reads it: an error for
// anything but a regular file of this user's. One past the size a host could write
// is read only as far as that: its size says the rest.
func readRecordFile(path string) (*os.File, fs.FileInfo, []byte, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	st, err := f.Stat()
	if err == nil && (!st.Mode().IsRegular() || !ownedBy(st, os.Geteuid())) {
		err = errors.New("not a record")
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, recordBytes))
	if err != nil {
		_ = f.Close()
		return nil, nil, nil, err
	}
	return f, st, b, nil
}

// ownedBy reports whether st is a file uid owns. The directory it is in is this
// user's alone (rundir.ExistingHostLogDir), so this answers only a file put there
// by root, and whatever it says, nothing another user wrote is acted on.
func ownedBy(st fs.FileInfo, uid int) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == uid
}

// finish lets the record go, removing the file unless something in it stays. What
// stays because it cannot be told goes once it has aged. What stays for a reason the
// next run cures does not: the caller was in a group, or this run was cut short.
func (r *groupRecord) finish() {
	if !r.keep || (!r.stay && time.Since(r.st.ModTime()) >= RecordKeep) {
		r.unlink()
	}
	r.release()
}

// unlink removes the file at the record's name if it is still what was read, by its
// inode and by its owner, read again under the lock. A host id can be used again, and
// another host's record at this name, written while this one was at work, is not
// this one's to remove. What stays open is the instant between this read and the
// unlink.
func (r *groupRecord) unlink() {
	if now, err := os.Lstat(r.path); err != nil || !os.SameFile(r.st, now) {
		return
	}
	if _, err := r.f.Seek(0, io.SeekStart); err != nil {
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.f, recordBytes))
	if err != nil {
		return
	}
	if owner, mixed := recordOwner(parseRecord(b, r.path, func(string, ...any) {})); owner != r.owner || mixed != r.mixed {
		return
	}
	_ = os.Remove(r.path)
}

// release lets the record go as it is.
func (r *groupRecord) release() { _ = r.f.Close() }

// hostVerdict is what examine makes of the host a record names.
type hostVerdict int

const (
	// hostLeave: running, or not to be acted on yet. The record is left.
	hostLeave hostVerdict = iota
	// hostDead: gone, here, so what the record names can be acted on.
	hostDead
	// hostDrop: nothing can ever act on it, and it has aged. It is removed, and
	// nothing it names is signalled.
	hostDrop
)

// verdict reads the host a record's lines name, in the boot and PID namespace given:
// its identity, read now, is what says it has gone. A host mid-exit is running, and
// removes its own record. A record of another boot or namespace, or of lines that
// disagree or an identity that cannot be read, is one nothing here can act on: it
// goes when it has aged. With no owner at all (an older craze's), there is nothing
// to act on, and it goes when it has aged and its host's lock is free.
func verdict(env rundir.Env, id string, owner GroupOwner, mixed bool, modTime time.Time, boot, pidNS string) hostVerdict {
	aged := func(age time.Duration) hostVerdict {
		if time.Since(modTime) >= age {
			return hostDrop
		}
		return hostLeave
	}
	switch {
	case mixed:
		return aged(RecordKeep)
	case owner == (GroupOwner{}):
		if !rundir.HostLockFree(env, id) {
			return hostLeave
		}
		return aged(bareRecordAge)
	case owner.Boot != boot || owner.PIDNS != pidNS:
		return aged(RecordKeep)
	}
	host, err := rundir.ProcessIdentity(owner.PID)
	switch {
	case errors.Is(err, rundir.ErrNoProcess):
		return hostDead
	case err != nil:
		return aged(RecordKeep)
	case host.Zombie || host.Start != owner.Start:
		return hostDead
	}
	return hostLeave
}

// examine decides about the record of host id, and answers it locked and read when
// its host has gone. busy says another has its lock. The read that names the owner
// comes first and decides nothing but that: the host may write again until it dies.
// Only with its death established is the file taken, by its name, locked and read
// for what it holds, the host's atomic replacement having had its chance to swap it
// meanwhile and a dead one having no more writes to make.
func examine(env rundir.Env, dir, id, boot, pidNS string, note noteFunc) (rec *groupRecord, busy bool) {
	path := filepath.Join(dir, AgentGroupsName(id))
	f, st, b, err := readRecordFile(path)
	if err != nil {
		return nil, false
	}
	_ = f.Close()
	owner, mixed := recordOwner(parseRecord(b, path, func(string, ...any) {}))
	oversize := st.Size() > recordBytes
	recordPeeked(path)
	v := verdict(env, id, owner, mixed || oversize, st.ModTime(), boot, pidNS)
	if v == hostLeave {
		return nil, false
	}
	rec, busy = openRecord(path, note)
	if rec == nil {
		return nil, busy
	}
	if rec.owner != owner || rec.mixed != mixed || (rec.st.Size() > recordBytes) != oversize {
		// Replaced by something else since: the next look decides about that.
		rec.release()
		return nil, false
	}
	if v == hostDrop {
		rec.unlink()
		rec.release()
		return nil, false
	}
	if rec.mixed {
		rec.release()
		return nil, false
	}
	return rec, false
}

// hostNote is what a reap says about host id's record: on the host's own log, which
// a sweep may be the last to write to, and to also (nil: nobody).
func hostNote(dir, id string, also noteFunc) noteFunc {
	log := filepath.Join(dir, id+".log")
	return func(format string, args ...any) {
		if also != nil {
			also(format, args...)
		}
		f, err := os.OpenFile(log, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = fmt.Fprintf(f, "craze: "+format+"\n", args...)
	}
}

// SweepDeadHosts ends what dead hosts in env's host logs left running: every
// record whose host has gone, up to SweepHostsMax of them from wherever this pass
// starts, in one pass and one grace however many there are, and removes each. Cheap
// on a healthy machine: a listing, and two small reads per running host. It usually
// takes SweepBound at most, and never returns an error: note may be nil, and nothing
// about it can fail the command that ran it.
func SweepDeadHosts(env rundir.Env, note func(format string, args ...any)) {
	dir, err := rundir.ExistingHostLogDir(env)
	if err != nil {
		return
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var ids []string
	for _, de := range names {
		if id, ok := strings.CutSuffix(de.Name(), groupsSuffix); ok && rundir.ValidHostID(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	recordsListed()
	// Only now: a listing that is slow must not spend what examining a record needs,
	// or a big directory would make no progress however many passes came.
	begun := time.Now()
	boot, pidNS, err := rundir.ProcessScope()
	if err != nil {
		return
	}
	start := sweepStart(len(ids))
	var recs []*groupRecord
	for i := range ids {
		if len(recs) >= SweepHostsMax || time.Since(begun) >= sweepRead {
			break
		}
		id := ids[(start+i)%len(ids)]
		if rec, _ := examine(env, dir, id, boot, pidNS, hostNote(dir, id, note)); rec != nil {
			recs = append(recs, rec)
		}
	}
	if len(recs) > 0 {
		r := &reaper{ctx: context.Background(), firstBy: begun.Add(2 * sweepRead), grace: ReapGrace}
		r.second(recs, r.first(recs))
	}
}

// ReapHost ends what the host id left running, now, for a caller that has seen it
// gone. Another sweep at the same record means the work is under way: it waits for
// that one to finish, at most SweepBound and a read, and acts itself if that one
// left the record there. So when it returns with ctx not done, each group the record
// named has been signalled to its end, unless the record was kept. A ctx done returns at once, and leaves the record for the next sweep:
// what has been signalled so far has, and nothing more. The record's own owner is
// still what decides, so a caller wrong about the host costs nothing.
func ReapHost(ctx context.Context, env rundir.Env, id string, note func(format string, args ...any)) {
	dir, err := rundir.ExistingHostLogDir(env)
	if err != nil || !rundir.ValidHostID(id) {
		return
	}
	boot, pidNS, err := rundir.ProcessScope()
	if err != nil {
		return
	}
	for deadline := time.Now().Add(reapLockWait); ; {
		rec, busy := examine(env, dir, id, boot, pidNS, hostNote(dir, id, note))
		if rec != nil {
			r := &reaper{ctx: ctx, firstBy: time.Now().Add(2 * sweepRead), grace: ReapGrace}
			r.second([]*groupRecord{rec}, r.first([]*groupRecord{rec}))
			return
		}
		if !busy || !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(groupsPoll):
		}
	}
}

// NewGroupOwner is this process as the owner of a record it writes: an error when
// its identity or its scope cannot be read. Such a host can write no shell command
// down, and writes an agent's line with no owner.
func NewGroupOwner() (GroupOwner, error) {
	id, err := rundir.ProcessIdentity(os.Getpid())
	if err != nil {
		return GroupOwner{}, err
	}
	boot, pidNS, err := rundir.ProcessScope()
	if err != nil {
		return GroupOwner{}, err
	}
	o := GroupOwner{PID: os.Getpid(), Start: id.Start, Boot: boot, PIDNS: pidNS}
	if !o.valid() {
		return GroupOwner{}, fmt.Errorf("the identity %+v", o)
	}
	return o, nil
}
