# 11 — Harness coordination

The native harness (`discovery/native-harness/`) is in active development in
parallel. This file records where the two tracks touch and the ordering that
matters. Any plan on either track that edits `internal/agent` reads this
first.

## Summary

**The tracks are complementary and only S1 overlaps.** The harness lives
below the `agent.Session` seam and is lint-isolated from the rest of craze;
session control lives at and above that seam. Ordering matters at one point:
**S1b before the first harness phase that needs an ask, and before H6 and H7**
(SD-17, relaxed below). H2 is not blocked, and S1a touched nothing the harness
owns beyond the emit path in `native.go`.

**The ordering is relaxed, 2026-09-19.** The constraint used to read "S1b
before H3", because `discovery/native-harness/07-roadmap.md` put the first ask
channel at H3 (permission cards). **Harness D-39** is now on `main` — merged in
PR #33 (`0277ce7`), written in `ee3c148` — and it takes permission prompts out
of H3 entirely: the gate seam ships as `AllowAll`, `Ask` is treated as a deny,
and the owner's direction is an auto-mode evaluator over the gate **after S1's
ask registry**. So H3 no longer needs an ask, and the constraint is the one
this file said to adopt once D-39 landed: S1b before whichever harness phase
first needs an ask (H5's plan and question tools are the likely first), and
before H6 (sub-agent event plumbing) and H7 (resume).

## Where they touch

| surface | harness | session control | resolution |
|---|---|---|---|
| `internal/agent/native.go` emit path | H2 maps tool events through the adapter's sink | S1a puts the ordering boundary and sequence numbers on the emit path | One emit choke point, kept by both. Whoever lands second rebases a small conflict. |
| `agent.Session` interface | H2 does not change it; native gains `Interject` via `Capabilities` | S1 adds `Subscribe`, ask reads, acknowledged cancel | No conflict while H2 leaves the interface alone. |
| `agent.Event` and its encoders | H2 adds no field; one new stop reason, `max_turn_requests`; a test asserts every native event renders through `internal/cli/events.go` | S1a adds a **lossless codec** that is the journal and wire schema; `events.go` stays a lossy CLI projection (SD-20) | H2's test is right for the CLI. From S1a, any new `agent.Event` field must also round-trip the lossless codec in the same PR; that codec, not `events.go`, is what native events must survive. |
| Persistence | `internal/harness/store`: pi's single tree store. The harness **dropped** its own two-rail proposal (harness D-03), and `03-session-store.md` defines TUI replay as the leaf→root walk | The journal: a UI-facing log craze adds above the provider seam (`04`, SD-31) | No harness change. The leaf→root walk stays the cross-incarnation replay authority for native (SD-23); the journal serves attach within an incarnation and is the debug record. Lines are joined by store entry id **with the persistence outcome**, because `StepDone` is emitted even when the store append failed. The journal does not import `internal/harness/store`. |
| Rewind and fork | The store is a tree: rewind moves the leaf, fork copies the file | The journal is linear | A `transcript.reset{fromEntryId}` event kind is reserved so a rewind can be expressed in a linear log (`03`). |
| Asks | Deferred; H2 ships a `Gate` seam (Allow / Deny{reason} / Ask) that allows everything | S1b's engine-owned ask registry, **shipped** (`internal/agent/asks.go`) | A future native Ask plugs into the registry. **Do not copy `live.go`'s parked-ask map into `native.go`.** Native's own asks close `cancelled, by: "call"` rather than `closing` at `Close`, because native cancels the turn's context first (Plan 023 §3.5, `05`) — the one place a native `Gate` `Ask` differs from an ACP one. |
| Settings (H5) | H5's PR 2 puts `SetMode` on native | S1b's settings worker and setter signatures, **shipped** (C10) | The setters now take a `cause` parameter and return `SetOutcome{Value, Ticket}`; a session enqueues its own delta under `s.mu`, in the section that mutates its snapshot, and every session-side `Flush` a setter calls passes its own `done` channel (`EventLog.Flush(ctx, done)`, alongside `Publish`'s). An enqueued event may not carry `Err` — the log replaces a non-nil one with an inert sentinel rather than call a method on it under the caller's lock (S1b's C2, X7). H5's PR 2 builds `SetMode` on these signatures directly; it is not a new pattern to invent. |
| Sub-agents (H6) | Child processes speaking `craze prompt --json` | The protocol's event schema; headless hosts (S4) | Children should speak the session-control event schema with `seq`, not a private variant; after S4 a child can be a real headless host and appear in the agent view under its parent id. |
| Resume (H7) | "Replay for the TUI is the same leaf→root walk emitting Events" | The journal replays UI events for every provider | The leaf→root walk is H7's replay and stays the cross-incarnation authority for native (SD-23); the new incarnation journals those replayed events, flagged. Whether earlier journals ever enrich a restored transcript is SQ13, not H7's problem. |
| Session index (H7) | Native sessions join `sessions.jsonl` at H7; until then `writeIndex` deliberately excludes them and `nativeSession.open` rejects a load | S1b moved index writes into the engine, **shipped** (C12: `engine.IndexOptions`, the index worker, `crazeId`) | S1b **kept the native exclusion**, unchanged by the move: `NativeProvider`'s own comment still says native sessions are never indexed until H7 gives them a loader, confirmed live on both platforms in S1b's smoke (`12`, V6/V4). H7 inherits that exclusion and the engine-owned index as the seam to extend, not to replace: attachable live sessions (the roster) and resumable stored sessions (the index) stay different lists. |
| TUI state | H2 adds nothing to `tui.Model` | S1 moves session semantics out of `tui.Model` | New session semantics go in the adapter or engine, never `tui.Model`. |

## State of the harness track (2026-09-19)

From the craze-harness session's reply on Plan 019 (H2 tools, draft, in panel
review; execution will be a separate session in worktree `../craze-plan019`):

1. Native events leave only through the adapter's single emit; H2 adds no
   reader of `Events()` and changes nothing in `internal/tui`.
2. H2 adds no field to `agent.Event` or `ToolEvent`; only the
   `max_turn_requests` stop reason.
3. `harness.Event` gains diagnosis-grade data: tool `At` / `Duration` /
   `ExitCode` / `ErrorClass` / `Truncation{kept,total,spill}`; `StepDone` with
   provider / model / wire model, raw and normalized finish reason, time to
   first token, usage including cache-read; `Retrying` with attempt and
   reason; and a generic `Diag{Kind,Fields}` for doom-loop trips, length
   handling, cancel-synthesized results, and steers. **In H2 the adapter drops
   what has no `agent.Event` home; S1's journal picks these up without a
   harness change.**
4. The harness mints a session-unique id per tool call (provider call ids
   repeat), and `StepDone` carries the store entry ids it wrote.
5. Permission prompts are deferred, possibly well past H3; the likely shape is
   an auto-mode evaluator behind the `Gate` seam.

Item 5 is now harness D-39 on `main`, and the ordering is relaxed accordingly
(see the summary). Items 3 and 4 are Plan 019's stated intent, not yet code:
treat the diagnostic and entry-id handoff as an **integration dependency to
verify** when both sides have landed. Today `harness.StepDone` carries usage
only.

## Agreed for the H2 / S1a integration (2026-09-19)

Both plans are final; the harness session adopted all five points of Plan 020's
§2.7 and R7:

1. `internal/cli/events.go` stays the CLI projection (Plan 019 §3.10 amended);
   the lossless codec is the journal and wire schema.
2. **No session lock is held across a publish.** Plan 019's C12 orders an
   accepted interjection before `EventDone` by making the turn goroutine the
   sole emitter of both, not by emitting under `s.mu`.
3. Native tool events are deep-copied before the merge lock is released (C10).
4. Lossy tool progress keeps its `select … default` send until S1a's
   `TryPublish` exists; whoever lands second switches it.
5. `StepDone` carries the persistence outcome, with store entry ids only when
   the append succeeded (C8).

Plan 019 ships in four PRs; `native.go`'s emit path changes in PR 3 (tools) and
PR 4 (interject).

**Who owns the joint `-race` test: whoever lands the second change to
`native.go`'s emit path.** That test is one run of tool progress + interject +
cancel + `Close` together, under `-race`, proving the lock-order rule of point
2 above still holds with both sides' code in place. As of S1a's own PR the
state is:

- `origin/main` carries Plan 019's PR #33 (`0277ce7`, foundations) and PR #34
  (`bc2aaab`, the tool framework). Neither touches `internal/agent` at all —
  `git log 215cac0..origin/main -- internal/agent/` is empty — so the emit path
  is still as H1 left it.
- **If S1a merges first** (the expected case, since Plan 019's PR 3 and PR 4
  are not open yet), Plan 019's executing session rebases `native.go` onto the
  `EventLog` publish, switches lossy tool progress from `select … default` to
  `TryPublish` (point 4), and owns the joint `-race` test in PR 3 or PR 4.
- **If Plan 019's PR 3 lands first**, S1a rebases onto it, converts whatever
  emit sites it added, and owns the joint `-race` test in S1a's own PR — and
  PR 4 then inherits the same rule again for interject.

Either way the test lands in the PR that makes the emit path carry both
changes, not in a follow-up, and the second lander tells the other session
before merging.

## What S1 owes the harness

- A way for the native adapter to forward `harness.Event` diagnostics to the
  journal that does not widen `agent.Event` for clients: a journal-only
  `diag` path beside the event path (`04`). S1a ships it; the adapter starts
  using it once H2's events exist, whichever lands second.
- The ask registry's API early, so a harness `Gate` that returns Ask has
  something to plug into.
- `seq` on `craze prompt --json` lines (S1a), so H6's child processes are born
  speaking the sequenced schema.

## Plan 021 (S1b) / harness H4 — parallel run (2026-09-20)

Plan 021 (S1b: the engine turn driver, the queue, asks, settings, identity)
and harness H4 (Claude compat: instruction files with imports and `paths`
gating, workspace skills and commands, `craze import claude`) run in
parallel, each in its own session and worktree. Where they meet (plan
`021-session-control-s1b-engine.md` §2.7):

| surface | this plan (S1b) | H4 | rule |
|---|---|---|---|
| `internal/harness/**` | one comment in `steer.go` (C6) | owns it | no conflict |
| `internal/agent/{catalog,skills,plugins,expand}.go`, `internal/cli/import.go` | untouched | likely owns | no conflict |
| `internal/agent/native.go` | prompt path, the queue, `Cancel`, `announceCurrent` and the setters, `Answer*` | `Start` / system-prompt assembly, the catalog it reports | H4 stayed out of the ranges this plan named. **`announceCurrent` did not end up a shared site**: native's `snap.Plugins` is set once in `Start` and never changes, so C10 announces nothing for it and waits for nothing (plan X3). What did overlap: H4's PR 3 added about ten lines to `native.go`'s `prompt()` and a new `emitCtx`, merged into the S1b branch as one gated merge commit (X21) |
| `agent.Session`, `agent.Event`, `agent.Snapshot` | narrowed and extended (§3) | should not change them | an H4 need here is raised with the owner first |
| `internal/tui/slash.go` | `/rename`, `/model`, mode paths only | menu population | second lander rebases |
| codec | new fields round-trip (the plan's A12) | any new `agent.Event` field must too (`11`, this file) | the completeness test enforces both, and both landed clean |
| reviewers, the mac-mini, `-race` on a shared box | | | stagger smokes; S1b's V5 ran on a quiet box |

**S1b shipped 2026-09-21 (all three PRs merged): the gate for H3, H5, H6, H7
and H8 is open.** H3 and H5 waited for Plan 021's PR 2 (`feature/plan-021-s1b-asks`,
#42 `db7686e`, the ask registry and its sequenced endings) and could start once
it merged — H5's own PR 1 (native asks, turn tokens) had already merged into
S1b's PR 3 branch mid-execution (`a41ba29`, plan X51). H6, H7 and H8 waited
for S1b as a whole, per SD-17's original ordering, relaxed above; all three
PRs are merged (#41 `fdaaa6d`, #42 `db7686e`, #NN `TBD`), so nothing more
blocks them from this file's side. `11`'s two shipped rows above — the
settings seam (H5's `SetMode`) and the session index (H7) — are what those
phases build on, as built rather than as planned.

Every PR in Plan 021 started from a freshly fetched `origin/main`, and the
executor told the `craze-harness` session when each PR opened and when it
merged — the same discipline as the S1a / H2 integration above.

## Practical notes

- This folder is on `main` from `f943472` (2026-09-19); a harness worktree
  sees it after rebasing onto or merging `origin/main`.
- Plan 019 tells its executing session to re-read this file before the runner
  commit and flag conflicts to the owner.
- Keep the two sessions talking: an S1 plan should be sent to the
  craze-harness session for review of the `native.go` and interface changes
  before its panel, and the reverse for any harness plan that touches
  `internal/agent` beyond `native.go`.
