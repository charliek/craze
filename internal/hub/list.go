package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
)

// The roster read once (plan 032 §3.12, craze ps): from the hub (List), or —
// with no hub to ask — straight from the hosts, as the hub itself would
// (Direct). A hub's client's connection (hubConn) is List's, and Create's
// (create.go).

// List asks the hub at socket for its roster — hello as who, then
// sessions.list — and answers the result as the hub wrote it, not
// re-encoded. The connection is peer-checked before a byte is written (the
// hub runs as this user), a full listen backlog is dialled again, and ctx
// bounds it all: its end closes the connection, and is the error. The
// connection has no deadline of its own: one at ctx's deadline would race
// ctx's own timer, and a read that timed out a moment before ctx ended would
// answer an i/o timeout rather than ctx's end. Anything but a hub answering
// hello, a refusal, or a result that is not a hub's roster is an error.
func List(ctx context.Context, socket string, who protocol.ClientInfo) (json.RawMessage, error) {
	hc, err := dialHub(ctx, socket, who)
	if err != nil {
		return nil, err
	}
	defer hc.close()
	res, err := hc.call(protocol.MethodSessionsList, protocol.SessionsListParams{})
	if err != nil {
		return nil, err
	}
	var check protocol.HubSessionsListResult
	if err := json.Unmarshal(res, &check); err != nil {
		return nil, fmt.Errorf("hub: %s's result: %w", protocol.MethodSessionsList, err)
	}
	if check.Epoch != hc.hello.Endpoint.HostID {
		return nil, fmt.Errorf("hub: the roster's epoch %q is not the hub's id %q", check.Epoch, hc.hello.Endpoint.HostID)
	}
	return res, nil
}

// hubConn is one connection to a hub, said hello to (dialHub): what List and
// Create ask over, one call at a time.
type hubConn struct {
	ctx   context.Context
	nc    net.Conn
	lr    *protocol.LineReader
	stop  func() bool
	hello protocol.HubHelloResult
	next  int
}

// dialHub dials the hub at socket, peer-checks it and says hello as who (the
// comment on List): the connection, its hello answered by a hub of a
// protocol this build speaks.
func dialHub(ctx context.Context, socket string, who protocol.ClientInfo) (*hubConn, error) {
	nc, err := dialPastBacklog(ctx, helloDial, socket)
	if err != nil {
		return nil, ctxOr(ctx, fmt.Errorf("hub: dial %s: %w", socket, err))
	}
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		_ = nc.Close()
		return nil, errors.New("hub: not a unix socket connection")
	}
	if err := rundir.DialCheck(os.Geteuid())(uc); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("hub: the socket %s: %w", socket, err)
	}
	hc := &hubConn{ctx: ctx, nc: nc, lr: protocol.NewLineReader(nc, protocol.OutboundLineMax),
		stop: context.AfterFunc(ctx, func() { _ = nc.Close() })}
	b, err := hc.call(protocol.MethodHello, protocol.HelloParams{Protocols: protocol.SupportedProtocols(), Client: who})
	if err != nil {
		hc.close()
		return nil, err
	}
	if err := json.Unmarshal(b, &hc.hello); err != nil {
		hc.close()
		return nil, fmt.Errorf("hub: hello's result: %w", err)
	}
	if hc.hello.Endpoint.Kind != protocol.EndpointHub {
		hc.close()
		return nil, fmt.Errorf("hub: an endpoint of kind %q answers at the hub's socket %s", hc.hello.Endpoint.Kind, socket)
	}
	if !slices.Contains(protocol.SupportedProtocols(), hc.hello.Protocol) {
		hc.close()
		return nil, fmt.Errorf("hub: the hub (craze %s) chose protocol %d, which this craze does not speak",
			hc.hello.Endpoint.CrazeVersion, hc.hello.Protocol)
	}
	return hc, nil
}

// call sends one request — ids counted from 1 — and reads its answer, which
// must be the next line: its result, or an error — the hub's refusal
// wrapped, ErrUnanswered for a connection that ended (or failed) before the
// answer, ctx's own once it has ended.
func (hc *hubConn) call(method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	hc.next++
	id := strconv.Itoa(hc.next)
	if err := protocol.WriteLine(hc.nc, protocol.Request{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(id),
		Method: method, Params: raw}); err != nil {
		return nil, ctxOr(hc.ctx, fmt.Errorf("%w: writing %s: %w", ErrUnanswered, method, err))
	}
	line, err := hc.lr.ReadLine()
	if err != nil {
		return nil, ctxOr(hc.ctx, fmt.Errorf("%w: reading %s's answer: %w", ErrUnanswered, method, err))
	}
	var resp protocol.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("hub: %s's answer: %w", method, err)
	}
	if string(resp.ID) != id {
		return nil, fmt.Errorf("hub: %s was answered by a line that is not its reply", method)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("hub: %s refused: %w", method, resp.Error)
	}
	return resp.Result, nil
}

// close ends the connection.
func (hc *hubConn) close() {
	hc.stop()
	_ = hc.nc.Close()
}

// ctxOr is ctx's error once ctx has ended — the close its end made (which
// runs only once ctx is done) is why err happened — and err otherwise.
func ctxOr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

// directBudget, when set, replaces a Direct poll's attempt budget
// (roster.HubOptions.Budget): a test's, so a starved scheduler never fails an
// attempt the test did not mean to (never in parallel). 0 is production's.
var directBudget time.Duration

// Direct is the roster a hub would answer, read with no hub: one run of the
// hub's own poll (roster.OpenHub) over env's registry — rundir.Hosts, which
// sweeps the dead — asking every host once, as who, by the poll's own budgets
// and cap. It answers once every host that can be listed has been heard from
// (an answer, or a failure) or ctx ends, the hosts not heard from by then
// listed as they stand (connecting). Its rows are built and bounded as the
// hub builds them (§3.6: the size contract, approximate, at most
// RosterRowsMax in host-id order, truncated beyond), at the time it answers;
// its Epoch is "" and its Cursor 0, since no hub's roster it is. A registry
// that cannot be read is an error, as is a ctx that ends before the first
// read.
func Direct(ctx context.Context, env rundir.Env, who protocol.ClientInfo) (protocol.HubSessionsListResult, error) {
	var (
		mu     sync.Mutex
		latest roster.Snapshot
		have   bool
	)
	changed := make(chan struct{}, 1)
	p := roster.OpenHub(roster.HubOptions{
		Hosts: func() ([]rundir.Entry, error) { return hostsRead(env) },
		Publish: func(s roster.Snapshot) {
			mu.Lock()
			latest, have = s, true
			mu.Unlock()
			select {
			case changed <- struct{}{}:
			default:
			}
		},
		Client: who,
		Budget: directBudget,
	})
	p.Resume(1)
	heard := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if !have || latest.Run != 1 {
			return false
		}
		if latest.RegistryErr != nil {
			return true
		}
		for _, r := range latest.Running {
			if listable(r.Host) && !r.Polled {
				return false
			}
		}
		return true
	}
wait:
	for !heard() {
		select {
		case <-changed:
		case <-ctx.Done():
			break wait
		}
	}
	p.Close()
	mu.Lock()
	snap, ok := latest, have
	mu.Unlock()
	switch {
	case !ok:
		return protocol.HubSessionsListResult{}, fmt.Errorf("hub: the registry was not read: %w", ctx.Err())
	case snap.RegistryErr != nil:
		return protocol.HubSessionsListResult{}, snap.RegistryErr
	}
	now := time.Now()
	res := protocol.HubSessionsListResult{Sessions: []protocol.RosterRow{}}
	for _, in := range snap.Running {
		row, ok := RosterRow(in, now)
		if !ok {
			continue
		}
		if len(res.Sessions) == protocol.RosterRowsMax {
			res.Truncated = true
			break
		}
		res.Sessions = append(res.Sessions, row)
	}
	return res, nil
}

// RosterRow is the poller's row in as the hub lists it at now (§3.6, P4):
// its bounded roster row — approximate when it was cut, is not reachable, or
// was last read more than freshFor before now — and false for a host the hub
// does not list (a host id, craze session id or protocol of the wrong form).
func RosterRow(in roster.Row, now time.Time) (protocol.RosterRow, bool) {
	if !listable(in.Host) {
		return protocol.RosterRow{}, false
	}
	e := rosterEntry{in: in}
	e.build(in)
	return e.at(now), true
}
