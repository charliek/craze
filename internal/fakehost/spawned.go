package fakehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/rundir"
)

// A fake host as a spawner starts a craze serve (plan 032 §3.10): what the
// hub's session.create tests and wire fixture 22 have the hub spawn in place
// of craze serve — the test binary re-executed, running RunSpawned — so a
// create spawns a real process, reads a real ready line and dials a real
// socket, and the fake host's Options decide how its session starts.

// SpawnArgs is what RunSpawned takes of craze serve's command line, as
// internal/hostspawn.Args writes it (--flag=value): the host's id, the
// session's directory, provider, model and effort, and the create's request.
type SpawnArgs struct {
	HostID, Workspace, Provider, Model, Effort string
	RequestID, RequestHash                     string
}

// ParseSpawnArgs reads argv — craze serve's command line, its first word
// "serve" — for SpawnArgs; every other flag is ignored.
func ParseSpawnArgs(argv []string) SpawnArgs {
	var a SpawnArgs
	for _, arg := range argv {
		k, v, ok := strings.Cut(arg, "=")
		if !ok {
			continue
		}
		switch k {
		case "--host-id":
			a.HostID = v
		case "--workspace":
			a.Workspace = v
		case "--provider":
			a.Provider = v
		case "--model":
			a.Model = v
		case "--effort":
			a.Effort = v
		case "--request-id":
			a.RequestID = v
		case "--request-hash":
			a.RequestHash = v
		}
	}
	return a
}

// spawnedVersion is the craze version a spawned fake host's ready line says.
const spawnedVersion = "0.0.0-fakehost"

// RunSpawned runs a fake host for a's command line in this process, as
// craze serve would run for it: o with a's host id, workspace and request
// (its session id "session-<hostId>" when o names none), prepared (prepare,
// when set, before anything can reach it), registered in env and served; its
// ready line — ok, its host id, socket, session and craze version — written
// on the spawner's ready pipe (CRAZE_READY_FD) once it is listed; then served
// until its stop is heard (Options.Stop), or SIGTERM or SIGINT arrives on
// sigs, when it unlists and closes. An error is a host that never served,
// whose ready line said why.
func RunSpawned(a SpawnArgs, env rundir.Env, o Options, prepare func(*Host), sigs <-chan os.Signal) error {
	ready, err := takeReady()
	if err != nil {
		return err
	}
	o.HostID, o.Workspace, o.RequestID, o.RequestHash = a.HostID, a.Workspace, a.RequestID, a.RequestHash
	if o.CrazeSessionID == "" {
		o.CrazeSessionID = "session-" + a.HostID
	}
	fail := func(err error) error {
		writeReady(ready, hostspawn.ReadyLine{Error: "craze serve: " + err.Error()})
		return err
	}
	h, err := New(o)
	if err != nil {
		return fail(err)
	}
	if prepare != nil {
		prepare(h)
	}
	reg, err := h.Register(env)
	if err != nil {
		_ = h.Close(context.Background())
		return fail(err)
	}
	served := make(chan error, 1)
	go func() { served <- h.Serve(reg.Listener()) }()
	writeReady(ready, hostspawn.ReadyLine{OK: true, HostID: o.HostID, Socket: reg.Socket(),
		CrazeSessionID: o.CrazeSessionID, CrazeVersion: spawnedVersion})
	for done := false; !done; {
		select {
		case <-h.StopHeard():
			done = true
		case sig := <-sigs:
			done = sig == syscall.SIGTERM || sig == syscall.SIGINT
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.Close(ctx)
	<-served
	return reg.Close()
}

// takeReady is the spawner's ready pipe, CRAZE_READY_FD's descriptor.
func takeReady() (*os.File, error) {
	raw := os.Getenv(hostspawn.ReadyFDEnv)
	_ = os.Unsetenv(hostspawn.ReadyFDEnv)
	_ = os.Unsetenv(hostspawn.HostChildEnv)
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, errors.New("fakehost: no ready pipe (CRAZE_READY_FD)")
	}
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "ready"), nil
}

// writeReady writes line on f and closes it.
func writeReady(f *os.File, line hostspawn.ReadyLine) {
	b, err := json.Marshal(line)
	if err == nil {
		_, _ = fmt.Fprintf(f, "%s\n", b)
	}
	_ = f.Close()
}
