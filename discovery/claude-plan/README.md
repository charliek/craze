# discovery/claude-plan/: Claude on a Pro/Max subscription as a craze provider

This folder records what it would take to add **Claude, signed in with the
user's Claude Pro/Max (or Ultra/Team) plan**, as a craze provider. It sits
beside cursor, grok, gx and native. It is discovery only. The owner has not
yet decided to build it, and nothing here is a plan. Panel-reviewed plans live
outside the repo (`CLAUDE.md`), and nothing here is published by `make docs`.

Claude on an **API key** is not the subject. Native already has that through
craze's own Messages API client (`internal/harness/llm/messagesapi`,
`internal/anthropicmodels`, plan 044). The question is only the subscription
login, which is a policy question first and an engineering one second.

It is the sibling of `discovery/native-harness/`, `discovery/session-control/`
and `discovery/subagents/` and follows their conventions. Its ID namespaces are:
phases `CL0`–`CL3`, decisions `CLD-nn` ([09](09-decisions.md)), open questions `CLQ-n`.
Plain `C0` is already taken by session-control's progress log.

## Status

| date | event |
|---|---|
| 2026-11-11 | Discovery session. Read t3code (`a21b43fc70`), pandacode (`106b385`) and opencode (`6c34e24488`); inspected `@agentclientprotocol/claude-agent-acp` 0.106.0 and the installed `claude` 2.1.322; read Anthropic's current legal, authentication, headless and Agent SDK pages. This folder written. |
| 2026-11-11 | Owner's call CLD-01: t3code's model (the user's own signed-in `claude`, driven through the Agent SDK ecosystem, no craze login) is the accepted model. CL0 run the same day ([08](08-cl0-results.md)): both routes bill to plan limits on the owner's account; route A fits with a known list of dialect items; unmodified craze 0.2.0 already completes a turn through the adapter. |
| 2026-11-11 | Owner's review of the approach ([10](10-review.md)): CLD-02 overage is an error; CLD-03 a listed provider, unavailable when its binaries are missing; CLD-04 background work gets its own design section; CLD-05 inherit Claude Code's setup as is; CLD-06 route A is the plan, B the pivot. The owner reads opencode's October trouble as its Console breach. The folder moved to branch `discovery/claude-plan` (its own worktree, while plan 050 was built in the main checkout), and the CL0 probe scripts were added under [probes/](probes/README.md). **Parked**: the owner will decide whether to build as their use cases develop. |

**Current phase: CL0 done; parked.** CL1, a listed provider on route A, is the
build when the owner picks this up. See [05](05-roadmap.md).

## Picking this up later

1. **Re-check the ground.** Things have moved fast in 2026, so the folder may
   be stale.
   - Re-read [02](02-policy-and-billing.md)'s sources. The policy moved four
     times in 2026.
   - Re-run probe 1 with [probes/](probes/README.md) (a few cents of plan
     quota) to confirm turns still bill to the plan.
   - Check the adapter's current version and protocol (`npm view
     @agentclientprotocol/claude-agent-acp version`, and its README's v1/v2
     notes). Re-run probe 2 against it.
2. **Rebase the branch** onto `main` (this folder is the only change on it),
   and re-check the craze file references in [04](04-options-and-design.md).
   They were written against `26181d1`.
3. **Write the CL1 plan** from [05](05-roadmap.md) CL1, [08](08-cl0-results.md)'s
   "craze needs" column and CLD-01 to CLD-06. Give background work (CLD-04)
   its own section. Answer CLQ-3, -4, -5, -8, -10 and -11 there
   ([06](06-open-questions.md)).

## The answer in brief

- **What is allowed.** craze may never run its own claude.ai OAuth or touch the
  user's token. That rules out the native harness on a subscription. What the
  docs allow is running the user's own unmodified `claude` binary, which the
  user signed in to themselves. That is how all three reference apps do it
  today. The owner takes t3code's version of this as the accepted model
  (CLD-01, [09](09-decisions.md)), and the plan follows it
  ([02](02-policy-and-billing.md)).
- **Billing.** A recording in opencode from 2026-10-17 shows the API refusing
  an Agent SDK call on a subscription with *"Third-party apps now draw from
  your extra usage, not your plan limits."* CL0 measured it on the owner's
  account on 2026-11-11, and both routes drew from plan limits
  (`isUsingOverage: false`, with overage disabled, so a misclassified turn
  would have failed) ([08](08-cl0-results.md)). The product still types a
  billing error and shows overage.
- **Engineering.** There are two viable routes, both of which spawn the user's
  `claude`:
  - **A, recommended first:** drive the ACP project's Claude adapter
    `claude-agent-acp` (Apache-2.0, formerly Zed's `claude-code-acp`) as a
    fifth ACP provider. It reuses `internal/acp` and
    `live.go`; the new work is a dialect plus elicitation, plan-exit and
    sub-agent mapping. Rough size: 1.5–3k lines of Go with tests. Cost: Node 22
    or the adapter's standalone binary, and a fast-moving dependency.
  - **B:** a Go driver for `claude -p --input-format stream-json` and its
    control protocol, an `agent.Session` of its own. It has no Node and full
    control, but rests on an undocumented wire. Rough size: 4–7k lines with
    tests (t3code's TypeScript equivalent is 13k plus a 22k-line test).

  See [03](03-reference-implementations.md) and [04](04-options-and-design.md).

## Reading order

1. [01: Goals and scope](01-goals-and-scope.md): what "a Claude provider" means here, and the non-goals.
2. [02: Policy and billing](02-policy-and-billing.md): Anthropic's current text, the 2026 timeline, the billing evidence. This is the gate.
3. [03: Reference implementations](03-reference-implementations.md): t3code, pandacode, opencode and the ACP adapter, side by side.
4. [04: Options and design sketch](04-options-and-design.md): routes A–D, and how the chosen route maps onto craze's seams.
5. [05: Roadmap](05-roadmap.md): CL0 probes, then CL1–CL3 with exit criteria.
6. [06: Open questions](06-open-questions.md): `CLQ-n`, each with a default.
7. [07: References](07-references.md): files to read in each clone, and the URLs.
8. [08: CL0 results](08-cl0-results.md): the billing and adapter-fit probes, feature by feature.
9. [09: Decisions](09-decisions.md): append-only, `CLD-nn`.
10. [10: Review](10-review.md): the risks raised after CL0, each with the owner's mark.
11. [probes/](probes/README.md): the CL0 scripts, to re-run the billing and fit probes.

## How to use this folder in a future session

- Re-check [02](02-policy-and-billing.md) first. It is dated, the policy has
  moved several times in 2026, and a change there can end the work or unblock
  it.
- Planning CL1: read this README, `04`, `05`, `08` and `09`. `08`'s "craze
  needs" column is CL1's work list. Write a plan per phase from [05](05-roadmap.md),
  and record each owner call in `09` (append-only, `CLD-nn`).
- When a phase closes, update the status table above and add its exit result
  under the phase in `05`, in the same PR.
