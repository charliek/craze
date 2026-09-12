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
	modelSel   int
	slashSel   int
	streamOpen bool
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
		m.addLine("error", msg.err.Error())
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
		if m.slashMenuOpen() {
			m.input.SetValue("")
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
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
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
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
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

func (m Model) handleModelPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.snap.Models)
	if n == 0 {
		m.picking = false
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEsc:
		m.picking = false
		return m, nil
	case tea.KeyEnter:
		id := m.snap.Models[m.modelSel].ID
		return m.applyModel(id)
	case tea.KeyDown:
		m.modelSel = (m.modelSel + 1) % n
		return m, nil
	case tea.KeyUp:
		m.modelSel = (m.modelSel - 1 + n) % n
		return m, nil
	}
	s := msg.String()
	if s == "q" {
		m.picking = false
		return m, nil
	}
	if s == "j" {
		m.modelSel = (m.modelSel + 1) % n
		return m, nil
	}
	if s == "k" {
		m.modelSel = (m.modelSel - 1 + n) % n
		return m, nil
	}
	if len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
		i := int(s[0] - '1')
		if i < n {
			return m.applyModel(m.snap.Models[i].ID)
		}
	}
	return m, nil
}

func (m Model) handleEnter() (tea.Model, tea.Cmd) {
	if m.pending != nil || m.status == statusWorking || !m.started {
		return m, nil
	}
	name, args, ok := parseSlashLine(m.input.Value())
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
		m.breakStream()
		name, status, id := "", "", ""
		if ev.Tool != nil {
			name, status, id = ev.Tool.Name, ev.Tool.Status, ev.Tool.ID
		}
		m.addLine("tool", fmt.Sprintf("%s %s %s", name, status, id))
	case agent.EventPermission:
		m.breakStream()
		m.pending = ev.Permission
		m.help = false
		m.picking = false
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
	footerH := 1
	composerH := composerBoxHeight()
	extra := 0
	if m.pending != nil {
		extra++
	}
	if m.help {
		extra += 8
	}
	if m.picking {
		extra += min(8, max(1, len(m.snap.Models)))
	}
	if m.slashMenuOpen() && !m.help && !m.picking {
		extra += min(6, max(1, len(m.filteredSlash())))
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
		line := fmt.Sprintf("/%s  %s", it.Name, it.Desc)
		st := lipgloss.NewStyle().Foreground(m.theme.Dim)
		if i == m.slashSel {
			st = lipgloss.NewStyle().Foreground(m.theme.Title)
		}
		b.WriteString(st.Render(line))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) helpView() string {
	lines := []string{
		"enter send   shift/alt+enter or ctrl+j newline   shift+tab cycle mode",
		"esc cancel   q empty composer quit   ctrl+c/d quit   pgup/pgdn scroll",
		"commands:",
	}
	for _, it := range m.slashCatalog() {
		lines = append(lines, fmt.Sprintf("  /%s  %s", it.Name, it.Desc))
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.theme.Title).
		Width(max(1, m.width-2)).
		Render(strings.Join(lines, "\n"))
}

func (m Model) modelPickerView() string {
	var b strings.Builder
	b.WriteString("model  (enter select, esc close)\n")
	for i, md := range m.snap.Models {
		mark := " "
		if i == m.modelSel {
			mark = ">"
		}
		fmt.Fprintf(&b, "%s %d %s  %s\n", mark, i+1, md.ID, md.Name)
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
	line := fmt.Sprintf("%s  %s  %s  %s  %s", model, mode, perm, m.cwd, st)
	return lipgloss.NewStyle().
		Foreground(m.theme.FooterFG).
		Background(m.theme.FooterBG).
		Width(max(1, m.width)).
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
