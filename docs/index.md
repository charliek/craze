# craze

A Linux and macOS terminal UI for coding agents. It talks ACP to
[Cursor CLI](https://cursor.com/cli) (`cursor-agent acp`),
[Grok CLI](https://docs.x.ai/build/cli/headless-scripting) (`grok`), Google's
Antigravity CLI (`agy`, [the antigravity provider](reference/configuration.md#the-antigravity-provider)) or your own
Claude Code on your Claude plan ([Claude on your plan](getting-started/claude.md)),
talks JSON-RPC to OpenAI's Codex CLI (`codex app-server`,
[the codex provider](reference/configuration.md#the-codex-provider)), and ships its own **native** provider that runs the agent inside craze instead,
against a model provider's API or a ChatGPT plan, needing no other CLI (see
[Native provider](getting-started/quick-start.md#native-provider)).
You own the chrome; the provider still runs the agent.

```bash
./bin/craze                      # the TUI, in the current directory
./bin/craze --workspace ../proj  # somewhere else
./bin/craze --no-force           # ask before each tool call
```

`--force` (yolo) is the default. `--no-force` turns on the permission line.

## Current Scope

Early-stage software, released for Linux and macOS (Homebrew, apt and release
archives). `craze` opens a TUI; `craze prompt --json` is the headless
path. Sessions run in a detached host and outlive the
terminal: `craze ps` lists them, `craze attach` and `craze -c` rejoin one,
`craze new` starts one in the background through the per-machine hub, and
`craze new --load` brings a saved one back through it (see the
[CLI reference](reference/cli.md#sessions-outlive-their-terminal)).

## What it does

- **Code reviews.** Bundle your changes and review them with an eligible provider. [Review command](reference/cli.md#craze-review)
- **Cross-provider targets.** Preview a model family, version pin or review panel with `craze resolve`. [CLI reference](reference/cli.md#craze-resolve)

- **Grouped tool calls.** Open individual groups with the keyboard and preview
  eight terminal themes, including high contrast. [Transcript layouts](reference/tui.md#transcript-layouts)

- **A TUI over six providers.** `cursor`, `grok`, `claude`, `antigravity`, `codex` and the built-in
  `native` agent share one set of keys, cards and slash commands
  ([TUI reference](reference/tui.md)).
- **Claude on your plan.** Your own Claude Code on your Claude Pro or Max
  plan, with its plan usage on the status row and a guard that stops a
  session Claude says is billing extra usage
  ([Claude on your plan](getting-started/claude.md)).
- **Sessions that outlive the terminal.** A detached host keeps the agent
  running; `craze ps`, `craze attach` and `craze -c` rejoin it
  ([CLI reference](reference/cli.md#sessions-outlive-their-terminal)).
- **Resume through the hub.** `craze ps --saved` lists the saved sessions and
  `craze new --load` reopens one; a hub client such as shed does the same over
  [`session.reopen`](reference/protocol.md#sessionreopen)
  ([CLI reference](reference/cli.md#craze-new-load)).
- **Scripted and background use.** `craze prompt --json` runs one turn;
  `craze new`, `send`, `wait`, `cancel` and `stop` drive sessions without a
  terminal ([CLI reference](reference/cli.md#craze-new)). The waiting commands
  give typed failures a program can branch on and retry
  ([failure kinds](reference/cli.md#failure-kinds)), end a session with its
  turn ([`--exit-after-turn`](reference/cli.md#ending-with-the-turn)), name the
  session before the result ([`--session-file`](reference/cli.md#the-session-file))
  and take prompts up to 512 KiB; a program on another machine runs them
  detached, reading each round's result from files
  ([a waiter run detached](reference/cli.md#a-waiter-run-detached)).
  [`craze models --json`](reference/cli.md#craze-models) lists the native models as JSON
  (each release ships the catalog as `models.json`), and `craze new --wait`
  refuses a model, effort or `--fast` a native model cannot take before it starts
  a session ([model checks](reference/cli.md#model-checks)).
- **Claude Code sub-agents.** A plugin hands a task to a craze session on a
  native, Grok, Cursor, Antigravity (Gemini) or Codex model, and returns the result ([Claude Code](getting-started/claude-code.md)).
- **Side questions.** `/btw <question>` asks a quick question without adding to
  the session ([TUI reference](reference/tui.md#side-questions-btw)).
- **Effort and speed.** `--effort` and `--fast`/`--no-fast` start a model at a
  chosen setting ([CLI reference](reference/cli.md#flags)); in a session,
  `/effort` changes it from a picker of the current model's levels, and on
  native the model dialog shows each model's levels as you highlight it
  ([TUI reference](reference/tui.md#effort)).
- **Native start models.** A new native session starts on the first provider
  you have connected, in the order Z.AI, ChatGPT plan, Fireworks, OpenRouter,
  Meta, Anthropic, and last an OpenAI API key stored with `craze auth login
  openai`; while a ChatGPT sign-in offers none of its models, craze never
  moves a new session on its own to a per-token provider after it in that
  order, nor onto an `OPENAI_API_KEY` that is only in the environment
  ([Configuration](reference/configuration.md#the-shipped-catalog)).
- **Native web search.** A native session can search the web and fetch pages
  through Exa, no key needed; it asks you once before the first search and
  remembers the answer, a session nobody answers never asks, and
  `websearch = false` turns it off
  ([Configuration](reference/configuration.md#native-web-search)); a waiting
  command's result reports each turn's searches, fetches and the URLs they
  returned ([web search counts](reference/cli.md#web-search-counts)).
- **A render check.** `craze frame` renders the TUI once for tests
  ([Testing](development/testing.md#craze-frame)).

To run craze you need:

- Linux or macOS
- One way to reach a model:
    - the **native provider** (`--provider native`), which needs none of the
      CLIs below: only a model provider's API key, or a ChatGPT plan sign-in
      (`craze auth login chatgpt`); or
    - one of: the [Cursor CLI](https://cursor.com/cli), with a working login
      (`cursor-agent login`, the same binary is also installed as `agent`); or
      the [Grok CLI](https://docs.x.ai/build/cli/headless-scripting) (`grok`),
      with `grok login` or `XAI_API_KEY` set; or Claude Code signed in to a
      Claude Pro or Max plan, with the `claude-agent-acp` adapter
      ([Claude on your plan](getting-started/claude.md)); or the Antigravity
      CLI (`agy`), signed in with `agy acp --login`
      ([the antigravity provider](reference/configuration.md#the-antigravity-provider));
      or OpenAI's Codex CLI (`codex`), signed in with `codex login` or `codex login --with-api-key`
      ([the codex provider](reference/configuration.md#the-codex-provider))

Go 1.27.3+ (this repo pins 1.27.3 via `.mise.toml`) is needed only to build from
source. Brew, apt and the release archives need no Go.

Upgrading from an earlier release? See [Upgrading](getting-started/upgrading.md).

## Next Steps

- [Quick Start](getting-started/quick-start.md) — install and first run
- [Upgrading](getting-started/upgrading.md) — what changed in each release
- [Claude Code](getting-started/claude-code.md) — craze sessions as Claude Code sub-agents
- [CLI Reference](reference/cli.md) — commands and flags
- [TUI Reference](reference/tui.md) — keys, cards, slash commands
- [Configuration](reference/configuration.md) — themes, config file, env vars
- [Protocol](reference/protocol.md) — the control socket's wire protocol
