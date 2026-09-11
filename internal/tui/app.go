package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
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
	input textinput.Model

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

func New(cfg Config) Model {
	ti := textinput.New()
	ti.Placeholder = "send a message"
	ti.Prompt = "> "
	ti.CharLimit = 0
	ti.Focus()

	cwd := cfg.Workspace
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}

	m := Model{
		theme: Preset(cfg.Theme),
		vp:    viewport.New(0, 0),
		input: ti,
		sess:  cfg.Session,
		cwd:   cwd,
		model: cfg.Model,
		yolo:  cfg.Yolo,
	}
	if m.sess == nil {
		m.sess = NewStub()
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
		return m, waitEvent(m.sess)

	case errMsg:
		m.status = statusError
		m.err = msg.err.Error()
		m.addLine("error", m.err)
		return m, nil

	case eventMsg:
		m.applyEvent(msg.ev)
		return m, waitEvent(m.sess)

	case promptDoneMsg:
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
	if m.pending != nil {
		return m.handlePermissionKey(msg)
	}
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyCtrlD:
		return m.requestQuit()
	case tea.KeyEsc:
		if m.status == statusWorking {
			return m, m.cancelCmd()
		}
		m.input.Blur()
		return m, nil
	case tea.KeyEnter:
		if m.status == statusWorking || !m.started {
			return m, nil
		}
		return m.send()
	}
	if msg.String() == "q" && m.input.Value() == "" {
		return m.requestQuit()
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m Model) handlePermissionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "a", "y", "1":
		return m.answerPending("allow_once")
	case "n", "r", "2":
		return m.answerPending("reject_once")
	case "esc", "ctrl+c":
		return m.answerPending("")
	}
	return m, nil
}

func (m Model) answerPending(kind string) (tea.Model, tea.Cmd) {
	p := m.pending
	m.pending = nil
	if p == nil {
		return m, nil
	}
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
			return errMsg{err}
		}
		return nil
	}
}

func (m Model) send() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	m.input.Reset()
	m.addLine("user", text)
	m.status = statusWorking
	m.err = ""
	sess := m.sess
	return m, func() tea.Msg {
		res, err := sess.Prompt(context.Background(), text)
		return promptDoneMsg{res, err}
	}
}

func (m Model) cancelCmd() tea.Cmd {
	sess := m.sess
	return func() tea.Msg {
		_ = sess.Cancel(context.Background())
		return nil
	}
}

func (m Model) requestQuit() (tea.Model, tea.Cmd) {
	m.quitting = true
	sess := m.sess
	working := m.status == statusWorking
	return m, tea.Sequence(func() tea.Msg {
		if working {
			_ = sess.Cancel(context.Background())
		}
		_ = sess.Close()
		return nil
	}, tea.Quit)
}

func (m *Model) applyEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventText:
		m.addLine("assistant", ev.Text)
	case agent.EventTool:
		name, status, id := "", "", ""
		if ev.Tool != nil {
			name, status, id = ev.Tool.Name, ev.Tool.Status, ev.Tool.ID
		}
		m.addLine("tool", fmt.Sprintf("%s %s %s", name, status, id))
	case agent.EventPermission:
		m.pending = ev.Permission
	case agent.EventDone:
		if ev.StopReason == "cancelled" {
			m.status = statusIdle
		}
	case agent.EventError:
		m.status = statusError
		if ev.Err != nil {
			m.err = ev.Err.Error()
			m.addLine("error", m.err)
		}
	}
}

func (m *Model) addLine(kind, text string) {
	if text == "" && kind != "user" {
		return
	}
	m.lines = append(m.lines, transcriptLine{kind: kind, text: text})
	m.refreshViewport()
}

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	footerH := 1
	composerH := 1
	overlayH := 0
	if m.pending != nil {
		overlayH = 1
	}
	h := m.height - footerH - composerH - overlayH
	if h < 1 {
		h = 1
	}
	m.vp.Width = m.width
	m.vp.Height = h
	m.input.Width = max(1, m.width-2)
	m.refreshViewport()
}

func (m *Model) refreshViewport() {
	var b strings.Builder
	for _, ln := range m.lines {
		switch ln.kind {
		case "user":
			b.WriteString(lipgloss.NewStyle().Foreground(m.theme.User).Render("you: " + ln.text))
		case "tool":
			b.WriteString(lipgloss.NewStyle().Foreground(m.theme.Tool).Render("tool " + ln.text))
		case "error":
			b.WriteString(lipgloss.NewStyle().Foreground(m.theme.Err).Render("error: " + ln.text))
		default:
			b.WriteString(lipgloss.NewStyle().Foreground(m.theme.Assistant).Render(ln.text))
		}
		b.WriteByte('\n')
	}
	m.vp.SetContent(strings.TrimRight(b.String(), "\n"))
	m.vp.GotoBottom()
}

func (m Model) View() string {
	if !m.ready {
		return "craze"
	}
	m.layout()
	composer := m.input.View()
	parts := []string{m.vp.View(), composer}
	if m.pending != nil {
		parts = append(parts, m.permissionOverlay())
	}
	parts = append(parts, m.footer())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
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
	if m.status == statusError && m.err != "" {
		st = "error"
	}
	line := fmt.Sprintf("%s  %s  %s  %s", m.model, perm, m.cwd, st)
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
