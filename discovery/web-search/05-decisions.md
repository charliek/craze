# 05 — Decisions

Append-only. A reversal is a new row naming the one it replaces. IDs are
`WD-nn`, distinct from `D-nn` (native harness), `SD-nn` (session control) and
`TD-nn` (sub-agents).

The design facts
this folder builds on (Exa through its hosted MCP endpoint, the consent
question, the key order env → stored → keyless, the key fixed at session open)
are plan 051's and are described in `docs/reference/configuration.md`
§ Native web search.

| ID | Date | Decision | Why | By |
|---|---|---|---|---|
| WD-01 | 2026-11-15 | A rate-limited call (HTTP 429, or an error result naming 429) is tried again, at most 3 attempts, after `Retry-After` (else 1 s) × (1 + jitter) × attempt, never past the call's 25 s. | E4 recovered every call in a batch of 8 with at most 2 retries; the keyed limit (E5) has no Retry-After. Resolves WQ-1. | owner (scope), build |
| WD-02 | 2026-11-15 | At most 4 calls per service at Exa at once, shared with children; no config key. Retry, not a rate limiter, covers the keyed 10-per-second limit. | A cap alone does not bound a start rate (PR #143 review); retry absorbs the keyed limit (24 of 24 live). Resolves WQ-2. | owner (scope), build |
| WD-03 | 2026-11-15 | The consent card has three options: Always allow (remembered), Allow for this session (not remembered), Disable web search (remembered); a session re-reads `websearch.json` before it asks. | The owner wanted to be asked once with a 'yes, going forward'; sessions opened side by side asked once each. | owner |
| WD-04 | 2026-11-15 | In-session visibility is a `/websearch` command (status, and `on`/`off`/`ask` for new sessions), fed by a new session-wide state section, `webStatus`. | The status row is crowded; the section reaches detached hosts with no new method. Resolves WQ-4. | owner |
| WD-05 | 2026-11-15 | Every search with results ends with a line saying they are excerpts and to fetch before relying on a detail; the description says so too. | The description alone moved nothing on T-WS6; with the footer Haiku fetched in 6 of 8 runs (0 of 6), eval/results/2026-11-websearch-w3. | build (eval) |
| WD-06 | 2026-11-15 | `objective` is not exposed; 8 results stays the default, `num_results` and `domains` are optional. | E8 (one trial) and no regression with the options in the eval. Resolves WQ-3, WQ-5. | owner (defaults), build |
