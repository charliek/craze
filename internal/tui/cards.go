package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/transcript"
)

// cardKind names the blocking agent request a card answers.
type cardKind int

const (
	cardPermission cardKind = iota
	cardQuestion
	cardPlan
)

// Permission option kinds, spelled as cursor sends them. A pick is by kind and
// always carries the request's own optionId; craze never invents one. The
// strings live here because internal/tui does not import internal/acp.
const (
	kindAllowOnce   = "allow_once"
	kindAllowAlways = "allow_always"
	kindRejectOnce  = "reject_once"
)

// card is one blocking request the agent is waiting on. The queue is FIFO and
// only the head is drawn; the rest stay queued in order. A card is taken out
// of the queue in the same Update that builds the command answering it, so
// every card answers exactly once.
type card struct {
	kind cardKind
	perm *agent.PermissionEvent
	ask  *agent.QuestionEvent
	plan *agent.PlanEvent

	// Question progress: the question on screen, the option cursor, the
	// toggles of a multi-select, and the answers gathered so far. The answer
	// is sent after the last question.
	qIdx    int
	optSel  int
	picked  []bool
	answers map[string][]string

	// truncated says the card was raised from a snapshot that carried only
	// the head of some text of its ask (transcript.Ask.Truncated, over the
	// snapshot's ItemCap): the card says so (truncatedTag). A card raised from
	// its opening event never is.
	truncated bool
}

// truncatedTag is what a card raised from a cut-short snapshot says of itself
// (card.truncated), beside what it names.
const truncatedTag = "(truncated)"

// question is the one question the card is showing.
func (c card) question() agent.Question {
	if c.ask == nil || c.qIdx < 0 || c.qIdx >= len(c.ask.Questions) {
		return agent.Question{}
	}
	return c.ask.Questions[c.qIdx]
}

func (c card) isPicked(i int) bool { return i >= 0 && i < len(c.picked) && c.picked[i] }

// toggle flips one option of a multi-select. The toggles are reallocated
// rather than written through, because a Model copy shares the slice.
func (c card) toggle(i int) card {
	q := c.question()
	picked := make([]bool, len(q.Options))
	copy(picked, c.picked)
	if i >= 0 && i < len(picked) {
		picked[i] = !picked[i]
	}
	c.picked = picked
	return c
}

// selectedIDs are the option ids the current question has picked, in request
// order. Ids the request did not offer are never invented: these come from the
// request's own options.
func (c card) selectedIDs() []string {
	q := c.question()
	out := make([]string, 0, len(q.Options))
	for i, o := range q.Options {
		if c.isPicked(i) && o.ID != "" {
			out = append(out, o.ID)
		}
	}
	return out
}

// ---------------------------------------------------------------- the queue

// cardOpen reports whether a blocking card owns the keyboard and the mouse.
func (m Model) cardOpen() bool { return len(m.cards) > 0 }

func (m Model) headCard() (card, bool) {
	if len(m.cards) == 0 {
		return card{}, false
	}
	return m.cards[0], true
}

// pushCard queues a blocking request. The lower overlays and the modal layer
// close on arrival (§3.11) — the theme dialog takes its live preview back out
// with it, or the preview would survive underneath the card.
//
// A card event that was already on its way when the turn was cancelled is
// dropped: Cancel answered every request the session was holding, so a card for
// it would be one nobody could answer. **What decides that is the ask itself,
// not the mask alone** (maskCards, maskDrops).
//
// While the mask is up, that decision is a read of the session's ask registry,
// which over the socket is a round trip (asks.get), so it goes through the
// command gate (plan 027 §3.12, §3.13 "Live reads that decide behaviour"): the
// read is the gated call, and the card's push — or its drop — is the
// continuation, with everything after it: the rest of the event's arm in
// reduceEvent, which for the three card arms is nothing more, and the rest of
// the event's Update (the gate's apply: its reader and the wrapper), which the
// release runs after the continuation. This is the one gate an event opens;
// its command goes back through applyEvent's. The mask down, or nothing to
// consult, and nothing is read: today's path, in this Update.
//
// The run above the card has already ended: the shared model's fold ends it at
// every non-Auto opening, at the opening's At, whether or not this client
// raises a card for it (plan 024 §3.3).
func (m *Model) pushCard(c card) tea.Cmd {
	if !m.cardMasking {
		m.placeCard(c)
		return nil
	}
	id := cardAskID(c)
	if m.eng == nil || id == "" {
		// Nothing to consult: the mask drops it, which is what it did for
		// everything before.
		return nil
	}
	var cmd tea.Cmd
	*m, cmd = m.run(gateDeadline, maskRead(id), func(m Model, r gateReply) (Model, tea.Cmd) {
		if !maskDrops(r) {
			m.placeCard(c)
		}
		return m, nil
	})
	return cmd
}

// placeCard is pushCard's push: the card joins the queue, and the rest of the
// UI makes way for it.
func (m *Model) placeCard(c card) {
	m.cards = append(append([]card(nil), m.cards...), c)
	m.makeWayForCard()
}

// makeWayForCard is what a card's arrival does to the rest of the UI: a
// pushed card's (placeCard), and a restore's for the cards it raises
// (restore.go).
func (m *Model) makeWayForCard() {
	// A card is a question the user has to answer first, so the offer stands
	// down while it is up — but it is not retired. cursor answers a plan-mode
	// turn with a cursor/create_plan card *and* assistant text, so the card
	// always arrives before the ending that arms the offer; killing the offer
	// here meant it could never appear in a real plan-mode session at all.
	// planOffering hides it while a card is open instead. The slash menu needs
	// nothing here at all: slashMenuOpen gates on composerCovered(), so the
	// menu is suspended rather than closed and the draft it was completing is
	// never touched (pinned).
	//
	// A card takes the mouse too, so an in-progress drag is dropped rather
	// than left waiting for a release that will never be handled.
	m.sel = selection{}
	m.pressed = tea.MouseButtonNone
	m.queueHov = noHover()
	// A card owns the keyboard, so a confirm waiting for Enter cannot stay on
	// screen behind it. It is discarded rather than remembered: the answer
	// the user was about to give was about a turn this card is now part of.
	// The draft was never touched, so there is nothing to restore.
	if m.confirm != nil {
		m.confirm = nil
		m.note("send now dropped")
	}
	// Either dialog goes with the rest: the model dialog applies nothing on
	// the way out, and the theme dialog takes its live preview with it. The
	// pre-session connect dialog stays (plan 036 §3.6): it is over the list
	// the session was left for, not over the session, which the card waits
	// in until the list goes back to it — as a card that arrives under the
	// list does.
	if !m.preConnectOpen() {
		*m = m.closeDialog(true)
	}
}

// askRead is the mask's read of one ask (maskRead): its record, and whether
// the session knows it.
type askRead struct {
	rec   agent.AskRecord
	known bool
}

// maskRead is the mask's gated call: the ask's record, read from the session's
// registry (Backend.Ask) — waiting on nothing in process, a round trip over
// the socket.
func maskRead(id string) gateCall {
	return func(ctx context.Context, b backend.Backend) (any, error) {
		rec, known, err := b.Ask(ctx, id)
		return askRead{rec: rec, known: known}, err
	}
}

// maskDrops reports whether the cancel mask swallows an opening, from the
// gated read of its ask (maskRead, pushCard). The mask being up is not enough:
// an opening is dropped only when **its ask is no longer open**.
//
// The registry's state leads its events, and an ask only ever goes open →
// resolved, so the two answers are both final. "Not open now" means the ending
// is already in the FIFO behind this opening and the card would be removed a
// moment later anyway — that is the flash the mask exists to prevent. "Open
// now" means the agent is really waiting on it: it belongs to no turn this
// cancel touched, and dropping it would strand the provider on a card nobody
// can ever raise again.
//
// A read that failed, or never answered (ErrNoAnswer, the gate's deadline),
// says nothing about the ask — "the read failed" is not "known closed" (§3.12,
// astra r2 12) — so the card is shown: stranding a live ask is the one outcome
// the mask must never produce, and a card whose ask the cancel had already
// resolved goes with its ending when that arrives.
func maskDrops(r gateReply) bool {
	a, ok := r.result.(askRead)
	if r.err != nil || !ok {
		return false
	}
	return !a.known || a.rec.Status != agent.AskOpen
}

// setHead replaces the visible card. The queue is copied rather than written
// through, because every Model copy shares the slice.
func (m *Model) setHead(c card) {
	if len(m.cards) == 0 {
		return
	}
	next := append([]card(nil), m.cards...)
	next[0] = c
	m.cards = next
}

func (m *Model) popCard() (card, bool) {
	if len(m.cards) == 0 {
		return card{}, false
	}
	c := m.cards[0]
	m.cards = append([]card(nil), m.cards[1:]...)
	return c, true
}

// answerCard answers one ask whose card the caller has just popped. It writes
// no row of its own: the answer's outcome note — a question's picks, its skip,
// a plan's verb — is the shared transcript's, drawn by the fold from the
// ending the answer causes, for this client and every other alike (plan 032
// §3.2 C4).
//
// The answer goes through the command gate (plan 027 §3.12): the command id is
// minted in the Update that popped the card, the call runs off it, and
// everything after the call — the card raised again, the error row — is the
// continuation. There is a window between the pop and the answer, in which the
// card is gone from the queue but the ask is still open; this client's own keys
// cannot reach into it, because the gate holds every message until the reply,
// so the only thing that can race it is another client's command — a cancel
// above all. The engine resolves that race atomically: Control.Answer
// validates and claims the ask in one registry section, so exactly one of the
// two takes it, and a cancel that won answers this with
// agent.ErrAlreadyResolved — the card is raised again and the cancel's ending
// removes it — never with the agent sent `cancelled` for something the user
// had already answered.
//
// The card is popped before the answer is sent, so **every refusal that leaves
// the ask unanswered puts it back at the head**; otherwise the agent waits for
// an Esc because craze mis-addressed one call, or because the log was busy for a
// moment:
//
//   - agent.ErrBadAnswer: the answer does not fit, and the ask is still open.
//     The card comes back and the error says why, so the user can press again.
//   - agent.ErrAskUnavailable: the outbox is over its bound, so Answer refused
//     **before it mutated anything** (§3.3's rejectable admissions). The ask is
//     untouched and no second opening is ever published for it, so nothing but
//     this would raise the card again. Same treatment,
//     same row: pressing again is exactly the retry it asks for.
//   - agent.ErrAlreadyResolved: another client answered it first and that
//     ending is already queued. The card was on screen, so the model has not
//     applied it yet: the card goes back with **no error row**, and the winner's
//     ending removes it (applyAskEnded) and its fold draws the winner's answer —
//     where before the row was lost altogether and an error the user could do
//     nothing about was written instead.
//   - ErrNoAnswer: no answer in time, so the outcome is unknown — and "unknown"
//     is not "known closed" (§3.12, astra r2 12). The card comes back, as for
//     ErrAskUnavailable, with the note that says so: pressing again is the
//     retry, and if the answer did land after all, its ending removes the card
//     as any ending does.
//
// Every other failure is the error row it has always been — the ask is gone
// either way, so there is no card to restore.
func (m Model) answerCard(popped card, id string, a agent.AskAnswer) (Model, tea.Cmd) {
	if m.eng == nil {
		return m, nil
	}
	c := m.nextCmd()
	return m.run(gateDeadline,
		func(ctx context.Context, b backend.Backend) (any, error) {
			return nil, b.Answer(ctx, c, id, a)
		},
		func(m Model, r gateReply) (Model, tea.Cmd) {
			m.answered(popped, r.err)
			return m, nil
		})
}

// noAnswerCardNote is a card's answer that did not answer in time (ErrNoAnswer,
// §3.12): the card is back, and the note says to press again.
const noAnswerCardNote = "no answer from the session — try again"

// answered is answerCard's continuation: what the answer came to, applied to
// the model as the Update that popped the card left it, before any event the
// answer caused. A taken answer leaves nothing to do: the card is already
// gone, and the ending it causes — which finds no card to remove — draws its
// note through the fold (applyAskEnded).
func (m *Model) answered(popped card, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, ErrNoAnswer):
		m.raiseCard(popped)
		m.addNote(noAnswerCardNote)
		return
	case errors.Is(err, agent.ErrAlreadyResolved):
		m.raiseCard(popped)
		return
	case errors.Is(err, agent.ErrBadAnswer), errors.Is(err, agent.ErrAskUnavailable):
		m.raiseCard(popped)
	}
	m.addError(err.Error())
}

// raiseCard puts a card back at the head of the queue, where it was before it
// was popped. It is not pushCard: nothing about the rest of the UI changed, and
// the card was already through all of that when it first arrived.
func (m *Model) raiseCard(c card) {
	m.cards = append([]card{c}, m.cards...)
}

// ------------------------------------------------------------------- keys

// handleCardKey gives the head card the keyboard. Ctrl+C and Ctrl+D are taken
// before this, and everything else — Ctrl+T, Ctrl+G, Ctrl+O, the arrow-key
// row selection — falls through to the card and is ignored unless the card
// binds it.
func (m Model) handleCardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	c, ok := m.headCard()
	if !ok {
		return m, nil
	}
	switch c.kind {
	case cardQuestion:
		return m.handleQuestionKey(c, msg)
	case cardPlan:
		return m.handlePlanKey(c, msg)
	default:
		return m.handlePermissionKey(c, msg)
	}
}

// handlePermissionKey keeps the pinned line: allow once, allow always, reject
// once — each one bound only while the request actually offers that kind, so a
// key that is not on the line does nothing. Esc cancels the turn, as always.
func (m Model) handlePermissionKey(c card, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyEsc {
		return m.cancelTurn()
	}
	kind := ""
	switch msg.String() {
	case "a", "y", "1":
		kind = kindAllowOnce
	case "A":
		kind = kindAllowAlways
	case "n", "r", "2":
		kind = kindRejectOnce
	}
	if kind == "" || !permissionOffers(c.perm, kind) {
		return m, nil
	}
	return m.answerPermission(kind)
}

// answerPermission picks by option kind, using the request's own optionId. A
// kind the request never offered is the explicit cancel — the empty option id
// that used to mean one — because an empty id is not an option and must not be
// able to cancel a request by accident (agent.AskAnswer). Only the offered
// kinds are bound, so this is unreachable from the keyboard; it is what the
// answer means if it ever is reached.
func (m Model) answerPermission(kind string) (tea.Model, tea.Cmd) {
	c, ok := m.popCard()
	if !ok || c.perm == nil {
		return m, nil
	}
	a := agent.AskAnswer{Cancel: true}
	if id, offered := agent.OptionIDForKind(c.perm.Options, kind); offered {
		a = agent.AskAnswer{OptionID: id}
	}
	return m.answerCard(c, c.perm.ID, a)
}

func permissionOffers(p *agent.PermissionEvent, kind string) bool {
	if p == nil {
		return false
	}
	for _, o := range p.Options {
		if o.Kind == kind && o.OptionID != "" {
			return true
		}
	}
	return false
}

// handleQuestionKey walks the questions one at a time: a number or Enter picks
// and advances a single-select, a number or Space toggles a multi-select and
// Enter confirms it. Esc skips the whole request.
func (m Model) handleQuestionKey(c card, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	q := c.question()
	n := len(q.Options)
	if n == 0 {
		return m.skipQuestion()
	}
	switch msg.Type {
	case tea.KeyEsc:
		return m.skipQuestion()
	case tea.KeyUp:
		c.optSel = (c.optSel - 1 + n) % n
	case tea.KeyDown:
		c.optSel = (c.optSel + 1) % n
	case tea.KeyEnter:
		return m.commitQuestion(c)
	default:
		s := msg.String()
		switch {
		case s == " ":
			if !q.AllowMultiple {
				return m, nil
			}
			c = c.toggle(c.optSel)
		case len(s) == 1 && s[0] >= '1' && s[0] <= '9':
			i := int(s[0] - '1')
			if i >= n {
				return m, nil
			}
			c.optSel = i
			if !q.AllowMultiple {
				return m.commitQuestion(c)
			}
			c = c.toggle(i)
		default:
			return m, nil
		}
	}
	m.setHead(c)
	return m, nil
}

// commitQuestion stores the answer to the question on screen and moves on; the
// request is answered once the last question has one.
func (m Model) commitQuestion(c card) (tea.Model, tea.Cmd) {
	q := c.question()
	answers := make(map[string][]string, len(c.answers)+1)
	for k, v := range c.answers {
		answers[k] = v
	}
	if q.AllowMultiple {
		answers[q.ID] = c.selectedIDs()
	} else if c.optSel >= 0 && c.optSel < len(q.Options) {
		answers[q.ID] = []string{q.Options[c.optSel].ID}
	}
	c.answers = answers
	if c.qIdx+1 < len(c.ask.Questions) {
		c.qIdx++
		c.optSel = 0
		c.picked = nil
		m.setHead(c)
		return m, nil
	}
	if _, ok := m.popCard(); !ok {
		return m, nil
	}
	// The notes are the fold's, drawn from the ending the session publishes
	// once it took the answer, so the transcript cannot claim a question was
	// answered when the reply never reached the agent.
	return m.answerCard(c, c.ask.ID, agent.AskAnswer{Answers: answers})
}

// skipQuestion is Esc: the whole request is skipped, not just this question.
func (m Model) skipQuestion() (tea.Model, tea.Cmd) {
	c, ok := m.popCard()
	if !ok || c.ask == nil {
		return m, nil
	}
	return m.answerCard(c, c.ask.ID, agent.AskAnswer{Skip: true})
}

// handlePlanKey is accept, reject, or Esc. Esc cancels the turn, which is how
// the plan request reaches cursor as `cancelled`: the session answers every
// request it is still holding.
func (m Model) handlePlanKey(_ card, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyEsc {
		return m.cancelTurn()
	}
	switch msg.String() {
	case "a", "y", "1":
		return m.answerPlan(true)
	case "r", "n", "2":
		return m.answerPlan(false)
	}
	return m, nil
}

func (m Model) answerPlan(accept bool) (tea.Model, tea.Cmd) {
	c, ok := m.popCard()
	if !ok || c.plan == nil {
		return m, nil
	}
	return m.answerCard(c, c.plan.ID, agent.AskAnswer{Accept: accept, Reject: !accept})
}

// ------------------------------------------------------------------- views

// modalBandView draws the head card into the rows degradation left it. A card
// the band cannot hold gives up its border first (degradation step 7 crops the
// modal), so the rows that survive are the question and its options rather
// than the top of a box.
func (m Model) modalBandView(lay frameLayout) string {
	band := lay.Region(regionModal).Height()
	return m.modalCardView(band > 0 && band < m.modalRows())
}

// modalView is the card band at its natural height: only the head card draws,
// which is what makes the rest of the stack "suspended, not drawn".
func (m Model) modalView() string { return m.modalCardView(false) }

func (m Model) modalCardView(cropped bool) string {
	c, ok := m.headCard()
	if !ok {
		return ""
	}
	switch c.kind {
	case cardQuestion:
		body := m.questionCardBody(c)
		if cropped {
			return body
		}
		return m.cardBox(body)
	case cardPlan:
		return m.planCardView(c)
	default:
		return m.permissionView(c)
	}
}

func (m Model) modalRows() int {
	v := m.modalView()
	if v == "" {
		return 0
	}
	return lipgloss.Height(v)
}

// permissionView is the pinned one-row line, in its pinned position above the
// status rows. [A]lways appears only when the request offers allow_always.
func (m Model) permissionView(c card) string {
	if c.perm == nil {
		return ""
	}
	// Only the kinds the request offered are drawn, because only those are
	// bound: an action craze cannot take must not look available.
	var keys []string
	for _, k := range []struct{ kind, label string }{
		{kindAllowOnce, "[a]llow once"},
		{kindAllowAlways, "[A]lways"},
		{kindRejectOnce, "[n] reject"},
	} {
		if permissionOffers(c.perm, k.kind) {
			keys = append(keys, k.label)
		}
	}
	if len(keys) == 0 {
		// A request offering none of them can only be got rid of by cancelling.
		keys = append(keys, "esc cancel")
	}
	segs := []seg{{"permission " + sanitizeLine(c.perm.Tool) + "  ", styleFG(m.theme.ChipPrompt)}}
	if c.truncated {
		segs = append(segs, seg{truncatedTag + "  ", styleFG(m.theme.Dim)})
	}
	segs = append(segs, seg{strings.Join(keys, "  "), styleFG(m.theme.Dim)})
	return renderSegs(m.width, segs...)
}

// questionCardBody is one question at a time: its position in the request, the
// options under a `>` cursor, and the keys that answer it. cardBox puts the
// pinned border around it.
func (m Model) questionCardBody(c card) string {
	if c.ask == nil || len(c.ask.Questions) == 0 {
		return ""
	}
	q := c.question()
	inner := max(1, m.width-4)
	head := []seg{{fmt.Sprintf("question %d/%d  ", c.qIdx+1, len(c.ask.Questions)), styleFG(m.theme.ChipPrompt)}}
	if c.truncated {
		head = append(head, seg{truncatedTag + "  ", styleFG(m.theme.Dim)})
	}
	head = append(head, seg{sanitizeLine(q.Prompt), styleFG(m.theme.FG)})
	rows := []string{renderSegs(inner, head...)}
	for i, o := range q.Options {
		mark, st := " ", styleFG(m.theme.FG)
		if i == c.optSel {
			mark, st = ">", styleFG(m.theme.Selection)
		}
		box := ""
		if q.AllowMultiple {
			box = "[ ] "
			if c.isPicked(i) {
				box = "[x] "
			}
		}
		prefix := fmt.Sprintf("%s %d %s", mark, i+1, box)
		rows = append(rows, renderSegs(inner, seg{prefix + sanitizeLine(o.Label), st}))
		// A description goes UNDER its label, dimmed, indented to where the
		// label starts (plan 023 §3.4). Beside it the two would compete for a
		// narrow card's width and the label — the thing a number picks — would
		// be the one clamped; under it, the description is the part that
		// clamps, and an option that has none draws exactly the row it always
		// did. Claude Code's AskUserQuestion is the one source of these
		// (native's ask_user_question); an ACP question carries none, so no
		// cursor or grok frame moves.
		if desc := sanitizeLine(o.Description); desc != "" {
			rows = append(rows, renderSegs(inner, seg{strings.Repeat(" ", lipgloss.Width(prefix)) + desc, styleFG(m.theme.Dim)}))
		}
	}
	hint := "1-9 pick · ↑↓ move · enter select · esc skip"
	if q.AllowMultiple {
		hint = "1-9 or space toggle · ↑↓ move · enter confirm · esc skip"
	}
	rows = append(rows, renderSegs(inner, seg{hint, styleFG(m.theme.Dim)}))
	return strings.Join(rows, "\n")
}

// planCardView is two lines: the plan itself is in the transcript, so the card
// only has to offer the three answers.
func (m Model) planCardView(c card) string {
	if c.plan == nil {
		return ""
	}
	head := []seg{{"plan ", styleFG(m.theme.ChipPrompt)}}
	if c.truncated {
		head = append(head, seg{truncatedTag + " ", styleFG(m.theme.Dim)})
	}
	head = append(head, seg{transcript.PlanName(c.plan), styleFG(m.theme.Accent)})
	return renderSegs(m.width, head...) + "\n" + renderSegs(m.width,
		seg{"[a]ccept  [r]eject  esc cancel", styleFG(m.theme.Dim)},
	)
}

func (m Model) cardBox(body string) string {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.theme.Accent).
		Width(max(1, m.width-2)).
		Render(body)
}
