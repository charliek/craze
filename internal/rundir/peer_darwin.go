//go:build darwin

package rundir

import (
	"errors"
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// xucredVersion is XUCRED_VERSION (<sys/ucred.h>), the only struct xucred
// layout the kernel has written; x/sys/unix does not export it. libc's
// getpeereid refuses any other version, and so does peerCred.
const xucredVersion = 0

// peerCred reads LOCAL_PEERCRED for the peer's effective uid (what
// getpeereid(3) returns) and LOCAL_PEERPID for its pid. A uid that cannot be
// read is an error; a pid that cannot be read is 0, not an error: the pid is
// only noted, never checked.
func peerCred(fd int) (pid, uid int, err error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, noUID, fmt.Errorf("LOCAL_PEERCRED: %w", err)
	}
	if cred.Version != xucredVersion {
		return 0, noUID, fmt.Errorf("LOCAL_PEERCRED: xucred version %d, want %d", cred.Version, xucredVersion)
	}
	if cred.Uid == math.MaxUint32 {
		return 0, noUID, errors.New("LOCAL_PEERCRED: the socket has no peer credentials")
	}
	pid, err = unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	if err != nil || pid < 0 {
		pid = 0
	}
	return pid, int(cred.Uid), nil
}
