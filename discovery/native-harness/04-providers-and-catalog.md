# 04 — Providers and catalog

## H0 outcome

H0 evaluated the provider, loop, and catalog layers independently. A review
on 2026-09-18 re-ran the rows H0 could not explain and corrected four of its
conclusions; `11` records both.

1. **Fantasy `v0.43.2` provider layer: fit**, for Fireworks, Z.AI,
   OpenRouter, and Meta.
2. **Fantasy `v0.43.2` `Agent.Stream`: fit behind a wrapper.** The streamed
   loop drops a complete tool call whose finish reason is `stop`. No live
   model did that, and a small `LanguageModel` wrapper removes the hazard, so
   H1 runs on `Agent.Stream` (`02`, D-21).
3. **Catwalk `v0.52.43`: not embedded.** The catalog is a small craze-owned
   model table seeded from gx's configuration; Catwalk is a reference to
   consult when adding a model (D-22). Nothing blocks H1.
4. **Overlay remains required.** It owns credentials, short aliases,
   owner-selected defaults, Meta direct, and narrowly documented provider
   overrides. Whether it reads gx's TOML or imports it remains open in `10`
   Q4.

Fantasy remains Apache-2.0 with its NOTICE; Catwalk is MIT. H0 added neither
module to craze's root dependencies.

## Fixed target results

`S,S` means two consecutive complete three-step sentinel loops. `F` means a
structurally recorded attempt that did not pass. `R` is the terminal
provider rejection observed for the configured alias. "Catalog note" counts
only contradictions that require correctness-critical correction; aliases,
owner-selected defaults, and metadata drift are ordinary overlay data.

| gx alias | wire model | effective effort | direct | Agent | catalog note |
|---|---|---:|---:|---:|---|
| `fireworks/kimi-k3` | `accounts/fireworks/models/kimi-k3` | `high` | `S,S` | `S,S` | backed; gx selects its default/allowlist |
| `fireworks/qwen3p8-max` | `accounts/fireworks/models/qwen3p8-max` | `high` | `S,S` | `S,S` | backed; gx supplies effort values absent from the entry |
| `fireworks/deepseek-v4-pro` | `accounts/fireworks/models/deepseek-v4-pro` | `high` | `R` | `R` | gx's wire id is stale (HTTP 404); `…/deepseek-v4-pro-0813` answers and Catwalk lists it; catalog price is stale |
| `fireworks/kimi-k2p7-code` | `accounts/fireworks/models/kimi-k2p7-code` | `high` | `S,S` | `S,S` | **correction 1:** Catwalk says `can_reason:false`; all 12 high-effort requests emitted reasoning |
| `fireworks/deepseek-v4-flash` | `accounts/fireworks/models/deepseek-v4-flash-0731` | `high` | `S,S` | `S,S` | backed; owner effort allowlist composes with the entry |
| `glm-5.3` | `glm-5.3` | `max` | `S,S` | `S,S` | backed; gx selects its effort vocabulary/default |
| `glm-5.3-flash` | `glm-5.3-flash` | `high` | `S,S` | `S,S` | backed; gx selects its effort vocabulary/default |
| `muse-spark-1.3` | `muse-spark-1.3` | `high` | `S,F,F` | `F,F,F` | expected Meta overlay; failures were the probe's 512-token ceiling; **review: 8 of 8 Agent loops clean** |
| `muse-spark-1.3-contributor` | `muse-spark-1.3-contributor` | `high` | `F,F,F` | `S,F,F` | expected Meta overlay; failures were the probe's 512-token ceiling; **review: 5 of 5 Agent loops clean** |
| `openrouter/minimax-m3` | `minimax/minimax-m3` | none | `S,S` | `S,S` | **correction 2:** Catwalk invents effort levels/default absent from OpenRouter's model contract and gx |
| `openrouter/gemini-3.8-flash` | `google/gemini-3.8-flash` | `medium` | `S,S` | `S,S` | backed; current public price differs from the embedded entry |

The eight other aliases passed both modes cleanly. Every one of the 46
successful attempts finished its steps `tool-calls, tool-calls, stop`; no
live model reported `stop` on a tool turn.

### Meta: what the failures were

H0 labelled the Meta failures model noncompliance. They were not. Fourteen of
the 18 Meta attempts were an HTTP 200 stream holding a single finish chunk:
no text, no tool call, no usage, `finish_reason: "stop"`. H0's recorder
discarded response bodies, so the review captured raw SSE directly:

- At the probe's settings Meta returned a normal tool call; rate-limit
  headers showed 3,000 requests and 4M tokens remaining, so throttling is
  ruled out.
- With `max_tokens` lowered to 80, below the model's reasoning, Meta
  returned exactly the empty `stop` stream, twice out of two.

Meta reports reasoning that exhausts `max_tokens` as an empty `stop` with no
usage chunk rather than `length`. H0's 512-token ceiling sat at the edge:
the Meta steps that succeeded used 283–438 output tokens, mostly reasoning.
Through released `Agent.Stream` with an 8,192-token ceiling both aliases then
passed 10 of 10 three-step loops, and `muse-spark-1.3` passed 3 of 3 at 512
with a shorter prompt. The two remaining H0 failures (`invalid_tool`) cannot
be diagnosed from the sanitized artifacts and did not recur.

Two rules follow (D-25): an output ceiling must leave room for reasoning, and
an empty `stop` step with no content and no usage is a failed step. Fantasy
passes it through as a clean stop; D-21's wrapper turns it into an error.

## H1 alias disposition

H1 carries all 11 alias records so configured names are not silently lost.
Ten are qualified for native tool use: the eight that passed H0 in both modes
and both Meta aliases. DeepSeek V4 Pro needs its wire id corrected to
`accounts/fireworks/models/deepseek-v4-pro-0813` (a plain chat request to it
returns 200) and one clean three-step tool loop before it is listed (D-24).
Qualifying a new alias means that loop, through the path H1 ships, with an
output ceiling that leaves room for reasoning.

## Why Catwalk is not embedded

Catwalk contains the expected Fireworks, Z.AI, and OpenRouter provider
identities/types/endpoints. Meta's absence was an explicitly allowed overlay.
Price, context, and default-output drift is tracked as non-authoritative
metadata drift because those fields require current provider sources anyway.

The two reasoning contradictions above are demonstrated contract mismatches,
not owner-selected defaults:

1. Catwalk's `can_reason:false` is a supported-capability claim for Kimi K2.7
   Code. gx's Fireworks contract enables high effort, and all four clean H0
   attempts sent high effort on three requests each. Every request emitted a
   balanced reasoning part; aggregate trusted reasoning-token totals per
   attempt were 197, 72, 184, and 254. The catalog must change the capability
   to reasoning-enabled before gx's effort default can compose with it.
2. Catwalk's OpenRouter generator treats generic `reasoning` support as proof
   of `low|medium|high` effort and a `medium` default. OpenRouter's public
   MiniMax M3 record instead lists `reasoning` and `include_reasoning`, omits
   `reasoning_effort`, and publishes neither supported efforts nor a default.
   gx's provider-verified preset therefore exposes and sends no effort. All
   four clean H0 attempts made three no-effort requests and still emitted
   reasoning, confirming that reasoning capability and effort control are
   separate. The required correction retains `can_reason:true` but removes
   Catwalk's unsupported effort values and default.

Two corrections among nine backed targets would be ordinary overlay work, and
D-04 expected the overlay to win on effort lists. The reason not to embed
Catwalk is simpler: the harness targets 11 models whose owner-verified facts
already live in gx's TOML, Meta is absent, and prices need provider sources
anyway. A 40-provider embedded catalog adds a dependency and an update
cadence without removing the table craze has to own. Catwalk is still worth
reading when adding a model; it carried the current DeepSeek V4 Pro wire id
that gx lacked.

## Provider and request behavior

- OpenAI-compatible targets use Fantasy's `providers/openaicompat`; OpenRouter
  uses `providers/openrouter` with nested `reasoning.effort`.
- H0 set `MaxRetries=0`, a 512-token output ceiling, per-request and
  per-attempt deadlines, and a pre-dispatch budget reservation. The ceiling
  was too low for high-effort reasoning models and caused the Meta failures
  above. Production retry policy remains owned by the later harness plan.
- Meta's rate-limit headers (`x-ratelimit-remaining-requests`,
  `x-ratelimit-remaining-tokens`) are present on every response.
- The GLM 5.3 Flash low-effort live variant passed with top-level
  `reasoning_effort`. It emitted no reasoning content, so the run verified
  field placement and history continuity but did not add live reasoning
  replay evidence. Offline fixtures cover replay ordering and explicit
  emptiness.
- A live GLM 5.3 Flash sampling cancellation returned the expected
  `cancelled_sampling` class without a later request. Offline fixtures also
  cover cancellation while a local tool is blocked.
- The approved live manifest is text-only, so live image variants were
  skipped rather than changing the reviewed scenario. Offline fixtures cover
  image history placement; production stripping/placeholders remain H8.
- `stream_tool_calls=false` is not a chat-completions switch in the current gx
  sampling path; it controls an optional Responses field. Fantasy's providers
  stream and reconstruct chat-completions tool input.
- Current gx sends ordinary top-level effort for Z.AI GLM. H0 found no basis
  for the earlier claim that the effective gx request adds a `thinking`
  object.
- Reasoning replay is not uniformly lossy: Fantasy has explicit
  OpenAI-compatible and OpenRouter replay paths. H1 must test and preserve
  structured fields per provider rather than blanket-strip them.

## Cost inputs

H0 did not trust embedded catalog prices for live accounting. It verified
prices from provider-published sources dated 2026-09-17 and applied a 2×
reservation before every request. The final totals, held reservations, source
URLs, and rerun limits are in `11-h0-progress-and-results.md`.
