//go:build !linux && !darwin

package rundir

import (
	"fmt"
	"runtime"
)

// peerCred has no implementation off Linux and macOS, so every lookup fails
// and every peer check refuses: craze never serves or trusts a peer it cannot
// name.
func peerCred(int) (pid, uid int, err error) {
	return 0, noUID, fmt.Errorf("peer credentials are not implemented on %s", runtime.GOOS)
}
