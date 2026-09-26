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
| Status | planned (Plan 027, FINAL after panel review 2026-09-24); PR 1 merged; PR 2 executing |
| Plan | `027-session-control-s2-socket` (outside the repo, `~/.claude/plans/craze/`, raw panel reviews in its `panel/` folder) |
| Baseline | `origin/main` `5901e4a`: H6 PR 2 (#53, sub-agent stop), merged on top of S1c PR 2 `79eb082` (#52, which completes S1) |
| Branch / PRs | four sequential PRs, each branched from a freshly fetched `origin/main` after the previous one merges: `feature/plan-027-s2-wire`, `feature/plan-027-s2-host`, `feature/plan-027-s2-tui-async`, `feature/plan-027-s2-attach` |
| Merged | PR 1 — #55 `318fc76` (2026-09-25); PR 2 — — (filled in as it lands) |

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
   the queue lands on the registry's one writer goroutine, in order, so a
   failed rewrite is made good only once a next one for that engine
   succeeds — until then, or before the first engine is ever attached, the
   entry can still describe the previous engine, or the empty bind-time one.
   SQ16 itself is `atomicfile.LockWithin` (a
   bounded, polled `LOCK_NB`) plus `sessions.Store.EnsureCrazeID` (mints a
   legacy row's id once, under the index's own lock) — three refusals: a
   **held** claim (`craze: that session is open in another craze (pid N)`), a
   **busy index** (`the session index is busy — try again`), and an index
   **that changed** under the load (`the session index changed — try again`,
   astra r30: otherwise two loaders could mint two ids for one provider
   session). The initial lookup (`--continue`'s `Latest`, `--resume`'s scan)
   failing still exits 1, same as no session found — there is no row yet to
   warn about; once a row is in hand, only a failure while giving a **legacy**
   row (no `crazeId`) its id is a warning, and the load proceeds
   unclaimed — the lock must not lock the user out of their own session over
   a filesystem fault. A row that already has an id is claimed normally
   regardless of that failure. The resume picker's error row
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
   exec "$p" bridge …`, tried in order — `$HOME/.local/bin/craze`,
   `command -v craze` (accepted only absolute), `/opt/homebrew/bin/craze`,
   `/usr/local/bin/craze`, `/home/linuxbrew/.linuxbrew/bin/craze`,
   `/usr/bin/craze`, `$HOME/.nix-profile/bin/craze`,
   `/etc/profiles/per-user/$USER/bin/craze`, `/run/current-system/sw/bin/craze`,
   else `craze: command not found` at exit 127 — following roost's ladder and
   craze's own Homebrew tap; every interpolated value, the session id
   included, is shell-quoted going in. Verified by running it
   (`bin/ladder-check.sh`). Craze ships no remote-exec code of its own; the
   ladder is published for shed (or anything else driving this over SSH) to
   copy verbatim.
