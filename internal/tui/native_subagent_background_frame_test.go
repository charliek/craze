package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// Background sub-agents on screen (plan 026 §3.11, A14): an interactive native
// session whose parent starts a child with run_in_background, the row band
// marking the child `bg` (capability-gated: native's rows alone), and the wake
// — the session's own turn delivering the child's result once the user's turn
// has ended — headed in the transcript by its note. The harness and the
// frame runner are native_subagent_frame_test.go's; what is new here is
// Options.Interactive, which is what turns background children on, and a
// call whose run_in_background is the boolean the tool declares.

// frameBackground is a native session over frameTable with background
// children on: every alias on the echo router, its clock c.
func frameBackground(t *testing.T, ws string, echo *frameRouter, c *frameClock) agent.Session {
	t.Helper()
	home := t.TempDir()
	table := frameTable()
	return agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir(), Interactive: true}, func(o *harness.Options) {
		o.Home = home
		o.Table = table
		o.Getenv = func(k string) string {
			if k == "NATIVE_TUI_TEST_KEY" {
				return "test-key"
			}
			return ""
		}
		o.NewModel = func(modeltable.Resolved) (fantasy.LanguageModel, error) { return echo, nil }
		o.Now = c.now
	})
}

// frameBackgroundCall is one agent call asking for the background.
func frameBackgroundCall(id, description, prompt string) []fantasy.StreamPart {
	args := fmt.Sprintf(`{"description":%q,"prompt":%q,"run_in_background":true}`, description, prompt)
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: "agent"},
		{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: args},
		{Type: fantasy.StreamPartTypeToolInputEnd, ID: id},
		{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: "agent", ToolCallInput: args},
	}
}

// thenClose is step followed by closing done: the parent's last step, which
// gates the child's answer so the result is delivered by a wake and never by
// the parent's own turn.
func thenClose(step frameStep, done chan struct{}) frameStep {
	return func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		step(ctx, yield)
		close(done)
	}
}

// after is step once gate has closed, or the stream's error if the context
// ends first (the session closing).
func after(gate <-chan struct{}, step frameStep) frameStep {
	return func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		select {
		case <-gate:
		case <-ctx.Done():
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
			return
		}
		step(ctx, yield)
	}
}

// TestFrameGoldenNativeSubagentBackgroundRows80x24: a background child running
// after the parent's turn has ended. The row band marks it `bg` beside its
// elapsed time and tokens; the parent's agent row is a finished task row
// already — final at the acknowledgement, the child's model on it as its
// receipt — and the parent has said its piece and gone idle while the child
// works on.
func TestFrameGoldenNativeSubagentBackgroundRows80x24(t *testing.T) {
	ws := frameWorkspace(t)
	writeFrameFile(t, ws, "main.go", "package main\n")
	echo := newFrameRouter("test", "wire-echo")
	const task = "Find every Go file and say what it does."
	echo.route("go", frameCalls(frameBackgroundCall("a1", "Scan the repo", task)),
		frameParts(nativeTextParts("started the scan in the background"), nativeFinishParts()))
	echo.route(task, frameRead("r1", "main.go"))
	echo.hold(task, frameOpenText("main.go is the entry point"))
	sess := frameBackground(t, ws, echo, &frameClock{})

	got := runNativeSubagentFrame(t, sess, ws, 80, 24,
		"<wait:idle>go<enter><wait:text:started the scan in the background><wait:idle><wait:text:15 tok>")
	assertGolden(t, "native-subagent-background-rows-80x24", 80, 24, got)
	for _, want := range []string{
		"○ general-purpose  Scan the repo  bg · 0s · 15 tok",
		"✓ agent  Scan the repo  echo",
		"started the scan in the background",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("frame is missing %q:\n%s", want, got)
		}
	}
}

// TestFrameGoldenNativeSubagentBackgroundWake100x30: the child finishes after
// the parent's turn ended, and the session wakes — its note heads the wake's
// reply in the transcript, under the parent's own — then the user's next
// prompt, typed as the wake ran or just after it, runs after the wake and is
// answered; the child's row lingers finished, still marked `bg`. The status
// row never flips to working for the wake (§3.11: grok's behaviour, kept).
func TestFrameGoldenNativeSubagentBackgroundWake100x30(t *testing.T) {
	ws := frameWorkspace(t)
	writeFrameFile(t, ws, "main.go", "package main\n")
	echo := newFrameRouter("test", "wire-echo")
	const task = "Find every Go file and say what it does."
	parentDone := make(chan struct{})
	echo.route("go",
		frameCalls(frameBackgroundCall("a1", "Scan the repo", task)),
		thenClose(frameParts(nativeTextParts("started the scan in the background"), nativeFinishParts()), parentDone))
	echo.route(task, frameRead("r1", "main.go"),
		after(parentDone, frameParts(nativeTextParts("main.go is the entry point"), nativeFinishParts())))
	// The wake: the session's own turn, whose request ends with the result.
	echo.routeWake(frameParts(nativeTextParts("the scan is in: main.go is the entry point, and that is the whole program"), nativeFinishParts()))
	echo.route("thanks", frameParts(nativeTextParts("you are welcome"), nativeFinishParts()))
	sess := frameBackground(t, ws, echo, &frameClock{})

	got := runNativeSubagentFrame(t, sess, ws, 100, 30,
		"<wait:idle>go<enter><wait:text:started the scan in the background><wait:idle>"+
			"<wait:text:the agent continues><wait:text:the whole program>thanks<enter><wait:text:you are welcome><wait:idle>")
	assertGolden(t, "native-subagent-background-wake-100x30", 100, 30, got)
	for _, want := range []string{
		"started the scan in the background",
		"sub-agent finished — the agent continues",
		"the scan is in: main.go is the entry point, and that is the whole program",
		"thanks",
		"you are welcome",
		"✓ general-purpose  Scan the repo  bg · echo",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("frame is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "interjection fallback") {
		t.Fatalf("the wake's note has the fallback wording:\n%s", got)
	}
}
