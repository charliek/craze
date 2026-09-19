# 06 — Claude compatibility

Owner priority: **CLAUDE.md, Claude skills, and marketplace plugins must
work.** grok-build's compat layer is the reference for behaviour; pi's
package installer is the reference for the import model. Settings, hooks,
and MCP are non-goals (`01`).

## Two classes of content

| class | examples | handling |
|---|---|---|
| **Live from the workspace** | `CLAUDE.md`, `CLAUDE.local.md`, `AGENTS.md`, `.claude/rules/*.md`, `.claude/skills/**`, `.agents/skills/**`, `.claude/agents/*.md`, `.claude/commands/*.md` | read where they live, every session |
| **Imported into the harness home** | `~/.claude/CLAUDE.md`, `~/.claude/skills/**`, `~/.claude/agents/*.md`, `~/.claude/commands/*.md`, marketplace plugins from `~/.claude/plugins/` | copied by `craze import claude` into the native directory (`~/.craze/native/` by default; it moves with `CRAZE_HOME`, D-27); never read from `~/.claude` at runtime. The tree inside it is H4's to design, so every such path in this document is provisional |

## Instruction files

Load, at every directory from the repo root down to the cwd, all of:
`AGENTS.override.md` (wins for that directory, pi), `AGENTS.md`,
`CLAUDE.md`, `CLAUDE.local.md`, `.claude/CLAUDE.md`,
`.claude/CLAUDE.local.md`. Deeper files come later in the prompt so they
win on conflict. Then `~/.craze/native/instructions/*.md` first of all
(global). Files matched by `.gitignore` are skipped except `CLAUDE.local.md`.

- **`@path` imports** inside an instruction file are followed, relative to
  the file, depth-capped at 5, cycles ignored. grok does not do this; Claude
  Code does.
- **Rules directories**: `.claude/rules/*.md` at each level and
  `~/.craze/native/instructions/rules/*.md`, alphabetical. Frontmatter
  `paths:` globs gate a rule to matching files: a gated rule is loaded when
  the cwd or a file the agent touches matches. grok strips frontmatter and
  ignores `paths`; Claude Code honours it; we honour it.
- **Lazy loading**: when a tool reads, lists, or edits a directory outside
  the initial set, its instruction files are injected once, as a system
  reminder, tracked per session so compaction and replay do not re-inject.
- **Placed once, never rewritten** within a session (cache stability).

## Skills

Agent Skills spec (`SKILL.md` with frontmatter). Roots, highest priority
first: `.claude/skills` and `.agents/skills` at each level from cwd to repo
root, then `~/.craze/native/skills/` (imported), then plugin skills. Dedup by
name, first wins; a collision is qualified `plugin:name`.

- Frontmatter honoured: `name`, `description`, `when-to-use`,
  `allowed-tools`, `argument-hint`, `user-invocable`,
  `disable-model-invocation`, `model`, `effort`, `paths`. Unknown fields
  ignored; missing description → warning, not loaded.
- **No skill tool.** The prompt carries `<skill name= description= path=>`
  rows; the model reads the file with `read`. A user-invoked `/name` expands
  the body at prompt-assembly time, which craze already does client-side.
- Skill roots ignore `.gitignore` on purpose (teams ignore `.claude/`).
- Vendor-shipped default skills are denylisted (grok's list).

## Commands

Flat `commands/*.md` (project `.claude/commands`, imported, plugin) become
slash commands, filename stem as the name, `$ARGUMENTS` / `$1..$n`
substitution, `!`cmd`` shell splice deferred.

## Plugins

Source of truth is **Claude Code's own install**: `craze import claude`
reads `~/.claude/plugins/installed_plugins.json` ∩ the enabled list (craze's
`plugins.go` already does this walk), copies each plugin snapshot into
`~/.craze/native/plugins/<id>/` and records `{source, version, sha}` in a
manifest. Components consumed: `skills/`, `commands/`, `agents/`. Ignored
for now: `hooks/hooks.json`, `.mcp.json`. `CLAUDE_PLUGIN_ROOT` resolves to
the snapshot directory for skills that reference it.

Re-running the import reports added / upgraded / kept, like `gx providers
install`. A native marketplace installer (git clone of
`.claude-plugin/marketplace.json`) is deferred (`10`).

## Agents

`.claude/agents/*.md` at each level, `~/.craze/native/agents/` (imported),
plugin `agents/`; frontmatter `name`, `description`, `tools`, `model`.
Name-dedup, highest priority wins. Used as sub-agent personas (`05`).

## Toggles

`[compat.claude]` with `instructions`, `rules`, `skills`, `commands`,
`agents`, `plugins` booleans, default on. Which file holds it is H4's call:
H1's native config is only `providers.toml` and `models.toml` (D-28), and
neither is the right home for compat toggles.

## Explicit non-goals

`~/.claude/settings.json` (permissions, env, defaultMode), hooks, MCP,
importing Claude Code sessions, Claude memory files.
