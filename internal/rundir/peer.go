package rundir

import (
	"errors"
	"fmt"
	"math"
	"net"
)

// Peer credentials (plan 027 §3.8, "Peer uid on both ends"). The socket's
// 0700 directory and 0600 mode stop another user opening it on this machine,
// and nothing once the socket is reached some other way: an SSH-forwarded
// Unix socket is opened by sshd running as the forwarding user, so the file
// mode says nothing about who is at the other end. The peer's effective uid,
// read from the kernel, does. So the server checks every accepted connection
// before it reads a byte of it (PeerCheck, control.Options.PeerCheck), and a
// client checks the server right after the dial, before it writes a byte
// (DialCheck, remote.Options.PeerCheck; craze bridge).
//
// Every lookup failure is a refusal, never an allow: a connection whose owner
// cannot be named is exactly the one not to trust (roost's peer.rs). Linux
// reads SO_PEERCRED (peer_linux.go), macOS LOCAL_PEERCRED for the uid and
// LOCAL_PEERPID for the pid (peer_darwin.go); any other OS refuses every
// connection (peer_other.go). Both report the peer's effective uid as it was
// when the connection was made.

// ErrPeerUID is a peer check's error for a peer that runs as another user.
// The error that wraps it names both uids, and nothing else about the peer.
var ErrPeerUID = errors.New("rundir: the peer runs as another user")

// noUID is the uid a lookup reports when it names nobody: a failed lookup's.
// It is never a real uid, so it is never read as root's by mistake.
const noUID = -1

// macOS's struct xucred (<sys/ucred.h>), which LOCAL_PEERCRED fills.
// x/sys/unix exports neither constant.
const (
	// xucredVersion is XUCRED_VERSION, the only layout the kernel has
	// written; libc's getpeereid refuses any other version, and so does
	// xucredUID.
	xucredVersion = 0
	// xucredGroups is XU_NGROUPS, the most groups one carries.
	xucredGroups = 16
)

// xucredUID is the uid a LOCAL_PEERCRED result names (peer_darwin.go), or an
// error when its fields cannot be trusted to name anyone. It lives here, not
// in the darwin file, so every platform's tests check it.
//
// unix.GetsockoptXucred drops the length getsockopt wrote, so a call that
// succeeded having written nothing, or too little, would leave the zeroed
// struct it started from: version 0, which is XUCRED_VERSION, and uid 0,
// which is root's — and PeerCheck(0) would accept a peer whose uid was never
// read. The group count closes that without a getsockopt of craze's own
// (which would need unsafe): the kernel copies the peer's POSIX credential,
// whose groups always hold at least its effective gid, so a real xucred
// carries 1 to XU_NGROUPS of them, and the zeroed one carries none. A uid of
// (uid_t)-1 is a socket with no credentials.
func xucredUID(version, uid uint32, ngroups int16) (int, error) {
	switch {
	case version != xucredVersion:
		return noUID, fmt.Errorf("xucred version %d, want %d", version, xucredVersion)
	case ngroups < 1 || ngroups > xucredGroups:
		return noUID, fmt.Errorf("xucred carries %d groups, and a real one carries 1 to %d: no credentials were read", ngroups, xucredGroups)
	case uid == math.MaxUint32:
		return noUID, errors.New("the socket has no peer credentials")
	}
	return int(uid), nil
}

// PeerCred is the peer's pid (0 when the OS cannot say) and effective uid, as
// the kernel recorded them when the connection was made. On an error the pid
// is 0 and the uid negative. The descriptor is reached through SyscallConn's
// Control, never File(), which would dup it and leave it blocking.
func PeerCred(uc *net.UnixConn) (pid, uid int, err error) {
	if uc == nil {
		return 0, noUID, errors.New("rundir: peer credentials: no connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, noUID, fmt.Errorf("rundir: peer credentials: %w", err)
	}
	var lookupErr error
	if err := raw.Control(func(fd uintptr) { pid, uid, lookupErr = peerCred(int(fd)) }); err != nil {
		return 0, noUID, fmt.Errorf("rundir: peer credentials: %w", err)
	}
	if lookupErr != nil {
		return 0, noUID, fmt.Errorf("rundir: peer credentials: %w", lookupErr)
	}
	return pid, uid, nil
}

// PeerCheck is the server's accept check, control.Options.PeerCheck: the
// peer's uid must be want, and a failed lookup is a refusal. It returns what
// the lookup named — the pid and uid, which the server notes, the uid negative
// when the lookup failed — and, for a peer it refuses, an error: ErrPeerUID
// naming both uids, or the lookup's own.
func PeerCheck(want int) func(*net.UnixConn) (pid, uid int, err error) {
	return PeerCheckWith(want, nil)
}

// PeerCheckWith is PeerCheck over an injected lookup, for tests; nil is
// PeerCred. A lookup that errs, or that names a negative uid, is a refusal
// whose uid is negative; a negative want refuses everyone.
func PeerCheckWith(want int, lookup func(*net.UnixConn) (pid, uid int, err error)) func(*net.UnixConn) (pid, uid int, err error) {
	if lookup == nil {
		lookup = PeerCred
	}
	return func(uc *net.UnixConn) (int, int, error) {
		pid, uid, err := lookup(uc)
		switch {
		case err != nil:
			return 0, noUID, err
		case uid < 0:
			return 0, noUID, errors.New("rundir: peer credentials: the lookup named no uid")
		case want < 0:
			return pid, uid, fmt.Errorf("%w: peer uid %d, and no uid is allowed", ErrPeerUID, uid)
		case uid != want:
			return pid, uid, fmt.Errorf("%w: peer uid %d, want %d", ErrPeerUID, uid, want)
		}
		return pid, uid, nil
	}
}

// DialCheck is a client's check of the server it dialed,
// remote.Options.PeerCheck: PeerCheck's rule, for a caller that has no use
// for the pid.
func DialCheck(want int) func(*net.UnixConn) error {
	return DialCheckWith(want, nil)
}

// DialCheckWith is DialCheck over an injected lookup, for tests; nil is
// PeerCred.
func DialCheckWith(want int, lookup func(*net.UnixConn) (pid, uid int, err error)) func(*net.UnixConn) error {
	check := PeerCheckWith(want, lookup)
	return func(uc *net.UnixConn) error {
		_, _, err := check(uc)
		return err
	}
}
