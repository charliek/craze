package opencode

import (
	"context"
	"errors"
	"math/rand/v2"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/tool"
)

// emoji is U+1F600: one rune, two UTF-16 units, four bytes. Tests build it
// from its number so no escape in this file can be mistranslated.
var emoji = string(rune(0x1F600))

// replaced runs replace and fails the test unless it succeeded.
func replaced(t *testing.T, content, oldString, newString string, all bool) string {
	t.Helper()
	out, _, err := replace(context.Background(), content, oldString, newString, all)
	if err != nil {
		t.Fatalf("replace(%q, %q, %q, %v): %v", content, oldString, newString, all, err)
	}
	return out
}

// refused runs replace and fails the test unless it failed with text.
func refused(t *testing.T, content, oldString, newString string, all bool, text string) {
	t.Helper()
	out, _, err := replace(context.Background(), content, oldString, newString, all)
	var f *failure
	if !errors.As(err, &f) || f.text != text {
		t.Fatalf("replace(%q, %q, %q, %v) = %q, %v; want the refusal %q", content, oldString, newString, all, out, err, text)
	}
}

// TestReplacerOrder pins the nine replacers to opencode's order
// (edit.ts:694-704), which is not the order edit.ts defines them in.
func TestReplacerOrder(t *testing.T) {
	var got []string
	for _, r := range replacers {
		name := runtime.FuncForPC(reflect.ValueOf(r).Pointer()).Name()
		got = append(got, name[strings.LastIndex(name, ".")+1:])
	}
	want := []string{"simple", "lineTrimmed", "blockAnchor", "whitespaceNormalized", "indentationFlexible",
		"escapeNormalized", "trimmedBoundary", "contextAware", "multiOccurrence"}
	if !slices.Equal(got, want) {
		t.Fatalf("replacers = %q, want %q", got, want)
	}
}

// TestReplacerYields: what each replacer yields for a case that exercises
// it — the expected yields are what opencode's own generators yield for the
// same content and find, run under bun at 5f9d9187. The empty expectations
// are the negative controls: a middle line or middle block too different.
func TestReplacerYields(t *testing.T) {
	cases := []struct {
		r             replacer
		content, find string
		want          []string
	}{
		{simple, "abc", "b", []string{"b"}},
		{lineTrimmed, "  foo\nbar  \nbaz", "foo\nbar\n", []string{"  foo\nbar  "}},
		{lineTrimmed, "x\n  foo\ny\n\tfoo \n", "foo", []string{"  foo", "\tfoo "}},
		{blockAnchor, "function a() {\n  return 1\n}\n", "function a() {\n  return 2\n}", []string{"function a() {\n  return 1\n}"}},
		{blockAnchor, "if (a) {\n  one()\n}\nif (a) {\n  two()\n}\n", "if (a) {\n  twe()\n}", []string{"if (a) {\n  two()\n}"}},
		{blockAnchor, "a\nX\nb", "a\nb\n", []string{"a\nX\nb"}},
		{blockAnchor, "start\n  removeAllUserData()\nend", "start\n  const enabled = true\nend", nil},
		{whitespaceNormalized, "x  =   1\ny", "x = 1", []string{"x  =   1"}},
		{whitespaceNormalized, "let x  =\t1;", "x = 1", []string{"x  =\t1"}},
		{whitespaceNormalized, "a  b\nc   d\n", "a b\nc d", []string{"a  b\nc   d"}},
		{indentationFlexible, "    if (a) {\n      b()\n    }", "if (a) {\n  b()\n}", []string{"    if (a) {\n      b()\n    }"}},
		{escapeNormalized, "a\nb", `a\nb`, []string{"a\nb", "a\nb"}},
		{escapeNormalized, `say("hi")`, `say(\"hi\")`, []string{`say("hi")`, `say("hi")`}},
		{escapeNormalized, `x\\n`, `x\\\\n`, []string{`x\\n`}},
		{trimmedBoundary, "foo", "  foo  ", []string{"foo", "foo"}},
		{trimmedBoundary, "a\n  b  \nc", "\n  b  \n", []string{"b"}},
		{contextAware, "start\n  a\n  CHANGED\n  c\nend", "start\n  a\n  b\n  c\nend", []string{"start\n  a\n  CHANGED\n  c\nend"}},
		{contextAware, "start\n  x\n  y\n  z\nend", "start\n  a\n  b\n  c\nend", nil},
		{multiOccurrence, "aXaXa", "a", []string{"a", "a", "a"}},
		{multiOccurrence, "aaaa", "aa", []string{"aa", "aa"}},
	}
	for _, tc := range cases {
		got := slices.Collect(tc.r(context.Background(), newText(tc.content), tc.find))
		if !slices.Equal(got, tc.want) {
			name := runtime.FuncForPC(reflect.ValueOf(tc.r).Pointer()).Name()
			t.Errorf("%s(%q, %q) yields %q, want %q", name, tc.content, tc.find, got, tc.want)
		}
	}
}

// TestReplaceFallsThrough: a span found more than once passes to the next
// candidate and the next replacer (edit.ts:717-718). "value" is in the
// file twice, so the simple replacer's hit is ambiguous; line-trimmed then
// yields "value" (ambiguous again) and "  value  " (once), which is
// replaced — opencode's rule, whole line and all. When every replacer's
// spans are ambiguous, the result is opencode's "multiple matches"; with
// replaceAll the first hit wins outright. A unique exact hit never falls
// through: the negative control.
func TestReplaceFallsThrough(t *testing.T) {
	if got := replaced(t, "value\n  value  \n", "value", "V", false); got != "value\nV\n" {
		t.Fatalf("fall-through = %q", got)
	}
	refused(t, "foo\nfoo\n", "foo", "bar", false, multipleText)
	if got := replaced(t, "foo\nfoo\n", "foo", "bar", true); got != "bar\nbar\n" {
		t.Fatalf("replaceAll = %q", got)
	}
	if got := replaced(t, "value\n  other  \n", "value", "V", false); got != "V\n  other  \n" {
		t.Fatalf("a unique hit = %q", got)
	}
	// A later replacer resolving what the earlier ones could not: two
	// blocks share both anchors, and block-anchor picks the closer one.
	content := "if (a) {\n  one()\n}\nif (a) {\n  two()\n}\n"
	if got := replaced(t, content, "if (a) {\n  twe()\n}", "X", false); got != "if (a) {\n  one()\n}\nX\n" {
		t.Fatalf("block-anchor = %q", got)
	}
}

// TestReplaceRefusals: opencode's sentences, each for its case, with a
// case beside each that goes through as the negative control.
func TestReplaceRefusals(t *testing.T) {
	refused(t, "content", "same", "same", false, identicalText)
	refused(t, "content", "", "", false, identicalText) // identical is checked first
	refused(t, "content", "", "x", false, emptyOldText)
	refused(t, "actual content", "not in file", "replacement", false, notFoundText)
	replaced(t, "actual content", "actual", "real", false)

	// Two loose block-anchor matches from opencode's edit.test.ts: too many
	// lines between the anchors, and an unrelated middle line.
	loose := "function configure() {\n  keepImportantState()\n  removeAllUserData()\n  archiveBackups()\n  auditLog()\n}"
	enabled := "function configure() {\n  const enabled = true\n}"
	refused(t, loose, enabled, "function configure() {\n  const enabled = false\n}", false, notFoundText)
	refused(t, "function configure() {\n  removeAllUserData()\n}", enabled, "x", false, notFoundText)
	replaced(t, "function configure() {\n  const enabled = tru\n}", enabled, "x", false)
}

// TestReplaceDisproportion: isDisproportionateMatch's two conditions
// (edit.ts:731-737), each at its boundary, the line one below it and the
// length one on both of its branches. The spans come from the replacers
// that can produce them: escape-normalized turns "\n" escapes into lines,
// and block-anchor accepts a block one line longer than oldString without
// comparing the extra line.
func TestReplaceDisproportion(t *testing.T) {
	// Condition 1: the span has at least max(old+3, 2*old) lines.
	refused(t, "a\nb\nc\nd", `a\nb\nc\nd`, "X", false, disproportionText)
	if got := replaced(t, "a\nb\nc\nd", `a\nb\nc`, "X", false); got != "X\nd" {
		t.Fatalf("three lines for one = %q", got)
	}

	// Condition 2, the +500 branch: "start\nmid\n<n x>\nend" for
	// "start\nmid\nend" is 14+n runes against 13, over max(513, 52) at 500.
	block := func(n int, s string) string { return "start\nmid\n" + strings.Repeat(s, n) + "\nend" }
	refused(t, block(500, "x"), "start\nmid\nend", "X", false, disproportionText)
	if got := replaced(t, block(499, "x"), "start\nmid\nend", "X", false); got != "X" {
		t.Fatalf("at the +500 boundary = %q", got)
	}

	// The x4 branch: a 200-rune oldString allows 800, where +500 alone
	// would stop at 700.
	mid := strings.Repeat("m", 196)
	old := "S\n" + mid + "\nE"
	long := func(k int) string { return "S\n" + mid + "\n" + strings.Repeat("x", k) + "\nE" }
	refused(t, long(600), old, "X", false, disproportionText)
	if got := replaced(t, long(599), old, "X", false); got != "X" {
		t.Fatalf("at the x4 boundary = %q", got)
	}

	// A one-line oldString is never refused for length: whitespace-
	// normalized matches a line with 600 spaces in it.
	if got := replaced(t, "a"+strings.Repeat(" ", 600)+"b", "a b", "X", false); got != "X" {
		t.Fatalf("one line = %q", got)
	}
}

// TestReplaceCountsRunes: the thresholds count runes, not UTF-16 units or
// bytes (plan 019 §3.2). With U+1F600 — two units, four bytes — the length
// boundary sits where runes put it, and a middle line of 14 "a" and six
// emoji against 20 "a" is 6 edits in 20 runes (0.7, a match) where it
// would be 12 in 26 units (0.54) or 24 in 38 bytes (0.37), not found.
func TestReplaceCountsRunes(t *testing.T) {
	block := func(n int) string { return "start\nmid\n" + strings.Repeat(emoji, n) + "\nend" }
	if got := replaced(t, block(499), "start\nmid\nend", "X", false); got != "X" {
		t.Fatalf("499 emoji = %q", got)
	}
	refused(t, block(500), "start\nmid\nend", "X", false, disproportionText)

	content := "start\n" + strings.Repeat("a", 20) + "\nend"
	old := "start\n" + strings.Repeat("a", 14) + strings.Repeat(emoji, 6) + "\nend"
	if got := replaced(t, content, old, "X", false); got != "X" {
		t.Fatalf("similarity in runes = %q", got)
	}
	if d, _ := levenshtein(context.Background(), strings.Repeat("a", 20), strings.Repeat("a", 14)+strings.Repeat(emoji, 6)); d != 6 {
		t.Fatalf("levenshtein = %d, want 6 (runes)", d)
	}
	if d, _ := levenshtein(context.Background(), "", "é"+emoji); d != 2 {
		t.Fatalf("levenshtein with an empty side = %d, want 2 (runes)", d)
	}
}

// TestReplaceIsLiteral: newString is inserted as it is, with or without
// replaceAll. JavaScript's replaceAll expands $&, $$, $` and $' in it
// (edit.ts:715); the plan makes that a bug fix. The $1 case would be
// literal in JavaScript too.
func TestReplaceIsLiteral(t *testing.T) {
	const nu = "$& $1 $$ $' $`"
	if got := replaced(t, "foo bar foo", "foo", nu, true); got != nu+" bar "+nu {
		t.Fatalf("replaceAll = %q", got)
	}
	if got := replaced(t, "foo bar", "foo", nu, false); got != nu+" bar" {
		t.Fatalf("one = %q", got)
	}
}

// TestReplaceSkipsAnEmptySpan: a whitespace-only oldString that is not in
// the file makes the trimming replacers yield "". opencode counts that as
// found: with replaceAll it puts newString between every character, and in
// an empty file it writes newString. Here it is no match. A whitespace-only
// oldString that is in the file is the negative control.
func TestReplaceSkipsAnEmptySpan(t *testing.T) {
	refused(t, "a\n\nb\n", "   ", "X", true, notFoundText)
	refused(t, "a\n\nb\n", "   ", "X", false, notFoundText)
	refused(t, "", "\n", "X", false, notFoundText)
	if got := replaced(t, "a\n   \nb\n", "   ", "X", true); got != "a\nX\nb\n" {
		t.Fatalf("a whitespace oldString in the file = %q", got)
	}
}

// TestReplaceUsesJavaScriptWhitespace: opencode trims and matches \s as
// JavaScript does. U+FEFF is whitespace to it, and U+0085 is not — the
// reverse of Go's unicode.IsSpace, which would match the second and miss
// the first.
func TestReplaceUsesJavaScriptWhitespace(t *testing.T) {
	bomChar := string(rune(0xFEFF))
	nel := string(rune(0x85))
	if got := replaced(t, "a\nfoo"+bomChar+"\nb", "foo\nb", "X", false); got != "a\nX" {
		t.Fatalf("U+FEFF trimmed = %q", got)
	}
	refused(t, "a\nfoo"+nel+"\nb", "foo\nb", "X", false, notFoundText)
	if !jsSpace(0x3000) || !jsSpace(0x2000) || !jsSpace(0x200a) || jsSpace(0x200b) || jsSpace(0x180e) {
		t.Fatal("jsSpace's Space_Separator range is wrong")
	}
	if got := normalizeWhitespace(" a \t\v" + string(rune(0x3000)) + "b\xff  "); got != "a b\xff" {
		t.Fatalf("normalizeWhitespace = %q (an invalid byte must compare as itself)", got)
	}
}

// TestReplaceHonoursCancel: a cancel ends the search with the context's
// error, before any replacer and during one.
func TestReplaceHonoursCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := replace(ctx, "a\nb\nc", "a\nx\nc", "y", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, ok := levenshtein(ctx, "abc", "abd"); ok {
		t.Fatal("levenshtein ran on a done context")
	}
	for _, r := range replacers[1:] {
		if got := slices.Collect(r(ctx, newText("a\na\na\na"), "a\na\na")); got != nil {
			t.Fatalf("a replacer yielded %q on a done context", got)
		}
	}
}

// TestReplaceChangesOnlyTheSpan is the lost-bytes property: for any content
// — invalid UTF-8, lone CRs, BOM characters and emoji included — and an
// oldString found once, the result is strings.Replace's, so no byte outside
// the span changes; with replaceAll, strings.ReplaceAll's. "Once" is
// opencode's indexOf === lastIndexOf, which counts overlapping occurrences:
// "bb" is in "bbb" twice. The generator is seeded, so a failure repeats.
func TestReplaceChangesOnlyTheSpan(t *testing.T) {
	rng := rand.New(rand.NewPCG(19, 5))
	tokens := []string{"a", "b", "c", "foo", " ", "  ", "\t", "\n", "\n", "\r", "\r\n", "é", emoji, "\xff", "\xc3",
		string(rune(0xFEFF)), "{", "}", `\n`, "$&", "$1"}
	gen := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteString(tokens[rng.IntN(len(tokens))])
		}
		return b.String()
	}
	checked := 0
	for range 20000 {
		content := gen(rng.IntN(200))
		if len(content) == 0 {
			continue
		}
		i := rng.IntN(len(content))
		j := i + 1 + rng.IntN(min(40, len(content)-i))
		old, nu := content[i:j], gen(rng.IntN(6))
		if old == nu {
			continue
		}
		all := rng.IntN(4) == 0
		want := strings.ReplaceAll(content, old, nu)
		if !all {
			if strings.Index(content, old) != strings.LastIndex(content, old) {
				continue
			}
			want = strings.Replace(content, old, nu, 1)
		}
		out, at, err := replace(context.Background(), content, old, nu, all)
		if err != nil || out != want || at != strings.Index(content, old) {
			t.Fatalf("replace(%q, %q, %q, %v) = %q at %d, %v; want %q at %d",
				content, old, nu, all, out, at, err, want, strings.Index(content, old))
		}
		checked++
	}
	if checked < 5000 {
		t.Fatalf("only %d cases were checked", checked)
	}
}

// TestSpliceMapsWhatReadShowsToTheBytes is the lost-bytes property for a
// file that is not valid UTF-8: match judges the view read shows (each
// invalid byte as U+FFFD) and splice edits the bytes. For random bytes and
// an oldString from the view without U+FFFD, either the edit would take in
// or re-join an invalid byte and is refused, or the result reads exactly as
// the same edit of the view does, and every invalid byte is still there.
// (It found the joining case: deleting what lies between E2 82 and 80.)
func TestSpliceMapsWhatReadShowsToTheBytes(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	tokens := []string{"a", "b", "foo", " ", "\n", "\n", "é", emoji, "\xff", "\xc3", "\xe2\x82", "\x80",
		"\xef\xbf\xbd", "{", "}"}
	gen := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteString(tokens[rng.IntN(len(tokens))])
		}
		return b.String()
	}
	invalid := func(s string) (n int) {
		for i := 0; i < len(s); {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				n++
			}
			i += size
		}
		return n
	}
	edited, refusedSpans := 0, 0
	for range 60000 {
		raw := gen(1 + rng.IntN(80))
		view := validUTF8([]byte(raw))
		i := rng.IntN(len(view))
		j := i + 1 + rng.IntN(min(20, len(view)-i))
		old, nu := view[i:j], gen(rng.IntN(4))
		nu = strings.ToValidUTF8(nu, "")
		if !utf8.ValidString(old) || strings.ContainsRune(old, utf8.RuneError) || old == nu {
			continue
		}
		all := rng.IntN(4) == 0
		want, _, wantErr := replace(context.Background(), view, old, nu, all)
		search, at, err := match(context.Background(), view, old, nu, all)
		if (err == nil) != (wantErr == nil) {
			t.Fatalf("match and replace disagree on %q", view)
		}
		if err != nil {
			continue
		}
		out, err := splice(raw, view, search, at, nu, all)
		var f *failure
		if errors.As(err, &f) && f.text == invalidBytesText {
			refusedSpans++
			continue
		}
		if err != nil || validUTF8([]byte(out)) != want || invalid(out) != invalid(raw) {
			t.Fatalf("splice(%q, %q -> %q, all %v) = %q, %v; it reads %q, want %q; invalid bytes %d, had %d",
				raw, old, nu, all, out, err, validUTF8([]byte(out)), want, invalid(out), invalid(raw))
		}
		edited++
	}
	if edited < 3000 || refusedSpans == 0 {
		t.Fatalf("edited %d, refused %d: the generator is not reaching both sides", edited, refusedSpans)
	}
}

// TestLevenshteinChecksCancelFirst: a cancelled search allocates nothing
// for a candidate, however long its lines (review finding: a huge line
// reached the rune slices before the first check). The live context,
// which does allocate, is the negative control.
func TestLevenshteinChecksCancelFirst(t *testing.T) {
	long, longer := strings.Repeat("x", 1<<20), strings.Repeat("x", 1<<20)+"y"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n := testing.AllocsPerRun(5, func() { levenshtein(ctx, long, longer) }); n != 0 {
		t.Fatalf("a cancelled levenshtein allocated %v times", n)
	}
	// Large enough that the rows cannot live on the stack.
	a, b := strings.Repeat("abcdefgh", 64), strings.Repeat("abcdefgi", 64)
	if n := testing.AllocsPerRun(1, func() { levenshtein(context.Background(), a, b) }); n == 0 {
		t.Fatal("the negative control allocated nothing: the measure is broken")
	}
}

// TestReplaceCapsTheResult: replaceAll multiplies newString, so a result
// over 10 MiB is refused before it is built; exactly 10 MiB is made, the
// negative control. (One replacement always fits: a 5 MiB file and a 5 MiB
// newString.)
func TestReplaceCapsTheResult(t *testing.T) {
	content := strings.Repeat("a", 1<<20)
	refused(t, content, "a", strings.Repeat("b", 11), true, resultTooLargeText)
	if got := replaced(t, content, "a", strings.Repeat("b", 10), true); len(got) != maxEditResultBytes {
		t.Fatalf("result is %d bytes", len(got))
	}
}

// TestTextCostsFourBytesALine: the replacers' view of a string holds only
// line offsets, so a file or an oldString of nothing but newlines — each up
// to 5 MiB — does not cost a string header per byte, as strings.Split's
// lines do (the negative control, which shows the measure works).
func TestTextCostsFourBytesALine(t *testing.T) {
	s := strings.Repeat("\n", 1<<20)
	allocated := func(f func()) uint64 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	var keep any
	if n := allocated(func() { keep = newText(s) }); n > 5<<20 {
		t.Fatalf("newText of 1 Mi lines allocated %d bytes, want about 4 MiB", n)
	}
	if n := allocated(func() { keep = strings.Split(s, "\n") }); n < 16<<20 {
		t.Fatalf("the negative control allocated only %d bytes", n)
	}
	_ = keep
}

// failureClass is the class a replace refusal carries.
func failureClass(err error) tool.ErrorClass {
	var f *failure
	if errors.As(err, &f) {
		return f.class
	}
	return ""
}

// TestReplaceClasses: the refusals the model can fix by changing its input
// are invalid_input; a match that fails is a tool error.
func TestReplaceClasses(t *testing.T) {
	for _, tc := range []struct {
		content, old, new string
		class             tool.ErrorClass
	}{
		{"x", "same", "same", tool.ClassInvalidInput},
		{"x", "", "y", tool.ClassInvalidInput},
		{"x", "nope", "y", tool.ClassToolError},
		{"a\na", "a", "y", tool.ClassToolError},
		{"a\nb\nc\nd", `a\nb\nc\nd`, "y", tool.ClassToolError},
	} {
		_, _, err := replace(context.Background(), tc.content, tc.old, tc.new, false)
		if got := failureClass(err); got != tc.class {
			t.Errorf("replace(%q, %q) class = %q (%v), want %q", tc.content, tc.old, got, err, tc.class)
		}
	}
}
