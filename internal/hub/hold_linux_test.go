//go:build linux

package hub

import (
	"syscall"
	"time"
	"unsafe"
)

// sysCloseRange is close_range(2)'s number, the same on amd64 and arm64.
const sysCloseRange = 436

// holdThread holds the calling thread for d where no stop signal reaches
// it: the parent's side of a vfork, a killable wait that only the child's
// exit — d later — or SIGKILL ends. A process-directed SIGSTOP picks the main
// thread to start the group stop when that thread can take it (the kernel's
// complete_signal), so a main thread held here keeps the whole process
// running, and answering, until the hold ends: the late stop a loaded
// machine makes, forced (TestEnsureReplacesASIGSTOPpedHub).
//
// The child is a copy of a multi-threaded runtime with one thread in it: it
// makes raw system calls and nothing else — all signals blocked, every
// descriptor closed (no copy of the hub's lock or socket outlives the hold),
// a sleep, an exit.
//
//go:norace
func holdThread(d time.Duration) {
	ts := syscall.NsecToTimespec(int64(d))
	all := ^uint64(0)
	pid, _, errno := syscall.RawSyscall6(syscall.SYS_CLONE, syscall.CLONE_VFORK|uintptr(syscall.SIGCHLD), 0, 0, 0, 0, 0)
	if errno != 0 {
		return
	}
	if pid == 0 {
		syscall.RawSyscall6(syscall.SYS_RT_SIGPROCMASK, 2 /* SIG_SETMASK */, uintptr(unsafe.Pointer(&all)), 0, 8, 0, 0)
		syscall.RawSyscall(sysCloseRange, 0, uintptr(^uint32(0)), 0)
		syscall.RawSyscall(syscall.SYS_NANOSLEEP, uintptr(unsafe.Pointer(&ts)), 0, 0)
		syscall.RawSyscall(syscall.SYS_EXIT_GROUP, 0, 0, 0)
	}
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(int(pid), &ws, 0, nil)
}
