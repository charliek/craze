# 02 — Experiments (2026-10-10)

Everything here ran live on Linux against craze 0.1.0 (`/usr/local/bin/craze`)
and Claude Code 2.1.296, in throwaway directories. Paid API spend was a few
cents (Muse); the rest ran on the ChatGPT, cursor and grok plans. Every
session started was stopped afterwards.

## The fixture

A small Python package, `stats/core.py`, with two bugs: `median` returns the
upper middle value for even-length input, and `percentile` indexes past the
end at `p=100`. `test_core.py` has five `unittest` tests; one fails and one
errors. Every run got its own copy and the same task: fix the bugs without
editing the tests, run the tests, summarise each fix.

## E1 — Four providers through `craze prompt --json`

Run in parallel, one copy each:

| provider | model | seconds | result |
|---|---|---|---|
| cursor | `composer-2.5` | 25.1 | both bugs fixed, 5/5 pass |
| native | `muse-spark-1.3-contributor` | 40.1 | both bugs fixed, 5/5 pass |
| native | `chatgpt/gpt-6.1-sol` | 51.3 | both bugs fixed, 5/5 pass |
| grok | `grok-4.7` | 55.4 | both bugs fixed, 5/5 pass |

Every diff was `stats/core.py | 4 ++--`, checked by hand.

Two findings for a result format:

- `text` events concatenate the agent's narration across tool calls ("I'll
  run the suite…", "Two bugs identified…", then the summary). The final
  message has to be cut out: the main session's text after its last tool
  call.
- `craze prompt --json` prints no usage. A usage-only state delta prints no
  JSON line (`docs/reference/protocol.md:1331`), so even native's exact cost
  never reaches stdout.

## E2 — A persistent session through the hub

A throwaway protocol-1 client, speaking through `craze bridge`:

1. `craze bridge --hub`: `hello`, then `session.create` with `cwd`,
   `provider: native`, `model: muse-spark-1.3-contributor` and no prompt.
2. `craze bridge --session <id>`: `hello`, `session.attach`, then
   `session.prompt` with `mode: queue`. The reply names the turn
   (`{"turn":"turn-1","text":…}`).
3. Read `event` notifications until `{"type":"turn","turn":{"id":"turn-1",
   "phase":"ended","stopReason":"end_turn"}}`. A `meta` event after every
   step carries `state.usage`, with exact `costPicoUsd`.
4. `session.state`, then `session.detach`.

Turn 1 fixed only the median bug, as asked. Turn 2, on a fresh connection,
fixed the percentile bug and correctly stated what turn 1 had done. The two
turns cost $0.0067 in total, with a 15,396-token context.

Facts learned:

- The turn's start and end are explicit events, so "wait for the answer" is
  well defined.
- `craze bridge` does not exit when stdin closes while attached: it
  half-closes and keeps reading (`docs/reference/cli.md:568`), and the host
  does not close the connection. A client must close it itself.
- `session.stop` needs the full craze session id. `craze bridge --session`
  accepts `craze ps`'s short ids, `session.stop` does not.
- A session created through the hub gets a session-index row once it is
  prompted, like any interactive session. The six hub sessions in these
  experiments left six rows in `~/.craze/sessions.jsonl`. The four `craze
  prompt` runs left none: `craze prompt` passes no index
  (`internal/cli/prompt.go:240`). See the `craze -c` problem in
  [03](03-design.md).

## E3 — A runner, and a Haiku wrapper sub-agent

`craze-task.py` (prototype, about 250 lines of Python) packaged E2:

- `start --provider P --model M [--effort E] [--mode plan|ask] --cwd D` reads
  the prompt on stdin, creates the session, runs the turn and prints the
  final message and a footer;
- `send --session ID` runs a follow-up turn, `wait --session ID` keeps
  waiting on a turn that outlived the wait limit, `stop --session ID` ends
  the session;
- open asks are answered automatically: questions skipped, plans accepted,
  permissions allowed once;
- every event is appended to `~/.cache/craze-task/<session>.jsonl`.

The footer: status, seconds, tool calls, session cost and context (native
only), and the session id with the `craze attach` command. A smoke run on
Muse took 42 s, 8 tool calls, $0.0045.

The sub-agent definition (`model: haiku`, `effort: high`, `tools: Bash`)
tells the wrapper to forward the task verbatim through one Bash call and
return stdout unchanged. A first-line `target: <provider> <model>
[effort=…] [mode=…]` routes it. On a follow-up it runs `send` against the
session id in its previous output.

**Haiku 5.5 at high effort, from this Opus session, to native
`chatgpt/gpt-6.1-sol` at medium effort.** The task: add a `mode()` function
with tests, and fix the failing ones.

- One Bash call. The output was returned exactly as printed (checked
  against the diff and the event log).
- The sol turn took 73 s, the whole sub-agent 79 s: about 6 s of wrapper
  overhead. The wrapper used about 31k tokens, mostly cached.
- The suite went from 5 tests to 12, all passing.

**`SendMessage` to the same wrapper**: make `mode()` accept a one-shot
generator, and first state the previous turn's tie rule and test count. The
wrapper ran `send` on the same session. The answer stated the tie rule and
"12 tests" correctly, and the suite reached 13 (27 s, 5 tool calls).

## E4 — End to end through a real plugin

The prototype as a plugin (`.claude-plugin/plugin.json`, `agents/craze-task.md`,
`scripts/craze-task.py`), loaded by a headless Claude Code:

```
claude -p --model opus --plugin-dir <proto> --permission-mode bypassPermissions \
  --output-format stream-json --verbose "… delegate the fix to craze:craze-task with 'target: grok grok-4.7' …"
```

- The plugin agent loaded as `craze:craze-task` and ran on `claude-haiku-5-5`.
- Opus dispatched it with the `Agent` tool. The grok turn took 25 s and 7
  tool calls; the whole run took 52 s.
- Opus then checked the work itself with `git diff` and the tests, agreed
  with it, and added a finding of its own (`percentile([])` raises
  `IndexError`).
- Cost at list prices: $0.196 in total, $0.0014 of it the Haiku wrapper.

## E5 — `codex exec` against craze native, same model, read-only review

Both reviewers ran `gpt-6.1-sol` at high effort, each in its own clone, with
the same prompt and the same diff file: `codex exec -s read-only` against
`craze-task.py start --provider native --model chatgpt/gpt-6.1-sol --effort
high --mode plan`.

| target | codex | craze native |
|---|---|---|
| `e050fcf` as merged | "no findings", 39 s, 43,760 tokens | "no findings", 59 s, 17 tool calls, 51,810-token context |
| `e050fcf` with a planted bug | found it (medium, the right failing scenario), 35 s, 22,048 tokens | found it, nearly the same words, 53 s, 8 tool calls, 36,641-token context |

The planted bug: in `reconcileProviderRows`, read the cursor's provider name
after the rows are replaced instead of before, so the cursor follows an index
into the new list instead of its provider.

Reading: the same model found the same thing. codex was about a third faster
and used fewer tokens, and its read-only mode is an OS sandbox. In craze's
plan mode the gate refuses edits but lets shell commands through
(`internal/harness/tool/modegate.go:135-138`), and ask mode refuses the shell
outright (`modegate.go:52`). One sample each; `eval/` is the place for a real
comparison (TQ-3).

## Claude Code facts used here

From Claude Code's documentation (sub-agents, MCP, LLM gateway, tools
reference), checked 2026-10-10:

- A sub-agent definition's frontmatter takes `model` (an alias, a full Claude
  model id, or `inherit`), `effort`, `tools`, `background`, `isolation` and
  more. **With no `model` field, a sub-agent runs on the parent's model.**
- A plugin's sub-agents ignore `permissionMode`, `mcpServers` and `hooks`.
- `SendMessage` resumes a finished sub-agent in the background with its full
  history.
- A main-conversation MCP call still running after two minutes
  (`CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS`, default 120000) moves to the
  background and reports back with a notification. Calls made from a
  sub-agent are excluded.
- A gateway must speak Anthropic Messages, Bedrock InvokeModel or Vertex
  rawPredict. Routing Claude Code to non-Claude models through one is not
  supported.
- A Bash call's timeout defaults to 2 minutes (`BASH_DEFAULT_TIMEOUT_MS`,
  120000) and can be raised to at most 10 (`BASH_MAX_TIMEOUT_MS`, 600000). The
  prototype's wrapper asked for 600000 explicitly; a wrapper that does not is
  killed at 2 minutes.

From the owner's `cc-plugins` repo: a Claude Code session pinned to a
worktree refuses heredocs, `$(…)` and `||` guards as too complex to verify
(`plugins/planning/commands/ask-codex.md:34`). So a prompt passed on a
heredoc can fail there. A prompt file can't.
