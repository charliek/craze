// Package roster is the session list's data (plan 030 §3.9): every running
// session of this user on this machine and what each is doing, and the saved
// sessions of this CRAZE_HOME that are not running — the list the TUI shows
// on ← and /sessions (tui.Config.Sessions).
//
// There is no hub yet (S4b): the roster reads the registry (rundir.Hosts) and
// asks each host's own control socket for its row (sessions.list), once a
// second while the list is open, over a connection it keeps per host. A host
// with the row facts (capability rowFacts, §3.8) says what it is doing, what
// it last said and since when; an older one is listed with what S2's row
// has and its craze version.
//
// # The poll (R2-8)
//
// One goroutine, the poller, owns every host's state: each tick it reads the
// registry and the index and asks every host once — an attempt each, on a
// goroutine of its own — and it takes their results. An attempt is one budget
// — a new connection's dial and hello within dialBudget, then its
// sessions.list within listBudget, all by one deadline fixed as the attempt
// starts, which a kept connection's redial spends what is left of — and a
// host is in at most one attempt, with at most maxInFlight in flight whatever
// the number of hosts: as one ends, the next host the tick has not asked
// takes its place, the least recently asked first, and a host still inside
// its previous attempt when a tick comes is skipped. A failed attempt marks
// the host Unreachable — the registry lists it and its socket does not
// answer; never shown as saved — and backs it off, 1 s doubling to 30 s. The
// protocol client is the roster's own (client.go): it has no reader and never
// redials, so between polls a kept connection costs no goroutine, and
// reconnection and backoff are the roster's alone. A host gone from the
// registry has its connection closed.
//
// The poller hands the list a Snapshot through a latest-value slot of one
// (Updates): replaced, never waited on. Close cancels the attempts in flight
// — the roster's context closes their connections — takes their results,
// joins their goroutines, closes every connection and returns once every
// goroutine the roster started has finished.
//
// Each poll connection is a connection of the host's like any other: it says
// hello (client kind "tui", name "craze sessions"), never attaches — so it
// does not keep a host from its idle exit (plan 030 §3.6) — and leaves the
// S2 open and close notes in that session's journal, two per list opened.
package roster
