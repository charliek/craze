package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// longTurnStep is how long each of the two execute tools stays in_progress.
// A turn has to outlive several frames for anything to be queued during it.
const longTurnStep = 600 * time.Millisecond

// queueScript reports whether a script uses the per-prompt request records and
// the serialized turn runner below. The older scripts keep the single
// cancelled flag: their turns are never concurrent, so per-prompt state would
// buy them nothing and changing them would change wire orders the 007 goldens
// pinned.
func queueScript(script string) bool {
	switch script {
	case "long-turn", "grok-long-turn", "grok-long-turn-fallback":
		return true
	}
	return false
}

// promptReq is one session/prompt the fake is still holding. Each carries its
// own cancel channel and its own reply, so a second prompt cancelling the
// first — the live cursor wire — cannot cancel itself, and no request is ever
// answered twice.
type promptReq struct {
	id       json.RawMessage
	promptID string
	text     string
	cancel   chan struct{}
	replied  sync.Once
}

func (r *promptReq) cancelled() bool {
	select {
	case <-r.cancel:
		return true
	default:
		return false
	}
}

// waitStep sleeps out one tool step, returning false as soon as the request is
// cancelled.
func (r *promptReq) waitStep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.cancel:
		return false
	case <-t.C:
		return true
	}
}

// beginPrompt registers a prompt on the read loop, in arrival order, and hands
// back the record the turn runner will drive. Registering here rather than in
// the handler goroutine is what makes "the second prompt cancels the first"
// deterministic: the cancel is decided by the order the prompts arrived in.
func (s *server) beginPrompt(msg *acp.Message) *promptReq {
	text := promptText(msg.Params)
	s.mu.Lock()
	s.promptSeq++
	r := &promptReq{
		id:       msg.ID,
		promptID: fmt.Sprintf("prompt-%d", s.promptSeq),
		text:     text,
		cancel:   make(chan struct{}),
	}
	// Everything already accepted, running or not: on cursor all of it is
	// overtaken by this prompt, and a record the runner has not picked up yet
	// is just as overtaken as the one in flight.
	overtaken := append([]*promptReq(nil), s.queued...)
	if s.running != nil {
		overtaken = append(overtaken, s.running)
	}
	s.queued = append(s.queued, r)
	s.mu.Unlock()

	if s.script == "long-turn" {
		// Cursor has no queue: a second prompt cancels the turn in flight and
		// runs instead of it (cursor-second-prompt.jsonl 14.01). Nothing is
		// broadcast: cursor has no queue extension at all.
		for _, prev := range overtaken {
			s.cancelReq(prev)
		}
		return r
	}
	// Grok broadcasts the queue as soon as the prompt lands, before it
	// broadcasts what is running.
	s.broadcastQueue()
	return r
}

// cancelReq closes a request's cancel channel exactly once.
func (s *server) cancelReq(r *promptReq) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-r.cancel:
	default:
		close(r.cancel)
	}
}

// cancelAll cancels every prompt the fake is holding: session/cancel is scoped
// to the session, not to one prompt.
func (s *server) cancelAll() {
	s.mu.Lock()
	reqs := append([]*promptReq(nil), s.queued...)
	if s.running != nil {
		reqs = append(reqs, s.running)
	}
	s.mu.Unlock()
	for _, r := range reqs {
		s.cancelReq(r)
	}
}

// runQueue is the turn runner: one turn at a time, in arrival order, so a
// prompt that arrived on a busy session starts when the running one ends.
func (s *server) runQueue() {
	for {
		s.mu.Lock()
		if len(s.queued) == 0 || s.fallbackOn {
			// A fallback turn holds the session the way a prompt does; the
			// runner is kicked again when it ends.
			s.draining = false
			s.mu.Unlock()
			s.runFallbackIfStranded()
			return
		}
		r := s.queued[0]
		s.queued = s.queued[1:]
		s.running = r
		s.mu.Unlock()
		if r.cancelled() {
			// Cancelled before it ever started: still one reply.
			s.endTurn(r, acp.StopCancelled)
			continue
		}
		s.runTurn(r)
	}
}

// kickQueue starts the runner unless it is already going.
func (s *server) kickQueue() {
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		return
	}
	s.draining = true
	s.mu.Unlock()
	go s.runQueue()
}

// runTurn is the long turn every queue script shares: two execute tools of
// longTurnStep each, then a reply naming the steps and whatever was
// interjected between them.
func (s *server) runTurn(r *promptReq) {
	if grokScript(s.script) {
		s.broadcastQueue()
	}
	s.thought("Working through the steps.")
	for i, step := range []string{"step1", "step2"} {
		call := fmt.Sprintf("call-step-%d", i+1)
		s.executeTool(call, step)
		if !r.waitStep(longTurnStep) {
			s.update(fakeSessionID, map[string]any{
				"sessionUpdate": "tool_call_update",
				"toolCallId":    call,
				"status":        "failed",
			})
			s.endTurn(r, acp.StopCancelled)
			return
		}
		s.update(fakeSessionID, map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    call,
			"status":        "completed",
			"rawOutput":     map[string]any{"type": "Text", "text": step},
		})
		// Safe point: an interjection waiting here is merged into this turn,
		// exactly as grok drains at a tool result.
		s.mergeInterjections()
	}
	s.say("DONE " + strings.Join(s.takeMerged(), " "))
	s.strandSelfInterjection(r)
	s.endTurn(r, acp.StopEndTurn)
}

// strandInterjectionMarker in a prompt makes the fallback script strand an
// interjection of its own as the turn ends. craze never interjects headlessly,
// but grok can mint a fallback for an interjection craze did not send, and the
// drain has to wait that turn out either way.
const strandInterjectionMarker = "STRAND-INTERJECTION"

func (s *server) strandSelfInterjection(r *promptReq) {
	if s.script != "grok-long-turn-fallback" || !strings.Contains(r.text, strandInterjectionMarker) {
		return
	}
	_ = s.conn.Notify(context.Background(), acp.MethodGrokInterjectionWrapped, map[string]any{
		"sessionId": fakeSessionID,
		"text":      "someone else's note",
	})
	// The fallback is announced while this turn is still finishing and
	// outlives its ending, which is the ordering a client's drain has to
	// survive: the turn it was waiting on is over and the session is still
	// not its own. Live grok mints the fallback a moment later instead; the
	// client cannot tell the two apart, and this one is testable.
	s.startFallback("someone else's note", strandFallbackFor)
}

// strandFallbackFor is how long the stranded fallback holds the session — long
// enough to outlive the turn that stranded it.
const strandFallbackFor = 400 * time.Millisecond

// endTurn writes one turn's ending: the turn stops being the running one, the
// queue broadcast says so, and turn_completed, prompt_complete and the RPC
// reply follow in the live order. Whatever the turn merged goes with it.
func (s *server) endTurn(r *promptReq, stop string) {
	r.replied.Do(func() {
		s.mu.Lock()
		if s.running == r {
			s.running = nil
		}
		// The merge buffer belongs to the turn that is ending: a cancelled
		// turn must not hand its interjection to the next one.
		s.merged = nil
		s.mu.Unlock()
		if grokScript(s.script) {
			s.broadcastQueue()
			s.turnCompletedID(fakeSessionID, r.promptID, stop)
			_ = s.conn.Notify(context.Background(), acp.MethodGrokPromptComplete, map[string]any{
				"sessionId":  fakeSessionID,
				"promptId":   r.promptID,
				"stopReason": stop,
			})
		}
		s.reply(r.id, map[string]any{
			"stopReason": stop,
			"_meta":      map[string]any{"promptId": r.promptID},
		})
	})
}

func (s *server) executeTool(callID, step string) {
	s.toolMeta(fakeSessionID, callID, "Execute sleep && echo "+step, "run_terminal_command", map[string]any{
		"command": "sleep 1 && echo " + step,
	})
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    callID,
		"status":        "in_progress",
		"_meta":         map[string]any{"x.ai/tool": map[string]any{"name": "run_terminal_command", "kind": "execute"}},
	})
}

// broadcastQueue publishes x.ai/queue/changed in the live shape: the queue as
// it stands and whatever holds the session right now.
//
// The snapshot is taken and written inside one broadcast lock, so a broadcast
// can never publish state older than one already on the wire — a stale
// "running" line would leave a client waiting for a turn that had already
// ended.
func (s *server) broadcastQueue() {
	s.bmu.Lock()
	defer s.bmu.Unlock()
	s.mu.Lock()
	params := map[string]any{"sessionId": fakeSessionID, "entries": queueRows(s.queued)}
	switch {
	case s.running != nil:
		params["runningPromptId"] = s.running.promptID
		params["runningText"] = s.running.text
		params["runningKind"] = "prompt"
	case s.fallbackOn:
		params["runningPromptId"] = s.fallbackID
		params["runningText"] = s.fallbackText
		params["runningKind"] = "prompt"
	}
	s.mu.Unlock()
	_ = s.conn.Notify(context.Background(), acp.MethodGrokQueueChangedWrapped, params)
}

// broadcastQueueAs is broadcastQueue for a turn with no request record of its
// own: the interject fallback, which may hold the session while the turn that
// stranded it is still writing its ending.
func (s *server) broadcastQueueAs(runningID, runningText string) {
	s.bmu.Lock()
	defer s.bmu.Unlock()
	s.mu.Lock()
	params := map[string]any{"sessionId": fakeSessionID, "entries": queueRows(s.queued)}
	s.mu.Unlock()
	params["runningPromptId"] = runningID
	params["runningText"] = runningText
	params["runningKind"] = "prompt"
	_ = s.conn.Notify(context.Background(), acp.MethodGrokQueueChangedWrapped, params)
}

func queueRows(entries []*promptReq) []map[string]any {
	rows := make([]map[string]any, 0, len(entries))
	for i, e := range entries {
		rows = append(rows, map[string]any{
			"id":       e.promptID,
			"version":  0,
			"kind":     "prompt",
			"text":     e.text,
			"position": i,
		})
	}
	return rows
}

// turnCompletedID is turnCompleted for a script that mints real prompt ids.
func (s *server) turnCompletedID(sessionID, promptID, stop string) {
	s.subagentNotify(sessionID, map[string]any{
		"sessionUpdate": "turn_completed",
		"prompt_id":     promptID,
		"stop_reason":   stop,
	})
}

// handleInterject answers x.ai/interject the way live grok does: a nested
// {"result":{"status":"queued"}} ack, then the broadcast every client renders
// the user block from. The text is held until the next safe point.
func (s *server) handleInterject(msg *acp.Message) {
	var p acp.InterjectParams
	if err := json.Unmarshal(msg.Params, &p); err != nil || p.Text == "" {
		_ = s.conn.ReplyErr(msg.ID, &acp.RPCError{Code: acp.CodeInvalidParams, Message: "invalid interject params"})
		return
	}
	s.reply(msg.ID, map[string]any{"result": map[string]any{"status": acp.InterjectStatusQueued}})
	body := map[string]any{"sessionId": fakeSessionID, "text": p.Text}
	if p.InterjectionID != "" {
		body["interjectionId"] = p.InterjectionID
	}
	_ = s.conn.Notify(context.Background(), acp.MethodGrokInterjectionWrapped, body)
	s.mu.Lock()
	s.pendingInterjections = append(s.pendingInterjections, p.Text)
	s.mu.Unlock()
	// With no turn running the interjection is stranded the moment it lands.
	s.runFallbackIfStranded()
}

// mergeInterjections moves whatever is waiting into this turn. The fallback
// script never merges: its whole point is the interjection that misses every
// safe point and becomes a turn of grok's own.
func (s *server) mergeInterjections() {
	if s.script == "grok-long-turn-fallback" {
		return
	}
	s.mu.Lock()
	s.merged = append(s.merged, s.pendingInterjections...)
	s.pendingInterjections = nil
	s.mu.Unlock()
}

// takeMerged returns the step names plus anything merged into this turn, and
// clears the merge buffer for the next one.
func (s *server) takeMerged() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{"step1"}
	out = append(out, s.merged...)
	out = append(out, "step2")
	s.merged = nil
	return out
}

// runFallbackIfStranded is grok's interject fallback, captured live: an
// interjection with no turn to merge into becomes a prompt of grok's own,
// announced by queue/changed as runningPromptId "interject-fallback-N",
// streamed as ordinary session/updates, and ended by turn_completed alone —
// there is no prompt_complete for it and no queue/changed clearing it.
func (s *server) runFallbackIfStranded() {
	if !grokScript(s.script) {
		return
	}
	s.mu.Lock()
	if s.running != nil || s.draining || len(s.pendingInterjections) == 0 || s.fallbackOn {
		s.mu.Unlock()
		return
	}
	text := strings.Join(s.pendingInterjections, " ")
	s.pendingInterjections = nil
	s.mu.Unlock()
	s.startFallback(text, 50*time.Millisecond)
}

// startFallback mints the interject-fallback turn and runs it in the
// background. It does not check whether a turn is running: a fallback can be
// announced while the turn that stranded it is still finishing.
func (s *server) startFallback(text string, runFor time.Duration) {
	s.mu.Lock()
	if s.fallbackOn {
		s.mu.Unlock()
		return
	}
	s.fallbackSeq++
	id := fmt.Sprintf("interject-fallback-%d", s.fallbackSeq)
	s.fallbackID, s.fallbackText = id, text
	s.fallbackOn = true
	s.mu.Unlock()
	// The announcement goes out before this returns, so a turn ending right
	// behind it cannot be mistaken for the session going idle.
	s.broadcastQueueAs(id, text)
	go s.runFallback(id, text, runFor)
}

func (s *server) runFallback(id, text string, runFor time.Duration) {
	s.say("noted: " + text)
	time.Sleep(runFor)
	s.mu.Lock()
	s.fallbackOn = false
	s.fallbackID, s.fallbackText = "", ""
	s.mu.Unlock()
	// Live grok sends no queue/changed and no prompt_complete for a
	// fallback: turn_completed is the whole ending.
	s.turnCompletedID(fakeSessionID, id, acp.StopEndTurn)
	s.kickQueue()
}
