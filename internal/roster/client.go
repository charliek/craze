package roster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/version"
)

// client is the roster's protocol client (plan 030 §3.9, R2-8): one
// connection to one host, which says hello once and then asks sessions.list,
// one call at a time, on the goroutine that holds it — no reader, no writer
// and no reconnection of its own. A kept connection costs the roster nothing
// between polls (no goroutine), a failed call ends the connection, and
// whether and when to dial again is the roster's alone (its backoff): the
// remote client's own redial episodes would fight it.
//
// It is not safe for concurrent use: the poller hands a connection to one
// attempt at a time (hostState.conn).
type client struct {
	nc net.Conn
	lr *protocol.LineReader
	// maxLine is the host's inbound line limit (hello's limits).
	maxLine int
	// next numbers this connection's requests.
	next uint64
	// version is hello's endpoint.crazeVersion: the craze the host says it
	// is, which a row of an older host shows.
	version string
}

// clientInfo is who the roster says it is: the TUI's session list. The
// hub's roster says it is the hub (HubOptions.Client).
var clientInfo = protocol.ClientInfo{Kind: "tui", Name: "craze sessions", Version: version.Version}

// dialFunc opens the transport to a socket; its context bounds the dial.
type dialFunc func(ctx context.Context, path string) (net.Conn, error)

func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// dialHello opens a connection to socket and says hello as who, by deadline
// (the end of the attempt's "dial + hello" share) and until ctx ends — the
// roster's life, whose end closes the connection mid-exchange. check, when
// not nil, is run on the connection before a byte is written
// (rundir.DialCheck: the host runs as this user). A hello refused, or
// answered by anything but a host that speaks a protocol this build does, is
// an error.
func dialHello(ctx context.Context, dial dialFunc, check func(*net.UnixConn) error, who protocol.ClientInfo, socket string, deadline time.Time) (*client, error) {
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	nc, err := dial(dctx, socket)
	if err != nil {
		return nil, fmt.Errorf("roster: dial: %w", err)
	}
	if check != nil {
		err := errors.New("not a unix socket connection")
		if uc, ok := nc.(*net.UnixConn); ok {
			err = check(uc)
		}
		if err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("roster: peer check: %w", err)
		}
	}
	c := &client{nc: nc, lr: protocol.NewLineReader(nc, protocol.OutboundLineMax), maxLine: protocol.InboundLineMax}
	var h protocol.HelloResult
	err = c.exchange(ctx, deadline, func() error {
		return c.call(protocol.MethodHello, protocol.HelloParams{Protocols: protocol.SupportedProtocols(), Client: who}, &h)
	})
	switch {
	case err != nil:
	case h.Endpoint.Kind != protocol.EndpointHost:
		err = fmt.Errorf("roster: an endpoint of kind %q answered where a session's host belongs", h.Endpoint.Kind)
	case !slices.Contains(protocol.SupportedProtocols(), h.Protocol):
		err = fmt.Errorf("roster: the host chose protocol %d, which this build does not speak", h.Protocol)
	}
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	if h.Limits.InboundLine > 0 {
		c.maxLine = h.Limits.InboundLine
	}
	c.version = h.Endpoint.CrazeVersion
	return c, nil
}

// list asks the host's sessions.list by deadline (the end of the attempt's
// share) and until ctx ends. Any failure — a refusal included, and a reply
// that does not decode — leaves the connection in no state to be asked again:
// the caller closes it.
//
// With raw (the hub's roster, plan 032 §3.6, P4) it answers each row's JSON
// value too, beside its decoding, index for index: the value the host sent,
// compacted — its members this build does not know kept, at any depth — or
// nil for a row that is not a JSON object (a null), which the hub never
// forwards.
func (c *client) list(ctx context.Context, deadline time.Time, raw bool) (protocol.SessionsListResult, []json.RawMessage, error) {
	if !raw {
		var res protocol.SessionsListResult
		err := c.exchange(ctx, deadline, func() error {
			return c.call(protocol.MethodSessionsList, protocol.SessionsListParams{}, &res)
		})
		return res, nil, err
	}
	var wire struct {
		Epoch    string            `json:"epoch"`
		Cursor   uint64            `json:"cursor"`
		Sessions []json.RawMessage `json:"sessions"`
	}
	err := c.exchange(ctx, deadline, func() error {
		return c.call(protocol.MethodSessionsList, protocol.SessionsListParams{}, &wire)
	})
	if err != nil {
		return protocol.SessionsListResult{}, nil, err
	}
	res := protocol.SessionsListResult{Epoch: wire.Epoch, Cursor: wire.Cursor}
	raws := make([]json.RawMessage, len(wire.Sessions))
	for i, b := range wire.Sessions {
		var row protocol.SessionRow
		if err := json.Unmarshal(b, &row); err != nil {
			return protocol.SessionsListResult{}, nil, fmt.Errorf("roster: decoding %s's result: %w", protocol.MethodSessionsList, err)
		}
		res.Sessions = append(res.Sessions, row)
		raws[i] = compactObject(b)
	}
	return res, raws, nil
}

// compactObject is b — one JSON value, valid — compacted, as the hub's
// encoder writes a json.RawMessage (no HTML escaping), so its length is the
// bytes it costs on the wire; nil when it is not an object.
func compactObject(b json.RawMessage) json.RawMessage {
	t := bytes.TrimSpace(b)
	if len(t) == 0 || t[0] != '{' {
		return nil
	}
	var out bytes.Buffer
	if err := json.Compact(&out, t); err != nil {
		return nil
	}
	return out.Bytes()
}

// close closes the connection; the host notes it and releases the client id
// hello was given.
func (c *client) close() {
	if c != nil {
		_ = c.nc.Close()
	}
}

// exchange runs fn — calls on the connection — with the connection's deadline
// at deadline and ctx's end closing it, which ends a read or write in flight.
// It is ctx's error once ctx has ended, and leaves the connection with no
// deadline: a kept connection waits for the next poll with none.
//
// The close ctx's end makes runs on a goroutine of the context package's; an
// exchange that ctx ended waits for it to be over before it returns, so an
// attempt that has returned has nothing of its own still running, and
// Close's join of the attempts is a join of that close too (sol r17-c9 3).
func (c *client) exchange(ctx context.Context, deadline time.Time, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(closed)
		_ = c.nc.Close()
	})
	_ = c.nc.SetDeadline(deadline)
	err := fn()
	stopped := stop()
	_ = c.nc.SetDeadline(time.Time{})
	if !stopped {
		// ctx ended, and the close has been started: it is waited for.
		<-closed
		return ctx.Err()
	}
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// reply is one line a host wrote, decoded as far as the roster needs it: a
// reply by its id, or a notification (a method and no id), which a
// connection that attaches nothing is never sent, and which is passed over.
type reply struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *protocol.Error `json:"error"`
}

// call writes one request and reads lines until its reply, decoding the
// result into result or answering the refusal as a *refusedError.
func (c *client) call(method string, params, result any) error {
	c.next++
	id := strconv.FormatUint(c.next, 10)
	raw, err := protocol.MarshalLine(params)
	if err != nil {
		return err
	}
	line, err := protocol.MarshalLine(protocol.Request{JSONRPC: protocol.JSONRPCVersion,
		ID: json.RawMessage(id), Method: method, Params: raw[:len(raw)-1]})
	if err != nil {
		return err
	}
	if len(line)-1 > c.maxLine {
		return fmt.Errorf("roster: a %s request of %d bytes is over the host's limit", method, len(line)-1)
	}
	if _, err := c.nc.Write(line); err != nil {
		return fmt.Errorf("roster: writing %s: %w", method, err)
	}
	for {
		b, err := c.lr.ReadLine()
		if err != nil {
			return fmt.Errorf("roster: reading %s's reply: %w", method, err)
		}
		var r reply
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("roster: the host wrote a malformed line: %w", err)
		}
		if r.Method != "" || string(r.ID) != id {
			continue
		}
		if r.Error != nil {
			return &refusedError{method: method, err: r.Error}
		}
		if err := json.Unmarshal(r.Result, result); err != nil {
			return fmt.Errorf("roster: decoding %s's result: %w", method, err)
		}
		return nil
	}
}

// refusedError is a host's refusal of a call: it answered, and said no — a
// hello before its engine is set, a sessions.list while it closes.
type refusedError struct {
	method string
	err    *protocol.Error
}

func (e *refusedError) Error() string {
	return fmt.Sprintf("roster: %s refused: %s (%s/%s)", e.method, e.err.Message, e.err.Data.Code, e.err.Data.Reason)
}

// stale says err is a kept connection found closed at the start of an
// attempt — the host dropped it while nobody was asking (an end of file, a
// reset, a broken pipe) — rather than a host that did not answer in time: the
// one failure the attempt dials past, once, with a fresh connection, inside
// what is left of the attempt's own budget.
func stale(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
