package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
)

// TestFrameGoldenNativeEcho80x24 is the native adapter's own frame golden: a
// real native session (agent.NewNative, never tui.Stub) driven through
// RunFrameScript with a scripted fantasy.LanguageModel that streams thinking
// then text, so the golden pins that the native adapter's events reach the
// transcript exactly as the live session's do. There is no environment hook
// on the production path here — the scripted model reaches Start only
// through NewNative's own tweak seam (harness.Options.NewModel), which is
// nil in every real build.
//
// RunFrameScript's isolateFrameHome swaps HOME and unsets CRAZE_HOME for the
// run, which would leave paths.NativeDir() empty inside it; the tweak points
// the harness at harnessHome directly instead of relying on that isolated
// HOME (plan 018 §3.8's note to C10), so Start's real code path runs against
// a table this test controls regardless of where HOME points during the run.
func TestFrameGoldenNativeEcho80x24(t *testing.T) {
	table := nativeOneModelTable()
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.steps = [][]fantasy.StreamPart{
		cat(nativeThoughtParts("working it out"), nativeTextParts("hi there"), nativeFinishParts()),
	}
	harnessHome := t.TempDir()
	ws := frameWorkspace(t)
	sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()}, nativeSessionTweak(harnessHome, table, model))

	got, _, err := RunFrameScript(Config{
		Session:   sess,
		Theme:     "tokyo-night",
		Workspace: ws,
		Yolo:      true,
	}, 80, 24, "<wait:idle>hello<enter><wait:text:hi there><wait:idle>", FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertGolden(t, "native-echo-80x24", 80, 24, got)
	for _, want := range []string{"hi there", "+ Thought", "native"} {
		if !strings.Contains(got, want) {
			t.Fatalf("frame is missing %q:\n%s", want, got)
		}
	}
	// A collapsed thought never shows its own text (TestFrameGoldenMarkdown120x40
	// pins the same rule for a live session).
	if strings.Contains(got, "working it out") {
		t.Fatalf("a collapsed thought must not show its text:\n%s", got)
	}
}

func cat(parts ...[]fantasy.StreamPart) []fantasy.StreamPart {
	var all []fantasy.StreamPart
	for _, p := range parts {
		all = append(all, p...)
	}
	return all
}
