package agent

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/charliek/craze/internal/acp"
)

const (
	updateUserMessage    = "user_message_chunk"
	subagentDescCap      = 256
	subagentTypeCap      = 64
	subagentModelCap     = 128
	subagentErrorCap     = 512
	subagentOutputCap    = 4 * 1024
	subagentActivityCap  = 256
	subagentToolsUsedCap = 8
	subagentFinishedCap  = 32
	childToolCap         = 256
	// childEvictedCap bounds the tombstones of evicted child tool ids, kept so
	// a late delta for a dropped id is ignored rather than resurrecting it.
	// Past the cap only childEvictedKeep of the newest survive: a delta for an
	// id evicted that many tools ago is not coming.
	childEvictedCap  = 512
	childEvictedKeep = 256
)

type subagentRec struct {
	info       SubagentInfo
	userChunks int
	tools      map[string]ToolEvent
	toolOrder  []string
	evicted    map[string]struct{}
	// evictedOrder is the insertion order of evicted, so the set can be
	// trimmed to its newest entries instead of growing for the whole session.
	evictedOrder []string
	// unrouted marks a child past the routing cap (§3.2/§9): it gets a row but
	// no transcript, so it never owns a tool map.
	unrouted bool
	// finishSeq orders terminal records by when they finished, which spawn
	// order does not: a long-runner spawned first can finish last.
	finishSeq uint64
	// preStamp is the joined tool's Task as it was before the child's fields
	// were stamped onto it, and preStampTool the tool it belongs to, so a
	// detach restores the tool's own parsed rawInput exactly.
	preStamp     *TaskInfo
	preStampTool string
}

// isEvicted reports whether a child tool id was dropped by the tool cap.
func (r *subagentRec) isEvicted(id string) bool {
	if r == nil || r.evicted == nil {
		return false
	}
	_, ok := r.evicted[id]
	return ok
}

// noteEvicted remembers a dropped child tool id, bounded by childEvictedCap.
func (r *subagentRec) noteEvicted(id string) {
	if r == nil {
		return
	}
	if r.evicted == nil {
		r.evicted = make(map[string]struct{})
	}
	if _, dup := r.evicted[id]; dup {
		return
	}
	r.evicted[id] = struct{}{}
	r.evictedOrder = append(r.evictedOrder, id)
	if len(r.evictedOrder) <= childEvictedCap {
		return
	}
	for _, old := range r.evictedOrder[:len(r.evictedOrder)-childEvictedKeep] {
		delete(r.evicted, old)
	}
	r.evictedOrder = append([]string(nil), r.evictedOrder[len(r.evictedOrder)-childEvictedKeep:]...)
}

func (s *session) emitAll(evs []Event) {
	for i := range evs {
		s.emit(evs[i])
	}
}

func (s *session) classifyTask(t ToolEvent) bool {
	if t.IsTodoTool() {
		return false
	}
	p := s.provider()
	if p.subagentToolName != "" && t.ToolName == p.subagentToolName {
		return true
	}
	if t.ToolName == "" && p.titleTaskFallback && t.Title != "" {
		return subagentTitleRe.MatchString(t.Title)
	}
	return false
}

func cloneSubagent(in SubagentInfo) SubagentInfo {
	if in.ToolsUsed != nil {
		in.ToolsUsed = append([]string(nil), in.ToolsUsed...)
	}
	return in
}

func snapshotSubagents(order []string, recs map[string]*subagentRec) []SubagentInfo {
	if len(order) == 0 {
		return nil
	}
	out := make([]SubagentInfo, 0, len(order))
	for _, id := range order {
		if rec, ok := recs[id]; ok {
			out = append(out, cloneSubagent(rec.info))
		}
	}
	return out
}

func capSubagent(info *SubagentInfo) {
	info.Description = truncateUTF8(sanitizeText(info.Description), subagentDescCap)
	info.SubagentType = truncateUTF8(sanitizeText(info.SubagentType), subagentTypeCap)
	info.Model = truncateUTF8(sanitizeText(info.Model), subagentModelCap)
	info.Error = truncateUTF8(sanitizeText(info.Error), subagentErrorCap)
	info.Prompt = truncateUTF8(sanitizeText(info.Prompt), taskPromptCap)
	info.Output = tailUTF8(sanitizeText(info.Output), subagentOutputCap)
	info.Activity = truncateUTF8(sanitizeText(info.Activity), subagentActivityCap)
	info.ToolsUsed = tailStrings(info.ToolsUsed, subagentToolsUsedCap)
}

func tailStrings(in []string, n int) []string {
	if len(in) == 0 {
		return nil
	}
	if len(in) > n {
		in = in[len(in)-n:]
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = sanitizeText(s)
	}
	return out
}

func sameSubagentFull(a, b SubagentInfo) bool {
	if a.ID != b.ID || a.AttemptID != b.AttemptID || a.ParentID != b.ParentID ||
		a.ToolCallID != b.ToolCallID || a.Description != b.Description ||
		a.SubagentType != b.SubagentType || a.Model != b.Model || a.Status != b.Status ||
		a.Error != b.Error || a.Prompt != b.Prompt || a.Output != b.Output ||
		a.Activity != b.Activity || a.DurationMs != b.DurationMs || a.ToolCalls != b.ToolCalls ||
		a.Turns != b.Turns || a.TokensUsed != b.TokensUsed || a.Transcript != b.Transcript ||
		a.Background != b.Background || !a.StartedAt.Equal(b.StartedAt) || !a.EndedAt.Equal(b.EndedAt) {
		return false
	}
	if len(a.ToolsUsed) != len(b.ToolsUsed) {
		return false
	}
	for i := range a.ToolsUsed {
		if a.ToolsUsed[i] != b.ToolsUsed[i] {
			return false
		}
	}
	return true
}

func (s *session) onSubagent(n acp.SubagentNotification) {
	var evs []Event
	s.mu.Lock()
	switch n.Kind {
	case acp.SubagentSpawned:
		evs = s.handleSpawnedLocked(n)
	case acp.SubagentProgress:
		evs = s.handleProgressLocked(n)
	case acp.SubagentFinished:
		evs = s.handleFinishedLocked(n)
	}
	s.mu.Unlock()
	s.emitAll(evs)
}

// subagentRecOf finds a record by ChildSessionID first (the routing key) and
// falls back to SubagentID for shapes that only carry it.
func subagentRecOf(recs map[string]*subagentRec, n acp.SubagentNotification) *subagentRec {
	if rec := recs[n.ChildSessionID]; rec != nil {
		return rec
	}
	return recs[n.SubagentID]
}

func (s *session) handleSpawnedLocked(n acp.SubagentNotification) []Event {
	id := n.ChildSessionID
	if id == "" {
		id = n.SubagentID
	}
	if id == "" {
		return nil
	}
	if rec, ok := s.subagents[id]; ok {
		if rec.info.AttemptID == n.AttemptID {
			return nil
		}
		rec.info.AttemptID = n.AttemptID
		rec.info.Status = SubagentRunning
		rec.info.Error = ""
		rec.info.Output = ""
		rec.info.DurationMs = 0
		rec.info.ToolCalls = 0
		rec.info.Turns = 0
		rec.info.TokensUsed = 0
		rec.info.ToolsUsed = nil
		rec.info.EndedAt = time.Time{}
		// The record is running again, so it no longer holds a finish slot.
		rec.finishSeq = 0
		s.applySpawnFieldsLocked(rec, n)
		join := s.joinByDescriptionLocked(id)
		info := cloneSubagent(rec.info)
		return append([]Event{{Type: EventSubagent, Subagent: &info, SubagentChange: SubagentChangeSpawned}}, dropSpawnEcho(join, info)...)
	}
	if rec := s.newSubagentLocked(n); rec == nil {
		return nil
	}
	join := s.joinByDescriptionLocked(id)
	info := cloneSubagent(s.subagents[id].info)
	return append([]Event{{Type: EventSubagent, Subagent: &info, SubagentChange: SubagentChangeSpawned}}, dropSpawnEcho(join, info)...)
}

// dropSpawnEcho removes the progress event a join emits when it carries
// exactly the state the spawned event already reports: §3.1 pins that
// identical state is never re-emitted.
func dropSpawnEcho(evs []Event, spawned SubagentInfo) []Event {
	out := evs[:0]
	for _, ev := range evs {
		if ev.Type == EventSubagent && ev.Subagent != nil && sameSubagentFull(*ev.Subagent, spawned) {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func (s *session) applySpawnFieldsLocked(rec *subagentRec, n acp.SubagentNotification) {
	info := &rec.info
	parent := n.ParentSessionID
	if parent == s.sessionID {
		parent = ""
	}
	info.ParentID = parent
	if n.Description != "" {
		info.Description = n.Description
	}
	if n.SubagentType != "" {
		info.SubagentType = n.SubagentType
	}
	if n.Model != "" {
		info.Model = n.Model
	}
	// An unrouted child never gets a stream, so a resume must not claim one.
	info.Transcript = !rec.unrouted && s.provider().Capabilities().SubagentTranscript
	capSubagent(info)
}

// subagentRunCap bounds the running records the ACP router will route a
// stream for: it refuses to route past 64 active children.
//
// subagentUnroutedCap bounds the rows kept past that. §3.2/§9 pin that a 65th
// concurrent child still gets a *row*, just no transcript, so the record is
// created with Transcript false and no tool map; past this many unrouted
// running children the spawned is dropped, capping running records at 96.
const (
	subagentRunCap      = 64
	subagentUnroutedCap = 32
)

// runningSubagentsLocked counts records still running, and how many of those
// are unrouted rows.
func (s *session) runningSubagentsLocked() (total, unrouted int) {
	for _, rec := range s.subagents {
		if rec.info.Status != SubagentRunning && rec.info.Status != "" {
			continue
		}
		total++
		if rec.unrouted {
			unrouted++
		}
	}
	return total, unrouted
}

func (s *session) newSubagentLocked(n acp.SubagentNotification) *subagentRec {
	running, unrouted := s.runningSubagentsLocked()
	// The router's 64 slots are the routed children only, so a routed slot
	// freed by a finish is usable again even while unrouted rows are held.
	routed := running-unrouted < subagentRunCap
	if !routed && unrouted >= subagentUnroutedCap {
		return nil
	}
	parent := n.ParentSessionID
	if parent == s.sessionID {
		parent = ""
	}
	id := n.ChildSessionID
	if id == "" {
		id = n.SubagentID
	}
	rec := &subagentRec{
		info: SubagentInfo{
			ID:           id,
			AttemptID:    n.AttemptID,
			ParentID:     parent,
			Description:  n.Description,
			SubagentType: n.SubagentType,
			Model:        n.Model,
			Status:       SubagentRunning,
			StartedAt:    time.Now(),
			Transcript:   routed && s.provider().Capabilities().SubagentTranscript,
		},
		unrouted: !routed,
	}
	if routed {
		rec.tools = make(map[string]ToolEvent)
		rec.evicted = make(map[string]struct{})
	}
	capSubagent(&rec.info)
	s.subagents[id] = rec
	s.subagentOrder = append(s.subagentOrder, id)
	return rec
}

func (s *session) handleProgressLocked(n acp.SubagentNotification) []Event {
	rec := subagentRecOf(s.subagents, n)
	if rec == nil {
		return nil
	}
	prev := cloneSubagent(rec.info)
	s.applyProgressFieldsLocked(&rec.info, n)
	if sameSubagentFull(prev, rec.info) {
		return nil
	}
	info := cloneSubagent(rec.info)
	return []Event{{Type: EventSubagent, Subagent: &info, SubagentChange: SubagentChangeProgress}}
}

func (s *session) applyProgressFieldsLocked(info *SubagentInfo, n acp.SubagentNotification) {
	if n.DurationMs > 0 {
		info.DurationMs = n.DurationMs
	}
	if n.ToolCalls > 0 {
		info.ToolCalls = n.ToolCalls
	}
	if n.Turns > 0 {
		info.Turns = n.Turns
	}
	if n.TokensUsed > 0 {
		info.TokensUsed = n.TokensUsed
	}
	if len(n.ToolsUsed) > 0 {
		info.ToolsUsed = tailStrings(n.ToolsUsed, subagentToolsUsedCap)
	}
	capSubagent(info)
}

func (s *session) handleFinishedLocked(n acp.SubagentNotification) []Event {
	rec := subagentRecOf(s.subagents, n)
	if rec == nil {
		return nil
	}
	prev := cloneSubagent(rec.info)
	wasTerminal := prev.Status != SubagentRunning && prev.Status != ""
	status := finishStatus(n.Status)
	rec.info.Status = status
	rec.info.Error = n.Error
	if n.Output != "" {
		rec.info.Output = n.Output
	}
	if n.DurationMs > 0 {
		rec.info.DurationMs = n.DurationMs
	}
	if n.ToolCalls > 0 {
		rec.info.ToolCalls = n.ToolCalls
	}
	if n.Turns > 0 {
		rec.info.Turns = n.Turns
	}
	// A zeroed counter on finished (seen live on cancel: tokens_used:0 after
	// progress had reported 1186) keeps the last progress value on purpose.
	if n.TokensUsed > 0 {
		rec.info.TokensUsed = n.TokensUsed
	}
	if rec.info.EndedAt.IsZero() {
		rec.info.EndedAt = time.Now()
	}
	capSubagent(&rec.info)

	if wasTerminal && prev.Status == rec.info.Status {
		if sameSubagentFull(prev, rec.info) {
			return nil
		}
		info := cloneSubagent(rec.info)
		return []Event{{Type: EventSubagent, Subagent: &info, SubagentChange: SubagentChangeProgress}}
	}

	evs := s.settleChildToolsLocked(rec)
	evs = append(evs, s.remergeJoinedTaskLocked(rec)...)
	s.stampFinishLocked(rec)
	s.evictFinishedLocked(rec.info.ID)
	info := cloneSubagent(rec.info)
	evs = append(evs, Event{Type: EventSubagent, Subagent: &info, SubagentChange: SubagentChangeFinished})
	return evs
}

func finishStatus(s string) SubagentStatus {
	switch SubagentStatus(s) {
	case SubagentFailed:
		return SubagentFailed
	case SubagentCancelled:
		return SubagentCancelled
	default:
		return SubagentCompleted
	}
}

func (s *session) settleChildToolsLocked(rec *subagentRec) []Event {
	st := "completed"
	switch rec.info.Status {
	case SubagentCancelled:
		st = "cancelled"
	case SubagentFailed:
		st = "failed"
	}
	var evs []Event
	for _, id := range rec.toolOrder {
		t, ok := rec.tools[id]
		if !ok || !toolStatusInFlight(t.Status) {
			continue
		}
		t.Status = st
		t.At = time.Now()
		rec.tools[id] = t
		ct := cloneTool(t)
		evs = append(evs, Event{Type: EventTool, Agent: rec.info.ID, Tool: &ct})
	}
	return evs
}

func (s *session) remergeJoinedTaskLocked(rec *subagentRec) []Event {
	if rec.info.ToolCallID == "" {
		return nil
	}
	owner := rec.info.ParentID
	tools, _, _, _ := s.toolStoreLocked(owner)
	if tools == nil {
		return nil
	}
	t, ok := tools[rec.info.ToolCallID]
	if !ok {
		return nil
	}
	prev := t.Task
	t.Task = mergeTaskInfo(t.Task, TaskInfo{
		AgentID:      rec.info.ID,
		Description:  rec.info.Description,
		Prompt:       rec.info.Prompt,
		SubagentType: rec.info.SubagentType,
		Model:        rec.info.Model,
		Status:       rec.info.Status,
		DurationMs:   rec.info.DurationMs,
		Background:   rec.info.Background,
	})
	if sameTask(prev, t.Task) {
		return nil
	}
	tools[rec.info.ToolCallID] = t
	ct := cloneTool(t)
	return []Event{{Type: EventTool, Agent: owner, Tool: &ct}}
}

// stampFinishLocked gives a record its place in finish order. It is claimed
// once per run: a "last finished wins" restatement of an already-terminal
// record must not make it the newest finish.
func (s *session) stampFinishLocked(rec *subagentRec) {
	if rec.finishSeq != 0 {
		return
	}
	s.subagentFinishSeq++
	rec.finishSeq = s.subagentFinishSeq
}

// evictFinishedLocked drops finished records past subagentFinishedCap, oldest
// *finish* first — not oldest spawn. A long-runner spawned first can finish
// after 32 newer children have come and gone; evicting it by spawn order
// would delete the record its own finished event is about, so the event would
// describe a row no snapshot ever held. keep is the record that just
// finished; it is never the one evicted.
func (s *session) evictFinishedLocked(keep string) {
	var finished []string
	for _, id := range s.subagentOrder {
		rec := s.subagents[id]
		if rec != nil && rec.info.Status != SubagentRunning && rec.info.Status != "" {
			finished = append(finished, id)
		}
	}
	if len(finished) <= subagentFinishedCap {
		return
	}
	sort.SliceStable(finished, func(i, j int) bool {
		a, b := s.subagents[finished[i]], s.subagents[finished[j]]
		if !a.info.EndedAt.Equal(b.info.EndedAt) {
			return a.info.EndedAt.Before(b.info.EndedAt)
		}
		return a.finishSeq < b.finishSeq
	})
	for len(finished) > subagentFinishedCap {
		drop := -1
		for i, id := range finished {
			if id != keep {
				drop = i
				break
			}
		}
		if drop < 0 {
			return
		}
		id := finished[drop]
		finished = append(finished[:drop], finished[drop+1:]...)
		delete(s.subagents, id)
		s.removeSubagentOrderLocked(id)
	}
}

func (s *session) removeSubagentOrderLocked(id string) {
	for i, x := range s.subagentOrder {
		if x == id {
			s.subagentOrder = append(s.subagentOrder[:i], s.subagentOrder[i+1:]...)
			return
		}
	}
}

func (s *session) onChildUpdate(id, wireName string, u sessionUpdateWire) {
	switch u.SessionUpdate {
	case acp.UpdateAgentMessage:
		if !s.knownSubagent(id) {
			return
		}
		s.emit(Event{Type: EventText, Agent: id, Text: messageText(u.Content)})
	case acp.UpdateAgentThought:
		if !s.knownSubagent(id) {
			return
		}
		s.emit(Event{Type: EventThought, Agent: id, Text: messageText(u.Content)})
	case updateUserMessage:
		text := messageText(u.Content)
		s.mu.Lock()
		rec := s.subagents[id]
		if rec == nil {
			s.mu.Unlock()
			return
		}
		if rec.userChunks == 0 {
			rec.info.Prompt = text
		} else {
			rec.info.Prompt += text
		}
		rec.userChunks++
		rec.info.Prompt = truncateUTF8(rec.info.Prompt, taskPromptCap)
		s.mu.Unlock()
		s.emit(Event{Type: EventUser, Agent: id, Text: text})
	case acp.UpdateToolCall, acp.UpdateToolCallUpd:
		if !s.knownSubagent(id) {
			return
		}
		delta, ok := toolDeltaFromWire(u)
		if !ok {
			return
		}
		delta.wireName = wireName
		tool, changed, extras := s.applyToolDelta(id, delta)
		if changed {
			s.emit(Event{Type: EventTool, Agent: id, Tool: &tool})
		}
		s.emitAll(extras)
	}
}

func (s *session) knownSubagent(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.subagents[id]
	return ok
}

// toolStoreLocked returns the tool map of a session: the main one for "", a
// child's private one otherwise. The third result is the owning record (nil
// for the main session) so the caller can read and extend its evicted set.
// An unrouted child (§3.2) has no store at all — nothing is ever routed to it.
func (s *session) toolStoreLocked(owner string) (map[string]ToolEvent, *[]string, *subagentRec, int) {
	if owner == "" {
		if s.tools == nil {
			s.tools = make(map[string]ToolEvent)
		}
		return s.tools, &s.toolOrder, nil, 0
	}
	rec := s.subagents[owner]
	if rec == nil || rec.unrouted {
		return nil, nil, nil, 0
	}
	if rec.tools == nil {
		rec.tools = make(map[string]ToolEvent)
	}
	return rec.tools, &rec.toolOrder, rec, childToolCap
}

func (s *session) evictChildToolLocked(tools map[string]ToolEvent, order *[]string, rec *subagentRec) {
	pick := func(terminal bool) string {
		for _, id := range *order {
			t := tools[id]
			if toolStatusInFlight(t.Status) != terminal {
				return id
			}
		}
		return ""
	}
	id := pick(true)
	if id == "" {
		id = pick(false)
	}
	if id == "" {
		return
	}
	delete(tools, id)
	rec.noteEvicted(id)
	for i, x := range *order {
		if x == id {
			*order = append((*order)[:i], (*order)[i+1:]...)
			return
		}
	}
}

func (s *session) joinByDescriptionLocked(id string) []Event {
	rec := s.subagents[id]
	if rec == nil || rec.info.Description == "" {
		return nil
	}
	owner := rec.info.ParentID
	tools, order, _, _ := s.toolStoreLocked(owner)
	if tools == nil || order == nil {
		return nil
	}
	spawnName := s.provider().subagentToolName
	found := ""
	for i := len(*order) - 1; i >= 0; i-- {
		tid := (*order)[i]
		t := tools[tid]
		if t.ToolName != spawnName {
			continue
		}
		if s.toolCallJoinedLocked(tid, owner) {
			continue
		}
		tdesc := ""
		if t.Task != nil {
			// The record's description went through capSubagent, so the tool's
			// raw one has to be capped the same way or anything over
			// subagentDescCap could never match.
			tdesc = truncateUTF8(sanitizeText(t.Task.Description), subagentDescCap)
		}
		if tdesc == rec.info.Description {
			found = tid
			break
		}
	}
	if found == "" {
		return nil
	}
	return s.attachSubagentLocked(id, found, owner)
}

func (s *session) joinFromOutputLocked(owner string, tool ToolEvent, raw, content json.RawMessage) []Event {
	if tool.ToolName != s.provider().subagentToolName {
		return nil
	}
	id := subagentIDFromSpawn(raw, content)
	if id == "" {
		return nil
	}
	if s.subagents[id] == nil {
		return nil
	}
	return s.attachSubagentLocked(id, tool.ID, owner)
}

func (s *session) toolCallJoinedLocked(toolID, owner string) bool {
	for _, rec := range s.subagents {
		if rec.info.ToolCallID == toolID && rec.info.ParentID == owner {
			return true
		}
	}
	return false
}

func (s *session) attachSubagentLocked(subID, toolID, owner string) []Event {
	rec := s.subagents[subID]
	if rec == nil {
		return nil
	}
	type touched struct{ id, owner string }
	var changed []touched
	note := func(tid, own string) {
		for _, c := range changed {
			if c.id == tid && c.owner == own {
				return
			}
		}
		changed = append(changed, touched{tid, own})
	}
	// A join rewrites the record too (ToolCallID, and the prompt/background it
	// learns off the tool). Without an EventSubagent for that, a --json
	// consumer would not see toolCallId until the child finished. Kept in a
	// slice, not a map, so the emitted order is deterministic.
	type before struct {
		id   string
		info SubagentInfo
	}
	prevInfos := []before{{subID, cloneSubagent(rec.info)}}

	if rec.info.ToolCallID != "" && rec.info.ToolCallID != toolID {
		oldOwner := rec.info.ParentID
		s.clearToolJoinLocked(rec.info.ToolCallID, subID, oldOwner)
		note(rec.info.ToolCallID, oldOwner)
	}
	s.eachToolStoreLocked(func(own string, tools map[string]ToolEvent) {
		for tid, t := range tools {
			if t.Task != nil && t.Task.AgentID == subID && tid != toolID {
				s.clearToolJoinLocked(tid, subID, own)
				note(tid, own)
			}
		}
	})
	for oid, orec := range s.subagents {
		if oid != subID && orec.info.ToolCallID == toolID && orec.info.ParentID == owner {
			old := orec.info.ToolCallID
			prevInfos = append(prevInfos, before{oid, cloneSubagent(orec.info)})
			orec.info.ToolCallID = ""
			s.clearToolJoinLocked(old, oid, orec.info.ParentID)
			note(old, orec.info.ParentID)
		}
	}

	prevTask := (*TaskInfo)(nil)
	if tools, _, _, _ := s.toolStoreLocked(owner); tools != nil {
		if t, ok := tools[toolID]; ok {
			prevTask = t.Task
		}
	}
	rec.info.ToolCallID = toolID
	s.stampJoinedToolLocked(toolID, owner, rec)
	if tools, _, _, _ := s.toolStoreLocked(owner); tools != nil {
		if t, ok := tools[toolID]; ok && !sameTask(prevTask, t.Task) {
			note(toolID, owner)
		}
	}

	var evs []Event
	for _, c := range changed {
		tools, _, _, _ := s.toolStoreLocked(c.owner)
		if tools == nil {
			continue
		}
		t, ok := tools[c.id]
		if !ok {
			continue
		}
		ct := cloneTool(t)
		evs = append(evs, Event{Type: EventTool, Agent: c.owner, Tool: &ct})
	}
	for _, b := range prevInfos {
		r := s.subagents[b.id]
		if r == nil || sameSubagentFull(b.info, r.info) {
			continue
		}
		info := cloneSubagent(r.info)
		evs = append(evs, Event{Type: EventSubagent, Subagent: &info, SubagentChange: SubagentChangeProgress})
	}
	return evs
}

func (s *session) clearToolJoinLocked(toolID, subID, owner string) {
	tools, _, _, _ := s.toolStoreLocked(owner)
	if tools == nil {
		s.eachToolStoreLocked(func(own string, m map[string]ToolEvent) {
			t, ok := m[toolID]
			if !ok || t.Task == nil || t.Task.AgentID != subID {
				return
			}
			m[toolID] = s.unstampToolLocked(t, toolID, subID)
		})
		return
	}
	t, ok := tools[toolID]
	if !ok || t.Task == nil {
		return
	}
	if t.Task.AgentID == subID || t.Task.AgentID == "" {
		tools[toolID] = s.unstampToolLocked(t, toolID, subID)
	}
}

// unstampToolLocked undoes stampJoinedToolLocked. Detaching a child has to
// take back everything the join wrote — the child's status, model, type and
// prompt, not just its id — or the tool's row keeps describing a child that
// is no longer its own. The pre-join Task is restored verbatim, so whatever
// the tool parsed out of its own rawInput survives; without that snapshot
// only the fields that can only have come from the child are cleared.
func (s *session) unstampToolLocked(t ToolEvent, toolID, subID string) ToolEvent {
	if rec := s.subagents[subID]; rec != nil && rec.preStampTool == toolID {
		task := TaskInfo{}
		if rec.preStamp != nil {
			task = *rec.preStamp
		}
		rec.preStamp, rec.preStampTool = nil, ""
		t.Task = &task
		return t
	}
	task := *t.Task
	task.AgentID = ""
	task.Status = ""
	task.Model = ""
	t.Task = &task
	return t
}

func (s *session) stampJoinedToolLocked(toolID, owner string, rec *subagentRec) {
	tools, _, _, _ := s.toolStoreLocked(owner)
	if tools == nil {
		return
	}
	t, ok := tools[toolID]
	if !ok {
		return
	}
	if t.Task != nil && t.Task.Background {
		rec.info.Background = true
	}
	if rec.info.Prompt == "" && t.Task != nil && t.Task.Prompt != "" {
		rec.info.Prompt = t.Task.Prompt
	}
	rec.preStamp, rec.preStampTool = nil, toolID
	if t.Task != nil {
		task := *t.Task
		rec.preStamp = &task
	}
	t.Task = mergeTaskInfo(t.Task, TaskInfo{
		AgentID:      rec.info.ID,
		Description:  rec.info.Description,
		Prompt:       rec.info.Prompt,
		SubagentType: rec.info.SubagentType,
		Model:        rec.info.Model,
		Status:       rec.info.Status,
		Background:   rec.info.Background,
	})
	tools[toolID] = t
}

func (s *session) eachToolStoreLocked(fn func(owner string, tools map[string]ToolEvent)) {
	if s.tools != nil {
		fn("", s.tools)
	}
	for id, rec := range s.subagents {
		if rec.tools != nil {
			fn(id, rec.tools)
		}
	}
}

func (s *session) syncCursorLocked(tool ToolEvent) []Event {
	if s.provider().subagentToolName != "task" || tool.Task == nil {
		return nil
	}
	rec, ok := s.subagents[tool.ID]
	if !ok {
		// Cursor synthesises a record per task tool, so the same running bound
		// grok's spawns get applies here. There is no transcript to lose, so a
		// record past the cap is simply dropped.
		if running, _ := s.runningSubagentsLocked(); running >= subagentRunCap {
			return nil
		}
		rec = &subagentRec{
			info: SubagentInfo{
				ID:         tool.ID,
				ToolCallID: tool.ID,
				Status:     SubagentRunning,
				StartedAt:  time.Now(),
			},
			tools:   make(map[string]ToolEvent),
			evicted: make(map[string]struct{}),
		}
		s.fillCursorLocked(rec, tool)
		s.subagents[tool.ID] = rec
		s.subagentOrder = append(s.subagentOrder, tool.ID)
		s.writeCursorTaskLocked(tool.ID, rec)
		info := cloneSubagent(rec.info)
		evs := []Event{{Type: EventSubagent, Subagent: &info, SubagentChange: SubagentChangeSpawned}}
		if terminalToolStatus(tool.Status) {
			s.applyCursorTerminalLocked(rec, tool)
			fin := cloneSubagent(rec.info)
			evs = append(evs, Event{Type: EventSubagent, Subagent: &fin, SubagentChange: SubagentChangeFinished})
		}
		return evs
	}
	prev := cloneSubagent(rec.info)
	s.fillCursorLocked(rec, tool)
	if terminalToolStatus(tool.Status) && prev.Status == SubagentRunning {
		s.applyCursorTerminalLocked(rec, tool)
		info := cloneSubagent(rec.info)
		return []Event{{Type: EventSubagent, Subagent: &info, SubagentChange: SubagentChangeFinished}}
	}
	s.writeCursorTaskLocked(tool.ID, rec)
	if sameSubagentFull(prev, rec.info) {
		return nil
	}
	info := cloneSubagent(rec.info)
	change := SubagentChangeProgress
	if rec.info.Status != SubagentRunning && prev.Status != rec.info.Status {
		change = SubagentChangeFinished
	}
	return []Event{{Type: EventSubagent, Subagent: &info, SubagentChange: change}}
}

func (s *session) writeCursorTaskLocked(toolID string, rec *subagentRec) {
	t, ok := s.tools[toolID]
	if !ok {
		return
	}
	t.Task = mergeTaskInfo(t.Task, TaskInfo{
		Status:       rec.info.Status,
		Description:  rec.info.Description,
		Prompt:       rec.info.Prompt,
		Model:        rec.info.Model,
		SubagentType: rec.info.SubagentType,
		DurationMs:   rec.info.DurationMs,
	})
	s.tools[toolID] = t
}

func (s *session) fillCursorLocked(rec *subagentRec, tool ToolEvent) {
	if tool.Task != nil {
		if tool.Task.Description != "" {
			rec.info.Description = tool.Task.Description
		}
		if tool.Task.Prompt != "" {
			rec.info.Prompt = tool.Task.Prompt
		}
		if tool.Task.Model != "" {
			rec.info.Model = tool.Task.Model
		}
		if tool.Task.SubagentType != "" {
			rec.info.SubagentType = tool.Task.SubagentType
		}
		if tool.Task.DurationMs > 0 {
			rec.info.DurationMs = tool.Task.DurationMs
		}
		// Cursor's record id is the tool call, so there is nothing to copy here.
		// The receipt's agent id stays on Task, not on the record.
	}
	if rec.info.Description == "" {
		rec.info.Description = strings.TrimPrefix(tool.Title, "Task: ")
	}
	rec.info.ToolCallID = tool.ID
	capSubagent(&rec.info)
}

func (s *session) applyCursorTerminalLocked(rec *subagentRec, tool ToolEvent) {
	rec.info.Status = toolStatusToSubagent(tool.Status)
	if rec.info.EndedAt.IsZero() {
		rec.info.EndedAt = time.Now()
	}
	if tool.Task != nil && tool.Task.DurationMs > 0 {
		rec.info.DurationMs = tool.Task.DurationMs
	}
	if tool.Output != nil && tool.Output.Content != "" {
		rec.info.Output = tool.Output.Content
	} else if tool.ContentText != "" {
		rec.info.Output = tool.ContentText
	}
	capSubagent(&rec.info)
	s.writeCursorTaskLocked(tool.ID, rec)
	s.stampFinishLocked(rec)
	s.evictFinishedLocked(rec.info.ID)
}

func terminalToolStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

func toolStatusToSubagent(status string) SubagentStatus {
	switch status {
	case "failed":
		return SubagentFailed
	case "cancelled":
		return SubagentCancelled
	case "completed":
		return SubagentCompleted
	default:
		return SubagentRunning
	}
}

func subagentIDFromSpawn(raw, content json.RawMessage) string {
	if id := subagentIDFromText(grokOutputText(raw)); id != "" {
		return id
	}
	return subagentIDFromText(contentSpawnText(content))
}

func grokOutputText(raw json.RawMessage) string {
	raw, ok := presentJSON(raw)
	if !ok || raw[0] != '{' {
		return ""
	}
	var w struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return ""
	}
	return w.Text
}

func contentSpawnText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return ""
	}
	var items []acp.ToolContent
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	var b strings.Builder
	for _, it := range items {
		if it.Type != "content" || it.Content == nil || it.Content.Type != "text" {
			continue
		}
		b.WriteString(it.Content.Text)
	}
	return b.String()
}

func subagentIDFromText(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "subagent_id:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
