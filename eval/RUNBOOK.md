# Running an eval campaign

`eval/README.md` is the reference for how `crazeeval` works. This is the procedure: how
to run a campaign end to end, and how to extend the suite as models change. Every command
below was checked against `eval/crazeeval/cli.py` (`uv run crazeeval <cmd> --help`, run
from `eval/`); if the two ever disagree, trust `--help`.

All commands run from `eval/` as `uv run crazeeval <command> ...`. This doc omits the `uv
run` prefix after the first example.

## 0. Starting the next campaign (checklist)

Short and concrete; each item points at the section that explains it.

- Pick a campaign name `YYYY-MM-<slug>` and `export CRAZEEVAL_CAMPAIGN_DIR=~/.craze-eval/<name>`
  (§1). Set the budget cap (§1).
- Update the reference harnesses (gx, opencode, codex) and note their versions — versions
  are recorded automatically; see §2.
- Update `eval/models.toml` and `eval/prices.toml` for the models to test: add new ones,
  drop retired ones. For Muse use `muse-spark-1.3-contributor` (§3). Then run
  `snapshot-config`, `validate` and `probe` (§2), and a smoke run per new model (§3).
- **Rotate the held-out set before any tuning (§6).** Plan 029's eight held-out tasks
  (T-E3, T-I2, T-B2, T-B3, T-F2, T-M1, T-V1, T-P2) are spent: move them to `split = "dev"`
  and write about eight new held-out tasks. Validate the new tasks with `crazeeval
  validate --tasks <ids>` (§4).
- Build B0 from current `main`, from a commit, not a dirty tree (§5d shows how), and run
  the baseline (§5a).
- Compare against the previous campaign using `eval/results/2026-09-plan029/summary.json`
  and its README. Plan 029's raw runs were deleted on 2026-09-28, so its full record is
  the archive in the private `charliek/craze-evals` repo at `campaigns/2026-09-plan029/`,
  not local batches. The start-model campaign of November 2026
  (`eval/results/2026-11-start-models/`) ran craze alone, with no reference harness and
  no judge: use it for objective passes, cost and tool use per model, not for win rates.
- Re-check the open items the earlier campaigns left:
  - Kimi K3.1 Pro, the Fireworks start model, finished 3 of 17 tasks in November 2026
    before the budget guard stopped the batch; its two T-E1 runs had each cost three to
    four times a whole Kimi K3 run by then. It is not yet shown to be as good as K3.
  - L7 (the verification-evidence prompt bullet) was kept provisionally and needs
    confirming on the fresh held-out set.
  - The deepseek plan-mode runaway: see §7.
  - T-F2's most-negative-size edge case recurred on three models.
  - glm-5.3-flash's dev win rate was 43%.
- At the end: summarize and publish (§5f), and keep only the current and previous
  campaign's raw runs.

A web-split campaign (craze-web against craze on the `web` tasks; craze plan 051) is
smaller and has its own checklist: §9.

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
# Once per campaign, again whenever the owner's model or provider files change, and
# again after building a craze whose shipped catalog changed. Each model's craze fields
# come from craze's shipped catalog with the owner's native models.toml (optional)
# merged over it by craze's own rule, or from the owner's file alone when it says
# `catalog = false`. A model the owner's gx config does not name gets no gx entry, and
# a run of the gx harness on it is refused:
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
and each harness's id for it; the `craze` and `gx` aliases are kept in step by hand, since
craze no longer imports gx's table) and `eval/prices.toml` (ledger and list prices per wire
model, keyed the same way — a wire model missing from `prices.toml` is refused by the
proxy). Both files are commented inline with what each field means; copy an existing
entry and adjust it. Re-run `snapshot-config` afterwards so the new model's provider
config is in the current snapshot.

The `craze` alias must be one craze's shipped catalog
(`internal/harness/modeltable/catalog.toml`) has with the same provider and wire model
(`tests/test_snapshot_view.py` checks every eval model against it, and that each wire
model has a price), or one the
owner's `~/.craze/native/models.toml` defines in full. `snapshot-config` builds the craze
fields as craze does: the catalog's entry, with any owner entry for the alias overriding
the keys it writes (an old gx import's copy of a shipped alias, marked `source = "gx"`, is
ignored), or the owner's file alone when it says `catalog = false`. The owner needs no
`models.toml` at all. The merge is the eval's copy of craze's rule and does not run craze's
validation, so `snapshot-config` prints a `note:` line for each eval model whose entry came
through an owner override: check that `craze models <alias>` shows the same values, since
craze keeps its shipped entry when it rejects an override. The entry must offer the eval's
pinned `effort`, or `snapshot-config` stops.

The `gx` alias is optional on the owner's side. When the owner's gx config
(`~/.grok/config.toml`, `providers.toml`) has no `[model."<alias>"]`, the snapshot records
`gx` as none for that model, `snapshot-config` prints `gx=- (not in the owner's gx
tables)`, and `run` with `gx` among `--harness` stops before starting anything (exit 2),
naming the model, as does `proxy --harness gx`; every other harness runs on it as usual. `probe` builds the homes a run
would, so on such a model it builds no gx home and checks the rest. To run gx, add the
model (and its `[model_providers.*]`) to the owner's gx config and re-run
`snapshot-config`. An alias the gx config does name must still name a provider it defines
and the eval's wire model.

The generated craze homes carry `catalog = false` in `models.toml` (craze plan 031): craze ships
its own model catalog, and without that line every shipped model would join the run and the
agent tool's description. With it, the run's two generated files are the whole table.

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
- **Anthropic** (`anthropic`: Opus, Sonnet and Haiku 5.5; craze plan 047 D11) speaks the
  Messages API, and each model runs at craze's default effort for Claude, `medium`.
  - *The key.* The proxy reads it as craze does: craze's provider is locked, so the
    owner's `[providers.anthropic]` holds a key and, at most, `env_keys`. With no
    `env_keys` written, an exported `ANTHROPIC_API_KEY` wins over the stored key -- check
    which account that is before a campaign. The route is `api.anthropic.com/v1`; it
    forwards only `POST …/messages` and `GET …/models`, with the key in `x-api-key`.
  - *craze* reaches the route through `CRAZE_TEST_ANTHROPIC_API` in the run's environment,
    set to the route without its `/v1` (craze keeps the path and adds `/v1/messages`); the
    generated `providers.toml` names `driver = "anthropic"` and no `base_url`. A live smoke
    through `crazeeval proxy --craze-home` must export the `craze_env` it prints.
  - *opencode* uses its built-in `anthropic` provider; *gx* its `messages` backend
    (`api_backend = "messages"`, which its user guide documents). gx on Anthropic is not
    yet verified live: smoke it before a batch, and leave `gx` out of `--harness` for these
    models if it cannot run. *codex* does not run (no Responses API).
  - *snapshot-config* takes the craze entries from craze's shipped catalog, which ships
    all three, and needs no `[providers.anthropic]` (an exported key is enough). For gx
    to run, the owner's gx config must name the models: a `[model_providers.anthropic]`
    with `base_url = "https://api.anthropic.com/v1"` and `api_backend = "messages"`, and a
    `[model."anthropic/claude-…-5-5"]` per model naming it (`model = "claude-…-5-5"`).
    Without the `[model.…]` table the snapshot has no gx entry for the model and a gx run
    on it is refused; a table that names a provider the gx config does not define stops
    `snapshot-config`.
  - *Accounting.* Cache reads are priced at `cached_input`, cache writes
    (`cache_creation_input_tokens`) at `cache_write`, the 5-minute write -- the only TTL
    craze asks for. A 1-hour write costs 2× input and would be priced as a 5-minute one, so
    before a batch, check one smoke capture's `message_start` events for
    `cache_creation.ephemeral_1h_input_tokens`: if any harness reports non-zero, add a
    1-hour rate first. Haiku 5.5's long-context tier is in `prices.toml`
    (`long_context_above`). A stream cut before its `message_delta` keeps its reservation.
    A request whose billed input its size cannot bound -- a document or image by URL or
    Files API id, a base64 PDF, a hosted fetching tool -- is refused as `unbounded-input`
    on every route (the run's `proxy.flags` say so): no reservation could cover it.
- **Muse Wonder** (`muse-wonder-1.0`, Meta's preview flagship) is set like Spark, at
  `high`. It costs 40× contributor's input and 100× its output, so smoke it on one task and
  budget from that run, not from contributor's.
- **Kimi K3.1 Pro** (`fireworks/kimi-k3p1-pro`) is craze's Fireworks start model and
  Moonshot's successor to Kimi K3; GitHub issue #118 asks for it to be evaluated as that
  start. `fireworks/kimi-k3` stays in `models.toml` as its baseline (craze's catalog marks
  it deprecated, but Fireworks still serves it and craze still runs it by name). Both
  have `last = true`, so they run after the other models in a batch, in the same
  position. K3.1 Pro's prices are two thirds of K3's (`prior_run_cost` 2.00).
- **DeepSeek V4.1 Flash 1017** (`fireworks/deepseek-v4p1-flash-1017`) is DeepSeek's
  2026-10-17 snapshot, which replaces `fireworks/deepseek-v4p1-flash` in craze's catalog
  at the same price. Both stay in `models.toml`; the older one is deprecated in craze's
  catalog and still runs by name.
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

Cost per run varies a lot by model; ground a projection in plan 029's final-report medians
(`eval/results/2026-09-plan029/summary.json`'s `metrics` rows, `cost $` field, and the
results README's *Spend* section) rather than a guess: deepseek-v4p1-flash ran
$0.012–0.017/run across harnesses (craze $0.014); muse-spark-1.3-contributor similarly
$0.011–0.017/run (craze $0.014) — `prices.toml` prices the full muse-spark-1.3 tier
roughly 12–20× higher per token, so budget around $0.15–0.18/run if you use that tier
instead of the contributor one; Kimi K3 ran $0.13–0.14/run, roughly 10× deepseek's rate
(its baseline needed a $6 run cap and was descoped to two harnesses on cost grounds alone
— see §7); `prices.toml`'s `prior_run_cost` for Wonder and the Claude models is
contributor's median scaled by each one's prices, the arithmetic beside each entry.
`crazeeval run` refuses to start a batch whose projected cost — the median cost per run
of that model so far, or `prices.toml`'s `prior_run_cost` with none yet — would pass the
budget cap, so a badly underpriced new model surfaces as a refusal before it burns money,
not after.

## 4. Adding a task

A task is `eval/tasks/<id>/task.toml` plus `eval/testdata/<id>/`. Nothing under `tasks/`
is reachable from inside the sandbox — `task.toml` fields: `id`, `category` (explain,
answer, investigate, bugfix, feature, refactor, multi-step, verify, plan), `split` (dev,
heldout, smoke, web), `mode` (build or plan), `prompt`, `timeout_s`, `rubric`, `[repo]`,
`[[checks]]`, `[validate]`. A web task also declares `harnesses`, `corpus`, `relevant` and
`needs`, is scored by its hidden tests alone, and adds the web lint to its controls: see
`eval/README.md`'s *The web split*.

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
its directory first. Extract into a fresh, empty directory each time: an old extraction
reused would keep a file the commit has since removed or renamed, and build it:

```shell
REPO_ROOT=$(git rev-parse --show-toplevel)
mkdir -p "$REPO_ROOT/bin"
BUILD=$(mktemp -d)
git archive --format=tar HEAD | tar -x -C "$BUILD"
(cd "$BUILD" && go build -trimpath -o "$REPO_ROOT/bin/craze-L1" ./cmd/craze)
rm -rf "$BUILD"
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
- **Runaway generations.** craze on deepseek-v4p1-flash ran away on 5 of 25 plan-mode task
  runs (20%): T-P1 twice, T-P2 three times, across builds with and without L7. Each
  response spun to `max_tokens` at 66–82k output tokens and 9–14 minutes, and the run was
  scored as crashed. craze had 0 runaways on 159 non-plan deepseek runs; gx had 0 of 4
  plan runs and opencode 0 of 2 (samples too small to call either immune); codex has no
  plan mode. During plan 029 craze sent no output-token ceiling for that model —
  `models.toml` had no `max_output_tokens` entry for it, so the provider's own cap
  applied; opencode caps output at min(model limit, 32k). **Since D-74 (2026-09-29)**
  craze sends a default ceiling of 32,000 tokens (a quarter of the window, rounded
  down, on a model under 128k) whenever `models.toml` sets none — see
  `docs/reference/configuration.md`, *Native output ceiling*. That bounds a runaway, it
  does not prevent one: the response can now run to at most 32k output tokens. A
  provider that reports the cut as a `length` finish makes it a `max_tokens` stop, and
  the run still fails (`craze prompt` exits 1 on any stop but `end_turn`), only sooner
  and cheaper. Still open, for the next campaign to decide from evidence: resampling a
  response that hits `max_tokens` without tool calls, and a client-side repetition
  detector. Check whether the runaway persists — plan-mode tasks with 2+ reps on
  deepseek would show it, now as runs stopping at about 32k output tokens (normally as
  `max_tokens`) — and look for the opposite failure too: a legitimate long answer cut at 32k (a `max_tokens` stop with no
  repetition), which means that model needs its own `max_output_tokens`.
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
- **One refused request stops the whole batch.** The ledger admits a request only when
  the committed spend plus every open reservation fits under the cap, and a refusal ends
  the batch for every model in it, not only the one refused. A model with no output limit
  in craze's catalog reserves a worst-case response per request (about $2.92 for
  `fireworks/kimi-k3p1-pro`), and `--cap-other` such requests can be open at once. The
  November 2026 campaign stopped with $2.82 of its $20 unspent, 26 Kimi K3.1 Pro runs and
  every GLM 5.3 Flash run not started. Run a lane per provider, give an expensive model
  its own lane with `--cap-other 1`, and leave headroom of at least three of its
  reservations.
- **A projection made from the smoke runs is far too low.** Once a model has completed
  runs in the ledger, `run` projects from their median, and after only the smoke split
  that median is of two tiny tasks. The November 2026 main batch was projected at $4.24
  and had spent $16.93 when the budget guard stopped it. Budget from the previous
  campaign's per-run medians (§3), and allow for a plan or explain task costing up to
  twelve times a model's median run (Kimi K3's T-P1 against its median, same campaign).
- **A run that makes very many tool calls can be voided.** A run may hold at most four
  open reservations (`MAX_OPEN_RESERVATIONS_PER_RUN`); a request past that is refused and
  the run is scored `budget-capped` even if its answer finishes and passes its checks
  (Muse Spark contributor, T-E1, November 2026: 101 tool calls).
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

## 9. A web-split campaign (checklist)

The `web` split (craze plan 051 E4; `eval/README.md`'s *The web split*) asks one question:
does craze with web search (`craze-web`) solve tasks plain `craze` cannot, and at what
cost? Six tasks (T-W1–T-W6), two arms, two cheap models, three reps: 72 runs, scored by
the hidden tests alone. No baseline against the open harnesses, no lever loop, no judge, no
held-out set. Each item points at the section that explains the command.

- Pick a campaign name `YYYY-MM-web` and `export CRAZEEVAL_CAMPAIGN_DIR=~/.craze-eval/<name>`
  (§1). A fresh campaign has its own ledger, so its cap is this campaign's alone. Every
  `run` below passes `--budget-cap 10 --run-cap 0.50` (the defaults are $95 and $3): $10
  is the hard cap on committed plus reserved spend, and the run cap stops a runaway run
  once it has settled $0.50.
- Build the binary from a **commit** that has craze's web tools (plan 051), never a dirty
  tree, extracted into a fresh, empty directory (§5d):

  ```shell
  REPO_ROOT=$(git rev-parse --show-toplevel)
  mkdir -p "$REPO_ROOT/bin"
  BUILD=$(mktemp -d)
  git archive --format=tar HEAD | tar -x -C "$BUILD"
  (cd "$BUILD" && go build -trimpath -o "$REPO_ROOT/bin/craze-web" ./cmd/craze)
  rm -rf "$BUILD"
  ```

- Pick two cheap models already in `models.toml` and `prices.toml` (§3), for example
  `muse-spark-1.3-contributor` and `fireworks/deepseek-v4p1-flash` (about $0.015 per craze
  run in plan 029). Any provider works: the web tools are craze's own and the proxy answers
  them from the task's corpus, not the provider.
- Snapshot the config, validate the split and probe the sandbox (§2). Each of the six tasks
  must pass all three controls (the web lint, the untouched fixture fails, the reference
  patch passes):

  ```shell
  uv run crazeeval snapshot-config
  uv run crazeeval validate --tasks split:web
  uv run crazeeval probe --craze-bin ../bin/craze-web
  ```

- Smoke one task, one rep, both arms and both models, then read the capture report's
  fidelity block: `web_tools_on_web_arm` must be true (craze-web was offered `websearch`
  and `webfetch` and no other web tool) and `web_tools_absent` true (plain craze was
  offered none). A craze-web run's `result.json` carries the web route's counters under
  `proxy.web`. The smoke also gives each model a median cost per run in this campaign's
  ledger, which the batch's projection uses from then on instead of `prices.toml`'s
  `prior_run_cost` (deepseek's $0.35 prior over 36 runs alone would pass $10, so without
  the smoke the batch is refused before it starts):

  ```shell
  uv run crazeeval run --harness craze,craze-web --model muse-spark-1.3-contributor,fireworks/deepseek-v4p1-flash \
    --tasks T-W1 --reps 1 --craze-bin ../bin/craze-web --budget-cap 10 --run-cap 0.50 --label web-smoke
  uv run crazeeval captures --run <web-smoke-batch>
  ```

- Run the campaign: both arms must be named (`craze-web` is never a default harness, and
  `split:web` is never in the default selection or in `split:all`). `--resume` reopens it
  after a crash; pass the same caps, since they are part of the batch's identity (§7):

  ```shell
  uv run crazeeval run --harness craze,craze-web --model muse-spark-1.3-contributor,fireworks/deepseek-v4p1-flash \
    --tasks split:web --reps 3 --craze-bin ../bin/craze-web --budget-cap 10 --run-cap 0.50 --label web
  uv run crazeeval ledger
  ```

- Report and check fidelity. Scoring is the hidden tests alone: no `calibrate`, no
  `judge-batch`. The report's *Web tasks* section has the pass per model and arm on each
  task and the web route's counters (searches, fetches, misses, bytes served); its success
  bar does not apply to this split:

  ```shell
  uv run crazeeval report --batch <web-batch> --out <web-batch>/report
  uv run crazeeval captures --run <web-batch>
  ```

- Summarize into this repo (§5f step 1). The report is sealed, which here hides nothing (a
  web batch has no held-out runs and no verdicts), so `summarize` needs `--allow-sealed`;
  the result is the summary's `web` object. Then write `results/<name>/README.md` by hand:
  the pass per arm and model, the calls and tokens craze-web spent, the spend, and what
  the campaign cannot show (below):

  ```shell
  uv run crazeeval summarize --report <web-batch>/report --campaign-name <name> \
    --out results/<name>/summary.json --allow-sealed
  ```

- Archive both batches into the private `craze-evals` clone (§5f step 2), commit and push
  `campaigns/<name>/` there and add a row to its README's campaign table; raw runs stay
  local (§5f step 3):

  ```shell
  uv run crazeeval archive --batch <web-smoke-batch> --batch <web-batch> --all-runs --unseal \
    --out ../../craze-evals/campaigns/<name>
  ```

**What it can show, and what it cannot.** It shows whether a model uses the web tools to
solve tasks it cannot solve without them (craze-web's pass rate against plain craze's on the
same tasks and models), and at what cost in calls and tokens. The tools search and fetch a
frozen corpus ranked by BM25, not Exa's index, so it measures tool use and nothing about
Exa's real ranking or results. It measures no harm either: the web tools are offered only
on web tasks, so whether having them makes a model worse on tasks that need no web is not
measured (future work). Plain craze failing a web task is the expected result, not a
regression; a task plain craze passes says its facts leaked or can be guessed.
