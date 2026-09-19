package opencode

import (
	"context"
	"iter"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/tool"
)

// This file is the edit tool's matching: opencode's replace and its nine
// replacers (edit.ts:217-737), ported literally. opencode's own header names
// where the approaches come from: cline's diff-apply evals and gemini-cli's
// editCorrector.
//
// match tries the replacers in order. Each yields the spans of the file it
// takes oldString to mean; the first span that occurs in the file exactly
// once — or at all, with replaceAll — is the one replaced, and a span that
// occurs more than once passes to the replacer's next span and then to the
// next replacer. splice then makes the replacement. The port differs from
// opencode only as NOTICE records:
//
//   - lengths count runes where opencode counts UTF-16 units: Levenshtein
//     distances and line lengths in block-anchor's similarity, and the
//     trimmed lengths in isDisproportionateMatch;
//   - replaceAll replaces literally, where JavaScript's replaceAll expands
//     $& and the like in newString (edit.ts:715);
//   - an empty span is no match (see match);
//   - the search stops when ctx is done;
//   - an edit that would take in or re-join bytes that are not valid UTF-8
//     is refused, and so is a result over 10 MiB (see splice).
//
// Offsets are bytes where opencode's are UTF-16 units, which changes
// nothing: every offset is taken from and applied to the same string.
// Whitespace is JavaScript's (jsSpace), not Go's, wherever opencode trims a
// string or matches \s.

// The similarity a block-anchor candidate needs (edit.ts:219-221).
const (
	singleCandidateSimilarityThreshold    = 0.65
	multipleCandidatesSimilarityThreshold = 0.65
)

// replace's refusals, opencode's words (edit.ts:683-729).
const (
	identicalText     = "No changes to apply: oldString and newString are identical."
	emptyOldText      = "oldString cannot be empty when editing an existing file. Provide the exact text to replace, or use write for an intentional full-file replacement."
	disproportionText = "Refusing replacement because the matched span is much larger than oldString. Re-read the file and provide the full exact oldString for the intended replacement."
	notFoundText      = "Could not find oldString in the file. It must match exactly, including whitespace, indentation, and line endings."
	multipleText      = "Found multiple matches for oldString. Provide more surrounding context to make the match unique."
)

// splice's refusals, craze's own: an edit that would change how bytes read
// showed as U+FFFD come out, and a result past the cap.
const (
	invalidBytesText   = "The text oldString matched contains, or borders on, bytes that are not valid UTF-8, which read shows as U+FFFD; edit cannot keep them exactly. Use bash to edit this part of the file."
	resultTooLargeText = "The edit would make the file larger than 10 MiB; use bash"
)

// replacer is opencode's Replacer (edit.ts:217): the spans of the content it
// takes find to mean, in the order it finds them. Each is either a substring
// of the content or a string replace looks for in it. A replacer ends its
// sequence early when ctx is done; replace then reports the cancel.
type replacer func(ctx context.Context, t *text, find string) iter.Seq[string]

// replacers in the order replace tries them (edit.ts:694-704), which is not
// the order edit.ts defines them in.
var replacers = []replacer{
	simple,
	lineTrimmed,
	blockAnchor,
	whitespaceNormalized,
	indentationFlexible,
	escapeNormalized,
	trimmedBoundary,
	contextAware,
	multiOccurrence,
}

// match is opencode's replace (edit.ts:682-729) up to its decision: search,
// the span oldString is taken to mean, and at, the offset of its first
// occurrence in content. Without replaceAll that occurrence is the only one;
// with it, every occurrence is to be replaced. splice applies the decision.
//
// One difference: a replacer's empty span is skipped. opencode counts it as
// found, because indexOf("") is 0; the replacers that trim yield one for a
// whitespace-only oldString (a blank line, or the trimmed oldString itself).
// With replaceAll, opencode then puts newString between every character of
// the file; without it, an empty span can only ever be ambiguous, or, in an
// empty file, replace nothing with newString. An empty span locates
// nothing, so here it is no match.
func match(ctx context.Context, content, oldString, newString string, replaceAll bool) (search string, at int, err error) {
	if oldString == newString {
		return "", 0, fail(tool.ClassInvalidInput, identicalText)
	}
	if oldString == "" {
		return "", 0, fail(tool.ClassInvalidInput, emptyOldText)
	}
	t := newText(content)
	notFound := true
	for _, r := range replacers {
		for search := range r(ctx, t, oldString) {
			if err := ctx.Err(); err != nil {
				return "", 0, err
			}
			if search == "" {
				continue
			}
			index := strings.Index(content, search)
			if index == -1 {
				continue
			}
			notFound = false
			if isDisproportionateMatch(search, oldString) {
				return "", 0, fail(tool.ClassToolError, disproportionText)
			}
			if replaceAll || index == strings.LastIndex(content, search) {
				return search, index, nil
			}
		}
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
	}
	if notFound {
		return "", 0, fail(tool.ClassToolError, notFoundText)
	}
	return "", 0, fail(tool.ClassToolError, multipleText)
}

// replace is opencode's replace whole: content with match's span replaced
// by newString, every occurrence of it with replaceAll, and at, the offset
// of the first.
func replace(ctx context.Context, content, oldString, newString string, replaceAll bool) (out string, at int, err error) {
	search, at, err := match(ctx, content, oldString, newString, replaceAll)
	if err != nil {
		return "", 0, err
	}
	out, err = splice(content, content, search, at, newString, replaceAll)
	return out, at, err
}

// splice replaces search, found at offset at of view, with newString in raw
// — and, with all, every later occurrence that does not overlap, as
// strings.ReplaceAll finds them — and returns raw so edited. view is raw as
// read shows it (validUTF8); it is raw itself when raw is valid UTF-8.
// Every offset is found in view, where match judged the spans, and mapped
// to the same place in raw; nothing else of raw changes, so its invalid
// bytes outside the spans are kept.
//
// In a file that is not valid UTF-8 two more things are refused, with
// invalidBytesText. A span holding invalid bytes: the model saw them as
// U+FFFD and newString cannot give them back, so the edit would change
// bytes it may have meant to keep. And a result that does not read, through
// validUTF8, exactly as the same edit of view: deleting the text between
// two invalid fragments can join them into a valid character the model
// never saw ("\xe2\x82" and "\x80" around a "b" become U+2080). So every
// invalid byte survives, still reads as U+FFFD, and the rest reads as the
// model's edit of what it read.
//
// It refuses a result over maxEditResultBytes before building it: a single
// replacement fits by the input caps, but replaceAll multiplies newString.
func splice(raw, view, search string, at int, newString string, all bool) (string, error) {
	n := int64(1)
	if all {
		n = int64(strings.Count(view, search))
	}
	size := int64(len(raw)) + n*(int64(len(newString))-int64(len(search)))
	if size > maxEditResultBytes {
		return "", fail(tool.ClassOutputLimit, resultTooLargeText)
	}
	identical := len(raw) == len(view) // validUTF8 lengthens raw by two bytes per invalid byte
	off := offsets{raw: raw}
	var b, vb strings.Builder // raw edited, and view edited when they differ
	b.Grow(int(max(size, 0)))
	copied, viewCopied := 0, 0 // raw[:copied] is in b, view[:viewCopied] in vb
	for i := at; ; {
		start, end := i, i+len(search)
		if !identical {
			var startOK, endOK bool
			start, startOK = off.rawAt(i)
			end, endOK = off.rawAt(i + len(search))
			if !startOK || !endOK || !utf8.ValidString(raw[start:end]) {
				return "", fail(tool.ClassToolError, invalidBytesText)
			}
			vb.WriteString(view[viewCopied:i])
			vb.WriteString(newString)
			viewCopied = i + len(search)
		}
		b.WriteString(raw[copied:start])
		b.WriteString(newString)
		copied = end
		if !all {
			break
		}
		next := strings.Index(view[i+len(search):], search)
		if next < 0 {
			break
		}
		i += len(search) + next
	}
	b.WriteString(raw[copied:])
	out := b.String()
	if !identical {
		vb.WriteString(view[viewCopied:])
		if validUTF8([]byte(out)) != vb.String() {
			return "", fail(tool.ClassToolError, invalidBytesText)
		}
	}
	return out, nil
}

// offsets maps offsets in validUTF8(raw) to raw, moving forward only: each
// byte of raw that is not valid UTF-8 is the three bytes of U+FFFD there,
// and everything else is itself.
type offsets struct {
	raw  string
	i, j int // raw[:i] reads as view[:j]
}

// rawAt is the offset in raw of view offset j, which must not be behind the
// last one asked for. ok is false when j falls inside the U+FFFD standing
// for an invalid byte, which has no offset in raw.
func (o *offsets) rawAt(j int) (i int, ok bool) {
	for o.j < j {
		r, size := utf8.DecodeRuneInString(o.raw[o.i:])
		if size == 0 {
			break // past the end of raw: j is not in view
		}
		o.i += size
		if r == utf8.RuneError && size == 1 {
			o.j += utf8.RuneLen(utf8.RuneError)
		} else {
			o.j += size
		}
	}
	return o.i, o.j == j
}

// isDisproportionateMatch is opencode's (edit.ts:731-737): a span with many
// more lines than oldString, or, for an oldString of more than one line, a
// span much longer once both are trimmed. Lengths are runes.
func isDisproportionateMatch(search, oldString string) bool {
	oldLines := strings.Count(oldString, "\n") + 1
	searchLines := strings.Count(search, "\n") + 1
	if searchLines >= max(oldLines+3, oldLines*2) {
		return true
	}
	if oldLines == 1 {
		return false
	}
	s := utf8.RuneCountInString(jsTrim(search))
	o := utf8.RuneCountInString(jsTrim(oldString))
	return s > max(o+500, o*4)
}

// text is a string as the replacers see it: split at "\n", as JavaScript's
// split("\n") splits it. It holds only where each line starts; a line, the
// line trimmed, and a block of lines are slices of the string made when
// asked for. A block is then what opencode's join of the same lines (and
// its sums of line lengths, edit.ts:270-283) produces, and a string of
// nothing but newlines costs four bytes a line rather than a string header
// or two — the file and oldString are both up to 5 MiB.
type text struct {
	s      string
	starts []int32 // starts[k] is where line k begins; s is under 2 GiB
}

func newText(s string) *text {
	starts := make([]int32, 1, strings.Count(s, "\n")+1)
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			starts = append(starts, int32(i+1))
		}
	}
	return &text{s: s, starts: starts}
}

// n is the number of lines: split("\n").length.
func (t *text) n() int { return len(t.starts) }

// end is where line k ends, before its "\n".
func (t *text) end(k int) int {
	if k+1 < len(t.starts) {
		return int(t.starts[k+1]) - 1
	}
	return len(t.s)
}

func (t *text) line(k int) string { return t.s[t.starts[k]:t.end(k)] }

func (t *text) trimmed(k int) string { return jsTrim(t.line(k)) }

// block is lines from through to, inclusive, as the string holds them:
// lines.slice(from, to + 1).join("\n").
func (t *text) block(from, to int) string { return t.s[t.starts[from]:t.end(to)] }

// used is the number of lines a find asks for: all of them, less an empty
// last one — the replacers' "remove the trailing empty line" (edit.ts:252-254,
// 296-298, 596-598), since a find that ends in a newline does not ask for
// an empty line after it.
func (t *text) used() int {
	if n := t.n(); t.line(n-1) == "" {
		return n - 1
	}
	return t.n()
}

// simple is SimpleReplacer (edit.ts:244-246): find itself.
func simple(_ context.Context, _ *text, find string) iter.Seq[string] {
	return func(yield func(string) bool) {
		yield(find)
	}
}

// lineTrimmed is LineTrimmedReplacer (edit.ts:248-286): every block of
// lines equal to find's lines once each line is trimmed.
func lineTrimmed(ctx context.Context, t *text, find string) iter.Seq[string] {
	return func(yield func(string) bool) {
		f := newText(find)
		m := f.used()
		if m == 0 {
			return // find is "", which match never passes
		}
		for i := 0; i <= t.n()-m; i++ {
			if ctx.Err() != nil {
				return
			}
			matches := true
			for j := range m {
				if t.trimmed(i+j) != f.trimmed(j) {
					matches = false
					break
				}
			}
			if matches && !yield(t.block(i, i+m-1)) {
				return
			}
		}
	}
}

// blockAnchor is BlockAnchorReplacer (edit.ts:288-425): for a find of three
// lines or more, a block whose first and last lines match find's once
// trimmed, whose length is within a quarter of find's, and whose middle
// lines are similar enough by Levenshtein distance — the only such block,
// or the most similar of several. The Levenshtein work is the costly part:
// it heeds ctx per candidate and, inside levenshtein, per row.
func blockAnchor(ctx context.Context, t *text, find string) iter.Seq[string] {
	return func(yield func(string) bool) {
		f := newText(find)
		if f.n() < 3 {
			return
		}
		searchBlockSize := f.used()
		firstLineSearch := f.trimmed(0)
		lastLineSearch := f.trimmed(searchBlockSize - 1)
		maxLineDelta := max(1, searchBlockSize/4) // Math.max(1, Math.floor(searchBlockSize * 0.25))

		// Every position where both anchors match.
		type candidate struct{ startLine, endLine int }
		var candidates []candidate
		for i := range t.n() {
			if ctx.Err() != nil {
				return
			}
			if t.trimmed(i) != firstLineSearch {
				continue
			}
			// The first matching last line after this first line, only.
			for j := i + 2; j < t.n(); j++ {
				if t.trimmed(j) == lastLineSearch {
					actualBlockSize := j - i + 1
					if abs(actualBlockSize-searchBlockSize) <= maxLineDelta {
						candidates = append(candidates, candidate{i, j})
					}
					break
				}
			}
		}
		if len(candidates) == 0 {
			return
		}

		// similarity scores a candidate's middle lines against find's. For a
		// single candidate it is the running sum of each line's share and
		// stops as soon as it passes the threshold (edit.ts:334-356); for
		// several it is the plain average (edit.ts:383-401). The expressions
		// are opencode's, in its order, so the floating point is too.
		similarity := func(c candidate, single bool) (float64, bool) {
			actualBlockSize := c.endLine - c.startLine + 1
			linesToCheck := min(searchBlockSize-2, actualBlockSize-2) // middle lines only
			if linesToCheck <= 0 {
				return 1.0, true // no middle lines: the anchors decide
			}
			sim := 0.0
			for j := 1; j < searchBlockSize-1 && j < actualBlockSize-1; j++ {
				originalLine := t.trimmed(c.startLine + j)
				searchLine := f.trimmed(j)
				maxLen := max(utf8.RuneCountInString(originalLine), utf8.RuneCountInString(searchLine))
				if maxLen == 0 {
					continue
				}
				distance, ok := levenshtein(ctx, originalLine, searchLine)
				if !ok {
					return 0, false
				}
				if single {
					sim += (1 - float64(distance)/float64(maxLen)) / float64(linesToCheck)
					if sim >= singleCandidateSimilarityThreshold {
						break
					}
				} else {
					sim += 1 - float64(distance)/float64(maxLen)
				}
			}
			if !single {
				sim /= float64(linesToCheck)
			}
			return sim, true
		}

		if len(candidates) == 1 {
			c := candidates[0]
			sim, ok := similarity(c, true)
			if ok && sim >= singleCandidateSimilarityThreshold {
				yield(t.block(c.startLine, c.endLine))
			}
			return
		}

		best, maxSimilarity := -1, -1.0
		for k, c := range candidates {
			if ctx.Err() != nil {
				return
			}
			sim, ok := similarity(c, false)
			if !ok {
				return
			}
			if sim > maxSimilarity {
				maxSimilarity, best = sim, k
			}
		}
		if maxSimilarity >= multipleCandidatesSimilarityThreshold && best >= 0 {
			yield(t.block(candidates[best].startLine, candidates[best].endLine))
		}
	}
}

// whitespaceNormalized is WhitespaceNormalizedReplacer (edit.ts:427-469):
// a line equal to find once every run of whitespace in both is one space;
// else, in a line that contains find so normalized, the first stretch that
// is find's words with any whitespace between them; and for a find of more
// than one line, every block of as many lines equal to it so normalized.
func whitespaceNormalized(ctx context.Context, t *text, find string) iter.Seq[string] {
	return func(yield func(string) bool) {
		normalizedFind := normalizeWhitespace(find)
		var words *regexp.Regexp
		compiled := false
		for k := range t.n() {
			if ctx.Err() != nil {
				return
			}
			line := t.line(k)
			normalizedLine := normalizeWhitespace(line)
			if normalizedLine == normalizedFind {
				if !yield(line) {
					return
				}
				continue
			}
			// Only a line that does not match whole is searched inside.
			if !strings.Contains(normalizedLine, normalizedFind) {
				continue
			}
			if !compiled {
				words, compiled = wordsPattern(find), true
			}
			if words == nil {
				continue // opencode skips a pattern that does not compile
			}
			if m := words.FindStringIndex(line); m != nil && !yield(line[m[0]:m[1]]) {
				return
			}
		}

		findLines := strings.Count(find, "\n") + 1
		if findLines > 1 {
			for i := 0; i <= t.n()-findLines; i++ {
				if ctx.Err() != nil {
					return
				}
				block := t.block(i, i+findLines-1)
				if normalizeWhitespace(block) == normalizedFind && !yield(block) {
					return
				}
			}
		}
	}
}

// jsSpaceClass is JavaScript's \s as a Go character class: the characters
// jsSpace accepts.
const jsSpaceClass = `[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

// wordsPattern is whitespaceNormalized's regular expression (edit.ts:442-446):
// find's words, trimmed and split on whitespace, each quoted, joined by
// "\s+". It is nil when the pattern does not compile, which opencode's
// try/catch skips.
func wordsPattern(find string) *regexp.Regexp {
	words := strings.FieldsFunc(jsTrim(find), jsSpace)
	if len(words) == 0 {
		words = []string{""} // "".split(/\s+/) is [""]
	}
	for k, w := range words {
		words[k] = regexp.QuoteMeta(w)
	}
	re, err := regexp.Compile(strings.Join(words, jsSpaceClass+"+"))
	if err != nil {
		return nil
	}
	return re
}

// normalizeWhitespace is whitespaceNormalized's text.replace(/\s+/g, " ")
// .trim(). Every other byte is kept as it is, so an invalid UTF-8 byte
// compares as itself.
func normalizeWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if jsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
			}
			inSpace = true
		} else {
			b.WriteString(s[i : i+size])
			inSpace = false
		}
		i += size
	}
	return jsTrim(b.String())
}

// indentationFlexible is IndentationFlexibleReplacer (edit.ts:471-497):
// every block of as many lines as find that equals it once each has its
// common indentation removed.
func indentationFlexible(ctx context.Context, t *text, find string) iter.Seq[string] {
	return func(yield func(string) bool) {
		normalizedFind := removeIndentation(find)
		findLines := strings.Count(find, "\n") + 1
		for i := 0; i <= t.n()-findLines; i++ {
			if ctx.Err() != nil {
				return
			}
			block := t.block(i, i+findLines-1)
			if removeIndentation(block) == normalizedFind && !yield(block) {
				return
			}
		}
	}
}

// removeIndentation is indentationFlexible's helper (edit.ts:472-485): the
// smallest leading whitespace of the lines that are not blank is cut from
// each of them; blank lines stay as they are. Whitespace is counted in
// runes, which for JavaScript's whitespace, all in the Basic Multilingual
// Plane, is its count in UTF-16 units.
func removeIndentation(s string) string {
	lines := strings.Split(s, "\n")
	minIndent := -1
	for _, line := range lines {
		if jsTrim(line) == "" {
			continue
		}
		if n := leadingSpace(line); minIndent < 0 || n < minIndent {
			minIndent = n
		}
	}
	if minIndent < 0 {
		return s
	}
	for k, line := range lines {
		if jsTrim(line) != "" {
			lines[k] = dropRunes(line, minIndent)
		}
	}
	return strings.Join(lines, "\n")
}

// leadingSpace is the number of runes of whitespace s starts with.
func leadingSpace(s string) int {
	n := 0
	for _, r := range s {
		if !jsSpace(r) {
			break
		}
		n++
	}
	return n
}

// dropRunes is s less its first n runes.
func dropRunes(s string, n int) string {
	i := 0
	for ; n > 0 && i < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[i:]
}

// escapeNormalized is EscapeNormalizedReplacer (edit.ts:499-546): find with
// its backslash escapes resolved, when the content holds that; and every
// block of as many lines as that which, its own escapes resolved, equals it.
func escapeNormalized(ctx context.Context, t *text, find string) iter.Seq[string] {
	return func(yield func(string) bool) {
		if ctx.Err() != nil {
			return
		}
		unescapedFind := unescapeString(find)
		if strings.Contains(t.s, unescapedFind) && !yield(unescapedFind) {
			return
		}
		findLines := strings.Count(unescapedFind, "\n") + 1
		for i := 0; i <= t.n()-findLines; i++ {
			if ctx.Err() != nil {
				return
			}
			block := t.block(i, i+findLines-1)
			if unescapeString(block) == unescapedFind && !yield(block) {
				return
			}
		}
	}
}

// unescapeString is escapeNormalized's helper (edit.ts:500-525), the global
// replace of /\\(n|t|r|'|"|`|\\|\n|\$)/: a backslash before n, t or r is
// that control character, and before ', ", `, \, a newline or $ is dropped.
// Any other backslash stays. The scan resumes after each replacement, as a
// global replace does. Every byte it looks at is ASCII, which is never part
// of a multi-byte UTF-8 sequence, so it scans bytes.
func unescapeString(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			switch next := s[i+1]; next {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case 't':
				b.WriteByte('\t')
				i++
				continue
			case 'r':
				b.WriteByte('\r')
				i++
				continue
			case '\'', '"', '`', '\\', '\n', '$':
				b.WriteByte(next)
				i++
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// trimmedBoundary is TrimmedBoundaryReplacer (edit.ts:562-586): for a find
// with whitespace at either end, find trimmed, when the content holds it;
// and every block of as many lines as find that, trimmed, equals it.
func trimmedBoundary(ctx context.Context, t *text, find string) iter.Seq[string] {
	return func(yield func(string) bool) {
		trimmedFind := jsTrim(find)
		if trimmedFind == find || ctx.Err() != nil {
			return // already trimmed: nothing to try
		}
		if strings.Contains(t.s, trimmedFind) && !yield(trimmedFind) {
			return
		}
		findLines := strings.Count(find, "\n") + 1
		for i := 0; i <= t.n()-findLines; i++ {
			if ctx.Err() != nil {
				return
			}
			block := t.block(i, i+findLines-1)
			if jsTrim(block) == trimmedFind && !yield(block) {
				return
			}
		}
	}
}

// contextAware is ContextAwareReplacer (edit.ts:588-644): for a find of
// three lines or more, a block that starts and ends with find's first and
// last lines once trimmed, has as many lines, and has at least half of its
// middle lines that are not both blank equal to find's once trimmed. For
// each first-line match only the first last-line match after it is tried.
func contextAware(ctx context.Context, t *text, find string) iter.Seq[string] {
	return func(yield func(string) bool) {
		f := newText(find)
		if f.n() < 3 {
			return // too little context to anchor on
		}
		findLines := f.used()
		firstLine := f.trimmed(0)
		lastLine := f.trimmed(findLines - 1)

		for i := range t.n() {
			if ctx.Err() != nil {
				return
			}
			if t.trimmed(i) != firstLine {
				continue
			}
			for j := i + 2; j < t.n(); j++ {
				if t.trimmed(j) != lastLine {
					continue
				}
				if j-i+1 == findLines {
					matchingLines, totalNonEmptyLines := 0, 0
					for k := 1; k < j-i; k++ {
						blockLine := t.trimmed(i + k)
						findLine := f.trimmed(k)
						if blockLine != "" || findLine != "" {
							totalNonEmptyLines++
							if blockLine == findLine {
								matchingLines++
							}
						}
					}
					if totalNonEmptyLines == 0 || float64(matchingLines)/float64(totalNonEmptyLines) >= 0.5 {
						if !yield(t.block(i, j)) {
							return
						}
					}
				}
				break // only the first last-line match
			}
		}
	}
}

// multiOccurrence is MultiOccurrenceReplacer (edit.ts:548-560): find once
// for each place it occurs, not overlapping, for replace to judge.
func multiOccurrence(ctx context.Context, t *text, find string) iter.Seq[string] {
	return func(yield func(string) bool) {
		start := 0
		for ctx.Err() == nil {
			index := strings.Index(t.s[start:], find)
			if index == -1 || !yield(find) {
				return
			}
			start += index + len(find)
		}
	}
}

// levenshtein is opencode's levenshtein (edit.ts:226-242) over runes, with
// two rows of the matrix rather than all of it, which gives the same
// distance. ok is false when ctx was done first: it checks before it
// allocates anything, and then once per row, so even two very long lines
// return promptly.
func levenshtein(ctx context.Context, a, b string) (distance int, ok bool) {
	if ctx.Err() != nil {
		return 0, false
	}
	if a == "" || b == "" {
		return max(utf8.RuneCountInString(a), utf8.RuneCountInString(b)), true
	}
	ra, rb := []rune(a), []rune(b)
	prev, cur := make([]int32, len(rb)+1), make([]int32, len(rb)+1)
	for j := range prev {
		prev[j] = int32(j)
	}
	for i := 1; i <= len(ra); i++ {
		if ctx.Err() != nil {
			return 0, false
		}
		cur[0] = int32(i)
		for j := 1; j <= len(rb); j++ {
			cost := int32(1)
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return int(prev[len(rb)]), true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// jsSpace reports whether r is whitespace to JavaScript: what String.trim
// removes and a regular expression's \s matches (ECMA-262's WhiteSpace and
// LineTerminator). It is not unicode.IsSpace: U+FEFF is whitespace here and
// U+0085 is not.
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// jsTrim is JavaScript's String.prototype.trim.
func jsTrim(s string) string { return strings.TrimFunc(s, jsSpace) }

// The line-ending helpers of edit.ts:22-33. edit matches and writes in the
// file's own ending: "\r\n" if the file has one anywhere, else "\n".
func normalizeLineEndings(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

func detectLineEnding(s string) string {
	if strings.Contains(s, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

func convertToLineEnding(s, ending string) string {
	if ending == "\n" {
		return s
	}
	return strings.ReplaceAll(s, "\n", "\r\n")
}
