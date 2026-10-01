// craze-fake-host is the standalone twin of internal/fakehost (plan 027
// §3.11): it serves the real control socket wire, deterministically, in
// front of a Stub instead of a real agent. It is a thin main over
// internal/fakehost — everything that matters lives there.
//
// Usage:
//
//	craze-fake-host --socket PATH [--host-id ID] [--session-id ID]
//	craze-fake-host --registry ROOT [--host-id ID] [--session-id ID]
//
// --socket serves on a socket at PATH, listed nowhere. --registry instead
// binds and lists the host exactly as a craze host is (plan 032 §3.15): its
// socket in the runtime tree (CRAZE_RUNTIME_DIR, else the usual bases), its
// entry and lifetime lock in ROOT/.cache/craze/hosts/ — ROOT standing for
// HOME — so a hub, `craze attach` or the TUI's list given that HOME finds it,
// and unlisted when it exits. Its namespace is CRAZE_HOME's, else
// ROOT/.craze's. --host-id (12 lowercase hex digits) and --session-id (the
// durable craze session id) replace the fixed defaults, so several fake hosts
// can run, and be listed, side by side.
//
// It prints one ready line to stdout, {"socket", "sessionId", "hostId"}, then
// reads NDJSON ops from stdin, one per line ({"name": "...", ...} —
// internal/fakehost's Host.Do), and exits once stdin reaches EOF or an op
// named "quit" runs.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/rundir"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// errUsage is a command line that cannot run: exit 2.
var errUsage = errors.New("usage")

// config is the command line, checked.
type config struct {
	socket, registry string
	opts             fakehost.Options
}

// parse reads the command line: exactly one of --socket and --registry, and
// ids a host can be bound and listed under.
func parse(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("craze-fake-host", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", "", "unix socket path to serve, listed nowhere")
	registry := fs.String("registry", "", "a HOME-like root whose registry (.cache/craze/hosts) lists the host, its socket bound in the runtime tree")
	hostID := fs.String("host-id", "", "the host id: 12 lowercase hex digits (default the fixed one)")
	sessionID := fs.String("session-id", "", "the durable craze session id (default the fixed one)")
	if err := fs.Parse(args); err != nil {
		return config{}, errUsage
	}
	switch {
	case fs.NArg() > 0:
		return config{}, fmt.Errorf("%w: unexpected arguments %q", errUsage, fs.Args())
	case (*socket == "") == (*registry == ""):
		return config{}, fmt.Errorf("%w: exactly one of --socket and --registry is required", errUsage)
	case *hostID != "" && !rundir.ValidHostID(*hostID):
		return config{}, fmt.Errorf("%w: --host-id %q is not 12 lowercase hex digits", errUsage, *hostID)
	case *sessionID != "" && !rundir.ValidToken(*sessionID):
		return config{}, fmt.Errorf("%w: --session-id %q is not a token ([A-Za-z0-9._-], at most 128)", errUsage, *sessionID)
	}
	return config{socket: *socket, registry: *registry, opts: fakehost.Options{HostID: *hostID, CrazeSessionID: *sessionID}}, nil
}

// run is the command: its exit status.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cfg, err := parse(args, stderr)
	if err != nil {
		// A flag the flag package refused has been reported already, with
		// the usage; every other refusal is said here.
		if err != errUsage {
			fmt.Fprintln(stderr, "craze-fake-host:", err)
		}
		return 2
	}
	if err := serve(cfg, stdin, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "craze-fake-host:", err)
		return 1
	}
	return 0
}

// registryEnv is the Env --registry root binds and lists the host in: the
// process's, with root as its home, and the craze directory CRAZE_HOME's or,
// unset, root's own .craze.
func registryEnv(root string) (rundir.Env, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return rundir.Env{}, err
	}
	env := rundir.ProcessEnv()
	env.Home = abs
	if strings.TrimSpace(os.Getenv("CRAZE_HOME")) == "" {
		env.CrazeDir = filepath.Join(abs, ".craze")
	}
	return env, nil
}

func serve(cfg config, stdin io.Reader, stdout, stderr io.Writer) error {
	h, err := fakehost.New(cfg.opts)
	if err != nil {
		return err
	}
	var (
		l      net.Listener
		reg    *rundir.Host
		socket = cfg.socket
	)
	if cfg.registry != "" {
		env, err := registryEnv(cfg.registry)
		if err == nil {
			reg, err = h.Register(env)
		}
		if err != nil {
			_ = h.Close(context.Background())
			return err
		}
		l, socket = reg.Listener(), reg.Socket()
	} else if l, err = net.Listen("unix", socket); err != nil {
		_ = h.Close(context.Background())
		return err
	}
	served := make(chan error, 1)
	go func() { served <- h.Serve(l) }()
	err = loop(h, socket, stdin, stdout, stderr)
	serr := <-served
	if reg != nil {
		// Unlisted last, once nothing is served.
		if cerr := reg.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	if err == nil {
		err = serr
	}
	return err
}

// loop prints the ready line, then runs stdin's ops until EOF or quit, and
// closes the host.
func loop(h *fakehost.Host, socket string, stdin io.Reader, stdout, stderr io.Writer) error {
	ready := struct {
		Socket    string `json:"socket"`
		SessionID string `json:"sessionId"`
		HostID    string `json:"hostId"`
	}{Socket: socket, SessionID: h.SessionID(), HostID: h.HostID()}
	line, err := json.Marshal(ready)
	if err != nil {
		_ = h.Close(context.Background())
		return err
	}
	if _, err := fmt.Fprintln(stdout, string(line)); err != nil {
		_ = h.Close(context.Background())
		return err
	}

	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	quit := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			fmt.Fprintln(stderr, "craze-fake-host: op:", err)
			continue
		}
		if err := h.Do(line); err != nil {
			fmt.Fprintln(stderr, "craze-fake-host: op:", err)
		}
		if probe.Name == "quit" {
			quit = true
			break
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(stderr, "craze-fake-host: stdin:", err)
	}
	if !quit {
		_ = h.Close(context.Background())
	}
	return nil
}
