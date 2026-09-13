# TUI Reference

```bash
./bin/craze
./bin/craze --provider grok
```

The TUI is the default command. It refuses to start on a non-tty.

Without `--provider`, a centred **provider** dialog lists `cursor` and `grok`
before the session is constructed. The preselected row is the resolved default
(see [Configuration](configuration.md)). `↑`/`↓`/`Tab` move, `Enter` starts
that row, `Esc` starts the default. After Start the provider cannot change.

## Layout

Top to bottom:

1. Transcript
2. Pinned tasks panel
3. Spinner line
4. Composer
5. Two status rows
6. One row per in-flight sub-agent (both providers)

The composer never scrolls: it grows to show every line up to a cap (6 rows,
3 on a short terminal), then windows to keep the cursor visible. A rule above
it names the session — the title the agent set, truncated to half the screen
width, or `craze` before one arrives.

The sub-agent band under the status rows lists every running sub-agent and,
for ten seconds after the TUI saw it finish, the finished ones — the one being
viewed stays as long as it is viewed. A row is `<glyph> <label>
<description> <suffix>`: `○` while it runs, `✓` when it completed, `✗` when
it failed, `–` when it was cancelled, and `●` for the one being viewed; the
label is the sub-agent type (`explore`, `general-purpose`; cursor's rows read
`task`). While it runs the suffix counts up — `0s · 4.7k tok` — and once it
finishes it reads the duration and model, `2.9s · grok-4.6`. Status row 2
counts the band (`← 2 agents`).

## Keys

| Key | Action |
|---|---|
| `Enter` | send |
| `Alt+Enter`, `Ctrl+J` | newline (see below) |
| `Esc` | answer the card on top; close a dialog (`/help` included); leave the sub-agent view; close the slash menu; otherwise cancel the running turn (the transcript says `cancelled`) |
| `Ctrl+C` | cancel the running turn; a second press within one second quits; quits outright when idle or after an error. Inside the sub-agent view it still cancels the **main** turn, and the view stays open |
| `Ctrl+D` | quit, always |
| `Shift+Tab` | cycle the ACP mode (agent / plan / ask); inside the sub-agent view, switch to the previous sub-agent instead |
| `Ctrl+T`, `/tasks` | tasks panel: compact → expanded → hidden |
| `Ctrl+G`, `/theme` | theme picker |
| `Ctrl+O` | expand / collapse transcript detail (diff hunks, command output, thoughts) — works inside the sub-agent view too |
| `Ctrl+Y` | copy the mouse selection, or the last reply when there is none (works with `--no-mouse`); inside the sub-agent view, copy from it |
| `↑` `↓` | select a sub-agent row (empty composer or not); inside the sub-agent view, scroll it; in a dialog or the slash menu, move the cursor (in `/help`, scroll the box) |
| `Enter` on a selected sub-agent | open it in the main area, read-only (see [Sub-agent view](#sub-agent-view)) |
| `Esc` or `←` inside the sub-agent view | return to the main transcript; entering or leaving cancels nothing |
| `Tab` inside the sub-agent view | switch to the next sub-agent |
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

## Sub-agent view

`Enter` on a selected row — or a click on the row — opens that sub-agent in
the main area, read-only. Grok streams a sub-agent's own session, so its
transcript is the child's own prompt, thoughts, tool calls and replies,
rendered by the same machinery as the main one. Cursor streams no sub-agent
transcript, so the view is what its `cursor/task` receipt carried: a note
saying so, the prompt, a `model · duration · agent id` line once the receipt
landed, and the output when there is one. There is no composer here — you
cannot prompt, message or stop a sub-agent from craze.

The composer's top rule carries the chip `(model) description` —
`(grok-4.6) List directory files` — and the input line is replaced by the
banner:

- running: `○ @explore · read-only · esc to return`, with `· tab next agent`
  when more than one row is visible
- finished: `✓ @explore · completed · esc to return`, in the warning colour
  so the end is unmistakable (`✗ failed` / `– cancelled`, with the error when
  there is one; the `esc to return` half never truncates away)
- cursor: `@task · receipt only · esc to return`

Entering or leaving cancels nothing. The main turn keeps running underneath —
while the viewed sub-agent runs, the spinner line is its own
`<spinner> <activity> · <elapsed> · <tokens>`, not the turn's — and the draft
and the main transcript's scroll position come back on return. A sub-agent
that finishes while viewed stays in the view until `Esc`, even past the
ten-second linger an unviewed row gets.

Keys inside the view: `Esc` and `←` return; `↑`/`↓` scroll it a line,
`PgUp`/`PgDn` page, the wheel scrolls three lines as everywhere else;
`Tab`/`Shift+Tab` switch to the next/previous sub-agent (the mode cycle is
unreachable here); `Ctrl+O` toggles detail and `Ctrl+Y` copies from it, both
as outside; `Ctrl+C` still cancels the main turn and the view stays open.
Typing, pasting and the slash menu are inert inside the view, and a card
still lands on top and owns the keyboard until it is answered.

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

Skills come from the **provider**, not a global scan. Cursor walks
`.cursor/skills`, `.agents/skills`, `.codex/skills`, `.claude/skills` under
the workspace and `$HOME`, and still skips `.cursor/plugins`. Grok walks
`.grok/skills`; its bundled and plugin skills need no walk because the grok
session advertises all of them (under their real slash names, such as
`coderabbit:code-review`) as ACP commands. Agent-advertised commands from
the ACP session are listed after the builtins.

## Model dialog

`/model`, or a click on the model name in status row 1, opens a centred box
over the transcript: a filter (`❯ `, type to narrow by name or id), the model
list (current model first, then the agent's own order, `current` tagged,
`▲`/`▼` when it scrolls), and, only when the agent advertises them, an `effort`
row (`low medium high xhigh`, the picked one bracketed) and a `fast` row
(`on`/`off`). Grok advertises effort but not fast, so the fast row stays
hidden.

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

On a provider that shows the band, `panels and views` gains the sub-agent view
keys — `enter on a row` to open one, `esc, ←` to return, and
`tab, shift+tab` to switch while inside — and the `↑ ↓` row reads
`sub-agent rows, or a list inside a dialog`.

## Mouse

Mouse reporting is on by default; `--no-mouse` turns it off.

The wheel scrolls the transcript three lines per notch. A left click hit-tests
the same layout the frame was drawn from, so what is on screen and what is
clickable cannot drift apart:

| Target | Effect |
|---|---|
| Tasks panel header | Cycles the panel (same as `Ctrl+T`) |
| Sub-agent row under the status rows | Selects that sub-agent and opens it in the main area (same as `Enter` on it) |
| The banner line inside the sub-agent view | Returns to the main transcript (same as `Esc`) |
| Model name in status row 1 | Opens the model dialog (same as `/model`) |
| `◆ agent` mode chip in status row 2 | Cycles the mode (same as `Shift+Tab`) |
| A row inside an open dialog | Picks that row; a `/help` row is inert |
| Anywhere outside an open dialog | Closes it, applying nothing |
| The transcript | Starts a selection |

Dragging and double-clicking inside the sub-agent view select and copy from
its transcript the same way. The mode chip is inert inside the view. Task
rows, the `… +n more` row, the separators between status segments and a
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
