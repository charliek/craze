package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm/responsesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The OpenAI key's provider on the openai-responses driver (plan 050 C3):
// OpenAI's own streams, captured live on 2026-11-11 (testdata/openai_*.sse,
// the stream bodies alone) and replayed from a loopback server, its cache
// writes counted apart from its input, a message's phase kept and replayed to
// it alone, its quota 429 final, and its overload retried.

// openaiRoute is a resolved model of the OpenAI key's provider at base, as
// the table resolves the shipped one: GPT-6.1 Sol.
func openaiRoute(base string) modeltable.Resolved {
	return modeltable.Resolved{
		Alias:           "openai/gpt-6.1-sol",
		ProviderID:      modeltable.OpenAIProvider,
		Driver:          modeltable.DriverOpenAIResponses,
		BaseURL:         base,
		APIKey:          canary,
		WireModel:       "gpt-6.1-sol",
		Name:            "GPT-6.1 Sol (OpenAI)",
		ContextWindow:   1048576,
		MaxOutputTokens: 128000,
		Efforts:         []string{"low", "medium", "high", "xhigh", "max"},
		DefaultEffort:   "medium",
		Vision:          true,
	}
}

// openaiModel is r's model aimed at the server, built by New.
func openaiModel(t *testing.T, r modeltable.Resolved) fantasy.LanguageModel {
	t.Helper()
	lm, err := New(r)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return lm
}

// rxCapture streams testdata/name as it was captured: OpenAI's own event and
// data lines, byte for byte.
func rxCapture(t *testing.T, name string) http.HandlerFunc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = w.Write(b)
	}
}

// openaiStep streams one step with prompt on lm and returns its parts,
// failing on an error part.
func openaiStep(t *testing.T, lm fantasy.LanguageModel, prompt fantasy.Prompt) []fantasy.StreamPart {
	t.Helper()
	stream, err := lm.Stream(context.Background(), fantasy.Call{Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	var parts []fantasy.StreamPart
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			t.Fatalf("the step failed: %v", part.Error)
		}
		parts = append(parts, part)
	}
	return parts
}

// TestOpenAICapturesThroughTheKeyRoute: each captured OpenAI stream, read by
// the key-route adapter, gives what the live session saw — the text, a
// reasoning item's encrypted content, the call — and its usage with the cache
// writes counted apart (CacheCreation) and every token of input_tokens once:
// Input + CacheRead + CacheCreation is input_tokens. Each message's end keeps
// its phase in the text part's metadata. Negative controls: an adapter that
// drops cache_write_tokens leaves CacheCreation 0 on the cache-write capture;
// one that drops the phase leaves the metadata empty.
func TestOpenAICapturesThroughTheKeyRoute(t *testing.T) {
	for _, tc := range []struct {
		file                      string
		text, phase, call         string
		sealed                    bool
		input, cached, written    int64
		inputTokens, outputTokens int64
	}{
		{"openai_text.sse", "The capital of France is Paris.", "final_answer", "", true, 29, 0, 0, 29, 24},
		{"openai_sol_commentary_call.sse", "(17 − 5) × 3 = 12 × 3 = 36.\n", "commentary", "get_weather", true, 80, 0, 0, 80, 80},
		{"openai_cache_write.sse", "543", "final_answer", "", false, 3, 0, 1758, 1761, 5},
	} {
		t.Run(tc.file, func(t *testing.T) {
			srv := newRxServer(t, rxCapture(t, tc.file))
			parts := openaiStep(t, openaiModel(t, openaiRoute(srv.srv.URL+"/v1")), fantasy.Prompt{fantasy.NewUserMessage("go")})
			var text strings.Builder
			var phase, call string
			var sealed bool
			var finish *fantasy.StreamPart
			for i, p := range parts {
				switch p.Type {
				case fantasy.StreamPartTypeTextDelta:
					text.WriteString(p.Delta)
				case fantasy.StreamPartTypeTextEnd:
					if md, ok := p.ProviderMetadata[typeResponsesMessage].(*responsesPhase); ok {
						phase = md.Phase
					}
				case fantasy.StreamPartTypeReasoningEnd:
					sealed = sealed || replayableMetadata(p.ProviderMetadata)
				case fantasy.StreamPartTypeToolCall:
					call = p.ToolCallName
				case fantasy.StreamPartTypeFinish:
					finish = &parts[i]
				}
			}
			if text.String() != tc.text || phase != tc.phase || call != tc.call || sealed != tc.sealed {
				t.Fatalf("text %q, phase %q, call %q, sealed reasoning %v; want %q, %q, %q, %v",
					text.String(), phase, call, sealed, tc.text, tc.phase, tc.call, tc.sealed)
			}
			if finish == nil {
				t.Fatal("no Finish part")
			}
			u := finish.Usage
			if u.InputTokens != tc.input || u.CacheReadTokens != tc.cached || u.CacheCreationTokens != tc.written || u.OutputTokens != tc.outputTokens {
				t.Fatalf("usage = %+v; want input %d, cache read %d, cache write %d, output %d", u, tc.input, tc.cached, tc.written, tc.outputTokens)
			}
			if sum := u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens; sum != tc.inputTokens {
				t.Fatalf("Input + CacheRead + CacheCreation = %d, want input_tokens %d", sum, tc.inputTokens)
			}
		})
	}
}

// replayableMetadata reports whether a reasoning end's metadata can be
// replayed on the key route.
func replayableMetadata(md fantasy.ProviderMetadata) bool {
	_, ok := replayableWith(fantasy.ReasoningPart{ProviderOptions: fantasy.ProviderOptions(md)}, keyItemID)
	return ok
}

// TestFantasyUsageCountsCacheWrites: a response's cache-write tokens are
// CacheCreation and its cached tokens CacheRead, neither also Input, each held
// to what input_tokens leaves — cached first — so a count that is negative,
// larger than input_tokens, or overlapping the cached one never counts a
// token twice, and Input + CacheRead + CacheCreation is input_tokens. A usage
// with no cache writes (Meta's) maps as it did before cache writes were read.
func TestFantasyUsageCountsCacheWrites(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		in                           responsesapi.Usage
		input, cacheRead, cacheWrite int64
	}{
		{"OpenAI's cache write", responsesapi.Usage{InputTokens: 5594, CacheWriteTokens: 5591, OutputTokens: 5, TotalTokens: 5599}, 3, 0, 5591},
		{"OpenAI's cache hit", responsesapi.Usage{InputTokens: 5594, CachedTokens: 5591, OutputTokens: 5}, 3, 5591, 0},
		{"both", responsesapi.Usage{InputTokens: 1000, CachedTokens: 600, CacheWriteTokens: 300}, 100, 600, 300},
		{"writes larger than the input", responsesapi.Usage{InputTokens: 100, CacheWriteTokens: 250}, 0, 0, 100},
		{"writes overlapping the cached", responsesapi.Usage{InputTokens: 100, CachedTokens: 80, CacheWriteTokens: 50}, 0, 80, 20},
		{"negative writes", responsesapi.Usage{InputTokens: 100, CachedTokens: 10, CacheWriteTokens: -5}, 90, 10, 0},
		{"negative cached", responsesapi.Usage{InputTokens: 100, CachedTokens: -10, CacheWriteTokens: 30}, 70, 0, 30},
		{"cached larger than the input", responsesapi.Usage{InputTokens: 100, CachedTokens: 400, CacheWriteTokens: 30}, 0, 100, 0},
		{"Meta's shape", responsesapi.Usage{InputTokens: 550, CachedTokens: 113, OutputTokens: 97, ReasoningTokens: 27, TotalTokens: 647}, 437, 113, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := fantasyUsage(tc.in)
			if u.InputTokens != tc.input || u.CacheReadTokens != tc.cacheRead || u.CacheCreationTokens != tc.cacheWrite {
				t.Fatalf("usage = %+v; want input %d, cache read %d, cache write %d", u, tc.input, tc.cacheRead, tc.cacheWrite)
			}
			if sum := u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens; sum != tc.in.InputTokens {
				t.Fatalf("Input + CacheRead + CacheCreation = %d, want input_tokens %d", sum, tc.in.InputTokens)
			}
			if u.OutputTokens != tc.in.OutputTokens || u.ReasoningTokens != tc.in.ReasoningTokens {
				t.Fatalf("output %d, reasoning %d; want the response's", u.OutputTokens, u.ReasoningTokens)
			}
		})
	}
	// Meta's shape maps field for field as before (plan 047's pinned usage).
	if got, want := fantasyUsage(responsesapi.Usage{InputTokens: 550, CachedTokens: 113, OutputTokens: 97, ReasoningTokens: 27, TotalTokens: 647}),
		(fantasy.Usage{InputTokens: 437, OutputTokens: 97, TotalTokens: 647, ReasoningTokens: 27, CacheReadTokens: 113}); got != want {
		t.Fatalf("Meta's usage = %+v, want %+v", got, want)
	}
}

// TestOpenAIReplaysTheMessagePhase: through Fantasy's agent, Sol's captured
// tool step — reasoning, a commentary message, the call — is replayed on the
// next request with the message's phase, "commentary", and the reasoning's
// encrypted content. The same prompt replayed to Meta's provider on the same
// driver sends no phase (plan 047 MR-5: never sent to Meta untested), and on
// the ChatGPT route none either; a phase the API does not take is never sent,
// nor one that is missing. Negative control: a replay that drops the
// metadata sends no phase to OpenAI.
func TestOpenAIReplaysTheMessagePhase(t *testing.T) {
	srv := newRxServer(t, rxCapture(t, "openai_sol_commentary_call.sse"), rxCapture(t, "openai_text.sse"))
	weather := fantasy.NewParallelAgentTool("get_weather", "Get the weather.", func(_ context.Context, in struct {
		City string `json:"city"`
	}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("sunny in " + in.City), nil
	})
	agent := fantasy.NewAgent(openaiModel(t, openaiRoute(srv.srv.URL+"/v1")), fantasy.WithTools(weather), fantasy.WithMaxRetries(0))
	res, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "What is (17-5)*3, and the weather in Paris?"})
	if err != nil {
		t.Fatalf("the turn failed: %v", err)
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	items := input(t, reqs[1].body)
	if got, want := itemTypes(items), []string{"message/user", "reasoning/rs_000c9e01b27bedc1016af52077d13c87d1b77ad4a58110295e",
		"message/assistant", "function_call/call_5z9XH9oSDQLcZrZ5U2nmNLKL", "function_call_output/call_5z9XH9oSDQLcZrZ5U2nmNLKL"}; !slices.Equal(got, want) {
		t.Fatalf("the second request's input = %v, want %v", got, want)
	}
	if items[2]["phase"] != "commentary" || items[1]["encrypted_content"] == nil {
		t.Fatalf("replayed: message %v, reasoning with encrypted content %v", items[2], items[1]["encrypted_content"] != nil)
	}

	// The stored step, replayed as history.
	var assistant fantasy.Message
	for _, m := range res.Steps[0].Messages {
		if m.Role == fantasy.MessageRoleAssistant {
			assistant = m
		}
	}
	textPart := func(phase string) fantasy.Message {
		md := fantasy.ProviderOptions{}
		if phase != "" {
			md[typeResponsesMessage] = &responsesPhase{Phase: phase}
		}
		return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "said", ProviderOptions: md}}}
	}
	phaseOf := func(lm fantasy.LanguageModel, srv *rxServer, prompt fantasy.Prompt) []any {
		var out []any
		for _, it := range input(t, streamPrompt(t, lm, srv, prompt)) {
			if it["type"] == "message" && it["role"] == "assistant" {
				out = append(out, it["phase"])
			}
		}
		return out
	}
	prompt := fantasy.Prompt{fantasy.NewUserMessage("go"), assistant, textPart("final_answer"), textPart("bogus"), textPart(""), fantasy.NewUserMessage("again")}
	key := newRxServer(t, rxAnswer("ok"))
	if got := phaseOf(openaiModel(t, openaiRoute(key.srv.URL+"/v1")), key, prompt); !slices.Equal(got, []any{"commentary", "final_answer", nil, nil}) {
		t.Fatalf("OpenAI's replayed phases = %v, want commentary, final_answer and none for the bogus and missing ones", got)
	}
	meta := newRxServer(t, metaAnswer("ok"))
	if got := phaseOf(meta.keyModel(t), meta, prompt); !slices.Equal(got, []any{nil, nil, nil, nil}) {
		t.Fatalf("Meta's replayed phases = %v, want none", got)
	}
	chat := newRxServer(t, rxAnswer("ok"))
	body := streamPrompt(t, chat.model(t, chatgpt(), newStaticAuth(chatgptToken)), chat, prompt)
	if strings.Contains(string(body), `"phase"`) {
		t.Fatalf("the ChatGPT route sent a phase:\n%s", body)
	}
}

// TestKeyRouteQuotaIsFinal (plan 050 C3): through Fantasy's agent, its
// retries on, a 429 whose code or type says the account's credit or spend
// limit is gone — insufficient_quota and OpenAI's four billing codes — is
// sent once, a *FinalError carrying the code and type for the harness to
// class as quota; never a retryable provider error. So is a quota code or
// type inside the stream, and one named beside a rate limit's, either way
// round. The control: a rate
// limit's 429 is retried (TestKeyRouteHTTPFailures), and so is one whose only
// quota-sounding part is its message.
func TestKeyRouteQuotaIsFinal(t *testing.T) {
	for _, code := range []string{"insufficient_quota", "credit_balance_exhausted", "organization_spend_limit_exceeded",
		"project_spend_limit_exceeded", "organization_usage_limit_exceeded"} {
		for _, where := range []string{"code", "type"} {
			t.Run(code+" as the "+where, func(t *testing.T) {
				body := fmt.Sprintf(`{"error":{"code":%q,"type":"requests","message":"You exceeded your current quota.","param":null}}`, code)
				if where == "type" {
					body = fmt.Sprintf(`{"error":{"code":null,"type":%q,"message":"You exceeded your current quota.","param":null}}`, code)
				}
				srv := newRxServer(t, rxJSON(429, body, "retry-after-ms", "1"), metaAnswer("a retry"), metaAnswer("a retry"))
				waits, err := keyTurn(t, srv)
				if n := len(srv.requests()); n != 1 || len(waits) != 0 {
					t.Fatalf("%d requests and %d retries, want 1 and none", n, len(waits))
				}
				var fe *FinalError
				if !errors.As(err, &fe) || fe.StatusCode != 429 || (fe.Code != code && fe.Type != code) {
					t.Fatalf("err = %v (%T), want a final HTTP 429 naming %s", err, err, code)
				}
			})
		}
	}
	// A quota named by one discriminator and a rate limit by the other is
	// the quota's, as a 429 and inside the stream before any output alike
	// (plan 050 C3 review): sent once, a *FinalError.
	for _, tc := range []struct{ name, code, typ string }{
		{"a quota code beside a rate-limit type", "project_spend_limit_exceeded", "rate_limit_error"},
		{"a rate-limit code beside a quota type", "rate_limit_exceeded", "credit_balance_exhausted"},
	} {
		for _, where := range []string{"as a 429", "inside the stream"} {
			t.Run(tc.name+" "+where, func(t *testing.T) {
				reply := rxJSON(429, fmt.Sprintf(`{"error":{"code":%q,"type":%q,"message":"Project spend limit reached"}}`, tc.code, tc.typ), "retry-after-ms", "1")
				if where == "inside the stream" {
					reply = rxEvents(rx(map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed",
						"error": map[string]any{"code": tc.code, "type": tc.typ, "message": "Project spend limit reached"}}}))
				}
				srv := newRxServer(t, reply, metaAnswer("a retry"), metaAnswer("a retry"))
				waits, err := keyTurn(t, srv)
				if n := len(srv.requests()); n != 1 || len(waits) != 0 {
					t.Fatalf("%d requests and %d retries, want 1 and none", n, len(waits))
				}
				var fe *FinalError
				if !errors.As(err, &fe) || fe.Code != tc.code || fe.Type != tc.typ {
					t.Fatalf("err = %v (%T), want a final error naming %s and %s", err, err, tc.code, tc.typ)
				}
			})
		}
	}
	t.Run("a quota's words alone (control)", func(t *testing.T) {
		srv := newRxServer(t, rxJSON(429, `{"error":{"code":"rate_limit_exceeded","message":"You exceeded your current quota."}}`, "retry-after-ms", "1"), metaAnswer("ok"))
		if _, err := keyTurn(t, srv); err != nil || len(srv.requests()) != 2 {
			t.Fatalf("err = %v after %d requests, want the retry to succeed", err, len(srv.requests()))
		}
	})
}

// TestKeyRouteRetriesAnOverloadBeforeOutput (plan 050 C3): OpenAI's overload
// inside the stream — server_is_overloaded, of type service_unavailable_error
// — before any output is a retryable provider error, by its code or its type
// alone, as a 503 in a status is; after output the wrapper retries it as a
// server's fault (TestInBandFailuresAfterOutput). Negative control: without the
// two names in the key route's transient codes the error is not retryable.
func TestKeyRouteRetriesAnOverloadBeforeOutput(t *testing.T) {
	for _, tc := range []struct{ name, event string }{
		{"the code", rx(map[string]any{"type": "error", "error": map[string]any{"code": "server_is_overloaded", "message": "busy"}})},
		{"the type", rx(map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed",
			"error": map[string]any{"type": "service_unavailable_error", "message": "busy"}}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := keyStep(t, newRxServer(t, rxEvents(tc.event)))
			var pe *fantasy.ProviderError
			if !errors.As(err, &pe) || !pe.IsRetryable() {
				t.Fatalf("err = %v (%T), want a retryable *fantasy.ProviderError", err, err)
			}
		})
	}
}

// TestOnlyOpenAIKeepsThePhase (plan 050 C3 review): a message's phase is kept
// in its text part's metadata — and so stored — only on the OpenAI key's
// provider, the one it is replayed to. On Meta's provider on the same driver,
// and on the ChatGPT route, a stream whose messages carry a phase gives text
// parts with no metadata at all, as before phases were read, so their
// transcripts stay ones an older craze can open (its registry refuses a
// provider-data type it does not know). Negative control: keeping the phase
// on every route fails the Meta and ChatGPT cases.
func TestOnlyOpenAIKeepsThePhase(t *testing.T) {
	withPhase := func(id, text, phase string) map[string]any {
		m := rxMessage(id, text)
		m["phase"] = phase
		return m
	}
	stream := rxEvents(rxAdded(0, withPhase("msg_1", "", "commentary")), rxText(0, "msg_1", "Looking."),
		rxDone(0, withPhase("msg_1", "Looking.", "commentary")), rxCompleted(10, 0, 2, 0))
	metadata := func(parts []fantasy.StreamPart) []fantasy.ProviderMetadata {
		var out []fantasy.ProviderMetadata
		for _, p := range parts {
			if p.Type == fantasy.StreamPartTypeTextEnd {
				out = append(out, p.ProviderMetadata)
			}
		}
		return out
	}
	prompt := fantasy.Prompt{fantasy.NewUserMessage("go")}
	openaiSrv := newRxServer(t, stream)
	if got := metadata(openaiStep(t, openaiModel(t, openaiRoute(openaiSrv.srv.URL+"/v1")), prompt)); len(got) != 1 || got[0] == nil {
		t.Fatalf("the OpenAI key's text end = %v, want the phase", got)
	}
	meta := newRxServer(t, stream)
	if got := metadata(openaiStep(t, meta.keyModel(t), prompt)); len(got) != 1 || got[0] != nil {
		t.Fatalf("Meta's text end = %v, want no metadata", got)
	}
	chat := newRxServer(t, stream)
	if got := metadata(openaiStep(t, chat.model(t, chatgpt(), newStaticAuth(chatgptToken)), prompt)); len(got) != 1 || got[0] != nil {
		t.Fatalf("the ChatGPT route's text end = %v, want no metadata", got)
	}
}
