package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
)

func pickerFactory(t *testing.T) func(agent.Provider) agent.Session {
	t.Helper()
	return func(p agent.Provider) agent.Session {
		s := NewStub()
		s.SetProvider(p)
		return s
	}
}

func newPicker(t *testing.T, def agent.Provider) Model {
	t.Helper()
	return newPickerRows(t, def, nil)
}

// newPickerRows is newPicker with the row list spelled out — what internal/cli
// hands over once it has filtered the optional providers for availability.
// internal/tui never asks PATH itself (§3.5), so a three-row picker is built by
// passing three rows and never by installing a binary.
func newPickerRows(t *testing.T, def agent.Provider, rows []agent.Provider) Model {
	t.Helper()
	isolateSkillsHome(t)
	m := New(Config{
		Theme:      "tokyo-night",
		Workspace:  t.TempDir(),
		Model:      "grok",
		Yolo:       true,
		Provider:   def,
		Providers:  rows,
		NewSession: pickerFactory(t),
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return tm.(Model)
}

// providerRowNames is the picker's list as ids, which is what the row-set
// assertions compare.
func providerRowNames(rows []agent.Provider) []string {
	out := make([]string, 0, len(rows))
	for _, p := range rows {
		out = append(out, p.Name())
	}
	return out
}

// pickerRowLine is the rendered picker row for a provider: the content of the
// one dialog row whose text, once the focus gutter is off it, starts with that
// name. Reading it out of the frame rather than off the model is what makes the
// "default" tag assertions about what the user sees.
//
// The rows are sliced out of m.lay.Dialog the way markedDialogRows does, not
// searched for by box character: the status line draws a "│" of its own, so a
// whole-frame scan could answer with a row that is not the dialog's.
func pickerRowLine(t *testing.T, m Model, name string) string {
	t.Helper()
	view := plainView(m)
	r := m.lay.Dialog
	for y := r.Y + 1; y < r.Y+r.H-1; y++ {
		inner := ansi.Cut(rows(view)[y], r.X+1, r.X+r.W-1)
		text := strings.TrimPrefix(inner, dialogCursorMark)
		if fields := strings.Fields(text); len(fields) > 0 && fields[0] == name {
			return inner
		}
	}
	t.Fatalf("no picker row for %q:\n%s", name, view)
	return ""
}

func TestProviderPickerShowsBeforeStart(t *testing.T) {
	m := newPicker(t, agent.CursorProvider())
	if !m.pickingProvider || m.dialog != dialogProvider {
		t.Fatalf("picker dialog=%v picking=%v", m.dialog, m.pickingProvider)
	}
	if m.started || m.sess != nil {
		t.Fatal("session must not exist until a row is chosen")
	}
	view := plainView(m)
	if strings.Contains(view, "starting…") {
		t.Fatalf("starting chip during picker:\n%s", view)
	}
	if !strings.Contains(view, "cursor") || !strings.Contains(view, "grok") {
		t.Fatalf("picker rows missing:\n%s", view)
	}
	if !strings.Contains(view, "default") {
		t.Fatalf("default tag missing:\n%s", view)
	}
}

func TestProviderLockedSkipsPicker(t *testing.T) {
	isolateSkillsHome(t)
	called := 0
	m := New(Config{
		Theme:          "tokyo-night",
		Workspace:      t.TempDir(),
		Yolo:           true,
		Provider:       agent.GrokProvider(),
		ProviderLocked: true,
		NewSession: func(p agent.Provider) agent.Session {
			called++
			s := NewStub()
			s.SetProvider(p)
			return s
		},
	})
	if m.pickingProvider || m.dialog != dialogNone {
		t.Fatalf("locked still picking dialog=%v", m.dialog)
	}
	if called != 1 {
		t.Fatalf("factory called %d times", called)
	}
	if m.sess == nil {
		t.Fatal("locked path must construct immediately")
	}
	if m.snap.Provider.Name != "grok" {
		t.Fatalf("provider %q", m.snap.Provider.Name)
	}
}

func TestProviderPickerEnterStartsSelected(t *testing.T) {
	m := newPicker(t, agent.CursorProvider())
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	if m.providerCursor != 1 {
		t.Fatalf("cursor %d", m.providerCursor)
	}
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.pickingProvider || m.dialog != dialogNone {
		t.Fatal("picker still open after enter")
	}
	msg := runCmd(cmd)
	if _, ok := msg.(startedMsg); !ok {
		t.Fatalf("start cmd %T", msg)
	}
	tm, _ = m.Update(msg)
	m = tm.(Model)
	if !m.started {
		t.Fatal("not started")
	}
	if m.snap.Provider.Name != "grok" {
		t.Fatalf("provider %q", m.snap.Provider.Name)
	}
}

func TestProviderPickerEscStartsDefault(t *testing.T) {
	m := newPicker(t, agent.GrokProvider())
	if m.providerCursor != 1 {
		t.Fatalf("preselect %d", m.providerCursor)
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = tm.(Model)
	if m.providerCursor != 0 {
		t.Fatalf("cursor after up %d", m.providerCursor)
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	msg := runCmd(cmd)
	tm, _ = m.Update(msg)
	m = tm.(Model)
	if m.snap.Provider.Name != "grok" {
		t.Fatalf("esc must start the default, got %q", m.snap.Provider.Name)
	}
}

func TestStartedMsgPersistsProvider(t *testing.T) {
	path := writeConfigFile(t, "")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	isolateSkillsHome(t)
	stub := NewStub()
	stub.SetProvider(agent.GrokProvider())
	m := New(Config{
		Session:         stub,
		Theme:           "tokyo-night",
		Workspace:       t.TempDir(),
		Yolo:            true,
		PersistProvider: true,
		ProviderLocked:  true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	if !tm.(Model).started {
		t.Fatal("not started")
	}
	if got := ConfigProvider(); got != "grok" {
		t.Fatalf("persisted %q", got)
	}
}

func TestProviderPickerCtrlDQuitsWithoutSession(t *testing.T) {
	path := writeConfigFile(t, "provider = \"codex\"\n")
	called := 0
	isolateSkillsHome(t)
	m := New(Config{
		Theme:           "tokyo-night",
		Workspace:       t.TempDir(),
		Yolo:            true,
		Provider:        agent.CursorProvider(),
		PersistProvider: true,
		FallbackDefault: true,
		NewSession: func(p agent.Provider) agent.Session {
			called++
			s := NewStub()
			s.SetProvider(p)
			return s
		},
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = tm.(Model)
	if !m.quitting {
		t.Fatal("ctrl+d should quit")
	}
	msg := runCmd(cmd)
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("quit cmd %T", msg)
	}
	if called != 0 {
		t.Fatal("factory must not run on picker quit")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "codex") {
		t.Fatalf("picker quit persisted:\n%s", body)
	}
}

func TestFallbackPickerEnterPersistsChosenProvider(t *testing.T) {
	path := writeConfigFile(t, "provider = \"codex\"\n")
	m := New(Config{
		Theme:           "tokyo-night",
		Workspace:       t.TempDir(),
		Yolo:            true,
		Provider:        agent.CursorProvider(),
		PersistProvider: true,
		FallbackDefault: true,
		NewSession:      pickerFactory(t),
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = tm.(Model)
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	tm, _ = m.Update(runCmd(cmd))
	m = tm.(Model)
	if m.snap.Provider.Name != "grok" {
		t.Fatalf("provider %q", m.snap.Provider.Name)
	}
	if got := ConfigProvider(); got != "grok" {
		t.Fatalf("persisted %q, file %s", got, path)
	}
}

func TestFallbackPickerEscDoesNotPersist(t *testing.T) {
	path := writeConfigFile(t, "provider = \"codex\"\n")
	m := New(Config{
		Theme:           "tokyo-night",
		Workspace:       t.TempDir(),
		Yolo:            true,
		Provider:        agent.CursorProvider(),
		PersistProvider: true,
		FallbackDefault: true,
		NewSession:      pickerFactory(t),
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	tm, _ = m.Update(runCmd(cmd))
	_ = tm
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "codex") {
		t.Fatalf("esc on fallback persisted:\n%s", body)
	}
	if ConfigProvider() == "cursor" {
		t.Fatal("esc on fallback must not write cursor over the unknown id")
	}
}

func TestFailedStartDoesNotPersistProvider(t *testing.T) {
	path := writeConfigFile(t, "")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	isolateSkillsHome(t)
	m := New(Config{
		Session:         NewStub(),
		Theme:           "tokyo-night",
		Workspace:       t.TempDir(),
		Yolo:            true,
		PersistProvider: true,
		ProviderLocked:  true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(errMsg{err: fmt.Errorf("nope")})
	_ = tm
	if got := ConfigProvider(); got != "" {
		t.Fatalf("failed start persisted %q", got)
	}
}

func TestStartedMsgDoesNotPersistWhenDisabled(t *testing.T) {
	path := writeConfigFile(t, "theme = \"tokyo-night\"\n")
	m := sized(t)
	tm, _ := m.Update(startedMsg{})
	_ = tm
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "provider") {
		t.Fatalf("wrote provider without PersistProvider:\n%s", body)
	}
}

// goldenPicker is the model the picker's frame goldens render. One Config
// serves all of them, so a golden differs from its neighbours only by the row
// list, the default and the terminal size, and a change to the frame cannot
// land in one of them and miss the others.
func goldenPicker(t *testing.T, def agent.Provider, list []agent.Provider, cols, rows int) Model {
	t.Helper()
	m := New(Config{
		Theme:      "tokyo-night",
		Workspace:  frameWorkspace(t),
		Model:      "grok",
		Yolo:       true,
		Provider:   def,
		Providers:  list,
		NewSession: pickerFactory(t),
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: cols, Height: rows})
	return tm.(Model)
}

func TestFrameGoldenProviderPicker(t *testing.T) {
	isolateSkillsHome(t)
	for _, size := range []struct{ cols, rows int }{{100, 30}, {80, 24}} {
		m := goldenPicker(t, agent.CursorProvider(), nil, size.cols, size.rows)
		name := "provider-picker-100x30"
		if size.cols == 80 {
			name = "provider-picker-80x24"
		}
		assertGolden(t, name, size.cols, size.rows, plainView(m))
	}
}

// TestProviderPickerThreeRows is the gx-installed picker: three rows, in
// registry order, with the default tagged. Passing a list that already holds
// the default also pins the deduplication — the union must not draw cursor
// twice.
func TestProviderPickerThreeRows(t *testing.T) {
	m := newPickerRows(t, agent.CursorProvider(), agent.Providers())
	if got := providerRowNames(m.providers); !reflect.DeepEqual(got, []string{"cursor", "grok", "gx"}) {
		t.Fatalf("rows %q, want [cursor grok gx]", got)
	}
	for _, name := range []string{"cursor", "grok", "gx"} {
		pickerRowLine(t, m, name)
	}
	if row := pickerRowLine(t, m, "cursor"); !strings.Contains(row, "default") {
		t.Fatalf("the default row is untagged: %q", row)
	}
	if row := pickerRowLine(t, m, "gx"); strings.Contains(row, "default") {
		t.Fatalf("only the default row may be tagged: %q", row)
	}
}

// TestProviderPickerWrapsOverThreeRows pins AC 3's navigation half: the cursor
// wraps in both directions over three rows, and tab/shift-tab are the same
// motion as down/up. Two rows made 0 and 1 adjacent either way, so the wrap
// arithmetic was never really exercised.
//
// The three rows are spelled out rather than taken from the registry: the
// concern is the modular arithmetic, so a fourth provider must not fail this
// test about logic that would be innocent.
func TestProviderPickerWrapsOverThreeRows(t *testing.T) {
	three := []agent.Provider{agent.CursorProvider(), agent.GrokProvider(), agent.GxProvider()}
	for _, keys := range []struct {
		name     string
		fwd, rev tea.KeyType
	}{
		{"arrows", tea.KeyDown, tea.KeyUp},
		{"tab", tea.KeyTab, tea.KeyShiftTab},
	} {
		t.Run(keys.name, func(t *testing.T) {
			m := newPickerRows(t, agent.CursorProvider(), three)
			for _, want := range []int{1, 2, 0, 1} {
				tm, _ := m.Update(tea.KeyMsg{Type: keys.fwd})
				m = tm.(Model)
				if m.providerCursor != want {
					t.Fatalf("forward cursor %d, want %d", m.providerCursor, want)
				}
			}
			for _, want := range []int{0, 2, 1, 0} {
				tm, _ := m.Update(tea.KeyMsg{Type: keys.rev})
				m = tm.(Model)
				if m.providerCursor != want {
					t.Fatalf("backward cursor %d, want %d", m.providerCursor, want)
				}
			}
		})
	}
}

// TestProviderPickerGxDefaultPreselectsThirdRow pins that the preselection is
// an index into the rendered list and not a two-provider assumption.
func TestProviderPickerGxDefaultPreselectsThirdRow(t *testing.T) {
	m := newPickerRows(t, agent.GxProvider(), agent.Providers())
	if m.providerCursor != 2 {
		t.Fatalf("gx default preselects row %d, want 2", m.providerCursor)
	}
	if row := pickerRowLine(t, m, "gx"); !strings.Contains(row, "default") {
		t.Fatalf("gx row is not tagged default: %q", row)
	}
}

// TestProviderPickerShowsDefaultMissingFromTheList is the §3.4 invariant, and
// the whole reason the union lives in tui.New: gx is the persisted default on a
// machine where its binary has gone missing, so the caller's availability
// filter leaves it out of the row list. Esc starts providerDefault regardless,
// so a picker that dropped the row would preselect cursor, tag nothing, and lie
// about what Esc does.
func TestProviderPickerShowsDefaultMissingFromTheList(t *testing.T) {
	m := newPickerRows(t, agent.GxProvider(), agent.DefaultProviders())
	if got := providerRowNames(m.providers); !reflect.DeepEqual(got, []string{"cursor", "grok", "gx"}) {
		t.Fatalf("rows %q, want the absent default inserted in registry order", got)
	}
	if m.providerCursor != 2 {
		t.Fatalf("cursor %d, want the inserted default at 2", m.providerCursor)
	}
	if row := pickerRowLine(t, m, "gx"); !strings.Contains(row, "default") {
		t.Fatalf("the inserted row is not tagged default: %q", row)
	}
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(Model)
	tm, _ = m.Update(runCmd(cmd))
	m = tm.(Model)
	if m.snap.Provider.Name != "gx" {
		t.Fatalf("esc started %q, not the row the picker tagged", m.snap.Provider.Name)
	}
}

// TestProviderPickerZeroConfigIsHermetic pins §3.5: a Config that names no
// providers falls back to agent.DefaultProviders() and never to the host's
// PATH. A tui that resolved binaries itself would satisfy every other test here
// and still make the two existing picker goldens differ between a dev box with
// gx installed and CI, so this case runs with a gx on PATH and demands two
// rows.
func TestProviderPickerZeroConfigIsHermetic(t *testing.T) {
	t.Setenv("CRAZE_AGENT_BIN", "")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gx"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	isolateSkillsHome(t)
	m := New(Config{Workspace: t.TempDir()})
	if got := providerRowNames(m.providers); !reflect.DeepEqual(got, []string{"cursor", "grok"}) {
		t.Fatalf("zero Config rows %q, want [cursor grok]", got)
	}
}

// TestProviderPickerClickMovesHighlight pins AC 3's click half: a press on a
// row moves the highlight and does nothing else. The picker stays up and no
// session is built, so a mis-click cannot start an agent.
func TestProviderPickerClickMovesHighlight(t *testing.T) {
	m := newPickerRows(t, agent.CursorProvider(), agent.Providers())
	r := m.lay.Dialog
	// Row 0 is the top border and row 1 the title, so row 2 is the first
	// provider.
	for i, name := range []string{"cursor", "grok", "gx"} {
		out := clickXY(t, m, r.X+2, r.Y+2+i)
		if out.providerCursor != i {
			t.Fatalf("click on %q moved the cursor to %d, want %d", name, out.providerCursor, i)
		}
		if !out.pickingProvider || out.dialog != dialogProvider {
			t.Fatalf("click on %q closed the picker", name)
		}
		if out.sess != nil {
			t.Fatalf("click on %q started a session", name)
		}
	}
}

func TestFrameGoldenProviderPickerThreeRows(t *testing.T) {
	isolateSkillsHome(t)
	m := goldenPicker(t, agent.CursorProvider(), agent.Providers(), 100, 30)
	assertGolden(t, "provider-picker-3rows-100x30", 100, 30, plainView(m))
}

// markedPickerRow is the one row of a rendered body that carries the focus
// gutter, as plain text. Reading it back out of the body is what makes the
// window assertions about what the box draws rather than about the numbers the
// plan returned, and finding two of them would be its own bug.
func markedPickerRow(t *testing.T, body []string, shown int) string {
	t.Helper()
	got := ""
	for _, row := range body[1 : 1+shown] {
		if text := plain(row); strings.HasPrefix(text, dialogCursorMark) {
			if got != "" {
				t.Fatalf("two rows carry the focus gutter:\n%s", strings.Join(body, "\n"))
			}
			got = text
		}
	}
	return got
}

// TestProviderPickerShortBudgetKeepsTheSelectedRow is §3.10: a box too short
// for every row draws the window the cursor is in, not the top of the list.
// Truncating from the bottom hid the selected row — the one Enter starts — and,
// because the click path shares the plan, made it unclickable too. Three rows
// with gx able to be the default at index 2 is what made it reachable, but a
// two-row picker had it at any budget of two.
//
// The budgets go to the body directly: craze's 40x12 floor still fits three
// rows (TestFrameGoldenProviderPickerFloor pins that), so a fourth provider or
// a band that takes a transcript row is what would reach this through a real
// WindowSizeMsg.
func TestProviderPickerShortBudgetKeepsTheSelectedRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cursor int
	}{
		{"last", 2},
		{"middle", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newPickerRows(t, agent.CursorProvider(), agent.Providers())
			m.providerCursor = tc.cursor
			want := m.providers[tc.cursor].Name()
			// 2 is the smallest budget that keeps a row at all, 4 the largest
			// that still drops one: a squeeze with and without the footer, and
			// a window of one row and of two.
			for budget := 2; budget <= 4; budget++ {
				top, shown, footer := m.providerDialogPlan(budget)
				if shown <= 0 || shown >= len(m.providers) {
					t.Fatalf("fixture: budget %d is not a squeeze (top %d, shown %d)", budget, top, shown)
				}
				if tc.cursor < top || tc.cursor >= top+shown {
					t.Fatalf("budget %d drew rows [%d,%d), which does not hold the cursor at %d", budget, top, top+shown, tc.cursor)
				}
				body := m.providerDialogBody(dialogMaxWidth-dialogBorder, budget)
				wantRows := shown + 1 // the title
				if footer {
					wantRows++
				}
				if got := len(body); got != wantRows {
					t.Fatalf("budget %d drew %d rows for a %d-row window, want %d", budget, got, shown, wantRows)
				}
				row := markedPickerRow(t, body, shown)
				if !strings.HasPrefix(row, dialogCursorMark+want) {
					t.Fatalf("budget %d: the row under the cursor is %q, want %q", budget, row, want)
				}
			}
		})
	}
}

// TestProviderPickerShortBoxClickSelectsTheVisibleRow is the click half of
// §3.10: the hit test maps a press back through the same offset the renderer
// used. The box here draws rows 1 and 2, so a press on its first row is grok —
// the bug this replaces answered cursor, the row that is not on screen.
//
// The shortened box is set on m.lay.Dialog because View, providerDialogBody
// and providerDialogClick all read that one rect, so the frame the test clicks
// on is the frame the model draws.
func TestProviderPickerShortBoxClickSelectsTheVisibleRow(t *testing.T) {
	m := newPickerRows(t, agent.GxProvider(), agent.Providers())
	if m.providerCursor != 2 {
		t.Fatalf("fixture: gx default preselects %d, want 2", m.providerCursor)
	}
	// Two borders, the title, two rows and the footer.
	m.lay.Dialog.H = dialogBorder + 4
	top, shown, _ := m.providerDialogPlan(m.lay.Dialog.H - dialogBorder)
	if top != 1 || shown != 2 {
		t.Fatalf("fixture: the squeezed box draws rows [%d,%d), want [1,3)", top, top+shown)
	}
	if row := pickerRowLine(t, m, "grok"); !strings.HasPrefix(row, dialogNoMark+"grok") {
		t.Fatalf("grok is not the box's first row: %q", row)
	}
	r := m.lay.Dialog
	// Row 0 is the top border and row 1 the title, so row 2 is the window's
	// first provider.
	for i, want := range []struct {
		name  string
		index int
	}{{"grok", 1}, {"gx", 2}} {
		out := clickXY(t, m, r.X+2, r.Y+2+i)
		if out.providerCursor != want.index {
			t.Fatalf("click on %q moved the cursor to %d, want %d", want.name, out.providerCursor, want.index)
		}
		if !out.pickingProvider || out.dialog != dialogProvider {
			t.Fatalf("click on %q closed the picker", want.name)
		}
		if out.sess != nil {
			t.Fatalf("click on %q started a session", want.name)
		}
	}
	// A press on the footer is still not a row, and the rows the window scrolled
	// past are not reachable by clicking below it either.
	if out := clickXY(t, m, r.X+2, r.Y+4); out.providerCursor != 2 {
		t.Fatalf("a click on the footer moved the cursor to %d", out.providerCursor)
	}
}

// TestProviderPickerWindowDoesNotScrollWhenEverythingFits is the other half of
// §3.10: the offset answers a squeeze, it is not a scroller. A window that
// tracked the cursor unconditionally would put the default's row at the top of
// a full-size box and move every picker golden with it.
func TestProviderPickerWindowDoesNotScrollWhenEverythingFits(t *testing.T) {
	for _, def := range []agent.Provider{agent.CursorProvider(), agent.GxProvider()} {
		m := newPickerRows(t, def, agent.Providers())
		top, shown, footer := m.providerDialogPlan(m.lay.Dialog.H - dialogBorder)
		if top != 0 || shown != len(m.providers) || !footer {
			t.Fatalf("%s default: top %d, shown %d, footer %v — want the whole list from row 0", def.Name(), top, shown, footer)
		}
		body := m.providerDialogBody(m.lay.Dialog.W-dialogBorder, m.lay.Dialog.H-dialogBorder)
		first := strings.TrimPrefix(plain(body[1]), dialogCursorMark)
		if got := strings.Fields(first); len(got) == 0 || got[0] != "cursor" {
			t.Fatalf("%s default: the box opens on %q, want cursor", def.Name(), plain(body[1]))
		}
	}
}

// TestFrameGoldenProviderPickerFloor is the three-row picker at 40x12, the
// smallest frame craze draws, with the default on the last row. Every row still
// fits there, so the golden is what says the window did not scroll at the size
// where the box is closest to giving a row up.
func TestFrameGoldenProviderPickerFloor(t *testing.T) {
	isolateSkillsHome(t)
	m := goldenPicker(t, agent.GxProvider(), agent.Providers(), 40, 12)
	assertGolden(t, "provider-picker-3rows-40x12", 40, 12, plainView(m))
}
