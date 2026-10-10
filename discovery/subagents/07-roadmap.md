# 07 — Roadmap

Phases are `T1`–`T3`. T1 is committed; T2 and T3 are directional and get
decided once T1 is in daily use.

| Phase | What | Status |
|---|---|---|
| T1 | Hub commands, unattended sessions, `craze prompt --effort`, the inline plugin | planned (plan 041) |
| T2 | Review parity, and the follow-ups that T1 leaves | directional |
| T3 | Other front ends: MCP, other orchestrators, cross-provider children | directional |

## T1 — Sub-agents that work

**Commands.** `craze new --wait`, `craze send`, `craze wait`, `craze result`,
`craze cancel` and `craze stop` (TQ-1), with:
- `--prompt-file`;
- a JSON result document with distinct exit codes;
- short ids resolved as `craze attach` resolves them (G9);
- cancel on SIGTERM, so `TaskStop` on a wrapper stops the turn.

**Unattended hosts.** Spawned by the waiting command itself, with no
`session.create` change (TD-08):
- `craze prompt`'s approval policy, with native background work off (TQ-2,
  TD-09);
- a mark in the session index that records the start settings, so `craze -c`
  skips the session (TQ-7);
- reloading an ended session for `craze send` (TQ-4).

**`craze prompt`.** `--effort` and `--fast`/`--no-fast` (G1). Usage on its
`done` line (TF-8) waits for T2, since it would move the V2 parity check.

**Plugin.** Inline, per [06](06-plugin.md):
- the `delegate` agent and the presets, on Haiku at high effort;
- the `craze` skill;
- `update-version.sh` bumping `plugin.json`;
- a CI check that the plugin's files parse.

**Exit criteria:**
- From a real Claude Code session with the plugin installed, an Opus
  orchestrator delegates to `craze:sol`, `craze:muse`, `craze:grok` and
  `craze:cursor`, each in the background. Each returns a verified result.
- A `SendMessage` follow-up continues the same session with its context.
- `TaskStop` on a wrapper cancels the craze turn.
- `craze -c` in that directory still finds the person's own session.
- A turn longer than ten minutes is waited on across calls.

## T2 — Review parity and loose ends

- A sandboxed read-only mode for native (TF-5).
- An `eval/` comparison of `codex exec` and craze on the same model (TF-4);
  then decide TQ-3. If craze wins or ties, `cc-plugins`' review flows can call
  `craze new --wait --mode plan` instead of `codex exec`.
- The remaining flag gaps (TF-2, TF-3, TF-7).

## T3 — Other front ends

- `craze mcp`, if the wrapper's limits bite (TF-6).
- The same plugin installed into cursor-agent or grok, which can load Claude
  Code plugins, so they delegate through craze too.
- Cross-provider children inside native: native's `agent` tool starting a
  cursor or grok sub-agent through the hub. This needs its own discovery.
