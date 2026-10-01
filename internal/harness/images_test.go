package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool/attach"
)

// Images on native (plan 033 §3.5; §7 A7, A8, A9b). A prompt's images are
// file parts after its text, kept in the transcript whatever the model; a
// model that accepts them is sent them, one that does not a placeholder —
// for a tool's image result too — and the summarizer never any. Most of these
// run the real stack (Fantasy, openaicompat, the llm wrapper) against the
// local Chat Completions server of wire_test.go, so what is checked is what a
// provider would read.

// shotPath is the attachment every test here sends: where the attachments
// directory would have it.
const shotPath = "/home/u/.craze/attachments/0123456789abcdef.png"

// glm is the alias of the non-vision model the tests switch to, the
// shipped catalog's GLM (H8's exit: "GLM gets the placeholder").
const glm = "glm-5.3"

// glmPlaceholder is what glm is sent for shotPath's image.
const glmPlaceholder = "[Image omitted: GLM 5.3 (Z.AI) does not accept images. File: " + shotPath + "]"

// testPNG is a w×h PNG of one grey: a real image, quick to make, small.
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// shot is a pasted screenshot: shotPath's image, w×h.
func shot(t *testing.T, w, h int) Image {
	t.Helper()
	return Image{Path: shotPath, MediaType: "image/png", Data: testPNG(t, w, h)}
}

// withVision marks alias, in f's table, as a model that accepts images: what
// a catalog's vision = true says (the shipped catalog sets none before C7).
func withVision(f *fixture, alias string) {
	m := f.table.Models[alias]
	m.Vision = true
	f.table.Models[alias] = m
}

// addGLM puts glm in f's table as the shipped catalog has it — its name, no
// vision — on the test provider, so its requests reach the test's server.
func addGLM(f *fixture) {
	f.table.Models[glm] = modeltable.Model{Provider: "test", WireModel: "glm-5.3", Name: "GLM 5.3 (Z.AI)"}
}

// sentPart is one part of a Chat Completions message's content array.
type sentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// sentMessage is one message of a request as the provider read it: its
// role, and its content — a string (Text) or an array of parts (Parts).
type sentMessage struct {
	Role  string
	Text  string
	Parts []sentPart
}

func decodeSent(t *testing.T, raw json.RawMessage) sentMessage {
	t.Helper()
	var m struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("a request message is not JSON: %v\n%s", err, raw)
	}
	out := sentMessage{Role: m.Role}
	var err error
	switch {
	case len(m.Content) > 0 && m.Content[0] == '"':
		err = json.Unmarshal(m.Content, &out.Text)
	case len(m.Content) > 0 && m.Content[0] == '[':
		err = json.Unmarshal(m.Content, &out.Parts)
	}
	if err != nil {
		t.Fatalf("a request message's content: %v\n%s", err, raw)
	}
	return out
}

// sentMessages is every message of the request body, decoded.
func sentMessages(t *testing.T, body []byte) []sentMessage {
	t.Helper()
	var out []sentMessage
	for _, raw := range messages(t, body) {
		out = append(out, decodeSent(t, raw))
	}
	return out
}

// imageURLs is every image_url part's URL in the request body, in order.
func imageURLs(t *testing.T, body []byte) []string {
	t.Helper()
	var out []string
	for _, m := range sentMessages(t, body) {
		for _, p := range m.Parts {
			if p.Type == "image_url" {
				out = append(out, p.ImageURL.URL)
			}
		}
	}
	return out
}

// dataURI is data as an image_url carries it.
func dataURI(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// storedPrompt is the transcript's user entry whose text is text: the
// message the turn opened with, as the file holds it.
func storedPrompt(t *testing.T, tr *store.Transcript, text string) fantasy.Message {
	t.Helper()
	for _, e := range tr.Entries {
		if e.Type == store.TypeMessage && e.Message.Role == fantasy.MessageRoleUser && len(e.Message.Content) > 0 {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](e.Message.Content[0]); ok && tp.Text == text {
				return e.Message
			}
		}
	}
	t.Fatalf("no stored prompt %q in %q", text, entries(tr))
	return fantasy.Message{}
}

// keptImages fails the test unless msg is text followed by exactly images'
// file parts, path, type and bytes: what the transcript keeps, whatever the
// model was sent (P4).
func keptImages(t *testing.T, msg fantasy.Message, text string, images ...Image) {
	t.Helper()
	want := fantasy.NewUserMessage(text, fileParts(images)...)
	if len(msg.Content) != len(want.Content) {
		t.Fatalf("the stored prompt has %d parts, want %d: its text, then one file part per image", len(msg.Content), len(want.Content))
	}
	for i, im := range images {
		got, ok := fantasy.AsMessagePart[fantasy.FilePart](msg.Content[i+1])
		if !ok || got.Filename != im.Path || got.MediaType != im.MediaType || !bytes.Equal(got.Data, im.Data) {
			t.Fatalf("the stored prompt's part %d = %T %q %q (%d bytes); want image %d's file part, kept exactly", i+1, msg.Content[i+1], got.Filename, got.MediaType, len(got.Data), i+1)
		}
	}
}

// TestAVisionModelIsSentTheImage (A7, the vision half): the prompt's message
// is its text, then the image as an image_url data URI of its bytes — the
// one part after the text — and the transcript keeps the file part.
func TestAVisionModelIsSentTheImage(t *testing.T) {
	w := newWire(t, sseReply(textChunk("A grey square."), finishChunk("stop", true)))
	f, opts := wireFixture(t, w)
	withVision(f, "test/a")
	s := f.open(opts)
	img := shot(t, 40, 30)
	if _, err := s.RunWith(context.Background(), "what is [Image #1]?", []Image{img}, nil); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	bodies := w.requests()
	if len(bodies) != 1 {
		t.Fatalf("the server saw %d requests, want 1", len(bodies))
	}
	msgs := sentMessages(t, bodies[0])
	last := msgs[len(msgs)-1]
	want := []sentPart{{Type: "text", Text: "what is [Image #1]?"}, {Type: "image_url"}}
	want[1].ImageURL.URL = dataURI("image/png", img.Data)
	if last.Role != "user" || !reflect.DeepEqual(last.Parts, want) {
		t.Fatalf("the prompt was sent as %+v; want its text, then the image as a data URI", last)
	}
	keptImages(t, storedPrompt(t, transcript(t, s), "what is [Image #1]?"), "what is [Image #1]?", img)
}

// TestANonVisionModelIsSentThePlaceholder (A7, H8's exit): glm-5.3 accepts no
// images, so its request has no image_url and none of the image's bytes —
// its prompt is the text, then a placeholder naming the model and the file —
// while the transcript still keeps the image, for a later model that can see
// it.
func TestANonVisionModelIsSentThePlaceholder(t *testing.T) {
	w := newWire(t, sseReply(textChunk("I cannot see it."), finishChunk("stop", true)))
	f, opts := wireFixture(t, w)
	addGLM(f)
	opts.Model = glm
	s := f.open(opts)
	img := shot(t, 40, 30)
	if _, err := s.RunWith(context.Background(), "what is [Image #1]?", []Image{img}, nil); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	body := w.requests()[0]
	if bytes.Contains(body, []byte("image_url")) || bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString(img.Data))) {
		t.Fatalf("glm-5.3's request carried the image:\n%s", body)
	}
	msgs := sentMessages(t, body)
	want := []sentPart{{Type: "text", Text: "what is [Image #1]?"}, {Type: "text", Text: glmPlaceholder}}
	if last := msgs[len(msgs)-1]; last.Role != "user" || !reflect.DeepEqual(last.Parts, want) {
		t.Fatalf("the prompt was sent as %+v; want its text, then the placeholder", last)
	}
	keptImages(t, storedPrompt(t, transcript(t, s), "what is [Image #1]?"), "what is [Image #1]?", img)
}

// TestAnImagePromptIsTheSameBytesNextTurn is the cache property with an image
// (CR R5), on a model that accepts images and on one that does not: the next
// turn's request begins with the whole of this turn's, byte for byte — the
// prompt as the turn sent it (Messages, Prompt "") is the prompt as the
// transcript replays it, the placeholder included — so a provider's prefix
// cache holds across the two.
func TestAnImagePromptIsTheSameBytesNextTurn(t *testing.T) {
	for _, vision := range []bool{true, false} {
		name := "vision"
		if !vision {
			name = "no vision"
		}
		t.Run(name, func(t *testing.T) {
			w := newWire(t,
				sseReply(textChunk("A grey square."), finishChunk("stop", true)),
				sseReply(textChunk("Nothing else."), finishChunk("stop", true)),
			)
			f, opts := wireFixture(t, w)
			if vision {
				withVision(f, "test/a")
			}
			s := f.open(opts)
			if _, err := s.RunWith(context.Background(), "what is [Image #1]?", []Image{shot(t, 40, 30)}, nil); err != nil {
				t.Fatalf("RunWith: %v", err)
			}
			run(t, s, "anything else?")
			bodies := w.requests()
			one, two := messages(t, bodies[0]), messages(t, bodies[1])
			if k := prefixBreak(one, two); k != -1 {
				t.Fatalf("turn 2's request breaks turn 1's prefix at message %d:\n turn 1 %s\n turn 2 %s", k, one[k], two[min(k, len(two)-1)])
			}
			if len(one) != 2 || !bytes.Equal(one[1], two[1]) {
				t.Fatalf("turn 1 sent %d messages; want the system prompt and the prompt, which turn 2 replays as the same bytes", len(one))
			}
			if got := len(imageURLs(t, bodies[1])); got != map[bool]int{true: 1, false: 0}[vision] {
				t.Fatalf("turn 2 sent %d images; want the history's image only to a model that accepts it", got)
			}
		})
	}
}

// seedImages writes one finished turn on s's current model holding both
// shapes of image: a prompt with an attached one, a read whose result is an
// image (a fantasy.ToolResultOutputContentMedia, as native Read returns one,
// built here directly), and the answer.
func seedImages(t *testing.T, s *Session, attached, read []byte) {
	t.Helper()
	id := s.cur.id()
	prompt := fantasy.NewUserMessage("what is [Image #1]? read diagram.png too",
		fantasy.FilePart{Filename: shotPath, MediaType: "image/png", Data: attached})
	if err := s.store.AppendUser(store.MessageEntry{Message: prompt, Model: id, Turn: 1}); err != nil {
		t.Fatal(err)
	}
	call := fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "read", Input: `{"filePath":"diagram.png"}`},
	}}
	result := fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
		fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentMedia{
			Data: base64.StdEncoding.EncodeToString(read), MediaType: "image/png", Text: "Image read: diagram.png",
		}},
	}}
	if _, err := s.store.AppendStep(nil, store.MessageEntry{Message: call, Model: id, StopReason: "tool_use"},
		&store.MessageEntry{Message: result, Model: id}); err != nil {
		t.Fatal(err)
	}
	answer := fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "A chart and a diagram."}}}
	if err := s.store.AppendAssistant(store.MessageEntry{Message: answer, Model: id, StopReason: "end_turn"}); err != nil {
		t.Fatal(err)
	}
}

// toolText is the content of the request's tool message answering id.
func toolText(t *testing.T, body []byte, id string) string {
	t.Helper()
	for _, raw := range messages(t, body) {
		var m struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		}
		if json.Unmarshal(raw, &m) == nil && m.Role == "tool" && m.ToolCallID == id {
			return m.Content
		}
	}
	t.Fatalf("no tool message for %s in %s", id, body)
	return ""
}

// TestASwitchToANonVisionModelStripsTheHistory (A9b): a history holding a
// pasted image and a read image goes to a model that accepts images with
// both — the read one as openaicompat's own user message after the tool
// result — and, after /model to glm-5.3, with neither: the pasted image is
// its placeholder, naming the file, and the read result is its text and a
// placeholder, still a tool message answering the same call.
func TestASwitchToANonVisionModelStripsTheHistory(t *testing.T) {
	w := newWire(t,
		sseReply(textChunk("Both are grey."), finishChunk("stop", true)),
		sseReply(textChunk("I cannot see them."), finishChunk("stop", true)),
	)
	f, opts := wireFixture(t, w)
	withVision(f, "test/a")
	addGLM(f)
	s := f.open(opts)
	attached, read := testPNG(t, 40, 30), testPNG(t, 20, 20)
	seedImages(t, s, attached, read)
	run(t, s, "describe both")
	if err := s.SetModel(glm); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	run(t, s, "and again")

	bodies := w.requests()
	if got, want := imageURLs(t, bodies[0]), []string{dataURI("image/png", attached), dataURI("image/png", read)}; !slices.Equal(got, want) {
		t.Fatalf("the vision model was sent images %d; want the pasted one, then the read one", len(got))
	}
	if bytes.Contains(bodies[1], []byte("image_url")) {
		t.Fatalf("glm-5.3 was sent an image:\n%s", bodies[1])
	}
	msgs := sentMessages(t, bodies[1])
	want := []sentPart{{Type: "text", Text: "what is [Image #1]? read diagram.png too"}, {Type: "text", Text: glmPlaceholder}}
	if !reflect.DeepEqual(msgs[1].Parts, want) {
		t.Fatalf("glm-5.3 was sent the pasted prompt as %+v; want its text, then the placeholder", msgs[1])
	}
	if got := toolText(t, bodies[1], "c1"); got != "Image read: diagram.png\n[Image omitted: GLM 5.3 (Z.AI) does not accept images]" {
		t.Fatalf("glm-5.3 was sent the read's result as %q; want its text, then the placeholder", got)
	}
}

// TestTheSummarizerIsNeverSentAnImage (A9b): a /compact over a history holding
// both shapes of image sends the summarizer no image, on a model that accepts
// them too. On one that does not, the summarizer's history is the turns' own,
// placeholders and all (so it is the prefix their cache holds); on one that
// does, the images go as placeholders saying the summarizer is not sent them —
// not that the model cannot see them, which it can.
func TestTheSummarizerIsNeverSentAnImage(t *testing.T) {
	for _, vision := range []bool{true, false} {
		name, why := "vision", summarizerOmits
		if !vision {
			name, why = "no vision", "Model A does not accept images"
		}
		t.Run(name, func(t *testing.T) {
			w := newWire(t, sseReply(textChunk(segmentSummary("two images.")), finishChunk("stop", true)))
			f, opts := wireFixture(t, w)
			if vision {
				withVision(f, "test/a")
			}
			s := f.open(opts)
			attached, read := testPNG(t, 40, 30), testPNG(t, 20, 20)
			seedImages(t, s, attached, read)
			if _, err := s.Compact(context.Background(), "", "/compact", nil); err != nil {
				t.Fatalf("Compact: %v", err)
			}
			bodies := w.requests()
			if len(bodies) != 1 {
				t.Fatalf("the server saw %d requests, want the summarizer's one", len(bodies))
			}
			body := bodies[0]
			if _, tools := fields(t, body)["tools"]; !tools {
				t.Fatal("the summarizer's request offered no tools: want the aligned form, which replays the history")
			}
			for _, data := range [][]byte{attached, read} {
				if bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString(data))) {
					t.Fatal("the summarizer was sent an image's bytes")
				}
			}
			if bytes.Contains(body, []byte("image_url")) {
				t.Fatalf("the summarizer was sent an image_url:\n%s", body)
			}
			msgs := sentMessages(t, body)
			want := []sentPart{{Type: "text", Text: "what is [Image #1]? read diagram.png too"},
				{Type: "text", Text: "[Image omitted: " + why + ". File: " + shotPath + "]"}}
			if !reflect.DeepEqual(msgs[1].Parts, want) {
				t.Fatalf("the summarizer was sent the pasted prompt as %+v; want its text, then %q", msgs[1], want[1].Text)
			}
			if got := toolText(t, body, "c1"); got != "Image read: diagram.png\n[Image omitted: "+why+"]" {
				t.Fatalf("the summarizer was sent the read's result as %q", got)
			}
		})
	}
}

// TestImagesSurviveTheTranscriptAndAResume: a resumed session's first request
// replays the prompt with its image — read back from the file, bytes and
// all, with no attachments directory involved (P4) — as the same bytes the
// prompt's own turn sent.
func TestImagesSurviveTheTranscriptAndAResume(t *testing.T) {
	w := newWire(t,
		sseReply(textChunk("A grey square."), finishChunk("stop", true)),
		sseReply(textChunk("Still grey."), finishChunk("stop", true)),
	)
	f, opts := wireFixture(t, w)
	withVision(f, "test/a")
	img := shot(t, 40, 30)
	id := imageTurnThenClose(t, opts, []Image{img})

	s := resumed(t, resumeOptions(opts, id))
	keptImages(t, storedPrompt(t, transcript(t, s), "what is [Image #1]?"), "what is [Image #1]?", img)
	run(t, s, "and now?")
	bodies := w.requests()
	if one, two := messages(t, bodies[0]), messages(t, bodies[1]); !bytes.Equal(one[1], two[1]) {
		t.Fatalf("the resumed request replays the prompt as\n%s\nwant the bytes its turn sent\n%s", two[1], one[1])
	}
}

// imageTurnThenClose opens a session with opts, runs one turn prompted
// "what is [Image #1]?" with images, closes it and returns its id.
func imageTurnThenClose(t *testing.T, opts Options, images []Image) string {
	t.Helper()
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.RunWith(context.Background(), "what is [Image #1]?", images, nil); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s.ID()
}

// TestAFullMessageStoresResumesAndSends: a message at the per-message
// aggregate (P29: attach.MaxPerMessage images, attach.MaxMessageBytes raw
// between them) is written to the transcript whole, read back on a resume,
// and sent on both sides of it; and the context it makes is estimated by the
// image rule, not by its base64 (some 5M tokens at bytes/4), so on a model
// with a 200k window the resumed turn sets off no compaction — the server
// sees no summarizer — and no ErrContextTooLarge. The turns report no usage,
// so the estimate is the whole request's.
//
// The images are incompressible noise under an image type, which the harness
// takes as it is: nothing here validates or decodes pixels (the host did
// that), and the estimate reads a header, which noise has not got, so each
// counts as the cap. Under the race detector, which slows the 20 MB of JSON
// a side some fifteen times over and has nothing concurrent here to check,
// the images are 64 KiB each.
func TestAFullMessageStoresResumesAndSends(t *testing.T) {
	w := newWire(t,
		sseReply(textChunk("Ten images."), finishChunk("stop", false)),
		sseReply(textChunk("Still ten."), finishChunk("stop", false)),
	)
	f, opts := wireFixture(t, w)
	withVision(f, "test/a")
	windowed(f, "test/a", 200_000, 0)
	size := attach.MaxMessageBytes / attach.MaxPerMessage
	if raceEnabled {
		size = 64 << 10
	}
	images := make([]Image, attach.MaxPerMessage)
	for i := range images {
		data := make([]byte, size)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		images[i] = Image{Path: shotPath, MediaType: "image/png", Data: data}
	}
	id := imageTurnThenClose(t, opts, images)

	s := resumed(t, resumeOptions(opts, id))
	keptImages(t, storedPrompt(t, s.store.Transcript(), "what is [Image #1]?"), "what is [Image #1]?", images...)
	bound := s.estimateContext(nil) + int64(attach.MaxPerMessage)*maxImageTokens + 1_000
	if tokens, _ := s.contextTokens(s.cur); tokens > bound {
		t.Fatalf("the resumed context is estimated at %d tokens; want at most %d: the system prompt and tools, each image at most %d, and a little text", tokens, bound, maxImageTokens)
	}
	run(t, s, "and now?")
	bodies := w.requests()
	if len(bodies) != 2 {
		t.Fatalf("the server saw %d requests, want the two turns' and no summarizer's", len(bodies))
	}
	// Each body is parsed once: the prompt is the message after the system
	// prompt on both sides, and its images are counted in its bytes.
	one, two := messages(t, bodies[0]), messages(t, bodies[1])
	if !bytes.Equal(one[1], two[1]) {
		t.Fatal("the resumed request does not replay the full message as the bytes its turn sent")
	}
	if got := bytes.Count(one[1], []byte(`"url":"data:image/png;base64,`)); got != attach.MaxPerMessage {
		t.Fatalf("the full message was sent with %d images, want %d", got, attach.MaxPerMessage)
	}
}

// TestAnOverflowResendCarriesTheImages: a turn's first request with an image
// overflows; the replacement after the overflow's compaction sends the prompt
// again — its text and its file part, the message the turn first sent — and
// the transcript holds it once.
func TestAnOverflowResendCarriesTheImages(t *testing.T) {
	_, s, a := overflowFixture(t, func(f *fixture) Options {
		withVision(f, "test/a")
		return f.options()
	})
	a.push(answerWith("hi"))
	run(t, s, "hello")
	a.push(overflowed(), summaryOf("a greeting."), answerWith("a grey square"))
	img := shot(t, 40, 30)
	res, err := s.RunWith(context.Background(), "what is [Image #1]?", []Image{img}, nil)
	if err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("the turn = %+v, %v; want end_turn", res, err)
	}
	reqs := a.requests()
	if len(reqs) != 4 || !isTextForm(reqs[2]) || !startsFromSummary(reqs[3]) {
		t.Fatalf("%d requests; want turn one, the one that overflowed, the text-form summarizer, then the replacement", len(reqs))
	}
	want := fantasy.NewUserMessage("what is [Image #1]?", fileParts([]Image{img})...)
	for _, i := range []int{1, 3} {
		if last := reqs[i].Prompt[len(reqs[i].Prompt)-1]; !reflect.DeepEqual(last, want) {
			t.Fatalf("request %d ends %q; want the prompt with its image", i+1, promptOf(reqs[i]))
		}
	}
	tr := transcript(t, s)
	keptImages(t, storedPrompt(t, tr, "what is [Image #1]?"), "what is [Image #1]?", img)
	if n := strings.Count(strings.Join(entries(tr), "\n"), "what is [Image #1]?"); n != 1 {
		t.Fatalf("the transcript holds the prompt %d times, want once:\n%s", n, strings.Join(entries(tr), "\n"))
	}
}

// TestARestartAfterAnImagePromptTakesItFromTheStore: a turn whose prompt
// carries an image reaches the threshold at its first step, compacts and
// restarts with Prompt "" — which Fantasy refuses with files — so the image
// is not sent again as a file: the prompt is in the store by then, and the
// summarizer, on a model that accepts images, was sent its placeholder.
func TestARestartAfterAnImagePromptTakesItFromTheStore(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	windowed(f, "test/a", testWindow, 0)
	withVision(f, "test/a")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(toolStep(1, over), summaryOf("looked at a chart."), answerWith("done"))
	res, err := s.RunWith(context.Background(), "loop over [Image #1]", []Image{shot(t, 40, 30)}, nil)
	if err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("the turn = %+v, %v; want end_turn after the restart", res, err)
	}
	reqs := a.requests()
	if len(reqs) != 3 || !isSummarizer(reqs[1]) || !startsFromSummary(reqs[2]) {
		t.Fatalf("%d requests; want the first step, the summarizer, then the restart from the summary", len(reqs))
	}
	if !hasFilePart(reqs[0]) {
		t.Fatal("the turn's first request did not carry the image")
	}
	for i, r := range reqs[1:] {
		if hasFilePart(r) {
			t.Fatalf("request %d carried a file part: the summarizer and the restart are sent none", i+2)
		}
	}
	if got := promptOf(reqs[1])[1]; got != "user: loop over [Image #1] [Image omitted: "+summarizerOmits+". File: "+shotPath+"]" {
		t.Fatalf("the summarizer was sent the prompt as %q", got)
	}
}

// hasFilePart reports whether any message of c's prompt holds a file part.
func hasFilePart(c fantasy.Call) bool {
	for _, m := range c.Prompt {
		for _, p := range m.Content {
			if _, ok := fantasy.AsMessagePart[fantasy.FilePart](p); ok {
				return true
			}
		}
	}
	return false
}

// TestImageTokens (P9, A8): an image is estimated from its header's size,
// ⌈w/28⌉·⌈h/28⌉ capped at 1600, and at the cap when it has no size to read —
// for an attached image and for a tool's image result alike — and its bytes
// count for nothing more: the message weighs what it weighs without them,
// plus the image's estimate.
func TestImageTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want int64
	}{
		{"28×28 is one tile", testPNG(t, 28, 28), 1},
		{"29×28 is two", testPNG(t, 29, 28), 2},
		{"100×60", testPNG(t, 100, 60), 4 * 3},
		{"2000×1299 is capped", testPNG(t, 2000, 1299), min(72*47, maxImageTokens)},
		{"no header", []byte("not an image at all, and not short either"), maxImageTokens},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageTokens(bytes.NewReader(tc.data)); got != tc.want || got > 1600 {
				t.Fatalf("the image alone is estimated at %d tokens, want %d, and at most 1600 (A8)", got, tc.want)
			}
			// The message without its image's bytes, weighed as JSON: what
			// the rest of it costs.
			rest := func(m fantasy.Message) int64 {
				t.Helper()
				b, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				return tokensOf(len(b))
			}
			file := func(data []byte) fantasy.Message {
				return fantasy.NewUserMessage("what is [Image #1]?", fantasy.FilePart{Filename: shotPath, MediaType: "image/png", Data: data})
			}
			if got, want := messageTokens(file(tc.data)), rest(file(nil))+tc.want; got != want {
				t.Fatalf("a prompt with an attached image weighs %d tokens, want %d: the rest, and the image's %d", got, want, tc.want)
			}
			media := func(data string) fantasy.Message {
				return fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{
					ToolCallID: "c1", Output: fantasy.ToolResultOutputContentMedia{Data: data, MediaType: "image/png", Text: "Image read"},
				}}}
			}
			if got, want := messageTokens(media(base64.StdEncoding.EncodeToString(tc.data))), rest(media(""))+tc.want; got != want {
				t.Fatalf("a tool's image result weighs %d tokens, want %d: the rest, and the image's %d", got, want, tc.want)
			}
		})
	}
	// Anything else that is a file still counts by its bytes: the rule is an
	// image's.
	text := fantasy.FilePart{Filename: "notes.txt", MediaType: "text/plain", Data: bytes.Repeat([]byte("a"), 4000)}
	if got := messageTokens(fantasy.NewUserMessage("x", text)); got < 4000*4/3/bytesPerToken {
		t.Fatalf("a text file part counts %d tokens; want its base64 by bytes/4", got)
	}
}

// TestStripImages is the strip on its own (P8): to a model that accepts
// images it changes nothing, not even the slice; to one that does not, an
// image file part becomes a text placeholder naming its file, a tool's image
// result becomes a text result of its text and a placeholder, and everything
// else — a non-image file part, a text result, a message with no image — is
// carried over as it is, the input never written to.
func TestStripImages(t *testing.T) {
	media := func(text string) fantasy.Message {
		return fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{
			ToolCallID: "c1", Output: fantasy.ToolResultOutputContentMedia{Data: "aGk=", MediaType: "image/png", Text: text},
		}}}
	}
	notes := fantasy.FilePart{Filename: "notes.txt", MediaType: "text/plain", Data: []byte("hi")}
	msgs := []fantasy.Message{
		fantasy.NewUserMessage("what is [Image #1]?", fantasy.FilePart{Filename: shotPath, MediaType: "image/png", Data: []byte("png")}),
		fantasy.NewUserMessage("and these notes", notes),
		media("Image read: a.png"),
		media(""),
		fantasy.NewUserMessage("plain"),
	}
	before, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if got := stripImages(msgs, true, "Eye"); &got[0] != &msgs[0] {
		t.Fatal("stripImages for a vision model made a new slice")
	}
	got := stripImages(msgs, false, "GLM")
	if after, _ := json.Marshal(msgs); !bytes.Equal(after, before) {
		t.Fatal("stripImages wrote to its input")
	}
	result := func(m fantasy.Message) fantasy.ToolResultOutputContent {
		r, _ := fantasy.AsMessagePart[fantasy.ToolResultPart](m.Content[0])
		return r.Output
	}
	if text, want := messageText(got[0]), "what is [Image #1]? [Image omitted: GLM does not accept images. File: "+shotPath+"]"; text != want {
		t.Fatalf("the pasted prompt = %q, want %q", text, want)
	}
	if !reflect.DeepEqual(got[1], msgs[1]) || !reflect.DeepEqual(got[4], msgs[4]) {
		t.Fatal("a message with no image was changed")
	}
	if o, ok := result(got[2]).(fantasy.ToolResultOutputContentText); !ok || o.Text != "Image read: a.png\n[Image omitted: GLM does not accept images]" {
		t.Fatalf("the image result = %#v; want a text result of its text and the placeholder", result(got[2]))
	}
	if o, ok := result(got[3]).(fantasy.ToolResultOutputContentText); !ok || o.Text != "[Image omitted: GLM does not accept images]" {
		t.Fatalf("the textless image result = %#v; want the placeholder alone", result(got[3]))
	}
	plain := []fantasy.Message{fantasy.NewUserMessage("plain"), msgs[1]}
	if got := stripImages(plain, false, "GLM"); &got[0] != &plain[0] {
		t.Fatal("stripImages made a new slice for a history with no image")
	}
}

// TestOutputOfKeepsAnImage: the output the harness writes itself for a
// synthetic or partial step (resultPart) is Fantasy's own for the same
// response (agent.go:859-875) — an image response a media output of its
// base64, its type and its text, never flattened to text — so a tool's image
// survives every path into the transcript.
func TestOutputOfKeepsAnImage(t *testing.T) {
	img := testPNG(t, 8, 8)
	r := fantasy.NewImageResponse(img, "image/png")
	r.Content = "Image read: a.png"
	want := fantasy.ToolResultOutputContentMedia{Data: base64.StdEncoding.EncodeToString(img), MediaType: "image/png", Text: "Image read: a.png"}
	if got := outputOf(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("outputOf(an image response) = %#v, want %#v", got, want)
	}
	if got := outputOf(fantasy.NewTextResponse("ok")); !reflect.DeepEqual(got, fantasy.ToolResultOutputContentText{Text: "ok"}) {
		t.Fatalf("outputOf(text) = %#v", got)
	}
	if got, ok := outputOf(fantasy.NewTextErrorResponse("no")).(fantasy.ToolResultOutputContentError); !ok || got.Error.Error() != "no" {
		t.Fatalf("outputOf(an error) = %#v", got)
	}
}
