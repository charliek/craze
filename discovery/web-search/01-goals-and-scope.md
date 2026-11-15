# 01 — Goals and scope

## What happened in use (2026-11-15)

A native session (Claude Opus 5.5) was asked to research how Markdown note apps write task metadata, for a feature in another of the owner's projects. It used `websearch` and `webfetch` heavily: 22 searches and 3 fetches over two turns.

- **Rate limits.** Five searches returned `Web search rate limited (HTTP 429). Setting EXA_API_KEY raises the limit`. Every failure was in a batch of five or six parallel calls, which is how models naturally fan out research; batches of two never failed. The model had to notice and re-run the failed queries itself. Nothing told the person the batch had partly failed.
- **A key stored mid-session was not used.** Between the two turns the owner ran `craze auth login websearch/exa`, and `craze auth list` showed `stored key`. The next batch of six in the same session still had three 429s, with the same "Setting EXA_API_KEY" hint. That is documented behaviour (the key is fixed when a session opens), but nothing in the session said so. Working it out took reading `internal/harness/web.go` and `internal/harness/websearch/service.go`.
- **A misleading fetch error.** `webfetch https://silverbullet.md/Tasks` answered `… CRAWL_NOT_FOUND`. The page really is a 404, but the text reads like a tool fault.
- **Large results.** Each search returned about 22 KB, about 5.5k tokens, for 8 results. Many result slots went to SEO articles restating the official documentation the model actually wanted.

What worked: results carried dates and useful excerpts; most of the research was answerable from excerpts alone; fetches of valid URLs were fast and clean; the consent question and the key handling did what the reference says.

## Goals

1. **Reliable on the default setup.** Most users run keyless, because Exa's keyless endpoint is the default. A normal parallel batch must not silently lose results.
2. **Diagnosable.** A person can tell, from inside a session, whether its search is keyed, how many calls failed and why, and what to do about it.
3. **Honest error text.** Every failure tells the model what happened in words it can act on (a wrong URL versus a broken tool versus a limit).
4. **Efficient.** Fewer wasted tokens and calls on research tasks, measured, without regressing the web eval.

## Non-goals here

- A second search provider, hosted `web_search` on the Responses driver, or a local `webfetch`. These stay in #136 as longer-term items. A local fetch is noted in [06](06-open-questions.md) WQ-6 only because it changes the documented promise that Exa, not your machine, fetches the page.
- Changing consent, the remembered choice, or how keys are stored and redacted. These work, and [03](03-design.md) keeps their invariants (the key a search sends is always one the session's redactor holds).
- Exa's paid `agent_run` tool.
