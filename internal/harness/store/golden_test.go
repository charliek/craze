package store

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
)

var updateGolden = flag.Bool("update", false, "rewrite internal/harness/store/testdata/transcript.golden.jsonl")

const goldenPath = "testdata/transcript.golden.jsonl"

// The golden transcript's answer comes from Fantasy's real OpenAI-compatible
// provider, the one both of H1's drivers use, streaming this reasoning
// model's SSE from a local server: so the golden records the message and its
// parts exactly as that provider shapes them, and a Fantasy bump that
// changes the shape shows up as a golden diff.
const (
	goldenPrompt    = "What is 2+2? Answer in one word."
	goldenReasoning = "Simple arithmetic: 2 plus 2 is 4."
	goldenAnswer    = "Four."
)

func reasoningChunk(text string) string {
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":%q},"finish_reason":null}]}`, text)
}

func contentChunk(text string) string {
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, text)
}

// finishWithUsage reports usage the way OpenAI-compatible providers do:
// prompt_tokens includes the cached ones, and reasoning is a detail of the
// completion.
const finishWithUsage = `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1200,"completion_tokens":40,"total_tokens":1240,` +
	`"prompt_tokens_details":{"cached_tokens":1024},"completion_tokens_details":{"reasoning_tokens":25}}}`

// openAICompatStep runs one turn through Fantasy's agent on the real
// OpenAI-compatible provider and returns the finished step, as the turn
// runner's OnStepFinish receives it.
func openAICompatStep(t *testing.T) fantasy.StepResult {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			reasoningChunk("Simple arithmetic: "), reasoningChunk("2 plus 2 is 4."),
			contentChunk("Four"), contentChunk("."),
			finishWithUsage,
		} {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	p, err := openaicompat.New(openaicompat.WithBaseURL(srv.URL), openaicompat.WithAPIKey("sk-canary-not-a-secret"), openaicompat.WithName(kimi.Provider))
	if err != nil {
		t.Fatal(err)
	}
	lm, err := p.LanguageModel(context.Background(), kimi.WireModel)
	if err != nil {
		t.Fatal(err)
	}
	var steps []fantasy.StepResult
	_, err = fantasy.NewAgent(lm, fantasy.WithMaxRetries(0)).Stream(context.Background(), fantasy.AgentStreamCall{
		Prompt:       goldenPrompt,
		OnStepFinish: func(s fantasy.StepResult) error { steps = append(steps, s); return nil },
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(steps) != 1 || len(steps[0].Messages) != 1 {
		t.Fatalf("got %d steps (%+v), want one with one message", len(steps), steps)
	}
	return steps[0]
}

// TestTranscriptGolden writes a representative transcript with a fixed clock
// and fixed ids, compares its bytes to the golden, and reads it back: a
// reasoning answer with usage, a model and an effort switch, and an answer
// cut short by a cancel. Regenerate with:
//
//	go test ./internal/harness/store -run TestTranscriptGolden -update
func TestTranscriptGolden(t *testing.T) {
	step := openAICompatStep(t)
	if got := messageTexts(step.Messages); !reflect.DeepEqual(got, []string{
		"assistant: (thinking: " + goldenReasoning + ") " + goldenAnswer,
	}) {
		t.Fatalf("the provider's step is %q; the SSE fixture is not being read as intended", got)
	}

	s := newStore(t, testOptions(t))
	// Turn 1, at medium effort: the answer as OnStepFinish hands it over.
	if err := s.AppendUser(MessageEntry{Message: fantasy.NewUserMessage(goldenPrompt), Model: kimi, Effort: "medium"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAssistant(MessageEntry{
		Message: step.Messages[0], Model: kimi, Effort: "medium",
		Usage: UsageOf(step.Usage), StopReason: "end_turn",
	}); err != nil {
		t.Fatal(err)
	}
	// A switch to another provider's model at high effort, then a turn
	// cancelled mid-answer: the runner builds the message from the deltas it
	// accumulated.
	if err := s.AppendModelChange(minimax); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEffortChange("high"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUser(MessageEntry{Message: fantasy.NewUserMessage("Now prove it <briefly> & formally."), Model: minimax, Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAssistant(MessageEntry{
		Message: assistantMsg("Peano: define 2 as S(S(0)).", "By the Peano axioms, 2 + 2 = S(S(0)) + S(S(0))"),
		Model:   minimax, Effort: "high", StopReason: "cancelled", Interrupted: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Fatalf("%v (regenerate with: go test ./internal/harness/store -run TestTranscriptGolden -update)", err)
		}
		if string(got) != string(want) {
			t.Fatalf("transcript differs from %s\n--- want ---\n%s\n--- got ---\n%s", goldenPath, want, got)
		}
	}

	// Read the golden itself back, so the check covers the committed bytes.
	tr, err := Load(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Header != s.Header() {
		t.Fatalf("header read back as %+v, want %+v", tr.Header, s.Header())
	}
	if !reflect.DeepEqual(tr.Entries, s.t.Entries) {
		t.Fatalf("entries read back differ from those written:\n%+v\n%+v", tr.Entries, s.t.Entries)
	}
	// The first answer's parts, provider options included, are exactly the
	// provider's.
	if back := tr.Entries[1].Message; !reflect.DeepEqual(back, step.Messages[0]) {
		t.Fatalf("the provider's message did not survive the round trip:\n got %#v\nwant %#v", back, step.Messages[0])
	}
	if u := tr.Entries[1].Usage; u == nil || *u != (Usage{Input: 176, Output: 40, Reasoning: 25, CacheRead: 1024}) {
		t.Fatalf("usage read back as %+v", u)
	}
	if e := tr.Entries[5]; !e.Interrupted || e.StopReason != "cancelled" {
		t.Fatalf("the cut-short answer read back as %+v", e)
	}
	// Each model sees only its own reasoning.
	if got := messageTexts(tr.Context(minimax)); !reflect.DeepEqual(got, []string{
		"user: " + goldenPrompt,
		"assistant: " + goldenAnswer,
		"user: Now prove it <briefly> & formally.",
		"assistant: (thinking: Peano: define 2 as S(S(0)).) By the Peano axioms, 2 + 2 = S(S(0)) + S(S(0))",
	}) {
		t.Fatalf("context for minimax = %q", got)
	}
	if got := messageTexts(s.Context(kimi)); !reflect.DeepEqual(got, []string{
		"user: " + goldenPrompt,
		"assistant: (thinking: " + goldenReasoning + ") " + goldenAnswer,
		"user: Now prove it <briefly> & formally.",
		"assistant: By the Peano axioms, 2 + 2 = S(S(0)) + S(S(0))",
	}) {
		t.Fatalf("context for kimi = %q", got)
	}
}

// TestReasoningMetadataSurvives: the golden's provider attaches no metadata
// to reasoning, so this pins what matters for one that does (a signed or
// encrypted reasoning block, here the OpenAI Responses shape) and for part
// options on text: typed provider data survives the file and Context, value
// and type, because the payload is Fantasy's own JSON.
func TestReasoningMetadataSurvives(t *testing.T) {
	sealed := "gAAAA-sealed-reasoning"
	reasoning := fantasy.ReasoningPart{
		Text: "hidden chain",
		ProviderOptions: fantasy.ProviderOptions{
			openai.Name: &openai.ResponsesReasoningMetadata{ItemID: "rs_1", EncryptedContent: &sealed, Summary: []string{"s1", "s2"}},
		},
	}
	text := fantasy.TextPart{
		Text: "answer",
		ProviderOptions: fantasy.ProviderOptions{
			openaicompat.Name: &openaicompat.ContentExtraFields{Fields: map[string]any{"cache_control": map[string]any{"type": "ephemeral"}}},
		},
	}
	msg := fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{reasoning, text}}

	s := newStore(t, testOptions(t))
	if err := s.AppendUser(user("q", kimi)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAssistant(MessageEntry{Message: msg, Model: kimi, StopReason: "end_turn"}); err != nil {
		t.Fatal(err)
	}
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]fantasy.Message{
		"read back":      tr.Entries[1].Message,
		"store context":  s.Context(kimi)[1],
		"loaded context": tr.Context(kimi)[1],
	} {
		if !reflect.DeepEqual(got, msg) {
			t.Errorf("%s:\n got %#v\nwant %#v", name, got, msg)
		}
	}
}
