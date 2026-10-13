package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/roster"
)

// TestSessionsListModelColumn (plan 046 A1): a running row's model is a 20-cell
// column after the provider from 100 columns, clipped with an ellipsis; a
// saved row draws it blank; below 100 columns there is no column at all.
func TestSessionsListModelColumn(t *testing.T) {
	const long = "anthropic/claude-opus-4.8-thinking-max"
	row := sessRunningRow(answered("a", "t", "native", "/w", 0, func(s *roster.Session) { s.Model = long }), sessKey{})
	if row.model != long {
		t.Fatalf("sessRunningRow model %q", row.model)
	}
	for _, c := range []struct {
		cols int
		show bool
	}{{100, true}, {99, false}, {80, false}} {
		m, _, _ := sessModel(t, c.cols, 30)
		line := m.sessRowView(row, false)
		plain := plain(line)
		clipped := strings.TrimSpace(plain[strings.Index(plain, "native")+len("native"):])
		has := strings.Contains(plain, "anthropic/claude-")
		if has != c.show {
			t.Fatalf("%d cols: model shown %v, want %v: %q", c.cols, has, c.show, plain)
		}
		if c.show && !strings.Contains(plain, "anthropic/claude-o…") {
			t.Fatalf("%d cols: the model is not clipped with …: %q (%q)", c.cols, plain, clipped)
		}
		if w := lipgloss.Width(plain); w != c.cols {
			t.Fatalf("%d cols: row is %d cells wide", c.cols, w)
		}
	}
	saved := sessRow{saved: true, provider: "native", title: "t"}
	m, _, _ := sessModel(t, 100, 30)
	if got := plain(m.sessRowView(saved, false)); strings.Contains(got, "…") && strings.Contains(got, "claude") {
		t.Fatalf("a saved row shows a model: %q", got)
	}
}
