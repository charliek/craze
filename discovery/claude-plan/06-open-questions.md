# 06: Open questions

Each question has a default, which is what happens if nobody decides. When the
owner answers one, rewrite it in place as `**Resolved (CLD-nn).**` and add the
decision to [09](09-decisions.md). A question answered by evidence rather than
a decision reads `**Answered by CL0.**`, with a pointer to
[08](08-cl0-results.md).

| id | question | default | when |
|---|---|---|---|
| CLQ-1 | **Resolved (CLD-01).** Ask Anthropic for written confirmation before shipping? | **Answered:** no; t3code's model is the accepted one | — |
| CLQ-2 | **Answered by CL0.** Build it at all, if the billing probe showed "extra usage"? | **Answered:** the probe showed plan-limit billing for both routes on the owner's account, so the gate is open. Whether to build is still the owner's call. | — |
| CLQ-3 | Strip `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL` and `CLAUDE_CODE_OAUTH_TOKEN` from the child (subscription only), or pass the environment through (whatever login `claude` would use)? | Pass through, and show the login kind at start (the `auth/status_update` plan label). A `[providers.claude] login = "subscription"` key strips. | CL1 |
| CLQ-4 | Route A's Node dependency: require Node ≥22 on `PATH`, or document the adapter's standalone release? | Either, found through `defaultBins`; the doctor names both. CL0 ran on Node 24 with a 79 MB install. | CL1 |
| CLQ-5 | What does `/ask` (`ModeReadOnly`) mean on Claude, which has no read-only mode? The adapter offers `default`, `acceptEdits`, `plan`, `auto` and `bypassPermissions` (no `dontAsk`). | Leave `ModeReadOnly` unmapped; `/ask` is unavailable on Claude, as a mode a provider lacks is today | CL1 |
| CLQ-6 | **Answered by CL0.** Does `_session/steering` match craze's `Interject`? | **Answered:** yes; it replied `injected`, and the model used the text in the running turn | — |
| CLQ-7 | Should the craze Claude Code plugin's presets offer the `claude` provider? | No: Claude Code delegating to Claude through craze doubles the plan draw | CL2 |
| CLQ-8 | Provider id and label: `claude` / "Claude (your plan)"? Branding rules forbid "Claude Code" as a product name. | id `claude`; label without "Code" | CL1 |
| CLQ-9 | **Answered by CL0.** Is plan usage visible through the adapter? | **Answered:** yes; `usage_update._meta["_claude/rateLimit"]` carries the same rate-limit object as stream-json (window, utilization, `isUsingOverage`). Not a reason for route B. | — |
| CLQ-10 | Start mode: the adapter starts new sessions in `auto` (a classifier approves tool calls). Should craze's default for Claude be `default` (ask), matching `--no-force`, or `auto`? | `default`; `auto` offered as a mode | CL1 |
| CLQ-11 | Declare `clientCapabilities.subagents` in CL1 (child sessions in the sub-agent view), or start with tool-call rows (`parentToolUseId` tags) and declare it in CL2? | Tool-call rows in CL1, child sessions in CL2 | CL1 |
