# Quick Start

## Prerequisites

1. Linux or macOS.
2. For the ACP providers, install the [Cursor CLI](https://cursor.com/cli), the
   [Grok CLI](https://docs.x.ai/build/cli/headless-scripting) (`grok`), or
   [`gx`](https://github.com/charliek/grok-build) — a third-party fork of
   the Grok CLI that speaks the same ACP dialect as `grok`, so everything
   here about Grok applies to it too.
3. Log in: `cursor-agent login` (the same binary is also installed as
   `agent`), or `grok login` / set `XAI_API_KEY`. `gx` currently shares
   grok's `~/.grok` home, so the same login covers it — that is the fork's
   current behaviour, not a craze guarantee.

The native provider needs neither step: it runs inside craze and needs only an
API key (see [Native provider](#native-provider) below).

## Install

### Homebrew (macOS, Apple Silicon and Linux amd64/arm64) — available from v0.0.1

```bash
brew install charliek/tap/craze
```

Intel macOS (`darwin/amd64`) is cross-compiled and shipped but not tested at
runtime; treat it as best-effort. Apple Silicon and Linux (amd64/arm64) are
tested.

### apt (Ubuntu 24.04+ and derivatives, amd64/arm64) — available from v0.0.1

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

### Direct `.deb` — available from v0.0.1

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
```

`make build` writes `./bin/craze` and `./bin/craze-fake-agent`. The fake agent
is for tests; a live session uses `cursor-agent`, `grok`, or `gx` on `PATH` (or
`--agent-bin` / `CRAZE_AGENT_BIN`). This path needs Go 1.27+ (this repo pins 1.27.1 via
`.mise.toml`; `mise install`).

## First run

`./bin/craze` is the build-from-source path; an installed craze is on your
`PATH`, so drop the `./bin/`.

```bash
./bin/craze                      # the TUI, in the current directory
./bin/craze --workspace ../proj  # somewhere else
./bin/craze --no-force           # ask before each tool call
./bin/craze --theme gruvbox --no-mouse
```

`--force` (yolo) is the default. `--no-force` turns on the permission line.
`--model` picks the model to start on (an ACP model id, or a native model
alias), `--ask` / `--plan` set the session mode.

The screen is, top to bottom: the transcript, the pinned tasks panel, the
spinner line, the composer, two status rows, and one row per in-flight
sub-agent. See the [TUI reference](../reference/tui.md) for keys and cards.

craze refuses to start the TUI on a non-tty. For a scripted turn, use
[`craze prompt`](../reference/cli.md#craze-prompt).

## Native provider

The native provider (`--provider native`) runs the agent inside craze itself,
talking to a model provider's API: there is no agent CLI to install, only an
API key to give it. craze ships a catalog of models from four providers and
needs a key for at least one:

| Provider | Id for `craze auth` | Or export |
|----------|---------------------|-----------|
| Fireworks | `fireworks` | `FIREWORKS_API_KEY` |
| Meta | `meta` | `META_API_KEY` |
| OpenRouter | `openrouter` | `OPENROUTER_API_KEY` |
| Z.AI Coding Plan | `zai-coding-plan` | `ZHIPU_API_KEY` or `ZAI_API_KEY` |

1. Give craze a key, one of two ways:

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
   (`env FIREWORKS_API_KEY`, `stored key`, or `not connected`).

3. Start a native session:

    ```bash
    craze --provider native                    # the TUI
    craze prompt --provider native "hello"     # one headless turn
    ```

    With no key for any provider, the session refuses to start and says how to
    give it one. `/model` switches models, listing those of providers that
    have a key (with a `Connect a provider…` row while some provider has none;
    `/connect` stores a key from inside the TUI, and a newly connected provider's
    models are in `/model` at once, and in new sessions). `--model <alias>` starts on one.

`craze auth logout fireworks` removes a stored key. Keys, and the two files you
can use to change or add models, are in [Native models and
providers](../reference/configuration.md#native-models-and-providers); the
commands are in the [CLI reference](../reference/cli.md#craze-auth).

**Upgrades:** the model catalog is part of the craze binary, so upgrading craze
(`brew upgrade`, `apt upgrade`, a new `go install`) is how new models arrive and
retired ones go, with no edit on your side. Your own files hold only keys,
changes to shipped models, and models you add, and an upgrade never rewrites
them.

## Exit codes

craze exits **nonzero when the session never started** — not logged in to the
provider, or an agent binary (`cursor-agent`, `grok`, `gx`) that would not come up. It still draws the TUI and puts the
error in the transcript, because that is where you can read it, but the process
tells a script that nothing ran. An error *during* a session leaves a usable
craze, so quitting out of one is an ordinary exit 0.

## Next

- [CLI](../reference/cli.md) — flags for `craze` and `craze prompt`, and `craze auth`
- [TUI](../reference/tui.md) — keys, cards, slash commands
- [Configuration](../reference/configuration.md) — themes and `~/.craze/config.toml`
