//go:build linux || darwin

package rundir

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// A peer gone (plan 035 C9, SF-123). craze bridge once its stdin has ended,
// and the hub's splice once its client has half-closed, each still owe a peer
// that only reads to the end every byte the session sends it (S2's
// half-close). A quiet session sends nothing, so a peer that has gone
// entirely is never found out by a failed write. PeerGone asks the kernel
// instead, writing nothing and waiting for nothing: poll(2) on the
// descriptor, for POLLOUT, with no timeout.
//
// It asks for POLLOUT, never for no events: macOS builds poll on kqueue and
// registers only the filters asked for, so a poll for no events reports
// nothing there, ever. Nor does it ask for POLLIN: a read filter reports a
// peer's write-shutdown, which is a half-close, not a peer gone. The answer is
// in the hang-up bits, the same rule on both platforms for different reasons:
//
//   - A pipe whose every reader has closed reports POLLERR on Linux, and
//     POLLHUP on macOS (its write filter's end-of-file).
//   - A Unix stream socket reports POLLHUP on Linux once both its directions
//     are shut, which the peer's close does and the peer's write-shutdown
//     alone does not (that ends only this side's reading); a close with bytes
//     still unread on the peer's side adds POLLERR. On macOS it reports
//     POLLHUP once this side can send no more, which the peer's close does and
//     its write-shutdown does not.
//
// Only those two kinds are supported, a pipe (or FIFO) and a Unix stream
// socket. A TCP socket whose peer closed gracefully sits in CLOSE_WAIT,
// writable, with neither POLLHUP nor POLLERR, so the probe would answer "not
// gone" for ever, and reading its FIN as "gone" would break a legitimate
// half-close. A datagram socket has no peer to be gone. Both are unsupported,
// which turns the probe off.
//
// POLLOUT itself, room to write, says nothing either way and is ignored. A
// socket this side has shut for writing itself is reported gone on macOS (and
// on Linux once the peer has shut its own): nothing could be sent to that peer
// anyway.

// PeerGone reports whether the peer at the other end of rc's descriptor (the
// reader of a pipe this process writes, or the other end of a Unix stream
// socket) has gone entirely. supported is false when it cannot tell, and the
// caller then stops asking, which is never worse than not asking at all: a
// descriptor that is neither a pipe (or FIFO) nor a Unix stream socket (a
// regular file, /dev/null, a terminal, a TCP or datagram socket), one poll calls invalid (POLLNVAL), or one that
// cannot be reached (rc closed) or read (fstat or poll failing). A poll
// interrupted by a signal has learned nothing: not gone, still supported, for
// the next look to ask again.
//
// The descriptor is reached through rc's Control, never os.File's Fd, which
// would switch it to blocking mode.
func PeerGone(rc syscall.RawConn) (gone, supported bool) {
	var revents int16
	looked := false
	err := rc.Control(func(fd uintptr) {
		var st unix.Stat_t
		if unix.Fstat(int(fd), &st) != nil {
			return
		}
		switch uint32(st.Mode) & unix.S_IFMT {
		case unix.S_IFIFO:
		case unix.S_IFSOCK:
			if !unixStreamSocket(int(fd)) {
				return
			}
		default:
			return
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		_, perr := unix.Poll(fds, 0)
		switch {
		case perr == nil:
			revents, looked = fds[0].Revents, true
		case errors.Is(perr, unix.EINTR):
			looked = true
		}
	})
	if err != nil || !looked || revents&unix.POLLNVAL != 0 {
		return false, false
	}
	return revents&(unix.POLLHUP|unix.POLLERR) != 0, true
}

// unixStreamSocket reports whether the socket fd is a Unix-domain stream
// socket, the one socket kind whose hang-up bits tell a peer gone from a peer
// half-closed. A socket whose family or type cannot be read is not.
func unixStreamSocket(fd int) bool {
	sa, err := unix.Getsockname(fd)
	if err != nil {
		return false
	}
	if _, ok := sa.(*unix.SockaddrUnix); !ok {
		return false
	}
	typ, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
	return err == nil && typ == unix.SOCK_STREAM
}
