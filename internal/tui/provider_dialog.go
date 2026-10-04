package tui

import (
	"sync/atomic"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

const (
	providerDialogTitle = "provider"
	providerDialogHint  = "↑↓ · tab · enter starts · esc uses default"
)

// ------------------------------------------------------------ availability

// The TUI's half of provider availability (plan 036 §3.3): the states craze's
// check answers (internal/cli's availability, §3.1), drawn in the TUI's two
// pickers — the startup picker (Config.Availability) and the session list's
// /provider (ProviderAvailabilitySource) — where a provider that is not ready
// is dimmed, says why, and is refused in place when it is chosen. Nothing
// else refuses on it: an explicit --provider, $CRAZE_PROVIDER, the resume
// picker and a create start as they always have (plan 036 decision 3), so a
// misjudged state never locks anyone out. Before the first answer arrives —
// and with no callback at all, as every test Config and golden has — every
// row is ready and a choice is taken as it always was: the states are advice
// (X12).

// AvailState is a provider's availability, in the check's (and the wire's)
// words: ready, needs_setup or unavailable.
type AvailState string

const (
	// AvailReady: the provider can start here.
	AvailReady AvailState = "ready"
	// AvailNeedsSetup: it cannot start until something is set up — native
	// with no model provider that has a key.
	AvailNeedsSetup AvailState = "needs_setup"
	// AvailUnavailable: it cannot start, and nothing in craze can make it.
	AvailUnavailable AvailState = "unavailable"
)

// word is the state as a picker's row says it: "" for ready (or a state
// this build does not know, which is taken as ready), else the state in
// words.
func (s AvailState) word() string {
	switch s {
	case AvailUnavailable:
		return "unavailable"
	case AvailNeedsSetup:
		return "needs setup"
	}
	return ""
}

// ProviderAvail is one provider's availability as craze's check answers it
// (plan 036 §3.1): its id, its state, and for any state but ready why it is
// not (Reason) and what to do about it (Fix), each one line.
type ProviderAvail struct {
	ID     string
	State  AvailState
	Reason string
	Fix    string
}

// ready says a's provider is drawn and chosen as every provider was before
// availability: ready, or with no state this build knows — the zero value,
// a row no answer named, among them.
func (a ProviderAvail) ready() bool { return a.State.word() == "" }

// reason is why a's provider is not ready, as one line — its state in words
// when the answer gave no reason.
func (a ProviderAvail) reason() string {
	if r := sanitizeLine(a.Reason); r != "" {
		return r
	}
	return a.State.word()
}

// fix is what to do about it, as one line ("" for none).
func (a ProviderAvail) fix() string { return sanitizeLine(a.Fix) }

// availRefusalHead is a refusal of p in state a without its fix: `can't
// start <label>: <reason>` — the startup picker's first refusal line (plan
// 036 X22), its fix on the line under it.
func availRefusalHead(p agent.Provider, a ProviderAvail) string {
	return "can't start " + sanitizeLine(p.DisplayName()) + ": " + a.reason()
}

// availRefusal is the words a picker refuses p in state a with (plan 036
// §3.3), whole and on one line: `can't start <label>: <reason> — <fix>` —
// what the startup picker keeps as its refusal (providerErr) and draws on its
// two lines (providerRefusalLines), and /provider's note.
func availRefusal(p agent.Provider, a ProviderAvail) string {
	head := availRefusalHead(p, a)
	if f := a.fix(); f != "" {
		return head + " — " + f
	}
	return head
}

// availVerdict is what choosing a provider in one of the TUI's pickers comes
// to (availVerdictOf).
type availVerdict int

const (
	// availStart: start it, as every choice was started before
	// availability — a ready provider, or one with no answer yet.
	availStart availVerdict = iota
	// availRefuse: refuse it in place — the picker's error row, the
	// popup's note — and start nothing.
	availRefuse
)

// availVerdictOf is the one place a picker decides what choosing a provider
// in state a does: both pickers, every way of choosing (Enter, Esc to the
// default, a click outside the box; Enter, Tab or a typed /provider), ask it.
// unavailable refuses in place; native's needs_setup refuses in place too in
// this build (plan 036 X11), the arm the pre-session connect dialog replaces
// (§3.6) — so it is decided here once, never at the call sites.
func availVerdictOf(a ProviderAvail) availVerdict {
	switch a.State {
	case AvailUnavailable, AvailNeedsSetup:
		return availRefuse
	}
	return availStart
}

// provChoice is one provider a picker offers, with its availability: the
// zero ProviderAvail — ready — when no answer names it.
type provChoice struct {
	p agent.Provider
	a ProviderAvail
}

// refusal is the words choosing c is refused with, "" when it is not
// (availVerdictOf).
func (c provChoice) refusal() string {
	if availVerdictOf(c.a) == availRefuse {
		return availRefusal(c.p, c.a)
	}
	return ""
}

// provAvailSeq numbers every availability call for the process: the startup
// picker's requests, made in the Update (askProviderAvail), and the session
// list's, numbered as the call starts off the Update (sessAvailAnswer). A
// number is never issued twice, so an answer is always told from a later one
// — a later opening's, or a later call's — even across the list's openings,
// whose state is made afresh each time.
var provAvailSeq atomic.Uint64

// providerAvailMsg is Config.Availability's answer for the startup picker,
// stamped with the request it answers (Model.availSeq).
type providerAvailMsg struct {
	seq   uint64
	avail []ProviderAvail
}

// availByID is avail keyed by provider id: the startup picker's states. Never
// nil, so an answer that names nothing is still an answer.
func availByID(avail []ProviderAvail) map[string]ProviderAvail {
	out := make(map[string]ProviderAvail, len(avail))
	for _, a := range avail {
		out[a.ID] = a
	}
	return out
}

// askProviderAvail asks Config.Availability for the startup picker's states
// as the picker opens (plan 036 §3.3): a new request, the only one an answer
// is taken for from now on, run off the Update since the check reads the
// disk. nil with no callback: every row stays ready.
func (m *Model) askProviderAvail() tea.Cmd {
	if m.availability == nil {
		return nil
	}
	m.availSeq = provAvailSeq.Add(1)
	return m.providerAvailCmd()
}

// providerAvailCmd is the call for the request askProviderAvail recorded:
// Init's, since Init cannot record one (New does).
func (m Model) providerAvailCmd() tea.Cmd {
	call, seq := m.availability, m.availSeq
	if call == nil || seq == 0 {
		return nil
	}
	return func() tea.Msg { return providerAvailMsg{seq: seq, avail: call()} }
}

// providerAvailed takes an answer for the startup picker — only the latest
// request's: an earlier one, from an opening since replaced, changes nothing.
func (m Model) providerAvailed(msg providerAvailMsg) Model {
	if msg.seq == m.availSeq {
		m.provAvail = availByID(msg.avail)
	}
	return m
}

// providerChoice is p as the startup picker offers it: with its state from
// the latest answer, ready with none.
func (m Model) providerChoice(p agent.Provider) provChoice {
	return provChoice{p: p, a: m.provAvail[p.Name()]}
}

// providerAnyNotReady says some row of the startup picker is not ready: its
// detail line's row is then kept whatever the selection, so the box does not
// change height as the cursor moves.
func (m Model) providerAnyNotReady() bool {
	for _, p := range m.providers {
		if !m.providerChoice(p).a.ready() {
			return true
		}
	}
	return false
}

// pickerRows settles the picker's list, and tui.New is its only caller: the
// rows are the caller's availability-filtered list — or the built-in
// non-optional set when it is empty, which keeps a hand-built Config hermetic —
// unioned with the resolved default.
//
// The union is the invariant, not a convention the caller honours (§3.4). Esc
// starts providerDefault whatever the list says, so a default that is missing
// from it would be started by a picker that never showed it, never tagged it
// and preselected cursor instead. Order is agent.Providers() order and names
// are deduplicated, so neither a caller's ordering nor an inserted default can
// move the rows around.
func pickerRows(list []agent.Provider, def agent.Provider) []agent.Provider {
	if len(list) == 0 {
		list = agent.DefaultProviders()
	}
	want := make(map[string]agent.Provider, len(list)+1)
	arrived := make([]string, 0, len(list)+1)
	remember := func(p agent.Provider) {
		if _, dup := want[p.Name()]; p.Name() == "" || dup {
			return
		}
		want[p.Name()] = p
		arrived = append(arrived, p.Name())
	}
	for _, p := range list {
		remember(p)
	}
	remember(def)
	rows := make([]agent.Provider, 0, len(arrived))
	for _, p := range agent.Providers() {
		if got, ok := want[p.Name()]; ok {
			rows = append(rows, got)
			delete(want, p.Name())
		}
	}
	// A name agent.Providers() does not list keeps the order it arrived in.
	// That is where a hidden provider lands when it is the resolved default
	// (from --provider, $CRAZE_PROVIDER or config.toml): one tagged row after
	// the listed ones, and never a row otherwise (plan 018 §3.4).
	for _, name := range arrived {
		if got, ok := want[name]; ok {
			rows = append(rows, got)
		}
	}
	return rows
}

// hiddenProvider reports whether id resolves to a provider the registry never
// lists. Such a provider is not written as the default on startedMsg (plan 018
// §3.4). The registry answers rather than the snapshot, which carries the id
// alone.
func hiddenProvider(id string) bool {
	p, err := agent.ProviderByName(id)
	return err == nil && p.Hidden()
}

func (m Model) providerIndex(p agent.Provider) int {
	want := p.Name()
	for i, c := range m.providers {
		if c.Name() == want {
			return i
		}
	}
	return 0
}

// confirmProvider starts p: Enter on a row (explicit), or the default on Esc
// and on a click outside the box. The command line's refusal comes first
// (Config.RefuseLoad), as the resume picker asks it of a row: internal/cli
// checked only the resolved default before the picker opened, and a row it
// never checked — native under --agent-bin — is exactly what `craze --provider
// native --agent-bin X` exits 2 over. A refused provider is the error row and
// nothing else: the picker stays up for another choice, no session is built,
// the current engine is left alone, and — since nothing starts — nothing is
// persisted on startedMsg either.
//
// Availability comes after it (plan 036 §3.3, decision 4), so the command
// line's refusals keep their order: a provider that is not ready is refused
// the same way, in its own words (provChoice.refusal) — Enter on its row,
// and Esc or a click outside the box when it is the default. With every row
// refused every confirmation is; Ctrl+C still quits (handleKey's, ahead of
// the dialog).
func (m Model) confirmProvider(p agent.Provider, explicit bool) (tea.Model, tea.Cmd) {
	if p.Name() == "" {
		p = agent.CursorProvider()
	}
	if m.refuseLoad != nil {
		if err := m.refuseLoad(p); err != nil {
			m.providerErr = err.Error()
			return m, nil
		}
	}
	if refusal := m.providerChoice(p).refusal(); refusal != "" {
		m.providerErr = refusal
		return m, nil
	}
	m.providerErr = ""
	m.pickingProvider = false
	m.dialog = dialogNone
	m.pickedExplicit = explicit
	m.sessProvider = p.Name()
	if m.spawnNew != nil {
		// The launch flow (plan 030 §3.5): the choice is spawned, and the
		// session it answers is adopted when it lands (launch.go). Nothing
		// is running to close — a picker is up only before any session.
		return m, m.spawn(dialogProvider, p, nil, explicit)
	}
	if m.newSession != nil {
		// The ENGINE is closed, not just the session: closing the session alone
		// would leave the old engine's driver running and its last events
		// unpublished, and a swap is the one place two of them could overlap.
		if m.eng != nil {
			_ = m.eng.Close()
		}
		// A NEW session, so a new durable id: nothing is being continued, and
		// the id of whatever this run started with belongs to the session it
		// just closed (session control SD-22).
		m.setSession(m.newSession(p), "")
	}
	if m.eng == nil && m.engErr == nil {
		m.setSession(NewStub(), "")
	}
	m.recompute()
	if m.model == "" && m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
	}
	if m.model == "" {
		m.model = "default"
	}
	// The same batch Init returns, for the same reason: the session this just
	// built may be a load, and its replay is emitted from the client's read
	// loop while Start is still running (§3.5). The read it arms is the one the
	// command gate's reader rule counts (readOn).
	m.reading = m.eng != nil
	return m, tea.Batch(m.startCmd(), waitEvent(m.eng, m.bgen))
}

func (m Model) handleProviderDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.providers)
	if n == 0 {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		return m.confirmProvider(m.providers[m.providerCursor], true)
	case tea.KeyEsc:
		return m.confirmProvider(m.providerDefault, false)
	case tea.KeyUp, tea.KeyShiftTab:
		m.providerCursor = (m.providerCursor - 1 + n) % n
		// A refusal is about the row it was for: once the cursor leaves it,
		// the error row goes.
		m.providerErr = ""
		return m, nil
	case tea.KeyDown, tea.KeyTab:
		m.providerCursor = (m.providerCursor + 1) % n
		m.providerErr = ""
		return m, nil
	}
	return m, nil
}

// providerDialogPlan is the window onto the row list and whether the footer
// survived: title + rows + footer, with the list giving its rows up one at a
// time and the footer going only once even one row no longer fits. The
// renderer and the hit-tester both take it, so the two cannot disagree about
// which rows the frame is showing.
//
// The window follows providerCursor instead of always starting at row 0. A box
// that truncated from the bottom would draw the top of the list while the keys
// — and the Enter that starts a session — were on a row nobody could see, and
// would bound clicks to rows that are not the ones on screen. Three rows and a
// default at the last index is what makes that reachable, but the arithmetic is
// the same one row or ten.
//
// A refusal's error row is laid out the way the resume picker's is: under the
// list and above the footer, and a short box keeps it over the footer but never
// over the last list row. With no refusal the plan is exactly what it was.
//
// With a row that is not ready (plan 036 §3.3, X22) that row is the first of
// two detail lines, both kept whatever the selection so the box keeps one
// height as the cursor moves: the refusal, else the selected row's reason,
// else blank; then the fix that goes with it, else blank. A short box gives
// the fix line up first — before the footer and any list row — and keeps the
// first line as it keeps the error row. With every row ready it is the error
// row alone, as ever.
func (m Model) providerDialogPlan(budget int) (top, shown int, footer bool) {
	rows := budget - 1
	if m.providerLineShown(budget) {
		rows--
	}
	if m.providerFixShown(budget) {
		rows--
	}
	footer = rows >= 2
	if footer {
		rows--
	}
	// dialogListWindow scrolls only as far as the cursor forces, so a list that
	// fits still starts at row 0 and the goldens are unmoved.
	top, shown = dialogListWindow(len(m.providers), m.providerCursor, max(rows, 0))
	return top, shown, footer
}

// providerLineShown is whether the row under the list is drawn — the error
// row, or the first detail line it shares with it — and fits the budget: the
// title, one list row and itself.
func (m Model) providerLineShown(budget int) bool {
	return (m.providerErr != "" || m.providerAnyNotReady()) && budget >= 3
}

// providerFixShown is whether the second detail line — the fix — is drawn:
// with a row that is not ready, and only when the whole box fits beside it
// (the title, every row, the first line and the footer), since it is the
// first row a short box gives up (plan 036 X22).
func (m Model) providerFixShown(budget int) bool {
	return m.providerAnyNotReady() && budget >= len(m.providers)+4
}

// providerRefusalLines is the refusal as the detail lines draw it (X22): a
// row's availability refusal — providerErr is that row's whole refusal —
// split into `can't start <label>: <reason>` and its fix; any other — the
// command line's, a spawn's — is its first line alone. providerErr itself
// stays whole.
func (m Model) providerRefusalLines() (head, fix string) {
	for _, p := range m.providers {
		if c := m.providerChoice(p); c.refusal() != "" && c.refusal() == m.providerErr {
			return availRefusalHead(p, c.a), c.a.fix()
		}
	}
	return sanitizeLine(m.providerErr), ""
}

// providerLines are the rows under the list (providerDialogPlan): the
// refusal in the error colour and the fix that goes with it, dim; else, on a
// row that is not ready, its reason and its fix, dim; else nothing — the rows
// are held for the next one.
func (m Model) providerLines(inner int) (first, fix string) {
	dim := styleFG(m.theme.Dim)
	if m.providerErr != "" {
		head, f := m.providerRefusalLines()
		if f != "" {
			fix = dim.Render(clampWidth(f, inner))
		}
		return styleFG(m.theme.Err).Render(clampWidth(head, inner)), fix
	}
	if n := len(m.providers); n > 0 && m.providerCursor >= 0 && m.providerCursor < n {
		if c := m.providerChoice(m.providers[m.providerCursor]); !c.a.ready() {
			if f := c.a.fix(); f != "" {
				fix = dim.Render(clampWidth(f, inner))
			}
			return dim.Render(clampWidth(c.a.reason(), inner)), fix
		}
	}
	return "", ""
}

// providerTag is the tag column of p's row, whose label is text: the state
// word first, then `default` — `unavailable · default`, `needs setup`,
// `default` — and, where the row is too narrow for both, the state word alone
// (plan 036 §3.3): `default` is dropped first. dialogRow drops whatever tag
// is left when even that does not fit.
func (m Model) providerTag(c provChoice, text string, inner int) string {
	word := c.a.State.word()
	def := ""
	if c.p.Name() == m.providerDefault.Name() {
		def = "default"
	}
	if word == "" || def == "" {
		return word + def
	}
	both := word + " · " + def
	if inner-lipgloss.Width(dialogNoMark+sanitizeLine(text))-lipgloss.Width(both) >= 1 {
		return both
	}
	return word
}

// providerDialogBody draws the window the plan settled. There is no ▲/▼ marker
// the way the theme list has one: the tag column is the "default" tag, and a
// row cannot carry both. A row that is not ready is drawn dim, its state in
// its tag (plan 036 §3.3).
func (m Model) providerDialogBody(inner, budget int) []string {
	top, shown, footer := m.providerDialogPlan(budget)
	rows := []string{m.dialogTitle(providerDialogTitle, inner)}
	for i := top; i < top+shown; i++ {
		c := m.providerChoice(m.providers[i])
		// The row shows the label; the id stays what the tag, the cursor and
		// the started session are matched by.
		text := c.p.DisplayName()
		rows = append(rows, m.dialogRowDim(text, m.providerTag(c, text, inner), i == m.providerCursor, true, !c.a.ready(), inner))
	}
	if m.providerLineShown(budget) {
		first, fix := m.providerLines(inner)
		rows = append(rows, first)
		if m.providerFixShown(budget) {
			rows = append(rows, fix)
		}
	}
	if footer {
		rows = append(rows, m.dialogFooter(providerDialogHint, inner))
	}
	return rows
}

func (m Model) providerDialogClick(i int) (tea.Model, tea.Cmd) {
	top, shown, _ := m.providerDialogPlan(m.lay.Dialog.H - dialogBorder)
	row := i - 1 // the title row
	if row < 0 || row >= shown {
		return m, nil
	}
	if top+row != m.providerCursor {
		// The click moves the cursor, as a key would, and the refusal was
		// about the row it leaves.
		m.providerErr = ""
	}
	m.providerCursor = top + row
	return m, nil
}
