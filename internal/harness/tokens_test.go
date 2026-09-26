package harness

import (
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/store"
)

// TestTokenEstimates (plan 028 §3.7): bytes/4, rounded up; a message weighs
// the bytes of its JSON as the transcript stores it, every part included.
func TestTokenEstimates(t *testing.T) {
	for n, want := range map[int]int64{0: 0, 1: 1, 3: 1, 4: 1, 5: 2, 8: 2, 4001: 1001} {
		if got := tokensOf(n); got != want {
			t.Errorf("tokensOf(%d) = %d, want %d", n, got, want)
		}
	}
	if got := textTokens(strings.Repeat("x", 10)); got != 3 {
		t.Errorf("textTokens(10 bytes) = %d, want 3", got)
	}
	call := fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.ReasoningPart{Text: strings.Repeat("r", 400)},
		fantasy.TextPart{Text: "reading"},
		fantasy.ToolCallPart{ToolCallID: "t1.1.1", ToolName: "read", Input: `{"filePath":"a.go"}`},
	}}
	b, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := messageTokens(call), tokensOf(len(b)); got != want || got < 100 {
		t.Fatalf("messageTokens = %d, want %d: the JSON's bytes, reasoning included", got, want)
	}
}

// TestTheTailBudget (plan 028 §3.9, PD23): tail_tokens, capped at a quarter
// of the threshold; with an unknown window (threshold 0), tail_tokens alone;
// and 0 is no tail.
func TestTheTailBudget(t *testing.T) {
	for _, tc := range []struct{ tail, threshold, want int64 }{
		{20000, 850000, 20000},
		{20000, 51000, 12750},
		{20000, 80000, 20000},
		{20000, 0, 20000},
		{0, 850000, 0},
		{0, 0, 0},
	} {
		if got := tailBudget(tc.tail, tc.threshold); got != tc.want {
			t.Errorf("tailBudget(%d, %d) = %d, want %d", tc.tail, tc.threshold, got, tc.want)
		}
	}
}

// TestTheCutWeighsStepsAsSent: the harness's estimator and budget with the
// store's cut, on a session's own transcript. Two turns of about 250 tokens
// each and a third of about 2,500: a budget that fits the first two but not
// the third keeps no tail (the newest step is over it); one that fits the
// newest two keeps them, starting at the second turn's prompt.
func TestTheCutWeighsStepsAsSent(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.models["test/a"].push(answerWith(strings.Repeat("a", 1000)), answerWith(strings.Repeat("b", 1000)), answerWith(strings.Repeat("c", 10000)))
	run(t, s, "q1")
	run(t, s, "q2")
	run(t, s, "q3")
	steps := s.store.Steps(s.cur.id())
	if len(steps) != 3 {
		t.Fatalf("%d steps, want 3", len(steps))
	}
	var weights []int64
	for _, st := range steps {
		n := int64(0)
		for _, m := range st.Messages {
			n += messageTokens(m)
		}
		weights = append(weights, n)
	}
	if weights[2] < 2500 || weights[1] > 300 {
		t.Fatalf("step weights %v; want the third over 2,500 and the others about 250", weights)
	}
	if got := store.Cut(steps, tailBudget(20000, 4*(weights[0]+weights[1])), messageTokens); got != 3 {
		t.Fatalf("with the newest step over the budget the cut is %d, want 3 (no tail)", got)
	}
	if got := store.Cut(steps, tailBudget(weights[1]+weights[2], 0), messageTokens); got != 1 || steps[1].First == "" {
		t.Fatalf("a budget of the newest two steps cuts at %d, want 1", got)
	}
}
