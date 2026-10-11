# craze commands for delegation

Needs craze 0.2.0 or newer. The full reference is `docs/reference/cli.md` in the craze repository.

## Start a task: `craze new --wait`

```bash
craze new --wait --timeout 9m --max-message 20000 --provider grok --model grok-4.7 --effort high --prompt-file /tmp/craze-delegate-<12 hex digits>.md --remove-prompt-file
```

Starts an unattended session in the current directory (no hub), sends it the prompt, waits for its turn, prints the result.

| Flag | Meaning |
|---|---|
| `--provider <id>` | `native`, `cursor`, `grok` (or `gx`). Without it `provider` in `config.toml`; none configured is exit 1 |
| `--model`, `--effort`, `--fast` / `--no-fast` | The start settings, where the model offers them |
| `--plan` / `--ask` | Mode. `--ask` refuses edits and the shell. `--plan` refuses edits except its own plan file, lets the shell through, and **ends the turn at the plan** (`completed` with a `plan:` section, nothing implemented) |
| `--prompt-file <path>` | The prompt: a file, or `-` for stdin. Else the words after the command, else stdin. Two sources, none, an empty prompt or one over 32 KiB is exit 2 |
| `--remove-prompt-file` | Delete the prompt file once read |
| `--timeout <d>` | Stop waiting after this long (`9m`): status `running`, exit 5, with a handle |
| `--max-message <n>` | Print at most `n` characters of the message and plan together; the whole result goes to a file named in the output (`[… cut: full result in <path>]`). Use `20000` |
| `--no-force` | Permissions are asked, not bypassed: an ask stays open (`needs_input`) |
| `--json` | One JSON object instead of text; a failure is `{"error":{"message":…}}` |
| `-C <dir>` | The directory (default: the current one) |

Questions the agent asks are answered with the first option (listed under `auto-answered:`); a plan it presents ends the turn and is returned, never accepted.

## Continue: `craze send`

```bash
craze send 175bfe79 --wait --timeout 9m --max-message 20000 --prompt-file /tmp/craze-delegate-<12 hex digits>.md --remove-prompt-file
craze send 175bfe79 "and the docs"
```

Sends the prompt to the session `<short>` (the id in the result's last line), queued behind any turn. Without `--wait` it prints `started turn-3` or `queued q-2 (position 1)` and a `next:` line, and exits 0. With `--wait` it follows **that** prompt's turn and prints its result like `craze new --wait`. A session that has ended (its host idled out or was stopped) is loaded again first, with its recorded mode, model and effort, and keeps its context. `--max-message` needs `--wait`.

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

`cancel` ends the running turn (the session and its queue go on); `nothing to cancel` is exit 0. `stop` ends the session (it stays in the index and can be sent to again).

## The result

```text
The flaky test raced the cache's eviction; I made eviction wait for readers.
---
completed · 27s · 5 tool calls · $0.04 · context 16.6k/272k
files: stats/core.py
session 175bfe79 · craze attach --session 175bfe79
```

The message is the turn's last assistant message (`(no final message)` if none). A presented plan follows under `plan:`. The footer has the status, time, tool calls and cost; then, when there is one, `error:`, `stopped:`, `files:`, `auto-answered:`, `ask:`, `blocked by:`, the session line, and `next:` (the exact command to run next for `running` and `needs_input`).

## Statuses and exit codes

| Exit | Status | Meaning |
|---|---|---|
| 0 | `completed`, `idle` | The turn ended normally; `idle`: the session has had no turn |
| 1 | none | craze failed (no such session, session ended, unreachable, lost connection, a session that could not start, `--timeout` before the prompt was sent, a prompt that may not have arrived). The message is on stderr |
| 2 | none | Usage error. An unknown flag or command means the craze is too old (needs 0.2.0 or newer) |
| 3 | `failed`, `cancelled`, `dropped` | The turn ended without completing, or the queued prompt was removed |
| 4 | `needs_input` | An ask is open that nothing here answers; answer it in `craze attach`, or `craze cancel` / `craze stop` |
| 5 | `running` | `--timeout` passed; the turn goes on. Run the `next:` command |

## Handles

A `running` or `needs_input` result (and a no-wait `craze send`) prints a handle, `<incarnation>.<seq>.<target>`: for example `0199c0de-1111-7000-8000-397e175bfe79.41.turn-3` or `….41.row-q-2`. Pass it to `craze wait <short> --from <handle>`. A handle is ownership: a signal (Ctrl+C, SIGTERM) while `--from` waits cancels that turn, or removes that queued prompt, and nothing else. Plain `craze wait` never cancels.

## Watch it

`craze attach --session <short>` opens the session's TUI. `craze ps` lists sessions. A session a delegation started is marked unattended in the session index: `craze -c` skips it, and the resume picker and session list show it dimmed or tagged.

## Large context

Put a large context in a file in the project and name the file in the prompt; give the prompt itself through `--prompt-file`. A prompt over 32 KiB is refused.

## Parallel runs

Run each `craze new --wait` as its own background Bash call with `timeout: 600000`. Each is a separate session; their results arrive as each finishes.

## Permission allow-list (default mode)

`Bash(craze new:*)`, `Bash(craze send:*)`, `Bash(craze wait:*)`, `Bash(craze cancel:*)`, `Edit(//tmp/craze-delegate-*)`. Claude Code applies `Edit` rules to its Write tool, and `//` marks an absolute path (a single leading `/` is relative to the settings file's own directory).
