package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/sessions"
)

// The session list's input (plan 030 §3.13's first two bullets, §3.15, C14):
// its frames against a fake Sessions that can start sessions — the list's
// input exists only then (SessionStarter), so the list's PR 2 frames, made
// against a fake that cannot, stay as they were — and its rules: the keys it
// takes and leaves, the rule's target, the leading token's binding and its
// resolution, the `@` directory source's order, notes and filtering,
// browsing, and the listing results a later query, a close or a switch left
// behind.

// startSessions is a fakeSessions that can start sessions: the list it
// configures has its input. RecentDirs answers recent, ModelCatalog catalogs
// (C16: the catalog cache, by provider id; none by default).
type startSessions struct {
	*fakeSessions
	mu       sync.Mutex
	recent   []sessions.RecentDir
	recents  int
	catalogs map[string]ModelCatalog
	catReads []string
}

var _ SessionStarter = (*startSessions)(nil)

func (f *startSessions) RecentDirs(n int) ([]sessions.RecentDir, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recents++
	if n > 0 && len(f.recent) > n {
		return slices.Clone(f.recent[:n]), nil
	}
	return slices.Clone(f.recent), nil
}

func (f *startSessions) ModelCatalog(provider string) (ModelCatalog, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.catReads = append(f.catReads, provider)
	c, ok := f.catalogs[provider]
	return c, ok
}

func (f *startSessions) recentCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recents
}

// newSessDirs are the directories under a test HOME the list's input works
// in: the mockup's projects, a hidden one, a file, and a few more levels.
var newSessDirs = []string{
	"projects/craze/cmd", "projects/craze/docs", "projects/craze/internal", "projects/craze/.git",
	"projects/lumen", "projects/roost", "projects/shed", "projects/spendwise", "projects/folio",
	"projects/prox", "projects/pharos", "projects/tapper", "projects/.cache", "notes/2026",
}

// newSessModel is sessModel with a Sessions that can start sessions: the
// session runs in ~/projects/craze of a test HOME whose tree is newSessDirs,
// and the recent directories are the mockup's history. Its clock is pinned at
// sessNow and its spinner at frame 0.
func newSessModel(t *testing.T, cols, rows int) (Model, *startSessions, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	home := os.Getenv("HOME")
	for _, d := range newSessDirs {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "projects", "README"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := func(name string) string { return filepath.Join(home, "projects", name) }
	fs := &startSessions{fakeSessions: &fakeSessions{}, recent: []sessions.RecentDir{
		{Dir: p("pharos"), UsedAt: sessNow.Add(-2 * time.Hour)},
		{Dir: p("spendwise"), UsedAt: sessNow.Add(-3 * time.Hour)},
		{Dir: filepath.Join(home, "notes"), UsedAt: sessNow.Add(-5 * 24 * time.Hour)},
		{Dir: p("folio"), UsedAt: sessNow.Add(-6 * 24 * time.Hour)},
		{Dir: p("tapper"), UsedAt: sessNow.Add(-14 * 24 * time.Hour)},
	}}
	stub := NewStub()
	m := New(Config{Session: stub, Theme: "tokyo-night", Workspace: p("craze"), Model: "grok", Yolo: true, Sessions: fs})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: cols, Height: rows})
	m = startedLikeInit(t, tm.(Model))
	m.clock = func() time.Time { return sessNow }
	return m, fs, stub
}

// newList is m's list opened on richSnapshot, with the recent directories
// read — as its opening's commands would deliver them.
func newList(t *testing.T, m Model, fs *startSessions) Model {
	t.Helper()
	m = openList(t, m)
	if !m.sessList.in.on {
		t.Fatal("a Sessions that can start sessions opened a list with no input")
	}
	m = listSnap(t, m, richSnapshot(os.Getenv("HOME"), m.hereKey()))
	return listRecents(t, m, fs)
}

// listRecents hands the open list the recent directories, as its read would.
func listRecents(t *testing.T, m Model, fs *startSessions) Model {
	t.Helper()
	dirs, _ := fs.RecentDirs(sessRecentDirs)
	tm, _ := m.Update(sessRecentsMsg{gen: m.sessList.gen, dirs: dirs})
	return tm.(Model)
}

// typeList types text into the list's input a key at a time, through Update,
// and answers the last command a key's handler returned: the listing a path
// token's popup awaits starts at the key that first names its directory
// (`@.` for `@./`), and a later key naming the same one starts none. (Update
// batches the tick chain last, so the handler's command is a batch's first;
// the chain is live before any key here, and none re-arms it.)
func typeList(t *testing.T, m Model, text string) (Model, tea.Cmd) {
	t.Helper()
	var last tea.Cmd
	for _, r := range text {
		var cmd tea.Cmd
		m, cmd = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		if cmd != nil {
			last = cmd
		}
	}
	return m, last
}

// listLoad runs cmd — the listing a path token's popup started — within one
// step, and answers its result.
func listLoad(t *testing.T, cmd tea.Cmd) completeLoadedMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("no listing started")
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- runCmd(cmd) }()
	select {
	case msg := <-ch:
		lm, ok := msg.(completeLoadedMsg)
		if !ok {
			t.Fatalf("the command answered %T, not a listing", msg)
		}
		return lm
	case <-time.After(completeStep):
		t.Fatalf("the listing did not answer within %v", completeStep)
		return completeLoadedMsg{}
	}
}

// browsed types text (a path token) into the list's input and delivers the
// listing it starts.
func browsed(t *testing.T, m Model, text string) Model {
	t.Helper()
	m, cmd := typeList(t, m, text)
	tm, _ := m.Update(listLoad(t, cmd))
	return tm.(Model)
}

// inputOf is the list's input line and its cursor as a byte offset.
func inputOf(m Model) (string, int) {
	v := m.sessList.in.ti.Value()
	return v, sessByteCursor(v, m.sessList.in.ti.Position())
}

// ruleOf is the frame's row naming the target.
func ruleOf(t *testing.T, m Model) string {
	t.Helper()
	for _, ln := range strings.Split(plainView(m), "\n") {
		if strings.Contains(ln, sessNewRuleLead) || strings.Contains(ln, "no directory") || strings.Contains(ln, " names ") {
			if strings.HasSuffix(strings.TrimRight(ln, " "), " ─") {
				return strings.TrimSpace(strings.Trim(ln, "─ "))
			}
		}
	}
	t.Fatalf("no target rule:\n%s", plainView(m))
	return ""
}

// home is the test HOME's path for a ~-relative name.
func homePath(rel string) string { return filepath.Join(os.Getenv("HOME"), rel) }

// TestFrameGoldenSessionsNew (§3.17): the list with its input at 100×30 and
// 80×24 — a prompt typed, the rule naming the selected row's directory; the
// `@` popup's names (here, the running directories, the recent ones); a path
// browsed; a picked, bound token with a prompt after it — and, at 80×24, the
// list with nothing else running, whose note points at the input.
func TestFrameGoldenSessionsNew(t *testing.T) {
	for _, size := range []struct{ cols, rows int }{{100, 30}, {80, 24}} {
		suffix := fmt.Sprintf("-%dx%d", size.cols, size.rows)
		m, fs, _ := newSessModel(t, size.cols, size.rows)
		m = newList(t, m, fs)

		typed := selectKey(t, m, runKey("lumen"))
		typed, _ = typeList(t, typed, "tidy the changelog")
		assertFrameGolden(t, "sessions-new"+suffix, size.cols, size.rows, plainView(typed),
			[]string{"new session → ~/projects/lumen", "❯ tidy the changelog", "enter starts it in the background · esc clear"},
			[]string{"where should it run?"})

		at, _ := typeList(t, m, "@")
		assertFrameGolden(t, "sessions-new-at"+suffix, size.cols, size.rows, plainView(at),
			[]string{"where should it run?", "craze", "here · 3 running", "1 running", "used 2h ago", "↓ 2 more",
				"new session → ~/projects/craze", "tab/enter use it"}, nil)

		browse := browsed(t, m, "@~/projects/")
		assertFrameGolden(t, "sessions-new-browse"+suffix, size.cols, size.rows, plainView(browse),
			[]string{"folders in ~/projects", "craze/", "spendwise/", "↓ 1 more", "tab open folder"}, []string{".cache", "README"})

		bound, _ := typeList(t, m, "@lu")
		bound, _ = press(bound, enter())
		bound, _ = typeList(t, bound, "add a search box")
		assertFrameGolden(t, "sessions-new-bound"+suffix, size.cols, size.rows, plainView(bound),
			[]string{"new session → ~/projects/lumen", "❯ @lumen add a search box"}, []string{"where should it run?"})
	}

	m, fs, _ := newSessModel(t, 80, 24)
	m = openList(t, m)
	here := m.hereKey()
	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{
		answeredInc(here.id, here.inc, "write the v0.1.0 release notes", "native", homePath("projects/craze"), 4*time.Minute, nil),
	}})
	m = listRecents(t, m, fs)
	assertFrameGolden(t, "sessions-new-empty-80x24", 80, 24, plainView(m),
		[]string{sessEmptyNoteInput, sessInputPlaceholder, "new session → ~/projects/craze"}, nil)
}

// TestTheListHasAnInputOnlyWhenItCanStartSessions: a Sessions that cannot
// start sessions (PR 2's) draws no input and takes no typing — its frame is
// PR 2's — and one that can reads its recent directories once per opening.
func TestTheListHasAnInputOnlyWhenItCanStartSessions(t *testing.T) {
	m, _, _ := sessModel(t, 80, 24)
	m = richList(t, m)
	before := plainView(m)
	typed, _ := typeList(t, m, "x@")
	if typed.sessList.in.on || plainView(typed) != before || strings.Contains(before, sessNewRuleLead) {
		t.Fatalf("a list that cannot start sessions has an input:\n%s", plainView(typed))
	}

	n, fs, _ := newSessModel(t, 80, 24)
	opened, cmd := press(n, tea.KeyMsg{Type: tea.KeyLeft})
	if !opened.sessList.in.on || !strings.Contains(plainView(opened), sessInputPlaceholder) {
		t.Fatalf("no input:\n%s", plainView(opened))
	}
	// The opening's commands: the roster's read — answered by a snapshot
	// pushed first — and the recent directories'.
	fs.rosters[0].ch <- roster.Snapshot{}
	var got *sessRecentsMsg
	for _, msg := range runAll(t, cmd) {
		if r, ok := msg.(sessRecentsMsg); ok {
			got = &r
		}
	}
	if got == nil || got.gen != opened.sessList.gen || len(got.dirs) != 5 || fs.recentCalls() != 1 {
		t.Fatalf("the recent directories' read: %+v, %d calls", got, fs.recentCalls())
	}
	// One from an earlier opening is dropped.
	stale := *got
	stale.gen--
	if tm, _ := opened.Update(stale); len(tm.(Model).sessList.in.recents) != 0 {
		t.Fatal("recent directories read for an earlier opening were taken")
	}
	if tm, _ := opened.Update(*got); len(tm.(Model).sessList.in.recents) != 5 {
		t.Fatal("the opening's recent directories were not taken")
	}
}

// runAll runs cmd and every member of the batch it is — none of them a
// timer — each within one step, and answers their messages.
func runAll(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(completeStep):
		t.Fatalf("a command did not answer within %v", completeStep)
	}
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var out []tea.Msg
	for _, c := range batch {
		out = append(out, runAll(t, c)...)
	}
	return out
}

// TestTheInputTakesTheKeys (§3.13): printable keys type; ↑/↓ move the list
// while no popup is up and the popup's selection while one is; enter with
// nothing typed opens the selected row; esc clears, then leaves; ← leaves
// only when nothing is typed and moves the cursor otherwise, as → opens the
// row only then; tab does nothing without a popup; ctrl+x, ctrl+s and ctrl+d
// are the list's; a paste lands in the input; a later `@` opens nothing.
func TestTheInputTakesTheKeys(t *testing.T) {
	m, fs, _ := newSessModel(t, 100, 30)
	m = newList(t, m, fs)
	here := m.sessList.sel

	m, _ = press(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.sessList.sel == here {
		t.Fatal("↓ with nothing typed did not move the list")
	}
	m, _ = typeList(t, m, "jk q")
	if v, _ := inputOf(m); v != "jk q" || !m.sessList.open {
		t.Fatalf("typed %q, open %v", v, m.sessList.open)
	}
	// ← and → with something typed are the cursor's.
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyLeft})
	if _, cur := inputOf(m); cur != 2 || !m.sessList.open {
		t.Fatalf("← ← : cursor %d, open %v", cur, m.sessList.open)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyRight})
	if _, cur := inputOf(m); cur != 3 || len(fs.opened()) != 0 {
		t.Fatalf("→ : cursor %d, opens %v", cur, fs.opened())
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyTab})
	if v, _ := inputOf(m); v != "jk q" {
		t.Fatalf("tab changed the input to %q", v)
	}
	// ↑/↓ still move the list with something typed and no popup.
	was := m.sessList.sel
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.sessList.sel == was {
		t.Fatal("↑ with text typed did not move the list")
	}
	// ctrl+s regroups, whatever is typed.
	if g, _ := press(m, tea.KeyMsg{Type: tea.KeyCtrlS}); !g.sessList.byDir {
		t.Fatal("ctrl+s did not regroup")
	}
	// A paste asked for in the input lands there, folded onto one line.
	pasted, _ := m.Update(pasteMsg{text: " two\nlines", shownGen: m.shownGen, list: true})
	if v, _ := inputOf(pasted.(Model)); v != "jk  two linesq" {
		t.Fatalf("pasted into %q", v)
	}
	// A later `@` is prompt text: no popup.
	later, _ := press(m, tea.KeyMsg{Type: tea.KeyEnd})
	later, _ = typeList(t, later, " @lu")
	if later.sessList.in.at.visible() {
		t.Fatal("an @ after the first word opened the popup")
	}
	// esc clears; the next leaves.
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if v, _ := inputOf(m); v != "" || !m.sessList.open {
		t.Fatalf("esc: input %q, open %v", v, m.sessList.open)
	}
	if left, _ := press(m, tea.KeyMsg{Type: tea.KeyEsc}); left.sessList.open {
		t.Fatal("esc on an empty input did not leave")
	}
	if left, _ := press(m, tea.KeyMsg{Type: tea.KeyLeft}); left.sessList.open {
		t.Fatal("← on an empty input did not leave")
	}
	// enter on an empty input opens the selected row.
	sel := selectKey(t, m, runKey("roost"))
	sel, _ = press(sel, enter())
	if sel.sessList.dialing == 0 {
		t.Fatalf("enter with nothing typed did not open the row: %s", sessHint(sel))
	}

	// With the popup up, ↑/↓ are the popup's: the list stays.
	pop, _ := typeList(t, m, "@")
	if !pop.sessList.in.at.visible() {
		t.Fatal("@ did not open the popup")
	}
	was = pop.sessList.sel
	pop, _ = press(pop, tea.KeyMsg{Type: tea.KeyDown})
	if pop.sessList.sel != was || pop.sessList.in.at.sel != 1 {
		t.Fatalf("↓ with the popup up: list %+v (was %+v), popup %d", pop.sessList.sel, was, pop.sessList.in.at.sel)
	}
	// esc hides the popup and keeps the text; the next esc clears it.
	pop, _ = press(pop, tea.KeyMsg{Type: tea.KeyEsc})
	if v, _ := inputOf(pop); pop.sessList.in.at.visible() || v != "@" {
		t.Fatalf("esc over the popup: visible %v, input %q", pop.sessList.in.at.visible(), v)
	}
	pop, _ = press(pop, tea.KeyMsg{Type: tea.KeyEsc})
	if v, _ := inputOf(pop); v != "" || !pop.sessList.open {
		t.Fatalf("the second esc: input %q, open %v", v, pop.sessList.open)
	}

	// ctrl+x is the list's with something typed; ctrl+d quits.
	x := selectKey(t, m, runKey("pty"))
	x, _ = typeList(t, x, "hi")
	x, _ = press(x, tea.KeyMsg{Type: tea.KeyCtrlX})
	if x.sessList.armed != runKey("pty") {
		t.Fatal("ctrl+x with text typed did not arm the close")
	}
	if q, _ := press(x, tea.KeyMsg{Type: tea.KeyCtrlD}); !q.quitting {
		t.Fatal("ctrl+d with text typed did not quit")
	}
}

// sessHint is the list's hint line.
func sessHint(m Model) string {
	rows := strings.Split(plainView(m), "\n")
	return strings.TrimSpace(rows[len(rows)-1])
}

// TestTheRuleNamesTheTarget (§3.13, decision 8): with no token the selected
// row's workspace — a running row's, a saved row's — and here on the saved
// group's line; a bound token's directory whatever is selected; a token
// being typed names nothing yet; one typed and finished names what it
// resolves to, or says it names nothing; text before it makes it prompt
// text, and the rule falls back, live.
func TestTheRuleNamesTheTarget(t *testing.T) {
	m, fs, _ := newSessModel(t, 100, 30)
	m = newList(t, m, fs)
	check := func(label string, m Model, want string) {
		t.Helper()
		if got := ruleOf(t, m); !strings.HasPrefix(got, want) {
			t.Fatalf("%s: the rule reads %q, want it to start %q", label, got, want)
		}
	}
	check("here's row", m, "new session → ~/projects/craze · ")
	check("a running row", selectKey(t, m, runKey("roost")), "new session → ~/projects/roost · ")
	saved := selectKey(t, m, sessSavedLine)
	check("the saved line", saved, "new session → ~/projects/craze · ")
	saved, _ = press(saved, enter())
	saved, _ = press(saved, tea.KeyMsg{Type: tea.KeyDown})
	saved, _ = press(saved, tea.KeyMsg{Type: tea.KeyDown})
	check("a saved row", saved, "new session → ~/projects/folio · ")

	typing, _ := typeList(t, selectKey(t, m, runKey("roost")), "@lum")
	check("a token being typed", typing, "new session → ~/projects/roost · ")
	typed, _ := typeList(t, typing, "en ")
	check("a token typed", typed, "new session → ~/projects/lumen · ")
	typed, _ = typeList(t, typed, "and a prompt")
	check("a token typed, a prompt after", typed, "new session → ~/projects/lumen · ")
	// Text before it: prompt text, and the row again.
	before, _ := press(typed, tea.KeyMsg{Type: tea.KeyHome})
	before, _ = typeList(t, before, "x ")
	check("text before the token", before, "new session → ~/projects/roost · ")

	missing, _ := typeList(t, m, "@nowhere ")
	check("a name that is nowhere", missing, "no directory named @nowhere")
	path, _ := typeList(t, m, "@~/notes/2026 ")
	check("a path typed", path, "new session → ~/notes/2026 · ")
	nopath, _ := typeList(t, m, "@~/nope ")
	check("a path that is not there", nopath, "no directory ~/nope")
	rel, _ := typeList(t, m, "@./docs ")
	check("a path relative to here", rel, "new session → ~/projects/craze/docs · ")
	// esc on a token still being typed: finished as far as the rule goes.
	hidden, _ := typeList(t, m, "@shed")
	hidden, _ = press(hidden, tea.KeyMsg{Type: tea.KeyEsc})
	check("a token whose popup esc hid", hidden, "new session → ~/projects/shed · ")
}

// TestAPickBindsAndAnEditDropsTheBinding (§3.15): enter on a candidate
// writes `@name ` and binds the token to that directory — the rule holds it
// though the candidates change so the name would no longer say which; an
// edit of the token drops the binding for good (enter then resolves the text
// afresh and finds it ambiguous), and so does deleting it; a pick of a name
// two directories share writes the path.
func TestAPickBindsAndAnEditDropsTheBinding(t *testing.T) {
	m, fs, _ := newSessModel(t, 100, 30)
	m = newList(t, m, fs)
	m, _ = typeList(t, m, "@lu")
	m, _ = press(m, enter())
	if v, cur := inputOf(m); v != "@lumen " || cur != len(v) || m.sessList.in.bound != homePath("projects/lumen") {
		t.Fatalf("picked into %q (cursor %d), bound %q", v, cur, m.sessList.in.bound)
	}
	m, _ = typeList(t, m, "hi")

	// Another lumen starts running: the name alone would no longer say which.
	other := t.TempDir()
	twin := filepath.Join(other, "lumen")
	if err := os.Mkdir(twin, 0o755); err != nil {
		t.Fatal(err)
	}
	snap := richSnapshot(os.Getenv("HOME"), m.hereKey())
	snap.Running = append(snap.Running, answered("twin", "the other lumen", "grok", twin, time.Second, nil))
	m = listSnap(t, m, snap)
	if got := ruleOf(t, m); !strings.HasPrefix(got, "new session → ~/projects/lumen · ") {
		t.Fatalf("the bound token's rule after the candidates changed: %q", got)
	}
	if sub, err := m.sessSubmit(); err != nil || sub.Dir != homePath("projects/lumen") || sub.Prompt != "hi" {
		t.Fatalf("submitted %+v, %v", sub, err)
	}

	// An edit inside the token: unbound, and the name is now two directories.
	edited, _ := press(m, tea.KeyMsg{Type: tea.KeyHome})
	for range 3 {
		edited, _ = press(edited, tea.KeyMsg{Type: tea.KeyRight})
	}
	edited, _ = press(edited, tea.KeyMsg{Type: tea.KeyBackspace})
	edited, _ = typeList(t, edited, "u")
	if v, _ := inputOf(edited); v != "@lumen hi" || edited.sessList.in.bound != "" {
		t.Fatalf("edited to %q, bound %q", v, edited.sessList.in.bound)
	}
	edited, _ = press(edited, tea.KeyMsg{Type: tea.KeyEnd})
	edited, _ = press(edited, enter())
	if !strings.Contains(sessHint(edited), "@lumen names 2 directories") || len(fs.opened()) != 0 {
		t.Fatalf("enter on an ambiguous name: %q", sessHint(edited))
	}

	// Deleting it drops the binding too, and typing it again does not bring
	// the binding back.
	gone, _ := press(m, tea.KeyMsg{Type: tea.KeyCtrlU})
	gone, _ = typeList(t, gone, "@lumen hi")
	if gone.sessList.in.bound != "" {
		t.Fatalf("a deleted token's binding came back: %q", gone.sessList.in.bound)
	}

	// Picking one of the two writes its path, and binds it.
	twins, _ := press(m, tea.KeyMsg{Type: tea.KeyCtrlU})
	twins, _ = typeList(t, twins, "@lumen")
	names := popupNames(twins.sessList.in.at)
	if !slices.Equal(names[:2], []string{"lumen", "lumen"}) {
		t.Fatalf("the two lumens: %v", names)
	}
	twins, _ = press(twins, tea.KeyMsg{Type: tea.KeyDown})
	it, _ := twins.sessList.in.at.selected()
	twins, _ = press(twins, tea.KeyMsg{Type: tea.KeyTab})
	want := "@" + sessTilde(os.Getenv("HOME"), it.Value) + " "
	if v, _ := inputOf(twins); v != want || twins.sessList.in.bound != it.Value {
		t.Fatalf("picked the second lumen into %q (want %q), bound %q", v, want, twins.sessList.in.bound)
	}
}

// TestEnterResolvesTheTokenAfresh (§3.15, R2-11): enter reads an unbound
// leading token again — a unique name, a path that is a directory now — and
// sends the rest as the prompt; a name that is nowhere, or a path that is not
// there, is an error on the hint line and nothing starts; a later `@…` stays
// in the prompt; the token alone is enter with an empty prompt.
func TestEnterResolvesTheTokenAfresh(t *testing.T) {
	m, fs, _ := newSessModel(t, 100, 30)
	m = newList(t, m, fs)
	for _, c := range []struct {
		typed, dir, prompt, err string
	}{
		{typed: "@roost fix it", dir: homePath("projects/roost"), prompt: "fix it"},
		{typed: "  @roost   fix @lumen too ", dir: homePath("projects/roost"), prompt: "fix @lumen too"},
		{typed: "@notes", dir: homePath("notes"), prompt: ""},
		{typed: "@~/notes/2026/ plan", dir: homePath("notes/2026"), prompt: "plan"},
		{typed: "@.. up", dir: homePath("projects"), prompt: "up"},
		{typed: `@"~/projects/shed" go`, dir: homePath("projects/shed"), prompt: "go"},
		{typed: "no token @roost", dir: homePath("projects/craze"), prompt: "no token @roost"},
		{typed: "@ros fix it", err: "no directory named @ros"},
		{typed: "@~/gone fix it", err: "no directory ~/gone"},
		{typed: "@~/projects/README x", err: "no directory ~/projects/README"},
	} {
		t.Run(c.typed, func(t *testing.T) {
			typed, _ := typeList(t, m, c.typed)
			sub, err := typed.sessSubmit()
			switch {
			case c.err != "":
				if err == nil || err.Error() != c.err {
					t.Fatalf("submitted %+v, %v; want %q", sub, err, c.err)
				}
				after, _ := press(typed, enter())
				if got := sessHint(after); got != c.err || after.sessList.noteKind != sessNoteErr {
					t.Fatalf("enter's hint %q", got)
				}
			case err != nil || sub.Dir != c.dir || sub.Prompt != c.prompt:
				t.Fatalf("submitted %+v, %v; want %s %q", sub, err, c.dir, c.prompt)
			}
		})
	}
	// A directory that goes away after the rule read it is found gone at
	// enter: the rule's reading is not enter's.
	gone := homePath("projects/gone-soon")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	typed, _ := typeList(t, m, "@~/projects/gone-soon x")
	if got := ruleOf(t, typed); !strings.HasPrefix(got, "new session → ~/projects/gone-soon") {
		t.Fatalf("the rule: %q", got)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := typed.sessSubmit(); err == nil || err.Error() != "no directory ~/projects/gone-soon" {
		t.Fatalf("a directory gone since the rule read it: %v", err)
	}
}

// TestTheAtSourceOrdersNotesAndFilters (§3.15): here first, noted with its
// running count; then each running directory once, the most recently active
// first, with its count; then the recent directories not already offered,
// with their age; a basename prefix first, then a path substring, case
// folded; a name two directories share, or one that reads as a path, writes
// the path.
func TestTheAtSourceOrdersNotesAndFilters(t *testing.T) {
	m, fs, _ := newSessModel(t, 100, 30)
	m = newList(t, m, fs)
	src := m.sessAtSource()
	all := src.complete(completeQuery{Text: ""})
	type row struct{ name, detail, note string }
	var got []row
	for _, it := range all.Items {
		got = append(got, row{it.Name, it.Detail, it.Note})
	}
	want := []row{
		{"craze", "~/projects/craze", "here · 3 running"},
		{"shed", "~/projects/shed", "1 running"},
		{"roost", "~/projects/roost", "1 running"},
		{"lumen", "~/projects/lumen", "1 running"},
		{"prox", "~/projects/prox", "1 running"},
		{"spendwise", "~/projects/spendwise", "1 running"},
		{"folio", "~/projects/folio", "1 running"},
		{"pharos", "~/projects/pharos", "used 2h ago"},
		{"notes", "~/notes", "used 5d ago"},
		{"tapper", "~/projects/tapper", "used 14d ago"},
	}
	if !slices.Equal(got, want) || all.Title != sessAtTitle {
		t.Fatalf("the candidates:\n%+v\nwant\n%+v", got, want)
	}
	names := func(q string) []string {
		var out []string
		for _, it := range src.complete(completeQuery{Text: q}).Items {
			out = append(out, it.Name)
		}
		return out
	}
	if got := names("P"); !slices.Equal(got, []string{"prox", "pharos", "craze", "shed", "roost", "lumen", "spendwise", "folio", "tapper"}) {
		t.Fatalf("p: %v (a basename prefix first, then the path)", got)
	}
	if got := names("NOT"); !slices.Equal(got, []string{"notes"}) {
		t.Fatalf("NOT: %v", got)
	}
	if got := names("jects/r"); !slices.Equal(got, []string{"roost"}) {
		t.Fatalf("jects/r: %v", got)
	}
	if got := names("zzz"); len(got) != 0 {
		t.Fatalf("zzz: %v", got)
	}

	// The inserted text: a name, a shared name's path, a path-like name's
	// path; a name with a space is quoted by the grammar.
	odd := sessDirSource{home: "/h", cands: []sessDirCand{{dir: "/h/a/lumen"}, {dir: "/h/b/lumen"}, {dir: "/h/.dots"}, {dir: "/h/my dir"}, {dir: "/h/solo"}}}
	var inserts []string
	for _, it := range odd.complete(completeQuery{}).Items {
		tok, _, _ := atTokenText(it.Insert)
		inserts = append(inserts, tok)
	}
	if want := []string{"@~/a/lumen", "@~/b/lumen", "@~/.dots", `@"my dir"`, "@solo"}; !slices.Equal(inserts, want) {
		t.Fatalf("inserted %q, want %q", inserts, want)
	}

	// An ended session runs nowhere, and a directory is offered once.
	ended := m
	ended.sessList.hereEnded = true
	for _, it := range ended.sessAtSource().complete(completeQuery{}).Items {
		if it.Name == "craze" && it.Note != "here · 2 running" {
			t.Fatalf("here after its session ended: %q", it.Note)
		}
	}
}

// TestBrowsingListsSubdirectories (§3.15): `~`, `/`, `.` browse — the named
// directory's subdirectories, read off the Update: no files, dot-directories
// only for a partial name starting with `.`, a link to a directory
// included, sorted; at most 200; tab descends and keeps the popup open,
// enter picks and binds; `~` alone is home; a directory that is not there,
// or a file, says `no directory …`.
func TestBrowsingListsSubdirectories(t *testing.T) {
	m, fs, _ := newSessModel(t, 100, 30)
	m = newList(t, m, fs)
	if err := os.Symlink(homePath("notes"), homePath("projects/zlink")); err != nil {
		t.Fatal(err)
	}
	b := browsed(t, m, "@~/projects/")
	want := []string{"craze/", "folio/", "lumen/", "pharos/", "prox/", "roost/", "shed/", "spendwise/", "tapper/", "zlink/"}
	if got := popupNames(b.sessList.in.at); !slices.Equal(got, want) || b.sessList.in.at.ans.Title != "folders in ~/projects" {
		t.Fatalf("~/projects/: %v (%q)", got, b.sessList.in.at.ans.Title)
	}
	// The same listing serves a partial name: no second read.
	p, _ := typeList(t, b, "p")
	if p.sessList.in.at.pending() {
		t.Fatal("a partial name read the directory again")
	}
	if got := popupNames(p.sessList.in.at); !slices.Equal(got, []string{"pharos/", "prox/"}) {
		t.Fatalf("~/projects/p: %v", got)
	}
	hidden, _ := typeList(t, b, ".")
	if got := popupNames(hidden.sessList.in.at); !slices.Equal(got, []string{".cache/"}) {
		t.Fatalf("~/projects/.: %v", got)
	}
	// tab descends: the popup stays, on what is inside.
	cr, _ := typeList(t, b, "cr")
	cr, cmd := press(cr, tea.KeyMsg{Type: tea.KeyTab})
	if v, _ := inputOf(cr); v != "@~/projects/craze/" || !cr.sessList.in.at.visible() {
		t.Fatalf("tab on craze/: %q, visible %v", v, cr.sessList.in.at.visible())
	}
	tm, _ := cr.Update(listLoad(t, cmd))
	cr = tm.(Model)
	if got := popupNames(cr.sessList.in.at); !slices.Equal(got, []string{"cmd/", "docs/", "internal/"}) {
		t.Fatalf("~/projects/craze/: %v", got)
	}
	// enter picks, and binds.
	cr, _ = press(cr, tea.KeyMsg{Type: tea.KeyDown})
	cr, _ = press(cr, enter())
	if v, _ := inputOf(cr); v != "@~/projects/craze/docs " || cr.sessList.in.bound != homePath("projects/craze/docs") || cr.sessList.in.at.visible() {
		t.Fatalf("enter on docs/: %q, bound %q", v, cr.sessList.in.bound)
	}

	home := browsed(t, m, "@~")
	if got := popupNames(home.sessList.in.at); !slices.Equal(got, []string{"notes/", "projects/"}) {
		t.Fatalf("~: %v", got)
	}
	if acc, _ := press(home, enter()); func() bool { v, _ := inputOf(acc); return v != "@~/notes " }() {
		t.Fatalf("enter under ~ wrote %q", func() string { v, _ := inputOf(acc); return v }())
	}
	dot := browsed(t, m, "@./")
	if got := popupNames(dot.sessList.in.at); !slices.Equal(got, []string{"cmd/", "docs/", "internal/"}) {
		t.Fatalf("./: %v", got)
	}
	for _, c := range []struct{ typed, note string }{
		{"@~/nope/", "no directory ~/nope"},
		{"@~/projects/README/", "no directory ~/projects/README"},
	} {
		gone := browsed(t, m, c.typed)
		if got := trimmed(popupText(gone.sessList.in.at, 60, 10)); !slices.Equal(got, []string{"  " + c.note}) {
			t.Fatalf("%s: %q", c.typed, got)
		}
	}
	// `~user` is not expanded: nothing is read.
	if user, _ := typeList(t, m, "@~bob/"); user.sessList.in.at.pending() || !strings.Contains(strings.Join(popupText(user.sessList.in.at, 60, 10), "\n"), "no directory ~bob") {
		t.Fatalf("~bob/: %v", popupText(user.sessList.in.at, 60, 10))
	}

	// At most 200 of a directory's subdirectories.
	many := homePath("many")
	for i := range 250 {
		if err := os.MkdirAll(filepath.Join(many, fmt.Sprintf("d%03d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lots := browsed(t, m, "@~/many/")
	if got := popupNames(lots.sessList.in.at); len(got) != sessBrowseMax || got[0] != "d000/" || got[199] != "d199/" {
		t.Fatalf("~/many/: %d names, %v … %v", len(got), got[:1], got[len(got)-1:])
	}
	if got := popupNames(browsedMore(t, lots, "d24")); !slices.Equal(got, []string{"d240/", "d241/", "d242/", "d243/", "d244/", "d245/", "d246/", "d247/", "d248/", "d249/"}) {
		t.Fatalf("~/many/d24: %v (the cap is on what is offered, not what is read)", got)
	}
}

// browsedMore types more into a browsed token — the listing already there —
// and answers the popup.
func browsedMore(t *testing.T, m Model, text string) completePopup {
	t.Helper()
	m, _ = typeList(t, m, text)
	if m.sessList.in.at.pending() {
		t.Fatal("the directory was read again")
	}
	return m.sessList.in.at
}

// TestSessListDirs: the listing itself — dot-directories kept (the source
// hides them), files and a link to a file left out, a broken link too, an
// order that folds case; a cancelled context stops it.
func TestSessListDirs(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"b", "A", ".h", "c"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"lf": "f", "ld": "c", "broken": "nothing"} {
		if err := os.Symlink(filepath.Join(dir, target), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	l := sessListDirs(context.Background(), dir)
	var got []string
	for _, it := range l.Items {
		got = append(got, it.Name)
	}
	if l.Err != nil || !slices.Equal(got, []string{".h", "A", "b", "c", "ld"}) {
		t.Fatalf("listed %v, %v", got, l.Err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l := sessListDirs(ctx, dir); !errors.Is(l.Err, context.Canceled) {
		t.Fatalf("a cancelled listing answered %v", l.Err)
	}
	if l := sessListDirs(context.Background(), filepath.Join(dir, "f")); !errors.Is(l.Err, errNotADirectory) {
		t.Fatalf("a file listed: %v", l.Err)
	}
}

// TestAListingNoLongerAwaitedIsDropped (§3.15): a directory's listing
// answered after the token moved on to another directory — its work
// cancelled — after the list was left and opened again, or after a switch to
// another session, is dropped; the popup takes only the one it awaits.
// Every schedule is forced: the listings are held and delivered by hand.
func TestAListingNoLongerAwaitedIsDropped(t *testing.T) {
	m, fs, _ := newSessModel(t, 100, 30)
	m = newList(t, m, fs)

	// The token moves on: the first listing is cancelled, its answer dropped.
	a, cmdA := typeList(t, m, "@~/")
	for range 2 {
		a, _ = press(a, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	b, cmdB := typeList(t, a, "~/projects/")
	msgA := listLoad(t, cmdA)
	if !errors.Is(msgA.res.Err, context.Canceled) {
		t.Fatalf("the listing no longer awaited answered %v, not its cancel", msgA.res.Err)
	}
	tm, _ := b.Update(msgA)
	if got := tm.(Model); len(popupNames(got.sessList.in.at)) != 0 || !got.sessList.in.at.pending() {
		t.Fatalf("the first listing was taken: %v", popupNames(got.sessList.in.at))
	}
	tm, _ = tm.(Model).Update(listLoad(t, cmdB))
	if got := popupNames(tm.(Model).sessList.in.at); len(got) == 0 || got[0] != "craze/" {
		t.Fatalf("the awaited listing: %v", got)
	}

	// The list left and opened again: the old opening's answer is dropped.
	c, cmdC := typeList(t, m, "@~/projects/")
	c, _ = press(c, tea.KeyMsg{Type: tea.KeyEsc})
	c, _ = press(c, tea.KeyMsg{Type: tea.KeyEsc})
	c, _ = press(c, tea.KeyMsg{Type: tea.KeyEsc})
	if c.sessList.open {
		t.Fatal("fixture: the list is still open")
	}
	c = openList(t, c)
	c, cmdC2 := typeList(t, c, "@~/projects/")
	tm, _ = c.Update(listLoad(t, cmdC))
	if got := tm.(Model); len(popupNames(got.sessList.in.at)) != 0 {
		t.Fatalf("a listing from the list's last opening was taken: %v", popupNames(got.sessList.in.at))
	}
	tm, _ = tm.(Model).Update(listLoad(t, cmdC2))
	if len(popupNames(tm.(Model).sessList.in.at)) == 0 {
		t.Fatal("the reopened list's own listing was not taken")
	}

	// A switch: the listing asked for in the session left is left behind by
	// the gate, before the list — the next session's, opened again — sees it.
	d, cmdD := typeList(t, m, "@~/projects/")
	sw, _ := d.switchBackend(newLane(t, "b", "bravo"), false, "")
	sw = openList(t, sw)
	sw, _ = typeList(t, sw, "@~/projects/")
	msgD := listLoad(t, cmdD)
	if !sw.leftBehind(msgD) {
		t.Fatal("a listing asked for before the switch is not left behind")
	}
	tm, _ = sw.Update(msgD)
	if got := tm.(Model); len(popupNames(got.sessList.in.at)) != 0 {
		t.Fatalf("a listing from before the switch was taken: %v", popupNames(got.sessList.in.at))
	}

	// Another source's result is not the list's.
	if _, _, ok := m.applySessMsg(completeLoadedMsg{source: "files"}); ok {
		t.Fatal("the list took another source's result")
	}
}

// TestTheInputsPopupSqueezesAboveTheList: with little room the popup gives
// way — the list keeps sessBodyMinRows, the frame its height.
func TestTheInputsPopupSqueezesAboveTheList(t *testing.T) {
	m, fs, _ := newSessModel(t, 40, 10)
	m = newList(t, m, fs)
	m, _ = typeList(t, m, "@")
	rows := strings.Split(plainView(m), "\n")
	if len(rows) != 10 {
		t.Fatalf("%d rows:\n%s", len(rows), strings.Join(rows, "\n"))
	}
	// header 2, list 2, popup 2 (a row and its count), rule, input, rule, hint.
	if !strings.HasPrefix(rows[4], "❯ craze") || !strings.Contains(rows[5], "more") || !strings.Contains(rows[6], "new session") {
		t.Fatalf("squeezed:\n%s", strings.Join(rows, "\n"))
	}
}

// TestALongLineShowsAroundItsCursor: a line wider than the row is shown
// around the cursor — its end while typing there, its start after Home —
// wide runes counted in cells; a long target directory is cut from the left,
// its last elements kept.
func TestALongLineShowsAroundItsCursor(t *testing.T) {
	ten := []rune("abcdefghij")
	for _, c := range []struct {
		rs         []rune
		pos, avail int
		from, to   int
		label      string
	}{
		{rs: ten, pos: 10, avail: 20, from: 0, to: 10, label: "fits"},
		{rs: ten, pos: 10, avail: 5, from: 6, to: 10, label: "the end, the cursor past it"},
		{rs: ten, pos: 0, avail: 5, from: 0, to: 5, label: "the start"},
		{rs: ten, pos: 5, avail: 5, from: 1, to: 6, label: "the middle"},
		{rs: []rune("日本語日本語"), pos: 6, avail: 5, from: 4, to: 6, label: "wide runes"},
	} {
		if from, to := sessInputWindow(c.rs, c.pos, c.avail); from != c.from || to != c.to {
			t.Errorf("%s: [%d, %d), want [%d, %d)", c.label, from, to, c.from, c.to)
		}
	}

	m, fs, _ := newSessModel(t, 40, 12)
	m = newList(t, m, fs)
	long := strings.Repeat("0123456789", 6)
	m, _ = typeList(t, m, long)
	row := func(m Model) string {
		rows := strings.Split(plainView(m), "\n")
		return rows[len(rows)-3]
	}
	if got := row(m); got != "❯ "+long[len(long)-37:]+" " {
		t.Fatalf("typing at the end of a long line: %q", got)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyHome})
	if got := row(m); got != "❯ "+long[:38] {
		t.Fatalf("at the start of a long line: %q", got)
	}

	deep := homePath("projects/a/very/deeply/nested/directory/for/lumen")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlU})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlK})
	m, _ = typeList(t, m, "@~/projects/a/very/deeply/nested/directory/for/lumen ")
	rule := plain(m.sessTargetRule(60))
	if !strings.HasPrefix(rule, "─ new session → …") || !strings.Contains(rule, "/for/lumen · cursor · Grok ─") || lipgloss.Width(rule) != 60 {
		t.Fatalf("a long directory's rule: %q", rule)
	}
}

// A directory listing the list's popup waits for is cancelled on every exit
// (sol r28-c14 2): the list's own quit (ctrl+d, /exit) closes the popup, and a
// quit the list never saw — a signal ending the program — reaches finishRun,
// which cancels what is left. Either way the listing, run after, ends with
// its context's cancellation.
func TestTheListsListingIsCancelledOnEveryExit(t *testing.T) {
	for _, exit := range []string{"ctrl+d", "finishRun"} {
		t.Run(exit, func(t *testing.T) {
			m, fs, _ := newSessModel(t, 100, 30)
			m = newList(t, m, fs)
			m, cmd := typeList(t, m, "@~/projects/")
			if cmd == nil || !m.sessList.in.at.pending() {
				t.Fatal("fixture: no listing awaited")
			}
			switch exit {
			case "ctrl+d":
				m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlD})
				if !m.quitting || m.sessList.in.at.pending() {
					t.Fatalf("ctrl+d: quitting %v, the popup still waits %v", m.quitting, m.sessList.in.at.pending())
				}
			case "finishRun":
				finishRun(io.Discard, m, m, nil)
			}
			if lm := listLoad(t, cmd); !errors.Is(lm.res.Err, context.Canceled) {
				t.Fatalf("the listing after the exit answered %v items, err %v", len(lm.res.Items), lm.res.Err)
			}
			if n := len(m.completeLoads.open); n != 0 {
				t.Fatalf("%d loads left in the set", n)
			}
		})
	}
}

// The browse listing's bounds (X137, sol r28-c14 3): it reads a directory
// sessBrowseBatch entries at a time and stops at the first batch boundary
// after its context is cancelled — forced there by the batch hook — and it
// reads no more than sessBrowseReadMax entries of a directory holding more.
func TestTheListingIsReadInBoundedBatches(t *testing.T) {
	mkdirs := func(t *testing.T, n int) string {
		t.Helper()
		dir := t.TempDir()
		for i := range n {
			if err := os.Mkdir(filepath.Join(dir, fmt.Sprintf("d%05d", i)), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	var reads []int
	sessListBatchHook = func(read int) { reads = append(reads, read) }
	t.Cleanup(func() { sessListBatchHook = nil })

	t.Run("cancelled between batches", func(t *testing.T) {
		dir := mkdirs(t, 3*sessBrowseBatch)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reads = nil
		sessListBatchHook = func(read int) {
			reads = append(reads, read)
			if len(reads) == 1 {
				cancel()
			}
		}
		res := sessListDirs(ctx, dir)
		if !errors.Is(res.Err, context.Canceled) || !slices.Equal(reads, []int{sessBrowseBatch}) {
			t.Fatalf("cancelled after the first batch: err %v, batches read to %v", res.Err, reads)
		}
	})
	t.Run("the cap holds", func(t *testing.T) {
		dir := mkdirs(t, sessBrowseReadMax+50)
		reads = nil
		sessListBatchHook = func(read int) { reads = append(reads, read) }
		res := sessListDirs(context.Background(), dir)
		if res.Err != nil || len(reads) == 0 || reads[len(reads)-1] != sessBrowseReadMax || len(res.Items) != sessBrowseReadMax {
			last := 0
			if len(reads) > 0 {
				last = reads[len(reads)-1]
			}
			t.Fatalf("a directory of %d: read %d entries in %d batches, listed %d (err %v)",
				sessBrowseReadMax+50, last, len(reads), len(res.Items), res.Err)
		}
		for i, r := range reads[:len(reads)-1] {
			if r != (i+1)*sessBrowseBatch {
				t.Fatalf("batch %d ended at %d: not %d at a time", i, r, sessBrowseBatch)
			}
		}
	})
}

// A paste lands where it was asked for (X140, C15): one asked for in the
// composer that arrives after the list opened lands in the composer's draft —
// there when the user goes back — not the list's input; one asked for in the
// list's input lands there, and not once the list has closed.
func TestAPasteLandsWhereItWasAskedFor(t *testing.T) {
	m, fs, _ := newSessModel(t, 100, 30)
	m = newList(t, m, fs)
	tm, _ := m.Update(pasteMsg{text: "a draft", shownGen: m.shownGen})
	m = tm.(Model)
	if v, _ := inputOf(m); v != "" || m.input.Value() != "a draft" {
		t.Fatalf("the composer's paste: input %q, composer %q", v, m.input.Value())
	}
	tm, _ = m.Update(pasteMsg{text: "a prompt", shownGen: m.shownGen, list: true})
	m = tm.(Model)
	if v, _ := inputOf(m); v != "a prompt" || m.input.Value() != "a draft" {
		t.Fatalf("the list's paste: input %q, composer %q", v, m.input.Value())
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.sessList.open || m.input.Value() != "a draft" {
		t.Fatalf("back in the session: list open %v, composer %q", m.sessList.open, m.input.Value())
	}
	tm, _ = m.Update(pasteMsg{text: " late", shownGen: m.shownGen, list: true})
	if got := tm.(Model).input.Value(); got != "a draft" {
		t.Fatalf("a paste for the closed list's input reached the composer: %q", got)
	}
}
