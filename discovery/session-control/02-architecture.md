# 02 — Architecture

## The pieces

```mermaid
flowchart LR
  subgraph machine["one machine, one user"]
    subgraph hostA["session host (process)"]
      engA["engine: session + journal + asks"] --- tuiA["TUI (optional, in-process client)"]
    end
    subgraph hostB["session host, headless"]
      engB["engine"]
    end
    hub["hub (owns no sessions)"]
    hostA -- "registers" --> hub
    hostB -- "registers" --> hub
    bridge["craze bridge (stdio pump)"] --> hub
    view["agent view (TUI as hub client)"] --> hub
  end
  phone["shed-mobile / shed desktop"] -- "SSH exec" --> bridge
  hub -. "S6: WebSocket + static bundle" .-> web["browser on tailnet"]
  hub -. "S7: dials out" .-> relay["remote relay"]
```

| piece | what it is | owns |
|---|---|---|
| **Session host** | One OS process per session. Today's `craze` is a host with a TUI attached in-process; `craze serve` (S4) is the same host with none. | The `agent.Session`, the agent child process, the journal, pending asks, its own socket |
| **Hub** | One small per-user process at a fixed socket path, auto-spawned, exits when idle. | A roster of hosts, routing of client connections to them, spawning headless hosts. **No sessions.** |
| **Client** | Anything speaking the protocol (`05`): the TUI, `craze attach`, the agent view, `shed-craze`, a browser. | Its own view state and drafts only |
| **Bridge** | `craze bridge`: stdin/stdout pumped to the right local socket. | Nothing. It is the stable entry point for SSH clients (SD-05). |

## Why process-per-session and a thin hub (SD-02)

The alternative is a prox-style daemon holding every session as a goroutine.
It was rejected because:

- A daemon crash or upgrade kills every running agent. With hosts, the hub
  can die or be replaced and sessions keep running, then re-register.
- The agents are already processes (`cursor-agent`, `grok`, `gx`). In-process
  native sessions would share one failure domain: one panic, all sessions.
- prox's daemon has to demand exact binary version equality on its socket
  (`internal/proxyd/daemon.go`, `VersionMismatchError`). A hub that only
  routes frames needs one protocol-version integer.
- Nothing in craze's engine is a singleton, but process-wide state is:
  `CRAZE_HOME`, `$HOME`-keyed skill and plugin caches, signal handling. A
  process per session keeps those honest.

What the hub buys over bare per-session sockets: one roster subscription
instead of N connections, one stable path, a place to spawn headless hosts,
and, later, one outbound uplink per machine rather than per session.

**Sequencing (SD-06):** per-session sockets ship first (S2); the hub arrives
with headless hosts (S4) and speaks the identical protocol. `craze bridge`
hides which of the two it dialed, so `shed-craze` never changes.

A `sessionId` on every method is necessary and **not sufficient** for that
promise (SD-28). S2's protocol must already separate connection-level from
session-level capabilities and incarnation (a hub fronts heterogeneous hosts;
`hello` cannot return one provider's capabilities), give every subscription
an id with unsubscribe, and route notifications by it. The **roster has its
own epoch and cursor**: a crashed host cannot journal its own removal and
per-session sequences do not order a machine, so after a hub restart clients
reseed the roster authoritatively. The hub also needs a registration grace
before idle exit, and must pass host identity and unknown payload fields
through untouched.

**Lifecycle separation comes before the hub, too (SD-28).** Today
`requestQuit`, `finishRun`, and `SIGHUP` all close the owned session. S2
defines transport close, closing an attached view, and explicitly stopping
the session as three different things, keeping today's quit behavior. S4's
detach is more than a key binding: the process must give the shell back,
release the terminal, and keep owning its agent child, and `acp.Spawn` today
isolates only the child's process group; startup and process-group
preparation are prerequisites.

## Paths

```
<runtime dir>/                   0700, validated (below)
  hub.sock                       0600   S4
  hub.lock                       lifetime flock, prox's and roost's pattern
  h/<short-id>.sock              0600   S2
  h/<short-id>.lock              lifetime flock held by the host
$CRAZE_HOME/journal/<cwd-slug>/<utc>_<incarnation-id>.jsonl   0600   S1 (SQ1)
```

**The runtime namespace is an S2 design item, not a detail (SD-27).** Three
facts make `$CRAZE_HOME/run/host/<session-id>.sock` wrong as written:

- `sun_path` is about 104 bytes on macOS and 108 on Linux; a long
  `CRAZE_HOME` plus a uuid overflows it. Names must be short, and the runtime
  dir should prefer a short base (`$XDG_RUNTIME_DIR/craze` where it exists),
  with an actionable refusal when no path fits.
- `paths.CrazeDir` deliberately allows a **relative** `CRAZE_HOME`, so two
  working directories could select two hubs. The runtime dir is resolved to
  one absolute, normalized path per user and `CRAZE_HOME`.
- An SSH exec does not inherit the environment or `PATH` of the tab that
  launched craze. How `craze bridge` finds the same namespace and how the
  client finds the binary must be specified; roost's `remote_command_for`
  (`roost-ipc/src/ssh.rs`) exists for exactly this. Session ids are validated
  as opaque identifiers before any path is built, and are shell-quoted if they
  ever appear in a command line; protocol input never becomes shell text.

**Connect failure is not authority to unlink.** It races another host's bind
and a predecessor's shutdown. The rules, all with working references in roost:
validate the runtime tree's owner, type, and mode (and its ancestors) before
probing, with an explicit symlink policy (`roost-ipc/src/runtime_dir.rs`);
hold a **lifetime lock** around probe, unlink, and bind
(`roost-engine/src/single_instance.rs`); unlink only a socket that is
provably ours (`roost-session/src/socket_guard.rs`); classify staleness
conservatively. A host removes its own socket on exit, including the `SIGHUP`
path added in plan 017.

## Local trust model (SD-04)

The boundary is **same uid on this machine**: a validated `0700` directory,
`0600` sockets, and a peer-credential uid check (`SO_PEERCRED` on Linux,
`LOCAL_PEERCRED` on macOS; roost's `roost-ipc/src/peer.rs` has both). The
check runs on **accept and on dial**, because a client resolving a runtime
path can be handed a substituted endpoint, and a failed credential lookup
rejects. Logging in over SSH as the owner *is* the authorization, exactly as
for roost (`epics/roost-pivot.md`: "UDS + SSH. No network listener, no auth
layer").

This is the main thing done differently from the gx lane. Loopback TCP has no
peer identity, so gx needs a token file with `O_EXCL`/`O_NOFOLLOW`/mode
discipline, a discovery record, `healthz` + `instanceId` pinning against port
recycling, an SSH probe script, and a roost metadata hint that goes stale and
cannot be cleared. None of those exist here.

Within that boundary the gate is the **session's own state**, published as a
verb × activity table in the spec (`05`), which is gx's framing and worth
keeping: credentials are provenance, state is the gate.

## The remote direction (S6, S7 — not committed)

- **S6, `craze web`:** the hub serves the protocol over WebSocket and a static
  bundle, bound to loopback or a tailnet address only. This is t3code's
  direct/Tailscale route: "bring your own network", no relay, no accounts.
  Binding rules can copy prox's listen-address classifier
  (`internal/proxyd/hub.go`): loopback or a point-to-point tunnel interface,
  never `0.0.0.0`.
- **S7, relay:** the hub dials **out** to a remote server and republishes the
  same protocol, so NAT and containers work. From prox: HTTP/1.1 `Upgrade`
  then a hijacked connection, generation-counted sessions, lease-based
  liveness with a reconnect grace, one goroutine owning the connection state
  machine, and a never-fatal posture (a dead relay costs one warning).
  **Not** from prox: its control plane on a second connection, its single
  shared bearer token, and origin strings asserted by the client.
- **Transport (SD-13):** session-tagged frames over one WebSocket first. It
  passes through proxies and TLS terminators and leaves the server language
  open. yamux earns its place only when raw byte streams (terminals, port
  forwards) need per-stream flow control; it can run inside the WebSocket.
- **Server (SD-14):** Go first, sharing the protocol package. The protocol is
  specified by JSON Schema, fixtures, and a fake (`05`), and `shed-craze` is a
  Rust client from S3, so a non-Go implementation is proven possible early.

**Security posture for anything remote.** Remote control of a coding agent is
remote code execution by design. Requirements recorded now so the protocol
does not preclude them: per-machine enrollment with a machine key (no shared
token); TLS always; scopes at least `observe` / `operate` / `approve`
(SQ11); a **capability allow-list enforced on the craze side** (SD-16, prox's
D11: the controlled end decides what is invocable); remote access opt-in per
machine and visible in the TUI while a remote client is attached; a separate
positive-list of methods for remote clients rather than "local minus some".

## What stays where

| concern | lives in | must not |
|---|---|---|
| Session lifecycle, asks, turn state, transcript model, journal | the engine, `internal/agent` and new packages beside it | import `internal/tui` |
| Protocol types, schema, encode/decode | a new package with no UI or engine imports | reach into `internal/agent` internals; it maps from `agent.Event` |
| Socket server, hub, bridge | new packages under `internal/` | hold session semantics |
| Rendering, keymap, composer, dialogs | `internal/tui` | hold state two clients must agree on |

The lint-enforced boundary that keeps `internal/harness` ignorant of craze
(`discovery/native-harness/02-architecture.md`) is unchanged (SD-08).
