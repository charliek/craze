# 04 — Parity with a native Claude Code sub-agent

The bar is a Sonnet sub-agent ([01](01-goals-and-scope.md)). "Prototype" is
what [02](02-experiments.md) measured; "after T1" is what plan 041
delivered, verified live where an `L` check is named ([07](07-roadmap.md)).

| Behaviour | Native Sonnet sub-agent | Prototype (2026-10-10) | After T1 |
|---|---|---|---|
| Dispatched with `Agent(subagent_type=…)` | yes | yes (`craze:craze-task`, E4) | yes (`craze:delegate`, presets) |
| Works in the caller's directory | yes | yes (`--cwd "$PWD"`) | yes |
| Read, edit, search and shell tools | Claude Code's | the provider's own (native: read, write, edit, glob, grep, bash, todo, agent) | same |
| Reads `CLAUDE.md` and rules | yes | native yes (`internal/agent/instructions.go:65`, the same `@import` depth of 5, `:40`); cursor and grok read their own | same |
| Background run, completion notification | yes | yes, through the wrapper | yes |
| Orchestrator's context stays clean | yes | yes: final message plus a footer | yes |
| `SendMessage` continues with context | yes | yes (E3) | yes, and after the host has ended: `craze send` reloads it (TD-12, L4) |
| `TaskStop` stops the work | yes | **no**: the craze turn runs on | yes: the turn ends `cancelled`, the session stays for `SendMessage` (TD-15, L6) |
| Turn longer than ten minutes | yes | by design, untested | `--timeout`, then `craze wait` |
| Watch it live | transcript view | `craze attach` | same |
| Cost reported | yes | native only | native only; plans shown as unpriced |
| `isolation: worktree` | yes | untested; should work, since the wrapper's directory is the worktree | yes: the session's workspace is the worktree (L10) |
| Claude Code's permission rules and hooks | apply | **do not apply** | do not apply (non-goal) |
| Read-only | `tools` allow-list | plan mode (edits refused, shell not sandboxed) or ask mode (no shell) | same; a sandbox is T2 (TF-5) |
| Asks during the turn | answered by Claude Code's own flow | answered by the waiting runner; one with nobody waiting parks the host forever | host policy (TD-11): a question takes its first option (L5), a plan is returned and the turn ends (L7); permissions report `needs_input` |
| Leaves no trace in the person's `craze -c` | n/a | **no**: becomes the newest row | yes; tagged `sub-agent` in the picker and session list (TD-16, L8) |
| Overhead per call | none | about 5–6 s and $0.0014 (Haiku) | same |

## The gaps that stay

- **Permissions and hooks.** A craze session's tools run in craze, so
  Claude Code never sees those calls. The plugin's documentation says so, and
  says how to keep a sub-agent read-only.
- **Review sandboxing.** codex's read-only mode is an OS sandbox; craze's is a
  gate the shell gets through. That is the main thing standing between craze
  and replacing `codex exec` for reviews (TQ-3, TF-5).
- **Usage from ACP providers.** cursor and grok send none through ACP, so
  there is nothing to report.
