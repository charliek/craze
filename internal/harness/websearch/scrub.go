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
	for _, re := range s.patterns {
		v = re.ReplaceAllLiteralString(v, redact.Marker)
	}
	for _, f := range s.forms {
		v = strings.ReplaceAll(v, f, redact.Marker)
	}
	return v
}

// valid is v as text a scrub sees whole: invalid UTF-8 dropped, and so is
// U+FFFD, which a JSON decoder puts in an invalid byte's place. Either, left
// inside a key, would hide it from the scrub, and dropped after it would join
// the key again (messagesapi's own valid, which is unexported).
func valid(v string) string {
	v = strings.ToValidUTF8(v, "")
	return strings.ReplaceAll(v, string(utf8.RuneError), "")
}
