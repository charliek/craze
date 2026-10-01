package agent

import (
	"strings"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/tool"
)

// The native adapter's background bash jobs (plan 033 §3.8 "Display", P12).
// The harness reports a job — a command an interactive session owns once its
// call has returned, started with run_in_background or promoted at its
// timeout (internal/harness/jobs.go) — as three events on the session's sink:
// JobStarted from the bash call's own goroutine as it hands the command over,
// then, from the job's goroutine, lossy JobOutput snapshots and one
// JobFinished. This file maps them onto what the adapter already publishes for
// a background sub-agent (native_subagents.go), so a client draws a job with
// no new event kind, field or protocol change (discovery bash-jobs.md §2):
//
//   - a roster row: ID and ToolCallID the job's id — its bash call's harness
//     id — SubagentType BashJobType ("bash job"), no model, Background and
//     Transcript set, the command's first line as its Description and the
//     whole command as its Prompt; EventSubagent spawned, then finished, in
//     the sections that change it, with the roster's eviction (subagents.go's
//     rule, which the fold replays);
//   - a child tool set of one execute row, the job's own: started and called
//     with the command, its live output as lossy progress, and closed with
//     what the command left — the exit code a collapsed row previews when it
//     exited — before the roster's finished goes out, as a child's rows are
//     settled before its finished.
//
// So the TUI's row band (the bg mark, the elapsed time, del to stop — the stop
// key reaches the job through CancelSubagent, which the harness routes to it)
// and its child view draw a job as they draw a background child, the engine's
// model and an attached client fold it alike, and `craze prompt --json` never
// meets one (headless sessions run no job, P11).
//
// # What it never does
//
// It never stamps the bash call's row in the parent's set. subagentStarted
// stamps an agent call's row as a task (its Task carries the child's id and
// model), and a bash row stamped so would draw as a task row: the call's row
// keeps its own result, the start or promotion receipt, and the job is read on
// its roster row and in its view (discovery §2's one adapter rule to avoid).
//
// # Statuses
//
// A job's row ends as its harness status says (JobFinished, whose Error is the
// row's word for any ending but a clean exit): exited with 0 is completed;
// exited otherwise is failed, "exit code N"; stopped — by the model, the user
// or the session closing — and timed out at its limit are cancelled, "stopped
// by the agent", "stopped by the user", "stopped: the session closed", "time
// limit reached"; and failed is failed. The row's duration is the job's own
// running time, a promoted command's foreground phase included — the
// duration its delivered result tells the model.
//
// # Payloads and locks
//
// Exactly native_subagents.go's: every row text is put through the session's
// widest redactor, taken before any lock, then sanitized and redacted again
// (nativeSafe), and capped (safeSubagent); the roster changes and its
// enqueue happen in one rosterMu section, the child's tool set is changed
// under toolMu in sections of its own, and the barrier after spawned and
// finished is the same Flush. The output the row draws is redacted the same
// way: a job runs for up to two hours on the redactor its call began with
// (plan 033 P19, R9), and the session may have learned a key since.

// BashJobType is a background bash job's SubagentType on its roster row (plan
// 033 P12): the harness's tool.JobType. Persona and agent-type names are
// slugs, so no sub-agent's type can be spelled with a space, and a client tells
// a job's row from a child's by it — the TUI's spinner and /connect's busy
// check ignore a running job (internal/tui's bashJobRow).
const BashJobType = tool.JobType

// jobStarted is a job starting: its roster row, EventSubagent{spawned}, the
// barrier, and its one execute row opened in its own tool set — and nothing
// stamped on the bash call's row (the file's comment).
func (s *nativeSession) jobStarted(e harness.JobStarted) {
	if e.ID == "" {
		return
	}
	// Taken before any lock (native_subagents.go's redactor).
	safe := nativeSafe{red: s.redactor()}
	cmd := safe.text(e.Command)
	info := SubagentInfo{
		ID:           e.ID,
		ToolCallID:   e.CallID,
		Description:  jobDescription(cmd),
		SubagentType: BashJobType,
		Status:       SubagentRunning,
		Prompt:       cmd,
		StartedAt:    wallClock(e.At, s.Now),
		Transcript:   true,
		Background:   true,
	}
	// The set before the row exists for anyone: the job's goroutine sends
	// nothing before this returns (the harness gates it on JobStarted).
	s.openChildTools(e.ID)

	s.rosterMu.Lock()
	if s.roster == nil {
		s.roster = map[string]*nativeChild{}
	}
	if _, again := s.roster[e.ID]; !again {
		s.rosterOrder = append(s.rosterOrder, e.ID)
	}
	row := &nativeChild{info: info}
	s.roster[e.ID] = row
	safeSubagent(&row.info, safe)
	s.enqueueRosterLocked(SubagentChangeSpawned, row.info)
	s.rosterMu.Unlock()
	s.flushRoster()

	// The view's one row: the command as a foreground bash call's row draws
	// it (its title and its raw input), published directly once spawned has
	// committed, as a child's first event follows its spawned.
	s.toolStarted(e.ID, harness.ToolStarted{ID: e.ID, Step: 1, Tool: tool.BashTool, Kind: tool.KindExecute})
	s.toolCalled(e.ID, harness.ToolCalled{ID: e.ID, At: e.At, Request: harness.ToolRequest{
		Tool: tool.BashTool, Kind: tool.KindExecute, Title: cmd, Command: cmd, Workdir: safe.text(e.Workdir),
	}})
}

// jobProgress is a snapshot of a running job's output on its execute row:
// lossy, as every progress is, and dropped once the row has closed or its set
// has gone with an evicted row (merge's rules).
func (s *nativeSession) jobProgress(e harness.JobOutput) {
	if e.ID == "" {
		return
	}
	safe := nativeSafe{red: s.redactor()}
	s.toolProgress(e.ID, harness.ToolProgress{ID: e.ID, Output: safe.text(e.Output)})
}

// jobFinished is a job ending: its execute row closed with the output it left
// — published directly, before the roster's finished is enqueued — then its
// row terminal and EventSubagent{finished} in one section with the eviction
// that may follow, the evicted rows' tool sets dropped, and the barrier.
// Nothing is stamped (the file's comment). During the session's Close — a job
// the close killed reports this and nothing else — done is closed, so the
// publish is a no-op and the barrier returns at once, while the roster's
// finished still reaches the journal through the outbox, which the log drains
// before it closes: the job's one record.
func (s *nativeSession) jobFinished(e harness.JobFinished) {
	if e.ID == "" {
		return
	}
	safe := nativeSafe{red: s.redactor()}
	status, settled := jobRowStatus(e)
	out := safe.text(e.Output)

	s.publishTool(e.ID, e.ID, func(t *ToolEvent) {
		t.Status = settled
		o := ToolOutput{}
		if t.Output != nil {
			o = *t.Output // what progress kept
		}
		if out != "" {
			setStdout(&o, out)
		}
		if e.Status == tool.JobExited && e.ExitCode >= 0 {
			code := e.ExitCode
			o.ExitCode = &code
			if code != 0 {
				// A collapsed failed command row previews StderrHead, as a
				// foreground call's does (toolFinished).
				o.StderrHead = o.StdoutHead
			}
		}
		if o != (ToolOutput{}) {
			t.Output = &o
		}
	})

	s.rosterMu.Lock()
	row := s.roster[e.ID]
	if row == nil || row.info.Status != SubagentRunning {
		// Never spawned here, or already finished: nothing to say again.
		s.rosterMu.Unlock()
		return
	}
	info := &row.info
	info.Status = status
	info.Error = e.Error // the row's word; safeSubagent below redacts it with the rest
	info.Output = out
	info.EndedAt = s.stampEndLocked(e.At)
	info.DurationMs = max(0, int(e.Duration.Milliseconds()))
	safeSubagent(info, safe)
	s.finishSeq++
	row.finish = s.finishSeq
	s.enqueueRosterLocked(SubagentChangeFinished, *info)
	evicted := s.evictFinishedLocked(e.ID)
	s.rosterMu.Unlock()

	s.dropChildTools(evicted)
	s.flushRoster()
}

// jobRowStatus is a finished job's roster status and its execute row's (the
// file's "Statuses").
func jobRowStatus(e harness.JobFinished) (SubagentStatus, string) {
	switch e.Status {
	case tool.JobExited:
		if e.ExitCode == 0 {
			return SubagentCompleted, toolCompleted
		}
		return SubagentFailed, toolFailed
	case tool.JobStopped, tool.JobTimedOut:
		return SubagentCancelled, toolCancelled
	}
	return SubagentFailed, toolFailed
}

// jobDescription is a job's row label: the command's first line that holds
// anything — a command can open with a blank line — and " …" when more lines
// follow it, so a heredoc's row does not read as its first line alone. The
// roster's cap (safeSubagent) bounds it.
func jobDescription(cmd string) string {
	lines := strings.Split(strings.TrimSpace(cmd), "\n")
	desc := strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		desc += " …"
	}
	return desc
}
