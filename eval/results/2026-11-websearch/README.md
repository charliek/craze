# Web search campaign: search-only tasks (craze plan 051 follow-up, 2026-11-14)

When a task does not say where its documentation is, does craze's native harness with web
search (`craze-web`) search for it, find the right pages and solve the task? The
[2026-11-web campaign](../2026-11-web/README.md) (C12) could not tell: its prompts named the
docs site, and no run searched. Run per `eval/RUNBOOK.md` §9.

## Setup

- **Tasks:** T-WS1–T-WS6, the search-only forms of C12's T-W1–T-W6 (`variant_of`): the same
  fixtures, frozen corpora (10–14 pages, at least three distractors per relevant page), hidden
  tests and reference patches. The prompt says only that the documentation "is published on the
  web", and a setup patch takes the docs site's address out of the fixture's README; the lint
  checks that no corpus host is in the prompt or the start files. Scored by hidden tests alone.
- **Arm:** `craze-web` alone in the campaign (`websearch` and `webfetch`, answered by the proxy
  from the task's corpus; BM25 for search). Plain `craze` ran only in the smoke: on these tasks
  it is C12's 0 of 36 again (the prompts differ by one clause), and its home now turns web search
  off (`websearch = false`), which the smoke checked.
- **Models:** `muse-spark-1.3-contributor` (effort high) and `anthropic/claude-haiku-5-5`
  (effort medium), as in C12. Three reps: 36 runs, plus a 4-run smoke on T-WS1 with both arms.
- **Binary:** craze built from `d9ea9fe` (main with #128).
- **Caps:** `--run-cap 0.50`; `--budget-cap 5` for the smoke and `10` for the campaign. The
  campaign at $5 was refused before it started: the projection takes each model's median smoke
  run, and Muse's two were $0.03 (craze-web) and $0.49 (plain), so it projected $5.00 for 36
  runs that settled $0.65. The smoke ran two runs at a time, the campaign three.

## Result

| model | craze-web | C12's craze-web (T-W, docs site named) |
|---|---|---|
| claude-haiku-5-5 | 15 of 18 (83%, 90% CI 65–93%) | 15 of 18 (83%, 65–93%) |
| muse-spark-1.3-contributor | 18 of 18 (100%, 87–100%) | 18 of 18 (100%, 87–100%) |
| both | 33 of 36 | 33 of 36 |

Both models solved the search-only tasks as often as the ones that named the docs site. Haiku's
three failures are again all on the retry task (T-WS6), for a different reason (below).

**Every run searched**: 1 or 2 searches per run, 56 in all, where C12 had none. In 34 of the 36
runs the first web call was a search. In the other two (Haiku, T-WS2 rep 3 and T-WS4 rep 3) it
was a fetch of the API's own base URL from the fixture's code, which missed, and then a search.
None guessed the docs site's address.

## Cost and behaviour (medians per run; half-token medians rounded up)

| model | main requests | tool calls | input tokens | output tokens | cost | wall |
|---|---|---|---|---|---|---|
| haiku, T-WS (search) | 10 | 12 | 193,831 | 8,894 | $0.0088 | 42 s |
| haiku, C12's T-W | 8 | 13.5 | 144,598 | 8,010 | $0.0075 | 38 s |
| muse, T-WS (search) | 16 | 20 | 249,564 | 6,785 | $0.0266 | 66 s |
| muse, C12's T-W | 16 | 22 | 226,997 | 6,963 | $0.0241 | 65 s |

Having to search costs little: about a tenth more cost and input tokens for Muse; for Haiku about
a sixth more cost and a third more input tokens; a few seconds for both.

| model | searches (median) | fetches (median) | misses | C12's fetches (median) |
|---|---|---|---|---|
| haiku | 29 (2) | 32 (1.5) | 4 | 90 (4.5) |
| muse | 27 (1.5) | 61 (4) | 0 | 123 (6.5) |

Fetches halved. A search result carries the first 1,500 characters of each page, so a run reads
part of each top-eight page before it fetches any, and needs fewer whole pages. Haiku's four
misses are the API base URLs above (`https://ingest.skerrywind.example`,
`https://api.tallowmere.example`, two each).

**Haiku on T-WS6 never fetched a page.** All three reps made two searches (one naming
`booking.hold_pending`) and wrote the fix from the snippets alone. The troubleshooting page's
claim header, which the resend needs, is past the snippet, and all three reps fail the same
four hidden tests (`test_a_held_booking_is_finished`,
`test_a_hold_still_pending_on_the_first_resend`,
`test_a_long_hold_is_resent_until_it_is_decided`, `test_a_released_hold_is_an_error`). C12's
Haiku fetched the pages and failed only `test_a_long_hold_is_resent_until_it_is_decided`. Muse
searched, then fetched the troubleshooting page, the idempotency page and the booking reference,
and passed every time. Haiku also passed two T-WS1 reps with no fetch at all, so it leans on
snippets wherever they are enough.

## Fidelity

In the campaign, one served model per run held, `craze-web` was offered exactly `websearch` and
`webfetch` in every run, effort parity held (Muse high, Haiku medium), and no run was
contaminated or retried: 36 attempts for 36 runs.

In the smoke, plain `craze` was offered no web tools (`web tools absent: True` for both models).
The plain Muse run looked for a way out, as four did in C12: it read the home's `config.toml`
(`websearch = false`) and `craze --help`, and sent its own requests through the proxy with a
hosted `web_search` tool and for another model. The proxy refused both (`unbounded-input`,
`model-not-allowed`). No nested `--web-search` session started, and the run failed its hidden
tests. Its hand-written requests carried no effort control, so the smoke's capture report says
`effort parity: no` for Muse, as C12's did.

## Spend

$1.20 committed over the campaign: the smoke's $0.56 (the plain Muse run alone $0.49) and the
36 runs' $0.65. No open reservations.

## What it shows, and what it does not

It shows that both models search for documentation they were not pointed at, find the relevant
pages among the distractors, and solve the tasks as often as when given the site, for little extra
cost. It also shows that a search snippet can stand in for a page, which hurt Haiku on T-WS6. It
does not measure:

- **Exa.** Search is BM25 over a frozen corpus, not Exa's index; a real index has far more
  distractors and other ranking.
- **an unprompted search.** The prompt says the docs are "published on the web", which points the
  model at its web tools.
- **harm.** The web tools were offered only on web tasks; whether having them makes a model worse
  on tasks that need no web is still not measured.

Small n (18 runs per cell, repeated trials on six fixed tasks).

## Follow-ups

- A no-harm A/B on the dev tasks with `craze-web` (unchanged from C12).
- Whether snippets that stop mid-page mislead: a task whose decisive fact is just past the
  snippet, and whether a prompt or tool-description nudge to fetch before relying on a result
  changes Haiku's T-WS6.

## What is in this directory

- `README.md`: this file.
- `summary.json`: the rollup from `crazeeval summarize` (`schema` 1, one group); its `web` object
  holds the pass rates, the web route's counters and the metric medians of the campaign batch.

The archive (both batches' `runs.jsonl`, `batches.json`, `archive.json`) is in the private repo
`charliek/craze-evals` at `campaigns/2026-11-websearch/`. Raw runs stay local.

## How it was produced

From `eval/`, with `CRAZEEVAL_CAMPAIGN_DIR=~/.craze-eval/2026-11-websearch`: `snapshot-config`,
`validate --tasks T-WS1,…,T-WS6` (6/6), `probe`, the smoke `run --harness craze,craze-web --model
muse-spark-1.3-contributor,anthropic/claude-haiku-5-5 --tasks T-WS1 --reps 1 --budget-cap 5
--run-cap 0.50 --parallel 2 --label websearch-smoke`, the campaign `run --harness craze-web
--model muse-spark-1.3-contributor,anthropic/claude-haiku-5-5 --tasks
T-WS1,T-WS2,T-WS3,T-WS4,T-WS5,T-WS6 --reps 3 --craze-bin ../bin/craze-web --budget-cap 10
--run-cap 0.50 --parallel 3 --label websearch`, `report`, `captures`, then `summarize
--allow-sealed` into this directory.
