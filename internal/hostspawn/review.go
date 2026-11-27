package hostspawn

import (
	"context"
	"fmt"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/rundir"
)

// ReviewMark follows an adapter through ordinary fork/exec, including when
// a child starts another POSIX session. It carries no caller credentials.
func ReviewMark(hostID string) string { return "CRAZE_REVIEW_HOST=" + hostID }

// ReviewScope records which sessions predate this host and which processes
// pre-existed the attempt. A newly detached helper can drop the environment
// marker. An observed parent chain proves ownership; an orphan whose origin
// cannot be proved blocks fallback without authorizing a signal.
type ReviewScope struct {
	before      map[int]uint64
	sessions    map[int]bool
	reaper      bool
	hostVersion int32
	known       map[int]uint64
}

func reviewMembers(deadline time.Time) (map[int]rundir.GroupMember, error) {
	groups, err := rundir.ProcessGroups(deadline)
	if err != nil {
		return nil, err
	}
	members := make(map[int]rundir.GroupMember)
	for _, group := range groups {
		for _, m := range group {
			members[m.PID] = m
		}
	}
	return members, nil
}

func NewReviewScope(ctx context.Context, standalone bool) (*ReviewScope, error) {
	deadline, _ := ctx.Deadline()
	members, err := reviewMembers(deadline)
	if err != nil {
		return nil, err
	}
	reaper, err := enableReviewAdoption(standalone)
	if err != nil {
		return nil, err
	}
	s := &ReviewScope{reaper: reaper, before: map[int]uint64{}, sessions: map[int]bool{}, known: map[int]uint64{}}
	for _, m := range members {
		s.before[m.PID] = m.Start
		s.sessions[m.Session] = true
	}
	return s, nil
}

// Bind captures the host's kernel generation before it can finish a turn.
func (s *ReviewScope) Bind(host *Child) { s.hostVersion = reviewParentVersion(host.PID()) }

// origin classifies a member of a new session against this attempt. Parent
// identities come from one scan. Take no action on an ambiguous identity.
func (s *ReviewScope) origin(m rundir.GroupMember, members map[int]rundir.GroupMember, host *Child) (owned, uncertain bool) {
	for hops := 0; hops <= len(members); hops++ {
		if m.Session == host.PID() && m.Start >= host.ownStart || s.known[m.PID] == m.Start && m.Start != 0 {
			return true, false
		}
		if s.reaper && m.PPID == os.Getpid() && s.before[m.PID] != m.Start {
			return true, false
		}
		if m.PPID == 1 {
			// Linux adopts our own orphans directly. On Darwin a launchd job
			// whose original parent predates the host is independently owned.
			return false, !s.reaper && !reviewOlderParent(m.PID, s.hostVersion)
		}
		if s.sessions[m.Session] || s.before[m.PID] == m.Start {
			return false, false
		}
		parent, ok := members[m.PPID]
		if !ok || parent.Start > m.Start {
			return false, !s.reaper
		}
		m = parent
	}
	return false, true
}

// Observe associates detached descendants with their start times while their
// original parent is still alive, without making a kill decision.
func (s *ReviewScope) Observe(ctx context.Context, host *Child) error {
	deadline, _ := ctx.Deadline()
	members, err := reviewMembers(deadline)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.Zombie || m.Start < host.ownStart || s.sessions[m.Session] {
			continue
		}
		if owned, _ := s.origin(m, members, host); owned {
			s.known[m.PID] = m.Start
		}
	}
	return nil
}

// StopReviewProcesses runs after the direct host has been reaped. Its POSIX
// session contains adapter-created groups even after their parent exits.
// Recorded shell groups and the attempt marker also cover new sessions.
func (s *ReviewScope) StopReviewProcesses(ctx context.Context, host *Child, hostID string, ids []int) error {
	deadline, _ := ctx.Deadline()
	mark := ReviewMark(hostID)
	quiet := false
	for {
		members, err := reviewMembers(deadline)
		if err != nil {
			return fmt.Errorf("could not verify review processes: %w", err)
		}
		if host.ownStart == 0 {
			return fmt.Errorf("could not verify review host identity")
		}
		if leader, exists := members[host.PID()]; exists && leader.Start != host.ownStart {
			return fmt.Errorf("review host identity was reused")
		}
		live := false
		for _, m := range members {
			if m.Zombie || m.Start < host.ownStart {
				continue
			}
			if slices.Contains(ids, m.PGID) {
				live = true
			}
			if s.sessions[m.Session] {
				continue
			}
			owned, uncertain := s.origin(m, members, host)
			if !owned && uncertain && hostID != "" {
				// A readable environment without the marker cannot establish the
				// origin of an orphan either. Keep it alive and fail verification.
				owned, _ = rundir.ProcessCarries(m.PID, mark)
			}
			if owned {
				live = true
				s.known[m.PID] = m.Start
				signalHost(m.PID, m.Start, syscall.SIGKILL, false)
			} else if uncertain && syscall.Kill(m.PID, 0) != syscall.EPERM {
				live = true
			}
		}
		if s.reaper {
			gone, err := reviewChildrenGone()
			if err != nil {
				return fmt.Errorf("verifying adopted review children: %w", err)
			}
			if !live && gone {
				return nil
			}
		} else if !live && quiet {
			return nil
		}
		quiet = !live
		select {
		case <-ctx.Done():
			return fmt.Errorf("could not confirm review process exit: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}
