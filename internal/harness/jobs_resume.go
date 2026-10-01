package harness

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/charliek/craze/internal/harness/tool"
)

// Resuming a session that ran background jobs (plan 033 §3.8 "Resume", P15,
// owner decision 3). Jobs are never reattached: a session that closes kills
// them, and nothing of a job's result is persisted until it is delivered. So a
// resumed session tells its model, once, that every job its last incarnation
// started and never delivered is not running, in the job's own block — and,
// since the session cannot tell a job the close killed from one that had
// finished with its result still to be delivered (a result the wake chain's
// cap suspended, P14, which a detached host closes on), that it may have
// finished, and where its output is (plan 033 C10r, V3 F1, superseding §3.7's
// "Start it again if you still need it", which a model read as "it produced
// nothing" of a job that had exited 0):
//
//	<background_command id="t4.2.1" status="unknown">
//	$ npm run dev
//	The session was closed before this command's result was delivered. It is not running now; it may have finished first. Its output, if it wrote any, is saved to: /…/tool-output/tool_t4.2.1. Check that file before running it again.
//	</background_command>
//
// The file is the one the job's receipt named (tool.JobSpillPath), which the
// job wrote from its first byte to its end; a receipt that named none — the
// file could not be opened — gives a notice that says so.
//
// Each such block is set aside as a suspended result (restoreJobs): it wakes
// nothing and keeps no host alive, and the first turn a person starts takes it
// at its step 0, as it takes a result a failed wake set aside; bash_output or
// bash_stop naming it deliver it too. It is never running — it has no handle —
// so a stop, the stop key's included, finds nothing to stop.
//
// # The scan (jobScan)
//
// What the path says, read with no store change:
//
//   - started: a bash call's result — a tool entry's result part answering a
//     call to bash in the assistant entry before it, not an error — whose
//     last line is the fixed marker (tool.ParseJobMarker), naming a job of the
//     turn the entry belongs to (turnReader's): both receipts, the start's
//     and a promotion's, end with it. Only tool results are read, so a model
//     that quotes the marker in its own text, or a person who pastes it,
//     starts nothing; and a marker naming another turn's call is a command's
//     output, not a receipt. The command is the call's own argument.
//   - delivered: a job's block in a results entry — a step's, a wake's, a
//     person's step 0 — read block by block (resultsJobIDs), or the block a
//     bash_output or bash_stop call's result is. A result committed only
//     when its append succeeded (delivery.go), so a block on the path is one
//     the model read.
//
// A job started and not delivered is one to tell the model about, in the
// order the path started them. A command that printed a marker line naming
// a call of its own turn as its last line forges one: the model is then told
// a job it never started stopped — the cost of P15's one-line rule, and a
// harmless one.

// jobResumeNotice is the resume notice (the file's comment): the body of a
// job's block after its command's line, naming spill, the file its receipt
// named, or saying there is none.
func jobResumeNotice(spill string) string {
	const head = "The session was closed before this command's result was delivered. It is not running now; it may have finished first. "
	if spill == "" {
		return head + "Its output was not saved to a file."
	}
	return head + "Its output, if it wrote any, is saved to: " + spill + ". Check that file before running it again."
}

// The delimiters of the result blocks a results entry holds: a job's
// (jobBlock) and a sub-agent's (resultBlock). Each block's own closing tag
// inside its text is escaped, so its closing line ends it and nothing else
// does.
const (
	jobBlockOpen       = `<background_command id="`
	jobBlockClose      = "</background_command>"
	subagentBlockOpen  = `<subagent_result id="`
	subagentBlockClose = "</subagent_result>"
)

// stoppedJob is a job a resumed session's last incarnation started and never
// delivered: its id, its command, as the call that started it gave it, and
// its spill file, as its receipt named it ("" for none).
type stoppedJob struct{ id, cmd, spill string }

// jobScan reads a transcript's path, entry by entry, for the jobs it started
// and the ones it delivered (the file's comment). It is handed each entry's
// parts as plain values (resume.go's scanJobs, which walks Fantasy's message
// parts): this file imports the tool framework, so it may not import Fantasy
// too (TestSeamOne).
type jobScan struct {
	// calls are the last assistant entry's calls to the job tools, by their
	// provider id.
	calls     map[string]storedCall
	started   []stoppedJob
	delivered map[string]bool
}

// storedCall is one tool call of an assistant entry: its provider id, its
// tool and its arguments as the model sent them.
type storedCall struct{ id, name, input string }

// answer reads an assistant entry's calls: the ones to the job tools are
// what its tool entry's results answer.
func (j *jobScan) answer(calls []storedCall) {
	clear(j.calls)
	for _, c := range calls {
		switch c.name {
		case tool.BashTool, tool.BashOutputTool, tool.BashStopTool:
			if j.calls == nil {
				j.calls = map[string]storedCall{}
			}
			j.calls[c.id] = c
		}
	}
}

// result reads one result of a tool entry of turn: the call callID's, its
// text, and whether it is an error.
func (j *jobScan) result(callID, text string, isErr bool, turn int) {
	c, ok := j.calls[callID]
	if !ok {
		return
	}
	switch c.name {
	case tool.BashTool:
		if id, ok := tool.ParseJobMarker(text); ok && !isErr && jobTurn(id) == turn {
			j.started = append(j.started, stoppedJob{id: id, cmd: commandArg(c.input), spill: tool.JobSpillPath(text)})
		}
	default: // bash_output, bash_stop: the block, when the answer is one
		if id, ok := blockID(text, jobBlockOpen); ok {
			j.deliver(id)
		}
	}
}

// results reads a results entry's text: every job block in it is delivered.
func (j *jobScan) results(text string) {
	for _, id := range resultsJobIDs(text) {
		j.deliver(id)
	}
}

func (j *jobScan) deliver(id string) {
	if j.delivered == nil {
		j.delivered = map[string]bool{}
	}
	j.delivered[id] = true
}

// stopped are the jobs the path started and never delivered, in the order it
// started them, each once.
func (j *jobScan) stopped() []stoppedJob {
	var out []stoppedJob
	seen := map[string]bool{}
	for _, s := range j.started {
		if j.delivered[s.id] || seen[s.id] {
			continue
		}
		seen[s.id] = true
		out = append(out, s)
	}
	return out
}

// jobTurn is the turn a job's id names — a harness call id,
// "t<turn>.<step>.<n>", which ParseJobMarker has checked — or -1.
func jobTurn(id string) int {
	head, _, _ := strings.Cut(strings.TrimPrefix(id, "t"), ".")
	n, err := strconv.Atoi(head)
	if err != nil {
		return -1
	}
	return n
}

// commandArg is a bash call's command argument, "" when its arguments do not
// hold one.
func commandArg(input string) string {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(input), &in) != nil {
		return ""
	}
	return in.Command
}

// blockID is the id a block opened by open names, when text begins with one.
func blockID(text, open string) (string, bool) {
	rest, ok := strings.CutPrefix(text, open)
	if !ok {
		return "", false
	}
	id, _, ok := strings.Cut(rest, `"`)
	return id, ok && id != ""
}

// resultsJobIDs are the ids of the job blocks a results entry's text holds:
// its blocks read in order, each to its own closing line — a sub-agent's
// skipped whole, so nothing its text quotes is read as a block.
func resultsJobIDs(text string) []string {
	var ids []string
	inside := "" // the closing line of the block being read, "" between blocks
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case inside != "":
			if line == inside {
				inside = ""
			}
		case strings.HasPrefix(line, jobBlockOpen):
			if id, ok := blockID(line, jobBlockOpen); ok {
				ids = append(ids, id)
			}
			inside = jobBlockClose
		case strings.HasPrefix(line, subagentBlockOpen):
			inside = subagentBlockClose
		}
	}
	return ids
}

// restoreJobs sets aside, in a resumed session, one result per job its last
// incarnation started and never delivered (the file's comment): suspended, in
// stopped's order, its done closed and no handle, its status unknown and its
// block the resume notice — the command redacted with the session's widest
// redaction as it is now (union, P19), and the text again as deliver makes
// the block. An id the registry holds already is left alone (it cannot be: a
// resumed session numbers its turns on from the path's). A nil runner has
// none.
func (r *subagents) restoreJobs(stopped []stoppedJob) {
	if r == nil || len(stopped) == 0 {
		return
	}
	red := r.union(nil)
	r.regMu.Lock()
	defer r.regMu.Unlock()
	for _, sj := range stopped {
		if r.results[sj.id] != nil {
			continue
		}
		cmd := red.String(sj.cmd)
		done := make(chan struct{})
		close(done)
		r.finished++
		r.results[sj.id] = &bgResult{kind: kindJob, id: sj.id, typ: tool.JobType, desc: commandLine(cmd), callID: sj.id,
			state: resultSuspended, status: tool.JobUnknown, text: red.String(jobText(cmd, jobResumeNotice(sj.spill))),
			seq: r.finished, done: done, job: &jobState{cmd: cmd, attrs: jobAttrs{exit: -1}}}
		r.order = append(r.order, sj.id)
	}
}
