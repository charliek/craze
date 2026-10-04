package fakehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/transcript"
)

// The two-socket fixtures' hub (plan 032 §3.15; fixtures 19–22, and plan
// 034's 24 and 25): the real hub, internal/hub's Run, in this process over
// the fixture's own registry — the production hub but for its idle grace,
// which is long enough never to end a fixture. Its id, version and this process's pid are named by
// placeholder in what it writes (fixtureRunner.hubToWire). A fixture whose
// host line says hubCreates (22) has a hub that creates sessions, each host
// it spawns this test binary run as a fake craze serve (spawnedChild, below).

func init() { fixtureHub = startFixtureHub }

// startFixtureHub runs a hub over env and answers its socket and its id, from
// its ready line; it is stopped — and its Run waited for — when the test ends.
func startFixtureHub(t *testing.T, env rundir.Env, creates bool) (string, string) {
	t.Helper()
	o := hub.Options{
		Env:       env,
		Codecs:    protocol.Codecs{Event: agent.EventCodecVersion, Snapshot: transcript.SnapshotVersion},
		IdleGrace: time.Hour,
	}
	if creates {
		spawnAsFakeHosts(t, env)
		o.Creates = &hub.Creates{
			DefaultProvider: func() string { return "" },
			KnownProvider: func(id string) bool {
				_, err := agent.ProviderByName(id)
				return err == nil
			},
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	o.Ready = hub.NewReadyPipe(w)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hub.Run(ctx, o) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(fixtureTimeout):
			t.Errorf("the fixture's hub did not stop within %v", fixtureTimeout)
		}
		_ = r.Close()
	})
	raw := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r) // one line, then the pipe closed
		raw <- b
	}()
	var line hub.ReadyLine
	select {
	case b := <-raw:
		if err := json.Unmarshal(b, &line); err != nil || !line.OK {
			t.Fatalf("the fixture's hub did not come up: %q (%v)", b, err)
		}
	case <-time.After(fixtureTimeout):
		t.Fatalf("the fixture's hub wrote no ready line within %v", fixtureTimeout)
	}
	return line.Socket, line.HubID
}

// spawnedChild runs this test binary as a fake craze serve, its command line
// the variable's value (JSON): fakehost.RunSpawned, in the environment's
// registry, its session "session-created" — the one a fixture names — whose
// first prompt's turn is held open (HangNext), so the row a create reads
// after it says working whatever the scheduler does, and whose start fails
// when the model asked for is spawnedStartFails. Its parent's pid is in
// spawnedParent: the child ends itself once that process is not its parent.
const (
	spawnedChild  = "CRAZE_FAKEHOST_TEST_SPAWNED"
	spawnedParent = "CRAZE_FAKEHOST_TEST_PARENT"
	// spawnedStartFails is the model whose session's start fails, with
	// spawnedFailure.
	spawnedStartFails = "start-fails"
	spawnedFailure    = "the agent could not start: no such binary"
)

func init() {
	raw, ok := os.LookupEnv(spawnedChild)
	if !ok {
		return
	}
	_ = os.Unsetenv(spawnedChild)
	if n, err := strconv.Atoi(os.Getenv(spawnedParent)); err == nil && n > 1 {
		go func() {
			for range time.Tick(100 * time.Millisecond) {
				if os.Getppid() != n {
					os.Exit(98)
				}
			}
		}()
	}
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil {
		fmt.Fprintln(os.Stderr, "fake craze serve:", err)
		os.Exit(97)
	}
	a := ParseSpawnArgs(argv)
	o := Options{CrazeSessionID: "session-created", Stop: true}
	if a.Model == spawnedStartFails {
		o.Start = func(context.Context) error { return errors.New(spawnedFailure) }
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	if err := RunSpawned(a, rundir.ProcessEnv(), o, func(h *Host) { h.HangNext() }, sigs); err != nil {
		fmt.Fprintln(os.Stderr, "fake craze serve:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// spawnAsFakeHosts makes every host a hub spawns for one test this test
// binary run as a fake craze serve (spawnedChild) in env's registry, and ends
// each one still running when the test ends.
func spawnAsFakeHosts(t *testing.T, env rundir.Env) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu   sync.Mutex
		cmds []*exec.Cmd
	)
	prev := hub.HostCommand
	hub.HostCommand = func(argv []string) (*exec.Cmd, error) {
		b, err := json.Marshal(argv)
		if err != nil {
			return nil, err
		}
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(os.Environ(), "HOME="+env.Home, "CRAZE_HOME="+env.CrazeDir, "CRAZE_RUNTIME_DIR="+env.CrazeRuntimeDir,
			spawnedChild+"="+string(b), spawnedParent+"="+strconv.Itoa(os.Getpid()))
		cmd.Stderr = os.Stderr
		mu.Lock()
		cmds = append(cmds, cmd)
		mu.Unlock()
		return cmd, nil
	}
	t.Cleanup(func() {
		hub.HostCommand = prev
		mu.Lock()
		defer mu.Unlock()
		for _, cmd := range cmds {
			if cmd.Process == nil {
				continue
			}
			_ = cmd.Process.Kill()
			for deadline := time.Now().Add(fixtureTimeout); syscall.Kill(cmd.Process.Pid, 0) == nil && time.Now().Before(deadline); {
				time.Sleep(5 * time.Millisecond)
			}
		}
	})
}
