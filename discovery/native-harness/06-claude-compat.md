# 06 — Claude compatibility

Owner priority: **CLAUDE.md, Claude skills, and marketplace plugins must
work.** grok-build's compat layer is the reference for behaviour (`01`).
Settings, hooks, and MCP stay non-goals. This document now describes what
H4 (Plan 022) actually builds.

## One seam, everything read live

The two classes of content this document used to describe — live workspace
content and an imported home directory — collapse into one: **everything is
read live**, project content from the workspace chain and user content from
`UserRoot` (today `<home>/.claude`), behind `contentSources`
(`internal/agent/sources.go`, plan §3.1). D-45 amends D-13 ("import, not
live"): `craze import claude` and an imported tree under `~/.craze/native/`
are not built in H4. The owner may move the loading locations later, to an
imported tree or to configured paths, so every location is resolved in
`contentSources` alone and nothing downstream knows where content came
from. An imported tree stays reachable through that same seam — D-45 amends
D-13's method, not its destination.

## Sources and order

Discovery walks three sources, first wins on the dedupe key; within every
source, commands are listed before skills:

1. **Workspace**, each directory of the chain innermost first:
   `.claude/commands/*.md`, `.claude/skills/*/SKILL.md`,
   `.agents/skills/*/SKILL.md`. Pseudo plugin id `project`.
2. **User**, under `UserRoot`: `commands/*.md` and `skills/**/SKILL.md`,
   skipping `skills/synced/` (Claude's own app-synced skills). Pseudo id
   `user`.
3. **Claude's installed-and-enabled plugins**, as today.

`project` and `user` are reserved ids: a real plugin named either is
skipped for native, with one diagnostic line. Discovered files are
de-duplicated by `os.SameFile` before naming, so a home directory that is
also a chain directory does not list a skill twice. Claude's vendor default
skills (`pdf`, `docx`, `xlsx`, `pptx`, `skill-creator`) are dropped when
their path holds a `.claude` segment. Symlinked roots and entries are
skipped; `.gitignore` is never consulted.

## The chain

`filepath.EvalSymlinks(workspace)`, then walk up to the nearest ancestor
holding a `.git` entry of any kind — file, directory, or symlink —
inclusive. If none is found within 16 levels or before the filesystem
root, the chain is the workspace alone. A submodule or a worktree stops at
its own `.git`, so a superproject's files are not loaded.

## Instruction files

Loaded by `internal/agent/instructions.go`:

1. Under `UserRoot`: `CLAUDE.md`, then `rules/*.md` alphabetically.
2. For each chain directory, outermost first: `AGENTS.override.md` in
   place of `AGENTS.md` when both exist, else `AGENTS.md`; then
   `CLAUDE.md`, `CLAUDE.local.md`, `.claude/CLAUDE.md`,
   `.claude/CLAUDE.local.md`; then `.claude/rules/*.md` alphabetically.

Symlinked instruction files are followed — `CLAUDE.md -> AGENTS.md` is
common in the owner's checkouts — and every file is de-duplicated by
`os.SameFile`. Rules have their frontmatter stripped; a rule carrying a
`paths:` key is skipped rather than loaded ungated (see "Not built in H4").

## `@path` imports

A line at column 0 that matches `^@(\S+)$`, outside a fence, is replaced by
the target's text with frontmatter kept. A fence opens on a line of up to
three leading spaces then three or more backticks or tildes, and closes on
a line starting with at least as many of the same character. Imports are
relative to the importing file, capped at depth 5; an already-loaded file
is not loaded again; a missing, non-regular, or non-UTF-8 target leaves the
line as it is. Inline imports (`see @README.md`) and paths with spaces are
not supported.

## Confinement

This is a hard rule, not a nicety: craze reads these files itself and sends
them to a remote provider before the first turn, where no tool gate will
ever see them. A file reached from a chain document — directly, through a
symlink, or by import — must resolve, after `EvalSymlinks`, under the
chain's outermost root; one reached from a user-root document must resolve
under `UserRoot`. `~/` is honoured only in a user-root document. A
violation — `@~/.ssh/id_rsa`, `@../../.env`, a `CLAUDE.md` symlinked to a
credentials file — is not read: an import line stays literal, a file is
skipped, each with one diagnostic line.

## The prompt

Instructions and the catalog go into the frozen system prompt (D-30 as
amended by D-45), rendered defensively by the harness because it cannot
trust its caller's text: line endings are normalised, every catalog field
and path is folded to one line, a row whose path is not absolute and clean
is dropped, and a heading-forgery line inside instruction text is escaped.
Budgets: 32 KiB per top-level instruction file including what it imports,
96 KiB for all of them; 24 KiB for the catalog with 400 bytes per row,
degrading to names and paths and then dropping rows with "… and N more". A
diagnostic fires above 64 KiB of total prompt.

The catalog's preamble defines `$ARGUMENTS`, `${CLAUDE_PLUGIN_ROOT}` (the
row's `Root:`), and what `` !`cmd` `` means (D-46).

## Expansion

A typed `/name` expands into the prompt for commands and skills alike, with
`$ARGUMENTS`, one-based `$1..$99`, `${CLAUDE_PLUGIN_ROOT}`,
`${CLAUDE_SKILL_DIR}`, and `${CLAUDE_SESSION_ID}`. With no argument token
in the body and arguments given, `**ARGUMENTS:** <args>` is appended. A
reference expands only as the first token of a line. Interjections expand
too. Caps: 8 blocks, 64 KiB per body, 128 KiB per prompt. Hidden entries
(`user-invocable: false`) take part in naming, are absent from the menu,
and do not expand when typed — but they are in the catalog;
`disable-model-invocation` entries are in the menu and not in the catalog.

## Toggles

`[compat.claude]` in craze's own `config.toml`, read by the existing
reader in `internal/tui/config.go`: `instructions`, `rules`, `skills`,
`commands`, `plugins`, each defaulting to true. A value that is not a
boolean is the default plus one diagnostic line.

| toggle | menu | catalog | prompt instructions |
|---|---|---|---|
| `instructions` | — | — | every instruction file |
| `rules` | — | — | `rules/*.md` only |
| `skills` | project and user skills | the same | — |
| `commands` | project and user commands | the same | — |
| `plugins` | every plugin row | the same | — |

## Not built in H4

`craze import claude`; lazy loading; `paths:` gating; `--plugin-dir` or
configured plugin paths for native; agents/personas (H6); the marketplace
installer; `${CLAUDE_PLUGIN_DATA}`, `$ARGUMENTS[N]`, nested command
directories, `argument-hint`, `allowed-tools`/`model`/`effort`, hooks, MCP,
`settings.json` beyond `enabledPlugins`, `@file` expansion in the user's
typed text, `skills/synced/`.

## Explicit non-goals

`~/.claude/settings.json` (permissions, env, defaultMode), hooks, MCP,
importing Claude Code sessions, Claude memory files.
