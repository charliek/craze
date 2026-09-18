# 11 — H0 progress and results

This is the durable, sanitized execution record for Plan 016, the H0
provider-stack fit spike. It is self-contained for future native-harness
implementers who cannot access the local supporting artifacts.

**Status:** complete — completed-negative experiment

**Completed:** 2026-09-18

**Planning baseline:** `origin/main` at `57357fe` (Plan 015, PR #16)

**Execution baseline:** `origin/main` at `5d24c82` (the subsequent
main-branch cancellation-test fix, PR #17)

**External supporting artifacts:**
`~/.claude/plans/craze/016-native-harness-h0/` (local-only; not required to
understand this record)

## Outcome

H0 evaluated three layers independently:

| layer | exact candidate | verdict | consequence |
|---|---|---|---|
| Fantasy provider layer | `charm.land/fantasy v0.43.2` public provider constructors and `LanguageModel.Stream` | **fit** | H1 may use the released provider layer |
| Fantasy agent loop | `charm.land/fantasy v0.43.2` released `Agent.Stream` | **no-go** | craze must own the direct-stream step loop |
| Fantasy combined | same release | **provider-only fit** | accept the provider layer, not the released agent loop |
| Catwalk catalog | `charm.land/catwalk v0.52.43` embedded catalog | **no-go** | a separate panel-reviewed catalog replacement spike blocks H1 |

The overall experiment is **completed-negative**, not blocked: the required
bounded evidence completed and produced supported negative verdicts for the
released Fantasy agent loop and Catwalk. DeepSeek V4 Pro's provider rejection,
the skipped gx controls, and skipped live image variants are bounded row or
variant dispositions described below; they do not make the three-layer
verdicts inconclusive.

## Candidates, toolchain, and provenance

| item | exact value |
|---|---|
| Go language/toolchain used by the probe | `go 1.27.0`; `go1.27.1` |
| Fantasy | `v0.43.2`; tag commit `326027229d6a8a37444b98119456df5a4f07ef2c`; module checksum `h1:BHnC/iu72aZLG5SE1jIViOIStRUIPwxAlw55TZeKEzc=`; Apache-2.0 plus `NOTICE` |
| Catwalk | `v0.52.43`; tag commit `aab3ef84556311c61900372867a1a90e20b65f8b`; module checksum `h1:VgwwWU7jtUxjgRFOo6pS53epQ2yNvIYaIv0p6sb9mYk=`; MIT |
| Probe-only TOML parser | `github.com/BurntSushi/toml v1.6.0`; MIT |
| Approved probe source manifest SHA-256 | `9fe61c8ad284f90969d4819a95985cd24c6a476d792f88f499afdf61da533746` |
| Approved cumulative Linux runner SHA-256 | `b741b6382114e02912f18c42c73c5b93fb5911f1bcc5f1f43aaf71c620b61dbd` |
| macOS probe binary SHA-256 | `36b73f09dcf1d77089fe21bd013cba54e6bec334134d83e417a3dc3165301eb2` |

The probe used released Go modules from the module cache. A Fantasy checkout
four commits beyond the release was inspected but never substituted for the
release. Probe code, live evidence, configuration, and credentials remain
outside the repository. No Fantasy or Catwalk dependency was added to
craze's root module because production imports belong to H1.

Fantasy requires Go 1.27.0. Accepting its provider layer therefore raises the
repository floor to `go 1.27.0`, toolchain and mise Go `1.27.1`.
`golangci-lint v2.10.1` could not read Go 1.27 export data; synchronized
`v2.13.2`, built with Go 1.27.0, passed the repository preflight and is the
new pin.

## Experiment contract

Each clean live attempt had to complete exactly three model steps:

1. call `probe_first` with a fresh nonce;
2. consume its returned sentinel and call `probe_second` with that sentinel;
3. return final text containing the second sentinel, with no further tool
   call.

The intentional sampling-cancellation variant could stop after two completed
tool steps, provided cancellation occurred during bounded sampling and no later
request was sent.

A success also required complete schema-valid tool arguments, distinct call
identifiers, adjacent assistant-call/tool-result history on replay, exact tool
order, no duplicate execution, explicit terminal stream parts, and trusted
per-step and aggregate usage. Both modes had a hard three-step bound.

Every request used `MaxRetries=0`, a numeric 512-token output ceiling, a
90-second request deadline, a five-minute attempt deadline, verified
provider pricing, and an immediate conservative 2× budget reservation. Each
model/mode allowed at most three attempts. Only `S,S` or `F,S,S` counted as a
clean pass.

The direct mode is a small public `LanguageModel.Stream` driver. The Agent
mode uses released `Agent.Stream` against the same model, tools, and scenario.
This separates provider transport/streaming suitability from the released
agent loop.

## Checkpoint chronology

### Baseline and preflight — 2026-09-17

- Verified the exact module tags, commits, checksums, licenses, Go floors, gx
  configuration precedence, 11 effective alias/wire-model pairs, and current
  provider prices.
- Confirmed the required credential classes existed on Linux and the
  mac-mini without recording their values or source locations.
- Created an isolated Plan 016 worktree and left the shared main worktree
  untouched.
- Determined that the repository's former linter pin was incompatible with
  Go 1.27 and verified `golangci-lint v2.13.2` as the smallest synchronized
  update.

### Offline probe and safety review — 2026-09-17

- Implemented the external probe with separate direct and Agent modes, closed
  artifacts/errors, exact endpoint containment, strict alias/config parsing,
  canary scans, and an interprocess-locked USD 10 ledger.
- Local streamed fixtures covered fragmented arguments, repairable and
  invalid JSON, incomplete calls, `length`, finish reasons, usage, reasoning
  boundaries, history replacement, image replay, redirects, malformed
  streams, cancellation during sampling and tool execution, and closed error
  classes.
- `go test -count=1 ./...`, `go test -race -count=1 ./...`, and `go vet ./...`
  passed under Go 1.27.1. Integrated HTTP fixtures proved reservation before
  each request, conservative retention for missing usage, and overrun
  persistence before failure.
- A credential-free safety review initially blocked traffic, all concrete
  findings were fixed with regressions, and the resumed reviewer approved the
  bounded campaign. Reviewers received source and tests only.

### Linux campaign and exact resume — 2026-09-17 to 2026-09-18

The first guarded campaign stopped after the first DeepSeek V4 Pro request.
Fireworks returned HTTP 404 for its configured wire id; the intentionally
closed recorder classified the unrecognized provider 4xx as `internal_closed`.
The probe was not weakened. A reviewed replacement runner recognized only
that exact alias/provider/wire id plus the exact non-retryable Fireworks 404
shape as summary-level `provider_rejected`; every other `internal_closed`
remained fatal. It preserved the original held reservation and resumed only
unrun rows with cumulative attempt numbering and a new reservation prefix.

The cumulative Linux result contains 22 model/mode rows and 46 attempts.
`S` is a structurally complete success, `F` a structurally recorded failure,
and `R` the terminal provider rejection.

| gx alias | wire model | effort | direct | Agent | disposition |
|---|---|---:|---:|---:|---|
| `fireworks/kimi-k3` | `accounts/fireworks/models/kimi-k3` | `high` | `S,S` | `S,S` | clean in both modes |
| `fireworks/qwen3p8-max` | `accounts/fireworks/models/qwen3p8-max` | `high` | `S,S` | `S,S` | clean in both modes |
| `fireworks/deepseek-v4-pro` | `accounts/fireworks/models/deepseek-v4-pro` | `high` | `R` | `R` | Fireworks HTTP 404; artifact remains closed `internal_closed`, summary class `provider_rejected` |
| `fireworks/kimi-k2p7-code` | `accounts/fireworks/models/kimi-k2p7-code` | `high` | `S,S` | `S,S` | clean in both modes |
| `fireworks/deepseek-v4-flash` | `accounts/fireworks/models/deepseek-v4-flash-0731` | `high` | `S,S` | `S,S` | clean in both modes |
| `glm-5.3` | `glm-5.3` | `max` | `S,S` | `S,S` | clean in both modes |
| `glm-5.3-flash` | `glm-5.3-flash` | `high` | `S,S` | `S,S` | clean in both modes |
| `muse-spark-1.3` | `muse-spark-1.3` | `high` | `S,F,F` | `F,F,F` | cap exhausted; HTTP-200 model noncompliance, including one invalid tool call |
| `muse-spark-1.3-contributor` | `muse-spark-1.3-contributor` | `high` | `F,F,F` | `S,F,F` | cap exhausted; HTTP-200 model noncompliance, including one invalid tool call |
| `openrouter/minimax-m3` | `minimax/minimax-m3` | none | `S,S` | `S,S` | clean in both modes |
| `openrouter/gemini-3.8-flash` | `google/gemini-3.8-flash` | `medium` | `S,S` | `S,S` | clean in both modes |

Eight aliases passed both modes cleanly. Both Meta aliases constructed,
reached their intended endpoint, and returned valid HTTP streams; their
failures were model tool-structure noncompliance rather than provider adapter
failures. DeepSeek V4 Pro's endpoint also constructed correctly, but the
configured model was unavailable through Fireworks at execution time.

### gx controls — 2026-09-18

No gx request was dispatched. gx `1.0.16+gx.12` exposes maximum turns,
effort, tools, and output format in its headless command, but no enforceable
numeric generated-token ceiling. Plan 016 prohibited uncapped requests, so
the gx control is `blocked/inconclusive` with zero requests and zero spend.
The layer verdicts do not rely on gx wire equivalence or reachability.

### Compatibility variants — 2026-09-18

- A GLM 5.3 Flash direct run at `low` effort completed the three-step scenario.
  All three requests placed `reasoning_effort` at the top level. The model
  emitted no reasoning content, so this verifies option placement and history
  continuity but not live reasoning replay. Offline fixtures cover structured
  replay and explicit emptiness.
- A separate GLM 5.3 Flash direct run was cancelled during live sampling after
  two completed tool steps. It returned `cancelled_sampling`, sent no later
  request, and retained the final reservation because complete usage was not
  available. Offline fixtures separately cover cancellation while a local
  tool is blocked.
- Live image variants were explicitly skipped. The reviewed live manifest is
  text-only, and changing the scenario after safety approval would have
  invalidated the evidence boundary. Offline fixtures cover image history;
  production image stripping and placeholders remain H8.

### macOS provider/platform smoke — 2026-09-18

The mac-mini ran the same approved source manifest on `macos/arm64` using only
credentials already present there. The validated package contains eight rows
and 18 attempts:

| representative | effort | direct | Agent | disposition |
|---|---:|---:|---:|---|
| `fireworks/kimi-k3` | `high` | `S,S` | `S,S` | clean in both modes |
| `glm-5.3-flash` | `high` | `S,S` | `S,S` | clean in both modes |
| `muse-spark-1.3-contributor` | `high` | `F,F,F` | `F,F,F` | all six attempts: HTTP-200 `model_noncompliance`, finish `stop` |
| `openrouter/gemini-3.8-flash` | `medium` | `S,S` | `S,S` | clean in both modes |

The remote campaign produced explicit terminal marker
`DONE macos-remote rc=3` and a `completed_negative` summary. Local import
required exact source-manifest and binary hashes, result/schema validation,
canary and credential scans, exact summary/ledger hashes, and full
reservation/usage reconciliation before atomically installing the returned
ledger rows.

This is a four-provider platform smoke, not evidence that every unrun
model/platform pair is OS-independent.

Several Darwin portability failures occurred before accepted live traffic:

- an orphaned gx process triggered the conflicting-process guard;
- the initial wrapper incorrectly trusted an SSH endpoint that always returned
  status zero, so terminal acceptance was changed to require a remote
  `campaign.rc` plus the exact completion marker;
- Darwin cannot execute a Mach-O binary through `/dev/fd`, and its temporary
  paths traverse `/var` symlinks, so the runner now executes an
  immutable, repeatedly revalidated pathname under a private root-local
  temporary directory while configuration inputs remain inherited
  descriptors;
- BSD same-process `flock` semantics do not serialize concurrent goroutines
  like Linux. The remote preflight skips only that exact goroutine test and
  replaces it with a fail-closed 24-process transaction test against one
  private ledger. The unchanged campaign itself uses process boundaries.

Each failed preflight stopped before campaign traffic or preserved only an
already classified safe state. The final run revalidated immutable path,
owner, mode, inode, link count, source manifest, binary hash, tests, race
checks, vet, build, and the interprocess replacement check. These incidents
do not weaken the accepted evidence.

## Budget reconciliation

Pricing came from provider-published sources captured on 2026-09-17:
Fireworks serverless pricing, Z.AI pricing, Meta pricing/rate limits, and the
OpenRouter model API. Embedded Catwalk prices were not trusted for live
accounting.

| allocation | limit | committed spend | held reservations | committed + held | reservations by state |
|---|---:|---:|---:|---:|---|
| Linux | USD 7.500000 | USD 0.096855 | USD 1.297608 | USD 1.394463 | 102 committed, 12 held |
| gx | USD 1.000000 | USD 0.000000 | USD 0.000000 | USD 0.000000 | 0 |
| macOS | USD 1.000000 | USD 0.041092 | USD 0.082170 | USD 0.123262 | 36 committed, 6 held |
| variants | USD 0.500000 | USD 0.000352 | USD 0.001043 | USD 0.001395 | 5 committed, 1 held |
| **total** | **USD 10.000000** | **USD 0.138299** | **USD 1.380821** | **USD 1.519120** | **143 committed, 19 held** |

The ledger contains 162 request reservations and **zero overruns**. Held
reservations remain charged for requests without complete trusted billable
usage; they were never released or reused. Every allocation and the global
USD 10 ceiling remained below its limit.

## Why the verdicts follow

### Fantasy provider layer: fit

The released public provider constructors and `LanguageModel.Stream`
constructed and streamed every provider class. Eight aliases passed two
consecutive exact three-step loops in direct mode on Linux, and the three
non-Meta macOS representatives repeated `S,S`. Meta returned valid streams
but repeatedly disobeyed the requested tool structure; DeepSeek V4 Pro was
rejected by Fireworks with HTTP 404. Neither result identifies a Fantasy
provider-layer defect. Direct fixtures also proved bounded continuation,
manual history, finish/usage accounting, endpoint containment, error
classification, and cancellation through public APIs.

### Released Fantasy Agent: no-go

Live Agent rows show that released `Agent.Stream` can complete the scenario
when a model reports the expected tool-call finish reason, but that is not the
correctness boundary craze needs. Deterministic released-version fixture
`TestStopWithCompleteToolSeparatesDirectDriverFromAgent` sends a complete,
valid tool call with finish reason `stop`. The direct driver continues it and
finishes all three requests; released Agent stops after one request and never
dispatches the tool. Correct behavior cannot depend on every provider/model
choosing `tool-calls` for a complete call. Fixing this requires owning the
loop, not a configuration overlay around Agent.

### Catwalk: no-go

Catwalk has the expected Fireworks, Z.AI, and OpenRouter provider identities,
types, and endpoints. Meta's absence was an allowed overlay. Price, context,
and default-output drift remains independently changing metadata rather than
a correctness verdict.

Two target facts require demonstrated correctness-critical reasoning
corrections rather than ordinary owner-selected defaults:

1. Catwalk marks Kimi K2.7 Code `can_reason:false`. gx's Fireworks contract
   enables high effort, and all four clean direct/Agent attempts sent high
   effort on all three requests. Each attempt recorded three reasoning
   starts/ends and nonzero reasoning content; its aggregate trusted
   reasoning-token total was 197, 72, 184, or 254. Catwalk's capability claim
   must therefore change to reasoning-enabled before gx's effort default can
   compose with it.
2. Catwalk's OpenRouter generator infers `low|medium|high` and default
   `medium` whenever an endpoint lists generic `reasoning` support.
   OpenRouter's public MiniMax M3 model record lists `reasoning` and
   `include_reasoning` but not `reasoning_effort`, and publishes neither
   supported efforts nor a default. gx's provider-verified preset accordingly
   exposes and sends no effort. All four clean direct/Agent attempts made
   three no-effort requests and still emitted reasoning, confirming that
   reasoning capability and effort control are separate. The correction keeps
   `can_reason:true` but clears the unsupported levels and default.

H0's threshold was zero corrections for fit, one isolated correction for a
bounded wrapper, and two or more for no-go. Catwalk therefore cannot be H1's
catalog. The existing small overlay is still needed for credentials, aliases,
owner-selected defaults, Meta direct, and narrow provider overrides; it must
not conceal a second competing catalog.

## H1 handoff

A separate panel-reviewed catalog replacement spike is the next action and
blocks H1. It must carry all 11 alias records and their H0 status so configured
names are not silently lost, compare authoritative small sources or a
deliberately owned craze registry against the fixed effective facts, define
update and provenance rules, and select the catalog input contract.

H1's initial supported set is the eight aliases that passed `S,S` in both
modes. DeepSeek V4 Pro remains recorded as unavailable until Fireworks
confirms a current wire id and it passes the same bounded requalification.
Both Meta aliases remain overlay records, consistent with D-19, but are
unsupported for native tool use until each passes that qualification. Every
requalification also refreshes prices, numeric limits, and endpoint metadata;
a catalog entry alone cannot promote one of these three rows to supported.

After the catalog decision, H1 may add Fantasy's provider package and must implement
a craze-owned `LanguageModel.Stream` loop covering:

- stream collection and explicit terminal-part validation;
- manual assistant tool-call and tool-result history;
- continuation of every complete valid tool call independent of finish
  reason;
- per-step and aggregate usage/finish accounting;
- mapping provider parts to craze events;
- steering and history replacement at step boundaries;
- sampling and tool cancellation; and
- hard step, tool, time, retry, output, and budget bounds.

The H0 implementation suggests roughly 1.5–2.5 kLOC for that production loop,
plus a comparable amount of scripted/local-stream fixtures. This is a
planning estimate, not an implementation commitment. H1 must refine it before
coding. The direct-read-versus-import choice for gx configuration remains open
in `10-open-questions.md` Q4.

## Safety and validation

- Credentialed calls ran only in the main execution session. No credential,
  credential-bearing configuration, or unreviewed live artifact was sent to a
  subagent or reviewer.
- macOS used only credentials already present on the mac-mini. No credential
  was copied or staged. After local evidence preservation and hash
  reconciliation, the private remote staging tree was removed and the
  pre-existing configuration files were verified free of leftover immutable
  flags.
- Persisted records omit raw prompts, responses, reasoning, images,
  authorization material, provider request identifiers, and generic provider
  error serialization. They retain only allowlisted structural observations,
  hashes, counts, and closed error classes.
- Literal, JSON-escaped, URL-encoded, and base64 forms of credential and
  payload canaries were scanned before evidence acceptance. The final artifact
  and repository scans are part of the repository-verification checkpoint
  below.

## Rerun guidance

No rerun is required to complete H0. If later facts justify one:

1. start from a fresh owner-only results directory and fresh ledger; never
   mutate these accepted rows;
2. reverify current provider prices, credentials by class, numeric output
   ceilings, aliases, endpoints, and exact module checksums before traffic;
3. keep the approved three-step scenario, `MaxRetries=0`, 512-token ceiling,
   deadlines, per-request 2× reservation, three-attempt cap, and `S,S` or
   `F,S,S` pass rule;
4. use a unique reservation prefix and preserve held reservations on unknown
   usage;
5. rerun DeepSeek V4 Pro only after Fireworks confirms a reachable current
   wire id; run gx only after it exposes an enforceable numeric output-token
   ceiling;
6. treat live image coverage as a newly reviewed manifest rather than silently
   extending this text-only campaign; and
7. on macOS, require the same source manifest, pre-existing local credentials,
   immutable binary/path checks, explicit remote terminal marker, local result
   validation, and ledger reconciliation before import.

## Repository verification

**Repository status:** pre-commit verification complete; ready to commit, not
ready to merge.

- CodeRabbit CLI `0.7.6` reviewed the complete uncommitted diff against
  `main`, including this new file after it was marked intent-to-add. The
  all-11 alias disposition, fixture-effort wording, and two record-clarity
  findings were fixed. Its last pass had no remaining content defect; it
  requested completion of this repository checkpoint.
- `go mod tidy` under Go 1.27.1 produced no `go.mod` or `go.sum` drift.
- `make lint && make test && make test-race && make build && make test-cli`
  passed. `make docs` also passed with Zensical strict mode. The first CLI run
  inherited this execution host's four color-suppression variables and failed
  only the test whose baseline requires terminal colors; the focused test and
  complete gate passed with those host overrides removed, matching CI. No
  test or application change was made for that environmental isolation issue.
- The final safety pass checked 272 combined campaign canaries in every
  supported encoding across canonical results, repository changes, and final
  narrative/log records. It checked four resolved credential classes across
  all 295 private artifact files and all 16 changed repository files. No match
  was found. The 106-entry stable evidence/tooling checksum inventory also
  verified exactly.
- `git diff --check` passed, every tracked/untracked path was inspected, and
  the shared `main` worktree remained clean at `5d24c82`.

Commit, open-PR, and CI results necessarily occur after this record's commit;
they are reported on the PR and in the external Plan 016 progress log. The PR
must remain open for the owner's merge decision.

## Related decisions and documents

- [02-architecture.md](02-architecture.md) — direct-stream ownership boundary
- [04-providers-and-catalog.md](04-providers-and-catalog.md) — fixed target and catalog findings
- [07-roadmap.md](07-roadmap.md) — catalog replacement prerequisite and H1 scope
- [08-decisions.md](08-decisions.md) — D-20 through D-23
- [09-references.md](09-references.md) — exact pins, licenses, and provenance
- [10-open-questions.md](10-open-questions.md) — unresolved H1 choices
