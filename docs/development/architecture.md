# Architecture

craze is an ACP client. It owns the chrome; `cursor-agent`, `grok`, or `gx`
(or `craze-fake-agent` in tests) is the agent process. The **native** provider
is the exception: its agent loop, tools and model clients run inside craze
(`internal/harness`), behind the same `agent.Session`, so the layers above it
cannot tell it from an ACP agent. `gx` is a third-party
fork of the Grok CLI that speaks the same ACP dialect as `grok` — see
[Configuration](../reference/configuration.md#provider-precedence) — so on
the wire a gx session *is* a grok session and the diagram below has no
separate node for it.

```mermaid
flowchart LR
  tui["TUI / prompt"] --> session["agent.Session"]
  session --> client["acp.Client"]
  client --> cursor["cursor-agent acp"]
  client --> grok["grok agent stdio"]
  cursor -->|"session/update, permission, ask, plan"| client
  grok -->|"session/update, x.ai ask/plan, prompt_complete"| client
  client --> session
  session --> tui
  session --> native["harness (native)"]
  native -->|"HTTPS"| api["model provider API / ChatGPT plan"]
```

The diagram shows the session layer. Around it, a session runs in a
[detached host](#host-and-client) and the TUI is a client of it; see below.

## Packages

| Package | Role |
|---------|------|
| `internal/acp` | JSON-RPC over stdio: spawn, framing, `session/new`, prompt, and the blocking agent→client requests (permission, question, plan) |
| `internal/agent` | Session wrapper: events, tool merge, todos, model/mode catalog |
| `internal/backend` | The TUI's seam onto a session (plan 027 §3.12): the `Backend` interface, `Item`, `SessionInfo`, the epoch sentinels (`ErrStaleEpoch`, `ErrOutcomeUnknown`) both the in-process and socket implementations share |
| `internal/tui` | Bubbletea screen: transcript, cards, composer, themes, slash, mouse; a command gate carries almost every engine call onto a `Backend` (in process, or over the socket for `craze attach`) — a hidden ask's answer and a sub-agent stop are the exceptions, fire-and-forget `tea.Cmd`s that are never gated |
| `internal/protocol` | The control-socket wire, protocol 1 (plan 027 §3.2–§3.4): envelope, methods, notifications, codes, reasons, limits and the JSON Schema — the one place `internal/control`, `internal/remote` and `internal/fakehost` take their shapes from |
| `internal/control` | The control socket's server (plan 027 §3.7): connections, `hello`, the binding table, command handlers, attach and forwarding, run by any process that serves a session: a detached `craze serve` by default (plan 030), the TUI's own process when detaching is off |
| `internal/remote` | The control socket's Go client (plan 027 §3.14): `remote.Session` implements `backend.Backend` over the wire — dial, resume, the reply barrier, reconnect and resend rules — for `craze attach` and `internal/tui`'s socket-transport frame goldens |
| `internal/roster` | The session list's data (plan 030 §3.9): a poller over this user's host registry — one kept, never-reconnecting connection per host, asked `sessions.list` once a second within a budget, backing off from a host that does not answer — and this `CRAZE_HOME`'s session index for the saved sessions; the TUI reads its latest snapshot (`tui.Config.Sessions`) |
| `internal/rundir` | Where a craze host puts what other processes must reach: the runtime namespace (the socket), and the registry, host locks, session locks, host logs and the model catalog cache under `~/.cache/craze/` (plus the process identity the launcher's last-resort agent kill checks) |
| `internal/modelcache` | The model catalog cache (plan 030 §3.14): one file per ACP provider holding the models its agent last installed, written by `craze serve` under a per-provider lock that keeps the newest observation, read by the session list's `/model` |
| `internal/fakehost` | The in-process twin of `cmd/craze-fake-host`: a scripted control-socket server for wire fixtures and tests |
| `internal/cli` | Cobra: the default TUI (the launcher), `prompt`, `bridge`, `attach`, `ps`, `new`, `send`, `wait`, `cancel`, `stop`, `providers`, `auth`, `serve` (the detached host), hidden `hub` and `frame`, `version` |
| `internal/engine` | The driver every client shares (plan 021): one engine wraps one `agent.Session` and owns the turn driver, the message queue and send-now, the ask registry, command receipts, the index writes and the session's durable id |
| `internal/harness` | The native agent: the model conversation, the turn loop, tools, background children, compaction, side questions and the session store. Sub-packages: `llm` (model clients), `modeltable` (the shipped catalog and the user's two files merged over it), `tool` (the tool framework), `store` (the JSONL transcript) and `redact` (keeps craze's own keys out of what a model sees) |
| `internal/hub` | The per-machine hub (`craze hub`): knows every host through the registry, routes `session.connect`, creates sessions and reports what can start; exits when idle |
| `internal/host` | Status reports to the terminal multiplexer craze runs in (herdr, roost) |
| `internal/hostspawn` | The process machinery of spawning a detached host: its command line, start, ready line, end, and the last-resort kill of the agents it leaves |
| `internal/sessions` | The session index, `~/.craze/sessions.jsonl`, behind `--continue`, `--resume` and `/rename` |
| `internal/journal` | The per-incarnation session journal under `~/.craze/journal/` |
| `internal/transcript` | The render-free model of a transcript, folded from a session's events; the engine and every client fold the same library |
| `internal/chatgptauth` | Sign in with ChatGPT: the browser sign-in, token store and token source, and the plan's model list |
| `internal/signinlog` | The ChatGPT sign-in log: one value-free JSON line per attempt |
| `internal/caplog` | A log file capped at a size with one rotation (the host log, the sign-in log) |
| `internal/atomicfile` | The lock and the atomic write the on-disk stores build on |
| `internal/paths` | Where craze's own files live: the home directory, the craze directory and its fixed names |
| `internal/plugin` | Tests only: checks the Claude Code plugin the repository ships (`.claude-plugin/`, `agents/`, `skills/`) |
| `internal/textdiff` | Diff hunks for edit tools |
| `cmd/craze-fake-agent` | Scripted ACP stdio server (`echo`, `todos`, `diff`, `ask`, `plan`, …) |
| `cmd/craze-fake-host` | Scripted control-socket server, driven by the same wire fixtures as `internal/fakehost` |

## Session

`agent.Session` starts the child, then exposes a stream of events the TUI and
`craze prompt --json` both consume:

| Event | Meaning |
|-------|---------|
| `text` / `thought` | Streaming assistant output; `Event.Agent` names the sub-agent when the line belongs to one |
| `tool` | Tool call create/update (merged by id, per session for a child) |
| `user` | A chunk of a sub-agent's prompt (always carries `Agent`) |
| `subagent` | Sub-agent lifecycle: spawned / progress / finished |
| `todos` | Cursor todo list replace or merge |
| `permission` / `question` / `plan` | Blocking card |
| `done` / `error` | Turn finished |
| `meta` | Session snapshot (models, modes, commands, title) |

The TUI turns those events into transcript rows, the tasks panel, status
rows, and cards. `craze prompt --json` writes the same events as JSON lines.

Grok runs its own sub-agents and reports them: lifecycle notifications
(`subagent_spawned` / `-progress` / `-finished`) arrive on the parent session
while the child's own `session/update` stream arrives on the same stdio under
the child's session id. The ACP client keeps an allowlist of child session
ids — registered on `spawned`, dropped after `finished`, capped — and tags
every forwarded update with the child id (`SessionNotification.Child`), so
routing never depends on a session id the session layer holds. `agent.Session`
keeps the per-child state (tools, prompt, progress counters) behind one
`SubagentInfo` list on the snapshot; the TUI keeps one transcript per child
and shows it read-only in the sub-agent view. Cursor has no child stream —
the session synthesizes the same records from `cursor/task` receipts.

## Host and client

The session does not live in the terminal's process.
`craze serve` is the **host**: it owns the engine, the agent process, the
claim on the session, the journal and the session-index writes, and serves the
session over the control socket. The ordinary `craze` is a **launcher** and
its TUI a **socket client** — the same `remote.Session` a `craze attach` uses,
behind `backend.Backend`, so the TUI cannot tell a host it spawned from one it
found. Closing the terminal closes a client, not the session.

```mermaid
flowchart LR
  launcher["craze (launcher + TUI)"] -->|"spawn: setsid, ready pipe"| host["craze serve"]
  host -->|"ready line: ok / held / error"| launcher
  launcher -->|"control socket"| host
  attach["craze attach"] -->|"control socket"| host
  host --> engine["engine + agent.Session"]
  engine --> agentproc["cursor-agent / grok / native harness"]
```

**The spawn handshake.** The launcher mints a host id and starts `craze
serve` with stdio on `/dev/null`, its own session (`setsid`), and a pipe on
which the host writes one JSON line once its registry entry carries the
session's identity: `ok` (host id, socket, craze session id, version), or not
ok with an `error`, marked `refused` when the session *asked for* was the
problem (no such row, a spawn flag the provider cannot take), or `held` (with
the holder's id and pid) when another craze already has the session. The pipe
is private to the two processes; it is not part of the control protocol. A
`held` answer is followed to the holder — the launcher attaches to it — and a
host that fails to answer is stopped or terminated, never left running unowned. Before spawning,
the launcher looks for a live registry entry that already serves the session
and attaches to it directly; an attach that succeeds is committed to, one that
fails (a stopping host's `closing`, a start failure) falls back to the spawn.

**The lifecycle coordinator.** A host has one stop sequence, run once on the
host's own goroutine: a `host_stop` note in the journal (why it stopped), the
attach fence, the engine's close, the control server's close order, the
start's join (bounded), then the claims and the agent record are released.
`session.stop`, SIGINT/SIGTERM and the idle watcher only *request* it. A host
that cannot join its start in time kills the agents it recorded itself
(each record is a process-group id plus the leader's start time, so a group
whose leader had already been reused when checked is not signalled; a short
race remains between that check and the signal — SF-81); the launcher does the same for a host it had to give
up on.

**The idle fence.** The idle watcher looks once a second; when its clock
(`host_idle_exit`) has run out it raises two fences — the server's attach
fence and the engine's admission fence — and only then counts attachments and
whatever is in flight. Nothing: the stop, fences left up. Anything: both are
released and the clock restarts. So a client that attaches, or a command that
is admitted, at that instant is either counted or refused with `closing`
(protocol code `unavailable`), never accepted into a session already ending.
A native session's owed background work counts as in flight; an ACP agent has
no admission fence and could start a turn of its own after the verdict (the
stop's close ends it).

**The opt-out.** `detach = false`, `CRAZE_DETACH=0`, or the control socket off
keep the older path: `runTUI` builds the engine in the TUI's own process, binds
its own socket when the control socket is enabled, and closes the session with the terminal. Both paths are kept
under test (`tests/cli` runs its core cases detached and in process); the
choice is `detachOn` in `internal/cli/launch.go`. See
[Configuration](../reference/configuration.md#detached-hosts).

## The hub

`craze hub` is one process per user and craze directory. It reads the host
registry that every host already writes, so it knows each running session
without being told. A client asks it for the roster (`sessions.list`), routes
through it to a session (`session.connect`: it splices the connection to the
session's host and steps out), asks it to start a session (`session.create`),
asks what can start (`sessions.createOptions`), asks for the saved
sessions (`sessions.saved`) and asks to bring one back (`session.reopen`:
it joins the host serving the session, or spawns one that loads it, the
session's claim deciding between racing loaders). `craze ps`, `craze new`,
`craze providers --hub` and the TUI's session list use it. It starts on
demand, holds no session state, and exits once no client and no live host
remain (60 seconds by default, `CRAZE_HUB_IDLE`); killing it loses nothing,
because hosts run without it. A host the hub creates is spawned through
`internal/hostspawn`, as the launcher spawns its own.

## Unattended sessions

`craze new --wait` starts a host for no one to attend, which is what the Claude
Code plugin uses, and a `craze send` that reloads an ended session does the same
when the session's index row carries the `unattended` record; a person's session
reloads as an ordinary interactive host. The command spawns
the host directly (the launcher's path through `internal/hostspawn`, not the
hub's `session.create`), reparented out of the command's process tree so that
the command's exit, or a signal sent to it, does not take the host with it. The
host is told `--unattended`: it is not interactive (native background work is
off), questions are answered with their first option, and an ACP agent's plan
is returned rather than accepted. The session's index row carries `unattended`
as provenance, which is what makes `craze -c` skip it. See [`craze new
--wait`](../reference/cli.md#craze-new-wait).

## Side questions (`/btw`)

A side question is answered beside the session, never in it. The TUI sends a
`session.aside` request over the control socket (or in process); the engine asks
the session, and the answer streams back as `aside` notifications of that request alone. The
question and answer are written nowhere in the transcript, put on the
session's event stream or steered into a running turn. The one trace is
usage: on native, the tokens a side question spends reach the session's
stream as a usage update, which is how the session's total counts them. Native builds the request from the
session's own context, so a provider's prefix cache still matches it. Grok and
gx answer through their one-shot `_x.ai/btw` method. Cursor has no
equivalent, so `/btw` is not offered there. See [Side
questions](../reference/tui.md#side-questions-btw).

## Fake agent

`craze-fake-agent` stands in for `cursor-agent acp` or `grok agent stdio`.
Unknown arguments (including `acp`, `--force`, `agent`, `stdio`, and
`--always-approve`) are ignored so the binary can be pointed at with
`--agent-bin`. Scripts are selected with `-script` or `CRAZE_FAKE_SCRIPT`.
Grok scripts are prefixed `grok-`. See [Testing](testing.md).
