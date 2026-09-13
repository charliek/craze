package tui

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

const (
	// modelDialogTitle / themeDialogTitle name the two dialogs; the frame is
	// shared and the title is how the user tells them apart.
	modelDialogTitle      = "model"
	themeDialogTitle      = "theme"
	modelDialogHint       = "type to filter · ↑↓ · tab effort/fast · enter · esc"
	modelDialogHintEffort = "type to filter · ↑↓ · tab effort · enter · esc"
	modelDialogHintFast   = "type to filter · ↑↓ · tab fast · enter · esc"
	modelDialogHintPlain  = "type to filter · ↑↓ · enter · esc"
	// modelValueHint is the footer once focus is on a toggle row: ←/→ is the
	// key that now does something, and the filter still takes what is typed, so
	// the hint says both rather than dropping one of them.
	modelValueHint  = "←→ change · tab cycles · type filters · enter · esc"
	themeDialogHint = "↑↓ · enter keeps · esc reverts"
	// modelFilterPrompt is the filter's own prompt, two cells like the
	// composer's.
	modelFilterPrompt = "❯ "
	// fastOn / fastOff are the words the dialog and the notes use. The values
	// on the wire are the agent's ("true"/"false"); these are craze's.
	fastOn  = "on"
	fastOff = "off"
	// dialogValueSep separates a toggle row's label and its values; the chosen
	// one is bracketed, which is what makes the row readable without colour.
	dialogValueSep = "  "
)

// dialogFocus is the row ←/→ act on. The list is always first, which is where
// the filter's keystrokes go whatever has focus.
type dialogFocus int

const (
	focusList dialogFocus = iota
	focusEffort
	focusFast
)

// modelDialog is the model dialog's own state: what has been typed, which
// model is under the cursor, and the effort and fast values the user has
// chosen but not yet applied.
type modelDialog struct {
	filter textinput.Model
	sel    int
	focus  dialogFocus
	effort string
	fast   string
}

// modelApplyMsg is the result of the apply chain: the steps that landed, and
// the one that did not. A tea.Cmd cannot append to the transcript — only Update
// can — so the chain reports what happened and Update writes it.
//
// gen is the apply it belongs to. Two applies can be in flight at once (the box
// closes optimistically, so it can be reopened while the first chain is still
// running) and only the newest one speaks for what the rows show: an older
// one's failure must not put back a value the user has changed since.
type modelApplyMsg struct {
	gen  int
	done []applyStep
	step string
	err  error
}

// applyStep is one leg of the chain. cfgID empty means the model step, which
// has its own fallback.
type applyStep struct {
	cfgID string
	value string
	note  string
	label string
}

func (m Model) openModelDialog() Model {
	m = m.closeDialog(true)
	ti := textinput.New()
	ti.Prompt = modelFilterPrompt
	ti.Placeholder = ""
	ti.CharLimit = 64
	ti.PromptStyle = styleFG(m.theme.Accent)
	ti.TextStyle = styleFG(m.theme.FG)
	// A static cursor keeps the box from starting a blink timer nothing in
	// craze routes back to the input, and keeps a frame deterministic.
	ti.Cursor.SetMode(cursor.CursorStatic)
	ti.Focus()
	d := modelDialog{filter: ti}
	if m.showEffort() {
		if opt := agent.EffortOption(m.snap); opt != nil {
			d.effort = currentOrFirst(opt)
		}
	}
	if m.showFast() {
		if opt := agent.FastOption(m.snap); opt != nil {
			d.fast = currentOrFirst(opt)
		}
	}
	m.mdlg = d
	m.dialog = dialogModel
	return m
}

// currentOrFirst is the option's current value, or its first advertised one
// when the agent named a value it does not offer.
func currentOrFirst(opt *agent.ConfigOption) string {
	for _, v := range opt.SelectValues {
		if v.Value == opt.Current {
			return opt.Current
		}
	}
	if len(opt.SelectValues) > 0 {
		return opt.SelectValues[0].Value
	}
	return ""
}

// closeDialog leaves the modal layer. revert puts a previewed theme back,
// which is what Esc, a click outside and an arriving card all want; the model
// dialog applies nothing on the way out either way.
func (m Model) closeDialog(revert bool) Model {
	switch m.dialog {
	case dialogTheme:
		m.dialog = dialogNone
		if revert {
			m.applyTheme(m.themePrev)
		}
		m.themeNames = nil
		m.themeSel = 0
	case dialogModel:
		m.dialog = dialogNone
		m.mdlg = modelDialog{}
	case dialogHelp:
		m.dialog = dialogNone
		m.helpTop = 0
	case dialogProvider:
		// The picker is not dismissed without starting: Esc and a click
		// outside go through confirmProvider instead.
		m.dialog = dialogNone
	}
	return m
}

// dialogModelList is the filtered list, current model first: a case-insensitive
// substring of the display name or the id.
func (m Model) dialogModelList() []agent.ModelInfo {
	all := agent.OrderModels(m.snap)
	q := strings.ToLower(strings.TrimSpace(m.mdlg.filter.Value()))
	if q == "" {
		return all
	}
	out := make([]agent.ModelInfo, 0, len(all))
	for _, md := range all {
		if strings.Contains(strings.ToLower(md.Name), q) || strings.Contains(strings.ToLower(md.ID), q) {
			out = append(out, md)
		}
	}
	return out
}

// modelDialogFocuses is the Tab order: the list, then whichever of the two
// toggle rows the session actually advertises.
func (m Model) modelDialogFocuses() []dialogFocus {
	out := []dialogFocus{focusList}
	if m.showEffort() {
		out = append(out, focusEffort)
	}
	if m.showFast() {
		out = append(out, focusFast)
	}
	return out
}

func (m Model) handleModelDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		return m.closeDialog(true), nil
	case tea.KeyEnter:
		return m.applyModelDialog()
	case tea.KeyTab:
		m.mdlg.focus = cycleFocus(m.modelDialogFocuses(), m.mdlg.focus, 1)
		return m, nil
	case tea.KeyShiftTab:
		m.mdlg.focus = cycleFocus(m.modelDialogFocuses(), m.mdlg.focus, -1)
		return m, nil
	case tea.KeyUp, tea.KeyDown:
		n := len(m.dialogModelList())
		if n == 0 {
			return m, nil
		}
		delta := 1
		if msg.Type == tea.KeyUp {
			delta = -1
		}
		m.mdlg.sel = (m.mdlg.sel + delta + n) % n
		return m, nil
	case tea.KeyLeft, tea.KeyRight:
		delta := 1
		if msg.Type == tea.KeyLeft {
			delta = -1
		}
		return m.moveDialogValue(delta), nil
	}
	// Everything else is filtering, whatever has focus: the footer promises
	// "type to filter" and the toggle rows have no text of their own.
	var cmd tea.Cmd
	m.mdlg.filter, cmd = m.mdlg.filter.Update(msg)
	if n := len(m.dialogModelList()); m.mdlg.sel >= n {
		m.mdlg.sel = max(n-1, 0)
	}
	return m, cmd
}

func cycleFocus(all []dialogFocus, cur dialogFocus, delta int) dialogFocus {
	for i, f := range all {
		if f == cur {
			return all[(i+delta+len(all))%len(all)]
		}
	}
	return all[0]
}

// moveDialogValue steps the focused toggle row along its advertised values.
func (m Model) moveDialogValue(delta int) Model {
	var opt *agent.ConfigOption
	cur := ""
	switch m.mdlg.focus {
	case focusEffort:
		opt, cur = agent.EffortOption(m.snap), m.mdlg.effort
	case focusFast:
		opt, cur = agent.FastOption(m.snap), m.mdlg.fast
	default:
		return m
	}
	if opt == nil || len(opt.SelectValues) == 0 {
		return m
	}
	idx := 0
	for i, v := range opt.SelectValues {
		if v.Value == cur {
			idx = i
			break
		}
	}
	next := opt.SelectValues[(idx+delta+len(opt.SelectValues))%len(opt.SelectValues)].Value
	if m.mdlg.focus == focusEffort {
		m.mdlg.effort = next
	} else {
		m.mdlg.fast = next
	}
	return m
}

// applyModelDialog is Enter: the box closes optimistically, and one chained
// command applies model → effort → fast, each step only if it changed. Notes
// are written for the steps that landed and an error names the one that did
// not, so a half-applied change is visible rather than guessed at.
func (m Model) applyModelDialog() (tea.Model, tea.Cmd) {
	list := m.dialogModelList()
	d := m.mdlg
	m = m.closeDialog(false)

	var steps []applyStep
	if d.sel >= 0 && d.sel < len(list) && list[d.sel].ID != m.snap.CurrentModel {
		id := list[d.sel].ID
		steps = append(steps, applyStep{value: id, note: "model → " + id, label: "model"})
		m.snap.CurrentModel = id
		m.model = id
	}
	if m.showEffort() {
		if opt := agent.EffortOption(m.snap); opt != nil && d.effort != "" && d.effort != opt.Current {
			steps = append(steps, applyStep{
				cfgID: opt.ID, value: d.effort, note: "effort → " + d.effort, label: "effort",
			})
			m = m.setConfigCurrent(opt.ID, d.effort)
		}
	}
	if m.showFast() {
		if opt := agent.FastOption(m.snap); opt != nil && d.fast != "" && d.fast != opt.Current {
			steps = append(steps, applyStep{
				cfgID: opt.ID, value: d.fast, note: "fast → " + fastWord(opt, d.fast), label: "fast",
			})
			m = m.setConfigCurrent(opt.ID, d.fast)
		}
	}
	if len(steps) == 0 {
		return m, nil
	}

	sess := m.sess
	modelCfgID := ""
	if opt := agent.ModelConfigOption(m.snap); opt != nil {
		modelCfgID = opt.ID
	}
	m.applyGen++
	gen := m.applyGen
	return m, func() tea.Msg {
		ctx := context.Background()
		out := modelApplyMsg{gen: gen}
		for _, st := range steps {
			err := applyOneStep(ctx, sess, st, modelCfgID)
			if err != nil {
				out.step, out.err = st.label, err
				return out
			}
			out.done = append(out.done, st)
		}
		return out
	}
}

// settleStep writes a step the agent accepted into the snapshot. It is the
// optimistic write made good: the session's own snapshot does not always carry
// what landed — the model step's fallback writes a config option and leaves
// CurrentModel alone — so the steps that succeeded have the last word over the
// refresh.
func (m Model) settleStep(st applyStep) Model {
	if st.cfgID != "" {
		return m.setConfigCurrent(st.cfgID, st.value)
	}
	m.snap.CurrentModel = st.value
	m.model = st.value
	return m
}

// applyOneStep runs one leg. The model step keeps the fallback /model has
// today: an agent without session/set_model may still take the model as a
// config option.
func applyOneStep(ctx context.Context, sess agent.Session, st applyStep, modelCfgID string) error {
	if st.cfgID != "" {
		return sess.SetConfig(ctx, st.cfgID, st.value)
	}
	err := sess.SetModel(ctx, st.value)
	if err != nil && modelCfgID != "" {
		if err2 := sess.SetConfig(ctx, modelCfgID, st.value); err2 == nil {
			return nil
		}
	}
	return err
}

// setConfigCurrent writes an option's new value into this model's own
// snapshot, copy-on-write: every Model copy shares the slice, so the optimistic
// value must not be written through into the one bubbletea already discarded.
func (m Model) setConfigCurrent(id, value string) Model {
	cfg := append([]agent.ConfigOption(nil), m.snap.Config...)
	for i := range cfg {
		if cfg[i].ID == id {
			cfg[i].Current = value
		}
	}
	m.snap.Config = cfg
	return m
}

// fastWord names a fast value the way the user picked it, not the way the
// agent spells it on the wire.
func fastWord(opt *agent.ConfigOption, value string) string {
	off, on, ok := agent.FastOnOff(opt)
	switch {
	case ok && value == on:
		return fastOn
	case ok && value == off:
		return fastOff
	}
	return value
}

// modelDialogRows is what fits: the list length, and whether each of the
// single rows is still drawn.
type modelDialogRows struct {
	list               int
	footer, filter     bool
	fastRow, effortRow bool
}

func (r modelDialogRows) total() int {
	n := 1 + r.list // the title row is never dropped
	for _, on := range []bool{r.footer, r.filter, r.fastRow, r.effortRow} {
		if on {
			n++
		}
	}
	return n
}

// fitModelDialog drops rows in the pinned order until the box fits its budget:
// the list shrinks first, then the footer hint, then the filter, and the two
// toggle rows last because nothing else in craze can set them.
func fitModelDialog(budget, list int, effort, fast bool) modelDialogRows {
	r := modelDialogRows{list: list, footer: true, filter: true, fastRow: fast, effortRow: effort}
	for r.total() > budget {
		switch {
		case r.list > 0:
			r.list--
		case r.footer:
			r.footer = false
		case r.filter:
			r.filter = false
		case r.fastRow:
			r.fastRow = false
		case r.effortRow:
			r.effortRow = false
		default:
			return r
		}
	}
	return r
}

// modelDialogPlan is what the box draws at one inner height: which rows
// survived the budget, and the window onto the filtered list. The renderer and
// the hit-tester both take it, so a click can never land on a row that was not
// drawn.
type modelDialogPlan struct {
	rows         modelDialogRows
	list         []agent.ModelInfo
	top, shown   int
	effort, fast *agent.ConfigOption
}

func (m Model) modelDialogPlan(budget int) modelDialogPlan {
	list := m.dialogModelList()
	var effort, fast *agent.ConfigOption
	if m.showEffort() {
		effort = agent.EffortOption(m.snap)
	}
	if m.showFast() {
		fast = agent.FastOption(m.snap)
	}
	rows := fitModelDialog(budget, min(len(list), dialogListMax), effort != nil, fast != nil)
	top, shown := dialogListWindow(len(list), m.mdlg.sel, rows.list)
	return modelDialogPlan{rows: rows, list: list, top: top, shown: shown, effort: effort, fast: fast}
}

func (m Model) modelDialogHintText() string {
	switch {
	case m.showEffort() && m.showFast():
		return modelDialogHint
	case m.showEffort():
		return modelDialogHintEffort
	case m.showFast():
		return modelDialogHintFast
	default:
		return modelDialogHintPlain
	}
}

func (m Model) modelDialogBody(inner, budget int) []string {
	p := m.modelDialogPlan(budget)
	rows := []string{m.dialogTitle(modelDialogTitle, inner)}
	if p.rows.filter {
		f := m.mdlg.filter
		// The prompt and the one cell the cursor always draws come out of the
		// width bubbles pads the value to, or the row overflows the box.
		f.Width = max(1, inner-lipgloss.Width(modelFilterPrompt)-1)
		rows = append(rows, f.View())
	}
	for i := 0; i < p.shown; i++ {
		md := p.list[p.top+i]
		tag := dialogScrollTag(i, p.top, p.shown, len(p.list))
		if md.ID == m.snap.CurrentModel {
			tag = strings.TrimSpace("current " + tag)
		}
		rows = append(rows, m.dialogRow(modelRowText(md), tag, p.top+i == m.mdlg.sel, m.mdlg.focus == focusList, inner))
	}
	if p.rows.effortRow {
		rows = append(rows, m.dialogValueRow("effort", p.effort, m.mdlg.effort, m.mdlg.focus == focusEffort, inner, nil))
	}
	if p.rows.fastRow {
		rows = append(rows, m.dialogValueRow("fast", p.fast, m.mdlg.fast, m.mdlg.focus == focusFast, inner, fastLabel(p.fast)))
	}
	if p.rows.footer {
		hint := m.modelDialogHintText()
		if m.mdlg.focus != focusList {
			hint = modelValueHint
		}
		rows = append(rows, m.dialogFooter(hint, inner))
	}
	return rows
}

// modelDialogClick maps a body row onto what it draws: a list row picks that
// model and applies, a toggle row takes the focus so the arrows reach it.
func (m Model) modelDialogClick(i int) (tea.Model, tea.Cmd) {
	p := m.modelDialogPlan(m.lay.Dialog.H - dialogBorder)
	row := i - 1 // the title row
	if p.rows.filter {
		row--
	}
	switch {
	case row < 0:
		return m, nil
	case row < p.shown:
		m.mdlg.sel = p.top + row
		return m.applyModelDialog()
	}
	row -= p.shown
	if p.rows.effortRow {
		if row == 0 {
			m.mdlg.focus = focusEffort
			return m, nil
		}
		row--
	}
	if p.rows.fastRow && row == 0 {
		m.mdlg.focus = focusFast
	}
	return m, nil
}

// modelRowText is the display name, with the id beside it when they differ, so
// a filter that matched the id shows why.
func modelRowText(md agent.ModelInfo) string {
	if md.Name == "" || md.Name == md.ID {
		return md.ID
	}
	return md.Name
}

// fastLabel renames the fast option's advertised values to on/off, which is
// what the row promises and what the note writes.
func fastLabel(opt *agent.ConfigOption) func(string) string {
	if opt == nil {
		return nil
	}
	return func(v string) string { return fastWord(opt, v) }
}

// dialogValueRow is one toggle row: the focus gutter, the label, then every
// advertised value with the chosen one in brackets. "[value]" says which value
// is picked and the gutter says whether this row is where the keys are — two
// different questions, so they get two different marks.
//
// The focused row also takes the SelectionBG band the list cursor uses, which
// is reinforcement and not the signal: the gutter is what survives an ANSI
// strip.
func (m Model) dialogValueRow(label string, opt *agent.ConfigOption, chosen string, focused bool, inner int, name func(string) string) string {
	base, pick := styleFG(m.theme.Dim), styleFG(m.theme.Bright)
	if focused {
		band := lipgloss.NewStyle().Background(m.theme.SelectionBG)
		base, pick = band.Foreground(m.theme.FG), band.Foreground(m.theme.Bright).Bold(true)
	}
	head := dialogMark(focused, false) + label
	segs := []seg{{head, base}}
	used := lipgloss.Width(head)
	for _, v := range opt.SelectValues {
		// The values are the agent's text too, so they are folded onto the one
		// line the row is allowed to be.
		text := sanitizeLine(v.Value)
		if name != nil {
			text = sanitizeLine(name(v.Value))
		}
		st := base
		if v.Value == chosen {
			text, st = "["+text+"]", pick
		}
		segs = append(segs, seg{dialogValueSep, base}, seg{text, st})
		used += lipgloss.Width(dialogValueSep) + lipgloss.Width(text)
	}
	// The band is only a band if it reaches the far border, so the row pays for
	// its own padding instead of leaving it to the unstyled fill in dialogView.
	if pad := inner - used; focused && pad > 0 {
		segs = append(segs, seg{strings.Repeat(" ", pad), base})
	}
	return renderSegs(inner, segs...)
}
