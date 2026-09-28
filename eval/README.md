# crazeeval: the craze native harness evaluation

`eval/` is plan 029's evaluation harness. It runs craze native and three reference
harnesses on the same models and the same tasks: gx (the grok-build fork), opencode and
codex. It records what each one sends on the wire and scores the results. It is not part
of CI or the per-commit gate. It costs money, and its runs are not deterministic.

It is a Python ≥ 3.11 `uv` project, package `crazeeval`. Run every command from this
directory:

```shell
uv run crazeeval <command>
uv run pytest            # the offline tests: no network, a few seconds (bubblewrap tests skip without it)
```

`eval/go.mod` is a module with no code. It keeps every Go file under `eval/` (fixture
sources, hidden tests) out of the root module. So `go list ./... | grep -c /eval/` from the
repo root prints 0, and `tests/test_sandbox_boundary.py` asserts it.

## How a run works

```
harness (in bwrap, its own netns) ──> 127.0.0.1:<port> ──relay──> proxy.sock ──> https://<upstream-host>/<prefix>/...
     base URL http://127.0.0.1:<port>/r/<token>/<upstream-host>/<prefix>    the recording proxy: the only holder of keys
```

1. **Workspace.** The run's workspace is materialised into an independent object store:
   no remote, no alternates.
   - craze tasks start from a template built once with `git init` and
     `git fetch --no-tags <repo> 3eabb31` (only that commit's ancestry). A task with a
     `setup.patch` instead gets a **fresh object store holding one parentless commit** of
     the patched tree (plan X7): `git cat-file -e 3eabb31` fails, `git rev-list --all` is
     one commit, every stored object is reachable from it -- nothing in git shows what was
     planted or what it replaced.
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
     additions only). A file over **50 MiB** or a workspace over **500 MiB** (apparent
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

- **Ledger:** `ledger.jsonl` in the plan folder (`--ledger` to move it) is append-only,
  fsync'd, reloaded at start, and guarded by an exclusive `fcntl` lock. Two processes never
  both think they have headroom.
- **Reservation:** before forwarding, the proxy reserves `bytes/3` input tokens at the
  uncached price plus the **largest** of `max_tokens`, `max_completion_tokens` and
  `max_output_tokens` at the output price. A request that names no limit is reserved at
  the **model's maximum output** -- the owner's craze `max_output_tokens` or gx
  `max_completion_tokens` from the config snapshot, else 131072 -- and is forwarded
  unchanged (the proxy never adds a limit). Only one completion per request is admitted.
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
  per run at **$3** (`--run-cap`, recorded as `budget-capped`).
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
    `<plan>/eval-config/<stamp>-<hash>/config.json` (and moves `CURRENT`). Only allowlisted
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
| gx | `GROK_HOME/config.toml` | Web search and fetch are off. Image and video tools are off (they would call api.x.ai). The title and image-description models are pinned to the target, since they default to grok-4.6. `turn_summary` and `title_refresh` are off: they replay the conversation and never reach the answer. Build tasks run `--always-approve`; plan tasks run `--permission-mode plan` alone (the two contradict; whether gx plans headless is plan X8, `plan_mode` in `result.json` records what was asked). |
| opencode | `OPENCODE_CONFIG` plus XDG dirs under the home | Uses opencode's own `meta` (Responses), `zai-coding-plan` and `fireworks-ai` providers, with only `baseURL`/`apiKey` overridden. `model` and `small_model` are the target. `webfetch`/`websearch` are denied, which removes them from the offered tools. Model fetch, autoupdate and share are off. |
| codex | `CODEX_HOME/config.toml` | A `wire_api = "responses"` provider with `env_key = "CRAZE_EVAL_DUMMY"`. `web_search = "disabled"`, analytics off, plugins off (exec would fetch them from GitHub). Runs with `--dangerously-bypass-approvals-and-sandbox`, since bwrap is the sandbox. |

Every child also gets:

- `SHELL=/bin/bash`, so gx and codex use the shell craze's bash tool uses;
- a fixed git identity;
- `GOTOOLCHAIN=local` and `GOPROXY=off`;
- the tool venv (system python plus pytest) first on `PATH`.

## Commands

```shell
# Once, and again whenever the owner's model tables change:
uv run crazeeval snapshot-config

# Validators: every task's category controls must pass before the task may run.
uv run crazeeval validate [--tasks <ids|split:...>]

# A batch. --craze-bin is required for craze; never /usr/local/bin/craze.
uv run crazeeval run --harness craze,gx,opencode,codex --model muse-spark-1.3-contributor,glm-5.3-flash \
    --tasks split:smoke --reps 1 --craze-bin ../bin/craze --label smoke [--parallel 4] \
    [--cap-zai 2] [--cap-other 3] [--out DIR] [--timeout S]
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
```

## Tasks

A task is `tasks/<id>/task.toml` plus `testdata/`. Nothing under `tasks/` is reachable from
inside a sandbox. The `task.toml` fields:

- `id`, `category` (explain, answer, investigate, bugfix, feature, refactor, multi-step,
  verify, plan), `split` (dev, heldout, smoke), `mode` (build or plan), `prompt`,
  `timeout_s` and `rubric`;
- `[repo]`: `kind = "craze"` (optional `setup_patch`) or `kind = "fixture"` plus `name`;
- `[[checks]]`: the objective checks;
- `[validate]`: the validator inputs.

The check types:

- `facts`: regexes over the answer.
- `no_writes`: final state only. It fails if a tracked file is modified or deleted, or if a
  new file exists outside the ignore list (`__pycache__/`, `*.pyc`, `.pytest_cache/`,
  `*.test`, and the task's `ignore`).
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
- `diff_scope`
- `structural_count`
- `test_discrimination` (`runner` as above): the agent's new or changed tests must fail
  on the original implementation and pass on the agent's.
- `executed_code`: a code-running tool call appears in the capture.

The validators, per category, are run by `crazeeval validate` (§3.1.5):

| category | the check must… |
|---|---|
| implementation | fail on the untouched workspace and pass with `reference.patch` (or pass on the base commit for a craze `setup.patch` task) |
| investigate | pass on a reference answer and fail on a decoy |
| verify | pass on a synthetic code-running capture and fail without one |
| explain, answer, plan | pass on the reference answer and fail on the decoy; `no_writes` must also fail when an edit is planted |

C1 ships two smoke tasks on the `smoke-py` fixture: `smoke-explain` and `smoke-fix`.

**Contamination.** Tool calls that touch `eval/`, `reference.patch`, `eval-runs`,
`.claude/plans`, the owner's `/home/*/.craze/native`, a GitHub URL of the craze
repository, or an absolute `testdata/` or `tasks/` path outside the run's workspace
invalidate the run. craze's own `testdata/` directories and the sandbox home are not hits.

## Where results go

Results never land in the repo. `--out` defaults to
`~/.claude/plans/craze/029-native-harness-quality/eval-runs/<label>-<timestamp>/`.

Each batch writes:

- `manifest.json`: executables with versions and sha256; the generated configs; the
  snapshot hash; fixture hashes; the craze template's isolation check; prices; the run
  order; the route table (hosts only);
- `validation.json`
- `results.jsonl`
- `summary.json`
- `proxy-refusals.jsonl`

Each run lives in `runs/<harness>/<model>/<task>/rep<N>/`. Held-out tasks go under the
sealed `heldout/` instead, and their verdicts are not printed. Inside a run directory:

- `result.json`: the final attempt, with an `attempts` list;
- `attempt-<n>/`:
  - `stdout.jsonl`, `stderr.txt`
  - `capture.jsonl.gz`
  - `start-manifest.json`, `final-manifest.json`, `diff.patch`, `gitpost/`
  - `ws/` (the final workspace) and `home/` (the harness's state). The run's Go cache and
    any download cache are deleted after the key scan (never through a planted link);
    `result.json`'s `pruned` records what was removed.
  - `scoring/*.stdout`, `scoring/*.out/` (JUnit reports)

`result.json` holds:

- the harness, model, wire model, effort, task, split, rep and attempt;
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
