package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
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
	if f.phase == phaseClosed {
		// Closed stays closed: an overage trip read before session/new
		// returned (review C5 item 9), or the client's close.
		phase = phaseClosed
	}
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
		c.cf.holdCtx, c.cf.holdCancel = context.WithCancelCause(context.Background())
		// The hold has a time bound (review C4b item 6): a session that never
		// gets to ReplayDone does not hold the agent's traffic for ever.
		gen := c.cf.holdGen
		c.cf.holdTimer = c.clockLocked().AfterFunc(replayHoldWait, func() {
			c.failHold(gen, fmt.Errorf("%w: the session did not take the agent's traffic within %v of the load", ErrReplayHold, replayHoldWait))
		})
	}
	c.mu.Unlock()
}

// ErrReplayHold is a load whose post-reply hold failed (review C4b item 6):
// past its bounds, past its time, or its load's context ended first. The
// client closes itself, and ReplayDone answers with it.
var ErrReplayHold = errors.New("acp: the load's live traffic could not be held")

// The post-load hold's bounds (review C4b item 6), vars for tests.
var (
	holdFrameCap   = 4096
	holdByteCap    = 16 << 20
	replayHoldWait = 30 * time.Second
)

// holdClaude holds f if the client is holding, closing the client when the
// hold fails; it answers whether f was taken.
func (c *Client) holdClaude(f heldFrame, size int) bool {
	c.mu.Lock()
	taken, failed := c.holdLocked(f, size)
	c.mu.Unlock()
	if failed {
		go func() { _ = c.Close() }()
	}
	return taken
}

// heldFrame is one frame held between a load's reply and ReplayDone, in
// arrival order: an update, a request, or a request's withdrawal.
type heldFrame struct {
	update *SessionNotification
	req    *Message
	cancel *Message
}

// holdingLocked reports whether the client holds what the agent sends: a
// load's reply has been read and the session's replay is not over.
func (c *Client) holdingLocked() bool {
	return c.cf.phase == phaseReplaying && c.cf.loaded
}

// holdLocked holds one frame of size bytes, or, past a bound, fails the hold.
// It answers whether the frame was taken (held or dropped): false only when
// the client is not holding.
func (c *Client) holdLocked(f heldFrame, size int) (taken, failed bool) {
	if !c.holdingLocked() {
		return false, false
	}
	if c.cf.holdErr != nil {
		c.dropped++
		return true, false
	}
	if len(c.cf.held) >= holdFrameCap || c.cf.heldBytes+size > holdByteCap {
		c.failHoldLocked(fmt.Errorf("%w: more than %d frames or %d bytes", ErrReplayHold, holdFrameCap, holdByteCap))
		return true, true
	}
	c.cf.held = append(c.cf.held, f)
	c.cf.heldBytes += size
	return true, false
}

// failHoldLocked fails the hold: what was held is dropped and counted, nothing
// more is taken, and the caller closes the client (off the read loop).
func (c *Client) failHoldLocked(err error) {
	if c.cf.holdErr != nil {
		return
	}
	c.cf.holdErr = err
	if c.cf.holdCancel != nil {
		c.cf.holdCancel(err)
	}
	c.dropped += int64(len(c.cf.held))
	c.cf.held, c.cf.heldBytes = nil, 0
	if c.cf.holdTimer != nil {
		c.cf.holdTimer.Stop()
		c.cf.holdTimer = nil
	}
	c.cf.holdGen++
}

// failHold is failHoldLocked for a hold still at gen (its time bound, or its
// load's context ending), and the client's close.
func (c *Client) failHold(gen uint64, err error) {
	c.mu.Lock()
	failed := (c.holdingLocked() || c.cf.draining) && c.cf.holdGen == gen && c.cf.holdErr == nil
	if failed {
		c.failHoldLocked(err)
	}
	c.mu.Unlock()
	if failed {
		go func() { _ = c.Close() }()
	}
}

// watchHold fails the hold if the load's own context ends while it lasts.
func (c *Client) watchHold(ctx context.Context) {
	c.mu.Lock()
	gen := c.cf.holdGen
	holding := c.holdingLocked()
	hctx := c.cf.holdCtx
	c.mu.Unlock()
	if !holding || hctx == nil {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			c.failHold(gen, fmt.Errorf("%w: %w", ErrReplayHold, ctx.Err()))
		case <-hctx.Done():
		}
	}()
}

// HoldSettled is closed once a claude load's post-reply hold is settled —
// drained by ReplayDone, failed by its bounds, or ended by the client's close
// — and at once when there is none (review C5c item 6).
func (c *Client) HoldSettled() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cf.holdCtx == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return c.cf.holdCtx.Done()
}

// LoadContext is the session's context for its own work between a claude
// load's reply and ReplayDone (review C4c item 1): it ends when the hold fails
// — its bounds, its time, its load's context — or the client closes, with that
// as its cause, so a stalled replay completion gives up and the load fails
// rather than waiting for ever. context.Background for every other client.
func (c *Client) LoadContext() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cf.holdCtx == nil {
		return context.Background()
	}
	return c.cf.holdCtx
}

// endHoldContextLocked ends the hold's context with cause, if there is one.
func (c *Client) endHoldContextLocked(cause error) {
	if c.cf.holdCancel != nil {
		c.cf.holdCancel(cause)
	}
}

// lockDispatchUntil takes dispatchMu, or gives up when ctx ends (nil: never),
// answering whether it holds it. A lock taken after the give-up is released at
// once by the goroutine that took it.
func (c *Client) lockDispatchUntil(ctx context.Context) bool {
	if c.dispatchMu.TryLock() {
		return true
	}
	if ctx == nil {
		c.dispatchMu.Lock()
		return true
	}
	got := make(chan struct{})
	gaveUp := make(chan struct{})
	go func() {
		c.dispatchMu.Lock()
		select {
		case got <- struct{}{}:
		case <-gaveUp:
			c.dispatchMu.Unlock()
		}
	}()
	select {
	case <-got:
		return true
	case <-ctx.Done():
		close(gaveUp)
		return false
	}
}

// JoinDispatch waits until no claude dispatch is running — the read loop's
// or ReplayDone's drain — or stop closes, answering whether it got there:
// whatever was dispatched before it has been handed on and published (review
// C4c item 4).
func (c *Client) JoinDispatch(stop <-chan struct{}) bool {
	got := make(chan struct{})
	gaveUp := make(chan struct{})
	go func() {
		c.dispatchMu.Lock()
		select {
		case got <- struct{}{}:
		case <-gaveUp:
		}
		c.dispatchMu.Unlock()
	}()
	select {
	case <-got:
		return true
	case <-stop:
		close(gaveUp)
		return false
	}
}

// ReplayDone is the session's word that its replay is over — its replay flag
// cleared and its end bracket out — on claude: the machine is idle and ready
// from here, and every update held since the load's reply is routed now, in
// order, under the rules, before anything the read loop reads next. Every
// other dialect, and a claude client that is not loading, does nothing.
//
// It answers the hold's failure, when it failed (ErrReplayHold): the client
// has closed itself, and the load is to fail.
func (c *Client) ReplayDone() error {
	if c.dialect != DialectClaude {
		return nil
	}
	// The dispatch is taken as the load's context allows (review C4d finding
	// 1): a read-loop handler that holds it can stall, and a hold that fails
	// meanwhile must still end this call.
	c.mu.Lock()
	hold := c.cf.holdCtx
	c.mu.Unlock()
	if !c.lockDispatchUntil(hold) {
		c.mu.Lock()
		err := c.cf.holdErr
		c.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("%w: %w", ErrReplayHold, context.Cause(hold))
		}
		return err
	}
	c.mu.Lock()
	if err := c.cf.holdErr; err != nil {
		c.mu.Unlock()
		c.dispatchMu.Unlock()
		return err
	}
	if !c.holdingLocked() {
		// Nothing held (a trip closed the machine during the hold, or no
		// load): the hold, if any, is settled.
		c.endHoldContextLocked(nil)
		c.mu.Unlock()
		c.dispatchMu.Unlock()
		return nil
	}
	held := c.cf.held
	c.cf.held, c.cf.heldBytes = nil, 0
	c.cf.loaded = false
	c.cf.phase = phaseIdle
	c.cf.ready = true
	// The time bound and the context watch stay armed through the drain
	// (review C4c item 1): a handler that blocks in it is bounded too.
	c.cf.draining = true
	hctx := c.cf.holdCtx
	c.mu.Unlock()
	// The drain runs on its own goroutine, holding dispatchMu until it is
	// done, so the read loop waits behind it; this call waits for it or for
	// the hold to fail, whichever is first.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer c.dispatchMu.Unlock()
		// In arrival order, requests and withdrawals with the updates
		// (review C4b item 5): a child's request comes after the announce
		// that registers it, as it came on the wire.
		for _, f := range held {
			if testDrainFrame != nil {
				testDrainFrame()
			}
			c.mu.Lock()
			failed := c.cf.holdErr != nil
			c.mu.Unlock()
			if failed || c.conn.severed.Load() {
				// The hold failed (its bounds, its time, its context), or the
				// overage guard's stop (review C5 item 7): nothing more
				// reaches the session. The transport ending is not one of
				// these (review C4d finding 2): every frame held was read
				// before it, and is delivered — the session retires its
				// children only once this drain is over (JoinDispatch).
				break
			}
			switch {
			case f.update != nil:
				c.routeClaude(*f.update)
			case f.req != nil:
				c.dispatchRequest(f.req)
			case f.cancel != nil:
				c.handleCancelRequest(f.cancel)
			}
		}
		c.mu.Lock()
		c.cf.draining = false
		if c.cf.holdErr == nil {
			if c.cf.holdTimer != nil {
				c.cf.holdTimer.Stop()
				c.cf.holdTimer = nil
			}
			c.cf.holdGen++
		}
		c.mu.Unlock()
	}()
	var done <-chan struct{}
	if hctx != nil {
		done = hctx.Done()
	}
	select {
	case <-drained:
	case <-done:
	}
	c.mu.Lock()
	err := c.cf.holdErr
	if err == nil {
		// Done with: the session's waits are over.
		c.endHoldContextLocked(nil)
	}
	c.mu.Unlock()
	return err
}

// testDrainFrame is a test seam, nil in production: it runs in ReplayDone's
// drain before each held frame is routed.
var testDrainFrame func()

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
	if c.conn.severed.Load() {
		// Severed (the overage guard's stop): an update a replay of a buffer
		// would route after it reaches no one (review C5 item 7).
		return
	}
	kind := sessionUpdateKind(n.Update)
	// The overage guard reads every usage_update first, whoever it is for and
	// whenever it comes (plan 052 § D3): a trip ends the running turn before
	// the update can settle it.
	c.noteRateLimit(kind, n)
	if c.conn.severed.Load() {
		// Severed while the rate limit was being handled — the overage
		// guard's own stop, run from it (review C5b item 8): the update is
		// handed to no one after the kill.
		return
	}
	c.mu.Lock()
	if c.sessionID == "" {
		c.pendingUpdates = append(c.pendingUpdates, n)
		c.mu.Unlock()
		return
	}
	// Live, after the load's reply, and held until the session's replay is
	// over (ReplayDone), within the hold's bounds.
	if taken, failed := c.holdLocked(heldFrame{update: &n}, len(n.Update)); taken {
		c.mu.Unlock()
		if failed {
			go func() { _ = c.Close() }()
		}
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
	if h != nil && !c.conn.severed.Load() {
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
			sub.Unrouted = true
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
			if c.conn.severed.Load() {
				return
			}
			h(SubagentNotification{SessionID: root, Kind: SubagentUpdate, SubagentID: id, ChildSessionID: id, State: ChildUnknown, Evicted: true})
		}
	}
	tellGone(evicted)
	if c.conn.severed.Load() {
		// Severed while a removal was being told — the overage guard's stop
		// (review C5c item 9): nothing more reaches the session.
		return
	}
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
