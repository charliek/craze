# Configuration

craze persists the theme and the last successfully started **provider** in
`~/.craze/config.toml`, and, once a session has a prompt or a title, a row
about it in the session index (`~/.craze/sessions.jsonl`) that
`--continue`, `--resume` and `/rename` use — see [Session
index](#session-index). Every session also writes a [journal](#session-journal)
of what happened in it, which can contain secrets and can be turned off.
Session flags (`--workspace`, `--model`, `--force`, `--ask` / `--plan`,
`--agent-bin`) are per-invocation.

## Theme precedence

An explicit `--theme` beats the config file, which beats `craze-dark`.

| Source | When it wins |
|--------|----------------|
| `--theme <name>` | Flag was passed (including `--theme ""`) |
| `~/.craze/config.toml` | No `--theme` on the command line, and the file names a theme |
| `craze-dark` | Neither of the above |

## Presets

Seven presets. While craze runs it sets your terminal's own default
background and text colours to the theme's and restores them when it exits;
`background = false` or `--no-background` leaves them alone — see [Terminal
colours](#terminal-colours).

| Name | Notes |
|------|-------|
| `craze-dark` | Default |
| `craze-light` | |
| `tokyo-night` | Aliases: `tokyonight`, `tokyo` |
| `dark` | |
| `light` | |
| `catppuccin` | |
| `gruvbox` | |

`craze` and `crazedark` are aliases for `craze-dark`; `crazelight` is an alias
for `craze-light`.

`Ctrl+G` or `/theme` opens a names-only picker that repaints the live screen as
the cursor moves; `Enter` keeps the theme and saves it, `Esc` puts back the one
that was active when the picker opened. `/theme <name>` sets and saves one
directly.

## The craze directory

craze keeps its own files in one directory, `~/.craze` by default, under
fixed names:

| File | Holds |
|------|-------|
| `config.toml` | The saved theme, provider, and other settings — see [Config file](#config-file) |
| `sessions.jsonl` | The [session index](#session-index) |
| `journal/` | One directory per workspace of [session journals](#session-journal) |

`CRAZE_HOME` moves the whole directory. With `CRAZE_HOME=/some/dir`, craze
reads and writes `/some/dir/config.toml`, `/some/dir/sessions.jsonl` and
`/some/dir/journal/` and nothing under `~/.craze`. A relative value stays
relative to the working directory craze runs in, and a leading `~` or `~/`
means your home directory. No variable moves a single file on its own.

`CRAZE_HOME` moves only craze's own directory. The user-level skills and the
plugin caches craze scans (see [Slash commands](tui.md#slash-commands)) are
still found under `HOME`, so a test or a container that wants those isolated
as well sets `HOME` too.

Earlier builds read `CRAZE_CONFIG`, which named the config *file*. It has
been removed, not kept as an alias: while it is still set, `craze`,
`craze prompt`, and `craze frame` exit with status 2 and a message that names
it, before reading or writing anything. Unset it and set `CRAZE_HOME` to the
directory the file was in — `CRAZE_HOME=/some/dir` for `/some/dir/config.toml`.
A config file with any other name has to be renamed `config.toml`.

## Config file

The saved theme lives in `config.toml` in the [craze
directory](#the-craze-directory), `~/.craze/config.toml` by default.

```toml
theme = "gruvbox"
provider = "grok"
```

Unrelated keys in that file are preserved. A file craze cannot parse is never
clobbered, and the save that refused to overwrite it is reported in the
transcript. A malformed file does not prevent startup; the theme falls back to
the default.

`craze frame` never reads this file, and never reads `$CRAZE_PROVIDER`. See
[Testing](../development/testing.md#craze-frame).

## Session index

`~/.craze/sessions.jsonl` is the on-disk index behind `--continue`,
`--resume` and `/rename` (see [CLI](cli.md#-continue-and-resume) and
[TUI](tui.md#resuming-a-session)). It is **always the sibling of the config
file** in the [craze directory](#the-craze-directory), so `CRAZE_HOME` carries
the index along with the config, and `craze frame`, which isolates `HOME` and
unsets `CRAZE_HOME` for its run, isolates it too. There is no separate
environment variable for it.

One JSON object per line:

```json
{"sessionId":"…","provider":"grok","cwd":"/abs/workspace","title":"fix flaky pty test","pinned":false,"createdAt":"2026-09-15T22:04:11.5Z","updatedAt":"2026-09-16T00:12:03.2Z"}
```

- `cwd` is matched by exact string against the workspace's absolute path —
  no symlink resolution — so a session started from one path is not offered
  under another that resolves to the same directory, cursor included even
  though its own `session/load` is permissive across directories.
- `title` is the session's stored name: the agent's own title, the first
  line of the first prompt as a fallback, or a `/rename`. `pinned` marks a
  `/rename`, which always wins over a later agent title.
- A row exists only once there is something to show it for: craze creates
  it on the session's first prompt or its first agent-supplied title,
  whichever comes first — never merely on start — so a session started and
  quit without a prompt leaves nothing behind.
- The index is capped at 500 rows; a write past the cap drops the oldest
  rows by `updatedAt`.
- An unknown provider id already in the file (written by a newer craze) is
  kept on disk but never offered by `--continue` or `--resume`.
- `craze prompt` never writes to the index — headless sessions are not
  resumable.

**A `--continue` or `--resume` never changes the persisted default
provider.** Loading yesterday's grok thread is not a decision about
tomorrow's default: `provider` in `config.toml` is only touched when
`--provider` was passed explicitly alongside `--continue`/`--resume`, the
same as an ordinary run.

## Session journal

Every session craze starts writes a journal of what happened in it, one JSON
object per line, under the [craze directory](#the-craze-directory):

```
~/.craze/journal/--home-me-src-app--/20260919T225929Z_01a0bbe5-69b4-79de-b75c-483a15b73d78.jsonl
```

The directory below `journal/` is the workspace's own path with its separators
turned into dashes, wrapped in `--…--` — the same name the native harness gives
that workspace's session store, so the two pair by eye. The file name is the
UTC time the session was built and the session's own id, which the first line
of the file repeats.

**One file per session, and no process ever appends to a file another process
wrote.** A `craze prompt` run leaves one; a TUI run leaves one per session it
starts. `--continue` and `--resume` start a *new* session, so they get a new
file, into which the agent's replay of the earlier conversation is written
again, flagged as replay; the earlier file is never touched. The file is
created with the first line worth writing, so a craze that is opened and quit
without ever starting a session leaves nothing behind. `craze frame` never
journals.

| Line | Holds |
|------|-------|
| `header` | Format and codec versions, the session id, the craze version, OS and architecture, the provider, the agent binary asked for, the workspace, `force`, `interactive`, `mode`, and the pid |
| `session` | The provider's own session id, the id it was loaded from on a resume, and the binary craze actually spawned |
| `event` | One line per event the session emitted, with its `seq` and the whole event — the same numbers `craze prompt --json` prints (see [CLI](cli.md#json-events)) |
| `prompt` / `prompt_end` | Your prompt or interjection **as typed**, before craze expanded any slash command into it; then how that turn ended — stop reason, or error class and message, and its duration |
| `diag` | Everything that is not transcript: the agent's own stderr, a start that failed, an event too large to record, a reader craze dropped, and the line that closes the file |
| `gap` | Written when the journal could not keep up: exactly what was lost, so a reader is never quietly short of events |

A journal never slows a turn down and never fails a session. If it falls
behind, it records what it lost as a `gap` rather than skipping it quietly; if
a write fails outright — a full disk — journaling stops for that session and
craze says so once on stderr. The session itself carries on either way.

### Journals can contain secrets

Prompts and tool output are written **verbatim, with no redaction**. Whatever
you type and whatever a tool prints — an API key in a shell command, the
contents of a file the agent read, a token in an error message — is on disk in
plain text. That is the point of the record, and it is why the files are
`0600` in `0700` directories, both *no broader than*: a restrictive umask
narrows them further, and a directory that already exists is used as it is,
never tightened. Treat a journal as being as sensitive as the session it came
from.

craze never uploads a journal, or any part of one, anywhere. They are local
files. A running session reads its own journal back — that is how a client
that reconnects is given the events it missed — but craze does not read a file
from an earlier run: once a session ends, its journal is only there for you.

craze does not prune them yet either: they accumulate until you delete them. As
a sense of scale, a measured turn with one tool call cost 6 KB against cursor
and about 50 KB against grok, which streams its reply token by token; a
fifty-second cursor turn that read dozens of files cost 224 KB.

### Turning it off

`journal = false` in `config.toml`, or `CRAZE_JOURNAL=0` (or `false`) for one
run, and craze writes no journal and creates no `journal` directory. Either
switch turns it off on its own, and neither can turn it on against the other:
`CRAZE_JOURNAL=1` does not defeat `journal = false`.

**The opt-out fails closed**, unlike every other switch on this page. A
journal holds prompts and tool output, so anything craze cannot read as a
plain `true` means *off*, not on: a `config.toml` it cannot read or parse, a
`journal` key that is not a bool (`journal = "false"` is a string), and a
`CRAZE_JOURNAL` it cannot read as a bool. Each of those prints one line saying
so; an explicit `false` is your own choice and is silent. Journaling is on only
when the config parsed and `journal` is absent or exactly `true`.

## The control socket

Every craze TUI process binds a Unix-domain control socket and serves the
session it runs over it — what [`craze bridge`](cli.md#craze-bridge) and a
future `craze attach` dial into. See the
[protocol reference](protocol.md#reaching-a-host) for the runtime namespace,
the registry, and where the socket itself lives; the registry and its locks
sit under your real `$HOME`, not `CRAZE_HOME` — an SSH login shares the one,
whatever the tab that started the host had set for the other. Binding
happens only once a run is actually starting a TUI: a `--continue`
[refused by SQ16](cli.md#a-session-already-open-in-another-craze) binds
nothing.

`control_socket = false` in `config.toml`, or `CRAZE_CONTROL_SOCKET=0` (or
`false`) for one run, turns it off; a craze with no socket serves nothing for
another process to reach, though the session's own SQ16 lock is still taken
either way — it does not depend on the socket.

**The opt-out fails closed**, [the journal's](#session-journal) own rule and
for the same reason: this is an access switch, so anything craze cannot read
as a plain `true` means *off*. `CRAZE_CONTROL_SOCKET`, when set, decides
**alone**: an unreadable-as-bool value turns the socket off with its own one
line on stderr, and `config.toml` is then not even read, so at most that one
line prints. Only when `CRAZE_CONTROL_SOCKET` is unset (or reads as true)
does `config.toml` get read at all, and there the same rule applies — a
`config.toml` it cannot read or parse, or a `control_socket` key that is not
a bool, prints its own one line. An explicit `false`, from either switch, is
your own choice and is silent. The socket is served only when the config
parsed and `control_socket` is absent or exactly `true`, *and*
`CRAZE_CONTROL_SOCKET` (when set) reads as true — either switch turning it
off is final; neither can turn it back on against the other.

A failure to *bind* at all — the runtime directory is unusable, or the
socket's own path would be too long for `sun_path` — is not the opt-out and
is not a privacy switch: it prints one line, `craze: control socket off:
<why>`, embedding the underlying error's own text as is — in the ordinary
case that is one stderr line, but nothing here promises the error text
itself is free of a line break (a runtime path containing one, say). The run
carries on exactly as it would with the opt-out set.
`CRAZE_RUNTIME_DIR` (below) is the fix when the reason is length.

## Terminal tab title

`terminal_title = false` in `config.toml` turns off every terminal tab-title
write (see [Tab title](tui.md#tab-title)); the default — including when the
key is absent or the file cannot be parsed — is `true`. `craze prompt` never
sets a tab title, and `craze frame` has no terminal to write one to.

## Host status

Inside a herdr pane or a roost tab, craze reports its own state to that host,
which then tracks craze like any other agent. That is all craze sends: its
state, a one-line reason (a card's header or an error's first line), and the
provider and model, never conversation content. A `--continue` or `--resume`
replay is not reported — the host first hears from craze when the restored
session is ready. The end of a turn reaches the host a quarter of a second after
the turn ends, so a message that drains from the queue the moment a turn ends
never shows as a finished turn in between.

Every exit craze can see — `/exit`, `Ctrl+D`, `Ctrl+C`, `SIGTERM`, and `SIGHUP`,
the terminal hangup a closed tab or window sends — releases the pane or tab
before the agent is shut down, waiting at most a second for the host to take
it, so an agent slow to exit cannot hold it. `SIGKILL` gives craze no
chance to, and leaves its last state in place until the pane or tab closes.

After the TUI exits, craze prints its own lines on stderr: a socket it could
not reach costs one `host status: herdr: …` or `host status: roost: …` line.
It prints the agent's own diagnostics too, but only when the run failed —
the session never started, the TUI run itself failed (a recovered panic
included), or the agent exited on its own. On a clean exit craze prints
nothing but its own lines.

### herdr

The pane's sidebar, `herdr agent list` and `herdr agent wait` see an agent named
`craze`: `idle` once the session is up and after each turn, `working` while a
turn runs, and `blocked` while a permission, question or plan card is waiting
on you — or when the session fails to start, or a turn ends in an error (until
the next send). A blocked state carries the one-line reason. The provider and
model go along as the pane's `provider` and `model` metadata tokens, which
craze clears when it releases the pane.

craze reports only when herdr's own variables say it is in a herdr pane:
`HERDR_ENV=1`, with `HERDR_SOCKET_PATH` and `HERDR_PANE_ID` both set.

If craze runs in a herdr pane but herdr never shows it, run
`herdr pane get <pane>`: an `agent_session` from another source (such as
`herdr:cursor`) means an agent hook tied the pane to its own session, and
herdr ignores craze's reports while it stands. A fresh pane has none.

### roost

craze claims the tab under the source `craze` when its agent session starts,
and the tab shows it the way it shows any agent: no agent state while the
session sits ready, `running` while a turn runs, `needs input` with a
notification while a permission, question or plan card is waiting on you, and
`failed` with a notification when a turn ends in an error (until the next
send); both notifications carry the one-line reason. A turn that ends normally
leaves the tab idle with a "Turn complete" notification; a turn you cancel
leaves it idle with none. `roostctl wait --state` sees the same states, a failed
turn as `needs_input`. The claim carries the `model`, `craze.provider` and
`version` metadata keys. A session that never starts has no session id to claim
the tab with, so roost hears nothing about it.

craze reports only when roost's own variables say it is in a roost tab:
`ROOST_SOCKET` set, and `ROOST_TAB_ID` a positive integer with nothing around
it — the same rule roost's own hooks apply.

`roostctl tab set-state` on craze's tab wins. roost drops every report craze
sends for that session from then on, craze never claims the tab back for it,
and one `host status: roost: …` line on stderr after exit names the tab's
owner. craze's next session — a new one in the same run, or the next run —
claims the tab again. `set-state none` is different: it leaves the tab with no
owner at all, which craze cannot tell from a restarted roost, so craze claims
the tab back when it next reports after finding it unowned.

### The agent's environment

**The agent loses each host's hook gate.** While craze reports to a host, the
agent it spawns (cursor-agent, grok or gx) gets craze's environment without the
one variable that host's agent integrations gate on: `HERDR_ENV` for herdr,
`ROOST_AGENT_HOOK` for roost. Otherwise a herdr or roost cursor or grok hook
installed on this machine would fire inside craze's child and take the pane or
tab from craze: herdr's would tie the pane to the agent's own session, and
herdr would silently ignore every report craze sends for the rest of the
session; roost's would claim the tab as `cursor` or `grok`. Everything else
stays — `HERDR_SOCKET_PATH`, `HERDR_PANE_ID` and `HERDR_BIN_PATH`, and
`ROOST_TAB_ID` and `ROOST_SOCKET` — so the `herdr` CLI and `roostctl` still
work from inside the agent. The cost is that herdr's agent skill, run inside
the child, believes it is not inside herdr, and roost's own agent hooks do
nothing inside the child.

`host_status = false` in `config.toml`, or `--no-host-status` on the command
line, turns reporting to both hosts off and leaves the agent's environment
whole, so the agent's own herdr and roost hooks work again. The default —
including when the key is absent or the file cannot be parsed — is `true`;
outside a herdr pane or roost tab there is nothing to report to either way.
`craze prompt` and `craze frame` never report.

## Terminal colours

The theme is more than craze's own cells: while craze runs it sets the
terminal's default background and text colours to the preset's (`OSC 11` and
`OSC 10`) and hands them back on exit (`OSC 111` and `OSC 110`), so the
picker's live preview moves the background too. The terminal's *configured*
defaults come back, not whatever colours happened to be set before craze
started.

`background = false` in `config.toml`, or `--no-background` on the command
line, turns that off: craze then draws on your terminal's own background and
writes none of those four sequences. The default — including when the key is
absent or the file cannot be parsed — is `true`, and only a literal `false`
disables it; there is no flag to force it back on. It is also off
automatically whenever craze is painting no colour at all (`NO_COLOR`,
`TERM=dumb`). A terminal that ignores `OSC 11` simply keeps its own
background; a hard kill (`SIGKILL`) leaves the theme's colours set until the
terminal is reset, the same as the tab title.

## Provider precedence

Ids are `cursor`, `grok`, and `gx`. An empty string (`--provider ""`,
`$CRAZE_PROVIDER=""`, `provider = ""`) is unset, not unknown.

`gx` is [`charliek/grok-build`](https://github.com/charliek/grok-build), a
third-party fork of the Grok CLI that speaks the same ACP dialect as
`grok` — same wire protocol, same auth, same skills and plugins — so
everything on this page and in the rest of the docs about Grok's behaviour
applies to it too. It currently shares grok's `~/.grok` home (config, auth,
sessions, skills); that is the fork's current behaviour, not a guarantee
craze makes.

gx is gated separately from the other two ids: it appears in the startup
picker only when a binary for it resolves (see [Environment](#environment)
below for lookup order); cursor and grok are always offered, so the picker
can never be empty. `--provider gx` works regardless of whether a binary
resolves, and fails at spawn — `agent binary not found: …` — if it does
not; this matches how `craze prompt --provider gx` and `craze frame
--provider gx` already behave, since neither consults the picker. Setting
`--agent-bin` or `$CRAZE_AGENT_BIN` also makes gx appear in the picker,
because with either set a gx session genuinely would spawn that binary; the
override is exclusive, so one pointing at nothing hides gx even when `gx`
is itself on `PATH`.

For the TUI and `craze prompt`, the first hit wins:

1. `--provider <id>` when the flag was passed (`Changed`).
2. `$CRAZE_PROVIDER` if non-empty.
3. `provider` in `~/.craze/config.toml` if it is a known id.
4. `cursor`.

Unknown `--provider` exits 2. Unknown env or config id warns and falls back to
cursor; the fallback is not written back. After a successful `Start`, the TUI
and `craze prompt` save the provider that actually started (an explicit picker
choice overwrites a stale unknown id; `Esc` on the fallback default does not).

`--agent-bin` / `$CRAZE_AGENT_BIN` override the **binary**. They do not select
the dialect.

## Environment

| Variable | Purpose |
|----------|---------|
| `CRAZE_HOME` | The [craze directory](#the-craze-directory) (default `~/.craze`): `config.toml` and the [session index](#session-index) live directly inside it. Skills and plugin caches still follow `HOME` |
| `CRAZE_AGENT_BIN` | Agent binary when `--agent-bin` is unset |
| `CRAZE_PROVIDER` | Provider id when `--provider` is unset (`cursor`, `grok`, or `gx`) |
| `CRAZE_JOURNAL` | Turns the [session journal](#session-journal) off for this run when it reads as false. It cannot turn one on against `journal = false`, and a value craze cannot read as a bool turns it off with one line saying so. Empty or unset leaves the decision to the config file |
| `CRAZE_CONTROL_SOCKET` | Turns [the control socket](#the-control-socket) off for this run when it reads as false. It cannot turn one on against `control_socket = false`, and a value craze cannot read as a bool turns it off with one line saying so. Empty or unset leaves the decision to the config file |
| `CRAZE_RUNTIME_DIR` | Overrides [the control socket's](#the-control-socket) runtime base (default: `$XDG_RUNTIME_DIR/craze`, then `/run/user/<uid>/craze` on Linux, then `/tmp/craze-<uid>`): an absolute path, short enough to leave room for `<ns>/<hostId>.sock` under `sun_path`'s limit. Meant for tests and unusual hosts; see [Protocol reference](protocol.md#reaching-a-host) |
| `XAI_API_KEY` | Grok API key; used when initialize advertises `xai.api_key` |
| `GROK_CODE_XAI_API_KEY` | Legacy alias for `XAI_API_KEY` |
| `HERDR_ENV`, `HERDR_SOCKET_PATH`, `HERDR_PANE_ID` | Read, never set. `HERDR_ENV=1` with the other two set means craze is in a herdr pane and reports [host status](#host-status) to it; `HERDR_ENV` is then removed from the agent's environment |
| `ROOST_SOCKET`, `ROOST_TAB_ID` | Read, never set. `ROOST_SOCKET` set with a positive integer `ROOST_TAB_ID` means craze is in a roost tab and reports [host status](#host-status) to it |
| `ROOST_AGENT_HOOK` | Never read, never set by craze; plays no part in detecting roost. While craze reports [host status](#host-status) to roost, it is removed from the agent child's environment, so roost's own agent hooks stay inert inside craze's agent |

If neither `--agent-bin` nor `CRAZE_AGENT_BIN` is set, Cursor looks for
`cursor-agent`, then `agent`, on `PATH`. Grok looks for `grok` only. gx looks
for `gx` only.
