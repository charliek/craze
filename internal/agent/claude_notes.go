package agent

import (
	"encoding/json"
	"strings"
)

// claude's notices and compactions (plan 052 § B2). With the session
// capabilities `session:{notices:{}, compaction:{}}` declared, claude-agent-acp
// sends Claude Code's notices as `notice` updates (session-notices.js) and a
// compaction of the context as `compaction_update` (context-compaction.js)
// instead of transcript text and a tool call; the session draws each as a note.

// The update kinds read here.
const (
	updateNoticeKind     = "notice"
	updateCompactionKind = "compaction_update"
)

// noticeTextCap bounds a notice's title and description, each.
const noticeTextCap = 2048

// onClaudeNotice is a root `notice` {severity, title, description?}: an
// EventNotice, which the transcript draws as a note. A replayed one is drawn
// as any replayed event is.
func (s *session) onClaudeNotice(raw json.RawMessage) {
	if !s.provider().isClaude() {
		return
	}
	var w struct {
		Severity    string `json:"severity"`
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return
	}
	n := NoticeInfo{
		Severity:    sanitizeLine(strings.TrimSpace(w.Severity)),
		Title:       truncateUTF8(sanitizeText(strings.TrimSpace(w.Title)), noticeTextCap),
		Description: truncateUTF8(sanitizeText(strings.TrimSpace(w.Description)), noticeTextCap),
	}
	if n.Title == "" && n.Description == "" {
		return
	}
	s.endTextRun()
	s.emit(Event{Type: EventNotice, Notice: &n})
}

// onClaudeCompaction is a root `compaction_update` {compactionId, status,
// error?}: in_progress is a compaction's started, and completed, failed or
// cancelled its ended — the error, or "cancelled", as why no summary was
// written. claude says nothing of the sizes, so the note has none. A second
// terminal update for the same compaction (an enrichment) draws nothing.
func (s *session) onClaudeCompaction(raw json.RawMessage) {
	if !s.provider().isClaude() {
		return
	}
	var w struct {
		ID     string `json:"compactionId"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return
	}
	info := CompactionInfo{Reason: CompactionAuto}
	s.mu.Lock()
	switch w.Status {
	case "in_progress":
		if s.compacting != "" && s.compacting == w.ID {
			s.mu.Unlock()
			return
		}
		s.compacting = w.ID
		if s.compacting == "" {
			s.compacting = "-"
		}
		info.Phase = CompactionStarted
	case "completed", "failed", "cancelled":
		if w.ID != "" && s.compactedLast == w.ID {
			s.mu.Unlock()
			return
		}
		s.compacting = ""
		s.compactedLast = w.ID
		info.Phase = CompactionEnded
		switch w.Status {
		case "failed":
			info.Err = sanitizeLine(strings.TrimSpace(w.Error))
			if info.Err == "" {
				info.Err = "failed"
			}
		case "cancelled":
			info.Err = "cancelled"
		}
	default:
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.endTextRun()
	s.emit(Event{Type: EventCompaction, Compaction: &info})
}
