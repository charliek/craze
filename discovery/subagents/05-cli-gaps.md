# 05 — Flag gaps across craze's commands

Surveyed on `origin/main` at `7016071` (2026-10-10). Flag registrations are
in `internal/cli/`. `craze prompt`'s gaps matter here because the
sub-agent work could have extended it instead of adding new commands
([03](03-design.md) says why it doesn't). The others are gaps in their own
right, and the plan decides which to close now.

## The matrix

`Y` means registered. `-` means not registered: cobra rejects it as an
unknown flag, exit 2. `refused` means registered but hidden, with an explicit
refusal.

| Flag | TUI | prompt | new | serve | attach |
|---|---|---|---|---|---|
| `--model` | Y | Y | Y | Y | refused |
| `--effort` | Y | **-** | Y | Y | - |
| `--fast` / `--no-fast` | Y | **-** | Y | Y | - |
| `--continue` / `-c` | Y | **-** | **-** | Y | refused |
| `--resume` / `-r` | Y | - | - | - | refused |
| `--load` | - | - | - | Y | - |
| `--ask` / `--plan` | Y | Y | **-** | Y | - |
| `--force` / `--no-force` | Y / Y | Y / Y | **-** / Y | Y / Y | - |
| workspace | `--workspace` | `--workspace` | `-C`/`--dir` | `--workspace` | - |
| `--json` | - | event stream | create result | - | - |
| `--plugin-dir` | Y | Y | - | Y | - |
| `--agent-bin` | Y | Y | - | Y | - |
| `--provider` | Y | Y | Y | Y | refused |

Where they live:

- The TUI and `serve` share `registerSessionFlags`
  (`internal/cli/sessionflags.go:35-53`). `serve` adds `--load` and `--log`
  (`serve_cmd.go:204-206`).
- `prompt` registers its own set (`prompt.go:133-144`). It never calls
  `registerSessionFlags`, which is why it lacks `--effort` and `--fast`.
- `new`'s flags are at `new.go:83-90`. `attach`'s refusals are at
  `attach.go:63` and `attach.go:98-100`.

## Gaps

**G1 — `craze prompt` has no `--effort` or `--fast`/`--no-fast`.**
`docs/reference/cli.md:23-25` states the omission, and no comment in
`prompt.go` gives a reason. `prompt` builds its session on its own path
(`prompt.go`, `resolveProvider`) rather than through `tuiFlags.settle`, so
adding them means wiring the start settings into that path. The setting
itself, applied inside the start, already exists for the TUI and hosts.
`eval/` drives every native run through `craze prompt --json`
(`eval/crazeeval/runners/craze.py:44`). It therefore cannot pin an effort
today; it gets the effort remembered for the model.

**G2 — `craze prompt` cannot continue or load a session.** No `--continue`
and no `--load`. Every `craze prompt` is a fresh session that ends with the
process. The new hub commands make a resumable headless session a different
command ([03](03-design.md)), so G2 can stay open. The cost is that `craze
prompt` stays single-session.

**G3 — `session.create` cannot start a session in plan or ask mode.**
`protocol.CreateParams` (`internal/protocol/types.go:709-718`) carries `cwd`,
`prompt`, `provider`, `model`, `effort`, `fast`, `permissionMode` and
`requestId`. The schema refuses anything else (`additionalProperties:false`
in `internal/protocol/schema/session.create.json`). The host spawn spec
already has `Ask` and `Plan` (`internal/hostspawn/spawn.go:111-125`), but the
hub's create never sets them (`internal/hub/create.go:692-694`). So `craze
new` has no `--plan`/`--ask`. A client can switch with `session.set` after
the start, but then the first prompt sent by `session.create` can run before
the switch. For a read-only sub-agent that ordering matters.

**G4 — `craze new` cannot pass plugin directories or an agent binary, and
cannot continue.** The same create path drops `PluginDirs`, `AgentBin` and
`Continue`. The agent binary is deliberate (the hub never hands a host a
launch's binary, `cli.md` under `craze new`). Plugin directories and continue
are gaps. Continue matters if a sub-agent's host has idled out and a
follow-up arrives (TQ-4).

**G5 — `craze new` has `--no-force` but no `--force`.** The default is
bypass, so `--force` would be a no-op, but every other command accepts it.
Cosmetic.

**G6 — The workspace flag is spelled two ways.** `new` takes `-C`/`--dir`;
the TUI, `serve` and `prompt` take `--workspace`. Cosmetic.

**G7 — `--json` means four different things.** An event stream on `prompt`,
the create result on `new`, the roster on `ps`, the provider list on
`providers`. Each is documented. The new commands' `--json` is a result
document, closest to `new`'s.

**G8 — `--model` is described differently.** `prompt.go:134` says "an ACP
model id (session/set_model after session/new), or a native model alias".
`sessionflags.go:39` and `new.go:85` say "or a model alias". Same behaviour,
different words.

**G9 — `session.stop` needs the full craze session id.** `craze bridge` and
`craze attach` resolve `craze ps`'s short ids (`matchSession`,
`bridge.go:178-195`, using only live hosts from `rundir.Hosts`). A command
that takes a session id should resolve it the same way before calling the
host.

## What the plan should close

- **The new commands** resolve short ids (G9). They take `--plan`/`--ask`
  themselves, because they spawn their host directly (TD-08). G3, a start mode
  on `session.create` itself, becomes TF-3.
- **G1 is cheap and helps `eval/`.** Add `--effort` and `--fast`/`--no-fast`
  to `craze prompt`, applied the way the TUI applies them.
- **G4's plugin directories** ride along only if the create change touches
  the same code; otherwise they are follow-ups ([10](10-follow-ups.md)).
- **G5–G8** are follow-ups.
