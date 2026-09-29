# Running an eval campaign

`eval/README.md` is the reference for how `crazeeval` works. This is the procedure: how
to run a campaign end to end, and how to extend the suite as models change. Every command
below was checked against `eval/crazeeval/cli.py` (`uv run crazeeval <cmd> --help`, run
from `eval/`); if the two ever disagree, trust `--help`.

All commands run from `eval/` as `uv run crazeeval <command> ...`. This doc omits the `uv
run` prefix after the first example.

## 1. What a campaign is

A campaign is one directory holding a series of batches, their budget and their config:

- `eval-runs/` — every batch `run` writes (unless `--out` points elsewhere);
- `ledger.jsonl` — the append-only budget ledger, so the cost cap is per campaign;
- `eval-config/` — config snapshots (`snapshot-config`) and a `CURRENT` pointer.

It's chosen, first match wins:

1. `--campaign NAME` (a global flag, before or after the subcommand) → `~/.craze-eval/NAME`;
2. `CRAZEEVAL_CAMPAIGN_DIR` — a full path;
3. `CRAZEEVAL_PLAN_DIR` — the same thing under its old name;
4. `~/.craze-eval/default`.

Nothing defaults into a plan folder anymore. Pick the campaign once, at the start of the
session, and every command below stays consistent with it automatically:

```shell
export CRAZEEVAL_CAMPAIGN_DIR=~/.craze-eval/2026-10-glm
```

The alternative is passing `--campaign NAME` on every single command instead — a global
flag, before or after the subcommand. The two aren't interchangeable per command: pick
one for the whole session. This runbook uses the environment variable and omits
`--campaign` from every command from here on; if you use `--campaign` instead, add it to
every command below, including `snapshot-config`, `validate`, `probe`, every `run`,
`calibrate`, `judge-batch`, `report`, `rescore`, `ledger` and `archive` call — a command
that omits it silently falls back to `~/.craze-eval/default` and won't see this
campaign's config snapshot or ledger.

Start a new campaign by snapshotting config into it first — an unset campaign has no
config and an empty ledger:

```shell
uv run crazeeval snapshot-config
uv run crazeeval run --harness craze,opencode --model glm-5.3-flash --tasks split:smoke --craze-bin ../bin/craze --label smoke
```

Raw runs (answers, captures, diffs, logs, workspaces) stay in `eval-runs/` — never
published anywhere, local only (keep the current and previous campaign; older ones can be
deleted, optionally after `crazeeval pack`). What's worth keeping goes two places: a
compact rollup (`crazeeval summarize` + a README) in this repo under
`eval/results/<campaign-or-label>/`, and the full `crazeeval archive` in the private repo
`charliek/craze-evals` (§5f).

One budget is enforced per campaign: `--budget-cap` (default $95 committed + reserved) and
`--run-cap` (default $3 settled per run). `crazeeval ledger` prints the running totals.

## 2. Before you start

Prerequisites: `bwrap` (bubblewrap ≥ 0.11), `uv`, and the reference harnesses on `PATH` —
`gx`, `opencode`, `codex` — plus `go` and `node` for the toolchains the sandbox binds in.
Plan 029 ran gx 1.0.16+gx.12, opencode 1.18.32, codex-cli 0.157.1; a version drift between
campaigns is expected and gets recorded automatically (each batch's `manifest.json`, and
`batches.json` in an archive, carry every executable's version and sha256).

Provider keys live in the owner's `~/.craze/native/providers.toml` — the proxy reads them
at startup and never writes or forwards them anywhere else. Nothing under `~/.craze`,
`~/.grok`, `~/.config/opencode`, `~/.local/share/opencode` or `~/.codex` is ever copied or
written by the eval; a campaign only ever reads them.

```shell
# Once per campaign, and again whenever the owner's model tables change:
uv run crazeeval snapshot-config

# Every task's category controls must pass before the task may run:
uv run crazeeval validate

# Proof the sandbox hides the owner's files, eval/tasks and every key, and that the relay
# reaches the proxy — run this once per campaign as a smoke check, and again after any
# sandboxing change:
uv run crazeeval probe --craze-bin ../bin/craze
```

## 3. Adding a model

Edit `eval/models.toml` (the target's provider, wire model, one pinned reasoning effort,
and each harness's id for it) and `eval/prices.toml` (ledger and list prices per wire
model, keyed the same way — a wire model missing from `prices.toml` is refused by the
proxy). Both files are commented inline with what each field means; copy an existing
entry and adjust it. Re-run `snapshot-config` afterwards so the new model's provider
config is in the current snapshot.

Per-harness support limits to know about before adding a model:

- **codex** only runs where the provider serves the Responses API (Meta, Fireworks in
  plan 029's set; not Z.AI) — leave its `codex`/`codex_provider` fields out of
  `models.toml` for a provider it can't reach, and codex is simply not run for that model.
  codex also has no headless plan mode, so it never runs plan-category tasks.
  It knows only its own bundled OpenAI models, so it runs every eval model on fallback
  metadata (a warning in its output, not a failure) — see `eval/README.md`'s *codex model
  metadata* note. It is always a reference, never a bar.
- **gx** has no working headless plan mode either (X8/X12): its plan-category tasks run
  with `--always-approve` and a "plan, no changes" prompt instead of `--permission-mode
  plan`, recorded as `plan_mode: "prompt-only"` in `result.json`. The no-writes check still
  applies, so this is still a meaningful measurement — just not literally gx's plan UI.
- **Z.AI** (`zai-coding-plan`) has no Responses API, so codex never runs on it, and it's a
  flat coding-plan subscription: `zero_rated = true` in `prices.toml`, $0 against the
  budget cap, with its list-equivalent price reported alongside for comparison.
- If a provider offers two tiers of what's functionally the same served model (plan 029's
  Meta case: `muse-spark-1.3` and a much cheaper `muse-spark-1.3-contributor`), and a
  baseline run shows the same served model id and near-identical objective counts across
  harnesses, treat them as one model and prefer the cheaper one for lever tuning and the
  final — cite the baseline comparison when you do.

Before running anything real, smoke it: one task, one rep, every harness that should
support the new model:

```shell
uv run crazeeval run --harness craze,gx,opencode,codex --model <new-model> --tasks split:smoke --reps 1 --craze-bin ../bin/craze --label smoke-<new-model>
```

Cost per run varies a lot by model; use plan 029's measured baseline medians as a rough
guide when projecting a batch's cost (ledger prices, `progress.md`/X11): deepseek-v4p1-flash
$0.03–0.08/run, muse-spark-1.3 $0.08–0.17/run depending on harness (craze ran ~$0.15/run),
Kimi K3 roughly 10–50× deepseek's rate (its baseline needed a $6 run cap and was descoped
to two harnesses on cost grounds alone — see §7). `crazeeval run` refuses to start a batch
whose projected cost — the median cost per run of that model so far, or `prices.toml`'s
`prior_run_cost` with none yet — would pass the budget cap, so a badly underpriced new
model surfaces as a refusal before it burns money, not after.

## 4. Adding a task

A task is `eval/tasks/<id>/task.toml` plus `eval/testdata/<id>/`. Nothing under `tasks/`
is reachable from inside the sandbox — `task.toml` fields: `id`, `category` (explain,
answer, investigate, bugfix, feature, refactor, multi-step, verify, plan), `split` (dev,
heldout, smoke), `mode` (build or plan), `prompt`, `timeout_s`, `rubric`, `[repo]`,
`[[checks]]`, `[validate]`.

**Choosing checks by category** (see `eval/README.md`'s *Tasks* section for each check
type's exact semantics):

- explain / answer / plan → `facts` (regexes over the answer) + `no_writes`;
- bugfix → `tests` (hidden + trusted, a real runner: pytest or go) + `diff_scope`;
- feature → `tests` + `test_discrimination` (the agent's own new tests must fail on the
  original implementation and pass on the agent's);
- refactor → `structural_count` and/or `shared_helper` (an AST check that one function is
  the single extraction point, never run) alongside `tests`;
- verify → `executed_code` (a shell call matching a pattern, whose result was actually
  sent back to the model, showing required evidence — without `evidence` a call like
  `python --version` would pass for free).

Each rubric item is something the judge grades, not just a regex; write 7–9 checkable
items per explain/answer/plan task and 2–3 `false_claims` the judge must treat as false if
the answer asserts them (plausible-sounding but wrong claims about the code, written from
reading it at the task's commit).

**Fixtures vs craze-repo tasks.** A fixture task (`[repo] kind = "fixture"`, `name = ...`)
copies `eval/fixtures/<name>/files/` into a fresh git repo. A craze-repo task (`[repo] kind
= "craze"`) materialises the craze repository itself at a commit — default `3eabb31`, or
pin `[repo] commit` (a full 40-char sha) to write a task against newer craze code. A commit
whose history ever held `eval/` is materialised **without** `eval/` (a parentless,
history-less tree) — so a task can safely pin a recent commit without exposing the eval's
own hidden tests and reference answers to the very harness being scored on them. Pinning a
commit changes the task's fingerprint (and so its identity in `--resume` and in a batch's
`craze_commits`); `tests/test_workspace_checks.py` checks that `CRAZE_REPO_IGNORES` (used
by `no_writes`/`diff_scope` for a craze-repo task) still matches that commit's
`.gitignore`, so pinning a commit with a different `.gitignore` fails that test until the
list is updated.

**Planting a bug** for a bugfix task on the craze repo itself: `[repo] setup_patch` applies
a patch and folds it into one orphan commit with no history, so nothing in `git log` or
`git show` reveals what was planted or what it replaced.

**`[validate]`** wires `crazeeval validate` (per category, see `eval/README.md`): a
reference-patch/reference-answer must pass and an untouched-workspace/decoy-answer must
fail, so a task that can't actually discriminate correct from wrong never runs for real.
Run it whenever a task or its testdata changes:

```shell
uv run crazeeval validate --tasks <new-id>
uv run crazeeval validate   # everything, before a batch that includes it
```

**Dev vs held-out.** Tasks in `split = "dev"` are what the lever loop tunes against and are
freely read at any time. Tasks in `split = "heldout"` are written to `<batch>/heldout/` and
never read by any report or judge call used for tuning until `--unseal` — that's what makes
the final's held-out numbers meaningful. Don't add a new task straight into `dev` if you
want it to eventually serve as a held-out check; see §6 for rotating the split.

**Adding or changing a task invalidates old verdicts for it, silently, unless you stamp
them first.** A verdict's `task_judge_hash` is what decides whether it still stands; an
older verdict recorded with only the global `judge_hash` reads as current only while that
global hash is untouched — which breaks the moment any task changes. Before adding or
changing a task, stamp every existing verdict/calibration file so each record keeps its own
task's hash and survives the edit:

```shell
uv run crazeeval judge-hash --stamp <batch>/judging/verdicts.jsonl <batch>/judging/heldout/verdicts.jsonl ...
```

It's idempotent (a second run changes nothing) and non-destructive (the original is kept
once as `<name>.pre-stamp.jsonl`); `--dry-run` counts without writing. Plan 029 stamped its
campaign's verdict and calibration files this way ahead of archiving, so a future campaign
adding or changing a task never orphans this one's record of what happened.

## 5. The campaign loop

**a. Baseline** — every harness × model × all tasks, 1 rep (2 for the priority model, if
budget allows):

```shell
uv run crazeeval run --harness craze,gx,opencode,codex --model <models> --tasks split:all --reps 1 --craze-bin ../bin/craze-B0 --label base-<lane>
```

Run a lane per model/provider group so a slow or capped provider doesn't block the rest
(plan 029 ran one lane per provider: Z.AI, Fireworks, Meta). `--cap-zai`/`--cap-other`
bound concurrency per provider; `--parallel` bounds it overall. `--resume` reopens a batch
directory (its identity must match) and runs only what has no final result yet — safe to
re-invoke after a crash or a provider outage.

Then run a first report against the baseline. Sealed like this (no `--unseal`), it only
reads dev tasks, so it's a provisional best-open-harness choice per model for early
sanity — it does **not** write `<baseline-batch>/best-open-harness.json` yet (see
`report.py`'s `fixed_best_open_harness`). That record is written by the first
**unsealed** `report --baseline <baseline-batch>` call that names this baseline — in
practice the final's report (§5e) — chosen from the baseline batch alone on all fifteen
tasks, and it's frozen from then on (a later re-run, e.g. a provider-drift check, reads
the recorded choice and can't move the bar underneath the final). Don't unseal early to
force the freeze sooner: an unsealed report reads the held-out results, so it must wait
until tuning is over (§6):

```shell
uv run crazeeval report --batch <baseline-batch> --baseline <baseline-batch> --out <baseline-batch>/report
```

**b. Judge calibration**, once per campaign (or after any judge/rubric change):

```shell
uv run crazeeval calibrate --batch <baseline-batch>
```

Read the gates in `<batch>/calibration/calibration.json`: `flip_gate` (≤ 20% order-flip →
dev judging can stay single-order; otherwise every subsequent `judge-batch` call is forced
to both orders), `luna_gate` (≥ 80% agreement with sol → luna may judge bulk pairs; fail
and luna is unused), `padding_gate` (the judge preferred a padded answer at most 2/10 times
→ the instruction stands as written; fail and revise the instruction before freezing and
re-run calibration). `judge-batch` reads this file (`--calibration`, default
`<batch>/calibration/calibration.json`) and enforces it automatically; pass a specific
baseline's calibration file explicitly when judging a lever build so its craze-vs-craze
comparison uses the same gates.

**c. Baseline judging** (dev tasks, single order if the flip gate passed), then a report:

```shell
uv run crazeeval judge-batch --batch <baseline-batch> --x craze --y gx,opencode,codex --tasks <dev-task-ids> --judge sol --seed 29 --calibration <baseline-batch>/calibration/calibration.json
uv run crazeeval report --batch <baseline-batch> --baseline <baseline-batch> --losses --out <baseline-batch>/report
```

**d. The lever loop**, one change at a time. Build the candidate from a **commit**, never
a dirty tree (so the build is reproducible and its provenance is a real sha), to exactly
the path the run command below names (`../bin/craze-L1`, relative to `eval/`), creating
its directory first:

```shell
REPO_ROOT=$(git rev-parse --show-toplevel)
mkdir -p "$REPO_ROOT/bin"
git archive --format=tar HEAD | (mkdir -p /tmp/build-L1 && tar -x -C /tmp/build-L1)
(cd /tmp/build-L1 && go build -trimpath -o "$REPO_ROOT/bin/craze-L1" ./cmd/craze)
```

Run the dev tasks, 2 reps, on the tuning models (plan 029 used the free/cheap ones: the
priority model, one free provider, one cheap provider):

```shell
uv run crazeeval run --tasks split:dev --harness craze --model <tuning-models> --reps 2 --craze-bin ../bin/craze-L1 --label lever-L1-<lane>
```

Judge craze-vs-craze against the previous build (not against the open harnesses — a
lever's keep decision is about craze's own delta):

```shell
uv run crazeeval judge-batch --batch <L1-batch> --batch-y <previous-batch> --x craze --y craze --tasks <dev-task-ids> --judge sol --seed 29 --calibration <baseline-batch>/calibration/calibration.json
```

**Keep rules** (§3.2 of the plan; pooled across the tuning models): a conditional lever, or
a draft replacing an earlier draft, is kept at pooled win rate ≥ 55% with no tuning model's
dev objective count dropping; an owner-requested lever is kept unless pooled win rate < 45%
or an objective count drops (in which case redesign it, within a 3-draft limit, rather than
dropping it silently). `report --losses` on the lever batch surfaces the tuning signal for
the next draft — missed rubric items and the judge's reasons for each lost pair, dev tasks
only:

```shell
uv run crazeeval report --batch <L1-batch> --compare <previous-batch> --compare-verdicts <L1-batch>/judging --losses --out <L1-batch>/report
```

**e. The final.** Freeze the build (best-scoring kept draft of each lever), run all fifteen
tasks, 2 reps, on every model:

```shell
uv run crazeeval run --tasks split:dev,split:heldout --harness craze --model <all-models> --reps 2 --craze-bin ../bin/craze-final --label final-<lane>
# adding a rep later to an already-scored batch, if budget opens up afterwards, without
# re-running what's already there: number the new rep from N instead of 1.
uv run crazeeval run --tasks split:dev,split:heldout --harness craze --model <all-models> --reps 1 --first-rep 3 --craze-bin ../bin/craze-final --label final3-<lane>
```

Judge success-bar (both orders by sol; astra re-judges a disagreement or a low-confidence
verdict) against the model's best open harness, held-out included — the candidate is
frozen, so unsealing it is safe now:

```shell
uv run crazeeval judge-batch --batch <final-batch> --batch-y <baseline-batch> --x craze --y <best-open-harness> --success-bar --seed 29 --calibration <baseline-batch>/calibration/calibration.json
uv run crazeeval report --batch <baseline-batch> --batch <final-batch> --verdicts <final-batch>/judging --verdicts <final-batch>/judging/heldout --baseline <baseline-batch> --unseal --out <final-batch>/report
```

This `report` call is the first **unsealed** one naming `<baseline-batch>`, so this is
where `<baseline-batch>/best-open-harness.json` actually gets written (§5a) — chosen from
the baseline batch alone, on all fifteen tasks, and frozen from here on.

The report's success-bar section reads as `meets`, `misses` (with the gap) or
`inconclusive` (coverage incomplete — some task never got a scored run and a bound verdict
on both sides) per model, on all fifteen tasks and again on the held-out eight alone. If
≥ $15 of budget remains, re-run the best open harness on the priority model's dev tasks as
a provider-drift check (§3.1.8) — same served model id, judged against its own baseline —
before trusting the final numbers on a provider that had an outage mid-campaign.

**f. Summarize and publish.** Once the campaign is done, three steps:

1. **Summarize**, into this repo — a compact `summary.json` from the campaign's final
   `report` output directories (one group per report), plus a hand-written `README.md`
   (see `eval/results/2026-09-plan029/README.md` for the shape):

   ```shell
   uv run crazeeval summarize --report <final-report-dir-1> [--report <final-report-dir-2> ...] \
     --campaign-name <campaign-name> --out results/<campaign-name>/summary.json
   ```

   (Run from `eval/` — `--out results/<campaign-name>/summary.json` lands at
   `eval/results/<campaign-name>/summary.json` in the repo.)

2. **Archive**, into a local clone of the private `craze-evals` repository — the full,
   uncompacted record (`runs.jsonl`, `verdicts.jsonl`, `batches.json`, `calibration.json`,
   `archive.json`) of every batch in the campaign:

   ```shell
   git clone git@github.com:charliek/craze-evals.git ../../craze-evals   # once, next to the craze checkout
   uv run crazeeval archive --batch <baseline-batch> --batch <lever-batch-1> ... --batch <final-batch> \
     --all-runs --unseal --max-bytes <N> --out ../../craze-evals/campaigns/<campaign-name>
   ```

   Use `--all-runs` for a whole campaign (every scored run of every batch, kept by
   `run_id`) — the *default* mode instead keeps only the report's current view (a later
   batch replaces an earlier one's run by run key), which silently drops one side of a
   craze-vs-craze lever comparison, since both batches reuse the same run keys. Plan 029's
   campaign archived with `--all-runs --unseal --max-bytes 12000000` over 36 batches
   (baseline, lever, final, final2, the drift check, the L7 A/B batches) into 876 runs and
   1,156 verdicts at 2.9 MB — set `--max-bytes` generously for a campaign-sized archive;
   the 2 MB default is sized for a single final's current view. `archive.json`'s
   `reconciliation` line must say `ok: true`; the command also fails loudly if it finds a
   key or a home path anywhere in the output. Then, in the `craze-evals` checkout, commit
   and push `campaigns/<campaign-name>/` and add a row to that repository's own README
   campaign table.

3. **Raw runs stay local.** Never uploaded, anywhere. Keep the current and previous
   campaign's `eval-runs/` directory; delete older ones. `crazeeval pack` can compact a
   kept campaign into a local tarball first (still local, never a publish step) — see
   "Where results go" in `eval/README.md`.

## 6. Held-out hygiene

The held-out split is spent the moment its results inform a change — reading a held-out
verdict to decide a lever is exactly what "held-out" means to prevent. Once a final has
used the held-out set (even just to report the bar, since a miss there shapes what gets
tried next), rotate it before the next tuning loop: move the tasks that were exposed into
`dev`, and write new tasks into `heldout` to replace them. Plan 029's L7 (wrap-up W1) is the
example this generalizes from — plan 029's amendment X20 (wrap-up W1) reported L7's
held-out numbers as informational only, precisely because those tasks had already been read.

## 7. Known behaviours and pitfalls

- **`rescore`** recomputes a finished batch's diff-derived checks (`no_writes`,
  `diff_scope`) from its kept manifests with today's ignore rules, without re-running
  anything: use it after a scoring-rule fix (plan 029's X15: the craze repo's own
  gitignored build outputs — `bin/craze`, `tests/cli/.venv/` — were wrongly counted as
  out-of-scope changes). It's per batch and needs `--unseal` to touch held-out runs;
  `--dry-run` previews without writing. A corrected run keeps its original as
  `result.pre-rescore.json`, and any judged pair whose objective result flipped should be
  re-judged (the judge packet shows the objective result, so a stale one is misleading).
- **Runaway generations** on some models (plan 029 saw it on deepseek's plan-category
  tasks) can spin to `max_tokens` in a repetition loop — a model failure, objectively
  scored as a crash, not a prompt problem; don't chase it as a lever candidate without
  more than one sighting.
- **The judge ranks correctness over length**, and calibration's padding control exists to
  catch a regression there — if a future padding gate fails, the instruction needs revising
  and calibration re-run before any bulk judging counts.
- **Single-order vs both-order judging** is decided by the flip gate, per campaign — don't
  assume single-order dev judging still holds after a judge or rubric change without
  re-running `calibrate`.
- **Provider outages happen mid-campaign** (plan 029 hit a Meta 402 billing lapse) — probe
  the provider with one smoke run before committing a lane's budget to it, and use the
  provider-drift check (§5e) rather than trusting a final run over an outage window.
- **Budget reservations that never settle** (a disconnected request whose usage never
  arrives) keep their conservative reservation against the cap forever — `crazeeval ledger`
  shows this as committed-but-unreconciled spend; it's expected, not a bug to chase.
- **A batch directory is exclusive.** `run` refuses to reuse an existing `--out` unless you
  pass `--resume`, and `--resume` only proceeds if the batch's identity fingerprint still
  matches (task definitions, testdata, fixtures, the config snapshot, model settings and
  prices, every executable's hash and version, timeouts and caps, the evaluator's own code).
- **Batch names carry timestamps for a reason.** Two batches sharing a label across
  different campaigns (or re-runs) can confuse which verdicts bind to which runs if you
  pass a stale directory into `report`/`archive` by hand — always resolve the actual
  timestamped directory, don't guess a label.

## 8. Plan 029 as a worked example

Plan 029's own campaign (baseline → four levers → the final, plus the wrap-up's L7 A/B) is
summarized in the repo at `eval/results/2026-09-plan029/README.md` and `summary.json`; its
full archive is in the private `craze-evals` repository at
`campaigns/2026-09-plan029/`. It's a concrete instance of every step above, with the actual
numbers, keep/reject decisions and spend — read it for what a full campaign's shape looks
like end to end.
