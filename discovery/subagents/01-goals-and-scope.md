# 01 — Goals and scope

## The idea

A Claude Code session, usually an Opus orchestrator, hands a task to a model
Claude Code cannot run: GPT-6.1 Sol or Astra on the ChatGPT plan, Muse Spark,
GLM, Kimi or DeepSeek through craze's native harness, or the cursor-agent and
grok CLIs. The handoff should feel like a native sub-agent, the way handing a
task to a Sonnet sub-agent does:

- dispatched with the `Agent` tool and an agent type;
- working in the same directory with real read, edit, search and shell tools;
- returning one final message, while the orchestrator's context stays clean;
- running in the background with a completion notification;
- continued later with `SendMessage`, its context intact.

craze already abstracts these providers behind one session model and one
control protocol ([[craze-flagship-provider-abstraction]] in the owner's
memory, and `discovery/session-control/`). Its hub can create a session in
any directory and keep it running after the client leaves. The discovery
question was whether that is enough to build sub-agents on, and what is
missing.

## Owner direction (2026-10-10)

- The goal is parity with a native Claude Code sub-agent. A sub-agent that is
  clearly worse than delegating to Sonnet is not worth shipping.
- Uses named: cursor and grok from an Opus orchestrator; Muse Spark 1.3
  Contributor on native; Sol on the ChatGPT plan through native. The point is
  to make other models easy to experiment with.
- Replacing the `codex exec` shell-outs in the owner's plugins with craze is
  in scope as an evaluation, if craze does the job as well.
- A Haiku wrapper sub-agent is acceptable and probably helpful: it is cheap
  and keeps the orchestrator's context clean.
- craze is the owner's project. Missing craze features are in scope.
- The Claude Code plugin ships **inside the craze repo**, the way shed and
  prox ship theirs, not in `cc-plugins` (TD-01).
- The new headless commands are not exposed in the TUI (TD-04).
- Gaps found in the other commands' flags, such as `--effort` or `--model`,
  belong in this discovery too ([05](05-cli-gaps.md)).

## In scope

- Headless commands that drive a hub session turn by turn and return a
  machine-readable result ([03](03-design.md)).
- What the host and protocol must add for unattended sessions: answering asks
  with no client attached, a start mode on create, an origin tag, a
  per-session idle limit ([03](03-design.md)).
- The Claude Code plugin: sub-agent definitions, a skill, and install
  instructions ([06](06-plugin.md)).
- Flag gaps across craze's commands ([05](05-cli-gaps.md)).
- How craze compares with `codex exec` for reviews ([02](02-experiments.md)).

## Non-goals

- **An LLM gateway that lets Claude Code's own harness drive a non-Claude
  model.** Claude Code's documentation says routing it to non-Claude models
  through a gateway is not supported, and every Opus request would have to
  pass through craze. Rejected (TD-03).
- **A TUI surface for the new commands.** Their sessions are ordinary
  sessions, so the TUI's session list already shows them. The only TUI-visible
  changes are a label and how `craze -c` picks a session (TD-04).
- **Making Claude Code's permission rules and hooks apply inside a craze
  session.** A craze session's tool calls happen in craze. The plugin
  documents this instead ([04](04-parity.md)).
- **Publishing to `cc-plugins`.** That repo's existing rescue plugins stay as
  they are; one small fix for them is recorded in [10](10-follow-ups.md).
