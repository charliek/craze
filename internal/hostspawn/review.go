package hostspawn

import (
	"context"
	"fmt"
	"slices"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/rundir"
)

// ReviewMark follows an adapter through ordinary fork/exec, including when
// a child starts another POSIX session. It carries no caller credentials.
func ReviewMark(hostID string) string { return "CRAZE_REVIEW_HOST=" + hostID }

// StopReviewProcesses runs after the direct host has been reaped. Its POSIX
// session contains adapter-created groups even after their parent exits.
// Recorded shell groups and the attempt marker also cover new sessions.
func StopReviewProcesses(ctx context.Context, host *Child, hostID string, ids []int) error {
	deadline, _ := ctx.Deadline()
	mark := ReviewMark(hostID)
	for {
		groups, err := rundir.ProcessGroups(deadline)
		if err != nil {
			return fmt.Errorf("could not verify review processes: %w", err)
		}
		sameSession := true
		for _, m := range groups[host.PID()] {
			if m.PID == host.PID() && m.Start != host.ownStart {
				sameSession = false
			}
		}
		live := false
		for pgid, members := range groups {
			for _, m := range members {
				if m.Zombie || host.ownStart > 0 && m.Start < host.ownStart {
					continue
				}
				// A recorded group number alone cannot authorize a signal after its
				// leader exits. The existing record reaper handles that case.
				if slices.Contains(ids, pgid) {
					live = true
				}
				owned := sameSession && m.Session == host.PID()
				if !owned && hostID != "" && host.ownStart > 0 {
					owned, _ = rundir.ProcessCarries(m.PID, mark)
				}
				if owned {
					live = true
					// Bind the signal to this member's identity, including when
					// the group leader has gone and its number could be reused.
					signalHost(m.PID, m.Start, syscall.SIGKILL, false)
				}
			}
		}
		if !live {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("could not confirm review process exit: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}
