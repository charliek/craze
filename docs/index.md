# craze

A Linux and macOS terminal UI for coding agents. It talks ACP to
[Cursor CLI](https://cursor.com/cli) (`cursor-agent acp`),
[Grok CLI](https://docs.x.ai/build/cli/headless-scripting) (`grok`), or `gx`
([`charliek/grok-build`](https://github.com/charliek/grok-build)), a
third-party fork of the Grok CLI. You own the chrome; the provider still
runs the agent. Its **native** provider runs the agent inside craze instead,
against a model provider's API or a ChatGPT plan, and needs no other CLI (see
[Native provider](getting-started/quick-start.md#native-provider)).

```bash
./bin/craze                      # the TUI, in the current directory
./bin/craze --workspace ../proj  # somewhere else
./bin/craze --no-force           # ask before each tool call
```

`--force` (yolo) is the default. `--no-force` turns on the permission line.

## Current Scope

Early-stage software: v0.1.x is released (Homebrew, apt and release archives)
for Linux and macOS. `craze` opens a TUI; `craze prompt --json` is the headless
path. Sessions run in a detached host and outlive the
terminal: `craze ps` lists them, `craze attach` and `craze -c` rejoin one, and
`craze new` starts one in the background through the per-machine hub (see the
[CLI reference](reference/cli.md#sessions-outlive-their-terminal)).

## What it does

- **A TUI over four providers.** `cursor`, `grok`, `gx` and the built-in
  `native` agent share one set of keys, cards and slash commands
  ([TUI reference](reference/tui.md)).
- **Sessions that outlive the terminal.** A detached host keeps the agent
  running; `craze ps`, `craze attach` and `craze -c` rejoin it
  ([CLI reference](reference/cli.md#sessions-outlive-their-terminal)).
- **Scripted and background use.** `craze prompt --json` runs one turn;
  `craze new`, `send`, `wait`, `cancel` and `stop` drive sessions without a
  terminal ([CLI reference](reference/cli.md#craze-new)).
- **Claude Code sub-agents.** A plugin hands a task to a craze session, on any
  provider, and returns the result ([Claude Code](getting-started/claude-code.md)).
- **Side questions.** `/btw <question>` asks a quick question without adding to
  the session ([TUI reference](reference/tui.md#side-questions-btw)).
- **Effort and speed.** `--effort` and `--fast`/`--no-fast` start a model at a
  chosen setting ([CLI reference](reference/cli.md#flags)).
- **Native start models.** A new native session starts on the first provider
  you have connected, in the order Z.AI, ChatGPT plan, Fireworks, OpenRouter,
  Meta ([Configuration](reference/configuration.md#the-shipped-catalog)).
- **A render check.** `craze frame` renders the TUI once for tests
  ([Testing](development/testing.md#craze-frame)).

To run craze you need:

- Linux or macOS
- One way to reach a model:
    - the **native provider** (`--provider native`), which needs none of the
      CLIs below: only a model provider's API key, or a ChatGPT plan sign-in
      (`craze auth login chatgpt`); or
    - one of: the [Cursor CLI](https://cursor.com/cli), with a working login
      (`cursor-agent login`, the same binary is also installed as `agent`); the
      [Grok CLI](https://docs.x.ai/build/cli/headless-scripting) (`grok`), with
      `grok login` or `XAI_API_KEY` set; or
      [`gx`](https://github.com/charliek/grok-build), a third-party fork of the
      Grok CLI that speaks the same ACP dialect as `grok`, so everything these
      docs say about Grok's behaviour applies to it too — it currently shares
      grok's `~/.grok` home (config, auth, sessions, skills), which is the
      fork's current behaviour, not a craze guarantee

Go 1.27+ (this repo pins 1.27.1 via `.mise.toml`) is needed only to build from
source. Brew, apt and the release archives need no Go.

Coming from v0.0.1? See [Upgrading](getting-started/upgrading.md).

## Next Steps

- [Quick Start](getting-started/quick-start.md) — install and first run
- [Upgrading](getting-started/upgrading.md) — what changed since v0.0.1 and since v0.1.0
- [Claude Code](getting-started/claude-code.md) — craze sessions as Claude Code sub-agents
- [CLI Reference](reference/cli.md) — commands and flags
- [TUI Reference](reference/tui.md) — keys, cards, slash commands
- [Configuration](reference/configuration.md) — themes, config file, env vars
- [Protocol](reference/protocol.md) — the control socket's wire protocol
