package acp

import (
	"encoding/json"
	"strconv"
	"time"
)

// The claude dialect's foreign turns (plan 052 § E1). claude-agent-acp runs
// model cycles of its own — a background command's or a sub-agent's wake, idle
// compaction, a steer that started a turn — and ACP v1 marks none of them: no
// start, no end. This is the inference that brackets them, one state machine
// with one owner, the client, under c.mu:
//
//   - idle: nothing of craze's is on the wire and the agent is quiet;
//   - prompted: a prompt of craze's is on the wire, until its reply is READ
//     (claudePromptAnswered, on the read goroutine in order with the updates —
//     never in PromptBlocks' deferred cleanup, which runs whenever the caller is
//     next scheduled). Everything the agent sends meanwhile is that turn's;
//   - foreign: a turn the agent started itself;
//   - replaying: from session/load's request until its reply.
//
// Opening. Only from idle, only once session/new or session/load has returned
// (ready), and only on root content (updateClassOf): agent text or thought, a
// tool_call with an id not seen before, a user_message_chunk, or a
// compaction_update in_progress (reason compaction; the rest background).
// Nothing else ever opens one: a session-level kind, a trailing
// tool_call_update, a notice, a plan, sub-agent traffic, any child's update,
// anything during a replay, and kinds this build does not know.
//
// Ending, three ways:
//
//  1. The result. A usage_update carrying cost while foreign is a completion
//     candidate. Content cancels it (the turn goes on; only a new cost usage
//     makes a new candidate, and content restarts no timer). It never completes
//     while a tool call is open — it waits for the close. Otherwise it
//     completes after a quiet grace (ForeignTiming.Grace). A tool_call_update
//     inside the grace is applied and does not cancel it. A follow-up whose
//     first content comes after the grace opens a second foreign turn, or —
//     when a queued prompt went out in between — is absorbed by that prompt's
//     turn (the v1 contract, R2-3: a num_turns:0 placeholder's usage comes
//     before its follow-up with no bound on the gap, v1/acp-agent.js:4165).
//  2. Silence. No root update for ForeignTiming.Silence, or
//     ForeignTiming.SilenceToolOpen with a tool call open, ends it Silent. The
//     ten minutes are t3code's no-frame limit (PROVIDER_RETRY_BUDGET_MS,
//     packages/provider-core/src/server/failure.ts, at 26f70e72fa).
//  3. A user cancel (CancelHeld) while foreign: session/cancel is written, the
//     turn is marked cancelled, and it ends after the grace — the cost usage the
//     adapter sends for the stopped cycle restarts the grace, so the cycle's
//     tool closes land inside it; content no longer holds it, nor does an open
//     tool.
//
// Events. Every transition that is announced — a start, an end — is decided
// under c.mu and announced under dispatchMu, which whoever decides it took
// BEFORE c.mu and holds until the handler returns: the read loop for a claude
// notification (onNotify), a reply hook, a timer, NewSession's flush, Close.
// So two announcements can never pass each other between the lock and the
// handler: an ending a timer decided reaches the session before the start the
// next update decides, and a start before the update that opened it.
//
// Timers are the Clock's, injectable, and each carries the generation it was
// armed under: a callback that waited on dispatchMu while the turn moved on
// finds a newer generation, or another phase, and does nothing.

// The reasons a claude foreign turn carries (ForeignTurn.Reason).
const (
	// ForeignReasonBackground: the agent's own cycle — a background command's
	// or a sub-agent's wake, or a steer that started a turn.
	ForeignReasonBackground = "background"
	// ForeignReasonCompaction: idle compaction, a model call the adapter makes
	// on its own while nothing runs (idle-compaction.js).
	ForeignReasonCompaction = "compaction"
)

// ForeignTiming is the state machine's three limits. A zero field keeps the
// default.
type ForeignTiming struct {
	// Grace is the quiet after a completion candidate before it completes.
	Grace time.Duration
	// Silence ends a foreign turn that has sent no root update for this long,
	// with no tool call open; SilenceToolOpen, with one open.
	Silence         time.Duration
	SilenceToolOpen time.Duration
}

// DefaultForeignTiming is plan 052 § E1's: a 2 s grace, a minute of silence,
// and ten minutes with a tool call open.
var DefaultForeignTiming = ForeignTiming{Grace: 2 * time.Second, Silence: 60 * time.Second, SilenceToolOpen: 10 * time.Minute}

// Clock is the time the client's claude state machine and early-child buffer
// read: the system's in production, a test's own to drive exact interleavings.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a Clock's pending call.
type Timer interface{ Stop() bool }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
func (systemClock) AfterFunc(d time.Duration, f func()) Timer {
	return time.AfterFunc(d, f)
}

// SetClock replaces the client's clock. It is for tests, and is called before
// anything is dispatched.
func (c *Client) SetClock(clk Clock) {
	c.mu.Lock()
	c.clock = clk
	c.mu.Unlock()
}

// SetForeignTiming sets the state machine's limits (a zero field keeps the
// default). Called before the session is set up.
func (c *Client) SetForeignTiming(t ForeignTiming) {
	c.mu.Lock()
	c.ftiming = t
	c.mu.Unlock()
}

func (c *Client) clockLocked() Clock {
	if c.clock == nil {
		return systemClock{}
	}
	return c.clock
}

func (c *Client) timingLocked() ForeignTiming {
	t := c.ftiming
	if t.Grace <= 0 {
		t.Grace = DefaultForeignTiming.Grace
	}
	if t.Silence <= 0 {
		t.Silence = DefaultForeignTiming.Silence
	}
	if t.SilenceToolOpen <= 0 {
		t.SilenceToolOpen = DefaultForeignTiming.SilenceToolOpen
	}
	return t
}

type foreignPhase uint8

const (
	phaseIdle foreignPhase = iota
	phasePrompted
	phaseForeign
	phaseReplaying
	// phaseClosed: the client has closed; nothing opens again.
	phaseClosed
)

func (p foreignPhase) String() string {
	switch p {
	case phaseIdle:
		return "idle"
	case phasePrompted:
		return "prompted"
	case phaseForeign:
		return "foreign"
	case phaseReplaying:
		return "replaying"
	case phaseClosed:
		return "closed"
	}
	return "phase(" + strconv.Itoa(int(p)) + ")"
}

// claudeSeenCap bounds the root tool ids the state machine remembers, to tell
// a tool_call with a new id from one re-sent for a call it has seen.
const claudeSeenCap = 512

// claudeForeign is the state machine's state, all of it under c.mu.
type claudeForeign struct {
	phase foreignPhase
	// ready: session/new or session/load has returned. Before it nothing opens.
	ready bool
	// turn is the client turn a prompted phase belongs to.
	turn int
	seq  uint64
	// The open foreign turn: its id and reason, its completion candidate,
	// whether a user cancel ended it, and its open root tool calls.
	id, reason string
	candidate  bool
	cancelled  bool
	open       map[string]struct{}
	// failErr is FailForeign's: what a turn still open at Close ends with.
	failErr error
	// seen is every root tool call id lately seen, in seenOrder.
	seen      map[string]struct{}
	seenOrder []string
	// The two timers and the generation each was armed under.
	grace, silence       Timer
	graceGen, silenceGen uint64
}

// rootTool is a root update's tool call id and status, for the kinds that
// carry one.
func rootTool(update json.RawMessage) (id, status string) {
	var w struct {
		ToolCallID string `json:"toolCallId"`
		Status     string `json:"status"`
	}
	if json.Unmarshal(update, &w) != nil {
		return "", ""
	}
	return w.ToolCallID, w.Status
}

func terminalStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

// hasCost reports whether a usage_update carries cost: the result's usage, the
// only one that does (v1/acp-agent.js:4027).
func hasCost(update json.RawMessage) bool {
	var w struct {
		Cost json.RawMessage `json:"cost"`
	}
	if json.Unmarshal(update, &w) != nil {
		return false
	}
	return len(w.Cost) > 0 && string(w.Cost) != "null"
}

// compactionStatus is a compaction_update's status.
func compactionStatus(update json.RawMessage) string {
	var w struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(update, &w)
	return w.Status
}

func (f *claudeForeign) noteSeenLocked(id string) (fresh bool) {
	if id == "" {
		return false
	}
	if _, ok := f.seen[id]; ok {
		return false
	}
	if f.seen == nil {
		f.seen = make(map[string]struct{})
	}
	if len(f.seenOrder) >= claudeSeenCap {
		delete(f.seen, f.seenOrder[0])
		f.seenOrder = f.seenOrder[1:]
	}
	f.seen[id] = struct{}{}
	f.seenOrder = append(f.seenOrder, id)
	return true
}

// foreignRootLocked is the state machine's read of one update of the root
// session, made on the read loop under dispatchMu and c.mu, before the update
// is handed on. It answers the start to announce when this update opened a
// foreign turn — announced ahead of the update itself — and nil otherwise.
func (c *Client) foreignRootLocked(kind string, update json.RawMessage) *ForeignTurn {
	f := &c.cf
	class := updateClassOf(kind)
	// A tool_call's id is read in every phase, so the machine knows it seen
	// when it next opens; a tool_call_update's only while foreign, below.
	var toolID, status string
	fresh := false
	if kind == UpdateToolCall {
		toolID, status = rootTool(update)
		fresh = f.noteSeenLocked(toolID)
	}
	// What the update is to the machine: content (opens from idle, and
	// cancels a candidate), and its reason.
	content, reason := false, ForeignReasonBackground
	switch class {
	case classContent:
		content = kind != UpdateToolCall || fresh
	case classEcho:
		content = true
	case classCompaction:
		if compactionStatus(update) == "in_progress" {
			content, reason = true, ForeignReasonCompaction
		}
	}
	var start *ForeignTurn
	switch f.phase {
	case phaseIdle:
		if !f.ready || !content {
			return nil
		}
		start = c.openForeignLocked(reason)
	case phaseForeign:
	default:
		// prompted absorbs everything; replaying and closed open nothing.
		return nil
	}
	// foreign, freshly opened or running.
	if kind == UpdateToolCallUpd {
		toolID, status = rootTool(update)
	}
	// fresh is a tool_call with an id (noteSeenLocked).
	if fresh && !terminalStatus(status) {
		f.open[toolID] = struct{}{}
	}
	closed := false
	// status is only read for the two tool kinds.
	if terminalStatus(status) {
		if _, ok := f.open[toolID]; ok {
			delete(f.open, toolID)
			closed = true
		}
	}
	switch {
	case content && f.candidate && !f.cancelled && start == nil:
		// The turn goes on: only a new cost usage makes a new candidate.
		f.candidate = false
		c.stopGraceLocked()
	case kind == updateUsage && hasCost(update):
		f.candidate = true
		if len(f.open) == 0 || f.cancelled {
			c.armGraceLocked()
		} else {
			c.stopGraceLocked()
		}
	case closed && f.candidate && len(f.open) == 0 && f.grace == nil:
		// The candidate waited for this close.
		c.armGraceLocked()
	}
	// Any root update is a frame: the silence rule starts again, with the
	// limit the open tools now call for.
	c.armSilenceLocked()
	return start
}

// openForeignLocked opens a foreign turn and answers its start.
func (c *Client) openForeignLocked(reason string) *ForeignTurn {
	f := &c.cf
	f.seq++
	f.phase = phaseForeign
	f.id = "claude-" + strconv.FormatUint(f.seq, 10)
	f.reason = reason
	f.candidate, f.cancelled = false, false
	f.open = make(map[string]struct{})
	return &ForeignTurn{ID: f.id, Reason: reason, Running: true}
}

// endForeignTurnLocked ends the open foreign turn and answers its ending. The
// timers are stopped and their generations moved on, so a callback already
// waiting does nothing.
func (c *Client) endForeignTurnLocked(silent bool, err error) *ForeignTurn {
	f := &c.cf
	ev := &ForeignTurn{ID: f.id, Reason: f.reason, Cancelled: f.cancelled && err == nil, Silent: silent, Err: err}
	c.stopGraceLocked()
	c.stopSilenceLocked()
	f.phase = phaseIdle
	f.id, f.reason = "", ""
	f.candidate, f.cancelled = false, false
	f.open = nil
	return ev
}

func (c *Client) armGraceLocked() {
	f := &c.cf
	c.stopGraceLocked()
	gen := f.graceGen
	f.grace = c.clockLocked().AfterFunc(c.timingLocked().Grace, func() { c.foreignTimerFired(gen, false) })
}

func (c *Client) stopGraceLocked() {
	f := &c.cf
	if f.grace != nil {
		f.grace.Stop()
		f.grace = nil
	}
	f.graceGen++
}

func (c *Client) armSilenceLocked() {
	f := &c.cf
	c.stopSilenceLocked()
	t := c.timingLocked()
	d := t.Silence
	if len(f.open) > 0 {
		d = t.SilenceToolOpen
	}
	gen := f.silenceGen
	f.silence = c.clockLocked().AfterFunc(d, func() { c.foreignTimerFired(gen, true) })
}

func (c *Client) stopSilenceLocked() {
	f := &c.cf
	if f.silence != nil {
		f.silence.Stop()
		f.silence = nil
	}
	f.silenceGen++
}

// Test seams, nil in production. testForeignAnnounce runs in a timer callback
// between its decision (made under c.mu, released) and its announcement — the
// gap a read-loop decision must not fall into — and testClaudeDispatch on the
// read loop just before a claude notification takes dispatchMu.
var (
	testForeignAnnounce func(end *ForeignTurn)
	testClaudeDispatch  func(method string)
)

// foreignTimerFired is a timer's callback: the grace (silence false) or the
// silence rule. It acts only if the turn is still the one it was armed in, at
// the generation it was armed under.
func (c *Client) foreignTimerFired(gen uint64, silence bool) {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	c.mu.Lock()
	f := &c.cf
	var end *ForeignTurn
	switch {
	case f.phase != phaseForeign:
	case silence && gen == f.silenceGen:
		f.silence = nil
		end = c.endForeignTurnLocked(true, nil)
	case !silence && gen == f.graceGen && f.candidate && (len(f.open) == 0 || f.cancelled):
		f.grace = nil
		end = c.endForeignTurnLocked(false, nil)
	}
	h := c.foreignHandler
	c.mu.Unlock()
	if testForeignAnnounce != nil && end != nil {
		testForeignAnnounce(end)
	}
	fireForeign(h, nil, end)
}

// enterPromptedLocked is PromptBlocks' move on claude, in the section that
// opens the prompt's turn, once it has refused a prompt while the agent runs a
// turn of its own (foreignRunningLocked): an idle machine is prompted for turn.
func (c *Client) enterPromptedLocked(turn int) {
	if f := &c.cf; f.phase == phaseIdle {
		f.phase = phasePrompted
		f.turn = turn
	}
}

// claudePromptAnswered is a claude prompt's reply hook: the prompted turn is
// over the moment its reply is read, so an update the read loop dispatches
// after it sees idle (§ E1).
func (c *Client) claudePromptAnswered(turn int) {
	c.mu.Lock()
	c.promptOverLocked(turn)
	c.mu.Unlock()
}

// promptOverLocked moves a prompted machine back to idle, if turn is still the
// one it is prompted for. PromptBlocks' cleanup calls it too, for a prompt
// whose reply was never read (the caller gave up, the connection closed).
func (c *Client) promptOverLocked(turn int) {
	if f := &c.cf; f.phase == phasePrompted && f.turn == turn {
		f.phase = phaseIdle
	}
}

// foreignCancelLocked is a user cancel while the agent runs a turn of its own:
// the turn is to end cancelled, after the grace (the file's comment, way 3).
func (c *Client) foreignCancelLocked() {
	f := &c.cf
	if f.phase != phaseForeign {
		return
	}
	f.cancelled = true
	f.candidate = true
	c.armGraceLocked()
}

// FailForeign records what a claude foreign turn still open when the client
// closes ends with: the session stopping for a reason of its own (plan 052 §
// E2). Close ends that turn with err, a failed turn; the first error stands.
func (c *Client) FailForeign(err error) {
	c.mu.Lock()
	if c.cf.failErr == nil {
		c.cf.failErr = err
	}
	c.mu.Unlock()
}

// closeForeign ends a claude foreign turn still open when the client closes
// (Close), with FailForeign's error, else ErrClosed; and closes the machine, so
// nothing opens again.
func (c *Client) closeForeign() {
	if c.dialect != DialectClaude {
		return
	}
	c.mu.Lock()
	if c.cf.phase != phaseForeign {
		// Nothing to announce: closed from here, so nothing opens. A read
		// loop deciding after this sees closed.
		c.closeMachineLocked()
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	if !c.lockDispatchWithin(closeCancelWait) {
		// The read loop is held by a handler that cannot deliver (a consumer
		// that stopped reading): the ending is dropped rather than the close
		// held — the session is going either way.
		c.mu.Lock()
		c.closeMachineLocked()
		c.mu.Unlock()
		return
	}
	defer c.dispatchMu.Unlock()
	c.mu.Lock()
	var end *ForeignTurn
	if c.cf.phase == phaseForeign {
		err := c.cf.failErr
		if err == nil {
			err = ErrClosed
		}
		end = c.endForeignTurnLocked(false, err)
	}
	c.closeMachineLocked()
	h := c.foreignHandler
	c.mu.Unlock()
	fireForeign(h, nil, end)
}

// closeMachineLocked retires the timers and closes the machine: nothing opens
// again.
func (c *Client) closeMachineLocked() {
	c.stopGraceLocked()
	c.stopSilenceLocked()
	c.cf.phase = phaseClosed
}

// lockDispatchWithin takes dispatchMu, or gives up after d. A lock taken after
// the give-up is released at once by the goroutine that took it.
func (c *Client) lockDispatchWithin(d time.Duration) bool {
	if c.dispatchMu.TryLock() {
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
	case <-time.After(d):
		close(gaveUp)
		return false
	}
}

// steerStartedTurn is a _session/steering reply of startedNewTurn (plan 052 §
// B7, review C3 carried): idleBehavior promptRequired should rule it out, and
// when it happens anyway the adapter is running the steered text as a turn of
// its own. From idle that is a foreign turn, opened here on the read loop in
// order with the updates; a prompted or foreign machine already holds it.
func (c *Client) steerStartedTurn() {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	c.mu.Lock()
	var start *ForeignTurn
	if f := &c.cf; f.phase == phaseIdle && f.ready {
		start = c.openForeignLocked(ForeignReasonBackground)
		c.armSilenceLocked()
	}
	h := c.foreignHandler
	c.mu.Unlock()
	fireForeign(h, start, nil)
}

// ForeignPhase names the claude machine's phase, for tests and diagnostics.
func (c *Client) ForeignPhase() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cf.phase.String()
}
