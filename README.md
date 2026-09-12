# craze

A Linux terminal UI for [Cursor CLI](https://cursor.com/cli), talking to
`cursor-agent acp` over ACP. You own the chrome; Cursor still runs the agent.

## Requirements

- Linux
- Go 1.24+ (this repo pins 1.24 via `.mise.toml`; `mise install`)
- A working Cursor login: `agent login`

## Build

```shell
make build
./bin/craze version
./bin/craze prompt --json --agent-bin ./bin/craze-fake-agent "hello"
```

## Running

```shell
./bin/craze                      # the TUI, in the current directory
./bin/craze --workspace ../proj  # somewhere else
./bin/craze --no-force           # ask before each tool call
./bin/craze --theme gruvbox --no-mouse
```

`--force` (yolo) is the default. `--no-force` turns on the permission line.
`--model` picks an ACP model id, `--ask` / `--plan` set the session mode, and
`--agent-bin` (or `CRAZE_AGENT_BIN`) points at `cursor-agent` or, for tests,
`./bin/craze-fake-agent`.

The screen is, top to bottom: the transcript, the pinned tasks panel, the
spinner line, the composer, two status rows, and one row per in-flight
sub-agent.

craze exits **nonzero when the session never started** — no Cursor login, or a
`cursor-agent` that would not come up. It still draws the TUI and puts the
error in the transcript, because that is where you can read it, but the process
tells a script that nothing ran. An error *during* a session leaves a usable
craze, so quitting out of one is an ordinary exit 0.

## Keys

| Key | Action |
|---|---|
| `Enter` | send |
| `Alt+Enter`, `Ctrl+J` | newline (see below) |
| `Esc` | close a peek, then the slash menu, then a picker or `/help`; answer the card on top; otherwise cancel the running turn (the transcript says `cancelled`) |
| `Ctrl+C` | cancel the running turn; a second press within one second quits; quits outright when idle or after an error |
| `Ctrl+D` | quit, always |
| `Shift+Tab` | cycle the ACP mode (agent / plan / ask) |
| `Ctrl+T`, `/tasks` | tasks panel: compact → expanded → hidden |
| `Ctrl+G`, `/theme` | theme picker |
| `Ctrl+O` | expand / collapse transcript detail (diff hunks, command output, thoughts) |
| `↑` `↓` | with an empty composer, select a sub-agent row; in a picker or the slash menu, move the cursor |
| `Enter` on a selected sub-agent | peek at its prompt; `Esc` closes the peek without cancelling the turn |
| `PgUp` / `PgDn`, wheel | scroll the transcript |
| `Tab` | complete the slash command being typed |

`/help` lists the same keys plus every slash command. `/exit` and `/quit` quit;
there are no bare `q` or `?` bindings, so a message that starts with either is
just a message.

**Shift+Enter is unreliable under bubbletea v1.** Most terminals send a bare
`Enter` for it, and craze cannot tell the two apart, so it sends the message.
`Alt+Enter` and `Ctrl+J` are the newline keys that always work; `Shift+Enter`
is bound as well, for the terminals that do report it distinctly.

**`Ctrl+G` is BEL.** Some terminals flash or beep when it is pressed. `/theme`
opens the same picker without the BEL.

### Cards

A blocking request from the agent is a card, and the card owns the keyboard and
the mouse while it is up: `Ctrl+T`, `Ctrl+G`, `Ctrl+O`, the arrows and the wheel
all do nothing until it is answered. `Ctrl+C` and `Ctrl+D` still work. Cards
queue, and only the one on top is drawn.

| Card | Keys |
|---|---|
| permission line — `permission <tool>  [a]llow once  [A]lways  [n] reject` | `a` allow once, **`A` allow always**, `n` reject. Only the options the request actually offered are drawn and bound. `Esc` cancels the turn. |
| question card | `1`–`9` pick, `↑` `↓` move, `Enter` selects (single-choice) or confirms (multiple-choice), `Space` toggles a multiple-choice option. `Esc` **skips** the whole request. |
| plan card — `plan <name>  [a]ccept  [r]eject  esc cancel` | `a` accept, `r` reject. `Esc` cancels the turn. |

`Esc` means two different things on purpose. On a question it is *skipped*: the
agent is told you declined to answer and the turn carries on. On a plan or a
permission it cancels the turn, because cancelling is the only way those two
reach the agent as anything other than an accept or a reject.

## Themes

Seven presets: `craze-dark` (the default), `craze-light`, `tokyo-night`,
`dark`, `light`, `catppuccin`, `gruvbox`. craze never paints a full-screen
background, so your terminal's own background shows through.

`Ctrl+G` or `/theme` opens a names-only picker that repaints the live screen as
the cursor moves; `Enter` keeps the theme and saves it, `Esc` puts back the one
that was active when the picker opened. `/theme <name>` sets and saves one
directly.

The saved theme lives in `~/.craze/config.toml` (`CRAZE_CONFIG` overrides the
path). Unrelated keys in that file are preserved; a file craze cannot parse is
never clobbered, and the save that refused to overwrite it is reported in the
transcript. An explicit `--theme` beats the config file, which beats
`craze-dark`.

## Mouse

Mouse reporting is on by default. The wheel scrolls the transcript three lines
per notch; a left click picks a row in the model or theme picker, cycles the
tasks panel from its header, or selects a sub-agent row.

**Turning mouse reporting on takes native drag-select away from the terminal.**
To select text anyway, hold `Shift` while dragging (`Option` in macOS
terminals), or start craze with `--no-mouse`.

## `craze frame` — the headless renderer

`craze frame` is a hidden command that runs the real TUI model with no terminal
attached, feeds it a key script, and prints the final frame. It is what the
frame goldens in `internal/tui/testdata/` render against.

```shell
./bin/craze frame --cols 100 --rows 30 \
  --agent-bin ./bin/craze-fake-agent --fake-script todos \
  --keys "go<enter><wait:text:TASKS>"
```

Flags: `--cols`, `--rows`, `--agent-bin`, `--fake-script` (sets
`CRAZE_FAKE_SCRIPT` for the child), `--keys`, `--ansi` (print the raw frame with
a forced true-colour profile), `--theme`, `--no-force`, `--timeout` (per wait,
default 10s) and `--print-frames` (stream every frame to stderr). There is no
`--workspace`: `craze frame` runs in the current directory. It never reads
`~/.craze/config.toml`, so a saved theme cannot reach a golden.

**`craze frame` cannot drive the real `cursor-agent`.** It points `HOME` at an
empty directory for the duration of the run — that is what keeps a developer's
config and skills out of a golden — and `cursor-agent` loses its credentials
along with it, so `Start` never returns and the run dies on the implicit
`<start>` barrier after the full `--timeout`. Use it with
`./bin/craze-fake-agent`; to drive a live agent in a terminal, use the tmux
smoke below or just run `craze`.

In a key script, literal text is typed rune by rune and anything in angle
brackets is a token:

```
<enter> <esc> <tab> <backspace> <space> <up> <down> <left> <right>
<pgup> <pgdn> <shift-tab> <alt-enter> <ctrl-a>..<ctrl-z> <lt>
<wheel-up> <wheel-down> <click:X,Y> <resize:COLS,ROWS> <sleep:250ms>
<wait:idle> <wait:working> <wait:card> <wait:text:foo> <wait:gone:foo>
```

Exit 0 prints the final frame, 2 is a bad script, and 3 is a wait that timed
out (the last frame and the wait it was stuck on go to stderr).

## Tests

```shell
make lint && make test && make build && make test-cli
```

`make test-cli` runs the Python suite in `tests/cli` under `uv`: `test_prompt.py`
(the headless `prompt --json` path), `test_frame.py` (`craze frame` through the
binary, for the same scripts the Go goldens cover) and `test_tui.py` (a real
PTY). Everything uses `craze-fake-agent`, so no Cursor credentials are needed.

`tests/cli/tmux_smoke.py` is the tmux smoke layer: it drives the real binary in
a real terminal at 100x30 and 80x24 for every fake script. It is opt-in and
never runs in CI — pytest only collects it when it is named explicitly, and it
skips unless `tmux` is on PATH and `CRAZE_TMUX` is set.

```shell
cd tests/cli && CRAZE_TMUX=1 uv run pytest -v tmux_smoke.py
cd tests/cli && CRAZE_TMUX=1 uv run python tmux_smoke.py --scripts echo,todos
```

Screen captures land in `~/.cursor/plans/craze/004-harness-tui/smoke/`
(`--out` or `CRAZE_SMOKE_OUT` moves them).

## Status

POC on Linux: `craze` opens a TUI; `craze prompt --json` is the headless path.
Shed-lane integration is explicitly later work.
