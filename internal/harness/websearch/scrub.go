package websearch

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/redact"
)

// minKeyLen is the shortest key craze accepts, in bytes, after trimming:
// modeltable.MinKeyLen, which this package may not import. A test in the
// harness holds the two rules together (usable against modeltable.KeyProblem).
const minKeyLen = 8

// usable says key, trimmed, is one craze can redact, so one it may send:
// modeltable.KeyProblem's rule (at least minKeyLen bytes, and not overlapping
// the redaction marker), which a value the session's redactor would not hold
// fails. A blank key is no key.
func usable(key string) bool {
	k := strings.TrimSpace(key)
	return len(k) >= minKeyLen && !redact.MarkerOverlaps(k)
}

// scrubber takes the caller's own key out of every text a reply gives (plan
// 051 §B, R1, R2), success and error alike, before anything parses, cuts,
// classifies or returns the text: the key as itself and in every spelling an
// echo can give it — any of its characters JSON-escaped or percent-encoded,
// and split by format characters or invalid bytes (messagesapi.KeyPattern) —
// with each of its spaces also a query string's "+", in any spelling, each
// space chosen on its own (keyPattern); then the key's whole query and path
// encodings as written. It mirrors the Anthropic model list's scrubber
// (internal/anthropicmodels), with redact.Marker, the marker modeltable
// keeps keys from overlapping, in the key's place. The dispatcher's
// exact-value redactor still runs over whatever a tool returns after it.
//
// A nil scrubber (no key, or one too short or overlapping the marker, which
// is never sent) takes nothing out; its text is still made valid.
type scrubber struct {
	patterns []*regexp.Regexp
	forms    []string
}

// newScrubber is the scrubber of key, trimmed; nil for a key that is not
// usable — an empty one above all, whose pattern would match everywhere.
func newScrubber(key string) *scrubber {
	if !usable(key) {
		return nil
	}
	k := strings.TrimSpace(key)
	s := &scrubber{patterns: []*regexp.Regexp{keyPattern(k)}}
	for _, f := range []string{url.QueryEscape(k), url.PathEscape(k)} {
		if f != "" && f != k && !slices.Contains(s.forms, f) {
			s.forms = append(s.forms, f)
		}
	}
	return s
}

// keyPattern is messagesapi.KeyPattern(k) with each space in k matching a
// query string's "+" as well, in any of its spellings, each space on its own:
// an echo may spell one space "+" and the next "%20" or a JSON escape. The
// key is cut at its spaces; each run between them is KeyPattern's, each space
// KeyPattern(" ") or KeyPattern("+"), and KeyGap stands at every join, as it
// does between any two of KeyPattern's characters. A key with no space is
// KeyPattern's pattern exactly.
func keyPattern(k string) *regexp.Regexp {
	if !strings.Contains(k, " ") {
		return messagesapi.KeyPattern(k)
	}
	space := "(?:" + messagesapi.KeyPattern(" ").String() + "|" + messagesapi.KeyPattern("+").String() + ")"
	var b strings.Builder
	for i, run := range strings.Split(k, " ") {
		if i > 0 {
			b.WriteString(messagesapi.KeyGap + space + messagesapi.KeyGap)
		}
		if run != "" {
			b.WriteString(messagesapi.KeyPattern(run).String())
		}
	}
	return regexp.MustCompile(b.String())
}

// text is v made valid (valid) with every spelling of the key replaced by
// redact.Marker.
func (s *scrubber) text(v string) string {
	v = valid(v)
	if s == nil {
		return v
	}
	// Every pattern and form is matched against the text as it is, and the
	// union of what they match is replaced: one replaced before another is
	// matched could leave part of a longer, overlapping key behind (a key
	// that starts with an earlier one, once SetKey has added it).
	var spans [][2]int
	for _, re := range s.patterns {
		for _, m := range re.FindAllStringIndex(v, -1) {
			spans = append(spans, [2]int{m[0], m[1]})
		}
	}
	for _, f := range s.forms {
		for i := 0; ; {
			j := strings.Index(v[i:], f)
			if j < 0 {
				break
			}
			spans = append(spans, [2]int{i + j, i + j + len(f)})
			i += j + 1
		}
	}
	if len(spans) == 0 {
		return v
	}
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	var b strings.Builder
	at := 0
	for i := 0; i < len(spans); {
		start, end := spans[i][0], spans[i][1]
		for i++; i < len(spans) && spans[i][0] < end; i++ {
			end = max(end, spans[i][1])
		}
		b.WriteString(v[at:start])
		b.WriteString(redact.Marker)
		at = end
	}
	b.WriteString(v[at:])
	return b.String()
}

// with is a scrubber that takes out every key s does and key too: s itself
// when key is not usable or s holds it already, and newScrubber(key) when s
// is nil. s is never changed.
func (s *scrubber) with(key string) *scrubber {
	add := newScrubber(key)
	switch {
	case add == nil:
		return s
	case s == nil:
		return add
	}
	if slices.ContainsFunc(s.patterns, func(re *regexp.Regexp) bool { return re.String() == add.patterns[0].String() }) {
		return s
	}
	out := &scrubber{patterns: append(slices.Clone(s.patterns), add.patterns...), forms: slices.Clone(s.forms)}
	for _, f := range add.forms {
		if !slices.Contains(out.forms, f) {
			out.forms = append(out.forms, f)
		}
	}
	return out
}

// valid is v as text a scrub sees whole: invalid UTF-8 dropped, and so is
// U+FFFD, which a JSON decoder puts in an invalid byte's place. Either, left
// inside a key, would hide it from the scrub, and dropped after it would join
// the key again (messagesapi's own valid, which is unexported).
func valid(v string) string {
	v = strings.ToValidUTF8(v, "")
	return strings.ReplaceAll(v, string(utf8.RuneError), "")
}
