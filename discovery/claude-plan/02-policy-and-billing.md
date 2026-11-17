# 02: Policy and billing (as read on 2026-11-11)

This is the gate. Everything else in the folder is moot if the answer here is
no. The text below was fetched on 2026-11-11. It has moved several times in
2026, so re-read the sources before acting on it. This is not legal advice.

## What Anthropic's pages say now

**Claude Code: Legal and compliance**
([code.claude.com/docs/en/legal-and-compliance](https://code.claude.com/docs/en/legal-and-compliance)):

> OAuth authentication is intended exclusively for purchasers of Claude Free,
> Pro, Max, Ultra, Team, and Enterprise subscription plans and is designed to
> support ordinary use of Claude Code and other native Anthropic applications.

> Developers building products or services that interact with Claude's
> capabilities, including those using the Agent SDK, should use API key
> authentication … Anthropic does not permit third-party developers to offer
> Claude.ai login into their own applications, or to route requests through
> Free, Pro, Max, or Ultra plan credentials on behalf of their users. Moreover,
> developers may not collect, store, or intermediate Claude.ai credentials or
> session tokens.

> [Nor does it prevent] an end user from signing in to the unmodified Claude
> Code binary with their own Claude subscription, including where a platform
> hosts Claude Code …

> Advertised usage limits for Pro, Max, and Ultra plans assume ordinary,
> individual usage of Claude Code and the Agent SDK.

**Agent SDK overview**
([code.claude.com/docs/en/agent-sdk/overview](https://code.claude.com/docs/en/agent-sdk/overview)):

> Unless previously approved, Anthropic does not allow third party developers
> to offer claude.ai login or rate limits for their products, including agents
> built on the Claude Agent SDK.

**Authentication: `claude setup-token`**
([code.claude.com/docs/en/authentication](https://code.claude.com/docs/en/authentication#generate-a-long-lived-token)):
a one-year OAuth token "for CI pipelines, scripts, or other environments where
interactive browser login isn't available". It "authenticates with your Claude
subscription … It can only make model requests." It is the user's own token in
the user's own environment, and craze must not ask for it or store it.

**Consumer Terms** (effective 2025-10-08) bar sharing account credentials and
"automated or non-human means … except … where Anthropic explicitly permits it".

**Branding.** The SDK's branding rules allow "Claude Agent" for a third-party
product built on it and do not allow "Claude Code" as a product name (opencode's
spec cites this). If craze builds it, the provider's display name follows that:
`claude` as the id, a label such as "Claude (your plan)", never "Claude Code".

## How the craze patterns sort against that text

| Pattern | Reading |
|---|---|
| craze runs claude.ai OAuth, or reads `.credentials.json` or the keychain, and calls the API with the token (old opencode, native-on-OAuth) | **Prohibited**: "collect, store, or intermediate". |
| craze reads the stored token only for a usage meter (`/api/oauth/usage`), as t3code and pandacode do | **Prohibited** by the same sentence. Not done. |
| craze spawns the user's own installed, unmodified, signed-in `claude` (pandacode; route B) | **Unsettled, leans allowed.** The carve-out is "an end user … signing in to the unmodified Claude Code binary with their own Claude subscription". craze never sees the login. But craze is a "third-party developer" whose product drives it, and the carve-out's example is hosting, not driving. |
| craze spawns an Agent-SDK-based adapter that spawns `claude` (t3code, opencode Claude Agent, `claude-agent-acp`; route A) | **Unsettled, leans riskier.** The SDK overview names "agents built on the Claude Agent SDK" in the no-claude.ai-login sentence. But every Agent SDK app runs the same `claude` binary, Anthropic's own support page says SDK use on a plan is fine, and the adapter is the one Zed and JetBrains ship. |

**The owner's position (CLD-01, 2026-11-11).** t3code's approach fits
Anthropic's model and permissions. At the least it is well known and accepted,
so it is acceptable under Anthropic's terms and is the approach a plan uses.
t3code uses the Agent SDK `query()` with the user's own signed-in `claude`,
and runs no login of its own. Both routes in [04](04-options-and-design.md)
sit inside that model. The analysis below stays as the background to that
call.

**What decides it for craze specifically.** craze is open source, but it is
used by its one maintainer on their own machines with their own subscription.
It does not offer a login, hold anyone's credential, or resell anything. That
is the strongest version of "ordinary, individual use". The residual risk is
that Anthropic classifies the traffic as "third-party" and bills or blocks it
(next section), not that craze breaks a rule a reading of the text makes plain.
CLQ-1 asked whether to seek written confirmation first; CLD-01 answers no.

## Timeline

| date | event | source |
|---|---|---|
| 2025-06-05 | opencode ships built-in claude.ai OAuth (Claude Code's client id, `oauth-2025-04-20` + `claude-code-20250219` betas, a "You are Claude Code" system-prompt prefix) | opencode `35b03e4cb3` |
| 2025-10-08 | Consumer Terms version in force | anthropic.com/legal/consumer-terms |
| 2026-01-09 | Server-side rejection of subscription tokens used outside Claude Code reported (secondary sources only, unverified) | press |
| 2026-01-20/25 | opencode docs: "some users having their subscriptions blocked"; then "not officially supported by Anthropic" | opencode `7f50b27996`, `23d85f4739` |
| 2026-02-19/20 | Legal page adds that using Free/Pro/Max OAuth tokens "in any other product, tool, or service … including the Agent SDK" is not permitted | [The Register, 2026-02-20](https://www.theregister.com/2026/02/20/anthropic_clarifies_ban_third_party_claude_access/) |
| 2026-03-19 | opencode removes the OAuth plugin: "anthropic legal requests (#18186)"; docs: "Anthropic explicitly prohibits this" | opencode `1ac1a0287c`, v1.3.0 |
| 2026-05-13/14 | Anthropic announces SDK, `claude -p` and ACP use would move to a monthly "Agent SDK credit" from 2026-06-15 | [Zed blog](https://zed.dev/blog/anthropic-subscription-changes) |
| 2026-06-15 | Paused: "We've paused the previously-announced changes to Claude Agent SDK usage." Zed: ACP, `claude -p`, the SDK and SDK apps "continue to work with Claude subscriptions exactly as they did before" | [support 15036540](https://support.claude.com/en/articles/15036540-use-the-claude-agent-sdk-with-your-claude-plan) |
| 2026-10-07 | Same support page: "Nothing changes for your plan … the Claude Agent SDK and `claude -p` still draw from your subscription limits." Max/Ultra/Team now also get monthly API credits, used only with an API key from the linked Console org. | same |
| 2026-10-10 | Amp lets users run their Claude Pro/Max plan inside Amp through the official Agent SDK and Claude Code: the pattern opencode later followed | owner's notes, 2026-11-11 (not independently verified) |
| 2026-10-16/17 | A legacy opencode system is broken into. Provider keys saved in OpenCode Console may have been exposed (OpenCode: tied to a large-scale distillation attack; 1.4% of Console users; "the Console, not OpenCode itself"). Nothing about subscription support was removed. | owner's notes, 2026-11-11 (not independently verified) |
| 2026-10-17 | **A real recorded failure** in opencode's Claude Agent tests: Claude Code 2.1.302, SDK 0.3.301, `apiKeySource: none` (a subscription login): `API Error: 400 Third-party apps now draw from your extra usage, not your plan limits. Add more at claude.ai/admin-settings/usage and keep going.` | opencode `packages/core/test/plugin/provider/claude-agent/fixtures/recordings/billing-error.jsonl` |
| 2026-10-30 | opencode 2.2.3 adds "Claude Agent" (SDK, the user's own `claude`). The same day its docs page is made `noindex` and it is left off the popular list; 2026-11-06 the home page drops its Claude Code mention. **The provider still ships** (v2.2.7 at the clone's head) and is actively maintained: fixes through 2026-11-10, and plan usage reported on step metadata (`c8a473bead`). | opencode `f29e281322`, `958f965d35`, `9cc63ddd2e`, `993a280887` |
| 2026-11-11 | CL0 on the owner's plan: `claude -p` stream-json and `claude-agent-acp` turns all bill to plan limits (`isUsingOverage: false`) | [08](08-cl0-results.md) |

## The billing evidence, and why it matters most

The support page (2026-10-07) and opencode's recording (2026-10-17) disagree.

**The owner's reading.** opencode's October trouble was its Console security
breach (2026-10-16/17, above), not a subscription-policy change, and its
Claude Agent provider still ships. The recording is a separate thing that
happens to share the date: its text is an Anthropic API 400. So it stays
evidence that such an error exists, and CLD-02 makes craze turn it, or any
overage, into an error.

Possible readings of the recording itself, none verified:

1. Anthropic tells "third-party apps" apart from Claude Code. It might use
   `CLAUDE_CODE_ENTRYPOINT` (the SDK sets `sdk-ts` or `sdk-py`; t3code sets
   `t3-code`), a replaced system prompt (opencode runs with `verbatimPrompts`
   and its own tools), or the absence of Claude Code's tools. It then bills
   those to extra usage. On this reading, route A or B with Claude Code's own
   prompt and tools might be billed to the plan, and opencode's configuration
   was not.
2. All SDK traffic on some plans or accounts moved to extra usage, and the
   support page lags.
3. It was a transient or account-specific state.

A provider that silently spends "extra usage" (real money) when the owner
expects plan limits is worse than no provider. So CL0's first probe measured it
on 2026-11-11 ([08](08-cl0-results.md)). Turns through `claude -p` stream-json
and through `claude-agent-acp` on the owner's plan all reported
`isUsingOverage: false` with plan status `allowed`. Overage is disabled
org-wide on that account, so a turn moved to extra usage would have failed, as
opencode's did; none failed. Reading 2 is therefore false for this account
today. Readings 1 and 3 remain possible for opencode's own failure.

Whatever the probe shows, the provider, if built, must:
- surface a `result` whose text is a billing or "extra usage" error as a typed
  craze error, not as assistant text;
- show which login a session runs on, at start;
- never pass `--allow-dangerously-skip-permissions` or similar on the user's
  behalf without the same consent craze already asks for with `--force`.
