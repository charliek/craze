package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The native adapter's events through this file's projection, which is what
// `craze prompt --json` writes for every event it sees (writeEvent calls
// encodeEvent). A real native session drives it, with a scripted model and
// the harness's real tools in a temporary workspace, so the tool objects
// here are the ones a headless caller would read.

// nativeJSONModel is a fantasy.LanguageModel that answers each Stream call
// with the next queued step (agent.NewNative's tweak seam, plan 018 §3.8).
// With no step left it fails, which is how this test provokes a failed turn.
type nativeJSONModel struct {
	steps [][]fantasy.StreamPart
	calls int
}

func (m *nativeJSONModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	if m.calls >= len(m.steps) {
		return nil, errors.New("nativeJSONModel: no step queued")
	}
	step := m.steps[m.calls]
	m.calls++
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range step {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (m *nativeJSONModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("nativeJSONModel: Generate is not used")
}

func (m *nativeJSONModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("nativeJSONModel: GenerateObject is not used")
}

func (m *nativeJSONModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("nativeJSONModel: StreamObject is not used")
}

func (m *nativeJSONModel) Provider() string { return "test" }
func (m *nativeJSONModel) Model() string    { return "wire" }

// nativeJSONCall is one tool call and a "tool-calls" finish: the step runs
// the tool and the turn goes on to the next one.
func nativeJSONCall(id, name, input string) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: name},
		{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: input},
		{Type: fantasy.StreamPartTypeToolInputEnd, ID: id},
		{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: name, ToolCallInput: input},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
	}
}

// nativeJSONAnswer is a step that thinks, answers and ends the turn.
func nativeJSONAnswer(thought, text string) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeReasoningStart, ID: "r"},
		{Type: fantasy.StreamPartTypeReasoningDelta, ID: "r", Delta: thought},
		{Type: fantasy.StreamPartTypeReasoningEnd, ID: "r"},
		{Type: fantasy.StreamPartTypeTextStart, ID: "0"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: text},
		{Type: fantasy.StreamPartTypeTextEnd, ID: "0"},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
	}
}

// nativeJSONSession is a started native session on a two-model table (so a
// model switch has somewhere to go), with the scripted model behind both.
func nativeJSONSession(t *testing.T, model *nativeJSONModel, ws string) agent.Session {
	t.Helper()
	table := &modeltable.Table{
		DefaultModel: "test/a",
		Providers: map[string]modeltable.Provider{
			"test": {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"NATIVE_JSON_TEST_KEY"}},
		},
		Models: map[string]modeltable.Model{
			"test/a": {Provider: "test", WireModel: "wire-a", Name: "A"},
			"test/b": {Provider: "test", WireModel: "wire-b", Name: "B"},
		},
	}
	home := t.TempDir()
	// ContentHome is an empty directory of this test's own: from plan 022 C3 a
	// native Start reads the user's Claude commands and skills, and this
	// file's assertions are about the event projection, not about whatever the
	// machine running them has installed.
	sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()}, func(o *harness.Options) {
		o.Home = home
		o.Table = table
		o.Getenv = func(k string) string {
			if k == "NATIVE_JSON_TEST_KEY" {
				return "test-key"
			}
			return ""
		}
		o.NewModel = func(modeltable.Resolved) (fantasy.LanguageModel, error) { return model, nil }
		o.Now = func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }
	})
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// drainNativeEvents is every event the session has already buffered. The
// adapter emits a turn's whole stream before its Prompt returns, so after
// one this is the turn.
func drainNativeEvents(sess agent.Session) []agent.Event {
	var out []agent.Event
	for {
		select {
		case ev := <-sess.Events():
			out = append(out, ev)
		default:
			return out
		}
	}
}

// encodeNativeEvents writes evs the way `craze prompt --json` writes them
// and returns one decoded object per line.
func encodeNativeEvents(t *testing.T, evs []agent.Event) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	for _, ev := range evs {
		if err := encodeEvent(&buf, ev); err != nil {
			t.Fatalf("encoding a %s event: %v", ev.Type, err)
		}
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("a line of the JSON stream is not an object: %q (%v)", line, err)
		}
		out = append(out, obj)
	}
	return out
}

// TestNativeToolsInThePromptJSONStream: a turn of three real calls, as
// `craze prompt --json` would print it. The tool objects are toolJSON's
// projection of the rows the adapter merged (plan 019 §3.10) — one object
// per update, the last of each call carrying its result.
func TestNativeToolsInThePromptJSONStream(t *testing.T) {
	ws := t.TempDir()
	for name, body := range map[string]string{"main.go": "package main\n", "notes.txt": "alpha\n"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	model := &nativeJSONModel{steps: [][]fantasy.StreamPart{
		nativeJSONCall("c1", "read", `{"filePath":"main.go"}`),
		nativeJSONCall("c2", "edit", `{"filePath":"notes.txt","oldString":"alpha","newString":"beta"}`),
		nativeJSONCall("c3", "bash", `{"command":"echo boom >&2; exit 3"}`),
		nativeJSONAnswer("thinking", "done"),
	}}
	sess := nativeJSONSession(t, model, ws)
	if _, err := sess.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	objs := encodeNativeEvents(t, drainNativeEvents(sess))

	// The last object for each call, which is the one holding its result.
	last := map[string]map[string]any{}
	var order []string
	for _, o := range objs {
		if o["type"] != "tool" {
			continue
		}
		id, _ := o["id"].(string)
		if _, seen := last[id]; !seen {
			order = append(order, id)
		}
		last[id] = o
	}
	if len(order) != 3 {
		t.Fatalf("got tool objects for %v, want three calls", order)
	}

	read := last[order[0]]
	if read["name"] != "read" || read["status"] != "completed" || read["kind"] != "read" || read["title"] != "main.go" {
		t.Fatalf("read object: %v", read)
	}
	out, _ := read["output"].(map[string]any)
	if out == nil || !strings.Contains(out["content"].(string), "package main") {
		t.Fatalf("read object: output %v, want the file it read", read["output"])
	}

	edit := last[order[1]]
	diffs, _ := edit["diffs"].([]any)
	if len(diffs) != 1 {
		t.Fatalf("edit object: diffs %v, want one", edit["diffs"])
	}
	d, _ := diffs[0].(map[string]any)
	if d["added"] != float64(1) || d["removed"] != float64(1) || d["truncated"] != false {
		t.Fatalf("edit object: diff %v, want +1 −1 untruncated", d)
	}

	cmd := last[order[2]]
	out, _ = cmd["output"].(map[string]any)
	switch {
	case cmd["kind"] != "execute" || cmd["title"] != "echo boom >&2; exit 3":
		t.Fatalf("bash object: %v", cmd)
	case out == nil || out["exitCode"] != float64(3):
		t.Fatalf("bash object: output %v, want exit 3", cmd["output"])
	case !strings.Contains(out["stdout"].(string), "boom"):
		t.Fatalf("bash object: stdout %v, want the command's output", out["stdout"])
	case out["stderr"] != "":
		// The harness keeps one merged stream, so there is no second one
		// to print; what a collapsed transcript row previews as stderr is
		// the head of that same stream (StderrHead, which this projection
		// does not carry).
		t.Fatalf("bash object: stderr %v, want the merged stream in stdout alone", out["stderr"])
	}

	// The control: a row only reaches this stream once it has a status, and
	// every object of the turn is one of the kinds this stream prints.
	for _, o := range objs {
		if o["type"] == "tool" && o["status"] == "" {
			t.Fatalf("a tool object with no status: %v", o)
		}
	}
}

// TestNativeEventsAllRenderAsJSON is plan 019 §3.10's "every event the
// native adapter can emit has a rendering in internal/cli/events.go". The
// turn below provokes each kind the adapter has — text, thinking, tool
// rows, a model switch, a question the headless policy answered itself, the
// todo list, a turn that ended and one that failed — and every one of
// them must project to a line, with the single documented exception below.
// EventQueue is not among them: the queue left the provider seam (plan 021
// §3.5), so the native adapter never emits one any more, and its rendering is
// held by TestQueueJSONCarriesTheEditedVersion and json_test.go instead.
func TestNativeEventsAllRenderAsJSON(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A command of the workspace's own, so the first prompt below expands and
	// the turn carries an EventCommand (plan 022 §3.3). It is written into the
	// workspace rather than the content home because a native session reads
	// both, and the workspace needs no settings file to be read.
	if err := os.MkdirAll(filepath.Join(ws, ".claude", "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".claude", "commands", "expandme.md"),
		[]byte("---\ndescription: a command of this project's\n---\nexpanded body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	model := &nativeJSONModel{steps: [][]fantasy.StreamPart{
		nativeJSONCall("c1", "read", `{"filePath":"main.go"}`),
		// The two H5 tools a headless run reaches (plan 023 §3.4): a question
		// craze answers itself, which prints its Auto opening, and a todo
		// list, which prints as `todos`. exit_plan_mode is not among them —
		// the gate refuses it outside plan mode, and no mode can be entered
		// until PR 2.
		nativeJSONCall("c2", "todo_write", `{"todos":[{"id":"1","content":"first","status":"in_progress"}]}`),
		nativeJSONCall("c3", "ask_user_question",
			`{"questions":[{"question":"Which?","options":[{"label":"alpha","description":"the first"},{"label":"beta"}]}]}`),
		nativeJSONAnswer("thinking", "done"),
		// Nothing queued for the second turn: the model fails, which is
		// the EventError this test needs.
	}}
	sess := nativeJSONSession(t, model, ws)

	if _, err := sess.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if _, err := sess.Prompt(context.Background(), "/expandme"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if _, err := sess.Prompt(context.Background(), "again"); err == nil {
		t.Fatal("the second turn did not fail")
	}
	evs := drainNativeEvents(sess)

	seen := map[agent.EventType]bool{}
	for _, ev := range evs {
		seen[ev.Type] = true
		_, ok := eventJSON(ev)
		switch {
		case ok:
		case ev.Type == agent.EventMeta && ev.Text == "":
			// The one event this file drops on purpose: an EventMeta with no
			// Text. The native session's answer to a model or effort switch is
			// one — it carries the model and config sections as a state delta
			// (plan 021 §3.8) and has nothing a headless caller could print,
			// because Event.Text is filled only by an agent naming the session
			// (eventJSON's EventMeta case). Any other bare event is a missing
			// rendering.
		case ev.Type == agent.EventTurn, ev.Type == agent.EventAsk:
			// The engine's turn events and the registry's ask endings fall to
			// eventJSON's default on purpose: `--json` gains no line kind in
			// S1b (plan 021 §3.9). Whether an ask ending should ever be printed
			// is a `--json` contract addition, and it is decided when H3 first
			// makes the native adapter emit one.
		default:
			t.Fatalf("no rendering for the %s event the adapter emitted: %+v", ev.Type, ev)
		}
	}
	// Without this the test would pass on a turn that emitted nothing.
	for _, want := range []agent.EventType{
		agent.EventText, agent.EventThought, agent.EventTool, agent.EventCommand,
		agent.EventQuestion, agent.EventTodos, agent.EventAsk,
		agent.EventMeta, agent.EventDone, agent.EventError,
	} {
		if !seen[want] {
			t.Fatalf("the session never emitted a %s event, so nothing checked its rendering", want)
		}
	}
}

// TestPromptJSONNativeSubagent (A11, plan 026 §3.12): a native sub-agent as
// `craze prompt --json` prints it, with no change to this file's projection:
// subagent lines for its spawn, its progress and its finish, carrying the row
// whole — its id, the parent's call it belongs to, its type, model, task and
// answer — and the child's own lines tagged with its id: its task as a user
// line, its tool rows and its answer. The parent's agent call is a task-kind
// tool line whose task names the child and its model once it has finished.
// Every event the session emitted has a line, and none of them holds the
// session's key, which the call's description carries whole and its prompt
// split by a zero-width space — which no redactor sees, and which the
// sanitizer would join back into the key (plan 026 §3.9, panel P38).
func TestPromptJSONNativeSubagent(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The key nativeJSONSession's environment holds: redacted wherever the
	// adapter publishes the call's own text.
	const key = "test-key"
	split := key[:4] + string(rune(0x200b)) + key[4:]
	args := fmt.Sprintf(`{"description":"scan with %s","prompt":"Read main.go with %s and say what it is."}`, key, split)
	model := &nativeJSONModel{steps: [][]fantasy.StreamPart{
		nativeJSONCall("a1", "agent", args),
		// The child's two requests: one model serves parent and child, one
		// request at a time, so the steps are taken in the order sent.
		nativeJSONCall("r1", "read", `{"filePath":"main.go"}`),
		nativeJSONAnswer("looking", "it is package main"),
		nativeJSONAnswer("done", "the child says it is package main"),
	}}
	sess := nativeJSONSession(t, model, ws)
	if _, err := sess.Prompt(context.Background(), "delegate it"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	evs := drainNativeEvents(sess)
	for _, ev := range evs {
		_, ok := eventJSON(ev)
		switch {
		case ok:
		case ev.Type == agent.EventMeta && ev.Text == "", ev.Type == agent.EventTurn, ev.Type == agent.EventAsk:
			// TestNativeEventsAllRenderAsJSON's documented exceptions.
		default:
			t.Fatalf("no rendering for the %s event the adapter emitted: %+v", ev.Type, ev)
		}
	}
	objs := encodeNativeEvents(t, evs)
	var buf bytes.Buffer
	for _, o := range objs {
		fmt.Fprintln(&buf, o)
	}
	if strings.Contains(buf.String(), key) {
		t.Fatalf("the JSON stream holds the key:\n%s", buf.String())
	}

	var subs []map[string]any
	for _, o := range objs {
		if o["type"] == "subagent" {
			subs = append(subs, o)
		}
	}
	if len(subs) < 3 || subs[0]["event"] != "spawned" || subs[len(subs)-1]["event"] != "finished" {
		t.Fatalf("the subagent lines are %v; want spawned first, finished last", subs)
	}
	id, _ := subs[0]["id"].(string)
	fin := subs[len(subs)-1]
	switch {
	case id == "" || fin["id"] != id:
		t.Fatalf("the subagent lines name %q and %v", id, fin["id"])
	case subs[0]["status"] != "running" || subs[0]["toolCallId"] != "t1.1.1" || subs[0]["subagentType"] != "general-purpose" ||
		subs[0]["model"] != "test/a" || subs[0]["transcript"] != true:
		t.Fatalf("the spawned line is %v", subs[0])
	case !strings.Contains(fmt.Sprint(subs[0]["description"]), "[craze:redacted-credential]"):
		t.Fatalf("the spawned line's description is %v; want the key redacted", subs[0]["description"])
	case fin["status"] != "completed" || fin["output"] != "it is package main" || fin["toolCalls"] != float64(1) ||
		fin["turns"] != float64(2):
		t.Fatalf("the finished line is %v", fin)
	}
	for _, s := range subs[1 : len(subs)-1] {
		if s["event"] != "progress" || s["id"] != id {
			t.Fatalf("a subagent line between spawned and finished is %v", s)
		}
	}

	var childKinds []string
	for _, o := range objs {
		if o["agent"] != id {
			continue
		}
		childKinds = append(childKinds, fmt.Sprint(o["type"]))
		switch o["type"] {
		case "user":
			if !strings.Contains(fmt.Sprint(o["text"]), "Read main.go with [craze:redacted-credential]") {
				t.Fatalf("the child's user line is %v; want its task, the key redacted", o)
			}
		case "tool":
			if o["name"] != "read" {
				t.Fatalf("a child tool line is %v", o)
			}
		}
	}
	for _, want := range []string{"user", "tool", "thought", "text"} {
		if !slices.Contains(childKinds, want) {
			t.Fatalf("the child's lines are %v; want a %s line among them", childKinds, want)
		}
	}
	if joined := joinedText(objs, id); joined != "it is package main" {
		t.Fatalf("the child's text lines say %q", joined)
	}
	if joined := joinedText(objs, ""); joined != "the child says it is package main" {
		t.Fatalf("the parent's text lines say %q", joined)
	}

	var call map[string]any
	for _, o := range objs {
		if o["type"] == "tool" && o["agent"] == nil && o["id"] == "t1.1.1" {
			call = o
		}
	}
	task, _ := call["task"].(map[string]any)
	if call["kind"] != "task" || call["status"] != "completed" || task == nil || task["agentId"] != id ||
		task["model"] != "test/a" || task["status"] != "completed" {
		t.Fatalf("the agent call's last line is %v", call)
	}
}

// joinedText is the text lines of agent's (the parent's for ""), joined.
func joinedText(objs []map[string]any, agentID string) string {
	var b strings.Builder
	for _, o := range objs {
		a, _ := o["agent"].(string)
		if o["type"] == "text" && a == agentID {
			fmt.Fprint(&b, o["text"])
		}
	}
	return b.String()
}
