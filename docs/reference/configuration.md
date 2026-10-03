# Configuration

craze persists the theme and the last successfully started **provider** in
`~/.craze/config.toml`, and, once a session has a prompt or a title, a row
about it in the session index (`~/.craze/sessions.jsonl`) that
`--continue`, `--resume` and `/rename` use — see [Session
index](#session-index). Every session also writes a [journal](#session-journal)
of what happened in it, which can contain secrets and can be turned off.
Session flags (`--workspace`, `--model`, `--effort`, `--fast` / `--no-fast`,
`--force`, `--ask` / `--plan`, `--agent-bin`) are per-invocation; the agent
binary each ACP provider runs can also be set once, in [`[agents]`](#agent-binaries).

## Theme precedence

An explicit `--theme` beats the config file, which beats `craze-dark`.

| Source | When it wins |
|--------|----------------|
| `--theme <name>` | Flag was passed (including `--theme ""`) |
| `~/.craze/config.toml` | No `--theme` on the command line, and the file names a theme |
| `craze-dark` | Neither of the above |

## Presets

Seven presets. While craze runs it sets your terminal's own default
background and text colours to the theme's and restores them when it exits;
`background = false` or `--no-background` leaves them alone — see [Terminal
colours](#terminal-colours).

| Name | Notes |
|------|-------|
| `craze-dark` | Default |
| `craze-light` | |
| `tokyo-night` | Aliases: `tokyonight`, `tokyo` |
| `dark` | |
| `light` | |
| `catppuccin` | |
| `gruvbox` | |

`craze` and `crazedark` are aliases for `craze-dark`; `crazelight` is an alias
for `craze-light`.

`Ctrl+G` or `/theme` opens a names-only picker that repaints the live screen as
the cursor moves; `Enter` keeps the theme and saves it, `Esc` puts back the one
that was active when the picker opened. `/theme <name>` sets and saves one
directly.

## The craze directory

craze keeps its own files in one directory, `~/.craze` by default, under
fixed names:

| File | Holds |
|------|-------|
| `config.toml` | The saved theme, provider, and other settings — see [Config file](#config-file) |
| `sessions.jsonl` | The [session index](#session-index) |
| `journal/` | One directory per workspace of [session journals](#session-journal) |
| `attachments/` | The images pasted into the composer — see [Attachments](#attachments) |

`CRAZE_HOME` moves the whole directory. With `CRAZE_HOME=/some/dir`, craze
reads and writes `/some/dir/config.toml`, `/some/dir/sessions.jsonl` and
`/some/dir/journal/` and nothing under `~/.craze`. A relative value stays
relative to the working directory craze runs in, and a leading `~` or `~/`
means your home directory. No variable moves a single file on its own.

`CRAZE_HOME` moves only craze's own directory. The user-level skills and the
plugin caches craze scans (see [Slash commands](tui.md#slash-commands)) are
still found under `HOME`, so a test or a container that wants those isolated
as well sets `HOME` too.

Earlier builds read `CRAZE_CONFIG`, which named the config *file*. It has
been removed, not kept as an alias: while it is still set, `craze`,
`craze prompt`, and `craze frame` exit with status 2 and a message that names
it, before reading or writing anything. Unset it and set `CRAZE_HOME` to the
directory the file was in — `CRAZE_HOME=/some/dir` for `/some/dir/config.toml`.
A config file with any other name has to be renamed `config.toml`.

## Attachments

An image pasted into the TUI's composer (see [Images](tui.md#images)) is
processed and stored in `attachments/` inside the [craze
directory](#the-craze-directory): `~/.craze/attachments/`, or
`$CRAZE_HOME/attachments/`. The directory is created `0700` and holds only
files named `<16 hex digits>.<png|jpg|webp>`, mode `0600`, written atomically.
The name is the start of the content's SHA-256, so pasting the same image twice
stores one file. craze refuses to read from a directory that is a symlink, is
owned by another user, or has any mode other than exactly `0700`, and an attachment path in a
message that points anywhere else is sent to the agent as text, not read.

Every TUI start sweeps the directory, quietly: files older than seven days are
deleted, and then the oldest files go until at most 500 MiB is left.
Deleting the directory by hand is always safe; an image that has gone by the
time the message is sent goes as text saying `no longer available`. What the
agent receives is described under [what the agent
receives](tui.md#what-the-agent-receives).

## Config file

The saved theme lives in `config.toml` in the [craze
directory](#the-craze-directory), `~/.craze/config.toml` by default.

```toml
theme = "gruvbox"
provider = "grok"
```

Unrelated keys in that file are preserved. A file craze cannot parse is never
clobbered, and the save that refused to overwrite it is reported in the
transcript. A malformed file does not prevent startup; the theme falls back to
the default.

`craze frame` never reads this file, and never reads `$CRAZE_PROVIDER`. See
[Testing](../development/testing.md#craze-frame).

## Session index

`~/.craze/sessions.jsonl` is the on-disk index behind `--continue`,
`--resume` and `/rename` (see [CLI](cli.md#-continue-and-resume) and
[TUI](tui.md#resuming-a-session)). It is **always the sibling of the config
file** in the [craze directory](#the-craze-directory), so `CRAZE_HOME` carries
the index along with the config, and `craze frame`, which isolates `HOME` and
unsets `CRAZE_HOME` for its run, isolates it too. There is no separate
environment variable for it.

One JSON object per line:

```json
{"sessionId":"…","provider":"grok","cwd":"/abs/workspace","title":"fix flaky pty test","pinned":false,"createdAt":"2026-09-15T22:04:11.5Z","updatedAt":"2026-09-16T00:12:03.2Z"}
```

- `cwd` is matched by exact string against the workspace's absolute path —
  no symlink resolution — so a session started from one path is not offered
  under another that resolves to the same directory, cursor included even
  though its own `session/load` is permissive across directories.
- `title` is the session's stored name: the agent's own title, the first
  line of the first prompt as a fallback, or a `/rename`. `pinned` marks a
  `/rename`, which always wins over a later agent title.
- A row exists only once there is something to show it for: craze creates
  it on the session's first prompt or its first agent-supplied title,
  whichever comes first — never merely on start — so a session started and
  quit without a prompt leaves nothing behind.
- The index is capped at 500 rows; a write past the cap drops the oldest
  rows by `updatedAt`.
- An unknown provider id already in the file (written by a newer craze) is
  kept on disk but never offered by `--continue` or `--resume`.
- `craze prompt` never writes to the index — headless sessions are not
  resumable.

**A `--continue` or `--resume` never changes the persisted default
provider.** Loading yesterday's grok thread is not a decision about
tomorrow's default: `provider` in `config.toml` is only touched when
`--provider` was passed explicitly alongside `--continue`/`--resume`, the
same as an ordinary run.

## Session journal

Every session craze starts writes a journal of what happened in it, one JSON
object per line, under the [craze directory](#the-craze-directory):

```
~/.craze/journal/--home-me-src-app--/20260919T225929Z_01a0bbe5-69b4-79de-b75c-483a15b73d78.jsonl
```

The directory below `journal/` is the workspace's own path with its separators
turned into dashes, wrapped in `--…--` — the same name the native harness gives
that workspace's session store, so the two pair by eye. The file name is the
UTC time the session was built and the session's own id, which the first line
of the file repeats.

**One file per session, and no process ever appends to a file another process
wrote.** A `craze prompt` run leaves one; a TUI run leaves one per session it
starts. `--continue` and `--resume` start a *new* session, so they get a new
file, into which the agent's replay of the earlier conversation is written
again, flagged as replay; the earlier file is never touched. The file is
created with the first line worth writing, so a craze that is opened and quit
without ever starting a session leaves nothing behind. `craze frame` never
journals.

| Line | Holds |
|------|-------|
| `header` | Format and codec versions, the session id, the craze version, OS and architecture, the provider, the agent binary asked for, the workspace, `force`, `interactive`, `mode`, and the pid |
| `session` | The provider's own session id, the id it was loaded from on a resume, and the binary craze actually spawned |
| `event` | One line per event the session emitted, with its `seq` and the whole event — the same numbers `craze prompt --json` prints (see [CLI](cli.md#json-events)) |
| `prompt` / `prompt_end` | Your prompt or interjection **as typed**, before craze expanded any slash command into it; then how that turn ended — stop reason, or error class and message, and its duration |
| `diag` | Everything that is not transcript: the agent's own stderr, a start that failed, a start setting (`--effort`, `--fast`) that could not be applied, an event too large to record, a reader craze dropped, and the line that closes the file |
| `gap` | Written when the journal could not keep up: exactly what was lost, so a reader is never quietly short of events |

A journal never slows a turn down and never fails a session. If it falls
behind, it records what it lost as a `gap` rather than skipping it quietly; if
a write fails outright — a full disk — journaling stops for that session and
craze says so once on stderr. The session itself carries on either way.

### Journals can contain secrets

Prompts and tool output are written **verbatim, with no redaction**. Whatever
you type and whatever a tool prints — an API key in a shell command, the
contents of a file the agent read, a token in an error message — is on disk in
plain text. That is the point of the record, and it is why the files are
`0600` in `0700` directories, both *no broader than*: a restrictive umask
narrows them further, and a directory that already exists is used as it is,
never tightened. Treat a journal as being as sensitive as the session it came
from.

craze never uploads a journal, or any part of one, anywhere. They are local
files. A running session reads its own journal back — that is how a client
that reconnects is given the events it missed — but craze does not read a file
from an earlier run: once a session ends, its journal is only there for you.

craze does not prune them yet either: they accumulate until you delete them. As
a sense of scale, a measured turn with one tool call cost 6 KB against cursor
and about 50 KB against grok, which streams its reply token by token; a
fifty-second cursor turn that read dozens of files cost 224 KB.

### Turning it off

`journal = false` in `config.toml`, or `CRAZE_JOURNAL=0` (or `false`) for one
run, and craze writes no journal and creates no `journal` directory. Either
switch turns it off on its own, and neither can turn it on against the other:
`CRAZE_JOURNAL=1` does not defeat `journal = false`.

**The opt-out fails closed**, unlike every other switch on this page. A
journal holds prompts and tool output, so anything craze cannot read as a
plain `true` means *off*, not on: a `config.toml` it cannot read or parse, a
`journal` key that is not a bool (`journal = "false"` is a string), and a
`CRAZE_JOURNAL` it cannot read as a bool. Each of those prints one line saying
so; an explicit `false` is your own choice and is silent. Journaling is on only
when the config parsed and `journal` is absent or exactly `true`.

## The control socket

Every host binds a Unix-domain control socket and serves its session over it
— what [`craze bridge`](cli.md#craze-bridge) and [`craze
attach`](cli.md#craze-attach) dial into. The host is the detached [`craze
serve`](cli.md#craze-serve) the ordinary `craze` spawns (see [Detached
hosts](#detached-hosts)), or, with detaching off, the TUI's own process. See the
[protocol reference](protocol.md#reaching-a-host) for the runtime namespace,
the registry, and where the socket itself lives; the registry and its locks
sit under the bridge process's own `$HOME`, not `CRAZE_HOME` — an SSH login
is **assumed** to share that `$HOME` with the tab that started the host,
true for an ordinary SSH login as the same user, whatever the tab had set
for the other variables. Binding
happens only once a run is actually starting a host or a TUI: a `--continue`
[refused by SQ16](cli.md#a-session-already-running-in-another-craze) binds
nothing.

`control_socket = false` in `config.toml`, or `CRAZE_CONTROL_SOCKET=0` (or
`false`) for one run, turns it off; a craze with no socket serves nothing for
another process to reach, though the session's own SQ16 lock is still taken
either way — it does not depend on the socket.

**The opt-out fails closed**, [the journal's](#session-journal) own rule and
for the same reason: this is an access switch, so anything craze cannot read
as a plain `true` means *off*. `CRAZE_CONTROL_SOCKET`, when set, decides
**alone**: an unreadable-as-bool value turns the socket off with its own one
line on stderr, and `config.toml` is then not even read, so at most that one
line prints. Only when `CRAZE_CONTROL_SOCKET` is unset (or reads as true)
does `config.toml` get read at all, and there the same rule applies — a
`config.toml` it cannot read or parse, or a `control_socket` key that is not
a bool, prints its own one line. An explicit `false`, from either switch, is
your own choice and is silent. The socket is served only when the config
parsed and `control_socket` is absent or exactly `true`, *and*
`CRAZE_CONTROL_SOCKET` (when set) reads as true — either switch turning it
off is final; neither can turn it back on against the other.

A failure to *bind* at all — the runtime directory is unusable, or the
socket's own path would be too long for `sun_path` — is not the opt-out and
is not a privacy switch: it prints one line, `craze: control socket off:
<why>`, embedding the underlying error's own text as is — in the ordinary
case that is one stderr line, but nothing here promises the error text
itself is free of a line break (a runtime path containing one, say). On the
in-process path (`CRAZE_DETACH=0`, `detach = false`) the run carries on
exactly as it would with the opt-out set. A [detached](#detached-hosts)
session cannot: its host exists to serve that socket, so `craze serve` fails
to start and the TUI shows the failure (`craze: the session host could not
start: …; CRAZE_DETACH=0 runs sessions inside craze instead`) and exits 1 when
dismissed. `CRAZE_RUNTIME_DIR` (below) is the fix when the reason is length;
`CRAZE_DETACH=0` runs the session inside craze meanwhile.

## Detached hosts

By default the ordinary `craze` runs the session in a detached host — [`craze
serve`](cli.md#craze-serve), in a session of its own — and the TUI is that
host's client, so [the session outlives the
terminal](cli.md#sessions-outlive-their-terminal). Three switches turn that
off and run the session inside the TUI's own process, as craze did before:

- `detach = false` in `config.toml`.
- `CRAZE_DETACH=0` (or `false`) for one run. It cannot turn detaching on
  against `detach = false`.
- `control_socket = false`, or `CRAZE_CONTROL_SOCKET=0`: a detached host is
  reachable only through its socket, so with the socket off detaching is off
  too, silently (the [control socket](#the-control-socket)'s own switches
  speak for it).

**The opt-out fails closed onto the in-process path**, like the other access
switches on this page: anything craze cannot read as a plain `true` means
*off* — a `config.toml` it cannot read or parse, a `detach` key that is not a
bool, a `CRAZE_DETACH` that is not a bool. Each prints one line
(`craze: detached sessions off: <why>`); an explicit `false` is your own
choice and is silent. `CRAZE_DETACH` is read trimmed, and empty counts as
unset.

With detaching off, closing the terminal ends the session, and there is no
agent view of it beyond the run's own control socket.

### Host idle exit

`host_idle_exit` in `config.toml` says how long a detached host may sit idle
before it exits on its own, saving the session like any stop (it stays
resumable with `craze -c`):

```toml
host_idle_exit = "1h"      # the default
host_idle_exit = "20m"     # any Go duration: "90m", "2h30m", "45s"
host_idle_exit = "0"       # exit as soon as the host is eligible
host_idle_exit = "never"   # any case; never exit for being idle
```

The value is a string. An absent key is `"1h"`, silently; a file that cannot
be read or parsed, a value that is not a string, or one that is neither a
duration nor `never` (or is negative) is also `"1h"`, with one line in the
host's log. The host reads it once, as it starts.

**Idle** means both: no client attached, and nothing in flight. A client is
one attached to the session (a `craze attach`, a TUI, a bridge); the session
list's polling does not count, nor does a peer that has half-closed its
connection. Something in flight is a turn, a queued prompt or setting, a
send-now armed, an open ask (a permission or a question: **an ask pins its
host until someone answers it or the session is stopped**), a sub-agent or a
background child running, or a session still starting or replaying. Any of
those, at any one-second look, restarts the clock, as does a turn that
ended since the last look. The stop is decided atomically: the host refuses
new work while it counts, so a client that attaches at that instant is either
counted or refused, never half-attached.

Three cases have their own limit:

- **A startup grace.** The clock does not start until a client has attached
  or 60 seconds have passed since the host started, so a host spawned for a
  client that has not dialled yet is not reaped first.
- **Never prompted: five minutes at most.** A session that has had no prompt
  has nothing to resume, so its limit is the smaller of `host_idle_exit` and
  five minutes — a loaded session nobody has prompted included.
- **A failed start: none.** A host whose session could not start (an auth
  failure, say) stays only long enough for its launcher to attach and be told
  why; its limit is 0, even under `"never"`.

Independently of the limit, a host whose socket file or registry entry has
vanished (`/run/user` removed at the last logout, a registry swept by hand)
stops at once, the ordinary way: it can no longer be reached, and stopping
saves the session.

### Host logs

A detached host writes its own diagnostics, its crash output and its agent's
stderr to `~/.cache/craze/host-logs/<hostId>.log` (under the process's own
`$HOME`, beside the [registry](protocol.md#the-registry) — not `CRAZE_HOME`).
The launcher names it whenever a host could not start. A spawned host's
`--log` is always that file; `craze serve` run by hand logs to stderr unless
`--log` says otherwise.

- The log is **rotated once**: at 4 MiB the current file becomes
  `<hostId>.log.1` (replacing an older one), and a new `<hostId>.log` starts.
- Beside it, `<hostId>.pgids` records the process groups of the host's agents
  while it runs, for its launcher's last-resort cleanup; it is removed when
  the host stops cleanly.
- Every host **sweeps the directory as it starts**: the files of a host that
  is gone (decided by its own host lock, never by its registry entry) and that
  nobody has touched for 7 days are removed. A live host's files are never
  removed, whatever their age.
- The directory is `0700`, and a `--log` inside it must be named for the host
  that opens it.

A host's log can hold what its agent printed to stderr; treat it like the
[journal](#session-journal).

A start that fails because its agent exited — with a non-zero status or a
signal, after craze sent it `initialize` — also carries **the agent's last
non-blank stderr lines** in its error, from the stderr craze had captured when
the failed start collected it — first waiting up to half a second for its copy
of that stderr to finish — sanitized, folded onto one line with ` / `, at most
512 bytes (a longer last line keeps its end). They go wherever a
detached host's start failure goes: a client attached to it,
`session.create`'s `data.cause`, `craze new`'s line. An agent that exits
before craze sends `initialize`, or exits 0, gets craze's own error only. The
lines are **not redacted**: they are the end of what the host's log holds, and
an agent's stderr can hold a token, so only that tail leaves the log. This is
the detached host's alone: the in-process TUI (`detach = false`) and `craze
prompt` say craze's own error only, so the two TUIs word the same failed start
differently.

On macOS a host runs in the login session of whoever started it: a detached
TUI's host in its terminal's, a host the hub created in the hub's — and a hub
keeps the session of whichever command first started it, while any session
runs. The login keychain, which `cursor` needs, is unlocked only in the GUI
login session, so start craze, and its hub, from a terminal in your Mac's
login session (a Roost tab, Terminal.app). A host outside it whose agent
exited that way ends its start error with a hint saying so: for a host the hub created
it names the hub to stop (`kill <hub pid>` ends only the hub; no session
ends), and for one a TUI launched it says to start the session from the login
session's terminal.

### Model catalog cache

cursor, grok and gx name the models they offer only once a session has
started. So that the session list's [`/model`](tui.md#provider-and-model) can
offer them before one has, every detached host records the model catalog its
agent installs — at `session/new`, when a loaded session's replay ends, and
whenever the agent changes it — in `~/.cache/craze/catalogs/<provider>.json`
(under the process's own `$HOME`, beside the host logs — not `CRAZE_HOME`: the
catalog is your account's, whichever craze directory is in use). Native's
models are its model table and are never cached.

```json
{"version":1,"observedAt":"2026-09-30T12:00:00Z","provider":"cursor",
 "models":[{"id":"composer-2.5","name":"Composer 2.5"}, …]}
```

- Each provider's file keeps **the newest observation**: a host takes
  `<provider>.lock` (an `flock`), reads the file's `observedAt`, and replaces
  the file — atomically — only when its own observation is newer, so of two
  hosts writing at once the older can never win by finishing last. A host
  writes only when the catalog differs from the one it last recorded, and
  never a catalog with a model the file cannot hold (no id, a name or id that
  is not one line): the whole observation is left out — the file keeps what it
  had — and the host's log names the model, rather than recording the rest as
  though it were the whole catalog.
- A file larger than 256 KiB, or one craze did not write (another version,
  another provider's, no models, text that is not one line), is ignored as if
  there were none, and the next write replaces it.
- The directory is `0700` and validated like the rest of `~/.cache/craze`; a
  failed write is one line in the host's log and never stops the session.
- It is only a list of choices, labelled with its age: deleting the directory
  is safe, and the next session of each provider writes it again.

## Terminal tab title

`terminal_title = false` in `config.toml` turns off every terminal tab-title
write (see [Tab title](tui.md#tab-title)); the default — including when the
key is absent or the file cannot be parsed — is `true`. `craze prompt` never
sets a tab title, and `craze frame` has no terminal to write one to.

## Host status

Inside a herdr pane or a roost tab, craze reports its own state to that host,
which then tracks craze like any other agent. That is all craze sends: its
state, a one-line reason (a card's header or an error's first line), and the
provider and model, never conversation content. A `--continue` or `--resume`
replay is not reported — the host first hears from craze when the restored
session is ready. The end of a turn reaches the host a quarter of a second after
the turn ends, so a message that drains from the queue the moment a turn ends
never shows as a finished turn in between.

Every exit craze can see — `/exit`, `Ctrl+D`, `Ctrl+C`, `SIGTERM`, and `SIGHUP`,
the terminal hangup a closed tab or window sends — releases the pane or tab
before the agent is shut down, waiting at most a second for the host to take
it, so an agent slow to exit cannot hold it. `SIGKILL` gives craze no
chance to, and leaves its last state in place until the pane or tab closes.

After the TUI exits, craze prints its own lines on stderr: a socket it could
not reach costs one `host status: herdr: …` or `host status: roost: …` line.
It prints the agent's own diagnostics too, but only when the run failed —
the session never started, the TUI run itself failed (a recovered panic
included), or the agent exited on its own. On a clean exit craze prints
nothing but its own lines.

### herdr

The pane's sidebar, `herdr agent list` and `herdr agent wait` see an agent named
`craze`: `idle` once the session is up and after each turn, `working` while a
turn runs, and `blocked` while a permission, question or plan card is waiting
on you — or when the session fails to start, or a turn ends in an error (until
the next send). A blocked state carries the one-line reason. The provider and
model go along as the pane's `provider` and `model` metadata tokens, which
craze clears when it releases the pane.

craze reports only when herdr's own variables say it is in a herdr pane:
`HERDR_ENV=1`, with `HERDR_SOCKET_PATH` and `HERDR_PANE_ID` both set.

If craze runs in a herdr pane but herdr never shows it, run
`herdr pane get <pane>`: an `agent_session` from another source (such as
`herdr:cursor`) means an agent hook tied the pane to its own session, and
herdr ignores craze's reports while it stands. A fresh pane has none.

### roost

craze claims the tab under the source `craze` when its agent session starts,
and the tab shows it the way it shows any agent: no agent state while the
session sits ready, `running` while a turn runs, `needs input` with a
notification while a permission, question or plan card is waiting on you, and
`failed` with a notification when a turn ends in an error (until the next
send); both notifications carry the one-line reason. A turn that ends normally
leaves the tab idle with a "Turn complete" notification; a turn you cancel
leaves it idle with none. `roostctl wait --state` sees the same states, a failed
turn as `needs_input`. The claim carries the `model`, `craze.provider` and
`version` metadata keys. A session that never starts has no session id to claim
the tab with, so roost hears nothing about it.

craze reports only when roost's own variables say it is in a roost tab:
`ROOST_SOCKET` set, and `ROOST_TAB_ID` a positive integer with nothing around
it — the same rule roost's own hooks apply.

`roostctl tab set-state` on craze's tab wins. roost drops every report craze
sends for that session from then on, craze never claims the tab back for it,
and one `host status: roost: …` line on stderr after exit names the tab's
owner. craze's next session — a new one in the same run, or the next run —
claims the tab again. `set-state none` is different: it leaves the tab with no
owner at all, which craze cannot tell from a restarted roost, so craze claims
the tab back when it next reports after finding it unowned.

### The agent's environment

**The agent loses each host's hook gate.** While craze reports to a host, the
agent it spawns (cursor-agent, grok or gx) gets craze's environment without the
one variable that host's agent integrations gate on: `HERDR_ENV` for herdr,
`ROOST_AGENT_HOOK` for roost. Otherwise a herdr or roost cursor or grok hook
installed on this machine would fire inside craze's child and take the pane or
tab from craze: herdr's would tie the pane to the agent's own session, and
herdr would silently ignore every report craze sends for the rest of the
session; roost's would claim the tab as `cursor` or `grok`. Everything else
stays — `HERDR_SOCKET_PATH`, `HERDR_PANE_ID` and `HERDR_BIN_PATH`, and
`ROOST_TAB_ID` and `ROOST_SOCKET` — so the `herdr` CLI and `roostctl` still
work from inside the agent. The cost is that herdr's agent skill, run inside
the child, believes it is not inside herdr, and roost's own agent hooks do
nothing inside the child.

`host_status = false` in `config.toml`, or `--no-host-status` on the command
line, turns reporting to both hosts off and leaves the agent's environment
whole, so the agent's own herdr and roost hooks work again. The default —
including when the key is absent or the file cannot be parsed — is `true`;
outside a herdr pane or roost tab there is nothing to report to either way.
`craze prompt` and `craze frame` never report.

## Terminal colours

The theme is more than craze's own cells: while craze runs it sets the
terminal's default background and text colours to the preset's (`OSC 11` and
`OSC 10`) and hands them back on exit (`OSC 111` and `OSC 110`), so the
picker's live preview moves the background too. The terminal's *configured*
defaults come back, not whatever colours happened to be set before craze
started.

`background = false` in `config.toml`, or `--no-background` on the command
line, turns that off: craze then draws on your terminal's own background and
writes none of those four sequences. The default — including when the key is
absent or the file cannot be parsed — is `true`, and only a literal `false`
disables it; there is no flag to force it back on. It is also off
automatically whenever craze is painting no colour at all (`NO_COLOR`,
`TERM=dumb`). A terminal that ignores `OSC 11` simply keeps its own
background; a hard kill (`SIGKILL`) leaves the theme's colours set until the
terminal is reset, the same as the tab title.

## Provider precedence

Ids are `cursor`, `grok`, `gx`, and `native`. An empty string (`--provider ""`,
`$CRAZE_PROVIDER=""`, `provider = ""`) is unset, not unknown.

`gx` is [`charliek/grok-build`](https://github.com/charliek/grok-build), a
third-party fork of the Grok CLI that speaks the same ACP dialect as
`grok` — same wire protocol, same auth, same skills and plugins — so
everything on this page and in the rest of the docs about Grok's behaviour
applies to it too. It currently shares grok's `~/.grok` home (config, auth,
sessions, skills); that is the fork's current behaviour, not a guarantee
craze makes.

gx is gated separately from the other ids: it appears in the startup picker
when a binary for it resolves, or when it is the resolved default (the default
is always included, so `Esc` never starts a provider the picker did not show)
— see [Environment](#environment) below for lookup order; cursor, grok and
native are always offered, so the picker can never be empty. `--provider gx`
works regardless of whether a binary resolves, and fails at spawn — `agent
binary not found: …` — if it does
not; this matches how `craze prompt --provider gx` and `craze frame
--provider gx` already behave, since neither consults the picker. The picker
asks the question a gx session would ([Agent binaries](#agent-binaries)):
`[agents].gx`, else `gx` on `PATH` — and `--agent-bin` or `$CRAZE_AGENT_BIN`
only when gx is itself the resolved default, since they are that provider's
binary. Then the override is exclusive, so one pointing at nothing hides gx
even when `gx` is itself on `PATH`. `native` runs in process (see [Native
compaction](#native-compaction) below and [Native sessions](tui.md#native-sessions))
— there is no binary to resolve, so it is never gated the way gx is.

For the TUI and `craze prompt`, the first hit wins:

1. `--provider <id>` when the flag was passed (`Changed`).
2. `$CRAZE_PROVIDER` if non-empty.
3. `provider` in `~/.craze/config.toml` if it is a known id.
4. `cursor`.

Unknown `--provider` exits 2. Unknown env or config id warns and falls back to
cursor; the fallback is not written back. After a successful `Start`, the TUI
and `craze prompt` save the provider that actually started (an explicit picker
choice overwrites a stale unknown id; `Esc` on the fallback default does not).

`--agent-bin` / `$CRAZE_AGENT_BIN` override the **binary** of the launch's own
provider ([Agent binaries](#agent-binaries)). They do not select the dialect.

## Agent binaries

Each ACP provider's agent binary can be set in `config.toml`, as an absolute
path:

```toml
[agents]
cursor = "/opt/cursor/bin/cursor-agent"
grok = "/home/me/.local/bin/grok"
gx = "/home/me/src/grok-build/bin/gx"
```

A session of an ACP provider runs the first of:

1. `--agent-bin`, then
2. `$CRAZE_AGENT_BIN` — both **only when the session's provider is the
   launch's own**: the provider the command line resolved (`--provider`,
   `$CRAZE_PROVIDER`, `provider` in this file, `cursor`), or, for a
   `--continue` or `--resume` load, the loaded session's;
3. `[agents].<provider>`;
4. the provider's own name on `PATH`: `cursor-agent` then `agent` for
   cursor, `grok` for grok, `gx` for gx.

So a session of any other provider — one the startup picker or the session
list starts, or a saved one of another provider the list resumes — never runs
the binary a launch named for its own: the launcher passes `--agent-bin` only
to a session host of the launch's own provider, and leaves `CRAZE_AGENT_BIN`
out of every other host's environment. [The hub](cli.md#the-hub) removes it
from its own environment, so a session it starts uses `[agents]` and
`PATH`.

A value that is not an absolute path (`bin/grok`, `~/bin/grok`) or not a
string is ignored, with one line on stderr naming it. A path that is set and
does not exist is not a fallback: that provider's sessions fail to start —
`agent binary not found: …` — as an `--agent-bin` naming nothing does.
`native` runs inside craze and has no key. `craze frame` never reads this
file.

## Native models and providers

The native provider (`--provider native`) knows a set of models before you
write anything: craze ships a **model catalog** inside the binary, and an
upgrade of craze is how that catalog changes. Your own two files in
`~/.craze/native/` (or `$CRAZE_HOME/native/`) are laid over it — they hold
keys, changes to shipped entries, and models you add, never a copy of the
catalog — so a release that adds, renames or retires a model reaches every
machine with no edit.

### The shipped catalog

The catalog's providers, and the environment variables each takes its key
from, tried in order:

| Provider id | Name | Key variables |
|-------------|------|---------------|
| `chatgpt` | ChatGPT plan | none: [signs in with ChatGPT](#the-chatgpt-plan) |
| `fireworks` | Fireworks | `FIREWORKS_API_KEY` |
| `meta` | Meta | `META_API_KEY` |
| `openrouter` | OpenRouter | `OPENROUTER_API_KEY` |
| `zai-coding-plan` | Z.AI Coding Plan | `ZHIPU_API_KEY`, `ZAI_API_KEY` |

Its models — aliases, wire model ids, context windows, efforts and costs — and
its default model are in
[`internal/harness/modeltable/catalog.toml`](https://github.com/charliek/craze/blob/main/internal/harness/modeltable/catalog.toml);
a session's `/model` lists those whose provider has a key ([which models, in
what order](tui.md#model-dialog)). With the catalog, a machine needs only a key:
store one with [`craze auth login`](#keys), or export one of the variables
above, and run `craze --provider native`. With no key at all, a native session
refuses to start and names `craze auth login`, the variables to set and the
file an inline key goes in.

### Your two files

Both files are optional, each on its own, and each starts with `version = 1`.

- `providers.toml` holds keys and provider settings. It is the only place a
  secret lives: keep it `0600` (craze tightens a looser one when it reads it,
  and [`craze auth`](#keys) writes it so) and never paste it anywhere.
- `models.toml` holds model settings and models you add, and no secrets.

An entry for something the catalog ships changes only the keys it writes:

```toml
# providers.toml
version = 1

[providers.fireworks]
api_key = "fw_..."            # an inline key (craze auth login writes this); an exported variable still wins
```

```toml
# models.toml
version = 1
default_model = "muse-spark-1.3-contributor"   # optional: where a new session starts when it remembers nothing

[models."fireworks/kimi-k3"]
default_effort = "max"        # everything else stays as shipped
```

An entry for anything else is yours, and must be complete: a provider needs
`driver` (`openai-compat`, with a `base_url`, or `openrouter`, without one),
and a model needs `provider` and `wire_model`:

```toml
# providers.toml
[providers.local]
name     = "Local"                          # optional display name
driver   = "openai-compat"
base_url = "http://127.0.0.1:8080/v1"
env_keys = ["LOCAL_API_KEY"]                # optional, tried in order
```

```toml
# models.toml
[models."local/qwen"]
provider   = "local"
wire_model = "qwen3-coder"
name       = "Qwen3 Coder (local)"          # optional, as is every key below
context_window = 131072
efforts        = ["low", "high"]
default_effort = "high"
```

A model's other optional keys are documented below:
[`max_output_tokens`](#native-output-ceiling), [`cost`](#native-cost),
[`vision`](#native-vision) and `tool_profile`; `models.toml` also takes
[`[compaction]`](#native-compaction) and
[`[subagents]`](tui.md#sub-agent-models). craze never rewrites `models.toml`,
and rewrites `providers.toml` only when you run [`craze auth`](#keys) or
store a key with [`/connect`](tui.md#connect).

### How the files merge

- **Field by field.** A key your entry writes replaces the shipped value; a key
  it leaves out keeps it. A list (`efforts`, `env_keys`) replaces the shipped
  list whole, and an explicit empty one means empty: `efforts = []` is a model
  with no effort control, `env_keys = []` a provider with no variables. A
  key written with an empty value (`name = ""`, `default_effort = ""`) is that
  value, not "unset".
- **Cost, rate by rate.** Each `[cost]` rate you write replaces the shipped
  rate on its own; one you leave out keeps it. An inherited rate cannot be
  cleared.
- **Efforts and the default effort.** An `efforts` list without a
  `default_effort` keeps the shipped default if the new list has it, and
  otherwise the model has none.
- **Driver and base URL.** A `driver` that differs from the shipped one drops
  the shipped `base_url` (so `driver = "openrouter"` alone is a valid
  override). An entry that changes the endpoint — `driver` or `base_url` —
  without writing `env_keys` does **not** inherit the shipped variables: the
  shipped `FIREWORKS_API_KEY` belongs to Fireworks' own endpoint and never
  pays for yours. craze still treats it as a key, though: exported, it is
  kept out of the commands a native session runs and redacted from what they
  print, as every key is ([Keys](#keys)), so it does not reach your endpoint
  that way either.
- **`default_model`** is yours when it names a model, the catalog's
  otherwise.

**A release never breaks a load.** What a file gets wrong on its own still
stops a native session, naming the file, the table and the key: TOML syntax,
a key craze does not know, a `version` other than 1, an inline key that
cannot be one (below), or a value no entry could hold (an unknown driver, a
negative window, an effort listed twice, a `default_effort` outside the
`efforts` the same entry lists, a cost out of range). What an entry gets wrong
only against the catalog is dropped with a warning at session start, naming
the file, the table and the key, and the session carries on: a model whose
provider no longer exists, a `default_effort` the model no longer offers, a
partial entry for a model craze no longer ships, a `default_model` or
`[subagents]` target that is not a model. Dropping your entry for a shipped
model restores the shipped entry, so the catalog's default model always
survives.

An entry for a shipped model or provider that repeats every value it writes
is legal, but it pins those values: a later catalog's change to them will not
reach you. Delete it to follow craze's updates.

### Keys

For each provider, craze takes the first of its `env_keys` variables that is
set to a usable key, and otherwise its inline `api_key`. A value that cannot
be a key — shorter than 8 bytes, or overlapping craze's redaction marker — is
skipped as though unset, with one line at session start naming the variable
(never the value); an inline `api_key` like that is a load error instead.
Every key craze knows, from every provider, is redacted from tool output, and
the commands a native session runs are started without the variables that hold
one. That covers every variable the shipped catalog or your `providers.toml`
names as a key variable, even one no provider takes its key from any more —
a shipped provider's after you point it at your own endpoint or give it
`env_keys` of your own: it no longer funds anything, but its value is still a
credential. A provider with no usable key leaves its models unusable, not the
others.

[`craze auth login <provider>`](cli.md#craze-auth-login) stores a key as that
provider's inline `api_key`, [`craze auth logout
<provider>`](cli.md#craze-auth-logout) removes it, and [`craze auth
list`](cli.md#craze-auth-list) shows how each provider is connected: by a
variable, which always wins, by its stored key, or not at all. In a native
session, [`/connect`](tui.md#connect) stores a key the same way `login` does.
No key is checked with its provider when it is stored; a wrong one shows on
first use. When they write, `login`, `logout` and `/connect`:

- **Rewrite `providers.toml` whole**, at `0600` (a new directory is made
  `0700`). Comments and layout are not kept — the header they write says so —
  but every key of every entry is, `source` and an explicit `env_keys = []`
  included.
- **Change one provider's `api_key` and nothing else.** A key the rules above
  refuse is not stored. Another provider's stored key that cannot be used is
  kept exactly as it is and named in a note, so two broken keys can be fixed
  one at a time (native sessions start again once both are).
- **Remove only the key.** An entry left with nothing else in it goes; one
  that sets a name, an endpoint or variables stays.
- **Refuse a `providers.toml` that is a symlink** — edit its target by hand —
  and never write `models.toml`.
- **Take turns**: each holds a lock beside the file, `providers.toml.lock`,
  while it reads and writes.

They know the catalog's providers and your own in `providers.toml`; in a
directory whose `models.toml` says [`catalog = false`](#isolated-setups-catalog-false),
only your own.

### Keys stored while a session runs

A running native session keeps the model table it started with. A provider
you give a key to while it runs — an `api_key` written into `providers.toml`
by `craze auth login`, `/connect`, by hand, or by another craze — is offered
by the next session, or by this conversation after `/exit` and `craze -c`,
never by the running one.

The running session does watch that file, only so it can redact what it
holds: at the start of every turn (a prompt, a `/compact`, or the delivery of
a background sub-agent's result) it checks the `providers.toml` of the craze
directory it started with, and when the file has changed it learns each
inline key it did not know and redacts it from then on, as it redacts every
key it knows — tool output and spill files included. A value that cannot be a
key (shorter than 8 bytes, or overlapping the redaction marker) is ignored
with one line naming the provider, never the value; a file that no longer
parses is one line too, and nothing in it is learned until it is fixed.

If a newly stored key turns out to be inside what the session sends with every
request — its system prompt (which names the working directory), its tool
definitions, or its plan file's path — none of that can be taken back, so the
session refuses every later turn with `a newly stored API key appears in this
session's frozen prompt; start a new session`.

The redaction starts at the next turn, so there is a window: a key stored
during a turn is learned when the next one starts. From then on a shell
command still running — a background command's included — redacts it from the
rest of its output and from its saved output file, but not from what it
printed before. A sub-agent already running keeps the redaction it started
with, though what it reports back is redacted of the key; one started after
the key was learned redacts it.

### The ChatGPT plan

The catalog's `chatgpt` provider, **ChatGPT plan**, runs OpenAI's models on
your ChatGPT subscription through OpenAI's Sign in with ChatGPT: requests are
billed to the plan's usage, not to an API account, and count against the
plan's limits (on Plus, a five-hour limit shared with your other apps that use
the plan). Manage that usage in [ChatGPT's settings](https://chatgpt.com/settings/usage).

**It takes no key.** It names no variable and stores no `api_key`: it is
funded by signing in with
[`craze auth login chatgpt`](cli.md#signing-in-to-the-chatgpt-plan) or, in a
native session, with [`/connect`](tui.md#connect) — the same sign-in, inside
the TUI, into the TUI's own craze directory — and
[`craze auth list`](cli.md#craze-auth-list) shows the sign-in. A
`providers.toml` entry for it may set only `name`; a `driver`, `base_url`,
`env_keys` or `api_key` written there is dropped with a warning naming the
key. The `chatgpt` driver belongs to this provider alone: an entry of yours
that names it under another id is dropped with a warning (a load error in a
[`catalog = false`](#isolated-setups-catalog-false) directory, which has no
ChatGPT plan).

**Its models are the account's own**, not the catalog's: the list ChatGPT
offers the signed-in account, fetched when you sign in and again, in the
background, by a native session that starts with a list over a day old. Each
is `chatgpt/<slug>` — `chatgpt/gpt-5.6-sol`, say — for `--model` and
`/model`, named as ChatGPT names it with ` (ChatGPT plan)` after, and listed
in `/model` together, in the account's order. Its context window is the one
the list gives; its efforts are those the list offers among `none`,
`minimal`, `low`, `medium`, `high`, `xhigh` and `max`, and it takes images
when the list says it does. It has no cost (the status row shows tokens
alone) and no `/fast`. Signed out, or with plan usage off, the plan's models
are not offered; a session already on one keeps it in its picker, as with any
provider that loses its key.

The shipped catalog's `[chatgpt_defaults]` table holds craze's own settings for
the plan's models, laid over the account's list: `start`, the model a new
session starts on when nothing it remembers is funded and the default model is
not either (`gpt-5.6-sol`), and per-slug `name`, `efforts` (which can only
narrow the account's), `default_effort` and `tool_profile` (`gpt-6-astra`
defaults to `low`). It ships in the binary like the rest of the catalog; it is
not a `models.toml` setting.

**Its files** are in the native directory (`~/.craze/native/`, or
`$CRAZE_HOME/native/`), each `0600`, the sign-in's under `auth/`, a directory
craze makes `0700`:

| File | Holds | Secret | Signing out |
|------|-------|--------|-------------|
| `auth/host-id` | This machine's id for ChatGPT (`urn:uuid:…`), made before the first sign-in | no | kept |
| `auth/chatgpt-client.json` | craze's registration with ChatGPT: the client id it was issued, the account's id and email, whether the account allowed plan usage, whether the one-time notice was shown | no | kept |
| `auth/chatgpt.json` | The tokens: access, refresh and id token, and when they expire | **yes** | deleted |
| `auth/chatgpt.json.lock` | The lock every change of the two files above is made under | no | kept |
| `chatgpt-models.json` | The account's model list, with the account it belongs to | no | kept |

Never copy or paste `auth/chatgpt.json`; craze writes it durably (synced to
disk) and never logs it. Signing out deletes it and keeps the rest, so the
next sign-in reuses craze's registration and the browser asks only which
account to use. A craze directory holds **one ChatGPT account**: a sign-in
with a different account is refused, since the registration is that
account's; use another `CRAZE_HOME` for a second account. The model list is
bound to the account too: a list that another account fetched — after a
change of account, say — is ignored until the next sign-in fetches the
account's own, and one craze cannot read is a warning at session start, with
no plan models until it is fetched again. So are the models it lists: a
session already running on one, or a sub-agent, sends nothing on another
account's sign-in — its next request, a summary's or a wake's too, ends with
`native: model "chatgpt/…" is from another ChatGPT account's model list; a
new session offers the signed-in account's models` — and goes on once the
first account is signed in again.

**Renewal.** An access token lasts about an hour. craze renews it itself when a
request finds less than five minutes left, so the sign-in lasts until you sign
out. Every craze process — each native session's host, `craze auth`, the TUI —
shares the token file: one renews it under the lock, and the others take its
new tokens rather than renewing again. A renewal ChatGPT refuses for good (the
sign-in revoked, say) deletes the tokens, and the next turn ends with
`native: ChatGPT sign-in is no longer valid; run /connect or craze auth login
chatgpt`.

**Requests** go to OpenAI's Responses API at `https://api.openai.com/v1` with
the access token, and nowhere else: no redirect is followed, and the token is
never put in any process's environment. Each carries the session's id, so
ChatGPT can reuse the cached start of a conversation. The
[output ceiling](#native-output-ceiling) is not sent: ChatGPT plan requests
refuse one, though the ceiling still sizes compaction.

**The usage limit.** When ChatGPT says the plan's usage limit is reached, the
turn ends with `native: ChatGPT plan usage limit reached for craze. Manage
usage: https://chatgpt.com/settings/usage`, and that craze process sends no
other request on the plan — a background sub-agent's, a wake's or a retry —
until you start the next turn. An account the plan cannot be used with in
craze, or a request it refuses, ends the turn with its own message.

**Redaction.** The token values are treated as keys are ([Keys](#keys)):
every native session, whatever its provider, learns the ones in
`auth/chatgpt.json` at the start of each turn and redacts them from tool
output, and a session that uses the plan learns renewed ones the moment it
renews them — its shell commands still running and its sub-agents included,
a sub-agent that is just starting too. The file tools refuse the whole `auth/`
directory, a hard link to any file in it included, and so does a search:
`grep` refuses to search there, and neither `grep` nor `glob` shows a file
there — or `providers.toml` — among what it finds in a directory above it.
A long line `grep` or `read` shortens is redacted, with every value the
session knows by then, before it is cut, so a value renewed while a search
runs leaves no piece of itself at the cut.

### Model memory: `recent.json`

A model or effort you pick in a native session's `/model` is remembered for
the next session, in `recent.json` beside the two files
(`~/.craze/native/recent.json`, or `$CRAZE_HOME/native/`):

```json
{
  "version": 1,
  "recent": [
    {
      "model": "muse-spark-1.3-contributor",
      "provider": "meta",
      "wire_model": "muse-spark-1.3-contributor",
      "effort": "xhigh",
      "at": "2026-09-30T10:12:00Z"
    }
  ]
}
```

The list is newest first, one entry per model, and keeps at most 10. Each
entry names the model by alias and by its provider and wire model, with the
effort last used on it (`""` for a model with no effort control) and when it
was picked (`at`, in UTC, for people reading the file). The file is `0600`.
Deleting it forgets everything.

**Who writes it.** Only a change made inside a running session — a model
switch (`/model`, the model dialog, or an attached client's switch) or an
effort change on the current model (which makes that model the newest, even if
`/model` was never used) — once it has taken, recording the model and the
effort the session is then on. `--model`, `craze
prompt`, a resume and a sub-agent's model never write it. A session reads it
once, when it starts: a switch changes where the next session starts, and the
order the next session's `/model` lists models in — the remembered ones right
after the current one, newest first — not the running one's.

**Two sessions at once.** Every write reads, changes and replaces the file
under a lock beside it, `recent.json.lock`, so two sessions switching at the
same moment both keep their choice. A switch waits at most 2 seconds for
another craze's lock. When the choice cannot be saved — the lock stayed busy,
the file cannot be read or written, or a newer craze wrote it — the session
says so in one note (`not saving the model choice: …`) and the switch itself
stands.

**Reading is bounded.** The read never waits on a FIFO and stops after 64 KiB,
though a stalled filesystem can still delay an ordinary read. A missing file, one craze cannot read, one
that is not a regular file (a FIFO, say) or is larger than 64 KiB, and one
that is not a `recent.json` (not JSON, or a version craze does not know) are
no memory at all. The next switch replaces a file that is not a
`recent.json`, and leaves alone one it cannot read, one that is not a regular file or is over
64 KiB, or one a newer craze wrote.

**Where a new session starts.** With no `--model`, a new native session starts
on the first of:

1. the newest remembered model whose provider has a key, at its remembered
   effort when the model still offers it (its `default_effort` otherwise);
2. `default_model`;
3. when `default_model`'s provider has no key, the first model, by alias, whose
   provider has one — with a note saying so.

A remembered model is found by its alias while that alias still names the
same provider and wire model, and otherwise by its provider and wire model,
under whatever alias has them now: a release that renames a model keeps your
choice, and an alias pointed at a different model does not inherit it. An
entry no model matches, or whose provider has no key, is passed over and left
in the file.

- `--model <alias>` (the TUI's and `craze prompt`'s) starts on that model at the
  effort remembered for it, when the model still offers it, and writes
  nothing.
- `craze prompt` without `--model` starts where a new TUI session would.
- A resume (`craze -c`, `--resume`) keeps its transcript's model and effort,
  and an explicit `--model` on a resume picks the model with the transcript's
  effort carried over: the memory never applies to a resume. A resumed session
  that turns out to have no transcript at all opens as a new one, and starts as
  above.

### Isolated setups: `catalog = false`

A `models.toml` with `catalog = false` at the top turns the catalog off for
its directory: the two files are then the whole table, as before the catalog
existed — both are needed, `default_model` is required, and every entry is
complete. It is meant for sandboxes such as the evaluation harness's homes,
where no model beyond the ones written may appear.

### Files from an older craze

craze once imported its entries from gx, marking each `source = "gx"`. Such
files still load and are never rewritten, but their entries yield to the
catalog: a model entry marked `source = "gx"` is ignored when craze ships (or
has retired) its alias, and a provider entry marked `source = "gx"` for a
provider craze ships contributes only its `api_key` and any `env_keys`
variable the catalog lacks. Any other entry — `source = "manual"`, or none —
is your own, as above. A hand-copied entry that repeats the shipped one is a
no-op.

### Retired models

When a release retires a model (or renames one, retiring the old alias), the
catalog records the alias and every *dead* wire model id it pointed at; an
alias whose wire id still answers is retired by alias alone, with no wire ids
listed, so an entry of yours pointing at that id is not dropped. A retired
alias disappears from an untouched directory with no warning: a `source =
"gx"` entry for it is ignored, a `default_model` naming it falls back to the
catalog's, and a `[subagents]` model or tier naming it falls back to its
default (the parent's model). Any model entry of yours, under any alias,
whose provider and wire model are a retired pair is dropped silently too — a
dead wire id is dead whoever wrote it. An entry of yours for a retired alias
that points at a live wire id is your own model and stays.

### Sub-agent settings

`[subagents]` (see [Sub-agent models](tui.md#sub-agent-models)) never stops a
session for what the catalog changed: a `model` or tier target that is not a
model is dropped — silently for a retired alias, with a warning otherwise —
and falls through to the defaults, and an `effort` the kept `model` does not
offer is dropped with a warning.

## Native output ceiling

On the native provider (`--provider native`), every request to a model names
an output-token ceiling: the most tokens one response may generate, reasoning
included. A model's optional `max_output_tokens` in `~/.craze/native/models.toml`
(or `$CRAZE_HOME/native/`) sets it:

```toml
[models."fireworks/deepseek-v4p1-flash"]
max_output_tokens = 65536   # optional, >= 0; absent or 0 = the default below
```

A model with no `max_output_tokens` gets **32,000** tokens, the same default
opencode uses — or a quarter of the model's `context_window`, rounded down and
never under one token, when that is smaller (a window under 128,000 tokens), so
a small model keeps room for its prompt. The default exists so a response that starts repeating itself stops
there instead of running to the provider's own cap. An explicit value always
wins, even above 32,000: set one for a model whose answers or reasoning need
more room, or lower for a provider that refuses 32,000. A negative value is
refused at load.

On the wire the ceiling is `max_tokens`, except for a wire model id that
contains `gpt-5` (among a few OpenAI reasoning-model names), which gets
`max_completion_tokens` instead. The same ceiling sizes
[compaction](tui.md#compaction): the automatic trigger never passes the window
less the ceiling. On a model with no `max_output_tokens` and a window under
about 213,000 tokens, the default therefore moves that trigger below 85% of the
window (to 75% under 128,000 tokens), where before it sat at 85%; a session
resumed after upgrading that is already past the new trigger compacts before
its next turn. A smaller `max_output_tokens` moves the trigger back up.
The value is your own entry in `models.toml`; craze
never rewrites it.

The [ChatGPT plan](#the-chatgpt-plan)'s models are the exception: no ceiling
is sent to them, since ChatGPT plan requests refuse one, but the ceiling
(32,000, or the quarter of the window) still sizes their compaction.

## Native compaction

On the native provider (`--provider native`), `~/.craze/native/models.toml`
(or `$CRAZE_HOME/native/` when the home is relocated) may carry an optional
`[compaction]` section — see [Compaction](tui.md#compaction) for what it
controls:

```toml
[compaction]
auto              = true    # optional; compact on its own at all
threshold_percent = 85      # optional, 1-99: % of the model's context window
tail_tokens       = 20000   # optional, >= 0: the most, in tokens, of a
                             # verbatim tail a compaction keeps (whole steps
                             # only), also capped at a quarter of the threshold
```

Every key is optional and applies to every model — a per-model override is a
follow-up. The section is your own entry in `models.toml`: craze reads it and
never rewrites the file. If a tool of yours does re-encode the file, a section
left with none of these keys is dropped rather than written out empty.

## Native cost

On the native provider, `~/.craze/native/models.toml` (or `$CRAZE_HOME/native/`
when the home is relocated) may carry an optional per-model `cost` table —
what the [status row](tui.md#usage-and-cost) and the [usage
section](protocol.md#usage) price a session's spend from:

```toml
[models."fireworks/kimi-k3".cost]
input       = 0.60   # $ per 1,000,000 uncached input tokens
output      = 2.50   # $ per 1,000,000 output tokens (reasoning included)
cache_read  = 0.15
cache_write = 0.0
```

Every key is optional and independent; a model with no `[cost]` table saves
exactly as it did before this section existed. Shipped models may carry a
cost of their own; a rate you write for one replaces that rate alone (see
[How the files merge](#how-the-files-merge)). Each rate is dollars per
1,000,000 tokens, from 0 up to $10,000 — a rate outside that range, or one
that is not a finite number (`nan`, `inf`), is refused at load. There are no
tiers yet: a tier is per request, and a sub-agent's usage row already sums
many requests, so a per-request price cannot be recovered from it.

craze prices a usage record by **identity** — the model's `(provider, wire
model)` pair — never by the alias it happened to run under, since a resumed
session's alias can stop existing while the identity it named still does.
On every price lookup craze rebuilds the canonical identity → rates map from
the table's models: for every alias that shares an identity, the **first in
sorted order that has a `cost`** prices that identity, and each rate is
rounded to an exact number of picodollars per token, half up, on that same
lookup — not built once and cached at load. Two priced aliases of the same
identity that set a *different* `cost` still load, with a warning naming
both and saying which one's price is used. An identity with no priced alias
is simply unpriced — its usage is still counted in tokens, but adds no cost.

The `cost` table is your own entry in `models.toml`, the same as
[`[compaction]`](#native-compaction) above: craze reads it and never rewrites
it.

## Native vision

A native model's `vision` key says whether craze may send it images: the
pictures pasted into the composer (see [Images](tui.md#images)) and the ones
the `Read` tool returns for an image file. The shipped catalog sets it
`true` on the models seen to describe a screenshot, among them the default
model, and leaves it off on `glm-5.3`. A model you add defaults to `false`.

```toml
[models."local/qwen"]
provider   = "local"
wire_model = "qwen3-vl"
vision     = true            # this one takes images

[models."glm-5.3"]
vision     = true            # turn it on for a shipped model, or false to turn it off
```

On a shipped model the key is an overlay like any other (see [How the files
merge](#how-the-files-merge)): the one you write replaces the catalog's for
that model alone. For a model without `vision`, the composer warns at paste
time and the model receives `[Image omitted: <model> does not accept images.
File: <path>]` where the image was; the
[Images](tui.md#what-the-agent-receives) section of the TUI reference has the
rest.

## Environment

| Variable | Purpose |
|----------|---------|
| `CRAZE_HOME` | The [craze directory](#the-craze-directory) (default `~/.craze`): `config.toml` and the [session index](#session-index) live directly inside it. Skills and plugin caches still follow `HOME` |
| `CRAZE_AGENT_BIN` | The launch's own provider's agent binary when `--agent-bin` is unset; never a session of another provider's ([Agent binaries](#agent-binaries)) |
| `CRAZE_PROVIDER` | Provider id when `--provider` is unset (`cursor`, `grok`, `gx`, or `native`) |
| `CRAZE_JOURNAL` | Turns the [session journal](#session-journal) off for this run when it reads as false. It cannot turn one on against `journal = false`, and a value craze cannot read as a bool turns it off with one line saying so. Empty or unset leaves the decision to the config file |
| `CRAZE_CONTROL_SOCKET` | Turns [the control socket](#the-control-socket) off for this run when it reads as false. It cannot turn one on against `control_socket = false`, and a value craze cannot read as a bool turns it off with one line saying so. Empty or unset leaves the decision to the config file |
| `CRAZE_DETACH` | Turns [detached hosts](#detached-hosts) off for this run when it reads as false: the session runs inside the TUI's own process and ends with the terminal. It cannot turn detaching on against `detach = false`, and a value craze cannot read as a bool turns it off with one line saying so. Empty or unset leaves the decision to the config file |
| `CRAZE_RUNTIME_DIR` | Overrides [the control socket's](#the-control-socket) runtime base (default: `$XDG_RUNTIME_DIR/craze`, then `/run/user/<uid>/craze` on Linux, then `/tmp/craze-<uid>`): an absolute path, short enough to leave room for `<ns>/<hostId>.sock` under `sun_path`'s limit. Meant for tests and unusual hosts; see [Protocol reference](protocol.md#reaching-a-host) |
| `FIREWORKS_API_KEY`, `META_API_KEY`, `OPENROUTER_API_KEY`, `ZHIPU_API_KEY` / `ZAI_API_KEY` | API keys for the native provider's [shipped providers](#the-shipped-catalog), tried before any inline `api_key`; a value under 8 bytes is skipped with a warning |
| `XAI_API_KEY` | Grok API key; used when initialize advertises `xai.api_key` |
| `GROK_CODE_XAI_API_KEY` | Legacy alias for `XAI_API_KEY` |
| `HERDR_ENV`, `HERDR_SOCKET_PATH`, `HERDR_PANE_ID` | Read, never set. `HERDR_ENV=1` with the other two set means craze is in a herdr pane and reports [host status](#host-status) to it; `HERDR_ENV` is then removed from the agent's environment |
| `ROOST_SOCKET`, `ROOST_TAB_ID` | Read, never set. `ROOST_SOCKET` set with a positive integer `ROOST_TAB_ID` means craze is in a roost tab and reports [host status](#host-status) to it |
| `ROOST_AGENT_HOOK` | Never read, never set by craze; plays no part in detecting roost. While craze reports [host status](#host-status) to roost, it is removed from the agent child's environment, so roost's own agent hooks stay inert inside craze's agent |

If none of `--agent-bin`, `CRAZE_AGENT_BIN` (each for the launch's own
provider only) and `[agents]` names one, Cursor looks for
`cursor-agent`, then `agent`, on `PATH`. Grok looks for `grok` only. gx looks
for `gx` only.
