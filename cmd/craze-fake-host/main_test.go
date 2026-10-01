package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// wait bounds every wait on a fake host the test runs: generous, and only
// ever hit when something is wedged.
const wait = 10 * time.Second

// shortDir is a fresh 0700 directory under /tmp itself (sun_path on macOS).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "czfc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// lockedBuffer is a stderr the command and the test share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// fakeHost is one run of the command, in this process.
type fakeHost struct {
	ready struct {
		Socket    string `json:"socket"`
		SessionID string `json:"sessionId"`
		HostID    string `json:"hostId"`
	}
	stdin  *io.PipeWriter
	exit   chan int
	stderr *lockedBuffer
}

// start runs the command with args and reads its ready line; it fails the
// test if the command exits first.
func start(t *testing.T, args ...string) *fakeHost {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	f := &fakeHost{stdin: inW, exit: make(chan int, 1), stderr: &lockedBuffer{}}
	go func() {
		code := run(args, inR, outW, f.stderr)
		_ = outW.Close()
		f.exit <- code
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case <-f.exit:
		case <-time.After(wait):
			t.Errorf("craze-fake-host %v never exited", args)
		}
	})
	line, err := bufio.NewReader(outR).ReadString('\n')
	if err != nil {
		t.Fatalf("craze-fake-host %v: no ready line (%v); stderr: %s", args, err, f.stderr.String())
	}
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	if err := json.Unmarshal([]byte(line), &f.ready); err != nil {
		t.Fatalf("ready line %q: %v", line, err)
	}
	return f
}

// exited is the command's exit status, once it has exited.
func (f *fakeHost) exited(t *testing.T) int {
	t.Helper()
	select {
	case code := <-f.exit:
		f.exit <- code
		return code
	case <-time.After(wait):
		t.Fatal("the fake host never exited")
		return -1
	}
}

// hello says hello on socket and answers the endpoint's host id.
func hello(t *testing.T, socket string) string {
	t.Helper()
	nc, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nc.Close() }()
	_ = nc.SetDeadline(time.Now().Add(wait))
	if err := protocol.WriteLine(nc, protocol.Request{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(`1`),
		Method: protocol.MethodHello, Params: json.RawMessage(`{"protocols":[1],"client":{"kind":"test"}}`)}); err != nil {
		t.Fatal(err)
	}
	line, err := protocol.NewLineReader(nc, protocol.OutboundLineMax).ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result protocol.HelloResult `json:"result"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	return resp.Result.Endpoint.HostID
}

// listed is every live host in root's registry, by host id.
func listed(t *testing.T, root string) map[string]rundir.Entry {
	t.Helper()
	env, err := registryEnv(root)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := rundir.Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]rundir.Entry{}
	for _, e := range entries {
		out[e.HostID] = e
	}
	return out
}

// TestRegisteredFakeHostsRunSideBySide (plan 032 §3.15): with --registry, a
// fake host is bound and listed in the root's registry as a craze host is,
// under the ids --host-id and --session-id give it, so two run at once and
// each is found, and answers, as itself; each is unlisted when it exits —
// by its quit op, or by its stdin's end. The negative control: a third under
// an id one of them holds cannot be listed, and exits 1.
func TestRegisteredFakeHostsRunSideBySide(t *testing.T) {
	t.Setenv("CRAZE_RUNTIME_DIR", filepath.Join(shortDir(t), "run"))
	t.Setenv("CRAZE_HOME", "")
	root := shortDir(t)
	a := start(t, "--registry", root, "--host-id", "00000000000a", "--session-id", "session-a")
	b := start(t, "--registry", root, "--host-id", "00000000000b", "--session-id", "session-b")
	if a.ready.HostID != "00000000000a" || a.ready.SessionID != "session-a" || b.ready.HostID != "00000000000b" || b.ready.SessionID != "session-b" {
		t.Fatalf("ready lines %+v and %+v", a.ready, b.ready)
	}
	hosts := listed(t, root)
	if len(hosts) != 2 || hosts["00000000000a"].CrazeSessionID != "session-a" || hosts["00000000000b"].CrazeSessionID != "session-b" ||
		hosts["00000000000a"].Socket != a.ready.Socket || hosts["00000000000b"].Socket != b.ready.Socket || !hosts["00000000000a"].Ready {
		t.Fatalf("the registry lists %+v", hosts)
	}
	if got := hello(t, hosts["00000000000a"].Socket); got != "00000000000a" {
		t.Fatalf("host a's listed socket is answered by %q", got)
	}
	if got := hello(t, hosts["00000000000b"].Socket); got != "00000000000b" {
		t.Fatalf("host b's listed socket is answered by %q", got)
	}

	// Negative control: an id already held.
	inR, inW := io.Pipe()
	defer func() { _ = inW.Close() }()
	var stderr lockedBuffer
	if code := run([]string{"--registry", root, "--host-id", "00000000000b"}, inR, io.Discard, &stderr); code != 1 {
		t.Fatalf("a second host under b's id exited %d (stderr %q), want 1", code, stderr.String())
	}

	if _, err := io.WriteString(a.stdin, `{"name":"quit"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if code := a.exited(t); code != 0 {
		t.Fatalf("host a exited %d on quit; stderr: %s", code, a.stderr.String())
	}
	if hosts := listed(t, root); len(hosts) != 1 || hosts["00000000000b"].HostID == "" {
		t.Fatalf("after host a quit the registry lists %+v", hosts)
	}
	_ = b.stdin.Close()
	if code := b.exited(t); code != 0 {
		t.Fatalf("host b exited %d at its stdin's end; stderr: %s", code, b.stderr.String())
	}
	if hosts := listed(t, root); len(hosts) != 0 {
		t.Fatalf("after both exited the registry lists %+v", hosts)
	}
}

// TestTheCommandLineIsChecked: --socket serves at its path, listed nowhere,
// under the ids given; and a command line that cannot run exits 2 saying
// why — neither of --socket and --registry, both, a host id that is not one,
// a session id no file could be named for, a stray argument, an unknown flag.
func TestTheCommandLineIsChecked(t *testing.T) {
	socket := filepath.Join(shortDir(t), "s")
	f := start(t, "--socket", socket, "--host-id", "0000000000cc", "--session-id", "session-c")
	if f.ready.Socket != socket || f.ready.HostID != "0000000000cc" || f.ready.SessionID != "session-c" {
		t.Fatalf("ready line %+v", f.ready)
	}
	if got := hello(t, socket); got != "0000000000cc" {
		t.Fatalf("the socket is answered by %q", got)
	}
	_ = f.stdin.Close()
	if code := f.exited(t); code != 0 {
		t.Fatalf("exited %d; stderr: %s", code, f.stderr.String())
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"neither", nil, "exactly one of --socket and --registry"},
		{"both", []string{"--socket", "/tmp/x", "--registry", "/tmp/y"}, "exactly one of --socket and --registry"},
		{"a host id that is not one", []string{"--socket", "/tmp/x", "--host-id", "ABCDEF012345"}, "not 12 lowercase hex digits"},
		{"a session id no file could be named for", []string{"--socket", "/tmp/x", "--session-id", "a/b"}, "is not a token"},
		{"a stray argument", []string{"--socket", "/tmp/x", "extra"}, "unexpected arguments"},
		{"an unknown flag", []string{"--sockets", "/tmp/x"}, "flag provided but not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr lockedBuffer
			if code := run(tc.args, strings.NewReader(""), io.Discard, &stderr); code != 2 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, stderr %q; want 2 and %q", code, stderr.String(), tc.want)
			}
		})
	}
}
