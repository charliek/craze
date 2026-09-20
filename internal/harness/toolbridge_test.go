package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/schema"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/tool/opencode"
)

// TestBridgedInfo: a bridged tool's schema is the Spec's own, hand-written —
// never derived by reflection — deep-copied every time, so Fantasy's
// normalization of the schema it sends (in place, agent.go:1131-1137) never
// reaches the Spec shared by every step; and required is [] when the Spec
// has none, never null. The control: normalizing the Spec's own map does
// change it, which is why the copy is taken.
func TestBridgedInfo(t *testing.T) {
	spec := func() tool.Spec {
		return tool.Spec{
			ID: "t", Description: "d", Kind: tool.KindRead, Parallel: true, Required: []string{},
			Parameters: map[string]any{
				"maybe": map[string]any{"type": []any{"string", "null"}},
				"list":  map[string]any{"type": "array"},
				"enum":  map[string]any{"type": "string", "enum": []string{"a", "b"}},
			},
		}
	}
	b := newBridged(spec(), nil)
	info := b.Info()
	if info.Name != "t" || info.Description != "d" || !info.Parallel || info.Required == nil || len(info.Required) != 0 {
		t.Fatalf("Info() = %+v", info)
	}
	if raw, _ := json.Marshal(info); strings.Contains(string(raw), `"required":null`) {
		t.Fatalf("required marshals as null: %s", raw)
	}
	// The schema goes out as written, whatever Go values a Spec holds it in.
	offered, _ := json.Marshal(info.Parameters)
	written, _ := json.Marshal(spec().Parameters)
	if string(offered) != string(written) {
		t.Fatalf("the offered schema is %s, the Spec's %s", offered, written)
	}
	schema.Normalize(map[string]any{"type": "object", "properties": info.Parameters, "required": info.Required})
	info.Parameters["enum"].(map[string]any)["enum"].([]any)[0] = "changed"
	if !reflect.DeepEqual(b.spec, spec()) {
		t.Fatalf("normalizing Info()'s schema changed the Spec: %+v", b.spec.Parameters)
	}
	if again, _ := json.Marshal(b.Info().Parameters); string(again) != string(offered) {
		t.Fatalf("a second Info() = %s, want the first's %s", again, offered)
	}
	if nilReq := newBridged(tool.Spec{ID: "n"}, nil).Info(); nilReq.Required == nil || nilReq.Parameters == nil {
		t.Fatalf("a Spec with nothing gave %+v; want [] and {}", nilReq)
	}

	// The shipped profile's schemas go out as written too — they hold
	// integers at JSON's exact limit, which the canonical form keeps exact.
	p, err := opencode.Profile()
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range p.Tools {
		s := tl.Spec()
		offered, _ := json.Marshal(newBridged(s, nil).Info().Parameters)
		written, _ := json.Marshal(s.Parameters)
		if string(offered) != string(written) {
			t.Errorf("%s is offered %s, its Spec says %s", s.ID, offered, written)
		}
	}

	own := spec()
	schema.Normalize(map[string]any{"type": "object", "properties": own.Parameters})
	if reflect.DeepEqual(own, spec()) {
		t.Fatal("control: Normalize no longer changes a schema in place, so the copy proves nothing")
	}
}

// TestBridgedInfoCopiesAnyContainer: a profile writes its schemas however it
// likes — the registry takes any value that marshals — so the copy Info
// hands Fantasy must be total, not just of the containers Go's JSON decoder
// happens to produce. Here a parameter holds a []map[string]any, whose
// elements a copy that walked only map[string]any and []any would share with
// the Spec: the control shows exactly that sharing, and Info does not have
// it — what it returns can be changed without the Spec, or a later Info,
// changing with it.
func TestBridgedInfoCopiesAnyContainer(t *testing.T) {
	nested := map[string]any{"type": "string"}
	spec := tool.Spec{ID: "t", Description: "d", Required: []string{}, Kind: tool.KindRead,
		Parameters: map[string]any{"x": map[string]any{"anyOf": []map[string]any{nested}}}}

	b := newBridged(spec, nil)
	got := b.Info().Parameters["x"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)
	got["type"] = "changed"
	if nested["type"] != "string" {
		t.Fatalf("Info's copy shares the Spec's nested map: %v", nested)
	}
	if again := b.Info().Parameters["x"].(map[string]any)["anyOf"].([]any)[0].(map[string]any); again["type"] != "string" {
		t.Fatalf("a later Info() carries the change: %v", again)
	}
	// What the model is offered is the schema as written, whatever container
	// it was written in.
	if raw, _ := json.Marshal(b.Info().Parameters); string(raw) != `{"x":{"anyOf":[{"type":"string"}]}}` {
		t.Fatalf("the offered schema = %s", raw)
	}

	// The control: without the canonical form, a structural copy of the same
	// Spec shares the nested map, and changing it changes the Spec.
	shared := cloneSchema(spec.Parameters).(map[string]any)["x"].(map[string]any)["anyOf"].([]map[string]any)[0]
	shared["type"] = "mutated"
	if nested["type"] != "mutated" {
		t.Fatal("control: the structural copy no longer shares the nested map, so the canonical form proves nothing")
	}
}

// TestBridgedInfoKeepsNumbersExact (round-2 review, then the live smoke):
// the canonical form must not round a number, or the tools the header hashes
// and the tools sent would say different things — and it must hand Fantasy a
// number a provider's own encoder can write. json.Number is a string
// underneath: Fireworks rejected the first live call with 'is not of type
// number' because the SDK wrote it as one. So a literal keeps its value, as
// an int64 where it fits, and openTools hashes that same canonical form.
// The control is the same schema decoded the ordinary way, through float64,
// which loses the integer past 2^53.
func TestBridgedInfoKeepsNumbersExact(t *testing.T) {
	spec := tool.Spec{ID: "t", Description: "d", Required: []string{}, Kind: tool.KindRead,
		Parameters: map[string]any{"n": map[string]any{
			"type":       "integer",
			"maximum":    int64(9007199254740993), // 2^53 + 1
			"multipleOf": json.Number("1.0"),
		}}}
	// What openTools hashes: the canonical form of the registered spec.
	hashed, err := tool.SpecsJSON([]tool.Spec{{ID: spec.ID, Description: spec.Description, Required: spec.Required,
		Parameters: canonicalSchema(spec.Parameters)}})
	if err != nil {
		t.Fatal(err)
	}
	offered, err := tool.SpecsJSON([]tool.Spec{{ID: spec.ID, Description: spec.Description, Required: spec.Required,
		Parameters: newBridged(spec, nil).Info().Parameters}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(offered, hashed) {
		t.Fatalf("the tools offered are\n%s\nthe tools hashed\n%s", offered, hashed)
	}
	if !bytes.Contains(offered, []byte("9007199254740993")) {
		t.Fatalf("2^53+1 did not survive: %s", offered)
	}
	// Every number is a JSON number, not a quoted one: that is what a provider
	// validates, and what json.Number broke.
	for _, quoted := range []string{`"9007199254740993"`, `"1"`, `"1.0"`} {
		if bytes.Contains(offered, []byte(quoted)) {
			t.Fatalf("a number was offered as a string (%s): %s", quoted, offered)
		}
	}

	// The control: decoding through float64 loses the large integer.
	var loose map[string]any
	if err := json.Unmarshal([]byte(`{"maximum":9007199254740993}`), &loose); err != nil {
		t.Fatal(err)
	}
	if again, _ := json.Marshal(loose); bytes.Contains(again, []byte("9007199254740993")) {
		t.Fatal("control: a plain decode no longer changes this number, so the canonical form proves nothing")
	}

	// The assertions above marshal with encoding/json, which writes a
	// json.Number unquoted — which is why they stayed green through the live
	// failure. The provider SDK's encoder quotes it instead, so the invariant
	// has to be checked on the value handed over, not on its JSON.
	if where := findJSONNumber(newBridged(spec, nil).Info().Parameters, "parameters"); where != "" {
		t.Fatalf("a json.Number survived canonicalization at %s: a provider SDK writes it as a string", where)
	}
}

// findJSONNumber returns the path of the first json.Number in a schema value,
// or "" when there is none.
func findJSONNumber(v any, path string) string {
	switch v := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if where := findJSONNumber(v[k], path+"."+k); where != "" {
				return where
			}
		}
	case []any:
		for i, x := range v {
			if where := findJSONNumber(x, fmt.Sprintf("%s[%d]", path, i)); where != "" {
				return where
			}
		}
	case json.Number:
		return path
	}
	return ""
}

// TestBridgedRunNeverFails: a bridged tool's Run hands Fantasy the result
// and never a Go error, which Fantasy would treat as fatal to the turn; an
// error result without text still carries some (the store refuses an empty
// one); and the result's metadata is the harness id.
func TestBridgedRunNeverFails(t *testing.T) {
	for _, res := range []tool.Result{
		{Text: "fine"},
		{Text: "broke", IsError: true, Class: tool.ClassToolError},
		{IsError: true},
	} {
		b := &bridged{spec: tool.Spec{ID: "t"}, run: func(context.Context, fantasy.ToolCall) fantasy.ToolResponse { return toResponse(res, "t1.2.3") }}
		resp, err := b.Run(context.Background(), fantasy.ToolCall{ID: "c"})
		if err != nil {
			t.Fatalf("Run returned an error: %v", err)
		}
		if resp.IsError != res.IsError || strings.TrimSpace(resp.Content) == "" || resp.Metadata != `{"id":"t1.2.3"}` {
			t.Fatalf("Run(%+v) = %+v", res, resp)
		}
	}
}

// bothImports lists the files among srcs that import Fantasy and the tool
// framework both.
func bothImports(t *testing.T, srcs map[string]string) []string {
	t.Helper()
	var both []string
	for name, src := range srcs {
		f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		var fantasyImp, toolImp bool
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			fantasyImp = fantasyImp || p == "charm.land/fantasy" || strings.HasPrefix(p, "charm.land/fantasy/")
			toolImp = toolImp || p == "github.com/charliek/craze/internal/harness/tool" || strings.HasPrefix(p, "github.com/charliek/craze/internal/harness/tool/")
		}
		if fantasyImp && toolImp {
			both = append(both, name)
		}
	}
	slices.Sort(both)
	return both
}

// TestSeamOne (plan 019 §3.1): toolbridge.go is the one file of the harness
// that imports both Fantasy and the tool framework, so replacing Fantasy's
// loop touches it and turn.go, and no tool. The control: the check finds a
// file that imports both.
func TestSeamOne(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	srcs := map[string]string{}
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		srcs[n] = string(b)
	}
	if got := bothImports(t, srcs); !slices.Equal(got, []string{"toolbridge.go"}) {
		t.Fatalf("files importing both Fantasy and the tool framework: %v; want toolbridge.go alone", got)
	}
	control := map[string]string{"x.go": "package x\nimport (\n\t\"charm.land/fantasy\"\n\t\"github.com/charliek/craze/internal/harness/tool/opencode\"\n)\n"}
	if got := bothImports(t, control); len(got) != 1 {
		t.Fatal("control: a file importing both was not found")
	}
}

// TestEventsRoundTrip: every event is plain data that survives a JSON round
// trip unchanged — what a journal records is what the sink was handed. The
// control is why ToolCalled's input is a string: a call's raw arguments may
// not be valid JSON, and as a json.RawMessage they would not encode at all.
func TestEventsRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 19, 10, 0, 0, 123456789, time.UTC)
	all := []Event{
		TextDelta{Text: "hi"},
		ThoughtDelta{Text: "hmm"},
		ToolStarted{ID: "t1.1.1", Step: 1, Tool: "read", Kind: tool.KindRead, ReadOnly: true},
		ToolCalled{ID: "t1.1.1", CallID: "c1", At: at, Request: ToolRequest{Tool: "read", Kind: tool.KindRead, ReadOnly: true,
			Title: "a.txt", Paths: []string{"/w/a.txt"}, Command: "", Workdir: "", Input: `{"filePath":"a.tx`}},
		ToolProgress{ID: "t1.1.2", Output: "line\n"},
		ToolFinished{ID: "t1.1.2", At: at, Duration: 1500 * time.Millisecond, Result: tool.Result{
			Text: "out", IsError: true, Class: tool.ClassTimeout, Content: "c",
			Output: &tool.ExecOutput{ExitCode: 124, Output: "o", Duration: time.Second},
			Edits:  []tool.FileEdit{{Path: "/w/a", Old: "x", New: "y"}},
			Trunc:  tool.Truncation{KeptBytes: 1, TotalBytes: 2, KeptLines: 3, TotalLines: 4, Spill: "/h/tool-output/tool_t1.1.2"},
		}},
		StepDone{Step: 2, Provider: "p", Model: "m", WireModel: "w", Finish: "tool-calls", FinishRaw: "stop", StopReason: StopToolUse,
			TimeToFirstToken: time.Millisecond, Usage: Usage{Input: 1, Output: 2, Reasoning: 3, CacheRead: 4, CacheCreation: 5},
			Saved: true, Entries: []string{"a1b2c3d4"}},
		StepDone{Step: 3, SaveError: "store: closed"},
		Retrying{Delay: time.Second, Attempt: 1, Reason: "HTTP 503: busy"},
		Diag{Kind: DiagNotExecuted, Fields: map[string]string{"step": "1"}},
	}
	for _, ev := range all {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("%T does not encode: %v", ev, err)
		}
		back := reflect.New(reflect.TypeOf(ev))
		if err := json.Unmarshal(b, back.Interface()); err != nil {
			t.Fatalf("%T does not decode: %v", ev, err)
		}
		if got := back.Elem().Interface(); !reflect.DeepEqual(got, ev) {
			t.Errorf("%T round-tripped to\n%#v\nwant\n%#v", ev, got, ev)
		}
	}
	if _, err := json.Marshal(tool.Request{Input: json.RawMessage(`{"filePath":"a.tx`)}); err == nil {
		t.Fatal("control: a request with truncated raw input encoded")
	}
}
