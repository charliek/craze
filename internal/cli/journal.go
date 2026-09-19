package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/tui"
)

// journalEnv turns the session journal off for one run (plan 020 §3.5).
const journalEnv = "CRAZE_JOURNAL"

// journalDir is the one place craze decides whether this run's sessions are
// journaled, and where: agent.Options.JournalDir for every session the TUI
// and `craze prompt` build. "" is off. `craze frame` never asks: a golden run
// leaves nothing on disk.
//
// Either switch turns journaling off and neither can turn it on against the
// other: CRAZE_JOURNAL set to a false value, `journal = false` in
// config.toml, or no craze directory at all (paths.JournalDir is "" then,
// and it is absolute even under a relative CRAZE_HOME). The opt-out fails
// closed, because it is a privacy switch: a CRAZE_JOURNAL that
// strconv.ParseBool cannot read, and a config.toml that cannot be read or
// parsed or whose `journal` is not a bool (tui.ConfigJournal), are off too —
// a typo must never journal prompts and tool output the user meant to keep
// off disk. Each of those says so in exactly one line on diag; an explicit
// false is the user's own choice and is silent.
//
// CRAZE_JOURNAL is read trimmed, as CRAZE_HOME is, and empty (or only
// spaces) counts as unset: the config decides.
//
// A run resolves this once, before it builds any session: the TUI's provider
// picker can build several, and the reason it is off is worth saying once.
//
// The two switches below read the environment separately — tui.ConfigJournal
// finds config.toml under CRAZE_HOME, and paths.JournalDir reads CRAZE_HOME
// again for the directory — so an environment that changed between the two
// reads would answer with one craze home's config and another's journal
// directory. Accepted: craze never mutates its own environment or working
// directory after start, and this resolver runs once per run, on the single
// goroutine that sets a run up, before any session exists.
func journalDir(diag io.Writer) string {
	if raw := strings.TrimSpace(os.Getenv(journalEnv)); raw != "" {
		on, err := strconv.ParseBool(raw)
		if err != nil {
			fmt.Fprintf(diag, "craze: journal off: %s=%q is not a bool\n", journalEnv, raw)
			return ""
		}
		if !on {
			return ""
		}
	}
	if on, why := tui.ConfigJournal(); !on {
		if why != "" {
			fmt.Fprintf(diag, "craze: journal off: %s\n", why)
		}
		return ""
	}
	return paths.JournalDir()
}
