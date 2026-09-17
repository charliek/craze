# 08 — Decision log

Append-only. A reversed decision gets a new row that points at the old one.
"Source" names the reference that grounded it (see `09`).

| ID | date | decision | why | source |
|---|---|---|---|---|
| D-01 | 2026-09-15 | **Native first, ACP later.** The harness implements `agent.Session` in-process; an ACP stdio binary is a later wrapper. | The TUI is already decoupled from `internal/acp`; no dialect work, no serialization, `craze prompt --json` works unchanged. craze's client speaks two vendor dialects, so a stock-ACP agent would need a third. | craze `internal/agent/session.go`, `internal/cli/tui.go`; coder/acp-go-sdk |
| D-02 | 2026-09-15 | **Harness core is free of craze types** (`internal/harness` never imports `internal/agent`, `tui`, `acp`). | Keeps D-01's wrapper cheap and lets the core be tested with a scripted `LanguageModel`. | pi (agent vs coding-agent split), crush |
| D-03 | 2026-09-16 | **Session store is pi's format**: one JSONL file per session, entries linked by `id`/`parentId` into a tree, leaf cursor, typed entries for compaction, model and effort changes, labels. | Branching, fork, and rewind fall out of the tree; no database dependency; craze already consumes replayed update streams. Replaces the earlier two-rail (grok) proposal. | pi `session-manager.ts`, `docs/session-format.md`; grok `updates.jsonl` |
| D-04 | 2026-09-15 | **Catalog = embedded catwalk + overlay**, overlay wins; the overlay may read gx's `providers.toml`/`config.toml`. | catwalk has the gx models with prices and flags; where it disagrees with gx (effort lists) the user's config is the truth; quirks stay data-driven. | catwalk configs; opencode `transform.ts` as the anti-pattern |
| D-05 | 2026-09-16 | **Home is `~/.craze-acp/`**, overridden by one env var (`CRAZE_ACP_HOME`). craze's TUI config stays at `~/.craze/config.toml` for now (`10` Q1). | Owner's call. | pi `PI_CODING_AGENT_DIR`, grok `$GROK_HOME` |
| D-06 | 2026-09-16 | **Four tools first** (read, bash, edit, write), then ls, glob, grep; edit is multi-edit with the opencode replacer ladder; parallel by default with a per-file mutation queue. | pi ships on four; the ladder is documented and cited; fantasy runs tools in parallel. | pi `tools/`, opencode `tool/edit.ts` |
| D-07 | 2026-09-16 | **A `length` stop with tool calls fails the calls** rather than executing possibly-truncated arguments. | Simpler and safe; grok's salvage path is not worth its complexity for us. | pi `agent-loop.ts`; grok `LengthPolicy` |
| D-08 | 2026-09-16 | **Retry**: fantasy's retries stay on for transport and 5xx; auth, quota, and context-overflow are non-retryable and surface at once; overflow triggers one compact-and-retry in the session. | pi disables SDK retries because they mask quota errors; opencode never retries overflow. | pi `docs/models.md`, opencode `session/retry.ts` |
| D-09 | 2026-09-16 | **Permission grammar accepts Claude Code's rule syntax** (`Bash(git *)`), with a fixed tool-alias table. | Costs nothing while the grammar is being designed; keeps imported or hand-written rules portable. Not a commitment to read `settings.json`. | grok `claude_alias.rs`, `claude_settings.rs` |
| D-10 | 2026-09-16 | **Sub-agents start as child craze processes** (`craze prompt --json`), in-process nesting later if needed. | Isolation and the headless path come free; pi's sub-agent extension proves the shape. | pi `examples/extensions/subagent/` |
| D-11 | 2026-09-15 | **Modes are rulesets plus prompt reminders; plan mode is enforced in the dispatcher** so it survives yolo; the plan file is writable; `exit_plan_mode` reads the plan from disk and fails closed on an unknown outcome. | Both opencode and grok converge on rulesets; grok's dispatcher gate is the safer variant for a tool with a force flag. | grok `tool_calls.rs`, opencode `agent.ts` |
| D-12 | 2026-09-15 | **No read-before-edit enforcement; truncation is one wrapper with full output on disk; doom-loop ask after three identical calls; interrupted tool calls are closed with errored results.** | All three references agree. | opencode, grok, pi |
| D-13 | 2026-09-16 | **Claude compat is import, not live**: project files are read from the workspace; user-level Claude content is copied into the home by `craze import claude`; `~/.claude/*` is never read at runtime. `settings.json`, hooks, MCP are non-goals. | Owner's call; removes the two most maintenance-heavy parts of grok's compat layer. | grok `claude_import.rs`; pi `package-manager.ts` |
| D-14 | 2026-09-16 | **No skill tool.** Skills enter the prompt as name/description/path rows; the model reads the file; user invocation expands at prompt-assembly time. | pi does this; craze already expands client-side. | pi `docs/skills.md` |
| D-15 | 2026-09-16 | **Instruction files exceed grok in two places**: `@path` imports are followed (depth 5) and rule `paths:` frontmatter gates rules. | Claude Code does both; both are small. | grok `agents_md.rs` (does neither) |
| D-16 | 2026-09-16 | **The provider is hidden** until a later decision flips it: never in the picker, reachable only by explicit `--provider native`, env, or persisted config. | Owner's call: ship in phases behind a name, enable for everyone once it works. | craze `Provider.optional` as the weaker precedent |
| D-17 | 2026-09-15 | **Tool calls carry grok's kind vocabulary** (`kind`, `namespace`, `read_only`, canonical input). | craze's cards already key on it; nothing in the TUI changes. | grok `tool_taxonomy.rs`, `normalization.rs` |
| D-18 | 2026-09-16 | **MCP is deferred**, and when it comes it is a tool wrapper, not a loop change. | Owner's call; pi refuses it outright, grok hides it behind `search_tool`/`use_tool`. | — |

## Open (not yet decided)

See `10-open-questions.md`. Notably: system prompt as transcript system
messages (pi) vs rebuilt per request; the provider id (`native` is the
working name); whether craze's TUI config moves under the new home.
