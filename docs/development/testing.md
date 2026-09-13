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

### Sub-agent scripts

The `grok-subagent*` fake scripts replay the wire shapes captured from a live
grok run — snake_case `_x.ai/session_notification` with `attempt_id`,
`update._meta["x.ai/tool"]`, and child session ids on the same stdio:

| Script | What it proves |
|--------|----------------|
| `grok-subagent` | One explore child end to end: spawn-tool join, the child's own user/thought/tool/text stream, progress, finish, wait-tool retitle |
| `grok-subagent-fail` | The same child finishing `failed` with an error |
| `grok-subagent-two` | Two interleaved children with colliding child tool call ids; an update for an unknown child is dropped; a duplicate `spawned` is a no-op |
| `grok-subagent-nested` | A grandchild spawned on the child's session registers flat and joins inside the child's tools |
| `grok-subagent-late` | The child still running at `prompt_complete`; its finish lands 400 ms later, so `done` is not EOF and the drain must be state-aware |
| `grok-subagent-cancel` | Cancel with a running child: `prompt_complete{cancelled}` first, `subagent_finished{cancelled}` 400 ms later |
| `grok-subagent-cancel-early` | The other live cancel order: `finished{completed}` before `prompt_complete{cancelled}` |

Cursor's `task` / `task-late` / `tasks` scripts cover the receipt-only path:
the session synthesizes the same sub-agent lifecycle from the `cursor/task`
receipt, and the TUI view shows what the receipt carried.

Sanitized excerpts of the live captures are committed under
`internal/acp/testdata/grok-subagent/` (`subagent.jsonl`, `two.jsonl`,
`cancel.jsonl`) and are what the ACP parser tests run against, so the repo
does not depend on the capture directory.

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
<press:X,Y> <motion:X,Y> <release:X,Y> <drag:X1,Y1,X2,Y2> <dblclick:X,Y>
<paste:one\ntwo>
<wait:idle> <wait:working> <wait:card> <wait:copied> <wait:text:foo> <wait:gone:foo>
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
and 80x24, one run per case: every fake script, plus cases for the model
dialog, the sub-agent view (both providers), and a real mouse drag. It is
opt-in and never runs in CI — pytest only collects it when it is named
explicitly, and it skips unless `tmux` is on `PATH` and `CRAZE_TMUX` is set.

```bash
cd tests/cli && CRAZE_TMUX=1 uv run pytest -v tmux_smoke.py
cd tests/cli && CRAZE_TMUX=1 uv run python tmux_smoke.py --cases echo,todos
```

Screen captures land in `./smoke-captures/` (git-ignored), overridden with
`--out` or `CRAZE_SMOKE_OUT`.

The suite is isolated from your machine: its own tmux socket (`-L`), its own
configuration (`-f /dev/null`, so no `~/.tmux.conf` hook can reach the panes),
its own `HOME`, and stub `wl-copy`/`xclip`/`xsel` on the pane's `PATH` so a copy
never touches your real clipboard.
