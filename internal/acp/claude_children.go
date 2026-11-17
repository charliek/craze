package acp

import (
	"context"
	"encoding/json"
)

// The claude dialect's routing of session/update (plan 052 § E1, § E3): the
// foreign-turn state machine reads every root update, and the child registry
// (children.go) every subagent_update, before the routing filter.

// resetForeignLocked puts the state machine back to phase for a new or loaded
// session: no turn open, nothing ready, every timer retired.
func (c *Client) resetForeignLocked(phase foreignPhase) {
	f := &c.cf
	c.stopGraceLocked()
	c.stopSilenceLocked()
	failErr := f.failErr
	seq := f.seq
	graceGen, silenceGen := f.graceGen, f.silenceGen
	*f = claudeForeign{phase: phase, seq: seq, failErr: failErr, graceGen: graceGen, silenceGen: silenceGen}
}

// claudeSetup is a claude session/new's reply hook, on the read loop: the
// session is the one the reply names from here, everything the adapter sent
// before the reply is routed now — in order, opening nothing (§ E1: the rules
// apply once session/new has returned) — and only then is the machine ready,
// so the next update read is judged by the rules.
func (c *Client) claudeSetup(sid string) {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	c.mu.Lock()
	c.sessionID = sid
	pending := c.pendingUpdates
	c.pendingUpdates = nil
	// A new session inherits none of the last one's: its children, its tool
	// calls, its foreign turn.
	c.resetSessionLocked()
	c.resetForeignLocked(phaseIdle)
	c.mu.Unlock()
	for _, n := range pending {
		c.routeClaude(n)
	}
	c.mu.Lock()
	if c.cf.phase == phaseIdle {
		c.cf.ready = true
	}
	c.mu.Unlock()
}

// claudeLoaded is a claude session/load's reply hook, on the read loop: the
// replay is over, and from the next update the rules apply.
func (c *Client) claudeLoaded() {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	c.mu.Lock()
	if c.cf.phase == phaseReplaying {
		c.cf.phase = phaseIdle
		c.cf.ready = true
	}
	c.mu.Unlock()
}

// requestSession is a request's params.sessionId, "" for none.
func requestSession(params json.RawMessage) string {
	var w struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(params, &w) != nil {
		return ""
	}
	return w.SessionID
}

// routeClaude routes one claude session/update. It runs on the read loop under
// dispatchMu (onNotify), and in claudeSetup for what was buffered before
// session/new returned; a replay of the early buffer re-enters it.
//
//   - Before session/new has returned everything is buffered (pendingUpdates),
//     to be routed by claudeSetup.
//   - An update on an id that is neither the root nor a registered child is
//     held in the early buffer: it may be a child whose announce is still to
//     come (R2-4). Past its bounds it is dropped and counted.
//   - A subagent_update — on the root or a registered child, the child's
//     immediate parent — registers, idles or forgets the child it names, BEFORE
//     anything of that child is routed, and replays what the early buffer held
//     for it.
//   - A root update is the foreign-turn machine's to read first; a turn it
//     opens is announced ahead of the update.
func (c *Client) routeClaude(n SessionNotification) {
	kind := sessionUpdateKind(n.Update)
	c.mu.Lock()
	if c.sessionID == "" {
		c.pendingUpdates = append(c.pendingUpdates, n)
		c.mu.Unlock()
		return
	}
	now := c.clockLocked().Now()
	c.dropped += c.kids.expireEarly(now)
	active := c.sessionID
	if n.SessionID != active && !c.kids.has(n.SessionID) {
		c.dropped += c.kids.bufferEarly(n, now)
		c.mu.Unlock()
		return
	}
	if kind == updateSubagent {
		c.claudeSubagentLocked(n, active)
		return
	}
	root := n.SessionID == active
	var start *ForeignTurn
	if root {
		start = c.foreignRootLocked(kind, n.Update)
	}
	h := c.onUpdate
	fh := c.foreignHandler
	c.mu.Unlock()
	// Only now, past the session filter: a dropped session's tool call must
	// never name one of the active session's (§ B5).
	c.noteClaudeTool(n.SessionID, n.Update)
	fireForeign(fh, start, nil)
	if !root {
		n.Child = n.SessionID
	}
	if h != nil {
		h(n)
	}
}

// sessionUpdateKind is an update's sessionUpdate, "" when there is none.
func sessionUpdateKind(update json.RawMessage) string {
	var w struct {
		SessionUpdate string `json:"sessionUpdate"`
	}
	if json.Unmarshal(update, &w) != nil {
		return ""
	}
	return w.SessionUpdate
}

// claudeSubagentLocked is one subagent_update on a routed session (c.mu held;
// released here). The child it names is registered — with its immediate
// parent, the session the update came on — or marked idle or working, before
// the handler runs and before anything held for it is replayed; unknown
// forgets it and its descendants once the handler has run (the lifecycle
// table, § E3). A child the cap evicted, and every descendant of a child
// forgotten, is told to the handler as unknown and Evicted, so its row ends.
func (c *Client) claudeSubagentLocked(n SessionNotification, root string) {
	sub, ok := parseClaudeSubagent(n.SessionID, root, n.Update)
	if !ok {
		c.dropped++
		c.mu.Unlock()
		return
	}
	child := sub.ChildSessionID
	var evicted []string
	var replay []SessionNotification
	if sub.State != ChildUnknown {
		registered, out := c.kids.childStarted(childPolicyClaude, n.SessionID, child)
		evicted = out
		if !registered {
			// Refused at the cap with no idle child to make room: the row
			// still shows; the child's stream stays in the early buffer and
			// expires there.
			c.dropped++
		} else {
			c.kids.childIdle(child, sub.State == ChildIdle)
			replay = c.kids.takeEarly(child)
		}
	}
	h := c.subagentHandler
	c.mu.Unlock()
	// tellGone tells the handler of children the registry dropped.
	tellGone := func(ids []string) {
		if h == nil {
			return
		}
		for _, id := range ids {
			h(SubagentNotification{SessionID: root, Kind: SubagentUpdate, SubagentID: id, ChildSessionID: id, State: ChildUnknown, Evicted: true})
		}
	}
	tellGone(evicted)
	if h != nil {
		h(sub)
	}
	if sub.State == ChildUnknown {
		c.mu.Lock()
		gone := c.kids.childGone(child)
		c.mu.Unlock()
		if len(gone) > 1 {
			tellGone(gone[1:])
		}
	}
	for _, r := range replay {
		c.routeClaude(r)
	}
}

// CancelSession is session/cancel for one claude child session (plan 052 §
// E3): the adapter stops that child's current work and nothing else
// (v1/acp-agent.js cancelChildSession). Only a registered child may be
// cancelled this way; the root's cancel is Cancel.
func (c *Client) CancelSession(ctx context.Context, id string) error {
	c.mu.Lock()
	ok := c.dialect == DialectClaude && id != "" && id != c.sessionID && c.kids.has(id)
	c.mu.Unlock()
	if !ok {
		return ErrNoSession
	}
	return c.conn.Notify(ctx, MethodSessionCancel, CancelParams{SessionID: id})
}
