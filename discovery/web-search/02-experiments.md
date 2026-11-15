# 02 — Experiments (2026-11-15)

All against `https://mcp.exa.ai/mcp` from one macOS machine on a home network, craze 0.3.0 (`28375511`). The probe (`probe/exa_probe.py`) sends what `caller.call` sends (`internal/harness/websearch/mcp.go`): one JSON-RPC `tools/call` per POST, no `initialize`, `Accept: application/json, text/event-stream`, and `X-Api-Key` when keyed. A sample of one network and one day: rate limits in particular may differ elsewhere.

## E1 — In-session failures (the starting point)

In a native session: 22 searches over two turns, 5 rate limited, all in batches of 5–6 parallel calls. Three of the five came after a key had been stored mid-session. The hint is only added when `!s.keyed` (`service.go:271`), which confirms the session was still unkeyed.

## E2 — What the endpoint offers

```
python3 probe/exa_probe.py --keyed list --tools web_search_exa,web_fetch_exa,web_search_advanced_exa
```

- `web_search_exa`: `query` and `objective` (both required), `numResults` (default 10). craze sends `numResults: 8` and `objective` = the query (`exa.go:40`).
- `web_fetch_exa`: `urls` (array), `maxCharacters` (default 3000). craze sends one URL and 50000.
- `web_search_advanced_exa`, available only when named in the URL's `tools` parameter, which replaces the default tool list:
  - result controls: `numResults`, `type` (`auto|lite|fast|instant`), `category`, `includeDomains`/`excludeDomains`, `start/endPublishedDate`, `start/endCrawlDate`, `includeText`/`excludeText`;
  - content controls: `enableHighlights`, `highlightsMaxCharacters`, `textMaxCharacters`, `contextMaxCharacters`, `enableSummary`;
  - freshness and crawling: `maxAgeHours`, `livecrawlTimeout`, `subpages`;
  - others: `userLocation`, `moderation`, `additionalQueries`.

## E3 — Keyless burst

```
python3 probe/exa_probe.py burst -n 8
```

3 of 8 succeeded and 5 got **HTTP 429**, answered in 0.1–0.2 s. Headers: `retry-after: 1`, `x-ratelimit-limit: 0`, `x-ratelimit-remaining: 0`, `x-ratelimit-reset: <ms epoch>`. The body is a JSON-RPC error:

> You've hit Exa's free MCP rate limit. Create a free Exa account to get your own usage allowance and rate limits. No payment method required. Fix: Create API key at https://dashboard.exa.ai/api-keys , then either: Set the header … Or use the URL …

craze reads none of the body, because any non-200 reply is `failStatus` (`mcp.go:162-164`).

## E4 — Keyless burst with retry

```
python3 probe/exa_probe.py burst -n 8 --retries 3
```

Each retry waits `Retry-After × (1 + random[0,1)) × attempt`. Result: **8 of 8 succeeded**. Three calls needed no retry, four needed one, and one needed two; the slowest took 4.6 s including waits. That is well inside `CallTimeout` (25 s, `mcp.go:34`).

## E5 — Keyed bursts

```
python3 probe/exa_probe.py --keyed burst -n 8
python3 probe/exa_probe.py --keyed burst -n 24
```

- 8 parallel: 8 of 8 succeeded.
- 24 parallel: **12 succeeded and 12 failed**, with a different shape from E3: **HTTP 200** with `result.isError: true` and text `web_search_exa error (429): Rate limit exceeded: 10 requests per second for your team. Wait at least 1 second before retrying, and reduce your request rate`. There is no `Retry-After` header. craze's `statusOf` (`mcp.go:377-387`) already maps this text to status 429, so the model sees `Web search rate limited (HTTP 429)` with no hint, because the session is keyed.

## E6 — A fresh session uses the stored key

```
craze prompt --json --model anthropic/claude-haiku-4-5-20251001 --web-search \
  "In ONE message, call the websearch tool 8 times in parallel with queries 'a1 markdown tasks' … 'a8 markdown tasks'. Then reply 'done'."
```

All 8 `websearch` tool events ended `completed`. Compare E1, where a session opened before the key was stored lost 3 of 6.

## E7 — Fetch error shapes

```
python3 probe/exa_probe.py --keyed call web_fetch_exa '{"urls":["https://silverbullet.md/Tasks"],"maxCharacters":50000}'
python3 probe/exa_probe.py --keyed call web_fetch_exa '{"urls":["https://no-such-host-wren-probe.example/x"],"maxCharacters":50000}'
```

| URL | Reply |
|---|---|
| A real 404 (`curl -sSL -o /dev/null -w '%{http_code}'` gives `404`) | HTTP 200, `isError`, `Error fetching URL(s): <url>: CRAWL_NOT_FOUND` |
| A host with no DNS | HTTP 200, `isError`, `Error fetching URL(s): <url>: CRAWL_UNKNOWN_ERROR` |
| Two URLs, one bad (not something craze sends) | HTTP 200, no `isError`: the good page alone; the bad URL is silently dropped |

Not reproduced: Exa's documented keyless 403 over IPv6 or a proxied path (https://docs.exa.ai/reference/exa-mcp, Troubleshooting).

## E8 — Result size and ranking

```
python3 probe/exa_probe.py --keyed compare
```

Query: `Obsidian Tasks plugin due date emoji format`.

| Call | Bytes | Results / hosts |
|---|---|---|
| craze today (`web_search_exa`, n=8, objective = query) | 22,264 (~5.5k tokens) | 8: 4× publish.obsidian.md, 2× taskforge.md, uphy.github.io, github.com |
| The same, with a model-written `objective` ("rank the official docs first, exclude blogs…") | 22,264, **identical** | no visible effect in one trial; the reply came back in 0.3 s, so it may have been cached |
| `web_search_exa`, n=4 | 11,110 | 4 |
| `web_search_advanced_exa`, n=5, `includeDomains: [publish.obsidian.md, github.com]`, highlights at 1500 | 10,256 | 5, all official docs or the repo |

The advanced tool replies in **JSON** (`requestId`, `results[]` with `url`, `title`, `highlights`, sometimes `publishedDate` and `author`, plus `searchTime` and `costDollars`), not the `Title:`/`URL:` block text `parseExa` reads (`exa.go:63-108`), which would call this reply a failure. `highlightsMaxCharacters` is not a hard cap: one result returned 6,532 characters against a limit of 1,500.

## E9 — The cost of a result in context

Across E3 to E8, a `web_search_exa` result averaged about 2.8 KB, so the default of 8 results costs about 22 KB of context per search. That is under `tool.MaxBytes` (50 KiB, `internal/harness/tool/truncate.go:22`), so nothing was truncated. The research session's 22 searches were roughly 120k tokens of results. Issue #131's eval saw short excerpts lead Haiku to skip fetching; in this session, excerpts were often long enough that fetching was rarely needed. Both observations are real, and they pull in different directions on result size (WQ-5).
