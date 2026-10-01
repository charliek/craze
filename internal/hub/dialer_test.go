package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// hub.Dialer (plan 032 §3.8): a refused or absent hub socket, and a hub that
// closes a connection before its first reply, are Ensure's — bounded by the
// dial's deadline, or the read's, or DialerBudget — and a live hub is dialled
// as it is. A real hub child killed with SIGKILL is replaced (A8's respawn;
// the whole of A8, a spliced client resuming, is internal/cli's
// TestAHubKilledMidTurnLosesNothing).

// ensureCall is one Ensure a Dialer made: its deadline and what it needed.
type ensureCall struct {
	deadline time.Time
	need     protocol.ConnectionCapabilities
}

// ensureSeam replaces the Dialer's Ensure for one test: each call is noted,
// and answered sock — or, with sock "", an error.
type ensureSeam struct {
	mu    sync.Mutex
	calls []ensureCall
}

func installEnsure(t *testing.T, sock string) *ensureSeam {
	t.Helper()
	s := &ensureSeam{}
	setVar(t, &dialerEnsure, func(ctx context.Context, _ rundir.Env, need protocol.ConnectionCapabilities) (string, error) {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("the Dialer gave Ensure no deadline")
		}
		s.mu.Lock()
		s.calls = append(s.calls, ensureCall{deadline: dl, need: need})
		s.mu.Unlock()
		if sock == "" {
			return "", errors.New("no hub here")
		}
		return sock, nil
	})
	return s
}

func (s *ensureSeam) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *ensureSeam) call(i int) ensureCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[i]
}

// sayHello writes a hello on nc and answers the hub's result.
func sayHello(t *testing.T, nc net.Conn) protocol.HubHelloResult {
	t.Helper()
	if err := protocol.WriteLine(nc, map[string]any{"jsonrpc": "2.0", "id": 1, "method": protocol.MethodHello,
		"params": protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{Kind: "test"}}}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	return readHello(t, nc)
}

// readHello reads a hello's answer on nc, which must be a hub's.
func readHello(t *testing.T, nc net.Conn) protocol.HubHelloResult {
	t.Helper()
	line, err := protocol.NewLineReader(nc, 0).ReadLine()
	if err != nil {
		t.Fatalf("hello's answer: %v", err)
	}
	var resp struct {
		Result protocol.HubHelloResult `json:"result"`
		Error  *protocol.Error         `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil || resp.Error != nil || resp.Result.Endpoint.Kind != protocol.EndpointHub {
		t.Fatalf("hello's answer %s (%v)", line, err)
	}
	return resp.Result
}

// stepContext is a context bounded by step.
func stepContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), step)
	t.Cleanup(cancel)
	return ctx
}

// TestTheDialerRespawnsADeadHub (§3.8, A8's respawn): a hub child killed with
// SIGKILL leaves its socket, which refuses, and its record, whose pid is gone;
// the Dialer, dialling that socket, runs Ensure, which starts a hub in its
// place, and hands back a connection to it: its hello answers the new hub's
// id. The negative control: a plain dial of the same path is refused.
func TestTheDialerRespawnsADeadHub(t *testing.T) {
	env := processEnv(t)
	kids := asChildren(t, nil)
	sock, err := Ensure(stepContext(t), env, protocol.ConnectionCapabilities{Connect: true})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := rundir.ReadHubRecord(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitGone(t, first.PID)
	// Its leader a zombie, its other threads may still hold its files for an
	// instant: the listener is closed once the last of them has exited.
	waitFor(t, "the killed hub's socket refuses", func() bool {
		nc, err := net.Dial("unix", sock)
		if err == nil {
			_ = nc.Close()
		}
		return errors.Is(err, syscall.ECONNREFUSED)
	})

	nc, err := Dialer(env)(stepContext(t), sock)
	if err != nil {
		t.Fatalf("the Dialer: %v", err)
	}
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(step))
	res := sayHello(t, nc)
	second, _, err := rundir.ReadHubRecord(env)
	if err != nil {
		t.Fatal(err)
	}
	if res.Endpoint.HostID != second.HubID || second.HubID == first.HubID || second.PID == first.PID {
		t.Fatalf("the Dialer reached hub %s; the hubs are %s (killed) and %s (pid %d)", res.Endpoint.HostID, first.HubID, second.HubID, second.PID)
	}
	if n := kids.n.Load(); n != 2 {
		t.Fatalf("%d hubs spawned, want 2", n)
	}
}

// TestTheDialerBoundsItsEnsure (§3.8, X34): a socket that is absent, or that
// refuses, sends the Dialer to Ensure for a hub that can connect sessions,
// with a deadline always — the dial's own, or DialerBudget from the dial when
// the dial has none — and its failure is the dial's. The negative control: a
// live socket is dialled as it is, with no Ensure.
func TestTheDialerBoundsItsEnsure(t *testing.T) {
	env := testEnv(t)
	seam := installEnsure(t, "")
	dir := shortDir(t, "czb")
	absentSock := filepath.Join(dir, "absent.sock")

	before := time.Now()
	if _, err := Dialer(env)(context.Background(), absentSock); err == nil {
		t.Fatal("a dial to no hub, whose Ensure failed, succeeded")
	}
	c := seam.call(0)
	if c.deadline.Before(before.Add(DialerBudget)) || c.deadline.After(time.Now().Add(DialerBudget)) || !c.need.Connect {
		t.Fatalf("Ensure was given %+v, want a deadline %v from the dial and connect", c, DialerBudget)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	want, _ := ctx.Deadline()
	refusing := filepath.Join(dir, "refusing.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: refusing, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	_ = ln.Close()
	if _, err := Dialer(env)(ctx, refusing); err == nil {
		t.Fatal("a dial to a refusing socket, whose Ensure failed, succeeded")
	}
	if c := seam.call(1); !c.deadline.Equal(want) {
		t.Fatalf("Ensure's deadline is %v, want the dial's %v", c.deadline, want)
	}

	rn := runIn(t, testEnv(t), quiet())
	live := rn.line(t).Socket
	nc, err := Dialer(env)(stepContext(t), live)
	if err != nil {
		t.Fatalf("a live hub's socket: %v", err)
	}
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(step))
	sayHello(t, nc)
	if n := seam.n(); n != 2 {
		t.Fatalf("Ensure ran %d times, want 2: a live hub sent the Dialer to it", n)
	}
}

// closer is a listener at a socket of the test's that plays a hub that closes
// a connection unanswered: at once on accept (closed tells each), or once it
// has read a line (eof); or, answer set, after answering that line as a hub.
type closer struct {
	path   string
	closed chan struct{}
}

const (
	closeAtAccept = "accept"
	closeAtEOF    = "eof"
	closeAnswered = "answer"
)

func newCloser(t *testing.T, mode string) *closer {
	t.Helper()
	c := &closer{path: filepath.Join(shortDir(t, "czc"), "hub.sock"), closed: make(chan struct{}, 8)}
	ln, err := net.Listen("unix", c.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() {
					_ = nc.Close()
					c.closed <- struct{}{}
				}()
				if mode == closeAtAccept {
					return
				}
				lr := protocol.NewLineReader(nc, 0)
				line, err := lr.ReadLine()
				if err != nil || mode == closeAtEOF {
					return
				}
				var req protocol.Request
				_ = json.Unmarshal(line, &req)
				resp := protocol.Response{JSONRPC: "2.0", ID: req.ID}
				resp.Result, _ = json.Marshal(hubResult("00000000c105", "0.0.0-closer", protocol.HubCapabilities()))
				_ = protocol.WriteLine(nc, resp)
			}()
		}
	}()
	return c
}

// TestTheDialerRespawnsAHubThatClosedUnanswered (§3.8): a hub that closes a
// connection before the first byte of its first reply — its idle decision
// or its teardown having come first — is no hub. The connection the Dialer
// handed back runs Ensure on the EOF of its read, or the broken pipe of its
// write, bounded by the read deadline the client set, and goes on with the
// hub Ensure answers: what was written is written there again, and its
// answer read as if nothing had happened. The negative control: a hub that
// answered first is the hub — its EOF after that is the caller's, and no
// Ensure runs.
func TestTheDialerRespawnsAHubThatClosedUnanswered(t *testing.T) {
	rn := runIn(t, testEnv(t), quiet())
	hubSock := rn.line(t).Socket
	hubID := rn.serving(t).id
	env := testEnv(t)

	t.Run("its read meets EOF", func(t *testing.T) {
		seam := installEnsure(t, hubSock)
		cl := newCloser(t, closeAtEOF)
		nc, err := Dialer(env)(stepContext(t), cl.path)
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		readBy := time.Now().Add(step)
		_ = nc.SetDeadline(readBy)
		if res := sayHello(t, nc); res.Endpoint.HostID != hubID {
			t.Fatalf("answered by %s, want the respawned hub %s", res.Endpoint.HostID, hubID)
		}
		if seam.n() != 1 || !seam.call(0).deadline.Equal(readBy) {
			t.Fatalf("Ensure ran %d times (%+v), want once, bounded by the read's deadline %v", seam.n(), seam.calls, readBy)
		}
	})
	t.Run("its write meets a broken pipe", func(t *testing.T) {
		seam := installEnsure(t, hubSock)
		cl := newCloser(t, closeAtAccept)
		nc, err := Dialer(env)(stepContext(t), cl.path)
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		select {
		case <-cl.closed:
		case <-time.After(step):
			t.Fatal("the closer did not close")
		}
		_ = nc.SetDeadline(time.Now().Add(step))
		if res := sayHello(t, nc); res.Endpoint.HostID != hubID {
			t.Fatalf("answered by %s, want the respawned hub %s", res.Endpoint.HostID, hubID)
		}
		if seam.n() != 1 {
			t.Fatalf("Ensure ran %d times, want once", seam.n())
		}
	})
	t.Run("a hub that answered first", func(t *testing.T) {
		seam := installEnsure(t, hubSock)
		cl := newCloser(t, closeAnswered)
		nc, err := Dialer(env)(stepContext(t), cl.path)
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		_ = nc.SetDeadline(time.Now().Add(step))
		if res := sayHello(t, nc); res.Endpoint.HostID != "00000000c105" {
			t.Fatalf("answered by %s, want the hub that answered", res.Endpoint.HostID)
		}
		if _, err := nc.Read(make([]byte, 1)); err == nil {
			t.Fatal("a read after the hub's close read something")
		}
		if seam.n() != 0 {
			t.Fatalf("Ensure ran %d times after a hub answered", seam.n())
		}
	})
}
