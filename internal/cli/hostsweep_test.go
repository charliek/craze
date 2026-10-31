package cli

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestASignalDuringTheJoinKeepsTheExitCode: a command has written its result and is
// waiting for its sweep, whose grace has a good way to run. A SIGTERM ends the wait at
// once, and the process exits with the code the command determined, not by the signal.
// Run in a process of its own, as Execute's exit would be.
func TestASignalDuringTheJoinKeepsTheExitCode(t *testing.T) {
	if os.Getenv("CRAZE_JOIN_SIGNAL") != "" {
		done := make(chan struct{}) // a sweep that never ends
		hostSweepDone.Store(&done)
		ready := make(chan bool, 1)
		go func() { ready <- joinHostSweep() }()
		// The handler is registered once the join has seen the sweep still running.
		time.Sleep(100 * time.Millisecond)
		os.Stdout.WriteString("joining\n")
		if <-ready {
			os.Exit(3)
		}
		os.Exit(9) // the bound, not the signal
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestASignalDuringTheJoinKeepsTheExitCode$")
	cmd.Env = append(os.Environ(), "CRAZE_JOIN_SIGNAL=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "joining" {
		t.Fatalf("the join's first line: %q, %v", line, err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if code := cmd.ProcessState.ExitCode(); code != 3 {
		t.Fatalf("exit %d (%v), want the command's 3", code, cmd.ProcessState)
	}
}

// TestAJoinOfAFinishedSweepRegistersNothing: nothing to wait for is nothing to catch, so
// a healthy command's exit is as it was.
func TestAJoinOfAFinishedSweepRegistersNothing(t *testing.T) {
	done := make(chan struct{})
	close(done)
	hostSweepDone.Store(&done)
	if joinHostSweep() {
		t.Fatal("a signal was reported with none sent")
	}
	if joinHostSweep() {
		t.Fatal("a second join found something")
	}
}
