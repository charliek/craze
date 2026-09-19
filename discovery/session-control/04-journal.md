# 04 — Journal

craze journals **every session, every provider** to disk (SD-07). The journal
has two jobs, and the owner asked for both on 2026-09-19:

1. **Replay and attach.** A client that connects late or reconnects gets the
   events it missed from the ring and the journal (`03`).
2. **Debug and improvement data.** A record rich enough to look over later
   for errors, slow paths, and product optimizations, for ACP agents and the
   native harness alike.

Revised 2026-09-19 after the panel review (`12`): SD-18, SD-21, SD-22, SD-23.

## Two rails (SD-08)

| rail | what | where | owner |
|---|---|---|---|
| model-facing history | what is sent back to the model: Fantasy messages in a tree | `$CRAZE_HOME/native/sessions/…` (`internal/harness/store`) | the harness; native only; unchanged |
| **UI-facing update log** | what happened, in order, as clients saw it | `$CRAZE_HOME/journal/…` | the engine, above the provider seam; all providers |

`internal/harness` stays ignorant of the journal; the depguard rule is
unchanged. The harness hands diagnosis-grade events to its sink and the
adapter forwards them (`11`). The rails are **joined, never merged**: a journal
line can name the store entries a step wrote, and must say whether that write
**succeeded**, because the harness emits `StepDone` even when the store append
failed. A UI journal can legitimately contain streamed output that is absent
from the recovered model context; the join is what lets a reader explain the
divergence.

`journal = false` turns off the journal and its wire sidecar. It does not and
cannot turn off the native model-history rail.

## One file per host incarnation (SD-21, SD-22)

```
$CRAZE_HOME/journal/<cwd-slug>/<UTC yyyymmddThhmmssZ>_<incarnation-id>.jsonl        0600
$CRAZE_HOME/journal/<cwd-slug>/<UTC yyyymmddThhmmssZ>_<incarnation-id>.wire.jsonl   0600, optional (SQ4)
```

A journal file belongs to exactly one process lifetime. **No process ever
appends to a file another process wrote.** That is the recovery policy: a torn
last line is tolerated on read and never becomes interior corruption, because
the next incarnation starts a new file. The harness store's harder problem
(it poisons further writes after a partial append) does not arise.

The header names the incarnation, and later lines record the provider session
id once `Start` learns it, the id it was loaded from, and, from S1b, the
durable craze session id. A session's history across restarts is the ordered
set of its incarnation files, linked by those ids.

Same slug and timestamp scheme as the harness store so files sort and pair by
eye (SQ1). JSONL over SQLite for the harness's reasons (`D-03`).

## Transcript authority (SD-23)

- **Within an incarnation** the ring plus the journal are authoritative for
  attach and resume.
- **Across incarnations** the provider's own replay (`session/load`) stays the
  transcript authority for ACP sessions, as today. The new incarnation
  journals the replayed events, flagged `Replayed`, so each file is
  self-contained and restoring from it never duplicates history.
- Earlier incarnations' files are debug data. Restoring a transcript from them
  instead of, or reconciled with, the provider's replay is a later decision
  (SQ13): it needs rules for which source wins when they differ, for turning
  historical asks and interrupted turns into terminal records rather than
  actionable ones, and for a failed load never reaching `synchronized`.
- Native sessions have no resume until H7, whose replay is the store's
  leaf→root walk; that walk is the cross-incarnation authority for native
  (`11`).

This narrows the first draft's promise. The journal makes attach exact while a
host lives and gives a complete record afterwards; it does not yet make craze
the source of truth for a dead host's transcript.

## Lines

Line 1 is a header; every other line has `ts` (RFC3339Nano UTC) and `type`.
`event` lines carry the session's `seq`; other lines are ordered by position.

| type | payload | purpose | from |
|---|---|---|---|
| `header` | format version, incarnation id, craze version, OS/arch, provider, agent binary, cwd, options that shape behavior (force, interactive, mode) | provenance for every later question | S1a |
| `session` | provider session id, loaded-from id, durable craze id (S1b), agent version when known | identity as it becomes known | S1a |
| `event` | `seq` + the `agent.Event` in the **lossless codec** (`03`) | replay; one line per emitted event, never coalesced (SD-18) | S1a |
| `prompt` / `prompt_end` | prompt id, the text as typed, kind (prompt or interject); then stop reason, error class, duration | the user's side of the conversation, which never reaches the event stream until S1b (SD-30). Journal-only, no `seq` | S1a |
| `diag` | kind + fields: agent stderr lines, wire outcomes, ACP errors with codes, signals, recovered panics, harness diagnostics, subscriber drops, journal health | everything that is not transcript | S1a |
| `command` | command id, verb, arguments, client kind, outcome or error code, latency | who did what, and what craze answered | S1b |
| `ask` | ask id, kind, options offered, transitions with timestamps, answering client, time blocked | the "blocked on you" record | S1b |
| `turn` | engine turn id, start/end, duration, stop reason, time to first event, tool and error counts, tokens and cost when known, queue depth | one line per turn to aggregate over | S1b |
| `client` | attach / detach / dropped-as-slow, client kind, protocol version, cursor used | reconnect and backpressure behavior | S2 |

Native harness diagnostics arrive as `diag` lines from the adapter: tool
start/end and duration, exit code and error class, truncation (kept vs total,
spill path), doom-loop trips, length-stop handling, cancel-synthesized tool
results, steers, per-step provider / model / wire model, raw **and** normalized
finish reason, time to first token, usage including cache-read tokens, retry
attempt + reason + backoff, and the store entry ids a step wrote **with the
persistence outcome**. Plan 019 (H2) pins most of these on `harness.Event`;
treat that as an integration dependency to verify when both have landed, not
as a code fact (`11`).

The optional `.wire.jsonl` sidecar holds raw ACP JSON-RPC frames in both
directions. It is off until its size is measured (SQ4); when it ships it has a
byte cap after which it stops with one `diag`, rather than rotating, because
rotation would break the one-file-per-incarnation pairing. H0's lesson
applies: a recorder that discards raw responses turns a diagnosable failure
into a mislabelled one.

## Writer rules (SD-21)

- **The writer never blocks the engine.** It is fed without blocking into a
  bounded queue and writes on its own goroutine. "Backpressure briefly" was
  dropped: a regular-file write that stalls is not bounded.
- **Overflow or a write error is a gap, recorded and published.** The writer
  marks the journal degraded, says so once in the TUI and in `diag` when it
  can, exposes journal health outside the journal itself, and invalidates
  replay guarantees across the gap: a resume that needs the gap gets `reset`.
- **Readers see complete records only.** The writer publishes a
  complete-record boundary (bytes flushed through the last newline); replay
  reads to that boundary and takes the rest from the ring. Replay decodes
  with bounded memory, never a whole-file load.
- **Durability is stated, not implied.** Flush on a short timer and at turn
  end; `fsync` at turn end and close; a crash can lose the last unflushed
  window, which the ring covered while the host lived.
- **Shutdown is bounded**: a final flush with a deadline, like
  `Close`'s owed writes in plan 017.
- **Disk exhaustion** degrades to journaling off with one notice, never a
  stalled or failed session.
- **Append-only, never rewritten.** Corrections are new lines.
- **Secrets.** Tool output and prompts can contain secrets. Files are `0600`
  in a `0700` tree, never uploaded (SQ3). Anything remote must treat journal
  content as sensitive as the session itself.
- **Version the format** with an integer in the header. Old journals must stay
  loadable.

## Later, not scheduled

`craze journal` subcommands to mine the data: errors by provider and tool,
turn latency percentiles, ask wait time, retry rates by model, slow-subscriber
drops, journal gaps. The line shapes are chosen so these are `jq` one-liners
first. Retention and pruning wait for measured sizes (SQ3); one size to
measure is the cross-incarnation aggregate, because each resume re-journals
the provider's full replay (SD-23), so a session resumed N times costs about N
times its history.
