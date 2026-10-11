package messagesapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestStreamReadsARealCapture: a tool step exactly as Opus 5.5 streamed it
// (LF-3, testdata/opus-tool-step.sse: adaptive thinking summarized, effort
// medium, one tool) — a ping inside the thinking block, the signature in one
// signature_delta, the tool's first partial_json empty, JSON padded with
// spaces. Every block opens and stops, the thinking's text and signature
// and the call's input are whole, and the usage is message_delta's laid
// over message_start's.
func TestStreamReadsARealCapture(t *testing.T) {
	capture, err := os.ReadFile("testdata/opus-tool-step.sse")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(capture), "sk-ant") {
		t.Fatal("the capture holds a key")
	}
	srv := newServer(t, raw(string(capture)))
	evs, s, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"start0", "thinking0", "thinking0", "thinking0", "thinking0", "stop0",
		"start1", "input1", "input1", "input1", "stop1", "delta", "completed"}
	if got := kinds(evs); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	blocks := stops(evs)
	th, call := blocks[0], blocks[1]
	if th.Type != TypeThinking || th.Text != "I'll go ahead and read the go.mod file.\n\n" {
		t.Errorf("thinking = %+v", th)
	}
	if deltas(evs, ThinkingDelta, 0) != th.Text {
		t.Errorf("the thinking deltas add up to %q, not the block's text", deltas(evs, ThinkingDelta, 0))
	}
	if !strings.HasPrefix(th.Signature, "CAQS7QQKEAgSGAI4AUIIdGhpbmtpbmcS") || !strings.HasSuffix(th.Signature, "+N/7+sYAQ==") || len(th.Signature) != 840 {
		t.Errorf("signature = %d bytes %q…, want the capture's 840", len(th.Signature), th.Signature[:min(len(th.Signature), 32)])
	}
	if call.Type != TypeToolUse || call.ID != "toolu_01GzUYQdbrz3Y6pjVvjTYLRj" || call.Name != "read" || call.Input != `{"path": "go.mod"}` {
		t.Errorf("tool use = %+v", call)
	}
	if e := evs[6]; e.Block.ID != call.ID || e.Block.Name != "read" {
		t.Errorf("the tool's start = %+v, want its id and name", e.Block)
	}
	done := last(evs)
	if done.StopReason != "tool_use" || done.StopDetails != (StopDetails{}) {
		t.Errorf("completed = %+v", done)
	}
	u := done.Usage
	if u.InputTokens != 402 || u.CacheCreationInputTokens != 0 || u.CacheReadInputTokens != 0 || u.OutputTokens != 68 || u.ThinkingTokens != 19 ||
		!u.Has(HasInput|HasCacheCreation|HasCacheRead|HasOutput|HasThinking) {
		t.Errorf("usage = %+v", u)
	}
	if s.ID() != "msg_011Cfvn7uWcGiuQ51jimokBE" || s.Model() != "claude-opus-5-5" || s.RequestID() != testRequestID || s.InputTransformations() != nil {
		t.Errorf("stream facts: id %q, model %q, request %q, transformations %v", s.ID(), s.Model(), s.RequestID(), s.InputTransformations())
	}
}

// TestStreamTakesInitialContentFromTheStart (P4): a block's content starts
// with content_block_start's — a text block's text, a thinking block's text
// and signature, a redacted block's data — and its deltas append to it. The
// start's text is reported as a delta, so the deltas add up to the block.
func TestStreamTakesInitialContentFromTheStart(t *testing.T) {
	srv := newServer(t, sse(
		messageStart(),
		blockStart(0, thinkingBlock("Let me", "sig-a")),
		thinkingDelta(0, " think"),
		signatureDelta(0, "-b"),
		blockStop(0),
		blockStart(1, redactedBlock("opaque-data")),
		blockStop(1),
		blockStart(2, textBlock("Hel")),
		textDelta(2, "lo"),
		blockStop(2),
		messageDelta("end_turn", nil),
		messageStop(),
	))
	evs, _, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	got := stops(evs)
	want := []ContentBlock{
		{Type: TypeThinking, Text: "Let me think", Signature: "sig-a-b"},
		{Type: TypeRedactedThinking, Data: "opaque-data"},
		{Type: TypeText, Text: "Hello"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("blocks =\n%+v\nwant\n%+v", got, want)
	}
	if d := deltas(evs, TextDelta, 2); d != "Hello" {
		t.Errorf("text deltas = %q", d)
	}
	if d := deltas(evs, ThinkingDelta, 0); d != "Let me think" {
		t.Errorf("thinking deltas = %q", d)
	}
}

// TestStreamJoinsSignatureFragments (P4): several signature_deltas are one
// signature, in order; none is reported as a thinking delta.
func TestStreamJoinsSignatureFragments(t *testing.T) {
	srv := newServer(t, sse(
		messageStart(),
		blockStart(0, thinkingBlock("", "")),
		thinkingDelta(0, "a"),
		signatureDelta(0, "EqQB"),
		signatureDelta(0, "Ck0I"),
		signatureDelta(0, "Ag=="),
		blockStop(0),
		messageDelta("end_turn", nil),
		messageStop(),
	))
	evs, _, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	if b := stops(evs)[0]; b.Signature != "EqQBCk0IAg==" || b.Text != "a" {
		t.Fatalf("block = %+v", b)
	}
	if d := deltas(evs, ThinkingDelta, 0); d != "a" {
		t.Fatalf("thinking deltas = %q, want the text alone", d)
	}
}

// TestStreamKeepsASignedThinkingBlockWithNoText (P4): display omitted sends
// a thinking block with no text and a signature — a block all the same,
// reported with no thinking delta at all.
func TestStreamKeepsASignedThinkingBlockWithNoText(t *testing.T) {
	srv := newServer(t, sse(
		messageStart(),
		blockStart(0, thinkingBlock("", "")),
		signatureDelta(0, "sig-only"),
		blockStop(0),
		blockStart(1, textBlock("")),
		textDelta(1, "ok"),
		blockStop(1),
		messageDelta("end_turn", nil),
		messageStop(),
	))
	evs, _, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"start0", "stop0", "start1", "text1", "stop1", "delta", "completed"}; !slices.Equal(kinds(evs), want) {
		t.Fatalf("events = %v, want %v", kinds(evs), want)
	}
	if b := stops(evs)[0]; b != (ContentBlock{Type: TypeThinking, Signature: "sig-only"}) {
		t.Fatalf("block = %+v", b)
	}
}

// TestStreamOverwritesOnlyTheUsageReported (P4): message_start sets every
// count it has; each message_delta overwrites only the counts it has — an
// output-only delta keeps the input and cache counts, a null count is not
// one, and a later delta without thinking_tokens keeps the earlier one.
func TestStreamOverwritesOnlyTheUsageReported(t *testing.T) {
	srv := newServer(t, sse(
		messageStartWith(map[string]any{"usage": usageOf(100, 20, 30, 1)}),
		blockStart(0, textBlock("")),
		textDelta(0, "x"),
		blockStop(0),
		messageDelta(nil, map[string]any{"output_tokens": 10, "output_tokens_details": map[string]any{"thinking_tokens": 4}}),
		messageDelta("end_turn", map[string]any{"input_tokens": nil, "output_tokens": 50}),
		messageStop(),
	))
	evs, s, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{InputTokens: 100, CacheCreationInputTokens: 20, CacheReadInputTokens: 30, OutputTokens: 50, ThinkingTokens: 4,
		Present: HasInput | HasCacheCreation | HasCacheRead | HasOutput | HasThinking}
	if u := last(evs).Usage; u != want {
		t.Fatalf("usage = %+v, want %+v", u, want)
	}
	var deltasSeen []Usage
	for _, e := range evs {
		if e.Kind == MessageDelta {
			deltasSeen = append(deltasSeen, e.Usage)
		}
	}
	if len(deltasSeen) != 2 || deltasSeen[0].OutputTokens != 10 || deltasSeen[0].InputTokens != 100 || deltasSeen[0].ThinkingTokens != 4 {
		t.Fatalf("message deltas' usage = %+v", deltasSeen)
	}
	if s.Usage() != want {
		t.Fatalf("Usage() = %+v", s.Usage())
	}

	t.Run("an output-only start", func(t *testing.T) {
		srv := newServer(t, sse(
			messageStartWith(map[string]any{"usage": map[string]any{"output_tokens": 1}}),
			messageDelta("end_turn", map[string]any{"output_tokens": 3}),
			messageStop(),
		))
		evs, _, err := streamOf(t, srv)
		if err != nil {
			t.Fatal(err)
		}
		if u := last(evs).Usage; u != (Usage{OutputTokens: 3, Present: HasOutput}) || u.Has(HasInput) {
			t.Fatalf("usage = %+v, want the output count alone", u)
		}
	})
}

// TestStreamProtocolViolations (P4): every break in the protocol ends the
// stream with ErrIncomplete — a delta for an unknown or stopped block, a
// delta of another block's type, a second start, a stop for a block not
// open, message_delta or message_stop with a block open, events before
// message_start, a stream that ends before message_stop with a stop reason,
// and an event that is not JSON — never a success, and retryable as a
// truncated stream is (io.ErrUnexpectedEOF). The events before the break
// are still reported.
func TestStreamProtocolViolations(t *testing.T) {
	end := []string{messageDelta("end_turn", nil), messageStop()}
	text := func(idx int) []string {
		return []string{blockStart(idx, textBlock("")), textDelta(idx, "x"), blockStop(idx)}
	}
	cat := func(parts ...[]string) []string { return slices.Concat(parts...) }
	one := func(evs ...string) []string { return evs }
	for _, tc := range []struct {
		name string
		evs  []string
	}{
		{"a delta for a block never started", cat(one(messageStart()), one(textDelta(3, "x")), end)},
		{"a delta for a stopped block", cat(one(messageStart()), text(0), one(textDelta(0, "more")), end)},
		{"a text delta for a thinking block", cat(one(messageStart(), blockStart(0, thinkingBlock("", "")), textDelta(0, "x"), blockStop(0)), end)},
		{"a thinking delta for a text block", cat(one(messageStart(), blockStart(0, textBlock("")), thinkingDelta(0, "x"), blockStop(0)), end)},
		{"a signature for a text block", cat(one(messageStart(), blockStart(0, textBlock("")), signatureDelta(0, "s"), blockStop(0)), end)},
		{"input for a text block", cat(one(messageStart(), blockStart(0, textBlock("")), inputDelta(0, "{}"), blockStop(0)), end)},
		{"a thinking delta for a tool use", cat(one(messageStart(), blockStart(0, toolUseBlock("t1", "read")), thinkingDelta(0, "x"), blockStop(0)), end)},
		{"a thinking delta for redacted thinking", cat(one(messageStart(), blockStart(0, redactedBlock("d")), thinkingDelta(0, "x"), blockStop(0)), end)},
		{"a second start for an open block", cat(one(messageStart(), blockStart(0, textBlock("")), blockStart(0, textBlock("")), blockStop(0)), end)},
		{"a second start for a stopped block", cat(one(messageStart()), text(0), one(blockStart(0, textBlock(""))), one(blockStop(0)), end)},
		{"a stop for a block never started", cat(one(messageStart(), blockStop(0)), end)},
		{"a second stop", cat(one(messageStart()), text(0), one(blockStop(0)), end)},
		{"message_delta with a block open", one(messageStart(), blockStart(0, textBlock("")), messageDelta("end_turn", nil), blockStop(0), messageStop())},
		{"message_stop with a block open", one(messageStart(), messageDelta("end_turn", nil), blockStart(0, textBlock("")), messageStop())},
		{"a block before message_start", cat(text(0), end)},
		{"message_delta before message_start", end},
		{"a second message_start", cat(one(messageStart(), messageStart()), end)},
		{"message_stop with no stop reason", cat(one(messageStart()), text(0), one(messageStop()))},
		{"message_stop after a null stop reason", cat(one(messageStart()), text(0), one(messageDelta(nil, nil), messageStop()))},
		{"the end before message_stop", cat(one(messageStart()), text(0), one(messageDelta("end_turn", nil)))},
		{"the end inside a block", one(messageStart(), blockStart(0, textBlock("")), textDelta(0, "x"))},
		{"the end before anything", nil},
		{"a start with no index", cat(one(messageStart(), `{"type":"content_block_start","content_block":{"type":"text","text":""}}`), end)},
		{"a known block of another shape", cat(one(messageStart(), `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":7}}`), end)},
		{"an event that is not JSON", cat(one(messageStart(), `{"type":"content_block_start","index":0,`), end)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t, sse(tc.evs...))
			evs, _, err := streamOf(t, srv)
			if !errors.Is(err, ErrIncomplete) || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("err = %v (%T), want ErrIncomplete", err, err)
			}
			if slices.Contains(kinds(evs), "completed") {
				t.Fatal("a broken stream reported Completed")
			}
		})
	}
}

// TestStreamKeepsTheEventsBeforeABreak: the blocks finished before a
// violation are reported, then the stream fails.
func TestStreamKeepsTheEventsBeforeABreak(t *testing.T) {
	srv := newServer(t, sse(messageStart(), blockStart(0, textBlock("")), textDelta(0, "partial"), blockStop(0), textDelta(0, "late")))
	evs, s, err := streamOf(t, srv)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v", err)
	}
	if got := kinds(evs); !slices.Equal(got, []string{"start0", "text0", "stop0"}) {
		t.Fatalf("events = %v", got)
	}
	if !s.Usage().Has(HasInput) {
		t.Fatal("the usage message_start reported is gone")
	}
	if strings.Contains(err.Error(), "late") {
		t.Fatalf("the error %q quotes the stream", err)
	}
}

// TestStreamIgnoresPingAnywhere: a ping before message_start, inside a
// block, between blocks and before message_stop changes nothing.
func TestStreamIgnoresPingAnywhere(t *testing.T) {
	plain := []string{messageStart(), blockStart(0, textBlock("")), textDelta(0, "a"), blockStop(0),
		blockStart(1, toolUseBlock("t1", "read")), inputDelta(1, `{"p":1}`), blockStop(1),
		messageDelta("tool_use", map[string]any{"output_tokens": 5}), messageStop()}
	pinged := []string{ping(), messageStart(), ping(), blockStart(0, textBlock("")), ping(), textDelta(0, "a"), ping(), blockStop(0), ping(),
		blockStart(1, toolUseBlock("t1", "read")), ping(), inputDelta(1, `{"p":1}`), ping(), blockStop(1),
		messageDelta("tool_use", map[string]any{"output_tokens": 5}), ping(), messageStop()}
	want, _, err := streamOf(t, newServer(t, sse(plain...)))
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := streamOf(t, newServer(t, sse(pinged...)))
	if err != nil {
		t.Fatalf("a stream with pings failed: %v", err)
	}
	if !slices.EqualFunc(got, want, func(a, b Event) bool { return a == b }) {
		t.Fatalf("with pings: %v\nwithout: %v", kinds(got), kinds(want))
	}
}

// TestStreamErrorEvents: an error event fails the stream with an *Error
// typed as the event says, its status the type's, its request id the
// stream's header — before message_start (the streaming form of a 529) and
// after output alike; the events before it are reported, and the usage
// message_start reported is kept.
func TestStreamErrorEvents(t *testing.T) {
	t.Run("before message_start", func(t *testing.T) {
		srv := newServer(t, sse(errorEvent("overloaded_error", "Overloaded")))
		evs, s, err := streamOf(t, srv)
		e := asError(t, err)
		if !e.InStream || e.Type != "overloaded_error" || e.StatusCode != 529 || e.Message != "Overloaded" || e.RequestID != testRequestID || !e.Transient() {
			t.Fatalf("error = %+v", e)
		}
		if len(evs) != 0 || s.Usage().Present != 0 {
			t.Fatalf("events %v, usage %+v; want none", kinds(evs), s.Usage())
		}
	})
	t.Run("after output", func(t *testing.T) {
		srv := newServer(t, sse(messageStart(), blockStart(0, textBlock("")), textDelta(0, "par"),
			errorEvent("rate_limit_error", "slow down")))
		evs, s, err := streamOf(t, srv)
		e := asError(t, err)
		if e.StatusCode != 429 || e.Type != "rate_limit_error" || !e.InStream {
			t.Fatalf("error = %+v", e)
		}
		if got := kinds(evs); !slices.Equal(got, []string{"start0", "text0"}) {
			t.Fatalf("events = %v", got)
		}
		if u := s.Usage(); u.InputTokens != 10 || !u.Has(HasInput) {
			t.Fatalf("usage = %+v", u)
		}
	})
	t.Run("undescribed", func(t *testing.T) {
		srv := newServer(t, sse(`{"type":"error","error":"no object"}`))
		_, _, err := streamOf(t, srv)
		if e := asError(t, err); e.Type != "" || e.StatusCode != 0 || e.Transient() {
			t.Fatalf("error = %+v", e)
		}
	})
}

// TestStreamEmptyToolInputIsAnEmptyObject (P4): a tool use's input is its
// partial_json joined; none, or only empty fragments, is {}. Fragments are
// reported as input deltas, the empty ones left out.
func TestStreamEmptyToolInputIsAnEmptyObject(t *testing.T) {
	srv := newServer(t, sse(
		messageStart(),
		blockStart(0, toolUseBlock("t0", "noop")),
		blockStop(0),
		blockStart(1, toolUseBlock("t1", "noop")),
		inputDelta(1, ""),
		inputDelta(1, ""),
		blockStop(1),
		blockStart(2, toolUseBlock("t2", "read")),
		inputDelta(2, ""),
		inputDelta(2, `{"path":`),
		inputDelta(2, `"a b"}`),
		blockStop(2),
		messageDelta("tool_use", nil),
		messageStop(),
	))
	evs, _, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	var inputs []string
	for _, b := range stops(evs) {
		inputs = append(inputs, b.Input)
	}
	if want := []string{"{}", "{}", `{"path":"a b"}`}; !slices.Equal(inputs, want) {
		t.Fatalf("inputs = %q, want %q", inputs, want)
	}
	if got := kinds(evs); slices.Contains(got, "input0") || slices.Contains(got, "input1") || deltas(evs, InputDelta, 2) != `{"path":"a b"}` {
		t.Fatalf("events = %v", got)
	}
}

// TestStreamSkipsWhatItDoesNotKnow: an event type craze does not know (with
// fields of any shape), a block of an unknown type with its deltas and its
// stop, and a delta type it does not know on a known block are passed over
// (forward compatibility); the rest of the message reads as if they were
// not there.
func TestStreamSkipsWhatItDoesNotKnow(t *testing.T) {
	srv := newServer(t, sse(
		`{"type":"message_flourish","index":"seven","delta":[1,2]}`,
		messageStart(),
		blockStart(0, map[string]any{"type": "server_tool_use", "id": "srv_1", "name": "web_search", "input": map[string]any{"q": "x"}, "text": 7}),
		inputDelta(0, `{"q":"x"}`),
		blockStop(0),
		blockStart(1, textBlock("")),
		blockDelta(1, map[string]any{"type": "citations_delta", "citation": map[string]any{"cited_text": 7}}),
		textDelta(1, "answer"),
		blockStop(1),
		`{"type":"content_block_glimmer","index":1}`,
		messageDelta("end_turn", nil),
		messageStop(),
	))
	evs, _, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"start1", "text1", "stop1", "delta", "completed"}; !slices.Equal(kinds(evs), want) {
		t.Fatalf("events = %v, want %v", kinds(evs), want)
	}
	if b := stops(evs)[0]; b.Text != "answer" {
		t.Fatalf("text = %q", b.Text)
	}
}

// TestStreamKeepsMessageStartFacts (P4): message_start's id, model and
// input_transformations are kept on the stream — each value kept only in
// the shape it should have.
func TestStreamKeepsMessageStartFacts(t *testing.T) {
	srv := newServer(t, sse(
		messageStartWith(map[string]any{
			"id":    "msg_abc",
			"model": "claude-opus-5-5",
			"input_transformations": []any{
				map[string]any{"type": "thinking_dropped", "path": "messages.1.content.0", "reason": "prefix_binding_mismatch"},
				map[string]any{"type": "Thinking Dropped!", "path": "messages 1", "reason": 7},
			},
		}),
		messageDelta("end_turn", nil),
		messageStop(),
	))
	_, s, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	want := []InputTransformation{{Type: "thinking_dropped", Path: "messages.1.content.0", Reason: "prefix_binding_mismatch"}, {}}
	if s.ID() != "msg_abc" || s.Model() != "claude-opus-5-5" || !slices.Equal(s.InputTransformations(), want) {
		t.Fatalf("id %q, model %q, transformations %+v", s.ID(), s.Model(), s.InputTransformations())
	}

	srv = newServer(t, sse(messageStartWith(map[string]any{"id": "msg with spaces", "model": "../etc"}), messageDelta("end_turn", nil), messageStop()))
	if _, s, err = streamOf(t, srv); err != nil || s.ID() != "" || s.Model() != "" {
		t.Fatalf("id %q, model %q, err %v; want both dropped", s.ID(), s.Model(), err)
	}
}

// TestStreamReportsBlocksInWireOrder (P1): thinking, a call, thinking, a
// call — each block is reported at its own stop, in the stream's order, so
// a call is known before the next block starts and the stored step keeps
// the wire's order.
func TestStreamReportsBlocksInWireOrder(t *testing.T) {
	srv := newServer(t, sse(
		messageStart(),
		blockStart(0, thinkingBlock("", "")), thinkingDelta(0, "first"), signatureDelta(0, "s0"), blockStop(0),
		blockStart(1, toolUseBlock("tA", "read")), inputDelta(1, `{"a":1}`), blockStop(1),
		blockStart(2, thinkingBlock("", "")), thinkingDelta(2, "second"), signatureDelta(2, "s2"), blockStop(2),
		blockStart(3, toolUseBlock("tB", "read")), inputDelta(3, `{"b":2}`), blockStop(3),
		messageDelta("tool_use", nil), messageStop(),
	))
	evs, _, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, e := range evs {
		switch e.Kind {
		case BlockStart:
			order = append(order, "start:"+e.Block.Type)
		case BlockStop:
			order = append(order, "stop:"+e.Block.Type+":"+e.Block.ID+e.Block.Signature)
		}
	}
	want := []string{"start:thinking", "stop:thinking:s0", "start:tool_use", "stop:tool_use:tA",
		"start:thinking", "stop:thinking:s2", "start:tool_use", "stop:tool_use:tB"}
	if !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// TestStreamStopDetails: a refusal's category is kept as an identifier; a
// category of another shape is dropped; a later null stop_details or
// stop_reason keeps the earlier one.
func TestStreamStopDetails(t *testing.T) {
	refusal := func(category any) string {
		return ev(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "refusal",
			"stop_details": map[string]any{"type": "refusal", "category": category, "explanation": "not this"}}})
	}
	for _, tc := range []struct {
		name string
		evs  []string
		want StopDetails
	}{
		{"a category", []string{refusal("cyber")}, StopDetails{Category: "cyber"}},
		{"none", []string{refusal(nil)}, StopDetails{}},
		{"not an identifier", []string{refusal("Cyber; rm -rf")}, StopDetails{}},
		{"kept through a null", []string{refusal("bio"), messageDelta(nil, map[string]any{"output_tokens": 9})}, StopDetails{Category: "bio"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t, sse(slices.Concat([]string{messageStart()}, tc.evs, []string{messageStop()})...))
			evs, _, err := streamOf(t, srv)
			if err != nil {
				t.Fatal(err)
			}
			if done := last(evs); done.StopReason != "refusal" || done.StopDetails != tc.want {
				t.Fatalf("completed = %+v, want refusal %+v", done, tc.want)
			}
		})
	}
}

// TestStreamEndsAtMessageStop: nothing after message_stop is read — not
// even an event that would have broken the protocol.
func TestStreamEndsAtMessageStop(t *testing.T) {
	srv := newServer(t, raw("event: message_start\ndata: "+messageStart()+"\n\n"+
		"data: "+messageDelta("end_turn", nil)+"\n\n"+
		"data: "+messageStop()+"\n\n"+
		"data: {not json\n\n"))
	evs, _, err := streamOf(t, srv)
	if err != nil || last(evs).Kind != Completed {
		t.Fatalf("events %v, err %v; want Completed", kinds(evs), err)
	}
}

// TestStreamReadsAnyFraming: a whole message framed with CRLF, comments and
// a data line split in two, its message_stop the last line with no blank
// line after it, reads to its completion.
func TestStreamReadsAnyFraming(t *testing.T) {
	srv := newServer(t, raw(": keep-alive\r\n"+
		"event: message_start\r\ndata: "+messageStart()+"\r\n\r\n"+
		"data: {\"type\":\"content_block_start\",\"index\":0,\r\ndata: \"content_block\":{\"type\":\"text\",\"text\":\"\"}}\r\n\r\n"+
		": another\r\n"+
		"data: "+textDelta(0, "line one\nline two")+"\r\n\r\n"+
		"data: "+blockStop(0)+"\r\n\r\n"+
		"data: "+messageDelta("end_turn", nil)+"\r\n\r\n"+
		"data: "+messageStop()))
	evs, _, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	if b := stops(evs); len(b) != 1 || b[0].Text != "line one\nline two" || last(evs).Kind != Completed {
		t.Fatalf("events %v, blocks %+v", kinds(evs), b)
	}
}

// TestStreamDispatchesOnTheDataType: the JSON's type decides, whatever the
// SSE event's name says; the name stands in only for data that names no
// type.
func TestStreamDispatchesOnTheDataType(t *testing.T) {
	srv := newServer(t, raw("event: ping\ndata: "+messageStart()+"\n\n"+
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"+
		"event: ping\ndata: "+messageStop()+"\n\n"))
	evs, _, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(evs); !slices.Equal(got, []string{"delta", "completed"}) || last(evs).StopReason != "end_turn" {
		t.Fatalf("events = %v", got)
	}
}

// TestStreamHonoursCancellation: once the context is done no further event
// is reported and Err is the context's error — for a stream still being
// written, and for one already buffered whole, where nothing left to read
// would fail.
func TestStreamHonoursCancellation(t *testing.T) {
	t.Run("mid stream", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: "+messageStart()+"\n\ndata: "+blockStart(0, textBlock("hi"))+"\n\n")
			w.(http.Flusher).Flush()
			<-release
		})
		ctx, cancel := context.WithCancel(context.Background())
		s, err := srv.client(t).Stream(ctx, minimalRequest())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if !s.Next() || s.Event().Kind != BlockStart {
			t.Fatalf("first event = %+v, err %v", s.Event(), s.Err())
		}
		cancel()
		if s.Next() {
			t.Fatalf("an event after the cancel: %+v", s.Event())
		}
		if !errors.Is(s.Err(), context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", s.Err())
		}
	})
	t.Run("buffered whole", func(t *testing.T) {
		srv := newServer(t, textReply("done"))
		ctx, cancel := context.WithCancel(context.Background())
		s, err := srv.client(t).Stream(ctx, minimalRequest())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if !s.Next() {
			t.Fatal(s.Err())
		}
		cancel()
		for s.Next() {
			if s.Event().Kind == Completed {
				t.Fatal("Completed after the cancel")
			}
		}
		if !errors.Is(s.Err(), context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", s.Err())
		}
	})
}

// TestStreamRefusesMalformedKnownEvents (P4, review c1 F2): an event of a
// type craze reads must have the fields it reads, as what they should be —
// a delta's content a present JSON string (null, absent or a number is not
// an empty fragment), a tool use's id and name, a redacted block's data, a
// message_stop that says it is one — or the stream is ErrIncomplete, naming
// the rule; it never completes with a call or a signature a mangled event
// emptied.
func TestStreamRefusesMalformedKnownEvents(t *testing.T) {
	end := []string{messageDelta("tool_use", nil), messageStop()}
	tool := func(delta map[string]any) []string {
		return slices.Concat([]string{messageStart(), blockStart(0, toolUseBlock("t1", "read")), blockDelta(0, delta), blockStop(0)}, end)
	}
	thinking := func(delta map[string]any) []string {
		return slices.Concat([]string{messageStart(), blockStart(0, thinkingBlock("", "")), blockDelta(0, delta), blockStop(0)}, end)
	}
	text := func(delta map[string]any) []string {
		return slices.Concat([]string{messageStart(), blockStart(0, textBlock("")), blockDelta(0, delta), blockStop(0)}, end)
	}
	frame := func(evs []string) string {
		var b strings.Builder
		for _, e := range evs {
			if strings.HasPrefix(e, "event:") {
				b.WriteString(e + "\n\n") // a hand-framed event, its name and data as given
				continue
			}
			typ, _ := typeOf([]byte(e))
			b.WriteString("event: " + typ + "\ndata: " + e + "\n\n")
		}
		return b.String()
	}
	stopped := []string{messageStart(), blockStart(0, textBlock("")), textDelta(0, "x"), blockStop(0), messageDelta("end_turn", nil)}
	for _, tc := range []struct {
		name string
		evs  []string
		rule string
	}{
		{"partial_json null", tool(map[string]any{"type": "input_json_delta", "partial_json": nil}), "partial_json is not a string"},
		{"partial_json absent", tool(map[string]any{"type": "input_json_delta"}), "partial_json is not a string"},
		{"partial_json a number", tool(map[string]any{"type": "input_json_delta", "partial_json": 7}), "partial_json is not a string"},
		{"text null", text(map[string]any{"type": "text_delta", "text": nil}), "text is not a string"},
		{"text absent", text(map[string]any{"type": "text_delta"}), "text is not a string"},
		{"thinking null", thinking(map[string]any{"type": "thinking_delta", "thinking": nil}), "thinking is not a string"},
		{"signature null", thinking(map[string]any{"type": "signature_delta", "signature": nil}), "signature is not a string"},
		{"signature absent", thinking(map[string]any{"type": "signature_delta"}), "signature is not a string"},
		{"message_stop null", append(slices.Clone(stopped), "event: message_stop\ndata: null"), "not a JSON object"},
		{"message_stop naming nothing", append(slices.Clone(stopped), "event: message_stop\ndata: {}"), "not a message_stop object"},
		{"message_delta null", slices.Concat([]string{messageStart()}, []string{"event: message_delta\ndata: null", messageStop()}), "not a JSON object"},
		{"a tool use with no id", slices.Concat([]string{messageStart(), blockStart(0, map[string]any{"type": "tool_use", "name": "read", "input": map[string]any{}}), blockStop(0)}, end), "no id or no name"},
		{"a tool use with a null name", slices.Concat([]string{messageStart(), blockStart(0, map[string]any{"type": "tool_use", "id": "t1", "name": nil, "input": map[string]any{}}), blockStop(0)}, end), "no id or no name"},
		{"redacted thinking with no data", slices.Concat([]string{messageStart(), blockStart(0, map[string]any{"type": "redacted_thinking"}), blockStop(0)}, end), "with no data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs, _, err := streamOf(t, newServer(t, raw(frame(tc.evs))))
			if !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), tc.rule) {
				t.Fatalf("err = %v, want ErrIncomplete naming %q", err, tc.rule)
			}
			if slices.Contains(kinds(evs), "completed") {
				t.Fatal("a malformed stream completed")
			}
		})
	}
}

// TestStreamAcceptsEmptyFragments (review c1 F2, the control): a delta
// whose content is "" — every type, as the API sends a tool's first
// fragment (LF-3) — is valid, and adds nothing.
func TestStreamAcceptsEmptyFragments(t *testing.T) {
	srv := newServer(t, sse(
		messageStart(),
		blockStart(0, thinkingBlock("", "")), thinkingDelta(0, ""), signatureDelta(0, ""), signatureDelta(0, "sig"), blockStop(0),
		blockStart(1, textBlock("")), textDelta(1, ""), textDelta(1, "hi"), blockStop(1),
		blockStart(2, toolUseBlock("t1", "read")), inputDelta(2, ""), blockStop(2),
		messageDelta("tool_use", nil), messageStop(),
	))
	evs, _, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	want := []ContentBlock{{Type: TypeThinking, Signature: "sig"}, {Type: TypeText, Text: "hi"}, {Type: TypeToolUse, ID: "t1", Name: "read", Input: "{}"}}
	if got := stops(evs); !slices.Equal(got, want) {
		t.Fatalf("blocks = %+v, want %+v", got, want)
	}
}

// TestStreamErrorKeepsTheStreamsRetryDirective (P2, review c1 F3): an
// error event inside a 200 whose headers said x-should-retry: false is not
// transient — the server's word on the stream holds for the failure in it —
// and keeps the stream's retry-after. The control: the same event on a
// stream that said nothing is transient, with no directive.
func TestStreamErrorKeepsTheStreamsRetryDirective(t *testing.T) {
	reply := sse(messageStart(), errorEvent("overloaded_error", "Overloaded"))
	_, _, err := streamOf(t, newServer(t, withHeaders(reply, "X-Should-Retry", "false", "Retry-After", "3")))
	e := asError(t, err)
	if !e.InStream || e.StatusCode != 529 || e.ShouldRetry == nil || *e.ShouldRetry || e.Transient() || e.RetryAfter != 3*time.Second {
		t.Fatalf("error = %+v (transient %v), want x-should-retry false kept", e, e.Transient())
	}
	_, _, err = streamOf(t, newServer(t, reply))
	if e := asError(t, err); e.ShouldRetry != nil || !e.Transient() || e.RetryAfter != 0 {
		t.Fatalf("with no directive: %+v (transient %v)", e, e.Transient())
	}
}

// TestStreamKeepsAToolsStartInput (review c1 F4): a tool use whose start
// carries its input whole — an object other than {} — completes with it,
// compacted, reported as its one input delta; an empty fragment after it is
// nothing; a fragment that is more would make the joined input no JSON, and
// is a violation; an input that is no object is one too — null included,
// a value rather than an absent field (review c1r2 5). The control: the {}
// placeholder, or no input field, still joins the fragments as before.
func TestStreamKeepsAToolsStartInput(t *testing.T) {
	start := func(input any) string {
		return blockStart(0, map[string]any{"type": "tool_use", "id": "t1", "name": "read", "input": input})
	}
	for _, tc := range []struct {
		name   string
		evs    []string
		input  string
		broken bool
	}{
		{"whole at the start", []string{start(map[string]any{"path": "go.mod"})}, `{"path":"go.mod"}`, false},
		{"whole, then an empty fragment", []string{start(map[string]any{"path": "go.mod"}), inputDelta(0, "")}, `{"path":"go.mod"}`, false},
		{"whole, then more", []string{start(map[string]any{"path": "go.mod"}), inputDelta(0, `{"x":1}`)}, "", true},
		{"no object", []string{start("go.mod")}, "", true},
		{"an array", []string{start([]any{1})}, "", true},
		{"the placeholder, then fragments", []string{start(map[string]any{}), inputDelta(0, `{"path":`), inputDelta(0, `"a"}`)}, `{"path":"a"}`, false},
		{"null", []string{start(nil)}, "", true},
		{"none, then fragments", []string{blockStart(0, map[string]any{"type": "tool_use", "id": "t1", "name": "read"}), inputDelta(0, `{"p":1}`)}, `{"p":1}`, false},
		{"none at all", []string{blockStart(0, map[string]any{"type": "tool_use", "id": "t1", "name": "read"})}, "{}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs := slices.Concat([]string{messageStart()}, tc.evs, []string{blockStop(0), messageDelta("tool_use", nil), messageStop()})
			got, _, err := streamOf(t, newServer(t, sse(evs...)))
			if tc.broken {
				if !errors.Is(err, ErrIncomplete) {
					t.Fatalf("err = %v, want ErrIncomplete", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if b := stops(got); len(b) != 1 || b[0].Input != tc.input {
				t.Fatalf("blocks = %+v, want input %s", b, tc.input)
			}
			if d := deltas(got, InputDelta, 0); tc.input != "{}" && d != tc.input {
				t.Fatalf("input deltas = %q, want %q", d, tc.input)
			}
		})
	}
}

// TestStreamRefusesAnOversizedEvent: an event past the reader's bound ends
// the stream with the reader's own refusal, as it is — no transport failure
// to scrub, and not an incomplete stream to retry.
func TestStreamRefusesAnOversizedEvent(t *testing.T) {
	old := maxEventBytes
	maxEventBytes = 1 << 10
	defer func() { maxEventBytes = old }()
	srv := newServer(t, sse(messageStart(), blockStart(0, textBlock("")), textDelta(0, strings.Repeat("x", 2<<10))))
	_, _, err := streamOf(t, srv)
	if !errors.Is(err, errEventTooLarge) || errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want errEventTooLarge", err)
	}
}
