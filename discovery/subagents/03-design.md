# 03 — Design

The shape that worked in [02](02-experiments.md), turned into craze features.
File references are to `origin/main` at `7016071`.

> **Revised by plan 041's panel (2026-10-10).** Where this file and the plan
> differ, the plan is right. In short:
> - the host is spawned by the waiting command itself, not through
>   `session.create`, so T1 changes no protocol (TD-08);
> - an unattended host runs with native background work off, so one turn is one
>   result (TD-09);
> - `wait` follows a resume handle (an attach cursor plus its turn or queue
>   row), and a result is built from the turn's own events, not a transcript
>   scan;
> - the index mark is provenance that records the start settings for reloads,
>   not policy;
> - signals touch only the command's own row or turn;
> - the plugin's Bash calls need an explicit 600-second timeout (the default is
>   120 s).

## Layers

```
Claude Code orchestrator (Opus)
  └─ Agent: craze:sol            Haiku 5.5, high effort, Bash (+ Write) — TD-02
       └─ craze new --wait …     Go client inside craze — TD-06
            └─ hub: session.create        per-machine hub, spawns the host
                 └─ craze serve           detached host, runs the session — TD-05
                      └─ native harness | cursor-agent | grok
```

The sub-agent is an ordinary craze session. It outlives each command, so a
follow-up keeps its context, `craze attach --session <short>` watches it live,
and a turn longer than one Bash call keeps running between waits.

## Commands (TQ-1: plain verbs)

| Command | Does |
|---|---|
| `craze new --wait [flags] [--prompt-file F \| PROMPT]` | Creates a session through the hub, prompts it, waits for that turn to end, prints the result. Without `--wait`, `craze new` is unchanged. |
| `craze send <id> [--wait] [--prompt-file F \| PROMPT]` | Prompts a running session, queued behind any turn already running. With `--wait`, waits for the turn this prompt becomes and prints its result. If the session's host has exited, reloads it first (TQ-4). |
| `craze wait <id>` | Waits for the current turn to end and prints its result. With nothing running, prints the last turn's result at once. |
| `craze cancel <id>` | Cancels the current turn (`session.cancel`). |
| `craze stop <id>` | Ends the session (`session.stop`). |

A separate `craze result` (TQ-1's sketch) is not needed: `wait` on an idle
session already is one.

**Shared flags.** `--json`, and `--timeout <duration>` on the waiting commands.

- With a timeout, a turn still running when it passes is reported as
  `running`, with its own exit code; `craze wait` picks it up later. The
  plugin passes about nine minutes, under Bash's ten-minute cap.
- With no timeout (the default, for a person at a terminal), the command
  waits as long as the turn runs.

**Session flags on `new --wait`.** The ones `craze new` has, plus a start mode:

- `--provider`, `--model`, `--effort`, `--fast`/`--no-fast`, `--no-force`,
  `-C`/`--dir`;
- `--plan`/`--ask`, which `session.create` cannot pass today (G3).

**Ids.** Any session-id argument takes what `craze ps` prints. It is resolved
by `matchSession` (`internal/cli/bridge.go:178-202`) over live hosts, then
against the session index for an ended session (TQ-4). The full id goes on
the wire, which every session-scoped method requires
(`internal/control/handlers.go:129-131`); this closes G9.

**Prompt input.** In order of precedence: `--prompt-file <path>` (`-` for
stdin), then the positional words, then stdin when it is not a terminal.
Empty is a usage error.

### How a wait works

`new --wait`:
1. Creates the session with **no** prompt. The hub's create reports a first
   prompt only as accepted, refused or unknown, never with its turn id
   (`internal/hub/create.go:1173-1198`).
2. Attaches with `remote.DialSession` and `WhenReady`, as `craze attach` does
   (`internal/cli/attach.go:296-312`).
3. Submits the prompt. A prompt that starts at once is answered `{turn, text}`
   (`internal/control/handlers.go:398-412`).

`send` on a busy session gets a queued row instead. Its turn is found from the
queue's `sent` event for that row, then the settlement's `ended{next}` and the
next `started` (`internal/engine/engine.go:2041-2063`). A failed turn, a
cancel or a stop clears the queue (`engine.go:2017-2050`); a row removed that
way reports `dropped`.

The client folds every event into a `transcript.Model`
(`internal/transcript/fold.go:31`) and stops at the `turn` event with
`phase: ended` for its turn id. The result comes from the fold:

- **message**: the last assistant entry of *this* turn.
  `Progress().LastReply` (`internal/transcript/progress.go:33-62`) scans back
  past turn boundaries (`:54-59`), so a turn that wrote no text would return
  the previous turn's reply. The scan has to stop at the turn's own user
  entry.
  - Tools and thoughts already split assistant entries
    (`internal/transcript/transcript.go:737-766`), so the last entry is the
    final message, the same cut the prototype made by hand.
  - An entry keeps at most 64 KiB, its tail (`internal/transcript/bounds.go:19-23`).
    The result says when a message was cut.
- **files**: the paths of the turn's edit-kind tool calls.
- **toolCalls**, **durationMs**, and the **stop reason** or **error** from the
  `ended` event.
- **usage**: `session.state`'s `settings.usage` (`internal/control/info.go:166-193`),
  for native sessions. Absent for ACP sessions, which report none.

Turn ids restart at `turn-1` when a host restarts
(`internal/agent/session.go:196-198`). A reload (TQ-4) is a new host, so the
client keys a wait on the host's epoch as well as the turn id.

### Result

Plain output is the message, then a footer: status, seconds, tool calls,
usage when there is any, and the session id with its `craze attach` command.
With `--json`, one object:

```json
{"session":"01a126cd-…-397e175bfe79","short":"175bfe79","turn":"turn-2",
 "status":"completed","stopReason":"end_turn","message":"…","messageCut":false,
 "files":["stats/core.py"],"toolCalls":5,"durationMs":27100,
 "provider":"native","model":"chatgpt/gpt-6.1-sol",
 "usage":{"contextTokens":16590,"contextWindow":272000,"turn":{…},"session":{…}},
 "attach":"craze attach --session 175bfe79"}
```

`status` is one of `completed`, `failed`, `cancelled`, `dropped`,
`needs_input` or `running`. Each non-`completed` status has its own exit code,
as `codex-run.py` does; the plan pins the numbers. Exit 2 stays craze's usage
error, and exit 1 a failure of craze itself. Unpriced usage, from a plan
subscription or an unpriced model, is shown as unpriced, not as $0.

### Cancelling on a signal

A waiting command that gets SIGINT, SIGTERM or SIGHUP cancels its own turn
(`session.cancel` with its `turnId`, which refuses `stale_turn` rather than
cancel a later one, `internal/engine/cancel.go:10-55`). It then exits. This
is how `TaskStop` on a wrapper is meant to stop the craze turn. Which signal
`TaskStop` actually delivers to a sub-agent's Bash call is not documented;
the plan verifies it live (TQ-9).

## Host and protocol

All additions are optional `session.create` params, advertised by one new
hub capability. That follows the protocol's rule: a client sends a new
optional param only after `hello` advertises it (`docs/reference/protocol.md`,
"Tolerant inbound, strict outbound").

**`mode`: `agent`, `plan` or `ask` (G3).** It goes to the host's existing
`Spec.Ask`/`Spec.Plan` (`internal/hostspawn/spawn.go:122-125,171-176`), which
the hub's create never sets today (`internal/hub/create.go:693-695`). Setting
the mode at the start matters:
- `session.set` takes the provider's raw mode id, not `plan`/`ask`
  (`internal/agent/live.go:1771-1776`, against `ResolveMode` at `:635`);
- it is refused until the session is up (`internal/engine/settings.go:170`);
- the journal records only the start mode (`internal/journal/writer.go:397`).

**`unattended: true` (TQ-2, TQ-7).** Marks a session that nobody is
expected to answer. It does two things in the host:

1. **An approval policy.** `craze serve` runs with `Interactive: true` and no
   `Approval` (`internal/cli/tui.go:321`, `serve_cmd.go:476`), so a question
   or a plan parks until someone answers it (`internal/agent/asks.go:262-266,303-310`).
   An open ask also stops the host from idling out
   (`internal/engine/closefence.go:85-92,187-190`). So today an unattended
   question leaves a host and its agent running indefinitely.
   - An unattended host sets `Options.Approval`, as `craze prompt` does
     (`internal/cli/prompt.go:206-217`): questions and plans are answered by
     the policy.
   - A permission request (only under `--no-force`, since native never raises
     one, `internal/harness/tool/gate.go:9-12,51`) stays open, and the waiting
     command reports `needs_input`.
   - It must not be done by setting `Interactive: false`, which also turns off
     native background children (`internal/agent/native.go:1259-1261`).
2. **A mark in the session index**, so `craze -c` skips the session:
   - `sessions.Row` (`internal/sessions/sessions.go:70-95`) gains a field,
     written first-wins like `crazeId`.
   - `Store.Latest`, behind `continueRow` (`internal/cli/sessionflags.go:121-131`;
     `sessions.go:358-402`), skips marked rows.
   - `Recent`, behind `--resume`'s picker, and the session list's `All` still
     list them, labelled.
   - Older crazes keep unknown keys when they rewrite the file
     (`sessions.go:97-105,679-685`).

`new --wait` always sets `unattended`. A plain `craze new` does not, since a
person may attach to it.

**Request hashes.** `createHash`'s key struct has no `omitempty`
(`internal/hub/create.go:474`). A new field would change every request's
hash, and a create retried across a hub upgrade would then be refused
`request_conflict`. New fields join the hash only when set.

**The capability and the fixtures.** A capability that is true on every
current hub changes the `hello` lines in two existing wire fixtures (22 and
26). `scripts/verify/v8.sh` reports that as `WIRE-MOVED: needs owner
approval`. The plan asks for that approval up front.

**Old hubs.** A hub already running from an older craze lacks the capability.
`craze new --wait` refuses with the existing "cannot create sessions; it
exits when idle" style of message (`internal/hub/ensure.go:210-225`), as
`craze new` does for a hub without `sessionCreate`.

## Reloading an ended session (TQ-4)

The hub cannot load a session: it never sets `Spec.Load`, and it treats a
"held" ready line as `spawn_failed` (`internal/hub/create.go:729-736`). The
TUI loads saved sessions **without** the hub, through its own host spawn
(`openSaved`, `internal/cli/session_list.go:262-274`; `loadBackend`,
`launch.go:387-395`; `spawnHost`/`serveSpec` with `loadArg`,
`internal/cli/spawn.go:140-264`). That path already handles a host that holds
the session.

`craze send` uses the same path when the id names an index row with no live
host. The reloaded host must be unattended again: the row's mark says so, and
the spawn spec carries it. A loaded session that is not prompted within five
minutes idles out under the never-prompted cap (`internal/cli/idle.go:58-63`).
`craze send` prompts it at once, so the cap does not bite.

## `craze -c`

Hub-created sessions are indexed exactly like interactive ones
(`internal/cli/serve_cmd.go:506`; the engine's `upsert`,
`internal/engine/index.go:781-808`). A sub-agent run in a person's repository
would therefore become `--continue`'s newest row there, and their next `craze
-c` would load the sub-agent instead of their own session. The unattended
mark above fixes it. `craze prompt` does not have the problem: it writes no
row (`internal/cli/prompt.go:240`).

## Not in the TUI (TD-04)

The commands are command-line only. What the TUI shows:
- unattended sessions in the session list and `--resume`'s picker, labelled;
- `craze -c` skipping them.

Attaching to one with `craze attach`, or opening it from the list, works as
for any session.

## Alternatives that lost

- **Extending `craze prompt`.** It runs the session in its own process with
  no index and no hub. Making it resumable would duplicate the hub path, and
  its in-process behaviour is what the tests, the V2 parity check and `eval/`
  depend on (TD-05).
- **A Python runner shipped with the plugin.** It needs locating at run time,
  needs Python, and would reimplement reconnect and retry that
  `internal/remote` already has (TD-06).
- **Answering asks from the client instead of the host.** `asks.answer` works
  without an attachment (`internal/control/handlers.go:581`), so a waiting
  command could answer asks. But a session with no command waiting would
  still park forever and never idle out. The policy belongs in the host.
- **A `craze task` command group.** See TQ-1.
- **An MCP server, or a gateway.** TD-07 and TD-03.
