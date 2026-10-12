package messagesapi

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the goldens under internal/harness/llm/messagesapi/testdata")

// forbiddenFields are the request fields craze never sends (plan 044 §3.2,
// D2): none may ever be in a body, at its top level or on a tool.
var forbiddenFields = []string{
	"temperature", "top_p", "top_k", "stop_sequences", "metadata", "speed", "fallbacks", "inference_geo",
	"eager_input_streaming",
}

// topLevel is a body's top-level keys with their raw values.
func topLevel(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("the body is not a JSON object: %v\n%s", err, body)
	}
	return m
}

func body(t *testing.T, r Request) []byte {
	t.Helper()
	b, err := r.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	return b
}

// fullRequest is a request with every field craze sets, and every content
// block: a step's signed thinking (one with no text), redacted thinking,
// text, two calls (one with no input), and the results — text, an error,
// an empty one, one with an image — with the user's steer after them.
func fullRequest() Request {
	png := []byte("\x89PNG\r\n\x1a\nnot-really")
	return Request{
		Model:       "claude-opus-5-5",
		MaxTokens:   64000,
		System:      []string{"You are craze.", "Project notes: <none> & more."},
		Tools:       []Tool{{Name: "read", Description: "Read a file.", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}, {Name: "noop"}},
		ToolChoice:  "none",
		Thinking:    &ThinkingConfig{Display: "summarized", DropMismatched: true},
		Effort:      "medium",
		CacheSystem: true,
		CacheAuto:   true,
		Headers:     map[string]string{"X-Extra": "header-not-body"},
		Messages: []Message{
			{Role: RoleUser, Content: []Content{Text("Read go.mod <now> & tell me."), Image("image/png", png)}},
			{Role: RoleAssistant, Content: []Content{
				Thinking("", "sig-empty-text"),
				Thinking("Which file?", "sig-1"),
				RedactedThinking("opaque-redacted"),
				Text("Reading."),
				ToolUse("toolu_01", "read", json.RawMessage(` { "path" : "go.mod" } `)),
				ToolUse("toolu_02", "noop", nil),
			}},
			{Role: RoleUser, Content: []Content{
				ToolResult("toolu_01", false, Text("module x")),
				ToolResult("toolu_02", true, Text("noop failed")),
				ToolResult("toolu_03", false),
				ToolResult("toolu_04", false, Text("a picture"), Image("image/webp", []byte("webp"))),
				Text("and be quick"),
			}},
		},
	}
}

// TestBodyGolden: the full request's body, byte for byte
// (testdata/body_full.golden): the field order, both cache breakpoints,
// adaptive thinking with drop_block, the effort, every block's shape, and
// nothing HTML-escaped.
func TestBodyGolden(t *testing.T) {
	got := body(t, fullRequest())
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, got, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	const golden = "testdata/body_full.golden"
	if *updateGolden {
		if err := os.WriteFile(golden, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(golden, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if pretty.String() != string(want) {
		t.Fatalf("body =\n%s\nwant (%s)\n%s", pretty.String(), golden, want)
	}
}

// TestBodySendsOnlyTheAllowedFields: the full request has exactly the keys
// craze sends — stream true, max_tokens always — and none of the forbidden
// fields, at the top level or on a tool; a tool has its name, description
// and schema alone. The request's headers are not in it.
func TestBodySendsOnlyTheAllowedFields(t *testing.T) {
	b := body(t, fullRequest())
	m := topLevel(t, b)
	want := []string{"cache_control", "max_tokens", "messages", "model", "output_config", "stream", "system", "thinking", "tool_choice", "tools"}
	if got := slices.Sorted(maps.Keys(m)); !slices.Equal(got, want) {
		t.Fatalf("top-level keys = %v, want exactly %v", got, want)
	}
	for _, f := range forbiddenFields {
		if strings.Contains(string(b), `"`+f+`"`) {
			t.Errorf("the body sends %s", f)
		}
	}
	for key, want := range map[string]string{
		"model":         `"claude-opus-5-5"`,
		"max_tokens":    "64000",
		"stream":        "true",
		"cache_control": `{"type":"ephemeral"}`,
		"thinking":      `{"type":"adaptive","display":"summarized","block_binding":{"prefix_mismatch_behavior":"drop_block"}}`,
		"output_config": `{"effort":"medium"}`,
		"tool_choice":   `{"type":"none"}`,
	} {
		if got := string(m[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(m["tools"], &tools); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if got := slices.Sorted(maps.Keys(tool)); !slices.Equal(got, []string{"description", "input_schema", "name"}) &&
			!slices.Equal(got, []string{"input_schema", "name"}) {
			t.Errorf("a tool's keys = %v", got)
		}
	}
	if strings.Contains(string(b), "header-not-body") {
		t.Error("the request's headers leaked into its body")
	}
}

// TestBodyMinimal: a request with nothing optional sends model, max_tokens,
// stream and messages alone — no thinking (a model without adaptive
// thinking), no output_config, no cache_control, no system, no tools, no
// tool_choice.
func TestBodyMinimal(t *testing.T) {
	m := topLevel(t, body(t, minimalRequest()))
	if got, want := slices.Sorted(maps.Keys(m)), []string{"max_tokens", "messages", "model", "stream"}; !slices.Equal(got, want) {
		t.Fatalf("top-level keys = %v, want %v", got, want)
	}
	if got := string(m["messages"]); got != `[{"role":"user","content":[{"type":"text","text":"hi"}]}]` {
		t.Fatalf("messages = %s", got)
	}
}

// TestBodyThinking: adaptive is the only type; display goes when set, and
// block_binding exactly when DropMismatched.
func TestBodyThinking(t *testing.T) {
	for _, tc := range []struct {
		thinking *ThinkingConfig
		want     string
	}{
		{nil, ""},
		{&ThinkingConfig{}, `{"type":"adaptive"}`},
		{&ThinkingConfig{Display: "summarized"}, `{"type":"adaptive","display":"summarized"}`},
		{&ThinkingConfig{DropMismatched: true}, `{"type":"adaptive","block_binding":{"prefix_mismatch_behavior":"drop_block"}}`},
	} {
		r := minimalRequest()
		r.Thinking = tc.thinking
		if got := string(topLevel(t, body(t, r))["thinking"]); got != tc.want {
			t.Errorf("thinking %+v = %s, want %s", tc.thinking, got, tc.want)
		}
		if r.binding() != (tc.thinking != nil && tc.thinking.DropMismatched) {
			t.Errorf("thinking %+v: binding %v", tc.thinking, r.binding())
		}
	}
}

// TestBodyCacheBreakpoints: CacheSystem marks the last system block only,
// and no block without it (or with no system prompt); CacheAuto is the
// top-level field alone.
func TestBodyCacheBreakpoints(t *testing.T) {
	r := minimalRequest()
	r.System = []string{"one", "two"}
	r.CacheSystem = true
	m := topLevel(t, body(t, r))
	if got := string(m["system"]); got != `[{"type":"text","text":"one"},{"type":"text","text":"two","cache_control":{"type":"ephemeral"}}]` {
		t.Errorf("system = %s", got)
	}
	if _, ok := m["cache_control"]; ok {
		t.Error("CacheSystem alone sent the top-level cache_control")
	}

	r.CacheSystem, r.CacheAuto = false, true
	m = topLevel(t, body(t, r))
	if got := string(m["system"]); got != `[{"type":"text","text":"one"},{"type":"text","text":"two"}]` {
		t.Errorf("system without CacheSystem = %s", got)
	}
	if got := string(m["cache_control"]); got != `{"type":"ephemeral"}` {
		t.Errorf("cache_control = %s", got)
	}

	r = minimalRequest()
	r.CacheSystem = true
	if _, ok := topLevel(t, body(t, r))["system"]; ok {
		t.Error("CacheSystem with no system prompt sent one")
	}
}

// TestBodyBlocks: each content block encodes exactly its type's fields — a
// thinking block's empty text kept, a call's empty input sent as {} and its
// input compacted as it is, a tool result's content left out when empty and
// is_error only when set.
func TestBodyBlocks(t *testing.T) {
	for _, tc := range []struct {
		c    Content
		want string
	}{
		{Text("a <b> & c"), `{"type":"text","text":"a <b> & c"}`},
		{Image("image/gif", []byte("GIF89a")), `{"type":"image","source":{"type":"base64","media_type":"image/gif","data":"R0lGODlh"}}`},
		{Thinking("", "sig"), `{"type":"thinking","thinking":"","signature":"sig"}`},
		{Thinking("t", ""), `{"type":"thinking","thinking":"t","signature":""}`},
		{RedactedThinking("blob"), `{"type":"redacted_thinking","data":"blob"}`},
		{ToolUse("id1", "read", nil), `{"type":"tool_use","id":"id1","name":"read","input":{}}`},
		{ToolUse("id1", "read", json.RawMessage("  ")), `{"type":"tool_use","id":"id1","name":"read","input":{}}`},
		{ToolUse("id1", "read", json.RawMessage(`{"z": 1, "a": "<x>"}`)), `{"type":"tool_use","id":"id1","name":"read","input":{"z":1,"a":"<x>"}}`},
		{ToolResult("id1", false), `{"type":"tool_result","tool_use_id":"id1"}`},
		{ToolResult("id1", true, Text("boom")), `{"type":"tool_result","tool_use_id":"id1","content":[{"type":"text","text":"boom"}],"is_error":true}`},
	} {
		got, err := encode(tc.c)
		if err != nil {
			t.Fatalf("%s: %v", tc.want, err)
		}
		r := minimalRequest()
		r.Messages[0].Content = []Content{tc.c}
		if string(got) != tc.want {
			t.Errorf("block = %s, want %s", got, tc.want)
		}
		if in := string(topLevel(t, body(t, r))["messages"]); !strings.Contains(in, tc.want) {
			t.Errorf("the body's messages %s do not hold %s as it is", in, tc.want)
		}
	}
}

// TestBodyIsDeterministic: the same request encodes to the same bytes, every
// time — a map anywhere would order its keys differently — so the prompt
// cache and the thinking signatures see the same prefix.
func TestBodyIsDeterministic(t *testing.T) {
	first := body(t, fullRequest())
	for range 20 {
		if again := body(t, fullRequest()); !bytes.Equal(again, first) {
			t.Fatalf("a second encoding differs:\n%s\n%s", first, again)
		}
	}
	if !bytes.Contains(first, []byte("<now> & tell me")) {
		t.Fatal("the body HTML-escaped the prompt")
	}
}

// TestBodyRefuses: a request that could only be refused is an error before
// anything is sent, and the error quotes nothing of the request.
func TestBodyRefuses(t *testing.T) {
	secretish := "do-not-echo-0001"
	for name, mutate := range map[string]func(*Request){
		"no model":            func(r *Request) { r.Model = " " },
		"no max_tokens":       func(r *Request) { r.MaxTokens = 0 },
		"no messages":         func(r *Request) { r.Messages = nil },
		"another role":        func(r *Request) { r.Messages[0].Role = Role(secretish) },
		"an empty message":    func(r *Request) { r.Messages[0].Content = nil },
		"a zero block":        func(r *Request) { r.Messages[0].Content = []Content{{}} },
		"thinking in results": func(r *Request) { r.Messages[0].Content = []Content{ToolResult("t", false, Thinking(secretish, "s"))} },
		"a forced tool":       func(r *Request) { r.ToolChoice = secretish },
		"input that is no JSON": func(r *Request) {
			r.Messages = append(r.Messages, Message{Role: RoleAssistant, Content: []Content{ToolUse("t", "read", json.RawMessage("{"+secretish))}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := minimalRequest()
			mutate(&r)
			_, err := r.Body()
			if err == nil {
				t.Fatal("Body accepted it")
			}
			if strings.Contains(err.Error(), secretish) {
				t.Fatalf("the error %q quotes the request", err)
			}
		})
	}
	for _, choice := range []string{"", "auto", "none"} {
		r := minimalRequest()
		r.ToolChoice = choice
		if _, err := r.Body(); err != nil {
			t.Errorf("tool choice %q: %v", choice, err)
		}
	}
}
