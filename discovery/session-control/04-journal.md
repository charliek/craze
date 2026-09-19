# 04 — Journal

craze journals **every session, every provider** to disk (SD-07). The journal
has two jobs, and the owner asked for both on 2026-09-19:

1. **Replay and attach.** A client that connects late, reconnects, or
   attaches after the host restarted gets history from the journal instead of
   depending on the agent's own `session/load` replay.
2. **Debug and improvement data.** A record rich enough to look over later
   for errors, slow paths, and product optimizations, for ACP agents and the
   native harness alike.

## Two rails (SD-08)

The harness design rule (`discovery/native-harness/`, reference review) is two
persistence rails from day one. They stay separate:

| rail | what | where | owner |
|---|---|---|---|
| model-facing history | what is sent back to the model: Fantasy messages in a tree | `$CRAZE_HOME/native/sessions/…` (`internal/harness/store`) | the harness; native only; unchanged |
| **UI-facing update log** | what happened, in order, as clients saw it | `$CRAZE_HOME/journal/…` | the engine, above the provider seam; all providers |

`internal/harness` stays ignorant of the journal; the depguard rule is
unchanged. The harness hands diagnosis-grade events to its sink and the
adapter forwards them (`11`). Journal lines carry the store entry ids that a
step wrote, so the two rails join.

For ACP providers the journal makes craze a second source of truth beside the
agent's own replay. That is accepted (owner, 2026-09-19): the agent's store is
opaque, provider-specific, and cannot be mined.

## File

```
$CRAZE_HOME/journal/<cwd-slug>/<UTC yyyymmddThhmmssZ>_<session-id>.jsonl   0600
$CRAZE_HOME/journal/<cwd-slug>/<…>_<session-id>.wire.jsonl                 0600, optional (SQ4)
```

Same slug and timestamp scheme as the harness store so the two sort and pair
by eye (SQ1). Append-only JSONL, torn last line tolerated on load, created
lazily on the first event, one writer (the host). JSONL over SQLite for the
same reasons as the harness (`D-03`): greppable, append-only, no migrations
to keep replayable forever.

## Lines

Line 1 is a header; every other line has `seq` (monotonic, the attach cursor),
`ts` (RFC3339Nano UTC), and `type`.

| type | payload | purpose |
|---|---|---|
| `header` | format version, session id, provider, agent binary + version, craze version, OS/arch, cwd, resumed-from id, host instance id | provenance for every later question |
| `event` | the `agent.Event` in its wire JSON (`05`), entry id it touched | replay; identical bytes to what subscribers got |
| `command` | command id, verb, arguments (prompt text included), client kind (`tui`, `socket`, `bridge`, `web`), outcome or error code, latency | who did what, and what craze answered |
| `ask` | ask id, kind, options offered, transitions with timestamps, answering client, time blocked | the "blocked on you" record; approval latency |
| `turn` | start/end, duration, stop reason, time to first event, tool count, error count, tokens and cost when known, queue depth at start | one line per turn to aggregate over |
| `client` | attach / detach / dropped-as-slow, client kind, protocol version, resume cursor used | reconnect and backpressure behavior |
| `diag` | kind + fields: agent stderr lines, wire outcome (`pending/sent/refused/failed/withdrawn`), ACP errors with codes, reconnects, host signals, panics recovered, harness diagnostics | everything that is not transcript |

Native harness diagnostics arrive as `diag` lines from the adapter: tool
start/end and duration, exit code and error class, truncation (kept vs total,
spill path), doom-loop trips, length-stop handling, cancel-synthesized tool
results, steers, per-step provider / model / wire model, raw **and** normalized
finish reason, time to first token, usage including cache-read tokens, retry
attempt + reason + backoff. Plan 019 (H2) already pins these on
`harness.Event` (`11`).

The optional `.wire.jsonl` sidecar holds raw ACP JSON-RPC frames in both
directions with direction and timestamp. H0's lesson applies
(`discovery/native-harness/07-roadmap.md`, conventions): a recorder that
discards raw responses turns a diagnosable failure into a mislabelled one.

## Rules

- **Append-only, never rewritten.** Corrections are new lines.
- **Sequence numbers are the protocol's cursors.** A subscriber's `afterSeq`
  is a journal position; nothing else numbers events.
- **Streaming text** is journaled per transcript entry, not per delta: the
  writer coalesces deltas for an entry and flushes on size, time, kind change,
  or turn end (shed-gx's segmentation: 8 KiB / 2 s). Replay into the middle of
  an entry re-sends the entry as an upsert by id (SQ2).
- **The journal may backpressure the engine briefly; nothing else may.** A
  failing disk degrades to "journaling off" with one `diag` and a TUI notice,
  never a stalled agent.
- **Secrets.** Tool output and prompts can contain secrets. Files are `0600`
  in a `0700` tree, never uploaded, and `journal = false` turns it off (SQ3).
  The remote phases must treat journal content as sensitive as the session.
- **Version the format** with an integer in the header. Old journals must stay
  loadable, which is t3code's hard-won rule for persisted events.

## Later, not scheduled

`craze journal` subcommands to mine the data: errors by provider and tool,
turn latency percentiles, ask wait time, retry rates by model, slow-subscriber
drops. The line shapes above are chosen so these are `jq` one-liners first.
