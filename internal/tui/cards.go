package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
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
}

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
// dropped: Cancel answered every request the session was holding and the
// session refuses to park another one for this turn, so a card for it would be
// one nobody could answer.
func (m *Model) pushCard(c card) {
	if m.cardsCancelled {
		return
	}
	m.breakStream()
	m.cards = append(append([]card(nil), m.cards...), c)
	m.help = false
	m.agentPeek = false
	// A card is a question the user has to answer first, so the offer stands
	// down while it is up — but it is not retired. cursor answers a plan-mode
	// turn with a cursor/create_plan card *and* assistant text, so the card
	// always arrives before the ending that arms the offer; killing the offer
	// here meant it could never appear in a real plan-mode session at all.
	// planOffering hides it while a card is open instead.
	// The draft itself is never touched (pinned), only the menu it opened.
	m.slashHide = true
	// A card takes the mouse too, so an in-progress drag is dropped rather
	// than left waiting for a release that will never be handled.
	m.sel = selection{}
	m.pressed = tea.MouseButtonNone
	// Either dialog goes with the rest: the model dialog applies nothing on
	// the way out, and the theme dialog takes its live preview with it.
	*m = m.closeDialog(true)
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

// answerCard sends one answer in the same Update that popped the card, and
// reports whether the session took it.
//
// This is deliberately not a tea.Cmd. Answer* is a hand-off to the handler
// goroutine that is parked on the request — a map delete under the session
// lock and a send on a buffered channel, never an RPC — so it cannot block the
// UI. Doing it later would leave a window in which the card is gone from the
// queue but the request is still in the session's waiting map, and a cancel
// arriving in that window would claim the request and send cursor `cancelled`
// for something the user had already answered.
func (m *Model) answerCard(fn func() error) bool {
	if err := fn(); err != nil {
		m.addError(err.Error())
		return false
	}
	return true
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

// answerPermission picks by option kind, using the request's own optionId.
func (m Model) answerPermission(kind string) (tea.Model, tea.Cmd) {
	c, ok := m.popCard()
	if !ok || c.perm == nil {
		return m, nil
	}
	id := ""
	for _, o := range c.perm.Options {
		if o.Kind == kind {
			id = o.OptionID
			break
		}
	}
	m.answerCard(func() error { return m.sess.AnswerPermission(c.perm.ID, id) })
	return m, nil
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
	// The notes follow the answer the session took, so the transcript cannot
	// claim a question was answered when the reply never reached the agent.
	if !m.answerCard(func() error { return m.sess.AnswerQuestion(c.ask.ID, answers, false) }) {
		return m, nil
	}
	for _, qq := range c.ask.Questions {
		m.addNote("? " + sanitizeLine(qq.Prompt) + " → " + answerLabels(qq, answers[qq.ID]))
	}
	return m, nil
}

// skipQuestion is Esc: the whole request is skipped, not just this question.
func (m Model) skipQuestion() (tea.Model, tea.Cmd) {
	c, ok := m.popCard()
	if !ok || c.ask == nil {
		return m, nil
	}
	if !m.answerCard(func() error { return m.sess.AnswerQuestion(c.ask.ID, nil, true) }) {
		return m, nil
	}
	m.addNote("? " + questionTitle(c.ask) + " → skipped")
	return m, nil
}

// answerLabels names what was picked, for the transcript note.
func answerLabels(q agent.Question, ids []string) string {
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		for _, o := range q.Options {
			if o.ID == id {
				labels = append(labels, sanitizeLine(o.Label))
				break
			}
		}
	}
	if len(labels) == 0 {
		return "nothing"
	}
	return strings.Join(labels, ", ")
}

func questionTitle(ev *agent.QuestionEvent) string {
	if ev == nil {
		return ""
	}
	if t := sanitizeLine(ev.Title); t != "" {
		return t
	}
	if len(ev.Questions) > 0 {
		return sanitizeLine(ev.Questions[0].Prompt)
	}
	return "question"
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
	if !m.answerCard(func() error { return m.sess.AnswerPlan(c.plan.ID, accept) }) {
		return m, nil
	}
	verb := "rejected"
	if accept {
		verb = "accepted"
	}
	m.addNote("plan " + planName(c.plan) + " → " + verb)
	return m, nil
}

func planName(p *agent.PlanEvent) string {
	if p == nil {
		return ""
	}
	if n := sanitizeLine(p.Name); n != "" {
		return n
	}
	return "plan"
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
	return renderSegs(m.width,
		seg{"permission " + sanitizeLine(c.perm.Tool) + "  ", styleFG(m.theme.ChipPrompt)},
		seg{strings.Join(keys, "  "), styleFG(m.theme.Dim)},
	)
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
	rows := []string{renderSegs(inner,
		seg{fmt.Sprintf("question %d/%d  ", c.qIdx+1, len(c.ask.Questions)), styleFG(m.theme.ChipPrompt)},
		seg{sanitizeLine(q.Prompt), styleFG(m.theme.FG)},
	)}
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
		rows = append(rows, renderSegs(inner, seg{fmt.Sprintf("%s %d %s%s", mark, i+1, box, sanitizeLine(o.Label)), st}))
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
	return renderSegs(m.width,
		seg{"plan ", styleFG(m.theme.ChipPrompt)},
		seg{planName(c.plan), styleFG(m.theme.Accent)},
	) + "\n" + renderSegs(m.width,
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
