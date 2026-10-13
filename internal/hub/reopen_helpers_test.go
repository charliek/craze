package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// session.reopen's tests' rig (reopen_test.go). A host spawned with --load —
// by the hub's reopen, or by a test standing in for another loader — is this
// test binary run as a fake craze serve that loads (runLoadChild): it takes
// the session's claim as a real host does (rundir.ClaimSession), answers
// held when another holds it, and otherwise serves the session as a fake host
// (fakehost.RunSpawnedWith) whose start and stop the test holds.

// runLoadChild is the created-host child for a --load (create_helpers_test's
// init hands it one), its mode comma-separated words:
//
//   - its start or its answer, one of: "ok" (the default), "fail" (the start
//     fails: failCause), "gate:<dir>" (the start waits for <dir>/go, as a
//     create's gated host does), "refuse:<words>" (a refused ready line in
//     craze's words, nothing claimed), "notok" (a not-ok line that is
//     neither held nor refused), "heldby:<pid>" (a held line naming pid as
//     the holder, nothing claimed), "hang" (no ready line, ever, nothing
//     claimed);
//   - "mark": its stop marks its entry stopping before anything of it is
//     seen (fakehost.Options.StoppingMark) — a host of this release's;
//     without it, a host from before the mark;
//   - "stop:<dir>": its stop held where a test wants it — <dir>/stopping
//     written once the stop is heard (and marked), and <dir>/unlist waited
//     for before it closes and unlists itself; then <dir>/unlisted written,
//     and <dir>/exit waited for before it exits and its claim goes with it;
//   - "watch:<dir>": <dir>/stopped written when a session.stop reaches it,
//     <dir>/signalled when SIGTERM or SIGINT does;
//   - "nostop": session.stop refused.
//
// The session it claims is the load's id, a legacy id's ":" made "-" (a
// real host gives a legacy row a craze id under its claim; this one is
// fixed, so a test knows it). Its workspace is its working directory, as a
// load's is its row's.
func runLoadChild(a fakehost.SpawnArgs, mode string) {
	start, stopDir, watchDir := "ok", "", ""
	mark, nostop := false, false
	for w := range strings.SplitSeq(mode, ",") {
		switch {
		case w == "mark":
			mark = true
		case w == "nostop":
			nostop = true
		case strings.HasPrefix(w, "stop:"):
			stopDir = strings.TrimPrefix(w, "stop:")
		case strings.HasPrefix(w, "watch:"):
			watchDir = strings.TrimPrefix(w, "watch:")
		case w != "":
			start = w
		}
	}
	crazeID := strings.ReplaceAll(a.Load, ":", "-")
	answer := func(line hostspawn.ReadyLine) {
		b, _ := json.Marshal(line)
		f := os.NewFile(3, "ready")
		_, _ = f.WriteString(string(b) + "\n")
		_ = f.Close()
		os.Exit(1)
	}
	switch {
	case strings.HasPrefix(start, "refuse:"):
		answer(hostspawn.ReadyLine{Error: "craze: " + strings.TrimPrefix(start, "refuse:"), Refused: true})
	case start == "notok":
		answer(hostspawn.ReadyLine{Error: "craze serve: the control socket: no such place"})
	case strings.HasPrefix(start, "heldby:"):
		pid, _ := strconv.Atoi(strings.TrimPrefix(start, "heldby:"))
		answer(hostspawn.ReadyLine{Error: fmt.Sprintf("craze serve: that session is already running (pid %d)", pid),
			Held: &hostspawn.ReadyHeld{HostID: "0000000000aa", PID: pid, CrazeSessionID: crazeID}})
	case start == "hang":
		select {}
	}
	env := rundir.ProcessEnv()
	claim, err := rundir.ClaimSession(env, crazeID, a.HostID)
	var held *rundir.HeldError
	if errors.As(err, &held) {
		answer(hostspawn.ReadyLine{Error: "craze serve: that session is already running (pid " + strconv.Itoa(held.Holder.PID) + ")",
			Held: &hostspawn.ReadyHeld{HostID: held.Holder.HostID, PID: held.Holder.PID, CrazeSessionID: crazeID}})
	}
	if err != nil {
		answer(hostspawn.ReadyLine{Error: "craze serve: " + err.Error()})
	}
	defer func() { _ = claim.Release() }()
	if wd, err := os.Getwd(); err == nil && a.Workspace == "" {
		a.Workspace = wd
	}
	o := fakehost.Options{Stop: !nostop, StoppingMark: mark, CrazeSessionID: crazeID}
	switch {
	case start == "ok":
	case start == "fail":
		o.Start = func(context.Context) error { return errors.New(failCause + "\nand a second line") }
	case strings.HasPrefix(start, "gate:"):
		o.Start = gatedStart(strings.TrimPrefix(start, "gate:"), a.HostID)
	default:
		fmt.Fprintln(os.Stderr, "created host child: no load mode", mode)
		os.Exit(97)
	}
	raw := make(chan os.Signal, 4)
	signal.Notify(raw, syscall.SIGTERM, syscall.SIGINT)
	sigs := make(chan os.Signal, 4)
	go func() {
		for sig := range raw {
			if watchDir != "" {
				_ = os.WriteFile(filepath.Join(watchDir, "signalled"), []byte(sig.String()), 0o600)
			}
			sigs <- sig
		}
	}()
	prepare := func(h *fakehost.Host) {
		if watchDir == "" {
			return
		}
		go func() {
			<-h.StopHeard()
			_ = os.WriteFile(filepath.Join(watchDir, "stopped"), []byte(a.HostID), 0o600)
		}()
	}
	hooks := fakehost.SpawnHooks{Stopping: func() {
		if stopDir != "" {
			_ = os.WriteFile(filepath.Join(stopDir, "stopping"), []byte(a.HostID), 0o600)
			awaitFile(filepath.Join(stopDir, "unlist"))
		}
	}}
	err = fakehost.RunSpawnedWith(a, env, o, prepare, sigs, hooks)
	if stopDir != "" {
		_ = os.WriteFile(filepath.Join(stopDir, "unlisted"), []byte(a.HostID), 0o600)
		awaitFile(filepath.Join(stopDir, "exit"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "created host child:", err)
		_ = claim.Release()
		os.Exit(1)
	}
	_ = claim.Release()
	os.Exit(0)
}

// awaitFile waits, in a child, for path to exist: for good, unless its
// watchdog ends it.
func awaitFile(path string) {
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitFile waits, in the test, for path to exist, within step.
func waitFile(t *testing.T, path string) {
	t.Helper()
	waitFor(t, filepath.Base(path), func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

// exists says path exists now.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// targets is a test hub's saved sessions by id, which its Reopen resolves:
// changed under its lock as a test goes.
type targets struct {
	mu sync.Mutex
	m  map[string]Reopening
	// asked counts the resolutions.
	asked int
}

func (ts *targets) set(id string, t Reopening) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.m[id] = t
}

// reopenCreates is creates("cursor") that also answers sessions.saved (no
// sessions) and resolves session.reopen's ids from ts: an id ts does not hold
// is unknown_session.
func reopenCreates(ts *targets) *Creates {
	c := creates("cursor")
	c.Saved = func(protocol.SavedParams, func(string, ...any)) (protocol.SavedResult, *protocol.Error) {
		return protocol.SavedResult{Sessions: []protocol.SavedSession{}}, nil
	}
	c.Reopen = func(id string, _ func(string, ...any)) (Reopening, *protocol.Error) {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		ts.asked++
		t, ok := ts.m[id]
		if !ok {
			return Reopening{}, refused(protocol.CodeUnknownSession, protocol.ReasonUnknownSession, "no saved session %s", id)
		}
		return t, nil
	}
	return c
}

// plainTarget is a person's saved session id in workspace ws: loaded by its
// craze id, no settings.
func plainTarget(id, ws string) Reopening {
	return Reopening{Load: id, CrazeID: id, Provider: "cursor", ProviderSessionID: "prov-" + id, Workspace: ws}
}

// reopenHub is a hub that reopens ts's sessions over env, its schedule hk's
// (nil: production's), each host it spawns a child in mode(n): the hub,
// serving, its spawns and its socket.
func reopenHub(t *testing.T, env rundir.Env, hk *hooks, ts *targets, mode func(n int) string) (*running, *spawned, string) {
	t.Helper()
	s := hostsAsChildren(t, env, mode)
	rn := runWith(t, env, hk, reopenCreates(ts))
	sock := rn.line(t).Socket
	rn.serving(t)
	return rn, s, sock
}

// newTargets is ts holding the sessions given, by id.
func newTargets(ts ...Reopening) *targets {
	out := &targets{m: map[string]Reopening{}}
	for _, t := range ts {
		id := t.CrazeID
		if id == "" {
			id = t.Load
		}
		out.m[id] = t
	}
	return out
}

// reopenOn sends session.reopen with params on c — a client that said hello —
// and reads its answer.
func reopenOn(t *testing.T, c *client, params any) protocol.Response {
	t.Helper()
	return c.call(t, protocol.MethodSessionReopen, params)
}

// reopenID is reopenOn of {id} on a fresh connection of its own to sock: one
// short connection per reopen, as a client's.
func reopenID(t *testing.T, sock, id string) protocol.Response {
	t.Helper()
	c := dial(t, sock)
	c.hello(t)
	return reopenOn(t, c, protocol.ReopenParams{ID: id})
}

// reopened is a reopen's answer, which must be a result.
func reopened(t *testing.T, resp protocol.Response) protocol.ReopenResult {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("session.reopen refused: %s (%s/%s, cause %q)", resp.Error.Message, resp.Error.Data.Code, resp.Error.Data.Reason, resp.Error.Data.Cause)
	}
	var res protocol.ReopenResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// holderProc is a loading host a test started itself — another loader than
// the hub (the TUI, craze send, a direct craze serve --load).
type holderProc struct {
	id, socket, session string
	pid                 int
	line                hostspawn.ReadyLine
	child               *hostspawn.Child
}

// startHolder starts a loading host of load's session in dir, as the hub's
// spawns are started but by the test — the child in mode — and reads its ready
// line, which must be ok unless notOK. It is SIGKILLed, if it still runs,
// when the test ends.
func startHolder(t *testing.T, env rundir.Env, load, dir, mode string, notOK bool) holderProc {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logs, err := rundir.HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	hostID := rundir.NewHostID()
	argv := hostspawn.Args(hostspawn.Spec{HostID: hostID, Log: filepath.Join(logs, hostID+".log"), Load: load, NoHostStatus: true})
	b, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	cmd := hostChildCmd(exe, env, mode, b)
	cmd.Dir = dir
	child, r, err := hostspawn.StartCmd(cmd, hostspawn.HostChildEnv, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !child.Exited() {
			_ = cmd.Process.Kill()
		}
		_ = stillThere(t, []int{child.PID()})
	})
	ctx, cancel := context.WithTimeout(context.Background(), step)
	defer cancel()
	line, failure, why := hostspawn.ReadReady(ctx, r)
	_ = r.Close()
	if failure != 0 {
		t.Fatalf("the holder gave no ready line: %d %s", failure, why)
	}
	if line.OK == notOK {
		t.Fatalf("the holder's ready line %+v", line)
	}
	return holderProc{id: hostID, socket: line.Socket, session: line.CrazeSessionID, pid: child.PID(), line: line, child: child}
}

// rewriteEntry rewrites host id's registry entry in env as edit says, at once
// (a temporary renamed over it, as a host rewrites its own): a host's rewrite
// of its entry, played by the test. It may run on any goroutine, and fails by
// its error.
func rewriteEntry(env rundir.Env, id string, edit func(*rundir.Entry)) error {
	dir := filepath.Join(env.Home, ".cache", "craze", "hosts")
	b, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return err
	}
	var e rundir.Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return err
	}
	edit(&e)
	if b, err = json.Marshal(e); err != nil {
		return err
	}
	tmp := filepath.Join(dir, id+".json.test")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, id+".json"))
}

// loopCounts is a reopen's turns of its loop, counted through its seams: its
// scans, its joins, and its waits' polls — slept, those that are not the
// first of their wait (each turn's scan is followed by one wait at most), so
// a poll after a sleep.
type loopCounts struct {
	scans, joins, polls, slept atomic.Int64
	// fresh says a scan came since the last poll: the next poll is a
	// wait's first.
	fresh atomic.Bool
}

// loopHooks is what countLoop tells of the reopen's turns, each after it is
// counted — and, tick set, its clock: reopenClock made the test's, frozen
// but for tick added as a poll begins, every poll with tickEvery, else each
// slept one. The waits' time — the holder allowance spent, a tentative
// mark's poll (rendezvousPoll of waiting) — then runs on polls, never on how
// soon a poll comes: a first poll, however late, never finds a mark's poll
// passed unless tickEvery says so.
type loopHooks struct {
	scanned   func(rundir.Entry, bool)
	joining   func(rundir.Entry)
	polled    func()
	tick      time.Duration
	tickEvery bool
}

// countLoop counts the reopen's turns (loopCounts), as hk says.
func countLoop(t *testing.T, hk loopHooks) *loopCounts {
	t.Helper()
	c := &loopCounts{}
	var clock atomic.Int64
	if hk.tick > 0 {
		base := time.Now()
		setVar(t, &reopenClock, func() time.Time { return base.Add(time.Duration(clock.Load())) })
	}
	setVar(t, &reopenScanned, func(e rundir.Entry, found bool) {
		c.scans.Add(1)
		c.fresh.Store(true)
		if hk.scanned != nil {
			hk.scanned(e, found)
		}
	})
	setVar(t, &reopenJoining, func(e rundir.Entry) {
		c.joins.Add(1)
		if hk.joining != nil {
			hk.joining(e)
		}
	})
	setVar(t, &hostspawn.AwaitPolled, func() {
		c.polls.Add(1)
		first := c.fresh.Swap(false)
		if !first {
			c.slept.Add(1)
		}
		if hk.tick > 0 && (hk.tickEvery || !first) {
			clock.Add(int64(hk.tick))
		}
		if hk.polled != nil {
			hk.polled()
		}
	})
	return c
}

// spawnedID is the host id the i-th spawned host was given.
func spawnedID(s *spawned, i int) string {
	_, argv := s.cmd(i)
	return fakehost.ParseSpawnArgs(argv).HostID
}

// stopHolder sends session.stop to h, which must take it.
func stopHolder(t *testing.T, h holderProc) {
	t.Helper()
	if err := stopCreated(h.socket, h.session); err != nil {
		t.Fatalf("session.stop to host %s: %v", h.id, err)
	}
}

// hostsServing is every live host in env's registry serving session id.
func hostsServing(t *testing.T, env rundir.Env, id string) []rundir.Entry {
	t.Helper()
	entries, err := rundir.Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	var out []rundir.Entry
	for _, e := range entries {
		if e.CrazeSessionID == id {
			out = append(out, e)
		}
	}
	return out
}

// barrier holds the first n callers of wait until all n have come.
type barrier struct {
	mu   sync.Mutex
	n    int
	here int
	all  chan struct{}
}

func newBarrier(n int) *barrier { return &barrier{n: n, all: make(chan struct{})} }

// wait holds the caller until n have come (the first n alone; any later
// caller passes at once), or step has passed.
func (b *barrier) wait() {
	b.mu.Lock()
	b.here++
	if b.here == b.n {
		close(b.all)
	}
	late := b.here > b.n
	b.mu.Unlock()
	if late {
		return
	}
	select {
	case <-b.all:
	case <-time.After(step):
	}
}
