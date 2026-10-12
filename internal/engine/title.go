package engine

import (
	"strings"

	"github.com/charliek/craze/internal/agent"
)

// NormalizeTitle is a session title as craze keeps one (plan 043 P9): the
// one normalization every place a title enters craze from outside a session
// runs, so that the same words given to any of them become the same title —
// the hub's session.create (internal/hub's parse, which also hashes what this
// returns into the create's identity), craze serve --title (the hidden flag
// the hub and craze new --wait spawn a host with), and craze new --title
// itself; the TUI's /rename folds its argument through it too (slash.go).
//
// It is exactly the title the session will store, and the session index will
// hold (review C7). So it is, in order:
//
//   - the session's own ingestion scrub (agent.SanitizeText, what SetTitle and
//     a new session's seed run): escape sequences and the C0 and C1 controls
//     but newline and tab dropped, the zero-width space, the word joiner and
//     the BOM dropped (not ZWJ or ZWNJ, which are letters and emoji glue), the
//     bidirectional embeddings, overrides and isolates dropped, invalid UTF-8
//     replaced by U+FFFD;
//   - the index's own fold (IndexTitleLine): one line — what escape and
//     control is left dropped, a newline or a tab turned into a space, runs of
//     whitespace collapsed to one space — cut to IndexTitleRunes runes;
//   - the ends trimmed: a cut that lands just after a space would otherwise
//     leave one at the end.
//
// What it returns is a fixed point of all three steps and of itself: the
// session's scrub, the index's fold and a second NormalizeTitle change
// nothing of it (TestNormalizeTitle), so the hub's title, normalized again by
// the craze serve it spawns, seeded and stored by the session and written to
// the index, is one string throughout.
//
// "" is a title that normalizes to nothing — empty, whitespace only, nothing
// but controls, zero-width or bidi runes — which every caller refuses rather
// than treats as none: a client that has no title omits it (session.create's
// "omit, don't send empty"), and one that sent a blank one asked for
// something craze cannot give.
func NormalizeTitle(s string) string {
	return strings.TrimSpace(IndexTitleLine(agent.SanitizeText(s)))
}
