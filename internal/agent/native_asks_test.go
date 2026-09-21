package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/tool/opencode"
)

// The native adapter's asks and todos (plan 023 §3.4, §3.5): the harness's
// three new tools driven through a real session, with the registry, the turn's
// token and the event log all doing what they do in production.
//
// The barriers are native's own (§3.5). An ask's opening is enqueued by the
// registry inside the section that parks it, so a watcher on the session's
// events is the "the ask is open" barrier. A turn's terminal event is the
// "everything this turn published has been published" barrier, and observing it
// through the watcher — which is the primary's only reader — is what makes
// counting events afterwards a fact rather than a race. The continuation's
// return is the "every tool goroutine has been joined" barrier, because Fantasy
// joins them before Run returns and the prompt retires the turn's token before
// it publishes its ending. Registry emptiness is asserted only after one of
// those, never from Cancel's return, which may come back on its own context or
// on the session's done (native.go's Cancel).

// nativeWatcher is ONE reader of the session's events, recording what it saw
// and letting a test wait for an event without guessing at a schedule. It has
// to be a reader rather than a drain at the end: the asker flushes the log's
// outbox around its Wait, and an outbox nobody is draining would hold the tool
// that is asking.
type nativeWatcher struct {
	t *testing.T

	mu   sync.Mutex
	evs  []Event
	bell chan struct{}

	stop chan struct{}
	done chan struct{}
}

func newNativeWatcher(t *testing.T, s *nativeSession) *nativeWatcher {
	t.Helper()
	w := &nativeWatcher{
		t:    t,
		bell: make(chan struct{}, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go func() {
		defer close(w.done)
		for {
			select {
			case ev := <-s.Events():
				w.add(ev)
			case <-w.stop:
				// Whatever is still buffered is the whole of what is left.
				for {
					select {
					case ev := <-s.Events():
						w.add(ev)
					default:
						return
					}
				}
			}
		}
	}()
	t.Cleanup(func() {
		close(w.stop)
		<-w.done
	})
	return w
}

func (w *nativeWatcher) add(ev Event) {
	w.mu.Lock()
	w.evs = append(w.evs, ev)
	w.mu.Unlock()
	select {
	case w.bell <- struct{}{}:
	default:
	}
}

// events is everything recorded so far, in order.
func (w *nativeWatcher) events() []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Event(nil), w.evs...)
}

// wait blocks until an event the predicate accepts has been recorded and
// answers with the first of them, so a test that waits twice for one kind sees
// the same event rather than racing the next.
func (w *nativeWatcher) wait(what string, pred func(Event) bool) Event {
	w.t.Helper()
	deadline := time.After(nativeWait)
	for {
		for _, ev := range w.events() {
			if pred(ev) {
				return ev
			}
		}
		select {
		case <-w.bell:
		case <-deadline:
			w.t.Fatalf("timed out after %v waiting for %s; the session published %s",
				nativeWait, what, strings.Join(w.kinds(), ", "))
		}
	}
}

func (w *nativeWatcher) waitType(typ EventType) Event {
	w.t.Helper()
	return w.wait("an "+string(typ)+" event", func(ev Event) bool { return ev.Type == typ })
}

// waitTerminal blocks until the turn's own ending has been recorded. Every
// event a turn publishes is published before it, on the same goroutine and
// through the same FIFO, so once the watcher has this one it has all of them:
// it is the barrier every count and every ordering assertion below stands on.
func (w *nativeWatcher) waitTerminal() {
	w.t.Helper()
	w.wait("the turn's ending", func(ev Event) bool {
		return ev.Type == EventDone || ev.Type == EventError
	})
}

// kinds is what was published, for a failure message.
func (w *nativeWatcher) kinds() []string {
	var out []string
	for _, ev := range w.events() {
		out = append(out, string(ev.Type))
	}
	return out
}

// count is how many events of one type were recorded.
func (w *nativeWatcher) count(typ EventType) int {
	n := 0
	for _, ev := range w.events() {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// asksOf is every ask ending recorded, in order.
func (w *nativeWatcher) asksOf() []*AskUpdate {
	var out []*AskUpdate
	for _, ev := range w.events() {
		if ev.Type == EventAsk && ev.Ask != nil {
			out = append(out, ev.Ask)
		}
	}
	return out
}

// nativeQuestionArgs is one ask_user_question call's arguments: one question,
// each option a label and a description built from it.
func nativeQuestionArgs(t *testing.T, prompt string, labels ...string) string {
	t.Helper()
	opts := make([]any, 0, len(labels))
	for _, l := range labels {
		opts = append(opts, map[string]any{"label": l, "description": "picking " + l})
	}
	return nativeArgs(t, map[string]any{
		"questions": []any{map[string]any{"question": prompt, "options": opts}},
	})
}

// toolResultsOf is every tool result a request carried, by the provider's call
// id: what the model was actually told each call answered.
func toolResultsOf(call fantasy.Call) map[string]string {
	out := map[string]string{}
	for _, m := range call.Prompt {
		for _, p := range m.Content {
			r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p)
			if !ok {
				continue
			}
			switch o := r.Output.(type) {
			case fantasy.ToolResultOutputContentText:
				out[r.ToolCallID] = o.Text
			case fantasy.ToolResultOutputContentError:
				out[r.ToolCallID] = o.Error.Error()
			}
		}
	}
	return out
}

// answerQuestion answers the ask with one option per question, by index.
func answerQuestion(t *testing.T, s *nativeSession, q *QuestionEvent, picks ...int) {
	t.Helper()
	answers := map[string][]string{}
	for i, one := range q.Questions {
		if i >= len(picks) {
			break
		}
		answers[one.ID] = []string{one.Options[picks[i]].ID}
	}
	if _, err := s.Asks().Answer("test/answer", q.ID, AskAnswer{Answers: answers}); err != nil {
		t.Fatalf("answering %s: %v", q.ID, err)
	}
}

// turnTokenOf is the session's current turn token, read the way the adapter
// reads it.
func turnTokenOf(s *nativeSession) TurnToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnToken
}

// harnessModeOf is the mode the session's harness is running in.
func harnessModeOf(s *nativeSession) string {
	s.mu.Lock()
	hs := s.hs
	s.mu.Unlock()
	return hs.Mode()
}

// writeNativeFile writes a file a test controls, creating its directory.
func writeNativeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireSettled asserts the registry holds nothing open, which is only a fact
// once the turn has settled: every ask is opened on a tool goroutine Fantasy
// joins before Run returns, and the prompt retires the turn's token before it
// publishes its ending.
func requireSettled(t *testing.T, s *nativeSession) {
	t.Helper()
	if open := s.Asks().Asks(); len(open) != 0 {
		t.Fatalf("the registry still holds %d open ask(s) after the turn settled: %+v", len(open), open)
	}
}

// endingsFor is every ending the registry recorded for one ask id: the place
// "exactly one ending" is read from, because it is the registry's own state and
// not an event a reader may not have drained yet.
func endingsFor(s *nativeSession, id string) []AskRecord {
	var out []AskRecord
	for _, rec := range s.Asks().Resolved() {
		if rec.ID == id {
			out = append(out, rec)
		}
	}
	return out
}

// TestNativeQuestionIsOneAskAndTheAnswerReachesTheModel (A3): the tool opens
// one ask through the registry, against the running turn's token; the registry
// publishes the opening and the ending and the adapter publishes neither a
// second time; the answer comes back to the model by index, as opencode's
// answer text.
func TestNativeQuestionIsOneAskAndTheAnswerReachesTheModel(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	m.push(
		nativeCallStep("c1", "ask_user_question", nativeQuestionArgs(t, "Which shall it be?", "alpha", "beta")),
		answer("went with beta"),
	)

	out := startPrompt(s, "ask me")
	q := w.waitType(EventQuestion).Question
	if q == nil || len(q.Questions) != 1 {
		t.Fatalf("the opening carried %+v", q)
	}
	// The registry's own id, the adapter's ids for what it offered, and the
	// turn it belongs to.
	if q.ID != "ask-1" || q.Questions[0].ID != "q1" || q.Questions[0].Options[1].ID != "o2" {
		t.Fatalf("ids: %+v", q.Questions[0])
	}
	if q.Title != "Which shall it be?" || q.Auto {
		t.Fatalf("title %q auto %v", q.Title, q.Auto)
	}
	if got := q.Questions[0].Options[1]; got.Label != "beta" || got.Description != "picking beta" {
		t.Fatalf("option: %+v", got)
	}
	rec, ok := s.Asks().Record(q.ID)
	if !ok || rec.Status != AskOpen || rec.Token != turnTokenOf(s) {
		t.Fatalf("record %+v, want an open ask on the running turn's token", rec)
	}

	answerQuestion(t, s, q, 1)
	got := await(t, out, "the prompt")
	if got.err != nil || got.res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want end_turn", got.res, got.err)
	}
	requireSettled(t, s)
	w.waitTerminal()

	// The ending is the registry's, and there is exactly one opening and one
	// ending: an adapter that emitted for itself would double them.
	ends := w.asksOf()
	if len(ends) != 1 || ends[0].ID != q.ID || ends[0].Outcome != AskAnswered {
		t.Fatalf("ask endings: %+v", ends)
	}
	if n := w.count(EventQuestion); n != 1 {
		t.Fatalf("%d question openings, want one", n)
	}

	// By index, in the model's own words: the second option's label, not the
	// id the card answered with.
	reqs := m.requests()
	if len(reqs) != 2 {
		t.Fatalf("the model saw %d requests, want 2", len(reqs))
	}
	text := toolResultsOf(reqs[1])["c1"]
	if !strings.Contains(text, `"Which shall it be?"="beta"`) {
		t.Fatalf("the model was told %q", text)
	}
}

// TestNativeSkippedQuestionIsUnanswered: a skip — what the card's Esc sends —
// is the person declining the whole request, so the model reads grok-build's
// unanswered text and the turn carries on.
func TestNativeSkippedQuestionIsUnanswered(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	m.push(
		nativeCallStep("c1", "ask_user_question", nativeQuestionArgs(t, "Which shall it be?", "alpha", "beta")),
		answer("asked and got nothing"),
	)

	out := startPrompt(s, "ask me")
	q := w.waitType(EventQuestion).Question
	if _, err := s.Asks().Answer("test/skip", q.ID, AskAnswer{Skip: true}); err != nil {
		t.Fatalf("skipping: %v", err)
	}
	got := await(t, out, "the prompt")
	if got.err != nil || got.res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want end_turn", got.res, got.err)
	}
	if text := toolResultsOf(m.requests()[1])["c1"]; text != opencode.UnansweredText {
		t.Fatalf("the model was told %q, want the unanswered text", text)
	}
}

// TestNativeHeadlessAnswersItsOwnAsks (A6, plan 023 §3.4): with craze's
// headless policy no card is ever raised — the registry writes the Auto opening
// and its automatic ending together — and the model is told the first option,
// exactly as it is for cursor.
func TestNativeHeadlessAnswersItsOwnAsks(t *testing.T) {
	f := newNativeFixture(t)
	// Options{} is the headless policy: no frontend, so a question takes its
	// first option and a plan is accepted (EffectiveApproval).
	s := f.started(Options{})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	m.push(
		nativeCallStep("c1", "ask_user_question", nativeQuestionArgs(t, "Which shall it be?", "alpha", "beta")),
		answer("took the first"),
	)

	got := await(t, startPrompt(s, "ask me"), "the prompt")
	if got.err != nil || got.res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want end_turn", got.res, got.err)
	}
	w.waitTerminal()
	opening := w.waitType(EventQuestion).Question
	if !opening.Auto || len(opening.Answers["q1"]) != 1 || opening.Answers["q1"][0] != "o1" {
		t.Fatalf("the opening is %+v, want an Auto one carrying the first option", opening)
	}
	ends := w.asksOf()
	if len(ends) != 1 || ends[0].Outcome != AskAutomatic || ends[0].By != AskByPolicy {
		t.Fatalf("ask endings: %+v", ends)
	}
	if text := toolResultsOf(m.requests()[1])["c1"]; !strings.Contains(text, `="alpha"`) {
		t.Fatalf("the model was told %q, want the first option", text)
	}
	requireSettled(t, s)
}

// TestNativeTwoAsksInOneStepSerialise (§3.5): ask_user_question is not
// Parallel, so Fantasy runs a step's two calls one after the other on its
// coordinator — the second opens only once the first has been answered, and
// each result reaches the model against its own call id.
func TestNativeTwoAsksInOneStepSerialise(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	m.push(
		reply(
			nativeCallParts("c1", "ask_user_question", nativeQuestionArgs(t, "First?", "one", "two")),
			nativeCallParts("c2", "ask_user_question", nativeQuestionArgs(t, "Second?", "three", "four")),
			finishParts(fantasy.FinishReasonToolCalls),
		),
		answer("both answered"),
	)

	out := startPrompt(s, "two of them")
	first := w.wait("the first question", func(ev Event) bool {
		return ev.Type == EventQuestion && ev.Question.Title == "First?"
	}).Question
	// The second has not been asked yet: one card at a time.
	if n := w.count(EventQuestion); n != 1 {
		t.Fatalf("%d openings while the first is unanswered, want one", n)
	}
	answerQuestion(t, s, first, 0)
	second := w.wait("the second question", func(ev Event) bool {
		return ev.Type == EventQuestion && ev.Question.Title == "Second?"
	}).Question
	answerQuestion(t, s, second, 1)

	got := await(t, out, "the prompt")
	if got.err != nil || got.res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want end_turn", got.res, got.err)
	}
	results := toolResultsOf(m.requests()[1])
	if !strings.Contains(results["c1"], `="one"`) || !strings.Contains(results["c2"], `="four"`) {
		t.Fatalf("the model was told %q and %q", results["c1"], results["c2"])
	}
	w.waitTerminal()
	// The record's own order says it: the second was opened after the first
	// had ended.
	var order []string
	for _, ev := range w.events() {
		switch {
		case ev.Type == EventQuestion:
			order = append(order, "open "+ev.Question.Title)
		case ev.Type == EventAsk && ev.Ask != nil:
			order = append(order, "end "+ev.Ask.ID)
		}
	}
	want := []string{"open First?", "end ask-1", "open Second?", "end ask-2"}
	if strings.Join(order, " | ") != strings.Join(want, " | ") {
		t.Fatalf("the record's order is %v, want %v", order, want)
	}
	requireSettled(t, s)
}

// TestNativeCancelWhileAnAskIsOpen (§3.5): a cancel ends the ask, ends the turn
// cancelled, leaves the registry empty once the turn has settled, and leaves
// the session able to run the next prompt.
func TestNativeCancelWhileAnAskIsOpen(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	// One step: the turn is cancelled while the tool is asking, so it never
	// comes back for another.
	m.push(nativeCallStep("c1", "ask_user_question", nativeQuestionArgs(t, "Which shall it be?", "alpha", "beta")))

	out := startPrompt(s, "ask me")
	q := w.waitType(EventQuestion).Question
	if _, err := s.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got := await(t, out, "the cancelled prompt")
	if got.err != nil || got.res.StopReason != harness.StopCancelled {
		t.Fatalf("Prompt = %+v, %v; want a cancelled turn", got.res, got.err)
	}
	// Settled: the continuation has returned, so every tool goroutine has been
	// joined and the token retired.
	requireSettled(t, s)
	rec, ok := s.Asks().Record(q.ID)
	if !ok || rec.Status != AskResolved || rec.Outcome != AskCancelled {
		t.Fatalf("the ask ended %+v, want cancelled", rec)
	}
	if n := len(endingsFor(s, q.ID)); n != 1 {
		t.Fatalf("%d endings for one ask, want exactly one", n)
	}
	if n := len(m.requests()); n != 1 {
		t.Fatalf("the model saw %d requests; a cancelled ask must not be followed by one", n)
	}

	// The session still works: the next prompt runs.
	m.push(answer("still here"))
	next := await(t, startPrompt(s, "again"), "the next prompt")
	if next.err != nil || next.res.StopReason != harness.StopEndTurn {
		t.Fatalf("the next prompt = %+v, %v", next.res, next.err)
	}
}

// TestNativeCancelOfOneTurnDoesNotDrainTheNext (§3.5, panel correction 5):
// CancelTurn drains EVERY open ask, of whatever turn, so the cancel reads the
// running turn and calls the registry in ONE critical section under s.mu. A
// cancel that read the turn and called the registry after releasing the lock
// could answer the next turn's card in the window between the two; there is no
// such window, and the next turn's ask is answered by the person, on its own
// token.
func TestNativeCancelOfOneTurnDoesNotDrainTheNext(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	// One step only: the cancel lands while the tool is asking, so the turn
	// never comes back for a second, and a step left queued would be taken by
	// turn B below.
	m.push(nativeCallStep("c1", "ask_user_question", nativeQuestionArgs(t, "Turn A?", "alpha", "beta")))

	out := startPrompt(s, "turn A")
	first := w.waitType(EventQuestion).Question
	tokenA := turnTokenOf(s)
	if _, err := s.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := await(t, out, "turn A"); got.res.StopReason != harness.StopCancelled {
		t.Fatalf("turn A = %+v, %v", got.res, got.err)
	}

	m.push(
		nativeCallStep("c2", "ask_user_question", nativeQuestionArgs(t, "Turn B?", "gamma", "delta")),
		answer("B answered"),
	)
	outB := startPrompt(s, "turn B")
	second := w.wait("turn B's question", func(ev Event) bool {
		return ev.Type == EventQuestion && ev.Question.Title == "Turn B?"
	}).Question
	tokenB := turnTokenOf(s)
	if tokenA == tokenB {
		t.Fatalf("both turns minted %v; every turn is its own token", tokenA)
	}
	answerQuestion(t, s, second, 0)
	if got := await(t, outB, "turn B"); got.err != nil || got.res.StopReason != harness.StopEndTurn {
		t.Fatalf("turn B = %+v, %v; want end_turn", got.res, got.err)
	}
	requireSettled(t, s)

	recA, _ := s.Asks().Record(first.ID)
	recB, _ := s.Asks().Record(second.ID)
	if recA.Outcome != AskCancelled || recA.By != AskByCancel || recA.Token != tokenA {
		t.Fatalf("turn A's ask ended %+v", recA)
	}
	if recB.Outcome != AskAnswered || recB.By != AskByClient || recB.Token != tokenB {
		t.Fatalf("turn B's ask ended %+v; the cancel of A must not reach it", recB)
	}
	if text := toolResultsOf(m.requests()[2])["c2"]; !strings.Contains(text, `="gamma"`) {
		t.Fatalf("turn B's model was told %q", text)
	}
}

// TestNativeCloseWhileAnAskIsOpen (§3.5, correction 20): Close cancels the
// turn's context before it closes the registry, so the tool that was asking is
// cut short and no request follows it; the ask ends exactly once, and the flush
// the asker makes around its Wait does not wedge the close (X42's lesson: a
// session's flush ends with the session).
func TestNativeCloseWhileAnAskIsOpen(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	// One step: Close cuts the turn short while the tool is asking.
	m.push(nativeCallStep("c1", "ask_user_question", nativeQuestionArgs(t, "Which shall it be?", "alpha", "beta")))

	out := startPrompt(s, "ask me")
	q := w.waitType(EventQuestion).Question
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	if err := await(t, closed, "Close"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := await(t, out, "the prompt")
	if got.err == nil && got.res.StopReason != harness.StopCancelled {
		t.Fatalf("Prompt = %+v, %v; want the turn cut short", got.res, got.err)
	}
	if n := len(m.requests()); n != 1 {
		t.Fatalf("the model saw %d requests; nothing may follow a step whose ask was closed", n)
	}
	requireSettled(t, s)
	rec, ok := s.Asks().Record(q.ID)
	// Two outcomes are legal here and both are one ending: Close cancels the
	// turn's context first, so Ask.Wait's own watch usually resolves the ask
	// "cancelled by call" before the registry's Close reaches it, and the
	// registry's wins when it gets there first (native.go's Close says which
	// order and why).
	if !ok || rec.Status != AskResolved || (rec.Outcome != AskCancelled && rec.Outcome != AskClosing) {
		t.Fatalf("the ask ended %+v, want cancelled or closing", rec)
	}
	if n := len(endingsFor(s, q.ID)); n != 1 {
		t.Fatalf("%d endings for one ask, want exactly one", n)
	}
}

// TestNativeAskEndsWithItsTurnWithoutCancellingTheCall (§3.5): a turn retired
// underneath a parked ask resolves it turn_ended and touches no tool context —
// the tool reads the record, answers with the unanswered text and returns.
//
// The prompt path cannot produce this, which is why it is driven at the seam:
// every ask is opened on a Fantasy tool goroutine the turn joins before Run
// returns, so by the time prompt() retires the token no ask of its own can
// still be open. What is under test is the adapter's reading of the outcome,
// which is what any later caller of EndTurn would meet.
func TestNativeAskEndsWithItsTurnWithoutCancellingTheCall(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("working"), finishParts(fantasy.FinishReasonStop)))

	out := startPrompt(s, "hold the turn open")
	await(t, h.reached, "the held step")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asked := make(chan tool.Answers, 1)
	go func() {
		ans, _ := nativeAsker{s: s}.AskQuestion(ctx, []tool.Question{{
			Question: "Which shall it be?",
			Options:  []tool.QuestionOption{{Label: "alpha"}, {Label: "beta"}},
		}})
		asked <- ans
	}()
	q := w.waitType(EventQuestion).Question
	s.asks.EndTurn(turnTokenOf(s))

	ans := await(t, asked, "the ask to come back")
	if ans.Answered || ans.Picked != nil {
		t.Fatalf("the asker answered %+v, want nobody answered", ans)
	}
	if ctx.Err() != nil {
		t.Fatalf("the call's context was cancelled by the turn's end: %v", ctx.Err())
	}
	rec, _ := s.Asks().Record(q.ID)
	if rec.Outcome != AskTurnEnded || rec.By != AskByTurn {
		t.Fatalf("the ask ended %+v, want turn_ended", rec)
	}
	close(h.release)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatalf("Prompt: %v", got.err)
	}
	requireSettled(t, s)
}

// TestNativeInterjectWhileAnAskIsOpenLandsAfterTheAnswer (§3.5): a steer taken
// while the card is up is merged at the next PrepareStep, which runs only once
// the tool has returned — so the model reads the answer and the interjection in
// the step after it.
func TestNativeInterjectWhileAnAskIsOpenLandsAfterTheAnswer(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	m.push(
		nativeCallStep("c1", "ask_user_question", nativeQuestionArgs(t, "Which shall it be?", "alpha", "beta")),
		answer("read them both"),
	)

	out := startPrompt(s, "ask me")
	q := w.waitType(EventQuestion).Question
	if err := s.Interject(context.Background(), "and hurry"); err != nil {
		t.Fatalf("Interject while the card is up: %v", err)
	}
	answerQuestion(t, s, q, 0)
	got := await(t, out, "the prompt")
	if got.err != nil || got.res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want end_turn", got.res, got.err)
	}

	second := m.requests()[1].Prompt
	if text := userTextOf(second[len(second)-1]); text != "and hurry" {
		t.Fatalf("the step after the answer ends with %q, want the interjection", text)
	}
	if text := toolResultsOf(m.requests()[1])["c1"]; !strings.Contains(text, `="alpha"`) {
		t.Fatalf("the same step carried %q as the answer", text)
	}
	requireSettled(t, s)
}

// nativeNasty is every control byte §7 A3 names and a provider key, for a test
// that puts them wherever a model can put text.
var nativeNasty = "\x1b[31mred\x1b]0;title\x07\x07\x00 and " + nativeCanary + zeroWidthSpace

// TestNativeQuestionBodyIsRedactedAndSanitized (A3, panel correction 11):
// nothing a model writes into a question reaches the registry, the snapshot or
// an event raw — not a provider key, and not an escape sequence. The tools
// redact with the harness's own keys before the asker sees anything (X9); this
// adapter sanitizes. Both halves are asserted, over every string of the body.
func TestNativeQuestionBodyIsRedactedAndSanitized(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	f.models["test/a"].push(
		nativeCallStep("c1", "ask_user_question", nativeArgs(t, map[string]any{
			"questions": []any{map[string]any{
				"question": "Which? " + nativeNasty,
				"header":   "head " + nativeNasty,
				"options": []any{map[string]any{
					"label":       "alpha\nsecond line " + nativeNasty,
					"description": "means " + nativeNasty,
				}},
			}},
		})),
		answer("done"),
	)

	out := startPrompt(s, "ask me")
	q := w.waitType(EventQuestion).Question
	answerQuestion(t, s, q, 0)
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatalf("Prompt: %v", got.err)
	}
	w.waitTerminal()

	one := q.Questions[0]
	for what, got := range map[string]string{
		"the title":           q.Title,
		"the prompt":          one.Prompt,
		"the label":           one.Options[0].Label,
		"the description":     one.Options[0].Description,
		"what an ending said": endingText(w),
	} {
		if strings.Contains(got, nativeCanary) {
			t.Fatalf("%s carries the canary key: %q", what, got)
		}
		if strings.ContainsAny(got, "\x1b\x07\x00") || strings.Contains(got, zeroWidthSpace) {
			t.Fatalf("%s carries a control byte: %q", what, got)
		}
	}
	// A label is one line: the card draws it beside a number.
	if strings.Contains(one.Options[0].Label, "\n") || strings.Contains(q.Title, "\n") {
		t.Fatalf("a label kept its newline: %q / %q", one.Options[0].Label, q.Title)
	}
	if !strings.Contains(one.Options[0].Label, "second line") {
		t.Fatalf("the label lost its text: %q", one.Options[0].Label)
	}
}

// endingText is every string the recorded endings carried, joined: an ending
// that publishes a body — one whose opening never was — must be as clean as an
// opening.
func endingText(w *nativeWatcher) string {
	var b strings.Builder
	for _, u := range w.asksOf() {
		b.WriteString(u.Label)
		if u.Body == nil {
			continue
		}
		if q := u.Body.Question; q != nil {
			b.WriteString(q.Title)
			for _, one := range q.Questions {
				b.WriteString(one.Prompt)
				for _, o := range one.Options {
					b.WriteString(o.Label)
					b.WriteString(o.Description)
				}
			}
		}
		if p := u.Body.Plan; p != nil {
			b.WriteString(p.Name)
			b.WriteString(p.Plan)
		}
	}
	return b.String()
}

// TestNativeExitPlanModeIsDeniedWhileModesAreOff is plan 023 §5's interim,
// exactly: the three tools are advertised and the cards work, but no mode can
// be entered until PR 2, so the gate refuses exit_plan_mode by name wherever it
// is called. The model reads grok-build's text and no ask is opened.
func TestNativeExitPlanModeIsDeniedWhileModesAreOff(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	m.push(
		nativeCallStep("c1", "exit_plan_mode", "{}"),
		answer("told off"),
	)

	got := await(t, startPrompt(s, "present a plan"), "the prompt")
	if got.err != nil || got.res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want end_turn", got.res, got.err)
	}
	if text := toolResultsOf(m.requests()[1])["c1"]; !strings.Contains(text, "Plan mode has been disabled") {
		t.Fatalf("the model was told %q, want the gate's refusal", text)
	}
	w.waitTerminal()
	if n := w.count(EventPlan); n != 0 {
		t.Fatalf("%d plan openings; a denied call opens no ask", n)
	}
	if got := s.Asks().Resolved(); len(got) != 0 {
		t.Fatalf("the registry recorded %+v", got)
	}
	// SetMode is still the refusal PR 2 removes, and the capability that draws
	// the chip is still off.
	if err := s.SetMode(context.Background(), "plan"); err != ErrUnsupported {
		t.Fatalf("SetMode = %v, want ErrUnsupported until PR 2", err)
	}
	if s.Snapshot().Provider.Capabilities().Modes {
		t.Fatal("Modes is on; PR 1 leaves it off (plan 023 §5)")
	}
}

// planModeFixture is a native session whose HARNESS runs in plan mode, through
// NewNative's own options seam. The adapter still refuses agent.Options.Mode
// and still reports no Modes capability — that is PR 2's work — so this is the
// one way to reach exit_plan_mode's behaviour from here, and it is the same
// session, gate, tools and registry a mode switch will give it.
// The plan file is the path it answers with. A session opened in plan mode
// creates it there and then (harness.Open), before any turn has written a
// transcript, so it is found on disk rather than derived from one.
func planModeFixture(t *testing.T, opts Options) (*nativeFixture, *nativeSession, string) {
	t.Helper()
	f := newNativeFixture(t)
	f.edit = func(o *harness.Options) { o.Mode = "plan" }
	s := f.started(opts)
	plans, err := filepath.Glob(filepath.Join(f.dir, "sessions", "*", "*"+planSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("%d plan files under %s, want the one a plan-mode session creates", len(plans), f.dir)
	}
	return f, s, plans[0]
}

// planSuffix is how store.PlanPath spells a transcript's plan file, taken from
// the function itself so the glob above cannot drift from it.
var planSuffix = strings.TrimPrefix(store.PlanPath("x.jsonl"), "x")

// TestNativePlanApprovedEndsTheTurnAfterItsEnding (A4, §3.5): the plan is
// offered as an ask, approving it ends the turn end_turn, and the turn's
// EventDone comes after the ask's ending in the record — the flush before the
// terminal event is what puts it there.
func TestNativePlanApprovedEndsTheTurnAfterItsEnding(t *testing.T) {
	f, s, plan := planModeFixture(t, Options{Interactive: true})
	w := newNativeWatcher(t, s)
	writeNativeFile(t, plan, "# Ship it\n\n1. do the thing\n")
	m := f.models["test/a"]
	m.push(nativeCallStep("c1", "exit_plan_mode", "{}"), answer("never requested"))

	out := startPrompt(s, "plan it")
	p := w.waitType(EventPlan).Plan
	if p.ID != "plan-1" || p.Name != "Ship it" || !strings.Contains(p.Plan, "do the thing") {
		t.Fatalf("the plan opening is %+v", p)
	}
	if _, err := s.Asks().Answer("test/accept", p.ID, AskAnswer{Accept: true}); err != nil {
		t.Fatalf("accepting: %v", err)
	}
	got := await(t, out, "the prompt")
	if got.err != nil || got.res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want end_turn", got.res, got.err)
	}
	if n := len(m.requests()); n != 1 {
		t.Fatalf("the model saw %d requests; an approved plan ends the turn", n)
	}
	// The mode is the person's to change, and approving does not (D-51).
	if mode := harnessModeOf(s); mode != "plan" {
		t.Fatalf("the session is in %q mode after an approved plan", mode)
	}
	w.waitTerminal()

	// The ask's ending is in the record before the turn's own.
	endedAt, doneAt := -1, -1
	for i, ev := range w.events() {
		switch {
		case ev.Type == EventAsk && ev.Ask != nil && ev.Ask.ID == p.ID:
			endedAt = i
		case ev.Type == EventDone:
			doneAt = i
		}
	}
	if endedAt < 0 || doneAt < 0 || endedAt > doneAt {
		t.Fatalf("the ask ended at %d and the turn at %d; the ending must precede the EventDone", endedAt, doneAt)
	}
	if u := w.asksOf(); len(u) != 1 || !u[0].Accepted {
		t.Fatalf("ask endings: %+v", u)
	}
	requireSettled(t, s)
}

// TestNativeApprovedPlanVetoesTheRestOfItsStep (A4, §3.4): the model cannot
// ask the person something the turn will never hear the answer to. Every call
// of the step that has not started when the approval is recorded is refused,
// so [exit_plan_mode, ask_user_question] raises one card and not two, and the
// refused call's row says why.
func TestNativeApprovedPlanVetoesTheRestOfItsStep(t *testing.T) {
	f, s, plan := planModeFixture(t, Options{Interactive: true})
	w := newNativeWatcher(t, s)
	writeNativeFile(t, plan, "# Ship it\n\n1. do the thing\n")
	f.models["test/a"].push(reply(
		nativeCallParts("c1", "exit_plan_mode", "{}"),
		nativeCallParts("c2", "ask_user_question", nativeQuestionArgs(t, "And then?", "one", "two")),
		finishParts(fantasy.FinishReasonToolCalls),
	))

	out := startPrompt(s, "plan it")
	p := w.waitType(EventPlan).Plan
	if _, err := s.Asks().Answer("test/accept", p.ID, AskAnswer{Accept: true}); err != nil {
		t.Fatalf("accepting: %v", err)
	}
	got := await(t, out, "the prompt")
	if got.err != nil || got.res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want end_turn", got.res, got.err)
	}
	w.waitTerminal()
	if n := w.count(EventQuestion); n != 0 {
		t.Fatalf("%d question openings; the vetoed call never ran", n)
	}
	rows := rowsOf(w.events())
	if len(rows) != 2 {
		t.Fatalf("%d tool rows, want one per call: %+v", len(rows), rows)
	}
	vetoed := rows[1]
	if vetoed.ToolName != "ask_user_question" || vetoed.Status != toolFailed {
		t.Fatalf("the second row is %+v, want a failed ask_user_question", vetoed)
	}
	if vetoed.Output == nil || !strings.Contains(vetoed.Output.Content, "the plan was approved") {
		t.Fatalf("the vetoed row shows %+v", vetoed.Output)
	}
	requireSettled(t, s)
}

// TestNativePlanRejectedContinuesTheTurn (A4): a rejected plan ends nothing —
// the model is told to ask what to change and the turn goes on.
func TestNativePlanRejectedContinuesTheTurn(t *testing.T) {
	f, s, plan := planModeFixture(t, Options{Interactive: true})
	w := newNativeWatcher(t, s)
	writeNativeFile(t, plan, "# Ship it\n\n1. do the thing\n")
	m := f.models["test/a"]
	m.push(nativeCallStep("c1", "exit_plan_mode", "{}"), answer("what should I change?"))

	out := startPrompt(s, "plan it")
	p := w.waitType(EventPlan).Plan
	if _, err := s.Asks().Answer("test/reject", p.ID, AskAnswer{Reject: true}); err != nil {
		t.Fatalf("rejecting: %v", err)
	}
	got := await(t, out, "the prompt")
	if got.err != nil || got.res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want the turn to carry on and end", got.res, got.err)
	}
	if n := len(m.requests()); n != 2 {
		t.Fatalf("the model saw %d requests; a rejected plan continues the turn", n)
	}
	if text := toolResultsOf(m.requests()[1])["c1"]; text != opencode.PlanRejectedText {
		t.Fatalf("the model was told %q, want the revise text", text)
	}
	if mode := harnessModeOf(s); mode != "plan" {
		t.Fatalf("the session is in %q mode; rejecting a plan does not change it", mode)
	}
	requireSettled(t, s)
}

// TestNativePlanHeadlessIsAccepted (A6): under craze's headless policy the plan
// is accepted without a card, through the registry's Automatic, and the turn
// ends there just as an answered one does.
func TestNativePlanHeadlessIsAccepted(t *testing.T) {
	f, s, plan := planModeFixture(t, Options{})
	w := newNativeWatcher(t, s)
	writeNativeFile(t, plan, "# Ship it\n\n1. do the thing\n")
	m := f.models["test/a"]
	m.push(nativeCallStep("c1", "exit_plan_mode", "{}"), answer("never requested"))

	got := await(t, startPrompt(s, "plan it"), "the prompt")
	if got.err != nil || got.res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want end_turn", got.res, got.err)
	}
	if n := len(m.requests()); n != 1 {
		t.Fatalf("the model saw %d requests", n)
	}
	w.waitTerminal()
	p := w.waitType(EventPlan).Plan
	if !p.Auto || !p.Accepted {
		t.Fatalf("the opening is %+v, want an accepted Auto plan", p)
	}
	ends := w.asksOf()
	if len(ends) != 1 || ends[0].Outcome != AskAutomatic || !ends[0].Accepted {
		t.Fatalf("ask endings: %+v", ends)
	}
	requireSettled(t, s)
}

// TestNativePlanBodyIsRedactedAndSanitized (A4, correction 11): the plan is the
// model's own writing about files it has read, so it goes through both hands
// like a question's strings — the heading the card shows included.
func TestNativePlanBodyIsRedactedAndSanitized(t *testing.T) {
	f, s, plan := planModeFixture(t, Options{Interactive: true})
	w := newNativeWatcher(t, s)
	writeNativeFile(t, plan, "# Ship \x1b[31mit\x07 "+nativeCanary+"\n\nthe key is "+nativeCanary+"\x00\n")
	m := f.models["test/a"]
	m.push(nativeCallStep("c1", "exit_plan_mode", "{}"), answer("never requested"))

	out := startPrompt(s, "plan it")
	p := w.waitType(EventPlan).Plan
	if _, err := s.Asks().Answer("test/accept", p.ID, AskAnswer{Accept: true}); err != nil {
		t.Fatalf("accepting: %v", err)
	}
	if got := await(t, out, "the prompt"); got.err != nil {
		t.Fatalf("Prompt: %v", got.err)
	}
	for what, got := range map[string]string{"the name": p.Name, "the plan": p.Plan} {
		if strings.Contains(got, nativeCanary) {
			t.Fatalf("%s carries the canary key: %q", what, got)
		}
		if strings.ContainsAny(got, "\x1b\x07\x00") {
			t.Fatalf("%s carries a control byte: %q", what, got)
		}
	}
	if !strings.HasPrefix(p.Name, "Ship it") {
		t.Fatalf("the name lost its heading: %q", p.Name)
	}
	// The plan keeps its shape: it is a document, not a label.
	if !strings.Contains(p.Plan, "\n") {
		t.Fatalf("the plan was folded onto one line: %q", p.Plan)
	}
}

// TestNativePlanNameIsTheFirstHeading: the card's word for a plan, and the
// fallback for one that has no heading of its own.
func TestNativePlanNameIsTheFirstHeading(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"no heading", "just prose\nand more of it\n", "Plan"},
		{"empty heading", "#\n\nprose\n", "Plan"},
		{"empty text", "", "Plan"},
		{"first line", "# Ship it\n\nprose\n", "Ship it"},
		{"later heading", "prose first\n\n## Second level\n", "Second level"},
		{"indented", "   ## Indented\n", "Indented"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := nativePlanName(c.text); got != c.want {
				t.Fatalf("nativePlanName = %q, want %q", got, c.want)
			}
		})
	}
	long := "# " + strings.Repeat("x", 400)
	if got := nativePlanName(long); len(got) > nativePlanNameBytes {
		t.Fatalf("nativePlanName kept %d bytes, want at most %d", len(got), nativePlanNameBytes)
	}
}

// TestNativeTodosReachTheSnapshotThenTheEvent (A5, plan 023 §3.4): the snapshot
// is written before the event goes out — a consumer that has the event has the
// snapshot — and a list cleared to nothing clears the snapshot, which is the
// only reason the TUI's panel empties, since it falls back to the snapshot when
// an event's list is empty.
func TestNativeTodosReachTheSnapshotThenTheEvent(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	m := f.models["test/a"]
	m.push(
		nativeCallStep("c1", "todo_write", nativeArgs(t, map[string]any{
			"todos": []any{
				map[string]any{"id": "1", "content": "first", "status": "in_progress"},
				map[string]any{"id": "2", "content": "second"},
			},
		})),
		answer("todos written"),
	)

	out := startPrompt(s, "track it")
	ev := w.waitType(EventTodos)
	// The event is published after the snapshot is written and on the same
	// goroutine, so a consumer holding one holds the other.
	snap := s.Snapshot().Todos
	if len(snap) != 2 || snap[0] != (Todo{ID: "1", Content: "first", Status: "in_progress"}) {
		t.Fatalf("the snapshot holds %+v when the event was published", snap)
	}
	if len(ev.Todos) != 2 || ev.Todos[1] != (Todo{ID: "2", Content: "second", Status: "pending"}) {
		t.Fatalf("the event carried %+v; a todo with no status is pending", ev.Todos)
	}
	if s.Snapshot().TodosUpdatedAt.IsZero() {
		t.Fatal("TodosUpdatedAt was never stamped")
	}
	got := await(t, out, "the prompt")
	if got.err != nil || got.res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want end_turn", got.res, got.err)
	}
	// The model reads its own list back, which is how it patches a row later.
	if text := toolResultsOf(m.requests()[1])["c1"]; !strings.Contains(text, `"first"`) {
		t.Fatalf("the model was told %q", text)
	}

	// A second turn clears it: the event carries nothing and the snapshot is
	// empty, which is what empties the panel.
	m.push(
		nativeCallStep("c2", "todo_write", nativeArgs(t, map[string]any{"merge": false, "todos": []any{}})),
		answer("cleared"),
	)
	cleared := await(t, startPrompt(s, "clear it"), "the second prompt")
	if cleared.err != nil {
		t.Fatalf("the second prompt: %v", cleared.err)
	}
	w.waitTerminal()
	var lists [][]Todo
	for _, e := range w.events() {
		if e.Type == EventTodos {
			lists = append(lists, e.Todos)
		}
	}
	if len(lists) != 2 || len(lists[1]) != 0 {
		t.Fatalf("todo events: %+v, want one per write and the second empty", lists)
	}
	if snap := s.Snapshot().Todos; len(snap) != 0 {
		t.Fatalf("the snapshot still holds %+v after the list was cleared", snap)
	}
}

// TestNativeTodoContentIsRedactedAndSanitized (A5): a todo is the model's own
// words, so the harness does not redact it (its events say so) — the adapter
// does, and sanitizes it, before the panel or `--json` can see it.
func TestNativeTodoContentIsRedactedAndSanitized(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	w := newNativeWatcher(t, s)
	f.models["test/a"].push(
		nativeCallStep("c1", "todo_write", nativeArgs(t, map[string]any{
			"todos": []any{map[string]any{
				"id":      "one\x1b[31m",
				"content": "write \x1b]0;x\x07 the key " + nativeCanary + "\x00",
			}},
		})),
		answer("done"),
	)

	if got := await(t, startPrompt(s, "track it"), "the prompt"); got.err != nil {
		t.Fatalf("Prompt: %v", got.err)
	}
	ev := w.waitType(EventTodos)
	for _, td := range append(append([]Todo(nil), ev.Todos...), s.Snapshot().Todos...) {
		for what, got := range map[string]string{"the id": td.ID, "the content": td.Content} {
			if strings.Contains(got, nativeCanary) {
				t.Fatalf("%s carries the canary key: %q", what, got)
			}
			if strings.ContainsAny(got, "\x1b\x07\x00") {
				t.Fatalf("%s carries a control byte: %q", what, got)
			}
		}
	}
	if len(ev.Todos) != 1 || !strings.HasPrefix(ev.Todos[0].Content, "write ") {
		t.Fatalf("the todo lost its text: %+v", ev.Todos)
	}
}

// TestNativeAskWithNoTurnOpensNothing (§3.4): the asker called outside a turn
// opens no ask — one against the no-turn token could never be ended by a turn —
// and answers as nobody having answered.
func TestNativeAskWithNoTurnOpensNothing(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Interactive: true})
	a := nativeAsker{s: s}
	ans, err := a.AskQuestion(context.Background(), []tool.Question{{
		Question: "Anyone there?",
		Options:  []tool.QuestionOption{{Label: "no"}},
	}})
	if err != nil || ans.Answered {
		t.Fatalf("AskQuestion between turns = %+v, %v", ans, err)
	}
	out, err := a.PresentPlan(context.Background(), tool.PlanOffer{Path: "p", Text: "# a plan"})
	if err != nil || out != tool.PlanUnanswered {
		t.Fatalf("PresentPlan between turns = %v, %v", out, err)
	}
	if len(s.Asks().Asks()) != 0 || len(s.Asks().Resolved()) != 0 {
		t.Fatalf("the registry holds %+v / %+v", s.Asks().Asks(), s.Asks().Resolved())
	}
}
