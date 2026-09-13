package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	fakeOnce sync.Once
	fakeBin  string
	fakeErr  error
)

func fakeAgentPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("CRAZE_FAKE_AGENT_BIN"); p != "" {
		return p
	}
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "craze-fake-agent-")
		if err != nil {
			fakeErr = err
			return
		}
		fakeBin = filepath.Join(dir, "craze-fake-agent")
		out, err := exec.Command("go", "build", "-o", fakeBin, "github.com/charliek/craze/cmd/craze-fake-agent").CombinedOutput()
		if err != nil {
			fakeErr = err
			t.Log(string(out))
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return fakeBin
}

type jsonLineEvent struct {
	raw string
	m   map[string]any
}

// runPromptJSON runs one headless turn against a fake script and returns each
// NDJSON line with its decoded form, so assertions can use the bytes craze
// actually wrote.
func isolateProviderConfig(t *testing.T) {
	t.Helper()
	t.Setenv("CRAZE_PROVIDER", "")
	t.Setenv("CRAZE_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
}

func runPromptJSON(t *testing.T, script string) []jsonLineEvent {
	t.Helper()
	return runPromptJSONArgs(t, script, nil, "go")
}

func runPromptJSONArgs(t *testing.T, script string, extra []string, text string) []jsonLineEvent {
	t.Helper()
	isolateProviderConfig(t)
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	t.Setenv("CRAZE_FAKE_SCRIPT", script)
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	args := append([]string{"prompt", "--json", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir()}, extra...)
	args = append(args, text)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt: %v\nstderr: %s", err, stderr.String())
	}
	return parseJSONLines(t, stdout.String())
}

func parseJSONLines(t *testing.T, stdout string) []jsonLineEvent {
	t.Helper()
	var out []jsonLineEvent
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, jsonLineEvent{raw: line, m: m})
	}
	return out
}

// pick returns the last event of the given type, optionally filtered.
func pick(t *testing.T, evs []jsonLineEvent, typ string, match func(map[string]any) bool) jsonLineEvent {
	t.Helper()
	var got jsonLineEvent
	for _, ev := range evs {
		if ev.m["type"] != typ {
			continue
		}
		if match != nil && !match(ev.m) {
			continue
		}
		got = ev
	}
	if got.raw == "" {
		var all []string
		for _, ev := range evs {
			all = append(all, ev.raw)
		}
		t.Fatalf("no %s event in:\n%s", typ, strings.Join(all, "\n"))
	}
	return got
}

func wantLine(t *testing.T, got jsonLineEvent, want string) {
	t.Helper()
	if got.raw != want {
		t.Fatalf("line\n got %s\nwant %s", got.raw, want)
	}
}

func wantContains(t *testing.T, got jsonLineEvent, want string) {
	t.Helper()
	if !strings.Contains(got.raw, want) {
		t.Fatalf("line\n got %s\nwant it to contain %s", got.raw, want)
	}
}

func TestPromptJSONTodos(t *testing.T) {
	evs := runPromptJSON(t, "todos")
	got := pick(t, evs, "todos", nil)
	wantLine(t, got, `{"type":"todos","todos":[`+
		`{"id":"1","content":"Read main.go","status":"completed"},`+
		`{"id":"2","content":"Edit main.go","status":"in_progress"},`+
		`{"id":"3","content":"Run go vet","status":"pending"}]}`)
}

func TestPromptJSONDiff(t *testing.T) {
	evs := runPromptJSON(t, "diff")
	edit := pick(t, evs, "tool", func(ev map[string]any) bool { return ev["kind"] == "edit" && ev["diffs"] != nil })
	wantContains(t, edit, `"diffs":[{"path":"/tmp/ws/main.go","added":1,"removed":1,"truncated":false}]`)
	read := pick(t, evs, "tool", func(ev map[string]any) bool { return ev["kind"] == "read" && ev["output"] != nil })
	out, _ := read.m["output"].(map[string]any)
	if !strings.Contains(out["content"].(string), "hello") {
		t.Fatalf("read output %v", out)
	}
	// Ids are emitted verbatim, JSON-escaped.
	if !strings.Contains(read.raw, `\nfc_read`) {
		t.Fatalf("id lost its escaped newline: %s", read.raw)
	}
	if !strings.Contains(read.m["id"].(string), "\n") {
		t.Fatalf("id %q lost its newline", read.m["id"])
	}
}

func TestPromptJSONBash(t *testing.T) {
	evs := runPromptJSON(t, "bash")
	tool := pick(t, evs, "tool", func(ev map[string]any) bool { return ev["output"] != nil })
	wantContains(t, tool, `"output":{"exitCode":127,"stdout":"",`+
		`"stderr":"Command 'go' not found, but can be installed with:\nsudo apt install golang-go\n"}`)
	if tool.m["status"] != "completed" || tool.m["kind"] != "execute" {
		t.Fatalf("tool %s", tool.raw)
	}
}

func TestPromptJSONTask(t *testing.T) {
	evs := runPromptJSON(t, "task")
	tool := pick(t, evs, "tool", func(ev map[string]any) bool { return ev["task"] != nil && ev["status"] == "completed" })
	wantContains(t, tool, `"task":{"description":"Count main.go lines",`+
		`"model":"cursor-grok-4.6-high-fast","agentId":"agent-1234","durationMs":8010,"status":"completed"}`)
	assertSubagentLifecycle(t, evs, "")
}

func TestPromptJSONTaskLate(t *testing.T) {
	evs := runPromptJSON(t, "task-late")
	assertSubagentLifecycle(t, evs, "")
}

func assertSubagentLifecycle(t *testing.T, evs []jsonLineEvent, id string) {
	t.Helper()
	var events []string
	for _, ev := range evs {
		if ev.m["type"] != "subagent" {
			continue
		}
		if id != "" && ev.m["id"] != id {
			continue
		}
		e, _ := ev.m["event"].(string)
		events = append(events, e)
	}
	i := 0
	want := []string{"spawned", "progress", "finished"}
	for _, e := range events {
		if i < len(want) && e == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("subagent lifecycle %v", events)
	}
}

func TestPromptJSONTitle(t *testing.T) {
	evs := runPromptJSON(t, "title")
	wantLine(t, pick(t, evs, "title", nil), `{"type":"title","title":"Fake Title"}`)
}

func TestPromptJSONGrokAsk(t *testing.T) {
	isolateProviderConfig(t)
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	t.Setenv("CRAZE_FAKE_SCRIPT", "grok-ask")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"prompt", "--json", "--provider", "grok", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir(), "q"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt: %v\nstderr: %s", err, stderr.String())
	}
	var evs []jsonLineEvent
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		evs = append(evs, jsonLineEvent{raw: line, m: m})
	}
	q := pick(t, evs, "question", nil)
	if q.m["auto"] != true {
		t.Fatalf("question %s", q.raw)
	}
	answers, _ := q.m["answers"].(map[string]any)
	if fmtSlice(answers["Pick one"]) != "A" {
		t.Fatalf("answers %s", q.raw)
	}
	text := ""
	for _, ev := range evs {
		if ev.m["type"] == "text" {
			text += ev.m["text"].(string)
		}
	}
	if text != "asked:accepted:Pick one=A;Pick any=X" {
		t.Fatalf("text %q", text)
	}
}

func fmtSlice(v any) string {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	s, _ := arr[0].(string)
	return s
}

func TestPromptJSONAutoAnswers(t *testing.T) {
	t.Run("question", func(t *testing.T) {
		evs := runPromptJSON(t, "ask")
		wantLine(t, pick(t, evs, "question", nil),
			`{"type":"question","id":"ask-1","title":"Question","auto":true,"answers":{"q1":["opt-a"],"q2":["opt-x"]}}`)
	})
	t.Run("plan", func(t *testing.T) {
		evs := runPromptJSON(t, "plan")
		wantLine(t, pick(t, evs, "plan", nil),
			`{"type":"plan","name":"Fake Plan","id":"plan-1","auto":true,"accepted":true}`)
	})
}

func TestPromptJSONGrokSubagent(t *testing.T) {
	evs := runPromptJSONArgs(t, "grok-subagent", []string{"--provider", "grok"}, "go")
	assertSubagentLifecycle(t, evs, "sub-1")
	user := pick(t, evs, "user", func(m map[string]any) bool { return m["agent"] == "sub-1" })
	if user.m["text"] == "" {
		t.Fatalf("user %s", user.raw)
	}
	childText := pick(t, evs, "text", func(m map[string]any) bool { return m["agent"] == "sub-1" })
	if childText.m["text"] == "" {
		t.Fatalf("child text %s", childText.raw)
	}
	for _, ev := range evs {
		if ev.m["type"] == "text" && ev.m["agent"] == nil && strings.Contains(fmtString(ev.m["text"]), "DONE") {
			return
		}
	}
	t.Fatal("missing parent DONE text")
}

func TestPromptJSONGrokSubagentLate(t *testing.T) {
	evs := runPromptJSONArgs(t, "grok-subagent-late", []string{"--provider", "grok"}, "go")
	if pick(t, evs, "done", nil).m["stopReason"] != "end_turn" {
		t.Fatal("missing done")
	}
	fin := pick(t, evs, "subagent", func(m map[string]any) bool { return m["event"] == "finished" })
	if fin.m["id"] != "sub-1" {
		t.Fatalf("finished %s", fin.raw)
	}
}

func TestPromptJSONGrokSubagentFollowUp(t *testing.T) {
	evs := runPromptJSONArgs(t, "grok-subagent-late", []string{"--provider", "grok", "--follow-up", "two"}, "one")
	var dones int
	for _, ev := range evs {
		if ev.m["type"] == "done" {
			dones++
		}
	}
	if dones != 2 {
		t.Fatalf("want two done, got %d", dones)
	}
	pick(t, evs, "subagent", func(m map[string]any) bool { return m["event"] == "finished" && m["id"] == "sub-1" })
}

func TestPromptJSONGrokSubagentCancel(t *testing.T) {
	isolateProviderConfig(t)
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	t.Setenv("CRAZE_FAKE_SCRIPT", "grok-subagent-cancel")
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"prompt", "--json", "--provider", "grok", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir(), "go"})
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()
	err := cmd.ExecuteContext(ctx)
	var ee *exitError
	if err == nil || !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("want exit 1, got %v", err)
	}
	evs := parseJSONLines(t, stdout.String())
	fin := pick(t, evs, "subagent", func(m map[string]any) bool { return m["event"] == "finished" })
	if fin.m["status"] != "cancelled" {
		t.Fatalf("finished %s", fin.raw)
	}
}

func TestPromptPlainExcludesChildText(t *testing.T) {
	isolateProviderConfig(t)
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	t.Setenv("CRAZE_FAKE_SCRIPT", "grok-subagent")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"prompt", "--provider", "grok", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir(), "go"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt: %v\nstderr: %s", err, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "DONE: main.py README.md") {
		t.Fatalf("missing parent text %q", got)
	}
	if strings.Contains(got, "Listing files.") {
		t.Fatalf("child thought leaked: %q", got)
	}
}

func fmtString(v any) string {
	s, _ := v.(string)
	return s
}
