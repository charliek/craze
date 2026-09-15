# craze

A Linux and macOS terminal UI for [Cursor CLI](https://cursor.com/cli) and
[Grok CLI](https://docs.x.ai/build/cli/headless-scripting), talking to
`cursor-agent acp` or `grok` over ACP. You own the chrome; the provider still
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
- Go 1.24+ (this repo pins 1.24 via `.mise.toml`)
- Either the [Cursor CLI](https://cursor.com/cli), with a working login
  (`cursor-agent login`, the same binary is also installed as `agent`), or
  the [Grok CLI](https://docs.x.ai/build/cli/headless-scripting) (`grok`),
  with `grok login` or `XAI_API_KEY` set

## Next Steps

- [Quick Start](getting-started/quick-start.md) — build and first run
- [CLI Reference](reference/cli.md) — commands and flags
- [TUI Reference](reference/tui.md) — keys, cards, slash commands
- [Configuration](reference/configuration.md) — themes, config file, env vars
