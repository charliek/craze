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
//  3. (C7) one forwarder per live subscription.
//  4. ONE WRITER (conn.write) draining a byte-counted FIFO (outbox) to the
//     socket. Every outbound byte counts against WriterQueueBytes (32 MiB),
//     ResetReserveBytes (1 KiB) of which only a final reset may use; a line
//     that fits an empty queue always gets in; every wait on the queue ends
//     when the connection closes. A write that makes no progress for the stall
//     bound (60 s) closes the connection; the session is untouched.
//
// A read-side EOF is half-close (astra 11): no more requests. Admitted
// requests complete and their replies are written, and the connection closes
// once nothing is in flight and no subscription is live (conn.idleLocked, the
// one predicate), when a write fails, or when the server closes it.
//
// # Client ids and the binding table (bind.go)
//
// hello mints a client id per connection and binds it with a resume token;
// a hello{resume} with the token takes the id back on a new connection (a
// TRANSFER) and supersedes the old one; a connection's close releases its
// client only if the binding still names that connection and its generation
// (compare-and-release), so a superseded connection's late cleanup releases
// nothing. See bind.go.
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
//   - conn.mu guards a connection's admission count and half-close state; the
//     outbox has its own mutex. Neither is held across a socket call, and
//     neither is held while taking any other lock.
//
// No lock is ever held across socket I/O or a blocking engine call.
//
// # Close
//
// Server.Close closes the listeners and every connection, joins every
// TRANSPORT goroutine (accept loops, readers, writers), and does not join the
// handlers: a command can be parked where nothing interrupts it (a SetTitle in
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
