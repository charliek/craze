# Quick Start

## Prerequisites

1. Linux or macOS.
2. One way to reach a model, either of:
    - **craze's own native provider**, which needs no other CLI: an API key for
      one of the model providers craze ships a catalog for, or a ChatGPT plan
      to sign in with (see [Native provider](#native-provider) below); or
    - **an ACP agent**: install the [Cursor CLI](https://cursor.com/cli), the
      [Grok CLI](https://docs.x.ai/build/cli/headless-scripting) (`grok`), or
      [`gx`](https://github.com/charliek/grok-build) — a third-party fork of
      the Grok CLI that speaks the same ACP dialect as `grok`, so everything
      here about Grok applies to it too — and log in: `cursor-agent login` (the
      same binary is also installed as `agent`), or `grok login` / set
      `XAI_API_KEY`. `gx` currently shares grok's `~/.grok` home, so the same
      login covers it — that is the fork's current behaviour, not a craze
      guarantee.

Coming from v0.0.1? Read [Upgrading](upgrading.md) first.

## Install

### Homebrew (macOS, Apple Silicon and Linux amd64/arm64) — v0.0.1 and later

```bash
brew install charliek/tap/craze
```

Intel macOS (`darwin/amd64`) is cross-compiled and shipped but not tested at
runtime; treat it as best-effort. Apple Silicon and Linux (amd64/arm64) are
tested.

### apt (Ubuntu 24.04+ and derivatives, amd64/arm64) — v0.0.1 and later

Add the repo once:

```bash
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://apt.stridelabs.ai/pubkey.gpg | \
  sudo tee /etc/apt/keyrings/apt-charliek.gpg > /dev/null
echo 'deb [signed-by=/etc/apt/keyrings/apt-charliek.gpg] https://apt.stridelabs.ai noble main' | \
  sudo tee /etc/apt/sources.list.d/apt-charliek.list
sudo apt update
```

Then:

```bash
sudo apt install craze
```

### Direct `.deb` — v0.0.1 and later

Download the asset for your architecture from the
[GitHub Release](https://github.com/charliek/craze/releases) — named
`craze_<version>_<arch>.deb` (no `linux` segment in the name) — then:

```bash
sudo apt install ./craze_<version>_<arch>.deb
```

### From source

Source installs require Go 1.27 or later on `PATH`; verify with `go version`.

```bash
go install github.com/charliek/craze/cmd/craze@latest
```

`go install` writes the binary to `$GOBIN`, or `$GOPATH/bin` when that is
unset — `$HOME/go/bin` by default — so with that directory on your `PATH`
the command is `craze`.

### `craze version`

Homebrew, the `.deb`, and the release archives all report the release tag.
`go install .../craze@v0.0.1` (and `@latest`) also reports `0.0.1`, because
the source default in `internal/version/version.go` is bumped as part of
cutting that release. `go install .../craze@main`, though, reports the
**last released version**, not `dev` — it reads that same bumped source
default, so it lags behind whatever landed on `main` since. Prefer `@latest`
over `@main` for that reason. `make build` (building from a checkout) always
reports `dev`.

## Build from source

```bash
git clone https://github.com/charliek/craze.git
cd craze
make build
./bin/craze version
./bin/craze prompt --json --agent-bin ./bin/craze-fake-agent "hello"
```

`make build` writes `./bin/craze` and `./bin/craze-fake-agent`. The fake agent
is for tests; a live session uses the native provider, or `cursor-agent`, `grok`
or `gx` on `PATH` (or `--agent-bin` / `CRAZE_AGENT_BIN`). This path needs Go 1.27+ (this repo pins 1.27.1 via
`.mise.toml`; `mise install`).

## First run

`./bin/craze` is the build-from-source path; an installed craze is on your
`PATH`, so drop the `./bin/`.

```bash
./bin/craze                      # picker, then the TUI in the current directory
./bin/craze --provider cursor    # skip the picker (also grok, gx, native)
./bin/craze --workspace ../proj  # somewhere else
./bin/craze --no-force           # ask before each tool call
./bin/craze --theme gruvbox --no-mouse
./bin/craze --continue           # reload the newest session in this workspace
./bin/craze --resume             # pick one of the last 10 sessions here
```

`--force` (yolo) is the default. `--no-force` turns on the permission line.
`--model` picks the model to start on (an ACP model id, or a native model
alias), `--effort` and `--fast`/`--no-fast` start it at an effort or speed
setting, `--ask` / `--plan` set the session mode.

The screen is, top to bottom: the transcript, the pinned tasks panel, the
queued messages, the spinner line, the composer, two status rows, and one row
per in-flight sub-agent. See the [TUI reference](../reference/tui.md) for keys
and cards. Three things worth knowing at once: `/btw <question>` asks a side
question without adding to the session ([where it is
offered](../reference/tui.md#side-questions-btw)), `/effort` changes the
current model's effort — a picker of its levels, or `/effort high`
([Effort](../reference/tui.md#effort)) — and `Ctrl+V` pastes an image from the
clipboard into the composer ([Images](../reference/tui.md#images)).

craze refuses to start the TUI on a non-tty. For a scripted turn, use
[`craze prompt`](../reference/cli.md#craze-prompt).

## Native provider

The native provider (`--provider native`) runs the agent inside craze itself,
talking to a model provider's API: there is no agent CLI to install, only an
API key or a ChatGPT plan sign-in to give it. craze ships a catalog of models
from five API-key providers, and can also use a ChatGPT plan; it needs one of
them connected:

| Provider | Id for `craze auth` | Or export |
|----------|---------------------|-----------|
| Anthropic | `anthropic` | `ANTHROPIC_API_KEY` |
| ChatGPT plan | `chatgpt` | none: `craze auth login chatgpt` signs in with your ChatGPT account |
| Fireworks | `fireworks` | `FIREWORKS_API_KEY` |
| Meta | `meta` | `META_API_KEY` |
| OpenRouter | `openrouter` | `OPENROUTER_API_KEY` |
| Z.AI Coding Plan | `zai-coding-plan` | `ZHIPU_API_KEY` or `ZAI_API_KEY` |

1. Connect one, either of two ways:

    **With a ChatGPT plan**, sign in instead of giving a key:

    ```bash
    craze auth login chatgpt          # prints an address to open and approve in a browser
    ```

    craze opens the browser itself on a desktop or on macOS, and waits for the
    browser to come back to `http://127.0.0.1:1455/auth/callback`. Over SSH, or
    when the browser is on another machine, that page does not load: copy its
    whole address from the browser's address bar and paste it at craze's
    `Redirect address: ` prompt (`craze auth login chatgpt --no-browser` skips
    the browser and the listener and uses only that prompt). Inside the TUI,
    `/connect` does the same. Details: [Signing in to the ChatGPT
    plan](../reference/cli.md#signing-in-to-the-chatgpt-plan).

    **With an API key**, give craze one of two ways:

    ```bash
    craze auth login fireworks        # asks for the key; it is not shown as you type
    export FIREWORKS_API_KEY=...      # or export the provider's variable
    ```

    `craze auth login` with no provider shows a numbered list to pick from.
    When stdin is not a terminal it reads the key from stdin's first line, so
    a script can pipe it in (`printf '%s\n' "$KEY" | craze auth login
    fireworks`). The key is stored in `~/.craze/native/providers.toml` (or
    under `$CRAZE_HOME`), readable only by you. An exported variable is used
    before a stored key. craze does not check the key with the provider when
    you store it: a wrong one shows as an error on first use.

2. Check it: `craze auth list` shows each provider and how it is connected
   (`env FIREWORKS_API_KEY`, `stored key`, `not connected`; the ChatGPT plan
   reads `signed in as <email> …` or `not signed in`).

3. Start a native session:

    ```bash
    craze --provider native                    # the TUI
    craze prompt --provider native "hello"     # one headless turn
    ```

    With nothing connected, the session refuses to start and says how to
    connect one. `/model` switches models, listing those of providers that
    have a key (with a `Connect a provider…` row while some provider has none;
    `/connect` stores a key from inside the TUI, and a newly connected provider's
    models are in `/model` at once, and in new sessions). `--model <alias>` starts on one.

    With no `--model` and nothing remembered, a new session starts on the
    first provider you have connected, in this order: Z.AI Coding Plan,
    ChatGPT plan, Fireworks, OpenRouter, Meta, Anthropic, each on its own
    start model ([the table](../reference/configuration.md#the-shipped-catalog);
    Anthropic's is the newest Claude Opus your account lists).
    OpenRouter's models are its `~<family>-latest` routers, so an alias there
    may resolve to a newer model after OpenRouter updates the router.

`craze auth logout fireworks` removes a stored key. Keys, and the two files you
can use to change or add models, are in [Native models and
providers](../reference/configuration.md#native-models-and-providers); the
commands are in the [CLI reference](../reference/cli.md#craze-auth).

**Upgrades:** the model catalog is part of the craze binary, so upgrading craze
(`brew upgrade`, `apt upgrade`, a new `go install`) is how new models arrive and
retired ones go, with no edit on your side. Your own files hold only keys,
changes to shipped models, and models you add, and an upgrade never rewrites
them.

## Sessions outlive the terminal

The ordinary `craze` runs the session in a detached host, so closing the terminal
keeps it running: `craze ps` lists this machine's sessions, `craze attach` (or
`craze -c`) joins one, `craze new` starts one in the background through the
per-machine hub, `craze ps --saved` and `craze new --load` find and bring back
one whose host has gone, and `/exit` ends one. On an empty composer `←` (or
`/sessions`) opens the [session list](../reference/tui.md#session-list). Other
tools reach the same sessions: `craze providers` reports which providers can
start, and `craze bridge` speaks the control protocol over stdio for a client
that is not a terminal ([CLI reference](../reference/cli.md#craze-providers),
[`craze bridge`](../reference/cli.md#craze-bridge)). Inside a herdr pane or a
roost tab craze reports its status to that host ([Host
status](../reference/configuration.md#host-status)). Detaching, the journal and
the control socket each have a switch, listed in [Upgrading](upgrading.md); the
[CLI reference](../reference/cli.md#sessions-outlive-their-terminal) has the
commands.

To use craze from Claude Code, as a sub-agent that runs a task on another
model, see [Claude Code sub-agents](claude-code.md).

## Exit codes

craze exits **nonzero when the session never started** — not logged in to the
provider, no native credential, or an agent binary (`cursor-agent`, `grok`, `gx`) that would not come up. It still draws the TUI and puts the
error in the transcript, because that is where you can read it, but the process
tells a script that nothing ran. An error *during* a session leaves a usable
craze, so quitting out of one is an ordinary exit 0. The commands that wait for
a turn (`craze wait`, `craze new --wait`, `craze send --wait`) exit with the
turn's result instead: see [Waiting commands](../reference/cli.md#waiting-commands).

## Next

- [CLI](../reference/cli.md) — flags for `craze` and `craze prompt`, and `craze auth`
- [TUI](../reference/tui.md) — keys, cards, slash commands
- [Upgrading](upgrading.md) — what changed since v0.0.1 and since v0.1.0
- [Claude Code](claude-code.md) — craze sessions as Claude Code sub-agents
- [Configuration](../reference/configuration.md) — themes and `~/.craze/config.toml`
