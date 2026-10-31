package hostspawn

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"
)

// A host's record of what it runs in process groups of their own (plan 030 §3.4,
// R2-2, X22): <host-logs>/<hostId>.pgids, one AgentGroup line per group. An ACP
// agent's line is appended as the agent is spawned and goes when the host's stop
// sequence has ended them all; a shell command's is appended as the command starts
// and goes when its group has been reaped. An ACP agent leads a process group of
// its own and may outlive its pipes closing, and a shell command leads a session;
// a host killed outright can end neither. What may act on the record once the host
// has gone is in reap.go. The host writes the record (craze serve's agentGroups);
// this is its format and its last resort.

// AgentGroupsName is a host's record of its process groups, in the host logs'
// directory beside its log.
func AgentGroupsName(hostID string) string { return hostID + groupsSuffix }

// groupsSuffix is a record's name past the host's id.
const groupsSuffix = ".pgids"

// AgentGroupsMax bounds how many entries a record holds, and how many a reader
// acts on. A host's entries are what is running now: one per agent, one per shell
// command, so a file past it is not a host's. A host that reaches it runs a command
// without writing it down (agentGroups.recordShell), which is a leak if the host
// is then killed; the docs say so.
const AgentGroupsMax = 1000

// The kinds of a record's entry.
const (
	// GroupAgent is an ACP agent's group (acp.Spawn's Setpgid).
	GroupAgent = "agent"
	// GroupShell is a native shell command's group, or ripgrep's: a session and a
	// group, Setsid, whose leader's pid is both ids, and CommandMark in the
	// environment of everything it starts.
	GroupShell = "shell"
)

// GroupOwner is the host a record's entry belongs to, and where that means
// anything: its pid and start time (rundir.ProcessIdentity), and the boot and
// the PID namespace a number is meaningful in (rundir.ProcessScope). What tells
// a sweep the host has died, and keeps it off a host still running, or one on
// another boot or in another namespace using the same home directory.
type GroupOwner struct {
	PID   int
	Start uint64
	Boot  string
	PIDNS string
}

func (o GroupOwner) valid() bool {
	return o.PID > 1 && o.PID <= math.MaxInt32 && o.Start > 0 && o.Boot != "" && o.PIDNS != "" &&
		!strings.ContainsAny(o.Boot+o.PIDNS, " \t\n")
}

// AgentGroup is one entry in a host's record: the process group, and that group's
// leader's start time (rundir.ProcIdentity), which is what makes the number the
// group's and nobody else's (astra r5-c3 1). A pid, and the group id with it, is
// free for the next process once its own has gone and been reaped; a start time
// cannot be the next process's, which started later. An ACP agent is spawned
// leading a group of its own, and a shell command too, so the group's id is the
// leader's pid. Kind and Owner are empty in an older craze's record, one whose
// host could not read its scope, and an intermediate's report (reparent.go).
type AgentGroup struct {
	PGID  int
	Start uint64
	Kind  string
	Owner GroupOwner
}

// Line is g as its record's line, one write(2): "<pgid> <start>\n", or with an
// owner "<pgid> <start> <kind> <host pid> <host start> <boot> <pid namespace>\n".
func (g AgentGroup) Line() string {
	line := strconv.Itoa(g.PGID) + " " + strconv.FormatUint(g.Start, 10)
	if g.Kind != "" {
		line += " " + g.Kind + " " + strconv.Itoa(g.Owner.PID) + " " + strconv.FormatUint(g.Owner.Start, 10) + " " + g.Owner.Boot + " " + g.Owner.PIDNS
	}
	return line + "\n"
}

// ParseAgentGroup reads one record line (Line): a group id a pid_t can hold and a
// start time, and with them either nothing or a known kind and a whole owner;
// false for anything else — a blank line, or one written by anything but
// AgentGroup.Line.
func ParseAgentGroup(line string) (AgentGroup, bool) {
	f := strings.Fields(line)
	if len(f) != 2 && len(f) != 7 {
		return AgentGroup{}, false
	}
	pgid, err := strconv.Atoi(f[0])
	if err != nil || pgid <= 0 || pgid > math.MaxInt32 {
		return AgentGroup{}, false
	}
	start, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return AgentGroup{}, false
	}
	g := AgentGroup{PGID: pgid, Start: start}
	if len(f) == 2 {
		return g, true
	}
	hostPID, err := strconv.Atoi(f[3])
	if err != nil {
		return AgentGroup{}, false
	}
	hostStart, err := strconv.ParseUint(f[4], 10, 64)
	if err != nil {
		return AgentGroup{}, false
	}
	g.Kind = f[2]
	g.Owner = GroupOwner{PID: hostPID, Start: hostStart, Boot: f[5], PIDNS: f[6]}
	if (g.Kind != GroupAgent && g.Kind != GroupShell) || g.Start == 0 || !g.Owner.valid() {
		return AgentGroup{}, false
	}
	return g, true
}

// KillRecordedAgents ends every process group the record at path names and removes
// the record: a spawner's last resort after its own child, the host, has gone, or a
// stopping host's own. pid and start name that host (start 0: not known). A host id
// can be used again, so what is at the path may be another host's by now: lines
// whose owner is not this host, or that disagree, are left alone and the file with
// them. A line with no owner has nothing to say about that, and is acted on here and
// nowhere else. It takes the record's lock as a sweep does, and leaves a record a
// sweep holds to that sweep. A group is signalled only while it is provably the group
// written down (reaper.fate), and each one left alone is noted, as is a line that
// names none. Never 0 or 1 or this process's own group, and at most AgentGroupsMax
// records. A shell command's entry that could not be told keeps the record, for the
// sweep that can.
//
// With a grace (a stopping host's own last resort, craze serve's
// serveHost.killOwnAgents) each group is sent SIGTERM first, and SIGKILL once the
// grace is over if it is still the group written down, read again, since its leader
// may have exited meanwhile and its number gone to another; this returns when that is
// done. Without one (the spawner, after its host has gone) an agent has SIGKILL at
// once, and a shell command, which a host killed outright gave no chance to stop, has
// SIGTERM and then SIGKILL after ReapGrace. That part goes on behind the return, so
// no spawner's watch waits on it; a spawner that exits first leaves the record for
// the next sweep.
func KillRecordedAgents(path string, pid int, start uint64, grace time.Duration, note func(format string, args ...any)) {
	rec, _ := openRecord(path, note)
	if rec == nil {
		return
	}
	if o := rec.owner; rec.mixed || (o != (GroupOwner{}) && (o.PID != pid || (start != 0 && o.Start != start))) {
		note("a record at %s that is not this host's (%d) left alone", path, pid)
		rec.release()
		return
	}
	recs := []*groupRecord{rec}
	r := &reaper{ctx: context.Background(), firstBy: time.Now().Add(2 * sweepRead), grace: grace, direct: true, agentsAtOnce: grace == 0}
	if grace == 0 {
		r.grace = ReapGrace
	}
	termed := r.first(recs)
	if grace == 0 && len(termed) > 0 {
		go r.second(recs, termed)
		return
	}
	r.second(recs, termed)
}
