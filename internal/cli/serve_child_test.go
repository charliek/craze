package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// The test binary as craze itself (plan 030 C2): a test that needs a host in
// a process of its own — real signals, a real exit code — re-executes this
// binary with cliChildEnv set to the command line as JSON, and init below runs
// it through the root command exactly as Execute does, before TestMain or any
// test. No craze binary is built for it, so the same test runs under a CPU
// quota without a link step inside the quota.

// cliChildEnv carries the child's argv (a JSON array, without "craze").
const cliChildEnv = "CRAZE_CLI_TEST_CHILD"

// The child's test hooks, each set in its environment by the test that wants
// it, and read by init before the command runs:
const (
	// cliChildPanic makes craze serve panic with this message once it serves
	// (serveServing), on its own goroutine: the crash-output test.
	cliChildPanic = "CRAZE_CLI_TEST_PANIC"
	// cliChildLogMax is the host log's rotation size in bytes (hostLogMax).
	cliChildLogMax = "CRAZE_CLI_TEST_LOG_MAX"
)

func init() {
	raw, ok := os.LookupEnv(cliChildEnv)
	if !ok {
		return
	}
	// Not the agent's to inherit: the fake agent is another binary, but a
	// grandchild of this one must never run as craze by accident.
	_ = os.Unsetenv(cliChildEnv)
	if msg, ok := os.LookupEnv(cliChildPanic); ok {
		_ = os.Unsetenv(cliChildPanic)
		serveServing = func() { panic(msg) }
	}
	if size, ok := os.LookupEnv(cliChildLogMax); ok {
		_ = os.Unsetenv(cliChildLogMax)
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil || n <= 0 {
			fmt.Fprintln(os.Stderr, "craze test child: bad", cliChildLogMax, size)
			os.Exit(97)
		}
		hostLogMax = n
	}
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil {
		fmt.Fprintln(os.Stderr, "craze test child:", err)
		os.Exit(97)
	}
	cmd := NewRootCmd()
	cmd.SetArgs(argv)
	ranCmd, err := cmd.ExecuteC()
	if err == nil {
		os.Exit(0)
	}
	line, code := diagnose(ranCmd, err)
	if line != "" {
		fmt.Fprintln(os.Stderr, line)
	}
	os.Exit(code)
}

// crazeChild is one craze run in a process of its own.
type crazeChild struct {
	cmd    *exec.Cmd
	output *lockedBuffer
	done   chan struct{}
	err    error // cmd.Wait's, once done is closed
}

// startCrazeChild starts `craze <argv...>` as a child of the test, in the
// test's environment (HOME, CRAZE_HOME and the rest as the test set them),
// stdout and stderr together in output. A child still running when the test
// ends is sent SIGTERM, then SIGKILL, so no host outlives its test.
func startCrazeChild(t *testing.T, argv ...string) *crazeChild {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	// -test.run matches nothing, so a child whose init somehow fell through
	// runs no test.
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), cliChildEnv+"="+string(b))
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &crazeChild{cmd: cmd, output: out, done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	t.Cleanup(func() {
		select {
		case <-c.done:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-c.done:
		case <-time.After(serveStep):
			_ = cmd.Process.Kill()
			<-c.done
		}
	})
	return c
}

// wait is the child's exit code once it has exited, within d.
func (c *crazeChild) wait(t *testing.T, d time.Duration) int {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(d):
		t.Fatalf("the child has not exited after %v; output: %s", d, c.output)
	}
	if c.err == nil {
		return 0
	}
	if ee, ok := c.err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	t.Fatalf("waiting for the child: %v", c.err)
	return -1
}

// running reports whether the child has not exited yet.
func (c *crazeChild) running() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}
