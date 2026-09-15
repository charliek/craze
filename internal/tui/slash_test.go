package tui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

// ------------------------------------------------------------------ fixtures

// slashModel is a started model whose agent advertises exactly the named
// commands, so a test owns the whole non-builtin half of the catalog.
func slashModel(t *testing.T, cols, rows int, names ...string) (Model, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	setStubCommands(stub, names...)
	return startStub(t, stub, t.TempDir(), cols, rows), stub
}

func setStubCommands(stub *Stub, names ...string) {
	cmds := make([]agent.CommandInfo, 0, len(names))
	for _, n := range names {
		cmds = append(cmds, agent.CommandInfo{Name: n, Description: "advertised " + n})
	}
	stub.SetCommands(cmds)
}

// slashScrolled is the scrolling fixture: twelve advertised commands, a
// mid-message token so the builtins stay out of the list, and the selection
// walked down n rows.
func slashScrolled(t *testing.T, n int) (Model, *Stub) {
	t.Helper()
	m, stub := slashModel(t, 100, 30, lettered("cmd", 12)...)
	m = draft(m, "see /cmd")
	for i := 0; i < n; i++ {
		m = pressKey(t, m, tea.KeyDown)
	}
	return m, stub
}

// caret puts a draft in the composer with the cursor at a byte offset and lets
// one Update settle the layout, which is where the menu's selection and window
// are synced. It is what a user who typed the draft and moved back leaves.
func caret(m Model, value string, off int) Model {
	m.input.SetValue(value)
	m.setComposerCursor(value, off)
	tm, _ := m.Update(refreshSnapMsg{})
	return tm.(Model)
}

// draft is caret with the cursor where typing leaves it: at the end.
func draft(m Model, value string) Model { return caret(m, value, len(value)) }

func slashNames(m Model) []string {
	items := m.filteredSlash()
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

// assertSent checks what the transcript recorded as the user's message.
func assertSent(t *testing.T, m Model, want string) {
	t.Helper()
	if got := strings.Join(texts(m, entryUser), ""); got != want {
		t.Fatalf("user %q, want %q", got, want)
	}
}

// lettered is n names sharing a prefix and ending in a, b, c … — enough for a
// list longer than the band, in an order the filter keeps.
func lettered(prefix string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, prefix+string(rune('a'+i)))
	}
	return out
}

// TestBuiltinNamesMatchTheAgentList pins the two halves of the builtin list to
// each other: the menu's table here, with its descriptions, and
// agent.BuiltinSlashNames, which the session folds into the names a plugin row
// may not take. They have to name the same commands, or a plugin shipping one
// of them would be offered bare by one half and shadowed by the other.
func TestBuiltinNamesMatchTheAgentList(t *testing.T) {
	var menu []string
	for _, it := range builtinSlash() {
		menu = append(menu, it.Name)
	}
	if strings.Join(menu, ",") != strings.Join(agent.BuiltinSlashNames(), ",") {
		t.Fatalf("menu builtins %v, agent builtins %v", menu, agent.BuiltinSlashNames())
	}
}

// ------------------------------------------------------------------ §3.1

func TestSlashTokenRules(t *testing.T) {
	// A row with no wantOK wants no token, and its want/wantStart/wantEnd
	// columns are not read.
	for _, tc := range []struct {
		name               string
		value              string
		cursor             int
		wantOK             bool
		want               string
		wantStart, wantEnd int
	}{
		{name: "at offset zero", value: "/he", cursor: 3, wantOK: true, want: "he", wantEnd: 3},
		{name: "after a space", value: "see /he", cursor: 7, wantOK: true, want: "he", wantStart: 4, wantEnd: 7},
		{name: "after a tab", value: "see\t/he", cursor: 7, wantOK: true, want: "he", wantStart: 4, wantEnd: 7},
		{name: "after a newline", value: "see\n/he", cursor: 7, wantOK: true, want: "he", wantStart: 4, wantEnd: 7},
		{name: "after a non-breaking space", value: "see\u00a0/he", cursor: 8, wantOK: true, want: "he", wantStart: 5, wantEnd: 8},
		{name: "inside a word", value: "foo/bar", cursor: 5},
		{name: "a url", value: "https://x", cursor: 9},
		{name: "a bare slash at offset zero", value: "/", cursor: 1, wantOK: true, wantEnd: 1},
		{name: "a bare slash mid message", value: "1 /", cursor: 3, wantOK: true, wantStart: 2, wantEnd: 3},
		{name: "cursor at the inclusive end", value: "/help", cursor: 5, wantOK: true, want: "help", wantEnd: 5},
		{name: "cursor inside the token", value: "/help", cursor: 2, wantOK: true, want: "help", wantEnd: 5},
		{name: "cursor at the start of the token", value: "see /help", cursor: 4, wantOK: true, want: "help", wantStart: 4, wantEnd: 9},
		{name: "cursor before the trailing space", value: "/help ", cursor: 5, wantOK: true, want: "help", wantEnd: 5},
		{name: "cursor on the space after the token", value: "/help ", cursor: 6},
		{name: "cursor in the next word", value: "/help me", cursor: 8},
		{name: "an empty draft", value: "", cursor: 0},
		{name: "a token on the second line", value: "one\n/two", cursor: 8, wantOK: true, want: "two", wantStart: 4, wantEnd: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, end, name, ok := slashToken(tc.value, tc.cursor)
			if !tc.wantOK {
				if ok {
					t.Fatalf("want no token, got %q at [%d,%d)", name, start, end)
				}
				return
			}
			if !ok {
				t.Fatalf("want token %q, got none", tc.want)
			}
			if name != tc.want || start != tc.wantStart || end != tc.wantEnd {
				t.Fatalf("got %q at [%d,%d), want %q at [%d,%d)",
					name, start, end, tc.want, tc.wantStart, tc.wantEnd)
			}
		})
	}
}

// TestComposerCursorOffsetMultiByte guards the rune/byte boundary the whole
// trigger rests on: the column bubbles reports is in runes.
func TestComposerCursorOffsetMultiByte(t *testing.T) {
	for _, value := range []string{"héllo /ga", "日本 /ga", "a\nhéllo /ga"} {
		m, _ := slashModel(t, 80, 24, "gauntlet")
		m = draft(m, value)
		if got := m.composerCursorOffset(); got != len(value) {
			t.Fatalf("%q: offset %d, want %d", value, got, len(value))
		}
		start, end, name, ok := slashToken(value, m.composerCursorOffset())
		if !ok || name != "ga" || end != len(value) || value[start:end] != "/ga" {
			t.Fatalf("%q: token %q at [%d,%d) ok=%v", value, name, start, end, ok)
		}
		// Mid-token too: the cursor one rune back is still inside it.
		m = caret(m, value, len(value)-1)
		if got := m.composerCursorOffset(); got != len(value)-1 {
			t.Fatalf("%q: mid-token offset %d, want %d", value, got, len(value)-1)
		}
		if !m.slashMenuOpen() {
			t.Fatalf("%q: the menu should be open mid-token", value)
		}
	}
}

// ------------------------------------------------------------------ §3.2

// TestSlashBuiltinsOnlyWhereEnterRunsThem is pin 5: a builtin is offered
// exactly where parseSlashLine would dispatch it.
func TestSlashBuiltinsOnlyWhereEnterRunsThem(t *testing.T) {
	m, _ := slashModel(t, 80, 30, "help-me", "research")

	m = draft(m, "  /he")
	if names := slashNames(m); !slices.Contains(names, "help") {
		t.Fatalf("leading whitespace still runs /help, so it is offered: %v", names)
	}

	m = draft(m, "see /he")
	names := slashNames(m)
	if slices.Contains(names, "help") {
		t.Fatalf("a mid-message token can only be sent: %v", names)
	}
	if !slices.Contains(names, "help-me") {
		t.Fatalf("the advertised command is still offered: %v", names)
	}

	// A newline anywhere in the draft is what parseSlashLine refuses, so the
	// builtins go even though the token opens the message.
	m = caret(m, "/he\nmore", 3)
	if names := slashNames(m); slices.Contains(names, "help") {
		t.Fatalf("a multi-line draft can only be sent: %v", names)
	}
	if names := slashNames(m); !slices.Contains(names, "help-me") {
		t.Fatalf("the advertised command survives the newline: %v", names)
	}

	// The mode builtins keep today's capability gate on top of the position
	// rule: an agent that advertises no modes is not offered /plan.
	m = draft(m, "/p")
	if names := slashNames(m); !slices.Contains(names, "plan") {
		t.Fatalf("modes are advertised, so /plan is offered: %v", names)
	}
	m.snap.Modes = nil
	if names := slashNames(m); slices.Contains(names, "plan") {
		t.Fatalf("/plan needs the capability: %v", names)
	}
}

// TestFilteredSlashPrefixThenSubstring is pin 3's fixture.
func TestFilteredSlashPrefixThenSubstring(t *testing.T) {
	m, _ := slashModel(t, 80, 30, "ask-panel", "panel-first", "my-panel", "panel-second")
	// Mid-message, so the builtins stay out of the fixture.
	m = draft(m, "see /panel")
	want := []string{"panel-first", "panel-second", "ask-panel", "my-panel"}
	if got := slashNames(m); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("filter order %v, want %v", got, want)
	}
}

// TestSlashCatalogRejectsUntypableNames: a name with whitespace in it could
// never be one token, so it never reaches the menu; descriptions are folded
// onto one line at catalog time.
func TestSlashCatalogRejectsUntypableNames(t *testing.T) {
	m, _ := slashModel(t, 80, 30)
	m.snap.Commands = []agent.CommandInfo{
		{Name: "two words", Description: "not one token"},
		{Name: "ctrl\x07bell", Description: "control character"},
		// bubbles' sanitizer drops U+FFFD, so a name carrying one would leave
		// SetValue shorter than acceptSlash measured its new cursor against.
		{Name: "bad\ufffdrune", Description: "the replacement rune"},
		{Name: "bad\xffbyte", Description: "invalid UTF-8, which range reads as U+FFFD"},
		// U+009B is a single-code-point CSI: a terminal reading C1 in UTF-8
		// would take an advertised description as an escape sequence.
		{Name: "good", Description: "first line\nsecond\u009b2J line"},
	}
	names := make([]string, 0, 3)
	desc := ""
	for _, it := range m.slashCatalog() {
		if it.Builtin {
			continue
		}
		names = append(names, it.Name)
		desc = it.Desc
	}
	if strings.Join(names, ",") != "good" {
		t.Fatalf("catalog kept an untypable name: %v", names)
	}
	if desc != "first line second2J line" {
		t.Fatalf("the description was not folded onto one safe line: %q", desc)
	}
}

// ------------------------------------------------------------------ §3.3

func TestAcceptSlashReplacesOnlyTheToken(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      string
		cursor     int
		want       string
		wantCursor int
	}{
		{"mid draft, the following space absorbed", "see /res  more", 8, "see /research  more", 14},
		{"at the end of a line, before the newline", "/res\nmore", 4, "/research \nmore", 10},
		{"at the very end of the draft", "see /res", 8, "see /research ", 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := slashModel(t, 80, 30, "research")
			m = caret(m, tc.value, tc.cursor)
			m = pressKey(t, m, tea.KeyTab)
			if m.input.Value() != tc.want {
				t.Fatalf("draft %q, want %q", m.input.Value(), tc.want)
			}
			if got := m.composerCursorOffset(); got != tc.wantCursor {
				t.Fatalf("cursor at %d, want %d", got, tc.wantCursor)
			}
			// The inserted space ends the token, so the menu closes by the
			// rule rather than by a flag.
			if m.slashMenuOpen() {
				t.Fatal("the menu should be closed after an accept")
			}
		})
	}
}

func TestAcceptSlashOutOfRangeIsANoOp(t *testing.T) {
	m, _ := slashModel(t, 80, 30, "research")
	m = draft(m, "see /res")
	for _, i := range []int{-1, 1, 99} {
		if next := m.acceptSlash(i); next.input.Value() != "see /res" {
			t.Fatalf("acceptSlash(%d) changed the draft: %q", i, next.input.Value())
		}
	}
}

// TestAcceptSlashRestoresCursorOnWrappedDraft is §3.3's one non-obvious
// mechanic: SetValue leaves the cursor at the end of the buffer, so the
// restore has to step back over wrapped rows as well as logical ones.
func TestAcceptSlashRestoresCursorOnWrappedDraft(t *testing.T) {
	m, _ := slashModel(t, 40, 30, "research")
	head := strings.Repeat("x", 30)
	tail := strings.Repeat("y", 30)
	value := "alpha\n" + head + " /res " + tail + "\nomega"
	tokenEnd := len("alpha\n") + len(head) + len(" /res")
	m = caret(m, value, tokenEnd)
	if m.input.Line() != 1 {
		t.Fatalf("fixture: the cursor should be on the second logical line, not %d", m.input.Line())
	}
	if rows := m.composerRows(); rows < 4 {
		t.Fatalf("fixture: the middle line should wrap, draft is %d rows", rows)
	}

	m = pressKey(t, m, tea.KeyTab)
	want := "alpha\n" + head + " /research " + tail + "\nomega"
	if m.input.Value() != want {
		t.Fatalf("draft %q", m.input.Value())
	}
	wantCursor := len("alpha\n") + len(head) + len(" /research ")
	if got := m.composerCursorOffset(); got != wantCursor {
		t.Fatalf("cursor at %d, want %d", got, wantCursor)
	}
	if m.input.Line() != 1 {
		t.Fatalf("the cursor left the second line: row %d", m.input.Line())
	}
	// What the restore is for: the next rune typed lands where the cursor is.
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'Z'}})
	m = tm.(Model)
	if got := m.input.Value(); got != want[:wantCursor]+"Z"+want[wantCursor:] {
		t.Fatalf("the typed rune landed elsewhere: %q", got)
	}
}

func TestEnterOnTheExactNameSends(t *testing.T) {
	m, _ := slashModel(t, 80, 30, "research")
	m = draft(m, "/research")
	if !m.slashActive() {
		t.Fatal("fixture: the menu should be up on the exactly typed name")
	}
	m, cmd := press(m, enter())
	if m.status != statusWorking || cmd == nil {
		t.Fatalf("the exactly typed row sends: status %v", m.status)
	}
	assertSent(t, m, "/research")
}

func TestEnterAcceptsThenTheNextEnterSends(t *testing.T) {
	m, _ := slashModel(t, 80, 30, "research")
	m = draft(m, "/resea")
	m, cmd := press(m, enter())
	if m.input.Value() != "/research " {
		t.Fatalf("the first enter accepts: %q", m.input.Value())
	}
	if m.status == statusWorking || cmd != nil {
		t.Fatal("the first enter must not send")
	}
	m, cmd = press(m, enter())
	if m.status != statusWorking || cmd == nil {
		t.Fatal("the second enter sends")
	}
	assertSent(t, m, "/research")
}

// TestBareSlashEnterAccepts is the recorded behaviour change: "/" + Enter used
// to send the prompt "/", and now accepts the first row. Esc then Enter still
// sends it.
func TestBareSlashEnterAccepts(t *testing.T) {
	m, _ := slashModel(t, 80, 30, "research")
	m = draft(m, "/")
	first := m.filteredSlash()[0].Name
	m = pressKey(t, m, tea.KeyEnter)
	if m.input.Value() != "/"+first+" " {
		t.Fatalf("bare slash + enter should accept %q, draft is %q", first, m.input.Value())
	}
	if m.status == statusWorking {
		t.Fatal("bare slash + enter must not send")
	}

	m = draft(m, "/")
	m = pressKey(t, m, tea.KeyEsc)
	m, cmd := press(m, enter())
	if m.status != statusWorking || cmd == nil {
		t.Fatal("esc then enter sends the bare slash as a prompt")
	}
	assertSent(t, m, "/")
}

// TestSlashAcceptInsideAQueueEdit: the menu outranks the edit's save, so the
// row completes the text being edited and the next Enter writes it back.
func TestSlashAcceptInsideAQueueEdit(t *testing.T) {
	m, stub := queueWorking(t)
	setStubCommands(stub, "research")
	m = typeEnter(t, m, "one")
	id := m.snap.Queue[0].ID
	m = pressKey(t, m, tea.KeyUp)
	m = pressKey(t, m, tea.KeyEnter)
	if m.queueEdit != id {
		t.Fatalf("fixture: edit mode is on %q, want %q", m.queueEdit, id)
	}

	m = draft(m, "/resea")
	m = pressKey(t, m, tea.KeyEnter)
	if m.input.Value() != "/research " {
		t.Fatalf("enter should accept inside an edit: %q", m.input.Value())
	}
	if m.queueEdit != id {
		t.Fatalf("the accept must not end the edit: %q", m.queueEdit)
	}
	m = pressKey(t, m, tea.KeyEnter)
	if m.queueEdit != "" {
		t.Fatal("the next enter saves the edit")
	}
	if len(m.snap.Queue) != 1 || m.snap.Queue[0].Text != "/research" {
		t.Fatalf("the row was not saved: %+v", m.snap.Queue)
	}
}

// TestEscHidesOneTokenOnly: Esc records the token, so moving the cursor into
// another one brings the menu back without touching the draft.
func TestEscHidesOneTokenOnly(t *testing.T) {
	m, _ := slashModel(t, 80, 30, "research")
	const value = "/he /res"
	m = caret(m, value, 3)
	if !m.slashActive() {
		t.Fatal("fixture: the menu should be up on /he")
	}
	m = pressKey(t, m, tea.KeyEsc)
	if m.input.Value() != value {
		t.Fatalf("esc touched the draft: %q", m.input.Value())
	}
	if m.slashMenuOpen() {
		t.Fatal("esc hides the menu for this token")
	}
	// Right five times walks the cursor into the second token.
	for i := 0; i < 5; i++ {
		m = pressKey(t, m, tea.KeyRight)
	}
	if got := m.composerCursorOffset(); got != len(value) {
		t.Fatalf("fixture: the cursor is at %d, want %d", got, len(value))
	}
	if !m.slashMenuOpen() {
		t.Fatal("moving into another token reopens the menu")
	}
	// And back: the hidden token is still hidden.
	m = caret(m, value, 3)
	if m.slashMenuOpen() {
		t.Fatal("the token esc hid stays hidden")
	}
}

// TestSlashHideDoesNotOutliveTheDraft: the token Esc hid belongs to the draft
// it was in, so the same token typed into the next draft opens the menu again.
func TestSlashHideDoesNotOutliveTheDraft(t *testing.T) {
	m, _ := slashModel(t, 80, 30, "research")
	m = draft(m, "/res")
	m = pressKey(t, m, tea.KeyEsc)
	if m.slashMenuOpen() {
		t.Fatal("fixture: esc hides the menu")
	}
	m = pressKey(t, m, tea.KeyEnter)
	if m.status != statusWorking {
		t.Fatalf("enter under a hidden menu sends: %v", m.status)
	}
	m = draft(m, "/res")
	if !m.slashMenuOpen() {
		t.Fatal("the sent draft took its hide with it")
	}
}

// TestEscUnderARunningTurnHidesThenCancels is the accepted consequence of the
// mid-message trigger: Esc on any /word hides the menu first.
func TestEscUnderARunningTurnHidesThenCancels(t *testing.T) {
	m, stub := queueWorking(t)
	setStubCommands(stub, "research")
	m = draft(m, "see /res")
	if !m.slashActive() {
		t.Fatal("fixture: the menu should be up mid-message")
	}
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil || m.cardsCancelled {
		t.Fatal("the first esc hides the menu and nothing else")
	}
	if m.status != statusWorking {
		t.Fatalf("the turn is still running: %v", m.status)
	}
	m, cmd = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil || !m.cardsCancelled {
		t.Fatal("the second esc cancels the turn")
	}
}

// ------------------------------------------------------------------ §3.4

func TestSlashSelectionResetsOnTokenChange(t *testing.T) {
	m, _ := slashScrolled(t, 3)
	if len(m.filteredSlash()) != 12 {
		t.Fatalf("fixture: %d matches", len(m.filteredSlash()))
	}
	if m.slashSel != 3 {
		t.Fatalf("selection %d, want 3", m.slashSel)
	}
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = tm.(Model)
	if m.input.Value() != "see /cmda" {
		t.Fatalf("draft %q", m.input.Value())
	}
	if m.slashSel != 0 || m.slashTop != 0 {
		t.Fatalf("a new token restarts the list: sel %d top %d", m.slashSel, m.slashTop)
	}
}

// TestSlashWindowFollowsTheSelection: ↑/↓ wrap, PgUp/PgDn move by the granted
// rows, and the window always holds the selection.
func TestSlashWindowFollowsTheSelection(t *testing.T) {
	m, _ := slashScrolled(t, 10)
	granted := m.lay.Region(regionOverlay).Height()
	if granted != slashMaxRows {
		t.Fatalf("fixture: %d rows granted, want %d", granted, slashMaxRows)
	}
	assertSlashWindow(t, m)
	if m.slashSel != 10 || m.slashTop != 3 {
		t.Fatalf("sel %d top %d, want 10 and 3", m.slashSel, m.slashTop)
	}
	// ↑/↓ wrap.
	for i := 0; i < 2; i++ {
		m = pressKey(t, m, tea.KeyDown)
	}
	if m.slashSel != 0 || m.slashTop != 0 {
		t.Fatalf("down past the last row wraps to the first: sel %d top %d", m.slashSel, m.slashTop)
	}
	m = pressKey(t, m, tea.KeyUp)
	if m.slashSel != 11 {
		t.Fatalf("up from the first row wraps to the last: %d", m.slashSel)
	}
	assertSlashWindow(t, m)
	// PgUp/PgDn move by the granted rows and clamp instead of wrapping.
	m = pressKey(t, m, tea.KeyPgDown)
	if m.slashSel != 11 {
		t.Fatalf("pgdn clamps at the last row: %d", m.slashSel)
	}
	m = pressKey(t, m, tea.KeyPgUp)
	if m.slashSel != 11-granted {
		t.Fatalf("pgup moves by %d rows: %d", granted, m.slashSel)
	}
	assertSlashWindow(t, m)
	for i := 0; i < 3; i++ {
		m = pressKey(t, m, tea.KeyPgUp)
	}
	if m.slashSel != 0 {
		t.Fatalf("pgup clamps at the first row: %d", m.slashSel)
	}
}

// TestSlashResizeKeepsTheSelectionInTheWindow: the window is settled in
// relayout, so every height that draws a band draws the selected row.
func TestSlashResizeKeepsTheSelectionInTheWindow(t *testing.T) {
	m, _ := slashScrolled(t, 10)
	for _, rows := range []int{30, 24, 20, 16, 14, 12} {
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: rows})
		m = tm.(Model)
		granted := m.lay.Region(regionOverlay).Height()
		if granted < 1 {
			t.Fatalf("%d rows: the band was granted nothing", rows)
		}
		assertSlashWindow(t, m)
		row := "/" + m.filteredSlash()[m.slashSel].Name
		if !strings.Contains(plainView(m), row) {
			t.Fatalf("%d rows: the selected row %q is not drawn:\n%s", rows, row, plainView(m))
		}
	}
}

// assertSlashWindow is the invariant every selection move owes the band: the
// selected row is one of the rows drawn.
func assertSlashWindow(t *testing.T, m Model) {
	t.Helper()
	items := m.filteredSlash()
	granted := m.lay.Region(regionOverlay).Height()
	if m.slashSel < 0 || m.slashSel >= len(items) {
		t.Fatalf("selection %d is outside the %d matches", m.slashSel, len(items))
	}
	if m.slashTop < 0 || m.slashTop > max(0, len(items)-granted) {
		t.Fatalf("window top %d with %d matches and %d rows", m.slashTop, len(items), granted)
	}
	if m.slashSel < m.slashTop || m.slashSel >= m.slashTop+granted {
		t.Fatalf("selection %d is outside the window [%d,%d)", m.slashSel, m.slashTop, m.slashTop+granted)
	}
	drawn := strings.Split(plain(m.slashMenuView(m.lay)), "\n")
	if len(drawn) != granted {
		t.Fatalf("the band drew %d rows, the layout granted %d", len(drawn), granted)
	}
	if want := "/" + items[m.slashSel].Name; !strings.Contains(drawn[m.slashSel-m.slashTop], want) {
		t.Fatalf("row %d of the band is not %q: %q", m.slashSel-m.slashTop, want, drawn[m.slashSel-m.slashTop])
	}
}

// TestCommandsUpdateClampsTheSlashSelection: an available_commands_update can
// shrink the catalog under an open menu, and no key may then index past it.
func TestCommandsUpdateClampsTheSlashSelection(t *testing.T) {
	m, stub := slashScrolled(t, 10)
	if m.slashSel != 10 {
		t.Fatalf("fixture: selection %d", m.slashSel)
	}
	setStubCommands(stub, "cmda", "cmdb", "cmdc")
	m = poke(t, m)
	if len(m.filteredSlash()) != 3 {
		t.Fatalf("the catalog should have shrunk: %v", slashNames(m))
	}
	assertSlashWindow(t, m)
	if m.slashSel != 2 {
		t.Fatalf("the selection clamps to the last match: %d", m.slashSel)
	}
	m = pressKey(t, m, tea.KeyTab)
	if m.input.Value() != "see /cmdc " {
		t.Fatalf("the clamped row is what tab accepts: %q", m.input.Value())
	}
}

// TestZeroGrantedRowsInterceptsNothing: a band the layout has no room for is
// the same as no band at all, for every key.
func TestZeroGrantedRowsInterceptsNothing(t *testing.T) {
	m, stub := queueWorking(t)
	setStubCommands(stub, "research")
	m = typeEnter(t, m, "queued")
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: minFrameRows})
	m = tm.(Model)
	const value = "one\ntwo\nsee /res"
	m = draft(m, value)
	if !m.slashMenuOpen() {
		t.Fatal("fixture: the menu is open by the token rule")
	}
	if granted := m.lay.Region(regionOverlay).Height(); granted != 0 {
		t.Fatalf("fixture: this frame should have no room for the band, got %d rows:\n%s",
			granted, plainView(m))
	}
	if m.slashActive() {
		t.Fatal("a band granted no rows is not active")
	}

	m = pressKey(t, m, tea.KeyTab)
	if m.input.Value() != value {
		t.Fatalf("tab completed against a band nobody can see: %q", m.input.Value())
	}
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.slashHideKey != "" {
		t.Fatalf("esc recorded a hide for an invisible band: %q", m.slashHideKey)
	}
	if cmd == nil || !m.cardsCancelled {
		t.Fatal("esc should have cancelled the turn")
	}
}

// TestDialogSuppressesTheSlashBand: a dialog is a layer over the transcript,
// and used to leave the band drawn underneath it.
func TestDialogSuppressesTheSlashBand(t *testing.T) {
	m, _ := slashModel(t, 100, 30, "research")
	m = draft(m, "/mo")
	if m.lay.Region(regionOverlay).Empty() {
		t.Fatal("fixture: the band should be up on /mo")
	}
	m = poke(t, m.openModelDialog())
	if m.slashMenuOpen() || m.overlayView(m.lay) != "" || !m.lay.Region(regionOverlay).Empty() {
		t.Fatalf("a dialog suppresses the band: %+v", m.lay.Region(regionOverlay))
	}
	if m.input.Value() != "/mo" {
		t.Fatalf("the draft is kept: %q", m.input.Value())
	}
	m = pressKey(t, m, tea.KeyEsc)
	if m.dialog != dialogNone {
		t.Fatal("esc closes the dialog")
	}
	if !m.slashMenuOpen() || m.lay.Region(regionOverlay).Empty() {
		t.Fatal("closing the dialog brings the band back")
	}
}

// TestSlashNameColumnCapsAndClamps is §3.4's name column: it is measured over
// every match rather than the window, so the descriptions do not shuffle
// sideways on each ↓, and slashNameCap stops one pathological name from taking
// the whole row — past the cap that one name is clamped and the column stops.
func TestSlashNameColumnCapsAndClamps(t *testing.T) {
	long := "zz-" + strings.Repeat("l", 40)
	m, _ := slashModel(t, 100, 30, "zz-short", long)
	m = draft(m, "see /zz")
	items := m.filteredSlash()
	if len(items) != 2 {
		t.Fatalf("fixture: matches %v", slashNames(m))
	}
	if w := slashNameWidth(items); w != slashNameCap {
		t.Fatalf("name column is %d cells, want the cap %d", w, slashNameCap)
	}
	rows := strings.Split(plain(m.slashMenuView(m.lay)), "\n")
	if len(rows) != 2 {
		t.Fatalf("the band drew %d rows:\n%s", len(rows), strings.Join(rows, "\n"))
	}
	// Both descriptions start in the same column: the gutter, the capped name
	// column, and the two spaces between them.
	const descAt = len(agentGutterBlank) + slashNameCap + 2
	for i, r := range rows {
		cells := []rune(r)
		if len(cells) < descAt+10 {
			t.Fatalf("row %d is only %d cells: %q", i, len(cells), r)
		}
		if got := string(cells[descAt : descAt+10]); got != "advertised" {
			t.Fatalf("row %d starts its description at %q, want %q", i, got, "advertised")
		}
	}
	// The name the column cannot hold is clamped into it, not past it.
	name := string([]rune(rows[1])[len(agentGutterBlank) : len(agentGutterBlank)+slashNameCap])
	if !strings.HasPrefix(name, "/zz-l") || !strings.HasSuffix(name, "…") {
		t.Fatalf("the long name was not clamped into the column: %q", name)
	}
}

// TestSlashMarks is §3.4's scroll marks: the count rides the first row with an
// ▲ once the window has moved, the ▼ rides the last while there is more below,
// a list that fits carries neither, and a band of one row is both ends at once
// — where the count already says what the arrows would.
// TestSlashMarkSurvivesTheNarrowestBand: the count is the one thing on the row
// the user cannot re-derive, so at the 40-column minimum the name column yields
// to it rather than letting dialogTagSeg drop it for want of a separating cell.
// codex review found a 28-cell name column and a four-digit count leaving the
// mark nowhere to go, and the row drawn with no mark at all.
func TestSlashMarkSurvivesTheNarrowestBand(t *testing.T) {
	names := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		names = append(names, fmt.Sprintf("a-very-long-command-name-%04d", i))
	}
	m, _ := slashModel(t, minFrameCols, 30, names...)
	m = caret(m, "see /a-very", len("see /a-very"))
	m.slashSel, m.slashTop = 998, 991
	m.relayout(false)
	row := plain(strings.Split(m.slashMenuView(m.lay), "\n")[0])
	if !strings.Contains(row, "999/1000") || !strings.Contains(row, "▲") {
		t.Fatalf("the narrowest band dropped its mark: %q", row)
	}
	if lipgloss.Width(row) > minFrameCols {
		t.Fatalf("row is %d cells wide, over the frame's %d: %q", lipgloss.Width(row), minFrameCols, row)
	}
}

// ------------------------------------------------------------------ §3.5

// pluginEntry is one discovered entry as the resolver sees it. The menu never
// reads a body or a path, so this fixture carries neither.
func pluginEntry(plugin, name, desc string) agent.PluginEntry {
	return agent.PluginEntry{
		Plugin:      plugin,
		Name:        name,
		Description: desc,
		Kind:        agent.PluginKindCommand,
	}
}

// slashPluginModel is slashModel with plugin rows on the snapshot, named by the
// same resolver the live session runs so the menu under test is never handed a
// spelling the session would not have produced.
func slashPluginModel(t *testing.T, entries []agent.PluginEntry, advertised ...string) Model {
	t.Helper()
	m, stub := slashModel(t, 100, 30, advertised...)
	stub.SetPlugins(agent.ResolvePluginNames(entries, advertised, false))
	m.refreshSnap()
	return m
}

// TestSlashCatalogOrderACPThenPluginThenDisk is §3.5's source order: what the
// agent advertised wins, because craze expands none of it; craze's own plugin
// rows come next; the disk skill walk is last.
func TestSlashCatalogOrderACPThenPluginThenDisk(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	writeSkillMD(t, filepath.Join(ws, ".cursor", "skills", "on-disk", "SKILL.md"), `---
name: on-disk
description: found by the walk
---
`)
	stub := NewStub()
	setStubCommands(stub, "advertised")
	stub.SetPlugins(agent.ResolvePluginNames([]agent.PluginEntry{
		pluginEntry("probe-plugin", "probe-echo", "the plugin one"),
	}, []string{"advertised"}, false))
	m := startStub(t, stub, ws, 100, 30)

	var got []string
	for _, it := range m.slashCatalog() {
		if it.Builtin {
			continue
		}
		got = append(got, it.Name)
	}
	if want := "advertised,probe-echo,on-disk"; strings.Join(got, ",") != want {
		t.Fatalf("catalog order %v, want %v", got, want)
	}
	it, ok := catalogByName(m, "probe-echo")
	if !ok {
		t.Fatal("the plugin row is missing from the catalog")
	}
	if it.Plugin != "probe-plugin" || it.Qualified != "probe-plugin:probe-echo" || it.Skill {
		t.Fatalf("plugin row %+v", it)
	}
}

// TestSlashCatalogSanitisesPluginRows: a row craze owns goes through the same
// two gates every advertised row does — an untypable name never reaches the
// menu, and a description is folded onto the one line the band has for it.
func TestSlashCatalogSanitisesPluginRows(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	setStubCommands(stub)
	stub.SetPlugins([]agent.PluginCommand{
		{Plugin: "ok", Bare: "good", Display: "good", Qualified: "ok:good", Description: "first line\nsecond line"},
		{Plugin: "two words", Bare: "spaced", Display: "spaced", Qualified: "two words:spaced"},
		{Plugin: "ok", Bare: "bell", Display: "be\x07ll", Qualified: "ok:be\x07ll"},
	})
	m := startStub(t, stub, t.TempDir(), 100, 30)
	var got []string
	for _, it := range m.slashCatalog() {
		if it.Builtin {
			continue
		}
		got = append(got, it.Name)
	}
	if strings.Join(got, ",") != "good" {
		t.Fatalf("catalog kept an untypable plugin row: %v", got)
	}
	it, _ := catalogByName(m, "good")
	if it.labeledDesc() != "first line second line (ok)" {
		t.Fatalf("labelled description %q", it.labeledDesc())
	}
}

// TestSlashLabeledDesc is the row's label: which plugin a row craze owns came
// from — the only thing telling four /rescue commands apart — and the (skill)
// the disk walk has carried since 009. Both kinds of plugin entry are labelled
// the same way; a row with nothing to describe it still says where it is from.
func TestSlashLabeledDesc(t *testing.T) {
	for _, tc := range []struct {
		name string
		it   slashItem
		want string
	}{
		{"an advertised row is its description alone", slashItem{Name: "a", Desc: "advertised"}, "advertised"},
		{"a disk skill is labelled", slashItem{Name: "a", Desc: "on disk", Skill: true}, "on disk (skill)"},
		{"a disk skill with no description is the label", slashItem{Name: "a", Skill: true}, "(skill)"},
		{"a plugin command names its plugin", slashItem{Name: "watch-pr", Desc: "watch it", Plugin: "git-commands"},
			"watch it (git-commands)"},
		{"a plugin skill names its plugin too", slashItem{Name: "gauntlet", Desc: "run it", Plugin: "forge"},
			"run it (forge)"},
		{"a plugin row with no description is the plugin", slashItem{Name: "watch-pr", Plugin: "git-commands"},
			"(git-commands)"},
		{"a description of spaces is no description", slashItem{Name: "a", Desc: "   ", Plugin: "p"}, "(p)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.it.labeledDesc(); got != tc.want {
				t.Fatalf("labeledDesc = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFilteredSlashFourBuckets is §3.5's filter, and the whole of it in one
// fixture: every row here is in a different bucket, and the catalog order they
// arrive in is the reverse of the order they come out in, so nothing about the
// result could be an accident of how they were listed.
//
// The two qualified buckets are plugin-only. my-zeta-cmd is an advertised row
// whose own name holds the query, so it lands in the name-substring bucket
// ahead of the plugin row that only matches through its plugin id.
func TestFilteredSlashFourBuckets(t *testing.T) {
	m := slashPluginModel(t, []agent.PluginEntry{
		// Bucket 4: only "my-zeta:tool" holds the query, and not at the front.
		pluginEntry("my-zeta", "tool", "the fourth bucket"),
		// Bucket 2: the bare name is unique, so the query only reaches it
		// through the qualified spelling craze resolved for it.
		pluginEntry("zeta", "runner", "the second bucket"),
		// Bucket 1: a plugin row matched by its displayed name is in the same
		// bucket as any other row, and in it exactly once.
		pluginEntry("zeta", "zeta-run", "the first bucket, from a plugin"),
	}, "my-zeta-cmd", "zeta-first")
	m = draft(m, "see /zeta")
	want := []string{"zeta-first", "zeta-run", "runner", "my-zeta-cmd", "tool"}
	if got := slashNames(m); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("filter order %v, want %v", got, want)
	}
}

// TestFilteredSlashQualifiedIsPluginOnly: an advertised name with a colon in it
// is one name, not two spellings. grok advertises codex:review, and the menu
// has completed it since 009 — putting it in the qualified buckets would move
// it behind every prefix hit for no reason at all.
func TestFilteredSlashQualifiedIsPluginOnly(t *testing.T) {
	m := slashPluginModel(t, []agent.PluginEntry{
		pluginEntry("codex", "rescue", "the plugin one"),
	}, "codex:review")
	m = draft(m, "see /codex:")
	// The advertised row is a bucket-1 hit, because "codex:review" is its
	// name. Nothing else here claims "rescue", so the plugin row is displayed
	// bare and only reaches the query through bucket 2 — which is exactly the
	// bucket the advertised row must not be treated as belonging to.
	want := []string{"codex:review", "rescue"}
	if got := slashNames(m); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("filter order %v, want %v", got, want)
	}
}

// TestAcceptSlashPluginSpelling is the accept rule: the displayed name, unless
// the token already carries a colon — which is the user asking for the plugin
// path and the only signal there is.
func TestAcceptSlashPluginSpelling(t *testing.T) {
	entries := []agent.PluginEntry{pluginEntry("probe-plugin", "probe-echo", "probe")}
	for _, tc := range []struct{ name, typed, want string }{
		{"a bare token accepts the bare name", "/pro", "/probe-echo "},
		{"a colon in the token accepts the qualified one", "/probe-plugin:pro", "/probe-plugin:probe-echo "},
		{"the whole plugin id and nothing else", "/probe-plugin:", "/probe-plugin:probe-echo "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := slashPluginModel(t, entries)
			m = draft(m, tc.typed)
			if len(m.filteredSlash()) != 1 {
				t.Fatalf("fixture: matches %v", slashNames(m))
			}
			m = pressKey(t, m, tea.KeyTab)
			if m.input.Value() != tc.want {
				t.Fatalf("draft %q, want %q", m.input.Value(), tc.want)
			}
		})
	}
}

// TestAcceptSlashQualifiedRowIgnoresTheColonRule: a row already displayed
// qualified has one spelling, so the colon changes nothing about it.
func TestAcceptSlashQualifiedRowIgnoresTheColonRule(t *testing.T) {
	m := slashPluginModel(t, []agent.PluginEntry{
		pluginEntry("alpha", "rescue", "one"),
		pluginEntry("beta", "rescue", "the other"),
	})
	m = draft(m, "/alpha:")
	m = pressKey(t, m, tea.KeyTab)
	if m.input.Value() != "/alpha:rescue " {
		t.Fatalf("draft %q", m.input.Value())
	}
}

// TestAcceptACPNameWithColon is §3.5's limit on the colon rule: it belongs to
// rows craze owns. grok advertises codex:review under exactly that name, and a
// /codex: query has to keep completing it the way it did before plugins.
func TestAcceptACPNameWithColon(t *testing.T) {
	m, _ := slashModel(t, 100, 30, "codex:review")
	m = draft(m, "/codex:")
	if names := slashNames(m); len(names) != 1 || names[0] != "codex:review" {
		t.Fatalf("fixture: matches %v", names)
	}
	m = pressKey(t, m, tea.KeyTab)
	if m.input.Value() != "/codex:review " {
		t.Fatalf("draft %q, want %q", m.input.Value(), "/codex:review ")
	}
}

// TestEnterOnEitherPluginSpellingSends: both spellings resolve to the same
// entry on the wire, so both are the row fully typed and neither is completed
// into the other.
func TestEnterOnEitherPluginSpellingSends(t *testing.T) {
	entries := []agent.PluginEntry{pluginEntry("probe-plugin", "probe-echo", "probe")}
	for _, typed := range []string{"/probe-echo", "/probe-plugin:probe-echo"} {
		t.Run(typed, func(t *testing.T) {
			m := slashPluginModel(t, entries)
			m = draft(m, typed)
			if !m.slashActive() {
				t.Fatal("fixture: the menu should be up on the typed name")
			}
			if !m.slashExactlyTyped() {
				t.Fatalf("%q is the row fully typed", typed)
			}
			m, cmd := press(m, enter())
			if m.status != statusWorking || cmd == nil {
				t.Fatalf("the exactly typed row sends: status %v", m.status)
			}
			assertSent(t, m, typed)
		})
	}
}

// TestPluginNamedLikeABuiltinShowsQualified: the builtins are in the taken set
// the resolver names against, so a plugin shipping "help" is offered as
// helper:help and the one bare /help in the menu is still craze's own.
func TestPluginNamedLikeABuiltinShowsQualified(t *testing.T) {
	m := slashPluginModel(t, []agent.PluginEntry{
		pluginEntry("helper", "help", "the plugin one"),
	})
	m = draft(m, "/help")
	want := []string{"help", "helper:help"}
	if got := slashNames(m); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("matches %v, want %v", got, want)
	}
	it, ok := catalogByName(m, "help")
	if !ok || !it.Builtin {
		t.Fatalf("the bare /help row is craze's own: %+v", it)
	}
	m = pressKey(t, m, tea.KeyDown)
	m = pressKey(t, m, tea.KeyTab)
	if m.input.Value() != "/helper:help " {
		t.Fatalf("draft %q", m.input.Value())
	}
}

func TestSlashMarks(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		i, top, granted, n, sel int
		want                    string
	}{
		{"a list that fits is unmarked", 0, 0, 8, 8, 0, ""},
		{"the first row counts", 0, 0, 8, 33, 0, "1/33"},
		{"the count follows the selection, not the window", 3, 3, 8, 33, 10, "11/33 ▲"},
		{"the rows between are bare", 5, 3, 8, 33, 10, ""},
		{"the last row points down", 10, 3, 8, 33, 10, "▼"},
		{"the end of the list points nowhere", 32, 25, 8, 33, 30, ""},
		{"one row carries the count alone", 4, 4, 1, 33, 4, "5/33"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := slashMark(tc.i, tc.top, tc.granted, tc.n, tc.sel); got != tc.want {
				t.Fatalf("slashMark = %q, want %q", got, tc.want)
			}
		})
	}
}
