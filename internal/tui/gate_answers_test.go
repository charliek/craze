package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
)

// A card's answer, an interjection and the cancel mask's read through the
// command gate (plan 027 §3.12, C18c): answerCard and interject are gated
// calls whose post-call work is their continuation; the mask's registry read
// is a gated Ask opened by an EVENT, inside applyEvent, whose continuation is
// the card's push or drop; the hidden answers are fire-and-forget; every
// session-dependent result carries the session generation, and every chain
// step the backend epoch. These are the site schedules
// TestTheGatedFrameSequenceIsTodays extends to (X34) and the named tests of
// §3.12 (d) and "Deadline and bounds" that land with them.

// shortDeadlines shortens the gate's deadlines for t: gate for every gated call
// but an Interject, interject for an Interject's own.
func shortDeadlines(t *testing.T, gate, interject time.Duration) {
	t.Helper()
	prevGate, prevInterject := gateDeadline, interjectDeadline
	gateDeadline, interjectDeadline = gate, interject
	t.Cleanup(func() { gateDeadline, interjectDeadline = prevGate, prevInterject })
}

// oneQuestion is a question card with a single single-select question: a
// number answers it.
func oneQuestion(id string) *agent.QuestionEvent {
	q := stubQuestion()
	q.ID = id
	q.Questions = q.Questions[:1]
	return q
}

// cardOpened is a prepare's step: the ask ev opens (Stub.Emit, as the session
// does) and is applied, its card up.
func cardOpened(r *schedRun, ev agent.Event) {
	r.t.Helper()
	r.stub.Emit(ev)
	r.feed()
	if !r.m().cardOpen() {
		r.t.Fatalf("prepare: %s raised no card", ev.Type)
	}
}

// grokWorking is a prepare's step: an interject-capable provider and a
// working turn open at the session.
func grokWorking(r *schedRun) {
	r.t.Helper()
	r.sess.SetProvider(agent.GrokProvider())
	r.step(refreshSnapMsg{})
	if !r.m().caps().Interject {
		r.t.Fatal("prepare: the provider cannot interject")
	}
	aWorkingTurn(r)
}

// headAsk is the head card's ask id, "" with no card.
func headAsk(m Model) string {
	c, ok := m.headCard()
	if !ok {
		return ""
	}
	return cardAskID(c)
}

// ------------------------------------------- the event and the earlier key

// TestAnEventNeverOvertakesAnEarlierKey (astra 2; X34): an Interject is
// pending — its gate open — when a key `1` arrives, then a question's opening,
// then the Interject's reply. Today the Interject blocked inside Ctrl+L's
// Update, so `1` reached the composer and the question opened after it; under
// the gate the reply is applied alone, then `1`, then the opening, one per
// drain, so `1` lands in the composer and the card opens after it, untouched
// by the key. The variants put an asynchronous reply ahead of the key — the
// plan offer's mode change (whose own Submit opens a gate mid-drain), a
// refused model change, a failed cancel — and it keeps its place too. Every
// case ends in the gateSync baseline's state.
func TestAnEventNeverOvertakesAnEarlierKey(t *testing.T) {
	type variant struct {
		name string
		// msg is the asynchronous reply that arrives first, nil for none.
		msg func(m Model) tea.Msg
		// applied says its effect is on the model.
		applied func(m Model) bool
	}
	for _, v := range []variant{
		{name: "the schedule"},
		{
			name: "a held planImplementMsg",
			msg: func(m Model) tea.Msg {
				return planImplementMsg{seq: m.turnSeq, gen: m.modeGen, mode: "agent"}
			},
			applied: func(m Model) bool {
				return slices.Contains(texts(m, entryNote), modeNote(m.snap.Modes, "agent")) && len(m.queue) == 1
			},
		},
		{
			name: "a held revertModelMsg",
			msg: func(Model) tea.Msg {
				return revertModelMsg{prev: "grok-3", err: errors.New("the model change was refused")}
			},
			applied: func(m Model) bool {
				return slices.Contains(texts(m, entryError), "the model change was refused")
			},
		},
		{
			name: "a held cancelFailedMsg",
			msg: func(Model) tea.Msg {
				return cancelFailedMsg{err: errors.New("the cancel never reached the agent")}
			},
			applied: func(m Model) bool {
				return slices.Contains(texts(m, entryError), "the cancel never reached the agent")
			},
		},
	} {
		t.Run(v.name, func(t *testing.T) {
			var finals [2]Model
			for i, mode := range frameGateModes {
				t.Run(mode.name, func(t *testing.T) {
					r := newSchedRun(t, mode.sync)
					grokWorking(r)
					r.typeText("hi")
					// The question opens while the Interject is in flight: in
					// the session's log ahead of the interjection's own row, and
					// on the stream before the model has applied it — so it is
					// taken off the stream now and handed to the model after
					// the key, where it arrived.
					ask := oneQuestion("ask-q")
					r.stub.Emit(agent.Event{Type: agent.EventQuestion, Question: ask})
					opening := r.until(func(evs []agent.Event) bool {
						return slices.ContainsFunc(evs, func(ev agent.Event) bool { return ev.Type == agent.EventQuestion })
					})
					r.noDrain = true
					r.step(tea.KeyMsg{Type: tea.KeyCtrlL})
					if !mode.sync && (r.m().gate == nil || len(r.calls) != 1) {
						t.Fatalf("the Interject opened no gate (gate %v, %d calls)", r.m().gate, len(r.calls))
					}
					if v.msg != nil {
						r.step(v.msg(r.m()))
					}
					r.step(runeKey('1'))
					r.stepEvents(opening)
					if !mode.sync {
						m := r.m()
						if m.input.Value() != "hi" || m.cardOpen() || (v.applied != nil && v.applied(m)) {
							t.Fatalf("a held message was applied under the gate: composer %q, card %v", m.input.Value(), m.cardOpen())
						}
						rep := runWatched(t, r.calls[0])
						r.calls = nil
						r.step(rep)
						m = r.m()
						if m.input.Value() != "" || m.cardOpen() || (v.applied != nil && v.applied(m)) {
							t.Fatalf("the release applied more than the reply: composer %q, card %v", m.input.Value(), m.cardOpen())
						}
						drainOne := func() {
							t.Helper()
							if r.drains == 0 {
								t.Fatal("no drain is owed")
							}
							r.drains--
							r.step(drainMsg{})
						}
						if v.msg != nil {
							drainOne()
							// The mode change's implement prompt is a Submit:
							// its gate stops the drain until its reply.
							r.answer()
							m = r.m()
							if !v.applied(m) || m.input.Value() != "" || m.cardOpen() {
								t.Fatalf("the held reply was not applied first: applied %v, composer %q, card %v", v.applied(m), m.input.Value(), m.cardOpen())
							}
						}
						drainOne()
						if m = r.m(); m.input.Value() != "1" || m.cardOpen() {
							t.Fatalf("the key was not applied before the opening: composer %q, card %v", m.input.Value(), m.cardOpen())
						}
						r.drainOwed()
					}
					// What the session published after — the interjection's
					// row, the implement prompt's row — follows, in both modes.
					r.noDrain = false
					r.feed()
					m := r.m()
					if m.input.Value() != "1" {
						t.Fatalf("the composer holds %q, want the key typed before the opening", m.input.Value())
					}
					if headAsk(m) != "ask-q" || m.cards[0].optSel != 0 {
						t.Fatalf("the card is %q with option %d, want the opening's, untouched by the key", headAsk(m), m.cards[0].optSel)
					}
					if open := r.stub.Asks().Asks(); len(open) != 1 || open[0].ID != "ask-q" {
						t.Fatalf("the key answered the question: open asks %+v", open)
					}
					if v.applied != nil && !v.applied(m) {
						t.Fatal("the held reply was never applied")
					}
					if got := r.stub.Interjections(); !slices.Equal(got, []string{"hi"}) {
						t.Fatalf("interjections %q", got)
					}
					finals[i] = m
				})
			}
			if t.Failed() {
				return
			}
			b, a := finals[0], finals[1]
			for _, kind := range []entryKind{entryUser, entryNote, entryError} {
				if bt, at := texts(b, kind), texts(a, kind); !slices.Equal(bt, at) {
					t.Fatalf("the transcripts differ: %q in the baseline, %q asynchronously", bt, at)
				}
			}
			if b.input.Value() != a.input.Value() || headAsk(b) != headAsk(a) || len(b.queue) != len(a.queue) {
				t.Fatal("the modes ended in different states")
			}
		})
	}
}

// --------------------------------------------- an answer that never arrives

// TestAnAnswerThatNeverArrivesKeepsItsCard (§3.12 "ErrNoAnswer never loses an
// open ask", astra r2 12): a card's answer that does not answer before its
// deadline — a command that never ran, and one that ran with its reply lost —
// raises the card again with the pinned note and writes no error. The ask the
// command never reached is still open, and the same key answers it once the
// session answers; the one it reached is resolved, and its ending removes the
// card and draws the answer's note (the fold's, plan 032 C4).
func TestAnAnswerThatNeverArrivesKeepsItsCard(t *testing.T) {
	shortDeadlines(t, 20*time.Millisecond, 20*time.Millisecond)
	for _, tc := range []struct {
		name  string
		ev    agent.Event
		key   tea.Msg
		ask   string
		notes []string
	}{
		{"a plan accepted", agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()}, runeKey('a'), "plan-1", []string{"plan Fake Plan → accepted"}},
		{"a question answered", agent.Event{Type: agent.EventQuestion, Question: oneQuestion("ask-q")}, runeKey('1'), "ask-q", []string{"? Pick one → A"}},
		{"a permission allowed", agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(false)}, runeKey('a'), "perm-1", nil},
	} {
		for _, ran := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ran %v", tc.name, ran), func(t *testing.T) {
				r := newSchedRun(t, false)
				cardOpened(r, tc.ev)
				notesBefore := len(texts(r.m(), entryNote))
				sb := r.stall("Answer", ran)
				r.step(tc.key)
				if len(r.calls) != 1 || r.m().gate == nil {
					t.Fatalf("the answer opened no gate (%d calls)", len(r.calls))
				}
				r.answerStalled(sb)
				m := r.m()
				if headAsk(m) != tc.ask {
					t.Fatalf("the card is %q, want %q raised again", headAsk(m), tc.ask)
				}
				notes := texts(m, entryNote)[notesBefore:]
				if !slices.Equal(notes, []string{noAnswerCardNote}) {
					t.Fatalf("notes %q, want the pinned no-answer note alone", notes)
				}
				if len(texts(m, entryError)) != 0 {
					t.Fatalf("an unanswered answer wrote an error %q", texts(m, entryError))
				}
				open := r.stub.Asks().Asks()
				if ran != (len(open) == 0) {
					t.Fatalf("open asks %+v (the answer ran: %v)", open, ran)
				}
				if ran {
					// The answer landed: its ending removes the card.
					r.feed()
					m = r.m()
					if m.cardOpen() {
						t.Fatalf("the landed answer's ending left the card: %+v", m.cards)
					}
					if got := texts(m, entryNote)[notesBefore+1:]; !slices.Equal(got, tc.notes) {
						t.Fatalf("the ending wrote %q, want %q", got, tc.notes)
					}
					return
				}
				// The ask is still open: the same key answers it.
				r.f.inner.eng = sb.Backend
				r.step(tc.key)
				r.answer()
				r.feed()
				m = r.m()
				if m.cardOpen() || len(r.stub.Asks().Asks()) != 0 {
					t.Fatalf("pressing again did not answer: card %v, open %+v", m.cardOpen(), r.stub.Asks().Asks())
				}
				if got := texts(m, entryNote)[notesBefore+1:]; !slices.Equal(got, tc.notes) {
					t.Fatalf("the answer wrote %q, want %q", got, tc.notes)
				}
			})
		}
	}
}

// ------------------------------------------------------- the mask's read

// maskedNoTurn is a prepare: a plan card that belongs to no turn of craze's
// own, and Esc on it — a cancel with no turn, which the engine accepts because
// an ask is pending (X9), and which masks keyed to no turn, so the mask stays
// up until the next turn begins. inflight are openings the session makes
// before the cancel, which it therefore resolves: they are taken off the
// stream unapplied and answered, in order, with everything the cancel then
// published.
func maskedNoTurn(r *schedRun, inflight ...agent.Event) (openings, after []agent.Event) {
	r.t.Helper()
	cardOpened(r, agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()})
	for _, ev := range inflight {
		r.stub.Emit(ev)
	}
	openings = r.pending()
	if len(openings) != len(inflight) {
		r.t.Fatalf("prepare: %d events in flight, want the %d openings", len(openings), len(inflight))
	}
	r.step(tea.KeyMsg{Type: tea.KeyEsc})
	if len(r.cancels) != 1 || !r.m().cardMasking || r.m().cardMask != "" {
		r.t.Fatalf("prepare: Esc handed back %d cancels, mask %v keyed %q", len(r.cancels), r.m().cardMasking, r.m().cardMask)
	}
	c := r.cancels[0]
	r.cancels = nil
	if msg := runWatched(r.t, c); msg != nil {
		r.t.Fatalf("prepare: the cancel answered %#v", msg)
	}
	if open := r.stub.Asks().Asks(); len(open) != 0 {
		r.t.Fatalf("prepare: the cancel left %+v open", open)
	}
	return openings, r.pending()
}

// maskedOpening is a prepare: the masked opening a schedule then applies, and
// the events after it. live is an ask opened once the no-turn cancel has
// settled, still open (a card raised); otherwise one in flight at the cancel,
// which resolved it (dropped), its ending among after.
func maskedOpening(r *schedRun, live bool) (opening agent.Event, after []agent.Event) {
	r.t.Helper()
	if !live {
		openings, after := maskedNoTurn(r, agent.Event{Type: agent.EventQuestion, Question: oneQuestion("ask-q")})
		return openings[0], after
	}
	_, done := maskedNoTurn(r)
	r.stepEvents(done)
	if r.m().cardOpen() || !r.m().cardMasking {
		r.t.Fatalf("prepare: card %v, mask %v", r.m().cardOpen(), r.m().cardMasking)
	}
	r.stub.Emit(agent.Event{Type: agent.EventQuestion, Question: oneQuestion("ask-live")})
	evs := r.pending()
	if len(evs) != 1 {
		r.t.Fatalf("prepare: %d events, want the live opening", len(evs))
	}
	return evs[0], nil
}

// TestAMaskReadThatFailsShowsTheCard (§3.12 "ErrNoAnswer never loses an open
// ask", astra r2 12): under the cancel mask an opening's card is decided by a
// gated read of its ask, and a read that does not answer — a command that
// never ran, and one whose reply was lost — pushes the card: "the read failed"
// is not "known closed". A card the cancel had already resolved is then
// removed by its ending when that arrives; a live one stays, and the same key
// answers it. The control: a read that answers drops the resolved opening,
// never raising its card, asynchronously as in the baseline.
func TestAMaskReadThatFailsShowsTheCard(t *testing.T) {
	shortDeadlines(t, 20*time.Millisecond, 20*time.Millisecond)
	resolvedAsk := agent.Event{Type: agent.EventQuestion, Question: oneQuestion("ask-q")}
	for _, ran := range []bool{false, true} {
		t.Run(fmt.Sprintf("resolved by the cancel/ran %v", ran), func(t *testing.T) {
			r := newSchedRun(t, false)
			openings, after := maskedNoTurn(r, resolvedAsk)
			sb := r.stall("Ask", ran)
			r.step(eventMsg{ev: openings[0]})
			if len(r.calls) != 1 || r.m().gate == nil {
				t.Fatalf("the masked opening opened no gate (%d calls)", len(r.calls))
			}
			r.answerStalled(sb)
			if !slices.ContainsFunc(r.m().cards, func(c card) bool { return cardAskID(c) == "ask-q" }) {
				t.Fatalf("a mask read that failed dropped the card: %+v", r.m().cards)
			}
			r.stepEvents(after)
			if r.m().cardOpen() {
				t.Fatalf("the resolved ask's ending did not remove its card: %+v", r.m().cards)
			}
		})
		t.Run(fmt.Sprintf("live/ran %v", ran), func(t *testing.T) {
			r := newSchedRun(t, false)
			opening, _ := maskedOpening(r, true)
			sb := r.stall("Ask", ran)
			r.step(eventMsg{ev: opening})
			r.answerStalled(sb)
			if headAsk(r.m()) != "ask-live" {
				t.Fatalf("a mask read that failed dropped a live card: %+v", r.m().cards)
			}
			r.f.inner.eng = sb.Backend
			r.step(runeKey('1'))
			r.answer()
			r.feed()
			if r.m().cardOpen() || len(r.stub.Asks().Asks()) != 0 {
				t.Fatalf("the live card could not be answered: card %v, open %+v", r.m().cardOpen(), r.stub.Asks().Asks())
			}
		})
	}
	for _, mode := range frameGateModes {
		t.Run("control: a read that answers drops the resolved opening/"+mode.name, func(t *testing.T) {
			r := newSchedRun(t, mode.sync)
			openings, after := maskedNoTurn(r, resolvedAsk)
			r.step(eventMsg{ev: openings[0]})
			if !mode.sync && len(r.calls) != 1 {
				t.Fatalf("the masked opening issued %d gated calls, want its read", len(r.calls))
			}
			r.answer()
			r.stepEvents(after)
			for _, f := range r.frames {
				if strings.Contains(f.plain, "question 1/1") {
					t.Fatalf("the resolved opening's card flashed up:\n%s", f.plain)
				}
			}
		})
	}
}

// TestAMaskedOpeningIsDecidedInItsContinuation: the masked opening's push —
// the card, and everything a card's arrival does to the rest of the UI (a
// confirm waiting for Enter dropped with its note) — is the continuation of
// its read, so an opening the mask drops leaves all of it alone, and one it
// raises does all of it; asynchronously, the opening's own Update draws
// neither, its frame being the gate's.
func TestAMaskedOpeningIsDecidedInItsContinuation(t *testing.T) {
	for _, live := range []bool{false, true} {
		for _, mode := range frameGateModes {
			t.Run(fmt.Sprintf("live %v/%s", live, mode.name), func(t *testing.T) {
				r := newSchedRun(t, mode.sync)
				opening, after := maskedOpening(r, live)
				r.f.inner.confirm = &strongSend{text: "held"}
				r.step(eventMsg{ev: opening})
				if !mode.sync {
					m := r.m()
					if m.confirm == nil || slices.ContainsFunc(m.cards, func(c card) bool { return cardAskID(c) == "ask-q" || cardAskID(c) == "ask-live" }) {
						t.Fatalf("the opening's own Update decided its card: confirm %v, cards %+v", m.confirm != nil, m.cards)
					}
					if !r.frames[len(r.frames)-1].gated {
						t.Fatal("the opening's frame was not the gate's")
					}
					r.answer()
				}
				m := r.m()
				if live {
					if headAsk(m) != "ask-live" || m.confirm != nil || m.copyNote != "send now dropped" {
						t.Fatalf("a raised card did not do its arrival's work: head %q, confirm %v, note %q", headAsk(m), m.confirm != nil, m.copyNote)
					}
					return
				}
				if slices.ContainsFunc(m.cards, func(c card) bool { return cardAskID(c) == "ask-q" }) || m.confirm == nil || m.copyNote == "send now dropped" {
					t.Fatalf("a dropped opening did its arrival's work: cards %+v, confirm %v, note %q", m.cards, m.confirm != nil, m.copyNote)
				}
				r.stepEvents(after)
			})
		}
	}
}

// TestADrainedOpeningThatOpensAGateRestartsTheParkedReader (§3.12 "The
// reader", "Drain"; the one gate an event opens): a masked opening held behind
// another gate is drained with the reader parked — an event that arrived
// during the drain is held, and no read follows it — and its read of the ask
// opens a gate of its own. The drain stops there, with what is still held left
// held, and the reader restarts at once: while a gate is open the stream must
// keep draining. The read's reply raises the card, and the drain goes on.
func TestADrainedOpeningThatOpensAGateRestartsTheParkedReader(t *testing.T) {
	m, stub := gatedModel(t)
	r := newGateRig(t, m)
	// Under a no-turn cancel's mask (the fixture sets it: the mask's own
	// schedule is maskedNoTurn's), a live ask opens, and the agent says
	// something after it.
	r.m.cardMasking, r.m.cardMask = true, ""
	stub.Emit(agent.Event{Type: agent.EventQuestion, Question: oneQuestion("ask-live")})
	stub.Emit(agent.Event{Type: agent.EventText, Text: "after the card"})

	release := make(chan struct{})
	r.send(gateOpMsg{call: blockedCall(release, nil), cont: noteCont("first")})
	if ev := r.read(); ev.Type != agent.EventQuestion {
		t.Fatalf("read %s, want the opening", ev.Type)
	}
	close(release)
	r.answer()
	if r.drains != 1 || len(r.reads) != 1 {
		t.Fatalf("after the first release: %d drains owed, %d reads", r.drains, len(r.reads))
	}
	// The read in flight lands while the drain is under way: held, and the
	// reader parks.
	if ev := r.read(); ev.Type != agent.EventText {
		t.Fatalf("read %s, want the text after the opening", ev.Type)
	}
	if len(r.reads) != 0 {
		t.Fatalf("%d reads outstanding while held messages drain with no gate open, want the reader parked", len(r.reads))
	}
	r.drain()
	switch {
	case r.m.gate == nil || len(r.calls) != 1:
		t.Fatalf("the drained opening opened no gate (gate %v, %d calls)", r.m.gate, len(r.calls))
	case r.drains != 0 || len(r.m.held) != 1:
		t.Fatalf("the drain went on past the opening's gate: %d owed, %d held", r.drains, len(r.m.held))
	case len(r.reads) != 1:
		t.Fatalf("%d reads outstanding under the opening's gate, want the parked reader restarted", len(r.reads))
	case r.m.cardOpen():
		t.Fatal("the opening's card was pushed before its read answered")
	}
	r.answer()
	if headAsk(r.m) != "ask-live" {
		t.Fatalf("the read's reply did not raise the live card: %+v", r.m.cards)
	}
	r.drainAll()
	if len(r.m.held) != 0 || !strings.Contains(plainView(r.m), "after the card") {
		t.Fatalf("the drain did not finish: %d held", len(r.m.held))
	}
}

// ------------------------------------------------------- the interjection

// slowInterject is the in-process backend with an Interject that takes d
// before the session's own.
type slowInterject struct {
	backend.Backend
	d time.Duration
}

func (b slowInterject) Interject(ctx context.Context, c engine.Command, text string) error {
	select {
	case <-time.After(b.d):
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.Backend.Interject(ctx, c, text)
}

// TestAnInterjectionIsGivenItsMinute (§3.12 "Deadline and bounds"): an
// Interject waits on the agent taking the message, and is given its own
// deadline — a minute, where every other gated call has fifteen seconds — so
// one that takes longer than a gated call's deadline still lands: the draft
// and the shell context go, and nothing is noted.
func TestAnInterjectionIsGivenItsMinute(t *testing.T) {
	shortDeadlines(t, 20*time.Millisecond, 5*time.Second)
	r := newSchedRun(t, false)
	grokWorking(r)
	r.f.inner = plantShellResult(r.f.inner, "git status", "clean\n")
	r.typeText("hi")
	r.f.inner.eng = slowInterject{Backend: r.m().eng, d: 200 * time.Millisecond}
	r.step(tea.KeyMsg{Type: tea.KeyCtrlL})
	if len(r.calls) != 1 {
		t.Fatalf("the Interject issued %d gated calls", len(r.calls))
	}
	rep := runWatched(t, r.calls[0])
	r.calls = nil
	if g := rep.(gateReply); g.err != nil {
		t.Fatalf("a slow Interject came back %v: it was held to another call's deadline", g.err)
	}
	r.step(rep)
	m := r.m()
	if m.input.Value() != "" || len(m.shellCtx) != 0 || m.copyNote != "" {
		t.Fatalf("the landed Interject left composer %q, %d shell results, note %q", m.input.Value(), len(m.shellCtx), m.copyNote)
	}
}

// TestAnInterjectionThatNeverAnswersKeepsTheDraft (§3.12 "On expiry"): an
// Interject that does not answer within its deadline — never run, or run with
// its answer lost — keeps the draft and the shell context and says the
// message may have been sent.
func TestAnInterjectionThatNeverAnswersKeepsTheDraft(t *testing.T) {
	shortDeadlines(t, 20*time.Millisecond, 20*time.Millisecond)
	for _, ran := range []bool{false, true} {
		t.Run(fmt.Sprintf("ran %v", ran), func(t *testing.T) {
			r := newSchedRun(t, false)
			grokWorking(r)
			r.f.inner = plantShellResult(r.f.inner, "git status", "clean\n")
			r.typeText("hi")
			sb := r.stall("Interject", ran)
			r.step(tea.KeyMsg{Type: tea.KeyCtrlL})
			r.answerStalled(sb)
			m := r.m()
			if m.copyNote != noAnswerInterjectNote {
				t.Fatalf("the note is %q, want %q", m.copyNote, noAnswerInterjectNote)
			}
			if m.input.Value() != "hi" || len(m.shellCtx) != 1 {
				t.Fatalf("an unanswered Interject lost the draft %q or its shell context (%d)", m.input.Value(), len(m.shellCtx))
			}
			if sent := r.stub.Interjections(); ran != (len(sent) == 1) {
				t.Fatalf("interjections %q (ran %v)", sent, ran)
			}
		})
	}
}

// ------------------------------------------------------- the hidden answers

// TestHiddenAnswersAreFireAndForget (§3.12 "Fire-and-forget", astra 3): an
// answer to an ask the config hides is a command of its own, never gated —
// several in one Update are several commands, a retry and a new opening's
// alike; one the outbox refuses for room comes back on the retry list through
// its own message, which arms the retry's beat. Their endings draw nothing:
// the fold is given the asks the capabilities hide (plan 032 C4).
func TestHiddenAnswersAreFireAndForget(t *testing.T) {
	for _, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			m, stub := hiddenAsksModel(t)
			m.gateSync = mode.sync
			ev := func(id string) agent.Event {
				q := stubQuestion()
				q.ID = id
				return agent.Event{Type: agent.EventQuestion, Question: q}
			}
			evs := []agent.Event{ev("ask-1"), ev("ask-2"), ev("ask-3")}
			for _, e := range evs {
				stub.Emit(e)
			}
			release := saturate(t, stub)

			// Three openings refused for room. Each Update hands back its own
			// answer as a command, and every answer still waiting for room
			// with it — an event is the retry's fast path — so the second
			// carries a retry and an opening, the third two and one: several
			// in one Update, several commands. None opens a gate; each
			// refusal comes back on the retry list, and arms its beat.
			for i, e := range evs {
				tm, cmd := m.Update(eventMsg{ev: e})
				m = tm.(Model)
				if m.gate != nil || len(m.held) != 0 {
					t.Fatal("a hidden answer opened a gate")
				}
				if cmds := hiddenAnswerCmds(cmd); len(cmds) != i+1 {
					t.Fatalf("opening %d handed back %d hidden answers, want %d", i+1, len(cmds), i+1)
				}
				if len(m.hiddenRetry) != 0 {
					t.Fatalf("opening %d left %+v on the list: its Update took every retry", i+1, m.hiddenRetry)
				}
				m = hiddenAnswered(t)(m, cmd)
				if len(m.hiddenRetry) != i+1 || !m.hiddenRetryLive {
					t.Fatalf("after opening %d the list holds %+v (beat %v), want every refusal back on it", i+1, m.hiddenRetry, m.hiddenRetryLive)
				}
			}
			if len(stub.Calls()) != 0 {
				t.Fatalf("a refused hidden answer resolved %+v", stub.Calls())
			}

			// Room again: the beats take them, the stream read as the program
			// would, until every hidden ask is answered.
			release()
			var endings []agent.Event
			deadline := time.Now().Add(stubEventWait)
			for len(stub.Calls()) < len(evs) {
				if time.Now().After(deadline) {
					t.Fatalf("the beats answered %d of %d hidden asks: list %+v", len(stub.Calls()), len(evs), m.hiddenRetry)
				}
				for _, e := range stubBuffered(stub) {
					if e.Type == agent.EventAsk {
						endings = append(endings, e)
					}
				}
				m = hiddenAnswered(t)(m.Update(hiddenRetryMsg{}))
				if m.gate != nil {
					t.Fatal("a retried hidden answer opened a gate")
				}
			}
			var ids []string
			for _, c := range stub.Calls() {
				if !c.Skip {
					t.Fatalf("a hidden question was answered %+v, want skipped", c)
				}
				ids = append(ids, c.ID)
			}
			slices.Sort(ids)
			if !slices.Equal(ids, []string{"ask-1", "ask-2", "ask-3"}) {
				t.Fatalf("the hidden asks answered: %q", ids)
			}
			// Their endings find no card and write nothing.
			for len(endings) < len(evs) {
				select {
				case e := <-stub.Events():
					if e.Type == agent.EventAsk {
						endings = append(endings, e)
					}
				case <-time.After(time.Until(deadline)):
					t.Fatalf("%d of %d hidden asks' endings came", len(endings), len(evs))
				}
			}
			before := plainView(m)
			for _, e := range endings {
				tm, _ := m.Update(eventMsg{ev: e})
				m = tm.(Model)
			}
			if m.cardOpen() || plainView(m) != before {
				t.Fatalf("a hidden ask's ending drew something:\n%s", plainView(m))
			}
		})
	}
}

// --------------------------------------------------- the session generation

// TestAStaleSettingsReplyIsDroppedAfterARestore (§3.12, astra r2 13): every
// session-dependent asynchronous result carries the session generation it was
// issued under, and one that lands after the session was replaced — in
// process an engine swap, the pickers' path; PR 4 adds a restore from another
// incarnation — is dropped, unapplied: a settings reply above all, and every
// other result a command hands back. The control: the same result, on the
// model that issued it, is applied.
func TestAStaleSettingsReplyIsDroppedAfterARestore(t *testing.T) {
	type issuedResult struct {
		m   Model
		msg tea.Msg
	}
	// issuedBy is a site's answer: the model its Update left, and what the
	// command it handed back answered.
	issuedBy := func(tm tea.Model, cmd tea.Cmd) issuedResult { return issuedResult{tm.(Model), runCmd(cmd)} }
	for _, tc := range []struct {
		name  string
		issue func(t *testing.T) issuedResult
	}{
		{"a mode change's answer (modeAppliedMsg)", func(t *testing.T) issuedResult {
			m := sized(t)
			return issuedBy(m.applyMode("plan"))
		}},
		{"a mode change refused (revertModeMsg)", func(t *testing.T) issuedResult {
			m := sized(t)
			stubOf(t, m).FailNextSetMode()
			return issuedBy(m.applyMode("plan"))
		}},
		{"/model refused (revertModelMsg)", func(t *testing.T) issuedResult {
			m, stub := perModelStub(t)
			stub.FailNextSetModel()
			m.input.SetValue("/model fast")
			return issuedBy(m.Update(enter()))
		}},
		{"/model's effort step (refreshSnapMsg)", func(t *testing.T) issuedResult {
			m, _ := cursorStub(t, "composer-2.5")
			m.input.SetValue("/model grok-4.6 high")
			return issuedBy(m.Update(enter()))
		}},
		{"the dialog's chain (modelApplyMsg)", func(t *testing.T) issuedResult {
			m, _ := perModelStub(t)
			m.input.SetValue("/model")
			tm, _ := m.Update(enter())
			m = typeInto(t, tm.(Model), "FAST")
			return issuedBy(m.Update(enter()))
		}},
		{"the plan offer's mode change (planImplementMsg)", func(t *testing.T) issuedResult {
			m := sized(t)
			if m.implementModeID() == "" {
				t.Fatal("the fixture's provider has no implement mode")
			}
			return issuedBy(m.implementPlan())
		}},
		{"a failed cancel (cancelFailedMsg)", func(t *testing.T) issuedResult {
			m, sess := scriptedModel(t)
			m = startScripted(t, m, sess, "go", scriptHeld())
			sess.FailNextCancel(errors.New("the cancel never reached the agent"))
			return issuedBy(m.cancelTurn())
		}},
		{"a hidden answer refused for room (hiddenRefusedMsg)", func(t *testing.T) issuedResult {
			m, stub := hiddenAsksModel(t)
			ev := agent.Event{Type: agent.EventQuestion, Question: stubQuestion()}
			stub.Emit(ev)
			release := saturate(t, stub)
			tm, cmd := m.Update(eventMsg{ev: ev})
			cmds := hiddenAnswerCmds(cmd)
			if len(cmds) != 1 {
				t.Fatalf("%d hidden answers", len(cmds))
			}
			msg := runWatched(t, cmds[0])
			release()
			return issuedResult{tm.(Model), msg}
		}},
		{"a start's answer (startedMsg)", func(t *testing.T) issuedResult {
			isolateSkillsHome(t)
			stub := NewStub()
			t.Cleanup(func() { _ = stub.Close() })
			m := New(Config{Session: stub, Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true})
			return issuedResult{m, runCmd(m.startCmd())}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.issue(t)
			if res.msg == nil {
				t.Fatal("the site's command answered nothing")
			}
			s, ok := res.msg.(interface{ issuedUnder() uint64 })
			if !ok || s.issuedUnder() != res.m.sessGen || res.m.sessGen == 0 {
				t.Fatalf("%T is not stamped with the generation it was issued under (%d)", res.msg, res.m.sessGen)
			}
			// The control: on the model that issued it, it is applied.
			applied := res.m
			before := digestModel(&applied)
			tm, _ := applied.update(res.msg)
			after := tm.(Model)
			if changed := before.diff(digestModel(&after)); len(changed) == 0 {
				t.Fatalf("%T changes nothing even on its own session: the test proves nothing", res.msg)
			}

			// The session is replaced — an engine swap, the pickers' path —
			// and the same result lands.
			swapped := res.m
			if b := swapped.eng; b != nil {
				t.Cleanup(func() { _ = b.Close() })
			}
			next := NewStub()
			t.Cleanup(func() { _ = next.Close() })
			swapped.setSession(next, "")
			if swapped.sessGen == res.m.sessGen {
				t.Fatal("the swap did not move the generation")
			}
			if !swapped.outdated(res.msg) {
				t.Fatalf("%T from the old session is not outdated", res.msg)
			}
			before = digestModel(&swapped)
			tm, _ = swapped.update(res.msg)
			after = tm.(Model)
			if changed := before.diff(digestModel(&after)); len(changed) != 0 {
				t.Fatalf("%T from the old session was applied to the new one: %v changed", res.msg, changed)
			}
		})
	}
}

// TestAGateReplyFromAnotherSessionIsRejectedFirst (plan 030 §3.11, R2-5): a
// gated call's reply carries the session generation it was issued under, and
// the gate turns one for a session the model has left away before any of its
// bookkeeping — even one naming the gate that is open: it releases nothing,
// runs no continuation, acknowledges no pending sync token and owes no drain.
// (Before C11 such a reply released the open gate without its continuation,
// on the premise that a restore from another incarnation could move the
// generation under an open gate. A restore is held behind the gate (C27), a
// switch leaves no gate open and gate ids are never reused, so nothing moves
// the generation while a gate is open; the order is fixed so that a stale
// reply can never be the one that releases a session's gate.) The open gate's
// own, current reply then releases it, and what it held drains.
func TestAGateReplyFromAnotherSessionIsRejectedFirst(t *testing.T) {
	m, _ := gatedModel(t)
	r := newGateRig(t, m)
	release := make(chan struct{})
	r.send(gateOpMsg{call: blockedCall(release, "stale"), cont: noteCont("continued")})
	g := r.m.gate
	broke := gateWatch.expectBreak(g)
	t.Cleanup(func() { gateWatch.forget(g) })
	r.send(frameSyncMsg{n: 9})
	r.send(runeKey('x'))
	if r.m.syncPending != 9 || len(r.m.held) != 1 {
		t.Fatalf("fixture: pending token %d, %d held", r.m.syncPending, len(r.m.held))
	}
	// The session generation moves under the open gate: standing in for the
	// session the call was issued for having been left.
	r.m.sessGen++
	close(release)
	if len(r.calls) != 1 {
		t.Fatalf("%d gated calls outstanding, want the one", len(r.calls))
	}
	rep, ok := runWatched(t, r.calls[0]).(gateReply)
	r.calls = r.calls[1:]
	if !ok || rep.id != g.id || rep.issuedUnder() == r.m.sessGen {
		t.Fatalf("fixture: the reply %+v is not the open gate's, issued under the old generation", rep)
	}
	r.send(rep)
	switch {
	case r.m.gate != g:
		t.Fatal("a reply issued for another session released the open gate")
	case slices.ContainsFunc(texts(r.m, entryNote), func(n string) bool { return strings.HasPrefix(n, "continued") }):
		t.Fatalf("a reply issued for another session ran its continuation: notes %q", texts(r.m, entryNote))
	case r.drains != 0 || r.m.syncAck == 9 || r.m.syncPending != 9 || len(r.m.held) != 1:
		t.Fatalf("a reply issued for another session moved the gate's bookkeeping: %d drains, ack %d, pending %d, %d held",
			r.drains, r.m.syncAck, r.m.syncPending, len(r.m.held))
	}
	// The open gate's own reply, issued for the session the model holds.
	r.send(gateReply{issued: r.m.issue(), id: g.id, result: "current"})
	if r.m.gate != nil || !slices.Contains(texts(r.m, entryNote), "continued: current <nil>") || r.m.syncAck != 9 {
		t.Fatalf("the current reply: gate %v, notes %q, ack %d", r.m.gate, texts(r.m, entryNote), r.m.syncAck)
	}
	if !slices.Equal(broke.fields, []string{"sessGen"}) {
		t.Fatalf("the watch saw %v move, want the generation alone", broke.fields)
	}
	r.drainAll()
	if r.m.input.Value() != "x" || len(r.m.held) != 0 {
		t.Fatalf("the held key did not drain: composer %q, %d held", r.m.input.Value(), len(r.m.held))
	}
}

// ------------------------------------------------------ the backend epoch

// epochMover is the in-process backend whose epoch moves after its first Set
// has answered: a reconnect to another incarnation between a chain's steps
// (PR 4's), standing in for it by moving the in-process backend's own epoch.
type epochMover struct {
	backend.Backend
	inner *engineBackend
	sets  int
}

func (b *epochMover) Set(ctx context.Context, c engine.Command, s engine.Setting) (engine.SetResult, error) {
	res, err := b.Backend.Set(ctx, c, s)
	if b.sets++; b.sets == 1 {
		b.inner.epoch.Add(1)
	}
	return res, err
}

// TestAChainStopsAtABackendReplacement (§3.12 "Chains are fenced in the
// backend too", astra r3 13): a model-change chain carries the backend epoch
// its Update read, and a later step against a backend that has moved to
// another session is refused before it sends anything — the effort never
// reaches the agent. `/model <id> <effort>` through the Update, and the
// dialog's chain; the control is the same chain with the epoch left alone.
func TestAChainStopsAtABackendReplacement(t *testing.T) {
	for _, moved := range []bool{false, true} {
		t.Run(fmt.Sprintf("/model's effort step, epoch moved %v", moved), func(t *testing.T) {
			m, stub := cursorStub(t, "composer-2.5")
			writes := configWrites(stub)
			inner := m.eng.(*engineBackend)
			if moved {
				m.eng = &epochMover{Backend: inner, inner: inner}
			}
			m.input.SetValue("/model grok-4.6 high")
			tm, cmd := m.Update(enter())
			m = tm.(Model)
			msg := runCmd(cmd)
			if got := stub.Snapshot().CurrentModel; got != "grok-4.6" {
				t.Fatalf("the model step did not land: the session is on %q", got)
			}
			if !moved {
				if !slices.Equal(writes(), []string{"effort=high"}) {
					t.Fatalf("the control sent %q", writes())
				}
				return
			}
			if am, ok := msg.(actionErrMsg); !ok || !errors.Is(am.err, backend.ErrStaleEpoch) {
				t.Fatalf("the chain answered %#v, want the effort step refused for its epoch", msg)
			}
			if got := writes(); len(got) != 0 {
				t.Fatalf("a step for the old session reached the agent: %q", got)
			}
		})
		t.Run(fmt.Sprintf("the dialog's chain, epoch moved %v", moved), func(t *testing.T) {
			m, stub := cursorStub(t, "composer-2.5")
			writes := configWrites(stub)
			inner := m.eng.(*engineBackend)
			var b backend.Backend = inner
			if moved {
				b = &epochMover{Backend: inner, inner: inner}
			}
			steps := []applyStep{
				{value: "grok-4.6", label: "model"},
				{cfgID: "effort", value: "high", label: "effort", role: roleEffort},
			}
			ctx := dispatchCtx(inner)
			out := runModelApply(ctx, m.issue(), b, m.snap.Provider, make([]engine.Command, len(steps)), steps, "grok-4.6", 1)
			if !moved {
				if out.err != nil || !slices.Equal(writes(), []string{"effort=high"}) {
					t.Fatalf("the control ended %v and sent %q", out.err, writes())
				}
				return
			}
			if out.step != "effort" || !errors.Is(out.err, backend.ErrStaleEpoch) || len(out.done) != 1 {
				t.Fatalf("the chain ended at %q with %v after %d steps, want the effort step refused", out.step, out.err, len(out.done))
			}
			if got := writes(); len(got) != 0 {
				t.Fatalf("a step for the old session reached the agent: %q", got)
			}
		})
	}
}

// TestTheEngineBackendRefusesAStaleEpochBeforeCallingTheEngine: every command
// and read of the in-process backend is fenced — a context carrying another
// epoch is ErrStaleEpoch, whatever the engine would have said, and the engine
// is never called: no prompt, no answer, no setting, no close.
func TestTheEngineBackendRefusesAStaleEpochBeforeCallingTheEngine(t *testing.T) {
	m, stub := questionCard(t)
	b := m.eng.(*engineBackend)
	stale := backend.WithEpoch(context.Background(), b.Epoch()+1)
	c := engine.Command{}
	calls := map[string]func() error{
		"Submit": func() error {
			_, err := b.Submit(stale, c, "never sent", engine.SubmitQueue, "")
			return err
		},
		"Answer":     func() error { return b.Answer(stale, c, "ask-1", agent.AskAnswer{Skip: true}) },
		"Unqueue":    func() error { _, err := b.Unqueue(stale, c, "row"); return err },
		"EditQueued": func() error { return b.EditQueued(stale, c, "row", "x", nil) },
		"ClearQueue": func() error { _, err := b.ClearQueue(stale, c); return err },
		"Disarm":     func() error { return b.Disarm(stale, c) },
		"Interject":  func() error { return b.Interject(stale, c, "x") },
		"SetTitle":   func() error { return b.SetTitle(stale, c, "never") },
		"Set": func() error {
			_, err := b.Set(stale, c, engine.Setting{Kind: engine.SettingMode, Value: "plan"})
			return err
		},
		"Cancel":         func() error { _, err := b.Cancel(stale, c, ""); return err },
		"CancelSubagent": func() error { return b.CancelSubagent(stale, c, "sub") },
		"Stop":           func() error { return b.Stop(stale, c) },
		"Ask":            func() error { _, _, err := b.Ask(stale, "ask-1"); return err },
		"Settings":       func() error { _, err := b.Settings(stale); return err },
		"LastTurn":       func() error { _, err := b.LastTurn(stale); return err },
	}
	// Every command and read of the interface, by name: a new one is fenced
	// or this fails.
	ty := reflect.TypeFor[backend.Backend]()
	for i := range ty.NumMethod() {
		meth := ty.Method(i)
		if meth.Type.NumIn() == 0 || meth.Type.In(0) != reflect.TypeFor[context.Context]() || meth.Name == "Start" || meth.Name == "Read" {
			continue
		}
		if _, ok := calls[meth.Name]; !ok {
			t.Fatalf("%s takes a context and is not checked here", meth.Name)
		}
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, backend.ErrStaleEpoch) {
			t.Fatalf("%s with a stale epoch answered %v, want ErrStaleEpoch", name, err)
		}
	}
	if got := stub.Prompts(); len(got) != 0 {
		t.Fatalf("a stale Submit reached the session: %q", got)
	}
	if open := stub.Asks().Asks(); len(open) != 1 || len(stub.Calls()) != 0 {
		t.Fatalf("a stale Answer reached the session: open %+v, calls %+v", open, stub.Calls())
	}
	if mode := stub.Snapshot().CurrentMode; mode == "plan" {
		t.Fatal("a stale Set reached the session")
	}
	if st := engineBehind(b).State(); st.Activity == engine.ActivityClosing {
		t.Fatal("a stale Stop closed the engine")
	}
	if err := b.Disarm(dispatchCtx(b), c); errors.Is(err, backend.ErrStaleEpoch) {
		t.Fatal("the current epoch was refused")
	}
}

// ------------------------------------------------- the frame sequences

// answerSites is C18c's key-issued sites as chain schedules (§5's row): a
// card's answer — a plan accepted, a question skipped, a permission allowed —
// and an interjection.
func answerSites() []chainSite {
	esc := tea.KeyMsg{Type: tea.KeyEsc}
	ctrlL := tea.KeyMsg{Type: tea.KeyCtrlL}
	return []chainSite{
		{
			name: "a plan accepted",
			prepare: func(r *schedRun) func() {
				cardOpened(r, agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()})
				return nil
			},
			keys:  theKeys(runeKey('a')),
			links: 1,
			held:  runeKey('k'),
			waits: []waitSpec{{kind: "text", needle: "plan Fake Plan → accepted"}, {kind: "gone", needle: "[a]ccept"}},
		},
		{
			name: "a question skipped",
			prepare: func(r *schedRun) func() {
				cardOpened(r, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
				return nil
			},
			keys:  theKeys(esc),
			links: 1,
			held:  runeKey('k'),
			waits: []waitSpec{{kind: "text", needle: "? Question → skipped"}, {kind: "gone", needle: "question 1/2"}},
		},
		{
			name: "a permission allowed",
			prepare: func(r *schedRun) func() {
				cardOpened(r, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(true)})
				return nil
			},
			keys:  theKeys(runeKey('A')),
			links: 1,
			held:  runeKey('k'),
			waits: []waitSpec{{kind: "gone", needle: "permission Shell"}},
		},
		{
			name:    "an interjection",
			prepare: func(r *schedRun) func() { grokWorking(r); return nil },
			keys:    runesThen("hi", ctrlL),
			links:   1,
			held:    runeKey('k'),
			still:   true,
			waits:   []waitSpec{{kind: "text", needle: "↳ hi"}},
		},
	}
}

// answerFrameSequences is TestTheGatedFrameSequenceIsTodays at C18c's
// key-issued sites (X34), over C18b's chain schedule (chainFrameSequences'
// assertions, chainOrders' every arrival order).
func answerFrameSequences(t *testing.T) {
	chainFrameSequencesOf(t, answerSites())
}

// maskSchedule runs the masked opening in one gate mode and one arrival order
// ("R" the read's reply, "K" a key and its sync token as one message, "E" the
// events after the opening). The opening is the issuing message: its own
// Update opens the read's gate. live says its ask is still open (a card
// raised) rather than resolved by the cancel (dropped). It answers every
// frame published and the key's token.
func maskSchedule(t *testing.T, live, sync bool, order []string) ([]frameState, int) {
	t.Helper()
	r := newSchedRun(t, sync)
	r.blind = true
	opening, after := maskedOpening(r, live)
	if live {
		r.stub.Emit(agent.Event{Type: agent.EventText, Text: "after the card"})
		after = r.pending()
	}
	r.blind = false
	r.f.publish()
	r.frames = append(r.frames, r.f.bus.last())
	r.step(eventMsg{ev: opening})
	r.n++
	arrivals := map[string]any{
		"K": frameTokenMsg{msg: tea.KeyMsg{Type: tea.KeyDown}, n: r.n},
		"E": after,
	}
	if sync {
		if len(r.calls) != 0 || r.m().gate != nil {
			t.Fatal("the baseline left the read open")
		}
	} else {
		if len(r.calls) != 1 || !r.frames[len(r.frames)-1].gated {
			t.Fatalf("the opening opened no gate (%d calls)", len(r.calls))
		}
		arrivals["R"] = runWatched(t, r.calls[0])
		r.calls = nil
	}
	for _, a := range order {
		msg, ok := arrivals[a]
		if !ok {
			t.Fatalf("arrival %s never came", a)
		}
		switch msg := msg.(type) {
		case []agent.Event:
			r.stepEvents(msg)
		default:
			r.step(msg)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("%d gated calls were left", len(r.calls))
	}
	return r.frames, r.n
}

// maskFrameSequences is TestTheGatedFrameSequenceIsTodays at the one gate an
// event opens: the masked opening, its read's reply, a key with its sync
// token — one message, as the frame runner delivers it, arriving while the
// read's gate is open (C17c, X42: held behind nothing but its key, then
// acknowledged in its turn) — and the events after it, in every order. The
// frames a wait can see after the token are the baseline's; a raised card
// takes the key (↓ moves its cursor) because the key arrived after the
// opening; a dropped one never flashes up.
func maskFrameSequences(t *testing.T) {
	for _, tc := range []struct {
		name  string
		live  bool
		waits []waitSpec
	}{
		{"a live ask raised", true, []waitSpec{{kind: "text", needle: "> 2 B"}, {kind: "text", needle: "after the card"}}},
		{"a resolved ask dropped", false, []waitSpec{{kind: "gone", needle: "plan Fake Plan"}, {kind: "idle"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Every order of the three: siteOrders' own rules name labels
			// these are not.
			for _, order := range siteOrders([]string{"R", "K", "E"}) {
				t.Run(strings.Join(order, ","), func(t *testing.T) {
					base := baselineOrder(order)
					bf, bn := maskSchedule(t, tc.live, true, base)
					af, an := maskSchedule(t, tc.live, false, order)
					if bn != an {
						t.Fatalf("the tokens differ: %d, %d", bn, an)
					}
					want, got := matchable(bf, bn), matchable(af, an)
					if len(want) == 0 {
						t.Fatal("the baseline published no frame after the token's barrier")
					}
					if !reflect.DeepEqual(want, got) {
						t.Fatalf("the matchable frames differ (baseline order %v)\n%s", base, frameSeqDiff(want, got))
					}
					for _, fast := range []bool{true, false} {
						bw, aw := chained(bf, bn, fast, tc.waits...), chained(af, an, fast, tc.waits...)
						if !slices.Equal(bw, aw) {
							t.Fatalf("a chained wait (fast runner %v) matched differently:\ngateSync %q\nasync    %q", fast, bw, aw)
						}
					}
					for _, f := range af {
						if f.gated && f.sync >= an {
							t.Fatalf("a gated frame carries the token (sync %d)", f.sync)
						}
						if !tc.live && strings.Contains(f.plain, "question 1/1") {
							t.Fatalf("the dropped opening's card flashed up:\n%s", f.plain)
						}
					}
					if last := af[len(af)-1]; tc.live && !strings.Contains(last.plain, "> 2 B") {
						t.Fatalf("the key did not reach the raised card:\n%s", last.plain)
					}
				})
			}
		})
	}
}
