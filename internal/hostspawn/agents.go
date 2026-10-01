package hostspawn

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/rundir"
)

// A spawned host's record of its agents' process groups (plan 030 §3.4, R2-2,
// X22): <host-logs>/<hostId>.pgids, one AgentGroup line per agent, which the
// host appends as each agent is spawned and removes once its stop sequence has
// ended them all. An ACP agent leads a process group of its own and may
// outlive its pipes closing; a host its spawner has to kill outright — one
// that never answered, or would not stop — cannot end it, and the spawner
// kills each group recorded there after it (Child.KillAgents), each only while
// its leader is the agent recorded. The host writes the record (craze serve's
// agentGroups); this is its format and its last resort.

// AgentGroupsName is a host's record of its agents' process groups, in the
// host logs' directory beside its log.
func AgentGroupsName(hostID string) string { return hostID + ".pgids" }

// agentGroupsMax bounds how many records a spawner acts on: a host spawns one
// agent per session start, so a file past it is not a host's.
const agentGroupsMax = 1000

// AgentGroup is one agent in a host's record: the process group it leads —
// an ACP agent is spawned leading one of its own (acp.Spawn's Setpgid), so
// the group's id is the agent's pid — and that leader's start time
// (rundir.ProcIdentity), which is what makes the number the agent's and
// nobody else's (astra r5-c3 1). A pid, and the group id with it, is free for
// the next process once its own has gone and been reaped; a start time
// cannot be the next process's, which started later.
type AgentGroup struct {
	PGID  int
	Start uint64
}

// Line is g as its record's line: "<pgid> <start>\n", one write(2).
func (g AgentGroup) Line() string {
	return strconv.Itoa(g.PGID) + " " + strconv.FormatUint(g.Start, 10) + "\n"
}

// ParseAgentGroup reads one record line: two decimal fields, a group id a
// pid_t can hold and a start time; false for anything else — a blank line,
// or one written by anything but AgentGroup.Line.
func ParseAgentGroup(line string) (AgentGroup, bool) {
	f := strings.Fields(line)
	if len(f) != 2 {
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
	return AgentGroup{PGID: pgid, Start: start}, true
}

// stale says why g may no longer be the agent's group, "" when it is: the
// process with the group's number — its leader, the agent — must still exist
// and have started when the agent did. A leader that has exited leaves a
// group, if anything is left in it, whose number cannot be told from a
// stranger's that has taken it since; a leader with another start time is a
// stranger, its group whatever that stranger's is. Either is left alone. What
// stays open is the instant between this read and the signal: the leader
// would have to exit, be reaped and have its number taken by a new group
// leader in it (plan 030 X22).
func (g AgentGroup) stale() string {
	id, err := rundir.ProcessIdentity(g.PGID)
	switch {
	case errors.Is(err, rundir.ErrNoProcess):
		return "its agent has exited, and a group whose leader has gone cannot be told from another's"
	case err != nil:
		return "its agent cannot be told: " + err.Error()
	case id.Start != g.Start:
		return fmt.Sprintf("its number is another process's now (started at %d, the agent at %d)", id.Start, g.Start)
	}
	return ""
}

// KillRecordedAgents kills every agent process group the record at path names
// and removes the record. A group is signalled only while it is provably the
// agent's (plan 030 X22, astra r5-c3 1): the process leading it — the agent,
// whose pid is the group's id — still there, and started at the instant the
// host recorded (AgentGroup.stale). A number an agent had can be anyone's once
// the agent has gone and been reaped; a group whose leader has exited or is a
// stranger is left alone, and note says so. Never 0 or 1 or this process's
// own group, and at most agentGroupsMax records.
//
// With a grace (a stopping host's own last resort, craze serve's
// serveHost.killOwnAgents) each group is sent SIGTERM first, and SIGKILL once
// the grace is over if its leader is still the agent — checked again, since
// the agent may have exited meanwhile and its number gone to another. Without
// one (the spawner, after its host has gone) SIGKILL at once.
func KillRecordedAgents(path string, grace time.Duration, note func(format string, args ...any)) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	own := syscall.Getpgrp()
	var ours []AgentGroup
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if n++; n > agentGroupsMax {
			note("agent process groups past the first %d in %s not killed", agentGroupsMax, path)
			break
		}
		g, ok := ParseAgentGroup(line)
		if !ok || g.PGID <= 1 || g.PGID == own {
			if len(line) > 64 {
				line = line[:64] + "…"
			}
			note("a record that names no agent's process group not acted on: %q", line)
			continue
		}
		if why := g.stale(); why != "" {
			note("agent process group %d not killed: %s", g.PGID, why)
			continue
		}
		ours = append(ours, g)
	}
	if grace > 0 && len(ours) > 0 {
		for _, g := range ours {
			_ = syscall.Kill(-g.PGID, syscall.SIGTERM)
		}
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) && anyAgentLeft(ours) {
			time.Sleep(agentExitPoll)
		}
	}
	for _, g := range ours {
		// Checked again after a grace: an agent that exited meanwhile, its
		// number since another's, is not signalled.
		if grace > 0 && g.stale() != "" {
			continue
		}
		_ = syscall.Kill(-g.PGID, syscall.SIGKILL)
	}
	_ = os.Remove(path)
}

// agentExitPoll is how often a grace looks for its agents to have gone.
const agentExitPoll = 20 * time.Millisecond

// anyAgentLeft reports whether any of gs still leads its group as the agent
// recorded.
func anyAgentLeft(gs []AgentGroup) bool {
	for _, g := range gs {
		if g.stale() == "" {
			return true
		}
	}
	return false
}
