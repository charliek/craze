package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// canary is the only key any test here holds. It is obviously not a secret,
// and every test that could leak a key looks for it.
const canary = "sk-canary-not-a-secret"

// The fixtures below run the real OpenAI-compatible provider, wrapped, inside
// Fantasy's real agent loop, against a local server speaking Chat Completions
// SSE. The server's chunks are the wire shapes the provider parses.

func sse(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func textChunk(text string) string {
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, text)
}

func toolChunk(id, name, args string) string {
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`, id, name, args)
}

func finishChunk(reason string, usage bool) string {
	u := ""
	if usage {
		u = `,"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}`
	}
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":%q}]%s}`, reason, u)
}

// streamErrorEvent is an in-band error event, the way a provider reports a
// failure after the 200 response has begun. "server_error" is a type Fantasy
// marks transient, so unwrapped it is retried.
const streamErrorEvent = `{"error":{"message":"upstream overloaded","type":"server_error"}}`

// jsonError writes an HTTP error response in the OpenAI error envelope.
func jsonError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":"invalid_request_error"}}`, message)
}

func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// resolved is an openai-compat model-table entry aimed at baseURL, keyed with
// the canary.
func resolved(baseURL string) modeltable.Resolved {
	return modeltable.Resolved{
		Alias:      "test/model",
		ProviderID: "test",
		Driver:     modeltable.DriverOpenAICompat,
		BaseURL:    baseURL,
		APIKey:     canary,
		WireModel:  "wire-model",
		Name:       "Test Model",
		Efforts:    []string{"low", "high"},
	}
}

// wrapped builds resolved(baseURL)'s model through the factory.
func wrapped(t *testing.T, baseURL string) fantasy.LanguageModel {
	t.Helper()
	lm, err := New(resolved(baseURL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return lm
}

// released builds the same provider without the wrapper: Fantasy as shipped.
func released(t *testing.T, baseURL string) fantasy.LanguageModel {
	t.Helper()
	p, err := openaicompat.New(openaicompat.WithBaseURL(baseURL), openaicompat.WithAPIKey(canary))
	if err != nil {
		t.Fatalf("openaicompat.New: %v", err)
	}
	lm, err := p.LanguageModel(context.Background(), "wire-model")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	return lm
}

type probeInput struct {
	Value string `json:"value"`
}

// runTool runs one agent turn that offers a single tool, with no retries and
// at most four steps, and reports how often the tool ran and how many steps
// finished.
func runTool(lm fantasy.LanguageModel) (dispatched, steps int32, err error) {
	var d, s atomic.Int32
	tool := fantasy.NewAgentTool("probe_first", "a probe", func(context.Context, probeInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		d.Add(1)
		return fantasy.NewTextResponse("ok"), nil
	})
	agent := fantasy.NewAgent(lm, fantasy.WithTools(tool), fantasy.WithMaxRetries(0), fantasy.WithStopConditions(fantasy.StepCountIs(4)))
	_, err = agent.Stream(context.Background(), fantasy.AgentStreamCall{
		Prompt:       "go",
		OnStepFinish: func(fantasy.StepResult) error { s.Add(1); return nil },
	})
	return d.Load(), s.Load(), err
}

// stopWithTool answers the first request with a complete tool call finished
// by "stop" and any later one with plain text.
func stopWithTool(reqs *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if reqs.Add(1) == 1 {
			sse(w, toolChunk("call-1", "probe_first", `{"value":"v"}`), finishChunk("stop", true))
			return
		}
		sse(w, textChunk("done"), finishChunk("stop", true))
	}
}

// TestReleasedAgentDropsStopToolCall is the control for rule 1: Fantasy as
// shipped neither runs a tool call finished by "stop" nor continues the turn.
// If a Fantasy upgrade fixes this, the wrapper's rule 1 can go.
func TestReleasedAgentDropsStopToolCall(t *testing.T) {
	var reqs atomic.Int32
	srv := newServer(t, stopWithTool(&reqs))
	d, _, err := runTool(released(t, srv.URL))
	if err != nil || d != 0 || reqs.Load() != 1 {
		t.Fatalf("released behaviour changed: dispatched=%d requests=%d err=%v; want 0, 1, nil", d, reqs.Load(), err)
	}
}

// Rule 1: the wrapper makes that call a tool turn, so it runs and the turn
// continues to a second step.
func TestWrapperContinuesStopToolCall(t *testing.T) {
	var reqs atomic.Int32
	srv := newServer(t, stopWithTool(&reqs))
	d, steps, err := runTool(wrapped(t, srv.URL))
	if err != nil || d != 1 || reqs.Load() != 2 || steps != 2 {
		t.Fatalf("wrapper did not continue: dispatched=%d requests=%d steps=%d err=%v; want 1, 2, 2, nil", d, reqs.Load(), steps, err)
	}
}

// Rule 1's limit: a call cut short by "length" is still never run.
func TestWrapperLeavesLengthSuppressed(t *testing.T) {
	var reqs atomic.Int32
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		reqs.Add(1)
		sse(w, toolChunk("call-1", "probe_first", `{"value":`), finishChunk("length", true))
	})
	d, _, err := runTool(wrapped(t, srv.URL))
	if err != nil || d != 0 || reqs.Load() != 1 {
		t.Fatalf("truncated call must not run: dispatched=%d requests=%d err=%v; want 0, 1, nil", d, reqs.Load(), err)
	}
}

// Rule 2: a "stop" with nothing in it and no usage is ErrEmptyStep, where
// Fantasy as shipped reports a clean, empty answer. Usage alone keeps it a
// clean stop.
func TestWrapperTurnsEmptyStopIntoErrEmptyStep(t *testing.T) {
	empty := func(w http.ResponseWriter, _ *http.Request) { sse(w, finishChunk("stop", false)) }
	emptyWithUsage := func(w http.ResponseWriter, _ *http.Request) { sse(w, finishChunk("stop", true)) }

	if _, _, err := runTool(released(t, newServer(t, empty).URL)); err != nil {
		t.Fatalf("released: want a silent clean stop, got %v", err)
	}
	if _, _, err := runTool(wrapped(t, newServer(t, empty).URL)); !errors.Is(err, ErrEmptyStep) {
		t.Fatalf("wrapper, no usage: want ErrEmptyStep, got %v", err)
	}
	if _, _, err := runTool(wrapped(t, newServer(t, emptyWithUsage).URL)); err != nil {
		t.Fatalf("wrapper, usage reported: want a clean stop, got %v", err)
	}
}

// TestFinishRules pins rules 1 and 2 at the wrapper itself. Through the real
// provider a truncated call never reaches the wrapper as a ToolCall part (the
// provider suppresses it before a "length" finish), so the fixtures cannot
// show that an abnormal finish is left alone even when complete calls
// precede it; this can.
func TestFinishRules(t *testing.T) {
	text := fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: "hi"}
	call := fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "t", ToolCallInput: "{}"}
	warn := fantasy.StreamPart{Type: fantasy.StreamPartTypeWarnings}
	finish := func(reason fantasy.FinishReason, total int64) fantasy.StreamPart {
		return fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: reason, Usage: fantasy.Usage{TotalTokens: total}}
	}
	cases := []struct {
		name  string
		parts []fantasy.StreamPart
		want  fantasy.FinishReason // "" = the finish became an ErrEmptyStep error part
	}{
		{"stop after a tool call is a tool turn", []fantasy.StreamPart{text, call, finish(fantasy.FinishReasonStop, 7)}, fantasy.FinishReasonToolCalls},
		{"stop after text stays stop", []fantasy.StreamPart{text, finish(fantasy.FinishReasonStop, 7)}, fantasy.FinishReasonStop},
		{"length after a tool call stays length", []fantasy.StreamPart{call, finish(fantasy.FinishReasonLength, 7)}, fantasy.FinishReasonLength},
		{"content-filter stays", []fantasy.StreamPart{call, finish(fantasy.FinishReasonContentFilter, 7)}, fantasy.FinishReasonContentFilter},
		{"error stays", []fantasy.StreamPart{call, finish(fantasy.FinishReasonError, 7)}, fantasy.FinishReasonError},
		{"unknown stays", []fantasy.StreamPart{call, finish(fantasy.FinishReasonUnknown, 7)}, fantasy.FinishReasonUnknown},
		{"empty stop is an error", []fantasy.StreamPart{warn, finish(fantasy.FinishReasonStop, 0)}, ""},
		{"empty stop with usage stays stop", []fantasy.StreamPart{finish(fantasy.FinishReasonStop, 7)}, fantasy.FinishReasonStop},
		{"empty length is not an empty step", []fantasy.StreamPart{finish(fantasy.FinishReasonLength, 0)}, fantasy.FinishReasonLength},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := wrap(stubModel{parts: tc.parts}, newScrubber(canary))
			parts, err := m.Stream(context.Background(), fantasy.Call{})
			if err != nil {
				t.Fatal(err)
			}
			var last fantasy.StreamPart
			for part := range parts {
				last = part
			}
			switch {
			case tc.want == "":
				if last.Type != fantasy.StreamPartTypeError || !errors.Is(last.Error, ErrEmptyStep) {
					t.Errorf("last part = %+v, want an ErrEmptyStep error part", last)
				}
			case last.Type != fantasy.StreamPartTypeFinish || last.FinishReason != tc.want:
				t.Errorf("last part = %s/%s, want finish/%s", last.Type, last.FinishReason, tc.want)
			default:
				// The reason the provider sent survives the rules, beside the
				// one they produced.
				sent := tc.parts[len(tc.parts)-1].FinishReason
				if raw, ok := RawFinish(last.ProviderMetadata); !ok || raw != sent {
					t.Errorf("RawFinish = %q, %v; want %q, the reason the provider sent", raw, ok, sent)
				}
			}
		})
	}
	// The control: metadata the wrapper did not write holds no raw finish.
	if raw, ok := RawFinish(fantasy.ProviderMetadata{}); ok || raw != "" {
		t.Errorf("RawFinish of empty metadata = %q, %v; want nothing", raw, ok)
	}
}

// The raw finish is the wrapper's own entry: a provider's metadata on the
// finish part is kept, and the provider's map is not written to.
func TestRawFinishLeavesProviderMetadataAlone(t *testing.T) {
	theirs := fantasy.ProviderMetadata{"test": &rawFinish{Reason: "theirs"}}
	parts := []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: "hi"},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{TotalTokens: 3}, ProviderMetadata: theirs},
	}
	resp, err := wrap(stubModel{parts: parts}, newScrubber(canary)).Stream(context.Background(), fantasy.Call{})
	if err != nil {
		t.Fatal(err)
	}
	var last fantasy.StreamPart
	for part := range resp {
		last = part
	}
	if len(theirs) != 1 {
		t.Fatalf("the provider's metadata map was written to: %v", theirs)
	}
	if last.ProviderMetadata["test"] != theirs["test"] {
		t.Fatalf("the provider's own entry was lost: %v", last.ProviderMetadata)
	}
	if raw, ok := RawFinish(last.ProviderMetadata); !ok || raw != fantasy.FinishReasonStop {
		t.Fatalf("RawFinish = %q, %v; want stop", raw, ok)
	}
}

// Rule 3: an error after text has streamed fails the step once. Unwrapped,
// the same error is one Fantasy retries — the replay would repeat the text.
func TestWrapperDoesNotRetryAfterOutput(t *testing.T) {
	handler := func(reqs *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			if reqs.Add(1) > 1 {
				sse(w, textChunk(" and again"), finishChunk("stop", true))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", textChunk("partial"))
			fmt.Fprintf(w, "data: %s\n\n", streamErrorEvent)
		}
	}

	// The premise: unwrapped, the error part is a retryable ProviderError.
	var rawReqs atomic.Int32
	raw, err := released(t, newServer(t, handler(&rawReqs)).URL).Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")},
	})
	if err != nil {
		t.Fatalf("released Stream: %v", err)
	}
	var rawErr error
	for part := range raw {
		if part.Type == fantasy.StreamPartTypeError {
			rawErr = part.Error
		}
	}
	var pe *fantasy.ProviderError
	if !errors.As(rawErr, &pe) || !pe.IsRetryable() {
		t.Fatalf("premise: the unwrapped mid-stream error should be a retryable ProviderError, got %#v", rawErr)
	}

	var reqs atomic.Int32
	srv := newServer(t, handler(&reqs))
	var text strings.Builder
	agent := fantasy.NewAgent(wrapped(t, srv.URL), fantasy.WithMaxRetries(1))
	_, err = agent.Stream(context.Background(), fantasy.AgentStreamCall{
		Prompt:      "go",
		OnTextDelta: func(_, delta string) error { text.WriteString(delta); return nil },
	})
	if reqs.Load() != 1 {
		t.Fatalf("requests = %d, want 1: a step with output on screen was retried", reqs.Load())
	}
	if text.String() != "partial" {
		t.Fatalf("streamed text = %q, want %q", text.String(), "partial")
	}
	var mse *MidStreamError
	if !errors.As(err, &mse) {
		t.Fatalf("err = %#v, want a *MidStreamError", err)
	}
	if !strings.Contains(mse.Message, "upstream overloaded") {
		t.Errorf("Message = %q, want the provider's text", mse.Message)
	}
	var ne net.Error
	if errors.As(err, &pe) || errors.As(err, &ne) || fantasy.IsTransportError(err) {
		t.Errorf("err %q still classifies as retryable (ProviderError %v, net.Error %v, transport %v)",
			err, errors.As(err, &pe), errors.As(err, &ne), fantasy.IsTransportError(err))
	}
}

// Rule 3's other side: a 503 before any output is retried, once, and the
// retry succeeds. The server's retry-after-ms is honoured every time, which
// needs the scrubber's lowercased headers (see scrubber.headers); without
// them about half the runs would wait Fantasy's 5 s default.
func TestWrapperRetriesBeforeOutput(t *testing.T) {
	var reqs atomic.Int32
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if reqs.Add(1) == 1 {
			w.Header().Set("retry-after-ms", "1")
			jsonError(w, http.StatusServiceUnavailable, "overloaded")
			return
		}
		sse(w, textChunk("ok"), finishChunk("stop", true))
	})
	var retries atomic.Int32
	var delay atomic.Int64
	var retryErr atomic.Pointer[fantasy.ProviderError]
	var text strings.Builder
	agent := fantasy.NewAgent(wrapped(t, srv.URL), fantasy.WithMaxRetries(1))
	_, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{
		Prompt:      "go",
		OnTextDelta: func(_, delta string) error { text.WriteString(delta); return nil },
		OnRetry: func(err *fantasy.ProviderError, d time.Duration) {
			retries.Add(1)
			delay.Store(int64(d))
			retryErr.Store(err)
		},
	})
	if err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if reqs.Load() != 2 || retries.Load() != 1 || text.String() != "ok" {
		t.Fatalf("requests=%d retries=%d text=%q; want 2, 1, %q", reqs.Load(), retries.Load(), text.String(), "ok")
	}
	if got := time.Duration(delay.Load()); got != time.Millisecond {
		t.Errorf("retry delay = %v, want the server's retry-after-ms (1ms)", got)
	}
	if pe := retryErr.Load(); pe == nil || pe.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("OnRetry error = %#v, want the scrubbed 503", pe)
	} else if found := leaks(pe, canary); len(found) > 0 {
		t.Errorf("the key reached OnRetry: %v", found)
	}
}

// Rule 4: a 401 whose body echoes the Authorization header — the realistic
// leak — comes back with no trace of the key anywhere reachable from the
// error, and still classifies as a 401.
func TestWrapperScrubsKeyFromAuthError(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, http.StatusUnauthorized, "invalid credentials: "+r.Header.Get("Authorization"))
	})
	agent := fantasy.NewAgent(wrapped(t, srv.URL), fantasy.WithMaxRetries(1))
	_, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "go"})
	if err == nil {
		t.Fatal("want the 401, got success")
	}
	if found := leaks(err, canary); len(found) > 0 {
		t.Fatalf("the key is reachable from the error at %v", found)
	}
	var pe *fantasy.ProviderError
	if !errors.As(err, &pe) || pe.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %#v, want a ProviderError with status 401", err)
	}
	if want := "invalid credentials: Bearer " + redacted; pe.Message != want {
		t.Errorf("Message = %q, want %q", pe.Message, want)
	}
}

// Cancellation mid-stream is neither retried nor disguised: it reaches the
// caller as context.Canceled, not as a MidStreamError.
func TestWrapperPassesCancelThrough(t *testing.T) {
	release := make(chan struct{})
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", textChunk("partial"))
		w.(http.Flusher).Flush()
		select { // hold the stream open until the client goes away
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) }) // runs before srv.Close

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := fantasy.NewAgent(wrapped(t, srv.URL), fantasy.WithMaxRetries(1))
	_, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Prompt:      "go",
		OnTextDelta: func(string, string) error { cancel(); return nil },
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %#v, want context.Canceled", err)
	}
	var mse *MidStreamError
	if errors.As(err, &mse) {
		t.Fatalf("a cancel became a MidStreamError: %v", err)
	}
}
