package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func mdLines(t *testing.T, text string, width int) []string {
	t.Helper()
	out := renderMarkdown(text, width, Preset("tokyo-night"))
	for i, ln := range out {
		if w := lipgloss.Width(ln); w > width {
			t.Fatalf("line is %d wide, limit is %d: %q", w, width, ln)
		}
		out[i] = plain(ln)
	}
	return out
}

func TestMarkdownBlocks(t *testing.T) {
	got := mdLines(t, strings.Join([]string{
		"## Heading",
		"",
		"- first",
		"  - nested",
		"1. one",
		"",
		"> quoted",
		"",
		"---",
	}, "\n"), 40)
	want := []string{
		"Heading",
		"",
		"• first",
		"  • nested",
		"1. one",
		"",
		"│ quoted",
		"",
		strings.Repeat("─", 38),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d:\n%q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMarkdownInlineMarkers(t *testing.T) {
	got := strings.Join(mdLines(t, "Inline `code` and **bold** and _italic_ and *also*.", 60), "\n")
	for _, want := range []string{"code", "bold", "italic", "also"} {
		if !strings.Contains(got, want) {
			t.Fatalf("lost %q in %q", want, got)
		}
	}
	if strings.ContainsAny(got, "`*_") {
		t.Fatalf("markers survived: %q", got)
	}
}

func TestMarkdownUnmatchedMarkersStayLiteral(t *testing.T) {
	got := strings.Join(mdLines(t, "a * b and `open and snake_case_name", 60), "\n")
	if !strings.Contains(got, "a * b") {
		t.Fatalf("lone asterisk: %q", got)
	}
	if !strings.Contains(got, "`open") {
		t.Fatalf("lone backtick: %q", got)
	}
	if !strings.Contains(got, "snake_case_name") {
		t.Fatalf("underscores inside a word: %q", got)
	}
}

func TestMarkdownFenceIsClippedNotWrapped(t *testing.T) {
	long := strings.Repeat("z", 200)
	got := mdLines(t, "```go\nfunc main() {\n\t"+long+"\n}\n```", 40)
	if len(got) != 4 {
		t.Fatalf("want a label and three code rows, got %q", got)
	}
	if got[0] != "  │ go" {
		t.Fatalf("language label %q", got[0])
	}
	if !strings.HasPrefix(got[1], "  │ func main() {") {
		t.Fatalf("code row %q", got[1])
	}
	if !strings.HasSuffix(got[2], "…") {
		t.Fatalf("a long code line must be clipped, got %q", got[2])
	}
	if strings.Contains(got[2], "\t") {
		t.Fatalf("tabs must be expanded: %q", got[2])
	}
}

func TestMarkdownFenceInsideAListItem(t *testing.T) {
	got := mdLines(t, strings.Join([]string{
		"- ```go",
		"  code()",
		"  ```",
		"after",
	}, "\n"), 40)
	want := []string{"•", "  │ go", "  │   code()", "after"}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMarkdownIndentedFenceInsideAListItem(t *testing.T) {
	got := mdLines(t, strings.Join([]string{
		"- item",
		"  ```",
		"  code()",
		"  ```",
		"after",
	}, "\n"), 40)
	want := []string{"• item", "  │   code()", "after"}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMarkdownItalicNeverSpansALineBreak(t *testing.T) {
	got := strings.Join(mdLines(t, "*foo\nbar* and *one line*", 60), "\n")
	if !strings.Contains(got, "*foo bar*") {
		t.Fatalf("a pair split across lines must stay literal: %q", got)
	}
	if strings.Contains(got, "*one line*") {
		t.Fatalf("a pair on one line should still style: %q", got)
	}
}

func TestMarkdownTripleMarkerIsBoldItalic(t *testing.T) {
	got := strings.Join(mdLines(t, "a ***loud*** b", 60), "\n")
	if got != "a loud b" {
		t.Fatalf("triple markers left debris: %q", got)
	}
}

func TestMarkdownUnterminatedFenceStillRenders(t *testing.T) {
	got := mdLines(t, "```\nstill streaming", 40)
	if len(got) != 1 || got[0] != "  │ still streaming" {
		t.Fatalf("open fence %q", got)
	}
}

func TestMarkdownWrapsProseAndBreaksLongTokens(t *testing.T) {
	token := strings.Repeat("q", 150)
	got := mdLines(t, "lead "+token+" tail", 40)
	if len(got) < 4 {
		t.Fatalf("a 150-char token must break across rows: %q", got)
	}
	joined := strings.Join(got, "")
	if strings.Count(joined, "q") != 150 {
		t.Fatalf("wrapping lost characters: %q", got)
	}
	if !strings.Contains(joined, "tail") {
		t.Fatalf("wrapping lost the tail: %q", got)
	}
}

func TestMarkdownCapsWidthOnWideTerminals(t *testing.T) {
	got := mdLines(t, strings.Repeat("word ", 80), 200)
	for _, ln := range got {
		if w := lipgloss.Width(ln); w > proseMaxWidth {
			t.Fatalf("prose is %d wide, cap is %d", w, proseMaxWidth)
		}
	}
}
