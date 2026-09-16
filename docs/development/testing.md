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

### Queue scripts

The 008 scripts are the long turns the message queue needs: a turn has to
outlive several frames before anything can be typed into it.

| Script | What it proves |
|--------|----------------|
| `long-turn` | Cursor: two execute tools, then `DONE step1 step2`. A second `session/prompt` during a turn answers the first `cancelled` and runs instead — the live cursor wire craze's queue exists to avoid, reachable only through a raw `acp.Conn` |
| `grok-long-turn` | The same over the grok dialect, with `x.ai/queue/changed` at turn start (which is where craze learns its own `promptId`) and `x.ai/interject` merged at the next tool result: the reply reads `DONE step1 <interjection> step2` |
| `grok-long-turn-fallback` | Interjections are never merged: each becomes grok's own `interject-fallback-…` turn once the session is idle, announced by `queue/changed` and ended by `turn_completed` **alone** — no `prompt_complete`, no trailing `queue/changed`, exactly as the live capture shows. A prompt whose text contains `STRAND-INTERJECTION` makes the fake strand one of its own as the turn ends, which is how the headless drain's wait is driven without craze interjecting |

Sanitized excerpts of those live captures are committed under
`internal/acp/testdata/grok-queue/` (`fallback.jsonl` is the whole
interject-fallback turn, `interject.jsonl` a merged one, `queue-changed.jsonl`
the queue broadcast) and are replayed through a real client.

Cursor's `task` / `task-late` / `tasks` scripts cover the receipt-only path:
the session synthesizes the same sub-agent lifecycle from the `cursor/task`
receipt, and the TUI view shows what the receipt carried.

Sanitized excerpts of the live captures are committed under
`internal/acp/testdata/grok-subagent/` (`subagent.jsonl`, `two.jsonl`,
`cancel-late.jsonl` for a cancel while the child still runs, `cancel-early.jsonl`
for a cancel after the child completed) and are what the ACP parser tests and
a replay through the client run against, so the repo
does not depend on the capture directory.

### Plugin scripts

The fake's `commands` script advertises a full catalog from `session/new`
(the same 24-entry catalog the slash-menu goldens use), so a plugin row
resolves on turn one; `nocommands` never sends `available_commands_update`
at all, so a session against it stays in the provisional window — every
plugin row qualified — for its whole life, which is the one state a real
Cursor session only holds for a few seconds and no golden could otherwise
capture. `callorder` is `echo` with a receipt: a second text block naming
every `session/prompt` and `session/cancel` the fake has read so far, in
arrival order, which is how `internal/agent/expand_test.go`'s catalog-wait
tests prove `Cancel` aborts the wait rather than racing the next queued
prompt.

Fixture plugins live in two places: `tests/cli/fixtures/probe-plugin/` (one
command, `probe-echo`, that spends its arguments, and one skill,
`probe-skill`, that does not) is what the Python suite and the manual JSON
checks drive; `internal/tui/testdata/plugins/` carries the same probe
plugin for the Go goldens plus `alpha` and `beta`, which each ship a
`rescue` command so a name collision — and the qualified spelling it
forces — has a fixture of its own.

Every test that starts a session or runs a binary isolates `HOME`: craze
walks the plugin caches under `HOME` at session start, so a test that
didn't would see whatever a developer happens to have installed. In Go
that's `t.Setenv("HOME", t.TempDir())` in the test's own setup
(`internal/agent/session_test.go`, `internal/cli/json_test.go`,
`internal/cli/frame_test.go`, `internal/cli/tui_test.go`, …); in the Python
suite it's the autouse `isolate_run_env` fixture in `tests/cli/conftest.py`.

To drive a plugin command through the JSON interface:

```bash
CRAZE_FAKE_SCRIPT=commands ./bin/craze prompt --json \
  --agent-bin ./bin/craze-fake-agent \
  --plugin-dir tests/cli/fixtures/probe-plugin \
  "/probe-echo banana"
```

and through `craze frame`:

```bash
./bin/craze frame --cols 100 --rows 30 \
  --agent-bin ./bin/craze-fake-agent --fake-script commands \
  --plugin-dir tests/cli/fixtures/probe-plugin \
  --keys "/probe-echo banana<enter><wait:text:PROBE-COMMAND-EXPANDED>"
```

Both isolate `HOME` themselves in the test suites above; run by hand like
this, they read whatever plugins your own `HOME` happens to have cached in
addition to the fixture.

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
| `--plugin-dir` | Extra plugin directory whose commands and skills craze expands (repeatable); resolved against the current directory, since `craze frame` has no `--workspace` |
| `--seed-session` | Write one session-index row before the model is built, as `provider:id:title` (repeatable; the title may contain `:`) |
| `--continue` | Load the newest seeded row, the way `craze --continue` does |
| `--resume` | Open the resume picker over the seeded rows |

There is no `--workspace`: `craze frame` runs in the current directory. It
never reads `~/.craze/config.toml`, so a saved theme cannot reach a golden.

`--seed-session` rows are written **inside** the isolated `HOME`, and
`--continue` / `--resume` resolve against that isolated index, so a replay or
picker golden can never see — or touch — a developer's real
`~/.craze/sessions.jsonl`.

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
<hover:X,Y>
<paste:one\ntwo>
<wait:idle> <wait:working> <wait:card> <wait:copied> <wait:text:foo> <wait:gone:foo>
```

`<motion:X,Y>` stamps the left button, so it models a drag. `<hover:X,Y>` is
motion with nothing held, which is one of the two things a queued message's
`[send now] [edit] [cancel]` strip appears on — the other is the row the
keyboard has selected.

`--freeze` stops the clock and the spinner cycle for the whole run. A frame of
a turn in progress is otherwise a race against wall time: the elapsed counter
and the spinner glyph both move on their own, so the same script captures a
different frame on a slower build. Every golden of a running turn uses it.

`CRAZE_FAKE_STEP` sets how long each of the fake's `long-turn` tool steps
takes. It is a comma-separated list, one entry per turn with the last
repeating: `2s,1ms` is a first turn slow enough to type into and later ones
that finish at once, which is what a frame of a *drained* queue needs.

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
dialog, the sub-agent view (both providers), a real mouse drag, the queue band
(`queue`) and a grok interjection (`grok-interject`). It is
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
