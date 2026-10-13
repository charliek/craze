package tui

import (
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// /effort in a session (plan 047 D4, O5): a bare /effort opens a picker of
// the current model's levels, and /effort <level> sets one. It is a builtin
// only where the session has an effort option for its current model
// (effortOffered) — the client's own reading of the snapshot, with no
// capability bit (P8) — and anywhere else it is the text it always was, so
// an agent's or a plugin's own /effort is what the menu shows and Enter sends.
// Like /model it needs the agent, so it is refused while a turn runs
// (handleEnter, runsWhileWorking), and it is refused while a model switch of
// this client's is pending (modelPending, plan 047 C2 review F1): the status
// row then names the model asked for while the effort option is still the
// one the session was on, and a picker would mix the two.
//
// The change goes through the engine setting /model's effort step sends
// (runModelEffort): the session's effort option, bound to the model it was
// chosen for (Setting.ForModel), with that step's notes — a change the
// session no longer takes is a note, a refusal an error row — and in this
// client's line of model changes (chainLock), behind any switch issued
// before it.

// effortBuiltin is /effort's row: in the slash catalog after /exit (so `/e`
// still selects /exit and `/ef` /effort), shown where the session has an
// effort option alone (its Requires, effortOffered). builtinNamed never
// names it — it is not reserved.
var effortBuiltin = tableBuiltin("effort")

const (
	// effortPickerTitle names the picker; the model it lists is named after
	// it, since the levels are that model's.
	effortPickerTitle = "effort"
	// effortPickerHint is the picker's footer.
	effortPickerHint = "↑↓ · enter · esc"
	// effortCurrentTag and effortDefaultTag mark the level in use and the
	// model's default (known with effort facts alone) at the row's right.
	effortCurrentTag = "current"
	effortDefaultTag = "default"
)

// effortOffered reports whether this session has /effort: an effort option
// for its current model (agent.EffortOption, plan 047 P8). That option always
// offers at least one level (EffortOption admits no empty select), so where
// there is one the picker has rows.
func (m Model) effortOffered() bool { return agent.EffortOption(m.snap) != nil }

// effortClaimed reports whether /effort is craze's own now, for the menu and
// for Enter alike: where the session has an effort option, and while a model
// switch of this client's is pending — whatever the option of the model it
// is leaving says — so a typed /effort then is refused (runEffort) rather
// than sent to an agent as text.
func (m Model) effortClaimed() bool { return m.effortOffered() || m.modelPending() }

// effortPendingText is /effort refused while a model switch of this client's
// is pending (F1): the effort option is still the old model's.
func effortPendingText(model string) string {
	return "The model is still switching to " + sanitizeLine(model) + "; /effort once it has."
}

// effortFacts reports whether the session states every model's effort facts
// (backend.SessionInfo.EffortFacts, plan 047 D2): a native session, in
// process or through a host that says capabilities.effortFacts. A client
// without it knows the current model's levels alone, from its effort option.
func (m Model) effortFacts() bool { return m.eng != nil && m.info().EffortFacts }

// listedModel is model id as the session lists it, and false when its list
// does not hold it.
func (m Model) listedModel(id string) (agent.ModelInfo, bool) {
	i := slices.IndexFunc(m.snap.Models, func(md agent.ModelInfo) bool { return md.ID == id })
	if i < 0 {
		return agent.ModelInfo{}, false
	}
	return m.snap.Models[i], true
}

// effortDefault is the default effort of the model the effort option belongs
// to (configModel), as its catalog row states it — known only on a session
// with effort facts; "" on any other, and for a model with no default.
func (m Model) effortDefault() string {
	if !m.effortFacts() {
		return ""
	}
	md, _ := m.listedModel(m.configModel())
	return md.DefaultEffort
}

// effortPicker is the picker's own state: the level under the cursor, by
// value. The picker renders from the live snapshot, so its levels are always
// the current model's — another model's once the session has switched under
// the open box — and the cursor stays on that value where the model offers
// it, else goes to the level in use (sel).
type effortPicker struct {
	hl string
}

// sel is the cursor's row among opt's values: the highlighted value, else the
// level in use, else the first.
func (p effortPicker) sel(opt *agent.ConfigOption) int {
	for _, want := range []string{p.hl, opt.Current} {
		if i := slices.IndexFunc(opt.SelectValues, func(v agent.SelectValue) bool { return v.Value == want }); i >= 0 {
			return i
		}
	}
	return 0
}

// openEffortPicker is a bare /effort: the picker, on the level in use.
func (m Model) openEffortPicker() Model {
	m = m.closeDialog(true)
	m.dialog = dialogEffort
	m.edlg = effortPicker{}
	if opt := agent.EffortOption(m.snap); opt != nil {
		m.edlg.hl = opt.Current
	}
	return m
}

func (m Model) handleEffortPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		return m.closeDialog(true), nil
	case tea.KeyEnter:
		return m.applyEffortPicker()
	case tea.KeyUp, tea.KeyDown:
		opt := agent.EffortOption(m.snap)
		if opt == nil {
			return m, nil
		}
		delta := 1
		if msg.Type == tea.KeyUp {
			delta = -1
		}
		n := len(opt.SelectValues)
		m.edlg.hl = opt.SelectValues[(m.edlg.sel(opt)+delta+n)%n].Value
	}
	return m, nil
}

// applyEffortPicker is Enter: the box closes, and the level under the cursor
// is applied to the model the picker lists — the model the effort option
// belongs to (configModel), which the title names (applyEffort). The level in
// use is sent too: the screen can be behind a change of this client's still
// waiting for its answer, and a choice dropped for equalling it would be lost
// when that change lands (the model dialog's rule, astra r6); the agent takes
// a write of the value it holds as no change. With no effort option — the
// session moved to a model without one under the open box — nothing is sent.
func (m Model) applyEffortPicker() (tea.Model, tea.Cmd) {
	opt, model, p := agent.EffortOption(m.snap), m.configModel(), m.edlg
	m = m.closeDialog(true)
	if opt == nil {
		return m, nil
	}
	return m.applyEffort(model, opt.SelectValues[p.sel(opt)].Value)
}

// runEffort is /effort's builtin (runBuiltin): bare, the picker; with an
// argument, that level for the model the effort option belongs to. While a
// model switch of this client's is pending neither runs (effortPendingText).
// `default` is reserved and decided first (F2): the model's default, which
// only a session with effort facts states, and refused anywhere else — even
// where an agent advertises a level spelled "default", which the picker
// still reaches. Any other argument is matched by agent.MatchEffortValue —
// the id exactly, then in any case, then the name; answered in the agent's
// spelling. What matches nothing is refused with an error row naming the
// levels, and the draft is kept for the user to correct.
func (m Model) runEffort(args string) (tea.Model, tea.Cmd) {
	if m.modelPending() {
		m.addError(effortPendingText(m.snap.CurrentModel))
		return m, nil
	}
	if args == "" {
		m.input.SetValue("")
		return m.openEffortPicker(), nil
	}
	opt := agent.EffortOption(m.snap)
	if opt == nil {
		// handleEnter dispatches /effort only with an option; a session that
		// lost it in between refuses rather than sending the text.
		m.addError("the model has no effort levels")
		return m, nil
	}
	def := m.effortDefault()
	reserved := strings.EqualFold(strings.TrimSpace(args), effortDefaultTag)
	level, ok := def, def != "" && offers(opt, def)
	if !reserved {
		level, ok = agent.MatchEffortValue(opt, args)
	}
	if !ok {
		m.addError(effortRefusal(opt, def, args, reserved))
		return m, nil
	}
	m.input.SetValue("")
	return m.applyEffort(m.configModel(), level)
}

// effortRefusal is the error row of an /effort argument that names no level of
// opt: what was typed and what would do — `default` first where the model has
// one it states (effort facts), then the levels as the agent spells them, but
// for a level spelled "default" when the reserved word was what failed: typed,
// it is the reserved word again.
func effortRefusal(opt *agent.ConfigOption, def, typed string, reserved bool) string {
	var one []string
	if def != "" && offers(opt, def) {
		one = append(one, effortDefaultTag)
	}
	for _, v := range opt.SelectValues {
		if reserved && strings.EqualFold(v.Value, effortDefaultTag) {
			continue
		}
		one = append(one, sanitizeLine(v.Value))
	}
	return "no effort " + sanitizeLine(strings.TrimSpace(typed)) + ": one of " + strings.Join(one, ", ")
}

// applyEffort is level applied to model's effort: the engine setting
// /model's effort step sends (runModelEffort) — read the session's settings,
// judge level against the effort option it holds for model, and set it bound
// to model — with that step's notes and error rows, in this client's line of
// model changes (chainLock). Nothing is shown before it lands; its answer
// (effortSetMsg) holds the confirmed value until the fold reaches it. On a
// native session the change is remembered (plan 047 D1), as /model's is.
func (m Model) applyEffort(model, level string) (tea.Model, tea.Cmd) {
	if m.eng == nil {
		return m, nil
	}
	m.applyGen++
	eng, cmd, at := m.eng, m.nextCmd(), m.configRev
	landed := modelLanded{gen: m.applyGen}
	prov, iss, ctx := m.snap.Provider, m.issue(), dispatchCtx(m.eng)
	return m, m.chains.take(func() tea.Msg {
		return runModelEffort(ctx, iss, eng, prov, cmd, model, level, landed, at)
	})
}

// effortPickerTitleText is the picker's title: what it sets, and for which
// model — the one its levels are, the model the effort option belongs to
// (configModel).
func (m Model) effortPickerTitleText() string {
	name := m.configModel()
	if md, ok := m.listedModel(name); ok {
		name = modelRowText(md)
	}
	if name == "" {
		return effortPickerTitle
	}
	return effortPickerTitle + " · " + sanitizeLine(name)
}

// effortPickerPlan is the window onto the levels and whether the footer
// survived (dialogListPlan, the theme picker's rule). The renderer and the
// hit-tester both take it.
func (m Model) effortPickerPlan(opt *agent.ConfigOption, budget int) (top, shown int, footer bool) {
	return dialogListPlan(len(opt.SelectValues), m.edlg.sel(opt), budget)
}

// effortPickerBody draws the picker from the live snapshot: one row per level
// of the current model's effort option, in its order, the cursor's in the
// gutter, the level in use marked current and the model's default — where
// the session states it (effort facts) — marked default.
func (m Model) effortPickerBody(inner, budget int) []string {
	rows := []string{m.dialogTitle(m.effortPickerTitleText(), inner)}
	opt := agent.EffortOption(m.snap)
	if opt == nil {
		rows = append(rows, m.dialogRowDim("this model has no effort levels", "", false, false, true, inner))
		return append(rows, m.dialogFooter("esc", inner))
	}
	def := m.effortDefault()
	top, shown, footer := m.effortPickerPlan(opt, budget)
	sel := m.edlg.sel(opt)
	for i := 0; i < shown; i++ {
		v := opt.SelectValues[top+i].Value
		var tags []string
		if v == opt.Current {
			tags = append(tags, effortCurrentTag)
		}
		if def != "" && v == def {
			tags = append(tags, effortDefaultTag)
		}
		if mark := dialogScrollTag(i, top, shown, len(opt.SelectValues)); mark != "" {
			tags = append(tags, mark)
		}
		rows = append(rows, m.dialogRow(v, strings.Join(tags, " · "), top+i == sel, true, inner))
	}
	if footer {
		rows = append(rows, m.dialogFooter(effortPickerHint, inner))
	}
	return rows
}

// effortPickerClick picks the level under the pointer and applies it, which
// is Enter under the pointer.
func (m Model) effortPickerClick(i int) (tea.Model, tea.Cmd) {
	opt := agent.EffortOption(m.snap)
	if opt == nil {
		return m, nil
	}
	top, shown, _ := m.effortPickerPlan(opt, m.lay.Dialog.H-dialogBorder)
	row := i - 1 // the title row
	if row < 0 || row >= shown {
		return m, nil
	}
	m.edlg.hl = opt.SelectValues[top+row].Value
	return m.applyEffortPicker()
}
