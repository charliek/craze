//go:build linux

package hostspawn

import "golang.org/x/sys/unix"

// closeOnExecFrom marks every open descriptor from first up close-on-exec
// (closeOnExecInherited): close_range(2) with CLOSE_RANGE_CLOEXEC in one call
// (Linux 5.11), and on a kernel without it a walk of /proc/self/fd.
func closeOnExecFrom(first int) error {
	if err := unix.CloseRange(uint(first), ^uint(0), unix.CLOSE_RANGE_CLOEXEC); err == nil {
		return nil
	}
	return closeOnExecListed("/proc/self/fd", first)
}
