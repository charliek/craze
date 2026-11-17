# 04: Options and design sketch

## The routes

| | Route | Wire | New craze code (guess, with tests) | Runtime deps | Verdict |
|---|---|---|---|---|---|
| **A** | Spawn `claude-agent-acp`, a fifth ACP provider | ACP v1 | 1.5–3k lines: a dialect, elicitation, plan-exit, sub-agent mapping, provider, fake-agent scripts | Node ≥22 **or** the adapter's standalone binary; the user's `claude` | **The plan (CLD-06)** |
| **B** | Spawn `claude -p` stream-json; Go control protocol | Claude Code's stream-json + `control_request` (undocumented) | 4–7k lines: an `agent.Session` of its own (shaped like `live.go`), a protocol package, a fake `claude` for tests | the user's `claude` only | The fallback if A proves limiting |
| C | Native harness on the subscription's OAuth token | Messages API | small | none | **Rejected**: prohibited ([02](02-policy-and-billing.md)) |
| D | Claude as a model under craze's own tools (opencode's MCP bridge) | SDK + MCP | large; needs a Node sidecar | Node + SDK | **Rejected**: serial tools, restarts, a ~270 MB SDK, nearer the policy line |

A Node sidecar that craze writes itself (its own TypeScript on the SDK,
speaking ACP or a private JSON) is route A done worse. It is not considered.

### Why A first (now CLD-06; B is the pivot)

- It reuses what craze already trusts: `internal/acp` (framing, the reaper,
  process groups, `session/load` replay, config options, the permission card)
  and `internal/agent/live*.go` (todos from `plan`, modes, the
  parameterized model picker, interject fallback, journal). A new provider is
  "a constructor and one line" (`internal/agent/provider.go:44-49`, `:408`).
- The adapter is the one Zed and JetBrains run. Its authors, not craze, carry
  the churn of Claude Code's wire: they release near daily, and the SDK
  version moves with them.
- t3code already runs Claude this way next to its SDK adapter
  (`packages/provider-acp-registry/src/server/claudeAcp.ts`), so the mapping
  is known.

### What A costs

- **A Node 22 dependency**, or a per-platform standalone download, for a Go
  binary that has none today. craze would not install it: like `cursor-agent`
  or `grok`, the provider is "optional", listed when its binary resolves (the
  `optional` field), with a doctor line saying how to get it.
- **Two moving parts** (the adapter and `claude`), each with its own version.
  A pin with a minimum version, checked at spawn, is the minimum.
- **ACP v1 only covers what the adapter chose to translate.** Rate-limit
  events, compaction detail and background-task wakes are lossy or behind
  `_meta`. If craze needs those exactly, B is the way.
- **Policy reading.** It is SDK-based, which is the reading the SDK
  overview's sentence aims at ([02](02-policy-and-billing.md)). CLD-01 settles
  it: this is t3code's model, which the owner accepts. CL0 found the turns
  billed to the plan ([08](08-cl0-results.md)).

### What B would buy

- No Node, and one moving part. Anthropic's own docs point non-Python/TS
  languages at "the CLI as a subprocess" (headless page).
- Exact control: `can_use_tool` straight to craze's permission card, with
  allow / allow-for-session (`updatedPermissions`) / deny + interrupt.
  - AskUserQuestion is intercepted in `can_use_tool` and answered through
    `updatedInput.answers`, as t3code does.
  - ExitPlanMode is captured as a plan card.
  - `set_model` and `set_permission_mode` are live.
  - `rate_limit_event` would show plan usage.
- It is shaped like grok in craze: one long-lived process per session, a
  stream of updates, blocking requests from the agent. pandacode is a complete
  map of the messages; t3code and opencode show the lifecycle edges.
- Against it: the protocol is undocumented, and the SDK's own docs say "treat
  a missing field as an older CLI". craze would own every Claude Code release's
  breakage. The turn-end rules in [03](03-reference-implementations.md)
  (pandacode `steering.rs`) are the part most likely to bite.

## Design sketch for route A

This is how the existing seams would take it. Nothing is decided; the open
questions are `CLQ-n` in [06](06-open-questions.md). CL0
([08](08-cl0-results.md)) checked this sketch against the real adapter. Its
"craze needs" column corrects this section where they differ, in particular:
- `elicitation.form` must be declared, or AskUserQuestion disappears;
- new sessions start in `auto`;
- a load forgets the model;
- background work starts turns of its own.

### Provider value (`internal/agent/provider.go`)

```go
func ClaudeProvider() Provider {
    return Provider{
        name:        "claude",
        displayName: "claude",                // never "Claude Code" (branding, 02)
        defaultBins: []string{"claude-agent-acp"},
        // no spawnArgs: the adapter speaks ACP on stdio by default
        authMethods: nil,                     // see Auth below
        loginHint:   "claude auth login",
        optional:    true,                    // CLD-03: listed; unavailable unless the adapter
                                              // AND the user's `claude` resolve
        resumable:   true,                    // session/load
        modeKinds:   claudeModeKinds,
        implementPrompt: cursorImplementPrompt,
        dialect:     acp.DialectClaude,
        subagentToolName: "Agent",            // and "Task" (older name)
        planUpdatesAreTodos: true,            // ACP plan → todos, as grok
        capabilities: Capabilities{Effort: true, Modes: true, Todos: true,
            AskCards: true, PlanCards: true, SubagentRows: true,
            ParameterizedPicker: true, Interject: true /* _session/steering, CLQ-6 */},
        skillScan: SkillScan{RelRoots: []string{filepath.Join(".claude", "skills")}},
    }
}
```

`forceArgs`: the adapter has no "always approve" flag. craze's `--force`
would map to `session/set_mode bypassPermissions` after `session/new`, only
where the adapter offers it (it hides bypass when settings or root forbid it).

### Spawn and environment

- `CLAUDE_CODE_EXECUTABLE` is set to the resolved user `claude`, so the
  unmodified binary the user signed in to is the one that runs, not the copy
  the SDK's platform package brings.
- `CLAUDE_AGENT_ACP_PROTOCOL=v1`, so the draft can't be negotiated by
  accident.
- `CLAUDECODE` and other `CLAUDE_CODE_*` variables from a parent Claude Code
  session are removed (t3code `ClaudeHome.ts:50-88`, the SDK's own
  `subprocess_cli.py` does the same). Without this, a craze started from inside
  Claude Code, which is how the craze plugin's sub-agents start, would inherit
  the parent's identity.
- Subscription-only mode (CLQ-3) also removes `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL` and `CLAUDE_CODE_OAUTH_TOKEN`,
  as pandacode's `claude_auth.rs:11-61` does, so a stray key cannot turn a
  "plan" session into a billed one.

### Auth

- craze runs `claude auth status --json` before spawning (and in
  `craze providers` / the doctor). It reads `loggedIn`, `authMethod`
  (`claude.ai`, `oauth_token`, `api_key`, …), `apiProvider` and
  `subscriptionType`. Nothing else is read, and no file under `~/.claude` is
  opened. This is the check pandacode uses; t3code reads the same facts from
  the SDK's init.
- The adapter advertises no auth methods to a client without terminal auth
  (`authMethods: []`). It reports the account in `auth/status_update`
  notifications (`state: authenticated` with a plan label). craze can show
  that label and needs no `authenticate` call (CL0).
- Not logged in: start fails before the spawn, with the `claude auth login`
  hint, worded as other providers' start errors are (`decorateStartErr`,
  `internal/agent/starterr.go:44`). craze never
  runs the login itself. The adapter's terminal auth methods are never invoked,
  and craze declares no terminal-auth capability.
- The status row shows the login kind ("plan: max" or "api key") so the user
  sees which bill a session lands on.

### Modes (`modeKinds`)

The adapter's modes are Claude Code's permission modes:

| Claude Code id | craze kind | Notes |
|---|---|---|
| `default` (named "Manual") | `ModeImplement` | asks per tool; craze's start mode |
| `acceptEdits` | `ModeImplement` | |
| `auto` | `ModeImplement` | classifier approves |
| `plan` | `ModePlan` | ExitPlanMode → plan card |
| `dontAsk` | n/a | Claude Code has it, but the adapter does not offer it (CL0: the mode option lists `default`, `acceptEdits`, `plan`, `auto`, `bypassPermissions`). Nothing maps to `ModeReadOnly` (CLQ-5). |
| `bypassPermissions` | `ModeImplement` | only via `--force` |

### The dialect (`acp.DialectClaude`)

What `internal/acp` would need beyond cursor and grok. Today about three dozen
provider or dialect branches sit across `client.go`, `outcome.go`,
`interject.go`, `aside.go` and `provider.go`:

1. **Elicitation.** The adapter asks AskUserQuestion through ACP
   `elicitation/create` (a form schema: one `oneOf` per question plus a
   free-text `_custom` field), and only when the client declares
   `clientCapabilities.elicitation.form`. Without that, Claude never sees the
   tool (CL0). craze has no elicitation today. Declare it, and map the form to
   the question card, as grok's `_x.ai/ask_user_question` is mapped
   (`askOutcome`, `internal/acp/outcome.go:128`). Unattended hosts answer with
   the first option, as today (TD-11).
2. **Plan exit.** The adapter asks ExitPlanMode as an ordinary
   `session/request_permission`. It is titled "Ready to code?" and has
   plan-specific options, including clear-context variants
   (`dist/permissions/presentation.js:49`, `dist/permissions/effects.js:100-149`).
   Recognize it by tool name and raise the plan card instead of a permission
   card (`planOutcome`, `outcome.go:211`). Map the card's answer back to the
   matching option id. Unattended hosts return the plan, as today.
3. **Sub-agents.**
   - First cut: undeclared capability. Agent/Task tool calls become sub-agent
     rows through `subagentToolName`, as cursor's `task` does, with no child
     transcript.
   - Later: declare `clientCapabilities.subagents` and fold `subagent_update`
     plus the child's `session/update` stream into the existing child routing
     (the allowlist built for grok's children, architecture §Session).
     t3code's `claudeAcp.ts` is the field map.
4. **`usage_update`.** craze names it but reads it nowhere
   (`internal/acp/heard.go:142`). Read it into the status row's usage part,
   unpriced on a plan (`subagents/03-design.md:133`: "a subscription … is shown
   as unpriced, not as $0").
5. **`notice`, `compaction_update`.** Show them as transcript notes. A notice
   carrying the "extra usage" billing text becomes a typed error
   ([02](02-policy-and-billing.md)).
6. **Cancel.** A `session/cancel` leaves background commands running
   (adapter README). craze's `CancelOutcome` contract (`session.go:1200-1218`)
   holds, but the doc for this provider must say so.
7. **Interject.** v1 has an extension method, `_session/steering`,
   advertised by `InitializeResponse._meta.steering.supported`
   (`dist/v1/acp-agent.js:193-198`). It folds text into the running turn, which
   is craze's `Interject` contract, so this would be grok's `_x.ai/interject`
   under another name (`internal/acp/interject.go`). Confirming the semantics
   live is CLQ-6.
8. **`/btw`.** The adapter has no aside. It stays off (`aside: false`), as for
   cursor.
9. **Foreign turns.** A background Agent or Bash that finishes after the
   prompt returned makes Claude Code run a turn of its own. Its updates arrive
   with no prompt in flight, and it closes with a `usage_update` whose
   `_meta["_claude/origin"].kind` is `task-notification` (CL0). Surface it
   through `ForeignTurn()`, as native's wake does. The engine already refuses
   to start a prompt into one.
10. **Mode and model on start and load.** Set the mode right after
    `session/new` (the adapter defaults to `auto`), and re-apply the model
    after `session/load` (the adapter restores the mode but not the model).

### Models and effort

These come over ACP config options, which craze's `ParameterizedPicker`
already reads (the client declares `_meta.parameterizedModelPicker`). The
session-list `/model` catalog for an ACP provider is the model cache that
`craze serve` writes (`internal/modelcache`), so nothing new is needed.
Claude's `[1m]` context-window variants and per-model effort lists arrive as
values.

### Instructions, skills, plugins

Claude Code reads `CLAUDE.md`, `.claude/skills`, its plugins and its
`settings.json` itself; that is the point of running its harness. craze adds
nothing to the prompt. Slash commands come over
`available_commands_update`. Unlike cursor, `pluginScan` stays zero.

### Unattended (`craze new --wait`) and the plugin

The craze Claude Code plugin runs a Claude Code orchestrator that delegates
*to* craze. A `claude` provider would let it delegate back to Claude through
craze, which is pointless and doubles the plan draw. The plugin's presets
should not offer it (CLQ-7).

CLD-05 settles the wider case. The user's Claude Code setup is inherited as
it is, including the craze plugin's agents, which CL0 saw offered in the
adapter's `agent` option. craze assumes a Claude session does not call back
into craze and adds no guard. That is revisited only if it becomes a real
case.

### Overage (CLD-02)

The first `usage_update` of each turn carries Claude Code's rate-limit object
in `_meta["_claude/rateLimit"]`. When it says `isUsingOverage: true`, or the
turn fails with a billing error (an `assistant` notice with model
`<synthetic>`, or a prompt error whose text names extra usage), the dialect:
1. sends `session/cancel`;
2. ends the turn with a typed error (a new `errkind`), saying the turn would
   have drawn extra usage.

The status row can also show the seven-day utilization from the same object.

## What route B would add on top

If A falls short, B replaces the dialect with a package of its own
(`internal/claudecode`, say) and an `agent.Session` implementation, and keeps
everything above the seam. Its protocol inventory is pandacode's
([03](03-reference-implementations.md)) plus the outbound subtypes the Python
SDK uses: `set_permission_mode`, `set_model`, `get_context_usage`, `stop_task`.
Its test double is a scripted fake `claude` in `cmd/`, alongside
`craze-fake-agent`. The live smoke would follow the `craze-live-smoke` skill
against the real binary.
