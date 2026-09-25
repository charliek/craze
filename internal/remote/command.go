package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
// a byte, and cleared only by an answer that says nothing ran — unavailable,
// not_accepting, stale_model, the codes the host never stores (protocol.Retry,
// in_progress aside, which is the command still running) — and never for a
// command one of whose attempts was on a connection that went before its
// answer came (stray): the host may still run that one.

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
	done   chan cmdResult  // one answer at a time, to the caller

	// Guarded by Client.mu.
	//
	// want says the caller waits for an answer to the current attempt: set
	// when it is first sent and on each retry, cleared when an answer or a
	// resolution is handed over; while it is set and out is not, the command
	// is held for a connection, and the reconnect sends it. out says the
	// attempt is on a connection, unanswered; attempt numbers the attempts,
	// and a reply to another is stale; resent says the attempt went out for a
	// command that may already have run under this identity — a resend after
	// a resume — so an in_progress answer is its first execution still
	// running, which the client waits out itself.
	want    bool
	out     bool
	attempt int
	resent  bool
	// ranUnder is the client identity the command may have run under, 0 when
	// it cannot have run at all; stray says an attempt of it was on a
	// connection that went before its answer came, so no later "nothing ran"
	// answer clears ranUnder. resolved says the client settled it (outcome
	// unknown, not run): nothing more is sent. gone says its caller has
	// returned.
	ranUnder uint64
	stray    bool
	resolved bool
	gone     bool
	backoff  time.Duration
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
	if l, err := protocol.MarshalLine(protocol.Request{JSONRPC: protocol.JSONRPCVersion,
		ID: json.RawMessage(longestRequestID), Method: method, Params: raw}); err != nil {
		return id, err
	} else if len(l)-1 > c.maxLine() {
		return id, ErrRequestTooLarge
	}
	cmd := &command{id: id, method: method, params: raw, done: make(chan cmdResult, 1), want: true}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return id, err
	}
	c.cmds = append(c.cmds, cmd)
	c.mu.Unlock()
	defer c.forget(cmd)
	c.attempt(cmd)
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

// attempt sends cmd on the current connection — THE one place a command is
// sent, so the resend rule is checked here: a command that may have run under
// another identity than the client's now is resolved outcome-unknown
// (resume_lost) instead. A command that may have run under this one is a
// resend (resent): an in_progress answer to it is its first execution still
// running, which the client waits out and resends. With no connection (the
// client is reconnecting) it sends nothing: the reconnect sends it once
// connected. A stopped client resolves it.
func (c *Client) attempt(cmd *command) {
	c.mu.Lock()
	switch {
	case cmd.gone || cmd.resolved || !cmd.want:
		c.mu.Unlock()
		return
	case c.err != nil:
		c.resolveLocked(cmd, protocol.ReasonDisconnected)
		c.mu.Unlock()
		return
	case cmd.ranUnder != 0 && cmd.ranUnder != c.identity:
		c.resolveLocked(cmd, protocol.ReasonResumeLost)
		c.mu.Unlock()
		return
	}
	w := c.cur
	if w == nil {
		cmd.out = false
		c.mu.Unlock()
		return
	}
	cmd.out = true
	cmd.attempt++
	cmd.resent = cmd.ranUnder != 0
	ran := cmd.ranUnder
	cmd.ranUnder = c.identity
	n := cmd.attempt
	c.mu.Unlock()
	// A failed send is a connection going, and the reconnect decides what
	// becomes of the attempt: it stays out, as one that may have reached the
	// host — unless not a byte of it was written, when it is held for the
	// next connection exactly as it stood before.
	_, err := c.send(w, cmd.method, cmd.params, func(resp *protocol.Response, err error) { c.commandReply(cmd, n, resp, err) })
	if errors.Is(err, errUnsent) {
		c.mu.Lock()
		if cmd.attempt == n && cmd.out {
			cmd.out = false
			cmd.ranUnder = ran
		}
		c.mu.Unlock()
	}
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

// commandReply is attempt n's reply, on its connection's reader; err says the
// connection went first, and then the reconnect decides.
func (c *Client) commandReply(cmd *command, n int, resp *protocol.Response, err error) {
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cmd.gone || cmd.resolved || !cmd.want || cmd.attempt != n {
		return
	}
	cmd.out = false
	if pe := resp.Error; pe != nil {
		code := pe.Data.Code
		if code == protocol.CodeInProgress && cmd.resent {
			// A resend after a resume found the first execution still
			// running: wait it out and resend the same id (§3.14).
			cmd.backoff = min(max(2*cmd.backoff, retryBackoffMin), retryBackoffMax)
			time.AfterFunc(cmd.backoff, func() { c.attempt(cmd) })
			return
		}
		if protocol.Retry(code) && code != protocol.CodeInProgress && !cmd.stray {
			// Never stored: nothing ran under this id.
			cmd.ranUnder = 0
		}
	}
	cmd.deliver(cmdResult{resp: resp})
}
