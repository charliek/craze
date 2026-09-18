# 10 — Open questions

Owner's calls, in the order the roadmap meets them. Resolve by adding a row
to `08-decisions.md`. H0 did not resolve Q4: the probe reproduced gx's
configuration semantics only for evaluation, not as a production read/import
decision.

| # | question | default if unanswered | needed by |
|---|---|---|---|
| Q1 | Does craze's TUI config (`~/.craze/config.toml`, `CRAZE_CONFIG`) move under `~/.craze-acp/`, or does only the harness live there? | Only the harness; TUI config stays put. | H1 |
| Q2 | Provider id: `native`, `craze`, or `craze-acp`? Shows in `--provider`, the status row, and persisted config. | `native` | H1 |
| Q3 | System prompt and tool declarations as system messages **in the transcript** with named sections and tool diffs (pi), or rebuilt per request and kept out of the store? | Rebuilt per request; revisit if provider cache misses show up in cost. | H1 |
| Q4 | Does the overlay **read gx's TOML** directly (zero new config, coupled to gx's schema) or does `craze import gx` copy it once into `~/.craze-acp/config.toml`? | Import once, matching D-13's model for Claude content. | H1 |
| Q5 | Bash background jobs: never (pi), or grok's auto-background at five minutes with wake-on-complete? | Never in H2; revisit after H6. | H2 |
| Q6 | Should `always` grants also be offered per-session-only (grok's `allow-edits-session`)? | Yes, one extra option on edit-kind asks. | H3 |
| Q7 | Marketplace: stay on Claude Code's cache, or build the native git installer? | Cache; installer deferred. | after H4 |
| Q8 | In-process sub-agents after the child-process cut, or never? | Decide on H6's latency numbers. | H6 |
| Q9 | Compaction trigger: `context − 16k reserve` (pi) or 85 % of window (grok)? Tail to keep: 20k tokens (pi) or `clamp(usable×0.25, 2k, 15k)` (opencode)? | pi's numbers. | H7 |
| Q10 | When does the provider become visible (D-16)? After H7? After H8? | After H7 passes its smoke on both platforms. | — |
| Q11 | ChatGPT-plan auth: port gx's token minting, or crush's oauth package, or skip for good? | Skip until asked. | — |
