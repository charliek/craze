package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/charliek/craze/internal/acp"
)

// Images in prompts (craze plan 033 §3.4): what the fake advertises, and the
// scripts that show what craze sent.
//
// Every script advertises agentCapabilities.promptCapabilities in the shape its
// dialect's live agent does — cursor's image:true, grok 1.0.30's image:false —
// so craze's choice between image blocks and path text runs against both.
// prompt-dump (cursor) and grok-prompt-dump (grok) echo each block they
// received; reject-image refuses any prompt carrying an image block with
// -32602, the fallback's trigger, and echoes the resend. CRAZE_FAKE_DUMP_PROMPTS
// records every prompt and interjection any script receives, for a test that
// drives a script whose reply does not show its blocks.

// promptCapabilities is initialize's agentCapabilities.promptCapabilities for
// script, as the live agents answer it (craze plan 033 discovery: cursor
// 010/013 probes, grok 013 grok-load.log).
func promptCapabilities(script string) map[string]any {
	if grokScript(script) {
		return map[string]any{"image": false, "audio": false, "embeddedContext": true}
	}
	return map[string]any{"audio": false, "embeddedContext": false, "image": true}
}

// blockDump is one received prompt block as the dumps show it: the type, the
// text of a text block, and for an image its type, the length of its base64
// data, how many bytes that decodes to (-1 if it is not standard base64) and
// its uri.
type blockDump struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     int    `json:"data,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// dumpBlocks reads a session/prompt's params as blockDumps, in order; nil for
// params that are not a prompt.
func dumpBlocks(params json.RawMessage) []blockDump {
	var p acp.PromptParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil
	}
	out := make([]blockDump, 0, len(p.Prompt))
	for _, b := range p.Prompt {
		d := blockDump{Type: b.Type, Text: b.Text, MimeType: b.MimeType, URI: b.URI}
		if b.Data != "" {
			d.Data = len(b.Data)
			d.Bytes = -1
			if raw, err := base64.StdEncoding.DecodeString(b.Data); err == nil {
				d.Bytes = len(raw)
			}
		}
		out = append(out, d)
	}
	return out
}

// hasImage says a prompt carries at least one image block.
func hasImage(blocks []blockDump) bool {
	for _, b := range blocks {
		if b.Type == "image" {
			return true
		}
	}
	return false
}

// dumpReply is the text prompt-dump answers with: the prompt's number and its
// block count, then one line per block — its 1-based index and type, then a
// text block's text (Go-quoted, so it stays on its line) or an image's mime
// type, data=<base64 length>, bytes=<decoded length> and uri.
//
//	prompt 1: 2 blocks
//	1 text "look at [Image #1]"
//	2 image image/png data=1124 bytes=840 uri=file:///…/0123456789abcdef.png
func dumpReply(n int, blocks []blockDump) string {
	var b strings.Builder
	fmt.Fprintf(&b, "prompt %d: %d blocks\n", n, len(blocks))
	for i, d := range blocks {
		switch d.Type {
		case "text":
			fmt.Fprintf(&b, "%d text %q\n", i+1, d.Text)
		case "image":
			fmt.Fprintf(&b, "%d image %s data=%d bytes=%d uri=%s\n", i+1, d.MimeType, d.Data, d.Bytes, d.URI)
		default:
			fmt.Fprintf(&b, "%d %s\n", i+1, d.Type)
		}
	}
	return b.String()
}

// promptDump answers prompt n with dumpReply as one message chunk, then ends
// the turn: prompt-dump and grok-prompt-dump.
func (s *server) promptDump(id json.RawMessage, params json.RawMessage, n int) {
	s.say(dumpReply(n, dumpBlocks(params)))
	s.finishPrompt(id, acp.StopEndTurn)
}

// sayFirstMarker in a prompt's text makes reject-image say something before it
// refuses — a session update for the turn, which is what makes the refusal one
// craze must not resend.
const sayFirstMarker = "SAY-FIRST"

// rejectImage is reject-image's turn: a prompt carrying an image block is
// refused -32602, as an agent that takes no images would; any other prompt —
// the resend, its images as path text — is answered as prompt-dump answers.
func (s *server) rejectImage(id json.RawMessage, params json.RawMessage, n int) {
	blocks := dumpBlocks(params)
	if !hasImage(blocks) {
		s.promptDump(id, params, n)
		return
	}
	if strings.Contains(promptText(params), sayFirstMarker) {
		s.say("looking at it")
	}
	_ = s.conn.ReplyErr(id, &acp.RPCError{
		Code:    acp.CodeInvalidParams,
		Message: "Invalid params",
		Data:    json.RawMessage(`"image content is not supported"`),
	})
}

// dumpPromptFile is CRAZE_FAKE_DUMP_PROMPTS: every session/prompt's blocks
// ({"prompt":[…]}) and every x.ai/interject's text ({"interject":"…"}) is
// appended to that file as one JSON line, on the read loop, so the file is in
// arrival order whatever the scripts' handler goroutines do.
func dumpPromptFile(entry map[string]any) {
	p := os.Getenv("CRAZE_FAKE_DUMP_PROMPTS")
	if p == "" {
		return
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// dumpPrompt records a session/prompt in CRAZE_FAKE_DUMP_PROMPTS.
func dumpPrompt(params json.RawMessage) {
	if os.Getenv("CRAZE_FAKE_DUMP_PROMPTS") == "" {
		return
	}
	dumpPromptFile(map[string]any{"prompt": dumpBlocks(params)})
}

// dumpInterject records an x.ai/interject in CRAZE_FAKE_DUMP_PROMPTS.
func dumpInterject(params json.RawMessage) {
	if os.Getenv("CRAZE_FAKE_DUMP_PROMPTS") == "" {
		return
	}
	var p acp.InterjectParams
	_ = json.Unmarshal(params, &p)
	dumpPromptFile(map[string]any{"interject": p.Text})
}
