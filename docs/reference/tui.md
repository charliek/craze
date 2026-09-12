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

The composer never scrolls: it grows to show every line up to a cap (6 rows,
3 on a short terminal), then windows to keep the cursor visible. A rule above
it names the session — the title the agent set, truncated to half the screen
width, or `craze` before one arrives.

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

`/help` lists the same keys plus every slash command — see
[Help dialog](#help-dialog). `/exit` quits; there are no bare `q` or `?`
bindings, so a message that starts with either is just a message.

!!! note
    Shift+Enter is unreliable under bubbletea v1. Most terminals send a bare
    `Enter` for it, and craze cannot tell the two apart, so it sends the
    message. `Alt+Enter` and `Ctrl+J` are the newline keys that always work;
    `Shift+Enter` is bound as well, for the terminals that do report it
    distinctly.

!!! note
    `Ctrl+G` is BEL. Some terminals flash or beep when it is pressed. `/theme`
    opens the same picker without the BEL.

## Modes

`Shift+Tab`, `/plan`, `/ask` and `/agent` cycle or set the ACP session mode.
The mode chip in status row 2 reads `◆ <mode id>`, coloured by what kind of
mode the agent advertised it as: accent for an implement-like mode, purple for
plan, teal for a read-only mode like ask. Clicking the chip cycles the mode the
same way `Shift+Tab` does — see [Mouse](#mouse). Every change writes a
`mode → <id>` note, followed by the agent's own description of the mode when
it gave one.

When a turn in a plan-kind mode ends with an assistant reply, the composer's
placeholder offers to act on it once craze is idle:

```
enter implements this plan  ·  type to refine  ·  shift+tab leaves plan mode
```

`Enter` on the still-empty composer switches to an implement-kind mode and
sends `Implement the plan above.`; typing instead keeps the session in plan
mode and the offer disappears once the composer is non-empty. `Esc` clears the
offer without losing focus. Changing mode, `/clear`, the next turn or a card
arriving all clear it too.

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
| `/model` | Switch model |
| `/clear` | Clear transcript |
| `/tasks` | Tasks panel: compact, expanded, hidden |
| `/theme` | Theme picker, or `/theme <name>` |
| `/plan` | Set plan mode |
| `/ask` | Set ask mode |
| `/agent` | Set agent mode |
| `/exit` | Quit craze |

Workspace and home `SKILL.md` files are listed on slash without a skills RPC.
Agent-advertised commands from the ACP session are listed too, after the
builtins.

## Model dialog

`/model`, or a click on the model name in status row 1, opens a centred box
over the transcript: a filter (`❯ `, type to narrow by name or id), the model
list (current model first, then the agent's own order, `current` tagged,
`▲`/`▼` when it scrolls), and, only when the agent advertises them, an `effort`
row (`low medium high xhigh`, the picked one bracketed) and a `fast` row
(`on`/`off`).

The list, `effort` and `fast` are three focus targets, and the one the keys are
on carries a `> ` gutter and the selection background — so exactly one row looks
active, in a screenshot and with the colour stripped alike. A model that is
selected but no longer focused keeps a dimmer `· ` mark: `[value]` says which
value is *picked*, the gutter says which row is *focused*. The footer says which
keys are live, `type to filter · ↑↓ · tab effort/fast · enter · esc` on the list
and `←→ change · tab cycles · type filters · enter · esc` on a toggle row.
Typing filters the list whatever has focus.

`Tab`/`Shift+Tab` move focus between the list, effort and fast; `←`/`→` change
the focused row's value; clicking a row focuses it. `Enter` closes the dialog
and applies whatever changed — model, then effort, then fast — each as its own
step; a note is written for every step that succeeds, and a failed step is
named in an error instead of silently reverting the rest. `Esc`, or a click
outside the box, closes it and applies nothing. Status row 1 then reads
`Name (effort · fast)`, with `fast` shown only when it is on.

`/model <id>` and `/model <id> <effort>` still work without opening the
dialog. There is no full-width picker band and no `Ctrl+M` binding; the only
band left under the transcript is the slash menu.

`Ctrl+G`/`/theme` opens the same kind of box (title `theme`, no filter);
`↑`/`↓` preview live, `Enter` keeps it, and `Esc` or a click outside reverts to
the theme that was active when it opened. A card arriving from the agent
closes any dialog — the model dialog applies nothing, the theme dialog
reverts.

## Help dialog

`/help` opens a third box in the same frame (title `help`, 72 cells wide
because it is a two-column table). The keys are one per row, the key in a fixed
left gutter and what it does beside it, grouped under headings — sending and
editing, mode, moving and scrolling, panels and views, selection and clipboard
— then `commands` for craze's own slash commands and
`this session's commands` for whatever the agent advertises and the skills
found on disk, which vary by session.

The content is taller than any terminal, so the box clips to the transcript
region and scrolls: `↑`/`↓` a row, `PgUp`/`PgDn` a page, with `▲`/`▼` marking
what is off screen. It shrinks the same way the other two do — content rows
first, then the footer — down to a title-only box. `Esc`, a click outside, or a
card arriving closes it; nothing else is bound while it is up.

## Mouse

Mouse reporting is on by default; `--no-mouse` turns it off.

The wheel scrolls the transcript three lines per notch. A left click hit-tests
the same layout the frame was drawn from, so what is on screen and what is
clickable cannot drift apart:

| Target | Effect |
|---|---|
| Tasks panel header | Cycles the panel (same as `Ctrl+T`) |
| Sub-agent row under the status rows | Selects that sub-agent and opens its peek |
| Model name in status row 1 | Opens the model dialog (same as `/model`) |
| `◆ agent` mode chip in status row 2 | Cycles the mode (same as `Shift+Tab`) |
| A row inside an open dialog | Picks that row; a `/help` row is inert |
| Anywhere outside an open dialog | Closes it, applying nothing |
| The transcript | Starts a selection |

Task rows, the `… +n more` row, the separators between status segments and a
segment the row truncated away are all inert.

### Selection and copy

Dragging over the transcript highlights the cells between the press and the
pointer and copies them when the button comes up. The range is inclusive of both
endpoints and normalises, so dragging backwards selects the same text. A
double-click — two presses on the same cell inside 400 ms — selects the word
under the pointer and copies that.

A drag that reaches the top or bottom row of the transcript scrolls one line and
keeps extending. Cell-motion reporting only sends an event when the pointer
changes cell, so a pointer held still on the edge stops scrolling; nudge it to
carry on.

Status row 2 says `copied 2 lines`, or `copied "the first thirty characters…"`
for a single row, for two seconds. The highlight outlives the note: it stays
until the next key, the next click, the next thing the agent says (any change to
the transcript, a resize or `/clear` included), or a blocking card.

`Ctrl+Y` copies the current selection. With no selection it copies the last
reply — the agent's own text, not the rows it was wrapped onto. It is a keyboard
feature, so it works under `--no-mouse` as well.

Every copy is written twice: once as a bare OSC 52 escape sequence, which is the
one that survives ssh, and once to the system clipboard. Copies are capped at
64 KiB, cut on a rune boundary, and the note then ends `… (truncated)`.

For OSC 52 to land in the system clipboard:

- **tmux** needs `set -g set-clipboard on`. craze sends the sequence bare, so
  `allow-passthrough` is not required.
- Some terminals need it enabled: **xterm** wants
  `XTerm*disallowedWindowOps: 20,21,SetXprop`. **Alacritty**, **kitty**,
  **WezTerm**, **foot** and **iTerm2** allow it out of the box. **GNOME
  Terminal** does not implement OSC 52, which is what the second, native write
  is for.

Turning mouse reporting on takes native drag-select away from the terminal, and
craze's own selection replaces it. To reach the terminal's instead — to select
across the whole scrollback, say — hold `Shift` while dragging (`Option` in
macOS terminals), or start craze with `--no-mouse`.
