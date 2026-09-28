//go:build darwin

package cli

import "golang.org/x/sys/unix"

// getTermios is the ioctl that reads a terminal's attributes (isTerminal).
const getTermios = unix.TIOCGETA
