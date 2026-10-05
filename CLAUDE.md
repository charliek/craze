# CLAUDE.md

Working conventions for agent sessions in this repo.

## What this is

craze is a Linux and macOS TUI that drives `cursor-agent`, `grok`, or `gx` (a
third-party fork of the Grok CLI) over ACP (Agent Client Protocol). It is a
proof of concept. Multi-project harness integration is later work.

## Per-commit gate

Run before every commit:

```shell
make lint && make test && make test-race && make build
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

## Verification beyond the gate

`scripts/verify/` holds the reviewed helpers plans run on a **committed** sha (exported to a short scratch
path, never the working tree): `gate.sh` (the gate; a concurrent run waits on golangci-lint's lock),
`cpu1.sh`, `starve.sh` (a CPU quota), `contend.sh` (copies sharing a few cores), `v1.sh` (the race soak:
`--plan`, then one `--job` per background command, then `--summary`), `v2.sh` (`craze prompt` parity against
commit `6581e0a`) and `v8.sh` (goldens, wire fixtures and schema files changed against the merge base). Each
prints its usage with `--help` and ends with a `<NAME>_EXIT=` line; output goes to
`~/.cache/craze-verify/<label>/`; `selftest.sh` checks their failure paths. A failure is diagnosed, never
re-run. For live smokes (the real cursor-agent, grok, gx or native in a dedicated tmux server, or on the
mac-mini) follow the `craze-live-smoke` skill in `.claude/skills/`.

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

## Evaluation (`eval/`)

`eval/` is the native harness's evaluation (a Python uv project, `crazeeval`); it is not
part of CI or the per-commit gate, and it spends real API money. The procedure is
`eval/RUNBOOK.md` (start with its §0 checklist); the reference is `eval/README.md`. Each
campaign's rollup is committed under `eval/results/<campaign>/` (`README.md` +
`summary.json`); the full archives live in the private repo `charliek/craze-evals`. Raw
runs stay local and are never uploaded.

## Plans

Panel-reviewed plans live outside this repo and are not committed.
