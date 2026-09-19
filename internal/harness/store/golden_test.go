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
	"strings"
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

// The golden's tool step: the model reasons, then calls two tools in
// parallel, one of which fails.
const (
	toolPrompt    = "Where is Peano arithmetic defined? Check notes.md and grep the repo."
	toolReasoning = "Two lookups, at once."
	toolSteer     = "Skip grep; notes.md is enough."
	toolAnswer    = "notes.md says Peano arithmetic lives in axioms.go."
)

// toolCallChunk opens call index with its id and name and the first piece
// of its arguments; toolArgsChunk streams more of them.
func toolCallChunk(index int, id, name, args string) string {
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`, index, id, name, args)
}

func toolArgsChunk(index int, args string) string {
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%q}}]},"finish_reason":null}]}`, index, args)
}

const finishToolCalls = `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],` +
	`"usage":{"prompt_tokens":1500,"completion_tokens":60,"total_tokens":1560,` +
	`"prompt_tokens_details":{"cached_tokens":1200},"completion_tokens_details":{"reasoning_tokens":10}}}`

// goldenTool answers every call with reply, as an error result when fail is
// set: Fantasy's agent turns an IsError response into one.
type goldenTool struct {
	name, reply string
	fail        bool
}

func (g goldenTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: g.name, Parameters: map[string]any{}, Required: []string{}, Parallel: true}
}

func (g goldenTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if g.fail {
		return fantasy.NewTextErrorResponse(g.reply), nil
	}
	return fantasy.NewTextResponse(g.reply), nil
}

func (goldenTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (goldenTool) SetProviderOptions(fantasy.ProviderOptions) {}

// openAICompatStep runs one step through Fantasy's agent, with tools on
// offer, on the real OpenAI-compatible provider named as m names it,
// streaming chunks, and returns the finished step as the turn runner's
// OnStepFinish receives it (a tool step with its tools' results).
func openAICompatStep(t *testing.T, m Model, prompt string, chunks []string, tools ...fantasy.AgentTool) fantasy.StepResult {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	p, err := openaicompat.New(openaicompat.WithBaseURL(srv.URL), openaicompat.WithAPIKey("sk-canary-not-a-secret"), openaicompat.WithName(m.Provider))
	if err != nil {
		t.Fatal(err)
	}
	lm, err := p.LanguageModel(context.Background(), m.WireModel)
	if err != nil {
		t.Fatal(err)
	}
	var steps []fantasy.StepResult
	_, err = fantasy.NewAgent(lm, fantasy.WithMaxRetries(0), fantasy.WithTools(tools...)).Stream(context.Background(), fantasy.AgentStreamCall{
		Prompt:       prompt,
		StopWhen:     []fantasy.StopCondition{fantasy.StepCountIs(1)},
		OnStepFinish: func(s fantasy.StepResult) error { steps = append(steps, s); return nil },
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("got %d steps (%+v), want one", len(steps), steps)
	}
	return steps[0]
}

// TestTranscriptGolden writes a representative transcript with a fixed clock
// and fixed ids, compares its bytes to the golden, and reads it back: a
// reasoning answer with usage, a model and an effort switch, an answer cut
// short by a cancel, then a turn whose first step calls two tools in
// parallel, one of them failing, and whose second step is the first to see
// a steer. Regenerate with:
//
//	go test ./internal/harness/store -run TestTranscriptGolden -update
func TestTranscriptGolden(t *testing.T) {
	step := openAICompatStep(t, kimi, goldenPrompt, []string{
		reasoningChunk("Simple arithmetic: "), reasoningChunk("2 plus 2 is 4."),
		contentChunk("Four"), contentChunk("."),
		finishWithUsage,
	})
	if got := messageTexts(step.Messages); !reflect.DeepEqual(got, []string{
		"assistant: (thinking: " + goldenReasoning + ") " + goldenAnswer,
	}) {
		t.Fatalf("the provider's step is %q; the SSE fixture is not being read as intended", got)
	}
	toolStep := openAICompatStep(t, minimax, toolPrompt, []string{
		reasoningChunk(toolReasoning),
		toolCallChunk(0, "call_1", "read", `{"filePath":`), toolArgsChunk(0, `"notes.md"}`),
		toolCallChunk(1, "call_2", "grep", `{"pattern":"Peano"}`),
		finishToolCalls,
	}, goldenTool{name: "read", reply: "1: Peano arithmetic lives in axioms.go"},
		goldenTool{name: "grep", reply: "ripgrep (rg) is not installed or not on PATH.", fail: true})
	wantToolStep := []string{
		`assistant: (thinking: ` + toolReasoning + `) [call call_1 read {"filePath":"notes.md"}] [call call_2 grep {"pattern":"Peano"}]`,
		"tool: [result call_1: 1: Peano arithmetic lives in axioms.go] [result call_2: error: ripgrep (rg) is not installed or not on PATH.]",
	}
	if got := messageTexts(toolStep.Messages); !reflect.DeepEqual(got, wantToolStep) {
		t.Fatalf("the provider's tool step is %q; the SSE fixture is not being read as intended", got)
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
	// A turn with tools: the tool step as OnStepFinish hands it over, then
	// the answer, led by what the user interjected while the tools ran.
	if err := s.AppendUser(MessageEntry{Message: fantasy.NewUserMessage(toolPrompt), Model: minimax, Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	stepIDs, err := s.AppendStep(nil,
		MessageEntry{Message: toolStep.Messages[0], Model: minimax, Effort: "high", Usage: UsageOf(toolStep.Usage), StopReason: "tool_use"},
		&MessageEntry{Message: toolStep.Messages[1], Model: minimax, Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	answerIDs, err := s.AppendStep(
		[]MessageEntry{{Message: fantasy.NewUserMessage(toolSteer), Model: minimax, Effort: "high"}},
		MessageEntry{Message: assistantMsg("", toolAnswer), Model: minimax, Effort: "high", StopReason: "end_turn"}, nil)
	if err != nil {
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
	// The tool step's two messages, results included, are exactly the
	// agent's; the step and the answer returned the ids they wrote.
	for i, want := range toolStep.Messages {
		if back := tr.Entries[7+i].Message; !reflect.DeepEqual(back, want) {
			t.Fatalf("the tool step's message %d did not survive the round trip:\n got %#v\nwant %#v", i, back, want)
		}
	}
	if u := tr.Entries[7].Usage; u == nil || *u != (Usage{Input: 300, Output: 60, Reasoning: 10, CacheRead: 1200}) {
		t.Fatalf("the tool step's usage read back as %+v", u)
	}
	if got, want := append(stepIDs, answerIDs...), entryIDs(tr.Entries[6:]); !reflect.DeepEqual(got, want) {
		t.Fatalf("the turn's steps returned ids %q; they wrote %q", got, want)
	}
	// Each model sees only its own reasoning, and every call its result.
	toolTurn := []string{
		"user: " + toolPrompt,
		wantToolStep[0],
		wantToolStep[1],
		"user: " + toolSteer,
		"assistant: " + toolAnswer,
	}
	wantMinimax := append([]string{
		"user: " + goldenPrompt,
		"assistant: " + goldenAnswer,
		"user: Now prove it <briefly> & formally.",
		"assistant: (thinking: Peano: define 2 as S(S(0)).) By the Peano axioms, 2 + 2 = S(S(0)) + S(S(0))",
	}, toolTurn...)
	if got := tr.Context(minimax); !reflect.DeepEqual(messageTexts(got), wantMinimax) || unpairedIn(got) != nil {
		t.Fatalf("context for minimax = %q (%v)", messageTexts(got), unpairedIn(got))
	}
	toolTurn[1] = strings.Replace(toolTurn[1], "(thinking: "+toolReasoning+") ", "", 1)
	wantKimi := append([]string{
		"user: " + goldenPrompt,
		"assistant: (thinking: " + goldenReasoning + ") " + goldenAnswer,
		"user: Now prove it <briefly> & formally.",
		"assistant: By the Peano axioms, 2 + 2 = S(S(0)) + S(S(0))",
	}, toolTurn...)
	if got := s.Context(kimi); !reflect.DeepEqual(messageTexts(got), wantKimi) || unpairedIn(got) != nil {
		t.Fatalf("context for kimi = %q (%v)", messageTexts(got), unpairedIn(got))
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
