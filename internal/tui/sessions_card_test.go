package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/roster"
)

// `←` through a card (plan 032 §3.2 C3, SF-99): with a card up, an unmodified
// ← leaves the session for the session list — the one key a card lets
// through (Ctrl+C and Ctrl+D are taken ahead of it), and only where there is
// a list. The ask stays open (nothing answers it), so the list has the
// session under "needs you"; the card, its progress, the draft under it and
// the sub-agent view it is over are all there on the way back. Every other
// key path keeps its ←: with no card up a draft, a queue edit, a dialog and
// the sub-agent view each have it first, and Alt+← stays the card's.

// hereFromEngine is the session behind the list as its host's sessions.list
// would row it now (internal/control's sessionsList): the engine's own open
// asks and activity (State) and the head ask's summary (RowFacts), with the
// title and directory the test names, in its state since a minute before
// sessNow.
func hereFromEngine(t *testing.T, m Model, title, ws string) roster.Row {
	t.Helper()
	eng := engineOf(t, m)
	st := eng.State()
	facts := eng.RowFacts(st)
	here := m.hereKey()
	if here.zero() {
		t.Fatal("fixture: the session has no identity to row")
	}
	return answeredInc(here.id, here.inc, title, "native", ws, time.Minute, func(s *roster.Session) {
		s.Activity, s.PendingAsks = st.Activity, st.PendingAsks
		if st.PendingAsks > 0 {
			s.HeadAsk = &roster.HeadAsk{ID: st.HeadAsk.ID, Kind: string(st.HeadAsk.Kind), Label: st.HeadAsk.Label, Summary: facts.Summary}
		}
	})
}

// left is an unmodified ←.
func left() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyLeft} }

// firstMissing is the first of subs that s does not contain, "" when it
// contains them all.
func firstMissing(s string, subs ...string) string {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return sub
		}
	}
	return ""
}

// TestSessionsLeftThroughACard: with a question card up, part answered, over
// a draft, ← opens the list and answers nothing — the card is kept as it
// was, the ask is still open, and the list has the session under "needs you"
// with the cursor on its row (not on a newer ask above it) and the card
// nowhere on screen. Esc returns to the same card at the same question with
// the same picks, which still owns the keyboard and answers with them; the
// draft and its cursor are kept under it throughout.
func TestSessionsLeftThroughACard(t *testing.T) {
	m, fs, stub := sessModel(t, 100, 30)
	m = typeInto(t, m, "half a thought")
	m, _ = press(m, left())
	if m.sessList.open {
		t.Fatal("fixture: ← in a draft opened the list")
	}
	draft, col := m.input.Value(), m.input.LineInfo().ColumnOffset
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	m, _ = press(m, runeKey('1'))
	m, _ = press(m, runeKey('3'))
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyUp})
	progress := []string{"question 2/2  Pick any", "[x] Z", "> 2 [ ] Y"}
	if miss := firstMissing(plainView(m), progress...); miss != "" {
		t.Fatalf("fixture: the card's progress %q:\n%s", miss, plainView(m))
	}
	card := m.cards

	m, _ = press(m, left())
	if !m.sessList.open {
		t.Fatalf("← with a card up did not open the list:\n%s", plainView(m))
	}
	if n, _, _ := fs.calls(); n != 1 || len(stub.Calls()) != 0 || !reflect.DeepEqual(m.cards, card) {
		t.Fatalf("← over the card: %d rosters (want 1), answers %+v, card kept %v", n, stub.Calls(), reflect.DeepEqual(m.cards, card))
	}
	ws := filepath.Join(os.Getenv("HOME"), "projects", "craze")
	newer := answered("roost", "fix the roost tab rename", "grok", ws, 0, func(s *roster.Session) {
		s.PendingAsks = 1
		s.HeadAsk = &roster.HeadAsk{ID: "ask-9", Kind: "permission", Label: "permission Shell", Summary: "cargo test"}
	})
	m = listSnap(t, m, roster.Snapshot{Running: []roster.Row{newer, hereFromEngine(t, m, "write the v0.1.0 release notes", ws)}})
	row, ok := sessFind(m.sessLines(), m.hereKey())
	if !ok || !row.here || row.state != sessNeedsYou || row.want != "question: Pick one" || m.sessList.sel != row.key {
		t.Fatalf("the session behind the list: %+v (found %v), the cursor on %+v", row, ok, m.sessList.sel)
	}
	if v := plainView(m); !strings.Contains(v, "needs you 2 ") || strings.Contains(v, "Pick any") {
		t.Fatalf("the list over the card:\n%s", v)
	}

	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.sessList.open {
		t.Fatal("esc did not leave the list")
	}
	if miss := firstMissing(plainView(m), progress...); miss != "" || !reflect.DeepEqual(m.cards, card) {
		t.Fatalf("back from the list, the card is %+v (was %+v), missing %q:\n%s", m.cards, card, miss, plainView(m))
	}
	if m.input.Value() != draft || m.input.LineInfo().ColumnOffset != col {
		t.Fatalf("back from the list, the draft is %q at column %d, was %q at %d", m.input.Value(), m.input.LineInfo().ColumnOffset, draft, col)
	}

	m, _ = press(m, enter())
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].ID != "ask-1" || calls[0].Skip || m.cardOpen() || m.input.Value() != draft ||
		strings.Join(calls[0].Answers["q1"], ",") != "opt-a" || strings.Join(calls[0].Answers["q2"], ",") != "opt-z" {
		t.Fatalf("the card answered %+v (want q1 opt-a and q2 opt-z); card open %v, draft %q", calls, m.cardOpen(), m.input.Value())
	}
}

// TestSessionsOnlyBareLeftPassesACard: on each kind of card, Alt+← is the
// card's as it always was (swallowed: nothing changes, nothing is answered)
// and an unmodified ← opens the list with the card kept and unanswered.
func TestSessionsOnlyBareLeftPassesACard(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   agent.Event
	}{
		{"question", agent.Event{Type: agent.EventQuestion, Question: stubQuestion()}},
		{"plan", agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()}},
		{"permission", agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(true)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, stub := sessModel(t, 80, 24)
			m = cardEvent(t, m, stub, tc.ev)
			if !m.cardOpen() {
				t.Fatal("fixture: no card")
			}
			card, view := m.cards, plainView(m)
			alt, _ := press(m, tea.KeyMsg{Type: tea.KeyLeft, Alt: true})
			if alt.sessList.open {
				t.Fatal("alt+← with a card up opened the list")
			}
			if !reflect.DeepEqual(alt.cards, card) || plainView(alt) != view || len(stub.Calls()) != 0 {
				t.Fatalf("alt+← reached past the card: calls %+v\n%s", stub.Calls(), plainView(alt))
			}
			m, _ = press(m, left())
			if !m.sessList.open {
				t.Fatalf("← with a %s card up did not open the list", tc.name)
			}
			if !reflect.DeepEqual(m.cards, card) || len(stub.Calls()) != 0 {
				t.Fatalf("← answered or moved the %s card: calls %+v", tc.name, stub.Calls())
			}
		})
	}
}

// TestSessionsLeftWithoutACardKeepsItsPlace: with no card up, everything
// that had ← before the list still has it — a draft (the cursor moves), a
// queue edit (its text, or its emptied composer, is not an empty draft), and
// a dialog (/help keeps it, and stays up).
func TestSessionsLeftWithoutACardKeepsItsPlace(t *testing.T) {
	t.Run("draft", func(t *testing.T) {
		m, _, _ := sessModel(t, 80, 24)
		m = typeInto(t, m, "hi")
		col := m.input.LineInfo().ColumnOffset
		m, _ = press(m, left())
		if m.sessList.open || m.input.Value() != "hi" || m.input.LineInfo().ColumnOffset != col-1 {
			t.Fatalf("← in a draft: list open %v, draft %q, column %d (was %d)", m.sessList.open, m.input.Value(), m.input.LineInfo().ColumnOffset, col)
		}
	})
	t.Run("queue edit", func(t *testing.T) {
		m, _, stub := sessModel(t, 80, 24)
		stub.HangNext()
		m = typeEnter(t, m, "go")
		if m.status != statusWorking {
			t.Fatal("fixture: the turn is not working")
		}
		m = typeEnter(t, m, "one")
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyUp})
		m, _ = press(m, enter())
		if m.queueEdit == "" || m.input.Value() != "one" {
			t.Fatalf("fixture: not editing the queued row (edit %q, composer %q)", m.queueEdit, m.input.Value())
		}
		col := m.input.LineInfo().ColumnOffset
		m, _ = press(m, left())
		if m.sessList.open || m.queueEdit == "" || m.input.LineInfo().ColumnOffset != col-1 {
			t.Fatalf("← in a queue edit: list open %v, editing %q, column %d (was %d)", m.sessList.open, m.queueEdit, m.input.LineInfo().ColumnOffset, col)
		}
		m.input.SetValue("")
		m, _ = press(m, left())
		if m.sessList.open || m.queueEdit == "" {
			t.Fatalf("← in an emptied queue edit: list open %v, editing %q", m.sessList.open, m.queueEdit)
		}
	})
	t.Run("dialog", func(t *testing.T) {
		m, _, _ := sessModel(t, 80, 24)
		m = m.openHelp()
		m, _ = press(m, left())
		if m.sessList.open || m.dialog != dialogHelp {
			t.Fatalf("← over /help: list open %v, dialog %v", m.sessList.open, m.dialog)
		}
	})
}

// TestSessionsLeftThroughACardInTheSubagentView: inside the sub-agent view
// with no card up, ← is the view's (back to the main transcript, no list);
// with a card up over the view, ← opens the list, and leaving the list
// returns to the view with the card still over it.
func TestSessionsLeftThroughACardInTheSubagentView(t *testing.T) {
	m, _, stub := sessModel(t, 80, 24)
	m = applyInFlight(t, m, []agent.ToolEvent{taskTool("task-1", "count lines", "in_progress")})
	m = openView(t, m)
	if back, _ := press(m, left()); back.viewing != "" || back.sessList.open {
		t.Fatalf("← in the sub-agent view with no card: viewing %q, list open %v", back.viewing, back.sessList.open)
	}
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(true)})
	card := m.cards
	m, _ = press(m, left())
	if !m.sessList.open {
		t.Fatalf("← with a card up in the sub-agent view did not open the list:\n%s", plainView(m))
	}
	if m.viewing != "task-1" || !reflect.DeepEqual(m.cards, card) || len(stub.Calls()) != 0 {
		t.Fatalf("behind the list: viewing %q, card kept %v, calls %+v", m.viewing, reflect.DeepEqual(m.cards, card), stub.Calls())
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	v := plainView(m)
	if miss := firstMissing(v, "permission Shell", "receipt only"); m.sessList.open || m.viewing != "task-1" || !reflect.DeepEqual(m.cards, card) || miss != "" {
		t.Fatalf("back from the list: list open %v, viewing %q, card kept %v, missing %q:\n%s", m.sessList.open, m.viewing, reflect.DeepEqual(m.cards, card), miss, v)
	}
}

// TestSessionsLeftThroughACardWithoutAList: with no session list
// (Config.Sessions nil) ← with a card up is the card's, as it always was:
// nothing on screen changes, nothing is answered, and the draft hidden under
// the card keeps its cursor.
func TestSessionsLeftThroughACardWithoutAList(t *testing.T) {
	m, stub := sizedCards(t)
	m = typeInto(t, m, "hi")
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	card, view, col := m.cards, plainView(m), m.input.LineInfo().ColumnOffset
	m, _ = press(m, left())
	if m.sessList.open || !reflect.DeepEqual(m.cards, card) || plainView(m) != view || len(stub.Calls()) != 0 {
		t.Fatalf("← with a card up and no list: list open %v, calls %+v\n%s", m.sessList.open, stub.Calls(), plainView(m))
	}
	if m.input.Value() != "hi" || m.input.LineInfo().ColumnOffset != col {
		t.Fatalf("← with a card up and no list reached the draft: %q at column %d (was %d)", m.input.Value(), m.input.LineInfo().ColumnOffset, col)
	}
}

// TestFrameGoldenSessionsOverACard (plan 032 §3.17): the list opened with ←
// over a session with a question card up, at 100×30 — the owner's mockup
// (richSnapshot) with the session the list came from rowed from its engine,
// so it is under "needs you" with its question, and the card is nowhere on
// screen. In process alone, as every list frame (golden_manifest_test.go).
func TestFrameGoldenSessionsOverACard(t *testing.T) {
	m, _, stub := sessModel(t, 100, 30)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	m, _ = press(m, left())
	if !m.sessList.open {
		t.Fatalf("← with a card up did not open the list:\n%s", plainView(m))
	}
	home := os.Getenv("HOME")
	snap := richSnapshot(home, m.hereKey())
	asking := hereFromEngine(t, m, "write the v0.1.0 release notes", filepath.Join(home, "projects", "craze"))
	replaced := false
	for i, r := range snap.Running {
		if sessRowKey(r) == m.hereKey() {
			snap.Running[i], replaced = asking, true
		}
	}
	if !replaced {
		t.Fatal("fixture: the mockup has no row for the session behind the list")
	}
	m = listSnap(t, m, snap)
	assertFrameGolden(t, "sessions-over-card-100x30", 100, 30, plainView(m),
		[]string{"! 3 need you", "needs you 3 ", "question: Pick one", "craze · here", "✳ "},
		[]string{"question 1/2", "Responding"})
}
