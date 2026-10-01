package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// session.connect and the splice (plan 032 §3.7; plan 027 §3.3's hub hop).
//
// # The handoff, exclusive
//
// session.connect must be the connection's first request after hello — no
// line answered since hello, whatever it was, and no roster subscription
// (conn.answered, conn.subs) — or it is refused bad_request, reason
// connect_not_first. The connection answers one line at a time, so nothing
// else is in flight while it is answered, and with no subscription there is
// no flusher: the connection's own goroutine is its only writer. On it the
// hub stops reading (the read loop ends with the answer), resolves the session
// (lookup), dials its host (dialHost), hands the connection to the splice
// (conn.handTo, which the teardown's close orders itself against), writes {}
// — the last line the hub writes on the connection, synchronously, so it is
// in the kernel's hands once the write returns: there is no writer to drain —
// and takes the reader's buffered bytes (protocol.LineReader.Buffered): what
// the client sent past session.connect's line before the {} arrived, a
// pipelining client's host hello. From then the splice alone touches the
// connection: one goroutine reads each direction, and each writes only to the
// other leg.
//
// # Lookup, dial
//
// craze bridge's rule (internal/cli's matchSession): every live host in the
// registry (rundir.Hosts, which sweeps the dead) whose craze session id,
// provider session id or host id equals sessionId. None is unknown_session;
// more than one bad_request, reason ambiguous_session — the client names the
// one it means by its host id. The dial is a connect and a peer check
// (rundir.DialCheck: the host runs as this user), together within
// connectWait; a full listen backlog is dialled again until then. A failure is
// unavailable, reason host_unreachable. The client's own host hello is the
// first thing the host reads: the hub says nothing to the host, so the host
// sees the hub's pid as its peer, and hello's via stays as the client sent it
// (null).
//
// # The splice
//
// The buffered bytes go to the host first, then two copies run, each through
// a buffer of spliceChunk (32 KiB) — a peer that does not read stops the copy
// toward it, and so the reading from the other: the hub holds at most one
// buffer per direction, and backpressure reaches the writer (a host's own
// write-stall bound ends a connection whose client stopped reading). A write
// fails once no byte of it has moved for spliceStall — measured from the last
// byte that moved, never from the write's start: the host's own stall bound
// and rule (internal/control's writeLine).
// The client's EOF closes the host leg's writing half (CloseWrite, never
// Close): the host sees the client go — its attachment leaves the host's idle
// count — and still answers what it admitted, which reaches the client. The
// host's EOF closes the client leg, and with it the splice. Any other error
// closes both legs. The splice runs on the connection's goroutine, so the
// connection stays a client — keeping the hub from its idle exit — until both
// legs are closed. The teardown ends a splice in two steps (server.closeAll):
// both legs half-closed, then closed.

// The splice's bounds: variables only so a test can change them (never in
// parallel).
var (
	// connectWait bounds session.connect's dial: the connect and the peer
	// check together.
	connectWait = 500 * time.Millisecond
	// spliceStall bounds a splice's write: a peer that takes nothing for that
	// long — from the last byte that moved — ends the splice. 60 s, the host's
	// own write-stall bound (internal/control's writeStall).
	spliceStall = 60 * time.Second
	// spliceDrain is how long the teardown gives a splice it has half-closed
	// to end on its own before it closes both legs.
	spliceDrain = 500 * time.Millisecond
)

// spliceChunk is a splice copy's buffer: 32 KiB per direction.
const spliceChunk = 32 << 10

// spliceAttempts is how many attempts spliceStall is cut into, as the host's
// writer cuts its own (internal/control's writeAttempts): each socket write
// waits at most a spliceAttempts'th of it (a second, of 60 s) before the
// splice looks at its progress again.
const spliceAttempts = 60

// errSpliceStalled is a splice write that moved no byte for spliceStall.
var errSpliceStalled = errors.New("splice write stalled: the peer is not reading")

// connect answers session.connect (the file's comment). A refusal answers
// and keeps the connection in hub mode, as any refusal does (true, while its
// write succeeded); otherwise the read loop ends (false), the connection
// handed to a splice (handoff) — or ended, when its {} could not be written
// or the teardown came first.
func (c *conn) connect(req *request) bool {
	if c.answered > 0 || len(c.subs) > 0 {
		return c.replyErr(req.id, refused(protocol.CodeBadRequest, protocol.ReasonConnectNotFirst,
			"session.connect must be the connection's first request after hello, with no roster subscription: the splice takes the whole connection"))
	}
	sessionID, perr := connectParams(req.params)
	if perr != nil {
		return c.replyErr(req.id, perr)
	}
	h := c.s.h
	e, perr := h.lookup(sessionID)
	if perr != nil {
		return c.replyErr(req.id, perr)
	}
	hc, err := h.dialHost(e.Socket)
	if err != nil {
		// The dial's error names the host's socket path, which never crosses
		// the wire (§3.6): it is the hub's log's alone (r23 2).
		h.logf("session.connect %s: host %s is unreachable: %v", sessionID, e.HostID, err)
		return c.replyErr(req.id, refused(protocol.CodeUnavailable, protocol.ReasonHostUnreachable,
			"session %s's host %s cannot be reached", sessionID, e.HostID))
	}
	sp := &splice{client: c.uc, host: hc, hostID: e.HostID, sessionID: sessionID, done: make(chan struct{})}
	if !c.handTo(sp) {
		// The teardown closed the connection meanwhile.
		sp.abandon()
		return false
	}
	if !c.reply(req.id, protocol.Empty{}) {
		sp.abandon()
		return false
	}
	sp.pre = c.lr.Buffered()
	if f := h.hk.handedOff; f != nil {
		f(sp)
	}
	c.handoff = sp
	h.logf("spliced a connection to host %s (session %s)", e.HostID, sessionID)
	return false
}

// connectParams is session.connect's params, held as a host holds a method's
// (strict): a member other than sessionId is unknown_field, and sessionId must
// be a non-empty string.
func connectParams(raw json.RawMessage) (string, *protocol.Error) {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || string(b) == "null" {
		return "", badParams("params.sessionId is required: the session to connect to")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return "", badParams("params must be an object")
	}
	var unknown []string
	for k := range m {
		if k != "sessionId" {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return "", &protocol.Error{Code: protocol.RPCInvalidParams, Message: "unknown field params." + unknown[0],
			Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonUnknownField}}
	}
	raw, ok := m["sessionId"]
	if !ok {
		return "", badParams("params.sessionId is required: the session to connect to")
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", badParams("params.sessionId must be a string")
	}
	if id == "" {
		return "", badParams("params.sessionId must not be empty")
	}
	return id, nil
}

// lookup is the one live host sessionID names (the file's comment), or the
// refusal that answers the connect. A registry that cannot be read is
// refused with no word of why — its error names the registry's paths — and
// the hub's log says it (r23 2).
func (h *hub) lookup(sessionID string) (rundir.Entry, *protocol.Error) {
	entries, err := hostsRead(h.o.Env)
	if err != nil {
		h.logf("session.connect %s: the registry cannot be read: %v", sessionID, err)
		return rundir.Entry{}, refused(protocol.CodeUnavailable, protocol.ReasonHostUnreachable,
			"the registry cannot be read, so no session's host can be found")
	}
	var matches []rundir.Entry
	for _, e := range entries {
		if e.CrazeSessionID == sessionID || e.ProviderSessionID == sessionID || e.HostID == sessionID {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return rundir.Entry{}, &protocol.Error{Code: protocol.RPCRefused, Message: "no session " + sessionID,
			Data: protocol.ErrorData{Code: protocol.CodeUnknownSession, Reason: protocol.ReasonUnknownSession}}
	case 1:
		return matches[0], nil
	}
	ids := make([]string, len(matches))
	for i, e := range matches {
		ids[i] = e.HostID
	}
	slices.Sort(ids)
	return rundir.Entry{}, refused(protocol.CodeBadRequest, protocol.ReasonAmbiguousSession,
		"%d sessions match %s; name the one you mean by its host id: %v", len(matches), sessionID, ids)
}

// dialHost dials a session's host at socket for a splice: the connect —
// again while its backlog is full — and the peer check, together within
// connectWait.
func (h *hub) dialHost(socket string) (*net.UnixConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectWait)
	defer cancel()
	dial := h.hk.connectDial
	if dial == nil {
		dial = func(ctx context.Context, path string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
	}
	nc, err := dialPastBacklog(ctx, dial, socket)
	if err != nil {
		return nil, err
	}
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		_ = nc.Close()
		return nil, errors.New("not a unix socket connection")
	}
	check := h.hk.connectCheck
	if check == nil {
		check = rundir.DialCheck(os.Geteuid())
	}
	// The check asks the kernel (SO_PEERCRED, LOCAL_PEERCRED): it never
	// waits on the peer.
	if err := check(uc); err != nil {
		_ = uc.Close()
		return nil, fmt.Errorf("the host's socket %s: %w", socket, err)
	}
	return uc, nil
}

// splice is a connection handed to a session's host (the file's comment).
type splice struct {
	// client is the connection's own socket, host the leg the hub dialled.
	client, host *net.UnixConn
	hostID       string
	sessionID    string
	// pre is the client's bytes the hub read past session.connect's line,
	// the host's before anything else.
	pre []byte
	// done is closed once the splice has ended, both legs closed.
	done     chan struct{}
	doneOnce sync.Once

	clientOnce, hostOnce sync.Once
	// up is the client-to-host direction's counts, down the host-to-client
	// one's: what the tests see of the backpressure.
	up, down flow
}

// flow is one direction of a splice: the bytes it has read and written, and
// whether a write is in the kernel's hands now.
type flow struct {
	read, written atomic.Int64
	writing       atomic.Bool
}

// run is the splice, on the connection's goroutine: it returns once both
// directions have ended and both legs are closed.
func (sp *splice) run() {
	defer sp.finish()
	_ = sp.client.SetDeadline(time.Time{})
	up := make(chan struct{})
	go func() {
		defer close(up)
		sp.upstream()
	}()
	sp.downstream()
	<-up
	sp.close()
}

// upstream is the client-to-host direction: the buffered bytes first, then
// the copy. The client's EOF closes the host leg's writing half; an error
// closes both legs.
func (sp *splice) upstream() {
	if len(sp.pre) > 0 {
		sp.up.read.Add(int64(len(sp.pre)))
		if err := sp.up.write(sp.host, sp.pre); err != nil {
			sp.close()
			return
		}
	}
	if err := sp.up.copy(sp.host, sp.client); err != nil {
		sp.close()
		return
	}
	if err := sp.host.CloseWrite(); err != nil {
		sp.close()
	}
}

// downstream is the host-to-client direction: the host's EOF closes the
// client leg — and so the splice: the other direction's read ends with it —
// as does an error.
func (sp *splice) downstream() {
	_ = sp.down.copy(sp.client, sp.host)
	sp.close()
}

// copy copies src to dst through one buffer of spliceChunk: nil at src's EOF,
// or the error that ended it.
func (f *flow) copy(dst, src *net.UnixConn) error {
	buf := make([]byte, spliceChunk)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			f.read.Add(int64(n))
			if werr := f.write(dst, buf[:n]); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// write writes b to dst whole, or fails once no byte of it has moved for
// spliceStall — the host's rule (internal/control's writeLine): each socket
// write gets a short deadline (a spliceAttempts'th of the bound, and never
// past the bound's end), a short write continues from where it stopped, the
// time of the last write that moved bytes is kept, and a write that times out
// fails once the bound has passed since then. A peer that reads, however
// slowly, keeps the splice; one that takes nothing for the bound ends it.
//
// A deadline that cannot be set is a failed write: nothing is written without
// one in place, since a write with none could block forever on a peer that
// stops reading.
func (f *flow) write(dst *net.UnixConn, b []byte) error {
	stall := spliceStall
	attempt := max(stall/spliceAttempts, time.Millisecond)
	f.writing.Store(true)
	defer f.writing.Store(false)
	last := time.Now()
	for len(b) > 0 {
		deadline := time.Now().Add(attempt)
		if end := last.Add(stall); end.Before(deadline) {
			deadline = end
		}
		if err := dst.SetWriteDeadline(deadline); err != nil {
			return fmt.Errorf("splice write failed: its deadline could not be set: %w", err)
		}
		n, err := dst.Write(b)
		b = b[n:]
		f.written.Add(int64(n))
		if n > 0 {
			last = time.Now()
		}
		if err == nil {
			continue
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			if time.Since(last) >= stall {
				return errSpliceStalled
			}
			continue
		}
		return err
	}
	return nil
}

// halfClose closes both legs' writing halves (the teardown's first step).
func (sp *splice) halfClose() {
	_ = sp.host.CloseWrite()
	_ = sp.client.CloseWrite()
}

// close closes both legs, each once.
func (sp *splice) close() {
	sp.hostOnce.Do(func() { _ = sp.host.Close() })
	sp.clientOnce.Do(func() { _ = sp.client.Close() })
}

// abandon is a splice that never runs — its {} could not be written, or the
// teardown came first: both legs closed, and done.
func (sp *splice) abandon() {
	sp.close()
	sp.finish()
}

func (sp *splice) finish() {
	sp.doneOnce.Do(func() { close(sp.done) })
}
