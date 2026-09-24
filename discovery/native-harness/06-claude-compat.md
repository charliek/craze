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

1. **Workspace**, the whole chain as one source: every
   `.claude/commands/*.md`, innermost directory first, and then every
   `.claude/skills/*/SKILL.md` and `.agents/skills/*/SKILL.md`, innermost
   first again — commands before skills across the chain, not one
   directory at a time, or a subdirectory's skill would beat the
   repository root's command of the same name. Pseudo plugin id `project`.
2. **User**, under `UserRoot`: `commands/*.md` and `skills/**/SKILL.md`,
   skipping `skills/synced/` (Claude's own app-synced skills). Pseudo id
   `user`.
3. **Claude's installed-and-enabled plugins**, as today.

`project` and `user` are reserved ids: a real plugin named either is
skipped for native, with one diagnostic line. Discovered files are
de-duplicated by `os.SameFile` before naming, across all three sources, so
a home directory that is also a chain directory does not list a skill
twice and a hard link costs one file of the budget rather than two.

Claude's vendor default skills (`pdf`, `docx`, `xlsx`, `pptx`,
`skill-creator`) are dropped from **the workspace and user sources only**,
and there only when their path holds a `.claude` segment. Plugins are not
filtered by name at all: every plugin installs under `<home>/.claude`, so
the path condition matches all of them and could only discriminate by
name — which would throw away a skill somebody deliberately installed.
Claude's own defaults do not arrive through a plugin; on this machine they
sit under the user root's `skills/synced/`, which the scan skips whole.

Symlinks are skipped **within** a root — a linked `.claude/commands`, a
linked `SKILL.md`, a linked skill directory — but a root the user
configured is followed: `UserRoot` itself may be a symlink, as a dotfiles
`~/.claude` usually is, exactly as cursor follows a `--plugin-dir` pointed
at a link while refusing links inside it. `.gitignore` is never consulted.

## The chain

`filepath.EvalSymlinks(workspace)`, then walk up to the nearest ancestor
holding a `.git` entry of any kind — file, directory, or symlink —
inclusive. If none is found within 16 levels or before the filesystem
root, the chain is the workspace alone. A submodule or a worktree stops at
its own `.git`, so a superproject's files are not loaded. A workspace
`EvalSymlinks` cannot resolve — it is not there, a component is
unreadable, a link loops — is the cleaned spelling alone and is **not**
walked: a path that resolved to nothing has no ancestors whose
instructions craze may put in front of the model.

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

## Personas (H6)

**Status: planned (Plan 026), not built.** Discovery follows the same seam
as skills and commands (`contentSources` / `resolveNativeSources`, D-45), in
a new `PluginKindAgent`, from three sources, first wins:

1. the workspace chain's `.claude/agents/*.md`, innermost first (layout
   field `Agents`);
2. `UserRoot`'s `agents/*.md` (`UserAgents`);
3. an installed-and-enabled plugin's `agents/*.md`, read in `rootEntries`
   after its commands and skills, named `plugin:name`.

Precedence is grok's: **project > built-in > user > plugin**. There are
three built-ins, owned by the harness rather than discovery:
`general-purpose` (every child tool, the default `subagent_type`),
`explore` (`read`, `grep`, `glob`, read-only search), and `plan` (the same
three tools, a read-only architect). A user persona named like a built-in
is skipped with one diagnostic; plugin names are qualified (`plugin:name`)
and cannot collide.

**A namespace of their own**, not `d.out` (the menu/catalog projection that
also feeds cursor's discovery). Personas share discovery's file-reading
protections — confinement, symlink rules, the byte caps — but not its
dedupe or its projections: they are keyed `agent:<name>` in a set of their
own, so a command, a skill, and an agent of the same name coexist; they
never resolve through the slash-menu name resolver and never reach the
frozen catalog, so adding one leaves the menu and the prompt bytes
unchanged. They have their own file budget, 128 files and 1 MiB, separate
from plugins' skill/command budget (256 files), so the persona files
installed on a machine cannot crowd out a later plugin's skills.

**Parsing** is an agent-only frontmatter reader (sharing the shared
line-splitter skills already use) for `tools`, `disallowedTools` (both
spellings), `model`, and `effort`. `tools` and `disallowedTools` take three
syntaxes, all found among installed plugins: a comma string
(`tools: Read, Glob`), a flow sequence (`tools: ["Read", "Grep"]`), or a
block list of `- Read` lines — the split is parenthesis-aware, since
`Agent(a, b, c)` has commas inside a single item. **It fails closed**: a
`tools:` key that is present but parses to nothing gives a text-only child
plus a diagnostic; the absence of `tools:` is the only way to get every
child tool (an explicit `AllTools bool`, never a nil slice). The body,
capped at 32 KiB, is the persona's role text; `color`, `skills`,
`initialPrompt`, `permissionMode`, `maxTurns`, `isolation`, `background`,
`hooks`, and `mcpServers` are ignored, each silently.

**Tool names.** Claude's tool names map to native ids case-insensitively
(`Read→read`, `Write→write`, `Edit`/`MultiEdit`→`edit`, `Bash→bash`,
`Glob→glob`, `Grep→grep`, `LS→read`); native ids pass through verbatim;
`Agent`, `Agent(…)`, `Task`/`Task*`, `TodoWrite`, `AskUserQuestion`,
`ExitPlanMode`, `WebFetch`, `WebSearch`, `NotebookEdit`/`NotebookRead`,
`KillShell`, `BashOutput`, `Workflow`, `Skill`, and any MCP name are dropped
silently. This mapping, and the shadowing of built-in names, run in the
**adapter** at discovery time — the harness has no diagnostic channel of
its own for it (a new `Options.Warn func(string)` carries a runtime
fall-through, such as a persona model that does not resolve, back to the
adapter to journal). `disallowedTools` is subtracted after mapping.

**`[compat.claude] agents`** (default true, `ClaudeCompat.NoAgents`) turns
off all three sources at once, plugin pass included — unlike
`skills`/`commands`, which leave a plugin's own rows alone and gate only
the workspace and user ones.

## Not built in H4

`craze import claude`; lazy loading; `paths:` gating; `--plugin-dir` or
configured plugin paths for native; agents/personas (H6); the marketplace
installer; `${CLAUDE_PLUGIN_DATA}`, `$ARGUMENTS[N]`, nested command
directories, `argument-hint`, `allowed-tools`/`model`/`effort`, hooks, MCP,
`settings.json` beyond `enabledPlugins`, `@file` expansion in the user's
typed text, `skills/synced/`.

## Limits

What H4 does not do, as built, beyond the "Not built" list above:

- **Confinement's threat model is a static hostile checkout**, not a
  process racing craze on the same machine. `confinedPath` resolves and
  checks a path, then the reader `Lstat`s and opens it; an adversary who
  can swap a directory for a symlink between those two steps already has
  code execution and does not need to trick craze into reading a file.
  Closing that window needs `openat2`-style directory-handle walking with
  per-component symlink refusal, which macOS has no equivalent of, so the
  window is accepted and stated on `confinedPath` itself rather than
  closed.
- **The seam relocates `UserRoot`, not the plugin paths.** Moving
  `UserRoot` to an imported tree or a configured location is purely a
  `resolveNativeSources` change, which is what the relocation test covers.
  Moving where plugins are read from is not: `scanClaudePlugins` builds
  its `.claude` locations from `Home` directly, so pointing native's
  plugin discovery elsewhere needs a change to that shared, cursor-owned
  code, out of scope here.
- **A project-scoped plugin under a symlinked or differently-cased path
  can be missed.** Native hands `scanClaudePlugins` a physical workspace,
  but the scan's own `samePath` compares it against the path an install
  recorded with a plain string comparison. On the machine this was built
  and smoked on every plugin is `scope: "user"`, so nothing is affected
  today, but a project-scoped install under a linked or differently-cased
  checkout would not be matched.
- **`readDirCapped` takes the filesystem's first 4096 entries per
  directory, then sorts.** Which files survive in a directory larger than
  that is order-dependent, and the cap applies per directory rather than
  across the whole scan. Both are cursor's pre-existing behaviour, kept
  rather than changed, since changing them would change cursor's results
  too.
- **Skill names are normalised; command names are not.** A skill file's
  name is lowercased and has whitespace folded to `-` before its
  characters are checked, so `My Skill` becomes `my-skill` and is kept. A
  command file named the same way fails the character check and is
  skipped with a diagnostic line instead. This is cursor's existing,
  tested behaviour and stays as is rather than being made symmetric,
  since changing it risks cursor's own results.
- **A command that deliberately detaches itself can outlive craze.**
  `setsid`, `disown`, or a daemon that puts itself in a session of its own
  leaves the process group craze kills, and craze cannot reach it
  afterwards. Everything else the shell command started is killed when the
  command ends and again when craze exits.
- **The pid-reuse hold is Linux-only.** The shell leader is kept unreaped
  (`waitid` with `WNOWAIT`) until after its group has been sent its last
  signal, so that signal cannot land on a stranger's group given the same
  pid in between. Where that wait is unavailable — macOS, or a Linux
  `waitid` that fails — the leader is reaped first and the signal follows,
  leaving the window `internal/acp/spawn.go`'s group shutdown and opencode
  already accept, narrowed by the group having to be empty first (a live
  member keeps the pid taken).
- **Shell mode is not a terminal.** No PTY, no interactive programs, no job
  control, no history.

## Explicit non-goals

`~/.claude/settings.json` (permissions, env, defaultMode), hooks, MCP,
importing Claude Code sessions, Claude memory files.
