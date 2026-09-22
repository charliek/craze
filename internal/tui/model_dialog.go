package tui

import (
	"context"
	"errors"
	"maps"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

const (
	// modelDialogTitle / themeDialogTitle name the two dialogs; the frame is
	// shared and the title is how the user tells them apart.
	modelDialogTitle = "model"
	themeDialogTitle = "theme"
	// modelHintHead and modelHintTail bracket the list's footer. Between them
	// goes "tab " and the tabs' labels joined by "/" when the catalog has any
	// (modelDialogHintText), which for effort and fast is modelDialogHint —
	// the footer every golden over the Stub's catalog is drawn with.
	modelHintHead        = "type to filter · ↑↓ · "
	modelHintTail        = "enter · esc"
	modelDialogHint      = modelHintHead + "tab effort/fast · " + modelHintTail
	modelDialogHintPlain = modelHintHead + modelHintTail
	// modelDialogHintOptions stands in for the tabs' labels when they do not
	// fit the box (modelDialogHintAt): four tabs' names are wider than the
	// box, and clamped they would cut off the enter · esc every footer ends on.
	modelDialogHintOptions = modelHintHead + "tab options · " + modelHintTail
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

// dialogFocus is the row ←/→ act on: focusList, or a tab named by the id of
// the option it sets. The list is always first, which is where the filter's
// keystrokes go whatever has focus.
//
// An id and not a position, because the tabs are read off the catalog every
// time they are asked for and a delta can add, drop or reorder options under
// an open box: a position would move the focus onto another option without the
// user touching a key, where an id either still names its option or names
// nothing, which repaired turns back into the list.
type dialogFocus string

// focusList is the model list. No tab can take it: an option with an empty id
// is never a tab (isDialogTab), since nothing could set it.
const focusList dialogFocus = ""

// modelDialog is the model dialog's own state: what has been typed, which
// model is under the cursor, which row has the keys, and the value chosen on
// each tab but not yet applied.
type modelDialog struct {
	filter textinput.Model
	sel    int
	focus  dialogFocus
	// chosen is each tab's value, by option id: seeded from currentOrFirst for
	// every tab when the box opens, and for a tab that appears while it is
	// open the first time it is needed (repaired). It is copied on write, like
	// setConfigCurrent's slice: every Model copy shares the map, and a key
	// must not write through into a copy bubbletea has already discarded.
	chosen map[string]string
	// touched is the tabs the user has moved (choose), by option id, copied on
	// write like chosen. Only a touched tab is a choice: an untouched one
	// shows its option's live value, following every delta that moves it
	// (repaired), and Enter sends nothing for it. Without the distinction a
	// value seeded when the box opened would be sent back on Enter after
	// another client or the agent had changed the option under the open box —
	// undoing a change nobody in this dialog asked to undo (plan 025 X10 (f),
	// panel astra 8).
	touched map[string]bool
}

// tabRole is what a tab is to the rest of craze. Effort and fast have a
// presentation of their own, byte for byte what the dialog drew before its
// rows came from the catalog (plan 025 design 4), and they are found by role
// on the model a chain switches to, whatever that model calls them (X3). Every
// other option is roleOther: its advertised name, its raw values, its id.
type tabRole int

const (
	roleOther tabRole = iota
	roleEffort
	roleFast
)

// modelTab is one row under the model list: an option the current catalog
// advertises, what the row calls it, and its role.
type modelTab struct {
	opt   agent.ConfigOption
	label string
	role  tabRole
}

func (t modelTab) focus() dialogFocus { return dialogFocus(t.opt.ID) }

// valueName is how the row spells a value: fast's as on/off, which is what the
// row promises and the note writes, and every other tab's as the agent sent it.
func (t modelTab) valueName() func(string) string {
	if t.role == roleFast {
		return fastLabel(&t.opt)
	}
	return nil
}

// modeCategory is the category cursor files its mode option under. The mode
// has its own chip, its own commands and Shift+Tab, so it is never a tab here.
const modeCategory = "mode"

// isDialogTab reports whether an advertised option gets a row: a select with
// at least one value to pick, and neither the mode nor the model — which are
// told apart by their semantic category first and by id second (plan 025
// design 4, astra 12), since the model has the list above and the mode its
// chip. A type left empty is read as a select, as isEffortSelect reads it.
func isDialogTab(opt agent.ConfigOption) bool {
	switch {
	case opt.ID == "":
		return false
	case opt.Type != "" && opt.Type != "select":
		return false
	case len(opt.SelectValues) == 0:
		return false
	case opt.Category == modeCategory || agent.IsModelConfigOption(opt):
		return false
	case strings.EqualFold(opt.ID, "mode") || strings.EqualFold(opt.ID, "model"):
		return false
	}
	return true
}

// catalogTabs is the dialog's tab list over a snapshot: one tab per option
// isDialogTab admits, in catalog order, each id once.
//
// An option the catalog advertises is shown, capability bit or not. The
// provider's Effort and FastToggle bits (showEffort, showFast) say which chips
// the status row draws; they do not hide a control the model itself
// advertises, because the agent's catalog is the one authority on what the
// current model takes and a bit fixed per provider cannot know that per model
// (plan 025 design 4, CodeRabbit 11).
//
// An id the agent sent twice is its first occurrence — the one the session
// sets and confirms, since its install finds the first option with the id —
// and the roles are found among the occurrences kept, never across the whole
// list: a role is matched by id, and an EffortOption that chose a later
// occurrence would hand its role to a row that draws another option's name
// and values (astra r4 item 6).
func catalogTabs(snap agent.Snapshot) []modelTab {
	seen := make(map[string]bool, len(snap.Config))
	kept := make([]agent.ConfigOption, 0, len(snap.Config))
	for _, opt := range snap.Config {
		if seen[opt.ID] {
			continue
		}
		seen[opt.ID] = true
		kept = append(kept, opt)
	}
	once := snap
	once.Config = kept
	effort, fast := agent.EffortOption(once), agent.FastOption(once)
	var out []modelTab
	for _, opt := range kept {
		if !isDialogTab(opt) {
			continue
		}
		t := modelTab{opt: opt, label: tabLabel(opt)}
		switch {
		case effort != nil && opt.ID == effort.ID:
			t.role, t.label = roleEffort, "effort"
		case fast != nil && opt.ID == fast.ID:
			t.role, t.label = roleFast, "fast"
		}
		out = append(out, t)
	}
	return out
}

// tabLabel is a tab's label when it is neither effort nor fast: the name the
// agent advertises, lowercased like those two, and folded onto the one line a
// row is allowed to be — the agent's text, so its id stands in when it sent no
// name at all.
func tabLabel(opt agent.ConfigOption) string {
	if name := sanitizeLine(opt.Name); name != "" {
		return strings.ToLower(name)
	}
	return strings.ToLower(sanitizeLine(opt.ID))
}

// modelDialogTabs is the tab list over this model's snapshot, derived each
// time it is read — at render, at a key, at a click — so it is always the
// current model's catalog and never one captured when the box opened.
func (m Model) modelDialogTabs() []modelTab { return catalogTabs(m.snap) }

// tabIndex is the tab f names, or -1 — for focusList among others, since no
// tab has an empty id.
func tabIndex(tabs []modelTab, f dialogFocus) int {
	for i, t := range tabs {
		if t.focus() == f {
			return i
		}
	}
	return -1
}

// repaired is the dialog made consistent with the tabs the catalog advertises
// now. A delta can arrive while the box is open — another client's model
// change brings another model's catalog, or the agent moves an option — so the
// option the focused tab set can be gone, an option's live value can have
// moved, and a chosen value can be one its option no longer offers. The focus
// then goes back to the list; a tab the user has not touched takes its
// option's live value (currentOrFirst), whatever it was showing; and a touched
// tab whose value its option no longer offers takes it too, and is untouched
// again — what it showed was the user's choice, and that choice is gone, so
// the value it falls back to is nobody's choice and must not be sent. A value
// chosen for an option that has gone is kept, and counts again if the option
// comes back offering it.
//
// It acts on the snapshot the model observes, not on every catalog the
// session passed through: a delta carries revisions, and refreshSnap reads the
// session as it is when the event is handled. So deltas that take a catalog
// away and bring it back (A→B→A) before Update sees either leave the focus
// and a touched choice standing, which is correct rather than a missed repair
// (plan 025 X13, astra r4 item 5): the choice is again one its option offers,
// on the model it was chosen for, and whatever Enter then sends is still bound
// to that model and re-judged on the latest catalog before it goes
// (runModelApply). The repair happens when a snapshot without the option IS
// observed.
func (d modelDialog) repaired(tabs []modelTab) modelDialog {
	if tabIndex(tabs, d.focus) < 0 {
		d.focus = focusList
	}
	var chosen map[string]string
	var touched map[string]bool
	for _, t := range tabs {
		id, live := t.opt.ID, currentOrFirst(&t.opt)
		v, ok := d.chosen[id]
		switch {
		case ok && d.touched[id] && offers(&t.opt, v):
			continue
		case ok && !d.touched[id] && v == live:
			continue
		}
		if chosen == nil {
			chosen = make(map[string]string, len(tabs))
			maps.Copy(chosen, d.chosen)
		}
		chosen[id] = live
		if d.touched[id] {
			if touched == nil {
				touched = maps.Clone(d.touched)
			}
			delete(touched, id)
		}
	}
	if chosen != nil {
		d.chosen = chosen
	}
	if touched != nil {
		d.touched = touched
	}
	return d
}

// choose is the dialog with one tab's value picked by the user, and the tab
// touched, each on a fresh map.
func (d modelDialog) choose(id, value string) modelDialog {
	next := make(map[string]string, len(d.chosen)+1)
	maps.Copy(next, d.chosen)
	next[id] = value
	d.chosen = next
	touched := make(map[string]bool, len(d.touched)+1)
	maps.Copy(touched, d.touched)
	touched[id] = true
	d.touched = touched
	return d
}

// offers reports whether opt advertises value, exactly as spelled.
func offers(opt *agent.ConfigOption, value string) bool {
	for _, v := range opt.SelectValues {
		if v.Value == value {
			return true
		}
	}
	return false
}

// modelApplyMsg is the result of the apply chain: the steps it got through, in
// order, and the one that failed. A tea.Cmd cannot append to the transcript —
// only Update can — so the chain reports what happened and Update writes it.
//
// done holds every step that landed and every step the chain noted instead of
// applying (applyStep.skipped), each with its note, so the transcript reads in
// the order the chain ran.
//
// gen is the apply it belongs to. Two applies can be in flight at once (the box
// closes optimistically, so it can be reopened while the first chain is still
// running) and only the newest one speaks for what the rows show: an older
// one's failure must not put back a value the user has changed since.
//
// unread is the model step failing on an answer the session could not read
// (agent.ErrBadCatalog): the chain ends there as on any failure, but nothing
// says the model was refused — the agent may have switched — so the row says
// the outcome is unknown (unreadModelText) rather than naming a refusal.
type modelApplyMsg struct {
	gen    int
	done   []applyStep
	step   string
	err    error
	unread bool
}

// applyStep is one leg of the chain. cfgID empty means the model step, which
// has its own fallback.
//
// at is the revision its section stood at when the chain was built, and rev the
// revision the engine confirmed it at — together, what settleStep needs to know
// whether writing this step's value would be writing over something newer
// (plan 021 §3.8, panel astra 15).
type applyStep struct {
	cfgID string
	// value is what the step asks for until it lands, and from then on what
	// the session installed (landed): the value the agent's answer holds,
	// which is not always the one asked for (plan 025 design 1).
	value string
	note  string
	label string
	// role and opt are an option step's tab: its role, and the option its
	// value belongs to — the one it was chosen on, until the chain re-resolves
	// it on the catalog the session holds just before sending it (resolveOn),
	// and that catalog's after.
	role tabRole
	opt  agent.ConfigOption
	// skipped is a step the chain did not apply: its note is the X4 note that
	// says why, and it has nothing to settle.
	skipped bool
	at      uint64
	rev     uint64
}

// landedNote is what a step that landed writes: the arrow note, fast spelled
// on/off, and the value as it is now — the installed one, once it has landed.
func (st applyStep) landedNote() string {
	switch {
	case st.cfgID == "":
		return "model → " + st.value
	case st.role == roleFast:
		return "fast → " + noteValue(fastWord(&st.opt, st.value))
	}
	return st.label + " → " + noteValue(st.value)
}

// noteValue is how a note spells an option's value: as the agent spells it,
// and an empty one as "" — an agent may install an empty value (its catalog
// parser accepts one), and a note that ends on its arrow names nothing.
func noteValue(v string) string {
	if v == "" {
		return `""`
	}
	return v
}

// shownValue is the step's value as its row showed it, which is how a note
// that names the value names it.
func (st applyStep) shownValue() string {
	if st.role == roleFast {
		return fastWord(&st.opt, st.value)
	}
	return st.value
}

// landed is the step as the engine confirmed it: the value the session is now
// at (SetResult.Value), the revision that was committed at, and the note
// rewritten to name what landed. The value is taken as it is, empty included:
// SetResult.Value is what the session installed, not a sentinel for "nothing
// confirmed", so an agent that answers with an empty value is showing an
// empty value, and the rows and the note must not claim the request instead
// (plan 025 X13, astra r4 item 4) — no later delta would correct them.
func (st applyStep) landed(res engine.SetResult) applyStep {
	st.value = res.Value
	st.rev = res.Rev
	st.note = st.landedNote()
	return st
}

// notApplied is the step as a note instead of a change.
func (st applyStep) notApplied(why notAppliedReason, model string) applyStep {
	st.skipped = true
	st.note = optionNotAppliedNote(st.label, model, st.shownValue(), why)
	return st
}

// notAppliedReason is why an option change was not applied (plan 025 X4).
type notAppliedReason int

const (
	// notAppliedStale: the session is no longer on the model the change was
	// chosen for — engine.ErrStaleModel, or a chain that found the model
	// moved before it could send the change at all.
	notAppliedStale notAppliedReason = iota
	// notAppliedMissing: the model has no such option — its catalog does not
	// advertise it, or the agent's answer no longer lists it
	// (agent.ErrOptionGone).
	notAppliedMissing
	// notAppliedUnoffered: the model has the option but not that value.
	notAppliedUnoffered
)

// optionNotAppliedNote is the note an option change that was not applied
// writes (plan 025 X4). It is a note and never an error row: nothing failed —
// the model the user chose does not take that setting, or is no longer the
// session's — and the steps around it still ran. label is the tab's label
// (effort, fast, context, …), model the id of the model the change was for,
// and value the value as the user saw it. The dialog writes these, and
// `/model <id> <effort>` writes the same three.
func optionNotAppliedNote(label, model, value string, why notAppliedReason) string {
	if model == "" {
		model = "the model"
	}
	head := label + " not applied: "
	switch why {
	case notAppliedStale:
		return head + "the model changed"
	case notAppliedUnoffered:
		return head + model + " does not offer " + value
	}
	return head + model + " has no " + label
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
	// Every tab starts on its option's current value: repaired seeds a tab
	// that has none, and at open that is all of them.
	m.mdlg = modelDialog{filter: ti}.repaired(m.modelDialogTabs())
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
	case dialogResume:
		// Nor is this one, and it has no default to fall back on: Esc quits
		// the program and a click outside is swallowed by handleClick (§3.7),
		// so nothing reaches this before a session exists — a card, which is
		// the one other caller, is a request a live turn makes.
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

// modelDialogFocuses is the Tab order: the list, then every tab the catalog
// advertises, in its order.
func modelDialogFocuses(tabs []modelTab) []dialogFocus {
	out := make([]dialogFocus, 0, 1+len(tabs))
	out = append(out, focusList)
	for _, t := range tabs {
		out = append(out, t.focus())
	}
	return out
}

func (m Model) handleModelDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The keys act on the tabs as they are now, and on a dialog repaired to
	// match them, so no key can land on an option the catalog has dropped.
	tabs := m.modelDialogTabs()
	m.mdlg = m.mdlg.repaired(tabs)
	switch msg.Type {
	case tea.KeyEsc:
		return m.closeDialog(true), nil
	case tea.KeyEnter:
		return m.applyModelDialog()
	case tea.KeyTab:
		m.mdlg.focus = cycleFocus(modelDialogFocuses(tabs), m.mdlg.focus, 1)
		return m, nil
	case tea.KeyShiftTab:
		m.mdlg.focus = cycleFocus(modelDialogFocuses(tabs), m.mdlg.focus, -1)
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
		return m.moveDialogValue(tabs, delta), nil
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

// moveDialogValue steps the focused tab along its advertised values.
func (m Model) moveDialogValue(tabs []modelTab, delta int) Model {
	i := tabIndex(tabs, m.mdlg.focus)
	if i < 0 {
		return m
	}
	opt := &tabs[i].opt
	idx := 0
	for j, v := range opt.SelectValues {
		if v.Value == m.mdlg.chosen[opt.ID] {
			idx = j
			break
		}
	}
	n := len(opt.SelectValues)
	m.mdlg = m.mdlg.choose(opt.ID, opt.SelectValues[(idx+delta+n)%n].Value)
	return m
}

// applyModelDialog is Enter: the box closes optimistically, and one chained
// command applies model → each tab in catalog order, each step only if it
// changed — and a tab only if the user moved it (modelDialog.touched). Notes
// are written for the steps that landed, and for the ones the chain did not
// apply (optionNotAppliedNote), and an error names the one that failed, so a
// half-applied change is visible rather than guessed at.
//
// Every option step is bound to the model the chain ends on: the one picked
// when the chain switches models, the current one when it does not (plan 025
// X3). Chosen against one model's catalog, a value is nobody's choice for
// another model's (design 3), so the engine refuses a step whose model has
// moved by the time it runs, and every step is first re-resolved against the
// latest catalog of the model it is bound to (runModelApply).
func (m Model) applyModelDialog() (tea.Model, tea.Cmd) {
	list := m.dialogModelList()
	tabs := m.modelDialogTabs()
	d := m.mdlg.repaired(tabs)
	m = m.closeDialog(false)

	var steps []applyStep
	forModel := m.snap.CurrentModel
	switching := false
	if d.sel >= 0 && d.sel < len(list) && list[d.sel].ID != m.snap.CurrentModel {
		id := list[d.sel].ID
		steps = append(steps, applyStep{value: id, note: "model → " + id, label: "model", at: m.modelRev})
		m.snap.CurrentModel = id
		m.model = id
		forModel = id
		switching = true
	}
	for _, t := range tabs {
		// Only a tab the user moved is a change to make: an untouched one is
		// on its option's live value, and sending it would at best repeat that
		// value and at worst send back one a delta has since replaced.
		v := d.chosen[t.opt.ID]
		if !d.touched[t.opt.ID] || v == "" {
			continue
		}
		// A value the option already holds is no change — on the model the
		// chain stays on. On one it switches to, the value the source holds
		// says nothing about the destination's, which only the chain can read
		// once it has switched: there the choice is a step, and runModelApply
		// drops it only if the destination already holds it. Judged on the
		// source, a delta that moved the source onto the value chosen for the
		// destination would silently lose the choice (plan 025 X13, astra r4
		// item 3).
		if !switching && v == t.opt.Current {
			continue
		}
		st := applyStep{cfgID: t.opt.ID, value: v, label: t.label, role: t.role, opt: t.opt, at: m.configRev}
		st.note = st.landedNote()
		steps = append(steps, st)
		m = m.setConfigCurrent(t.opt.ID, v)
	}
	if len(steps) == 0 {
		return m, nil
	}
	if m.eng == nil {
		return m, nil
	}

	modelCfgID := ""
	if opt := agent.ModelConfigOption(m.snap); opt != nil {
		modelCfgID = opt.ID
	}
	m.applyGen++
	gen := m.applyGen
	// One command per step, plus one for the model step's fallback: the closure
	// runs off this Update and may not mint any of its own.
	eng, cmds := m.eng, m.nextCmds(len(steps)+1)
	return m, func() tea.Msg {
		return runModelApply(eng, cmds, steps, forModel, modelCfgID, gen)
	}
}

// runModelApply is the chain itself, on the command's goroutine: it reads the
// engine and never the Model, which belongs to Update.
//
// forModel is the model every option step is bound to: the one the user
// picked when the chain switches models, the current one when it does not.
// It is the id the model step sends and never the model the session reports
// back. The dialog sends a canonical id off the model list, so there is no
// alias for the session to resolve, and a reported model that is not the one
// sent is a move — the agent's, or another client's — installed before the
// setter read its outcome, which the value alone cannot tell apart from an
// alias resolved. Adopted as the destination, it would send the user's
// choices to a model they did not pick (plan 025 X13, superseding X10 (a);
// astra r4 item 1). So a model step that confirms another model ends the
// chain there: every option step is the stale note.
//
// Each option step is judged immediately before it is sent, on the snapshot
// read then (resolveOn): not the tabs it was chosen on, which were the
// screen's, and not a catalog read once after the model step, because every
// step's own answer installs a catalog, and an earlier step's can drop an
// option a later step sets (astra r4 item 2). A session no longer on forModel
// makes that step and every one after it stale notes; an option the model
// does not advertise, or a value it does not offer, is its note, and the chain
// goes on; a value the model already holds is no change, and nothing is sent
// or written for it. Every step is sent bound to the model
// (Setting.ForModel), which is what refuses a model change landing between
// that read and the write: the worker answers engine.ErrStaleModel, the step
// and every step after it become notes, and nothing more is sent, because the
// model is not the one they were chosen for. A step the agent took but whose
// answer no longer lists the option (agent.ErrOptionGone) is a note too, and
// the chain goes on. Any other error names its step and ends the chain, as it
// always has — the model step's answer that could not be read
// (agent.ErrBadCatalog) included, which ends it too, since no option can be
// judged against a catalog nobody could read, but whose row says the outcome
// is unknown (modelApplyMsg.unread).
func runModelApply(eng *engine.Engine, cmds []engine.Command, steps []applyStep, forModel, modelCfgID string, gen int) modelApplyMsg {
	ctx := context.Background()
	out := modelApplyMsg{gen: gen}
	for i, st := range steps {
		if st.cfgID == "" {
			res, err := applyModelStep(ctx, eng, cmds[i], cmds[len(steps)], forModel, modelCfgID)
			if err != nil {
				out.step, out.err = st.label, err
				out.unread = errors.Is(err, agent.ErrBadCatalog)
				return out
			}
			st = st.landed(res)
			if st.value != forModel {
				// The agent took the switch and the session is already on
				// another model. The note names the switch as it was taken,
				// and the stale notes after it say the model then changed —
				// the transcript a move just after the setter's answer writes,
				// which this schedule cannot be told apart from. The value it
				// settles is the session's own word, which its delta carries.
				st.note = "model → " + forModel
				out.done = append(out.done, st)
				return notAppliedFrom(out, steps[i+1:], notAppliedStale, forModel)
			}
			out.done = append(out.done, st)
			continue
		}
		snap := eng.State().Snapshot
		if snap.CurrentModel != forModel {
			// Moved before this step could be sent: the catalog read here is
			// some other model's, and judging the step against it would say
			// the wrong thing about the model the user picked.
			return notAppliedFrom(out, steps[i:], notAppliedStale, forModel)
		}
		var ok bool
		if st, ok = st.resolveOn(catalogTabs(snap), forModel); !ok {
			out.done = append(out.done, st)
			continue
		}
		if st.value == st.opt.Current {
			// Already the model's value: on a chain that switched, the
			// destination's own (applyModelDialog leaves this judgement to
			// the chain), and on one that did not, a change somebody else made
			// since Enter. Nothing to send, and nothing to say.
			continue
		}
		res, err := eng.Set(ctx, cmds[i], engine.Setting{
			Kind: engine.SettingConfig, ID: st.cfgID, Value: st.value, ForModel: forModel,
		})
		switch {
		case errors.Is(err, engine.ErrStaleModel):
			return notAppliedFrom(out, steps[i:], notAppliedStale, forModel)
		case errors.Is(err, agent.ErrOptionGone):
			out.done = append(out.done, st.notApplied(notAppliedMissing, forModel))
			continue
		case err != nil:
			out.step, out.err = st.label, err
			return out
		}
		out.done = append(out.done, st.landed(res))
	}
	return out
}

// notAppliedFrom ends a chain with every remaining step noted for the same
// reason and none of them sent.
func notAppliedFrom(out modelApplyMsg, rest []applyStep, why notAppliedReason, model string) modelApplyMsg {
	for _, st := range rest {
		out.done = append(out.done, st.notApplied(why, model))
	}
	return out
}

// resolveOn is an option step re-resolved against the tabs of the model it is
// bound to, as the session holds them when the step is about to be sent
// (plan 025 X3, X13): effort and fast by role, whatever that model calls them,
// and every other tab by id — design 5's rule for `/model <id> <effort>`,
// applied here so the two paths agree. It is sent only if that model
// advertises the option and offers the chosen value; otherwise the step comes
// back as its note, with ok false.
func (st applyStep) resolveOn(dest []modelTab, model string) (applyStep, bool) {
	var to *agent.ConfigOption
	for i := range dest {
		t := &dest[i]
		if (st.role != roleOther && t.role == st.role) || (st.role == roleOther && t.opt.ID == st.cfgID) {
			to = &t.opt
			break
		}
	}
	if to == nil {
		return st.notApplied(notAppliedMissing, model), false
	}
	v, ok := st.valueOn(to)
	if !ok {
		return st.notApplied(notAppliedUnoffered, model), false
	}
	st.cfgID, st.value, st.opt = to.ID, v, *to
	return st, true
}

// valueOn is the step's value as the option to spells it, if it offers one.
// Effort is matched without regard to case, as `/model`'s effort word is
// (agent.MatchEffortValue); fast by what the value means, since what the user
// picked is on or off and each model spells its own two values; everything
// else exactly as the agent advertised it.
func (st applyStep) valueOn(to *agent.ConfigOption) (string, bool) {
	switch st.role {
	case roleEffort:
		for _, v := range to.SelectValues {
			if strings.EqualFold(v.Value, st.value) {
				return v.Value, true
			}
		}
		return "", false
	case roleFast:
		srcOff, srcOn, srcOK := agent.FastOnOff(&st.opt)
		dstOff, dstOn, dstOK := agent.FastOnOff(to)
		switch {
		case srcOK && dstOK && st.value == srcOn:
			return dstOn, true
		case srcOK && dstOK && st.value == srcOff && dstOff != "":
			return dstOff, true
		}
	}
	return st.value, offers(to, st.value)
}

// settleStep writes a step the agent accepted into the snapshot. It is the
// optimistic write made good: the session's own snapshot does not always carry
// what landed — the model step's fallback writes a config option and leaves
// CurrentModel alone — so the steps that succeeded have the last word over the
// refresh. What it writes is what the session installed (applyStep.landed),
// never the request: an agent whose answer holds another value than the one
// asked for is showing that value, and so must the rows.
//
// Unless something newer has been applied since. A step is written only if no
// delta for its section with a higher revision has reached this model since the
// chain was issued: another client changing the model while this chain ran, or
// this step's own confirmation coming back older than a change already applied,
// would otherwise be overwritten by an answer about a value that is no longer
// current (plan 021 §3.8, panel astra 15).
func (m Model) settleStep(st applyStep) Model {
	if st.skipped {
		return m
	}
	applied := m.modelRev
	if st.cfgID != "" {
		applied = m.configRev
	}
	if !mayApply(applied, st.at, st.rev) {
		return m
	}
	if st.cfgID != "" {
		return m.setConfigCurrent(st.cfgID, st.value)
	}
	m.snap.CurrentModel = st.value
	// m.model keeps the last model anyone named, as refreshSnap keeps it:
	// the value is the session's word as it is (landed), and a word that
	// names no model leaves the status row nothing to draw from.
	if st.value != "" {
		m.model = st.value
	}
	return m
}

// applyModelStep runs the model step and answers with what the engine
// confirmed. It keeps the fallback /model has today: an agent without
// session/set_model may still take the model as a config option, and fb is
// the command id that fallback spends. The live session takes the config path
// itself now when the catalog has a model option (plan 025 design 2), so the
// fallback is rarely reached; it costs nothing where it is not.
//
// An answer the session could not read (agent.ErrBadCatalog) takes no
// fallback. It is not the model refused: the agent answered, and may have
// switched, so setting the model again as a config option would be a second
// write craze did not mean — the session's own changeModel does not fall back
// on it either. Nothing of that answer was installed, so both callers show
// what the session's snapshot says, and say the outcome is unknown
// (unreadModelText).
//
// The same holds for the fallback's own answer. When the first call was
// refused and the fallback's answer could not be read, the fallback's error is
// the step's answer, not the first refusal: the fallback was sent and
// answered, and may have switched the model, so "refused" is no longer true of
// where the model is — returned as the refusal, `/model` would put its prev
// back without reading the session, and the dialog would name a refusal
// instead of saying the outcome is unknown (astra r5 item 2). Any other
// fallback error leaves the first refusal as the answer, as it always has.
func applyModelStep(ctx context.Context, eng *engine.Engine, cmd, fb engine.Command, id, modelCfgID string) (engine.SetResult, error) {
	res, err := eng.Set(ctx, cmd, engine.Setting{Kind: engine.SettingModel, Value: id})
	if err != nil && modelCfgID != "" && !errors.Is(err, agent.ErrBadCatalog) {
		res2, err2 := eng.Set(ctx, fb, engine.Setting{
			Kind: engine.SettingConfig, ID: modelCfgID, Value: id,
		})
		if err2 == nil || errors.Is(err2, agent.ErrBadCatalog) {
			return res2, err2
		}
	}
	return res, err
}

// unreadModelText is the error row for a model change whose answer could not
// be read (agent.ErrBadCatalog; applyModelStep): the outcome is unknown, and
// the row says so and names the model craze shows once the screen has read the
// session back — the one it was on before the call, unless somebody has moved
// it since, because nothing of that answer was installed.
func (m Model) unreadModelText() string {
	shown := m.snap.CurrentModel
	if shown == "" {
		shown = m.model
	}
	return "model: the agent's answer could not be read — it may have switched; craze still shows " + sanitizeLine(shown)
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

// modelDialogRows is what fits: the list length, whether the footer and the
// filter are still drawn, and how many tab rows are — always the first ones,
// in catalog order.
type modelDialogRows struct {
	list           int
	footer, filter bool
	tabs           int
}

func (r modelDialogRows) total() int {
	n := 1 + r.list + r.tabs // the title row is never dropped
	for _, on := range []bool{r.footer, r.filter} {
		if on {
			n++
		}
	}
	return n
}

// fitModelDialog drops rows in the pinned order until the box fits its budget:
// the list shrinks first, then the footer hint, then the filter, and the tab
// rows last, from the last tab to the first, because nothing else in craze can
// set them. Over effort and fast that is the order the two fixed rows always
// dropped in: fast, then effort.
func fitModelDialog(budget, list, tabs int) modelDialogRows {
	r := modelDialogRows{list: list, footer: true, filter: true, tabs: tabs}
	for r.total() > budget {
		switch {
		case r.list > 0:
			r.list--
		case r.footer:
			r.footer = false
		case r.filter:
			r.filter = false
		case r.tabs > 0:
			r.tabs--
		default:
			return r
		}
	}
	return r
}

// modelDialogPlan is what the box draws at one inner height: which rows
// survived the budget, the window onto the filtered list, and the tabs — every
// one the catalog advertises, of which rows.tabs are drawn. The renderer and
// the hit-tester both take it, so a click can never land on a row that was not
// drawn.
type modelDialogPlan struct {
	rows       modelDialogRows
	list       []agent.ModelInfo
	top, shown int
	tabs       []modelTab
}

func (m Model) modelDialogPlan(budget int) modelDialogPlan {
	list := m.dialogModelList()
	tabs := m.modelDialogTabs()
	rows := fitModelDialog(budget, min(len(list), dialogListMax), len(tabs))
	top, shown := dialogListWindow(len(list), m.mdlg.sel, rows.list)
	return modelDialogPlan{rows: rows, list: list, top: top, shown: shown, tabs: tabs}
}

// modelDialogHintText is the list's footer: what Tab reaches, named by the
// tabs' labels in their order — "tab effort/fast", "tab effort", "tab fast"
// over the catalogs craze has always drawn — or no Tab at all when the model
// advertises nothing.
func modelDialogHintText(tabs []modelTab) string {
	if len(tabs) == 0 {
		return modelDialogHintPlain
	}
	labels := make([]string, len(tabs))
	for i, t := range tabs {
		labels[i] = t.label
	}
	return modelHintHead + "tab " + strings.Join(labels, "/") + " · " + modelHintTail
}

// modelDialogHintAt is the footer the box draws at an inner width: the tabs
// named (modelDialogHintText) when that fits, and "tab options" in their place
// when it does not.
//
// Except over effort and fast alone, whose footers — "tab effort/fast", "tab
// effort", "tab fast" — are drawn at every width exactly as craze has always
// drawn them (plan 025 design 4 pins their presentation byte for byte): where
// the box is narrower than one of them, it is clamped as it always was, as the
// 40x12 frame shows. A catalog with any other tab has no footer of old to keep.
func modelDialogHintAt(tabs []modelTab, inner int) string {
	hint := modelDialogHintText(tabs)
	if lipgloss.Width(hint) <= inner || onlyEffortAndFast(tabs) {
		return hint
	}
	return modelDialogHintOptions
}

// onlyEffortAndFast reports whether every tab is effort or fast: the catalogs
// the dialog drew before its tabs came from the catalog — no tab at all among
// them, whose footer names none and so has nothing to stand in for.
func onlyEffortAndFast(tabs []modelTab) bool {
	for _, t := range tabs {
		if t.role == roleOther {
			return false
		}
	}
	return true
}

func (m Model) modelDialogBody(inner, budget int) []string {
	p := m.modelDialogPlan(budget)
	// Drawn from the dialog as the tabs now are, which a key or a refresh
	// would have repaired it to anyway: a focus mark on a tab that is not
	// drawn, or a value its option does not offer, would be a row nobody can
	// act on.
	d := m.mdlg.repaired(p.tabs)
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
		rows = append(rows, m.dialogRow(modelRowText(md), tag, p.top+i == d.sel, d.focus == focusList, inner))
	}
	for _, t := range p.tabs[:p.rows.tabs] {
		rows = append(rows, m.dialogValueRow(t.label, &t.opt, d.chosen[t.opt.ID], d.focus == t.focus(), inner, t.valueName()))
	}
	if p.rows.footer {
		hint := modelDialogHintAt(p.tabs, inner)
		if d.focus != focusList {
			hint = modelValueHint
		}
		rows = append(rows, m.dialogFooter(hint, inner))
	}
	return rows
}

// modelDialogClick maps a body row onto what it draws: a list row picks that
// model and applies, a tab row takes the focus so the arrows reach it. The
// rows are counted off the same plan the renderer drew, so the rows the budget
// dropped are rows no click can reach.
func (m Model) modelDialogClick(i int) (tea.Model, tea.Cmd) {
	p := m.modelDialogPlan(m.lay.Dialog.H - dialogBorder)
	m.mdlg = m.mdlg.repaired(p.tabs)
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
	if row -= p.shown; row < p.rows.tabs {
		m.mdlg.focus = p.tabs[row].focus()
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
