package transcript

import (
	"strconv"
	"strings"
	"unicode"
)

// The wordings of the rows the fold draws from an event, with the TUI's
// exact text (internal/tui/app.go and transcript.go at plan 024's baseline):
// every client folding the same event writes the same row.
const (
	// NoteCancelled is the row a cancelled turn leaves: EventDone with the
	// stop reason "cancelled", or the engine's synthetic cancelled ending.
	NoteCancelled = "cancelled"
	// NoteForeignTurn heads the stream of a turn the agent started on its own.
	NoteForeignTurn = "agent continued on its own (interjection fallback)"
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
)

// stopCancelled is the one stop reason the fold reads: EventDone's, and a
// synthetic TurnEnded's.
const stopCancelled = "cancelled"

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
