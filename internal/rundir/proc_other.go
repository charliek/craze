//go:build !linux && !darwin

package rundir

import (
	"fmt"
	"runtime"
)

// processIdentity has no implementation off Linux and macOS: every read
// fails, so a detached host records no agent (and its start fails), and a
// spawner signals no recorded group — never one it cannot prove is its own.
func processIdentity(int) (ProcIdentity, error) {
	return ProcIdentity{}, fmt.Errorf("process identities are not implemented on %s", runtime.GOOS)
}
