# 04 — Providers and catalog

## Layers

1. **catwalk** (`charm.land/catwalk`, MIT, embedded via `pkg/embedded`,
   optional refresh from the hosted endpoint): providers with id, type,
   endpoint, `$ENV` key reference, headers, and models with context window,
   max tokens, cost per 1M in/out/cached, `can_reason`, `reasoning_levels`,
   default effort, `supports_attachments`.
2. **Overlay** (`~/.craze-acp/config.toml`, D-04): keys, short aliases,
   per-model overrides (effort list, vision flag, context window), extra
   providers catwalk lacks (Meta direct at `api.meta.ai/v1`), and header
   overrides (catwalk's OpenRouter headers advertise crush; replace them).
   The overlay can also point at `~/.grok/providers.toml` and
   `config.toml` so the user's existing keys and gx aliases work with no
   new config; craze already depends on BurntSushi/toml.
3. **Provider factory**: catwalk `type` → fantasy package.

| catwalk type | fantasy package | notes |
|---|---|---|
| `openai-compat` | `providers/openaicompat` | Fireworks, Z.AI, Meta, xAI, DeepSeek |
| `openrouter` | `providers/openrouter` | own reasoning/usage hooks |
| `openai` | `providers/openai` (`WithUseResponsesAPI` per model) | openai-api |
| `anthropic` / `google` / … | available, not targeted | |

## Reasoning effort

gx sends a top-level `reasoning_effort` on chat completions. fantasy's
openaicompat `ProviderOptions.ReasoningEffort` writes exactly that field,
with values `none minimal low medium high xhigh max`. The harness advertises
effort as a config option; craze's existing effort picker works unchanged.
Per-provider shapes that differ (opencode's tables, pi's `thinkingFormat`):

| provider | wire shape |
|---|---|
| Fireworks, Meta, OpenAI-compatible default | `reasoning_effort` |
| Z.AI GLM | `thinking: {type: enabled}` + `reasoning_effort` (via `ExtraBody`) |
| DeepSeek | `thinking: {type}` + effort; reasoning arrives as `reasoning_content` |
| OpenRouter | `reasoning: {effort}` (the openrouter package handles it) |
| OpenAI responses | `reasoning.effort` |

Where catwalk and gx disagree on a model's effort list (kimi-k3, glm-5.3),
the overlay wins.

## Quirks to smoke in H0

- **Tool-call streaming on Fireworks, OpenRouter, Meta.** gx sets
  `stream_tool_calls = false` for these. fantasy always streams and
  reassembles deltas with a JSON-repair fallback. Verify a three-step tool
  loop per model before anything else is built.
- **GLM 5.3 rejects images anywhere in the request.** catwalk and gx agree
  (`supports_attachments: false`). Strip images per model before sending,
  with a loud placeholder in the text (silent stripping induces
  hallucination, grok's lesson).
- **Reasoning replay across tool turns** on chat-completions providers is
  lossy (GLM, Kimi, Muse). Check what fantasy replays; expect to strip.
- **Reasoning across a model switch**: strip reasoning parts when the model
  that produced them is not the one being called.
- **xAI direct** needs a funded API key (the user's key returned 402 in an
  earlier smoke).

## Retry

fantasy retries inside the loop (`WithMaxRetries`, `OnRetry`,
`OnAuthRefresh`). pi disables SDK retries because they mask quota and
billing errors; opencode retries in the session with a regex of retryable
messages and never retries context overflow. Decision D-08: fantasy retries
stay on for transport and 5xx, but auth, quota, and context-overflow errors
are classified non-retryable and surfaced immediately; overflow triggers one
compact-and-retry in the session.

## Cost

`cost = in × price_in + out × price_out + cached × price_cached` per
assistant message from catwalk prices; tiered pricing only if a targeted
model needs it.
