package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

// sizedCards is sized(t) with the stub kept, so a test can read back every
// answer the UI sent.
func sizedCards(t *testing.T) (Model, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	m := New(Config{
		Session:   stub,
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Model:     "grok",
		Yolo:      true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	return tm.(Model), stub
}

// cardEvent announces a blocking request to the stub and then delivers it to
// the model, which is the order the live session does it in.
func cardEvent(t *testing.T, m Model, stub *Stub, ev agent.Event) Model {
	t.Helper()
	stub.noteOpen(ev)
	tm, _ := m.Update(eventMsg{ev})
	return tm.(Model)
}

func stubQuestion() *agent.QuestionEvent {
	return &agent.QuestionEvent{
		ID:    "ask-1",
		Title: "Question",
		Questions: []agent.Question{
			{
				ID:      "q1",
				Prompt:  "Pick one",
				Options: []agent.Option{{ID: "opt-a", Label: "A"}, {ID: "opt-b", Label: "B"}},
			},
			{
				ID:            "q2",
				Prompt:        "Pick any",
				AllowMultiple: true,
				Options: []agent.Option{
					{ID: "opt-x", Label: "X"},
					{ID: "opt-y", Label: "Y"},
					{ID: "opt-z", Label: "Z"},
				},
			},
		},
	}
}

func stubPlanEvent() *agent.PlanEvent {
	return &agent.PlanEvent{
		ID:       "plan-1",
		Name:     "Fake Plan",
		Overview: "Two steps, then stop.",
		Plan:     "## Steps\n\n- read main.go\n- edit main.go\n",
		Todos: []agent.Todo{
			{ID: "1", Content: "Read main.go", Status: "pending"},
			{ID: "2", Content: "Edit main.go", Status: "pending"},
		},
	}
}

func stubPermissionEvent(always bool) *agent.PermissionEvent {
	opts := []agent.PermissionOption{
		{OptionID: "opt-once", Name: "Allow once", Kind: kindAllowOnce},
		{OptionID: "opt-reject", Name: "Reject once", Kind: kindRejectOnce},
	}
	if always {
		opts = append([]agent.PermissionOption{
			{OptionID: "opt-always", Name: "Allow always", Kind: kindAllowAlways},
		}, opts...)
	}
	return &agent.PermissionEvent{ID: "perm-1", Tool: "Shell", Options: opts}
}

func questionCard(t *testing.T) (Model, *Stub) {
	t.Helper()
	m, stub := sizedCards(t)
	return cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()}), stub
}

func planCard(t *testing.T) (Model, *Stub) {
	t.Helper()
	m, stub := sizedCards(t)
	return cardEvent(t, m, stub, agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()}), stub
}

func press(m Model, k tea.KeyMsg) (Model, tea.Cmd) {
	tm, cmd := m.Update(k)
	return tm.(Model), cmd
}

func TestQuestionCardWalksTheQuestionsAndAnswersOnce(t *testing.T) {
	m, stub := questionCard(t)
	view := plainView(m)
	for _, want := range []string{"question 1/2  Pick one", "> 1 A", "  2 B", "esc skip"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Pick any") {
		t.Fatalf("questions come one at a time:\n%s", view)
	}

	// A single-select picks and advances on the number.
	m, _ = press(m, runeKey('1'))
	if calls := stub.Calls(); len(calls) != 0 {
		t.Fatalf("the answer is only sent after the last question, got %+v", calls)
	}
	view = plainView(m)
	if !strings.Contains(view, "question 2/2  Pick any") {
		t.Fatalf("the card should be on the second question:\n%s", view)
	}
	if !strings.Contains(view, "> 1 [ ] X") {
		t.Fatalf("a multi-select shows its toggles:\n%s", view)
	}

	// Numbers toggle a multi-select; the arrows move the cursor.
	m, _ = press(m, runeKey('1'))
	m, _ = press(m, runeKey('3'))
	view = plainView(m)
	if !strings.Contains(view, "[x] X") || !strings.Contains(view, "[x] Z") || !strings.Contains(view, "[ ] Y") {
		t.Fatalf("toggles did not take:\n%s", view)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyUp})
	if !strings.Contains(plainView(m), "> 2 [ ] Y") {
		t.Fatalf("↑ should move the cursor:\n%s", plainView(m))
	}

	m, _ = press(m, enter())
	if m.cardOpen() {
		t.Fatal("the card should be gone once it is answered")
	}
	calls := stub.Calls()
	if len(calls) != 1 {
		t.Fatalf("every card answers exactly once, got %+v", calls)
	}
	c := calls[0]
	if c.Method != "question" || c.ID != "ask-1" || c.Skip {
		t.Fatalf("answer %+v", c)
	}
	if got := strings.Join(c.Answers["q1"], ","); got != "opt-a" {
		t.Fatalf("q1 = %q", got)
	}
	if got := strings.Join(c.Answers["q2"], ","); got != "opt-x,opt-z" {
		t.Fatalf("q2 = %q", got)
	}
	// One note per question, naming what was picked.
	transcript := plainView(m)
	for _, want := range []string{"? Pick one → A", "? Pick any → X, Z"} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("missing note %q:\n%s", want, transcript)
		}
	}
}

// TestQuestionCardFitsAShortTerminal holds the height contract with the
// tallest card craze draws, and degradation step 7: a band too short for the
// box gives the border up and keeps the question itself.
func TestQuestionCardFitsAShortTerminal(t *testing.T) {
	for _, tc := range []struct{ cols, rows int }{{100, 30}, {80, 24}, {40, 12}} {
		t.Run(fmt.Sprintf("%dx%d", tc.cols, tc.rows), func(t *testing.T) {
			m, stub := sizedCards(t)
			tm, _ := m.Update(tea.WindowSizeMsg{Width: tc.cols, Height: tc.rows})
			m = cardEvent(t, tm.(Model), stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})

			view := plainView(m)
			if h := lipgloss.Height(view); h != tc.rows {
				t.Fatalf("frame is %d rows, want %d:\n%s", h, tc.rows, view)
			}
			for _, ln := range strings.Split(view, "\n") {
				if w := lipgloss.Width(ln); w != tc.cols {
					t.Fatalf("line is %d wide, want %d: %q", w, tc.cols, ln)
				}
			}
			if got := m.lay.Region(regionTranscript).Height(); got < minTranscriptRows {
				t.Fatalf("transcript kept %d rows, want at least %d", got, minTranscriptRows)
			}
			// However few rows the band was left, the question is in them: at
			// the minimum size that is the whole card.
			band := m.lay.Region(regionModal)
			if band.Empty() {
				t.Fatalf("the card must be drawn: %+v", m.lay)
			}
			lines := strings.Split(view, "\n")
			found := false
			for y := band.Top; y < band.Bottom && y < len(lines); y++ {
				if strings.Contains(lines[y], "question 1/2") {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("the cropped card lost the question itself:\n%s", strings.Join(lines[band.Top:band.Bottom], "\n"))
			}
		})
	}
}

func TestQuestionCardEscSkipsTheWholeRequest(t *testing.T) {
	m, stub := questionCard(t)
	// Halfway through is still a skip of the request, not of one question.
	m, _ = press(m, runeKey('1'))
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.cardOpen() {
		t.Fatal("esc should close the card")
	}
	if m.status == statusWorking {
		t.Fatal("the fixture is idle; esc must not have cancelled a turn")
	}
	calls := stub.Calls()
	if len(calls) != 1 || !calls[0].Skip || calls[0].ID != "ask-1" {
		t.Fatalf("esc should skip once, got %+v", calls)
	}
	if !strings.Contains(plainView(m), "? Question → skipped") {
		t.Fatalf("missing the skipped note:\n%s", plainView(m))
	}
}

func TestPlanCardWritesTheBlockAndAnswers(t *testing.T) {
	for _, tc := range []struct {
		key    tea.KeyMsg
		accept bool
		note   string
	}{
		{runeKey('a'), true, "plan Fake Plan → accepted"},
		{runeKey('r'), false, "plan Fake Plan → rejected"},
	} {
		t.Run(tc.note, func(t *testing.T) {
			m, stub := planCard(t)
			view := plainView(m)
			// The plan itself is transcript material…
			for _, want := range []string{"PLAN Fake Plan", "Two steps, then stop.", "• read main.go", "○ Read main.go"} {
				if !strings.Contains(view, want) {
					t.Fatalf("missing %q from the transcript block:\n%s", want, view)
				}
			}
			// …and the card is the two lines that answer it.
			if !strings.Contains(view, "plan Fake Plan") || !strings.Contains(view, "[a]ccept  [r]eject  esc cancel") {
				t.Fatalf("missing the plan card:\n%s", view)
			}
			if got := m.lay.Region(regionModal).Height(); got != 2 {
				t.Fatalf("the plan card is %d rows, want 2", got)
			}

			m, _ = press(m, tc.key)
			if m.cardOpen() {
				t.Fatal("the card should be gone")
			}
			calls := stub.Calls()
			if len(calls) != 1 || calls[0].Method != "plan" || calls[0].ID != "plan-1" || calls[0].Accept != tc.accept {
				t.Fatalf("plan answered %+v", calls)
			}
			if !strings.Contains(plainView(m), tc.note) {
				t.Fatalf("missing note %q:\n%s", tc.note, plainView(m))
			}
		})
	}
}

func TestPlanCardEscCancels(t *testing.T) {
	m, stub := planCard(t)
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.cardOpen() {
		t.Fatal("esc should close the card")
	}
	if msg := runCmd(cmd); msg != nil {
		t.Fatalf("cancel failed: %v", msg)
	}
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].ID != "plan-1" || !calls[0].Cancelled {
		t.Fatalf("esc should cancel the plan exactly once, got %+v", calls)
	}
}

func TestPermissionAlwaysOnlyWhenOffered(t *testing.T) {
	t.Run("offered", func(t *testing.T) {
		m, stub := sizedCards(t)
		m.yolo = false
		m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(true)})
		view := plainView(m)
		if !strings.Contains(view, "permission Shell  [a]llow once  [A]lways  [n] reject") {
			t.Fatalf("missing the permission line:\n%s", view)
		}
		m, _ = press(m, runeKey('A'))
		if m.cardOpen() {
			t.Fatal("A should answer the request")
		}
		calls := stub.Calls()
		// The pick is by kind, carrying the request's own optionId.
		if len(calls) != 1 || calls[0].Option != "opt-always" {
			t.Fatalf("A picked %+v", calls)
		}
	})

	// A request that offers only allow_always must not advertise — or accept —
	// the other two: pressing them would pop the card, find no option and send
	// an empty id, which the session turns into `cancelled`.
	t.Run("only always", func(t *testing.T) {
		m, stub := sizedCards(t)
		m.yolo = false
		perm := &agent.PermissionEvent{
			ID:      "perm-1",
			Tool:    "Shell",
			Options: []agent.PermissionOption{{OptionID: "opt-always", Kind: kindAllowAlways}},
		}
		m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: perm})
		view := plainView(m)
		if !strings.Contains(view, "permission Shell  [A]lways") {
			t.Fatalf("only the offered action is drawn:\n%s", view)
		}
		if strings.Contains(view, "[a]llow once") || strings.Contains(view, "[n] reject") {
			t.Fatalf("an action the request never offered is drawn:\n%s", view)
		}
		for _, k := range []rune{'a', 'n', 'r', 'y', '1', '2'} {
			var cmd tea.Cmd
			m, cmd = press(m, runeKey(k))
			if !m.cardOpen() || cmd != nil {
				t.Fatalf("%q answered a request that does not offer it", k)
			}
		}
		if calls := stub.Calls(); len(calls) != 0 {
			t.Fatalf("an unavailable action reached the session: %+v", calls)
		}
		m, _ = press(m, runeKey('A'))
		if calls := stub.Calls(); len(calls) != 1 || calls[0].Option != "opt-always" {
			t.Fatalf("the offered action should still work: %+v", calls)
		}
	})

	// Nothing craze knows how to bind: the card says so rather than offering
	// keys that cannot work.
	t.Run("nothing offered", func(t *testing.T) {
		m, stub := sizedCards(t)
		m.yolo = false
		perm := &agent.PermissionEvent{
			ID:      "perm-1",
			Tool:    "Shell",
			Options: []agent.PermissionOption{{OptionID: "opt-never", Kind: "reject_always"}},
		}
		m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: perm})
		if got := plainView(m); !strings.Contains(got, "permission Shell  esc cancel") {
			t.Fatalf("a card with no bound action should say so:\n%s", got)
		}
	})

	t.Run("not offered", func(t *testing.T) {
		m, stub := sizedCards(t)
		m.yolo = false
		m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(false)})
		if strings.Contains(plainView(m), "[A]lways") {
			t.Fatalf("allow_always was not offered:\n%s", plainView(m))
		}
		m, cmd := press(m, runeKey('A'))
		if !m.cardOpen() || cmd != nil {
			t.Fatal("A must do nothing when the request does not offer allow_always")
		}
		m, _ = press(m, runeKey('a'))
		if m.cardOpen() {
			t.Fatal("a should still allow once")
		}
		if calls := stub.Calls(); len(calls) != 1 || calls[0].Option != "opt-once" {
			t.Fatalf("a picked %+v", calls)
		}
	})
}

func TestQueuedCardsAnsweredInOrder(t *testing.T) {
	m, stub := sizedCards(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()})
	if len(m.cards) != 2 {
		t.Fatalf("both requests should queue, got %d", len(m.cards))
	}
	// Only the head is drawn: the plan card's own line is not on screen while
	// the question is up (its transcript block is, which is a different row).
	band := m.lay.Region(regionModal)
	lines := strings.Split(plainView(m), "\n")
	for y := band.Top; y < band.Bottom && y < len(lines); y++ {
		if strings.Contains(lines[y], "[a]ccept") {
			t.Fatalf("the queued plan card is drawn under the question card:\n%s", strings.Join(lines, "\n"))
		}
	}

	// Answer the question; the plan takes its place.
	m, _ = press(m, runeKey('1'))
	m, _ = press(m, enter())
	if len(m.cards) != 1 || m.cards[0].kind != cardPlan {
		t.Fatalf("the plan card should be next, got %+v", m.cards)
	}
	if !strings.Contains(plainView(m), "[a]ccept") {
		t.Fatalf("the plan card should be drawn now:\n%s", plainView(m))
	}
	m, _ = press(m, runeKey('a'))
	if m.cardOpen() {
		t.Fatal("the queue should be empty")
	}
	calls := stub.Calls()
	if len(calls) != 2 || calls[0].Method != "question" || calls[1].Method != "plan" {
		t.Fatalf("cards answered out of order or more than once: %+v", calls)
	}
}

func TestCtrlCAnswersEveryQueuedCardOnce(t *testing.T) {
	m, stub := sizedCards(t)
	stub.HangNext()
	m.input.SetValue("go")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking {
		t.Fatalf("fixture is %s, want working", m.status)
	}
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()})

	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if m.quitting {
		t.Fatal("ctrl+c with a card up should cancel, not quit")
	}
	if m.cardOpen() {
		t.Fatal("cancelling drops the whole queue")
	}
	if msg := runCmd(cmd); msg != nil {
		t.Fatalf("cancel failed: %v", msg)
	}
	calls := stub.Calls()
	if len(calls) != 2 {
		t.Fatalf("each queued card is answered exactly once, got %+v", calls)
	}
	for _, c := range calls {
		if !c.Cancelled {
			t.Fatalf("a cancelled turn answers every card cancelled, got %+v", c)
		}
	}

	// Nothing is left to answer a second time.
	m, cmd = press(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if !m.quitting {
		t.Fatal("the second ctrl+c inside the window quits")
	}
	runCmd(cmd)
	if got := len(stub.Calls()); got != 2 {
		t.Fatalf("quitting answered a card twice: %+v", stub.Calls())
	}
}

// TestAnswerReachesTheSessionBeforeACancelCan is the card-UI half of the
// waiting-map seam: the request must leave the session in the same Update that
// pops the card. If the answer were a command that ran later, a cancel pressed
// in between would claim the request and send cursor `cancelled` for a
// question the user had already answered — and the transcript would already
// say otherwise.
func TestAnswerReachesTheSessionBeforeACancelCanClaimIt(t *testing.T) {
	m, stub := sizedCards(t)
	stub.HangNext()
	m.input.SetValue("go")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()})

	m, _ = press(m, runeKey('1'))
	m, _ = press(m, enter())
	// No command carries the answer: it is already in the session, so the
	// cancel below cannot take the request back.
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].Method != "question" || calls[0].Cancelled || calls[0].Skip {
		t.Fatalf("the answer must reach the session in the same update: %+v", calls)
	}
	if !strings.Contains(plainView(m), "? Pick one → A") {
		t.Fatalf("the note follows the answer the session took:\n%s", plainView(m))
	}

	// Cancelling now can only claim what is still queued.
	_, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if msg := runCmd(cmd); msg != nil {
		t.Fatalf("cancel failed: %v", msg)
	}
	calls = stub.Calls()
	if len(calls) != 2 {
		t.Fatalf("one answer and one cancel, got %+v", calls)
	}
	if calls[0].Cancelled {
		t.Fatalf("the cancel claimed the answered question: %+v", calls[0])
	}
	if calls[1].Method != "plan" || !calls[1].Cancelled {
		t.Fatalf("the queued plan should be the cancelled one: %+v", calls[1])
	}
}

// TestCardEventAfterACancelIsDropped is the other half: a card event already
// on its way when the turn was cancelled must not raise a card, because the
// session answered that request on the way out and will not park another one
// for this turn.
func TestCardEventAfterACancelIsDropped(t *testing.T) {
	m, stub := sizedCards(t)
	stub.HangNext()
	m.input.SetValue("go")
	tm, _ := m.Update(enter())
	m = cardEvent(t, tm.(Model), stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})

	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	runCmd(cmd)

	late := stubQuestion()
	late.ID = "ask-2"
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: late})
	if m.cardOpen() {
		t.Fatalf("an event from a cancelled turn raised a card: %+v", m.cards)
	}
	if strings.Contains(plainView(m), "question 1/2") {
		t.Fatalf("a ghost card is on screen:\n%s", plainView(m))
	}

	// The next turn takes cards again.
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventDone, StopReason: "cancelled"}})
	m = cardEvent(t, tm.(Model), stub, agent.Event{Type: agent.EventQuestion, Question: late})
	if !m.cardOpen() {
		t.Fatal("a card after the turn ended is a real card again")
	}
}

func TestQuitAnswersQueuedCards(t *testing.T) {
	m, stub := sizedCards(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlD})
	if !m.quitting {
		t.Fatal("ctrl+d always quits")
	}
	runCmd(cmd)
	calls := stub.Calls()
	if len(calls) != 1 || !calls[0].Cancelled || calls[0].ID != "ask-1" {
		t.Fatalf("closing the session answers the queue cancelled: %+v", calls)
	}
}

// TestCardsOwnTheKeyboard holds §3.10: the global keys are ignored while a
// card is up, and the draft is never touched.
func TestCardsOwnTheKeyboard(t *testing.T) {
	m, stub := sizedCards(t)
	m.input.SetValue("half a thought")
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})

	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyCtrlT},
		{Type: tea.KeyCtrlG},
		{Type: tea.KeyCtrlO},
		{Type: tea.KeyShiftTab},
	} {
		var cmd tea.Cmd
		m, cmd = press(m, k)
		if cmd != nil {
			t.Fatalf("%v should be ignored while a card is up", k)
		}
	}
	if m.tasksState != tasksCompact {
		t.Fatalf("ctrl+t cycled the panel behind the card: %v", m.tasksState)
	}
	if m.dialog == dialogTheme {
		t.Fatal("ctrl+g opened the theme picker behind the card")
	}
	if m.expanded {
		t.Fatal("ctrl+o toggled the transcript behind the card")
	}
	if m.snap.CurrentMode != "agent" {
		t.Fatalf("shift+tab cycled the mode behind the card: %q", m.snap.CurrentMode)
	}
	if !m.cardOpen() {
		t.Fatal("none of those keys answers the card")
	}
	if m.input.Value() != "half a thought" {
		t.Fatalf("the card touched the draft: %q", m.input.Value())
	}
}

// TestCardSuspendsTheLowerOverlays is §3.11's stack: the picker closes with its
// preview reverted, and the slash menu the draft opened is not drawn.
func TestCardSuspendsTheLowerOverlays(t *testing.T) {
	m, stub := sizedCards(t)
	m = m.openThemePicker()
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyDown})
	preview := m.theme.Name
	if preview == "tokyo-night" {
		t.Fatal("the picker should have previewed another theme")
	}
	m.input.SetValue("/")
	m.slashHide = false
	if !m.slashMenuOpen() {
		t.Fatal("fixture: the slash menu should be open")
	}

	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	if m.dialog == dialogTheme {
		t.Fatal("a card closes the theme picker")
	}
	if m.theme.Name != "tokyo-night" {
		t.Fatalf("the live preview survived the card: theme is %q", m.theme.Name)
	}
	if !m.lay.Region(regionOverlay).Empty() || m.overlayView() != "" {
		t.Fatalf("the overlay band should be suspended while a card is up: %+v", m.lay)
	}
	if m.input.Value() != "/" {
		t.Fatalf("the draft is kept: %q", m.input.Value())
	}
}

// TestCardArrivalClosesTheHelpAndThePeek covers the rest of the stack.
func TestCardArrivalClosesTheHelpAndThePeek(t *testing.T) {
	m, stub := sizedCards(t)
	m.help = true
	m.agentPeek = true
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(true)})
	if m.help || m.agentPeek {
		t.Fatalf("a card closes the lower overlays: help=%v peek=%v", m.help, m.agentPeek)
	}
}
