# 04 — Providers and catalog

## H0 outcome

H0 evaluated the provider, loop, and catalog layers independently:

1. **Fantasy `v0.43.2` provider layer: fit.** H1 may use the released public
   provider constructors and `LanguageModel.Stream` API.
2. **Fantasy `v0.43.2` `Agent.Stream`: no-go.** The released loop stops on a
   complete tool call whose finish reason is `stop`; craze must own the
   direct-stream step loop described in `02`.
3. **Catwalk `v0.52.43`: no-go as H1's catalog.** Two target models need
   correctness-critical reasoning-capability corrections, crossing H0's
   threshold. A separate panel-reviewed replacement spike blocks H1.
4. **Overlay remains required.** It owns credentials, short aliases,
   owner-selected defaults, Meta direct, and narrowly documented provider
   overrides. Whether it reads gx's TOML or imports it remains open in `10`
   Q4.

Fantasy remains Apache-2.0 with its NOTICE; Catwalk was evaluated under MIT
but is not selected. H0 added neither module to craze's root dependencies.

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
| `fireworks/deepseek-v4-pro` | `accounts/fireworks/models/deepseek-v4-pro` | `high` | `R` | `R` | backed; Fireworks returned HTTP 404; catalog price is stale |
| `fireworks/kimi-k2p7-code` | `accounts/fireworks/models/kimi-k2p7-code` | `high` | `S,S` | `S,S` | **correction 1:** Catwalk says `can_reason:false`; all 12 high-effort requests emitted reasoning |
| `fireworks/deepseek-v4-flash` | `accounts/fireworks/models/deepseek-v4-flash-0731` | `high` | `S,S` | `S,S` | backed; owner effort allowlist composes with the entry |
| `glm-5.3` | `glm-5.3` | `max` | `S,S` | `S,S` | backed; gx selects its effort vocabulary/default |
| `glm-5.3-flash` | `glm-5.3-flash` | `high` | `S,S` | `S,S` | backed; gx selects its effort vocabulary/default |
| `muse-spark-1.3` | `muse-spark-1.3` | `high` | `S,F,F` | `F,F,F` | expected Meta overlay; HTTP 200 model noncompliance exhausted the cap |
| `muse-spark-1.3-contributor` | `muse-spark-1.3-contributor` | `high` | `F,F,F` | `S,F,F` | expected Meta overlay; HTTP 200 model noncompliance exhausted the cap |
| `openrouter/minimax-m3` | `minimax/minimax-m3` | none | `S,S` | `S,S` | **correction 2:** Catwalk invents effort levels/default absent from OpenRouter's model contract and gx |
| `openrouter/gemini-3.8-flash` | `google/gemini-3.8-flash` | `medium` | `S,S` | `S,S` | backed; current public price differs from the embedded entry |

The two Meta rows were reachable and returned valid HTTP streams; their
failures were tool-structure noncompliance, not transport or Fantasy
provider-construction failures. DeepSeek V4 Pro's HTTP 404 is retained as a
provider rejection for that alias rather than evidence against the provider
library. The eight other aliases passed both modes cleanly.

## H1 alias disposition

The catalog replacement spike carries all 11 alias records and their H0
status so configured names are not silently lost. H1's initial supported set
is the eight aliases that passed `S,S` in both modes. DeepSeek V4 Pro remains
in the input registry as unavailable until Fireworks confirms a current wire
id and it passes requalification. Both Meta aliases remain overlay records,
consistent with D-19, but are unsupported for native tool use until each
passes the same bounded consecutive-pass scenario. Requalification must also
refresh prices, limits, and endpoint metadata. H1 must not advertise any of
these three negative rows as supported merely because the selected catalog
contains an entry.

## Why Catwalk is a no-go

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

H0's acceptance rule was zero such corrections for fit, one isolated
correction for a bounded wrapper, and two or more for no-go. Catwalk therefore
cannot be H1's catalog. The replacement investigation should compare smaller
authoritative sources or a craze-owned target registry rather than disguising
a second catalog as an overlay.

## Provider and request behavior

- OpenAI-compatible targets use Fantasy's `providers/openaicompat`; OpenRouter
  uses `providers/openrouter` with nested `reasoning.effort`.
- H0 set `MaxRetries=0`, a 512-token output ceiling, per-request and
  per-attempt deadlines, and a pre-dispatch budget reservation. Production
  retry policy remains owned by the later harness plan.
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
