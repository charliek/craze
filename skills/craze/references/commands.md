# craze commands for delegation

Review and routing need craze 0.6.0 or newer. Delegation starts with craze 0.2.0, subject to each provider’s release floor. The full reference is `docs/reference/cli.md` in the craze repository. These are the commands any agent runs to hand work to another model (Claude Code's agents run them for it). Never start a session on your own provider: see "Don't call yourself" in `SKILL.md`.

With craze 0.2.0, a prompt is limited to 32 KiB (exit 2 above that size), a `--json` failure is `{"error":{"message":…}}` alone, a turn that stopped short has a `stopped:` footer line instead of `error:`, `stop` on a session that has ended is exit 1, and `cancel --turn` does not exist. `craze version` says which you have.

## Start a task: `craze new --wait`

```bash
craze new --wait --timeout 9m --max-message 20000 --provider grok --model grok-4.7 --effort high --prompt-file /tmp/craze-delegate-<12 hex digits>.md --remove-prompt-file
```

Starts an unattended session in the current directory (no hub), sends it the prompt, waits for its turn, prints the result.

| Flag | Meaning |
|---|---|
| `--provider <id>` | `native`, `cursor`, `grok`, `claude` (a craze newer than 0.3.0; no `--ask`, no `--web-search`, no `--fast`), `antigravity` (agy; a craze newer than 0.4.1 — an older one exits 2 with `unknown provider "antigravity"`; no `--ask`, no `--web-search`), `codex` (Codex CLI via `codex app-server`; a craze newer than 0.5.0 — an older one exits 2 with `unknown provider "codex"`; no `--web-search`, no `--fast`). Without it `provider` in `config.toml`; none configured is exit 1. Your own provider is refused, exit 2 (below) |
| `--allow-same-provider` | Start it even on the provider the calling agent runs on, which craze new otherwise refuses. For a person who means it: an agent never adds it |
| `--model`, `--effort`, `--fast` / `--no-fast` | The start settings. On `native`, an unknown or retired `--model`, an `--effort` the model does not list (with `--model`) and `--fast` (no native model has one) fail before the session starts: exit 1, kind `config`, `error.detail.reason`. `craze models <alias> --json` shows a native model's efforts. On `codex`, an `--effort` the model does not support also fails unattended startup (exit 1, kind `config`), while interactive sessions fall back with a diagnostic; `--fast` is ignored/not offered. ACP providers (`cursor`, `grok`, `claude`, `antigravity`) ignore a setting the model does not offer |
| `--web-search` / `--no-web-search` | Native only; needs a craze newer than 0.2.0. Offer the session the web tools (`websearch`, `webfetch`; queries and fetched URLs go to Exa), or none, whatever the home says. Without either the home's own setting applies (off for an unattended run unless the owner turned it on). Use `--web-search` for information newer than a model's training or not in the repository (current docs, release notes, an error message, comparing tools), not for edits or reviews of local code or work the user called confidential. Fixed when the session is created. `--json` results carry a `webSearch` object of what was searched and fetched. Both together fail (exit 2). `--web-search` with a provider that is not native, or over a `websearch = false` in `config.toml`, fails (exit 1, kind `config`, `error.detail.reason` `web_search_not_offered`); `--no-web-search` on another provider does nothing |
| `--plan` / `--ask` | Mode. On `native`, `cursor` and `grok`, `--ask` refuses edits and the shell, and `--plan` refuses edits except its own plan file, lets the shell through, and **ends the turn at the plan** (`completed` with a `plan:` section, nothing implemented by craze). On `codex`, both `--ask` and `--plan` run in Codex's OS-enforced read-only sandbox (shell reads are allowed, workspace and plan-file writes are blocked, and `--plan` returns the plan as the result message rather than a separate `plan:` section; Codex hooks and MCP servers run outside Codex's command sandbox). On `antigravity` it is agy's own plan mode: the edit refusal is not enforced |
| `--prompt-file <path>` | The prompt: a file, or `-` for stdin. Else the words after the command, else stdin. Two sources, none, an empty prompt or one over 512 KiB (32 KiB on craze 0.2.0) is exit 2 |
| `--remove-prompt-file` | Delete the prompt file once read |
| `--timeout <d>` | Stop waiting after this long (`9m`): status `running`, exit 5, with a handle |
| `--max-message <n>` | Print at most `n` characters of the message and plan together; the whole result goes to a file named in the output (`[… cut: full result in <path>]`). Use `20000` |
| `--no-force` | Craze leaves a provider permission request open (`needs_input`). Antigravity 1.5.18 can run shell commands and edit files without sending a request. |
| `--json` | One JSON object instead of text; a failure is `{"error":{"message":…,"kind":…,"retryable":…}}`, its kind one of a closed set (failure kinds in `docs/reference/cli.md`) |
| `-C <dir>` | The directory (default: the current one) |

Questions with options are answered with the first option (listed under `auto-answered:`); on `codex`, a free-text-only question (with no options) returns `needs_input` (exit 4) even under `--force`, while permission-expansion and MCP elicitation requests are declined automatically. A plan the agent presents ends the turn and is returned, never accepted. On claude, a background sub-agent keeps the turn open until it is done, but a background shell command (`run_in_background`) is not waited for: the turn ends once it starts. Both claude and codex turns are unpriced: claude runs on the user's Claude plan, and codex runs on the user's ChatGPT or OpenAI account.

## Continue: `craze send`

```bash
craze send 175bfe79 --wait --timeout 9m --max-message 20000 --prompt-file /tmp/craze-delegate-<12 hex digits>.md --remove-prompt-file
craze send 175bfe79 "and the docs"
```

Sends the prompt to the session `<short>` (the id in the result's last line), queued behind any turn. Without `--wait` it prints `started turn-3` or `queued q-2 (position 1)` and a `next:` line, and exits 0. With `--wait` it follows **that** prompt's turn and prints its result like `craze new --wait`. A session that has ended (its host idled out or was stopped) is loaded again first, with its recorded mode, model, effort and web search choice, and keeps its context. `--max-message` needs `--wait`.

## Follow a turn: `craze wait`

```bash
craze wait 175bfe79 --from 0199c0de-1111-7000-8000-397e175bfe79.41.turn-3 --timeout 9m --max-message 20000
craze wait 175bfe79
```

`--from <handle>` follows exactly the turn or queued prompt the handle names, even one that has ended since. Plain `craze wait <short>` follows whatever turn is running (or reports the last one) and never touches the session; its result is partial.

## Cancel and stop

```bash
craze cancel 175bfe79
craze stop 175bfe79
```

`cancel` ends the running turn (the session and its queue go on); `nothing to cancel` is exit 0. `cancel --turn <turn>` (a result's `turn`, `turn-3`) cancels only that turn, and only while it is the current one, so a cancel sent again never reaches the next queued turn. `stop` ends the session (it stays in the index and can be sent to again); `stop` on a session that has ended is exit 0. Both take `--json`.

## The result

```text
The flaky test raced the cache's eviction; I made eviction wait for readers.
---
completed · 27s · 5 tool calls · $0.04 · context 16.6k/272k
files: stats/core.py
session 175bfe79 · craze attach --session 175bfe79
```

The message is the turn's last assistant message (`(no final message)` if none). A presented plan follows under `plan:`. The footer has the status, time, tool calls and cost; then, when there is one, `usage:` (a step was in flight, its spend not counted), `error:` (why it did not complete, a turn that stopped short included), `files:`, `outside the workspace:`, `plan file:`, `auto-answered:`, `ask:`, `blocked by:`, the session line, `session stopping (--exit-after-turn)`, and `next:` (the exact command to run next for `running` and `needs_input`).

## Statuses and exit codes

| Exit | Status | Meaning |
|---|---|---|
| 0 | `completed`, `idle` | The turn ended normally; `idle`: the session has had no turn |
| 1 | none | craze failed (no such session, session ended, unreachable, lost connection, a session that could not start, `--timeout` before the prompt was sent, a prompt that may not have arrived, a `send` over 32 KiB to a busy session). The message is on stderr; with `--json`, `error.kind` says which: `stopped` (the session ended), `connection_lost`, `host_exited`, `unobserved`, `start_failed`, `prompt` (the busy-session refusal) and others |
| 2 | none | Usage error. `craze: this agent already runs on <provider>; use your own sub-agent tool for <provider> work, not craze (--allow-same-provider overrides)` is the self-delegation refusal: use your own sub-agent tool, and do not retry. An unknown flag or command means the craze is too old (needs 0.2.0 or newer); when the command included `--web-search` or `--no-web-search`, it means this craze does not support web search (a craze newer than 0.2.0): run again without it |
| 3 | `failed`, `cancelled`, `dropped` | The turn ended without completing, or the queued prompt was removed |
| 4 | `needs_input` | An ask is open that nothing here answers; answer it in `craze attach`, or `craze cancel` / `craze stop` |
| 5 | `running` | `--timeout` passed; the turn goes on. Run the `next:` command |

## Handles

A `running` or `needs_input` result (and a no-wait `craze send`) prints a handle, `<incarnation>.<seq>.<target>`: for example `0199c0de-1111-7000-8000-397e175bfe79.41.turn-3` or `….41.row-q-2`. Pass it to `craze wait <short> --from <handle>`. A handle is ownership: a signal (Ctrl+C, SIGTERM) while `--from` waits cancels that turn, or removes that queued prompt, and nothing else. Plain `craze wait` never cancels.

## Watch it

`craze attach --session <short>` opens the session's TUI. `craze ps` lists sessions. A session a delegation started is marked unattended in the session index: `craze -c` skips it, and the resume picker and session list show it dimmed or tagged.

## Large context

Put a large context in a file in the project and name the file in the prompt; give the prompt itself through `--prompt-file`. A prompt can be up to 512 KiB (32 KiB on craze 0.2.0), but a smaller one is cheaper (a native turn sends it again with every request). A `craze send` over 32 KiB to a session that is still running a turn is refused (exit 1, kind `prompt`, nothing queued): send it once the turn has ended.

## Parallel runs

Run each `craze new --wait` as its own background shell call allowed ten minutes (Claude Code: Bash `timeout: 600000`). Each is a separate session; their results arrive as each finishes.

## Permission allow-list (Claude Code, default mode)

`Bash(craze new:*)`, `Bash(craze send:*)`, `Bash(craze wait:*)`, `Bash(craze cancel:*)`, `Edit(//tmp/craze-delegate-*)`. Claude Code applies `Edit` rules to its Write tool, and `//` marks an absolute path (a single leading `/` is relative to the settings file's own directory).


## Reviews and model routing

`craze resolve` lists targets and eligible fallbacks offline. Use
`craze resolve --target sol@6.2 --json` to inspect an explicit version or
`craze resolve --role panel --json` to select panel seats.
`craze review --role review --changes-since main --criteria correctness`
runs the review on an eligible provider. Use `--out` for an atomic, full
Markdown file and `--bundle-only` to hand the material to a read-only seat.

When replacing codex-run.py, migrate empty exit 4 to 3, stalled exit 5 to
3 and capped exit 6 to 5. Check `error.kind` for `empty` or `stalled`.
Craze exits 4 for a review that needs input.
