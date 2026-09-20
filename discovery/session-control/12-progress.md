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
| Status | planned |
| Plan | `021-session-control-s1b-engine` (outside the repo, `~/.claude/plans/craze/`) |
| Baseline | `6581e0a` |
| Branch / PRs | three sequential PRs, each from fresh `origin/main`: `feature/plan-021-s1b-driver`, `feature/plan-021-s1b-asks`, `feature/plan-021-s1b-state` |
| Merged | — (date and squash commits go here when all three PRs merge) |

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

To be filled at merge.

### Deviations from the plan

To be filled at merge.

### Live smoke

To be filled at merge.

### Decisions and questions touched

SD-33 recorded in `08`, ahead of execution (SQ12 resolved in `10`). Further
rows to be filled at merge.

### Handoff

To be filled at merge.
