package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// A detached host and background bash jobs (plan 033 §3.8, owner decision 3,
// P14; §7 A13, A13b): the idle watcher over a real engine over a real
// interactive native session, whose jobs are real commands through the real
// bash tool, each held on a named pipe so it ends when the test opens it. The
// watcher's clock is the rig's own (idle_test.go), so only the test moves it.

// lockedSteps is a fantasy.LanguageModel that answers each Stream call with
// the next step, and counts the calls under a lock: the session's goroutines
// make them while the test reads the count.
type lockedSteps struct {
	mu    sync.Mutex
	steps [][]fantasy.StreamPart
	calls int
}

func (m *lockedSteps) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls >= len(m.steps) {
		m.calls++
		return nil, errors.New("lockedSteps: no step queued")
	}
	step := m.steps[m.calls]
	m.calls++
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range step {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (m *lockedSteps) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *lockedSteps) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("lockedSteps: Generate is not used")
}

func (m *lockedSteps) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("lockedSteps: GenerateObject is not used")
}

func (m *lockedSteps) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("lockedSteps: StreamObject is not used")
}

func (m *lockedSteps) Provider() string { return "test" }
func (m *lockedSteps) Model() string    { return "wire" }

// interactiveNative is an unstarted interactive native session in ws over
// model, for an engine to start.
func interactiveNative(t *testing.T, model fantasy.LanguageModel, ws string) agent.Session {
	t.Helper()
	table := &modeltable.Table{
		DefaultModel: "test/a",
		Providers: map[string]modeltable.Provider{
			"test": {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"IDLE_JOBS_TEST_KEY"}},
		},
		Models: map[string]modeltable.Model{"test/a": {Provider: "test", WireModel: "wire-a", Name: "A"}},
	}
	home := t.TempDir()
	return agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir(), Interactive: true}, func(o *harness.Options) {
		o.Home = home
		o.Table = table
		o.Getenv = func(k string) string {
			if k == "IDLE_JOBS_TEST_KEY" {
				return "test-key-for-idle"
			}
			return ""
		}
		o.NewModel = func(modeltable.Resolved) (fantasy.LanguageModel, error) { return model, nil }
		o.Now = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }
	})
}

// waitUntil polls cond, a state the session reaches on its own, within
// serveStep.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", serveStep, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestIdleExitAfterTheJobWakeCap (A13, A13b): running jobs keep a detached
// host alive past its idle limit — the negative control, looked at twice:
// with four running and with the last one running after three wakes. Three
// jobs ending one after another wake the session three times; the fourth's
// result is set aside (the wake chain's cap), so once it ends nothing is in
// flight, no fourth wake runs, and the next look past the limit stops the
// host.
func TestIdleExitAfterTheJobWakeCap(t *testing.T) {
	ws := t.TempDir()
	var calls []fantasy.StreamPart
	for i := 1; i <= 4; i++ {
		if err := syscall.Mkfifo(fmt.Sprintf("%s/g%d", ws, i), 0o600); err != nil {
			t.Fatal(err)
		}
		in := fmt.Sprintf(`{"command":"read line < g%d","run_in_background":true}`, i)
		calls = append(calls, nativeJSONCall(fmt.Sprintf("c%d", i), "bash", in)[:4]...) // its parts, not its finish
	}
	calls = append(calls, fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
	model := &lockedSteps{steps: [][]fantasy.StreamPart{
		calls,
		nativeJSONAnswer("", "four started"),
		nativeJSONAnswer("", "noted 1"),
		nativeJSONAnswer("", "noted 2"),
		nativeJSONAnswer("", "noted 3"),
	}}
	sess := interactiveNative(t, model, ws)
	r := newIdleRigOver(t, "", true, sess)
	gate := func(i int) {
		t.Helper()
		f, err := os.OpenFile(fmt.Sprintf("%s/g%d", ws, i), os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("x\n")
		_ = f.Close()
	}
	running := func() int {
		n := 0
		for _, row := range sess.Snapshot().Subagents {
			if row.Status == agent.SubagentRunning && row.SubagentType == agent.BashJobType {
				n++
			}
		}
		return n
	}

	if _, err := r.eng.Submit(engine.Command{}, "go", engine.SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the turn's end, four jobs running", func() bool { return r.eng.State().LastTurn != nil && running() == 4 })
	r.advance(hostStartupGrace + time.Second)
	r.wantStay("four jobs running")
	r.advance(2 * time.Hour)
	r.wantStay("four jobs running past the idle limit")

	for i := 1; i <= 3; i++ {
		gate(i)
		waitUntil(t, fmt.Sprintf("wake %d's end", i), func() bool { return model.count() == 2+i && !sess.ForeignTurn() && running() == 4-i })
	}
	r.advance(2 * time.Hour)
	r.wantStay("the fourth job running")

	gate(4)
	waitUntil(t, "nothing in flight once the fourth job ended", func() bool { return running() == 0 && !r.eng.Busy() })
	if n := model.count(); n != 5 {
		t.Fatalf("the model was asked %d times; want 5: the turn's two steps and three wakes, none for the fourth job", n)
	}
	r.advance(2 * time.Hour)
	r.wantStop("the cap reached, nothing owed", "idle for")
}
