package cli

import (
	"encoding/json"
	"testing"
)

// The complete ordered `--json` stream of every fixture that opens a blocking
// request, pinned as literals (plan 021 A13).
//
// The ask registry changed who opens, answers and ends a blocking request, and
// the point of S1b is that `craze prompt --json` is byte-identical to what it
// was apart from the `seq` key's value. The whole-stream assertions elsewhere
// in this package pick particular lines; these pin the ORDER and the CONTENT of
// every line, which is what an auto-answered question or plan can lose — its
// line is published by the handler before the decision goes back to the agent,
// and the text the agent writes once it has been answered must follow it.
//
// **The literals below were taken from the BASELINE binary**, built from
// `6581e0a` before any of this plan landed
// (`021-session-control-s1b-engine/v2/craze-baseline-6581e0a` with its own
// fake agent), with `seq` stripped exactly as the pytest suite's `without_seq`
// strips it. They are not regenerated from this tree: a difference here is a
// regression, and the two versions are shown rather than the file updated.
var askJSONStreams = map[string]struct {
	script   string
	provider string
	text     string
	flags    []string
	lines    []string
}{
	// A non-interactive question: one Auto opening carrying the answers craze
	// sent, then the agent's reply to that answer, then the turn's ending.
	"ask": {
		script: "ask", text: "q",
		lines: []string{
			`{"answers":{"q1":["opt-a"],"q2":["opt-x"]},"auto":true,"id":"ask-1","title":"Question","type":"question"}`,
			`{"text":"asked:answered:q1=opt-a;q2=opt-x","type":"text"}`,
			`{"stopReason":"end_turn","type":"done"}`,
		},
	},
	// A non-interactive plan: the same shape, accepted.
	"plan": {
		script: "plan", text: "go",
		lines: []string{
			`{"accepted":true,"auto":true,"id":"plan-1","name":"Fake Plan","type":"plan"}`,
			`{"text":"planned:accepted","type":"text"}`,
			`{"stopReason":"end_turn","type":"done"}`,
		},
	},
	// grok's own ask dialect, which answers by prompt text rather than by
	// option id.
	"grok-ask": {
		script: "grok-ask", provider: "grok", text: "q",
		lines: []string{
			`{"answers":{"Pick any":["X"],"Pick one":["A"]},"auto":true,"id":"ask-1","type":"question"}`,
			`{"text":"asked:accepted:Pick one=A;Pick any=X","type":"text"}`,
			`{"stopReason":"end_turn","type":"done"}`,
		},
	},
	// A permission answered from --permission-decision: the opening is
	// published, the run answers it, and the agent says which option it got.
	"permission": {
		script: "permission", text: "go",
		flags: []string{"--no-force", "--permission-decision", "allow-once"},
		lines: []string{
			`{"optionIds":["opt-always","opt-once","opt-reject"],"tool":"Shell","type":"permission"}`,
			`{"text":"decision:opt-once","type":"text"}`,
			`{"stopReason":"end_turn","type":"done"}`,
		},
	},
}

func TestAskFixturesJSONStreamsAreUnchanged(t *testing.T) {
	for name, want := range askJSONStreams {
		t.Run(name, func(t *testing.T) {
			flags := append([]string{"--json"}, want.flags...)
			if want.provider != "" {
				flags = append(flags, "--provider", want.provider)
			}
			got := withoutSeq(t, runPromptStdout(t, want.script, flags, want.text))
			if len(got) != len(want.lines) {
				t.Fatalf("the stream is %d lines, want %d:\ngot:  %v\nwant: %v", len(got), len(want.lines), got, want.lines)
			}
			for i, line := range got {
				if line != want.lines[i] {
					t.Fatalf("line %d moved:\ngot:  %s\nwant: %s", i+1, line, want.lines[i])
				}
			}
		})
	}
}

// withoutSeq is one --json stream as a list of lines with the seq key dropped
// and the remaining keys in a canonical order: the same normalisation the
// pytest suite's without_seq applies, so a literal taken from one binary can be
// compared against another's. Only seq's VALUE is allowed to differ between
// the baseline and this tree; every other byte is the contract.
func withoutSeq(t *testing.T, stdout string) []string {
	t.Helper()
	var out []string
	for _, ev := range parseJSONLines(t, stdout) {
		m := make(map[string]any, len(ev.m))
		for k, v := range ev.m {
			if k == "seq" {
				continue
			}
			m[k] = v
		}
		// encoding/json sorts a map's keys and writes no spaces, which is what
		// the literals above were canonicalised with.
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(raw))
	}
	return out
}
