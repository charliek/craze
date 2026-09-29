package control

import (
	"encoding/json"
	"sync"

	"github.com/charliek/craze/internal/protocol"
)

// Stopping the session, and the close fence over attaches (plan 030 §3.6,
// §3.6a).
//
// # session.stop
//
// A server built with Options.Stop serves session.stop and says so — its info
// document's capabilities.stop is true (sessionInfoReady); one without refuses
// it at dispatch, unsupported, reason stop_unsupported, byte for byte as every
// host before plan 030 did (protocol.MethodInfo.Capability). The reply is a
// RECEIPT, never the stop's completion: what completes a stop is the host's
// own lifecycle — the engine's close, then the server's close, the registry
// entry and socket, the claims, the process's exit — and a handler must
// neither wait for that nor start it. Server.Close waits for the handlers, so
// a handler that closed the server would wait for itself.
//
// For the first stop the server accepts, in this order, on its handler's
// goroutine (sessionStop):
//
//  1. a close fence of the stop's own goes up over attach reservations
//     (FenceAttaches), never to be released: a stop is one way. From here a
//     new session.attach is refused unavailable, reason closing;
//  2. the receipt {} is queued (conn.stopReceipt), outside the writer budget
//     (outbox.admit): it never waits for room;
//  3. the request is handed to Options.Stop, the host's coordinator.
//
// So the fence is up before the receipt can be read — a client that reads it
// and attaches, on any connection, is refused closing, never half-attached.
// The receipt is queued before the coordinator hears of the stop, so it
// precedes, on its connection, every line the stop goes on to cause there —
// the closing records the engine's close commits, and reset{session_closed} —
// and a client that ends its read at the session's end has read its receipt
// by then (astra r2-c1 1). And it is queued without waiting — never refused
// for room, taking nothing from the ordinary budget or the reset's reserve —
// so a peer that has stopped reading, its outbox full, never holds the stop
// back from the coordinator. The receipt reaches its client before the
// connection closes for the session's end: the request holds its admission
// slot until its reply is written, and an ending connection closes only once
// nothing it admitted is unwritten (conn.end).
//
// Every later stop — the same client's, another's, the same commandId resent,
// one arriving while the first is still being handed over (stopOnce waits for
// it) — is answered {} as an ordinary reply and is otherwise nothing: it
// JOINS the first, whose sequence the coordinator runs once. A stop that
// arrives once the session's end is already under way — a later one, or the
// first after a signal or the idle exit set the end going — causes nothing,
// and its receipt may follow closing traffic that end had already put on its
// connection: a client whose stop is outstanding when the session ends has
// its answer either way. A stop makes no engine call, so it is in no receipts
// table (a resend is answered {} again, never unknown_command), takes no
// host-wide command slot, and waits for no reply barrier: nothing it causes
// is committed before its receipt is queued. Nor is it held back when its
// client has since moved on to another connection (conn.command's movedOn):
// the stop was that client's, and stopping twice is stopping once.
//
// # The close fence
//
// FenceAttaches is the primitive the coordinator's stop sequence starts with,
// and the idle exit's FenceClose reuses, reversibly (§3.6, "The decision is
// atomic"):
// it goes up under the server's attachment lock (Server.attachMu), which an
// attach holds from its fence check to the install of its pending attachment
// (conn.reserve), and it reads the count of attachments the server holds in
// every state but closed — pending, live, closing — under the same lock. So
// no attach slips in between the fence and the count: one that checked before
// the fence is in the count, and every one after is refused. While the fence
// stays up the count can only fall. release lowers it; attaches are admitted
// again once no fence is up. TestAStopRacingANewAttach holds an attach
// between its check and its install and proves a fence waits for it there
// (TestHooks.FenceWaits).

// StopFunc is the host's lifecycle coordinator as the server hands it a stop
// (Options.Stop; plan 030 §3.6a). The server calls it at most once in its
// life, for the first session.stop it accepts, on that request's handler
// goroutine, once the stop's own close fence is up and its receipt is queued
// — so everything the stop sequence causes on the stopping connection follows
// the receipt.
//
// It must return promptly and do the stop's work elsewhere, on a goroutine of
// its own: it must not wait on the engine, the stream or any connection, and
// must not call Server.Close — which waits for the handlers, this one among
// them — or Server.SetEngine. It may call FenceAttaches. What the host then
// runs is its own (§3.6a): the engine's close, which authors a running turn's
// ending and ends every attachment's stream with reset{session_closed}; then
// the close order — the flush, Server.Close, the registry entry and the
// socket, the claims — and the exit, exactly once however many ways a stop
// reaches it (this, a signal, the idle exit).
type StopFunc func(StopRequest)

// StopRequest is one session.stop the server accepted: who asked, for the
// coordinator's own record.
type StopRequest struct {
	// Client is the asking connection's client id, and CommandID the command
	// id it sent.
	Client    string
	CommandID string
}

// serves reports whether this server serves a method a host may refuse
// (protocol.MethodInfo.HostUnsupported): one gated by a session capability
// it advertises — session.stop, with Options.Stop set. The hub's methods are
// never served.
func (s *Server) serves(info protocol.MethodInfo) bool {
	return info.Capability == protocol.CapabilityStop && s.opts.Stop != nil
}

// FenceAttaches raises a close fence over the server's attach reservations
// (plan 030 §3.6, §3.6a; the package's "The close fence"): from its return
// until release, a new session.attach is refused unavailable, reason closing.
// attached is how many attachments the server counts — every one not yet
// closed, pending (reserved, not yet answered), live or closing, whose
// connection's peer has not half-closed (attach.go, "Counting attachments") —
// read under the lock the fence went up under, so every attach that reserved
// before the fence is in it and none can reserve after; while the fence is
// up the count can only fall. release lowers this fence, and is idempotent; attaches are admitted
// again once every fence raised has been released. The stop sequence never
// releases its fence. It waits on nothing but the attachment lock, which is
// never held across anything that waits.
func (s *Server) FenceAttaches() (attached int, release func()) {
	s.lockAttachments(s.hooks.fenceWaits)
	s.fences++
	n := s.attachedCount()
	s.attachMu.Unlock()
	var once sync.Once
	return n, func() {
		once.Do(func() {
			s.attachMu.Lock()
			s.fences--
			s.attachMu.Unlock()
		})
	}
}

// FenceClose raises the host's whole close fence and reads what a detached
// host's idle exit is decided on (plan 030 §3.6, R2-1): (1) the attach fence
// (FenceAttaches), and the count of attachments read under it; then (2) the
// engine's own close fence (engine.Engine.FenceClose), and whether anything is
// in flight, read under it. With both up nothing can be admitted — no attach,
// no prompt, no setting, no queue edit, no turn of the session's own where it
// has an admission fence — so a caller that finds attached 0 and busy false
// may stop the session with both left up, and the decision cannot be
// overtaken. release lowers both, the engine's first: a client whose attach is
// answered after the release finds the engine admitting (an attached client
// never meets closing from the engine after its attach went through); one
// refused closing by either tries again. release is idempotent. With no
// engine served, busy is true: there is nothing to decide an idle exit on.
func (s *Server) FenceClose() (attached int, busy bool, release func()) {
	attached, releaseAttaches := s.FenceAttaches()
	if h := s.hooks.closeFenceStep; h != nil {
		h("attaches fenced")
	}
	releaseEngine := func() {}
	busy = true
	if eng, _ := s.engine(); eng != nil {
		releaseEngine, busy = eng.FenceClose()
	}
	if h := s.hooks.closeFenceStep; h != nil {
		h("engine fenced")
	}
	var once sync.Once
	return attached, busy, func() {
		once.Do(func() {
			releaseEngine()
			releaseAttaches()
		})
	}
}

// lockAttachments takes the attachment lock. waits is a test's hook
// (TestHooks.FenceWaits), nil in production: set, the lock is tried first, and
// waits runs just before a wait for a lock found held — where a test learns
// that the lock has excluded its caller.
func (s *Server) lockAttachments(waits func()) {
	if waits != nil {
		if s.attachMu.TryLock() {
			return
		}
		waits()
	}
	s.attachMu.Lock()
}

// sessionStop is session.stop on a server that serves it (dispatch refuses it
// stop_unsupported on any other): the first accepted stop's fence, receipt and
// hand-off, in that order (the package's "session.stop"); every later stop's
// receipt, {}, as an ordinary reply.
func (c *conn) sessionStop(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.StopParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	c.srv.connNote(c.id, map[string]any{"event": "stop", "clientId": b.client})
	first := false
	c.srv.stopOnce.Do(func() {
		first = true
		// Never released: a stop is one way. Up before the receipt is
		// queued, and so before it can be read.
		_, _ = c.srv.FenceAttaches()
		if h := c.srv.hooks.beforeReply; h != nil {
			h(req.method)
		}
		c.stopReceipt(req.id)
		// Only now does the coordinator hear of the stop: whatever its
		// sequence puts on this connection queues behind the receipt.
		c.srv.opts.Stop(StopRequest{Client: b.client, CommandID: p.CommandID})
	})
	if first {
		return replied()
	}
	return answer(protocol.Empty{})
}

// stopReceipt queues the first stop's receipt, {}, as the answer to id,
// outside the writer budget (outbox.admit): at once, whatever the outbox
// holds. It takes over the request's admission slot as every reply does:
// given back once the line is written, or at once when it is refused — the
// connection has closed, or its engine was replaced, and the receipt goes
// nowhere, as any handler's reply then does.
func (c *conn) stopReceipt(id json.RawMessage) {
	resp := protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: id}
	raw, err := rawJSON(protocol.Empty{})
	if err != nil {
		resp.Error = failed(err)
	} else {
		resp.Result = raw
	}
	line, _ := c.responseLine(resp)
	if line == nil || c.out.admit(line, c.release) != nil {
		c.release()
	}
}
