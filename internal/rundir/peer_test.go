package rundir

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// connectedPair is both ends of one real Unix stream connection, made the way
// a host and its client make one: a listener bound in a short directory, a
// dial, and the accept.
func connectedPair(t *testing.T) (server, client *net.UnixConn) {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(shortDir(t), "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	nc, err := net.Dial("unix", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client = nc.(*net.UnixConn)
	t.Cleanup(func() { _ = client.Close() })
	ac, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	server = ac.(*net.UnixConn)
	t.Cleanup(func() { _ = server.Close() })
	return server, client
}

// unconnected is a Unix stream socket bound in a short directory and never
// connected or listening: one with no peer.
func unconnected(t *testing.T) *net.UnixConn {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "unconnected")
	defer f.Close()
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: filepath.Join(shortDir(t), "u")}); err != nil {
		t.Fatal(err)
	}
	fc, err := net.FileConn(f) // its own descriptor, a dup
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fc.Close() })
	uc, ok := fc.(*net.UnixConn)
	if !ok {
		t.Fatalf("a Unix socket is a %T", fc)
	}
	return uc
}

// TestPeerCredNamesThisProcess: over a real connection, each end's lookup
// names this process — its pid and effective uid — on both OSes craze ships
// on (the pid is SO_PEERCRED's on Linux, LOCAL_PEERPID's on macOS), and
// leaves the descriptor as it found it.
func TestPeerCredNamesThisProcess(t *testing.T) {
	server, client := connectedPair(t)
	for name, uc := range map[string]*net.UnixConn{"the accepted end": server, "the dialed end": client} {
		pid, uid, err := PeerCred(uc)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if uid != os.Geteuid() || pid != os.Getpid() {
			t.Errorf("%s: peer (pid %d, uid %d), want this process's (%d, %d)", name, pid, uid, os.Getpid(), os.Geteuid())
		}
	}
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1)
	if _, err := server.Read(b); err != nil || b[0] != 'x' {
		t.Fatalf("a read after the lookup: %q, %v", b, err)
	}
}

// TestAPeerCredThatCannotBeReadFails: a lookup that cannot name the peer is an
// error with pid 0 and a negative uid, never a uid: a closed connection, no
// connection, and a socket with no peer (Linux reports that one's uid as
// (uid_t)-1, which is not a uid).
func TestAPeerCredThatCannotBeReadFails(t *testing.T) {
	_, closed := connectedPair(t)
	_ = closed.Close()
	for name, uc := range map[string]*net.UnixConn{
		"a closed connection":   closed,
		"no connection":         nil,
		"a socket with no peer": unconnected(t),
	} {
		pid, uid, err := PeerCred(uc)
		if err == nil || uid >= 0 || pid != 0 {
			t.Errorf("%s: (%d, %d, %v), want an error, pid 0 and a negative uid", name, pid, uid, err)
		}
	}
}

// lookupOf is an injected credential lookup that answers pid, uid and err.
func lookupOf(pid, uid int, err error) func(*net.UnixConn) (int, int, error) {
	return func(*net.UnixConn) (int, int, error) { return pid, uid, err }
}

// TestPeerCheckWantsTheUID: the check passes exactly the wanted uid — the pid
// never counts, whatever it is — and refuses every other one with ErrPeerUID,
// whose text names both uids and nothing else about the peer; a failed lookup
// refuses with the lookup's own error and a negative uid; a lookup naming no
// uid, or a check wanting none, refuses.
func TestPeerCheckWantsTheUID(t *testing.T) {
	lookupFailed := errors.New("injected: no credentials")
	const mismatch = "rundir: the peer runs as another user: peer uid 1001, want 1000"
	for _, tc := range []struct {
		name     string
		want     int
		lookup   func(*net.UnixConn) (int, int, error)
		pid, uid int
		err      error  // nil: passes, unless msg is set
		msg      string // the refusal's whole text, when it is pinned
	}{
		{name: "the wanted uid", want: 1000, lookup: lookupOf(4242, 1000, nil), pid: 4242, uid: 1000},
		{name: "the wanted uid, the pid unknown", want: 1000, lookup: lookupOf(0, 1000, nil), uid: 1000},
		{name: "root, wanted", want: 0, lookup: lookupOf(1, 0, nil), pid: 1, uid: 0},
		{name: "another uid", want: 1000, lookup: lookupOf(4242, 1001, nil), pid: 4242, uid: 1001,
			err: ErrPeerUID, msg: mismatch},
		{name: "another uid, the pid the wanted uid", want: 1000, lookup: lookupOf(1000, 1001, nil), pid: 1000, uid: 1001,
			err: ErrPeerUID, msg: mismatch},
		{name: "root, not wanted", want: 1000, lookup: lookupOf(1000, 0, nil), pid: 1000, uid: 0,
			err: ErrPeerUID, msg: "rundir: the peer runs as another user: peer uid 0, want 1000"},
		{name: "a failed lookup", want: 1000, lookup: lookupOf(4242, 1000, lookupFailed), uid: -1,
			err: lookupFailed, msg: lookupFailed.Error()},
		{name: "a lookup naming no uid", want: 1000, lookup: lookupOf(4242, -1, nil), uid: -1,
			msg: "rundir: peer credentials: the lookup named no uid"},
		{name: "a check wanting no uid", want: -1, lookup: lookupOf(4242, 1000, nil), pid: 4242, uid: 1000,
			err: ErrPeerUID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pid, uid, err := PeerCheckWith(tc.want, tc.lookup)(nil)
			if pid != tc.pid || uid != tc.uid {
				t.Errorf("named (pid %d, uid %d), want (%d, %d)", pid, uid, tc.pid, tc.uid)
			}
			refused := tc.err != nil || tc.msg != ""
			switch {
			case !refused && err != nil:
				t.Fatalf("refused: %v", err)
			case refused && err == nil:
				t.Fatal("passed")
			case tc.err != nil && !errors.Is(err, tc.err):
				t.Errorf("err %v, want %v", err, tc.err)
			case tc.msg != "" && err.Error() != tc.msg:
				t.Errorf("err %q, want %q", err, tc.msg)
			}
		})
	}
}

// TestPeerCheckOverARealConnection: PeerCheck's lookup is PeerCred — this
// process's own uid passes and is named with its pid, and any other wanted
// uid refuses the same connection, still naming the peer.
func TestPeerCheckOverARealConnection(t *testing.T) {
	server, _ := connectedPair(t)
	pid, uid, err := PeerCheck(os.Geteuid())(server)
	if err != nil || pid != os.Getpid() || uid != os.Geteuid() {
		t.Fatalf("PeerCheck(euid): (%d, %d, %v), want (%d, %d, nil)", pid, uid, err, os.Getpid(), os.Geteuid())
	}
	pid, uid, err = PeerCheckWith(os.Geteuid()+1, nil)(server)
	if !errors.Is(err, ErrPeerUID) || pid != os.Getpid() || uid != os.Geteuid() {
		t.Fatalf("PeerCheck(euid+1): (%d, %d, %v), want this process named and ErrPeerUID", pid, uid, err)
	}
	if _, uid, err := PeerCheck(os.Geteuid())(unconnected(t)); err == nil || uid >= 0 {
		t.Fatalf("PeerCheck of a socket with no peer: (uid %d, %v), want a refusal naming no uid", uid, err)
	}
}

// TestDialCheckIsPeerChecksRule: the client's check is the server's rule —
// the wanted uid over a real connection passes; another wanted uid, an
// injected other uid and a failed lookup refuse.
func TestDialCheckIsPeerChecksRule(t *testing.T) {
	_, client := connectedPair(t)
	if err := DialCheck(os.Geteuid())(client); err != nil {
		t.Fatalf("DialCheck(euid) over a real connection: %v", err)
	}
	if err := DialCheck(os.Geteuid() + 1)(client); !errors.Is(err, ErrPeerUID) {
		t.Fatalf("DialCheck(euid+1) over a real connection: %v, want ErrPeerUID", err)
	}
	if err := DialCheckWith(1000, lookupOf(4242, 1001, nil))(client); !errors.Is(err, ErrPeerUID) ||
		err.Error() != "rundir: the peer runs as another user: peer uid 1001, want 1000" {
		t.Fatalf("DialCheckWith an injected other uid: %v", err)
	}
	lookupFailed := errors.New("injected: no credentials")
	if err := DialCheckWith(1000, lookupOf(4242, 1000, lookupFailed))(client); !errors.Is(err, lookupFailed) {
		t.Fatalf("DialCheckWith a failed lookup: %v, want the lookup's error", err)
	}
}
