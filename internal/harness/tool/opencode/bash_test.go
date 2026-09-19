package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// The bash tool's tests run real commands. Every command that could outlive
// a failing test is bounded twice: startBash's cleanup closes the call, which
// kills its process group, and a test that learns a background pid kills
// that process at cleanup if it is still the one it started (bashPID). The
// commands' sleeps use distinct durations (601, 602, ...) as markers, so no
// test mistakes another's process, or a stranger that reused a pid, for its
// own.
//
// Ported from opencode's test/tool/shell.test.ts (5f9d9187):
//
//	basic                                      TestBashOutput
//	captures stderr in output                  TestBashOutput
//	returns non-zero exit code                 TestBashOutput
//	terminates command on timeout              TestBashTimeout
//	uses RuntimeFlags bashDefaultTimeoutMs ... TestBashTimeout (craze's default is fixed: 120000 ms, in the description and in Prepare)
//	preserves output when aborted              TestBashAbortKeepsOutput
//	streams metadata updates progressively     TestBashStreamsProgress (0.8 s apart, not 0.1 s: progress is throttled to 100 ms)
//	truncates output exceeding line limit      TestBashTruncation
//	truncates output exceeding byte limit      TestBashTruncation
//	does not truncate small output             TestBashTruncation
//	full output is saved to file when truncated TestBashTruncation
//	falls back from terminal-only configured shell  TestBashShell (craze has no configured shell: bash, else sh)
//
// Skipped: every case under "tool.shell permissions" — the bash and
// external_directory permission patterns opencode derives with tree-sitter —
// because H2 asks for no permission and parses no command (plan 019
// decision 2, §3.2); and every PowerShell, cmd.exe and Windows case, since
// craze runs on Linux and macOS only.

// The plan's lifetime numbers (plan 019 §3.9). The timing tests bound Run
// with these, not with the package's constants, which are what is under
// test.
const (
	planGrace = 3 * time.Second // SIGTERM to SIGKILL
	planDrain = 2 * time.Second // the leader's exit to SIGKILL
)

// bashEnv is a session's Env for calling the bash tool directly, not through
// the dispatcher: the dispatcher redacts and throttles again on its own, and
// would hide a fault in the tool's own redaction and throttle. Its child
// environment is this process's, as the harness's would be with no provider
// keys configured.
func bashEnv(t *testing.T, red *redact.Replacer) tool.Env {
	t.Helper()
	return tool.Env{Workspace: t.TempDir(), Home: t.TempDir(), Redactor: red, Environ: tool.ChildEnviron(os.Environ(), nil)}
}

var bashIDs atomic.Int64

// prepareBash prepares a bash call with in as its arguments: a value to
// marshal, or a string of raw JSON.
func prepareBash(t *testing.T, env tool.Env, in any) *bashCall {
	t.Helper()
	tl, err := newBash()
	if err != nil {
		t.Fatal(err)
	}
	b := tl.(*bashTool)
	// Tests keep out of the machine's temporary directory.
	b.host.tmp = filepath.Join(env.Home, "tmp")
	raw, isRaw := in.(string)
	if !isRaw {
		j, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(j)
	}
	p, err := b.Prepare(env, tool.Call{ID: fmt.Sprintf("t1.1.%d", bashIDs.Add(1)), Tool: "bash", Input: json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return p.(*bashCall)
}

// bashRun is a call running on a goroutine of its own.
type bashRun struct {
	cancel context.CancelCauseFunc
	res    chan tool.Result
}

// startBash runs c. When the test ends it closes the call, as a session
// would, and waits for it: a test that failed early leaves nothing running.
func startBash(t *testing.T, c *bashCall, env tool.Env) *bashRun {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	r := &bashRun{cancel: cancel, res: make(chan tool.Result, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.res <- c.Run(ctx, env)
	}()
	t.Cleanup(func() {
		cancel(tool.ErrClosing)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after the call was closed")
		}
	})
	return r
}

// await returns the call's result, failing the test if it takes longer than
// limit.
func (r *bashRun) await(t *testing.T, limit time.Duration) tool.Result {
	t.Helper()
	select {
	case res := <-r.res:
		return res
	case <-time.After(limit):
		t.Fatalf("Run did not return within %v", limit)
		return tool.Result{}
	}
}

// runBash makes one call and returns its result.
func runBash(t *testing.T, env tool.Env, in any) tool.Result {
	t.Helper()
	return startBash(t, prepareBash(t, env, in), env).await(t, 30*time.Second)
}

// meta is the <shell_metadata> block a result ends with.
func meta(lines ...string) string {
	return "\n\n<shell_metadata>\n" + strings.Join(lines, "\n") + "\n</shell_metadata>"
}

// proc is pid as ps shows it: its state and command line; ok is false when
// there is no such process.
func proc(pid int) (state, args string, ok bool) {
	out, err := exec.Command("ps", "-o", "stat=", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	fields := strings.Fields(string(out))
	if err != nil || len(fields) == 0 {
		return "", "", false
	}
	return fields[0], strings.Join(fields[1:], " "), true
}

// alive reports whether pid is a running process — not a zombie waiting to
// be reaped by its new parent — whose command line holds marker, so a pid
// that some other process has reused reads as gone.
func alive(pid int, marker string) bool {
	state, args, ok := proc(pid)
	return ok && !strings.HasPrefix(state, "Z") && strings.Contains(args, marker)
}

// waitFor polls cond until it holds or limit passes, and reports whether it
// held.
func waitFor(limit time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// bashPID waits for a command to write a pid, `echo $! > file`, and returns
// it. At cleanup that process is killed if it is still running as marker.
func bashPID(t *testing.T, file, marker string) int {
	t.Helper()
	var pid int
	if !waitFor(15*time.Second, func() bool {
		b, err := os.ReadFile(file)
		if err != nil || !bytes.HasSuffix(b, []byte("\n")) {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil
	}) {
		t.Fatalf("the command never wrote a pid to %s", file)
	}
	t.Cleanup(func() {
		if alive(pid, marker) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return pid
}

// gone fails the test unless pid, which ran as marker, dies soon: a killed
// process may take a moment to die and longer to be reaped, and alive reads
// a zombie as dead.
func gone(t *testing.T, pid int, marker string) {
	t.Helper()
	if !waitFor(5*time.Second, func() bool { return !alive(pid, marker) }) {
		state, args, _ := proc(pid)
		t.Fatalf("pid %d (%s) is still running: %s %s", pid, marker, state, args)
	}
}

// TestBashOutput ports opencode's basic, stderr and exit-code cases, and
// pins the result text exactly: the output, "(no output)" for none, and
// "exit code: N" in <shell_metadata> for a non-zero exit, which is not an
// error. Each exit-0 case is the control for the metadata.
func TestBashOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, command, out string
		code               int
	}{
		{"basic", "echo test", "test\n", 0},
		{"stderr is merged", "echo stdout_msg && echo stderr_msg >&2", "stdout_msg\nstderr_msg\n", 0},
		{"no output", "true", "", 0},
		{"non-zero exit", "exit 42", "", 42},
		{"non-zero exit with output", "echo out; exit 3", "out\n", 3},
		{"killed by a signal", "echo x; kill -KILL $$", "x\n", 137},
		{"output closed before the exit", "exec >/dev/null 2>&1; sleep 0.2; exit 5", "", 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := tc.out
			if want == "" {
				want = "(no output)"
			}
			if tc.code != 0 {
				want += meta(fmt.Sprintf("exit code: %d", tc.code))
			}
			res := runBash(t, bashEnv(t, nil), map[string]any{"command": tc.command})
			if res.IsError || res.Class != "" || res.Text != want {
				t.Fatalf("result = {IsError:%v Class:%q Text:%q}, want {Text:%q}", res.IsError, res.Class, res.Text, want)
			}
			if res.Output == nil || res.Output.ExitCode != tc.code || res.Output.Output != tc.out {
				t.Fatalf("Output = %+v, want exit code %d and output %q", res.Output, tc.code, tc.out)
			}
		})
	}
}

// TestBashRequest: the card's title and the gate's command are the command,
// and the workdir is resolved against the workspace.
func TestBashRequest(t *testing.T) {
	t.Parallel()
	env := bashEnv(t, nil)
	c := prepareBash(t, env, map[string]any{"command": "make test", "workdir": "sub"})
	want := tool.Request{Title: "make test", Command: "make test", Workdir: filepath.Join(env.Workspace, "sub")}
	if got := c.Request(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Request = %+v, want %+v", got, want)
	}
}

// TestBashWorkdir: the command runs in the workspace, or in workdir resolved
// against it; a workdir that does not exist or is not a directory is refused
// before anything runs.
func TestBashWorkdir(t *testing.T) {
	t.Parallel()
	env := bashEnv(t, nil)
	sub := filepath.Join(env.Workspace, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(env.Workspace, "file"), "x")
	outside := t.TempDir()

	for _, tc := range []struct {
		name    string
		workdir any // nil: absent
		pwd     string
		class   tool.ErrorClass
		text    string
	}{
		{name: "absent", pwd: env.Workspace},
		{name: "empty", workdir: "", pwd: env.Workspace},
		{name: "relative", workdir: "sub", pwd: sub},
		{name: "absolute", workdir: sub, pwd: sub},
		{name: "outside the workspace", workdir: outside, pwd: outside},
		{name: "missing", workdir: "missing", class: tool.ClassNotFound, text: "workdir does not exist: " + filepath.Join(env.Workspace, "missing")},
		{name: "under a file", workdir: "file/x", class: tool.ClassNotFound, text: "workdir does not exist: " + filepath.Join(env.Workspace, "file/x")},
		{name: "a file", workdir: "file", class: tool.ClassToolError, text: "workdir is not a directory: " + filepath.Join(env.Workspace, "file")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := map[string]any{"command": "pwd; touch ran"}
			if tc.workdir != nil {
				in["workdir"] = tc.workdir
			}
			res := runBash(t, env, in)
			if tc.class != "" {
				failed(t, res, tc.class, tc.text)
				return
			}
			if res.IsError || res.Text != tc.pwd+"\n" {
				t.Fatalf("result = %+v, want the command run in %s", res, tc.pwd)
			}
			if _, err := os.Stat(filepath.Join(tc.pwd, "ran")); err != nil {
				t.Fatalf("the command did not run in %s: %v", tc.pwd, err)
			}
		})
	}
}

// TestBashShell ports "falls back from terminal-only configured shell" as
// craze's rule: bash when it is there, else sh; and the description names
// the shell and the machine.
func TestBashShell(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	notExec := filepath.Join(dir, "bash")
	put(t, notExec, "")
	for _, tc := range []struct{ name, bash, want string }{
		{"bash is there", bashPath, bashPath},
		{"no bash", filepath.Join(dir, "missing"), shPath},
		{"bash is not executable", notExec, shPath},
		{"bash is a directory", dir, shPath},
	} {
		if got := pickShell(tc.bash, shPath); got != tc.want {
			t.Errorf("%s: pickShell = %q, want %q", tc.name, got, tc.want)
		}
	}
	if _, err := os.Stat(bashPath); err != nil {
		t.Skipf("no %s on this machine: %v", bashPath, err)
	}
	h, err := thisHost()
	if err != nil {
		t.Fatal(err)
	}
	// The temporary directory is the fixed one, or — where something else
	// holds that name on this machine — a session's own under the same base
	// (TestBashTmpDir); either way one that passes the check. Each part is
	// reported on its own: this runs against whatever the machine's temp
	// directory already holds, so a failure has to say which part was wrong
	// and what it saw.
	if h.shell != bashPath {
		t.Errorf("shell = %q, want %q", h.shell, bashPath)
	}
	// Cleaned on both sides: on macOS os.TempDir() is $TMPDIR as the
	// environment spells it, trailing slash and all, while filepath.Dir of
	// the joined path has none.
	if dir, want := filepath.Dir(h.tmp), filepath.Clean(os.TempDir()); dir != want {
		t.Errorf("the temporary directory %q is under %q, not the machine's %q", h.tmp, dir, want)
	}
	if base := filepath.Base(h.tmp); !strings.HasPrefix(base, "craze") {
		t.Errorf("the temporary directory is named %q, which is neither craze nor a craze- fallback", base)
	}
	if err := ensureTmp(h.tmp); err != nil {
		info, statErr := os.Lstat(h.tmp)
		t.Errorf("the temporary directory thisHost chose does not pass the check: %v (Lstat: %v, %v)", err, info, statErr)
	}
	if t.Failed() {
		t.FailNow() // the rest of this test builds on the host it chose
	}
	tl, err := newBash()
	if err != nil {
		t.Fatal(err)
	}
	if d := tl.Spec().Description; !strings.Contains(d, "Be aware: OS: "+h.os+", Shell: bash\n") || !strings.Contains(d, "Use `"+h.tmp+"`") {
		t.Fatalf("the description does not name this machine:\n%s", d)
	}
	// The directory the description promises exists once a command runs
	// (prepareBash moves it under the test's home).
	env := bashEnv(t, nil)
	if res := runBash(t, env, map[string]any{"command": "test -d '" + filepath.Join(env.Home, "tmp") + "'"}); res.IsError || res.Text != "(no output)" {
		t.Fatalf("the temporary directory was not made: %+v", res)
	}
}

// TestBashTimeout ports "terminates command on timeout" and the default
// timeout case, and pins the 10-minute cap: a longer timeout is reduced, and
// the result says so; the cap itself and the default are the controls.
func TestBashTimeout(t *testing.T) {
	t.Parallel()
	env := bashEnv(t, nil)
	c := prepareBash(t, env, map[string]any{"command": "echo started; sleep 607", "timeout": 500})
	began := time.Now()
	res := startBash(t, c, env).await(t, 30*time.Second)
	took := time.Since(began)
	want := "started\n" + meta("shell tool terminated command after exceeding timeout 500 ms. If this command is expected to take longer and is not waiting for interactive input, retry with a larger timeout value in milliseconds.")
	failed(t, res, tool.ClassTimeout, want)
	if res.Output == nil || res.Output.ExitCode != -1 || res.Output.Output != "started\n" {
		t.Fatalf("Output = %+v, want no exit code and the output", res.Output)
	}
	// sleep dies on SIGTERM: no grace is used up.
	if took < 500*time.Millisecond || took > 500*time.Millisecond+planGrace {
		t.Fatalf("a 500 ms timeout returned after %v", took)
	}

	for _, tc := range []struct {
		name      string
		in        map[string]any
		timeout   time.Duration
		requested int64
	}{
		{"default", map[string]any{"command": "echo hi"}, 2 * time.Minute, 0},
		{"at the cap", map[string]any{"command": "echo hi", "timeout": 600000}, 10 * time.Minute, 0},
		{"over the cap", map[string]any{"command": "echo hi", "timeout": 900000}, 10 * time.Minute, 900000},
		{"far over the cap", map[string]any{"command": "echo hi", "timeout": maxSafeInteger}, 10 * time.Minute, maxSafeInteger},
	} {
		call := prepareBash(t, env, tc.in)
		if call.timeout != tc.timeout || call.requested != tc.requested {
			t.Errorf("%s: timeout %v, requested %d; want %v, %d", tc.name, call.timeout, call.requested, tc.timeout, tc.requested)
		}
		want := "hi\n"
		if tc.requested != 0 {
			want += meta(fmt.Sprintf("The requested timeout of %d ms is above the maximum of 600000 ms; the command ran with a timeout of 600000 ms.", tc.requested))
		}
		if res := startBash(t, call, env).await(t, 30*time.Second); res.IsError || res.Text != want {
			t.Errorf("%s: result = %+v, want text %q", tc.name, res, want)
		}
	}
	tl, _ := newBash()
	if !strings.Contains(tl.Spec().Description, "commands will time out after 120000ms.") {
		t.Fatal("the description does not give the default timeout")
	}
}

// TestBashAbortKeepsOutput ports "preserves output when aborted": a cancel
// once the output shows "before" ends the command, and the result, class
// aborted, starts with opencode's "Tool execution aborted" and keeps the
// output with opencode's metadata line.
func TestBashAbortKeepsOutput(t *testing.T) {
	t.Parallel()
	env := bashEnv(t, nil)
	var r *bashRun
	var seen atomic.Int64
	ready := make(chan struct{})
	env.Progress = func(s string) {
		<-ready
		if strings.Contains(s, "before") && seen.Add(1) == 1 {
			r.cancel(nil)
		}
	}
	r = startBash(t, prepareBash(t, env, map[string]any{"command": "echo before && sleep 608"}), env)
	close(ready)
	res := r.await(t, 30*time.Second)
	failed(t, res, tool.ClassAborted, tool.AbortedText+"\n\nbefore\n"+meta("User aborted the command"))
	if seen.Load() == 0 {
		t.Fatal("no progress snapshot showed the output")
	}
}

// TestBashAbortBeforeItStarts: a call whose ctx is already done starts
// nothing.
func TestBashAbortBeforeItStarts(t *testing.T) {
	t.Parallel()
	env := bashEnv(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := prepareBash(t, env, map[string]any{"command": "touch ran"}).Run(ctx, env)
	failed(t, res, tool.ClassAborted, tool.AbortedText)
	if _, err := os.Stat(filepath.Join(env.Workspace, "ran")); err == nil {
		t.Fatal("the command ran")
	}
}

// TestBashCancelKillsTheCommand: `sleep & wait` dies on a cancel, the
// backgrounded sleep with it, and the result is aborted. The control is the
// sleep running before the cancel.
func TestBashCancelKillsTheCommand(t *testing.T) {
	t.Parallel()
	env := bashEnv(t, nil)
	r := startBash(t, prepareBash(t, env, map[string]any{"command": "echo before; sleep 601 & echo $! > pid; wait"}), env)
	pid := bashPID(t, filepath.Join(env.Workspace, "pid"), "sleep 601")
	if !alive(pid, "sleep 601") {
		t.Fatal("control: the backgrounded sleep is not running before the cancel")
	}
	cancelled := time.Now()
	r.cancel(nil)
	res := r.await(t, 30*time.Second)
	if took := time.Since(cancelled); took > planGrace {
		t.Fatalf("Run took %v after a cancel of a command that dies on SIGTERM", took)
	}
	failed(t, res, tool.ClassAborted, tool.AbortedText+"\n\nbefore\n"+meta("User aborted the command"))
	gone(t, pid, "sleep 601")
}

// outlives is the control for the normal-exit tests: run by a plain shell in
// a session of its own, the command — which ends in `&` — leaves its
// background sleep running after the shell has exited. It kills the sleep
// itself.
func outlives(t *testing.T, command, marker string) {
	t.Helper()
	// The shell's output goes to a file, not a pipe exec would wait on: the
	// sleep may hold it.
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	cmd := exec.Command(shPath, "-c", command+" echo $!")
	cmd.Stdout = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(load(t, f.Name())))
	if err != nil {
		t.Fatalf("control: %q printed no pid: %v", command, err)
	}
	defer func() {
		if alive(pid, marker) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}()
	if !alive(pid, marker) {
		t.Fatalf("control: without the tool, the background sleep of %q did not outlive its shell", command)
	}
}

// TestBashExitKillsWhatItLeftRunning: a command that exits leaving a child
// in the background — holding the output pipe, or with its output
// redirected — takes the child with it, and Run returns within the 2 s drain
// wait, not when the child would have ended.
func TestBashExitKillsWhatItLeftRunning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, command, marker string
		holdsPipe             bool
	}{
		{"holding the pipe", "sleep 602 &", "sleep 602", true},
		{"output redirected", "sleep 603 >/dev/null 2>&1 &", "sleep 603", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outlives(t, tc.command, tc.marker)
			env := bashEnv(t, nil)
			began := time.Now()
			r := startBash(t, prepareBash(t, env, map[string]any{"command": tc.command + " echo $! > pid"}), env)
			pid := bashPID(t, filepath.Join(env.Workspace, "pid"), tc.marker)
			res := r.await(t, 30*time.Second)
			took := time.Since(began)
			if res.IsError || res.Text != "(no output)" {
				t.Fatalf("result = %+v, want a clean exit", res)
			}
			gone(t, pid, tc.marker)
			switch {
			case tc.holdsPipe && (took < planDrain || took > planDrain+5*time.Second):
				t.Fatalf("Run took %v; want the %v drain wait, then the kill", took, planDrain)
			case !tc.holdsPipe && took >= planDrain:
				t.Fatalf("Run took %v; nothing held the pipe, so nothing should have waited", took)
			}
		})
	}
}

// TestBashTermIgnoringCommandIsKilled: a command whose leader ignores
// SIGTERM gets SIGKILL after the 3 s grace; one whose background child
// ignores it and holds the pipe has the child killed once the leader has
// exited and the 2 s drain wait is over. Either way Run returns within the
// grace and the drain wait of the cancel.
func TestBashTermIgnoringCommandIsKilled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, command, marker string
		atLeast               time.Duration
	}{
		{"the leader ignores it", "trap '' TERM; sleep 604 & echo $! > pid; wait", "sleep 604", planGrace},
		{"a child holding the pipe ignores it", "(trap '' TERM; sleep 605) & echo $! > pid; wait", "sleep 605", planDrain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := bashEnv(t, nil)
			r := startBash(t, prepareBash(t, env, map[string]any{"command": tc.command}), env)
			pid := bashPID(t, filepath.Join(env.Workspace, "pid"), tc.marker)
			if !alive(pid, tc.marker) {
				t.Fatal("control: the sleep is not running before the cancel")
			}
			cancelled := time.Now()
			r.cancel(nil)
			res := r.await(t, 30*time.Second)
			took := time.Since(cancelled)
			if res.Class != tool.ClassAborted {
				t.Fatalf("result = %+v, want aborted", res)
			}
			gone(t, pid, tc.marker)
			if took < tc.atLeast || took > planGrace+planDrain+3*time.Second {
				t.Fatalf("Run took %v after the cancel; want at least %v (SIGTERM is ignored) and at most the grace, the drain wait and a margin", took, tc.atLeast)
			}
		})
	}
}

// TestBashCloseKillsAtOnce: a cancel whose cause is tool.ErrClosing sends
// SIGKILL at once, so a command that ignores SIGTERM ends well inside the
// 3 s grace an ordinary cancel gives it (TestBashTermIgnoringCommandIsKilled
// is the control); and a close during a timeout's grace cuts the grace
// short.
func TestBashCloseKillsAtOnce(t *testing.T) {
	t.Parallel()
	t.Run("a close", func(t *testing.T) {
		t.Parallel()
		env := bashEnv(t, nil)
		r := startBash(t, prepareBash(t, env, map[string]any{"command": "trap '' TERM; sleep 606 & echo $! > pid; wait"}), env)
		pid := bashPID(t, filepath.Join(env.Workspace, "pid"), "sleep 606")
		closed := time.Now()
		r.cancel(fmt.Errorf("session 1: %w", tool.ErrClosing))
		res := r.await(t, 30*time.Second)
		if took := time.Since(closed); took >= planGrace-500*time.Millisecond {
			t.Fatalf("Run took %v after a close; want it well inside the %v grace", took, planGrace)
		}
		if res.Class != tool.ClassAborted || !strings.HasPrefix(res.Text, tool.AbortedText) {
			t.Fatalf("result = %+v, want aborted", res)
		}
		gone(t, pid, "sleep 606")
	})
	t.Run("a close during a timeout's grace", func(t *testing.T) {
		t.Parallel()
		env := bashEnv(t, nil)
		// The shell survives SIGTERM and says it got it.
		c := prepareBash(t, env, map[string]any{"command": "trap 'echo term > got' TERM; while :; do sleep 0.05; done", "timeout": 200})
		r := startBash(t, c, env)
		if !waitFor(15*time.Second, func() bool { _, err := os.Stat(filepath.Join(env.Workspace, "got")); return err == nil }) {
			t.Fatal("the timeout never sent SIGTERM")
		}
		closed := time.Now()
		r.cancel(tool.ErrClosing)
		res := r.await(t, 30*time.Second)
		if took := time.Since(closed); took >= planGrace-500*time.Millisecond {
			t.Fatalf("Run took %v after a close in the grace; want the grace cut short", took)
		}
		if res.Class != tool.ClassTimeout {
			t.Fatalf("result = %+v, want the timeout it was first", res)
		}
	})
}

// withClosing gives env a session close signal, Env.Closing, and returns
// the func that closes it.
func withClosing(env tool.Env) (tool.Env, func()) {
	ch := make(chan struct{})
	var once sync.Once
	env.Closing = ch
	return env, func() { once.Do(func() { close(ch) }) }
}

// untilFile waits for the command to make name in the workspace.
func untilFile(t *testing.T, env tool.Env, name, what string) {
	t.Helper()
	if !waitFor(15*time.Second, func() bool { _, err := os.Stat(filepath.Join(env.Workspace, name)); return err == nil }) {
		t.Fatalf("control: %s never happened", what)
	}
}

// TestBashCloseSignal: Env.Closing closes the session for a call in every
// state — the case it exists for being a call an ordinary cancel has already
// cancelled, whose ctx can carry no other cause. A close during that
// cancel's grace kills at once, so Run returns well inside the grace a
// TERM-ignoring child would otherwise take; a close during the drain wait
// ends it; a close while the command runs kills it; a close skips the spill
// wait. The controls: with the channel left open, the same call waits the
// grace out (TestBashTermIgnoringCommandIsKilled), the drain out
// (TestBashExitKillsWhatItLeftRunning), and the spill wait out (below).
func TestBashCloseSignal(t *testing.T) {
	t.Parallel()
	t.Run("after a cancel, during its grace", func(t *testing.T) {
		t.Parallel()
		env, closeSession := withClosing(bashEnv(t, nil))
		// The shell survives SIGTERM and says it got it; its child ignores
		// SIGTERM.
		c := prepareBash(t, env, map[string]any{"command": "trap 'echo term > got' TERM; (trap '' TERM; sleep 614) & echo $! > pid; while :; do sleep 0.05; done"})
		r := startBash(t, c, env)
		pid := bashPID(t, filepath.Join(env.Workspace, "pid"), "sleep 614")
		r.cancel(nil)
		untilFile(t, env, "got", "the cancel's SIGTERM")
		closed := time.Now()
		closeSession()
		res := r.await(t, 30*time.Second)
		if took := time.Since(closed); took > 1500*time.Millisecond {
			t.Fatalf("Run took %v after a close in the grace; want the grace (%v) cut short", took, planGrace)
		}
		if res.Class != tool.ClassAborted || !strings.HasPrefix(res.Text, tool.AbortedText) {
			t.Fatalf("result = %+v, want the abort it was first", res)
		}
		gone(t, pid, "sleep 614")
	})
	t.Run("while it runs", func(t *testing.T) {
		t.Parallel()
		env, closeSession := withClosing(bashEnv(t, nil))
		r := startBash(t, prepareBash(t, env, map[string]any{"command": "trap '' TERM; sleep 615 & echo $! > pid; wait"}), env)
		pid := bashPID(t, filepath.Join(env.Workspace, "pid"), "sleep 615")
		closed := time.Now()
		closeSession()
		res := r.await(t, 30*time.Second)
		if took := time.Since(closed); took > 1500*time.Millisecond {
			t.Fatalf("Run took %v after a close", took)
		}
		if res.Class != tool.ClassAborted {
			t.Fatalf("result = %+v, want aborted", res)
		}
		gone(t, pid, "sleep 615")
	})
	t.Run("during the drain wait", func(t *testing.T) {
		t.Parallel()
		env, closeSession := withClosing(bashEnv(t, nil))
		// The leader exits at once; its child holds the pipe.
		r := startBash(t, prepareBash(t, env, map[string]any{"command": "echo $$ > leader; sleep 616 & echo $! > pid"}), env)
		leader := bashPID(t, filepath.Join(env.Workspace, "leader"), "sleep 616")
		pid := bashPID(t, filepath.Join(env.Workspace, "pid"), "sleep 616")
		if !waitFor(15*time.Second, func() bool { return !alive(leader, "sleep 616") }) {
			t.Fatal("control: the leader never exited")
		}
		closed := time.Now()
		closeSession()
		res := r.await(t, 30*time.Second)
		if took := time.Since(closed); took > time.Second {
			t.Fatalf("Run took %v after a close in the drain wait; want the %v wait cut short", took, planDrain)
		}
		if res.IsError || res.Text != "(no output)" {
			t.Fatalf("result = %+v, want the exit it was first", res)
		}
		gone(t, pid, "sleep 616")
	})
	t.Run("before it starts", func(t *testing.T) {
		t.Parallel()
		env, closeSession := withClosing(bashEnv(t, nil))
		closeSession()
		res := startBash(t, prepareBash(t, env, map[string]any{"command": ": > ran"}), env).await(t, 30*time.Second)
		failed(t, res, tool.ClassAborted, tool.AbortedText)
		if _, err := os.Stat(filepath.Join(env.Workspace, "ran")); err == nil {
			t.Fatal("the command ran")
		}
	})
	// The spill file stalls, so an ordinary cancel waits the 1 s spill wait
	// out (the control) and a close does not — neither one before the wait
	// nor one that comes during it.
	for _, tc := range []struct {
		name          string
		cancel, close bool
	}{
		{"skipping the spill wait", false, true},
		{"ending the spill wait", true, true},
		{"control: a cancel waits for the spill", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, closeSession := withClosing(bashEnv(t, nil))
			c := prepareBash(t, env, map[string]any{"command": "seq 1 20000; echo $$ > leader; sleep 617"})
			stallOp(t, c, "spill write")
			r := startBash(t, c, env)
			untilFile(t, env, "leader", "the output")
			stopped := time.Now()
			if tc.cancel {
				r.cancel(nil)
			}
			if tc.close {
				if tc.cancel {
					// The cancel ends the command at once (sleep dies on
					// SIGTERM), so Run is soon in the spill wait; nothing
					// shows when, so the close simply comes later. Should it
					// come first, the wait is skipped: the bound holds
					// either way.
					time.Sleep(300 * time.Millisecond)
				}
				closeSession()
			}
			res := r.await(t, 30*time.Second)
			took := time.Since(stopped)
			if res.Class != tool.ClassAborted || res.Trunc.Spill != "" {
				t.Fatalf("result = {Class:%q Spill:%q}, want aborted with no file", res.Class, res.Trunc.Spill)
			}
			switch {
			case tc.close && took > 800*time.Millisecond:
				t.Fatalf("Run took %v with a close and the spill stalled; want no more spill wait", took)
			case !tc.close && took < time.Second:
				t.Fatalf("Run took %v after a cancel with the spill stalled; want the 1 s spill wait", took)
			}
		})
	}
}

// TestBashCancelContinuesAStoppedCommand: a cancel continues the group as
// it terminates it, so a stopped command acts on SIGTERM at once instead of
// waiting out the grace for SIGKILL.
func TestBashCancelContinuesAStoppedCommand(t *testing.T) {
	t.Parallel()
	env := bashEnv(t, nil)
	r := startBash(t, prepareBash(t, env, map[string]any{"command": "echo $$ > leader; kill -STOP $$; sleep 609"}), env)
	leader := bashPID(t, filepath.Join(env.Workspace, "leader"), "sleep 609")
	if !waitFor(15*time.Second, func() bool { state, _, ok := proc(leader); return ok && strings.HasPrefix(state, "T") }) {
		t.Fatal("control: the command never stopped")
	}
	cancelled := time.Now()
	r.cancel(nil)
	if res := r.await(t, 30*time.Second); res.Class != tool.ClassAborted {
		t.Fatalf("result = %+v, want aborted", res)
	}
	if took := time.Since(cancelled); took >= planGrace-500*time.Millisecond {
		t.Fatalf("Run took %v: the stopped command waited out the grace", took)
	}
}

// TestBashLeaderStaysUnreaped: on Linux, the leader that has exited is not
// reaped until the group has had its last SIGKILL, so its pid — the group's
// id — cannot pass to a stranger's group in between: during the drain wait
// it is a zombie. Once Run returns it is reaped.
func TestBashLeaderStaysUnreaped(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("macOS's waitid is not reliable for this; the leader is reaped before the last kill there (group.watch)")
	}
	t.Parallel()
	env := bashEnv(t, nil)
	// The leader exits at once; its background sleep holds the pipe, so the
	// drain wait runs.
	r := startBash(t, prepareBash(t, env, map[string]any{"command": "echo $$ > leader; sleep 610 & echo $! > pid"}), env)
	leader := bashPID(t, filepath.Join(env.Workspace, "leader"), "sleep 610")
	pid := bashPID(t, filepath.Join(env.Workspace, "pid"), "sleep 610")
	if !waitFor(planDrain, func() bool { state, _, ok := proc(leader); return ok && strings.HasPrefix(state, "Z") }) {
		state, args, _ := proc(leader)
		t.Fatalf("the exited leader was reaped, or never exited, before the group was killed: %q %q", state, args)
	}
	if res := r.await(t, 30*time.Second); res.IsError {
		t.Fatalf("result = %+v", res)
	}
	gone(t, pid, "sleep 610")
	// A zombie's command line is gone ("[bash] <defunct>"); its state says.
	if state, args, ok := proc(leader); ok && strings.HasPrefix(state, "Z") {
		t.Fatalf("the leader was not reaped: %q %q", state, args)
	}
}

// TestBashSetsidEscapee is the one way out of the kill, documented here and
// not asserted (plan 019 §3.9, §9).
func TestBashSetsidEscapee(t *testing.T) {
	t.Skip("documented, not asserted: a process that calls setsid(2) — setsid(1), a daemon — or moves to another " +
		"process group with setpgid(2) — a shell's job control, `set -m` — leaves the command's process group, and " +
		"the group kill does not reach it; internal/acp's group kill makes the same promise")
}

// TestBashNoControllingTerminal: the command runs in a session of its own,
// as the leader of its own process group, with no controlling terminal, so
// `: </dev/tty` fails in it. The control runs in a helper process that does
// have a controlling terminal, a pseudo-terminal, where an ordinary child
// opens /dev/tty and the tool's child cannot — so the failure is the tool's
// doing, whether or not this test process has a terminal.
func TestBashNoControllingTerminal(t *testing.T) {
	t.Parallel()
	res := runBash(t, bashEnv(t, nil), map[string]any{"command": "echo $$; ps -o pgid= -p $$; : </dev/tty"})
	fields := strings.Fields(res.Text)
	if res.IsError || len(fields) < 2 || res.Output.ExitCode == 0 || !strings.Contains(res.Text, "/dev/tty") {
		t.Fatalf("result = %+v, want /dev/tty refused", res)
	}
	if fields[0] != fields[1] || fields[1] == strconv.Itoa(syscall.Getpgrp()) {
		t.Fatalf("the shell %s is in process group %s (this test's is %d); want its own", fields[0], fields[1], syscall.Getpgrp())
	}

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptmx.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	helper := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBashTTYHelper$", "-test.v", "-test.count=1")
	helper.Env = append(os.Environ(), ttyHelper+"=1")
	helper.Stdin = tty
	var out bytes.Buffer
	helper.Stdout, helper.Stderr = &out, &out
	helper.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	err = helper.Run()
	_ = tty.Close()
	if err != nil || !strings.Contains(out.String(), "--- PASS: TestBashTTYHelper") {
		t.Fatalf("the helper under a terminal failed (%v):\n%s", err, out.String())
	}
}

// ttyHelper is set in TestBashTTYHelper's environment when
// TestBashNoControllingTerminal runs it under a terminal.
const ttyHelper = "CRAZE_BASH_TTY_HELPER"

func TestBashTTYHelper(t *testing.T) {
	if os.Getenv(ttyHelper) != "1" {
		t.Skip("run by TestBashNoControllingTerminal, under a pseudo-terminal")
	}
	if out, err := exec.Command(shPath, "-c", ": </dev/tty").CombinedOutput(); err != nil {
		t.Fatalf("control: an ordinary child cannot open /dev/tty (%v: %s), so this process has no terminal and proves nothing", err, out)
	}
	res := runBash(t, bashEnv(t, nil), map[string]any{"command": ": </dev/tty"})
	if res.IsError || res.Output.ExitCode == 0 || !strings.Contains(res.Text, "exit code: ") {
		t.Fatalf("the tool's child opened the controlling terminal: %+v", res)
	}
}

// envNames is the set of variable names in `env` output.
func envNames(text string) map[string]string {
	vars := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			vars[name] = value
		}
	}
	return vars
}

// TestBashEnvironment: the child gets the environment the harness passes,
// with craze's provider keys already gone, and never an OPENAI_* variable;
// everything else stays, and PWD is the working directory. The control
// passes the provider key unfiltered, and the child sees it.
func TestBashEnvironment(t *testing.T) {
	t.Parallel()
	base := append(slices.Clip(os.Environ()),
		"CRAZE_TEST_PROVIDER_KEY="+keyA, "OPENAI_API_KEY=sk-openai-canary", "OPENAI_BASE_URL=http://example.invalid",
		"CRAZE_TEST_UNRELATED=kept", "GITHUB_TOKEN=gh-kept")
	env := bashEnv(t, nil)
	env.Environ = tool.ChildEnviron(base, []string{"CRAZE_TEST_PROVIDER_KEY"})
	vars := envNames(ok(t, runBash(t, env, map[string]any{"command": "env"})))
	for _, name := range []string{"CRAZE_TEST_PROVIDER_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL"} {
		if _, found := vars[name]; found {
			t.Errorf("the child has %s", name)
		}
	}
	if vars["CRAZE_TEST_UNRELATED"] != "kept" || vars["GITHUB_TOKEN"] != "gh-kept" || vars["PWD"] != env.Workspace {
		t.Errorf("the child lacks what it should have: CRAZE_TEST_UNRELATED=%q GITHUB_TOKEN=%q PWD=%q", vars["CRAZE_TEST_UNRELATED"], vars["GITHUB_TOKEN"], vars["PWD"])
	}

	// The control: unfiltered, the key reaches the child, but OPENAI_* never
	// does — the tool removes those itself.
	env.Environ = base
	vars = envNames(ok(t, runBash(t, env, map[string]any{"command": "env"})))
	if vars["CRAZE_TEST_PROVIDER_KEY"] != keyA {
		t.Fatal("control: an unfiltered variable does not reach the child, so the test proves nothing")
	}
	if _, found := vars["OPENAI_API_KEY"]; found {
		t.Error("the tool passed OPENAI_API_KEY on")
	}
}

// TestBashRefusesWithoutAnEnvironment: with no Env.Environ bash runs
// nothing — it cannot know which of craze's variables hold provider keys, so
// it fails closed rather than fall back to craze's own environment. The
// controls: this process's environment, the fallback it refuses, would have
// handed the child a provider key; an empty environment is a configured one,
// and runs. Not parallel: it sets the process's environment.
func TestBashRefusesWithoutAnEnvironment(t *testing.T) {
	t.Setenv("FIREWORKS_API_KEY", keyA)
	env := bashEnv(t, nil)
	env.Environ = nil
	// Were it to run, its text would be the environment: the failure names
	// only the class, so no test log ever holds that.
	res := runBash(t, env, map[string]any{"command": ": > ran; env"})
	if !res.IsError || res.Class != tool.ClassToolError || res.Text != noEnvironText {
		t.Fatalf("result is {IsError:%v Class:%q}, want the tool_error %q", res.IsError, res.Class, noEnvironText)
	}
	if _, err := os.Stat(filepath.Join(env.Workspace, "ran")); err == nil {
		t.Fatal("the command ran")
	}

	env.Environ = os.Environ()
	if vars := envNames(ok(t, runBash(t, env, map[string]any{"command": "env"}))); vars["FIREWORKS_API_KEY"] != keyA {
		t.Fatal("control: craze's own environment does not hand the child the key, so refusing it proves nothing")
	}
	env.Environ = []string{}
	if text := ok(t, runBash(t, env, map[string]any{"command": ": > ran; echo ran"})); text != "ran\n" {
		t.Fatalf("an empty environment did not run: %q", text)
	}
}

// snapshots records progress snapshots.
type snapshots struct {
	mu   sync.Mutex
	list []string
}

func (s *snapshots) add(snap string) {
	s.mu.Lock()
	s.list = append(s.list, snap)
	s.mu.Unlock()
}

func (s *snapshots) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.list)
}

// TestBashRedactsItsOutput: a key the command prints never reaches the
// result, a progress snapshot or the spill file — not even when it reaches
// the pipe in two writes 0.8 s apart, so that a snapshot is taken
// between them. The control, with no redactor, sees the key, and a snapshot
// holding its first half alone, which is what shows it really was split.
func TestBashRedactsItsOutput(t *testing.T) {
	t.Parallel()
	const split = `printf 'before\n'; printf 'sk-canary-'; sleep 0.8; printf 'alpha-0001\n'; sleep 0.8; printf 'after\n'`
	run := func(red *redact.Replacer, command string) (tool.Result, []string, tool.Env) {
		env := bashEnv(t, red)
		var s snapshots
		env.Progress = s.add
		res := runBash(t, env, map[string]any{"command": command})
		return res, s.all(), env
	}
	leaks := func(s string) bool { return strings.Contains(s, "sk-canary-") || strings.Contains(s, "alpha-0001") }

	res, snaps, _ := run(redact.New(keyA), split)
	if want := "before\n" + redact.Marker + "\nafter\n"; res.Text != want || res.Output.Output != want {
		t.Fatalf("result = %q, want %q", res.Text, want)
	}
	marked := false
	for _, s := range snaps {
		if leaks(s) {
			t.Fatalf("a progress snapshot holds the key or part of it: %q", s)
		}
		marked = marked || strings.Contains(s, redact.Marker)
	}
	if !marked {
		t.Fatalf("no snapshot was taken after the key was printed: %q", snaps)
	}

	res, snaps, _ = run(nil, split)
	if !strings.Contains(res.Text, keyA) {
		t.Fatalf("control: with no redactor the key is not in the result: %q", res.Text)
	}
	if !slices.ContainsFunc(snaps, func(s string) bool { return strings.Contains(s, "sk-canary-") && !strings.Contains(s, keyA) }) {
		t.Fatalf("control: no snapshot saw the key half written, so it was not split across reads: %q", snaps)
	}

	// Past 50 KiB the output spills; the key comes after that, split again.
	const spilled = `seq 1 12000; printf 'sk-canary-'; sleep 0.3; printf 'alpha-0001\n'`
	res, snaps, env := run(redact.New(keyA), spilled)
	if res.Trunc.Spill == "" || leaks(res.Text) || !strings.HasSuffix(res.Text, redact.Marker+"\n") {
		t.Fatalf("result = %q, Trunc %+v; want the tail redacted and a spill file", res.Text, res.Trunc)
	}
	for _, s := range snaps {
		if leaks(s) {
			t.Fatal("a progress snapshot holds the key or part of it")
		}
	}
	spill := load(t, res.Trunc.Spill)
	if leaks(spill) || !strings.HasSuffix(spill, "12000\n"+redact.Marker+"\n") {
		t.Fatalf("the spill file holds the key, or not the marker: ...%q", spill[max(len(spill)-80, 0):])
	}
	if p := perm(t, res.Trunc.Spill); p != 0o600 || filepath.Dir(res.Trunc.Spill) != filepath.Join(env.Home, tool.SpillDir) {
		t.Fatalf("spill file %s is mode %v", res.Trunc.Spill, p)
	}
	res, _, _ = run(nil, spilled)
	if !strings.Contains(load(t, res.Trunc.Spill), keyA) {
		t.Fatal("control: with no redactor the key is not in the spill file")
	}
}

// seqOutput is `seq from to`'s output.
func seqOutput(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	return b.String()
}

// TestBashTruncation ports opencode's truncation cases, through the
// dispatcher as the runner will call it: past 2000 lines or 50 KiB the model
// gets the tail with opencode's notice naming the spill file, which holds
// everything, mode 0600; the dispatcher does not cut it again. Small output
// is the control: no notice, no file.
func TestBashTruncation(t *testing.T) {
	t.Parallel()
	notice := func(res tool.Result) string {
		return "...output truncated...\n\nFull output saved to: " + res.Trunc.Spill + "\n\n"
	}
	for _, tc := range []struct {
		name, command, full, kept string
		keptLines                 int
	}{
		// 20000 lines, 108894 bytes: the line limit binds first.
		{"lines and bytes", "seq 1 20000", seqOutput(1, 20000), seqOutput(18002, 20000), 2000},
		// "full output is saved to file when truncated": lines only.
		{"lines", "seq 1 2100", seqOutput(1, 2100), seqOutput(102, 2100), 2000},
		// One line of 61440 bytes: its last 51200 are kept.
		{"bytes", `head -c 61440 /dev/zero | tr '\0' a`, strings.Repeat("a", 61440), strings.Repeat("a", 51200), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			_, res := f.call(t, "bash", map[string]any{"command": tc.command})
			if text := ok(t, res); res.Trunc.Spill == "" || text != notice(res)+tc.kept {
				t.Fatalf("text = %.200q..., want the notice and the tail", text)
			}
			if res.Output.Output != tc.kept {
				t.Fatal("Output.Output is not the tail the model was shown")
			}
			want := tool.Truncation{KeptBytes: len(tc.kept), TotalBytes: len(tc.full), KeptLines: tc.keptLines,
				TotalLines: strings.Count(tc.full, "\n") + 1, Spill: res.Trunc.Spill}
			if res.Trunc != want {
				t.Fatalf("Trunc = %+v, want %+v", res.Trunc, want)
			}
			if load(t, res.Trunc.Spill) != tc.full {
				t.Fatal("the spill file does not hold the whole output")
			}
			if p := perm(t, res.Trunc.Spill); p != 0o600 || filepath.Base(res.Trunc.Spill) != "tool_t1.1.1" {
				t.Fatalf("spill file %s is mode %v", res.Trunc.Spill, p)
			}
		})
	}
	t.Run("small", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		_, res := f.call(t, "bash", map[string]any{"command": "seq 1 1"})
		if text := ok(t, res); text != "1\n" || res.Trunc.Spill != "" || res.Trunc.Truncated() {
			t.Fatalf("result = %+v, want the output alone", res)
		}
		if _, err := os.Stat(filepath.Join(f.env.Home, tool.SpillDir)); err == nil {
			t.Fatal("small output made a spill directory")
		}
	})
}

// TestTailText pins opencode's tail rule at its edges, with small limits.
func TestTailText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text, kept string
		cut              bool
	}{
		{"fits", "a\nb\nc", "a\nb\nc", false},
		{"fits exactly", "ab\ncd\n", "ab\ncd\n", false}, // 3 lines, 6 bytes
		{"too many lines", "a\nb\nc\nd", "b\nc\nd", true},
		{"a trailing newline is a line", "a\nb\nc\n", "b\nc\n", true},
		{"too many bytes", "aaaa\nbb\ncc", "bb\ncc", true},
		{"the last line alone is too long", "a\nbbbbbbbbbb", "bbbbbb", true},
		{"a cut inside a character moves past it", "a\nxxé" + strings.Repeat("y", 5), "yyyyy", true},
	} {
		kept, cut := tailText(tc.text, 3, 6)
		if kept != tc.kept || cut != tc.cut {
			t.Errorf("%s: tailText(%q) = %q, %v; want %q, %v", tc.name, tc.text, kept, cut, tc.kept, tc.cut)
		}
	}
}

// TestBashStreamsProgress ports "streams metadata updates progressively":
// a snapshot shows the first line before the second exists, and a later one
// shows both.
func TestBashStreamsProgress(t *testing.T) {
	t.Parallel()
	env := bashEnv(t, nil)
	var s snapshots
	env.Progress = s.add
	res := runBash(t, env, map[string]any{"command": "echo first; sleep 0.8; echo second; sleep 0.8"})
	if ok(t, res) != "first\nsecond\n" {
		t.Fatalf("result = %q", res.Text)
	}
	snaps := s.all()
	early := slices.Index(snaps, "first\n")
	late := slices.Index(snaps, "first\nsecond\n")
	if early < 0 || late < early {
		t.Fatalf("snapshots = %q, want the first line alone, then both", snaps)
	}
}

// TestBashProgressIsThrottled: a command that writes a hundred times gets at
// most one snapshot per 100 ms of its run, not one per write.
func TestBashProgressIsThrottled(t *testing.T) {
	t.Parallel()
	env := bashEnv(t, nil)
	var n atomic.Int64
	env.Progress = func(string) { n.Add(1) }
	began := time.Now()
	text := ok(t, runBash(t, env, map[string]any{"command": `i=0; while [ $i -lt 100 ]; do echo "line $i"; i=$((i+1)); sleep 0.01; done`}))
	took := time.Since(began)
	if strings.Count(text, "\n") != 100 {
		t.Fatalf("the command wrote %q", text)
	}
	// The plan's interval, not the package's constant, which is what is
	// under test.
	if got, most := n.Load(), int64(took/(100*time.Millisecond))+1; got < 2 || got > most {
		t.Fatalf("%d snapshots in %v; want at least 2 and at most %d", got, took, most)
	}
}

// TestBashBlockedProgress: a consumer that takes a snapshot and never
// returns holds up neither the command, whose output is read in full, nor
// Run, which returns while the consumer is still blocked.
func TestBashBlockedProgress(t *testing.T) {
	t.Parallel()
	env := bashEnv(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	env.Progress = func(string) {
		once.Do(func() { close(entered) })
		<-release
	}
	t.Cleanup(func() { close(release) })
	began := time.Now()
	r := startBash(t, prepareBash(t, env, map[string]any{"command": "seq 1 30000; sleep 0.5; seq 30001 60000; echo done"}), env)
	res := r.await(t, 30*time.Second)
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("Run took %v", took)
	}
	select {
	case <-entered:
	default:
		t.Fatal("control: the consumer was never called, so it blocked nothing")
	}
	if text := ok(t, res); !strings.HasSuffix(text, "60000\ndone\n") || load(t, res.Trunc.Spill) != seqOutput(1, 60000)+"done\n" {
		t.Fatalf("the output was not read in full: ...%q", text[max(len(text)-40, 0):])
	}
}

// stall holds one of a call's operations until it is freed, as a filesystem
// that does not answer would hold a syscall: entered closes once the
// operation has been called and is waiting; free lets it go on, and the
// test's cleanup frees it if the test did not.
type stall struct {
	entered, release chan struct{}
	enter, let       sync.Once
}

func newStall(t *testing.T) *stall {
	s := &stall{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(s.free)
	return s
}

// hold is called by the stalled operation: it waits until the stall is freed.
func (s *stall) hold() {
	s.enter.Do(func() { close(s.entered) })
	<-s.release
}

func (s *stall) free() { s.let.Do(func() { close(s.release) }) }

// await fails the test unless the operation is reached and held.
func (s *stall) await(t *testing.T, what string) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(15 * time.Second):
		t.Fatalf("control: %s was never reached, so nothing stalled", what)
	}
}

// stallOps names every operation of a call that stallOp can hold: each
// filesystem or process call bash makes, on its own (bashCall.ops).
var stallOps = []string{"workdir stat", "tmp mkdir", "exec start", "spill open", "spill write", "spill close"}

// stallOp makes c's op hold until the stall it returns is freed; the op then
// goes on as it would have. The spill ops use a spill file in memory, so
// nothing is written under the test's directories after the test ends.
func stallOp(t *testing.T, c *bashCall, op string) *stall {
	t.Helper()
	s := newStall(t)
	real := c.ops
	f := &fakeSpill{name: filepath.Join(t.TempDir(), "spill"), closed: make(chan struct{})}
	switch op {
	case "workdir stat":
		c.ops.stat = func(p string) (os.FileInfo, error) { s.hold(); return real.stat(p) }
	case "tmp mkdir":
		c.ops.ensureTmp = func(p string) error { s.hold(); return real.ensureTmp(p) }
	case "exec start":
		c.ops.start = func(cmd *exec.Cmd) (*group, error) { s.hold(); return real.start(cmd) }
	case "spill open":
		c.ops.openSpill = func(string, string) (spillFile, error) { s.hold(); return f, nil }
	case "spill write":
		f.onWrite = s
		c.ops.openSpill = func(string, string) (spillFile, error) { return f, nil }
	case "spill close":
		f.onClose = s
		c.ops.openSpill = func(string, string) (spillFile, error) { return f, nil }
	default:
		t.Fatalf("no such op to stall: %q", op)
	}
	return s
}

// startedPIDs wraps c's start so the test learns the pid of what it starts,
// whenever that is.
func startedPIDs(c *bashCall) <-chan int {
	pids := make(chan int, 1)
	start := c.ops.start
	c.ops.start = func(cmd *exec.Cmd) (*group, error) {
		g, err := start(cmd)
		if err == nil {
			pids <- g.pid
		}
		return g, err
	}
	return pids
}

// fakeSpill is a spill file in memory. Each Write may take delay, as a slow
// filesystem's would; a Write or the Close may hold on a stall, as a stalled
// filesystem's would. It keeps what reaches it.
type fakeSpill struct {
	name     string
	delay    time.Duration
	onWrite  *stall        // set: every Write holds until it is freed
	onClose  *stall        // set: Close holds until it is freed
	writeErr error         // set: every Write fails with it, after any hold
	closed   chan struct{} // closed once Close has returned
	once     sync.Once

	mu  sync.Mutex
	buf bytes.Buffer
}

// slowSpill returns a spill file whose every write takes delay, and an
// opener that hands it out.
func slowSpill(t *testing.T, delay time.Duration) (*fakeSpill, spillOpener) {
	t.Helper()
	f := &fakeSpill{name: filepath.Join(t.TempDir(), "spill"), delay: delay, closed: make(chan struct{})}
	return f, func(string, string) (spillFile, error) { return f, nil }
}

func (f *fakeSpill) Write(p []byte) (int, error) {
	if f.onWrite != nil {
		f.onWrite.hold()
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf.Write(p)
	return len(p), nil
}

func (f *fakeSpill) Close() error {
	if f.onClose != nil {
		f.onClose.hold()
	}
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeSpill) Name() string { return f.name }

func (f *fakeSpill) content() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}

// notSaved is the notice a cut output gets when there is no spill file.
const notSaved = "...output truncated...\n\nThe full output could not be saved.\n\n"

// timeoutText is opencode's metadata line for a command stopped at its
// timeout of ms.
func timeoutText(ms int) string {
	return fmt.Sprintf("shell tool terminated command after exceeding timeout %d ms. If this command is expected to take longer and is not waiting for interactive input, retry with a larger timeout value in milliseconds.", ms)
}

// TestBashStalledFilesystem (AC1, AC3): with any one of the call's
// filesystem or process operations held for ever — the workdir check, the
// temporary directory, the exec, the spill file's open, write or close — Run
// still returns within its bound after a cancel, a close, its timeout, or
// the leader's exit, and says what it honestly can: aborted, timed out, or
// the output with no file. Whatever the stalled start goes on to start once
// freed is killed. The control for each op is the same stall, freed: the
// command runs and the output is saved. Were any lock on Run's path held
// across the stalled call, or any wait on it unbounded, the case would hang
// until await's limit.
func TestBashStalledFilesystem(t *testing.T) {
	t.Parallel()
	full := seqOutput(1, 20000)
	n := 0
	for _, op := range stallOps {
		preStart := !strings.HasPrefix(op, "spill")
		for _, trigger := range []string{"cancel", "close", "timeout", "exit", "control"} {
			if preStart && trigger == "exit" {
				continue // nothing has started, so nothing exits
			}
			marker := fmt.Sprintf("sleep %d", 620+n)
			n++
			t.Run(op+", "+trigger, func(t *testing.T) {
				t.Parallel()
				env, closeSession := withClosing(bashEnv(t, nil))
				// A spill op is reached once the output passes 50 KiB; the
				// command then says its output is all written (leader) and
				// waits for the trigger. A pre-start op is reached before
				// anything runs.
				in := map[string]any{"command": "seq 1 20000; echo $$ > leader; " + marker}
				timeout := 3 * time.Second // past the leader marker on any machine
				switch {
				case preStart && trigger == "control":
					in["command"] = "echo ran"
				case preStart:
					in["command"], timeout = marker, 300*time.Millisecond
				case trigger == "exit", trigger == "control":
					in["command"] = "seq 1 20000"
				}
				if trigger == "timeout" {
					in["timeout"] = timeout.Milliseconds()
				}
				c := prepareBash(t, env, in)
				s := stallOp(t, c, op)
				pids := startedPIDs(c)
				r := startBash(t, c, env)
				// The spill file's close is reached only on Run's return
				// path, so after the trigger; everything else before it.
				ending := trigger == "exit" || trigger == "control"
				if !preStart && !ending {
					untilFile(t, env, "leader", "the output")
				}
				if op != "spill close" || ending {
					s.await(t, op)
				}
				stopped := time.Now()
				switch trigger {
				case "cancel":
					r.cancel(nil)
				case "close":
					closeSession()
				case "control":
					s.free()
				}
				if op == "spill close" && !ending {
					s.await(t, op)
				}
				res := r.await(t, 30*time.Second)
				took := time.Since(stopped)

				// The bound: the spill wait (1 s) where the output ended and
				// the session is not closing, the timeout where it is the
				// trigger, and a margin for a loaded machine.
				limit := 3 * time.Second
				if !preStart && trigger != "close" {
					limit += time.Second
				}
				if trigger == "timeout" {
					limit += timeout
				}
				if trigger != "control" && took > limit {
					t.Fatalf("Run took %v after %s with %s stalled; want at most %v", took, trigger, op, limit)
				}

				switch {
				case trigger == "control" && preStart:
					if text := ok(t, res); text != "ran\n" {
						t.Fatalf("control: freed, the command did not run: %q", text)
					}
					return
				case trigger == "control":
					if text := ok(t, res); res.Trunc.Spill == "" || !strings.HasPrefix(text, "...output truncated...\n\nFull output saved to: ") {
						t.Fatalf("control: freed, the output was not saved: %.120q", text)
					}
					return
				case preStart && trigger == "timeout":
					failed(t, res, tool.ClassTimeout, "(no output)"+meta(timeoutText(300)))
				case preStart:
					failed(t, res, tool.ClassAborted, tool.AbortedText)
				default:
					// Every byte was read, none of it saved, and the result
					// says so; the tail is shown whatever the class.
					class := tool.ErrorClass("")
					switch trigger {
					case "cancel", "close":
						class = tool.ClassAborted
					case "timeout":
						class = tool.ClassTimeout
					}
					if res.Class != class || res.Trunc.Spill != "" || res.Trunc.TotalBytes != len(full) || !strings.Contains(res.Text, notSaved+seqOutput(18002, 20000)) {
						t.Fatalf("result = {Class:%q Spill:%q TotalBytes:%d Text:%.100q...}, want class %q, every byte read, no file", res.Class, res.Trunc.Spill, res.Trunc.TotalBytes, res.Text, class)
					}
				}

				// Freed, a start stalled before the stop's check starts
				// nothing: the check sees the stop. What was started — the
				// command that ran, or one whose exec was the stall, after
				// the check — dies: the group kill, or the abandoned start's.
				s.free()
				if preStart && op != "exec start" {
					select {
					case pid := <-pids:
						t.Fatalf("a start freed after %s started the command (pid %d)", trigger, pid)
					case <-time.After(500 * time.Millisecond):
					}
					return
				}
				var pid int
				select {
				case pid = <-pids:
				case <-time.After(15 * time.Second):
					t.Fatal("the command was never started, even once the stall was freed")
				}
				t.Cleanup(func() {
					if alive(pid, marker) {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				})
				gone(t, pid, marker)
			})
		}
	}
}

// TestBashTimeoutCoversTheStart (AC2): the timeout runs from Run's entry, so
// a slow start eats into it rather than adding to it. The workdir check takes
// 1 s of a 1.5 s timeout; the command is stopped 1.5 s after the call began,
// not 2.5 s. A timer started only once the command had started would put
// Run past 2.5 s.
func TestBashTimeoutCoversTheStart(t *testing.T) {
	t.Parallel()
	env := bashEnv(t, nil)
	c := prepareBash(t, env, map[string]any{"command": "sleep 619", "timeout": 1500})
	stat := c.ops.stat
	c.ops.stat = func(p string) (os.FileInfo, error) { time.Sleep(time.Second); return stat(p) }
	pids := startedPIDs(c)
	began := time.Now()
	res := startBash(t, c, env).await(t, 30*time.Second)
	took := time.Since(began)
	failed(t, res, tool.ClassTimeout, "(no output)"+meta(timeoutText(1500)))
	if took < 1500*time.Millisecond || took > 2400*time.Millisecond {
		t.Fatalf("Run took %v; want the 1.5 s timeout counted from the call's start, not from the command's", took)
	}
	gone(t, <-pids, "sleep 619")
}

// TestSpillerAbandonDropsTheBacklog (AC4): on every path that gives the spill
// file up — the spill wait passing, the session closing during it, no wait
// at all, the writer too far behind, a file that cannot be opened, a write
// that fails — the backlog is dropped at once, nothing more is taken, and a
// writer that comes back finds nothing to write and offers no file. Each
// case first has the writer held in its first write (or its open) with a
// chunk waiting in the backlog behind it, and checks that it is: the control
// that there was something to drop. The last control is a writer that keeps
// up: its file is offered.
func TestSpillerAbandonDropsTheBacklog(t *testing.T) {
	t.Parallel()
	chunk := bytes.Repeat([]byte{'x'}, 1<<20)
	closing := make(chan struct{})
	close(closing)
	backlog := func(s *spiller) int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.backlog)
	}
	// primed starts a writer whose first write holds, and fails with
	// writeErr once freed; waits until it is held; then puts a chunk in the
	// backlog behind it.
	primed := func(t *testing.T, writeErr error) (*spiller, *stall) {
		t.Helper()
		st := newStall(t)
		f, open := slowSpill(t, 0)
		f.onWrite, f.writeErr = st, writeErr
		s := startSpiller(open, "", "")
		s.send(chunk)
		st.await(t, "the write")
		s.send(chunk)
		if n := backlog(s); n != len(chunk) {
			t.Fatalf("control: the backlog holds %d bytes behind the held write, not the chunk", n)
		}
		return s, st
	}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) (s *spiller, free func())
	}{
		{"the spill wait passing", func(t *testing.T) (*spiller, func()) {
			s, st := primed(t, nil)
			if got := s.wait(20*time.Millisecond, nil); got != "" {
				t.Fatalf("wait offered %q", got)
			}
			return s, st.free
		}},
		{"the session closing during the spill wait", func(t *testing.T) (*spiller, func()) {
			s, st := primed(t, nil)
			if got := s.wait(time.Hour, closing); got != "" {
				t.Fatalf("wait offered %q", got)
			}
			return s, st.free
		}},
		{"no spill wait", func(t *testing.T) (*spiller, func()) {
			s, st := primed(t, nil)
			if got := s.wait(0, nil); got != "" {
				t.Fatalf("wait offered %q", got)
			}
			return s, st.free
		}},
		{"the writer too far behind", func(t *testing.T) (*spiller, func()) {
			st := newStall(t)
			f, open := slowSpill(t, 0)
			f.onWrite = st
			o := &output{open: open, cap: 1 << 40}
			_, _ = o.Write(bytes.Repeat([]byte{'y'}, tool.MaxBytes+1)) // starts the writer, which holds
			st.await(t, "the write")
			_, _ = o.Write(chunk)
			if n := backlog(o.spill); n != len(chunk) {
				t.Fatalf("control: the backlog holds %d bytes behind the held write, not the chunk", n)
			}
			_, _ = o.Write(bytes.Repeat([]byte{'z'}, spillBacklog)) // more than the backlog takes
			if !o.spillDone {
				t.Fatal("the writer was not given up")
			}
			return o.spill, st.free
		}},
		{"a file that cannot be opened", func(t *testing.T) (*spiller, func()) {
			st := newStall(t)
			s := startSpiller(func(string, string) (spillFile, error) { st.hold(); return nil, fmt.Errorf("no") }, "", "")
			st.await(t, "the open")
			s.send(chunk)
			if n := backlog(s); n != len(chunk) {
				t.Fatalf("control: the backlog holds %d bytes behind the held open, not the chunk", n)
			}
			st.free()
			<-s.done
			return s, st.free
		}},
		{"a write that fails", func(t *testing.T) (*spiller, func()) {
			s, st := primed(t, fmt.Errorf("disk full"))
			st.free()
			<-s.done
			return s, st.free
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, free := tc.run(t)
			s.mu.Lock()
			n, abandoned := len(s.backlog), s.abandoned
			s.mu.Unlock()
			if n != 0 || !abandoned {
				t.Fatalf("after the abandonment the backlog holds %d bytes, abandoned %v", n, abandoned)
			}
			if s.send(chunk) {
				t.Fatal("an abandoned writer took more output")
			}
			free()
			select {
			case <-s.done:
			case <-time.After(5 * time.Second):
				t.Fatal("the freed writer did not stop")
			}
			if s.path != "" {
				t.Fatalf("an abandoned file was offered: %q", s.path)
			}
		})
	}
	t.Run("control: a writer that keeps up", func(t *testing.T) {
		t.Parallel()
		f, open := slowSpill(t, 0)
		s := startSpiller(open, "", "")
		s.send(chunk)
		s.end()
		if got := s.wait(5*time.Second, nil); got != f.name || len(f.content()) != len(chunk) {
			t.Fatalf("wait = %q with %d bytes written; want the whole file", got, len(f.content()))
		}
	})
}

// heldFile is a real spill file whose first write holds on a stall.
type heldFile struct {
	*os.File
	hold   *stall
	closed chan struct{}
}

func (h *heldFile) Write(p []byte) (int, error) { h.hold.hold(); return h.File.Write(p) }
func (h *heldFile) Close() error {
	err := h.File.Close()
	close(h.closed)
	return err
}

// TestBashAbandonedSpillIsLeftForSweep (AC5): a spill file given up because
// its write stalled is left where it is, unnamed in the result, for
// tool.Sweep — never removed by name. So when another file has taken its name
// by the time the writer comes back (the directory is the model's to reach
// through bash), that file is untouched. The control is that the write did
// stall the file half-written: it is there, and the result does not name it.
func TestBashAbandonedSpillIsLeftForSweep(t *testing.T) {
	t.Parallel()
	env := bashEnv(t, nil)
	c := prepareBash(t, env, map[string]any{"command": "seq 1 20000"})
	s := newStall(t)
	opened := make(chan *heldFile, 1)
	c.ops.openSpill = func(home, id string) (spillFile, error) {
		f, err := tool.OpenSpill(home, id)
		if err != nil {
			return nil, err
		}
		h := &heldFile{File: f, hold: s, closed: make(chan struct{})}
		opened <- h
		return h, nil
	}
	res := startBash(t, c, env).await(t, 30*time.Second)
	if res.IsError || res.Trunc.Spill != "" || !strings.HasPrefix(res.Text, notSaved) {
		t.Fatalf("result = {IsError:%v Spill:%q Text:%.80q}, want no file offered", res.IsError, res.Trunc.Spill, res.Text)
	}
	var h *heldFile
	select {
	case h = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the spill file was never opened")
	}
	path := h.Name()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the half-written spill file is gone: %v", err)
	}
	// Another file takes the name; the writer comes back, finishes its
	// write, and closes.
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	put(t, path, "planted")
	s.free()
	select {
	case <-h.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the freed writer never closed its file")
	}
	if got := load(t, path); got != "planted" {
		t.Fatalf("the file that took the spill file's name was touched: %q", got)
	}
	if _, err := os.Stat(path + ".moved"); err != nil {
		t.Fatalf("the abandoned file was removed: %v", err)
	}
}

// TestBashSlowSpillLosesNothing: a spill file that is merely slow never
// slows the reading of the pipe, so no byte the command wrote is lost; the
// file is kept when it is finished within the spill wait, and is said not
// to be saved when it is not — never a file that holds less than it says.
func TestBashSlowSpillLosesNothing(t *testing.T) {
	t.Parallel()
	full := seqOutput(1, 200000)
	for _, tc := range []struct {
		name  string
		delay time.Duration
		saved bool
	}{
		{"slow", time.Millisecond, true},
		{"slower than the spill wait", 3 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := bashEnv(t, nil)
			c := prepareBash(t, env, map[string]any{"command": "seq 1 200000"})
			f, open := slowSpill(t, tc.delay)
			c.ops.openSpill = open
			res := startBash(t, c, env).await(t, 30*time.Second)
			tail := seqOutput(198002, 200000)
			if res.IsError || !strings.HasSuffix(res.Text, "\n\n"+tail) || res.Trunc.TotalBytes != len(full) || strings.Contains(res.Text, "<shell_metadata>") {
				t.Fatalf("result = {IsError:%v Text:%.160q... Trunc:%+v}, want every byte read and the tail", res.IsError, res.Text, res.Trunc)
			}
			switch {
			case tc.saved && (res.Trunc.Spill != f.name || f.content() != full):
				t.Fatalf("the slow file was not kept whole: Spill %q, %d of %d bytes", res.Trunc.Spill, len(f.content()), len(full))
			case !tc.saved && (res.Trunc.Spill != "" || !strings.HasPrefix(res.Text, notSaved)):
				t.Fatalf("an unfinished file was offered: Spill %q, text %.100q", res.Trunc.Spill, res.Text)
			}
		})
	}
}

// TestBashSpillCap: the spill file stops at the cap — its first cap bytes —
// while the output is still read to its end and its tail kept, and the
// notice says where the saved output stops. Under the cap is the control:
// the whole output, and opencode's notice.
func TestBashSpillCap(t *testing.T) {
	t.Parallel()
	if maxSpillBytes != 100<<20 || size(maxSpillBytes) != "100 MiB" {
		t.Fatalf("the cap is %d (%s), want 100 MiB", int64(maxSpillBytes), size(maxSpillBytes))
	}
	full := seqOutput(1, 20000)
	for _, tc := range []struct {
		name  string
		cap   int64
		saved string
	}{
		{"under the cap", 1 << 20, full},
		{"past the cap", 60000, full[:60000]},
		{"a cap below what the model is shown", 1000, full[:1000]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := bashEnv(t, nil)
			c := prepareBash(t, env, map[string]any{"command": "seq 1 20000"})
			c.spillCap = tc.cap
			res := startBash(t, c, env).await(t, 30*time.Second)
			notice := "Full output saved to: " + res.Trunc.Spill
			if tc.saved != full {
				notice = fmt.Sprintf("Output saved to: %s\nThe saved output stops at %d bytes; the rest was not saved.", res.Trunc.Spill, tc.cap)
			}
			if want := "...output truncated...\n\n" + notice + "\n\n" + seqOutput(18002, 20000); res.IsError || res.Text != want {
				t.Fatalf("text = %.200q..., want %.200q...", res.Text, want)
			}
			if res.Trunc.Spill == "" || load(t, res.Trunc.Spill) != tc.saved || res.Trunc.TotalBytes != len(full) {
				t.Fatalf("the spill file does not hold the first %d bytes, or the output was not read to its end: %+v", len(tc.saved), res.Trunc)
			}
		})
	}
}

// escapeeHelper names, in TestBashEscapeeHelper's environment, the file it
// makes once it has left the command's session.
const escapeeHelper = "CRAZE_BASH_ESCAPEE"

// TestBashEscapeeHelper is a command's background child that escapes: it
// starts a session of its own and writes to the command's output until the
// pipe closes — SIGPIPE ends it — or 30 s pass.
func TestBashEscapeeHelper(t *testing.T) {
	file := os.Getenv(escapeeHelper)
	if file == "" {
		t.Skip("run by TestBashOutputHeldOpen, as a command's background child")
	}
	if _, err := syscall.Setsid(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if _, err := fmt.Println("escapee"); err != nil {
			return
		}
	}
}

// TestBashOutputHeldOpen: a child that left the command's process group
// survives the group's SIGKILL and keeps the output open; the reading stops
// at its bound, Run returns, and the result says the output may be
// incomplete, never dropping it silently. Every other test's exact text is
// the control: output read to its end carries no such line. When such an
// output is also long enough to spill (AC6), the file holds what was read
// and the notice says just that, never "Full output saved": TestBashTruncation
// is the control, where the output ended and the notice says "Full".
func TestBashOutputHeldOpen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, prefix string
		spilled      bool
	}{
		{"short", "", false},
		{"spilled", "seq 1 20000; ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := bashEnv(t, nil)
			const marker = "TestBashEscapeeHelper"
			escaped := filepath.Join(env.Workspace, "escaped")
			command := fmt.Sprintf("%s%s=%s '%s' -test.run='^%s$' & echo $! > pid; i=0; while [ ! -e escaped ] && [ $i -lt 400 ]; do sleep 0.05; i=$((i+1)); done",
				tc.prefix, escapeeHelper, escaped, os.Args[0], marker)
			r := startBash(t, prepareBash(t, env, map[string]any{"command": command}), env)
			bashPID(t, filepath.Join(env.Workspace, "pid"), marker)
			if !waitFor(20*time.Second, func() bool { _, err := os.Stat(escaped); return err == nil }) {
				t.Fatal("control: the child never left the command's session")
			}
			began := time.Now()
			res := r.await(t, 30*time.Second)
			if res.IsError || !strings.Contains(res.Text, "escapee\n") || !strings.HasSuffix(res.Text, meta(partialText)) {
				t.Fatalf("result = {IsError:%v Text:...%q}, want the output so far and the note", res.IsError, res.Text[max(len(res.Text)-300, 0):])
			}
			// The 2 s drain wait, then at most the 0.7 s read of the pipe.
			if took := time.Since(began); took > planDrain+time.Second+3*time.Second {
				t.Fatalf("Run took %v after the leader exited", took)
			}
			if strings.Contains(res.Text, "Full output saved") {
				t.Fatalf("an output not read to its end was called full: %.200q", res.Text)
			}
			if !tc.spilled {
				return
			}
			want := "...output truncated...\n\nOutput saved to: " + res.Trunc.Spill + "\nThe saved output holds only what was read of the output.\n\n"
			if res.Trunc.Spill == "" || !strings.HasPrefix(res.Text, want) {
				t.Fatalf("text = %.200q..., want %q", res.Text, want)
			}
			if spill := load(t, res.Trunc.Spill); !strings.HasPrefix(spill, seqOutput(1, 20000)+"escapee\n") || !strings.HasSuffix(spill, "escapee\n") {
				t.Fatalf("the spill file does not hold what was read: %.60q...%q", spill, spill[max(len(spill)-40, 0):])
			}
		})
	}
}

// TestStopped pins launch's recheck of the stop conditions: a cancel, a close
// and a passed deadline each stop the call, a cancel or a close before the
// deadline, and a deadline still within its slack does not.
func TestStopped(t *testing.T) {
	t.Parallel()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	closed := make(chan struct{})
	close(closed)
	past, future := time.Now().Add(-time.Second), time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		closing  <-chan struct{}
		deadline time.Time
		want     error
	}{
		{"nothing", context.Background(), nil, future, nil},
		{"a cancel", cancelled, nil, future, context.Canceled},
		{"a close", context.Background(), closed, future, context.Canceled},
		{"the deadline passed", context.Background(), nil, past, errLaunchTimeout},
		{"the deadline just passed, within its slack", context.Background(), nil, time.Now(), nil},
		{"a cancel and the deadline passed", cancelled, nil, past, context.Canceled},
		{"a close and the deadline passed", context.Background(), closed, past, context.Canceled},
	} {
		if got := stopped(tc.ctx, tc.closing, tc.deadline); !errors.Is(got, tc.want) {
			t.Errorf("%s: stopped = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestBashStopAsTheStartCompletes: a stop that fires as the start completes
// — inside the start itself, so the started command and the stop are ready
// for launch together — is still a stop: the result is the bare abort (or
// the timeout), nothing the command wrote or would have written, and the
// command is killed at once. Whichever of the two ready channels select
// takes, the outcome is this one; a launch that took the start over the stop
// would instead supervise the command, and the result would carry the
// command's output and metadata. The deadline case has the deadline pass
// during the start: the start's result is ready only after the stop.
func TestBashStopAsTheStartCompletes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, marker string
	}{
		{"a cancel", "sleep 644"},
		{"a close", "sleep 645"},
		{"the deadline", "sleep 646"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, closeSession := withClosing(bashEnv(t, nil))
			in := map[string]any{"command": tc.marker}
			if tc.name == "the deadline" {
				in["timeout"] = 200
			}
			c := prepareBash(t, env, in)
			pids := startedPIDs(c)
			var r *bashRun
			ready := make(chan struct{})
			start := c.ops.start
			c.ops.start = func(cmd *exec.Cmd) (*group, error) {
				g, err := start(cmd)
				<-ready
				switch tc.name {
				case "a cancel":
					r.cancel(nil)
				case "a close":
					closeSession()
				case "the deadline":
					time.Sleep(400 * time.Millisecond)
				}
				return g, err
			}
			began := time.Now()
			r = startBash(t, c, env)
			close(ready)
			res := r.await(t, 30*time.Second)
			took := time.Since(began)
			if tc.name == "the deadline" {
				failed(t, res, tool.ClassTimeout, "(no output)"+meta(timeoutText(200)))
			} else {
				failed(t, res, tool.ClassAborted, tool.AbortedText)
			}
			if took > 1500*time.Millisecond {
				t.Fatalf("Run took %v; want the stop acted on at once", took)
			}
			gone(t, <-pids, tc.marker)
		})
	}
}

// TestBashSavedNotice pins the notice over a cut output for every state of
// its spill file: whole, capped, partial, both, and none. "Full" is said
// only of a whole file, and each shortfall is stated when a file has both.
func TestBashSavedNotice(t *testing.T) {
	t.Parallel()
	c := prepareBash(t, bashEnv(t, nil), map[string]any{"command": "true"})
	c.spillCap = 1000
	const (
		path    = "/home/x/tool-output/tool_t1.1.1"
		capped  = "\nThe saved output stops at 1000 bytes; the rest was not saved."
		partial = "\nThe saved output holds only what was read of the output."
	)
	for _, tc := range []struct {
		name            string
		spill           string
		capped, partial bool
		want            string
	}{
		{"whole", path, false, false, "Full output saved to: " + path},
		{"capped", path, true, false, "Output saved to: " + path + capped},
		{"partial", path, false, true, "Output saved to: " + path + partial},
		{"capped and partial", path, true, true, "Output saved to: " + path + capped + partial},
		{"no file", "", false, false, "The full output could not be saved."},
		{"no file, partial", "", false, true, "The full output could not be saved."},
	} {
		res := c.result(outcome{kept: "tail", cut: true, trunc: tool.Truncation{Spill: tc.spill}, capped: tc.capped, partial: tc.partial})
		want := "...output truncated...\n\n" + tc.want + "\n\ntail"
		if tc.partial {
			want += meta(partialText)
		}
		if res.Text != want {
			t.Errorf("%s: text = %q, want %q", tc.name, res.Text, want)
		}
	}
}

// TestBashAbandonedStartsLeakOneEach: a start that never returns, once its
// call is abandoned, holds one goroutine and one descriptor (the pipe's write
// end, which Start may be using), no more: the read end is closed and no
// second goroutine waits on the first. Twenty such calls grow the process by
// at most twenty of each. Not parallel: it counts the process's goroutines
// and descriptors.
func TestBashAbandonedStartsLeakOneEach(t *testing.T) {
	const calls = 20
	fds := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return -1 // not Linux: descriptors are not counted
		}
		return len(entries)
	}
	runtime.GC()
	goroutines, descriptors := runtime.NumGoroutine(), fds()
	var stalls []*stall
	var pids []<-chan int
	for i := 0; i < calls; i++ {
		env := bashEnv(t, nil)
		c := prepareBash(t, env, map[string]any{"command": "sleep 647"})
		s := stallOp(t, c, "exec start")
		stalls, pids = append(stalls, s), append(pids, startedPIDs(c))
		r := startBash(t, c, env)
		s.await(t, "the start")
		r.cancel(nil)
		failed(t, r.await(t, 30*time.Second), tool.ClassAborted, tool.AbortedText)
	}
	// The calls' own goroutines have returned; startBash's may still be
	// finishing, so a little slack.
	if grew := runtime.NumGoroutine() - goroutines; grew > calls+3 {
		t.Errorf("%d abandoned starts left %d more goroutines; want at most one each", calls, grew)
	}
	if descriptors >= 0 {
		if grew := fds() - descriptors; grew > calls+2 {
			t.Errorf("%d abandoned starts left %d more descriptors; want at most one each", calls, grew)
		}
	}
	// Freed, each start starts its command, and the goroutine that ran it
	// discards it.
	for _, s := range stalls {
		s.free()
	}
	for _, pid := range pids {
		select {
		case pid := <-pid:
			gone(t, pid, "sleep 647")
		case <-time.After(15 * time.Second):
			t.Fatal("a freed start never started its command")
		}
	}
}

// TestSuperviseShutdownHasOneDeadline: the shutdown after a cancel or a
// close has one deadline, set when it starts — 5 s after a cancel's SIGTERM,
// 2 s after a close's SIGKILL — that a leader dying late does not extend: the
// drain wait that follows its exit ends at the deadline, not 2 s later. The
// leader here is a stub whose exit the test announces itself, late in the
// shutdown, while the output never closes; the signals go to a real
// TERM-ignoring shell in a session of its own, which SIGKILL ends. Were the
// drain to restart the clock, each case would take 1.5 s longer than its
// bound.
func TestSuperviseShutdownHasOneDeadline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		closeAt     time.Duration // 0: an ordinary cancel; else a close through Env.Closing this long after it
		exitAt      time.Duration // when the stub leader exits
		want, bound time.Duration // the deadline, and the most Run may take
	}{
		{"a cancel, the leader dying just before the deadline", 0, 4500 * time.Millisecond, 5 * time.Second, 5800 * time.Millisecond},
		{"a close, the leader dying just before the deadline", -1, 1500 * time.Millisecond, 2 * time.Second, 2800 * time.Millisecond},
		{"a close late in a cancel's grace", 4500 * time.Millisecond, 4600 * time.Millisecond, 5 * time.Second, 5800 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(shPath, "-c", "trap '' TERM; sleep 648")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = cmd.Wait() // the zombie held the pid until now
			})
			g := &group{cmd: cmd, pid: cmd.Process.Pid, exited: make(chan struct{}), release: make(chan struct{}), reaped: make(chan struct{})}
			ctx, cancel := context.WithCancelCause(context.Background())
			closing := make(chan struct{})
			switch {
			case tc.closeAt < 0:
				cancel(tool.ErrClosing)
			case tc.closeAt > 0:
				cancel(nil)
				time.AfterFunc(tc.closeAt, func() { close(closing) })
			default:
				cancel(nil)
			}
			time.AfterFunc(tc.exitAt, func() {
				close(g.exited)
				close(g.reaped)
			})
			began := time.Now()
			why, _ := g.supervise(ctx, closing, time.Hour, make(chan struct{}))
			took := time.Since(began)
			if why != endAbort {
				t.Fatalf("supervise ended with %v, want the abort", why)
			}
			if took < tc.exitAt || took > tc.bound {
				t.Fatalf("supervise returned after %v; want the leader's exit at %v then the deadline at %v, never a fresh drain wait", took, tc.exitAt, tc.want)
			}
		})
	}
}

// TestSetupStopsBeforeTheStart: a stop that has fired by the time the work
// before the command is done — a cancel, a close, the deadline — starts
// nothing: setup returns the stop's error and never reaches the exec. This is
// the check that keeps a stop from being taken for one that came after the
// command had started, an order launch's select alone cannot tell. The
// control, with no stop, starts the command.
func TestSetupStopsBeforeTheStart(t *testing.T) {
	t.Parallel()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	closed := make(chan struct{})
	close(closed)
	future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		closing  <-chan struct{}
		deadline time.Time
		want     error
	}{
		{"a cancel", cancelled, nil, future, context.Canceled},
		{"a close", context.Background(), closed, future, context.Canceled},
		{"the deadline", context.Background(), nil, past, errLaunchTimeout},
		{"control: no stop", context.Background(), nil, future, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := bashEnv(t, nil)
			env.Closing = tc.closing
			c := prepareBash(t, env, map[string]any{"command": "sleep 649"})
			pids := startedPIDs(c)
			var r readEnd
			g, f, err := c.setup(tc.ctx, env, tc.deadline, &r)
			if !errors.Is(err, tc.want) {
				t.Fatalf("setup = %v, want %v", err, tc.want)
			}
			if tc.want != nil {
				if g != nil || f != nil || len(pids) != 0 {
					t.Fatal("setup started the command despite the stop")
				}
				return
			}
			launched{g: g, r: f}.discard()
			gone(t, <-pids, "sleep 649")
		})
	}
}

// TestBashTmpDir: the directory bash offers for temporary work is the fixed
// one under the machine's temporary directory when it can be made or is
// safely ours; when something else holds that name — a symlink, a directory
// of ours with the wrong mode, a file: what another user of the machine
// could plant — a directory with a random name is made for the session
// instead, and the plant is left as it was. Before every command the
// directory is checked again: one replaced since is refused, and the command
// does not run; one merely removed is remade. The control is nothing
// planted. A directory owned by another user cannot be planted by a test
// without privileges; the check on it is the same check.
func TestBashTmpDir(t *testing.T) {
	t.Parallel()
	fallback := func(t *testing.T, base, got string) {
		t.Helper()
		if got == filepath.Join(base, "craze") || filepath.Dir(got) != base || !strings.HasPrefix(filepath.Base(got), "craze-") {
			t.Fatalf("tmpDir = %q; want a session's own directory under %s", got, base)
		}
		if err := ensureTmp(got); err != nil {
			t.Fatalf("the fallback does not pass the check: %v", err)
		}
	}
	t.Run("control: nothing planted", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		got, err := tmpDir(base)
		if err != nil || got != filepath.Join(base, "craze") || perm(t, got) != 0o700 {
			t.Fatalf("tmpDir = %q, %v; want %s, mode 0700", got, err, filepath.Join(base, "craze"))
		}
		if again, err := tmpDir(base); err != nil || again != got {
			t.Fatalf("a second session got %q, %v; want the same directory, already ours", again, err)
		}
	})
	t.Run("a planted symlink", func(t *testing.T) {
		t.Parallel()
		base, elsewhere := t.TempDir(), t.TempDir()
		fixed := filepath.Join(base, "craze")
		if err := os.Symlink(elsewhere, fixed); err != nil {
			t.Fatal(err)
		}
		got, err := tmpDir(base)
		if err != nil {
			t.Fatal(err)
		}
		fallback(t, base, got)
		if info, err := os.Lstat(fixed); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the planted symlink was touched: %v, %v", info, err)
		}
	})
	t.Run("a planted directory with the wrong mode", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		fixed := filepath.Join(base, "craze")
		if err := os.Mkdir(fixed, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(fixed, 0o755); err != nil { // whatever the umask
			t.Fatal(err)
		}
		got, err := tmpDir(base)
		if err != nil {
			t.Fatal(err)
		}
		fallback(t, base, got)
		if perm(t, fixed) != 0o755 {
			t.Fatal("the planted directory's mode was changed")
		}
	})
	t.Run("a planted file", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		put(t, filepath.Join(base, "craze"), "")
		got, err := tmpDir(base)
		if err != nil {
			t.Fatal(err)
		}
		fallback(t, base, got)
	})
	t.Run("an unusable base", func(t *testing.T) {
		t.Parallel()
		if got, err := tmpDir(filepath.Join(t.TempDir(), "missing", "base")); err == nil {
			t.Fatalf("tmpDir = %q under a base that does not exist; want an error", got)
		}
	})
	t.Run("checked before every command", func(t *testing.T) {
		t.Parallel()
		env := bashEnv(t, nil)
		tmp := filepath.Join(env.Home, "tmp") // prepareBash's
		if text := ok(t, runBash(t, env, map[string]any{"command": "echo ran"})); text != "ran\n" || perm(t, tmp) != 0o700 {
			t.Fatalf("control: the first command did not run, or left %s with mode %v", tmp, perm(t, tmp))
		}
		if err := os.Remove(tmp); err != nil {
			t.Fatal(err)
		}
		if text := ok(t, runBash(t, env, map[string]any{"command": "echo ran"})); text != "ran\n" || perm(t, tmp) != 0o700 {
			t.Fatal("a removed directory was not remade for the next command")
		}
		if err := os.Remove(tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), tmp); err != nil {
			t.Fatal(err)
		}
		res := runBash(t, env, map[string]any{"command": ": > ran"})
		failed(t, res, tool.ClassToolError, "The temporary directory cannot be used: "+tmp+" is a symlink")
		if _, err := os.Stat(filepath.Join(env.Workspace, "ran")); err == nil {
			t.Fatal("the command ran with the directory replaced")
		}
	})
}
