//go:build !linux && !darwin

package cli

import "os"

// isTerminal has no termios read off Linux and macOS, so it takes a
// character device for a terminal, as craze always did there.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
