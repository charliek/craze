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
	"github.com/charliek/craze/internal/transcript"
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
	// trimmedNote leads a pane whose oldest rows were dropped: the shared
	// model's wording, so every client draws the same one.
	trimmedNote = transcript.TrimmedNote
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

// entry is one row of a pane's display list: what the row shows, and its render
// cache (rendered, renderedFor, dirty). A shared row's display value is read
// from the shared model's entry it shows (show); a local row's is the client's
// own.
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
	// local marks a row this client wrote for a message of its own rather than
	// one an event drew (see pane): it is no event's, and no other client has it.
	local bool
	// id is the shared entry the row shows, the zero id for a local row.
	id transcript.EntryID
	// streaming marks the row showing the shared model's open stream entry,
	// whose text is the model's tail and grows with every chunk.
	streaming bool
	// cont marks a continuation (execution amendment X30): the run that was
	// open at /clear, shown from contFrom bytes into its text and dated at the
	// first chunk after the clear, which is what that chunk has always drawn.
	// Once the run is longer than the stream cap its text is a moving tail and
	// the offset no longer names a place in it, so the row shows the whole
	// tail (the recorded approximation).
	cont     bool
	contFrom int

	// rendered is never written to once it is set: a render makes a new
	// slice, which is what lets a paint's rowIndex hold it rather than copy it.
	rendered    []string
	renderedFor renderKey
	// dirty marks content that changed under an unchanged key.
	dirty bool
	// plain is rendered as the selection reads it, worked out the first time
	// a selection asks (pane.plainRow) and dropped when the entry renders again.
	plain []string
}

func (m Model) renderKey() renderKey {
	return renderKey{width: m.width, theme: m.theme.Name, expanded: m.expanded}
}

// kindOf is the row kind a shared entry draws as.
func kindOf(k transcript.Kind) entryKind {
	switch k {
	case transcript.KindUser:
		return entryUser
	case transcript.KindAssistant:
		return entryAssistant
	case transcript.KindThought:
		return entryThought
	case transcript.KindTool:
		return entryTool
	case transcript.KindPlan:
		return entryPlan
	case transcript.KindError:
		return entryError
	}
	return entryNote
}

// show makes r display the shared entry e of tr, as it stands now: its kind,
// its text — the model's tail for the open stream entry, whose stored text is
// empty (X7, X24), read afresh on every touch — its payload, its span and its
// marks. An error entry's text is the error's, which the model read through
// Options.ErrText. A continuation keeps its own At and shows the text from its
// offset. r is re-rendered at the next paint.
func (r *entry) show(e *transcript.Entry, tr *transcript.Transcript) {
	text := e.Text
	if e.Streaming {
		text = tr.Tail()
	}
	at := e.At
	if r.cont {
		at = r.at
		if !e.Cut && r.contFrom <= len(text) {
			text = text[r.contFrom:]
		}
	}
	r.kind = kindOf(e.Kind)
	r.text = text
	r.tool, r.plan = e.Tool, e.Plan
	r.at, r.end = at, e.End
	r.open, r.interject, r.streaming = e.Open, e.Interject, e.Streaming
	r.local = false
	r.id = e.ID
	r.dirty = true
}

// stamp is the time a row drawn from an event is written at: the event's own
// At, and this client's clock only for an event that carries none, which is a
// unit test's fixture — every production event is stamped. The shared model
// stamps its entries by the same rule (plan 024 §3.2); this is for the one row
// the pane writes on an event's behalf (a todo note, todoNoteOwed).
func (m *Model) stamp(at time.Time) time.Time {
	if at.IsZero() {
		return m.now()
	}
	return at
}

// userText is what a user row shows of text that went to the agent. The shell
// context in front of it is wire content and never display content (plan 022
// §3.6): the row shows the message, not the command output craze attached to it
// — which is already on screen, in the `!` row the user watched it come out of.
// It is this client's own send's rule (addUser), and the shared model's for
// every user entry it draws (agent.SplitShellContext), which is what makes the
// rule hold for the rows the engine reports as well as the ones this client
// sends.
func userText(text string) string {
	_, text = agent.SplitShellContext(text)
	return text
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

// notePath records which directories a basename has been seen in, so a row can
// fall back to dir/file once the basename is ambiguous.
func (t *pane) notePath(tool agent.ToolEvent) {
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
		for _, e := range t.rows {
			if e.kind == entryTool {
				e.dirty = true
			}
		}
		t.dirty = true
	}
}

func (m Model) displayPath(tr *pane, p string) string {
	if p == "" {
		return ""
	}
	base := filepath.Base(p)
	if len(tr.pathDirs[base]) < 2 {
		return base
	}
	return filepath.Join(filepath.Base(filepath.Dir(p)), base)
}

// todoNoteOwed is the pane's half of the todo stream's two dim notes — the
// panel itself is the pinned home for the list — and the note, if any, this
// pane owes for todos: today's dedupe over m.todoPlanned and m.todoDone, which
// /clear resets, moved on by the list. The shared model writes the notes under
// its own dedupe, which is the session's and never resets; the pane's decides
// what the pane shows, in both directions (execution amendment X31, revised at
// r17): a note the fold writes is the display only when the pane owes it, and
// is given no row otherwise, and a note the pane owes that the fold does not
// write is the pane's own (applyEvent).
//
// The two disagree only where /clear lowers the pane's counters and not the
// model's. todos is the event's own list, the whole of the new one (the fold's
// rule is replacement; plan 027 §3.13), so the pane never notes a list ahead
// of the event that carries it, nor an old one for an event that clears it.
func (m *Model) todoNoteOwed(todos []agent.Todo) string {
	if len(todos) == 0 {
		return ""
	}
	closed := 0
	for _, td := range todos {
		if td.Status == "completed" || td.Status == "cancelled" {
			closed++
		}
	}
	if closed == len(todos) {
		if m.todoDone {
			return ""
		}
		m.todoDone = true
		return fmt.Sprintf("tasks: %d/%d done", closed, len(todos))
	}
	m.todoDone = false
	if len(todos) <= m.todoPlanned {
		return ""
	}
	m.todoPlanned = len(todos)
	return fmt.Sprintf("tasks: %d planned", len(todos))
}

func (m *Model) refreshViewport() {
	m.setViewportContent(m.vp.Height == 0 || m.vp.AtBottom())
}

func (m *Model) storeViewport(tr *pane) {
	tr.yOffset = m.vp.YOffset
	tr.atBottom = m.vp.AtBottom()
}

// setViewportContent re-renders the dirty entries of the drawn transcript,
// hands the viewport the rows (paint) and only then scrolls, so "stick to
// bottom" is decided by where the user was before the change, not after it.
func (m *Model) setViewportContent(stick bool) {
	tr := m.cur()
	// Every rebuild moves the text under the selection — a streaming chunk, a
	// tool update, /clear, a resize or a theme change — so the highlight goes
	// with it rather than pointing at rows that are no longer there.
	m.sel = selection{}
	if m.width <= 0 {
		tr.drawn = nil
		tr.reshaped = false
		m.vp.setRows(noRows)
		tr.dirty = false
		m.storeViewport(tr)
		return
	}
	// The canonical rows: exactly what the viewport is about to hold, which
	// the hit test, the highlight and the copy read too.
	tr.drawn = m.paint(tr)
	if paintHook != nil {
		paintHook(m, tr)
	}
	m.vp.setRows(tr.drawn)
	if stick {
		m.vp.GotoBottom()
	}
	tr.dirty = false
	m.storeViewport(tr)
}

// paintHook, when a test sets it, is told of every paint, with the pane just
// painted (TestMain's paint watch). Nil in production, like foldHook.
var paintHook func(m *Model, tr *pane)

func (m *Model) renderEntry(tr *pane, e *entry, key renderKey) []string {
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
		seg{transcript.PlanName(p), styleFG(m.theme.Bright).Bold(true)},
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
// the terminal is broken instead of being cut by the viewport. A hyphen that
// begins a word is held out of the word wrap (holdFlagHyphens), so a flag
// stays whole: `craze -c.` moves to the next row as one word.
func wrapProse(s string, width int) string {
	if width <= 0 {
		return s
	}
	w := proseWidth(width)
	s, stand, held := holdFlagHyphens(s)
	out := ansi.Hardwrap(ansi.Wordwrap(s, w, ""), w, true)
	if held {
		out = strings.ReplaceAll(out, string(stand), "-")
	}
	return out
}

// flagHyphen is the private-use rune that stands in, while prose is wrapped,
// for a hyphen that begins a word (holdFlagHyphens), unless the text holds it
// already, when the next one it does not hold is used. ansi.Wordwrap takes
// every hyphen as a place to break, and at the end of a full row it writes the
// space before one and the hyphen itself past the width, so the hard wrap
// after it leaves the hyphen alone on a row of its own and the word's rest on
// the next: `run craze` / ` -` / `c.` at 110 columns (plan 031 C7r,
// verification V3). A private-use rune is one cell wide, as the hyphen is, and
// is word text to the wrap; it is put back once the text is wrapped.
const flagHyphen = '\ue000'

// holdRune is a private-use rune s does not contain, or false when it holds
// them all. One pass collects the ones s holds (usually none), so a passage
// full of them costs no more than one scan.
func holdRune(s string) (rune, bool) {
	var held map[rune]bool
	for _, r := range s {
		if r >= flagHyphen && r <= '\uf8ff' {
			if held == nil {
				held = map[rune]bool{}
			}
			held[r] = true
		}
	}
	for r := flagHyphen; r <= '\uf8ff'; r++ {
		if !held[r] {
			return r, true
		}
	}
	return 0, false
}

// holdFlagHyphens is s with each hyphen that begins a word — the dash of a
// flag, `-c`, `--model`, or of a negative number — swapped for a stand-in
// rune (flagHyphen, or another private-use rune when s holds that one, so a
// literal one in the text is never mistaken for a swap), that rune, and
// whether any was swapped. A word begins at the text's start or after a space
// or a line break; an escape sequence between is no text, so a styled `-c`
// begins one too. A hyphen, or a run of them, begins a word only when text
// follows it: a lone dash between spaces stays a place to break.
func holdFlagHyphens(s string) (string, rune, bool) {
	if !strings.Contains(s, "-") {
		return s, 0, false
	}
	stand, ok := holdRune(s)
	if !ok {
		return s, 0, false
	}
	// The text's graphemes and escape sequences, in order.
	var parts []string
	var state byte
	for rest := s; rest != ""; {
		seq, _, n, next := ansi.DecodeSequence(rest, state, nil)
		if n <= 0 {
			return s, 0, false
		}
		parts = append(parts, seq)
		rest, state = rest[n:], next
	}
	escape := func(p string) bool { return p[0] == ansi.ESC }
	space := func(p string) bool {
		r, _ := utf8.DecodeRuneInString(p)
		return r != ' ' && unicode.IsSpace(r)
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	held, wordStart := false, true
	for i := 0; i < len(parts); {
		p := parts[i]
		switch {
		case escape(p):
			b.WriteString(p)
			i++
			continue
		case p != "-" || !wordStart:
			b.WriteString(p)
			wordStart = space(p)
			i++
			continue
		}
		// A run of hyphens at a word's start, and the escapes among them.
		j := i
		for j < len(parts) && (parts[j] == "-" || escape(parts[j])) {
			j++
		}
		hold := j < len(parts) && !space(parts[j])
		for _, q := range parts[i:j] {
			if hold && q == "-" {
				b.WriteRune(stand)
				held = true
				continue
			}
			b.WriteString(q)
		}
		wordStart, i = false, j
	}
	if !held {
		return s, 0, false
	}
	return b.String(), stand, true
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

func (m *Model) renderTool(tr *pane, tool *agent.ToolEvent, key renderKey) []string {
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

func (m *Model) readRows(tr *pane, tool *agent.ToolEvent, key renderKey) []string {
	rows := []string{m.toolHead(tool, "read", m.toolTarget(tr, tool), "", styleFG(m.theme.Dim), key.width)}
	if !key.expanded || tool.Output == nil {
		return rows
	}
	return append(rows, m.outputRows(tool.Output.Content, key.width)...)
}

func (m *Model) editRows(tr *pane, tool *agent.ToolEvent, key renderKey) []string {
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

func (m *Model) toolTarget(tr *pane, tool *agent.ToolEvent) string {
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
