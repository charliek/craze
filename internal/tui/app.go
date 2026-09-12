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

const (
	// wheelLines is how far one wheel notch scrolls the transcript.
	wheelLines = 3
	// pickerHeaderRows is the box border plus the picker's own title row,
	// which sit above option 1.
	pickerHeaderRows = 2
)

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

	help       bool
	picking    bool
	effortStep bool
	modelSel   int
	effortSel  int

	// Theme picker: the list is frozen when it opens because the live preview
	// changes the current theme on every move, and themePrev is the theme Esc
	// (or an arriving card) puts back.
	themePicking bool
	themeSel     int
	themeNames   []string
	themePrev    Theme

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
		theme: th,
		vp:    vp,
		input: newComposer(th),
		sess:  cfg.Session,
		cwd:   cwd,
		model: cfg.Model,
		yolo:  cfg.Yolo,
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
	opts := []tea.ProgramOption{tea.WithAltScreen()}
	if !cfg.NoMouse {
		// Cell motion, not all motion: craze ignores drags, and the quieter
		// mode keeps the terminal from streaming a report per cell.
		opts = append(opts, tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(m, opts...)
	_, err := p.Run()
	if m.sess != nil {
		_ = m.sess.Close()
	}
	return err
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
		m.status = statusError
		m.err = msg.err.Error()
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
		m.picking = false
		m.effortStep = false
		m.addError(msg.err.Error())
		return m, nil

	case refreshSnapMsg:
		m.refreshSnap()
		return m, nil

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

// handleMouse is the pinned mouse contract: the wheel scrolls the transcript,
// a left press hit-tests the regions frameLayout drew. Drag, motion and every
// other button are ignored.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || m.cardOpen() {
		// A card owns the mouse as well as the keyboard, wheel included.
		return m, nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.vp.ScrollUp(wheelLines)
	case tea.MouseButtonWheelDown:
		m.vp.ScrollDown(wheelLines)
	case tea.MouseButtonLeft:
		return m.handleClick(msg.Y)
	}
	return m, nil
}

// handleClick reads the same frameLayout View() drew from, so what is on the
// screen and what is clickable cannot drift apart.
func (m Model) handleClick(y int) (tea.Model, tea.Cmd) {
	lay := m.lay
	if lay.TooSmall {
		return m, nil
	}
	switch {
	case m.picking && lay.Region(regionOverlay).Contains(y):
		// The picker is a bordered box: its top border and its header sit
		// above the first option.
		if i := lay.Region(regionOverlay).Row(y) - pickerHeaderRows; i >= 0 {
			return m.applyPickerIndex(i)
		}
	case m.themePicking && lay.Region(regionOverlay).Contains(y):
		if i := lay.Region(regionOverlay).Row(y) - pickerHeaderRows; i >= 0 && i < len(m.themeNames) {
			m.themeSel = i
			m.applyTheme(Preset(m.themeNames[i]))
			return m.keepTheme(), nil
		}
	case lay.Region(regionTasks).Contains(y):
		if lay.Region(regionTasks).Row(y) == 0 {
			return m.cycleTasks()
		}
	case lay.Region(regionAgents).Contains(y):
		// The last row can be "… +n more", which is not a sub-agent.
		m.selectAgent(lay.Region(regionAgents).Row(y))
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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

	if m.themePicking {
		return m.handleThemePickerKey(msg)
	}
	if m.picking {
		return m.handleModelPickerKey(msg)
	}
	if m.help {
		return m.handleHelpKey(msg)
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

func (m Model) pickerCount() int {
	if m.effortStep {
		if opt := agent.EffortOption(m.snap); opt != nil {
			return len(opt.SelectValues)
		}
		return 0
	}
	return len(agent.OrderModels(m.snap))
}

func (m Model) applyPickerIndex(i int) (tea.Model, tea.Cmd) {
	if m.effortStep {
		opt := agent.EffortOption(m.snap)
		if opt == nil || i < 0 || i >= len(opt.SelectValues) {
			return m, nil
		}
		return m.applyEffort(opt.SelectValues[i].Value)
	}
	models := agent.OrderModels(m.snap)
	if i < 0 || i >= len(models) {
		return m, nil
	}
	return m.applyModel(models[i].ID)
}

func (m Model) closePicker() Model {
	m.picking = false
	m.effortStep = false
	return m
}

func (m Model) handleModelPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := m.pickerCount()
	if n == 0 {
		return m.closePicker(), nil
	}
	if m.effortStep {
		if m.effortSel < 0 || m.effortSel >= n {
			m.effortSel = 0
		}
	} else if m.modelSel < 0 || m.modelSel >= n {
		m.modelSel = 0
	}
	sel := m.modelSel
	if m.effortStep {
		sel = m.effortSel
	}
	switch msg.Type {
	case tea.KeyEsc:
		return m.closePicker(), nil
	case tea.KeyEnter:
		return m.applyPickerIndex(sel)
	case tea.KeyDown:
		sel = (sel + 1) % n
	case tea.KeyUp:
		sel = (sel - 1 + n) % n
	default:
		s := msg.String()
		if s == "j" {
			sel = (sel + 1) % n
		} else if s == "k" {
			sel = (sel - 1 + n) % n
		} else if len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
			i := int(s[0] - '1')
			if i < n {
				return m.applyPickerIndex(i)
			}
			return m, nil
		} else {
			return m, nil
		}
	}
	if m.effortStep {
		m.effortSel = sel
	} else {
		m.modelSel = sel
	}
	return m, nil
}

func (m Model) handleEnter() (tea.Model, tea.Cmd) {
	name, args, ok := parseSlashLine(m.input.Value())
	if ok && (name == "exit" || name == "quit") {
		return m.runBuiltin(name, args)
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
	m.addUser(text)
	m.status = statusWorking
	m.cardsCancelled = false
	m.turnStart = m.now()
	m.err = ""
	sess := m.sess
	return m, func() tea.Msg {
		res, err := sess.Prompt(context.Background(), text)
		return promptDoneMsg{res, err}
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
		if ev.StopReason == "cancelled" {
			m.status = statusIdle
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
	m.snap = m.sess.Snapshot()
	if m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
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
	return strings.Join(rows, "\n")
}

// overlayView is the one lower overlay that draws under the transcript; the
// layout crops it rather than letting it squeeze the transcript away.
//
// A card outranks every one of them (§3.11): the ones it did not close are
// suspended — kept in state, not drawn — until it has been answered.
func (m Model) overlayView() string {
	if m.cardOpen() {
		return ""
	}
	switch {
	case m.help:
		return m.helpView()
	case m.themePicking:
		return m.themePickerView()
	case m.picking:
		return m.modelPickerView()
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

func (m Model) modelPickerView() string {
	var b strings.Builder
	if m.effortStep {
		b.WriteString("effort  (enter select, esc close)\n")
		opt := agent.EffortOption(m.snap)
		if opt != nil {
			for i, v := range opt.SelectValues {
				mark := " "
				if i == m.effortSel {
					mark = ">"
				}
				fmt.Fprintf(&b, "%s %d %s  %s\n", mark, i+1, v.Value, v.Name)
			}
		}
	} else {
		b.WriteString("model  (enter select, esc close)\n")
		for i, md := range agent.OrderModels(m.snap) {
			mark := " "
			if i == m.modelSel {
				mark = ">"
			}
			fmt.Fprintf(&b, "%s %d %s  %s\n", mark, i+1, md.ID, md.Name)
		}
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.theme.Accent).
		Width(max(1, m.width-2)).
		Render(strings.TrimRight(b.String(), "\n"))
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
