package tui

import (
	"slices"
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

// A wrapped blockquote keeps its bar dim on every row. The styled variant of
// hangingRows takes the continuation prefix style as its own parameter so the
// user row's blank indent can go unstyled without stripping the colour from a
// continuation prefix that is a visible glyph, like this one; the blockquote
// itself still goes through plain hangingRows, which paints the whole row.
func TestBlockquoteBarStaysDimWhenWrapped(t *testing.T) {
	th := Preset("tokyo-night")
	rows := renderMarkdown("> "+strings.Repeat("word ", 20), 40, th)
	if len(rows) < 2 {
		t.Fatalf("want a wrapped quote, got %d row(s): %q", len(rows), rows)
	}
	dim := ansiFG(th.Dim.TrueColor)
	for i, ln := range rows {
		if !strings.HasPrefix(ln, dim) {
			t.Fatalf("quote row %d does not open with the dim colour: %q", i, ln)
		}
		if !strings.HasPrefix(plain(ln), "│ ") {
			t.Fatalf("quote row %d does not start with a bar: %q", i, plain(ln))
		}
	}
}

func TestSplitTableRow(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"| a | b |", []string{"a", "b"}},
		{"a | b", []string{"a", "b"}},
		{"| a | b", []string{"a", "b"}},
		{"a | b |", []string{"a", "b"}},
		{"| a | |", []string{"a", ""}},
		{"| a\tb | c |", []string{"a b", "c"}},
		{`| a \| b | c |`, []string{"a | b", "c"}},
		{"| `a|b` | c |", []string{"`a|b`", "c"}},
		{"no pipe", nil},
		{"`a|b`", nil},
		{"|", nil},
		{`a \| b`, nil},
	}
	for _, tc := range cases {
		got := splitTableRow(tc.in)
		if !slices.Equal(got, tc.want) {
			t.Fatalf("split %q = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestMarkdownTableAlignsColumns(t *testing.T) {
	got := mdLines(t, strings.Join([]string{
		"| item | n | c |",
		"| :--- | ---: | :---: |",
		"| ab | 123 | WXYZ |",
	}, "\n"), 40)
	want := []string{
		"item" + " │ " + "  n" + " │ " + " c  ",
		strings.Repeat("─", 4) + "─┼─" + strings.Repeat("─", 3) + "─┼─" + strings.Repeat("─", 4),
		"ab  " + " │ " + "123" + " │ " + "WXYZ",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
	assertTableBarsLineUp(t, got)
}

func TestMarkdownTableWrapsInsideTheColumn(t *testing.T) {
	got := mdLines(t, strings.Join([]string{
		"| k | v |",
		"| --- | --- |",
		"| ab | one two three four |",
		"| cd | z |",
	}, "\n"), 20)
	want := []string{
		"k " + " │ " + "v" + strings.Repeat(" ", 12),
		"──" + "─┼─" + strings.Repeat("─", 13),
		"ab │ one two three",
		strings.Repeat(" ", 2) + " │ " + "four" + strings.Repeat(" ", 9),
		"cd │ z" + strings.Repeat(" ", 12),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
	assertTableBarsLineUp(t, got)
}

// The shape from the broken transcript: a command table whose rows were joined
// into one wrapping paragraph, delimiter and all. Each command has to open its
// own row, and the sentence before and after the table stay outside it.
func TestMarkdownTableDoesNotCollapseIntoAParagraph(t *testing.T) {
	got := mdLines(t, strings.Join([]string{
		"Other commands:",
		"",
		"| Command | Role |",
		"| --- | --- |",
		"| `craze prompt --json` | Headless path. Same session events, written as JSON lines. |",
		"| `craze frame` | Hidden. Runs the real TUI model with no terminal, feeds a key script, prints the final frame. Used for golden tests. |",
		"| `craze attach` | Connects to a running session. |",
		"| `craze bridge` | Byte pump between stdin/stdout and a running session's control socket, for an SSH client. It does not speak the protocol. |",
		"| `craze version` | Prints the version. **make build** reports `dev`. |",
		"",
		"Config and the session index live in `~/.craze`.",
	}, "\n"), 80)
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "|") || strings.Contains(joined, "---") {
		t.Fatalf("table stayed raw:\n%s", joined)
	}
	if strings.ContainsAny(joined, "`*") {
		t.Fatalf("markers survived:\n%s", joined)
	}
	if got[0] != "Other commands:" || got[1] != "" {
		t.Fatalf("preamble:\n%q", got[:2])
	}
	if got[len(got)-1] != "Config and the session index live in ~/.craze." {
		t.Fatalf("trailer: %q", got[len(got)-1])
	}
	var table []string
	for _, ln := range got {
		if strings.Contains(ln, "│") || strings.Contains(ln, "┼") {
			table = append(table, ln)
		}
	}
	assertTableBarsLineUp(t, table)
	for _, cmd := range []string{"craze prompt --json", "craze frame", "craze attach", "craze bridge", "craze version"} {
		if !rowHasPrefix(table, cmd) {
			t.Fatalf("no row opens with %q:\n%s", cmd, joined)
		}
	}
	if !strings.Contains(joined, "make build") || !strings.Contains(joined, "dev") {
		t.Fatalf("inline spans dropped:\n%s", joined)
	}
}

func TestMarkdownTableKeepsPipesThatAreText(t *testing.T) {
	escaped := mdLines(t, "| a \\| b | c |\n| --- | --- |\n| 1 | 2 |", 30)
	wantEsc := []string{
		"a | b" + " │ " + "c",
		strings.Repeat("─", 5) + "─┼─" + "─",
		"1" + strings.Repeat(" ", 4) + " │ " + "2",
	}
	if !slices.Equal(escaped, wantEsc) {
		t.Fatalf("escaped pipe:\n%q\nwant\n%q", escaped, wantEsc)
	}
	coded := mdLines(t, "| `a|b` | c |\n| --- | --- |\n| 1 | 2 |", 30)
	wantCode := []string{
		"a|b" + " │ " + "c",
		strings.Repeat("─", 3) + "─┼─" + "─",
		"1" + strings.Repeat(" ", 2) + " │ " + "2",
	}
	if !slices.Equal(coded, wantCode) {
		t.Fatalf("pipe in code:\n%q\nwant\n%q", coded, wantCode)
	}
}

func TestMarkdownPipeTextIsNotATable(t *testing.T) {
	prose := mdLines(t, "use a | b here\nand more", 60)
	if len(prose) != 1 || prose[0] != "use a | b here and more" {
		t.Fatalf("prose with a pipe: %q", prose)
	}
	bad := strings.Join(mdLines(t, "| a | b |\n| --- |\n| 1 | 2 |", 60), "\n")
	if strings.Contains(bad, "┼") || !strings.Contains(bad, "| --- |") {
		t.Fatalf("mismatched delimiter: %q", bad)
	}
	fenced := mdLines(t, "```\n| a | b |\n| --- | --- |\n| 1 | 2 |\n```", 40)
	wantFence := []string{"  │ | a | b |", "  │ | --- | --- |", "  │ | 1 | 2 |"}
	if !slices.Equal(fenced, wantFence) {
		t.Fatalf("fence swallowed a table: %q", fenced)
	}
}

func TestMarkdownTableEndsAtTheNextBlock(t *testing.T) {
	cases := []struct {
		in, last string
	}{
		{"| a | b |\n| --- | --- |\n| 1 | 2 |\n- next", "• next"},
		{"| a | b |\n| --- | --- |\n| 1 | 2 |\n## Next", "Next"},
		{"| a | b |\n| --- | --- |\n| 1 | 2 |\n> quote", "│ quote"},
		{"| a | b |\n| --- | --- |\n| 1 | 2 |\n\nafter", "after"},
	}
	for _, tc := range cases {
		got := mdLines(t, tc.in, 40)
		if got[len(got)-1] != tc.last {
			t.Fatalf("last row = %q, want %q\n%q", got[len(got)-1], tc.last, got)
		}
		if strings.Contains(got[len(got)-1], "1") && strings.Contains(got[len(got)-1], "│") {
			t.Fatalf("the next block was eaten: %q", got)
		}
	}
	blank := mdLines(t, "| a | b |\n| --- | --- |\n| 1 | 2 |\n\nafter", 40)
	if blank[len(blank)-2] != "" {
		t.Fatalf("blank line between table and prose dropped: %q", blank)
	}
}

func TestMarkdownTableOneColumnAndOptionalPipes(t *testing.T) {
	one := mdLines(t, "| only |\n| --- |\n| cell |", 20)
	if !slices.Equal(one, []string{"only", "────", "cell"}) {
		t.Fatalf("one column: %q", one)
	}
	with := mdLines(t, "| a | b |\n| --- | --- |\n| 1 | 2 |", 20)
	without := mdLines(t, "a | b\n--- | ---\n1 | 2", 20)
	if !slices.Equal(with, without) {
		t.Fatalf("outer pipes changed the table:\n%q\n%q", with, without)
	}
	empty := mdLines(t, "| a | b |\n| --- | --- |", 20)
	if len(empty) != 2 || !strings.Contains(empty[1], "┼") {
		t.Fatalf("header-only table: %q", empty)
	}
}

func TestMarkdownTableHeaderAndRuleAreStyled(t *testing.T) {
	th := Preset("tokyo-night")
	rows := renderMarkdown("| A | B |\n| --- | --- |\n| 1 | 2 |", 40, th)
	if len(rows) != 3 {
		t.Fatalf("got %d rows: %q", len(rows), rows)
	}
	head := lipgloss.NewStyle().Foreground(th.Bright).Bold(true)
	dim := styleFG(th.Dim)
	if !strings.HasPrefix(rows[0], head.Render("A")) {
		t.Fatalf("header is not bright: %q", rows[0])
	}
	if !strings.HasPrefix(rows[1], ansiFG(th.Dim.TrueColor)) {
		t.Fatalf("rule is not dim: %q", rows[1])
	}
	if !strings.Contains(rows[2], dim.Render(tableGap)) {
		t.Fatalf("gutter is not dim: %q", rows[2])
	}
}

func TestMarkdownTableCapsWidth(t *testing.T) {
	cell := strings.Repeat("word ", 40)
	text := "| a | b |\n| --- | --- |\n| " + cell + " | " + cell + " |"
	rows := renderMarkdown(text, 200, Preset("tokyo-night"))
	if len(rows) < 3 {
		t.Fatalf("got %q", rows)
	}
	for _, ln := range rows {
		if w := lipgloss.Width(ln); w > proseMaxWidth {
			t.Fatalf("table row is %d wide, cap is %d: %q", w, proseMaxWidth, plain(ln))
		}
	}
	narrow := mdLines(t, "| a | b |\n| --- | --- |\n| hello | world |", 8)
	var left, right strings.Builder
	for _, ln := range narrow {
		parts := strings.Split(ln, "│")
		if len(parts) != 2 {
			continue
		}
		left.WriteString(strings.TrimSpace(parts[0]))
		right.WriteString(strings.TrimSpace(parts[1]))
	}
	if !strings.Contains(left.String(), "hello") || !strings.Contains(right.String(), "world") {
		t.Fatalf("narrow table dropped a cell:\nleft %q\nright %q\n%q", left.String(), right.String(), narrow)
	}
}

func TestMarkdownTableKeepsACellPastTheHeader(t *testing.T) {
	got := strings.Join(mdLines(t, "| a | b |\n| --- | --- |\n| 1 | 2 | 3 |", 40), "\n")
	if !strings.Contains(got, "2 | 3") {
		t.Fatalf("a cell past the header was dropped: %q", got)
	}
}

func TestMarkdownTableGutterMatchesTheRule(t *testing.T) {
	if lipgloss.Width(tableGap) != lipgloss.Width(tableJunction) {
		t.Fatalf("gutter is %d cells, junction is %d", lipgloss.Width(tableGap), lipgloss.Width(tableJunction))
	}
}

// Markers are removed before the column is measured, so **ab** is two cells
// wide, not six. A compact delimiter (|---|---|) is the form models emit.
func TestMarkdownTableMeasuresTheStyledCell(t *testing.T) {
	got := mdLines(t, "| **ab** | c |\n|---|---|\n| wxyz | z |", 30)
	want := []string{
		"ab  " + " │ " + "c",
		strings.Repeat("─", 4) + "─┼─" + "─",
		"wxyz" + " │ " + "z",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

// A table may open on the line after a paragraph, with no blank between them.
func TestMarkdownTableInterruptsAParagraph(t *testing.T) {
	got := mdLines(t, "Intro\n| a | b |\n|---|---|\n| 1 | 2 |", 40)
	want := []string{"Intro", "a │ b", "──┼──", "1 │ 2"}
	if !slices.Equal(got, want) {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

// An empty column still occupies a cell, so the gutter does not slide left
// into the text beside it.
func TestMarkdownTableEmptyColumnKeepsItsGutter(t *testing.T) {
	got := mdLines(t, "| a | |\n| --- | --- |\n| 1 | |", 20)
	want := []string{
		"a" + " │ " + " ",
		"─" + "─┼─" + "─",
		"1" + " │ " + " ",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
	assertTableBarsLineUp(t, got)
}

func TestMarkdownTablePadsAShortRow(t *testing.T) {
	got := mdLines(t, "| aa | bb |\n| --- | --- |\n| 1 |", 40)
	want := []string{
		"aa" + " │ " + "bb",
		strings.Repeat("─", 2) + "─┼─" + strings.Repeat("─", 2),
		"1" + strings.Repeat(" ", 1) + " │ " + strings.Repeat(" ", 2),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

// 你 is two cells wide. The rule is built in display columns, so a rune count
// of the header and the rule would disagree while the gutters still meet.
func TestMarkdownTableWideRuneKeepsTheRuleAligned(t *testing.T) {
	got := mdLines(t, "| 你 | b |\n| --- | --- |\n| a | c |", 20)
	want := []string{
		"你" + " │ " + "b",
		strings.Repeat("─", 2) + "─┼─" + "─",
		"a" + strings.Repeat(" ", 1) + " │ " + "c",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
	assertTableBarsLineUp(t, got)
}

// A dash that lives in a cell is not a list, and a fence or a rule after the
// table is that block, not another row.
func TestMarkdownTableReleasesTheFollowingBlock(t *testing.T) {
	dash := mdLines(t, "| a | b |\n| --- | --- |\n| - item | x |", 40)
	if strings.Contains(strings.Join(dash, "\n"), "•") {
		t.Fatalf("a cell became a list: %q", dash)
	}
	if !rowHasPrefix(dash, "- item") {
		t.Fatalf("the cell was dropped: %q", dash)
	}
	fenced := mdLines(t, "| a | b |\n| --- | --- |\n| 1 | 2 |\n```\ncode\n```", 40)
	if fenced[len(fenced)-1] != "  │ code" {
		t.Fatalf("fence after a table: %q", fenced)
	}
	ruled := mdLines(t, "| a | b |\n| --- | --- |\n| 1 | 2 |\n---", 40)
	if ruled[len(ruled)-1] != strings.Repeat("─", proseWidth(40)) {
		t.Fatalf("rule after a table: %q", ruled)
	}
}

func TestMarkdownTableAtWidthOneDoesNotPanic(t *testing.T) {
	got := mdLines(t, "| a | b |\n| --- | --- |\n| 1 | 2 |", 1)
	if len(got) == 0 {
		t.Fatal("width 1 dropped the table")
	}
}

// A column forced under a wide grapheme still has to keep the gutters
// aligned: the grapheme is clipped to the column rather than shoving the bar.
func TestMarkdownTableClipsAWideRuneToTheColumn(t *testing.T) {
	got := mdLines(t, "| 你 | 你 | 你 | 你 |\n| --- | --- | --- | --- |\n| 你 | 你 | 你 | 你 |", 16)
	assertTableBarsLineUp(t, got)
}

func rowHasPrefix(rows []string, prefix string) bool {
	for _, ln := range rows {
		if strings.HasPrefix(ln, prefix) && strings.Contains(ln, "│") {
			return true
		}
	}
	return false
}

// assertTableBarsLineUp checks that every gutter sits on a junction in the
// rule and on a gutter in every other row. Columns are display cells: a wide
// rune takes two, so a rune index would report drift that the screen does not.
func assertTableBarsLineUp(t *testing.T, rows []string) {
	t.Helper()
	if len(rows) < 2 {
		t.Fatalf("table has %d row(s): %q", len(rows), rows)
	}
	first := glyphCols(rows[0])
	for _, row := range rows[1:] {
		rr := glyphCols(row)
		if len(rr) == 0 || len(first) == 0 || rr[len(rr)-1].end != first[len(first)-1].end {
			t.Fatalf("row widths differ:\n%q\n%q", rows[0], row)
		}
		bars, under := map[int]bool{}, map[int]bool{}
		for _, g := range first {
			if g.r == '│' {
				bars[g.col] = true
			}
		}
		for _, g := range rr {
			if g.r == '│' || g.r == '┼' {
				under[g.col] = true
			}
		}
		if len(bars) != len(under) {
			t.Fatalf("bar drift:\n%q\n%q", rows[0], row)
		}
		for col := range bars {
			if !under[col] {
				t.Fatalf("bar drift at column %d:\n%q\n%q", col, rows[0], row)
			}
		}
	}
}

type glyphCol struct {
	col, end int
	r        rune
}

func glyphCols(s string) []glyphCol {
	var out []glyphCol
	col := 0
	for _, r := range s {
		w := lipgloss.Width(string(r))
		out = append(out, glyphCol{col: col, end: col + w, r: r})
		col += w
	}
	return out
}
