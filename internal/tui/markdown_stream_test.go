package tui

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// The checkpointed markdown render (plan 032 §3.3 C6, R2-4, R2-5): a render of
// a growing text resumed from a checkpoint must be the render from the top,
// byte for byte, at every chunk.

// mdStream renders the stream of chunks the way a streaming row does — one
// generation, one checkpoint carried from render to render — and holds every
// render to renderMarkdown's of the same text, and every checkpoint to the
// rule: what it resumes from is complete lines only. It reports how many
// renders it made and how many of them resumed.
func mdStream(t *testing.T, label string, chunks []string, width int, th Theme) (renders, resumed int) {
	t.Helper()
	var (
		cp   *mdCheckpoint
		text string
	)
	for i, c := range chunks {
		text += c
		got, parsed := renderStreamingMarkdown(text, width, th, 1, &cp)
		want := renderMarkdown(text, width, th)
		if !slices.Equal(got, want) {
			t.Fatalf("%s, width %d, chunk %d: the resumed render differs from the full one\ntext %q\n got %q\nwant %q", label, width, i, text, got, want)
		}
		renders++
		if parsed < len(text) {
			resumed++
		}
		if cp == nil {
			continue
		}
		// The checkpoint's source is complete lines, followed by more text:
		// the line it resumes at is complete too (mdCheckpoint's rule).
		switch {
		case cp.off <= 0 || cp.off >= len(text) || text[cp.off-1] != '\n':
			t.Fatalf("%s, chunk %d: a checkpoint at %d of %q", label, i, cp.off, text)
		case !strings.Contains(text[cp.off:], "\n"):
			t.Fatalf("%s, chunk %d: a checkpoint at %d is at the unterminated last line of %q", label, i, cp.off, text)
		case len(cp.out) != cap(cp.out):
			t.Fatalf("%s, chunk %d: a checkpoint's rows have room to grow (%d of %d), so a resumed render would write into drawn rows", label, i, len(cp.out), cap(cp.out))
		}
	}
	return renders, resumed
}

// mdChunks cuts text into chunks of 1 to 9 bytes — anywhere, a rune's bytes
// included, as a stream may.
func mdChunks(rng *rand.Rand, text string) []string {
	var out []string
	for pos := 0; pos < len(text); {
		n := min(1+rng.Intn(9), len(text)-pos)
		out = append(out, text[pos:pos+n])
		pos += n
	}
	return out
}

// mdBytewise is text one byte at a time.
func mdBytewise(text string) []string {
	out := make([]string, len(text))
	for i := range text {
		out[i] = text[i : i+1]
	}
	return out
}

// mdRandomDoc is markdown from the renderer's own block kinds, with the edges
// the checkpoint rule has to survive: tables whose next line may grow into a
// row, a paragraph line that becomes a table's header when its next line
// does, open and closed fences (both markers, a language, lines inside that
// look like other blocks), fences owned by list items, nested and numbered
// lists, quotes, rules, blank lines of spaces and tabs, trailing spaces; wide,
// private-use, joined and combining runes; and unmatched inline markers.
func mdRandomDoc(rng *rand.Rand) string {
	words := []string{
		"alpha", "beta", "word", "`code`", "**bold**", "*it*", "_em_", "***both***",
		"snake_case_name", "-c", "--model", "a-b", "x|y", `a\|b`, "[link](u)",
		"宽字符", "表", "🙂", "🇯🇵", "👨‍👩‍👧", "", "é", "\t",
		"*", "**open", "`", "_", "***", "close**", "longtoken" + strings.Repeat("z", 110),
	}
	pick := func() string { return words[rng.Intn(len(words))] }
	para := func() string {
		var b strings.Builder
		for i, n := 0, 1+rng.Intn(14); i < n; i++ {
			if i > 0 {
				b.WriteString(" ")
			}
			b.WriteString(pick())
			if rng.Intn(10) == 0 {
				b.WriteString("\n")
			}
		}
		return b.String()
	}
	fence := func(marker string) string {
		body := marker + []string{"", "go", "sh"}[rng.Intn(3)] + "\nfunc main() {\n\tfmt.Println(1)\n# not a heading\n| a | b |\n}"
		if rng.Intn(2) == 0 {
			body += "\n" + marker
		}
		return body
	}
	var blocks []string
	for k, n := 0, 2+rng.Intn(6); k < n; k++ {
		switch rng.Intn(16) {
		case 0:
			blocks = append(blocks, strings.Repeat("#", 1+rng.Intn(6))+" "+para())
		case 1:
			blocks = append(blocks, "- "+para()+"\n  - "+pick()+"\n\t* "+pick()+"\n1. "+para()+"\n2) "+pick())
		case 2:
			blocks = append(blocks, fence("```"))
		case 3:
			blocks = append(blocks, fence("~~~"))
		case 4:
			blocks = append(blocks, "| a | b |\n|---|:-:|\n| "+pick()+" | two |\n| three | "+para()+" |")
		case 5:
			// A table whose last line is a paragraph line that may still
			// grow into a row.
			blocks = append(blocks, "| a |\n| - |\nlong"+[]string{"", " |", " | more", ""}[rng.Intn(4)])
		case 6:
			blocks = append(blocks, "a | b\n--- | ---\n1 | 2")
		case 7:
			// A paragraph line that is a table's header only once its next
			// line is a delimiter.
			blocks = append(blocks, "head | er\n"+[]string{"-", "--- | ---", "x", "|-|-|"}[rng.Intn(4)])
		case 8:
			blocks = append(blocks, "> "+para())
		case 9:
			blocks = append(blocks, []string{"---", "* * *", "___", "--"}[rng.Intn(4)])
		case 10:
			blocks = append(blocks, "- ```\ncode in item\n```", "1. ~~~sh\nopen in item")
		case 11:
			blocks = append(blocks, []string{"   \t ", "", " "}[rng.Intn(3)])
		case 12:
			blocks = append(blocks, para()+"   ")
		default:
			blocks = append(blocks, para())
		}
	}
	sep := []string{"\n\n", "\n", "\n\n\n", "\n  \n", "\n\t\n"}
	var b strings.Builder
	for i, bl := range blocks {
		if i > 0 {
			b.WriteString(sep[rng.Intn(len(sep))])
		}
		b.WriteString(bl)
	}
	if rng.Intn(3) == 0 {
		b.WriteString(strings.Repeat("\n", 1+rng.Intn(3)))
	}
	return b.String()
}

// mdDocs is how many random documents the equivalence test streams: enough to
// pass every block kind at every edge many times over (it logs the renders and
// how many resumed), few enough for a starved CPU. Under -race, which finds
// nothing in a pure function on one goroutine, the first third of them.
func mdDocs() int {
	if raceEnabled {
		return 40
	}
	return 120
}

// TestStreamingMarkdownResumesByteIdentical streams seeded random markdown in
// chunks of 1 to 9 bytes at widths 1 to 160, and every resumed render must be
// the full render of the same text; then the fixed cases R2-5 names, each one
// byte at a time and in the chunks that cut it where it is fragile.
func TestStreamingMarkdownResumesByteIdentical(t *testing.T) {
	th := Preset("tokyo-night")
	rng := rand.New(rand.NewSource(32))
	renders, resumed := 0, 0
	for d := range mdDocs() {
		doc := mdRandomDoc(rng)
		width := 1 + rng.Intn(160)
		if d%8 == 0 {
			width = []int{1, 2, 3, 160}[d/8%4]
		}
		n, r := mdStream(t, fmt.Sprintf("doc %d", d), mdChunks(rng, doc), width, th)
		renders += n
		resumed += r
	}
	t.Logf("random docs: %d renders, %d resumed", renders, resumed)
	// The stream has to resume, or it proves nothing about resuming.
	if resumed*2 < renders {
		t.Fatalf("only %d of %d renders resumed", resumed, renders)
	}

	fixed := []struct {
		name   string
		chunks []string
	}{
		// R2-5: the line after a table grows into one of its rows.
		{"a table's next line grows into a row", []string{"| a |\n| - |\nlong", " |", "\nafter\n\nmore"}},
		{"a table's next line stays prose", []string{"| a |\n| - |\nlong", " words", "\n\nmore"}},
		// The fence's closing marker arrives a byte at a time.
		{"a fence closed in pieces", []string{"```go\ncode\n", "`", "`", "`", "\nafter\n\nmore"}},
		{"a list item's fence closed in pieces", []string{"- ```\ncode\n", "~", "``", "`", "\n\nafter"}},
		{"a tilde fence is not closed by backticks", []string{"~~~\ncode\n```\n", "~~", "~\n\npara"}},
		// A paragraph's line becomes a table header when the delimiter
		// arrives after it.
		{"a delimiter arrives under a paragraph line", []string{"intro\n\na | b\n", "-", "--", " | ---\n| 1 | 2 |\n", "\nend"}},
		{"a delimiter that never completes", []string{"a | b\n", "--- | -", "x\n\nnext"}},
		// blank()'s dedup and the trailing-blank trim on both sides of a
		// checkpoint.
		{"blank lines around a checkpoint", []string{"# h\n\n\n", "\n", "para\n\n\n", "\n", "- item\n\n", "tail"}},
		{"a heading arrives in pieces", []string{"para\n\n#", "#", " title\n", "next"}},
	}
	for _, fc := range fixed {
		text := strings.Join(fc.chunks, "")
		for _, width := range []int{1, 7, 40, 100} {
			mdStream(t, fc.name+" (chunked)", fc.chunks, width, th)
			mdStream(t, fc.name+" (bytewise)", mdBytewise(text), width, th)
		}
	}
}

// TestStreamingMarkdownStartsOverWhenItsCheckpointDoesNotHold: a checkpoint
// is resumed only for the generation, width and theme it was taken at, and a
// text longer than its source; anything else renders from the top and leaves
// a checkpoint of its own.
func TestStreamingMarkdownStartsOverWhenItsCheckpointDoesNotHold(t *testing.T) {
	th, other := Preset("tokyo-night"), Preset("gruvbox")
	text := "# one\n\npara one\n\npara two\n\n- item\n\ntail"
	var cp *mdCheckpoint
	if _, parsed := renderStreamingMarkdown(text, 80, th, 7, &cp); parsed != len(text) || cp == nil {
		t.Fatalf("the first render read %d of %d bytes, checkpoint %v", parsed, len(text), cp)
	}
	taken := *cp
	if _, parsed := renderStreamingMarkdown(text+"s", 80, th, 7, &cp); parsed >= len(text) {
		t.Fatalf("a render under the same generation read %d bytes, not resuming at %d", parsed, taken.off)
	}
	for _, c := range []struct {
		name  string
		text  string
		width int
		th    Theme
		gen   uint64
	}{
		{"another generation", text + "s", 80, th, 8},
		{"another width", text + "s", 81, th, 7},
		{"another theme", text + "s", 80, other, 7},
		{"a text no longer than its source", text[:taken.off], 80, th, 7},
	} {
		cp2 := taken
		cpp := &cp2
		rows, parsed := renderStreamingMarkdown(c.text, c.width, c.th, c.gen, &cpp)
		if parsed != len(c.text) {
			t.Errorf("%s: the render read %d of %d bytes: it resumed", c.name, parsed, len(c.text))
		}
		if want := renderMarkdown(c.text, c.width, c.th); !slices.Equal(rows, want) {
			t.Errorf("%s: the render differs from the full one", c.name)
		}
		if cpp != nil && (cpp.gen != c.gen || cpp.width != c.width || cpp.theme != c.th.Name) {
			t.Errorf("%s: the new checkpoint is %+v", c.name, *cpp)
		}
	}
}
