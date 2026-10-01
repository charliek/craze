package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// Background jobs, bash's side (plan 033 §3.7–§3.8). The session's jobs are
// the harness's; here a fake stands in for them (fakeJobs), recording what
// bash reserves, starts and gives back, so these tests see the hand-over
// itself: what the call returns, what the job is handed, and that the job —
// not the call — then owns the command, its output and its end. Every race
// at the timeout is forced at its instant through the expiring seam
// (promotion.expiring), so the race a test names is the one that happens.

// fakeJobs is a session's jobs as bash sees them (tool.Jobs).
type fakeJobs struct {
	refuse error            // Reserve's refusal, when set
	red    *redact.Replacer // Redact's, nil: none

	mu       sync.Mutex
	reserved []string
	released []string
	specs    []tool.JobSpec
	bodies   []tool.JobBody
	started  chan tool.JobBody // each started body, as it is handed over
}

func newFakeJobs() *fakeJobs { return &fakeJobs{started: make(chan tool.JobBody, 4)} }

func (f *fakeJobs) Reserve(id string) (tool.JobSlot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserved = append(f.reserved, id)
	if f.refuse != nil {
		return nil, f.refuse
	}
	return &fakeSlot{f: f, id: id}, nil
}

func (f *fakeJobs) Output(context.Context, tool.JobOutputCall) tool.Result { return tool.Result{} }
func (f *fakeJobs) Stop(context.Context, tool.JobStopCall) tool.Result     { return tool.Result{} }

func (f *fakeJobs) Redact(text string) string {
	if f.red == nil {
		return text
	}
	return f.red.String(text)
}

func (f *fakeJobs) reservations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reserved)
}

func (f *fakeJobs) releases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.released)
}

func (f *fakeJobs) spec(t *testing.T) tool.JobSpec {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.specs) != 1 {
		t.Fatalf("%d jobs started; want one", len(f.specs))
	}
	return f.specs[0]
}

// fakeSlot is one reserved slot: Start or Release, once — the contract.
type fakeSlot struct {
	f    *fakeJobs
	id   string
	used bool
}

func (s *fakeSlot) Start(spec tool.JobSpec, body tool.JobBody) {
	s.f.mu.Lock()
	if s.used {
		s.f.mu.Unlock()
		panic("fakeSlot: used twice")
	}
	s.used = true
	s.f.specs = append(s.f.specs, spec)
	s.f.bodies = append(s.f.bodies, body)
	s.f.mu.Unlock()
	s.f.started <- body
}

func (s *fakeSlot) Release() {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.used {
		return // released after Start: nothing, as the contract has it
	}
	s.used = true
	s.f.released = append(s.f.released, s.id)
}

// jobEnv is bashEnv with jobs.
func jobEnv(t *testing.T, jobs tool.Jobs) tool.Env {
	t.Helper()
	env := bashEnv(t, nil)
	env.Jobs = jobs
	return env
}

// ownedBody is a started job's body the test now owns as the harness would:
// waited exactly once — by the test, or else at cleanup with the session's
// close, so nothing outlives the test.
type ownedBody struct {
	tool.JobBody
	once sync.Once
	end  tool.JobEnd
}

// adopt takes the body the call handed over, and waits it at cleanup unless
// the test has.
func adopt(t *testing.T, f *fakeJobs) *ownedBody {
	t.Helper()
	b := &ownedBody{JobBody: await(t, f.started, "the job's hand-over")}
	t.Cleanup(func() {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(tool.ErrClosing)
		b.wait(ctx, time.Hour)
	})
	return b
}

// wait is the body's Wait, once; a second call returns the first's end.
func (b *ownedBody) wait(ctx context.Context, limit time.Duration) tool.JobEnd {
	b.once.Do(func() { b.end = b.Wait(ctx, limit, func(string) {}) })
	return b.end
}

// await receives from ch within 30 s.
func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// fifo makes a named pipe in the workspace: a command that reads it waits
// until the test writes a line to it (release).
func fifo(t *testing.T, env tool.Env, name string) string {
	t.Helper()
	path := filepath.Join(env.Workspace, name)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// release writes a line to the named pipe path, waiting for its reader.
func release(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("x\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// groups wraps c's start so the test holds the group it starts.
func groups(c *bashCall) <-chan *group {
	gs := make(chan *group, 1)
	start := c.ops.start
	c.ops.start = func(cmd *exec.Cmd) (*group, error) {
		g, err := start(cmd)
		if err == nil {
			gs <- g
		}
		return g, err
	}
	return gs
}

// promotionText is §3.7's promotion receipt for job id, after the output so
// far, spelled out.
func promotionText(out string, timeoutMs int, id, path string) string {
	return out + "\n\n<shell_metadata>\n" +
		fmt.Sprintf("The command did not finish within its timeout of %d ms. It was not stopped: it was moved to the background as "+
			"job `%s` and is still running, for at most 30 more minutes. Its output so far is above; all of it is saved to: "+
			"%s. Its result is delivered to you when it finishes; do not poll it or sleep waiting for it. "+
			"Call bash_output with its id to read newer output, or bash_stop to stop it.", timeoutMs, id, path) +
		"\n</shell_metadata>\n" + `<background_job id="` + id + `"/>`
}

// startText is §3.7's start receipt for job id, spelled out.
func startText(id, limit, path string) string {
	return "Started the command in the background as job `" + id + "`. It runs until it exits, until you stop it with bash_stop, " +
		"or for at most " + limit + "; the session closing stops it too. Its output is saved to: " + path + "\n" +
		"Its result is delivered to you when it finishes; do not poll it or sleep waiting for it. " +
		"Call bash_output with its id to read its output so far.\n" + `<background_job id="` + id + `"/>`
}

// spillOf is the spill file of call c under env's home.
func spillOf(env tool.Env, c *bashCall) string {
	return filepath.Join(env.Home, tool.SpillDir, "tool_"+c.id)
}

// TestBashPromotion (A11): in a session that runs jobs, a command still
// running at its timeout is moved to the background, not killed. The call
// returns §3.7's promotion receipt — the output so far, the metadata, the
// marker — and hands the job the command whole: its limit 30 minutes from the
// promotion, its read cursor after the output the receipt showed, its spill
// file holding the output from the first byte. The command goes on after the
// call has returned — what it writes then reaches the job's output, not a
// closed pipe — and the job's Wait, not the call, ends it and reads its exit.
// The negative control is the same command with no jobs: the timeout kills
// it, as it always has (TestBashTimeout).
func TestBashPromotion(t *testing.T) {
	t.Parallel()
	jobs := newFakeJobs()
	env := jobEnv(t, jobs)
	gate := fifo(t, env, "gate")
	c := prepareBash(t, env, map[string]any{"command": "echo before; read line < gate; echo after $line", "timeout": 300})
	res := startBash(t, c, env).await(t, 30*time.Second)
	body := adopt(t, jobs)
	path := spillOf(env, c)
	if res.IsError || res.Text != promotionText("before\n", 300, c.id, path) {
		t.Fatalf("result = %+v\nwant the promotion receipt:\n%s", res, promotionText("before\n", 300, c.id, path))
	}
	spec := jobs.spec(t)
	if spec.ID != c.id || spec.Command != c.command || spec.Workdir != env.Workspace || spec.Limit != 30*time.Minute ||
		!spec.Promoted || spec.Seen != int64(len("before\n")) || spec.Began.IsZero() {
		t.Fatalf("the job's spec = %+v; want the call's command, 30 minutes from now, promoted, its cursor past the receipt's output", spec)
	}
	if got := jobs.reservations(); !slices.Equal(got, []string{c.id}) || len(jobs.releases()) != 0 {
		t.Fatalf("reservations %v, releases %v; want one slot, kept", got, jobs.releases())
	}

	// The command runs on: the call has returned, and its pipe is the job's.
	release(t, gate)
	end := body.wait(context.Background(), time.Hour)
	if end.Status != tool.JobExited || end.ExitCode != 0 || end.Text != "before\nafter x\n" {
		t.Fatalf("the job ended %+v; want exited 0 with the whole output", end)
	}
	if out := body.Output(spec.Seen); out.Text != "after x\n" || out.Skipped != 0 || out.Total != int64(len("before\nafter x\n")) {
		t.Fatalf("the output after the receipt's = %+v; want what came after the promotion", out)
	}
	if got := load(t, path); got != "before\nafter x\n" {
		t.Fatalf("the spill file holds %q; want the whole output, from its first byte", got)
	}
}

// TestBashPromotionRaces (A11, §3.8's race tests): each thing that can meet
// the timeout at its instant, forced there. A cancelled call (Esc) and a
// closing session are never promoted — no slot is asked for, and the command
// is killed as a timeout kills it — and a leader that exits at the instant is
// an exit, read as one. The cap's refusal (A11b, P26) kills as before, its
// text in the metadata, and so does a close that lands between the timeout
// and the slot. The control for all of them is TestBashPromotion: the same
// instant with nothing landing promotes.
func TestBashPromotionRaces(t *testing.T) {
	t.Parallel()
	timeoutLine := "shell tool terminated command after exceeding timeout 300 ms. If this command is expected to take longer and is not waiting for interactive input, retry with a larger timeout value in milliseconds."

	t.Run("a cancel at the timeout", func(t *testing.T) {
		t.Parallel()
		jobs := newFakeJobs()
		env := jobEnv(t, jobs)
		c := prepareBash(t, env, map[string]any{"command": "echo before; sleep 621", "timeout": 300})
		pids := startedPIDs(c)
		runs := make(chan *bashRun, 1)
		c.ops.expiring = func() { (<-runs).cancel(nil) }
		r := startBash(t, c, env)
		runs <- r
		pid := <-pids
		res := r.await(t, 30*time.Second)
		failed(t, res, tool.ClassTimeout, "before\n"+meta(timeoutLine))
		if got := jobs.reservations(); len(got) != 0 {
			t.Fatalf("a cancelled call asked for a slot: %v", got)
		}
		gone(t, pid, "sleep 621")
	})

	t.Run("a close at the timeout", func(t *testing.T) {
		t.Parallel()
		jobs := newFakeJobs()
		env, closeSession := withClosing(jobEnv(t, jobs))
		c := prepareBash(t, env, map[string]any{"command": "echo before; sleep 622", "timeout": 300})
		pids := startedPIDs(c)
		c.ops.expiring = closeSession
		r := startBash(t, c, env)
		pid := <-pids
		res := r.await(t, 30*time.Second)
		failed(t, res, tool.ClassTimeout, "before\n"+meta(timeoutLine))
		if got := jobs.reservations(); len(got) != 0 {
			t.Fatalf("a closing session's call asked for a slot: %v", got)
		}
		gone(t, pid, "sleep 622")
	})

	t.Run("the leader exiting at the timeout", func(t *testing.T) {
		t.Parallel()
		jobs := newFakeJobs()
		env := jobEnv(t, jobs)
		gate := fifo(t, env, "gate")
		c := prepareBash(t, env, map[string]any{"command": "echo before; read line < gate; echo done", "timeout": 300})
		gs := groups(c)
		c.ops.expiring = func() {
			g := <-gs
			release(t, gate)
			<-g.exited // the leader has exited by the time the timeout is read
		}
		res := startBash(t, c, env).await(t, 30*time.Second)
		if res.IsError || res.Text != "before\ndone\n" || res.Output.ExitCode != 0 {
			t.Fatalf("result = %+v; want the command's exit, read as one", res)
		}
		if got := jobs.reservations(); len(got) != 0 {
			t.Fatalf("an exited command asked for a slot: %v", got)
		}
	})

	t.Run("the cap refuses", func(t *testing.T) {
		t.Parallel()
		jobs := newFakeJobs()
		jobs.refuse = tool.JobsFull{Max: 8}
		env := jobEnv(t, jobs)
		c := prepareBash(t, env, map[string]any{"command": "echo before; sleep 623", "timeout": 300})
		pids := startedPIDs(c)
		r := startBash(t, c, env)
		pid := <-pids
		res := r.await(t, 30*time.Second)
		failed(t, res, tool.ClassTimeout, "before\n"+meta(timeoutLine, "8 background jobs are already running; stop one with bash_stop first."))
		if got := jobs.reservations(); !slices.Equal(got, []string{c.id}) {
			t.Fatalf("reservations %v; want the one refused", got)
		}
		gone(t, pid, "sleep 623")
	})

	t.Run("a close refuses the slot", func(t *testing.T) {
		t.Parallel()
		jobs := newFakeJobs()
		jobs.refuse = fmt.Errorf("harness: %w", tool.ErrClosing)
		env := jobEnv(t, jobs)
		c := prepareBash(t, env, map[string]any{"command": "echo before; sleep 624", "timeout": 300})
		pids := startedPIDs(c)
		r := startBash(t, c, env)
		pid := <-pids
		res := r.await(t, 30*time.Second)
		failed(t, res, tool.ClassTimeout, "before\n"+meta(timeoutLine, closingRefusal))
		gone(t, pid, "sleep 624")
	})
}

// TestBashRunInBackground (A12): run_in_background takes a slot before it
// starts anything, starts the command, hands it over at once and returns
// §3.7's start receipt; its timeout is the job's limit — 30 minutes when none
// is given, 2 hours at most, a longer one reduced and said so. At the cap
// (A11b) nothing is started: the refusal is the call's error. A closing
// session's refusal is aborted.
func TestBashRunInBackground(t *testing.T) {
	t.Parallel()

	t.Run("the receipt, the spec and the order", func(t *testing.T) {
		t.Parallel()
		jobs := newFakeJobs()
		env := jobEnv(t, jobs)
		gate := fifo(t, env, "gate")
		c := prepareBash(t, env, map[string]any{"command": "echo started; read line < gate; exit 3", "run_in_background": true})
		var reservedFirst bool
		start := c.ops.start
		c.ops.start = func(cmd *exec.Cmd) (*group, error) {
			reservedFirst = len(jobs.reservations()) == 1
			return start(cmd)
		}
		res := startBash(t, c, env).await(t, 30*time.Second)
		body := adopt(t, jobs)
		path := spillOf(env, c)
		if res.IsError || res.Text != startText(c.id, "30 minutes", path) {
			t.Fatalf("result = %+v\nwant the start receipt:\n%s", res, startText(c.id, "30 minutes", path))
		}
		if !reservedFirst {
			t.Fatal("the command started before its slot was reserved")
		}
		spec := jobs.spec(t)
		if spec.ID != c.id || spec.Limit != tool.JobDefaultLimit || spec.Promoted || spec.Seen != 0 {
			t.Fatalf("spec = %+v; want the default limit, not promoted, the cursor at 0", spec)
		}
		release(t, gate)
		if end := body.wait(context.Background(), spec.Limit); end.Status != tool.JobExited || end.ExitCode != 3 || end.Text != "started\n" {
			t.Fatalf("the job ended %+v; want exited 3", end)
		}
		if got := load(t, path); got != "started\n" {
			t.Fatalf("the spill file = %q; want the output, saved however short", got)
		}
	})

	t.Run("the limit, reduced and said so", func(t *testing.T) {
		t.Parallel()
		jobs := newFakeJobs()
		env := jobEnv(t, jobs)
		c := prepareBash(t, env, map[string]any{"command": "true", "run_in_background": true, "timeout": 9000000})
		res := startBash(t, c, env).await(t, 30*time.Second)
		adopt(t, jobs)
		want := strings.TrimSuffix(startText(c.id, "2 hours", spillOf(env, c)), "\n"+`<background_job id="`+c.id+`"/>`) +
			"\n\n<shell_metadata>\nThe requested timeout of 9000000 ms is above the maximum of 7200000 ms for a background job; it runs for at most 7200000 ms.\n</shell_metadata>\n" +
			`<background_job id="` + c.id + `"/>`
		if res.Text != want || jobs.spec(t).Limit != tool.JobMaxLimit {
			t.Fatalf("result = %q, limit %v\nwant %q, 2 hours", res.Text, jobs.spec(t).Limit, want)
		}
		if id, ok := tool.ParseJobMarker(res.Text); !ok || id != c.id {
			t.Fatalf("the receipt's marker reads %q, %v; want %s", id, ok, c.id)
		}
	})

	t.Run("a limit it gives", func(t *testing.T) {
		t.Parallel()
		jobs := newFakeJobs()
		env := jobEnv(t, jobs)
		c := prepareBash(t, env, map[string]any{"command": "sleep 625", "run_in_background": true, "timeout": 300})
		res := startBash(t, c, env).await(t, 30*time.Second)
		body := adopt(t, jobs)
		if res.Text != startText(c.id, "300 ms", spillOf(env, c)) {
			t.Fatalf("result = %q", res.Text)
		}
		// The limit is the job's: Wait ends it there, timed out.
		spec := jobs.spec(t)
		if spec.Limit != 300*time.Millisecond {
			t.Fatalf("the job's limit is %v; want the call's timeout, 300ms", spec.Limit)
		}
		if end := body.wait(context.Background(), spec.Limit); end.Status != tool.JobTimedOut || end.ExitCode != -1 {
			t.Fatalf("the job ended %+v; want timed out at its limit", end)
		}
	})

	t.Run("refused at the cap: nothing starts", func(t *testing.T) {
		t.Parallel()
		jobs := newFakeJobs()
		jobs.refuse = tool.JobsFull{Max: 8}
		env := jobEnv(t, jobs)
		c := prepareBash(t, env, map[string]any{"command": "touch ran", "run_in_background": true})
		pids := startedPIDs(c)
		res := startBash(t, c, env).await(t, 30*time.Second)
		failed(t, res, tool.ClassToolError, "8 background jobs are already running; stop one with bash_stop first.")
		select {
		case pid := <-pids:
			t.Fatalf("a command started (pid %d) though the slot was refused", pid)
		default:
		}
		if _, err := os.Stat(filepath.Join(env.Workspace, "ran")); err == nil {
			t.Fatal("the command ran")
		}
	})

	t.Run("refused by a close: aborted", func(t *testing.T) {
		t.Parallel()
		jobs := newFakeJobs()
		jobs.refuse = fmt.Errorf("harness: %w", tool.ErrClosing)
		env := jobEnv(t, jobs)
		res := runBash(t, env, map[string]any{"command": "touch ran", "run_in_background": true})
		failed(t, res, tool.ClassAborted, tool.AbortedText)
	})

	t.Run("a cancel while it starts: no job", func(t *testing.T) {
		t.Parallel()
		jobs := newFakeJobs()
		env := jobEnv(t, jobs)
		c := prepareBash(t, env, map[string]any{"command": "echo hi; sleep 626", "run_in_background": true})
		runs := make(chan *bashRun, 1)
		start := c.ops.start
		c.ops.start = func(cmd *exec.Cmd) (*group, error) {
			g, err := start(cmd)
			(<-runs).cancel(nil) // the command has started; the call is cancelled before it is handed over
			return g, err
		}
		r := startBash(t, c, env)
		runs <- r
		res := r.await(t, 30*time.Second)
		if res.Class != tool.ClassAborted || !strings.HasPrefix(res.Text, tool.AbortedText) {
			t.Fatalf("result = %+v; want aborted", res)
		}
		if got := jobs.releases(); !slices.Equal(got, []string{c.id}) || len(jobs.specs) != 0 {
			t.Fatalf("releases %v, started %d; want the slot given back and no job", got, len(jobs.specs))
		}
	})
}

// TestBashReceiptsRedacted (P19): both receipts go through the session's
// widest redaction as they are made (Jobs.Redact), not only the call's own
// redactor — here none, while the session knows a key that the home's path,
// and so the spill path the receipt names, holds.
func TestBashReceiptsRedacted(t *testing.T) {
	t.Parallel()
	for _, background := range []bool{true, false} {
		t.Run(fmt.Sprintf("background %v", background), func(t *testing.T) {
			t.Parallel()
			jobs := newFakeJobs()
			jobs.red = redact.New(keyA)
			env := jobEnv(t, jobs)
			env.Home = filepath.Join(t.TempDir(), keyA)
			if err := os.MkdirAll(env.Home, 0o700); err != nil {
				t.Fatal(err)
			}
			in := map[string]any{"command": "echo before; sleep 627", "timeout": 300}
			if background {
				in["run_in_background"] = true
			}
			c := prepareBash(t, env, in)
			res := startBash(t, c, env).await(t, 30*time.Second)
			adopt(t, jobs)
			if strings.Contains(res.Text, keyA) || !strings.Contains(res.Text, redact.Marker) || !strings.Contains(res.Text, "<background_job id=") {
				t.Fatalf("receipt = %q; want the key redacted by the session's redaction", res.Text)
			}
		})
	}
}

// TestBashBackgroundWithoutJobs (P11): where the session runs no jobs —
// headless, a sub-agent's — run_in_background runs the command in the
// foreground, with the foreground's timeout, and the result says so; the
// timeout kills. A call that does not ask is the control: no such line.
func TestBashBackgroundWithoutJobs(t *testing.T) {
	t.Parallel()
	env := bashEnv(t, nil)
	note := "This session does not run background jobs: run_in_background was ignored, and the command ran in the foreground with the foreground's timeout."
	if res := runBash(t, env, map[string]any{"command": "echo hi", "run_in_background": true}); res.IsError || res.Text != "hi\n"+meta(note) {
		t.Fatalf("result = %+v; want the output and the note", res)
	}
	if res := runBash(t, env, map[string]any{"command": "echo hi"}); res.Text != "hi\n" {
		t.Fatalf("control: result = %+v", res)
	}
	c := prepareBash(t, env, map[string]any{"command": "echo before; sleep 628", "run_in_background": true, "timeout": 300})
	failed(t, startBash(t, c, env).await(t, 30*time.Second), tool.ClassTimeout, "before\n"+meta(note,
		"shell tool terminated command after exceeding timeout 300 ms. If this command is expected to take longer and is not waiting for interactive input, retry with a larger timeout value in milliseconds."))
	// The foreground's clamp, not a job's: 30 minutes is reduced to 10.
	if c := prepareBash(t, env, map[string]any{"command": "true", "run_in_background": true, "timeout": 1800000}); c.timeout != maxTimeout || c.requested != 1800000 || c.background {
		t.Fatalf("timeout %v requested %d background %v; want the foreground's cap", c.timeout, c.requested, c.background)
	}
	// null is false, as the agent tool reads its own; a string is refused.
	if c := prepareBash(t, env, `{"command":"true","run_in_background":null}`); c.foreground || c.background {
		t.Fatal("run_in_background null read as true")
	}
	tl, _ := newBash()
	if _, err := tl.Prepare(env, tool.Call{ID: "t1.1.1", Tool: "bash", Input: json.RawMessage(`{"command":"true","run_in_background":"true"}`)}); err == nil ||
		!strings.Contains(err.Error(), "run_in_background") {
		t.Fatalf("Prepare of a string run_in_background = %v; want it refused", err)
	}
}

// TestJobBodyEnds (§3.8's table, bash's half): how a job's Wait reads each
// ending — a stop is a cancel of the job's context (who stopped it is the
// harness's to say), its limit a timeout, and the session's close a kill at
// once — and its body is the tail a finished command's result shows,
// without the metadata a block's attributes carry instead.
func TestJobBodyEnds(t *testing.T) {
	t.Parallel()
	start := func(t *testing.T, command string) (*ownedBody, int, *bashCall, tool.Env) {
		jobs := newFakeJobs()
		env := jobEnv(t, jobs)
		c := prepareBash(t, env, map[string]any{"command": command, "run_in_background": true})
		pids := startedPIDs(c)
		startBash(t, c, env).await(t, 30*time.Second)
		return adopt(t, jobs), <-pids, c, env
	}
	t.Run("stopped", func(t *testing.T) {
		t.Parallel()
		body, pid, _, _ := start(t, "echo up; sleep 631")
		// Stopped once its output has been read, so the end holds it.
		if !waitFor(15*time.Second, func() bool { return body.Output(0).Total > 0 }) {
			t.Fatal("control: the job's output never arrived")
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(errors.New("stopped"))
		if end := body.wait(ctx, time.Hour); end.Status != tool.JobStopped || end.ExitCode != -1 || end.Text != "up\n" {
			t.Fatalf("end = %+v; want stopped, with the output", end)
		}
		gone(t, pid, "sleep 631")
	})
	t.Run("closing", func(t *testing.T) {
		t.Parallel()
		body, pid, _, _ := start(t, "trap '' TERM; echo up; sleep 632")
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(tool.ErrClosing)
		began := time.Now()
		if end := body.wait(ctx, time.Hour); end.Status != tool.JobStopped {
			t.Fatalf("end = %+v; want stopped", end)
		}
		if took := time.Since(began); took > planGrace {
			t.Fatalf("a close took %v: it waited out the grace a TERM-ignoring command is given", took)
		}
		gone(t, pid, "sleep 632")
	})
	t.Run("a long output", func(t *testing.T) {
		t.Parallel()
		body, _, c, env := start(t, "seq 1 3000")
		end := body.wait(context.Background(), time.Hour)
		path := spillOf(env, c)
		want := "...output truncated...\n\nFull output saved to: " + path + "\n\n" + seqOutput(1002, 3000)
		if end.Status != tool.JobExited || end.Text != want {
			t.Fatalf("end = %+v\nwant the tail with the notice:\n%.200s", end, want)
		}
		if out := body.Output(0); out.Text != seqOutput(1002, 3000) || out.Skipped != int64(len(seqOutput(1, 1001))) || out.Spill != path {
			t.Fatalf("Output(0) = text %.40q… skipped %d spill %q; want the last 2000 lines, the rest in the file", out.Text, out.Skipped, out.Spill)
		}
	})
}

// TestOutputKeepsAJobsSpillFile: a spill file started on purpose — a job's,
// which its receipt names from the start — is kept and finished however short
// the output ends; one nobody asked for exists only for an output too long to
// show (the control).
func TestOutputKeepsAJobsSpillFile(t *testing.T) {
	t.Parallel()
	out := &output{home: t.TempDir(), id: "t1.1.1", open: openSpill, cap: maxSpillBytes}
	out.startSpill()
	if _, err := out.Write([]byte("short\n")); err != nil {
		t.Fatal(err)
	}
	kept, cut, _, _, spill := out.finish()
	if kept != "short\n" || cut || spill == nil {
		t.Fatalf("finish = %q, cut %v, spill %v; want the output whole, and its file", kept, cut, spill)
	}
	if path := spill.wait(30*time.Second, nil); path == "" || load(t, path) != "short\n" {
		t.Fatalf("the kept spill file is %q; want it whole", path)
	}
	plain := &output{home: t.TempDir(), id: "t1.1.2", open: openSpill, cap: maxSpillBytes}
	if _, err := plain.Write([]byte("short\n")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, spill := plain.finish(); spill != nil {
		t.Fatal("control: a short output nobody asked to save has a spill file")
	}
}
