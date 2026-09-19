package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/version"
)

// A session's journal (plan 020 §3.4–3.5) is built with its event log, in
// newSession and newNative, when Options.JournalDir is set. Its header is
// fixed there, at construction: the workspace made absolute by the rule Start
// applies later, the agent binary as requested, and the options that shape
// the session. What only Start learns — the provider's session id, and the
// binary the spawn actually resolved — goes in the session note (noteSession)
// once Start has it. The log writes the closing diag itself, in its Close.

// journalHeader is what a session's journal header records beyond what
// Options holds: the provider's name and the binary asked for ("" when the
// provider's own lookup will choose, and always for an in-process provider).
type journalHeader struct {
	provider string
	binary   string
}

// newSessionLog is a session's event log, feeding a journal when
// opts.JournalDir is set. A journal that cannot be built is never a reason to
// refuse the session: it runs without one, and says so in one line on its
// diagnostic writer (diagWriter). The incarnation is minted here either way,
// because the journal's file name and header carry it and the journal is
// built before the log (EventLogOptions.Incarnation).
func newSessionLog(opts Options, h journalHeader) *EventLog {
	inc := NewIncarnation()
	if opts.JournalDir == "" {
		return NewEventLog(EventLogOptions{Incarnation: inc})
	}
	w, err := newSessionJournal(opts, h, inc)
	if err != nil {
		if d := diagWriter(opts); d != nil {
			fmt.Fprintf(d, "craze: %v; this session is not journaled\n", err)
		}
		return NewEventLog(EventLogOptions{Incarnation: inc})
	}
	return NewEventLog(EventLogOptions{Incarnation: inc, Journal: w})
}

// newSessionJournal builds the journal writer. It does no I/O beyond reading
// the working directory: the file appears with the first line worth writing.
func newSessionJournal(opts Options, h journalHeader, inc string) (*journal.Writer, error) {
	cwd, err := absWorkspace(opts.Workspace)
	if err != nil {
		return nil, fmt.Errorf("journal: workspace: %w", err)
	}
	return journal.New(journal.Options{
		Dir:          opts.JournalDir,
		Incarnation:  inc,
		Cwd:          cwd,
		CrazeVersion: version.Version,
		Provider:     h.provider,
		AgentBinary:  h.binary,
		Force:        opts.Force,
		Interactive:  opts.Interactive,
		Mode:         opts.Mode,
		EventCodec:   EventCodecVersion,
		Diag:         diagWriter(opts),
	})
}

// absWorkspace is the rule both sessions apply to Options.Workspace before
// they use it (live Start, nativeWorkspace): the process's working directory
// when none was given, made absolute. The journal applies it at construction,
// so its header names the directory Start will run in.
func absWorkspace(ws string) (string, error) {
	if ws == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		ws = wd
	}
	return filepath.Abs(ws)
}

// diagWriter is where craze's own notes about a session go: Options.Diag,
// falling back to Stderr as the sessions' other notes do (discoverPlugins,
// nativeSession.note). nil when neither is set.
func diagWriter(opts Options) io.Writer {
	if opts.Diag != nil {
		return opts.Diag
	}
	return opts.Stderr
}

// noteSession writes the session note: the provider's session id once Start
// has learned it, the id it was loaded from on a resume, and the agent binary
// the spawn resolved. It is a Note, so it never blocks; the caller must not
// hold s.mu (plan 020 §3.5: no note is ever written under it).
func (l *EventLog) noteSession(n journal.SessionNote) {
	if l.journal == nil {
		return
	}
	l.Note(n)
}

// resolvedBinary is the agent binary acp.Spawn resolved for a session with a
// journal: the same lookup Spawn makes (an explicit binary, else
// CRAZE_AGENT_BIN, else the provider's candidates on PATH), asked again
// because the spawn does not report it. "" without a journal, where nothing
// would record it, and if the lookup no longer finds it.
func (l *EventLog) resolvedBinary(explicit string, candidates []string) string {
	if l.journal == nil {
		return ""
	}
	bin, err := acp.ResolveBinaryCandidates(explicit, candidates)
	if err != nil {
		return ""
	}
	return bin
}
