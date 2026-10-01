package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Images on the wire (plan 033 §3.4): the image block's shape, block 1 kept as
// the correlation text, the capability decode, and the Heard signal the -32602
// resend is decided by.

// imageTestBlocks is a prompt as craze sends one with an image: block 1, the
// image, the downscale note.
func imageTestBlocks(data string) []ContentBlock {
	return []ContentBlock{
		{Type: "text", Text: "look at [Image #1]"},
		{Type: "image", Data: data, MimeType: "image/png", URI: "file:///h/attachments/0123456789abcdef.png"},
		{Type: "text", Text: "[Image #1 was downscaled from 3024×1964 to 2000×1299]"},
	}
}

// TestImageBlockWireShape: an image block is ACP's image content — type,
// standard base64 data, mimeType and a file:// uri — in the one
// session/prompt, after block 1; a text block carries none of the image
// fields; and block 1 alone is the correlation text, on both dialects.
func TestImageBlockWireShape(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n not really"))
	want := `{"sessionId":"s1","prompt":[` +
		`{"type":"text","text":"look at [Image #1]"},` +
		`{"type":"image","data":"` + data + `","mimeType":"image/png","uri":"file:///h/attachments/0123456789abcdef.png"},` +
		`{"type":"text","text":"[Image #1 was downscaled from 3024×1964 to 2000×1299]"}]}`
	for _, d := range []DialectID{DialectCursor, DialectGrok} {
		t.Run(string(d), func(t *testing.T) {
			p := newRawPipeDialect(t, d)
			p.setSession("s1")
			done, req := startPromptBlocks(t, p, imageTestBlocks(data), nil, nil)
			if string(req.Params) != want {
				t.Fatalf("params\n got %s\nwant %s", req.Params, want)
			}
			p.client.mu.Lock()
			got := p.client.promptText
			p.client.mu.Unlock()
			if got != "look at [Image #1]" {
				t.Fatalf("promptText %q: block 1 is the correlation text with images behind it", got)
			}
			replyPrompt(t, p, req.ID, StopEndTurn)
			if out := <-done; out.err != nil {
				t.Fatal(out.err)
			}
		})
	}
}

// TestFirstBlockTextIgnoresImages: firstBlockText is block 1 whatever follows
// it — images, expansions, the note — so a prompt with images correlates as
// the same prompt without them (P6).
func TestFirstBlockTextIgnoresImages(t *testing.T) {
	with := imageTestBlocks("AAAA")
	if got, plain := firstBlockText(with), firstBlockText(with[:1]); got != plain || got != "look at [Image #1]" {
		t.Fatalf("firstBlockText %q with images, %q without", got, plain)
	}
}

// TestQueueChangedCorrelatesAPromptWithImages is grok's queue correlation with
// an image block appended: queue/changed names the prompt by block 1's text,
// the client learns its id from that, and the prompt's own completion settles
// it.
func TestQueueChangedCorrelatesAPromptWithImages(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, _ := startPromptBlocks(t, p, imageTestBlocks("AAAA"), nil, nil)
	p.send(t, nil, MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[],"runningPromptId":"p-1","runningText":"look at [Image #1]","runningKind":"prompt"}`)
	waitFor(t, func() bool { return p.client.PromptID() == "p-1" }, "promptID from block 1")
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"p-1","stopReason":"end_turn"}`)
	select {
	case out := <-done:
		if out.err != nil || out.res.StopReason != StopEndTurn {
			t.Fatalf("prompt: %+v, %v", out.res, out.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt never settled")
	}
}

func TestInitializePromptImageCapability(t *testing.T) {
	cases := []struct {
		name         string
		caps         string
		image, known bool
	}{
		{"cursor", `{"loadSession":true,"promptCapabilities":{"audio":false,"embeddedContext":false,"image":true}}`, true, true},
		{"grok", `{"loadSession":true,"promptCapabilities":{"image":false,"audio":false,"embeddedContext":true}}`, false, true},
		{"other fields of any type", `{"loadSession":"yes","promptCapabilities":{"image":true,"audio":7}}`, true, true},
		{"no promptCapabilities", `{"loadSession":true}`, false, false},
		{"no image", `{"promptCapabilities":{"audio":true}}`, false, false},
		{"image null", `{"promptCapabilities":{"image":null}}`, false, false},
		{"image a string", `{"promptCapabilities":{"image":"true"}}`, false, false},
		{"promptCapabilities not an object", `{"promptCapabilities":true}`, false, false},
		{"empty object", `{}`, false, false},
		{"no capabilities at all", ``, false, false},
		{"null", `null`, false, false},
		{"not an object", `["promptCapabilities"]`, false, false},
		{"malformed", `{"promptCapabilities":{"image":true`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := InitializeResult{AgentCapabilities: json.RawMessage(tc.caps)}
			image, known := res.PromptImage()
			if image != tc.image || known != tc.known {
				t.Fatalf("PromptImage() = (%v, %v) for %q, want (%v, %v)", image, known, tc.caps, tc.image, tc.known)
			}
		})
	}
}

// refusePrompt answers req with the -32602 an agent refusing the prompt's
// params sends.
func refusePrompt(t *testing.T, p *rawPipe, req *Message) {
	t.Helper()
	if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: req.ID, Error: &RPCError{Code: CodeInvalidParams, Message: "Invalid params"}}); err != nil {
		t.Fatal(err)
	}
}

// refusedPrompt waits for the prompt's return and fails unless it is the
// agent's own -32602.
func refusedPrompt(t *testing.T, done <-chan struct {
	res *PromptResult
	err error
}) {
	t.Helper()
	select {
	case out := <-done:
		var rpc *RPCError
		if !errors.As(out.err, &rpc) || rpc.Code != CodeInvalidParams {
			t.Fatalf("the prompt returned %+v, %v; want the -32602", out.res, out.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt never returned")
	}
}

// TestHeardIsTurnContentOnly pins the signal the image resend is decided by
// (plan 033 §3.4, C3r): a prompt the agent refused with nothing before the
// refusal was not heard; turn content the agent sent of its own before the
// refusal — an update of a turn kind, a request, cursor's todos and tasks,
// grok's turn and sub-agent events, grok naming the prompt in its queue —
// makes it heard, because the read loop counts it before it delivers the
// reply behind it; what is about the session and not the prompt does not —
// the catalog, mode, config, title, usage, the prompt's own echo, grok's model
// and session lists and its summaries (the C3 gate flake: the catalog read
// after the turn opened refused the resend); grok's bookkeeping for other
// prompts does not; and a kind nobody knows counts. Every prompt starts
// unheard.
func TestHeardIsTurnContentOnly(t *testing.T) {
	blocks := imageTestBlocks("AAAA")
	update := func(kind string) func(*testing.T, *rawPipe) {
		return func(t *testing.T, p *rawPipe) {
			p.pushUpdate(t, "s1", `{"sessionUpdate":"`+kind+`","content":{"type":"text","text":"x"}}`)
		}
	}
	grokNote := func(kind string) func(*testing.T, *rawPipe) {
		return func(t *testing.T, p *rawPipe) {
			p.send(t, nil, MethodGrokSessionNotificationWrapped, `{"sessionId":"s1","update":{"sessionUpdate":"`+kind+`"}}`)
		}
	}
	notify := func(method, params string) func(*testing.T, *rawPipe) {
		return func(t *testing.T, p *rawPipe) { p.send(t, nil, method, params) }
	}
	cases := []struct {
		name    string
		dialect DialectID
		before  func(t *testing.T, p *rawPipe)
		heard   bool
	}{
		{"bare refusal", DialectCursor, func(*testing.T, *rawPipe) {}, false},
		// Turn content.
		{"agent_message_chunk", DialectCursor, update(UpdateAgentMessage), true},
		{"agent_thought_chunk", DialectCursor, update(UpdateAgentThought), true},
		{"tool_call", DialectCursor, update(UpdateToolCall), true},
		{"tool_call_update", DialectCursor, update(UpdateToolCallUpd), true},
		{"plan", DialectCursor, update(UpdatePlan), true},
		{"an update for another session", DialectCursor, func(t *testing.T, p *rawPipe) {
			p.pushUpdate(t, "s-other", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}`)
		}, true},
		{"cursor todos", DialectCursor, notify(MethodCursorUpdateTodos, `{"toolCallId":"t1","todos":[]}`), true},
		{"cursor task", DialectCursor, notify(MethodCursorTask, `{"toolCallId":"t1"}`), true},
		{"a request first", DialectCursor, func(t *testing.T, p *rawPipe) {
			p.send(t, 77, "cursor/something_new", `{}`)
			if reply := p.readWithin(t, 3*time.Second, "the -32601"); reply.Error == nil || reply.Error.Code != CodeMethodNotFound {
				t.Fatalf("reply %+v", reply)
			}
		}, true},
		// The safe side: what is not known counts.
		{"an unknown update kind", DialectCursor, update("something_new"), true},
		{"an update with no kind", DialectCursor, func(t *testing.T, p *rawPipe) { p.pushUpdate(t, "s1", `{}`) }, true},
		{"an update that is not an object", DialectCursor, notify(MethodSessionUpdate, `[1]`), true},
		{"an unknown notification", DialectCursor, notify("cursor/something_new", `{}`), true},
		// About the session, not the prompt.
		{"available_commands_update", DialectCursor, func(t *testing.T, p *rawPipe) {
			p.pushUpdate(t, "s1", `{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"research","description":"x"}]}`)
		}, false},
		{"current_mode_update", DialectCursor, func(t *testing.T, p *rawPipe) {
			p.pushUpdate(t, "s1", `{"sessionUpdate":"current_mode_update","currentModeId":"plan"}`)
		}, false},
		{"config_option_update", DialectCursor, func(t *testing.T, p *rawPipe) {
			p.pushUpdate(t, "s1", `{"sessionUpdate":"config_option_update","configOptions":[]}`)
		}, false},
		{"session_info_update", DialectCursor, func(t *testing.T, p *rawPipe) {
			p.pushUpdate(t, "s1", `{"sessionUpdate":"session_info_update","title":"A title"}`)
		}, false},
		{"usage_update", DialectCursor, func(t *testing.T, p *rawPipe) {
			p.pushUpdate(t, "s1", `{"sessionUpdate":"usage_update","used":10,"size":100}`)
		}, false},
		{"the prompt's own echo", DialectGrok, update("user_message_chunk"), false},
		{"grok models/update", DialectGrok, notify(MethodGrokModelsUpdateWrapped, `{}`), false},
		{"grok models/update unwrapped", DialectGrok, notify(MethodGrokModelsUpdate, `{}`), false},
		{"grok sessions/changed", DialectGrok, notify(MethodGrokSessionsChangedWrapped, `{}`), false},
		{"grok last_turn_summary", DialectGrok, grokNote("last_turn_summary"), false},
		{"grok session_summary_generated", DialectGrok, grokNote("session_summary_generated"), false},
		// grok's own turn events.
		{"grok image_compressed", DialectGrok, grokNote("image_compressed"), true},
		{"grok response_completed", DialectGrok, grokNote("response_completed"), true},
		{"grok hook_execution", DialectGrok, grokNote("hook_execution"), true},
		{"grok an unknown session_notification", DialectGrok, grokNote("something_new"), true},
		{"grok subagent_spawned", DialectGrok, func(t *testing.T, p *rawPipe) {
			p.send(t, nil, MethodGrokSessionNotificationWrapped, `{"sessionId":"s1","update":{"sessionUpdate":"subagent_spawned","subagent_id":"c1","child_session_id":"c1"}}`)
		}, true},
		{"grok turn_completed", DialectGrok, func(t *testing.T, p *rawPipe) {
			p.send(t, nil, MethodGrokSessionNotificationWrapped, `{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p-9","stop_reason":"end_turn"}}`)
		}, true},
		{"a grok update first", DialectGrok, update(UpdateAgentThought), true},
		// grok's bookkeeping: only naming this prompt counts.
		{"grok bookkeeping for other prompts", DialectGrok, func(t *testing.T, p *rawPipe) {
			p.send(t, nil, MethodGrokQueueChangedWrapped,
				`{"sessionId":"s1","entries":[{"id":"p-0","text":"something else"}]}`)
			p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"interject-fallback-1","stopReason":"end_turn"}`)
		}, false},
		{"grok queue/changed naming the prompt by block 1", DialectGrok, func(t *testing.T, p *rawPipe) {
			p.send(t, nil, MethodGrokQueueChangedWrapped,
				`{"sessionId":"s1","entries":[],"runningPromptId":"p-1","runningText":"look at [Image #1]","runningKind":"prompt"}`)
		}, true},
		{"grok queue/changed naming the prompt by its joined text", DialectGrok, func(t *testing.T, p *rawPipe) {
			p.send(t, nil, MethodGrokQueueChangedWrapped,
				`{"sessionId":"s1","entries":[{"id":"p-1","text":"look at [Image #1]\n\n[Image #1 was downscaled from 3024×1964 to 2000×1299]"}]}`)
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, tc.dialect)
			p.setSession("s1")
			done, req := startPromptBlocks(t, p, blocks, nil, nil)
			tc.before(t, p)
			refusePrompt(t, p, req)
			refusedPrompt(t, done)
			if got := p.client.Heard(); got != tc.heard {
				t.Fatalf("Heard() = %v, want %v", got, tc.heard)
			}
			// The next prompt starts unheard, whatever the last one heard.
			done, req = startPromptBlocks(t, p, blocks, nil, nil)
			refusePrompt(t, p, req)
			refusedPrompt(t, done)
			if p.client.Heard() {
				t.Fatal("a new prompt kept the last one's heard")
			}
		})
	}
}

// goResendBlocks is goPromptBlocks for ResendBlocks.
func goResendBlocks(p *rawPipe, blocks []ContentBlock) <-chan struct {
	res *PromptResult
	err error
} {
	done := make(chan struct {
		res *PromptResult
		err error
	}, 1)
	go func() {
		res, err := p.client.ResendBlocks(context.Background(), blocks, nil, nil)
		done <- struct {
			res *PromptResult
			err error
		}{res, err}
	}()
	return done
}

// awaitPrompt is a prompt's return, failing the test instead of hanging when
// there is none within 5 s.
func awaitPrompt(t *testing.T, done <-chan struct {
	res *PromptResult
	err error
}) (*PromptResult, error) {
	t.Helper()
	select {
	case out := <-done:
		return out.res, out.err
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt never returned")
		return nil, nil
	}
}

// pathTextBlocks is imageTestBlocks' resend: the image as path text in its
// place.
func pathTextBlocks() []ContentBlock {
	b := imageTestBlocks("")
	b[1] = ContentBlock{Type: "text", Text: "[Image #1: /h/attachments/0123456789abcdef.png]"}
	return b
}

// TestAnUpdateAfterTheRefusalStopsTheResend is "error, then a late update"
// (plan 033 C3r, r1 #7): the agent refused with nothing before it, then said
// something. The prompt may have run after all, so the resend is refused
// before the wire — the check is ResendBlocks' own, in the section that opens
// its turn — and nothing more is written.
func TestAnUpdateAfterTheRefusalStopsTheResend(t *testing.T) {
	for _, d := range []DialectID{DialectCursor, DialectGrok} {
		t.Run(string(d), func(t *testing.T) {
			p := newRawPipeDialect(t, d)
			p.setSession("s1")
			done, req := startPromptBlocks(t, p, imageTestBlocks("AAAA"), nil, nil)
			refusePrompt(t, p, req)
			refusedPrompt(t, done)
			if p.client.Heard() {
				t.Fatal("heard before the late update")
			}
			p.pushUpdate(t, "s1", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"late"}}`)
			p.roundTrip(t)
			if !p.client.Heard() {
				t.Fatal("an update after the refusal was not counted")
			}
			if res, err := awaitPrompt(t, goResendBlocks(p, pathTextBlocks())); !errors.Is(err, ErrHeardSinceRefusal) {
				t.Fatalf("ResendBlocks = %+v, %v; want ErrHeardSinceRefusal", res, err)
			}
			p.noReply(t)
		})
	}
}

// TestALateCompletionDoesNotSettleTheResend is "error, then a late
// completion" on grok (plan 033 C3r, r1 #7): the refused prompt's id was never
// learned, so its prompt_complete cannot be told from the resend's by id —
// block 1 is the same text. Landing between the attempts it is a turn craze
// cannot place, so the prompt is heard and the resend refused; landing while
// the resend runs it does not settle the resend, whose own reply ends it.
func TestALateCompletionDoesNotSettleTheResend(t *testing.T) {
	t.Run("between the attempts", func(t *testing.T) {
		p := newRawPipeDialect(t, DialectGrok)
		p.setSession("s1")
		done, req := startPromptBlocks(t, p, imageTestBlocks("AAAA"), nil, nil)
		refusePrompt(t, p, req)
		refusedPrompt(t, done)
		p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"p-1","stopReason":"end_turn"}`)
		p.roundTrip(t)
		if res, err := awaitPrompt(t, goResendBlocks(p, pathTextBlocks())); !errors.Is(err, ErrHeardSinceRefusal) {
			t.Fatalf("ResendBlocks = %+v, %v; want ErrHeardSinceRefusal", res, err)
		}
		p.noReply(t)
	})
	t.Run("during the resend", func(t *testing.T) {
		p := newRawPipeDialect(t, DialectGrok)
		p.setSession("s1")
		done, req := startPromptBlocks(t, p, imageTestBlocks("AAAA"), nil, nil)
		refusePrompt(t, p, req)
		refusedPrompt(t, done)
		again := goResendBlocks(p, pathTextBlocks())
		resend := p.readWithin(t, 3*time.Second, "the resend")
		p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"p-1","stopReason":"cancelled"}`)
		p.roundTrip(t)
		select {
		case out := <-again:
			t.Fatalf("the refused prompt's late completion settled the resend: %+v, %v", out.res, out.err)
		default:
		}
		// The resend's own id, once learned, does settle it.
		p.send(t, nil, MethodGrokQueueChangedWrapped,
			`{"sessionId":"s1","entries":[],"runningPromptId":"p-2","runningText":"look at [Image #1]","runningKind":"prompt"}`)
		waitFor(t, func() bool { return p.client.PromptID() == "p-2" }, "the resend's own id")
		p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"p-2","stopReason":"end_turn"}`)
		select {
		case out := <-again:
			if out.err != nil || out.res.StopReason != StopEndTurn {
				t.Fatalf("resend: %+v, %v", out.res, out.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the resend never settled")
		}
		replyPrompt(t, p, resend.ID, StopEndTurn)
	})
	t.Run("the resend's reply ends it", func(t *testing.T) {
		p := newRawPipeDialect(t, DialectGrok)
		p.setSession("s1")
		done, req := startPromptBlocks(t, p, imageTestBlocks("AAAA"), nil, nil)
		refusePrompt(t, p, req)
		refusedPrompt(t, done)
		again := goResendBlocks(p, pathTextBlocks())
		resend := p.readWithin(t, 3*time.Second, "the resend")
		p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"p-1","stopReason":"cancelled"}`)
		replyPrompt(t, p, resend.ID, StopEndTurn)
		select {
		case out := <-again:
			if out.err != nil || out.res.StopReason != StopEndTurn {
				t.Fatalf("resend: %+v, %v; want its own reply's end_turn", out.res, out.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the resend never returned")
		}
	})
}

// TestARefusedPromptGrokNamedIsHeardAndRetired is "queue/changed before the
// error" (plan 033 C3r, r1 #7): grok named the prompt in its queue, so it took
// it — a refusal behind that is not resent (heard) — and the id is retired with
// the prompt, so neither its late completion nor a broadcast still naming it
// speaks for the next prompt, even one sent with the same text.
func TestARefusedPromptGrokNamedIsHeardAndRetired(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, req := startPromptBlocks(t, p, imageTestBlocks("AAAA"), nil, nil)
	p.send(t, nil, MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[{"id":"p-1","text":"look at [Image #1]"}]}`)
	waitFor(t, func() bool { return p.client.PromptID() == "p-1" }, "the refused prompt's id")
	refusePrompt(t, p, req)
	refusedPrompt(t, done)
	if !p.client.Heard() {
		t.Fatal("a prompt grok queued was not heard")
	}
	if res, err := awaitPrompt(t, goResendBlocks(p, pathTextBlocks())); !errors.Is(err, ErrHeardSinceRefusal) {
		t.Fatalf("ResendBlocks = %+v, %v; want ErrHeardSinceRefusal", res, err)
	}

	// The next prompt, same block 1: the retired id is not learned for it, and
	// its late completion does not end it.
	next, req := startPromptBlocks(t, p, imageTestBlocks("AAAA"), nil, nil)
	p.send(t, nil, MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[{"id":"p-1","text":"look at [Image #1]"}]}`)
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"p-1","stopReason":"end_turn"}`)
	p.roundTrip(t)
	if id := p.client.PromptID(); id != "" {
		t.Fatalf("the next prompt learned the retired id %q", id)
	}
	select {
	case out := <-next:
		t.Fatalf("the retired prompt's completion settled the next prompt: %+v, %v", out.res, out.err)
	default:
	}
	replyPrompt(t, p, req.ID, StopEndTurn)
	if res, err := awaitPrompt(t, next); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("next prompt: %+v, %v", res, err)
	}
}

// TestQueueChangedCorrelatesTheJoinedText is grok 1.0.44's queue shape (plan
// 033 V1 finding 2): an entry's text and runningText are every text block of
// the prompt joined by a blank line — block 1 with the downscale note, block 1
// with a plugin expansion — and the prompt's id is learned from either that or
// block 1 alone (grok 1.0.30).
func TestQueueChangedCorrelatesTheJoinedText(t *testing.T) {
	expansion := []ContentBlock{
		{Type: "text", Text: "/probe-plugin:probe-echo banana"},
		{Type: "text", Text: "<craze-plugin-command>body</craze-plugin-command>"},
	}
	cases := []struct {
		name   string
		blocks []ContentBlock
		queue  string
	}{
		{"the downscale note, queued", imageTestBlocks("AAAA"),
			`{"sessionId":"s1","entries":[{"id":"p-1","text":"look at [Image #1]\n\n[Image #1 was downscaled from 3024×1964 to 2000×1299]"}]}`},
		{"the downscale note, running", imageTestBlocks("AAAA"),
			`{"sessionId":"s1","entries":[],"runningPromptId":"p-1","runningText":"look at [Image #1]\n\n[Image #1 was downscaled from 3024×1964 to 2000×1299]"}`},
		{"a plugin expansion", expansion,
			`{"sessionId":"s1","entries":[{"id":"p-1","text":"/probe-plugin:probe-echo banana\n\n<craze-plugin-command>body</craze-plugin-command>"}]}`},
		{"block 1 alone (grok 1.0.30)", expansion,
			`{"sessionId":"s1","entries":[{"id":"p-1","text":"/probe-plugin:probe-echo banana"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, DialectGrok)
			p.setSession("s1")
			done, _ := startPromptBlocks(t, p, tc.blocks, nil, nil)
			p.send(t, nil, MethodGrokQueueChangedWrapped, tc.queue)
			waitFor(t, func() bool { return p.client.PromptID() == "p-1" }, "promptID from the queue text")
			// Only its own completion settles it now.
			p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"p-other","stopReason":"end_turn"}`)
			p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"p-1","stopReason":"end_turn"}`)
			select {
			case out := <-done:
				if out.err != nil || out.res.StopReason != StopEndTurn {
					t.Fatalf("prompt: %+v, %v", out.res, out.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the prompt never settled")
			}
		})
	}
}
