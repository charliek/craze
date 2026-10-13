package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
)

// A key-route response cut short (plan 047 C6): response.incomplete for the
// output ceiling or the content filter ends the step as Chat Completions'
// "length" and "content-filter" do — what was said kept, its usage counted
// once, no error — and for any other reason it fails, as it does on the
// ChatGPT route for every reason.

// rxIncomplete is response.incomplete for reason, with usage when not nil.
func rxIncomplete(reason string, usage map[string]any) string {
	r := map[string]any{"id": "resp_1", "status": "incomplete", "output": []any{}, "incomplete_details": map[string]any{"reason": reason}}
	if usage != nil {
		r["usage"] = usage
	}
	return rx(map[string]any{"type": "response.incomplete", "response": r})
}

// stepParts streams one step on lm and returns every part it yielded.
func stepParts(t *testing.T, lm fantasy.LanguageModel) []fantasy.StreamPart {
	t.Helper()
	stream, err := lm.Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}})
	if err != nil {
		t.Fatal(err)
	}
	var parts []fantasy.StreamPart
	for p := range stream {
		parts = append(parts, p)
	}
	return parts
}

// partKinds is parts' types, each text, reasoning or tool-input part with its
// id, a finish with its reason: "text-start:m1", "finish:length", …
func partKinds(parts []fantasy.StreamPart) []string {
	var out []string
	for _, p := range parts {
		k := string(p.Type)
		switch p.Type {
		case fantasy.StreamPartTypeFinish:
			k += ":" + string(p.FinishReason)
		case fantasy.StreamPartTypeWarnings, fantasy.StreamPartTypeError:
		default:
			k += ":" + p.ID
		}
		out = append(out, k)
	}
	return out
}

// TestKeyRouteCutShortAtTheCeiling: a response cut by max_output_tokens —
// a reasoning item done, a message still streaming — ends the step with a
// length finish and no error: the partial text closed and kept, the done
// reasoning kept with its encrypted content, the usage once. Through
// Fantasy's agent, retries on, it is one request and a finished turn.
func TestKeyRouteCutShortAtTheCeiling(t *testing.T) {
	usage := rxUsage(100, 20, 4000, 3990)
	reply := func() http.HandlerFunc {
		return rxEvents(
			rxAdded(0, metaReasoning("rs_a:rs_b", "")),
			rxDone(0, metaReasoning("rs_a:rs_b", "sealed-cut")),
			rxAdded(1, metaMessage("rs_m", "")),
			rxText(1, "rs_m", "The answer begins"),
			rxIncomplete("max_output_tokens", usage),
		)
	}
	want := fantasy.Usage{InputTokens: 80, CacheReadTokens: 20, OutputTokens: 4000, ReasoningTokens: 3990, TotalTokens: 4100}

	srv := newRxServer(t, reply(), reply(), reply())
	parts := stepParts(t, srv.keyModel(t))
	if got, wantKinds := partKinds(parts), []string{"reasoning_start:rs_a:rs_b", "reasoning_end:rs_a:rs_b", "text_start:rs_m",
		"text_delta:rs_m", "text_end:rs_m", "finish:length"}; !slices.Equal(got, wantKinds) {
		t.Fatalf("parts = %v, want %v", got, wantKinds)
	}
	if fin := parts[len(parts)-1]; fin.Usage != want {
		t.Fatalf("usage = %+v, want %+v", fin.Usage, want)
	}

	resp, err := srv.keyModel(t).Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.FinishReason != fantasy.FinishReasonLength || resp.Content.Text() != "The answer begins" || resp.Usage != want {
		t.Fatalf("finish %q, text %q, usage %+v", resp.FinishReason, resp.Content.Text(), resp.Usage)
	}
	r, ok := resp.Content[0].(fantasy.ReasoningContent)
	if md := openai.GetReasoningMetadata(fantasy.ProviderOptions(r.ProviderMetadata)); !ok || md == nil || md.EncryptedContent == nil || *md.EncryptedContent != "sealed-cut" {
		t.Fatalf("the done reasoning was not kept with its encrypted content: %#v", resp.Content[0])
	}

	agent := fantasy.NewAgent(srv.keyModel(t), fantasy.WithMaxRetries(3))
	res, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "go"})
	if err != nil {
		t.Fatalf("the turn failed: %v", err)
	}
	if n := len(srv.requests()); n != 3 {
		t.Fatalf("%d requests, want 3: one each for the stream, Generate and the turn", n)
	}
	if res.Response.FinishReason != fantasy.FinishReasonLength || res.TotalUsage != want || res.Response.Content.Text() != "The answer begins" {
		t.Fatalf("the turn: finish %q, usage %+v, text %q", res.Response.FinishReason, res.TotalUsage, res.Response.Content.Text())
	}
}

// TestKeyRouteCutShortMidCall: a response cut by max_output_tokens while a
// call's arguments stream ends as Fantasy's OpenAI-compatible client ends a
// "length" stream with a call cut off (the control below runs that client
// on the same cut): the call is closed and never sent as a call, with a
// warning, and the step finishes length. A call done before the cut was
// sent whole; neither runs, since Fantasy runs no call of a length step.
func TestKeyRouteCutShortMidCall(t *testing.T) {
	srv := newRxServer(t, rxEvents(
		rxAdded(0, metaCall("fc_a", "call_a", "read_file", "")),
		rxArgs(0, "fc_a", `{"path":"a"}`),
		rxDone(0, metaCall("fc_a", "call_a", "read_file", `{"path":"a"}`)),
		rxAdded(1, metaCall("fc_b", "call_b", "read_file", "")),
		rxArgs(1, "fc_b", `{"path":`),
		rxIncomplete("max_output_tokens", rxUsage(50, 0, 30, 0)),
	))
	parts := stepParts(t, srv.keyModel(t))
	got := partKinds(parts)
	want := []string{"tool_input_start:call_a", "tool_input_delta:call_a", "tool_input_end:call_a", "tool_call:call_a",
		"tool_input_start:call_b", "tool_input_delta:call_b", "tool_input_end:call_b", "warnings", "finish:length"}
	if !slices.Equal(got, want) {
		t.Fatalf("parts = %v, want %v", got, want)
	}

	// The control: Fantasy's OpenAI-compatible client, on a Chat
	// Completions stream cut the same way, closes the cut call, sends no
	// call for it, warns, and finishes length.
	chat := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w,
			`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_b","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
			finishChunk("length", true))
	})
	lm, err := New(resolved(chat.URL + "/v1"))
	if err != nil {
		t.Fatal(err)
	}
	compat := partKinds(stepParts(t, lm))
	if wantCompat := []string{"tool_input_start:call_b", "tool_input_delta:call_b", "tool_input_end:call_b", "warnings", "finish:length"}; !slices.Equal(compat, wantCompat) {
		t.Fatalf("the control: openai-compat's parts = %v, want %v", compat, wantCompat)
	}
	if !slices.Equal(got[4:], compat) {
		t.Fatalf("the cut call's parts %v differ from openai-compat's %v", got[4:], compat)
	}

	var mu sync.Mutex
	var seen []string
	srv = newRxServer(t, rxEvents(
		rxAdded(0, metaCall("fc_a", "call_a", "read_file", "")),
		rxDone(0, metaCall("fc_a", "call_a", "read_file", `{"path":"a"}`)),
		rxAdded(1, metaCall("fc_b", "call_b", "read_file", "")),
		rxArgs(1, "fc_b", `{"path":`),
		rxIncomplete("max_output_tokens", nil),
	), metaAnswer("never asked"))
	agent := fantasy.NewAgent(srv.keyModel(t), fantasy.WithTools(readTool(&seen, &mu)), fantasy.WithMaxRetries(3))
	res, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "go"})
	if err != nil {
		t.Fatalf("the turn failed: %v", err)
	}
	if len(seen) != 0 || len(srv.requests()) != 1 || res.Response.FinishReason != fantasy.FinishReasonLength {
		t.Fatalf("the tool ran for %v after %d requests, finish %q; want no run, 1 request, length", seen, len(srv.requests()), res.Response.FinishReason)
	}
	var calls []string
	for _, c := range res.Response.Content {
		if tc, ok := c.(fantasy.ToolCallContent); ok {
			calls = append(calls, tc.ToolCallID)
		}
	}
	if !slices.Equal(calls, []string{"call_a"}) {
		t.Fatalf("the step's calls = %v, want only the one done before the cut", calls)
	}
}

// TestKeyRouteCutShortReasons: content_filter is a content-filter finish,
// the text so far kept; a reason craze does not know is a failure, its
// usage reported first, as every failure's is — after output, one the
// wrapper never retries.
func TestKeyRouteCutShortReasons(t *testing.T) {
	partial := func(reason string) http.HandlerFunc {
		return rxEvents(rxAdded(0, metaMessage("rs_m", "")), rxText(0, "rs_m", "so far"), rxIncomplete(reason, rxUsage(10, 0, 5, 0)))
	}
	srv := newRxServer(t, partial("content_filter"))
	resp, err := srv.keyModel(t).Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}})
	if err != nil || resp.FinishReason != fantasy.FinishReasonContentFilter || resp.Content.Text() != "so far" || resp.Usage.TotalTokens != 15 {
		t.Fatalf("content_filter: %+v, %v", resp, err)
	}

	for _, reason := range []string{"interrupted", ""} {
		t.Run(fmt.Sprintf("reason %q", reason), func(t *testing.T) {
			parts := stepParts(t, newRxServer(t, partial(reason)).keyModel(t))
			got := partKinds(parts)
			if want := []string{"text_start:rs_m", "text_delta:rs_m", "finish:error", "error"}; !slices.Equal(got, want) {
				t.Fatalf("parts = %v, want %v", got, want)
			}
			// After output began, the wrapper fails the step once (rule 3).
			var me *MidStreamError
			if err := parts[len(parts)-1].Error; !errors.As(err, &me) {
				t.Fatalf("err = %v (%T), want the step failed after output", err, err)
			}
		})
	}
}

// TestChatGPTRouteCutShortIsUnchanged: on the ChatGPT route a response cut by
// max_output_tokens or the content filter fails as before — the usage in a
// Finish part, then a non-retryable error naming the reason — and nothing
// already done behind a streaming item is released.
func TestChatGPTRouteCutShortIsUnchanged(t *testing.T) {
	for _, reason := range []string{"max_output_tokens", "content_filter"} {
		t.Run(reason, func(t *testing.T) {
			srv := newRxServer(t, rxEvents(
				rxAdded(0, rxMessage("msg_1", "")), rxText(0, "msg_1", "so far"),
				rxAdded(1, rxReasoning("rs_1", "")), rxDone(1, rxReasoning("rs_1", "sealed")),
				rxIncomplete(reason, rxUsage(10, 0, 5, 0))))
			parts := stepParts(t, srv.model(t, chatgpt(), newStaticAuth(chatgptToken)))
			if got, want := partKinds(parts), []string{"text_start:msg_1", "text_delta:msg_1", "reasoning_start:rs_1", "finish:error", "error"}; !slices.Equal(got, want) {
				t.Fatalf("parts = %v, want %v", got, want)
			}
			var pe *fantasy.ProviderError
			if err := parts[len(parts)-1].Error; !errors.As(err, &pe) || pe.IsRetryable() {
				t.Fatalf("err = %v (%T), want a non-retryable provider error", err, err)
			}
			if code, _ := structuredError(pe.ResponseBody); code != reason {
				t.Fatalf("the error's code = %q, want %q", code, reason)
			}
		})
	}
}

// TestKeyRouteCutShortReleasesWhatWasDone: a reasoning item done while a
// lower-index message still streamed is reported at the cut, its encrypted
// content kept, beside the message's text so far.
func TestKeyRouteCutShortReleasesWhatWasDone(t *testing.T) {
	srv := newRxServer(t, rxEvents(
		rxAdded(0, metaMessage("rs_m", "")), rxText(0, "rs_m", "so far"),
		rxAdded(1, metaReasoning("rs_x:rs_y", "")), rxDone(1, metaReasoning("rs_x:rs_y", "sealed-late")),
		rxIncomplete("max_output_tokens", nil)))
	resp, err := srv.keyModel(t).Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}})
	if err != nil || resp.FinishReason != fantasy.FinishReasonLength {
		t.Fatalf("Generate = %+v, %v", resp, err)
	}
	var sealed string
	for _, c := range resp.Content {
		if r, ok := c.(fantasy.ReasoningContent); ok {
			if md := openai.GetReasoningMetadata(fantasy.ProviderOptions(r.ProviderMetadata)); md != nil && md.EncryptedContent != nil {
				sealed = *md.EncryptedContent
			}
		}
	}
	if sealed != "sealed-late" || resp.Content.Text() != "so far" {
		t.Fatalf("content = %#v, want the done reasoning and the text so far", resp.Content)
	}
}

// TestKeyRouteCutShortKeepsTheOutputOrder (plan 047 C6, review F1): a
// response cut while message 0 still streams, after message 1 was done,
// closes message 0 before message 1 — the done one held behind the cut one
// is reported after it, in output_index order — so the parts, the collected
// response and the next request's replay all read "First." then "Second.".
func TestKeyRouteCutShortKeepsTheOutputOrder(t *testing.T) {
	cut := func() http.HandlerFunc {
		return rxEvents(
			rxAdded(0, metaMessage("rs_m0", "")), rxText(0, "rs_m0", "First."),
			rxAdded(1, metaMessage("rs_m1", "")), rxText(1, "rs_m1", "Second."), rxDone(1, metaMessage("rs_m1", "Second.")),
			rxIncomplete("max_output_tokens", rxUsage(30, 0, 9, 0)))
	}
	srv := newRxServer(t, cut(), cut(), cut(), metaAnswer("ok"))
	if got, want := partKinds(stepParts(t, srv.keyModel(t))), []string{"text_start:rs_m0", "text_delta:rs_m0", "text_start:rs_m1",
		"text_delta:rs_m1", "text_end:rs_m0", "text_end:rs_m1", "finish:length"}; !slices.Equal(got, want) {
		t.Errorf("parts = %v, want %v", got, want)
	}

	resp, err := srv.keyModel(t).Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, c := range resp.Content {
		if tc, ok := c.(fantasy.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}
	if !slices.Equal(texts, []string{"First.", "Second."}) || resp.FinishReason != fantasy.FinishReasonLength {
		t.Errorf("collected %v, finish %q; want First. then Second., length", texts, resp.FinishReason)
	}

	lm := srv.keyModel(t)
	res, err := fantasy.NewAgent(lm, fantasy.WithMaxRetries(0)).Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "go"})
	if err != nil || res.Response.FinishReason != fantasy.FinishReasonLength {
		t.Fatalf("the turn: %v, %v", res, err)
	}
	prompt := append(fantasy.Prompt{fantasy.NewUserMessage("go")}, res.Steps[0].Messages...)
	prompt = append(prompt, fantasy.NewUserMessage("go on"))
	var said []string
	for _, it := range input(t, streamPrompt(t, lm, srv, prompt)) {
		if it["type"] == "message" && it["role"] == "assistant" {
			b, _ := json.Marshal(it["content"])
			said = append(said, string(b))
		}
	}
	if want := []string{`[{"text":"First.","type":"output_text"}]`, `[{"text":"Second.","type":"output_text"}]`}; !slices.Equal(said, want) {
		t.Fatalf("the next request replays %v, want %v", said, want)
	}
}

// metaIncompleteCall is a function call's end as a response cut in its
// arguments may give it: the arguments partial, status "incomplete".
func metaIncompleteCall(id, callID, name, args string) map[string]any {
	c := metaCall(id, callID, name, args)
	c["status"] = "incomplete"
	return c
}

// TestKeyRouteCutShortIncompleteCall (plan 047 C6, review F2): a call whose
// end says status "incomplete" — its arguments partial — in a response cut
// by max_output_tokens is a call cut short, whether it ended behind a message
// still streaming or with nothing before it left to report: closed and never
// sent as a call, with the warning, so it is not in the step, not run, and
// not replayed by the next request.
func TestKeyRouteCutShortIncompleteCall(t *testing.T) {
	partial := `{"path":`
	usage := rxUsage(40, 0, 12, 0)
	for _, tc := range []struct {
		name   string
		events []string
		want   []string
	}{
		{"held behind a message still streaming", []string{
			rxAdded(0, metaMessage("rs_m0", "")), rxText(0, "rs_m0", "Reading."),
			rxAdded(1, metaCall("fc_1", "call_1", "read_file", "")), rxArgs(1, "fc_1", partial),
			rxDone(1, metaIncompleteCall("fc_1", "call_1", "read_file", partial)),
			rxIncomplete("max_output_tokens", usage),
		}, []string{"text_start:rs_m0", "text_delta:rs_m0", "tool_input_start:call_1", "tool_input_delta:call_1",
			"text_end:rs_m0", "tool_input_end:call_1", "warnings", "finish:length"}},
		{"releasable at once", []string{
			rxAdded(0, metaMessage("rs_m0", "")), rxText(0, "rs_m0", "Reading."), rxDone(0, metaMessage("rs_m0", "Reading.")),
			rxAdded(1, metaCall("fc_1", "call_1", "read_file", "")), rxArgs(1, "fc_1", partial),
			rxDone(1, metaIncompleteCall("fc_1", "call_1", "read_file", partial)),
			rxIncomplete("max_output_tokens", usage),
		}, []string{"text_start:rs_m0", "text_delta:rs_m0", "text_end:rs_m0", "tool_input_start:call_1", "tool_input_delta:call_1",
			"tool_input_end:call_1", "warnings", "finish:length"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRxServer(t, rxEvents(tc.events...), rxEvents(tc.events...), metaAnswer("ok"))
			if got := partKinds(stepParts(t, srv.keyModel(t))); !slices.Equal(got, tc.want) {
				t.Errorf("parts = %v, want %v", got, tc.want)
			}

			var mu sync.Mutex
			var seen []string
			lm := srv.keyModel(t)
			res, err := fantasy.NewAgent(lm, fantasy.WithTools(readTool(&seen, &mu)), fantasy.WithMaxRetries(0)).
				Stream(context.Background(), fantasy.AgentStreamCall{Prompt: "go"})
			if err != nil || res.Response.FinishReason != fantasy.FinishReasonLength {
				t.Fatalf("the turn: %v, %v", res, err)
			}
			for _, c := range res.Response.Content {
				if _, ok := c.(fantasy.ToolCallContent); ok {
					t.Errorf("the step holds the cut call: %#v", c)
				}
			}
			if len(seen) != 0 {
				t.Errorf("the tool ran for %v", seen)
			}
			prompt := append(fantasy.Prompt{fantasy.NewUserMessage("go")}, res.Steps[0].Messages...)
			prompt = append(prompt, fantasy.NewUserMessage("go on"))
			body := streamPrompt(t, lm, srv, prompt)
			if got := itemTypes(input(t, body)); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "function_call") }) ||
				strings.Contains(string(body), `{\"path\":"`) {
				t.Errorf("the next request replays the cut call: %v\n%s", got, body)
			}
		})
	}
}

// TestKeyRouteIncompleteItemInACompletedResponse (review F2's fallback): an
// item whose end said "incomplete" in a response that then completes all the
// same is reported done, as it always was: the call is a call of the step,
// in its place between the messages, and the step finishes tool-calls.
func TestKeyRouteIncompleteItemInACompletedResponse(t *testing.T) {
	call := metaIncompleteCall("fc_1", "call_1", "read_file", `{"path":"a"}`)
	srv := newRxServer(t, rxEvents(
		rxAdded(0, metaMessage("rs_m0", "")), rxText(0, "rs_m0", "First."), rxDone(0, metaMessage("rs_m0", "First.")),
		rxAdded(1, metaCall("fc_1", "call_1", "read_file", "")), rxDone(1, call),
		rxAdded(2, metaMessage("rs_m2", "")), rxText(2, "rs_m2", "Then."), rxDone(2, metaMessage("rs_m2", "Then.")),
		metaCompleted(20, 0, 7, 0, metaMessage("rs_m0", "First."), call, metaMessage("rs_m2", "Then.")),
	))
	resp, err := srv.keyModel(t).Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range resp.Content {
		switch v := c.(type) {
		case fantasy.TextContent:
			got = append(got, "text:"+v.Text)
		case fantasy.ToolCallContent:
			got = append(got, "call:"+v.ToolCallID+":"+v.Input)
		}
	}
	if want := []string{"text:First.", `call:call_1:{"path":"a"}`, "text:Then."}; !slices.Equal(got, want) || resp.FinishReason != fantasy.FinishReasonToolCalls {
		t.Fatalf("content %v, finish %q; want %v, tool-calls", got, resp.FinishReason, want)
	}
}

// TestChatGPTRouteIgnoresItemStatus: the ChatGPT route reads no item status —
// a call whose end says "incomplete" is reported done at once, as before —
// and its incomplete response is a failure as before.
func TestChatGPTRouteIgnoresItemStatus(t *testing.T) {
	partial := `{"path":`
	srv := newRxServer(t, rxEvents(
		rxAdded(0, rxCall("fc_1", "call_1", "read_file", "")), rxArgs(0, "fc_1", partial),
		rxDone(0, func() map[string]any {
			c := rxCall("fc_1", "call_1", "read_file", partial)
			c["status"] = "incomplete"
			return c
		}()),
		rxIncomplete("max_output_tokens", rxUsage(10, 0, 5, 0))))
	got := partKinds(stepParts(t, srv.model(t, chatgpt(), newStaticAuth(chatgptToken))))
	if want := []string{"tool_input_start:call_1", "tool_input_delta:call_1", "tool_input_end:call_1", "tool_call:call_1", "finish:error", "error"}; !slices.Equal(got, want) {
		t.Fatalf("parts = %v, want %v", got, want)
	}
}
