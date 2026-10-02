package responsesapi

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

// forbiddenFields are the request fields the route's preview limitations
// list as unsupported, or that it does not apply (plan 033 §3.9): none may
// ever be in a body.
var forbiddenFields = []string{
	"max_output_tokens", "temperature", "top_p", "top_logprobs", "service_tier", "previous_response_id",
	"prompt_cache_key", "prompt_cache_retention", "metadata", "user", "truncation", "background",
	"conversation", "max_tool_calls", "moderation", "multi_agent", "prompt", "safety_identifier",
}

// topLevel is a body's top-level keys, sorted, with their raw values.
func topLevel(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("the body is not a JSON object: %v\n%s", err, body)
	}
	return m
}

func fullRequest() Request {
	parallel := true
	return Request{
		Model:             "gpt-5.6-luna",
		Instructions:      "You are craze.",
		Input:             []Item{UserMessage(InputText("hi"))},
		Tools:             []Tool{{Name: "read", Description: "Read a file.", Parameters: map[string]any{"type": "object"}}},
		ParallelToolCalls: &parallel,
		Effort:            "high",
		Headers:           map[string]string{"session-id": "sess-1"},
		UserAgent:         "craze-test",
	}
}

// TestBodySendsOnlyTheAllowedFields: a request with every field craze sets
// has exactly the allowed top-level keys, store false, stream true, and the
// encrypted reasoning included; none of the forbidden fields; and nothing of
// the request's headers. Without tools (the control on the tool fields),
// tool_choice and parallel_tool_calls go too.
func TestBodySendsOnlyTheAllowedFields(t *testing.T) {
	body, err := fullRequest().Body()
	if err != nil {
		t.Fatal(err)
	}
	m := topLevel(t, body)
	want := []string{"include", "input", "instructions", "model", "parallel_tool_calls", "reasoning", "store", "stream", "tool_choice", "tools"}
	if got := slices.Sorted(maps.Keys(m)); !slices.Equal(got, want) {
		t.Fatalf("top-level keys = %v, want exactly %v", got, want)
	}
	for _, f := range forbiddenFields {
		if _, ok := m[f]; ok {
			t.Errorf("the body sends %s", f)
		}
	}
	for key, want := range map[string]string{
		"store":               "false",
		"stream":              "true",
		"include":             `["reasoning.encrypted_content"]`,
		"instructions":        `"You are craze."`,
		"model":               `"gpt-5.6-luna"`,
		"tool_choice":         `"auto"`,
		"parallel_tool_calls": "true",
		"reasoning":           `{"effort":"high","summary":"auto"}`,
	} {
		if got := string(m[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	if strings.Contains(string(body), "sess-1") || strings.Contains(string(body), "craze-test") {
		t.Error("the request's headers leaked into its body")
	}

	r := fullRequest()
	r.Tools, r.Instructions, r.Effort = nil, "", ""
	body, err = r.Body()
	if err != nil {
		t.Fatal(err)
	}
	m = topLevel(t, body)
	if got, want := slices.Sorted(maps.Keys(m)), []string{"include", "input", "model", "reasoning", "store", "stream"}; !slices.Equal(got, want) {
		t.Fatalf("without tools, top-level keys = %v, want %v", got, want)
	}
}

// TestBodySendsToolsInOneNamespace: every function tool goes inside one
// namespace named craze (P32), as a non-strict function with its schema; a
// tool with no schema gets an empty object's.
func TestBodySendsToolsInOneNamespace(t *testing.T) {
	r := fullRequest()
	r.Tools = append(r.Tools, Tool{Name: "noop"})
	body, err := r.Body()
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"type":"namespace","name":"craze","description":"craze tools","tools":[` +
		`{"type":"function","name":"read","description":"Read a file.","parameters":{"type":"object"},"strict":false},` +
		`{"type":"function","name":"noop","parameters":{"properties":{},"type":"object"},"strict":false}]}]`
	if got := string(topLevel(t, body)["tools"]); got != want {
		t.Fatalf("tools =\n%s\nwant\n%s", got, want)
	}
}

// TestBodyReasoning: reasoning always asks for the summary, and for the
// effort when there is one; a model with no effort control sends the summary
// alone.
func TestBodyReasoning(t *testing.T) {
	for effort, want := range map[string]string{
		"":        `{"summary":"auto"}`,
		"none":    `{"effort":"none","summary":"auto"}`,
		"minimal": `{"effort":"minimal","summary":"auto"}`,
		"low":     `{"effort":"low","summary":"auto"}`,
		"medium":  `{"effort":"medium","summary":"auto"}`,
		"high":    `{"effort":"high","summary":"auto"}`,
		"xhigh":   `{"effort":"xhigh","summary":"auto"}`,
		"max":     `{"effort":"max","summary":"auto"}`,
	} {
		r := fullRequest()
		r.Effort = effort
		body, err := r.Body()
		if err != nil {
			t.Fatal(err)
		}
		if got := string(topLevel(t, body)["reasoning"]); got != want {
			t.Errorf("effort %q: reasoning = %s, want %s", effort, got, want)
		}
	}
}

// TestBodyToolChoice: "auto" unless the call says otherwise; a tool's name
// is the function choice that names it.
func TestBodyToolChoice(t *testing.T) {
	for choice, want := range map[string]string{
		"":         `"auto"`,
		"auto":     `"auto"`,
		"none":     `"none"`,
		"required": `"required"`,
		"read":     `{"type":"function","name":"read"}`,
	} {
		r := fullRequest()
		r.ToolChoice = choice
		body, err := r.Body()
		if err != nil {
			t.Fatal(err)
		}
		if got := string(topLevel(t, body)["tool_choice"]); got != want {
			t.Errorf("choice %q: tool_choice = %s, want %s", choice, got, want)
		}
	}
}

// TestItemsEncode: each input item as the route takes it — user content as
// input_text and input_image data URLs, the model's text as output_text, a
// reasoning item with its id, summary and encrypted content, a function call
// with its call_id in the craze namespace, and an output as a string or as
// content parts.
func TestItemsEncode(t *testing.T) {
	for _, tc := range []struct {
		name string
		item Item
		want string
	}{
		{"user", UserMessage(InputText("look"), InputImage("image/png", []byte{1, 2, 3})),
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AQID"}]}`},
		{"empty user", UserMessage(), `{"type":"message","role":"user","content":[]}`},
		{"assistant", AssistantMessage("done"),
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`},
		{"reasoning", Reasoning("rs_1", []string{"a", "b"}, "sealed"),
			`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"a"},{"type":"summary_text","text":"b"}],"encrypted_content":"sealed"}`},
		{"reasoning without a summary", Reasoning("rs_1", nil, "sealed"),
			`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"sealed"}`},
		{"call", FunctionCall("call_1", "read", `{"path":"a"}`),
			`{"type":"function_call","call_id":"call_1","name":"read","namespace":"craze","arguments":"{\"path\":\"a\"}"}`},
		{"call with no arguments", FunctionCall("call_1", "noop", ""),
			`{"type":"function_call","call_id":"call_1","name":"noop","namespace":"craze","arguments":"{}"}`},
		{"output", FunctionCallOutput("call_1", "file text"),
			`{"type":"function_call_output","call_id":"call_1","output":"file text"}`},
		{"output parts", FunctionCallOutputContent("call_1", InputText("an image"), InputImage("image/jpeg", []byte{9})),
			`{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"an image"},{"type":"input_image","image_url":"data:image/jpeg;base64,CQ=="}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.item)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("encoded\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
	if _, err := json.Marshal(Item{}); err == nil {
		t.Error("an item built without a constructor encoded")
	}
}

// TestBodyKeepsTextAsWritten: a prompt's <, > and & go as themselves, not
// HTML-escaped, inside items too.
func TestBodyKeepsTextAsWritten(t *testing.T) {
	r := fullRequest()
	r.Instructions = "if a < b && b > c"
	r.Input = []Item{UserMessage(InputText("<tag> & more"))}
	body, err := r.Body()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"if a < b && b > c", "<tag> & more"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the body does not hold %q as written:\n%s", want, body)
		}
	}
	if _, err := (Request{}).Body(); err == nil {
		t.Error("a request with no model built a body")
	}
}
