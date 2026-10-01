package harness

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// Native Read returning images (plan 033 §3.5, owner decision 8; §7 A9, A9b).
// The tool's own behaviour is opencode's read_image_test.go; here is what the
// turn does with it: the vision a turn's tools are told of (begin), the image
// response (toResponse), the request's regroup (llm's rule 5), the strip, and
// the transcript. The wire tests run the real stack against wire_test.go's
// Chat Completions server; the sub-agent ones the routed fixture.

// toolCallAt is one whole tool call at index in a Chat Completions stream:
// a step's parallel calls are its indexes 0, 1, ...
func toolCallAt(index int, id, name, args string) string {
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`, index, id, name, args)
}

// readArgs is a read call's arguments for path.
func readArgs(path string) string { return fmt.Sprintf(`{"filePath":%q}`, path) }

// readLine is what read says beside an image it returns that it did not
// scale: the file, its size and its type.
func readLine(path string, w, h int) string {
	return fmt.Sprintf("Read image file: %s (%d×%d image/png)", path, w, h)
}

// roles is each of a request's messages as its role, a user message carrying
// images as "user+N".
func roles(t *testing.T, body []byte) []string {
	t.Helper()
	var out []string
	for _, m := range sentMessages(t, body) {
		r := m.Role
		n := 0
		for _, p := range m.Parts {
			if p.Type == "image_url" {
				n++
			}
		}
		if n > 0 {
			r = fmt.Sprintf("%s+%d", r, n)
		}
		out = append(out, r)
	}
	return out
}

// TestTwoParallelReadsAreRegrouped (A9, P10): a vision model's two parallel
// reads of two PNGs come back as the step's two tool messages, together —
// each its read's line — then one user message: "Images returned by tool
// call(s) call_1, call_2:" and both images, in the calls' order, each the
// file's own bytes. The transcript still holds each image in its own result
// (the regroup is the request's alone), and the next turn's request replays
// the step as the same bytes. The control, Fantasy's client unwrapped, is
// llm's TestWrapperRegroupsToolImagesOnTheWire.
func TestTwoParallelReadsAreRegrouped(t *testing.T) {
	w := newWire(t,
		sseReply(toolCallAt(0, "call_1", "read", readArgs("one.png")), toolCallAt(1, "call_2", "read", readArgs("two.png")), finishChunk("tool_calls", true)),
		sseReply(textChunk("Two grey rectangles."), finishChunk("stop", true)),
		sseReply(textChunk("Nothing else."), finishChunk("stop", true)),
	)
	f, opts := wireFixture(t, w)
	withVision(f, "test/a")
	s := f.open(opts)
	one, two := testPNG(t, 40, 30), testPNG(t, 20, 50)
	pathOne, pathTwo := f.put("one.png", string(one)), f.put("two.png", string(two))
	run(t, s, "what are one.png and two.png?")
	run(t, s, "anything else?")

	bodies := w.requests()
	if len(bodies) != 3 {
		t.Fatalf("the server saw %d requests, want 3", len(bodies))
	}
	body := bodies[1]
	if got, want := roles(t, body), []string{"system", "user", "assistant", "tool", "tool", "user+2"}; !slices.Equal(got, want) {
		t.Fatalf("the step after the reads sent %q; want %q: both tool messages together, then one user message with both images", got, want)
	}
	if got, want := toolText(t, body, "call_1"), readLine(pathOne, 40, 30); got != want {
		t.Fatalf("call_1's tool message = %q, want %q", got, want)
	}
	if got, want := toolText(t, body, "call_2"), readLine(pathTwo, 20, 50); got != want {
		t.Fatalf("call_2's tool message = %q, want %q", got, want)
	}
	msgs := sentMessages(t, body)
	want := []sentPart{{Type: "text", Text: "Images returned by tool call(s) call_1, call_2:"}, {Type: "image_url"}, {Type: "image_url"}}
	want[1].ImageURL.URL, want[2].ImageURL.URL = dataURI("image/png", one), dataURI("image/png", two)
	if got := msgs[len(msgs)-1].Parts; !reflect.DeepEqual(got, want) {
		t.Fatalf("the images' user message = %+v; want the calls named, then both images in their order", got)
	}

	// The transcript: each image in its own result, as Fantasy recorded it.
	var results []fantasy.ToolResultPart
	for _, e := range transcript(t, s).Entries {
		if e.Type == store.TypeMessage && e.Message.Role == fantasy.MessageRoleTool {
			for _, p := range e.Message.Content {
				if r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p); ok {
					results = append(results, r)
				}
			}
		}
	}
	for i, data := range [][]byte{one, two} {
		if i >= len(results) {
			t.Fatalf("the transcript holds %d results, want 2", len(results))
		}
		o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](results[i].Output)
		if !ok || o.Data != base64.StdEncoding.EncodeToString(data) || o.MediaType != "image/png" {
			t.Fatalf("the transcript's result %d = %#v; want the read's image, kept in its own result", i+1, results[i].Output)
		}
	}

	// The next turn replays the step, regrouped, as the same bytes.
	if k := prefixBreak(messages(t, bodies[1]), messages(t, bodies[2])); k != -1 {
		t.Fatalf("turn 2's request breaks the regrouped step's prefix at message %d", k)
	}
}

// TestANonVisionModelsReadIsRefused (A9, the refusal): glm-5.3 accepts no
// images, so its read of a PNG is refused naming the model, and the request
// after it carries no image — no image_url and none of the file's bytes.
func TestANonVisionModelsReadIsRefused(t *testing.T) {
	w := newWire(t,
		sseReply(toolCallChunk("call_1", "read", readArgs("shot.png")), finishChunk("tool_calls", true)),
		sseReply(textChunk("I cannot see it."), finishChunk("stop", true)),
	)
	f, opts := wireFixture(t, w)
	addGLM(f)
	opts.Model = glm
	s := f.open(opts)
	img := testPNG(t, 40, 30)
	f.put("shot.png", string(img))
	run(t, s, "what is shot.png?")

	body := w.requests()[1]
	if got, want := toolText(t, body, "call_1"), "Cannot read image file: GLM 5.3 (Z.AI) does not accept images"; got != want {
		t.Fatalf("glm-5.3's read = %q, want %q", got, want)
	}
	if bytes.Contains(body, []byte("image_url")) || bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString(img))) {
		t.Fatalf("glm-5.3's request carried the image:\n%s", body)
	}
	if got, want := roles(t, body), []string{"system", "user", "assistant", "tool"}; !slices.Equal(got, want) {
		t.Fatalf("glm-5.3's request = %q, want %q", got, want)
	}
}

// TestAModelSwitchRefusesReadAndStripsItsImages (A9b, end to end through
// Read): a read on a vision model sends its image; after /model to glm-5.3,
// the next turn's read is refused naming glm-5.3, and the earlier read's
// result — still an image in the transcript — goes to glm-5.3 as its line and
// the placeholder (C5's strip), on every request of that turn; nothing is
// regrouped, since nothing is left to regroup.
func TestAModelSwitchRefusesReadAndStripsItsImages(t *testing.T) {
	w := newWire(t,
		sseReply(toolCallChunk("call_1", "read", readArgs("shot.png")), finishChunk("tool_calls", true)),
		sseReply(textChunk("A grey rectangle."), finishChunk("stop", true)),
		sseReply(toolCallChunk("call_2", "read", readArgs("shot.png")), finishChunk("tool_calls", true)),
		sseReply(textChunk("I cannot see it now."), finishChunk("stop", true)),
	)
	f, opts := wireFixture(t, w)
	withVision(f, "test/a")
	addGLM(f)
	s := f.open(opts)
	img := testPNG(t, 40, 30)
	path := f.put("shot.png", string(img))
	run(t, s, "what is shot.png?")
	if err := s.SetModel(glm); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	run(t, s, "and now?")

	bodies := w.requests()
	if len(bodies) != 4 {
		t.Fatalf("the server saw %d requests, want 4", len(bodies))
	}
	if got := imageURLs(t, bodies[1]); !slices.Equal(got, []string{dataURI("image/png", img)}) {
		t.Fatalf("the vision model was sent %d images after its read; want the file's", len(got))
	}
	for i, body := range bodies[2:] {
		if bytes.Contains(body, []byte("image_url")) || bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString(img))) {
			t.Fatalf("glm-5.3's request %d carried the image:\n%s", i+1, body)
		}
		if got, want := toolText(t, body, "call_1"), readLine(path, 40, 30)+"\n[Image omitted: GLM 5.3 (Z.AI) does not accept images]"; got != want {
			t.Fatalf("glm-5.3's request %d replays the earlier read as %q, want %q", i+1, got, want)
		}
	}
	if got, want := toolText(t, bodies[3], "call_2"), "Cannot read image file: GLM 5.3 (Z.AI) does not accept images"; got != want {
		t.Fatalf("glm-5.3's own read = %q, want %q", got, want)
	}
	// The transcript still holds the first read's image, for a later model
	// that accepts images.
	tr := transcript(t, s)
	found := false
	for _, e := range tr.Entries {
		for _, p := range e.Message.Content {
			if r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p); ok && r.ToolCallID == "call_1" {
				_, found = fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](r.Output)
			}
		}
	}
	if !found {
		t.Fatal("the transcript lost the first read's image")
	}
}

// resultIn is the output of the tool result answering id in c's prompt.
func resultIn(t *testing.T, c fantasy.Call, id string) fantasy.ToolResultOutputContent {
	t.Helper()
	for _, m := range c.Prompt {
		for _, p := range m.Content {
			if r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p); ok && r.ToolCallID == id {
				return r.Output
			}
		}
	}
	t.Fatalf("no result for %s in the request", id)
	return nil
}

// readPart is one read call of path in a scripted step.
func readPart(t *testing.T, id, path string) []fantasy.StreamPart {
	t.Helper()
	return callParts(id, "read", input(t, map[string]any{"filePath": path}))
}

// TestAChildReadsWithItsOwnModelsVision (plan 033 §3.5): a sub-agent's tools
// are told of the child's model, not the parent's. In one step the parent
// reads a PNG and starts a child on the other model, which reads it too: a
// parent that accepts images gets the image while its child, on a model that
// does not, is refused naming the child's model — and the reverse. Each is
// the other's control.
func TestAChildReadsWithItsOwnModelsVision(t *testing.T) {
	for _, parentSees := range []bool{true, false} {
		t.Run(fmt.Sprintf("parent vision=%v", parentSees), func(t *testing.T) {
			f := newRouted(t)
			if parentSees {
				withVision(f.fixture, "test/a")
			} else {
				withVision(f.fixture, "test/b")
			}
			img := testPNG(t, 40, 30)
			f.put("shot.png", string(img))
			s := f.open(f.options())
			f.routers["test/a"].route("go", callStep(readPart(t, "r1", "shot.png"),
				agentPart(t, "a1", task("look", "look at shot.png", "model", "test/b"))), answerWith("done"))
			f.routers["test/b"].route("look at shot.png", callStep(readPart(t, "r2", "shot.png")), answerWith("seen"))
			if _, err := s.Run(context.Background(), "go", nil); err != nil {
				t.Fatal(err)
			}

			parent, child := f.routers["test/a"].requests("go"), f.routers["test/b"].requests("look at shot.png")
			if len(parent) != 2 || len(child) != 2 {
				t.Fatalf("the parent sent %d requests and the child %d; want 2 each", len(parent), len(child))
			}
			check := func(who string, out fantasy.ToolResultOutputContent, sees bool, name string) {
				t.Helper()
				if sees {
					o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](out)
					if !ok || o.Data != base64.StdEncoding.EncodeToString(img) {
						t.Fatalf("the %s, whose model accepts images, got %#v; want the image", who, out)
					}
					return
				}
				text, isErr := outputText(out)
				if want := "Cannot read image file: " + name + " does not accept images"; !isErr || text != want {
					t.Fatalf("the %s, whose model does not accept images, got %q (error %v); want %q", who, text, isErr, want)
				}
			}
			check("parent", resultIn(t, parent[1], "r1"), parentSees, "Model A")
			check("child", resultIn(t, child[1], "r2"), !parentSees, "test/b")
		})
	}
}

// TestResultPartKeepsAnImage: the result part the harness writes itself, for
// a synthetic or a partial step (resultPart), keeps a tool's image — a media
// output of its bytes' base64, its type and its text, with the call's id in
// its metadata — as Fantasy's own recording of the response would; an error
// result never carries one; and the response a run hands Fantasy is an image
// response for it (toResponse), a text one otherwise.
func TestResultPartKeepsAnImage(t *testing.T) {
	img := testPNG(t, 8, 8)
	res := tool.Result{Text: "Read image file: a.png (8×8 image/png)", Media: &tool.Media{Data: img, MIME: "image/png"}}
	got := resultPart("call_1", "t1.1.1", res)
	want := fantasy.ToolResultPart{ToolCallID: "call_1", ClientMetadata: metadata("t1.1.1"), Output: fantasy.ToolResultOutputContentMedia{
		Data: base64.StdEncoding.EncodeToString(img), MediaType: "image/png", Text: res.Text,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resultPart(an image result) = %#v\nwant %#v", got, want)
	}
	if r := toResponse(res, "t1.1.1"); r.Type != "image" || !bytes.Equal(r.Data, img) || r.MediaType != "image/png" || r.Content != res.Text || r.IsError {
		t.Fatalf("toResponse(an image result) = %+v; want an image response with its text", r)
	}

	// The controls: an error with an image is an error, and a result with
	// none is text.
	res.IsError, res.Class = true, tool.ClassToolError
	if out, ok := resultPart("call_1", "t1.1.1", res).Output.(fantasy.ToolResultOutputContentError); !ok || out.Error.Error() != res.Text {
		t.Fatalf("resultPart(an error with an image) = %#v; want the error alone", out)
	}
	if r := toResponse(res, "t1.1.1"); r.Type != "text" || r.Data != nil {
		t.Fatalf("toResponse(an error with an image) = %+v; want text", r)
	}
	if r := toResponse(tool.Result{Text: "ok"}, ""); r.Type != "text" || r.Content != "ok" {
		t.Fatalf("toResponse(text) = %+v", r)
	}
}

// TestASynthesizedStepKeepsAReadImage: a step the runner writes itself — a
// Fantasy that returns without finishing a step whose read ran
// (TestSynthesisDefence's course) — keeps the read's image in the tool entry,
// as recorded, beside an aborted result for the call that never ran; and the
// read returned the image at all because the turn told its tools the model
// accepts images (begin). The control is the same course on a model that
// does not: the recorded result is the refusal.
func TestASynthesizedStepKeepsAReadImage(t *testing.T) {
	for _, vision := range []bool{true, false} {
		t.Run(fmt.Sprintf("vision=%v", vision), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			if vision {
				withVision(f, "test/a")
			}
			s := f.open(f.options())
			img := testPNG(t, 16, 16)
			f.put("shot.png", string(img))
			in := input(t, map[string]any{"filePath": "shot.png"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.newAgent = func(_ fantasy.LanguageModel, _ string, tools []fantasy.AgentTool) fantasy.Agent {
				return fakeAgent{tools: tools, run: func(ctx context.Context, tools []fantasy.AgentTool, c fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
					_ = c.OnStepStart(0)
					_ = c.OnToolInputStart("c1", "read")
					_ = c.OnToolCall(fantasy.ToolCallContent{ToolCallID: "c1", ToolName: "read", Input: in})
					_ = c.OnToolCall(fantasy.ToolCallContent{ToolCallID: "c2", ToolName: "read", Input: in})
					i := slices.IndexFunc(tools, func(tl fantasy.AgentTool) bool { return tl.Info().Name == "read" })
					resp, _ := tools[i].Run(ctx, fantasy.ToolCall{ID: "c1", Name: "read", Input: in})
					// What Fantasy records for the response (agent.go:859-875).
					_ = c.OnToolResult(fantasy.ToolResultContent{ToolCallID: "c1", ToolName: "read", Result: outputOf(resp), ClientMetadata: resp.Metadata})
					cancel()
					return nil, ctx.Err()
				}}
			}
			if res, err := s.Run(ctx, "look", nil); err != nil || res.StopReason != StopCancelled {
				t.Fatalf("Run = %+v, %v; want cancelled", res, err)
			}
			var toolEntry *store.Entry
			for i, e := range transcript(t, s).Entries {
				if e.Type == store.TypeMessage && e.Message.Role == fantasy.MessageRoleTool {
					toolEntry = &transcript(t, s).Entries[i]
				}
			}
			if toolEntry == nil || !toolEntry.Interrupted || len(toolEntry.Message.Content) != 2 {
				t.Fatalf("the synthesized tool entry = %+v; want one, interrupted, answering both calls", toolEntry)
			}
			c1, _ := fantasy.AsMessagePart[fantasy.ToolResultPart](toolEntry.Message.Content[0])
			c2, _ := fantasy.AsMessagePart[fantasy.ToolResultPart](toolEntry.Message.Content[1])
			if text, isErr := outputText(c2.Output); !isErr || text != tool.AbortedText {
				t.Fatalf("the unrun call's result = %q (error %v); want aborted", text, isErr)
			}
			if !vision {
				if text, isErr := outputText(c1.Output); !isErr || !strings.Contains(text, "Model A does not accept images") {
					t.Fatalf("control: the read on a model without vision recorded %#v; want the refusal", c1.Output)
				}
				return
			}
			o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](c1.Output)
			if !ok || o.Data != base64.StdEncoding.EncodeToString(img) || o.MediaType != "image/png" || !strings.HasPrefix(o.Text, "Read image file: ") {
				t.Fatalf("the read's result in the synthesized entry = %#v; want its image, kept", c1.Output)
			}
		})
	}
}
