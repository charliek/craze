# craze

A Linux and macOS terminal UI that talks ACP to **Cursor** (`cursor-agent acp`),
**Grok** (`grok agent stdio`), or **gx** — a third-party fork of the Grok
CLI. You own the chrome; the provider still runs the agent.

## Install

Either way, you also need the [Cursor CLI](https://cursor.com/cli), the
[Grok CLI](https://docs.x.ai/build/cli/headless-scripting) (`grok`), or
[`gx`](https://github.com/charliek/grok-build) installed and logged in:
`cursor-agent login` (also installed as `agent`), or `grok login` / set
`XAI_API_KEY`. `gx` is a third-party fork of `grok-build` that speaks the
same ACP dialect as `grok`, so everything here about Grok's behaviour
applies to it too; it currently shares grok's `~/.grok` home (config, auth,
sessions, skills) — that is the fork's current behaviour, not a craze
guarantee.

### Homebrew (macOS, Apple Silicon and Linux amd64/arm64) — available from v0.0.1

```shell
brew install charliek/tap/craze
```

Intel macOS (`darwin/amd64`) is cross-compiled and shipped but not tested at
runtime; treat it as best-effort. Apple Silicon and Linux (amd64/arm64) are
tested.

### apt (Ubuntu 24.04+ and derivatives, amd64/arm64) — available from v0.0.1

Add the repo once:

```bash
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://apt.stridelabs.ai/pubkey.gpg | \
  sudo tee /etc/apt/keyrings/apt-charliek.gpg > /dev/null
echo 'deb [signed-by=/etc/apt/keyrings/apt-charliek.gpg] https://apt.stridelabs.ai noble main' | \
  sudo tee /etc/apt/sources.list.d/apt-charliek.list
sudo apt update
```

Then:

```shell
sudo apt install craze
```

### Direct `.deb` — available from v0.0.1

Download the asset for your architecture from the
[GitHub Release](https://github.com/charliek/craze/releases) — named
`craze_<version>_<arch>.deb` (no `linux` segment in the name) — then:

```shell
sudo apt install ./craze_<version>_<arch>.deb
```

### From source

Source installs require Go 1.27 or later on `PATH`; verify with `go version`.
For a checkout, `mise install` installs the repo-pinned Go 1.27.1, and `make`
uses its mise shim without shell activation.

```shell
go install github.com/charliek/craze/cmd/craze@latest
```

`go install` writes the binary to `$GOBIN`, or `$GOPATH/bin` when that is
unset — `$HOME/go/bin` by default — so with that directory on your `PATH`
the command is `craze`.

### `craze version`

Homebrew, the `.deb`, and the release archives all report the release tag.
`go install .../craze@v0.0.1` (and `@latest`) also reports `0.0.1`, because
the source default in `internal/version/version.go` is bumped as part of
cutting that release. `go install .../craze@main`, though, reports the
**last released version**, not `dev` — it reads that same bumped source
default, so it lags behind whatever landed on `main` since. Prefer `@latest`
over `@main` for that reason. `make build` (building from a checkout) always
reports `dev`.

## Build from source

```shell
git clone https://github.com/charliek/craze.git
cd craze
make build
./bin/craze version
./bin/craze prompt --json --agent-bin ./bin/craze-fake-agent "hello"
```

## Running

`./bin/craze` below is the build-from-source path; an installed craze is on your
`PATH`, so drop the `./bin/`.

```shell
./bin/craze                      # picker, then the TUI in the current directory
./bin/craze --provider cursor    # skip the picker
./bin/craze --provider grok
./bin/craze --provider gx
./bin/craze --workspace ../proj  # somewhere else
./bin/craze --no-force           # ask before each tool call
./bin/craze --theme gruvbox --no-mouse
./bin/craze --continue           # reload the newest session in this workspace
./bin/craze --resume             # pick one of the last 10 sessions here
```

Without `--provider`, the TUI shows a picker listing `cursor`, `grok`, and
`gx` — gx only when a binary for it resolves — preselected to
`$CRAZE_PROVIDER`, then `provider` in `~/.craze/config.toml`, then cursor.
`Enter` starts that row; `Esc` starts the preselected default. `--provider`
skips the picker; `--provider gx` works regardless of whether a binary
resolves, and fails at spawn if it's missing. The last **successful** Start
is saved and used next time.

`--force` (yolo) is the default. `--no-force` turns on the permission line.
`--model` picks an ACP model id, `--ask` / `--plan` set the session mode, and
`--agent-bin` (or `CRAZE_AGENT_BIN`) overrides the binary. Cursor looks up
`cursor-agent` then `agent`; Grok looks up `grok` only; gx looks up `gx`
only.

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
| `Enter` | send; queue the draft while a turn is running |
| `Ctrl+L` | the strong send: on Grok, add the draft to the running turn without cancelling it; on Cursor, cancel the running turn and send (it asks first) |
| `Alt+Enter`, `Ctrl+J` | newline (see below) |
| `Esc` | answer the card on top; close a dialog (`/help` included); leave the sub-agent view; close the slash menu; otherwise cancel the running turn (the transcript says `cancelled`) |
| `Ctrl+C` | cancel the running turn and everything queued behind it; a second press within one second quits; quits outright when idle or after an error. Inside the sub-agent view it still cancels the **main** turn, and the view stays open |
| `Ctrl+D` | quit, always |
| `Shift+Tab` | cycle the ACP mode (agent / plan / ask); inside the sub-agent view, switch to the previous sub-agent instead |
| `Ctrl+T`, `/tasks` | tasks panel: compact → expanded → hidden |
| `Ctrl+G`, `/theme` | theme picker |
| `Ctrl+O` | expand / collapse transcript detail (diff hunks, command output, thoughts) — works inside the sub-agent view too |
| `Ctrl+Y` | copy the mouse selection, or the last reply when there is none (works with `--no-mouse`); inside the sub-agent view, copy from it |
| `↑` `↓` | move the keyboard out of the composer (draft or not) and then between rows: `↑` reaches the queued messages first and the sub-agent rows when nothing is queued, `↓` the other way round; the selected row carries a `❯` gutter mark; `↑` past the first row, `Esc`, or typing anything returns to the composer; inside the sub-agent view, scroll it; in a dialog or the slash menu, move the cursor |
| `Enter` while the sub-agent rows have the keyboard | open it in the main area, read-only: its own transcript on Grok, what the task receipt carried on Cursor |
| `Enter` / `Backspace` / `Ctrl+L` on a queued message | edit it in place / cancel it / send it now |
| `Esc` or `←` inside the sub-agent view | return to the main transcript; entering or leaving cancels nothing |
| `Tab` inside the sub-agent view | switch to the next sub-agent |
| `PgUp` / `PgDn`, wheel | scroll the transcript, or page the `/help` box |
| `Tab` / `Enter` on the slash menu | accept the highlighted row, replacing just the `/token` under the cursor; `Enter` on a name already typed in full runs or sends it instead |
| `PgUp` / `PgDn`, wheel, click on the slash menu | page it, move the selection one row a notch, or accept the row clicked |
| `Esc` on the slash menu | hide it for that token only; a second `Esc` cancels a running turn as usual |

`/help` opens a centred, scrollable box listing the same keys — grouped, one
per row — plus every slash command. `/exit` quits; there are no bare `q` or `?`
bindings, so a message that starts with either is just a message.

**Shift+Enter is unreliable under bubbletea v1.** Most terminals send a bare
`Enter` for it, and craze cannot tell the two apart, so it sends the message.
`Alt+Enter` and `Ctrl+J` are the newline keys that always work; `Shift+Enter`
is bound as well, for the terminals that do report it distinctly. `Ctrl+Enter`
is the same story, which is why the strong send is `Ctrl+L`.

**Queued messages.** While a turn runs, `Enter` queues instead of refusing.
The queued messages show above the spinner as `#1`, `#2`, … with
`[send now] [edit] [cancel]` on the row under the pointer, and one is sent per
settled turn. `Esc` cancels the turn and lets the queue continue; `Ctrl+C`
stops everything. See [docs/reference/tui.md](docs/reference/tui.md) for the
whole thing.

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
`dark`, `light`, `catppuccin`, `gruvbox`. While craze runs it sets your
terminal's own default background and text colours to the theme's and
restores them when it exits; `background = false` in `~/.craze/config.toml`,
or `--no-background`, leaves your terminal's colours alone.

Inside a herdr pane or a roost tab, craze also reports its state — idle,
working, or blocked on a card or an error — to the host, which shows it like
any other agent's. `host_status = false`, or `--no-host-status`, turns that off.

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
row 2, opens the model dialog from the model name in status row 1, or opens a
sub-agent row; inside the sub-agent view, a click on the banner returns.

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
cases for the model dialog, the sub-agent view (both providers), and a real
mouse drag. It is opt-in and never runs in CI — pytest only collects it when it
is named explicitly, and it skips unless `tmux` is on PATH and `CRAZE_TMUX` is
set.

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

POC on Linux and macOS: `craze` opens a TUI; `craze prompt --json` is the headless path.
Multi-project harness integration is later work.

## License

MIT. See [LICENSE](LICENSE).
