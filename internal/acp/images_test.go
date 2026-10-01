package acp

import (
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

// TestHeardIsTheAgentsOwnTrafficDuringThePrompt pins the signal the image
// resend is decided by (plan 033 §3.4): a prompt the agent refused with
// nothing before the refusal was not heard; anything the agent sent of its own
// while the prompt was in flight — an update, a request — makes it heard,
// because the read loop counts it before it delivers the reply behind it;
// grok's turn bookkeeping (queue/changed, prompt_complete) does not; traffic
// between prompts does not; and every prompt starts unheard.
func TestHeardIsTheAgentsOwnTrafficDuringThePrompt(t *testing.T) {
	blocks := imageTestBlocks("AAAA")
	cases := []struct {
		name    string
		dialect DialectID
		before  func(t *testing.T, p *rawPipe)
		heard   bool
	}{
		{"bare refusal", DialectCursor, func(*testing.T, *rawPipe) {}, false},
		{"an update first", DialectCursor, func(t *testing.T, p *rawPipe) {
			p.pushUpdate(t, "s1", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"looking"}}`)
		}, true},
		{"an update for another session", DialectCursor, func(t *testing.T, p *rawPipe) {
			p.pushUpdate(t, "s-other", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}`)
		}, true},
		{"a request first", DialectCursor, func(t *testing.T, p *rawPipe) {
			p.send(t, 77, "cursor/something_new", `{}`)
			if reply := p.readWithin(t, 3*time.Second, "the -32601"); reply.Error == nil || reply.Error.Code != CodeMethodNotFound {
				t.Fatalf("reply %+v", reply)
			}
		}, true},
		{"grok bookkeeping only", DialectGrok, func(t *testing.T, p *rawPipe) {
			p.send(t, nil, MethodGrokQueueChangedWrapped,
				`{"sessionId":"s1","entries":[],"runningPromptId":"p-1","runningText":"look at [Image #1]","runningKind":"prompt"}`)
			p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"p-other","stopReason":"end_turn"}`)
		}, false},
		{"a grok update first", DialectGrok, func(t *testing.T, p *rawPipe) {
			p.pushUpdate(t, "s1", `{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"hm"}}`)
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
			// The next prompt starts unheard, and traffic after a prompt has
			// returned belongs to no prompt.
			p.pushUpdate(t, "s1", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"late"}}`)
			p.roundTrip(t)
			if tc.heard == false && p.client.Heard() {
				t.Fatal("an update between prompts was counted as the last prompt's")
			}
			done, req = startPromptBlocks(t, p, blocks, nil, nil)
			refusePrompt(t, p, req)
			refusedPrompt(t, done)
			if p.client.Heard() {
				t.Fatal("a new prompt kept the last one's heard")
			}
		})
	}
}
