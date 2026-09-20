package cli

import (
	"fmt"
	"io"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/tui"
)

// compatClaude is craze's [compat.claude] table as a session's Options carry
// it (plan 022 §3.5): which classes of Claude's own content a native session
// reads. Every key defaults to true, so a run with no config file, or with no
// such table, gets everything — and a value that is not a bool gets the
// default and one line on craze's own lane, because a typo there would
// otherwise look like craze having lost the user's instructions.
//
// diag is the same lane journalDir writes on: the TUI's deferred craze
// stream, headless craze's stderr. Unlike the journal's directory this is read
// at each session the run builds rather than once, since a run that rebuilds a
// session after a provider switch is a run that should see the config as it is
// then; the only cost is that a config with a typo in it says so once per
// session rather than once per run.
func compatClaude(diag io.Writer) agent.ClaudeCompat {
	c, why := tui.ConfigCompatClaude()
	for _, line := range why {
		if diag != nil {
			fmt.Fprintf(diag, "craze: %s\n", line)
		}
	}
	return c
}
