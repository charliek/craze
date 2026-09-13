# CLI Reference

```bash
craze [flags]
craze prompt [text] [flags]
craze version
```

The default command is the [interactive TUI](tui.md). It refuses to start on a
non-tty.

## Flags

These flags apply to `craze` (the TUI). `craze prompt` shares the session
flags; see [craze prompt](#craze-prompt).

| Flag | Description |
|------|-------------|
| `--workspace` | Existing workspace directory (default: current directory) |
| `--model` | ACP model id |
| `--agent-bin` | Path to the agent binary (or `CRAZE_AGENT_BIN`) |
| `--provider` | ACP provider: `cursor` or `grok`. Empty is unset. Unknown id exits 2 |
| `--force` | Spawn the agent with `--force` / `--always-approve` (yolo). Default: on |
| `--no-force` | Disable yolo and handle permission requests |
| `--no-mouse` | Disable mouse reporting (wheel scroll and clicks) |
| `--theme` | TUI theme preset. See [Configuration](configuration.md) |
| `--ask` | Set session mode to ask after `session/new` |
| `--plan` | Set session mode to plan after `session/new` |

`--ask` and `--plan` are mutually exclusive.

`--provider` on the TUI skips the startup picker. Without it, `$CRAZE_PROVIDER`
then `provider` in the config file then `cursor` is the default, and the picker
lets you change it before Start. `craze prompt` has no picker; it uses the same
precedence. `craze frame` ignores env and config and defaults to cursor unless
`--provider` is passed.

```bash
./bin/craze
./bin/craze --provider grok
./bin/craze --workspace ../proj
./bin/craze --no-force
./bin/craze --theme gruvbox --no-mouse
```

## craze prompt

Run a headless ACP turn (and optional follow-ups). Text comes from the
argument, or from stdin when it is not a tty.

```bash
./bin/craze prompt --json --provider grok --agent-bin ./bin/craze-fake-agent "hello"
echo "hello" | ./bin/craze prompt --json
./bin/craze prompt --json --follow-up "and then this" "start here"
```

| Flag | Description |
|------|-------------|
| `--workspace` | Existing workspace directory (default: current directory) |
| `--model` | ACP model id (`session/set_model` after `session/new`) |
| `--agent-bin` | Path to the agent binary (or `CRAZE_AGENT_BIN`) |
| `--provider` | ACP provider: `cursor` or `grok` |
| `--follow-up` | Additional prompt on the same ACP session (repeatable) |
| `--permission-decision` | Headless permission answer: `allow-once` or `reject-once` (repeatable) |
| `--force` | Spawn the agent with `--force` (yolo). Default: on |
| `--no-force` | Disable yolo and handle permission requests |
| `--ask` | Set session mode to ask after `session/new` |
| `--plan` | Set session mode to plan after `session/new` |
| `--json` | Write only JSON events to stdout |

Without `--json`, only assistant text is written to stdout.

## craze version

Print the craze version and exit.

```bash
./bin/craze version
```

## Exit codes

| Code | When |
|------|------|
| 0 | Session started; quitting after a usable session, including an error *during* the session |
| 1 | Session never started (no login, agent would not come up), or a runtime failure |
| 2 | Usage error (bad flags, missing prompt text, non-tty TUI) |

`craze frame` uses 2 for a bad key script and 3 for a wait timeout; that
command is hidden. See [Testing](../development/testing.md#craze-frame).
