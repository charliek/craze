//go:build linux

package agentproc

import (
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPidfdRefusedFallsBackToTheCheckedKill(t *testing.T) {
	for _, tc := range []struct {
		name       string
		open, send error
	}{
		{"open EPERM", unix.EPERM, nil},
		{"open EACCES", unix.EACCES, nil},
		{"send EPERM", nil, unix.EPERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prevOpen, prevSend := pidfdOpen, pidfdSendSignal
			t.Cleanup(func() { pidfdOpen, pidfdSendSignal = prevOpen, prevSend })
			if tc.open != nil {
				pidfdOpen = func(int, int) (int, error) { return -1, tc.open }
			}
			if tc.send != nil {
				pidfdSendSignal = func(int, unix.Signal, *unix.Siginfo, int) error { return tc.send }
			}
			cmd := exec.Command("sleep", "60")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			e, ok := statEntry(cmd.Process.Pid)
			if !ok {
				t.Fatal("no stat entry for the child")
			}
			if !platformKillVerified(e) {
				t.Fatal("the refused pidfd left the process unkilled")
			}
			select {
			case <-waited:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("the process is still running")
			}
		})
	}
}
