# 11 — Harness coordination

The native harness (`discovery/native-harness/`) is in active development in
parallel. This file records where the two tracks touch and the ordering that
matters. Any plan on either track that edits `internal/agent` reads this
first.

## Summary

**The tracks are complementary and only S1 overlaps.** The harness lives
below the `agent.Session` seam and is lint-isolated from the rest of craze;
session control lives at and above that seam. Ordering matters at one point:
**S1 should land before the harness adds an ask channel, sub-agent event
plumbing, or resume** (SD-17). H2 is not blocked.

## Where they touch

| surface | harness | session control | resolution |
|---|---|---|---|
| `internal/agent/native.go` emit path | H2 maps tool events through the adapter's sink | S1a puts a broker and sequence numbers on the emit path | One emit choke point, kept by both. Whoever lands second rebases a small conflict. |
| `agent.Session` interface | H2 does not change it; native gains `Interject` via `Capabilities` | S1 adds `Subscribe`, ask reads, acknowledged cancel | No conflict while H2 leaves the interface alone. |
| `agent.Event` / `internal/cli/events.go` | H2 adds no field; one new stop reason, `max_turn_requests`; a test asserts every native event renders through `events.go` | These shapes become the wire schema (`05`) | Any later field must round-trip through `events.go` in the same PR. |
| Persistence | `internal/harness/store`: the model-facing rail | The journal: the UI-facing rail (`04`) | Two rails, never merged (SD-08). Journal lines carry store entry ids. |
| Asks | Deferred; H2 ships a `Gate` seam (Allow / Deny{reason} / Ask) that allows everything | S1b's engine-owned ask registry | A future native Ask plugs into the registry. **Do not copy `live.go`'s parked-ask map into `native.go`.** |
| Sub-agents (H6) | Child processes speaking `craze prompt --json` | The protocol's event schema; headless hosts (S4) | Children should speak the session-control event schema with `seq`, not a private variant; after S4 a child can be a real headless host and appear in the agent view under its parent id. |
| Resume (H7) | "Replay for the TUI is the same leaf→root walk emitting Events" | The journal replays UI events for every provider | Plan H7 on two rails: model context from the store, UI replay from the journal. The leaf→root walk remains the fallback for a session with no journal. |
| Session index (H7) | Native sessions join `sessions.jsonl` | S1b moves index writes into the engine; S4's hub roster lists hosts | Coordinate so a native session is indexed once, by the engine. |
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

Item 5 relaxes the ordering: with no native ask channel soon, S1 is not racing
H3. The constraint is now "before whichever harness phase first needs an ask
(H5's plan and question tools are the likely first), before H6, before H7".

## What S1 owes the harness

- A way for the native adapter to forward `harness.Event` diagnostics to the
  journal that does not widen `agent.Event` for clients: a journal-only
  `diag` path on the emit side (`04`).
- The ask registry's API early, so a harness `Gate` that returns Ask has
  something to plug into.
- `seq` on `craze prompt --json` lines (S1a), so H6's child processes are born
  speaking the sequenced schema.

## Practical notes

- This folder was written untracked in the shared checkout on `main`
  (2026-09-19). Until it is committed, a harness worktree reads it by absolute
  path: `/home/charliek/projects/craze/discovery/session-control/`.
- Plan 019 tells its executing session to re-read this file before the runner
  commit and flag conflicts to the owner.
- Keep the two sessions talking: an S1 plan should be sent to the
  craze-harness session for review of the `native.go` and interface changes
  before its panel, and the reverse for any harness plan that touches
  `internal/agent` beyond `native.go`.
