package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/protocol"
)

// The side view (/btw, plan 040 §3.6, §7.7): offered where the session
// answers side questions, opened mid-turn, streamed, followed up, closed by
// Esc or Ctrl+C without touching the main session, and never written into it.

// asideModel is the standard config around an AsidingStub — a session that
// answers side questions, as a native, grok or gx one does — sized and
// started.
func asideModel(t *testing.T) (Model, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	return startSession(t, AsidingStub{Stub: stub}, t.TempDir(), 100, 30), stub
}

// asideWorking is asideModel with a turn of its own running, hung until it
// is cancelled.
func asideWorking(t *testing.T) (Model, *Stub) {
	t.Helper()
	m, stub := asideModel(t)
	stub.HangNext()
	m.input.SetValue("wait")
	m, _ = press(m, enter())
	if m.status != statusWorking {
		t.Fatalf("fixture: %s, want working", m.status)
	}
	waitInTurn(t, stub)
	return m, stub
}

// pressBtw types draft into the composer and presses Enter.
func pressBtw(t *testing.T, m Model, draft string) (Model, tea.Cmd) {
	t.Helper()
	m.input.SetValue(draft)
	return press(m, enter())
}

// answerBtw runs the side question's call cmd holds to its end and pumps its
// answer into m.
func answerBtw(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	ask, wait := mustCmd(t, cmd, "askBtwCmd"), mustCmd(t, cmd, "waitBtw")
	if msg := runWatched(t, ask); msg != nil {
		t.Fatalf("the call answered %#v, want nothing", msg)
	}
	return pumpBtw(t, m, wait)
}

// pumpBtw applies what the pump's waits take, until one hands back no wait.
func pumpBtw(t *testing.T, m Model, wait tea.Cmd) Model {
	t.Helper()
	for wait != nil {
		msg := runWatched(t, wait)
		if msg == nil {
			return m
		}
		var next tea.Cmd
		m, next = press(m, msg)
		wait = findCmd(next, "waitBtw")
	}
	return m
}

// btwRows is the side view's rows of kind, by text.
func btwRows(m Model, kind entryKind) []string {
	if m.btw == nil {
		return nil
	}
	var out []string
	for _, e := range m.btw.pane.rows {
		if e.kind == kind {
			out = append(out, e.text)
		}
	}
	return out
}

// paneHas reports a row of p whose text contains s.
func paneHas(p *pane, s string) bool {
	for _, e := range p.rows {
		if strings.Contains(e.text, s) {
			return true
		}
	}
	return false
}

// stubAnswer is AsidingStub's whole answer to req.
func stubAnswer(req agent.AsideRequest) string { return strings.Join(StubAsidePieces(req), "") }

// TestTheSideQuestionIsOfferedOnlyWhereTheSessionAnswersOne (§7.7): /btw is
// in the catalog — after the mode rows, before /exit — and /help lists it
// and the side view's keys only where the backend says aside; a cursor-like
// session's catalog and box are what they were, and its /btw goes to the
// agent as the text it is.
func TestTheSideQuestionIsOfferedOnlyWhereTheSessionAnswersOne(t *testing.T) {
	names := func(m Model) []string {
		var out []string
		for _, it := range m.slashCatalog() {
			if it.Builtin {
				out = append(out, it.Name)
			}
		}
		return out
	}
	helpHas := func(m Model, key string) bool {
		return slices.ContainsFunc(m.helpLines(), func(l helpLine) bool { return l.key == key || l.desc == key })
	}

	plain, stub := sizedCards(t)
	if slices.Contains(names(plain), "btw") || helpHas(plain, "/btw") || helpHas(plain, "side questions") || plain.btwOffered() {
		t.Fatalf("a session with no side questions offers /btw: %v", names(plain))
	}
	if builtinNamed("btw") {
		t.Fatal("/btw is one of the fixed builtins")
	}
	plain, _ = pressBtw(t, plain, "/btw hello")
	if plain.btw != nil || !slices.Contains(stub.Prompts(), "/btw hello") {
		t.Fatalf("a cursor-like session's /btw: view %v, prompts %q", plain.btw != nil, stub.Prompts())
	}

	m, _ := asideModel(t)
	got := names(m)
	if want := []string{"help", "model", "clear", "tasks", "theme", "rename", "plan", "ask", "agent", "btw", "exit"}; !slices.Equal(got, want) {
		t.Fatalf("the builtins are %v, want %v", got, want)
	}
	if !helpHas(m, "/btw") || !helpHas(m, "side questions") || !helpHas(m, "/btw question") {
		t.Fatal("/help on a session with side questions does not list /btw and its keys")
	}
}

// TestASideQuestionOpensMidTurnAndStreams (§7.7, §3.6): /btw on a running
// turn opens the side view at once — the draft consumed, the composer
// focused, the main turn still working and nothing sent into it — and the
// answer streams into an assistant row that grows under one generation and
// settles on the result's text, then the footer.
func TestASideQuestionOpensMidTurnAndStreams(t *testing.T) {
	m, stub := asideWorking(t)
	held, release := stub.HoldNextAside()
	defer release()
	m, cmd := pressBtw(t, m, "/btw what is going on?")
	switch {
	case m.btw == nil || m.cur() != m.btw.pane:
		t.Fatal("the side view did not open")
	case m.input.Value() != "" || !m.input.Focused():
		t.Fatalf("the composer is %q (focused %v), want empty and focused", m.input.Value(), m.input.Focused())
	case m.status != statusWorking:
		t.Fatalf("the main turn is %s", m.status)
	case !slices.Equal(btwRows(m, entryUser), []string{"what is going on?"}):
		t.Fatalf("the question rows: %q", btwRows(m, entryUser))
	case !slices.Equal(btwRows(m, entryNote), []string{btwHeaderNote}):
		t.Fatalf("the notes: %q", btwRows(m, entryNote))
	}
	ask, wait := mustCmd(t, cmd, "askBtwCmd"), mustCmd(t, cmd, "waitBtw")
	done := make(chan tea.Msg, 1)
	go func() { done <- ask() }()
	awaitStep(t, held, "the aside parking after its first piece")

	m, next := press(m, runWatched(t, wait))
	first := StubAsidePieces(agent.AsideRequest{Question: "what is going on?"})[0]
	if a := btwRows(m, entryAssistant); !slices.Equal(a, []string{first}) || m.btw.answer == nil || m.btw.answer.gen == 0 {
		t.Fatalf("after the first piece the answer rows are %q (row %+v)", a, m.btw.answer)
	}
	if !strings.Contains(plainView(m), "Answering · ") {
		t.Fatalf("no working line for the side question:\n%s", plainView(m))
	}
	gen := m.btw.answer.gen
	release()
	if msg := <-done; msg != nil {
		t.Fatalf("the call answered %#v", msg)
	}
	m = pumpBtw(t, m, findCmd(next, "waitBtw"))

	req := agent.AsideRequest{Question: "what is going on?"}
	if a := btwRows(m, entryAssistant); !slices.Equal(a, []string{stubAnswer(req)}) {
		t.Fatalf("the answer rows are %q", a)
	}
	row := m.btw.pane.rows[2]
	if row.gen != 0 || row.md != nil || m.btw.pending != nil || m.btw.answer != nil || gen == 0 {
		t.Fatalf("the answer row did not settle: gen %d, md %v, pending %v", row.gen, row.md, m.btw.pending != nil)
	}
	if notes := btwRows(m, entryNote); !slices.Equal(notes, []string{btwHeaderNote, "Grok · 1k in (800 cached) · 4 out"}) {
		t.Fatalf("the notes: %q", notes)
	}
	if asked := stub.Asked(); len(asked) != 1 || asked[0].Question != "what is going on?" || asked[0].Prior != nil {
		t.Fatalf("the session was asked %+v", asked)
	}
	if !slices.Equal(stub.Prompts(), []string{"wait"}) || stub.CancelsSent() != 0 || m.status != statusWorking {
		t.Fatalf("the main turn was touched: prompts %q, cancels %d, %s", stub.Prompts(), stub.CancelsSent(), m.status)
	}
	if paneHas(m.main, "what is going on?") || paneHas(m.main, "You asked") {
		t.Fatal("the side question reached the main pane")
	}
}

// TestASideQuestionReadsTheRawDraft (§3.6): /btw is the raw draft — any case,
// after leading whitespace, the question spanning lines verbatim — and not
// /btwx; an empty one is the usage line, the draft consumed.
func TestASideQuestionReadsTheRawDraft(t *testing.T) {
	m, stub := asideModel(t)
	// A `!` command's output waiting for the main session's next message is
	// the main session's: it is not attached to the side question, and is
	// still waiting after it.
	pending := []agent.ShellResult{{Command: "ls", Output: "a.go"}}
	m.shellCtx = pending
	m, cmd := pressBtw(t, m, "  /BTW  first line\nsecond line  ")
	m = answerBtw(t, m, cmd)
	if asked := stub.Asked(); len(asked) != 1 || asked[0].Question != "first line\nsecond line" {
		t.Fatalf("the session was asked %+v", asked)
	}
	if len(stub.Prompts()) != 0 {
		t.Fatalf("a /btw of two lines went to the agent: %q", stub.Prompts())
	}
	if !slices.Equal(m.shellCtx, pending) {
		t.Fatalf("the pending shell context is %+v after the side question", m.shellCtx)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})

	m, _ = pressBtw(t, m, "/btw")
	if m.btw != nil || m.input.Value() != "" || !paneHas(m.main, btwUsage) {
		t.Fatalf("a bare /btw: view %v, composer %q", m.btw != nil, m.input.Value())
	}
	for _, draft := range []string{"/btwx hi", "say /btw hi"} {
		if q, ok := btwDraft(draft); ok {
			t.Fatalf("%q reads as /btw %q", draft, q)
		}
	}
}

// TestAFollowUpCarriesTheThread (§3.6, §3.2): Enter in the view asks a
// follow-up carrying every earlier pair as prior; Enter while a question is
// in flight does nothing and keeps the text.
func TestAFollowUpCarriesTheThread(t *testing.T) {
	m, stub := asideModel(t)
	m, cmd := pressBtw(t, m, "/btw first")
	m = answerBtw(t, m, cmd)
	first := stubAnswer(agent.AsideRequest{Question: "first"})

	held, release := stub.HoldNextAside()
	defer release()
	m = typeComposer(t, m, "second")
	m, cmd = press(m, enter())
	if m.input.Value() != "" || m.btw.pending == nil {
		t.Fatalf("the follow-up: composer %q, pending %v", m.input.Value(), m.btw.pending != nil)
	}
	ask, wait := mustCmd(t, cmd, "askBtwCmd"), mustCmd(t, cmd, "waitBtw")
	done := make(chan tea.Msg, 1)
	go func() { done <- ask() }()
	awaitStep(t, held, "the follow-up parking")
	m = typeComposer(t, m, "third")
	m, cmd = press(m, enter())
	if findCmd(cmd, "askBtwCmd") != nil || m.input.Value() != "third" {
		t.Fatalf("enter while a question is in flight asked again (composer %q)", m.input.Value())
	}
	release()
	<-done
	m = pumpBtw(t, m, wait)

	asked := stub.Asked()
	want := []agent.AsideTurn{{Question: "first", Answer: first}}
	if len(asked) != 2 || asked[1].Question != "second" || !slices.Equal(asked[1].Prior, want) {
		t.Fatalf("the follow-up was asked %+v", asked)
	}
	if !slices.Equal(btwRows(m, entryUser), []string{"first", "second"}) || len(m.btw.thread) != 2 {
		t.Fatalf("the questions %q, thread %d", btwRows(m, entryUser), len(m.btw.thread))
	}
}

// TestASideViewTakesTwentyOneQuestions (§3.2, §3.6): the 21st question
// carries 20 prior pairs, and the view refuses a 22nd with its row, the
// composer keeping its text.
func TestASideViewTakesTwentyOneQuestions(t *testing.T) {
	m, stub := asideModel(t)
	m, cmd := pressBtw(t, m, "/btw q1")
	m = answerBtw(t, m, cmd)
	for i := 2; i <= btwPriorMax+1; i++ {
		m = typeComposer(t, m, fmt.Sprintf("q%d", i))
		m, cmd = press(m, enter())
		m = answerBtw(t, m, cmd)
	}
	asked := stub.Asked()
	if len(asked) != 21 || len(asked[20].Prior) != btwPriorMax || asked[20].Prior[0].Question != "q1" {
		t.Fatalf("the session was asked %d times, the last with %d prior", len(asked), len(asked[len(asked)-1].Prior))
	}
	m = typeComposer(t, m, "q22")
	m, cmd = press(m, enter())
	if findCmd(cmd, "askBtwCmd") != nil || len(stub.Asked()) != 21 {
		t.Fatal("a 22nd question was asked")
	}
	if errs := btwRows(m, entryError); !slices.Equal(errs, []string{btwFullNote}) || m.input.Value() != "q22" {
		t.Fatalf("the refusal: errors %q, composer %q", errs, m.input.Value())
	}
}

// TestTheSideViewResendsWithinTheWiresBounds (§3.2): a prior answer over
// AsideAnswerMax is resent as its head, cut on a rune boundary and marked,
// within the bound; an encoded request over the line budget drops its oldest
// pairs until it fits; a question that cannot fit on its own is refused.
func TestTheSideViewResendsWithinTheWiresBounds(t *testing.T) {
	long := strings.Repeat("é", 40<<10) // 80 KiB of two-byte runes
	req, ok := btwRequest("now?", []agent.AsideTurn{{Question: "q", Answer: long}, {Question: "r", Answer: "short"}})
	cut := req.Prior[0].Answer
	switch {
	case !ok || len(req.Prior) != 2:
		t.Fatalf("the request: ok %v, %d prior", ok, len(req.Prior))
	case len(cut) > btwAnswerMax || !strings.HasSuffix(cut, btwTruncated) || !utf8.ValidString(cut):
		t.Fatalf("the long answer was resent as %d bytes, marked %v, valid %v", len(cut), strings.HasSuffix(cut, btwTruncated), utf8.ValidString(cut))
	case !strings.HasPrefix(long, strings.TrimSuffix(cut, btwTruncated)):
		t.Fatal("the cut answer is not the answer's head")
	case req.Prior[1].Answer != "short":
		t.Fatalf("a short answer was changed: %q", req.Prior[1].Answer)
	}

	// Each control byte escapes to six (\u0001): twenty of these are well
	// past the line, though each is within its own bound.
	var thread []agent.AsideTurn
	for i := range btwPriorMax {
		thread = append(thread, agent.AsideTurn{Question: fmt.Sprintf("q%d", i), Answer: strings.Repeat("\x01", 60<<10)})
	}
	req, ok = btwRequest("now?", thread)
	if !ok || btwEncodedSize(req) > btwLineBudget || len(req.Prior) == 0 || len(req.Prior) >= btwPriorMax {
		t.Fatalf("the request: ok %v, %d bytes, %d prior", ok, btwEncodedSize(req), len(req.Prior))
	}
	if last := req.Prior[len(req.Prior)-1]; last.Question != fmt.Sprintf("q%d", btwPriorMax-1) {
		t.Fatalf("the newest pair was dropped: the last kept is %q", last.Question)
	}
	if _, ok := btwRequest(strings.Repeat("\x01", 1<<20), nil); ok {
		t.Fatal("a question past the line on its own was not refused")
	}
}

// TestTheSideViewKeepsTheWiresBounds: the TUI's restated bounds are the
// protocol's.
func TestTheSideViewKeepsTheWiresBounds(t *testing.T) {
	if btwAnswerMax != protocol.AsideAnswerMax || btwPriorMax != protocol.AsidePriorMax ||
		btwLineBudget != protocol.InboundLineMax-64<<10 {
		t.Fatalf("the side view's bounds (%d, %d, %d) are not the wire's (%d, %d, %d)",
			btwAnswerMax, btwPriorMax, btwLineBudget, protocol.AsideAnswerMax, protocol.AsidePriorMax, protocol.InboundLineMax-64<<10)
	}
}

// scrolledWorking is asideWorking with the main pane long and scrolled up
// off its bottom: where a return has to bring it back to.
func scrolledWorking(t *testing.T) (Model, *Stub) {
	t.Helper()
	m, stub := asideWorking(t)
	for i := range 100 {
		m.addNote(fmt.Sprintf("main row %d", i))
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyPgUp})
	if m.vp.AtBottom() || m.vp.YOffset == 0 {
		t.Fatalf("fixture: the main pane is not scrolled (offset %d)", m.vp.YOffset)
	}
	return m, stub
}

// TestEscAndCtrlCCloseTheSideView (§7.7): Esc and Ctrl+C each close the view
// — the question in flight cancelled, the composer empty, the main pane back
// at its offset — and touch nothing else: the main turn runs on, nothing is
// cancelled, and Ctrl+C arms no quit (the next press cancels the main turn).
func TestEscAndCtrlCCloseTheSideView(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyEsc, tea.KeyCtrlC} {
		t.Run(tea.KeyMsg{Type: key}.String(), func(t *testing.T) {
			m, stub := scrolledWorking(t)
			off := m.vp.YOffset
			held, release := stub.HoldNextAside()
			defer release()
			m, cmd := pressBtw(t, m, "/btw why?")
			ask, wait := mustCmd(t, cmd, "askBtwCmd"), mustCmd(t, cmd, "waitBtw")
			done := make(chan struct{})
			go func() { ask(); close(done) }()
			awaitStep(t, held, "the aside parking")
			m = typeComposer(t, m, "a follow-up half typed")
			gen := m.composerGen

			m, _ = press(m, tea.KeyMsg{Type: key})
			switch {
			case m.btw != nil || m.cur() != m.main:
				t.Fatal("the side view is still open")
			case m.input.Value() != "":
				t.Fatalf("the composer came back with %q", m.input.Value())
			case m.vp.YOffset != off:
				t.Fatalf("the main pane came back at %d, want %d", m.vp.YOffset, off)
			case m.composerGen == gen:
				t.Fatal("the composer's generation did not move")
			case m.quitting || !m.ctrlCDeadline.IsZero():
				t.Fatalf("quitting %v, ctrl+c armed until %v", m.quitting, m.ctrlCDeadline)
			case m.status != statusWorking || stub.CancelsSent() != 0:
				t.Fatalf("the main turn: %s, %d cancels", m.status, stub.CancelsSent())
			}
			awaitStep(t, done, "the cancelled aside returning")
			if msg := runWatched(t, wait); msg != nil {
				m, _ = press(m, msg)
				if m.btw != nil || paneHas(m.main, "You asked") {
					t.Fatal("the cancelled question's answer landed")
				}
			}
			m, cmd = press(m, tea.KeyMsg{Type: tea.KeyCtrlC})
			if m.quitting {
				t.Fatal("the next ctrl+c quit: the view's close armed the window")
			}
			runCmd(cmd)
			if stub.CancelsSent() != 1 {
				t.Fatalf("the next ctrl+c sent %d cancels, want the main turn's one", stub.CancelsSent())
			}
		})
	}
}

// TestACardOverTheSideView (§3.6, §8): a card arriving over the view takes
// the keyboard — Esc answers the card, not the view, which is there again
// once it is answered — and Ctrl+C with a card up is what it always was: the
// main turn cancelled, the window armed, the view left alone.
func TestACardOverTheSideView(t *testing.T) {
	t.Run("esc", func(t *testing.T) {
		m, stub := asideWorking(t)
		m.yolo = false
		m, cmd := pressBtw(t, m, "/btw why?")
		m = answerBtw(t, m, cmd)
		m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: parityPermission("perm-1")})
		if !m.cardOpen() || !strings.Contains(plainView(m), "Waiting for your answer") {
			t.Fatalf("no card over the view:\n%s", plainView(m))
		}
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
		if m.cardOpen() || m.btw == nil || m.cur() != m.btw.pane {
			t.Fatalf("esc: card %v, view %v", m.cardOpen(), m.btw != nil)
		}
		if !strings.Contains(plainView(m), btwHeaderNote) {
			t.Fatalf("the view is not back:\n%s", plainView(m))
		}
	})
	t.Run("ctrl+c", func(t *testing.T) {
		m, stub := asideWorking(t)
		m.yolo = false
		m, cmd := pressBtw(t, m, "/btw why?")
		m = answerBtw(t, m, cmd)
		m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: parityPermission("perm-2")})
		m, cmd = press(m, tea.KeyMsg{Type: tea.KeyCtrlC})
		runCmd(cmd)
		if m.btw == nil || m.ctrlCDeadline.IsZero() || m.quitting || stub.CancelsSent() != 1 {
			t.Fatalf("ctrl+c with a card: view %v, armed %v, quitting %v, cancels %d",
				m.btw != nil, !m.ctrlCDeadline.IsZero(), m.quitting, stub.CancelsSent())
		}
	})
}

// TestTheMainSessionsRowsStayInTheMainPane (§3.6, §7.7): what the main
// session writes while the view is open — a note, an error, the agent's text
// — lands in the main pane, never the side view's, and is there on return.
func TestTheMainSessionsRowsStayInTheMainPane(t *testing.T) {
	m, stub := asideWorking(t)
	m, cmd := pressBtw(t, m, "/btw why?")
	m = answerBtw(t, m, cmd)
	m.addNote("a main note")
	m.addError("a main error")
	stub.Emit(agent.Event{Type: agent.EventText, Text: "the main turn speaks"})
	m = feed(t, m, stubEventsUntil(t, stub, agent.EventText)...)
	for _, s := range []string{"a main note", "a main error", "the main turn speaks"} {
		if !paneHas(m.main, s) || paneHas(m.btw.pane, s) {
			t.Fatalf("%q: in main %v, in the side view %v", s, paneHas(m.main, s), paneHas(m.btw.pane, s))
		}
	}
	if strings.Contains(plainView(m), "the main turn speaks") {
		t.Fatal("the main pane's row is drawn over the side view")
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if v := plainView(m); !strings.Contains(v, "the main turn speaks") || strings.Contains(v, btwHeaderNote) {
		t.Fatalf("after the return:\n%s", v)
	}
}

// TestAnUnstartedSessionRefusesASideQuestion (§3.6, §7.7): an unstarted
// session whose provider answers side questions refuses /btw with a line and
// spawns nothing; on cursor /btw is the first prompt it always was.
func TestAnUnstartedSessionRefusesASideQuestion(t *testing.T) {
	for _, tc := range []struct {
		p       agent.Provider
		refused bool
	}{{agent.NativeProvider(), true}, {agent.GrokProvider(), true}, {agent.CursorProvider(), false}} {
		t.Run(tc.p.Name(), func(t *testing.T) {
			m, fs, _, _ := openedUnstarted(t, 100, 30)
			u := *m.unstarted
			u.spec.Provider = tc.p
			m.unstarted = &u
			m = typeComposer(t, m, "/btw what now?")
			m, cmd := press(m, enter())
			spawn := findCmd(cmd, "enterUnstarted")
			if tc.refused {
				if spawn != nil || len(fs.spawns) != 0 || !paneHas(m.main, btwUnstarted) || m.input.Value() != "/btw what now?" {
					t.Fatalf("refused: spawn %v, row %v, composer %q", spawn != nil, paneHas(m.main, btwUnstarted), m.input.Value())
				}
				return
			}
			if spawn == nil || paneHas(m.main, btwUnstarted) {
				t.Fatalf("cursor's /btw did not spawn its session as its first prompt")
			}
		})
	}
}

// TestCtrlLOnABtwDraftOpensTheSideView (§3.6): Ctrl+L on a /btw draft opens
// the view as Enter does — on a provider that interjects and one that sends
// now — and never interjects it, asks to send it now, or sends it.
func TestCtrlLOnABtwDraftOpensTheSideView(t *testing.T) {
	for _, interject := range []bool{true, false} {
		t.Run(fmt.Sprint("interject=", interject), func(t *testing.T) {
			isolateSkillsHome(t)
			stub := NewStub()
			if interject {
				stub.SetProvider(agent.GrokProvider())
			}
			m := startSession(t, AsidingStub{Stub: stub}, t.TempDir(), 100, 30)
			if m.caps().Interject != interject {
				t.Fatalf("fixture: interject %v", m.caps().Interject)
			}
			stub.HangNext()
			m.input.SetValue("wait")
			m, _ = press(m, enter())
			waitInTurn(t, stub)
			m.input.SetValue("/btw a side matter")
			m, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlL})
			if m.btw == nil || m.confirm != nil || findCmd(cmd, "askBtwCmd") == nil {
				t.Fatalf("ctrl+l: view %v, confirm %v", m.btw != nil, m.confirm != nil)
			}
			m = answerBtw(t, m, cmd)
			if len(stub.Interjections()) != 0 || !slices.Equal(stub.Prompts(), []string{"wait"}) || stub.CancelsSent() != 0 {
				t.Fatalf("the draft reached the main turn: interjections %q, prompts %q, cancels %d",
					stub.Interjections(), stub.Prompts(), stub.CancelsSent())
			}
			if asked := stub.Asked(); len(asked) != 1 || asked[0].Question != "a side matter" {
				t.Fatalf("the session was asked %+v", asked)
			}
		})
	}
}

// TestAQueueEditNeverSavesASideQuestion (plan 040, C3–C4 review): /btw typed
// over a queued row's text is refused on Enter and on Ctrl+L alike — the row
// keeps its text, the edit stays open, nothing is asked and nothing reaches
// the main session. Negative control: without the refusal in saveQueueEdit
// the row is saved as "/btw why?" and Ctrl+L sends it now.
func TestAQueueEditNeverSavesASideQuestion(t *testing.T) {
	for _, key := range []tea.KeyMsg{enter(), {Type: tea.KeyCtrlL}} {
		t.Run(key.String(), func(t *testing.T) {
			m, stub := asideWorking(t)
			m = typeEnter(t, m, "later")
			rows := queuedRows(m)
			if len(rows) != 1 {
				t.Fatalf("fixture: %d queued", len(rows))
			}
			m.startQueueEdit(rows[0])
			m.input.SetValue("/btw why?")
			m, _ = press(m, key)
			if got := queueTexts(m); len(got) != 1 || got[0] != "later" {
				t.Fatalf("the queued row became %q", got)
			}
			if m.queueEdit == "" || m.btw != nil {
				t.Fatalf("edit open %v, side view %v", m.queueEdit != "", m.btw != nil)
			}
			if len(stub.Asked()) != 0 || !slices.Equal(stub.Prompts(), []string{"wait"}) {
				t.Fatalf("asked %+v, prompts %q", stub.Asked(), stub.Prompts())
			}
			if !paneHas(m.main, btwNotQueued) {
				t.Fatal("no line said why the edit was not saved")
			}
		})
	}
}

// TestASideQuestionWithAnImageIsRefused (§3.6): a /btw draft carrying an
// image chip is refused with a line — side questions are text only — and
// keeps its draft and its image; nothing is asked.
func TestASideQuestionWithAnImageIsRefused(t *testing.T) {
	m, stub := asideModel(t)
	m = typeComposer(t, m, "/btw what is ")
	m = settleImages(t, pasteText(t, m, shotPNG(t)))
	if len(m.images.list) != 1 {
		t.Fatalf("fixture: %d chips", len(m.images.list))
	}
	draft := m.input.Value()
	m, cmd := press(m, enter())
	switch {
	case m.btw != nil || findCmd(cmd, "askBtwCmd") != nil || len(stub.Asked()) != 0:
		t.Fatal("a side question with an image was asked")
	case !paneHas(m.main, btwTextOnly):
		t.Fatal("no line says why")
	case m.input.Value() != draft || len(m.images.list) != 1:
		t.Fatalf("the draft is %q with %d chips, want %q kept", m.input.Value(), len(m.images.list), draft)
	}
}

// TestTheSideComposerMovesItsCursorAcrossLines (§3.6, §8): in the view the
// arrows move the cursor through a draft of many lines, and ↑ on an empty
// composer stays in it — the queue band below is never focused.
func TestTheSideComposerMovesItsCursorAcrossLines(t *testing.T) {
	m, _ := asideWorking(t)
	m.input.SetValue("queued behind")
	m, _ = press(m, enter())
	if len(m.visibleQueue()) != 1 {
		t.Fatalf("fixture: %d queued", len(m.visibleQueue()))
	}
	m, cmd := pressBtw(t, m, "/btw why?")
	m = answerBtw(t, m, cmd)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.queueFocus || m.agentFocus || !m.input.Focused() {
		t.Fatal("↑ on the side view's empty composer left it")
	}
	m = typeComposer(t, m, "first")
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	m = typeComposer(t, m, "second")
	if m.input.Value() != "first\nsecond" || m.input.Line() != 1 {
		t.Fatalf("fixture: %q on line %d", m.input.Value(), m.input.Line())
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Line() != 0 || m.queueFocus || m.btw == nil {
		t.Fatalf("↑ in a draft of two lines: line %d, queue focus %v", m.input.Line(), m.queueFocus)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.input.Line() != 1 {
		t.Fatalf("↓ back: line %d", m.input.Line())
	}
}

// TestAPasteNeverCrossesTheSideView (§3.6, §8): a paste asked for in one of
// the composer's drafts lands in it or nowhere — one asked for in the view
// and answering after Esc, or asked for in the main composer and answering
// once the view is open, is dropped — while one answering in its own draft
// lands, as text alone in the view.
func TestAPasteNeverCrossesTheSideView(t *testing.T) {
	prev := clipboardRead
	clipboardRead = func() (string, error) { return "pasted", nil }
	t.Cleanup(func() { clipboardRead = prev })

	m, _ := asideModel(t)
	m, cmd := pressBtw(t, m, "/btw why?")
	m = answerBtw(t, m, cmd)
	m, paste := press(m, tea.KeyMsg{Type: tea.KeyCtrlV})
	m, _ = press(m, runWatched(t, mustCmd(t, paste, "pasteFromClipboard")))
	if m.input.Value() != "pasted" {
		t.Fatalf("a paste in the view's own draft: %q", m.input.Value())
	}
	m, paste = press(m, tea.KeyMsg{Type: tea.KeyCtrlV})
	late := runWatched(t, mustCmd(t, paste, "pasteFromClipboard"))
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	m, _ = press(m, late)
	if m.input.Value() != "" {
		t.Fatalf("the view's paste landed in the main composer: %q", m.input.Value())
	}

	m, paste = press(m, tea.KeyMsg{Type: tea.KeyCtrlV})
	late = runWatched(t, mustCmd(t, paste, "pasteFromClipboard"))
	m, cmd = pressBtw(t, m, "/btw again?")
	m = answerBtw(t, m, cmd)
	m, _ = press(m, late)
	if m.btw == nil || m.input.Value() != "" {
		t.Fatalf("the main composer's paste landed in the view: %q", m.input.Value())
	}
}

// failingAsider answers side questions with its pieces and then err: the
// failures a Stub's script never has.
type failingAsider struct {
	*Stub
	pieces []string
	err    error
}

func (failingAsider) Asides() bool { return true }

func (f failingAsider) Aside(_ context.Context, _ agent.AsideRequest, onDelta func(string)) (agent.AsideResult, error) {
	for _, p := range f.pieces {
		onDelta(p)
	}
	return agent.AsideResult{}, f.err
}

// TestAFailedSideQuestionSettlesItsRow (§3.6, §8): a failure after part of
// the answer settles the row as streamed — no generation, no checkpoint,
// marked incomplete — before the error row; the view stays open and the
// thread unchanged. A call a reconnect cut says so and is never resent.
func TestAFailedSideQuestionSettlesItsRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pieces []string
		err    error
		row    string
	}{
		{"partial", []string{"half an ", "answer"}, errors.New("the model stopped"), "the model stopped"},
		{"lost", nil, fmt.Errorf("remote: %w", backend.ErrConnectionLost), btwLostNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateSkillsHome(t)
			stub := NewStub()
			m := startSession(t, failingAsider{Stub: stub, pieces: tc.pieces, err: tc.err}, t.TempDir(), 100, 30)
			m, cmd := pressBtw(t, m, "/btw why?")
			m = answerBtw(t, m, cmd)
			if errs := btwRows(m, entryError); !slices.Equal(errs, []string{tc.row}) {
				t.Fatalf("the error rows: %q", errs)
			}
			if m.btw.pending != nil || len(m.btw.thread) != 0 || m.cur() != m.btw.pane {
				t.Fatalf("pending %v, thread %d", m.btw.pending != nil, len(m.btw.thread))
			}
			a := btwRows(m, entryAssistant)
			if tc.pieces == nil {
				if len(a) != 0 {
					t.Fatalf("an answer row with nothing streamed: %q", a)
				}
				return
			}
			row := m.btw.pane.rows[2]
			if !slices.Equal(a, []string{"half an answer\n\n" + btwIncomplete}) || row.gen != 0 || row.md != nil {
				t.Fatalf("the partial row: %q, gen %d, md %v", a, row.gen, row.md)
			}
		})
	}
}

// TestASideViewOpensAgainAfterACancel (§8): a view closed with its question
// in flight opens again on a new question that answers in full, and the
// cancelled question's late message lands nowhere.
func TestASideViewOpensAgainAfterACancel(t *testing.T) {
	m, stub := asideModel(t)
	held, release := stub.HoldNextAside()
	defer release()
	m, cmd := pressBtw(t, m, "/btw first")
	ask := mustCmd(t, cmd, "askBtwCmd")
	old := m.btw.pending.id
	done := make(chan struct{})
	go func() { ask(); close(done) }()
	awaitStep(t, held, "the first parking")
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	awaitStep(t, done, "the cancelled call returning")

	m, cmd = pressBtw(t, m, "/btw second")
	if m.btw.pending.id == old {
		t.Fatal("the new question reused the old one's number")
	}
	m, _ = press(m, btwPumpMsg{id: old, shownGen: m.shownGen, text: "stale", done: &btwDone{}})
	m = answerBtw(t, m, cmd)
	req := agent.AsideRequest{Question: "second"}
	if a := btwRows(m, entryAssistant); !slices.Equal(a, []string{stubAnswer(req)}) {
		t.Fatalf("the answer rows: %q", a)
	}
	if !slices.Equal(btwRows(m, entryUser), []string{"second"}) || len(m.btw.thread) != 1 {
		t.Fatalf("the new view holds %q, thread %d", btwRows(m, entryUser), len(m.btw.thread))
	}
}

// TestASessionSwitchClosesTheSideView (§3.6, §8): opening another session in
// place closes the view first — its question cancelled, its half-typed
// follow-up discarded rather than stashed as the session's draft — and so
// does quitting.
func TestASessionSwitchClosesTheSideView(t *testing.T) {
	open := func(t *testing.T) (Model, chan struct{}) {
		t.Helper()
		m, stub := asideModel(t)
		eng := engineOf(t, m)
		t.Cleanup(func() { _ = eng.Close() })
		held, release := stub.HoldNextAside()
		t.Cleanup(release)
		m, cmd := pressBtw(t, m, "/btw first")
		ask := mustCmd(t, cmd, "askBtwCmd")
		done := make(chan struct{})
		go func() { ask(); close(done) }()
		awaitStep(t, held, "the aside parking")
		return typeComposer(t, m, "half typed"), done
	}
	t.Run("switch", func(t *testing.T) {
		m, done := open(t)
		next, _ := m.switchBackend(newLane(t, "sess-b", "bravo"), false, "")
		if next.btw != nil {
			t.Fatal("the side view survived the switch")
		}
		awaitStep(t, done, "the cancelled call returning")
		for key, d := range next.drafts {
			if strings.Contains(d.text, "half typed") {
				t.Fatalf("the view's follow-up was stashed as %q's draft", key)
			}
		}
	})
	t.Run("quit", func(t *testing.T) {
		m, done := open(t)
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlD})
		if !m.quitting || m.btw != nil {
			t.Fatalf("ctrl+d: quitting %v, view %v", m.quitting, m.btw != nil)
		}
		awaitStep(t, done, "the cancelled call returning")
	})
}

// TestARestoreLeavesTheSideViewOpen (§3.6): a restore — what a reconnect
// brings — rebuilds the main pane underneath and leaves the view, its rows
// and its thread as they were.
func TestARestoreLeavesTheSideViewOpen(t *testing.T) {
	m, _ := asideModel(t)
	m, cmd := pressBtw(t, m, "/btw why?")
	m = answerBtw(t, m, cmd)
	rows := len(m.btw.pane.rows)
	snap, err := m.shared.Snapshot(0)
	if err != nil {
		t.Fatal(err)
	}
	m, _ = press(m, restoreOf(m, snap, 1))
	if m.btw == nil || m.cur() != m.btw.pane || len(m.btw.pane.rows) != rows || len(m.btw.thread) != 1 {
		t.Fatal("the restore closed or emptied the side view")
	}
	if !strings.Contains(plainView(m), btwHeaderNote) {
		t.Fatalf("the restore drew over the side view:\n%s", plainView(m))
	}
}

// TestCtrlYCopiesTheSideAnswer (§3.6): in the view Ctrl+Y copies its own last
// answer, never the main session's last reply.
func TestCtrlYCopiesTheSideAnswer(t *testing.T) {
	rec := captureCopies(t)
	m, _ := asideModel(t)
	m.input.SetValue("hello")
	m, _ = press(m, enter())
	// The stub answers on the session's own goroutine, so the reply is waited
	// for, not drained once: a single applyPending can run before the turn has
	// published it (seen under a loaded full-package run).
	m = pumpUntil(t, m, allOf(isIdle, viewHas("echo: hello")))
	if !paneHas(m.main, "echo: hello") {
		t.Fatal("fixture: no main reply")
	}
	m, cmd := pressBtw(t, m, "/btw why?")
	m = answerBtw(t, m, cmd)
	m, cmd = press(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	runCmd(cmd)
	want := stubAnswer(agent.AsideRequest{Question: "why?"})
	if copies := rec.copies(); len(copies) != 1 || copies[0] != want {
		t.Fatalf("ctrl+y copied %q, want [%q]", copies, want)
	}
}

// TestTheSideViewsChromeIsInert (§3.6): while the view is open the queue
// strip shows no actions and a click on it starts no edit, the status row
// says where the keyboard is in the mode's place and its click cycles
// nothing, and the working line, the composer's rule and its hint are the
// view's.
func TestTheSideViewsChromeIsInert(t *testing.T) {
	m, stub := asideWorking(t)
	m.frozen = true
	m.input.SetValue("queued behind")
	m, _ = press(m, enter())
	held, release := stub.HoldNextAside()
	defer release()
	m, cmd := pressBtw(t, m, "/btw why?")
	ask := mustCmd(t, cmd, "askBtwCmd")
	done := make(chan struct{})
	go func() { ask(); close(done) }()
	awaitStep(t, held, "the aside parking")
	defer func() { release(); <-done }()

	view := plainView(m)
	for _, want := range []string{"Answering · 0s · esc closes", btwStatusChip, " " + btwRuleChip + " ─", btwHint, "#1 queued behind"} {
		if !strings.Contains(view, want) {
			t.Fatalf("the view is missing %q:\n%s", want, view)
		}
	}
	q := m.lay.Region(regionQueue)
	if q.Empty() {
		t.Fatal("fixture: no queue strip")
	}
	m, _ = press(m, tea.MouseMsg{X: 4, Y: q.Top, Button: tea.MouseButtonNone, Action: tea.MouseActionMotion})
	if m.queueHov != noHover() {
		t.Fatalf("the strip shows its actions: %+v", m.queueHov)
	}
	m, _ = press(m, tea.MouseMsg{X: 4, Y: q.Top, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.queueEdit != "" || m.queueFocus || m.btw == nil {
		t.Fatalf("a click on the strip: edit %q, focus %v, view %v", m.queueEdit, m.queueFocus, m.btw != nil)
	}
	_, spans := m.statusRow2(m.lay)
	if spanAt(spans, 0) == spanMode {
		t.Fatal("the status row's first span is still the mode")
	}
	mode := m.snap.CurrentMode
	m, _ = press(m, tea.MouseMsg{X: 0, Y: m.lay.Region(regionStatus).Top + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.modeInFlight != "" || m.snap.CurrentMode != mode {
		t.Fatal("a click on the status row cycled the mode")
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.modeInFlight != "" {
		t.Fatal("shift+tab cycled the mode")
	}
}
