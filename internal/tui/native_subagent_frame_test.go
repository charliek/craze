package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The native adapter's sub-agents on screen (plan 026 §3.9, §3.12, A11): a
// real native session (agent.NewNative) whose parent calls the agent tool,
// the harness's runner opening real child sessions, and the frame drawn by the
// row band, the child view and the task row grok's children already use —
// nothing in internal/tui changed for any of it (§3.12).
//
// Parent and children share one routing scripted model per alias
// (frameRouter), keyed by each request's turn prompt, so children streaming at
// once never take each other's steps. A child's last step can hold
// (frameRouter.hold) until the frame runner has ended the session, which is
// how a frame of children still running is taken. Two things a frame shows are
// pinned from outside the harness: the order two children spawn in, which a
// routing model cannot decide — the second child runs on a model whose client
// (harness.Options.NewModel, built in the child's Open) is handed out only once
// the first child has sent its first request, and a child's first request
// follows its spawn — and a finished child's duration, which the adapter reads
// off the parent's clock (harness.Options.Now): a child's own step moves that
// clock on (frameClock), so the row says 1.2s every run.

// frameStep is one scripted response.
type frameStep func(ctx context.Context, yield func(fantasy.StreamPart) bool)

// frameParts is a step that yields parts and ends.
func frameParts(parts ...[]fantasy.StreamPart) frameStep {
	return func(_ context.Context, yield func(fantasy.StreamPart) bool) {
		for _, p := range cat(parts...) {
			if !yield(p) {
				return
			}
		}
	}
}

// frameRouter is a fantasy.LanguageModel answering each request with the next
// step queued for its turn's prompt — the last user message's text: every
// session here runs one turn, and no steer or mode reminder follows it.
type frameRouter struct {
	provider, wire string

	mu     sync.Mutex
	queues map[string][]frameStep
	holds  map[string][]fantasy.StreamPart
	// asked, when set, is told each prompt the first time a request for it
	// arrives.
	asked func(prompt string)
	seen  map[string]bool
}

func newFrameRouter(provider, wire string) *frameRouter {
	return &frameRouter{provider: provider, wire: wire,
		queues: map[string][]frameStep{}, holds: map[string][]fantasy.StreamPart{}, seen: map[string]bool{}}
}

func (m *frameRouter) route(prompt string, steps ...frameStep) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queues[prompt] = append(m.queues[prompt], steps...)
}

// hold answers prompt's request once its queued steps are spent: before, and
// then a wait for the session to end — a child caught mid-answer.
func (m *frameRouter) hold(prompt string, before []fantasy.StreamPart) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.holds[prompt] = before
}

func (m *frameRouter) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	key := ""
	for _, msg := range call.Prompt {
		if msg.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, p := range msg.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
				key = tp.Text
				break
			}
		}
	}
	m.mu.Lock()
	first := !m.seen[key]
	m.seen[key] = true
	asked := m.asked
	var next frameStep
	if q := m.queues[key]; len(q) > 0 {
		next, m.queues[key] = q[0], q[1:]
	} else if before, ok := m.holds[key]; ok {
		next = func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			for _, p := range before {
				if !yield(p) {
					return
				}
			}
			<-ctx.Done()
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
		}
	}
	m.mu.Unlock()
	if first && asked != nil {
		asked(key)
	}
	if next == nil {
		return nil, fmt.Errorf("frameRouter: no step queued for %q", key)
	}
	return func(yield func(fantasy.StreamPart) bool) { next(ctx, yield) }, nil
}

func (m *frameRouter) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("frameRouter: Generate is not used")
}

func (m *frameRouter) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("frameRouter: GenerateObject is not used")
}

func (m *frameRouter) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("frameRouter: StreamObject is not used")
}

func (m *frameRouter) Provider() string { return m.provider }
func (m *frameRouter) Model() string    { return m.wire }

// frameClock is the parent's clock (harness.Options.Now): a fixed instant
// plus what the scripted steps moved it on by.
type frameClock struct{ offset atomic.Int64 }

func (c *frameClock) now() time.Time {
	return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC).Add(time.Duration(c.offset.Load()))
}

// timed is a step that yields before, moves the clock on by d, and yields
// after: a child that took d.
func (c *frameClock) timed(d time.Duration, before, after []fantasy.StreamPart) frameStep {
	return func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		for _, p := range before {
			if !yield(p) {
				return
			}
		}
		c.offset.Add(int64(d))
		for _, p := range after {
			if !yield(p) {
				return
			}
		}
	}
}

// frameTable is two models on one keyed provider: echo, the parent's and
// every child's but one, and two, the model the second of two concurrent
// children runs on so its spawn can wait for the first's.
func frameTable() *modeltable.Table {
	return &modeltable.Table{
		DefaultModel: "test/echo",
		Providers: map[string]modeltable.Provider{
			"test": {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"NATIVE_TUI_TEST_KEY"}},
		},
		Models: map[string]modeltable.Model{
			"test/echo": {Provider: "test", WireModel: "wire-echo", Name: "Echo"},
			"test/two":  {Provider: "test", WireModel: "wire-two", Name: "Two"},
		},
	}
}

// frameSubagents is a native session over frameTable, its models the two
// routers, its clock c. gate, when set, is waited for before test/two's client
// is handed out (the file's comment).
func frameSubagents(t *testing.T, ws string, echo, two *frameRouter, c *frameClock, gate <-chan struct{}) agent.Session {
	t.Helper()
	home := t.TempDir()
	table := frameTable()
	return agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()}, func(o *harness.Options) {
		o.Home = home
		o.Table = table
		o.Getenv = func(k string) string {
			if k == "NATIVE_TUI_TEST_KEY" {
				return "test-key"
			}
			return ""
		}
		o.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
			if r.Alias != "test/two" {
				return echo, nil
			}
			if gate != nil {
				select {
				case <-gate:
				case <-time.After(15 * time.Second):
					return nil, errors.New("frameSubagents: the first child never asked")
				}
			}
			return two, nil
		}
		o.Now = c.now
	})
}

// frameAgentCall is one agent call's parts in a parent's step.
func frameAgentCall(id, description, prompt string, more ...string) []fantasy.StreamPart {
	args := fmt.Sprintf(`{"description":%q,"prompt":%q`, description, prompt)
	for i := 0; i+1 < len(more); i += 2 {
		args += fmt.Sprintf(`,%q:%q`, more[i], more[i+1])
	}
	args += "}"
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: "agent"},
		{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: args},
		{Type: fantasy.StreamPartTypeToolInputEnd, ID: id},
		{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: "agent", ToolCallInput: args},
	}
}

// frameCalls is a step making every call in calls and finishing "tool-calls".
func frameCalls(calls ...[]fantasy.StreamPart) frameStep {
	return frameParts(append(calls, []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish,
		FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}}})...)
}

// frameRead is a step that reads path.
func frameRead(id, path string) frameStep {
	return frameParts(nativeToolStep(id, "read", `{"filePath":"`+path+`"}`))
}

// frameOpenText opens a text block and streams text into it, leaving it open:
// a child mid-answer.
func frameOpenText(text string) []fantasy.StreamPart {
	parts := nativeTextParts(text)
	return parts[:len(parts)-1]
}

// runNativeSubagentFrame runs keys against sess and returns the frame.
func runNativeSubagentFrame(t *testing.T, sess agent.Session, ws string, cols, rows int, keys string) string {
	t.Helper()
	got, _, err := RunFrameScript(Config{
		Session:   sess,
		Theme:     "tokyo-night",
		Workspace: ws,
		Yolo:      true,
	}, cols, rows, keys, FrameOpts{Timeout: 20 * time.Second, Freeze: true})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	return got
}

// oneRunningChild scripts a parent that fans out one child, which reads a file
// and is then held mid-answer.
func oneRunningChild(t *testing.T) (agent.Session, string) {
	t.Helper()
	ws := frameWorkspace(t)
	writeFrameFile(t, ws, "main.go", "package main\n")
	echo, two := newFrameRouter("test", "wire-echo"), newFrameRouter("test", "wire-two")
	const task = "Find every Go file and say what it does."
	echo.route("go", frameCalls(frameAgentCall("a1", "Scan the repo", task)))
	echo.route(task, frameRead("r1", "main.go"))
	echo.hold(task, frameOpenText("main.go is the entry point"))
	return frameSubagents(t, ws, echo, two, &frameClock{}, nil), ws
}

// TestFrameGoldenNativeSubagentRows80x24: a native child running shows in the
// row band as grok's does — its type, its description, its elapsed time and
// the tokens it has spent — and the parent's agent call is a running task row.
//
// The band is drawn from the snapshot, which runs ahead of the events the
// transcript is drawn from, so the frame waits for both: the task row's label,
// which only the call's second event carries, and the child's tokens, which
// are final once the child is held.
func TestFrameGoldenNativeSubagentRows80x24(t *testing.T) {
	sess, ws := oneRunningChild(t)
	got := runNativeSubagentFrame(t, sess, ws, 80, 24, "<wait:idle>go<enter><wait:text:Scan the repo  running><wait:text:15 tok>")
	assertGolden(t, "native-subagent-rows-80x24", 80, 24, got)
	for _, want := range []string{"○ general-purpose  Scan the repo  0s · 15 tok", "● agent  Scan the repo  running", "← 1 agent"} {
		if !strings.Contains(got, want) {
			t.Fatalf("frame is missing %q:\n%s", want, got)
		}
	}
}

// TestFrameGoldenNativeSubagentView100x30: the child view on a running native
// child is its own transcript as it streamed — its task as the user line, its
// read row, its answer so far — under the banner and the model chip grok's
// children get. The answer so far is the child's last event before it is
// held, so once it is drawn every event before it has been.
func TestFrameGoldenNativeSubagentView100x30(t *testing.T) {
	sess, ws := oneRunningChild(t)
	got := runNativeSubagentFrame(t, sess, ws, 100, 30,
		"<wait:idle>go<enter><wait:text:Scan the repo  running><wait:text:15 tok><down><enter>"+
			"<wait:text:main.go is the entry point><wait:text:15 tok>")
	assertGolden(t, "native-subagent-view-100x30", 100, 30, got)
	for _, want := range []string{
		"Find every Go file and say what it does.", // the child's task, its user line
		"✓ read  main.go",                          // its own tool row
		"main.go is the entry point",               // its answer so far
		"○ @general-purpose · read-only · esc to return",
		"(echo) Scan the repo", // the chip: the child's model and its task
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("frame is missing %q:\n%s", want, got)
		}
	}
}

// TestFrameGoldenNativeSubagentTwoView100x30 is the roadmap's exit criterion
// on screen (07-roadmap.md: a task fanned out to two children with both
// transcripts in the sub-agent view): two children running at once, the view
// on the first and Tab to the second, whose own transcript is drawn.
func TestFrameGoldenNativeSubagentTwoView100x30(t *testing.T) {
	ws := frameWorkspace(t)
	writeFrameFile(t, ws, "main.go", "package main\n")
	writeFrameFile(t, ws, "README.md", "# ws\n")
	echo, two := newFrameRouter("test", "wire-echo"), newFrameRouter("test", "wire-two")
	const first, second = "Read main.go and summarise it.", "Read README.md and summarise it."
	firstAsked := make(chan struct{})
	var once sync.Once
	echo.asked = func(prompt string) {
		if prompt == first {
			once.Do(func() { close(firstAsked) })
		}
	}
	echo.route("go", frameCalls(
		frameAgentCall("a1", "Summarise main", first),
		frameAgentCall("a2", "Summarise README", second, "model", "test/two")))
	echo.route(first, frameRead("r1", "main.go"))
	echo.hold(first, frameOpenText("main.go declares package main"))
	two.route(second, frameRead("r2", "README.md"))
	two.hold(second, frameOpenText("README.md is one heading"))
	sess := frameSubagents(t, ws, echo, two, &frameClock{}, firstAsked)

	got := runNativeSubagentFrame(t, sess, ws, 100, 30,
		"<wait:idle>go<enter><wait:text:Summarise README  running><wait:text:Summarise README  0s · 15 tok>"+
			"<wait:text:Summarise main  0s · 15 tok><down><enter><wait:text:main.go declares package main>"+
			"<tab><wait:text:README.md is one heading><wait:text:15 tok>")
	assertGolden(t, "native-subagent-two-view-100x30", 100, 30, got)
	for _, want := range []string{
		second, "✓ read  README.md", "README.md is one heading",
		"esc to return · tab next agent",
		"(two) Summarise README",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("frame is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "main.go declares package main") {
		t.Fatalf("tab should have left the first child's transcript:\n%s", got)
	}
}

// TestFrameGoldenNativeSubagentFail80x24: one child answers and the next fails
// mid-answer, one after the other; both rows linger in the band finished, and
// the parent's two finished task rows show `duration · model` — the model only
// for a receipt, which a native task row is once its call has finished
// (panel P15) — with the durations the parent's clock gives.
func TestFrameGoldenNativeSubagentFail80x24(t *testing.T) {
	ws := frameWorkspace(t)
	echo, two := newFrameRouter("test", "wire-echo"), newFrameRouter("test", "wire-two")
	clock := &frameClock{}
	echo.route("go",
		frameCalls(frameAgentCall("a1", "Scan the repo", "Scan it.")),
		frameCalls(frameAgentCall("a2", "Break it", "Break it.")),
		frameParts(nativeTextParts("one answered, one broke"), nativeFinishParts()))
	echo.route("Scan it.", clock.timed(1200*time.Millisecond, nativeTextParts("scanned"), nativeFinishParts()))
	echo.route("Break it.", clock.timed(800*time.Millisecond, frameOpenText("half an answer"),
		[]fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: errors.New("the provider went away")}}))
	sess := frameSubagents(t, ws, echo, two, clock, nil)

	got := runNativeSubagentFrame(t, sess, ws, 80, 24, "<wait:idle>go<enter><wait:text:one answered, one broke><wait:idle>")
	assertGolden(t, "native-subagent-fail-80x24", 80, 24, got)
	for _, want := range []string{
		"✓ agent  Scan the repo  1.2s · echo",
		"✗ agent  Break it  0.8s · echo",
		"✓ general-purpose  Scan the repo  1.2s · echo",
		"✗ general-purpose  Break it  0.8s · echo",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("frame is missing %q:\n%s", want, got)
		}
	}
}
