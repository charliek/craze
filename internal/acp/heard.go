package acp

import (
	"context"
	"encoding/json"
	"errors"
)

// The image resend's client support (plan 033 §3.4, C3r): what counts as the
// agent having done something with a prompt (heard), and the resend's own way
// onto the wire (ResendBlocks), which re-checks that in the same locked section
// that opens its turn.

// ErrHeardSinceRefusal is ResendBlocks' refusal: the agent sent something of
// its own about the refused prompt — before refusing it or since — so the
// prompt may have run and is not sent again. Nothing reached the wire.
var ErrHeardSinceRefusal = errors.New("acp: the agent was heard from since it refused the prompt")

// Grok's session-level ext notifications (plan 033 V1, grok 1.0.44): the
// model list and the session list changed. Neither is the agent doing anything
// with a prompt; craze reads neither.
const (
	MethodGrokModelsUpdate           = "x.ai/models/update"
	MethodGrokModelsUpdateWrapped    = "_x.ai/models/update"
	MethodGrokSessionsChanged        = "x.ai/sessions/changed"
	MethodGrokSessionsChangedWrapped = "_x.ai/sessions/changed"
)

// Heard reports whether the agent sent anything of its own about the last
// prompt since its turn opened (heard): a turn update, a tool call, a
// permission request, a plan, grok naming the prompt in its queue — anything
// turnActivity counts. It goes on counting after PromptBlocks has returned,
// until the next PromptBlocks resets it: an update the agent sent after its
// refusal says the refused prompt ran as surely as one sent before it. It is
// what decides whether a prompt the agent refused may be sent again: only one
// the agent did nothing with (plan 033 §3.4).
func (c *Client) Heard() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.heard
}

// noteHeard marks the last prompt as one the agent has sent something about.
// Before the first prompt it marks nothing anyone reads: every PromptBlocks
// resets it as it opens its turn.
func (c *Client) noteHeard() {
	c.mu.Lock()
	c.heard = true
	c.mu.Unlock()
}

// noteActivity is the read loop's heard check for one notification: once the
// last prompt is heard there is nothing left to learn, so the kind is only
// peeked while it is not.
func (c *Client) noteActivity(msg *Message) {
	c.mu.Lock()
	heard := c.heard
	c.mu.Unlock()
	if !heard && turnActivity(msg) {
		c.noteHeard()
	}
}

// turnActivity reports whether a notification is the agent doing something
// with a prompt. Only turn content counts: agent_message_chunk,
// agent_thought_chunk, tool_call, tool_call_update and plan updates, cursor's
// todos and tasks, grok's sub-agent and turn events, the interjection
// broadcast — and every request the agent sends (onRequest, not here). What is
// about the session and not the prompt does not count: the catalog, the mode,
// the config options, the title, usage, grok's model and session lists and its
// summaries, and the echo of the user's own prompt (sessionLevelUpdate,
// grokSessionLevel). An agent sends those whenever it likes — cursor's catalog
// lands seconds after session/new, inside the first prompt as often as not —
// and counting them refused resends no agent had begun on (the C3 gate flake).
//
// grok's prompt_complete and queue/changed are the turn's bookkeeping and are
// judged where they are handled: a queue/changed that names the prompt in
// flight (learnPromptIDLocked) and a completion craze cannot place
// (handlePromptComplete) both count. Anything not known is counted: a refusal
// that is not resent is the safe side of not knowing.
func turnActivity(msg *Message) bool {
	switch msg.Method {
	case MethodGrokPromptComplete, MethodGrokPromptCompleteWrapped,
		MethodGrokQueueChanged, MethodGrokQueueChangedWrapped,
		MethodGrokModelsUpdate, MethodGrokModelsUpdateWrapped,
		MethodGrokSessionsChanged, MethodGrokSessionsChangedWrapped:
		return false
	case MethodSessionUpdate:
		return !sessionLevelUpdate(updateKind(msg.Params))
	case MethodGrokSessionNotification, MethodGrokSessionNotificationWrapped:
		return !grokSessionLevel(updateKind(unwrapExtParams(msg.Params)))
	}
	return true
}

// updateKind is a notification's update.sessionUpdate, the field session/update
// and grok's session_notification both name their kind by; "" when there is
// none to read.
func updateKind(params json.RawMessage) string {
	var w struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
		} `json:"update"`
	}
	if decodeObject(params, &w) != nil {
		return ""
	}
	return w.Update.SessionUpdate
}

// sessionLevelUpdate is the session/update kinds that are about the session,
// not a prompt. user_message_chunk is the agent's echo of the prompt it was
// sent (grok and gx echo live; craze never draws it outside a replay, live.go's
// onUpdate) — the user's own words, not anything the agent did with them, and
// real grok 1.0.44 sends none before it answers (plan 033 V1). An empty kind
// is not known, so it is not session-level.
func sessionLevelUpdate(kind string) bool {
	switch kind {
	case UpdateAvailableCommands, UpdateCurrentMode, UpdateConfigOption, UpdateSessionInfo,
		updateUsage, updateUserMessageChunk:
		return true
	}
	return false
}

// grokSessionLevel is the x.ai/session_notification kinds that are about the
// session, not a prompt: the summaries grok writes after a turn has ended (plan
// 033 V1, grok 1.0.44), which can land inside the next one. The rest of what
// it was seen to send in a turn — image_compressed, response_completed,
// hook_run_started, hook_execution, turn_completed, the sub-agent lifecycle —
// is the prompt being worked on, and anything else is not known.
func grokSessionLevel(kind string) bool {
	switch kind {
	case grokLastTurnSummary, grokSessionSummaryGenerated:
		return true
	}
	return false
}

// The update kinds above that craze reads nowhere else.
const (
	updateUsage                 = "usage_update"
	updateUserMessageChunk      = "user_message_chunk"
	grokLastTurnSummary         = "last_turn_summary"
	grokSessionSummaryGenerated = "session_summary_generated"
)

// ResendBlocks is PromptBlocks for the image resend (plan 033 §3.4): the
// prompt the agent just refused, sent once more, refused before the wire with
// ErrHeardSinceRefusal if the agent has been heard from since that prompt's
// turn opened. The check is made in the same locked section that opens the
// resend's turn, so nothing the read loop counts can fall between the decision
// and the bytes: what it reads before the turn opens refuses the resend, and
// what it reads after belongs to the resend.
//
// On grok the resend's turn is settled by a prompt_complete only once
// queue/changed has named the resend's own promptId: block 1 is the refused
// prompt's, so a late completion of that prompt — its id never learned —
// could otherwise end the resend while it runs. The RPC reply always ends it.
func (c *Client) ResendBlocks(ctx context.Context, blocks []ContentBlock, accepted, sent func()) (*PromptResult, error) {
	return c.promptBlocks(ctx, blocks, accepted, sent, true)
}
