package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/sessions"
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
	// A saved row comes from the index, which records no model: its model
	// column's cells — the 20 after the 8-cell provider — are blank, where the
	// running row's carry the model.
	saved := sessSavedRow(sessions.Row{CrazeID: "c1", Provider: "native", SessionID: "s1", Title: "t", CWD: "/w"})
	if saved.model != "" {
		t.Fatalf("sessSavedRow model %q", saved.model)
	}
	m, _, _ := sessModel(t, 100, 30)
	modelCells := func(line string) string {
		r := []rune(plain(line))
		at := strings.Index(string(r), "native")
		if at < 0 {
			t.Fatalf("no provider in %q", string(r))
		}
		at = len([]rune(string(r)[:at])) + 8
		return string(r[at : at+sessModelCols])
	}
	if got := modelCells(m.sessRowView(row, false)); !strings.HasPrefix(got, "anthropic/claude-") {
		t.Fatalf("the running row's model cells are %q", got)
	}
	if got := modelCells(m.sessRowView(saved, false)); strings.TrimSpace(got) != "" {
		t.Fatalf("a saved row's model cells are %q, want blank", got)
	}
}

// TestTheSelectedRowOutlivesTheMoreMarkers (plan 046 C2 review): a wrapped
// note can leave the list two rows at 40x10, and the window there is the
// selected row and one neighbour. The `↑ more above` and `↓ more below`
// markers take an edge row each, so they must not take the selected one.
func TestTheSelectedRowOutlivesTheMoreMarkers(t *testing.T) {
	m, _, _ := sessModel(t, 40, 10)
	var lines []sessLine
	for i := range 12 {
		lines = append(lines, sessLine{kind: sessLineRow, row: sessRow{
			key: sessKey{id: fmt.Sprintf("r%02d", i)}, title: fmt.Sprintf("row-%02d", i), provider: "native",
		}})
	}
	for _, sel := range []int{0, 1, 5, 10, 11} {
		m.sessList.sel = lines[sel].row.key
		for _, n := range []int{2, 3, 4} {
			out := m.sessBody(lines, n)
			want := fmt.Sprintf("row-%02d", sel)
			found := false
			for _, l := range out {
				found = found || strings.Contains(plain(l), want)
			}
			if !found {
				t.Fatalf("sel %d, %d rows: the selected row is not shown: %q", sel, n, out)
			}
		}
	}
}
