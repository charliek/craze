//go:build !linux && !darwin

package rundir

import "syscall"

// PeerGone has no implementation off Linux and macOS: it is never supported,
// so its callers never ask again and keep on as they would without it.
func PeerGone(syscall.RawConn) (gone, supported bool) {
	return false, false
}
