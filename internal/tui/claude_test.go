package tui

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// claudeCatalog is claude-agent-acp's catalog, as its session/new answers it
// (L0's real frames): mode, model, effort, fast and ultracode.
func claudeCatalog() []agent.ConfigOption {
	sel := func(id, name, cat, cur string, vals ...string) agent.ConfigOption {
		opt := agent.ConfigOption{ID: id, Name: name, Category: cat, Type: "select", Current: cur}
		for _, v := range vals {
			opt.SelectValues = append(opt.SelectValues, agent.SelectValue{Value: v, Name: v})
		}
		return opt
	}
	return []agent.ConfigOption{
		sel("model", "Model", "model", "opus", "opus", "haiku"),
		sel("effort", "Effort", "thought_level", "high", "low", "high"),
		sel("fast", "Fast mode", "model_config", "off", "on", "off"),
		sel("ultracode", "Ultracode", "model_config", "off", "on", "off"),
	}
}

// TestClaudeModelDialogHidesFast (plan 052 § D3): claude's picker offers no
// fast tab — fast mode bills extra usage on a Claude plan — and keeps every
// other option the catalog has. Cursor's keeps its fast tab.
func TestClaudeModelDialogHidesFast(t *testing.T) {
	cfg := claudeCatalog()
	for _, tc := range []struct {
		p        agent.Provider
		wantFast bool
	}{{agent.ClaudeProvider(), false}, {agent.CursorProvider(), true}} {
		tabs := catalogTabs(agent.Snapshot{Provider: tc.p.Info(), Config: cfg})
		var fast, ultra bool
		for _, tab := range tabs {
			fast = fast || tab.opt.ID == "fast"
			ultra = ultra || tab.opt.ID == "ultracode"
		}
		if fast != tc.wantFast || !ultra {
			t.Fatalf("%s: fast tab %v, ultracode tab %v", tc.p.Name(), fast, ultra)
		}
	}
}

// TestClaudeImplementsInManualAndOffersNoPlan (plan 052 § C1, § B5): claude's
// implement mode is default (Manual), and its empty implement prompt is what
// keeps a plan-mode turn from offering one.
func TestClaudeImplementsInManualAndOffersNoPlan(t *testing.T) {
	m := Model{}
	m.snap.Provider = agent.ClaudeProvider().Info()
	for _, id := range []string{"default", "acceptEdits", "plan", "auto", "bypassPermissions"} {
		m.snap.Modes = append(m.snap.Modes, agent.ModeInfo{ID: id})
	}
	m.snap.CurrentMode = "plan"
	if got := m.implementModeID(); got != "default" {
		t.Fatalf("implement mode %q", got)
	}
	if m.snap.Provider.ImplementPrompt() != "" {
		t.Fatal("claude has an implement prompt")
	}
	if sessProviderDetail["claude"] != "your Claude Code, on your Claude plan" {
		t.Fatalf("detail %q", sessProviderDetail["claude"])
	}
}

// claudeModeIDs are the adapter's five modes, in its order.
var claudeModeIDs = []string{"default", "acceptEdits", "plan", "auto", "bypassPermissions"}

// claudeScripted is a scripted session on the claude provider, its five modes
// advertised and the session in mode.
func claudeScripted(t *testing.T, p agent.Provider, mode string, modes ...string) (Model, *scriptedSession) {
	t.Helper()
	isolateSkillsHome(t)
	s := newScriptedSession()
	s.SetProvider(p)
	s.Stub.mu.Lock()
	s.snap.Modes = nil
	for _, id := range modes {
		s.snap.Modes = append(s.snap.Modes, agent.ModeInfo{ID: id, Name: id})
	}
	s.snap.CurrentMode = mode
	s.Stub.mu.Unlock()
	return startSession(t, s, t.TempDir(), 80, 24), s
}

// shiftTab is one Shift+Tab, its SetMode run and applied.
func shiftTab(t *testing.T, m Model) Model {
	t.Helper()
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = tm.(Model)
	if cmd == nil {
		t.Fatalf("Shift+Tab did nothing in mode %q", m.snap.CurrentMode)
	}
	tm, _ = m.Update(runCmd(cmd))
	return tm.(Model)
}

// TestClaudeShiftTabCycle (plan 052 § C1, AC11; L1's finding): with C3's
// Modes capability the key works on claude, and it walks Manual → accept edits
// → plan → Manual. It never lands on auto or bypassPermissions; from either of
// them — reachable by name only — it goes to Manual.
func TestClaudeShiftTabCycle(t *testing.T) {
	m, _ := claudeScripted(t, agent.ClaudeProvider(), "default", claudeModeIDs...)
	if !m.showModes() {
		t.Fatal("claude shows no modes")
	}
	var got []string
	for range 6 {
		m = shiftTab(t, m)
		got = append(got, m.snap.CurrentMode)
	}
	if want := []string{"acceptEdits", "plan", "default", "acceptEdits", "plan", "default"}; !slices.Equal(got, want) {
		t.Fatalf("cycle %v, want %v", got, want)
	}
	for _, from := range []string{"auto", "bypassPermissions"} {
		m, _ := claudeScripted(t, agent.ClaudeProvider(), from, claudeModeIDs...)
		if m = shiftTab(t, m); m.snap.CurrentMode != "default" {
			t.Fatalf("from %s: %q, want default", from, m.snap.CurrentMode)
		}
	}
}

// TestClaudePlanTurnOffersNothing (plan 052 § B5, AC7): a claude plan-mode
// turn that says its plan in words and ends end_turn arms no implement offer —
// claude approves a plan on its own card, and its empty implement prompt is
// what planEarnsOffer turns the offer off on. The same turn on grok, whose
// prompt is not empty, does arm it: the control for the gate.
func TestClaudePlanTurnOffersNothing(t *testing.T) {
	for _, tc := range []struct {
		p     agent.Provider
		modes []string
		offer bool
	}{
		{agent.ClaudeProvider(), claudeModeIDs, false},
		{agent.GrokProvider(), []string{"default", "plan", "ask"}, true},
	} {
		t.Run(tc.p.Name(), func(t *testing.T) {
			m, sess := claudeScripted(t, tc.p, "plan", tc.modes...)
			sc := scriptHeld()
			m = startScripted(t, m, sess, "plan it", sc)
			sess.Emit(agent.Event{Type: agent.EventText, Text: "Here is the plan: write x."})
			sc.Release()
			m = pumpUntil(t, m, isIdle)
			m = pumpSettled(t, m)
			if m.sawAssistantSeq != m.turnSeq {
				t.Fatal("setup: the turn said nothing in words")
			}
			if got := m.planArmed(); got != tc.offer {
				t.Fatalf("planArmed = %v, want %v:\n%s", got, tc.offer, plainView(m))
			}
			if got := strings.Contains(plainView(m), planOfferPlaceholder); got != tc.offer {
				t.Fatalf("the offer drawn = %v, want %v:\n%s", got, tc.offer, plainView(m))
			}
		})
	}
}

// claudeQuestion is AskUserQuestion's card as the claude dialect raises it:
// a single-select with its "Other" box, and a multi-select with its own.
func claudeQuestion() *agent.QuestionEvent {
	return &agent.QuestionEvent{
		ID: "ask-1",
		Questions: []agent.Question{
			{ID: "question_0", Prompt: "Do you prefer red or blue?", AllowText: true, Options: []agent.Option{
				{ID: "Red", Label: "Red", Description: "Choose red"}, {ID: "Blue", Label: "Blue", Description: "Choose blue"},
			}},
			{ID: "question_1", Prompt: "Which sizes?", AllowMultiple: true, AllowText: true, Options: []agent.Option{
				{ID: "S", Label: "Small"}, {ID: "L", Label: "Large"},
			}},
		},
	}
}

// TestQuestionCardOtherRow (plan 052 § B4, AC6): a question that takes free
// text has an "Other…" row after its options, numbered after them. Picking it
// opens a one-line input; Esc goes back with nothing kept; Enter takes the
// text, which answers a single-select alone and joins a multi-select's
// toggles. The answer carries the text, and the transcript note quotes it.
func TestQuestionCardOtherRow(t *testing.T) {
	m, stub := sizedCards(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: claudeQuestion()})
	view := plainView(m)
	for _, want := range []string{"> 1 Red", "Choose red", "  2 Blue", "  3 Other…"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q:\n%s", want, view)
		}
	}
	// 3 opens the input; Esc closes it with nothing kept.
	m, _ = press(m, runeKey('3'))
	m, _ = press(m, runeKey('x'))
	if !strings.Contains(plainView(m), "> 3 Other: x▏") || !strings.Contains(plainView(m), "enter send · esc back") {
		t.Fatalf("the input is not open:\n%s", plainView(m))
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if !m.cardOpen() || !strings.Contains(plainView(m), "> 3 Other…") {
		t.Fatalf("esc should go back to the options, the card open:\n%s", plainView(m))
	}
	// Enter on the row opens it again; typed text answers the single-select.
	m, _ = press(m, enter())
	for _, r := range "teal" {
		m, _ = press(m, runeKey(r))
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeySpace})
	m, _ = press(m, runeKey('!'))
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyBackspace})
	m, _ = press(m, enter())
	view = plainView(m)
	if !strings.Contains(view, "question 2/2  Which sizes?") || !strings.Contains(view, "  3     Other…") {
		t.Fatalf("the card should be on the second question:\n%s", view)
	}
	// The multi-select: Large toggled, then the input, whose Enter confirms.
	m, _ = press(m, runeKey('2'))
	m, _ = press(m, runeKey('3'))
	for _, r := range "XL" {
		m, _ = press(m, runeKey(r))
	}
	m, _ = press(m, enter())
	if m.cardOpen() {
		t.Fatalf("the card should be answered:\n%s", plainView(m))
	}
	calls := stub.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls %+v", calls)
	}
	c := calls[0]
	if len(c.Answers["question_0"]) != 0 || !slices.Equal(c.Answers["question_1"], []string{"L"}) {
		t.Fatalf("answers %+v", c.Answers)
	}
	if c.Text["question_0"] != "teal" || c.Text["question_1"] != "XL" {
		t.Fatalf("text %+v", c.Text)
	}
	m = endingDelivered(t, m, stub)
	for _, want := range []string{`? Do you prefer red or blue? → "teal"`, `? Which sizes? → Large, "XL"`} {
		if !strings.Contains(plainView(m), want) {
			t.Fatalf("missing note %q:\n%s", want, plainView(m))
		}
	}
}

// TestQuestionCardWithoutTextHasNoOtherRow: a question that takes no free
// text — every cursor and grok question — draws no "Other…" row, and its
// numbers stop at its options.
func TestQuestionCardWithoutTextHasNoOtherRow(t *testing.T) {
	m, stub := questionCard(t)
	if strings.Contains(plainView(m), "Other") {
		t.Fatalf("an Other row on a question that takes no text:\n%s", plainView(m))
	}
	m, _ = press(m, runeKey('3'))
	if !strings.Contains(plainView(m), "question 1/2") || len(stub.Calls()) != 0 {
		t.Fatalf("3 did something on a two-option question:\n%s", plainView(m))
	}
}

// TestClaudeQuestionEscCancels (plan 052 § B4 as amended, 2026-11-17): Esc on
// a claude question card cancels it — Claude Code's Esc, which aborts the
// tool — rather than skipping it, which would let the tool run on empty
// answers. The hint says so, and the row reads cancelled. Cursor's card still
// skips (TestQuestionCardEscSkipsTheWholeRequest).
func TestClaudeQuestionEscCancels(t *testing.T) {
	m, stub := sizedCards(t)
	stub.SetProvider(agent.ClaudeProvider())
	m.snap.Provider = agent.ClaudeProvider().Info()
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: claudeQuestion()})
	if !strings.Contains(plainView(m), "enter select · esc cancel") {
		t.Fatalf("the hint:\n%s", plainView(m))
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].Skip || !calls[0].Cancelled {
		t.Fatalf("esc sent %+v", calls)
	}
	if rec, ok := stub.Asks().Record("ask-1"); !ok || !rec.Answer.Cancel {
		t.Fatalf("the record %+v", rec)
	}
	m = endingDelivered(t, m, stub)
	if !strings.Contains(plainView(m), "? Do you prefer red or blue? → cancelled") {
		t.Fatalf("missing the cancelled row:\n%s", plainView(m))
	}
}

// TestRequiredMultiSelectTakesNoEmptyConfirm: a question that must be answered
// does not take Enter with nothing toggled and nothing typed.
func TestRequiredMultiSelectTakesNoEmptyConfirm(t *testing.T) {
	m, stub := sizedCards(t)
	q := claudeQuestion()
	q.Questions = q.Questions[1:]
	q.Questions[0].Required = true
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: q})
	m, _ = press(m, enter())
	if !m.cardOpen() || len(stub.Calls()) != 0 {
		t.Fatalf("an empty confirm answered a required question: %+v", stub.Calls())
	}
	m, _ = press(m, runeKey('1'))
	m, _ = press(m, enter())
	if m.cardOpen() || len(stub.Calls()) != 1 {
		t.Fatalf("a pick did not answer it: %+v", stub.Calls())
	}
}

// TestClaudeSteerPromptRequiredQueuesTheText (plan 052 § B7, review C3 item
// 8): the turn ends just before claude takes a steer, and claude answers
// promptRequired. The text is not lost: the engine queues it as the next
// prompt, and the draft is cleared, the message shown queued.
func TestClaudeSteerPromptRequiredQueuesTheText(t *testing.T) {
	m, sess := scriptedModel(t)
	sess.SetProvider(agent.ClaudeProvider())
	m = deliver(t, m, refreshSnapMsg{})
	sc := scriptHeld()
	m = startScripted(t, m, sess, "go", sc)
	sess.FailNextInterject(agent.ErrPromptRequired)
	m.input.SetValue("BANANA")
	m = pumpKey(t, m, tea.KeyMsg{Type: tea.KeyCtrlL})
	m = pumpUntil(t, m, func(m Model) bool { return m.input.Value() == "" && len(m.queue) == 1 })
	if m.queue[0].Text != "BANANA" {
		t.Fatalf("the queued text %+v\n%s", m.queue, plainView(m))
	}
	if strings.Contains(plainView(m), "nothing to interject into") {
		t.Fatalf("a queued steer says it was refused:\n%s", plainView(m))
	}
	sc.Release()
}

// TestRequiredSingleChoiceNeedsAPick (review C3c 3): on a two-question card
// whose first question is required and single-choice, text in Other… alone
// does not advance it — the registry would refuse the whole answer, and the
// card has no way back to the first question. Picking an option then
// advances, the text riding along.
func TestRequiredSingleChoiceNeedsAPick(t *testing.T) {
	m, stub := sizedCards(t)
	q := claudeQuestion()
	q.Questions[0].Required = true
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: q})
	m, _ = press(m, runeKey('3'))
	for _, r := range "teal" {
		m, _ = press(m, runeKey(r))
	}
	m, _ = press(m, enter())
	if !strings.Contains(plainView(m), "question 1/2") {
		t.Fatalf("Other text alone advanced a required question:\n%s", plainView(m))
	}
	m, _ = press(m, runeKey('2'))
	if !strings.Contains(plainView(m), "question 2/2") {
		t.Fatalf("a pick did not advance it:\n%s", plainView(m))
	}
	m, _ = press(m, runeKey('1'))
	_, _ = press(m, enter())
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].Cancelled || !slices.Equal(calls[0].Answers["question_0"], []string{"Blue"}) || calls[0].Text["question_0"] != "teal" {
		t.Fatalf("calls %+v", calls)
	}
}

// TestClaudeForeignTurnShowsWorking (plan 052 L4 O1): while claude runs a turn
// of its own the working line shows, with its Esc, and the empty composer says
// Enter queues; grok's interjection fallback keeps its own presentation.
func TestClaudeForeignTurnShowsWorking(t *testing.T) {
	for _, tc := range []struct {
		p       agent.Provider
		working bool
	}{
		{agent.ClaudeProvider(), true},
		{agent.GrokProvider(), false},
	} {
		m, _ := claudeScripted(t, tc.p, "default", claudeModeIDs...)
		if m.status == statusWorking || m.spinnerVisible() {
			t.Fatalf("%s: working before the turn", tc.p.Name())
		}
		m.snap.ForeignTurn = true
		m.foreignStart = m.now()
		line, hint := m.spinnerView(), m.composerHint()
		if tc.working != strings.Contains(line, "esc to interrupt") || tc.working != (hint == queueForeignHint) || tc.working != m.wantFastTick() {
			t.Fatalf("%s during its own turn: line %q, hint %q, fast tick %v", tc.p.Name(), line, hint, m.wantFastTick())
		}
	}
}
