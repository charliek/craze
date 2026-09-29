package tui

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/sessions"
)

// The session list (plan 030 §3.10, C10): its frames, in process, against a
// fake Sessions whose roster the test feeds by hand (sessSnapMsg) — the list
// is the client's own screen, so its goldens run in process alone — and its
// rules: entry only with Config.Sessions, the list routed ahead of every
// other key, the selection and an armed close held by identity, the header
// dropping its counts as it narrows, ctrl+x's two verbs, and the session
// behind the list ending without quitting craze.

// fakeSessions is Config.Sessions for a test: each Roster is a fakeRoster the
// test feeds, and Cancel and Stop record what they were asked.
type fakeSessions struct {
	mu        sync.Mutex
	rosters   []*fakeRoster
	cancels   []roster.Ref
	stops     []roster.Ref
	cancelErr error
	stopErr   error
}

var _ Sessions = (*fakeSessions)(nil)

func (f *fakeSessions) Roster() SessionRoster {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := &fakeRoster{ch: make(chan roster.Snapshot, 1)}
	f.rosters = append(f.rosters, r)
	return r
}

func (f *fakeSessions) Open(roster.Ref) (backend.Backend, error) {
	return nil, errors.New("fakeSessions: Open is C11's")
}

func (f *fakeSessions) Spawn(SpawnSpec) (roster.Ref, error) {
	return roster.Ref{}, errors.New("fakeSessions: Spawn is PR 3's")
}

func (f *fakeSessions) Stop(ref roster.Ref) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, ref)
	return f.stopErr
}

func (f *fakeSessions) Cancel(ref roster.Ref) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, ref)
	return f.cancelErr
}

func (f *fakeSessions) calls() (rosters, cancels, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rosters), len(f.cancels), len(f.stops)
}

// fakeRoster is a poller the test feeds: Updates is a slot of one, closed by
// Close.
type fakeRoster struct {
	ch     chan roster.Snapshot
	once   sync.Once
	closed atomic.Bool
}

func (r *fakeRoster) Updates() <-chan roster.Snapshot { return r.ch }

func (r *fakeRoster) Close() {
	r.once.Do(func() {
		r.closed.Store(true)
		close(r.ch)
	})
}

// sessNow is the list's clock in every test here: the ages it draws are
// against it.
var sessNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// sessModel is a started in-process session at cols×rows with a fake session
// list, its clock pinned at sessNow. The spinner is at frame 0 (no tick is
// ever delivered), so a working row's glyph is ✳.
func sessModel(t *testing.T, cols, rows int) (Model, *fakeSessions, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	fs := &fakeSessions{}
	stub := NewStub()
	m := New(Config{Session: stub, Theme: "tokyo-night", Workspace: frameWorkspace(t), Model: "grok", Yolo: true, Sessions: fs})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: cols, Height: rows})
	m = startedLikeInit(t, tm.(Model))
	m.clock = func() time.Time { return sessNow }
	return m, fs, stub
}

// openList presses ← on m's empty composer: the list opens.
func openList(t *testing.T, m Model) Model {
	t.Helper()
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyLeft})
	if !m.sessList.open {
		t.Fatalf("← on an empty composer did not open the list:\n%s", plainView(m))
	}
	return m
}

// listSnap hands the open list a snapshot, as its roster's read would.
func listSnap(t *testing.T, m Model, s roster.Snapshot) Model {
	t.Helper()
	tm, _ := m.Update(sessSnapMsg{gen: m.sessList.gen, snap: s})
	return tm.(Model)
}

// selectKey moves the selection to key with ↑/↓, or fails.
func selectKey(t *testing.T, m Model, key sessKey) Model {
	t.Helper()
	keys := sessKeys(m.sessLines())
	want := slices.Index(keys, key)
	if want < 0 {
		t.Fatalf("no line %+v among %+v", key, keys)
	}
	for range len(keys) + 1 {
		at := slices.Index(keys, m.sessList.sel)
		switch {
		case at == want:
			return m
		case at < want:
			m, _ = press(m, tea.KeyMsg{Type: tea.KeyDown})
		default:
			m, _ = press(m, tea.KeyMsg{Type: tea.KeyUp})
		}
	}
	t.Fatalf("the selection never reached %+v (at %+v)", key, m.sessList.sel)
	return m
}

func runKey(id string) sessKey { return sessKey{id: id, inc: "inc-" + id} }

// answered is a running row whose host answered with the row facts: idle,
// prompted, in state since ago before sessNow; set adjusts its Session.
func answered(id, title, prov, ws string, ago time.Duration, set func(*roster.Session)) roster.Row {
	return answeredInc(id, "inc-"+id, title, prov, ws, ago, set)
}

func answeredInc(id, inc, title, prov, ws string, ago time.Duration, set func(*roster.Session)) roster.Row {
	s := &roster.Session{
		ID: id, Incarnation: inc, Provider: prov, Workspace: ws, Title: title,
		Activity: engine.ActivityIdle, RowFacts: true, Stop: true, Prompted: true,
		Since: sessNow.Add(-ago),
	}
	if set != nil {
		set(s)
	}
	return roster.Row{
		Host: roster.Host{ID: "host-" + id, PID: 4242, Socket: "/run/craze/" + id + ".sock",
			StartedAt: sessNow.Add(-6 * time.Hour), CrazeSessionID: id, Incarnation: inc,
			Provider: prov, Workspace: ws, Ready: true},
		Status: roster.Reachable, Version: "0.1.0", Session: s,
	}
}

// older is a running row from a host without the row facts (craze version
// 0.0.9): S2's row alone.
func older(id, title, prov, ws string, set func(*roster.Session)) roster.Row {
	r := answered(id, title, prov, ws, 0, func(s *roster.Session) {
		s.RowFacts, s.Stop, s.Prompted, s.Since = false, false, false, time.Time{}
		if set != nil {
			set(s)
		}
	})
	r.Version = "0.0.9"
	return r
}

// richSnapshot is the owner's mockup, in craze's rows: two sessions asking,
// three working (one a host still starting), one failed, one idle, one from
// an older craze, one not answering; three saved. here is the session the
// list is opened from — writing the release notes, in the craze directory.
func richSnapshot(home string, here sessKey) roster.Snapshot {
	ws := func(name string) string { return filepath.Join(home, "projects", name) }
	starting := roster.Row{
		Host:   roster.Host{ID: "host-new", PID: 4343, Socket: "/run/craze/new.sock", StartedAt: sessNow.Add(-3 * time.Second), Provider: "grok", Workspace: ws("shed")},
		Status: roster.Connecting,
	}
	unreachable := answered("prox", "spike the prox sweep", "native", ws("prox"), 20*time.Minute, nil)
	unreachable.Status = roster.Unreachable
	return roster.Snapshot{
		Running: []roster.Row{
			answered("roost", "fix the roost tab rename", "grok", ws("roost"), 2*time.Minute, func(s *roster.Session) {
				s.Activity, s.PendingAsks = engine.ActivityWorking, 1
				s.HeadAsk = &roster.HeadAsk{ID: "ask-1", Kind: "permission", Label: "permission Shell", Summary: "cargo test -p roost-ipc"}
			}),
			answered("lumen", "add persistence to the reading list", "native", ws("lumen"), 14*time.Minute, func(s *roster.Session) {
				s.PendingAsks = 1
				s.HeadAsk = &roster.HeadAsk{ID: "ask-2", Kind: "question", Label: "question", Summary: "Which storage?"}
			}),
			answered("queue", "move queue edits onto remote.Session", "cursor", ws("craze"), 12*time.Minute, func(s *roster.Session) {
				s.Activity, s.Doing = engine.ActivityWorking, "go test ./internal/remote/..."
			}),
			answeredInc(here.id, here.inc, "write the v0.1.0 release notes", "native", ws("craze"), time.Minute, func(s *roster.Session) {
				s.Activity, s.Doing = engine.ActivityWorking, engine.DoingResponding
			}),
			starting,
			answered("deps", "bump the flutter deps", "native", ws("spendwise"), 3*time.Hour, func(s *roster.Session) {
				s.Activity = engine.ActivityError
				s.LastTurn = &engine.LastTurn{Outcome: engine.TurnFailed, Err: "402 Payment Required from openrouter", EndedAt: sessNow.Add(-3 * time.Hour)}
			}),
			answered("pty", "triage the flaky pty test", "grok", ws("craze"), time.Hour, func(s *roster.Session) {
				s.LastReply = "Found it: os.NewFile(0) races the pty close in the test helper."
			}),
			older("folio", "port the docs site", "gx", ws("folio"), nil),
			unreachable,
		},
		Saved: []sessions.Row{
			{SessionID: "p-wrap", Provider: "cursor", CWD: ws("craze"), Title: "teach the composer to wrap", CrazeID: "wrap", UpdatedAt: sessNow.Add(-2 * time.Hour)},
			{SessionID: "p-docs", Provider: "gx", CWD: ws("folio"), Title: "port the docs site to zensical", CrazeID: "docs", UpdatedAt: sessNow.Add(-5 * 24 * time.Hour)},
			{SessionID: "p-legacy", Provider: "native", CWD: ws("prox"), Title: "spike a stale-entry sweep for prox", UpdatedAt: sessNow.Add(-6 * 24 * time.Hour)},
		},
	}
}

// richList is m with the list open on richSnapshot.
func richList(t *testing.T, m Model) Model {
	t.Helper()
	m = openList(t, m)
	return listSnap(t, m, richSnapshot(os.Getenv("HOME"), m.hereKey()))
}

// TestFrameGoldenSessions: the list at 100×30 and 80×24 (plan 030 §3.17) —
// grouped by state and, ctrl+s, by directory; the saved group expanded (at
// 80×24 it scrolls, the selection kept in view); with nothing but the
// session it was opened from; a close armed; an unreachable row's ctrl+x;
// rows from an older craze.
func TestFrameGoldenSessions(t *testing.T) {
	for _, size := range []struct{ cols, rows int }{{100, 30}, {80, 24}} {
		suffix := "-100x30"
		if size.cols == 80 {
			suffix = "-80x24"
		}
		m, _, _ := sessModel(t, size.cols, size.rows)
		m = richList(t, m)
		if m.sessList.sel != m.hereKey() {
			t.Fatalf("the cursor starts on %+v, want the session the list came from %+v", m.sessList.sel, m.hereKey())
		}
		assertFrameGolden(t, "sessions"+suffix, size.cols, size.rows, plainView(m),
			[]string{" sessions  9 running", "needs you 2 ", "permission: cargo test -p r", "✳ ", "Starting…",
				"error: 402 Payment Required", "unreachable 1 ", "not answering", "craze 0.0.9", "craze · h", "▸ saved · 3 not running"},
			[]string{"agents"})

		dirs, _ := press(m, tea.KeyMsg{Type: tea.KeyCtrlS})
		assertFrameGolden(t, "sessions-dirs"+suffix, size.cols, size.rows, plainView(dirs),
			[]string{"~/projects/craze 3 ", "~/projects/roost 1 ", "ctrl+s by state"}, []string{"needs you", "· here"})

		saved := selectKey(t, m, sessSavedLine)
		saved, _ = press(saved, enter())
		if !saved.sessList.savedOpen {
			t.Fatal("enter on the saved line did not expand it")
		}
		saved, _ = press(saved, tea.KeyMsg{Type: tea.KeyDown})
		saved, _ = press(saved, tea.KeyMsg{Type: tea.KeyDown})
		if saved.sessList.sel != (sessKey{id: "\x00saved:docs"}) {
			t.Fatalf("↓↓ from the saved line selected %+v", saved.sessList.sel)
		}
		assertFrameGolden(t, "sessions-saved"+suffix, size.cols, size.rows, plainView(saved),
			[]string{"▾ saved · 3 not running", "· port the docs site t", "enter resume"}, nil)
	}

	m, _, _ := sessModel(t, 80, 24)
	m = openList(t, m)
	here := m.hereKey()
	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{
		answeredInc(here.id, here.inc, "write the v0.1.0 release notes", "native", filepath.Join(os.Getenv("HOME"), "projects", "craze"), 4*time.Minute, func(s *roster.Session) {
			s.LastReply = "The notes are in CHANGELOG.md."
		}),
	}})
	assertFrameGolden(t, "sessions-empty-80x24", 80, 24, plainView(m),
		[]string{sessEmptyNote, "The notes are in CHANGELOG", "enter back to it"}, []string{"saved"})

	m, _, _ = sessModel(t, 80, 24)
	m = richList(t, m)
	m = selectKey(t, m, runKey("pty"))
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	assertFrameGolden(t, "sessions-armed-80x24", 80, 24, plainView(m),
		[]string{"ctrl+x again closes it; the transcript stays resumable"}, nil)

	m, _, _ = sessModel(t, 80, 24)
	m = richList(t, m)
	m = selectKey(t, m, runKey("prox"))
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	assertFrameGolden(t, "sessions-unreachable-80x24", 80, 24, plainView(m),
		[]string{"? spike the prox sweep", sessUnreachNote}, nil)

	m, _, _ = sessModel(t, 100, 30)
	m = openList(t, m)
	here = m.hereKey()
	ws := func(name string) string { return filepath.Join(os.Getenv("HOME"), "projects", name) }
	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{
		answeredInc(here.id, here.inc, "write the v0.1.0 release notes", "native", ws("craze"), 4*time.Minute, nil),
		older("folio", "port the docs site", "gx", ws("folio"), nil),
		older("index", "migrate the index", "cursor", ws("lumen"), func(s *roster.Session) {
			s.PendingAsks = 1
			s.HeadAsk = &roster.HeadAsk{ID: "ask-9", Kind: "permission", Label: "permission Shell"}
		}),
		older("notes", "tidy the notes", "grok", ws("notes"), func(s *roster.Session) { s.Activity = engine.ActivityWorking }),
		older("bump", "bump the deps", "native", ws("spendwise"), func(s *roster.Session) { s.Activity = engine.ActivityError }),
	}})
	assertFrameGolden(t, "sessions-older-host-100x30", 100, 30, plainView(m),
		[]string{"permission: Shell · craze 0.0.9", "Working · craze 0.0.9", "error · craze 0.0.9", "waiting for a prompt · craze 0.0.9"}, nil)
}

// TestSessionsListIsAbsentWithoutASessionList (§3.9's last paragraph):
// Config.Sessions nil leaves ← on an empty composer to the textarea, the
// slash catalog and /help as they were — no /sessions, no ← line.
func TestSessionsListIsAbsentWithoutASessionList(t *testing.T) {
	m := sized(t)
	before := plainView(m)
	after, _ := press(m, tea.KeyMsg{Type: tea.KeyLeft})
	if after.sessList.open || plainView(after) != before {
		t.Fatalf("← without a session list changed the screen:\n%s", plainView(after))
	}
	for _, it := range m.slashCatalog() {
		if it.Name == "sessions" {
			t.Fatal("/sessions is in the catalog without a session list")
		}
	}
	for _, l := range m.helpKeyLines() {
		if l.key == "←" {
			t.Fatalf("/help has a ← line without a session list: %+v", l)
		}
	}
	m = typeInto(t, m, "/sessions")
	if m.sessList.open {
		t.Fatal("typing /sessions opened a list")
	}

	with, _, _ := sessModel(t, 80, 24)
	if got, want := len(with.helpKeyLines()), len(m.helpKeyLines())+1; got != want {
		t.Fatalf("/help with a session list has %d key lines, want %d (one ← line more)", got, want)
	}
	names := []string{}
	for _, it := range with.slashCatalog() {
		if it.Builtin {
			names = append(names, it.Name)
		}
	}
	if i := slices.Index(names, "sessions"); i < 0 || names[i+1] != "exit" {
		t.Fatalf("the builtins with a session list: %v, want /sessions just before /exit", names)
	}
}

// TestSessionsListOpensOnLeftAndSlashSessions: ← opens the list on an empty
// composer only — not with a draft — and /sessions opens it and takes its
// draft; each opening starts a roster, and leaving closes it; alt+← and
// alt+→ are not list keys. (alt+← on an empty composer is not pressed here:
// bubbles' textarea word-left never returns when only whitespace precedes the
// cursor — a hang of its own, the same with or without a session list.)
func TestSessionsListOpensOnLeftAndSlashSessions(t *testing.T) {
	draft, _, _ := sessModel(t, 80, 24)
	draft = typeInto(t, draft, "hi")
	if d, _ := press(draft, tea.KeyMsg{Type: tea.KeyLeft}); d.sessList.open || d.input.Value() != "hi" {
		t.Fatalf("← with a draft: open %v, draft %q", d.sessList.open, d.input.Value())
	}
	m, fs, _ := sessModel(t, 80, 24)
	m = openList(t, m)
	if n, _, _ := fs.calls(); n != 1 {
		t.Fatalf("%d rosters started, want 1", n)
	}
	m = listSnap(t, m, richSnapshot(os.Getenv("HOME"), m.hereKey()))
	for _, k := range []tea.KeyMsg{{Type: tea.KeyLeft, Alt: true}, {Type: tea.KeyRight, Alt: true}} {
		if alt, _ := press(m, k); !alt.sessList.open || alt.sessList.note != "" {
			t.Fatalf("%v on the list: open %v, note %q", k, alt.sessList.open, alt.sessList.note)
		}
	}
	r := fs.rosters[0]
	back, cmd := press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if back.sessList.open {
		t.Fatal("esc did not leave the list")
	}
	runCmd(cmd)
	if !r.closed.Load() {
		t.Fatal("leaving the list did not close its roster")
	}

	m = typeInto(t, back, "/sessions")
	m, _ = press(m, enter())
	if !m.sessList.open || m.input.Value() != "" {
		t.Fatalf("/sessions: open %v, draft %q", m.sessList.open, m.input.Value())
	}
	if n, _, _ := fs.calls(); n != 2 || m.sessList.gen != back.sessList.gen+1 {
		t.Fatalf("%d rosters, opening %d after %d", n, m.sessList.gen, back.sessList.gen)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyLeft})
	if m.sessList.open {
		t.Fatal("← did not leave the list")
	}
}

// TestSessionsListKeysComeFirst (§3.10): while the list is open it takes
// every key ahead of Ctrl+C, Ctrl+D, a card, a dialog and the mouse. A card
// the session behind it raises meanwhile is drawn once the list is left,
// and took no key; a dialog behind it keeps its place; the first Ctrl+C
// cancels nothing and quits nothing, the second quits craze without asking
// the session to stop, and closes the roster; Ctrl+D quits the same way.
func TestSessionsListKeysComeFirst(t *testing.T) {
	m, fs, stub := sessModel(t, 80, 24)
	m = richList(t, m)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	if !m.cardOpen() || !m.sessList.open {
		t.Fatal("fixture: the card and the list")
	}
	if strings.Contains(plainView(m), "Pick one") {
		t.Fatalf("the card is drawn over the list:\n%s", plainView(m))
	}
	was := m.sessList.sel
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.sessList.sel == was || !m.cardOpen() {
		t.Fatalf("↓ with a card behind the list: selection %+v (was %+v), card open %v", m.sessList.sel, was, m.cardOpen())
	}
	m = m.openHelp()
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	if !m.cardOpen() {
		t.Fatal("a key reached the card behind the list")
	}
	clicked, _ := m.Update(tea.MouseMsg{X: 3, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if c := clicked.(Model); c.sessList.sel != m.sessList.sel || !c.sessList.open {
		t.Fatal("the mouse reached the list")
	}

	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if m.quitting || !m.sessList.open || m.ctrlCDeadline.IsZero() || cmd == nil {
		t.Fatalf("the first ctrl+c: quitting %v, open %v, window %v", m.quitting, m.sessList.open, m.ctrlCDeadline)
	}
	if !strings.Contains(plainView(m), "ctrl+c again quits craze; every session keeps running") {
		t.Fatalf("the first ctrl+c's hint:\n%s", plainView(m))
	}
	if _, cancels, stops := fs.calls(); cancels != 0 || stops != 0 {
		t.Fatalf("the first ctrl+c cancelled %d and stopped %d", cancels, stops)
	}
	m, cmd = press(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if !m.quitting {
		t.Fatal("the second ctrl+c did not quit")
	}
	if msg := runCmd(cmd); msg != (tea.QuitMsg{}) {
		t.Fatalf("the quit's command answered %#v", msg)
	}
	if !fs.rosters[0].closed.Load() {
		t.Fatal("quitting from the list left its roster running")
	}
	if stopping, _, _ := m.exit.outcome(); stopping {
		t.Fatal("quitting from the list asked the session to stop")
	}
	if !m.cardOpen() || m.dialog != dialogHelp {
		t.Fatal("the card or the dialog behind the list moved")
	}

	d, fs2, _ := sessModel(t, 80, 24)
	d = richList(t, d)
	d, cmd = press(d, tea.KeyMsg{Type: tea.KeyCtrlD})
	if !d.quitting || runCmd(cmd) != (tea.QuitMsg{}) || !fs2.rosters[0].closed.Load() {
		t.Fatal("ctrl+d from the list did not quit and close the roster")
	}
}

// TestSessionsListLeavesToTheCard: once the list is left, the card the
// session raised behind it is on screen and owns the keyboard.
func TestSessionsListLeavesToTheCard(t *testing.T) {
	m, _, stub := sessModel(t, 80, 24)
	m = richList(t, m)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.sessList.open || !strings.Contains(plainView(m), "Pick one") {
		t.Fatalf("after the list, the card:\n%s", plainView(m))
	}
}

// TestSessionsSelectionHeldByIdentity (§3.10): the selection is a row's
// identity, not its place — a reorder keeps it on its row; a replaced
// incarnation keeps it on that session; a row that disappears leaves it to
// its neighbour; toggling the grouping keeps it.
func TestSessionsSelectionHeldByIdentity(t *testing.T) {
	m, _, _ := sessModel(t, 100, 30)
	ws := filepath.Join(os.Getenv("HOME"), "projects", "craze")
	a := answered("a", "session a", "grok", ws, 1*time.Minute, nil)
	b := answered("b", "session b", "grok", ws, 2*time.Minute, nil)
	c := answered("c", "session c", "grok", ws, 3*time.Minute, nil)
	m = openList(t, m)
	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{a, b, c}})
	if got := sessKeys(m.sessLines()); !slices.Equal(got, []sessKey{runKey("a"), runKey("b"), runKey("c")}) {
		t.Fatalf("idle rows newest first: %+v", got)
	}
	m = selectKey(t, m, runKey("b"))

	b2 := answered("b", "session b", "grok", ws, 0, nil)
	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{a, b2, c}})
	if got := sessKeys(m.sessLines()); got[0] != runKey("b") || m.sessList.sel != runKey("b") {
		t.Fatalf("after b moved to the top (%+v) the selection is %+v", got, m.sessList.sel)
	}
	doing := answered("b", "session b", "grok", ws, 0, func(s *roster.Session) { s.LastReply = "said something else" })
	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{a, doing, c}})
	if got := sessKeys(m.sessLines()); got[0] != runKey("b") {
		t.Fatalf("a row that changed only what it says moved: %+v", got)
	}

	m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlS})
	if m.sessList.sel != runKey("b") {
		t.Fatalf("regrouping moved the selection to %+v", m.sessList.sel)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlS})

	replaced := answeredInc("b", "inc-b2", "session b", "grok", ws, 0, nil)
	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{a, replaced, c}})
	if m.sessList.sel != (sessKey{id: "b", inc: "inc-b2"}) {
		t.Fatalf("a replaced incarnation left the selection at %+v", m.sessList.sel)
	}

	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{a, c}})
	if got := sessKeys(m.sessLines()); m.sessList.sel != got[0] {
		t.Fatalf("b gone: the selection is %+v, want its neighbour %+v", m.sessList.sel, got[0])
	}
	m = selectKey(t, m, runKey("c"))
	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{a}})
	if m.sessList.sel != runKey("a") {
		t.Fatalf("the last row gone: the selection is %+v, want the one above", m.sessList.sel)
	}
}

// TestSessionsArmedCloseHeldByIdentity (§3.10): ctrl+x on an idle row arms
// a close, a second within two seconds sends it — to that row, whatever
// moved meanwhile. Any other key, the window running out, a replaced
// incarnation, and the row leaving idle each disarm it.
func TestSessionsArmedCloseHeldByIdentity(t *testing.T) {
	m, fs, _ := sessModel(t, 100, 30)
	ws := filepath.Join(os.Getenv("HOME"), "projects", "craze")
	a := answered("a", "session a", "grok", ws, 1*time.Minute, nil)
	b := answered("b", "session b", "grok", ws, 2*time.Minute, nil)
	m = openList(t, m)
	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{a, b}})
	m = selectKey(t, m, runKey("b"))
	ctrlX := tea.KeyMsg{Type: tea.KeyCtrlX}

	m, _ = press(m, ctrlX)
	if m.sessList.armed != runKey("b") {
		t.Fatalf("ctrl+x on an idle row armed %+v", m.sessList.armed)
	}
	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{a, answered("b", "session b", "grok", ws, 0, nil)}})
	if m.sessList.armed != runKey("b") {
		t.Fatal("a reorder disarmed the close")
	}
	m, cmd := press(m, ctrlX)
	msg := runCmd(cmd)
	if _, _, stops := fs.calls(); stops != 1 || fs.stops[0].Host.ID != "host-b" {
		t.Fatalf("the second ctrl+x stopped %+v", fs.stops)
	}
	m = applyMsg(t, m, msg)
	if !strings.Contains(plainView(m), "closed: session b") {
		t.Fatalf("the close's note:\n%s", plainView(m))
	}

	m, _ = press(m, ctrlX)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyDown})
	if !m.sessList.armed.zero() {
		t.Fatal("another key left the close armed")
	}
	m = selectKey(t, m, runKey("b"))

	m, _ = press(m, ctrlX)
	start := sessNow
	m.clock = func() time.Time { return start.Add(sessCloseWindow) }
	m, _ = press(m, ctrlX)
	if _, _, stops := fs.calls(); stops != 1 || m.sessList.armed != runKey("b") {
		t.Fatalf("a ctrl+x after the window: %d stops, armed %+v; want it armed afresh", stops, m.sessList.armed)
	}
	m.clock = func() time.Time { return sessNow }

	stale := applyMsg(t, m, sessDisarmMsg{gen: m.sessList.gen, seq: m.sessList.armSeq - 1})
	if stale.sessList.armed.zero() {
		t.Fatal("an earlier arming's expiry disarmed this one")
	}
	expired := applyMsg(t, m, sessDisarmMsg{gen: m.sessList.gen, seq: m.sessList.armSeq})
	if !expired.sessList.armed.zero() {
		t.Fatal("the window's expiry left the close armed")
	}

	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{a, answeredInc("b", "inc-b2", "session b", "grok", ws, 0, nil)}})
	if !m.sessList.armed.zero() {
		t.Fatal("a replaced incarnation left the close armed")
	}
	m, _ = press(m, ctrlX)
	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{a, answeredInc("b", "inc-b2", "session b", "grok", ws, 0, func(s *roster.Session) {
		s.Activity = engine.ActivityWorking
	})}})
	if !m.sessList.armed.zero() {
		t.Fatal("a row that went to work kept its armed close")
	}
	if _, _, stops := fs.calls(); stops != 1 {
		t.Fatalf("%d stops, want the one", stops)
	}
}

// applyMsg is m.Update(msg) as a Model.
func applyMsg(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	tm, _ := m.Update(msg)
	return tm.(Model)
}

// TestSessionsCtrlXStopsAWorkingRow (§3.10): ctrl+x on a working or asking
// row is Sessions.Cancel — its turn stopped, its queue cleared — at once,
// with no arming; its answer is the hint line's note. A host that cannot
// stop its session says so; a saved row and a host not answering yet take
// nothing.
func TestSessionsCtrlXStopsAWorkingRow(t *testing.T) {
	m, fs, _ := sessModel(t, 100, 30)
	m = richList(t, m)
	for _, id := range []string{"queue", "lumen"} {
		m = selectKey(t, m, runKey(id))
		var cmd tea.Cmd
		m, cmd = press(m, tea.KeyMsg{Type: tea.KeyCtrlX})
		if !m.sessList.armed.zero() {
			t.Fatalf("ctrl+x on %s armed a close", id)
		}
		m = applyMsg(t, m, runCmd(cmd))
	}
	if _, cancels, stops := fs.calls(); cancels != 2 || stops != 0 || fs.cancels[0].Host.ID != "host-queue" || fs.cancels[1].Host.ID != "host-lumen" {
		t.Fatalf("cancels %+v, %d stops", fs.cancels, stops)
	}
	if !strings.Contains(plainView(m), "stopped: add persistence to the reading list") {
		t.Fatalf("the cancel's note:\n%s", plainView(m))
	}

	fs.cancelErr = errors.New("the socket went away")
	m = selectKey(t, m, runKey("queue"))
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	m = applyMsg(t, m, runCmd(cmd))
	if !strings.Contains(plainView(m), "could not stop move queue edits onto remote.Session: the socket went away") {
		t.Fatalf("a failed cancel's note:\n%s", plainView(m))
	}

	fs.stopErr = backend.ErrStopUnsupported
	m = selectKey(t, m, sessKey{id: "folio", inc: "inc-folio"})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	m, cmd = press(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	m = applyMsg(t, m, runCmd(cmd))
	if !strings.Contains(plainView(m), sessOlderNote) {
		t.Fatalf("an older host's close:\n%s", plainView(m))
	}

	_, before, stops := fs.calls()
	for _, key := range []sessKey{{id: "\x00host:host-new"}, sessSavedLine} {
		m = selectKey(t, m, key)
		var cmd tea.Cmd
		m, cmd = press(m, tea.KeyMsg{Type: tea.KeyCtrlX})
		if cmd != nil && runCmd(cmd) != nil {
			t.Fatalf("ctrl+x on %+v made a call", key)
		}
		if !m.sessList.armed.zero() {
			t.Fatalf("ctrl+x on %+v armed a close", key)
		}
	}
	if _, cancels, stops2 := fs.calls(); cancels != before || stops2 != stops {
		t.Fatal("ctrl+x on a starting host or the saved line reached Sessions")
	}
}

// TestSessionsHeaderDropsCountsByWidth (§3.10): the header's counts are
// dropped from the right as the width shrinks — every width shows a prefix
// of them in order, never fewer at a wider width — and the row never
// overflows.
func TestSessionsHeaderDropsCountsByWidth(t *testing.T) {
	m, _, _ := sessModel(t, 100, 30)
	m = richList(t, m)
	all := []string{"! 2 need you", "✳ 3 working", "✗ 1 failed", "○ 2 idle"}
	prev := len(all) + 1
	for w := 100; w >= 20; w-- {
		m.width = w
		row := plain(m.sessHeaderRow(m.sessLines()))
		if lipgloss.Width(row) > w {
			t.Fatalf("at %d the header is %d wide: %q", w, lipgloss.Width(row), row)
		}
		if !strings.HasPrefix(row, " sessions  9 running") {
			t.Fatalf("at %d the header lost its left side: %q", w, row)
		}
		n := 0
		for n < len(all) && strings.Contains(row, all[n]) {
			n++
		}
		for _, c := range all[n:] {
			if strings.Contains(row, c) {
				t.Fatalf("at %d the header dropped a count from the middle: %q", w, row)
			}
		}
		if n > prev {
			t.Fatalf("at %d the header shows %d counts, more than at %d", w, n, w+1)
		}
		switch w {
		case 100:
			if n != len(all) {
				t.Fatalf("at 100 the header shows %d counts: %q", n, row)
			}
		case 40:
			if n == len(all) {
				t.Fatalf("at 40 the header kept every count: %q", row)
			}
		case 20:
			if n != 0 {
				t.Fatalf("at 20 the header kept %d counts: %q", n, row)
			}
		}
		prev = n
	}
}

// TestSessionsListWhenTheSessionBehindItEnds (§3.10): the session the list
// was opened from ending does not quit craze; its row is marked ended, and
// esc, ← and enter on it stay on the list and say so.
func TestSessionsListWhenTheSessionBehindItEnds(t *testing.T) {
	m, _, _ := sessModel(t, 100, 30)
	m = richList(t, m)
	tm, cmd := m.Update(endMsg{})
	m = tm.(Model)
	if m.quitting || !m.ended || !m.sessList.hereEnded {
		t.Fatalf("the end behind the list: quitting %v, ended %v, marked %v", m.quitting, m.ended, m.sessList.hereEnded)
	}
	if cmd != nil {
		if msg := runCmd(cmd); msg == (tea.QuitMsg{}) {
			t.Fatal("the end behind the list quit craze")
		}
	}
	r, ok := sessFind(m.sessLines(), m.hereKey())
	if !ok || !r.ended || r.want != "ended" {
		t.Fatalf("the session's row: %+v", r)
	}
	for _, k := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyLeft}, {Type: tea.KeyEnter}} {
		m = selectKey(t, m, m.hereKey())
		m, _ = press(m, k)
		if !m.sessList.open || !strings.Contains(plainView(m), sessEndedNote) {
			t.Fatalf("%v after the end:\n%s", k, plainView(m))
		}
	}
}

// TestSessionsEnterOpensTheSessionBehind: enter on the session the list came
// from goes back to it; on another the note says C11's switch is to come.
func TestSessionsEnterOpensTheSessionBehind(t *testing.T) {
	m, _, _ := sessModel(t, 100, 30)
	m = richList(t, m)
	other := selectKey(t, m, runKey("pty"))
	other, _ = press(other, tea.KeyMsg{Type: tea.KeyRight})
	if !other.sessList.open || !strings.Contains(plainView(other), sessOpenLater) {
		t.Fatalf("→ on another session:\n%s", plainView(other))
	}
	back, _ := press(m, enter())
	if back.sessList.open {
		t.Fatal("enter on the session behind the list did not go back to it")
	}
}

// TestSessionsListTooSmall (§3.10): below 40×10 the list is the too-small
// message; at 40×10 it draws, exactly its size.
func TestSessionsListTooSmall(t *testing.T) {
	for _, size := range []struct{ cols, rows int }{{39, 10}, {40, 9}} {
		m, _, _ := sessModel(t, 80, 24)
		m = richList(t, m)
		m = applyMsg(t, m, tea.WindowSizeMsg{Width: size.cols, Height: size.rows})
		if !strings.Contains(plainView(m), "(need 40×10)") {
			t.Fatalf("%dx%d:\n%s", size.cols, size.rows, plainView(m))
		}
	}
	m, _, _ := sessModel(t, 80, 24)
	m = richList(t, m)
	m = applyMsg(t, m, tea.WindowSizeMsg{Width: 40, Height: 10})
	view := plainView(m)
	if !strings.HasPrefix(view, " sessions  9 running") || lipgloss.Height(view) != 10 {
		t.Fatalf("40x10:\n%s", view)
	}
	for _, ln := range strings.Split(view, "\n") {
		if lipgloss.Width(ln) != 40 {
			t.Fatalf("a 40x10 row is %d wide: %q", lipgloss.Width(ln), ln)
		}
	}
}

// TestSessionsListReadsItsRoster: the opening's command reads the roster's
// slot; a snapshot of an earlier opening, and one for a closed list, are
// dropped.
func TestSessionsListReadsItsRoster(t *testing.T) {
	m, fs, _ := sessModel(t, 100, 30)
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyLeft})
	fs.rosters[0].ch <- roster.Snapshot{Running: []roster.Row{answered("a", "session a", "grok", "/w", time.Minute, nil)}}
	msg, ok := runCmd(cmd).(sessSnapMsg)
	if !ok || msg.gen != m.sessList.gen || len(msg.snap.Running) != 1 {
		t.Fatalf("the read answered %#v", msg)
	}
	stale := applyMsg(t, m, sessSnapMsg{gen: m.sessList.gen - 1, snap: msg.snap})
	if stale.sessList.have {
		t.Fatal("an earlier opening's snapshot was applied")
	}
	m = applyMsg(t, m, msg)
	if !m.sessList.have || len(m.sessRunningRows()) != 1 {
		t.Fatal("the opening's own snapshot was not applied")
	}
	closed, _ := press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if after := applyMsg(t, closed, msg); after.sessList.have {
		t.Fatal("a snapshot for a closed list was applied")
	}
}

// TestSessionsRowMapping pins §3.10's table on the rows: glyph, group and
// what each wants, from rowFacts hosts and an older one.
func TestSessionsRowMapping(t *testing.T) {
	for _, c := range []struct {
		name  string
		row   roster.Row
		state sessState
		want  string
	}{
		{"permission", answered("a", "t", "grok", "/w", 0, func(s *roster.Session) {
			s.PendingAsks, s.HeadAsk = 1, &roster.HeadAsk{Kind: "permission", Label: "permission Shell", Summary: "rm -rf build"}
		}), sessNeedsYou, "permission: rm -rf build"},
		{"plan", answered("a", "t", "grok", "/w", 0, func(s *roster.Session) {
			s.PendingAsks, s.HeadAsk = 1, &roster.HeadAsk{Kind: "plan", Label: "plan ship it", Summary: "ship it"}
		}), sessNeedsYou, "plan: ship it"},
		{"start failed", answered("a", "t", "grok", "/w", 0, func(s *roster.Session) {
			s.Activity, s.StartFailed, s.StartErr = engine.ActivityError, true, "auth failed"
		}), sessFailed, "error: auth failed"},
		{"starting", answered("a", "t", "grok", "/w", 0, func(s *roster.Session) { s.Activity = engine.ActivityStarting }), sessWorking, "Starting…"},
		{"replaying", answered("a", "t", "grok", "/w", 0, func(s *roster.Session) { s.Activity = engine.ActivityReplaying }), sessWorking, "Loading…"},
		{"closing", answered("a", "t", "grok", "/w", 0, func(s *roster.Session) { s.Activity = engine.ActivityClosing }), sessWorking, "Closing…"},
		{"foreign turn", answered("a", "t", "grok", "/w", 0, func(s *roster.Session) {
			s.ForeignTurn, s.Doing = true, engine.DoingThinking
		}), sessWorking, "Thinking"},
		{"idle, never prompted", answered("a", "t", "grok", "/w", 0, nil), sessIdle, "waiting for a prompt"},
		{"a turn failed, then one done", answered("a", "t", "grok", "/w", 0, func(s *roster.Session) {
			s.LastTurn, s.LastReply = &engine.LastTurn{Outcome: engine.TurnDone}, "ok"
		}), sessIdle, "ok"},
	} {
		got := sessRunningRow(c.row, "", false)
		if got.state != c.state || got.want != c.want {
			t.Errorf("%s: state %d want %q; expected %d %q", c.name, got.state, got.want, c.state, c.want)
		}
	}
	conn := sessRunningRow(roster.Row{Host: roster.Host{ID: "h", CrazeSessionID: "x", Ready: true}, Status: roster.Connecting, IndexTitle: "from the index"}, "", false)
	if conn.state != sessWorking || !conn.connecting || conn.want != "Connecting…" || conn.title != "from the index" || conn.key != (sessKey{id: "x"}) {
		t.Errorf("a ready host not answered yet: %+v", conn)
	}
	untitled := sessRunningRow(roster.Row{Host: roster.Host{ID: "h"}, Status: roster.Connecting}, "", false)
	if untitled.want != "Starting…" || untitled.title != sessUntitled || untitled.key != (sessKey{id: "\x00host:h"}) {
		t.Errorf("a host with no session yet: %+v", untitled)
	}
}
