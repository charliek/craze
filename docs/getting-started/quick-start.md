# Quick Start

## Prerequisites

1. Linux.
2. Install the [Cursor CLI](https://cursor.com/cli).
3. Log in: `cursor-agent login` (the same binary is also installed as
   `agent`).

## Install

```bash
go install github.com/charliek/craze/cmd/craze@main
```

`@main` pins the branch explicitly. `go install` writes the binary to `$GOBIN`,
or `$GOPATH/bin` when that is unset — `$HOME/go/bin` by default — so with that
directory on your `PATH` the command is `craze`:

```bash
craze version
```

Until the first tag, it reports `dev`.

## Build from source

```bash
git clone https://github.com/charliek/craze.git
cd craze
make build
./bin/craze version
```

`make build` writes `./bin/craze` and `./bin/craze-fake-agent`. The fake agent
is for tests; a live session uses `cursor-agent` on `PATH` (or `--agent-bin` /
`CRAZE_AGENT_BIN`). This path needs Go 1.24+ (this repo pins 1.24 via
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

craze exits **nonzero when the session never started** — no Cursor login, or a
`cursor-agent` that would not come up. It still draws the TUI and puts the
error in the transcript, because that is where you can read it, but the process
tells a script that nothing ran. An error *during* a session leaves a usable
craze, so quitting out of one is an ordinary exit 0.

## Next

- [CLI](../reference/cli.md) — flags for `craze` and `craze prompt`
- [TUI](../reference/tui.md) — keys, cards, slash commands
- [Configuration](../reference/configuration.md) — themes and `~/.craze/config.toml`
