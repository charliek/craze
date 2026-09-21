package tui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/textdiff"
)

const (
	// proseMaxWidth caps transcript wrapping so wide terminals stay readable.
	proseMaxWidth = 100
	// maxEntries bounds the main transcript; older entries are dropped with a note.
	maxEntries = 5000
	// Sub-agent transcripts are tighter: 1000 entries / 1 MiB raw / 64 KiB
	// per streamed entry (tail kept).
	subMaxEntries = 1000
	subTextBudget = 1 << 20
	entryTextCap  = 64 << 10
	trimmedNote   = "… earlier transcript trimmed"
	// outputPreviewLines is how much of a tool's output an expanded row shows.
	outputPreviewLines = 20
	// editCollapsedLines is how much of the first hunk a collapsed edit shows.
	editCollapsedLines = 6
	tabWidth           = 4
)

type entryKind int

const (
	entryUser entryKind = iota
	entryAssistant
	entryThought
	entryTool
	entryNote
	entryPlan
	entryError
	// entryShell is the composer's `!`: a command craze ran for the user, on
	// their own machine. It is the one kind that never came from an agent and
	// never goes to one (plan 022 §3.6).
	entryShell
)

// renderKey is everything outside an entry that changes how it draws. An entry
// whose key still matches is reused verbatim, so a stream chunk only re-renders
// the entry it landed in.
type renderKey struct {
	width    int
	theme    string
	expanded bool
}

// entry is one transcript item plus its render cache.
type entry struct {
	kind entryKind
	text string
	tool *agent.ToolEvent
	plan *agent.PlanEvent
	// shell is an entryShell's own state; see shellEntry. It is a pointer so
	// the run that finishes minutes later settles the row it opened, whatever
	// the entry slice has done in between.
	shell *shellEntry
	at    time.Time
	// end closes a thought run: the At of the first non-thought event after it.
	end time.Time
	// open marks a thought run that is still streaming.
	open bool
	// interject marks a user entry that came from a grok interjection rather
	// than from a prompt craze sent: it went into a turn already running, so
	// it is drawn with a different mark.
	interject bool

	rendered    []string
	renderedFor renderKey
	// dirty marks content that changed under an unchanged key.
	dirty bool
}

func (m Model) renderKey() renderKey {
	return renderKey{width: m.width, theme: m.theme.Name, expanded: m.expanded}
}

// transcript is one conversation: entries, per-transcript caches, and the
// viewport offset last painted for it. Mutation sets dirty; Model paints m.vp
// only when this transcript is the one on screen (m.cur()).
type transcript struct {
	entries         []entry
	toolLine        map[string]int
	trimmed         bool
	streamOpen      bool
	pathDirs        map[string]map[string]struct{}
	renders         int
	transcriptRows  []string
	transcriptPlain []string
	yOffset         int
	atBottom        bool
	dirty           bool
	// entryCap / textBudget are 0 on main (maxEntries, unlimited text).
	entryCap   int
	textBudget int
}

func (m *Model) cur() *transcript {
	if m.viewing != "" {
		if t := m.subs[m.viewing]; t != nil {
			return t
		}
	}
	return &m.main
}

// appendEntry is the only way an entry reaches the transcript, so it is also
// where an open run ends: a note or a tool row between two chunks means they
// are not one run, and a run that is not the last entry can never be closed.
func (t *transcript) appendEntry(e entry, now time.Time) {
	e.dirty = true
	if e.at.IsZero() {
		e.at = now
	}
	if e.end.IsZero() {
		e.end = e.at
	}
	t.endRun(e.at)
	t.entries = append(t.entries, e)
	t.trimEntries()
	t.dirty = true
}

func (m *Model) appendEntry(e entry) { m.main.appendEntry(e, m.now()) }

// trimEntries enforces the entry cap. Tool rows are addressed by index, so the
// map moves with the slice and rows that fell off are forgotten.
func (t *transcript) trimEntries() {
	maxE := t.entryCap
	if maxE <= 0 {
		maxE = maxEntries
	}
	if len(t.entries) > maxE {
		t.dropFirst(len(t.entries) - maxE)
	}
	if t.textBudget > 0 {
		for t.rawTextLen() > t.textBudget && len(t.entries) > 1 {
			t.dropFirst(1)
		}
	}
}

func (t *transcript) dropFirst(n int) {
	if n <= 0 || n > len(t.entries) {
		return
	}
	t.entries = append(t.entries[:0], t.entries[n:]...)
	t.trimmed = true
	t.dirty = true
	for id, idx := range t.toolLine {
		if idx-n < 0 {
			delete(t.toolLine, id)
			continue
		}
		t.toolLine[id] = idx - n
	}
}

func (t *transcript) rawTextLen() int {
	n := 0
	for i := range t.entries {
		n += len(t.entries[i].text)
	}
	return n
}

func (t *transcript) addUser(text string, now time.Time) {
	t.appendEntry(entry{kind: entryUser, text: text}, now)
}

// addUser writes the user block for text that went to the agent, wherever the
// model learned of it: its own send, a turn the engine started for a drained
// row or another client, or a prompt out of a replayed transcript.
//
// The shell context in front of that text is wire content and never display
// content (plan 022 §3.6): the row shows the message, not the command output
// craze attached to it — which is already on screen, in the `!` row the user
// watched it come out of. Stripping in this one wrapper rather than at each of
// the three callers is what makes the rule hold for every route into a user
// row, including the ones the engine reports rather than this client sending.
func (m *Model) addUser(text string) {
	_, text = agent.SplitShellContext(text)
	m.main.addUser(text, m.now())
}

// addInterjection is the user block for text merged into the running turn.
// It is written from the agent's broadcast, not from the send: the ack only
// says the text was accepted, and grok broadcasts one for an interjection it
// could not merge as well.
func (t *transcript) addInterjection(text string, now time.Time) {
	if text == "" {
		return
	}
	t.appendEntry(entry{kind: entryUser, text: text, interject: true}, now)
}

// addInterjection strips the same block for the same reason addUser does. An
// interjection's echo is the one user row craze draws from what came back
// rather than from what it sent, and on native it is the typed spelling the
// adapter kept beside the steer — which is still the text craze handed to
// Interject, block and all.
func (m *Model) addInterjection(text string) {
	_, text = agent.SplitShellContext(text)
	m.main.addInterjection(text, m.now())
}

func (t *transcript) addNote(text string, now time.Time) {
	if text == "" {
		return
	}
	t.appendEntry(entry{kind: entryNote, text: text}, now)
}

func (m *Model) addNote(text string) { m.main.addNote(text, m.now()) }

// commandLineMark leads the provenance line under a user entry craze expanded
// a plugin command or skill into. It points down and to the right, at the entry
// above it rather than at anything the agent said.
const commandLineMark = "⤷ "

// addCommandLine records that craze expanded something into the prompt above:
// which entry, by the plugin:name spelling that always resolves, and whether it
// was a command or a skill. It is a note, so it draws dim under the user block
// like every other thing craze says about a turn rather than in it.
//
// The block itself is deliberately not shown. It is the plugin's whole body,
// often pages of it, and the transcript is the conversation the user is having;
// a headless caller that wants the text reads the command JSON line.
func (t *transcript) addCommandLine(cmd *agent.ExpandedCommand, now time.Time) {
	if cmd == nil {
		return
	}
	name := sanitizeLine(cmd.Qualified)
	if name == "" {
		return
	}
	line := commandLineMark + name
	if kind := sanitizeLine(cmd.Kind); kind != "" {
		line += " (" + kind + ")"
	}
	t.addNote(line, now)
}

func (m *Model) addCommandLine(cmd *agent.ExpandedCommand) { m.main.addCommandLine(cmd, m.now()) }

// addPlan puts the plan cursor proposed into the transcript as a note block,
// which is why the card itself only has to carry the three answers.
func (t *transcript) addPlan(p *agent.PlanEvent, now time.Time) {
	if p == nil {
		return
	}
	plan := *p
	t.appendEntry(entry{kind: entryPlan, plan: &plan}, now)
}

func (m *Model) addPlan(p *agent.PlanEvent) { m.main.addPlan(p, m.now()) }

func (t *transcript) addError(text string, now time.Time) {
	if text == "" {
		return
	}
	t.appendEntry(entry{kind: entryError, text: text}, now)
}

func (m *Model) addError(text string) { m.main.addError(text, m.now()) }

// appendStream grows the open entry of the same kind, so a reply that arrives
// in five chunks stays one entry and costs one re-render per chunk.
func (t *transcript) appendStream(kind entryKind, text string, at, now time.Time) {
	if text == "" {
		return
	}
	if at.IsZero() {
		at = now
	}
	if t.streamOpen && len(t.entries) > 0 {
		last := &t.entries[len(t.entries)-1]
		if last.kind == kind {
			last.text = capEntryText(last.text + text)
			last.end = at
			last.dirty = true
			t.dirty = true
			return
		}
	}
	// appendEntry ends the previous run, so the flag is raised after it.
	t.appendEntry(entry{kind: kind, text: capEntryText(text), at: at, end: at, open: kind == entryThought}, now)
	t.streamOpen = true
}

func capEntryText(s string) string {
	if len(s) <= entryTextCap {
		return s
	}
	keep := entryTextCap - len("…")
	if keep < 0 {
		keep = 0
	}
	start := len(s) - keep
	if start < 0 {
		start = 0
	}
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return "…" + s[start:]
}

func (m *Model) appendStream(kind entryKind, text string, at time.Time) {
	m.main.appendStream(kind, text, at, m.now())
}

// endRun ends the open run in place, reporting whether anything changed. The
// open run is always the last entry, which appendEntry keeps true.
func (t *transcript) endRun(at time.Time) bool {
	t.streamOpen = false
	n := len(t.entries)
	if n == 0 {
		return false
	}
	e := &t.entries[n-1]
	if e.kind != entryThought || !e.open {
		return false
	}
	e.open = false
	if !at.IsZero() {
		e.end = at
	}
	e.dirty = true
	return true
}

// closeStream ends the open run. A thought run freezes its elapsed time at the
// first event that follows it.
func (t *transcript) closeStream(at time.Time) {
	if t.endRun(at) {
		t.dirty = true
	}
}

func (t *transcript) breakStream(now time.Time) { t.closeStream(now) }

func (m *Model) breakStream() { m.main.breakStream(m.now()) }

// upsertTool keeps one row per toolCallId, updated in place. Cursor's todo
// writer is hidden: the todo stream owns that state.
func (t *transcript) upsertTool(tool *agent.ToolEvent, now time.Time) {
	if tool == nil || tool.IsTodoTool() {
		return
	}
	ev := *tool
	t.notePath(ev)
	// A tool call ends the run above it either way: an update that lands in an
	// existing row still means the thinking before it is over.
	t.closeStream(tool.At)
	if tool.ID != "" {
		if idx, ok := t.toolLine[tool.ID]; ok && idx >= 0 && idx < len(t.entries) && t.entries[idx].kind == entryTool {
			e := &t.entries[idx]
			e.tool = &ev
			e.dirty = true
			t.dirty = true
			return
		}
	}
	t.appendEntry(entry{kind: entryTool, tool: &ev, at: tool.At}, now)
	if tool.ID != "" {
		if t.toolLine == nil {
			t.toolLine = make(map[string]int)
		}
		t.toolLine[tool.ID] = len(t.entries) - 1
	}
}

func (m *Model) upsertTool(tool *agent.ToolEvent) { m.main.upsertTool(tool, m.now()) }

// notePath records which directories a basename has been seen in, so a row can
// fall back to dir/file once the basename is ambiguous.
func (t *transcript) notePath(tool agent.ToolEvent) {
	p := toolPath(&tool)
	if p == "" {
		return
	}
	base, dir := filepath.Base(p), filepath.Dir(p)
	if t.pathDirs == nil {
		t.pathDirs = make(map[string]map[string]struct{})
	}
	set := t.pathDirs[base]
	if set == nil {
		set = make(map[string]struct{})
		t.pathDirs[base] = set
	}
	if _, ok := set[dir]; ok {
		return
	}
	set[dir] = struct{}{}
	if len(set) == 2 {
		// The basename just became ambiguous, so every row showing it redraws.
		for i := range t.entries {
			if t.entries[i].kind == entryTool {
				t.entries[i].dirty = true
			}
		}
		t.dirty = true
	}
}

func (m Model) displayPath(tr *transcript, p string) string {
	if p == "" {
		return ""
	}
	base := filepath.Base(p)
	if len(tr.pathDirs[base]) < 2 {
		return base
	}
	return filepath.Join(filepath.Base(filepath.Dir(p)), base)
}

// clearTranscript drops the entries and every cache keyed off them.
func (m *Model) clearTranscript() {
	t := &m.main
	t.entries = nil
	t.toolLine = nil
	t.pathDirs = nil
	t.trimmed = false
	t.streamOpen = false
	t.dirty = true
	m.todoPlanned = 0
	m.todoDone = false
	// "the plan above" is gone, so there is nothing left to offer.
	m.retirePlanOffer()
	// The pending shell context is keyed off these entries as much as any
	// cache is: it describes `!` rows that are no longer on screen, and a
	// /clear is the user saying that conversation is over (plan 022 §3.6).
	m.dropShellContext()
}

// noteTodos turns the todo stream into the two dim transcript notes; the panel
// itself is the pinned home for the list.
func (m *Model) noteTodos(todos []agent.Todo) {
	if len(todos) == 0 {
		return
	}
	closed := 0
	for _, td := range todos {
		if td.Status == "completed" || td.Status == "cancelled" {
			closed++
		}
	}
	if closed == len(todos) {
		if !m.todoDone {
			m.todoDone = true
			m.addNote(fmt.Sprintf("tasks: %d/%d done", closed, len(todos)))
		}
		return
	}
	m.todoDone = false
	if len(todos) > m.todoPlanned {
		m.todoPlanned = len(todos)
		m.addNote(fmt.Sprintf("tasks: %d planned", len(todos)))
	}
}

func (m *Model) refreshViewport() {
	m.setViewportContent(m.vp.Height == 0 || m.vp.AtBottom())
}

func (m *Model) storeViewport(tr *transcript) {
	tr.yOffset = m.vp.YOffset
	tr.atBottom = m.vp.AtBottom()
}

// setViewportContent re-renders the dirty entries of the drawn transcript,
// joins everything and only then scrolls, so "stick to bottom" is decided by
// where the user was before the change, not after it.
func (m *Model) setViewportContent(stick bool) {
	tr := m.cur()
	// Every rebuild moves the text under the selection — a streaming chunk, a
	// tool update, /clear, a resize or a theme change — so the highlight goes
	// with it rather than pointing at rows that are no longer there.
	m.sel = selection{}
	if m.width <= 0 {
		tr.transcriptRows, tr.transcriptPlain = nil, nil
		m.vp.SetContent("")
		tr.dirty = false
		m.storeViewport(tr)
		return
	}
	key := m.renderKey()
	lines := make([]string, 0, len(tr.entries)+1)
	if tr.trimmed {
		lines = append(lines, renderSegs(m.width, seg{trimmedNote, styleFG(m.theme.Dim)}))
	}
	for i := range tr.entries {
		e := &tr.entries[i]
		// A shell row that is still running draws the spinner, and the cache is
		// keyed on things that do not move while it spins, so the row is
		// re-rendered on every rebuild until it settles. The tick is what asks
		// for those rebuilds (handleTick), so this costs one comparison per
		// entry rather than a walk of its own.
		if e.dirty || e.renderedFor != key || (e.shell != nil && !e.shell.done) {
			e.rendered = m.renderEntry(tr, e, key)
			e.renderedFor = key
			e.dirty = false
			tr.renders++
		}
		lines = append(lines, e.rendered...)
	}
	// The canonical rows: exactly what the viewport is about to hold, plus the
	// plain form the selection cuts and copies from.
	tr.transcriptRows = lines
	tr.transcriptPlain = make([]string, len(lines))
	for i, ln := range lines {
		tr.transcriptPlain[i] = strings.TrimRight(ansi.Strip(ln), " ")
	}
	m.vp.SetContent(strings.Join(lines, "\n"))
	if stick {
		m.vp.GotoBottom()
	}
	tr.dirty = false
	m.storeViewport(tr)
}

func (m *Model) renderEntry(tr *transcript, e *entry, key renderKey) []string {
	switch e.kind {
	case entryUser:
		// The mark carries the colour; the two-space continuation indent is
		// blank, so it stays unstyled rather than carrying a pointless SGR.
		markSt, textSt := styleFG(m.theme.UserMark), styleFG(m.theme.User).Bold(true)
		contSt := lipgloss.NewStyle()
		if e.interject {
			// It joined a turn that was already running, so it does not get
			// the prompt mark a turn of its own does.
			return hangingRowsStyled(e.text, "↳ ", "  ", key.width, markSt, contSt, textSt)
		}
		return hangingRowsStyled(e.text, "❯ ", "  ", key.width, markSt, contSt, textSt)
	case entryAssistant:
		return renderMarkdown(e.text, key.width, m.theme)
	case entryThought:
		return m.thoughtRows(e, key)
	case entryTool:
		return m.renderTool(tr, e.tool, key)
	case entryNote:
		return hangingRows(e.text, "", "", key.width, styleFG(m.theme.Dim))
	case entryPlan:
		return m.planRows(e.plan, key)
	case entryError:
		return hangingRows(e.text, "error: ", "  ", key.width, styleFG(m.theme.Err))
	case entryShell:
		return m.shellRows(e, key)
	}
	return nil
}

// thoughtRows collapses a whole thought run to one row; Ctrl+O reveals the text.
func (m *Model) thoughtRows(e *entry, key renderKey) []string {
	// Cursor delivers a thought run in a burst, so a whole run can land inside
	// one second and the row is honestly "0s" — which reads like a bug seven
	// rows in a row. Nothing was measured, so nothing is shown.
	head, sep := "+ Thought", " for "
	if e.open {
		head, sep = "+ Thinking…", " "
	}
	if d := e.end.Sub(e.at); d.Round(time.Second) > 0 {
		head += sep + formatElapsed(d)
	}
	rows := []string{renderSegs(key.width, seg{head, styleFG(m.theme.Thought)})}
	if !key.expanded {
		return rows
	}
	st := lipgloss.NewStyle().Foreground(m.theme.Thought).Italic(true)
	return append(rows, hangingRows(e.text, "  ", "  ", key.width, st)...)
}

// planRows is the plan block: a header, the overview, the plan body through
// markdown-lite and the todos it proposes. The card on the modal band answers
// it; this is the only place the plan text itself is readable.
func (m *Model) planRows(p *agent.PlanEvent, key renderKey) []string {
	if p == nil {
		return nil
	}
	rows := []string{renderSegs(key.width,
		seg{"PLAN ", styleFG(m.theme.Accent).Bold(true)},
		seg{planName(p), styleFG(m.theme.Bright).Bold(true)},
	)}
	if strings.TrimSpace(p.Overview) != "" {
		rows = append(rows, hangingRows(p.Overview, "", "", key.width, styleFG(m.theme.Dim))...)
	}
	if strings.TrimSpace(p.Plan) != "" {
		rows = append(rows, renderMarkdown(p.Plan, key.width, m.theme)...)
	}
	for _, td := range p.Todos {
		glyph, gst := m.todoGlyph(td.Status)
		rows = append(rows, renderSegs(key.width,
			seg{"  " + glyph + " ", gst},
			seg{sanitizeLine(td.Content), styleFG(m.theme.Dim)},
		))
	}
	return rows
}

// hangingRowsStyled wraps prose and indents the continuation rows under the
// first, colouring the prefix glyph separately from the body text. Only the
// first row's prefix paints with firstSt: a continuation row carries no
// glyph of its own (usually plain indent spaces), so colouring it would only
// add ANSI noise nothing on screen shows. textSt paints every row's text.
// hangingRowLines wraps text under a hanging indent and hands each row's
// prefix and content to fn. hangingRows and hangingRowsStyled share it so the
// wrap and indent arithmetic lives in exactly one place while each stays free
// to paint the row its own way.
func hangingRowLines(text string, first, cont string, width int, fn func(i int, prefix, ln string)) {
	indent := lipgloss.Width(first)
	if w := lipgloss.Width(cont); w > indent {
		indent = w
	}
	wrapped := wrapProse(text, width-indent)
	for i, ln := range strings.Split(wrapped, "\n") {
		prefix := cont
		if i == 0 {
			prefix = first
		}
		fn(i, prefix, ln)
	}
}

// hangingRows paints the whole row, prefix and text alike, with one style. It
// renders each row as a single seg, exactly as it always has: a two-seg split
// clamps differently when the prefix alone fills width (renderSegSpans stops
// before the text seg, so it truncates without an ellipsis), and this helper's
// callers must keep rendering byte-identically.
func hangingRows(text string, first, cont string, width int, st lipgloss.Style) []string {
	var out []string
	hangingRowLines(text, first, cont, width, func(_ int, prefix, ln string) {
		out = append(out, renderSegs(width, seg{prefix + ln, st}))
	})
	return out
}

// hangingRowsStyled is hangingRows with the prefix painted apart from the
// text: firstSt for the opening prefix, contSt for the continuation indent,
// textSt for the text itself. contSt is its own parameter so a blank indent
// can go unstyled while a visible continuation glyph keeps its colour.
func hangingRowsStyled(text string, first, cont string, width int, firstSt, contSt, textSt lipgloss.Style) []string {
	var out []string
	hangingRowLines(text, first, cont, width, func(i int, prefix, ln string) {
		pst := contSt
		if i == 0 {
			pst = firstSt
		}
		out = append(out, renderSegs(width, seg{prefix, pst}, seg{ln, textSt}))
	})
	return out
}

// wrapProse word-wraps and then hard-wraps, so an unbroken token longer than
// the terminal is broken instead of being cut by the viewport.
func wrapProse(s string, width int) string {
	if width <= 0 {
		return s
	}
	w := proseWidth(width)
	return ansi.Hardwrap(ansi.Wordwrap(s, w, ""), w, true)
}

func proseWidth(width int) int {
	w := width - 2
	if w > proseMaxWidth {
		w = proseMaxWidth
	}
	if w < 1 {
		w = 1
	}
	return w
}

// seg is one styled piece of a row. Rows are assembled from segs so a row can
// be clamped to the terminal width without cutting inside an escape sequence.
type seg struct {
	text  string
	style lipgloss.Style
}

// idSeg is a seg a click can land on. Only the status rows draw any; every
// other row is plain segs and reports no spans.
type idSeg struct {
	seg
	id spanID
}

func styleFG(c lipgloss.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

// renderSegs styles and concatenates segs, clamping the result to width.
func renderSegs(width int, segs ...seg) string {
	ids := make([]idSeg, len(segs))
	for i, s := range segs {
		ids[i] = idSeg{seg: s}
	}
	row, _ := renderSegSpans(width, ids)
	return row
}

// renderSegSpans is renderSegs plus where each identified seg landed. The
// spans come out of the same walk that wrote the row — separators, drops,
// display widths and the truncating clamp included — so a hit test can never
// be a second guess at what was drawn.
func renderSegSpans(width int, segs []idSeg) (string, []segSpan) {
	if width <= 0 {
		return "", nil
	}
	var b strings.Builder
	var spans []segSpan
	used := 0
	for _, s := range segs {
		if s.text == "" {
			continue
		}
		avail := width - used
		if avail <= 0 {
			break
		}
		w := lipgloss.Width(s.text)
		if w > avail {
			cut := clampWidth(s.text, avail)
			b.WriteString(s.style.Render(cut))
			if s.id != spanNone {
				// clampWidth can land short of avail — a wide rune it could
				// not split, plus the ellipsis — and the cells it did not use
				// were never drawn, so a click there must not land on it.
				spans = append(spans, segSpan{id: s.id, x0: used, x1: used + lipgloss.Width(cut)})
			}
			break
		}
		b.WriteString(s.style.Render(s.text))
		if s.id != spanNone {
			spans = append(spans, segSpan{id: s.id, x0: used, x1: used + w})
		}
		used += w
	}
	return b.String(), spans
}

// clampWidth truncates to w display cells, marking the cut with an ellipsis.
func clampWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// sanitizeLine folds a string onto one line: escape sequences and control
// characters go, and runs of whitespace collapse. Agent text is already
// sanitised at ingestion; this keeps titles to one row regardless.
func sanitizeLine(s string) string {
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(dropControls(ansi.Strip(s))), " ")
}

// plainLine makes one agent-supplied line safe to draw while keeping its
// indentation: escape sequences go, tabs become spaces so widths add up.
func plainLine(s string) string {
	if s == "" {
		return ""
	}
	return dropControls(strings.ReplaceAll(ansi.Strip(s), "\t", strings.Repeat(" ", tabWidth)))
}

func dropControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		// unicode.IsControl is the whole Cc category, not just C0 and DEL:
		// U+009B is a single-code-point CSI, and a terminal that reads C1 in
		// UTF-8 would take an agent's description as an escape sequence.
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Round(time.Second) / time.Second)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

func formatMillis(ms int) string {
	if ms <= 0 {
		return ""
	}
	if ms < 60_000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return formatElapsed(time.Duration(ms) * time.Millisecond)
}

// ---------------------------------------------------------------- tool rows

func (m *Model) renderTool(tr *transcript, tool *agent.ToolEvent, key renderKey) []string {
	if tool == nil {
		return nil
	}
	if tool.IsTask() {
		return m.taskRows(tool, key.width)
	}
	switch tool.Kind {
	case "edit":
		return m.editRows(tr, tool, key)
	case "execute":
		return m.execRows(tool, key)
	case "read":
		return m.readRows(tr, tool, key)
	}
	return m.otherRows(tool, key.width)
}

// toolRow renders "<glyph> <label>  <target>  <suffix>". The target is clamped
// first so a long path never pushes the counts or the exit code off the row.
func (m *Model) toolRow(glyph string, gst lipgloss.Style, label, target, suffix string, sufSt lipgloss.Style, width int) string {
	fixed := 2 + lipgloss.Width(label)
	if suffix != "" {
		fixed += 2 + lipgloss.Width(suffix)
	}
	if target != "" {
		if avail := width - fixed - 2; avail >= 1 {
			target = clampWidth(target, avail)
		}
	}
	segs := []seg{{glyph + " ", gst}, {label, styleFG(m.theme.ToolKind)}}
	if target != "" {
		segs = append(segs, seg{"  " + target, styleFG(m.theme.FG)})
	}
	if suffix != "" {
		segs = append(segs, seg{"  " + suffix, sufSt})
	}
	return renderSegs(width, segs...)
}

func (m *Model) toolHead(t *agent.ToolEvent, label, target, suffix string, sufSt lipgloss.Style, width int) string {
	glyph, gst := m.statusGlyph(t.Status)
	return m.toolRow(glyph, gst, label, target, suffix, sufSt, width)
}

func (m *Model) statusGlyph(status string) (string, lipgloss.Style) {
	switch status {
	case "in_progress":
		return "⟳", styleFG(m.theme.Accent)
	case "completed":
		return "✓", styleFG(m.theme.OK)
	case "failed":
		return "✗", styleFG(m.theme.Err)
	case "cancelled":
		return "–", styleFG(m.theme.Dim)
	default:
		return "◌", styleFG(m.theme.Dim)
	}
}

func (m *Model) dimRow(text string, width int) string {
	return renderSegs(width, seg{text, styleFG(m.theme.Dim)})
}

func (m *Model) readRows(tr *transcript, tool *agent.ToolEvent, key renderKey) []string {
	rows := []string{m.toolHead(tool, "read", m.toolTarget(tr, tool), "", styleFG(m.theme.Dim), key.width)}
	if !key.expanded || tool.Output == nil {
		return rows
	}
	return append(rows, m.outputRows(tool.Output.Content, key.width)...)
}

func (m *Model) editRows(tr *transcript, tool *agent.ToolEvent, key renderKey) []string {
	added, removed, truncated := diffTotals(tool.Diffs)
	suffix, sufSt := "", styleFG(m.theme.Dim)
	switch {
	case truncated:
		suffix = fmt.Sprintf("diff too large (%d KiB)", diffKiB(tool.Diffs))
		sufSt = styleFG(m.theme.Warn)
	case len(tool.Diffs) > 0:
		suffix = fmt.Sprintf("+%d −%d", added, removed)
		sufSt = styleFG(m.theme.OK)
	}
	rows := []string{m.toolHead(tool, "edit", m.toolTarget(tr, tool), suffix, sufSt, key.width)}
	if truncated {
		return rows
	}
	return append(rows, m.diffRows(tool.Diffs, key)...)
}

func (m *Model) diffRows(diffs []agent.ToolDiff, key renderKey) []string {
	var hunks []textdiff.Hunk
	for _, d := range diffs {
		_, _, hs, err := textdiff.Lines(d.OldText, d.NewText)
		if err != nil {
			return []string{m.dimRow("  diff too large", key.width)}
		}
		hunks = append(hunks, hs...)
	}
	if len(hunks) == 0 {
		return nil
	}
	if key.expanded {
		var rows []string
		for _, h := range hunks {
			rows = append(rows, m.hunkRows(h, key.width, 0)...)
		}
		return rows
	}
	rows := m.hunkRows(hunks[0], key.width, editCollapsedLines)
	switch more := len(hunks) - 1; {
	case more == 1:
		rows = append(rows, m.dimRow("  … 1 more hunk · ⌃o expand", key.width))
	case more > 1:
		rows = append(rows, m.dimRow(fmt.Sprintf("  … %d more hunks · ⌃o expand", more), key.width))
	case len(hunks[0].Lines) > editCollapsedLines:
		rows = append(rows, m.dimRow("  … ⌃o expand", key.width))
	}
	return rows
}

func (m *Model) hunkRows(h textdiff.Hunk, width, limit int) []string {
	lines := h.Lines
	if limit > 0 && len(lines) > limit {
		lines = lines[:limit]
	}
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		no := ln.NewNo
		st := styleFG(m.theme.FG)
		switch ln.Kind {
		case textdiff.Add:
			st = styleFG(m.theme.DiffAdd)
		case textdiff.Del:
			no = ln.OldNo
			st = styleFG(m.theme.DiffDel)
		}
		out = append(out, renderSegs(width,
			seg{fmt.Sprintf("%6d ", no), styleFG(m.theme.LineNo)},
			seg{string(ln.Kind) + " " + plainLine(ln.Text), st},
		))
	}
	return out
}

func (m *Model) execRows(t *agent.ToolEvent, key renderKey) []string {
	suffix, sufSt := "", styleFG(m.theme.Dim)
	failed := t.Output != nil && t.Output.ExitCode != nil && *t.Output.ExitCode != 0
	if failed {
		suffix = fmt.Sprintf("exit %d", *t.Output.ExitCode)
		sufSt = styleFG(m.theme.Err)
	}
	rows := []string{m.toolHead(t, "bash", execCommand(t), suffix, sufSt, key.width)}
	if t.Output == nil {
		return rows
	}
	if key.expanded {
		rows = append(rows, m.outputRows(t.Output.Stdout, key.width)...)
		return append(rows, m.outputRows(t.Output.Stderr, key.width)...)
	}
	head := t.Output.StdoutHead
	if failed {
		head = t.Output.StderrHead
	}
	if line := firstNonEmpty(head); line != "" {
		rows = append(rows, m.dimRow("  "+plainLine(line), key.width))
	}
	return rows
}

func (m *Model) otherRows(t *agent.ToolEvent, width int) []string {
	label := sanitizeLine(t.Kind)
	if label == "" {
		label = "tool"
	}
	target := sanitizeLine(t.Title)
	if (t.Kind == "search" || t.Kind == "fetch") && t.RawInput != "" {
		if q := queryNotInTitle(t.RawInput, target); q != "" {
			target = strings.TrimSpace(target + " " + sanitizeLine(q))
		}
	}
	return []string{m.toolHead(t, label, target, "", styleFG(m.theme.Dim), width)}
}

// taskRows render a sub-agent call. The model name only appears once the
// cursor/task receipt has been joined in.
func (m *Model) taskRows(t *agent.ToolEvent, width int) []string {
	desc := taskDesc(t)
	st := t.Status
	if t.Task != nil && t.Task.Status != "" {
		st = string(t.Task.Status)
	}
	if st != "completed" && st != "failed" && st != "cancelled" {
		return []string{m.toolRow("●", styleFG(m.theme.Accent), "agent", desc, "running", styleFG(m.theme.Dim), width)}
	}
	suffix := ""
	if t.Task != nil {
		suffix = formatMillis(t.Task.DurationMs)
		if t.Task.Receipt && t.Task.Model != "" {
			if suffix != "" {
				suffix += " · "
			}
			suffix += shortModelName(t.Task.Model)
		}
	}
	glyph, gst := m.statusGlyph(st)
	return []string{m.toolRow(glyph, gst, "agent", desc, suffix, styleFG(m.theme.Dim), width)}
}

func (m *Model) outputRows(text string, width int) []string {
	var out []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		out = append(out, m.dimRow("  "+plainLine(ln), width))
		if len(out) >= outputPreviewLines {
			break
		}
	}
	return out
}

func (m *Model) toolTarget(tr *transcript, tool *agent.ToolEvent) string {
	if p := m.displayPath(tr, toolPath(tool)); p != "" {
		return p
	}
	return sanitizeLine(tool.Title)
}

// toolPath finds the file a read or edit acted on: the location cursor reports,
// else rawInput.path, else the diff it produced.
// queryNotInTitle returns raw only when it would tell the user something the
// title does not already say. Cursor titles its search "Find `**/main.go`" and
// sends rawInput `{"pattern":"**/main.go"}`, so appending it verbatim printed
// the pattern twice, the second time as raw JSON.
func queryNotInTitle(raw, title string) string {
	vals := rawInputValues(raw)
	if len(vals) == 0 {
		return raw
	}
	for _, v := range vals {
		if !strings.Contains(title, v) {
			return raw
		}
	}
	return ""
}

// rawInputValues pulls the quoted string values out of a JSON object, ignoring
// the keys. A non-object, or one with no string values, yields nothing and the
// caller falls back to showing raw.
func rawInputValues(raw string) []string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil
	}
	vals := make([]string, 0, len(obj))
	for _, v := range obj {
		if s, ok := v.(string); ok && s != "" {
			vals = append(vals, s)
		}
	}
	return vals
}

func toolPath(t *agent.ToolEvent) string {
	if len(t.Locations) > 0 && t.Locations[0] != "" {
		return t.Locations[0]
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(t.RawInput), &obj); err == nil {
		for _, k := range []string{"path", "filePath", "file"} {
			if v, ok := obj[k].(string); ok && v != "" {
				return v
			}
		}
	}
	for _, d := range t.Diffs {
		if d.Path != "" {
			return d.Path
		}
	}
	return ""
}

// execCommand prefers the command cursor put in rawInput; its title is the same
// command wrapped in backticks.
func execCommand(t *agent.ToolEvent) string {
	if t.RawInput != "" && !strings.HasPrefix(t.RawInput, "{") {
		return sanitizeLine(t.RawInput)
	}
	return strings.Trim(sanitizeLine(t.Title), "`")
}

func taskDesc(t *agent.ToolEvent) string {
	if t.Task != nil && t.Task.Description != "" {
		return sanitizeLine(t.Task.Description)
	}
	return strings.TrimPrefix(sanitizeLine(t.Title), "Task: ")
}

// shortModelName drops cursor's provider prefix: the row has no space for it.
// shortModelName is the model id as a row can afford it: cursor's `cursor-`
// prefix and a routing prefix such as `openrouter/` carry no information the
// provider column does not already show.
func shortModelName(s string) string {
	s = strings.TrimPrefix(s, "cursor-")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return sanitizeLine(s)
}

func diffTotals(diffs []agent.ToolDiff) (added, removed int, truncated bool) {
	for _, d := range diffs {
		added += d.Added
		removed += d.Removed
		if d.Truncated {
			truncated = true
		}
	}
	return added, removed, truncated
}

func diffKiB(diffs []agent.ToolDiff) int {
	n := 0
	for _, d := range diffs {
		n += len(d.OldText) + len(d.NewText)
	}
	return n / 1024
}

func firstNonEmpty(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			return ln
		}
	}
	return ""
}
