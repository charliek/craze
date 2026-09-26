//go:build linux

package rundir

import (
	"errors"
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// peerCred reads SO_PEERCRED: the pid and effective uid the kernel recorded
// for the peer when the connection was made. A socket with no peer
// credentials — never connected — reports the uid (uid_t)-1, which is a
// failed lookup here, never a uid.
func peerCred(fd int) (pid, uid int, err error) {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, noUID, fmt.Errorf("SO_PEERCRED: %w", err)
	}
	if cred.Uid == math.MaxUint32 {
		return 0, noUID, errors.New("SO_PEERCRED: the socket has no peer credentials")
	}
	return max(int(cred.Pid), 0), int(cred.Uid), nil
}
