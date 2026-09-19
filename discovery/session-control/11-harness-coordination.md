# 11 — Harness coordination

The native harness (`discovery/native-harness/`) is in active development in
parallel. This file records where the two tracks touch and the ordering that
matters. Any plan on either track that edits `internal/agent` reads this
first.

## Summary

**The tracks are complementary and only S1 overlaps.** The harness lives
below the `agent.Session` seam and is lint-isolated from the rest of craze;
session control lives at and above that seam. Ordering matters at one point:
**S1b should land before the harness adds an ask channel, sub-agent event
plumbing, or resume** (SD-17). H2 is not blocked, and S1a touches nothing the
harness owns beyond one emit call in `native.go`.

As recorded in `discovery/native-harness/07-roadmap.md`, the first ask channel
is **H3** (permission cards). The craze-harness session reports that the owner
has since deferred native permission prompts, possibly well past H3; until
that is written down as a harness decision, read the constraint as "S1b before
H3". The harness session reports (2026-09-19) that the decision is written as
**harness D-39** on Plan 019's branch (`15a3553`, landing with H2's first PR):
native permission prompts lose their phase, the gate ships allow-all, and the
owner's direction is an auto-mode evaluator over it, **after S1's ask
registry**. Once that PR merges, cite D-39 here and relax the constraint to
"S1b before the first harness phase that needs an ask, and before H6 and H7".

## Where they touch

| surface | harness | session control | resolution |
|---|---|---|---|
| `internal/agent/native.go` emit path | H2 maps tool events through the adapter's sink | S1a puts the ordering boundary and sequence numbers on the emit path | One emit choke point, kept by both. Whoever lands second rebases a small conflict. |
| `agent.Session` interface | H2 does not change it; native gains `Interject` via `Capabilities` | S1 adds `Subscribe`, ask reads, acknowledged cancel | No conflict while H2 leaves the interface alone. |
| `agent.Event` and its encoders | H2 adds no field; one new stop reason, `max_turn_requests`; a test asserts every native event renders through `internal/cli/events.go` | S1a adds a **lossless codec** that is the journal and wire schema; `events.go` stays a lossy CLI projection (SD-20) | H2's test is right for the CLI. From S1a, any new `agent.Event` field must also round-trip the lossless codec in the same PR; that codec, not `events.go`, is what native events must survive. |
| Persistence | `internal/harness/store`: pi's single tree store. The harness **dropped** its own two-rail proposal (harness D-03), and `03-session-store.md` defines TUI replay as the leaf→root walk | The journal: a UI-facing log craze adds above the provider seam (`04`, SD-31) | No harness change. The leaf→root walk stays the cross-incarnation replay authority for native (SD-23); the journal serves attach within an incarnation and is the debug record. Lines are joined by store entry id **with the persistence outcome**, because `StepDone` is emitted even when the store append failed. The journal does not import `internal/harness/store`. |
| Rewind and fork | The store is a tree: rewind moves the leaf, fork copies the file | The journal is linear | A `transcript.reset{fromEntryId}` event kind is reserved so a rewind can be expressed in a linear log (`03`). |
| Asks | Deferred; H2 ships a `Gate` seam (Allow / Deny{reason} / Ask) that allows everything | S1b's engine-owned ask registry | A future native Ask plugs into the registry. **Do not copy `live.go`'s parked-ask map into `native.go`.** |
| Sub-agents (H6) | Child processes speaking `craze prompt --json` | The protocol's event schema; headless hosts (S4) | Children should speak the session-control event schema with `seq`, not a private variant; after S4 a child can be a real headless host and appear in the agent view under its parent id. |
| Resume (H7) | "Replay for the TUI is the same leaf→root walk emitting Events" | The journal replays UI events for every provider | The leaf→root walk is H7's replay and stays the cross-incarnation authority for native (SD-23); the new incarnation journals those replayed events, flagged. Whether earlier journals ever enrich a restored transcript is SQ13, not H7's problem. |
| Session index (H7) | Native sessions join `sessions.jsonl` at H7; until then `writeIndex` deliberately excludes them and `nativeSession.open` rejects a load | S1b moves index writes into the engine; S4's hub roster lists hosts | S1b **keeps the native exclusion** until H7. Attachable live sessions (the roster) and resumable stored sessions (the index) are different lists. |
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

If item 5 is recorded as a harness decision, the ordering relaxes to "S1b
before whichever harness phase first needs an ask (H5's plan and question
tools are the likely first), before H6, before H7". Items 3 and 4 are Plan
019's stated intent, not yet code: treat the diagnostic and entry-id handoff
as an **integration dependency to verify** when both sides have landed. Today
`harness.StepDone` carries usage only.

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
PR 4 (interject). Whoever lands second rebases and owns the joint `-race` test.

## What S1 owes the harness

- A way for the native adapter to forward `harness.Event` diagnostics to the
  journal that does not widen `agent.Event` for clients: a journal-only
  `diag` path beside the event path (`04`). S1a ships it; the adapter starts
  using it once H2's events exist, whichever lands second.
- The ask registry's API early, so a harness `Gate` that returns Ask has
  something to plug into.
- `seq` on `craze prompt --json` lines (S1a), so H6's child processes are born
  speaking the sequenced schema.

## Practical notes

- This folder is on `main` from `f943472` (2026-09-19); a harness worktree
  sees it after rebasing onto or merging `origin/main`.
- Plan 019 tells its executing session to re-read this file before the runner
  commit and flag conflicts to the owner.
- Keep the two sessions talking: an S1 plan should be sent to the
  craze-harness session for review of the `native.go` and interface changes
  before its panel, and the reverse for any harness plan that touches
  `internal/agent` beyond `native.go`.
