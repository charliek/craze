//go:build darwin

package rundir

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// peerCred reads LOCAL_PEERCRED for the peer's effective uid (what
// getpeereid(3) returns) and LOCAL_PEERPID for its pid. A uid that cannot be
// read, or a struct xucred that cannot be trusted to name one (xucredUID), is
// an error; a pid that cannot be read is 0, not an error: the pid is only
// noted, never checked.
func peerCred(fd int) (pid, uid int, err error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, noUID, fmt.Errorf("LOCAL_PEERCRED: %w", err)
	}
	uid, err = xucredUID(cred.Version, cred.Uid, cred.Ngroups)
	if err != nil {
		return 0, noUID, fmt.Errorf("LOCAL_PEERCRED: %w", err)
	}
	pid, err = unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	if err != nil || pid < 0 {
		pid = 0
	}
	return pid, uid, nil
}
