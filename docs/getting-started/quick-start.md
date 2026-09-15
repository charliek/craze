# Quick Start

## Prerequisites

1. Linux or macOS.
2. Install either the [Cursor CLI](https://cursor.com/cli) or the
   [Grok CLI](https://docs.x.ai/build/cli/headless-scripting) (`grok`).
3. Log in: `cursor-agent login` (the same binary is also installed as
   `agent`), or `grok login` / set `XAI_API_KEY`.

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
is for tests; a live session uses `cursor-agent` or `grok` on `PATH` (or
`--agent-bin` / `CRAZE_AGENT_BIN`). This path needs Go 1.24+ (this repo pins 1.24 via
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
`--model` picks an ACP model id, `--ask` / `--plan` set the session mode.

The screen is, top to bottom: the transcript, the pinned tasks panel, the
spinner line, the composer, two status rows, and one row per in-flight
sub-agent. See the [TUI reference](../reference/tui.md) for keys and cards.

craze refuses to start the TUI on a non-tty. For a scripted turn, use
[`craze prompt`](../reference/cli.md#craze-prompt).

## Exit codes

craze exits **nonzero when the session never started** — not logged in to the
provider, or an agent binary (`cursor-agent`, `grok`) that would not come up. It still draws the TUI and puts the
error in the transcript, because that is where you can read it, but the process
tells a script that nothing ran. An error *during* a session leaves a usable
craze, so quitting out of one is an ordinary exit 0.

## Next

- [CLI](../reference/cli.md) — flags for `craze` and `craze prompt`
- [TUI](../reference/tui.md) — keys, cards, slash commands
- [Configuration](../reference/configuration.md) — themes and `~/.craze/config.toml`
