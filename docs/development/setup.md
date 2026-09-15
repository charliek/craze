# Development Setup

## Prerequisites

- Linux or macOS
- Go 1.24 or later
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
│   └── craze-fake-agent/     # Scripted ACP server for tests
├── internal/
│   ├── acp/                  # JSON-RPC client, spawn, framing
│   ├── agent/                # Session, events, tools, todos
│   ├── cli/                  # Cobra commands: TUI, prompt, frame
│   ├── textdiff/             # Diff rendering for edit tools
│   ├── tui/                  # Bubbletea screen
│   └── version/              # Build/version metadata
├── tests/cli/                # Python CLI suite (uv + pytest)
├── docs/                     # Documentation
├── zensical.toml
└── go.mod
```

## Per-commit gate

```bash
make lint && make test && make build
```

Once `tests/cli/pyproject.toml` exists, also run `make test-cli` (and CI will).
Do not pipe gate commands through `| tail`.

Docs are not in that gate. See [Documentation](#documentation) and
[Testing](testing.md).

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
