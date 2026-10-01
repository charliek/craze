// Package fakehost is the in-process twin of cmd/craze-fake-host (plan 027
// §3.11): the real internal/control server, wrapping the real engine
// (internal/engine) over a tui.Stub, with every source of nondeterminism the
// server's own Options exposes pinned — the clock, the token source, the
// host id, the craze version, the pid and the durable session id.
//
// Its control API (the Host's Go methods, and the same shapes as NDJSON ops
// on cmd/craze-fake-host's stdin) drives the Stub the way a real agent would:
// text, thoughts, tool updates, permissions, questions, plans, a turn's end, a
// foreign turn, plus the fixtures' own needs — a stalled write path, dropped
// connections, a restart (a new incarnation) and an oversized event (an
// omitted record). Two things it cannot pin: the Stub's log mints its own
// random UUIDv7 incarnation, and the CI environment's own timing is never
// asserted on. The wire fixtures under testdata/wire/ name the incarnation by
// placeholder (INCARNATION-1, INCARNATION-2 after a restart, …) instead —
// see wire_test.go's runner, which does the substitution both ways.
//
// By default the Host is an S2 host, which to a plan 030 client is an older
// one: it refuses session.stop, leaves the info document's permissionMode
// and startedAt out, and answers sessions.list with S2's row. Options turns
// each on (plan 030 §3.6a, §3.7, §3.8; X1): Stop serves session.stop through
// a coordinator whose sequence the run_stop op runs, RowFacts puts the row
// facts on the row, and a fixture asks for them in its first line ({"dir":
// "host", …}). The same line's models (Options.Models, plan 031 §3.6) gives
// the Stub the catalog a native session advertises — remembered models with
// their rank, the catalogs' "recent" — in place of its own; the Stub's
// provider stays its own, since only the catalog is on trial.
//
// A Host is served on a listener its caller makes, listed nowhere, unless it
// is registered (Register, plan 032 §3.15): then it is bound and listed in a
// registry exactly as a craze host is — its socket in the runtime tree, its
// entry and lifetime lock in the cache tree — so a hub finds it, and several
// Hosts with ids of their own (Options.HostID, CrazeSessionID) are listed
// side by side; the unlist op takes it out of the registry again (Unlist).
// cmd/craze-fake-host's --registry, --host-id and --session-id are the same,
// and the wire fixtures' runner registers a two-socket fixture's Host for the
// hub — the real internal/hub, in process — it runs in front of it
// (wire_test.go, hub_fixture_test.go).
//
// For the hub's session.create (plan 032 §3.10) a Host can stand in for the
// craze serve a spawner starts (RunSpawned, over craze serve's command line,
// ParseSpawnArgs): its ready line on the spawner's pipe, its entry carrying
// the create's request (Options.RequestID, RequestHash), its start held or
// failed as its test says (Options.Start), and its end on session.stop
// (StopHeard) or a signal.
//
// depguard: this package may import internal/tui (for the Stub),
// internal/control (the server) and internal/rundir (Register); it may not
// import internal/cli, internal/acp or internal/harness (.golangci.yml's
// fakehost rule).
package fakehost
