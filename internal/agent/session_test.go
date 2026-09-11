package agent

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

func startScript(t *testing.T, script string, force bool) *session {
	t.Helper()
	s := newSession(Options{
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
		if err := s.AnswerPermission(ev.Permission.ID, "opt-once"); err != nil {
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
		if err := s.AnswerPermission(ev.Permission.ID, "opt-reject"); err != nil {
			t.Fatal(err)
		}
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
		log.waitTexts(t, "decision:opt-reject")
	})
}

func TestCancelWaitsUntilPromptReturns(t *testing.T) {
	s := startScript(t, "hang", true)
	errCh := make(chan error, 1)
	var res Result
	go func() {
		var err error
		res, err = s.Prompt(t.Context(), "wait")
		errCh <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !s.promptInFlight() {
		time.Sleep(5 * time.Millisecond)
	}
	if !s.promptInFlight() {
		t.Fatal("prompt not in flight")
	}
	start := time.Now()
	if err := s.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Cancel did not wait for prompt")
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if res.StopReason != acp.StopCancelled {
		t.Fatalf("stop %q", res.StopReason)
	}
}

func TestSerializedPrompt(t *testing.T) {
	s := startScript(t, "hang", true)
	go func() { _, _ = s.Prompt(t.Context(), "one") }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !s.promptInFlight() {
		time.Sleep(5 * time.Millisecond)
	}
	_, err := s.Prompt(t.Context(), "two")
	if err == nil {
		t.Fatal("expected in-flight error")
	}
	_ = s.Cancel(t.Context())
}

func TestAuthFailStart(t *testing.T) {
	s := newSession(Options{
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

func TestAskAndPlanYolo(t *testing.T) {
	t.Run("ask", func(t *testing.T) {
		s := startScript(t, "ask", true)
		log := collect(t, s)
		if _, err := s.Prompt(t.Context(), "q"); err != nil {
			t.Fatal(err)
		}
		log.waitTexts(t, "asked:answered")
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
	if err := s.Cancel(t.Context()); err != nil {
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
		s := newSession(Options{
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
		s := newSession(Options{
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

func TestSetModeOnStart(t *testing.T) {
	s := newSession(Options{
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
	s := newSession(Options{
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
