package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/sessions"
)

// The session list's input (plan 030 §3.13's first two bullets, §3.15, owner
// decisions 7–8, R2-11): one line under the rows, always focused, where a new
// session is started — a prompt typed and `enter`, in a directory an `@`
// picks (dispatch.go starts it).
//
// It exists only when Config.Sessions can start sessions (SessionStarter,
// checked as the list opens): without that the list is PR 2's, frame for
// frame.
//
//   - Printable keys type into it; ↑/↓ move the list while no popup is up;
//     `enter` with nothing typed opens the selected row; `esc` clears what is
//     typed, then leaves; `←` leaves only when nothing is typed, and moves
//     the cursor otherwise, as `→` opens the selected row only then.
//   - The rule above it names where a new session would run, live: the
//     directory the leading `@` token was bound to by a pick; else — the
//     token typed rather than picked — the one it names, once the user has
//     finished typing it; else the selected row's workspace (running or
//     saved); else the workspace of the session the list came from. With it
//     the provider and model the session the list came from runs (C16's
//     /provider and /model set them for the list).
//   - Only a leading `@` token — the first thing typed — chooses the
//     directory, and it alone opens the `@` popup (at_dirs.go); any later
//     `@…` is prompt text the agent reads. Text typed before it makes it
//     prompt text too, and the rule falls back to the row, live.
//   - `enter` starts what the input holds (C15, dispatch.go): a prompt in
//     the background — the input says starting… and takes nothing more until
//     that answers — or, on a leading `@dir` alone, an unstarted session in
//     place. `/exit` alone quits craze, every session left running.
//   - Picking a candidate binds the token to that directory: the rule shows
//     it whatever the list does meanwhile. Editing the token, or deleting it,
//     drops the binding for good; `enter` then resolves the token afresh — a
//     name that is exactly one candidate's, or a path that is a directory —
//     and an ambiguous or missing one is an error on the hint line, and
//     nothing starts (sessSubmit).
//   - The token only chooses: the prompt sent is the rest (sessSubmit).
//   - `/` first opens the list's commands in a popup of their own —
//     /provider and /model, which set what new sessions run, and /exit
//     (sessions_models.go, C16).

// The input's words.
const (
	sessInputPrompt      = "❯ "
	sessInputPlaceholder = "type a prompt to start a session · @ picks a directory · / provider and model"
	sessEmptyNoteInput   = "No other sessions. Type a prompt below to start one."
	sessNewRuleLead      = "new session → "
)

// sessInputRows is the footer with an input: the rule naming the target, the
// input, the rule under it and the hint line (the mockup's).
const sessInputRows = 4

// sessBodyMinRows is the fewest rows the list keeps above an open popup: the
// popup gives way first (its shape squeezes its title, then its rows).
const sessBodyMinRows = 2

// sessInput is the list's input: its line, its `@` popup, the leading
// token's binding, what the popup offers besides the rows — the index's
// recent directories — and what the rule last found of a typed path.
type sessInput struct {
	// on says the list has an input: Config.Sessions was a SessionStarter
	// as the list opened (openSessions).
	on bool
	// ti is the line: bubbles' textinput as the editing engine alone — its
	// keys, its cursor — drawn by sessInputRow, which colours the token.
	ti textinput.Model
	// at is the `@` popup (complete.go) over the leading token, and cmd the
	// `/` popup over a line of the list's commands (sessions_models.go):
	// never both up, since the input begins with one or the other.
	at  completePopup
	cmd completePopup
	// bound is the directory a pick bound the leading token to, and boundTok
	// the token as the pick wrote it — `@lumen`, `@"~/my dir"` — which the
	// leading token must still be, character for character, for the binding
	// to hold. Dropped for good the first time it is not.
	bound, boundTok string
	// recents is the index's recent directories (SessionStarter.RecentDirs),
	// read once per opening of the list.
	recents []sessions.RecentDir
	// statText, statDir and statOK are the rule's reading of an unbound
	// leading token that is a path: the token's text, and the directory it
	// names if it is one. Read in the Update as the text changes (a stat per
	// edit), never in View; enter reads the disk afresh (sessSubmit).
	statText string
	statDir  string
	statOK   bool
	// dispatching is the background dispatch the input waits for
	// (sessListState.dispatchSeq; 0 for none): while it runs the input says
	// starting…, keeps what was typed for an outcome that gives it back, and
	// takes no key of its own — a second enter among them.
	dispatching uint64
}

// sessRecentsMsg is the index's recent directories, read for the list's
// opening gen.
type sessRecentsMsg struct {
	gen  uint64
	dirs []sessions.RecentDir
	err  error
}

// newSessInput is an empty, focused input, its popup's loads recorded in
// loads (Model.completeLoads).
func newSessInput(loads *completeLoadSet) sessInput {
	ti := textinput.New()
	ti.Prompt = ""
	// A static cursor starts no blink timer nothing would route back, and
	// keeps a frame deterministic (the model dialog's filter does the same).
	ti.Cursor.SetMode(cursor.CursorStatic)
	// ctrl+v is the list's own paste (the clipboard read every paste in
	// craze goes through); the textinput's would answer a message only it
	// knows, which the list never hands it.
	ti.KeyMap.Paste = key.NewBinding(key.WithDisabled())
	ti.Focus()
	at := newCompletePopup(sessDirSource{}, sessLeadGrammar)
	at.trackIn(loads)
	cmd := newCompletePopup(sessCmdSource{}, sessCmdGrammar)
	cmd.trackIn(loads)
	return sessInput{on: true, ti: ti, at: at, cmd: cmd}
}

// closePopups takes down whichever popup is up, cancelling what it awaits:
// the list closing, a dispatch starting, a switch.
func (in *sessInput) closePopups() {
	in.at.close()
	in.cmd.close()
}

// popup is the popup that is up — the `@` one or the `/` one — if either is.
func (in sessInput) popup() (completePopup, bool) {
	switch {
	case in.at.visible():
		return in.at, true
	case in.cmd.visible():
		return in.cmd, true
	}
	return completePopup{}, false
}

// readRecents reads the index's recent directories off the Update, for the
// list's opening gen.
func readRecents(s SessionStarter, gen uint64) tea.Cmd {
	return func() tea.Msg {
		dirs, err := s.RecentDirs(sessRecentDirs)
		return sessRecentsMsg{gen: gen, dirs: dirs, err: err}
	}
}

// ------------------------------------------------------------ the token

// sessLeadToken is the input's leading `@` token — the one that starts at its
// first non-space rune — if it has one.
func sessLeadToken(value string) (atToken, bool) {
	i := strings.IndexFunc(value, func(r rune) bool { return !unicode.IsSpace(r) })
	if i < 0 {
		return atToken{}, false
	}
	return atTokenAt(value, i)
}

// sessLeadGrammar is the list's `@` grammar: the `@` token's (atGrammar),
// only ever the leading one. A later `@…` is prompt text, and opens nothing.
var sessLeadGrammar = completeGrammar{
	under: func(value string, cursor int) (completeToken, bool) {
		tok, ok := atTokenUnder(value, cursor)
		if !ok {
			return completeToken{}, false
		}
		if lead, ok := sessLeadToken(value); !ok || lead.start != tok.start {
			return completeToken{}, false
		}
		return completeToken{start: tok.start, end: tok.end, text: tok.text}, true
	},
	write: atTokenText,
}

// sessByteCursor is the textinput's cursor, a rune index, as a byte offset
// into value.
func sessByteCursor(value string, pos int) int {
	i := 0
	for n := 0; n < pos && i < len(value); n++ {
		_, size := utf8.DecodeRuneInString(value[i:])
		i += size
	}
	return i
}

// ------------------------------------------------------------ syncing

// syncSessInput follows the input after anything that could change what it
// shows — an edit, a snapshot, the recent directories arriving: the binding
// is dropped if the leading token is no longer the one picked, a typed path
// is read for the rule, and the popup is handed the candidates as they stand
// and synced. The command is a load the popup starts.
func (m *Model) syncSessInput() tea.Cmd {
	in := &m.sessList.in
	if !in.on {
		return nil
	}
	if in.dispatching != 0 {
		// Starting: the input is not edited, and no popup opens over it
		// until the dispatch answers (dispatch.go).
		in.closePopups()
		return nil
	}
	v := in.ti.Value()
	lead, hasLead := sessLeadToken(v)
	if in.boundTok != "" && (!hasLead || v[lead.start:lead.end] != in.boundTok) {
		in.bound, in.boundTok = "", ""
	}
	if hasLead && in.boundTok == "" && sessPathLike(lead.text) {
		if lead.text != in.statText {
			in.statText = lead.text
			in.statDir, in.statOK = sessStatDir(lead.text, m.sessList.home, m.sessHereDir())
		}
	} else {
		in.statText, in.statDir, in.statOK = "", "", false
	}
	env := completeEnv{Workspace: m.sessHereDir(), Shown: m.shownGen}
	cur := sessByteCursor(v, in.ti.Position())
	in.at.setSource(m.sessAtSource())
	at := in.at.sync(v, cur, env)
	in.cmd.setSource(m.sessCmdSourceNow())
	return tea.Batch(at, in.cmd.sync(v, cur, env))
}

// sessStatDir is a path token's directory, if it names one that is there.
func sessStatDir(text, home, here string) (string, bool) {
	dir, ok := sessExpand(text, home, here)
	if !ok {
		return "", false
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", false
	}
	return dir, true
}

// set puts value in the input with the cursor at byte offset cur.
func (in *sessInput) set(value string, cur int) {
	in.ti.SetValue(value)
	in.ti.SetCursor(utf8.RuneCountInString(value[:min(max(cur, 0), len(value))]))
}

// ------------------------------------------------------------ keys

// sessInputKey is the input's share of a key while the list is open: the
// popup's keys while it is up, then the input's own. handled false leaves
// the key to the list (handleSessionsKey), as PR 2 had it: ctrl+d, ctrl+c,
// ctrl+s, ctrl+x, ↑/↓, and esc, ←, → and enter while nothing is typed.
func (m Model) sessInputKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	in := &m.sessList.in
	if in.dispatching != 0 {
		// Starting: the list's own keys are the list's — esc and ← leave it,
		// the dispatch going on without it — and every other key, enter's
		// second press among them, does nothing (§3.13).
		switch msg.Type {
		case tea.KeyCtrlD, tea.KeyCtrlC, tea.KeyCtrlS, tea.KeyCtrlX, tea.KeyUp, tea.KeyDown, tea.KeyEsc, tea.KeyLeft:
			return m, nil, false
		}
		return m, nil, true
	}
	if in.at.visible() {
		choice, handled := in.at.key(msg)
		if handled {
			if choice.verb == 0 {
				return m, nil, true
			}
			in.set(choice.value, choice.cursor)
			if choice.verb == completeAccepted {
				// A pick binds the leading token — the only token the popup
				// opens on — to the directory chosen.
				if lead, ok := sessLeadToken(choice.value); ok {
					in.bound, in.boundTok = choice.item.Value, choice.value[lead.start:lead.end]
				}
			}
			sync := m.syncSessInput()
			return m, sync, true
		}
	}
	if in.cmd.visible() {
		choice, handled := in.cmd.key(msg)
		if handled {
			if choice.verb == 0 {
				return m, nil, true
			}
			// A value is applied, /exit quits (sessCmdChosen); anything
			// else is written, as the `@` popup's choices are.
			if next, cmd, ok := m.sessCmdChosen(msg.Type, choice.item); ok {
				return next, cmd, true
			}
			in.set(choice.value, choice.cursor)
			sync := m.syncSessInput()
			return m, sync, true
		}
	}
	empty := strings.TrimSpace(in.ti.Value()) == ""
	switch msg.Type {
	case tea.KeyCtrlD, tea.KeyCtrlC, tea.KeyCtrlS, tea.KeyCtrlX, tea.KeyUp, tea.KeyDown:
		return m, nil, false
	case tea.KeyEsc:
		if in.ti.Value() == "" {
			return m, nil, false
		}
		in.set("", 0)
		sync := m.syncSessInput()
		return m, sync, true
	case tea.KeyLeft, tea.KeyRight:
		if in.ti.Value() == "" && !msg.Alt {
			return m, nil, false
		}
	case tea.KeyEnter:
		if empty {
			return m, nil, false
		}
		return m.sessInputEnter()
	case tea.KeyTab:
		// Nothing to complete: tab never submits (§3.15).
		return m, nil, true
	case tea.KeyCtrlV:
		return m, pasteFromClipboard(m.shownGen, true), true
	}
	before, pos := in.ti.Value(), in.ti.Position()
	var cmd tea.Cmd
	in.ti, cmd = in.ti.Update(msg)
	if in.ti.Value() == before && in.ti.Position() == pos {
		return m, cmd, true
	}
	sync := m.syncSessInput()
	return m, tea.Batch(cmd, sync), true
}

// sessInputPaste is a paste landing in the list's input: one bracketed paste,
// the way a terminal delivers one — the textinput folds its newlines and tabs
// to spaces.
func (m Model) sessInputPaste(text string) (Model, tea.Cmd) {
	in := &m.sessList.in
	if in.dispatching != 0 {
		// Starting: the input is not edited until the dispatch answers.
		return m, nil
	}
	var cmd tea.Cmd
	in.ti, cmd = in.ti.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text), Paste: true})
	sync := m.syncSessInput()
	return m, tea.Batch(cmd, sync)
}

// sessInputEnter is enter with something typed and no popup taking it
// (§3.13): `/exit` alone quits craze, every session left running (decision
// 11); anything else is resolved (sessSubmit) — an `@` token that names no
// directory, or more than one, and a list with no directory to fall back to,
// say so and nothing starts — and started, with the target, provider, model
// and permission mode captured now (sessNewSpec): a prompt in the background
// (sessDispatch), a leading `@dir` alone as an unstarted session in place
// (openUnstarted).
func (m Model) sessInputEnter() (Model, tea.Cmd, bool) {
	if name, args, ok := parseSlashLine(m.sessList.in.ti.Value()); ok && name == "exit" && args == "" {
		tm, cmd := m.sessQuit()
		return tm.(Model), cmd, true
	}
	// `/provider <id>` and `/model <id>` set what new sessions run (C16).
	if next, cmd, ok := m.sessCmdTyped(m.sessList.in.ti.Value()); ok {
		return next, cmd, true
	}
	sub, err := m.sessSubmit()
	if err == nil && sub.Dir == "" {
		err = errors.New(sessNoTargetNote)
	}
	var spec SpawnSpec
	if err == nil {
		spec, err = m.sessNewSpec(sub.Dir)
	}
	if err != nil {
		m.sessNote(err.Error(), sessNoteErr)
		return m, nil, true
	}
	if sub.Prompt == "" {
		next, cmd := m.openUnstarted(spec)
		return next, cmd, true
	}
	next, cmd := m.sessDispatch(spec, sub.Prompt)
	return next, cmd, true
}

// ------------------------------------------------------------ the target

// sessTargetKind is why a directory is the target.
type sessTargetKind int

const (
	// sessTargetToken: the leading `@` token's, bound by a pick or named by
	// what was typed.
	sessTargetToken sessTargetKind = iota + 1
	// sessTargetRow: the selected row's workspace.
	sessTargetRow
	// sessTargetHere: the workspace of the session the list came from.
	sessTargetHere
)

// sessTarget is where a new session from the list would run: dir and why —
// or, when the leading token names no directory, or more than one, err, the
// words for it.
type sessTarget struct {
	dir  string
	kind sessTargetKind
	err  error
}

// sessTargetNow is the target as the rule draws it (pure: a typed path is the
// Update's last reading of it, syncSessInput). A token still being typed —
// the popup up over it, unbound — names nothing yet: the rule shows what it
// falls back to, as the mockup's does, until the token is finished.
func (m Model) sessTargetNow() sessTarget {
	in := m.sessList.in
	v := in.ti.Value()
	lead, ok := sessLeadToken(v)
	switch {
	case !ok:
	case in.boundTok != "":
		return sessTarget{dir: in.bound, kind: sessTargetToken}
	case in.at.visible():
	case sessPathLike(lead.text):
		if in.statText == lead.text && in.statOK {
			return sessTarget{dir: in.statDir, kind: sessTargetToken}
		}
		return sessTarget{err: sessNoDir(lead.text)}
	default:
		return m.sessResolveName(lead.text)
	}
	return m.sessFallback()
}

// sessFallback is the target no token names (owner decision 8): the selected
// row's workspace — running or saved — else the workspace of the session the
// list came from, when there is one behind the list (sessCameFrom): after
// its connection was lost, or once an unstarted session was discarded, there
// is none (X142), and with no row selected either there is nowhere to start
// — the rule says so, and enter starts nothing.
func (m Model) sessFallback() sessTarget {
	if r, ok := m.sessSelected(); ok && r.workspace != "" {
		return sessTarget{dir: r.workspace, kind: sessTargetRow}
	}
	if dir := m.sessCameFrom(); dir != "" {
		return sessTarget{dir: dir, kind: sessTargetHere}
	}
	return sessTarget{err: errors.New(sessNoTargetNote)}
}

// sessResolveName is a name token's directory: the one candidate whose
// basename it is, exactly. None, or more than one, is an error.
func (m Model) sessResolveName(name string) sessTarget {
	var hits []string
	for _, c := range m.sessDirCands() {
		if filepath.Base(c.dir) == name {
			hits = append(hits, c.dir)
		}
	}
	switch len(hits) {
	case 1:
		return sessTarget{dir: hits[0], kind: sessTargetToken}
	case 0:
		return sessTarget{err: fmt.Errorf("no directory named @%s", sanitizeLine(name))}
	}
	return sessTarget{err: fmt.Errorf("@%s names %d directories; pick one with @", sanitizeLine(name), len(hits))}
}

// sessNoDir is a path token that names no directory.
func sessNoDir(text string) error { return errors.New("no directory " + sanitizeLine(text)) }

// sessSubmission is what enter on the list's input starts (§3.13,
// dispatch.go): the directory it runs in, and the prompt — the input without
// its leading `@` token, which only chose the directory (R2-11). An empty
// prompt is enter on the token alone: the unstarted session there.
type sessSubmission struct {
	Dir    string
	Prompt string
}

// sessSubmit is what the input would start now, the target read afresh: a
// bound token's directory; an unbound one resolved again (§3.15) — a name
// exactly one candidate's, a path to a directory that is there now; else
// the row's, else here. err says why nothing can start: the leading token
// names no directory, or more than one.
func (m Model) sessSubmit() (sessSubmission, error) {
	in := m.sessList.in
	v := in.ti.Value()
	lead, ok := sessLeadToken(v)
	if !ok {
		t := m.sessFallback()
		if t.err != nil {
			return sessSubmission{}, t.err
		}
		return sessSubmission{Dir: t.dir, Prompt: strings.TrimSpace(v)}, nil
	}
	sub := sessSubmission{Prompt: strings.TrimSpace(v[lead.end:])}
	switch {
	case in.boundTok != "":
		sub.Dir = in.bound
	case sessPathLike(lead.text):
		dir, ok := sessStatDir(lead.text, m.sessList.home, m.sessHereDir())
		if !ok {
			return sessSubmission{}, sessNoDir(lead.text)
		}
		sub.Dir = dir
	default:
		t := m.sessResolveName(lead.text)
		if t.err != nil {
			return sessSubmission{}, t.err
		}
		sub.Dir = t.dir
	}
	return sub, nil
}

// sessNewProvider and sessNewModel are what a new session from the list runs,
// as the rule names them: /provider's and /model's choice (sessPick,
// sessions_models.go), else the session the list came from's provider, as
// the status row names it, and its model.
func (m Model) sessNewProvider() string {
	if m.sessPick.provSet {
		return sanitizeLine(m.sessPick.prov.DisplayName())
	}
	if p := sanitizeLine(m.snap.Provider.Label()); p != "" {
		return p
	}
	return sanitizeLine(m.sessProvider)
}

func (m Model) sessNewModel() string {
	if m.sessPick.modelSet {
		return sessModelLabel(m.sessPick)
	}
	id := m.snap.CurrentModel
	if id == "" {
		id = m.model
	}
	for _, md := range m.snap.Models {
		if md.ID == id && md.Name != "" {
			return sanitizeLine(md.Name)
		}
	}
	return sanitizeLine(id)
}

// ------------------------------------------------------------ view

// sessTargetRule is the rule over the input, naming the target: `──── new
// session → ~/projects/lumen · native · glm-5.3 ─`, the directory in the
// accent when a token chose it, or the error in red when the leading token
// names none. Too long for the row, the directory is cut from the left — its
// last elements are what tell it apart — then the whole is clipped.
func (m Model) sessTargetRule(width int) string {
	th := m.theme
	rule := styleFG(th.Rule)
	t := m.sessTargetNow()
	var segs []seg
	if t.err != nil {
		segs = []seg{{t.err.Error(), styleFG(th.Err)}}
	} else {
		dirSt := styleFG(th.Bright)
		if t.kind == sessTargetToken {
			dirSt = styleFG(th.Accent)
		}
		dir := sanitizeLine(sessTilde(m.sessList.home, t.dir))
		tail := []seg{{" · ", styleFG(th.Dim)}, {m.sessNewProvider(), styleFG(th.Provider)}, {" · " + m.sessNewModel(), styleFG(th.Dim)}}
		room := width - 4 - lipgloss.Width(sessNewRuleLead) - segsWidth(tail)
		if lipgloss.Width(dir) > room && room > 1 {
			dir = sessCutLeft(dir, room)
		}
		segs = append([]seg{{sessNewRuleLead, styleFG(th.Dim)}, {dir, dirSt}}, tail...)
	}
	text := segsWidth(segs)
	fill := max(1, width-text-3)
	out := append([]seg{{strings.Repeat("─", fill) + " ", rule}}, segs...)
	out = append(out, seg{" ─", rule})
	return renderSegs(width, out...)
}

// sessCutLeft is s in w cells, its start dropped behind an ellipsis.
func sessCutLeft(s string, w int) string {
	rs := []rune(s)
	for i := range rs {
		if lipgloss.Width(string(rs[i:]))+1 <= w {
			return "…" + string(rs[i:])
		}
	}
	return "…"
}

// sessInputRow is the input's line: the prompt and what is typed — the
// leading `@` token in the accent, or red once finished when it names no
// directory — with the cursor, or the placeholder when nothing is. A line too
// long for the row is shown around the cursor.
func (m Model) sessInputRow(width int) string {
	th := m.theme
	in := m.sessList.in
	prompt := seg{sessInputPrompt, styleFG(th.Accent)}
	cur := lipgloss.NewStyle().Reverse(true)
	v := in.ti.Value()
	if in.dispatching != 0 {
		// What was typed is kept, not shown: the session it starts is.
		return renderSegs(width, prompt, seg{dispatchStartingText, styleFG(th.Dim)})
	}
	if v == "" {
		ph := []rune(sessInputPlaceholder)
		return renderSegs(width, prompt, seg{string(ph[:1]), cur.Foreground(th.Dim)}, seg{string(ph[1:]), styleFG(th.Dim)})
	}
	rs := []rune(v)
	pos := min(in.ti.Position(), len(rs))
	// The token's runes, coloured.
	tokFrom, tokTo := -1, -1
	tokSt := styleFG(th.Accent)
	if lead, ok := sessLeadToken(v); ok {
		tokFrom, tokTo = utf8.RuneCountInString(v[:lead.start]), utf8.RuneCountInString(v[:lead.end])
		if t := m.sessTargetNow(); t.err != nil {
			tokSt = styleFG(th.Err)
		}
	}
	from, to := sessInputWindow(rs, pos, width-lipgloss.Width(sessInputPrompt))
	// Runs of one style: before the token, the token, after it — the cursor's
	// rune a run of its own.
	class := func(i int) int {
		switch {
		case i == pos:
			return 3
		case i >= tokFrom && i < tokTo:
			return 1
		}
		return 2
	}
	style := func(i int) lipgloss.Style {
		st := styleFG(th.FG)
		if i >= tokFrom && i < tokTo {
			st = tokSt
		}
		if i == pos {
			st = st.Reverse(true)
		}
		return st
	}
	segs := []seg{prompt}
	for i := from; i < to; {
		j := i + 1
		for j < to && class(j) == class(i) {
			j++
		}
		segs = append(segs, seg{string(rs[i:j]), style(i)})
		i = j
	}
	if pos >= to {
		segs = append(segs, seg{" ", cur})
	}
	return renderSegs(width, segs...)
}

// sessInputWindow is the runes [from, to) of rs the input shows in avail
// cells with the cursor at pos — a cell of its own past the end: all of them
// when they fit, else a window as far right as the cursor lets it reach (the
// end of a long line is the part being typed).
func sessInputWindow(rs []rune, pos, avail int) (from, to int) {
	cells := func(a, b int) int {
		w := 0
		for _, r := range rs[a:b] {
			w += lipgloss.Width(string(r))
		}
		return w
	}
	end := len(rs)
	past, curW := 0, 1
	if pos >= end {
		past = 1
	} else {
		curW = lipgloss.Width(string(rs[pos]))
	}
	if cells(0, end)+past <= avail {
		return 0, end
	}
	for from < pos && cells(from, pos)+curW > avail {
		from++
	}
	to = min(pos+1, end)
	for to < end && cells(from, to+1) <= avail {
		to++
	}
	return from, to
}

// sessInputHint is the hint line's keys for the input, or nil for the list's
// own: the popup's while it is up — browsing a path, or picking by name — and
// what enter and esc do with what is typed (the mockup's words: enter on the
// token alone opens an empty session there, enter with a prompt starts it in
// the background).
func (m Model) sessInputHint(key, txt func(string) seg) []seg {
	in := m.sessList.in
	if !in.on {
		return nil
	}
	if in.dispatching != 0 {
		return []seg{txt("starting it in the background · "), key("esc"), txt(" back")}
	}
	v := in.ti.Value()
	lead, hasLead := sessLeadToken(v)
	if in.cmd.visible() {
		if p, ok := in.popup(); ok && len(p.ans.Items) == 0 && !p.pending() && !p.ans.NoteErr {
			// Nothing listed to choose — no catalog, nothing matching — so
			// enter takes what is typed.
			return []seg{txt("type the model id · "), key("enter"), txt(" uses it · "), key("esc"), txt(" close")}
		}
		return []seg{key("↑↓"), txt(" choose · type to narrow · "), key("tab/enter"), txt(" use it · "), key("esc"), txt(" close")}
	}
	if in.at.visible() {
		if hasLead && sessPathLike(lead.text) {
			return []seg{key("↑↓"), txt(" choose · "), key("tab"), txt(" open folder · "), key("enter"), txt(" use it · "),
				key("esc"), txt(" close")}
		}
		return []seg{key("↑↓"), txt(" choose · type to narrow · "), key("tab/enter"), txt(" use it · "), key("~/"),
			txt(" browse · "), key("esc"), txt(" close")}
	}
	if strings.TrimSpace(v) == "" {
		return nil
	}
	if h := sessCmdLineHint(v, key, txt); h != nil {
		return h
	}
	if hasLead && strings.TrimSpace(v[lead.end:]) == "" {
		return []seg{txt("type the prompt · "), key("enter"), txt(" opens an empty session there · "), key("esc"), txt(" clear")}
	}
	return []seg{key("enter"), txt(" starts it in the background · "), key("esc"), txt(" clear")}
}
