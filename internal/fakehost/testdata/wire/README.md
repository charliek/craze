# The wire fixtures (plan 027 §3.11, C9)

Each `*.ndjson` file is one scripted scenario against a fresh `fakehost.Host`
(`internal/fakehost`'s `TestWireFixtures`, in `../wire_test.go`). A line is one
JSON object, one of:

- `{"conn": N, "dir": "c2s", "msg": {...}}` — a request the runner sends
  verbatim on connection `N` (dialed lazily, the first time its number is
  named). `N` has no relation to the host's own ids (`c-1`, `s-1`, …); it is
  only how this file tells its connections apart.
- `{"conn": N, "dir": "s2c", "msg": {...}}` — a line the host must write to
  connection `N` next, checked byte for byte (after the incarnation
  placeholder substitution below). `msg` may be absent
  (`{"conn": N, "dir": "s2c"}`): a bare marker meaning "one line is expected
  here, content not yet recorded" — `-update` fills it in; a plain run
  refuses to guess and fails outright, telling you to run `-update` first.
- `{"dir": "op", "op": {"name": "...", ...}}` — not a wire message at all: a
  host-side script step (`fakehost.Host.Do`), run against the Host directly,
  never sent or received over any connection. It is how a fixture drives the
  Stub without an agent: emitting text, opening an ask, restarting the engine
  (a new incarnation), and so on. `cmd/craze-fake-host`'s stdin reads the same
  shape.
- A c2s line may also carry `"invalid": true` (fixture 10 only): it is
  deliberately not a well-formed request of a method protocol 1 defines with
  today's params (an unknown method, or a field no schema allows) — sent as it
  stands, and not held to the request schema, which such a line is designed
  never to pass. Every other line, c2s and s2c alike, is schema-checked
  (`internal/control/wiretest`) as it is sent or read.

## The incarnation placeholder

A Stub's event log mints its own random UUIDv7 incarnation — the one thing a
`Host` cannot pin deterministically (see `../doc.go`). So a fixture never
names a real incarnation: it writes `INCARNATION-1`, then `INCARNATION-2` after
the first `restart` op, and so on. The runner (`wire_test.go`) tracks the
Host's real incarnation at construction and after every `restart`, and does
the substitution both ways — real to placeholder in every line it reads off
the wire before comparing or recording it, placeholder to real in every c2s
line before sending it. Exact string replacement of values the Host itself
reported, never a guess at their shape.

## Determinism

Every other source of nondeterminism a real host would have is pinned by
`fakehost.Options`'s defaults: a fixed clock (2026-01-01T00:00:00Z, moved only
by an explicit `advance_clock` op), a fixed pseudo-random token source, a
fixed host id, craze version, pid, workspace and durable session id. Fixture
4 (a `slow_consumer` reset) and fixture 12 (an `omitted` reset) are made
deterministic from the host side alone — a small subscription budget the
client asks for (fixture 4: `budget.maxBytes`, small enough that a single
ordinary-sized push can never fit, so no race against a forwarder goroutine
decides the outcome) and a single event too large for any subscription to
carry (fixture 12: the `oversized_event` op) — never by racing a stalled
write against a fixed sleep. `stall_writes`/`resume_writes` exist as ops (see
`../host.go`) but neither of those two fixtures needed them once a
size-based trigger was found to be exactly reproducible under `-race`; an
earlier design that stalled the connection's writes and then raced a
duration-bounded burst of pushes against it was measured to be flaky under
`-race -count=20` and was replaced by the size-based design here.

## Re-recording

`go test ./internal/fakehost/... -run TestWireFixtures -update` re-records
every bare s2c marker (and re-validates every already-recorded line) from
each fixture's own c2s and op lines. The committed files here pass
`TestWireFixtures` without `-update`; run it `-race -count=20` to confirm a
fixture is not flaky before committing it.

## The fixtures

1. `01-hello-attach-snapshot` — hello, a fresh attach (a snapshot, no
   cursor), synchronized.
2. `02-resume-cursor-replay` — detach, two events published while detached,
   then an attach with a cursor: a replay of both, then synchronized.
3. `03-cursor-foreign-incarnation` — a `restart` (a new incarnation) closes
   the first connection; a new one's attach names the old incarnation's
   cursor: `reset: foreign_incarnation`, a fresh snapshot.
4. `04-slow-consumer-reattach` — an attach with a tiny `maxBytes` budget,
   then one push too large for it to ever hold: `reset{slow_consumer}` with
   nothing delivered, then a cursor re-attach replays it.
5. `05-ask-answered-twice` — a permission ask, answered, then answered again:
   `{}` then `already_resolved`.
6. `06-invalid-answer` — a permission answered with an option it never
   offered: `bad_request`/`bad_answer`, and `asks.get` shows it still open.
7. `07-cancel-idle` — `session.cancel` with nothing running:
   `not_accepting`.
8. `08-resend-commandid` — the same `commandId` sent twice with the same
   payload (replayed, same result) and then with a different one
   (`bad_request`).
9. `09-prompt-seen-by-both` — two connections attached to the same session;
   one sends `session.prompt`, and the other's subscription carries every
   event the turn produced.
10. `10-refusals` — a command before `hello` (`hello_required`), an unknown
    `sessionId` (`unknown_session`), an unknown method (`unknown_method`),
    and an unknown params field (`unknown_field`).
11. `11-snapshot-main-and-child` — `session.snapshot` of the main transcript
    and of a child, both windowed to a small byte budget, plus an unknown
    child id (`unknown_subagent`).
12. `12-omitted-reset` — a normal event, then one too large for any budget:
    `reset{omitted}`, then a fresh (cursorless) re-attach.
13. `13-hello-resume-tokens` — a resume with the right token (`resumed:
    true`), a token that names another client (`bad_token`), and a resume
    of a binding aged past the idle bound (`resumed: false`, a fresh id).
