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
// replay is over on the wire. What the adapter sends from here is live, but it
// is held (routeClaude) until the session has closed its own replay bracket
// (ReplayDone), so the one boundary between replayed and live is the
// session's: nothing live reaches it stamped replayed, and a foreign turn
// opened by it is one the session sees (review C4 item 5).
func (c *Client) claudeLoaded() {
	c.mu.Lock()
	if c.cf.phase == phaseReplaying {
		c.cf.loaded = true
	}
	c.mu.Unlock()
}

// ReplayDone is the session's word that its replay is over — its replay flag
// cleared and its end bracket out — on claude: the machine is idle and ready
// from here, and every update held since the load's reply is routed now, in
// order, under the rules, before anything the read loop reads next. Every
// other dialect, and a claude client that is not loading, does nothing.
func (c *Client) ReplayDone() {
	if c.dialect != DialectClaude {
		return
	}
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	c.mu.Lock()
	if c.cf.phase != phaseReplaying || !c.cf.loaded {
		c.mu.Unlock()
		return
	}
	held := c.cf.held
	c.cf.held = nil
	c.cf.loaded = false
	c.cf.phase = phaseIdle
	c.cf.ready = true
	c.mu.Unlock()
	for _, n := range held {
		c.routeClaude(n)
	}
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
	if c.cf.phase == phaseReplaying && c.cf.loaded {
		// Live, after the load's reply, and held until the session's replay
		// is over (ReplayDone).
		c.cf.held = append(c.cf.held, n)
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
		known := c.kids.has(child)
		registered, out := c.kids.childStarted(childPolicyClaude, n.SessionID, child)
		evicted = out
		switch {
		case !registered:
			// Refused at the cap with no idle child to make room: the row
			// still shows; the child's stream stays in the early buffer and
			// expires there.
			c.dropped++
		case sub.State == "":
			// A label or a cwd alone says nothing of the child's work
			// (review C4 item 6): a known child keeps its idle bit, and one
			// first named this way — a restored ancestor — is not working.
			if !known {
				c.kids.childIdle(child, true)
			}
		default:
			c.kids.childIdle(child, sub.State == ChildIdle)
		}
		if registered {
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

// ChildRouted reports whether id is a claude child this client routes — the
// one CancelSession can cancel.
func (c *Client) ChildRouted(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dialect == DialectClaude && id != "" && id != c.sessionID && c.kids.has(id)
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
