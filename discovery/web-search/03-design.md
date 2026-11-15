# 03 — Design (proposed)

One section per area, in the order of [04](04-roadmap.md). Each keeps plan 051's invariants:
- the key goes only to the provider's own endpoint, in `X-Api-Key`, and only when the session's redactor holds it;
- every error the model reads is a `*tool.WebError` whose text carries no cause, URL of the request, or header;
- a rate limit is never presented as "no results";
- consent is asked at most once per session.

## A. Rate limits: retry, then cap (#138)

**Retry, inside the caller.** `caller.call` (`internal/harness/websearch/mcp.go:114-143`) retries a rate-limited attempt within the same `CallTimeout` deadline:
- **What counts as rate limited.** Both shapes from [02](02-experiments.md): an HTTP 429 reply (keyless, E3), and an `isError` result whose text `statusOf` maps to 429 (keyed, E5).
- **The wait.** `Retry-After` in seconds when present, else 1 s, times `(1 + jitter) × attempt`, capped so the call still finishes inside its deadline. If the next wait would pass the deadline, return the 429 failure at once rather than time out.
- **Attempts:** at most 3 (WQ-1). E4 recovered every call in a batch of 8 with at most 2 retries.
- **Cancel.** A cancel during a wait is an abort (`failAborted`), as today.
- **Reporting.** `Config.Report` still sees one call (`tool.WebCall`). Optionally it gains a `Retries` count, which #141 can show.

**Cap, in the service.** A semaphore in `Service` (`service.go:131`) bounds calls in flight to N per session (WQ-2, default 4). Children share it already, because `forChild` hands them the parent's service (`internal/harness/web.go:61-66`). Time spent waiting for a slot does not count against `CallTimeout`, as time on the consent card already doesn't. The cap matters for keyed sessions too: E5's limit is per team, and sub-agents plus parallel batches can exceed 10 requests per second.

**Tests.** `mcp_test.go` already runs a fake MCP server, so the following are fixtures:
- 429 then 200 succeeds;
- an `isError` 429 then success succeeds;
- `Retry-After: 30` with 25 s left fails fast with the 429 text;
- a cancel mid-wait aborts;
- N+1 concurrent calls see at most N on the server.

## B. The key lifecycle and the 429 hint (#137)

**B1, the hint (small).** `webError` (`service.go:255-284`) builds the unkeyed 429 text. Proposed:
> `Web search rate limited (HTTP 429) on Exa's keyless endpoint. Store a key with 'craze auth login websearch/exa' (or set EXA_API_KEY); a key stored now is used from the next session.`

The text is chosen when the error is built: if a usable key now exists (env or stored) that the session did not open with, it says so. That needs the service to be able to read the current key source, which B2 provides anyway. Without B2, the hint names both ways and the next-session rule.

**B2, mid-session pickup (medium).** The redactor already learns a key stored mid-session at its next turn (`configuration.md` § Keys stored while a session runs). At that same point, the session hands the web service the new usable key. The service swaps its `caller` (key, `keyed`) under a lock, and its scrubber gains the new key without dropping the old one. Calls already in flight finish with the old caller. Invariant: the swap happens only after the redactor holds the key, never before. Children share the service, so they switch with it.

## C. In-session visibility (#141)

One line wherever the TUI shows the session's model and effort:
- `Web search: Exa · keyless` or `· stored key` or `· EXA_API_KEY`;
- after the first call, `· 22 calls, 5 failed (5 rate limited)`.

The counts come from `Config.Report`, which already reports every call (`service.go:117-122`); it needs a failure class added to `tool.WebCall`. Optionally, a one-time inline notice the first time a keyless session hits a 429, with the B1 text. Where that line lives is a TUI decision (WQ-4). `craze prompt --json`'s web counts (`cli.md` § web search counts) could gain the same failure split.

## D. Error text (#139)

In `caller.answer` (`mcp.go:325-369`), an `isError` text matching `Error fetching URL(s): <url>: CRAWL_<CODE>` becomes a `failure` with a new kind or a fixed detail:

| Code | Text the model reads |
|---|---|
| `CRAWL_NOT_FOUND` | `The page was not found (HTTP 404 or no longer there): <url>. Search for its current address.` |
| `CRAWL_UNKNOWN_ERROR` | `Exa could not fetch <url> (the site may be unreachable or block crawlers).` |
| `CRAWL_EMPTY_CONTENT` | `Exa fetched <url> but found no readable text (the page may need JavaScript). Try a search; its excerpts may carry the page's text.` |
| Any other `CRAWL_*` | `Exa could not fetch <url>: <CODE>` |

A keyless HTTP 403 gets a hint like the 429's: the keyless endpoint refuses IPv6 and proxied connections, and a key avoids that. Text changes move goldens; run `scripts/verify/v8.sh`.

## E. Search controls (#140, with #131)

- **Small and default-preserving.** Add an optional `domains` (include list) and `num_results` (1–10) to `websearch`. With neither set, the call is exactly today's `web_search_exa`, so default goldens and eval behaviour stay put. With `domains` set, call `web_search_advanced_exa` with `includeDomains`, `enableHighlights`, and a highlights cap that craze enforces itself (E8). That needs:
  - the endpoint URL to carry `?tools=web_search_exa,web_fetch_exa,web_search_advanced_exa` (the provider table's endpoint, `exa.go:15`);
  - a JSON reply parser beside `parseExa`, producing the same `tool.WebResult` (title, URL, date, content = highlights joined);
  - the parameters in the tool description, which moves goldens.
- **Not proposed:** exposing `objective` to the model. E8 showed no effect in one trial (WQ-3).
- **Measure first.** Use the web eval tasks (`eval/results/2026-11-websearch/`) before and after, and #131's T-WS6 for the excerpt-trust question. Tool-description changes move goldens; the eval's proxy must learn the advanced tool's arguments and JSON reply before it can score this.
