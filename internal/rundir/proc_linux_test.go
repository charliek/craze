//go:build linux

package rundir

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestAStartTokenNeedsThisNamespacesProc is not parallel: it replaces
// procSelfStatus, which every Linux StartToken reads. A /proc mounted for an
// ancestor PID namespace (an unshare -p without a remount) names other
// processes by this process's pids. Its /proc/self/status's NStgid line then
// lists this process's pid in each namespace from the mount's down — two
// fields or more, even when the numbers are equal (a pid can be the same in
// both). Then, or when the line is missing or cannot be read, there is no
// scope: no token is made, and none is carried, not even this process's own.
// Its own namespace's /proc — one field, this pid — scopes a token.
func TestAStartTokenNeedsThisNamespacesProc(t *testing.T) {
	self, err := StartToken(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	was := procSelfStatus
	t.Cleanup(func() { procSelfStatus = was })
	pid := strconv.Itoa(os.Getpid())
	for name, status := range map[string]func() ([]byte, error){
		"equal pids in an ancestor and its own namespace": func() ([]byte, error) {
			return []byte("Name:\tx\nNStgid:\t" + pid + "\t" + pid + "\nNSpid:\t" + pid + "\t" + pid + "\n"), nil
		},
		"another pid in an ancestor": func() ([]byte, error) {
			return []byte("NStgid:\t4242\t" + pid + "\n"), nil
		},
		"another number": func() ([]byte, error) {
			return []byte("NStgid:\t" + strconv.Itoa(os.Getpid()+1) + "\n"), nil
		},
		"no NStgid line": func() ([]byte, error) { return []byte("Name:\tx\nPid:\t" + pid + "\n"), nil },
		"unreadable":     func() ([]byte, error) { return nil, errors.New("permission denied") },
	} {
		procSelfStatus = status
		if tok, err := StartToken(os.Getpid()); err == nil {
			t.Errorf("%s: StartToken = %q; want no token", name, tok)
		}
		if CarriesStartToken(os.Getpid(), self) {
			t.Errorf("%s: this process carries its token", name)
		}
	}
	procSelfStatus = func() ([]byte, error) { return []byte("Name:\tx\nNStgid:\t" + pid + "\nNSpid:\t" + pid + "\n"), nil }
	if tok, err := StartToken(os.Getpid()); err != nil || tok != self {
		t.Fatalf("its own namespace's /proc: StartToken = %q, %v; want %q", tok, err, self)
	}
}

// TestAnEnvironmentThatCannotBeReadIsNotKnown: a read that fails of a process still
// there says nothing about it; of one that has gone, or of another user's, it does.
func TestAnEnvironmentThatCannotBeReadIsNotKnown(t *testing.T) {
	const mark = "CRAZE_REAP_TEST_MARK=1"
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "environ"), 0o700); err != nil { // a read of it fails
		t.Fatal(err)
	}
	if carries, known := carriesIn(dir, os.Getpid(), mark); carries || known {
		t.Fatalf("a failing read of a process still there: carries %v, known %v", carries, known)
	}
	if carries, known := carriesIn(dir, 1<<30, mark); carries || !known {
		t.Fatalf("a failing read of a process that has gone: carries %v, known %v", carries, known)
	}
	readable := t.TempDir()
	if err := os.WriteFile(filepath.Join(readable, "environ"), []byte("A=b\x00"+mark+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if carries, known := carriesIn(readable, os.Getpid(), mark); !carries || !known {
		t.Fatalf("a read that holds the mark: carries %v, known %v", carries, known)
	}
	if os.Geteuid() != 0 {
		if carries, known := carriesIn("/proc/1", 1, mark); carries || !known {
			t.Fatalf("another user's process: carries %v, known %v", carries, known)
		}
	}
}
