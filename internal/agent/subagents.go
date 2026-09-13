package agent

import (
	"bytes"
	"encoding/json"
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
)

type subagentRec struct {
	info       SubagentInfo
	userChunks int
	tools      map[string]ToolEvent
	toolOrder  []string
	evicted    map[string]struct{}
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
		return strings.HasPrefix(t.Title, "Task:") || subagentTitleRe.MatchString(t.Title)
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

func (s *session) handleSpawnedLocked(n acp.SubagentNotification) []Event {
	id := n.SubagentID
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
		s.applySpawnFieldsLocked(&rec.info, n)
		join := s.joinByDescriptionLocked(id)
		info := cloneSubagent(rec.info)
		return append([]Event{{Type: EventSubagent, Subagent: &info, SubagentChange: SubagentChangeSpawned}}, join...)
	}
	s.newSubagentLocked(n)
	join := s.joinByDescriptionLocked(id)
	info := cloneSubagent(s.subagents[id].info)
	return append([]Event{{Type: EventSubagent, Subagent: &info, SubagentChange: SubagentChangeSpawned}}, join...)
}

func (s *session) applySpawnFieldsLocked(info *SubagentInfo, n acp.SubagentNotification) {
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
	info.Transcript = s.provider().Capabilities().SubagentTranscript
	capSubagent(info)
}

func (s *session) newSubagentLocked(n acp.SubagentNotification) *subagentRec {
	parent := n.ParentSessionID
	if parent == s.sessionID {
		parent = ""
	}
	rec := &subagentRec{
		info: SubagentInfo{
			ID:           n.SubagentID,
			AttemptID:    n.AttemptID,
			ParentID:     parent,
			Description:  n.Description,
			SubagentType: n.SubagentType,
			Model:        n.Model,
			Status:       SubagentRunning,
			StartedAt:    time.Now(),
			Transcript:   s.provider().Capabilities().SubagentTranscript,
		},
		tools:   make(map[string]ToolEvent),
		evicted: make(map[string]struct{}),
	}
	capSubagent(&rec.info)
	s.subagents[n.SubagentID] = rec
	s.subagentOrder = append(s.subagentOrder, n.SubagentID)
	return rec
}

func (s *session) handleProgressLocked(n acp.SubagentNotification) []Event {
	rec := s.subagents[n.SubagentID]
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
	rec := s.subagents[n.SubagentID]
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
	s.evictFinishedLocked()
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

func (s *session) evictFinishedLocked() {
	var finished []string
	for _, id := range s.subagentOrder {
		rec := s.subagents[id]
		if rec != nil && rec.info.Status != SubagentRunning {
			finished = append(finished, id)
		}
	}
	for len(finished) > subagentFinishedCap {
		drop := finished[0]
		finished = finished[1:]
		delete(s.subagents, drop)
		s.removeSubagentOrderLocked(drop)
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

func (s *session) toolStoreLocked(owner string) (map[string]ToolEvent, *[]string, map[string]struct{}, int) {
	if owner == "" {
		if s.tools == nil {
			s.tools = make(map[string]ToolEvent)
		}
		return s.tools, &s.toolOrder, nil, 0
	}
	rec := s.subagents[owner]
	if rec == nil {
		return nil, nil, nil, 0
	}
	if rec.tools == nil {
		rec.tools = make(map[string]ToolEvent)
	}
	if rec.evicted == nil {
		rec.evicted = make(map[string]struct{})
	}
	return rec.tools, &rec.toolOrder, rec.evicted, childToolCap
}

func (s *session) evictChildToolLocked(tools map[string]ToolEvent, order *[]string, evicted map[string]struct{}) {
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
	if evicted != nil {
		evicted[id] = struct{}{}
	}
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
			tdesc = t.Task.Description
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
			t.Task.AgentID = ""
			m[toolID] = t
		})
		return
	}
	t, ok := tools[toolID]
	if !ok || t.Task == nil {
		return
	}
	if t.Task.AgentID == subID || t.Task.AgentID == "" {
		t.Task.AgentID = ""
		tools[toolID] = t
	}
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
	s.evictFinishedLocked()
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
