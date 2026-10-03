package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/acp"
)

// A failed start in the agent's own words (plan 035 C7, SF-125, P4): an ACP
// session whose Options.StartErrAgentStderr is set — craze serve's, and only
// its — and whose start failed because its agent exited carries the agent's
// last stderr lines in its error, and then Options.StartErrExitHint. The text
// rides the error everywhere a host's start failure goes (Engine.Started's
// state, an attach's data.cause, the ready notification's err, the hub's
// start_failed), so it is kept to one line: every hop after the host takes
// the first line only. craze prompt and the in-process TUI do not set it, and
// their start failures read exactly as they did.

// startErrStderrWait bounds how long a failed start waits for the copy of its
// agent's stderr to reach its end: acp's stderrDrain, the reaper's own bound
// for the same copy. The agent's last line can still be in the pipe when its
// exit fails the start's call.
const startErrStderrWait = 500 * time.Millisecond

// startErrTailMax is how much of the agent's stderr a start error carries:
// the last non-blank lines that fit 512 bytes, sanitized and folded onto one
// line.
const startErrTailMax = 512

// startErrTailSep is what joins the kept lines.
const startErrTailSep = " / "

// decorateStartErr is err as Start returns it and journals it: unchanged,
// unless the session opted in (Options.StartErrAgentStderr) and err's chain
// holds an *acp.ExitError — the agent exited. Then it is err wrapped with the
// agent's last stderr lines (stderrTailLine), unless they are empty or err
// already says them, and then with StartErrExitHint. Wrapped, never
// replaced: errors.As still finds the exit.
func (s *session) decorateStartErr(err error) error {
	if !s.opts.StartErrAgentStderr {
		return err
	}
	var exit *acp.ExitError
	if !errors.As(err, &exit) {
		return err
	}
	var raw []byte
	if c := s.clientRef(); c != nil {
		raw = c.StderrTail(startErrStderrWait)
	}
	return withAgentWords(err, stderrTailLine(raw), s.opts.StartErrExitHint)
}

// withAgentWords is err with the agent's folded stderr tail after ": ", when
// there is one that err does not already say, then hint as it is: on the
// same line, both.
func withAgentWords(err error, tail, hint string) error {
	if tail != "" && !strings.Contains(err.Error(), tail) {
		err = fmt.Errorf("%w: %s", err, tail)
	}
	if hint != "" {
		err = fmt.Errorf("%w%s", err, hint)
	}
	return err
}

// stderrTailLine folds the raw end of an agent's stderr onto one line: split
// on "\n" — a last fragment with no newline counts as a line — each line's
// trailing "\r" trimmed and the line sanitized (sanitizeLine: escape
// sequences and control characters gone, invalid UTF-8 replaced, whitespace
// collapsed); blank lines dropped; the last lines that fit startErrTailMax
// bytes kept, joined by " / ". A last line longer than that alone is cut at
// the cap, on a rune boundary. "" for nothing to say.
func stderrTailLine(raw []byte) string {
	lines := strings.Split(string(raw), "\n")
	var kept []string
	size := 0
	for i := len(lines) - 1; i >= 0; i-- {
		line := sanitizeLine(strings.TrimSuffix(lines[i], "\r"))
		if line == "" {
			continue
		}
		grown := size + len(line)
		if len(kept) > 0 {
			grown += len(startErrTailSep)
		}
		if grown > startErrTailMax {
			if len(kept) == 0 {
				kept = append(kept, cutRunes(line, startErrTailMax))
			}
			break
		}
		kept = append(kept, line)
		size = grown
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return strings.Join(kept, startErrTailSep)
}

// cutRunes is s cut to at most n bytes, on a rune boundary.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
