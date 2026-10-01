package transcript

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/charliek/craze/internal/agent"
)

// The wordings of the rows the fold draws from an event, with the TUI's
// exact text (internal/tui/app.go and transcript.go at plan 024's baseline):
// every client folding the same event writes the same row.
const (
	// NoteCancelled is the row a cancelled turn leaves: EventDone with the
	// stop reason "cancelled", or the engine's synthetic cancelled ending.
	NoteCancelled = "cancelled"
	// NoteForeignTurn heads the stream of a turn the agent started on its own:
	// grok's interjection fallback, and every foreign turn whose reason this
	// build does not know (noteForForeignTurn).
	NoteForeignTurn = "agent continued on its own (interjection fallback)"
	// NoteSubagentWake heads the stream of the native session's wake: a turn
	// of its own delivering a background sub-agent's result (plan 026 §3.11,
	// agent.ReasonSubagentWake).
	NoteSubagentWake = "sub-agent finished — the agent continues"
	// NoteRestored closes a session/load replay: everything above it is
	// history the agent handed back, everything below is this session.
	NoteRestored = "restored"
	// CommandLineMark leads the provenance line under a user entry craze
	// expanded a plugin command or skill into.
	CommandLineMark = "⤷ "
	// TrimmedNote is the row a client draws above a transcript whose oldest
	// entries were dropped (Trimmed). It is the client's row, not an entry;
	// the wording lives here so every client draws the same one.
	TrimmedNote = "… earlier transcript trimmed"
	// CompactingLabel is what a client's working line reads while a
	// transcript has a compaction open (Transcript.Compacting, plan 028
	// §3.13). Like TrimmedNote it is the client's to draw, not an entry; the
	// wording lives here so every client draws the same one.
	CompactingLabel = "compacting context…"
)

// The compaction notes (plan 028 §3.13): the row a compaction's ended draws,
// by its reason, each followed by " · <before> → <after> tokens"
// (compactionNote), and the failure's, followed by ": <why>".
const (
	NoteCompacted          = "context compacted"
	NoteCompactedOnRequest = "context compacted on request"
	NoteCompactedOverflow  = "context was too large — compacted"
	NoteCompactionFailed   = "compaction failed"
)

// compactionNote is the row an ended compaction draws: how it went, and what
// it did to the context's estimated size — `context compacted · 890k → 21k
// tokens`, worded by the reason (a reason this build does not know reads as
// an automatic one: it is still a compaction) — or why it failed,
// `compaction failed: <err>`, the error folded onto one line.
func compactionNote(c *agent.CompactionInfo) string {
	if c.Err != "" {
		if why := sanitizeLine(c.Err); why != "" {
			return NoteCompactionFailed + ": " + why
		}
		return NoteCompactionFailed
	}
	head := NoteCompacted
	switch c.Reason {
	case agent.CompactionManual:
		head = NoteCompactedOnRequest
	case agent.CompactionOverflow:
		head = NoteCompactedOverflow
	}
	return head + " · " + tokenCount(c.TokensBefore) + " → " + tokenCount(c.TokensAfter) + " tokens"
}

// TokenCount is a token count as craze says one anywhere it is drawn — the
// compaction note's "890k → 21k tokens" and the status row's usage part
// (plan 028 §3.14) alike: tokenCount's rule, below.
func TokenCount(n int64) string { return tokenCount(n) }

// tokenCount is a token count as the compaction note says it: the number
// itself under a thousand, else in k or M to three significant figures with
// trailing zeros dropped — 850, 1.23k, 12.3k, 890k, 1.21M, 2M. A negative
// count, which no harness reports, reads as 0.
func tokenCount(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(max(n, 0), 10)
	}
	v, unit := float64(n)/1e3, "k"
	if n >= 1e6 {
		v, unit = float64(n)/1e6, "M"
	}
	digits := 0
	switch {
	case v < 10:
		digits = 2
	case v < 100:
		digits = 1
	}
	s := strconv.FormatFloat(v, 'f', digits, 64)
	if unit == "k" {
		if r, err := strconv.ParseFloat(s, 64); err == nil && r >= 1000 {
			// 999,600 rounds to "1000k": it is a million.
			s, unit = "1", "M"
		}
	}
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s + unit
}

// stopCancelled is the one stop reason the fold reads: EventDone's, and a
// synthetic TurnEnded's.
const stopCancelled = "cancelled"

// noteForForeignTurn is the note a foreign turn's start draws, by the reason the
// bracket carries (agent.ForeignTurnInfo.Reason): the native wake's own
// wording for agent.ReasonSubagentWake, and today's for "" — and for any
// reason this build does not know, which a newer session may publish: the
// reason is an open string on the wire, and an unknown one is still a turn
// the agent ran on its own.
func noteForForeignTurn(reason string) string {
	if reason == agent.ReasonSubagentWake {
		return NoteSubagentWake
	}
	return NoteForeignTurn
}

// The outcome notes an answered ask's ending draws (foldAsk, plan 032 §3.2
// C4), with the TUI's wording as its cards.go wrote them when they were each
// client's own rows: `? <prompt> → <labels>` per question answered, `? <title>
// → skipped` for a skipped question, and `plan <name> → accepted|rejected`.

// answerNotes is one note per question of q, naming what answers picked for
// it: the options' labels, comma-separated, or "nothing". A question that asks
// nothing draws none.
func answerNotes(q *agent.QuestionEvent, answers map[string][]string) []string {
	out := make([]string, 0, len(q.Questions))
	for _, qq := range q.Questions {
		out = append(out, "? "+sanitizeLine(qq.Prompt)+" → "+answerLabels(qq, answers[qq.ID]))
	}
	return out
}

// answerLabels names the options ids picks, in their order: each one's label
// folded onto one line. An id the question does not offer names nothing, and
// nothing named reads "nothing".
func answerLabels(q agent.Question, ids []string) string {
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		for _, o := range q.Options {
			if o.ID == id {
				labels = append(labels, sanitizeLine(o.Label))
				break
			}
		}
	}
	if len(labels) == 0 {
		return "nothing"
	}
	return strings.Join(labels, ", ")
}

// skipNote is what a skipped question draws.
func skipNote(q *agent.QuestionEvent) string {
	return "? " + questionTitle(q) + " → skipped"
}

// questionTitle names a question request: its title, else its first
// question's prompt, else "question".
func questionTitle(q *agent.QuestionEvent) string {
	if q == nil {
		return ""
	}
	if t := sanitizeLine(q.Title); t != "" {
		return t
	}
	if len(q.Questions) > 0 {
		return sanitizeLine(q.Questions[0].Prompt)
	}
	return "question"
}

// planNote is what an answered plan draws.
func planNote(p *agent.PlanEvent, accepted bool) string {
	verb := "rejected"
	if accepted {
		verb = "accepted"
	}
	return "plan " + PlanName(p) + " → " + verb
}

// PlanName is a plan as craze names it anywhere it is drawn — its outcome
// note here, and the TUI's plan card and plan entry: its name folded onto one
// line, else "plan"; "" for none.
func PlanName(p *agent.PlanEvent) string {
	if p == nil {
		return ""
	}
	if n := sanitizeLine(p.Name); n != "" {
		return n
	}
	return "plan"
}

// ellipsis leads a streamed entry's text once the stream cap dropped its
// beginning (capText).
const ellipsis = "…"

// todosPlannedNote and todosDoneNote are the two todo-stream notes, spelled as
// the TUI's fmt.Sprintf("tasks: %d planned") and ("tasks: %d/%d done").
func todosPlannedNote(n int) string { return "tasks: " + strconv.Itoa(n) + " planned" }

func todosDoneNote(closed, n int) string {
	return "tasks: " + strconv.Itoa(closed) + "/" + strconv.Itoa(n) + " done"
}

// commandLine is the note an EventCommand draws: the mark, the qualified name
// and the kind, each folded onto one line, and nothing at all for a name that
// sanitises to empty — the TUI's addCommandLine, rule for rule.
func commandLine(qualified, kind string) string {
	name := sanitizeLine(qualified)
	if name == "" {
		return ""
	}
	if k := sanitizeLine(kind); k != "" {
		return CommandLineMark + name + " (" + k + ")"
	}
	return CommandLineMark + name
}

// sanitizeLine folds a string onto one line: escape sequences and control
// characters go, and runs of whitespace collapse. It is the TUI's sanitizeLine
// (internal/tui/transcript.go) ported whole, because this package may not
// import the terminal library its ansi.Strip comes from: stripANSI below is
// that function, transcribed.
//
// A name that is already one clean line — printable ASCII words separated by
// single spaces, which is every plugin name craze resolves — comes back as it
// was, without allocating; that fast path returns exactly what the full path
// would.
func sanitizeLine(s string) string {
	if s == "" {
		return ""
	}
	if cleanLine(s) {
		return s
	}
	return strings.Join(strings.Fields(dropControls(stripANSI(s))), " ")
}

// SanitizeLine is sanitizeLine for the packages below the clients that fold a
// string onto one line the TUI's way and may not import a terminal library to
// do it either: internal/engine's IndexTitleLine (plan 030 §3.3), which every
// host writes a session-index title through.
func SanitizeLine(s string) string { return sanitizeLine(s) }

// cleanLine reports whether s is printable ASCII with single inner spaces and
// none at either end: a string sanitizeLine's full path returns unchanged,
// since stripANSI prints every byte 0x20-0x7E in the ground state, nothing in
// it is a control character, and Fields/Join rebuild exactly the same words
// and spaces.
func cleanLine(s string) bool {
	if s[0] == ' ' || s[len(s)-1] == ' ' {
		return false
	}
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			if prevSpace {
				return false
			}
			prevSpace = true
		case c > ' ' && c < 0x7f:
			prevSpace = false
		default:
			return false
		}
	}
	return true
}

// dropControls is the TUI's: newlines, carriage returns and tabs become a
// space, and every other control code point goes — unicode.IsControl, the
// whole Cc category, so a C1 CSI in UTF-8 cannot pass as text.
func dropControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
