package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// Plan 046 A4 (SF-154, SF-144): the lines a user acts on — the startup
// picker's reason and fix, the session list's note, the /provider popup's
// refusal — wrap instead of being clipped, and dialogWrap breaks a path at
// its separators.

// wordwrapped is dialogWrap as it was before A4: ansi's word wrap, then a
// hard wrap at the width. Text with no path wider than a row still wraps
// exactly so.
func wordwrapped(s string, inner int) []string {
	return strings.Split(ansi.Hardwrap(ansi.Wordwrap(s, inner, ""), inner, true), "\n")
}

// assertRowsFit fails when a row is wider than inner cells.
func assertRowsFit(t *testing.T, label string, rows []string, inner int) {
	t.Helper()
	for i, r := range rows {
		if w := ansi.StringWidth(r); w > inner {
			t.Errorf("%s: row %d is %d cells, wider than %d: %q", label, i, w, inner, r)
		}
	}
}

// TestDialogWrapBreaksAPathAtItsSlashes (SF-144): a path wider than a row is
// broken after the last "/" that fits on each row — its first piece on the
// row before it when one fits there — so read off row by row it is the path,
// whole; the text after it goes on from its last piece's row. A hyphen in a
// path is no place to break it.
func TestDialogWrapBreaksAPathAtItsSlashes(t *testing.T) {
	for _, tc := range []struct {
		label string
		s     string
		inner int
		want  []string
	}{
		{"the connect box's line", "Stored in /home/charliek/projects/craze-plan046/native/providers.toml.", 20,
			[]string{"Stored in /home/", "charliek/projects/", "craze-plan046/", "native/", "providers.toml."}},
		{"no room for a piece on the row before", "a long word: /srv/teams/x.log", 16,
			[]string{"a long word:", "/srv/teams/x.log"}},
		{"text after it", "see /var/log/craze/host-logs/a.log for why", 16,
			[]string{"see /var/log/", "craze/host-logs/", "a.log for why"}},
		{"a segment wider than a row, cut at the width", "/a/bbbbbbbbbbbbbbbbbbbbbbbbb/c", 10,
			[]string{"/a/", "bbbbbbbbbb", "bbbbbbbbbb", "bbbbb/c"}},
		{"its leading slash is no place to cut", "/bbbbbbbbbbbb/c", 10,
			[]string{"/bbbbbbbbb", "bbb/c"}},
	} {
		got := dialogWrap(tc.s, tc.inner)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.label, got, tc.want)
		}
		assertRowsFit(t, tc.label, got, tc.inner)
	}
}

// TestDialogWrapHardBreaksAWordWithNoSlash: a word wider than the row with no
// "/" in it is broken at the width, mid-word, exactly as before A4 — and text
// with no path wider than a row (a short path included, which moves to the
// next row whole) wraps exactly as before too.
func TestDialogWrapHardBreaksAWordWithNoSlash(t *testing.T) {
	const long = "see averyveryverylongwordwithnoslash here"
	if got, want := dialogWrap(long, 10), []string{"see", "averyveryv", "erylongwor", "dwithnosla", "sh", "here"}; !slices.Equal(got, want) {
		t.Errorf("the separator-free word: %q, want %q", got, want)
	}
	for _, tc := range []struct {
		s     string
		inner int
	}{
		{long, 10}, {long, 20}, {long, 52},
		// Paths no wider than the row: each moves to the next row whole.
		{"install cursor-agent, or set [agents].cursor in ~/.craze/config.toml", 20},
		{"install cursor-agent, or set [agents].cursor in ~/.craze/config.toml", 52},
		{"Stored in ~/.craze/native/providers.toml.", 52},
		{"a hyphenated-word-that-is-long-but-has-no-slash at all", 10},
		{"a hyphenated-word-that-is-long-but-has-no-slash at all", 20},
		{"", 10},
	} {
		if got, want := dialogWrap(tc.s, tc.inner), wordwrapped(tc.s, tc.inner); !slices.Equal(got, want) {
			t.Errorf("%q at %d: %q, want ansi's %q", tc.s, tc.inner, got, want)
		}
	}
}

// TestDialogWrapCountsCells: widths are terminal cells — a wide rune is two
// — both where a "/" is found to fit and where a path is cut at the width.
func TestDialogWrapCountsCells(t *testing.T) {
	for _, tc := range []struct {
		label string
		s     string
		inner int
		want  []string
	}{
		{"pieces of wide runes", "日本語/日本語/日本語", 10, []string{"日本語/", "日本語/", "日本語"}},
		{"cut at the width between wide runes", "/x/日本語日本語日本語", 7, []string{"/x/", "日本語", "日本語", "日本語"}},
		// A rune wider than the row still moves on: it is a row of its own.
		{"a row narrower than a rune", "/a/日", 1, []string{"/", "a", "/", "日"}},
	} {
		got := dialogWrap(tc.s, tc.inner)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.label, got, tc.want)
		}
		if tc.inner > 1 {
			assertRowsFit(t, tc.label, got, tc.inner)
		}
	}
}

// TestCapRowsEndsACutPartInAnEllipsis: rows past the cap go, and the last
// kept ends in "…" within the width — beside its text when there is room,
// over its last cell when there is not.
func TestCapRowsEndsACutPartInAnEllipsis(t *testing.T) {
	rows := []string{"one two", "three four", "five"}
	if got := capRows(rows, 3, 10); !slices.Equal(got, rows) {
		t.Errorf("uncut: %q", got)
	}
	if got, want := capRows(rows, 2, 11), []string{"one two", "three four…"}; !slices.Equal(got, want) {
		t.Errorf("cut, room for it: %q, want %q", got, want)
	}
	if got, want := capRows(rows, 2, 10), []string{"one two", "three fou…"}; !slices.Equal(got, want) {
		t.Errorf("cut, no room: %q, want %q", got, want)
	}
	if got := capRows(rows, 0, 10); got != nil {
		t.Errorf("no rows: %q", got)
	}
	if &capRows(rows, 2, 10)[0] == &rows[0] {
		t.Error("capRows wrote into its argument")
	}
}

// ------------------------------------------------------- the startup picker

// TestProviderPickerKeepsItsHeightAcrossEveryRow (A-3): the detail lines'
// height is fixed for the picker's life — the cursor walked across every row,
// a refusal shown (providerErr, by Enter and by Esc) and cleared, the box is
// one height throughout — and at 80 columns cursor's fix shows its config
// path whole, wrapped at a word. At 40x12 the fix lines are the first rows
// given up and the selected row always stays.
func TestProviderPickerKeepsItsHeightAcrossEveryRow(t *testing.T) {
	log := &builtLog{}
	m := availPicker(t, availPickerConfig(t, agent.CursorProvider(), availFixture, nil, log.build), 80, 24)
	h := m.lay.Dialog.H
	if !strings.Contains(plainView(m), "│~/.craze/config.toml ") {
		t.Fatalf("cursor's fix does not show its config path whole:\n%s", plainView(m))
	}
	same := func(m Model, how string) {
		t.Helper()
		if m.lay.Dialog.H != h {
			t.Fatalf("%s: the box is %d rows, it opened at %d:\n%s", how, m.lay.Dialog.H, h, plainView(m))
		}
	}
	n := len(m.providers)
	for i := range n + 1 {
		same(m, "on row "+m.providers[m.providerCursor].Name())
		refused, _ := press(m, enter())
		if refused.providerErr != "" {
			same(refused, "a refusal on "+m.providers[m.providerCursor].Name())
			moved, _ := press(refused, tea.KeyMsg{Type: tea.KeyDown})
			if moved.providerErr != "" {
				t.Fatal("moving off the refused row kept the refusal")
			}
			same(moved, "the refusal cleared")
		}
		if i < n {
			m, _ = press(m, tea.KeyMsg{Type: tea.KeyDown})
		}
	}
	onGrok, _ := press(m, tea.KeyMsg{Type: tea.KeyDown}) // the walk ended on cursor
	esc, _ := press(onGrok, tea.KeyMsg{Type: tea.KeyEsc})
	if esc.providerErr != availCursorRefusal {
		t.Fatalf("fixture: Esc from grok was not refused: %q", esc.providerErr)
	}
	same(esc, "Esc's refusal")

	// At 40x12 the box has room for one detail line: every fix is given up,
	// and the row under the cursor is drawn whichever it is.
	narrow := availPicker(t, availPickerConfig(t, agent.CursorProvider(), availFixture, nil, log.build), 40, 12)
	inner, budget := narrow.lay.Dialog.W-dialogBorder, narrow.lay.Dialog.H-dialogBorder
	nh := narrow.lay.Dialog.H
	for range len(narrow.providers) {
		if narrow.lay.Dialog.H != nh {
			t.Fatalf("at 40x12 the box changed height on %s", narrow.providers[narrow.providerCursor].Name())
		}
		if got := narrow.providerDetailShown(inner, budget); got != 1 {
			t.Fatalf("at 40x12 %d detail lines, want the first alone", got)
		}
		view := plainView(narrow)
		for _, fix := range []string{"install cursor-agent", "craze auth login"} {
			if strings.Contains(view, fix) {
				t.Fatalf("at 40x12 a fix line is drawn:\n%s", view)
			}
		}
		want := dialogCursorMark + narrow.providers[narrow.providerCursor].DisplayName()
		if !strings.Contains(view, "│"+want) {
			t.Fatalf("at 40x12 the selected row %q is not drawn:\n%s", want, view)
		}
		narrow, _ = press(narrow, tea.KeyMsg{Type: tea.KeyDown})
	}
}

// TestProviderPickerCutsAPartAtTwoRows: a reason or a fix wider than two rows
// is cut at two, its second ending in "…"; a short box that keeps only the
// first detail line ends it in "…" when the reason runs on.
func TestProviderPickerCutsAPartAtTwoRows(t *testing.T) {
	long := strings.Repeat("the reason runs on and on ", 8)
	fix := strings.Repeat("and so does the fix ", 8)
	log := &builtLog{}
	m := availPicker(t, availPickerConfig(t, agent.CursorProvider(), func() []ProviderAvail {
		return []ProviderAvail{
			{ID: "cursor", State: AvailUnavailable, Reason: long, Fix: fix},
			{ID: "grok", State: AvailReady},
			{ID: "native", State: AvailReady},
		}
	}, nil, log.build), 80, 24)
	inner := m.lay.Dialog.W - dialogBorder
	if all, heads := m.providerDetailRows(inner); all != 4 || heads != 2 {
		t.Fatalf("the detail lines are %d rows, %d of them first parts; want 4 and 2", all, heads)
	}
	lines := m.providerLines(inner, 4)
	for _, i := range []int{1, 3} {
		if !strings.HasSuffix(plain(lines[i]), "…") {
			t.Errorf("row %d of the cut part does not end in …: %q", i, plain(lines[i]))
		}
	}
	if one := m.providerLines(inner, 1); len(one) != 1 || !strings.HasSuffix(plain(one[0]), "…") {
		t.Errorf("the first line alone: %q", one)
	}
	if two := m.providerLines(inner, 2); len(two) != 2 || plain(two[0]) != plain(lines[0]) || plain(two[1]) != plain(lines[1]) {
		t.Errorf("the fix given up: %q", two)
	}
	assertRowsFit(t, "the cut lines", []string{plain(lines[0]), plain(lines[1]), plain(lines[2]), plain(lines[3])}, inner)
}

// ------------------------------------------------------- the session list

// hostLogPath is the fixture's host log, under a home long enough that the
// path cannot fit on an 80-column row.
const hostLogPath = "/home/a-deliberately-long-user-name/workspaces/craze-golden/.cache/craze/host-logs/0123456789ab.log"

// failedStartList is the list at cols×rows over a dispatch whose session
// started and failed, its note ending with the host's log as
// launchedBackend.Start names it.
func failedStartList(t *testing.T, cols, rows int) Model {
	t.Helper()
	m, _, hb, _ := dispatchModel(t, cols, rows)
	hb.startErr = errors.New("cursor-agent exited before it answered (the session host's log: " + hostLogPath + ")")
	m = selectKey(t, m, runKey("lumen"))
	m, _ = typeList(t, m, "go")
	m, cmd := press(m, enter())
	msg := dispatched(t, mustCmd(t, cmd, "sessDispatch"))
	if msg.out != dispatchFailed {
		t.Fatalf("the outcome %v: %v", msg.out, msg.err)
	}
	tm, _ := m.Update(msg)
	return tm.(Model)
}

// assertPathWhole fails unless rows hold path whole, read off row by row,
// broken only after a "/".
func assertPathWhole(t *testing.T, rows []string, path string) {
	t.Helper()
	joined := strings.Join(rows, "\n")
	at := strings.Index(joined, path[:strings.IndexByte(path[1:], '/')+2])
	if at < 0 {
		t.Fatalf("the path does not start on any row: %q", rows)
	}
	tail := joined[at:]
	if got := strings.ReplaceAll(tail, "\n", ""); !strings.HasPrefix(got, path) {
		t.Fatalf("the path read off the rows is %q, want %q", got, path)
	}
	for i := 0; i < len(path); i++ {
		if tail[0] == '\n' {
			if path[i-1] != '/' {
				t.Fatalf("the path is broken after %q, not after a /", path[:i])
			}
			tail = tail[1:]
		}
		tail = tail[1:]
	}
}

// TestFrameGoldenSessionsHintWrapped (A-4): a failed start's note at 80x24 is
// wrapped over the hint line's three rows — the host's log, the path to read
// next, whole across them, broken only at a "/" — the list giving up the two
// rows it takes.
func TestFrameGoldenSessionsHintWrapped(t *testing.T) {
	m := failedStartList(t, 80, 24)
	assertFrameGolden(t, "sessions-hint-wrapped-80x24", 80, 24, plainView(m),
		[]string{"could not start a session in ~/projects/lumen:", "(the session host's log:"}, nil)
	rows := sessHintLines(m)
	if len(rows) < 2 || len(rows) > sessNoteMaxRows {
		t.Fatalf("the note is %d rows, want 2 to %d: %q", len(rows), sessNoteMaxRows, rows)
	}
	if strings.HasSuffix(rows[len(rows)-1], "…") {
		t.Fatalf("the note was cut: %q", rows)
	}
	assertPathWhole(t, rows, hostLogPath)
	if len(strings.Split(plainView(m), "\n")) != 24 {
		t.Fatal("the frame is not 24 rows")
	}
}

// sessRegionsOf is m's list laid out, and the frame it draws checked against
// it: exactly the screen's rows, the hint's rows last.
func sessRegionsOf(t *testing.T, m Model) sessRegions {
	t.Helper()
	reg := m.sessRegions(m.sessLines())
	footer := sessFooterRows
	if m.sessList.in.on {
		footer = sessInputRows
	}
	if got := reg.body + reg.pop + len(reg.hint); got != m.height-sessHeaderRows-footer+1 {
		t.Fatalf("at %dx%d the regions add up to %d rows (body %d, popup %d, hint %d), want %d", m.width, m.height, got,
			reg.body, reg.pop, len(reg.hint), m.height-sessHeaderRows-footer+1)
	}
	if reg.body < sessBodyMinRows {
		t.Fatalf("at %dx%d the list keeps %d rows, under its minimum %d", m.width, m.height, reg.body, sessBodyMinRows)
	}
	rows := strings.Split(plainView(m), "\n")
	if len(rows) != m.height {
		t.Fatalf("at %dx%d the frame is %d rows", m.width, m.height, len(rows))
	}
	hint := rows[len(rows)-len(reg.hint):]
	for i, r := range reg.hint {
		if strings.TrimRight(hint[i], " ") != strings.TrimRight(plain(r), " ") {
			t.Fatalf("hint row %d drawn %q, laid out %q", i, hint[i], plain(r))
		}
	}
	if rule := rows[len(rows)-len(reg.hint)-1]; strings.Trim(rule, "─") != "" {
		t.Fatalf("the row over the hint is %q, not the footer's rule", rule)
	}
	return reg
}

// TestTheHintNoteTakesItsRowsFromTheList (A-4): the arithmetic itself. A
// note wraps over up to three rows taken from the list's, which keeps its
// two-row minimum — at 40x10 with the input the note is cut to fit and ends
// in "…" — and a resize while it shows lays it out again at the new size.
func TestTheHintNoteTakesItsRowsFromTheList(t *testing.T) {
	m := failedStartList(t, 80, 24)
	full := sessRegionsOf(t, m)
	if len(full.hint) != 3 || full.body != 24-sessHeaderRows-sessInputRows-2 {
		t.Fatalf("at 80x24: hint %d rows, body %d", len(full.hint), full.body)
	}
	note := m.sessList.note

	// Resized while the note shows: at 40x10 the list is at its minimum and
	// the note has what is left — its first row and the two the list can give.
	for _, size := range []struct{ cols, rows, hint, body int }{
		{40, 10, 3, 2},
		{40, 12, 3, 4},
		{100, 30, 3, 30 - sessHeaderRows - sessInputRows - 2},
		{80, 24, 3, 24 - sessHeaderRows - sessInputRows - 2},
	} {
		tm, _ := m.Update(tea.WindowSizeMsg{Width: size.cols, Height: size.rows})
		m = tm.(Model)
		if m.sessList.note != note {
			t.Fatalf("the resize to %dx%d dropped the note", size.cols, size.rows)
		}
		reg := sessRegionsOf(t, m)
		if len(reg.hint) != size.hint || reg.body != size.body {
			t.Fatalf("at %dx%d: hint %d rows, body %d; want %d and %d", size.cols, size.rows, len(reg.hint), reg.body,
				size.hint, size.body)
		}
		if cut := strings.HasSuffix(plain(reg.hint[len(reg.hint)-1]), "…"); cut != (size.cols == 40) {
			t.Fatalf("at %dx%d the note cut %v: %q", size.cols, size.rows, cut, reg.hint)
		}
	}

	// A short note is one row, the list keeping every other.
	m.sessNote("started in ~/projects/lumen", sessNoteOK)
	if reg := sessRegionsOf(t, m); len(reg.hint) != 1 || reg.body != 24-sessHeaderRows-sessInputRows {
		t.Fatalf("a short note: hint %d rows, body %d", len(reg.hint), reg.body)
	}

	// Without an input the footer is two rows: at 40x10 the list has six,
	// and gives two of them to the note.
	lst, _, _ := sessModel(t, 40, 10)
	lst = richList(t, lst)
	lst.sessNote("could not close "+strings.Repeat("a long title ", 6)+": "+hostLogPath, sessNoteErr)
	if reg := sessRegionsOf(t, lst); len(reg.hint) != 3 || reg.body != 4 {
		t.Fatalf("no input at 40x10: hint %d rows, body %d", len(reg.hint), reg.body)
	}
}

// TestTheHintNoteGivesWayToThePopupAndTheArmedClose (A-4): with the input,
// its popup and a note at once, the popup keeps its rows and the note has
// what the list can give over its minimum — at 40x10 none, so one row, cut;
// an armed close's hint is said over a note, on one row; and so is a Ctrl+C
// window's.
func TestTheHintNoteGivesWayToThePopupAndTheArmedClose(t *testing.T) {
	long := "could not start a session in ~/projects/lumen: cursor-agent exited (the session host's log: " + hostLogPath + ")"
	m, fs, _ := newSessModel(t, 40, 10)
	m = newList(t, m, fs)
	m, _ = typeList(t, m, "@")
	m.sessNote(long, sessNoteErr)
	reg := sessRegionsOf(t, m)
	if reg.pop != 2 || reg.body != sessBodyMinRows || len(reg.hint) != 1 {
		t.Fatalf("40x10, popup and note: popup %d, body %d, hint %d rows", reg.pop, reg.body, len(reg.hint))
	}
	if !strings.HasSuffix(plain(reg.hint[0]), "…") {
		t.Fatalf("the note squeezed to one row is not cut: %q", plain(reg.hint[0]))
	}

	big, fs, _ := newSessModel(t, 80, 24)
	big = newList(t, big, fs)
	big, _ = typeList(t, big, "@")
	before := sessRegionsOf(t, big)
	big.sessNote(long, sessNoteErr)
	reg = sessRegionsOf(t, big)
	if reg.pop != before.pop || len(reg.hint) != 3 || reg.body != before.body-2 {
		t.Fatalf("80x24, popup and note: popup %d (was %d), body %d (was %d), hint %d rows", reg.pop, before.pop,
			reg.body, before.body, len(reg.hint))
	}

	// An armed close is said over the note, on one row.
	armed, _, _ := sessModel(t, 80, 24)
	armed = richList(t, armed)
	armed = selectKey(t, armed, runKey("pty"))
	armed, _ = press(armed, tea.KeyMsg{Type: tea.KeyCtrlX})
	armed.sessNote(long, sessNoteErr)
	reg = sessRegionsOf(t, armed)
	if len(reg.hint) != 1 || !strings.Contains(plain(reg.hint[0]), "ctrl+x again closes it") {
		t.Fatalf("an armed close under a note: %q", reg.hint)
	}
	// A Ctrl+C window, likewise.
	quit, _ := press(armed, tea.KeyMsg{Type: tea.KeyCtrlC})
	quit.sessNote(long, sessNoteErr)
	reg = sessRegionsOf(t, quit)
	if len(reg.hint) != 1 || !strings.Contains(plain(reg.hint[0]), "again quits craze") {
		t.Fatalf("a Ctrl+C window under a note: %q", reg.hint)
	}
}

// ------------------------------------------------------- the /provider popup

// TestFrameGoldenSessionsNewProviderRefused (A-4): /provider's refusal at
// 80x24 is wrapped under the candidates — its fix, the config path, shown —
// and the popup's rows are the ones laid out.
func TestFrameGoldenSessionsNewProviderRefused(t *testing.T) {
	m, fs, _ := newSessModel(t, 80, 24)
	as := &availSessions{startSessions: fs, answer: availFixture()}
	m.sessions = as
	m, cmd := openAvailList(t, m, as)
	m = takeOpenRead(t, m, cmd)
	m = providersLoaded(t, m, "/provider ")
	m, _ = press(m, enter())
	if m.sessList.in.cmd.ans.Note != availCursorRefusal {
		t.Fatalf("fixture: Enter on cursor was not refused: %q", m.sessList.in.cmd.ans.Note)
	}
	assertFrameGolden(t, "sessions-new-provider-refused-80x24", 80, 24, plainView(m),
		[]string{"  can't start cursor: cursor-agent not found on PATH — install cursor-agent, or",
			"  set [agents].cursor in ~/.craze/config.toml"}, nil)
	sessRegionsOf(t, m)
}

// TestThePopupsRefusalIsOneLayout (A-4): the popup's height and what it
// draws come from one width-aware layout — at every width and row budget the
// view is as many lines as height says, each the width — and the refusal
// wraps over up to three rows, the last ending in "…" when it is cut, while
// one candidate row is left beside it.
func TestThePopupsRefusalIsOneLayout(t *testing.T) {
	p := newCompletePopup(&listSource{id: "refusal", items: named("a", "b", "c")}, atGrammar)
	p.sync("@", 1, completeEnv{})
	p.refuse("can't start a: " + strings.Repeat("the reason is long ", 12))
	th := Preset("tokyo-night")
	for _, width := range []int{20, 40, 80} {
		for rows := 0; rows <= 9; rows++ {
			h := p.height(width, rows)
			v := p.view(th, width, rows)
			lines := strings.Split(v, "\n")
			if v == "" {
				lines = nil
			}
			if len(lines) != h || h > rows {
				t.Fatalf("width %d, %d rows: height %d, drawn %d lines", width, rows, h, len(lines))
			}
			for _, l := range lines {
				if w := ansi.StringWidth(l); w != width {
					t.Fatalf("width %d, %d rows: a line %d cells wide: %q", width, rows, w, plain(l))
				}
			}
			s := p.shape(width, rows)
			if rows >= 2 && (s.note < 1 || s.note > completeNoteMaxRows || s.shown < 1) {
				t.Fatalf("width %d, %d rows: %+v", width, rows, s)
			}
			if s.note > 0 && s.note < len(p.noteRows(width)) && !strings.HasSuffix(strings.TrimRight(plain(lines[len(lines)-1]), " "), "…") {
				t.Fatalf("width %d, %d rows: the cut refusal does not end in …: %q", width, rows, plain(lines[len(lines)-1]))
			}
		}
	}
	if s := p.shape(80, 9); s.note != 3 || s.shown != 3 {
		t.Fatalf("at 80x9 the refusal and the candidates: %+v", s)
	}
}

// ------------------------------------------------------- the connect box

// TestTheConnectBoxBreaksTheStorePathAtASlash (SF-144): the key step's
// "Stored in <path>" line, its path wider than the box, is broken after a
// "/" — the path whole, read off its rows — never mid-name.
func TestTheConnectBoxBreaksTheStorePathAtASlash(t *testing.T) {
	const file = "/srv/shared-homes/platform-team/craze-home-for-ci/native/providers.toml"
	var m Model
	m.cdlg.file = file
	inner := dialogMaxWidth - dialogBorder
	rows := m.connectKeyHints(modeltable.ProviderInfo{}, inner)
	assertRowsFit(t, "the Stored in line", rows, inner)
	if len(rows) < 2 || !strings.HasPrefix(rows[0], "Stored in /") {
		t.Fatalf("the Stored in line: %q", rows)
	}
	assertPathWhole(t, rows, file+".")
}
