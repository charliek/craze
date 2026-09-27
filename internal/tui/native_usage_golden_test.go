package tui

import (
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// nativeUsageFinish is a step's clean finish reporting what its request cost:
// input (uncached), cache reads and output tokens.
func nativeUsageFinish(input, cacheRead, output int64) []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop,
		Usage: fantasy.Usage{InputTokens: input, CacheReadTokens: cacheRead, OutputTokens: output,
			TotalTokens: input + cacheRead + output}}}
}

// TestFrameGoldenNativeUsage80x24 (plan 028 §3.14, §7 A33) is a priced native
// session's status row: two turns on a model the table prices — $3 per
// million input tokens, $15 per million output, $0.30 per million cache
// reads — with a 200k window. Row 1 then reads the context's share of the
// window and what the last turn and the whole session cost:
//
//   - the context the next request sends is the last step's reported usage,
//     8,000 + 58,000 cached + 2,000 out = 68,000 of 200,000: 34%;
//   - the second turn: 8,000 × $3/M + 58,000 × $0.30/M + 2,000 × $15/M =
//     $0.0714, so $0.07;
//   - the session: that and the first turn's 60,000 × $3/M + 1,500 × $15/M =
//     $0.2025, $0.2739 in all, so $0.27.
//
// The session and its scripted model are built inside the builder, fresh per
// call, and the call count is asserted against the model this run built: a
// both-gate-modes wrapper can call build() once per mode (S2's
// runFrameModes seam, as TestFrameGoldenNativeCompaction100x30's).
func TestFrameGoldenNativeUsage80x24(t *testing.T) {
	isolateSkillsHome(t)
	ws := frameWorkspace(t)

	var model *nativeScriptedModel
	build := func() Config {
		table := nativeOneModelTable()
		echo := table.Models["test/echo"]
		echo.ContextWindow = 200_000
		in, out, cached := 3.0, 15.0, 0.30
		echo.Cost = &modeltable.Cost{Input: &in, Output: &out, CacheRead: &cached}
		table.Models["test/echo"] = echo
		model = &nativeScriptedModel{provider: "test", wire: "wire-echo"}
		model.steps = [][]fantasy.StreamPart{
			cat(nativeTextParts("the first answer"), nativeUsageFinish(60_000, 0, 1_500)),
			cat(nativeTextParts("the second answer"), nativeUsageFinish(8_000, 58_000, 2_000)),
		}
		sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()},
			nativeSessionTweak(t.TempDir(), table, model))
		return Config{Session: sess, Theme: "tokyo-night", Workspace: ws, Yolo: true}
	}

	got, _, err := RunFrameScript(build(), 80, 24,
		"<wait:idle>hello<enter><wait:text:the first answer><wait:idle>again<enter><wait:text:the second answer><wait:idle>",
		FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "native-usage-80x24", 80, 24, got,
		[]string{"the first answer", "the second answer", "ws │ native │ Echo │ 34% ctx · $0.07 / $0.27 │ 0m"},
		[]string{" tok"})
	if model.calls != 2 {
		t.Fatalf("the session sent %d requests; want one per turn", model.calls)
	}
}
