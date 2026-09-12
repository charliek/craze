package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
)

// The chips are the anchors the view tests use for the status rows.
const (
	chipYolo      = "▸▸ bypass permissions on"
	chipPrompt    = "▸ prompting for permissions"
	modeChipAgent = "◆ agent"
)

// statusFixture is §3.8's own example row, laid out so the pinned drop order
// is visible one segment at a time as the terminal narrows.
func statusFixture(t *testing.T) Model {
	t.Helper()
	isolateSkillsHome(t)
	now := time.Date(2026, 9, 12, 10, 12, 0, 0, time.UTC)
	m := New(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Yolo:      true,
	})
	m.clock = func() time.Time { return now }
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)

	m.cwd = "/home/dev/craze"
	m.branch = "feature/status-rows"
	m.sessStart = now.Add(-12 * time.Minute)
	m.snap.Models = []agent.ModelInfo{{ID: "grok-4.6", Name: "Cursor Grok 4.6"}}
	m.snap.CurrentModel = "grok-4.6"
	m.snap.CurrentMode = "agent"
	for i := range m.snap.Config {
		if m.snap.Config[i].ID == "effort" {
			m.snap.Config[i].Current = "high"
		}
	}
	return m
}

func TestStatusRow1DropsFromTheRight(t *testing.T) {
	m := statusFixture(t)
	for _, tc := range []struct {
		cols int
		want string
	}{
		{120, "craze │ feature/status-rows │ cursor │ Cursor Grok 4.6 (high) │ 12m"},
		{80, "craze │ feature/status-rows │ cursor │ Cursor Grok 4.6 (high) │ 12m"},
		// 65 drops the elapsed first, 60 the branch after it.
		{65, "craze │ feature/status-rows │ cursor │ Cursor Grok 4.6 (high)"},
		{60, "craze │ cursor │ Cursor Grok 4.6 (high)"},
		// then the provider, and the model goes last.
		{30, "craze │ Cursor Grok 4.6 (high)"},
		{20, "craze"},
	} {
		m.width = tc.cols
		got := statusText(m.statusRow1())
		if got != tc.want {
			t.Fatalf("%d cols:\n got %q\nwant %q", tc.cols, got, tc.want)
		}
		if strings.Contains(got, modeChipAgent) {
			t.Fatalf("%d cols: the mode belongs to row 2 alone: %q", tc.cols, got)
		}
	}
}

func TestStatusRow1PreStart(t *testing.T) {
	isolateSkillsHome(t)
	m := New(Config{Session: NewStub(), Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	m.cwd = "/home/dev/craze"
	if got, want := statusText(m.statusRow1()), "craze │ starting…"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestStatusChipByForceFlag(t *testing.T) {
	m := statusFixture(t)
	text, style := m.permissionChip()
	if text != chipYolo {
		t.Fatalf("yolo chip %q", text)
	}
	if got := style.GetForeground(); got != m.theme.ChipBypass {
		t.Fatalf("bypass chip is %v, want the chip-bypass colour %v", got, m.theme.ChipBypass)
	}

	m.yolo = false
	text, style = m.permissionChip()
	if text != chipPrompt {
		t.Fatalf("prompting chip %q", text)
	}
	if got := style.GetForeground(); got != m.theme.ChipPrompt {
		t.Fatalf("prompting chip is %v, want the chip-prompt colour %v", got, m.theme.ChipPrompt)
	}
}

func TestStatusRow2CountsInFlightWorkByKind(t *testing.T) {
	m := statusFixture(t)
	m.snap.Tools = []agent.ToolEvent{
		{ID: "sh-1", Kind: "execute", Status: "in_progress"},
		{ID: "rd-1", Kind: "read", Status: "in_progress"},
		{ID: "rd-2", Kind: "read", Status: "pending"},
		{ID: "ed-1", Kind: "edit", Status: "pending"},
		{ID: "sh-2", Kind: "execute", Status: "completed"},
		{ID: "td-1", Kind: "other", ToolName: "updateTodos", Title: "Update TODOs", Status: "in_progress"},
		{ID: "tk-1", Kind: "other", ToolName: "task", Title: "Task: research", Status: "in_progress"},
	}
	if got, want := m.inFlightCounts(), "1 shell, 2 reads, 1 edit"; got != want {
		t.Fatalf("counts %q, want %q", got, want)
	}
	if got, want := m.agentCount(), "← 1 agent"; got != want {
		t.Fatalf("agents %q, want %q", got, want)
	}
	row := statusText(m.statusRow2(m.lay))
	if !strings.HasPrefix(row, modeChipAgent+statusDot+modeHint+statusDot+chipYolo) ||
		!strings.Contains(row, "1 shell, 2 reads, 1 edit") || !strings.HasSuffix(row, "← 1 agent") {
		t.Fatalf("row 2 %q", row)
	}
}

// TestStatusRow2DropOrder is the pinned order the row gives ground in: the
// hint first, then the agent count, then the counts. Neither chip ever goes.
func TestStatusRow2DropOrder(t *testing.T) {
	m := statusFixture(t)
	m.snap.Tools = []agent.ToolEvent{
		{ID: "sh-1", Kind: "execute", Status: "in_progress"},
		{ID: "tk-1", Kind: "other", ToolName: "task", Title: "Task: research", Status: "in_progress"},
	}
	chip := modeChipAgent + statusDot + chipYolo
	for _, tc := range []struct {
		cols int
		want string
	}{
		// The whole row is 68 cells, so 67 is the first width with no room
		// for the hint — it costs its separator too.
		{100, modeChipAgent + statusDot + modeHint + statusDot + chipYolo + statusDot + "1 shell" + statusDot + "← 1 agent"},
		{80, modeChipAgent + statusDot + modeHint + statusDot + chipYolo + statusDot + "1 shell" + statusDot + "← 1 agent"},
		{67, modeChipAgent + statusDot + chipYolo + statusDot + "1 shell" + statusDot + "← 1 agent"},
		{50, modeChipAgent + statusDot + chipYolo + statusDot + "1 shell"},
		{40, chip},
		// Below both chips the permission chip truncates, never the mode.
		{30, modeChipAgent + statusDot + "▸▸ bypass permissio…"},
	} {
		m.width = tc.cols
		if got := statusText(m.statusRow2(m.lay)); got != tc.want {
			t.Fatalf("%d cols:\n got %q\nwant %q", tc.cols, got, tc.want)
		}
	}

	// The merged spinner is worth more than any of them, so every width loses
	// one more segment with it on.
	merged := frameLayout{SpinnerMerged: true}
	m.status = statusWorking
	m.turnStart = m.now().Add(-14 * time.Second)
	spin := m.spinnerGlyph() + " 14s"
	for _, tc := range []struct {
		cols int
		want string
	}{
		{100, spin + statusDot + modeChipAgent + statusDot + modeHint + statusDot + chipYolo + statusDot + "1 shell" + statusDot + "← 1 agent"},
		{80, spin + statusDot + modeChipAgent + statusDot + modeHint + statusDot + chipYolo + statusDot + "1 shell" + statusDot + "← 1 agent"},
		{70, spin + statusDot + modeChipAgent + statusDot + chipYolo + statusDot + "1 shell" + statusDot + "← 1 agent"},
		{60, spin + statusDot + modeChipAgent + statusDot + chipYolo + statusDot + "1 shell"},
		{45, spin + statusDot + modeChipAgent + statusDot + chipYolo},
		{40, spin + statusDot + modeChipAgent + statusDot + "▸▸ bypass permissions…"},
	} {
		m.width = tc.cols
		if got := statusText(m.statusRow2(merged)); got != tc.want {
			t.Fatalf("%d cols merged:\n got %q\nwant %q", tc.cols, got, tc.want)
		}
	}
}

// TestModeChipColourPerKind: the chip is coloured by what the mode means, not
// by its id, so an agent that spells plan mode "architect" still reads as plan.
func TestModeChipColourPerKind(t *testing.T) {
	m := statusFixture(t)
	for _, tc := range []struct {
		mode  string
		want  lipgloss.Color
		which string
	}{
		{"agent", m.theme.ModeImplement, "implement"},
		{"plan", m.theme.ModePlan, "plan"},
		{"ask", m.theme.ModeReadOnly, "read-only"},
		{"architect", m.theme.ModePlan, "plan (alias)"},
		{"wat", m.theme.Dim, "unknown"},
	} {
		m.snap.CurrentMode = tc.mode
		text, style := m.modeChip()
		if text != "◆ "+tc.mode {
			t.Fatalf("%s chip text %q", tc.mode, text)
		}
		if got := style.GetForeground(); got != tc.want {
			t.Fatalf("%s (%s) chip is %v, want %v", tc.mode, tc.which, got, tc.want)
		}
		row, _ := m.statusRow2(m.lay)
		if !strings.Contains(row, ansiFG(string(tc.want))+text) {
			t.Fatalf("%s chip is not drawn in %s:\n%q", tc.mode, tc.want, row)
		}
		if !strings.Contains(plainView(m), text) {
			t.Fatalf("%s chip missing from the frame:\n%s", tc.mode, plainView(m))
		}
	}
}

// TestStatusSpansMatchTheRenderedRow holds the span contract: a span is where
// the fitting pass actually drew the segment, so it moves with every drop and
// every separator before it.
func TestStatusSpansMatchTheRenderedRow(t *testing.T) {
	m := statusFixture(t)
	m.width = 120
	row, spans := m.statusRow2(m.lay)
	mode := spanRange(t, spans, spanMode)
	if mode.x0 != 0 {
		t.Fatalf("the chip leads row 2, got x0 %d", mode.x0)
	}
	if got := cells(plain(row), mode); got != modeChipAgent {
		t.Fatalf("the mode span covers %q", got)
	}
	if spanAt(spans, mode.x0) != spanMode || spanAt(spans, mode.x1-1) != spanMode {
		t.Fatal("both ends of the span are inside it")
	}
	if spanAt(spans, mode.x1) != spanNone || spanAt(spans, mode.x0-1) != spanNone {
		t.Fatal("the separator either side is not the chip")
	}

	// The merged spinner pushes the chip along, and the span follows it.
	m.status = statusWorking
	m.turnStart = m.now().Add(-14 * time.Second)
	row, spans = m.statusRow2(frameLayout{SpinnerMerged: true})
	mode = spanRange(t, spans, spanMode)
	if mode.x0 != lipgloss.Width(m.spinnerGlyph()+" 14s"+statusDot) {
		t.Fatalf("the chip should start after the spinner, got x0 %d in %q", mode.x0, plain(row))
	}
	if got := cells(plain(row), mode); got != modeChipAgent {
		t.Fatalf("the moved span covers %q", got)
	}

	// Row 1 records the model span for V3's dialog; it moves when the
	// branch before it drops, and goes away with the segment itself.
	m.width = 120
	row1, spans1 := m.statusRow1()
	wide := spanRange(t, spans1, spanModel)
	if got := cells(plain(row1), wide); got != "Cursor Grok 4.6 (high)" {
		t.Fatalf("the model span covers %q", got)
	}
	m.width = 60
	row1, spans1 = m.statusRow1()
	narrow := spanRange(t, spans1, spanModel)
	if narrow.x0 >= wide.x0 {
		t.Fatalf("dropping the branch should move the model left: %d then %d", wide.x0, narrow.x0)
	}
	if got := cells(plain(row1), narrow); got != "Cursor Grok 4.6 (high)" {
		t.Fatalf("the moved model span covers %q", got)
	}
	m.width = 20
	if _, spans1 = m.statusRow1(); spanAt(spans1, 5) != spanNone {
		t.Fatal("the model dropped, so nothing on row 1 is clickable")
	}
}

// TestRenderSegSpansTruncates: the last segment on a full row is clamped, and
// its span is the cells it actually got, not the cells it asked for.
func TestRenderSegSpansTruncates(t *testing.T) {
	st := lipgloss.NewStyle()
	row, spans := renderSegSpans(12, []idSeg{
		{seg: seg{"◆ plan", st}, id: spanMode},
		{seg: seg{statusDot, st}},
		{seg: seg{"a long tail", st}, id: spanModel},
	})
	if got, want := plain(row), "◆ plan · a …"; got != want {
		t.Fatalf("row %q, want %q", got, want)
	}
	mode, model := spanRange(t, spans, spanMode), spanRange(t, spans, spanModel)
	if mode != (segSpan{id: spanMode, x0: 0, x1: 6}) {
		t.Fatalf("mode span %+v", mode)
	}
	if model != (segSpan{id: spanModel, x0: 9, x1: 12}) {
		t.Fatalf("a clamped span ends at the row edge: %+v", model)
	}
}

// cells is the display cells a span covers, which is not a byte slice: the
// chip and the separators are multi-byte.
func cells(row string, s segSpan) string { return ansi.Cut(row, s.x0, s.x1) }

func spanRange(t *testing.T, spans []segSpan, id spanID) segSpan {
	t.Helper()
	for _, s := range spans {
		if s.id == id {
			return s
		}
	}
	t.Fatalf("no span %v in %+v", id, spans)
	return segSpan{}
}

func TestStatusRow2CarriesTheMergedSpinner(t *testing.T) {
	m := statusFixture(t)
	m.status = statusWorking
	m.turnStart = m.now().Add(-14 * time.Second)
	unmerged := statusText(m.statusRow2(m.lay))
	if strings.Contains(unmerged, "14s") {
		t.Fatalf("an unmerged row 2 has no spinner: %q", unmerged)
	}
	merged := statusText(m.statusRow2(frameLayout{SpinnerMerged: true}))
	if !strings.HasPrefix(merged, m.spinnerGlyph()+" 14s"+statusDot+modeChipAgent) {
		t.Fatalf("degradation step 6 leaves %q in row 2, got %q", "✳ 14s", merged)
	}
}

func TestSessionElapsedFormat(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0m"},
		{59 * time.Second, "0m"},
		{12 * time.Minute, "12m"},
		{2*time.Hour + 14*time.Minute, "2h14m"},
		{2*time.Hour + 4*time.Minute, "2h04m"},
	} {
		if got := formatCoarse(tc.d); got != tc.want {
			t.Fatalf("%s -> %q, want %q", tc.d, got, tc.want)
		}
	}
}
