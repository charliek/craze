package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// The two chips are the anchors the view tests use for the status rows.
const (
	chipYolo   = "▸▸ bypass permissions on"
	chipPrompt = "▸ prompting for permissions"
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
		{120, "craze │ feature/status-rows │ cursor │ Cursor Grok 4.6 (high) │ agent │ 12m"},
		{80, "craze │ feature/status-rows │ cursor │ Cursor Grok 4.6 (high) │ agent │ 12m"},
		// 60 drops the elapsed first, then the branch.
		{60, "craze │ cursor │ Cursor Grok 4.6 (high) │ agent"},
		// then the mode, then the provider, and the model goes last.
		{40, "craze │ cursor │ Cursor Grok 4.6 (high)"},
		{30, "craze │ Cursor Grok 4.6 (high)"},
		{20, "craze"},
	} {
		m.width = tc.cols
		if got := plain(m.statusRow1()); got != tc.want {
			t.Fatalf("%d cols:\n got %q\nwant %q", tc.cols, got, tc.want)
		}
	}
}

func TestStatusRow1PreStart(t *testing.T) {
	isolateSkillsHome(t)
	m := New(Config{Session: NewStub(), Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	m.cwd = "/home/dev/craze"
	if got, want := plain(m.statusRow1()), "craze │ starting…"; got != want {
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
	row := plain(m.statusRow2(m.lay))
	if !strings.HasPrefix(row, chipYolo) || !strings.Contains(row, "1 shell, 2 reads, 1 edit") ||
		!strings.HasSuffix(row, "← 1 agent") {
		t.Fatalf("row 2 %q", row)
	}

	// Narrow: the agent count goes first, then the counts, and the chip stays.
	m.width = 55
	if got := plain(m.statusRow2(m.lay)); got != chipYolo+statusDot+"1 shell, 2 reads, 1 edit" {
		t.Fatalf("55 cols dropped the wrong segment: %q", got)
	}
	m.width = 30
	if got := plain(m.statusRow2(m.lay)); got != chipYolo {
		t.Fatalf("30 cols should leave the chip alone: %q", got)
	}
	m.width = 10
	if got := plain(m.statusRow2(m.lay)); got != "▸▸ bypass…" {
		t.Fatalf("the chip truncates rather than disappearing: %q", got)
	}
}

func TestStatusRow2CarriesTheMergedSpinner(t *testing.T) {
	m := statusFixture(t)
	m.status = statusWorking
	m.turnStart = m.now().Add(-14 * time.Second)
	unmerged := plain(m.statusRow2(m.lay))
	if strings.Contains(unmerged, "14s") {
		t.Fatalf("an unmerged row 2 has no spinner: %q", unmerged)
	}
	merged := plain(m.statusRow2(frameLayout{SpinnerMerged: true}))
	if !strings.HasPrefix(merged, m.spinnerGlyph()+" 14s"+statusDot) {
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
