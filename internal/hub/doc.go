// Package hub is the per-machine hub (plan 032 §3.5–§3.8, S4b): one process
// per user, HOME and CRAZE_HOME namespace that knows every session host on
// the machine — by reading the registry, which every host already writes
// (P1) — and that a client asks for the roster, routes through to a session
// (session.connect), and asks to start one (session.create). It is `craze
// hub`, started on demand by Ensure and gone when idle: no client and no live
// host, after a grace (P12, owner decision 7).
//
// The pieces:
//
//   - lifecycle.go: Run, the process — its lock, staged start, record,
//     Lost, signals, idle exit, orphan sweep and bounded teardown.
//   - server.go: its socket's connections — peer check, hello, refusals,
//     the roster's two methods, and every write's bounds.
//   - roster.go: the roster — internal/roster's poll, run while someone
//     wants it, its rows bounded and forwarded, sessions.list and
//     sessions.subscribe, and each subscription's net-change notifications.
//   - ensure.go: Ensure, a client's find-or-start (and the wedged hub's
//     replacement, P17), and Command, the spawn seam.
//   - env.go: the environment contract (P13) for the hub and every host it
//     spawns.
//   - ready.go: the hub's ready line, both ends.
//
// The hub's files are rundir's (plan 032 §3.4): its lock and record in the
// cache tree (hubs/<ns>.lock, hubs/<ns>.json), its socket in its namespace's
// runtime directory (<base>/<ns>/hub.sock), its log beside the hosts' logs
// (LogPath). It spawns through internal/hostspawn, and imports no client
// package and no provider transport (.golangci.yml's hub rule): the CLI starts
// it and dials it, and the hosts it lists run the sessions.
package hub
