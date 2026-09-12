package tui

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
)

// markdown-lite: headings, bullet and numbered lists, fenced and inline code,
// bold, italic, blockquote and rules. Tables and links stay literal, and an
// unmatched marker prints as typed. No glamour.

const (
	// listIndent is one nesting step, and maxListDepth caps how deep it goes.
	listIndent   = 2
	maxListDepth = 3
	// codeBar is the dim rail a fenced block hangs off.
	codeBar = "  │ "
)

var (
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	bulletRe  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	numberRe  = regexp.MustCompile(`^(\s*)(\d{1,9})[.)]\s+(.*)$`)
	fenceRe   = regexp.MustCompile("^\\s*(```+|~~~+)\\s*(\\S*)\\s*$")
	quoteRe   = regexp.MustCompile(`^\s*>\s?(.*)$`)
)

// renderMarkdown turns one assistant message into styled, wrapped rows.
func renderMarkdown(text string, width int, th Theme) []string {
	if width <= 0 {
		return nil
	}
	r := mdRenderer{width: width, th: th}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		ln := strings.TrimRight(lines[i], " \t")
		if f := fenceRe.FindStringSubmatch(ln); f != nil {
			r.flush()
			i = r.fence(lines, i, f[1], f[2], "")
			continue
		}
		switch {
		case strings.TrimSpace(ln) == "":
			r.flush()
			r.blank()
		case headingRe.MatchString(ln):
			r.flush()
			r.heading(ln)
		case isRule(ln):
			r.flush()
			r.rule()
		case quoteRe.MatchString(ln):
			r.flush()
			r.quote(ln)
		case bulletRe.MatchString(ln) || numberRe.MatchString(ln):
			r.flush()
			i = r.list(lines, i)
		default:
			r.para = append(r.para, strings.TrimSpace(ln))
		}
	}
	r.flush()
	return r.trimTrailingBlank()
}

type mdRenderer struct {
	width int
	th    Theme
	out   []string
	para  []string
}

func (r *mdRenderer) push(prefix string, body string, st lipgloss.Style) {
	r.out = append(r.out, renderSegs(r.width, seg{prefix + body, st}))
}

func (r *mdRenderer) blank() {
	if len(r.out) > 0 && r.out[len(r.out)-1] != "" {
		r.out = append(r.out, "")
	}
}

func (r *mdRenderer) trimTrailingBlank() []string {
	for len(r.out) > 0 && r.out[len(r.out)-1] == "" {
		r.out = r.out[:len(r.out)-1]
	}
	return r.out
}

// flush wraps the pending paragraph. Inline markers are styled before wrapping
// so a span that survives the line break keeps its colour.
func (r *mdRenderer) flush() {
	if len(r.para) == 0 {
		return
	}
	// Inline markers are matched per source line: §3.4 pins a pair to one line,
	// and joining first would let *foo\nbar* become one italic span.
	styled := make([]string, len(r.para))
	for i, ln := range r.para {
		styled[i] = inlineMarkdown(ln, r.th)
	}
	text := strings.Join(styled, " ")
	r.para = nil
	r.out = append(r.out, hangingRows(text, "", "", r.width, styleFG(r.th.Assistant))...)
}

func (r *mdRenderer) heading(ln string) {
	mt := headingRe.FindStringSubmatch(ln)
	st := lipgloss.NewStyle().Foreground(r.th.Accent).Bold(true)
	r.push("", inlineMarkdown(strings.TrimSpace(mt[2]), r.th), st)
}

func (r *mdRenderer) rule() {
	w := proseWidth(r.width)
	r.push("", strings.Repeat("─", w), styleFG(r.th.Dim))
}

func (r *mdRenderer) quote(ln string) {
	body := quoteRe.FindStringSubmatch(ln)[1]
	r.out = append(r.out, hangingRows(inlineMarkdown(body, r.th), "│ ", "│ ", r.width, styleFG(r.th.Dim))...)
}

// list renders one item and returns the index of its last source line: an item
// whose body opens a fence owns the block that follows it.
func (r *mdRenderer) list(lines []string, i int) int {
	ln := strings.TrimRight(lines[i], " \t")
	var lead, marker, body string
	if mt := numberRe.FindStringSubmatch(ln); mt != nil {
		lead, marker, body = mt[1], mt[2]+".", mt[3]
	} else {
		mt := bulletRe.FindStringSubmatch(ln)
		lead, marker, body = mt[1], "•", mt[2]
	}
	depth := len(strings.ReplaceAll(lead, "\t", "  ")) / listIndent
	if depth > maxListDepth {
		depth = maxListDepth
	}
	pad := strings.Repeat(" ", depth*listIndent)
	if f := fenceRe.FindStringSubmatch(body); f != nil {
		r.push("", pad+marker, styleFG(r.th.Assistant))
		return r.fence(lines, i, f[1], f[2], pad)
	}
	first := pad + marker + " "
	cont := strings.Repeat(" ", lipgloss.Width(first))
	r.out = append(r.out, hangingRows(inlineMarkdown(body, r.th), first, cont, r.width, styleFG(r.th.Assistant))...)
	return i
}

// fence renders a code block: never wrapped, clipped to the terminal instead,
// behind a dim rail. An unterminated fence still renders as one.
func (r *mdRenderer) fence(lines []string, start int, marker, lang, pad string) int {
	bar := styleFG(r.th.Dim)
	if lang != "" {
		r.push(pad+codeBar, lang, bar)
	}
	i := start + 1
	for ; i < len(lines); i++ {
		if f := fenceRe.FindStringSubmatch(strings.TrimRight(lines[i], " \t")); f != nil && f[1][0] == marker[0] {
			return i
		}
		r.out = append(r.out, renderSegs(r.width,
			seg{pad + codeBar, bar},
			seg{plainLine(lines[i]), styleFG(r.th.FG)},
		))
	}
	return i - 1
}

// isRule matches a thematic break: three or more of the same marker, spaces
// aside. A list bullet never matches because it carries other text.
func isRule(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case c:
			n++
		case ' ', '\t':
		default:
			return false
		}
	}
	return n >= 3
}

// inlineMarkdown styles `code`, **bold** and *italic* / _italic_ spans. A
// marker without its partner on the same line is left as typed.
func inlineMarkdown(s string, th Theme) string {
	code := styleFG(th.ToolKind)
	bold := lipgloss.NewStyle().Bold(true)
	italic := lipgloss.NewStyle().Italic(true)
	boldItalic := lipgloss.NewStyle().Bold(true).Italic(true)

	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		switch {
		case rs[i] == '`':
			if j := indexRuneFrom(rs, '`', i+1); j > i+1 {
				b.WriteString(code.Render(string(rs[i+1 : j])))
				i = j + 1
				continue
			}
		case runAt(rs, i, '*', 3):
			if j := indexRunFrom(rs, '*', 3, i+3); j > i+3 {
				b.WriteString(boldItalic.Render(string(rs[i+3 : j])))
				i = j + 3
				continue
			}
		case rs[i] == '*' && i+1 < len(rs) && rs[i+1] == '*':
			if j := indexRunFrom(rs, '*', 2, i+2); j > i+2 {
				b.WriteString(bold.Render(string(rs[i+2 : j])))
				i = j + 2
				continue
			}
		case rs[i] == '*' || rs[i] == '_':
			if j := indexRuneFrom(rs, rs[i], i+1); j > i+1 && rs[i+1] != ' ' && rs[j-1] != ' ' && !intraword(rs, i, j) {
				b.WriteString(italic.Render(string(rs[i+1 : j])))
				i = j + 1
				continue
			}
		}
		b.WriteRune(rs[i])
		i++
	}
	return b.String()
}

// intraword keeps snake_case_names literal: an underscore only opens emphasis
// when it sits on a word boundary.
func intraword(rs []rune, open, close int) bool {
	if rs[open] != '_' {
		return false
	}
	if open > 0 && wordRune(rs[open-1]) {
		return true
	}
	return close+1 < len(rs) && wordRune(rs[close+1])
}

func wordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func indexRuneFrom(rs []rune, r rune, from int) int {
	for i := from; i < len(rs); i++ {
		if rs[i] == r {
			return i
		}
	}
	return -1
}

// runAt reports whether n copies of r start at i.
func runAt(rs []rune, i int, r rune, n int) bool {
	if i+n > len(rs) {
		return false
	}
	for k := 0; k < n; k++ {
		if rs[i+k] != r {
			return false
		}
	}
	return true
}

func indexRunFrom(rs []rune, r rune, n, from int) int {
	for i := from; i+n <= len(rs); i++ {
		if runAt(rs, i, r, n) {
			return i
		}
	}
	return -1
}
