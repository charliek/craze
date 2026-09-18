# 01 — Goals and scope

## Goal

Own the agent loop. Today craze is an ACP client: cursor-agent, grok, or gx
runs the agent and craze draws it. The native harness runs the agent inside
craze itself, against the models the user already pays for through gx
(Fireworks, Z.AI, OpenRouter, Meta, xAI, the OpenAI API), with craze's
existing TUI, event model, and headless `craze prompt --json` unchanged.

Success looks like: pick the native provider, choose a gx model, and get the
same transcript, cards, queue, plan mode, sub-agent rows, and resume that a
grok session gives today, without a second process on the wire.

## Why fantasy

- One Go API over every provider we need; the agentic loop (multi-step tool
  calls, parallel tools, retries, tool-call repair, cancel via context) is
  already written and is what crush ships on.
- Stateless by design: the harness owns the conversation, which is what we
  want for a store we control.
- Apache-2.0. Pre-1.0 (v0.43.x) and moves with crush, so pin and expect
  breaking minors.

## In scope

| area | first phase |
|---|---|
| gx model set via a craze-owned model table + overlay, reasoning effort, model switch mid-session | H1 |
| JSONL tree session store; resume, fork, rename | H1, H7 |
| read, ls, glob, grep, bash, write, edit; centralized truncation; edit diffs | H2 |
| permission cards: once / always / reject-with-feedback; per-project grants; yolo | H3 |
| CLAUDE.md / AGENTS.md / rules; Claude skills; marketplace plugins via import | H4 |
| plan, ask, implement modes; plan and question cards; todos | H5 |
| sub-agents with the existing rows and per-child transcript | H6 |
| compaction; cost per turn | H7 |
| image paste | H8 |

## Non-goals (for now)

These are deliberate. Each has a row in `08-decisions.md`.

- **Reading `~/.claude/*` at runtime.** User-level Claude content is imported
  into the harness home; project-level files are read from the workspace.
- **Claude `settings.json`** (permissions, env, defaultMode). Not translated.
- **Hooks** (PreToolUse and friends, plugin `hooks/hooks.json`).
- **MCP**, including plugin `.mcp.json`. Tool wrappers are shaped so it can
  be added later without touching the loop.
- **ChatGPT-plan (codex) auth.** gx does it; the harness starts without it.
- **A native marketplace installer.** Import from Claude Code's cache first.
- **An ACP server binary.** The core is kept free of craze types so this
  stays a thin wrapper later, but it is not built now.
- **Sandboxing.** Same stance as pi and crush: containerize if you need a
  boundary; the harness does not pretend to be one.

## Visibility policy

The native provider is **hidden**. It never appears in the startup picker,
even when everything it needs is present. It is reachable only when named
directly: `--provider native`, `CRAZE_PROVIDER=native`, or a persisted
`provider = "native"` in craze's config (which only gets there after an
explicit selection). This holds through every phase until the roadmap says
otherwise; flipping it to visible is its own decision, not a side effect of
a phase landing.

Implementation note: `agent.Provider` already has `optional` (hidden unless
the binary resolves — gx). This needs a second, stricter flag, `hidden`
(never listed). `DefaultProviders`, `pickerProviders`, and the provider
dialog must all honour it; `ProviderByName` must still resolve it.

## Target models

From `~/.grok/providers.toml` and `~/.grok/config.toml` as of 2026-09-15:

| provider | models | wire |
|---|---|---|
| fireworks | kimi-k3, qwen3p8-max, deepseek-v4-pro, kimi-k2p7-code, deepseek-v4-flash | chat_completions |
| zai-coding-plan | glm-5.3, glm-5.3-flash | chat_completions |
| openrouter | minimax-m3, gemini-3.8-flash (+ legacy gpt-5.6 and glm entries) | chat_completions |
| meta | muse-spark-1.3, muse-spark-1.3-contributor | chat_completions |
| openai-api | user-added | responses |
| openai-codex | gpt-6-astra, gpt-5.6-sol/terra/luna | responses, deferred |
| xai direct | grok-4.x | needs a funded API key |
