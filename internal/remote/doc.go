// Package remote is the control socket's Go client (plan 027 §3.14, PR 1's
// core): protocol 1 (internal/protocol) over a Unix socket, to one craze host
// — directly, or through a hub's splice (§3.3). PR 4 builds remote.Session, the
// TUI's backend over the wire, on it; this package decodes nothing it does
// not need to route, and folds nothing.
//
// # What a Client does
//
//   - Dial connects, runs the hub hop when Options.Connect names a session
//     (hello to the hub — whose hub-kind answer is accepted only here, X5 —
//     and session.connect), and says hello to the host with protocols [1],
//     resuming Options.Resume's client id with its token when one is given. It
//     records the client id, the token, resumed, the retry horizon, the
//     endpoint, the capabilities, the codecs and the limits (Hello).
//   - Call makes a call that is not a command (a read, a detach), with a
//     request id, on the connection's one reader, which demultiplexes replies
//     by id and hands notifications to the stream. Inbound is tolerant:
//     unknown fields, unknown notification methods and unknown event kinds
//     (the stream never decodes an event) are ignored, never an error; a line
//     that breaks the protocol (not a message, a reply with neither result nor
//     error, params that do not decode) drops the connection. A line may be up
//     to 16 MiB, the longest a host writes, and a longer one drops the
//     connection wherever it comes, hello included; a request longer than the
//     host's inbound limit is never sent.
//   - Command sends a mutating method with a commandId the client mints —
//     per client from 1, never reused — and remembers it, its method and its
//     params until its answer is handed over; one attempt of it is on the
//     wire at a time (command.go). Retry by code
//     (CommandOptions.Retry) resends the SAME id on unavailable,
//     not_accepting, in_progress and stale_model, and never on any other code
//     (protocol.Retry). A refusal is an *Error: the host's code, reason,
//     message, result and cause, as a plain struct (PR 4 maps them to the
//     sentinels).
//   - Attach puts the client on its session's stream, which folds nothing
//     and turns each reset into a re-attach by §3.4's table (attach.go).
//   - When the connection is lost it redials, resumes, re-attaches with its
//     cursor, and resends its commands, in wire order — ONLY IF the host
//     answered resumed: true for the same client, token and host
//     (reconnect.go, command.go's resend rule): after a resume loss nothing
//     that may have run is resent, and it resolves ErrOutcomeUnknown, reason
//     resume_lost. A reconnect episode is bounded in attempts and time; once
//     it is spent the client stops, reason disconnected.
//   - ResumeState is what a caller persists so a new process can Dial from it
//     and Attach from its cursor (A4).
//
// resume_lost and disconnected are protocol's client-side reasons: a client
// names these outcomes itself, and no host ever sends them.
//
// # Goroutines and locks
//
// A connection has one reader goroutine (Client.read); the stream's work —
// its notifications and its own attach replies — runs on it, in the order the
// host wrote them, and it never waits for the stream's caller: an item that
// finds no room in the stream's queue makes the stream fall behind
// (attach.go). Nor does it ever write: a re-attach or a detach it decides on
// is posted to the connection's writer goroutine, which runs them in order
// (Client.post) — as is Stream.Close's own detach, so a Close waits for
// nothing but answers, bounded by its context. So replies, resets and the
// connection's end are always read, even while a caller's write holds the
// connection's write lock on a full socket. A lost connection starts one
// reconnect goroutine, which opens the
// next connection, re-attaches on it before its reader starts, sends every
// command held, in wire order, and only then hands it over — every write of
// it bounded by the episode, even one made while the episode's clock is
// stopped for the re-attach's reply, and the connection it adopts closed by
// Close (reconnect.go). Client.mu guards the client's state (the connection,
// the hello, the commands); Stream.mu the stream's, and the stream's queue has
// its own lock, taken under Stream.mu and never the other way; Client.mu and
// Stream.mu are never held together, nor any lock across I/O but a
// connection's write lock. Under the write lock, as a write begins and ends,
// only short sections run that wait on nothing: a command's wire-order key
// (its own leaf lock), a stream's admission of its attach (Stream.mu), and the
// reconnect episode's clock (its own lock); none of those locks is held while
// a write lock is taken. Close joins every goroutine the client started.
//
// # Import boundary
//
// A non-test file here imports internal/protocol and the standard library
// alone — never internal/control (the client must not import the server),
// internal/tui, internal/cli, internal/acp or internal/harness (the remote
// rule, .golangci.yml). Its tests run the real server (internal/control) in
// front of an engine over tui.Stub, on real Unix sockets (remote-test).
package remote
