package tui

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/roster"
)

// `/provider` and `/model` in the session list's input (plan 030 §3.14, AC11,
// C16): the `/` popup's commands and values, the providers filtered as the
// startup picker filters them, the model reset to the provider's default on a
// provider change (native's read from its table), an ACP provider's models
// from the catalog cache with its age — typed text taken as typed with none —
// the pick lasting across openings and switches, and what it makes the next
// dispatch and the unstarted session run.

// cursorCatalog is a cached cursor catalog, seen three hours before sessNow.
var cursorCatalog = ModelCatalog{
	Models: []agent.ModelInfo{
		{ID: "composer-2.5", Name: "Composer 2.5"},
		{ID: "grok-4.6", Name: "Grok 4.6"},
		{ID: "claude-opus-5", Name: "Claude Opus 5"},
	},
	ObservedAt: sessNow.Add(-3 * time.Hour),
}

// cmdList is newSessModel's list, open on richSnapshot with its recent
// directories, and the catalog cache holding catalogs.
func cmdList(t *testing.T, cols, rows int, catalogs map[string]ModelCatalog) (Model, *startSessions) {
	t.Helper()
	m, fs, _ := newSessModel(t, cols, rows)
	fs.catalogs = catalogs
	return newList(t, m, fs), fs
}

// cmdItems is the `/` popup's candidates by name.
func cmdItems(m Model) []string {
	if !m.sessList.in.cmd.visible() {
		return nil
	}
	var out []string
	for _, it := range m.sessList.in.cmd.ans.Items {
		out = append(out, it.Name)
	}
	return out
}

// cmdItem is the `/` popup's candidate named name, or the test fails.
func cmdItem(t *testing.T, m Model, name string) completeItem {
	t.Helper()
	for _, it := range m.sessList.in.cmd.ans.Items {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("no candidate %q among %v", name, cmdItems(m))
	return completeItem{}
}

// clearInput empties the list's input with esc — the popup's first, when one
// is up, then the input's.
func clearInput(t *testing.T, m Model) Model {
	t.Helper()
	for range 3 {
		if m.sessList.in.ti.Value() == "" {
			return m
		}
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	}
	t.Fatalf("esc did not clear the input %q", m.sessList.in.ti.Value())
	return m
}

// modelsLoaded types text into the list's input and delivers the model list
// its `/model ` starts (the popup's load), within a step.
func modelsLoaded(t *testing.T, m Model, text string) Model {
	t.Helper()
	m, cmd := typeList(t, m, text)
	msg, ok := runWatched(t, mustCmd(t, cmd, "startLoad")).(completeLoadedMsg)
	if !ok || msg.source != sessCmdSourceID {
		t.Fatalf("the load answered %+v", msg)
	}
	tm, _ := m.Update(msg)
	return tm.(Model)
}

// nativeHome writes native's model table under a CRAZE_HOME of the test's
// own — alpha on a provider with an inline key, beta on one with none, beta
// the default_model unless def names another — and answers its native dir.
func nativeHome(t *testing.T, def string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	dir := filepath.Join(home, "native")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	providers := `version = 1

[providers.funded]
driver = "openai-compat"
base_url = "http://127.0.0.1:1/v1"
api_key = "sk-c16-test-0123456789abcdef"
source = "manual"

[providers.unfunded]
driver = "openai-compat"
base_url = "http://127.0.0.1:1/v1"
env_keys = ["CRAZE_C16_TEST_KEY"]
source = "manual"
`
	if def == "" {
		def = "beta"
	}
	// catalog = false (plan 031 C2): these two files are the whole table, as
	// they were before craze shipped a catalog to merge them over.
	models := fmt.Sprintf(`version = 1
catalog = false
default_model = %q

[models.alpha]
provider = "funded"
wire_model = "alpha-wire"
name = "Alpha One"

[models.beta]
provider = "unfunded"
wire_model = "beta-wire"
name = "Beta"
`, def)
	if err := os.WriteFile(filepath.Join(dir, modeltable.ProvidersFile), []byte(providers), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, modeltable.ModelsFile), []byte(models), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_C16_TEST_KEY", "")
	return dir
}

// TestTheSlashOpensTheListsCommands (§3.14): `/` first in the input opens the
// list's commands — /provider and /model with what new sessions use now, and
// /exit — narrowed as the command is typed; a word no command starts with,
// and any other `/…` line, is a prompt and opens nothing; enter on a command
// writes it with the space that lists its values; the `@` and `/` popups are
// never up together.
func TestTheSlashOpensTheListsCommands(t *testing.T) {
	m, _ := cmdList(t, 100, 30, nil)
	m, _ = typeList(t, m, "/")
	if got, want := cmdItems(m), []string{"/provider", "/model", "/exit"}; !slices.Equal(got, want) {
		t.Fatalf("the commands %v, want %v", got, want)
	}
	if n := cmdItem(t, m, "/provider").Note; n != "cursor" {
		t.Fatalf("/provider's note %q, want the provider new sessions run", n)
	}
	if n := cmdItem(t, m, "/model").Note; n != "Grok" {
		t.Fatalf("/model's note %q, want the model new sessions run", n)
	}
	if m.sessList.in.at.visible() {
		t.Fatal("the `@` popup is up over a `/` line")
	}
	if h := sessHint(m); !strings.Contains(h, "tab/enter use it") {
		t.Fatalf("the hint line %q", h)
	}
	m, _ = typeList(t, m, "m")
	if got := cmdItems(m); !slices.Equal(got, []string{"/model"}) {
		t.Fatalf("/m lists %v", got)
	}
	m = clearInput(t, m)
	for _, line := range []string{"/x", "/review the diff", "/exit now", "/models"} {
		mm, _ := typeList(t, m, line)
		if mm.sessList.in.cmd.visible() {
			t.Fatalf("%q opened the popup: %v", line, cmdItems(mm))
		}
	}
	mm, _ := typeList(t, m, "/review the diff")
	if h := sessHint(mm); !strings.Contains(h, "starts it in the background") {
		t.Fatalf("a `/…` prompt's hint line %q", h)
	}

	m, _ = typeList(t, m, "/pro")
	m, _ = press(m, enter())
	if v, _ := inputOf(m); v != "/provider " {
		t.Fatalf("enter on /provider left %q", v)
	}
	if got := cmdItems(m); !slices.Equal(got, []string{"cursor", "grok", "native"}) {
		t.Fatalf("/provider lists %v", got)
	}
	if it := cmdItem(t, m, "cursor"); it.Note != sessCurrentNote || it.Detail != "cursor-agent over ACP" {
		t.Fatalf("cursor's row %+v", it)
	}
	if it := cmdItem(t, m, "grok"); it.Note != "" {
		t.Fatalf("grok's row %+v is marked", it)
	}

	m = clearInput(t, m)
	m, _ = typeList(t, m, "@")
	if !m.sessList.in.at.visible() || m.sessList.in.cmd.visible() {
		t.Fatal("an `@` line's popups")
	}
}

// TestTheProvidersAreThePickersProviders (§3.14): /provider offers exactly
// the startup picker's providers — agent.Providers() filtered for what this
// machine can run (pickerRows over the launch's list): gx, optional, only
// when its binary resolves — in the picker's order.
func TestTheProvidersAreThePickersProviders(t *testing.T) {
	m, _ := cmdList(t, 100, 30, nil)
	var ids []string
	for _, p := range m.providers {
		ids = append(ids, p.Name())
	}
	if slices.Contains(ids, agent.GxProvider().Name()) {
		t.Fatalf("fixture: the picker's default rows %v hold gx", ids)
	}
	m, _ = typeList(t, m, "/provider ")
	if got := cmdItems(m); !slices.Equal(got, ids) {
		t.Fatalf("/provider lists %v, the picker %v", got, ids)
	}

	// gx's binary resolved (the launch's list has it): offered, in order.
	m = clearInput(t, m)
	m.providers = pickerRows(agent.Providers(), m.providerDefault)
	m, _ = typeList(t, m, "/provider ")
	if got, want := cmdItems(m), []string{"cursor", "grok", "gx", "native"}; !slices.Equal(got, want) {
		t.Fatalf("/provider lists %v, want %v", got, want)
	}
	// Narrowed by id.
	m, _ = typeList(t, m, "g")
	if got := cmdItems(m); !slices.Equal(got, []string{"grok", "gx"}) {
		t.Fatalf("/provider g lists %v", got)
	}
	m, _ = typeList(t, m, "z")
	if p := m.sessList.in.cmd; len(p.ans.Items) != 0 || !p.ans.NoteErr || p.ans.Note != "no provider gz" {
		t.Fatalf("/provider gz: %+v", p.ans)
	}
}

// TestChoosingAProviderResetsTheModel (§3.14): a model chosen, then a
// provider: the model goes back to that provider's default — an ACP
// provider's agent's own (no --model), native's read from its table, the
// funded one when the table's default has no key — and the rule, the hint
// line and the next session's spec say so. A native default read for a
// choice since replaced is dropped.
func TestChoosingAProviderResetsTheModel(t *testing.T) {
	m, _ := cmdList(t, 100, 30, map[string]ModelCatalog{"cursor": cursorCatalog})
	m.nativeDir, m.nativeEnv = nativeHome(t, ""), os.Getenv // after cmdList, whose HOME clears CRAZE_HOME
	m = modelsLoaded(t, m, "/model comp")
	m, _ = press(m, enter())
	if r := ruleOf(t, m); !strings.HasSuffix(r, " · cursor · Composer 2.5") {
		t.Fatalf("after /model: the rule %q", r)
	}
	if h := sessHint(m); h != "new sessions use cursor · Composer 2.5" {
		t.Fatalf("after /model: the hint %q", h)
	}
	if v, _ := inputOf(m); v != "" {
		t.Fatalf("a choice left %q in the input", v)
	}

	m, _ = typeList(t, m, "/provider gr")
	m, _ = press(m, enter())
	spec, err := m.sessNewSpec("/somewhere")
	if err != nil || spec.Provider.Name() != "grok" || spec.Model != "" {
		t.Fatalf("after /provider grok: %+v (%s), %v", spec, spec.Provider.Name(), err)
	}
	if r := ruleOf(t, m); !strings.HasSuffix(r, " · grok · default") {
		t.Fatalf("after /provider grok: the rule %q", r)
	}
	if h := sessHint(m); h != "new sessions use grok · default" {
		t.Fatalf("after /provider grok: the hint %q", h)
	}

	// native: its default read off the Update — beta, the table's, has no
	// key, so alpha, the first funded alias.
	m, _ = typeList(t, m, "/provider native")
	m, cmd := press(m, enter())
	read := mustCmd(t, cmd, "readNativeDefault")
	if r := ruleOf(t, m); !strings.HasSuffix(r, " · native · default") {
		t.Fatalf("while native's default is read: the rule %q", r)
	}
	tm, _ := m.Update(runWatched(t, read))
	m = tm.(Model)
	spec, _ = m.sessNewSpec("/somewhere")
	if spec.Provider.Name() != "native" || spec.Model != "alpha" {
		t.Fatalf("after /provider native: %+v (%s)", spec, spec.Provider.Name())
	}
	if r := ruleOf(t, m); !strings.HasSuffix(r, " · native · Alpha One") {
		t.Fatalf("after /provider native: the rule %q", r)
	}
	if h := sessHint(m); h != "new sessions use native · Alpha One" {
		t.Fatalf("after /provider native: the hint %q", h)
	}

	// A read for a choice since replaced is dropped.
	m, _ = typeList(t, m, "/provider native")
	m, cmd = press(m, enter())
	stale := runWatched(t, mustCmd(t, cmd, "readNativeDefault"))
	m, _ = typeList(t, m, "/provider cursor")
	m, _ = press(m, enter())
	tm, _ = m.Update(stale)
	m = tm.(Model)
	if spec, _ := m.sessNewSpec("/x"); spec.Provider.Name() != "cursor" || spec.Model != "" {
		t.Fatalf("a stale native default was taken: %+v (%s)", spec, spec.Provider.Name())
	}
}

// TestModelListsTheCachedCatalog (§3.14, AC11): an ACP provider's /model is
// the catalog a host last recorded, read once per opening of the popup, its
// age in the title, its agent's own default first, narrowed by a substring
// of a name or an id; a model chosen is the next dispatch's, and it stays the
// provider's it was chosen for when the list is opened over another's.
func TestModelListsTheCachedCatalog(t *testing.T) {
	m, fs := cmdList(t, 100, 30, map[string]ModelCatalog{"cursor": cursorCatalog})
	m = modelsLoaded(t, m, "/model ")
	p := m.sessList.in.cmd
	if p.ans.Title != "cursor models for new sessions · last seen 3h ago" {
		t.Fatalf("the title %q", p.ans.Title)
	}
	if got, want := cmdItems(m), []string{"default", "Grok 4.6", "Claude Opus 5", "Composer 2.5"}; !slices.Equal(got, want) {
		t.Fatalf("/model lists %v, want %v", got, want)
	}
	if it := cmdItem(t, m, "Grok 4.6"); it.Detail != "grok-4.6" || it.Note != "" {
		t.Fatalf("a model's row %+v", it)
	}
	m, _ = typeList(t, m, "OPUS")
	if got := cmdItems(m); !slices.Equal(got, []string{"Claude Opus 5"}) {
		t.Fatalf("/model OPUS lists %v", got)
	}
	if reads := fs.catReads; !slices.Equal(reads, []string{"cursor"}) {
		t.Fatalf("the catalog was read %v, want once for the popup", reads)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyTab}) // tab uses it too
	if spec, _ := m.sessNewSpec("/x"); spec.Model != "claude-opus-5" || spec.Provider.Name() != "cursor" {
		t.Fatalf("after the choice: %+v", spec)
	}

	// Chosen, it is marked the next time the list is offered.
	m = modelsLoaded(t, m, "/model ")
	if it := cmdItem(t, m, "Claude Opus 5"); it.Note != sessCurrentNote {
		t.Fatalf("the model chosen is not marked: %+v", it)
	}
	m = clearInput(t, m)

	// The list over a grok session: the model stays cursor's.
	m.snap.Provider = agent.GrokProvider().Info()
	if spec, _ := m.sessNewSpec("/x"); spec.Provider.Name() != "cursor" || spec.Model != "claude-opus-5" {
		t.Fatalf("over a grok session: %+v (%s)", spec, spec.Provider.Name())
	}

	// And the next dispatch spawns it.
	hb := newHostBackend("new", homePath("projects/lumen"), &callLog{})
	fs.spawn = func(SpawnSpec) (roster.Ref, error) { return hostRef("new"), nil }
	fs.open = func(roster.Ref) (backend.Backend, error) { return hb, nil }
	m, _ = typeList(t, m, "@lu")
	m, _ = press(m, enter())
	m, _ = typeList(t, m, "tidy the changelog")
	_, cmd := press(m, enter())
	if msg := dispatched(t, mustCmd(t, cmd, "sessDispatch")); msg.out != dispatchAccepted {
		t.Fatalf("the dispatch: %v, %v", msg.out, msg.err)
	}
	if len(fs.spawns) != 1 || fs.spawns[0].Provider.Name() != "cursor" || fs.spawns[0].Model != "claude-opus-5" {
		t.Fatalf("spawned %+v", fs.spawns)
	}
}

// TestTheAgentsOwnDefaultIsNotACatalogModel (C15r, sol r31-c16 1): a cached
// catalog with a model whose id is `default` — the fake agent's has one — is
// listed as that model, and the agent's own default is still offered beside
// it, first, where it used to be hidden by it. Each is its own choice: the
// agent's own starts new sessions with no model (no --model), the catalog's
// with the model `default` (--model=default) — the one then marked current.
func TestTheAgentsOwnDefaultIsNotACatalogModel(t *testing.T) {
	catalog := ModelCatalog{
		Models:     []agent.ModelInfo{{ID: "default", Name: "Default"}, {ID: "composer", Name: "Composer"}},
		ObservedAt: sessNow.Add(-time.Hour),
	}
	m, fs := cmdList(t, 100, 30, map[string]ModelCatalog{"cursor": catalog})
	m = modelsLoaded(t, m, "/model defa")
	if got, want := cmdItems(m), []string{sessDefaultModel, "Default"}; !slices.Equal(got, want) {
		t.Fatalf("/model defa lists %v, want the agent's own default and the catalog's", got)
	}
	own, cat := cmdItem(t, m, sessDefaultModel), cmdItem(t, m, "Default")
	if own.Value != "model:" || own.Detail != sessDefaultDetail || cat.Value != "model:default" || cat.Detail != "default" {
		t.Fatalf("the agent's own %+v, the catalog's %+v", own, cat)
	}

	// The agent's own (the first row): no model.
	mine, _ := press(m, enter())
	if spec, _ := mine.sessNewSpec("/x"); spec.Model != "" || spec.Provider.Name() != "cursor" {
		t.Fatalf("the agent's own default chosen: %+v, want no model", spec)
	}
	if r := ruleOf(t, mine); !strings.HasSuffix(r, " · cursor · default") {
		t.Fatalf("the rule %q", r)
	}

	// The catalog's `default` (the row under it): the model `default`.
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = press(m, enter())
	if spec, _ := m.sessNewSpec("/x"); spec.Model != "default" {
		t.Fatalf("the catalog's default chosen: %+v, want the model default", spec)
	}
	m = modelsLoaded(t, m, "/model ")
	if it := cmdItem(t, m, "Default"); it.Note != sessCurrentNote {
		t.Fatalf("the catalog's default chosen is not marked: %+v", it)
	}
	if it := cmdItem(t, m, sessDefaultModel); it.Note != "" || it.Value != "model:" {
		t.Fatalf("the agent's own beside it: %+v", it)
	}
	m = clearInput(t, m)

	// And a dispatch spawns it by that id.
	hb := newHostBackend("new", homePath("projects/lumen"), &callLog{})
	fs.spawn = func(SpawnSpec) (roster.Ref, error) { return hostRef("new"), nil }
	fs.open = func(roster.Ref) (backend.Backend, error) { return hb, nil }
	m, _ = typeList(t, m, "@lu")
	m, _ = press(m, enter())
	m, _ = typeList(t, m, "go")
	_, cmd := press(m, enter())
	if msg := dispatched(t, mustCmd(t, cmd, "sessDispatch")); msg.out != dispatchAccepted {
		t.Fatalf("the dispatch: %v, %v", msg.out, msg.err)
	}
	if len(fs.spawns) != 1 || fs.spawns[0].Model != "default" {
		t.Fatalf("spawned %+v", fs.spawns)
	}
}

// TestAModelIsTakenAsTypedWithoutACatalog (§3.14): with no catalog cached —
// or none of its models matching — /model says so, and enter takes what is
// typed as the model id, which the next start applies as --model would.
func TestAModelIsTakenAsTypedWithoutACatalog(t *testing.T) {
	m, _ := cmdList(t, 100, 30, nil)
	m = modelsLoaded(t, m, "/model ")
	p := m.sessList.in.cmd
	if len(p.ans.Items) != 0 || p.ans.NoteErr || p.ans.Note != "no cursor catalog seen yet · enter uses the id as typed" {
		t.Fatalf("no catalog: %+v", p.ans)
	}
	if h := sessHint(m); !strings.HasPrefix(h, "type the model id · enter uses it") {
		t.Fatalf("the hint line %q", h)
	}
	if !strings.Contains(plainView(m), "no cursor catalog seen yet") {
		t.Fatalf("the popup does not say so:\n%s", plainView(m))
	}
	m, _ = typeList(t, m, "gpt-6-sol")
	m, _ = press(m, enter())
	if spec, _ := m.sessNewSpec("/x"); spec.Model != "gpt-6-sol" || spec.Provider.Name() != "cursor" {
		t.Fatalf("typed: %+v", spec)
	}
	if r := ruleOf(t, m); !strings.HasSuffix(r, " · cursor · gpt-6-sol") {
		t.Fatalf("the rule %q", r)
	}

	m2, _ := cmdList(t, 100, 30, map[string]ModelCatalog{"cursor": cursorCatalog})
	m2 = modelsLoaded(t, m2, "/model zzz")
	if p := m2.sessList.in.cmd; len(p.ans.Items) != 0 || p.ans.Note != sessNoModelNote {
		t.Fatalf("no match: %+v", p.ans)
	}
	m2, _ = press(m2, enter())
	if spec, _ := m2.sessNewSpec("/x"); spec.Model != "zzz" {
		t.Fatalf("typed past the catalog: %+v", spec)
	}
}

// TestTypedCommandLines (§3.14): `/provider <id>` and `/model <id>` taken
// with the popup put away; a provider the picker does not offer, and either
// command with nothing after it, is the hint line's error and changes
// nothing; `/model default` is the provider's own; /exit from the popup
// quits craze.
func TestTypedCommandLines(t *testing.T) {
	m, _ := cmdList(t, 100, 30, nil)
	typed := func(m Model, line string) Model {
		t.Helper()
		m, _ = typeList(t, m, line)
		if m.sessList.in.cmd.visible() {
			m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
		}
		if h := sessHint(m); !strings.Contains(h, "sets it for new sessions") {
			t.Fatalf("%q's hint line %q", line, h)
		}
		m, _ = press(m, enter())
		return m
	}
	m = typed(m, "/provider GROK")
	if spec, _ := m.sessNewSpec("/x"); spec.Provider.Name() != "grok" || spec.Model != "" {
		t.Fatalf("/provider GROK: %+v", spec)
	}
	m = typed(m, "/model grok-code-fast-1")
	if spec, _ := m.sessNewSpec("/x"); spec.Provider.Name() != "grok" || spec.Model != "grok-code-fast-1" {
		t.Fatalf("/model: %+v", spec)
	}
	m = typed(m, "/model default")
	if spec, _ := m.sessNewSpec("/x"); spec.Model != "" || !strings.HasSuffix(ruleOf(t, m), " · grok · default") {
		t.Fatalf("/model default: %+v, %q", spec, ruleOf(t, m))
	}
	for _, tc := range []struct{ line, note string }{
		{"/provider nope", "no provider nope: one of cursor, grok, native"},
		{"/provider ", sessNeedsProvider + "cursor, grok, native"},
		{"/model ", sessNeedsModel},
	} {
		before := m.sessPick
		m = typed(m, tc.line)
		if h := sessHint(m); h != tc.note || m.sessList.noteKind != sessNoteErr {
			t.Fatalf("%q: the hint %q (%v), want %q", tc.line, h, m.sessList.noteKind, tc.note)
		}
		if !reflect.DeepEqual(m.sessPick, before) {
			t.Fatalf("%q changed the pick", tc.line)
		}
		m = clearInput(t, m)
	}

	m, _ = typeList(t, m, "/ex")
	m, cmd := press(m, enter())
	if !m.quitting || cmd == nil {
		t.Fatal("/exit from the popup did not quit")
	}
}

// TestThePickLasts (§3.14): what /provider and /model chose holds across the
// list's openings and a switch to another session, until changed; an
// unstarted session opened from the list runs it.
func TestThePickLasts(t *testing.T) {
	m, _ := cmdList(t, 100, 30, nil)
	m, _ = typeList(t, m, "/provider grok")
	m, _ = press(m, enter())
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.sessList.open {
		t.Fatal("esc on an empty input did not leave the list")
	}
	m = openList(t, m)
	if r := ruleOf(t, m); !strings.Contains(r, " · grok · default") {
		t.Fatalf("reopened: the rule %q", r)
	}
	if next := m.withSession(sessionSeed{workspace: t.TempDir(), provider: "cursor"}); !reflect.DeepEqual(next.sessPick, m.sessPick) {
		t.Fatal("a switch dropped the pick")
	}
	m = listSnap(t, m, richSnapshot(os.Getenv("HOME"), m.hereKey()))
	m, _ = typeList(t, m, "@lu")
	m, _ = press(m, enter())
	m, _ = press(m, enter())
	if u := m.unstarted; u == nil || u.spec.Provider.Name() != "grok" || u.spec.Model != "" {
		t.Fatalf("the unstarted session: %+v", u)
	}
	if !strings.Contains(plainView(m), "new session · grok · ~/projects/lumen") {
		t.Fatalf("the unstarted session's band:\n%s", plainView(m))
	}
}

// TestTheNativeSeam: nativeModelChoices and nativeDefaultModel (the seam Plan
// 031 switched to its rules, §3.5/§3.6): only funded models, the remembered
// ones first in recent order with their rank and the rest by name; the
// default is StartModel's — the newest funded remembered model, else the
// table's, else the first funded alias — and an error when nothing is funded;
// with no files, the shipped catalog, unfunded. Neither reads or writes
// anything but the directory and environment it is given, and neither writes
// recent.json (§3.4).
func TestTheNativeSeam(t *testing.T) {
	t.Setenv("CRAZE_HOME", t.TempDir())
	// No files (plan 031 C2): the shipped catalog is the table, and with no key
	// anywhere — the package's TestMain scrubs the catalog's variables —
	// nothing in it is funded.
	empty := t.TempDir()
	if got, err := nativeModelChoices(empty, os.Getenv); err == nil {
		t.Fatalf("no files, the choices: %v; want an error, nothing is funded", got)
	}
	if md, err := nativeDefaultModel(empty, os.Getenv); err == nil {
		t.Fatalf("no files, the default: %+v; want an error, nothing is funded", md)
	}

	dir := nativeHome(t, "")
	// Only alpha's provider has a key: beta, unfunded, is not offered.
	got, err := nativeModelChoices(dir, os.Getenv)
	if want := []agent.ModelInfo{{ID: "alpha", Name: "Alpha One"}}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("the choices %v, %v; want %v", got, err, want)
	}
	if md, err := nativeDefaultModel(dir, os.Getenv); err != nil || md != (agent.ModelInfo{ID: "alpha", Name: "Alpha One"}) {
		t.Fatalf("an unfunded default: %+v, %v; want alpha, the first funded", md, err)
	}
	// The seam's environment is the one it is given, not the process's.
	funded := func(k string) string {
		if k == "CRAZE_C16_TEST_KEY" {
			return "sk-c16-env-0123456789abcdef"
		}
		return ""
	}
	got, err = nativeModelChoices(dir, funded)
	if want := []agent.ModelInfo{{ID: "alpha", Name: "Alpha One"}, {ID: "beta", Name: "Beta"}}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("both funded, the choices %v, %v; want %v", got, err, want)
	}
	if md, err := nativeDefaultModel(dir, funded); err != nil || md.ID != "beta" {
		t.Fatalf("a funded default: %+v, %v; want beta", md, err)
	}
	if md, err := nativeDefaultModel(dir, os.Getenv); err != nil || md.ID != "alpha" {
		t.Fatalf("the process environment leaked into the seam's: %+v, %v", md, err)
	}

	// The memory: beta remembered puts it first, ranked, and makes it the
	// start model while funded; unfunded, it is neither offered nor started.
	if _, err := os.Stat(filepath.Join(dir, modeltable.RecentFile)); !os.IsNotExist(err) {
		t.Fatalf("recent.json exists before anything wrote it: %v", err)
	}
	rem := modeltable.RecentEntry{Alias: "beta", Provider: "unfunded", WireModel: "beta-wire"}
	if err := modeltable.Remember(dir, rem, time.Now()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, modeltable.RecentFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err = nativeModelChoices(dir, funded)
	if want := []agent.ModelInfo{{ID: "beta", Name: "Beta", Recent: 1}, {ID: "alpha", Name: "Alpha One"}}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("remembered, the choices %v, %v; want %v", got, err, want)
	}
	if md, err := nativeDefaultModel(dir, funded); err != nil || md != (agent.ModelInfo{ID: "beta", Name: "Beta"}) {
		t.Fatalf("remembered and funded, the default: %+v, %v; want beta", md, err)
	}
	got, err = nativeModelChoices(dir, os.Getenv)
	if want := []agent.ModelInfo{{ID: "alpha", Name: "Alpha One"}}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("remembered but unfunded, the choices %v, %v; want %v (no gap in the ranks)", got, err, want)
	}
	if md, err := nativeDefaultModel(dir, os.Getenv); err != nil || md.ID != "alpha" {
		t.Fatalf("remembered but unfunded, the default: %+v, %v; want alpha", md, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the seam changed recent.json: %v\n%s\n%s", err, before, after)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "recent") && e.Name() != modeltable.RecentFile && e.Name() != modeltable.RecentFile+".lock" {
			t.Fatalf("the seam left %s", e.Name())
		}
	}

	dir = nativeHome(t, "alpha")
	if md, err := nativeDefaultModel(dir, os.Getenv); err != nil || md.ID != "alpha" {
		t.Fatalf("the default alpha: %+v, %v", md, err)
	}
	if _, err := os.Stat(filepath.Join(dir, modeltable.RecentFile)); !os.IsNotExist(err) {
		t.Fatalf("the seam wrote recent.json: %v", err)
	}

	dir = nativeHome(t, "beta")
	providers := filepath.Join(dir, modeltable.ProvidersFile)
	raw, _ := os.ReadFile(providers)
	unfunded := strings.Replace(string(raw), `api_key = "sk-c16-test-0123456789abcdef"`+"\n", "", 1)
	if err := os.WriteFile(providers, []byte(unfunded), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeDefaultModel(dir, os.Getenv); !errors.Is(err, errNativeNothingFunded) {
		t.Fatalf("nothing funded, the default: %v", err)
	}
	if got, err := nativeModelChoices(dir, os.Getenv); !errors.Is(err, errNativeNothingFunded) {
		t.Fatalf("nothing funded, the choices: %v, %v", got, err)
	}
}

// TestTheListsNativeModelFollowsTheMemory (plan 031 §3.4-§3.6, §3.12): through
// the Model's own seams (Config.NativeDir, Config.Getenv as m.nativeDir and
// m.nativeEnv), the list's native /model offers only funded models, the
// remembered one first, with no label; /provider native resets to the start
// model — the remembered one when funded — and says so when nothing is
// funded; and the list never writes recent.json.
func TestTheListsNativeModelFollowsTheMemory(t *testing.T) {
	m, _ := cmdList(t, 100, 30, nil)
	dir := nativeHome(t, "")
	env := map[string]string{}
	m.nativeDir, m.nativeEnv = dir, func(k string) string { return env[k] }
	list := func(m Model) []string {
		m, _ = typeList(t, m, "/provider native")
		m, _ = press(m, enter())
		m = modelsLoaded(t, m, "/model ")
		return cmdItems(m)
	}
	if got := list(m); !slices.Equal(got, []string{"Alpha One"}) {
		t.Fatalf("beta has no key; /model lists %v, want only Alpha One", got)
	}
	env["CRAZE_C16_TEST_KEY"] = "sk-c16-env-0123456789abcdef"
	if got := list(m); !slices.Equal(got, []string{"Alpha One", "Beta"}) {
		t.Fatalf("nothing remembered; /model lists %v", got)
	}
	if err := modeltable.Remember(dir, modeltable.RecentEntry{Alias: "beta", Provider: "unfunded", WireModel: "beta-wire"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, modeltable.RecentFile))
	if got := list(m); !slices.Equal(got, []string{"Beta", "Alpha One"}) {
		t.Fatalf("beta remembered; /model lists %v, want Beta first", got)
	}
	// /provider native resets to the start model: the remembered one.
	m, _ = typeList(t, m, "/provider native")
	m, cmd := press(m, enter())
	tm, _ := m.Update(runWatched(t, mustCmd(t, cmd, "readNativeDefault")))
	m = tm.(Model)
	if spec, _ := m.sessNewSpec("/somewhere"); spec.Model != "beta" {
		t.Fatalf("the remembered, funded model is the start model: %+v", spec)
	}
	// Nothing funded: /provider native says why and leaves no --model.
	env["CRAZE_C16_TEST_KEY"] = ""
	providers := filepath.Join(dir, modeltable.ProvidersFile)
	raw, _ := os.ReadFile(providers)
	if err := os.WriteFile(providers, []byte(strings.Replace(string(raw), `api_key = "sk-c16-test-0123456789abcdef"`+"\n", "", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _ = typeList(t, m, "/provider native")
	m, cmd = press(m, enter())
	tm, _ = m.Update(runWatched(t, mustCmd(t, cmd, "readNativeDefault")))
	m = tm.(Model)
	if h := sessHint(m); !strings.Contains(h, "no model provider has an API key") || m.sessList.noteKind != sessNoteWarn {
		t.Fatalf("nothing funded: the hint %q (%v)", h, m.sessList.noteKind)
	}
	m = modelsLoaded(t, m, "/model ")
	if n := m.sessList.in.cmd.ans.Note; !strings.Contains(n, "native has no models: no model provider has an API key") {
		t.Fatalf("nothing funded, /model says %q", n)
	}
	after, _ := os.ReadFile(filepath.Join(dir, modeltable.RecentFile))
	if !bytes.Equal(before, after) {
		t.Fatalf("the list wrote recent.json:\n%s\n%s", before, after)
	}
}

// TestFrameGoldenSessionsSlash (§3.17): the `/` popup at 100×30 and 80×24 —
// the list's commands, and /model over a cached catalog with its age.
func TestFrameGoldenSessionsSlash(t *testing.T) {
	for _, size := range []struct{ cols, rows int }{{100, 30}, {80, 24}} {
		suffix := fmt.Sprintf("-%dx%d", size.cols, size.rows)
		m, _ := cmdList(t, size.cols, size.rows, map[string]ModelCatalog{"cursor": cursorCatalog})

		slash, _ := typeList(t, m, "/")
		assertFrameGolden(t, "sessions-new-slash"+suffix, size.cols, size.rows, plainView(slash),
			[]string{"commands", "/provider", "provider for new sessions", "/model", "Grok", "/exit", "❯ /",
				"tab/enter use it"}, []string{"where should it run?"})

		models := modelsLoaded(t, m, "/model ")
		assertFrameGolden(t, "sessions-new-model"+suffix, size.cols, size.rows, plainView(models),
			[]string{"cursor models for new sessions · last seen 3h ago", "default", "Grok 4.6", "grok-4.6",
				"Composer 2.5", "❯ /model"}, nil)
	}
}

// TestEffortAndFastAreRevealedByPrefix (plan 032 §3.11, C17): /effort and
// /fast are never in the bare `/` popup — it lists /provider, /model and
// /exit as it always did — and are listed once the typed command word is a
// prefix of theirs and of no older command's: `/e` is /exit's alone (it starts
// /exit too), `/ef` reveals /effort, `/f` and `/fa` /fast; typed in full each
// is listed alone, case folded. A word past them opens nothing; with a space
// each lists its values, `default` first and current.
func TestEffortAndFastAreRevealedByPrefix(t *testing.T) {
	m, _ := cmdList(t, 100, 30, nil)
	for _, tc := range []struct {
		typed string
		want  []string
	}{
		{"/", []string{"/provider", "/model", "/exit"}},
		{"/e", []string{"/exit"}},
		{"/ex", []string{"/exit"}},
		{"/ef", []string{"/effort"}},
		{"/effort", []string{"/effort"}},
		{"/EFF", []string{"/effort"}},
		{"/f", []string{"/fast"}},
		{"/fa", []string{"/fast"}},
		{"/fast", []string{"/fast"}},
	} {
		mm, _ := typeList(t, m, tc.typed)
		if got := cmdItems(mm); !slices.Equal(got, tc.want) {
			t.Fatalf("%q lists %v, want %v", tc.typed, got, tc.want)
		}
	}
	for _, line := range []string{"/efforts", "/fastest", "/fix the build"} {
		if mm, _ := typeList(t, m, line); mm.sessList.in.cmd.visible() {
			t.Fatalf("%q opened the popup: %v", line, cmdItems(mm))
		}
	}
	mm, _ := typeList(t, m, "/ef")
	if it := cmdItem(t, mm, "/effort"); it.Detail != sessEffortTitle || it.Note != sessDefaultModel {
		t.Fatalf("/effort's row %+v", it)
	}
	mm, _ = typeList(t, m, "/f")
	if it := cmdItem(t, mm, "/fast"); it.Detail != sessFastTitle || it.Note != sessDefaultModel {
		t.Fatalf("/fast's row %+v", it)
	}
	// enter on the command writes it with the space that lists its values.
	mm, _ = press(mm, enter())
	if v, _ := inputOf(mm); v != "/fast " {
		t.Fatalf("enter on /fast left %q", v)
	}
	if got := cmdItems(mm); !slices.Equal(got, []string{"default", "on", "off"}) {
		t.Fatalf("/fast lists %v", got)
	}
	mm, _ = typeList(t, m, "/effort ")
	if got := cmdItems(mm); !slices.Equal(got, []string{"default", "low", "medium", "high", "xhigh"}) {
		t.Fatalf("/effort lists %v", got)
	}
	if it := cmdItem(t, mm, "default"); it.Note != sessCurrentNote || it.Detail != sessSettingDetail {
		t.Fatalf("/effort's default row %+v", it)
	}
	if mm.sessList.in.cmd.ans.Title != sessEffortTitle {
		t.Fatalf("/effort's title %q", mm.sessList.in.cmd.ans.Title)
	}
	mm, _ = typeList(t, mm, "x")
	if got := cmdItems(mm); !slices.Equal(got, []string{"xhigh"}) {
		t.Fatalf("/effort x lists %v", got)
	}
	mm, _ = typeList(t, m, "/effort max")
	if p := mm.sessList.in.cmd; len(p.ans.Items) != 0 || !p.ans.NoteErr || p.ans.Note != "no effort max: one of default, low, medium, high, xhigh" {
		t.Fatalf("/effort max: %+v", p.ans)
	}
}

// TestChoosingEffortAndFast (plan 032 §3.11, C17): a value chosen from the
// popup — enter or tab — or typed in full sets the effort or fast mode of the
// next session the list starts, the rule and the hint line name it, it is
// marked current the next time it is offered, and `default` clears it. A
// value not offered, and either command with none, is the hint line's error
// and changes nothing.
func TestChoosingEffortAndFast(t *testing.T) {
	m, _ := cmdList(t, 100, 30, nil)
	before := ruleOf(t, m)
	if !strings.HasSuffix(before, " · cursor · Grok") {
		t.Fatalf("the rule %q", before)
	}
	spec := func(m Model) SpawnSpec {
		t.Helper()
		s, err := m.sessNewSpec("/x")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	m, _ = typeList(t, m, "/effort hi")
	m, _ = press(m, enter())
	if s := spec(m); s.Effort != "high" || s.Fast != nil {
		t.Fatalf("after /effort high: %+v", s)
	}
	if r := ruleOf(t, m); !strings.HasSuffix(r, " · cursor · Grok · high") {
		t.Fatalf("after /effort high: the rule %q", r)
	}
	if h := sessHint(m); h != "new sessions use cursor · Grok · high" {
		t.Fatalf("after /effort high: the hint %q", h)
	}
	if v, _ := inputOf(m); v != "" {
		t.Fatalf("a choice left %q in the input", v)
	}
	m, _ = typeList(t, m, "/effort ")
	if it := cmdItem(t, m, "high"); it.Note != sessCurrentNote {
		t.Fatalf("the effort chosen is not marked: %+v", it)
	}
	if it := cmdItem(t, m, "default"); it.Note != "" {
		t.Fatalf("default is still marked: %+v", it)
	}
	m = clearInput(t, m)
	m, _ = typeList(t, m, "/f")
	if it := cmdItem(t, m, "/fast"); it.Note != sessDefaultModel {
		t.Fatalf("/fast's row %+v", it)
	}
	m = clearInput(t, m)

	m, _ = typeList(t, m, "/fast o")
	if got := cmdItems(m); !slices.Equal(got, []string{"on", "off"}) {
		t.Fatalf("/fast o lists %v", got)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyTab}) // tab uses it too
	if s := spec(m); s.Effort != "high" || s.Fast == nil || !*s.Fast {
		t.Fatalf("after /fast on: %+v", s)
	}
	if r := ruleOf(t, m); !strings.HasSuffix(r, " · cursor · Grok · high · fast") {
		t.Fatalf("after /fast on: the rule %q", r)
	}

	typed := func(m Model, line string) Model {
		t.Helper()
		m, _ = typeList(t, m, line)
		if m.sessList.in.cmd.visible() {
			m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
		}
		if h := sessHint(m); !strings.Contains(h, "sets it for new sessions") {
			t.Fatalf("%q's hint line %q", line, h)
		}
		m, _ = press(m, enter())
		return m
	}
	m = typed(m, "/fast OFF")
	if s := spec(m); s.Fast == nil || *s.Fast {
		t.Fatalf("after /fast OFF: %+v", s)
	}
	if r := ruleOf(t, m); !strings.HasSuffix(r, " · cursor · Grok · high · no fast") {
		t.Fatalf("after /fast off: the rule %q", r)
	}
	m = typed(m, "/effort XHigh")
	if s := spec(m); s.Effort != "xhigh" {
		t.Fatalf("after /effort XHigh: %+v", s)
	}

	for _, tc := range []struct{ line, note string }{
		{"/effort max", "no effort max: one of default, low, medium, high, xhigh"},
		{"/effort ", "/effort needs a value: default, low, medium, high, xhigh"},
		{"/fast maybe", "no fast maybe: one of default, on, off"},
		{"/fast ", "/fast needs a value: default, on, off"},
	} {
		was := m.sessPick
		m = typed(m, tc.line)
		if h := sessHint(m); h != tc.note || m.sessList.noteKind != sessNoteErr {
			t.Fatalf("%q: the hint %q (%v), want %q", tc.line, h, m.sessList.noteKind, tc.note)
		}
		if !reflect.DeepEqual(m.sessPick, was) {
			t.Fatalf("%q changed the pick", tc.line)
		}
		m = clearInput(t, m)
	}

	// default clears each: typed, and chosen from the popup.
	m = typed(m, "/effort default")
	if s := spec(m); s.Effort != "" || s.Fast == nil || *s.Fast {
		t.Fatalf("after /effort default: %+v", s)
	}
	m, _ = typeList(t, m, "/fast def")
	m, _ = press(m, enter())
	if s := spec(m); s.Effort != "" || s.Fast != nil {
		t.Fatalf("after /fast default: %+v", s)
	}
	if r := ruleOf(t, m); r != before {
		t.Fatalf("both cleared, the rule %q; want %q as before", r, before)
	}
	if h := sessHint(m); h != "new sessions use cursor · Grok" {
		t.Fatalf("both cleared, the hint %q", h)
	}
}

// TestTheRuleNamesEffortAndFast (plan 032 §3.11, C17): the rule over the
// input appends ` · <effort>` when an effort is set, ` · fast` when fast mode
// is on and ` · no fast` when off — nothing when neither is set, so the rule
// is what it was — and the hint line's `new sessions use …` says the same.
func TestTheRuleNamesEffortAndFast(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		effort string
		fast   *bool
		tail   string
	}{
		{"", nil, ""},
		{"high", nil, " · high"},
		{"", &on, " · fast"},
		{"", &off, " · no fast"},
		{"xhigh", &on, " · xhigh · fast"},
		{"low", &off, " · low · no fast"},
	} {
		m, _ := cmdList(t, 100, 30, nil)
		base := ruleOf(t, m)
		m.sessPick.effort, m.sessPick.fast = tc.effort, tc.fast
		if r := ruleOf(t, m); r != base+tc.tail {
			t.Fatalf("effort %q, fast %v: the rule %q, want %q", tc.effort, tc.fast, r, base+tc.tail)
		}
		if n := m.sessUseNote(); n != "new sessions use cursor · Grok"+tc.tail {
			t.Fatalf("effort %q, fast %v: the note %q", tc.effort, tc.fast, n)
		}
	}
}

// TestEffortAndFastReachTheDispatch (plan 032 §3.11, A13): a session the
// list starts with `/effort high` and `/fast on` set is spawned with
// SpawnSpec.Effort "high" and Fast on — the spec's own copy, never the
// pick's; the setting outlives a /provider choice, which resets only the
// model, and the list's closing and reopening; and an unstarted session
// opened from the list carries it too.
func TestEffortAndFastReachTheDispatch(t *testing.T) {
	m, fs := cmdList(t, 100, 30, nil)
	m, _ = typeList(t, m, "/effort high")
	m, _ = press(m, enter())
	m, _ = typeList(t, m, "/fast on")
	m, _ = press(m, enter())

	hb := newHostBackend("new", homePath("projects/lumen"), &callLog{})
	fs.spawn = func(SpawnSpec) (roster.Ref, error) { return hostRef("new"), nil }
	fs.open = func(roster.Ref) (backend.Backend, error) { return hb, nil }
	m, _ = typeList(t, m, "@lu")
	m, _ = press(m, enter())
	m, _ = typeList(t, m, "tidy the changelog")
	m, cmd := press(m, enter())
	msg := dispatched(t, mustCmd(t, cmd, "sessDispatch"))
	if msg.out != dispatchAccepted {
		t.Fatalf("the dispatch: %v, %v", msg.out, msg.err)
	}
	if len(fs.spawns) != 1 || fs.spawns[0].Effort != "high" || fs.spawns[0].Fast == nil || !*fs.spawns[0].Fast {
		t.Fatalf("spawned %+v", fs.spawns)
	}
	if fs.spawns[0].Fast == m.sessPick.fast {
		t.Fatal("the spec shares the pick's fast setting")
	}
	tm, _ := m.Update(msg)
	m = tm.(Model)

	// /provider resets the model, not the settings.
	m, _ = typeList(t, m, "/provider grok")
	m, _ = press(m, enter())
	if s, _ := m.sessNewSpec("/x"); s.Provider.Name() != "grok" || s.Effort != "high" || s.Fast == nil || !*s.Fast {
		t.Fatalf("after /provider grok: %+v", s)
	}
	if h := sessHint(m); h != "new sessions use grok · default · high · fast" {
		t.Fatalf("after /provider grok: the hint %q", h)
	}

	// Across the list's closing and reopening, to the unstarted session.
	m = clearInput(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.sessList.open {
		t.Fatal("esc on an empty input did not leave the list")
	}
	m = openList(t, m)
	if r := ruleOf(t, m); !strings.HasSuffix(r, " · grok · default · high · fast") {
		t.Fatalf("reopened: the rule %q", r)
	}
	m = listSnap(t, m, richSnapshot(os.Getenv("HOME"), m.hereKey()))
	m, _ = typeList(t, m, "@lu")
	m, _ = press(m, enter())
	m, _ = press(m, enter())
	if u := m.unstarted; u == nil || u.spec.Effort != "high" || u.spec.Fast == nil || !*u.spec.Fast {
		t.Fatalf("the unstarted session: %+v", u)
	}
}

// TestFrameGoldenSessionsEffortAndFast (plan 032 §3.17, C17): /effort revealed
// by `/ef`, /fast by `/fa`, and the rule with both set, at 100×30 and 80×24.
func TestFrameGoldenSessionsEffortAndFast(t *testing.T) {
	for _, size := range []struct{ cols, rows int }{{100, 30}, {80, 24}} {
		suffix := fmt.Sprintf("-%dx%d", size.cols, size.rows)
		m, _ := cmdList(t, size.cols, size.rows, map[string]ModelCatalog{"cursor": cursorCatalog})

		effort, _ := typeList(t, m, "/ef")
		assertFrameGolden(t, "sessions-new-effort"+suffix, size.cols, size.rows, plainView(effort),
			[]string{"commands", "/effort", sessEffortTitle, "❯ /ef", "tab/enter use it"},
			[]string{"/provider", "/exit", "/fast"})

		fast, _ := typeList(t, m, "/fa")
		assertFrameGolden(t, "sessions-new-fast"+suffix, size.cols, size.rows, plainView(fast),
			[]string{"commands", "/fast", sessFastTitle, "❯ /fa", "tab/enter use it"},
			[]string{"/provider", "/exit", "/effort"})

		set, _ := typeList(t, m, "/effort high")
		set, _ = press(set, enter())
		set, _ = typeList(t, set, "/fast on")
		set, _ = press(set, enter())
		assertFrameGolden(t, "sessions-new-settings"+suffix, size.cols, size.rows, plainView(set),
			[]string{"new session → ~/projects/craze · cursor · Grok · high · fast ─", "new sessions use cursor · Grok · high · fast"},
			nil)
	}
}

// TestNativeNothingFundedNamesTheChatGPTPlan is SF-143's TUI half: the
// session list's nothing-funded note says what craze auth login does — an
// API key, or the ChatGPT plan's sign-in — as craze providers' fix does.
func TestNativeNothingFundedNamesTheChatGPTPlan(t *testing.T) {
	want := `no model provider has an API key — run craze auth login (an API key, or "craze auth login chatgpt" for a ChatGPT plan), or set its API key variable`
	if got := errNativeNothingFunded.Error(); got != want {
		t.Fatalf("%q\nwant %q", got, want)
	}
}
