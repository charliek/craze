// Package transcript is the render-free model of one session's transcript
// (plan 024, session control S1c): the entries every client agrees on, the
// sub-agent roster and its child transcripts, the open asks, the turn, the
// settings, the queue and the todo list — folded from the session's event
// stream, one event at a time, by Fold.
//
// # Two instances, one library
//
// The engine keeps one Model, folded from the event log's observer inside the
// log's publishing boundary (plan 024 §3.1); it is the authority a snapshot is
// cut from. Every client folds its own from the events it receives. Both run
// this code, so a client that folded from the first event and one that
// attached from a snapshot hold the same entries under the same EntryIDs.
//
// Because the engine's instance runs inside the boundary, Fold honours the
// observer's contract (internal/agent/eventlog.go, Observe): it takes the
// model's own mutex and nothing else, never blocks, never calls the log or the
// session, runs no callback — no clock when Options.Clock is nil, no
// Options.ErrText when it is nil, never a foreign error's methods (the log
// hands its observer an *agent.RemoteError, whose text the fold reads as a
// field) — and allocates within the bounds §3.4 states. It is total:
// every EventType with every payload pointer nil is a no-op or a defined
// effect, never a panic.
//
// # Immutable entries
//
// An *Entry, once it is in a transcript, never changes: a chunk, a closed run
// and a tool update each build a new Entry and store its pointer in the slot,
// keeping the entry's ID. Payloads (*agent.ToolEvent, *agent.PlanEvent) are
// the event's own and are retained by pointer, never written (SD-19). So a
// reader — a snapshot's cut, a client's pane — that copied the pointer slice
// holds entries that will never change under it. The one piece of mutable
// storage is each transcript's stream builder, which no reader ever aliases:
// the open entry's text is read through Transcript.Tail, which copies.
//
// # What is not here
//
// Rows a client writes for a message of its own (a usage error, the
// optimistic user row at Enter, an ask's answer notes), how anything renders,
// and the basename-ambiguity map a tool row's label is drawn with (pathDirs)
// are the client's. The trim note is drawn by the client from Trimmed.
package transcript
