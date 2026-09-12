package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

// ctrlCWindow is how long a Ctrl+C that cancelled a turn stays armed; a second
// press inside it quits.
const ctrlCWindow = time.Second

// stopCancelled is the one stop reason the TUI reads. internal/tui never
// imports internal/acp, so the string is spelled here.
const stopCancelled = "cancelled"

// wheelLines is how far one wheel notch scrolls the transcript.
const wheelLines = 3

type status int

const (
	statusIdle status = iota
	statusWorking
	statusError
)

func (s status) String() string {
	switch s {
	case statusWorking:
		return "working"
	case statusError:
		return "error"
	default:
		return "idle"
	}
}

type Config struct {
	Session   agent.Session
	Theme     string
	Workspace string
	Model     string
	Yolo      bool
	// NoMouse turns mouse reporting off, which gives the terminal its native
	// drag-select back.
	NoMouse bool
}

type Model struct {
	theme Theme
	vp    viewport.Model
	input textarea.Model

	sess   agent.Session
	cwd    string
	model  string
	yolo   bool
	status status
	err    string
	// startErr is the session that never came up. It is the only error that
	// reaches craze's exit status: Run returns it once the program is over.
	startErr error

	// git is found once, at start; branch is re-read when a turn ends.
	git    gitInfo
	branch string
	// sessStart is when Start returned, which is what the status row's
	// elapsed counts from.
	sessStart time.Time

	entries  []entry
	expanded bool
	trimmed  bool
	renders  int
	width    int
	height   int
	ready    bool
	quitting bool
	started  bool

	// transcriptRows is exactly what setViewportContent handed the viewport,
	// and transcriptPlain is the same rows stripped and right-trimmed. The
	// selection, the highlight and the copy all read these rather than
	// vp.View(), so a selection whose anchor has scrolled off the screen still
	// knows what it holds.
	transcriptRows  []string
	transcriptPlain []string

	// mouseEnabled is --no-mouse kept on the model: the flag decides what
	// bubbletea reports, and this decides what craze does with a mouse message
	// that reached it anyway (the frame runner injects them).
	mouseEnabled bool

	// sel is the drag in progress or the highlight it left; pressed is the
	// button that went down, because an X10 terminal reports ButtonNone on the
	// release that ends it.
	sel     selection
	pressed tea.MouseButton
	// The double-click state machine: the last press's cell and time, and how
	// many presses have landed on it inside the window.
	clickPos cellPos
	clickAt  time.Time
	clicks   int

	// copyNote is what the last copy did, shown in status row 2 until
	// copyUntil.
	copyNote  string
	copyUntil time.Time

	// lay is the one layout computation per Update; View and the mouse
	// hit-tester both read it rather than measuring anything themselves.
	// layouts counts the computations relayout made, so a test can hold
	// §3.1's "computed once per Update".
	lay     frameLayout
	layouts int

	// cards is the blocking-request queue (§3.11): permission, question and
	// plan requests in arrival order. Only the head is drawn.
	//
	// cardsCancelled holds from a cancel until the turn ends: the session has
	// answered everything it was holding and will not park another request for
	// this turn, so a card event still in flight must not raise a card.
	cards          []card
	cardsCancelled bool
	snap           agent.Snapshot

	help bool

	// dialog is the modal layer: at most one is up, drawn over the transcript
	// region and hit-tested before any band. mdlg is the model dialog's own
	// state.
	dialog dialogKind
	mdlg   modelDialog

	// Theme dialog: the list is frozen when it opens because the live preview
	// changes the current theme on every move, and themePrev is the theme Esc
	// (or a click outside, or an arriving card) puts back.
	themeSel   int
	themeNames []string
	themePrev  Theme

	slashSel   int
	slashHide  bool
	skills     []slashItem
	streamOpen bool

	toolLine    map[string]int
	toolTouch   []string
	pathDirs    map[string]map[string]struct{}
	todoPlanned int
	todoDone    bool

	// Agent rows: the selection is held by tool id because the in-flight list
	// reorders on every update.
	agentSel   int
	agentID    string
	agentPeek  bool
	agentStart map[string]time.Time
	agentDone  map[string]time.Time

	tasksState    tasksPanelState
	todosSeen     bool
	todosClosedAt time.Time

	// planOffer is set when a plan-mode turn ended with something to implement,
	// and turnSawAssistant is what "something" means: a turn that only thought
	// or only ran tools left no plan behind. The offer is armed by EventDone
	// but only becomes actionable once the status has settled, because the two
	// arrive in either order — see planOffering.
	planOffer        bool
	turnSawAssistant bool

	tickGen     int
	tickLive    bool
	tickFast    bool
	spinFrame   int
	turnStart   time.Time
	lastThought bool

	ctrlCDeadline time.Time
	clock         func() time.Time
}

// now reads the clock through an indirection so tests can inject one.
func (m Model) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

type eventMsg struct{ ev agent.Event }
type startedMsg struct{}
type promptDoneMsg struct {
	res agent.Result
	err error
}
type errMsg struct{ err error }
type actionErrMsg struct{ err error }
type revertModeMsg struct {
	prev string
	err  error
}
type revertModelMsg struct {
	prev string
	err  error
}
type refreshSnapMsg struct{}

// dblClickMsg is the frame runner's <dblclick:X,Y>: the gesture without the
// two presses and the 400 ms between them.
type dblClickMsg struct{ X, Y int }

// planImplementMsg says the mode change the plan offer asked for landed, so
// the implement turn may now be written and sent. planImplementFailedMsg is
// the other half: nothing was sent, so the offer survives.
type planImplementMsg struct{ mode string }
type planImplementFailedMsg struct {
	prev string
	err  error
}

func New(cfg Config) Model {
	cwd := cfg.Workspace
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}

	vp := viewport.New(0, 0)
	vp.KeyMap = viewport.KeyMap{
		PageUp:   key.NewBinding(key.WithKeys("pgup")),
		PageDown: key.NewBinding(key.WithKeys("pgdown")),
	}

	th := Preset(cfg.Theme)
	m := Model{
		theme:        th,
		vp:           vp,
		input:        newComposer(th),
		sess:         cfg.Session,
		cwd:          cwd,
		model:        cfg.Model,
		yolo:         cfg.Yolo,
		mouseEnabled: !cfg.NoMouse,
	}
	m.git = discoverGit(cwd)
	m.branch = m.git.branch()
	if m.sess == nil {
		m.sess = NewStub()
	}
	m.refreshSnap()
	if m.model == "" && m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
	}
	if m.model == "" {
		m.model = "default"
	}
	return m
}

func Run(cfg Config) error {
	m := New(cfg)
	// One writer for the whole session: bubbletea's frames and the OSC 52 copy
	// are written from different goroutines, and a copy landing inside a frame
	// would tear it, so both go through the same lock.
	out := newSyncWriter(os.Stdout)
	clipboardOut = out
	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithOutput(out)}
	if !cfg.NoMouse {
		// Cell motion, not all motion: the quieter mode reports a drag once per
		// cell rather than per pixel, which is all the selection needs.
		opts = append(opts, tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(m, opts...)
	final, err := p.Run()
	if m.sess != nil {
		_ = m.sess.Close()
	}
	if err != nil {
		return err
	}
	// A quit is clean unless the session never started. Only startCmd's
	// failure counts: an error mid-session leaves a usable craze, and quitting
	// out of one is a normal exit.
	if fm, ok := final.(Model); ok {
		return fm.startErr
	}
	return nil
}

func (m Model) Init() tea.Cmd {
	return m.startCmd()
}

func (m Model) startCmd() tea.Cmd {
	sess := m.sess
	return func() tea.Msg {
		if err := sess.Start(context.Background()); err != nil {
			return errMsg{err}
		}
		return startedMsg{}
	}
}

// Update runs the handler and then lays the frame out exactly once, from the
// state the handler left behind, and keeps the single tick chain alive.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	tm, cmd := m.update(msg)
	next, ok := tm.(Model)
	if !ok {
		return tm, cmd
	}
	// The handler has already decided where the transcript sits: sticking now
	// only follows it down when the chrome above it changed shape.
	next.relayout(next.vp.Height == 0 || next.vp.AtBottom())
	// The tick chain is batched last, so a test can run the handler's own
	// command without waiting out a timer.
	if tick := next.armTick(); tick != nil {
		cmd = tea.Batch(cmd, tick)
	}
	return next, cmd
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Stickiness is decided from where the user was before the resize; the
		// layout itself is left to the one relayout the Update wrapper runs.
		stick := !m.ready || m.vp.Height == 0 || m.vp.AtBottom()
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		m.setViewportContent(stick)
		return m, nil

	case tickMsg:
		m.handleTick(msg)
		return m, nil

	case startedMsg:
		m.started = true
		m.status = statusIdle
		m.sessStart = m.now()
		m.branch = m.git.branch()
		m.refreshSnap()
		m.rescanSkills()
		return m, waitEvent(m.sess)

	case errMsg:
		// errMsg is only ever startCmd's: the session never came up. The TUI
		// stays on screen so the error is readable, but craze must not exit 0
		// afterwards, so the failure rides out on the final model.
		m.status = statusError
		m.err = msg.err.Error()
		m.startErr = msg.err
		m.addError(m.err)
		return m, nil

	case actionErrMsg:
		m.addError(msg.err.Error())
		return m, nil

	case revertModeMsg:
		m.snap.CurrentMode = msg.prev
		m.addError(msg.err.Error())
		return m, nil

	case revertModelMsg:
		m.snap.CurrentModel = msg.prev
		m.model = msg.prev
		m.addError(msg.err.Error())
		return m, nil

	case modelApplyMsg:
		// The steps that landed are the truth; the one that did not is named,
		// and the snapshot is re-read so the rows show what the agent has
		// rather than what the dialog hoped for.
		for _, note := range msg.done {
			m.addNote(note)
		}
		if msg.err != nil {
			m.refreshSnap()
			m.addError(msg.step + ": " + msg.err.Error())
		}
		return m, nil

	case refreshSnapMsg:
		m.refreshSnap()
		return m, nil

	case clipboardDoneMsg:
		m.copyNote = msg.note
		m.copyUntil = m.now().Add(copyNoteLinger)
		return m, nil

	case dblClickMsg:
		// The frame runner's deterministic double-click: two real presses would
		// make a golden depend on the clock. The state machine itself is
		// unit-tested against the injected one.
		if !m.mouseEnabled || m.cardOpen() || !m.selectable(msg.X, msg.Y) {
			return m, nil
		}
		pos, ok := m.transcriptCell(msg.X, msg.Y)
		if !ok {
			return m, nil
		}
		// The token stands in for the whole gesture, release included, so it
		// copies the way the real release does.
		return m.selectWord(pos).copySelection()

	case planImplementMsg:
		// The session is in the implement mode now, so the note and the user
		// entry are honest and the prompt goes out behind them.
		m.addNote(modeNote(m.snap.Modes, msg.mode))
		return m.sendText(m.snap.Provider.ImplementPrompt())

	case planImplementFailedMsg:
		// Nothing was sent: the mode reverts the way any failed SetMode does,
		// and the plan is still the last thing on screen, so it is still on
		// offer.
		tm, cmd := m.update(revertModeMsg(msg))
		next := tm.(Model)
		next.planOffer = true
		return next, cmd

	case eventMsg:
		m.applyEvent(msg.ev)
		return m, waitEvent(m.sess)

	case promptDoneMsg:
		// The stream is closed by EventDone, which shares the event channel with
		// the chunks; this message races them and would split a run in two.
		if msg.err != nil {
			m.status = statusError
			m.err = msg.err.Error()
			m.addError(m.err)
			return m, nil
		}
		if m.status != statusError {
			m.status = statusIdle
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return m, nil
}

// handleMouse is the mouse contract: the wheel scrolls the transcript, a left
// press either starts a selection over the transcript or hit-tests the regions
// frameLayout drew, and motion and release carry the drag.
//
// --no-mouse is kept on the model as well as withheld from bubbletea, so a
// mouse message that reached craze another way (the frame runner injects them)
// is ignored too.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if !m.mouseEnabled || m.cardOpen() {
		// A card owns the mouse as well as the keyboard, wheel included. It
		// arrived while the button was down, so the drag goes with it.
		return m, nil
	}
	switch msg.Action {
	case tea.MouseActionPress:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			// The selection is in transcript rows, not screen rows, so it
			// scrolls with the text it holds and survives the wheel.
			m.vp.ScrollUp(wheelLines)
		case tea.MouseButtonWheelDown:
			m.vp.ScrollDown(wheelLines)
		case tea.MouseButtonLeft:
			return m.handlePress(msg.X, msg.Y)
		}
	case tea.MouseActionMotion:
		// Only a drag extends. A double-click's word is already the selection
		// it meant, and the pointer wobbling over it must not eat into it.
		if m.pressed == tea.MouseButtonLeft && m.sel.drag {
			return m.handleDrag(msg.X, msg.Y), nil
		}
	case tea.MouseActionRelease:
		// X10 terminals report ButtonNone on release, so the button that went
		// down is the only record of which one came up.
		if m.pressed == tea.MouseButtonLeft {
			return m.handleRelease(msg.X, msg.Y)
		}
	}
	return m, nil
}

// handlePress is the left button going down: over the transcript it anchors a
// selection (or picks a word, on the second press inside the double-click
// window), and anywhere else it is the click the regions already understood.
func (m Model) handlePress(x, y int) (tea.Model, tea.Cmd) {
	now := m.now()
	if !m.selectable(x, y) {
		// The press belongs to another band, so whatever was highlighted is
		// the previous gesture and goes.
		m.sel = selection{}
		m.clicks = 0
		return m.handleClick(x, y)
	}
	m.pressed = tea.MouseButtonLeft
	pos, ok := m.transcriptCell(x, y)
	if !ok {
		return m, nil
	}
	// A second press on the same cell inside the window is a double-click; a
	// third one inside it starts over, so a rattle of clicks does not keep
	// re-selecting the word.
	double := m.clicks == 1 && pos == m.clickPos && now.Sub(m.clickAt) < doubleClickWindow
	m.clickPos, m.clickAt = pos, now
	if double {
		// The word is selected now and copied by the release that follows,
		// exactly as a drag over it would be.
		m.clicks = 2
		return m.selectWord(pos), nil
	}
	m.clicks = 1
	m.sel = selection{on: true, drag: true, anchor: pos, head: pos}
	return m, nil
}

// handleDrag extends the selection to the cell under the pointer. A motion
// event on the top or bottom row of the band scrolls one line first, so a drag
// can reach past the screen.
//
// Cell motion only reports a change of cell: a pointer held still on the edge
// does not keep scrolling. That is the mode's contract, not a bug to fix.
func (m Model) handleDrag(x, y int) tea.Model {
	tr := m.lay.Region(regionTranscript)
	if tr.Empty() {
		return m
	}
	switch {
	case y <= tr.Top:
		m.vp.ScrollUp(1)
	case y >= tr.Bottom-1:
		m.vp.ScrollDown(1)
	}
	if pos, ok := m.transcriptCell(x, y); ok {
		m.sel.head = pos
	}
	return m
}

// handleRelease finalises the drag: the head is the cell under the pointer, and
// a selection of more than the one cell the press made is copied.
func (m Model) handleRelease(x, y int) (tea.Model, tea.Cmd) {
	m.pressed = tea.MouseButtonNone
	if !m.sel.on {
		return m, nil
	}
	if pos, ok := m.transcriptCell(x, y); ok && m.sel.drag {
		m.sel.head = pos
	}
	m.sel.drag = false
	return m.copySelection()
}

// selectWord is the double-click: the run of non-space cells under the pointer.
// The selection is not a drag, so nothing afterwards moves its head — a word is
// the gesture's whole answer.
func (m Model) selectWord(pos cellPos) Model {
	m.sel = selection{}
	if pos.line >= len(m.transcriptPlain) {
		return m
	}
	lo, hi, ok := wordAt(m.transcriptPlain[pos.line], pos.col)
	if !ok {
		return m
	}
	m.sel = selection{
		on:     true,
		anchor: cellPos{line: pos.line, col: lo},
		head:   cellPos{line: pos.line, col: hi},
	}
	return m
}

// copySelection puts the highlighted cells on the clipboard. An empty
// selection — a plain click, or a double-click on whitespace — copies nothing
// and says nothing.
func (m Model) copySelection() (tea.Model, tea.Cmd) {
	text := m.selectionText()
	if text == "" {
		return m, nil
	}
	return m, copyText(text, copyNote(text, m.selectionLines()))
}

// copySelectionOrLastReply is Ctrl+Y. Without a selection it copies the last
// reply's own text rather than the rows it was wrapped into, so what lands on
// the clipboard is the agent's paragraph and not the screen's line breaks.
func (m Model) copySelectionOrLastReply() (tea.Model, tea.Cmd) {
	if !m.sel.empty() {
		return m.copySelection()
	}
	for i := len(m.entries) - 1; i >= 0; i-- {
		if e := &m.entries[i]; e.kind == entryAssistant && e.text != "" {
			return m, copyText(e.text, "copied last reply")
		}
	}
	return m, nil
}

// copyLingering is the note's 2-second window, which is also why the tick chain
// has to stay fast until it closes.
func (m Model) copyLingering() bool {
	return !m.copyUntil.IsZero() && m.now().Before(m.copyUntil)
}

// handleClick reads the same frameLayout View() drew from, so what is on the
// screen and what is clickable cannot drift apart.
//
// The modal layer is hit-tested first and swallows the click either way: a
// press inside it is a dialog row, and a press anywhere else closes it the way
// Esc does — applying nothing, reverting a theme preview.
func (m Model) handleClick(x, y int) (tea.Model, tea.Cmd) {
	lay := m.lay
	if lay.TooSmall {
		return m, nil
	}
	if r := lay.Dialog; !r.Empty() {
		if r.Contains(x, y) {
			return m.dialogClick(y - r.Y)
		}
		return m.closeDialog(true), nil
	}
	switch {
	case lay.Region(regionTasks).Contains(y):
		if lay.Region(regionTasks).Row(y) == 0 {
			return m.cycleTasks()
		}
	case lay.Region(regionAgents).Contains(y):
		// The last row can be "… +n more", which is not a sub-agent.
		m.selectAgent(lay.Region(regionAgents).Row(y))
	case lay.Region(regionStatus).Contains(y):
		return m.clickStatus(x, lay.Region(regionStatus).Row(y), lay)
	}
	return m, nil
}

// clickStatus hit-tests a status row against the spans the fitting pass that
// drew it reported, so a click cannot land on a segment that was dropped or
// truncated away: row 1's model name opens the model dialog, row 2's chip
// cycles the mode.
func (m Model) clickStatus(x, row int, lay frameLayout) (tea.Model, tea.Cmd) {
	// Both are a key under the pointer, gating included: a card blocks them
	// (handleMouse already returned), an overlay that swallows the key
	// swallows the click, and working blocks neither.
	if m.dialogOpen() || m.help {
		return m, nil
	}
	switch row {
	case 0:
		_, spans := m.statusRow1()
		if spanAt(spans, x) == spanModel {
			return m.openModelDialog(), nil
		}
	case 1:
		_, spans := m.statusRow2(lay)
		if spanAt(spans, x) == spanMode {
			return m.cycleMode()
		}
	}
	return m, nil
}

// dialogClick turns a press inside the box into the row it landed on: the
// border and the rows above the list are not options.
func (m Model) dialogClick(row int) (tea.Model, tea.Cmd) {
	body := m.dialogBody(m.lay.Dialog.W-dialogBorder, m.lay.Dialog.H-dialogBorder)
	i := row - 1 // the top border
	if i < 0 || i >= len(body) {
		return m, nil
	}
	switch m.dialog {
	case dialogModel:
		return m.modelDialogClick(i)
	case dialogTheme:
		return m.themeDialogClick(i)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The highlight is a mouse gesture: any key but the one that copies it
	// means the user has moved on.
	if msg.Type != tea.KeyCtrlY {
		m.sel = selection{}
	}
	switch msg.Type {
	case tea.KeyCtrlD:
		return m.requestQuit()
	case tea.KeyCtrlC:
		return m.handleCtrlC()
	}
	m.ctrlCDeadline = time.Time{}

	// A card owns the keyboard: everything below this, Ctrl+T / Ctrl+G /
	// Ctrl+O and the agent-row arrows included, is out of reach until it is
	// answered.
	if m.cardOpen() {
		return m.handleCardKey(msg)
	}

	switch m.dialog {
	case dialogTheme:
		return m.handleThemeDialogKey(msg)
	case dialogModel:
		return m.handleModelDialogKey(msg)
	}
	if m.help {
		return m.handleHelpKey(msg)
	}

	if msg.Type == tea.KeyCtrlY {
		// A keyboard feature, so it works under --no-mouse as well.
		return m.copySelectionOrLastReply()
	}
	if msg.Type == tea.KeyCtrlO {
		return m.toggleExpanded()
	}
	if msg.Type == tea.KeyCtrlT {
		return m.cycleTasks()
	}
	if msg.Type == tea.KeyCtrlG {
		return m.openThemePicker(), nil
	}
	if msg.Type == tea.KeyShiftTab {
		return m.cycleMode()
	}
	if msg.Type == tea.KeyEsc {
		if m.agentPeek {
			// The peek closes on its own; the turn underneath keeps running.
			m.agentPeek = false
			return m, nil
		}
		if m.slashMenuOpen() {
			m.slashHide = true
			m.slashSel = 0
			return m, nil
		}
		if m.status == statusWorking {
			return m.cancelTurn()
		}
		if m.planOffer {
			// Esc declines the plan offer and nothing else: the composer is
			// still where the user is, so it keeps the focus.
			m.planOffer = false
			return m, nil
		}
		m.input.Blur()
		return m, nil
	}
	if msg.Type == tea.KeyPgUp || msg.Type == tea.KeyPgDown {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	if isNewlineKey(msg) {
		return m, m.updateComposer(msg)
	}
	if msg.Type == tea.KeyEnter {
		return m.handleEnter()
	}
	if msg.Type == tea.KeyTab && m.slashMenuOpen() {
		m = m.completeSlash()
		return m, nil
	}
	if m.slashMenuOpen() && (msg.Type == tea.KeyUp || msg.Type == tea.KeyDown) {
		items := m.filteredSlash()
		if len(items) == 0 {
			return m, nil
		}
		if msg.Type == tea.KeyDown {
			m.slashSel = (m.slashSel + 1) % len(items)
		} else {
			m.slashSel = (m.slashSel - 1 + len(items)) % len(items)
		}
		return m, nil
	}
	// Only the arrow keys select a row; ctrl+p / ctrl+n stay with the textarea.
	if composerEmpty(m.input) && len(m.visibleAgents()) > 0 && (msg.Type == tea.KeyUp || msg.Type == tea.KeyDown) {
		delta := 1
		if msg.Type == tea.KeyUp {
			delta = -1
		}
		m.moveAgent(delta)
		return m, nil
	}
	return m, m.updateComposer(msg)
}

func (m *Model) updateComposer(msg tea.KeyMsg) tea.Cmd {
	prev := m.input.Value()
	// bubbles repositions its own viewport inside Update (textarea.go:1087),
	// against the height in force *before* the key, and never rewinds slack
	// afterwards: at height 1 a second line scrolled the first one — and the
	// `❯` that only ever marks display row 0 — off the top, and SetHeight does
	// not reposition. Lending the textarea more rows than any draft can have
	// keeps the cursor inside the view, so the offset stays 0 through newline,
	// wrap boundary and bracketed paste. relayout puts the real height back
	// before anything is drawn.
	m.input.SetHeight(composerHeadroom)
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() != prev {
		m.slashHide = false
		if name, _, ok := parseSlashLine(m.input.Value()); ok && name == "" {
			m.rescanSkills()
		}
	}
	return cmd
}

func (m Model) handleHelpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyEsc {
		m.help = false
	}
	return m, nil
}

// handleCtrlC implements the pinned state machine: working cancels and arms a
// one-second window, a second press inside the window quits, and idle or an
// error state quits outright.
func (m Model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.status != statusWorking {
		return m.requestQuit()
	}
	now := m.now()
	if !m.ctrlCDeadline.IsZero() && now.Before(m.ctrlCDeadline) {
		return m.requestQuit()
	}
	tm, cmd := m.cancelTurn()
	next := tm.(Model)
	next.ctrlCDeadline = now.Add(ctrlCWindow)
	return next, cmd
}

func (m Model) handleEnter() (tea.Model, tea.Cmd) {
	name, args, ok := parseSlashLine(m.input.Value())
	if ok && (name == "exit" || name == "quit") {
		return m.runBuiltin(name, args)
	}
	// The offer outranks the peek: an empty composer under a live offer means
	// "build it", and the peek is still there for an empty composer without
	// one. The test is Value()=="" and not composerEmpty, so Enter agrees with
	// the placeholder the user is looking at.
	if m.planOffering() && m.input.Value() == "" {
		return m.implementPlan()
	}
	if composerEmpty(m.input) && len(m.visibleAgents()) > 0 {
		m.agentPeek = true
		return m, nil
	}
	if m.cardOpen() || m.status == statusWorking || !m.started {
		return m, nil
	}
	if ok && name != "" && builtinNamed(name) {
		return m.runBuiltin(name, args)
	}
	return m.send()
}

func (m Model) cycleMode() (tea.Model, tea.Cmd) {
	if m.cardOpen() || len(m.snap.Modes) == 0 {
		return m, nil
	}
	id := agent.NextModeID(m.snap)
	return m.applyMode(id)
}

func (m Model) send() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	m.input.SetValue("")
	m.slashSel = 0
	return m.sendText(text)
}

// sendText starts a turn with text that is already decided. It is what the
// composer's own send and the plan offer have in common: the plan offer never
// touches the draft, so the two differ only in where the text came from.
func (m Model) sendText(text string) (tea.Model, tea.Cmd) {
	m.addUser(text)
	m.status = statusWorking
	m.cardsCancelled = false
	m.turnStart = m.now()
	m.err = ""
	// A new turn retires whatever the last one offered and starts counting its
	// own evidence again.
	m.planOffer = false
	m.turnSawAssistant = false
	sess := m.sess
	return m, func() tea.Msg {
		res, err := sess.Prompt(context.Background(), text)
		return promptDoneMsg{res, err}
	}
}

// planOffering is the offer once it can be acted on. EventDone arms it and
// promptDoneMsg settles the status, in either order, so everything the user
// can see or press waits for both — which is what makes the two orderings
// indistinguishable.
func (m Model) planOffering() bool { return m.planOffer && m.status == statusIdle }

// implementModeID is the advertised mode that means "do the work". Without one
// there is nowhere for the offer to go, so it is never made.
func (m Model) implementModeID() string {
	for _, md := range m.snap.Modes {
		if m.snap.Provider.Kind(md.ID) == agent.ModeImplement {
			return md.ID
		}
	}
	return ""
}

// planEarnsOffer is what a finished turn has to have been for the composer to
// offer the plan it left behind.
func (m Model) planEarnsOffer(stopReason string) bool {
	if stopReason == stopCancelled || m.status == statusError {
		return false
	}
	// A turn that only thought, or only ran tools, said nothing to implement.
	if !m.turnSawAssistant {
		return false
	}
	if m.snap.Provider.Kind(m.snap.CurrentMode) != agent.ModePlan {
		return false
	}
	return m.implementModeID() != ""
}

// implementPlan is Enter on the offer. The mode change and the prompt are
// chained rather than batched: cursor must not be asked to build the plan
// while the session is still in plan mode, so the prompt is only written once
// SetMode has come back, and a SetMode that fails sends nothing at all.
func (m Model) implementPlan() (tea.Model, tea.Cmd) {
	id := m.implementModeID()
	if id == "" {
		return m, nil
	}
	prev := m.snap.CurrentMode
	// Optimistic, the way applyMode is: the chip flips now and reverts if the
	// agent refuses.
	m.snap.CurrentMode = id
	m.planOffer = false
	sess := m.sess
	return m, func() tea.Msg {
		if err := sess.SetMode(context.Background(), id); err != nil {
			return planImplementFailedMsg{prev: prev, err: err}
		}
		return planImplementMsg{mode: id}
	}
}

// cancelTurn drops the whole card queue and cancels. Cancel answers every
// request the session is still holding — permission, question and plan alike —
// with that kind's cancelled outcome, exactly once each, so the UI must not
// answer them itself and race it.
func (m Model) cancelTurn() (tea.Model, tea.Cmd) {
	cards := len(m.cards)
	working := m.status == statusWorking
	if cards == 0 && !working {
		return m, nil
	}
	m.cards = nil
	m.cardsCancelled = true
	sess := m.sess
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = sess.Cancel(ctx)
		return nil
	}
}

// requestQuit closes the session, which answers every card still queued with
// its cancelled outcome on the way out. The queue is left alone: the model is
// on its way out with it, and clearing it would only restart the tick chain.
func (m Model) requestQuit() (tea.Model, tea.Cmd) {
	m.quitting = true
	sess := m.sess
	return m, func() tea.Msg {
		_ = sess.Close()
		return tea.Quit()
	}
}

func (m *Model) applyEvent(ev agent.Event) {
	// The spinner names what the turn is doing; only a thought chunk leaves it
	// on "Thinking…".
	m.lastThought = ev.Type == agent.EventThought
	switch ev.Type {
	case agent.EventText:
		m.turnSawAssistant = true
		m.appendStream(entryAssistant, ev.Text, ev.At)
	case agent.EventThought:
		m.appendStream(entryThought, ev.Text, ev.At)
	case agent.EventTool:
		m.refreshSnap()
		if ev.Tool != nil {
			m.noteToolUpdate(ev.Tool.ID)
			m.noteAgentTiming(ev.Tool)
			m.upsertTool(ev.Tool)
		}
	case agent.EventTodos:
		m.refreshSnap()
		todos := m.todosOf(ev)
		m.noteTodoLifecycle(todos)
		m.noteTodos(todos)
	case agent.EventPermission:
		if ev.Permission != nil {
			m.pushCard(card{kind: cardPermission, perm: ev.Permission})
		}
	case agent.EventQuestion:
		// An auto-answered request (headless) is already decided; only an
		// interactive one is a card.
		if ev.Question != nil && !ev.Question.Auto {
			m.pushCard(card{kind: cardQuestion, ask: ev.Question})
		}
	case agent.EventPlan:
		if ev.Plan != nil && !ev.Plan.Auto {
			// The plan itself is transcript material; the card is only the
			// three answers it needs.
			m.addPlan(ev.Plan)
			m.pushCard(card{kind: cardPlan, plan: ev.Plan})
		}
	case agent.EventDone:
		m.breakStream()
		m.cardsCancelled = false
		// The turn is over, so this is the one moment the branch can have
		// changed under craze. No polling, no resize hook.
		m.branch = m.git.branch()
		if ev.StopReason == stopCancelled {
			m.status = statusIdle
			// Esc leaves nothing else behind: the spinner going away is the
			// only other sign the cancel landed, and it is indistinguishable
			// from the turn having finished on its own.
			m.addNote(stopCancelled)
		}
		// EventDone, not promptDoneMsg, is what orders the transcript, so it is
		// also what decides whether the turn left a plan behind.
		if m.planEarnsOffer(ev.StopReason) {
			m.planOffer = true
		}
	case agent.EventError:
		m.status = statusError
		if ev.Err != nil {
			m.err = ev.Err.Error()
			m.addError(m.err)
		}
	case agent.EventMeta:
		m.refreshSnap()
	}
}

// toggleExpanded is the global Ctrl+O detail toggle; every entry redraws
// because the render key changed.
func (m Model) toggleExpanded() (tea.Model, tea.Cmd) {
	stick := m.vp.Height == 0 || m.vp.AtBottom()
	m.expanded = !m.expanded
	m.setViewportContent(stick)
	return m, nil
}

// todosOf prefers the list the event carried and falls back to the snapshot.
func (m Model) todosOf(ev agent.Event) []agent.Todo {
	if len(ev.Todos) > 0 {
		return ev.Todos
	}
	return m.snap.Todos
}

func (m *Model) refreshSnap() {
	if m.sess == nil {
		return
	}
	prevMode := m.snap.CurrentMode
	m.snap = m.sess.Snapshot()
	if m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
	}
	if m.snap.CurrentMode != prevMode {
		// The agent changed the mode under craze, so whatever plan the last
		// turn left belongs to a mode the session is no longer in.
		m.planOffer = false
	}
}

// View places the regions the layout decided, each forced to exactly its own
// row count, so the frame is always exactly as tall as the terminal.
func (m Model) View() string {
	if !m.ready || m.width <= 0 || m.height <= 0 {
		// A degenerate size still owes the terminal exactly its own rows.
		return blankFrame(m.width, m.height)
	}
	lay := m.lay
	if lay.Width != m.width || lay.Height != m.height {
		// Only reachable when something resized the model without an Update;
		// m is a copy here, so this does not count against "once per Update".
		lay = m.computeLayout()
	}
	if lay.TooSmall {
		return tooSmallView(m.width, m.height)
	}
	rows := make([]string, 0, m.height)
	for i := range frameRegions {
		r := lay.regions[i]
		if r.Empty() {
			continue
		}
		rows = append(rows, fitRows(frameRegions[i].view(m, lay), r.Height(), m.width)...)
	}
	base := strings.Join(rows, "\n")
	// The dialog is a layer, not a band: it is composited over the frame the
	// region list just drew, so the bands keep their rows and the box covers
	// only the transcript.
	if r := lay.Dialog; !r.Empty() {
		base = overlay(base, m.dialogView(r), r)
	}
	return base
}

// overlayView is the one lower overlay that draws under the transcript; the
// layout crops it rather than letting it squeeze the transcript away. The
// pickers left it in V3: a dialog is a layer over the transcript, not a band
// under it.
//
// A card outranks both (§3.11): the one it did not close is suspended — kept
// in state, not drawn — until it has been answered.
func (m Model) overlayView() string {
	if m.cardOpen() {
		return ""
	}
	switch {
	case m.help:
		return m.helpView()
	case m.slashMenuOpen():
		return m.slashMenuView()
	}
	return ""
}

func (m Model) overlayRows() int {
	v := m.overlayView()
	if v == "" {
		return 0
	}
	return lipgloss.Height(v)
}

func (m Model) slashMenuView() string {
	items := m.filteredSlash()
	if len(items) == 0 {
		return ""
	}
	if m.slashSel >= len(items) {
		m.slashSel = 0
	}
	var b strings.Builder
	for i, it := range items {
		line := fmt.Sprintf("/%s  %s", it.Name, it.labeledDesc())
		st := lipgloss.NewStyle().Foreground(m.theme.Dim)
		if i == m.slashSel {
			st = lipgloss.NewStyle().Foreground(m.theme.Accent)
		}
		b.WriteString(st.Render(line))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) helpView() string {
	lines := []string{
		"enter send   shift/alt+enter or ctrl+j newline   shift+tab cycle mode",
		"esc cancel   ctrl+c cancel then quit   ctrl+d quit   pgup/pgdn scroll",
		"ctrl+t tasks panel   ctrl+g theme   ctrl+o expand detail",
		"ctrl+y copy the selection, or the last reply   drag to select",
		"commands:",
	}
	for _, it := range m.slashCatalog() {
		lines = append(lines, fmt.Sprintf("  /%s  %s", it.Name, it.labeledDesc()))
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.theme.Accent).
		Width(max(1, m.width-2)).
		Render(strings.Join(lines, "\n"))
}

// workspaceName is the basename of the workspace, falling back to the path
// itself at a filesystem root.
func workspaceName(cwd string) string {
	base := filepath.Base(cwd)
	switch base {
	case "", ".", string(filepath.Separator):
		return cwd
	}
	return base
}

func waitEvent(sess agent.Session) tea.Cmd {
	if sess == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-sess.Events()
		if !ok {
			return nil
		}
		return eventMsg{ev}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
