# 07 — Roadmap

Each phase is one panel-reviewed plan (outside the repo) and one to five
PRs, gated per commit. S1–S5 are committed; S6 and S7 are directional and get
decided after S1–S5 are in daily use (SD-12). Sizes are rough estimates of
non-test source lines, from the reference reviews (`09`).

| order | ID | status | one line |
|---|---|---|---|
| done | S0 | complete | Discovery: four codebases reviewed, topology / protocol / remote scope / journaling settled |
| done | S1 | complete | Engine core: fan-out, sequenced journal, engine-owned asks and turn state, render-free transcript; TUI becomes the first client. **S1a complete (2026-09-19)**; **S1b complete (Plan 021, all three PRs merged, 2026-09-21)**; **S1c complete (Plan 024, PRs #50 and #52, 2026-09-24)** |
| done | S2 | complete (Plan 027, 4 PRs: #55, #56, #61, #63) | Per-session Unix socket, published protocol spec + schema, fake host, `craze bridge`, `craze attach` |
| done | S4a | complete (Plan 030, with S5; 5 PRs: #66, #68, #70, #71, PR 4) | Detached hosts: `craze serve`, hosts born detached, `session.stop`, idle exit |
| done | S5 | complete (Plan 030, with S4a) | Agent view in the TUI: a session list of every running session on the machine, new sessions started from it, composer `@` mentions |
| 1 | S4b | not started (next) | The hub: `craze ps`, `hub.sock`, `sessions.subscribe` on the hub, `session.create`, remote-machine aggregation |
| 2 | S3 | not started (after S4b, and after shed's first release of its own lane work) | `shed-craze` lane adapter in shed; craze in shed-mobile's `LANE_KINDS` |
| 3 | S6 | directional | `craze web`: hub serves WebSocket + a web bundle on loopback / tailnet |
| 3 | S7 | directional | Outbound relay uplink and a hosted server; enrollment, scopes, TLS |

The order column is the order phases run in (SD-34); phase IDs are stable
names, not positions. S4a and S5 were built together in Plan 030. S4b runs
before S3 so that the shed lane starts with a hub roster and `create`.

## Phase detail

### S0 — discovery

**Exit result:** completed 2026-09-19. Findings in `01`–`06`, decisions
SD-01 to SD-17, open questions SQ1–SQ12, harness overlap in `11`.

### S1 — engine core

Detail in `03` and `04`. Three slices, each its own plan and PR:

- **S1a** (complete) — the ordering boundary and `Subscribe` (primary and
  budgeted subscribers) as one component shared by the ACP session, the native
  adapter, and `tui.Stub`; sequence numbers; incarnation identity; the
  lossless event codec; the ring; the journal writer; `seq` on
  `craze prompt --json`. No behavior change.
- **S1b** (complete) — the engine turn driver (admission, queue drain,
  send-now, foreign turn wait, synthetic endings and rows); the
  shared/client-local split of TUI-authored rows and state deltas in place of
  bare `EventMeta`; ask registry with sequenced terminal outcomes; cancel
  outcomes on an engine turn id; command ids; settings order; approval policy
  separated from frontend presence; durable craze session id and index writes
  in the engine. **Shipped under Plan 021 (panel-reviewed 2026-09-20): three
  PRs — the driver, the asks, state and identity.**
- **S1c** (complete) — the render-free `internal/transcript` model folded
  inside the boundary (the engine's own instance, plus one per client); a
  bounded, lossless snapshot codec; in-process attach (`Engine.Attach` on
  `Control`) with snapshot + cursor and three pinned error paths; a
  convergence check per event kind. The TUI became its first client with no
  golden moved except four permitted one-row changes: SF-01's two title rows
  (X38) and X39's two spinner glyphs. **Shipped under Plan 024: two PRs —
  the package/engine/attach (#50, `27c1db6`), then the TUI (#52, merged
  2026-09-24).**

- Size: L, about 4–5k lines plus test churn (raised after the panel: S1b is a
  driver, not a field move). S1b and S1c are the risk.
- Ordering against the harness: does not block H2; S1b should precede the
  first harness phase that adds an ask channel, and H6 and H7 (`11`). Harness
  D-39, on `main` since 2026-09-19, moved permission prompts out of H3, so
  that is no longer the first such phase.
- **Exit (S1a)**: goldens and `craze prompt --json` output unchanged apart
  from `seq`; a budgeted subscriber attached mid-turn with a cursor receives
  exactly the events the primary did after it, in order, across the
  ring/journal seam; a stalled budgeted subscriber is dropped without delaying
  the primary; every session, ACP and native, leaves a journal whose `event`
  lines decode losslessly to the emitted events; a failing or stalled disk
  degrades to a recorded gap, never a stalled turn; race detector clean.
- **Exit (S1b)**: two in-process clients on one session cannot double-drain
  the queue; a session with no client drains its own queue and parks asks; an
  ask answered twice yields one resolution and one `already_resolved`, and an
  invalid answer leaves it open; every ask ending is a sequenced event; a
  cancel names its turn and cannot hit the next one; two concurrent settings
  changes converge on every client; golden files byte-identical; and, because
  `craze prompt`'s own driver moves into the engine, its `--json` fixtures and
  tests are byte-identical apart from `seq`.
- **Exit (S1c)**: golden files byte-identical, with four permitted one-row
  exceptions (SF-01's two title rows, X38; X39's two spinner glyphs), and
  `transcript_test.go` assertions move packages unchanged; a second
  in-process subscriber attached mid-turn from a snapshot reproduces the
  first's transcript exactly; snapshot and replay memory stay inside stated
  byte bounds on a worst-case session.

**Exit result (S1a):** completed 2026-09-19, plan `020-session-control-s1a-event-log`,
branch `feature/plan-020-session-control-s1a` (the PR number goes here on
merge). All six exit clauses met,
each against a named test or the Linux smoke; the criterion-by-criterion table,
the amendments X1–X18, the Linux smoke record, and the V3/V4 measurements are in
`12`. Two qualifications, both recorded there rather than waved through: the
live smoke could not exercise a **native tool call** (H1 has no tools; re-run
after H2 lands), and V6's "`config.toml` byte-identical" clause **fails for a
pre-existing reason** — `persistProvider` writes the `provider` key on every
run, unchanged on `origin/main` — so it is restated as "unchanged apart from
the `provider` key". The mac-mini half ran at the branch tip and passed for
grok and native; `cursor-agent` is skipped there because the login keychain
blocks it over ssh, and that skip exercised the stderr tee against a real
agent. Inputs to SQ3 (retention) and SQ15 (tool-event conflation) are
measured in `12` and left undecided.

**Exit result (S1b):** shipped 2026-09-21, plan `021-session-control-s1b-engine`,
three PRs from fresh `origin/main` — `feature/plan-021-s1b-driver` (#41,
`fdaaa6d`), `feature/plan-021-s1b-asks` (#42, `db7686e`),
`feature/plan-021-s1b-state` (#46). All seven exit clauses met, each
against a named test; the criterion-by-criterion table, the execution
amendments X1–X59, the live smoke record, and V2/V3/V5/V6/V7 are in `12`; what
it found and did not do is the backlog in `13`. No
golden moved and no `testdata/` file changed except additions;
`craze prompt --json` is byte-identical apart from `seq` (V2: 103/103 SAME at
the tip). One defect was found live and fixed before the tip: quitting while a
turn ran left it `started` and never `ended`; `Close` now authors that turn's
ending itself before the session closes (`12`, "Deviations", item 11). The
permission path is still without live coverage on either platform — no agent
this smoke could drive ever asks one, on Linux or the mac-mini — recorded as
open, not failing, because nothing in S1b caused it.

**Exit result (S1c):** shipped 2026-09-24, plan
`024-session-control-s1c-transcript-model`, two sequential PRs from fresh
`origin/main` — `feature/plan-024-s1c-model` (#50, `27c1db6`),
`feature/plan-024-s1c-tui` (#52, merged 2026-09-24). All three exit clauses
met, each against a named test; the criterion-by-criterion table, the
execution amendments X1–X39, the live smoke record, and the measurements are
in `12`; what it found and did not do is the backlog in `13`. No golden
moved except four permitted one-row changes: SF-01's owner decision
(`native-echo-80x24` row 20, `native-mode-100x30`'s separator row, X38) and a
pre-existing macOS-CI flake fixed test-side (`grok-subagent-rows-{80x24,100x30}`'s
frozen spinner glyph, `✴` → `✳`, X39); `transcript_test.go`'s mixed
assertions moved packages under their own names with no behaviour change.
Live smoke ran on Linux (cursor, grok, native) and the mac-mini (grok,
native; cursor skipped there, the login keychain over ssh, as every earlier
phase found too), plus the hidden attach probe on every reachable provider,
SAME throughout (PR 1's Linux probe, PR 2's Linux and mac-mini probes). SQ9
is resolved (`10`); SQ15 stays open, measured but not decided. SF-01 and
SF-03 shipped here; SF-04 stays out; SF-02 is re-pointed at S2, which also
inherits `Engine.Attach`, `SubscribeOptions.Ctx`, the snapshot codec and its
version, and the receipt-mode gap (`12`, "Handoff").

### S2 — control socket

Detail in `05`. The protocol package, schema, socket server in the host,
`craze bridge`, a minimal `craze attach` (a second TUI on a running session,
which is also the best test client), the fake host, published reference docs.

- Size: M, about 3k lines.
- Owed by S1b and taken here (`13`, SF-10 to SF-19): a connection-local barrier
  so a reply is ordered after its events (`Control.Sync` returns no sequence
  number); a client id minted per connection, bound to it and released on
  disconnect; the gate table's remaining rows and the retry policy by code in
  the schema; index writes kept off connection goroutines; the TUI's engine
  calls moved onto `tea.Cmd`s; how a remote client words another client's
  settings change.
- Settled in S2 even though they pay off later (`02`, `05`): the runtime
  namespace (short, absolute, validated, lifetime locks, identity-checked
  unlink, peer checks both ways); transport close vs view close vs
  `session.stop`; connection-level vs session-level capabilities,
  subscription ids, the roster's own epoch; one attached session per
  connection (SQ14); bounded history pages; what `craze bridge` may assume
  under an SSH exec; what happens when `--continue` starts a second host for
  one session (SQ16). **SQ12 is decided (SD-33): hosts are born detached from
  S4 on and the TUI is a socket client for good**, so the socket-backed
  `Session` is the TUI's permanent path and S2's exit adds "the full TUI runs
  unchanged over it, goldens included".
- **Exit**: two TUIs on one live `cursor-agent` and one `grok` session show
  the same transcript; a prompt from either appears in both; an ask answered
  in one closes in the other; kill and reattach resumes silently from
  `afterSeq`; a deliberately stalled client is reset as `slow_consumer`
  without delaying the agent; socket refused for another uid; live smoke on
  Linux and the mac-mini.

**Exit result (S2):** complete, plan `027-session-control-s2-socket`, four
sequential PRs from fresh `origin/main` — `feature/plan-027-s2-wire` (#55,
`318fc76`), `feature/plan-027-s2-host` (#56, `2acd54a`),
`feature/plan-027-s2-tui-async` (#61, `3eabb31`), `feature/plan-027-s2-attach`
(#63, `73ed5e0`, merged 2026-09-28). Every
exit clause met, each against a named test or live leg (the plan's §7
acceptance table, A1–A25; `12`'s "S2 — as shipped" has the clause-by-clause
proof): two TUIs on one live session show the same transcript
(`TestSocketGoldensMatchTheEngine`, `TestAttachMidTurnOverTheSocketReproducesTheFirst`,
V4/V6); a prompt from either appears in both (V4/V6); an ask answered in one
closes in the other (V4/V6); kill and reattach resumes silently from
`afterSeq` — the silent cursor resume itself is `TestAKilledConnectionResumesSilently`
and `TestAClientProcessRestartResumesFromItsCursor` (`internal/remote`) plus
PR 2's V3 live legs (the bridge killed and resumed by its token and cursor;
the smoke client itself `kill -9`-ed and restarted from its persisted cursor
file); `craze attach` has no persisted cursor of its own, so V4/V6 leg 7 is a
different case, a snapshot attach after the kill, not a cursor resume; a stalled client
is reset `slow_consumer` without delaying the agent
(`TestAStalledClientIsResetWithoutDelayingTheAgent`); the socket refuses
another uid (`TestAnotherUIDIsRefusedBeforeAByteIsRead`, V5); live smoke ran
on Linux (cursor, grok) and the mac-mini (grok, native; cursor skipped there,
the login keychain over ssh, as every earlier phase found too); and SD-33's
addition — the full TUI runs unchanged over the socket, goldens included — is
`golden_manifest_test.go`'s 119-file manifest (113 goldens under both
transports, six picker frames in process only,
`internal/tui/golden_manifest_test.go:460-461`), enforced through two fix
rounds (astra r69, r71). Every frame `RunFrameScript` produces — a golden
matrix's (`runFrameModes`) or a direct call's — is logged in order
(`frameProductions`, `:192-229`); `assertGolden` judges the frame it holds by
the **most recent production of those exact bytes in one global log**, across
the whole test binary, not scoped to the asserting test itself
(`judgeFrame`, `:243-264`, called through `checkGoldenTransports` at
`internal/tui/frame_test.go:426`; the scan stops at the first byte match,
whichever test produced it, which is also `13`'s SF-67 (a)): an unspent
matrix credit belonging to the asserting test is spent, while a direct run,
an already-spent credit, another test's credit, or no production at all is
in process alone — so an identical-looking frame a later direct call
produces can steal an earlier matrix run's unspent credit
(`TestATransportCreditIsTheFramesOwn`, `:475-517`, pins both directions).
`TestMain`'s `goldenCoverage` (`:355-403`) fails an unfiltered run — no
`-run`/`-skip`/`-list`, `-update` off, and `-count` greater than 0 — if any
manifest golden, except `native-tools-80x24` when `ripgrep` is missing (the
same rule its own test skips by), was not asserted under every transport it
lists, **in every one
of the run's `-count` iterations** (coverage is counted per test-and-golden
pair, per iteration, `:318-339`). `make test`/`test-race` and CI's `test` job
pin `CRAZE_GOLDEN_TRANSPORT=both` (Makefile, `.github/workflows/ci.yml`); a
local run narrows the matrix only by running `go test` itself. A socket
run's verdict also checks that no reset escaped it: a final `session.sync`
barrier on the model's own connection, and the view close's `session.detach`
answered, both read through the tap (`internal/tui/frame_socket_test.go:335-409`).
No golden file's bytes moved in PR 3 or PR 4 (`git diff --stat -- '*testdata*'`
empty at every commit through `adac650`). The execution amendments
(X1–X58), C29a through C29d's fix rounds, the review record, and the live
smoke's findings and
backlog are in `12` and `13`. **S4a + S5 is next (Plan 030; see SD-34), then
S4b, then S3.**

### S3 — shed lane

Detail in `06`. Work is in the shed and shed-mobile repos and follows their
process ("mobile first").

- **Order (SD-34):** S3 runs after S4b, and after shed has made its first
  release of its own lane work. It starts with a hub roster and `create`,
  both available from S4b.
- Size: M, about 3k lines of Rust plus the lane-transport change in
  `shed-core`.
- **Exit**: shed's own bar. craze started in a roost tab; from the Flutter
  desktop build and the phone: read the transcript, send a prompt, cancel,
  answer a permission and a question; background the phone for a minute and
  resume with no reseed.

### S4a — detached hosts

Split from the original S4 by SD-34 and built with S5 in Plan 030 (outside
the repo). `craze serve` is the headless host, a real subcommand; the
ordinary `craze` spawns it detached (`setsid`, stdio to `/dev/null`, a ready
handshake on an inherited pipe), then runs the TUI as its client. Hosts are
born detached (SD-33), so by default every TUI is a socket client (the
`detach = false` opt-out below keeps the in-process path).

- `session.stop` behind a `stop` capability. `/exit` (with `ctrl+d` and the
  second `ctrl+c`) ends the session in every client, including `craze
  attach`; closing the terminal leaves it running (SD-35).
- The idle exit: a host with no client attached and nothing in flight exits
  after an hour by default, configurable (`host_idle_exit`). The decision is
  atomic against attaches and new work.
- The `detach = false` opt-out (and `CRAZE_DETACH=0`) keeps today's
  in-process path.
- **Detach is not a key binding (SD-33).** The TUI process is a job of the
  terminal's shell; Go cannot fork without exec and a process-group leader
  cannot `setsid`; closing a roost or tmux tab, or a systemd login scope with
  `KillUserProcesses=yes`, kills it whatever it ignores. The robust shape,
  decided as SD-33, is **hosts born detached, with the TUI always a socket
  client**; what a host does when its last client leaves (stop, or keep
  running) is then policy (SD-35). Spawning from a bridge must fully detach
  (`setsid`, stdio to `/dev/null`) or the SSH channel never closes. A
  headless host parks asks with zero clients (`03`), and on macOS cannot
  start `cursor-agent` from a plain SSH exec (locked login keychain).
- Size: an estimate of about 2k lines. prox's lifecycle lesson applies here
  first: every hard bug in this area is a **lifecycle** bug (leaked
  registrations, late close callbacks racing a reconnect, shutdown order).
  Write generation-guarded registration and "cancel workers, join, then
  deregister" teardown on day one.
- Reuse from prox: auto-spawn by re-exec with an env marker. (The flock'd
  singleton, health poll and stale sweep belong to S4b's hub.)
- **Exit**: the lifetime half of S5's exit below. After a prompt, killing the
  terminal leaves the host listed in the registry and `craze -c` reattaches
  with the transcript; `/exit` ends the session and the host's registry entry
  is gone within seconds; an unattended idle host exits after its timeout,
  and one with a turn, an open ask, or an attached client does not.

**Exit result (S4a):** complete 2026-09-30, plan
`030-session-control-s5-agent-view`, built with S5 in five sequential PRs
from fresh `origin/main` — `docs/plan-030-roadmap` (#66, `77f1cd3`),
`feature/plan-030-hosts` (#68, `6be2273`, which is S4a itself),
`feature/plan-030-sessions-list` (#70, `b8aa5bb`),
`feature/plan-030-new-sessions` (#71, `b401fec`) and PR 4
(`feature/plan-030-composer-at`). Every exit clause met, each against a named
test and the live smoke (V4 on Linux, V5 on the mac-mini; the legs are in
`12`): after a prompt, killing the terminal left the host running, listed and
answering `hello` — `tmux kill-session` of three sessions (V4 leg 2), quitting
cosmic-term (V4 leg 3), and an ssh logout on macOS, after which a new login's
`craze -c` reattached to that same host with its transcript and spawned
nothing (V5 leg 10; `test_hangup_leaves_the_host_and_dash_c_reattaches`,
`tests/cli/test_detach.py`); `/exit` ended the session with the client, the
host and its registry entry all gone at the first 0.1 s poll (V4 leg 5; on the
mac the host and its entry within 0.01 s, the client at 0.15 s), the index row
kept; with `host_idle_exit = "2s"` an unattended host exited 2.74 s after its
last client left (V4 leg 9; V5 2.76 s), and a never-prompted one at its
5-minute cap (V4 leg 3: 5 m 0.8 s), while a running turn, an open ask, a
replay, a queued prompt or an attached client keeps a host and the list's
polling does not (`TestEachInFlightConditionKeepsTheHost`,
`TestAnAttachArrivingAtExpiryKeepsTheHost`, `TestAListPollerDoesNotKeepAHost`,
`internal/cli/idle_test.go`). Plan 030 §9's systemd risk did not occur on the
two terminals tried: hosts started in tmux sit in tmux's scopes and survive
`kill-session`, and cosmic-term's app scope was not torn down when the
application quit — it stayed until its host's idle exit, then was collected
— so the `systemd-run --user --scope` spawn was not built (`13`, SF-79, kept
open for a terminal that stops its scope on exit). Detaching costs +7 ms p50
and +18 ms p95 to the first answer (V3, against a 150/300 ms budget). S4a's
execution amendments are PR 1's, X1–X62; they, the review record and the
live smoke are in `12`, and what it left is in `13`.

### S4b — the hub

The per-machine hub at `run/hub.sock` with roster, routing, spawn, and stop;
`craze ps`; `sessions.subscribe` on the hub; `session.create` turns shed's
`create` capability on; remote-machine aggregation. The hub replaces the
per-host polling that S5's session list does until it exists. Not started;
next now that S4a + S5 are complete, and before S3.

- Size: M, about 2k lines (the old S4's estimate, which covered the hub, the
  detached host and `craze ps` together; S4a took the host part).
- Reuse from prox: flock'd pidfile singleton, health poll, exit-when-idle,
  stale sweep keyed on pid plus a start token; the auto-spawn by re-exec with
  an env marker is already S4a's.
- **Exit**: start two sessions, close every terminal, list them with
  `craze ps`, attach to one; kill the hub mid-turn and lose nothing, hosts
  re-register with the respawned hub; a host crash removes its row within a
  sweep; hub and host of different craze versions interoperate on the
  protocol integer.

### S5 — agent view

Owner design (Plan 030, 2026-09-28), following Claude Code's agent view. A
whole-screen session list opened with `←` on an empty composer (and
`/sessions`): every running session of this user on this machine, whatever
its directory, one line per row, grouped by state (needs you, working,
failed, idle, then saved) with `ctrl+s` toggling grouping by directory.
`enter` opens a session in place. New sessions start from an always-focused
input on the list, with `@` choosing the directory (recent directories where
sessions ran, plus a typed path completed with `Tab`; nothing is hard-coded);
the selected row sets the directory; `/provider` and `/model` in that input
set what new sessions use. Composer `@` file and directory mentions in the
session composer ship with it.

- **Not in the MVP:** a preview of the selected session; answering an ask
  from the list (you open the session to answer); notices of other sessions
  inside a session; git worktrees. Each is a row in `13`.
- **Data:** the list polls each host's socket through the registry; there is
  no hub until S4b, which replaces the polling.
- Built on S4a: Plan 030, five PRs (roadmap docs, detached hosts, the list,
  new sessions, composer `@`). The existing sub-agent view is the UI
  precedent.
- Size: an estimate of about 3k lines.
- **Exit**: close every terminal, reopen craze, and `←` lists the sessions
  still running; one blocked on an ask is opened and answered; a new session
  is started in another directory from the list with `@`; `/exit` ends a
  session and the list shows it saved; an unattended idle session exits after
  the timeout.

**Exit result (S5):** complete 2026-09-30, the same plan and PRs as S4a —
the list in #70, new sessions from it in #71, composer `@` in PR 4
(`feature/plan-030-composer-at`). Every exit clause met live on Linux
(cursor, grok, native) and the mac-mini (grok, native; cursor skipped there,
the login keychain over ssh, as every earlier phase found too): with every
terminal closed, a new craze's `←` listed the sessions still running and
`enter` opened each in place with its transcript (V4 and V5 leg 2); a session
blocked on a question was listed under "needs you" (`question: Which colour
do you prefer?`), opened and answered (V4 and V5 leg 4; on Linux the answer
also reached a second client attached to it); a prompt behind a leading
`@proj-b` started a session in the other directory in the background
(`started in …` after 3.1 s for cursor, under 0.3 s on the mac), and
`@proj-b` alone opened an unstarted session that spawned nothing until its
first prompt (V4 and V5 leg 6); `/exit` ended a session, the list showed it
under saved, and `enter` resumed it in place (V4 and V5 leg 5); an unattended
idle session exited after the timeout (S4a's result above). Beyond the
clauses, `/provider` and `/model` set the next dispatch's `--provider` and
`--model` (V4 and V5 leg 7), and composer `@` sent `@README.md` verbatim to
every provider tried (V4 and V5 leg 8: grok attaches the file itself, cursor
and native read it with their read tool). One clause holds with a caveat:
`←` and `/sessions` cannot leave a session whose card is up (the card owns the
keyboard), so on Linux the blocked session's list was reached from a second
terminal, and on the mac `←` was pressed before the question arrived (`13`,
SF-99). Automated: `tests/cli/test_sessions.py`, `test_dispatch.py` and
`test_tui_composer_at_mentions_a_file` in real terminals, and 31 new frame
goldens across PRs 2–4, with no existing golden or fixture moved (V6).
Everything S4a and S5 found and did not do is in `13`, SF-68–SF-102 (the
plan's own rows, PR 1's, and those of PRs 2–4 and the live smoke). **S4b (the
hub) is next, then S3.**

### S6 — `craze web` (directional)

The hub serves the protocol over WebSocket and a static React bundle, bound
to loopback or a tailnet interface only (`02`). No accounts, no relay.

- Size: L; the web client is most of it.
- Decide after S1–S5: if shed-mobile and shed desktop cover the need, this
  may never be built.

### S7 — relay (directional)

The hub dials out; a hosted server republishes the protocol to web and mobile
clients off the tailnet. Needs machine enrollment, scopes, TLS, a craze-side
capability allow-list, and visible remote-attach state (`02`).

- Size: XL; it is a product, not a feature.
- Its one clear benefit over S3 + tailnet is reach when off the tailnet.

## Deferred (not scheduled)

`craze journal` mining subcommands · remote PTY · port forwards and preview ·
file browse/edit over the protocol · yamux transport · persisted command
receipts · windowed snapshot pagination · multi-user anything · a non-Go
server · IDE-like desktop shell.

## Conventions per phase

- Plan first (panel review), then the repository's gated implementation flow;
  `make lint && make test && make test-race && make build && make test-cli`
  per commit.
- Any phase that changes the wire updates the schema, the fixtures, and the
  published spec in the same PR.
- Every phase closes with a live smoke on Linux and the mac-mini against real
  agents, recorded as an **Exit result** here.
- Update `08` when a phase changes a decision; update the table above and the
  README status table when a phase merges.
- A phase is not complete until everything it found and did not do is a row in
  `13`, and every `13` row it closed is deleted.
