//go:build linux || darwin

package rundir

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// PeerGone (plan 035 C9, SF-123) on each kind of descriptor craze bridge's
// stdout or a splice's client leg can be: a pipe, a socket shared by stdin and
// stdout as some sshd builds hand one over, and the kinds it cannot tell
// about. Each is an *os.File, as the bridge's stdout is.

// peerGone is PeerGone on f, through f's SyscallConn as the bridge reaches its
// stdout.
func peerGone(t *testing.T, f *os.File) (gone, supported bool) {
	t.Helper()
	rc, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	return PeerGone(rc)
}

// wantPeerGone checks PeerGone's answer on f.
func wantPeerGone(t *testing.T, what string, f *os.File, gone, supported bool) {
	t.Helper()
	if g, s := peerGone(t, f); g != gone || s != supported {
		t.Fatalf("%s: PeerGone answered (gone %v, supported %v), want (%v, %v)", what, g, s, gone, supported)
	}
}

// TestPeerGoneOnAPipe: a pipe this process writes, its reader there, is not
// gone; once its only reader has closed it is (POLLERR on Linux, POLLHUP on
// macOS). The first look is the negative control: the same pipe, the same
// call, with the reader still open.
func TestPeerGoneOnAPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	wantPeerGone(t, "a pipe whose reader is open", w, false, true)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	wantPeerGone(t, "a pipe whose reader has closed", w, true, true)
}

// socketPair is both ends of one Unix stream socketpair: ours as an *os.File
// (the bridge's stdout, or a splice's client leg), the peer as a bare
// descriptor the test shuts down, and closes with closePeer (once: a second
// close of the number could close a descriptor reused since).
func socketPair(t *testing.T) (ours *os.File, peer int, closePeer func() error) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	ours = os.NewFile(uintptr(fds[0]), "ours")
	peer = fds[1]
	var once sync.Once
	var cerr error
	closePeer = func() error {
		once.Do(func() { cerr = unix.Close(peer) })
		return cerr
	}
	t.Cleanup(func() {
		_ = ours.Close()
		_ = closePeer()
	})
	return ours, peer, closePeer
}

// TestPeerGoneOnASocketItsPeerHalfClosed: a socket whose peer has only shut
// its writing half (SHUT_WR) is not gone (S2's half-close: a peer that only
// reads to the end is still delivered), even shared as stdin and stdout and
// with stdin already at its end; the peer still reads what this side writes.
// Once the peer closes it is gone. A probe that took any revent, POLLOUT
// included, for a peer gone would fail the half-closed look; one that
// ignored POLLHUP would fail the closed one.
func TestPeerGoneOnASocketItsPeerHalfClosed(t *testing.T) {
	ours, peer, closePeer := socketPair(t)
	wantPeerGone(t, "a connected socket", ours, false, true)
	if err := unix.Shutdown(peer, unix.SHUT_WR); err != nil {
		t.Fatal(err)
	}
	// The half-close has landed: this side reads its end, as the bridge's
	// stdin does.
	if n, err := ours.Read(make([]byte, 8)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("after the peer's SHUT_WR this side read %d bytes (%v), want its end", n, err)
	}
	wantPeerGone(t, "a socket whose peer did SHUT_WR", ours, false, true)
	if _, err := ours.Write([]byte("still read")); err != nil {
		t.Fatalf("a write to a half-closed peer: %v", err)
	}
	buf := make([]byte, 32)
	if n, err := unix.Read(peer, buf); err != nil || string(buf[:n]) != "still read" {
		t.Fatalf("the half-closed peer read %q (%v), want %q", buf[:n], err, "still read")
	}
	if err := closePeer(); err != nil {
		t.Fatal(err)
	}
	wantPeerGone(t, "a socket whose peer has closed", ours, true, true)
}

// TestPeerGoneCannotTellAFileOrADevice: a regular file and /dev/null are
// neither a pipe nor a socket, so the probe turns itself off (supported
// false), where poll would only have reported them writable for ever; and a
// closed descriptor, which Control cannot reach, is not supported either.
func TestPeerGoneCannotTellAFileOrADevice(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	wantPeerGone(t, "a regular file", f, false, false)

	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = null.Close() })
	wantPeerGone(t, os.DevNull, null, false, false)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	rc, err := w.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if g, s := PeerGone(rc); g || s {
		t.Fatalf("a closed descriptor: PeerGone answered (gone %v, supported %v), want (false, false)", g, s)
	}
}
