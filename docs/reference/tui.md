# TUI Reference

```bash
./bin/craze
./bin/craze --provider grok
```

The TUI is the default command. It refuses to start on a non-tty.

Without `--provider`, a centred **provider** dialog lists `cursor`, `grok`,
`gx` and `native` — `gx` shown when a binary for it resolves, or when it is
the resolved default (the default is always listed, so `Esc` never starts a
row the picker did not show), since gx is a third-party fork nobody can assume
is installed (see [Configuration](configuration.md#provider-precedence)) —
before the session is constructed. The preselected row is the resolved default
(see [Configuration](configuration.md)). `↑`/`↓`/`Tab` move, `Enter` starts
that row, `Esc` starts the default. After Start the provider cannot change.

A row the command line rules out is not started: `native` with `--agent-bin`
or `CRAZE_AGENT_BIN` set, since it runs inside craze and has no binary to
spawn. `Enter` on it shows the refusal `--provider native` would have exited 2
with, as an error row under the list, and the dialog stays open for another
choice; nothing is saved as the default. Moving the cursor clears the row.

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
counts the running ones (`← 2 agents`; `↓ 2 agents` when sessions run in
detached hosts, where `←` opens the [session list](#session-list) and `↓` is
the key that reaches the rows); a lingering finished row is not counted.
The keyboard starts in the composer; `↓` moves it to the rows, where the
selected row carries the `❯` mark, and `↑` past the first row, `Esc` or any
typed key move it back. Returning from the sub-agent view leaves it on the
rows, marking the row the view came from. Rows sit in spawn order and never reshuffle
while a sub-agent streams, so `↑`/`↓` aim at a fixed target; a finished row
keeps its slot until it leaves the band, and the rows below close up. Past
the cap the band ends in `… +n more`; the viewed sub-agent always keeps a
visible row, taking the last one when it would otherwise fall behind the cap.

On a provider that can stop one child (native today), `Delete` or
`Backspace` on the selected **running** row stops just that child; the turn
goes on and the parent reads that the user stopped it. On any other row —
grok, cursor, or a finished row — the key goes to the composer as before.
This works the same on a background sub-agent's row (below).

### Background sub-agents

On native, a sub-agent the model started with `run_in_background` shows in
the same band, marked `bg` first in its suffix — a running row reads `bg ·
0s · 15 tok`, a finished one just `bg · <model>` with no duration, since its
row went final at the call's own acknowledgement, before the child ever
started. `Delete`/`Backspace` stops a running background row exactly like
any other.

A background child that finishes while its own turn is still running is
steered into the agent's next step, as if it had answered in place. One
that finishes after the agent has gone idle **wakes** it: craze starts a
turn of its own that heads the transcript with "sub-agent finished — the
agent continues" and delivers the result — with no working spinner, since
the status row stays idle for it (the terminal tab title still marks it,
as it does grok's own foreign-turn fallback). A prompt typed while the wake
runs is queued, and sent once the wake ends. `Esc` stops a running wake —
any turn the agent runs on its own — when nothing of craze's own is
working, leaving a `cancelled` note. Headless `craze prompt` never
sees any of this: every `run_in_background` call there runs in the
foreground, as if the flag had not been set.

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
- **Grok and gx** show no tabs in the [model dialog](#model-dialog) right
  after a resume — their `session/load` result carries no catalog, unlike a
  fresh `session/new`. A later live turn may supply one again.
- A rename is craze's own and is never sent to the agent: `agent ls` and
  `grok --resume` still show whatever title the agent itself gave the
  session.

### Native sessions

The native provider (`--provider native`) resumes the same way as
above, over its own transcript in place of an agent's `session/load`:

- **Tool cards** have no exit code, diff or truncation metadata to restore —
  only the stored output text — so a replayed card's body ends with a
  `(replayed)` line: a collapsed row still previews its output's first line,
  an expanded one ends with the label, and a card with no body of its own
  (an edit, or an `agent` call) carries the label unseen.
- **The model** is the transcript's last one, matched by identity (provider
  and wire model) rather than restored by name — a re-pointed alias never
  silently switches models; one no longer available falls back to the model
  table's default and warns on stderr, naming the alias it fell back to and
  why. **The mode** (agent/plan/ask) is the transcript's last `mode → <id>`
  change, unless `--plan`/`--ask` set it explicitly; an unrecognised last
  mode restores agent mode instead, warning the same way.
- **The todo list** is restored from the last tool entry that recorded one.
- As with an ACP load, replay shows text output only: a foreground `agent`
  call folds to a `task` card with the child's final text, a background one
  to its launch receipt, and no sub-agent rows come back — there is no live
  view behind them to reopen.

The title is kept, and — native sessions being indexed now —
[`/rename`](#slash-commands) persists it back to the index like any other
provider's. A session that compacted at some point replays those notes in
place too; see [Compaction](#compaction).

#### Which model a session starts on

A **new** native session starts where you left off: a model or effort picked
in any native session's [`/model`](#model-dialog) is remembered, and the next
session with no `--model` starts on the newest remembered model whose provider
has a key, at the effort last used on it. With no remembered model it can use,
it starts on the model table's default, and if that has no key, on the first
model that has one, with a note. `--model` picks the model for one start — at
the effort remembered for that model — and is never remembered itself. A
resume is never moved by the memory: it keeps its transcript's model and
effort, as above.

The memory is read once, when the session starts, so a switch changes where
the *next* session starts, not what the running one offers. It lives in
`recent.json` beside the model files; see [Model
memory](configuration.md#model-memory-recentjson) for the file, who writes it,
and what happens when two sessions switch at once.

#### What the model is told

Native's system prompt is craze's own text: no sentence is reproduced whole
from another agent's, though a few sentences are adapted from wording in the
credited reference prompts (grok-build, opencode, and codex; see the
`opencode` tool profile's NOTICE for the full list). After it come the
resolved project and user instruction files, and the
skills-and-commands catalog — the same one the [slash menu](#slash-commands)
is built from. The prompt's last section is a session-start snapshot: today's
local date, and — only when the workspace sits inside a git work tree — the
branch, the default branch, the status, and the last few commits, taken once
when the session opens or resumes and never updated afterwards; outside a git
work tree nothing runs, and the git part of the snapshot is left out. No part
of the prompt's own text is reproduced here.

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
| `Enter` | with the [`@` file popup](#file-mentions) open on a candidate, pick it; with the slash menu open on a token that is not already typed out in full, accept the highlighted row; on a draft that starts with `!`, run it in your own shell (see [Shell mode](#shell-mode)); otherwise send the draft, or **queue** it while a turn is running (see [Queued messages](#queued-messages)) |
| `Ctrl+L` | the strong send: on Grok, add the draft to the running turn without cancelling it; on Cursor, cancel the running turn and send (it asks first). On an idle session it is a plain send |
| `Alt+Enter`, `Ctrl+J` | newline (see below) |
| `Esc` | answer the card on top; close a dialog (`/help` included); leave the sub-agent view; kill a running `!` command; clear a `!` draft; hide the slash menu or the [`@` file popup](#file-mentions) for the token under the cursor — a second `Esc` then cancels the running turn; otherwise cancel the running turn (the transcript says `cancelled`). An Esc pressed immediately after Enter cancels that turn; craze never writes the cancel ahead of the prompt |
| `Ctrl+C` | kill a running `!` command, and nothing else — it is the only way to stop one while a card has the keyboard. With none running: cancel the running turn **and everything queued behind it** — the queue, a confirm on screen, a send-now waiting to fire; a second press within one second ends the session; it ends it outright when idle or after an error. Inside the sub-agent view it still cancels the **main** turn, and the view stays open |
| `Ctrl+D` | ends the session, always |
| `Shift+Tab` | cycle the ACP mode (agent / plan / ask); inside the sub-agent view, switch to the previous sub-agent instead |
| `Ctrl+T`, `/tasks` | tasks panel: compact → expanded → hidden |
| `Ctrl+G`, `/theme` | theme picker |
| `Ctrl+O` | expand / collapse transcript detail (diff hunks, command output, thoughts) — works inside the sub-agent view too |
| `Ctrl+Y` | copy the mouse selection, or the last reply when there is none (works with `--no-mouse`); inside the sub-agent view, copy from it |
| `↑` `↓` | move the keyboard out of the composer (draft or not) and then between rows: the selected row carries a `❯` gutter mark and the composer loses its cursor. `↑` reaches the queue band first and the sub-agent rows when nothing is queued; `↓` reaches the sub-agent rows first and the queue band when there are none. `↑` past the first row, `Esc`, or typing anything returns to the composer; inside the sub-agent view, scroll it; with the slash menu or the [`@` file popup](#file-mentions) open, move its highlighted row instead, wrapping at either end; in a dialog, move its selection (in `/help`, scroll the box) |
| `Enter` while the sub-agent rows have the keyboard | open it in the main area, read-only (see [Sub-agent view](#sub-agent-view)) |
| `Backspace` / `Delete` on a focused **running** sub-agent row, or inside a running sub-agent's view | stop that one sub-agent (native only); on any other row or provider the key falls through as if unhandled |
| `Enter` while the queue band has the keyboard | edit that message in place — its text loads into the composer, `Enter` saves, `Esc` restores the draft |
| `Backspace` / `Delete` on a queued row | cancel it |
| `Ctrl+L` on a queued row | send it now instead of the running turn (it asks first) |
| `←` on an empty composer, `/sessions` | open the [session list](#session-list) — only when sessions run in detached hosts (the default); under `CRAZE_DETACH=0` the key reaches the composer as it always has |
| `Esc` or `←` inside the sub-agent view | return to the main transcript; entering or leaving cancels nothing |
| `Tab` inside the sub-agent view | switch to the next sub-agent |
| `PgUp` / `PgDn` | scroll the transcript, or page the `/help` box; page the slash menu instead when it is open — the menu takes priority over both |
| wheel | scroll the transcript three lines a notch; over an open slash menu, move its selection one row a notch instead — it selects, it does not page (see [Mouse](#mouse)) |
| `Tab` | with the slash menu open, accept the highlighted row (see [Slash commands](#slash-commands)); with the [`@` file popup](#file-mentions) open, open the highlighted folder or pick the highlighted file; elsewhere a no-op |

`/help` lists the same keys plus every slash command — see
[Help dialog](#help-dialog). `/exit` ends the session; there are no bare `q` or `?`
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

## File mentions

`@` at the start of a word in the composer — the start of the draft, or after
a space or a newline — opens a popup of the session's workspace, its files and
folders, in the band above the composer where the slash menu opens:

```text
───────────────────────────────────────────────────────── files in craze ─
❯ internal/config/
  internal/tui/colors.go
  internal/tui/complete.go
  internal/tui/composer.go
  internal/config/config.go
  internal/tui/composer_at.go
  internal/tui/complete_test.go
  internal/tui/composer_test.go
  ↓ 3 more
───────────────────────────────────────────────────────────────── craze ─
❯ explain @co
```

Typing narrows it. What you type after the `@` matches a path when its
letters appear in it in order, anywhere, ignoring case; a file or folder whose
**name starts with it** comes first, then matches at the start of a path
segment and in unbroken runs, and on a tie the shorter path. A `/` in it
narrows to that folder: `@internal/tui/` lists what is inside `internal/tui/`,
and anything typed after the last `/` matches inside it. The popup lists the
workspace alone — `@/`, `@~/` and `@../` offer nothing. Folders end in `/`.

| Key | Action |
|---|---|
| `↑` `↓`, `Ctrl+P` `Ctrl+N` | move the highlight, wrapping at either end |
| `Tab` | on a folder, open it — `@internal/` is written with no space after it, and the popup stays open on what is inside; on a file, pick it. It never sends |
| `Enter` | pick the highlighted entry. With nothing to pick — nothing matches, or the list is still loading — `Enter` is the composer's and sends the draft as typed |
| `Esc` | hide the popup for this word; the draft is left alone, and a second `Esc` cancels a running turn. Moving the cursor, within the word or away and back, keeps it hidden; typing into the word, or starting another, brings it back. Editing a [queued message](#queued-messages), `Esc` still cancels the edit |

While it is open the popup takes those keys from the composer — `↑`/`↓` do
not move the keyboard to the queue or the sub-agent rows, and `Ctrl+P`/`Ctrl+N`
do not move between the draft's lines. Everything else is the composer's:
`Alt+Enter` starts a new line (which ends the word, and closes the popup),
`Ctrl+L` is the strong send, `PgUp`/`PgDn` scroll the transcript. It has no
mouse: a click on it does nothing. Only the `@` word the cursor is in is
completed; a draft can mention any number of files. An `@` inside a word
(`user@example.com`) opens nothing, and neither does a draft in [shell
mode](#shell-mode), a card, a dialog or the sub-agent view. It works the same
however the session runs — a detached host, `CRAZE_DETACH=0`, `craze attach`
— and the list is always read on this machine, in the session's workspace.
`/help` does not list it.

### What is inserted

A pick replaces the word with `@`, the path relative to the workspace, and
one space: `@internal/tui/app.go `. A path with a space in it is quoted, with
`\"` and `\\` escaped inside the quotes — `@"docs/guide/getting started.md" `
— and a folder ends in `/`: `@internal/tui/ `. That text is exactly what the
agent receives: craze expands nothing and attaches nothing — the file's
contents are not sent with the message. In the popup's row, a path's Unicode
default-ignorable and format characters — a right-to-left override, a
zero-width space, a variation selector, a Hangul filler: characters a terminal
draws as nothing, as a blank, or by reordering what follows — are shown as
their code points (`<U+202E>`), and a run of spaces as one; the rest of the
name, an accent included, is drawn as it is. The pick writes the name exactly
as it is.

What happens next is the provider's. **grok** reads the `@path` in the message
itself and attaches the whole file to what it sends its model — the message
unchanged, the file's contents beside it — so no tool row appears. **cursor**
and **native** receive only the text and read the file with their read tool:
a `✓ read  README.md` row before the reply (native sometimes searches for the
file first).

### Where the list comes from

The first `@` of a popup lists the workspace once, in the background, and
every keystroke after it matches against that list while the popup stays
open:

1. `rg --files --hidden -g '!.git'` when `rg` is on your `PATH` — the files
   ripgrep would search: `.gitignore`, `.ignore` and git's excludes honoured,
   dotfiles shown, `.git` never, and your `RIPGREP_CONFIG_PATH` not read
   (`--no-config`);
2. otherwise, in a git work tree, `git ls-files -co --exclude-standard` —
   tracked files, and untracked ones git would not ignore;
3. otherwise a walk of the folder, which skips `.git` and `node_modules` and
   neither follows nor lists a symbolic link.

Folders are derived from the files' paths, so an **empty folder is never
listed** — nor, under `rg` or `git`, a folder whose every file is ignored. A
path with a control character or invalid UTF-8 in it is left out: it could
not be written into a mention.

| Bound | Value | When it is reached |
|---|---|---|
| Paths read from `rg` or `git` | 50,000 | the rest are not listed, and the title says `only the first 50,000 files` |
| Entries the walk reads | 20,000 | the title says `only the first 20,000 entries` |
| Time | 3 s for the whole listing | what was read is offered, the title saying `listing stopped after 3s`; with nothing read, the popup says `listing files took over 3s` |
| Matches handed to the popup | the best 100 | the rest are counted in `↓ N more`: type more to narrow |

The popup says `searching…` while the list is read, `nothing matches` when
nothing does, `no files here` in an empty workspace, and `could not list
files: <why>` when the listing failed.

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

To send a message that begins with `!`, type a single space before it. A
draft whose first character is a space is not shell mode, and that space is
trimmed on the way out, so a draft of space followed by `!important` reaches
the agent as `!important` — the space is how you say which of the two you
meant, and it never travels.

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
read-only. Grok and native each stream a sub-agent's own session, so its
transcript is the child's own prompt, thoughts, tool calls and replies,
rendered by the same machinery as the main one. Cursor streams no sub-agent
transcript, so the view is what its `cursor/task` receipt carried: a note
saying so, the prompt, a `model · duration · agent id` line once the receipt
landed, and the output when there is one. There is no composer here — you
cannot prompt or message a sub-agent from craze. On native,
`Delete`/`Backspace` in a running child's view stops it (see the banner
below); a grok or cursor sub-agent cannot be stopped from craze.

The composer's top rule carries the chip `(model) description` —
`(grok-4.6) List directory files` — and the input line is replaced by the
banner:

- running: `○ @explore · read-only · esc to return`, with `· tab next agent`
  when more than one row is visible, and, on native, a further
  ` · del to stop` at the tail's end — the full banner then reads exactly
  `○ @general-purpose · read-only · esc to return · tab next agent · del to
  stop`
- finished: `✓ @explore · completed · esc to return`, in the warning colour
  so the end is unmistakable (`✗ failed` / `– cancelled`, with the error when
  there is one; the `esc to return` half never truncates away) — a child the
  user stopped on native reads exactly `– @general-purpose · cancelled ·
  stopped by the user · esc to return`
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

## Session list

`←` on an empty composer, or `/sessions`, replaces the screen with every
session of yours running on this machine, whatever its directory, and the
saved ones that are not running. It exists only when sessions run in detached
hosts (the default); under `CRAZE_DETACH=0`, `detach = false` or with the
control socket off there is no list, no `/sessions` and no `←` binding (and
`/help` has no line for it). The session you came from stays attached behind
the list — its turn keeps running, a card it raises waits for you — and the
cursor starts on its row. If it ends while the list is up (another client's
`/exit`, a close from the list, the idle timeout), craze stays on the list and
its row stays, `· ended`, for as long as the list is up, even once its host
has gone; a later run of the same session is a row of its own.

```text
 sessions  9 running                             ! 2 need you   ✳ 3 working   ✗ 1 failed   ○ 2 idle

needs you 2 ─────────────────────────────────────────────────────────────────────
  ! fix the roost tab rename      permission: cargo test -p roost-ipc    grok    roost            2m

working 3 ───────────────────────────────────────────────────────────────────────
❯ ✳ write the v0.1.0 release no…  Responding                             native  craze · here     1m
```

The header counts what needs you, what is working, what failed and what is
idle, dropping counts from the right as the terminal narrows. Rows are grouped
by state — **needs you** (an open permission, question or plan, with what it
is about), **working** (what it is doing: the running tool, `Responding`,
`Thinking`, or `Starting…`/`Loading…`/`Closing…`), **failed** (the error's
first line), **idle** (the first line of its last reply, or `waiting for a
prompt`), **unreachable** (`?`: its host is registered and not answering) —
then `▸ saved · N not running`, collapsed. Within a group the row that
changed state most recently is first. Each row reads: state glyph, title, what
it wants, provider, directory (`· here` for the session you came from), and
how long it has been in its state. A session run by an older craze is listed
with what that craze reports, and its version on the row (`· craze 0.0.9`).
A host that has not answered yet is drawn with the working rows, `Starting…`
or `Connecting…`. With no other session the list says `No other sessions.
Type a prompt below to start one.` It needs 40×10; smaller, it says so.

Under the rows is an input, always focused, where a new session is started
([Starting a session from the list](#starting-a-session-from-the-list)): a
prompt typed there and `Enter` starts one, `@` picks the directory it runs in,
and `/provider` and `/model` choose what it runs. The rule above the input
names where and as what the next one would start:

```text
─────────────────────────── new session → ~/projects/lumen · cursor · Composer 2.5 ─
❯ type a prompt to start a session · @ picks a directory · / provider and model
```

| Key | Action |
|---|---|
| typing | goes to the input under the list: a prompt, an `@` directory, a `/` command |
| `↑` `↓` | move the selection; it stays on its session however the rows reorder (with a popup up over the input, they move its selection instead) |
| `Enter` with something typed | start it: see [Starting a session from the list](#starting-a-session-from-the-list) |
| `Enter`, `→` with nothing typed | on another session, [open it here](#opening-a-session-in-place); on a saved one, [resume it here](#saved-sessions); on the session you came from, go back to it; on `▸ saved`, expand or collapse the saved sessions. A host that is not answering says `that session is not answering` |
| `Esc` with something typed | hide the input's popup, then clear the input |
| `←` `→` with something typed | move the input's cursor (`Alt+←`/`Alt+→` by word) |
| `Esc`, `←` with nothing typed | back to the session you came from — unless it ended while the list was up, when the list stays and says `that session ended`, or this terminal lost its connection to it, when the list stays and says `lost the connection to that session` |
| `Ctrl+X` | on a working or asking session, stop its turn **and clear its queue**; on an idle or failed one, the first press arms a close (`ctrl+x again closes it`) and a second within two seconds ends the session on its host — any other key disarms it. Nothing on a saved row or a host that is not answering |
| `Ctrl+S` | group by directory instead of by state, and back; the grouping is kept for the rest of the run |
| `Ctrl+D`, `Ctrl+C` twice, `/exit` in the input | quit craze; every session keeps running |

The hint line under the list names what the selected row takes (`enter
open`, `enter resume`, `enter back to it`, `ctrl+x stop` or `close`), and
says what a key came to: `stopped: <title>`, `closed: <title>`, `could not
stop <title>: …`. The mouse does nothing in the list.

### Opening a session in place

`Enter` on another session's row opens it in this terminal: the list stays up
with `opening <title>…` on its hint line while its host is dialled, and then
the screen is that session's — its transcript, its cards, its queue, the
status row naming its directory and provider — as if you had started it here.
The session you left is not stopped: this terminal detaches from it and it
goes on on its host, where the list finds it again. A dial that fails leaves
the list up with `could not open <title>: <why>`; `Enter` on another row
before the first answers opens that one instead, and leaving the list
abandons the open.

Once the list has been opened, every session's screen starts with a band — the
session's title, provider and directory, and `← sessions` on the right:

```text
─ fix the roost tab rename · grok · roost ─────────────────────── ← sessions ─
```

A terminal too short for it drops it. `←` on an empty composer goes back to
the list, where the cursor starts on the session you now have open (`· here`).

The composer's text belongs to the session it was typed in: a session that
ends with a draft in the composer keeps it — it is in the composer again when
you open that session again — and it never follows you to another session.

When the session you are in ends — its host stopped by another terminal's
`/exit`, a `ctrl+x` close from a list, or the idle timeout — craze goes back
to the list, its row marked `· ended`, and the hint line says `that session
ended`. Your own quit (`/exit`, `Ctrl+D`) still ends the session and quits
craze.

When this terminal loses its connection to the session instead — its host
stopped answering for longer than craze keeps reconnecting — craze goes back
to the list too, and the hint line says `lost the connection to that session`
and why. The session may still be running: its row is listed as its host
reports it — or `?` while the host does not answer — and `Enter` on it opens
it again, `Ctrl+X` stops or closes it, as on any other row.

### Saved sessions

`Enter` on `▸ saved · N not running` shows the saved sessions: every session
in the session index that is not running, newest first, at most 50, each
once — `·`, its title, provider and directory. `Enter` on one resumes it in
this terminal: a session host is started in the directory the session ran in
(not this terminal's) and loads it, the hint line saying `resuming <title>…`
meanwhile, and the screen then shows it as [opening one in
place](#opening-a-session-in-place) does, `restoring…` until its transcript
is back. A saved session that another terminal has resumed meanwhile is
opened where it runs. The command line's own session flags (`--provider`,
`--model`, `--ask`, `--plan`) were for the session it started and are not
applied to a resumed one; its permission mode, `--plugin-dir` and (for a
provider craze does not run in process) `--agent-bin` are. A
session whose directory is gone, or whose provider this craze cannot resume,
is refused on the hint line (`could not resume <title>: that session ran in
…, which is no longer a directory`) and nothing is started. `Ctrl+X` does
nothing on a saved row: nothing runs to stop.

### Starting a session from the list

What is typed in the input under the list is the first prompt of a new
session. `Enter` starts it **in the background**: the list stays up, the input
says `starting…` and takes nothing more until the session has answered (a
second `Enter` does nothing), and a session host is spawned in the target
directory, started, and given the prompt. The hint line then says what came
of it:

- `started in ~/projects/lumen` — the session took the prompt; the input is
  cleared, and its row appears with the list's next refresh. It runs on its
  host like any other session, and `Enter` on its row opens it here.
- `could not start a session in ~/projects/lumen: <why>` — no session came up
  (the host could not start, the agent refused the model, …) or the session
  refused the prompt; the input keeps what was typed, and a host that was
  spawned is stopped.
- `may have started — check the list` — the connection went after the prompt
  was sent, so the session may have taken it; the host is left running and
  the input keeps the prompt.

`Esc` or `←` while it starts leaves the list; the start carries on, and its
row is listed as any other.

The new session runs:

- **where** the rule says: the directory a leading `@` token names (below);
  else the selected row's directory — a running session's or a saved one's;
  else the directory of the session you came from. With none of them the
  rule says `no directory to start in: pick one with @`.
- **as** the rule says: the provider and model chosen with [`/provider` and
  `/model`](#provider-and-model), else those of the session you came from —
  its current model; when its host has not said which model it is on, the
  launch's own `--model`; else the provider's default.
- with the **permission mode** of the session you came from (`--force` or
  `--no-force`, as its host reports it), the launch's `--plugin-dir`, and —
  for a provider craze does not run in process — its `--agent-bin`. The
  command line's own `--ask`, `--plan` and `--provider` were for the session
  it started and are not applied, and its `--model` only in the one case
  above.

All of it is decided when `Enter` is pressed: moving the selection while the
session starts changes nothing.

**`@dir` alone opens an unstarted session.** `Enter` on a leading `@` token
with nothing after it opens a new session in that directory in place, and
spawns nothing yet: the band says `new session · <provider> · ~/projects/lumen`,
the status row names its directory, provider and model, and the composer says
`type the first prompt to start this session`. Its first prompt spawns its
host there and it comes up in this terminal like any session opened in place,
the prompt sent as your first turn. If it cannot start, the session is
unstarted again with the error on screen (`could not start the session: …`)
and the prompt still in the composer. Slash commands and `!` do nothing
until the session exists. `←` or `/sessions` before its first prompt discards it:
nothing was spawned and nothing is left.

`/exit`, alone, in the list's input quits craze and leaves every session
running; `/exit` with anything after it is a prompt like any other.

#### Choosing the directory: `@`

Only an `@` token that is the **first thing** in the input picks the
directory, and it is taken out of the prompt the agent receives (`@lumen tidy
the changelog` sends `tidy the changelog`). An `@…` later in the input is
ordinary prompt text, and text typed before a token makes it ordinary text
too — the rule then falls back to the selected row, live.

While the cursor is in the leading token a popup above the input offers:

- **by name** — the directory of the session you came from (`here · 2
  running`), then every directory a session is running in, the most recently
  active first (`1 running`), then the ten most recent directories in the
  [session index](configuration.md#session-index) that still exist (`used 3h
  ago`). Typing narrows them: a name that starts with what is typed first,
  then a path that contains it. Nothing is hard-coded: there is no projects
  folder and no setting.
- **by path** — a token that starts with `~`, `/` or `.` browses: `@~/`
  lists the subdirectories of your home directory (`folders in ~`), those
  whose names start with what follows the last `/`, sorted, at most 200.
  Dot-directories are listed only once what is typed starts with `.`. `Tab`
  descends into the highlighted one and stays open; `Enter` picks it. A
  directory that is not there says `no directory ~/x`.

`↑` `↓` (or `Ctrl+P` `Ctrl+N`) choose, `Tab` completes (it never starts
anything), `Enter` picks, `Esc` hides the popup until the token changes.
Picking writes the directory's name (`@lumen `) — its path when two
candidates share a name, and `@"~/my dir" ` quoted when it has a space — and
**binds** the token to that directory, whatever the list does meanwhile.
Editing or deleting the token drops the binding; `Enter` then reads the token
afresh — a name that is exactly one candidate's, or a path that is a
directory — and one that names none, or more than one, is an error on the
rule and the hint line (`no directory named @x`, `@x names 2 directories;
pick one with @`), and nothing starts.

### Provider and model

`/` as the first thing in the list's input opens its commands:

```text
───────────────────────────────────────────────────────── commands ─
❯ /provider  provider for new sessions             cursor
  /model     model for new sessions                Composer 2.5
  /exit      quit; sessions keep running
```

`/provider` and `/model` set what every session started from the list runs,
until you change them or craze quits — across every opening of the list and
whichever session you came from. The rule over the input names them, and the
hint line says `new sessions use cursor · Composer 2.5` when one is chosen.
Choosing a command lists its values; `↑` `↓` choose, typing narrows,
`Tab` or `Enter` uses the highlighted one, `Esc` hides the popup. A `/` word no
command starts with is a prompt (an agent's own slash command), and opens
nothing.

- **`/provider`** lists the providers the startup [provider
  dialog](#tui-reference) offers — `gx` only where its binary resolves — the
  one in use marked `current`. Choosing one **resets the model** to that
  provider's default: for cursor, grok and gx, the agent's own (`default`: no
  model is passed); for native, the model a native session started with no
  `--model` would use ([which model a session starts
  on](#which-model-a-session-starts-on)) — the newest remembered model whose
  provider has a key, else the model table's `default_model`, else the first
  model (alphabetically) whose key resolves. With no provider funded it says
  so (`no model provider has an API key — run craze auth login, …`) and leaves
  the model to the agent's own.
- **`/model`** lists the provider's models. Native's are its model table's,
  only those whose provider has a key, the recently used ones first in the
  order they were last picked and the rest by name, with no label — the same
  order a native session's own `/model` lists. The list reads the memory and
  never writes it: only a switch made inside a native session does.
  cursor, grok and gx name their models only once a session has started, so
  craze lists **the catalog the last session of that provider installed** —
  every detached host records it in the [model catalog
  cache](configuration.md#model-catalog-cache) — titled with its age (`cursor
  models for new sessions · last seen 3h ago`), the agent's own `default`
  first — always offered, and no model is passed with it; a model of the
  catalog whose id happens to be `default` is a row of its own, and passes
  `--model default`. A catalog can be stale: it is a list of choices, nothing
  more. With
  no catalog recorded yet the popup says `no cursor catalog seen yet · enter
  uses the id as typed`: `/model <id>` then takes the id as typed, as `craze
  --model <id>` would, and so does `Enter` on an id no listed model matches. A
  bad id fails that session's start the way `--model` does, on the hint line.
  A model is chosen for the provider it was listed for: a list opened later
  over a session of another provider still starts that provider on it.

Typed in full, `/provider grok`, `/model gpt-6-sol` and `/model default` (the
provider's own — a catalog model whose id is `default` is picked from the
popup) do the same without the popup; a provider the picker does not offer is
refused on the hint line.

A session started from the list is saved as the last provider started, as any
launch is (see [Configuration](configuration.md)), so the next `craze` without
`--provider` preselects its provider in the provider dialog. The list's own
choice still lasts only until craze quits.

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

On the native provider (`--provider native`) the same `/plan`,
`/ask`, `/agent`, `Shift+Tab` and the offer above work. Plan mode lets the
model edit only its plan file, which lives under the harness home beside the
session transcript — by default
`~/.craze/native/sessions/<workspace-slug>/<stamp>_<id>.plan.md`, or under
`$CRAZE_HOME/native/` when the home is relocated — and is never deleted by
craze. Ask mode denies every edit, write and shell command.
Accepting the plan card ends the turn and arms the offer above, even when
the model said nothing else in that turn.

### Sub-agent models

On native, `~/.craze/native/models.toml` (or `$CRAZE_HOME/native/` when the
home is relocated) may carry an optional `[subagents]` section: a default
`model` and `effort` for children, and a `[subagents.tiers]` map from
Claude's tier names (`fable`, `opus`, `sonnet`, `haiku`) to native aliases.
The parent's `agent` tool call resolves a child's model and effort in this
order — the call's own `model`/`effort`, then the persona's, then
`[subagents]`'s default, then the parent's own running model and effort —
and `model` (on the call, a persona, or `[subagents]`) may also be `inherit`,
which always means the parent's model. A tier name with no entry in
`[subagents.tiers]` means the parent's model too, so a persona that says
`model: opus` still works with no tier map configured at all. Every alias
named in `[subagents]` or `[subagents.tiers]` should be a model — one craze
ships or one in `models.toml`; one that is not falls back to the default,
never stopping the session: silently for a model a release retired, with a
warning for any other unknown name (see [Sub-agent settings](configuration.md#sub-agent-settings)). `inherit`
itself is refused as a tier key, since a tier mapping to `inherit` could never
be reached. For example:

```toml
[subagents]
model  = ""   # optional default for children; "" = the parent's
effort = ""   # optional default effort
[subagents.tiers]   # optional: what a Claude tier name means here
opus   = "fireworks/kimi-k3"
sonnet = "glm-5.3"
haiku  = "fireworks/deepseek-v4p1-flash"
```

The `agent` tool runs at most four children at once; a fifth call waits for
a slot to free.

## Compaction

On the native provider (`--provider native`), a long session
summarizes its own context rather than growing it forever. Once the context
reaches 85% of the model's context window — capped at the window less its
[output ceiling](configuration.md#native-output-ceiling) — craze summarizes the
conversation so far and carries on from the summary in place of it. The
check runs before a turn's first request and again after every step that
called a tool, so a turn that crosses the line partway through splits into
two requests without ending: the same turn, one final `done`. Separately, a
request the provider refuses outright as too large for the model's context
window compacts once and retries, on the same turn — a second such refusal,
or the compaction itself failing, fails the turn instead, its message saying
so ("even after compacting" when a compaction did run first). Switching to a
model with a smaller context window compacts on the *previous* model first,
so its own context and prefix cache are what gets summarized, falling back
to the new model when the previous one no longer resolves. A model with no
known context window (`context_window` unset in `models.toml`) never
compacts on its own — `/compact` below and the provider-refused case above
still work. See [`[compaction]`](configuration.md#native-compaction) in the
configuration reference for `auto`, `threshold_percent` and `tail_tokens`.
Compacting on its own switches itself off after a compaction that failed or
still left the context over the threshold, and back on after one that
succeeded under it, or a model change.

While a compaction runs, status row 1 reads `compacting context…` in place
of the model or the elapsed time. Once it ends it leaves one note in the
transcript:

| how it ended | note |
|---|---|
| on its own | `context compacted · 890k → 21k tokens` |
| `/compact` | `context compacted on request · 890k → 21k tokens` |
| the provider refused the request | `context was too large — compacted · 890k → 21k tokens` |
| failed | `compaction failed: <why>` |

Token counts are plain under 1,000, else `k` or `M` to three significant
figures (`890k`, `1.21M`).

### `/compact`

Native advertises one [slash command](#slash-commands), `/compact [focus]` —
"Summarize the conversation so far to free context; `/compact <what to
keep>`" — so a plugin's own `compact` is offered qualified, the same rule any
name collision follows. Typed as a prompt, `/compact` is never sent to the
model: it is a turn of its own, ending as soon as the compaction does, with
no model turn after it. `/compact` with nothing in the context yet says so
("nothing to compact yet") instead of compacting an empty conversation.
Sent while a turn is already running, `/compact` is never sent into it as a
steer — like any interjection it is refused, which keeps the draft rather
than sending it (`Ctrl+L` on it shows "nothing to interject into"). Nothing
queues it automatically: pressing `Enter` on the kept draft is what queues
it, to run as its own turn once the current one ends. A
`/compact` that succeeds turns compacting-on-its-own back on only when it
leaves the context under the threshold; if the context is still at or over
the threshold afterward, automatic compaction stays off (or turns off, if it
was not already).

### Resuming a compacted session

[Resuming](#native-sessions) a session that compacted at some point replays
its notes in place, exactly where they happened in the original session, and
the next turn's request to the model carries the summary in place of
everything it stood for, with the kept tail — the most recent whole steps
(a step is never split from its own tool results), up to `tail_tokens` and
never more than a quarter of the threshold, and possibly empty — following it
unchanged.

### Segment files

Before it writes the summary, craze saves what the summary stands for to a
Markdown file beside the transcript — `<stem>.compaction/segment_001.md`,
`segment_002.md`, and so on, one file per compaction — which the model can
`read` or `grep` like any other file under the harness home, exactly as it
can the [plan file](#modes). The segment keeps each tool result's first 8 KiB
(a note says how much was left out), and the file itself is capped at 512
KiB: over that, the oldest steps are dropped first — which can drop only
part of a turn, not the whole thing — with a line pointing at the
transcript instead — which, unlike the segment, keeps everything, so an
exact detail can still be found there when the segment no longer has it. The
summary message names the directory, so the model knows the files are there
when an exact detail — a command, an error string, a path — matters more
than the summary's own account of it.

## Usage and cost

On the native provider, status row 1 gains a usage part after the model name
once the session has taken a step — how full the context is, and what it has
spent, the turn's amount then the session's:

| state | text |
|---|---|
| priced | `34% ctx · $0.04 / $1.20` |
| some usage had no price | `34% ctx · $0.04 / $1.20+` |
| nothing priced at all | `34% ctx · 12.3k / 1.21M tok` |
| a window craze does not know | `$0.04 / $1.20` (the `NN% ctx ·` prefix is left out) |
| no usage yet (ACP, or before native's first step) | no part at all |

Money is to the cent, rounded half up, and reads `<$0.01` for an amount
above nothing that would otherwise round to nothing. A `+` on an amount
means at least some of the usage behind it had no [price](configuration.md#native-cost)
in `models.toml` — it is a floor, not the true cost. With nothing priced at
all, the row falls back to billed tokens instead of a dollar figure: input,
cache, and output, added together. The turn's amount is what the turn the
last report followed spent; the session's is everything the session has
spent, including its compactions' usage and its sub-agents', each priced by
its own model. Row 1 drops its parts in a pinned order as the terminal
narrows — elapsed first, then the branch, then this usage part, then the
provider, and the model last of all.

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

A question card's option may carry a description, drawn dimmed on its own
line under the label.

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
| `/connect` | Store a model provider's API key — [native sessions only](#connect) |
| `/clear` | Clear transcript |
| `/tasks` | Tasks panel: compact, expanded, hidden |
| `/theme` | Theme picker, or `/theme <name>` |
| `/rename <title>` | Rename this session — craze's own; never sent to the agent |
| `/plan` | Set plan mode |
| `/ask` | Set ask mode |
| `/agent` | Set agent mode |
| `/sessions` | The [session list](#session-list) — listed only when sessions run in detached hosts |
| `/exit` | End this session |

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

### `/connect`

`/connect` gives one of the native provider's model providers an API key
without leaving the TUI — the dialog twin of [`craze auth
login`](cli.md#craze-auth-login), storing the key the same way, as that
provider's `api_key` in `providers.toml` (see
[Keys](configuration.md#keys)). Like `craze auth login` it never checks the
key with the provider: a wrong one shows as an error on first use. It exists
in native sessions only: in a cursor, grok or gx session it is not in the menu,
and a typed `/connect` goes to the agent as ordinary text. It is also what the
last row of native's [`/model`](#model-dialog) opens, while some provider has
no key.

- **Step one, `Connect a provider`**, lists every provider craze knows — the
  shipped ones and any in your own `providers.toml` — by name, a `✓` beside each
  one that has a usable key, from its variable or stored; connecting one that
  already has a key replaces its stored key. A provider whose stored key cannot
  be used (shorter than 8 bytes, or overlapping the redaction marker) is marked
  `stored key unusable`. The box opens on the first provider with no key.
  `↑`/`↓` move, `Enter` or a click picks, `Esc` closes.
- **Step two, `<Name> API key`**, is a masked field: what you type or paste is
  drawn as `•`, never as text. Under it, where the key is stored — `Stored in
  ~/.craze/native/providers.toml.`, or wherever this TUI's `CRAZE_HOME` puts it —
  and, when one of the provider's variables is set in this TUI's environment,
  `<VAR> is set in this environment; craze uses it before the stored key.`
  A terminal paste and `Ctrl+V` both land in the field and nowhere else: a
  clipboard paste that answers after you have left the field, or opened another
  one, is dropped — never put in the composer. `Esc` goes back to step one and
  empties the field.
- **`Enter` stores the key.** An empty key, one shorter than 8 bytes, one that
  overlaps the redaction marker, or one over 8 KiB is refused in the field —
  `Not saved: …`, naming the rule, never the key — the field is emptied, and
  nothing is written. Otherwise the box closes and the transcript says
  `Connected <Name>. New sessions offer its models; to use them in this
  conversation, /exit and run craze -c.` Another provider's stored key that
  cannot be used is kept as it was, and named in a note after it. A store that
  refuses — `providers.toml` a symlink, a file that no longer parses, a lock that
  cannot be taken — is an error row naming the problem, never the key.
- **The key is never shown**: not in the transcript, a note or an error row, the
  composer, the session's journal, or anything sent to the model. The field is
  emptied on every way out of the dialog — a save, `Esc`, a refusal, a card
  arriving, another dialog opening, switching to another session, quitting
  craze, or the session ending or this terminal losing its connection to it
  (the box closes before craze goes back to the session list).

**Refused while work runs.** While a turn is running — yours, another
client's, or the agent's own — or a sub-agent is still running in the
background, `/connect` writes `Finish or stop the running work first, then
/connect.` and opens nothing; work that starts while the box is open refuses
the save in the field instead, keeping the key for another `Enter`. A running
session learns a newly stored key, to redact it, only when its next turn starts
(see [Keys stored while a session
runs](configuration.md#keys-stored-while-a-session-runs)), so a shell command
or a sub-agent already running would not know it. The TUI's view of the
session can lag its host by a moment, and another client attached to the same
session can start work while the box is open, so the refusal narrows that
window rather than closing it.

**The running session keeps its models.** A provider connected here is offered
by every new session, and by this conversation once you `/exit` and resume it
with `craze -c`; the running one keeps the model table it started with, and a
key you replace reaches it only after the same `/exit` and `craze -c`.

**Where it writes.** `/connect` writes to the `providers.toml` of *this* TUI's
craze directory (`CRAZE_HOME`, or `~/.craze`). A session attached with `craze
attach` from a shell with another `CRAZE_HOME` reads its host's directory, not
this one — which is why step two names the file.

It is no way in on a machine with no key at all: a native session with nothing
funded does not start, and its error names `craze auth login`, which is.

## Model dialog

`/model`, or a click on the model name in status row 1, opens a centred box
over the transcript: a filter (`❯ `, type to narrow by name or id), the model
list (`▲`/`▼` when it scrolls; which models, in what order, is below), and
one tab below it per select option the **current model's** catalog
advertises, other than `mode` and `model` (semantic category first, then id,
in the agent's own order). `effort`
(`low medium high xhigh`, the picked one bracketed) and `fast` (`on`/`off`)
are drawn exactly as they always were, whichever id or name the agent files
them under; any other option the model offers — `context` or `thinking` on
cursor's Claude models — gets a tab too, labelled with the agent's own name,
lowercased, showing its raw values. A tab is shown whenever the catalog
advertises the option; there is no capability-bit gate on a tab (the
provider's effort/fast bits gate only the status-row chips and the
`/model <effort>` shorthand, never a control the model itself advertises).
Composer-2.5 shows `fast` alone; claude-opus-5 shows `effort`, `fast`,
`context` and `thinking`; grok shows `effort` alone, never `fast`.

**Which models, in what order.** The current model is always the first row,
and the row the dialog opens on. A row is the model's name and nothing else —
no `current` or `recent` tag: the order says it. After the current model come,
on native, the models you picked most recently in any native session's
`/model`, newest first (the [model
memory](configuration.md#model-memory-recentjson)); then any model whose id
or name contains `grok`; then the rest, each group by name. An ACP provider
(cursor, grok, gx) has no model memory, so its list starts at the `grok`
group.

A native session lists only the models of providers that have a key — an
exported variable or a stored one ([Keys](configuration.md#keys)) — plus the
model it is running on, and says nothing about the providers that have none,
except one row: while some provider has no key, the list ends with
`Connect a provider…`, which `↑`/`↓` and a click reach like any row and which
opens [`/connect`](#connect) — applying nothing else, as `Esc` would not. It is
judged when the dialog opens, against the providers this TUI's craze directory
and environment know (the file `/connect` writes), so it goes once every
provider has a key, even though the running session's own list does not change
until `/exit` and `craze -c`. An ACP provider's dialog never has it.
It judges both when it starts: a model you pick in this session moves up in
the *next* session's list, not this one's, and a provider you connect while
the session runs appears in a new session, or in this conversation after
`/exit` and `craze -c`. A typed `/model <alias>` naming a model the list
leaves out fails as an unknown model does; `--model <alias>` at start still
resolves against every model craze knows, and refuses one whose provider has
no key, saying how to give it one.

The dialog does not rebuild itself for the model under the cursor: the tabs
on screen are the current model's, and highlighting another model in the list
never previews its options. A choice made on a tab still travels with a model
switch in the same `Enter` when the destination model offers that option and
value; after the switch, the new model's own tabs are edited by reopening
`/model`, which then shows exactly them.

The list and each tab are focus targets, and the one the keys are on carries
a `> ` gutter and the selection background — so exactly one row looks active,
in a screenshot and with the colour stripped alike. A model that is selected
but no longer focused keeps a dimmer `· ` mark: `[value]` says which value is
*picked*, the gutter says which row is *focused*. The footer says which keys
are live: `type to filter · ↑↓ · tab <label>/<label> · enter · esc` on the
list, naming the tabs in order, and `←→ change · tab cycles · type filters ·
enter · esc` on a tab. Effort-and-fast-only footers (`tab effort/fast`,
`tab effort`, `tab fast`) are drawn at every width exactly as before,
clamped the same way when the box is narrower than the text; a catalog with
any other tab has no footer of old to keep and falls back to `tab options`
once the tabs' names do not fit. Typing filters the list whatever has focus.

`Tab`/`Shift+Tab` move focus between the list and the tabs; `←`/`→` change
the focused tab's value; clicking a row focuses it. `Enter` closes the
dialog and applies model, then each **changed** tab, in catalog order, each
as its own step — a tab the user never moved is left alone: it always shows
its option's live value (tracking a delta that lands while the box is open),
and nothing is sent for it. A note is written for every step that lands, and
for one that could not be — the model changed under it, the destination
model has no such option, or does not offer that value — never an error row
for those; an actual failure is still named in an error row instead of
silently reverting the rest. `Esc`, or a click outside the box, closes it and
applies nothing. Status row 1 then reads `Name (effort · fast)`, with `fast`
shown only when it is on.

`/model <id>` and `/model <id> <effort>` still work without opening the
dialog. `/model <id> <effort>` resolves the model first — matched against the
model list without reading any catalog — and then checks the effort word
against the **destination** model's catalog, so `/model grok-4.6 high` typed
while on composer-2.5 (which has no effort) still lands on grok-4.6 instead
of being read as an unknown model. There is no full-width picker band and no
`Ctrl+M` binding; the only band left under the transcript is the slash menu.

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
`sub-agent rows, or a list inside a dialog`. On native, `panels and views`
also gains `del, backspace — stop the selected running sub-agent, or the one
in view`.

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
