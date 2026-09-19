package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/version"
)

// A session's journal (plan 020 §3.4–3.5) is built with its event log, in
// newSession and newNative, when Options.JournalDir is set. Its header is
// fixed there, at construction: the workspace made absolute by the rule Start
// applies later, the agent binary as requested, and the options that shape
// the session. What only Start learns — the provider's session id, and the
// binary Start resolved and spawned — goes in the session note (noteSession)
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
//
// Start applies it again later, so a process that changed its working
// directory in between would journal a path the agent does not run in.
// Accepted: craze never chdirs, and every production caller settles the
// workspace once before the session is built — internal/cli/tui.go and
// prompt.go both go through resolveWorkspace — so the rule applied here and
// the one applied in Start name the same directory.
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
// Start resolved and ran. It is a Note, so it never blocks; the caller must not
// hold s.mu (plan 020 §3.5: no note is ever written under it).
//
// Both sessions read the id under their lock and note it after releasing it
// (live.go's two call sites, native.go's), so a Close that cuts the log's note
// admission in that window drops this note. Accepted: the drop is counted in
// the log's health (EventLogHealth.NotesDropped), the file's closing diag is
// still the last line, and it happens only when a session is being closed
// concurrently with its own start — a session nobody will read a journal for.
// Holding s.mu across the note to close the window is the one thing §3.5
// forbids, because a note written under it would put the journal's queue
// inside the session's lock.
func (l *EventLog) noteSession(n journal.SessionNote) {
	if l.journal == nil {
		return
	}
	l.Note(n)
}

// The prompt_end error classes (plan 020 §3.3–3.4). This is the second of the
// two class tables, and deliberately not the codec's EventErrClass: that one
// names what reaches Event.Err on the wire (RPC codes, HTTP statuses, an
// agent's exit), while these name how a prompt attempt ended for its caller —
// the sentinels of session.go and queue.go, which are return values and emit
// nothing. A turn the agent failed is other here, with its message kept; its
// classified form is on the EventError the turn emitted.
//
// What each path can return, and the class it lands in:
//
//	Begin refused, native's duplicate continuation      prompt_in_flight
//	the wire refused it: another prompt in flight       prompt_in_flight
//	the wire refused it: the agent's own turn is on     foreign_turn
//	cancelled in the catalog wait, or withdrawn         prompt_cancelled
//	Interject with no turn to merge into                not_in_turn
//	Interject on a provider without it, native's        unsupported
//	the connection or the harness closed under the turn closed
//	Close ending an attempt that was still open         closed
//	the caller's own context ended the turn             caller_ended
//	a queue refusal (no prompt path reaches one today)  queue_full,
//	                                                    queue_text_too_long
//	anything else, message kept: a turn the agent
//	failed, a session not started, a spawn that failed  other
const (
	promptEndPromptInFlight   = "prompt_in_flight"
	promptEndPromptCancelled  = "prompt_cancelled"
	promptEndForeignTurn      = "foreign_turn"
	promptEndUnsupported      = "unsupported"
	promptEndNotInTurn        = "not_in_turn"
	promptEndClosed           = "closed"
	promptEndCallerEnded      = "caller_ended"
	promptEndQueueFull        = "queue_full"
	promptEndQueueTextTooLong = "queue_text_too_long"
	promptEndOther            = "other"
)

// promptErrClass is err's class for a prompt_end or a start_failed note, "" for
// no error. The order is the table above: every case is a distinct sentinel, so
// only the wrapping decides which matches — a native turn error carries the
// harness's typed cause, and phraseTurnError's wrapper keeps errors.Is working
// through it.
func promptErrClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrPromptInFlight):
		return promptEndPromptInFlight
	case errors.Is(err, ErrPromptCancelled):
		return promptEndPromptCancelled
	case errors.Is(err, ErrForeignTurn):
		return promptEndForeignTurn
	case errors.Is(err, ErrUnsupported):
		return promptEndUnsupported
	case errors.Is(err, ErrNotInTurn):
		return promptEndNotInTurn
	case errors.Is(err, acp.ErrClosed), errors.Is(err, harness.ErrClosed):
		return promptEndClosed
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return promptEndCallerEnded
	case errors.Is(err, ErrQueueFull):
		return promptEndQueueFull
	case errors.Is(err, ErrQueueTextTooLong):
		return promptEndQueueTextTooLong
	}
	return promptEndOther
}

// noteStartFailed records a Start that failed, with the class and message of
// what failed it. Both sessions write it before they tear down what Start
// built (plan 020 §3.5): Close is the log's admission cutoff, and a note
// written after it would be counted and dropped.
//
// A Close from another goroutine can still cut the admission first, and then
// this note is dropped — accepted for the same reason as the session note
// above, and counted the same way.
func (l *EventLog) noteStartFailed(err error) {
	if l.journal == nil || err == nil {
		return
	}
	l.Note(journal.DiagNote{Kind: journal.DiagStartFailed, Fields: map[string]any{
		"errClass":   promptErrClass(err),
		"errMessage": err.Error(),
	}})
}
