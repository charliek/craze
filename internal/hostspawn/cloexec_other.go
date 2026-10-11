//go:build !linux

package hostspawn

// closeOnExecFrom marks every open descriptor from first up close-on-exec
// (closeOnExecInherited): macOS has no close_range, so a walk of /dev/fd,
// which lists this process's own descriptors.
func closeOnExecFrom(first int) error {
	return closeOnExecListed("/dev/fd", first)
}
