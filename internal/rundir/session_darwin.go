//go:build darwin

package rundir

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// auditinfoAddr is <bsm/audit.h>'s struct auditinfo_addr, 48 bytes: the
// audit user and mask, the terminal id, the session id and the session's
// flags. x/sys has getaudit_addr's number and no wrapper.
type auditinfoAddr struct {
	Auid    uint32
	Success uint32
	Failure uint32
	Port    int32
	Type    uint32
	Addr    [4]uint32
	Asid    int32
	Flags   uint64
}

// auditSession reads this process's audit session with getaudit_addr(2): one
// system call, no cgo and no privilege. A call that fails is a session not
// known.
//
// It is a direct system call by number, which x/sys marks deprecated on
// macOS in favour of libSystem's wrappers — and x/sys has no wrapper for
// getaudit_addr; one would take cgo or an assembly trampoline of craze's own.
// The number has not moved (357), the call is proven on macOS 26 (plan 035's
// discovery), and should it ever fail the session is only not known: no hint
// is given and nothing is refused.
func auditSession() (flags uint32, gui bool, known bool) {
	var ai auditinfoAddr
	_, _, e := unix.Syscall(unix.SYS_GETAUDIT_ADDR, uintptr(unsafe.Pointer(&ai)), unsafe.Sizeof(ai), 0) //nolint:staticcheck // no libSystem wrapper for getaudit_addr (above)
	if e != 0 {
		return 0, false, false
	}
	// Every flag the kernel defines is in the low 32 bits.
	return uint32(ai.Flags), ai.Flags&AuditFlagGraphicAccess != 0, true
}
