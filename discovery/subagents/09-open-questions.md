# 09 — Open questions

`TQ-n`. Each has a default that holds until the owner or a plan decides
otherwise. A resolved question stays here, rewritten in place as
`**Resolved (TD-nn).**`.

**TQ-1 — Plain verbs, or a `craze task` group?** Plain verbs next to `new`,
`ps` and `attach`: `craze new --wait`, `craze send`, `craze wait`, `craze
result`, `craze cancel`, `craze stop`. A group is easier to document for
plugins (`craze task start|send|wait|…`). *Default: plain verbs.* Most of
them make sense for any session, such as sending a message to a session open
in the TUI. What makes a call sub-agent-like is a flag.

**TQ-2 — How an unattended session answers asks.** A session created for a
sub-agent usually has no client attached. *Default:*
- a question takes its first option, as `craze prompt` answers one (the
  approval policy has no skip, `internal/agent/asks.go:259-276`);
- a plan is accepted;
- a permission request (which only arises with `--no-force`) stays open, and
  the waiting command returns `needs_input` with its own exit code, so the
  orchestrator decides.

The host applies the policy, not the client, so it holds between calls. See
[03](03-design.md).

**TQ-3 — Does craze replace `codex exec` in `cc-plugins`' flows?** Same model,
same finding on one planted bug. codex was faster and leaner, and its
read-only mode is an OS sandbox ([02](02-experiments.md) E5). *Default: not
for reviews yet.* Revisit when craze has a sandboxed read-only mode and
`eval/` has compared the two on more than one sample. For implementation and
rescue work, `craze:sol` can be used now.

**TQ-4 — A follow-up after the session's host has exited.** A host with no
client exits after `host_idle_exit` (an hour by default). A `SendMessage`
after that finds no session. *Default:* `craze send` loads the session in a
new host through the hub, the way `craze serve --load` loads one, then
prompts it. If that is out of reach for the first plan, `craze send` fails
saying the session has ended and how to resume it.

**TQ-5 — Which presets ship.** *Default:* `delegate` (names its target),
`sol`, `astra`, `muse`, `grok`, `cursor`. A preset whose provider is not
ready here fails with craze's own availability message.

**TQ-6 — Getting the prompt to craze without a heredoc.** A session pinned to
a worktree refuses heredocs and `$(…)`. *Default:* the wrapper writes the
prompt to a file with Claude Code's `Write` tool (so the agent's tools are
`Bash, Write`), and passes `--prompt-file`. A plain heredoc on stdin stays
supported for everyone else.

**TQ-7 — Marking sessions so `craze -c` skips them.** *Default:* a session
created with `--wait` (or through `craze send`'s reload) is recorded as
unattended in its session-index row. `craze -c` skips unattended rows unless
told otherwise, and `--resume`'s picker and the session list label them. A
plain `craze new` stays an ordinary session, since a person may attach to it.

**TQ-8 — A shorter idle exit for unattended sessions.** *Default:* the same
`host_idle_exit` as any session until TQ-4's reload exists. After that, a
short one (about ten minutes), since a follow-up can always reload.

**TQ-9 — What `TaskStop` sends to a sub-agent's Bash call.** The waiting
commands cancel their turn on SIGINT, SIGTERM or SIGHUP ([03](03-design.md)).
Claude Code's documentation does not say which signal a stopped sub-agent's
foreground command gets, or whether it is SIGKILL. *Default:* catch the three
catchable signals, and verify live in T1. If `TaskStop` turns out to kill
outright, the plugin documents `craze cancel <id>` as the way to stop the
work, and the parity row in [04](04-parity.md) says so.

**TQ-10 — Ordering against plan 040 (`/btw`).** Plan 040 is executing in
another session. It changes the same protocol files: `internal/protocol/types.go`,
`schema/info.json`, the schema tests, `docs/reference/protocol.md`, and the
wire fixtures (it adds 27–29). *Default (revised by TD-08):* T1 changes no protocol
file, so it can run beside plan 040. Two things remain: the two sessions must
not gate at the same time on one machine, and the branch rebases onto
`origin/main` before its push. A new fake-agent script or a TUI change waits
for 040 if it would touch 040's files.
