//go:build linux

package acp

import (
	"sync/atomic"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// stubObserve puts a stub over the observation's system call (waitid) for the
// rest of the test: refuse, when set, answers every call; otherwise the first
// interrupts calls are answered EINTR and the rest made. It counts the calls.
func stubObserve(t *testing.T, interrupts int32, refuse error) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	real := waitid
	waitid = func(idType, id int, info *unix.Siginfo, options int, rusage *unix.Rusage) error {
		n := calls.Add(1)
		if refuse != nil {
			return refuse
		}
		if n <= interrupts {
			return unix.EINTR
		}
		return real(idType, id, info, options, rusage)
	}
	// Registered before any Spawn, so it runs after every Close.
	t.Cleanup(func() { waitid = real })
	return &calls
}

// TestCLDStatus: a child's exit, as waitid's si_code and si_status say it,
// reads as the wait status the reap's would.
func TestCLDStatus(t *testing.T) {
	for _, tc := range []struct {
		code, status int32
		want         string
	}{
		{cldExited, 0, "exit status 0"},
		{cldExited, 3, "exit status 3"},
		{cldExited, 255, "exit status 255"},
		{cldKilled, int32(syscall.SIGKILL), "signal: killed"},
		{cldKilled, int32(syscall.SIGTERM), "signal: terminated"},
		{cldDumped, int32(syscall.SIGSEGV), "signal: segmentation fault (core dumped)"},
	} {
		st, err := cldStatus(tc.code, tc.status)
		if err != nil {
			t.Fatalf("cldStatus(%d, %d): %v", tc.code, tc.status, err)
		}
		if got := (&ExitError{Status: st}).Error(); got != tc.want {
			t.Errorf("cldStatus(%d, %d) reads %q, want %q", tc.code, tc.status, got, tc.want)
		}
	}
	for _, code := range []int32{0, 4, 5, 6} { // not an exit: trapped, stopped, continued
		if _, err := cldStatus(code, 0); err == nil {
			t.Errorf("cldStatus(%d) took a stop or a continue for an exit", code)
		}
	}
}

// TestSigchldInfoLayout: the _sigchld member of siginfo_t's union starts
// where the union does, after the three int header aligned as a pointer is,
// and the whole fits in unix.Siginfo.
func TestSigchldInfoLayout(t *testing.T) {
	ptr := unsafe.Sizeof(uintptr(0))
	want := (12 + ptr - 1) &^ (ptr - 1)
	if got := unsafe.Offsetof(sigchldInfo{}.pid); got != want {
		t.Fatalf("si_pid at offset %d, want %d", got, want)
	}
	if unsafe.Offsetof(sigchldInfo{}.status) != want+8 {
		t.Fatalf("si_status at offset %d, want %d", unsafe.Offsetof(sigchldInfo{}.status), want+8)
	}
	if unsafe.Sizeof(sigchldInfo{}) > unsafe.Sizeof(unix.Siginfo{}) {
		t.Fatal("sigchldInfo is larger than the siginfo it reads")
	}
}
