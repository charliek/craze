package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// runReplayStubFrame is runStubFrame for a loaded session: the stub hands its
// Replay back between the two EventReplay phases exactly as the live session's
// session/load path does, and Config.Loading is what makes the model start in
// the restoring state (§3.5).
func runReplayStubFrame(t *testing.T, cols, rows int, script string, replay []agent.Event) string {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	stub.Replay = replay
	plain, _, err := RunFrameScript(Config{
		Session:   stub,
		Theme:     "tokyo-night",
		Workspace: frameWorkspace(t),
		Model:     "grok",
		Yolo:      true,
		Loading:   true,
	}, cols, rows, script, FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run replay frame script: %v", err)
	}
	return plain
}

// TestFrameGoldenReplay is what a --continue looks like once it has finished:
// the restored user block, thought, tool row and reply, the `restored` note
// that marks the seam, and an idle composer under it.
//
// The script waits on the note and never on <start>, which fires on Start
// returning alone — mid-replay, in every live session (§3.5).
func TestFrameGoldenReplay(t *testing.T) {
	got := runReplayStubFrame(t, 100, 30, "<wait:text:restored><wait:idle>", []agent.Event{
		{Type: agent.EventUser, Text: "List the files in the current working directory in one line."},
		{Type: agent.EventThought, Text: "Recalling the earlier turn."},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "replay-0-1", Title: "List Directory", Kind: "read", Status: "completed"}},
		{Type: agent.EventText, Text: "the workspace holds main.py and README.md"},
	})
	assertFrameGolden(t, "replay-100x30", 100, 30, got,
		[]string{
			"List the files in the current working directory in one line.",
			// The thought is collapsed, as every finished thought is; what
			// matters is that it is there and that the note below it is not
			// inside it.
			"+ Thought", "List Directory",
			"the workspace holds main.py and README.md", "restored",
		},
		// The gate is open again, so neither half of the not-yet-up row is
		// still on screen.
		[]string{"restoring", "starting"})
}

// TestFrameGoldenRename is §3.6 end to end: the title the user gave the session
// is on the composer rule and the note says what happened.
func TestFrameGoldenRename(t *testing.T) {
	got := runStubFrame(t, 100, 30, "<wait:idle>/rename fix the flaky pty test<enter><wait:text:renamed>")
	assertFrameGolden(t, "rename-100x30", 100, 30, got,
		[]string{"renamed to fix the flaky pty test", "fix the flaky pty test ─"},
		// The draft is consumed, so the menu it opened is gone with it.
		[]string{"❯ /rename"})
}

// TestFrameGoldenLoadLongReplayDoesNotDeadlock is the regression for §2.2(a),
// and it can only be a frame test: an Update-driven test hands the model one
// event at a time and never fills a channel, which is the whole failure.
//
// `load-long` replays 600 message chunks before it answers session/load. With
// the event reader armed by startedMsg — where it was before §3.5 — the
// session's 256-slot channel fills, emitCtx blocks the ACP read loop, the load
// result is never read, Start never returns and even <start> times out. With it
// armed by Init the frame completes and ends on the `restored` note.
func TestFrameGoldenLoadLongReplayDoesNotDeadlock(t *testing.T) {
	got := runLoadFrame(t, "load-long", 100, 30, "<wait:text:restored><wait:idle>")
	assertFrameGolden(t, "load-long-100x30", 100, 30, got,
		[]string{"line 600", "restored"},
		[]string{"restoring", "starting"})
}

// TestFrameLoadReplaysTheWholeTranscript is the same path on cursor's own
// replay shape: a two-chunk user prompt the session coalesces into one block, a
// thought, a tool call completed by a second update, and the answer. No golden —
// the point is the content, and the frame is one more place for it to drift.
func TestFrameLoadReplaysTheWholeTranscript(t *testing.T) {
	got := runLoadFrame(t, "load", 100, 30, "<wait:text:restored><wait:idle>")
	for _, want := range []string{
		"List the files in the current working directory in one line.",
		"+ Thought", "✓ read  List Directory", "the workspace holds main.py and README.md",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("restored frame is missing %q:\n%s", want, got)
		}
	}
	// One user block, not one per wire chunk.
	if n := strings.Count(got, "List the files in the"); n != 1 {
		t.Fatalf("the replayed prompt drew %d blocks:\n%s", n, got)
	}
}

// runLoadFrame drives the real program against a real session/load: the whole
// protocol-to-transcript path a --continue takes, without the CLI flags that
// reach it (those are C5's). agent.Options.LoadSessionID and Config.Loading are
// exactly what runTUI will set.
func runLoadFrame(t *testing.T, script string, cols, rows int, keys string) string {
	t.Helper()
	bin := buildFakeAgent(t)
	isolateSkillsHome(t)
	ws := frameWorkspace(t)
	prov := agent.CursorProvider()
	sess := agent.New(agent.Options{
		Binary:        bin,
		ExtraArgs:     []string{"-script=" + script},
		Workspace:     ws,
		Force:         true,
		Interactive:   true,
		Stderr:        io.Discard,
		Provider:      &prov,
		LoadSessionID: "sess-load-1",
	})
	plain, _, err := RunFrameScript(Config{
		Session:        sess,
		Theme:          "tokyo-night",
		Workspace:      ws,
		Yolo:           true,
		Provider:       prov,
		ProviderLocked: true,
		Loading:        true,
	}, cols, rows, keys, FrameOpts{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("run %s frame: %v", script, err)
	}
	return plain
}
