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
| Status | complete |
| Plan | `020-session-control-s1a-event-log` (outside the repo, with its raw panel reviews and its execution amendments X1–X18) |
| Baseline | plan: `b3f7b0f`. Execution started from `37ab6a1` and rebased onto `origin/main` at `0277ce7` (PR #33) mid-run, after C3; the orchestrator rebases once more onto `origin/main` before opening the PR |
| Branch / PRs | `feature/plan-020-session-control-s1a`, worktree `../craze-plan020`; twelve commits — the plan's nine (C1–C4, C5a–C5c, C6, C7) plus three review-fix commits (`d00519c`, `1ead7f1`, `8fa2219`). PR — (the number goes here) |
| Merged | — (date and squash commit go here when the PR merges) |

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
follow-ups; `04`'s row is narrowed to the seven kinds that shipped, with a
table naming who picks up the rest.

### Outcome

Shipped as planned, with no pinned decision reopened. `agent.EventLog` is the
one ordering boundary every `Session` publishes through — the ACP session, the
native adapter, and `tui.Stub` — assigning `Seq`, keeping a ring, serving
budgeted subscriptions across the ring/journal seam, and feeding
`internal/journal` directly and without blocking. `internal/agent/eventcodec.go`
is the lossless codec, guarded by a reflection-driven completeness test that
fails when a field is added to `agent.Event` and not mapped. Every session the
TUI or `craze prompt` builds writes a journal; `craze frame` writes none.
`craze prompt --json` lines carry `seq`. No user-visible behaviour changed: no
golden file moved, and C4 — the commit that rerouted every emit — edited no
existing test at all, adding two new ones instead (A2's exception went unused).

**The phase's exit criteria** (`07`, "Exit (S1a)"):

| criterion | result | evidence |
|---|---|---|
| goldens and `craze prompt --json` output unchanged apart from `seq` | pass | `git diff --stat 0277ce7..HEAD -- '*testdata*'` touches one new file, `internal/journal/testdata/slug_fixtures.json`; `json_test.go`'s five end-to-end `wantLine` literals are byte-identical with `seq` removed by a helper (X18); the seven whole-dict pytest sites pop `seq` and compare the rest exactly |
| a subscriber attached mid-turn with a cursor gets exactly the primary's events after it, in order, across the ring/journal seam | pass | `TestEventLogResumeInsideTheRingWhileAPublisherRuns`, `…ResumeWithItsHeadInTheJournal`, `…ResumeSpanningTheJournalAndTheRing`, `…SubscribePinsItsBacklogAgainstLaterEviction`, `…AResumeWaitsForAStalledJournalAndThenServesIt` |
| a stalled subscriber is dropped without delaying the primary | pass | `TestEventLogASubscriberThatNeverReadsIsDroppedAndHoldsNobodyBack` |
| every session, ACP and native, leaves a journal whose `event` lines decode losslessly | pass | `TestJournalLiveSessionWritesOneFile` and `TestJournalNativeSessionWritesOneFile`, both through `assertEventLines` (`Record.Event` → `DecodeEvent`, compared against what the primary delivered); end to end in `tests/cli/test_journal.py::test_prompt_writes_one_journal` |
| a failing or stalled disk degrades to a recorded gap, never a stalled turn | pass | `TestAppendNeverWaitsForAStalledWrite`, `TestOverflowIsAnOrderedGapBetweenItsNeighbors`, `TestAPartialWriteFailsTheWriterWithOneNoticeOffTheAppendPath`, `TestASyncErrorFailsTheWriter`, `TestCloseReturnsWhileAWriteIsStalled` |
| race detector clean | pass, with one caveat | `make test-race` green at every commit that changed Go code, with `./internal/journal/...` added to the Makefile's list at C2. V5's `-count=20` loop found one failure in a **pre-existing** load-sensitive test — see "V5" below |

**The plan's acceptance criteria** (§7). Every criterion is named; where the
evidence is a test the test's name is given, because a criterion with no named
test is a criterion nobody checked.

| # | result | evidence |
|---|---|---|
| A1 gate at every commit; `make docs`; `internal/journal` under `test-race` | pass | the per-commit gate ran at each commit; `Makefile`'s `test-race` line gained `./internal/journal/...` in C2; `make docs` green at C7, which rebuilds the whole site including C6's `cli.md` change |
| A2 no golden change; no existing test edited in C4 | pass, stronger than asked | no golden changed; C4 (`660afb0`) touched `live.go`, `live_queue.go`, `native.go`, `stub.go` and **two new** test files only — no `Event.Seq` comparison needed fixing |
| A3 codec completeness and table tests | pass | `TestEventCodecCarriesEveryFieldReachableFromEvent`, `TestEventCodecCompletenessFillerReachesEveryField`, `TestEventCodecRoundTripsWhatTheEmitSitesBuild`, `TestEventCodecPinsTheWireShape`. The "a scratch field fails it" and "swapping two same-typed fields fails it" checks were run locally and deliberately not committed; the bool one-hot pass added in X3 caught a real swapped `Plan.Auto`/`Accepted` |
| A4 concurrent publishers, one gapless sequence everywhere | pass | `TestEventLogConcurrentPublishersGetOneGaplessSequenceEverywhere` |
| A5 emitted means buffered | pass | `TestEventLogWhatAReturnedPublisherEmittedIsAlreadyInThePrimary` |
| A6 cancellable admission | pass | `TestEventLogAWaitingPublisherHonorsItsCtxWhileAnotherIsBlockedInside` (barrier-driven) |
| A7 slow subscriber dropped, nobody else loses or waits | pass | `TestEventLogASubscriberThatNeverReadsIsDroppedAndHoldsNobodyBack` |
| A8 abandoned publish consumes no number and reaches nothing | pass | `TestEventLogAnAbandonedPublishConsumesNoSeqAndReachesNothing` |
| A9 cursor resume, six schedules plus five failure reasons | pass | the five resume tests above, `…AResumeGivesUpOnAJournalThatNeverCatchesUp`, `…SubscribeRefusesACursorItCannotServeAndRegistersNothing`, `…TheFileLegsErrorsEachMapToOneReason`, `…AJournaledSubscribeStillRefusesWhatItCannotServe`, `TestSubscriptionEndsRatherThanDeliverAHole` |
| A10 lifecycle under `-race`, no goroutine leak | pass | `TestEventLogSubscriptionsSurviveConcurrentPublishDropCloseAndLogClose`, `…AnAbandonedBacklogReaderNeedsNoReaderToEnd`, `…CloseTwiceAndErrStaysPut`, `…AWedgedPrimaryHoldsNeitherNotesNorCloseAndCloseReleasesSubscribers`, `…CloseLeavesNoOwnerOrJournalGoroutineBehind` (counts goroutines) |
| A11 records immutable | pass | `TestEventLogARetainedRecordNeverChanges` |
| A12 omitted records identical in ring, live and file | pass | `TestEventLogOmittedRecordsAreTheSameInLiveRingAndFile`, `…InAFileReplay`, `…AnOverlongEventTypeIsTheSameOmittedRecordEverywhere`, `TestEventLogTakesTheJournalsSmallerRecordLimit` |
| A13 writer: stalls, ordered gaps, failure, sync, live reader, torn tail | pass | `TestAppendNeverWaitsForAStalledWrite`, `TestOverflowIsAnOrderedGapBetweenItsNeighbors`, `TestANoteOnlyOverflowIsAGapWithCountsAndNoRange`, `TestAPartialWriteFailsTheWriterWithOneNoticeOffTheAppendPath`, `TestADroppedPromptEndStillAsksForASync`, `TestTheLiveReaderNeverSeesAPartialLine`, `TestATornTailIsTolerated` |
| A14 bounded close for a blocked write and a blocked sync | pass | `TestCloseReturnsWhileAWriteIsStalled`, `TestCloseReturnsWhileASyncIsStalled`, `TestCloseHonorsAnEndedContextDuringAStall` |
| A15 the prompt-note path table, ACP and native | pass, one row restated | the sixteen tests in `internal/agent/promptnotes_test.go`, including `TestCloseSynthesizesAnEndingForAnOpenPrompt` and `TestNativeCloseSynthesizesAnEndingForAnUnrunContinuation`. X16 restates the native duplicate-continuation row: first-writer-wins means it records the run that happened, and `prompt_in_flight` is covered by a refused `Begin` instead |
| A16 `start_failed` present and before `closing` | pass | `TestStartFailedIsNotedBeforeClosing`, `TestNativeStartFailedIsNoted`; live, the stub-binary smoke run |
| A17 stderr tee, and plan 017's tests unchanged | pass | the ten tests in `internal/agent/stderr_test.go`, including `TestStderrTeeIsTransparentToItsWriter` and `TestStderrTeeDoesNotWaitOnTheJournal`; plan 017's exit-tail and SIGHUP tests were not edited and pass |
| A18 every session kind journals; bodies decode in Go | pass | as the exit row above, plus `tests/cli/test_journal.py::test_prompt_writes_one_journal` for modes, header, prompt text, increasing `seq` and JSON validity |
| A19 isolation, and the guard | pass, one clause restated | `TestJournalOffIsolation`; `TestMain` in `internal/cli/main_test.go` fails the package if a test journals into the real craze directory (matched on the header's pid, so a developer's own craze cannot trip it); `test_journal_off_leaves_no_directory`, `test_frame_never_journals`. X14: `craze frame --continue` builds its session inside the frame runner's own isolated HOME, so the **resumed** path's "no journal" rests on `frame.go` never calling the resolver plus the outside-visible check; the fresh path is asserted directly |
| A20 file creation, modes, umask, slug parity | pass | `TestCreationIsExclusive`, `TestAnExistingDirectoryIsUsedAsItIs`, `TestNewDirectoriesAndTheFileAreOwnerOnly`, `TestModesAreExactUnderAnOpenOrARestrictiveUmask`, `TestAUmaskThatNarrowsTheFileNeverBreaksTheJournal`, `TestTheFileLivesUnderTheWorkspaceSlugWithAnAbsoluteCwd`, `TestSlugMatchesTheHarnessStoreByteForByte` |
| A21 `seq` on `--json`, second key, none on CLI-authored lines | pass | `TestJSONSeqIsTheSecondKeyOfEveryLine`, `TestJSONSeqExactLines`, `TestSignalBetweenTheTakeAndTheSendRemovesTheRow`; live, `seq-crosscheck.sh` over five headless runs |
| A22 resume journals the replay in a new file; the first file unchanged | pass | `TestJournalResumeWritesANewFile`; live on cursor and grok, the first file's `sha256sum -c` `OK` after the resume |
| A23 `TryPublish` never waits | pass | `TestEventLogTryPublishNeverWaitsAndDropsForEveryone` |
| A24 opt-out fails closed | pass | `TestJournalRefusedIsOneDiagLine`, `tests/cli/test_journal.py::test_a_switch_craze_cannot_read_fails_closed`; live, `CRAZE_JOURNAL=0` wrote nothing and left `--json` untouched |
| A25 stderr budget keeps a count and loses no events | pass | `TestStderrBudgetKeepsOneCountAndLosesNoEvents` |
| A26 a session built and closed without starting leaves no file | pass | `TestJournalUnstartedSessionLeavesNothing`, `TestAnUnusedWriterOwnsNoGoroutineAndCreatesNothing`, `TestAClosingNoteAloneCreatesNothing` |
| V1 live smoke, Linux | pass, **one leg not run** | see "Live smoke". The native **tool-call** leg is not satisfiable today: `--provider native` is harness H1, which has no tools. Re-run after H2 lands |
| V2 live smoke, mac-mini | pass | grok and native at `1dfc551`; `cursor-agent` skipped (login keychain over ssh), which instead exercised the stderr tee against a real agent. See Live smoke below |
| V3 measurements | done | see "Measurements"; artifacts in the smoke folder |
| V4 overhead benchmarks | pass | budget met: a text delta with the journal attached is ~0.9–1.2 µs against a 5 µs budget. See "Measurements" |
| V5 `-race -count=20` | one pre-existing failure, diagnosed | see "V5" |
| V6 no-disruption | `sessions.jsonl` pass; **`config.toml` clause fails, pre-existing** | `sessions.jsonl`: 15 rows before, 21 after, the first 15 byte-identical. `config.toml`: changed mid-smoke because `persistProvider` (`internal/cli/provider.go`) writes the `provider` key on every run whose `--provider` names a real provider — **unchanged on `origin/main`** (`git show origin/main:internal/cli/provider.go`), so it is craze's existing behaviour triggered by the smoke's own `--provider grok` runs, not anything S1a added. **The criterion is restated for future phases as "`config.toml` unchanged apart from the `provider` key"**; as written it cannot hold for any smoke that exercises more than one provider explicitly |

### Deviations from the plan

The plan's execution amendments X1–X18, compressed. None reopens a pinned
decision; the full text is in the plan.

1. **X1 — the error-class table was wrong.** `ErrAgentExited` never reaches an
   event (only `Client.Close` returns it): an agent that dies non-zero mid-turn
   arrives through the spawn reaper's wrapper, one that exits 0 as
   `acp.ErrClosed`. The codec gained `agent_exit_status` (with the exit status,
   -1 on a signal), `empty_prompt` and `harness_closed`; `code` carries an exit
   status as well as an RPC code or HTTP status.
2. **X2 — one consumer does distinguish nil from empty**: the values inside
   `QuestionEvent.Answers`, which `events.go` passes straight to `--json`
   (`null` versus `[]`). Those keep the distinction; every other collection
   follows the plan's normalization rule.
3. **X3 — codec shape.** One flat object, `type` first, zero values omitted.
   Invalid UTF-8 becomes U+FFFD (a new equivalence rule, a JSON limit). An
   unknown `type` decodes instead of failing and unknown keys are ignored, so a
   newer craze's journal still replays; a missing or empty `type` is refused
   both ways. Leaf types convert by struct conversion, so a field added to one
   side fails to compile. The completeness test gained a one-hot pass per bool,
   because a counter cannot make two bools distinct.
4. **X4 — journal health.** `DroppedEvents` and `DroppedNotes` added, and
   `Truncated` sits on `Health` rather than on a range: a lost note has no seq,
   so a gap interval alone cannot account for it. The reserved gap slot is
   implemented as "records and notes cap at `QueueEntries-1`; gap markers do
   not count".
5. **X5 — the flush boundary advances over a written gap**, so `WaitFlushed(n)`
   for a dropped `n` returns once the gap is on disk and `ReadRange` over it
   returns `ErrGap` rather than hanging. `WaitFlushed` requests a flush itself.
6. **X6 — the journal distrusts what it is handed**: a body over
   `MaxRecordBytes` or one that is not a JSON object becomes an omitted record
   on the journal's side too, so ring and file agree. Notes are truncated twice
   (raw bytes at acceptance so queue memory is bounded, encoded bytes at write);
   one that still cannot fit becomes a small `note_too_large` diag.
   `Options.EventCodec` is passed in, because the journal cannot import
   `internal/agent`.
7. **X7 — umask.** Modes are owner-only, so a 0077 umask narrows nothing. A
   0277 umask on a directory the journal must create leaves it unwritable; the
   journal then fails with one notice and the session is unaffected. Fixing it
   would need a chmod, which the plan rules out.
8. **X8 — a `closing` note is discarded only when nothing else was queued**;
   queued alongside other lines it is written with them.
9. **X9 — the incarnation is minted before the log.** `journal.New` needs it
   for the file name and header, so `EventLogOptions` gained `Incarnation` and
   `agent` exports `NewIncarnation()`. UUIDv7 comes from the standard library,
   so `go.mod` is unchanged.
10. **X10 — decisions the plan left open.** `droppedAtClose` also counts
    admission escapes and a `TryPublish` refused at the cutoff, and is taken
    before the boundary is released. `record_omitted` and `subscriber_dropped`
    notes are queued inside the boundary so they always precede `closing`, and
    an RWMutex orders `Note` against the closing note so nothing follows it.
    `Records()` is unbuffered; the ring always keeps the newest record even
    alone over budget; `Subscribe` checks incarnation, future seq, budget, then
    the head.
11. **X11 — C4 details.** The `events` alias stays on `session` and
    `nativeSession` (receive-only) and is removed from the Stub, where nothing
    read it. Native's `Close` has no tee flush point (no child process). The
    per-emit-site immutability audit found nothing to fix. The Stub's boundary
    sits under `queueOp`, since it has no `emitMu`, and never under `mu`.
12. **X12 then X15 — two rounds on the publish path, both from the reviewer.**
    X12 released a subscription's budget just before each blocking send (so a
    subscriber within budget is never dropped after its reader already took a
    record), made `Close` wait for in-flight publishers before snapshotting
    `droppedAtClose`, and made the log adopt the journal's `MaxRecordBytes`
    when smaller. X15 then **replaced** X12's in-flight RWMutex with
    encode-before-entry plus a non-blocking counter: the RWMutex could deadlock
    when an `Event.Err`'s own `Error()` called `Close` or re-entered `Publish`
    during encoding, and could hold the pre-wire `EventCommand` emit in a wait
    its context could not cancel.
13. **X13 — six journal fixes from the C2 review.** `DiagNote.Fields` accepts
    plain scalars and `[]string` only and never calls a method on a caller's
    value; an omitted record's text is charged to the queue; a gap made only of
    `closing` notes never creates a file; the reader's line limit is exact for
    an unterminated last line; an overlong event type or an over-cap event line
    becomes an omitted record; and a time outside years 0–9999 is left out of
    its line rather than making the rest of the file unreadable.
14. **X14 — no existing `internal/cli` test needed a `CRAZE_HOME` line.** The
    tests R6 named either build `agent.Options` without calling `agent.New` or
    already isolate. The `TestMain` guard stays: it caught a real journal when
    one isolation line was removed on purpose. `CRAZE_JOURNAL` is trimmed and
    empty counts as unset; an explicit `false` is silent, only an unreadable
    value prints. The TUI sets `JournalDir` inside its build closure, so no
    existing call site moved. The header's `agentBinary` is the **requested**
    binary; the `session` note carries what the spawn resolved.
15. **X16 — an attempt's ending is first-writer-wins.** `Close`'s synthesized
    `prompt_end{closed}` is final, and native's duplicate continuation records
    the run that happened instead of overwriting it. The class table gained
    `caller_ended` and `other`; attempt ids are `prompt-N` / `interject-N`. The
    stderr tee is given only to the spawned child, so craze's own notes are
    never journaled as the agent's words. `Start` was split into `Start` (note,
    then teardown) and `start`, because a blanket close-on-any-error would have
    closed a live session on a second `Start`.
16. **X17 — the file leg reads through a small seam** whose production value is
    the `*journal.Writer` itself, because the journal's stall hooks are
    unexported and belong to its own tests; a held decorator wraps the real
    writer so a released resume still reads a real file.
    `ErrCursorUnresolvable` gained `Err`/`Unwrap` (additive);
    `journal_gap` wins over `journal_behind` when a re-read of `Health()` shows
    the writer recorded the loss.
17. **X18 — `json_test.go`'s `wantLine` literals could not carry an explicit
    `Seq`**: all five are end-to-end runs against the fake agent, where the
    number is as timing-dependent as pytest's. The literals stay byte-identical
    and a helper removes the number after asserting it; two new unit tests
    spell whole lines, numbered and not, for every line shape. `seq` is
    `omitempty` on six line structs declared straight after `type`, so its
    position is a property of the type and the no-`seq` rule needs no code at
    the two CLI-authored sites. There are **seven** whole-dict pytest sites,
    not the nine the plan counted.
18. **Recorded against the roadmap, as the plan said**: the journal is fed
    directly from inside the boundary rather than as a droppable subscription
    (SD-32), and S1a ships a subset of `04`'s `diag` kinds. `04`'s row is
    narrowed in this PR.

### Live smoke

#### Linux — pass

Full record, scripts and journals:
`~/.claude/plans/craze/020-session-control-s1a-event-log/smoke/linux/`
(`RESULTS.md`, `journals/`, `captures/`, `baseline/`). Binary built from
`719dc01`; cursor-agent `2026.09.18-9a7762b`, grok `1.0.30`, native on
`fireworks/deepseek-v4-flash`. Journals landed in the developer's **real**
`~/.craze`, which is the point of V6.

| provider | scenario | result |
|---|---|---|
| cursor | headless turn with a tool call | pass |
| grok | headless turn with a tool call | pass |
| native | headless turn — **no tool call possible** (H1 has no tools) | pass, with that deviation |
| cursor | TUI turn with a tool call | pass |
| grok | TUI turn with a tool call | pass |
| cursor | `--continue` | pass |
| grok | `--continue` | pass |
| cursor | cancelled turn (Esc mid-turn) | pass — `prompt_end` `stopReason: cancelled` |
| cursor | errored turn (SIGKILL the agent mid-turn) | pass — `errClass: closed`, `acp: connection closed` |
| cursor | output-heavy turn | pass |
| grok | output-heavy turn | pass |
| cursor | stub binary writing only stderr | pass — `start_failed` and three `agent_stderr` notes |
| cursor | `--plan --no-force` header fields | pass |
| cursor | `CRAZE_JOURNAL=0` | pass — no file written, `seq` still on stdout |

All 24 files craze wrote during the smoke hold the shape invariants: `header`
first, `diag{closing}` last, **no `gap` line anywhere**, event `seq` contiguous
from 1, `droppedAtClose` 0, modes 0600 in a 0700 directory, and the file name's
UUID equal to the header's incarnation. `craze prompt --json`'s `seq` values are
a subset of the journal's on all five headless runs.

The **native tool call** was re-run on 2026-09-20, once H2's tools were on
main and merged into this branch, and it passes: `craze prompt --provider
native --model openrouter/gemini-3.8-flash` over a three-file workspace called
`glob` and answered from what it read, and its journal holds a header, a
session note, the prompt, seven events numbered 1–7 with the three tool events
among them, a `prompt_end`, and `closing`. Re-run from merged main
(`ee94a788`), `fireworks/kimi-k3` and `glm-5.3-flash` (zai-coding-plan) each
complete the same task too, calling `bash` and answering from what they read.

A first pass of that re-run reported both of those providers refusing H2's
tool schema with an HTTP 400, and **that was wrong**: the binary predated
`cd4ff42`, the harness's schema-number fix, which was not yet in the tree the
smoke was built from. Before it, every schema number was a `json.Number` —
a string underneath — and the provider SDK's own encoder wrote it quoted, so
the provider rejected it. The literal value each validator names is whichever
property it reaches first, which is why the message differed between runs and
why zai's vaguer wording is the same bug. The harness session reproduced both
sides to establish this. It is recorded here because a smoke report is only
worth what its build provenance is: rebuild before believing a provider
failure.

Those failed runs are still evidence for the error path, which is what S1a
owns: each journaled the `error` event **and** a `prompt_end` carrying the
class and the provider's message.

Not covered by the Linux smoke, and why: `agent_exit_status` (a SIGKILL closes the pipe first,
so the class is `closed`); a prompt **after** a resume (the `--continue` runs
deliberately sent none, to keep the resumed file a pure measure of replay cost);
`gx` (not in the brief); a grok 402. There was no `agent_stderr` on the
SIGKILLed cursor because it had written nothing — hence the stub-binary run.

One finding worth carrying forward: **a note's `ts` is not monotonic across
notes** (the stub run's `agent_stderr` notes carry `ts` ~70 µs earlier than the
`start_failed` line that precedes them). File position is the contract, as §3.4
says; a reader that sorts notes by `ts` will be wrong.

#### mac-mini — run at `1dfc551` (three commits after the Linux leg)

macOS 26 arm64, clone `~/projects.bak/craze`. Artifacts:
`020-session-control-s1a-event-log/smoke/macos/`.

| provider | scenario | result |
|---|---|---|
| grok | headless turn with a real tool call | pass |
| grok | TUI turn with a real tool call | pass |
| grok | `--continue` | pass |
| native | headless turn | pass, no tool call possible (H1 has no tools) |
| native | TUI turn | pass |
| cursor | any live turn | **skipped**: `Error: Your macOS login keychain is locked.` — recorded, not worked around |
| — | `prompt --json` stdout `seq` ⊆ journal `seq` | pass (grok 20 lines, native 14, none missing) |
| — | modes, `CRAZE_HOME` redirect, `CRAZE_JOURNAL=0`, V6 | pass; `config.toml` byte-identical here |

All seven journals: `header` first with `os: darwin` and `arch: arm64`, a
`session` note, `prompt` and `prompt_end`, `seq` contiguous from 1, no `gap`
line, `closing` last with `droppedAtClose: 0`, files 0600 in directories craze
created at 0700 under umask 022. The resume matched Linux exactly: `loadedFrom`
equal to the previous `providerSessionId`, seven `replayed: true` events inside
the bracket, and the first incarnation's file byte-identical afterwards.

Two macOS facts a journal reader needs. Timestamps are microsecond-resolution
on darwin (nanosecond on Linux), so ties are likelier and file position stays
the contract. And `ts` must never be compared as a string: RFC3339Nano trims
trailing zeros, so `.9701Z` sorts after `.970164Z` lexicographically while
being earlier in time — compared as time, all seven files are strictly
monotonic in file order. The Linux leg's out-of-order note timestamps did not
reproduce here.

The cursor skip paid for itself: it exercised the stderr tee against a **real**
agent, which the Linux leg could only do with a stub binary — two
`agent_stderr` notes with their ANSI intact and correctly escaped, then
`start_failed{errClass: "closed"}` and `closing`, with no `session` or `prompt`
note, because the session never started. A second `start_failed` class
(`other`, "native: no models configured") came free from the `CRAZE_HOME` run.

The full gate did **not** run green in that clone, and the part that did is
worth separating from the part that was blocked. Green: `make build`,
`make test-race` (all 13 packages), and `go test` over every tracked package —
and the known darwin-only `TestPTYAltScreenAndCtrlDQuit` failure did not
reproduce at this commit. Blocked: `make test` and `make lint` both exit
non-zero there, for a reason that is not the branch — an untracked, gitignored
`scratch/` left from the plan-018 spike has a missing `go.sum` entry, and
`./...` walks into it. That clone needs tidying before its gate means anything;
CI's own macos-latest leg runs the same targets green on this branch.

### Measurements

Inputs to **SQ3** (retention) and **SQ15** (tool-event conflation). Neither is
decided here; these are the numbers the decision needs.

**Journal size per session** (V3; full per-`eventType` breakdowns in the smoke
folder's `measurements.txt`):

| journal | turn | total B | events | largest line B | tool share |
|---|---|---|---|---|---|
| cursor headless | `ls`, 1 tool call | 6 066 | 18 | 855 | 27.3% |
| grok headless | `ls`, 1 tool call | 53 022 | 253 | 751 | 5.7% |
| native headless | text only (no tools in H1) | 27 702 | 135 | 324 | 0% |
| cursor cancelled | 13 tool calls, cancelled at 5 s | 54 799 | 65 | 9 120 | 92.3% |
| cursor, run to completion | "read every file under `internal/agent`", 50.6 s | 224 152 | 374 | 9 122 | 75.8% |
| grok output-heavy | `find /usr -maxdepth 3` | 63 562 | 187 | 2 638 | 44.2% |

Fixed per-session overhead (header + session + prompt + prompt_end + closing) is
about 890 B. **The number to design retention around is 224 KB, not 6 KB.**

**Tool re-emit cost** (R4 / SQ15): every tool update re-emits the whole merged
tool object, about **4 lines per tool id**. Against a floor that kept only each
id's final state, that costs **1.6x–6.7x** — a steady ~1.7x on cursor's big
turns (early `pending`/`in_progress` copies are small beside the completed one)
and up to 6.7x on grok, which streams a growing `contentText` through many
`in_progress` updates.

**The cost of one `--continue`**: 3 069 B on cursor and 3 127 B on grok — about
79% of a resume-only incarnation's whole file, and **set by the agent's
compacted replay, not by the first file's size**. grok's 223-event, 46 KB
session replays into the same ~3 KB as cursor's 19-event, 6 KB one. A chain of
N resumes costs roughly `N × (3 KB + that incarnation's own new events)`, not
`N ×` the previous file.

**Neither agent puts large tool output on the wire** (SQ15's shape): a command
printing 499 KB became 4.7 KB of cursor tool events (cursor spills to a file and
sends `{"exitCode":0}`) and 28 KB of grok's (grok sends a truncated tail). The
largest line seen anywhere in 24 files is 9 124 B, far under `MaxRecordBytes`.
So the journal grows with the **number** of tool updates, not with one noisy
command — R4's worry is real, but its shape is "many medium lines".

**Publish overhead** (V4, at `719dc01`, 32-core Linux, `-benchtime=2000x
-count=3`):

| case | ns/op | B/op | allocs/op | journal drops/op |
|---|---|---|---|---|
| text delta, no journal | 774–863 | ~291 | 2 | — |
| text delta, journal | 872–1 235 | ~980 | 5–7 | 0 |
| large tool, no journal | 19.5k–25.4k | ~27.7k | 5 | — |
| large tool, journal | 22.3k–28.1k | ~41–46k | 7 | 0.11–0.30 |

**The plan's budget — a text delta under 5 µs with the journal attached — is
met** at about 0.9–1.2 µs. The large-tool drops are an artifact of the
benchmark, which is a tight publish loop of ~40 KB events with no agent in the
way: the writer sheds into `gap` lines rather than slowing `Publish`, which is
the designed behaviour (SD-21). No live smoke journal contains a `gap` line.
That the shedding threshold is reachable at all is another SQ15 input.

### V5 — `go test -race -count=20 ./internal/agent/... ./internal/journal/...`

`internal/journal` passed 20/20. `internal/agent` failed once, in
`TestInterjectLandsAsAUserEventFromTheBroadcast`, and the package then hit Go's
default 10-minute timeout (not a defect: twenty iterations of the package
exceed it, and `make test-race` runs `-count=1 -timeout 5m`).

**The failing test is pre-existing and untouched by this branch.** It waits on
`waitUntil`, a 10 s wall-clock deadline polling every 10 ms, for a scripted
grok turn's reply; the V5 run shared a 32-core box with a live smoke driving two
real agent processes and another full gate. Evidence that the branch is not the
cause:

- in isolation on this tree, `-count=20 -run TestInterjectLandsAsAUserEventFromTheBroadcast`
  passes 20/20 (25.7 s);
- under 24 deliberate busy loops, the same test passes at **base** (`origin/main`
  in a scratch worktree, 25.963 s) and at the branch **tip** (26.171 s) —
  `loadtest.sh`, `loadtest-base.txt`, `loadtest-tip.txt` in the execution
  session's scratchpad;
- the whole `internal/agent` package under `-race -count=10` passes with zero
  failures at both base and tip (325.6 s and 363.9 s; `pkgloop.sh`,
  `pkgloop-base.txt`, `pkgloop-tip.txt`, `pkgloop-summary.txt` beside those);
- the branch adds about 1 µs of encoding per event (V4), which cannot account
  for a 10 s miss.

**This is a diagnosis, not a retry**
(memory note `diagnose-flakes-never-retry`): the test is load-sensitive because
it bounds a scripted multi-step turn with wall-clock, and the fix — widening
that bound for the scripted-turn cases, or replacing the wait with a barrier —
belongs to whoever next touches `session_queue_test.go`. It is recorded here so
it is not rediscovered as "S1a made the agent tests flaky".

### Decisions and questions touched

- **SD-32** (recorded with the plan, before execution) held under
  implementation: cancellable admission, encode before admission, the sequence
  number in the envelope, the journal fed directly, pinned ring records at the
  cutoff, one delivery owner per subscription. Nothing in X1–X18 reopens it.
- **SQ1** (journal location and naming) is settled as its default said, and the
  pairing with the harness store is now enforced rather than intended:
  `TestSlugMatchesTheHarnessStoreByteForByte` runs both copies of the algorithm
  over shared fixtures.
- **SQ3** (retention, size caps, secrets): the secrets half is implemented as
  its default said — 0600 in a 0700 tree, no redaction, a `journal = false`
  opt-out that **fails closed**, nothing uploaded, and the native model store
  untouched. The retention half now has numbers (above) and is **still open**:
  craze prunes nothing.
- **SQ15** (tool-event conflation) now has its measurement: ~4 lines per tool
  id, 1.6x–6.7x over a last-state floor, and growth driven by the number of
  updates rather than the size of any one. **Not decided**; S1c's transcript
  model is where folding would live.
- **SQ4** (the raw wire sidecar) is untouched and stays off.

No new `SD-nn`, and no row in `10` is rewritten as resolved: SQ1's default was
simply followed, and SQ3 and SQ15 gained data, not answers.

### Handoff

What S1b inherits:

- **`agent.EventLog`**, the one ordering boundary, with `Publish`,
  `TryPublish`, `Note`, `Subscribe` and a bounded `Close`; `agent.EventSource`
  as the optional interface every `Session` in the repo implements. S1b's
  engine is a consumer of this, not a replacement for it.
- **The lossless codec.** From now on, any new field on `agent.Event` must
  round-trip it **in the same PR** — the completeness test enforces this and
  will fail on an unmapped field. That applies to the harness track too (`11`).
- **The journal-only `Note` path**, which is what `04` promised the harness for
  its diagnostics. It exists and is used by craze's own notes; the native
  adapter starts feeding `harness.Event` diagnostics into it once Plan 019's
  fields exist, whichever side lands second.
- **`seq` on `craze prompt --json`** (documented in `docs/reference/cli.md`),
  so H6's child processes are born speaking the sequenced schema.
- **`journalDir()`** in `internal/cli`, the single place that decides whether a
  run journals, and `TestMain` in that package guarding against a test writing
  into a developer's real home.

Known limitations, none of them accidental:

- **No retention or pruning.** Journals accumulate. SQ3's remaining half.
- **No wire sidecar** (SQ4), **no snapshot or attach protocol** (S1c, S2), and
  no `craze journal` mining subcommands.
- **`diag` kinds are a subset** of what `04` first listed; that file now names
  what is missing and who should pick each up.
- **The joint `-race` test with Plan 019** is owed by whoever lands the second
  change to `native.go`'s emit path; `11` states the rule and the current state
  of both branches.
- **The native tool-call smoke leg** was re-run on 2026-09-20 with H2's tools
  merged in and passes on `openrouter/gemini-3.8-flash`, and again from merged
  main on `fireworks/kimi-k3` and `glm-5.3-flash`. The HTTP 400s an earlier
  pass reported were a stale binary, not a live limitation (above).
- **`TestInterjectLandsAsAUserEventFromTheBroadcast`** bounds a scripted turn
  with wall-clock and should become a barrier.
- **Note `ts` is not monotonic**; file position is the ordering contract. Any
  later reader or `craze journal` subcommand must not sort by `ts`.
- **Two queue hazards S1a does *not* fix, and S1b's driver should.** The
  craze-harness session raised both (found by the codex review of Plan 019's
  interject commit) on the understanding that the ordering boundary would
  absorb them. It does not: the boundary orders publishes against each other
  and changes nothing about who holds which lock while publishing.
  1. `queueTx` still takes `emitMu` and holds it across its emits, and the
     primary send still blocks, because that is the pinned "emitted means
     buffered" contract. So the schedule stands: with the primary nearly full,
     a consumer receives one event and calls `Queue` synchronously; a
     multi-event transaction fills the freed slot, blocks on its next send
     while holding `emitMu`, and `Queue` waits for `emitMu`, so nothing drains
     and only `Close` releases it. The error path's `ClearQueue` emits one
     removal per row, and rows accumulate across turns because the requeue is
     cap-exempt. `TryPublish` is not the answer: queue events are lossless by
     contract, and a dropped removal would leave a row the stream never
     accounts for.
  2. The native adapter's `closed` check is not atomic with the queue
     mutation, so `closed` can read false, `Close` can then set it, and a later
     push leaves a row `Snapshot` still shows after `Close` while its event is
     suppressed.
  Both are the same shape: queue state and the events describing it are
  mutated under one lock and published under another, so a client cannot
  rebuild the queue from the stream across a close or a full primary. That is
  exactly what SD-24's engine turn driver is for.

No follow-up issues were filed: the list above and `04`'s table of unshipped
`diag` kinds are the record, and each names the phase that should pick it up.

## S1b — the engine turn driver, asks, settings, identity

| | |
|---|---|
| Status | shipped |
| Plan | `021-session-control-s1b-engine` (outside the repo, `~/.claude/plans/craze/`) |
| Baseline | `6581e0a` |
| Branch / PRs | three sequential PRs, each from fresh `origin/main`: `feature/plan-021-s1b-driver` (#41), `feature/plan-021-s1b-asks` (#42), `feature/plan-021-s1b-state` (#46) |
| Merged | PR 1 2026-09-20, `fdaaa6d`; PR 2 2026-09-20/21, `db7686e`; PR 3 2026-09-21, the merge commit of #46 |

### Plan review — 2026-09-20

| reviewer | result |
|---|---|
| Codex, `gpt-6-astra`, high effort | 27 findings (4 blocker) |
| CodeRabbit | 25 findings (1 blocker) |
| GLM 5.3 | 13 findings (2 blocker) |
| `craze-harness` session | 7 findings, on the provider seam |

The reviewers found the same structural holes independently: a cancel that
lands before `Begin` claims a turn; `Close` racing what is still in the
outbox; how far the cancel hold has to reach (every admission path, not just
the drain); `craze prompt`'s chain rule needing to run inside settlement
rather than after it; and delta/mutation atomicity for settings and turn
endings (astra and CodeRabbit). Because independent reviewers kept finding
the same holes, the plan pins the fixes rather than leaving them to the
executor: `Submit` never calls `Session.Cancel`, and `Begin` runs under `e.mu`
in the same section that reserves the turn; `Close` gains phases, so what is
left in the outbox at shutdown is still committed to the ring, the journal
and every subscription with a non-blocking primary send; the cancel hold is
reserved in the same critical section that validates the cancel and covers
every admission path; the chain policy runs inside the settlement
transaction, where `craze prompt`'s rule has to see it; and a turn's
settlement, and every settings delta, is one `Enqueue`d batch under the
owning lock, so state order is event order. Raw reviews in
`021-session-control-s1b-engine/panel/`.

### Roadmap wording this plan departs from

Per the plan's §4 last bullet, recorded here and, on merge, in `05` at C14:
the ask registry lives below the seam, in `internal/agent` beside `EventLog`;
the settings revision is the event's `Seq`; SD-30's shared mode / model /
title *rows* ship as state deltas with no TUI rendering in S1b (how a remote
client words someone else's change is S2's); command ids ship with an
in-memory receipts table and no wire; and `NoPrimary` arrives a phase early,
in S1b, for tests, rather than waiting for S4.

### Outcome

Shipped in three PRs, each branched from a freshly fetched `origin/main` and
gated per commit. `internal/engine` now owns the turn driver, the queue and
send-now (PR 1, `feature/plan-021-s1b-driver`, #41 `fdaaa6d`); the ask
registry (PR 2, `feature/plan-021-s1b-asks`, #42 `db7686e`); and settings as
state deltas, command ids and the durable session id (PR 3,
`feature/plan-021-s1b-state`, #46). The TUI and `craze prompt` are both
engine clients now; `tui.Model` and `internal/cli/prompt.go` no longer drive a
turn. It ships no feature: frame goldens are byte-identical at every commit,
no `testdata/` file changed except additions, and `craze prompt --json` is
byte-identical apart from `seq` — **V2: 103/103 SAME** at the PR 3 tip
(`9a00f32`, `021-session-control-s1b-engine/v2/run_pr3_final`).

**The roadmap's S1b exit** (`07`):

| criterion | result | evidence |
|---|---|---|
| two in-process clients on one session cannot double-drain the queue | pass | `TestTwoClientsSubmittingAndQueueingAtOnceAgreeOnEveryTurn` (`internal/engine/exit_test.go`), `TestTwoClientsSubmittingAtOnceStartEveryRowOnce` (`internal/engine/lifecycle_test.go`) |
| a session with no client drains its own queue and parks asks | pass | `TestASessionWithNoClientRunsEveryTurnAndParksAnAskUntilItIsAnswered` (`internal/engine/exit_test.go`), `TestASessionWithNoClientDrainsItself` (`internal/engine/driver_test.go`) |
| an ask answered twice yields one resolution and one `already_resolved`, and an invalid answer leaves it open | pass | `TestAnAskAnsweredTwiceThroughControlEndsOnce`, `TestAnInvalidAnswerThroughControlLeavesTheAskAnswerable`, `TestTwoClientsRacingOneAnswerLeaveOneEnding` (`internal/engine/exit_asks_test.go`); registry level `TestAskAnsweredOnceEndsOnce`, `TestAskBadAnswerLeavesItOpen`, `TestAskRacingAnswersLeaveOneEnding` (`internal/agent/asks_test.go`) |
| every ask ending is a sequenced event | pass | the A-X4 matrix: `TestACancelWhileAnAskIsParkedEndsItOnce`, `TestCloseWhileAnAskIsParkedEndsItOnce`, `TestAnAskOpenedBetweenTurnsSurvivesAWholeTurn`, `TestATurnThatEndsWithAnAskOpenEndsItOnce` (`internal/engine/exit_asks_test.go`, `exit_test.go`), plus the ACP-answered-early / `Force` / non-interactive / lost-reply / room-refused / call-context rows in `internal/agent/asks_test.go` and `asks_live_test.go` |
| a cancel names its turn and cannot hit the next one | pass | `TestACancelNamingAStaleTurnIsRefused`, `TestACancelHeldBeforeTheSessionAdmitsNothing`, `TestOverlappingCancelsEachHoldTheirOwn` (`internal/engine/cancel_test.go`, `cancel_schedules_test.go`) |
| two concurrent settings changes converge on every client | pass | `TestTwoClientsTwoSetsAndAProviderUpdateAgreeOnTheLastDelta` (`internal/engine/exit_test.go`); `TestTheLastDeltaWinsInBothForcedOrders`, `TestForcedStateOrderForTheTitle`, `TestForcedStateOrderForTheMode`, `TestStartAndLoadFoldToTheSnapshot` (`internal/engine/settings_test.go`, `internal/agent/settings_test.go`) |
| golden files byte-identical | pass | `git diff --stat 6581e0a..9a00f32 -- '*testdata*'` touches only additions; no frame golden edited at any commit |
| `craze prompt`'s own driver moves into the engine, and its `--json` fixtures and tests are byte-identical apart from `seq` | pass | V2 103/103 SAME (above); `internal/cli/json_test.go` and `internal/cli/ask_json_test.go` unedited apart from the one listed exception (A-X8: `TestSignalBetweenTheTakeAndTheSendRemovesTheRow` removed, replaced by `TestSignalWithARowQueuedRemovesIt`, same content); `tests/cli/**` (147 pytest cases) unedited |

**The plan's other acceptance criteria** (§7), where `07`'s exit wording
doesn't already name a test:

| # | result | evidence |
|---|---|---|
| A7 (status half): no idle across a cancelled turn with a queued row, or before a fired send-now | pass | `TestHostStatusNoIdleAcrossACancelledTurnWithAQueuedRow`, `TestHostStatusNoIdleBeforeAFiredSendNow` (`internal/tui/host_test.go`) |
| A14: a delayed `/model` failure does not undo a newer confirmed model | pass | `TestADelayedModelRefusalDoesNotUndoANewerChange`, `TestACompletedStepOlderThanAnAppliedDeltaIsNotWritten`, `TestAFallbackCompletionOlderThanAnotherClientsChangeIsNotWritten`, `TestAZeroRevisionSuccessIsWrittenUnlessSomethingNewerWas`, `TestMayApply` (`internal/tui/settings_test.go`) |
| A15: `crazeId` on all three load paths; native unindexed; a failed seed retries; a stalled index write blocks no `Control` method | pass | `TestTheCrazeSessionIDTravelsEveryConstructionPath`, `TestContinueCarriesTheRowsCrazeID`, `TestResumeRowsCarryTheirCrazeIDs`, `TestAHiddenProvidersSessionIsNeverIndexed`, `TestADrainedFirstPromptIsSeededByTheWorker`, `TestAStalledIndexWriteBlocksNoControlMethod`, `TestBothKindsOfWriteContendOnARealFileLock` (`internal/tui/engine_seam_test.go`, `internal/cli/resume_test.go`, `internal/engine/index_test.go`) |
| A16: command ids never collide, replay a resend, reject a changed payload, expire past the horizon | pass | `TestTwoClientsBothNumberingFromOneNeverCollide` and the rest of `internal/engine/receipts_test.go`; `TestClassifyIsTheOneTable` (`internal/engine/classify_test.go`) |
| A17: the engine's `State` derives the same `host.Status` as the TUI's mirror | pass | `TestEngineStateDerivesTheSameHostStatusAsTheTUIsMirror`, `TestInputFromStateReadsEveryFieldDeriveDoes` (`internal/tui/activity_parity_test.go`) |
| A20: a craze-initiated change fills neither `Mode` nor `Text` | pass | `TestAPlanOfferSurvivesAModeClickMadeJustBeforeTheTurn`, `TestTheInstallDeltaIsCrazesOwn` (`internal/tui/settings_test.go`, `internal/agent/settings_test.go`); `internal/cli/settings_json_test.go` unedited |
| smoke A1 (found live, fixed in `f65c6c6`): a turn current at `Close` gets its `ended` | pass | `TestCloseEndsTheTurnOnTheRecord` (`internal/engine/lifecycle_test.go`), `TestClosingTheEngineEndsTheHungTurnOnTheRecord` (`internal/engine/engine_stub_test.go`) |

Per-commit gate (`make lint && make test && make test-race && make build &&
make test-cli`) green at every one of the three PRs' commits, re-run by the
orchestrator as well as each implementer; 147 pytest CLI tests unedited.
`-cpu=1 -count=2` on the changed packages before every push-worthy commit from
PR 3 on (a lesson PR 2's CI found, X45). `internal/engine` is in `test-race`
alongside `internal/agent`. Ten external review rounds on PR 3 alone
(`reviews/r23`–`r32`: astra on C10, sol on the rest), 36 findings, every one
accepted and fixed or recorded; PR 1 fifteen rounds, PR 2 seven — see the two
PR bodies for the full tables. CodeRabbit reviewed each PR's first head for
real and found nothing actionable beyond what PR 1's four findings already
list; it was rate-limited on every fix commit after and, per the owner's rule
(`coderabbit-rate-limit`), was not waited for. Greptile posted nothing on any
PR.

### What shipped per commit

**PR 1 — `feature/plan-021-s1b-driver`** (#41, `fdaaa6d`): C1 (roadmap docs,
SD-33); T1/T2 (driver tests re-expressed against observable behaviour while
the old drivers still passed them, X1–X2); C2 (`EventLog`'s outbox — `Enqueue`,
`OutboxRoom`, `Flush`, the close phases, `Observe`, `NoPrimary`, X4–X7); C3a
(`EventTurn`, `Event.Cause`, `Session.Cancel`'s outcome, `ForeignTurn()`, the
depguard rule); C3b (`internal/engine`: `Control`, `State`, `Command`,
admission with `Begin` under `e.mu`, the settlement transaction, the chain
policy, `Cancel`/`Stop` with the hold, `Sync`, X8–X17); C3c (the queue verbs,
the requeue rules, send-now); C4 (the TUI drives its session through the
engine, X18, X22, X25); C5 (`craze prompt` is an engine client — `GiveUp`,
`GiveUpDrain`, `State.Waiting`, `TurnErr`, X24, X26, X28, X31–X34); C6 (the
queue leaves the provider seam, −1,370 lines, X27, X29).

**PR 2 — `feature/plan-021-s1b-asks`** (#42, `db7686e`): C7 (`asks.go`: the ask
registry, tokens, `Open`/`Answer`, `EventAsk`, `ApprovalPolicy`, X37); C8a
(ACP's `EarlyAnswer` and `ReplyDisposition`, X38); C8b (every ask parked in the
registry — `acp.Arrival`/`TurnActive` replace the turn-counter mapping, the
hidden `perm-xN` ids, the turn-keyed cancel mask, X39–X44).

**PR 3 — `feature/plan-021-s1b-state`** (#46): C10 (settings as state
deltas, the settings worker, `EventLog.EnqueueTicket`, X47–X48); C11 (command
ids, the receipts table, `classify`, X49); C12 (the durable `crazeId`, the
index worker, `ErrIndexWrite`, X53); C13 (§7's A-X1..A-X6 as integration
tests, the activity-parity test); three fix commits from the live smoke and
its review (`fd46f0b`, `f28a245`, `f65c6c6` — the index worker's write
ordering, a context error after the write, and `Close` ending the running
turn on the record, X54–X55); `9a00f32` (`test(engine)`, close/cancel/Stop
schedules, no production change); **C14** (this commit).

### Deviations from the plan

Numbered and grouped, mirroring the plan's execution amendments X1–X56; the
full text and every failing schedule are in the plan.

1. **`Close`'s phases run in a different order than §3.3 lists, and the outbox
   is made immune to the cutoff rather than sequenced before it** (X4): cut
   the outbox → `close(l.closed)` → join the drainer → the existing teardown.
   A `Flush` parked when `Close` begins is answered nil, not `ErrLogClosing`,
   because the at-close path commits rather than abandons (X5). Smaller
   decisions: a batch stays contiguous in `Seq` against direct publishers too;
   an `Enqueue` after the cut is `droppedAtClose`; `Stub.NoPrimary` is fixed at
   construction (X6); an enqueued event may not carry `Err` — a non-nil one is
   replaced by an inert sentinel, counted in `Health.OutboxErrReplaced` (X7).
2. **Admission and cancel, decided during C3b/C3c** (X8–X17, X19): a cancel
   with no turn of craze's own was accepted at first (PR 1 could not see
   asks yet) and tightened to §3.7's `not_accepting` rule once PR 2 gave it
   `PendingAsks` (X9, closed by X40); `CancelResult.Outcome` is decided from
   the engine's own state after the hold releases, not from
   `CancelOutcome.Settled`; `Stop` flushes the outbox between clearing the
   queue and cancelling, found by a test that failed one run in thirty (X10);
   the foreign-turn retry is paced by a tick alone, not by any wake-up (X13);
   the seam's promise that `Cancel` is bounded by its context is made true in
   `live.go` — the write moves to its own goroutine (X16) — and a late helper
   could otherwise cancel the next turn's ask, fixed with `acp.Client.Held` /
   `CancelHeld`, the one edit PR 1 makes to `internal/acp` (X19).
3. **`Control.Subscribe` blocks** (X14): it registers inside the log's
   publishing boundary, so the primary's own reader calling it with the
   primary full would wait for a slot only it can free. Not in §3.2's list
   either way; found first as a hang in the engine's own saturation test.
4. **The TUI on the engine** (C4, X18, X22, X25): `Engine.Started(err)` opens
   the gate the ~45 unit tests that inject a `startedMsg` need;
   `Snapshot.Queue` stays the model's one queue mirror until C6; two model-lags-the-engine
   defects from review — a `Submit` result applied early, and a cancel's
   failure reported twice — fixed so a turn `Submit` hands back while another
   is on screen is remembered as the pending successor rather than applied at
   once, and each cancel's failure has exactly one reporter; a failed cancel
   behind an armed send still draws its error row, from `StateDelta.Detail`.
5. **`craze prompt`'s foreign-turn budget could not be done with `Stop`**
   (X24, X26, X28, X31–X34): the plan's "the client owns the budget and calls
   `Stop`" is `State.Waiting` + `Control.GiveUp(c, turn)` for a claim refused
   outright, and `Control.GiveUpDrain(c)` for rows held behind a foreign turn
   — both conditional, atomic, and waiting on nothing. Four review rounds
   (r9, r11–r14) each found a schedule the previous fix left open (an
   unconditional `Stop` could cancel a re-claim that had just succeeded; a
   timed grace could not order the client against the engine's driver); the
   final shape needs no wall-clock timing at all. **V2 at the final PR 1 fix:
   103/103.**
6. **The queue left the provider seam** (C6, X27): −1,370 lines; `queueOp` and
   `emitMu` went with it, which is how S1a's hazard 1 closes.
7. **PR 2's ask-to-turn mapping could not be done from ACP's own counter**
   (X39, the one design change a review round forced): a request registered
   during a turn whose handler is delayed past that turn's end, and one
   registered between turns, both carry the same counter value and are not
   distinguishable from session state alone. `acp.Arrival{Turn, InTurn}`,
   captured at registration, replaces the plain `turn int` on every ACP
   handler. Ids gained a second, hidden counter (`perm-xN` / `ask-xN` /
   `plan-xN`, X41): an ask whose opening is never published (a refused
   `Open`, `Force`, an automatic resolution) must not spend a *visible*
   number, or `--json`'s next real question renumbers. `Control.Ask(id)` was
   added in the simplify pass (X44) as `05`'s `asks.get`.
8. **Settings, the install and its merge rule** (C10, X47–X51): `Set` /
   `SetTitle` gained the cause parameter and a FIFO settings worker;
   `EventLog.EnqueueTicket` is how a `Set` learns its own delta's `Seq` as
   `Rev`; start-up and a `session/load` now enqueue their own install delta
   (an addition the plan did not ask for — X48 found a folding client could
   otherwise end permanently different from `Snapshot()`); an arm that races
   the install keeps its section and the install publishes ONE merged delta
   (X51); a `Set` whose caller's context ends after the worker has claimed it
   is `ErrSetOutcomeUnknown`, never a silent "nothing happened" (X50). Native's
   first-prompt title still publishes nothing — the plan's brief assumed it
   already did (X47) — recorded for S1c below.
9. **Receipts as built** (C11, X49, X52): the table's mutex is a leaf, held
   only to admit and to finish a command, never across one — C12's index I/O
   inside `Submit`/`SetTitle` would otherwise block every client's commands
   (r24). No minted client is ever retired, only its completed commands (r26
   overturned r24's first fix, which was found to revoke a live client). The
   stored-or-forgotten invariant — `unavailable` / `not_accepting` /
   `in_progress` ⇔ never stored, everything else stored — is now one switch,
   `classify`, that both `Code` and the table's gate consult, so the two
   cannot again say different things about one error (r28). New codes:
   `in_progress`, `aborted`, `failed`, beside `unknown_row` / `unknown_command`
   / `index_write`.
10. **Identity and the index** (C12, X53–X54): `crazeId` is first-one-wins — an
    empty stored id is filled, a differing incoming one dropped; the index
    worker's slot merges touch/load/seed under a leaf mutex with a one-slot
    kick, all bounded by one 500 ms close bound (the journal's own), because
    `flock` cannot be interrupted; `/rename`'s write failure is
    `ErrIndexWrite`, stored, because the rename happened whatever the file
    did.
11. **`Close` ends the running turn `closing`** (found live, V1, fixed in
    `f65c6c6`; see "Live smoke" below): the plan's doc comment for `Close`
    promised an ending for the turn that was running, and the first cut did
    not deliver one.
12. **Roadmap wording this plan departs from** (§4's last bullet, also
    recorded in `05` and `03` by this commit): the ask registry lives below
    the provider seam, in `internal/agent`, not above it; the settings
    revision is the event's `Seq`; SD-30's shared mode/model/title rows ship
    as state deltas with no TUI rendering (how a *remote* client words
    someone else's change is S2's); command ids ship with an in-memory
    receipts table and no wire; `NoPrimary` arrives a phase early, in S1b for
    tests, rather than waiting for S4.
13. **Recorded behaviour changes** — no user-visible change except these
    seven, all deliberate:
    - An invalid permission answer no longer cancels the request (§4).
    - A card whose turn ended is removed (§4).
    - Native's steer requeue follows `done` instead of preceding it (§4).
    - A `Cancel` with nothing to cancel (no turn, no pending ask, no foreign
      turn) is refused `not_accepting` rather than written (PR 2, X9/X40).
    - Enter pressed while the session knows of a foreign turn the model has
      not yet seen is now **queued** (removable, drains when the foreign turn
      ends) where the baseline claimed it, drew it, and let Esc withdraw it
      (X22's fourth change — the model's own admission-gate rule, not a
      defect).
    - A request of turn N whose handler starts only after ACP has settled N
      but before the session released it is now refused `turn_ended` (the
      agent is told cancelled), where the baseline's counter equality would
      have parked it (X43, sol r19 judged this the safe result).
    - `--json`'s one CLI-authored un-numbered `queue removed` line, for a row
      a signal caught between the queue and the wire, is gone with the window
      it existed for: the row's removal is now an engine event with a `seq`
      (A-X8, the plan's one listed exception).
14. **Known and recorded, not fixed** — each a genuine finding, deliberately
    left rather than chased:
    - A handler that loses the race to `EndTurn` enqueues its refused-`Open`
      ending after `EventDone`, and an early-answer goroutine that runs after
      the log has closed writes nothing: both are card-less records for
      requests the agent already has its reply to, and closing them needs
      request accounting the close phases exist to avoid (X42 finding 6).
    - An ABA on a re-used ADOPTED id under the TUI's cancel mask — production
      ids are minted and never re-used within an incarnation, so this is test
      Stub territory only (X43).
    - The id-adoption subsystem in `asks.go` (~265 lines) exists solely
      because `tui.Stub` adopts test-chosen ids at ~40 sites; worth revisiting
      once S1c gives the Stub a reason to mint (X44).
    - `Start` dispatches an update the agent sends before its `session/new`
      reply on `Start`'s own goroutine, where it waits for the primary like
      any emit; with a full primary and no reader, `Start` wedges until the
      session closes. Pre-existing, no worse than the baseline (X50, r27).
    - The `SetModel` marker set (keeping the models that were current before
      each direct set made while no model option existed) suppresses a
      genuine return to a pre-set value on first appearance exactly once,
      indistinguishable from the stale report it exists to catch (X52).
    - The permission path has still never been reached by a live agent: six
      attempts over two providers and four kinds of action on Linux, and
      grok's config plus native's `AllowAll` gate rule it out on macOS too
      (V1/V4, "What was not exercised").
    - A live update in the gap before a `session/load`'s install delta is
      lost exactly as a contradicting replay is (X51).
    - `Model.loading` went with the touch it existed for, but `m.sess` stays
      on the TUI model — 71 test sites use it, write-only in production — a
      cheap follow-up (X53).
15. **The smoke's pre-existing findings that are NOT S1b's** (found while
    driving V1/V4, neither caused nor fixable by this branch): a pasted word
    that spells a default `textarea` key name (`end`, `up`, `left`, `ctrl+a`
    …) is silently eaten by the composer, because bubbletea v1 splits a
    burst at spaces and bubbles' keymap consumes the word as that key (V1
    anomaly A2); a grok interject-fallback foreign turn shows no spinner,
    because `spinnerVisible()` keys off `statusWorking` alone and a foreign
    turn does not set it (V1 anomaly A3, no macOS evidence either way — M1).

### Live smoke

Full records: `021-session-control-s1b-engine/smoke/{linux,macos}/RESULTS.md`,
`verification-summary.md`.

#### Linux — `f28a245` (re-checked at `f65c6c6`)

| leg | cursor | grok | native |
|---|---|---|---|
| 1 — queued follow-up drains | PASS | PASS | PASS |
| 2 — Esc mid-turn with a row queued, no idle flash | PASS | PASS | PASS |
| 3 — send-now | PASS | n/a | n/a |
| 4a — interject mid-turn | n/a | PASS | n/a |
| 4b — interject fallback holds the drain | n/a | PASS | n/a |
| 5 — unanswered interjection requeued ahead of a queued row | n/a | n/a | PASS |
| 6 — Esc right after Enter | PASS | not run | PASS |
| 7 — `--no-force` permission | NOT REACHABLE | NOT REACHABLE | n/a (`AllowAll`) |
| 8a — plan card (accept / reject / Esc) | PASS | n/a | n/a |
| 8b — question card (answer / skip) | NOT REACHABLE | PASS (answer) | PASS (answer + skip) |
| 9 — `/model` + mode cycle + `/rename` at speed | PASS | n/a | n/a |
| 10 — `--continue` keeps `crazeId` | PASS | n/a | n/a |
| 11 (re-check) — quit mid-turn (A1) | PASS | n/a | PASS |

The ask registry's first live coverage: every ask, one opening, one ending.
**Finding A1** (fixed in `f65c6c6`): quitting while a turn ran left it
`started` and never `ended` — `Close` ran the session's own close, which ends
the turn and closes the log, before the driver's settlement could publish the
ending. Fixed by having `Close` author that turn's ending itself, synthetic,
`closing`, inside the same locked section that starts shutdown, before the
session (and so the log) closes — confirmed on Linux by re-running the repro
at `f65c6c6` (native and cursor both: exactly one `ended{stopReason:closing,
synthetic:true}`, the last event in the file). `droppedAtClose` stays
non-zero on a mid-turn quit (2, unchanged before and after the fix): the
session's own late tool-completion event, published after the cut — S1a's
designed teardown, not a regression.

#### macOS (V4) — mac-mini, `f65c6c6`

| leg | grok | native |
|---|---|---|
| 1 — queued follow-up drains | PASS | PASS |
| 2 — Esc mid-turn with a row queued, no idle flash | PASS | PASS |
| 4a — interject mid-turn | PASS | n/a |
| 4b — interject fallback holds the drain | NOT REACHABLE (7 attempts) | n/a |
| 5 — unanswered interjection requeued ahead of a queued row | n/a | PASS |
| 6 — Esc right after Enter | PASS | PASS |
| 8b — question card answered / skipped | n/a (not asked) | PASS (both) |
| 9 — `/model` + mode cycle + `/rename` at speed | PASS | n/a (no modes) |
| 10 — `--continue` / native writes no row | PASS | PASS (no row) |
| 11 — quit mid-turn (A1) | PASS | PASS |

cursor is **NOT REACHABLE over ssh** (locked login keychain, as plan 020's
smoke found) and is skipped by the brief. The grok interject fallback could
not be provoked on this box's `Grok 4.7 (xhigh)`, whose turn ends within one
0.3 s frame of its last token — a property of the model and the box, not of
craze (M1); it PASSed on Linux. Two benign, box-dependent shape notes: grok's
install delta lands at seq 3 on the mac (two catalog updates precede it) where
it is seq 1 on Linux (M3); the `/rename` leg's delta lands **last** on grok
(completion order, not typed order) where it landed first on Linux/cursor
(M2) — a folding client keyed on the delta stream ends level with
`Snapshot()` either way, and a client that assumed a fixed order would be
wrong.

#### V3 — the journal record

Linux: **30 of 33 journals PASS**; the 3 failures are the deliberate
mid-turn-quit repros of finding A1, all failing the same two invariants
(unbalanced turn record, `droppedAtClose != 0`) before the fix — re-checked
clean at `f65c6c6`. macOS: **25 of 27 PASS**; the 2 failures are the same
deliberate repros, and after the fix they fail **only** `droppedAtClose == 0`
(the turn record itself passes). Across all 60 journals: zero `gap` lines,
`outboxSkippedPrimary` 0, exactly one `craze_session` diag note per file,
`seq` contiguous from 1, every `meta` carries a state delta, no
craze-initiated delta sets `mode` or `text`.

#### V2, V5, V6, V7

- **V2** (`--json` parity against the `6581e0a` baseline, 103 scenarios):
  **103/103 SAME** at the PR 3 tip (`9a00f32`). Every intermediate DIFF during
  execution was one of two documented unordered pairs
  (`sigint-between-turns`, which varies in the baseline alone; or a foreign
  turn's closing bracket against process teardown), each matching an output
  the baseline itself produces.
- **V5** (`go test -race -count=20 -timeout 60m ./internal/engine/...
  ./internal/agent/...`, a quiet box): PASS, exit 0 — engine 38 s, agent
  1,111 s, no failure, nothing retried.
- **V6** (no disruption): Linux and macOS both PASS — the user's pre-existing
  `sessions.jsonl` rows are a byte-identical prefix after the smoke, every new
  row is the smoke's own workspace with a `crazeId`, native sessions are
  correctly absent, and `config.toml` changed only in the pre-existing
  `provider` key (restored on Linux; already matching on the mac).
- **V7** (publish cost with an observer set): a text delta with the journal
  attached is 1.7–2.4 µs over five runs (plain publish 1.8–2.1 µs); budget
  5 µs, met by a wide margin
  (`021-session-control-s1b-engine/smoke/linux/v7-publish-cost.txt`).

### Decisions and questions touched

SD-33 (session hosts born detached, the TUI a socket client for good) was
recorded in `08` ahead of execution, and SQ12 resolved in `10` before the plan
was written; nothing in execution reopened either. No new `SD-nn`. The
roadmap-wording departures the plan's §4 flagged are recorded in this file's
deviations above and mirrored into `03` and `05` by this commit, per the
plan's own instruction, rather than as a new decision row: none of them
reverses a settled `SD-nn`, and each is a departure from prose that was never
itself a decision entry.

### Handoff

Everything below, and the smaller items the deviations above record as "not
fixed", is the backlog in `13` (SF-01 to SF-41) with a phase and a size per row;
plan from there. This section is the narrative.

**What S1c folds.** The delta sections (`StateDelta`'s `Title` / `Mode` /
`Model` / `Config` / `Commands` / `Plugins` / `SendNow`), `EventTurn` and
`EventAsk` are the shared facts the transcript model folds into entries; the
engine's `Observe` callback — already the seed for the driver's and the index
worker's own wake-ups — is also the fold's seed, called inside the log's
publishing boundary, once per committed event, in `Seq` order. **Native's
first-prompt title is NOT in the stream** (X47: `Start`'s own first-prompt
title publishes nothing, on purpose, because S1b ships no feature and
publishing it moved a golden) — a folding client therefore does not learn
native's self-assigned title from the event stream alone until S1c decides
whether that golden may move.

**What S2 must do.** Call `Control.Sync` before replying to a command, because
in-process a response is no longer ordered after the events its own command
caused by the call simply returning (§4, and `05` below) — that promise now
needs `Sync` plus a connection-local barrier that `Control` does not offer yet
(`Sync` returns no sequence number): S2 adds either a `Sync` that returns the
sequence it committed through, or a barrier acknowledged by the forwarding
goroutine, so that the reply reaches the connection's one outbound writer only
behind every record `Sync` committed. `Sync` alone only commits the events and
offers them to the subscription, and a serialized writer alone does not help
while the forwarding goroutine is still unscheduled (`05`). **The index, for a
client that is not the TUI:** `Close` waits (inside its 500 ms bound) for an
inline first-prompt seed still writing on a client's goroutine only when a
retry is retained behind it; a LONE inline seed in flight at `Close` is not
waited for. No client can reach that today — the TUI's `Update` is inside that
very `Submit`, so its quit cannot be processed until it returns, and `craze
prompt` has no index — but a socket server's handler goroutines can, so S2
either counts an active seed as owed in the worker's exit or keeps the index
write off connection goroutines. Related and also recorded, not fixed: an
end-of-turn touch taken while that seed is still INSIDE `Upsert` finds no row
and is dropped, so the row keeps the seed's timestamp, taken a few
milliseconds before the turn's end. Call `Subscribe` off the primary's own reader goroutine: it
blocks inside the log's publishing boundary (X14). Mint a client id per
connection (`Control.NewClientID`), bind it to that connection, and add
release-on-disconnect — no client is ever retired from the receipts table
today (X49), which is exactly right for the TUI's one process-lifetime client
and exactly wrong for a socket server that mints one per connection forever.
Specify the gate table's `failed` / `cancelling` / `closing` rows (`05`,
"to be specified in S2"). Implement the retry policy by code
(`internal/engine/control.go`'s `Command` doc, mirrored into `05` below) at
the wire, not just in process.

**§9's "Open, for later phases", unchanged by this plan:** the gate table's
`failed` / `cancelling` / `closing` rows (S2); restoring a row-sourced turn
the TUI lost to a foreign-turn refusal (today's behaviour, kept — §4);
retention (SQ3).

**The H3 note** (R8): nothing forces a `--json` rendering to exist for a new
event kind — `eventJSON`'s default silently drops an unknown kind — so H3,
which is the first phase after S1b to add a native ask, must decide and build
its own `--json` line for a native ask ending; none exists today because no
native ask has ever fired one.

## S1c — the transcript model

| | |
|---|---|
| Status | shipped |
| Plan | `024-session-control-s1c-transcript-model` (outside the repo, `~/.claude/plans/craze/`) |
| Baseline | `origin/main` `2b5229b` (harness H5 PR 2 #48 merged) |
| Branch / PRs | two sequential PRs, the second branched from `origin/main` after the first merges: `feature/plan-024-s1c-model` (#50), `feature/plan-024-s1c-tui` (#52) |
| Merged | PR 1 2026-09-24, `27c1db6`; PR 2 #52, merged 2026-09-24 (rebased onto `origin/main` `f5c3cfd`, H6 PR 1 #51) |

### Outcome

Shipped in two PRs, each gated per commit. `internal/transcript` is a
render-free package — the model, the fold, the snapshot codec, the bounds —
that every client folds. Two instances exist: the **engine's**, folded as the
first statement of `engine.observe` under a leaf `model.mu` (ahead of the
sub-agent guard, so children reach it too), the authority a snapshot is cut
from; and **each client's own** (SD-33: no client reads the engine's
instance, the TUI folds from its primary like any other client). Entries are
immutable — every mutation is a new `*Entry` — addressed by `EntryID{Seq, N}`,
no packing. Timestamps are the event's own `At`; a client's clock is only a
fallback for a zero stamp. Snapshots are bounded (`SnapshotBytes`, default
4 MiB) with continuation state (`StreamOpen`, the todo-note dedupe, a
windowed-entry ledger) and a fixed filling order: mandatory sections first
(per-item caps, `Truncated`, `ErrSnapshotTooLarge` if they alone overflow),
then the main transcript's tail newest-first, then each child the same way.
`Engine.Attach` (snapshot + cursor → live events from N+1) joins the
`Control` interface in process; S2 wraps it for the wire. PR 1 shipped the
package, the engine's fold and `Attach` (#50, `27c1db6`); PR 2 made the TUI
the first client, with its pane owning an **explicit display list** (not an
anchored overlay) so local rows and shared-entry echoes land exactly where
today's single slice put them — no golden moved except the two rows SF-01's
owner decision named (X38) and one spinner glyph a pre-existing macOS-CI
flake fix moved in two more (X39).

**The roadmap's S1c exit** (`07`):

| criterion | result | evidence |
|---|---|---|
| golden files byte-identical, with four permitted one-row exceptions (SF-01's two title rows, X38; X39's two spinner glyphs); `transcript_test.go` assertions move packages unchanged | pass | A1: PR 2's diff (`git diff --stat f5c3cfd..HEAD -- '*testdata*'`) is exactly those four files — `native-echo-80x24` row 20 and `native-mode-100x30`'s separator row (SF-01, X38, at C8), `grok-subagent-rows-{80x24,100x30}`'s frozen spinner glyph `✴` → `✳` (a pre-existing macOS-CI flake fixed test-side, X39) — every other golden byte-identical; A13: the listed assertions kept / moved / split / deleted, named in both packages at T1a and C5c, the PR bodies list every one |
| a second in-process subscriber attached mid-turn from a snapshot reproduces the first's transcript exactly | pass | A2: `TestASecondSubscriberAttachedMidTurnReproducesTheFirst`, `TestAttachOverTheFakeAgentReproducesTheFirst`, `TestAWindowedSnapshotReproducesTheSuffix` (`internal/engine/exactness_test.go`); live, PR 1's attach probe (4/4 SAME, Linux) and PR 2's V1 attach probe (3/3 SAME on Linux at the early phase and at the pre-rebase `065675e` binary (`bee8547` after the rebase), 2/2 SAME on the mac-mini) — see "Live smoke" |
| snapshot and replay memory stay inside stated byte bounds on a worst-case session | pass | A3: `TestTheModelIsBoundedOnAWorstCaseSession`, `TestSnapshotStaysInsideItsByteBudget`, `TestMandatoryStateOverTheBudgetIsRefused`, `TestAttachOverAWorstCaseSessionFitsTheSubscription`; numbers in "Measurements" below |

### What shipped per commit

**PR 1 — `feature/plan-024-s1c-model`** (#50, `27c1db6`): C0 (`5c3788e`,
roadmap docs marking S1c in progress); T1a (`9bc3e95`, the mixed
`transcript_test.go` cases split into model-fact and render-fact halves, the
old fold still authoritative and passing both); T1b (`6145e6d`, the five
`breakStream()` sites and every event-driven row take the closing event's own
`At` — X1 — no golden moved, no golden prints a duration); C1 (`97a2647` +
review fixes `6c53ce2`, `1976bc5` — the package itself: immutable entries,
`EntryID{Seq,N}`, the deque with incremental byte accounting, the capped
stream builder, all 18 event kinds and every `StateDelta` section folded, the
bounds incl. the roster rule, `Change`, the kind/section guards, convergence
and append-only history, allocation bounds, the codec's `*RemoteError`
plumbed to the fold (X14), X12's `ansi.Strip` transcription); C2 (`73d365f` +
fixes `a1e985b`, `8cc1db6` (X24, an allocation-free chunk), `aeeefda`,
`9dd830c` (X23's ledger), `5dea2e1`, `4aff2f1` (X25) — `Model.Snapshot(budget)`,
the filling order, `ErrSnapshotTooLarge`, `Restore`, the exported codec
wrappers, the windowed-entry ledger); C3 (`0d5aba4` — `e.model.Fold(ev)` as
the observer's first statement, `Engine.Attach` on `Control`, the three error
paths, `SubscribeOptions.Ctx`, the exactness test at every cut, V7 over the
engine); C4 (`ec6e749` + fixes `6bd6b79`, `16ececb`, `9c78aa2` — the hidden
`craze prompt --attach-probe`); three test-only commits at the tip
(`666a7cf`, `f55b46a`, `d7c073a`) diagnosing a V5 `-race -count=20` failure (a
test synchronisation bug, an unscheduled goroutine's missing `run` frame
since Go 1.22 — count subscription owners from their creation, not their
run), never retried; and one production fix (`026e21e`) for a CI
`test (ubuntu-latest)` failure found the same way — a ticket answering before
its whole batch committed — also never retried (memory note
`diagnose-flakes-never-retry`).

**PR 2 — `feature/plan-024-s1c-tui`** (#52, merged 2026-09-24): C5a
(`bf8272c` — the pane: a display list and a render cache beside the old
fold, the compatibility accessor, the pointer-aliasing audit); C5b
(`446dc88` — local rows and echo hiding written explicitly at the sites of
§2.4, the old fold still authoritative); C5c (`8b4620b` + fixes `624eae8`
(r16), `5c770b8` (r17/r18), `4e3df13` (r18 #2) — `m.shared` folds every
primary event, the pane consumes `Change`, the duplicate model-assertion
tests deleted, the parity watch (A11) installed package-wide); C7
(`375919e` — `m.sess` goes, SF-03, through one helper); C8 (`bee8547` +
fixes `0f7730a` (r19) — native's first-prompt `Title` delta, SF-01, X38's
two-golden move); and, after the branch rebased onto `origin/main` `f5c3cfd`
(H6 PR 1 #51), one further test-only commit (`83e58e3`, H6's own diagnosis —
X39: `grok-subagent-hold` plus a wait on the tool row before the tokens,
moving `grok-subagent-rows-{80x24,100x30}` by one spinner glyph). This
docs commit (C9) is `c6f17ec`, landed before the rebase pulled `83e58e3` on
top of it. The SHAs above are the rebased history; the pre-rebase SHAs
(`1a72d21`, `02f45ac`, `aeaa59d`, `ef0afb9`, `a51ff56`, `ec4aa17`,
`d53a82a`, `065675e`, `f20bbab`, `abeaed9`) name the same commits' content
and are what the smoke and soak artifacts below were built from. After the
rebase, the full gate (`make lint && make test && make test-race && make
build && make test-cli`), `go test -cpu=1 -count=2` on
`./internal/{tui,transcript,agent}/...`, and `go test -race -count=5
./internal/tui/...` all passed at `83e58e3`.

### Plan review — 2026-09-21

| reviewer | result |
|---|---|
| Codex, `gpt-6-astra`, high effort | 28 findings (10 blocker) |
| CodeRabbit | 27 findings (3 blocker) |
| GLM 5.3 | 14 findings (1 blocker) |
| `craze-harness-modes` session (seam review) | no objections, two cautions |

The reviewers found the same holes independently, which is why the fixes are
pinned in the plan rather than left to the executor:

1. Every entry is immutable; every mutation is a new `*Entry` (a tool update,
   a closed run, a chunk) — a snapshot copies pointers to entries that will
   never change again.
2. No arbitrary code runs under the boundary: `Err` is held and its text read
   outside it; the engine's instance carries no clock.
3. The cost claims are pinned concretely — a deque, incremental byte
   accounting, a 2× stream builder, `Snapshot`'s critical section bounded and
   tested (≤ 1 ms worst case).
4. The snapshot carries continuation state and a stated four-window filling
   order, tested at the exact edges.
5. Bounds count retained bytes (text plus payload strings), not text alone.
6. `EntryID` is `{Seq, N}`, never a packed integer.
7. Attach's three error paths are pinned: a synchronous refusal after a fresh
   snapshot retries with a fresh snapshot; an asynchronous file-leg failure
   discards the prefix and re-attaches; an `Omitted` record re-attaches with
   no cursor. `SubscribeOptions.Ctx` makes `Attach` cancellable.
8. Convergence is a property of the state projection, not of history; history
   stays append-only.
9. Ask endings create no shared entry; every non-`Auto` opening breaks the
   run, a recorded behaviour change.
10. The pane owns an explicit display list, not an anchored overlay.
11. Parity compares at matching sequence prefixes, never against the live
    engine.
12. The clock rule (a run's `End` from the closing event's own `At`) is its
    own production commit (T1b), probed alone before anything else moves.
13. T1a precedes C1 so the package copies test assertions in their final
    form.
14. C5 is three commits with a named consumer inventory and a stated
    pointer-aliasing rule for bubbletea's `Model` copies.
15. SF-02 is deferred to S2 (below); SF-01 stands alone as the owner's
    decision.
16. A2 (the exactness test) runs at every cut of a recorded, bounded trace.
17. Several corrections of fact in the plan's §2 held after all three
    reviewers checked the file:line claims (bar items 25–27, also corrected).
18. From the harness session: children are depth 1; a `Mode` delta with an
    empty `Event.Mode` inside a replay bracket is a plain section
    replacement; native's plan approval ends the turn `end_turn`.
19. §10 names what is environment-provided; PR 2 branches only after H5 PR 2
    merged, for `app.go`'s sake too.

**Owner decisions as taken (2026-09-21):**

- **SF-01 — yes.** C8, in PR 2, publishes native's first-prompt title as a
  State-only `Title` delta; nothing else changes. `native-echo-80x24` row 20
  moves from `─ craze ─` to `─ hello ─`.
- **Auto-merge — yes**, under the gauntlet's rules (each PR merges once
  `/git-commands:watch-pr` is green and every real bot finding has a
  disposition on its thread).
- **The TUI's main transcript takes the 8 MiB retained-byte budget it lacks
  today**, with a V6 check in PR 1 that raises it to 32 MiB before PR 2's C5c
  if a real session's retained bytes come within 4× of it.
- **PR 1 started before H5 PR 2 merged**; H5 PR 2 has since merged (`2b5229b`).
- **The provider effort/speed work is Plan 025, not S1c** (§2.9).

Recorded behaviour changes are listed in full below, as shipped.

### Roadmap wording this plan departs from

Per the plan's §4 last bullet: `03` §8's "the engine applies a command's
transcript effect inside the command call and returns the entry and its
`seq`" is superseded by S1b's echo rule and this plan's display list (the
in-process client draws its own row and hides the echo); "the TUI keeps a
render cache keyed by entry id" holds; "client-local entries live in a TUI
overlay anchored after an engine entry id" becomes "in the pane's display
list, where today's slice put them" (an anchored overlay would reorder rows);
"the engine takes the injected clock: entry timestamps and thought-run
elapsed time come from the TUI's `m.now()`" becomes "the event's `At` stamps
every shared row; a client's clock is a fallback for a zero stamp". `07`'s
"folded inside the boundary" holds for the engine's instance; clients fold
their own.

### Deviations from the plan

The plan's execution amendments X1–X39, one paragraph each, plus two
unnumbered decisions the plan records alongside them; none reopens a pinned
decision. The full text and every failing schedule are in the plan.

**PR 1** (`de98834..27c1db6`):

1. **X1 (T1b)** — a tool's stamp. The close of the run above it and a new
   tool row take `Tool.At` when set, else the envelope's `Event.At`, else the
   client's clock (the engine's instance has none: a zero stamp stays zero).
   Only unstamped test events see the difference; every production tool
   event carries `Tool.At`.
2. **X2 (C1)** — ids for events production never produces. `EntryID{Seq, N}`
   names an entry by the event that created it, unique only while `Seq`
   strictly increases. Where it does not (the convergence test's
   re-application; PR 2's `Seq`-0 unit fixtures), entries take `{0, n}` from
   a model-local counter and the model's own `Seq` is left alone. The zero
   `EntryID` means "none".
3. **X3 (C1)** — the open stream entry accounts its tail's length (≤ 64 KiB),
   not its builder's, so a first client and a restored client trim at the
   same moment. Superseded in scope by X25 below.
4. **X4 (C1), revised (r2 fix)** — the builder compacts in place (a `copy`
   within its own array, no allocation) and is kept across runs rather than
   reallocated per run; a child's builder is released when its roster row
   finishes; a live transcript retains at most 2× `StreamText` of builder.
5. **X5 (C1), revised (r2 fix)** — the byte budget is enforced after every
   append and chunk merge, **never** after an in-place tool update (a trim
   there could drop the row just updated, and re-applying the update would
   append it again); an in-place update can therefore exceed the budget by
   at most one tool payload until the next append or chunk.
6. **X6 (C1)** — `turn{ended}` clears `Turn.ID` only when it names the
   current turn (a late ending of turn N after N+1 started is reachable); a
   `started` keeps `Turn.Foreign`.
7. **X7 (C1)** — `Entry.Streaming` marks the open stream entry of either
   kind, since an assistant entry's `Open` stays false while it streams, so a
   reader knows its text is in `Tail()`.
8. **X8 (C1)** — asks. An opening with an empty id is never kept (it can
   never end). The last-ended list (256 entries: id, kind, outcome, by, at —
   no bodies) is not part of `State()`.
9. **X9 (C1)** — child transcripts are created wherever the TUI's `ensureSub`
   creates them: on every child-routed event (a kind a child ignores
   included) and every roster event; a child id with no roster row is never
   evicted.
10. **X10 (C1)** — empty lists (todos, settings sections' lists, the queue,
    asks) are held as nil, so projections compare equal across the codec.
11. **X11 (C1, for PR 2), superseded by X14** — an `EventError` whose
    `Error()` is `""` draws no row, matching today's TUI.
12. **X12 (C1)** — the command-line note needs `sanitizeLine`, which calls
    `ansi.Strip` from a module §3.1's depguard rule denies; the package
    carries its own transcription (363 lines, MIT, v0.10.1, notice retained),
    held against the TUI's own over random inputs so a future `x/ansi` bump
    that changes the answer fails the gate.
13. **X13 (r1)** — a sub-agent's `finished` closes the child's run at the
    event's `At`, else the client's clock (T1b had left this one site on the
    raw `ev.At`).
14. **X14 (r2 fix)** — the observer's `Err` is the codec's already-built
    `*RemoteError`: the log encodes every event once before admission, and
    now keeps that value and hands the observer a copy of the event carrying
    it, so the fold knows an error's text and accounts its length without
    running an error's code under the boundary; an empty message draws
    nothing and closes nothing. A client folding outside a boundary (the
    TUI) passes `Options.ErrText`. Supersedes X11.
15. **X15 (r2 fix)** — convergence at a retention cap. Below the caps every
    state/marker row converges under re-application; at a cap, re-applying a
    row whose effect is to append history trims the oldest entry as any new
    history would, and a tool whose row was trimmed leaves the projection
    (today's rule). Exactness (A2) is unaffected — the attach protocol never
    re-delivers an event.
16. **X16 (C2)** — `Truncated` lives on `Ask` for an open ask and on the
    snapshot's roster row (`AgentRow.Truncated`, surfaced on the model as
    `State.TruncatedAgents`); each capped field is cut to its head at a rune
    boundary and cleared by the next opening or roster event.
17. **X17 (C2)** — the snapshot carries more continuation state than first
    drafted: the X2 id counter (`Local`), the roster's finish order
    (`FinishSeq` per row), the ended-ask list (`Ended`, X8), each
    transcript's `TailCut` and `OmittedRun`. `Entry.Bytes` is not on the
    wire; `Restore` recomputes it.
18. **X18 (C2)** — A3's model bound counts live children: 32 finished plus
    the running ones (a running row is never evicted), so the worst case is
    8 MiB + (32 + running) × 1 MiB — 42.95 MB against 8 + 33 MiB with one
    child still running.
19. **X19 (C2)** — the lock-hold bound as measured. On the worst case a
    cut's median is 223–367 µs, fastest 162–193 µs, slowest 1.2–1.9 ms
    (1.0–3.2 ms under `-race`, which can draw GC assist inside the section);
    §3.4's "≤ 1 ms" holds for the median, and the test asserts the fastest
    of 30 cuts.
20. **X20 (C2 → r3 fix)** — the turn's text is capped in a snapshot at
    256 KiB with `Truncated`, like an ask's body — an uncapped mandatory
    `Turn` made every snapshot of a multi-MiB prompt `ErrSnapshotTooLarge`
    until the next turn.
21. **X21 (C3)** — attach's details. Any synchronous `ErrCursorUnresolvable`
    from the snapshot's cursor is retried with a fresh snapshot (first
    attempt + 3 retries), then `ErrAttachRaced` (`unavailable`, never
    stored). `ErrClosed` and a context error return at once; a journal read
    fails asynchronously only. `ErrSnapshotTooLarge` returns wrapped
    (`failed`). `Reset` is set only from the first refusal of the client's
    own cursor. A nil `ctx` is `context.Background()`.
22. **X22 (C3)** — an `Omitted` record can need two re-attaches: the commit
    order can let a client see the omission and cut its own snapshot just
    before it, so its `Subscribe` pins the omitted record again; the second
    re-attach is past it. Bounded at two, no livelock; the client helper
    handles it.
23. **X23 (owner, 2026-09-23), as built (`9dd830c`)** — a byte ledger for
    windowed-out entries. A windowed snapshot carries, per omitted entry, its
    retained bytes and (for a tool) its id (`TranscriptSnap.Omitted
    []Omitted{Bytes, Tool}`); the restored model keeps them as
    payload-free, invisible placeholders that trim exactly when the first
    model trims the real rows, so an update to a windowed-out tool applies to
    nothing on both models, not just the first. A dropped placeholder is not
    counted in `Change.Dropped`.
24. **X24 (owner, 2026-09-23)** — V7 and the chunk allocation. Rather than
    allocate a replacement `Entry` per chunk, an open run's end and tail live
    on the `Transcript` and are materialised into an immutable entry only
    when read, cut or closed; V7 is re-measured both ways and reported both
    ways, and 100,000 iterations is the budget's measure.
25. **X25 (r7)** — a streamed entry accounts `min(bytes streamed,
    StreamText)`, content-independent, replacing X3's exact-tail accounting
    (which could only approximate a placeholder's size once multi-byte runes
    were involved). `Entry.Cut` records whether a stream was cut, carried on
    the wire, so `Restore` re-accounts a closed entry exactly.

**PR 2** (`feature/plan-024-s1c-tui`, #52), decided by the executor per the owner's
2026-09-24 instruction not to stop and ask, each recapped here:

26. **X26 (C5b)** — an interjection has no local twin to hide: its row has
    always been drawn from the agent's broadcast, never from the send (one
    source per entry), so the only echo the pane hides is the started user
    row of the turn `Submit` already drew.
27. **C5a's aliasing audit, as run** — a go/ast scan of every `func (m
    Model)` method in `internal/tui`, transitively through `Model` methods,
    found none that writes a row without returning `Model`, and no call site
    discards a returned `Model`. One test relied on discarded copies
    discarding their rows (`TestRenameWithNoTitleIsAUsageError`) and was
    rewritten to build a model per iteration; about 18 dialog/click tests
    paint through a fork the next `Update` overwrites before anything reads
    it.
28. **X27 (C5c)** — the local-row-inside-a-stream change is visible for
    assistant text too, not only an open thought run as §3.8 assumed:
    today's `appendEntry` ended every run for every row, so a local row used
    to split a streaming reply into a new entry below it; now the local row
    ends no run and the chunk grows the shared entry above it. No golden
    reaches the schedule.
29. **X28 (C5c)** — how a model trim reaches the pane. After a fold with
    `Dropped > 0`, the longest front prefix of the display list whose shared
    rows the model no longer holds leaves whole; the pane's own caps then
    apply, only when a row was added (a chunk growing a row never trims).
    This is how owner decision 3's 8 MiB main budget reaches the frame.
30. **X29 (C5c)** — the `/clear` re-append rule, by kind: a touched **tool**
    entry with no row re-appends at the tail (today's rule); a touched
    **closed non-tool** entry with no row draws nothing (a cleared run is
    forgotten, as today); the run open at the clear continues as X30
    describes. Both implementers reached this independently.
31. **X30 (C5c)** — a chunk into the run that was open at `/clear` appends a
    continuation row showing the tail from the clear mark's offset, dated at
    that chunk; past the 64 KiB cap the offset means nothing and the row
    shows the whole tail.
32. **X31 (C5c), revised after r17 (`5c770b8`)** — the todo notes after
    `/clear`. The pane's dedupe decision is authoritative in both
    directions: a fold note the pane does not owe gets no row (hidden by
    kind, like an echo), and an owed note the fold did not write is the
    pane's own local row — an empty `EventTodos` consumed after a newer list
    reached the snapshot is the schedule that needs the second direction.
33. **X32 (C5c)** — a child's byte budget now counts payloads, not text
    alone (§3.2 (b)), so a tool-heavy child drops its oldest rows sooner. No
    `subview_test.go` assertion depended on the text-only figure. Recorded
    behaviour change beside §4 (ii).
34. **X33 (C5c)** — a session swap (the provider picker's swap, the resume
    picker's load) detaches the rows on screen: the new session gets a fresh
    `m.shared`, and rows the old one drew stay as this client's own local
    rows (`pane.detach`), exactly as today's transcript kept them.
35. **X34 (C5c), as built** — the parity watch (A11). A test-only `foldHook`
    (nil in production), installed package-wide, shadows every fold in
    `internal/tui`'s whole test suite against a model folded from exactly the
    recorded events, checking `Change`/`Seq` equality and P0–P4 (history/state
    equality, no stale row, every entry shown once unless hidden/cleared/
    trimmed, model order but for re-appends, no local row in the model). One
    run: 606 models, 7,810 folds, 3,700 whole checks; four planted pane bugs
    were each caught. After r17 the watch predicts the exact row list at
    every fold and between folds rather than accepting any suffix once
    `trimmed` is set; `CRAZE_PARITY_STRICT=1` runs the whole check at every
    one of the ~7,900 folds (33 s, still green).
36. **X35 (r17)** — an error's text is read once, before the fold.
    `applyEvent` reads a main `EventError`'s `Error()` once and hands the
    string to both the fold and `m.err` (`foldInputs`), so a foreign
    `Error()` is never called twice; a child's error is never read.
37. **X36 (r17)** — a child the model evicted (33+ finished) keeps its rows
    as local rows: the pane is detached after the roster fold, the same
    shape X33 uses for a session swap, so no shared row ever names an entry
    that is gone.
38. **X37 (r18)** — a hidden duplicate todo note still ends the run it lands
    in. On X31's lagged schedule, the fold's own note for the next list gets
    no row but is still an entry in the shared model, and like every entry it
    ends the stream run it lands in: a thought or reply streaming across it
    is drawn as two blocks where the old fold drew one. Recorded, not fixed
    — nothing is lost or duplicated.
39. **X38 (C8)** — SF-01 moves two goldens, not one. `native-mode-100x30`
    (H5's plan-mode golden, added after this plan was written) moves the
    same way `native-echo-80x24` does, on its separator row only — the same
    consequence of the same owner decision — so it is regenerated with
    `-update` too and both one-row diffs go in PR 2's body.
40. **X39 (after r19)** — a pre-existing golden flake, fixed here, moves two
    goldens by one spinner glyph. The H6 session diagnosed a macOS-CI flake
    in `TestFrameGoldenGrokSubagentRows` (reproduced 40/40 under one-CPU
    starvation): the agent row's `4.7k tok` is read from the live snapshot
    (`refreshSnap`), which can run ahead of the event stream, so the
    script's `<wait:text:4.7k tok>` could match before the wait tool's own
    event was applied, and the golden was captured without that tool row.
    S1c does not remove this — agent rows still come from `State()`, SF-02
    is S2's. The fix, landed as its own test-only commit (`83e58e3`, after
    the branch rebased onto `origin/main` `f5c3cfd`/H6 PR 1 #51): a
    `grok-subagent-hold` fake mode that sends nothing after its progress
    event, a wait on the tool row before the tokens, and
    `runFakeFrameFrozen` so the capture is deterministic — which moves
    `grok-subagent-rows-{80x24,100x30}` by one glyph (the frozen spinner
    frame, `✴` → `✳`), nothing else. The class — rows drawn from the live
    snapshot racing rows drawn from events, suspected in the grok
    SubagentView / TwoView / Cancel goldens too — is recorded for S2 in
    `13` (SF-45).
41. **Declined (r17 finding 4)** — a tool row re-appended after `/clear`
    keeps the model entry's original `At`/`End` where the old fold dated a
    new row at the update; no renderer reads a tool row's stamps, so no
    frame differs.

### The recorded behaviour changes

Plan §4's list, verbatim: "No user-visible change except, all recorded: (i)
§3.8's local-row-inside-a-stream rule; (ii) the main transcript's 8 MiB
retained-byte budget (owner decision 3); (iii) §3.2's run-end provenance (a
duration measures the events, not the client's consumption); (iv) §3.3's
break on a masked ask opening in the cancel window; (v) SF-01's row if the
owner takes it."

PR 2 added, mirrored into `12` here per the plan's own instruction:

- **X27** corrects (i): the change is visible for assistant text too, not
  only an open thought run, because today's `appendEntry` ended every run for
  every row.
- **X32**: a child's byte budget now counts payloads, not text alone.
- **X33**: a session swap (the provider picker, the resume picker) detaches
  the rows on screen as this client's own local rows.
- **X37**: a hidden duplicate todo note still ends the run it lands in, so a
  stream across it draws as two blocks where the old fold drew one.
- **X38**: SF-01 (v) moves two goldens (`native-echo-80x24` and
  `native-mode-100x30`), not one.

### Live smoke

**PR 1 — the attach probe, Linux** (`smoke/linux/pr1-probe.md`, the PR 1 tip
`16ececb`, `craze prompt --json --attach-probe=PATH`, a tool-using prompt
with one follow-up, attaching from its own goroutine on the first text of
the first turn):

| provider | model | verdict | attached | retained (first client's model) |
|---|---|---|---|---|
| cursor | default | **SAME** | snapshot seq 6, folded 258 records to seq 264 | main 14 entries, 5,418 B |
| grok | default | **SAME** | snapshot seq 19, folded 660 records to seq 679 | main 15 entries, 6,788 B |
| native | `fireworks/kimi-k3` | **SAME** | snapshot seq 22, folded 240 records to seq 262 | main 10 entries, 11,780 B |
| grok, with a sub-agent | default | **SAME** | snapshot seq 39, folded 892 records to seq 931 | main 9 entries, 6,166 B; subs 1/5 entries/4,000 B (570 child-tagged lines) |

**PR 2 — V1, Linux, tmux** (`smoke/linux/pr2/RESULTS.md`, binary from
`065675e` — the pre-rebase commit, the same code as `bee8547` before H6
PR 1 (#51) was merged under it; an earlier phase at the C5c binary is
`EARLY.md`, all legs PASS there too):

| # | leg | cursor | grok | native |
|---|---|---|---|---|
| 1 | queued follow-up drains | PASS | PASS | PASS |
| 2 | Esc mid-turn, no idle flash | PASS | PASS | PASS |
| 4a | interject on grok | n/a | PASS | n/a |
| 8a | plan card: accept / reject / Esc | PASS (×3) | n/a | n/a |
| — | sub-agent view | n/a | PASS | n/a |
| 9 | `/model` + mode cycle + `/rename` | PASS | n/a | n/a |
| 10 | `--continue` | PASS | n/a | n/a |
| x27 | local row inside a stream | PASS | PASS | n/a |
| x30 | `/clear` mid-stream | PASS | not run | n/a |
| C8 | native title on the separator | n/a | n/a | PASS |
| — | attach probe | PASS (SAME) | PASS (SAME) | PASS (SAME) |

**PR 2 — V4, mac-mini, over ssh** (`smoke/macos/RESULTS.md`, the same
`065675e` darwin binary as V1's; cursor **NOT REACHABLE** — login keychain,
as S1a/S1b/PR1 all found):

| # | leg | grok | native |
|---|---|---|---|
| 1 | queued follow-up drains | PASS | PASS |
| 2 | Esc mid-turn, no idle flash | PASS | PASS |
| 4a | interject mid-turn | PASS | n/a |
| — | sub-agent view | PASS | n/a |
| 9 | `/model` + mode cycle + `/rename` | PASS | n/a |
| 10 | `--continue` | PASS | PASS (native writes no row; native `--continue` errors outright, Anomaly 5) |
| x27 | local row mid-stream | PASS | PASS |
| x30 | `/clear` mid-stream | PASS | PASS |
| — | native title on the separator | n/a | PASS |
| — | attach probe | PASS (SAME) | PASS (SAME) |

**V3, the journal record**: Linux, the pre-rebase `065675e` binary (`bee8547` after the rebase), 21/21
PASS (the early phase on the C5c binary: 11/11 PASS); mac-mini 17/17 PASS. No `gap` lines anywhere, every
`closing` diag `droppedAtClose = 0`/`outboxSkippedPrimary = 0`, exactly one
`craze_session` note per file, every ask has one opening and one ending, no
craze-initiated meta sets `mode`/`text`.

### Measurements

**PR 1.** **V6** (a live 66-tool cursor session, Linux — 66 tools for both
the baseline and the candidate run, 65 with the probe's two extra models
attached): craze's own peak RSS 31.1 MiB baseline (`9125ec7`) vs 31.0 MiB
candidate (−0.1 MiB, within run-to-run noise), 34.9 MiB with the probe's two
extra models; the model retained 1,677,517 bytes (1.60 MiB) on 65 tools / 32
large edits — **4.9×
under the 8 MiB main budget**. Owner decision 3's raise-trigger (within 4× /
≥ 2 MiB) was not met, so `Bounds.MainBytes` stays 8 MiB. Recorded for the
owner: this session retained ~25 KiB per tool (dominated by edit diffs), so
the 8 MiB budget is reached after roughly 320 such tools in one session —
past that, the oldest rows leave scroll-back behind the trim note (the
journal keeps them).

**V7** (engine-level text-delta publish, journal attached, the real fold):
1.8–4.5 µs at 2,000 iterations × 5, 1.7–2.6 µs at 100,000 × 5 — every run
under the 5 µs budget, after X24 made a chunk allocation-free.

**A3 / X19, the four bounds, measured:** model retention worst case (5,000
main entries at the output cap, 33 children, one still running) 42,954,787
bytes (main 8,381,291; children 34,573,496) against the 8 + 33 MiB bound
(X18), heap 1.34–1.49× the accounting; a 4 MiB snapshot budget encodes to
exactly 4,194,304 bytes, one byte over windows; a 5 MiB open plan is
`ErrSnapshotTooLarge`, 200 KiB is carried whole, 300 KiB cut to its 256 KiB
head; the worst case's windowed ledger is 32,896 records in 494 KB; an
attach over the worst case (41,367 events) holds a 1,000-record burst in a
1,024 / 8 MiB subscription with no `ErrSlowConsumer`; decoding a 4 MiB
snapshot allocates 1.66× its size and retains 0.97–0.99×. X19's lock-hold
bound: a cut's median 223–367 µs, fastest 162–193 µs, slowest 1.2–1.9 ms
(1.0–3.2 ms under `-race`).

**PR 2.** **V2** (`--json` parity against the `6581e0a` baseline harness, 103
scenarios) at `f20bbab` (pre-rebase; the same code as `0f7730a` before H6
PR 1 (#51) was merged under it): 102/103 SAME by bytes, 103/103 by content
(`sigint-between-turns`, the documented unordered-pair race, calibrated —
`v2/README.md` "PR 2 at `f20bbab`").

**V5** — `go test -race -count=20 -timeout 60m` of `internal/transcript`,
`engine`, `agent` and `tui` at `f20bbab` (pre-rebase; the same code as
`0f7730a`): all pass — transcript 477 s, engine 305 s, agent 1,111 s, tui
1,118 s; no failure, nothing retried.

**The parity watch's coverage** (A11, X34): one run over `internal/tui`'s
whole test suite folded 606 models across 7,810 folds, with 3,700 whole
state/history checks (the two cap tests sampled at 1–64, powers of two, and
every 256th fold); four planted pane bugs were each caught. The state
mirrors (`TestTheStateMirrorsMatchTheModelWhenQuiet`) matched at 153
quiescent checkpoints across 82 tests, with two documented exemptions (the
Stub publishes no install delta at `Start`; native's title until C8).
`CRAZE_PARITY_STRICT=1` runs the whole check at every one of the ~7,900
folds — 33 s, still green.

### Decisions and questions touched

**SQ9** resolved (§3.2 (b) / §3.5): the model's own bounds (5,000 entries /
8 MiB main, 1,000 / 1 MiB per child, 32 finished children, 64 KiB streamed
text), the snapshot's 4 MiB budget with its filling order and
`ErrSnapshotTooLarge`; rewritten in `10` by this commit. **SQ15** stays
open: the model conflates tool events by last state per id, which is what
SQ15's clients needed, but whether the journal or a queue conflates is
unchanged and still measured, not decided. **SF-01** taken (native publishes
a State-only `Title` delta; two goldens moved, X38). **SF-03** taken (`m.sess`
gone, C7). **SF-04** stays out (§3.9: the Stub's ~265-line id-adoption
subsystem is untouched; nothing here needed the Stub to mint). **SF-05**
taken alongside SQ9 above. **SF-02** re-pointed at S2 (§3.9: reading settings
from the fold changes intermediate frames, and proving them identical is
S2's work where the whole mirror moves at once; the model folds every
section for the snapshot and A11 proves it equal to `State()` only at
quiescent checkpoints). No new `SD-nn`; nothing here reopens SD-33.

### Handoff

What S2 inherits:

- **`Engine.Attach` joins `Control` in process** (§3.6): S2 wraps
  `Attachment` in `snapshot`/`reset`/`event`/`synchronized` notifications and
  adds the connection-local barrier `05`/SF-10 already owed (`Control.Sync`
  returns no sequence number).
- **`SubscribeOptions.Ctx`** makes every wait `Attach` makes cancellable (a
  two-line `select` change, mirrored in `Observe`'s); still on the blocking
  side of `control.go`'s list, still must not be called from the primary's
  reader while that reader is not reading (SF-14).
- **The snapshot codec and its version** (`SnapshotVersion = 1`,
  `EncodeSnapshot`/`DecodeSnapshot`, the exported leaf wrappers added to
  `internal/agent/eventcodec.go`): S2's wire schema is generated from or
  checked against these same wrappers, so a field added to an agent type
  reaches both codecs by one line.
- **SF-02**, re-pointed here and above: the TUI still mirrors settings
  through `refreshSnap()`/`State()` rather than the fold; S2 moves the whole
  mirror at once and proves intermediate frames identical.
- **The receipt-mode gap** (plan §4 last bullet, CodeRabbit 25): the model
  carries no `Provider` and no session-wide tool index, so a remote client
  cannot rebuild a receipt-mode sub-agent transcript
  (`rebuildReceiptTranscript` reads `State().Provider` and `State().Tools`).
  S2 decides whether the snapshot grows those fields or the receipt rows
  become shared entries.
- **H6 PR 3's pending `ForeignTurnInfo.Reason` fold row**: native-harness H6
  (Plan 026, panel-reviewed, not yet executed) adds `agent.ForeignTurnInfo.Reason`
  and its codec twin, so the shared model's foreign-turn note wording
  (today's interjection-fallback text vs native's `subagent_wake` wording)
  comes from the event rather than a provider flag the TUI no longer reads.
  The `foreign_turn` fold row (§3.3) needs that field folded in when H6
  lands; nothing here anticipates it.

## S2 — the control socket

| | |
|---|---|
| Status | complete (Plan 027, FINAL after panel review 2026-09-24); all four PRs merged, PR 4 on 2026-09-28. |
| Plan | `027-session-control-s2-socket` (outside the repo, `~/.claude/plans/craze/`, raw panel reviews in its `panel/` folder) |
| Baseline | `origin/main` `5901e4a`: H6 PR 2 (#53, sub-agent stop), merged on top of S1c PR 2 `79eb082` (#52, which completes S1) |
| Branch / PRs | four sequential PRs, each branched from a freshly fetched `origin/main` after the previous one merges: `feature/plan-027-s2-wire`, `feature/plan-027-s2-host`, `feature/plan-027-s2-tui-async`, `feature/plan-027-s2-attach` |
| Merged | PR 1 — #55 `318fc76` (2026-09-25); PR 2 — #56 `2acd54a` (2026-09-26); PR 3 — #61 `3eabb31` (2026-09-27); PR 4 — #63 `73ed5e0` (2026-09-28) |

### The PR cut

| PR | branch | content |
|---|---|---|
| 1 | `feature/plan-027-s2-wire` | the engine's owed seams (SF-10, SF-11, SF-13, SF-15, SF-16), `Subscription.Cutoff`/`Rest`, `SubmitResult.Text`; `internal/protocol` + the JSON Schema; `internal/control` (the server over any listener); `internal/remote` core; `internal/fakehost` + `cmd/craze-fake-host` + wire fixtures; `docs/reference/protocol.md` |
| 2 | `feature/plan-027-s2-host` | `internal/rundir` (namespace, locks, identity-checked unlink, peer uid, registry); the TUI process serves its session; the SQ16 lock (refuse); `craze bridge`; a scripted smoke client |
| 3 | `feature/plan-027-s2-tui-async` | the TUI holds a `Backend`; the command gate (SF-17) with operation chains; the mirror onto the fold (SF-02, SF-45, SF-43, SF-38); `maskDrops` through the gate; hidden answers and H6's stop fire-and-forget; two-client queue correctness |
| 4 | `feature/plan-027-s2-attach` | `internal/remote` implements `Backend`; restores and resume; `craze attach`; SQ16 attaches; SF-18's marks (optional); every frame golden also runs over the socket; the exit smoke; S2 docs complete |

**Why not the candidate order** (wire → attach → TUI async → bridge): attach
as the full TUI needs the TUI's engine calls asynchronous and its mirror off
`State()` first, so that move has to happen before attach exists at all.
Doing it in process first (PR 3) also separates its golden risk from any
transport risk — a golden that moves in PR 3 is the gate or the mirror, never
the socket. The bridge moves forward into PR 2 because it is small, needs
only the namespace, and is what makes PR 2's server exercisable by a script.
The published spec lands with the wire (PR 1), per `07`'s own convention that
a PR changing the wire updates the schema, fixtures, and spec together.

### Owner decisions (2026-09-24, not for the panel to reopen)

1. One plan, 3–4 PRs, in the shape of S1b/S1c; the kickoff's candidate cut was
   a hypothesis discovery changed.
2. **SD-33** (SQ12): session hosts are born detached from S4 on and the TUI is
   a socket client for good — S2's exit adds "the full TUI runs unchanged
   over it, goldens included".
3. SQ7's default holds: `craze attach` is the full TUI over a socket-backed
   session.
4. Decide, don't ask: product calls and one-way doors go to the owner once,
   together (§3.19), recapped here rather than asked mid-run.
5. The four product calls, answered 2026-09-24:
   - **SF-18** (how a client words another client's action): the recommended
     mark, as one small optional commit (C28b, PR 4) — dropped for the status
     quo if it grows past the note sites and a `Cause`-to-own-client
     comparison, or if it moves any golden.
   - **`craze attach` with no session in this directory**: exit 1 with the
     list of sessions running elsewhere and the `--session` hint; several
     sessions in this directory lists them and exits 2.
   - **Keys in `craze attach`**: identical to the host TUI's; the first
     Ctrl+C while a turn works acts on the shared session.
   - **Auto-merge**: yes for all four PRs, under the gauntlet's rules.
   - Scopes (SQ11) stand as stated and unobjected to: local same-uid only,
     scopes arrive at S6.

### The planner's decisions

- The PR order is wire → host + bridge → TUI async → attach, not the
  candidate's wire → attach → TUI async → bridge (§3.1, above).
- The command gate (§3.12) keeps every golden byte-identical while every
  synchronous engine call becomes a `tea.Cmd`: strict arrival order,
  operation chains, continuations that show a command's effect from its
  result — production semantics, not a test barrier, with a `gateSync`
  baseline running every golden beside the async one.
- Static session facts (provider, capabilities, the provider's session id,
  model/mode catalogs) ride a session-info document; dynamic state stays on
  the stream — nothing new is added to `agent.Event` or `StateDelta`.
- The Stub publishes an install delta at `Start` as an opt-in option (on for
  `internal/tui` and the fake host); `SetCommands`/`SetPlugins` publish their
  deltas too — closes Plan 024's X34 exemption.
- Wire errors carry a `reason` beside the `code`, so `errors.Is` works over
  the socket.
- Client ids are minted per connection and bound with generations, released
  on disconnect, re-claimable within the engine's incarnation by a
  resume-token holder, and retired only when released, aged out, and holding
  no reservation; nothing is resent unless the host answers `resumed: true`.
- `session.stop` is specified but unsupported on a TUI-hosted session
  (capability `stop: false`) until S4's headless hosts.
- Bounded history is `session.snapshot`, not a page API — a departure from
  `06` (below); paging older entries waits for a client that needs it.
- SQ16's default is adopted: the lock covers `--continue` and the resume
  picker, and lives under `<HOME>/.cache/craze/locks/`, independent of the
  runtime base and `CRAZE_HOME`.
- The socket binds in a validated 0700 directory, then is chmod-ed to 0600,
  rather than bound under a changed umask; Go's unlink-on-close is disabled.
- `hello` is the one tolerant method, carrying a list of protocol versions.
- The hub splice (`session.connect`) is pinned now: a host issues its own
  credentials after the splice.

### Plan review — 2026-09-24

| reviewer | result |
|---|---|
| Codex, `gpt-6-astra`, high effort, 3 rounds | round 1: 31 findings, 6 blockers, plus 5 claims verified to hold; round 2: 1 blocker closed by a new route; round 3: 1 new blocker, 1 major, resolved by simplifying |
| CodeRabbit | 31 findings, 1 blocker |
| GLM 5.3 | 13 findings, plus 1 set of verified claims |
| The H6 session's seam review | applied before the panel: the gate's reader (one outstanding, parked while draining), SQ16 covering the resume picker too, H6 PR 3's real file list, every `agent.Capabilities` field on the wire, the back-pressure change on primary-less hosts, the wake golden in V8/V6 |

**Where they converged** (round 1): the draft's command gate was wrong in
three ways, client-id release raced a resume, the barrier's 5 s timeout could
reorder a reply, the settings chains had no read, and the SQ16 lock could be
split — the reviewers found these independently, so the fixes are pinned
rather than left to the executor.

**What changed, condensed:**

- **The gate keeps strict arrival order.** A reply applies the moment it
  arrives, alone; every held message keeps its place; a continuation shows
  its command's effect from its result. Round 2 found the fix still consumed
  the working frame on a short turn; round 3 replaced that fix by going back
  to today's `await` — a `frameSyncMsg` arriving while a gate is open is
  acknowledged in the release Update, so the frame order around a token's
  barrier is exactly today's.
- **Operation chains**: everything after a gated call moves into its
  continuation; hidden answers and the sub-agent stop become fire-and-forget
  (no card, nothing drawn).
- **`session.history` is replaced by `session.snapshot`**, a bounded snapshot
  with its own cut — pages of mutable entries needed rules the draft lacked.
- **`session.digest` was proposed, then withdrawn in round 3**: exactness is
  proven in-process instead, where both sides are held.
- **The reply barrier has no timeout escape**; a wedged connection is closed
  and the receipt keeps the answer; **nothing is resent unless the host
  answers `resumed: true`**.
- **`Subscription.Rest`** delivers a session's closing records from the
  subscription's own retained undelivered state, never the ring; round 3 also
  gave it the record the owner was blocked sending and the rest of its local
  batch, in order.
- **Client ids**: a binding table with generations, compare-and-release;
  retirement only when released, aged out, and holding no reservation; tokens
  scoped to the engine incarnation.
- **The registry, host locks, and session locks** all moved under
  `<HOME>/.cache/craze/`, independent of the runtime base and `CRAZE_HOME`;
  discovery no longer searches runtime bases.
- **The overlay inventory** (mode, model, config, command-result overlays)
  retires on the command's own event for that item — cause plus item, never
  equality — closing SF-38 and SF-44 along the way.
- The Stub's install-delta change was narrowed to an opt-in option, so the
  wire fixtures never re-record.

**Not adopted, with reasons:** CodeRabbit's message-replay alternative for
the gate (the model shares pointers across copies); moving the Stub out of
`internal/tui` for the fake host's sake (a test binary's size does not
matter; SF-04 owns that cleanup); CodeRabbit's claim that `RunFrameScript`
cannot host the socket matrix (it runs one TUI there fine; only the two-TUI
tests move to a pump).

No owner decision was reopened at any round.

### Roadmap wording this plan departs from

- `05` "Attach and resume": the snapshot travels in the attach reply, not as
  a separate `snapshot` notification.
- `05`'s `hello` gains `protocols`, `token`/`resume`, and `via`.
- `05`'s `reset{reason}` list becomes §3.4's.
- `05`'s "a session whose `session/load` failed never reaches `synchronized`"
  becomes: `synchronized` is the stream's catch-up, and a failed start is
  `ready{startFailed}` (or `not_accepting`, reason `start_failed`, for a
  `when: "ready"` attach); mutating commands are refused either way.
- **`06`'s `history(beforeSeq, limit)` becomes `session.snapshot`**: a bounded
  snapshot with its own cut. Paging older entries waits for a client that
  needs it, behind a new capability.
- SD-27's "umask around bind" becomes chmod inside the 0700 directory.
- `02`'s `h/<short-id>` becomes `<ns>/<hostId>.sock` in the runtime base, with
  the registry, host locks, and session locks under `<HOME>/.cache/craze/` (a
  fixed per-user path discovery can rely on) rather than in the runtime
  namespace.

### What shipped per commit

**PR 3 — `feature/plan-027-s2-tui-async`** (shipped, #61 `3eabb31`,
2026-09-27): C16 (`33cdebb` — the
TUI holds a `backend.Backend`, `internal/backend/backend.go:67-102`; every
command, `Read`, `Ask` and `Settings` takes a `context.Context` first —
`ClientID`, `Started`, `Close`, `Info` and `Epoch` take none, and all but
`Close` wait on nothing — `Close` can block while the agent is reaped
(`backend.go:75-78`, `:94`); `Settings` narrows to `{Model, Mode, Config}`, no
provider); C17 (`b8bf66b` — the command gate: `Model.run`/`gated`
(`internal/tui/gate.go:179`, `:297`) wraps a gated call in a goroutine under
`gateDeadline` (15 s, `:55`), `runFrameModes` runs every frame golden but
the six pre-start pickers in both the synchronous and asynchronous gate
modes) with its fix rounds C17a
(`c0a0e59` — the frame harness's rendezvous closes the barrier race, the
invisibility digest widened, every payload-bearing message charged, the
drained slot zeroed), C17b (`9749185` — a key/mouse/resize token and its
sync token become one program message, delivered back to back), and C17c
(`1040cda` — a token parks only when nothing is held, the capture waits for
a settled model then quits through the model's own FIFO, shutdown bounded);
C18a (`5c801ee` — `Submit` through the gate, `internal/tui/app.go`; the
band's transitional read moves into the call; a gated call's panic is
re-raised on the command's goroutine so bubbletea recovers it); the
fake-agent flake fix (`ac0030e` — `grok-subagent-late` ends its turn between
spinner beats, X38); C18b (`c396b35` — the queue verbs and Ctrl+C/Esc
through the gate; `clearPending`'s Disarm and ClearQueue are one gated call,
back to back on the call's goroutine, `internal/tui/app.go:2406-2461`) and
C18b1 (`c8fa58e` — a settling-turn Ctrl+C property test, test-only); C18c
(`1dd6fe3` — answers, Interject and the masked opening (`pushCard` while
`cardMasking`) through the gate; `m.sessGen` and `Backend.Epoch()`
(`internal/tui/engine_backend.go:58`) fence every command and chain step;
V8's seeded-jitter run, async only: 760 passes, 0 diffs); the H7 compaction golden wrap
(`f2ece4b`); C19 (`fd72eee` — the Stub's `InstallOnStart`
(`internal/tui/stub.go:99`) publishes the live session's one install delta
and flushes, for `internal/tui`'s Stubs only); C20 (`a2655f1` —
`Backend.Info()` (`internal/tui/engine_backend.go:225`) and ordered
`Tools()`) with C20a (`f604b2a` — `Tools()` deep-copies every tool's nested
fields) and C20b (`ad200c0` — `State().Tools` clones too); C21 (`a280912` —
the mirror is the fold: `recompute` (`internal/tui/mirror.go:39`) is the one
writer of `m.snap`, `m.queue` and the send-now arm, through
`transcript.Model.Mirror()` (`internal/transcript/mirror.go:48`);
`refreshSnap`, `refreshQueue` and `Backend.State()` are deleted) with the H7
usage golden wrap (`f44f7ca`) and C21a (`d086fe4` — `Mirror()` deep-copies
every slice and pointer it hands out); C22 (`674da35` — two clients on one
queue: `expectedVersion` captured at edit start on every save,
`internal/tui/engine_backend.go:141`; a stale refusal keeps the edit's text
and refreshes its version instead of stranding it); C23 (`240718f` —
`stopSubagent` returns a `tea.Cmd`, `internal/tui/subcancel.go:91`, never
gated, threaded through `handleRowsKey` and `handleViewKey`).

Proof, at the code tip `240718f` (the commits after it touch only `discovery/`):
V1 — `-race -count=20` on tui (`-timeout 150m`: 3,673 s, about 184 s a
pass), backend, engine, agent, transcript, control and remote: all ok, no
race; V2 — `craze prompt --json` against the plan-021 baseline: 103/103
SAME by bytes; V8 — the seeded 0–10 ms jitter run, async only: 33 golden
tests (38 runs with their subtests) under 20 seeds, 760 passes, 0 failures;
V6 — mac-mini key latency under a four-child
native fan-out with gated commands mid-stream (PR 3 at `674da35` against
`main` at `ddd9812`): worst key-to-screen 26.2 ms against 23.4 ms, no
significant difference (rank-sum p = 0.19 over the fan-out, 0.85 after a
gated command), and PR 3's whole process used about 15% more CPU per
journal event (2.40 against 2.09 ms, not attributed further; X46 6 expected
the fold mirror's per-event copies to cost CPU); starvation — every golden under a 5% CPU quota:
83/83 pass. `git diff --stat origin/main..HEAD -- '*testdata*'` is empty.

**PR 4 — `feature/plan-027-s2-attach`** (in review): C26 (`9acd8d1` —
`remote.Session` implements `backend.Backend`: `DialSession`
(`internal/remote/session.go:161`) says hello and checks the host's codecs;
`Info()` (`:366`) is the Session's own copy, updated by `Ready`/`Restore` as
received; `Epoch()` (`:360`) returns the client's identity number; errors
reconstruct through one sentinel table, `internal/remote/errors.go`) with its
fix rounds C26a (`4c37522` — a command or read binds to the client identity
*number* captured atomically at the Session's entry, never the id's spelling,
so a restarted engine minting the same `c-1` cannot run an old command against
the replacement; an adoption settles every command bound to the identity it
left, sent or not) and C26b (`851b34b` — the adoption wakes every waiter under
`changedLocked`, so a bound read sleeping through a slow re-attach answers
`ErrStaleEpoch` on time; a read's reply is judged against its binding after it
arrives); the prelude (`fd5a7e3` — SF-55 fixed in the fold: every
`send_now`-origin `started` ends the fold's arm under the engine's lock, no
`Cause` needed; SF-54 fixed in the fold: a child's `EventTool` with a title
sets its roster row's `Activity` (`internal/transcript/fold.go:170-172`,
`:483-491`), and the mirror's `armFired`/`childActivity` compensations are
gone); C27 (`9118a9f` — a restored session: `tui.Config.Backend` builds no
engine, picker, `OnEngine`, index or claim; `applyRestore`
(`internal/tui/restore.go:81`) rebuilds every pane, a card per open ask, the
turn, the foreign turn, the queue band and every revision guard, and clears
every overlay and echo marker; older-generation events are dropped while the
restore is HELD, not when applied; `TestAttachMidTurnWithAnOpenQuestionCanAnswerAtOnce`,
`TestARestoreDuringAHoldFoldsNothingTwice`) with C27a (`72ed882` — a restore
from another incarnation ends any queue edit, retires every turn-bound
transient (the send-now confirm, the Ctrl+C window), and uses the restored
item's own `Info` for its decisions) and C27b (`eef9775` — the replay guard's
empty-snapshot rule is narrowed to the first or a same-incarnation restore; a
pending plan implementation retires on a restore's both outcomes; `cancelled`
is kept only for the same running engine turn; `upDone` resets in
`dropSession`); C28 (`7b3c1f5` — `craze attach`, `internal/cli/attach.go`:
resolution through the bridge's matcher, `tui.Config.Viewer` clears the
pickers/claims/index/host reporting, SQ16 attaches through `attachHeld`) with
C28a (`2da9be2` — a row with a craze id is claimed first: held routes to the
attach path with the spawn flags it ignores named in the note, claimed routes
to the spawn-flag refusal (releasing the claim on a refusal); the holder's
registry entry must also name the claimed session (`holderEntry`,
`internal/cli/attach.go:382-404`); the tty check is a real termios check per
OS); C28b dropped (the owner's rule, header 5: the ask-note marks fit, but the
model/title notes would duplicate an open model dialog or an in-flight
`/model` chain — three deliberate "no extra note" tests pin it; status quo,
SF-18 stays with this finding); C29 (`1256e18` — every frame golden also runs
over the socket: `FrameOpts.transport` is test-only, installed by a hook; the
socket run builds a `NoPrimary` session, serves it over `internal/control` on
its own `/tmp`-rooted directory, and attaches `when: "now"` before the host's
`Start`; the coverage manifest lists 119 golden files, 113 under both
transports and the six picker frames in process only
(`internal/tui/golden_manifest_test.go:460-461`); `TestSocketGoldensMatchTheEngine`
pins an exact common seq at a sync-token rendezvous;
`TestAttachMidTurnOverTheSocketReproducesTheFirst`
(`internal/engine/exactness_socket_test.go:184`) attaches over a real server
at every cut of a 144-event recorded trace plus eight mid-turn cuts over the
fake agent) with four fix rounds. **C29a** (`680d345`, astra r69): a
transport credit keyed by (test, frame digest) — none existed before this —
plus a suite-level `TestMain` coverage check, `make test`/`test-race` and
CI's `test` job pinning `CRAZE_GOLDEN_TRANSPORT=both`, and a socket run's
verdict adding a final `session.sync` barrier plus requiring the view
close's `session.detach` to have been answered, both read through the tap,
so no reset can escape it unread (`internal/tui/frame_socket_test.go:335-409`);
every test that binds a socket makes its directory under `/tmp` explicitly,
never `TMPDIR`, which overflows `sun_path` on macOS. **C29b** (`0dbed9f`,
astra r71): C29a's credit matched by digest alone and spent the *oldest*
matching one — but identical golden frames exist in the tree
(`composer-six-lines`/`composer-nine-lines-top`, `select-two-lines`/
`select-reverse`, `task-80x24`/`task-late-80x24`), so an unspent matrix
frame's credit could be spent by an identical *direct*, in-process frame
instead — fixed by logging every frame `RunFrameScript` produces, in
production order (`frameProductions`, `internal/tui/golden_manifest_test.go:192-229`),
and judging the frame an assertion holds by the **most recent production of
its exact bytes in that one global log** — not scoped to the asserting test;
the scan stops at the first byte match, whichever test produced it
(`judgeFrame`, `:243-264`; `13`'s SF-67 (a) is what that residual costs): an
unspent matrix credit belonging to the asserting test is spent, while a
direct run, an already-spent credit, another test's credit, or no
production at all is in process alone
(`TestATransportCreditIsTheFramesOwn`, `:475-517`, a negative control
requiring the direct and matrix frames differ). Coverage is now counted per
(test, golden) pair, per iteration (`goldenAssertions`, `:318-339`): under
`-count=N` each pair must be asserted in all `N` iterations
(`goldenCoverage`, `:355-403`), `-count=0` runs no check, and the one
standing exemption — `native-tools-80x24` may go unasserted when `ripgrep`
is missing, the same rule its own test skips by (`:386-388`) — still
applies. **C29c** (`f9fdc40`, orchestrator starvation run at `0dbed9f`):
under a 5% CPU quota the socket run's capture settle outlasted
`grok-subagent-late`'s 400 ms child-finish timer and folded the finish
first, so `TestFrameGoldenGrokSubagentLate80x24` could capture the child
already gone; fixed with a new fake-agent mode, `grok-subagent-late-hold`,
which ends the parent exactly as `grok-subagent-late` does but holds the
child until the session closes (the golden's own bytes are unchanged — the
frame shows the child running either way); every other user of
`grok-subagent-late` keeps the old mode; the spinner-glyph race this golden
still carries (X38) stays, unfrozen, since freezing it would move its glyph
(the owner's call). **C29d** (`adac650`, orchestrator starvation run at
`f9fdc40`): the same quota caught two more spinner races —
`TestFrameGoldenPermissionNoForce100x30` (async in process vs `gateSync`)
and `TestFrameGoldenNativeQuestion` (socket vs `gateSync`) captured the
spinner a beat apart, since an unfrozen golden that shows the spinner races
the 250 ms tick; fixed by freezing every golden whose frozen frame already
draws `spinnerGlyphs[0]` (✳) — `ask-100x30`, `plan-100x30`,
`permission-noforce-100x30`, `grok-ask-{100x30,80x24}` and
`native-question-100x30` — which moves no byte, since that is the glyph
each already holds; `grok-subagent-late-80x24` holds ✴ (frame 1) and stays
unfrozen, freezing it would move its glyph (X38, the owner's). C30
(`docs: S2 complete`, `d6daeaf`) through this commit.

Proof: the gate passed at every commit, implementer and orchestrator, on
the exported SHA, with one diagnosed failure: the orchestrator's gate at
C29a (`680d345`, `logs/gate-c29a-orch.log`) failed in `make test-race` on
`TestASocketRunFailsOnAnyReset` — C29's own test had pinned exactly two
attaches, but X22 (§3.4's table) allows up to two re-attaches per omission,
and a run met three; diagnosed from the log, not retried, and fixed in
C29b (`0dbed9f`). Every other commit's gate passed outright, `make
test-race` included: the orchestrator's own gates at C29b (`0dbed9f`, 448 s,
`logs/gate-c29b-orch.log`), C29c (`f9fdc40`, 439 s,
`logs/gate-c29c-orch.log`) and C29d (`adac650`, 462 s,
`logs/gate-c29d-orch.log`) each passed in full — a post-C29b `make
test-race` result, not only the narrower pre-push run below. CI's
`pull_request` run on #63 also passed in full. **CI's `push` run on the
same SHA** (`efb6a3c`) **failed two tests under runner load** —
`TestAReplyFollowsItsEvents/cancel` (a PR 1 test) and
`TestNoResetEscapesTheSocketRunsVerdict/after_the_barrier` — both assumed
one legitimate ordering where two exist under load (an async settlement
landing before the reply barrier; a reset reaching the tap before the
barrier, or the view close beating the re-attach); fixed test-side, both
cases now accepted, in C29e (`21ca66d`), the commit before this one. Pre-push
`-cpu=1 -count=2` at `0dbed9f` on
`internal/tui`, `internal/remote`, `internal/backend`, `internal/transcript`,
`internal/cli` and `internal/engine`, and the mac-mini's own `-count=1` at
`0dbed9f` — all ok. V1 `-race -count=20`: at `680d345`, `internal/backend`,
`internal/engine` (835 s), `internal/agent` (1,220 s), `internal/transcript`
(706 s), `internal/control` (832 s, plus its wire test), `internal/remote`
(233 s), `internal/rundir`, `internal/protocol` and `internal/fakehost` —
all ok, 0 DATA RACE, 1,230 s wall; at `0dbed9f`, `internal/tui`
`-timeout 180m` — 4,833 s, ~242 s a pass — ok, 0 DATA RACE (the commits
after `0dbed9f` are C29c, C29d, C29e and docs, test-only or docs). V2 at
`2da9be2`: 102/103 SAME by bytes, `sigint-between-turns` byte-identical to
PR 2's recorded race variant of the baseline's own → 103/103 SAME by
content. V8 at `0dbed9f`, 20 seeds: 760 golden passes, 0 fail, 780
jittered socket runs all matching the host's model. Starvation (5% CPU
quota), every golden test, at `adac650`: 85/85 — two earlier runs at
`0dbed9f` and `f9fdc40` found three fixture races, fixed in C29c and C29d,
never retried. V4/V6/V9 live smoke at `2da9be2`
(`smoke/RESULTS-pr4.md`): Linux cursor and grok, the mac-mini's grok and
native, every leg PASS on all four, `check.py` PASS ×4. Reviews r61–r78.

### Deviations from the plan

PR 1's execution amendments X1–X26, one paragraph each except the H6 seam
group, mirrored here as `12`'s own record; the full text and every failing
schedule are in the plan (`~/.claude/plans/craze/027-session-control-s2-socket.md`,
"Execution amendments"). None reopens a pinned decision.

**PR 1** (C1–C9):

1. **Plan 027 X1, X7, X9, X11, X14 (the H6 PR 3 seam)** — H6 PR 3 (native
   sub-agent wakes, merged as `017417c` before PR 1) changes nothing on the
   wire, but PR 1 rebases onto it: `foreignTurn.reason` (an open string, `""`
   or `subagent_wake`) and `Capabilities.SubagentBackground` (wire
   `subagentBackground`) both exist and are documented; a wake has no
   `EventDone`/`EventError`, only `running: false`; a native prompt that
   claims a wake's owed drain always flushes the wake's ending first, so
   `foreignTurn{running: false}` precedes the next turn's output; H6 PR 3
   also takes SF-47 and SF-48, so any row `13` adds for S2 starts at SF-49
   (none was needed — see X13 below).
2. **Plan 027 X2 (C2, SF-13)** — the gate table (§3.5) is corrected against
   `TestTheGateTableIsTheEngines`, never the engine: `set` in the refused
   rows is `not_accepting` (not "the worker's answer"), `answer` on a
   closing/closed engine splits into `allowed` (closing — the session's own
   close has not run yet) and `already_resolved` (closed), `cancel` naming a
   stale turn is `stale_turn` in every row but closed, `interject` is
   `unsupported` when the provider cannot before it is `not_in_turn`, and a
   `waiting` row (`craze prompt`'s own foreign-turn retry policy) is added.
   The published table in `docs/reference/protocol.md` is rendered
   mechanically from this same test's table (`gateTableMarkdown()`,
   `TestPublishedGateTableIsTheTested`).
3. **Plan 027 X3 (C2)** — client lifecycle details: a client retires at `≥
   receiptAge` released with no table entry (an entry itself evicts at `>
   receiptAge`); a retired client's commands are `bad_request`, never
   reaching the wire (a failed resume answers `resumed: false` instead); a
   second release keeps the first release time.
4. **Plan 027 X4 (C3)** — SF-15/16: "any goroutine" includes the index
   worker's own inline seed. A quit while that seed is parked, owing
   nothing else, now waits up to the existing 500 ms bound instead of giving
   up at once.
5. **Plan 027 X5, X6 (C4, flagged in the report)** — two contradictions the
   draft left in the wire, resolved in the executor's favour: `hello`'s
   result shape is disambiguated by `endpoint.kind` (`"host"` carries
   `clientId`/`token`/`resumed`/`retryHorizon`, always, `resumed: false`
   included; `"hub"` carries none — no protocol change needed at S4), and
   `session.create` on a host answers like `session.connect`
   (`unsupported`, reason `hub_only`). C4's other open calls: schema `$id`s
   under `https://charliek.github.io/craze/reference/protocol/schema/`;
   lists always `[]`, never `null`; `settings.config` is `{}` when empty.
6. **Plan 027 X8, X11 (the TUI's Esc ladder, for PR 3's kickoff)** — Esc now
   cancels a running foreign turn when nothing of craze's own is working,
   already asynchronous on `main` as of H6 PR 3's own fix round
   (`cancelForeignTurn`, a `tea.Cmd`). Its residual is a small window where a
   turnless `session.cancel` can land on a drain that just claimed a queued
   row instead of the foreign turn it meant — tracked as **SF-48** and
   documented on the wire (`session.cancel`'s "no turn" text); the eventual
   fix is additive (`session.cancel{expect: "foreign"}`), not in protocol 1.
7. **Plan 027 X10 (C4)** — the frame reader tolerates a final line with no
   trailing `\n` at EOF (and strips a lone trailing `\r`): a peer that
   half-closes right after an unterminated object still gets its answer.
   Every line a host writes still ends in `\n`.
8. **Plan 027 X12, X13, X15 (C6, C6a, C6b)** — the server's binding table:
   the resume token is stable across resumes (never rotated, so two
   simultaneous resumes can still be serialised and a client that crashed
   between the reply and saving a new token does not lose its identity); a
   token is valid only on the host that issued it; a binding with no
   connection for 2× the receipts table's age bound (20 min) is dropped
   lazily, which replaced X12's proposed follow-up row (none was needed); a
   command whose binding has since moved on (a transfer, an eviction,
   another engine) is refused rather than run.
9. **Plan 027 X16, X19, X22, X25 (C7, C7a, C7b, C7e)** — attach's open calls
   and three successive review rounds converge on one rule: a replaced
   connection is **terminal-only** (superseding the earlier piecemeal
   rules). Its outbox admits exactly one terminal line —
   `reset{session_replaced}` for a live/closing attachment nobody has
   claimed the end of, or a claimed detach's own `{}` — seals on it, and
   drops every ordinary line, queued or not, giving back its slot: a client
   learns outcomes by resending after a fresh `hello` (`resumed: false` →
   outcome unknown). A barrier also waits for an attach not yet answered on
   its connection, so a reply can never precede an event the new attachment
   will forward.
10. **Plan 027 X17, X18, X20, X21, X23, X24 (C8, C8a, C8b, C8c)** — the Go
    client's (`internal/remote`) resume and resend mechanics, arrived at over
    four review rounds: one attempt of a command on the wire at a time;
    "may have run" clears only from the latest attempt's answer; resends go
    in wire order, after the re-attach is answered, never before; `resumed:
    true` is trusted only for the same client id **and the same host**;
    `busy` on a resend is waited out and resent, exactly like
    `in_progress`; the reconnect episode's clock pauses only while the
    adoption waits for the re-attach's *reply* (a write during that wait
    restarts it with the time left); the socket reader never writes, so a
    local slow consumer's re-attach or detach goes through the connection's
    one writer goroutine.
11. **Plan 027 X26 (C9)** — the fixtures live in
    `internal/fakehost/testdata/wire/` (not `internal/protocol/testdata/wire/`
    as §3.11 first said), each line `{conn, dir, msg}` plus `op` lines that
    script the host directly and an `"invalid": true` flag on fixture 10's
    deliberately malformed line. The host's incarnation crosses as a
    placeholder (`INCARNATION-1`, …), substituted both ways by the runner —
    a Stub option to pin it was rejected as a hard stop. `craze-fake-host`
    joins `make build`.

**PR 2** (C11–C15):

1. **Plan 027 X28 (C11 `b41dd5e`, C11a `339a28b`)** — the runtime namespace's
   validation departs from §3.8's "roost's rules" in three ways, each for a
   real box roost's own rules would refuse: the parent of each craze-owned
   chain is canonicalised once (`EvalSymlinks`) rather than refusing any
   symlink outright, since that would refuse Fedora Silverblue's `/home`,
   macOS's `/tmp`, and a `~/.cache` symlinked to another disk — every
   component of the canonical path is then an ancestor, a directory owned by
   root or the euid, not group- or world-writable unless sticky; a
   user-private-group exemption was tried next, for a `0775 ~/.cache` under
   umask `0002` (this box's own shape), gated on `nsswitch.conf` resolving
   `passwd`/`group` locally — and then **deleted, superseded by X30** below,
   once three astra rounds broke every attempt to prove it safe from `/etc`
   alone; every directory craze names (all four base candidates, `<ns>`,
   `.cache/craze`, `hosts`, `locks`) is a leaf — `lstat`-ed, never followed,
   created `0700` and never repaired. Also as built: the socket path's length
   is checked before anything is created; lock files are `O_EXCL` +
   `fchmod`-ed `0600`; a crashed predecessor's holder line reads as `pid ?`
   (`kill 0`); `Release` truncates the holder line before `LOCK_UN`, never
   unlinks.
2. **Plan 027 X29 (C12 `51325a4`, C11a)** — peer credentials as built: the
   §3.8 name `CheckPeer` becomes `rundir.PeerCred` (the raw lookup) plus
   `PeerCheck(want)`/`PeerCheckWith` (the server's accept check) and
   `DialCheck(want)`/`DialCheckWith` (the client's dial check), with an
   `ErrPeerUID` sentinel; `control.Options.PeerCheck` widens to
   `func(*net.UnixConn) (pid, uid int, err error)` so the connection-open
   note (and a refused note whose lookup worked) can log the peer's pid and
   uid, while the close note does not repeat them. Darwin refuses an
   `xucred` whose version is not 0 or whose group count is outside 1–16 (`
   x/sys` drops the length `getsockopt` wrote, so a zero-length answer would
   otherwise read as uid 0 — sol r28); a darwin pid that cannot be read is 0,
   not a refusal, since the uid alone decides.
3. **Plan 027 X31 (C13 `e75d0a3`, C13a `92036d2`)** — the TUI process serving
   its own session, as built: the host id is minted first in `runTUI`, so
   every SQ16 claim writes it whether or not a socket ends up binding;
   `resolveLoad` claims a `--continue`'s row before `build` is ever called,
   so a refused continue spawns no agent and creates no socket; the teardown
   lives in `internal/cli/serve.go` (not `finishRun`) and runs explicitly
   right after `tui.Run` and before the deferred stderr flush, with the
   `defer` only as the fallback for an early return — the server gets 500 ms
   to flush what it owes once the engine has closed each connection, then
   `Server.Close` is bounded at 1 s, then `Host.Close`'s identity-checked
   unlinks run, then every session claim is released last. The opt-out
   (`CRAZE_CONTROL_SOCKET`, `control_socket`) fails closed, the journal's own
   rule, since it is an access switch a typo must not open.
   `tui.Config.OnEngine` claims a new session's id (unless this process
   already holds it) and queues a registry rewrite (`enqueue`, off the
   `Update` goroutine), each write carrying the engine's complete identity;
   the queue lands on the registry's one writer goroutine, which processes a
   **current**-engine rewrite in queue order and drops one whose engine has
   since been superseded (`writeLoop`) — so a failed rewrite is made good
   only once a *next one for that engine* succeeds, and may never be, if
   none follows. Until it lands — or before the first engine is ever
   attached — the entry can still describe the previous engine, or the empty
   bind-time one.
   SQ16 itself is `atomicfile.LockWithin` (a
   bounded, polled `LOCK_NB`) plus `sessions.Store.EnsureCrazeID` (mints a
   legacy row's id once, under the index's own lock) — three refusals: a
   **held** claim (`craze: that session is open in another craze (pid N)`), a
   **busy index** (`the session index is busy — try again`), and an index
   **that changed** under the load (`the session index changed — try again`,
   astra r30: otherwise two loaders could mint two ids for one provider
   session). The initial lookup (`--continue`'s `Latest`, `--resume`'s scan)
   failing still exits 1, same as no session found — there is no row yet to
   warn about. Once a row is in hand, two things warn and proceed
   **unclaimed** instead of refusing: a **legacy** row (no `crazeId`) whose
   id cannot be written to the index, and any claim failure other than a
   held claim (the lock tree unusable, the lock file unopenable or
   unwritable; `serve.go:515`) — whether the row already had an id or was
   just given one. An unclaimed load has no protection against a second
   craze; the lock must not lock the user out of their own session over a
   filesystem fault. The
   resume picker's error row
   names the pid only, not `craze attach --session <id>`, which does not
   exist until PR 4.
4. **Plan 027 X30 (C11b `2e70c28`, C11c `75e3cf1`, C13a `92036d2`'s
   `O_PATH`) — supersedes X28 item 2.** Three astra rounds (r27, r29) broke
   every attempt to *prove* from `/etc` (passwd/group, then nsswitch) that a
   group-writable `~/.cache` is writable only by its user, so a group
   member's rename is made **harmless** instead: the cache tree
   (`<HOME>/.cache/craze/{hosts,locks}`) is walked from `/` one component at
   a time with `openat(O_NOFOLLOW|O_DIRECTORY)` (ancestors held `O_PATH` on
   Linux, so a search-only `/home 0711` still walks; darwin's `x/sys` has no
   `O_SEARCH`, so an execute-only ancestor there refuses — a residual), and
   every operation below the leaves — lock files, the registry's
   temp-then-`renameat` write, the sweep's own listing — is relative to the
   held descriptor, never to a path walked again: a rename after validation
   changes nothing craze touches. The runtime tree (the socket itself) stays
   path-based under the strict rule with **no** exemption, since `bind(2)`
   takes a path and nothing holds it open between the check and the call.
   **The stale sweep never unlinks sockets any more** — only the stale
   entry, that host's own temporaries, and its lock, all under that lock —
   because a lock in the cache tree is not authority over a file in the
   runtime tree: a crashed host's socket file stays in its runtime directory
   under a name never reused, and only a host's own clean `Close` removes
   its socket, identity-checked. Cache-tree directories are `mkdirat 0700`
   and never chmod-ed (any step by name after `mkdirat`, in a group-writable
   parent, could meet another of the user's own directories renamed into
   place); a leaf may carry setgid (inherited, grants nothing without group
   bits), while setuid and sticky are refused.
5. **Plan 027 X32 (C14 `0e8bcd2`, C14a `48c2eee`, C15 `eb305e2`)** — `craze
   bridge` and the published SSH exec, as built: the error contract lives in
   `Execute` (`root.go`), which runs `ExecuteC()` and, when the failed
   command is `bridge`, prints any error — cobra's flag and argument errors
   and the root's shared pre-run refusal included, since bridge defines no
   `PersistentPreRunE` of its own — as one `craze bridge: …` line (a leading
   `craze: ` stripped, every control character replaced by a space, so a
   newline in a workspace path cannot split it), exit 1; `--help` is still a
   success. Several live hosts with no `--session` is one line listing them;
   an explicitly passed `--session` is validated whatever its value (an
   empty `--session ''` is refused, never silently read as "no session
   given"). SIGPIPE is notified for the pump's life, so a write to a closed
   SSH channel is `EPIPE` and exit 1, never a signal death; the dial is
   bounded (10 s), and the peer check runs before the first byte crosses
   either direction. The published binary ladder (`protocol.md` "SSH exec")
   is one argv `sh -c '<ladder>'`, each rung `[ -f "$p" ] && [ -x "$p" ] &&
   exec "$p" bridge …`, tried in order — `$HOME/.local/bin/craze` (only when
   `$HOME` is itself absolute, `case "${HOME:-}" in /*) …`, so a relative or
   empty `HOME` cannot make this rung exec something relative to whatever
   directory the shell started in), `command -v craze` (accepted only
   absolute), `/opt/homebrew/bin/craze`, `/usr/local/bin/craze`,
   `/home/linuxbrew/.linuxbrew/bin/craze`, `/usr/bin/craze`,
   `$HOME/.nix-profile/bin/craze` (the same absolute-`HOME` guard),
   `/etc/profiles/per-user/$USER/bin/craze`, `/run/current-system/sw/bin/craze`,
   else `craze: command not found` at exit 127 — following roost's ladder and
   craze's own Homebrew tap; every interpolated value, the session id
   included, is shell-quoted going in. Verified by running it with `sh -c`
   the way an SSH exec would. Craze ships no remote-exec code of its own; the
   ladder is published for shed (or anything else driving this over SSH) to
   copy verbatim.

**PR 3's execution amendments X33–X48**, one paragraph each, mirrored here
as `12`'s own record; the full text and every failing schedule are in the
plan (`~/.claude/plans/craze/027-session-control-s2-socket.md`, "Execution
amendments — PR 3"). Review rounds: `reviews/dispositions-pr3.md`. None
reopens a pinned decision.

**PR 3** (C16–C23):

1. **Plan 027 X33 (C16 `33cdebb`)** — the Backend as built: `Model.eng` keeps
   its name, typed `backend.Backend` — H7 PR 2 edits `app.go` in parallel and
   a rename would turn every `m.eng` hunk into a conflict; its doc says it is
   the session's backend, in process now and a socket in PR 4. Every Backend
   command, `Read`, `Ask` and `Settings` take a `context.Context` first, the
   permanent interface (SD-33) — `ClientID`, `Started`, `Close`, `Info` and
   `Epoch` take none (the backend epoch rides in the ctx), and all but `Close`
   wait on nothing: `Close` can block while the agent is reaped.
   In process the non-waiting verbs pass no ctx to the engine; since C18c
   every command, `Ask` and `Settings` check the ctx's epoch first
   (`internal/tui/engine_backend.go:120`). `Settings` narrows to `{Model,
   Mode, Config}`, carrying no
   provider — the effort/fast vocabulary does not depend on the provider
   today (`agent/provider.go:624-633`), so the chains' captured
   `m.snap.Provider` is invisible to every test. `Backend.Info()` is C20's;
   tests reach the engine through `engineOf(t, m)`, so C21's later deletion
   of the transitional `State()` touches no test.
2. **Plan 027 X34** — a test-placement note, not a behaviour change: the
   gate's mechanism tests (FIFO order, nesting, the deadline release, the
   byte bound) land in C17 over a test-only gated operation plus `/rename`;
   the site-specific gate tests land with their sites in C18a, C18b and C18c.
3. **Plan 027 X35 (C17 `b8bf66b`)** — the gate as built: the release Update
   runs the old Update wrapper (`finish`) after the continuation, so nothing
   held is applied in that Update; the held queue's bound is queue-wide — a
   gate opened while the backlog is already at the bound releases
   `ErrNoAnswer` at its first held message, keeping memory bounded; `m.run`
   panics if a gate is already open or there is no backend. `runFrameModes`
   runs 111 frame goldens through both the synchronous and asynchronous gate
   modes (the six picker goldens, pre-start dialogs where no gate can open,
   run in one mode only). `Makefile`'s `test-race` timeout moved to 10 m (was
   5 m): `internal/tui`'s `-race` run went from ~60 s to 105–180 s running
   every golden twice.
4. **Plan 027 X36 (C17a, astra r41)** — the frame barrier's rendezvous: a
   short turn's held `started`/`ended` could drain right after the release
   and, if the runner resumed late, the barrier's "latest frame" was already
   idle, clearing the release's working frame. Fixed structurally in the
   harness: the program now pauses after publishing a frame that
   acknowledges a sync token until the runner has taken that barrier, so the
   barrier's latest frame is always the acknowledgement and every later frame
   stays queued for the next wait. **Residual, documented:** a card that
   opens and closes on its own before the runner reaches it can still be
   matched by `await`'s oldest-queued fallback — today's `await` semantics,
   not the gate's, unreachable in a golden whose card waits for the script's
   answer.
5. **Plan 027 X37 (C18a `5c801ee`)** — Submit through the gate as built:
   `submit` is continuation-passing, so only two callers above the gate have
   post-call work left to move. The band's transitional read moves into the
   call itself (read at the release, applied by the continuation) rather
   than re-reading `refreshQueue` afterward: reading only at the release let
   an armed send-now that fired right after `Submit` returned drop a row the
   same Update's frame had shown. `ErrNoAnswer` writes to the status line and
   keeps the shell context. The gate re-raises a gated call's panic on the
   command's goroutine, where bubbletea recovers it — C17's bare goroutine
   would have crashed the process with the terminal left raw. The pump's
   `pumpApply` waits out a gate its own key opened, delivering only that
   gate's replies and setting other arrivals aside for later waits, matching
   the synchronous-era pump's own behaviour.
6. **Plan 027 X38** — a pre-existing golden flake found inside C18a's gate:
   `TestFrameGoldenGrokSubagentLate80x24` raced the TUI's 250 ms tick against
   the fake agent's `grok-subagent-late` script, which ended the parent turn
   about 250 ms in. Fixed in the fake agent alone — the script now ends
   mid-way between beats; no golden's bytes moved. **Residual:** the two
   clocks stay unsynchronised, so the fix narrows the race (0/180 under load)
   rather than removing it; removing it means freezing the golden, which
   moves its bytes — the owner's call.
7. **Plan 027 X39 (astra r43)** — accepted residuals after C17a: a gated call
   that never returns keeps both its worker and, after a deadline release,
   its `linger` goroutine — bounded by however many calls are already stuck,
   each of which already kept its worker. C17b's unification of a runner's
   key and its sync token into one program message closes the pre-send
   window structurally.
8. **Plan 027 X40 (C18b `c396b35`)** — the queue verbs and the Ctrl+C/Esc
   chains as built: the band's transitional read moves into the call for
   every queue verb, as X37 did for Submit. `clearPending`'s `Disarm` and
   `ClearQueue` are **one gated call**, run back to back on the call's
   goroutine (`internal/tui/app.go:2406-2461`) rather than two gates: the
   implementer found that two gates put a reply's round trip between them,
   letting the arm's own in-flight cancel settle and drain the queue head
   before the second gate's `ClearQueue` landed — a row the user was clearing
   would run. One call keeps today's in-process window and call order.
   **Residual for PR 4**, recorded as `13`'s SF-50: over the socket the two
   calls become two sequential round trips, so closing the window needs a
   combined engine verb (a wire change). A non-empty edit whose row drains
   while its save goes unanswered is ended by `syncQueue`, which restores the
   pre-edit draft and loses the edited text — today's existing rule for any
   row that leaves the queue mid-edit, which the gate only reaches by a rare
   no-answer route (its 15-s deadline, or the held queue's bound); keeping the edited text is a UX change, recorded as
   `13`'s SF-51. `handleQueueKey` returns `(bool, Model, tea.Cmd)`. V1's
   `internal/tui` `-race` pass runs under `-timeout 150m`, not 60 m: the
   both-mode goldens' 1,200+ schedule orders make one pass ~165 s, so 20
   passes exceed the plan's original bound.
9. **Plan 027 X41 (C17b `9749185`)** — the frame runner made deterministic: a
   key, mouse or resize token and its sync token are delivered as **one**
   program message, applied back to back in one frame-model Update, so the
   token always follows its key. The run's shutdown is deferred, and the
   capture is of a settled model — after the last token, the runner waits for
   a frame with no gate open, nothing held and the fold at the stream's head.
10. **Plan 027 X42 (C17c `1040cda`; the harness review converged at astra
    r47)** — the frame runner's final shape: a sync token waits where its key
    waits, parked under an open gate only when nothing is held, otherwise
    held behind what arrived first. The capture settles on the newest frame,
    then sends a harness-only quit through the model's own FIFO, so the
    captured frame is the model after every message that reached the program
    before the quit, in order, and none after it (a message arriving after
    the quit may remain held — an r47 restatement, not an overclaim). The
    baseline (`gateSync`) capture changed the same way; every golden is
    byte-identical in both modes. Shutdown is bounded: the engine is closed
    (unblocking a call an Update waits in), the program killed, then waited
    for. **Residual, pre-existing:** the engine's `Close` in this test-only
    shutdown path sits outside every timeout, since session close is
    unbounded by contract — the test binary's own timeout is the backstop.
11. **Plan 027 X43 (C18c `1dd6fe3`)** — answers, interjections and the mask
    as built: a card's answer is a gate; Interject is a gate with a 60-s
    deadline whose `ErrNoAnswer` keeps the draft and the shell context. The
    masked opening (`pushCard` while `cardMasking`) is the one gate an
    **event** opens. Hidden answers stay fire-and-forget Cmds in both gate
    modes, never gated. The session generation (`m.sessGen`) and the backend
    epoch (`Backend.Epoch()`, `internal/tui/engine_backend.go:58`) fence
    every command, both chains and the hidden answers: a gate reply from
    another session releases its gate without running the continuation
    (dropping it outright would leave the gate open forever; C27 decides what
    a restore does to an open gate). V8's seeded-jitter run (0–10 ms, 33
    golden tests, 38 runs, `CRAZE_V8_SEEDS=20`, async only): 760 passes, 0
    diffs.
12. **Plan 027 X44 (C19 `fd72eee`)** — the Stub publishes what a live session
    would: `InstallOnStart` is on for `internal/tui`'s Stubs through a
    package `TestMain` default (`internal/tui/stub.go:99`); `SetCommands`/
    `SetPlugins` publish their one-section delta and flush, as the live
    session's catalog update does. **Found:** the install reaches far fewer
    unit tests than planned, since the Stub's `Start` runs only where
    something calls it. **Decided for C21:** `startSession` starts the
    session as `Init` does — one helper line, measured green across the whole
    suite, reaching 412 of 917 tests at the time — recorded rather than put
    to the owner (decide-don't-ask). **Plan correction:** the install delta
    cannot end "a transcript run with a new entry", since the fold's meta
    handler touches no transcript; only seq numbers move.
13. **Plan 027 X45 (C20 `a2655f1`, C20a `f604b2a`, C20b `ad200c0`)** —
    ordered tools and `Info`, as built: `Backend.Info()`'s `Workspace` is the
    TUI's own resolved cwd (`internal/tui/engine_backend.go:225`), passed in
    rather than read from the engine, which holds none. `m.caps()` reads
    `Info()` — the engine's `State()` — on every call, several per frame, but
    the engine's index I/O runs outside its lock, so no UI stall. The tool
    projections `Tools()` (C20a) and `State().Tools` (C20b) are the caller's
    own — both clone each tool's nested fields (`History()`'s entries still
    share their payload, SF-53) — so two tests that had compared a
    `State().Tools` value with the folded payload by pointer now compare by
    value, their meaning unchanged. One unexplained `tests/cli`
    failure in C20's implementer gate was closed as not PR 3's — 0/30 at both
    the failing and the fixed commit in isolation, confirmed by H7's own
    author — a watch, not a fix.
14. **Plan 027 X46 (C21 `a280912`, C21a `d086fe4`)** — the mirror is the
    fold, as built: `recompute` (`internal/tui/mirror.go:39`) is the one
    writer of `m.snap`, `m.queue` and the send-now mirror, built from a new
    `transcript.Model.Mirror()` read (`internal/transcript/mirror.go:48`,
    under the lock, without copying any transcript), `Backend.Info()` and the
    overlays; `refreshSnap`, `refreshQueue`, `verbRead` and `refreshAfter` are
    gone, and `Backend.State()` is deleted. The mode overlay carries `Rev`,
    since a Set to the current mode publishes no delta and would otherwise
    never retire. **Two plan gaps found**, both given a TUI-side workaround
    pending a source fix: (a) the fold's roster does not carry a grok child's
    `Activity` (it moves with each tool call, `agent/tools.go:150-153`, with
    no roster event), so the mirror derives it from the child's own tool-call
    titles until the next roster event carries the row whole
    (`internal/tui/mirror.go:74-84`) — recorded as `13`'s SF-54, for carrying
    it in the fold; (b) the fold's send-now stays armed after the send fires,
    since firing publishes no delta, only the `started`
    (`internal/tui/mirror.go:69-73`, `engine.go`'s `nextLocked`) — must be
    fixed at the source for PR 4, carried into `kickoff-pr4.md` and recorded
    as `13`'s SF-55. C21a widened `Mirror()` to deep-copy every slice and
    pointer it hands out (astra r53), with a reflective ownership test.
15. **Plan 027 X47 (C22 `674da35`)** — two clients on one queue, as built:
    the sent row is drawn from `SubmitResult.Text`; `expectedVersion` is
    captured at edit start from the row as the band shows it and sent on
    every save. **UX, decided by the orchestrator:** a stale refusal does not
    strand the edit — the text stays, nothing is installed, and the edit's
    version refreshes to the row's current one, so a second Enter knowingly
    saves over the other client's change (residual: a change not yet folded
    when the refusal lands makes the second Enter a refusal too, never an
    overwrite of a change this client was never shown). The edited-row
    overlay's version is `expectedVersion + 1` — the engine's confirmed
    answer, since the check (`engine/queue.go:87`) and `PromptQueue.Edit`'s
    `Version++` (`agent/queue.go:145`) run in one locked section.
16. **Plan 027 X48 (C23 `240718f`)** — the sub-agent stop, as built:
    `stopSubagent` (`internal/tui/subcancel.go:91`) returns a `tea.Cmd` (the
    command id and `dispatchCtx` taken in the Update, no result message,
    every error ignored as before), threaded through `handleRowsKey` (now
    `(bool, Model, tea.Cmd)`) and `handleViewKey`; never gated, the same in
    both gate modes. `TestStopKeyStopsTheRunningChild` was rewritten, since it
    had encoded the synchronous call this commit removes.

**PR 4's execution amendments X49–X58**, one paragraph each, mirrored here as
`12`'s own record; the full text and every failing schedule are in the plan
(`~/.claude/plans/craze/027-session-control-s2-socket.md`, "Execution
amendments — PR 4"). Review rounds: `reviews/dispositions-pr4.md`. None
reopens a pinned decision.

**PR 4** (C26–C30):

1. **Plan 027 X49 (C26)** — SF-50 stays open: Ctrl+C's `Disarm` and
   `ClearQueue` stay two engine calls, so over the socket the window X40 2
   named becomes two sequential round trips rather than the one local round
   trip in process. A combined verb needs a wire change (a new method or
   param behind a capability); nothing asks for it yet, and the fix is
   additive when a client does.
2. **Plan 027 X50 (C26 `9acd8d1`, C26a `4c37522`, C26b `851b34b`; astra
   r61–r64, converged)** — `remote.Session` as built: `DialSession` checks
   the host's codecs and says hello; `Attach` is exported so the golden
   matrix attaches before the host starts; `Start` returns once readiness is
   received or the host's start fails (`*StartError`). `Read` delivers
   `ItemRestore`, `ItemEvent`, `ItemReady` and `ItemEnd`; a decode failure
   ends the Session as `End` does. Commands send the caller's own command id
   (`CommandOptions.ID`), and a command or read binds to the client
   **identity number** current at its entry — never the id's spelling, which
   a restarted host mints again — atomically with the client id;
   `Epoch()` is that identity, and it moves on every `hello` that did not
   resume. `backend.ErrOutcomeUnknown` covers resume_lost, disconnected and a
   stale-epoch command; a stale *read* is plain `backend.ErrStaleEpoch`.
   `*remote.Error.Is` maps `(code, reason)` through one table
   (`internal/remote/errors.go`); `TestEveryReasonReconstructsItsSentinel` and
   `TestEveryTUIErrorSiteWorksOverTheWire` (an AST inventory) cover it. Three
   review rounds fixed, in order: (r61) a command with no epoch bound by the
   client id's *spelling*, not its number, across a host restart; an unsent
   command bound to the old identity left unresolved at adoption; an event
   whose body fails the codec ending the stream without closing `s.ended`; (r62)
   adoption not waking a bound read (`changedLocked` added); a `Ready`
   received after `broken()` still closing `ready`; a post-call epoch check
   dropped, so a buffered reply on a stale identity could still be returned.
   r64: no finding, the C26 series converged.
3. **Plan 027 X51 (the prelude, `fd5a7e3`)** — SF-55 and SF-54 fixed in the
   fold: every `send_now`-origin `started` ends the fold's arm (the engine
   holds one arm, a `send_now` Submit while armed is `already_submitted`, and
   the started and a later arm are ordered under the engine's lock — no wire
   change, no `Cause` needed); a child's `EventTool` with a title sets its
   roster row's `Activity` (`agent.SubagentActivity`, `internal/transcript/fold.go:170-172`,
   `:483-491`) until a roster event replaces the row. The mirror's `armFired`
   and `childActivity` compensations are gone; two `fixture:` guards that
   pinned the gaps were inverted. **Residual (astra r63 8b), a `13` row
   (SF-56):** the fold's activity is still the PR 3 mirror's rule — native
   sets `Activity` on `ToolCalled` only, and grok changes it on an update it
   does not publish — so it is a derivation until a provider publishes it as
   a roster fact.
4. **Plan 027 X52 (C27 `9118a9f`, C27a `72ed882`, C27b `eef9775`; astra r63,
   r65, r67, converged)** — the TUI's restore as built: `tui.Config.Backend`
   builds no engine, picker, `OnEngine`, index or claim. The reader delivers
   every item (`eventMsg`, `restoreMsg`, `readyMsg`, `endMsg`), each held and
   drained through the gate; older-generation events are dropped **when the
   restore is HELD**, not when applied (§3.14 said "when applied" — with one
   read outstanding they always sit ahead of their restore, so a drop at
   application would come too late). `applyRestore` (`internal/tui/restore.go:81`)
   rebuilds the fold, every pane, a card per open ask in opening order, the
   turn, the foreign turn, replaying, the queue band and every revision
   guard; it clears every overlay, echo marker, the cancel mask and the plan
   offer. A restore from another incarnation ends any queue edit, the
   send-now confirm and the Ctrl+C window, and a pending plan implementation;
   `cancelled` is kept only for the same running engine turn; the replay
   guard's empty-snapshot rule is narrowed to the first or a same-incarnation
   restore. `End` quits (`m.ended`, `m.endErr`). Three review rounds fixed a
   queue edit surviving a restore from another incarnation, the empty
   pre-start restore clobbering `Config.Loading`, a send-now confirm
   surviving another turn's restore, the empty-snapshot replay-guard rule
   over-applying, and a pending plan implementation surviving a
   same-incarnation restore. **Residuals, `13` rows:** (SF-57) a turn that
   failed while a client was disconnected restores as idle, because the
   snapshot's `Turn` keeps no ending outcome; (SF-58) a remote model whose
   `Start` failed does not recover on a restored replacement incarnation —
   unreachable in S2, since a host replaces its engine only in the pre-start
   pickers, and a start failure offers no picker; S4's headless hosts make it
   reachable; (SF-59, astra r67) a restore keeps a foreign episode's plan
   evidence, so a restored foreign turn's `Done` with no text of its own can
   re-arm the implement offer from the previous episode — a spurious *offer*
   only, never an unasked action.
5. **Plan 027 X53 (C28 `7b3c1f5`, C28a `2da9be2`; sol r66, r68, converged)**
   — `craze attach` as built: resolution is the bridge's matcher
   (`matchSession`), cwd compared absolute, cleaned and symlink-resolved on
   both sides; titles come from the session index by craze id, never by
   connecting; an invalid explicit `--session` is exit 2; resolution runs
   before the tty check, which is now a real termios check shared with the
   host TUI (`craze >/dev/null` is refused too). `tui.Config.Viewer` clears
   the pickers, session builders, claims, `PersistProvider`, the index,
   `OnEngine` and the host reporter; the shell runs in `Info().Workspace`.
   SQ16 attaches (`internal/cli/tui.go:305-318`, `:366-393`,
   `internal/cli/attach.go:406-436`): a `--continue` row with a craze id is
   claimed first — held routes to the attach path (the note names the spawn
   flags it ignores), claimed routes to the spawn-flag refusal, releasing the
   claim on a refusal; a legacy row keeps the refusal before any index write.
   The resume picker's refusal names `craze attach --session <hostId>`
   (`internal/cli/serve.go:538-566`) only when the holder serves that exact
   session, else why it cannot be reached. Review fixed: `resolveLoad`
   applying the spawn-flag refusal before learning a row was held (a held
   native session with `--agent-bin` refused instead of attaching);
   `holderEntry` matching the holder's host id alone, not also its claimed
   session; the tty check accepting any character device; `attachExit`
   checking `Ended` before a `tui.Run` error. **Residual, a `13` row
   (SF-60):** the attach TUI's permission chip shows craze's own default
   (yolo) — the host's `--force` is not on the wire.
6. **Plan 027 X54 (C28b dropped; the owner's rule, header 5)** — the ask-note
   marks fit the rule, but the model/title notes would duplicate what an open
   model dialog or an in-flight `/model` chain already shows: three
   deliberate tests (`TestAnotherClientsModelChangeBeforeTheEffortIsANote`,
   `TestAnotherClientsModelChangeMidChainIsANote`,
   `TestATabThatAppearsWhileOpenIsSeeded`) pin "no extra note" for a foreign
   change landing during this client's own chain or box. Status quo; SF-18
   stays in `13` with this finding.
7. **Plan 027 X55 (C29 `1256e18`)** — the goldens over the socket, as built:
   `FrameOpts.transport` is unexported and test-only, installed by a hook
   (`frameSocketHook`), so `craze frame` stays in process and production
   links no server. `runFrameModes` runs three: `gateSync` and async in
   process, async over the socket — all three end on the same frame and
   error. The socket run builds the session `NoPrimary` through the same
   builders, serves the host engine over `internal/control` on a directory
   made under `/tmp` (not `t.TempDir()`, which overflows `sun_path` on
   macOS) with `MaxBudget` and the attach budget raised to 1M items / 1 GiB,
   and attaches `when: "now"` before the host's `Start`; a second dial, a
   second attach or any reset fails the run. `TestSocketGoldensMatchTheEngine`
   pins an exact common seq at a sync-token rendezvous, since "once the host
   is quiescent" cannot otherwise be enforced. The coverage manifest lists
   119 golden files — 113 under both transports, the six picker frames in
   process only (`internal/tui/golden_manifest_test.go:460-461`).
   `TestAttachMidTurnOverTheSocketReproducesTheFirst`
   (`internal/engine/exactness_socket_test.go:184`) lives in `internal/engine`
   beside A2's recorder: every cut of a recorded Stub trace (144 events, 145
   cuts) attaches over a real server and folds to the first client's model,
   plus eight mid-turn cuts over the fake agent. The two-client tests run two
   pump-driven remote models on one host, since `RunFrameScript` serialises
   on one HOME. Cost: `internal/tui` 70 → 93.5 s (`test`), 187 → 251 s
   (`-race`) on this box; `Makefile`'s `test-race` timeout is 15 m (moved in
   PR 3, X35, confirmed still enough for PR 4's heavier `-race` pass).
   **Fix round C29a (`680d345`; astra r69).** Two blockers: transport credit
   had been recorded per *test*, not per frame, so a second `RunFrameScript`
   in one test inherited an earlier frame's `{inproc, socket}` record with no
   suite-level check that every golden was actually asserted both ways —
   fixed by a credit keyed to (test, frame digest) and `TestMain`'s
   `goldenCoverage`, which fails an unfiltered run — no
   `-run`/`-skip`/`-list`/`-update` — if any manifest golden was never
   asserted under its full transport set (the one exemption is
   `native-tools-80x24` when its test itself skips, no `ripgrep`).
   `CRAZE_GOLDEN_TRANSPORT` could be narrowed to `inproc` by an exported env
   var reaching `make test`/`make test-race` and CI alike, silently
   weakening the coverage check to match — fixed by pinning
   `CRAZE_GOLDEN_TRANSPORT=both` inside both `Makefile` targets and CI's
   `test` job; a local run now narrows the matrix only by invoking `go test`
   directly. One major: a reset the server wrote after the run's comparison
   but before the client's reader had consumed it could escape the tap's
   verdict — fixed by a final `session.sync` barrier on the model's own
   connection (answered only once every record up to it is queued to the
   connection) plus requiring the view close's `session.detach` to have been
   answered too, both read back through the tap
   (`internal/tui/frame_socket_test.go:335-409`). One minor: every socket
   test's directory is now made under `/tmp` explicitly (`os.MkdirTemp("/tmp",
   …)`) rather than the default temp dir, which overflows `sun_path` under a
   long macOS `TMPDIR` — not only the golden harness's own hosts, but every
   test across `internal/cli`, `internal/control`, `internal/engine`,
   `internal/fakehost`, `internal/host`, `internal/remote` and `internal/tui`
   that binds a control socket.
   **Fix round C29b (`0dbed9f`; astra r71).** C29a's credit matched by digest
   alone and spent the *oldest* matching one, but identical golden frames
   exist in the tree (`composer-six-lines`/`composer-nine-lines-top`,
   `select-two-lines`/`select-reverse`, `task-80x24`/`task-late-80x24`), so
   an unspent matrix frame's credit could be spent by an identical *direct*,
   in-process frame instead — fixed by logging every frame `RunFrameScript`
   produces, in production order (`frameProductions`,
   `internal/tui/golden_manifest_test.go:192-229`), and judging the frame an
   assertion holds by the **most recent production of its exact bytes in
   that one global log** — the scan is not scoped to the asserting test and
   stops at the first byte match, whichever test produced it (`judgeFrame`,
   `:243-264`, spent through `checkGoldenTransports` at
   `internal/tui/frame_test.go:426`; a residual of this shape is `13`'s
   SF-67 (a)): an unspent matrix credit belonging to the asserting test is
   spent, while a direct run, an already-spent credit, another test's
   credit, or no production at all is in process alone
   (`TestATransportCreditIsTheFramesOwn`, `:475-517`, a negative control
   requiring the direct and matrix frames differ). One
   major, also fixed: with `-count=2` a golden covered in the first
   iteration hid a socket run skipped in the second, one persistent coverage
   entry per pair — fixed by counting coverage per (test, golden) pair, per
   iteration (`goldenAssertions`, `:318-339`): under `-count=N` each pair
   must be asserted in all `N` iterations (`goldenCoverage`, `:355-403`).
   One minor: `-count=0` used to fail the coverage check though it runs
   nothing — fixed to run no check.
8. **Plan 027 X56 (C29c `f9fdc40`)** — found by the orchestrator's own
   starvation run (5% CPU quota) at `0dbed9f`: under that quota the socket
   run's capture settle outlasted `grok-subagent-late`'s 400 ms wall-clock
   child-finish timer, so `TestFrameGoldenGrokSubagentLate80x24`'s socket
   run could capture the child already gone, where the in-process run still
   caught it running. Fixed with a new fake-agent mode,
   `grok-subagent-late-hold`: it ends the parent exactly as
   `grok-subagent-late` does, but holds the child running until the session
   closes, so the timer can never race the capture; the golden's own bytes
   are unchanged, since the frame shows the child running either way. Every
   other user of `grok-subagent-late` keeps the old, timed mode. The
   spinner-glyph race this golden still carries (X38) is unchanged and
   stays unfrozen: freezing it would move its glyph, which is the owner's
   call, not this fix's.
9. **Plan 027 X57 (C29d `adac650`)** — found by the orchestrator's next
   starvation run at `f9fdc40`: the same 5% CPU quota caught two more
   spinner races between frame-modes —
   `TestFrameGoldenPermissionNoForce100x30` (async in process vs
   `gateSync`) and `TestFrameGoldenNativeQuestion` (socket vs `gateSync`)
   captured the 250 ms spinner tick a beat apart between their two runs,
   since an unfrozen golden that shows the spinner races that tick. Fixed
   by freezing every golden whose frame already draws `spinnerGlyphs[0]`
   (✳) — `ask-100x30`, `plan-100x30`, `permission-noforce-100x30`,
   `grok-ask-100x30`, `grok-ask-80x24` and `native-question-100x30` — which
   moves no byte in any of them, since ✳ is what each already held; the
   rest of the spinner goldens were frozen already. `grok-subagent-late-80x24`
   holds ✴ (frame 1), not ✳, so freezing it would move its glyph; it stays
   unfrozen (X38, the owner's).
10. **Plan 027 X58 (C29e `21ca66d`, C29f `ff1badb`)** — CI's findings on #63,
   all fixed test-side. The first push's `push` run failed two tests under
   runner load (the `pull_request` run on the same SHA passed): PR 1's
   `TestAReplyFollowsItsEvents/cancel` assumed a cancel commits nothing
   before its reply barrier's door (its settlement is asynchronous), and
   C29a's `TestNoResetEscapesTheSocketRunsVerdict/after_the_barrier`
   expected one of two reset-caused failures; each now accepts the other
   legitimate outcome (r80; the after-barrier arm's inability to prove its
   own reset occurred on the unanswered-detach path is r71's documented
   limitation). The second push's runs failed
   `TestAttachMidTurnOverTheSocketReproducesTheFirst` under `-race`: one 40 s
   budget over the 145-cut loop (about 24 s under `-race` at full CPU; it
   fails at a 50% quota), so the budget is now per step (r82: a fold hung
   under its client's mutex is not bounded by its watchdog, a pre-existing
   test-only liveness edge; the slow cut follows the trace's ~1 MiB edit
   report). Each diagnosed from its log or a measurement; none retried.

### S2 — as shipped

The roadmap's S2 exit (`07`), clause by clause, plus SD-33's addition (full
detail in the plan's §7 acceptance table, A1–A25):

- **Two TUIs on one live session show the same transcript** (A1): the live
  exit smoke on Linux (cursor, grok) and the mac-mini (grok, native), with the
  two TUIs' transcript regions compared line by line at quiescent points
  (`compare.py`) — SAME on every leg of every run
  (`027-session-control-s2-socket/smoke/RESULTS-pr4.md`). Automated:
  `TestSocketGoldensMatchTheEngine`, `TestAttachMidTurnOverTheSocketReproducesTheFirst`.
- **A prompt from either appears in both** (A2): V4/V6 leg 2, every run;
  `TestAPromptFromEitherClientAppearsInBoth`.
- **An ask answered in one closes in the other** (A3): V4/V6 leg 3 (a plan
  card on cursor, a question on grok and native); `TestAnAnswerClosesTheCard`-
  class tests and `TestAnAskAnsweredInOneClosesInTheOther`.
- **Kill and reattach resumes silently from `afterSeq`** (A4): the silent
  cursor resume itself is proven in `internal/remote`
  (`TestAKilledConnectionResumesSilently`, `resume_test.go:26`;
  `TestAClientProcessRestartResumesFromItsCursor`, `resume_test.go:232`) and
  by PR 2's V3 live legs — the bridge process killed and the client resuming
  with its persisted token and cursor, and the smoke client itself `kill -9`-ed
  and restarted from its own persisted cursor file, in both cases with no
  `Restore` item on screen. `craze attach` (PR 4) has no persisted cursor to
  resume from: **V4/V6 leg 7 is a different case, a snapshot attach** — the
  `kill -9`-ed attach's replacement is a *fresh* `craze attach`, which takes a
  new snapshot rather than resuming (what it visibly lacks against the host is
  F1, `smoke/RESULTS-pr4.md`).
- **A deliberately stalled client is reset `slow_consumer` without delaying
  the agent** (A5): `TestAStalledClientIsResetWithoutDelayingTheAgent` (PR 1);
  no live leg needed one in PR 4.
- **The socket is refused for another uid** (A6): `TestAnotherUIDIsRefusedBeforeAByteIsRead`,
  `TestAFailedCredentialLookupRejects` (PR 2).
- **Live smoke on Linux and the mac-mini** (A7): V4 (Linux, cursor and grok),
  V6 (mac-mini, grok and native — cursor skipped there, the login keychain
  over ssh, as every earlier phase found too), V9 (`check.py` PASS on all
  four journals). Two findings, both accepted rather than fixed in S2 — see
  `13`, SF-61 and SF-62 — and two cosmetic observations (SF-63; native's
  roster rows lingering ~10 s past attach, outside the transcript region).
- **SD-33's addition — the full TUI runs unchanged over the socket, goldens
  included** (A8, A9): every golden in `golden_manifest_test.go` runs under
  both transports, byte-identical, except the six picker frames (in process
  only, since pickers exist only in the host TUI); no golden file's bytes
  moved in PR 3 or PR 4, C29c's and C29d's fixture and freezing fix rounds
  included (`git diff --stat -- '*testdata*'` empty at every commit through
  `adac650`). Coverage is
  enforced, not just recorded: `assertGolden` judges the frame it holds by
  the most recent production of those exact bytes in one global log, across
  the whole binary rather than scoped to the asserting test — a matrix
  run's unspent credit, or in process alone (`judgeFrame`,
  `internal/tui/golden_manifest_test.go:243-264`; `13`'s SF-67 (a) is that
  scan's residual) — and `TestMain`'s `goldenCoverage` (`:355-403`) fails an
  unfiltered run — no `-run`/`-skip`/`-list`, `-update` off, `-count` above
  0 — if any manifest golden, except `native-tools-80x24` when `ripgrep` is
  missing, was not asserted under its full transport set in every one of the
  run's `-count` iterations (C29a `680d345`, C29b `0dbed9f`); `make
  test`/`test-race` and CI's `test` job pin `CRAZE_GOLDEN_TRANSPORT=both`,
  so this coverage cannot be silently narrowed there — only a direct
  `go test` invocation narrows
  it.

`craze attach` (PR 4) is the first full client the protocol has ever had
outside the host TUI itself and the test harness: it is what actually proves
one attachment per connection (SQ14), the full TUI over the socket (SQ7), and
`--continue` of an open session attaching (SQ16) rather than only refusing.
What `13` still owes after S2 is closed out below and in `13` itself; nothing
open blocks the exit clauses above.

## S4a + S5 — detached hosts and the agent view

| | |
|---|---|
| Status | complete (Plan 030, FINAL after panel rounds 1 and 2, 2026-09-28); all five PRs done, PR 4 carrying this record |
| Plan | `030-session-control-s5-agent-view` (outside the repo, `~/.claude/plans/craze/`; research, discovery reports, the mockup, the raw panel reviews, every review round's disposition and the V1–V5 artifacts in its folder) |
| Baseline | `origin/main` `9606fc5` (#64, Plan 029's wrap-up), on top of S2's last PR #63 `73ed5e0` |
| Branch / PRs | five sequential PRs, each branched from a freshly fetched `origin/main`: `docs/plan-030-roadmap`, `feature/plan-030-hosts`, `feature/plan-030-sessions-list`, `feature/plan-030-new-sessions`, `feature/plan-030-composer-at` |
| Merged | PR 0 — #66 `77f1cd3` (2026-09-28); PR 1 — #68 `6be2273` (2026-09-29); PR 2 — #70 `b8aa5bb` (2026-09-30); PR 3 — #71 `b401fec` (2026-09-30); PR 4 — `feature/plan-030-composer-at`, rebased on `b401fec` |

### The PR cut

| PR | branch | content |
|---|---|---|
| 0 | `docs/plan-030-roadmap` | the roadmap refactor: `07`'s order column and the S4a/S4b/S5 rewrite, SD-34, SD-35, `13`'s rows, S2's PR 4 record and X58 |
| 1 | `feature/plan-030-hosts` | `craze serve`, spawning and the ready handshake, `session.stop`, lifetime (`/exit`, idle exit), the launch flow, the client gaps SF-57 (part), SF-60, SF-63 |
| 2 | `feature/plan-030-sessions-list` | the list (`internal/roster`, row facts on the wire, the screen), `switchBackend` (opening a session in place), saved sessions |
| 3 | `feature/plan-030-new-sessions` | the always-focused input, `@` directories, dispatch, `/provider` and `/model`, the completion component |
| 4 | `feature/plan-030-composer-at` | `@` file and directory mentions in the session composer |

PR 1 ships detached hosts with no list and is usable on its own (`craze -c`
reattaches after closing a tab).

### Owner decisions (2026-09-28)

Taken with the owner in the planning session (`research/DECISIONS.md` in the
plan folder has them in order); not for the panel to reopen.

1. **Order.** The agent view is next, before the shed lane; S3 waits until
   shed has made its first release of its own lane work. S4's detached host is
   folded in as the plan's first code PR; the hub, `craze ps` and a combined
   roster stay for later (SD-34).
2. The roadmap in this folder is refactored for the new order and scope, as
   part of the plan (PR 0).
3. The list shows every running session of this user on this machine,
   whatever its directory.
4. One line per row, grouped by state by default; a key toggles grouping by
   directory.
5. Not in the MVP: a preview of the selected session; answering an ask from
   the list (you open the session to answer); notices of other sessions inside
   a session; git worktrees (wanted later).
6. The UI generally follows Claude Code.
7. New sessions start from an always-focused input on the list, with `@`
   choosing the directory: recent directories where sessions ran plus a typed
   location completed with `Tab`. Nothing is hard-coded: no projects folder,
   no setting.
8. The selected row sets the directory a new session runs in; `@` overrides it.
9. `/provider` and `/model` in the list's input set what new sessions use.
10. `@` file and directory mentions in the session composer are added in this
    plan.
11. `/exit` (one quit path with `ctrl+d` and the second `ctrl+c`) ends the
    session you are in, in every client including `craze attach`. Closing the
    terminal leaves the session running. `/exit` in the list's input quits
    craze and leaves every session running (SD-35).
12. Idle timeout: 1 hour, like Claude Code, configurable; a session with
    nobody attached and nothing in flight (SD-35).
13. The `detach = false` opt-out stays for now; revisit later (a `13` row).
14. Decide, do not ask: the rest are the planner's, recapped at the end.

### The planner's decisions

Before the panel:

- Phase IDs stay stable: S4 splits into S4a and S4b; order S4a + S5, S4b, S3,
  S6/S7 (SD-34).
- Five PRs (the cut above).
- `craze serve` is the headless host, a real subcommand; the ordinary `craze`
  spawns it and attaches. Pickers stay in the TUI. The host owns the claim,
  journal and index writes. roost/herdr status is reported by the client TUI.
- The list polls each host's socket (no hub). Opening a session swaps the
  TUI's backend in place, with per-session composer drafts. `@dir` + `enter`
  opens an unstarted session (spawned on the first prompt). Dispatched
  sessions take the permission mode of the session you came from. ACP model
  catalogs are cached for `/model`. One new completion component; `slash.go`'s
  tokenizer is untouched. Composer `@` inserts `@path` as text. No blinking
  and no mouse in the list.

After the panel (UX-visible):

- No `alt+←`/`alt+→` cycling: those are the composer's word motion, and macOS
  Terminal sends `ESC b`/`ESC f`. The band keeps `← sessions`; you switch via
  the list. A `13` row records cycling.
- Grouping toggles with `ctrl+s` (Claude Code's key); `ctrl+g` is craze's
  theme picker.
- A viewed session that ends returns you to the list instead of quitting
  craze.
- The agent view exists only when sessions are detached (not under
  `detach = false`, where closing a backend would close its engine).
- A host that never received a prompt exits after 5 minutes idle (it has
  nothing to resume); a host whose start failed exits as soon as no client is
  attached, after the startup grace.
- `/exit` on an older host that cannot stop detaches and says so.
- `ctrl+x` on a working row cancels the turn and clears its queue (otherwise
  the queue drains and the row stays working).
- **The list is labelled "sessions"** (header ` sessions`, band `← sessions`,
  builtin `/sessions`), not "agents" as in the mockup: craze already has
  `/agent` (set agent mode) and says "agents" for sub-agents (`← n agents`).
  Offered to the owner with the plan; reverting it is a text change.

### Plan review — 2026-09-28

| round | reviewer | result |
|---|---|---|
| 1 | Codex `gpt-6-astra`, GLM 5.3, CodeRabbit | 29 findings, all adopted, none declined; no reviewer disagreed with another; all three verified the plan's file:line claims |
| 2 | Codex `gpt-6-astra` on the revision | 19 of round 1's 29 resolved, the rest partial; 1 blocker, 7 major, 4 minor, all adopted |

Round 1's substance: a detached host must set `agent.Options.NoPrimary`;
`session.stop` needed a real stop protocol (`Backend.Stop`, a lifecycle
coordinator) because `Engine.Stop` does not close the engine; `switchBackend`
needed a backend generation on every stream message and a fresh-session-state
constructor; the spawn handshake (fd 3, close-on-exec, timeout kill, identity
before ready); idle counted attached clients, not connections; capabilities
`stop` and `rowFacts` announce the new wire behaviour (the schema is closed);
`LastTurn` on state and rows only, not in the snapshot (a codec bump would
break attach across versions); `alt+←/→` and `ctrl+g` conflicts; the goldens
rule (new catalog entries only when a session list is configured).

**Round 2's blocker: the idle decision's atomicity.** Marking a host
"closing" refused attaches only, so an unattended prompt, a settings change or
foreign work could race the exit, and pending or reserved attachments went
uncounted. Fixed with a **close fence** over the server's attach reservations
and the engine's admission (`Engine.FenceClose`): eligibility is checked under
both, and the fence is reversible. The seven majors: killing the serve process
group leaks the ACP child (graceful `SIGTERM` first, agent process groups
recorded for the last-resort kill); a fast start failure could vanish before
the launcher attached (startup grace, rendezvous on published identity);
dispatch and the unstarted path's adoption split; `sessGen` and `gateSeq`
monotonic across switches with stale gate replies rejected first; legacy saved
rows have no craze id (`--load <provider>:<id>`); a post-restore `LastTurn`
read could install an obsolete ending (`LastTurn.TurnID`); the roster's
deadline must cover dial and hello, with a non-reconnecting client. The four
minors fixed wording (`/sessions` and composer `@` against the golden rule),
`StartErr` on the wire, `@` token submission semantics and the catalog cache's
freshness race. Round 2's fixes were not re-reviewed as a plan; the executor's
per-commit astra reviews of the lifecycle commits carry that check.

### Roadmap wording this plan departs from

- `07`'s S5 was "a TUI screen that is a hub client" that answers an ask from
  the list and starts a new headless session. S5 now polls each host's socket
  (no hub until S4b), does not answer from the list (you open the session),
  and starts new sessions from an input with an `@` directory picker.
- `07`'s S4 was one phase (headless hosts, the hub, `craze ps`). It splits:
  S4a (detached hosts) is built here with S5; S4b (the hub, `craze ps`,
  `session.create`) follows, before S3. S4's exit is split accordingly.
- `07`'s S5 exit ("blocked on an ask and answered from the view") becomes
  "opened and answered".
- `SQ12`'s default ("a plain quit still ends the session, with a confirm when
  a turn is working") is refined by SD-35: closing the terminal leaves the
  session running, and an idle timeout ends unattended hosts.
- `01`'s "the TUI as a client of a hub": the list reads the registry and polls
  each host's socket until S4b's hub exists.

### Outcome

Shipped in five PRs, each gated per commit. PR 0 refactored this roadmap for
the new order (SD-34, SD-35). **S4a (PR 1):** every craze session runs in a
detached host that outlives its terminal — `craze serve`, which the ordinary
`craze` spawns (`setsid`, stdio on `/dev/null`, a ready line on an inherited
pipe written after the identity-bearing registry write) and then attaches to
as a socket client. `session.stop`, behind a new `stop` capability, makes
`/exit` (with `ctrl+d` and the second `ctrl+c`) end the session in every
client, `craze attach` included; closing the terminal leaves it running; an
unattended host exits after `host_idle_exit` (1 h by default, 5 minutes for a
session never prompted), a decision made atomic by a close fence over the
server's attach reservations and the engine's admission
(`Engine.FenceClose`). Every client now shows the host's permission mode,
start time and last turn (SF-60, SF-63, SF-57 in part). **S5 (PRs 2–4):** `←`
on an empty composer, or `/sessions`, opens a list of every running session of
this user on the machine, polled host by host (`internal/roster`), grouped by
state (`ctrl+s`: by directory); `enter` opens a session in place
(`switchBackend`, a backend generation on every stream message) or resumes a
saved one, and `ctrl+x` cancels a turn or closes a session. The list's input
starts a new session in the background, a leading `@` token choosing its
directory, or opens an unstarted one in place; `/provider` and `/model` choose
what new sessions run, ACP catalogs coming from a new cache
(`internal/modelcache`) that each host writes. Composer `@` completes the
workspace's files and folders into `@path` text. The `detach = false` /
`CRAZE_DETACH=0` / `control_socket = false` opt-out keeps the in-process path,
with no list. No wire change beyond what the plan pinned (§3.6a, §3.8:
`session.stop`, the `stop` and `rowFacts` capabilities, the info document's
`permissionMode` and `startedAt`, `lastTurn`, the row facts, the `closing`
reason; fixtures 14–17, additions).

**The roadmap's S4a and S5 exits** (`07`):

| criterion | result | evidence |
|---|---|---|
| S4a: after a prompt, killing the terminal leaves the host listed and `craze -c` reattaches with the transcript | pass | V4 leg 2 (`tmux kill-session` of three sessions: every host alive, answering `hello`), leg 3 (cosmic-term quit); V5 leg 10 (an ssh logout; the next login's `craze -c` attached to the same host, nothing spawned); `test_hangup_leaves_the_host_and_dash_c_reattaches` |
| S4a: `/exit` ends the session and the host's registry entry is gone within seconds | pass | V4 leg 5 (client, host and entry gone at the first 0.1 s poll); V5 leg 5 (host and entry within 0.01 s, the client at 0.15 s); the index row kept both times; `test_quit_ends_the_session` |
| S4a, S5: an unattended idle host exits after its timeout; one with a turn, an open ask or an attached client does not | pass | V4 leg 9 (`host_idle_exit = "2s"`: gone 2.74 s after the detach), V5 leg 9 (2.76 s); the never-prompted 5-minute cap, V4 leg 3 (5 m 0.8 s); `TestEachInFlightConditionKeepsTheHost`, `TestAnAttachArrivingAtExpiryKeepsTheHost`, `TestAListPollerDoesNotKeepAHost` (`internal/cli/idle_test.go`); `test_idle_host_exits_and_keeps_its_row` |
| S5: close every terminal, reopen craze, and `←` lists the sessions still running | pass | V4 and V5 leg 2 (listed under idle, each opened in place with its transcript); `test_enter_opens_a_session_in_place_and_ctrl_d_leaves_them_running` |
| S5: one blocked on an ask is opened and answered | pass, with a caveat | V4 and V5 leg 4 (`question: Which colour do you prefer?` under "needs you"; opened; answered). A card up blocks `←` and `/sessions`, so on Linux the list was reached from a second terminal (SF-99) |
| S5: a new session is started in another directory from the list with `@` | pass | V4 and V5 leg 6 (`started in …`; `@proj-b` alone opened an unstarted session that spawned nothing until its first prompt); `test_a_prompt_starts_a_session_in_another_workspace`, `test_an_at_directory_alone_opens_an_unstarted_session` |
| S5: `/exit` ends a session and the list shows it saved | pass | V4 and V5 leg 5 (the row under saved; `enter` resumed it in place, `restored`); `test_a_saved_session_resumes_in_place` |

**The proof.** Every commit was gated — the full gate (`make lint && make
test && make test-race && make build && make test-cli`), by its implementer
and again on the committed tree (a few small follow-up commits by the next
tip's run) — green throughout (about 445–517 s; `tests/cli` 178 → 208
tests), and every commit was reviewed: gpt-6-astra (45-minute cap) for the
lifecycle commits (C1–C5, C11, C15) and their fix rounds, gpt-6-sol (20-minute
cap) for the rest, 39 rounds (r1–r39), plus whole-PR astra reviews of #68 and
#70 and CodeRabbit on #66 and #71. One blocker in all (C3: a stale recorded
pgid could be signalled; now identity-checked, X22); every finding and its
disposition is in the plan folder's `reviews/dispositions-pr1.md` …
`dispositions-pr4.md`, and each PR's accepted risks are `13` rows. Every new
lifecycle test ran under a 5 % CPU quota as it was written; the lifecycle
schedules are forced with seams, not left to repetition. The verification
plan (plan §8):

- **V1** (`-race -count=20` on the changed packages, per PR): PR 1 no data
  race, two test-side flakes fixed on the branch (`cb0d184`, `ec21fad`);
  PR 2 all green, `DATA_RACE=0`; PR 3 all green, `DATA_RACE=0`, except
  `TestFrameGoldenLoadLongReplayDoesNotDeadlock` once in 20 — a pre-existing
  wall-clock bound (it fails 7/7 at a 25 % quota under `-race` on `main`
  `b8aa5bb`, before PR 3, as on PR 3's tip), fixed in PR 4 (X192). PR 4's run goes with its CI.
- **V2** (`craze prompt --json` parity, 103 scenarios): PR 1 102/103, PR 2
  103/103, PR 3 103/103 at `4e586ec` and 102/103 at its tip, PR 4 102/103 at
  `202928d`. The one DIFF is `sigint-between-turns`, a pre-existing race (X59,
  SF-87: 15/20 DIFF on a `main` with no Plan 030 code) — in PR 4's run it was
  the baseline whose SIGINT landed late, after the third prompt was sent; the
  output did not move.
- **V3** (launch cost, the fake agent, 20 interleaved runs): time to the first
  answer in process p50 48 / p95 59 ms, detached p50 56 / p95 77 ms — **+7 ms
  p50, +18 ms p95**, against a 150 / 300 ms budget.
- **V4, V5** (live smoke, Linux and the mac-mini): every leg run passed; one
  leg not run on each platform (below, "Live smoke").
- **V6** (goldens): `git diff --name-status origin/main -- '*testdata*'` showed
  only `A` at every commit — 32 new frame goldens (1 in PR 1, 12 in PR 2, 15
  in PR 3, 4 in PR 4; the manifest from 113 under both transports + 6 in
  process to 115 + 36) and wire fixtures 14–17. No existing golden or fixture
  moved.
- **V7** (CI, ubuntu and macOS): green at every pushed tip of PRs 0–3; PR 4's
  with its push.

### What shipped per commit

**PR 1 — `feature/plan-030-hosts`** (#68, merged 2026-09-29 as `6be2273`;
the `*r` commits are review-fix rounds, and
`030-session-control-s5-agent-view/reviews/dispositions-pr1.md` has every
finding and its disposition):

- C1 (`16e8981`) — the wire: `session.stop` behind the new `stop` capability
  (`Backend.Stop`, `control.Options.Stop`), the attach fence
  (`FenceAttaches`) and the `closing` reason, the info document's
  `permissionMode` and `startedAt`, and `lastTurn` in `session.state` and a
  roster row; fixtures 14–16 (additions) and the published schema. C1r
  (`19a95ed`) makes a stop's receipt precede everything the stop puts on its
  connection.
- C2 (`8011307`) — `craze serve`: the headless host in the foreground (the
  session flags, `--load`, `--log`), built as the TUI builds a session,
  served over the socket with the lifecycle coordinator as its stop seam.
  C2r (`5aa6fa5`) makes the claim a condition of running and reworks the
  host-log sweep to decide a host's death by its own lock; C2r2 (`a9507ee`)
  holds one validated cache parent for the sweep and claims a new session's
  id before anything is built.
- C3 (`412782b`) — the spawner: `spawnHost` (setsid, stdio on `/dev/null`, a
  ready pipe), the ready line (`ok`, `held`, error), the rendezvous with a
  holder, and the agent-group record the launcher's last-resort kill reads.
  C3r (`a9ff24c`) makes the record prove whose group it is
  (`rundir.ProcessIdentity`), required to run, and outlasting the start.
- C4 (`614d9e9`) — the launch: the ordinary `craze` spawns a host and runs the
  TUI as its client (`internal/cli/launch.go`, `detachOn`), the spawn-failure
  UX, `-c`/`--resume` attaching to a held session, cleanup of a host the
  launch spawned whose session never came up. C4r (`cb34190`) keeps a host only
  once the TUI took its start, separates a refusal from a host that could not
  come up, and attaches a reattach directly.
- C5 (`8f055b8`) — lifetime: `/exit` (and Ctrl+D, the second Ctrl+C) stops the
  session on its host (`stopQuit`), the idle watcher and `host_idle_exit`, the
  engine's close fence, `Engine.Busy`, socket-lost exit, `host_stop` journal
  diag, a start-failed host stays attachable. C5r (`5d3be17`): owed background
  work and admitted-but-unfinished commands keep a host, a failed start retires
  replay, one quit deadline; C5r2 (`37b073c`): the deadline ends a stop it
  cannot reach, and a direct reattach attaches before it commits.
- C6 (`be31640`) — client gaps: the TUI shows the host's facts (permission
  mode, start time, last turn), the workspace follows the session, SQ16's
  wording. C6r (`9dbe89f`): a failure reported before the session came up
  stands; one frame run counts elapsed from one start on both transports.
- C7 (`d9845f7`) — `tests/cli`: the core cases run detached and in process,
  `test_detach.py`. C7r (`0a2e86d`): a start failure keeps its row through a
  restore, and the test hosts' cleanup is exact. C7r2 (`ad0d2c6`): a start
  failure belongs to its incarnation; the macOS process scan is bounded. C7r3
  (`b7e3078`): a regression test for that scan.
- C8 (`abd93de`, C8r `31448ef`) — docs: `craze serve`, the launch behaviour and `craze attach`'s quit in
  `docs/reference/cli.md`; `detach`, `CRAZE_DETACH`, `host_idle_exit` and the
  host logs in `docs/reference/configuration.md`; the host/client split in
  `docs/development/architecture.md`; this record; `13`'s SF-80..SF-87.
- C8r2 (`d5f069f`) — this record after the rebase onto #67. On the pushed
  branch: `e659bb0` (astra's whole-PR review, r16: a detached session cannot
  run without its socket, in the docs) and V1's two test-side flake fixes,
  `cb0d184` (fixture 14's `run_stop` waits for the coordinator's stop) and
  `ec21fad` (the test waits for a host to close its ready pipe).

**PR 2 — `feature/plan-030-sessions-list`** (#70, merged 2026-09-30 as
`b8aa5bb`; the `*r` commits are review-fix rounds, and
`030-session-control-s5-agent-view/reviews/dispositions-pr2.md` has every
finding and its disposition):

- C9 (`30014cc`) — the list's data: the engine's row facts (`Doing`,
  `LastReply`, `Since`, the head ask's `Summary`, `StartFailed`/`StartErr`,
  `Prompted`) on the `sessions.list` row behind the new `rowFacts` session
  capability (fixture 17, an addition); `internal/roster`, a poller over the
  registry — one kept, never-reconnecting connection per host, one attempt
  budget, at most 8 in flight, a backoff, unreachable never saved — and over
  the index for the saved rows; `tui.Config.Sessions` and the launch's
  `sessionList`; a 50-host scale test. C9r (`dd0e739`): one deadline per
  attempt, `Close` joins, a host the list's `Spawn` started is the launch's
  until opened or left running.
- C10 (`bff36d2`) — the list screen: `←` on an empty composer and
  `/sessions` (only with a session list), routed ahead of every other key;
  the header and its counts, the groups by state and by directory
  (`ctrl+s`), the row mapping and columns, selection and an armed close held
  by identity, `ctrl+x` (cancel with the queue cleared; a two-press close),
  quitting from the list leaving every session running, the too-small and
  empty states; goldens at 100×30 and 80×24 (additions). X92 (`af1d22a`): a
  pre-existing composer hang (`alt+←` over blank text) found and fixed.
  `b9a7e0b`: a pre-existing harness race (X93) fixed test-side. C10r
  (`df5d03f`): the ended row outlives its host, "here" is an incarnation,
  every exit closes the roster, `ctrl+x`'s clear-then-cancel proven on the
  wire.
- C11 (`94f12e9`) — opening a session in place: `switchBackend` (a backend
  generation on every stream message, one per-session constructor shared with
  `New`, stale replies rejected first), the dial as a `tea.Cmd` whose late
  answer is closed, per-session drafts, the band (`title · provider · dir`,
  `← sessions`), the sub-agent hint `↓ n agents`, and a viewed session's end
  returning to the list; goldens of an opened session (additions). C11r
  (`178b7c2`): a paste, a copy's note or a `!` result never crosses a switch
  (with an audit of every message kind that reaches `Update`), and the band's
  title is the row's.
- C12 (`65388e6`) — saved sessions: `enter` on a saved row resumes it in place (a host
  spawned with `--load` in the row's own workspace, or the holder when it
  runs after all), a row this craze cannot run refused on the hint line;
  `tests/cli/test_sessions.py` (the list in real terminals: groups,
  regrouping, cancel, close, opening in place, quitting, an unreachable host,
  a resume) and the fake agent's `CRAZE_FAKE_SESSION_ID`; the session list,
  opening in place, the band and saved sessions in `docs/reference/tui.md`;
  `internal/roster` in the architecture's package table; this record.
- C12r (`0fcb40e`) — tests (sol on C12): two resumes truly at once, the
  cleared queue proven, this record current. C11r2 (`1fc9468`, astra on C11r
  and C12r): a paste asked for before the first adoption stays with that
  session (X121). `ebcd90b` merged `main` (#69, `d8c3276`) into the branch.
  C12r2 (`25deb26`, astra's whole-PR review, r27): a lost connection is not
  the session's end; the roster's first tick publishes (X142–X144).

**PR 3 — `feature/plan-030-new-sessions`** (#71, merged 2026-09-30 as
`b401fec`; the `*r` commits are review-fix rounds, and
`030-session-control-s5-agent-view/reviews/dispositions-pr3.md` has every
finding and its disposition):

- C13 (`3f51bfe`) — the completion popup (`internal/tui/complete.go`): one
  component for every input that completes a token — a source answers at
  once or names a keyed load run off the Update, stamped and dropped once no
  longer awaited; `↑`/`↓`, `tab` (descend into a directory), `enter`, `esc`;
  up to eight rows and a `↓ N more` line; the `@` token's grammar (`@name`,
  `@"a path"`).
- C14 (`88d672a`) — the list's input (only with a `tui.SessionStarter`, so
  PR 2's frames stay byte-identical): the rule naming the target, the leading
  `@` token and its binding, the `@` directory source (here, running
  directories, `sessions.Store.RecentDirs`; paths browsed a level at a time),
  its error words; goldens (additions).
- C15 (`8d13fa9`) — starting a session from the list: the background
  dispatch (`Spawn` → its own connection, drained → `Start` → the prompt with
  that connection's first command id → `LeaveRunning` → close; accepted,
  refused, outcome unknown), the unstarted session (`@dir` alone: opened in
  place, spawned and adopted on its first prompt with the TUI's own command
  numbering, discarded by `←`), `/exit` in the input; goldens (additions).
- C16 (`f3cce7b`) — `/provider` and `/model` in the list's input: the `/`
  popup, the providers the startup picker offers, the model reset to the
  provider's default (native's from its model table, through the seam Plan
  031 swaps), the catalog cache (`internal/modelcache`, written by `craze
  serve` after each catalog install through the engine's new
  `Options.CatalogChanged`, `rundir.CatalogDir`); `tests/cli/test_dispatch.py`
  (a prompt dispatched into another workspace, the unstarted session, and
  `/provider` `/model` changing the next dispatch, in real terminals); the
  input, `@`, dispatch, the unstarted session and `/provider` `/model` in
  `docs/reference/tui.md`, the catalog cache in
  `docs/reference/configuration.md`; this record.
- C15r (`f266967`) — review fixes (astra on C15, sol on C16): the dispatch's
  prompt waited for no longer than its deadline even when its write is
  blocked; the exit closing a dispatch's connection at once; a list paste
  landing only in the opening that asked for it; the unstarted session's
  shutdown schedules proven in a pty; the agent's own default a row of its
  own; a catalog with a bad model not recorded.
- C15r2 (`4e586ec`) — review fixes (astra on C15r): a dispatch's answer read
  by its error, not the clock; the exit cutting a dispatch's detach short.
- C15r3 (`f096a27`) — tests (astra on C15r2): the two regression tests made
  to fail on the parent's code, deterministically.
- The record's commit (`885b43c`) — X163–X176 below.
- C15r4 (`ef8116a`) — CodeRabbit on #71: an unstarted session's first
  prompt dropped when its session ends behind the list (X177).

**PR 4 — `feature/plan-030-composer-at`** (C17–C19 implemented 2026-09-30,
rebased onto `b401fec`; the `*r` commits are review-fix rounds, and
`030-session-control-s5-agent-view/reviews/dispositions-pr4.md` has every
finding and its disposition):

- C17 (`ab8dcc9`) — the composer `@` search and match
  (`internal/tui/at_files.go`): `rg --no-config --files --hidden -g '!.git'
  -0` when `rg` is on the `PATH`, else `git ls-files -co --exclude-standard
  -z` in a work tree, else a breadth-first walk; one 3 s budget, 50,000 paths
  (20,000 entries for the walk), a partial list offered and titled; clean
  relative paths only, folders derived from the files; a simplified fzf score
  over the relative path, a `/` narrowing to a folder; the best 100 handed to
  the popup and the rest counted (`completeAnswer.More`, new — with it zero,
  every existing golden is unchanged).
- C18 (`0c14708`) — the popup and the insertion (`internal/tui/composer_at.go`):
  `Model.composerAt`, open only while the composer has the keyboard, drawn
  where the slash menu draws, its keys ahead of the composer's, titled
  `files in <workspace>`; the audit of every existing frame script (no `@` at
  a word start in the session composer, X185); four goldens (additions);
  `test_tui_composer_at_mentions_a_file` in both modes; `## File mentions` in
  `docs/reference/tui.md`.
- C18r (`c033c5d`) — review fix (sol on C18): the slash menu stays out of an
  `@` token (X193).
- C17r (`63aba2e`) — review fixes (sol on C17): what a listing holds is
  capped (100,000 candidates, 8 MiB), the listing runs on its own goroutine
  so the popup never waits on a stuck call, and git's paths are checked on
  disk (X194–X197).
- C17r2 (`03ba3c0`) — review fix (sol on C17r and C18r): the tool is reaped
  on a goroutine of its own, whatever the listing is inside (X198). Its
  re-review's one finding (a pipe's read end closed twice on cancel) was
  accepted: `os.File.Close` guards the second close.
- `202928d` — the long-replay golden's wall-clock bound, found by PR 3's V1
  (X192): its wait fails on a stall in progress rather than a fixed 20 s; and
  a plugin frame test's wait needle no wrap can split (X199).
- C19 (`fa91c12`) — the phase record: `07`'s exit results for S4a and S5
  and its table, this section's outcome, PR 4's deviations, the live smoke,
  the decisions and the handoff; `13`'s SF-88–SF-103 and the rows Plan 030
  closed; the README's status; what each provider does with `@path` and a
  list dispatch's saved provider in `docs/reference/tui.md`.
- C19r (`dc0a764`) — review fixes to the record (sol): the ranges reach X199 and
  SF-102, and the long-replay test's comment calls its stall a measured bound.
- C18r2 (`c01bfb9`) — CodeRabbit on PR 4: a composer `@` row shows a path's
  Unicode format characters as `<U+XXXX>` (X200); SF-97 reworded; SF-103.
- C18r3 (this commit) — review fix (sol on C18r2): the row escapes Unicode's
  whole default-ignorable set, not only format characters (X201).

### Deviations from the plan

PR 1's execution amendments X1–X62, PR 2's X63–X121 (with C12r2's
X142–X144), PR 3's X122–X141 and X145–X177, and PR 4's X178–X201 (X192
among them, found by PR 3's V1), mirrored here as `12`'s own record (the full
text is in the plan, `~/.claude/plans/craze/030-session-control-s5-agent-view.md`,
"Execution amendments"); review-fix rounds are grouped with the commit they
amend. None reopens an owner decision.

**PR 1** (C1–C8):

1. **Plan 030 X1 (C1)** — the info document's `permissionMode`/`startedAt` and
   `lastTurn` are `omitempty`, and the fake host leaves them unset by default
   (an older host's document); an opt-in mode with a pinned clock sets them
   for new fixtures. The 13 existing wire fixtures do not move, and double as
   the old-host/new-client direction of §3.8.
2. **Plan 030 X2, X3, X4, X5, X6, X7 (C1, C1r)** — the seams: `Backend.Stop(ctx,
   c engine.Command)` (the caller's command id, not its own); the stop
   coordinator is `control.Options.Stop`, called at most once, raising the
   attach fence, then queueing the `{}` receipt outside the writer budget,
   then stopping (a later stop joins and is answered `{}`; a client treats a
   session end while its stop is outstanding as the answer); the fence is
   `(*Server).FenceAttaches()`, stackable, held from check to install
   (`reserve`'s refusal order: replaced, `already_attached`, `closing`);
   `closing` is a new reason under `unavailable`; `StartedAt` is the host's
   session start, fixed for the server's life; `LastTurn` is recorded in
   commit order and cleared when any turn starts (failed = the ending carried
   an error; cancelled = `cancelled` or the close; done = anything else,
   foreign turns included).
3. **Plan 030 X8, X9 (C1)** — `session.stop` is not a row of the engine's
   generated gate table (a second `Engine.Close` cannot be driven in its
   "closing" row): its entry is prose above the `gate-table` markers in
   `docs/reference/protocol.md`. The fixtures' `README.md` stays at 13 (it is
   under `testdata/`); fixtures 14–16 are documented in `protocol.md`,
   `internal/fakehost/doc.go` and `wire_test.go`.
4. **Plan 030 X10, X11, X12, X13, X14, X15 (C2)** — `craze serve` is listed in
   `craze --help` (the root's Flags section is byte-identical); `serveFlags`
   embeds `tuiFlags` (refusals read as the root's, `serve`'s own read `craze
   serve: …`); the index-title rule uses `transcript.SanitizeLine` (exported);
   `--load` runs in the row's own workspace (another `--workspace` is exit 2,
   a gone directory exit 1); a held `serve` exits 1 with the picker's refusal
   text, carrying `*rundir.HeldError`; the stop sequence runs on `runServe`'s
   goroutine (`hostLifecycle` only records requests), `Start` runs with
   `context.Background()`, and the join after a stop is bounded at 5 s.
5. **Plan 030 X16, X17, X18 (C2, C2r, C2r2)** — the host log also takes crash
   output (`debug.SetCrashOutput`) and fatal errors; `SweepHostLogs(keep)`
   decides each host's death by its own host lock and removes nothing if the
   registry directory fails validation; the fake agent gains a `long-reply`
   script (600 chunks). `serve` requires its session claim (an unusable lock
   tree or a legacy row that cannot be given its id is exit 1 with nothing
   left behind; the root keeps its warn-and-run fallback), and a new
   session's craze id is minted and claimed before anything is built
   (`engine.Options.MintedCrazeSessionID`). `flushWait` became a package
   variable (a pre-existing teardown-test flake under a 5 % CPU quota, fixed
   test-side).
6. **Plan 030 X19, X20, X21, X22 (C3, C3r)** — the ready line: not-ok carries
   `held{hostId, pid, crazeSessionId}` (the pid added), unknown fields are
   ignored, the parent mints the host id and passes the hidden `--host-id`,
   `--log` is `host-logs/<hostId>.log` and a `--log` there not named for its
   own host is exit 2; `--no-host-status` is hidden and passed through.
   Agent groups are recorded by a spawned host only, and X22 replaces X20's
   kill rule: each `<hostId>.pgids` line is `<pgid> <start>` from
   `rundir.ProcessIdentity` (Linux `/proc/<pid>/stat` field 22, macOS
   `kinfo_proc`), signalled only when a process with that pid and start time
   exists; a record that cannot be made fails the start (`agent: the agent's
   process group could not be recorded: …`). Handshake details: `ok` waits
   for the first identity write that lands, an EPIPE stops the host,
   `CRAZE_READY_FD`/`CRAZE_HOST_CHILD` leave `serve`'s environment, and EOF
   with no line waits the 5 s grace. Ready lines are validated per branch.
   Accepted residuals and the pre-existing `acp.Child.Shutdown` hazard are in
   `13` (SF-80, SF-81).
7. **Plan 030 X23, X35 (C4, C5)** — the launch first attached when the host
   was ready, not "now" (a failed start closed the event log before any ready
   notice and the TUI quit `craze: session ended`, exit 0); C5 fixed the root
   cause (a start-failed host keeps answering) and the launch attaches `now`
   again, so a load's replay streams in progressively.
8. **Plan 030 X24, X25, X26, X27 (C4)** — spawn-failure UX: a host that could
   not come up names its log and ends `; CRAZE_DETACH=0 runs sessions inside
   craze instead`, exit 1; a host that refused the choice brings the picker
   back with the refusal as its error row (for `-c`, a start failure without
   the hint). `--resume`'s held row attaches as `-c` does. `NewBackend(p,
   explicit bool)` and `Config.Continue *sessions.Row`: the launcher passes
   `--provider` unless the choice is the fallback nobody picked; `-c`'s flag
   refusals are checked before the TUI starts unless a live host serves the
   session. Quitting stops any host this launch spawned whose session never
   came up, never a held one; a held attach prints, after the TUI, `craze: …;
   attached to it (ignored: …)` only when flags were ignored.
9. **Plan 030 X28, X49 (C4, C5r)** — tests stay in process by default:
   `internal/cli`'s `TestMain` and `tests/cli/conftest.py` set
   `CRAZE_DETACH=0` (C7 runs the core cases in both modes); every
   `CRAZE_CLI_TEST_CHILD` process watches `CRAZE_CLI_TEST_PARENT` and exits
   when it is gone (SIGTERM, then 98 after 5 s; 30-minute cap).
10. **Plan 030 X29, X30, X31 (C5)** — the idle fence: an attachment counts from
    reservation to close, except that a read EOF takes it out of the count (a
    half-closed bridge that only reads does not keep an idle host);
    `Server.FenceClose` raises the attach fence, then the engine's, the
    watcher decides on "attached 0 and not busy", a stay releases the engine
    fence first; while fenced the engine refuses (`ErrClosing`, reason
    `closing`, never stored) Submit, Queue, EditQueued, Unqueue, ClearQueue,
    Disarm, Interject, Set, SetTitle and GiveUpDrain, and the driver starts
    nothing (Cancel, Stop, GiveUp, CancelSubagent and Answer are not fenced).
    `engine.Busy()` is the fence-less sample. Residual: SF-83.
11. **Plan 030 X32, X33, X34 (C5)** — `host_idle_exit` (a Go duration ≥ 0, `"0"`
    or `"never"`; absent → 1 h silently, bad → 1 h with one host-log line;
    a 1 s tick, a 60 s grace from the host's start, never-prompted capped at
    5 min, a start-failed host's limit 0 even under `"never"`); socket lost is
    checked first every tick (`Host.Lost()` compares dev/ino) and is the
    ordinary resumable stop; a start-failed `serve` host stays listable and
    attachable (`agent.Options.KeepLogOnFailedStart`, `serve` only).
12. **Plan 030 X36, X37, X38, X39 (C5)** — `/exit` → Stop, client side, only for
    a backend served elsewhere, one 2 s budget for receipt and end, a second
    quit exits at once; stop taken prints nothing, a host that cannot stop
    prints the older-craze note (exit 0), any other failure prints `craze: the
    session may still be running: <err>` (exit 0, a line the plan did not
    pin); SIGTERM and SIGHUP stay view closes; only a clean session end (not a
    transport loss) answers an outstanding stop. The help dialog's
    golden-pinned wording is unchanged (SF-86). The stop order is `host_stop`
    note, fence, engine close, S2's close order, join the start (5 s), release
    claims, remove the record; on a join timeout the host seals the record and
    kills its own recorded agents (SIGTERM, 2 s, identity re-check, SIGKILL).
    Every stop journals its cause as a `host_stop` diag.
13. **Plan 030 X40, X41, X42, X43 (C4r)** — retention keys on the TUI's
    acknowledgement (`AckStarted()` on the launched backend; a signal that
    beats `startedMsg` stops the host); the not-ok ready line gains optional
    `"refused": true` (absent = could not come up; on an ok line, malformed),
    so refusals of the chosen session keep the picker and everything else is a
    start failure from pickers too (exit 1); the host checks a `--resume`'s
    flags after its claim, so a held row attaches whatever they say; `-c` and
    a `--resume` choice with a craze id attach directly to a live entry (no
    spawn, log or claim), a failed dial falls back to the spawn, legacy rows
    always spawn.
14. **Plan 030 X44, X45, X46, X47, X48 (C5r)** — owed work keeps a host
    (`agent.OwedWork`; native's `BackgroundOwed()`: a result running or
    pending, not reserved, suspended or undelivered ones); `admitLocked`
    counts an admitted command whose work runs after `e.mu` is released
    (`SetTitle`, `Interject`, `Submit`'s first index write) until it ends; a
    failed start retires replay; one quit deadline, taken when the quit is
    asked for, closing through `remote.Session.CloseWithin`; the explicit
    quit's outcome wins (no stream-end line after a stop note). Residual:
    SF-84.
15. **Plan 030 X54, X55 (C5r2)** — the quit's deadline ends a stop it cannot
    reach (`awaitStop`; at the deadline it records `ctx.Err()`, closes the
    transport with no detach and joins the stop for at most `quitJoinWait`,
    1 s; `remote.Client.Command`'s write still does not honour its context);
    a direct reattach dials and completes `Attach(now)` within `dialTimeout`
    before taking the host, and any failure falls back to the spawn.
    Residual: SF-82.
16. **Plan 030 X50, X51, X52, X53 (C6)** — SQ16's wording is `that session is
    already running (pid N)` (the plan's literal substitution read "open in a
    running session"; `rundir.HeldError.Error()` too) and the resume picker
    keeps the text after " — " whole on its row; the workspace follows on
    adopt, not on restore, and a non-viewer's shell runs in the session's
    workspace; the post-restore `LastTurn` read applies only if the epoch, the
    session generation, the restore count and the turns begun are unchanged
    (C11 adds `bgen`), a failed last turn sets the same error state a live
    failure does, `LastTurn(ctx)` is a required `Backend` method; a new
    golden `restore-failed-80x24` is in-process only (manifest: 113 both + 7
    in-process) and `ErrClosing` reads "the session is closing".
17. **Plan 030 X56, X57, X58 (C6r)** — a failure reported before the session
    came up stands (`sessionUp` no longer idles an error state); a frame
    run's elapsed counts from one wall-clock start on both transports
    (`RunFrameScript`, `FrameOpts.sessionStart`); a pre-existing S2 flake
    (`TestNoResetEscapesTheSocketRunsVerdict/after_the_check`) fixed
    test-side: `assertOneOmission` takes its floor from the caller.
18. **Plan 030 X59 (PR 1)** — V2 read 102/103: `sigint-between-turns` differed,
    a pre-existing timing race, not a moved output (the scenario alone ×20
    against `origin/main` `77f1cd3` DIFFs 15/20, against the PR 1 candidate
    13/20), so `craze prompt --json` did not move. SF-87.
19. **Plan 030 X60, X61 (C7r; amends X56)** — a start failure keeps its row
    through a restore (`applyRestore` redraws `startErr`'s row at the tail
    after every restore; C7's `_wait_start_failure` workaround is removed);
    detached test cases seed `host_idle_exit = "30s"`, `marker_pids` matches
    the exact `CRAZE_RUNTIME_DIR` entry, and `host_cleanup` SIGTERMs registered
    and marked hosts and verifies its SIGKILL fallback. Residuals: SF-85.
20. **Plan 030 X62 (C7r2; amends X60)** — a start failure is its incarnation's:
    the model records the incarnation it held when the stream's Ready arrived;
    a restore of another incarnation takes an old start failure away (no
    redrawn row, no stale error at quit), and a failure arriving after such a
    restore is not applied — the held session comes up as a late start does.
    The macOS test-cleanup scan is bounded (`PS_TIMEOUT`) and fails closed.

**PR 2** (C9–C12):

21. **Plan 030 X63, X64, X65 (C9)** — `rowFacts` is a session capability, the
    host's own like `stop` (a hub in S4b will carry rows of hosts of different
    builds); every craze host sets it, the fake host only in its opt-in mode,
    so the 16 earlier fixtures are the older-host direction and fixture 17
    covers it. A row's state has the precedence needs you > failed > working >
    idle (failed also covers an activity error with no `lastTurn` and no
    foreign turn), and `Since` is computed at read time as the latest time
    that could have moved the row into its state, from times the engine and
    its observer record — no poll-dependent memo, no new locks; a settlement
    that starts the next queued turn keeps the first turn's start. `Doing`
    scans back to the current turn's boundary and names a tool by title, else
    name; `LastReply` is the last closed assistant entry (over 64 KiB, its
    tail); `Summary` is a permission's tool title, a question's first
    non-empty question, a plan's name; every string is its first non-blank
    line, tabs expanded, control characters dropped, at most 200 cells.
22. **Plan 030 X66, X67, X68, X69 (C9)** — the roster has its own
    never-reconnecting client (synchronous, deadline-driven; a kept connection
    found closed at an attempt's start is re-dialled once within the budget);
    each host is asked at most once per tick, freed slots going to hosts not
    yet asked; a third host status, `Connecting` (no answer yet, or no
    session published); the list's types live in `internal/roster`, which
    imports neither the TUI nor the CLI, and `sessions.Store` gains `All()`
    for its `Index`.
23. **Plan 030 X70, X71, X72 (C9)** — `tui.Sessions`: `Roster()`, `Open`
    (a running session dialled and attached — a stopping host is `Open`'s
    error, never a failed start; a saved one loaded by a host spawned for it,
    through `openSaved` since C12 and not the launcher's own load, X111),
    `Spawn(SpawnSpec)`, `Stop`. The scale test runs 50 fake hosts in the parent
    and the roster in a child process (CPU from `getrusage`; the 5 % bound not
    asserted under `-race`): 0.8 % of a core, ≤ 11 goroutines above baseline.
    Roster tests are an external package (`fakehost` imports `tui`).
24. **Plan 030 X86, X87, X88, X89, X90, X91 (C9r)** — a host the list's
    `Spawn` started is the launch's until `Open` takes it or the new
    `LeaveRunning` releases it (`finish` stops the rest; a host `Spawn` did
    not start is never stopped); one deadline per roster attempt, fixed as it
    starts (an attempt lasts ≤ 1 s); `Close` joins the attempt goroutines; an
    ask's ending moves `Since` only if the ask was open; the backoff test
    counts attempt starts at the tick barrier and the scale window runs until
    three full rounds with all 50 hosts reachable (0.9–1.7 % CPU, the same at
    a 5 % quota).
25. **Plan 030 X73, X74, X76, X79 (C10)** — rows the plan's table has no line
    for: a host that has not answered yet is drawn with the working rows and
    the spinner (`Starting…`, then `Connecting…`), counted as working,
    `ctrl+x` doing nothing on it; unreachable rows are their own group after
    idle and before saved, counted in `N running` but in none of the four
    header counts, keeping their last answer's title and age. The columns are
    the mockup's shares (mark 2, glyph 2, two gaps of 2, provider 8, age 5;
    of the rest title 36 %, directory 18 %, "what it wants" the remainder);
    an older host's `· craze <ver>` note keeps its cells; the age is one unit
    (s/m/h/d), zero under `--freeze`, blank without a time. Grouped by
    directory, directories are ordered by their most urgent row, `$HOME` is
    `~`, and the grouping is kept for the run.
26. **Plan 030 X75, X77, X78, X80, X81 (C10)** — `ctrl+x` on another session is
    a new `Sessions.Cancel(ref)`: `session.queue.clear` then `session.cancel`
    over a connection of its own that never attaches (`not_accepting` is
    success; bounded at 2 s; no wire change). A replaced incarnation keeps the
    selection (only an armed close drops); a vanished row leaves the
    selection to the line now at its place. `No other sessions.` when nothing
    runs besides yours and nothing is saved. `ctrl+d`, or `ctrl+c` twice within
    1 s, is a view close (the first `ctrl+c` says so). Your session ending
    while the list is up does not quit craze: its row reads `· ended` and
    `esc`/`←`/`enter` on it stay on the list with `that session ended`.
27. **Plan 030 X82, X83, X84, X85 (C10)** — `enter` on another row said
    `opening another session in place is not built yet` until C11/C12;
    `/sessions` runs before your session is up, and `/help` gains one `←` line,
    both only with a session list; below 40×10 the list says `need 40×10`;
    the hint line's words (`enter open`/`back to it`/`resume`, `ctrl+x
    stop`/`close`, `ctrl+s by directory`/`by state`, `← back`, and the notes
    `stopped: …`, `closed: …`, `could not stop|close …: <err>`, an older
    host's `that session runs in an older craze; close it there`); `ctrl+x`
    close is allowed on your own session.
28. **Plan 030 X92, X93 (own commits)** — found by C10 and the gate: a
    pre-existing bubbles v0.21.0 hang (`wordLeft` loops for ever when only
    whitespace precedes the cursor, so `alt+←`, `alt+b` and macOS `ESC b` on
    an empty composer froze craze; `updateComposer` drops the word-backward
    keys in exactly that case); a pre-existing native-harness test race
    (`TestSteerRacesTheTurnEnd`) fixed test-side — a gated batch waits for its
    first accepted steer before releasing.
29. **Plan 030 X94, X95, X96, X97, X98, X99 (C10r; amend X81, X86)** — the ended
    row outlives its host (kept as last listed, or built from the model if it
    was never listed; its age counts from the end); "here" is craze id +
    incarnation, frozen once the session ends, another incarnation being an
    ordinary row; every exit closes the list's roster (a set shared by every
    copy of the model; `finishRun` closes what is left); a recording proxy
    proves `ctrl+x`'s clear precedes its cancel; a spawned host's end is
    decided per host (a failed `Open` stops it only if nothing else holds it);
    `LeaveRunning(ref) error` is decided under `finish`'s lock.
30. **Plan 030 X100, X101, X102 (C11)** — the backend generation `bgen` moves on
    every adopt and is stamped on events, restores, readies, ends and both
    start answers; `gated` drops a stale one before `reading` is touched.
    Stale replies are rejected first: `gated` checks the session generation
    before the gate id, and `release` no longer does (reversing plan 027
    C17's order; its premise is unreachable now). The shared constructor is
    `withSession`; a test classifies every `Model` field. Carried: the
    monotonic counters (`bgen`, `sessGen`, `gateSeq`, and `restores` and
    `turnStarts`, the post-restore read's tag), `expanded`, the held queue
    filtered by `dropStaleHeld`, timers and terminal modes in flight, `exit`,
    `quitting`; a launch spawn in flight at a switch is abandoned.
31. **Plan 030 X103, X104, X105, X106, X107 (C11)** — while `Open` dials, the
    list stays up with `opening <title>…`, the dial stamped per list opening
    (a later enter replaces it; leaving, reopening or quitting abandons it;
    an abandoned answer is closed); a failed dial says `could not open
    <title>: <err>`. Retired backends close off the Update through a shared
    set. Drafts are stashed by craze id through `draftKey` (PR 3's
    temporary-id seam). The band is `─ <title · provider · workspace
    basename> ─── ← sessions ─` (`new session` when untitled), the first
    region, dropped at degradation's last step and first by `fitChrome`. A
    viewed session's end opens the list with `that session ended` unless
    this client's own quit asked for it; the launching TUI whose session an
    attach client's `/exit` stopped now returns to its list (a
    `test_attach.py` expectation updated).
32. **Plan 030 X108, X109, X110 (C11)** — test plumbing: a socket frame run
    whose owner no longer holds the host's session uses the owner's stream
    head as its capture boundary; the gate digest skips `sessions`,
    `sessRosters`, `retired`. A backend `Open` answered tells the launcher
    once when it closes, so a failed later `Open` stops a spawned host only
    when nothing holds it. Residuals: a let-go spawned host whose session
    never came up is stopped only by `finish` or a later failed `Open`;
    roost/herdr shows the previous session's status until the new one is
    ready; quitting while viewing a session whose start failed makes that
    failure craze's exit status.
33. **Plan 030 X111, X112 (C12; amend X70)** — a resume from the list is not
    the launcher's load: `Open` of a saved row goes through `openSaved`, not
    `loadBackend`, whose `--provider` is a filter on a load (it would refuse
    another provider's row) and whose `--model`, `--ask` and `--plan` are the
    launch's own; a resume passes the permission mode, `--plugin-dir`,
    `--no-host-status` and `--agent-bin` (dropped for an in-process
    provider), and a held row attached to prints no ignored-flags note. A
    saved row craze cannot run — an unknown or non-resumable provider, a
    workspace no longer a directory — is refused before any spawn, a
    `*tui.Refusal` in the host's own words, leaving no host and no log.
    Residual: a row whose host started serving it meanwhile is refused too
    if its directory is gone.
34. **Plan 030 X113, X114 (C12; amend X103)** — the hint line says `resuming
    <title>…` and `could not resume <title>: <why>`, and drops the
    launcher's leading `craze: ` from every open failure's note; a switch to
    a saved session is a load: the status row says `restoring…` until the
    replay ends, as the resume picker's does, and a held session already up
    comes up at its first restore.
35. **Plan 030 X115, X116 (C12)** — test support: the fake agent's
    `CRAZE_FAKE_SESSION_ID` gives a run its own session id (every fake
    session was `fake-session-1`, so two sessions in one test HOME were one
    index row). `test_sessions.py` is four cases reading whole bubbletea
    frames replayed into a grid (`Screen`); the wait after `SIGCONT` is
    bounded at the step's bound plus 30 s, the roster's backoff cap; one
    failure at a 5 % CPU quota (a `Connecting…` row satisfied a "working
    group" check) was diagnosed and fixed test-side. Residual (an SF row with
    PR 4's docs): a resume abandoned mid-dial keeps its spawned host until
    craze quits, when `finish` stops it.
36. **Plan 030 X117, X118 (C11r)** — the terminal's own results carry the
    backend generation: a paste, a copy's note and the `!` completion are
    stamped with `bgen` where they are asked for; a stale paste or note is
    dropped by the gate, taken out of the held queue at a switch and dropped
    where it drains; a stale shell completion settles only its own row while
    the pane it was opened in is shown (plan 022's rule) and is never written
    as a new row. The stamp is `bgen`, not `sessGen`: a restore of another
    incarnation moves `sessGen` but keeps the composer and the shell. The
    audit: every message kind that reaches `Update` is stamped and rejected
    after a switch, or harmless (the tick and hidden-retry beats, whose live
    flags carry; `callPanicMsg`; the frame harness's tokens; quits — skills,
    git, dialogs and the title are synchronous). Residual (an SF row): until
    a command a switch killed has died (at most ~7 s), the next session's
    Esc/Ctrl+C go to "kill the command" and `!` is refused. **Amended by X121
    (C11r2, astra r24-fix1112):** the terminal's results are stamped with
    `shownGen` — a counter only a switch moves, not the first adoption — so work
    asked for before the first backend is adopted (a paste while the launch's
    spawn runs) stays with that session and is dropped after any later switch;
    `bgen` stamps only a backend's own messages.
37. **Plan 030 X119, X120 (C11r; amend X106)** — the band's title is the
    row's: one rule, `sessTitle(own, index)` — the session's own title, else
    the index's, else `new session` — for a running row and the band,
    `Model.indexTitle` seeded by the row a switch opens it from, refreshed
    while the list is up and carried by an ended row never listed. Residual:
    as fresh as the list's last listing (fresher would be a wire change).
    The switch tests hold each of A's own messages (11 kinds, the start's
    answers from the real `startCmd` over a held `Start`) through A→B→A and
    deliver each after A2 is adopted, each dropped; a generation reused on
    return fails the new test.

**PR 3** (C13–C16; X142–X144 are C12r2's, PR 2's last commit, recorded
with them):

38. **Plan 030 X122, X123, X124, X125 (C13)** — a completion source answers at
    once or names a keyed load, run as a `tea.Cmd`, its result kept by key
    for the popup's life, at most one running, one the query moved away from
    cancelled; a result is dropped unless it is the load awaited (same key and
    number) — the query is recorded, not compared, so a listing asked for
    `~/p` serves `~/pr`; one process-wide counter numbers openings and loads
    (the popup is a value its owner rebuilds); a load's result is
    shown-stamped (dropped after a switch, X121), and a popup synced with
    another session shown or workspace closes and cancels its load.
39. **Plan 030 X126, X127, X128, X129 (C13)** — the grammar: inside the token
    is start < cursor ≤ end; quoted only when the text has whitespace or
    starts with `"`; a newline ends any token; text with control characters
    or invalid UTF-8 cannot be written, so is never offered. The keys wrap,
    take ↑ ↓ ctrl+p ctrl+n tab esc even with no candidate, and `enter` only
    with one; accepting adds one space, descending none. The count line is
    `↓ N more`, or `↑ N more` once the window reaches the bottom (the mockup's
    `· type to narrow` suffix left out); the drawing measures columns over
    every candidate, marks the selected row with the accent `❯`.
40. **Plan 030 X130, X131, X132, X133 (C14)** — the list's input exists only
    with a `tui.SessionStarter` (PR 2's goldens unmoved); the leading token is
    the `@` token at the input's first non-space rune; the rule shows what
    `enter` would do (a bound token's directory, a finished unbound token
    resolved live, else the row's, else here); a binding is held by the
    token's exact text and dropped for good the first time it differs.
41. **Plan 030 X134–X141 (C14)** — the error words (`no directory named @x`,
    `@x names N directories; pick one with @`, `no directory ~/x`); the `@`
    name mode (here, running directories by recency, recents; basename
    prefix, then path substring; a pick writes the basename, or the `~/` path
    on a collision); browsing (a path token with no `/` names the directory
    itself, ≤ 20,000 entries read 256 at a time, links to directories
    included, 200 shown); `RecentDirs` (every provider's rows, cleaned, stat'ed
    until n); the layout (target rule, input, rule, hint line; the popup above
    the rule, the list keeping ≥ 2 rows); the input's keys (`←`/`→` move the
    cursor when there is text, `tab` alone does nothing, `ctrl+v` pastes
    folded to one line); `setSource` hands the popup the candidates as they
    stand.
42. **Plan 030 X142, X143, X144 (C12r2)** — a lost connection is not the
    session's end: only a clean end (or a re-attach refused `not_accepting`)
    marks the row `· ended`; a transport loss leaves nothing behind the list
    (`lost the connection to that session: <err>`) and the row is the
    roster's, `enter` reopening it. The roster's first tick always publishes
    (an empty registry shows the empty list). A roster test waits for each
    host's first answer after a drop by id, with generous budgets (4/100
    failures under `-race` at 5 %, 100/100 after).
43. **Plan 030 X145, X146, X147 (C15)** — a new session from the list takes
    the launch's `--plugin-dir`, `--no-host-status` and (not for native)
    `--agent-bin`, never its `--ask`, `--plan`, `--model` or `--provider`; its
    model is the session the list came from's, else the launch's `--model`,
    else the provider's default. The dispatch leaves the host running
    **before** it closes its connection (no instant it is neither held nor
    left); an unknown outcome leaves the host running; a refusal or a failed
    start closes, then stops. While it starts the input says `❯ starting…`,
    only the list's own keys pass, and `esc`/`←` leave the list with the
    dispatch carrying on.
44. **Plan 030 X148, X149, X150, X151 (C15)** — the unstarted session is a
    switch to a session with no backend (its band `new session · <provider>
    · ~/projects/lumen`, builtins and `!` inert); its first prompt's host is
    adopted by its own function under X121's first-adoption rule, the draft
    key moving from a temporary id to the craze id, the prompt sent with the
    TUI's own command `"1"` once up; a failed start returns it to unstarted
    with the prompt kept; `←`/`/sessions` discards it. With nothing behind the
    list, the `@` picker has no `here` and the target is the selected row's
    alone (`no directory to start in: pick one with @`).
45. **Plan 030 X152, X153, X154 (C15)** — only exactly `/exit` quits from the
    input (`/exit now` is a prompt); a dismissed popup clears when its token
    disappears; awaited listings are cancelled by every exit; a paste lands
    where it was asked for (the composer's draft, or dropped after the list
    closed). Residual: a dispatch's outcome is invisible once the list has
    been left (its row still appears).
46. **Plan 030 X155 (C16)** — the catalog cache's writer is `craze serve`
    alone: the engine's new `Options.CatalogChanged`, called by the observer
    for a Config section that was not replayed and for a replay's end (a
    load's install is inside its replay), kicks a one-slot worker
    (`catalogRecorder`) that reads the session's snapshot off every lock,
    takes the observation time at that read, and writes
    `agent.CatalogModels` (the models block, else the model option's values)
    when it differs from the last it recorded; a pending kick is recorded as
    the host stops. The opt-out's TUI host, `craze prompt` and the frame
    harness write nothing. No wire change.
47. **Plan 030 X156 (C16)** — the cache file (`internal/modelcache`,
    `rundir.CatalogDir`): `{"version":1,"observedAt","provider","models":[{"id","name"}]}`,
    0600, under `<HOME>/.cache/craze/catalogs` (HOME, never CRAZE_HOME);
    `Write` holds `<provider>.lock` (`atomicfile.LockWithin`, 2 s) across
    reading the file and replacing it, and replaces only a strictly older
    observation; `Read` ignores another version or provider, no time, no
    models, a model with no id, a control character, trailing data, more
    than 256 KiB, anything but a regular file; a catalog `Write` would not
    read back is not written. The older-writer-finishes-last race is forced
    in both orders.
48. **Plan 030 X157 (C16)** — the `/` popup lists `/provider`, `/model` and
    `/exit` (`/exit` is the input's already; without it `/ex` would say
    nothing matches); it opens only while the first word is a prefix of one of
    them, or is `provider`/`model` and a space — any other `/…` line is a
    prompt and opens nothing (the mockup's `unknown command` is not drawn).
    `enter` on `/provider` or `/model` writes it with the space that lists its
    values; a value is used by `tab` and `enter` alike; `/exit` quits on
    `enter` and is only written by `tab`. The placeholder reads `… · /
    provider and model` (the mockup's `/ sets provider and model` is 84
    cells with the prompt).
49. **Plan 030 X158, X159 (C16)** — `/provider` lists the startup picker's
    rows (`Model.providers`), narrowed by id or name prefix, `current`
    marked, with the mockup's one-line descriptions; choosing one resets the
    model — an ACP provider's to its agent's own (no `--model`, drawn
    `default`), native's to `nativeDefaultModel()`, read off the Update, a
    read for a choice since replaced dropped. `/model` lists native's
    `nativeModelChoices()` or an ACP provider's cached catalog
    (`SessionStarter.ModelCatalog`, a new method; one read per popup
    opening), titled `<provider> models for new sessions · last seen <age>
    ago`, `default` (the agent's own) first unless the catalog has that id,
    narrowed by a substring of name or id; with no catalog, or nothing
    matching, `enter` takes the typed id as `--model`. The live catalog of the
    session the list came from is not consulted: its host wrote it to the
    cache.
50. **Plan 030 X160 (C16)** — the pick is `Model.sessPick`, the TUI's (carried
    by `withSession`), lasting across openings and switches until changed; a
    model chosen pins the provider it was listed for, so one provider's model
    id never reaches another's session; the hint line then says `new sessions
    use <provider> · <model>`. Typed `/provider x` (id or name, any case) and
    `/model x` work without the popup; an unknown provider, or either command
    with nothing after it, is the hint line's error.
51. **Plan 030 X161 (C16)** — the seam agreed with Plan 031:
    `nativeModelChoices()` and `nativeDefaultModel()` in
    `internal/tui/sessions_models.go` are the only readers of native's model
    table in the TUI; today's bodies are `modeltable.Load(paths.NativeDir())`
    with the aliases sorted and named as native's own list names them, and
    the table's default unless its key does not resolve, then the first
    sorted alias that does. Plan 031 swaps both bodies.
52. **Plan 030 X162 (C16)** — test support: the fake agent's
    `CRAZE_FAKE_SESSION_ID` expands `{dir}` to its working directory's name,
    since the hosts a terminal's list spawns inherit its environment and
    their sessions otherwise shared its index row; `internal/modelcache`
    joins `make test-race`.
53. **Plan 030 X163, X164 (C15r)** — the dispatch's prompt runs on a
    goroutine and is waited for no longer than the command gate's deadline
    (`awaitPrompt`; an answer there as it passes wins): still out, the
    outcome is unknown — the host left running first, then the connection
    closed at once, which ends a write blocked on a host that has stopped
    reading (X54 reaches no context), the submission and drain joined within
    1 s. The program's exit closes a dispatch's connection at once
    (`dispatchConn`), so it never waits behind that write.
54. **Plan 030 X165, X166 (C15r)** — a list paste carries the list opening
    it was asked in (`pasteMsg.listGen`) and lands only there; the unstarted
    session's shutdown schedules (a quit or SIGTERM while its spawn or Open
    runs) are proven in a pty through the real `finish`.
55. **Plan 030 X167, X168 (C15r)** — the agent's own default (no `--model`)
    is always its own first row; a catalog model whose id is `default` is
    another row (`--model=default`); typed `/model default` stays the
    agent's own. A catalog with any bad model is not recorded (the cache
    keeps what it had; one host-log line per unchanged refused catalog).
56. **Plan 030 X169, X170 (C15r)** — the docs state that a new session
    falls back to the launch's `--model` when the session it came from has
    none (X145); `test_dispatch.py` waits on events, not a fixed sleep.
57. **Plan 030 X171, X172 (C15r2)** — a dispatch's answer is read by its
    error (`promptUnknown`: a context error or `ErrOutcomeUnknown` is
    unknown), never by the clock — a refusal taken as the deadline passes
    stops its host; a host's own bound on the prompt now reads unknown. The
    program's exit cancels a dispatch's view close already detaching
    (`dispatchConn` holds the context `CloseWithin` waits on), so `finishRun`
    no longer waits the detach's 3 s bound inside `remote.Session`'s shared
    close.
58. **Plan 030 X173, X174 (C15r2)** — tests: the proof that an unstarted
    session spawns nothing is the Go test, which runs every command the
    opening returned (`test_dispatch.py` corroborates); the unknown-outcome
    test holds its deadline instead of racing a real 50 ms one (a starved
    run started the call after it — diagnosed, not retried).
59. **Plan 030 X175, X176 (C15r3)** — tests: the refusal-at-the-deadline test
    holds the dispatch before it waits (two hook steps, `dispatchAwaiting`
    and `dispatchSubmitted`) until the refusal is sent and the deadline
    passed, so it fails on the parent's classification; the exit's cut of a
    dispatch's detach is proven with a fake whose close waits only for its
    context (no clock); the real-socket test stays as the integration check.
60. **Plan 030 X177 (C15r4)** — an unstarted session's first prompt whose
    session ends while the list is open over it is dropped (the list-open
    `endMsg` arm clears it, as the `errMsg` arm does): a `startedMsg`
    delivered after that end no longer submits it to the ended session.

**PR 4** (C17–C18r, C17r, C17r2):

61. **Plan 030 X178 (C17; one deviation)** — the search: `rg` if
    `exec.LookPath` finds it, else `git ls-files -co --exclude-standard -z` in
    a git work tree, else the walk. `rg` runs as `rg --no-config --files
    --hidden -g '!.git' -0`: `--no-config` is the deviation (as
    `internal/harness`'s grep and glob), so a user's `RIPGREP_CONFIG_PATH`
    cannot narrow the list. One 3 s budget covers the tool and any fallback
    walk; the tool runs with no shell, stdin `/dev/null`, SIGKILL on cancel,
    stderr kept to 4 KiB; the cap reads the 50,001st record to know it was
    cut, then kills the tool. git failing with nothing listed gives way to the
    walk; `rg` failing with nothing is the answer (exit 1 with nothing is "no
    files"); a failure after paths leaves them standing. The walk:
    breadth-first, ≤ 20,000 entries read 256 at a time, regular files only
    (symbolic links neither followed nor listed), never `.git` or
    `node_modules`, unreadable subdirectories skipped.
62. **Plan 030 X179, X180 (C17)** — what a search offers: what was read, even
    cut short, titled `only the first 50,000 files`, `only the first 20,000
    entries` or `listing stopped after 3s`; the notes `listing files took over
    3s`, `could not list files: <why>`, `no files here`, `no workspace to
    search`. Paths: NUL-separated, a leading `./` removed, control characters,
    invalid UTF-8 and anything not a clean relative path refused (`atTextOK`,
    X126's rule factored out), records over 64 KiB dropped; folders derived
    from the files (empty ones never appear), git's conflict-stage duplicates
    merged. Residuals: SF-95.
63. **Plan 030 X181, X182, X183 (C17)** — the match: a case-insensitive
    subsequence over the relative path with a simplified fzf score (segment
    starts, case changes and adjacent runs rewarded, gaps penalised, +1000
    when the last segment starts with the query, the best of three
    alignments), ties to the shorter name, then bytes; a `/` in the query makes
    what precedes its last `/` a folder prefix, and `/…`, `~/`, `../` offer
    nothing. Candidates are `Name = Value = Insert` = the relative path,
    folders ending in `/` and openable; at most 100 are ranked and handed over,
    `completeAnswer.More` (new) counting the rest into the count line — with it
    zero every existing golden is byte-identical. Speed: a per-path byte mask
    and a 100-slot heap (proven equal to a full sort); 2.75 ms per keystroke on
    a repository-shaped 50,000-file tree, 3.84 ms on a pathological one; the
    index built off the Update in ~20–27 ms. Residual: SF-96.
64. **Plan 030 X184 (C17)** — tests: the fake `rg` and `git` written once in
    `TestMain` (a script run right after it is written can fail with
    `ETXTBSY`, go.dev/issue/22315); the 3 s timeout forced through a held
    clock; reaping checked by signal 0; 190/190 at a 5 % quota with and
    without `-race`.
65. **Plan 030 X185 (C18)** — the audit plan §3.16 asked for: no existing
    frame script (every `internal/tui` test feeding keys or text,
    `internal/cli/session_dispatch_test.go`, `tests/cli`) types an `@` at a
    word start into the session composer; every `@` found is the list's
    input, a component test, an expected string or a digest marker. None
    did, so no owner decision was needed and the popup shipped.
66. **Plan 030 X186, X187 (C18; X187 amends X127 for the composer)** — the
    popup is the TUI's (`Model.composerAt`, carried by `withSession`), synced
    with the draft before the layout; `switchBackend` and `openUnstarted`
    close it and cancel its search, the first adoption keeps it (X121). It
    opens only while the composer has the keyboard — no list, no card,
    dialog or sub-agent view over it, no confirm line, no queue or sub-agent
    focus, not shell mode, a workspace — and its keys apply only while the
    layout gives the band a row. It works in the unstarted session and in
    `craze attach` (the search runs on this machine, in the session's
    workspace); a restored draft ending inside an `@` token reopens it. Keys:
    `esc` at the slash menu's rung (after a running `!`'s kill, shell mode's
    clear and a queue edit's cancel; the first `esc` hides, the second
    cancels a running turn); `↑`/`↓`/`ctrl+p`/`ctrl+n`/`tab` ahead of the
    composer's; `enter` only with a candidate; `PgUp`/`PgDn`, `ctrl+l`,
    `alt+enter`, `shift+tab` stay the composer's; no mouse.
67. **Plan 030 X188, X189 (C18)** — drawn in `regionOverlay` through
    `layout.go`'s `overlayBandRows`/`overlayBandView`, where the slash menu
    draws (`slash.go` untouched), outranking it for a quoted `@"a /b"`, at
    most 10 rows; titled `files in <workspace name>` on every answer (the
    height never jumps while searching), a partial list's reason after ` · `;
    `searching…` and `nothing matches`. No help-dialog line.
68. **Plan 030 X190, X191 (C18)** — four in-process goldens over a fixed
    listing (`composer-at-files`, `composer-at-dir`, 100×30 and 80×24;
    manifest 115 + 36); the exact inserted text (plain, `tab`, spaces,
    Unicode, a folder, descend then accept, a token mid-draft, only the token
    under the cursor); `test_tui_composer_at_mentions_a_file` in both modes.
    A test trap found: copies of a bubbles `textarea.Model` share line
    storage, so goldens build a fresh model per frame. The docs are
    `docs/reference/tui.md`'s `## File mentions`, what each provider does
    filled in from V4/V5 by this commit.
69. **Plan 030 X193 (C18r, sol r37-c18)** — the slash menu stays out of an `@`
    token: a quoted `@"a /` has a `/` after a space that `slash.go`'s
    tokenizer reads as a slash token, so the composer refuses the menu while
    it completes `@` and the cursor is inside an `@` token — whether the popup
    is up, hidden by `esc`, or closed because the keyboard is elsewhere (a
    token's kind is the draft's, not the focus's). While it holds, `PgUp`/
    `PgDn` and the wheel scroll the transcript, `enter` with no file
    candidate sends the draft as typed, the second `esc` cancels a running
    turn. `slash.go` untouched (§3.15). Residuals: SF-98.
70. **Plan 030 X194, X195, X196, X197 (C17r, sol r36-c17)** — what a listing
    holds is capped: candidates (files and the folders met) at 100,000 and
    their bytes at 8 MiB, a path that would pass either stopping the listing
    like the path cap (a real 49,980-file tree is 59,142 candidates, 3.7
    MiB). The whole listing — `rg`, git and its checks, or the walk — runs on
    its own goroutine (a deviation: the review asked for the walk only),
    handing over batches of ≤ 1,024 candidates; the search checks its
    deadline and the popup's cancel between batches and answers at either
    without waiting for the listing. git's paths are kept only if `Lstat`
    says a regular file, each new folder stat'ed once top down, so tracked
    links, files under a folder since made a link, deleted files and
    submodules are not offered — as `rg` and the walk offer none (settles
    X180's residual; ~61 ms at 50,000 paths). Tests
    `TestAtFilesCapsWhatTheListingHolds`, `TestAtFilesGitOffersOnlyWhatIsThere`,
    `TestAtFilesAnswersWhileTheWalkIsStuck`; 210/210 at 5 % with and without
    `-race`. Residual: SF-97.
71. **Plan 030 X198 (C17r2, sol r38-c17r-c18r)** — the tool is reaped on a
    goroutine of its own, started right after `Start`, so a tool killed at the
    deadline or the popup's cancel is reaped at once whatever the listing
    goroutine is inside (git's `Lstat` on a stalled mount held a killed tool
    as a zombie until the stat returned). Stdout is craze's own `os.Pipe`,
    which `Wait` never closes; the normal, cap and full paths still reap
    before the answer. `TestAtFilesReapsTheToolWhileAGitCheckIsStuck`
    (reverted, the tool sits `Z`); 220/220 at 5 % with and without `-race`.
    Residuals: SF-97.
72. **Plan 030 X192 (found by PR 3's V1; pre-existing)** — V1 at `885b43c`
    failed `TestFrameGoldenLoadLongReplayDoesNotDeadlock` once in 20: the
    asynchronous run's frame script timed out at 20 s at line 570 of 600 on a
    heavily loaded machine. Not a regression — at a 25 % quota with `-race` it
    fails 7/7 on `main` `b8aa5bb` (no PR 3 code) and 7/7 on PR 3's tip, with
    the same durations; PR 3 merged on that diagnosis. The bound is fixed in
    PR 4 (`202928d`, X199).
73. **Plan 030 X199 (`202928d`)** — the long-replay golden's one wait is the
    whole 600-event replay (0.5 s unloaded, 7 s under `-race`, 37–58 s under
    `-race` at a 25 % quota, 275–320 s at 5 %) against a fixed 20 s; the two
    gate modes fold the same messages. `FrameOpts` gains an opt-in stall: a
    wait fails once no frame has folded a new event for it (progress is the
    fold's seq, not the frame text), the timeout only the overall cap. This
    test sets a 30 s stall (the longest gap measured is 2.2 s, at 5 % under
    `-race`) and a 10-minute cap, raises acp's session/load deadline to the
    cap (in process the load answers only after the fold), and pins the
    session start ahead so the golden's `0m` holds. Passed ×10 at 5 % under
    `-race` on both transports; a reader armed too late and a load that never
    answers each fail within the stall. `TestFramePluginBlockReachesTheAgent`'s
    wait needle spanned a space the echo's wrap could fall on (the checkout
    path's length moves the wrap); it now waits on fragments no wrap can
    split. Found, not fixed: SF-102.
74. **Plan 030 X200 (C18r2, CodeRabbit on PR 4)** — a composer `@` row's
    name is the path's display form (`atFileName`): `sanitizeLine`, then every
    Unicode format character (`Cf`: bidi overrides and isolates, zero-width
    characters, tags) written as `<U+XXXX>`, so a row cannot read as a
    different path than the one a pick writes; `Value` and `Insert` stay the
    exact path, and matching stays on it. `sanitizeLine` itself is unchanged
    (an emoji's U+200D is `Cf`). Residuals: the display is not reversible;
    an emoji's joiner shows escaped in the popup; runs of spaces draw as one;
    the draft and the transcript draw a picked path's `Cf` runes raw (SF-103).
75. **Plan 030 X201 (C18r3, sol on C18r2)** — the row escapes Unicode's
    Default_Ignorable_Code_Point set with every `Cf` rune (Go's `Cf`,
    `Variation_Selector` and `Other_Default_Ignorable_Code_Point` tables):
    the combining grapheme joiner, variation selectors and Hangul fillers,
    which drew as nothing, now show as `<U+XXXX>`; visible combining marks
    are kept. UAX #44's subtractions from the set (interlinear annotation,
    Egyptian format controls, prepended concatenation marks) are `Cf` and
    stay escaped. A test sweeps every code point against UAX #44's
    derivation. Residual: a non-ASCII space (U+00A0, U+3000) draws as an
    ASCII space in the row (`sanitizeLine`) while the pick writes it.

### Live smoke

Two runs on 2026-09-30 against one build (`28dfc7d`, C17r, with C15r4's
`app.go`; `--version` 0.0.1), driven in tmux with bracketed paste; notes and
captures in the plan folder's `v4/NOTES.md` and `v5/NOTES.md`. **V4, Linux**:
COSMIC on Wayland, tmux 3.4 and cosmic-term 1.8.0; cursor-agent 2026.09.28,
grok 1.0.44, native on `fireworks/deepseek-v4p1-flash`. **V5, the mac-mini**
over ssh: macOS 26.5.1 arm64; grok 1.0.30, native on
`muse-spark-1.3-contributor` (DeepSeek V4.1 Flash in legs 7–8); cursor
skipped, the login keychain over ssh, as every earlier phase found.

| # | leg | V4 Linux (cursor, grok, native) | V5 mac-mini (grok, native) |
|---|---|---|---|
| 1 | launch each provider, one prompt | PASS — one detached `craze serve` per launch, its own session leader with the agent in a group of its own; registry entry `ready` | PASS |
| 2 | close the tab; reopen; `←`; `enter` | PASS — three `tmux kill-session`s; every host alive and answering `hello`; the list showed them idle, each opened in place with its band and transcript | PASS — the hosts reparented to pid 1 |
| 3 | quit the terminal application | PASS — cosmic-term (SIGTERM) gone with its TUI; the host lived on in the application's scope, which stayed active until the host's never-prompted idle exit (5 m 0.8 s), then was collected | not run (no GUI over ssh) |
| 4 | a session blocked on a question, answered from the list | PASS, with a caveat — `question: Which colour do you prefer?` under "needs you"; opened; `2` → Green, shown in a second client attached to it too; the card blocked `←` and `/sessions`, so the list came from a second terminal | PASS — `←` pressed before the question arrived; `2` → Blue |
| 5 | `/exit` one | PASS — client, host and registry entry gone at the first 0.1 s poll; the row under saved; `enter` resumed it (`--load`) with `restored` | PASS — host and entry within 0.01 s, the client at 0.15 s |
| 6 | dispatch into another directory with `@`; `@dir` alone | PASS — `started in /tmp/craze030-v4/proj-b` after 3.1 s (cursor); the unstarted session spawned nothing until its first prompt | PASS — through a browsed `@/tmp/craze030-v5/`; `started in …` within 0.3 s |
| 7 | `/provider`, `/model` | PASS — `grok models for new sessions · last seen 7m ago`; Grok 4.7 Fast → the host's `--provider=grok --model=grok-4.7-build-fast`; `/provider native` reset the model to the table's default | PASS — the same, `last seen 4m ago` |
| 8 | composer `@` a file | PASS — every provider received `What is the first line of @README.md` verbatim (the journal's `prompt` record); grok attached the file itself, no tool row; cursor and native read it with their read tool | PASS — grok's own history holds the message plus an attached-files block with the file; native read it (DeepSeek searched for it first) |
| 9 | idle exit at 2 s | PASS — gone 2.74 s after the detach; `craze -c` resumed it | PASS — 2.76 s |
| 10 | ssh logout and login | not run (no sshd on the box) | PASS — `login`, the shell and the TUI gone; the host and its agent survived; the next login's `craze -c` attached to that host |

Both runs ended with every host they started stopped, and no `craze serve`,
agent process or `.pgids` record left.

- **Cgroups and scopes** (plan §9, SF-79): `setsid` moves a host's session,
  not its cgroup. A host spawned in a tmux pane's first instant stayed in the
  tmux server's own scope, later ones in the pane's `tmux-spawn-<uuid>.scope`
  (probably tmux's asynchronous move racing the spawn; not verified); neither
  is stopped by `kill-session`. cosmic-term's
  `app-…-cosmic-term-<pid>.scope` outlived its main process for as long as
  the host ran. The systemd risk did not occur on either terminal; COSMIC's
  own launcher scope and a full logout were not tried. macOS has no logind.
- **Timings:** `/exit` ≤ 0.11 s on Linux, ≤ 0.15 s on the mac, for the client,
  the host and its entry together. Dispatch to `started in …`: cursor 3.1 s,
  grok ≤ 0.26 s, native ≤ 0.21 s on Linux, < 0.3 s on the mac; opening a row
  about 1 s or less. Never-prompted hosts exited 5 m 0.03–0.8 s after their
  last detach under the default 1 h; `"2s"` hosts 2.74–2.94 s.
- **What each provider does with `@path`:** the text arrives exactly as
  inserted (the pick's trailing space trimmed on send). grok attaches the
  whole file itself (its own `chat_history.jsonl` holds the message, then an
  attached-files block with the file's contents); cursor
  (`Read README.md (1 - 5)`) and native (`read`, sometimes after a `glob`)
  read it with their tools. This commit puts it in `docs/reference/tui.md`.
- **The ssh leg** (V5 leg 10): killing the local `ssh -tt` ended `login`, the
  shell and the TUI; the host (parent now pid 1) and its grok agent kept
  running, the host log showing no stop; a new login's `craze -c` attached to
  that same host (no `craze serve` spawned, no `--load`).
- cursor's catalog has a model whose id is `default` (`Auto`), so X167's
  case is real. A bare `craze` with a provider in `config.toml` shows the
  provider dialog with it preselected (V5) — the documented picker, older
  than Plan 030.

Surprises that became rows: a card blocking the way to the list (SF-99); the
list's last-reply column showing raw Markdown (SF-100); a list dispatch saving
its provider as the last one started (SF-101 — kept, and documented in
`docs/reference/tui.md` by this commit); a resumed native session's replay
leaving out the question's answer row (V5, added to SF-61).

**The V5 incident (the environment, not craze).** The run's first `tmux
new-session` went to the owner's long-running default tmux server on the mac
(up 14 days, its working directory on the external volume
`/Volumes/miniext`), which hung in the kernel opening its own working
directory to spawn the pane — a macOS privacy check
(`kTCCServiceSystemPolicyAllFiles`, the responsible app Ghostty) that never
completes over ssh — and every later command against that server hung too.
Nothing of craze ran under it. The run left that server untouched and used a
private server (`tmux -L v5`) for every leg; the owner's server needs a
restart from a directory on the internal disk, and later mac smokes use a
private tmux server.

### Decisions and questions touched

**SD-34** (the order: S4a + S5, then S4b, then S3, then S6/S7; S4 split in
two) and **SD-35** (the lifetime rules for detached hosts), both written in
PR 0 from the owner's decisions; SQ12's default is refined by SD-35 (`10`,
PR 0). No other `SD-nn` added or superseded; no `SQ` resolved.

Decided during execution without the owner (decide, do not ask), each an
amendment above; not every amendment is here.

Architecture:

- **Wire additions never move a fixture** (X1, X63): the new info-document
  fields and the row facts are omitted when unset, and the fake host sets
  them only in an opt-in mode, so the existing wire fixtures stand for the
  older host; `rowFacts` is a session capability, the host's own, since S4b's
  hub will carry rows of hosts of different builds.
- **An agent group is identified by its leader's start time** (X22): a
  recorded group is signalled only while a process with that pid and start
  time exists (`rundir.ProcessIdentity`), and a record that cannot be made
  fails the start.
- **Attachments are counted until EOF** (X29): a read EOF takes a connection
  out of the idle count, so a half-closed bridge cannot pin a host; the close
  fence refuses admissions while it is up and is reversible (X30).
- **The quit's deadline closes a transport whose write is blocked** (X54):
  `remote.Client.Command`'s write does not honour its context, so the quit
  closes under it instead.
- **The switch's stamps** (X100, X117, X121): `bgen` on every message of a
  backend, `shownGen` on the terminal's own results (a paste, a copy's note,
  a `!` completion), each checked before the gate's bookkeeping; stale
  replies rejected first.
- **The list's input needs a `SessionStarter`** (X130), so PR 2's list
  goldens stayed byte-identical.
- **A lost connection is not the session's end** (X142): only a clean end
  marks a row `· ended`.
- **The dispatch's order** (X146): Spawn, its own connection, Start, the
  prompt as that connection's command `1`, then `LeaveRunning` **before** the
  connection closes; an unknown outcome keeps the host, a refusal or a failed
  start stops it. Its answer is read by its error, never the clock (X171).
- **Only `craze serve` writes the catalog cache** (X155), off every lock,
  replacing only a strictly older observation under a per-provider lock
  (X156).
- **`rg --no-config`** for composer `@` (X178), so a user's ripgrep config
  cannot narrow the list; the whole listing on its own goroutine and its tool
  reaped by another (X195, X198).

UX:

- **The list's groups, glyphs and words** (X73–X85): `Starting…` and
  `Connecting…` rows drawn with the working ones; an unreachable group after
  idle, counted only in `N running`; the columns and one-unit ages; `ctrl+x`
  clearing a session's queue before it cancels its turn, twice to close;
  `ctrl+d` (or `ctrl+c` twice) from the list a view close; an ended
  session's row kept; the hint line's words.
- **Opening in place** (X103, X106, X107): the list stays up with `opening
  <title>…` while the dial runs; the band `─ <title · provider · dir> ─── ←
  sessions ─`; a viewed session's end returns to the list with `that session
  ended`. Resuming a saved one says `resuming <title>…` and `restoring…`
  (X113, X114).
- **New sessions' pending and unstarted states** (X147–X152): `❯ starting…`
  with only the list's keys passing; the unstarted session's band `new
  session · <provider> · <dir>`, its builtins and `!` inert, `←` discarding
  it; with nothing behind the list, `no directory to start in: pick one with
  @`; only exactly `/exit` quits from the input.
- **`/provider` and `/model`** (X157–X160): the `/` popup lists `/provider`,
  `/model` and `/exit` and opens only for a prefix of them (any other `/…`
  line is a prompt); a provider resets the model to its default; a model
  pins the provider it was listed for; the choice lasts across openings and
  switches until craze quits. **The agent's own default is always its own
  first row** (X167); a catalog model whose id is `default` is another row.
- **Composer `@`** (X185–X189): after the audit found no frame script at
  risk, the popup ships in every mode, the opt-out and `craze attach`
  included; `esc` at the slash menu's rung (the first hides, the second
  cancels a turn); `enter` only with a candidate; no mouse and no help-dialog
  line; titled `files in <workspace>`. **The slash menu stays out of an `@`
  token** (X193).
- **A list dispatch saves its provider as the last started** (found by V4):
  kept as the launch's ordinary rule and documented in
  `docs/reference/tui.md`; SF-101 asks the owner whether list sessions should
  not persist.

### Handoff

**S4b (the hub) is next** (SD-34), then S3. What it inherits:

- **The roster and its one client.** `sessions.list` rows carry the row facts
  behind `rowFacts` (`05`, "As shipped (S5)"; fixture 17). `internal/roster`
  polls every registry entry — one kept, never-reconnecting connection per
  host, one deadline per attempt covering dial, hello and list, at most 8 in
  flight, a backoff, unreachable never shown as saved — and the index for the
  saved rows. The TUI reads the list only through `tui.Sessions` (`Roster`,
  `Open`, `Spawn`, `LeaveRunning`, `Stop`, `Cancel`) and `SessionStarter`
  (`RecentDirs`, `ModelCatalog`), `internal/tui/sessions_config.go` — the
  seam at which the hub's `sessions.subscribe` would replace the per-host
  polling (SF-76).
- **Spawning and lifetime.** `spawnHost` (`setsid`, the ready line on fd 3,
  the recorded agent groups, the held rendezvous), `craze serve --host-id`
  and `LeaveRunning` are what `session.create` will spawn through;
  `session.stop` behind `stop`, the close fence and `host_idle_exit` are
  what a hub's stop and roster sit on. A host's registry entry, lock, log and
  `.pgids` record live under `~/.cache/craze/` (`rundir`).
- **The model catalog cache** (`internal/modelcache`, `rundir.CatalogDir`),
  written by every `craze serve`.
- **The rows that matter most** (`13`): SF-76 (the hub), SF-70 (notices of
  other sessions, which a subscription makes cheap), SF-99 (a card blocks the
  way to the list), SF-78 (an open ask pins a host), SF-79 (a terminal that
  stops its scope, not seen yet), SF-61 (ask-outcome rows on a snapshot
  attach or a resume), and the owner's SF-77 (the opt-out), SF-86 (the help
  dialog's quit wording) and SF-101 (list sessions saving their provider).
- **The Plan 031 seam is on `main`** (#71): `nativeModelChoices()` and
  `nativeDefaultModel()` in `internal/tui/sessions_models.go` are the TUI's
  only readers of native's model table (X161); Plan 031 swaps their bodies.
