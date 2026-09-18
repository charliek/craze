# craze

A Linux and macOS terminal UI for [Cursor CLI](https://cursor.com/cli) and
[Grok CLI](https://docs.x.ai/build/cli/headless-scripting), talking to
`cursor-agent acp` or `grok` over ACP — or to `gx`
([`charliek/grok-build`](https://github.com/charliek/grok-build)), a
third-party fork of the Grok CLI. You own the chrome; the provider still
runs the agent.

```bash
./bin/craze                      # the TUI, in the current directory
./bin/craze --workspace ../proj  # somewhere else
./bin/craze --no-force           # ask before each tool call
```

`--force` (yolo) is the default. `--no-force` turns on the permission line.

## Current Scope

POC on Linux and macOS. `craze` opens a TUI; `craze prompt --json` is the
headless path. Multi-project harness integration is later work.

craze requires:

- Linux or macOS
- Go 1.27+ (this repo pins 1.27.1 via `.mise.toml`)
- One of: the [Cursor CLI](https://cursor.com/cli), with a working login
  (`cursor-agent login`, the same binary is also installed as `agent`); the
  [Grok CLI](https://docs.x.ai/build/cli/headless-scripting) (`grok`), with
  `grok login` or `XAI_API_KEY` set; or
  [`gx`](https://github.com/charliek/grok-build), a third-party fork of the
  Grok CLI that speaks the same ACP dialect as `grok`, so everything these
  docs say about Grok's behaviour applies to it too — it currently shares
  grok's `~/.grok` home (config, auth, sessions, skills), which is the
  fork's current behaviour, not a craze guarantee

## Next Steps

- [Quick Start](getting-started/quick-start.md) — build and first run
- [CLI Reference](reference/cli.md) — commands and flags
- [TUI Reference](reference/tui.md) — keys, cards, slash commands
- [Configuration](reference/configuration.md) — themes, config file, env vars
