package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

var (
	fakeOnce sync.Once
	fakeBin  string
	fakeErr  error
)

func fakeAgentPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("CRAZE_FAKE_AGENT_BIN"); p != "" {
		return p
	}
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "craze-fake-agent-")
		if err != nil {
			fakeErr = err
			return
		}
		fakeBin = filepath.Join(dir, "craze-fake-agent")
		cmd := exec.Command("go", "build", "-o", fakeBin, "github.com/charliek/craze/cmd/craze-fake-agent")
		out, err := cmd.CombinedOutput()
		if err != nil {
			fakeErr = err
			fakeBin += "\n" + string(out)
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return fakeBin
}

// newTestSession is how every test in this package builds a session, so that
// pointing HOME at an empty directory is a property of the constructor rather
// than something each test has to remember. Everything that starts a session
// reads HOME — the plugin caches live under it — so a test that skipped it
// would pass or fail on what the developer running it happens to have
// installed.
// It also closes the session when the test ends. That used to be the caller's
// business, because a session nobody started left nothing behind; since plan
// 021's C10 it can, because a settings delta is *enqueued* rather than
// published and the log starts its outbox drainer to publish it. Close is
// idempotent, so the constructors that register a cleanup of their own
// (newLoadSession, startScript) are unaffected.
func newTestSession(t *testing.T, opts Options) *session {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	s := newSession(opts)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func startScript(t *testing.T, script string, force bool) *session {
	t.Helper()
	s := newTestSession(t, Options{
		Binary:    fakeAgentPath(t),
		ExtraArgs: []string{"-script=" + script},
		Workspace: t.TempDir(),
		Force:     force,
		Stderr:    io.Discard,
	})
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func collect(t *testing.T, s Session) *eventLog {
	t.Helper()
	log := &eventLog{}
	// The session's own log, when it has one: what the exactly-once check of
	// waitAsk counts, because this collector only ever knows what its goroutine
	// has got round to appending.
	if owner, ok := s.(LogOwner); ok {
		log.log = owner.EventLog()
	}
	ctx := t.Context()
	go func() {
		for {
			select {
			case ev, ok := <-s.Events():
				if !ok {
					return
				}
				log.add(ev)
			case <-ctx.Done():
				return
			}
		}
	}()
	return log
}

type eventLog struct {
	// log is the session's own event log, when collect could find one: the
	// record itself, as against this collector's view of it.
	log *EventLog

	mu   sync.Mutex
	list []Event
}

func (e *eventLog) add(ev Event) {
	e.mu.Lock()
	e.list = append(e.list, ev)
	e.mu.Unlock()
}

func (e *eventLog) snapshot() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Event, len(e.list))
	copy(out, e.list)
	return out
}

func (e *eventLog) waitType(t *testing.T, typ EventType) Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range e.snapshot() {
			if ev.Type == typ {
				return ev
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", typ)
	return Event{}
}

func (e *eventLog) waitTexts(t *testing.T, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := texts(e.snapshot())
		if got == want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for text %q, got %q", want, texts(e.snapshot()))
	return ""
}

func drainEvents(s *session) {
	for {
		select {
		case <-s.Events():
		default:
			return
		}
	}
}

func waitEventType(t *testing.T, s *session, typ EventType) Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", typ)
			return Event{}
		}
	}
}

func texts(evs []Event) string {
	var b strings.Builder
	for _, ev := range evs {
		if ev.Type == EventText {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

func TestSessionPromptStreamFollowUp(t *testing.T) {
	s := startScript(t, "followup", true)
	log := collect(t, s)
	res, err := s.Prompt(t.Context(), "one")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != acp.StopEndTurn {
		t.Fatalf("stop %q", res.StopReason)
	}
	log.waitTexts(t, "first reply")
	if _, err := s.Prompt(t.Context(), "two"); err != nil {
		t.Fatal(err)
	}
	log.waitTexts(t, "first replysecond reply")
}

func TestSnapshotModelsModesCommands(t *testing.T) {
	s := startScript(t, "echo", true)
	deadline := time.Now().Add(5 * time.Second)
	var snap Snapshot
	for time.Now().Before(deadline) {
		snap = s.Snapshot()
		if commandNamed(snap, "research") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap.CurrentModel != "default" || snap.CurrentMode != "agent" {
		t.Fatalf("%+v", snap)
	}
	if len(snap.Models) < 2 || snap.Models[1].ID != "composer" {
		t.Fatalf("models %+v", snap.Models)
	}
	if len(snap.Modes) != 3 {
		t.Fatalf("modes %+v", snap.Modes)
	}
	if !commandNamed(snap, "research") {
		t.Fatalf("commands %+v", snap.Commands)
	}
	if _, err := s.SetMode(t.Context(), "", "plan"); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().CurrentMode != "plan" {
		t.Fatalf("mode %q", s.Snapshot().CurrentMode)
	}
}

func commandNamed(snap Snapshot, name string) bool {
	for _, c := range snap.Commands {
		if c.Name == name {
			return true
		}
	}
	return false
}

func TestYoloAutoAllow(t *testing.T) {
	s := startScript(t, "permission", true)
	log := collect(t, s)
	res, err := s.Prompt(t.Context(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != acp.StopEndTurn {
		t.Fatalf("stop %q", res.StopReason)
	}
	log.waitTexts(t, "decision:opt-always")
	for _, ev := range log.snapshot() {
		if ev.Type == EventPermission {
			t.Fatal("yolo should auto-answer without exposing a pending permission")
		}
	}
}

func TestPromptModePermissionAllowAndReject(t *testing.T) {
	t.Run("allow", func(t *testing.T) {
		s := startScript(t, "permission", false)
		log := collect(t, s)
		errCh := make(chan error, 1)
		go func() {
			_, err := s.Prompt(t.Context(), "go")
			errCh <- err
		}()
		ev := log.waitType(t, EventPermission)
		if ev.Permission == nil || len(ev.Permission.Options) == 0 {
			t.Fatal("missing permission options")
		}
		if err := answerAsk(s, ev.Permission.ID, AskAnswer{OptionID: "opt-once"}); err != nil {
			t.Fatal(err)
		}
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
		log.waitTexts(t, "decision:opt-once")
	})

	t.Run("reject", func(t *testing.T) {
		s := startScript(t, "permission", false)
		log := collect(t, s)
		errCh := make(chan error, 1)
		go func() {
			_, err := s.Prompt(t.Context(), "go")
			errCh <- err
		}()
		ev := log.waitType(t, EventPermission)
		if err := answerAsk(s, ev.Permission.ID, AskAnswer{OptionID: "opt-reject"}); err != nil {
			t.Fatal(err)
		}
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
		log.waitTexts(t, "decision:opt-reject")
	})
}

// TestCancelWaitsUntilPromptReturns pins the ordering the CI hang broke:
// Cancel must never write session/cancel before session/prompt is on the
// wire. session.Prompt sets s.inPrompt (what promptInFlight reads) before
// client.PromptBlocks ever writes the request, and until issue #18 was fixed
// Cancel wrote off that flag alone, so waiting on it — as this test used to —
// let Cancel race ahead of the prompt it meant to interrupt. The fake's
// session/prompt handler clears its own cancelled flag on purpose the instant
// it reads a prompt (see onRequest in cmd/craze-fake-agent/server.go),
// specifically so a cancel that arrived first is discarded rather than
// answered twice; a discarded cancel then left the "hang" script waiting
// forever for a second one that never came, which is exactly the 4m45s CI
// hang this test was moved to close. Cancel now waits for the prompt's own
// write before it writes (TestCancelOffPromptInFlightOnHang is that shape,
// re-added). This test keeps the "hang-ack" script, which sends one chunk the
// instant it reads the prompt, so waiting for that chunk is proof of the read,
// not a hope about goroutine scheduling — and what it pins does not lean on
// that fix.
func TestCancelWaitsUntilPromptReturns(t *testing.T) {
	s := startScript(t, "hang-ack", true)
	log := collect(t, s)
	errCh := make(chan error, 1)
	var res Result
	go func() {
		var err error
		res, err = s.Prompt(t.Context(), "wait")
		errCh <- err
	}()
	log.waitTexts(t, "ack: wait")

	// Cancel gets its own bounded context so a regression of the ordering
	// inversion above fails this one test in 15s with a clear message,
	// instead of hanging the whole package to its 5-minute timeout the way
	// the CI run that found this did.
	cancelCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := s.Cancel(cancelCtx); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Cancel did not wait for prompt")
	}
	// The elapsed check above only says Cancel was not slow; this says it
	// waited at all. Prompt clears inPrompt in the same locked section that
	// closes promptDone, and Cancel returns on that close, so a Cancel that
	// fired the notification and returned without waiting is caught here
	// with no window of its own to race.
	if s.promptInFlight() {
		t.Fatal("Cancel returned while the prompt was still in flight")
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if res.StopReason != acp.StopCancelled {
		t.Fatalf("stop %q", res.StopReason)
	}
}

// TestSerializedPrompt shares the "hang-ack" script with
// TestCancelWaitsUntilPromptReturns, and its trailing Cancel shares the exact
// same hazard: nothing else here writes to the wire (the second Prompt is
// refused entirely in-process, before ever reaching the client), but the
// cleanup Cancel is the same race against session/prompt described above.
// Left on the old "hang" script and an unbounded context, a lost race would
// hang this test's goroutine, and with it the package, forever.
func TestSerializedPrompt(t *testing.T) {
	s := startScript(t, "hang-ack", true)
	log := collect(t, s)
	go func() { _, _ = s.Prompt(t.Context(), "one") }()
	log.waitTexts(t, "ack: one")
	_, err := s.Prompt(t.Context(), "two")
	if err == nil {
		t.Fatal("expected in-flight error")
	}
	cancelCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if _, err := s.Cancel(cancelCtx); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

// TestCancelOffPromptInFlightOnHang is the exact shape that hung CI for 4m45s
// (run 35290845362), and the shape PR #17 had to move the tests above away
// from: the plain, silent "hang" script, and Cancel issued the moment
// promptInFlight says the turn is open, with nothing else to wait on. Then
// promptInFlight flipped before session/prompt was written, the cancel could
// reach the fake first, the fake dropped it by design, and hang waited forever
// for another. It is re-addable only because of the fix for issue #18 —
// Cancel now waits for the prompt's own write before writing — and it is not
// a retry of a flaky test: under the fix it passes every time. On a regression
// it fails probabilistically, since the race has to be lost, as a 15s timeout
// with the message below rather than a package hang; the deterministic proof is
// TestCancelWaitsForThePromptToBeWritten.
func TestCancelOffPromptInFlightOnHang(t *testing.T) {
	s := startScript(t, "hang", true)
	type outcome struct {
		res Result
		err error
	}
	out := make(chan outcome, 1)
	go func() {
		res, err := s.Prompt(context.Background(), "wait")
		out <- outcome{res, err}
	}()
	waitUntil(t, "the turn to open", s.promptInFlight)

	cancelCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if _, err := s.Cancel(cancelCtx); err != nil {
		t.Fatalf("cancel: %v (a cancel written ahead of its prompt is dropped, and hang never returns)", err)
	}
	select {
	case o := <-out:
		if o.err != nil {
			t.Fatal(o.err)
		}
		if o.res.StopReason != acp.StopCancelled {
			t.Fatalf("stop %q", o.res.StopReason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt never returned after Cancel")
	}
}

func TestAuthFailStart(t *testing.T) {
	s := newTestSession(t, Options{
		Binary:    fakeAgentPath(t),
		ExtraArgs: []string{"-script=authfail"},
		Workspace: t.TempDir(),
		Force:     true,
		Stderr:    io.Discard,
	})
	err := s.Start(t.Context())
	if err == nil {
		t.Fatal("expected auth error")
	}
	if !strings.Contains(err.Error(), "agent login") {
		t.Fatalf("want agent login hint, got %v", err)
	}
}

func TestNoAuthSkipsAuthenticate(t *testing.T) {
	s := startScript(t, "noauth", true)
	log := collect(t, s)
	res, err := s.Prompt(t.Context(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != acp.StopEndTurn {
		t.Fatalf("stop %q", res.StopReason)
	}
	log.waitTexts(t, "echo: hello")
}

func TestPromptEchoDropsOtherSession(t *testing.T) {
	s := startScript(t, "echo", true)
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	got := log.waitTexts(t, "echo: hello")
	if strings.Contains(got, "NOPE") {
		t.Fatalf("leaked other-session chunk: %q", got)
	}
}

func TestToolEvent(t *testing.T) {
	s := startScript(t, "tool", true)
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	ev := log.waitType(t, EventTool)
	if ev.Tool == nil || ev.Tool.ID != "call-1" {
		t.Fatalf("tool %+v", ev.Tool)
	}
	log.waitTexts(t, "after tool")
}

func TestTasksToolsSnapshot(t *testing.T) {
	s := startScript(t, "tasks", true)
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	log.waitTexts(t, "done tasks")
	log.waitType(t, EventDone)
	snap := s.Snapshot()
	if len(snap.Tools) != 2 {
		t.Fatalf("tools %+v", snap.Tools)
	}
	task := toolByID(t, snap.Tools, "task-1")
	sh := toolByID(t, snap.Tools, "sh-1")
	if task.Status != "completed" || sh.Status != "completed" {
		t.Fatalf("status task=%q sh=%q", task.Status, sh.Status)
	}
	if task.Title != "Subagent research" || task.Kind != "other" {
		t.Fatalf("task %+v", task)
	}
	if sh.Title != "Shell" || sh.Kind != "execute" {
		t.Fatalf("sh %+v", sh)
	}
	if !strings.Contains(sh.RawInput, "echo hi") {
		t.Fatalf("rawInput %q", sh.RawInput)
	}
	if snap.Tools[0].ID != "task-1" || snap.Tools[1].ID != "sh-1" {
		t.Fatalf("create order %+v", snap.Tools)
	}
	n := 0
	for _, ev := range log.snapshot() {
		if ev.Type == EventTool {
			n++
		}
	}
	if n < 2 || n > 16 {
		t.Fatalf("EventTool count %d (want small, not one-per-byte)", n)
	}
}

func TestSetConfigEffort(t *testing.T) {
	s := startScript(t, "effort", true)
	if got := configCurrent(s.Snapshot(), "effort"); got != "medium" {
		t.Fatalf("current %q, config %+v", got, s.Snapshot().Config)
	}
	if _, err := s.SetConfig(t.Context(), "", "effort", "high"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if configCurrent(s.Snapshot(), "effort") == "high" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("current %q, config %+v", configCurrent(s.Snapshot(), "effort"), s.Snapshot().Config)
}

func toolByID(t *testing.T, tools []ToolEvent, id string) ToolEvent {
	t.Helper()
	for _, tool := range tools {
		if tool.ID == id {
			return tool
		}
	}
	t.Fatalf("missing tool %s in %+v", id, tools)
	return ToolEvent{}
}

func configCurrent(snap Snapshot, id string) string {
	for _, c := range snap.Config {
		if c.ID == id {
			return c.Current
		}
	}
	return ""
}

func TestAskAndPlanYolo(t *testing.T) {
	t.Run("ask", func(t *testing.T) {
		s := startScript(t, "ask", true)
		log := collect(t, s)
		if _, err := s.Prompt(t.Context(), "q"); err != nil {
			t.Fatal(err)
		}
		log.waitTexts(t, "asked:answered:q1=opt-a;q2=opt-x")
	})
	t.Run("plan", func(t *testing.T) {
		s := startScript(t, "plan", true)
		log := collect(t, s)
		if _, err := s.Prompt(t.Context(), "p"); err != nil {
			t.Fatal(err)
		}
		log.waitTexts(t, "planned:accepted")
	})
}

func TestCancelPendingPermission(t *testing.T) {
	s := startScript(t, "permission", false)
	log := collect(t, s)
	errCh := make(chan error, 1)
	var res Result
	go func() {
		var err error
		res, err = s.Prompt(t.Context(), "go")
		errCh <- err
	}()
	log.waitType(t, EventPermission)
	if _, err := s.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if res.StopReason != acp.StopCancelled {
		t.Fatalf("stop %q", res.StopReason)
	}
}

func TestSpawnArgvForceAndTrust(t *testing.T) {
	t.Run("yolo", func(t *testing.T) {
		dump := filepath.Join(t.TempDir(), "argv")
		s := newTestSession(t, Options{
			Binary:    fakeAgentPath(t),
			ExtraArgs: []string{"-script=echo"},
			Workspace: t.TempDir(),
			Force:     true,
			Env:       append(os.Environ(), "CRAZE_FAKE_DUMP_ARGV="+dump),
			Stderr:    io.Discard,
		})
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		got := readArgv(t, dump)
		if !strings.HasSuffix(got, "-script=echo\n--force\n--trust\nacp\n") &&
			!matchTail(got, []string{"-script=echo", "--force", "--trust", "acp"}) {
			t.Fatalf("argv %q", got)
		}
	})
	t.Run("no-force", func(t *testing.T) {
		dump := filepath.Join(t.TempDir(), "argv")
		s := newTestSession(t, Options{
			Binary:    fakeAgentPath(t),
			ExtraArgs: []string{"-script=echo"},
			Workspace: t.TempDir(),
			Force:     false,
			Env:       append(os.Environ(), "CRAZE_FAKE_DUMP_ARGV="+dump),
			Stderr:    io.Discard,
		})
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		got := readArgv(t, dump)
		if !matchTail(got, []string{"-script=echo", "--trust", "acp"}) {
			t.Fatalf("argv %q", got)
		}
		if strings.Contains(got, "--force") {
			t.Fatalf("did not expect --force: %q", got)
		}
	})
}

func TestUnknownModeRejected(t *testing.T) {
	s := newTestSession(t, Options{
		Binary:    fakeAgentPath(t),
		ExtraArgs: []string{"-script=echo"},
		Workspace: t.TempDir(),
		Force:     true,
		Mode:      "not-a-mode",
		Stderr:    io.Discard,
	})
	err := s.Start(t.Context())
	if err == nil {
		_ = s.Close()
		t.Fatal("expected unknown mode error")
	}
	if !strings.Contains(err.Error(), "did not advertise") {
		t.Fatalf("got %v", err)
	}
}

func TestSetModeOnStart(t *testing.T) {
	s := newTestSession(t, Options{
		Binary:    fakeAgentPath(t),
		ExtraArgs: []string{"-script=echo"},
		Workspace: t.TempDir(),
		Force:     true,
		Mode:      "plan",
		Model:     "default",
		Stderr:    io.Discard,
	})
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "hi"); err != nil {
		t.Fatal(err)
	}
	log.waitTexts(t, "echo: hi")
}

func TestChildCrashStart(t *testing.T) {
	s := newTestSession(t, Options{
		Binary:    "/bin/false",
		Workspace: t.TempDir(),
		Force:     true,
		Stderr:    io.Discard,
	})
	err := s.Start(t.Context())
	if err == nil {
		_ = s.Close()
		t.Fatal("expected start error after child exit")
	}
}

// TestSessionCloseReportsAgentExitedRepeatedly is issue #23's §3.7.3: two
// session.Close calls on a session whose client self-exited both give an
// errors.Is(…, ErrAgentExited) error, which is what requestQuit-then-
// finishRun relies on. The agent is killed directly, by pid, rather than
// through any craze API, so the death is genuinely the agent's own.
//
// kill(pid, 0) reporting ESRCH is deterministic proof the reaper's own
// cmd.Wait has already reaped the zombie: nothing else in this process calls
// wait4 on this child, so the pid cannot disappear from the process table
// any earlier than that — the same fact Close's own waitCh probe rests on.
func TestSessionCloseReportsAgentExitedRepeatedly(t *testing.T) {
	s := startScript(t, "echo", true)
	pid := s.client.PID()
	if pid == 0 {
		t.Fatal("setup: the session has no child pid")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find process: %v", err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitUntil(t, "the agent to be reaped", func() bool {
		return proc.Signal(syscall.Signal(0)) != nil
	})
	first := s.Close()
	if !errors.Is(first, ErrAgentExited) {
		t.Fatalf("Close() = %v, want an error wrapping ErrAgentExited", first)
	}
	if second := s.Close(); !errors.Is(second, ErrAgentExited) {
		t.Fatalf("second Close() = %v, want an error wrapping ErrAgentExited", second)
	}
}

func readArgv(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var b []byte
	var err error
	for time.Now().Before(deadline) {
		b, err = os.ReadFile(path)
		if err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("argv dump: %v", err)
	return ""
}

func matchTail(got string, want []string) bool {
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) < len(want) {
		return false
	}
	tail := lines[len(lines)-len(want):]
	for i := range want {
		if tail[i] != want[i] {
			return false
		}
	}
	return true
}

// TestCursorSpawnArgvRegression pins the current spawn line, ExtraArgs first,
// so the provider seam cannot silently reorder it.
func TestCursorSpawnArgvRegression(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "argv")
	cursor := CursorProvider()
	s := newTestSession(t, Options{
		Binary:    fakeAgentPath(t),
		ExtraArgs: []string{"-script=echo"},
		Workspace: t.TempDir(),
		Force:     true,
		Provider:  &cursor,
		Env:       append(os.Environ(), "CRAZE_FAKE_DUMP_ARGV="+dump),
		Stderr:    io.Discard,
	})
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	got := readArgv(t, dump)
	if !matchTail(got, []string{"-script=echo", "--force", "--trust", "acp"}) {
		t.Fatalf("argv %q", got)
	}
	if strings.Contains(got, "--always-approve") || strings.Contains(got, "--no-auto-update") {
		t.Fatalf("cursor argv carries grok flags: %q", got)
	}
}

// TestGrokSpawnArgvOrder pins the grok spawn line: ExtraArgs first, then the
// global flag, the agent subcommand, the force flag, and stdio. --trust acp
// must never appear on the grok line.
func TestGrokSpawnArgvOrder(t *testing.T) {
	for _, force := range []bool{true, false} {
		t.Run(map[bool]string{true: "yolo", false: "no-force"}[force], func(t *testing.T) {
			dump := filepath.Join(t.TempDir(), "argv")
			grok := GrokProvider()
			s := newTestSession(t, Options{
				Binary:    fakeAgentPath(t),
				ExtraArgs: []string{"-script=echo"},
				Workspace: t.TempDir(),
				Force:     force,
				Provider:  &grok,
				Env:       append(os.Environ(), "CRAZE_FAKE_DUMP_ARGV="+dump),
				Stderr:    io.Discard,
			})
			// The fake only advertises cursor auth, so grok without an API
			// key and without a cached token fails auth by design; the
			// argv dump it writes on startup is still what this asserts.
			_ = s.Start(t.Context())
			t.Cleanup(func() { _ = s.Close() })
			got := readArgv(t, dump)
			want := []string{"-script=echo", "--no-auto-update", "agent", "stdio"}
			if force {
				want = []string{"-script=echo", "--no-auto-update", "agent", "--always-approve", "stdio"}
			}
			if !matchTail(got, want) {
				t.Fatalf("argv %q", got)
			}
			if strings.Contains(got, "--trust") {
				t.Fatalf("grok argv must not carry --trust: %q", got)
			}
			if strings.Contains(got, "--force") {
				t.Fatalf("grok argv must not carry --force: %q", got)
			}
		})
	}
}

func startGrokScript(t *testing.T, script string, force bool) *session {
	t.Helper()
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	grok := GrokProvider()
	s := newTestSession(t, Options{
		Binary:    fakeAgentPath(t),
		ExtraArgs: []string{"-script=" + script},
		Workspace: t.TempDir(),
		Force:     force,
		Provider:  &grok,
		Stderr:    io.Discard,
	})
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestGrokLoginHint(t *testing.T) {
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	grok := GrokProvider()
	s := newTestSession(t, Options{
		Binary:    fakeAgentPath(t),
		ExtraArgs: []string{"-script=authfail"},
		Workspace: t.TempDir(),
		Force:     true,
		Provider:  &grok,
		Stderr:    io.Discard,
	})
	err := s.Start(t.Context())
	if err == nil {
		_ = s.Close()
		t.Fatal("expected auth error")
	}
	if !strings.Contains(err.Error(), "grok login") {
		t.Fatalf("login hint: %v", err)
	}
}

func TestGrokEnvKeyVsCachedTokenStart(t *testing.T) {
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	for _, tc := range []struct {
		name   string
		key    string
		wantID string
	}{
		{"env-key", "test-key", acp.AuthXAIAPIKey},
		{"cached-token", "", acp.AuthCachedToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XAI_API_KEY", tc.key)
			dump := filepath.Join(t.TempDir(), "auth")
			grok := GrokProvider()
			s := newTestSession(t, Options{
				Binary:    fakeAgentPath(t),
				ExtraArgs: []string{"-script=grok-echo"},
				Workspace: t.TempDir(),
				Force:     true,
				Provider:  &grok,
				Env:       append(os.Environ(), "CRAZE_FAKE_DUMP_AUTH="+dump, "XAI_API_KEY="+tc.key),
				Stderr:    io.Discard,
			})
			if err := s.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			raw := readArgv(t, dump)
			var params acp.AuthenticateParams
			if err := json.Unmarshal([]byte(raw), &params); err != nil {
				t.Fatalf("auth dump %q: %v", raw, err)
			}
			if params.MethodID != tc.wantID {
				t.Fatalf("methodId %q want %q", params.MethodID, tc.wantID)
			}
			if params.Meta["headless"] != true {
				t.Fatalf("meta %v", params.Meta)
			}
		})
	}
}

func TestGrokHeadlessAskAndPlan(t *testing.T) {
	t.Run("ask", func(t *testing.T) {
		s := startGrokScript(t, "grok-ask", true)
		log := collect(t, s)
		if _, err := s.Prompt(t.Context(), "q"); err != nil {
			t.Fatal(err)
		}
		q := log.waitQuestion(t)
		if !q.Auto || q.Answers["Pick one"][0] != "A" {
			t.Fatalf("question %+v", q)
		}
		log.waitTexts(t, "asked:accepted:Pick one=A;Pick any=X")
	})
	t.Run("ask-wrapped", func(t *testing.T) {
		s := startGrokScript(t, "grok-ask-wrapped", true)
		log := collect(t, s)
		if _, err := s.Prompt(t.Context(), "q"); err != nil {
			t.Fatal(err)
		}
		log.waitTexts(t, "asked:accepted:Pick one=A;Pick any=X")
	})
	t.Run("plan", func(t *testing.T) {
		s := startGrokScript(t, "grok-plan", true)
		log := collect(t, s)
		if _, err := s.Prompt(t.Context(), "p"); err != nil {
			t.Fatal(err)
		}
		ev := log.waitType(t, EventPlan)
		if !ev.Plan.Auto || !ev.Plan.Accepted || !strings.Contains(ev.Plan.Plan, "## Steps") {
			t.Fatalf("plan %+v", ev.Plan)
		}
		log.waitTexts(t, "planned:approved")
	})
}

func TestGrokEchoPromptComplete(t *testing.T) {
	s := startGrokScript(t, "grok-echo", true)
	log := collect(t, s)
	res, err := s.Prompt(t.Context(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != acp.StopEndTurn {
		t.Fatalf("stop %q", res.StopReason)
	}
	log.waitTexts(t, "echo: hello")
	waitFor(t, "EventDone", func() bool {
		for _, ev := range log.snapshot() {
			if ev.Type == EventDone {
				return true
			}
		}
		return false
	})
	nDone := 0
	for _, ev := range log.snapshot() {
		if ev.Type == EventDone {
			nDone++
		}
	}
	if nDone != 1 {
		t.Fatalf("done events %d", nDone)
	}
}
