# The start-model campaign (2026-11-13/14)

The record of the campaign issue #118 asked for: is `fireworks/kimi-k3p1-pro` at least as
good as `fireworks/kimi-k3` as the model a Fireworks-only machine starts on? It also ran
the other providers' start models on the same tasks as a regression check after plans 050
and 051.

**It does not answer the Fireworks question.** The budget guard stopped the main batch
when Kimi K3.1 Pro had finished 3 of the 17 tasks. Every other model ran all 17 tasks
twice. Read *What the campaign cannot say* before quoting a number from here.

## Setup

craze native only, built from `00fa533` (plans 050 and 051 merged), evaluator at `abed7b8`,
config snapshot `c57cc8e8…`. Headless, each run sandboxed behind the keyed recording proxy,
as in `eval/RUNBOOK.md`.

17 tasks, 2 reps each: 9 dev and 8 held out. No reference harness ran and no judge ran,
so there are no win rates and the success bar reads `inconclusive` for every model by
construction. The scores here are the objective checks (hidden tests, required facts,
scope), cost, time and tool use.

- gx could not run: the owner's gx config names no models now.
- opencode and codex were left out to keep the campaign inside its budget.

Models, each at the effort craze's catalog starts it on:

| model | provider | effort |
|---|---|---|
| `fireworks/kimi-k3p1-pro` | Fireworks | high |
| `fireworks/kimi-k3` | Fireworks | high |
| `fireworks/deepseek-v4p1-flash-1017` | Fireworks | high |
| `anthropic/claude-haiku-5-5` | Anthropic | medium |
| `muse-spark-1.3-contributor` | Meta | high |
| `glm-5.3` | Z.AI coding plan | max |
| `glm-5.3-flash` | Z.AI coding plan | high |

Batches: `main-20261113-224427` (all seven models, stopped by the budget guard),
then three zero-cost Z.AI batches for the GLM runs the stop cut off
(`glm-flash-20261114-002047`, `glm-rest-20261114-011601`, `glm-rest2-20261114-013744`).
A 14-run smoke batch came first and passed 14 of 14.

## Results

Objective passes are per task, averaged over the two reps, so one failed rep costs half a
point. Cost is the provider's list price. Medians are over finished runs.

| model | runs finished | dev (of 9) | held-out (of 8) | all (of 17) | median cost / run | total cost | median wall | median tool calls | median answer words |
|---|---|---|---|---|---|---|---|---|---|
| `anthropic/claude-haiku-5-5` | 34 of 34 | 9 | 8 | 17 | $0.004 | $0.41 | 23 s | 7 | 254 |
| `fireworks/deepseek-v4p1-flash-1017` | 34 of 34 | 9 | 8 | 17 | $0.019 | $1.49 | 33 s | 14 | 391 |
| `glm-5.3-flash` | 34 of 34 | 9 | 8 | 17 | $0 (plan) | $0 (plan) | 109 s | 13 | 277 |
| `fireworks/kimi-k3` | 34 of 34 | 9 | 7.5 | 16.5 | $0.088 | $5.55 | 47 s | 10 | 238 |
| `muse-spark-1.3-contributor` | 33 of 34 | 8.5 | 8 | 16.5 | $0.014 | $1.00 | 47 s | 12 | 173 |
| `glm-5.3` | 33 of 34 | 8.5 | 7.5 | 16 | $0 (plan) | $0 (plan) | 193 s | 13 | 240 |
| `fireworks/kimi-k3p1-pro` | 6 of 34 | 3 of 3 run | not run | 3 of 3 run | $0.056 | $3.30 | 47 s | 14 | 64 |

The two GLM models are on the Z.AI coding plan, a flat subscription, so their runs cost
nothing against the budget. At Fireworks' list price for GLM 5.3 the same tokens would be
$6.83 (`glm-5.3`) and $4.82 (`glm-5.3-flash`).

The four runs that did not pass:

- **`fireworks/kimi-k3`, T-F2 rep 2** and **`glm-5.3`, T-F2 rep 2**: the model's `Bytes`
  crashed the hidden test for the most negative size (`TestHiddenBytesNegative`). This is
  the same case plan 029 recorded on three models; the other rep passed on both.
- **`glm-5.3`, T-P1 rep 2**: timed out at the task's 1,200 s limit after 92 tool calls and
  5.3M input tokens, with no answer.
- **`muse-spark-1.3-contributor`, T-E1 rep 2**: the answer was finished and passed its
  checks, but the evaluator voided the run (`budget-capped`). The model made 101 tool
  calls, and one request was refused because the run already held four open budget
  reservations. This is a limit of the evaluator, not a wrong answer.

`fireworks/deepseek-v4p1-flash-1017` showed no runaway generation on the plan tasks, the
failure the runbook asks each campaign to look for: 4 of 4 plan runs passed, and its
largest single response was 14,950 output tokens (T-P1). Four runs is a small sample.

## Kimi K3.1 Pro against Kimi K3

On the three tasks both finished (T-B1, T-D1, T-D2; two reps each):

| | objective | cost, 6 runs | tool calls, 6 runs | input tokens, 6 runs | median answer words |
|---|---|---|---|---|---|
| `fireworks/kimi-k3` | 6 of 6 | $0.33 | 46 | 319k | 139 |
| `fireworks/kimi-k3p1-pro` | 6 of 6 | $0.37 | 69 | 452k | 64 |

K3.1 Pro passed the same checks with half again as many tool calls and answers under half
as long. Its lower price per token roughly cancelled its higher token use.

On T-E1 (explain how craze builds its prompt prefix) the two models diverged. Both K3.1
Pro runs were still going when the batch stopped, two and four minutes in:

| | status | tool calls | file reads | input tokens | cost |
|---|---|---|---|---|---|
| `fireworks/kimi-k3` rep 1 | passed | 17 | 11 | 358k | $0.38 |
| `fireworks/kimi-k3` rep 2 | passed | 19 | 14 | 494k | $0.48 |
| `fireworks/kimi-k3p1-pro` rep 1 | stopped, unscored | 112 | 79 | 1.81M | $1.39 |
| `fireworks/kimi-k3p1-pro` rep 2 | stopped, unscored | 132 | 95 | 2.93M | $1.54 |

The reads were all different (no loop), so K3.1 Pro was reading far more of the
repository than K3 did, and resending a growing context each time. Each stopped run had
already cost three to four times one whole K3 run. Whether either would have passed is
unknown. The other models' T-E1 runs took 22 to 67 tool calls, apart from the Muse run of
101 calls that the evaluator voided.

## What the campaign cannot say

- **Whether K3.1 Pro is at least as good as K3.** It ran 3 of 17 tasks. The condition
  issue #118 sets for keeping it as the Fireworks start is not shown either way.
- **Which model writes the better answer.** With no judge, a passing run by one model and
  a passing run by another count the same. Plan 029 found the objective checks sit near
  the ceiling for every harness, and that is true here: all six finished models scored
  16 or more of 17.
- **Anything blind about the held-out tasks.** These eight were already read in plan
  029's final. Nothing in craze was changed from this campaign's results, but the set is
  still due the rotation `eval/RUNBOOK.md` §6 describes before the next tuning loop.

## What it does show

- **The other start models hold up after plans 050 and 051.** Haiku 5.5, Muse Spark
  contributor, GLM 5.3 and GLM 5.3 Flash passed every task at least once, and the misses
  are one rep each.
- **K3.1 Pro can be far more expensive than K3 on a repository question**, despite its
  lower price per token. Two runs on one task is thin evidence, but it is the kind of task
  a new session often starts with.
- **`fireworks/deepseek-v4p1-flash-1017` is the cheap Fireworks candidate that finished.**
  It passed 34 of 34 for $1.49, against K3's 33 of 34 for $5.55. It uses more tool calls
  than K3 (median 14 against 10) and writes longer answers (391 words against 238).
  Answer quality was not judged.

## Budget

Approved cap: $20, counted by the evaluator's ledger, which charges Fireworks models at
1.5 times list (the US serverless rate). The ledger ended at $17.18 committed. At list
price the campaign cost about $12.

Two things went wrong with the budget, both in the evaluator:

1. **One refused request stops the whole batch.** The ledger admits a request only if
   everything committed plus every open reservation fits under the cap. K3.1 Pro has no
   output limit in the catalog, so each of its requests reserved about $2.92, and three
   ran at once. With $2.82 of headroom left, one K3.1 Pro request was refused and the
   batch stopped for all seven models, leaving 26 K3.1 Pro runs and all of GLM 5.3 Flash
   not started. The GLM runs were then run separately at no cost.
2. **The estimate was too low.** The evaluator projected the main batch at $4.24, from
   the medians of the smoke runs, which are far smaller than the real tasks. The batch
   had spent $16.93 when it stopped. Kimi K3's two T-P1 runs alone cost $2.48 on the
   ledger, and K3.1 Pro's two T-E1 runs $4.40.

For the next campaign: run a lane per provider so one model's reservations cannot stop
the others, give an expensive model its own lane with `--cap-other 1`, and leave headroom
of at least three of its reservations.

## Open

Finishing K3.1 Pro needs more budget than was approved. One more rep of its 14 unrun
tasks plus T-E1, one run at a time, fits under a cap raised to about $35. Until then the
Fireworks start model in `catalog.toml` rests on the maker's guidance and the partial
evidence above.

## How this record was made

From `eval/`, with `CRAZEEVAL_CAMPAIGN_DIR=~/.craze-eval/2026-11-start-models`:

```shell
uv run crazeeval report --unseal --out $CRAZEEVAL_CAMPAIGN_DIR/report \
  --batch $CRAZEEVAL_CAMPAIGN_DIR/eval-runs/main-20261113-224427 \
  --batch $CRAZEEVAL_CAMPAIGN_DIR/eval-runs/glm-flash-20261114-002047 \
  --batch $CRAZEEVAL_CAMPAIGN_DIR/eval-runs/glm-rest-20261114-011601 \
  --batch $CRAZEEVAL_CAMPAIGN_DIR/eval-runs/glm-rest2-20261114-013744
uv run crazeeval summarize --report $CRAZEEVAL_CAMPAIGN_DIR/report \
  --campaign-name 2026-11-start-models --out results/2026-11-start-models/summary.json
```

`summary.json` holds the report's own numbers: its `cost $` medians are the ledger's
(1.5 times list for the Fireworks models), where the tables above use list price, and its
`statuses` omit the runs a stopped batch never started. The per-run figures in the Kimi
tables come from each run's `result.json`. The full archive has not been published to
`charliek/craze-evals` yet; the raw runs are local.
