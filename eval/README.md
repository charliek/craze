# crazeeval: the craze native harness evaluation

`eval/` is plan 029's evaluation harness. It runs craze native and three reference
harnesses on the same models and the same tasks: gx (the grok-build fork), opencode and
codex. It records what each one sends on the wire and scores the results. It is not part
of CI or the per-commit gate. It costs money, and its runs are not deterministic.

See `eval/RUNBOOK.md` for the procedure -- a campaign end to end, and how to add a model or
a task.

It is a Python ≥ 3.11 `uv` project, package `crazeeval`. Run every command from this
directory:

```shell
uv run crazeeval <command>
uv run pytest            # the offline tests: no network, a few seconds (bubblewrap tests skip without it)
```

`eval/go.mod` is a module with no code. It keeps every Go file under `eval/` (fixture
sources, hidden tests) out of the root module. So `go list ./... | grep -c /eval/` from the
repo root prints 0, and `tests/test_sandbox_boundary.py` asserts it.

## Campaigns

A **campaign** is one directory holding a series of batches, their budget and their
config: `eval-runs/` (every batch `run` makes without `--out`), `ledger.jsonl` (the
budget ledger, so the $95 cap is per campaign) and `eval-config/` (the config snapshots
and `CURRENT`). It is chosen, first match wins, by:

1. `--campaign NAME`, a global option (before or after the command):
   `~/.craze-eval/<NAME>` (letters, digits, `.`, `_`, `-`);
2. `CRAZEEVAL_CAMPAIGN_DIR`: a full path;
3. `CRAZEEVAL_PLAN_DIR`: the same, under its old name (plan 029's scripts set it to the
   plan folder, so they keep working);
4. `~/.craze-eval/default`.

Nothing defaults into a plan folder. A new campaign starts with no config snapshot (run
`snapshot-config` in it first) and an empty ledger. `run` prints the campaign it uses,
and each batch's `batch.json` records it (`campaign`, outside the identity fingerprint,
so `--resume` is unaffected). Caches shared by every campaign -- the craze templates, the
opencode seed, the judge's `CODEX_HOME` -- stay under `~/.cache/crazeeval`
(`CRAZEEVAL_CACHE_DIR`).

```shell
uv run crazeeval --campaign 2026-10-glm snapshot-config
uv run crazeeval --campaign 2026-10-glm run --harness craze,opencode --model glm-5.3-flash ...
```

## How a run works

```
harness (in bwrap, its own netns) ──> 127.0.0.1:<port> ──relay──> proxy.sock ──> https://<upstream-host>/<prefix>/...
     base URL http://127.0.0.1:<port>/r/<token>/<upstream-host>/<prefix>    the recording proxy: the only holder of keys
```

1. **Workspace.** The run's workspace is materialised into an independent object store:
   no remote, no alternates.
   - craze tasks start from a template built once per commit with `git init` and
     `git fetch --no-tags <repo> <commit>` (only that commit's ancestry), cached as
     `~/.cache/crazeeval/templates/craze-<sha12>`. The commit is the task's
     `[repo] commit`, default `3eabb31`. A task with a `setup.patch` instead gets a
     **fresh object store holding one parentless commit** of the patched tree (plan X7):
     `git cat-file -e <commit>` fails, `git rev-list --all` is one commit, every stored
     object is reachable from it -- nothing in git shows what was planted or what it
     replaced.
   - A commit whose history ever held `eval/` -- its own tree or any ancestor's, even if a
     later commit removed it (`git rev-list --full-history <commit> -- eval` is not
     empty: every commit since the eval landed) -- is never materialised with it: `eval/`
     holds every task's hidden tests and reference answers. Its template
     (`craze-<sha12>-noeval`) is one parentless commit of the tree without `eval/`, with
     no history (the history holds `eval/`); the isolation check adds `hidden_absent`.
     The history is checked on every materialisation. `3eabb31`'s whole history predates
     `eval/`, so its tasks keep their history, exactly as before.
   - Fixture tasks copy `fixtures/<name>/files/` and commit it with a fixed author and date.
   - The start manifest is saved: every entry except `.git/`, classified by `lstat` --
     `f:<mode>:<sha256>` for a file (full permission bits, e.g. `0644`), `o:<mode>:<bytes>`
     for a file over 50 MiB (never partly fingerprinted), `l:<target>` for a link,
     `s:<kind>` for a FIFO/socket/device (never opened).
2. **Route.** The proxy opens a route for the run: a random 32-hex token that stops working
   when the run ends, with an allowlist holding the run's one target wire model.
3. **Home.** A home is generated for the harness from field allowlists (below). It holds
   only the dummy key `craze-eval-dummy-key`, only the target model, and the proxy route.
4. **Sandbox.** The harness runs under **bubblewrap**:
   - `--die-with-parent --unshare-pid --unshare-ipc --unshare-uts --unshare-net --new-session --clearenv`
   - its **own network namespace with no route out**: the proxy's Unix socket is bound in,
     and a relay (`crazeeval/relay.py`, run by the sandbox's python) listens on
     `127.0.0.1:<port>` and starts the harness as its child. The proxy is the only thing
     reachable; no resolver socket is bound, so DNS fails too;
   - `/usr` and `/etc` read-only; the toolchains the harness needs read-only under
     `/sandbox/tools`;
   - the owner's `GOMODCACHE` read-only, and the run's **own** writable Go cache
     (`attempt-<n>/gocache`, deleted after the key scan). Every scoring and validation
     invocation -- they run submitted or task code too -- likewise gets its own empty Go
     cache, deleted when it ends: nothing writable is shared between runs, scoring runs
     or validation;
   - the workspace at `/sandbox/work/<repo>` and the temp home at `/sandbox/home`, both
     writable;
   - opencode's config-dir packages from the offline seed (below), read-only;
   - Nothing of `/home` exists inside, and `/usr/local/bin` is masked.

   The environment is built from nothing. It is checked before launch: a variable that
   holds a key, or whose name matches `KEY|TOKEN|SECRET` (the dummy variable aside),
   refuses the launch. The timeout (20 min by default) kills bwrap, and the PID namespace
   takes every descendant with it, `setsid` children included. Afterwards the runner
   asserts quiescence.
5. **Scoring.** After the run -- and every host-side touch of the agent's tree goes
   through `safefs`: paths walked from a directory fd with `O_NOFOLLOW` at every
   component, only regular files opened (`O_NONBLOCK`, so a FIFO cannot block), writes
   that unlink first and never land through a link, bounded reads:
   - the answer is extracted from the harness's own output, with its explicit terminal
     success or failure (a run is `ok` only on a successful completion);
   - the final manifest is compared with the start manifest (ignore patterns apply to
     additions only; for a craze-repo task they include the craze repository's own
     `.gitignore`, so the build outputs verifying leaves -- `bin/craze`,
     `tests/cli/.venv/` -- are no change, as git and `diff.patch` never show them either;
     a leading `/` anchors a pattern at the workspace root, as in gitignore). A file over
     **50 MiB** or a workspace over **500 MiB** (apparent
     sizes) fails the run as `oversize` before anything is copied or hashed further;
     scoring copies enforce the same limits while copying, and log tails are read by
     seeking;
   - **post-run git** (`gitpost.py`) runs in a network-less sandbox with the workspace
     read-only: the diff in a fresh repository of ours (the start commit from the trusted
     pristine copy's objects, the agent's `.git/config` never read, in-tree
     `.gitattributes` ignored, no textconv/fsmonitor), agent commits read with the same
     overrides; no git command ever touches an agent repository on the host. The evidence
     is accepted only when that sandbox exits 0 in time and the diff succeeded; otherwise
     the attempt is an infrastructure failure (a file git cannot index -- an embedded
     repository with no commit -- is skipped and noted, not fatal);
   - the checks run in a network-less sandbox on a scoring copy;
   - the capture is scanned for contamination;
   - every file of the run is grepped for every loaded key.

   Infrastructure failures (a 429 or 5xx last, a sandbox that did not start or did not go
   quiet, post-run git evidence that could not be made) are re-run up to twice; a
   contaminated run once.

### Keys

- The proxy loads each provider's key from the owner's `~/.craze/native/providers.toml`
  with craze's rule: the first non-empty `env_keys` variable, else `api_key`.
- It keeps keys in memory only (`KeyRing`, whose `repr` shows none) and puts one in
  `Authorization` on the way out.
- It **refuses to forward** any request that carries a loaded key in its raw body, in any
  decoded JSON string (a key sent `\u`-escaped), in the body it would actually forward
  (after usage injection re-serialises it), or in its path, query or headers. The run is
  then marked `key-exposure`, and `proxy.key_exposure_refusals` in `result.json` counts
  the refusals.
- Responses reach the harness **redacted**: a header or HTTP reason phrase holding a key
  is dropped, and the body is streamed through a redactor that holds back only a tail that
  could be the start of a key split across chunks.
- Every capture and ledger line is scrubbed of every key before it is written.
- `result.json`'s `key_scan` is a grep of the run directory. `crazeeval keyscan <dir>` does
  the same for any directory. Both print paths and counts only.
- The owner's files are read-only inputs: nothing under `~/.craze`, `~/.grok`,
  `~/.config/opencode`, `~/.local/share/opencode` or `~/.codex` is ever written.

**Accepted residuals** (review r2-c1; recorded, not closed):

- Key checks match literal and JSON-escaped spellings only. A response carrying a key
  JSON-escaped, or reassembled from separate SSE deltas, would reach the harness, and a
  request carrying a key base64-encoded or split across two JSON strings would be
  forwarded. Agents cannot obtain keys inside the sandbox -- no key file, no key variable,
  no route but the proxy -- so these matter only if a provider echoes our key back in a
  transformed form.
- Scoring runs the submitted implementation in the same process as the test runner (a
  test imports the code it tests), so deliberately adversarial code could forge the JUnit
  report or the `go test -json` events. The runners close the cheap routes (workspace off
  `sys.path` at start-up, no config file from the tree, pytest hooks and package
  initializers in trusted test directories replaced, agent `TestMain` rejected, every
  expected test id required to pass) and every scoring run is isolated from every other;
  in-process forgery by submitted code is accepted for non-adversarial agents.

### What the proxy forwards

- **Allowed requests:** `POST …/chat/completions`, `POST …/responses` and `GET …/models`,
  only to the upstreams in the route table (`meta`, `zai-coding-plan`, `fireworks`,
  `openrouter`, each a host plus a path prefix).
- **Refused and recorded:** anything else, a model other than the run's target, a wire
  model missing from `prices.toml`, more than one completion (`n` or `best_of` other than
  1), an unusable output limit, and a request whose route closed while its body was being
  read (checked again just before admission, with no await in between). Refusals are
  recorded in the capture; unattributable ones go to `<batch>/proxy-refusals.jsonl`.
- **Streaming:** SSE passes through unbuffered. When a streaming chat request lacks
  `stream_options.include_usage`, the proxy adds it and records `usage_injected`.
  Responses-API streams report usage from `response.completed`.
- **Headers:** only `content-type` is ever written. Redirects are not followed, and
  hop-by-hop and `Content-Encoding` headers are dropped.
- **Capture:** a journal. A forwarded request writes a `start` line (the request) before
  it goes upstream, an `events` line per batch of streamed SSE events as they pass, and
  an `end` line with status, usage and accounting; a refusal is one `record` line.
  `read_capture` reassembles one record per request (an unfinished one is marked
  `incomplete`). `capture.jsonl` stays plain JSONL while the run is live and is gzipped
  on close, so a killed proxy leaves every started request readable.
- **Exchanges:** each forwarded request's upstream side runs in a task of its own from
  the start, so a harness that hangs up -- even before the response headers -- only stops
  the relay to it; the read continues (bounded) and the usage is still reconciled. Bytes
  waiting for the harness are bounded (8 MiB) and each write to it has a deadline (60 s):
  a harness that stops reading, connected or not, switches the exchange to
  discard-and-drain (`client_stalled` in the capture). The SSE parser is bounded by bytes
  too: an event over 16 MiB is skipped to its separator, and past 64 MiB of kept events
  only accounting events (usage, the served model) are kept.
- **Reading a capture** decodes each binary line on its own: a record cut anywhere by a
  killed proxy (even inside a UTF-8 character) is skipped and every earlier one kept.

### Budget

- **Ledger:** `ledger.jsonl` in the campaign directory (`--ledger` to move it) is append-only,
  fsync'd, reloaded at start, and guarded by an exclusive `fcntl` lock. Two processes never
  both think they have headroom.
- **Reservation:** before forwarding, the proxy reserves the request's **byte count** as
  input tokens (an upper bound: a byte-level tokenizer emits at most one token per byte) at
  the uncached price plus the **largest** of `max_tokens`, `max_completion_tokens` and
  `max_output_tokens` at the output price. A request that names no limit is reserved at
  the **model's maximum output** -- the largest of every limit that applies: the owner's
  craze `max_output_tokens`, gx `max_completion_tokens` and the snapshot's models.dev
  `limit.output` (deepseek-v4p1-flash lists 384000); 131072 when none is known -- and is
  forwarded unchanged (the proxy never adds a limit). Only one completion per request is admitted.
  At Kimi K3's ledger price an unbounded request reserves about $2.95 of output, so a Kimi
  run needs `--run-cap` above the $3 default.
- **Reconciliation:** after the request, the reservation is replaced by the reported usage.
  A request whose usage never arrives (a disconnect, an error) keeps its reservation.
- **Drains:** when a harness hangs up on a stream, the proxy reads the rest of the upstream
  stream (up to 2 min, bounded by the run's close) so the usage still arrives. This
  happens with opencode's and gx's title calls when the harness exits, and with codex,
  which closes a Responses stream once `response.completed` arrives. Without drains,
  every such request would keep a `max_tokens`-sized reservation. The capture marks these
  requests `client_disconnected` and `drained`.
- **Run ids:** the ledger's `run_id` is `<batch dir>:<harness>/<model>/<task>/rep<N>:a<attempt>`,
  unique per batch, so the per-run cap never mixes two batches that share a label.
- **Stops:** admission refuses at **$95** committed plus reserved (`--budget-cap`), and
  per run at **$3** settled (`--run-cap`, recorded as `budget-capped`). A run may also
  hold at most 4 open (unsettled) reservations at once; a fifth concurrent request is
  refused, also as `budget-capped` -- this bounds a run's exposure before its first
  settlement, while still admitting one worst-case reservation up front.
- **Estimates:** `crazeeval run` refuses a batch whose estimate would pass the cap. The
  estimate is the median cost per run of each model so far, else `prices.toml`'s
  `prior_run_cost`.
- **Prices:** these are §2.7's ledger prices, keyed by wire model. Fireworks is at the US
  ×1.5 rate. The Z.AI coding plan is $0 against the cap, with its list-equivalent
  reported. `crazeeval ledger` prints the totals.

## Config

- `models.toml`: the six eval models. Each entry has its provider, its wire model, its one
  pinned reasoning effort, and each harness's id for it. codex runs only where the provider
  serves `/responses` (Meta, Fireworks).
- `prices.toml`: see Budget.
- **`crazeeval snapshot-config`:**
  - It copies the target definitions from the owner's craze and gx tables into
    `<campaign>/eval-config/<stamp>-<hash>/config.json` (and moves `CURRENT`). Only allowlisted
    fields are copied: no key, env-key name, auth helper or header.
  - It saves a models.dev catalog for opencode (`OPENCODE_MODELS_PATH`), and records which
    `--variant`s opencode offers per model, asked of opencode itself in a sandbox.
  - Every run uses the snapshot and records its hash; runs never read the owner's live files.
- **The opencode seed:** agent runs are offline, and opencode installs
  `@opencode-ai/plugin@<its version>` into its config directory at start-up. Once per
  opencode version `crazeeval run` installs it -- with network, in a sandbox, lifecycle
  scripts off -- into `~/.cache/crazeeval/opencode-seed/plugin-<version>/`; every run gets
  its `package.json`/`package-lock.json` and a read-only `node_modules`, which makes
  opencode skip the install. The provider SDKs are built into opencode.
- **Helper environments:** host helpers (uv, the seed) get an explicit allowlist of
  variables, and none may hold a loaded key's value.

The generated homes, per harness:

| harness | home | notes |
|---|---|---|
| craze | `CRAZE_HOME` with `native/models.toml` and `native/providers.toml` (0600) | `[subagents] model` is the target; `default_effort` is the pinned effort |
| gx | `GROK_HOME/config.toml` | Web search and fetch are off. Image and video tools are off (they would call api.x.ai). The title and image-description models are pinned to the target, since they default to grok-4.6. `turn_summary` and `title_refresh` are off: they replay the conversation and never reach the answer. Every task -- build and plan -- runs `--always-approve` (plan X12): gx's headless plan mode cannot approve a sub-agent spawn (no `--allow` rule matches `spawn_subagent`, and `--always-approve` overrides `--permission-mode plan`, plan X8), so a delegating gx plan task was cancelled under plan mode alone (base-meta-spark T-P1). Plan tasks therefore run prompt-only: the task prompt asks for a plan and no changes, gx answers without entering plan mode, and the no-writes check still applies. `plan_mode` in `result.json` records `"prompt-only"` for these tasks; see `runners/gx.py`. |
| opencode | `OPENCODE_CONFIG` plus XDG dirs under the home | Uses opencode's own `meta` (Responses), `zai-coding-plan` and `fireworks-ai` providers, with only `baseURL`/`apiKey` overridden. `model` and `small_model` are the target. `webfetch`/`websearch` are denied, which removes them from the offered tools. Model fetch, autoupdate and share are off. |
| codex | `CODEX_HOME/config.toml` | A `wire_api = "responses"` provider with `env_key = "CRAZE_EVAL_DUMMY"`. `web_search = "disabled"`, analytics off, plugins off (exec would fetch them from GitHub). Runs with `--dangerously-bypass-approvals-and-sandbox`, since bwrap is the sandbox. See *codex model metadata* below. |

**codex model metadata (a recorded limitation).** codex knows only its bundled OpenAI
models, so for the four eval targets it warns "Unknown model … This will use fallback model
metadata" and runs with the fallback: a 272k context, `exec_command` as its shell tool and no
separate `apply_patch` tool (the models still run `apply_patch` through the shell -- the C1
smoke captures show `apply_patch <<'EOF'` inside `exec_command`). codex 0.157.1 does accept
a catalog (`model_catalog_json`, and a per-provider `model_catalog_url`), whose entries are
its full `ModelInfo` records (shell and patch tool types, truncation policy, tool mode,
Responses-lite, reasoning-summary support, instructions template…). Writing entries for
Meta and Fireworks means choosing each of those wire-visible settings for providers they
were never tested against, and proving it needs real runs; that is not cheap, so C2 leaves
codex on its fallback. codex is a reference, never a bar.

Every child also gets:

- `SHELL=/bin/bash`, so gx and codex use the shell craze's bash tool uses;
- a fixed git identity;
- `GOTOOLCHAIN=local` and `GOPROXY=off`;
- the tool venv (system python plus pytest) first on `PATH`.

## Commands

Every command takes the global `--campaign NAME` (see *Campaigns*); without it the
campaign comes from the environment or is `~/.craze-eval/default`.

```shell
# Once per campaign, and again whenever the owner's model tables change:
uv run crazeeval snapshot-config

# Validators: every task's category controls must pass before the task may run.
uv run crazeeval validate [--tasks <ids|split:...>]

# A batch. --craze-bin is required for craze; never /usr/local/bin/craze.
uv run crazeeval run --harness craze,gx,opencode,codex --model muse-spark-1.3-contributor,glm-5.3-flash \
    --tasks split:smoke --reps 1 --craze-bin ../bin/craze --label smoke [--parallel 4] \
    [--cap-zai 2] [--cap-other 3] [--out DIR] [--timeout S] [--first-rep N]
# --first-rep numbers reps from N instead of 1 -- a later batch adding a second rep
# where budget allows, without re-running (and so replacing) rep1.
# The batch directory is created exclusively and locked; an existing one is refused.
# --resume reopens --out when its identity fingerprint matches -- task definitions and
# testdata, fixtures, the config snapshot, model settings and prices, every executable's
# hash and version, timeouts and caps, the evaluator's own code -- and runs only what has no final
# result, in new attempt directories (nothing is deleted). run exits non-zero when a
# requested task fails validation or nothing can run.
uv run crazeeval run ... --out DIR --resume

# The proxy on its own (the live tmux smoke): prints the base URLs; --craze-home writes a
# CRAZE_HOME that points at it.
uv run crazeeval proxy --port 8765 --model muse-spark-1.3,glm-5.3 --run-id live --craze-home /tmp/live-craze

# Proof that the sandbox hides /home/charliek, eval/tasks and every key, that nothing
# outside is reachable (no DNS, no address), and that the relay reaches the proxy socket.
uv run crazeeval probe --craze-bin ../bin/craze

uv run crazeeval keyscan <dir>
uv run crazeeval ledger

# The judge (see "The judge" below). The global fingerprint of the instruction, the schema
# and every rubric (each verdict also carries its own task's hash):
uv run crazeeval judge-hash
# Give older records (global hash only) their task hash while the global hash still
# matches and the task is what its batch ran, so they keep standing once a task is
# added (see "judge-hash --stamp" under "The judge"):
uv run crazeeval judge-hash --stamp FILE [FILE ...] [--dry-run]
# One pair (the judge smoke): two rep directories.
uv run crazeeval judge-pair --a <rep-dir> --b <rep-dir> [--judge sol] [--both-orders] [--seed N]
# Every pair of craze runs against the other harnesses' (held-out verdicts sealed):
uv run crazeeval judge-batch --batch DIR [--x craze] [--y gx,opencode,codex] [--judge sol|luna] \
    [--both-orders | --success-bar] [--seed N] [--parallel 4] [--out DIR] \
    [--calibration FILE] [--override-calibration REASON]
# A build against an earlier build of the same harness (a lever's before/after):
uv run crazeeval judge-batch --batch NEW --batch-y OLD --x craze --y craze --both-orders --out DIR
# Calibration before bulk judging: 30 dev pairs both orders by sol and by luna, 10 padding pairs.
uv run crazeeval calibrate --batch DIR [--seed N]

# The report (held-out sealed unless --unseal), and the wire-capture report.
uv run crazeeval report --batch DIR [--batch LATER ...] [--baseline DIR] [--verdicts DIR ...] [--unseal] \
    [--compare OLD --compare-verdicts DIR] [--losses] [--out DIR]
uv run crazeeval captures --run DIR [--out DIR] [--unseal]

# A compact, diffable record of batches (see "The archive"): runs, final verdicts,
# provenance; reconciled, key- and home-path-scanned, 2 MB by default.
uv run crazeeval archive --batch DIR [--batch LATER ...] --out DIR [--unseal] [--verdicts DIR ...] \
    [--all-runs] [--max-bytes N]

# Recompute a finished batch's diff and its diff-derived checks (no_writes, diff_scope)
# from each rep's final attempt's manifests with today's ignores (X15); every other
# check stands, objective_pass is recomputed. Held-out runs only with --unseal; --dry-run
# prints the summary and writes nothing. Reads manifests, batch.json and result.json, plus
# the current task definitions (hashed to check each run's task fingerprint against the
# batch); never answers, captures or anything key-bearing.
uv run crazeeval rescore --batch DIR [--unseal] [--dry-run]
```

## Tasks

A task is `tasks/<id>/task.toml` plus `testdata/`. Nothing under `tasks/` is reachable from
inside a sandbox. The `task.toml` fields:

- `id`, `category` (explain, answer, investigate, bugfix, feature, refactor, multi-step,
  verify, plan), `split` (dev, heldout, smoke), `mode` (build or plan), `prompt`,
  `timeout_s` and `rubric`;
- `[repo]`: `kind = "craze"` (optional `setup_patch`, optional `commit`) or
  `kind = "fixture"` plus `name`;
- `[repo] commit` (craze tasks only): the craze commit the task materialises, a full
  40-character sha (an abbreviation or a ref name is refused). It defaults to `3eabb31`
  (`paths.CRAZE_TEMPLATE_COMMIT`), so a task about newer craze code pins the commit its
  rubric was written against. Everything that builds, caches, verifies or records the
  workspace uses it: the template cache is per commit, the isolation checks verify
  against it, validation and the scoring side's pristine copy materialise it,
  `result.json` records `craze_commit`, and `manifest.json` lists `craze_templates` per
  commit. A pinned commit is in `task.toml`, so in the task's fingerprint; a task on
  another commit than `3eabb31` -- pinned, or because the default moved -- also has the
  commit folded into its fingerprint, and the batch identity then lists `craze_commits`
  (a task on `3eabb31` keeps the fingerprint it always had). `CRAZE_REPO_IGNORES` stays a
  hard-coded list: `tests/test_workspace_checks.py` checks that the `.gitignore` at every
  commit a task under `tasks/` uses parses to exactly it, so pinning a commit whose
  `.gitignore` differs fails that test until the list is updated;
- `[[checks]]`: the objective checks;
- `[validate]`: the validator inputs.

The check types:

- `facts`: regexes over the answer.
- `no_writes`: final state only. It fails if a tracked file is modified or deleted, or if a
  new file exists outside the ignore list (`__pycache__/`, `*.pyc`, `.pytest_cache/`,
  `*.test`, the task's `ignore`, the harness's own workspace state and, for a craze-repo
  task, the patterns of the craze repository's `.gitignore` at the task's commit:
  `workspace.CRAZE_REPO_IGNORES`). `diff_scope` sees the same diff.
- `tests`: hidden files are added (`add`), trusted files are restored over the agent's
  copies (`restore` from testdata, `restore_from_start`), then a **trusted runner** runs,
  sandboxed and offline, on a scoring copy -- `runner = "pytest"` or `"go"`, with
  `args`; a free-form command is refused. It passes only when the runner exits 0 **and
  every id in `expect` reports a pass**:
  - pytest runs as `python -I -B -m pytest -c /dev/null --rootdir <ws> -p no:cacheprovider
    --junitxml …` (the workspace is not on `sys.path` at start-up, no config file from the
    tree is read); agent-added `conftest.py`, `pytest.py`, `sitecustomize.py`,
    `usercustomize.py` and `pytest.ini` are removed (trusted ones restored), bytecode
    dropped; results come from the JUnit XML;
  - go runs `go test -json -count=1`; an agent-added `TestMain` rejects the check;
    results come from the JSON events. Expected ids: `pkg/suffix::TestName` (or
    `::TestName`).
  - **trusted test directories** (`trusted_dirs`, else the non-root directories of the
    test files the check adds or restores) hold only the trusted set: every other file
    there -- an agent's `__init__.py`, support module or extra test -- is removed, and the
    start version of every trusted file put back (for Go, only `_test.go` files: the
    package's other files are the implementation).
- `diff_scope`: every changed path matches `allow` and none matches `deny`.
- `structural_count`: occurrences of a regex in the matching files (`glob`, minus
  `exclude`), or with `target = "path"` the number of matching paths, against `value`.
- `shared_helper`: the workspace's Python (`glob` minus `exclude`) read with `ast`, never
  run: some function other than the `callers` references every `markers` name (itself or
  through the functions it calls), every caller calls it, and no caller references a
  marker itself -- an extraction, not three copies rewritten.
- `test_discrimination` (`runner` as above): the agent's new or changed tests must fail
  on the original implementation and pass on the agent's.
- `executed_code`: a shell call whose command matches `patterns`, whose result was sent
  back to the model (`require_result`, on by default: an attempted call is no evidence),
  and which shows one of the `evidence` regexes -- in its command, in its result, or in an
  earlier call that wrote a file the command runs. `evidence` is required: without it
  `python --version` would pass.

The validators, per category, are run by `crazeeval validate` (§3.1.5):

| category | the check must… |
|---|---|
| implementation | fail on the untouched workspace and pass with `reference.patch` (for a craze `setup.patch` task, the planted tree with the patch reversed: its commit's content); a refactor's tests pass on both, its structural and `shared_helper` checks carry the control; `diff_scope` passes the reference and fails it plus an edit to a denied file (`scope_denied_edit`) or outside the scope (`scope_outside_edit`) |
| investigate | pass on a reference answer and fail on a decoy |
| verify | pass on a synthetic capture whose code-running call exercised the case and was answered (`synthetic_command`, `synthetic_result`); fail with none, with an irrelevant answered call (`python --version`), and with the right call never answered |
| explain, answer, plan | pass on the reference answer and fail on the decoy; `no_writes` must also fail when an edit is planted |

C1 ships two smoke tasks on the `smoke-py` fixture: `smoke-explain` and `smoke-fix`. C2 adds
the fifteen tasks of §3.1.5 (`name` is the task's slug; `false_claims` lists claims the
judge must treat as false; a `structural_count` check may `exclude` globs or count paths
with `target = "path"`):

| id | name | category | split | repo | objective checks |
|---|---|---|---|---|---|
| T-E1 | explain-prompt-prefix | explain | dev | craze | facts (4 of 5), no writes |
| T-E2 | explain-bad-tool-call | explain | dev | craze | facts (4 of 5), no writes |
| T-E3 | answer-native-cost | answer | held-out | craze | facts (4 of 5), no writes |
| T-I1 | investigate-regression | investigate | dev | `invoicing` (9 scripted commits, tags v1.1/v1.3) | the commit (46d165a) and the rounding mode, no writes |
| T-I2 | investigate-flaky-test | investigate | held-out | `quota` (Go) | shared `Defaults`, the mutating test, order/shuffle; no writes |
| T-B1 | fix-python-parser | bugfix | dev | `envfile` | 9 hidden + 10 trusted tests |
| T-B2 | fix-go-cache | bugfix | held-out | `lrucache` (Go) | 6 hidden + 4 trusted tests |
| T-B3 | fix-craze-redact | bugfix | held-out | craze + `setup.patch` (one orphan commit) | the package's original 10 tests; diff confined to its non-test files (with scope controls) |
| T-F1 | feature-python-json-flag | feature | dev | `wordstat` | 4 hidden + 5 trusted tests; the agent's tests discriminate |
| T-F2 | feature-go-humanize | feature | held-out | `humanize` (Go) | 4 hidden (incl. `math.MinInt64`) + 1 trusted test; the agent's tests discriminate |
| T-R1 | refactor-dedupe | refactor | dev | `accounts` | trusted tests; one helper used at all three call sites (`shared_helper`); each check appears at most once |
| T-M1 | multi-step-rename | multi-step | held-out | `kvstore` (Go + docs) | 3 hidden API tests + 3 hidden CLI tests (the built `kv`: `-namespace`, `list-namespaces`, messages); "bucket" in no file and no path |
| T-V1 | verify-edge-case | verify | held-out | `batchexport` | ValueError and its cause; an answered Python call that exercised `parse_batch("")`; no writes |
| T-P1 | plan-craze-feature | plan | dev | craze | facts (3 of 4), no writes |
| T-P2 | plan-fixture-feature | plan | held-out | `pricing` | facts (4 of 5), no writes |

The craze tasks' rubrics are written from the code at their commit (`3eabb31` for all
fifteen) and cite file:line; each explain/answer/plan rubric has 7–9 checkable items and
three false claims. The regex fact
checks are the subset of the rubric that is safe to match mechanically; the judge grades
every rubric item. Validators added for C2's categories: a refactor's tests pass both
untouched and with the reference (its structural checks carry the control), and a
`test_discrimination` check passes with the reference patch's tests and fails untouched.

Scoring time: each Go scoring invocation compiles with a cold, private Go cache and takes
about 2 s (T-B2, T-F2, T-M1 on a fixture; T-B3's `go test ./internal/harness/redact/` in the
craze tree); a pytest invocation about 0.1–0.2 s. `crazeeval validate` of all seventeen
tasks takes about 25 s.

**Contamination.** Tool calls that touch `eval/`, `reference.patch`, `eval-runs`,
`.claude/plans`, the owner's `/home/*/.craze/native`, a GitHub URL of the craze
repository, or an absolute `testdata/` or `tasks/` path outside the run's workspace
invalidate the run. craze's own `testdata/` directories and the sandbox home are not hits.

## Where results go

Raw results never land in the repo. `--out` defaults to
`<campaign>/eval-runs/<label>-<timestamp>/` (plan 029's batches are under
`~/.claude/plans/craze/029-native-harness-quality/eval-runs/`, its scripts set
`CRAZEEVAL_PLAN_DIR`). What is worth keeping in the repo is a `crazeeval archive` of
them (see *The archive*).

Each batch writes:

- `batch.json`: the identity whose fingerprint `--resume` checks, plus the `campaign`
  the batch ran in (recorded, not fingerprinted);
- `manifest.json`: executables with versions and sha256; the generated configs; the
  snapshot hash; fixture hashes; each craze template's commit, tasks and isolation check
  (`craze_templates`); prices; the run order; the route table (hosts only);
- `validation.json`
- `results.jsonl`
- `summary.json`
- `proxy-refusals.jsonl`
- `rescore.jsonl` (after `crazeeval rescore`): one line per changed run -- an unchanged
  run writes no line -- with the run id and key, task, harness, model, the checks whose
  verdict flipped, `objective_pass` before and after; a held-out run's line goes to
  `heldout/rescore.jsonl`. A rescored run's
  `result.json` (rep and attempt level) carries a `rescored` record, and its original
  is kept once, never overwritten, as `result.pre-rescore.json` beside it. `results.jsonl`
  and `summary.json` keep the scores as the batch wrote them.

Per-run disk stays small (a baseline batch is ~250 runs): after the key scan each attempt
drops its Go cache, the harness's download and state caches (opencode's npm cache,
workspace snapshot and XDG cache; gx's unpacked user guide; codex's bundled skills and
sqlite state) and, for a craze-repo task, the workspace itself (the repository with its
history); `diff.patch`, the manifests, the capture, `result.json` and the logs stay.

Each run lives in `runs/<harness>/<model>/<task>/rep<N>/`. Held-out tasks go under the
sealed `heldout/` instead, and their verdicts are not printed. Inside a run directory:

- `result.json`: the final attempt, with an `attempts` list;
- `attempt-<n>/`:
  - `stdout.jsonl`, `stderr.txt`
  - `capture.jsonl.gz`
  - `start-manifest.json`, `final-manifest.json`, `diff.patch`, `gitpost/`
  - `ws/` (the final workspace; removed for craze-repo tasks) and `home/` (the harness's
    state). The run's Go cache and the caches above are deleted after the key scan (never
    through a planted link); `result.json`'s `pruned` records what was removed.
  - `scoring/*.stdout`, `scoring/*.out/` (JUnit reports)

`result.json` holds:

- the harness, model, wire model, effort, task, split, rep and attempt; `craze_commit`
  (the commit a craze-repo task materialised);
- `exit`, `timed_out`, `wall_s`;
- `answer`, `answer_words`, `notes`;
- `status`: ok, crashed, timeout, infra, contaminated, key-exposure, budget-capped,
  budget-stop, oversize or launch-error; `completed` and `failure` (the harness's own terminal
  report); `plan_mode` (how plan mode was asked for);
- `objective_pass` and `checks`;
- `contamination`;
- `metrics`, derived from the capture:
  - requests, split into main and aux;
  - tool calls and round-trips;
  - tokens and cost;
  - served models;
  - offered tools and the web tools among them;
  - the main requests' effective reasoning controls;
- `proxy`: the route summary, including the refusal counters;
- `sandbox`: quiescence and environ samples;
- `key_scan`.

## The judge

`crazeeval/packet.py`, `judge.py` and `judging.py` (plan 029 §3.1.6).

- **The packet** holds the task prompt, the rubric (with its known false claims) and, per
  side between `<<<BEGIN/END UNTRUSTED CANDIDATE X>>>` markers: the final answer (for a plan
  task, the plan the harness presented), the execution evidence (every tool call in order,
  shown by its **kind** -- `shell`, `read`, `edit`, `write`, `search`, `list`, `todo`,
  `delegate`... (`capture.TOOL_KINDS`, which maps every name the four harnesses use), with
  argument names unified (`path=`, `pattern=`) and gx's `session_title` left out, since tool
  names alone would tell the judge which harness a side is -- with its command or path, and
  its result's exit status, first lines and, for a long result, last lines, since a test
  summary is at the end; at most 12 KB, counted in UTF-8 bytes, then a marker),
  the workspace changes (every file with its stats, then the diff up to 40 KB, then
  `[diff truncated: N bytes in M files not shown]`; generated files and harness state paths
  stripped) and the objective results, a timeout or crash included.
- **Provenance is normalised, names are not scrubbed:** the sandbox workspace, home and
  `/tmp/<x>` paths (and the host's run directory) become `<workspace>`, `<home>`, `<tmp>`
  (`<run>`), and a harness's plan-file location becomes `<plan-file>`; task-subject words,
  repository paths and harness names stay.
- **The instruction** ranks correctness (against the rubric, the objective results and the
  evidence; a "tests pass" with no such command in the evidence is unverified; a false
  claim costs more than an omission), then completeness, evidence and clarity; length earns
  nothing. The candidates' text is untrusted data whose instructions are ignored.
- **The schema** (`--output-schema`, strict): `winner` (A/B/tie), `confidence`
  (low/medium/high), `score_a`/`score_b` (1–10), `rubric_a`/`rubric_b` (every item graded
  met/missed/false), `reasons`. Outputs are validated again before they count.
- **The call** is exactly `codex exec --ephemeral --ignore-user-config --ignore-rules
  --skip-git-repo-check -s read-only -C <empty temp dir> -m <model> -c
  model_reasoning_effort=medium --output-schema <schema> -o <out> -` with the prompt on
  stdin, under the judge's own `CODEX_HOME` (`~/.cache/crazeeval/judge-codex-home`: a
  minimal `config.toml` and a **symlink** to `~/.codex/auth.json`, never a copy) and an
  environment built from nothing but `HOME`, `PATH`, `CODEX_HOME` and locale. At most 4
  calls at once. A failed call (non-zero exit, no output, an invalid output) is no
  verdict, never a tie, and is re-run up to twice; a usage-limit exit pauses every call
  and retries the same model (5 min, 10, 20, 30, then hourly, up to 12 h) -- it never
  moves to another model.
- **Order:** which run is "A" is decided by the recorded seed salted with the pair's
  identity (so it does not depend on batching). Both-order mode judges both orders; a
  disagreement is a tie; a missing order is no verdict. Success-bar pairs
  (`--success-bar`) are both-order judged by sol, and an order disagreement or a
  low-confidence verdict is re-judged by astra in both orders, whose result stands.
- **Verdict files:** `judge-batch` appends one record per pair to `<out>/verdicts.jsonl`,
  held-out pairs to the sealed `<out>/heldout/verdicts.jsonl` (never printed). Each record
  holds the two judged runs' ids (`x_run_id`, `y_run_id`: batch, run key and attempt), both
  orders' raw outputs, the mapped verdict, the seed, the judge model and effort, wall time,
  attempts, rate-limit waits, the judge hashes (`judge_hash` and `task_judge_hash`, below)
  and the calibration decisions it was judged under; a re-run skips a pair already judged
  in the same mode by a verdict that still stands.
- **Calibration** (`calibrate`): 30 dev pairs spread over tasks and models, judged in both
  orders by sol and again by luna, plus 10 padding pairs (a run's answer against itself
  padded with a restatement of the task and a recap of its first paragraph -- same
  evidence, diff and objective results). It writes the flip rate, luna's agreement with
  sol, how often the padded side won, and each gate as pass, fail or **incomplete**: a gate
  passes only when every requested pair has a verdict in both orders (one agreeing luna
  verdict out of 30 is not 100% agreement), and incomplete fails it. Both orders for every
  pair unless the flip gate passed (≤ 20% flip); luna for bulk pairs only when its gate
  passed (≥ 80% agreement); the instruction is revised (before freezing) unless the
  padding gate passed (the padded side won at most 2 of 10).
- **Enforcement:** `judge-batch` reads the decisions (`--calibration`, default
  `<batch>/calibration/calibration.json`) and obeys them: it refuses `--judge luna` unless
  the luna gate passed, and turns single-order judging into both-order unless the flip gate
  passed. A missing file, one from another judge hash, or an incomplete one counts as no
  calibration: both orders, no luna. `--override-calibration REASON` judges as asked and
  records the reason in every verdict. (For a lever's before/after, pass the baseline's
  calibration file.)
- **The frozen hash:** `crazeeval judge-hash` fingerprints the instruction, the schema and
  every task's prompt, rubric and false claims (`judge_hash`); record it in `progress.md`.
  Adding any task changes it, so it is not what decides whether a verdict stands.
- **The per-task hash** (`task_judge_hash`, in every new verdict record beside
  `judge_hash`): the instruction, the schema and what the judge is shown of that one
  task that no run changes -- its prompt, the plan-task note, its rubric and its false
  claims, as the packet renders them. **A verdict stands** when its `task_judge_hash`
  equals its task's current one -- or, for an older record without one, when its global
  `judge_hash` equals today's. `judge-batch`'s skip and the report's acceptance both use
  this rule, so adding a task, or changing one task's rubric, never invalidates another
  task's verdicts; changing a task's rubric re-judges only that task's pairs. Plan 029's
  records carry only the global hash: they stand until the first task is added or
  changed.
- **The legacy fallback's blind spot:** the global hash covers each task's prompt, rubric
  and false claims but not its mode (which adds the packet's plan note) nor anything else
  of the packet, so an older record judged before such a change still reads as current.
  The global hash is left as it is (changing it would orphan plan 029's verdicts);
  stamping is how an older verdict is made durable.
- **`crazeeval judge-hash --stamp FILE ...`**: in each verdict or calibration JSONL file,
  a record without a `task_judge_hash` gets its task's current hash only when its
  `judge_hash` is today's **and** its task's fingerprint now equals the one in the
  `batch.json` of the batch that owns the file (the nearest of the file's three closest
  ancestor directories holding one: `<batch>/judging/`, `<batch>/judging/heldout/`,
  `<batch>/calibration/`). Records with another global hash, with none, or whose task
  changed, is gone or has no batch fingerprint (`task changed or unknown`) are left alone
  and counted; every other line keeps its bytes (read and written as UTF-8). The file as
  it first was is kept once as `<name>.pre-stamp.jsonl` beside it, created exclusively
  (`O_EXCL`) and never replaced; a second run changes nothing; `--dry-run` only counts
  and takes no lock. **Locking:** every writer of a verdict or calibration file --
  `judge-batch`'s appends, `calibrate` -- takes an exclusive `flock` on `<file>.lock`
  beside it, and the stamper holds that lock across its read, compute and replace, so
  an append made meanwhile waits and is never lost. Run it before adding or changing a
  task; `crazeeval archive` applies the same rule to such records in its own output.

The judge smoke (C2): craze vs gx on `smoke-fix` (deepseek-v4p1-flash, C1's last smoke),
sol, one order: a schema-valid verdict in 11 s.

## The report

`crazeeval report` (`report.py`, §3.1.8) reads batches and verdict files and writes
`report.md`, `report.json` and `length_vs_verdict.csv`:

- **verdicts that still stand:** a record judged under another judge hash (see *The
  per-task hash*) is set aside before anything else, and the report prints how many, the
  global hashes of the verdicts it counted (and today's) and their per-task hashes;
- objective pass counts per model × harness on dev, held-out and all tasks (a task with two
  reps counts its mean pass);
- the win-rate matrix per model and pooled, with Wilson 90% intervals (ties half);
- **verdicts bound to runs:** a verdict counts only for the exact two runs it judged
  (`x_run_id`/`y_run_id`), and only when both are scored runs the report selected -- one on
  a replaced, unscored or older build's run counts for nothing. A pair judged several times
  counts once, by its final verdict: astra's both-order re-judgement, else sol's
  both-order verdict, else a single order (the later record on a tie); a failed final
  verdict is a missing verdict, not replaced by a lesser one. The report prints how many
  verdicts were loaded, bound and final;
- the success bar per model: the **best open harness is fixed from the baseline batch**
  (`--baseline`, default the first `--batch`) -- the higher objective count of gx and
  opencode there, tiebreak their head-to-head win rate -- and, unsealed, recorded in
  `<baseline>/best-open-harness.json` the first time; after that the record stands, so no
  later rerun (a provider-drift check) can change it. Then (a) native's objective count ≥
  its, (b) native's win rate ≥ 50%, (c) both again on the held-out tasks alone, (d) coverage
  -- every task has a scored run on both sides and a bound verdict. The outcome is meets,
  misses (with the gap) or inconclusive (coverage incomplete). A later `--batch` replaces an
  earlier one's run with the same key, so a final craze batch is compared with the
  baseline's reference runs;
- metric medians (main requests, tool calls, tokens, cost, wall time, answer words);
- verdicts against the log length ratio of the two answers (bins, the longer side's win
  rate, the correlation);
- task-level verdict tables (craze's W/L/T per task and harness; `l` marks low confidence);
- `--compare OLD --compare-verdicts DIR`: the new craze build against the old, pooled and per
  model, with the keep rule's readout (≥ 55% for a conditional lever, ≥ 45% for a
  requested one, and no dev objective drop) as `keep`, `drop` or `inconclusive` -- the last
  until every (task, model) both builds ran has a verdict bound to the two builds' runs, so
  a comparison with no verdicts never reads keep;
- `--losses` adds **"Where craze lost"**: for each final verdict a craze run lost (craze on
  either side), the task, model, split, the craze run key and the other run, the scores,
  and per judged order the rubric items craze did not meet against the other side's
  (each item only craze missed is quoted from the rubric) and the judge's reasons -- the
  tuning signal behind a lever such as L7. Sealed, it covers the dev tasks only.

**Sealed:** without `--unseal` nothing under a batch's `heldout/` -- runs or verdicts -- is
read; the success bar uses the dev tasks alone and reports `provisional-meets` or
`provisional-misses`. Unseal only for the final.

## The archive

`crazeeval archive --batch DIR [--batch DIR ...] --out DIR [--unseal] [--max-bytes N]`
(`archive.py`, plan 029 W2) writes a compact, diffable record of chosen batches -- what
is worth keeping in the repo once the raw runs (captures, answers, workspaces) stay
outside it. By default it selects runs and verdicts as the report does (a "current
view"): a later `--batch` wins a run key (so give the baseline first), held-out runs and
verdicts only with `--unseal`, verdicts bound to the scored runs, one final verdict per
pair by the report's ranking. `--all-runs` keeps every scored run of every given batch
instead, by its `run_id` rather than replaced by run key -- use it to archive a whole
campaign (baseline, lever, final, A/B batches) rather than a final view, since a
craze-vs-craze comparison's two batches often reuse the same run keys and the default
would drop one side's runs (and unbind its verdicts). `archive.json`'s `selection`
records which mode wrote the archive. A batch directory given more than once is read
once either way.

- `runs.jsonl`: one line per scored run -- campaign and batch names, run key and id,
  harness with its version and executable hash (a sha256 prefix), the craze build for a
  craze run (binary basename, sha256 prefix, version, the source commit it was built
  from), the model, wire model and served models, the task, split, category, mode, plan
  mode, rep, status, objective pass and each check's result, answer words, tool calls,
  main requests, wall time, cost, list cost and tokens;
- `verdicts.jsonl`: one line per final verdict -- task, model, split, the two run ids and
  harnesses, judge model and mode, winner, confidence, scores, each judged order's rubric
  grades and reasons, a success-bar escalation's first result, `judge_hash` and
  `task_judge_hash`. The verdicts that still stand come first (the report's own set);
  a pair with none keeps its best older verdict, marked `"current": false`. A record from
  before per-task hashes gets its task's hash stamped by `judge-hash --stamp`'s rule --
  its global hash is today's and its task's fingerprint equals its batch's
  (`task_judge_hash_source: "stamped"`); otherwise the field stays null and
  `task_judge_hash_note` says why;
- `batches.json`: per batch its name, campaign, label, harnesses, models, reps (and
  `first_rep`), prices, task fingerprints (and `craze_commits` when a task is off
  `3eabb31`), fixture, evaluator and config-snapshot hashes, executables, the craze build,
  and a copy of its `best-open-harness.json` (paths reduced to batch names);
- `calibration.json`, only when a batch has calibration: per such batch (name and
  campaign) its summary (seed, judge hash, flip rate, luna agreement, padding, gates),
  never the raw calibration records;
- `archive.json`: the selection mode (`"current-view"` or `"all-runs"`), the verdict
  counts, how many paths were redacted, the file sizes, the scan of the data files and
  **the reconciliation**, also printed:
  `archive: N scored runs selected, N rows written; M final verdicts (K current), M rows
  written -- reconciled`. Either count differing fails the command.

Provenance (campaign, build, executables) is looked up by each batch's resolved
directory, so two campaigns' batches that share a name keep their own; only names are
written.

**Left out on purpose:** answers, captures, logs, diffs, command lines, judge prompts,
raw calibration records, and every path (batches are named, never located).

**Free text is kept without local paths:** the judge's reasons -- and every other string
written -- have each local-machine filesystem path replaced by `<path>`: a path under
`/home`, `/Users`, `/tmp`, `/private`, `/root`, `/sandbox`, `/var/folders`, `/run/user`
or the owner's own home (the root alone too), with a trailing `:line[:col]` kept after
it (`/tmp/foo.py:12` becomes `<path>:12`). Nothing else is touched: routes
(`/api/v1/users`), fractions (`2 /3/4`, `7/10`), other slash-rooted text
(`/usr/local/bin/craze`), repository-relative paths, `<workspace>/internal/...`, `~/...`
and URLs stay. `archive.json` counts the redactions (`paths_redacted`); the scan below
remains the backstop.

**Safety, before it finishes** (the backstop): every file in `--out` is scanned with the
batch runner's key ring and key scan (`keys.scan_tree`; the owner's providers are loaded
for it, and the command refuses to run with no key loaded) and grepped for the owner's
home path and `/home/`. Any hit fails the command, naming the file; nothing is deleted.
A total over `--max-bytes` (default 2 000 000), counted over every file in `--out`, an
earlier archive's extra files (a README) included, fails it too. The output has no
timestamp: the same batches archive to the same bytes. `--out` must be new, empty, or an
earlier archive (it holds `archive.json`; other files there are left alone) -- any other
non-empty directory is refused, whatever it holds -- and never inside a batch, whose
files are read-only inputs.

## The capture report

`crazeeval captures --run <batch>` (`captures.py`, §3.1.10) writes `captures/captures.md`,
`captures.json` and each harness's system prompt under `captures/prompts/<model>/`: per
model and harness, the system prompt's bytes and sha256 (and how many distinct prompts its
runs sent), where the environment text sits (workspace, date, git, OS, shell: system
prompt or first user message), the offered tools with each description's bytes, every
request parameter other than the conversation and the tools, the replayed assistant turns
and how many of them carry their reasoning (counted per turn, over the requests after each
run's first and for the last request; chat messages and Responses items alike), the last
request's role sequence, and the served and requested model ids. Its fidelity block is
AC-A8: effort parity across harnesses (each harness's effective reasoning control from its
main requests), opencode's GLM `thinking` flag on every request, no web tool offered, one
model per run -- every run requested only the target and was served only the target -- and
no contamination hit in an accepted run. Every check needs main-request evidence from
every run: a run with no main request makes it `no-evidence`, never true, and one failing
run makes it false. Held-out runs are read only with `--unseal`.
