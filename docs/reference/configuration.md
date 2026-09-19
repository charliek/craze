# Configuration

craze persists the theme and the last successfully started **provider** in
`~/.craze/config.toml`, and, once a session has a prompt or a title, a row
about it in the session index (`~/.craze/sessions.jsonl`) that
`--continue`, `--resume` and `/rename` use — see [Session
index](#session-index). Session flags (`--workspace`, `--model`, `--force`,
`--ask` / `--plan`, `--agent-bin`) are per-invocation.

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

## Config file

The saved theme lives in `~/.craze/config.toml`. `CRAZE_CONFIG` overrides the
whole path.

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
file** — the same directory, `sessions.jsonl` next to `config.toml` — so a
`CRAZE_CONFIG` pointing somewhere else carries the index along with it, and
`craze frame`'s isolated `HOME` isolates it too. There is no separate
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
| `CRAZE_CONFIG` | Replace the config path (`~/.craze/config.toml`); the [session index](#session-index) always follows it, as its sibling |
| `CRAZE_AGENT_BIN` | Agent binary when `--agent-bin` is unset |
| `CRAZE_PROVIDER` | Provider id when `--provider` is unset (`cursor`, `grok`, or `gx`) |
| `XAI_API_KEY` | Grok API key; used when initialize advertises `xai.api_key` |
| `GROK_CODE_XAI_API_KEY` | Legacy alias for `XAI_API_KEY` |
| `HERDR_ENV`, `HERDR_SOCKET_PATH`, `HERDR_PANE_ID` | Read, never set. `HERDR_ENV=1` with the other two set means craze is in a herdr pane and reports [host status](#host-status) to it; `HERDR_ENV` is then removed from the agent's environment |
| `ROOST_SOCKET`, `ROOST_TAB_ID` | Read, never set. `ROOST_SOCKET` set with a positive integer `ROOST_TAB_ID` means craze is in a roost tab and reports [host status](#host-status) to it |
| `ROOST_AGENT_HOOK` | Never read, never set by craze; plays no part in detecting roost. While craze reports [host status](#host-status) to roost, it is removed from the agent child's environment, so roost's own agent hooks stay inert inside craze's agent |

If neither `--agent-bin` nor `CRAZE_AGENT_BIN` is set, Cursor looks for
`cursor-agent`, then `agent`, on `PATH`. Grok looks for `grok` only. gx looks
for `gx` only.
