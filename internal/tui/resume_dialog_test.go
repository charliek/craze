package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/sessions"
)

// resumeNow is the clock every resume case reads, so a row's age is a fact
// about the row and not about how long the suite took to get here.
var resumeNow = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

// resumeRow is one index row, `age` old.
func resumeRow(id, provider, title string, age time.Duration) sessions.Row {
	return sessions.Row{
		SessionID: id,
		Provider:  provider,
		CWD:       "/ws",
		Title:     title,
		UpdatedAt: resumeNow.Add(-age),
		CreatedAt: resumeNow.Add(-age),
	}
}

// newResumePicker is the model --resume builds: rows, no session, and a
// LoadSession that records what the picker asked for.
func newResumePicker(t *testing.T, rows []sessions.Row, cols, height int) (Model, *[]sessions.Row) {
	t.Helper()
	isolateSkillsHome(t)
	loaded := &[]sessions.Row{}
	m := New(Config{
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Model:     "grok",
		Yolo:      true,
		Provider:  agent.CursorProvider(),
		Resume:    rows,
		LoadSession: func(p agent.Provider, row sessions.Row) agent.Session {
			*loaded = append(*loaded, row)
			s := NewStub()
			s.SetProvider(p)
			return s
		},
	})
	m.clock = func() time.Time { return resumeNow }
	tm, _ := m.Update(tea.WindowSizeMsg{Width: cols, Height: height})
	return tm.(Model), loaded
}

func threeResumeRows() []sessions.Row {
	return []sessions.Row{
		resumeRow("s-1", "grok", "fix: the flaky pty test", 3*time.Minute),
		resumeRow("s-2", "cursor", "teach the composer to wrap", 2*time.Hour),
		resumeRow("s-3", "gx", "port the docs site", 5*24*time.Hour),
	}
}

func resumeKey(t *testing.T, m Model, k tea.KeyMsg) Model {
	t.Helper()
	tm, _ := m.Update(k)
	return tm.(Model)
}

// TestResumePickerShowsBeforeStart: --resume outranks the provider picker and
// starts nothing. A session built before the user has said which one to load
// would be a session/new — the one thing a resume must never fall back to.
func TestResumePickerShowsBeforeStart(t *testing.T) {
	m, loaded := newResumePicker(t, threeResumeRows(), 100, 30)
	if !m.pickingResume || m.dialog != dialogResume {
		t.Fatalf("dialog=%v pickingResume=%v", m.dialog, m.pickingResume)
	}
	if m.pickingProvider {
		t.Fatal("the provider picker must not be up as well")
	}
	if m.sess != nil || m.started || len(*loaded) != 0 {
		t.Fatal("no session may exist until a row is chosen")
	}
	if m.Init() != nil {
		t.Fatal("Init must not start anything while the picker is up")
	}
	view := plainView(m)
	for _, want := range []string{"resume", "fix: the flaky pty test", "grok", "3m", "2h", "5d"} {
		if !strings.Contains(view, want) {
			t.Fatalf("the picker is missing %q:\n%s", want, view)
		}
	}
}

// TestResumePickerWraps: ↑/↓ and tab/shift-tab are the same motion and both
// wrap, over three rows so the modular arithmetic is really exercised.
func TestResumePickerWraps(t *testing.T) {
	m, _ := newResumePicker(t, threeResumeRows(), 100, 30)
	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
		want []int
	}{
		{"down", tea.KeyMsg{Type: tea.KeyDown}, []int{1, 2, 0}},
		{"tab", tea.KeyMsg{Type: tea.KeyTab}, []int{1, 2, 0}},
		{"up", tea.KeyMsg{Type: tea.KeyUp}, []int{2, 1, 0}},
		{"shift-tab", tea.KeyMsg{Type: tea.KeyShiftTab}, []int{2, 1, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := m
			for i, want := range tc.want {
				cur = resumeKey(t, cur, tc.key)
				if cur.resumeCursor != want {
					t.Fatalf("press %d: cursor %d, want %d", i+1, cur.resumeCursor, want)
				}
			}
		})
	}
}

// TestResumePickerEnterLoadsTheRow: Enter is the whole of the gesture — the
// session is built from the row, its provider is locked (a grok row is started
// by grok however --provider resolved), and the model is replaying before its
// first event, because Start is already reading a transcript back.
func TestResumePickerEnterLoadsTheRow(t *testing.T) {
	m, loaded := newResumePicker(t, threeResumeRows(), 100, 30)
	m = resumeKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	out := tm.(Model)
	if len(*loaded) != 1 || (*loaded)[0].SessionID != "s-2" {
		t.Fatalf("loaded %v", *loaded)
	}
	if out.dialog != dialogNone || out.pickingResume {
		t.Fatal("the picker stayed up after Enter")
	}
	if !out.providerLocked || out.providerDefault.Name() != "cursor" {
		t.Fatalf("provider %q locked=%v", out.providerDefault.Name(), out.providerLocked)
	}
	if !out.replaying {
		t.Fatal("a loaded session is replaying from the moment it is built (§3.5)")
	}
	if out.sess == nil {
		t.Fatal("no session was built")
	}
	assertOwned(t, out)
	if cmd == nil {
		t.Fatal("Enter must return the start batch")
	}
}

// TestResumePickerNilLoadFallsBackToAStub: a LoadSession that builds nothing
// leaves confirmResume's NewStub fallback, and that assignment goes through the
// owner as well.
func TestResumePickerNilLoadFallsBackToAStub(t *testing.T) {
	isolateSkillsHome(t)
	m := New(Config{
		Theme:       "tokyo-night",
		Workspace:   t.TempDir(),
		Yolo:        true,
		Provider:    agent.CursorProvider(),
		Resume:      threeResumeRows(),
		LoadSession: func(agent.Provider, sessions.Row) agent.Session { return nil },
	})
	if !m.pickingResume || m.sess != nil {
		t.Fatal("setup: no resume picker, or a session before a row was chosen")
	}
	assertOwned(t, m)
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	out := tm.(Model)
	if out.pickingResume {
		t.Fatal("setup: Enter did not confirm the row")
	}
	if _, ok := out.sess.(*Stub); !ok {
		t.Fatalf("a nil LoadSession left %T, want the *Stub fallback", out.sess)
	}
	assertOwned(t, out)
}

// TestResumePickerClickLoads: a click on a row loads it. The provider picker
// only moves its highlight, but there is no second gesture to make here — the
// box is the whole of the session's start.
func TestResumePickerClickLoads(t *testing.T) {
	m, loaded := newResumePicker(t, threeResumeRows(), 100, 30)
	r := m.lay.Dialog
	// Row 0 is the top border and row 1 the title, so row 2 is the first row.
	out := clickXY(t, m, r.X+2, r.Y+2+2)
	if len(*loaded) != 1 || (*loaded)[0].SessionID != "s-3" {
		t.Fatalf("loaded %v", *loaded)
	}
	if out.dialog != dialogNone || out.sess == nil {
		t.Fatal("the click did not start the chosen session")
	}
	assertOwned(t, out)
}

// TestResumePickerSwallowsAClickOutside is §3.7's pin: a click outside a
// pre-start picker is swallowed and the dialog stays. Closing it the way an
// ordinary dialog closes would leave a model with no session and no start
// command — a craze that draws a frame and can never do anything.
func TestResumePickerSwallowsAClickOutside(t *testing.T) {
	m, loaded := newResumePicker(t, threeResumeRows(), 100, 30)
	r := m.lay.Dialog
	out := clickXY(t, m, max(r.X-2, 0), r.Y-1)
	if !out.pickingResume || out.dialog != dialogResume {
		t.Fatalf("the click outside closed the picker: dialog=%v picking=%v", out.dialog, out.pickingResume)
	}
	if out.sess != nil || len(*loaded) != 0 {
		t.Fatal("the click outside started a session")
	}
}

// TestResumePickerEscQuitsClean: Esc is "never mind" — the program ends with
// no session, and therefore no startErr, which is craze exiting 0. There is no
// default row: starting somebody's last session because they pressed Esc is
// the opposite of what Esc means.
func TestResumePickerEscQuitsClean(t *testing.T) {
	m, loaded := newResumePicker(t, threeResumeRows(), 100, 30)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	out := tm.(Model)
	if !out.quitting {
		t.Fatal("Esc did not quit")
	}
	if out.startErr != nil {
		t.Fatalf("Esc left an exit status behind: %v", out.startErr)
	}
	if out.sess != nil || len(*loaded) != 0 {
		t.Fatal("Esc started a session")
	}
	if cmd == nil {
		t.Fatal("Esc returned no command")
	}
	if _, ok := runCmd(cmd).(tea.QuitMsg); !ok {
		t.Fatal("Esc's command is not a quit")
	}
}

// TestResumePickerCapsTheList: ten rows, however many the index hands over —
// past that it is a panel and not a picker.
func TestResumePickerCapsTheList(t *testing.T) {
	rows := make([]sessions.Row, 0, 14)
	for i := 0; i < 14; i++ {
		rows = append(rows, resumeRow("s-"+string(rune('a'+i)), "cursor", "session "+string(rune('a'+i)), time.Duration(i)*time.Hour))
	}
	m, _ := newResumePicker(t, rows, 100, 30)
	if len(m.resume) != resumeDialogMax {
		t.Fatalf("%d rows, want %d", len(m.resume), resumeDialogMax)
	}
	// The cap takes the first ten of a list that is already newest-first, so
	// the oldest rows are the ones dropped.
	if m.resume[0].SessionID != "s-a" || m.resume[9].SessionID != "s-j" {
		t.Fatalf("kept %q..%q", m.resume[0].SessionID, m.resume[9].SessionID)
	}
}

// TestResumePickerDropsAnUnknownProvider: a row written by a build that knew a
// provider this one does not stays in the file (a newer craze may know it
// again) but is never offered — there is nothing Enter could start.
func TestResumePickerDropsAnUnknownProvider(t *testing.T) {
	m, _ := newResumePicker(t, []sessions.Row{
		resumeRow("s-1", "claude", "from another build", time.Minute),
		resumeRow("s-2", "grok", "a session craze can start", 2*time.Minute),
	}, 100, 30)
	if len(m.resume) != 1 || m.resume[0].SessionID != "s-2" {
		t.Fatalf("rows %+v", m.resume)
	}
	if strings.Contains(plainView(m), "from another build") {
		t.Fatalf("the unknown row was offered:\n%s", plainView(m))
	}
}

// TestResumePickerDropsAHiddenProvider: a hidden provider resolves by id, but
// its sessions have no loader yet (plan 018 §3.4), so its row is dropped
// exactly as an unknown provider's is — even as the newest row, and even when
// the caller built the list without internal/cli's filter.
func TestResumePickerDropsAHiddenProvider(t *testing.T) {
	plantHidden(t)
	m, _ := newResumePicker(t, []sessions.Row{
		resumeRow("s-1", hiddenID, "a hidden session", time.Minute),
		resumeRow("s-2", "grok", "a session craze can start", 2*time.Minute),
	}, 100, 30)
	if len(m.resume) != 1 || m.resume[0].SessionID != "s-2" {
		t.Fatalf("rows %+v", m.resume)
	}
	if strings.Contains(plainView(m), "a hidden session") {
		t.Fatalf("the hidden row was offered:\n%s", plainView(m))
	}
}

// TestResumeRowClampsTheTitleFirst: the provider and the age are what tell two
// sessions apart, so a title long enough to fill the box gives up its own
// cells rather than theirs.
func TestResumeRowClampsTheTitleFirst(t *testing.T) {
	long := strings.Repeat("make the composer wrap ", 12)
	m, _ := newResumePicker(t, []sessions.Row{resumeRow("s-1", "cursor", long, 90*time.Minute)}, 100, 30)
	row := plain(m.resumeDialogBody(dialogMaxWidth-dialogBorder, 10)[1])
	if !strings.HasSuffix(row, "cursor · 1h") {
		t.Fatalf("the tail was clamped away: %q", row)
	}
	if !strings.Contains(row, "make the composer wrap") {
		t.Fatalf("the title is gone: %q", row)
	}
	if got := ansi.StringWidth(row); got != dialogMaxWidth-dialogBorder {
		t.Fatalf("row is %d cells, want %d: %q", got, dialogMaxWidth-dialogBorder, row)
	}
}

// TestResumeAge is the one unit a row shows: minutes under an hour, hours
// under a day, days after that, and never a negative one.
func TestResumeAge(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{0, "0m"},
		{90 * time.Second, "1m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{23*time.Hour + 59*time.Minute, "23h"},
		{24 * time.Hour, "1d"},
		{5*24*time.Hour + time.Hour, "5d"},
		{-time.Hour, "0m"},
	} {
		if got := resumeAge(resumeNow, resumeNow.Add(-tc.age)); got != tc.want {
			t.Fatalf("%v ago: %q, want %q", tc.age, got, tc.want)
		}
	}
	if got := resumeAge(resumeNow, time.Time{}); got != "" {
		t.Fatalf("a row with no timestamp has no age, got %q", got)
	}
}

// TestFrameGoldenResumePicker is what `craze --resume` opens on: three rows,
// three providers, newest first, one of them with a colon in its title.
func TestFrameGoldenResumePicker(t *testing.T) {
	m, _ := newResumePicker(t, threeResumeRows(), 100, 30)
	assertGolden(t, "resume-picker-100x30", 100, 30, plainView(m))
}

// TestFrameGoldenResumePickerShort is the same picker with more rows than the
// box can hold: the window is what fits, the cursor is inside it, and the list
// is clipped from the bottom rather than the box covering another band.
//
// §5 calls this golden "footer dropped", but the footer is only given up at an
// inner budget below three rows — a box four rows tall — and craze's shortest
// frame still leaves the dialog seven. The squeeze a real terminal can reach
// is this one: rows go, the hint stays. The footer's own drop is pinned by
// TestResumePickerShortBudgetKeepsTheSelectedRow at budget 2.
func TestFrameGoldenResumePickerShort(t *testing.T) {
	rows := append(threeResumeRows(),
		resumeRow("s-4", "cursor", "chase the pty flake", 6*24*time.Hour),
		resumeRow("s-5", "grok", "write the release notes", 8*24*time.Hour),
		resumeRow("s-6", "gx", "read the acp schema", 9*24*time.Hour),
	)
	m, _ := newResumePicker(t, rows, 100, 12)
	body := m.resumeDialogBody(m.lay.Dialog.W-dialogBorder, m.lay.Dialog.H-dialogBorder)
	if len(body) >= 1+len(rows) {
		t.Fatalf("the box was not squeezed at all: %d rows for %d sessions", len(body), len(rows))
	}
	assertGolden(t, "resume-picker-short-100x12", 100, 12, plainView(m))
}

// TestResumePickerShortBudgetKeepsTheSelectedRow: the window follows the
// cursor, so the row Enter would load is on screen — and, because the click
// path shares the plan, clickable — at every budget.
func TestResumePickerShortBudgetKeepsTheSelectedRow(t *testing.T) {
	m, _ := newResumePicker(t, threeResumeRows(), 100, 30)
	m.resumeCursor = 2
	for budget := 2; budget <= 4; budget++ {
		top, shown, footer := m.resumeDialogPlan(budget)
		if shown == 0 {
			t.Fatalf("budget %d showed no rows", budget)
		}
		if m.resumeCursor < top || m.resumeCursor >= top+shown {
			t.Fatalf("budget %d: window [%d,%d) does not hold the cursor %d", budget, top, top+shown, m.resumeCursor)
		}
		// Two rows is the smallest box that still shows a row at all, and it
		// pays for it with the hint.
		if want := budget >= 3; footer != want {
			t.Fatalf("budget %d: footer %v, want %v", budget, footer, want)
		}
		body := m.resumeDialogBody(dialogMaxWidth-dialogBorder, budget)
		if got := markedPickerRow(t, body, shown); !strings.Contains(got, "port the docs site") {
			t.Fatalf("budget %d (footer %v): the focused row is %q", budget, footer, got)
		}
	}
}

// TestResumePickerWindowDoesNotScrollWhenEverythingFits: the offset answers a
// squeeze and is not a scroller, so a full-size box opens on the newest row.
func TestResumePickerWindowDoesNotScrollWhenEverythingFits(t *testing.T) {
	m, _ := newResumePicker(t, threeResumeRows(), 100, 30)
	top, shown, footer := m.resumeDialogPlan(m.lay.Dialog.H - dialogBorder)
	if top != 0 || shown != len(m.resume) || !footer {
		t.Fatalf("top %d shown %d footer %v — want the whole list from row 0", top, shown, footer)
	}
}
