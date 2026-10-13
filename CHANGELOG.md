# Changelog

Each release gets its own section headed `## vX.Y.Z — YYYY-MM-DD`, newest first.

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
