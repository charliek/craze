package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

func TestActivityGroupsFollowUpdatesAndLocalDisclosure(t *testing.T) {
	m := sized(t)
	call := func(id, kind, status string) {
		m = toolEvent(m, &agent.ToolEvent{ID: id, Kind: kind, Status: status, Title: id, Locations: []string{id}})
	}
	call("a", "read", "completed")
	call("b", "search", "in_progress")
	call("c", "read", "completed")
	requireRows := func(want ...string) {
		t.Helper()
		got := m.main.plainRows()
		if !slices.Equal(got, want) {
			t.Fatalf("rows: %q, want %q", got, want)
		}
		fresh := fullRebuild(&m, m.main)
		if !slices.Equal(fresh, m.main.drawnRows()) {
			t.Fatalf("incremental differs from full paint: %q", fresh)
		}
	}
	call("b", "search", "completed")
	requireRows("▸ Explored · 2 reads, 1 search")
	folded := m.main.drawn
	head := m.main.rows[0]
	head.detailOpen, head.dirty = true, true
	m.refreshViewport()
	requireRows("▾ Explored · 2 reads, 1 search", "✓ read  a", "✓ search  b", "✓ read  c")
	if got := plain(folded.appendRows(nil, 0, folded.total)[0]); !strings.HasPrefix(got, "▸") {
		t.Fatalf("previous index changed: %q", got)
	}
	call("b", "search", "failed")
	requireRows("✓ read  a", "✗ search  b", "✓ read  c")
	call("b", "search", "completed")
	m = toolEvent(m, &agent.ToolEvent{ID: "d", Kind: "edit", Status: "completed", Title: "d", Edited: &agent.ToolEdited{Files: []string{"d"}}})
	m.transcriptLayout = layoutCompact
	m.refreshViewport()
	requireRows("▾ Activity · 2 reads, 1 search, 1 edit", "✓ read  a", "✓ search  b", "✓ read  c", "✓ edit  d")
	m.transcriptLayout = layoutClassic
	m.refreshViewport()
	requireRows("✓ read  a", "✓ search  b", "✓ read  c", "✓ edit  d")
}

func TestActivityGroupAnchorsScrolledHistory(t *testing.T) {
	m := sized(t)
	for _, id := range []string{"a", "b", "c"} {
		m = toolEvent(m, &agent.ToolEvent{ID: id, Kind: "read", Status: "completed", Locations: []string{id}})
	}
	for _, msg := range []string{"one", "two", "three", "four", "five", "six"} {
		m.addNote(msg)
	}
	m.refreshViewport()
	m.vp.Height = 3
	m.vp.SetYOffset(2)
	before := m.main.plainRow(m.vp.YOffset)
	head := m.main.rows[0]
	head.detailOpen, head.dirty = true, true
	m.setViewportContent(false)
	if after := m.main.plainRow(m.vp.YOffset); after != before {
		t.Fatalf("anchor moved from %q to %q", before, after)
	}
	m.vp.SetYOffset(2) // a group member
	head.detailOpen, head.dirty = false, true
	m.setViewportContent(false)
	if m.vp.YOffset != 0 {
		t.Fatalf("hidden member anchored to %d, want header", m.vp.YOffset)
	}
}

func TestFailedCommandPreviewsStdoutWhenStderrEmpty(t *testing.T) {
	m := sized(t)
	exit := 1
	m = toolEvent(m, &agent.ToolEvent{ID: "cmd", Kind: "execute", Status: "failed", Title: "test", Output: &agent.ToolOutput{ExitCode: &exit, StdoutHead: "failing test\nother"}})
	if got := strings.Join(m.main.plainRows(), "\n"); !strings.Contains(got, "exit 1") || !strings.Contains(got, "failing test") || strings.Contains(got, "other") {
		t.Errorf("unexpected diagnostic %q", got)
	}
}
