# CLAUDE.md

Working conventions for agent sessions in this repo.

## What this is

craze is a Linux and macOS TUI that drives `cursor-agent`, `grok`, or `gx` (a
third-party fork of the Grok CLI) over ACP (Agent Client Protocol). It is a
proof of concept. Multi-project harness integration is later work.

## Per-commit gate

Run before every commit:

```shell
make lint && make test && make build
```

Once `tests/cli/pyproject.toml` exists, also run `make test-cli` (and CI will).
Do not pipe gate commands through `| tail`.

CI runs `test` and `cli` on both `ubuntu-latest` and `macos-latest`; `lint`,
`build`, and `release-snapshot` stay ubuntu-only (see the comment atop
`.github/workflows/ci.yml` for why that split is safe). The mac-mini is the
live-smoke box for macOS: real `cursor-agent`/`grok` sessions driven in tmux,
outside CI. Toolchain is [mise](https://mise.jdx.dev/) (`.mise.toml`); the
Makefile prepends `~/.local/share/mise/shims` so `make` works without an
activated shell.

## Docs

Docs are **not** in the per-commit gate. Run this only when touching `docs/`,
`zensical.toml`, `pyproject.toml`, `uv.lock`, or the docs workflows:

```shell
make docs
```

That is `uv sync --locked --group docs && uv run --locked zensical build --strict`.
Preview with `make docs-serve` (binds `0.0.0.0:7070`).

Gotchas:

1. Zensical **silently ignores unknown config keys** even under `--strict`.
2. Emoji callables must be `zensical.extensions.emoji.*`, not `material.extensions.emoji.*`.

## Plans

Panel-reviewed plans live outside this repo and are not committed.
