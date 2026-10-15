# craze

craze is a terminal UI and CLI for Linux and macOS that drives a coding agent
over ACP (the Agent Client Protocol): **Cursor** (`cursor-agent`), **Grok**
(`grok`), **gx** (a third-party fork of the Grok CLI), or craze's own **native**
agent, which runs inside craze against a model provider's API or a ChatGPT plan
with no other CLI to install. You own the chrome; the provider still runs the
agent.

Sessions run in a detached host and outlive the terminal: close the tab and the
session keeps going, and you can start, script and rejoin sessions from other
terminals and programs. It is for people who work with coding agents all day
and want one fast, scriptable front end for several of them.

## Highlights

- **The TUI**: transcript, queued messages, tasks, sub-agent rows, cards, mouse
  and copy. [TUI reference](docs/reference/tui.md)
- **Native providers and the ChatGPT plan**: Anthropic (Claude), Fireworks,
  Meta, OpenRouter and Z.AI with an API key, or a ChatGPT plan sign-in; no
  agent CLI needed. [Native provider](docs/getting-started/quick-start.md#native-provider)
- **Sessions that outlive the terminal**: `craze ps`, `craze attach`,
  `craze new`. [Sessions](docs/reference/cli.md#sessions-outlive-their-terminal)
- **Resume a saved session through the hub**: `craze ps --saved` and
  `craze new --load`, and the same for a hub client such as shed on a phone.
  [`craze new --load`](docs/reference/cli.md#craze-new-load)
- **Scripting**: `craze new --wait`, `craze send` and `craze wait` run a task
  and print the result. [`craze new`](docs/reference/cli.md#craze-new),
  [`craze wait`](docs/reference/cli.md#craze-wait)
- **Claude Code sub-agents**: a plugin that hands a task to a craze session.
  [Claude Code sub-agents](docs/getting-started/claude-code.md)
- **`/btw` side questions**: ask without adding to the session.
  [Side questions](docs/reference/tui.md#side-questions-btw)
- **`/effort`**: change the current model's effort in a session, from a picker
  or as `/effort high`. [Effort](docs/reference/tui.md#effort)
- **Themes**: seven presets, set from `Ctrl+G` or `/theme`.
  [Configuration](docs/reference/configuration.md)
- **`craze prompt`**: one headless turn, optionally as JSON.
  [`craze prompt`](docs/reference/cli.md#craze-prompt)

## Install

Homebrew (macOS and Linux):

```shell
brew install charliek/tap/craze
```

apt (Ubuntu 24.04+ and derivatives, amd64/arm64), add the repo once, then install:

```bash
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://apt.stridelabs.ai/pubkey.gpg | \
  sudo tee /etc/apt/keyrings/apt-charliek.gpg > /dev/null
echo 'deb [signed-by=/etc/apt/keyrings/apt-charliek.gpg] https://apt.stridelabs.ai noble main' | \
  sudo tee /etc/apt/sources.list.d/apt-charliek.list
sudo apt update
sudo apt install craze
```

The [Quick Start](docs/getting-started/quick-start.md#install) has the direct
`.deb`, `go install`, building from source and what `craze version` reports. To
run craze you also need a way to reach a model: the native provider (an API key
or a ChatGPT plan), or `cursor-agent`, `grok` or `gx` installed and logged in
([prerequisites](docs/getting-started/quick-start.md#prerequisites)).
Upgrading? See [Upgrading](docs/getting-started/upgrading.md).

## Quick start

```shell
craze                       # pick a provider, then the TUI in the current directory
craze --provider grok       # skip the picker (cursor, grok, gx or native)
craze auth login fireworks  # connect a native provider; `craze auth login chatgpt` signs in with a ChatGPT plan
craze --provider native     # start a native session
craze -c                    # continue the newest session in this workspace
craze ps                    # list this machine's sessions
```

More in the [Quick Start](docs/getting-started/quick-start.md).

## Use from Claude Code

This repository is also a Claude Code plugin: sub-agents that hand a task to a
craze session on a native, Grok or Cursor model and return its result. It needs
craze 0.2.0 or newer on your `PATH`.

```text
/plugin marketplace add charliek/craze
/plugin install craze@craze
```

See [Claude Code sub-agents](docs/getting-started/claude-code.md).

## Documentation

Published site: https://charliek.github.io/craze/

Sources live under `docs/`:

- [Quick Start](docs/getting-started/quick-start.md)
- [Upgrading](docs/getting-started/upgrading.md)
- [Claude Code sub-agents](docs/getting-started/claude-code.md)
- [CLI](docs/reference/cli.md)
- [TUI](docs/reference/tui.md)
- [Configuration](docs/reference/configuration.md)
- [Protocol](docs/reference/protocol.md)
- [Development](docs/development/setup.md), [Testing](docs/development/testing.md) and [Architecture](docs/development/architecture.md)

## Development

```shell
make build        # ./bin/craze and ./bin/craze-fake-agent
make lint && make test && make test-race && make build && make test-cli   # the gate
make docs         # same as CI: zensical build --strict
```

See [setup](docs/development/setup.md) and [testing](docs/development/testing.md).

## Status

Early-stage software, released as v0.1.x for Linux and macOS (Homebrew, apt and
release archives). `craze` opens a TUI; `craze prompt --json` is the headless
path, and `craze new --wait` and `craze send` drive sessions from other programs.

## License

MIT. See [LICENSE](LICENSE).
