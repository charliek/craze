# 05: Roadmap

None of this is scheduled. Each phase stands alone, and the owner can stop after
any of them. Sizes are guesses from reading the reference code, not
commitments.

| phase | what | spends | exit criterion |
|---|---|---|---|
| **CL0** (done 2026-11-11) | Two probes, no repo code | a few cents of plan quota, with the owner's go-ahead | the billing question and the adapter fit are answered with evidence |
| CL1 | Route A as a listed, optional provider (CLD-03, CLD-06) | engineering + live smoke | a turn, a permission card, a plan card, a question card, cancel, resume, background wakes and the overage error work against the real adapter on Linux; docs |
| CL2 | Full integration | engineering + live smoke | grok-level integration (child sessions, interject) on Linux and the mac-mini |
| CL3 | Route B (only if CL1/CL2 show A falls short) | engineering | B passes the same live gate as A, and A is retired |

## CL0: probes

Run these by hand from scratch directories, never in the working tree,
following the `craze-live-smoke` skill's hygiene (scratch `HOME` is **not**
possible here: the probe must use the owner's real `claude` login).

1. **Billing probe.** Two small turns on the owner's plan, with `/usage` read
   in the `claude` TUI before and after each:
   - one through `claude -p --input-format stream-json --output-format stream-json --verbose`,
     sending one user message, with Claude Code's own prompt and tools;
   - one through `claude-agent-acp` (Node 22 via mise, or the standalone
     release) driven by a minimal ACP script, with `CLAUDE_CODE_EXECUTABLE`
     pointing at the owner's `claude`.

   Record for each: the `rate_limit_event` frames; whether the turn drew plan
   usage or "extra usage"; any "Third-party apps…" error; and
   `CLAUDE_CODE_ENTRYPOINT` as set. **Exit:** a yes or no for "a subscription
   turn driven this way draws from plan limits", for each route. A "no" for
   both ends the work and gets recorded as `CLD-01`.
2. **Adapter fit.** Point craze's ACP client at the adapter. A throwaway
   branch with a hidden `claude` provider is enough; `RegisterHiddenProviderForTest`
   shows the pattern. List every `session/update` kind and request the adapter
   sends in one session with a tool call, a permission, AskUserQuestion,
   ExitPlanMode, a sub-agent, cancel, and `session/load` of the same session.
   **Exit:** a table of what craze drops, what it mis-renders, and what it
   handles. That table sizes CL1.

**Exit result (2026-11-11).** Both probes ran ([08](08-cl0-results.md)).
- **Billing:** every turn through both routes drew from plan limits
  (`isUsingOverage: false`, overage disabled org-wide). So the "no" that would
  have ended the work did not happen.
- **Adapter fit:** unmodified craze 0.2.0 completed a turn through the adapter
  (model set, tool rows with diffs, text, `end_turn`). The gaps are dialect
  items:
  - declare `elicitation.form` for AskUserQuestion;
  - raise the plan card from the "Approve Plan" permission;
  - set the mode after `session/new` (the default is `auto`);
  - re-apply the model after `session/load`;
  - treat background-task wakes as foreign turns;
  - read `usage_update` (rate limits, cost);
  - map `_session/steering` to `Interject`;
  - optionally declare `subagents` for child sessions.

  These are CL1's list.
- Probe 2 used a standalone driver as well as craze, so no throwaway branch
  was needed.

## CL1: a listed, optional provider (route A)

Built on CLD-01 to CLD-06 ([09](09-decisions.md)). The plan for this phase
should have its own section on background work (CLD-04).

- **Provider.** `ClaudeProvider()`, listed and `optional` (CLD-03):
  - offered when both `claude-agent-acp` and the user's `claude` resolve, and
    shown as unavailable otherwise;
  - `acp.DialectClaude`;
  - the spawn environment and the `claude auth status --json` pre-check
    ([04](04-options-and-design.md));
  - a minimum-version check for both binaries.
- **Dialect items** (from [08](08-cl0-results.md)'s "craze needs" column):
  - declare `elicitation.form`, and map AskUserQuestion to the question card,
    with its free-text "Other" answer;
  - map the "Approve Plan" permission to the plan card;
  - set the mode after `session/new` (CLQ-10);
  - re-apply the model after `session/load`;
  - read `usage_update` into the usage part, unpriced on a plan;
  - make Agent/Task sub-agent rows at tool-call level;
  - route `_session/steering` to `Interject`.
- **Overage (CLD-02).** `isUsingOverage: true` or a billing error cancels the
  turn and ends it with a typed error.
- **Background work (CLD-04).**
  - Wake turns are foreign turns.
  - The idle watcher, the unattended limit and `--exit-after-turn` count open
    background tasks and running children.
  - Cancel's docs say that background commands survive it.
- **Claude Code's setup (CLD-05)** is inherited as is, with nothing stripped.
- **Tests.** `cmd/craze-fake-agent` gets `claude-*` scripts replaying CL0's
  captured frames, as the `grok-*` scripts do. Wire fixtures go under
  `internal/acp/testdata`.
- **Docs**, in the same commit as the code (`CLAUDE.md`):
  - `docs/reference/cli.md`, `configuration.md` and `tui.md`;
  - `docs/getting-started/` and the README highlight;
  - a page that states the login and billing position plainly: craze runs the
    user's own `claude` and never touches the login, and overage is an error.
- **Exit:** these work on Linux against the real adapter and the owner's
  login, through the TUI on a detached host:
  - a new session, a turn and a permission card;
  - a question card, a plan card and approve → implement;
  - cancel, then resume with `-c`;
  - `/model` and effort;
  - a background wake;
  - the not-signed-in error;
  - "unavailable" when either binary is missing.

## CL2: full integration

- Declare the `subagents` capability, so child sessions show in the sub-agent
  view (CLQ-11).
- Show notices and compaction as notes.
- Run the mac-mini smoke, covering the probe gaps in [10](10-review.md) item 6:
  images, compaction, concurrent sessions, and the model plan mode actually
  runs.
- **Exit:** the CL1 list plus sub-agent transcripts, interject, and unattended
  `craze new --wait` (first-option answers, the plan returned), on both
  platforms.

## CL3: route B (conditional)

The pivot named by CLD-06: only if A's gaps matter, or the adapter's Node
dependency or churn proves a burden.
- `internal/claudecode`: a stream-json and control-protocol client.
- An `agent.Session` implementation.
- A scripted fake `claude`.
- pandacode's protocol inventory and turn-end rules, with t3code's lifecycle
  edges ([03](03-reference-implementations.md)).

**Exit:** passes CL2's gate without Node.
