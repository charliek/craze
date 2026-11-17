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
| CLQ-3 | **Resolved (CLD-09).** Strip `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL` and `CLAUDE_CODE_OAUTH_TOKEN` from the child (subscription only), or pass the environment through (whatever login `claude` would use)? | **Answered:** Plan only: the child env is stripped of credential, cloud and gateway overrides, and a session refuses to start unless the saved login is a claude.ai subscription and the adapter reports it as the effective login. There is no `login` key. | — |
| CLQ-4 | **Resolved (CLD-08).** Route A's Node dependency: require Node ≥22 on `PATH`, or document the adapter's standalone release? | **Answered:** The adapter (`claude-agent-acp`, Node >=22) is found on PATH with a minimum version checked at session start. `[agents].claude` overrides the path. craze never installs it and never uses `npx`. The doctor names the missing adapter and `claude` (plan 052 § A). | — |
| CLQ-5 | **Resolved (plan 052 § C1).** What does `/ask` (`ModeReadOnly`) mean on Claude, which has no read-only mode? The adapter offers `default`, `acceptEdits`, `plan`, `auto` and `bypassPermissions` (no `dontAsk`). | **Answered:** No ReadOnly mode: `claudeModeKinds` maps nothing to it, and `/ask` and `--ask` fail with "mode ask is not advertised". | — |
| CLQ-6 | **Answered by CL0.** Does `_session/steering` match craze's `Interject`? | **Answered:** yes; it replied `injected`, and the model used the text in the running turn | — |
| CLQ-7 | **Resolved (CLD-13).** Should the craze Claude Code plugin's presets offer the `claude` provider? | **Answered:** Replaced: the plugin offers claude to every caller except Claude Code, and no agent delegates to its own provider through craze. | — |
| CLQ-8 | **Resolved (plan 052 § A1).** Provider id and label: `claude` / "Claude (your plan)"? Branding rules forbid "Claude Code" as a product name. | **Answered:** Provider id and display name are both `claude`, with no "Code" in the label. The TUI detail line reads "your Claude Code, on your Claude plan". | — |
| CLQ-9 | **Answered by CL0.** Is plan usage visible through the adapter? | **Answered:** yes; `usage_update._meta["_claude/rateLimit"]` carries the same rate-limit object as stream-json (window, utilization, `isUsingOverage`). Not a reason for route B. | — |
| CLQ-10 | **Resolved (plan 052 § C2).** Start mode: the adapter starts new sessions in `auto` (a classifier approves tool calls). Should craze's default for Claude be `default` (ask), matching `--no-force`, or `auto`? | **Answered:** The start mode is `default` (Manual), or `plan` under `--plan`, sent on both new and load. It overrides the user's `permissions.defaultMode` (CLD-15). `auto` stays available by name. | — |
| CLQ-11 | **Resolved (CLD-07).** Declare `clientCapabilities.subagents` in CL1 (child sessions in the sub-agent view), or start with tool-call rows (`parentToolUseId` tags) and declare it in CL2? | **Answered:** Both are in scope of one plan. Every claude session declares `clientCapabilities.subagents`, unattended ones included, and child-session transcripts are built (plan 052 § E3). | — |
