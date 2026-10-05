# Upgrading from v0.0.1

v0.0.1 was the first release. Nothing in `~/.craze` needs migrating: it wrote
only `config.toml` and `sessions.jsonl` (each with a `.lock` beside it), and
this version reads both unchanged. Your theme, your default provider and your
saved sessions carry over, and `craze --continue` and `craze --resume` load
the sessions v0.0.1 saved. A downgrade works too: v0.0.1 keeps what it does
not know, and falls back to `cursor` with a warning from a
`provider = "native"`.

What changed is how craze behaves around those files. This page lists each
change a v0.0.1 user meets, how to turn it off where it can be turned off, and
the setting that controls it.

| Change | Opt out |
|--------|---------|
| [`CRAZE_CONFIG` is gone](#craze_config-is-now-craze_home) | none; use `CRAZE_HOME` |
| [Sessions outlive the terminal](#sessions-outlive-the-terminal) | `detach = false` or `CRAZE_DETACH=0` |
| [A journal of every session](#the-journal) | `journal = false` or `CRAZE_JOURNAL=0` |
| [A control socket](#the-control-socket) | `control_socket = false` or `CRAZE_CONTROL_SOCKET=0` |
| [A per-machine hub](#the-hub) | `craze ps --no-hub`; it exits by itself |
| [Status reports to herdr and roost](#status-reports-to-herdr-and-roost) | `host_status = false` or `--no-host-status` |
| [`native` in the picker](#native-in-the-provider-picker) | `--provider` skips the picker |
| [`←` opens the session list](#opens-the-session-list) | `detach = false` |
| [`--agent-bin` is per launch](#-agent-bin-and-craze_agent_bin) | `[agents]` in `config.toml` |
| [`seq` in `prompt --json`](#seq-in-prompt-json) | none; it is additive |
| [New directories](#new-directories) | see the page |

## `CRAZE_CONFIG` is now `CRAZE_HOME`

v0.0.1 read `CRAZE_CONFIG`, the path of the config *file*. It is removed, not
kept as an alias: with it set, every command exits with status 2 before
reading or writing anything (`craze bridge` exits 1), except `--help` and
`--version`:

```text
craze: CRAZE_CONFIG is no longer supported; unset it and set CRAZE_HOME to the directory that holds config.toml instead (it defaults to ~/.craze)
```

To move over, unset `CRAZE_CONFIG` (check your shell's startup files and any
scripts) and set `CRAZE_HOME` to the **directory** the file was in:
`CRAZE_CONFIG=/some/dir/config.toml` becomes `CRAZE_HOME=/some/dir`. craze now
looks for the fixed names `config.toml` and `sessions.jsonl` there, so a config
file with any other name has to be renamed `config.toml`. `CRAZE_HOME` moves
the whole [craze directory](../reference/configuration.md#the-craze-directory),
and no variable moves a single file on its own.

## Sessions outlive the terminal

The ordinary `craze` no longer runs the session inside the terminal's process.
It starts a detached host (`craze serve`, in a session of its own with no
terminal) and the TUI is that host's client. Closing the terminal, a dropped
ssh connection or a SIGHUP closes the *view*: the host and its agent keep
running, and a turn already under way keeps working. `craze -c` or `craze
attach` picks it up again, and `craze ps` lists what is running.

- **Stop a session:** `/exit`, `Ctrl+D` or a second `Ctrl+C` in the TUI or in
  `craze attach` ends it everywhere; in the session list (`←`), `Ctrl+X` stops a
  working session's turn and, pressed twice, closes an idle one. A stopped session stays
  resumable with `craze -c`.
- **Idle exit:** a host with no client attached and nothing in flight exits by
  itself after an hour. `host_idle_exit` in `config.toml` sets that (`"20m"`,
  `"2h30m"`, `"never"`); a session that was never prompted is capped at five
  minutes, and a host whose start failed goes as soon as its launcher has been
  told.
- **Turn it off:** `detach = false` in `config.toml`, or `CRAZE_DETACH=0` for
  one run, runs the session inside the TUI's own process as v0.0.1 did:
  closing the terminal ends it, and nothing outlives it.

An unattended host's agent is a running process, so a long-lived session holds
whatever its agent holds. See [Detached
hosts](../reference/configuration.md#detached-hosts) and [Sessions outlive their
terminal](../reference/cli.md#sessions-outlive-their-terminal). A host's own log
is `~/.cache/craze/host-logs/<hostId>.log`.

## The journal

Every session now writes a journal, one file per session, under
`~/.craze/journal/<workspace>/` (or `$CRAZE_HOME/journal/`). A `craze prompt`
run leaves one file too. It records the events, and your prompts and every
tool's output as the session admitted them. The journal itself adds no
redaction (on native, a `!` command's known provider credentials are already
redacted before the prompt is recorded), so any other secret, such as a key you
typed into a shell command or a file the agent read, is in it in plain text. The files are `0600`
in `0700` directories, never uploaded, and **never pruned**: they accumulate
until you delete them (deleting the directory is always safe).

- **Turn it off:** `journal = false` in `config.toml`, or `CRAZE_JOURNAL=0` for
  one run. The opt-out fails closed: anything craze cannot read as a plain
  `true` means off.

See [Session journal](../reference/configuration.md#session-journal).

## The control socket

Every host binds a Unix-domain control socket, which is how `craze attach`,
`craze bridge` and the session list reach it. It lives in a `0700` runtime
directory, `$XDG_RUNTIME_DIR/craze/<ns>/` (`/tmp/craze-<uid>` where there is
none), is mode `0600`, and is reachable only by you.

- **Turn it off:** `control_socket = false` in `config.toml`, or
  `CRAZE_CONTROL_SOCKET=0` for one run. A detached host is reachable only
  through its socket, so with the socket off detaching is off too. The opt-out
  fails closed.

See [The control socket](../reference/configuration.md#the-control-socket).

## The hub

`craze hub` is one process per user and craze directory. It serves the list of
running sessions and starts sessions in the background (`craze new`). You never
start it: `craze ps`, `craze new` and the TUI's session list start one when
none runs, and the next one finds it. **It exits by itself 60 seconds after the
last host has gone and the last client has disconnected.** Killing it loses
nothing: hosts run on without it. `craze ps --no-hub` reads each session's host
directly and starts none. Its log is `~/.cache/craze/host-logs/hub-<ns>.log`.
See [The hub](../reference/cli.md#the-hub).

## Status reports to herdr and roost

Inside a herdr pane or a roost tab, craze reports its state (idle, working,
blocked), a one-line reason, and the provider and model to that host, never
conversation content. Outside both it reports nothing.

- **Turn it off:** `host_status = false` in `config.toml`, or `--no-host-status`
  on the command line. That also leaves the agent's environment whole.

See [Host status](../reference/configuration.md#host-status).

## `native` in the provider picker

Without `--provider`, the startup picker now lists `native`, craze's own agent,
beside `cursor`, `grok` and `gx` (gx only when its binary resolves). A provider
that cannot start is dimmed and says why: `native` reads `needs setup` until you
give it an API key or sign in to a ChatGPT plan (see [Native
provider](quick-start.md#native-provider)). The default is still the one you
had: `$CRAZE_PROVIDER`, then `provider` in `config.toml`, then cursor.
`--provider cursor` (or grok, gx) skips the picker.

## `←` opens the session list

On an empty composer `←` opens the [session list](../reference/tui.md#session-list),
and `/sessions` does the same. It applies only when sessions run in detached
hosts (the default): with `detach = false` or `CRAZE_DETACH=0` the key reaches
the composer as it did in v0.0.1.

## `--agent-bin` and `CRAZE_AGENT_BIN`

`--agent-bin` and `CRAZE_AGENT_BIN` now apply only to the launch's own
provider: the one the command line resolved (`--provider`, `$CRAZE_PROVIDER`,
`provider` in `config.toml`, then cursor), or the loaded session's for a
`--continue` or `--resume`. A session of another provider, one the picker or the
session list starts, or one the hub creates, never runs the binary a launch
named for its own. To set a provider's binary for good, use `[agents]` in
`config.toml` with an absolute path:

```toml
[agents]
cursor = "/opt/cursor/bin/cursor-agent"
```

See [Agent binaries](../reference/configuration.md#agent-binaries).

## `seq` in `prompt --json`

Every event `craze prompt --json` prints now carries a `seq`, the number the
journal and the control socket use for it. It is an added field: a reader that
ignores unknown fields is unaffected. See [JSON events](../reference/cli.md#json-events).

## New directories

craze now also creates, all `0700` with `0600` files, and only when it needs
them:

| Where | Holds |
|-------|-------|
| `~/.craze/journal/` | [the journals](#the-journal) |
| `~/.craze/native/` | the native provider's keys (`providers.toml`), `models.toml`, `recent.json`, and the ChatGPT plan's tokens (`auth/chatgpt.json`); native session transcripts (`sessions/`); created on first use |
| `~/.craze/attachments/` | images pasted into the composer, swept after seven days |
| `~/.cache/craze/` | the host registry, the hub's record, locks, host logs and the model catalog cache; under your own `$HOME`, not `CRAZE_HOME` |
| `$XDG_RUNTIME_DIR/craze/<ns>/` | [the sockets](#the-control-socket) |

`~/.craze` itself moves with `CRAZE_HOME`; the cache and runtime directories do
not. A `~/.cache/craze` that already exists with a looser mode than `0700` is
refused, and craze never changes it: `chmod 700` it or remove it, or run with
`CRAZE_DETACH=0`. See [The craze directory](../reference/configuration.md#the-craze-directory).
