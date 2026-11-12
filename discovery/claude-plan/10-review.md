# 10: Review of the approach, and the owner's marks (2026-11-11)

After CL0, the session that wrote this folder was asked whether the approach
holds up and what to question. Its answer was that it would work well for
personal use. The risks are about durability, and about where two full agent
harnesses meet, more than about technical fit. Each item below carries the
owner's mark.

| # | concern | owner's mark |
|---|---|---|
| 1 | **The billing evidence is thin.** It is one account on one day, mostly Haiku, with overage disabled. Anthropic changed this area four times in 2026, and there are unverified reports that SDK use burns plan quota faster than the TUI. | **Decided: CLD-02** (overage is an error). The owner reads opencode's October trouble as its 2026-10-16/17 Console security breach rather than a policy change ([02](02-policy-and-billing.md)). Re-run probe 1 ([probes/](probes/README.md)) on big `claude` or adapter releases. |
| 2 | **Personal use and a listed provider are different risks.** opencode shipped the same pattern and keeps its docs page unlisted. | **Decided: CLD-03** (listed; unavailable when the binaries are missing). CLD-01 covers the policy side. |
| 3 | **Claude Code's background work collides with craze's host lifecycle.** Unprompted wake turns, background commands that survive cancel, the idle watcher (no admission fence on ACP agents), `--exit-after-turn`, the unattended limit, the queue and send-now. The most likely source of real bugs. | **Decided: CLD-04** (its own design section in the build plan, handled as native's wake). |
| 4 | **The user's whole Claude Code setup comes along.** Hooks, plugins, MCP, settings and permission rules apply, and Claude can call craze's own agents (recursion, a doubled plan draw). | **Decided: CLD-05** (inherit as is; assume no call back into craze; revisit later). |
| 5 | **Version skew** between the adapter (near-daily releases, pinned to an Agent SDK built against one `claude`), the user's self-updating `claude`, and Node ≥22. | **Decided: CLD-06** (route A, with B as the pivot). The plan still needs a minimum-version check at spawn and a way to pin the adapter. |
| 6 | **Probe gaps:**<br>• the detached host and TUI (only `craze prompt` in process ran);<br>• macOS, images, compaction;<br>• concurrent sessions;<br>• `craze -c` end to end;<br>• plan mode silently running Sonnet while the picker said Haiku. | Open: the build's live smoke covers them ([05](05-roadmap.md) CL1/CL2 exits). |
| 7 | **Soft estimates.** "1.5–3k lines" will grow under craze's bar: docs drift tests, wire fixtures, fake-agent scripts, two-platform smokes. The free-text "Other" answer on the question card is a visible change. Many line references in [03](03-reference-implementations.md) came from research agents and were spot-checked, not all verified. | Noted. Re-size from [08](08-cl0-results.md)'s "craze needs" column when planning. |
| 8 | **The value question.** Claude Code already has background sessions, `claude attach` and remote control. craze would add one chrome across providers, the session list and delegation. It would lose Claude Code's own UI: rewind, checkpoints, status line, hook prompts. | Open. The owner is parking the work until their use cases call for it. |
