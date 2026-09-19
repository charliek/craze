package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite internal/harness/testdata/system_prompt.golden")

const systemGolden = "testdata/system_prompt.golden"

// TestSystemPromptGolden pins the prompt's text: a change to it changes
// every session's prompt-cache prefix, so it should be deliberate.
// Regenerate with:
//
//	go test ./internal/harness -run TestSystemPromptGolden -update
func TestSystemPromptGolden(t *testing.T) {
	got := systemPrompt("/home/user/project", "linux")
	if *updateGolden {
		if err := os.WriteFile(systemGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(systemGolden)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./internal/harness -run TestSystemPromptGolden -update)", err)
	}
	if got != string(want) {
		t.Fatalf("system prompt differs from %s\n--- want ---\n%s\n--- got ---\n%s", systemGolden, want, got)
	}
}

// TestSystemPromptIsFrozen: two sessions on one workspace, opened at
// different times, send byte-identical prompts, which name the workspace
// and this OS; and a turn's transcript header records the prompt's hash and
// craze's version.
func TestSystemPromptIsFrozen(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	first := f.open(f.options())
	later := f.options()
	later.Now = func() time.Time { return testNow().Add(36 * time.Hour) }
	second := f.open(later)
	if first.system != second.system {
		t.Fatalf("two sessions built different prompts:\n%s\n---\n%s", first.system, second.system)
	}
	if first.system != systemPrompt(f.workspace, runtime.GOOS) {
		t.Fatalf("Open's prompt is not systemPrompt(workspace, GOOS):\n%s", first.system)
	}
	for _, want := range []string{"- Working directory: " + f.workspace + "\n", "- Operating system: " + runtime.GOOS + "\n"} {
		if !strings.Contains(first.system, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}

	f.models["test/a"].push(answerWith("hello"), answerWith("again"))
	run(t, first, "hi")
	run(t, first, "hi again")
	for i, call := range f.models["test/a"].requests() {
		if got := promptOf(call)[0]; got != "system: "+first.system {
			t.Fatalf("request %d's first message is %q, want the frozen system prompt", i+1, got)
		}
	}
	h := transcript(t, first).Header
	sum := sha256.Sum256([]byte(first.system))
	if h.SystemPromptSHA256 != hex.EncodeToString(sum[:]) || h.CrazeVersion != "v0.0.0-test" {
		t.Fatalf("header = %+v; want the prompt's SHA-256 %x and version v0.0.0-test", h, sum)
	}
}
