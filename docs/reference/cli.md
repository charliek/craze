# CLI Reference

```bash
craze [flags]
craze prompt [text] [flags]
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
| `--provider` | ACP provider: `cursor` or `grok`. Empty is unset. Unknown id exits 2 |
| `--force` | Spawn the agent with `--force` / `--always-approve` (yolo). Default: on |
| `--no-force` | Disable yolo and handle permission requests |
| `--no-mouse` | Disable mouse reporting (wheel scroll and clicks) |
| `--theme` | TUI theme preset. See [Configuration](configuration.md) |
| `--ask` | Set session mode to ask after `session/new` |
| `--plan` | Set session mode to plan after `session/new` |
| `--plugin-dir` | Extra plugin directory whose commands and skills craze expands (repeatable). Relative to the workspace; a missing directory is a diagnostic on stderr, not an error; ignored (with a diagnostic) on Grok |

`--ask` and `--plan` are mutually exclusive.

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
```

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
| `--provider` | ACP provider: `cursor` or `grok` |
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
`end_turn`, or an error, stops it with exit 1 and no further turn. `SIGINT`
and `SIGTERM` clear the queue first and then cancel, so nothing starts behind
the signal; the run exits 1.

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

## craze version

Print the craze version and exit.

```bash
./bin/craze version
```

## Exit codes

| Code | When |
|------|------|
| 0 | Session started; quitting after a usable session, including an error *during* the session |
| 1 | Session never started (no login, agent would not come up), or a runtime failure |
| 2 | Usage error (bad flags, missing prompt text, non-tty TUI) |

`craze frame` uses 2 for a bad key script and 3 for a wait timeout; that
command is hidden. See [Testing](../development/testing.md#craze-frame).
