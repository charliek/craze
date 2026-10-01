package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/harness/tool/attach"
	"github.com/charliek/craze/internal/paths"
)

// Images on the ACP prompt path (plan 033 §3.4, C3): promptBlocks' order and
// shapes, the capability and dialect decision, the -32602 path-text resend,
// host-read refusals as path text, and interjections as text on both sessions
// (P7). Native's prompts send their images as file parts
// (native_images_test.go).

// imageHome points CRAZE_HOME at a fresh directory, so paths.AttachmentsDir is
// this test's own.
func imageHome(t *testing.T) {
	t.Helper()
	t.Setenv("CRAZE_HOME", t.TempDir())
}

// savedImage stores a w×h PNG in the test's attachments directory as the TUI
// stores one, and returns its envelope ref as chip n, and its bytes.
func savedImage(t *testing.T, n, w, h int) (AttachmentRef, []byte) {
	t.Helper()
	data := testPNG(t, w, h)
	path, err := attach.Save(paths.AttachmentsDir(), data, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	return AttachmentRef{N: n, Path: path, MIME: "image/png"}, data
}

// promptDumpFile points the fake agent's CRAZE_FAKE_DUMP_PROMPTS at a fresh
// file and returns it: every prompt and interjection the fake reads.
func promptDumpFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prompts.jsonl")
	t.Setenv("CRAZE_FAKE_DUMP_PROMPTS", p)
	return p
}

// dumpedBlock is one block of a dumped prompt (the fake's blockDump).
type dumpedBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	MimeType string `json:"mimeType"`
	Data     int    `json:"data"`
	Bytes    int    `json:"bytes"`
	URI      string `json:"uri"`
}

// dumpedPrompts is every session/prompt the fake read, in arrival order.
func dumpedPrompts(t *testing.T, path string) [][]dumpedBlock {
	t.Helper()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]dumpedBlock
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var line struct {
			Prompt *[]dumpedBlock `json:"prompt"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("a dump line is not JSON: %v: %s", err, sc.Bytes())
		}
		if line.Prompt != nil {
			out = append(out, *line.Prompt)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func blockTypes(blocks []dumpedBlock) string {
	types := make([]string, len(blocks))
	for i, b := range blocks {
		types[i] = b.Type
	}
	return strings.Join(types, ",")
}

// --- promptBlocks -------------------------------------------------------

// imageAtts is two read attachments: #1 downscaled (its envelope claims a
// larger original), #2 not.
func imageAtts(t *testing.T) []Attachment {
	t.Helper()
	return []Attachment{
		{AttachmentRef: AttachmentRef{N: 1, Path: "/h/attachments/0123456789abcdef.png", MIME: "image/png", OW: 3024, OH: 1964},
			Data: testPNG(t, 20, 13), Width: 2000, Height: 1299},
		{AttachmentRef: AttachmentRef{N: 2, Path: "/h/attachments/fedcba9876543210.jpg", MIME: "image/jpeg"},
			Data: testJPEG(t, 16, 16), Width: 16, Height: 16},
	}
}

// TestPromptBlocksPutImagesAfterTheExpansions pins the order (plan 033 §3.4,
// P6): block 1 the draft as typed, then the expansions in reference order,
// then one image block per attachment in chip order — base64 of the bytes,
// the type, a file:// uri — then the one note for the downscaled image. Block
// 1 is the draft exactly, so the correlation text is what it would be with no
// images.
func TestPromptBlocksPutImagesAfterTheExpansions(t *testing.T) {
	entries := []PluginEntry{
		entryOf("p", "one", PluginKindCommand, "first body"),
		entryOf("p", "two", PluginKindSkill, "second body"),
	}
	draft := "/one a [Image #1]\n/two b [Image #2]"
	atts := imageAtts(t)
	blocks, cmds := promptBlocks(draft, pluginRefs(draft, lookupOf(entries)), promptImages{atts: atts})
	if got := blockTypesOf(blocks); got != "text,text,text,image,image,text" {
		t.Fatalf("block types %s", got)
	}
	if blocks[0].Text != draft || acpFirstBlock(blocks) != draft {
		t.Fatalf("block 1 %q, want the draft as typed", blocks[0].Text)
	}
	if !strings.Contains(blocks[1].Text, "first body") || !strings.Contains(blocks[2].Text, "second body") || len(cmds) != 2 {
		t.Fatalf("expansions out of place: %q %q (%d commands)", blocks[1].Text, blocks[2].Text, len(cmds))
	}
	for i, a := range atts {
		b := blocks[3+i]
		raw, err := base64.StdEncoding.DecodeString(b.Data)
		if err != nil || string(raw) != string(a.Data) {
			t.Fatalf("image %d data does not decode to the file's bytes (%v)", a.N, err)
		}
		if b.MimeType != a.MIME || b.URI != "file://"+a.Path || b.Text != "" {
			t.Fatalf("image %d block %+v", a.N, acp.ContentBlock{Type: b.Type, MimeType: b.MimeType, URI: b.URI, Text: b.Text})
		}
	}
	if note := blocks[5].Text; note != "[Image #1 was downscaled from 3024×1964 to 2000×1299]" {
		t.Fatalf("the note is %q", note)
	}
	// No images, no image blocks and no note: the prompt is what it always was.
	plain, _ := promptBlocks(draft, nil, promptImages{})
	if len(plain) != 1 || plain[0].Text != draft {
		t.Fatalf("a prompt with no images: %+v", plain)
	}
}

// blockTypesOf is the blocks' types, comma-joined.
func blockTypesOf(blocks []acp.ContentBlock) string {
	types := make([]string, len(blocks))
	for i, b := range blocks {
		types[i] = b.Type
	}
	return strings.Join(types, ",")
}

// acpFirstBlock is what acp's firstBlockText reads: block 1 when it is text.
func acpFirstBlock(blocks []acp.ContentBlock) string {
	if len(blocks) == 0 || blocks[0].Type != "text" {
		return ""
	}
	return blocks[0].Text
}

// TestPromptBlocksPathTextInsteadOfImages: for an agent that takes no image
// blocks (and for the resend), each image becomes [Image #N: <path>] text in
// its block's place, block 1 untouched, the note kept.
func TestPromptBlocksPathTextInsteadOfImages(t *testing.T) {
	draft := "look at [Image #1] and [Image #2]"
	blocks, _ := promptBlocks(draft, nil, promptImages{atts: imageAtts(t), asText: true})
	want := []acp.ContentBlock{
		{Type: "text", Text: draft},
		{Type: "text", Text: "[Image #1: /h/attachments/0123456789abcdef.png]"},
		{Type: "text", Text: "[Image #2: /h/attachments/fedcba9876543210.jpg]"},
		{Type: "text", Text: "[Image #1 was downscaled from 3024×1964 to 2000×1299]"},
	}
	if fmt.Sprint(blocks) != fmt.Sprint(want) {
		t.Fatalf("blocks\n got %+v\nwant %+v", blocks, want)
	}
}

// TestPromptBlocksPutFallbacksInBlockOne: an image the host could not attach
// is path text inside block 1 — after a leading shell block, before the
// user's words — and an image that was read still rides behind.
func TestPromptBlocksPutFallbacksInBlockOne(t *testing.T) {
	shell := ShellContextBlock([]ShellResult{{Command: "ls", Output: "a\n"}})
	fallback := "[Image #2: /h/attachments/fedcba9876543210.jpg (not attached: no longer available)]"
	atts := imageAtts(t)[:1]
	atts[0].OW, atts[0].OH = 0, 0
	blocks, _ := promptBlocks(shell+"see [Image #1] and [Image #2]", nil, promptImages{atts: atts, fallbacks: []string{fallback}})
	if got := blockTypesOf(blocks); got != "text,image" {
		t.Fatalf("block types %s", got)
	}
	if want := shell + fallback + "\nsee [Image #1] and [Image #2]"; blocks[0].Text != want {
		t.Fatalf("block 1\n got %q\nwant %q", blocks[0].Text, want)
	}
}

func TestImageBlockURIIsAFileURL(t *testing.T) {
	for path, want := range map[string]string{
		"/home/u/.craze/attachments/0123456789abcdef.png": "file:///home/u/.craze/attachments/0123456789abcdef.png",
		"/a b/c#d/0123456789abcdef.png":                   "file:///a%20b/c%23d/0123456789abcdef.png",
	} {
		if got := fileURI(path); got != want {
			t.Errorf("fileURI(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestImagesDespiteCapabilityIsGrokAndGx: the dialect flag is on for grok and
// gx (P5) and off for cursor; native never reaches the ACP path.
func TestImagesDespiteCapabilityIsGrokAndGx(t *testing.T) {
	for _, tc := range []struct {
		p    Provider
		want bool
	}{{GrokProvider(), true}, {GxProvider(), true}, {CursorProvider(), false}, {NativeProvider(), false}} {
		if got := tc.p.imagesDespiteCapability; got != tc.want {
			t.Errorf("%s: imagesDespiteCapability = %v, want %v", tc.p.Name(), got, tc.want)
		}
	}
}

// TestReplayDropsImageChunks: an agent's image chunk — on replay or live —
// carries no text, so the transcript shows nothing for it; block 1's chip is
// what the user sees (plan 033 §3.4).
func TestReplayDropsImageChunks(t *testing.T) {
	if got := messageText(json.RawMessage(`{"type":"image","data":"AAAA","mimeType":"image/png","uri":"file:///x.png"}`)); got != "" {
		t.Fatalf("messageText of an image chunk = %q", got)
	}
	if got := messageText(json.RawMessage(`{"type":"text","text":"look at [Image #1]"}`)); got != "look at [Image #1]" {
		t.Fatalf("messageText of a text chunk = %q", got)
	}
}

// --- the live session over the fake agent --------------------------------

// TestPromptDumpReceivesTheImageBlocks is the whole path with the fake in the
// middle (prompt-dump, cursor: image:true): the envelope never reaches the
// agent, block 1 is the visible text, the image follows as an image block
// whose data decodes to the stored file's bytes, and the downscale note the
// envelope's untrusted original size allows comes last.
func TestPromptDumpReceivesTheImageBlocks(t *testing.T) {
	imageHome(t)
	ref, data := savedImage(t, 1, 40, 30)
	ref.OW, ref.OH = 3024, 1964
	s := startScript(t, "prompt-dump", false)
	log := collect(t, s)
	res, err := s.Prompt(t.Context(), AttachmentBlock([]AttachmentRef{ref})+"look at [Image #1]")
	if err != nil || res.StopReason != acp.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v", res, err)
	}
	want := "prompt 1: 3 blocks\n" +
		`1 text "look at [Image #1]"` + "\n" +
		fmt.Sprintf("2 image image/png data=%d bytes=%d uri=file://%s\n", base64.StdEncoding.EncodedLen(len(data)), len(data), ref.Path) +
		`3 text "[Image #1 was downscaled from 3024×1964 to 40×30]"` + "\n"
	log.waitTexts(t, want)
}

// TestTheDialectFlagDecidesImagesAgainstImageFalse: grok-prompt-dump
// advertises image:false, as grok 1.0.30 and gx do. grok and gx send the image
// block regardless (imagesDespiteCapability, P5); a provider without the flag
// sends the image as [Image #1: <path>] text in the block's place.
func TestTheDialectFlagDecidesImagesAgainstImageFalse(t *testing.T) {
	noFlag := GrokProvider()
	noFlag.imagesDespiteCapability = false
	for _, tc := range []struct {
		name  string
		p     Provider
		image bool
	}{{"grok", GrokProvider(), true}, {"gx", GxProvider(), true}, {"image:false without the flag", noFlag, false}} {
		t.Run(tc.name, func(t *testing.T) {
			imageHome(t)
			t.Setenv("XAI_API_KEY", "")
			t.Setenv("GROK_CODE_XAI_API_KEY", "")
			dump := promptDumpFile(t)
			ref, data := savedImage(t, 1, 24, 24)
			p := tc.p
			s := startScriptOpts(t, "grok-prompt-dump", Options{Force: true, Provider: &p})
			if _, err := s.Prompt(t.Context(), AttachmentBlock([]AttachmentRef{ref})+"what is [Image #1]?"); err != nil {
				t.Fatal(err)
			}
			prompts := dumpedPrompts(t, dump)
			if len(prompts) != 1 {
				t.Fatalf("the agent read %d prompts", len(prompts))
			}
			got := prompts[0]
			if got[0].Text != "what is [Image #1]?" {
				t.Fatalf("block 1 %q", got[0].Text)
			}
			if tc.image {
				if blockTypes(got) != "text,image" || got[1].Bytes != len(data) || got[1].MimeType != "image/png" {
					t.Fatalf("blocks %+v, want block 1 and the image", got)
				}
				return
			}
			if blockTypes(got) != "text,text" || got[1].Text != "[Image #1: "+ref.Path+"]" {
				t.Fatalf("blocks %+v, want block 1 and the path text", got)
			}
		})
	}
}

// eventsOf is every event of one type in evs.
func eventsOf(evs []Event, typ EventType) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// TestRejectImageIsResentOnceWithPathText (plan 033 §3.4, A5): reject-image
// refuses the image block with -32602 before saying anything; craze sends the
// prompt once more, block 1 and the expansion unchanged and the image as
// [Image #1: <path>] text, and that is the turn's answer. The expansion is
// announced once, there is no error event, and the journal records the resend.
func TestRejectImageIsResentOnceWithPathText(t *testing.T) {
	imageHome(t)
	dump := promptDumpFile(t)
	ref, _ := savedImage(t, 1, 40, 30)
	dir := probeFixtureDir(t)
	s, w := journaledScript(t, "reject-image", Options{PluginDirs: []string{dir}})
	log := collect(t, s)
	draft := "/probe-plugin:probe-echo banana [Image #1]"
	res, err := s.Prompt(t.Context(), AttachmentBlock([]AttachmentRef{ref})+draft)
	if err != nil || res.StopReason != acp.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want the resend's end_turn", res, err)
	}
	prompts := dumpedPrompts(t, dump)
	if len(prompts) != 2 {
		t.Fatalf("the agent read %d prompts, want the refused one and one resend", len(prompts))
	}
	first, again := prompts[0], prompts[1]
	if blockTypes(first) != "text,text,image" || blockTypes(again) != "text,text,text" {
		t.Fatalf("prompts %s then %s", blockTypes(first), blockTypes(again))
	}
	if first[0].Text != draft || again[0].Text != draft || first[1].Text != again[1].Text {
		t.Fatalf("block 1 or the expansion changed: %+v then %+v", first[:2], again[:2])
	}
	if again[2].Text != "[Image #1: "+ref.Path+"]" {
		t.Fatalf("the resend's image is %q", again[2].Text)
	}
	log.waitType(t, EventDone)
	evs := log.snapshot()
	if reply := texts(evs); !strings.HasPrefix(reply, "prompt 2: 3 blocks\n") {
		t.Fatalf("the turn's answer is %q, want the resend's", reply)
	}
	if n := len(commandEvents(evs)); n != 1 {
		t.Fatalf("%d expansion events, want 1", n)
	}
	if errs := eventsOf(evs, EventError); len(errs) != 0 {
		t.Fatalf("error events %+v", errs)
	}
	_, lines := journaledAttemptsOf(t, s, w)
	resends := diags(lines, diagImageResend)
	if len(resends) != 1 || resends[0]["images"] != float64(1) || !strings.Contains(fmt.Sprint(resends[0]["refusal"]), "-32602") {
		t.Fatalf("image_resend diags %v", resends)
	}
}

// TestARejectionAfterActivityIsNotResent: the agent said something for the
// turn before it refused (SAY-FIRST), so the prompt is one it began on and is
// not sent again: the -32602 surfaces as the turn's error, once.
func TestARejectionAfterActivityIsNotResent(t *testing.T) {
	imageHome(t)
	dump := promptDumpFile(t)
	ref, _ := savedImage(t, 1, 40, 30)
	s := startScript(t, "reject-image", false)
	log := collect(t, s)
	_, err := s.Prompt(t.Context(), AttachmentBlock([]AttachmentRef{ref})+"SAY-FIRST [Image #1]")
	var rpc *acp.RPCError
	if !errors.As(err, &rpc) || rpc.Code != acp.CodeInvalidParams {
		t.Fatalf("Prompt error %v, want the -32602", err)
	}
	if n := len(dumpedPrompts(t, dump)); n != 1 {
		t.Fatalf("the agent read %d prompts, want 1: no resend after activity", n)
	}
	log.waitType(t, EventError)
	evs := log.snapshot()
	if n := len(eventsOf(evs, EventError)); n != 1 {
		t.Fatalf("%d error events", n)
	}
	if !strings.Contains(texts(evs), "looking at it") {
		t.Fatalf("the agent's own words are missing: %q", texts(evs))
	}
}

// TestACancelBetweenTheAttemptsIsNotResent: a Cancel that lands after the
// refusal and before the resend's decision finds the prompt cancelling, and
// the refusal stands: nothing more is sent. The Cancel runs from
// testBeforeResend, the one point between the two attempts.
func TestACancelBetweenTheAttemptsIsNotResent(t *testing.T) {
	imageHome(t)
	dump := promptDumpFile(t)
	ref, _ := savedImage(t, 1, 40, 30)
	s := startScript(t, "reject-image", false)
	cancelled := make(chan error, 1)
	testBeforeResend = func(s *session) {
		go func() {
			_, err := s.Cancel(context.Background())
			cancelled <- err
		}()
		waitFor(t, "the cancel's mark", func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.cancelling
		})
	}
	t.Cleanup(func() { testBeforeResend = nil })
	_, err := s.Prompt(t.Context(), AttachmentBlock([]AttachmentRef{ref})+"look at [Image #1]")
	var rpc *acp.RPCError
	if !errors.As(err, &rpc) || rpc.Code != acp.CodeInvalidParams {
		t.Fatalf("Prompt error %v, want the -32602", err)
	}
	if err := await(t, cancelled, "the cancel"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if n := len(dumpedPrompts(t, dump)); n != 1 {
		t.Fatalf("the agent read %d prompts, want 1: a cancelled prompt is not resent", n)
	}
}

// TestHostReadRefusalsReachTheAgentAsPathText (A4 on the wire): a forged
// path outside the attachments directory is path text with its reason in
// block 1; an image whose chip is not in the text is dropped — no image, no
// path text (P28); neither is sent as an image; and both are journaled.
func TestHostReadRefusalsReachTheAgentAsPathText(t *testing.T) {
	imageHome(t)
	dump := promptDumpFile(t)
	outside := plantFile(t, t.TempDir(), "0123456789abcdef.png", testPNG(t, 24, 24))
	unseen, _ := savedImage(t, 2, 40, 30)
	s, w := journaledScript(t, "prompt-dump", Options{})
	refs := []AttachmentRef{{N: 1, Path: outside, MIME: "image/png"}, unseen}
	if _, err := s.Prompt(t.Context(), AttachmentBlock(refs)+"see [Image #1]"); err != nil {
		t.Fatal(err)
	}
	prompts := dumpedPrompts(t, dump)
	if len(prompts) != 1 || blockTypes(prompts[0]) != "text" {
		t.Fatalf("prompts %+v, want block 1 alone", prompts)
	}
	block1 := prompts[0][0].Text
	if !strings.HasPrefix(block1, "[Image #1: "+outside+" (not attached: ") || !strings.HasSuffix(block1, ")]\nsee [Image #1]") {
		t.Fatalf("block 1 %q, want the forged path's path text with a reason before the words", block1)
	}
	if strings.Contains(block1, unseen.Path) || strings.Contains(block1, "craze_attachments") {
		t.Fatalf("block 1 %q carries the dropped image or the envelope", block1)
	}
	_, lines := journaledAttemptsOf(t, s, w)
	notes := diags(lines, diagAttachments)
	if len(notes) != 1 || notes[0]["via"] != "prompt" {
		t.Fatalf("attachments diags %v", notes)
	}
	problems := fmt.Sprint(notes[0]["problems"])
	if !strings.Contains(problems, "image #1") || !strings.Contains(problems, "not attached") ||
		!strings.Contains(problems, "image #2") || !strings.Contains(problems, "dropped") {
		t.Fatalf("the journaled problems %s miss a refusal", problems)
	}
}

// --- interjections ---------------------------------------------------------

// TestNativeInterjectSendsPathText (P7 on native): an interjection carrying an
// envelope reaches the model as [Image #1: <path>] text, and its echo says the
// same.
func TestNativeInterjectSendsPathText(t *testing.T) {
	f := newNativeFixture(t)
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := f.started(Options{Workspace: ws})
	h := newHeld(t)
	m := f.models["test/a"]
	m.push(h.step(nativeCallParts("c1", "read", nativeArgs(t, map[string]any{"filePath": "a.txt"})),
		finishParts(fantasy.FinishReasonToolCalls)), answer("done"))
	out := startPrompt(s, "read a.txt")
	await(t, h.reached, "the held tool step")
	ref := AttachmentRef{N: 1, Path: "/h/attachments/0123456789abcdef.png", MIME: "image/png"}
	if err := s.Interject(context.Background(), AttachmentBlock([]AttachmentRef{ref})+"also [Image #1]"); err != nil {
		t.Fatalf("Interject: %v", err)
	}
	close(h.release)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatal(got.err)
	}
	want := "[Image #1: /h/attachments/0123456789abcdef.png]\nalso [Image #1]"
	calls := m.requests()
	if len(calls) != 2 {
		t.Fatalf("%d requests", len(calls))
	}
	users := userTexts(calls[1])
	if got := users[len(users)-1]; got != want {
		t.Fatalf("the model read the interjection as %q, want %q", got, want)
	}
	if texts, _ := interjected(drained(s)); len(texts) != 1 || texts[0] != want {
		t.Fatalf("interjection echoes %q", texts)
	}
}
