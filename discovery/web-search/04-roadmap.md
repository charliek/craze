# 04 — Roadmap (proposed, not reviewed)

Three phases, ordered by how much each improves the default (keyless) experience per unit of work. Each ends with a live check against the real endpoint, because the eval's proxy cannot show rate limits or Exa's error texts.

## W1 — Reliable by default

Takes: #138 (retry, then cap; [03](03-design.md) §A), #137 part B1 (the hint), #139 (error text; §D).

Exit criteria:
- Keyless, a native session's batch of 8 parallel searches completes 8 of 8 (E3 lost 5).
- Keyed, a batch of 24 completes 24 of 24 within the per-call limit, or the cap keeps it under 10 per second (E5 lost 12).
- A 404 fetch reads as "not found", with the URL.
- The unkeyed 429 hint names `craze auth login websearch/exa` and the next-session rule.
- Fixtures cover both 429 shapes, `Retry-After` past the deadline, cancel mid-wait and the cap; goldens moved deliberately (`v8.sh`).
- Live: E3, E5 and E7 re-run through `craze prompt --json --web-search` on Linux and the mac-mini.

## W2 — Diagnosable

Takes: #141 (visibility; §C), #137 part B2 (mid-session key pickup; §B).

Exit criteria:
- The TUI shows the session's key source and the call and failure counts.
- `craze auth login websearch/exa` during a running keyless session makes the next turn's searches keyed, which E1 showed it does not today, and the redaction invariant holds (a test asserts the redactor holds the key before the first keyed call).

## W3 — Better results per token

Takes: #140 (domains and result count; §E), #131 (excerpt trust).

Exit criteria:
- The web eval (T-WS1–T-WS6, both models) shows no regression in pass rate.
- With `domains` set, median tokens per research search fall by a measured amount.
- #131's T-WS6 re-run decides whether the "results are excerpts" line ships.

## Later

#136 (a second provider, hosted search on the Responses driver, a local `webfetch`, moving `KeyPattern`). Unscheduled; WQ-6 notes what a local fetch would change.
