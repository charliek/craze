//go:build !linux

package hostspawn

import "syscall"

// signalHost sends sig to a reparented host — the process group it leads,
// with group — only while pid is that host (plan 041 C6 r1, 4). macOS has
// no pidfd: the host's start time is checked, then the signal sent by number
// (checkThenSignal) — SF-120's window: the host would have to exit, be reaped
// and its number be taken in the instant between the two. bound is always
// false here.
func signalHost(pid int, start uint64, sig syscall.Signal, group bool) (sent, bound bool) {
	return checkThenSignal(pid, start, sig, group), false
}
