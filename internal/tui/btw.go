package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/transcript"
)

// The side view (/btw, plan 040 §3.6): a side question about the session's
// work, asked without interrupting it and never written into it. `/btw
// <question>` opens a full-window view over the transcript region — the
// status rows, the sub-agent rows, the queue strip and the tasks panel stay
// on screen as the main session's live indicators, inert — and asks the
// session (backend.Aside, session.aside on the wire), whose answer streams
// into the view. The composer stays live in it, for follow-ups; Esc or
// Ctrl+C closes the view and discards the thread. Nothing of it is saved: no
// row reaches the main pane, the journal, the event log or the session's
// history.
//
// # The call
//
// It is no command and not the command gate's (m.run, which freezes the model
// until its reply — unusable for an answer that streams for half a minute):
// askBtwCmd runs backend.Aside on its own goroutine, under a context of its
// own cancelled when the view closes, and the answer reaches Update through a
// delta pump (btwPump). onDelta never blocks — over the socket it runs inline
// on the connection's reader — so it only appends to the pump's pending text
// and pokes a one-slot channel; the call's end parks the result in the pump
// and pokes it too. waitBtw, armed again after each message, takes everything
// pending at once, the text first and then the result, as one btwPumpMsg: the
// result always follows every piece, and at most one pump message is ever
// waiting in the gate's held queue.

// The side view's words.
const (
	btwHeaderNote = "Side question — not saved. Esc returns to the session."
	btwHint       = "Ask a follow-up · esc to return"
	btwStatusChip = "side question · esc to return"
	btwRuleChip   = "btw"
	btwFullNote   = "this side thread is full: Esc and start a new /btw"
	btwUsage      = "usage: /btw <question>"
	btwTextOnly   = "side questions are text only"
	btwNotQueued  = "/btw is a side question, not a queued message: esc the edit, then ask it"
	btwUnstarted  = "/btw needs a running session: send a first message"
	btwLostNote   = "connection lost — ask again"
	btwTooLong    = "this side question is too long"
	btwIncomplete = "(incomplete)"
	// btwTruncated marks a prior answer cut to btwAnswerMax as it is resent:
	// the grok fold's own marker (agent's asideTruncated).
	btwTruncated = "[…truncated]"
)

// The protocol's bounds on a side question (plan 040 §3.2), which the TUI
// keeps its requests within. They are the wire's — protocol.AsideAnswerMax,
// AsidePriorMax and InboundLineMax — restated here because the TUI's seam onto
// a session knows no wire (plan 027 §3.12); TestTheSideViewKeepsTheWiresBounds
// holds them equal.
const (
	// btwAnswerMax bounds each earlier answer resent as prior: a longer one
	// keeps its head and is marked (btwCutAnswer).
	btwAnswerMax = 64 << 10
	// btwPriorMax is how many earlier pairs one question may carry, so a
	// side view takes btwPriorMax+1 questions.
	btwPriorMax = 20
	// btwLineBudget is the host's inbound line limit less 64 KiB of headroom:
	// the encoded request is kept within it, the oldest pairs dropped first.
	btwLineBudget = 4<<20 - 64<<10
	// btwEnvelopeSlack stands for what the request's JSON-RPC envelope, its
	// session id and its aside id add to the params btwEncodedSize counts.
	btwEnvelopeSlack = 1 << 10
)

// btwBuiltin is /btw's row (plan 040 §3.6): in the slash catalog where the
// session answers side questions alone (btwOffered), after the mode rows and
// before /exit (slashCatalog). builtinNamed never names it — like /connect,
// where it is not offered a plugin's or the agent's /btw is what it always
// was — so dispatch has its own gate (handleEnter).
var btwBuiltin = slashItem{Name: "btw", Desc: "Ask a side question (not saved): /btw <question>", Builtin: true}

// btwOffered reports whether this session has /btw: its backend says it
// answers side questions (SessionInfo.Aside) — a native, grok or gx session,
// in process or over the socket. Cursor's, and a host from before the
// method, offer none.
func (m Model) btwOffered() bool { return m.eng != nil && m.info().Aside }

// btwDraft reads draft as /btw: `/btw`, in any case, after leading
// whitespace, then whitespace (newlines included) or nothing. question is the
// rest, trimmed, verbatim. It reads the raw draft rather than
// parseSlashLine's, which collapses the arguments' whitespace and refuses a
// draft of more than one line: a multi-line /btw would otherwise go to the
// agent as a prompt.
func btwDraft(draft string) (question string, ok bool) {
	const name = "/btw"
	s := strings.TrimLeftFunc(draft, unicode.IsSpace)
	if len(s) < len(name) || !strings.EqualFold(s[:len(name)], name) {
		return "", false
	}
	rest := s[len(name):]
	if r, _ := utf8.DecodeRuneInString(rest); rest != "" && !unicode.IsSpace(r) {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// btwState is the side view while it is open: its pane, the thread of
// questions answered in it, and the question in flight. A value written copy
// on write (m.btw is replaced, never edited in place), but for the pane and
// its rows, which are shared by every copy of the model as every pane is.
type btwState struct {
	pane *pane
	// thread is every question answered in this view and its whole answer,
	// oldest first: what the next question carries as prior.
	thread []agent.AsideTurn
	// pending is the question in flight, nil when none is.
	pending *btwCall
	// answer is pending's answer row once its first piece has come, nil
	// before; start is when pending was asked.
	answer *entry
	start  time.Time
}

// btwCall is one side question in flight.
type btwCall struct {
	id       uint64
	question string
	pump     *btwPump
	cancel   context.CancelFunc
}

// stop ends c: its context cancelled, its pump retired. Idempotent.
func (c *btwCall) stop() {
	c.cancel()
	c.pump.retire()
}

// retire stops the question b has in flight, if any. nil-safe.
func (b *btwState) retire() {
	if b == nil || b.pending == nil {
		return
	}
	b.pending.stop()
}

// btwPending reports a side question in flight in the open view. The frame
// runner's capture waits for none (frame.go's settled).
func (m Model) btwPending() bool { return m.btw != nil && m.btw.pending != nil }

// btwDone is how a side question ended: its result, or why not.
type btwDone struct {
	res agent.AsideResult
	err error
}

// btwPump carries a side question's answer from its call's goroutine — and
// from onDelta, which over the socket runs on the connection's reader — to
// Update. Neither ever blocks on it: onDelta appends to the pending text under
// mu and pokes notify, a one-slot channel whose send never waits; the call's
// end parks its result the same way. notify is never closed, so a late
// onDelta after the view has closed is an append into a retired pump, never a
// send on a closed channel. stop is closed once, when the pump is retired.
type btwPump struct {
	notify chan struct{}
	stop   chan struct{}
	once   sync.Once

	mu      sync.Mutex
	retired bool
	text    strings.Builder
	done    *btwDone
}

func newBtwPump() *btwPump {
	return &btwPump{notify: make(chan struct{}, 1), stop: make(chan struct{})}
}

// delta is the call's onDelta: s appended to the pending text, and the pump
// poked. A retired pump keeps nothing.
func (p *btwPump) delta(s string) {
	if s == "" {
		return
	}
	p.mu.Lock()
	if p.retired {
		p.mu.Unlock()
		return
	}
	p.text.WriteString(s)
	p.mu.Unlock()
	p.poke()
}

// finish parks the call's outcome, after every piece it delivered (onDelta
// never runs once the call has returned), and pokes the pump.
func (p *btwPump) finish(d btwDone) {
	p.mu.Lock()
	if !p.retired {
		p.done = &d
	}
	p.mu.Unlock()
	p.poke()
}

// poke wakes waitBtw, without ever waiting: a poke already pending is enough.
func (p *btwPump) poke() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// take is everything pending: the text, then the outcome when it has come.
func (p *btwPump) take() (string, *btwDone) {
	p.mu.Lock()
	defer p.mu.Unlock()
	text := p.text.String()
	p.text.Reset()
	d := p.done
	p.done = nil
	return text, d
}

// retire stops the pump: what is pending and what comes later is dropped, and
// a waitBtw waiting on it returns nothing. Idempotent.
func (p *btwPump) retire() {
	p.once.Do(func() {
		p.mu.Lock()
		p.retired = true
		p.text.Reset()
		p.done = nil
		p.mu.Unlock()
		close(p.stop)
	})
}

// btwPumpMsg is what waitBtw took from the pump of side question id: its
// pending text and, once the call has ended, its outcome. It is stamped with
// the shown session it was asked under, and dropped once another is shown
// (shownStamped); one whose id is not the open view's question in flight is
// dropped where it lands (applyBtwPump).
type btwPumpMsg struct {
	id       uint64
	shownGen uint64
	text     string
	done     *btwDone
}

func (m btwPumpMsg) shownUnder() uint64 { return m.shownGen }

// waitBtw waits for p to have something, and takes all of it at once; a
// retired pump answers nothing.
func waitBtw(p *btwPump, id, shown uint64) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-p.notify:
		case <-p.stop:
			return nil
		}
		text, done := p.take()
		return btwPumpMsg{id: id, shownGen: shown, text: text, done: done}
	}
}

// askBtwCmd is the side question's call: b.Aside on the command's goroutine,
// its pieces and then its outcome into p. A panic in it is re-raised inside
// Update (callPanicMsg), where bubbletea recovers it, as a gated call's is.
func askBtwCmd(ctx context.Context, b backend.Backend, req agent.AsideRequest, p *btwPump) tea.Cmd {
	return func() (msg tea.Msg) {
		defer func() {
			if v := recover(); v != nil {
				msg = callPanicMsg{p: callPanic{value: v, stack: debug.Stack()}}
			}
		}()
		res, err := b.Aside(ctx, req, p.delta)
		p.finish(btwDone{res: res, err: err})
		return nil
	}
}

// ---------------------------------------------------------------- opening

// openBtw is Enter — or Ctrl+L — on a /btw draft where the session offers it
// (handleEnter, strongSendDraft): the view opens on question. An empty
// question is the usage line, and a draft carrying image chips is refused —
// side questions are text only — its draft kept; both are the main pane's
// rows, the view being closed. The pending `!` shell context stays the main
// session's: it is never attached to a side question.
func (m Model) openBtw(question string) (tea.Model, tea.Cmd) {
	if !m.sessionReady() {
		return m, nil
	}
	if question == "" {
		m.clearDraft()
		m.addError(btwUsage)
		return m, nil
	}
	if len(m.images.list) > 0 {
		m.addError(btwTextOnly)
		return m, nil
	}
	m.enterBtw()
	return m.askBtw(question)
}

// enterBtw opens the side view: the main pane's place is kept for the way
// back, the keyboard is the composer's — live and focused, the /btw draft
// consumed, a new composer generation — and the view starts with its header.
func (m *Model) enterBtw() {
	m.storeViewport(m.main)
	m.btw = &btwState{pane: &pane{}}
	m.queueHov = noHover()
	m.composerGen++
	m.clearDraft()
	m.focusComposer()
	m.btwRow(entry{kind: entryNote, text: btwHeaderNote})
	m.setViewportContent(true)
}

// leaveBtw closes the side view: its question in flight is cancelled, its
// thread and a follow-up half typed are discarded, and the main pane comes
// back where it was left (restoreViewport, as leaveView's does). A no-op with
// the view closed.
func (m *Model) leaveBtw() {
	if m.btw == nil {
		return
	}
	m.btw.retire()
	m.btw = nil
	m.composerGen++
	m.clearDraft()
	m.focusComposer()
	m.restoreViewport(m.main)
}

// btwRow is one of the side view's own rows, written to its pane alone —
// never through addNote or addError, which write the main pane.
func (m *Model) btwRow(e entry) { m.btw.pane.appendLocal(e, m.now()) }

// btwEnter is Enter in the side view: a follow-up with the composer's text,
// when no question is in flight (ignored while one is, and on an empty
// composer). A view takes btwPriorMax+1 questions; after that the row says
// so and the composer keeps its text.
func (m Model) btwEnter() (tea.Model, tea.Cmd) {
	if m.btw.pending != nil {
		return m, nil
	}
	q := strings.TrimSpace(m.input.Value())
	if q == "" {
		return m, nil
	}
	if len(m.btw.thread) > btwPriorMax {
		m.btwRow(entry{kind: entryError, text: btwFullNote})
		return m, nil
	}
	m.clearDraft()
	return m.askBtw(q)
}

// askBtw asks question in the open view: its row, then the call, carrying
// the thread so far as prior (btwRequest).
func (m Model) askBtw(question string) (Model, tea.Cmd) {
	b := *m.btw
	m.btwRow(entry{kind: entryUser, text: question})
	req, ok := btwRequest(question, b.thread)
	if !ok {
		m.btwRow(entry{kind: entryError, text: btwTooLong})
		return m, nil
	}
	m.btwSeq++
	// The epoch fence (dispatchCtx): a call that runs once the backend has
	// moved to another session is refused before anything is sent.
	ctx, cancel := context.WithCancel(dispatchCtx(m.eng))
	call := &btwCall{id: m.btwSeq, question: question, pump: newBtwPump(), cancel: cancel}
	b.pending, b.answer, b.start = call, nil, m.now()
	m.btw = &b
	return m, tea.Batch(askBtwCmd(ctx, m.eng, req, call.pump), waitBtw(call.pump, call.id, m.shownGen))
}

// btwRequest is question's request after thread: every earlier pair as
// prior, each answer cut to btwAnswerMax, and the oldest pairs dropped while
// the encoded request is over btwLineBudget (JSON escaping can grow a
// request well past its bytes). ok is false when the question does not fit
// on its own.
func btwRequest(question string, thread []agent.AsideTurn) (agent.AsideRequest, bool) {
	if len(thread) > btwPriorMax {
		thread = thread[len(thread)-btwPriorMax:]
	}
	prior := make([]agent.AsideTurn, len(thread))
	for i, t := range thread {
		prior[i] = agent.AsideTurn{Question: t.Question, Answer: btwCutAnswer(t.Answer)}
	}
	for {
		req := agent.AsideRequest{Question: question, Prior: prior}
		if len(prior) == 0 {
			req.Prior = nil
		}
		if btwEncodedSize(req) <= btwLineBudget {
			return req, true
		}
		if len(prior) == 0 {
			return req, false
		}
		prior = prior[1:]
	}
}

// btwCutAnswer is an answer as it is resent: whole when it is within
// btwAnswerMax, else its head cut on a rune boundary and marked, within it.
func btwCutAnswer(s string) string {
	if len(s) <= btwAnswerMax {
		return s
	}
	cut := btwAnswerMax - len(btwTruncated)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + btwTruncated
}

// btwEncodedSize is what req's session.aside line comes to on the wire: its
// params encoded as the protocol encodes them, and the envelope's share
// (btwEnvelopeSlack).
func btwEncodedSize(req agent.AsideRequest) int {
	type pair struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}
	type params struct {
		Question string `json:"question"`
		Prior    []pair `json:"prior,omitempty"`
	}
	p := params{Question: req.Question}
	for _, t := range req.Prior {
		p.Prior = append(p.Prior, pair{Question: t.Question, Answer: t.Answer})
	}
	b, err := json.Marshal(p)
	if err != nil {
		// Strings always encode; this is unreachable, and an unknown size
		// is no size the host takes.
		return btwLineBudget + 1
	}
	return len(b) + btwEnvelopeSlack
}

// ---------------------------------------------------------------- the answer

// applyBtwPump is a pump message landing: taken only by the open view's
// question in flight that it names — anything else is a question the view
// has let go of, or a view since closed — its text streamed into the answer
// row, and then its outcome settled, or the pump waited on again.
func (m Model) applyBtwPump(msg btwPumpMsg) (Model, tea.Cmd) {
	b := m.btw
	if b == nil || b.pending == nil || b.pending.id != msg.id {
		return m, nil
	}
	if msg.text != "" {
		m.btwStream(msg.text)
	}
	if msg.done == nil {
		return m, waitBtw(b.pending.pump, msg.id, m.shownGen)
	}
	m.settleBtw(*msg.done)
	return m, nil
}

// btwStream appends text to the answer row, which it opens on the first
// piece: an assistant row whose generation (entry.gen, its own streaming
// checkpoint's) holds while its text only grows by appends (the streaming
// render resumes, as the main pane's stream does), and moves when the stream
// cap takes its head (capEntryText).
func (m *Model) btwStream(text string) {
	b := *m.btw
	e := b.answer
	if e == nil {
		b.pane.appendLocal(entry{kind: entryAssistant, gen: 1}, m.now())
		e = b.pane.rows[len(b.pane.rows)-1]
		b.answer = e
	}
	if len(e.text)+len(text) > entryTextCap {
		e.gen++
	}
	e.text = capEntryText(e.text + text)
	e.dirty = true
	b.pane.dirty = true
	m.btw = &b
}

// settleBtw is the question in flight ending. Answered: the answer row holds
// the result's text — authoritative over the pieces streamed — and grows no
// more (gen 0, no checkpoint), the pair joins the thread, and the footer
// names the model and, where the provider counts them, the tokens. Failed:
// whatever streamed is settled the same way and marked incomplete, the error
// row follows, and the thread is unchanged.
func (m *Model) settleBtw(d btwDone) {
	b := *m.btw
	call, e := b.pending, b.answer
	call.stop()
	b.pending, b.answer = nil, nil
	now := m.now()
	// settle is the answer row's last text: it grows no more.
	settle := func(text string) {
		e.text = capEntryText(text)
		e.gen, e.md, e.dirty = 0, nil, true
		b.pane.dirty = true
	}
	switch {
	case d.err != nil:
		if e != nil {
			settle(e.text + "\n\n" + btwIncomplete)
		}
		b.pane.appendLocal(entry{kind: entryError, text: btwErrText(d.err)}, now)
	default:
		text := d.res.Text
		if e == nil {
			b.pane.appendLocal(entry{kind: entryAssistant, text: capEntryText(text)}, now)
		} else {
			settle(text)
		}
		thread := make([]agent.AsideTurn, 0, len(b.thread)+1)
		b.thread = append(append(thread, b.thread...), agent.AsideTurn{Question: call.question, Answer: text})
		if f := btwFooter(d.res); f != "" {
			b.pane.appendLocal(entry{kind: entryNote, text: f}, now)
		}
	}
	m.btw = &b
}

// btwFooter is the dim line under an answer: the model that answered and,
// for a provider that counts them (native), the tokens it spent —
// `Echo · 1.2k in (800 cached) · 40 out`. "" when it says neither.
func btwFooter(r agent.AsideResult) string {
	var bits []string
	if name := sanitizeLine(r.Model); name != "" {
		bits = append(bits, name)
	}
	if u := r.Usage; u != nil {
		bits = append(bits, fmt.Sprintf("%s in (%s cached) · %s out",
			transcript.TokenCount(u.Input), transcript.TokenCount(u.CacheRead), transcript.TokenCount(u.Output)))
	}
	return strings.Join(bits, " · ")
}

// btwErrText is a side question's failure as its error row says it: a call
// a reconnect cut, or one answered for a session the backend has left, is
// `connection lost — ask again` — never resent, the host may have run it —
// and the rest by sentinel, or in the error's own words.
func btwErrText(err error) string {
	switch {
	case errors.Is(err, backend.ErrConnectionLost), errors.Is(err, backend.ErrStaleEpoch):
		return btwLostNote
	case errors.Is(err, engine.ErrAsideBusy):
		return "two side questions are already running on this session — ask again in a moment"
	case errors.Is(err, agent.ErrQueueTextTooLong):
		return btwTooLong
	case errors.Is(err, engine.ErrClosing):
		return closingNote
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "the side question was stopped before it was answered — ask again"
	}
	return sanitizeLine(err.Error())
}

// ---------------------------------------------------------------- keys

// handleBtwKey is the keyboard while the side view is open and nothing above
// it — a card, a dialog, the confirm line — has it (handleKey). Esc closes
// it; Enter asks a follow-up; PgUp/PgDn scroll it; Ctrl+Y copies from it;
// Ctrl+O, Ctrl+T and Ctrl+G are what they are everywhere (a dialog they open
// sits over the view and returns to it). The composer keeps all its text
// editing — the arrows move the cursor in a draft of many lines — and its
// text paste; what would leave the view or reach the main session is
// suppressed: ↑ out of an empty composer into the rows, ← into the session
// list, Ctrl+L, Shift+Tab, an image paste, and the slash and `@` popups (off
// while the view is open, so Tab completes nothing).
func (m Model) handleBtwKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.leaveBtw()
		return m, nil
	case tea.KeyCtrlY:
		return m.copySelectionOrLastReply()
	case tea.KeyCtrlO:
		return m.toggleExpanded()
	case tea.KeyCtrlT:
		return m.cycleTasks()
	case tea.KeyCtrlG:
		return m.openThemePicker(), nil
	case tea.KeyCtrlV:
		return m, m.composerPaste()
	case tea.KeyCtrlL, tea.KeyShiftTab:
		return m, nil
	case tea.KeyPgUp, tea.KeyPgDown:
		m.catchUp()
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	if msg.Type == tea.KeyEnter && !isNewlineKey(msg) {
		return m.btwEnter()
	}
	return m, m.updateComposer(msg)
}

// ---------------------------------------------------------------- chrome

// btwElapsed is how long the question in flight has been asked.
func (m Model) btwElapsed() string {
	if !m.btwPending() || m.frozen {
		return formatElapsed(0)
	}
	return formatElapsed(m.now().Sub(m.btw.start))
}

// btwSpinnerView is the working line while a side question is in flight.
func (m Model) btwSpinnerView() string {
	return renderSegs(m.width,
		seg{m.spinnerGlyph() + " ", styleFG(m.theme.Accent)},
		seg{"Answering · " + m.btwElapsed() + " · esc closes", styleFG(m.theme.Dim)},
	)
}

// btwHelpLines are the side view's keys in /help, where /btw is offered.
func (m Model) btwHelpLines() []helpLine {
	if !m.btwOffered() {
		return nil
	}
	return []helpLine{
		{desc: "side questions"},
		{"/btw question", "ask beside the session, even mid-turn; not saved"},
		{"enter", "in the side view, ask a follow-up"},
		{"esc, ctrl+c", "close the side view; the session goes on"},
	}
}
