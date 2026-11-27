package agent

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charliek/craze/internal/codexrpc"
)

// Item & diff translation for codexSession (Plan 054 § D2).

const (
	codexCmdStreamCap = 128 << 10 // cap accumulated live command output per call
	codexCmdHeadCap   = outputHeadCap
)

// codexCmdBuffer accumulates streaming command output without retaining
// oversized backing arrays, preserving the original head and truncation state.
type codexCmdBuffer struct {
	head      string
	tail      string
	truncated bool
}

func (b *codexCmdBuffer) append(delta string) {
	if delta == "" {
		return
	}
	if len(b.head) < codexCmdHeadCap {
		need := codexCmdHeadCap - len(b.head)
		if len(delta) <= need {
			b.head = strings.Clone(b.head + delta)
		} else {
			b.head = strings.Clone(b.head + truncateUTF8(delta, need))
		}
	}
	combined := b.tail + delta
	if len(combined) > codexCmdStreamCap {
		b.truncated = true
		cut := len(combined) - codexCmdStreamCap
		// Advance to a valid UTF-8 rune boundary.
		for cut < len(combined) && !utf8.RuneStart(combined[cut]) {
			cut++
		}
		b.tail = strings.Clone(combined[cut:])
		return
	}
	b.tail = strings.Clone(combined)
}

func (b *codexCmdBuffer) toolOutput(exitCode *int) *ToolOutput {
	out := codexCommandOutput(b.tail, exitCode)
	if out == nil {
		return nil
	}
	if b.truncated {
		out.Truncated = true
		head := sanitizeText(strings.ReplaceAll(b.head, "\r\n", "\n"))
		if head != "" {
			out.StdoutHead = truncateUTF8(head, outputHeadCap)
			if exitCode != nil && *exitCode != 0 {
				out.StderrHead = out.StdoutHead
			}
		}
	}
	return out
}

// codexFileChangeToToolDiff converts one Codex FileUpdateChange into Craze's
// ToolDiff{Path, OldText, NewText, Added, Removed, Truncated} contract so
// internal/tui/transcript.go renders real diff lines for add, delete, update,
// and move/rename operations.
func codexFileChangeToToolDiff(ch codexrpc.FileUpdateChange) ToolDiff {
	path := ch.Path
	if dst := ch.Kind.MoveTarget(); dst != "" {
		path = dst
	}
	switch ch.Kind.Type {
	case "add":
		return diffOf(path, "", ch.Diff)
	case "delete":
		return diffOf(path, ch.Diff, "")
	case "update":
		oldText, newText, added, removed, isUnified := unifiedDiffToOldNew(ch.Diff)
		if !isUnified {
			return diffOf(path, "", ch.Diff)
		}
		return buildCodexUnifiedDiff(path, oldText, newText, added, removed)
	default:
		oldText, newText, added, removed, isUnified := unifiedDiffToOldNew(ch.Diff)
		if !isUnified {
			return diffOf(path, "", ch.Diff)
		}
		return buildCodexUnifiedDiff(path, oldText, newText, added, removed)
	}
}

func buildCodexUnifiedDiff(path, oldText, newText string, added, removed int) ToolDiff {
	d := ToolDiff{
		Path:    sanitizeText(path),
		Added:   added,
		Removed: removed,
	}
	o := sanitizeText(oldText)
	n := sanitizeText(newText)
	if len(o) > diffTextCap {
		o = truncateUTF8(o, diffTextCap)
		d.Truncated = true
	}
	if len(n) > diffTextCap {
		n = truncateUTF8(n, diffTextCap)
		d.Truncated = true
	}
	d.OldText = o
	d.NewText = n
	return d
}

// parseUnifiedHunkHeader parses `@@ -a[,b] +c[,d] @@` returning the expected
// old and new line counts for the hunk.
func parseUnifiedHunkHeader(line string) (oldCount, newCount int, ok bool) {
	if !strings.HasPrefix(line, "@@ ") {
		return 0, 0, false
	}
	rest := line[3:]
	end := strings.Index(rest, " @@")
	if end < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(rest[:end])
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "-") || !strings.HasPrefix(fields[1], "+") {
		return 0, 0, false
	}
	parseRange := func(tok string) (int, bool) {
		if _, after, hasComma := strings.Cut(tok, ","); hasComma {
			n, err := strconv.Atoi(after)
			return n, err == nil && n >= 0
		}
		return 1, true
	}
	oc, ok1 := parseRange(fields[0][1:])
	nc, ok2 := parseRange(fields[1][1:])
	return oc, nc, ok1 && ok2
}

// unifiedDiffToOldNew reconstructs paired OldText and NewText strings from a
// unified diff patch (hunks starting with `@@ ... @@`), preserving original
// `+`/`-` line counts and `\ No newline at end of file` markers so newline-only
// edits and multi-hunk patches never lose edits or distort counts.
func unifiedDiffToOldNew(patch string) (oldText, newText string, added, removed int, isUnified bool) {
	if patch == "" {
		return "", "", 0, 0, false
	}
	rawLines := strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n")
	// Drop a single trailing empty element produced by a trailing newline.
	if len(rawLines) > 0 && rawLines[len(rawLines)-1] == "" {
		rawLines = rawLines[:len(rawLines)-1]
	}
	var (
		oldLines       []string
		newLines       []string
		inHunk         bool
		remOld, remNew int
		oldNoNL        bool
		newNoNL        bool
		lastSide       byte // '-', '+', ' '
	)
	for _, line := range rawLines {
		if oc, nc, ok := parseUnifiedHunkHeader(line); ok {
			inHunk = true
			isUnified = true
			remOld = oc
			remNew = nc
			lastSide = 0
			continue
		}
		if !inHunk {
			continue
		}
		if strings.HasPrefix(line, `\ No newline at end of file`) {
			switch lastSide {
			case '-':
				oldNoNL = true
			case '+':
				newNoNL = true
			case ' ':
				oldNoNL = true
				newNoNL = true
			}
			continue
		}
		if remOld == 0 && remNew == 0 {
			// Outside the declared hunk line counts: skip file headers between hunks.
			inHunk = false
			continue
		}
		if line == "" {
			// Some unified diff generators omit the leading space on blank context lines.
			if remOld > 0 && remNew > 0 {
				oldLines = append(oldLines, "")
				newLines = append(newLines, "")
				remOld--
				remNew--
				lastSide = ' '
			}
			continue
		}
		switch line[0] {
		case '+':
			if remNew > 0 {
				newLines = append(newLines, line[1:])
				remNew--
				added++
				lastSide = '+'
			}
		case '-':
			if remOld > 0 {
				oldLines = append(oldLines, line[1:])
				remOld--
				removed++
				lastSide = '-'
			}
		case ' ':
			if remOld > 0 || remNew > 0 {
				oldLines = append(oldLines, line[1:])
				newLines = append(newLines, line[1:])
				if remOld > 0 {
					remOld--
				}
				if remNew > 0 {
					remNew--
				}
				lastSide = ' '
			}
		}
	}
	if !isUnified {
		return "", patch, 0, 0, false
	}
	if len(oldLines) > 0 {
		oldText = strings.Join(oldLines, "\n")
		if !oldNoNL {
			oldText += "\n"
		}
	}
	if len(newLines) > 0 {
		newText = strings.Join(newLines, "\n")
		if !newNoNL {
			newText += "\n"
		}
	}
	return oldText, newText, added, removed, true
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

// acceptsTurnFrame reports whether an incoming item/delta/plan notification for
// turnID belongs to the currently active wire turn (or foreign turn), preventing
// late frames from a prior settled turn from contaminating a successor turn.
func (s *codexSession) acceptsTurnFrame(turnID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if turnID == "" {
		return (s.activeTurn != nil && (s.activeTurn.phase == codexPhaseSent || s.activeTurn.phase == codexPhaseIdentified) && !s.activeTurn.settling) ||
			s.foreignTurnID != ""
	}
	if s.activeTurn != nil && !s.activeTurn.settling {
		switch s.activeTurn.phase {
		case codexPhaseIdentified:
			if s.activeTurn.turnID == turnID {
				return true
			}
		case codexPhaseSent:
			// TurnStart sent; turn/started may not have arrived yet, provided
			// this turnID was not already settled earlier.
			if !s.settledTurns[turnID] {
				return true
			}
		}
	}
	if s.foreignTurnID != "" && s.foreignTurnID == turnID {
		return true
	}
	return false
}

func (s *codexSession) handleItemStarted(raw json.RawMessage) {
	var p codexrpc.ItemNotificationParams
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if !s.isMainThread(p.ThreadID) || !s.acceptsTurnFrame(p.TurnID) {
		return
	}
	s.processItem(p.Item, false, false)
}

func (s *codexSession) handleItemCompleted(raw json.RawMessage) {
	var p codexrpc.ItemNotificationParams
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if !s.isMainThread(p.ThreadID) || !s.acceptsTurnFrame(p.TurnID) {
		return
	}
	s.processItem(p.Item, true, false)
}

func (s *codexSession) handleAgentMessageDelta(raw json.RawMessage) {
	var p codexrpc.ItemDeltaParams
	if json.Unmarshal(raw, &p) != nil || p.Delta == "" {
		return
	}
	if !s.isMainThread(p.ThreadID) || !s.acceptsTurnFrame(p.TurnID) {
		return
	}
	s.emitTextDelta(p.ItemID, p.Delta, false)
}

func (s *codexSession) handlePlanDelta(raw json.RawMessage) {
	var p codexrpc.ItemDeltaParams
	if json.Unmarshal(raw, &p) != nil || p.Delta == "" {
		return
	}
	if !s.isMainThread(p.ThreadID) || !s.acceptsTurnFrame(p.TurnID) {
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
	if !s.isMainThread(p.ThreadID) || !s.acceptsTurnFrame(p.TurnID) {
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
	if !s.isMainThread(p.ThreadID) || !s.acceptsTurnFrame(p.TurnID) {
		return
	}
	s.emit(Event{Type: EventThought, Text: sanitizeText(p.Delta)})
}

func (s *codexSession) handleCommandOutputDelta(raw json.RawMessage) {
	var p codexrpc.ItemDeltaParams
	if json.Unmarshal(raw, &p) != nil || p.Delta == "" || p.ItemID == "" {
		return
	}
	if !s.isMainThread(p.ThreadID) || !s.acceptsTurnFrame(p.TurnID) {
		return
	}
	s.mu.Lock()
	if s.cmdBuffers == nil {
		s.cmdBuffers = make(map[string]*codexCmdBuffer)
	}
	buf := s.cmdBuffers[p.ItemID]
	if buf == nil {
		buf = &codexCmdBuffer{}
		s.cmdBuffers[p.ItemID] = buf
	}
	buf.append(p.Delta)
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
	row.Output = buf.toolOutput(nil)
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
	if !s.isMainThread(p.ThreadID) || !s.acceptsTurnFrame(p.TurnID) {
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
		var streamed *codexCmdBuffer
		if s.cmdBuffers != nil {
			streamed = s.cmdBuffers[item.ID]
			if completed {
				delete(s.cmdBuffers, item.ID)
			}
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
		if item.AggregatedOutput != "" {
			row.Output = codexCommandOutput(item.AggregatedOutput, item.ExitCode)
		} else if streamed != nil {
			row.Output = streamed.toolOutput(item.ExitCode)
		} else if completed || item.ExitCode != nil {
			row.Output = codexCommandOutput("", item.ExitCode)
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
			if dst := ch.Kind.MoveTarget(); dst != "" {
				allPaths = appendDistinct(allPaths, dst)
				if len(locs) < locationsCap {
					locs = appendDistinct(locs, sanitizeText(dst))
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
