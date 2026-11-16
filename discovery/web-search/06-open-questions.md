# 06 — Open questions

Each has the default this folder uses until it is answered. A resolved question stays here, rewritten in place as `**Resolved (WD-nn).**`.

**WQ-1. **Resolved (WD-01).** How many retries, and how long?** Default: at most 3 attempts, with `Retry-After` (else 1 s) × (1 + jitter) × attempt, never past the call's 25 s deadline. E4 needed at most 2 retries in a batch of 8.

**WQ-2. **Resolved (WD-02).** What cap on concurrent web calls per session?** Default: 4, shared with children. Keyless allowed 3 parallel calls in E3 before limiting, but with retry in place a cap of 3 would mostly add queueing. Keyed, the limit is 10 per second per team (E5). Should the cap be configurable (a `[websearch]` key)? Default: no, until someone needs it; a new `config.toml` key also needs `configuration.md` and its drift test.

**WQ-3. **Resolved (WD-06).** Expose `objective` to the model?** Default: no. E8 saw no effect in one trial. Re-test with several queries before closing.

**WQ-4. **Resolved (WD-04).** Where does the web-search status line go in the TUI?** Default: beside the model and effort, wherever the session info shows them. The status row is crowded, so it may belong in a session-info view instead.

**WQ-5. **Resolved (WD-06).** Fewer results by default?** E8 halved bytes at `numResults: 4`, but #131 shows models already lean on excerpts, and fewer results means less to choose from. Default: keep 8, add `num_results` as an option (§E), and let the eval decide whether to change the default.

**WQ-6. A local `webfetch` fallback?** It would serve pages Exa cannot crawl, but it changes the documented promise that pages are fetched by Exa, not your machine (`configuration.md` § Native web search). It also needs SSRF guards (#136). Default: not in W1–W3.

**WQ-7. Nudge keyless users at consent time?** The consent card could mention that a free Exa key raises the limit. Default: no change to the card; the 429 hint (§B1) and the status line (§C) carry it, so a user who never hits the limit is never nagged.
