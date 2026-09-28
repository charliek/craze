//go:build linux || darwin

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal is whether f is a terminal: whether it answers the ioctl that
// reads a terminal's attributes. A character device that is not a terminal
// (/dev/null) does not, where a mode check would have taken it for one.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), getTermios)
	return err == nil
}
