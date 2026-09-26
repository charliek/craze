# CLI Reference

```bash
craze [flags]
craze prompt [text] [flags]
craze bridge [flags]
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
| `--model` | ACP model id |
| `--agent-bin` | Path to the agent binary (or `CRAZE_AGENT_BIN`) |
| `--provider` | ACP provider: `cursor`, `grok`, or `gx`. Empty is unset. Unknown id exits 2 |
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

`--ask` and `--plan` are mutually exclusive. So are `--continue` and
`--resume` (exit 2).

`--provider` on the TUI skips the startup picker. Without it, `$CRAZE_PROVIDER`
then `provider` in the config file then `cursor` is the default, and the picker
lets you change it before Start. `craze prompt` has no picker; it uses the same
precedence. `craze frame` ignores env and config and defaults to cursor unless
`--provider` is passed.

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

### A session already open in another craze

`--continue` claims the row it loads before anything is built: its own lock
under `~/.cache/craze/locks/`, independent of `CRAZE_HOME` and the runtime
directory (see [Protocol reference](protocol.md#reaching-a-host)). A session
another running `craze` already holds refuses instead of loading it:

```text
craze: that session is open in another craze (pid N)
```

exit 1, and no agent is spawned — nothing is built once the row's session is
found to be held. Two narrower refusals cover the claim mechanism itself,
not the session:

```text
craze: the session index is busy — try again
craze: the session index changed — try again
```

The first is another writer holding the session index past a two-second
bound; the second is a legacy row with no durable id that left the index
between being read and being given one (another loader may already hold it
under an id this one does not know about — trying again re-reads the index).

`--resume`'s picker claims each row the same way, when you press <kbd>Enter</kbd>
on it: a refused row shows an error naming the holder's pid in place of
loading it, and the picker keeps running.

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
new native session — it has nothing to spawn — exit 2, before the index is
touched or anything claimed:

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
above](#a-session-already-open-in-another-craze): a transcript another craze
process already has open refuses the load the same way. A transcript left
with a torn or cut-off tail by a crash is trimmed back to its last complete
step on open, and the dropped bytes are kept beside it in a
`<transcript>.torn-<UTC stamp>` file rather than discarded; a tail written by
a newer craze build — one this version does not recognize — is never
trimmed, and the load is refused instead, so nothing unrecognized is thrown
away.

### The control socket

Every craze TUI process binds a control socket in the runtime namespace and
serves the session it runs over it (see
[Protocol reference](protocol.md#reaching-a-host)), which is what
[`craze bridge`](#craze-bridge) and a future `craze attach` reach. Binding
happens only for a run that actually starts the TUI — a `--continue` SQ16
refuses above binds nothing and creates no socket.

`control_socket = false` in `config.toml`, or `CRAZE_CONTROL_SOCKET=0` (or
`false`) for one run, turns it off; see
[Configuration](configuration.md#the-control-socket). The session lock above
is still taken either way — it does not depend on the socket.

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
| `--model` | ACP model id (`session/set_model` after `session/new`) |
| `--agent-bin` | Path to the agent binary (or `CRAZE_AGENT_BIN`) |
| `--provider` | ACP provider: `cursor`, `grok`, or `gx` |
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

| Code | When |
|------|------|
| 0 | The socket closed cleanly (the session ended, or the far side hung up) |
| 1 | Every failure above: no session, an ambiguous `--session`, an unreachable socket, a read or write error. Never 255 (`ssh`'s own exit code for a failed connection) |
| 127 | Not `craze bridge`'s own exit: what an SSH client sees when the far end's shell falls off the published binary ladder (see [Protocol reference](protocol.md#ssh-exec-what-a-client-may-assume)) without ever reaching a `craze` to exec |

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
