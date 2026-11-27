//go:build darwin

package hostspawn

import (
	"syscall"
	"unsafe"
)

// Darwin's PROC_PIDUNIQIDENTIFIERINFO keeps the birth parent's version
// across reparenting. Kernel-assigned versions identify each fork/exec.
// This distinguishes services born to launchd from newly orphaned helpers.
func reviewVersions(pid int) (self, parent int32) {
	var info struct {
		uuid                           [16]byte
		unique, parentUnique           uint64
		version, originalParentVersion int32
		reserved                       [2]uint64
	}
	const procInfoCallPIDInfo, procPIDUniqIdentifierInfo = 2, 17
	n, _, e := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDUniqIdentifierInfo, 0, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if e != 0 || n != unsafe.Sizeof(info) {
		return 0, 0
	}
	return info.version, info.originalParentVersion
}
func reviewParentVersion(pid int) int32 { v, _ := reviewVersions(pid); return v }
func reviewOlderParent(pid int, hostVersion int32) bool {
	_, parent := reviewVersions(pid)
	return parent > 0 && hostVersion > 0 && parent < hostVersion
}
func enableReviewAdoption(bool) (bool, error) { return false, nil }
func reviewChildrenGone() (bool, error)       { return true, nil }
