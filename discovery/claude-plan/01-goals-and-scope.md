# 01: Goals and scope

## The idea

The goal is a craze session whose model is Claude, paid for by the user's Claude
subscription rather than an API key. It would carry the same chrome every other
provider has:
- streaming text and thinking;
- tool rows;
- permission, question and plan cards;
- todos;
- sub-agent rows;
- modes;
- `/model` and effort;
- resume;
- the detached host, `craze attach`;
- `craze new --wait` for unattended runs.

The owner's draw is the plan's rate limits: Claude Opus/Fable-class models at
a flat price, next to the ChatGPT plan that native already uses (D-81).

## What "Claude provider" means here

A provider in craze is one `agent.Provider` value plus a session behind
`agent.Session` (`internal/agent/provider.go:40-49`, `internal/agent/session.go:1220`).
There are two shapes today:

- **ACP agents** (cursor, grok, gx). craze spawns a binary and speaks ACP v1
  JSON-RPC over stdio (`internal/acp`, `internal/agent/live*.go`, about 9.7k
  lines without tests). Provider differences are a `DialectID` and the fields
  of `Provider`.
- **In-process** (native). craze runs the loop, tools and model clients itself
  (`internal/harness`, adapter `internal/agent/native*.go`, about 10k lines
  without tests).

A subscription-backed Claude can only be the first shape, or a third "spawned,
but not ACP" shape. It cannot be the second, because craze may not hold the
subscription credential ([02](02-policy-and-billing.md)). In every viable
design **Claude Code's own harness runs the loop and the tools**, so craze is
chrome around Claude Code, as it is around cursor-agent.

## Goals, if built

- Use the login the user made with `claude auth login`. craze stores, reads and
  forwards no Claude credential.
- Spawn the **unmodified** `claude` the user installed, or an unmodified one the
  adapter ships.
- Match grok's level of craze integration: cards, modes, models and effort,
  todos, sub-agents, resume, usage.
- Tell the user plainly at sign-in which login the session is using
  (subscription, Console/API key, cloud), from `claude auth status --json`
  (`loggedIn`, `authMethod`, `subscriptionType`), and its plan limits from
  `rate_limit_event`.

## Non-goals

- **A craze-run claude.ai OAuth flow, or reading `~/.claude/.credentials.json`
  or the keychain** for any purpose, the usage endpoints included. Anthropic
  forbids third parties to "collect, store, or intermediate" these. t3code and
  pandacode do read them for usage meters; opencode's spec calls that "not
  allowed but everyone else does it". craze will not.
- **Native harness on a subscription token.** This is the old opencode pattern
  (spoofed system prompt, Claude Code betas, Claude Code's client id). It was
  removed from opencode on legal request (`1ac1a0287c`, 2026-03-19).
- **Replacing Claude Code's tools with craze's**, as opencode's Claude Agent
  does through an MCP bridge. That would make Claude a model under craze's
  harness, not a harness of its own. It costs serial tool calls, a process
  restart per tool-set change, and a ~270 MB SDK. It also sits closer to the
  policy line. It is noted as route D in [04](04-options-and-design.md) and not
  pursued.
- **Pooling or rotating accounts** (pandacode, t3code). One user, one login.
- **Anthropic API-key Claude.** Native already does this.
