# 09 — Open questions

`TQ-n`. Each has a default that holds until the owner or a plan decides
otherwise. A resolved question stays here, rewritten in place as
`**Resolved (TD-nn).**`.

**TQ-1 — Plain verbs, or a `craze task` group?** **Resolved (TD-10).** Plain
verbs: `craze new --wait`, `craze send`, `craze wait`, `craze cancel`, `craze
stop`; no `craze result` (a handle replays a finished turn).

**TQ-2 — How an unattended session answers asks.** **Resolved (TD-11).** A
question takes its first option; on cursor, grok and gx a plan is returned and
the turn ends (grok implemented an accepted plan live); native's accepted plan
ends its turn; a permission request (`--no-force` only) reports `needs_input`,
exit 4. The host applies the policy, so it holds with no client attached.

**TQ-3 — Does craze replace `codex exec` in `cc-plugins`' flows?** Same model,
same finding on one planted bug. codex was faster and leaner, and its
read-only mode is an OS sandbox ([02](02-experiments.md) E5). *Default: not
for reviews yet.* Revisit when craze has a sandboxed read-only mode and
`eval/` has compared the two on more than one sample. For implementation and
rescue work, `craze:sol` can be used now.

**TQ-4 — A follow-up after the session's host has exited.** **Resolved
(TD-12).** `craze send` reloads the session from the index, through a direct
spawn rather than the hub, with its recorded start settings.

**TQ-5 — Which presets ship.** **Resolved (TD-13).** `delegate`, `sol`,
`astra`, `muse`, `grok`, `cursor`.

**TQ-6 — Getting the prompt to craze without a heredoc.** **Resolved
(TD-14).** `Write` to `/tmp/craze-delegate-<12 hex>.md`, then `--prompt-file …
--remove-prompt-file`; the default-mode rule is `Edit(//tmp/craze-delegate-*)`.

**TQ-7 — Marking sessions so `craze -c` skips them.** **Resolved (TD-16).**
The index row's `unattended` record marks the session (set-only, inherited by
craze id); `-c` skips it, the picker and session list tag it `sub-agent`.

**TQ-8 — A shorter idle exit for unattended sessions.** *Default:* the same
`host_idle_exit` as any session for now; an unattended host stays until
`craze stop`. TQ-4's reload now exists (TD-12), so a short idle exit (about ten
minutes) is safe to add: TF-9.

**TQ-9 — What `TaskStop` sends to a sub-agent's Bash call.** **Resolved
(TD-15).** SIGTERM, to the Bash call's whole process tree by parent pid. The
waiting command catches it and cancels its own turn; a direct-spawned host is
outside that tree, so the session stays and `SendMessage` continues it
(live L6).

**TQ-10 — Ordering against plan 040 (`/btw`).** **Resolved (TD-08).** T1
changed no protocol file; it ran beside plan 040 without overlapping gates and
rebased onto `origin/main` once plan 040 merged.
