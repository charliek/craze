package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// The claude children (plan 052 § E3): one roster row per child, from
// subagent_update alone — the tool names (Agent, Task) are only the fallback
// for an adapter that announces none, and the launching call never makes a
// second row. The lifecycle, as the table pins it:
//
//	running, first time         a new row, labelled with the title and role
//	running, an idle child      a new attempt on the same row (handleSpawned's
//	                            attempt mechanism)
//	requires_action             the row stays running, activity "waiting for
//	                            you"; the child's ask is an open ask
//	idle + stopReason           end_turn completed, cancelled cancelled
//	                            (stopped by the user when craze asked), error
//	                            failed with the error's message
//	idle, no stop reason        completed; the child stays registered and may
//	                            report again
//	unknown                     that child's row ends cancelled, activity "no
//	                            longer observable"; other rows untouched
//
// session_message gives a child its opening user block, and its id is the
// launching call's. The state mapping is ported from t3code's
// (packages/provider-acp-registry/src/server/claudeAcp.ts,
// extractClaudeSubagentSessionEvent, at 26f70e72fa: requires_action is
// running; error and the unfinished stops — refusal, max_tokens,
// max_turn_requests, commit 0351dbb092 — fail; cancelled and unknown cancel;
// the rest completes), with craze's activity and reuse on top.

const (
	activityWaiting      = "waiting for you"
	activityUnobservable = "no longer observable"
	stoppedByUser        = "stopped by the user"
)

func claudeStatus(n acp.SubagentNotification, asked bool) (SubagentStatus, string) {
	switch n.StopReason {
	case acp.StopCancelled:
		if asked {
			return SubagentCancelled, stoppedByUser
		}
		return SubagentCancelled, ""
	case "error":
		msg := n.Error
		if msg == "" {
			msg = "the sub-agent failed"
		}
		return SubagentFailed, msg
	}
	if why := stoppedShortWhy(n.StopReason); why != "" {
		// A child that stopped short of its end fails as a root turn does
		// (§ B8).
		return SubagentFailed, why
	}
	return SubagentCompleted, ""
}

func subagentTerminal(st SubagentStatus) bool {
	return st != SubagentRunning && st != ""
}

// handleClaudeSubagentLocked is one claude subagent_update (s.mu held).
func (s *session) handleClaudeSubagentLocked(n acp.SubagentNotification) []Event {
	id := n.ChildSessionID
	if id == "" {
		return nil
	}
	rec := s.subagents[id]
	if n.State == acp.ChildUnknown {
		return s.claudeChildGoneLocked(rec)
	}
	spawned := false
	if rec == nil {
		rec = s.newSubagentLocked(acp.SubagentNotification{
			SubagentID: id, ChildSessionID: id, AttemptID: "1",
			ParentSessionID: n.ParentSessionID, Description: n.Title, SubagentType: n.SubagentType,
		})
		if rec == nil {
			return nil
		}
		rec.attempts = 1
		// No cancel unless the update grants it (applyClaudeLabelsLocked).
		rec.info.NoCancel = true
		if n.State == "" {
			// A label alone — a restored ancestor the adapter names without a
			// state — is a child that is not working.
			rec.info.Status = SubagentCompleted
		}
		spawned = true
	} else if working := n.State == acp.ChildRunning || n.State == acp.ChildRequiresAction; working && subagentTerminal(rec.info.Status) {
		// An idle child reused: a new attempt on the same row.
		rec.attempts++
		rec.stopAsked = false
		s.restartAttemptLocked(rec, strconv.Itoa(rec.attempts))
		rec.info.Activity = ""
		rec.info.StartedAt = time.Now()
		spawned = true
	}
	prev := cloneSubagent(rec.info)
	s.applyClaudeLabelsLocked(rec, n)
	capSubagent(&rec.info)
	switch n.State {
	case acp.ChildRequiresAction:
		if rec.info.Status == SubagentRunning {
			rec.info.Activity = activityWaiting
		}
	case acp.ChildRunning:
		if rec.info.Activity == activityWaiting {
			rec.info.Activity = ""
		}
	case acp.ChildIdle:
		if rec.info.Status == SubagentRunning {
			var evs []Event
			if spawned {
				evs = append(evs, subagentRowEvent(rec, SubagentChangeSpawned))
			}
			status, msg := claudeStatus(n, rec.stopAsked)
			return append(evs, s.finishClaudeChildLocked(rec, status, msg, "")...)
		}
		if status, msg := claudeStatus(n, rec.stopAsked); rec.info.Status == SubagentFailed && status == SubagentFailed && n.Error != "" {
			// The adapter reports a failure again once the SDK supplies the
			// error text its first report lacked (native-subagents.js
			// supplyError, review C4 item 8): the row takes it.
			rec.info.Error = truncateUTF8(sanitizeText(msg), subagentErrorCap)
		}
	}
	switch {
	case spawned:
		return []Event{subagentRowEvent(rec, SubagentChangeSpawned)}
	case !sameSubagentFull(prev, rec.info):
		return []Event{subagentRowEvent(rec, SubagentChangeProgress)}
	}
	return nil
}

// subagentRowEvent is rec's row as an event of change.
func subagentRowEvent(rec *subagentRec, change string) Event {
	info := cloneSubagent(rec.info)
	return Event{Type: EventSubagent, Subagent: &info, SubagentChange: change}
}

// applyClaudeLabelsLocked takes what an update says of the child's labels and
// its cancel capability.
func (s *session) applyClaudeLabelsLocked(rec *subagentRec, n acp.SubagentNotification) {
	if n.Title != "" {
		rec.info.Description = n.Title
	}
	if n.SubagentType != "" {
		rec.info.SubagentType = n.SubagentType
	}
	if n.Cancel != nil {
		rec.info.NoCancel = !*n.Cancel
	}
}

// finishClaudeChildLocked ends a running child's row: its open tool calls
// settled, its status, error and activity, and its place in finish order.
func (s *session) finishClaudeChildLocked(rec *subagentRec, status SubagentStatus, msg, activity string) []Event {
	rec.info.Status = status
	rec.info.Error = msg
	rec.info.Activity = activity
	if rec.info.EndedAt.IsZero() {
		rec.info.EndedAt = time.Now()
	}
	if !rec.info.StartedAt.IsZero() {
		rec.info.DurationMs = int(rec.info.EndedAt.Sub(rec.info.StartedAt) / time.Millisecond)
	}
	capSubagent(&rec.info)
	return s.closeAttemptLocked(rec)
}

// claudeChildGoneLocked is unknown — the adapter can no longer observe the
// child — or the registry dropping it (its parent's unknown or eviction): a
// running row ends cancelled, no longer observable. A row already ended keeps
// its outcome.
func (s *session) claudeChildGoneLocked(rec *subagentRec) []Event {
	if rec == nil || rec.info.Status != SubagentRunning {
		return nil
	}
	rec.info.NoCancel = true
	return s.finishClaudeChildLocked(rec, SubagentCancelled, "", activityUnobservable)
}

// claudeSessionMessage is a session_message: a prompt one session sent
// another (§ E3). The one a child receives is its opening user block — drawn
// once, whichever of its two copies (on the sender, on the recipient) comes
// first — and the launching call it replaces is the row's tool call. A reply
// (`<launch>:reply`) is the sender's report back to its parent — the
// parent's tool result — and never the parent's prompt (review C4 item 9): it
// is the sender's row's output.
func (s *session) claudeSessionMessage(raw json.RawMessage) {
	var w struct {
		MessageID string          `json:"messageId"`
		Sender    string          `json:"senderSessionId"`
		Recipient string          `json:"recipientSessionId"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &w) != nil || w.Recipient == "" {
		return
	}
	text := sessionMessageText(w.Content)
	if strings.HasSuffix(w.MessageID, replySuffix) {
		s.claudeChildReply(w.Sender, text)
		return
	}
	s.mu.Lock()
	rec := s.subagents[w.Recipient]
	if rec == nil || w.MessageID == "" || rec.promptIDs[w.MessageID] {
		s.mu.Unlock()
		return
	}
	if rec.promptIDs == nil {
		rec.promptIDs = make(map[string]bool)
	}
	rec.promptIDs[w.MessageID] = true
	prev := cloneSubagent(rec.info)
	rec.info.Prompt = truncateUTF8(text, taskPromptCap)
	rec.userChunks++
	if rec.info.ToolCallID == "" {
		rec.info.ToolCallID = w.MessageID
	}
	capSubagent(&rec.info)
	var evs []Event
	if !sameSubagentFull(prev, rec.info) {
		evs = append(evs, subagentRowEvent(rec, SubagentChangeProgress))
	}
	routed := !rec.unrouted
	s.mu.Unlock()
	s.emitAll(evs)
	if routed && text != "" {
		s.emit(Event{Type: EventUser, Agent: w.Recipient, Text: text})
	}
}

// replySuffix ends the messageId of a child's report back to its parent
// (native-subagents.js launchReply).
const replySuffix = ":reply"

// claudeChildReply is a child's report back to its parent: the child's row's
// output. A report from no row the session holds is the root's own tool
// result, and draws nothing.
func (s *session) claudeChildReply(sender, text string) {
	s.mu.Lock()
	rec := s.subagents[sender]
	if rec == nil || text == "" {
		s.mu.Unlock()
		return
	}
	prev := cloneSubagent(rec.info)
	rec.info.Output = text
	capSubagent(&rec.info)
	var evs []Event
	if !sameSubagentFull(prev, rec.info) {
		evs = append(evs, subagentRowEvent(rec, SubagentChangeProgress))
	}
	s.mu.Unlock()
	s.emitAll(evs)
}

// sessionMessageText is a session_message's content: its text blocks joined.
func sessionMessageText(raw json.RawMessage) string {
	var blocks []acp.ContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return messageText(raw)
	}
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type == "text" {
			b.WriteString(bl.Text)
		}
	}
	return sanitizeText(b.String())
}

// CancelSubagent is SubagentCanceller for claude (plan 052 § E3): the child's
// own session/cancel, written off this goroutine so the call waits on nothing.
// The child's idle cancelled report ends its row, stopped by the user. Only a
// running child whose capabilities grant cancel can be stopped; cursor and
// grok have no per-child stop (ErrUnsupported).
func (s *session) CancelSubagent(id string) error {
	if !s.provider().Capabilities().SubagentCancel {
		return ErrUnsupported
	}
	s.mu.Lock()
	rec := s.subagents[id]
	client := s.client
	if rec == nil || rec.info.Status != SubagentRunning || client == nil {
		s.mu.Unlock()
		return ErrNoSuchSubagent
	}
	if rec.info.NoCancel {
		s.mu.Unlock()
		return ErrUnsupported
	}
	if rec.stopAsked {
		s.mu.Unlock()
		return nil
	}
	if !client.ChildRouted(id) {
		// The registry no longer holds it (refused at the cap, forgotten):
		// there is no session to cancel, and the caller is told so rather
		// than left with a stop that never went (review C4 item 6).
		s.mu.Unlock()
		// Both: the session's cause, and the protocol's unknown_subagent.
		return fmt.Errorf("agent: sub-agent %s cannot be stopped: %w: %w", id, ErrNoSuchSubagent, acp.ErrNoSession)
	}
	rec.stopAsked = true
	s.mu.Unlock()
	go func() {
		if err := client.CancelSession(context.Background(), id); err != nil {
			// It never went: the next stop may try again.
			s.mu.Lock()
			rec.stopAsked = false
			s.mu.Unlock()
		}
	}()
	return nil
}

// finishedCapLocked is how many finished rows the session keeps. A claude
// child the ACP registry still holds may report again — late updates, a reuse
// — so claude keeps as many as the registry routes (review C4 item 7).
func (s *session) finishedCapLocked() int {
	if s.provider().isClaude() {
		return max(subagentFinishedCap, acp.ChildRouteCap)
	}
	return subagentFinishedCap
}

// retireChildrenAtEnd waits for the agent's transport to end and then ends
// every claude child row still running, failed: its agent is gone, so it will
// never report again, and a row left running would hold the session busy for
// ever (review C4 item 3). A session that closes first has nothing to retire.
func (s *session) retireChildrenAtEnd(client *acp.Client) {
	select {
	case <-client.Conn().Done():
	case <-s.done:
		return
	}
	s.mu.Lock()
	var evs []Event
	for _, id := range append([]string(nil), s.subagentOrder...) {
		if rec := s.subagents[id]; rec != nil && rec.info.Status == SubagentRunning {
			rec.info.NoCancel = true
			evs = append(evs, s.finishClaudeChildLocked(rec, SubagentFailed, agentStopped, "")...)
		}
	}
	s.mu.Unlock()
	s.emitAll(evs)
}

// agentStopped is the error of a claude child whose agent stopped under it.
const agentStopped = "the agent stopped"

// rootText is a claude root text chunk as the transcript is to draw it: a
// chunk of another message than the open run's starts a paragraph of its own
// (review C4 item 11), so a held turn's launch text and its hand-back do not
// run together. Every other provider's text goes as it came.
func (s *session) rootText(u sessionUpdateWire) string {
	text := messageText(u.Content)
	if !s.provider().isClaude() {
		return text
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if text == "" {
		return text
	}
	if s.textRun != "" && u.MessageID != "" && u.MessageID != s.textRun {
		text = "\n\n" + text
	}
	if u.MessageID != "" {
		s.textRun = u.MessageID
	}
	return text
}

// endTextRun is anything that ends the transcript's open text run.
func (s *session) endTextRun() {
	s.mu.Lock()
	s.textRun = ""
	s.mu.Unlock()
}

var _ SubagentCanceller = (*session)(nil)

// childLabel is how a card names the claude child that asked (§ E3): "sub-agent
// <title>", or "" for the root's own request.
func (s *session) childLabel(a acp.Arrival) string {
	if a.Child == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	name := a.Child
	if rec := s.subagents[a.Child]; rec != nil && rec.info.Description != "" {
		name = rec.info.Description
	}
	return "sub-agent " + sanitizeText(name)
}
