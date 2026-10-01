# CLI Reference

```bash
craze [flags]
craze prompt [text] [flags]
craze bridge [flags]
craze attach [flags]
craze ps [flags]
craze new [prompt...] [flags]
craze serve [flags]
craze auth login [provider]
craze auth logout <provider>
craze auth list
craze version
```

The default command is the [interactive TUI](tui.md). It refuses to start on a
non-tty.

## Flags

These flags apply to `craze` (the TUI). `craze prompt` shares the session
flags; see [craze prompt](#craze-prompt).

| Flag | Description |
|------|-------------|
| `--workspace` | Existing workspace directory (default: current directory) |
| `--model` | Model to start on: an ACP model id, or on `native` a model alias. Native resolves it against every model it knows — shipped or yours, connected or not — and a model whose provider has no key refuses to start, saying how to give it one. It applies to this start only, and is never remembered as a default; a new native session takes the effort last picked for that model in a session's `/model`, when the model still offers it ([Model memory](configuration.md#model-memory-recentjson)) |
| `--effort` | Effort to start at, where the model offers an effort setting: a level's id (`high`), the id in any case (`HIGH`), or its name (`extra high`) — tried in that order, and a step counts only when it matches exactly one level. Set after `--model`, so against that model's levels, and before the session takes any prompt. A value that matches no level, or more than one, or a model with no effort setting, leaves the model's own effort, with one line on stderr. Applies to this start only — a `--continue` or `--resume` load included — and is never remembered |
| `--fast`, `--no-fast` | Start with the model's fast setting on, or off, where the model offers one (cursor's per-model `fast`). Neither leaves it as the model has it. Set as `--effort` is; a model without one is a line on stderr |
| `--agent-bin` | Path to the agent binary for this launch's own provider (or `CRAZE_AGENT_BIN`); a session of another provider uses its own. See [Agent binaries](configuration.md#agent-binaries) |
| `--provider` | Provider: `cursor`, `grok`, `gx` (ACP agents) or `native` (runs inside craze). Empty is unset. Unknown id exits 2 |
| `--force` | Spawn the agent with `--force` / `--always-approve` (yolo). Default: on |
| `--no-force` | Disable yolo and handle permission requests |
| `--no-mouse` | Disable mouse reporting (wheel scroll and clicks) |
| `--no-background` | Keep the terminal's own background and text colours instead of the theme's. Same as `background = false` |
| `--no-host-status` | Report nothing to the herdr pane or roost tab craze runs in, and leave the agent's environment whole. Same as `host_status = false`. See [Host status](configuration.md#host-status) |
| `--theme` | TUI theme preset. See [Configuration](configuration.md) |
| `--ask` | Set session mode to ask after `session/new` |
| `--plan` | Set session mode to plan after `session/new` |
| `--plugin-dir` | Extra plugin directory whose commands and skills craze expands (repeatable). Relative to the workspace; a missing directory is a diagnostic on stderr, not an error; ignored (with a diagnostic) on Grok |
| `--continue`, `-c` | Load the newest session in this workspace instead of starting a new one. No matching session exits 1 |
| `--resume`, `-r` | Open a picker of the last 10 sessions in this workspace. Empty index exits 1 the same way; `Esc` in the picker exits 0 |

`--ask` and `--plan` are mutually exclusive. So are `--fast` and `--no-fast`,
and `--continue` and `--resume` (exit 2).

`--effort` and `--fast`/`--no-fast` are set inside the session's start, after
`--model` and `--ask`/`--plan`, so no prompt — typed, queued, or sent by
another client — reaches the agent before them; that holds whether the
session runs in a [detached host](#sessions-outlive-their-terminal) or, under
`detach = false`, in the TUI's own process. A setting the agent refuses is a
line on stderr, and the session starts without it; one the agent does not
answer within 15 s fails the start. Each one that is skipped or
refused is also a `diag` line in the session's
[journal](configuration.md#session-journal) (`effort_unmatched`,
`fast_unmatched`, `start_setting_refused`).

`--provider` on the TUI skips the startup picker. Without it, `$CRAZE_PROVIDER`
then `provider` in the config file then `cursor` is the default, and the picker
lets you change it before Start. The picker holds its choice to the same flags
`--provider` is held to: picking `native` with `--agent-bin` or
`CRAZE_AGENT_BIN` set shows the message `--provider native` would exit 2
with, as an error row, and starts nothing (see [TUI reference](tui.md)).
`--agent-bin` and `CRAZE_AGENT_BIN` are the resolved provider's: another
provider picked there runs the binary `[agents]` names for it, or the one on
`PATH` ([Agent binaries](configuration.md#agent-binaries)).
`craze prompt` has no picker; it uses the same precedence. `craze frame`
ignores env and config and defaults to cursor unless `--provider` is passed.

```bash
./bin/craze
./bin/craze --provider grok
./bin/craze --workspace ../proj
./bin/craze --no-force
./bin/craze --theme gruvbox --no-mouse
./bin/craze --continue
./bin/craze --resume
./bin/craze --provider grok --continue
```

### `--continue` and `--resume`

Both load a session out of the session index
(`~/.craze/sessions.jsonl` — see
[Configuration](configuration.md#session-index)), filtered to the current
workspace. An **explicit** `--provider` filters the search further; a
provider that only came from `$CRAZE_PROVIDER` or `config.toml` does not, so
a plain `--continue` finds the newest session for this workspace across
every provider, and picker rows show each row's own provider. Whichever row
is loaded, its own provider starts the agent — it overrides whatever
`--provider` would otherwise have resolved to, and it is never written back
as the persisted default (see
[Configuration](configuration.md#session-index)).

No matching row:

```text
craze: no session to continue in /abs/workspace
craze: no session to continue in /abs/workspace for provider grok
```

exit 1, and no TUI is drawn. A session index that exists but craze cannot
parse fails the same way, exit 1, naming the file; the index is never
rewritten by a failed read. `--continue` and `--resume` together is a usage
error, exit 2, and `--resume` on a non-tty gets the ordinary non-tty
refusal — checked before either flag is considered, so it fires even for a
workspace with no sessions at all.

A load the agent itself refuses (no `session/load` support, an unknown
session id, a working directory Grok or gx reject) is shown as an on-screen
error exactly like a failed `session/new`; the row stays in the index, craze
never falls back to starting a new session, and the exit code on quit is 1.

See [`/rename`](tui.md#slash-commands) and
[Resuming a session](tui.md#resuming-a-session) for what a restored session
looks like in the TUI.

### A session already running in another craze

*Everything below describes the claim as the in-process path makes it. With
[detached hosts](#sessions-outlive-their-terminal) (the default) the
launching `craze` first attaches directly to a live host that serves the
session, and otherwise spawns a host, which makes this same claim and answers
`held` when another craze has it — the launcher then finds the holder and
attaches to it. The refusals are worded the same, but the detached launcher
prints no line before the TUI for a plain reattach; only when the command line
gave flags a new session would take does it print the ignored-flag note
(`craze: that session is already running (pid N); attached to it (ignored:
--model)`), after the TUI exits. The stderr line shown below is the in-process
path's.*

`--continue` claims the row it loads before anything is built: its own lock
under `~/.cache/craze/locks/`, independent of `CRAZE_HOME` and the runtime
directory (see [Protocol reference](protocol.md#reaching-a-host)). **A row
that already has a craze id is claimed first, and a session another running
`craze` already holds is attached to instead of refused** — the second
`craze -c` becomes [`craze attach --session <id>`](#craze-attach), resolved
through the holder's host id, after one line on stderr:

```text
craze: that session is already running (pid N); attaching
```

The flags a *new* session would take (`--model`, `--effort`,
`--fast`/`--no-fast`, `--ask`, `--plan`, `--agent-bin`, `--provider`) do not
apply to an attach; each one this command
line explicitly passed is named in the note — whether the flag was
**changed** on the command line, not what it is worth — e.g. `(ignored:
--model, --provider)`. Nothing is built, spawned, bound, or claimed for it.
**A nonempty, explicit `--provider` also does one more thing the others
don't**: it filters the session index *before* the lookup that finds the row
to claim ([above](#-continue-and-resume)), so a `--provider` that excludes
the held session's own provider means that row is never found at all — the
ordinary no-match refusal fires (`craze: no session to continue … for
provider …`), and the attach path, note included, is never reached. An
empty, or all-whitespace, explicit `--provider` (`''`, `' '`) does **not**
filter the index — only a value with visible content counts — so the row is
still found and the attach path is still reached — and `--provider` is still
named in the note, exactly like the others, because it was still changed on
the command line. Two cases keep the plain "session held" refusal instead of
attaching:

- **A legacy row** (no durable craze id yet, from before `crazeId` existed)
  is refused before the claim, whose `EnsureCrazeID` would otherwise mint
  and write its id for a load the command line can never start.
- **The spawn flags this command line gives are refused first**, once this
  run holds the claim: `craze -c --agent-bin X` for a *held* session still
  attaches (an attach takes none of the spawn flags), but for a session this
  run just claimed itself, the same usage errors a new session's flags would
  give still apply, and the claim is released.

A holder whose live registry entry does not serve the claimed session (or
whose socket has not been bound yet) keeps the plain refusal instead, with
why appended: `— it serves no control socket` or `— it serves another
session`. A holder whose lock names no pid yet (`pid ?`, the instant after
its `flock`) also keeps the plain refusal:

```text
craze: that session is already running (pid N)
```

exit 1, and no agent is spawned — nothing is built once the row's session is
found to be held and cannot be attached to. Two narrower refusals cover the
claim mechanism itself, not the session:

```text
craze: the session index is busy — try again
craze: the session index changed — try again
```

The first is another writer holding the session index past a two-second
bound; the second is a legacy row with no durable id that left the index
between being read and being given one (another loader may already hold it
under an id this one does not know about — trying again re-reads the index).

`--resume`'s picker claims each row the same way, when you press <kbd>Enter</kbd>
on it — but it never attaches itself: a held row's error names the holder's
pid, plus `craze attach --session <hostId>` when the holder's live entry
actually serves that session, or why it cannot be reached instead (`— it
serves no control socket`, `— it serves another session`); the picker keeps
running either way, in place of loading it.

An unreadable lock tree or session index does not fail one way. The initial
lookup — `--continue`'s `Latest`, or `--resume`'s scan for rows — still exits
1 if it cannot be read, the same as no session found at all: there is no row
yet to warn about. Once a row is in hand, two things warn on stderr and let
the load proceed **unclaimed**: a **legacy row** (one with no durable
`crazeId`) whose id cannot be written to the index, and any failure to take
the session's claim other than another craze holding it (the lock tree
unusable, the lock file unopenable or unwritable) — whether the row already
had an id or was just given one. An unclaimed load has no protection
against a second craze loading the same session; the lock exists to stop
one, not to lock you out of your own session because a filesystem
misbehaved. Three things still refuse instead: the session already
**held** by another craze, a **busy** index, and a legacy row that
**vanished** from the index before it could be given an id.

### Native sessions

Native sessions are indexed and resumable on the same terms as cursor's and
grok's: `--continue` and `--resume` find and load a native row the same way,
and `--plan`/`--ask` set the resumed session's mode explicitly, overriding
whatever mode the transcript's own last `mode_change` recorded (see
[Resuming a session](tui.md#resuming-a-session)).

`--agent-bin`/`CRAZE_AGENT_BIN` refuse a native row exactly as they refuse a
new native session — it has nothing to spawn — exit 2, before any index
write, crazeId mint, or session claim (`--continue` still reads the index to
find the row before the refusal is reached):

```text
craze: --agent-bin cannot be used with provider native, which runs inside craze
craze: CRAZE_AGENT_BIN cannot be used with provider native, which runs inside craze; unset it
```

A row whose transcript file does not exist yet — its first prompt was
cancelled before any output came back — is not a failed load: craze opens an
**empty** session under that same id instead of refusing. This is the one
deliberate exception to "a load never falls back to a new session" above —
it is still the same session, continuing from nothing, not a different one,
so nothing is silently switched out from under you.

A native transcript adds its own lock beneath [the claim
above](#a-session-already-running-in-another-craze): a transcript another craze
process already has open refuses the load the same way. A transcript left
with a torn or cut-off tail by a crash is trimmed back to its last complete
step on open, and the dropped bytes are kept beside it in a
`<stem>.torn-<UTC stamp>` file — the transcript's name with its `.jsonl`
suffix removed, e.g. `20260926T093537Z_<id>.jsonl`'s torn tail lands in
`20260926T093537Z_<id>.torn-20260927T101500Z` — rather than discarded; a tail written by
a newer craze build — one this version does not recognize — is never
trimmed, and the load is refused instead, so nothing unrecognized is thrown
away.

### Sessions outlive their terminal

The ordinary `craze` no longer runs the session inside the terminal's
process. It spawns a detached host — [`craze serve`](#craze-serve), in a
session of its own with no terminal — waits for the host to say it is ready,
and runs the TUI as that host's client over the [control
socket](protocol.md#reaching-a-host) (plan 030 §3.5). What follows:

- **Closing the terminal keeps the session.** A closed terminal (SIGHUP), a
  SIGTERM to `craze`, and a dropped ssh connection are all *view closes*:
  the TUI goes, the host and its agent stay, and a turn already under way
  keeps working.
- **`craze -c` reattaches.** `--continue` (and a `--resume` choice) of a
  session a live host serves attaches to that host directly: nothing is
  spawned, and no note is printed unless the command line gave flags a new
  session would take (`craze: that session is already running (pid N);
  attached to it (ignored: --model)`). [`craze attach`](#craze-attach)
  reaches the same host.
- **`/exit` ends the session, everywhere.** `/exit`, <kbd>Ctrl+D</kbd> and
  the second <kbd>Ctrl+C</kbd> ask the host to stop the session
  (`session.stop`): the agent is closed, the session stays resumable if it
  has an index row (it has one once it was prompted), and every other client
  attached sees it end. The TUI waits at most two
  seconds for that, in total; a second quit ends the wait at once. A host
  that cannot stop its session (a TUI-hosted one, or one from an older
  craze) is detached from instead, with `craze: that session runs in an
  older craze; close it there`; any other failure prints `craze: the session
  may still be running: <reason>`. Exit 0 either way — except after a failed
  start, which exits 1.
- **An unattended host does not run for ever.** A host with no client
  attached and nothing in flight exits after
  [`host_idle_exit`](configuration.md#host-idle-exit) (an hour by default),
  saving the session like any stop. A host whose session was never prompted
  has its idle limit capped at five minutes (after the startup grace, and
  only while nothing keeps it busy); one whose start failed, as soon as its
  launcher has been told.
- **A quit before the session is up leaves nothing behind.** A host this
  launch spawned whose session never came up in the TUI (quit while starting,
  a failed start) is stopped; a host that was already running is never
  stopped that way.
- **A host that could not come up is a start failure.** The message names the
  host's log (`~/.cache/craze/host-logs/<hostId>.log`, see
  [Host logs](configuration.md#host-logs)) and ends `; CRAZE_DETACH=0 runs
  sessions inside craze instead`; craze exits 1. A host that *refused* the
  session asked for (no such session, a held one that cannot be attached)
  shows the refusal as the in-process path does, from a picker as its error
  row.

The switches that turn this off (`detach = false`, `CRAZE_DETACH=0`, or the
control socket switched off) run the session inside the TUI's own process, as
before: closing the terminal ends it, and nothing outlives it. See
[Configuration](configuration.md#detached-hosts).

### The control socket

Every host binds a control socket in the runtime namespace and serves its
session over it: the detached host [`craze serve`](#craze-serve) by default,
or the TUI's own process when [detaching is off](configuration.md#detached-hosts)
(see
[Protocol reference](protocol.md#reaching-a-host)), which is what
[`craze bridge`](#craze-bridge) and [`craze attach`](#craze-attach) reach.
A launching `craze` binds nothing of its own; the in-process path binds only
for a run that actually starts the TUI — a `--continue` that attaches instead
(above) or refuses binds nothing and creates no socket of its own.

`control_socket = false` in `config.toml`, or `CRAZE_CONTROL_SOCKET=0` (or
`false`) for one run, turns it off; see
[Configuration](configuration.md#the-control-socket). The session lock above
is still taken either way — it does not depend on the socket.

## craze serve

```bash
craze serve [flags]
craze serve --load <craze session id | provider:session id> [flags]
```

Hosts one session with no terminal, over its control socket, until a client
ends it (`session.stop`), the process gets SIGTERM or SIGINT, or [it has been
idle](configuration.md#host-idle-exit) for too long. It builds the session
exactly as the TUI does: the engine, the claim on the session, the journal
and the session-index writes are the host's, and [`craze
attach`](#craze-attach) (or the ordinary `craze`, which is a client of it)
joins it. **You rarely run it yourself:** the ordinary `craze` spawns one per
session (its own session, stdio on `/dev/null`) and attaches to it. Run by hand
it is the same host in the foreground, logging to stderr; SIGHUP is ignored.

It takes the session flags the TUI takes — `--workspace`, `--model`,
`--effort`, `--fast`/`--no-fast`, `--agent-bin`, `--provider`,
`--force`/`--no-force`, `--ask`/`--plan`, `--plugin-dir`, `--continue`/`-c` —
with the same refusals in the same words, and:

| Flag | Description |
|------|-------------|
| `--load` | Load a session by id instead of starting one: a craze session id, or `<provider>:<session id>` for an index row that has no craze id yet. It runs in the row's own workspace: an explicit `--workspace` naming another directory is exit 2, a row whose directory is gone is exit 1, and no such row is exit 1. `--provider` filters as it does for `-c` |
| `--log` | Write the host's diagnostics, its crash output and the agent's stderr to this file, rotated once at 4 MiB (default: stderr) |

`--host-id` and `--no-host-status` also exist, hidden: the spawner passes them
(the id it minted for the host, and the launching TUI's host-status choice).
They are internal, not a user interface.

A session another craze already holds is exit 1 — `craze serve: that session
is already running (pid N)`. A control socket switched off is a usage error
(exit 2): a headless host is reachable through nothing else. A session that
cannot be claimed (an unusable lock tree) is exit 1 with nothing left behind.
A host whose start failed (an auth failure, say) stays up, listed and
attachable, so its client is told why, and goes as soon as it has none.

The host's log and its retention are under
[Host logs](configuration.md#host-logs).

## craze prompt

Run a headless ACP turn (and optional follow-ups). Text comes from the
argument, or from stdin when it is not a tty.

```bash
./bin/craze prompt --json --provider grok --agent-bin ./bin/craze-fake-agent "hello"
echo "hello" | ./bin/craze prompt --json
./bin/craze prompt --json --follow-up "and then this" "start here"
```

| Flag | Description |
|------|-------------|
| `--workspace` | Existing workspace directory (default: current directory) |
| `--model` | Model to start on: an ACP model id (`session/set_model` after `session/new`), or a native model alias, resolved as the TUI's `--model` is, at the effort remembered for it. Never remembered. Without it a native run starts where a new TUI session would, on the remembered model ([Model memory](configuration.md#model-memory-recentjson)) |
| `--agent-bin` | Path to the agent binary (or `CRAZE_AGENT_BIN`, then `[agents]` in `config.toml`: [Agent binaries](configuration.md#agent-binaries)) |
| `--provider` | Provider: `cursor`, `grok`, `gx` (ACP agents) or `native` (runs inside craze) |
| `--follow-up` | Additional prompt on the same ACP session (repeatable) — the headless queue, see below |
| `--permission-decision` | Headless permission answer: `allow-once` or `reject-once` (repeatable) |
| `--force` | Spawn the agent with the provider's yolo flag (`--force` for Cursor, `--always-approve` for Grok). Default: on |
| `--no-force` | Disable yolo and handle permission requests |
| `--ask` | Set session mode to ask after `session/new` |
| `--plan` | Set session mode to plan after `session/new` |
| `--plugin-dir` | Extra plugin directory whose commands and skills craze expands (repeatable). Relative to the workspace; a missing directory is a diagnostic on stderr, not an error; ignored (with a diagnostic) on Grok |
| `--json` | Write only JSON events to stdout |

Without `--json`, only the **main session's** assistant text is written to
stdout — a sub-agent's text is never printed, so a piped reply stays the
agent's own answer.

### `--follow-up` is the queue

Every `--follow-up` is queued before the first turn and sent one per settled
turn, in order — the same queue the TUI's `Enter` fills. It holds 32 messages
of up to 32 KiB each; past either bound the run stops with an error rather
than sending a truncated message.

The chain still ends the way it always did: a stop reason that is not
`end_turn`, or an error, stops it with exit 1 and no further turn. `SIGINT`,
`SIGTERM` and `SIGHUP` (the hangup a closed terminal sends) clear the queue
first and then cancel, so nothing starts behind the signal; the run exits 1.

When the agent starts a turn craze did not prompt for — Grok's interject
fallback (see [Queued messages](tui.md#queued-messages)) — the next queued
message waits for it, up to a minute, and the run exits 1 with a note on
stderr if it is still going after that.

### JSON events

`--json` writes one JSON object per line:

| `type` | Carries |
|--------|---------|
| `text` | Assistant reply chunk; `agent` names the sub-agent when the chunk belongs to one |
| `thought` | Reasoning chunk; `agent` as on `text` |
| `user` | A chunk of a sub-agent's prompt (with `agent`), or a Grok interjection on the main session (with `"interjection":true`) |
| `command` | A plugin command or skill craze expanded into the prompt |
| `queue` | One change to craze's own message queue |
| `foreign_turn` | A turn the agent started without a craze prompt, bracketed |
| `compaction` | One end of a native session's compaction of its context; `agent` as on `text` |
| `tool` | Tool call create/update, merged by id; `agent` as on `text`; a sub-agent tool also carries `task` |
| `todos` | Todo list replace or merge |
| `permission` / `question` / `plan` | Blocking request, answered headless by `--permission-decision`; a permission with no decision left is rejected |
| `subagent` | Sub-agent lifecycle |
| `title` | The title the session took |
| `done` / `error` | Turn finished / turn failed |

Every line that came from the session's event stream carries `seq`, an integer,
as its **second** key — right after `type`. It is the number the session's
event log gave that event, the same one the session journal records for it, so
a headless caller can line the two up.

`seq` is increasing but **not** contiguous. The JSON above is a projection:
events it has nothing to say about (a mode change, a title-less metadata
update) are dropped, and a dropped event still took its number. Rely on the
order of two lines; never on the arithmetic between them.

The lines craze writes itself carry **no** `seq` at all, and craze never
invents one: a startup failure (an `error` line written before any session
existed) is the one left. The `queue` `removed` line for a row that left the
queue just as a signal landed used to be one too; it now carries a `seq` like
any other queue line, because taking a row and starting its turn are one
transaction in the engine (session control S1b). An absent `seq` means craze
said it, not the agent.

`agent` is the sub-agent's own session id (grok) and appears only on lines
that belong to one; main-session lines have no `agent` field. Grok streams a
sub-agent's session on the same connection, so its prompt, thoughts, tool
calls and replies arrive as tagged `user`, `thought`, `tool` and `text` lines.

`subagent` is the lifecycle,
`{"type":"subagent","event":"spawned|progress|finished","id":…}`, with
`attemptId`, `parentId`, `status`, `description`, `subagentType`, `model`,
`toolCallId`, `durationMs`, `toolCalls`, `turns`, `tokens`, `output` and
`error` alongside, each omitted when empty, plus `transcript` (whether the
provider streams one), which is always present — `true` or `false`. Grok's
lines come from its own lifecycle notifications; cursor
sends none, so craze synthesizes the same lines from the `cursor/task`
receipt. `tool.task` carries `description`, `model`, `agentId`, `durationMs`
and `status` (`running`, `completed`, `failed`, `cancelled`).

`command` is one plugin command or skill craze found on disk and expanded
into the prompt (see [Slash commands](tui.md#slash-commands)):
`{"type":"command","name":"watch-pr","qualified":"git-commands:watch-pr",
"plugin":"git-commands","kind":"command","path":"/home/…/watch-pr.md",
"text":"…"}`. `name` is the entry's own name and `qualified` its
`plugin:name` spelling — the same row under two names when the bare one was
free; `text` is the block exactly as it went out on the wire, the only way a
headless caller can see what the agent was actually given. Plain mode
prints none of this.

`queue` is `{"type":"queue","event":"queued|edited|removed|sent","id":"q-1",
"position":0,"version":0,"text":"…"}`. `position` is the row's index **when
the change happened**, not where it sits now — so a `sent` line from the drain
carries 0 (the head was position 0 when it went), every line of a clear
carries 0 for the same reason, and a `sent` line for the TUI's send now
carries whatever row the user picked. `--follow-up` produces a `queued` line
per follow-up before the first turn and a `sent` line before each turn after
it. Plain (non-`--json`) mode prints none of this.

`foreign_turn` is `{"type":"foreign_turn","event":"started|ended","id":"…",
"text":"…"}`; the id is Grok's `interject-fallback-…` prompt id. Nothing
drains between the two lines.

`compaction` is `{"type":"compaction","phase":"started|ended",
"reason":"auto|manual|overflow","tokensBefore":…,"tokensAfter":…,
"error":"…"}`, with `agent` for a sub-agent's: the native session summarizing
its conversation to free context, on its own near the model's window, after
a request the window refused, or for `/compact [focus]` sent as the prompt
(`manual`). A `started` line is followed by its `ended` one; the counts are
the context's estimated size before and after, `0` on a `started` line and
for the after of one that failed, which carries `error` instead. `agent` and
`error` are omitted when empty.

`done` is **not** EOF: it says the *turn* ended, not the sub-agents.
Lifecycle lines for a still-running sub-agent may follow the turn's `done` —
grok sends `subagent_finished` after `prompt_complete` when a turn is
cancelled — and a sub-agent spawned in one turn may report during the next.
Before every exit, `craze prompt` drains the stream: when no sub-agent was
ever spawned nothing is added; otherwise events keep being read and written
until every sub-agent is terminal, then 250 ms pass with no event, or 1.5 s
elapse — whichever comes first.

## craze bridge

```bash
./bin/craze bridge
./bin/craze bridge --session 01a0bbe5-69b4-79de-b75c-483a15b73d78
```

A pure byte pump for an SSH client (plan 027 §3.10): resolves the one
running craze session — or `--session`'s — dials its control socket, and
relays stdin/stdout to it verbatim. It speaks no protocol itself; `hello` is
the client's job, not this command's.

| Flag | Description |
|------|-------------|
| `--session` | A craze session id, a provider session id, or a host id (default: the one running session) |
| `--hub` | Relay to this machine's [hub](#the-hub) instead, starting it when none runs. Not with `--session` |

`--session`'s id is resolved against the registry under the bridge
process's own `$HOME` (`~/.cache/craze/hosts/*.json`; see
[Protocol reference](protocol.md#reaching-a-host)) — `HOME` wins over the
account database, so neither `CRAZE_HOME` nor `$XDG_RUNTIME_DIR` in an SSH
login's own environment can hide or misdirect it, but the SSH exec is
**assumed** to run with the same `HOME` as the tab that started the host:
true for an ordinary SSH login as the same user, not guaranteed in general.
With no `--session`, exactly one live host in total is the target; zero or
several is an error listing them on one line.

**The pump** (roost's bridge rules):

- stdout carries only bytes read off the socket, flushed as they arrive.
- stdin's EOF half-closes the socket's write side (`CloseWrite`) and keeps
  reading the socket — the session may still have plenty left to send.
- The socket's own EOF ends the pump and exits 0.

**The error contract: every failure is one line on stderr, exit 1.** That
covers every failure *before* the pump starts — resolving the target,
dialing, the peer check — and "nothing on stdout" is true only for those:
flag parsing is included, so a stray removed config-file environment
variable (see [Configuration](configuration.md#the-craze-directory)) in an
SSH environment still reads as a bridge error in this same shape. Once the
pump is running, stdout carries socket bytes only (see below); a failure
mid-stream still exits 1 with the same one stderr line, but after whatever
socket bytes were already relayed to stdout:

```text
craze bridge: no session running
craze bridge: no session <id>
craze bridge: 2 sessions running; pass --session <id>: <id> (grok, /a), <id> (cursor, /b)
craze bridge: session <id> is unreachable: <reason>
```

`craze bridge: no session <id>` is the contract line for an unknown
`--session`: a device reading it back knows the id it asked for is not (or no
longer) running here, not that something else broke.

**`--hub`** relays to [the hub](#the-hub) instead of one session's host,
starting it when none runs (found, like a host, under the bridge process's
own `$HOME`). The same pump and the same error contract apply; the client
says `hello` to the hub and then reads the [roster](protocol.md#the-hubs-roster)
or splices itself to a session with
[`session.connect`](protocol.md#the-hub-splice). Plain `craze bridge` is
unchanged. Its own failures:

```text
craze bridge: --hub and --session cannot be used together: the hub's session.connect names the session
craze bridge: no hub: <reason>
craze bridge: the hub is unreachable: <reason>
```

| Code | When |
|------|------|
| 0 | The socket closed cleanly (the session ended, or the far side hung up) |
| 1 | Every failure above: no session, an ambiguous `--session`, an unreachable socket, a read or write error. Never 255 (`ssh`'s own exit code for a failed connection) |
| 127 | Not `craze bridge`'s own exit: what an SSH client sees when the far end's shell falls off the published binary ladder (see [Protocol reference](protocol.md#ssh-exec-what-a-client-may-assume)) without ever reaching a `craze` to exec |

## craze attach

```bash
./bin/craze attach
./bin/craze attach --session 01a0bbe5-69b4-79de-b75c-483a15b73d78
```

Runs the full TUI over a running craze session's control socket (plan 027
§3.15): the session another `craze` process in this directory is hosting, or
`--session`'s. Nothing is spawned, nothing binds a socket, no session claim is
taken, and no index row is written. Quitting it (`/exit`, <kbd>Ctrl+D</kbd>,
<kbd>Ctrl+C</kbd> when idle) **ends the session** on a host that can stop it,
as it does in every client; only a closed terminal or SIGTERM leaves it
running (see [Quitting vs. the session ending](#quitting-vs-the-session-ending)).

| Flag | Description |
|------|-------------|
| `--session` | A craze session id, a provider session id, or a host id (default: the one session running in this directory) |
| `--theme` | TUI theme preset. See [Configuration](configuration.md) |
| `--no-mouse` | Disable mouse reporting (wheel scroll and clicks) |
| `--no-background` | Keep the terminal's own background and text colours instead of the theme's |

`--continue`/`-c`, `--resume`/`-r`, `--provider` and `--model` are refused —
an attach joins a running session rather than starting or loading one:

```text
craze attach: --continue does not apply: attach joins a running session
```

### Resolution

With an explicit `--session`, the one entry it names (matched the way
[`craze bridge`](#craze-bridge) matches, by craze session id, provider
session id or host id); an invalid `--session` — whatever its value, `""`
included — is a usage error, exit 2, before a registry read builds a path.
No match is exit 1:

```text
craze attach: no session <id>
```

and several matches is exit 2, listing them (ids do not collide in practice,
but nothing here assumes it).

With no `--session`, the live sessions whose workspace is the current
directory (both sides made absolute, cleaned and symlink-resolved, so
`/tmp/x` and macOS's `/private/tmp/x` are one directory): exactly one is the
target. Several is exit 2:

```text
craze: 2 running craze sessions in /abs/workspace:
  01a0bbe5…  /abs/workspace  My session
  01a0bbe6…  /abs/workspace
attach to one with: craze attach --session <id>
```

None in this directory is exit 1, naming the sessions running elsewhere (id,
workspace, and title when the session index has one) plus the same hint — or
just the first line alone when nothing runs anywhere:

```text
craze: no running craze session in /abs/workspace
  01a0bbe7…  /abs/other      Other session
attach to one with: craze attach --session <id>
```

A listing never connects to a session just to describe it: the workspace and
title come from the registry and the session index alone.

Resolution runs before the terminal is checked, so a usage error or an
unambiguous exit 1/2 above is reported whatever stdout is; only once a target
is resolved does attach refuse a non-tty the same way the TUI does (a real
termios check per OS, so `craze attach >/dev/null` is refused too):

```text
craze attach: refusing to start TUI on a non-tty
```

A resolved target that cannot be dialled — the socket is gone, or the peer
check fails — is exit 1:

```text
craze attach: session <id> is unreachable: <reason>
```

### Viewer mode

The pickers, provider persistence, session swaps, host status reporting
(the herdr pane or roost tab belongs to the host) and session-index writes
are all the host's; an attach does none of them. Shell mode (`!`) still runs
locally, in the session's own workspace. The permission chip shows the
host's permission mode (its `--force`/`--no-force`) when the host advertises
it, and falls back to craze's own default (yolo) for an older host that does
not.

### Keys

Identical to the host TUI's (plan 027 §3.19): the session — its queue and its
turn — is shared, so the first <kbd>Ctrl+C</kbd> while a turn works cancels
the turn and clears the queue for **every** attached client, exactly as it
does in the host TUI. <kbd>Ctrl+C</kbd> when idle, a second <kbd>Ctrl+C</kbd>,
<kbd>Ctrl+D</kbd> and `/exit` all end the session
([below](#quitting-vs-the-session-ending)).

### Quitting vs. the session ending

Quitting `craze attach` is the same act as quitting the host's own TUI (plan
030 §3.6): <kbd>Ctrl+C</kbd> when idle, a second <kbd>Ctrl+C</kbd>,
<kbd>Ctrl+D</kbd> and `/exit` ask the host to **stop the session**
(`session.stop`). It ends for every client attached and stays resumable if
it has an index row; the attach exits 0 and prints nothing more (1 after a
failed start). Two things do not end it, and are **view closes** — the attach
detaches, exits 0, and the session goes on running on its host: SIGTERM, and a
closed terminal (SIGHUP).

A host that cannot stop its session — a TUI-hosted one (the in-process
opt-out), or one from an older craze — is detached from instead, and one line
on stderr says so:

```text
craze: that session runs in an older craze; close it there
```

A stop that went unanswered (the host stopped reading, or the two-second
deadline passed) is exit 0 too, saying so:

```text
craze: the session may still be running: <reason>
```

An attach TUI whose session ended some other way — another client's stop, the
host's own quit, the host's idle exit — prints one line on stderr once its own
screen is restored, and exits 0:

```text
craze: session ended
```

A transport failure the client's redials could not recover from — the
session may still be running, but this client lost it — is exit 1 instead:

```text
craze: lost the session: <reason>
```

### `craze -c` of a running session

See [A session already running in another craze](#a-session-already-running-in-another-craze):
`--continue`/`-c` (and a `--resume` choice) of a session already running in
another `craze` attaches to it through this same command, rather than
refusing.

## craze ps

```bash
./bin/craze ps
./bin/craze ps --json
./bin/craze ps --no-hub
```

Lists every running craze session of this user on this machine — every live
host in the [registry](protocol.md#the-registry) under `$HOME` that has a
craze session id, whichever terminal or `CRAZE_HOME` started it — one row
each, from [the hub](#the-hub)'s roster:

```text
SESSION   STATE        PROVIDER  MODEL  DIR       SINCE  TITLE
0000a001  needs you    cursor    -      ~/proj-a  2m     fix the flaky test
0000c003  starting     cursor    -      ~         3h     add the ps command
0000e007  idle         native    -      ~/newer   5m     -
```

| Column | |
|--------|---|
| `SESSION` | The last eight characters of the session's craze id: its random end. A craze id is a UUIDv7, whose first characters are its clock's, the same for every session started within a minute or so. `--json`, `craze attach --session` and `craze bridge --session` take the whole id |
| `STATE` | The session list's state: `needs you` (an ask is open), `working`, `starting` (its host has not answered yet), `failed` (its start or its last turn failed), `idle`, or `unreachable` (listed, and its socket does not answer) |
| `PROVIDER` | The session's provider |
| `MODEL` | `-` for now: a session's row does not carry the model it runs |
| `DIR` | The session's working directory, `~` for `$HOME` |
| `SINCE` | How long the session has been in its state, in one unit (`42s`, `5m`, `3h`, `6d`); `-` when its host does not say (an older craze) |
| `TITLE` | The session's title, else its first prompt (the session index's title), else `-`: one line, cut with `…` to what is left of the terminal's width, or to 100 cells when stdout is not a terminal |

Rows are ordered by state, in the order above, then newest in its state
first. On a terminal no line is wider than it: when fewer than 10 cells are
left for the title, `DIR` gives up cells first (down to 10), its paths cut
from the left so their ends stay, and a terminal too narrow even for the
other columns has every line cut. With nothing running it prints `no
sessions running`.

| Flag | Description |
|------|-------------|
| `--json` | Print the hub's roster (its `sessions.list` result, [the protocol's](protocol.md#the-hubs-roster) roster rows) on one line instead of the table |
| `--no-hub` | Read each session's host directly, without the hub |

`--no-hub` — and any failure to have a hub (none could be started, or it did
not answer within 10 s) — reads each host itself, exactly as the hub would
(one round of the same poll; the same rows), and says so on stderr:

```text
craze ps: --no-hub: reading each session's host directly
craze ps: the hub is not available (<reason>); reading each session's host directly
```

With `--json`, that roster's `epoch` is `""` and its `cursor` 0: it is no
hub's. `craze ps` exits 0 unless that read fails too — the registry cannot be
read — which is exit 1 with the reason on one line.

### The hub

The hub is one process per user and craze directory (`CRAZE_HOME`), `craze
hub`: it serves the [roster](protocol.md#the-hubs-roster) of every running
session and a [splice](protocol.md#the-hub-splice) to any one of them, on a
socket of its own in the runtime namespace. **Nothing needs starting by
hand:** `craze ps`, [`craze new`](#craze-new) and `craze bridge --hub` start
it when none runs — re-executed, in a session of its own, its stdio on
`/dev/null` — and the next one finds it. A hub that does not answer is
replaced.

It **exits by itself 60 seconds after the last host has gone and the last
client has disconnected** — one nobody ever connected to included — and
while any session runs it stays. Killing it loses nothing: hosts run on
without it, and the next `craze ps` starts another.

Its log is `~/.cache/craze/host-logs/hub-<ns>.log`, beside the
[host logs](configuration.md#host-logs) (under the process's own `$HOME`;
`<ns>` names the craze directory), rotated once at 4 MiB like a host's.

## craze new

```bash
./bin/craze new "fix the flaky test"
./bin/craze new -C ~/projects/lumen --provider grok --effort high "add the ps command"
./bin/craze new --json
```

Starts a session in the background — in its own host, through the hub's
[`session.create`](protocol.md#sessioncreate) — in the current directory or
`-C`'s, and returns once the session has started, its first prompt (the
arguments, joined by spaces) sent to it when there is one:

```text
started 8327352e in ~/projects/lumen
```

The id is the session's short id, as [`craze ps`](#craze-ps) shows it. When
the prompt was sent and its answer lost, a second line says `the prompt may
not have reached it`: the session exists, and may be working on it. The
session runs on as any detached session does — `craze attach`, the session
list (`←`) and `craze ps` find it — until its host's idle exit.

| Flag | Description |
|------|-------------|
| `-C`, `--dir <dir>` | Start the session in this directory (default: the current one) |
| `--provider <id>` | The session's provider. Without it the hub's configured default — `provider` in `config.toml`, the one the last session to start persisted — and with none configured the create is refused |
| `--model <id>` | The model to start on |
| `--effort <value>` | The effort to start at, where the model offers one (as the [session flag](#flags)) |
| `--fast` / `--no-fast` | The fast setting to start with, where the model offers one |
| `--no-force` | The session's agent asks for permission, and a client answers (the default is a plain launch's: `--force`'s bypass — `config.toml` has no permission setting) |
| `--json` | Print the hub's answer (`session.create`'s result: the session as the roster lists it, and the prompt's outcome) on one line |

The session's host finds its agent binary as a session the list starts for
another provider does — `[agents]` in `config.toml`, then `PATH`: a hub never
hands a host the launch's `--agent-bin` or `CRAZE_AGENT_BIN`. It persists its
provider as the next plain launch's default, as every new session does.

A refusal is the hub's own words on one line, exit 1 — a session whose start
failed (`the session did not start: …`, its agent's first error line), a
directory that does not exist, no provider. So is a session that started and
refused its first prompt: it runs on, idle, and the line says why. A hub from
before `craze new` says `this hub (craze <v>) cannot create sessions; it exits
when idle`: it is not replaced, and the next `craze new` after it has gone
starts this craze's. Each run creates under a request id of its own, and a
connection to the hub that ends before the answer (the hub restarted
mid-create) is tried once more under the same id, so the new hub answers the
session the first one started rather than start a second.

On macOS a session the hub creates runs in the hub's security session: a hub
first started over `ssh` cannot start a `cursor` session (its keychain is
locked there), and says so as a start that failed.

## craze auth

```bash
craze auth login [provider]
craze auth logout <provider>
craze auth list
```

Manages the API keys of the [native provider](configuration.md#native-models-and-providers)'s
model providers. A key is stored as its provider's `api_key` in
`native/providers.toml` in the craze directory (`~/.craze`, or `$CRAZE_HOME`),
written at `0600`; a provider's environment variable, when it holds a usable
key, is used before the stored one. **No key is checked with its provider**
when it is stored — a wrong one shows on first use — and none of the three
commands makes a network request. What they do to the file is under
[Keys](configuration.md#keys).

A provider is named by its id or its display name, in any case: `fireworks`,
`Fireworks`, `"z.ai coding plan"`. The providers are the
[catalog's](configuration.md#the-shipped-catalog) and any of your own in
`providers.toml` (a directory whose `models.toml` says
[`catalog = false`](configuration.md#isolated-setups-catalog-false) has only
its own). All three exit 1 when there is no craze directory (neither `HOME`
nor `CRAZE_HOME` is set). An argument too many, or a flag a command does not
take, is exit 2, and what was typed is not repeated back: it may be a key.

### craze auth login

Stores a provider's key. On a terminal, craze asks for it with a prompt that
does not echo (`Fireworks API key: `) — after a numbered list of the providers
to pick from when none is named, the connected ones marked `(connected)`. The
echo is off from before the first prompt is drawn until the key is read, so
nothing typed at either prompt shows, however quickly it comes: at the list,
only a number on it is written back, so a key pasted there by mistake is never
displayed. Ctrl-C at either prompt leaves the terminal's echo on. When stdin
is not a terminal, the key is stdin's first line (at most 8 KiB), so a script
can pipe it in. Surrounding whitespace is trimmed.

```bash
craze auth login fireworks        # prompts; the key is not shown as you type
craze auth login                  # pick from the list, then the prompt
printf '%s\n' "$FIREWORKS_KEY" | craze auth login fireworks
```

```text
Saved the Fireworks key in /home/you/.craze/native/providers.toml.
FIREWORKS_API_KEY is set in this environment; craze uses it before the stored key.
```

The second line only when that variable is set. Nothing is saved, and craze
exits 1, when the key is empty, shorter than 8 bytes, or overlaps craze's
redaction marker — the error names the rule, never the key. No provider named
without a terminal, or a name that matches none, is exit 2; the name is not
repeated back, in case what was typed there was the key. Another provider's
stored key that cannot be used is kept as it is and named in a note on
stderr, so it can be replaced or removed next.

### craze auth logout

Removes a provider's stored key, and only the key: whatever else its entry
sets stays. Exit 0 whether or not there was one.

```text
Removed the stored Fireworks key.
Fireworks is still connected through FIREWORKS_API_KEY.
```

With nothing stored the first line is `No stored Fireworks key.`; the second
is there only while a variable still funds the provider. No provider named is
exit 2.

### craze auth list

One row per provider, by display name: its name, its id, and how it is
connected — `env <VAR>`, `stored key` or `not connected`, in the order a
session tries them.

```text
Fireworks         fireworks        env FIREWORKS_API_KEY
Meta              meta             stored key
OpenRouter        openrouter       not connected
Z.AI Coding Plan  zai-coding-plan  not connected
```

Then notes, on stderr, each starting `note: `: a stored key that cannot be
used, an entry of yours that [repeats what craze
ships](configuration.md#how-the-files-merge) (delete it to follow craze's
updates), the warnings a native session prints when it starts, and — when the
model table does not load, a broken `models.toml` say — why. The rows need
only `providers.toml` and the catalog, so they are listed either way. No key,
nor any part of one, is ever printed.

## craze version

Print the craze version and exit.

```bash
./bin/craze version
```

## Exit codes

| Code | When |
|------|------|
| 0 | Session started; quitting after a usable session, including an error *during* the session; also `Esc` on the `--resume` picker (no session was ever started) |
| 1 | Session never started (no login, agent would not come up, or a load the agent refused), a runtime failure, or nothing to `--continue`/`--resume` — no matching session, or a session index craze could not read |
| 2 | Usage error (bad flags, missing prompt text, non-tty TUI, or `--continue` with `--resume`). The non-tty refusal is checked before `--continue`/`--resume` are considered |

`craze frame` uses 2 for a bad key script and 3 for a wait timeout; that
command is hidden. See [Testing](../development/testing.md#craze-frame).
