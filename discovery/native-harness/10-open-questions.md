# 10 — Open questions

Owner's calls, in the order the roadmap meets them. Resolve by adding a row
to `08-decisions.md`. **Q1–Q4 were resolved by Plan 018 on 2026-09-18**
(D-27..D-30); their rows are kept below for history, each marked with the
decision that closed it.

| # | question | default if unanswered | needed by |
|---|---|---|---|
| Q1 | **Resolved (D-27).** Does craze's TUI config (`~/.craze/config.toml`, `CRAZE_CONFIG`) move under `~/.craze-acp/`, or does only the harness live there? | **Answered:** everything lives under `~/.craze` (harness in `~/.craze/native/`); one `CRAZE_HOME` relocates it; `CRAZE_CONFIG` is removed. | H1 |
| Q2 | **Resolved (D-29).** Provider id: `native`, `craze`, or `craze-acp`? Shows in `--provider`, the status row, and persisted config. | **Answered:** `native`, with a display label separate from the id. | H1 |
| Q3 | **Resolved (D-30).** System prompt and tool declarations as system messages **in the transcript** with named sections and tool diffs (pi), or rebuilt per request and kept out of the store? | **Answered:** built once per session, frozen (byte-identical prefix), never stored. | H1 |
| Q4 | **Resolved (D-28).** Does the overlay **read gx's TOML** directly (zero new config, coupled to gx's schema) or does `craze import gx` copy it once into `~/.craze-acp/config.toml`? | **Answered:** import once into two files, `~/.craze/native/providers.toml` (0600) and `models.toml` (0644); nothing reads gx's TOML at runtime. | H1 |
| Q5 | Bash background jobs: never (pi), or grok's auto-background at five minutes with wake-on-complete? **H2 answer: never**; everything left in the command's session is killed when the command returns (D-38). | Never in H2; revisit after H6. | H2 |
| Q6 | Should `always` grants also be offered per-session-only (grok's `allow-edits-session`)? H3 itself is reworded: the owner's likely direction is an auto-mode evaluator over the H2 gate, not ask-on-everything (D-39), so this question — and the grants question generally — waits for H3 to be planned, after session-control S1. | Yes, one extra option on edit-kind asks, if H3 keeps an asks model at all. | H3 |
| Q7 | Marketplace: stay on Claude Code's cache, or build the native git installer? H4 (Plan 022, owner decision 5) stays on Claude Code's own install and reads only the plugins installed and enabled there, so this question is untouched by H4 and stays open. | Cache; installer deferred. | after H4 |
| Q8 | In-process sub-agents after the child-process cut, or never? | Decide on H6's latency numbers. | H6 |
| Q9 | Compaction trigger: `context − 16k reserve` (pi) or 85 % of window (grok)? Tail to keep: 20k tokens (pi) or `clamp(usable×0.25, 2k, 15k)` (opencode)? | pi's numbers. | H7 |
| Q10 | When does the provider become visible (D-16)? After H7? After H8? | After H7 passes its smoke on both platforms. | — |
| Q11 | ChatGPT-plan auth: port gx's token minting, or crush's oauth package, or skip for good? | Skip until asked. | — |

H5 (Plan 023, 2026-09-21) leaves three items as follow-ups rather than
questions for now, each recorded in the plan's §4/§9: free-text "Other"
answers on the question card, a model-initiated `enter_plan_mode` tool, and
a system-prompt sentence nudging the model to write todos — R2's smoke
measures whether a model writes them unprompted with none. R2's answer:
neither `fireworks/kimi-k3` nor `openrouter/glm-5.3-flash` writes todos
unprompted on a multi-step task, on either PR, so the prompt-sentence
follow-up stands. R3 measured the plan-mode reminder's cache cost stated in
`05`: a full cache-read miss on every plan-mode turn's first request on
Fireworks (0 against ~9,000–10,000 in agent mode) — worse than expected, so
the variant-marker replay is now a priority follow-up, not a nice-to-have.
