package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

const (
	// queueRowsMax is the band's cap; past it the rows end in "… +n more",
	// exactly as the sub-agent rows do.
	queueRowsMax = 3
	// noteLinger is how long a one-line note stays in status row 2. It shares
	// the copy chip's slot, which is the only feedback of its kind craze has.
	noteLinger = 2 * time.Second
	// confirmLine is the one line the composer becomes while a send-now is
	// waiting to be confirmed.
	confirmLine = "cancel the running turn and send? enter · esc"
)

// queueAction is one of the three verbs a row offers. It is also the hover
// hit-test's answer: actionNone means the pointer is on the row but not on a
// button.
type queueAction int

const (
	actionNone queueAction = iota
	actionSendNow
	actionEdit
	actionCancel
)

// queueActions is the right-aligned button strip, in the order it is drawn.
var queueActions = []struct {
	action queueAction
	label  string
}{
	{actionSendNow, "[send now]"},
	{actionEdit, "[edit]"},
	{actionCancel, "[cancel]"},
}

// queueHover is the pointer's position inside the band: which row, and which
// button under it. row is -1 when the pointer is not over the band.
type queueHover struct {
	row    int
	action queueAction
}

func noHover() queueHover { return queueHover{row: -1} }

// strongSend is a send-now waiting for the cancelled turn to settle. There is
// only ever one: a second is refused with a note rather than queued behind
// the first, because two turns cannot both be "the one that replaces this".
//
// A row is held by id and stays in the queue until it actually goes, so a
// send-now that never fires loses nothing and one that does cannot be drained
// a second time. A draft is held as text and stays in the composer, which is
// where the user can still see it.
type strongSend struct {
	text string
	from string
	// seq is the turn this was armed against. A turn that ended some other
	// way — an error, a cancel that beat it — takes the armed send with it
	// rather than firing it into whatever runs next.
	seq int
}

// queueItems is the queue as the band draws it.
func (m Model) queueItems() []agent.QueuedPrompt { return m.snap.Queue }

// queueRowCap is how many rows this frame has room for.
func (m Model) queueRowCap() int {
	if m.lay.Height == 0 {
		return queueRowsMax
	}
	return max(0, m.lay.QueueRows)
}

// visibleQueue is the rows actually drawn.
func (m Model) visibleQueue() []agent.QueuedPrompt {
	items := m.queueItems()
	capN := m.queueRowCap()
	if capN <= 0 || len(items) == 0 {
		return nil
	}
	if len(items) > capN {
		items = items[:capN]
	}
	return items
}

// syncQueue re-finds the selection against the rows this frame will draw. The
// selection is held by id so it survives a row leaving the band above it, and
// an emptied band gives the keyboard back to the composer.
func (m *Model) syncQueue() {
	// A row being edited can leave under the editor — the drain sends the
	// head when the turn settles, and Backspace and a click both remove one.
	// The edit ends rather than saving into a row that is gone.
	if m.queueEdit != "" && !m.queueHasID(m.queueEdit) {
		m.cancelQueueEdit()
		m.note("the message you were editing is gone")
	}
	items := m.visibleQueue()
	if len(items) == 0 {
		// No drawn rows is not an empty queue: degradation takes the band
		// away while the messages are still there, and an edit in progress
		// belongs to a row, not to a band.
		m.queueSel = 0
		m.queueID = ""
		m.queueHov = noHover()
		if m.queueFocus {
			m.focusComposer()
		}
		return
	}
	if m.queueID != "" {
		for i, p := range items {
			if p.ID == m.queueID {
				m.queueSel = i
				m.clampHover(len(items))
				return
			}
		}
	}
	m.queueSel = min(max(m.queueSel, 0), len(items)-1)
	m.queueID = items[m.queueSel].ID
	m.clampHover(len(items))
}

// queueHasID reports whether the whole queue — not just the drawn rows —
// still holds an id.
func (m Model) queueHasID(id string) bool {
	for _, p := range m.queueItems() {
		if p.ID == id {
			return true
		}
	}
	return false
}

// clampHover keeps the pointer's row inside the band when rows are removed
// under it.
func (m *Model) clampHover(n int) {
	if m.queueHov.row >= n {
		m.queueHov = noHover()
	}
}

// focusQueue moves the keyboard from the composer to the band.
func (m *Model) focusQueue(row int) {
	items := m.visibleQueue()
	if len(items) == 0 {
		return
	}
	m.queueFocus = true
	m.agentFocus = false
	m.input.Blur()
	m.queueSel = min(max(row, 0), len(items)-1)
	m.queueID = items[m.queueSel].ID
}

func (m *Model) moveQueue(delta int) {
	items := m.visibleQueue()
	if len(items) == 0 {
		return
	}
	m.queueSel = min(max(m.queueSel+delta, 0), len(items)-1)
	m.queueID = items[m.queueSel].ID
}

// queueSelected is the row the keyboard is on, if any.
func (m Model) queueSelected() (agent.QueuedPrompt, bool) {
	items := m.visibleQueue()
	if !m.queueFocus || len(items) == 0 {
		return agent.QueuedPrompt{}, false
	}
	i := min(max(m.queueSel, 0), len(items)-1)
	return items[i], true
}

// note puts one line in status row 2 for two seconds. It shares the copy
// chip's slot and never drops: a refusal the user cannot see is a refusal
// they will repeat.
func (m *Model) note(text string) {
	m.copyNote = text
	m.copyUntil = m.now().Add(noteLinger)
}

// ---------------------------------------------------------------- the verbs

// queueDraft queues the composer's text during a running turn.
func (m Model) queueDraft(text string) (tea.Model, tea.Cmd) {
	if _, err := m.sess.Queue(text); err != nil {
		// The draft is untouched: a refused message is still the user's to
		// shorten or send later.
		m.note(queueErrNote(err))
		return m, nil
	}
	m.input.SetValue("")
	m.resetSlash()
	m.refreshSnap()
	return m, nil
}

func queueErrNote(err error) string {
	switch {
	case err == nil:
		return ""
	case strings.Contains(err.Error(), "queue is full"):
		return "queue full"
	case strings.Contains(err.Error(), "too long"):
		return "message too long"
	default:
		return sanitizeLine(err.Error())
	}
}

// strongSendDraft is Ctrl+L on the composer: interject where the provider can,
// and send now — with the confirm — where it cannot.
func (m Model) strongSendDraft() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	if m.status != statusWorking {
		// Nothing to be strong about: this is a plain send.
		return m.send()
	}
	if m.caps().Interject {
		return m.interject(text)
	}
	return m.askStrongSend(text, "")
}

// interject merges the draft into the running turn. The transcript entry comes
// from the session's broadcast, not from here: one source per entry.
func (m Model) interject(text string) (tea.Model, tea.Cmd) {
	sess := m.sess
	if err := sess.Interject(context.Background(), text); err != nil {
		m.note(interjectErrNote(err))
		return m, nil
	}
	m.input.SetValue("")
	m.resetSlash()
	return m, nil
}

func interjectErrNote(err error) string {
	switch {
	case errors.Is(err, agent.ErrNotInTurn):
		return "nothing to interject into"
	case errors.Is(err, agent.ErrUnsupported):
		return "this agent cannot interject"
	case errors.Is(err, agent.ErrQueueFull):
		// The turn has taken as many interjections as the queue could hold if
		// it answered none of them; the draft stays in the composer.
		return "too many interjections in this turn"
	default:
		return "interject failed"
	}
}

// askStrongSend raises the confirm. Only one send-now can be pending: a second
// is refused rather than queued behind the first.
func (m Model) askStrongSend(text, from string) (tea.Model, tea.Cmd) {
	if m.strong != nil || m.confirm != nil {
		m.note("send now already pending")
		return m, nil
	}
	m.confirm = &strongSend{text: text, from: from}
	return m, nil
}

// confirmStrongSend is Enter on the confirm: the turn is cancelled and the
// send is armed. Nothing is prompted here — the drain starts it when the
// cancelled turn settles, so nothing races the one-in-flight rule.
//
// Nothing is taken out of the queue and nothing is taken out of the composer:
// until the send actually fires there is still a chance it never will, and
// text that exists in exactly one place cannot be lost by a path that forgot
// to put it back.
func (m Model) confirmStrongSend() (tea.Model, tea.Cmd) {
	pending := m.confirm
	m.confirm = nil
	if pending == nil {
		return m, nil
	}
	if m.status != statusWorking {
		// The turn it was going to replace ended while the question was on
		// screen, so there is nothing to cancel: it is a plain send.
		return m.fireStrongSend(*pending)
	}
	pending.seq = m.turnSeq
	m.strong = pending
	return m.cancelTurn()
}

// declineStrongSend is Esc or any other key on the confirm: nothing was taken
// from anywhere, so there is nothing to put back.
func (m *Model) declineStrongSend() {
	m.confirm = nil
}

// dropStrongSend disarms a send-now that will not fire. The text is wherever
// it was — the composer for a draft, the queue for a row — so nothing is
// restored and nothing is lost.
func (m *Model) dropStrongSend(note string) {
	if m.strong == nil {
		return
	}
	m.strong = nil
	if note != "" {
		m.note(note)
	}
}

// fireStrongSend sends what a confirmed send-now was about. A row leaves the
// queue here and not before, so it is taken exactly once; a draft leaves the
// composer only if it is still the text that was armed.
func (m Model) fireStrongSend(p strongSend) (tea.Model, tea.Cmd) {
	text := p.text
	if p.from != "" {
		taken, ok := m.sess.TakeQueued(p.from)
		if !ok {
			// The row went some other way — the drain sent it, or it was
			// cancelled. There is nothing left to send now.
			m.note("that message has already gone")
			return m, nil
		}
		text = taken.Text
		m.refreshSnap()
	} else if strings.TrimSpace(m.input.Value()) == p.text {
		// The armed text was trimmed; the draft may not have been.
		m.input.SetValue("")
		m.resetSlash()
	}
	if strings.TrimSpace(text) == "" {
		return m, nil
	}
	return m.sendText(text)
}

// ------------------------------------------------------------ the edit mode

// startQueueEdit loads a row into the composer. The row keeps its id and its
// position: an edit is not a cancel plus a re-queue.
func (m *Model) startQueueEdit(p agent.QueuedPrompt) {
	if m.queueEdit == "" {
		// Only the first edit displaces a draft. Moving from one row to
		// another must not overwrite it with the row being left behind.
		m.editDraft = m.input.Value()
	}
	m.queueEdit = p.ID
	m.queueEditPos = m.queueSel
	m.input.SetValue(p.Text)
	m.resetSlash()
	m.focusComposer()
	m.queueFocus = false
}

// saveQueueEdit writes the composer back into the row.
func (m Model) saveQueueEdit() (tea.Model, tea.Cmd) {
	id := m.queueEdit
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		// An emptied edit is a cancel: an empty message is not a message.
		m.sess.Unqueue(id)
		m.finishQueueEdit()
		m.refreshSnap()
		return m, nil
	}
	if err := m.sess.EditQueued(id, text); err != nil {
		m.note(queueErrNote(err))
		return m, nil
	}
	m.finishQueueEdit()
	m.refreshSnap()
	return m, nil
}

// finishQueueEdit leaves edit mode and puts the draft back.
func (m *Model) finishQueueEdit() {
	m.queueEdit = ""
	m.input.SetValue(m.editDraft)
	m.editDraft = ""
	m.resetSlash()
}

// cancelQueueEdit is Esc in edit mode: the row is untouched.
func (m *Model) cancelQueueEdit() { m.finishQueueEdit() }

// queueEditChip is the composer rule's label while a row is being edited.
func (m Model) queueEditChip() string {
	if m.queueEdit == "" {
		return ""
	}
	// The row's number is read now, not at edit time: a drain that sends #1
	// while #2 is being edited makes that row #1.
	pos := m.queueEditPos
	for i, p := range m.snap.Queue {
		if p.ID == m.queueEdit {
			pos = i
			break
		}
	}
	return fmt.Sprintf("editing #%d · enter saves · esc cancels", pos+1)
}

// ------------------------------------------------------------------ the band

func (m Model) queueRowsView() string {
	items := m.visibleQueue()
	if len(items) == 0 {
		return ""
	}
	more := len(m.queueItems()) - len(items)
	sel := -1
	if m.queueFocus {
		sel = min(max(m.queueSel, 0), len(items)-1)
	}
	rows := make([]string, 0, len(items)+1)
	for i, p := range items {
		rows = append(rows, m.queueRow(i, p, i == sel, i == m.queueHov.row))
	}
	if more > 0 {
		rows = append(rows, renderSegs(m.width,
			seg{agentGutterBlank + fmt.Sprintf("… +%d more", more), styleFG(m.theme.Dim)}))
	}
	return strings.Join(rows, "\n")
}

// queueRow is "#n <text>" with the action strip right-aligned, drawn on the
// hovered row and the keyboard's row only. The strip's width is reserved
// before the text is clamped, so the two can never overlap.
func (m Model) queueRow(i int, p agent.QueuedPrompt, selected, hovered bool) string {
	num := fmt.Sprintf("#%d", i+1)
	gutter := seg{agentGutterBlank, styleFG(m.theme.Dim)}
	textStyle := styleFG(m.theme.Dim)
	if selected {
		gutter = seg{agentGutterMark, styleFG(m.theme.Accent)}
		textStyle = styleFG(m.theme.Accent)
	}
	prefix := len(agentGutterBlank) + lipgloss.Width(num)
	var actions []seg
	actionsW := 0
	// The strip is drawn only when it can be right-aligned with a cell of
	// text beside it: a truncated strip would put a button where
	// queueActionAt does not expect one, and a click would land on the wrong
	// verb. minFrameCols leaves room for it, so this is a backstop.
	if (selected || hovered) && m.width >= prefix+queueActionStripWidth()+2 {
		actions = m.queueActionSegs(hovered)
		actionsW = queueActionStripWidth()
	}
	avail := m.width - prefix - 1 - actionsW
	text := ""
	if avail >= 1 {
		text = clampWidth(sanitizeLine(p.Text), avail)
	}
	segs := []seg{gutter, {num, styleFG(m.theme.ToolKind)}}
	used := prefix
	if text != "" {
		segs = append(segs, seg{" " + text, textStyle})
		used += 1 + lipgloss.Width(text)
	}
	if actionsW > 0 {
		if pad := m.width - actionsW - used; pad > 0 {
			segs = append(segs, seg{strings.Repeat(" ", pad), lipgloss.NewStyle()})
		}
		segs = append(segs, actions...)
	}
	return renderSegs(m.width, segs...)
}

// queueActionSegs is the button strip. The button under the pointer is drawn
// in the accent colour so a click has a target that reads.
func (m Model) queueActionSegs(hovered bool) []seg {
	out := make([]seg, 0, len(queueActions)*2)
	for i, a := range queueActions {
		if i > 0 {
			out = append(out, seg{" ", lipgloss.NewStyle()})
		}
		st := styleFG(m.theme.Dim)
		if hovered && m.queueHov.action == a.action {
			st = styleFG(m.theme.Accent)
		}
		out = append(out, seg{a.label, st})
	}
	return out
}

func queueActionStrip() string {
	parts := make([]string, 0, len(queueActions))
	for _, a := range queueActions {
		parts = append(parts, a.label)
	}
	return strings.Join(parts, " ")
}

func queueActionStripWidth() int { return lipgloss.Width(queueActionStrip()) }

// queueActionAt maps a column inside the row to the button under it. The strip
// is right-aligned exactly as queueRow drew it, so the hit test cannot land on
// a button that was not there.
func queueActionAt(x, width int) queueAction {
	start := width - queueActionStripWidth()
	if start < 0 || x < start {
		return actionNone
	}
	off := start
	for _, a := range queueActions {
		w := lipgloss.Width(a.label)
		if x >= off && x < off+w {
			return a.action
		}
		off += w + 1
	}
	return actionNone
}

// ----------------------------------------------------------------- keyboard

// handleStrongSend is Ctrl+L wherever it lands: on a queued row it is that
// row's send now, and on the composer it is the strong send — interject where
// the provider can, send now (with the confirm) where it cannot.
func (m Model) handleStrongSend() (tea.Model, tea.Cmd) {
	if id := m.queueEdit; id != "" {
		// The composer holds a queued row, not a draft: Ctrl+L saves the
		// edit and sends that row now, so the text cannot go out twice
		// (once as a draft and again from the queue).
		tm, _ := m.saveQueueEdit()
		m = tm.(Model)
		if m.queueEdit != "" {
			return m, nil // the save was refused and said why
		}
		for _, p := range m.snap.Queue {
			if p.ID == id {
				return m.sendQueuedNow(p)
			}
		}
		return m, nil // an emptied edit cancelled the row
	}
	if p, ok := m.queueSelected(); ok {
		return m.sendQueuedNow(p)
	}
	return m.strongSendDraft()
}

// sendQueuedNow takes a row out of the queue and asks the confirm. The row
// leaves only once the answer is yes: a row that left and then lost its
// confirm would simply be gone.
func (m Model) sendQueuedNow(p agent.QueuedPrompt) (tea.Model, tea.Cmd) {
	if m.status != statusWorking {
		// Nothing to cancel, so nothing to confirm: the row just goes.
		taken, ok := m.sess.TakeQueued(p.ID)
		if !ok {
			return m, nil
		}
		m.focusComposer()
		m.queueFocus = false
		m.refreshSnap()
		return m.sendText(taken.Text)
	}
	return m.askStrongSend(p.Text, p.ID)
}

// handleQueueKey is the keyboard while the band has it. Anything that is not a
// row key hands the keyboard back to the composer and is then handled as
// usual, so typing never needs a second press.
func (m Model) handleQueueKey(msg tea.KeyMsg) (bool, Model) {
	items := m.visibleQueue()
	if len(items) == 0 {
		m.focusComposer()
		m.queueFocus = false
		return false, m
	}
	sel, _ := m.queueSelected()
	switch msg.Type {
	case tea.KeyUp:
		if m.queueSel <= 0 {
			m.focusComposer()
			m.queueFocus = false
		} else {
			m.moveQueue(-1)
		}
		return true, m
	case tea.KeyDown:
		if m.queueSel < len(items)-1 {
			m.moveQueue(1)
			return true, m
		}
		// Past the last row: the sub-agent rows are the next band down, and
		// the composer when there are none.
		if len(m.visibleAgents()) > 0 {
			m.queueFocus = false
			m.focusRows()
		} else {
			m.focusComposer()
			m.queueFocus = false
		}
		return true, m
	case tea.KeyEnter:
		m.startQueueEdit(sel)
		return true, m
	case tea.KeyBackspace, tea.KeyDelete:
		m.sess.Unqueue(sel.ID)
		m.refreshSnap()
		if len(m.visibleQueue()) == 0 {
			m.focusComposer()
			m.queueFocus = false
		}
		return true, m
	case tea.KeyEsc:
		m.focusComposer()
		m.queueFocus = false
		return true, m
	}
	m.focusComposer()
	m.queueFocus = false
	return false, m
}

// -------------------------------------------------------------------- mouse

// queueHoverAt is the pointer landing inside the band: which row, and which
// button under it.
func (m Model) queueHoverAt(x, y int) queueHover {
	r := m.lay.Region(regionQueue)
	row := r.Row(y)
	if row < 0 || row >= len(m.visibleQueue()) {
		return noHover()
	}
	return queueHover{row: row, action: queueActionAt(x, m.width)}
}

// queueClick runs the verb under the pointer. A click on the row text selects
// it, the way a click on a sub-agent row opens it.
func (m Model) queueClick(x, row int) (tea.Model, tea.Cmd) {
	items := m.visibleQueue()
	if row < 0 || row >= len(items) {
		return m, nil
	}
	p := items[row]
	// The strip is drawn on the hovered row and the keyboard's row only, so
	// only those rows have buttons to hit. Anywhere else the click is the
	// row itself, which is what the user can see there.
	drawn := row == m.queueHov.row || (m.queueFocus && row == m.queueSel)
	m.focusQueue(row)
	if !drawn {
		return m, nil
	}
	switch queueActionAt(x, m.width) {
	case actionSendNow:
		return m.sendQueuedNow(p)
	case actionEdit:
		m.startQueueEdit(p)
		return m, nil
	case actionCancel:
		m.sess.Unqueue(p.ID)
		m.refreshSnap()
		return m, nil
	}
	return m, nil
}

// ------------------------------------------------------------- mouse modes

// queueMouseCmd is the terminal's motion mode following the queue. Hover needs
// motion reports with no button down (1003), which cell motion (1002) does not
// send; neither escape disables the other, so every transition goes through
// tea.DisableMouse. Nothing is issued under --no-mouse: craze asked the
// terminal for no reporting at all and must not start now.
//
// "Rows" means queued messages, not drawn rows: a band degradation took away
// keeps all-motion, and the hover simply has nothing to hit.
func (m *Model) queueMouseCmd() tea.Cmd {
	if !m.mouseEnabled {
		return nil
	}
	want := len(m.queueItems()) > 0
	if want == m.mouseAll {
		return nil
	}
	m.mouseAll = want
	if want {
		return tea.Sequence(tea.DisableMouse, tea.EnableMouseAllMotion)
	}
	return tea.Sequence(tea.DisableMouse, tea.EnableMouseCellMotion)
}
