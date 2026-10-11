# 07 — Roadmap

Phases are `T1`–`T3`. T1 is done; T2 and T3 are directional and get
decided once T1 is in daily use.

| Phase | What | Status |
|---|---|---|
| T1 | Waiting commands, unattended sessions, `craze prompt --effort`, the inline plugin | done (plan 041, 2026-10-11) |
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

**Exit result (2026-10-11, plan 041).** Live, from headless Claude Code (Opus
5.5) with the plugin loaded from the branch, on real providers:

- `craze:sol`, `craze:muse`, `craze:grok` and `craze:cursor` each fixed the
  discovery fixture and Opus verified the diff and tests (L2). The wrappers ran
  on Haiku 5.5 at about $0.001–0.002 a call. The delegations ran in the
  foreground; background runs were exercised with `craze:muse` (L6).
- `SendMessage` continued the same session, and after `craze stop` it reloaded
  the session with its context, on native and grok (L4). The wrapper first
  answered a recap from its own transcript; its wording was tightened so every
  message is forwarded.
- `TaskStop` cancels the craze turn (`cancelled`, exit 3) and the session
  stays (L6), after direct-spawned hosts were moved out of the caller's process
  tree (TD-15).
- `craze -c` loads the person's session past a newer sub-agent one; `-r` and
  the session list tag sub-agent sessions (L8).
- A 194 s turn completed through one Bash call (L9); `--timeout` then
  `craze wait --from <handle>` continues a turn across calls (L7). A turn
  longer than ten minutes was not run live; the mechanism is the same.
- Default permission mode works with the skill's allow-list once the file rule
  is `Edit(//tmp/craze-delegate-*)` (L3, TD-14). `isolation: "worktree"` puts
  the session in the worktree (L10). `mode=plan` returns a plan without
  changes on cursor and grok (L7, TD-11).

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
