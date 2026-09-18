# 11 — H0 progress and results

This is the durable, sanitized record of Plan 016, the H0 provider-stack fit
spike, and of the review that followed it. It is self-contained for future
native-harness implementers who cannot access the local supporting artifacts.

**Status:** complete. H0's own outcome was "completed-negative"; the review
below overturned most of the negative part, and H1 is unblocked.

**Completed:** 2026-09-18

**Planning baseline:** `origin/main` at `57357fe` (Plan 015, PR #16)

**Execution baseline:** `origin/main` at `5d24c82` (the subsequent
main-branch cancellation-test fix, PR #17)

**External supporting artifacts:**
`~/.claude/plans/craze/016-native-harness-h0/` (local-only; not required to
understand this record)

## Outcome

| layer | exact candidate | H0 verdict | after review | consequence |
|---|---|---|---|---|
| Fantasy provider layer | `charm.land/fantasy v0.43.2` provider constructors and `LanguageModel.Stream` | fit, Meta unexplained | **fit, including Meta** | H1 uses the released provider layer (D-20) |
| Fantasy agent loop | `charm.land/fantasy v0.43.2` `Agent.Stream` | no-go | **fit behind a finish-normalizing wrapper** | H1 runs on `Agent.Stream` plus a roughly 40-line `LanguageModel` wrapper (D-21) |
| Catwalk catalog | `charm.land/catwalk v0.52.43` embedded catalog | no-go; replacement spike blocks H1 | **not embedded; no blocking spike** | a small craze-owned model table seeded from gx (D-22) |

H0's observations are reliable and are kept below as recorded. Four of its
conclusions were not supported by them; the review section says which and
why. DeepSeek V4 Pro's rejection, the skipped gx controls, and the skipped
live image variants are bounded row or variant dispositions.

## Review — 2026-09-18

H0 was executed by one model and reviewed by another the next day. The review
checked the load-bearing claims against the released module source and the
raw result files, then ran what H0 had left unexplained. Its working files
are in the artifact directory under `followup-2026-09-18/`.

### Confirmed

- Streamed `Agent.Stream` dispatches tools and continues only when the step's
  finish reason is `tool-calls` (`agent.go`, the two
  `stepFinishReason == FinishReasonToolCalls` checks). The OpenAI provider's
  non-streaming path rewrites `stop` to `tool-calls` when calls are present;
  its streaming path does not. H0's fixture reproduces the dropped call.
- The Catwalk entries are as H0 described: Kimi K2.7 Code is
  `can_reason:false`, and OpenRouter MiniMax M3 carries `low|medium|high`
  with default `medium`.

### Corrected

1. **`Agent.Stream` is not a no-go.** Plan 016 asked whether the `stop` hazard
   is observed live. It was not: all 46 successful attempts, and the 13 Meta
   loops run in the review, finished `tool-calls, tool-calls, stop`. H0 wrote
   that fixing it "requires owning the loop, not a configuration overlay". A
   `LanguageModel` wrapper that rewrites the finish part does it through the
   public API. Four fixtures pass under `-race` against the released module:
   released Agent drops the call; the wrapped model dispatches it and
   continues; a `length` finish with a partial call still dispatches nothing;
   Meta's empty `stop` becomes an error. In Plan 016's own vocabulary that is
   "conditional fit: bounded wrapper".
2. **The Meta failures were not model noncompliance.** Fourteen of 18 Meta
   attempts were an HTTP 200 stream with one finish chunk and nothing else:
   no text, no tool call, no usage, finish `stop`. A model that disobeys a
   tool protocol still emits something. Raw SSE capture showed Meta returns
   exactly that stream when reasoning exhausts `max_tokens` (reproduced twice
   of twice at an 80-token ceiling) and rate limits were nowhere near (3,000
   requests and 4M tokens remaining). H0's 512-token ceiling sat at the edge:
   its successful Meta steps used 283–438 output tokens. Through released
   `Agent.Stream` at an 8,192-token ceiling, `muse-spark-1.3` and
   `muse-spark-1.3-contributor` each passed 5 of 5 loops, and
   `muse-spark-1.3` passed 3 of 3 at 512 with a shorter prompt. H0 could not
   have found this from its artifacts, because its recorder discarded
   response bodies; its three-attempt pass rule then absorbed the failures
   instead of forcing a diagnosis. Two `invalid_tool` attempts remain
   unexplained for the same reason and did not recur.
3. **Catwalk "no-go" rested on a threshold the plan set for itself.** Two
   corrections among nine backed targets is ordinary overlay work, which
   D-04 already expected. The sound reason not to embed Catwalk is that 11
   owner-verified models fit a table craze must own anyway (D-22). That needs
   no separate panel-reviewed spike.
4. **DeepSeek V4 Pro's 404 was a stale wire id** in gx's configuration.
   `accounts/fireworks/models/deepseek-v4-pro-0813` answers a plain chat
   request with 200; Catwalk lists it. It still needs one tool loop.

### What this says about the process

H0 wrote roughly 5.6k lines of Go and 2.8k of Python, with a locked budget
ledger, canary scans, and checksum manifests, to spend USD 0.14. The safety
work was careful and nothing leaked, but it also threw away the one thing
needed to explain the only interesting failure. Later spikes should be a
throwaway `main` that keeps raw responses with credentials stripped (`07`).

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
| `muse-spark-1.3` | `muse-spark-1.3` | `high` | `S,F,F` | `F,F,F` | attempt cap exhausted; the probe classed the empty HTTP-200 streams `model_noncompliance` (see Review), plus one `invalid_tool` |
| `muse-spark-1.3-contributor` | `muse-spark-1.3-contributor` | `high` | `F,F,F` | `S,F,F` | attempt cap exhausted; the probe classed the empty HTTP-200 streams `model_noncompliance` (see Review), plus one `invalid_tool` |
| `openrouter/minimax-m3` | `minimax/minimax-m3` | none | `S,S` | `S,S` | clean in both modes |
| `openrouter/gemini-3.8-flash` | `google/gemini-3.8-flash` | `medium` | `S,S` | `S,S` | clean in both modes |

Eight aliases passed both modes cleanly. Both Meta aliases constructed,
reached their intended endpoint, and returned valid HTTP streams. H0 read
their failures as model noncompliance; the Review shows they were reasoning
truncated by the 512-token ceiling. DeepSeek V4 Pro's endpoint also
constructed correctly, but gx's configured wire id is no longer served.

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
| `muse-spark-1.3-contributor` | `high` | `F,F,F` | `F,F,F` | all six attempts: empty HTTP-200 stream, finish `stop`, classed `model_noncompliance` (see Review) |
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

## H0's verdict reasoning, as recorded

Kept for the record; the review above supersedes it where they disagree.

- **Provider layer, fit.** Every provider class constructed and streamed
  through the released public API. Eight aliases passed two consecutive
  three-step loops in direct mode on Linux, and the three non-Meta macOS
  representatives repeated `S,S`.
- **Released Agent, no-go.** Fixture
  `TestStopWithCompleteToolSeparatesDirectDriverFromAgent` sends a complete,
  valid tool call with finish reason `stop`; the direct driver continues it
  and released Agent stops after one request. H0 concluded that correctness
  cannot depend on the provider choosing `tool-calls`. True, and the wrapper
  is what removes the dependency.
- **Catwalk, no-go.** Kimi K2.7 Code is marked `can_reason:false` although
  all four clean attempts sent high effort on three requests and recorded
  197, 72, 184, and 254 reasoning tokens. Catwalk's OpenRouter generator
  infers `low|medium|high` from generic `reasoning` support, while
  OpenRouter's MiniMax M3 record publishes no effort contract and gx sends
  none; all four clean attempts still emitted reasoning. H0's threshold was
  zero corrections for fit, one for a bounded wrapper, two for no-go.

## H1 handoff

H1 is next and nothing blocks it.

- **Turn runner**: released `Agent.Stream` behind the finish-normalizing
  `LanguageModel` wrapper, behind the harness's own interface (D-21). The
  wrapper and its four fixtures are the first code to port from the review
  follow-up. H0 sized the alternative, a craze-owned `LanguageModel.Stream`
  loop, at roughly 1.5–2.5 kLOC plus comparable fixtures.
- **Catalog**: a craze-owned model table seeded from gx's configuration
  (D-22); `10` Q4 still decides read versus import.
- **Models**: all 11 aliases are carried; ten are qualified. DeepSeek V4 Pro
  needs the `-0813` wire id and one clean tool loop (D-24).
- **Output ceilings** leave room for reasoning, and an empty `stop` step with
  no content and no usage is a failed step (D-25).
- **Repository**: the harness lives here, hidden, with D-02's import rule
  enforced by lint (D-26).

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

## Qualifying a model later

No rerun of H0 is needed. To qualify a new or corrected alias (DeepSeek V4
Pro first), run the three-step scenario from "Experiment contract" through
the path H1 ships, with `MaxRetries=0` and deadlines, and:

1. give the model an output ceiling that leaves room for its reasoning; do
   not reuse H0's 512 tokens;
2. keep the raw response with credentials stripped, and diagnose any failure
   rather than counting it against an attempt cap;
3. check the wire id against the provider's current model list first; and
4. treat live image coverage as H8 work with its own scenario.

## Repository record

The toolchain floor (Go 1.27.0, toolchain 1.27.1, golangci-lint 2.13.2) landed
on its own in PR #27. PR #26 carries these discovery documents only: no
production harness code and no Fantasy or Catwalk dependency. H0's final
safety pass found no credential or canary in the 295 private artifact files
or the repository diff.

## Related decisions and documents

- [02-architecture.md](02-architecture.md) — the wrapper and the turn loop
- [04-providers-and-catalog.md](04-providers-and-catalog.md) — fixed target and catalog findings
- [07-roadmap.md](07-roadmap.md) — H1 scope
- [08-decisions.md](08-decisions.md) — D-19 through D-26
- [09-references.md](09-references.md) — exact pins, licenses, and provenance
- [10-open-questions.md](10-open-questions.md) — unresolved H1 choices
