# 01 — Goals and scope

## Goal

Make a craze session something **several clients can observe and control at
once**, through one documented interface, so that:

1. **shed-mobile and shed desktop** get a first-class lane for craze — read
   the transcript, send a prompt, cancel, answer a permission or a question —
   over SSH, with no token, port, or discovery record to manage.
2. **Sessions can run headless** and be listed, checked on, and attached to
   from an **agent view** inside the TUI (the grok `/agent-dashboard`, Claude
   Code left-arrow shape).
3. Directionally, the same interface can be served to a **web UI** on the
   tailnet and, later, republished through a **remote relay** for web and
   mobile clients off the tailnet.

The owner's framing (2026-09-19): each of these makes craze more likely to be
the daily driver; not all need to be built; the combination points toward an
IDE-like surface (t3code) without that being a goal now.

## Why this is one project, not three

All three features need the same four things the engine lacks today (`03`):
multi-subscriber events, an engine-owned sequenced transcript, engine-owned
asks and turn state, and idempotent commands. Once those exist, the features
are transports and frontends over one protocol (SD-01):

| feature | is |
|---|---|
| shed lane | the protocol over a Unix socket, reached through `craze bridge` on an SSH exec |
| agent view | the TUI as a client of a hub that lists session hosts |
| web UI | the hub serving the protocol over WebSocket plus a static bundle |
| remote relay | the hub dialing **out** and republishing the same protocol |

## In scope

| area | first phase |
|---|---|
| Event fan-out with per-subscriber budgets | S1 |
| Sequenced on-disk journal for every provider, ACP and native | S1 |
| Engine-owned asks, turn state, render-free transcript model | S1 |
| Per-session Unix socket, protocol spec, fake server, `craze bridge` | S2 |
| `shed-craze` lane adapter (in the shed repo) | S3 |
| Headless session hosts, per-machine hub, `craze ps` / `craze attach` | S4 |
| Agent view in the TUI | S5 |
| `craze web` on loopback / tailnet | S6 (directional) |
| Outbound relay uplink and a hosted server | S7 (directional) |

## Non-goals

- A single daemon that owns every session (rejected, SD-02).
- A TCP listener for local control, bearer tokens, or discovery records on
  the local path (SD-04). These are what the gx lane needs and what a Unix
  socket removes.
- Pane or pty scraping, ever. Status comes from the engine; roost stays the
  status authority for tab-hosted sessions (SD-15).
- Remote PTYs, port forwards, file sync, or an IDE surface. The protocol must
  not preclude them; nothing here builds them.
- yamux in the first remote cut (SD-13).
- Speaking ACP upward as the control protocol (considered, SD-03): ACP has no
  multi-client attach, resumable cursors, roster, queue, or addressable asks.
- Multi-user or multi-tenant anything. One human, their machines.

## What "done" means for the committed part (S1–S5)

Borrowing shed's bar (`epics/roost-pivot.md`): it is a demonstration, not a
checklist. Start craze in a roost tab on one machine; from the phone, read the
transcript, send a prompt, cancel, and answer a permission and a question
while the TUI shows every one of those happen. Then start two headless
sessions, close the terminal, reopen craze, and find both in the agent view,
one of them blocked on an ask that can be answered from there.
