//go:build linux

package hostspawn

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

func enableReviewAdoption(standalone bool) (bool, error) {
	if !standalone {
		return false, nil
	}
	return true, unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

// Once the direct host has been waited for, all other children of this
// single-command process are adopted descendants. ECHILD is a kernel check
// across forks and reparenting, even when /proc enumeration saw a handoff.
func reviewChildrenGone() (bool, error) {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.ECHILD):
			return true, nil
		case err != nil:
			return false, err
		case pid == 0:
			return false, nil
		}
	}
}
func reviewParentVersion(int) int32     { return 0 }
func reviewOlderParent(int, int32) bool { return false }
