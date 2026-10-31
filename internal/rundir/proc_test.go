package rundir

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestParseProcStat is a Linux stat line read (parseProcStat): the command
// ends at the last ")" whatever it holds, the state is field 3 — Z or X a
// zombie (plan 041 C6 r1, 3) — the parent field 4 and the start time field
// 22; a line with too few fields, no command, a state that is not one letter
// or a field that is not a number is an error, never a zero identity.
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
		{name: "a zombie", line: "77 (x) Z" + tail[2:], want: ProcIdentity{Start: 987654, PPID: 4242, Zombie: true}},
		{name: "a zombie its reaper is taking", line: "77 (x) X" + tail[2:], want: ProcIdentity{Start: 987654, PPID: 4242, Zombie: true}},
		{name: "a state that is not one letter", line: "77 (x) SS" + tail[2:], err: true},
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

// TestParseProcMember is a stat line's group and session, its command's spaces
// and parentheses no trouble: the same reading as TestParseProcStat's.
func TestParseProcMember(t *testing.T) {
	// The fields after the command, 3 on: state, ppid, pgrp, session, then 15 more
	// up to the start time, field 22.
	tail := " S 4242 900 901 0 0 0 0 0 0 0 0 0 0 0 20 0 1 0 987654 0 0\n"
	for _, tc := range []struct {
		name, line string
		want       GroupMember
	}{
		{"plain", "77 (sleep)" + tail, GroupMember{PID: 77, PGID: 900, Session: 901, Start: 987654}},
		{"a command like a) b (c", "77 (a) b (c)" + tail, GroupMember{PID: 77, PGID: 900, Session: 901, Start: 987654}},
		{"a command that ends like a line", "77 (x) S 1 2 3 ) 4)" + tail, GroupMember{PID: 77, PGID: 900, Session: 901, Start: 987654}},
		{"a zombie", "77 (x) Z" + tail[2:], GroupMember{PID: 77, PGID: 900, Session: 901, Start: 987654, Zombie: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProcMember(77, tc.line)
			if err != nil || got != tc.want {
				t.Fatalf("%q: %+v, %v; want %+v", tc.line, got, err, tc.want)
			}
		})
	}
	for _, line := range []string{
		"", "77 no parenthesis", "77 (x)", "77 (x) S 1",
		"77 (x) S 4242 nine 901" + strings.Repeat(" 0", 16),
		"77 (x) S 4242 900 nine" + strings.Repeat(" 0", 16),
	} {
		if m, err := parseProcMember(77, line); err == nil {
			t.Fatalf("%q read as %+v", line, m)
		}
	}
}

// TestEnvironHolds takes a mark whole and nothing that merely has it in it.
func TestEnvironHolds(t *testing.T) {
	const mark = "CRAZE_AGENT=1"
	for env, want := range map[string]bool{
		"":                         false,
		"CRAZE_AGENT=1":            true,
		"A=b\x00CRAZE_AGENT=1\x00": true,
		"CRAZE_AGENT=1\x00A=b\x00": true,
		"XCRAZE_AGENT=1\x00":       false,
		"CRAZE_AGENT=10\x00":       false,
		"A=CRAZE_AGENT=1\x00":      false,
		"CRAZE_AGENT=\x001":        false,
		"\x00\x00":                 false,
	} {
		if got := environHolds([]byte(env), mark); got != want {
			t.Fatalf("%q: %v, want %v", env, got, want)
		}
	}
}

// TestProcArgsEnviron reads a kern.procargs2 buffer: argc, the executable's path,
// its padding, the arguments and then the environment.
func TestProcArgsEnviron(t *testing.T) {
	buf := func(argc byte, rest string) []byte { return append([]byte{argc, 0, 0, 0}, rest...) }
	for _, tc := range []struct {
		name string
		b    []byte
		want string
	}{
		// "/bin/sh" and its NUL are 8 bytes, so no padding; "/bin/zsh" and its NUL are 9,
		// so 7 NULs.
		{"whole", buf(2, "/bin/zsh\x00\x00\x00\x00\x00\x00\x00\x00sh\x00-c\x00A=b\x00CRAZE_AGENT=1\x00\x00\x00apple=x\x00"), "A=b\x00CRAZE_AGENT=1\x00"},
		{"an empty argument", buf(2, "/bin/sh\x00sh\x00\x00A=b\x00\x00"), "A=b\x00"},
		{"an empty first argument, the mark alone", buf(1, "/bin/sh\x00\x00CRAZE_AGENT=1\x00\x00"), "CRAZE_AGENT=1\x00"},
		{"an empty first argument after padding", buf(1, "/bin/zsh\x00\x00\x00\x00\x00\x00\x00\x00\x00CRAZE_AGENT=1\x00\x00"), "CRAZE_AGENT=1\x00"},
		{"padding wider than a pointer's", buf(1, "/bin/sh\x00\x00\x00\x00sh\x00A=b\x00\x00"), "sh\x00A=b\x00"},
		{"nothing after the arguments", buf(1, "/bin/sh\x00sh\x00"), ""},
		{"too short", []byte{1, 0}, ""},
		{"no path's end", buf(1, "/bin/sh"), ""},
		{"too few arguments", buf(3, "/bin/sh\x00sh\x00"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(procArgsEnviron(tc.b)); got != tc.want {
				t.Fatalf("%q: %q, want %q", tc.b, got, tc.want)
			}
		})
	}
}

// TestProcessGroupsFindsAGroupsMembers is the scan on this machine: a child leading
// a group of its own is found in it, with its session and its start time, and
// nothing in that group but it; a past deadline is a scan that says nothing.
func TestProcessGroupsFindsAGroupsMembers(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.Env = append(os.Environ(), "CRAZE_REAP_TEST_MARK=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	id, err := ProcessIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := ProcessGroups(time.Now().Add(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := groups[pid], []GroupMember{{PID: pid, PGID: pid, Session: pid, Start: id.Start}}; !slices.Equal(got, want) {
		t.Fatalf("the group: %+v, want %+v", got, want)
	}
	if m, err := ProcessMember(pid); err != nil || m != groups[pid][0] {
		t.Fatalf("one read of it: %+v, %v; the scan's: %+v", m, err, groups[pid][0])
	}
	if _, err := ProcessMember(1 << 30); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("a pid nothing has: %v", err)
	}
	if !ProcessCarries(pid, "CRAZE_REAP_TEST_MARK=1") {
		t.Fatal("the child does not carry what its environment holds")
	}
	if ProcessCarries(pid, "CRAZE_REAP_TEST_MARK=2") || ProcessCarries(1<<30, "CRAZE_REAP_TEST_MARK=1") {
		t.Fatal("something carries what it was not given")
	}
	if _, err := ProcessGroups(time.Now().Add(-time.Hour)); !errors.Is(err, ErrScanCut) && runtime.GOOS == "linux" {
		t.Fatalf("a deadline past: %v, want %v", err, ErrScanCut)
	}
	if boot, ns, err := ProcessScope(); err != nil || boot == "" || ns == "" {
		t.Fatalf("the scope: %q %q %v", boot, ns, err)
	}
}
