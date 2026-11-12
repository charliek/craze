# Changelog

Each release gets its own section headed `## vX.Y.Z — YYYY-MM-DD`, newest first.

## Unreleased

The waiting commands become a contract a program can drive unattended: typed
failures it can branch on and retry, a session that ends with its turn, a session
file that names the session before the result, `stop` and `cancel` safe to send
twice, and prompts up to 512 KiB. A program that reads `craze new --wait --json`,
`craze send --wait --json` or `craze wait --json` needs changes: [Upgrading, since
v0.2.0](docs/getting-started/upgrading.md#since-v020) lists each one. Nothing in
`~/.craze` needs migrating; restart a running hub after upgrading.

### Behaviour changes

- The waiting commands' JSON: `error` is one object, `{message, kind, retryable,
  detail?}`, on a result and on an exit-1 failure alike (it was a string on a
  result); a `failed` result always has one, and the plain footer's `stopped:`
  line is folded into `error:`. `stopReason` is on every terminal status,
  `model` is left out until it is resolved, and `planEnded` is new.
- A turn a stop cuts short (`craze stop`, a `SIGTERM` to the host) is a
  `cancelled` result, exit 3, with what it did and spent so far, `stopReason:
  stopped` and `usageComplete: false` when a step was in flight. It was a
  failure, exit 1. A killed host is told from a stop and from a lost connection.
- `craze stop` on a session that has ended is exit 0 (`already_ended`); it was
  exit 1.
- `files` lists only successful edits inside the workspace, relative to it;
  edits elsewhere move to `outsideFiles`, the session's plan file to `planFile`.
- Unattended hosts (`craze new --wait`, and `craze send` reloading one) idle out
  after `host_idle_exit_unattended`, ten minutes by default, and an open
  permission ask no longer holds them up. They stayed for `host_idle_exit`, an
  hour by default.
- A usage error under `--json` prints an `{"error":{…}}` object on stdout, and
  plain `craze new --json` gains `kind`.
- A native host killed outright has its shell commands ended (`SIGTERM`, then
  `SIGKILL`) by the waiter that sees it go or by the next craze command; every
  native command has `CRAZE_HOST_ID` in its environment.

### Sessions and scripting

- `--exit-after-turn` on `craze new --wait` and `craze send --wait` ends the
  session with its turn: no other prompt is admitted, and the host is gone by
  the command's exit.
- `--session-file <path>` writes the session's identity (`session`, `short`,
  `hostPid`, `hostId`) before the prompt is sent, and the round's `handle` once
  it is taken.
- `craze stop --json` and `craze cancel --json`, and `craze cancel --turn
  <turn>`, which cancels that turn only while it is current, so a cancel sent
  again never reaches the next queued turn.
- `craze ps --json --brief` leaves out each session's model and mode
  catalogs.
- Failure kinds: each `error` names one of a closed set of kinds and whether it
  may be retried (`provider`, `stopped`, `host_exited`, `connection_lost`,
  `unobserved`, `prompt`, `config` and the rest); cursor's and grok's refusal of
  `--model` reads `the agent refused model "x": <the agent's words>`, kind
  `model_not_found`.
- A native step that fails mid-stream (an HTTP/2 reset, an unexpected EOF, a
  server error event) is sent again from the same input, once by default
  (`provider_retries`, 0 to 3). The result gains `retries`; `craze prompt
  --json` gains a `step_retry` line, and the control protocol a `step_retry`
  event and `retracted` entries.
- `craze new --wait` and `craze send` take a prompt of up to 512 KiB, from
  words, `--prompt-file` or stdin (was 32 KiB, #111). The bound fits the
  socket's 4 MiB request line with any JSON escaping; over it is still exit 2,
  kind `prompt`.
- A `craze send` over 32 KiB to a session that is running a turn would have to
  wait in the session's queue, which still holds 32 KiB a row: the session
  refuses it, exit 1, kind `prompt`, nothing queued, in words of its own (`it
  is over 32 KiB, the most a prompt can wait in the session's queue, and the
  session was busy; send it once the turn has ended`). On v0.2.0 any prompt
  over 32 KiB was refused before anything was sent, a usage error, exit 2.
- The CLI reference has a new section, "A waiter run detached", for a program
  that drives craze from elsewhere: run each round's waiter in a session of its
  own with its result and exit code in files (the exit file marking the round's
  end, and the round's pid telling a running round from a lost waiter; macOS,
  which has no `setsid` binary, calls `setsid(2)` from the driving program),
  `craze cancel` and `craze stop` to end a round or the session, no
  `--timeout`, plain `craze wait` to read a
  lost round, `CRAZE_RUNTIME_DIR` on a machine people log in to, and the last
  round's `usage.session` as the session's total (a floor when a step was cut
  short, sent again or unpriced). End-to-end tests run it that
  way: detached, no terminal, across a host that idled out and was loaded
  again, and a later `craze send --wait` round that a plain `craze wait` reads
  without owning and `craze cancel` ends.
- `craze auth login` from a pipe takes a key with no trailing newline (it
  always did; now tested and documented).

### The native provider

- A shipped model can be deprecated (`deprecated` and `replaced_by` in the
  catalog): it runs when named, is never offered or chosen for you, and each
  `models.toml` reference to it warns. A `[[retired]]` row can name its
  replacement, which a `--model` naming it leads with (`<alias> was retired;
  use <replacement>`) and a dropped reference warns about. A model is retired
  no earlier than the next minor release and 30 days after a release shipped
  it deprecated (RELEASING.md; `TestDeprecationHistory` holds the catalog to
  the release half).
- `default_context`: a context smaller than the window that a session works
  within — compaction's threshold and the usage report's `contextWindow` are
  measured against it — set by the catalog and per model in `models.toml`
  (`0` turns it off).
- `--model <provider id>/<alias>`: `zai-coding-plan/glm-5.3` is `glm-5.3`.
- The agent tool's model list and its "Unknown model" answer hold to the
  models `/model` offers plus the ones you configured.

### Maintenance

- Go 1.27.3 (was 1.27.1; `go.mod`'s floor is now `go 1.27.3`) and golangci-lint
  2.15.0 (was 2.13.2, which cannot read Go 1.27.2+'s export data). Its errcheck
  now reports an error received from a channel and dropped; the three places
  that did so wait without discarding a received error.
- Fantasy v0.45.0 (was v0.43.2). Fantasy builds on Charm's fork of the OpenAI
  SDK (`github.com/charmbracelet/openai-go`) from v0.45.0, so
  `github.com/openai/openai-go/v3` leaves the build; the dependency-boundary
  tests forbid the fork as they forbid the SDK. Through it: an
  OpenAI-compatible provider that reports reasoning tokens apart from output
  tokens has them counted in output (and spend), tool messages stay together
  when tool results carry images (craze's own regrouping is unchanged), and
  JSON repair fixes. `bin/craze` is 44,139,178 bytes (was 42,484,176).
- **Held:** Fantasy v0.45.1 to v0.48.0. From v0.45.1 Fantasy no longer retries
  a step because a tool returned an error, and four of the native harness's
  cancel and callback tests fail against it; that is more than a version bump.
- golang.org/x/sys v0.50.0, golang.org/x/image v0.48.0, spf13/pflag v1.1.0,
  kaptinlin/jsonschema v0.9.10 and charmbracelet/x/term v0.2.2; no TUI golden
  or wire fixture moved.
- **Held:** charmbracelet/x/ansi v0.11 (stays v0.10.1), and with it
  charmbracelet/bubbles v1.0.0 (needs it; stays v0.21.0) and
  mattn/go-runewidth (stays v0.0.16). x/ansi v0.11.9 measures some graphemes
  (Devanagari with combining marks, keycap emoji) differently from the
  grapheme walk the TUI copies and highlights with: copying a whole row like
  `नमस्ते hello` dropped its last character, highlighting next to `1️⃣` erased
  the character after it, and a clamped title could overrun its width by a
  cell. A test now holds the TUI's width measures to one another.
- GitHub Actions: actions/checkout v7, actions/setup-go v7, astral-sh/setup-uv
  v10.3.3.
- Python locks: `eval/` takes aiohttp 3.14.5 (three advisories fixed in 3.14.4)
  and pytest 9.2.0; the docs site takes zensical 0.1.1.

## v0.2.0 — 2026-10-14

Effort becomes a control of its own on native models (`/effort`, and a switch
lands on the model's own effort), Meta's models move to its Responses API at
about 0.6 times the spend, saved sessions reopen through the hub, and Gemini 4
Argon joins OpenRouter. This is the release the repository's Claude Code plugin
needs (craze 0.2.0 or newer). Nothing in `~/.craze` needs migrating, but several
existing behaviours changed: [Upgrading, since
v0.1.1](docs/getting-started/upgrading.md#since-v011) lists each one and how to
keep the old behaviour.

### Behaviour changes

- A native `/model` switch lands on the effort you last used on that model, else
  its `default_effort`, instead of carrying the current effort over. Name a
  level to keep one: `/model anthropic/claude-opus-5-5 max`.
- Native Meta models use Meta's Responses API (the new `openai-responses`
  driver): craze replays each step's encrypted reasoning and Meta caches the
  session's prompt. On `muse-spark-1.3-contributor` it passed as many tasks as
  Chat Completions at about 0.6 times the median spend per run. A
  `[providers.meta]` with `driver = "openai-compat"` goes back; an entry of your
  own with a `base_url` and no `driver` now sends Responses requests.
- Haiku 5.5 requests whose prompt is over 100,000 tokens are priced at
  Anthropic's long-context rates, so their spend no longer reads low.
- Plain `craze prompt` puts a blank line between text runs; `--json` is
  unchanged.
- A stopped session leaves the roster, the session list and `craze ps` at once,
  with no `Closing…` row (for hosts started after the upgrade).
- `/model <id> <effort>` matches the effort word as `--effort` does, and refuses
  a word that names two levels. A `models.toml` model with more than 8 efforts,
  or an effort that is not one short printable line, no longer loads.
- The session list offers a saved session by its newest row only.

### The native provider

- **OpenRouter**: Gemini 4 Argon (`openrouter/gemini-4-argon`), with efforts,
  vision and its price. Gemini Flash Latest stays the start model.
- **Meta**: `muse-spark-1.3` offers `max`.
- The `openai-responses` driver serves any Responses API endpoint with an API
  key, and a `models.toml` cost can carry a long-context tier
  (`long_context_above`, `[models."<alias>".cost.long_context]`).

### The terminal UI

- `/effort` opens a picker of the current model's levels; `/effort <level>` sets
  one. The model dialog's effort row follows the highlighted model, and the
  session list's `/effort` offers the chosen model's real levels.
- Shift+Enter starts a new line in Ghostty and Roost.
- A `/model`, `/plan`, `/ask` or `/agent` typed mid-turn says why it did
  nothing, and the draft stays.
- The session list shows each running session's model at 100 columns and wider.
- Reasons, fixes and notes wrap instead of being cut short, and a long path
  breaks after a `/`.

### Sessions and scripting

- `craze ps --saved` lists saved sessions no host runs, and
  `craze new --load <id>` reopens one in a host of its own. A hub client does the
  same with `sessions.saved` and `session.reopen`.
- `craze bridge --hub --no-start` attaches to a running hub and never starts one
  (exit 3 when none runs).
- When an upgrade replaced its binary, a hub from before says how to stop it
  (`kill <pid>`; no session ends), and `craze new` explains an older hub's
  refusal.
- The Claude Code plugin gains a `craze:wonder` preset (`muse-wonder-1.0`).
- The control protocol gains effort facts on native catalog models
  (`capabilities.effortFacts`), the slash catalog's builtin `effort` row,
  `stopping` on a registry entry, and the two saved-session methods. All
  additive: the protocol and codec versions are unchanged.

### Fixes

- A closing native session cancels its own turn before its sub-agents, so it no
  longer asks the model once more with their aborted results.
- A model-dialog switch sends the effort the dialog showed, even when another
  craze process changed the remembered effort since.
- A failed ChatGPT-plan model list fetch backs off only that account.
- The session list keeps ` · here` whole and shortens the directory's name
  instead.

## v0.1.1 — 2026-10-12

Anthropic joins the native providers, `/btw` asks a side question without
touching the session, and new commands drive sessions from a script. Nothing in
`~/.craze` needs migrating, but a few existing behaviours changed: [Upgrading,
since v0.1.0](docs/getting-started/upgrading.md#since-v010) lists each one and
how to keep the old behaviour.

### Behaviour changes

- An exported `ANTHROPIC_API_KEY` now funds craze's Anthropic provider, and is
  kept out of the commands a native session runs. `env_keys = []` under
  `[providers.anthropic]` in `providers.toml` opts out. A
  `[providers.anthropic]` of your own now overlays craze's: rename it to keep a
  gateway.
- `craze -c` and `--continue` skip sub-agent sessions (those `craze new --wait`
  starts). `--resume` lists them after your own, tagged `sub-agent`.
- OpenRouter aliases send OpenRouter's `~family-latest` routers where one
  exists (`openrouter/gpt-sol-latest`), so an alias moves when OpenRouter moves
  its router. The numbered aliases they replace are retired; their wire ids
  still work from `models.toml`.
- A skill saved mid-session reaches the `/` menu at the next turn end, not when
  you type `/`.

### The native provider

- **Anthropic**: Claude models on Anthropic's API with an API key
  (`craze auth login anthropic`, `/connect`, or `ANTHROPIC_API_KEY`). The
  account's model list is fetched at most once a day, so new Claude models
  appear without a craze release. It is last in the start order; a session with
  only an Anthropic key starts on the newest Opus at medium effort.
- Under an Anthropic parent, a sub-agent's `opus`, `sonnet`, `haiku` or `fable`
  picks that family's newest Claude model unless `[subagents.tiers]` maps it.
- **Meta**: Muse Wonder 1.0 (`muse-wonder-1.0`), chosen with `/model`,
  `--model`, `default_model` or a sub-agent's model. Muse Spark stays Meta's
  start model.
- **OpenRouter**: MiMo V2.6 Pro and the `glm-latest` and
  `deepseek-flash-latest` routers. The start model is
  `openrouter/gemini-flash-latest`.
- A turn the model refuses ends with a note naming the refusal category. The
  note is shown but never saved in the session or sent to a model.

### The terminal UI

- `/btw <question>` asks a side question in a full-window view, mid-turn or
  idle, without adding to the session; Esc throws it away. Native streams the
  answer; Grok and gx answer in one piece. Cursor does not offer it.

### Scripting

- `craze new --wait`, `craze send`, `craze wait`, `craze cancel` and
  `craze stop` start, continue, wait on and stop sessions, with an exit code per
  outcome. The Claude Code plugin in this repository is built on them; it is
  supported from v0.2.0.
- `craze new --title` names a session as it starts. A hub still running from
  before the upgrade refuses until it exits idle.
- `craze prompt --effort` and `--fast`/`--no-fast`.
- The control protocol gains `session.aside` (side questions), the `slash`
  catalog section and a `title` on `session.create`. All additive: the protocol
  and codec versions are unchanged.

### Fixes

- `craze wait` no longer reports `completed` when an interrupt or `--timeout`
  ended the wait before it read a returned plan.
- craze no longer mistakes a dead host for a live one while another scan is
  cleaning it up.
- A title is normalized the same way in the session and in the session index
  (zero-width spaces, bidi controls and carriage returns are dropped).

### Docs

- The README is now a short overview that links to the docs site. The site
  gains a [Claude Code sub-agents](docs/getting-started/claude-code.md) page and
  an Upgrading section for changes since v0.1.0, and tests in `make test` fail
  when the reference pages fall behind the code.

## v0.1.0 — 2026-10-05

Two big changes since v0.0.1. craze now has its own agent, the **native**
provider, which runs against a model provider's API or a ChatGPT plan with no
other CLI to install. And sessions now run in a detached host that outlives the
terminal. Cursor, Grok and gx work as before. Nothing in `~/.craze` needs
migrating, but several defaults changed: [Upgrading from
v0.0.1](docs/getting-started/upgrading.md) lists each one and how to turn it
off.

### Breaking changes

- `CRAZE_CONFIG` is removed. Set `CRAZE_HOME` to the **directory** that holds
  `config.toml`. While `CRAZE_CONFIG` is set, every command but `--help` and
  `--version` refuses to run and says so.
- Sessions run in a detached host by default. `detach = false` in
  `config.toml`, or `CRAZE_DETACH=0`, keeps the session in the TUI's own
  process, as v0.0.1 did.
- `--agent-bin` and `CRAZE_AGENT_BIN` now apply only to the launch's own
  provider. To set a provider's binary for good, use `[agents]` in
  `config.toml`.

### The native provider

- `--provider native` runs the agent inside craze. It connects to Fireworks,
  Meta, OpenRouter or the Z.AI Coding Plan with an API key, or to a ChatGPT
  plan (`craze auth login chatgpt`, or `/connect` in the TUI).
  `craze auth login`, `logout` and `list` manage the keys.
- The model catalog ships in the binary, so upgrading craze brings new models;
  `models.toml` changes or adds models. `/model` switches model and effort.
- Where a new session starts: on your last model, else your `default_model`,
  else the start model of the first connected provider in this order:
  - Z.AI Coding Plan: `glm-5.3`
  - ChatGPT plan: its preferred model
  - Fireworks: `fireworks/ember-1`
  - OpenRouter: `openrouter/gemini-3.8-flash`
  - Meta: `muse-spark-1.3-contributor`
- Tools for reading, searching, editing and running commands. A command still
  running at its timeout becomes a background job instead of being killed.
- Agent, plan and ask modes, and foreground and background sub-agents, each
  sub-agent on a model you can choose.
- Your project and user instruction files and your skills and commands are in
  its prompt.
- Resume, `/compact` and automatic compaction, and usage and cost per session.
- Vision models see pasted images and images read from disk.

### Sessions outlive the terminal

- Each session runs in a detached host (`craze serve`). Closing the terminal or
  losing an ssh connection closes only the view.
- `craze -c` or `craze attach` rejoins a session and `craze ps` lists them.
  `craze new` starts one in the background through a per-machine hub, which
  starts and exits on its own.
- A host with no client and nothing to do exits after an hour
  (`host_idle_exit`).
- Every session writes a journal (`journal = false` turns it off), and every
  host has a control socket (`control_socket = false`).

### The terminal UI

- A session list (`←` on an empty composer, or `/sessions`) to switch to,
  start and stop sessions. A new session's directory can be chosen with `@`,
  along with its provider, model, effort and fast mode.
- Images: paste or drag a screenshot, or Ctrl+V an image, and it becomes an
  `[Image #N]` chip. Cursor and Grok receive the image itself.
- `@` file mentions, and `!` shell mode, which runs a command from the
  composer.
- `/model` sets effort and fast mode on each provider that offers them.
- Inside herdr or roost, craze reports idle, working or blocked, and never
  conversation content (`host_status = false` turns it off).

### Scripting

- `craze prompt --json` events now carry a `seq`.
- `craze bridge` relays an SSH client to a session's control socket; the
  socket's protocol is documented in `docs/reference/protocol.md`.
- `craze providers` says which providers can start a session here, and why the
  others cannot.

### Known limits

- Still a proof of concept.
- On macOS, dragging a screenshot into Ghostty inserts its path rather than an
  image chip (#5). Pasting from the clipboard works, and so does dragging in
  roost.
- Intel macOS is untested at runtime.

## v0.0.1 — 2026-09-16

First release. craze is a Linux and macOS terminal UI that speaks ACP to an
agent you already have: **Cursor** (`cursor-agent acp`), **Grok**
(`grok agent stdio`), or **gx**, a third-party fork of the Grok CLI. craze owns
the chrome; the provider still runs the agent. It is a proof of concept —
usable, but the version number is honest.

### Install

Homebrew (`brew install charliek/tap/craze`) and apt
(`https://apt.stridelabs.ai`), both from this release. Apple Silicon and Linux
amd64/arm64 are tested; Intel macOS is cross-compiled and shipped best-effort.

### The terminal UI

- A transcript of rendered entries — markdown, tool rows rewritten in place,
  diffs, todos and streamed assistant text — rather than a debug log.
- A boxed composer that keeps the whole draft on screen, with slash commands
  that open on a token anywhere in the draft.
- Questions, plans and permission requests arrive as cards you answer inline.
- Sub-agent rows for both providers, in spawn order, with a read-only view of
  any child's own transcript.
- Status rows and a clickable mode chip; mouse selection and copy from the
  transcript; wheel scroll separate from PgUp/PgDn.
- Seven themes with a picker that repaints live as you move through it, and a
  themed terminal background (OSC 10/11) that is handed back on exit —
  `background = false` or `--no-background` turns it off.

### Sessions

- `--continue` resumes the last session; `--resume` opens a picker; `/rename`
  names one. The terminal's tab title follows the session.
- A message queue: type while a turn runs, send now, or interject mid-turn.
- Plan mode offers to implement the plan when a plan-mode turn ends.

### Providers

- Cursor, Grok and gx, chosen with `--provider` or a startup picker that only
  offers the ones that actually resolve on your machine.
- Cursor's plugin commands are found and expanded client-side, because its ACP
  server never advertises them.
- Skills are scanned by each agent's own rules rather than one guess at both.

### Headless

- `craze prompt` drives an ACP turn without a TUI, for scripting and tests.
- `craze frame` renders a deterministic frame, which is what the golden suite
  compares.

### Known limits

- Proof of concept: multi-project harness integration is later work.
- Intel macOS is untested at runtime.
- A terminal that honours OSC 11 but not OSC 10 (or that ignores the reset)
  can be left recoloured after an abnormal exit; `background = false` avoids
  the whole mechanism.
