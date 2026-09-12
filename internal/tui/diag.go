package tui

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// diagOut is where craze's own diagnostics go. It is os.Stderr for everything
// that does not own the terminal, and the caller's buffer for the duration of
// Run: under the alt screen a write to the real stderr is painted over the
// frame bubbletea just drew, with no lock between the two.
//
// Guarded because the skill scan writes from the Update goroutine while Run
// installs and restores the seam from the one that called it.
var (
	diagMu  sync.Mutex
	diagOut io.Writer = os.Stderr
)

// setDiag installs a diagnostics writer and returns the previous one.
func setDiag(w io.Writer) io.Writer {
	diagMu.Lock()
	defer diagMu.Unlock()
	prev := diagOut
	if w == nil {
		w = os.Stderr
	}
	diagOut = w
	return prev
}

func diagf(format string, args ...any) {
	diagMu.Lock()
	defer diagMu.Unlock()
	fmt.Fprintf(diagOut, format, args...)
}
