package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
)

// TestFrameGoldenNativeTools80x24 is plan 019 §3.10's proof that the native
// adapter's tool rows need nothing new from the TUI: a real native session
// (agent.NewNative, never tui.Stub) runs the harness's real tools in a
// temporary workspace, and the frame is drawn by exactly the code the ACP
// path draws with — an expanded read showing its content, an edit showing
// its diff, a command that passed, one that exited non-zero showing the
// stderr head a collapsed row previews, and a search row. Nothing in
// internal/tui changed for any of it.
//
// It needs ripgrep for the search row, so it follows the repo's rule for a
// test that does (plan 019 §3.9): skip on a laptop without rg, fail in CI,
// which installs it and sets CRAZE_REQUIRE_RG.
func TestFrameGoldenNativeTools80x24(t *testing.T) {
	requireFrameRG(t)
	ws := frameWorkspace(t)
	writeFrameFile(t, ws, "main.go", "package main\n\n// TODO: ship it\n")
	writeFrameFile(t, ws, "notes.txt", "alpha\n")

	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.steps = [][]fantasy.StreamPart{
		nativeToolStep("c1", "read", `{"filePath":"main.go"}`),
		nativeToolStep("c2", "edit", `{"filePath":"notes.txt","oldString":"alpha","newString":"beta"}`),
		nativeToolStep("c3", "bash", `{"command":"echo hi"}`),
		nativeToolStep("c4", "bash", `{"command":"echo boom >&2; exit 3"}`),
		nativeToolStep("c5", "grep", `{"pattern":"TODO","path":"."}`),
		cat(nativeTextParts("done tools"), nativeFinishParts()),
	}
	sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()}, nativeSessionTweak(t.TempDir(), nativeOneModelTable(), model))

	got, _, err := RunFrameScript(Config{
		Session:   sess,
		Theme:     "tokyo-night",
		Workspace: ws,
		Yolo:      true,
	}, 80, 24, "<wait:idle>go<enter><wait:text:done tools><wait:idle><ctrl-o><wait:text:package main>",
		FrameOpts{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertGolden(t, "native-tools-80x24", 80, 24, got)
	for _, want := range []string{
		"✓ read  main.go",  // the row, and below it the file it read
		"// TODO: ship it", // Output.Content, which only an expanded read draws
		"✓ edit  notes.txt  +1 −1",
		"- alpha", // the diff the adapter computed from the edit
		"+ beta",
		"✓ bash  echo hi",
		"exit 3",              // the failing command's code
		"boom",                // its stderr head, from the merged stream
		"✓ search  TODO in .", // a search takes the transcript's other row
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("frame is missing %q:\n%s", want, got)
		}
	}
	// One row per call, and nothing left running.
	if n := strings.Count(got, "✓ bash"); n != 2 {
		t.Fatalf("got %d command rows, want the two that ran:\n%s", n, got)
	}
	for _, no := range []string{"◌ ", "⟳ "} {
		if strings.Contains(got, no) {
			t.Fatalf("a row is still running after the turn ended:\n%s", got)
		}
	}
}

// nativeToolStep is one scripted step that calls a tool and finishes
// "tool-calls", so the harness runs it and the turn goes on.
func nativeToolStep(id, name, input string) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: name},
		{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: input},
		{Type: fantasy.StreamPartTypeToolInputEnd, ID: id},
		{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: name, ToolCallInput: input},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls,
			Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}},
	}
}

func writeFrameFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// requireFrameRG is internal/harness/tool/opencode's rule for a test that
// needs ripgrep, in this package: skip without it, fail when CI says it must
// be there.
func requireFrameRG(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		if os.Getenv("CRAZE_REQUIRE_RG") == "1" {
			t.Fatalf("CRAZE_REQUIRE_RG=1, and ripgrep (rg) is not on PATH: %v", err)
		}
		t.Skipf("ripgrep (rg) is not on PATH (set CRAZE_REQUIRE_RG=1 to fail instead): %v", err)
	}
}
