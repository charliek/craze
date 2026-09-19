# 10 — Open questions

Each has a default so a plan can proceed without an answer. Resolved rows are
kept and rewritten in place as `**Resolved (SD-nn).**`.

| # | question | default if unanswered | needed by |
|---|---|---|---|
| SQ1 | Journal location and naming: own tree `$CRAZE_HOME/journal/<cwd-slug>/<utc>_<session-id>.jsonl`, or beside the harness store for native sessions? | Own tree for every provider, same slug and timestamp scheme as the harness store so files pair by eye. | S1 |
| SQ2 | Streaming text: is every delta a sequenced journal line, or are deltas coalesced per transcript entry? How does a resume land inside a streaming entry? | Coalesce per entry (flush on 8 KiB / 2 s / kind change / turn end); entries have stable ids and events are upserts, so a resume mid-entry re-sends the entry. Live subscribers still get deltas immediately. | S1 |
| SQ3 | Journal retention, size caps, and secrets: tool output and prompts can hold credentials. | `0600` files in a `0700` tree, no redaction, `journal = false` opt-out, no automatic pruning until sizes are measured in real use. | S1 |
| SQ4 | Raw ACP wire capture in a `.wire.jsonl` sidecar: on by default? | On by default with a size cap and rotation; the owner asked for rich debug data and H0 showed what a recorder that discards raw frames costs. Revisit if sizes are unreasonable. | S1 |
| SQ5 | Identity: is the protocol's `sessionId` the ACP session id, or a craze-minted id that survives `session/load` into a new agent session? | craze-minted host/session id in the protocol and journal header, with the provider's id recorded beside it; roost ownership keeps using the provider id it uses today, and `craze bridge --session` accepts either. | S1/S2 |
| SQ6 | Hub lifecycle: auto-spawned by the first host or client that needs it (prox), or explicit `craze hub`? Does it exit when idle? | Auto-spawn by re-exec, exit when the roster is empty and no client is connected. | S4 |
| SQ7 | Does `craze attach` reuse the full TUI (`tui.Model` over a remote subscription), or a thinner viewer? | Full TUI: after S1 the in-process TUI is already a subscription client, so a socket-backed `agent.Session` implementation gives attach nearly for free and is the best protocol test. | S2 |
| SQ8 | shed side: the lane wiring assumes a loopback HTTP base URL. How does a duplex-stream transport land in `shed-core`, how does desktop dial locally (direct Unix socket), and how do headless sessions with no roost tab get listed on the phone? | New lane transport variant keyed on kind `craze`; desktop connects to the socket directly; headless rows come from a hub roster subscription once S4 exists, until then only tab-hosted sessions show. | S3 |
| SQ9 | Snapshot size for very long sessions. | Full snapshot, bounded by the existing transcript caps (5,000 entries main, 64 KiB per entry); windowing is deferred until a phone shows it is needed. | S2 |
| SQ10 | Two clients acting at once: both prompting, or both changing model or mode. | The queue is shared engine state, so prompts simply queue in arrival order; settings are last-write-wins and broadcast; composer drafts are client-local and never shared. | S1b |
| SQ11 | Scopes for non-local clients: may a remote client answer `allow_always`, change mode to yolo, or create sessions? | Three scopes (`observe`, `operate`, `approve`); `allow_always` and permission-mode changes need `approve`; local socket clients hold all three. Only matters from S6. | S6 |
| SQ12 | Quit versus detach: today quitting the TUI ends the session. When can a TUI leave a session running? | Quit stays quit through S3. S4 adds an explicit detach (key and `/detach`) that turns the process into a headless host; plain quit still ends the session, with a confirm when a turn is working. | S4 |
