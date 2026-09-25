// Package control is the control socket's server (plan 027 §3.7): protocol 1
// (internal/protocol) over any net.Listener, serving the one session a host
// runs — an *engine.Engine, which it drives through engine.Control and a few
// reads of its own. The name is engine.Control's, the surface it serves;
// internal/host is taken.
//
// # A connection's roles
//
// Each accepted connection has:
//
//  1. ONE READER (conn.read). It reads lines with protocol's framing (a 4 MiB
//     inbound limit; an over-long line is answered -32600 line_too_long with
//     id null and the connection goes on), answers what the envelope alone
//     decides, runs hello itself, and hands every other request to a handler.
//     It takes an admission slot BEFORE it reads a line and a request keeps
//     that slot until its reply has been WRITTEN to the socket, so a reader
//     stops reading while RequestsPerConnection (16) requests are admitted and
//     unwritten: a peer that sends and never reads stalls at 16.
//  2. ONE HANDLER goroutine per admitted request (conn.handle), never on any
//     primary's reader — the server holds no primary (SF-14). A command runs
//     on a server-owned context (never the connection's), then the reply
//     barrier, then its reply is queued.
//  3. ONE FORWARDER per live attachment (forward.go), started once the
//     attach reply is queued: it turns the subscription's records into event
//     notifications (Record.Body verbatim), synchronized at the cutoff, the
//     one ready an attachment made before readiness is owed, and — when the
//     subscription ends — its final records and reset. An attachment is
//     pending, live, closing, then closed (attach.go); a connection holds at
//     most one that is not closed.
//  4. ONE WRITER (conn.write) draining a byte-counted FIFO (outbox) to the
//     socket. Every outbound byte counts against WriterQueueBytes (32 MiB),
//     ResetReserveBytes (1 KiB) of which only a final reset may use; a line
//     that fits an empty queue always gets in; every wait on the queue ends
//     when the connection closes. A writer that moves no byte for the stall
//     bound (60 s, from the last byte that moved) closes the connection; the
//     session is untouched.
//
// A read-side EOF is half-close (astra 11): no more requests. Admitted
// requests complete and their replies are written, a live subscription keeps
// delivering, and the connection closes once nothing is in flight and no
// subscription is live (conn.idleLocked, the one predicate), when a write
// fails, or when the server closes it. The session's end is the same rule with
// the session in the peer's place (conn.end): once the engine has closed
// (Server.watchEngine), or an attachment has delivered its final records and
// reset{session_closed}, the connection admits nothing more and closes once
// what it owes is written. Replacing the engine (Server.SetEngine) is not: an
// attached connection is sent reset{session_replaced} — whatever reset its
// forwarder was about to send; a detach's reply instead when that detach had
// already claimed the attachment's end — and closed as soon as that is
// written, every other one at once, and a handler still running then replies
// to nobody: the replacement's reset seals the outbox, which admits no line
// after it (conn.replace).
//
// # Client ids and the binding table (bind.go)
//
// hello mints a client id per connection and binds it with a resume token;
// a hello{resume} with the token takes the id back on a new connection (a
// TRANSFER) and supersedes the old one; a connection's close releases its
// client only if the binding still names that connection and its generation
// (compare-and-release), so a superseded connection's late cleanup releases
// nothing. A superseded or closing connection admits nothing more: its reader
// stops. A command admitted before a transfer re-checks its binding just
// before the engine call and does not run once the binding has moved on (a
// newer generation, a binding dropped since — X15 — or another engine); one
// whose connection merely closed, its binding still in the table, still runs
// (§3.6). A binding with no connection for twice the receipts table's age
// bound (20 min) is dropped with its token, so neither grows with clients that
// never come back. See bind.go.
//
// # Locks
//
//   - Server.bindMu guards the engine pointer, the binding table and every
//     conn's bound state. It is a LEAF above the receipts table's own leaf:
//     under it the server calls only the receipts table's leaf methods
//     (NewClientID, ClaimClient, ReleaseClient), never I/O, a handler, or any
//     engine call that can block. bindMu → receipts.mu is the one edge.
//   - Server.connMu guards the connection and listener sets and the closed
//     flag; it is never held with bindMu, across I/O, or across a close.
//   - conn.mu guards a connection's admission count, half-close and end
//     state, and its attachment's lifecycle and position; the outbox has its
//     own mutex. conn.mu → outbox.mu is the one edge between them: a line
//     that makes a lifecycle step visible — an attach reply, a detach reply,
//     a final reset, an event and the position it moves — is offered to the
//     outbox under conn.mu, in the same section as its step (conn.enqueue), so
//     no reader of that state is ever behind the wire (astra r8). An offer
//     never waits: room is waited for with neither lock held. Neither is held
//     across a socket call, the outbox's never while taking another lock, and
//     conn.mu while taking none but the outbox's (a subscription is closed
//     outside it).
//
// No lock is ever held across socket I/O or a blocking engine call.
//
// # Close
//
// Server.Close closes the listeners and every connection — and with each its
// subscription — joins every TRANSPORT goroutine (accept loops, readers,
// writers, forwarders, ready watchers, the engine's watcher), and does not
// join the handlers: a command can be parked where nothing interrupts it (a SetTitle in
// the index flock), so handlers are counted and Close returns at its ctx's
// deadline with the count still running. Such a command still completes into
// the engine's receipts table; its reply goes nowhere.
//
// # Logging
//
// Each connection's open and close, and why it ended, is a journal diag note
// (journal.DiagControlConn) through Control.Note, and a line to Options.Log.
// Nothing a client sends is logged, and never a resume token.
package control
