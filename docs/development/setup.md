# Development Setup

## Prerequisites

- Linux or macOS
- Go 1.27 or later
- [mise](https://mise.jdx.dev/) (optional; this repo pins Go and golangci-lint in `.mise.toml`)
- [uv](https://docs.astral.sh/uv/) (docs site and `make test-cli`)

## Clone and Build

```bash
git clone https://github.com/charliek/craze.git
cd craze
mise install
make build
```

The Makefile prepends `~/.local/share/mise/shims` so `make` works without an
activated shell.

Or without make:

```bash
go build -o bin/craze ./cmd/craze
go build -o bin/craze-fake-agent ./cmd/craze-fake-agent
```

## Project Structure

```
craze/
├── cmd/
│   ├── craze/                # CLI entrypoint
│   ├── craze-fake-agent/     # Scripted ACP server for tests
│   └── craze-fake-host/      # Scripted control-socket server for wire fixtures
├── internal/
│   ├── acp/                  # JSON-RPC client, spawn, framing
│   ├── agent/                # Session, events, tools, todos; provider adapters
│   ├── atomicfile/           # Lock and atomic write for on-disk stores
│   ├── backend/              # The TUI's seam onto a session
│   ├── caplog/               # Size-capped log file with one rotation
│   ├── chatgptauth/          # Sign in with ChatGPT: tokens, model list
│   ├── cli/                  # Cobra commands: TUI, prompt, ps, new, send, wait,
│   │                         #   cancel, stop, providers, auth, bridge, attach,
│   │                         #   serve; hidden frame and hub
│   ├── control/              # The control socket's server
│   ├── engine/               # The driver every client shares
│   ├── fakehost/             # In-process twin of craze-fake-host
│   ├── harness/              # The native agent (llm, modeltable, tool, store, redact)
│   ├── host/                 # Status reports to herdr and roost
│   ├── hostspawn/            # Spawning a detached session host
│   ├── hub/                  # The per-machine hub
│   ├── journal/              # The session journal
│   ├── modelcache/           # Model catalog cache for ACP providers
│   ├── paths/                # Where craze's files live
│   ├── plugin/               # Tests for the Claude Code plugin files
│   ├── protocol/             # The control-socket wire and its JSON Schema
│   ├── remote/               # The control socket's Go client
│   ├── roster/               # The session list's data
│   ├── rundir/               # Runtime and cache directories, registry, locks
│   ├── sessions/             # The session index
│   ├── signinlog/            # The ChatGPT sign-in log
│   ├── textdiff/             # Diff rendering for edit tools
│   ├── transcript/           # Render-free transcript model
│   ├── tui/                  # Bubbletea screen
│   └── version/              # Build/version metadata
├── agents/, skills/, .claude-plugin/  # The Claude Code plugin
├── scripts/
│   ├── release/              # Version bump for a release
│   └── verify/               # Reviewed verification helpers (below)
├── tests/cli/                # Python CLI suite (uv + pytest)
├── eval/                     # Native harness evaluation (Python uv project)
├── discovery/                # Design notes behind the larger features
├── .claude/skills/           # Repo-local agent skills (craze-live-smoke)
├── docs/                     # Documentation
├── RELEASING.md              # How a release is cut
├── zensical.toml
└── go.mod
```

## Per-commit gate

```bash
make lint && make test && make test-race && make build && make test-cli
```

Do not pipe gate commands through `| tail`.

Docs are not in that gate. See [Documentation](#documentation) and
[Testing](testing.md).

## Verification helpers

`scripts/verify/` holds the reviewed helpers a change is checked with, run on a
committed sha rather than the working tree. See [Testing](testing.md#verification-scripts).
A live check against the real `cursor-agent`, `grok`, `gx` or native, in tmux
or on a second machine, follows the `craze-live-smoke` skill in
`.claude/skills/craze-live-smoke/`. How a release is cut is in `RELEASING.md`
at the repository root.

## Documentation

The documentation site is built with [Zensical](https://zensical.org),
configured in `zensical.toml`.

```bash
make docs         # same as CI: uv sync --locked && zensical build --strict
make docs-serve   # preview on :7070 (all interfaces)
```

Or without make:

```bash
uv sync --locked --group docs
uv run --locked zensical serve
uv run --locked zensical build --strict
```
