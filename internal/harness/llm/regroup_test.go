package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"testing"

	"charm.land/fantasy"
)

// media is a tool result answering id whose output is an image of type mime
// (base64 of data) with text beside it, as Fantasy records a tool's image
// response.
func media(id, mime string, data []byte, text string) fantasy.ToolResultPart {
	return fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentMedia{
		Data: base64.StdEncoding.EncodeToString(data), MediaType: mime, Text: text,
	}}
}

// textResult is a tool result answering id with text.
func textResult(id, text string) fantasy.ToolResultPart {
	return fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentText{Text: text}}
}

func toolMessage(parts ...fantasy.MessagePart) fantasy.Message {
	return fantasy.Message{Role: fantasy.MessageRoleTool, Content: parts}
}

func callsMessage(ids ...string) fantasy.Message {
	m := fantasy.Message{Role: fantasy.MessageRoleAssistant}
	for _, id := range ids {
		m.Content = append(m.Content, fantasy.ToolCallPart{ToolCallID: id, ToolName: "read", Input: `{}`})
	}
	return m
}

// TestRegroupToolImages is rule 5 on its own (plan 033 §3.5, P10): in each run
// of tool messages, an image result becomes a text result of its text (or a
// line saying the image follows, with none), and one user message after the
// run carries "Images returned by tool call(s) <ids>:" and the images, in
// order; a run with no image, every other message, a non-image media result
// and an image whose base64 does not decode are carried over as they are; the
// input is never written to, and a prompt with nothing to move comes back as
// the same slice.
func TestRegroupToolImages(t *testing.T) {
	a, b, c := []byte("png a"), []byte("png b"), []byte("jpeg c")
	wav := fantasy.ToolResultPart{ToolCallID: "w", Output: fantasy.ToolResultOutputContentMedia{Data: "d2F2", MediaType: "audio/wav", Text: "a sound"}}
	broken := fantasy.ToolResultPart{ToolCallID: "x", Output: fantasy.ToolResultOutputContentMedia{Data: "not base64!", MediaType: "image/png", Text: "bad"}}
	prompt := fantasy.Prompt{
		fantasy.NewSystemMessage("system"),
		fantasy.NewUserMessage("look"),
		callsMessage("c1", "c2", "c3"),
		// One step's results, the run split over two tool messages.
		toolMessage(media("c1", "image/png", a, "Read image file: a.png"), textResult("c2", "plain")),
		toolMessage(media("c3", "image/png", b, "")),
		callsMessage("c4", "w", "x"),
		toolMessage(media("c4", "image/jpeg", c, "Read image file: c.jpg"), wav, broken),
		fantasy.NewUserMessage("a steer"),
		callsMessage("c5"),
		toolMessage(textResult("c5", "no image here")),
	}
	before, err := json.Marshal(prompt)
	if err != nil {
		t.Fatal(err)
	}
	got := regroupToolImages(prompt)
	if after, _ := json.Marshal(prompt); !bytes.Equal(after, before) {
		t.Fatal("regroupToolImages wrote to its input")
	}
	images := func(intro string, files ...fantasy.FilePart) fantasy.Message {
		m := fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: intro}}}
		for _, f := range files {
			m.Content = append(m.Content, f)
		}
		return m
	}
	want := fantasy.Prompt{
		prompt[0], prompt[1], prompt[2],
		toolMessage(textResult("c1", "Read image file: a.png"), textResult("c2", "plain")),
		toolMessage(textResult("c3", "The tool returned an image (image/png); it follows the tool results.")),
		images("Images returned by tool call(s) c1, c3:",
			fantasy.FilePart{Data: a, MediaType: "image/png"}, fantasy.FilePart{Data: b, MediaType: "image/png"}),
		prompt[5],
		toolMessage(textResult("c4", "Read image file: c.jpg"), wav, broken),
		images("Images returned by tool call(s) c4:", fantasy.FilePart{Data: c, MediaType: "image/jpeg"}),
		prompt[7], prompt[8], prompt[9],
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("regrouped =\n%s\nwant\n%s", g, w)
	}
	// The same rewrite every time: a history replayed is regrouped into the
	// same bytes (the prefix cache).
	if again := regroupToolImages(prompt); !reflect.DeepEqual(again, got) {
		t.Fatal("a second regroup of the same prompt differs from the first")
	}

	// The control: nothing to move, nothing made.
	plain := slices.Concat(prompt[:3], fantasy.Prompt{toolMessage(textResult("c1", "x"), wav, broken)}, prompt[7:])
	if out := regroupToolImages(plain); &out[0] != &plain[0] || len(out) != len(plain) {
		t.Fatal("a prompt with no image to move came back as a new slice")
	}
	if out := regroupToolImages(nil); out != nil {
		t.Fatal("a nil prompt came back non-nil")
	}
}

// capture is a Chat Completions server that keeps every request body and
// answers each with a short text reply.
type capture struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (c *capture) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading a request: %v", err)
		}
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.mu.Unlock()
		sse(w, textChunk("ok"), finishChunk("stop", true))
	}
}

func (c *capture) last(t *testing.T) []byte {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("the server saw no request")
	}
	return c.bodies[len(c.bodies)-1]
}

// stream sends prompt through lm and reads the whole response.
func stream(t *testing.T, lm fantasy.LanguageModel, prompt fantasy.Prompt) {
	t.Helper()
	resp, err := lm.Stream(context.Background(), fantasy.Call{Prompt: prompt})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for part := range resp {
		if part.Type == fantasy.StreamPartTypeError {
			t.Fatalf("the stream failed: %v", part.Error)
		}
	}
}

// sentShape is a request's messages as role, and for a user message the
// number of image_url parts it carries ("user+2").
func sentShape(t *testing.T, body []byte) []string {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("a request body is not JSON: %v", err)
	}
	var out []string
	for _, m := range req.Messages {
		s := m.Role
		if n := bytes.Count(m.Content, []byte(`"type":"image_url"`)); n > 0 {
			s += "+" + string(rune('0'+n))
		}
		out = append(out, s)
	}
	return out
}

// TestWrapperRegroupsToolImagesOnTheWire is rule 5 through the real
// OpenAI-compatible client (A9's shape): two images two parallel calls
// returned go out as the step's two tool messages, together, then one user
// message holding both images. The control is the same prompt through the
// client unwrapped, Fantasy as shipped: a user message after each tool
// message, splitting the run. The caller's prompt is untouched either way.
func TestWrapperRegroupsToolImagesOnTheWire(t *testing.T) {
	var c capture
	srv := newServer(t, c.handler(t))
	prompt := fantasy.Prompt{
		fantasy.NewUserMessage("read both"),
		callsMessage("call_1", "call_2"),
		toolMessage(media("call_1", "image/png", []byte("one"), "Read image file: one.png"),
			media("call_2", "image/png", []byte("two"), "Read image file: two.png")),
	}
	before, _ := json.Marshal(prompt)

	stream(t, released(t, srv.URL), prompt)
	if got, want := sentShape(t, c.last(t)), []string{"user", "assistant", "tool", "user+1", "tool", "user+1"}; !slices.Equal(got, want) {
		t.Fatalf("control: Fantasy as shipped sent %q; want %q (a user message splitting the run)", got, want)
	}

	stream(t, wrapped(t, srv.URL), prompt)
	body := c.last(t)
	if got, want := sentShape(t, body), []string{"user", "assistant", "tool", "tool", "user+2"}; !slices.Equal(got, want) {
		t.Fatalf("the wrapper sent %q; want %q", got, want)
	}
	for _, want := range []string{
		`"content":"Read image file: one.png","tool_call_id":"call_1"`,
		`"content":"Read image file: two.png","tool_call_id":"call_2"`,
		`"text":"Images returned by tool call(s) call_1, call_2:"`,
		`"url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString([]byte("one")) + `"`,
		`"url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString([]byte("two")) + `"`,
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("the request lacks %s:\n%s", want, body)
		}
	}
	if bytes.Index(body, []byte("base64,"+base64.StdEncoding.EncodeToString([]byte("one")))) >
		bytes.Index(body, []byte("base64,"+base64.StdEncoding.EncodeToString([]byte("two")))) {
		t.Fatal("the images are not in the calls' order")
	}
	if after, _ := json.Marshal(prompt); !bytes.Equal(after, before) {
		t.Fatal("the wrapper wrote to the caller's prompt")
	}
}
