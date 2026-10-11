# 06 — The Claude Code plugin, inline in craze

## Convention (shed and prox)

Both sibling repos ship their Claude Code plugin from the repo root:

- `.claude-plugin/plugin.json`: `name`, `version`, `description`, `author`,
  `homepage`, `repository`, `license`, `keywords`
  (`prox/.claude-plugin/plugin.json:1-13`).
- `.claude-plugin/marketplace.json`: one plugin, `"source": "./"`
  (`prox/.claude-plugin/marketplace.json:1-14`).
- `skills/<name>/SKILL.md` plus `references/*.md`. The frontmatter is `name`
  and a long `description` saying when to use the skill and when not to; no
  `allowed-tools` (`prox/skills/prox/SKILL.md:1-4`).
- Install: `/plugin marketplace add charliek/prox`, then `/plugin install
  prox@prox` (`prox/README.md:72-76`).
- The release bumps `plugin.json`'s version with the binary's.
  `scripts/release/update-version.sh` delegates to `scripts/set-version.sh`,
  then reads the value back with `jq` (`prox/scripts/release/update-version.sh:43-57`).
  prox's release workflow also checks the tag against `plugin.json`
  (`prox/.github/workflows/release.yaml:53-66`).
- shed validates the plugin in CI (`shed/scripts/validate-plugin.sh`: both
  JSON files parse, and every skill has `name` and `description`). prox does
  not.
- Neither ships `agents/` or `commands/` yet. craze would be the first with
  sub-agent definitions.

craze's root has no `skills/`, `agents/` or `.claude-plugin/`. Its own
working skill lives at `.claude/skills/craze-live-smoke/` and stays there, as
shed keeps its repo-local `.claude/skills/` apart from the plugin's `skills/`
(`shed/CLAUDE.md:171`). `AGENTS.md` is a file and does not collide with an
`agents/` directory.

Two craze-specific constraints:

- `internal/paths/paths_repo_test.go:42-80` walks the repo and fails on the
  removed config-directory environment variable's name outside a short
  allow-list. Plugin text must say `CRAZE_HOME`.
- craze's `scripts/release/update-version.sh` bumps only
  `internal/version/version.go`. It needs the same `plugin.json` step and
  read-back as prox, and a line in `RELEASING.md`'s list of version files.

## Layout

```
.claude-plugin/plugin.json        name "craze", version = craze's version
.claude-plugin/marketplace.json   source "./"
agents/delegate.md                the general sub-agent: names its target
agents/<preset>.md                a few presets (below)
skills/craze/SKILL.md             how to use craze from an agent: the hub commands,
                                  the result document, follow-ups, fan-out, attach
skills/craze/references/*.md      the commands and the result format in full
```

Installed as `/plugin marketplace add charliek/craze`, then `/plugin install
craze@craze`. Agents appear as `craze:delegate`, `craze:sol` and so on.

## The sub-agents

Every agent file is a thin wrapper:

```yaml
---
name: sol
description: <when to use; proactive routing guidance>
tools: Bash, Write
model: haiku
effort: high
---
```

- **Every Bash call sets `timeout: 600000`.** The Bash tool's *default*
  timeout is 120 s; 600 s is its maximum. `craze`'s own `--timeout 9m` stays
  under it, and the result gives a handle to keep waiting.

- **`model: haiku` is required, not optional.** With no `model` field a
  sub-agent runs on the parent's model ([02](02-experiments.md)), so the
  wrapper would run on Opus.
- **`effort: high`** is the owner's setting for the wrapper. The wrapper does
  no reasoning about the task; effort buys faithful forwarding.
- **The body tells the wrapper to forward and return.** One Bash call to
  `craze new --wait --json` (or `craze send <id> --wait --json` on a
  follow-up), then return the final message and the footer exactly. On
  failure it reports stderr, never substitutes its own answer, and never
  retries on another model.
- **The prompt goes in through a file** (`--prompt-file`), written by the
  wrapper's own Bash call, not a heredoc, so a worktree-pinned session's
  refusal of heredocs and `$(…)` cannot break it. Whether the wrapper can
  write the file without such constructs is TQ-6.
- **The general agent, `craze:delegate`,** takes a first line naming its
  target (`target: native chatgpt/gpt-6.1-sol effort=high mode=plan`), as the
  prototype did. Each preset pins one target, so the orchestrator picks by
  name and cannot mistype a model id.
- **Presets** (the plan pins the list; TQ-5):
  - `craze:sol`: native `chatgpt/gpt-6.1-sol`. Rescue, second opinions,
    implementation.
  - `craze:astra`: native `chatgpt/gpt-6-astra`. Hard diagnosis.
  - `craze:muse`: native `muse-spark-1.3-contributor`. Cheap experiments.
  - `craze:grok`: grok `grok-4.7`.
  - `craze:cursor`: cursor with its default model.
- **Continuing** a sub-agent is `SendMessage` to it. The wrapper finds the
  session id in its previous output and runs `craze send` against it.

## The skill

`skills/craze/SKILL.md` serves two readers:

- **The wrappers**, which it teaches the commands and the result document so
  each agent file can stay short.
- **An orchestrator that wants no wrapper**, for example to fan the same
  prompt out to three models in parallel. It runs `craze new --wait` itself
  as a background Bash call and reads the result document. This costs no
  extra model hop, but the orchestrator reads the result in its own context.

## What the plugin does not do

- It does not route Claude Code's own permission prompts or hooks into
  craze. A craze session runs in yolo mode unless created with
  `--no-force`; read-only means plan or ask mode ([04](04-parity.md)).
- It does not replace `cc-plugins`' rescue plugins. They go on working;
  whether craze replaces `codex exec` there is TQ-3.
