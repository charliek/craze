package tui

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/key"
	bubblesvp "github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
)

// drawnRows is every row the pane's last paint drew, oldest first: the
// rowIndex flattened, which nothing outside a test ever does.
func (t *pane) drawnRows() []string {
	if t.drawn == nil {
		return nil
	}
	return t.drawn.appendRows(make([]string, 0, t.drawn.total), 0, t.drawn.total)
}

// plainRows is every drawn row as the selection reads it (plainRow): what the
// paint used to build eagerly as transcriptPlain, for the tests that read it.
func (t *pane) plainRows() []string {
	out := make([]string, t.drawnLen())
	for i := range out {
		out[i] = t.plainRow(i)
	}
	return out
}

// ------------------------------------------------------------- the paint watch

// paints is TestMain's paint watch: every paint of every test, checked against
// the full rebuild it stands in for. The index a paint made must be the trim
// note, then every entry's rendered rows in the list's order, each span
// starting where the one before it ended; every entry must be clean under the
// key; and no row the paint assembled may hold a line break or a carriage
// return. bubbles' viewport split a row holding a "\n" (after making "\r\n"
// "\n") into lines of their own; craze's viewport shows a row as one line, so
// the paint splits an entry's rows into their lines (physicalLines) and the
// watch holds it to that: a line break left in a row is a line the viewport
// would lose. A lone "\r" is refused too, since nothing craze draws holds one
// (physicalLines has why that matters).
var paints paintWatch

type paintWatch struct {
	mu     sync.Mutex
	failed error
}

func installPaintWatch() { paintHook = paints.check }

func (w *paintWatch) err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed
}

func (w *paintWatch) check(m *Model, tr *pane) {
	if err := checkPaint(m, tr); err != nil {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.failed == nil {
			w.failed = fmt.Errorf("paint watch: %w\n%s", err, debug.Stack())
		}
	}
}

// checkPaint is the paint watch's rule for one paint of tr.
func checkPaint(m *Model, tr *pane) error {
	x := tr.drawn
	key := m.renderKey()
	if x.key != key || x.trimmed != tr.trimmed {
		return fmt.Errorf("the index was made for %+v (trimmed %v), the pane is drawn under %+v (trimmed %v)", x.key, x.trimmed, key, tr.trimmed)
	}
	off := 0
	if tr.trimmed {
		off = 1
	}
	if x.spans() != off+len(tr.rows) {
		return fmt.Errorf("the index holds %d spans for %d rows (trimmed %v)", x.spans(), len(tr.rows), tr.trimmed)
	}
	start := 0
	for j := 0; j < x.spans(); j++ {
		s := x.span(j)
		if s.start != start {
			return fmt.Errorf("span %d starts at row %d, want %d", j, s.start, start)
		}
		start += len(s.rows)
		if j < off {
			if s.e != nil || len(s.rows) != 1 || s.rows[0] != renderSegs(m.width, seg{trimmedNote, styleFG(m.theme.Dim)}) {
				return fmt.Errorf("span 0 is not the trim note: %q", s.rows)
			}
			continue
		}
		e := tr.rows[j-off]
		switch {
		case s.e != e:
			return fmt.Errorf("span %d shows another entry than row %d", j, j-off)
		case !sameRows(s.rows, e.rendered):
			return fmt.Errorf("span %d does not hold row %d's rendered rows", j, j-off)
		case e.dirty || e.renderedFor != key:
			return fmt.Errorf("row %d is still dirty after the paint", j-off)
		}
	}
	if start != x.total {
		return fmt.Errorf("the index says %d rows, its spans hold %d", x.total, start)
	}
	for _, s := range x.tail {
		for _, row := range s.rows {
			if strings.ContainsAny(row, "\r\n") {
				return fmt.Errorf("a drawn row holds a line break: %q", row)
			}
		}
	}
	return nil
}

// ------------------------------------------------------------ viewport parity

// crazeKeyMap is the keymap craze gave bubbles' viewport.
func crazeKeyMap() bubblesvp.KeyMap {
	return bubblesvp.KeyMap{
		PageUp:   key.NewBinding(key.WithKeys("pgup")),
		PageDown: key.NewBinding(key.WithKeys("pgdown")),
	}
}

// parityRow is one random row of the kinds a transcript draws, and a few it
// should not: plain words, styled runs, wide and combining graphemes, flags,
// tabs (which lipgloss widens past the width it was measured at), trailing
// blanks, rows far wider than any viewport here, and line breaks — a styled
// run of two lines, which lipgloss pads to one width, and a bare "\n" or
// "\r\n" between runs — as a value an agent sent can put inside a row (a
// tool's location). It is a row as rendered, before physicalLines splits it.
// Never a lone "\r": nothing craze draws holds one (physicalLines).
func parityRow(rng *rand.Rand) string {
	if rng.Intn(8) == 0 {
		return ""
	}
	bits := []string{"word", "a", "longer-word", " ", "  ", "\t", "日本語", "🙂", "é", "🇯🇵", "x", "│", "…", "tail   ", "two\nlines"}
	styles := []lipgloss.Style{
		lipgloss.NewStyle(),
		lipgloss.NewStyle().Foreground(lipgloss.Color("#ff8800")),
		lipgloss.NewStyle().Bold(true).Italic(true),
		lipgloss.NewStyle().Background(lipgloss.Color("#202020")).Foreground(lipgloss.Color("#a0f0a0")),
	}
	var b strings.Builder
	for n := rng.Intn(14); n >= 0; n-- {
		if rng.Intn(12) == 0 {
			b.WriteString([]string{"\n", "\r\n"}[rng.Intn(2)])
		}
		b.WriteString(styles[rng.Intn(len(styles))].Render(bits[rng.Intn(len(bits))]))
	}
	return b.String()
}

// parityIndex lays rows out the way a paint might have: entries of random
// lengths, empty ones among them, each entry's rows split into their lines
// (physicalLines, as paint stores them), and the spans split between base and
// tail at random.
func parityIndex(rng *rand.Rand, rows []string) *rowIndex {
	var spans []rowSpan
	start := 0
	for i := 0; i < len(rows); {
		n := min(rng.Intn(5), len(rows)-i)
		lines := physicalLines(rows[i : i+n])
		spans = append(spans, rowSpan{rows: lines, start: start})
		i += n
		start += len(lines)
	}
	cut := 0
	if len(spans) > 0 {
		cut = rng.Intn(len(spans) + 1)
	}
	return &rowIndex{base: spans[:cut:cut], tail: spans[cut:], total: start}
}

// TestViewportMatchesBubbles is the viewport's parity (plan 032 §3.3 C5): the
// craze viewport and bubbles' v0.21.0, configured as craze configured it, are
// driven through the same seeded operations — content changes (bubbles is
// handed the rows joined, as craze handed them before C5, and craze the index
// of their lines; some rows hold a "\n" or a "\r\n"), resizes,
// scrolls, page keys (with and without alt), goto top and bottom, offsets set
// in and out of range — from a viewport never given content, and after every
// operation they draw the same View and stand at the same offset.
func TestViewportMatchesBubbles(t *testing.T) {
	const opsPerSeed = 1000
	for seed := int64(1); seed <= 8; seed++ {
		rng := rand.New(rand.NewSource(seed))
		bv := bubblesvp.New(0, 0)
		bv.KeyMap = crazeKeyMap()
		var cv viewport
		for op := 0; op < opsPerSeed; op++ {
			var did string
			switch k := rng.Intn(10); k {
			case 0, 1:
				rows := make([]string, rng.Intn(40))
				for i := range rows {
					rows[i] = parityRow(rng)
				}
				bv.SetContent(strings.Join(rows, "\n"))
				cv.setRows(parityIndex(rng, rows))
				did = fmt.Sprintf("content of %d rows", len(rows))
			case 2:
				w, h := rng.Intn(26), rng.Intn(14)
				bv.Width, bv.Height = w, h
				cv.Width, cv.Height = w, h
				did = fmt.Sprintf("resize %dx%d", w, h)
			case 3:
				n := []int{0, 1, 3, rng.Intn(30)}[rng.Intn(4)]
				bv.ScrollUp(n)
				cv.ScrollUp(n)
				did = fmt.Sprintf("scroll up %d", n)
			case 4:
				n := []int{0, 1, 3, rng.Intn(30)}[rng.Intn(4)]
				bv.ScrollDown(n)
				cv.ScrollDown(n)
				did = fmt.Sprintf("scroll down %d", n)
			case 5:
				msg := tea.KeyMsg{Type: []tea.KeyType{tea.KeyPgUp, tea.KeyPgDown}[rng.Intn(2)], Alt: rng.Intn(4) == 0}
				bv, _ = bv.Update(msg)
				cv, _ = cv.Update(msg)
				did = "key " + msg.String()
			case 6:
				bv.GotoTop()
				cv.GotoTop()
				did = "goto top"
			case 7:
				bv.GotoBottom()
				cv.GotoBottom()
				did = "goto bottom"
			default:
				n := rng.Intn(60) - 10
				bv.SetYOffset(n)
				cv.SetYOffset(n)
				did = fmt.Sprintf("set offset %d", n)
			}
			if bv.YOffset != cv.YOffset || bv.AtBottom() != cv.AtBottom() || bv.AtTop() != cv.AtTop() {
				t.Fatalf("seed %d op %d (%s): bubbles at %d (bottom %v, top %v), craze at %d (bottom %v, top %v)",
					seed, op, did, bv.YOffset, bv.AtBottom(), bv.AtTop(), cv.YOffset, cv.AtBottom(), cv.AtTop())
			}
			if want, got := bv.View(), cv.View(); got != want {
				t.Fatalf("seed %d op %d (%s): View differs\nbubbles: %q\ncraze:   %q", seed, op, did, want, got)
			}
		}
	}
}

// ------------------------------------------------------------- a pane script

// paneScript drives one model through a seeded sequence of the operations
// that change a pane — every way a row is added, changed or taken away, and
// every key change — painting after each as the Update wrapper does, and
// hands each step to check.
type paneScript struct {
	t     *testing.T
	rng   *rand.Rand
	m     Model
	tools []string
	shell int
}

// newPaneScript is a script over a fresh model, seeded.
//
// The script takes rows out of the pane directly — removeRow anywhere in the
// list, dropFirst at the front — which the fold only ever does for an entry the
// shared model dropped. That is the point (a row leaving from the middle is a
// shape change the paint must survive), and it is exactly what the parity
// watch refuses, so the watch is off for the script's models.
func newPaneScript(t *testing.T, seed int64) *paneScript {
	t.Helper()
	fh, rh := foldHook, restoreHook
	foldHook, restoreHook = nil, nil
	t.Cleanup(func() { foldHook, restoreHook = fh, rh })
	return &paneScript{t: t, rng: rand.New(rand.NewSource(seed)), m: sized(t)}
}

// scriptText is random agent text: prose, markdown blocks, tabs, wide runes.
func (s *paneScript) scriptText() string {
	bits := []string{
		"word ", "words in a row ", "more prose that wraps across the row ", "\n", "\n\n", "\t",
		"日本語 ", "🙂 ", "`code` ", "**bold** ", "- item\n", "1. first\n", "# Heading\n",
		"```go\nx := 1\n", "```\n", "| a | b |\n|---|---|\n| 1 | 2 |\n", "> quoted\n", "---\n",
	}
	var b strings.Builder
	for n := s.rng.Intn(4); n >= 0; n-- {
		b.WriteString(bits[s.rng.Intn(len(bits))])
	}
	return b.String()
}

func (s *paneScript) feed(ev agent.Event) {
	tm, _ := s.m.Update(eventMsg{ev: ev})
	s.m = tm.(Model)
}

// paint is the Update wrapper's half that paints, for an operation made
// straight on the pane.
func (s *paneScript) paint() {
	if s.m.cur().dirty {
		s.m.refreshViewport()
	}
}

// step makes one random operation and says what it was.
func (s *paneScript) step() string {
	m, rng, tr := &s.m, s.rng, s.m.main
	switch k := rng.Intn(20); k {
	case 0, 1, 2, 3:
		s.feed(agent.Event{Type: agent.EventText, Text: s.scriptText()})
		return "chunk"
	case 4:
		s.feed(agent.Event{Type: agent.EventThought, Text: s.scriptText()})
		return "thought"
	case 5:
		s.feed(agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
		return "done"
	case 6:
		id := fmt.Sprintf("tool-%d", len(s.tools))
		if len(s.tools) > 0 && rng.Intn(2) == 0 {
			id = s.tools[rng.Intn(len(s.tools))]
		} else {
			s.tools = append(s.tools, id)
		}
		kinds := []string{"read", "edit", "execute", "search"}
		dirs := []string{"a", "b", "c"}
		// A path holding a line break, as a location or as rawInput's decoded
		// path, is drawn inside one rendered row (toolRow); the paint splits
		// it into the lines bubbles showed (physicalLines).
		names := []string{"main.go", "main.go", "main.go", "ma\nin.go", "lf\r\ncr.go"}
		status := []string{"pending", "in_progress", "completed", "failed"}[rng.Intn(4)]
		tool := &agent.ToolEvent{
			ID: id, Kind: kinds[rng.Intn(len(kinds))], Status: status, Title: "tool " + id,
			Locations: []string{dirs[rng.Intn(len(dirs))] + "/" + names[rng.Intn(len(names))]},
		}
		if rng.Intn(4) == 0 {
			raw, _ := json.Marshal(map[string]string{"path": tool.Locations[0]})
			tool.Locations, tool.RawInput = nil, string(raw)
		}
		s.feed(agent.Event{Type: agent.EventTool, Tool: tool})
		return "tool " + id + " " + status
	case 7:
		s.feed(agent.Event{Type: agent.EventUser, Text: "a question\tfrom the user"})
		return "user"
	case 8:
		m.addNote("a local note " + s.scriptText())
		s.paint()
		return "local note"
	case 9:
		m.addError("a local failure")
		s.paint()
		return "local error"
	case 10:
		s.shell++
		m.addShell(s.shell, fmt.Sprintf("sleep %d", s.shell))
		s.paint()
		return "shell started"
	case 11:
		for _, e := range tr.rows {
			if e.shell != nil && !e.shell.done {
				e.text, e.shell.done, e.dirty = "out\nput", true, true
				tr.dirty = true
				break
			}
		}
		m.spinFrame++
		tr.dirty = true
		s.paint()
		return "shell settled, spinner moved"
	case 12:
		if len(tr.rows) > 0 {
			tr.removeRow(tr.rows[rng.Intn(len(tr.rows))])
		}
		s.paint()
		return "remove a row"
	case 13:
		tr.dropFirst(min(len(tr.rows), 1+rng.Intn(3)))
		s.paint()
		return "drop the first rows"
	case 14:
		if rng.Intn(3) == 0 {
			m.clearTranscript()
			s.paint()
			return "clear"
		}
		m.refreshViewport()
		return "paint with nothing changed"
	case 15:
		w, h := 20+rng.Intn(100), 10+rng.Intn(30)
		tm, _ := s.m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		s.m = tm.(Model)
		return fmt.Sprintf("resize %dx%d", w, h)
	case 16:
		names := ThemeNames()
		m.applyTheme(Preset(names[rng.Intn(len(names))]))
		return "theme " + m.theme.Name
	case 17:
		tm, _ := s.m.toggleExpanded()
		s.m = tm.(Model)
		return "ctrl+o"
	case 18:
		tm, _ := s.m.Update(tea.KeyMsg{Type: []tea.KeyType{tea.KeyPgUp, tea.KeyPgDown}[rng.Intn(2)]})
		s.m = tm.(Model)
		return "page"
	default:
		for n := rng.Intn(4); n >= 0; n-- {
			s.feed(agent.Event{Type: agent.EventText, Text: s.scriptText()})
		}
		return "chunks"
	}
}

// fullRebuild is the rows a paint from nothing makes of the pane as it stands:
// the trim note, then every entry's rows, assembled whole.
func fullRebuild(m *Model, tr *pane) []string {
	kept := tr.drawn
	tr.drawn = nil
	full := m.paint(tr)
	tr.drawn = kept
	return full.appendRows(nil, 0, full.total)
}

// logicalRows is what the paint before C5 joined and handed bubbles'
// SetContent: the trim note, then every entry's rows rendered afresh and not
// split into lines. Each entry is rendered from a copy with no markdown
// checkpoint, and the pane's work counters are put back, so the oracle shares
// no cache with the paint it checks: a cached render gone stale shows up as a
// View bubbles would not draw.
func logicalRows(m *Model, tr *pane) []string {
	var rows []string
	if tr.trimmed {
		rows = append(rows, renderSegs(m.width, seg{trimmedNote, styleFG(m.theme.Dim)}))
	}
	key, work := m.renderKey(), tr.work
	for _, e := range tr.rows {
		c := *e
		c.md = nil
		rows = append(rows, m.renderEntry(tr, &c, key)...)
	}
	tr.work = work
	return rows
}

// TestIncrementalAssemblyMatchesAFullRebuild is incremental assembly's
// equivalence (plan 032 §3.3 C5): after every step of seeded pane scripts —
// chunks, thoughts, tool rows and their updates, local rows, `!` rows running
// and settling, removals, the cap's front trim, /clear, resizes, themes, Ctrl+O
// and page keys — the rows the paint assembled from its first changed span
// are the rows a paint from nothing makes; the viewport over them draws what
// bubbles' viewport draws over the entries rendered afresh and joined
// (logicalRows: some tool rows hold a path with a "\n" or a "\r\n" in it,
// which bubbles split and the paint must have split the same way); and the
// index the paint before
// it made still reads exactly what it read (no paint writes over an index a
// Model copy may still hold).
func TestIncrementalAssemblyMatchesAFullRebuild(t *testing.T) {
	const steps = 400
	// The paints whose index is held and read again after every step. A paint
	// that wrote over a shared span would show it on an index one or more
	// paints back: cutting a base leaves the index before it whole, and the
	// append after the cut is what would overwrite the one before that.
	type held struct {
		idx  *rowIndex
		rows []string
	}
	const window = 16
	for seed := int64(1); seed <= 6; seed++ {
		s := newPaneScript(t, seed)
		var olds []held
		for i := 0; i < steps; i++ {
			did := s.step()
			m, tr := &s.m, s.m.main
			got := tr.drawnRows()
			if want := fullRebuild(m, tr); !slices.Equal(got, want) {
				t.Fatalf("seed %d step %d (%s): the assembled rows differ from a full rebuild\n got %d rows %q\nwant %d rows %q",
					seed, i, did, len(got), got, len(want), want)
			}
			// bubbles at the same offset — set, not scrolled to: a viewport made
			// taller while scrolled up stands past its bottom, as bubbles' did.
			bv := bubblesvp.New(m.vp.Width, m.vp.Height)
			bv.SetContent(strings.Join(logicalRows(m, tr), "\n"))
			bv.YOffset = m.vp.YOffset
			if bv.AtBottom() != m.vp.AtBottom() || bv.View() != m.vp.View() {
				t.Fatalf("seed %d step %d (%s): at offset %d the viewport draws\n%q\nbubbles over the same rows draws\n%q", seed, i, did, m.vp.YOffset, m.vp.View(), bv.View())
			}
			for k, o := range olds {
				if again := o.idx.appendRows(nil, 0, o.idx.total); !slices.Equal(again, o.rows) {
					t.Fatalf("seed %d step %d (%s): the index of %d paints back changed under it", seed, i, did, len(olds)-k)
				}
			}
			if n := len(olds); n == 0 || olds[n-1].idx != tr.drawn {
				olds = append(olds, held{tr.drawn, got})
				if len(olds) > window {
					olds = olds[1:]
				}
			}
		}
	}
}

// TestPlainRowsMatchTheEagerBuild is plain rows on demand (plan 032 §3.3 C5):
// after every step of the same seeded scripts, with a few rows asked for first
// so some entries hold their plain rows from an earlier paint, every drawn
// row's plain form is the eager build's — the row stripped of its styling and
// its trailing blanks — so an entry that rendered again never answers from
// the plain rows of its old render.
func TestPlainRowsMatchTheEagerBuild(t *testing.T) {
	const steps = 400
	for seed := int64(11); seed <= 16; seed++ {
		s := newPaneScript(t, seed)
		for i := 0; i < steps; i++ {
			did := s.step()
			tr := s.m.main
			if n := tr.drawnLen(); n > 0 {
				for k := s.rng.Intn(3); k >= 0; k-- {
					tr.plainRow(s.rng.Intn(n))
				}
			}
			rows := tr.drawnRows()
			got := tr.plainRows()
			for r, row := range rows {
				if want := strings.TrimRight(ansi.Strip(row), " "); got[r] != want {
					t.Fatalf("seed %d step %d (%s): plain row %d is %q, want %q", seed, i, did, r, got[r], want)
				}
			}
		}
	}
}

// TestPaintWatchCatchesAWrongIndex is the paint watch's own negative: an index
// that skips an entry, one that starts a span at the wrong row, and a row
// holding a line break are each refused.
func TestPaintWatchCatchesAWrongIndex(t *testing.T) {
	m := sized(t)
	m.addNote("one")
	m.addNote("two")
	m.refreshViewport()
	tr := m.main
	if err := checkPaint(&m, tr); err != nil {
		t.Fatalf("a true paint was refused: %v", err)
	}
	good := tr.drawn
	defer func() { tr.drawn = good }()

	spans := slices.Concat(good.base, good.tail)
	for _, tc := range []struct {
		name string
		idx  *rowIndex
	}{
		{"an entry skipped", &rowIndex{tail: spans[:1], total: len(spans[0].rows), key: good.key}},
		{"a span at the wrong row", &rowIndex{tail: []rowSpan{spans[0], {e: spans[1].e, rows: spans[1].rows, start: 0}}, total: good.total, key: good.key}},
	} {
		tr.drawn = tc.idx
		if err := checkPaint(&m, tr); err == nil {
			t.Fatalf("%s: the watch let it through", tc.name)
		}
	}
	e := tr.rows[1]
	broken := []string{"line\nbreak"}
	e.rendered = broken
	tr.drawn = &rowIndex{tail: []rowSpan{spans[0], {e: e, rows: broken, start: spans[1].start}}, total: spans[1].start + 1, key: good.key}
	if err := checkPaint(&m, tr); err == nil || !strings.Contains(err.Error(), "line break") {
		t.Fatalf("a row holding a line break: the watch said %v", err)
	}
}

// ------------------------------------------------------ rows with line breaks

// TestALineBreakInARowIsALineOfItsOwn: a value an agent sent can put a line
// break inside one rendered row — a read tool's location "/tmp/a\nb" survives
// sanitizeText, and toolRow draws "a\nb" as the target of one row — and
// bubbles' viewport, handed every row joined, split it into two lines. So must
// craze's: three notes and that tool at a height of three, stuck to the
// bottom, stand at offset 2 and show note2, the tool's head and "b", which is
// what bubbles draws over the same rows joined. The same holds for a path
// decoded from rawInput with a "\r\n" in it.
func TestALineBreakInARowIsALineOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool agent.ToolEvent
	}{
		{"a location holding \\n", agent.ToolEvent{
			ID: "r1", Kind: "read", Status: "completed", Title: "Read", Locations: []string{"/tmp/a\nb"},
		}},
		{"a rawInput path holding \\r\\n", agent.ToolEvent{
			ID: "r1", Kind: "read", Status: "completed", Title: "Read", RawInput: `{"path":"/tmp/a\r\nb"}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(t)
			for i := range 3 {
				m.addNote(fmt.Sprintf("note%d", i))
			}
			tool := tc.tool
			tm, _ := m.Update(eventMsg{ev: agent.Event{Type: agent.EventTool, Tool: &tool}})
			m = tm.(Model)
			m.vp.Height = 3
			m.refreshViewport()
			m.vp.GotoBottom()

			var view []string
			for _, ln := range strings.Split(plain(m.vp.View()), "\n") {
				view = append(view, strings.TrimRight(ln, " "))
			}
			if want := []string{"note2", "✓ read  a", "b"}; m.vp.YOffset != 2 || !m.vp.AtBottom() || !slices.Equal(view, want) {
				t.Fatalf("at offset %d (bottom %v) the viewport shows %q, want offset 2 and %q", m.vp.YOffset, m.vp.AtBottom(), view, want)
			}
			if n := m.main.drawnLen(); n != 5 {
				t.Fatalf("the transcript is %d lines, want 5 (three notes, and the tool's two): %q", n, m.main.plainRows())
			}
			bv := bubblesvp.New(m.vp.Width, m.vp.Height)
			bv.SetContent(strings.Join(logicalRows(&m, m.main), "\n"))
			bv.GotoBottom()
			if bv.YOffset != m.vp.YOffset || bv.View() != m.vp.View() {
				t.Fatalf("bubbles over the same rows stands at %d and draws\n%q\ncraze stands at %d and draws\n%q", bv.YOffset, bv.View(), m.vp.YOffset, m.vp.View())
			}
		})
	}
}

// TestPhysicalLinesAreBubblesLines is physicalLines against the split it
// stands in for: rows holding "\n", "\r\n", a break at either end and nothing
// but a break, laid out as two entries, draw in craze's viewport what bubbles
// draws over the rows joined, at every offset; and rows with no break come
// back as the same slice, with nothing allocated.
func TestPhysicalLinesAreBubblesLines(t *testing.T) {
	first := []string{"plain", "a\r\nb", "c\nd\n", "\r\n", "cr\r", "crcr\r\r", "lf-cr\n\r"}
	second := []string{"", "\ne\n\nf", "x\r\n\r\ny", "ends\r", "\nstarts", "tail"}
	lines1, lines2 := physicalLines(first), physicalLines(second)
	idx := &rowIndex{
		base:  []rowSpan{{rows: lines1}},
		tail:  []rowSpan{{rows: lines2, start: len(lines1)}},
		total: len(lines1) + len(lines2),
	}
	bv := bubblesvp.New(12, 3)
	bv.SetContent(strings.Join(slices.Concat(first, second), "\n"))
	cv := viewport{Width: 12, Height: 3}
	cv.setRows(idx)
	bv.GotoBottom()
	cv.GotoBottom()
	if bv.YOffset != cv.YOffset {
		t.Fatalf("at the bottom bubbles stands at %d, craze at %d", bv.YOffset, cv.YOffset)
	}
	// The bottom's offset is taken before the walk moves it (r12 7): read in
	// the loop's condition, it was 0 after the first step.
	bottom := bv.YOffset
	if bottom == 0 {
		t.Fatal("fixture: the rows fit the viewport, so there is no offset to walk")
	}
	for off := 0; off <= bottom; off++ {
		bv.SetYOffset(off)
		cv.SetYOffset(off)
		if want, got := bv.View(), cv.View(); got != want {
			t.Fatalf("at offset %d bubbles draws %q, craze %q", off, want, got)
		}
	}
	for _, row := range slices.Concat(lines1, lines2) {
		if strings.Contains(row, "\n") {
			t.Fatalf("a line still holds a break: %q", row)
		}
	}
	// A row ending in "\r" is cut back as the join cut it (review r8):
	// "cr\r" + the join's "\n" was "cr\r\n", which bubbles made "cr".
	if want := []string{"cr"}; !slices.Equal(physicalLines([]string{"cr\r"}), want) {
		t.Fatalf("a row ending in \\r: got %q, want %q", physicalLines([]string{"cr\r"}), want)
	}

	clean := []string{"one", "two \x1b[1mthree\x1b[0m", ""}
	if got := physicalLines(clean); !sameRows(got, clean) {
		t.Fatalf("rows with no break came back as another slice: %q", got)
	}
	if n := testing.AllocsPerRun(100, func() { _ = physicalLines(clean) }); n != 0 {
		t.Fatalf("rows with no break cost %v allocations, want 0", n)
	}
}

// TestSelectingARowWithALineBreakCopiesItsLines: the selection reads the lines
// the viewport shows. A drag from the start of the tool's head down onto the
// "b" its location broke onto copies the two lines; before C5 the selection
// read the rows (the tool's one row) while the viewport showed the lines, so
// the press and the copy could disagree.
func TestSelectingARowWithALineBreakCopiesItsLines(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	rec := captureCopies(t)
	m := selModel(t, &now, "filler")
	tool := agent.ToolEvent{ID: "r1", Kind: "read", Status: "completed", Title: "Read", Locations: []string{"/tmp/a\nb"}}
	tm, _ := m.Update(eventMsg{ev: agent.Event{Type: agent.EventTool, Tool: &tool}})
	m = tm.(Model)

	rows := m.main.plainRows()
	at := slices.IndexFunc(rows, func(r string) bool { return strings.HasPrefix(r, "✓ read") })
	if at < 0 {
		t.Fatalf("no tool row: %q", rows)
	}
	y := m.lay.Region(regionTranscript).Top + at - m.vp.YOffset
	m = drag(t, m, 0, y, 0, y+1)

	const want = "✓ read  a\nb"
	if copies := rec.copies(); len(copies) != 1 || copies[0] != want {
		t.Fatalf("clipboard got %q, want [%q]", copies, want)
	}
}
