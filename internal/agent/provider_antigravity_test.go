package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/errkind"
)

// The antigravity provider (plan 053 M1) end to end, over the fake agent's
// antigravity scripts (cmd/craze-fake-agent/antigravity.go), and its pieces
// alone.

// agyRig is an antigravity session on the fake agent, with a HOME of its own
// (agy's brain directory is under it) and the fake's dumps beside it.
type agyRig struct {
	s    *session
	dir  string
	home string
	diag *lockedDiag
	log  *eventLog
}

func newAgyRig(t *testing.T, script string, opts Options, env ...string) *agyRig {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &agyRig{dir: dir, home: home, diag: &lockedDiag{}}
	p := AntigravityProvider()
	opts.Provider = &p
	opts.Binary = fakeAgentPath(t)
	opts.ExtraArgs = []string{"-script=" + script}
	if opts.Workspace == "" {
		opts.Workspace = t.TempDir()
	}
	opts.Stderr = &lockedDiag{}
	opts.Diag = r.diag
	t.Setenv("HOME", home)
	opts.Env = append(os.Environ(), append([]string{
		"HOME=" + home,
		"CRAZE_FAKE_DUMP_ARGV=" + filepath.Join(dir, "argv"),
		"CRAZE_FAKE_DUMP_CALLS=" + filepath.Join(dir, "calls"),
		"CRAZE_FAKE_DUMP_ENV=" + filepath.Join(dir, "env"),
	}, env...)...)
	r.s = newSession(opts)
	r.log = collect(t, r.s)
	t.Cleanup(func() { _ = r.s.Close() })
	return r
}

func (r *agyRig) start(t *testing.T) *agyRig {
	t.Helper()
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return r
}

// brain is a path under the rig's agy brain directory.
func (r *agyRig) brain(elem ...string) string {
	return filepath.Join(append([]string{r.home, ".gemini", "antigravity-cli", "brain"}, elem...)...)
}

func (r *agyRig) read(name string) string {
	raw, _ := os.ReadFile(filepath.Join(r.dir, name))
	return string(raw)
}

// sets is every session/set_config_option the fake read, in order.
func (r *agyRig) sets() []string { return sets(strings.Split(r.read("calls"), "\n")) }

func (r *agyRig) tool(t *testing.T, id string) ToolEvent {
	t.Helper()
	return toolByID(t, r.s.Snapshot().Tools, id)
}

func (r *agyRig) row(t *testing.T, id string) SubagentInfo {
	t.Helper()
	for _, sa := range r.s.Snapshot().Subagents {
		if sa.ID == id {
			return sa
		}
	}
	t.Fatalf("no row %s in %+v", id, r.s.Snapshot().Subagents)
	return SubagentInfo{}
}

func (r *agyRig) prompt(t *testing.T, text string) (Result, error) {
	t.Helper()
	return r.s.Prompt(t.Context(), text)
}

// TestAntigravityProviderData pins the provider the spawn, the registry and
// the TUI read (plan 053 § A).
func TestAntigravityProviderData(t *testing.T) {
	p := AntigravityProvider()
	if p.Name() != "antigravity" || p.DisplayName() != "antigravity" || p.Dialect() != acp.DialectAntigravity {
		t.Fatalf("identity %q %q %q", p.Name(), p.DisplayName(), p.Dialect())
	}
	if got := p.Bins(); !reflect.DeepEqual(got, []string{"agy"}) {
		t.Fatalf("bins %q", got)
	}
	if got := p.Args(nil, true); !reflect.DeepEqual(got, []string{"acp", "--dangerously-skip-permissions"}) {
		t.Fatalf("force argv %q", got)
	}
	if got := p.Args(nil, false); !reflect.DeepEqual(got, []string{"acp"}) {
		t.Fatalf("no-force argv %q", got)
	}
	if len(p.AuthMethodIDs()) != 0 || p.LoginHint() != "agy acp --login" {
		t.Fatalf("auth %q hint %q", p.AuthMethodIDs(), p.LoginHint())
	}
	want := Capabilities{Effort: true, Modes: true, AskCards: true, SubagentRows: true, ParameterizedPicker: true}
	if p.Capabilities() != want {
		t.Fatalf("capabilities %+v", p.Capabilities())
	}
	for id, kind := range map[string]ModeKind{"default": ModeImplement, "accept-edits": ModeImplement, "auto": ModeImplement, "plan": ModePlan, "ask": ModeUnknown, "agent": ModeUnknown} {
		if got := p.ModeKind(id); got != kind {
			t.Fatalf("mode %s = %v, want %v", id, got, kind)
		}
	}
	if p.CycleModes() != nil || p.ImplementPrompt() != "Implement the plan above." {
		t.Fatalf("cycle %q implement %q", p.CycleModes(), p.ImplementPrompt())
	}
	if p.Hidden() || !p.Resumable() || p.InProcess() || p.Aside() || !p.RecordsModel() || p.RefusesFast() {
		t.Fatalf("hidden %v resumable %v inProcess %v aside %v records %v fast %v", p.Hidden(), p.Resumable(), p.InProcess(), p.Aside(), p.RecordsModel(), p.RefusesFast())
	}
	if len(p.SkillScan().RelRoots) != 0 || p.PluginScan() != (PluginScan{}) || p.CompanionBins() != nil || p.AdapterInstall() != "" {
		t.Fatal("agy advertises its own skills: no scan, no plugins, no companion")
	}
	if p.Info().QuestionDismissCancels() {
		t.Fatal("Esc on an agy question skips it")
	}
	got, err := ProviderByName("antigravity")
	if err != nil || got.Name() != "antigravity" || got.Hidden() {
		t.Fatalf("ProviderByName = %+v, %v", got, err)
	}
	if want := []string{"cursor", "grok", "claude", "antigravity", "native"}; !slices.Equal(ProviderNames(), want) {
		t.Fatalf("listed providers %q, want %q", ProviderNames(), want)
	}
	// Images go as image blocks: agy says image true, so no override.
	if p.imagesDespiteCapability {
		t.Fatal("agy advertises images; nothing to override")
	}
}

// TestAntigravitySpawn (A2, A3): under force the agent is `agy acp
// --dangerously-skip-permissions`, under --no-force `agy acp`; no
// authenticate is ever sent for the four interactive methods it advertises;
// the environment carries the caller marker.
func TestAntigravitySpawn(t *testing.T) {
	for _, force := range []bool{true, false} {
		r := newAgyRig(t, "antigravity-basic", Options{Force: force}).start(t)
		argv := strings.Split(strings.TrimSpace(r.read("argv")), "\n")
		want := []string{"-script=antigravity-basic", "acp"}
		if force {
			want = append(want, "--dangerously-skip-permissions")
		}
		if !reflect.DeepEqual(argv, want) {
			t.Fatalf("force %v: argv %q, want %q", force, argv, want)
		}
		if strings.Contains(r.read("calls"), acp.MethodAuthenticate) {
			t.Fatalf("authenticate was called: %s", r.read("calls"))
		}
		if !strings.Contains(r.read("env"), "CRAZE_SESSION_PROVIDER=antigravity") {
			t.Fatal("no caller marker")
		}
	}
}

// TestAntigravityModes (C1): --ask is refused before anything is spawned, in
// the words /ask is refused with, as a config error; --plan starts in agy's
// plan mode; the cycle is every advertised mode.
func TestAntigravityModes(t *testing.T) {
	r := newAgyRig(t, "antigravity-basic", Options{Mode: "ask"})
	err := r.s.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "mode ask is not advertised") || ErrorInfo(err).Kind != errkind.Config {
		t.Fatalf("--ask: %v (%v)", err, ErrorInfo(err))
	}
	if r.read("argv") != "" {
		t.Fatal("--ask spawned the agent")
	}
	r = newAgyRig(t, "antigravity-basic", Options{Mode: "plan"}).start(t)
	snap := r.s.Snapshot()
	if snap.CurrentMode != "plan" || len(snap.Modes) != 4 {
		t.Fatalf("mode %q modes %+v", snap.CurrentMode, snap.Modes)
	}
	if !strings.Contains(r.read("calls"), acp.MethodSessionSetMode) {
		t.Fatal("--plan was not set")
	}
}

// TestAntigravityCatalog (C2, C3): the models are the model option's nine base
// models, not the legacy block's suffixed ids; the effort is thought_level,
// whose levels follow the model, and a model without it has no effort; a
// suffixed --model is the model and its level.
func TestAntigravityCatalog(t *testing.T) {
	r := newAgyRig(t, "antigravity-basic", Options{}).start(t)
	snap := r.s.Snapshot()
	var ids []string
	for _, m := range snap.Models {
		ids = append(ids, m.ID)
	}
	if len(ids) != 9 || ids[0] != "gemini-4-neon" || slices.ContainsFunc(ids, func(id string) bool { return strings.HasSuffix(id, "-high") }) {
		t.Fatalf("models %q", ids)
	}
	if snap.CurrentModel != "gemini-4-neon" || snap.Models[0].Name != "Gemini 4 Neon" {
		t.Fatalf("current %q first %+v", snap.CurrentModel, snap.Models[0])
	}
	eff := EffortOption(snap)
	if eff == nil || eff.ID != "thought_level" || eff.Current != "high" || len(eff.SelectValues) != 4 || eff.SelectValues[3].Name != "Extra" {
		t.Fatalf("effort %+v", eff)
	}
	if _, err := r.s.SetModel(t.Context(), "", "gpt-oss-120b-medium"); err != nil {
		t.Fatal(err)
	}
	if snap := r.s.Snapshot(); EffortOption(snap) != nil || snap.CurrentModel != "gpt-oss-120b-medium" {
		t.Fatalf("gpt-oss: model %q effort %+v", snap.CurrentModel, EffortOption(snap))
	}
	if _, err := r.s.SetModel(t.Context(), "", "gemini-3.8-flash-low"); err != nil {
		t.Fatal(err)
	}
	snap = r.s.Snapshot()
	if eff := EffortOption(snap); snap.CurrentModel != "gemini-3.8-flash" || eff == nil || eff.Current != "low" || len(eff.SelectValues) != 3 {
		t.Fatalf("suffixed: model %q effort %+v", snap.CurrentModel, eff)
	}
	// --model and --effort at the start, by the paths every provider's take.
	r = newAgyRig(t, "antigravity-basic", Options{Model: "gemini-4-argon", Effort: "low"}).start(t)
	snap = r.s.Snapshot()
	if eff := EffortOption(snap); snap.CurrentModel != "gemini-4-argon" || eff == nil || eff.Current != "low" {
		t.Fatalf("--model --effort: %q %+v", snap.CurrentModel, eff)
	}
}

// TestAntigravityEffortRule (C3): antigravity's effort is thought_level by
// id under the usable-select checks; cursor's thinking select, filed under
// thought_level, is still no effort, and neither is a thought_level select on
// cursor.
func TestAntigravityEffortRule(t *testing.T) {
	vals := []SelectValue{{Value: "low"}, {Value: "high"}}
	agy, cursor := AntigravityProvider(), CursorProvider()
	for _, tc := range []struct {
		p    Provider
		opt  ConfigOption
		want bool
	}{
		{agy, ConfigOption{ID: "thought_level", Name: "Thinking Level", Category: "thought_level", Type: "select", SelectValues: vals}, true},
		{agy, ConfigOption{ID: "thought_level", Type: "", SelectValues: vals}, true},
		{agy, ConfigOption{ID: "thought_level", Type: "boolean", SelectValues: vals}, false},
		{agy, ConfigOption{ID: "thought_level", Type: "select"}, false},
		{agy, ConfigOption{ID: "model", Category: "model", Type: "select", SelectValues: vals}, false},
		{cursor, ConfigOption{ID: "thinking", Name: "Thinking", Category: "thought_level", Type: "select", SelectValues: vals}, false},
		{cursor, ConfigOption{ID: "thought_level", Name: "Thinking Level", Category: "thought_level", Type: "select", SelectValues: vals}, false},
		{cursor, ConfigOption{ID: "effort", Type: "select", SelectValues: vals}, true},
	} {
		if got := tc.p.isEffortOption(tc.opt); got != tc.want {
			t.Errorf("%s %+v = %v, want %v", tc.p.Name(), tc.opt, got, tc.want)
		}
	}
}

// TestAntigravityQuestionCard (B4): ask_question is a question card whose
// question takes free text; a pick with text goes back as q1 and q1_other,
// the dismissal (Esc) as a skip — decline — and an unattended session answers
// the first option.
func TestAntigravityQuestionCard(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer *AskAnswer
		opts   Options
		want   string
	}{
		{"pick and text", &AskAnswer{Answers: map[string][]string{"q1": {"2"}}, Text: map[string]string{"q1": "teal"}}, Options{Interactive: true},
			`ask answer: {"action":"accept","content":{"q1":"2","q1_other":"teal"}}`},
		{"skip", &AskAnswer{Skip: true}, Options{Interactive: true}, `ask answer: {"action":"decline"}`},
		{"unattended", nil, Options{Force: true, Unattended: true, Approval: &ApprovalPolicy{
			Permission: ApprovalAllow, Question: ApprovalFirstOption, Plan: ApprovalReturn,
		}}, `ask answer: {"action":"accept","content":{"q1":"1"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAgyRig(t, "antigravity-basic", tc.opts).start(t)
			done := make(chan error, 1)
			go func() { _, err := r.s.Prompt(context.Background(), "ASK"); done <- err }()
			if tc.answer != nil {
				rec := waitOpenAsk(t, r.s, AskQuestion)
				q := rec.Body.Question
				if q == nil || len(q.Questions) != 1 || q.Questions[0].ID != "q1" || !q.Questions[0].AllowText ||
					q.Questions[0].Prompt != "What is your favourite colour?" || q.Questions[0].Options[0].Label != "(Recommended) Red" {
					t.Fatalf("question %+v", q)
				}
				if _, err := r.s.asks.Answer("", rec.ID, *tc.answer); err != nil {
					t.Fatal(err)
				}
			}
			r.log.waitText(t, tc.want)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := r.tool(t, "call_ask"); got.ToolName != "ask_question" || got.Status != toolCompleted {
				t.Fatalf("ask row %+v", got)
			}
		})
	}
}

// TestAntigravityRunCommandRows (B3, D1): a run_command row is named by agy's
// tool name, shows its CommandLine, and its output without agy's header, with
// LF line ends and the exit code — a headerless or empty output as it is, and
// "Command output:" later in the text untouched.
func TestAntigravityRunCommandRows(t *testing.T) {
	r := newAgyRig(t, "antigravity-basic", Options{Force: true}).start(t)
	for _, tc := range []struct {
		kind, stdout string
		code         *int
	}{
		{"header", "line one\nline two\n", ptr(0)},
		{"noheader", "plain output\n", ptr(0)},
		{"empty", "", nil},
		{"literal", "first\nCommand output:\nsecond\n", ptr(0)},
		{"exit2", "boom\n", ptr(2)},
	} {
		if _, err := r.prompt(t, "RUN "+tc.kind); err != nil {
			t.Fatal(err)
		}
		row := r.tool(t, "call_run")
		if row.ToolName != "run_command" || row.RawInput != "echo hi" || row.Status != toolCompleted {
			t.Fatalf("%s: row %+v", tc.kind, row)
		}
		switch {
		case tc.stdout == "" && tc.code == nil:
			if row.Output != nil {
				t.Fatalf("%s: output %+v", tc.kind, row.Output)
			}
		case row.Output == nil || row.Output.Stdout != tc.stdout || !reflect.DeepEqual(row.Output.ExitCode, tc.code):
			t.Fatalf("%s: output %+v", tc.kind, row.Output)
		}
		if row.ContentText != tc.stdout {
			t.Fatalf("%s: content %q, want %q", tc.kind, row.ContentText, tc.stdout)
		}
	}
}

// TestAntigravityCommandText is D1's header rule alone.
func TestAntigravityCommandText(t *testing.T) {
	const head = "Created At: 2026-11-22T22:16:51-06:00\nCompleted At: 2026-11-22T22:16:51-06:00\nCommand exited with code 0.\n\nCommand output:\n"
	for _, tc := range []struct {
		in, want string
		code     int
		ok       bool
	}{
		{head + "a\r\nb\r\n", "a\nb\n", 0, true},
		{strings.Replace(head, "code 0", "code 127", 1) + "x", "x", 127, true},
		// Not the complete block (review M1 F5): no Completed At, no blank
		// line — the command's own output, untouched.
		{"Created At: t\nCommand exited with code 1.\nCommand output:\nx", "Created At: t\nCommand exited with code 1.\nCommand output:\nx", 0, false},
		{"Created At: t\nCompleted At: t\nCommand exited with code 1.\nCommand output:\nx", "Created At: t\nCompleted At: t\nCommand exited with code 1.\nCommand output:\nx", 0, false},
		{"Created At: t\nCommand exited with code 1.\n\nCommand output:\nx", "Created At: t\nCommand exited with code 1.\n\nCommand output:\nx", 0, false},
		{"plain\r\n", "plain\n", 0, false},
		{"", "", 0, false},
		{"text\n" + head + "x", "text\n" + head + "x", 0, false},
		{"Command output:\nx", "Command output:\nx", 0, false},
	} {
		got, code, ok := antigravityCommandText(tc.in)
		if got != tc.want || code != tc.code || ok != tc.ok {
			t.Errorf("%q = %q %d %v, want %q %d %v", tc.in, got, code, ok, tc.want, tc.code, tc.ok)
		}
	}
}

// TestAntigravityNormaliserIsItsOwn (D1): cursor and grok read a rawInput's
// CommandLine and a rawOutput's output/exit_code exactly as they always did —
// as unknown keys.
func TestAntigravityNormaliserIsItsOwn(t *testing.T) {
	raw := mustJSON(map[string]any{"output": "Created At: t\nCommand exited with code 0.\n\nCommand output:\nx\r\n", "exit_code": 0})
	in := mustJSON(map[string]any{"CommandLine": "ls"})
	content := mustJSON([]map[string]any{{"type": "content", "content": map[string]string{"type": "text", "text": "Created At: t\nCommand exited with code 0.\n\nCommand output:\nx\r\n"}}})
	for _, p := range []Provider{CursorProvider(), GrokProvider()} {
		s := newSession(Options{Provider: &p})
		completed, name := "completed", "run"
		tool, _ := s.mergeTool(toolDelta{id: "t1", title: &name, status: &completed, rawInput: in, hasRawInput: true,
			rawOutput: raw, hasRawOutput: true, content: content, hasContent: true})
		if tool.Output != nil || tool.RawInput != `{"CommandLine":"ls"}` || !strings.HasPrefix(tool.ContentText, "Created At:") {
			t.Fatalf("%s: row %+v output %+v", p.Name(), tool, tool.Output)
		}
		_ = s.Close()
	}
}

// TestAntigravityPermissionCard (B5): under --no-force agy asks before a
// command, and the card shows; its allow goes back as agy's allow_once.
func TestAntigravityPermissionCard(t *testing.T) {
	r := newAgyRig(t, "antigravity-basic", Options{Force: false, Interactive: true}).start(t)
	done := make(chan error, 1)
	go func() { _, err := r.s.Prompt(context.Background(), "RUN"); done <- err }()
	rec := waitOpenAsk(t, r.s, AskPermission)
	if _, err := r.s.asks.Answer("", rec.ID, AskAnswer{OptionID: "allow"}); err != nil {
		t.Fatal(err)
	}
	r.log.waitText(t, `permission: {"outcome":{"outcome":"selected","optionId":"allow"}}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestAntigravityAuth (A3): a logged-out agy's -32000 "Authentication
// required" fails the start as an auth error naming `agy acp --login`, on
// session/new and on session/load; a prompt's does the same; a -32000 with
// another message is left alone.
func TestAntigravityAuth(t *testing.T) {
	for _, load := range []string{"", fakeSessionIDForTests} {
		r := newAgyRig(t, "antigravity-loggedout", Options{LoadSessionID: load})
		err := r.s.Start(t.Context())
		if err == nil || ErrorInfo(err).Kind != errkind.Auth || StartErrorKind(err) != errkind.Auth || !strings.Contains(err.Error(), "agy acp --login") {
			t.Fatalf("load %q: %v (%v)", load, err, ErrorInfo(err))
		}
	}
	r := newAgyRig(t, "antigravity-basic", Options{}).start(t)
	_, err := r.prompt(t, "AUTH")
	if err == nil || ErrorInfo(err).Kind != errkind.Auth || !strings.Contains(err.Error(), "agy acp --login") {
		t.Fatalf("prompt: %v (%v)", err, ErrorInfo(err))
	}
	_, err = r.prompt(t, "ERR32000")
	if err == nil || ErrorInfo(err).Kind == errkind.Auth || strings.Contains(err.Error(), "agy acp --login") {
		t.Fatalf("another -32000: %v (%v)", err, ErrorInfo(err))
	}
	// Only antigravity's classifier reads it: cursor's -32000 is as it was.
	rpc := &acp.RPCError{Code: -32000, Message: "Authentication required"}
	if got := CursorProvider().classifyAuth(rpc); got != error(rpc) {
		t.Fatalf("cursor classified %v", got)
	}
}

// fakeSessionIDForTests is the id the fake agent's session/new answers.
const fakeSessionIDForTests = "fake-session-1"

// TestAntigravityResumeRestore (C4): a load restores the recorded model and
// effort agy's load forgot, by the override rules — --model wins over both,
// --effort over the recorded effort — and a refused recorded value, or a
// model with no effort, leaves a note and the agent's value, never a failed
// start. Each case is checked on the snapshot and on the deltas a folding
// client sees, with the catalog update before the reply and after it.
func TestAntigravityResumeRestore(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		recModel, recEffort   string
		model, effort         string
		env                   []string
		wantSets              []string
		wantModel, wantEffort string
		wantNote              string
	}{
		{name: "model and effort", recModel: "gemini-3.8-flash", recEffort: "low",
			wantSets: []string{"model=gemini-3.8-flash", "thought_level=low"}, wantModel: "gemini-3.8-flash", wantEffort: "low"},
		{name: "push after the reply", recModel: "gemini-3.8-flash", recEffort: "low", env: []string{"CRAZE_FAKE_AGY_PUSH_AFTER=1"},
			wantSets: []string{"model=gemini-3.8-flash", "thought_level=low"}, wantModel: "gemini-3.8-flash", wantEffort: "low"},
		{name: "model alone", recModel: "gemini-4-argon",
			wantSets: []string{"model=gemini-4-argon"}, wantModel: "gemini-4-argon", wantEffort: "medium"},
		{name: "effort alone", recEffort: "low",
			wantSets: []string{"thought_level=low"}, wantModel: "gemini-4-neon", wantEffort: "low"},
		{name: "already there", recModel: "gemini-4-neon", recEffort: "high",
			wantModel: "gemini-4-neon", wantEffort: "high"},
		{name: "--model wins", recModel: "gemini-3.8-flash", recEffort: "low", model: "gemini-4-argon",
			wantSets: []string{"model=gemini-4-argon"}, wantModel: "gemini-4-argon", wantEffort: "medium"},
		{name: "suffixed --model", recModel: "gemini-3.8-flash", recEffort: "low", model: "gemini-4-hydrogen-xhigh",
			wantSets: []string{"model=gemini-4-hydrogen-xhigh"}, wantModel: "gemini-4-hydrogen", wantEffort: "xhigh"},
		{name: "--effort wins", recModel: "gemini-3.8-flash", recEffort: "low", effort: "high",
			wantSets: []string{"model=gemini-3.8-flash", "thought_level=high"}, wantModel: "gemini-3.8-flash", wantEffort: "high"},
		{name: "refused model", recModel: "gemini-3.8-flash", recEffort: "low", env: []string{"CRAZE_FAKE_AGY_REFUSE=gemini-3.8-flash"},
			wantSets: []string{"model=gemini-3.8-flash", "thought_level=low"}, wantModel: "gemini-4-neon", wantEffort: "low",
			wantNote: "recorded model gemini-3.8-flash was not applied; it runs on gemini-4-neon"},
		{name: "refused effort", recModel: "gemini-3.8-flash", recEffort: "low", env: []string{"CRAZE_FAKE_AGY_REFUSE=low"},
			wantSets: []string{"model=gemini-3.8-flash", "thought_level=low"}, wantModel: "gemini-3.8-flash", wantEffort: "medium",
			wantNote: "recorded effort low was not applied; it runs at medium"},
		{name: "unoffered effort", recModel: "gemini-3.8-flash", recEffort: "xhigh",
			wantSets: []string{"model=gemini-3.8-flash"}, wantModel: "gemini-3.8-flash", wantEffort: "medium",
			wantNote: "recorded effort xhigh was not applied; it runs at medium"},
		{name: "model without effort", recModel: "gpt-oss-120b-medium", recEffort: "high",
			wantSets: []string{"model=gpt-oss-120b-medium"}, wantModel: "gpt-oss-120b-medium",
			wantNote: "recorded effort high was not applied; model gpt-oss-120b-medium has no effort setting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAgyRig(t, "antigravity-basic", Options{LoadSessionID: fakeSessionIDForTests,
				LoadModel: tc.recModel, LoadEffort: tc.recEffort, Model: tc.model, Effort: tc.effort}, tc.env...).start(t)
			if got := r.sets(); !slices.Equal(got, tc.wantSets) {
				t.Fatalf("sets %q, want %q", got, tc.wantSets)
			}
			snap := r.s.Snapshot()
			effort := ""
			if eff := EffortOption(snap); eff != nil {
				effort = eff.Current
			}
			if snap.CurrentModel != tc.wantModel || effort != tc.wantEffort {
				t.Fatalf("snapshot %q %q, want %q %q", snap.CurrentModel, effort, tc.wantModel, tc.wantEffort)
			}
			if tc.wantNote == "" && r.diag.String() != "" {
				t.Fatalf("unexpected note %q", r.diag.String())
			}
			if !strings.Contains(r.diag.String(), tc.wantNote) {
				t.Fatalf("note %q, want %q", r.diag.String(), tc.wantNote)
			}
			// The settings deltas are enqueued, not published: the outbox
			// is drained before the collector is read.
			r.s.flushDelta()
			r.log.caughtUp(t)
			f := foldDeltas(r.log.snapshot())
			deltaEffort := ""
			if eff := EffortOption(Snapshot{Provider: snap.Provider, Config: f.Config}); eff != nil {
				deltaEffort = eff.Current
			}
			if f.Model != tc.wantModel || deltaEffort != tc.wantEffort {
				t.Fatalf("folded deltas %q %q, want %q %q", f.Model, deltaEffort, tc.wantModel, tc.wantEffort)
			}
		})
	}
}

// TestAntigravityResumeFailsOnAuth (C4): a recorded value's set failing for
// any reason but a refusal of the value fails the start: an auth error here.
func TestAntigravityResumeFailsOnAuth(t *testing.T) {
	rpc := &acp.RPCError{Code: -32000, Message: "Authentication required"}
	if recordedRefused(rpc) || !recordedRefused(&acp.RPCError{Code: acp.CodeInvalidParams}) || !recordedRefused(&acp.RPCError{Code: acp.CodeMethodNotFound}) {
		t.Fatal("recordedRefused reads the wrong codes")
	}
	s := newSession(Options{Provider: ptr(AntigravityProvider())})
	defer func() { _ = s.Close() }()
	set, err := s.restoreSet(t.Context(), "the recorded model x", func(context.Context) error { return rpc }, func() { t.Error("an auth error noted as a refusal") })
	if set || ErrorInfo(err).Kind != errkind.Auth {
		t.Fatalf("restoreSet = %t %v", set, err)
	}
}

// TestAntigravityStaleLoad (C5): agy answers a load of an id it no longer
// has with an empty session and no error — the documented exception to the
// no-silent-new-session rule: nothing is replayed, and the session runs.
func TestAntigravityStaleLoad(t *testing.T) {
	r := newAgyRig(t, "antigravity-basic", Options{LoadSessionID: "stale-session"}).start(t)
	snap := r.s.Snapshot()
	if snap.SessionID != "stale-session" || len(snap.Tools) != 0 || len(snap.Subagents) != 0 {
		t.Fatalf("stale load %+v", snap)
	}
	r.log.caughtUp(t)
	for _, ev := range r.log.snapshot() {
		switch ev.Type {
		case EventUser, EventText, EventTool, EventSubagent:
			t.Fatalf("a stale load replayed %+v", ev)
		}
	}
	if _, err := r.prompt(t, "hello"); err != nil {
		t.Fatal(err)
	}
}

// TestAntigravityEdits (D2, D3): edit_file and write_file stamp their
// workspace paths, relative; a failed write stamps nothing; a write into
// another session's brain directory is an outside edit; the write that asks
// for feedback in this session's brain is the plan file, and the todo list
// beside it is dropped.
func TestAntigravityEdits(t *testing.T) {
	r := newAgyRig(t, "antigravity-basic", Options{Force: true}).start(t)
	if _, err := r.prompt(t, "EDITS"); err != nil {
		t.Fatal(err)
	}
	if e := r.tool(t, "call_edit").Edited; e == nil || !slices.Equal(e.Files, []string{"hello.txt"}) || e.PlanFile != "" {
		t.Fatalf("edit %+v", e)
	}
	if e := r.tool(t, "call_write").Edited; e == nil || !slices.Equal(e.Files, []string{"new.txt"}) {
		t.Fatalf("write %+v", e)
	}
	if e := r.tool(t, "call_write_failed").Edited; e != nil {
		t.Fatalf("a failed write was stamped %+v", e)
	}
	other := r.brain("another-session", "plan.md")
	if e := r.tool(t, "call_other_brain").Edited; e == nil || !slices.Equal(e.OutsideFiles, []string{other}) || e.PlanFile != "" {
		t.Fatalf("another session's brain %+v", e)
	}
	if _, err := r.prompt(t, "PLAN"); err != nil {
		t.Fatal(err)
	}
	plan := r.brain(fakeSessionIDForTests, "add_gamma_plan.md")
	if e := r.tool(t, "call_plan").Edited; e == nil || e.PlanFile != plan || len(e.Files)+len(e.OutsideFiles) != 0 {
		t.Fatalf("plan %+v, want %s", e, plan)
	}
	if e := r.tool(t, "call_todo").Edited; e != nil {
		t.Fatalf("the todo list was stamped %+v", e)
	}
}

// TestAntigravityEditsSymlinkedRoot (D3): a brain root reached through a
// symlink is the same root — the plan file is told by where it resolves.
func TestAntigravityEditsSymlinkedRoot(t *testing.T) {
	r := newAgyRig(t, "antigravity-basic", Options{Force: true})
	real := filepath.Join(r.dir, "real-gemini")
	if err := os.MkdirAll(filepath.Join(real, "antigravity-cli", "brain", fakeSessionIDForTests), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(r.home, ".gemini")); err != nil {
		t.Fatal(err)
	}
	r.start(t)
	if _, err := r.prompt(t, "PLAN"); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(real, "antigravity-cli", "brain", fakeSessionIDForTests))
	if err != nil {
		t.Fatal(err)
	}
	if e := r.tool(t, "call_plan").Edited; e == nil || e.PlanFile != filepath.Join(resolved, "add_gamma_plan.md") {
		t.Fatalf("plan through a symlink %+v", e)
	}
	if e := r.tool(t, "call_todo").Edited; e != nil {
		t.Fatalf("the todo list was stamped %+v", e)
	}
}

// TestAntigravityReplayClassified (D3, D4): a load's replayed calls are
// classified as live ones are — the plan file in the loaded session's brain
// directory is its plan — and a replayed sub-agent row is never running.
func TestAntigravityReplayClassified(t *testing.T) {
	r := newAgyRig(t, "antigravity-basic", Options{LoadSessionID: fakeSessionIDForTests}).start(t)
	plan := r.brain(fakeSessionIDForTests, "replayed_plan.md")
	if e := r.tool(t, "call_replay_plan").Edited; e == nil || e.PlanFile != plan {
		t.Fatalf("replayed plan %+v", e)
	}
	row := r.row(t, "call_replay_sub")
	if row.Status != SubagentCompleted || row.Description != "Replayed runner" || row.SubagentType != "self" {
		t.Fatalf("replayed row %+v", row)
	}
	r.log.caughtUp(t)
	if countStatus(r.s, SubagentRunning) != 0 {
		t.Fatal("a replayed row is running")
	}
}

// rowTimeline is every event of row id, and the index of the turn's EventDone
// or EventError.
func rowTimeline(evs []Event, id string) (statuses []SubagentStatus, callCompletedAt, finishedAt, endAt int) {
	callCompletedAt, finishedAt, endAt = -1, -1, -1
	for i, ev := range evs {
		switch {
		case ev.Type == EventTool && ev.Tool != nil && ev.Tool.ID == id && ev.Tool.Status == toolCompleted && callCompletedAt < 0:
			callCompletedAt = i
		case ev.Type == EventSubagent && ev.Subagent != nil && ev.Subagent.ID == id:
			statuses = append(statuses, ev.Subagent.Status)
			if ev.SubagentChange == SubagentChangeFinished && finishedAt < 0 {
				finishedAt = i
			}
		case (ev.Type == EventDone || ev.Type == EventError) && endAt < 0 && finishedAt >= 0:
			endAt = i
		}
	}
	return statuses, callCompletedAt, finishedAt, endAt
}

// TestAntigravitySubagentRows (D4): an invoke_subagent call is a row that
// stays running after its call completes and is settled with its turn,
// ahead of the turn's end: completed at end_turn, cancelled for a cancelled
// turn, failed for a prompt error or the agent dying; a call that fails
// settles at once; a late update never reopens a settled row; and each turn
// settles only the rows it launched.
func TestAntigravitySubagentRows(t *testing.T) {
	t.Run("end_turn", func(t *testing.T) {
		r := newAgyRig(t, "antigravity-basic", Options{}).start(t)
		if _, err := r.prompt(t, "SUBAGENTS"); err != nil {
			t.Fatal(err)
		}
		r.log.caughtUp(t)
		for _, id := range []string{"call_sub_a", "call_sub_b"} {
			statuses, called, finished, end := rowTimeline(r.log.snapshot(), id)
			if called < 0 || finished < called || end < finished || statuses[0] != SubagentRunning || statuses[len(statuses)-1] != SubagentCompleted {
				t.Fatalf("%s: statuses %v, call completed at %d, finished at %d, turn ended at %d", id, statuses, called, finished, end)
			}
			for _, st := range statuses[:len(statuses)-1] {
				if st != SubagentRunning {
					t.Fatalf("%s settled before its turn: %v", id, statuses)
				}
			}
		}
		if row := r.row(t, "call_sub_a"); row.Description != "Runner A" || row.SubagentType != "self" || !strings.Contains(row.Prompt, "echo A") {
			t.Fatalf("row %+v", row)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		r := newAgyRig(t, "antigravity-basic", Options{}).start(t)
		if _, err := r.prompt(t, "SUBAGENT"); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := r.s.Prompt(context.Background(), "SUBAGENT hold"); done <- err }()
		waitFor(t, "the held row", func() bool {
			for _, sa := range r.s.Snapshot().Subagents {
				if sa.ID == "call_sub_hold" {
					return true
				}
			}
			return false
		})
		waitFor(t, "the held call completed", func() bool { return r.tool(t, "call_sub_hold").Status == toolCompleted })
		if row := r.row(t, "call_sub_hold"); row.Status != SubagentRunning {
			t.Fatalf("held row %+v", row)
		}
		if _, err := r.s.Cancel(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-done
		if row := r.row(t, "call_sub_hold"); row.Status != SubagentCancelled {
			t.Fatalf("cancelled turn's row %+v", row)
		}
		// The turn before settled its own row, and this one did not touch it.
		if row := r.row(t, "call_sub_1"); row.Status != SubagentCompleted {
			t.Fatalf("the earlier turn's row %+v", row)
		}
	})
	for _, how := range []string{"error", "die"} {
		t.Run(how, func(t *testing.T) {
			r := newAgyRig(t, "antigravity-basic", Options{}).start(t)
			if _, err := r.prompt(t, "SUBAGENT "+how); err == nil {
				t.Fatal("the turn did not fail")
			}
			id := map[string]string{"error": "call_sub_err", "die": "call_sub_die"}[how]
			if row := r.row(t, id); row.Status != SubagentFailed {
				t.Fatalf("%s: row %+v", how, row)
			}
		})
	}
	t.Run("failed launch", func(t *testing.T) {
		r := newAgyRig(t, "antigravity-basic", Options{}).start(t)
		if _, err := r.prompt(t, "SUBAGENT fail"); err != nil {
			t.Fatal(err)
		}
		r.log.caughtUp(t)
		statuses, _, finished, end := rowTimeline(r.log.snapshot(), "call_sub_fail")
		if statuses[len(statuses)-1] != SubagentFailed || finished < 0 || end < finished {
			t.Fatalf("statuses %v finished %d end %d", statuses, finished, end)
		}
	})
	t.Run("late update", func(t *testing.T) {
		r := newAgyRig(t, "antigravity-basic", Options{}).start(t)
		if _, err := r.prompt(t, "SUBAGENT late"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the late update", func() bool { return r.row(t, "call_sub_late").Description == "Late runner, renamed" })
		if row := r.row(t, "call_sub_late"); row.Status != SubagentCompleted {
			t.Fatalf("a late update reopened the row %+v", row)
		}
		if _, err := r.prompt(t, "hello"); err != nil {
			t.Fatal(err)
		}
		if row := r.row(t, "call_sub_late"); row.Status != SubagentCompleted {
			t.Fatalf("the next turn touched the row %+v", row)
		}
	})
}

// TestTurnRowStatus is D4's table of a turn's outcome.
func TestTurnRowStatus(t *testing.T) {
	for _, tc := range []struct {
		res  *acp.PromptResult
		err  error
		want string
	}{
		{&acp.PromptResult{StopReason: acp.StopEndTurn}, nil, toolCompleted},
		{&acp.PromptResult{StopReason: acp.StopMaxTokens}, nil, toolCompleted},
		{&acp.PromptResult{StopReason: acp.StopMaxTurnRequests}, nil, toolCompleted},
		{&acp.PromptResult{StopReason: acp.StopRefusal}, nil, toolCompleted},
		{&acp.PromptResult{StopReason: acp.StopCancelled}, nil, toolCancelled},
		{nil, acp.ErrAgentExited, toolFailed},
	} {
		if got := turnRowStatus(tc.res, tc.err); got != tc.want {
			t.Errorf("%+v %v = %v, want %v", tc.res, tc.err, got, tc.want)
		}
	}
}

// TestAntigravityCancelMidTurn: a cancelled run_command turn ends cancelled
// and its row failed, as agy reports it after the reply (frames-07).
func TestAntigravityCancelMidTurn(t *testing.T) {
	r := newAgyRig(t, "antigravity-basic", Options{}).start(t)
	done := make(chan Result, 1)
	go func() { res, _ := r.s.Prompt(context.Background(), "SLEEP"); done <- res }()
	waitFor(t, "the running command", func() bool {
		for _, tl := range r.s.Snapshot().Tools {
			if tl.ID == "call_sleep" {
				return true
			}
		}
		return false
	})
	if _, err := r.s.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	if res := <-done; res.StopReason != acp.StopCancelled {
		t.Fatalf("stop %q", res.StopReason)
	}
	waitFor(t, "the failed row", func() bool { return r.tool(t, "call_sleep").Status == toolFailed })
}
