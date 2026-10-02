package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// session.create's tests' rig (create_test.go). A created host is this test
// binary re-executed as a fake craze serve (serveTestChild, below):
// fakehost.RunSpawned over the command line the hub built, so a create spawns
// a real process, reads its real ready line and dials its real socket, while
// the test decides how the session's start goes (its mode). Every child is
// SIGKILLed — and its exit waited for — when its test ends, and each watches
// the test process as its parent (childWatchdog).

// The created-host child's environment.
const (
	// serveTestChild runs this test binary as a fake craze serve; its value
	// is the mode (spawnedMode), its command line in serveTestArgv.
	serveTestChild = "CRAZE_HUB_TEST_SERVE"
	serveTestArgv  = "CRAZE_HUB_TEST_SERVE_ARGV"
)

// The modes of a created-host child (serveTestChild):
//
//   - "ok": its session starts at once.
//   - "fail": its start fails: failCause, then a second line.
//   - "gate:<dir>": its start waits for <dir>/go: its content "ok" starts it,
//     anything else fails it with failCause. It writes <dir>/waiting first.
//   - "exit": it exits 3 with no ready line.
//   - "notok": it answers a ready line that is not ok.
//   - "hang": it never answers its ready line, and serves nothing; with
//     ",ignoreterm" it ignores SIGTERM too, so only a SIGKILL ends it, and
//     with ",ignoreterm:<dir>" it says so once it does: it writes
//     <dir>/ignoring after its SIGTERM is ignored.
//
// Each serves session.stop and ends on it; a mode with the suffix ",nostop"
// refuses it (stop_unsupported), and ends only on SIGTERM. One with the
// suffix ",agent" first starts an agent of its own — a `sleep`, leading a
// process group of its own as an ACP agent does — and records it in its
// agents' record (<host-logs>/<hostId>.pgids) as craze serve records its
// agent, for the hub to end once the host has gone.
const failCause = "the agent could not start: fake agent missing"

func init() {
	mode, ok := os.LookupEnv(serveTestChild)
	if !ok {
		return
	}
	_ = os.Unsetenv(serveTestChild)
	parent := os.Getppid()
	if n, err := strconv.Atoi(os.Getenv(hubTestParent)); err == nil && n > 1 {
		parent = n
	}
	go childWatchdog(parent)
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv(serveTestArgv)), &argv); err != nil {
		fmt.Fprintln(os.Stderr, "created host child:", err)
		os.Exit(97)
	}
	a := fakehost.ParseSpawnArgs(argv)
	if hang, ok := strings.CutPrefix(mode, "hang"); ok {
		if rest, ok := strings.CutPrefix(hang, ",ignoreterm"); ok {
			signal.Ignore(syscall.SIGTERM)
			if dir, ok := strings.CutPrefix(rest, ":"); ok {
				_ = os.WriteFile(filepath.Join(dir, "ignoring"), []byte(a.HostID), 0o600)
			}
		}
		select {}
	}
	mode, agent := strings.CutSuffix(mode, ",agent")
	mode, nostop := strings.CutSuffix(mode, ",nostop")
	if agent {
		if err := startRecordedAgent(a.HostID); err != nil {
			fmt.Fprintln(os.Stderr, "created host child: its agent:", err)
			os.Exit(97)
		}
	}
	o := fakehost.Options{Stop: !nostop}
	switch {
	case mode == "ok":
	case mode == "fail":
		o.Start = func(context.Context) error { return errors.New(failCause + "\nand a second line") }
	case strings.HasPrefix(mode, "gate:"):
		dir := strings.TrimPrefix(mode, "gate:")
		o.Start = func(ctx context.Context) error {
			_ = os.WriteFile(filepath.Join(dir, "waiting"), []byte(a.HostID), 0o600)
			for {
				if b, err := os.ReadFile(filepath.Join(dir, "go")); err == nil {
					if strings.TrimSpace(string(b)) == "ok" {
						return nil
					}
					return errors.New(failCause)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
	case mode == "exit":
		os.Exit(3)
	case mode == "notok":
		f := os.NewFile(3, "ready")
		_, _ = f.WriteString(`{"ok":false,"error":"craze serve: no such provider here"}` + "\n")
		_ = f.Close()
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "created host child: no mode", mode)
		os.Exit(97)
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	if err := fakehost.RunSpawned(a, rundir.ProcessEnv(), o, nil, sigs); err != nil {
		fmt.Fprintln(os.Stderr, "created host child:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// startRecordedAgent starts a `sleep` leading a process group of its own and
// appends it to host hostID's agents' record, as craze serve records the
// agent it spawns (hostspawn.AgentGroup). An agent it could not record is
// killed and waited for before it returns (r38 7): nobody else could ever
// find it.
func startRecordedAgent(hostID string) (err error) {
	cmd := exec.Command("sleep", "600")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	id, err := rundir.ProcessIdentity(cmd.Process.Pid)
	if err != nil {
		return err
	}
	dir, err := rundir.HostLogDir(rundir.ProcessEnv())
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, hostspawn.AgentGroupsName(hostID)), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(hostspawn.AgentGroup{PGID: cmd.Process.Pid, Start: id.Start}.Line()); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// spawned is every created-host child a test's hub started (hostsAsChildren).
type spawned struct {
	mu   sync.Mutex
	cmds []*exec.Cmd
	argv [][]string
}

// count is how many hosts were spawned.
func (s *spawned) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cmds)
}

// cmd is the i-th spawned host's command, as the hub started it.
func (s *spawned) cmd(i int) (*exec.Cmd, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmds[i], s.argv[i]
}

// hostsAsChildren installs HostCommand for one test: each host the hub
// spawns is this test binary run as a fake craze serve (serveTestChild) in
// mode(n) — n counting the spawns from 0 — in the test's environment plus
// env's HOME, CRAZE_HOME and runtime base, so it is listed where the test's
// hub looks. Every child still running when the test ends is SIGKILLed and
// waited for.
func hostsAsChildren(t *testing.T, env rundir.Env, mode func(n int) string) *spawned {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A starved child takes its time to come up: the bounds on its answer
	// and its dial are the test's step, as every wait of these tests.
	setVar(t, &hostspawn.ReadyWait, step)
	setVar(t, &createDialWait, step)
	s := &spawned{}
	prev := HostCommand
	HostCommand = func(argv []string) (*exec.Cmd, error) {
		b, err := json.Marshal(argv)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		n := len(s.cmds)
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(os.Environ(), "HOME="+env.Home, "CRAZE_HOME="+env.CrazeDir, "CRAZE_RUNTIME_DIR="+env.CrazeRuntimeDir,
			serveTestChild+"="+mode(n), serveTestArgv+"="+string(b), hubTestParent+"="+strconv.Itoa(os.Getpid()))
		cmd.Stderr = os.Stderr
		s.cmds = append(s.cmds, cmd)
		s.argv = append(s.argv, slices.Clone(argv))
		s.mu.Unlock()
		return cmd, nil
	}
	t.Cleanup(func() {
		HostCommand = prev
		s.mu.Lock()
		cmds := slices.Clone(s.cmds)
		s.mu.Unlock()
		for _, cmd := range cmds {
			if cmd.Process == nil {
				continue
			}
			_ = cmd.Process.Kill()
			deadline := time.Now().Add(step)
			for alive(cmd.Process.Pid) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
		}
	})
	return s
}

// creates is session.create's options for a test's hub: def is the
// configured default provider ("" for none), and the providers it knows are
// cursor and grok.
func creates(def string) *Creates {
	c, _ := mutableCreates(def)
	return c
}

// mutableCreates is creates whose configured default provider set changes
// for the creates that follow — a config file edited meanwhile.
func mutableCreates(def string) (*Creates, func(string)) {
	var mu sync.Mutex
	return &Creates{
		DefaultProvider: func() string {
			mu.Lock()
			defer mu.Unlock()
			return def
		},
		KnownProvider: func(p string) bool { return p == "cursor" || p == "grok" },
	}, func(v string) {
		mu.Lock()
		defer mu.Unlock()
		def = v
	}
}

// creating is a hub that creates, over env, its schedule hk's (nil:
// production's), its created hosts children in mode(n): the hub, serving,
// and its socket.
func creating(t *testing.T, env rundir.Env, hk *hooks, mode func(n int) string) (*running, *spawned, string) {
	t.Helper()
	s := hostsAsChildren(t, env, mode)
	rn := runWith(t, env, hk, creates("cursor"))
	sock := rn.line(t).Socket
	rn.serving(t)
	return rn, s, sock
}

// always is a mode for every spawn.
func always(mode string) func(int) string { return func(int) string { return mode } }

// gateDir is a fresh directory for a gated start (mode "gate:<dir>").
func gateDir(t *testing.T) string {
	t.Helper()
	return shortDir(t, "czg")
}

// waitGate waits for a gated host to be waiting at its gate: its host id.
func waitGate(t *testing.T, dir string) string {
	t.Helper()
	var id []byte
	waitFor(t, "a gated start waiting", func() bool {
		b, err := os.ReadFile(filepath.Join(dir, "waiting"))
		id = b
		return err == nil && len(b) > 0
	})
	return string(id)
}

// openGate lets a gated start go on: "ok" starts it, anything else fails it.
func openGate(t *testing.T, dir, how string) {
	t.Helper()
	tmp := filepath.Join(dir, ".go")
	if err := os.WriteFile(tmp, []byte(how), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "go")); err != nil {
		t.Fatal(err)
	}
}

// createOn sends session.create on c with params and reads its answer.
func createOn(t *testing.T, c *client, params any) protocol.Response {
	t.Helper()
	return c.call(t, protocol.MethodSessionCreate, params)
}

// sendCreate writes a session.create on c and does not read its answer:
// readAnswer does, later.
func (c *client) sendCreate(t *testing.T, params any) {
	t.Helper()
	c.id++
	req := map[string]any{"jsonrpc": "2.0", "id": c.id, "method": protocol.MethodSessionCreate, "params": params}
	_ = c.nc.SetWriteDeadline(time.Now().Add(step))
	if err := protocol.WriteLine(c.nc, req); err != nil {
		t.Fatalf("session.create: write: %v", err)
	}
	_ = c.nc.SetWriteDeadline(time.Time{})
}

// readAnswer reads the next line on c as a response, within step.
func (c *client) readAnswer(t *testing.T) protocol.Response {
	t.Helper()
	_ = c.nc.SetReadDeadline(time.Now().Add(step))
	defer func() { _ = c.nc.SetReadDeadline(time.Time{}) }()
	line, err := c.lr.ReadLine()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp protocol.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	return resp
}

// result is a create's answer, which must be a result.
func result(t *testing.T, resp protocol.Response) protocol.CreateResult {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("session.create refused: %s (%s/%s, cause %q)", resp.Error.Message, resp.Error.Data.Code, resp.Error.Data.Reason, resp.Error.Data.Cause)
	}
	var res protocol.CreateResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// refusedAs checks a create's answer is a refusal of code and reason.
func refusedAs(t *testing.T, resp protocol.Response, code protocol.Code, reason protocol.Reason) *protocol.Error {
	t.Helper()
	if resp.Error == nil || resp.Error.Data.Code != code || resp.Error.Data.Reason != reason {
		t.Fatalf("session.create answered %+v (%s), want %s/%s", resp.Error, resp.Result, code, reason)
	}
	return resp.Error
}

// listedEntry is host id's live registry entry in env, and whether it is listed.
func listedEntry(t *testing.T, env rundir.Env, id string) (rundir.Entry, bool) {
	t.Helper()
	entries, err := rundir.Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.HostID == id {
			return e, true
		}
	}
	return rundir.Entry{}, false
}

// gone waits for the i-th spawned host to have exited and left the registry.
func (s *spawned) gone(t *testing.T, env rundir.Env, i int) {
	t.Helper()
	cmd, argv := s.cmd(i)
	id := fakehost.ParseSpawnArgs(argv).HostID
	waitFor(t, "host "+id+" gone", func() bool {
		_, listed := listedEntry(t, env, id)
		return !listed && cmd.Process != nil && !alive(cmd.Process.Pid)
	})
}

// writeFile writes an empty file at path.
func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}
