# Changelog

Each release gets its own section headed `## vX.Y.Z — YYYY-MM-DD`, newest first.

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
