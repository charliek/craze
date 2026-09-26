package remote_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
)

// The client's half of the peer check (plan 027 §3.8): Options.PeerCheck —
// internal/rundir's DialCheck — runs on the dialed connection before a byte
// is written. These tests dial a real Unix socket with no tap in between
// (the tap's connection is not a *net.UnixConn, which a check refuses).

// peerClient is who these tests say they are.
var peerClient = protocol.ClientInfo{Kind: "test", Name: "remote_test"}

// TestARefusedPeerIsNeverWrittenTo: a client whose check names the server as
// another user — rundir's rule over an injected lookup — fails Dial with a
// peer-check error carrying rundir.ErrPeerUID, having written nothing: the
// listener's end reads EOF with no byte.
func TestARefusedPeerIsNeverWrittenTo(t *testing.T) {
	l, err := net.Listen("unix", filepath.Join(shortDir(t), "s"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	type read struct {
		got []byte
		err error
	}
	reads := make(chan read, 1)
	go func() {
		nc, err := l.Accept()
		if err != nil {
			reads <- read{err: err}
			return
		}
		defer nc.Close()
		// One read: EOF with nothing when the client wrote nothing, and its
		// first bytes — a hello — when it did. The close that follows fails a
		// client that is still waiting for an answer at once.
		_ = nc.SetReadDeadline(time.Now().Add(watchdog))
		buf := make([]byte, 4096)
		n, err := nc.Read(buf)
		reads <- read{got: buf[:n], err: err}
	}()

	other := os.Geteuid() + 1
	lookup := func(*net.UnixConn) (int, int, error) { return 4242, other, nil }
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	c, err := remote.Dial(ctx, l.Addr().String(), remote.Options{Client: peerClient, PeerCheck: rundir.DialCheckWith(os.Geteuid(), lookup)})
	if err == nil {
		_ = c.Close()
		t.Error("dialed a host that runs as another user")
	} else if !errors.Is(err, rundir.ErrPeerUID) || !strings.Contains(err.Error(), "peer check") {
		t.Errorf("dial: %v, want a peer-check error carrying rundir.ErrPeerUID", err)
	}
	select {
	case r := <-reads:
		if len(r.got) != 0 || !errors.Is(r.err, io.EOF) {
			t.Fatalf("the host's end read %q, %v; want EOF with nothing written", r.got, r.err)
		}
	case <-time.After(watchdog):
		t.Fatal("the host's end never finished its read")
	}
}

// TestTheHostsOwnUserPassesTheDialCheck: with the real check,
// rundir.DialCheck(euid), a host running as this user — this process — is
// dialed and says hello.
func TestTheHostsOwnUserPassesTheDialCheck(t *testing.T) {
	h := newHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	c, err := remote.Dial(ctx, h.path, remote.Options{Client: peerClient, PeerCheck: rundir.DialCheck(os.Geteuid())})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if c.Hello().ClientID == "" {
		t.Fatalf("hello answered no client id: %+v", c.Hello())
	}
}
