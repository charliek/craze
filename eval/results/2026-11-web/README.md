# Web search campaign (craze plan 051, 2026-11-14)

Does craze's native harness with web search (`craze-web`) solve tasks plain `craze` cannot, and
at what cost? Plan 051 E6, run per `eval/RUNBOOK.md` §9.

## Setup

- **Tasks:** the six `web` tasks, T-W1–T-W6. Each is a client for a fictional HTTP API whose
  decisive facts are only in a frozen corpus of 10–14 pages, at least three distractors per
  relevant page. Two tasks are reference docs, two a changelog's breaking change, two a
  troubleshooting page. Scored by hidden tests alone; no judge.
- **Arms:** `craze` (no web tools) and `craze-web` (`websearch` and `webfetch`, answered by the
  proxy from the task's corpus; BM25 for search). The sandbox has no network in either arm.
- **Models:** `muse-spark-1.3-contributor` (effort high) and `anthropic/claude-haiku-5-5`
  (effort medium). Three reps: 72 runs, plus a 4-run smoke on T-W1.
- **Binary:** craze built from `e579578` (main with plan 051 and the eval fixes #122, #123).
- **Caps:** `--budget-cap 10 --run-cap 0.50`, three runs at a time.

## Result

| model | craze | craze-web |
|---|---|---|
| claude-haiku-5-5 | 0 of 18 (0%, 90% CI 0–13%) | 15 of 18 (83%, 65–93%) |
| muse-spark-1.3-contributor | 0 of 18 (0%, 0–13%) | 18 of 18 (100%, 87–100%) |
| both | 0 of 36 | 33 of 36 |

Plain craze solved none of the six tasks with either model; with the web tools both models solved
nearly all of them. Haiku's three failures were all T-W6 (a resend-with-the-answer's-wait protocol),
which Muse passed every time.

## Cost and behaviour (medians per run)

| model / arm | main requests | tool calls | input tokens | output tokens | cost | wall |
|---|---|---|---|---|---|---|
| haiku / craze | 6 | 5 | 84,574 | 2,244 | $0.0032 | 31 s |
| haiku / craze-web | 8 | 13.5 | 144,598 | 8,010 | $0.0075 | 38 s |
| muse / craze | 53.5 | 57 | 3,096,720 | 29,730 | $0.3175 | 376 s |
| muse / craze-web | 16 | 22 | 226,996 | 6,963 | $0.0241 | 65 s |

Web costs Haiku a little more: it reads the docs before writing. Muse without the web searches the
sandbox for minutes (four of its plain runs hit the $0.50 run cap); with it, it reads the docs and
finishes in a fifth of the time at a thirteenth of the cost.

The web route served 90 fetches to Haiku (4.5 per run) and 123 to Muse (6.5 per run, 3 misses on
T-W5). **No run used `websearch`.** Every task prompt names its documentation site
(`https://docs.<product>.example`), so the models fetched it and followed its links.

## Fidelity

Effort parity and one model per run held; `craze-web` was offered exactly `websearch` and
`webfetch` in every run. Plain `craze` was offered no web tools by the harness, but the capture
check reports `web tools absent: False` for Muse: in three plain runs the model looked for a way
out. In T-W4 rep 1 and T-W6 rep 2 it ran a nested `craze prompt --web-search` through `bash` (the
craze binary is inside the sandbox); in T-W3 rep 2 it sent its own request through the model proxy
with a provider-hosted `web_search` tool. Containment held: the nested sessions' web calls failed
(the sandbox has no network, and the proxy refused `api.exa.ai` as an upstream not in its route
table), and the proxy answered the hosted-search request with a 403. All three runs failed their
hidden tests. No web content reached the plain arm.

## Spend

$6.92 committed over the campaign (the smoke's $0.20 included), against the $10 cap; no open
reservations.

## What it shows, and what it does not

It shows that with the web tools a model reads documentation it is pointed at and uses it to solve
tasks it otherwise cannot, and that for a model that otherwise flails (Muse) this is also far
cheaper. It does not measure:

- **search.** No run searched; the prompts name the docs site. A task that names only the product
  would exercise `websearch` (future work).
- **Exa.** The tools read a frozen corpus, not Exa's index; nothing here is about Exa's ranking.
- **harm.** The web tools were offered only on web tasks; whether having them makes a model worse
  on tasks that need no web is not measured.

Small n (18 runs per cell). Six fictional APIs are easier to "need the docs for" than real ones,
whose facts a model may half-know.

## Follow-ups

- Web tasks whose prompt names only the product, not the docs site, to measure `websearch`.
- The plain arm's escape attempts: the craze binary in the sandbox lets a model start a nested
  `--web-search` session. It cannot reach anything, but a plain-arm home that turns web search off
  (or a test endpoint at a dead port) would keep the capture check clean.
- A no-harm A/B on the dev tasks with `craze-web`.

## What is in this directory

- `README.md`: this file.
- `summary.json`: the rollup from `crazeeval summarize` (`schema` 1, one group); its `web` object
  holds the pass rates per model and arm, the web route's counters and the metric medians.

The archive (both batches' `runs.jsonl`, `batches.json`, `archive.json`) is in the private repo
`charliek/craze-evals` at `campaigns/2026-11-web/`. Raw runs stay local.

## How it was produced

From `eval/`, with `CRAZEEVAL_CAMPAIGN_DIR=~/.craze-eval/2026-11-web`: `snapshot-config`,
`validate --tasks split:web` (6/6), `probe`, the smoke `run --tasks T-W1 --reps 1 --label
web-smoke`, the campaign `run --harness craze,craze-web --model
muse-spark-1.3-contributor,anthropic/claude-haiku-5-5 --tasks split:web --reps 3 --craze-bin
../bin/craze-web --budget-cap 10 --run-cap 0.50 --parallel 3 --label web`, `report`, `captures`,
then `summarize --allow-sealed` into this directory.
