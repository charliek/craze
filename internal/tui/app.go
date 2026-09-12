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

	lines    []transcriptLine
	width    int
	height   int
	ready    bool
	quitting bool
	started  bool

	pending *agent.PermissionEvent
	snap    agent.Snapshot

	help       bool
	picking    bool
	effortStep bool
	modelSel   int
	effortSel  int
	slashSel   int
	slashHide  bool
	skills     []slashItem
	streamOpen bool

	toolLine  map[string]int
	toolTouch []string
	stripSel  int
	stripID   string
	stripPeek bool
}

type transcriptLine struct {
	kind string
	text string
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

	m := Model{
		theme: Preset(cfg.Theme),
		vp:    vp,
		input: newComposer(),
		sess:  cfg.Session,
		cwd:   cwd,
		model: cfg.Model,
		yolo:  cfg.Yolo,
	}
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
	p := tea.NewProgram(m, tea.WithAltScreen())
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

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		m.layout()
		return m, nil

	case startedMsg:
		m.started = true
		m.status = statusIdle
		m.refreshSnap()
		m.rescanSkills()
		return m, waitEvent(m.sess)

	case errMsg:
		m.status = statusError
		m.err = msg.err.Error()
		m.addLine("error", m.err)
		return m, nil

	case actionErrMsg:
		m.addLine("error", msg.err.Error())
		return m, nil

	case revertModeMsg:
		m.snap.CurrentMode = msg.prev
		m.addLine("error", msg.err.Error())
		return m, nil

	case revertModelMsg:
		m.snap.CurrentModel = msg.prev
		m.model = msg.prev
		m.picking = false
		m.effortStep = false
		m.addLine("error", msg.err.Error())
		return m, nil

	case refreshSnapMsg:
		m.refreshSnap()
		return m, nil

	case eventMsg:
		m.applyEvent(msg.ev)
		return m, waitEvent(m.sess)

	case promptDoneMsg:
		m.breakStream()
		if msg.err != nil {
			m.status = statusError
			m.err = msg.err.Error()
			m.addLine("error", m.err)
			return m, nil
		}
		if m.status != statusError {
			m.status = statusIdle
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC || msg.Type == tea.KeyCtrlD {
		return m.requestQuit()
	}

	if m.pending != nil {
		if msg.Type == tea.KeyEsc {
			return m.cancelTurn()
		}
		if msg.String() == "q" && composerEmpty(m.input) {
			return m.requestQuit()
		}
		return m.handlePermissionKey(msg)
	}

	if m.picking {
		return m.handleModelPickerKey(msg)
	}
	if m.help {
		return m.handleHelpKey(msg)
	}

	if msg.Type == tea.KeyShiftTab {
		return m.cycleMode()
	}
	if msg.Type == tea.KeyEsc {
		if m.stripPeek {
			m.stripPeek = false
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
	if msg.String() == "q" && composerEmpty(m.input) {
		return m.requestQuit()
	}
	if msg.String() == "?" && composerEmpty(m.input) {
		m.help = true
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
	if composerEmpty(m.input) && len(m.stripItems()) > 0 && (msg.Type == tea.KeyUp || msg.Type == tea.KeyDown) {
		delta := 1
		if msg.Type == tea.KeyUp {
			delta = -1
		}
		m.moveStrip(delta)
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
	switch msg.String() {
	case "q", "?", "esc":
		m.help = false
	}
	if msg.Type == tea.KeyEsc {
		m.help = false
	}
	return m, nil
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
		if s == "q" {
			return m.closePicker(), nil
		}
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
	if composerEmpty(m.input) && len(m.stripItems()) > 0 {
		m.stripPeek = true
		return m, nil
	}
	if m.pending != nil || m.status == statusWorking || !m.started {
		return m, nil
	}
	if ok && name != "" && builtinNamed(name) {
		return m.runBuiltin(name, args)
	}
	return m.send()
}

func (m Model) cycleMode() (tea.Model, tea.Cmd) {
	if m.pending != nil || len(m.snap.Modes) == 0 {
		return m, nil
	}
	id := agent.NextModeID(m.snap)
	return m.applyMode(id)
}

func (m Model) handlePermissionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "a", "y", "1":
		return m.answerPending("allow_once")
	case "n", "r", "2":
		return m.answerPending("reject_once")
	}
	return m, nil
}

func (m Model) answerPending(kind string) (tea.Model, tea.Cmd) {
	p := m.pending
	m.pending = nil
	if p == nil {
		return m, nil
	}
	m.layout()
	id := ""
	if kind != "" {
		for _, o := range p.Options {
			if o.Kind == kind {
				id = o.OptionID
				break
			}
		}
	}
	return m, func() tea.Msg {
		if err := m.sess.AnswerPermission(p.ID, id); err != nil {
			return actionErrMsg{err}
		}
		return nil
	}
}

func (m Model) send() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	m.input.SetValue("")
	m.slashSel = 0
	m.breakStream()
	m.addLine("user", text)
	m.status = statusWorking
	m.err = ""
	sess := m.sess
	return m, func() tea.Msg {
		res, err := sess.Prompt(context.Background(), text)
		return promptDoneMsg{res, err}
	}
}

func (m Model) cancelTurn() (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	if m.pending != nil {
		tm, cmd := m.answerPending("")
		m = tm.(Model)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if m.status == statusWorking {
		cmds = append(cmds, m.cancelCmd())
	}
	return m, tea.Batch(cmds...)
}

func (m Model) cancelCmd() tea.Cmd {
	sess := m.sess
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = sess.Cancel(ctx)
		return nil
	}
}

func (m Model) requestQuit() (tea.Model, tea.Cmd) {
	m.quitting = true
	sess := m.sess
	return m, func() tea.Msg {
		_ = sess.Close()
		return tea.Quit()
	}
}

func (m *Model) applyEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventText:
		m.appendStream("assistant", ev.Text)
	case agent.EventThought:
		m.appendStream("thought", ev.Text)
	case agent.EventTool:
		m.refreshSnap()
		m.breakStream()
		if ev.Tool != nil {
			m.noteToolUpdate(ev.Tool.ID)
			m.upsertToolLine(ev.Tool)
		}
		m.syncStrip()
		m.layout()
	case agent.EventPermission:
		m.breakStream()
		m.pending = ev.Permission
		m.help = false
		m.picking = false
		m.effortStep = false
		m.layout()
	case agent.EventDone:
		m.breakStream()
		if ev.StopReason == "cancelled" {
			m.status = statusIdle
		}
	case agent.EventError:
		m.breakStream()
		m.status = statusError
		if ev.Err != nil {
			m.err = ev.Err.Error()
			m.addLine("error", m.err)
		}
	case agent.EventMeta:
		m.refreshSnap()
	}
}

func (m *Model) appendStream(kind, text string) {
	if text == "" {
		return
	}
	if m.streamOpen && len(m.lines) > 0 && m.lines[len(m.lines)-1].kind == kind {
		m.lines[len(m.lines)-1].text += text
		m.refreshViewport()
		return
	}
	m.streamOpen = true
	m.addLine(kind, text)
}

func (m *Model) breakStream() {
	m.streamOpen = false
}

func (m *Model) addLine(kind, text string) {
	if text == "" && kind != "user" {
		return
	}
	m.lines = append(m.lines, transcriptLine{kind: kind, text: text})
	m.refreshViewport()
}

func formatToolLine(t *agent.ToolEvent) string {
	if t == nil {
		return ""
	}
	parts := make([]string, 0, 4)
	if t.Kind != "" {
		parts = append(parts, t.Kind)
	}
	if t.Status != "" {
		parts = append(parts, t.Status)
	}
	if t.Title != "" {
		parts = append(parts, t.Title)
	}
	if t.ID != "" {
		parts = append(parts, t.ID)
	}
	s := strings.Join(parts, " ")
	if t.RawInput != "" {
		s += " (" + t.RawInput + ")"
	}
	return s
}

func (m *Model) upsertToolLine(t *agent.ToolEvent) {
	if t == nil {
		return
	}
	text := formatToolLine(t)
	if text == "" {
		return
	}
	if t.ID != "" && m.toolLine != nil {
		if idx, ok := m.toolLine[t.ID]; ok && idx >= 0 && idx < len(m.lines) && m.lines[idx].kind == "tool" {
			m.lines[idx].text = text
			m.refreshViewport()
			return
		}
	}
	m.addLine("tool", text)
	if t.ID != "" {
		if m.toolLine == nil {
			m.toolLine = make(map[string]int)
		}
		m.toolLine[t.ID] = len(m.lines) - 1
	}
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

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	footerH := max(1, lipgloss.Height(m.footer()))
	composerH := composerBoxHeight()
	extra := 0
	if m.pending != nil {
		extra += max(1, lipgloss.Height(m.permissionOverlay()))
	}
	if m.help {
		extra += lipgloss.Height(m.helpView())
	}
	if m.picking {
		extra += lipgloss.Height(m.modelPickerView())
	}
	if m.slashMenuOpen() && !m.help && !m.picking {
		if h := lipgloss.Height(m.slashMenuView()); h > 0 {
			extra += h
		}
	}
	if s := m.stripView(); s != "" {
		extra += lipgloss.Height(s)
	}
	h := m.height - footerH - composerH - extra
	if h < 1 {
		h = 1
	}
	m.vp.Width = m.width
	m.vp.Height = h
	m.input.SetWidth(max(1, m.width-4))
}

func (m *Model) refreshViewport() {
	stick := m.vp.Height == 0 || m.vp.AtBottom()
	var b strings.Builder
	for _, ln := range m.lines {
		switch ln.kind {
		case "user":
			b.WriteString(lipgloss.NewStyle().Foreground(m.theme.User).Render("you: " + ln.text))
		case "tool":
			b.WriteString(lipgloss.NewStyle().Foreground(m.theme.Tool).Render("tool " + ln.text))
		case "error":
			b.WriteString(lipgloss.NewStyle().Foreground(m.theme.Err).Render("error: " + ln.text))
		case "thought":
			b.WriteString(lipgloss.NewStyle().Foreground(m.theme.Dim).Italic(true).Render(ln.text))
		default:
			b.WriteString(lipgloss.NewStyle().Foreground(m.theme.Assistant).Render(ln.text))
		}
		b.WriteByte('\n')
	}
	m.vp.SetContent(strings.TrimRight(b.String(), "\n"))
	if stick {
		m.vp.GotoBottom()
	}
}

func (m Model) View() string {
	if !m.ready {
		return "craze"
	}
	m.layout()
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.theme.Border).
		Width(max(1, m.width-2)).
		Render(m.input.View())
	parts := []string{m.vp.View()}
	if m.help {
		parts = append(parts, m.helpView())
	}
	if m.picking {
		parts = append(parts, m.modelPickerView())
	}
	if m.slashMenuOpen() && !m.help && !m.picking {
		parts = append(parts, m.slashMenuView())
	}
	parts = append(parts, box)
	if s := m.stripView(); s != "" {
		parts = append(parts, s)
	}
	if m.pending != nil {
		parts = append(parts, m.permissionOverlay())
	}
	parts = append(parts, m.footer())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
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
			st = lipgloss.NewStyle().Foreground(m.theme.Title)
		}
		b.WriteString(st.Render(line))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) overlayReserve() int {
	h := composerBoxHeight() + max(1, lipgloss.Height(m.footer()))
	if m.pending != nil {
		h += max(1, lipgloss.Height(m.permissionOverlay()))
	}
	if s := m.stripView(); s != "" {
		h += lipgloss.Height(s)
	}
	return h
}

func (m Model) helpView() string {
	lines := []string{
		"enter send   shift/alt+enter or ctrl+j newline   shift+tab cycle mode",
		"esc cancel   q empty composer quit   ctrl+c/d quit   pgup/pgdn scroll",
		"commands:",
	}
	for _, it := range m.slashCatalog() {
		lines = append(lines, fmt.Sprintf("  /%s  %s", it.Name, it.labeledDesc()))
	}
	st := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.theme.Title).
		Width(max(1, m.width-2))
	if m.height > 0 {
		budget := m.height - m.overlayReserve() - 1
		if budget > 0 {
			st = st.MaxHeight(budget)
		}
	}
	return st.Render(strings.Join(lines, "\n"))
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
		BorderForeground(m.theme.Title).
		Width(max(1, m.width-2)).
		Render(strings.TrimRight(b.String(), "\n"))
}

func (m Model) permissionOverlay() string {
	tool := ""
	if m.pending != nil {
		tool = m.pending.Tool
	}
	return lipgloss.NewStyle().Foreground(m.theme.Warn).Render(
		fmt.Sprintf("permission %s  [a]llow-once  [n] reject-once", tool),
	)
}

func (m Model) footer() string {
	perm := "yolo"
	if !m.yolo {
		perm = "prompt"
	}
	st := m.status.String()
	if !m.started && m.status != statusError {
		st = "starting"
	} else if m.status == statusError {
		st = "error"
	}
	model := m.model
	if m.snap.CurrentModel != "" {
		model = m.snap.CurrentModel
	}
	mode := m.snap.CurrentMode
	if mode == "" {
		mode = "-"
	}
	effort := "-"
	if opt := agent.EffortOption(m.snap); opt != nil && opt.Current != "" {
		effort = opt.Current
	}
	prefix := fmt.Sprintf("%s  %s  %s  %s  ", model, effort, mode, perm)
	suffix := "  " + st
	w := max(1, m.width)
	cwd := m.cwd
	budget := w - lipgloss.Width(prefix) - lipgloss.Width(suffix)
	if budget < 1 {
		budget = 1
	}
	cwd = clampWidthTail(cwd, budget)
	line := prefix + cwd + suffix
	return lipgloss.NewStyle().
		Foreground(m.theme.FooterFG).
		Background(m.theme.FooterBG).
		Width(w).
		Render(line)
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
