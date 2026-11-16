# Web search W3: domains, num_results and the excerpt line (2026-11-15)

Does PR W3 of `discovery/web-search` (issues #140 and #131) regress the search-only web
tasks, and does it get Claude Haiku to fetch the page T-WS6's answer is on? Run per
`eval/RUNBOOK.md` §9, `craze-web` arm alone.

## Setup

- **Tasks:** T-WS1–T-WS6, as in [2026-11-websearch](../2026-11-websearch/README.md).
- **Models:** `muse-spark-1.3-contributor` and `anthropic/claude-haiku-5-5`, three reps.
- **Binaries**, each built from a commit object (`git archive`):
  - *before*: `0d9b88c` (W1: rate-limit retries and failure words; nothing the proxy can see);
  - *after*: W3 with the excerpt line in the tool description only;
  - *footer*: W3 as shipped, the description line plus a last line after every search's
    results ("These results are excerpts, often only the start of each page. Before
    relying on a detail from one, read the page with webfetch.").
- The proxy learned `web_search_advanced_exa` (domain filter, Exa's JSON) and `numResults`.
- **Caps:** `--budget-cap 10 --run-cap 0.50`. Ledger total for the campaign, smoke and the
  extra T-WS6 runs included: **$1.88**.

## Result

| binary | Haiku | Muse | Haiku on T-WS6 | fetches (all runs) |
|---|---|---|---|---|
| before | 15 of 18 | 18 of 18 | 0 of 3, no run fetched | 91 |
| after (description only) | 15 of 18 | 18 of 18 | 0 of 3, no run fetched | 92 |
| footer | 15 of 18 | 18 of 18 | 0 of 3 (2 of 3 fetched the page) | 114 |
| footer, T-WS6 alone, 5 reps | | | 2 of 5 (4 of 5 fetched the page) | |

`summary.json` is the footer batch's.

- **No regression** on any task or model with either change.
- **The description line alone changed nothing** on T-WS6: Haiku searched twice and wrote
  the fix from the excerpts in every run, as in 2026-11-websearch.
- **The footer gets Haiku to read the page**: 6 of its 8 T-WS6 runs fetched the
  troubleshooting page (0 of 6 without it), and 2 of 8 passed (0 of 6). The 4 that read the
  page and still failed are the model's reasoning, not the tool. Fetches rose a quarter
  overall, with no change in pass rate elsewhere.
- **Neither model used `domains`** on these tasks, where the docs site is unknown until a
  first search shows it. `num_results` was used by Muse (10) in the smoke. The options are
  for research sessions on real sites; this split cannot measure their token savings.

## What it cannot show

The corpus proxy never rate limits, refuses or returns Exa's crawl errors, so W1 and W2
(retries, key handling, failure words, `/websearch`) are invisible here; those were checked
live against Exa. Three reps per cell (five on T-WS6) is a small sample.
