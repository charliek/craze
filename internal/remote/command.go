package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/charliek/craze/internal/protocol"
)

// Commands (plan 027 §3.6, §3.14): every mutating method carries a commandId
// the client mints — a canonical positive decimal, counted from 1 and never
// reused (Client.nextCmd) — and the client remembers every command whose
// caller waits, its method and its params with that id, until its answer is
// handed over. A resend of the SAME id is how a client asks "did it run?": the
// host's receipts table answers a completed one with its stored answer, never
// a second execution.
//
// THE RESEND RULE. A command that may have run under one client identity is
// never sent under another. The one way two connections share an identity is
// a hello the host answered resumed: true (the same client id, the same engine
// incarnation): only then is a command whose reply was lost resent. After
// resumed: false — a retired client, a replaced engine, a restarted host —
// nothing that may have run is resent: it resolves ErrOutcomeUnknown, reason
// resume_lost (astra 13: a resend under a fresh client id would be a new
// command to the host, and could run a completed one twice). The rule is
// checked in one place, attempt, which every send of a command goes through;
// the client's identity counter (Client.identity) moves on every hello that
// did not resume, so a fresh id that happens to spell an old one (ids restart
// at c-1 per engine) is still another identity.
//
// "May have run" (command.ranUnder) is set by every send that may have written
// a byte, and cleared only by the answer to the command's LATEST attempt when
// that answer says nothing ran — unavailable, not_accepting, stale_model, the
// codes the host never stores (protocol.Retry, in_progress aside, which is the
// command still running) — AND no earlier attempt of it is unanswered on any
// connection (command.unanswered): an attempt on a connection that went before
// its answer came is unanswered for good, and the host may still run it, so a
// busy refusal of a later attempt never erases it (X18 1).
//
// ONE ATTEMPT AT A TIME (X18 1). An attempt is outstanding (command.out) from
// the moment it is claimed to its answer — or, when its connection goes first,
// until the reconnect has counted it unanswered; attempt claims under
// Client.mu and sends nothing while one is outstanding. So the caller's own
// attempt, a retry by code, the in_progress wait-out and a reconnect's resend
// never overlap: two attempts in flight would let the first run and the second
// be refused, and the refusal would say "nothing ran".
//
// A resend that in_progress or busy answered waits out its backoff (command
// .waiting) before its next attempt: nothing — a reconnect's publication
// included — sends it while the backoff runs (X21), and only the caller's
// context bounds how long it waits on a host that stays at its cap.
//
// WIRE ORDER (X18 3, X21, C8c). Each command is numbered as the write of its
// first attempt begins — under the connection's write lock, before its first
// byte (command.key) — so the order of the numbers is the order on the wire,
// and a command partly written is numbered even while its writer has not yet
// returned from the write: it always sorts before one never sent. A write that
// sends nothing gives its number back (unless another attempt has written
// under it meanwhile). A reconnect resends in that order — a command never
// written after every one that was, in the order it was issued — and sends
// everything it holds before it publishes the connection, so no new command,
// retry or wait-out overtakes a resend on it.
//
// A command is sized against the host it is sent to: one over the inbound
// limit of the host a reconnect reached (its hello's) is never claimed, and
// resolves "not run" with ErrRequestTooLarge — not a byte was written (X21).

// CommandOptions shape one Command.
type CommandOptions struct {
	// Retry resends the SAME command id, after a short backoff, while the
	// answer's code is one protocol.Retry allows — unavailable,
	// not_accepting, in_progress and stale_model: nothing ran (or, for
	// in_progress, the command is still running), so a resend is a genuine
	// attempt — and never on any other code, whose answer is the command's
	// own. The retries end with ctx.
	Retry bool
}

// command is one command whose caller waits (Command).
type command struct {
	id     string
	method string
	params json.RawMessage // with its commandId
	// size is its request line at its longest (the longest request id),
	// without the newline: what a host's inbound limit is held against.
	size int
	done chan cmdResult // one answer at a time, to the caller

	// kmu guards the command's place in wire order, a leaf lock (taken under
	// a connection's write lock, and under Client.mu). seq is that place
	// (Client.wireOrder), 0 while it has none; keyedBy is the attempt whose
	// write took seq and has not written a byte of it — 0 once any write of
	// the command's has written one, when seq is the command's for good.
	kmu     sync.Mutex
	seq     uint64
	keyedBy int

	// Guarded by Client.mu.
	//
	// want says the caller waits for an answer to the current attempt: set
	// when it is registered and on each retry, cleared when an answer or a
	// resolution is handed over; while it is set and out is not, the command
	// is held for a connection, and the reconnect sends it. out says an
	// attempt is outstanding — claimed, and neither answered nor counted lost
	// by a reconnect — and while it is, no other attempt is made (one at a
	// time). attempt numbers the attempts, and a reply to another is stale;
	// resent says the attempt went out for a command that may already have
	// run under this identity — a resend after a resume — so an in_progress
	// answer is its first execution still running, which the client waits out
	// itself. waiting says that wait-out's backoff is running: the command
	// is sent again when it runs out, and not before (X21).
	want    bool
	out     bool
	attempt int
	resent  bool
	waiting bool
	// ranUnder is the client identity the command may have run under, 0 when
	// it cannot have run at all; unanswered counts its attempts that may have
	// reached a host and have no answer — one on a connection that went first
	// never will — and while it is not 0 no "nothing ran" answer clears
	// ranUnder. resolved says the client settled it (outcome unknown, not
	// run): nothing more is sent. gone says its caller has returned.
	ranUnder   uint64
	unanswered int
	resolved   bool
	gone       bool
	backoff    time.Duration
}

// cmdResult is an answer: a reply, or the client's own resolution.
type cmdResult struct {
	resp *protocol.Response
	err  error
}

// deliver hands r to the caller; Client.mu is held. The channel holds one:
// each attempt is answered at most once, and the caller takes each answer
// before it asks for another attempt.
func (cmd *command) deliver(r cmdResult) {
	cmd.want = false
	cmd.out = false
	select {
	case cmd.done <- r:
	default:
	}
}

// Command sends one mutating method (params: its params object, sessionId
// included; the client adds commandId) and waits for its answer, decoded into
// result (nil: ignored). It returns the command id it minted.
//
// A refusal is an *Error. A command whose connection goes before its answer is
// resent under the same id only after a reconnect the host answered resumed:
// true (the stored answer comes back, or in_progress, which the client waits
// out and resends); otherwise it resolves *OutcomeUnknownError (reason
// resume_lost, or disconnected once the redials are spent), and one that never
// left the client is ErrNotRun. Retry by code is opts.Retry. A ctx that ends
// returns ctx.Err(): the command may still run, and a resend of its id (none
// is made for it) would say.
func (c *Client) Command(ctx context.Context, method string, params, result any, opts CommandOptions) (string, error) {
	if info, ok := protocol.Method(method); !ok || !info.Mutating {
		return "", fmt.Errorf("remote: %s is not a command: use Call", method)
	}
	raw, err := paramsJSON(params)
	if err != nil {
		return "", err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return "", fmt.Errorf("remote: %s's params must be an object", method)
	}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return "", err
	}
	id := strconv.FormatUint(c.nextCmd, 10)
	c.nextCmd++
	c.mu.Unlock()
	obj["commandId"] = json.RawMessage(strconv.Quote(id))
	if raw, err = paramsJSON(obj); err != nil {
		return id, err
	}
	// Sized before it is remembered, with the longest request id there is:
	// a command over the host's inbound limit is never sent at all.
	l, err := protocol.MarshalLine(protocol.Request{JSONRPC: protocol.JSONRPCVersion,
		ID: json.RawMessage(longestRequestID), Method: method, Params: raw})
	if err != nil {
		return id, err
	}
	size := len(l) - 1
	if size > c.maxLine() {
		return id, ErrRequestTooLarge
	}
	cmd := &command{id: id, method: method, params: raw, size: size, done: make(chan cmdResult, 1), want: true}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return id, err
	}
	c.cmds = append(c.cmds, cmd)
	c.mu.Unlock()
	defer c.forget(cmd)
	if h := c.hooks.registered; h != nil {
		h(id)
	}
	c.attempt(cmd)
	if h := c.hooks.attempted; h != nil {
		h(id)
	}
	backoff := retryBackoffMin
	for {
		select {
		case r := <-cmd.done:
			if r.err != nil {
				return id, r.err
			}
			pe := r.resp.Error
			if pe == nil {
				return id, decodeReply(r.resp, result)
			}
			if !opts.Retry || !protocol.Retry(pe.Data.Code) {
				return id, newError(pe)
			}
			if h := c.hooks.retrying; h != nil {
				h(id)
			}
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return id, ctx.Err()
			}
			backoff = min(2*backoff, retryBackoffMax)
			c.retry(cmd)
		case <-ctx.Done():
			return id, ctx.Err()
		}
	}
}

// maxLine is the host's inbound line limit, as its last hello said.
func (c *Client) maxLine() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := c.hello.Limits.InboundLine; n > 0 {
		return n
	}
	return protocol.InboundLineMax
}

// forget drops cmd once its caller has returned.
func (c *Client) forget(cmd *command) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cmd.gone = true
	for i, x := range c.cmds {
		if x == cmd {
			c.cmds = append(c.cmds[:i], c.cmds[i+1:]...)
			break
		}
	}
}

// retry asks for another attempt of cmd, its caller having taken an answer
// whose code allows one.
func (c *Client) retry(cmd *command) {
	c.mu.Lock()
	cmd.want = true
	c.mu.Unlock()
	c.attempt(cmd)
}

// attempt sends cmd on the current connection (attemptOn).
func (c *Client) attempt(cmd *command) { _ = c.attemptOn(cmd, nil) }

// attemptOn sends cmd on via — the connection a reconnect is adopting — or,
// via nil, on the current one: THE one place a command is sent, so the resend
// rule and one-attempt-at-a-time are checked here, under Client.mu. It sends
// nothing while an attempt is outstanding (out) or a wait-out's backoff runs
// (waiting), or once the caller has its answer or has gone. A command that may
// have run under another identity than the client's now is resolved
// outcome-unknown (resume_lost) instead; one that may have run under this one
// is a resend (resent): an in_progress answer to it is its first execution
// still running, which the client waits out and resends. With no connection
// (the client is reconnecting) it sends nothing: the reconnect sends it before
// it publishes the next one. A stopped client resolves it, and so does a
// connection whose host reads less than it (ErrRequestTooLarge: nothing is
// written). errUnsent says not a byte of it was written, and it is held again
// exactly as it stood before; ErrConnectionLost that some may have been.
func (c *Client) attemptOn(cmd *command, via *wire) error {
	c.mu.Lock()
	switch {
	case cmd.gone || cmd.resolved || !cmd.want || cmd.out || cmd.waiting:
		c.mu.Unlock()
		return nil
	case c.err != nil:
		c.resolveLocked(cmd, protocol.ReasonDisconnected)
		c.mu.Unlock()
		return nil
	case cmd.ranUnder != 0 && cmd.ranUnder != c.identity:
		c.resolveLocked(cmd, protocol.ReasonResumeLost)
		c.mu.Unlock()
		return nil
	}
	w := via
	if w == nil {
		w = c.cur
	}
	if w == nil {
		c.mu.Unlock()
		return nil
	}
	if cmd.size > w.maxLine {
		// The host this connection reached reads less than the command
		// (its hello's limit, smaller than the one it was sized against):
		// it is never claimed, and nothing of it is written (X21).
		cmd.resolved = true
		cmd.deliver(cmdResult{err: c.tooLarge(cmd, w.maxLine)})
		c.mu.Unlock()
		return ErrRequestTooLarge
	}
	cmd.out = true
	cmd.attempt++
	cmd.unanswered++
	cmd.resent = cmd.ranUnder != 0
	ran := cmd.ranUnder
	cmd.ranUnder = c.identity
	n := cmd.attempt
	c.mu.Unlock()
	if h := c.hooks.claimed; h != nil {
		h(cmd.id)
	}
	// A failed send is a connection going, and the reconnect decides what
	// becomes of the attempt: it stays out, as one that may have reached the
	// host — unless not a byte of it was written, when it is held for the
	// next connection exactly as it stood before. Its place in wire order is
	// taken as its write begins, and given back if it writes nothing (C8c).
	_, err := c.send(w, cmd.method, cmd.params, func(resp *protocol.Response, err error) { c.commandReply(cmd, n, resp, err) },
		writeHooks{begin: func() bool { c.keyBegin(cmd, n); return true }, end: func(k int) { c.keyEnd(cmd, n, k) }})
	if errors.Is(err, errUnsent) {
		c.mu.Lock()
		if cmd.attempt == n && cmd.out {
			cmd.out = false
			cmd.unanswered--
			cmd.ranUnder = ran
		}
		c.mu.Unlock()
	}
	return err
}

// keyBegin is attempt n's write beginning, under its connection's write lock:
// a command with no place in wire order takes the next, before a byte of it
// is written.
func (c *Client) keyBegin(cmd *command, n int) {
	cmd.kmu.Lock()
	defer cmd.kmu.Unlock()
	if cmd.seq == 0 {
		cmd.seq, cmd.keyedBy = c.wireOrder.Add(1), n
	}
}

// keyEnd is attempt n's write having returned, k bytes written, under the
// same lock: a write that wrote a byte fixes the command's place (taking one
// if an attempt that wrote nothing gave it back meanwhile); one that wrote
// nothing gives back the place it took, if no write has written under it.
func (c *Client) keyEnd(cmd *command, n, k int) {
	cmd.kmu.Lock()
	defer cmd.kmu.Unlock()
	switch {
	case k > 0:
		if cmd.seq == 0 {
			cmd.seq = c.wireOrder.Add(1)
		}
		cmd.keyedBy = 0
	case cmd.keyedBy == n:
		cmd.seq, cmd.keyedBy = 0, 0
	}
}

// key is cmd's place in wire order, 0 for none.
func (cmd *command) key() uint64 {
	cmd.kmu.Lock()
	defer cmd.kmu.Unlock()
	return cmd.seq
}

// resolveLocked settles cmd with the client's own answer — outcome unknown
// (reason) when it may have run, ErrNotRun when it cannot have — and sends
// nothing more of it; c.mu is held.
func (c *Client) resolveLocked(cmd *command, reason protocol.Reason) {
	if cmd.resolved {
		return
	}
	cmd.resolved = true
	err := ErrNotRun
	if cmd.ranUnder != 0 {
		err = &OutcomeUnknownError{Method: cmd.method, CommandID: cmd.id, Reason: reason}
	}
	cmd.deliver(cmdResult{err: err})
}

// tooLarge is why cmd, over the inbound limit (limit bytes) of the host a
// connection reached, is settled without an attempt: not run, with why — a
// *TooLargeError — when no earlier attempt of it may have run; outcome unknown
// (resume_lost) when one may have, since the resumed connection cannot carry
// its resend. c.mu is held.
func (c *Client) tooLarge(cmd *command, limit int) error {
	if cmd.ranUnder != 0 {
		return &OutcomeUnknownError{Method: cmd.method, CommandID: cmd.id, Reason: protocol.ReasonResumeLost}
	}
	return &TooLargeError{Method: cmd.method, CommandID: cmd.id, Size: cmd.size, Limit: limit}
}

// waitedOut is a wait-out's backoff having run out: the command is held
// again, and sent on the connection calls go to — or, while the client
// reconnects, by the reconnect's publication.
func (c *Client) waitedOut(cmd *command) {
	c.mu.Lock()
	cmd.waiting = false
	c.mu.Unlock()
	c.attempt(cmd)
}

// commandReply is attempt n's reply, on its connection's reader; err says the
// connection went first, and then the reconnect decides (the attempt stays
// out until it counts it lost, unanswered for good).
func (c *Client) commandReply(cmd *command, n int, resp *protocol.Response, err error) {
	if err != nil {
		return
	}
	if h := c.hooks.replied; h != nil {
		defer h(cmd.id)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cmd.gone || cmd.resolved || !cmd.want || !cmd.out || cmd.attempt != n {
		return
	}
	cmd.out = false
	cmd.unanswered--
	if pe := resp.Error; pe != nil {
		code := pe.Data.Code
		if cmd.resent && (code == protocol.CodeInProgress || (code == protocol.CodeUnavailable && pe.Data.Reason == protocol.ReasonBusy)) {
			// A resend of a command that may have run found its first
			// execution still running — or was refused by the host's cap on
			// commands in flight, which answers before the receipts table is
			// asked and so says nothing of the first execution: neither is
			// the command's answer. Wait it out and resend the same id
			// (§3.14, §3.6) — once the backoff has run, and not before: a
			// reconnect's publication passes a waiting command over (X21).
			cmd.backoff = min(max(2*cmd.backoff, retryBackoffMin), retryBackoffMax)
			cmd.waiting = true
			time.AfterFunc(cmd.backoff, func() { c.waitedOut(cmd) })
			return
		}
		if protocol.Retry(code) && code != protocol.CodeInProgress && cmd.unanswered == 0 {
			// Never stored, and no earlier attempt is unanswered: nothing
			// ran under this id.
			cmd.ranUnder = 0
		}
	}
	cmd.deliver(cmdResult{resp: resp})
}
