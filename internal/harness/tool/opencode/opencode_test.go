package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

var updateGolden = flag.Bool("update", false, "rewrite internal/harness/tool/opencode/testdata/specs.golden")

const specsGolden = "testdata/specs.golden"

// keyA is the only credential any test here holds: obviously not a secret,
// and over modeltable's 8-byte floor.
const keyA = "sk-canary-alpha-0001"

// fixture is a session's worth of the opencode profile: a workspace, a
// harness home, and a dispatcher over the profile's tools, which every test
// calls through, as the runner will.
type fixture struct {
	env tool.Env
	d   *tool.Dispatcher
	n   int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, redact.New(keyA))
}

// newFixtureWith is newFixture with red as the session's redactor; a nil
// one redacts nothing.
func newFixtureWith(t *testing.T, red *redact.Replacer) *fixture {
	t.Helper()
	env := tool.Env{
		Workspace: t.TempDir(),
		Home:      t.TempDir(),
		Redactor:  red,
		Environ:   tool.ChildEnviron(os.Environ(), nil),
		Locks:     &tool.PathLocks{},
	}
	p, err := Profile()
	if err != nil {
		t.Fatal(err)
	}
	d, err := tool.NewDispatcher(tool.Options{Tools: p.Tools, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{env: env, d: d}
}

// path is name in the workspace.
func (f *fixture) path(name string) string { return filepath.Join(f.env.Workspace, name) }

// call makes one call of the named tool: Prepare, then Run. in is the
// arguments, marshalled, or a string of raw JSON.
func (f *fixture) call(t *testing.T, name string, in any) (tool.Request, tool.Result) {
	t.Helper()
	return f.callCtx(t, context.Background(), name, in)
}

func (f *fixture) callCtx(t *testing.T, ctx context.Context, name string, in any) (tool.Request, tool.Result) {
	t.Helper()
	raw, ok := in.(string)
	if !ok {
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(b)
	}
	f.n++
	id := fmt.Sprintf("t1.1.%d", f.n)
	req, res, ok := f.d.Prepare(tool.Call{ID: id, CallID: "call_1", Tool: name, Input: json.RawMessage(raw)})
	if !ok {
		f.d.Discard(id)
		return req, res
	}
	return req, f.d.Run(ctx, id, nil)
}

// ok fails the test unless res succeeded, and returns its text.
func ok(t *testing.T, res tool.Result) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("result is an error (%s): %s", res.Class, res.Text)
	}
	return res.Text
}

// failed fails the test unless res is an error of class with exactly text.
func failed(t *testing.T, res tool.Result, class tool.ErrorClass, text string) {
	t.Helper()
	if !res.IsError || res.Class != class || res.Text != text {
		t.Fatalf("result = {IsError:%v Class:%q Text:%q}\nwant an error {Class:%q Text:%q}", res.IsError, res.Class, res.Text, class, text)
	}
}

func put(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func names(p tool.Profile) []string {
	var out []string
	for _, tl := range p.Tools {
		out = append(out, tl.Spec().ID)
	}
	return out
}

// TestProfile: the tools come in opencode's registry order, each Spec
// passes the registry's rules, and the profile registers as it is — with its
// system prompt; the negative control, the same profile without one, is
// refused.
func TestProfile(t *testing.T) {
	p, err := Profile()
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != Name || Name != "opencode" {
		t.Fatalf("profile name = %q", p.Name)
	}
	if got := names(p); !slices.Equal(got, []string{"bash", "read", "glob", "grep", "edit", "write", "agent", "todo_write", "ask_user_question", "exit_plan_mode"}) {
		t.Fatalf("tools = %q, want opencode's registry order", got)
	}
	want := map[string]struct {
		kind               tool.Kind
		readOnly, parallel bool
		trunc              tool.Direction
	}{
		"bash":  {tool.KindExecute, false, false, tool.None},
		"read":  {tool.KindRead, true, true, tool.None},
		"glob":  {tool.KindSearch, true, true, tool.None},
		"grep":  {tool.KindSearch, true, true, tool.None},
		"edit":  {tool.KindEdit, false, false, tool.Head},
		"write": {tool.KindEdit, false, false, tool.Head},
		// plan 026 §3.3: a child may edit, so not ReadOnly; fanned out, so
		// Parallel; the runner cuts its answer itself, so None (review r6).
		"agent": {tool.KindTask, false, true, tool.None},
		// plan 023 §3.4's table: the two that block on a person are not
		// Parallel, and all three change nothing.
		"todo_write":        {tool.KindTodo, true, true, tool.Head},
		"ask_user_question": {tool.KindAsk, true, false, tool.Head},
		"exit_plan_mode":    {tool.KindAsk, true, false, tool.Head},
	}
	for _, tl := range p.Tools {
		s, w := tl.Spec(), want[tl.Spec().ID]
		if s.Kind != w.kind || s.ReadOnly != w.readOnly || s.Parallel != w.parallel || s.Truncate != w.trunc {
			t.Errorf("%s: kind %q readOnly %v parallel %v truncate %d, want %+v", s.ID, s.Kind, s.ReadOnly, s.Parallel, s.Truncate, w)
		}
		if s.Required == nil {
			t.Errorf("%s: Required is nil, which marshals as null", s.ID)
		}
	}

	var r tool.Registry
	bare := p
	bare.System = nil
	if err := r.Register(bare); err == nil || !strings.Contains(err.Error(), "no System func") {
		t.Fatalf("Register without a system prompt = %v, want it refused for that reason", err)
	}
	if err := r.Register(p); err != nil {
		t.Fatalf("the profile as Profile returns it does not register: %v", err)
	}
	// Each call builds its own tools, so two sessions share none; within a
	// session, grep and glob share the one ripgrep that finds rg.
	q, _ := Profile()
	if q.Tools[0] == p.Tools[0] {
		t.Fatal("two profiles share a tool")
	}
	if p.Tools[2].(*globTool).rg != p.Tools[3].(*grepTool).rg {
		t.Fatal("a session's grep and glob do not share their ripgrep")
	}
	if p.Tools[2].(*globTool).rg == q.Tools[2].(*globTool).rg {
		t.Fatal("control: two sessions share a ripgrep")
	}
}

// TestSystemPrompt: the profile's prompt is opencode's default.txt with
// exactly the edits NOTICE lists — each removed passage gone, the sentence
// or line on either side of it kept (the negative controls), "opencode"
// renamed — and H1's environment block, filled with the session's two facts
// and nothing else; a value that looks like a placeholder is inserted as it
// is. (The whole text is pinned by internal/harness's system_prompt.golden.)
func TestSystemPrompt(t *testing.T) {
	p, err := Profile()
	if err != nil {
		t.Fatal(err)
	}
	got := p.System(tool.SystemEnv{Workspace: "/home/user/project", OS: "linux"})
	for _, gone := range []string{
		"opencode", "OpenCode", "/help", "report the issue", "WebFetch", "use the ls tool", "npm run dev",
		"AGENTS.md", "system-reminder", "Task tool", "TodoWrite", "${",
	} {
		if strings.Contains(got, gone) {
			t.Errorf("the prompt still says %q", gone)
		}
	}
	for _, kept := range []string{
		"You are craze, an interactive CLI tool that helps users with software engineering tasks.",
		"IMPORTANT: You must NEVER generate or guess URLs",
		"# Tone and style\n",
		"user: what command should I run to list files in the current directory?\nassistant: ls\n</example>\n\n<example>\nuser: what files are in the directory src/?",
		"# Proactiveness\n", "# Following conventions\n", "# Code style\n", "# Doing tasks\n",
		"with Bash if they were provided to you to ensure your code is correct.\nNEVER commit changes unless the user explicitly asks you to.",
		"otherwise the user will feel that you are being too proactive.\n\n# Tool usage policy\n- You have the capability to call multiple tools in a single response.",
		"`file_path:line_number`",
	} {
		if !strings.Contains(got, kept) {
			t.Errorf("the prompt lost %q", kept)
		}
	}
	if !strings.HasSuffix(got, "</example>\n\nEnvironment:\n- Working directory: /home/user/project\n- Operating system: linux\n") {
		t.Errorf("the prompt does not end in the environment block:\n%s", got[max(0, len(got)-300):])
	}
	if again := p.System(tool.SystemEnv{Workspace: "/home/user/project", OS: "linux"}); again != got {
		t.Error("two calls built different prompts")
	}
	odd := p.System(tool.SystemEnv{Workspace: "/tmp/${os}", OS: "darwin"})
	if !strings.Contains(odd, "- Working directory: /tmp/${os}\n- Operating system: darwin\n") {
		t.Errorf("a workspace holding a placeholder was not inserted as it is:\n%s", odd[len(odd)-120:])
	}
}

// TestSpecsGolden pins what the model is offered — each tool's name,
// description and parameter schema, in order, exactly as ToolsJSON sends
// them — and their hash, which is part of every request's cache prefix.
// Regenerate with:
//
//	go test ./internal/harness/tool/opencode -run TestSpecsGolden -update
func TestSpecsGolden(t *testing.T) {
	// bash's description names the machine's OS, shell and temporary
	// directory; the golden pins bashVars' instead, whatever runs the test.
	// Not parallel: thisHost is package state.
	machine := thisHost
	thisHost = func() (host, error) {
		return host{os: bashVars["os"], shell: "/bin/" + bashVars["shell"], tmp: bashVars["tmp"]}, nil
	}
	t.Cleanup(func() { thisHost = machine })
	p, err := Profile()
	if err != nil {
		t.Fatal(err)
	}
	wire, err := p.ToolsJSON()
	if err != nil {
		t.Fatal(err)
	}
	sum, err := p.ToolsSHA256()
	if err != nil {
		t.Fatal(err)
	}
	var tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(wire, &tools); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "# The opencode profile's tools as a model is offered them (Profile.ToolsJSON).\n# tools_sha256: %s\n", sum)
	for _, tl := range tools {
		var params bytes.Buffer
		if err := json.Indent(&params, tl.Parameters, "", "  "); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "\n=== tool: %s\n--- description\n%s--- parameters\n%s\n", tl.Name, tl.Description, params.String())

		// required is an array, never null: strict providers reject null.
		var schema struct {
			Required *[]string `json:"required"`
		}
		if err := json.Unmarshal(tl.Parameters, &schema); err != nil || schema.Required == nil {
			t.Errorf("%s: required is missing or null in %s", tl.Name, tl.Parameters)
		}
	}
	if bytes.Contains(wire, []byte(`"required":null`)) {
		t.Fatalf("the wire tools hold a null required list:\n%s", wire)
	}
	if *updateGolden {
		if err := os.WriteFile(specsGolden, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(specsGolden)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./internal/harness/tool/opencode -run TestSpecsGolden -update)", err)
	}
	if !bytes.Equal(b.Bytes(), want) {
		t.Fatalf("the specs differ from %s (regenerate with -update if the change is deliberate)\n--- want ---\n%s\n--- got ---\n%s", specsGolden, want, b.Bytes())
	}
}

// bashVars are the values bash's description is rendered with; the bash
// tool supplies its own.
var bashVars = map[string]string{
	"os": "linux", "shell": "bash", "tmp": "/tmp/craze",
	"defaultTimeoutMs": "120000", "maxLines": "2000", "maxBytes": "51200",
}

// TestDescriptions: every description renders with nothing left to fill,
// and carries exactly the edits NOTICE lists — each removed sentence gone,
// each added one present — and no name of a tool this harness lacks. The
// negative controls are the sentences kept on either side of each edit, and
// a bash description rendered without its variables.
func TestDescriptions(t *testing.T) {
	vars := map[string]map[string]string{"bash": bashVars}
	rendered := map[string]string{}
	for _, name := range []string{"read", "write", "edit", "bash", "grep", "glob"} {
		d, err := description(name, vars[name])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(d, "${") {
			t.Errorf("%s: a placeholder is left: %q", name, d)
		}
		if !strings.HasSuffix(d, "\n") || strings.HasSuffix(d, "\n\n") {
			t.Errorf("%s: want opencode's single final newline", name)
		}
		for _, absent := range []string{"Task tool", "TodoWrite", "WebFetch", "LSP", "opencode", "OpenCode"} {
			if strings.Contains(d, absent) {
				t.Errorf("%s mentions %q", name, absent)
			}
		}
		rendered[name] = d
	}
	if _, err := description("bash", nil); err == nil {
		t.Fatal("bash's description rendered with no variables")
	}

	edits := []struct {
		file, gone, kept, added string
	}{
		{"read", "This tool can read image files and PDFs", "Avoid tiny repeated slices", "- This tool cannot read image files or PDFs yet.\n"},
		{"write", "This tool will fail if you did not read the file first.", "you MUST use the Read tool first to read the file's contents.\n", ""},
		{"edit", "This tool will error if you attempt an edit without reading the file.", "at least once in the conversation before editing.\n", ""},
		{"grep", "use the Task tool instead", "Do NOT use `grep`.\n", ""},
		{"glob", "use the Task tool instead", "as a batch that are potentially useful.\n", ""},
		{"bash", "", "commands will time out after 120000ms.",
			" The timeout cannot exceed 600000ms (10 minutes); a longer timeout is reduced to 600000ms.\n" +
				"  - When the command returns, every process still in its process group is killed, so nothing it runs in the background (for example, with `&` or `nohup`) outlives the call; a process that starts its own session or process group (setsid, setpgid, `set -m`) escapes this.\n"},
	}
	for _, e := range edits {
		d := rendered[e.file]
		if e.gone != "" && strings.Contains(d, e.gone) {
			t.Errorf("%s still says %q", e.file, e.gone)
		}
		if !strings.Contains(d, e.kept) {
			t.Errorf("%s lost %q", e.file, e.kept)
		}
		if e.added != "" && !strings.Contains(d, e.added) {
			t.Errorf("%s lacks the added %q", e.file, e.added)
		}
	}
	if !strings.Contains(rendered["bash"], "Be aware: OS: linux, Shell: bash\n") ||
		!strings.Contains(rendered["bash"], "If the output exceeds 2000 lines or 51200 bytes") ||
		!strings.Contains(rendered["bash"], "Use `/tmp/craze` for temporary work") {
		t.Errorf("bash's variables are not where opencode puts them:\n%s", rendered["bash"])
	}
}

// TestParameters ports opencode's parameters.test.ts cases for bash, read,
// write and edit, and adds what opencode's schemas refuse and Fantasy would
// not: a number or a boolean sent as a string, a fraction, a negative, null,
// a value past the safe-integer range. Each refusal is an invalid_input
// result naming the field; each acceptance is its negative control.
func TestParameters(t *testing.T) {
	f := newFixture(t)
	put(t, f.path("a"), "x\n")
	a := f.path("a")
	// The edit cases' own file: the accepted ones edit it x -> y -> z -> x.
	put(t, f.path("e"), "x\n")
	e := f.path("e")
	cases := []struct {
		name, tool, input string
		field             string // "" when the input is accepted
	}{
		{"read accepts filePath-only", "read", fmt.Sprintf(`{"filePath":%q}`, a), ""},
		{"read accepts optional offset + limit", "read", fmt.Sprintf(`{"filePath":%q,"offset":1,"limit":100}`, a), ""},
		{"read accepts zero offset and limit", "read", fmt.Sprintf(`{"filePath":%q,"offset":0,"limit":0}`, a), ""},
		{"read accepts an integral float", "read", fmt.Sprintf(`{"filePath":%q,"offset":1.0,"limit":1e2}`, a), ""},
		{"read ignores an unknown field", "read", fmt.Sprintf(`{"filePath":%q,"extra":true}`, a), ""},
		{"read refuses a numeric string", "read", fmt.Sprintf(`{"filePath":%q,"offset":"10"}`, a), "offset"},
		{"read refuses a numeric string limit", "read", fmt.Sprintf(`{"filePath":%q,"limit":"10"}`, a), "limit"},
		{"read refuses a fraction", "read", fmt.Sprintf(`{"filePath":%q,"limit":1.5}`, a), "limit"},
		{"read refuses a negative", "read", fmt.Sprintf(`{"filePath":%q,"offset":-1}`, a), "offset"},
		{"read refuses null", "read", fmt.Sprintf(`{"filePath":%q,"offset":null}`, a), "offset"},
		{"read refuses past the safe range", "read", fmt.Sprintf(`{"filePath":%q,"limit":9007199254740992}`, a), "limit"},
		{"read refuses a missing filePath", "read", `{"offset":1}`, "filePath"},
		{"read refuses a numeric filePath", "read", `{"filePath":5}`, "filePath"},
		{"read refuses a non-object", "read", `["a"]`, "JSON object"},
		{"bash accepts command", "bash", `{"command":"ls"}`, ""},
		{"bash accepts optional timeout + workdir", "bash", fmt.Sprintf(`{"command":"ls","timeout":5000,"workdir":%q}`, f.env.Workspace), ""},
		{"bash accepts an empty command", "bash", `{"command":""}`, ""},
		{"bash refuses a missing command", "bash", `{}`, "command"},
		{"bash refuses a numeric string timeout", "bash", `{"command":"ls","timeout":"5000"}`, "timeout"},
		{"bash refuses a zero timeout", "bash", `{"command":"ls","timeout":0}`, "timeout"},
		{"bash refuses a negative timeout", "bash", `{"command":"ls","timeout":-1}`, "timeout"},
		{"bash refuses a fractional timeout", "bash", `{"command":"ls","timeout":1.5}`, "timeout"},
		{"bash refuses a non-string workdir", "bash", `{"command":"ls","workdir":1}`, "workdir"},
		{"write accepts content + filePath", "write", fmt.Sprintf(`{"content":"hi","filePath":%q}`, a), ""},
		{"write refuses a missing filePath", "write", `{"content":"hi"}`, "filePath"},
		{"write refuses a missing content", "write", fmt.Sprintf(`{"filePath":%q}`, a), "content"},
		{"write refuses non-string content", "write", fmt.Sprintf(`{"content":123,"filePath":%q}`, a), "content"},
		{"edit accepts all four fields", "edit", fmt.Sprintf(`{"filePath":%q,"oldString":"x","newString":"y","replaceAll":true}`, e), ""},
		{"edit: replaceAll is optional", "edit", fmt.Sprintf(`{"filePath":%q,"oldString":"y","newString":"z"}`, e), ""},
		{"edit accepts replaceAll false", "edit", fmt.Sprintf(`{"filePath":%q,"oldString":"z","newString":"x","replaceAll":false}`, e), ""},
		{"edit rejects missing filePath", "edit", `{"oldString":"x","newString":"y"}`, "filePath"},
		{"edit rejects an empty filePath", "edit", `{"filePath":"","oldString":"x","newString":"y"}`, "filePath is required"},
		{"edit rejects missing oldString", "edit", fmt.Sprintf(`{"filePath":%q,"newString":"y"}`, e), "oldString"},
		{"edit rejects missing newString", "edit", fmt.Sprintf(`{"filePath":%q,"oldString":"x"}`, e), "newString"},
		{"edit rejects a boolean string", "edit", fmt.Sprintf(`{"filePath":%q,"oldString":"x","newString":"y","replaceAll":"true"}`, e), "replaceAll"},
		{"edit rejects a numeric replaceAll", "edit", fmt.Sprintf(`{"filePath":%q,"oldString":"x","newString":"y","replaceAll":1}`, e), "replaceAll"},
		{"edit rejects a null replaceAll", "edit", fmt.Sprintf(`{"filePath":%q,"oldString":"x","newString":"y","replaceAll":null}`, e), "replaceAll"},
		{"edit rejects a non-string oldString", "edit", fmt.Sprintf(`{"filePath":%q,"oldString":1,"newString":"y"}`, e), "oldString"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, res := f.call(t, tc.tool, tc.input)
			if tc.field == "" {
				ok(t, res)
				return
			}
			if !res.IsError || res.Class != tool.ClassInvalidInput || !strings.Contains(res.Text, tc.field) {
				t.Fatalf("result = %+v, want invalid_input naming %q", res, tc.field)
			}
		})
	}
}
