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
// depguard: this package may import internal/tui (for the Stub) and
// internal/control (the server); it may not import internal/cli, internal/acp
// or internal/harness (.golangci.yml's fakehost rule).
package fakehost
