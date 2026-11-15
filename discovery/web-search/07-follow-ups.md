# 07 — Follow-ups

`WF-nn`, in proposed priority order. Each is a GitHub issue; the issue holds the discussion and the evidence comments, and this table holds the order and the phase. A plan's work breakdown says which rows it takes; a row it closes is deleted.

| ID | Priority | Issue | What | Phase | Effort |
|---|---|---|---|---|---|
| WF-1 | 1 | #138 | Retry rate-limited calls (both shapes) and cap concurrent calls per session. Keyless batches of 8 lose 5 of 8 today; retry recovered all 8 within 4.6 s (E3, E4). | W1 | small–medium |
| WF-2 | 2 | #137 (B1) | The unkeyed 429 hint names `craze auth login websearch/exa` and the next-session rule. | W1 | small |
| WF-3 | 3 | #139 | Plain words for `CRAWL_NOT_FOUND`, `CRAWL_UNKNOWN_ERROR` and the keyless 403 (E7). | W1 | small |
| WF-4 | 4 | #141 | In-session key source, plus call and failure counts. | W2 | small |
| WF-5 | 5 | #137 (B2) | A key stored mid-session is used from the next turn, after the redactor holds it. | W2 | medium |
| WF-6 | 6 | #131 | Models trust excerpts instead of fetching: try a line in the tool description; re-run T-WS6. | W3 | small–medium |
| WF-7 | 7 | #140 | Optional `domains` and `num_results` through `web_search_advanced_exa`; needs a JSON reply parser (E2, E8). | W3 | medium |
| WF-8 | later | #136 | A second provider, hosted search on the Responses driver, a local `webfetch`, moving `KeyPattern`. | later | large |
