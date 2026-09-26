package agent

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/tool"
)

// The native adapter's tool rows (plan 019 §3.10). The harness reports one
// call as four events — started, called, progress, finished — and everything
// that draws a call reads one merged agent.ToolEvent: the transcript's rows
// (internal/tui/transcript.go) and `craze prompt --json`'s tool objects
// (internal/cli/events.go's toolJSON). So the adapter keeps its own merge
// here, with the ACP path's caps and its clone-before-publish rule
// (tools.go's mergeToolLocked): nothing in internal/tui changes, and no
// field is added to agent.Event or ToolEvent.
//
// Every row is published through the session's one emit choke point, and the
// merge lock is never held across it. The row handed to a consumer is a deep
// copy taken under that lock, because a consumer encodes it on a goroutine
// of its own while the next event is already merging into the stored row
// (coordination point 3, 019-native-harness-h2-tools/coordination-s1a.md).
//
// # Sets (plan 026 §3.9)
//
// The merge works on a set of rows: the parent's own, which Snapshot().Tools
// is, and one per sub-agent, which only EventTool{Agent: id} ever shows — the
// live session keeps a child's rows private the same way (SubagentInfo's
// comment). Every set lives under toolMu, a leaf: it is never taken with
// rosterMu held nor rosterMu with it (native_subagents.go), so a child's set is
// opened, settled and dropped in sections of its own, beside the roster's and
// never inside them. A child's set is opened when the child is spawned and
// dropped when its roster row is evicted; an event for a child with no set is
// one that arrived after its row was gone — only a lossy progress snapshot can
// (§3.9's carve-out) — and it is dropped rather than resurrecting the set.
//
// The owner is the set's name: "" for the parent's, the child's id otherwise,
// which is also the Agent every event published from it carries.

// The statuses a row carries. They are the ACP words for the same states,
// which is what the transcript's glyphs switch on (transcript.go's
// statusGlyph) and what toolStatusInFlight counts as running.
const (
	toolPending    = "pending"
	toolInProgress = "in_progress"
	toolCompleted  = "completed"
	toolFailed     = "failed"
	// toolCancelled is what settling leaves on a row whose call never
	// reported how it ended (settleTools).
	toolCancelled = "cancelled"
)

// nativeToolSet is one set of rows (the file's comment): the rows by call id,
// the order the calls were first seen in, and — for a child's set — the cap
// and the ids it has evicted.
type nativeToolSet struct {
	rows  map[string]ToolEvent
	order []string
	// capN bounds a child's set at childToolCap (subagents.go), evicting the
	// oldest settled row first, as the live session bounds a grok child's
	// (evictChildToolLocked). 0 is the parent's set, unbounded as it has always
	// been: a turn's own calls are the harness's to bound.
	capN int
	// evicted remembers the ids the cap dropped, bounded as the live session's
	// are (childEvictedCap, childEvictedKeep), so a late update for one is
	// ignored rather than reopening a row with half its fields.
	evicted      map[string]struct{}
	evictedOrder []string
}

// merge applies apply to row id — opening it the first time — and returns the
// row and whether it changed. onlyIfInFlight drops the change when the row has
// already settled or does not exist, so a terminal update that raced in is not
// overwritten (mergeToolLocked's own rule) and a stamp never opens a row of
// its own. toolMu is held.
//
// The row's TaskInfo is copied before apply sees it (plan 026 §3.9, panel P5).
// A stored row and the one before it share every pointer the plain copy below
// copies, and apply is free to write through out.Task — the agent row's stamp
// and its close do — so without the copy the previous row's Task would change
// with the new one, and the change check could never see a Task-only update:
// it would be comparing a value with itself. The other pointer fields are
// replaced whole by every apply that touches them (Output, Locations, Diffs),
// so they need no copy.
//
// The returned row is a deep copy taken under the lock: the caller hands it to
// a consumer that reads it on another goroutine, while the stored row goes on
// merging.
func (set *nativeToolSet) merge(id string, onlyIfInFlight bool, apply func(*ToolEvent)) (ToolEvent, bool) {
	if _, gone := set.evicted[id]; gone {
		return ToolEvent{}, false
	}
	prev, exists := set.rows[id]
	if onlyIfInFlight && (!exists || !toolStatusInFlight(prev.Status)) {
		return ToolEvent{}, false
	}
	out := prev
	if out.Task != nil {
		task := *out.Task
		out.Task = &task
	}
	if !exists {
		if set.rows == nil {
			set.rows = map[string]ToolEvent{}
		}
		if set.capN > 0 && len(set.order) >= set.capN {
			set.evictOne()
		}
		out.ID = id
		set.order = append(set.order, id)
	}
	apply(&out)
	out.At = time.Now()
	// The fields a consumer draws; a repeated progress snapshot changes
	// none of them and is not published twice. Task is one of them: the
	// agent row's stamp at spawn changes nothing else (tools.go's live merge
	// compares it too).
	changed := !exists ||
		out.Status != prev.Status ||
		out.Title != prev.Title ||
		out.Kind != prev.Kind ||
		out.RawInput != prev.RawInput ||
		!sameOutput(prev.Output, out.Output) ||
		!sameDiffs(prev.Diffs, out.Diffs) ||
		!sameTask(prev.Task, out.Task)
	set.rows[id] = out
	return cloneTool(out), changed
}

// evictOne drops the oldest settled row, else the oldest row, and remembers
// its id: the live session's rule for a child's set (evictChildToolLocked).
// toolMu is held.
func (set *nativeToolSet) evictOne() {
	pick := func(settled bool) int {
		for i, id := range set.order {
			if toolStatusInFlight(set.rows[id].Status) != settled {
				return i
			}
		}
		return -1
	}
	i := pick(true)
	if i < 0 {
		i = pick(false)
	}
	if i < 0 {
		return
	}
	id := set.order[i]
	delete(set.rows, id)
	set.order = append(set.order[:i], set.order[i+1:]...)
	if set.evicted == nil {
		set.evicted = map[string]struct{}{}
	}
	set.evicted[id] = struct{}{}
	set.evictedOrder = append(set.evictedOrder, id)
	if len(set.evictedOrder) > childEvictedCap {
		for _, old := range set.evictedOrder[:len(set.evictedOrder)-childEvictedKeep] {
			delete(set.evicted, old)
		}
		set.evictedOrder = append([]string(nil), set.evictedOrder[len(set.evictedOrder)-childEvictedKeep:]...)
	}
}

// toolSetLocked is owner's set: the parent's for "", a child's otherwise, and
// nil for a child with none — never spawned, or evicted. toolMu is held.
func (s *nativeSession) toolSetLocked(owner string) *nativeToolSet {
	if owner == "" {
		return &s.tools
	}
	return s.childTools[owner]
}

// openChildTools gives a child its set, capped, when it is spawned
// (native_subagents.go): before its first event can arrive, since the runner
// runs the child only once the sink has returned from its SubagentStarted.
func (s *nativeSession) openChildTools(owner string) {
	s.toolMu.Lock()
	defer s.toolMu.Unlock()
	if s.childTools == nil {
		s.childTools = map[string]*nativeToolSet{}
	}
	s.childTools[owner] = &nativeToolSet{capN: childToolCap}
}

// dropChildTools forgets the sets of children whose roster rows were evicted:
// the rows go with them (§3.9). It runs after rosterMu is released — the two
// locks are never nested (panel P17).
func (s *nativeSession) dropChildTools(owners []string) {
	if len(owners) == 0 {
		return
	}
	s.toolMu.Lock()
	defer s.toolMu.Unlock()
	for _, owner := range owners {
		delete(s.childTools, owner)
	}
}

// toolStarted opens the call's row: the model has begun it and its arguments
// are still streaming, so all that is known is which tool it is. Name is the
// tool's own, not a title — the row has to say something before the
// arguments arrive, and ToolCalled brings the title.
//
// An agent call's row is a task row from here (plan 026 §3.9): the kind is
// task and the row carries a TaskInfo, which is what makes the TUI draw it as
// the task row it already draws for grok and cursor (ToolEvent.IsTask). What
// the task is arrives with its arguments (toolCalled).
func (s *nativeSession) toolStarted(owner string, e harness.ToolStarted) {
	s.publishTool(owner, e.ID, func(t *ToolEvent) {
		t.Status = toolPending
		t.Kind = string(e.Kind)
		t.ToolName = sanitizeText(e.Tool)
		t.Name = t.ToolName
		if e.Kind == tool.KindTask && t.Task == nil {
			t.Task = &TaskInfo{}
		}
	})
}

// toolCalled fills the row from the prepared call: the title a row shows,
// the paths it touches — which is how a read or edit row finds its file —
// and the raw input a command or a search query is drawn from. An agent
// call's row learns its task from the call's own arguments (nativeTaskOf).
//
// An agent row's title and raw input are the task's own text — the
// description and the whole prompt, which a model wrote — so they take the
// task payload's discipline too (nativeSafe; plan 026 §3.9, panel P38): the
// dispatcher's redaction of the request cannot see a key a zero-width space
// splits, and the sanitizer below would put it back together.
func (s *nativeSession) toolCalled(owner string, e harness.ToolCalled) {
	var task *TaskInfo
	title, raw := sanitizeText(e.Request.Title), nativeRawInput(e.Request)
	if e.Request.Kind == tool.KindTask {
		safe := nativeSafe{red: s.redactor()}
		task = nativeTaskOf(e.Request, safe)
		title = safe.text(e.Request.Title)
		raw = truncateUTF8(safe.text(compactJSON(e.Request.Input)), rawInputCap)
	}
	s.publishTool(owner, e.ID, func(t *ToolEvent) {
		t.Status = toolInProgress
		t.Title = title
		t.Locations = nativeLocations(e.Request.Paths)
		t.RawInput = raw
		// The harness opens every call with a ToolStarted, so these are
		// already set; a row that somehow missed one would otherwise draw
		// as the kindless "tool" for the rest of its life.
		if t.Kind == "" {
			t.Kind = string(e.Request.Kind)
		}
		if t.ToolName == "" {
			t.ToolName = sanitizeText(e.Request.Tool)
			t.Name = t.ToolName
		}
		if task != nil {
			t.Task = mergeTaskInfo(t.Task, *task)
		}
	})
}

// nativeTaskOf is an agent call's task as its arguments state it — the
// canonical input the agent tool itself decodes (opencode's agent.go):
// description, prompt and subagent_type, the last defaulting as the tool
// defaults it. Input is the dispatcher's redacted copy, but a key spelled with
// JSON escapes, or split by a character the sanitizer drops, is only a key
// once decoded or sanitized, so every field goes through nativeSafe's
// redact → sanitize → redact (plan 026 §3.9, panel P30, P38) and is capped: the
// prompt at taskPromptCap, as grok's task rows cap it, and the two labels at
// the roster's own caps, so the row and its child's roster row say the same.
// Arguments that do not decode leave an empty task, which the row's title
// still labels.
func nativeTaskOf(req harness.ToolRequest, safe nativeSafe) *TaskInfo {
	var in struct {
		Description  string `json:"description"`
		Prompt       string `json:"prompt"`
		SubagentType string `json:"subagent_type"`
	}
	if err := json.Unmarshal([]byte(req.Input), &in); err != nil {
		return &TaskInfo{}
	}
	typ := in.SubagentType
	if typ == "" {
		typ = tool.DefaultAgentType
	}
	return &TaskInfo{
		Description:  truncateUTF8(safe.text(in.Description), subagentDescCap),
		Prompt:       truncateUTF8(safe.text(in.Prompt), taskPromptCap),
		SubagentType: truncateUTF8(safe.text(typ), subagentTypeCap),
	}
}

// resafeTask puts an agent row's own texts through safe again — its title, its
// raw input, and its task's description, prompt, type and model — with the
// caps toolCalled and the stamps gave them, before the row is published once
// more (plan 026 §3.9, review r8, finding 2). They were redacted when they
// arrived, with the keys known then; a key the parent learned since — a
// SetModel that resolved one while the child ran — or one only the child knows,
// which the redactor covers once the child is registered, would otherwise go
// out again in every later publication of the row. safe is taken before toolMu
// and applying it is pure, so this runs inside the merge (apply). A row it
// has already been through, with no key learned since, is unchanged.
func resafeTask(t *ToolEvent, safe nativeSafe) {
	t.Title = safe.text(t.Title)
	t.RawInput = truncateUTF8(safe.text(t.RawInput), rawInputCap)
	if task := t.Task; task != nil {
		task.Description = truncateUTF8(safe.text(task.Description), subagentDescCap)
		task.Prompt = truncateUTF8(safe.text(task.Prompt), taskPromptCap)
		task.SubagentType = truncateUTF8(safe.text(task.SubagentType), subagentTypeCap)
		task.Model = truncateUTF8(safe.text(task.Model), subagentModelCap)
	}
}

// toolProgress keeps a running call's output: the whole of what it has
// produced so far, not a delta, as the tail an expanded row draws and the
// head a collapsed one previews. It is published without blocking —
// progress is lossy by contract and the harness drops a snapshot rather
// than wait on this sink (plan 019 §3.5).
func (s *nativeSession) toolProgress(owner string, e harness.ToolProgress) {
	s.publishToolLossy(owner, e.ID, func(t *ToolEvent) {
		out := ToolOutput{}
		if t.Output != nil {
			out = *t.Output
		}
		setStdout(&out, e.Output)
		t.Output = &out
	})
}

// toolFinished closes the row with the call's result. Each kind fills the
// one field its row draws (plan 019 §3.10): read the content an expanded
// row shows, execute the merged stream and the exit code a failed row
// previews as stderr, edit and write the diff — computed here, from the
// texts the tool held, and nowhere else.
//
// The parent's agent row is closed as a task (plan 026 §3.9): its status and
// duration, and Receipt, which for native means "the lifecycle is complete and
// Model is final" — the row's model is shown only for a receipt
// (transcript.go's taskRows; panel P15). A call that started a child had its
// row stamped with the child's status, duration and model when the child
// finished (subagentFinished; SubagentFinished precedes the call's
// ToolFinished, §3.9), and keeps them: nothing here reads the roster, which may
// have evicted the child since (review r8, finding 3). One that never started
// a child — a refused type, a failed Open, a call aborted while it waited for
// a slot — takes them from its own result and duration.
//
// A replayed result (e.Replayed, a load's; plan 028 §3.4, P9) is the text the
// model read and nothing else, so its row is drawn from that alone
// (replayedOutput): no diff, no exit code, no duration, and a label saying so.
// An error stays an error, and an agent row is closed as a task from its
// result — the child's final text for a foreground call, the launch receipt
// for a background one — with no model and no duration, since no child ran.
func (s *nativeSession) toolFinished(owner string, e harness.ToolFinished) {
	res := e.Result
	status, ms := taskStatusOf(res), int(e.Duration.Milliseconds())
	task := owner == "" && s.isTaskRow(e.ID)
	var safe nativeSafe
	if task {
		// A failed agent call's text is the child's last output, which a model
		// wrote: the task payload's discipline, as toolCalled's (P38). The
		// redactor still covers the child's keys, retired as it is (harness's
		// Session.Redact: a child's are kept for the rest of its turn).
		safe = nativeSafe{red: s.redactor()}
		res.Text, res.Content = safe.text(res.Text), safe.text(res.Content)
	}
	s.publishTool(owner, e.ID, func(t *ToolEvent) {
		t.Status = toolCompleted
		if res.IsError {
			t.Status = toolFailed
		}
		if e.Replayed {
			t.Output = replayedOutput(res, t.Kind)
			if task && t.Task != nil {
				resafeTask(t, safe)
				if t.Task.Status == "" {
					t.Task.Status = status
				}
				t.Task.Receipt = true
			}
			return
		}
		if len(res.Edits) > 0 {
			t.Diffs = nativeDiffs(res.Edits)
		}
		out := ToolOutput{}
		if t.Output != nil {
			out = *t.Output // what progress kept, for a call that reported some
		}
		if o := res.Output; o != nil {
			setStdout(&out, o.Output)
			code := o.ExitCode
			out.ExitCode = &code
		}
		if res.Content != "" {
			setContent(&out, res.Content)
		}
		// A failed call with nothing of its own to show — a refusal, a call
		// the turn aborted, a path that was not there — shows the text the
		// model was given, which is the only thing that says why. An
		// expanded row draws Content; a command's row draws its stream.
		if res.IsError {
			if out.Content == "" {
				setContent(&out, res.Text)
			}
			if out.Stdout == "" && t.Kind == string(tool.KindExecute) {
				setStdout(&out, res.Text)
			}
		}
		if out.ExitCode != nil && *out.ExitCode != 0 {
			// A collapsed failed command row previews StderrHead
			// (transcript.go:931-954), and what the harness keeps is the
			// command's stdout and stderr merged, in order.
			out.StderrHead = out.StdoutHead
		}
		if out != (ToolOutput{}) {
			t.Output = &out
		}
		if task && t.Task != nil {
			// merge copied the Task before handing it here (P5).
			resafeTask(t, safe)
			if t.Task.Status == "" { // no child finished for this call
				t.Task.Status = status
				// A background call's row is final here (plan 026 §3.11, X33)
				// and carries no duration: the call returned as soon as its
				// child had started, so its own duration is the spawn's — a
				// millisecond or none — and the child's is on its roster row.
				if !t.Task.Background {
					t.Task.DurationMs = max(ms, 0)
				}
			}
			t.Task.Receipt = true
		}
	})
}

// replayedLabel is the last line of a replayed row's body (plan 028 §3.4,
// P9): the row was rebuilt from the transcript, which keeps only the text the
// model read, so what a live row draws beside it — an exit code, a diff, a
// truncation — is not missing from the call but from the record.
const replayedLabel = "(replayed)"

// replayedOutput is a replayed result's row body: the stored text — Content,
// which is the text the model read (ToolFinished.Replayed) — with the label
// after it, as the content an expanded row draws and, on an execute row, the
// stream it draws as well (setStdout). The label goes last so a collapsed row
// looks like a live one: an execute row previews the first line of its stream
// (StdoutHead), which is the output's own, and a read row draws no body until
// it is expanded. An expanded row ends with the label, unless the output runs
// past the rows an expanded row draws. ExitCode stays nil: a replay has no
// code, and a zero would say the command succeeded. The text is tail-capped
// to leave room for the label, so the label survives a long output's cap, and
// Truncated says when the text was cut. The head is taken from the whole
// stored text, not from that capped tail, exactly as setStdout takes a live
// row's — so a long command still previews its own first line rather than
// the start of what the cap kept (astra r1-c3 F3).
//
// Nothing a row draws can say "replayed" on its own — the head row's suffix
// is computed from the exit code and the diff, and an edit or an agent row
// draws no body — so the label lives in the body: an expanded execute or read
// row shows it, and every other row carries it in its content.
func replayedOutput(res tool.Result, kind string) *ToolOutput {
	text := res.Content
	if text == "" {
		text = res.Text
	}
	clean := sanitizeText(text)
	room := outputTailCap - len(replayedLabel) - 1
	body, whole := replayedLabel, replayedLabel
	if clean != "" {
		body = tailUTF8(clean, room) + "\n" + replayedLabel
		whole = clean + "\n" + replayedLabel
	}
	out := ToolOutput{}
	setContent(&out, body)
	if kind == string(tool.KindExecute) {
		setStdout(&out, body)
		out.StdoutHead = truncateUTF8(whole, outputHeadCap)
	}
	out.Truncated = len(clean) > room
	return &out
}

// taskStatusOf is a finished agent call's status from its result alone, for a
// call that never started a child: aborted is the tool contract's cancel
// (tool.ClassAborted), any other error a failure.
func taskStatusOf(res tool.Result) SubagentStatus {
	switch {
	case res.IsError && res.Class == tool.ClassAborted:
		return SubagentCancelled
	case res.IsError:
		return SubagentFailed
	}
	return SubagentCompleted
}

// isTaskRow reports whether the parent's row id is an agent call's.
func (s *nativeSession) isTaskRow(id string) bool {
	s.toolMu.Lock()
	defer s.toolMu.Unlock()
	return s.tools.rows[id].Task != nil
}

// settleTools closes every row the turn left running, whichever way it
// ended — done with any stop reason, error, cancel, Close. It is the native
// equivalent of the live session's closeInFlightTools (live.go:909-943),
// and it exists for the same reason: a row nobody ever finishes spins for
// ever and the status row keeps counting it. The harness settles its own
// calls (toolbridge.go's settleCalls), so this usually finds nothing; what
// it stands against is a call whose ending never reached the sink at all —
// the common live case being a ToolStarted whose arguments the response cut
// off, which the ACP path never meets.
//
// It is the parent's set: a child's is settled by its SubagentFinished
// (settleChildTools), which a foreground child's call always delivers before
// the turn that ran it can end (plan 026 §3.9).
func (s *nativeSession) settleTools() { s.settleSet("", toolCancelled) }

// settleChildTools closes every row child left running to the child's
// outcome, as the live session settles a grok child's (settleChildToolsLocked):
// its calls never reported an ending, and the child has ended.
func (s *nativeSession) settleChildTools(child, status string) { s.settleSet(child, status) }

// settleSet is settleTools for owner's set, to status. An agent row it
// settles is published again, so its own texts go through the redactor again
// first (resafeTask, review r8): it is taken before toolMu, once, and only for
// the parent's set, the one set that holds agent rows.
func (s *nativeSession) settleSet(owner, status string) {
	s.toolMu.Lock()
	var ids []string
	if set := s.toolSetLocked(owner); set != nil {
		ids = append([]string(nil), set.order...)
	}
	s.toolMu.Unlock()
	if len(ids) == 0 {
		return
	}
	var safe nativeSafe
	if owner == "" {
		safe = nativeSafe{red: s.redactor()}
	}
	for _, id := range ids {
		// The in-flight test happens under the merge lock: a terminal
		// update that lands between the scan and the merge wins.
		if t, changed := s.mergeTool(owner, id, true, func(t *ToolEvent) {
			if t.Task != nil && safe.red != nil {
				resafeTask(t, safe)
			}
			t.Status = status
		}); changed {
			s.emit(Event{Type: EventTool, Agent: owner, Tool: &t})
		}
	}
}

// toolRows is every row of the parent's set, in call order, for Snapshot: a
// child's rows are only ever events (SubagentInfo's comment).
func (s *nativeSession) toolRows() []ToolEvent {
	s.toolMu.Lock()
	defer s.toolMu.Unlock()
	return snapshotTools(s.tools.order, s.tools.rows)
}

// publishTool merges one event into its row in owner's set and publishes the
// row when something a consumer can see changed.
func (s *nativeSession) publishTool(owner, id string, apply func(*ToolEvent)) {
	if t, changed := s.mergeTool(owner, id, false, apply); changed {
		s.emit(Event{Type: EventTool, Agent: owner, Tool: &t})
	}
}

// stampTool is publishTool for a row that must already be running: the agent
// row's stamp at spawn, which never opens a row of its own (merge's
// onlyIfInFlight).
func (s *nativeSession) stampTool(owner, id string, apply func(*ToolEvent)) {
	if t, changed := s.mergeTool(owner, id, true, apply); changed {
		s.emit(Event{Type: EventTool, Agent: owner, Tool: &t})
	}
}

// publishToolLossy is publishTool for progress: the send gives up rather
// than wait for a consumer that is not draining (plan 019 §3.5). The next
// snapshot carries the whole output again, so a drop loses nothing but a
// frame. (S1a replaces the select-default with EventLog.TryPublish;
// coordination point 4.)
func (s *nativeSession) publishToolLossy(owner, id string, apply func(*ToolEvent)) {
	if t, changed := s.mergeTool(owner, id, false, apply); changed {
		s.emitLossy(Event{Type: EventTool, Agent: owner, Tool: &t})
	}
}

// mergeTool is merge on owner's set under toolMu: nothing, for a child with
// no set (toolSetLocked).
func (s *nativeSession) mergeTool(owner, id string, onlyIfInFlight bool, apply func(*ToolEvent)) (ToolEvent, bool) {
	s.toolMu.Lock()
	defer s.toolMu.Unlock()
	set := s.toolSetLocked(owner)
	if set == nil {
		return ToolEvent{}, false
	}
	return set.merge(id, onlyIfInFlight, apply)
}

// nativeLocations are the paths a call touches, as the row's Locations: the
// first is the file a read or edit row names (transcript.go's toolPath).
func nativeLocations(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, 0, min(len(paths), locationsCap))
	for _, p := range paths {
		if p == "" {
			continue
		}
		out = append(out, sanitizeText(p))
		if len(out) >= locationsCap {
			break
		}
	}
	return out
}

// nativeRawInput is what a row draws the call's arguments from: the command
// for an execute call, which its row prints as the command
// (transcript.go:1074-1079), and the arguments as compact JSON for every
// other kind, which is where a search row looks for a query it has not
// already shown. Capped as mergeToolLocked caps a raw input.
func nativeRawInput(req harness.ToolRequest) string {
	if req.Kind == tool.KindExecute && req.Command != "" {
		return truncateUTF8(sanitizeText(req.Command), rawInputCap)
	}
	return truncateUTF8(sanitizeText(compactJSON(req.Input)), rawInputCap)
}

// compactJSON is s without the whitespace between its tokens, and s itself
// when it is not valid JSON — a model's arguments need not be.
func compactJSON(s string) string {
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}

// nativeDiffs are the row's diffs for what an edit or a write changed. The
// lines are counted on the whole texts and only then capped (diffOf), and
// no more diffs are kept than the ACP path keeps.
func nativeDiffs(edits []tool.FileEdit) []ToolDiff {
	out := make([]ToolDiff, 0, min(len(edits), diffsCap))
	for _, e := range edits {
		out = append(out, diffOf(e.Path, e.Old, e.New))
		if len(out) >= diffsCap {
			break
		}
	}
	return out
}

// setStdout keeps text as a row's stream: the 8 KiB tail an expanded row
// draws and the 512 B head a collapsed one previews.
func setStdout(out *ToolOutput, text string) {
	s := sanitizeText(text)
	out.Stdout = tailUTF8(s, outputTailCap)
	out.StdoutHead = truncateUTF8(s, outputHeadCap)
	out.Truncated = out.Truncated || len(s) > outputTailCap
}

// setContent keeps text as a row's content: what an expanded read draws
// (transcript.go:850-856), never ContentText.
func setContent(out *ToolOutput, text string) {
	s := sanitizeText(text)
	out.Content = tailUTF8(s, outputTailCap)
	out.Truncated = out.Truncated || len(s) > outputTailCap
}
