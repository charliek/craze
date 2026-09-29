# Plan 029's evaluation campaign (2026-09-27/28)

The durable record of the campaign that tuned craze's native harness against reference
harnesses on the same models and tasks. Read this before comparing a later campaign
against it. See `eval/RUNBOOK.md` for how to run the next one.

## Setup

craze native vs gx (grok 1.0.16+gx.12), opencode 1.18.32 and codex-cli 0.157.1, same model
per run, headless, each run sandboxed (bubblewrap 0.11.0) behind a keyed recording proxy.
15 tasks: 7 dev (used for tuning) and 8 held out until the final. Objective checks plus a
blind pairwise judge (codex gpt-6-sol, astra on split calls; judge hash `163ea21f…`).
Judge calibration: sol's order-flip rate 16.7% (pass, so dev judging is single order),
luna's agreement with sol 66.7% (fail, so luna was not used), padding controls 0/10 fooled.

Builds: B0 = craze at `3eabb31` (the old opencode-derived terse prompt); final = `e18dd28`
(L1 draft 1 + L2 + L3 draft 2); L7 = `feb4f22` (the wrap-up's A/B, base `73ed5e0`).

Models: `glm-5.3`, `glm-5.3-flash` (Z.AI), `fireworks/deepseek-v4p1-flash`,
`fireworks/kimi-k3` (Fireworks), `muse-spark-1.3` and `muse-spark-1.3-contributor` (Meta —
the owner treats them as one model; the final used the contributor). codex has no headless
plan mode (13 of the 15 tasks apply to it) and cannot reach Z.AI; gx and codex were not run
on Kimi (cost; plan 029 amendment X11).

## Success bar

Per §3.1.8: craze meets the bar on a model when, over all 15 tasks **and** over the 8
held-out tasks alone, its objective pass count is ≥ the best open harness's (gx or
opencode, fixed from the baseline) and its win rate is ≥ 50% (both orders judged, ties
half). The final ran the frozen build for 2 craze reps against 1 baseline rep of the open
harness.

| model | best open | objective all / held-out | win rate all 15 (90% CI, n) | held-out | dev | bar |
|---|---|---|---|---|---|---|
| glm-5.3 | gx | 15 vs 15 / 8 vs 8 | 52% (0.37–0.66, 30) | 50% | 54% | meets |
| glm-5.3-flash | gx | 14.5 vs 15 / 7.5 vs 8 | 53% (0.39–0.67, 30) | 62.5% | 43% | misses (objective) |
| deepseek-v4p1-flash | opencode | 14.5 vs 15 / 7.5 vs 8 | 78% (0.64–0.88, 30) | 69% | 89% | misses (objective) |
| kimi-k3 | opencode | 14.5 vs 15 / 7.5 vs 8 | 58% (0.43–0.72, 30) | 59% | 57% | misses (objective) |
| muse-spark-1.3-contributor | opencode | 15 vs 15 / 8 vs 8 | 50% (0.36–0.64, 30) | 44% | 57% | misses (held-out win rate) |

Only glm-5.3 clears every condition. Each objective miss above is one rep of one held-out
task: T-F2's most-negative-size case panicked craze (glm-5.3-flash, kimi-k3); deepseek's
T-P2 hit a degenerate repetition loop to `max_tokens`. Every second rep passed all 15
tasks; T-F2's case recurred on three models (see *Recurring failure modes* below), so it
may be a pattern rather than noise.

A provider-drift check (opencode re-run on the contributor's dev tasks, after a Meta
billing outage mid-campaign): same served model id, 7/7 objective pass, judged 57% against
its own baseline (W3 T2 L2) — no drift found.

## Before → after

Judge win rate of craze vs each reference harness; ties count half. "before" = B0, dev
tasks, single order, n≈7 (n≈14 for Muse, which had two baseline reps). "after" = the final
build's prompt and tools on the dev tasks, single order, n≈14 for deepseek and
glm-5.3-flash, or the final success-bar run over all 15 tasks (starred, n=30). Muse's
"after" is the L2 build (prompt + snapshot, before L3), dev only — the priority model's
tuning stopped there when Meta's API started refusing the key (2026-09-28, billing).

| model | vs gx | vs opencode | vs codex |
|---|---|---|---|
| deepseek-v4p1-flash | 57% → 68% | 79% → 78%* | 50% → 79% |
| glm-5.3 | 43% → 52%* | 93% → (not re-judged) | n/a |
| glm-5.3-flash | 64% → 53%* | 64% → 68% | n/a |
| kimi-k3 | not run | (not judged) → 58%* | not run |
| muse-spark-1.3 | 32% → 75% | 14% → 46–61% (L2 / L1) | 42% → 79% |
| muse-spark-1.3-contributor | 21% → – | 21% → 50%* | 25% → – |

For context, the open harnesses' baseline win rate against each other (gx vs opencode,
dev): deepseek 43%, glm-5.3 79%, glm-5.3-flash 64%, muse-spark-1.3 43%, contributor 29% —
opencode was the harness to beat on Meta and deepseek, gx on GLM.

## Objective passes, all 15 tasks

craze B0 / craze final / gx / opencode / codex:

- deepseek-v4p1-flash: 15 / 14.5 / 15 / 15 / 13 of 13
- glm-5.3: 15 / 15 / 15 / 14 / –
- glm-5.3-flash: 15 / 14.5 / 15 / 14 / –
- kimi-k3: 14 / 14.5 / – / 15 / –
- muse-spark-1.3: 14 / – / 14 / 15 / 13 of 13
- muse-spark-1.3-contributor: 14 / 15 / 14–15 / 15 / 13 of 13

The checks sit near the ceiling for every harness on every model; the judge, not the
objective checks, is what actually separates them.

## Behaviour

Median per run over all 15 tasks: answer words; tool calls.

- deepseek: B0 75 words / 11 tools → final 304 / 14; gx 297 / 14; opencode 77 / 12; codex 128 / 6.
- kimi-k3: B0 53 / 8 → final 224 / 10; opencode 227 / 11.
- glm-5.3: B0 128 / 12 → final 288 / 15; gx 317 / 16; opencode 187 / 11.
- glm-5.3-flash: B0 100 / 10 → final 298 / 14; gx 319 / 13; opencode 155 / 11.
- muse-spark-1.3 (B0 only): craze 24; gx 207; opencode 96; codex 96.

The old (pre-L1) prompt produced the shortest answers of any harness on every model — one
of the things L1 (below) set out to fix.

## Lever history

craze vs its own previous build, dev tasks, 2 reps, sol single order, pooled over the
tuning models (deepseek, glm-5.3-flash, muse-spark-1.3) unless noted. Keep rules: a
conditional lever needs ≥ 55% pooled; an owner-requested lever needs ≥ 45% pooled and no
objective drop.

- **L1** — craze's own system prompt, replacing opencode's old terse default: 74% vs B0
  (62/84) — kept. On muse-spark-1.3 alone: 93% vs B0; vs opencode 14% → 61%, vs gx
  35% → 77%, vs codex 42% → 83%.
- **L2** — a session-start snapshot (date + bounded git state) appended to the frozen
  prompt: 54% vs L1 — kept.
- **L1 draft 2** (sharper explain/plan accuracy text): 38% — rejected; draft 1 stands.
- **L3 draft 1** (todo_write, agent and read tool descriptions): 44% — below the no-harm
  line, redesigned.
- **L3 draft 2** (drops the read tool's "whole file" bullet): 51% — kept.
- **L4** (per-provider request parameters) and **L5** (tool-call repair): no evidence in
  the captures that would license them, so neither was built. **L6** (a Muse-specific
  prompt variant) deferred — L1 alone already won on Muse, and Meta's outage stopped
  further Muse tuning.
- **L7** — the final answer now shows the command and output behind a verification, and
  describes test results exactly rather than generously. A/B on `73ed5e0` vs
  `73ed5e0`+L7, all 15 tasks, 2 reps, three models (muse-spark-1.3-contributor, deepseek,
  glm-5.3-flash): dev 52.4% pooled (W38 T12 L34, n=84; contributor 67.9%, deepseek 44.6%,
  glm-5.3-flash 44.6%), no dev objective drop — kept, provisionally. Held-out 46.4%
  (n=96), but the held-out tasks informed L7's design, so that number is not blind; the
  target task T-V1 went 4-0 (contributor), 4-0 (deepseek), 2-2 (GLM). See `eval/RUNBOOK.md`
  §6 (held-out hygiene) for why the next campaign should rotate its held-out set before
  trusting L7's held-out number further.

## Scoring correction (X15)

A task's scope check originally counted the craze repo's own gitignored build outputs
(`bin/craze` from `make build`) as out-of-scope changes. `crazeeval rescore` corrected 6
runs from their kept manifests (3 of those runs' verdicts were re-judged; the winner didn't
change in any of them). The numbers throughout this README are post-correction.

## Recurring failure modes worth a lever next time

- deepseek's runaway generation on plan-category tasks: four separate `max_tokens` crashes
  across different builds — this reads as a model-side repetition problem, not a
  prompt one; worth a repetition guard in the turn loop regardless of model.
- T-F2's most-negative-size edge case tripped glm-5.3-flash, kimi-k3 and deepseek at
  different points.
- Explanation tasks (T-E1/T-E3) lost single rubric items against opencode specifically on
  Muse.

## Spend

$63.64 of metered API spend committed over the whole campaign, plus $4.74 held in three
reservations whose usage never arrived (never released, per plan 029 §3.1.7's "keeps its
reservation" rule), against a $95 hard stop. Z.AI runs were on the coding plan ($0
metered). The judge ran on the ChatGPT plan (not metered, so not counted above).

## Caveats

Small n (90% confidence intervals are roughly ±15 points at n=30). Only one baseline rep
for the open harnesses (craze got two reps at the final; they didn't). The judge is an
OpenAI model grading every harness, including OpenAI's own reference (codex) — codex is a
reference only, never a bar. 5 of the 15 tasks are about craze's own code, so the suite
leans toward craze's own domain. The held-out set is now spent — rotate it
(`eval/RUNBOOK.md` §6) before the next campaign's tuning loop.
cursor was not run in this campaign.

## What is in this directory

- `runs.jsonl` — one line per scored run: campaign and batch, harness (with version and
  executable hash), the craze build (binary, sha256, source commit) for a craze run, model
  and served model, task/split/category/mode, rep, status, objective pass and each check's
  result, answer words, tool calls, main requests, wall time, cost and list cost, tokens.
  876 rows.
- `verdicts.jsonl` — one line per final verdict: task, model, split, the two run ids and
  harnesses, judge model and mode, winner, confidence, scores, each judged order's rubric
  grades and reasons, `judge_hash` and `task_judge_hash`. 1,156 rows, all 1,156 current
  (bound to the scored runs above). Before archiving, the campaign's raw verdict files
  under the plan folder were stamped with `judge-hash --stamp`: every verdict record got
  its per-task hash, with each original kept as `.pre-stamp.jsonl`. So the raw verdicts
  stay current when tasks are added later, and the archive saw every record already
  carrying its task hash.
- `batches.json` — provenance per batch: name, campaign, label, harnesses, models, reps
  (and `first_rep`), prices, task fingerprints, fixture/evaluator/config-snapshot hashes,
  every executable's version and hash, the craze build, and a copy of that batch's
  `best-open-harness.json` where it has one.
- `calibration.json` — the one calibration run's summary (seed, judge hash, flip rate,
  luna agreement, padding result, gates) — not the raw calibration records.
- `archive.json` — the archive's own manifest: selection mode, file sizes, the safety scan
  result, and the reconciliation line: `876 scored runs selected, 876 rows written; 1156
  final verdicts (1156 current), 1156 rows written — reconciled`.

**Not here, and why:** answers, captures, diffs, logs and workspaces stay outside the repo
(they can be large, and some hold model-generated text that shouldn't sit in git history
indefinitely) — they live at
`~/.claude/plans/craze/029-native-harness-quality/eval-runs/` on the machine that ran the
campaign, alongside the plan document and its `progress.md` execution log.

## How it was produced

36 batches in eval-runs/ went into the archive, in full: base-fw-ds, base-fw-kimi-dev,
base-fw-kimi-ho, base-gxplan, base-meta-contrib, base-meta-craze, base-meta-spark,
base-zai (the baseline); lever-B0r2-{ds,zai}, lever-L1d1-{ds,meta,zai},
lever-L2-{ds,meta,zai}, lever-L2d2-{ds,zai}, lever-L3-{ds,zai}, lever-L3d2-{ds,zai} (the
lever loop); final-{ds,kimi,meta-contrib,zai} and final2-{ds,kimi,zai} (the final, rep1
and rep2); drift-meta-contrib (the provider-drift check); l7a-{contrib,ds,zai} and
l7b-{contrib,ds,zai} (the wrap-up's L7 A/B). Run from `eval/` (see `eval/RUNBOOK.md`) --
`--out results/2026-09-plan029` lands here, at `eval/results/2026-09-plan029`; `--out
eval/results/2026-09-plan029` from `eval/` would land one level too deep, at
`eval/eval/results/2026-09-plan029`.

```shell
E=~/.claude/plans/craze/029-native-harness-quality/eval-runs
args=()
for b in "$E"/base-* "$E"/lever-* "$E"/final-* "$E"/final2-* "$E"/drift-* "$E"/l7a-* "$E"/l7b-*; do args+=(--batch "$b"); done
uv run crazeeval archive --all-runs --unseal --max-bytes 12000000 --out results/2026-09-plan029 "${args[@]}"
```

The command actually used also filtered the glob to only the 36 batch directories that
have a `batch.json` (a batch that never got that far, e.g. aborted or a debug probe, is
not a batch to archive); the loop above passes every directory the glob matches, which
for this campaign's `eval-runs/` happens to be exactly those same 36.

`--all-runs` keeps every run of every batch by its `run_id`, rather than the default
current-view selection (a later batch replacing an earlier one's run by run key) — needed
here because the lever and L7 A/B batches deliberately reuse run keys across builds, and
the default mode would have dropped one side of each comparison along with its verdicts.

See `eval/RUNBOOK.md` for the full procedure this campaign followed, generalized for the
next one.
