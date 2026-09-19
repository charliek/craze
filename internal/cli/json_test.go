package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

var (
	fakeOnce sync.Once
	fakeBin  string
	fakeErr  error
	// buildEnv is the environment before any test rewrites HOME: the one
	// fake-agent build runs in it, so it keeps the developer's warm build
	// cache and never writes a Go cache into whichever test's temp HOME
	// happened to trigger it (which a test asserting that HOME stays empty
	// would then see, but only when run on its own).
	buildEnv = os.Environ()
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
		build := exec.Command("go", "build", "-o", fakeBin, "github.com/charliek/craze/cmd/craze-fake-agent")
		build.Env = buildEnv
		out, err := build.CombinedOutput()
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

// typeMemberOnly matches a line's head when nothing but `type` precedes the
// key that follows it.
var typeMemberOnly = regexp.MustCompile(`^\{"type":"[^"]*"$`)

// isolateHome points HOME at an empty directory. The plugin scan walks the
// caches under it at session start, so a run that skipped this would find
// whatever the developer happens to have installed. It is separate from
// isolateRunEnv because a test that wants its own provider or config still
// wants this.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// isolateRunEnv is everything a headless run reads out of the environment
// before it reaches the agent: HOME, the provider override, the journal
// switch and the craze directory, whose config file does not exist yet. The
// journal switch is cleared (empty reads as unset) so a developer's exported
// CRAZE_JOURNAL can neither turn a run's journal off nor print a diagnostic
// into a run whose stderr a case asserts on.
func isolateRunEnv(t *testing.T) {
	t.Helper()
	isolateHome(t)
	t.Setenv("CRAZE_PROVIDER", "")
	t.Setenv("CRAZE_JOURNAL", "")
	crazeHome(t)
}

// crazeHome points CRAZE_HOME at a fresh, empty directory, so the config file
// and the session index a case reads and writes are its own and a developer's
// exported CRAZE_HOME is never touched. Returns the directory.
func crazeHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CRAZE_HOME", dir)
	return dir
}

// writeCrazeConfig points CRAZE_HOME at a fresh directory holding a
// config.toml with body in it. Returns the config file's path.
func writeCrazeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(crazeHome(t), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runPromptJSON runs one headless turn against a fake script and returns each
// NDJSON line with its decoded form, so assertions can use the bytes craze
// actually wrote.
func runPromptJSON(t *testing.T, script string) []jsonLineEvent {
	t.Helper()
	return runPromptJSONArgs(t, script, nil, "go")
}

func runPromptJSONArgs(t *testing.T, script string, extra []string, text string) []jsonLineEvent {
	t.Helper()
	return parseJSONLines(t, runPromptStdout(t, script, append([]string{"--json"}, extra...), text))
}

// runPromptStdout is one headless turn's stdout, whatever mode it ran in. The
// plain-mode tests take it raw; the JSON ones parse it.
func runPromptStdout(t *testing.T, script string, extra []string, text string) string {
	t.Helper()
	isolateRunEnv(t)
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	t.Setenv("CRAZE_FAKE_SCRIPT", script)
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	args := append([]string{"prompt", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir()}, extra...)
	args = append(args, text)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt: %v\nstderr: %s", err, stderr.String())
	}
	return stdout.String()
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

// splitSeq takes the `seq` member off a raw line and returns it with the bytes
// that surround it, untouched. The seq a run's lines carry depends on how many
// events the session published before them — dropped kinds take numbers too
// (plan 020 §3.6) — so an end-to-end case cannot spell one; the rest of the
// line still can, byte for byte. A line with no `seq` returns 0 and itself.
func splitSeq(t *testing.T, raw string) (uint64, string) {
	t.Helper()
	const key = `,"seq":`
	i := strings.Index(raw, key)
	if i < 0 {
		return 0, raw
	}
	// `seq` is the second key: everything before it is the `type` member.
	if !typeMemberOnly.MatchString(raw[:i]) {
		t.Fatalf("line %s: seq must be the second key, right after type", raw)
	}
	j := i + len(key)
	end := j
	for end < len(raw) && raw[end] >= '0' && raw[end] <= '9' {
		end++
	}
	seq, err := strconv.ParseUint(raw[j:end], 10, 64)
	if err != nil {
		t.Fatalf("line %s: seq %q is not a number: %v", raw, raw[j:end], err)
	}
	return seq, raw[:i] + raw[end:]
}

// sessionLine is one line of a run with its `seq` taken off, so an assertion
// on the rest stays exact. The line must carry one: it came from the session's
// event stream, and every such line is numbered (plan 020 A21). craze's own
// lines — the signal window's queue removal, a startup error — are the ones
// with no `seq`, and they are asserted where they are written.
func sessionLine(t *testing.T, got jsonLineEvent) string {
	t.Helper()
	seq, rest := splitSeq(t, got.raw)
	if seq == 0 {
		t.Fatalf("line %s: a line from the session's stream must carry a seq", got.raw)
	}
	return rest
}

func wantLine(t *testing.T, got jsonLineEvent, want string) {
	t.Helper()
	if rest := sessionLine(t, got); rest != want {
		t.Fatalf("line\n got %s\nwant %s", rest, want)
	}
}

func wantContains(t *testing.T, got jsonLineEvent, want string) {
	t.Helper()
	if !strings.Contains(got.raw, want) {
		t.Fatalf("line\n got %s\nwant it to contain %s", got.raw, want)
	}
}

// TestJSONSeqIsTheSecondKeyOfEveryLine: one case per branch of eventJSON, each
// encoded twice — once as the session's log numbered it, once as an event that
// never passed through a log. A line shape that forgot the field, or declared
// it anywhere but right after Type, would print a line no reader can place in
// the sequence (plan 020 §3.6, A21); the pair also pins that `seq` is the only
// difference between the two.
func TestJSONSeqIsTheSecondKeyOfEveryLine(t *testing.T) {
	const seq = 9
	events := []agent.Event{
		{Type: agent.EventText, Text: "hi"},
		{Type: agent.EventThought, Text: "hmm"},
		{Type: agent.EventUser, Agent: "sub-1", Text: "go"},
		{Type: agent.EventCommand, Command: &agent.ExpandedCommand{
			PluginCommand: agent.PluginCommand{Plugin: "p", Bare: "c", Qualified: "p:c", Kind: "command"},
			Path:          "/p/commands/c.md",
			Text:          "block",
		}},
		{Type: agent.EventQueue, QueueChange: agent.QueueQueued, Queue: &agent.QueuedPrompt{ID: "q-1", Text: "later"}},
		{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "interject-fallback-1", Running: true}},
		{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}},
		{Type: agent.EventSubagent, SubagentChange: agent.SubagentChangeSpawned, Subagent: &agent.SubagentInfo{ID: "sub-1"}},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "call-1", Name: "Shell", Status: "pending"}},
		{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "1", Content: "read", Status: "pending"}}},
		{Type: agent.EventPermission, Permission: &agent.PermissionEvent{Tool: "Shell"}},
		{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "ask-1", Title: "Question"}},
		{Type: agent.EventPlan, Plan: &agent.PlanEvent{ID: "plan-1", Name: "Fake Plan"}},
		{Type: agent.EventMeta, Text: "Fake Title"},
		{Type: agent.EventDone, StopReason: "end_turn"},
		{Type: agent.EventError, Err: errors.New("boom")},
	}
	var kinds []string
	for _, ev := range events {
		numbered := ev
		numbered.Seq = seq
		line := encodeOne(t, numbered)
		kind, _ := parseJSONLines(t, line+"\n")[0].m["type"].(string)
		kinds = append(kinds, kind)
		head := `{"type":"` + kind + `","seq":` + strconv.Itoa(seq)
		if !strings.HasPrefix(line, head) || (line != head+"}" && !strings.HasPrefix(line, head+",")) {
			t.Fatalf("%s line\n got %s\nwant it to start %s", ev.Type, line, head)
		}
		// An event no log numbered is the same line without the key.
		unnumbered := encodeOne(t, ev)
		if strings.Contains(unnumbered, `"seq"`) {
			t.Fatalf("%s: an event with no sequence number must print no seq: %s", ev.Type, unnumbered)
		}
		if _, rest := splitSeq(t, line); rest != unnumbered {
			t.Fatalf("%s: seq is not the only difference\n got %s\nwant %s", ev.Type, rest, unnumbered)
		}
	}
	want := []string{
		"text", "thought", "user", "command", "queue", "foreign_turn", "replay",
		"subagent", "tool", "todos", "permission", "question", "plan", "title",
		"done", "error",
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("line kinds\n got %v\nwant %v", kinds, want)
	}
}

// TestJSONSeqExactLines is the bytes themselves, which an end-to-end run
// cannot spell: the number goes between `type` and everything else, and
// craze's own lines — a startup error, the queue row a signal took — carry no
// `seq` at all.
func TestJSONSeqExactLines(t *testing.T) {
	row := agent.QueuedPrompt{ID: "q-1", Text: "follow-up"}
	for _, tc := range []struct {
		name string
		ev   agent.Event
		want string
	}{
		{"numbered text", agent.Event{Type: agent.EventText, Text: "hi", Seq: 7},
			`{"type":"text","seq":7,"text":"hi"}`},
		{"numbered done", agent.Event{Type: agent.EventDone, StopReason: "end_turn", Seq: 1234},
			`{"type":"done","seq":1234,"stopReason":"end_turn"}`},
		{"a startup error craze wrote itself", agent.Event{Type: agent.EventError, Err: errors.New("boom")},
			`{"type":"error","message":"boom"}`},
		{"the queue row a signal took", agent.Event{Type: agent.EventQueue, Queue: &row, QueueChange: agent.QueueRemoved},
			`{"type":"queue","event":"removed","id":"q-1","position":0,"version":0,"text":"follow-up"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := encodeOne(t, tc.ev); got != tc.want {
				t.Fatalf("line\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// encodeOne is the one line ev projects to, without its newline.
func encodeOne(t *testing.T, ev agent.Event) string {
	t.Helper()
	var buf bytes.Buffer
	if err := encodeEvent(&buf, ev); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("%s produced no line: %q", ev.Type, line)
	}
	return strings.TrimSuffix(line, "\n")
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
	isolateRunEnv(t)
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
	isolateRunEnv(t)
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
	isolateRunEnv(t)
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

// pluginFixture lays out a plugin under a temp directory and returns its root,
// so a headless run can be given a --plugin-dir that holds exactly what the
// test is about.
func pluginFixture(t *testing.T, plugin string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), plugin)
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestPromptJSONPluginCommand is the command line a headless caller reads: the
// bare name and the qualified one side by side — two plugins ship gauntlet
// here, so the row is only reachable qualified — and the block as it was sent.
func TestPromptJSONPluginCommand(t *testing.T) {
	forge := pluginFixture(t, "forge", map[string]string{
		"commands/gauntlet.md": "---\ndescription: the forge one\n---\nRun the gauntlet on $ARGUMENTS.\n",
	})
	flows := pluginFixture(t, "flows", map[string]string{
		"commands/gauntlet.md": "---\ndescription: the flows one\n---\nSomething else.\n",
	})
	evs := runPromptJSONArgs(t, "echo", []string{"--plugin-dir", forge, "--plugin-dir", flows}, "/forge:gauntlet craze")
	got := pick(t, evs, "command", nil)
	prefix := `{"type":"command","name":"gauntlet","qualified":"forge:gauntlet","plugin":"forge","kind":"command","path":"` +
		filepath.Join(forge, "commands", "gauntlet.md") + `","text":"The user invoked /forge:gauntlet craze `
	if rest := sessionLine(t, got); !strings.HasPrefix(rest, prefix) {
		t.Fatalf("line\n got %s\nwant it to start %s", rest, prefix)
	}
	wantContains(t, got, `Run the gauntlet on craze.`)
	// The flows entry was never invoked, so it has no line.
	for _, ev := range evs {
		if ev.m["type"] == "command" && ev.m["plugin"] != "forge" {
			t.Fatalf("unexpected command line %s", ev.raw)
		}
	}
	// The echo is the draft, then a newline, then the block that went with it.
	var text strings.Builder
	for _, ev := range evs {
		if ev.m["type"] == "text" {
			text.WriteString(fmtString(ev.m["text"]))
		}
	}
	if !strings.HasPrefix(text.String(), "echo: /forge:gauntlet craze\nThe user invoked ") {
		t.Fatalf("echo %q", text.String())
	}
}

// TestPromptJSONUnknownSlashHasNoCommandLine: a name craze did not resolve is
// the agent's own, and nothing about it is craze's to report.
func TestPromptJSONUnknownSlashHasNoCommandLine(t *testing.T) {
	dir := pluginFixture(t, "probe-plugin", map[string]string{
		"commands/probe-echo.md": "---\ndescription: probe\n---\nSay PROBE.\n",
	})
	evs := runPromptJSONArgs(t, "echo", []string{"--plugin-dir", dir}, "/nope banana")
	for _, ev := range evs {
		if ev.m["type"] == "command" {
			t.Fatalf("unknown name reported %s", ev.raw)
		}
	}
	got := pick(t, evs, "text", func(m map[string]any) bool { return strings.Contains(fmtString(m["text"]), "nope") })
	if fmtString(got.m["text"]) != "/nope banana" {
		t.Fatalf("echo %s", got.raw)
	}
}

// TestPromptPlainSaysNothingAboutCommands: plain mode is the agent's text and
// nothing else, expansion included.
func TestPromptPlainSaysNothingAboutCommands(t *testing.T) {
	dir := pluginFixture(t, "probe-plugin", map[string]string{
		"commands/probe-echo.md": "---\ndescription: probe\n---\nSay PROBE-COMMAND-EXPANDED args=[$ARGUMENTS].\n",
	})
	got := runPromptStdout(t, "echo", []string{"--plugin-dir", dir}, "/probe-plugin:probe-echo banana")
	if strings.Contains(got, `"type":"command"`) {
		t.Fatalf("plain mode wrote an event line: %q", got)
	}
	// The expansion still happened; it is only craze's own line that is absent.
	if !strings.HasPrefix(got, "echo: /probe-plugin:probe-echo banana\nThe user invoked ") {
		t.Fatalf("plain output %q", got)
	}
	if !strings.Contains(got, "PROBE-COMMAND-EXPANDED args=[banana]") {
		t.Fatalf("plain output %q", got)
	}
}

// TestPromptJSONHasNoMainUserOrReplayLines is the blast radius of plan 013's
// replay work on the headless stream. grok and gx echo the user's own prompt
// back as a main-session user_message_chunk; craze still drops it, so a `user`
// line without an agent — a doubled user block in the TUI, and a line
// `prompt --json` never wrote — cannot appear. And `craze prompt` never sets a
// load id, so the `replay` bracket events.go now knows how to encode is never
// emitted by a headless run.
func TestPromptJSONHasNoMainUserOrReplayLines(t *testing.T) {
	for _, script := range []string{"grok-echo", "grok-subagent"} {
		t.Run(script, func(t *testing.T) {
			evs := runPromptJSONArgs(t, script, []string{"--provider", "grok"}, "go")
			if len(evs) == 0 {
				t.Fatal("no output")
			}
			for _, ev := range evs {
				if ev.m["type"] == "replay" {
					t.Fatalf("prompt emitted a replay line: %s", ev.raw)
				}
				if ev.m["type"] != "user" {
					continue
				}
				// A child's prompt is a user line by design; the main
				// session's own echo is not.
				if agent, _ := ev.m["agent"].(string); agent == "" {
					t.Fatalf("main-session user line: %s", ev.raw)
				}
			}
		})
	}
}
