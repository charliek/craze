package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// session.connect and the splice (plan 032 §3.7, §3.18, A11): the exclusive
// handoff's refusals, the lookup by each id a session has, an unreachable
// host, the bytes a client pipelined past session.connect, half-close each
// way, a host's EOF, bounded backpressure, a write's stall bound (no
// progress, never a slow write), the client a splice stays, and the
// teardown's close. The hosts are fake hosts listed in a registry of the
// test's own (hostIn), or a host of the test's own whose socket it serves
// byte by byte (rawHost); the hub runs in process with a quiet schedule. Each
// lifecycle test here is also run under a 5% CPU quota.

// spliceRig is a hub in process over env, its schedule quiet, every splice it
// hands a connection to sent on handed.
type spliceRig struct {
	rn     *running
	h      *hub
	sock   string
	handed chan *splice
}

// newSpliceRig runs the rig's hub; hk, when set, adjusts its hooks — a
// handedOff of its own calls the rig's after it (prev).
func newSpliceRig(t *testing.T, env rundir.Env, hk func(k *hooks)) *spliceRig {
	t.Helper()
	rg := &spliceRig{handed: make(chan *splice, 16)}
	k := quiet()
	k.handedOff = func(sp *splice) { rg.handed <- sp }
	if hk != nil {
		hk(k)
	}
	rg.rn = runIn(t, env, k)
	rg.sock = rg.rn.line(t).Socket
	rg.h = rg.rn.serving(t)
	return rg
}

// splice is the next splice the hub handed a connection to, within step.
func (rg *spliceRig) splice(t *testing.T) *splice {
	t.Helper()
	select {
	case sp := <-rg.handed:
		return sp
	case <-time.After(step):
		t.Fatalf("the hub handed no connection to a splice within %v", step)
		return nil
	}
}

// hostHello is a client's hello to a host, through a splice.
var hostHello = protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{Kind: "test", Name: "through the hub"}}

// connected is a peer spliced to sessionID's host through the hub at sock:
// hello to the hub, session.connect answered {}, then hello to the host,
// whose answer it returns.
func connected(t *testing.T, sock, sessionID string) (*peer, protocol.HelloResult) {
	t.Helper()
	p := dialPeer(t, sock)
	if m := p.call(protocol.MethodSessionConnect, protocol.ConnectParams{SessionID: sessionID}); m.Error != nil || string(m.Result) != "{}" {
		t.Fatalf("session.connect %s: %s", sessionID, clip(m.raw))
	}
	return p, p.hostHello()
}

// hostHello says hello to the host the peer is spliced to and answers its
// result.
func (p *peer) hostHello() protocol.HelloResult {
	p.t.Helper()
	m := p.call(protocol.MethodHello, hostHello)
	if m.Error != nil {
		p.t.Fatalf("the host's hello: %+v", m.Error)
	}
	var hr protocol.HelloResult
	if err := json.Unmarshal(m.Result, &hr); err != nil {
		p.t.Fatal(err)
	}
	if hr.Endpoint.Kind != protocol.EndpointHost || hr.ClientID == "" {
		p.t.Fatalf("the hello through the splice was answered by %+v", hr.Endpoint)
	}
	return hr
}

// refusal checks m is a refusal with code and reason.
func refusal(t *testing.T, m msg, code protocol.Code, reason protocol.Reason) {
	t.Helper()
	if m.Error == nil || m.Error.Data.Code != code || m.Error.Data.Reason != reason {
		t.Fatalf("answered %s, want %s/%s", clip(m.raw), code, reason)
	}
}

// entryOf is host id's entry in env's registry.
func entryOf(t *testing.T, env rundir.Env, id string) rundir.Entry {
	t.Helper()
	entries, err := rundir.Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.HostID == id {
			return e
		}
	}
	t.Fatalf("host %s is not in the registry: %+v", id, entries)
	return rundir.Entry{}
}

// rawHost is a session host of the test's own: listed in env's registry
// (rundir.Bind, its lock held) with its socket served by the test, every
// connection it accepts handed to the test (accept).
type rawHost struct {
	conns chan *net.UnixConn
}

func newRawHost(t *testing.T, env rundir.Env, id, session string) *rawHost {
	t.Helper()
	reg, err := rundir.Bind(env, id, rundir.Entry{CrazeSessionID: session, Ready: true, Workspace: "/work/" + id,
		StartedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	rh := &rawHost{conns: make(chan *net.UnixConn, 8)}
	ln := reg.Listener()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			uc, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = uc.Close() })
			rh.conns <- uc
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		_ = reg.Close()
	})
	return rh
}

// accept is the next connection the host accepted, within step.
func (rh *rawHost) accept(t *testing.T) *net.UnixConn {
	t.Helper()
	select {
	case uc := <-rh.conns:
		return uc
	case <-time.After(step):
		t.Fatalf("the host accepted no connection within %v", step)
		return nil
	}
}

// TestConnectFindsTheSessionByEachOfItsIds (§3.7's lookup, craze bridge's
// rule): a session is found by its craze session id, its provider session id
// and its host id; each connect answers {}, and from then the client speaks
// to that host itself — its hello answered by the host, which mints the
// client id, and an attach answered with the host's session. The negative
// control is TestConnectRefusals' unknown and ambiguous ids.
func TestConnectFindsTheSessionByEachOfItsIds(t *testing.T) {
	env := testEnv(t)
	hostIn(t, env, 1)
	e := entryOf(t, env, hostOf(1))
	rg := newSpliceRig(t, env, nil)
	for _, id := range []string{e.CrazeSessionID, e.ProviderSessionID, e.HostID} {
		p, hr := connected(t, rg.sock, id)
		if hr.Endpoint.HostID != hostOf(1) {
			t.Fatalf("connect %s reached host %s, want %s", id, hr.Endpoint.HostID, hostOf(1))
		}
		m := p.call(protocol.MethodSessionAttach, protocol.AttachParams{SessionID: sessionOf(1)})
		var res protocol.AttachResult
		if m.Error != nil || json.Unmarshal(m.Result, &res) != nil || res.Session.SessionID != sessionOf(1) {
			t.Fatalf("an attach through the splice (connect %s): %s", id, clip(m.raw))
		}
		_ = p.nc.Close()
	}
}

// TestConnectRefusals (§3.7's exclusive handoff, its lookup and its params):
// session.connect before hello is hello_required; after any other request on
// the connection — a list, a subscribe, a refused method — or after a
// connect refused itself, it is connect_not_first; a session no live host
// has is unknown_session; one more than one host matches (the hosts share a
// provider session id) is ambiguous_session, naming them; params other than
// a non-empty sessionId are refused as a host refuses them. Each refused
// connection stays in hub mode. The negative controls: a fresh connection's
// first request connects, and the ambiguous session's host connects by its
// host id.
func TestConnectRefusals(t *testing.T) {
	env := testEnv(t)
	hostIn(t, env, 1)
	hostIn(t, env, 2)
	shared := entryOf(t, env, hostOf(1)).ProviderSessionID
	if entryOf(t, env, hostOf(2)).ProviderSessionID != shared {
		t.Fatal("fixture: the two hosts do not share a provider session id")
	}
	rg := newSpliceRig(t, env, nil)
	connect := func(p *peer, id string) msg {
		t.Helper()
		return p.call(protocol.MethodSessionConnect, protocol.ConnectParams{SessionID: id})
	}

	t.Run("before hello", func(t *testing.T) {
		c := dial(t, rg.sock)
		resp := c.call(t, protocol.MethodSessionConnect, protocol.ConnectParams{SessionID: sessionOf(1)})
		if resp.Error == nil || resp.Error.Data.Reason != protocol.ReasonHelloRequired {
			t.Fatalf("connect before hello: %+v", resp)
		}
	})
	t.Run("after a list", func(t *testing.T) {
		p := dialPeer(t, rg.sock)
		p.list()
		refusal(t, connect(p, sessionOf(1)), protocol.CodeBadRequest, protocol.ReasonConnectNotFirst)
		p.list() // still in hub mode
	})
	t.Run("after a subscribe", func(t *testing.T) {
		p := dialPeer(t, rg.sock)
		p.subscribe()
		refusal(t, connect(p, sessionOf(1)), protocol.CodeBadRequest, protocol.ReasonConnectNotFirst)
	})
	t.Run("after a refusal", func(t *testing.T) {
		p := dialPeer(t, rg.sock)
		refusal(t, p.call(protocol.MethodSessionState, map[string]any{"sessionId": sessionOf(1)}),
			protocol.CodeUnsupported, protocol.ReasonHostOnly)
		refusal(t, connect(p, sessionOf(1)), protocol.CodeBadRequest, protocol.ReasonConnectNotFirst)
	})
	t.Run("unknown", func(t *testing.T) {
		p := dialPeer(t, rg.sock)
		refusal(t, connect(p, "no-such-session"), protocol.CodeUnknownSession, protocol.ReasonUnknownSession)
		// No longer the first request: a connect after a refused one is too.
		refusal(t, connect(p, sessionOf(1)), protocol.CodeBadRequest, protocol.ReasonConnectNotFirst)
	})
	t.Run("ambiguous", func(t *testing.T) {
		p := dialPeer(t, rg.sock)
		m := connect(p, shared)
		refusal(t, m, protocol.CodeBadRequest, protocol.ReasonAmbiguousSession)
		if !strings.Contains(m.Error.Message, hostOf(1)) || !strings.Contains(m.Error.Message, hostOf(2)) {
			t.Fatalf("the refusal does not name both hosts: %q", m.Error.Message)
		}
		if _, hr := connected(t, rg.sock, hostOf(2)); hr.Endpoint.HostID != hostOf(2) {
			t.Fatalf("connect by host id reached %s", hr.Endpoint.HostID)
		}
	})
	t.Run("params", func(t *testing.T) {
		for _, c := range []struct {
			params any
			reason protocol.Reason
		}{
			{map[string]any{}, protocol.ReasonBadRequest},
			{map[string]any{"sessionId": ""}, protocol.ReasonBadRequest},
			{map[string]any{"sessionId": 7}, protocol.ReasonBadRequest},
			{map[string]any{"sessionId": sessionOf(1), "via": nil}, protocol.ReasonUnknownField},
		} {
			c2 := dial(t, rg.sock)
			c2.hello(t)
			resp := c2.call(t, protocol.MethodSessionConnect, c.params)
			if resp.Error == nil || resp.Error.Code != protocol.RPCInvalidParams || resp.Error.Data.Reason != c.reason {
				t.Fatalf("connect %v: %+v, want -32602 %s", c.params, resp, c.reason)
			}
		}
	})
	t.Run("first", func(t *testing.T) {
		if _, hr := connected(t, rg.sock, sessionOf(1)); hr.Endpoint.HostID != hostOf(1) {
			t.Fatalf("a first connect reached %s", hr.Endpoint.HostID)
		}
	})
}

// TestConnectToAHostThatCannotBeReached (§3.7's dial): a listed host whose
// socket refuses, one whose dial does not complete within connectWait, and
// one whose peer check fails are each unavailable, reason host_unreachable —
// the slow one no sooner than the bound and no later than a step — and the
// connection stays in hub mode. The negative control: a host whose socket
// serves, behind the same hooks, connects.
func TestConnectToAHostThatCannotBeReached(t *testing.T) {
	env := testEnv(t)
	refusing, err := rundir.Bind(env, hostOf(7), rundir.Entry{CrazeSessionID: sessionOf(7), Ready: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = refusing.Close() })
	// Its listener closed and its socket left (Bind never unlinks on
	// close): listed, its lock held, and refusing.
	_ = refusing.Listener().Close()
	newRawHost(t, env, hostOf(8), sessionOf(8))
	slow := entryOf(t, env, hostOf(8)).Socket
	hostIn(t, env, 9)
	var badPeer atomic.Bool
	rg := newSpliceRig(t, env, func(k *hooks) {
		k.connectDial = func(ctx context.Context, path string) (net.Conn, error) {
			if path == slow {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
		k.connectCheck = func(*net.UnixConn) error {
			if badPeer.Load() {
				return errors.New("the peer runs as another user")
			}
			return nil
		}
	})
	p := dialPeer(t, rg.sock)
	refusal(t, p.call(protocol.MethodSessionConnect, protocol.ConnectParams{SessionID: sessionOf(7)}),
		protocol.CodeUnavailable, protocol.ReasonHostUnreachable)
	p.list() // still in hub mode

	p = dialPeer(t, rg.sock)
	start := time.Now()
	refusal(t, p.call(protocol.MethodSessionConnect, protocol.ConnectParams{SessionID: sessionOf(8)}),
		protocol.CodeUnavailable, protocol.ReasonHostUnreachable)
	if took := time.Since(start); took < connectWait {
		t.Fatalf("a dial that never completed was given up after %v, before its bound %v", took, connectWait)
	}

	badPeer.Store(true)
	p = dialPeer(t, rg.sock)
	refusal(t, p.call(protocol.MethodSessionConnect, protocol.ConnectParams{SessionID: sessionOf(9)}),
		protocol.CodeUnavailable, protocol.ReasonHostUnreachable)
	badPeer.Store(false)
	if _, hr := connected(t, rg.sock, sessionOf(9)); hr.Endpoint.HostID != hostOf(9) {
		t.Fatalf("the reachable host's connect reached %s", hr.Endpoint.HostID)
	}
}

// TestTheSpliceForwardsTheBufferedBytes (§3.7, A11): a client that pipelines
// — hello to the hub, session.connect and its hello to the host in one write
// — has its host hello read by the hub past session.connect's line, and the
// splice hands those bytes to the host first: the host answers it. The
// negative control drops them (a splice that forgot the reader's buffer): the
// host never had a hello, and answers the client's next request
// hello_required.
func TestTheSpliceForwardsTheBufferedBytes(t *testing.T) {
	env := testEnv(t)
	hostIn(t, env, 1)
	line := func(id int, method string, params any) []byte {
		b, err := protocol.MarshalLine(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	pipelined := line(3, protocol.MethodHello, hostHello)
	for _, drop := range []bool{false, true} {
		name := "forwarded"
		if drop {
			name = "dropped"
		}
		t.Run(name, func(t *testing.T) {
			pre := make(chan []byte, 1)
			rg := newSpliceRig(t, env, func(k *hooks) {
				prev := k.handedOff
				k.handedOff = func(sp *splice) {
					pre <- slices.Clone(sp.pre)
					if drop {
						sp.pre = nil
					}
					prev(sp)
				}
			})
			p := dialPeer(t, rg.sock) // its hello said
			p.methods["2"], p.methods["3"], p.methods["4"] = protocol.MethodSessionConnect, protocol.MethodHello, protocol.MethodSessionAttach
			p.id = 4
			batch := append(line(2, protocol.MethodSessionConnect, protocol.ConnectParams{SessionID: sessionOf(1)}), pipelined...)
			if _, err := p.nc.Write(batch); err != nil {
				t.Fatal(err)
			}
			if m := p.read(); string(m.ID) != "2" || string(m.Result) != "{}" {
				t.Fatalf("session.connect: %s", clip(m.raw))
			}
			select {
			case b := <-pre:
				if !bytes.Equal(b, pipelined) {
					t.Fatalf("the hub's buffered bytes at the handoff: %q, want the host hello %q", b, pipelined)
				}
			case <-time.After(step):
				t.Fatal("no handoff")
			}
			if !drop {
				m := p.read()
				var hr protocol.HelloResult
				if string(m.ID) != "3" || m.Error != nil || json.Unmarshal(m.Result, &hr) != nil || hr.Endpoint.HostID != hostOf(1) {
					t.Fatalf("the pipelined hello's answer: %s", clip(m.raw))
				}
				return
			}
			if _, err := p.nc.Write(line(4, protocol.MethodSessionAttach, protocol.AttachParams{SessionID: sessionOf(1)})); err != nil {
				t.Fatal(err)
			}
			m := p.read()
			if string(m.ID) != "4" {
				t.Fatalf("the host answered %s", clip(m.raw))
			}
			refusal(t, m, protocol.CodeBadRequest, protocol.ReasonHelloRequired)
		})
	}
}

// TestHalfCloseBothWays (§3.7, A11): spliced and attached, the client closes
// its writing half: the host reads its EOF — its attachment leaves its count —
// yet the host leg was only half-closed, so what the host sends after it
// still reaches the client, and the splice is still a client of the hub's.
// Then the host's EOF closes the client leg: the client reads its end, and
// with both legs closed the hub counts no client. The negative controls: the
// count is 1 until the client's half-close; the event after it arrives (a
// closed host leg would deliver nothing); the hub counts the splice until the
// host's end.
func TestHalfCloseBothWays(t *testing.T) {
	env := testEnv(t)
	h1, closeHost := hostIn(t, env, 1)
	counts := make(chan int, 64)
	h1.OnAttachments(func(n int) {
		select {
		case counts <- n:
		default:
		}
	})
	rg := newSpliceRig(t, env, nil)
	p, _ := connected(t, rg.sock, sessionOf(1))
	sp := rg.splice(t)
	if m := p.call(protocol.MethodSessionAttach, protocol.AttachParams{SessionID: sessionOf(1)}); m.Error != nil {
		t.Fatalf("attach: %s", clip(m.raw))
	}
	if m := p.read(); m.Method != protocol.NotifySynchronized {
		t.Fatalf("after the attach: %s", clip(m.raw))
	}
	count := func(want int) {
		t.Helper()
		for {
			select {
			case n := <-counts:
				if n == want {
					return
				}
			case <-time.After(step):
				t.Fatalf("the host's attachments never reached %d", want)
			}
		}
	}
	count(1)
	if n := clients(rg.h); n != 1 {
		t.Fatalf("the hub counts %d clients with one splice", n)
	}

	if err := p.nc.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	count(0)
	h1.Text("", "after the half-close")
	for {
		m := p.read()
		if m.Method == protocol.NotifyEvent && bytes.Contains(m.Params, []byte("after the half-close")) {
			break
		}
	}
	if n := clients(rg.h); n != 1 {
		t.Fatalf("the hub counts %d clients with a splice whose client half-closed: both legs are not closed", n)
	}
	select {
	case <-sp.done:
		t.Fatal("the splice ended at the client's half-close")
	default:
	}

	// The host's end: it closes every connection it has (a half-closed one
	// whose subscription has nothing to write is not noticed by a drop).
	closeHost()
	if !p.eof() {
		t.Fatal("the host's end did not close the client leg")
	}
	select {
	case <-sp.done:
	case <-time.After(step):
		t.Fatal("the splice did not end with the host's end")
	}
	waitFor(t, "the hub counts no client", func() bool { return clients(rg.h) == 0 })
}

// TestBackpressureIsBounded (§3.7): a client that reads nothing while its
// host floods it stops the splice's copy toward it, and so the copy's reads
// from the host: the hub holds at most its one buffer of the flood — what it
// has read of the host is what it wrote toward the client plus at most
// spliceChunk, at every look — and the host's own writes stall far short of
// the flood. Then the client reads, and the whole flood arrives in order. The
// negative control: a hub that buffered would have read the whole flood, and
// let the host's writes finish while the client read nothing.
func TestBackpressureIsBounded(t *testing.T) {
	const flood = 24 << 20
	const held = 8 << 20 // well past any socket buffering, well short of the flood
	env := testEnv(t)
	rh := newRawHost(t, env, hostOf(3), sessionOf(3))
	rg := newSpliceRig(t, env, nil)
	p := dialPeer(t, rg.sock)
	if m := p.call(protocol.MethodSessionConnect, protocol.ConnectParams{SessionID: sessionOf(3)}); m.Error != nil {
		t.Fatalf("connect: %s", clip(m.raw))
	}
	sp := rg.splice(t)
	hc := rh.accept(t)
	var wrote atomic.Int64
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 64<<10)
		for off := 0; off < flood; off += len(buf) {
			for i := range buf {
				buf[i] = pattern(off + i)
			}
			n, err := hc.Write(buf)
			wrote.Add(int64(n))
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	inMemory := func() {
		t.Helper()
		if d := sp.down.read.Load() - sp.down.written.Load(); d > spliceChunk {
			t.Fatalf("the hub holds %d bytes of the flood, past its %d-byte buffer", d, spliceChunk)
		}
	}
	// Until the flood has stalled: the hub inside a write toward the client,
	// and the host's writes not moving.
	last, still := int64(-1), 0
	deadline := time.Now().Add(step)
	for still < 10 {
		select {
		case err := <-done:
			t.Fatalf("the host wrote its whole %d-byte flood to a client that read nothing (%v): the hub took it", flood, err)
		default:
		}
		inMemory()
		if w := wrote.Load(); w == last && sp.down.writing.Load() {
			still++
		} else {
			still, last = 0, w
		}
		if time.Now().After(deadline) {
			t.Fatalf("the flood never stalled: the host has written %d bytes", wrote.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r := sp.down.read.Load(); r > held {
		t.Fatalf("the hub read %d bytes of the host's flood toward a client that read nothing", r)
	}
	t.Logf("stalled: the host wrote %d, the hub read %d and wrote %d", wrote.Load(), sp.down.read.Load(), sp.down.written.Load())

	got := 0
	buf := make([]byte, 64<<10)
	if b := p.lr.Buffered(); len(b) > 0 {
		t.Fatalf("the client's reader held %d bytes past the connect's {}", len(b))
	}
	_ = p.nc.SetReadDeadline(time.Now().Add(step))
	for got < flood {
		n, err := p.nc.Read(buf)
		for i := range n {
			if buf[i] != pattern(got+i) {
				t.Fatalf("byte %d of the flood is %d, want %d", got+i, buf[i], pattern(got+i))
			}
		}
		got += n
		if got%(1<<20) < n {
			inMemory()
		}
		if err != nil {
			t.Fatalf("after %d bytes of the flood: %v", got, err)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the host's flood: %v", err)
		}
	case <-time.After(step):
		t.Fatal("the host's flood did not finish once the client read it")
	}
}

// TestASpliceWriteStallsOnlyWithoutProgress (§3.7, the host's writeLine
// rule): a splice's write fails once no byte of it has moved for spliceStall —
// measured from the last byte that moved, never from the write's start. The
// window is shortened, and the client leg's send buffer shrunk to the
// kernel's least, so what the client reads reaches the hub's write a few KiB
// at a time.
//
//   - a client that reads a few bytes at a time — slower than one splice
//     buffer per window, yet always moving — receives every byte of the
//     host's, in order, and the splice stays up, though the hub spent longer
//     than the window writing one buffer;
//   - a client that reads nothing ends the splice once the window has passed:
//     the client reads what the hub wrote and then its end, the host leg is
//     closed, and the hub counts no client.
//
// The negative control: a write bounded from its start (one deadline of the
// window for the whole buffer) ends the slow reader's splice mid-buffer.
func TestASpliceWriteStallsOnlyWithoutProgress(t *testing.T) {
	t.Run("a slow reader", spliceSlowReader)
	t.Run("a reader that stops", spliceStoppedReader)
}

// patterned is n bytes of a pattern a reader checks byte by byte (pattern).
func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = pattern(i)
	}
	return b
}

func pattern(i int) byte { return byte(i % 251) }

// preloaded is a client spliced to a raw host whose payload is all in the
// hub's host leg before the splice runs — the handoff held in handedOff while
// the host writes it — so the splice's first read is one whole buffer. The
// client leg's send buffer is shrunk to the kernel's least.
type preloaded struct {
	rg *spliceRig
	p  *peer
	sp *splice
	hc *net.UnixConn
	// ran is when the splice was let run: no byte of it moved before.
	ran time.Time
}

func preload(t *testing.T, n int, payload []byte) *preloaded {
	t.Helper()
	env := testEnv(t)
	rh := newRawHost(t, env, hostOf(n), sessionOf(n))
	held, release := make(chan *splice, 1), make(chan struct{})
	rg := newSpliceRig(t, env, func(k *hooks) {
		prev := k.handedOff
		k.handedOff = func(sp *splice) {
			held <- sp
			<-release
			prev(sp)
		}
	})
	var once sync.Once
	run := func() { once.Do(func() { close(release) }) }
	t.Cleanup(run)
	p := dialPeer(t, rg.sock)
	if m := p.call(protocol.MethodSessionConnect, protocol.ConnectParams{SessionID: sessionOf(n)}); m.Error != nil {
		t.Fatalf("connect: %s", clip(m.raw))
	}
	var sp *splice
	select {
	case sp = <-held:
	case <-time.After(step):
		t.Fatalf("the hub handed no connection to a splice within %v", step)
	}
	hc := rh.accept(t)
	if err := sp.client.SetWriteBuffer(4 << 10); err != nil {
		t.Fatal(err)
	}
	if err := hc.SetWriteBuffer(4 * len(payload)); err != nil {
		t.Fatal(err)
	}
	_ = hc.SetWriteDeadline(time.Now().Add(step))
	if _, err := hc.Write(payload); err != nil {
		t.Fatalf("the host's %d bytes did not fit its leg before the splice ran: %v", len(payload), err)
	}
	if b := p.lr.Buffered(); len(b) > 0 {
		t.Fatalf("the client's reader held %d bytes past the connect's {}", len(b))
	}
	ran := time.Now()
	run()
	if got := rg.splice(t); got != sp {
		t.Fatal("the splice let run is not the one held")
	}
	return &preloaded{rg: rg, p: p, sp: sp, hc: hc, ran: ran}
}

// spliceSlowReader: the client takes at most readSize bytes per readEvery,
// about 21 KiB/s, so the hub's write of its first 32 KiB buffer — less the
// few KiB the shrunk leg holds — cannot finish within a second, past the
// 750 ms window, while each step the kernel frees (4 KiB on Linux) reaches
// the write every ~190 ms, well inside it.
func spliceSlowReader(t *testing.T) {
	const (
		size      = spliceChunk + spliceChunk/2
		readSize  = 256
		readEvery = 12 * time.Millisecond
	)
	setVar(t, &spliceStall, 750*time.Millisecond)
	pl := preload(t, 5, patterned(size))
	sp := pl.sp
	buf := make([]byte, readSize)
	got := 0
	// first is the hub's first read of the host — one whole buffer — seen at
	// firstAt; nextAt is when its second read is seen, the first buffer's
	// write between them.
	var first int64
	var firstAt, nextAt time.Time
	_ = pl.p.nc.SetReadDeadline(time.Now().Add(step))
	for got < size {
		time.Sleep(readEvery)
		switch r := sp.down.read.Load(); {
		case first == 0 && r > 0:
			first, firstAt = r, time.Now()
		case first > 0 && r > first && nextAt.IsZero():
			nextAt = time.Now()
		}
		n, err := pl.p.nc.Read(buf)
		for i := range n {
			if buf[i] != pattern(got+i) {
				t.Fatalf("byte %d is %d, want %d", got+i, buf[i], pattern(got+i))
			}
		}
		got += n
		if err != nil {
			t.Fatalf("after %d of the host's %d bytes, read slowly but steadily: %v (the hub wrote %d)",
				got, size, err, sp.down.written.Load())
		}
	}
	select {
	case <-sp.done:
		t.Fatal("the splice ended under a client that never stopped reading")
	default:
	}
	if n := clients(pl.rg.h); n != 1 {
		t.Fatalf("the hub counts %d clients with one live splice", n)
	}
	// The regime the test claims: the first buffer's write outlasted the
	// window (its second read seen up to an iteration late, hence the slack).
	if first != spliceChunk || nextAt.IsZero() {
		t.Fatalf("the hub's first read of the host was %d bytes, want one whole %d-byte buffer", first, spliceChunk)
	}
	if took := nextAt.Sub(firstAt) - 2*readEvery; took <= spliceStall {
		t.Fatalf("the hub wrote its first buffer in about %v, within the %v window: the reader was not slow enough to test the bound", took, spliceStall)
	} else {
		t.Logf("one buffer's write took about %v against a %v window; the splice stayed up", took, spliceStall)
	}
}

// spliceStoppedReader: the client reads nothing; the hub's write moves the few
// KiB the shrunk leg holds and then nothing, and the window ends the splice.
func spliceStoppedReader(t *testing.T) {
	const size = spliceChunk + spliceChunk/2
	setVar(t, &spliceStall, 300*time.Millisecond)
	pl := preload(t, 6, patterned(size))
	sp := pl.sp
	select {
	case <-sp.done:
	case <-time.After(step):
		t.Fatalf("a client that read nothing kept its splice past %v, its window %v", step, spliceStall)
	}
	if took := time.Since(pl.ran); took < spliceStall {
		t.Fatalf("the splice ended %v after it ran, inside its %v window", took, spliceStall)
	}
	written := sp.down.written.Load()
	if written <= 0 || written >= size {
		t.Fatalf("the hub wrote %d of the host's %d bytes toward a client that read nothing", written, size)
	}
	buf := make([]byte, 64<<10)
	got := 0
	_ = pl.p.nc.SetReadDeadline(time.Now().Add(step))
	var err error
	for err == nil {
		var n int
		n, err = pl.p.nc.Read(buf)
		for i := range n {
			if buf[i] != pattern(got+i) {
				t.Fatalf("byte %d is %d, want %d", got+i, buf[i], pattern(got+i))
			}
		}
		got += n
	}
	if !errors.Is(err, io.EOF) || int64(got) != written {
		t.Fatalf("the client read %d bytes and then %v, want the hub's %d and its end", got, err, written)
	}
	_ = pl.hc.SetReadDeadline(time.Now().Add(step))
	if _, err := pl.hc.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("the host leg is still open after the splice ended (%v)", err)
	}
	waitFor(t, "the hub counts no client", func() bool { return clients(pl.rg.h) == 0 })
}

// TestTeardownClosesALiveSplice (§3.5, §3.7): a hub tearing down with a live
// splice half-closes both legs first: the host reads its end, and the client
// its own, while the hub still runs. Then:
//
//   - a host that keeps its leg and says nothing, beside a client that does
//     nothing either, leaves the splice to the teardown's bound (its drain set
//     past it): both legs are closed then, and the hub is gone within that
//     bound;
//   - a host that answers its end can still write to the hub — its leg was
//     half-closed, not closed — and what it writes meets the client leg's
//     closed writing half, which ends the splice.
//
// Through the splice the host read the client's hello as the client wrote it
// — via still absent — from the hub, its peer. The negative control: before
// the teardown the host had read no end.
func TestTeardownClosesALiveSplice(t *testing.T) {
	for _, speaks := range []bool{false, true} {
		name := "a silent host"
		if speaks {
			name = "a host that answers its end"
		}
		t.Run(name, func(t *testing.T) { teardownWithASplice(t, speaks) })
	}
}

func teardownWithASplice(t *testing.T, speaks bool) {
	env := testEnv(t)
	rh := newRawHost(t, env, hostOf(4), sessionOf(4))
	setVar(t, &teardownBound, 2*time.Second)
	setVar(t, &spliceDrain, time.Hour)
	rg := newSpliceRig(t, env, nil)
	p := dialPeer(t, rg.sock)
	if m := p.call(protocol.MethodSessionConnect, protocol.ConnectParams{SessionID: sessionOf(4)}); m.Error != nil {
		t.Fatalf("connect: %s", clip(m.raw))
	}
	hc := rh.accept(t)
	if pid, _, err := rundir.PeerCheck(os.Geteuid())(hc); err != nil || pid != os.Getpid() {
		t.Fatalf("the host's peer is pid %d (%v), want the hub's, %d", pid, err, os.Getpid())
	}
	sent, err := protocol.MarshalLine(map[string]any{"jsonrpc": "2.0", "id": 9, "method": "hello", "params": hostHello})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.nc.Write(sent); err != nil {
		t.Fatal(err)
	}
	got, err := protocol.NewLineReader(hc, 0).ReadLine()
	if err != nil || !bytes.Equal(append(got, '\n'), sent) {
		t.Fatalf("the host read %q (%v), want the client's hello as written, %q", got, err, sent)
	}
	hostEnd := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, hc)
		hostEnd <- err
	}()
	select {
	case err := <-hostEnd:
		t.Fatalf("the host read its end before the teardown (%v)", err)
	case <-time.After(50 * time.Millisecond):
	}

	start := time.Now()
	rg.rn.sigs <- syscall.SIGTERM
	select {
	case err := <-hostEnd:
		if err != nil {
			t.Fatalf("the host's leg failed (%v), where it is half-closed first", err)
		}
	case <-time.After(step):
		t.Fatal("the host never read its end")
	}
	if !rg.rn.isRunning() {
		t.Fatal("the hub had stopped by the time the host read its end: closed, not half-closed first")
	}
	if speaks {
		_ = hc.SetWriteDeadline(time.Now().Add(step))
		if _, err := hc.Write([]byte("still there?\n")); err != nil {
			t.Fatalf("the host's leg was closed at the teardown's start, not half-closed: %v", err)
		}
	}
	if !p.eof() {
		t.Fatal("the client did not read its end")
	}
	rg.rn.stopped(t)
	took := time.Since(start)
	if took > teardownBound+time.Second {
		t.Fatalf("the teardown took %v with a live splice, past its bound %v", took, teardownBound)
	}
	if !speaks && took < teardownBound {
		t.Fatalf("the teardown closed a splice nobody ended after %v, before its bound %v", took, teardownBound)
	}
	_ = hc.SetWriteDeadline(time.Now().Add(step))
	if _, err := hc.Write([]byte("anyone?\n")); err == nil {
		t.Fatal("the host's leg is still open after the teardown")
	}
	if log := rg.rn.stderr.String(); !strings.Contains(log, "closed 1 splice(s)") {
		t.Fatalf("the hub's log: %s", log)
	}
}
