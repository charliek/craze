# 12 — Progress and results

The durable record of what each phase actually did. `07` says what a phase is
for and how it exits; this file says what happened. Plans, panel reviews, and
smoke artifacts stay outside the repo (`~/.claude/plans/craze/`); what belongs
here is what a future session needs and cannot get from `git log`: baselines,
deviations, evidence, and what the next phase inherits.

## How to record a phase

When a phase's plan is final, add a section with **Status: planned**. When it
merges, fill the rest in the same PR that updates `07`'s table and the
README's status table.

```
## S<n> — <name>

| | |
|---|---|
| Status | planned / in progress / complete |
| Plan | NNN-<slug> (path outside the repo) |
| Baseline | commit the plan was verified against |
| Branch / PRs | … |
| Merged | date, commit |

### Outcome
One paragraph, then the exit criteria from `07` as a table: criterion | result | evidence.

### Deviations from the plan
Numbered; each says what changed and why. Mirrors the plan's execution amendments.

### Live smoke
Table per platform (Linux, mac-mini): provider | scenario | result.

### Decisions and questions touched
SD-nn added or superseded; SQ-n resolved.

### Handoff
What the next phase inherits: new seams, known limitations, follow-up issues filed.
```

Rules: evidence over adjectives; a failed or skipped criterion is recorded as
failed or skipped; raw artifacts are referenced by path, not pasted.

## S0 — discovery

| | |
|---|---|
| Status | complete |
| Plan | none (discovery session) |
| Baseline | `215cac0` |
| Branch / PRs | direct to `main` at the owner's direction |
| Merged | 2026-09-19, `f943472`; panel amendments and this file in the following commit |

### Outcome

Four delegated reviews (craze's session architecture; prox's yamux tunnel and
per-user daemon; shed + shed-mobile with the gx lane; t3code) grounded `01`–`06`
and `09`. The owner settled topology, protocol, remote scope, and journaling in
session (SD-02, SD-03, SD-07, SD-12). The craze-harness session was briefed and
replied with Plan 019's position (`11`).

### Review — 2026-09-19

The roadmap was put to the review panel with the goals and SD-01..SD-17
declared settled and feedback limited to technical merit.

| reviewer | result |
|---|---|
| Codex, `gpt-6-astra`, high effort, read-only | 20 findings (2 blocker, 18 major), all code-grounded |
| CodeRabbit | 18 findings (2 blocker, 13 major, 3 minor) |
| GLM 5.3 via opencode | first run ended with no review (opencode's own permission rules rejected its shell calls); a retry limited to read-only tools returned 11 findings (3 major, 8 minor), applied in a follow-up commit |

### Confirmed

Topology, protocol family, Unix-socket trust, and the phase order all stood.
No reviewer argued a settled decision was unsound.

### Corrected

Both blockers were raised independently by both reviewers, and the planning
read for S1a had found two of the same problems.

1. **Coalesced journaling plus subscribe-then-read left a silent gap.** The
   log is now per event with one attach cutoff served from a ring and the
   journal (SD-18).
2. **Numbering events does not sequence state, and a pump breaks "emitted
   means buffered".** One leaf-lock ordering boundary, inline in emit, shared
   by every `Session` implementation including the Stub (SD-19).
3. `internal/cli/events.go` is a lossy projection, not a schema base (SD-20).
4. Journal crash and stall semantics: one file per incarnation, a writer that
   never blocks, gaps recorded (SD-21); three identities (SD-22); the
   provider's replay stays the cross-incarnation authority (SD-23).
5. The engine must own the turn **driver**, and the event stream is not yet a
   complete record of the transcript (SD-24, SD-30). S1 was re-sized upward.
6. Ask, cancel, command-id, and settings semantics tightened (SD-25);
   approval policy separated from frontend presence (SD-26).
7. Socket namespace hardening and what an SSH exec can assume (SD-27);
   lifecycle and hub-protocol contracts pulled forward into S2 (SD-28); the
   shed transport handoff and append-only rows stated exactly (SD-29).
8. SD-08 cited the harness for a two-rail rule the harness had dropped
   (SD-31).
9. New open questions SQ12 (rewritten: hosts born detached), SQ13–SQ16.
   **SQ12 should be decided before the S2 plan.**

10. From GLM's retry: a browser can reach a loopback WebSocket, so S6 needs an
    `Origin` allow-list and a per-boot token, with an optional auth field in
    `hello` from S2; the hub dials the host per attached client and splices;
    the fold under the lock must be amortized O(1); attach during a load
    replay; responses ordered after their events; the engine publishes
    activity for headless hosts; `craze prompt --json` stability in S1b's exit;
    two pinned error cases. Its "folder drift" finding described files it read
    mid-edit and was already resolved.

Not taken: CodeRabbit's proposal that the engine be a wrapper that is the sole
reader of `Events()` with sequence numbers assigned there. It needs a delivery
barrier on every mutating method to preserve "emitted means buffered"; the
inline shared component gets the same ordering without one. Recorded in `03`.

### Handoff

S1 is next. Its first slice, S1a, is recorded below.

## S1a — event log, codec, journal

| | |
|---|---|
| Status | planned |
| Plan | `020-session-control-s1a-event-log` (outside the repo, with its raw panel reviews) |
| Baseline | `b3f7b0f` |
| Branch / PRs | `feature/plan-020-session-control-s1a`, worktree `../craze-plan020` |
| Merged | — |

### Plan review — 2026-09-19

| reviewer | result |
|---|---|
| Codex, `gpt-6-astra`, high effort, read-only | 20 findings (2 blocker, 16 major, 2 minor) |
| GLM 5.3 via opencode, read-only tools | 18 findings (2 blocker, 8 major, 8 minor), against the first draft |
| CodeRabbit | 18 findings (1 blocker, 10 major, 7 minor), against the first draft |

Both blockers were concurrency schedules in the draft's design, found before
any code existed: a publisher waiting for the ordering lock could no longer be
cancelled by its own context, and the attach cutoff read a range it had not
retained, so eviction or a journal gap could leave a silent hole.

CodeRabbit added what the other two missed: Plan 019's tool progress is lossy
by contract and needs a non-blocking publish; the journal opt-out failed open
on a malformed config; and the error classes in the draft were sentinels that
never reach an event. The design that came out is roadmap SD-32: cancellable admission, encode before admission,
the sequence number in the envelope, the journal fed directly, pinned ring
records at the cutoff, and one delivery owner per subscription.

What S1a does **not** ship from `04`'s S1a rows: `diag` kinds for wire
outcomes, signals, and recovered panics, and the raw wire sidecar. Those stay
follow-ups; the executing session narrows `04`'s row when it lands.

Outcome, deviations, live smoke, measurements (journal size by event type and
the cost of a resume, inputs to SQ3 and SQ15), and handoff are filled in by the
executing session in the PR's last commit.
