# craze

A Linux terminal UI that talks ACP to **Cursor** (`cursor-agent acp`) or
**Grok** (`grok agent stdio`). You own the chrome; the provider still runs
the agent.

## Install

1. Linux, and either the [Cursor CLI](https://cursor.com/cli) or the
   [Grok CLI](https://docs.x.ai/build/cli/headless-scripting) (`grok`) installed.
2. Log in: `cursor-agent login` (also installed as `agent`), or `grok login`
   / set `XAI_API_KEY`.
3. Install craze:

   ```shell
   go install github.com/charliek/craze/cmd/craze@main
   ```

   `@main` pins the branch explicitly. `go install` writes the binary to
   `$GOBIN`, or `$GOPATH/bin` when that is unset — `$HOME/go/bin` by default —
   so with that directory on your `PATH` the command is `craze`:

   ```shell
   craze version
   ```

   It reports `dev` until the first tag lands.

## Build from source

```shell
git clone https://github.com/charliek/craze.git
cd craze
make build
./bin/craze version
./bin/craze prompt --json --agent-bin ./bin/craze-fake-agent "hello"
```

Needs Go 1.24+ (this repo pins 1.24 via `.mise.toml`; `mise install`).

## Running

`./bin/craze` below is the build-from-source path; an installed craze is on your
`PATH`, so drop the `./bin/`.

```shell
./bin/craze                      # picker, then the TUI in the current directory
./bin/craze --provider cursor    # skip the picker
./bin/craze --provider grok
./bin/craze --workspace ../proj  # somewhere else
./bin/craze --no-force           # ask before each tool call
./bin/craze --theme gruvbox --no-mouse
```

Without `--provider`, the TUI shows a two-row picker (`cursor` / `grok`)
preselected to `$CRAZE_PROVIDER`, then `provider` in `~/.craze/config.toml`,
then cursor. `Enter` starts that row; `Esc` starts the preselected default.
`--provider` skips the picker. The last **successful** Start is saved and
used next time.

`--force` (yolo) is the default. `--no-force` turns on the permission line.
`--model` picks an ACP model id, `--ask` / `--plan` set the session mode, and
`--agent-bin` (or `CRAZE_AGENT_BIN`) overrides the binary. Cursor looks up
`cursor-agent` then `agent`; Grok looks up `grok` only.

The screen is, top to bottom: the transcript, the pinned tasks panel, the
spinner line, the composer, two status rows, and one row per in-flight
sub-agent.

craze exits **nonzero when the session never started** — no login (`agent login`
or `grok login`), or an agent that would not come up. It still draws the TUI
and puts the error in the transcript, because that is where you can read it,
but the process tells a script that nothing ran. An error *during* a session
leaves a usable craze, so quitting out of one is an ordinary exit 0.

## Keys

| Key | Action |
|---|---|
| `Enter` | send |
| `Alt+Enter`, `Ctrl+J` | newline (see below) |
| `Esc` | close a peek, then the slash menu, then a dialog (`/help` included); answer the card on top; otherwise cancel the running turn (the transcript says `cancelled`) |
| `Ctrl+C` | cancel the running turn; a second press within one second quits; quits outright when idle or after an error |
| `Ctrl+D` | quit, always |
| `Shift+Tab` | cycle the ACP mode (agent / plan / ask) |
| `Ctrl+T`, `/tasks` | tasks panel: compact → expanded → hidden |
| `Ctrl+G`, `/theme` | theme picker |
| `Ctrl+O` | expand / collapse transcript detail (diff hunks, command output, thoughts) |
| `Ctrl+Y` | copy the mouse selection, or the last reply when there is none (works with `--no-mouse`) |
| `↑` `↓` | with an empty composer, select a sub-agent row; in a dialog or the slash menu, move the cursor (in `/help`, scroll the box) |
| `Enter` on a selected sub-agent | peek at its prompt; `Esc` closes the peek without cancelling the turn |
| `PgUp` / `PgDn`, wheel | scroll the transcript, or page the `/help` box |
| `Tab` | complete the slash command being typed |

`/help` opens a centred, scrollable box listing the same keys — grouped, one
per row — plus every slash command. `/exit` quits; there are no bare `q` or `?`
bindings, so a message that starts with either is just a message.

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

## Mouse and copy

Mouse reporting is on by default. The wheel scrolls the transcript three lines
per notch. A left click picks a row in the model or theme dialog, cycles the
tasks panel from its header, cycles the mode from the `◆ agent` chip in status
row 2, opens the model dialog from the model name in status row 1, or selects a
sub-agent row.

Dragging over the transcript draws a selection and copies it on release; a
double-click selects the word under the pointer. The status row says `copied 2
lines` (or the start of the one line) for two seconds, and the highlight stays
until the next key, the next click or the next thing the agent says. A drag that
reaches the top or bottom row of the transcript scrolls one line and keeps
going — cell-motion reporting only sends an event when the pointer changes cell,
so a pointer held still on the edge stops scrolling; nudge it to continue.

`Ctrl+Y` copies the selection, or the last reply when there is none. It is a
keyboard feature, so it works with `--no-mouse` too.

A copy is written twice: as an OSC 52 escape sequence, which is the one that
works over ssh, and to the system clipboard. For OSC 52 to reach the system
clipboard:

- **tmux** needs `set -g set-clipboard on` (craze sends the bare sequence, so
  `allow-passthrough` is not needed).
- Some terminals have to be told to allow it — in **xterm**,
  `XTerm*disallowedWindowOps: 20,21,SetXprop`; **Alacritty**, **kitty**,
  **WezTerm**, **foot** and **iTerm2** allow it by default. **GNOME Terminal**
  does not support OSC 52 at all, which is what the system-clipboard write is
  for.

Copies are capped at 64 KiB; past that the note says `… (truncated)`.

**Turning mouse reporting on takes native drag-select away from the terminal.**
craze's own selection replaces it. To use the terminal's instead — to select
across the whole scrollback, for instance — hold `Shift` while dragging
(`Option` in macOS terminals), or start craze with `--no-mouse`.

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
<press:X,Y> <motion:X,Y> <release:X,Y> <drag:X1,Y1,X2,Y2> <dblclick:X,Y>
<paste:one\ntwo>
<wait:idle> <wait:working> <wait:card> <wait:copied> <wait:text:foo> <wait:gone:foo>
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
a real terminal at 100x30 and 80x24, one run per case: every fake script, plus
cases for the model dialog and for a real mouse drag. It is opt-in and
never runs in CI — pytest only collects it when it is named explicitly, and it
skips unless `tmux` is on PATH and `CRAZE_TMUX` is set.

```shell
cd tests/cli && CRAZE_TMUX=1 uv run pytest -v tmux_smoke.py
cd tests/cli && CRAZE_TMUX=1 uv run python tmux_smoke.py --cases echo,todos
```

Screen captures land in `./smoke-captures/` (git-ignored), overridden with
`--out` or `CRAZE_SMOKE_OUT`.

## Documentation

Published site: https://charliek.github.io/craze/

Sources live under `docs/`:

- [Quick Start](docs/getting-started/quick-start.md)
- [CLI](docs/reference/cli.md)
- [TUI](docs/reference/tui.md)
- [Configuration](docs/reference/configuration.md)
- [Development](docs/development/setup.md)

```shell
make docs         # same as CI: zensical build --strict
make docs-serve   # preview on :7070 (all interfaces)
```

Documentation is automatically published to GitHub Pages on push to main.

## Status

POC on Linux: `craze` opens a TUI; `craze prompt --json` is the headless path.
Multi-project harness integration is later work.

## License

MIT. See [LICENSE](LICENSE).
