package control

import (
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
//  2. the request is handed to Options.Stop, the host's coordinator;
//  3. the receipt {} is queued.
//
// So the fence is up before the receipt can be read — a client that reads it
// and attaches, on any connection, is refused closing, never half-attached —
// and the coordinator has the stop before the receipt waits for room in an
// outbox a stalled peer may be holding full: a wedged connection never holds a
// stop back. The receipt still reaches its client before the connection closes
// for the session's end: the request holds its admission slot until its reply
// is written, and an ending connection closes only once nothing it admitted is
// unwritten (conn.end).
//
// Every later stop — the same client's, another's, the same commandId resent,
// one arriving while the first is still being handed over (stopOnce waits for
// it) — is answered {} and is otherwise nothing: it JOINS the first, whose
// sequence the coordinator runs once. A stop makes no engine call, so it is in
// no receipts table (a resend is answered {} again, never unknown_command),
// takes no host-wide command slot, and waits for no reply barrier: its receipt
// precedes, by design, every event the stop goes on to cause. Nor is it held
// back when its client has since moved on to another connection (conn.command's
// movedOn): the stop was that client's, and stopping twice is stopping once.
//
// # The close fence
//
// FenceAttaches is the primitive the coordinator's stop sequence starts with,
// and C5's idle watcher reuses, reversibly (§3.6, "The decision is atomic"):
// it goes up under the server's attachment lock (Server.attachMu), which an
// attach holds from its fence check to the install of its pending attachment
// (conn.reserve), and it reads the count of attachments the server holds in
// every state but closed — pending, live, closing — under the same lock. So
// no attach slips in between the fence and the count: one that checked before
// the fence is in the count, and every one after is refused. While the fence
// stays up the count can only fall. release lowers it; attaches are admitted
// again once no fence is up.

// StopFunc is the host's lifecycle coordinator as the server hands it a stop
// (Options.Stop; plan 030 §3.6a). The server calls it at most once in its
// life, for the first session.stop it accepts, on that request's handler
// goroutine, once the stop's own close fence is up and before the receipt is
// queued.
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
// attached is how many attachments the server holds that are not yet closed —
// pending (reserved, not yet answered), live or closing — read under the lock
// the fence went up under, so every attach that reserved before the fence is
// in it and none can reserve after; while the fence is up the count can only
// fall. release lowers this fence, and is idempotent; attaches are admitted
// again once every fence raised has been released. The stop sequence never
// releases its fence. It waits on nothing but the attachment lock, which is
// never held across anything that waits.
func (s *Server) FenceAttaches() (attached int, release func()) {
	if h := s.hooks.fencing; h != nil {
		h()
	}
	s.attachMu.Lock()
	s.fences++
	n := int(s.attached.Load())
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

// sessionStop is session.stop on a server that serves it (dispatch refuses it
// stop_unsupported on any other): the first accepted stop's fence and hand-off
// (the package's "session.stop"), and for every stop the receipt, {}.
func (c *conn) sessionStop(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.StopParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	c.srv.connNote(c.id, map[string]any{"event": "stop", "clientId": b.client})
	c.srv.stopOnce.Do(func() {
		// Never released: a stop is one way. Up before the coordinator
		// hears of the stop, and before the receipt is queued.
		_, _ = c.srv.FenceAttaches()
		c.srv.opts.Stop(StopRequest{Client: b.client, CommandID: p.CommandID})
	})
	return answer(protocol.Empty{})
}
