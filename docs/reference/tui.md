# TUI Reference

```bash
./bin/craze
```

The TUI is the default command. It refuses to start on a non-tty.

## Layout

Top to bottom:

1. Transcript
2. Pinned tasks panel
3. Spinner line
4. Composer
5. Two status rows
6. One row per in-flight sub-agent

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

!!! note
    Shift+Enter is unreliable under bubbletea v1. Most terminals send a bare
    `Enter` for it, and craze cannot tell the two apart, so it sends the
    message. `Alt+Enter` and `Ctrl+J` are the newline keys that always work;
    `Shift+Enter` is bound as well, for the terminals that do report it
    distinctly.

!!! note
    `Ctrl+G` is BEL. Some terminals flash or beep when it is pressed. `/theme`
    opens the same picker without the BEL.

## Cards

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

## Slash commands

| Command | Action |
|---------|--------|
| `/help` | Keybindings and commands |
| `/model`, `/models` | Switch model |
| `/clear` | Clear transcript |
| `/tasks` | Tasks panel: compact, expanded, hidden |
| `/theme` | Theme picker, or `/theme <name>` |
| `/plan` | Set plan mode |
| `/ask` | Set ask mode |
| `/agent` | Set agent mode |
| `/exit`, `/quit` | Quit craze |

Workspace and home `SKILL.md` files are listed on slash without a skills RPC.
Agent-advertised commands from the ACP session are listed too, after the
builtins.

## Mouse

Mouse reporting is on by default. The wheel scrolls the transcript three lines
per notch; a left click picks a row in the model or theme picker, cycles the
tasks panel from its header, or selects a sub-agent row.

Turning mouse reporting on takes native drag-select away from the terminal. To
select text anyway, hold `Shift` while dragging (`Option` in macOS terminals),
or start craze with `--no-mouse`.
