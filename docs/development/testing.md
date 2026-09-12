# Testing

```bash
make lint && make test && make build && make test-cli
```

Everything below that talks to an agent uses `craze-fake-agent`, so no Cursor
credentials are needed.

## Go tests

```bash
make test
```

Or without make:

```bash
go test -timeout 5m -v ./...
```

Frame goldens live in `internal/tui/testdata/` and render through the same TUI
model as a live session. CI also runs `-race` on `internal/acp`,
`internal/agent`, and `internal/tui`.

## craze frame

`craze frame` is a hidden command that runs the real TUI model with no
terminal attached, feeds it a key script, and prints the final frame. It is
what the goldens render against.

```bash
./bin/craze frame --cols 100 --rows 30 \
  --agent-bin ./bin/craze-fake-agent --fake-script todos \
  --keys "go<enter><wait:text:TASKS>"
```

| Flag | Description |
|------|-------------|
| `--cols` | Terminal width (default 100) |
| `--rows` | Terminal height (default 30) |
| `--agent-bin` | Path to `cursor-agent` / fake agent (or `CRAZE_AGENT_BIN`) |
| `--fake-script` | Sets `CRAZE_FAKE_SCRIPT` for the child |
| `--keys` | Key script, e.g. `go<enter><wait:text:TASKS>` |
| `--ansi` | Print the raw frame with a forced true-colour profile |
| `--theme` | TUI theme preset |
| `--no-force` | Disable yolo and handle permission requests |
| `--timeout` | Per-wait timeout (default 10s) |
| `--print-frames` | Stream every frame to stderr |

There is no `--workspace`: `craze frame` runs in the current directory. It
never reads `~/.craze/config.toml`, so a saved theme cannot reach a golden.

`craze frame` cannot drive the real `cursor-agent`. It points `HOME` at an
empty directory for the duration of the run — that is what keeps a developer's
config and skills out of a golden — and `cursor-agent` loses its credentials
along with it. Use `./bin/craze-fake-agent`. To drive a live agent in a
terminal, use the [tmux smoke](#tmux-smoke) or just run `craze`.

In a key script, literal text is typed rune by rune and anything in angle
brackets is a token:

```text
<enter> <esc> <tab> <backspace> <space> <up> <down> <left> <right>
<pgup> <pgdn> <shift-tab> <alt-enter> <ctrl-a>..<ctrl-z> <lt>
<wheel-up> <wheel-down> <click:X,Y> <resize:COLS,ROWS> <sleep:250ms>
<wait:idle> <wait:working> <wait:card> <wait:text:foo> <wait:gone:foo>
```

| Exit | Meaning |
|------|---------|
| 0 | Prints the final frame |
| 2 | Bad script |
| 3 | A wait timed out (the last frame and the wait it was stuck on go to stderr) |

## Python CLI suite

`make test-cli` runs the Python suite in `tests/cli` under `uv`:

- `test_prompt.py` — the headless `prompt --json` path
- `test_frame.py` — `craze frame` through the binary, for the same scripts the
  Go goldens cover
- `test_tui.py` — a real PTY

```bash
make test-cli
```

## tmux smoke

`tests/cli/tmux_smoke.py` drives the real binary in a real terminal at 100x30
and 80x24 for every fake script. It is opt-in and never runs in CI — pytest
only collects it when it is named explicitly, and it skips unless `tmux` is on
`PATH` and `CRAZE_TMUX` is set.

```bash
cd tests/cli && CRAZE_TMUX=1 uv run pytest -v tmux_smoke.py
cd tests/cli && CRAZE_TMUX=1 uv run python tmux_smoke.py --scripts echo,todos
```

Screen captures land in `~/.cursor/plans/craze/004-harness-tui/smoke/`
(`--out` or `CRAZE_SMOKE_OUT` moves them).
