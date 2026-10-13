# Upgrading

Three sections: [since v0.1.1](#since-v011), the changes after the third
release; [since v0.1.0](#since-v010), the second release's; and [from
v0.0.1](#from-v001), the first release's. Whatever the version, [upgrading
while sessions run](#upgrading-while-sessions-run) says what an upgrade does
to the hub and the sessions already running.

## Upgrading while sessions run

Sessions run on through an upgrade: each keeps the craze it started with, and
so does the per-machine [hub](#the-hub) until it exits by itself. What a hub
from before the upgrade can still do depends on how the upgrade replaces
craze:

- **macOS (Homebrew) and apt: nothing to do.** The new craze takes the old
  one's place at the same path, so the old hub starts the new craze's
  sessions.
- **Linuxbrew, or any install that removes the old binary:** a hub from before
  this release cannot start sessions after the upgrade — the craze it runs
  them with is gone. `craze new` says the session host cannot be started and
  names that hub's pid: `the session host cannot be started; the hub (pid
  <pid>) runs craze <old> and this is craze <new>: if craze was upgraded, stop
  that hub with kill <pid> (no session ends) and run this again`. `kill` it:
  every session runs on, and the next command starts the new craze's hub.
- **From this release on, the hub says so itself** — `this hub's craze
  (<path>) was replaced by an upgrade: stop this hub with kill <pid> (no
  session ends) and run this again` — and goes on serving its clients and the
  sessions it lists until you stop it. A TUI left running from before the
  upgrade cannot start a session either, and says `craze was upgraded or
  removed since this craze started (<path> is gone): quit and start craze
  again`.

A running hub too old for what a command needs (`craze new --title` against a
hub from before titles) now ends its refusal with the same way out: `this hub
(craze <v>) cannot …; it exits once no session runs and no client is
connected, or stop it now with kill <pid> (no session ends)`, where it used to
end `it exits when idle`.

## Since v0.1.1

Nothing needs migrating.

- **Plain `craze prompt` puts a blank line between text runs.** Text that
  follows a tool call, a thought or the end of a turn (a `--follow-up`'s
  reply, for one) used to run on from the text before it, as in
  `…first reply…second reply`; it now starts a paragraph. A script that
  split plain output on the old run-on must expect the break. `--json`
  events, `--wait` messages and the exclusion of sub-agent text are unchanged.
  See [craze prompt](../reference/cli.md#craze-prompt).
- **A `craze:wonder` Claude Code preset** runs the native `muse-wonder-1.0`
  at its default effort, for hard tasks. See [Claude
  Code](claude-code.md#the-agents).

- **A native `/model` switch no longer carries the effort over.** `/model X`
  — typed, picked in the model dialog, or an attached client's switch — lands
  on the effort you last used on X, else X's `default_effort`, where it used
  to keep the effort you were on whenever X offered it: Muse Wonder at `max`,
  then `/model anthropic/claude-opus-5-5`, now runs Opus at its own `medium`
  rather than `max`. To keep a level across a switch, name it:
  `/model anthropic/claude-opus-5-5 max`. Switching to the model you are on
  keeps its effort, and ACP providers are unchanged. See [Effort on a
  switch](../reference/tui.md#effort-on-a-switch).
- **A native session's `/model` list is ranked again at every switch**, the
  model switched to moving up at once rather than in the next session.
- **For a client reading the session stream:** a native session's catalog
  models carry `efforts`, `defaultEffort` and `switchEffort`, and its info
  document says [`capabilities.effortFacts`](../reference/protocol.md#capabilities);
  a switch or an effort change that changes them publishes the `catalog`
  section in the same delta as its `model` and `config` sections. Nothing
  else moved: a client that ignores the new members folds the same state.
- **`muse-spark-1.3` offers `max`.** Its contributor tier stays at `minimal`
  to `xhigh`.
- **Native Meta models now use Meta's Responses API.** The shipped `meta`
  provider moved from Chat Completions to the [`openai-responses`
  driver](../reference/configuration.md#the-responses-api-driver). On Chat
  Completions Meta withholds a model's reasoning from an API key, so every
  step of a tool loop reasoned from scratch; on the Responses API craze replays
  each step's encrypted reasoning on the next request, and sends the session's
  id as `prompt_cache_key`, so Meta caches the session's prompt. In our
  comparison on `muse-spark-1.3-contributor` (9 tasks, 3 runs each) it passed
  as many tasks as Chat Completions at about 0.6 times the median spend per
  run. A session recorded on Chat Completions resumes on it. To go back, write
  `[providers.meta]` with `driver = "openai-compat"`, `base_url =
  "https://api.meta.ai/v1"` and `env_keys = ["META_API_KEY"]`. `env_keys` may
  be left out if the entry has an inline `api_key`. If the entry says `source =
  "gx"` (an old gx import wrote it), remove that line or set `source =
  "manual"`, since craze ignores such an entry's driver. An entry you wrote
  with `driver = "openai-compat"` and no `env_keys` or `api_key` no longer
  takes `META_API_KEY`: it still loads, with a warning to add `env_keys`. One
  with a `base_url` and no `driver` now sends Responses requests to that URL,
  so add `driver = "openai-compat"` to keep a Chat Completions endpoint. See
  [Going back to Chat
  Completions](../reference/configuration.md#going-back-to-chat-completions).
- **A `models.toml` model with more than 8 efforts, or one over 32 bytes or
  not one printable line, no longer loads**; it names the entry and the key.
- **`/effort` in a session.** A bare `/effort` opens a picker of the current
  model's levels; `/effort <level>` sets one. It is listed after `/exit`, so
  `/e` still selects `/exit`, and only where the current model has an effort
  control: anywhere else an agent's or a plugin's own `/effort` is what runs,
  as before. Like `/model`, it is refused while a turn runs. See
  [Effort](../reference/tui.md#effort).
- **The native model dialog's effort row follows the highlighted model**: its
  levels, on the effort a switch to it would land on, and no row for a model
  with no effort control. A switch from the dialog now sends the effort the
  row showed for that model, so it lands where the box said even if another
  craze process remembered another effort since. ACP providers' dialogs are
  unchanged. See [Model dialog](../reference/tui.md#model-dialog).
- **The session list's `/effort` offers real levels**: on native the chosen
  model's (`default` alone for one with no effort control), on an ACP
  provider `minimal` and `max` too; a value outside them is refused, and an
  effort the newly chosen model does not offer is shown `(not offered)` on the
  rule. See [Effort and fast mode](../reference/tui.md#effort-and-fast-mode).
- **`/model <id> <effort>` matches the effort word as `--effort` does**: the
  level's id exactly, then in any case, then its name — so a level the agent
  calls `High` under another id is taken as `high` — where it compared the id
  in any case alone. A word that names two levels is now refused as
  `--effort` refuses it.
- **For a client reading the session stream:** the slash catalog gains a
  builtin `effort` row, `requires: "effort"` and `run.picker: "effort"`; a
  client that does not know `requires: "effort"` hides it, as it hides any
  row with a value it does not know. See [Slash
  catalog](../reference/protocol.md#slash-catalog).
- **Spend on long Haiku 5.5 prompts now prices Anthropic's higher tier.** A
  request whose prompt — input, cache reads and cache writes — is over 100,000
  tokens is priced at Haiku 5.5's long-context rates, five times the base
  ones, where it read at the base rates and so read low. Each request is
  priced on its own, a sub-agent's and a compaction's included. A
  `models.toml` cost can set a tier of its own with `long_context_above` and a
  `[models."<alias>".cost.long_context]` table, or turn Haiku's off with
  `long_context_above = 0`. Transcripts gain an optional `long_context` beside
  a sub-agent's usage row and a compaction's usage, which an older craze
  ignores. In a session written before this, sub-agent rows and compactions
  price as they did, while each of its own long Haiku 5.5 steps now reads at
  the tier, so its spend can rise on a resume. See [Long-context
  pricing](../reference/configuration.md#long-context-pricing).
- **A typed `/model`, `/plan`, `/ask` or `/agent` mid-turn now says why it
  did nothing.** While a turn runs, a slash builtin that needs the agent used
  to do nothing at all; it now adds one error row, `Finish or stop the running
  work first, then /model.` (the same line `/connect` shows), and the draft
  stays in the composer. Nothing runs differently: the builtins that run
  mid-turn (`/exit`, `/help`, `/theme`, `/tasks`, `/clear`, `/rename`, `/btw`)
  still do. See [Queued messages](../reference/tui.md#queued-messages).
- **The session list shows each running session's model at 100 columns and
  wider.** A 20-cell column after the provider, cut with `…`, blank on saved
  rows. At narrower terminals nothing changes. At 100 columns the title, what
  it wants and the directory each give up some width to make room. See
  [Session list](../reference/tui.md#session-list).
- **Reasons, fixes and notes wrap instead of being cut short.** The provider
  dialog's reason and fix each wrap over up to two rows, so a fix naming
  `config.toml` shows its whole path and the box can be a row or two taller
  than before (it still keeps one height as the cursor moves). The session
  list's hint line wraps a note — a failed start's names the host's log —
  over up to three rows, taken from the list while it shows, and
  `/provider`'s refusal wraps over up to three rows under the candidates, its
  fix included. A path wider than a row is broken after a `/`, never in the
  middle of a name (also `/connect`'s `Stored in` line). Anything still too
  long is cut short with `…`. See [Provider
  states](../reference/tui.md#provider-states) and [Session
  list](../reference/tui.md#session-list).
- **A stopped session leaves the roster at once.** Its host marks itself
  stopping before anything of the stop can be seen — before a stop's receipt,
  or before a signal's, an idle exit's or its own TUI's quit goes ahead — and
  the hub's roster, the session list and `craze ps` list it no more: it is
  gone at the roster's next round, and nothing the hub sees of it after the
  mark reaches a roster subscriber — it used to be listed, closing or
  unreachable, for about a second after the stop's receipt. Notifications
  composed from what the hub saw before the mark may still arrive after the
  receipt, as late as a slow subscriber reads them, and may still carry the
  session; no time bound is promised for them, and lines on different
  connections were never ordered against each other. Once the hub's next
  read sees the mark, the session is removed and not listed again. (The mark
  is best effort: a host that cannot write it, or a hub that cannot read it
  back, leaves the row until the host's entry is gone, as before.) So the
  session list no longer shows `Closing…` on a stopped row: the row
  going is the feedback, and the session is a saved row from then on. This
  holds for sessions whose host started after the upgrade; a host from an
  earlier release writes no mark, and its stop looks as it did. For a reader
  of the registry, an entry gains a `stopping` member. See [The hub's
  roster](../reference/protocol.md#the-hubs-roster) and [Session
  list](../reference/tui.md#session-list).
- **A saved session can be brought back through the hub.** `craze ps --saved`
  lists the sessions saved in the session index that no host runs, and `craze
  new --load <id>` reopens one in a host of its own (`reopened <id> in
  <dir>`), or answers `already running` when a host serves it already. A
  client of the hub (shed, a phone) does the same with
  [`sessions.saved`](../reference/protocol.md#sessionssaved) and
  [`session.reopen`](../reference/protocol.md#sessionreopen) where the hub's
  `hello` says `reopen`. A hub still running from before the upgrade cannot:
  `craze new --load` says `this hub (craze <v>) cannot reopen a saved session;
  it exits once no session runs and no client is connected, or stop it now
  with kill <pid> (no session ends)`, exit 1 (no session ends with it), and
  `craze ps --saved` reads the session index itself. See [`craze new
  --load`](../reference/cli.md#craze-new-load).
- **The session list shows a saved session by its newest row only.** A
  session whose newest index row is of a provider this craze does not know (a
  newer craze's) used to be offered by an older row, which its resume would
  then not load; it is no longer offered at all. See [Saved
  sessions](../reference/tui.md#saved-sessions).

## Since v0.1.0

Nothing needs migrating. Every change is an addition, but several change what
an existing command or configuration does — read each item below, which says
how to keep the old behaviour where you can. In particular, the Anthropic
provider takes up an `ANTHROPIC_API_KEY` you already export, and a
`[providers.anthropic]` of your own now overlays craze's Anthropic provider.

- **`craze -c` and `--continue` skip sub-agent sessions.** A session started by
  `craze new --wait` (the Claude Code plugin's) is marked `unattended` in the
  session index, and `--continue` loads the newest session that is not one. In a
  workspace holding only such sessions it refuses, saying how many there are:
  `craze: no session to continue in /abs/workspace (3 sub-agent sessions here:
  craze -r lists them)`. `--resume` lists them after your own sessions, tagged
  `sub-agent`. See [`--continue` and
  `--resume`](../reference/cli.md#-continue-and-resume).
- **OpenRouter models are `~family-latest` routers.** Where OpenRouter has a
  router for a family, craze's alias sends it (`openrouter/gpt-sol-latest`), so
  the alias may resolve to a newer model after OpenRouter moves the router. To
  stay on a numbered model, add it to `models.toml` with its wire id. See [The
  shipped catalog](../reference/configuration.md#the-shipped-catalog).
- **A skill saved mid-session appears in the `/` menu at the session's next
  turn end, not when you type `/`.** The session's host now builds the menu
  (the [slash catalog](../reference/protocol.md#slash-catalog)) and walks the
  disk for skills itself — at the start, after every turn, at each command
  list the agent sends (whether or not it differs from the last), and when a
  client attaches (`craze attach` from another terminal) — so a bare `/` no
  longer rescans. Some agents, Grok among them, send their command list again
  a few seconds after a skill file appears, so there a skill can arrive without
  any of those. A TUI attached to a session served by an older craze keeps
  rescanning on a bare `/`, as before. See [Slash
  commands](../reference/tui.md#slash-commands).
- **For a client reading the session stream:** a delta carrying `commands`
  or `plugins` now carries the `slash` section beside them (with
  [`capabilities.slashCatalog`](../reference/protocol.md#capabilities)), a
  change of the disk skills alone publishes `slash` alone, and an
  `available_commands_update` identical to the last one publishes nothing — it
  used to republish `commands` and `plugins` every time. Nothing else moved:
  `commands` and `plugins` are what they were, the codec versions are
  unchanged, and a client that ignores `slash` folds the same state.
- **A native session starts on the first connected provider, in a fixed
  order**: Z.AI Coding Plan, ChatGPT plan, Fireworks, OpenRouter, Meta,
  Anthropic, each on its own start model, unless you remembered a model with
  `/model` or pinned one with `default_model`. `provider_order` in
  `models.toml` changes the order.
- **Anthropic is a native provider.** Claude models run on Anthropic's API with
  an API key ([Anthropic](../reference/configuration.md#anthropic)). An
  `ANTHROPIC_API_KEY` exported in your shell now funds it — and, as every key
  variable craze knows, is kept out of the commands a native session runs and
  redacted from what they print. A native session started with it fetches the
  account's model list from Anthropic as it opens (`GET /v1/models`, which
  costs nothing, at most once a day). To keep an exported key from funding
  craze, write `env_keys = []` under `[providers.anthropic]` in
  `providers.toml`.
- **Sub-agent tier words pick a Claude model on Anthropic.** Under a parent
  running on Anthropic, an `agent` call or a persona that says `model: haiku`
  (or `fable`, `opus`, `sonnet`) with no `[subagents.tiers]` entry for it runs
  on the newest Claude model of that family, where it ran on the parent's own
  model before; map the word in `[subagents.tiers]` to choose otherwise
  ([Sub-agent models](../reference/tui.md#sub-agent-models)). Other parents are
  unchanged.
- **A `[providers.anthropic]` of your own now overlays craze's Anthropic
  provider**: its `driver` and `base_url` are ignored, each with a warning, and
  its key goes to Anthropic's API. Rename yours (`[providers.my-gateway]`, and
  its models' `provider`) to keep a gateway.
- **New commands:** `craze new --wait`, `craze send`, `craze wait`, `craze
  cancel` and `craze stop` drive sessions from a script or another program
  ([CLI reference](../reference/cli.md#craze-new)); the [Claude
  Code](claude-code.md) plugin uses them.
- **`/btw <question>`** asks a side question in a view of its own, without
  adding to the session, on native, Grok and gx
  ([TUI reference](../reference/tui.md#side-questions-btw)).
- **A native turn the model refuses ends with a note**: `craze: the model
  declined this request (refusal category: cyber). Switch models with /model,
  or rephrase.`, after whatever the model wrote — `(refusal)` when the provider
  names no category. It is shown, and is the end of `craze prompt --json`'s
  `message`, but it is craze's: never saved in the session or sent to a model.
  The stop reason is still `refusal`.
- **`craze new --title <title>`** names a session from its start, pinned as a
  `/rename` is ([CLI reference](../reference/cli.md#craze-new)); a client of
  the hub sends `title` with
  [`session.create`](../reference/protocol.md#sessioncreate) where the hub's
  `hello` says `createTitle`. A hub still running from before the upgrade
  cannot: `craze new --title` says `this hub (craze <v>) cannot set a title at
  create`, exit 1, until that hub has exited idle (no session ends with it).
  `/rename` and a create's title are normalized alike, to exactly what the
  session stores: the session index's row now drops what the session's own
  title always dropped (a zero-width space, a bidi control, a carriage
  return), so the two agree, and a space the 120-character cut would leave
  at the end is trimmed.

## From v0.0.1

v0.0.1 was the first release. Nothing in `~/.craze` needs migrating: it wrote
only `config.toml` and `sessions.jsonl` (each with a `.lock` beside it), and
this version reads both unchanged. Your theme, your default provider and your
saved sessions carry over, and `craze --continue` and `craze --resume` load
the sessions v0.0.1 saved. A downgrade works too: v0.0.1 keeps what it does
not know, and falls back to `cursor` with a warning from a
`provider = "native"`.

What changed is how craze behaves around those files. This section lists each
change a v0.0.1 user meets, how to turn it off where it can be turned off, and
the setting that controls it.

| Change | Opt out |
|--------|---------|
| [`CRAZE_CONFIG` is gone](#craze_config-is-now-craze_home) | none; use `CRAZE_HOME` |
| [Sessions outlive the terminal](#sessions-outlive-the-terminal) | `detach = false` or `CRAZE_DETACH=0` |
| [A journal of every session](#the-journal) | `journal = false` or `CRAZE_JOURNAL=0` |
| [A control socket](#the-control-socket) | `control_socket = false` or `CRAZE_CONTROL_SOCKET=0` |
| [A per-machine hub](#the-hub) | `craze ps --no-hub`; it exits by itself |
| [Status reports to herdr and roost](#status-reports-to-herdr-and-roost) | `host_status = false` or `--no-host-status` |
| [`native` in the picker](#native-in-the-provider-picker) | `--provider` skips the picker |
| [`←` opens the session list](#opens-the-session-list) | `detach = false` |
| [`--agent-bin` is per launch](#-agent-bin-and-craze_agent_bin) | `[agents]` in `config.toml` |
| [`seq` in `prompt --json`](#seq-in-prompt-json) | none; it is additive |
| [New directories](#new-directories) | see the page |

## `CRAZE_CONFIG` is now `CRAZE_HOME`

v0.0.1 read `CRAZE_CONFIG`, the path of the config *file*. It is removed, not
kept as an alias: with it set, every command exits with status 2 before
reading or writing anything (`craze bridge` exits 1), except `--help` and
`--version`:

```text
craze: CRAZE_CONFIG is no longer supported; unset it and set CRAZE_HOME to the directory that holds config.toml instead (it defaults to ~/.craze)
```

To move over, unset `CRAZE_CONFIG` (check your shell's startup files and any
scripts) and set `CRAZE_HOME` to the **directory** the file was in:
`CRAZE_CONFIG=/some/dir/config.toml` becomes `CRAZE_HOME=/some/dir`. craze now
looks for the fixed names `config.toml` and `sessions.jsonl` there, so a config
file with any other name has to be renamed `config.toml`. `CRAZE_HOME` moves
the whole [craze directory](../reference/configuration.md#the-craze-directory),
and no variable moves a single file on its own.

## Sessions outlive the terminal

The ordinary `craze` no longer runs the session inside the terminal's process.
It starts a detached host (`craze serve`, in a session of its own with no
terminal) and the TUI is that host's client. Closing the terminal, a dropped
ssh connection or a SIGHUP closes the *view*: the host and its agent keep
running, and a turn already under way keeps working. `craze -c` or `craze
attach` picks it up again, and `craze ps` lists what is running.

- **Stop a session:** `/exit`, `Ctrl+D` or a second `Ctrl+C` in the TUI or in
  `craze attach` ends it everywhere; in the session list (`←`), `Ctrl+X` stops a
  working session's turn and, pressed twice, closes an idle one. A stopped session stays
  resumable with `craze -c`.
- **Idle exit:** a host with no client attached and nothing in flight exits by
  itself after an hour. `host_idle_exit` in `config.toml` sets that (`"20m"`,
  `"2h30m"`, `"never"`); a session that was never prompted is capped at five
  minutes, and a host whose start failed goes as soon as its launcher has been
  told.
- **Turn it off:** `detach = false` in `config.toml`, or `CRAZE_DETACH=0` for
  one run, runs the session inside the TUI's own process as v0.0.1 did:
  closing the terminal ends it, and nothing outlives it.

An unattended host's agent is a running process, so a long-lived session holds
whatever its agent holds. See [Detached
hosts](../reference/configuration.md#detached-hosts) and [Sessions outlive their
terminal](../reference/cli.md#sessions-outlive-their-terminal). A host's own log
is `~/.cache/craze/host-logs/<hostId>.log`.

## The journal

Every session now writes a journal, one file per session, under
`~/.craze/journal/<workspace>/` (or `$CRAZE_HOME/journal/`). A `craze prompt`
run leaves one file too. It records the events, and your prompts and every
tool's output as the session admitted them. The journal itself adds no
redaction. Native already replaces the provider keys it knows with
`[craze:redacted-credential]` — in a `!` command's output before the prompt is
recorded, and in its tools' results — but any other secret, such as a token you
typed by hand or one craze does not know, can be in it in plain text. The files are `0600`
in `0700` directories, never uploaded, and **never pruned**: they accumulate
until you delete them (deleting the directory is always safe).

- **Turn it off:** `journal = false` in `config.toml`, or `CRAZE_JOURNAL=0` for
  one run. The opt-out fails closed: anything craze cannot read as a plain
  `true` means off.

See [Session journal](../reference/configuration.md#session-journal).

## The control socket

Every host binds a Unix-domain control socket, which is how `craze attach`,
`craze bridge` and the session list reach it. It lives in a `0700` runtime
directory, `$XDG_RUNTIME_DIR/craze/<ns>/` (`/tmp/craze-<uid>` where there is
none), is mode `0600`, and is reachable only by you.

- **Turn it off:** `control_socket = false` in `config.toml`, or
  `CRAZE_CONTROL_SOCKET=0` for one run. A detached host is reachable only
  through its socket, so with the socket off detaching is off too. The opt-out
  fails closed.

See [The control socket](../reference/configuration.md#the-control-socket).

## The hub

`craze hub` is one process per user and craze directory. It serves the list of
running sessions and starts sessions in the background (`craze new`). You never
start it: `craze ps`, `craze new` and the TUI's session list start one when
none runs, and the next one finds it. **It exits by itself 60 seconds after the
last host has gone and the last client has disconnected.** Killing it loses
nothing: hosts run on without it. `craze ps --no-hub` reads each session's host
directly and starts none. Its log is `~/.cache/craze/host-logs/hub-<ns>.log`.
See [The hub](../reference/cli.md#the-hub).

## Status reports to herdr and roost

Inside a herdr pane or a roost tab, craze reports its state (idle, working,
blocked), a one-line reason, and the provider and model to that host, never
conversation content. Outside both it reports nothing.

- **Turn it off:** `host_status = false` in `config.toml`, or `--no-host-status`
  on the command line. That also leaves the agent's environment whole.

See [Host status](../reference/configuration.md#host-status).

## `native` in the provider picker

Without `--provider`, the startup picker now lists `native`, craze's own agent,
beside `cursor`, `grok` and `gx` (gx only when its binary resolves). A provider
that cannot start is dimmed and says why: `native` reads `needs setup` until you
give it an API key or sign in to a ChatGPT plan (see [Native
provider](quick-start.md#native-provider)). The default is still the one you
had: `$CRAZE_PROVIDER`, then `provider` in `config.toml`, then cursor.
`--provider cursor` (or grok, gx) skips the picker.

## `←` opens the session list

On an empty composer `←` opens the [session list](../reference/tui.md#session-list),
and `/sessions` does the same. It applies only when sessions run in detached
hosts (the default): with `detach = false` or `CRAZE_DETACH=0` the key reaches
the composer as it did in v0.0.1.

## `--agent-bin` and `CRAZE_AGENT_BIN`

`--agent-bin` and `CRAZE_AGENT_BIN` now apply only to the launch's own
provider: the one the command line resolved (`--provider`, `$CRAZE_PROVIDER`,
`provider` in `config.toml`, then cursor), or the loaded session's for a
`--continue` or `--resume`. A session of another provider, one the picker or the
session list starts, or one the hub creates, never runs the binary a launch
named for its own. To set a provider's binary for good, use `[agents]` in
`config.toml` with an absolute path:

```toml
[agents]
cursor = "/opt/cursor/bin/cursor-agent"
```

See [Agent binaries](../reference/configuration.md#agent-binaries).

## `seq` in `prompt --json`

Every event `craze prompt --json` prints now carries a `seq`, the number the
journal and the control socket use for it. It is an added field: a reader that
ignores unknown fields is unaffected. See [JSON events](../reference/cli.md#json-events).

## New directories

craze now also creates, all `0700` with `0600` files, and only when it needs
them:

| Where | Holds |
|-------|-------|
| `~/.craze/journal/` | [the journals](#the-journal) |
| `~/.craze/native/` | the native provider's keys (`providers.toml`), `models.toml`, `recent.json`, and the ChatGPT plan's tokens (`auth/chatgpt.json`); native session transcripts (`sessions/`); created on first use |
| `~/.craze/attachments/` | images pasted into the composer, swept after seven days |
| `~/.cache/craze/` | the host registry, the hub's record, locks, host logs and the model catalog cache; under your own `$HOME`, not `CRAZE_HOME` |
| `$XDG_RUNTIME_DIR/craze/<ns>/` | [the sockets](#the-control-socket) |

`~/.craze` itself moves with `CRAZE_HOME`; the cache and runtime directories do
not. A `~/.cache/craze` that already exists with a looser mode than `0700` is
refused, and craze never changes it: `chmod 700` it or remove it, or run with
`CRAZE_DETACH=0`. See [The craze directory](../reference/configuration.md#the-craze-directory).
