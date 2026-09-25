package agent

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/journal"
)

// The native adapter's sub-agents (plan 026 §3.9). The harness runs a child
// session per agent call (internal/harness/subagents.go) and reports it into
// the parent's sink as three events: SubagentStarted once the child has
// opened, every event of the child's own wrapped in SubagentEvent, and
// SubagentFinished once it has ended and closed. This file maps them onto what
// the live session already publishes for grok's children — EventSubagent with
// SubagentChange spawned|progress|finished, and the child's text, thought,
// user and tool events tagged with Event.Agent — so the TUI's row band, its
// child view and its task row, the engine's transcript model and `craze prompt
// --json` all draw native children with no change of their own, and no
// agent.Event kind or field is added (§3.12).
//
// # The roster (S1c's seam review: roster order is event order)
//
// The roster is this session's list of children, in spawn order, which
// Snapshot().Subagents hands out cloned. rosterMu guards it and is a leaf in
// the adapter's own locks: s.mu → rosterMu → the log's outbox, and nothing
// else is ever taken under it — never toolMu, whose sets the roster's
// evictions drop in a section of their own (panel P17), never s.mu, and never
// the harness's locks: the redactor a section applies is taken before the
// section, and applying it takes no lock (redactor).
//
// Every EventSubagent is ENQUEUED (log.Enqueue: the outbox mutex is a strict
// leaf) inside the rosterMu section that made the change it reports, so the
// order the events commit in is the order the roster changed in, whichever
// children's goroutines raced to change it; a client that folds the events —
// the engine's model, an attached one — holds exactly the roster the
// snapshot holds at every quiescent point, spawn order and evictions included
// (internal/transcript's foldSubagent replays the live rule below). Each event
// carries the row whole, cloned under the lock (cloneSubagent: ToolsUsed is
// the one slice), because the outbox keeps the event until it is delivered
// and the roster goes on changing (panel P5); and no roster field changes
// anywhere but in such a section, since the fold keeps the last payload per id
// and a change made in the snapshot alone would be one it never sees.
//
// Two of the three are followed, outside rosterMu, by a barrier:
// log.Flush(context.Background(), s.done), the one native's prompt already
// stands on before its terminal event. After spawned, so the child's first
// event — published directly, from the child's own goroutine, once the sink
// has returned — follows it; after finished, so the parent's ToolFinished for
// the call — published directly too, once the call returns — follows it. The
// context is Background and never a turn's or a call's: a flush abandoned on
// a cancel would let the direct publish overtake the event still in the outbox
// (panel P1); done is what lets it end when Close has begun (native.go's
// Close). Progress is enqueued only: nothing is published that has to follow
// it.
//
// # Eviction (the live rule, S1c's seam review)
//
// Finished rows past subagentFinishedCap (32) are evicted oldest finish
// first — by EndedAt, then by finish order — and never the row that has just
// finished (subagents.go's evictFinishedLocked, which the fold replicates). So
// the two agree even when two finishes carry the same clock reading, EndedAt is
// stamped under rosterMu strictly increasing: a finish whose time is not after
// the last one's is moved 1 ns past it (macOS clocks are microsecond). The
// eviction runs in the section that finished the row, right after its event
// is enqueued, which is where the fold runs it; an evicted child's tool rows go
// with it, dropped under toolMu once rosterMu is released.
//
// # Payloads (panel P9, P30, P38; review r8)
//
// A roster, lifecycle or task payload is text the runner redacted as it built
// the event (with the parent's and the child's keys together), and the adapter
// redacts it again with the session's widest redactor — every key the parent
// knows now and every key its children know, one that only a child learned
// included (harness.Session.Redact; review r8, finding 1) — so a key the parent
// learned while the event waited for the parent turn's lock is covered here
// (review r4); sanitizes it and redacts it once more (nativeSafe), because
// sanitizing can put a key back together, a child's that the runner's union
// could not see split included; then capSubagent caps it, which neither
// un-sanitizes nor rebuilds anything (a cut only removes).
//
// That is done to the WHOLE row, in every section that publishes it, and not
// only to the fields the event brought (review r8, finding 2): a row keeps its
// description, prompt and activity from event to event, and a key the parent
// learns after they were redacted — a SetModel that resolves one while the
// child runs — would otherwise ride out again in the next progress or finished
// payload, and in the snapshot. The redactor is taken before rosterMu, and
// applying it is pure (redactor), so the section takes no lock of the
// harness's. The row keeps what the section redacted it to, which is what its
// payload carries: the snapshot and the fold still agree. The parent's agent
// row is held to the same rule in every section that republishes it
// (resafeTask). What no section can cover is a key learned after its redactor
// was taken, while its payload waits: X14's accepted residual.
//
// A child's streamed text and thought are the parent's contract exactly: raw
// model output, sanitized and not redacted (harness/events.go). A child's tool
// events were redacted by its own dispatcher and are merged like the
// parent's.

// diagSubagentUsage is the journal diag the parent's unpersisted sub-agent
// usage is written as (plan 026 §3.7, panel P40).
const diagSubagentUsage = "subagent_usage"

// subagentUsageRowsPerNote is how many usage rows one subagent_usage note
// holds: eight fields a row and three for the note keeps it under the
// journal's 64 fields (journal.DiagNote), past which a note keeps only its
// count. A step with more models than this is written as several notes.
const subagentUsageRowsPerNote = 7

// nativeChild is one roster row: the child as a consumer sees it, and its
// place in finish order — 0 while it runs — which breaks an EndedAt tie as the
// live session's finishSeq does.
type nativeChild struct {
	info   SubagentInfo
	finish uint64
}

// redactor is the session's widest redactor as it is now — every key the
// session knows, the ones learned after Open included, and every key its
// sub-agents know (harness.Session.Redactor; review r8, finding 1) — or none
// before Start. It is TAKEN with no lock of the adapter's held: it takes s.mu
// for the read, and the harness's own leaf locks to gather the keys, and s.mu
// is never taken under toolMu or rosterMu, which Snapshot takes under it. The
// function it returns is pure — applying it takes no lock at all — so a
// section under rosterMu or toolMu applies one taken before it (review r8,
// finding 2).
func (s *nativeSession) redactor() func(string) string {
	s.mu.Lock()
	hs := s.hs
	s.mu.Unlock()
	if hs == nil {
		return func(text string) string { return text }
	}
	return hs.Redactor()
}

// CancelSubagent is SubagentCanceller (plan 026 §3.10): the harness's stop of
// one child, which cancels that child's context and waits for nothing; what it
// comes to reaches the roster through the child's SubagentFinished like any
// other ending (subagentFinished), cancelled with the Error "stopped by the
// user" when the stop alone decided it. Nothing is published here.
//
// hs is read under s.mu, released before the harness is called, as redactor
// does: s.mu is never held across anything that can wait — the one registry
// call Cancel makes under it does not, and Close, SetModel and the prompt take
// the harness only after releasing it — so a stop from the TUI's Update never
// queues behind a turn. The harness's stop takes only its registry's and the
// child's handle's leaf locks. Before Start there is no child to stop; after
// Close there is none either, the harness's Close having retired them all.
func (s *nativeSession) CancelSubagent(id string) error {
	s.mu.Lock()
	hs := s.hs
	s.mu.Unlock()
	if hs == nil {
		return ErrNoSuchSubagent
	}
	if err := hs.CancelSubagent(id); err != nil {
		if errors.Is(err, harness.ErrNoSuchSubagent) {
			return ErrNoSuchSubagent
		}
		return err
	}
	return nil
}

var _ SubagentCanceller = (*nativeSession)(nil)

// safeSubagent puts every text field of info through safe and then caps it
// (the file's "Payloads"). ID and ToolCallID are craze's own — the runner's
// UUID and the harness's t<turn>.<step>.<n> — and pass as they are. It is pure,
// so a section applies it under rosterMu, to the row, before the row's payload
// is cloned and enqueued; over a row it has already been through, with no key
// learned since, it changes nothing.
func safeSubagent(info *SubagentInfo, safe nativeSafe) {
	info.Description = safe.text(info.Description)
	info.SubagentType = safe.text(info.SubagentType)
	info.Model = safe.text(info.Model)
	info.Error = safe.text(info.Error)
	info.Prompt = safe.text(info.Prompt)
	info.Output = safe.text(info.Output)
	info.Activity = safe.text(info.Activity)
	for i, used := range info.ToolsUsed {
		info.ToolsUsed[i] = safe.text(used)
	}
	capSubagent(info)
}

// subagentStarted is a child opening: its roster row, EventSubagent{spawned},
// the barrier, the child's first user line — its task, so its transcript
// opens with it as grok's does — and the parent's agent row stamped with the
// child's id and model.
func (s *nativeSession) subagentStarted(e harness.SubagentStarted) {
	if e.ID == "" {
		return
	}
	// Taken before any lock: the child is registered with the harness by now,
	// so it covers a key only the child knows (redactor).
	safe := nativeSafe{red: s.redactor()}
	info := SubagentInfo{
		ID:           e.ID,
		ToolCallID:   e.CallID,
		Description:  e.Description,
		SubagentType: e.Type,
		Model:        e.Model,
		Status:       SubagentRunning,
		Prompt:       e.Prompt,
		StartedAt:    wallClock(e.At, s.Now),
		Transcript:   true,
		Background:   e.Background,
	}
	// The child's tool rows get their set before the child exists for anyone:
	// it cannot stream before this returns (the runner runs it after).
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
	info = cloneSubagent(row.info) // what spawned said, for what follows it
	s.rosterMu.Unlock()
	s.flushRoster()

	if info.Prompt != "" {
		s.emit(Event{Type: EventUser, Agent: e.ID, Text: info.Prompt})
	}
	// Task-only but for the row's own texts, which go through the redactor
	// that now covers the child (resafeTask): that is why the merge compares
	// Task and copies it before this writes through it (P5).
	//
	// A background child's row is final at its acknowledgement (plan 026
	// §3.11, X33): the call returns as soon as the child has started, so
	// this stamp — inside the call, before its ToolFinished — is the row's
	// one chance to carry the child's identity and model, and it marks the
	// task as background; the child's outcome lives in its roster row and in
	// the result a later turn delivers, never in a stamp after the call.
	s.stampTool("", e.CallID, func(t *ToolEvent) {
		resafeTask(t, safe)
		if t.Task == nil {
			t.Task = &TaskInfo{}
		}
		t.Task.AgentID = info.ID
		t.Task.Model = info.Model
		if e.Background {
			t.Task.Background = true
		}
	})
}

// subagentEvent is one of child e.ID's own events. The switch names every
// harness event, so what a child can send is decided here and nowhere else
// (panel GLM 14).
func (s *nativeSession) subagentEvent(e harness.SubagentEvent) {
	id := e.ID
	if id == "" {
		return
	}
	switch c := e.Event.(type) {
	case harness.TextDelta:
		// The parent's contract, exactly (the sink's TextDelta case): raw
		// model output, sanitized for the terminal and not redacted.
		if t := sanitizeText(c.Text); t != "" {
			s.emit(Event{Type: EventText, Agent: id, Text: t})
		}
	case harness.ThoughtDelta:
		if t := sanitizeText(c.Text); t != "" {
			s.emit(Event{Type: EventThought, Agent: id, Text: t})
		}
	case harness.ToolStarted:
		s.toolStarted(id, c)
	case harness.ToolCalled:
		s.toolCalled(id, c)
		safe := nativeSafe{red: s.redactor()}
		activity := safe.text(c.Request.Title)
		if activity == "" {
			activity = safe.text(c.Request.Tool)
		}
		used := safe.text(c.Request.Tool)
		s.subagentProgress(id, safe, func(info *SubagentInfo) {
			info.ToolCalls++
			info.Activity = activity
			info.ToolsUsed = tailStrings(append(slices.Clone(info.ToolsUsed), used), subagentToolsUsedCap)
		})
	case harness.ToolProgress:
		// Lossy, as the parent's is: it goes out through TryPublish or not at
		// all, and may trail the child's finished (§3.9's one carve-out) —
		// once the child's set is gone it goes nowhere.
		s.toolProgress(id, c)
	case harness.ToolFinished:
		s.toolFinished(id, c)
	case harness.StepDone:
		tokens := int(c.Usage.Input + c.Usage.Output)
		if tokens > 0 {
			s.subagentProgress(id, nativeSafe{red: s.redactor()}, func(info *SubagentInfo) { info.TokensUsed += tokens })
		}
	case harness.Retrying, harness.Todos, harness.Steered, harness.Diag:
		// Dropped, by name. A retry discards what its step streamed, which the
		// runner's own text rule already does (childObserver); a child has no
		// todo list and takes no steer; and a Diag stays out of the stream, the
		// parent's rule (plan 019 §3.5).
	case harness.SubagentStarted, harness.SubagentEvent, harness.SubagentFinished:
		// A child starts no child of its own (depth 1, §3.2): never sent.
	}
}

// subagentProgress applies apply to child id's running row and, when a field
// a consumer reads changed, enqueues EventSubagent{progress} in the same
// section. The whole row is then put through safe, taken before the lock
// (review r8, finding 2): the payload carries the description, the prompt and
// the activity it kept from earlier events, and a key the parent learned since
// they were redacted is redacted in them here, before they go out again.
func (s *nativeSession) subagentProgress(id string, safe nativeSafe, apply func(*SubagentInfo)) {
	s.rosterMu.Lock()
	defer s.rosterMu.Unlock()
	row := s.roster[id]
	if row == nil || row.info.Status != SubagentRunning {
		return
	}
	prev := cloneSubagent(row.info)
	apply(&row.info)
	safeSubagent(&row.info, safe)
	if sameSubagentFull(prev, row.info) {
		return
	}
	s.enqueueRosterLocked(SubagentChangeProgress, row.info)
}

// subagentFinished is a child ending: its open tool rows settled to its
// outcome, its row terminal and EventSubagent{finished} in one section with the
// eviction that may follow, the evicted children's tool rows dropped, the
// barrier, and the parent's agent row stamped with how the child ended.
//
// The stamp is why the call's ToolFinished needs nothing of the roster (review
// r8, finding 3): the child's status, its duration — EndedAt − StartedAt, the
// row's own — and its final model go on the agent row here, while the roster
// row is certainly there, and the ToolFinished only closes the row with its
// receipt. A roster that has evicted the row by then — thirty-two more children
// finishing between the child's end and its call's return — no longer holds
// them, and the row closed with the call's own duration instead, measured from
// its ToolCalled, the delay and all.
//
// A background child's finish stamps nothing (plan 026 §3.11, X33; F5): its
// call's row closed with the acknowledgement long ago, and a stamp here would
// republish a parent tool row outside its turn — possibly in the middle of a
// later turn's reply, whose stream run the fold's upsert would close — or,
// for a child that finished before the call even returned, merge a running
// row's snapshot and publish it after the ToolFinished that closed the row.
// The roster row is where a background child's outcome is read.
func (s *nativeSession) subagentFinished(e harness.SubagentFinished) {
	if e.ID == "" {
		return
	}
	// Taken before any lock, while the child is still registered with the
	// harness: it covers a key only the child knows (redactor).
	safe := nativeSafe{red: s.redactor()}
	status := finishStatus(e.Status)
	model := safe.text(e.Model)
	tokens := int(e.Usage.Input + e.Usage.Output)

	// Settled, and published directly, before the roster's finished is
	// enqueued: a child's rows are all terminal by the time it is.
	settled := toolCompleted
	switch status {
	case SubagentFailed:
		settled = toolFailed
	case SubagentCancelled:
		settled = toolCancelled
	}
	s.settleChildTools(e.ID, settled)

	s.rosterMu.Lock()
	row := s.roster[e.ID]
	if row == nil || row.info.Status != SubagentRunning {
		// Never spawned here, or already finished: nothing to say again.
		s.rosterMu.Unlock()
		return
	}
	info := &row.info
	info.Status = status
	info.Error = e.Error // raw: the section's safeSubagent below redacts it with the rest
	info.Output = e.Text
	if model != "" {
		info.Model = model
	}
	info.EndedAt = s.stampEndLocked(e.At)
	// The row's own two stamps, on the one clock both are read from (the
	// parent's, harness.Options.Now): a duration a client can check against
	// the row it came with.
	info.DurationMs = max(0, int(info.EndedAt.Sub(info.StartedAt).Milliseconds()))
	info.ToolCalls = e.ToolCalls
	info.Turns = e.Steps
	info.TokensUsed = tokens
	// The whole row, not only what this event brought (review r8, finding 2).
	safeSubagent(info, safe)
	s.finishSeq++
	row.finish = s.finishSeq
	s.enqueueRosterLocked(SubagentChangeFinished, *info)
	done := cloneSubagent(*info)
	evicted := s.evictFinishedLocked(e.ID)
	s.rosterMu.Unlock()

	s.dropChildTools(evicted)
	s.flushRoster()
	if done.Background {
		return
	}
	// After the barrier, so the agent row says the child ended only once its
	// finished has committed; published like the stamp at spawn, the row's own
	// texts redacted again (resafeTask).
	s.stampTool("", done.ToolCallID, func(t *ToolEvent) {
		resafeTask(t, safe)
		if t.Task == nil {
			t.Task = &TaskInfo{}
		}
		t.Task.Status, t.Task.DurationMs, t.Task.Model = done.Status, done.DurationMs, done.Model
	})
}

// enqueueRosterLocked enqueues the roster change change of info, cloned under
// the lock (the file's comment). info is a row the same section has put
// through safeSubagent whole (the file's "Payloads"). rosterMu is held.
func (s *nativeSession) enqueueRosterLocked(change string, info SubagentInfo) {
	payload := cloneSubagent(info)
	s.log.Enqueue(Event{Type: EventSubagent, Subagent: &payload, SubagentChange: change, At: s.Now()})
}

// flushRoster is the barrier behind spawned and finished (the file's comment):
// Background and the session's done, called with no lock held.
func (s *nativeSession) flushRoster() { _ = s.log.Flush(context.Background(), s.done) }

// stampEndLocked is a finishing row's EndedAt: at on the wall clock — its
// monotonic reading dropped, since what a client decodes and orders by has
// none — moved 1 ns past the previous finish when it is not after it, so the
// roster's finish order and its EndedAt order are one order. rosterMu is held.
func (s *nativeSession) stampEndLocked(at time.Time) time.Time {
	end := wallClock(at, s.Now)
	if !end.After(s.lastEnded) {
		end = s.lastEnded.Add(time.Nanosecond)
	}
	s.lastEnded = end
	return end
}

// wallClock is at without its monotonic reading, or now's when at is zero.
func wallClock(at time.Time, now func() time.Time) time.Time {
	if at.IsZero() {
		at = now()
	}
	return at.Round(0)
}

// evictFinishedLocked is the live session's rule (subagents.go's
// evictFinishedLocked), which internal/transcript's evictFinished replicates:
// the finished rows past subagentFinishedCap go, oldest finish first — by
// EndedAt, then finish order — and never keep, the row that just finished. It
// returns the ids it evicted, whose tool rows the caller drops once rosterMu
// is released. rosterMu is held.
func (s *nativeSession) evictFinishedLocked(keep string) []string {
	var finished []string
	for _, id := range s.rosterOrder {
		if st := s.roster[id].info.Status; st != SubagentRunning && st != "" {
			finished = append(finished, id)
		}
	}
	if len(finished) <= subagentFinishedCap {
		return nil
	}
	slices.SortStableFunc(finished, func(x, y string) int {
		a, b := s.roster[x], s.roster[y]
		if c := a.info.EndedAt.Compare(b.info.EndedAt); c != 0 {
			return c
		}
		switch {
		case a.finish < b.finish:
			return -1
		case a.finish > b.finish:
			return 1
		}
		return 0
	})
	var evicted []string
	for len(finished) > subagentFinishedCap {
		drop := slices.IndexFunc(finished, func(id string) bool { return id != keep })
		if drop < 0 {
			break
		}
		id := finished[drop]
		finished = slices.Delete(finished, drop, drop+1)
		delete(s.roster, id)
		if i := slices.Index(s.rosterOrder, id); i >= 0 {
			s.rosterOrder = slices.Delete(s.rosterOrder, i, i+1)
		}
		evicted = append(evicted, id)
	}
	return evicted
}

// rosterRows is the roster in spawn order, cloned, for Snapshot.
func (s *nativeSession) rosterRows() []SubagentInfo {
	s.rosterMu.Lock()
	defer s.rosterMu.Unlock()
	if len(s.rosterOrder) == 0 {
		return nil
	}
	out := make([]SubagentInfo, 0, len(s.rosterOrder))
	for _, id := range s.rosterOrder {
		out = append(out, cloneSubagent(s.roster[id].info))
	}
	return out
}

// noteSubagentUsage journals what the parent's step spent on sub-agents when
// the step's tool entry, which would have carried it as subagent_usage, was not
// written: an append that failed, or a step refused for its call ids (plan
// 026 §3.7, panel P40). A saved step's rows are in the transcript already, and
// a step with no sub-agent has none. DiagNote takes scalars only, so each row
// is its own numbered fields — provider_1, model_1, wire_model_1, input_1,
// output_1, reasoning_1, cache_read_1, cache_creation_1, then _2 — beside the
// step, the save's error and how many rows the step had. The strings are the
// table's names, redacted all the same, as every journal line from here is.
func (s *nativeSession) noteSubagentUsage(e harness.StepDone) {
	if e.Saved || len(e.SubagentUsage) == 0 {
		return
	}
	red := s.redactor()
	s.noteUsageRows(e.SubagentUsage, red, func() map[string]any {
		return map[string]any{"step": e.Step, "save_error": red(e.SaveError)}
	})
}

// subagentUndelivered journals a background child whose result was never
// delivered to the model (harness.SubagentUndelivered, plan 026 §3.11, astra
// r14): what it spent has no entry to live in, so it goes to the journal as a
// subagent_usage note of noteSubagentUsage's shape, with the child's id, its
// type and undelivered: true beside the rows — and one note even when it
// spent nothing, so the child is on the record. It arrives during Close,
// after the session's done has closed: nothing is published, and the log
// still accepts notes until it closes, which native's Close does last.
func (s *nativeSession) subagentUndelivered(e harness.SubagentUndelivered) {
	if e.ID == "" {
		return
	}
	red := s.redactor()
	s.noteUsageRows(e.Usage, red, func() map[string]any {
		return map[string]any{"subagent": e.ID, "type": red(e.Type), "undelivered": true}
	})
}

// noteUsageRows writes rows as subagent_usage notes of at most
// subagentUsageRowsPerNote each, the numbered per-row fields beside base's,
// which is built afresh for every note. No rows is one note of base alone.
func (s *nativeSession) noteUsageRows(rows []harness.ModelUsage, red func(string) string, base func() map[string]any) {
	for start := 0; start == 0 || start < len(rows); start += subagentUsageRowsPerNote {
		end := min(start+subagentUsageRowsPerNote, len(rows))
		fields := base()
		fields["rows"] = len(rows)
		for i := start; i < end; i++ {
			r, n := rows[i], strconv.Itoa(i+1)
			fields["provider_"+n] = red(r.Provider)
			fields["model_"+n] = red(r.Model)
			fields["wire_model_"+n] = red(r.WireModel)
			fields["input_"+n] = r.Usage.Input
			fields["output_"+n] = r.Usage.Output
			fields["reasoning_"+n] = r.Usage.Reasoning
			fields["cache_read_"+n] = r.Usage.CacheRead
			fields["cache_creation_"+n] = r.Usage.CacheCreation
		}
		s.log.Note(journal.DiagNote{Kind: diagSubagentUsage, Fields: fields})
	}
}
