package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

// slashMaxRows is the menu's natural height — grok-build's window, and the
// most rows the band ever asks the layout for. The list itself is uncapped:
// the band windows onto it, so the whole catalog stays reachable.
const slashMaxRows = 8

// slashNameCap is the widest the name column ever grows, in cells. Names this
// long are pathological — cursor's and grok's run to about twenty — and without
// a ceiling one of them would push every description off the row it shares the
// band with. Past the cap the name is clamped with an ellipsis, so the cost
// stays on the row that earned it.
const slashNameCap = 28

type slashItem struct {
	Name, Desc string
	Builtin    bool
	Skill      bool
	// Plugin is the plugin id a row craze owns came from, and Qualified its
	// plugin:name spelling. Plugin != "" is the whole marker: only those rows
	// answer to a second spelling in the filter and the accept, so an agent
	// command whose advertised name happens to hold a colon — grok's
	// codex:review — stays the single name it has always been.
	Plugin, Qualified string
}

// labeledDesc is the description as the row draws it, with what the entry is
// named after it. A plugin row says which plugin it came from, because the four
// /rescue commands on a real machine are told apart by nothing else; a disk
// skill keeps the (skill) it has had since 009. An entry that described itself
// with nothing still gets the label, which is more than the bare name says.
func (it slashItem) labeledDesc() string {
	label := ""
	switch {
	case it.Plugin != "":
		label = "(" + it.Plugin + ")"
	case it.Skill:
		label = "(skill)"
	default:
		return it.Desc
	}
	if strings.TrimSpace(it.Desc) == "" {
		return label
	}
	return it.Desc + " " + label
}

// qualifiedAlias is the second spelling a row answers to, or "" when it has
// none. Only a plugin row has one, and only while its displayed name is the
// bare one: a row already showing plugin:name is matched by the name buckets,
// and putting it in the qualified ones too would move it behind every other
// prefix hit. This is the one place the "answers to two names" rule is spelled
// out — the filter, the Enter rule and the accept all ask it here.
func (it slashItem) qualifiedAlias() string {
	if it.Plugin == "" || it.Qualified == "" || strings.EqualFold(it.Qualified, it.Name) {
		return ""
	}
	return it.Qualified
}

func builtinSlash() []slashItem {
	return []slashItem{
		{Name: "help", Desc: "Keybindings and commands", Builtin: true},
		{Name: "model", Desc: "Switch model", Builtin: true},
		{Name: "clear", Desc: "Clear transcript", Builtin: true},
		{Name: "tasks", Desc: "Tasks panel: compact, expanded, hidden", Builtin: true},
		{Name: "theme", Desc: "Theme picker, or /theme <name>", Builtin: true},
		{Name: "rename", Desc: "Rename this session, /rename <title>", Builtin: true},
		{Name: "plan", Desc: "Set plan mode", Builtin: true},
		{Name: "ask", Desc: "Set ask mode", Builtin: true},
		{Name: "agent", Desc: "Set agent mode", Builtin: true},
		{Name: "exit", Desc: "Quit craze", Builtin: true},
	}
}

func builtinNamed(name string) bool {
	want := strings.ToLower(name)
	for _, b := range builtinSlash() {
		if b.Name == want {
			return true
		}
	}
	return false
}

func parseSlashLine(s string) (name, args string, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "/") || strings.ContainsRune(s, '\n') {
		return "", "", false
	}
	rest := strings.TrimSpace(s[1:])
	if rest == "" {
		return "", "", true
	}
	fields := strings.Fields(rest)
	name = strings.ToLower(fields[0])
	args = strings.Join(fields[1:], " ")
	return name, args, true
}

func (m Model) slashCatalog() []slashItem {
	// The two capability reads are hoisted: each one rebuilds the whole
	// Provider to answer a bool, and the loop would ask four times.
	modes, todos := m.showModes(), m.showTodos()
	builtins := builtinSlash()
	// An upper bound on the whole catalog: every loop below can only drop
	// rows. It sizes the row slice and the dedupe map, which holds one key per
	// row that survives and so grows to the same shape.
	size := len(builtins) + len(m.snap.Commands) + len(m.snap.Plugins) + len(m.skills)
	items := make([]slashItem, 0, size)
	for _, it := range builtins {
		switch it.Name {
		case "plan", "ask", "agent":
			if !modes {
				continue
			}
		case "tasks":
			if !todos {
				continue
			}
		}
		items = append(items, it)
	}
	seen := make(map[string]struct{}, size)
	for _, it := range items {
		seen[it.Name] = struct{}{}
	}
	// add is the one gate every non-builtin source passes: an untypable name
	// never reaches the menu, and the first source to claim a name keeps it,
	// case insensitively. Source order is therefore the whole precedence rule.
	add := func(it slashItem) {
		if !slashNameOK(it.Name) {
			return
		}
		n := strings.ToLower(it.Name)
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		items = append(items, it)
	}
	for _, c := range m.snap.Commands {
		add(slashItem{Name: c.Name, Desc: sanitizeLine(c.Description)})
	}
	// Plugin rows sit between what the agent advertised and what the disk
	// scan found. The agent's own names win because craze expands nothing it
	// advertised — the resolver has already qualified anything that would
	// have collided — and a plugin row beats a disk skill of the same name
	// because only the plugin row says which plugin it came from.
	for _, p := range m.snap.Plugins {
		// Both spellings have to be typable, not just the displayed one: the
		// filter and the accept offer the qualified name too.
		if !slashNameOK(p.Qualified) || !slashNameOK(p.Plugin) {
			continue
		}
		add(slashItem{
			Name:      p.Display,
			Desc:      sanitizeLine(p.Description),
			Plugin:    sanitizeLine(p.Plugin),
			Qualified: sanitizeLine(p.Qualified),
		})
	}
	for _, sk := range m.skills {
		add(slashItem{Name: sk.Name, Desc: sanitizeLine(sk.Desc), Skill: true})
	}
	return items
}

// slashNameOK keeps the catalog to names a token could hold. A directory name
// or an advertised command with whitespace in it could never be typed as one
// token, so it would sit in the menu unreachable; a control character would
// break the row it draws on. Rejecting them here means every later stage — the
// filter, the accept, the row — can assume one clean token.
//
// utf8.RuneError goes with them, and for a sharper reason than looks: bubbles'
// rune sanitizer *drops* it (runeutil.go:67), so a name carrying one — or the
// invalid UTF-8 that range yields it for — would come out of SetValue shorter
// than the string acceptSlash measured the new cursor offset against, and the
// cursor would land that many bytes into the rest of the draft.
func slashNameOK(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError {
			return false
		}
	}
	return true
}

// slashToken is the slash token under the cursor: [start,end) are byte offsets
// into value on rune boundaries, name is the token without its "/", and ok
// says the cursor sits inside a token that is one.
//
// The token is the maximal run of non-whitespace runes containing the cursor,
// where containing means start <= cursor <= end. The inclusive end is what
// keeps the menu up with the cursor right after the last rune typed, and the
// space that follows closes it. Because the run is maximal, a token beginning
// with "/" has that "/" at offset 0 or after whitespace by construction, which
// is what keeps `foo/bar` and `https://x` out of the menu without a second
// test. A bare "/" is a token with an empty name and opens the whole catalog —
// browsing is the point.
//
// Whitespace is unicode.IsSpace on both sides, so a newline ends a token (a
// slash on the second line of a draft completes like any other) and a
// non-breaking space before the "/" does not silence the menu. grok-build
// rejects a bare mid-text "/" and tests ASCII whitespace only; both are
// deliberate deviations.
func slashToken(value string, cursor int) (start, end int, name string, ok bool) {
	cursor = min(max(cursor, 0), len(value))
	start = cursor
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(value[:start])
		if unicode.IsSpace(r) {
			break
		}
		start -= size
	}
	end = cursor
	for end < len(value) {
		r, size := utf8.DecodeRuneInString(value[end:])
		if unicode.IsSpace(r) {
			break
		}
		end += size
	}
	if end <= start || value[start] != '/' {
		return 0, 0, "", false
	}
	return start, end, value[start+1 : end], true
}

// slashTokenKey identifies the token being completed: where it starts and what
// has been typed into it. Esc records it and relayout compares it, so the menu
// hides for exactly one token and comes back the moment the token under the
// cursor is another one — typed into, deleted from, or left behind by an arrow
// key. A real token's key is never empty, so an empty slashHideKey hides
// nothing.
func slashTokenKey(start int, name string) string {
	return strconv.Itoa(start) + ":" + name
}

// slashMenuOpen is the band's whole gate: a token under the cursor that Esc
// has not hidden, and nothing covering the composer. A card suspends the menu
// rather than closing it (§3.11) and a dialog now suppresses it too, and both
// fall out of composerCovered — so no flag of its own, and the band and the
// keys cannot disagree about when it is up.
func (m Model) slashMenuOpen() bool {
	if m.composerCovered() {
		return false
	}
	// A shell draft has no slash tokens in it, whatever it looks like: `!ls
	// /usr` is a path the shell will resolve, not a command craze would offer
	// to complete. Suppressing it here rather than in the filter is what keeps
	// the band and the keys agreeing — Tab, Enter and Esc all read slashRows,
	// which reads this (plan 022 §3.6).
	if m.shellMode() {
		return false
	}
	start, _, name, ok := slashToken(m.input.Value(), m.composerCursorOffset())
	return ok && m.slashHideKey != slashTokenKey(start, name)
}

// slashRows is how many rows the menu has this frame: its natural height
// capped by the room the layout leaves the overlay.
//
// It reads OverlayCap rather than the drawn region because a key is handled
// before the frame it belongs to is laid out: the keystroke that opened the
// menu left the previous layout with a zero-row overlay. The cap — what the
// transcript can spare above its minimum — does not move when the menu opens,
// so it already says what the next frame will grant. OverlayCap is already
// floored at zero where it is recorded (layout.go), so nothing re-clamps it.
func (m Model) slashRows() int { return m.slashRowsFor(m.filteredSlash()) }

// slashRowsFor is slashRows for a caller that already holds the matches, so
// one Update does not build the catalog a second time only to count it.
func (m Model) slashRowsFor(items []slashItem) int {
	return min(m.overlayRowsFor(items), m.lay.OverlayCap)
}

// slashActive is the menu the keyboard can reach. Zero matches and a band the
// layout granted zero rows are the same thing to a key: nothing is on screen,
// so Tab, Enter, Esc, PgUp/PgDn and the arrows mean what they mean with no
// menu at all.
func (m Model) slashActive() bool { return m.slashRows() > 0 }

// handleSlashKey is the band's keyboard, called only with the band up and the
// rows it was granted. Tab accepts the highlighted row; the arrows move the
// selection with wrap; PgUp/PgDn page by the rows the user can see, clamped
// rather than wrapped, because a page that jumped to the far end of the
// catalog is not a page. Enter is not here: §3.3 pins its position inside
// handleEnter, between the confirm and the queue edit's save.
func (m Model) handleSlashKey(k tea.KeyType, rows int) Model {
	if k == tea.KeyTab {
		return m.acceptSlash(m.slashSel)
	}
	items := m.filteredSlash()
	switch k {
	case tea.KeyDown:
		m.slashSel = (m.slashSel + 1) % len(items)
	case tea.KeyUp:
		m.slashSel = (m.slashSel - 1 + len(items)) % len(items)
	case tea.KeyPgDown:
		m.slashSel = min(m.slashSel+rows, len(items)-1)
	case tea.KeyPgUp:
		m.slashSel = max(m.slashSel-rows, 0)
	}
	return m
}

// slashBuiltinsEligible is pin 5, defined by what Enter can actually run:
// parseSlashLine trims the draft and refuses a newline, so a builtin is only
// ever dispatched when the token is the draft's first non-whitespace run and
// the draft is one line. Offering /help where Enter could only send the text
// would be a lie, so the builtins are dropped before the filter runs.
func slashBuiltinsEligible(value string, start int) bool {
	return !strings.ContainsRune(value, '\n') && strings.TrimSpace(value[:start]) == ""
}

// filteredSlash is the menu's rows: the eligible catalog in four buckets —
// displayed-name prefix, qualified-only prefix, displayed-name substring,
// qualified-only substring — each in catalog order, each row in its first
// bucket only, case insensitive. A row craze does not own has no second
// spelling, so it can only land in buckets 1 and 3, which is 009's pin 3
// unchanged. The qualified buckets are what make /git-commands: a query at
// all: cursor's own name class has no colon, so the spelling exists only
// because craze resolves it. No fuzzy matcher and no cap — the band windows
// onto this list, so the whole catalog stays reachable by scrolling.
func (m Model) filteredSlash() []slashItem {
	value := m.input.Value()
	start, _, name, ok := slashToken(value, m.composerCursorOffset())
	if !ok {
		return nil
	}
	builtins := slashBuiltinsEligible(value, start)
	q := strings.ToLower(name)
	var prefix, qualPrefix, sub, qualSub []slashItem
	for _, it := range m.slashCatalog() {
		if it.Builtin && !builtins {
			continue
		}
		n := strings.ToLower(it.Name)
		qual := strings.ToLower(it.qualifiedAlias())
		switch {
		case strings.HasPrefix(n, q):
			prefix = append(prefix, it)
		case qual != "" && strings.HasPrefix(qual, q):
			qualPrefix = append(qualPrefix, it)
		case q != "" && strings.Contains(n, q):
			sub = append(sub, it)
		case q != "" && qual != "" && strings.Contains(qual, q):
			qualSub = append(qualSub, it)
		}
	}
	// Concat of four empty buckets is nil, which is the "no matches" the
	// callers test with len().
	return slices.Concat(prefix, qualPrefix, sub, qualSub)
}

// slashExactlyTyped is grok-build's Enter rule: a token that already spells the
// highlighted row is not completed again, it is run or sent. A bare "/" spells
// nothing, so Enter there accepts the first row instead of sending "/".
func (m Model) slashExactlyTyped() bool {
	_, _, name, ok := slashToken(m.input.Value(), m.composerCursorOffset())
	if !ok || name == "" {
		return false
	}
	items := m.filteredSlash()
	if m.slashSel < 0 || m.slashSel >= len(items) {
		return false
	}
	it := items[m.slashSel]
	// Either spelling of a plugin row is the row fully typed: both resolve to
	// the same entry on the wire, so completing one into the other would be
	// the menu rewriting a name that already works. name is non-empty here, so
	// a row with no alias cannot match the "" qualifiedAlias returns.
	return strings.EqualFold(name, it.Name) || strings.EqualFold(name, it.qualifiedAlias())
}

func (m Model) runBuiltin(name, args string) (tea.Model, tea.Cmd) {
	// Every branch consumes the draft — clearing it, replacing it, or sending
	// it — so the menu that draft opened goes with it.
	m.resetSlash()
	switch name {
	case "help":
		m.input.SetValue("")
		return m.openHelp(), nil
	case "exit":
		m.input.SetValue("")
		return m.requestQuit()
	case "clear":
		// Nothing pending survives a clear: a queued message sent minutes
		// later, into a transcript that no longer shows why it was queued, is
		// worse than losing it. The pending state goes first, because ending
		// an edit puts the displaced draft back into the composer.
		m.clearPending()
		m.input.SetValue("")
		m.clearTranscript()
		return m, nil
	case "tasks":
		m.input.SetValue("")
		return m.cycleTasks()
	case "theme":
		m.input.SetValue("")
		if args == "" {
			return m.openThemePicker(), nil
		}
		return m.setThemeNamed(args), nil
	case "rename":
		// The draft is consumed whichever way this goes: the command has been
		// read, and leaving it in the composer would invite a second run of it.
		m.input.SetValue("")
		title := capRunes(sanitizeLine(strings.TrimSpace(args)), titleRuneCap)
		if title == "" {
			m.addError("usage: /rename <title>")
			return m, nil
		}
		if !m.sessionReady() {
			// There is no snapshot to rename yet, and no session id to write
			// the index row against.
			m.addError("session is still starting")
			return m, nil
		}
		if m.eng != nil {
			// SetTitle renames, pins and records the row (plan 021 §3.8). Two
			// different failures come back from it:
			//
			//   - the rename itself was refused — the log's outbox is backed up
			//     — and nothing was renamed or pinned, so the note and the row
			//     would both be saying something untrue: the error alone;
			//   - the rename happened and only the index write failed
			//     (ErrIndexWrite), which is the error row writeIndex used to
			//     draw, followed by the note, in that order, because the
			//     session really is renamed.
			if err := m.eng.SetTitle(m.nextCmd(), title); err != nil {
				if !errors.Is(err, engine.ErrIndexWrite) {
					m.addError(err.Error())
					return m, nil
				}
				m.addError(indexWriteText(err))
			}
		}
		m.refreshSnap()
		m.addNote("renamed to " + title)
		return m, nil
	case "model":
		if args == "" {
			m.input.SetValue("")
			return m.openModelDialog(), nil
		}
		id, effortArg, err := resolveModelArgs(m.snap, args, m.effortShorthand())
		if err != nil {
			m.input.SetValue("")
			m.addError(err.Error())
			return m, nil
		}
		return m.applyModelEffort(id, effortArg)
	case "plan", "ask", "agent":
		m.input.SetValue("")
		if !m.showModes() {
			m.addError("mode " + name + " is not advertised")
			return m, nil
		}
		id, ok := agent.ResolveMode(name, modeIDs(m.snap.Modes))
		if !ok {
			m.addError("mode " + name + " is not advertised")
			return m, nil
		}
		return m.applyMode(id)
	}
	return m.send()
}

func modeIDs(modes []agent.ModeInfo) []string {
	ids := make([]string, 0, len(modes))
	for _, m := range modes {
		ids = append(ids, m.ID)
	}
	return ids
}

func (m Model) applyMode(id string) (tea.Model, tea.Cmd) {
	if id == "" || id == m.snap.CurrentMode || m.eng == nil {
		return m, nil
	}
	prev := m.snap.CurrentMode
	m.snap.CurrentMode = id
	// The generation is what the answer is matched on, not the mode id: see
	// Model.modeGen. It is captured for the closure here, because m.modeGen
	// belongs to a copy the next change is free to bump.
	m.modeGen++
	m.modeInFlight = id
	gen := m.modeGen
	// Leaving the mode the plan was made in retires the offer with it, and the
	// kill is recorded against the turn so a late ending cannot bring it back.
	m.retirePlanOffer()
	m.addNote(modeNote(m.snap.Modes, id))
	// The revision the mode section stood at when this asked, so a refusal that
	// comes back after somebody else's change cannot roll that change back
	// (revertModeMsg).
	eng, cmd, at := m.eng, m.nextCmd(), m.modeRev
	return m, func() tea.Msg {
		// Bounded, so an agent that never answers produces a revert instead of
		// pinning the chip for ever. See modeCallTimeout.
		ctx, cancel := context.WithTimeout(context.Background(), modeCallTimeout)
		defer cancel()
		if _, err := eng.Set(ctx, cmd, engine.Setting{Kind: engine.SettingMode, Value: id}); err != nil {
			return revertModeMsg{gen: gen, prev: prev, err: err, at: at}
		}
		return modeAppliedMsg{gen: gen, id: id}
	}
}

// modeNote is what a mode change writes to the transcript: the id, and the
// agent's own description of the mode when it advertised one. Notes render
// dim as a whole, so the description needs no style of its own.
func modeNote(modes []agent.ModeInfo, id string) string {
	note := "mode → " + id
	for _, md := range modes {
		if md.ID == id && md.Description != "" {
			return note + " · " + sanitizeLine(md.Description)
		}
	}
	return note
}

// resolveModelArgs reads `/model`'s argument into a model id and an effort
// candidate. With the shorthand on (effortShorthand) the model is resolved
// first and the last word may be the candidate (agent.MatchModelEffort); off,
// the whole argument is the model's name and there is never an effort.
func resolveModelArgs(snap agent.Snapshot, args string, shorthand bool) (id, effort string, err error) {
	if !shorthand {
		id, err = agent.MatchModel(snap, strings.TrimSpace(args))
		return id, "", err
	}
	return agent.MatchModelEffort(snap, args)
}

// effortNotAppliedMsg is `/model <id> <effort>` coming back with its model
// step landed and its effort not applied: note is the X4 note that says why
// (optionNotAppliedNote). Nothing failed — the model the user chose does not
// take that effort, or the session is no longer on it — so it is a note and
// never an error row.
type effortNotAppliedMsg struct{ note string }

// applyModelEffort is `/model <id> [effort]`: optimistic, with the same
// SetModel → SetConfig(model_config) fallback the dialog's model step uses
// (applyModelStep). A refused model is revertModelMsg, as it always was. An
// answer the session could not read is not a refusal (agent.ErrBadCatalog):
// it is modelUnreadMsg, with no fallback and no effort sent.
//
// The effort, when there is one, is a candidate until the model step has
// landed, and is then judged against the catalog the session installed for the
// model the command names (plan 025 design 5, runModelEffort) — never the
// catalog the session was on when the command was typed, which on cursor is
// another model's, with its own effort option or none, under its own id and
// with its own values. The model the command names is the destination even
// when it is the one the session is already on, and whatever model the
// session reports back (X13).
func (m Model) applyModelEffort(id, effort string) (tea.Model, tea.Cmd) {
	if m.eng == nil {
		m.input.SetValue("")
		return m, nil
	}
	prev := m.snap.CurrentModel
	m.snap.CurrentModel = id
	m.model = id
	m.input.SetValue("")

	modelCfgID := ""
	if opt := agent.ModelConfigOption(m.snap); opt != nil {
		modelCfgID = opt.ID
	}

	// One command per call the closure can make — the model, the fallback that
	// sets it as a config option, and the effort — minted here because the
	// closure runs off this Update and may not touch the model. An id that goes
	// unused is simply a number nobody spent.
	eng, cmds, at := m.eng, m.nextCmds(3), m.modelRev
	return m, func() tea.Msg {
		ctx := context.Background()
		res, err := applyModelStep(ctx, eng, cmds[0], cmds[1], id, modelCfgID)
		switch {
		case errors.Is(err, agent.ErrBadCatalog):
			// The agent may have switched, so there is no prev to put back and
			// no effort to judge: the catalog it would be judged against is
			// the one nobody could read.
			return modelUnreadMsg{}
		case err != nil:
			return revertModelMsg{prev: prev, err: err, at: at}
		}
		if effort == "" {
			return nil
		}
		// Bound to the model the command names, never the one the session
		// reports back: id is canonical, off the model list (MatchModel), so
		// there is no alias to resolve, and a reported model that is not id is
		// a move installed before the setter read its outcome — adopted, it
		// would send the effort to a model nobody chose (plan 025 X13,
		// superseding X10 (a); astra r4 item 1). So the effort is stale.
		if res.Value != id {
			return effortNotAppliedMsg{note: optionNotAppliedNote("effort", id, effort, notAppliedStale)}
		}
		return runModelEffort(ctx, eng, cmds[2], id, effort)
	}
}

// runModelEffort is `/model`'s effort step, on the command's goroutine once the
// model step has landed on model: it reads the engine and never the Model,
// which belongs to Update.
//
// The candidate is judged against the catalog the session holds now, which the
// model step installed: that model's effort option, whatever its id (effort,
// reasoning_effort, reasoning — agent.EffortOption), and the value spelled as
// it offers it (agent.MatchEffortValue). This is the rule the dialog's chain
// re-resolves its effort step by once it has switched models (resolveOn, plan
// 025 X3), so the two paths agree about what a model takes. The change is sent
// bound to model (Setting.ForModel), so another client's model change landing
// after this read is refused by the engine rather than applied to a model the
// effort was never chosen for.
//
// What is not applied is a note (optionNotAppliedNote, X4): the session had
// moved on before the effort could be sent, or the engine refused it for that
// (engine.ErrStaleModel) — "the model changed"; the model has no effort, or
// the agent's answer no longer lists it (agent.ErrOptionGone) — "has no
// effort"; the model does not offer the value — "does not offer". Any other
// refusal is the error row it always was.
func runModelEffort(ctx context.Context, eng *engine.Engine, cmd engine.Command, model, effort string) tea.Msg {
	note := func(why notAppliedReason) tea.Msg {
		return effortNotAppliedMsg{note: optionNotAppliedNote("effort", model, effort, why)}
	}
	snap := eng.State().Snapshot
	if snap.CurrentModel != model {
		// Moved again before the effort could be sent: the catalog read here is
		// some other model's, and judging the effort against it would say the
		// wrong thing about the model the user picked (X10 (b), X13).
		return note(notAppliedStale)
	}
	opt := agent.EffortOption(snap)
	if opt == nil {
		return note(notAppliedMissing)
	}
	value, ok := agent.MatchEffortValue(opt, effort)
	if !ok {
		return note(notAppliedUnoffered)
	}
	_, err := eng.Set(ctx, cmd, engine.Setting{
		Kind: engine.SettingConfig, ID: opt.ID, Value: value, ForModel: model,
	})
	switch {
	case errors.Is(err, engine.ErrStaleModel):
		return note(notAppliedStale)
	case errors.Is(err, agent.ErrOptionGone):
		return note(notAppliedMissing)
	case err != nil:
		return actionErrMsg{err}
	}
	return refreshSnapMsg{}
}

// acceptSlash puts row i into the draft: only the token under the cursor is
// replaced, with "/name " — and the trailing space is what ends the token, so
// the menu closes by the rule in slashToken rather than by a flag.
//
// A space already following the token is absorbed rather than doubled, so
// accepting inside "/gau  more" yields "/gauntlet  more"; only a space, because
// a tab or the newline that ends a line is structure the draft meant to have.
// An out-of-range i is a no-op: the list can shrink under an open menu between
// the frame that drew it and the key that accepts.
func (m Model) acceptSlash(i int) Model {
	items := m.filteredSlash()
	if i < 0 || i >= len(items) {
		return m
	}
	value := m.input.Value()
	start, end, typed, ok := slashToken(value, m.composerCursorOffset())
	if !ok {
		return m
	}
	if end < len(value) && value[end] == ' ' {
		end++
	}
	ins := "/" + slashAcceptName(items[i], typed) + " "
	next := value[:start] + ins + value[end:]
	m.input.SetValue(next)
	m.setComposerCursor(next, start+len(ins))
	m.slashSel, m.slashTop = 0, 0
	return m
}

// slashAcceptName is the spelling an accept writes: the row's displayed name,
// unless the row is one craze owns and the token already carries a colon. The
// colon is the user saying "I want the plugin path", and it is the only signal
// there is — the row means the same entry either way. A row craze does not own
// always inserts its name, colon or not, so grok's advertised codex:review
// completes from /codex: exactly as it did before plugins existed.
func slashAcceptName(it slashItem, typed string) string {
	if alias := it.qualifiedAlias(); alias != "" && strings.ContainsRune(typed, ':') {
		return alias
	}
	return it.Name
}

// resetSlash forgets the menu's state along with the draft it belonged to: the
// selection, the window, and the token Esc hid. A draft that has been sent,
// queued, interjected, run as a builtin or swapped out for a queued row is
// gone, and nothing about the menu it opened should outlive it — least of all
// a hide that would silence the same token typed into the next draft.
func (m *Model) resetSlash() {
	m.slashSel, m.slashTop = 0, 0
	m.slashKey, m.slashHideKey = "", ""
}

// syncSlash re-finds the menu's selection and window against the rows this
// frame will draw, for the same reason syncQueue and syncAgents do: the row
// count is only settled in relayout, and the catalog can be replaced under an
// open menu by an available_commands_update or a capability change.
//
// The selection moves in the key handler; the window follows here. That is
// what keeps a resize after scrolling, and the keystroke that opened the menu
// on a frame that had granted it nothing, from leaving the selection off
// screen — and a click can only ever land on a row that was drawn.
func (m *Model) syncSlash() {
	key := ""
	if start, _, name, ok := slashToken(m.input.Value(), m.composerCursorOffset()); ok {
		key = slashTokenKey(start, name)
	}
	if key != m.slashKey {
		// A new token is a new list: typing, pasting and cursor motion all
		// land here, so there is one place the selection restarts from.
		m.slashKey = key
		m.slashSel, m.slashTop = 0, 0
	}
	items := m.filteredSlash()
	granted := m.slashRowsFor(items)
	if granted <= 0 {
		// Nothing is drawn, so there is no window to hold. An empty list can
		// only grant zero rows, so this is also the no-matches case, and the
		// selection is settled again the next time the band has rows.
		m.slashTop = 0
		return
	}
	m.slashSel = min(max(m.slashSel, 0), len(items)-1)
	m.slashTop = min(max(m.slashTop, 0), max(0, len(items)-granted))
	if m.slashSel < m.slashTop {
		m.slashTop = m.slashSel
	}
	if m.slashSel >= m.slashTop+granted {
		m.slashTop = m.slashSel - granted + 1
	}
}

// ------------------------------------------------------------------ the band

// overlayView is the slash menu, the one overlay that still draws as a band
// under the transcript; the layout crops it rather than letting it squeeze the
// transcript away. The pickers left it in V3 and help left it here: a dialog is
// a layer over the transcript, not a band under it.
//
// A card outranks it (§3.11): the menu it did not close is suspended — kept in
// state, not drawn — until it has been answered. So does a dialog, which is a
// layer over the transcript and used to leave this band drawn under it. Both
// live in slashMenuOpen, so the band and the keys agree on when it is up.
func (m Model) overlayView(lay frameLayout) string {
	if !m.slashMenuOpen() {
		return ""
	}
	return m.slashMenuView(lay)
}

// overlayRows is the band's natural height, computed rather than rendered:
// the layout asks for it before the view exists, and measuring a string to
// learn a number the list already knows is one more place for the two to
// disagree.
func (m Model) overlayRows() int { return m.overlayRowsFor(m.filteredSlash()) }

// overlayRowsFor is overlayRows for a caller that already holds the matches.
func (m Model) overlayRowsFor(items []slashItem) int {
	if !m.slashMenuOpen() {
		return 0
	}
	return min(slashMaxRows, len(items))
}

// slashMenuView draws the window, not the list: only [slashTop, slashTop+n) of
// the matches, where n is what the layout granted. The selection is not
// clamped here — syncSlash settled it against these same rows in relayout — so
// what is drawn and what a key or a click selects cannot drift apart. The
// window is, because View recomputes lay on a bare resize and that layout is
// not the one syncSlash saw.
//
// The window arithmetic stays here, as it does in queueRowsView and
// agentRowsView: the row is handed the decisions it draws — which item, whether
// it is selected, its scroll mark — and never the window they came from.
func (m Model) slashMenuView(lay frameLayout) string {
	items := m.filteredSlash()
	granted := min(lay.Region(regionOverlay).Height(), len(items))
	if granted <= 0 {
		return ""
	}
	top := min(max(m.slashTop, 0), len(items)-granted)
	// The first row's mark is the widest this frame has: it is the only one
	// carrying the count. The column yields to it rather than the other way
	// round — at the 40-column minimum a 28-cell name would otherwise leave
	// dialogTagSeg no room for the separating space and the mark would vanish
	// silently, which is the one thing on the row the user cannot re-derive.
	markW := lipgloss.Width(slashMark(top, top, granted, len(items), m.slashSel))
	nameW := slashNameWidth(items)
	if markW > 0 {
		nameW = min(nameW, max(1, m.width-len(agentGutterBlank)-markW-1))
	}
	rows := make([]string, 0, granted)
	for i := top; i < top+granted; i++ {
		mark := slashMark(i, top, granted, len(items), m.slashSel)
		rows = append(rows, m.slashRow(items[i], mark, i == m.slashSel, nameW))
	}
	return strings.Join(rows, "\n")
}

// slashNameWidth is the name column, measured over every match rather than the
// eight on screen: a column that re-measured itself per window would shuffle
// the descriptions sideways on each ↓, which is exactly the jitter pin 4 exists
// to prevent. slashNameCap keeps one pathological name from eating the row —
// past it the name is clamped and only that row loses its tail.
func slashNameWidth(items []slashItem) int {
	w := 0
	for _, it := range items {
		w = max(w, lipgloss.Width("/"+it.Name))
	}
	return min(w, slashNameCap)
}

// slashRow is one menu row, built like the queue's and the sub-agent rows it
// sits beside: the house gutter mark naming the selected row, the name in its
// fixed column, two spaces, and the description clamped to whatever is left.
// The scroll mark's cells are reserved before the description is clamped, the
// way queueRow reserves its action strip, so the two can never overlap; the
// mark itself is right-aligned by dialogTagSeg, the same right edge helpRow and
// the dialog lists ride.
//
// The name is clamped and then padded, which is not one step too many:
// clampWidth can land a cell short of the column when a wide rune cannot be
// split beside the ellipsis, and padRow returns an over-wide string unpadded,
// so only the pair guarantees exactly nameW cells — which is the whole point of
// a column measured over every match.
//
// labeledDesc was sanitised at catalog time, so an advertised description with
// a newline in it is already one line by the time it gets here — the row owes
// the band exactly one physical line and nothing downstream re-splits it.
func (m Model) slashRow(it slashItem, mark string, selected bool, nameW int) string {
	gutter := seg{agentGutterBlank, styleFG(m.theme.Dim)}
	fg := m.theme.Dim
	if selected {
		gutter = seg{agentGutterMark, styleFG(m.theme.Accent)}
		fg = m.theme.Accent
	}
	markW := 0
	if mark != "" {
		// One cell of separation, so a count never abuts the description.
		// dialogTagSeg folds that cell into its own padding.
		markW = lipgloss.Width(mark) + 1
	}
	used := len(agentGutterBlank) + nameW
	desc := ""
	if avail := m.width - used - 2 - markW; avail >= 1 {
		desc = clampWidth(it.labeledDesc(), avail)
	}
	segs := []seg{gutter, {padRow(clampWidth("/"+it.Name, nameW), nameW), styleFG(fg)}}
	if desc != "" {
		segs = append(segs, seg{"  " + desc, styleFG(fg)})
		used += 2 + lipgloss.Width(desc)
	}
	return renderSegs(m.width, append(segs, m.dialogTagSeg(used, mark, m.width))...)
}

// slashMark is the row's scroll mark: the position count on the first row, an
// ▲ after it when the window has scrolled off the top, and a ▼ on the last row
// when there is more below. The arrows are dialogScrollTag's — the house rule
// for a windowed list, in window-relative coordinates, which is why i loses its
// top here. A list that fits gets none of it: there is nothing to say, and the
// marks would only cost the descriptions cells.
//
// A band granted a single row is both first and last, and there k/n already
// says whether anything is above it or below it, so it carries the count alone
// (§3.4).
func slashMark(i, top, granted, n, sel int) string {
	if n <= granted {
		return ""
	}
	tag := dialogScrollTag(i-top, top, granted, n)
	if i != top {
		return tag
	}
	count := fmt.Sprintf("%d/%d", sel+1, n)
	if granted > 1 && tag != "" {
		count += " " + tag
	}
	return count
}
