//go:build darwin

package cli

import "golang.org/x/sys/unix"

// getTermios is the ioctl that reads a terminal's attributes (isTerminal);
// setTermios writes them at once (echoOff, plan 031 §3.7).
const (
	getTermios = unix.TIOCGETA
	setTermios = unix.TIOCSETA
)
