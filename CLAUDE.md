# CLAUDE.md

Working conventions for agent sessions in this repo.

## What this is

craze is a Linux TUI that drives `cursor-agent` over ACP (Agent Client Protocol).
It is a proof of concept. Shed-lane / roost integration is out of scope for the
current bootstrap.

## Per-commit gate

Run before every commit:

```shell
make lint && make test && make build
```

Once `tests/cli/pyproject.toml` exists, also run `make test-cli` (and CI will).
Do not pipe gate commands through `| tail`.

Linux only. No macOS CI. Toolchain is [mise](https://mise.jdx.dev/) (`.mise.toml`); the Makefile prepends `~/.local/share/mise/shims` so `make` works without an activated shell.

## Plans

Panel-reviewed plans live outside this repo at `~/.cursor/plans/craze/`.
The plan file is not committed here.
