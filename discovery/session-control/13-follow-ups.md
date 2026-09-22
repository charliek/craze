# 13 — Follow-ups

One backlog for everything a phase found and did not do, so it can be planned
rather than rediscovered. `12` is the record of what happened and why; this
file is the list of what is still owed. A plan that takes a row says so in its
work breakdown, and the PR that closes it deletes the row (git history keeps
it). A phase's close-out adds its own rows here before it is called complete.

Ids are `SF-nn` and are never reused. **Phase** is the earliest phase that
should take the row, not a promise; `owner` means it needs a decision before it
can be planned; `any` means it is independent of the session-control phases.
Size: S is under a day, M a few days. Plan amendment numbers (`X47`) refer to
the phase's plan, which lives outside the repo; the pointer into `12` is the
durable one.

## S1c — the transcript model

| id | what | why it is open | where | size |
|---|---|---|---|---|
| SF-01 | **Native's first-prompt title is not in the event stream.** A folding client learns every other title from a `Title` delta; native's self-assigned one is only in `Snapshot()`. | Publishing it moves `native-echo-80x24` (the separator reads the title instead of `craze`), because the TUI re-reads the snapshot on every `EventMeta`. S1b ships no feature, so it stayed silent. S1c decides whether that golden may move, or folds the title another way. | `12` S1b "Handoff"; `internal/agent/native.go` (`prompt`, the comment at the title) | S |
| SF-02 | **The TUI still mirrors settings with `refreshSnap()` on every `EventMeta`**, and reads `State()` rather than the delta's payload. | Changing the mirror's source is what moved a golden once (`12` S1b deviations, the C4 note on `native-echo-80x24`). S1c's fold is the natural replacement; until then the per-section revision check (`modeRev` / `modelRev` / `configRev`) stands on `Seq` alone. | `internal/tui/app.go` (`applyStateDelta`, `refreshSnap`, `mayApply`) | M, inside S1c |
| SF-03 | **`m.sess` is still a field on the TUI model**, write-only in production; 71 test sites reach for it. | Mechanical rename plus one assertion in `provider_picker_test.go`; left out of S1b as churn. S1c moves the model's state anyway. | `internal/tui/app.go` (`setSession`, the field's doc) | S |
| SF-04 | **The ask registry's id-adoption subsystem (~265 lines) exists only for `tui.Stub`**, which adopts test-chosen ids at ~40 sites. With it goes the recorded ABA on a re-used ADOPTED id under the cancel mask, which production ids (minted, never re-used) cannot reach. | Worth removing once the Stub has a reason to mint its ids — S1c's snapshot tests are that reason. | `internal/agent/asks.go` (adoption); `12` S1b deviations (PR 2) | M |
| SF-05 | Snapshot and history byte bounds (SQ9) and tool-event conflation (SQ15). | Already open questions with defaults; S1c is where the bound is chosen. Listed so the plan does not miss them. | `10` SQ9, SQ15; S1a's measurements in `12` | in S1c's plan |

## S2 — the control socket

| id | what | why it is open | where | size |
|---|---|---|---|---|
| SF-10 | **A connection-local barrier, so a reply is ordered after the events its command caused.** `Control.Sync(ctx) error` commits what was enqueued and offers it to subscriptions, but returns no sequence number, and a single serialized writer does not help while the forwarding goroutine is unscheduled. | S2 adds either a `Sync` that returns the sequence it committed through, or a barrier acknowledged by the forwarding goroutine. `05` promises the ordering "at the wire"; nothing delivers it yet. | `05` "Responses and events"; `12` S1b "What S2 must do" | M |
| SF-11 | **Mint a client id per connection, bind it, and release it on disconnect.** No client is ever retired from the receipts table today: right for the TUI's one process-lifetime client, wrong for a server that mints per connection for ever. A server-minted id must be bound to its connection, never trusted from the wire (`c-N` is predictable). | Retirement was tried in S1b and revoked a LIVE client; it needs an explicit release. | `internal/engine/receipts.go` (package doc); `12` S1b deviations | S |
| SF-12 | **Wire validation of command ids and the retry policy by code.** Canonical positive decimal ids, numbered per client from 1; `unavailable` / `not_accepting` / `in_progress` mean "did not run, resend the same id", everything else is a stored answer. In process only so far. | The schema and fixtures must carry it. | `05` "Errors"; `internal/engine/control.go` (`Command`, `classify`) | S, in the schema commit |
| SF-13 | **The gate table's `failed` / `cancelling` / `closing` rows**, and the `session.set` column beside them. | "To be specified in S2", derived from the code's actual states. | `05` "The verb × activity gate" | S |
| SF-14 | **`Subscribe` blocks inside the log's publishing boundary**, so the primary's own reader may not call it with the primary full. | The server calls it off the reader's goroutine. | `internal/engine/control.go` (contract comment) | S, a rule not a change |
| SF-15 | **`Close` does not wait for a LONE inline first-prompt seed still writing on a client's goroutine** (it does when a retry is retained behind it). Unreachable today — the TUI's `Update` is inside that very `Submit`, and `craze prompt` has no index — reachable from a server's handler goroutines. | Either count an active seed as owed in the index worker's exit (inside its one 500 ms bound), or keep index writes off connection goroutines. | `12` S1b "What S2 must do"; `internal/engine/index.go` (`finish`, `owed`) | S |
| SF-16 | An end-of-turn touch taken while that seed is INSIDE `Upsert` finds no row and is dropped, so the row keeps the seed's timestamp, a few milliseconds before the turn's end. | Cosmetic for `--resume` ordering; fix with SF-15 (retain a touch that meets an active seed). | same | S |
| SF-17 | **The TUI still calls the engine synchronously from `Update`**, and `Interject` blocks there. SD-33 moves every call site onto a `tea.Cmd` when the TUI becomes a socket client. | Unchanged on purpose in S1b. | `internal/tui/queue.go` (`Interject`), `cards.go` (`answerCard`), `slash.go` (`/rename`) | M, the bulk of S2's TUI work |
| SF-18 | **How a remote client words another client's settings change.** S1b's TUI draws no transcript row from a settings delta; its own optimistic notes stay where they were. A second TUI on the same session sees the chip move and no note. | A product decision, then a small renderer. | `12` S1b deviations (the settings deltas); `08` SD-30 | owner, then S |
| SF-19 | `--continue` twice on one session (SQ16); one attached session per connection (SQ14); `craze attach` as the full TUI (SQ7). | Already open questions with defaults. | `10` | in S2's plan |

## Plan 025 — effort and speed per provider

Not a session-control phase's own row set — plan 025 (`feature/plan-025-effort-speed`)
landed on S1b's settings worker (`03-engine-core.md`, `05-protocol.md`) to fix
cursor's stale-catalog defect, and left these for the owner.

| id | what | why it is open | where | size |
|---|---|---|---|---|
| SF-27 | **Grok has no speed knob over ACP.** Its catalog advertises `model` and an effort option (`reasoning_effort`); no `fast`-shaped select, on any model probed. | Nothing for craze to build until grok's CLI advertises one; recorded as "waiting on grok". | plan 025 design 7; `13`'s SF-30/31 (grok's other live-smoke gaps) | — |
| SF-28 | **Cursor's per-model catalog defect is fixed.** A model switch used to leave the previous model's effort/fast control on screen (`set_model` answers `{}` and pushes nothing); a settings reply's catalog is now installed by the read loop in wire order, and a model change is one `set_config_option(model, X)` call whose reply carries the destination's whole catalog. | Not open — recorded so the defect this backlog would otherwise still be tracking is not rediscovered. | plan 025 designs 1–2, its PR (`feature/plan-025-effort-speed`); `internal/acp/client.go`, `internal/agent/live.go` | done |
| SF-29 | **A model-moving push racing the start-up `--model` install can still show the push's model in `Config` and `--model`'s value as `CurrentModel`.** The install seeds `s.snap` from `session/new`'s answer before the call so an earlier agent push is not kept over a newer reply, but a push landing in that one window is not resolved against it — pre-existing since S1b, not newly opened by this plan. | Fixing it means widening what the start-up seed claims already written; plan 025 left it as it was. | plan 025 X6; `internal/agent/live.go` (`start`'s local `snap` seed) | S |
| SF-35 | **`SetMode` has the same before/after-comparison shape `SetConfig` had before design 2/X5's fix, and was not this plan's to close.** Its locked section still writes `CurrentMode` over a mode update landing between the reply and the section; a reply that arrives after its caller has given up is dropped by design (no pending entry to `deliver` to), so the catalog stays as it was until the next one arrives. | Same class of fix as `SetConfig`'s, out of scope for a plan pinned to effort/speed. | plan 025 X9; `internal/agent/live.go` (`SetMode`) | M |
| SF-36 | **Accepted on the seam (astra r2), recorded for the trail:** the start-up seed (SF-29) is visible through `Snapshot()` before the delta that announces it; native's `announceCurrent` and the live session's setters share one `Close`-vs-announce window (guarded in both by this plan, but the shape — a caller closing between an install and the delta that announces it — is worth checking in any future setter); `Setting.ForModel` cannot tell an agent-initiated model move from craze's own in the instant between the worker's check and its write, or crossing it on the wire — ACP cannot order it. | Each is a narrow window the review weighed and accepted rather than closed. | plan 025 X6, X11 (astra r2) | — |
| SF-37 | **Accepted on the seam (astra r3), recorded for the trail:** a bound request refused `ErrStaleModel` after the settings worker's claim, while its caller's context ends in that same instant, is stored `aborted` (outcome unknown) rather than the refusal — a narrow weakening of an earlier finding; a malformed settings acknowledgement (the agent took the change but answered unreadably) leaves craze on the previous catalog until the agent's next one, never a fallback write or an optimistic revert. | Narrow windows the review weighed and accepted; not closed. | plan 025 X11, X14 (astra r3) | — |

## Recorded, not fixed — small, any phase

| id | what | why it is open | where | size |
|---|---|---|---|---|
| SF-20 | **A refused-`Open` ending can be enqueued after its turn's `EventDone`, and an early-answer goroutine that runs after the log closed writes nothing.** Both are card-less records for requests the agent already has its reply to. | Closing it needs request accounting that joins ACP's handler goroutines before the terminal flush or the log cutoff — the deadlock the close phases exist to avoid. Needs a design, not a patch. | `12` S1b deviations (PR 2, review r17 finding 6) | M |
| SF-21 | **A row-sourced turn refused by a foreign turn still loses its row in the TUI.** Today's behaviour, kept because goldens were the constraint. | Restore the row to the queue on the refusal. | `12` S1b; plan 021 §4, §9 | S |
| SF-22 | **A live (non-replay) update landing between a `session/load` reply and the load's install is overwritten by the reply**, exactly as a contradicting replay is. The install delta keeps folding clients level with `Snapshot()`; only the agent's live value is lost. | The session cannot tell that update from a replayed one while `replaying` is set. | `internal/agent/live.go` (`loadSession`'s doc) | S–M |
| SF-23 | **A model option that first appears carrying the value `CurrentModel` had before craze's own `SetModel` is suppressed once**, even if the agent genuinely went back to it. Indistinguishable from the stale report on the wire. | Needs an ordering signal the wire does not give; revisit if a provider adds one. | `internal/agent/live.go` (`modelBeforeSet`) | — |
| SF-24 | **An agent that sends 257 updates before its `session/new` reply, to a caller not yet reading, wedges `Start`** until the session closes. Verified the baseline's behaviour and no worse. | `craze prompt` reads only after `Start`; start its reader first, or dispatch pre-reply updates off `Start`'s goroutine. | `internal/agent/session.go` (`Session.Start`'s doc) | S |
| SF-25 | **`/model <id> <effort>` and the model dialog make two `Set`s**, which another client's `Set` can interleave. Each write is revision-guarded; the pair is not atomic. | A compound `Setting`, or accept it. | `internal/tui/slash.go` (`applyModelEffort`), `model_dialog.go` | owner, then S |
| SF-26 | `internal/cli/json_test.go`'s row "the queue row a signal took" is a stale label: that removal is an engine event with a `seq` since S1b; the row now pins "a queue event with no `Seq` prints no `seq` key". | The literals were frozen for S1b's byte-identity proof. They are not any more. | `internal/cli/json_test.go` (`TestJSONSeqExactLines`) | S |

## Verification gaps

| id | what | why it is open | where | size |
|---|---|---|---|---|
| SF-30 | **No live agent has ever raised a PERMISSION card**, on Linux or macOS: grok always approves, cursor never asks, native's gate is allow-all. `--no-force` answered, rejected, Esc'd, and an answer racing the turn's end rest on the fake agent and the Stub alone. | Re-run S1b's V1 leg 7 the first time something can ask: harness H3 (native permission prompts), or a provider configured to ask. | `12` S1b "Live smoke"; `discovery/native-harness/` H3 | S, with H3's smoke |
| SF-31 | cursor's `ask_question` live (it answered in prose twice); grok's interject-fallback foreign turn on macOS (7 attempts: that model ends its turn within a frame of its last token); cursor on macOS at all (login keychain over ssh). | Opportunistic: take them in the next phase's smoke. | `12` S1b "Live smoke" | S |
| SF-32 | **A native ask parked at `Close` commonly ends `cancelled`, by `call`, not `closing`.** Harness Plan 023 cancels the turn context first on purpose; the cheap shape that keeps `closing` (close the registry first, have the adapter hold a `closing` record until the turn context is done) is recorded there as an owner follow-up. | One sentence of decision, then adapter-only. | `05` (the `closing` outcome); `11`; `discovery/native-harness/` H5 | owner, then S |

## Found by S1b's live smoke, not S1b's — the owner triages

| id | what | evidence | size |
|---|---|---|---|
| SF-40 | **A pasted word that spells a key name is eaten by the composer.** `end` → gone, `go up the tree` → `go  the tree`, `left right` → empty, `ctrl+a and alt+b` → ` and`; `xendx` and `END` survive. bubbletea v1 splits a burst at spaces into `KeyRunes` messages whose `String()` is that word, and the textarea's default keymap consumes it as that key. A user pasting an error message loses words silently. | Reproduces against the fake agent with `tmux send-keys -l`; first seen as a grok interjection that went out without the word `end`. | S–M |
| SF-41 | **A grok interject-fallback foreign turn shows no spinner.** For ~7 s the screen looks idle (no `esc to interrupt`, composer back to its placeholder) while a row sits queued; the tab-title mark and the fallback note do cover it. `spinnerVisible()` keys off `statusWorking` alone. | S1b Linux smoke, leg 4b. | S |

## Standing open questions

Retention, size caps and secrets in the journal (SQ3), the wire sidecar's
default (SQ4), tool-event conflation (SQ15), and restoring a transcript from
earlier incarnations (SQ13) are in `10` with their defaults and are not
repeated here.
