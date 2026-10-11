# craze

A Linux and macOS terminal UI that talks ACP to **Cursor** (`cursor-agent acp`),
**Grok** (`grok agent stdio`), or **gx** — a third-party fork of the Grok
CLI. You own the chrome; the provider still runs the agent. Its **native**
provider runs the agent inside craze instead, against a model provider's API
or a ChatGPT plan, with no other CLI to install (see [Native
provider](#native-provider)).

Sessions run in a detached host and outlive the terminal: close the tab and the
session keeps going. `craze ps` lists this machine's sessions, `craze attach`
(or `craze -c`) rejoins one, and `craze new` starts one in the background
through a per-machine hub that starts itself and exits when idle. See [Sessions
outlive their terminal](docs/reference/cli.md#sessions-outlive-their-terminal)
and the [`craze ps`](docs/reference/cli.md#craze-ps) and [`craze
new`](docs/reference/cli.md#craze-new) reference.

A session can also run **unattended**, for no one at a terminal: `craze new
--wait` starts one, hands it a task and prints the result, and the [Claude Code
plugin](#use-craze-from-claude-code) uses it to put a craze session behind a
Claude Code sub-agent. `/btw <question>` asks a side question in a view of its
own, without adding to the session (native, Grok and gx). craze is early-stage
software: v0.1.x is released for Linux and macOS.

## Install

Either way, to run craze you need one way to reach a model. For the ACP
providers that is the [Cursor CLI](https://cursor.com/cli), the
[Grok CLI](https://docs.x.ai/build/cli/headless-scripting) (`grok`), or
[`gx`](https://github.com/charliek/grok-build) installed and logged in:
`cursor-agent login` (also installed as `agent`), or `grok login` / set
`XAI_API_KEY`. `gx` is a third-party fork of `grok-build` that speaks the
same ACP dialect as `grok`, so everything here about Grok's behaviour
applies to it too; it currently shares grok's `~/.grok` home (config, auth,
sessions, skills) — that is the fork's current behaviour, not a craze
guarantee. The native provider needs none of them, only an API key or a
ChatGPT plan sign-in (see [Native provider](#native-provider)).

Upgrading? See [Upgrading](docs/getting-started/upgrading.md): what changed
since v0.0.1 and since v0.1.0, and how to turn each change off.

### Homebrew (macOS, Apple Silicon and Linux amd64/arm64) — v0.0.1 and later

```shell
brew install charliek/tap/craze
```

Intel macOS (`darwin/amd64`) is cross-compiled and shipped but not tested at
runtime; treat it as best-effort. Apple Silicon and Linux (amd64/arm64) are
tested.

### apt (Ubuntu 24.04+ and derivatives, amd64/arm64) — v0.0.1 and later

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

### Direct `.deb` — v0.0.1 and later

Download the asset for your architecture from the
[GitHub Release](https://github.com/charliek/craze/releases) — named
`craze_<version>_<arch>.deb` (no `linux` segment in the name) — then:

```shell
sudo apt install ./craze_<version>_<arch>.deb
```

### From source

Source installs require Go 1.27 or later on `PATH`; verify with `go version`.

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

Needs Go 1.27+ (this repo pins 1.27.1 via `.mise.toml`; `mise install`).

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

Without `--provider`, the TUI shows a picker listing `cursor`, `grok`, `gx` — gx
only when a binary for it resolves — and `native`, a provider that cannot start
dimmed with why (`unavailable`, `needs setup`), preselected to
`$CRAZE_PROVIDER`, then `provider` in `~/.craze/config.toml`, then cursor.
`Enter` starts that row; `Esc` starts the preselected default when it can
start. `--provider`
skips the picker; `--provider gx` works regardless of whether a binary
resolves, and fails at spawn if it's missing. The last **successful** Start
is saved and used next time.

`--force` (yolo) is the default. `--no-force` turns on the permission line.
`--model` picks the model to start on (an ACP model id, or a native model
alias), `--effort` and `--fast` / `--no-fast` start it at an effort or speed
setting, `--plugin-dir` adds a directory of plugin commands and skills,
`--ask` / `--plan` set the session mode, and
`--agent-bin` (or `CRAZE_AGENT_BIN`) overrides the binary. [The flags
table](docs/reference/cli.md#flags) lists them all. Cursor looks up
`cursor-agent` then `agent`; Grok looks up `grok` only; gx looks up `gx`
only.

The screen is, top to bottom: the transcript, the pinned tasks panel, the
queued messages, the spinner line, the composer, two status rows, and one row
per in-flight sub-agent.

craze exits **nonzero when the session never started** — no login (`agent login`
or `grok login`), or an agent that would not come up. It still draws the TUI
and puts the error in the transcript, because that is where you can read it,
but the process tells a script that nothing ran. An error *during* a session
leaves a usable craze, so quitting out of one is an ordinary exit 0.

### Driving sessions from scripts

Besides the TUI and `craze prompt --json` (one headless turn), these commands
work on sessions without a terminal. Each has a section in the [CLI
reference](docs/reference/cli.md):

- [`craze new`](docs/reference/cli.md#craze-new) starts a session in the
  background; [`craze new --wait`](docs/reference/cli.md#craze-new-wait) also
  sends it a task and prints the result
- [`craze send`](docs/reference/cli.md#craze-send) gives a session its next
  prompt, loading it again if it has ended
- [`craze wait`](docs/reference/cli.md#craze-wait) prints a turn's result; the
  waiting commands exit with its status
  ([codes](docs/reference/cli.md#waiting-commands))
- [`craze cancel`](docs/reference/cli.md#craze-cancel) and
  [`craze stop`](docs/reference/cli.md#craze-stop) cancel a turn or end a session
- [`craze serve`](docs/reference/cli.md#craze-serve) hosts a session with no
  terminal, and [`craze bridge`](docs/reference/cli.md#craze-bridge) pumps its
  control socket over stdio for another program ([protocol](docs/reference/protocol.md))
- [`craze providers`](docs/reference/cli.md#craze-providers) says which
  providers can start here

### Native provider

`--provider native` needs no agent CLI, only a credential for one of the five
providers craze ships a catalog for:

| Provider | Id for `craze auth` | Or export |
|---|---|---|
| ChatGPT plan | `chatgpt` | none: a sign-in, not a key |
| Fireworks | `fireworks` | `FIREWORKS_API_KEY` |
| Meta | `meta` | `META_API_KEY` |
| OpenRouter | `openrouter` | `OPENROUTER_API_KEY` |
| Z.AI Coding Plan | `zai-coding-plan` | `ZHIPU_API_KEY` or `ZAI_API_KEY` |

A ChatGPT plan signs in rather than giving a key: `craze auth login chatgpt`
prints an address to open in a browser (it opens one itself on a desktop or a
Mac) and, over SSH or with the browser on another machine, takes the address the
browser lands on pasted back. See [Signing in to the ChatGPT
plan](docs/reference/cli.md#signing-in-to-the-chatgpt-plan).

```shell
./bin/craze auth login fireworks   # asks for the key without echoing it; or export FIREWORKS_API_KEY
./bin/craze auth list              # how each provider is connected
./bin/craze --provider native
```

`craze auth login` with no provider shows a list to pick from, and reads the
key from stdin's first line when stdin is not a terminal. The key is stored in
`~/.craze/native/providers.toml` (0600); an exported variable is used before
it; craze never checks a key with the provider when you store it, so a wrong
one shows on first use. `craze auth logout <provider>` removes it.

The model catalog is part of the binary: upgrading craze is how new models
arrive and retired ones go, with no edit on your side. See [Native
provider](docs/getting-started/quick-start.md#native-provider) and [Native
models and providers](docs/reference/configuration.md#native-models-and-providers).

## Use craze from Claude Code

This repository is also a Claude Code plugin: sub-agents that hand a task to a
craze session on a native, Grok or Cursor model and return its result, so
Claude Code can delegate work to those models. It needs **craze 0.2.0 or
newer** (the next release) on your `PATH`.

```text
/plugin marketplace add charliek/craze
/plugin install craze@craze
```

Then dispatch `craze:delegate` (any provider and model), or a preset: `craze:sol`,
`craze:astra`, `craze:muse`, `craze:grok`, `craze:cursor`. A craze session's tools
run in craze: Claude Code's permission rules and hooks do not apply inside it.
[Claude Code sub-agents](docs/getting-started/claude-code.md) has the agents,
examples, permissions and the differences from a native sub-agent.

## Keys

| Key | Action |
|---|---|
| `Enter` | send; queue the draft while a turn is running; on a draft starting with `!`, run it in your own shell (see [Shell mode](docs/reference/tui.md#shell-mode)) |
| `Ctrl+L` | the strong send: on Grok, gx and native, add the draft to the running turn without cancelling it; on Cursor, cancel the running turn and send (it asks first) |
| `Alt+Enter`, `Ctrl+J` | newline (see below) |
| `Ctrl+V`, `Alt+V` | paste the clipboard: an image becomes an `[Image #N]` chip, otherwise its text (see [Images](docs/reference/tui.md#images)) |
| `Esc` | answer the card on top; close a dialog (`/help` included) or the `/btw` side view; leave the sub-agent view; kill a running `!` command; clear a `!` draft; close the slash menu or the `@` file popup; otherwise cancel the running turn (the transcript says `cancelled`), stop a turn the agent runs on its own (native's wake), or, with nothing running, decline the plan offer |
| `Ctrl+C` | kill a running `!` command, and nothing else. With none running: cancel the running turn and everything queued behind it; a second press within one second ends the session; it ends it outright when idle or after an error. Inside the sub-agent view it still cancels the **main** turn, and the view stays open. Inside the `/btw` side view, with no card open, it closes the view |
| `Ctrl+D` | ends the session, always |
| `Shift+Tab` | cycle the ACP mode (agent / plan / ask); inside the sub-agent view, switch to the previous sub-agent instead |
| `Ctrl+T`, `/tasks` | tasks panel: compact → expanded → hidden |
| `Ctrl+G`, `/theme` | theme picker |
| `Ctrl+O` | expand / collapse transcript detail (diff hunks, command output, thoughts) — works inside the sub-agent view too |
| `Ctrl+Y` | copy the mouse selection, or the last reply when there is none (works with `--no-mouse`); inside the sub-agent view, copy from it |
| `←` on an empty composer, `/sessions` | open the session list: every session on this machine (see [Session list](docs/reference/tui.md#session-list)); only when sessions run in detached hosts, the default |
| `!command` | run a command in your own shell; its output goes to the agent with your next message |
| `@path` | mention a file or folder: a popup completes it |
| `/btw <question>` | ask a side question in a view of its own, not saved (native, Grok, gx); runs mid-turn |
| `↑` `↓` | move the keyboard out of the composer (draft or not) and then between rows: `↑` reaches the queued messages first and the sub-agent rows when nothing is queued, `↓` the other way round; the selected row carries a `❯` gutter mark; `↑` past the first row, `Esc`, or typing anything returns to the composer; inside the sub-agent view, scroll it; in a dialog or the slash menu, move the cursor |
| `Enter` while the sub-agent rows have the keyboard | open it in the main area, read-only: its own transcript on Grok, what the task receipt carried on Cursor |
| `Enter` / `Backspace` / `Ctrl+L` on a queued message | edit it in place / cancel it / send it now; while editing, `Enter` saves, `Esc` restores the draft and `Ctrl+L` saves and sends it now |
| `Esc` or `←` inside the sub-agent view | return to the main transcript; entering or leaving cancels nothing |
| `Tab` inside the sub-agent view | switch to the next sub-agent |
| `PgUp` / `PgDn`, wheel | scroll the transcript, or page the `/help` box |
| `Tab` / `Enter` on the slash menu | accept the highlighted row, replacing just the `/token` under the cursor; `Enter` on a name already typed in full runs or sends it instead |
| `PgUp` / `PgDn`, wheel, click on the slash menu | page it, move the selection one row a notch, or accept the row clicked |
| `Esc` on the slash menu | hide it for that token only; a second `Esc` cancels a running turn as usual |

`/help` opens a centred, scrollable box listing the same keys — grouped, one
per row — plus every slash command. `/exit` ends the session; there are no bare `q` or `?`
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

`--resume` lists the last ten sessions in the workspace. A session started for
a coding agent (`craze new --wait`) is tagged `sub-agent` and listed after your
own; `--continue` skips it.

**`Ctrl+G` is BEL.** Some terminals flash or beep when it is pressed. `/theme`
opens the same picker without the BEL.

### Cards

A blocking request from the agent is a card, and the card owns the keyboard and
the mouse while it is up: `Ctrl+T`, `Ctrl+G`, `Ctrl+O`, the arrows and the wheel
all do nothing until it is answered. `Ctrl+C` and `Ctrl+D` still work, and so
does `←`, where there is a [session list](docs/reference/tui.md#session-list):
it opens the list and leaves the card unanswered. Cards queue, and only the one
on top is drawn.

| Card | Keys |
|---|---|
| permission line — `permission <tool>  [a]llow once  [A]lways  [n] reject` | `a`, `y` or `1` allow once, **`A` allow always**, `n`, `r` or `2` reject. Only the options the request actually offered are drawn and bound. `Esc` cancels the turn. |
| question card | `1`–`9` pick, `↑` `↓` move, `Enter` selects (single-choice) or confirms (multiple-choice), `Space` toggles a multiple-choice option. `Esc` **skips** the whole request. |
| plan card — `plan <name>  [a]ccept  [r]eject  esc cancel` | `a`, `y` or `1` accept, `r`, `n` or `2` reject. `Esc` cancels the turn. |

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
the cursor moves (`↑`/`↓` or `j`/`k`, wrapping); `Enter` keeps the theme and
saves it, a digit `1`–`9` applies and keeps the theme at that position, and
`Esc` puts back the one that was active when the picker opened. `/theme <name>` sets and saves one
directly.

The saved theme lives in `~/.craze/config.toml`. Unrelated keys in that file
are preserved; a file craze cannot parse is never clobbered, and the save that
refused to overwrite it is reported in the transcript. An explicit `--theme`
beats the config file, which beats `craze-dark`.

`CRAZE_HOME` moves the whole `~/.craze` directory — the config file and the
session index both live directly inside it, as `config.toml` and
`sessions.jsonl`. It does not move the user-level skills or plugin caches,
which stay under `HOME`. It replaces an earlier variable that named the
config file itself: while that one is still set, every craze command exits
with status 2 and a message that names it (`--help` and `--version` still
answer).

## Mouse and copy

Mouse reporting is on by default. The wheel scrolls the transcript three lines
per notch. A left click picks a row in the model or theme dialog, cycles the
tasks panel from its header, cycles the mode from the `◆ agent` chip in status
row 2, opens the model dialog from the model name in status row 1, opens a
sub-agent row, accepts a row of the slash menu, or presses `[send now]`,
`[edit]` or `[cancel]` on the queued message under the pointer; inside the
sub-agent view, a click on the banner returns.

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

Its main flags are `--cols`, `--rows`, `--agent-bin`, `--fake-script` (sets
`CRAZE_FAKE_SCRIPT` for the child), `--keys`, `--ansi` (print the raw frame with
a forced true-colour profile), `--theme`, `--no-force`, `--timeout` (per wait,
default 10s) and `--print-frames` (stream every frame to stderr); it also takes
`--continue`, `--resume`, `--seed-session`, `--freeze`, `--provider` and
`--plugin-dir`. The [full table](docs/development/testing.md#craze-frame) is in
the testing guide. There is no
`--workspace`: `craze frame` runs in the current directory. It never reads
`~/.craze/config.toml`, so a saved theme cannot reach a golden.

**`craze frame` cannot drive the real `cursor-agent`.** It points `HOME` at an
empty directory and unsets `CRAZE_HOME` for the duration of the run — that is
what keeps a developer's config and skills out of a golden — and
`cursor-agent` loses its credentials along with it, so `Start` never returns
and the run dies on the implicit `<start>` barrier after the full `--timeout`.
Use it with
`./bin/craze-fake-agent`; to drive a live agent in a terminal, use the tmux
smoke below or just run `craze`.

In a key script, literal text is typed rune by rune and anything in angle
brackets is a token:

```
<enter> <esc> <tab> <backspace> <delete> <space> <up> <down> <left> <right>
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
make lint && make test && make test-race && make build && make test-cli
```

`make test-cli` runs the Python suite in `tests/cli` under `uv`: among others
`test_prompt.py` (the headless `prompt --json` path), `test_frame.py` (`craze
frame` through the binary, for the same scripts the Go goldens cover) and
`test_tui.py` (a real PTY). Everything uses `craze-fake-agent`, so no Cursor credentials are needed.

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
- [Upgrading](docs/getting-started/upgrading.md)
- [Claude Code sub-agents](docs/getting-started/claude-code.md)
- [CLI](docs/reference/cli.md)
- [TUI](docs/reference/tui.md)
- [Configuration](docs/reference/configuration.md)
- [Protocol](docs/reference/protocol.md)
- [Development](docs/development/setup.md), [Testing](docs/development/testing.md) and [Architecture](docs/development/architecture.md)

```shell
make docs         # same as CI: zensical build --strict
make docs-serve   # preview on :7070 (all interfaces)
```

Documentation is automatically published to GitHub Pages on push to main.

## Status

Early-stage software, released as v0.1.x for Linux and macOS (Homebrew, apt and
release archives). `craze` opens a TUI; `craze prompt --json` is the headless
path, and `craze new --wait` and `craze send` drive sessions from other programs.

## License

MIT. See [LICENSE](LICENSE).
