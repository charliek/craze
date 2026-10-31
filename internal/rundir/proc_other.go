//go:build !linux && !darwin

package rundir

import (
	"fmt"
	"runtime"
	"time"
)

// processIdentity has no implementation off Linux and macOS: every read
// fails, so a detached host records no agent (and its start fails), and a
// spawner signals no recorded group — never one it cannot prove is its own.
func processIdentity(int) (ProcIdentity, error) {
	return ProcIdentity{}, fmt.Errorf("process identities are not implemented on %s", runtime.GOOS)
}

// processTokenScope has no implementation off Linux and macOS either: no
// start token is ever made, and none is ever carried.
func processTokenScope() (string, string, error) {
	return "", "", fmt.Errorf("start token scopes are not implemented on %s", runtime.GOOS)
}

// processGroups has no implementation off Linux and macOS: a scan says nothing,
// so no group is signalled on its word.
func processGroups(time.Time) (map[int][]GroupMember, error) {
	return nil, fmt.Errorf("process scans are not implemented on %s", runtime.GOOS)
}

// processMember has no implementation off Linux and macOS.
func processMember(int) (GroupMember, error) {
	return GroupMember{}, fmt.Errorf("process reads are not implemented on %s", runtime.GOOS)
}

// processCarries is not known off Linux and macOS: nothing can be read.
func processCarries(int, string) (carries, known bool) { return false, false }
