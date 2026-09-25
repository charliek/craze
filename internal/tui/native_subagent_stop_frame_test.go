package tui

import (
	"context"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
)

// Stopping one native sub-agent on screen (plan 026 §3.10, A12, V9): the stop
// key on a focused running row and in a running child's view, through the real
// engine and a real native session — agent.NewNative, the harness's runner and
// real child sessions — over the frame helpers native_subagent_frame_test.go
// built for PR 1's goldens. The key's gate and its fall-through are
// subcancel_test.go's; these are the frames.

// untilStopped is a step that yields before and then holds until its context
// ends — the child stopped — and closes stopped as it returns, however it
// returns: the moment the rows golden's second child answers from.
func untilStopped(stopped chan<- struct{}, before []fantasy.StreamPart) frameStep {
	return func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		defer close(stopped)
		for _, p := range before {
			if !yield(p) {
				return
			}
		}
		<-ctx.Done()
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
	}
}

// afterStopped is a step that waits for stopped and then yields parts: a child
// that answers only once another has been stopped. Its own context ending
// first — the runner ending the session on a script that failed — ends it.
func afterStopped(stopped <-chan struct{}, parts ...[]fantasy.StreamPart) frameStep {
	return func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		select {
		case <-stopped:
		case <-ctx.Done():
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
			return
		}
		for _, p := range cat(parts...) {
			if !yield(p) {
				return
			}
		}
	}
}

// TestFrameGoldenNativeSubagentStopRows80x24: two native children running at
// once, the rows focused on the first, and Backspace. Only the first stops —
// its row turns – (cancelled), the keyboard still on it — while the second,
// which answers only once the first's stream has been cancelled, completes; the
// parent's turn goes on to its own answer, and both agent task rows show how
// their child ended.
//
// The two children spawn in a fixed order, as in PR 1's two-child golden: the
// second's model client is handed out only once the first has sent its request
// (frameSubagents' gate), so by the time the second's row is drawn the first is
// in its held step and the stop cancels that step's stream.
func TestFrameGoldenNativeSubagentStopRows80x24(t *testing.T) {
	ws := frameWorkspace(t)
	echo, two := newFrameRouter("test", "wire-echo"), newFrameRouter("test", "wire-two")
	const first, second = "Look for TODO comments.", "Summarise the README."
	firstAsked := make(chan struct{})
	var once sync.Once
	echo.asked = func(prompt string) {
		if prompt == first {
			once.Do(func() { close(firstAsked) })
		}
	}
	stopped := make(chan struct{})
	echo.route("go",
		frameCalls(frameAgentCall("a1", "Find TODOs", first), frameAgentCall("a2", "Summarise README", second, "model", "test/two")),
		frameParts(nativeTextParts("one stopped, one answered"), nativeFinishParts()))
	echo.route(first, untilStopped(stopped, frameOpenText("two TODOs so far")))
	two.route(second, afterStopped(stopped, nativeTextParts("README.md is one heading"), nativeFinishParts()))
	sess := frameSubagents(t, ws, echo, two, &frameClock{}, firstAsked)

	got := runNativeSubagentFrame(t, sess, ws, 80, 24,
		"<wait:idle>go<enter><wait:text:Summarise README  running><wait:text:Find TODOs  0s>"+
			"<wait:text:Summarise README  0s><down><backspace><wait:text:one stopped, one answered><wait:idle>")
	assertGolden(t, "native-subagent-stop-rows-80x24", 80, 24, got)
	for _, want := range []string{
		"❯ – general-purpose  Find TODOs", // the stopped row, the keyboard still on it
		"  ✓ general-purpose  Summarise README",
		"– agent  Find TODOs  echo", // the task rows, each ended as its child did
		"✓ agent  Summarise README  two",
		"one stopped, one answered",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("frame is missing %q:\n%s", want, got)
		}
	}
}

// TestFrameGoldenNativeSubagentStopView100x30: in a running native child's
// view the banner offers the stop — `· del to stop` at its end, waited for
// before the key — and Delete stops that child. The view stays open on it and
// its banner turns to the finished child's: cancelled, and saying the user
// stopped it. The parent's turn goes on to its answer behind the view.
func TestFrameGoldenNativeSubagentStopView100x30(t *testing.T) {
	ws := frameWorkspace(t)
	writeFrameFile(t, ws, "main.go", "package main\n")
	echo, two := newFrameRouter("test", "wire-echo"), newFrameRouter("test", "wire-two")
	const task = "Find every Go file and say what it does."
	echo.route("go", frameCalls(frameAgentCall("a1", "Scan the repo", task)),
		frameParts(nativeTextParts("the scan was stopped"), nativeFinishParts()))
	echo.route(task, frameRead("r1", "main.go"))
	echo.hold(task, frameOpenText("main.go is the entry point"))
	sess := frameSubagents(t, ws, echo, two, &frameClock{}, nil)

	got := runNativeSubagentFrame(t, sess, ws, 100, 30,
		"<wait:idle>go<enter><wait:text:Scan the repo  running><wait:text:15 tok><down><enter>"+
			"<wait:text:main.go is the entry point><wait:text:esc to return · del to stop><delete>"+
			"<wait:text:stopped by the user><wait:idle>")
	assertGolden(t, "native-subagent-stop-view-100x30", 100, 30, got)
	for _, want := range []string{
		task,                         // the child's task, its user line
		"main.go is the entry point", // what it had streamed
		"– @general-purpose · cancelled · stopped by the user · esc to return",
		"(echo) Scan the repo", // the chip: still this child's view
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("frame is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "del to stop") {
		t.Fatalf("a finished child's banner offers the stop:\n%s", got)
	}
}
