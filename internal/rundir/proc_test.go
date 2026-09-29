package rundir

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestParseProcStat is a Linux stat line read (parseProcStat): the command
// ends at the last ")" whatever it holds, the parent is field 4 and the start
// time field 22; a line with too few fields, no command or a field that is
// not a number is an error, never a zero identity.
func TestParseProcStat(t *testing.T) {
	// Fields 3..22, the state first: the parent 4242, the start time 987654.
	tail := " S 4242 77 77 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1234 56"
	for _, tc := range []struct {
		name string
		line string
		want ProcIdentity
		err  bool
	}{
		{name: "plain", line: "77 (craze-fake-agen)" + tail, want: ProcIdentity{Start: 987654, PPID: 4242}},
		{name: "a command with spaces and parentheses", line: "77 (a) b (c) 1 2)" + tail, want: ProcIdentity{Start: 987654, PPID: 4242}},
		{name: "exactly twenty fields", line: "77 (x)" + strings.Join(strings.Fields(tail)[:20], " ") + "\n", want: ProcIdentity{Start: 987654, PPID: 4242}},
		{name: "too few fields", line: "77 (x) S 4242 77", err: true},
		{name: "no command", line: "77 x S 4242", err: true},
		{name: "a parent that is not a number", line: "77 (x) S p 77 77 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654", err: true},
		{name: "a start time that is not a number", line: "77 (x) S 4242 77 77 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 -5", err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProcStat(tc.line)
			if tc.err {
				if err == nil {
					t.Fatalf("parsed %+v, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parsed %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

// TestProcessIdentity reads real processes: this one — its parent's pid, a
// start time that reads the same twice — and a child it starts, whose parent
// is this process and which started no earlier; once the child is killed and
// reaped its pid names nobody (ErrNoProcess). A pid that cannot be one is an
// error of another kind.
func TestProcessIdentity(t *testing.T) {
	self, err := ProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	again, err := ProcessIdentity(os.Getpid())
	switch {
	case err != nil:
		t.Fatal(err)
	case self.PPID != os.Getppid() || self.Start == 0 || again != self:
		t.Fatalf("this process: %+v, then %+v; its parent is %d", self, again, os.Getppid())
	}

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	child, err := ProcessIdentity(cmd.Process.Pid)
	switch {
	case err != nil:
		t.Fatal(err)
	case child.PPID != os.Getpid() || child.Start < self.Start:
		t.Fatalf("the child: %+v; this process %d started at %d", child, os.Getpid(), self.Start)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	reaped = true
	if got, err := ProcessIdentity(cmd.Process.Pid); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("a reaped child: %+v, %v; want ErrNoProcess", got, err)
	}
	if _, err := ProcessIdentity(0); err == nil || errors.Is(err, ErrNoProcess) {
		t.Fatalf("pid 0: %v, want an error that is not ErrNoProcess", err)
	}
}
