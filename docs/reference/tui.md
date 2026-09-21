# TUI Reference

```bash
./bin/craze
./bin/craze --provider grok
```

The TUI is the default command. It refuses to start on a non-tty.

Without `--provider`, a centred **provider** dialog lists `cursor`, `grok`,
and `gx` — the last shown only when a binary for it resolves, since gx is a
third-party fork nobody can assume is installed (see
[Configuration](configuration.md#provider-precedence)) — before the session
is constructed. The preselected row is the resolved default (see
[Configuration](configuration.md)). `↑`/`↓`/`Tab` move, `Enter` starts
that row, `Esc` starts the default. After Start the provider cannot change.

`--resume` shows a **resume** picker instead of that dialog, and `--continue`
skips both and loads a session directly — see [Resuming a
session](#resuming-a-session) and [CLI reference](cli.md#-continue-and-resume).

## Layout

Top to bottom:

1. Transcript
2. Pinned tasks panel
3. Spinner line
4. Composer
5. Two status rows
6. One row per in-flight sub-agent (all providers)

The composer never scrolls: it grows to show every line up to a cap (6 rows,
3 on a short terminal), then windows to keep the cursor visible. A rule above
it names the session — the title the agent set, truncated to half the screen
width, or `craze` before one arrives.

The sub-agent band under the status rows lists every running sub-agent and,
for ten seconds after the TUI saw it finish, the finished ones — the one being
viewed stays as long as it is viewed. A row is `<gutter> <glyph> <label>
<description> <suffix>`: the gutter is `❯` on the selected row (its
description is accent-coloured too) and blank on the others; `○` while it runs, `✓` when it completed, `✗` when
it failed, `–` when it was cancelled, and `●` for the one being viewed; the
label is the sub-agent type (`explore`, `general-purpose`; cursor's rows read
`task`). While it runs the suffix counts up — `0s · 4.7k tok` — and once it
finishes it reads the duration and model, `2.9s · grok-4.6`. Status row 2
counts the running ones (`← 2 agents`); a lingering finished row is not counted.
The keyboard starts in the composer; `↓` moves it to the rows, where the
selected row carries the `❯` mark, and `↑` past the first row, `Esc` or any
typed key move it back. Returning from the sub-agent view leaves it on the
rows, marking the row the view came from. Rows sit in spawn order and never reshuffle
while a sub-agent streams, so `↑`/`↓` aim at a fixed target; a finished row
keeps its slot until it leaves the band, and the rows below close up. Past
the cap the band ends in `… +n more`; the viewed sub-agent always keeps a
visible row, taking the last one when it would otherwise fall behind the cap.

## Resuming a session

`--resume` opens a **resume** picker in place of the provider dialog: rows
of `<title> · <provider> · <age>` (age as `3m`, `2h`, or `5d` since the
session was last touched), newest first, up to 10 rows. `↑`/`↓`/`Tab`/`Shift+Tab`
move and wrap, `Enter` loads the selected row, and a click loads the row
clicked; `Esc` quits craze outright (exit 0 — no session was ever started,
so there is no "default" the way the provider dialog has one) and a click
outside the box does nothing, unlike every other dialog. `--continue` skips
the picker and loads the newest row for the workspace directly. See [CLI
reference](cli.md#-continue-and-resume) for the flags, the filtering
rules, and the exit codes when nothing matches.

Loading a row is a **replay**, not a fresh start. Status row 1 reads
`restoring…` in place of `starting…`, and sending a message or running
`/rename` is refused until the whole transcript is back — even after the
agent has otherwise answered the initial handshake. The replay renders as
ordinary transcript entries (the user's prompt, thoughts, tool rows,
replies) and ends with a `restored` note marking the seam between the old
session's history and the live one. Once restored, the rule above the
composer shows the row's stored title (see [`/rename`](#slash-commands))
instead of `craze`.

A restored session does not reconstruct everything:

- **Timestamps** on replayed entries are when craze loaded the session, not
  when the agent originally sent them — the agents themselves do not replay
  their own timestamps.
- **Sub-agent transcripts** are not reconstructed. A finished sub-agent's
  row comes back from the replayed spawn/finish lifecycle, but there is no
  transcript behind it to open.
- **Grok and gx** show no `effort` or `fast` row in the [model
  dialog](#model-dialog) right after a resume — their `session/load` result
  carries neither, unlike a fresh `session/new`. A later live turn may
  supply them again.
- A rename is craze's own and is never sent to the agent: `agent ls` and
  `grok --resume` still show whatever title the agent itself gave the
  session.

## Tab title

craze sets the terminal tab title (Ghostty, roost, and anything else that
honours OSC 0/2) to `<mark> <text>`: `<text>` is `craze` until the session
has a title, then the stored title (agent, first-prompt fallback, or a
`/rename`) truncated to 40 cells. The mark is one glyph, highest priority
first:

| State | Mark | When |
|---|---|---|
| needs you | `⚠` | a permission, question, or plan card is open |
| error | `✕` | a start failure or a turn error (cleared by the next send) |
| working | `❖` | a turn is running, a session is replaying, or a foreign turn is in progress |
| idle | `✦` | everything else, including the pre-start picker |

The strong-send confirm line does not raise the needs-you mark: it is the
user's own keystroke, not the agent asking for something. The title updates
the instant the state changes and is cleared on every exit, so the tab falls
back to the terminal's own derived name rather than a stale `✦ craze`.

Inside a herdr pane or a roost tab the same states also go to the host itself,
so its sidebar and its wait command follow craze like any other agent. herdr
sees `blocked` for an open card or an error, `working` for a turn and `idle`
otherwise; roost shows `needs input` for an open card, `failed` for an error,
`running` for a turn, and a "Turn complete" notification when a turn ends. A
replay is not reported. See [Host status](configuration.md#host-status) for
what is sent, what it changes in the agent's environment, and how to turn it
off.

`terminal_title = false` in `~/.craze/config.toml` turns every write off
(default: on) — see [Configuration](configuration.md#terminal-tab-title).
`craze prompt` and `craze frame` never set a tab title.

## Keys

| Key | Action |
|---|---|
| `Enter` | with the slash menu open on a token that is not already typed out in full, accept the highlighted row; on a draft that starts with `!`, run it in your own shell (see [Shell mode](#shell-mode)); otherwise send the draft, or **queue** it while a turn is running (see [Queued messages](#queued-messages)) |
| `Ctrl+L` | the strong send: on Grok, add the draft to the running turn without cancelling it; on Cursor, cancel the running turn and send (it asks first). On an idle session it is a plain send |
| `Alt+Enter`, `Ctrl+J` | newline (see below) |
| `Esc` | answer the card on top; close a dialog (`/help` included); leave the sub-agent view; kill a running `!` command; clear a `!` draft; hide the slash menu for the token under the cursor — a second `Esc` then cancels the running turn; otherwise cancel the running turn (the transcript says `cancelled`). An Esc pressed immediately after Enter cancels that turn; craze never writes the cancel ahead of the prompt |
| `Ctrl+C` | kill a running `!` command, and nothing else — it is the only way to stop one while a card has the keyboard. With none running: cancel the running turn **and everything queued behind it** — the queue, a confirm on screen, a send-now waiting to fire; a second press within one second quits; quits outright when idle or after an error. Inside the sub-agent view it still cancels the **main** turn, and the view stays open |
| `Ctrl+D` | quit, always |
| `Shift+Tab` | cycle the ACP mode (agent / plan / ask); inside the sub-agent view, switch to the previous sub-agent instead |
| `Ctrl+T`, `/tasks` | tasks panel: compact → expanded → hidden |
| `Ctrl+G`, `/theme` | theme picker |
| `Ctrl+O` | expand / collapse transcript detail (diff hunks, command output, thoughts) — works inside the sub-agent view too |
| `Ctrl+Y` | copy the mouse selection, or the last reply when there is none (works with `--no-mouse`); inside the sub-agent view, copy from it |
| `↑` `↓` | move the keyboard out of the composer (draft or not) and then between rows: the selected row carries a `❯` gutter mark and the composer loses its cursor. `↑` reaches the queue band first and the sub-agent rows when nothing is queued; `↓` reaches the sub-agent rows first and the queue band when there are none. `↑` past the first row, `Esc`, or typing anything returns to the composer; inside the sub-agent view, scroll it; with the slash menu open, move its highlighted row instead, wrapping at either end; in a dialog, move its selection (in `/help`, scroll the box) |
| `Enter` while the sub-agent rows have the keyboard | open it in the main area, read-only (see [Sub-agent view](#sub-agent-view)) |
| `Enter` while the queue band has the keyboard | edit that message in place — its text loads into the composer, `Enter` saves, `Esc` restores the draft |
| `Backspace` / `Delete` on a queued row | cancel it |
| `Ctrl+L` on a queued row | send it now instead of the running turn (it asks first) |
| `Esc` or `←` inside the sub-agent view | return to the main transcript; entering or leaving cancels nothing |
| `Tab` inside the sub-agent view | switch to the next sub-agent |
| `PgUp` / `PgDn` | scroll the transcript, or page the `/help` box; page the slash menu instead when it is open — the menu takes priority over both |
| wheel | scroll the transcript three lines a notch; over an open slash menu, move its selection one row a notch instead — it selects, it does not page (see [Mouse](#mouse)) |
| `Tab` | with the slash menu open, accept the highlighted row (see [Slash commands](#slash-commands)); elsewhere a no-op |

`/help` lists the same keys plus every slash command — see
[Help dialog](#help-dialog). `/exit` quits; there are no bare `q` or `?`
bindings, so a message that starts with either is just a message.

!!! note
    Shift+Enter is unreliable under bubbletea v1. Most terminals send a bare
    `Enter` for it, and craze cannot tell the two apart, so it sends the
    message. `Alt+Enter` and `Ctrl+J` are the newline keys that always work;
    `Shift+Enter` is bound as well, for the terminals that do report it
    distinctly.

!!! note "Why `Ctrl+L` and not `Ctrl+Enter`"
    The same limitation: bubbletea v1 has no `Ctrl+Enter` and does not parse
    the kitty keyboard protocol, so `Ctrl+Enter` arrives as a bare `\r` —
    indistinguishable from `Enter`, which is the key that queues. Grok's own
    TUI falls back to `Ctrl+L` for the same reason, so craze does too.

## Queued messages

While a turn runs, `Enter` queues the draft instead of refusing it. The queued
messages appear in a band above the spinner, newest last:

```text
  #1 Reply with PINEAPPLE
  #2 Reply with MANGO          [send now] [edit] [cancel]
✳ Working · 12s · esc to interrupt
```

One is sent per settled turn, in order, and it only becomes a transcript entry
when it is actually sent. Status row 2 carries `⧗ n queued` so the number is
visible even when the band has been degraded away on a short terminal.

The three actions are drawn on the row under the pointer and on the row the
keyboard is on. Each is also a key: `Ctrl+L` sends that message now,
`Enter` edits it in place (the row keeps its number and its place in the
queue), and `Backspace` cancels it.

### The three verbs

| Verb | Cursor | Grok | Cancels the turn? | Asks first? |
|---|---|---|---|---|
| queue (`Enter` on a draft during a turn) | craze's queue | craze's queue | no | no |
| send now (`Ctrl+L` on a queued row; on a draft under Cursor) | cancels the turn, then sends | same | **yes** | **yes** |
| interject (`Ctrl+L` on a draft under Grok) | not offered — `Ctrl+L` is send now | merged into the running turn | no | no |

Grok is the only agent with a mid-turn path: `x.ai/interject` hands it text
that it folds into the turn at its next safe point (after a tool result), and
the transcript shows it as a `↳` entry when the agent broadcasts it back.
Cursor has none — a second prompt there silently cancels the first — so craze
never sends one and offers send now instead, always with

```text
cancel the running turn and send? enter · esc
```

in place of the composer's input rows. `Esc`, or any other key, declines and
loses nothing: nothing leaves the composer or the queue until the message
actually goes.

### What clears the queue and what does not

| | Queue |
|---|---|
| `Esc` (cancel the turn) | kept — the next queued message starts once the cancelled turn settles |
| `Ctrl+C` (first press) | cleared, along with a confirm and a pending send-now |
| `/clear` | cleared |
| A turn that ends in an error | cleared, with a note — nothing drains from an error state |
| Quitting | gone with the session; there is no persistence across restarts |

Slash builtins never queue. `/exit`, `/help`, `/theme`, `/tasks` and `/clear`
run immediately even mid-turn; the ones that need the agent (`/model`,
`/plan`, …) keep today's refusal. A slash line the agent advertises is
ordinary text and queues like any other message.

The queue holds 32 messages of up to 32 KiB each. Past either bound the
message is **refused**, never truncated, and the draft stays in the composer
so you can shorten it: status row 2 says `queue full` or `message too long`.

### When the agent talks on its own

Grok turns an interjection it could not merge — one that arrives when no turn
is running, or too late in the one that is — into a turn of its own, called an
*interject fallback*. craze shows its reply under

```text
agent continued on its own (interjection fallback)
```

and holds the queue until it ends: a message sent into that turn would be
queued behind it on the agent's side, where craze can no longer tell its
ending from the fallback's.

!!! note
    `Ctrl+G` is BEL. Some terminals flash or beep when it is pressed. `/theme`
    opens the same picker without the BEL.

## Shell mode

A draft whose first character is `!` is a command for your own shell, not a
message for the agent. There is no toggle: the mode is the draft. The
composer's two rules take the shell colour and the top one reads

```text
shell · enter run · esc clear
```

in place of the session title, and the slash menu stays shut — `!ls /usr` must
not offer `/usr`. Delete the `!` and everything is as it was.

`Enter` runs the whole draft after the `!`, so a multi-line paste is one
script, and the draft clears as it does on a send. The command runs through
`$SHELL -c` (`/bin/sh -c` when `$SHELL` is not an absolute path craze can
execute), in the session's workspace, with craze's own environment, its stdin
on `/dev/null`, and in a process group of its own so nothing it starts outlives
it.

!!! warning
    A command runs locally, immediately, with your own rights — the same thing
    would have happened had you typed it into your own shell. craze asks
    nothing first and the agent has no say in it: shell mode runs what **you**
    typed, and nothing an agent said can put a command in the composer.

To send a message that begins with `!`, start it with a space: the leading
space is trimmed on the way out, so ` !important` reaches the agent as
`!important`.

### Keys

| Key | Action |
|---|---|
| `Enter` | run the draft |
| `Esc` | kill the running command; with none running, clear the draft (it never cancels the agent's turn) |
| `Ctrl+C` | kill the running command — one press, one kill: it does not quit, does not cancel the turn, and does not arm the quit window. It is the way to kill one while a card has the keyboard |
| `Ctrl+L` | refused in shell mode: `Enter` is how a command runs |

A command is killed with SIGTERM to its whole process group, SIGKILL three
seconds later, and it is killed on every way out of craze — `Ctrl+D`, `/exit`,
a double `Ctrl+C`, SIGTERM, and a session change.

!!! warning "What can outlive craze"
    A command that deliberately detaches itself — `setsid`, `disown`, or a
    daemon that puts itself in a session of its own — leaves the process group
    craze kills, and craze cannot reach it afterwards: it can outlive the
    command and craze with it, and is yours to stop. Everything else the
    command started is killed when the command ends and again when craze
    exits, whether it is still writing output (`server &`) or not
    (`server >/dev/null 2>&1 &`).

Enter is refused, and the draft kept, while a card is open, while a session is
still starting or restoring, and while another command is running — one at a
time. A bare `!` does nothing. It is **allowed while the agent is working**: it
is your shell, and it needs nothing of the session but its workspace.

Shell mode is the composer's alone. A queued message, an edited queued row, the
plan offer's `Enter`, a replayed prompt and a send-now all go to the agent as
text however they start, and `craze prompt '!x'` sends `!x` as a prompt.

### The transcript row

A command writes one row, `! <cmd>`, with the spinner while it runs. When it
finishes the row carries its output, dimmed and collapsed past 20 lines under
the usual `Ctrl+O`, and the right-hand end says how it ended when it was not a
clean exit: `exit 2`, `killed`, `timed out after 120s`.

| Limit | Value |
|---|---|
| Output kept | 16 KiB, from the **tail** — an earlier head is replaced by `[craze: output truncated]` |
| Time | 120 s, then the group is killed (`timed out after 120s`) |
| Carried to the agent | the last 3 commands, 16 KiB of output together |

Shell mode is not a terminal: no pty, no interactive programs, no job control
and no history. A command that wants to be answered reads EOF; one that wants a
terminal should be run in one.

### What the agent is told

Each finished command is kept, and the command and its output travel with the
**next message you send** — a sent message, a message you queue, or an
interjection. They go once, in front of your text, and are gone: craze sends
nothing when a command finishes, and a session you quit without writing another
message never mentions any of it.

```text
!git status --short          ← runs locally, output on screen
what changed?                ← this message carries both to the agent
```

The `Enter` that accepts the plan offer sends craze's own sentence and carries
nothing. The context is cleared only once the message was **accepted**: a
message refused for length keeps the draft *and* the output for the next
attempt — and the attached output counts against the queue's 32 KiB per-message
limit. `/clear` and a session change drop it.

What is attached is never drawn: your transcript row, the queue band, the queue
editor and the session title show the message you typed, not the block that
went with it. A `/name` inside a command's output is output and nothing else —
`!cat plan.md` on a file full of slash commands expands none of them.

## Sub-agent view

`Enter` while the rows have the keyboard (`↓` from the composer, then `↑`/`↓`
to the row) — or a click on the row — opens that sub-agent in the main area,
read-only. Grok streams a sub-agent's own session, so its
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
- cursor: `○ @task · receipt only · esc to return` while it runs, then the
  same warning-coloured finish line with `· receipt only` kept:
  `✓ @task · completed · receipt only · esc to return`

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

The menu opens on the `/` token under the cursor, not only on a whole-line
`/name`: a `/` at offset 0 of the draft or right after any whitespace starts a
token, so typing `see /gau` mid-sentence opens the menu on `/gau` the way
Cursor's and Grok's own TUIs already let you invoke a skill there. `foo/bar`
and `https://x` never trigger — the `/` has to sit at the start of the draft
or follow whitespace, not mid-word. A bare `/`, anywhere in the draft, opens
on an empty token and lists the whole catalog; that is how you browse it.

craze's own commands are only ever offered where `Enter` would actually run
them: the draft's first non-whitespace run, with no newline anywhere in the
draft. Typed after the first word, on a second line, or with anything else
ahead of it, they drop out of the menu — offering `/help` where `Enter` could
only send the text as a prompt would be a lie. Agent-advertised commands and
skills carry no such restriction; they complete anywhere in the draft.

| Command | Action |
|---------|--------|
| `/help` | Keybindings and commands |
| `/model` | Switch model |
| `/clear` | Clear transcript |
| `/tasks` | Tasks panel: compact, expanded, hidden |
| `/theme` | Theme picker, or `/theme <name>` |
| `/rename <title>` | Rename this session — craze's own; never sent to the agent |
| `/plan` | Set plan mode |
| `/ask` | Set ask mode |
| `/agent` | Set agent mode |
| `/exit` | Quit craze |

Matches are prefix hits first, then substring hits, each group kept in the
catalog's own order, case-insensitive — no fuzzy matching. The band shows up
to 8 rows and scrolls to keep the selection in view rather than capping the
list, so the whole catalog stays reachable by scrolling. `Tab` or `Enter`
accepts the highlighted row, replacing just the token with `/name ` (absorbing
one space that already followed it) and closing the menu; `Enter` on a token
that already spells the highlighted name falls through and runs or sends
instead, and `Enter` on a bare `/` accepts the first row rather than sending
`/` itself as a prompt. `↑`/`↓` move the selection with wrap; `PgUp`/`PgDn`
page it, ahead of paging the transcript, while the menu is open. A click on a
row accepts it and the wheel over the band moves the selection — see
[Mouse](#mouse).

When the list is longer than the band, the first row carries `k/n` — `k` is
the selection's position, `n` the match count — followed by `▲` once the
window has scrolled past the top; the last row carries `▼` when there is more
below. A band cropped to a single row shows `k/n` alone: it is both the first
and the last row, and the count already says what the arrows would.

Skills come from the agent's own ACP catalog first: Grok's ACP server loads a
plugins service, so its catalog already carries every plugin skill it has
enabled, named and expanded by Grok itself — craze changes nothing there.
Cursor's ACP server never constructs a plugins service, so its catalog never
carries anything a plugin ships, command or skill, and never expands one
either: sent as prompt text, `/watch-pr` reaches the model as the two words
`/watch-pr`, and the model is left to guess what they meant. Everything
Cursor's catalog *does* carry — project and user skills, `.cursor/commands`,
`.claude/commands`, and workspace and global commands — is still expanded by
cursor itself exactly as before. The on-disk scan is a supplement, for
project skills the session's own catalog does not carry (Grok in particular
does not advertise its own project skills over ACP, so the walk is their
only path to the menu): Cursor walks `.cursor/skills`, `.agents/skills`,
`.codex/skills`, `.claude/skills` under the workspace and `$HOME`, still
skipping `.cursor/plugins`; Grok walks `.grok/skills` and `.agents/skills`.
Cursor and Grok disagree on how a skill on disk gets its name — **Cursor
names it after the skill's directory**, whatever its frontmatter says;
**Grok names it after the frontmatter `name`**, falling back to the
directory only when there is none — and on both, `user-invocable: false` in
the frontmatter hides the skill from the menu the same way it hides it from
the agent's own catalog.

For Cursor, craze makes up the difference itself: it finds a plugin's
commands and skills on disk and expands them into the prompt the way
cursor's own TUI does, so `/watch-pr` and the rest of a plugin's commands
reach the menu and work when sent even though Cursor's ACP server never
sees them. It looks in three places, first source wins on a `plugin:name`
collision: `--plugin-dir` directories, in the order given; then the cursor
plugin cache (`~/.cursor/plugins/cache/<marketplace>/<plugin>/<version>/`,
the version with the newest modification time among those carrying that
plugin's `.cache-complete` sentinel — a plugin with no finished install
contributes nothing); then Claude Code's enabled plugins
(`~/.claude/plugins/installed_plugins.json`, filtered by the workspace's and
the user's `.claude/settings.json` the way cursor's own loader filters
them). An entry is offered under its own bare name when nothing else —
Cursor, a builtin, another plugin — claims that name, and under
`plugin:name` on a collision or when Cursor or a builtin already owns the
bare spelling. The menu's order stays builtins, then whatever Cursor
advertised, then these plugin rows, then disk skills, the same
case-insensitive first-name-wins dedupe as everywhere else.

Typing `:` is how you ask for a plugin's qualified spelling: `/git-c` finds
`watch-pr` and `merge-pr` through the plugin's own name even when neither
collides with anything else, `Tab` on `/git-commands:w` inserts
`/git-commands:watch-pr `, `Tab` on `/wat` inserts the bare `/watch-pr `, and
`Enter` sends on either fully-typed spelling. A name several plugins share
(four installs on this machine each ship a `/rescue`) is never offered
bare, so a bare `/rescue` typed by hand is not a plugin reference at all —
it goes to the agent as ordinary text, the same as any other name nothing
advertises.

Under the user's transcript entry, a plugin reference leaves a dim
`⤷ plugin:name (kind)` line — the qualified spelling and whether it was a
command or a skill, never the body itself. A command's body substitutes
`$ARGUMENTS` and `$1`..`$99` from the typed arguments, cursor's own rule for
plugin commands; a skill's body is sent whole and unsubstituted, because
that is what cursor itself does with a skill's content — the arguments show
up only in the block's lead sentence and its `args` attribute. Both substitute `${CLAUDE_PLUGIN_ROOT}`
and `${CURSOR_PLUGIN_ROOT}` with the plugin's own install path. Up to 8
references expand per prompt, first occurrence of each name only; a
headless `craze prompt --json` run can read the whole block in the
`command` event's `text` field (see [CLI reference](cli.md#json-events)).
Grok changes on none of this: it already advertises and expands every
plugin skill itself, so craze's menu and wire are exactly what they were
before, and `/plugin:name` typed by hand for a skill Grok advertises bare is
honoured by Grok's own resolver.

Cursor's ACP catalog lands a few seconds after the session starts
(`session/new` itself carries none of it), so a `/` typed right away shows
only craze's builtins, whatever plugin rows resolved from disk — qualified,
see below — and whatever the disk scan already found; the rest appears once
the agent advertises it. Plugin rows stay qualified for exactly that window:
a row shown as `plugin:name` before Cursor's first catalog update goes bare
the moment that update confirms nothing else owns the name, so the menu
renames itself once per session; a draft already typed with the qualified
spelling keeps resolving no matter which way the row is drawn, so nothing
sent during that window is ever wrong. If Cursor ever stopped sending the
catalog altogether, plugin rows would simply stay qualified for the whole
session — safe, just less tidy.

`craze prompt`'s one-shot turn does not get to wait out that window by
watching the menu, so when the very first prompt of a session carries a
slash token that does not yet resolve, craze holds it for up to 5 seconds so
a bare name is not sent before the catalog can say whether it is really
unclaimed. `Esc` cancels the wait like any other turn — the prompt never
reaches the wire. A draft with no slash token, an already-qualified name, or
any prompt after the first never waits, and falling through the 5 seconds
unresolved is exactly the old behaviour: the draft goes out verbatim.

Two things worth knowing about where plugin rows come from. The cursor
plugin cache is read as *installed*, not as currently enabled, so a plugin
the account has since turned off keeps its cache until cursor prunes it and
craze can still offer it — because craze expands the command itself rather
than counting on the agent to recognise it, that is optimistic about what
the account still has on, not a lie about what will happen. And because the
token scan reads the whole prompt line the way cursor's own regex does, a
stray `/watch-pr` in the middle of a sentence ("see /watch-pr for context")
expands exactly as if it had been typed at the start.

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

The wheel scrolls the transcript three lines per notch — except over the
slash menu, where it moves the selection one row per notch instead, clamped
rather than wrapped: the menu is a selection, not a viewport, so three rows a
notch would jump past rows never seen highlighted. A left click hit-tests the
same layout the frame was drawn from, so what is on screen and what is
clickable cannot drift apart:

| Target | Effect |
|---|---|
| A row in the slash menu | Accepts it (same as `Tab`); a click after scrolling lands on the row actually drawn there |
| Tasks panel header | Cycles the panel (same as `Ctrl+T`) |
| Sub-agent row under the status rows | Selects that sub-agent and opens it in the main area (same as `Enter` on it) |
| A queued message's text | Selects that row |
| `[send now]` on a queued row | Sends it instead of the running turn (asks first) |
| `[edit]` on a queued row | Loads it into the composer for an in-place edit |
| `[cancel]` on a queued row | Removes it from the queue |
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

The queue's actions appear on hover, which needs motion reports the default
mouse mode does not send. craze switches the terminal to all-motion reporting
while at least one message is queued and back to cell motion when the last one
leaves — and never at all under `--no-mouse`. A button is only clickable on
the row it was drawn on, which is the row under the pointer and the row the
keyboard has selected; a click anywhere else in the band selects that row
rather than acting on it.

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
