package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/transcript"
)

// TestAttachProbeHiddenFromHelp is the hard stop's own claim: the flag is
// hidden, and never documented.
func TestAttachProbeHiddenFromHelp(t *testing.T) {
	isolateRunEnv(t)
	var stdout, stderr strings.Builder
	cmd := NewRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"prompt", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt --help: %v\nstderr: %s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), "attach-probe") {
		t.Fatalf("--attach-probe leaked into --help:\n%s", stdout.String())
	}
}

// TestAttachProbeInertWithoutTheFlag is the Tests section's own claim: with
// the flag absent, no file and the same stdout as today. It runs the same
// tool-using, two-turn script with and without --attach-probe and holds
// stdout to be byte-identical, so the flag's presence — and the extra
// goroutine, fold and Attach it drives — never reaches the command's own
// output.
func TestAttachProbeInertWithoutTheFlag(t *testing.T) {
	without := runPromptStdout(t, "tasks", []string{"--follow-up", "two"}, "one")

	probePath := filepath.Join(t.TempDir(), "probe.txt")
	with := runPromptStdout(t, "tasks", []string{"--follow-up", "two", "--attach-probe", probePath}, "one")

	if without != with {
		t.Fatalf("--attach-probe changed stdout:\nwithout:\n%s\nwith:\n%s", without, with)
	}
	if _, err := os.Stat(probePath); err != nil {
		t.Fatalf("the probe file was not written: %v", err)
	}
}

// TestAttachProbeSameOverToolsAndFollowUp is the Tests section's happy path:
// a tool-using script (two tool_calls per turn) with at least one follow-up
// (a second turn, queued before the first) drives the probe's whole
// lifecycle — the attached goroutine started on the first EventText, folded
// through both turns, compared at the chain's end — and the probe file must
// say SAME.
func TestAttachProbeSameOverToolsAndFollowUp(t *testing.T) {
	probePath := filepath.Join(t.TempDir(), "probe.txt")
	runPromptStdout(t, "tasks", []string{"--follow-up", "two", "--attach-probe", probePath}, "one")

	body, err := os.ReadFile(probePath)
	if err != nil {
		t.Fatalf("the probe file: %v", err)
	}
	lines := strings.SplitN(string(body), "\n", 2)
	if lines[0] != "SAME" {
		t.Fatalf("the probe file's first line = %q, want SAME:\n%s", lines[0], body)
	}
	if !strings.Contains(string(body), "--- first, at seq ") || !strings.Contains(string(body), "--- attached ---") {
		t.Fatalf("the probe file is missing its projections:\n%s", body)
	}
	retained := regexp.MustCompile(`(?m)^retained: main \d+ entries \d+ bytes; subs \d+ / \d+ entries / \d+ bytes$`)
	if !retained.MatchString(string(body)) {
		t.Fatalf("the probe file is missing its retained-size line:\n%s", body)
	}
}

// TestDiffProbeModelsDetectsADifference is the comparison logic's own unit
// test (the Tests section): two hand-folded models built the same way must
// compare SAME, and two that differ must compare DIFF, with no fake agent or
// engine involved.
func TestDiffProbeModelsDetectsADifference(t *testing.T) {
	build := func(text string) *transcript.Model {
		m := transcript.New(transcript.Options{})
		m.Fold(agent.Event{Seq: 1, Type: agent.EventText, Text: text})
		m.Fold(agent.Event{Seq: 2, Type: agent.EventDone, StopReason: "end_turn"})
		return m
	}

	if d := diffProbeModels(build("hello"), build("hello")); d != "" {
		t.Fatalf("two identical folds compared DIFF: %s", d)
	}
	d := diffProbeModels(build("hello"), build("goodbye"))
	if d == "" {
		t.Fatal("two folds with different text compared SAME, want a difference")
	}
}

// TestDiffProbeModelsCanonicalisesErrors is X14's own rule, unit-tested: a
// primary-style fold (Options.ErrText, the publisher's own error value held
// on the entry) and an attached-style fold (no ErrText, an *agent.RemoteError
// already) of the SAME error must compare SAME once the codec's own
// representation — the error's concrete type — is set aside.
func TestDiffProbeModelsCanonicalisesErrors(t *testing.T) {
	primary := transcript.New(transcript.Options{ErrText: func(e error) string { return e.Error() }})
	primary.Fold(agent.Event{Seq: 1, Type: agent.EventError, Err: errBoom{}})

	attached := transcript.New(transcript.Options{})
	attached.Fold(agent.Event{Seq: 1, Type: agent.EventError, Err: agent.RemoteErrorOf(errBoom{})})

	if d := diffProbeModels(primary, attached); d != "" {
		t.Fatalf("a primary fold and a decoded fold of the same error compared DIFF: %s", d)
	}
}

// errBoom is a plain error value with no exported fields, the shape a
// publisher's own synthetic error can take on the primary (never an
// *agent.RemoteError, X14) and which agent.RemoteErrorOf must still
// canonicalise for a comparison against a decoded one.
type errBoom struct{}

func (errBoom) Error() string { return "boom" }
