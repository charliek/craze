//go:build !linux && !darwin

package cli

import (
	"errors"
	"os"
)

// isTerminal has no termios read off Linux and macOS, so it takes a
// character device for a terminal, as craze always did there.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// terminalWidth has no window-size ioctl off Linux and macOS: craze ps's
// title column takes its fixed width there.
func terminalWidth(*os.File) (int, bool) { return 0, false }

// echoOff has no termios to change off Linux and macOS (terminal.go).
type echoOff struct{}

// quiet cannot turn a terminal's echo off here, so craze auth login reads a
// key from stdin only.
func quiet(*os.File) (*echoOff, error) {
	return nil, errors.New("craze cannot turn this terminal's echo off; pipe the key on stdin")
}

// muteEcho cannot either, so the ChatGPT sign-in reads a pasted redirect
// from stdin only, as a key is.
func muteEcho(*os.File) (*echoOff, error) {
	return nil, errors.New("craze cannot turn this terminal's echo off; pipe the redirect address on stdin")
}

func (*echoOff) restore() {}
