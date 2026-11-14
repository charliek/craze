# The start-model campaign (2026-11-13/14)

The record of the campaign issue #118 asked for: is `fireworks/kimi-k3p1-pro` at least as
good as `fireworks/kimi-k3` as the model a Fireworks-only machine starts on? It also ran
the other providers' start models on the same tasks as a regression check after plans 050
and 051.

**Kimi K3.1 Pro is not shown to be as good as Kimi K3 on this suite, and it cost about
two and a half times as much.** It passed 15 of the 17 tasks. On the other two (T-E1 and
T-P1, both questions about the craze repository) it reached the $3 spending limit for a
run without giving an answer, where K3 answered both, twice, for a fraction of that.
K3.1 Pro has one scored run on 14 of the tasks and two on the other three, so this is
thinner evidence than the other models' two reps. Read *What the campaign cannot say*
before quoting a number from here.

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

Batches, in order:

- A 14-run smoke batch, which passed 14 of 14.
- `main-20261113-224427`: all seven models, two reps. The budget guard stopped it when
  Kimi K3.1 Pro had finished three tasks and GLM 5.3 Flash had not started (see *Budget*).
- `glm-flash-20261114-002047`, `glm-rest-20261114-011601`, `glm-rest2-20261114-013744`:
  the GLM runs the stop cut off, at no cost.
- `k31-rest-20261114-021556`: one rep of Kimi K3.1 Pro on its 14 remaining tasks, one run
  at a time with a $3 limit per run, after the owner raised the cap to $35.

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
| `fireworks/kimi-k3p1-pro` | 18 of 22 started | 7 | 8 | 15 | $0.078 | $10.21 | 65 s | 18 | 83 |

Kimi K3.1 Pro's row is one rep on 14 tasks and two on three (T-B1, T-D1, T-D2); its 13
other second reps were never run, so its score is over the reps it has. Its total
includes the four runs that were stopped or reached the run limit.

The two GLM models are on the Z.AI coding plan, a flat subscription, so their runs cost
nothing against the budget. At Fireworks' list price for GLM 5.3 the same tokens would be
$6.83 (`glm-5.3`) and $4.82 (`glm-5.3-flash`).

The runs that did not pass:

- **`fireworks/kimi-k3`, T-F2 rep 2** and **`glm-5.3`, T-F2 rep 2**: the model's `Bytes`
  crashed the hidden test for the most negative size (`TestHiddenBytesNegative`). This is
  the same case plan 029 recorded on three models; the other rep passed on both.
- **`glm-5.3`, T-P1 rep 2**: timed out at the task's 1,200 s limit after 92 tool calls and
  5.3M input tokens, with no answer.
- **`muse-spark-1.3-contributor`, T-E1 rep 2**: the answer was finished and passed its
  checks, but the evaluator voided the run (`budget-capped`). The model made 101 tool
  calls, and one request was refused because the run already held four open budget
  reservations. This is a limit of the evaluator, not a wrong answer.
- **`fireworks/kimi-k3p1-pro`, T-E1 and T-P1**: see the next section.

`fireworks/deepseek-v4p1-flash-1017` showed no runaway generation on the plan tasks, the
failure the runbook asks each campaign to look for: 4 of 4 plan runs passed, and its
largest single response was 14,950 output tokens (T-P1). Four runs is a small sample.

## Kimi K3.1 Pro against Kimi K3

One pass over the 17 tasks, K3's two reps beside K3.1 Pro's one scored run per task (rep 1
where it has two):

| | tasks passed | cost | tool calls | input tokens | median cost / run | median answer words |
|---|---|---|---|---|---|---|
| `fireworks/kimi-k3` rep 1 | 17 | $2.90 | 196 | 3.4M | $0.082 | 232 |
| `fireworks/kimi-k3` rep 2 | 16 | $2.65 | 195 | 2.8M | $0.094 | 246 |
| `fireworks/kimi-k3p1-pro` | 15 | $7.10 | 765 | 14.3M | $0.081 | 88 |

On a typical task the two cost the same: K3.1 Pro is cheaper per token and uses more
tokens. It makes about twice the tool calls (median 19 against 10) and writes answers
about a third as long.

The difference is in the tasks that ask about the craze repository itself. K3's two reps
beside K3.1 Pro's run:

| task | K3 cost | K3 tool calls | K3.1 Pro cost | K3.1 Pro tool calls | K3.1 Pro result |
|---|---|---|---|---|---|
| T-E1 explain the prompt prefix | $0.38, $0.48 | 17, 19 | $2.11 | 156 | no answer at the run limit |
| T-E2 explain a bad tool call | $0.36, $0.40 | 13, 16 | $1.34 | 117 | passed |
| T-E3 answer a cost question | $0.13, $0.26 | 12, 19 | $0.45 | 66 | passed |
| T-P1 plan a craze feature | $1.07, $0.58 | 32, 27 | $2.16 | 209 | no answer at the run limit |

On these K3.1 Pro reads far more of the repository than K3, several files per request,
and resends a growing context each time: 4.2M and 4.5M input tokens on T-E1 and T-P1. Its
reads were all different, so this is breadth, not a loop. Both runs were cut off by the
run's $3 ledger limit ($2 at list price). The most any other model's run spent was $1.61
on the ledger (K3, T-P1). Whether they would have passed with more money is unknown.

T-E1 has now failed to finish three times out of three. In the main batch its two runs
were stopped by the batch's budget guard after 112 and 132 tool calls, at $1.39 and $1.54.

## What the campaign cannot say

- **Whether K3.1 Pro's answers are better where it does answer.** It may be reading more
  because it is more thorough. Nothing here measures that.
- **How K3.1 Pro behaves at another effort.** It ran at `high`, the catalog's start for
  it. A lower effort might read less; that was not tried.
- **Which model writes the better answer.** With no judge, a passing run by one model and
  a passing run by another count the same. Plan 029 found the objective checks sit near
  the ceiling for every harness, and that is true here: the six models with two full reps
  scored 16 or more of 17, and K3.1 Pro 15.
- **Anything blind about the held-out tasks.** These eight were already read in plan
  029's final. No prompt or tool in craze was tuned from this campaign's results (the
  one change is the Fireworks start model, below), but the set is still due the rotation `eval/RUNBOOK.md` §6 describes before the next tuning loop.

## What it does show

- **The other start models hold up after plans 050 and 051.** Haiku 5.5, Muse Spark
  contributor, GLM 5.3 and GLM 5.3 Flash passed every task at least once, and the misses
  are one rep each.
- **K3.1 Pro does not meet the condition issue #118 set.** It passed fewer tasks than K3
  (15 against 16 and 17) and one pass cost $7.10 against K3's $2.65 to $2.90. Both misses
  and most of the extra cost are on questions about a large repository, the kind of task
  a new session often starts with.
- **`fireworks/deepseek-v4p1-flash-1017` is the cheap Fireworks candidate that finished.**
  It passed 34 of 34 for $1.49, against K3's 33 of 34 for $5.55. It uses more tool calls
  than K3 (median 14 against 10) and writes longer answers (391 words against 238).
  Answer quality was not judged.

## Budget

The cap is counted by the evaluator's ledger, which charges Fireworks models at 1.5 times
list (the US serverless rate). The owner approved $20, then $35 to finish Kimi K3.1 Pro.
The ledger ended at $27.54 committed: $17.18 when the main batch stopped, and $10.36 for
the K3.1 Pro batch. At list price the campaign cost about $19.

Two things went wrong with the first $20, both in the evaluator:

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

## What changed because of it

On 2026-11-14 the owner changed the Fireworks `start` in `catalog.toml` from
`fireworks/kimi-k3p1-pro` to `fireworks/deepseek-v4p1-flash-1017`, on the results above.
Kimi K3.1 Pro stays an ordinary model.

Not run: K3.1 Pro's second rep on 13 tasks, K3.1 Pro at a lower effort, and any judged
comparison of DeepSeek's answers with Kimi's.

## How this record was made

From `eval/`, with `CRAZEEVAL_CAMPAIGN_DIR=~/.craze-eval/2026-11-start-models`:

```shell
uv run crazeeval report --unseal --out $CRAZEEVAL_CAMPAIGN_DIR/report \
  --batch $CRAZEEVAL_CAMPAIGN_DIR/eval-runs/main-20261113-224427 \
  --batch $CRAZEEVAL_CAMPAIGN_DIR/eval-runs/glm-flash-20261114-002047 \
  --batch $CRAZEEVAL_CAMPAIGN_DIR/eval-runs/glm-rest-20261114-011601 \
  --batch $CRAZEEVAL_CAMPAIGN_DIR/eval-runs/glm-rest2-20261114-013744 \
  --batch $CRAZEEVAL_CAMPAIGN_DIR/eval-runs/k31-rest-20261114-021556
uv run crazeeval summarize --report $CRAZEEVAL_CAMPAIGN_DIR/report \
  --campaign-name 2026-11-start-models --out results/2026-11-start-models/summary.json
```

`summary.json` holds the report's own numbers: its `cost $` medians are the ledger's
(1.5 times list for the Fireworks models), where the tables above use list price, and its
`statuses` omit the runs a stopped batch never started. Its medians for Kimi K3.1 Pro
include the two runs that hit the run limit, where the results table counts finished runs
only. The per-run figures in the Kimi tables come from each run's `result.json`. The full archive is in the private repository
`charliek/craze-evals` at `campaigns/2026-11-start-models/`; the raw runs are local.
