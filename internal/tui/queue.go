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
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
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

// strongSend is a send-now waiting to be confirmed: the text, and the queued row
// it came from if it came from one. Only the confirm line holds one — the
// send-now it becomes is the engine's armed send, which is what knows the turn it
// was armed against and which of the ways it can be lost this was.
//
// A row is held by id and stays in the queue until it actually goes, so a
// send-now that never fires loses nothing and one that does cannot be drained
// a second time. A draft is held as text and stays in the composer, which is
// where the user can still see it.
type strongSend struct {
	text string
	from string
}

// queueItems is the queue as the band draws it.
func (m Model) queueItems() []agent.QueuedPrompt { return m.queue }

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

// editGoneNote is a queue edit ended because its row left the queue.
const editGoneNote = "the message you were editing is gone"

// syncQueue re-finds the selection against the rows this frame will draw. The
// selection is held by id so it survives a row leaving the band above it, and
// an emptied band gives the keyboard back to the composer.
func (m *Model) syncQueue() {
	// A row being edited can leave under the editor — the drain sends the
	// head when the turn settles, and Backspace and a click both remove one.
	// The edit ends rather than saving into a row that is gone.
	if m.queueEdit != "" && !m.queueHasID(m.queueEdit) {
		m.cancelQueueEdit()
		m.note(editGoneNote)
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

// Enter during a running turn used to call Control.Queue from here (queueDraft).
// It does not any more: Queue never starts a turn and never wakes the driver, so
// choosing it from the model's own view of the session strands the row whenever
// that view lags — the engine has settled, nothing will drain, and the row sits in
// the band. Enter's intent is "send this when you can", which is Submit's queue
// mode, and the engine answers with what it did. Control.Queue is for a caller
// that means queue-ONLY, and the TUI has no such action: its band edits and
// removes rows, it never adds one without meaning to send it.

// queueErrNote is a refused queue verb as one line. The two refusals it names
// are matched by sentinel (plan 027 §3.13, "Errors by sentinel"), not by the
// error's text: over the socket the error is the remote client's
// reconstruction, whose Is answers for the sentinel whatever its text says.
func queueErrNote(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, agent.ErrQueueFull):
		return "queue full"
	case errors.Is(err, agent.ErrQueueTextTooLong):
		return "message too long"
	default:
		return sanitizeLine(err.Error())
	}
}

// strongSendDraft is Ctrl+L on the composer: interject where the provider can,
// and send now — with the confirm — where it cannot.
func (m Model) strongSendDraft() (tea.Model, tea.Cmd) {
	if m.shellMode() {
		// Ctrl+L is the one send that does not go through handleEnter, so shell
		// mode has to refuse it here or a `!` draft would reach the agent by
		// the single key that bypasses the ladder. The draft stays where it is:
		// Enter is how a command runs, and a leading space is how the text goes
		// to the agent instead (plan 022 §3.6).
		return m, nil
	}
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
//
// It goes through the engine, which adds nothing but the door — the refusals are
// the session's own — and it waits: the session's Interject returns once the
// agent has taken the message. So it goes through the command gate (plan 027
// §3.12), with a minute's deadline (interjectDeadline): the text and the command
// id are read in the Update that sends, and the draft's fate is the
// continuation, once the answer says what became of it — cleared with the
// pending shell context only once the turn has taken it. A refusal keeps both,
// because the draft is still in the composer and the output is still what it is
// about (plan 022 §3.6). So does an Interject that did not answer in time
// (ErrNoAnswer): the message may have been sent, and the note says so; the
// draft stays, for the user to judge.
func (m Model) interject(text string) (tea.Model, tea.Cmd) {
	if m.eng == nil {
		return m, nil
	}
	// The composer's own text, so it carries the pending shell context exactly
	// as a send does.
	c := m.nextCmd()
	sent := m.withShellContext(text)
	return m.run(interjectDeadline,
		func(ctx context.Context, b backend.Backend) (any, error) {
			return nil, b.Interject(ctx, c, sent)
		},
		func(m Model, r gateReply) (Model, tea.Cmd) {
			if r.err != nil {
				m.note(interjectErrNote(r.err))
				return m, nil
			}
			m.dropShellContext()
			m.input.SetValue("")
			m.resetSlash()
			return m, nil
		})
}

// noAnswerInterjectNote is an Interject that did not answer in time
// (ErrNoAnswer, §3.12): the command may have run, so the note says the message
// may have been sent, and the draft and its shell context are kept.
const noAnswerInterjectNote = "no answer from the session — the message may have been sent"

// interjectErrNote is a refused Interject as one line, the unanswered one
// among them (submitErrNote's shape).
func interjectErrNote(err error) string {
	switch {
	case errors.Is(err, ErrNoAnswer):
		return noAnswerInterjectNote
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
// is refused rather than queued behind the first, because two turns cannot both
// be "the one that replaces this". The engine refuses a second arm the same way;
// this is the fast path, and the one that keeps the confirm line from going up at
// all.
func (m Model) askStrongSend(text, from string) (tea.Model, tea.Cmd) {
	if m.confirm != nil || m.sendNowPending() {
		m.note("send now already pending")
		return m, nil
	}
	m.confirm = &strongSend{text: text, from: from}
	return m, nil
}

// linkThen is what a caller did once a gated call it made had returned and
// been applied: its own post-call work, which runs in the callee's
// continuation after the callee's own, and where a chain's next link is issued
// (§3.12 "The operation chain").
type linkThen func(m Model) (Model, tea.Cmd)

// linkDone is the caller with nothing left to do once the call has been
// applied: it returned there.
func linkDone(m Model) (Model, tea.Cmd) { return m, nil }

// The queue verbs' notes when their call did not answer in time (ErrNoAnswer,
// §3.12 "On expiry"): the command may have run, so each says what may be so,
// and the band shows the fold's facts.
const (
	noAnswerDisarmNote = "no answer from the session — the send may still be armed"
	noAnswerClearNote  = "no answer from the session — the queue may not have been cleared"
	noAnswerRowNote    = "no answer from the session — the row may have changed"
)

// staleEditNote is a queue edit refused ErrStaleVersion: another client
// changed the row after this edit began, so this edit was not written over it
// (plan 027 §4 behaviour change 9). The draft and the edit are kept, and the
// edit's version is refreshed, so Enter again saves over that change.
const staleEditNote = "the message changed — your edit was not saved; Enter again saves over it"

// unqueueCall is an Unqueue of row id, sent as c, as a gated call: its answer
// is the error alone, and a continuation that succeeded shows the row gone
// from the result (rowAbsent).
func unqueueCall(c engine.Command, id string) gateCall {
	return func(ctx context.Context, b backend.Backend) (any, error) {
		_, err := b.Unqueue(ctx, c, id)
		return nil, err
	}
}

// withdrawSendNow takes back an armed send-now. The text is wherever it was — the
// composer for a draft, the queue for a row — so nothing is restored and nothing
// is lost, and the turn that was cancelled to make room for it simply settles
// into whatever was queued behind it.
//
// The note is written in Disarm's continuation, before any event the command
// caused is applied — the command gate holds them until then (§3.12) — and the
// delta the engine publishes for this same command is then this model's own
// echo and is skipped: apply your own command's effect from its return value,
// skip exactly that effect's echo (§3.4). Which is also why the caller says
// what the note is — Esc owes one, Ctrl+C does not.
//
// The command id is minted in the Update that asks; everything after the call
// — the marker, the note, and the caller's own post-call work (then) — is the
// continuation. A Disarm that did not answer (ErrNoAnswer) sets no marker and
// clears no draft, says the send may still be armed, and the caller's work
// goes on: a Ctrl+C still clears and cancels.
func (m Model) withdrawSendNow(note string, then linkThen) (Model, tea.Cmd) {
	if m.eng == nil {
		return then(m)
	}
	c := m.nextCmd()
	return m.run(gateDeadline,
		func(ctx context.Context, b backend.Backend) (any, error) {
			return nil, b.Disarm(ctx, c)
		},
		func(m Model, r gateReply) (Model, tea.Cmd) {
			m.withdrawn(c, note, r.err)
			return then(m)
		})
}

// withdrawn applies what a Disarm this model sent as c came to: Esc's
// withdrawal, and the first half of clearPending's one call.
func (m *Model) withdrawn(c engine.Command, note string, err error) {
	switch {
	case err == nil:
		// The result, as an overlay until this Disarm's own delta is folded:
		// the send-now is not armed (§3.12's table) — even over an earlier
		// arm's delta still on its way.
		m.noteResult(resultEntry{kind: resultDisarmed, cause: c.Cause()})
		m.disarmed = c.Cause()
		// The arm this took back is gone, so the draft it was holding is
		// nobody's to consume. Disarm refuses when there is nothing armed, so
		// reaching here means the arm really was still waiting — which is what
		// makes clearing the marker safe: an arm that had already fired would
		// have been refused instead, leaving its started free to take the draft
		// it went with.
		m.armedDraft = ""
		if note != "" {
			m.note(note)
		}
	case errors.Is(err, ErrNoAnswer):
		// The outcome is unknown: the arm may still be waiting, or may be gone.
		// Neither marker moves — its own delta, if it went, is then noted as
		// anyone's would be.
		m.note(noAnswerDisarmNote)
	default:
		// Nothing was armed, or the engine is no longer admitting: either way
		// there is nothing to say about a send that is not waiting.
	}
}

// confirmStrongSend is Enter on the confirm: the turn is cancelled and the
// send is armed. Nothing is prompted here — the engine starts it when the
// cancelled turn settles, so nothing races the one-in-flight rule.
//
// Nothing is taken out of the queue and nothing is taken out of the composer:
// until the send actually fires there is still a chance it never will, and
// text that exists in exactly one place cannot be lost by a path that forgot
// to put it back.
//
// The confirm comes down here, before the Submit: it is the Update that
// answered the question, and the gated call's continuation (submit's) has
// nothing of this function's left to run.
func (m Model) confirmStrongSend() (tea.Model, tea.Cmd) {
	pending := m.confirm
	m.confirm = nil
	if pending == nil {
		return m, nil
	}
	mode := engine.SubmitSendNow
	if m.status != statusWorking {
		// The turn it was going to replace ended while the question was on
		// screen, so there is nothing to cancel: it is a plain send. Queue mode
		// is "start now, else queue", which is what that is — and what leaves the
		// text queued rather than refused if a turn has started since.
		mode = engine.SubmitQueue
	}
	if pending.from == "" {
		// The composer's own draft, taking the long way round through the
		// confirm: it carries the shell context as its plain send would, and
		// the context is read now rather than when the question went up,
		// because a command that finished while it was up is context for this
		// message too (plan 022 §3.6).
		return m.submitOwn(pending.text, mode, submitted)
	}
	return m.submit(pending.text, mode, pending.from, submitted)
}

// declineStrongSend is Esc or any other key on the confirm: nothing was taken
// from anywhere, so there is nothing to put back.
func (m *Model) declineStrongSend() {
	m.confirm = nil
}

// ------------------------------------------------------------ the edit mode

// startQueueEdit loads a row into the composer. The row keeps its id and its
// position: an edit is not a cancel plus a re-queue.
//
// What is loaded is the message, not the shell context in front of it: the
// composer is where text is written, and a block the user would have to scroll
// past — and could delete, or corrupt — is not text they wrote. It is held
// aside here and put back by saveQueueEdit, so the row keeps the output it was
// queued with whatever the edit does to the message (plan 022 §3.6).
//
// The row's version is recorded too, as it stands in the band this edit was
// begun from: it is what a save of this edit is checked against, so the edit
// applies only to the row the user loaded — until a refusal has told the user
// the row changed, and refreshed it (saveQueueEdit).
func (m *Model) startQueueEdit(p agent.QueuedPrompt) {
	if m.queueEdit == "" {
		// Only the first edit displaces a draft. Moving from one row to
		// another must not overwrite it with the row being left behind.
		m.editDraft = m.input.Value()
	}
	m.queueEdit = p.ID
	m.queueEditVer = p.Version
	m.queueEditPos = m.queueSel
	block, text := agent.SplitShellContext(p.Text)
	m.queueEditCtx = block
	m.input.SetValue(text)
	m.resetSlash()
	m.focusComposer()
	m.queueFocus = false
}

// saveQueueEdit writes the composer back into the row. The edit is a
// check-and-edit (plan 027 §3.13, "Two-client correctness"): it carries the
// row's version as it was when the edit began (startQueueEdit), and another
// client's edit since — folded here or not — refuses it with ErrStaleVersion
// rather than being silently overwritten. The refusal keeps the edit open with
// the text the user wrote and says the message changed; the band is the
// fold's, which shows the other client's text once its edit is folded. The
// refusal also refreshes the edit's version to the row's as this band shows it
// then, so a second Enter knowingly saves over the change the note told the
// user of — never over one this client's fold had not shown at the refusal: a
// change still on its way then, or one made since, refuses that Enter too, and
// refreshes the version again. A row restored after a refused drain keeps its
// id and its version (plan 027 X1), so an edit begun before that drain still
// applies after it.
//
// The call goes through the command gate (§3.12): the text, the version and
// the command id are read in the Update that saves; the edit's end, the band,
// and the caller's own post-call work (then — Ctrl+L's send of the saved row)
// are the continuation. A save that did not answer (ErrNoAnswer) stays in edit
// mode — the composer still holds the text, so nothing is lost and Enter saves
// again — notes that the row may have changed, and shows the fold's band.
func (m Model) saveQueueEdit(then linkThen) (Model, tea.Cmd) {
	id := m.queueEdit
	text := strings.TrimSpace(m.input.Value())
	if m.eng == nil {
		return then(m)
	}
	c := m.nextCmd()
	if text == "" {
		// An emptied edit is a cancel: an empty message is not a message. The
		// shell context goes with it — it was context for the message that is
		// no longer being sent, not a message of its own. It carries no
		// version: a removal is not a check-and-edit, and Unqueue takes none —
		// the user asked for the row to go, whatever it holds now.
		return m.run(gateDeadline, unqueueCall(c, id),
			func(m Model, r gateReply) (Model, tea.Cmd) {
				if errors.Is(r.err, ErrNoAnswer) {
					return m.saveUnanswered(then)
				}
				m.finishQueueEdit()
				if r.err == nil {
					// The row is gone, from the result, until this Unqueue's own
					// removal is folded (§3.12's table).
					m.noteResult(rowAbsent(c.Cause(), id))
				}
				return then(m)
			})
	}
	edited := m.queueEditCtx + text
	expected := m.queueEditVer
	return m.run(gateDeadline,
		func(ctx context.Context, b backend.Backend) (any, error) {
			return nil, b.EditQueued(ctx, c, id, edited, &expected)
		},
		func(m Model, r gateReply) (Model, tea.Cmd) {
			switch {
			case errors.Is(r.err, ErrNoAnswer):
				return m.saveUnanswered(then)
			case errors.Is(r.err, engine.ErrStaleVersion):
				// Another client changed the row since this edit began: the
				// edit stays open with the user's text, nothing is installed,
				// and the band is the fold's. The edit now begins from the row
				// as this band shows it, so the next Enter saves over the
				// change the note has just told the user of — and only that
				// one: a change this fold has not shown yet refuses it again.
				// A row the band no longer holds keeps nothing to refresh
				// from; syncQueue ends its edit.
				if row, ok := m.queuedRow(id); ok && m.queueEdit == id {
					m.queueEditVer = row.Version
				}
				m.note(staleEditNote)
				return then(m)
			case r.err != nil:
				m.note(queueErrNote(r.err))
				return then(m)
			}
			m.finishQueueEdit()
			// The row holds the new text, from the result, until this edit's
			// own event is folded (§3.12's table) — at the version the engine
			// committed, which the check makes an answer rather than a guess:
			// the save carried expected, so its success says the engine's row
			// was at expected (engine/queue.go:87's check), and the engine's
			// edit adds exactly one in that same locked section
			// (agent/queue.go:145's Version++ in PromptQueue.Edit).
			m.noteResult(resultEntry{kind: resultRowEdited, cause: c.Cause(),
				row: agent.QueuedPrompt{ID: id, Text: edited, Version: expected + 1}})
			return then(m)
		})
}

// saveUnanswered is a save whose call did not answer: the edit stays open with
// the text the user wrote, the note says the row may have changed, and the
// band is the fold's. A row that turns out to be gone ends the edit on its own
// (syncQueue).
func (m Model) saveUnanswered(then linkThen) (Model, tea.Cmd) {
	m.note(noAnswerRowNote)
	m.recompute()
	return then(m)
}

// finishQueueEdit leaves edit mode and puts the draft back. The row's shell
// context is let go here rather than in either caller, so a save and a cancel
// cannot leave a block behind for the next row to be edited to inherit.
func (m *Model) finishQueueEdit() {
	m.queueEdit = ""
	m.queueEditCtx = ""
	m.queueEditVer = 0
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
	for i, p := range m.queue {
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
		// The message the row is, not the shell context queued in front of it:
		// a band of rows all reading "<shell_context>" would say nothing about
		// what is waiting to be sent (plan 022 §3.6).
		_, shown := agent.SplitShellContext(p.Text)
		text = clampWidth(sanitizeLine(shown), avail)
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
		// (once as a draft and again from the queue). A chain of two links
		// (§3.12): the send, and the look for the row it sends, need the
		// save's answer, so they are the save's continuation, which issues
		// the Submit.
		return m.saveQueueEdit(func(m Model) (Model, tea.Cmd) {
			if m.queueEdit != "" {
				return m, nil // the save was refused, or not answered, and said why
			}
			for _, p := range m.queue {
				if p.ID == id {
					tm, cmd := m.sendQueuedNow(p)
					return tm.(Model), cmd
				}
			}
			return m, nil // an emptied edit cancelled the row
		})
	}
	if p, ok := m.queueSelected(); ok {
		return m.sendQueuedNow(p)
	}
	return m.strongSendDraft()
}

// sendQueuedNow sends a queued row now, or asks the confirm when a turn is
// running. The row leaves the queue only once it is actually sent — the engine
// takes it in the same locked section that claims its turn — so a row that lost
// its confirm, or one whose turn could not start, is still where it was.
func (m Model) sendQueuedNow(p agent.QueuedPrompt) (tea.Model, tea.Cmd) {
	if m.status != statusWorking {
		// Nothing to cancel, so nothing to confirm: the row just goes. Where
		// the keyboard goes next depends on Submit's answer, so it is decided
		// in the continuation, after submit has applied that answer.
		return m.submit(p.Text, engine.SubmitQueue, p.ID, func(next Model, res engine.SubmitResult, _ error) (Model, tea.Cmd) {
			if res.Turn == "" {
				// It could not start — the agent is running a turn of its own,
				// or the session did not answer (ErrNoAnswer) — so the row is
				// still queued, as far as this client knows, and the band keeps
				// the keyboard.
				return next, nil
			}
			next.focusComposer()
			next.queueFocus = false
			return next, nil
		})
	}
	return m.askStrongSend(p.Text, p.ID)
}

// handleQueueKey is the keyboard while the band has it. Anything that is not a
// row key hands the keyboard back to the composer and is then handled as
// usual, so typing never needs a second press. A handled key's command is the
// band's own (Backspace's gated Unqueue), which the caller returns.
func (m Model) handleQueueKey(msg tea.KeyMsg) (bool, Model, tea.Cmd) {
	items := m.visibleQueue()
	if len(items) == 0 {
		m.focusComposer()
		m.queueFocus = false
		return false, m, nil
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
		return true, m, nil
	case tea.KeyDown:
		if m.queueSel < len(items)-1 {
			m.moveQueue(1)
			return true, m, nil
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
		return true, m, nil
	case tea.KeyEnter:
		m.startQueueEdit(sel)
		return true, m, nil
	case tea.KeyBackspace, tea.KeyDelete:
		// Where the keyboard goes depends on the band the Unqueue leaves, so
		// it is decided in the continuation.
		next, cmd := m.dropQueuedRow(sel.ID, func(m Model) (Model, tea.Cmd) {
			if len(m.visibleQueue()) == 0 {
				m.focusComposer()
				m.queueFocus = false
			}
			return m, nil
		})
		return true, next, cmd
	case tea.KeyEsc:
		m.focusComposer()
		m.queueFocus = false
		return true, m, nil
	}
	m.focusComposer()
	m.queueFocus = false
	return false, m, nil
}

// dropQueuedRow drops a row the user cancelled from the band — Backspace on it,
// or its [cancel] — through the command gate (§3.12): the command id is minted
// in the Update that asks, and the band the Unqueue leaves and the caller's
// own post-call work (then) are the continuation. A refusal (the row already
// gone) says nothing, as it never has; a call that did not answer says the row
// may have changed, over the fold's band.
func (m Model) dropQueuedRow(id string, then linkThen) (Model, tea.Cmd) {
	if m.eng == nil {
		return then(m)
	}
	c := m.nextCmd()
	return m.run(gateDeadline, unqueueCall(c, id),
		func(m Model, r gateReply) (Model, tea.Cmd) {
			switch {
			case r.err == nil:
				// The row is gone, from the result, until this Unqueue's own
				// removal is folded (§3.12's table).
				m.noteResult(rowAbsent(c.Cause(), id))
			case errors.Is(r.err, ErrNoAnswer):
				m.note(noAnswerRowNote)
			}
			return then(m)
		})
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
		// The focus moved above, in the Update of the click; the band the
		// Unqueue leaves is the continuation's.
		return m.dropQueuedRow(p.ID, linkDone)
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
