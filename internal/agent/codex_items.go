package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charliek/craze/internal/codexrpc"
)

// Item & diff translation for codexSession (Plan 054 § D2).

const codexCmdStreamCap = 128 << 10 // cap accumulated live command output per call

// codexFileChangeToToolDiff converts one Codex FileUpdateChange into Craze's
// ToolDiff{Path, OldText, NewText, Added, Removed, Truncated} contract so
// internal/tui/transcript.go renders real diff lines for add, delete, update,
// and move/rename operations.
func codexFileChangeToToolDiff(ch codexrpc.FileUpdateChange) ToolDiff {
	path := ch.Path
	if ch.Kind.MovePath != "" {
		path = ch.Kind.MovePath
	}
	switch ch.Kind.Type {
	case "add":
		return diffOf(path, "", ch.Diff)
	case "delete":
		return diffOf(path, ch.Diff, "")
	case "update":
		oldText, newText := unifiedDiffToOldNew(ch.Diff)
		return diffOf(path, oldText, newText)
	default:
		oldText, newText := unifiedDiffToOldNew(ch.Diff)
		if oldText == "" && newText == "" && ch.Diff != "" {
			return diffOf(path, "", ch.Diff)
		}
		return diffOf(path, oldText, newText)
	}
}

// unifiedDiffToOldNew reconstructs paired OldText and NewText strings from a
// unified diff patch (hunks starting with `@@ ... @@`). Context lines (` `)
// belong to both, `-` lines to OldText, `+` lines to NewText, while file
// headers (`---`, `+++`, `diff --git`, `index`, `\ No newline`) are skipped.
func unifiedDiffToOldNew(patch string) (oldText, newText string) {
	if patch == "" {
		return "", ""
	}
	lines := strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n")
	var oldLines, newLines []string
	inHunk := false
	hasAnyHunk := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "@@ "):
			inHunk = true
			hasAnyHunk = true
		case strings.HasPrefix(line, "diff --git "),
			strings.HasPrefix(line, "index "),
			strings.HasPrefix(line, "new file mode "),
			strings.HasPrefix(line, "deleted file mode "),
			strings.HasPrefix(line, "rename from "),
			strings.HasPrefix(line, "rename to "),
			strings.HasPrefix(line, "--- "),
			strings.HasPrefix(line, "+++ "):
			if !inHunk {
				continue
			}
			// Inside a hunk, "--- foo" is a removal of "-- foo" and "+++ foo"
			// is an addition of "++ foo".
			if strings.HasPrefix(line, "-") {
				oldLines = append(oldLines, line[1:])
			} else if strings.HasPrefix(line, "+") {
				newLines = append(newLines, line[1:])
			}
		case strings.HasPrefix(line, `\ No newline at end of file`):
			continue
		case !inHunk:
			continue
		case strings.HasPrefix(line, "+"):
			newLines = append(newLines, line[1:])
		case strings.HasPrefix(line, "-"):
			oldLines = append(oldLines, line[1:])
		case strings.HasPrefix(line, " "):
			oldLines = append(oldLines, line[1:])
			newLines = append(newLines, line[1:])
		case line == "":
			// Trailing empty element from trailing newline or blank context line.
			continue
		}
	}
	if !hasAnyHunk {
		// Not a unified diff; treat as raw replacement text.
		return "", patch
	}
	if len(oldLines) > 0 {
		oldText = strings.Join(oldLines, "\n") + "\n"
	}
	if len(newLines) > 0 {
		newText = strings.Join(newLines, "\n") + "\n"
	}
	return oldText, newText
}

// codexCommandOutput builds ToolOutput for a completed/in-progress Codex
// commandExecution item, populating Stdout + StdoutHead (and StderrHead on
// non-zero exitCode per internal/tui/transcript.go:979-984).
func codexCommandOutput(aggregated string, exitCode *int) *ToolOutput {
	text := sanitizeText(strings.ReplaceAll(aggregated, "\r\n", "\n"))
	if exitCode == nil && text == "" {
		return nil
	}
	if exitCode != nil && *exitCode != 0 {
		// Codex merges stdout/stderr into aggregatedOutput; on non-zero exit,
		// populate Stdout/StdoutHead AND StderrHead so TUI error previews show
		// the failing command's output cleanly.
		out := cappedToolOutput(text, "", "", exitCode)
		out.StderrHead = truncateUTF8(text, outputHeadCap)
		return out
	}
	return cappedToolOutput(text, "", "", exitCode)
}

func mapCodexItemStatus(status string) string {
	switch status {
	case "inProgress":
		return "in_progress"
	case "completed":
		return toolCompleted
	case "declined":
		return "cancelled"
	case "failed":
		return toolFailed
	default:
		if status == "" {
			return "in_progress"
		}
		return sanitizeText(status)
	}
}

func (s *codexSession) handleItemStarted(raw json.RawMessage) {
	var p codexrpc.ItemNotificationParams
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	s.processItem(p.Item, false, false)
}

func (s *codexSession) handleItemCompleted(raw json.RawMessage) {
	var p codexrpc.ItemNotificationParams
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	s.processItem(p.Item, true, false)
}

func (s *codexSession) handleAgentMessageDelta(raw json.RawMessage) {
	var p codexrpc.ItemDeltaParams
	if json.Unmarshal(raw, &p) != nil || p.Delta == "" {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	s.emitTextDelta(p.ItemID, p.Delta, false)
}

func (s *codexSession) handlePlanDelta(raw json.RawMessage) {
	var p codexrpc.ItemDeltaParams
	if json.Unmarshal(raw, &p) != nil || p.Delta == "" {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	s.emitTextDelta(p.ItemID, p.Delta, true)
}

func (s *codexSession) emitTextDelta(itemID, delta string, isPlan bool) {
	prefix := ""
	s.mu.Lock()
	if s.turnItemDeltas == nil {
		s.turnItemDeltas = make(map[string]bool)
	}
	if !isPlan && itemID != "" {
		if s.lastMsgItemID != "" && s.lastMsgItemID != itemID {
			prefix = "\n\n"
		}
		s.lastMsgItemID = itemID
	}
	if itemID != "" {
		s.turnItemDeltas[itemID] = true
	}
	s.mu.Unlock()
	s.emit(Event{Type: EventText, Text: sanitizeText(prefix + delta)})
}

func (s *codexSession) handleReasoningSummaryPartAdded(raw json.RawMessage) {
	var p codexrpc.ReasoningSummaryPartParams
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	if p.SummaryIndex > 0 {
		s.emit(Event{Type: EventThought, Text: "\n"})
	}
}

func (s *codexSession) handleReasoningDelta(raw json.RawMessage) {
	var p codexrpc.ItemDeltaParams
	if json.Unmarshal(raw, &p) != nil || p.Delta == "" {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	s.emit(Event{Type: EventThought, Text: sanitizeText(p.Delta)})
}

func (s *codexSession) handleCommandOutputDelta(raw json.RawMessage) {
	var p codexrpc.ItemDeltaParams
	if json.Unmarshal(raw, &p) != nil || p.Delta == "" || p.ItemID == "" {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	s.mu.Lock()
	if s.cmdBuffers == nil {
		s.cmdBuffers = make(map[string]string)
	}
	buf := s.cmdBuffers[p.ItemID] + p.Delta
	if len(buf) > codexCmdStreamCap {
		buf = buf[len(buf)-codexCmdStreamCap:]
	}
	s.cmdBuffers[p.ItemID] = buf
	row, exists := s.tools[p.ItemID]
	if !exists {
		row = ToolEvent{
			ID:       p.ItemID,
			Kind:     "execute",
			ToolName: "shell",
			Status:   "in_progress",
		}
		s.toolOrder = append(s.toolOrder, p.ItemID)
	}
	row.Output = codexCommandOutput(buf, nil)
	row.At = s.Now()
	s.tools[p.ItemID] = row
	ct := cloneTool(row)
	s.mu.Unlock()
	s.emit(Event{Type: EventTool, Tool: &ct})
}

func (s *codexSession) handlePlanUpdated(raw json.RawMessage) {
	var p codexrpc.TurnPlanUpdatedParams
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	todos := make([]Todo, 0, len(p.Plan))
	for i, step := range p.Plan {
		st := step.Status
		switch st {
		case "inProgress", "in_progress":
			st = "in_progress"
		case "completed", "done":
			st = "completed"
		default:
			st = "pending"
		}
		todos = append(todos, Todo{
			ID:      fmt.Sprintf("step-%d", i+1),
			Content: sanitizeText(step.Step),
			Status:  st,
		})
	}
	s.mu.Lock()
	s.snap.Todos = append([]Todo(nil), todos...)
	s.mu.Unlock()
	s.emit(Event{Type: EventTodos, Todos: todos})
}

func (s *codexSession) handleThreadNameUpdated(raw json.RawMessage) {
	var p codexrpc.ThreadNameUpdatedParams
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	title := sanitizeText(strings.TrimSpace(p.ThreadName))
	if title == "" {
		return
	}
	s.mu.Lock()
	if s.titlePinned || s.snap.Title == title {
		s.mu.Unlock()
		return
	}
	s.snap.Title = title
	t := title
	s.enqueueDeltaLocked("", Event{Text: title}, &StateDelta{Title: &t})
	s.mu.Unlock()
}

// processItem handles one Codex ThreadItem (started, completed, or replayed).
func (s *codexSession) processItem(raw json.RawMessage, completed, replaying bool) {
	var head codexrpc.ThreadItemHeader
	if json.Unmarshal(raw, &head) != nil || head.Type == "" {
		return
	}
	switch head.Type {
	case "userMessage":
		// Suppress live userMessage echoes (Plan 054 § D1); emit only during replay.
		if !replaying {
			return
		}
		var item codexrpc.UserMessageItem
		if json.Unmarshal(raw, &item) != nil {
			return
		}
		text := extractCodexUserText(item.Content)
		if text != "" {
			s.emit(Event{Type: EventUser, Text: sanitizeText(text)})
		}

	case "agentMessage":
		if !completed {
			return
		}
		var item codexrpc.AgentMessageItem
		if json.Unmarshal(raw, &item) != nil || item.Text == "" {
			return
		}
		s.mu.Lock()
		hadDelta := s.turnItemDeltas[item.ID]
		prefix := ""
		if !hadDelta {
			if s.lastMsgItemID != "" && s.lastMsgItemID != item.ID {
				prefix = "\n\n"
			}
			s.lastMsgItemID = item.ID
		}
		s.mu.Unlock()
		if !hadDelta {
			s.emit(Event{Type: EventText, Text: sanitizeText(prefix + item.Text)})
		}

	case "plan":
		if !completed {
			return
		}
		var item codexrpc.PlanItem
		if json.Unmarshal(raw, &item) != nil || item.Text == "" {
			return
		}
		s.mu.Lock()
		hadDelta := s.turnItemDeltas[item.ID]
		s.mu.Unlock()
		if !hadDelta {
			s.emit(Event{Type: EventText, Text: sanitizeText(item.Text)})
		}

	case "reasoning":
		if !replaying || !completed {
			return
		}
		var item codexrpc.ReasoningItem
		if json.Unmarshal(raw, &item) != nil {
			return
		}
		parts := item.Summary
		if len(parts) == 0 {
			parts = item.Content
		}
		if len(parts) > 0 {
			s.emit(Event{Type: EventThought, Text: sanitizeText(strings.Join(parts, "\n"))})
		}

	case "commandExecution":
		var item codexrpc.CommandExecutionItem
		if json.Unmarshal(raw, &item) != nil || item.ID == "" {
			return
		}
		st := mapCodexItemStatus(item.Status)
		if replaying && toolStatusInFlight(st) {
			st = "cancelled"
		}
		title := sanitizeText(item.Command)
		s.mu.Lock()
		if completed && s.cmdBuffers != nil {
			delete(s.cmdBuffers, item.ID)
		}
		row, exists := s.tools[item.ID]
		if !exists {
			s.toolOrder = append(s.toolOrder, item.ID)
		}
		row.ID = item.ID
		row.Kind = "execute"
		row.ToolName = "shell"
		if title != "" {
			row.Title = title
			row.Name = title
			row.RawInput = truncateUTF8(title, rawInputCap)
		}
		row.Status = st
		if completed || item.AggregatedOutput != "" || item.ExitCode != nil {
			row.Output = codexCommandOutput(item.AggregatedOutput, item.ExitCode)
		}
		row.At = s.Now()
		s.tools[item.ID] = row
		ct := cloneTool(row)
		s.mu.Unlock()
		s.emit(Event{Type: EventTool, Tool: &ct})

	case "fileChange":
		var item codexrpc.FileChangeItem
		if json.Unmarshal(raw, &item) != nil || item.ID == "" {
			return
		}
		st := mapCodexItemStatus(item.Status)
		if replaying && toolStatusInFlight(st) {
			st = "cancelled"
		}
		var (
			locs     []string
			allPaths []string
			diffs    []ToolDiff
		)
		for _, ch := range item.Changes {
			if ch.Path != "" {
				allPaths = appendDistinct(allPaths, ch.Path)
				if len(locs) < locationsCap {
					locs = appendDistinct(locs, sanitizeText(ch.Path))
				}
			}
			if ch.Kind.MovePath != "" {
				allPaths = appendDistinct(allPaths, ch.Kind.MovePath)
				if len(locs) < locationsCap {
					locs = appendDistinct(locs, sanitizeText(ch.Kind.MovePath))
				}
			}
			if len(diffs) < diffsCap {
				diffs = append(diffs, codexFileChangeToToolDiff(ch))
			}
		}
		title := "apply_patch"
		if len(locs) == 1 {
			title = "Edit " + locs[0]
		} else if len(locs) > 1 {
			title = fmt.Sprintf("Edit %d files", len(locs))
		}
		var edited *ToolEdited
		if !replaying && completed && st == toolCompleted && len(allPaths) > 0 {
			scope := editScope{workspace: s.workspace()}
			edited = scope.edited(allPaths)
		}
		s.mu.Lock()
		row, exists := s.tools[item.ID]
		if !exists {
			s.toolOrder = append(s.toolOrder, item.ID)
		}
		row.ID = item.ID
		row.Kind = "edit"
		row.ToolName = "apply_patch"
		row.Title = title
		row.Name = title
		row.Status = st
		if len(locs) > 0 {
			row.Locations = locs
		}
		if len(diffs) > 0 {
			row.Diffs = diffs
		}
		if st == toolCompleted {
			row.Edited = edited
		} else {
			row.Edited = nil
		}
		row.At = s.Now()
		s.tools[item.ID] = row
		ct := cloneTool(row)
		s.mu.Unlock()
		s.emit(Event{Type: EventTool, Tool: &ct})

	case "mcpToolCall":
		var item codexrpc.MCPToolCallItem
		if json.Unmarshal(raw, &item) != nil || item.ID == "" {
			return
		}
		st := mapCodexItemStatus(item.Status)
		if replaying && toolStatusInFlight(st) {
			st = "cancelled"
		}
		name := sanitizeText(item.Tool)
		if item.Server != "" {
			name = sanitizeText(item.Server + ":" + item.Tool)
		}
		s.mu.Lock()
		row, exists := s.tools[item.ID]
		if !exists {
			s.toolOrder = append(s.toolOrder, item.ID)
		}
		row.ID = item.ID
		row.Kind = "other"
		row.ToolName = name
		row.Title = name
		row.Name = name
		row.Status = st
		if len(item.Arguments) > 0 {
			row.RawInput = stringifyRawInput(item.Arguments)
		}
		if completed {
			if item.Error != nil && item.Error.Message != "" {
				row.Output = cappedToolOutput("", sanitizeText(item.Error.Message), "", nil)
			} else if len(item.Result) > 0 && string(item.Result) != "null" {
				row.Output = cappedToolOutput("", "", sanitizeText(string(item.Result)), nil)
			}
		}
		row.At = s.Now()
		s.tools[item.ID] = row
		ct := cloneTool(row)
		s.mu.Unlock()
		s.emit(Event{Type: EventTool, Tool: &ct})

	case "webSearch":
		var item codexrpc.WebSearchItem
		if json.Unmarshal(raw, &item) != nil || item.ID == "" {
			return
		}
		st := mapCodexItemStatus(item.Status)
		if completed && item.Status == "" {
			st = toolCompleted
		}
		if replaying && toolStatusInFlight(st) {
			st = "cancelled"
		}
		q := sanitizeText(item.Query)
		title := "Web search"
		if q != "" {
			title = "Web search: " + q
		}
		s.mu.Lock()
		row, exists := s.tools[item.ID]
		if !exists {
			s.toolOrder = append(s.toolOrder, item.ID)
		}
		row.ID = item.ID
		row.Kind = "search"
		row.ToolName = "web_search"
		row.Title = title
		row.Name = title
		row.RawInput = truncateUTF8(q, rawInputCap)
		row.Status = st
		row.At = s.Now()
		s.tools[item.ID] = row
		ct := cloneTool(row)
		s.mu.Unlock()
		s.emit(Event{Type: EventTool, Tool: &ct})

	case "contextCompaction":
		phase := CompactionStarted
		if completed {
			phase = CompactionEnded
		}
		s.emit(Event{
			Type:       EventCompaction,
			Compaction: &CompactionInfo{Phase: phase, Reason: CompactionAuto},
		})
	}
}

func extractCodexUserText(content []codexrpc.UserInput) string {
	var parts []string
	for _, u := range content {
		switch u.Type {
		case "text":
			if u.Text != "" {
				parts = append(parts, u.Text)
			}
		case "localImage":
			if u.Path != "" {
				parts = append(parts, fmt.Sprintf("[Image: %s]", u.Path))
			}
		case "skill":
			if u.Name != "" {
				parts = append(parts, "/"+u.Name)
			}
		}
	}
	return strings.Join(parts, "\n")
}
