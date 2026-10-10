package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/protocol"
)

// Side questions over the socket (plan 040 §3.7): Session.Aside is
// session.aside, its answer's pieces the aside notifications the host sends
// the connection that asked.
//
// # The sink
//
// An aside's notifications carry no subscription: the reader hands each, by
// its asideId, to the sink its Session.Aside registered on the Client — bound
// to the one connection the aside was sent on, so a notice read on any other
// is dropped — before the stream's notifications are looked at (notification).
// The sink calls the caller's onDelta inline, on the reader, under the sink's
// own lock: onDelta must not block (the agent.Asider contract), and the stream
// waits behind it. Aside retires its sink — taking that lock, so a callback in
// progress finishes first — before it returns, and takes it off the Client:
// no callback runs once Aside has returned, and the reader handles a
// connection's lines in order, so every notice the host wrote before the reply
// has been handed over by the time the reply is. Close retires every sink.
//
// # No resend
//
// A side question costs a model call, so it is never sent twice: one the
// connection took before a byte of it was written is sent on the next
// connection of the same identity, as a read is (nothing reached the host);
// one sent and then lost is ErrConnectionLost — the host may have answered it,
// and its connection's close has cancelled it if not. Aside is bound to the
// identity the client holds at entry as a read is (Session.read): the ctx's
// epoch must name it, the request is written only on one of its connections,
// and an answer that comes back after the client has left it is
// backend.ErrStaleEpoch.
//
// # Cancel
//
// A ctx that ends while the host owes the answer sends session.aside.cancel
// on the connection the aside went out on — never another, whose host never
// saw the id; nothing when that connection is gone, whose close has cancelled
// the aside already — and waits for its {} (the host answers it once the
// aside's own reply is queued). The cancel's whole cost is bounded by
// asideCancelWait: its send, which waits for the connection's write lock and
// writes with no deadline (a peer that has stopped reading, another request
// mid-write), and its reply run on a goroutine of their own, which Aside
// waits for no longer than that. Aside then answers the ctx's error, its sink
// retired and taken off; the goroutine ends once its write returns — at the
// latest when the connection closes, as Close closes it — and its reply's
// wait is bounded too.

// asideCancelWait bounds how long Aside, its ctx ended, waits for the host to
// answer the session.aside.cancel it sent.
const asideCancelWait = 2 * time.Second

// asideSink is one Session.Aside's receiver of its aside notifications.
type asideSink struct {
	// w is the connection the aside was sent on: a notice read on another is
	// not this aside's.
	w       *wire
	onDelta func(string)

	mu sync.Mutex
	// retired says Aside has stopped listening: no callback runs after it is
	// set. seq is the last notice handed over: one that does not come after
	// it is dropped.
	retired bool
	seq     uint64
}

// deliver hands n's text to the caller's onDelta, unless the sink is retired,
// n was read on another connection, or n does not follow the last notice.
func (k *asideSink) deliver(w *wire, n protocol.AsideNoticeParams) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.retired || w != k.w || n.Seq <= k.seq {
		return
	}
	k.seq = n.Seq
	k.onDelta(n.Text)
}

// retire stops the sink, once any callback in progress has returned.
func (k *asideSink) retire() {
	k.mu.Lock()
	k.retired = true
	k.mu.Unlock()
}

// mintAsideID is a fresh asideId: never reused by this client, so unique
// among any connection's asides in flight, and session.create's requestId
// shape (protocol.ValidRequestID).
func (c *Client) mintAsideID() string {
	return "a-" + strconv.FormatUint(c.nextAside.Add(1), 10)
}

// addAsideSink registers k as id's sink; false says the client has stopped,
// and nothing was registered.
func (c *Client) addAsideSink(id string, k *asideSink) bool {
	c.asideMu.Lock()
	defer c.asideMu.Unlock()
	if c.asidesClosed {
		return false
	}
	if c.asides == nil {
		c.asides = map[string]*asideSink{}
	}
	c.asides[id] = k
	return true
}

// dropAsideSink retires k and takes it off the client.
func (c *Client) dropAsideSink(id string, k *asideSink) {
	k.retire()
	c.asideMu.Lock()
	if c.asides[id] == k {
		delete(c.asides, id)
	}
	c.asideMu.Unlock()
}

// closeAsideSinks retires every sink and takes them all off the client, which
// registers none from now on (terminate).
func (c *Client) closeAsideSinks() {
	c.asideMu.Lock()
	sinks := c.asides
	c.asides, c.asidesClosed = nil, true
	c.asideMu.Unlock()
	for _, k := range sinks {
		k.retire()
	}
}

// asideNotice routes one aside notification, read on w, to its sink. One
// whose aside has no sink — finished, cancelled, or never this client's — is
// dropped; params that do not decode break the protocol (errMalformed).
func (c *Client) asideNotice(w *wire, params json.RawMessage) error {
	var n protocol.AsideNoticeParams
	if err := json.Unmarshal(params, &n); err != nil {
		return fmt.Errorf("%w: %s's params: %w", errMalformed, protocol.NotifyAside, err)
	}
	c.asideMu.Lock()
	k := c.asides[n.AsideID]
	c.asideMu.Unlock()
	if k == nil {
		return nil
	}
	if h := c.hooks.delivering; h != nil {
		h(n.AsideID, n.Seq)
	}
	k.deliver(w, n)
	return nil
}

// Aside is session.aside (plan 040 §3.7): one side question, its answer's
// pieces handed to onDelta (nil: none) as the aside notifications bring them
// — inline, on the connection's reader, so onDelta must not block — and never
// once Aside has returned; the reply's text is the whole answer all the same.
//
// A session whose info document does not say aside — cursor's, a host from
// before the method, or no attach reply yet — is answered
// agent.ErrAsideUnsupported here and nothing is sent; so is a host that
// refuses it anyway, code unsupported (aside_unsupported, or unknown_method
// from an older host the document came from before a reconnect reached it). A
// ctx already ended sends nothing. It is never resent (see the file's doc): a
// request sent and then lost is ErrConnectionLost. A ctx that ends while the
// answer is owed posts session.aside.cancel on the same connection and
// answers the ctx's error. The host's other refusals are its *Error, which
// reconstructs their sentinels (aside_busy is engine.ErrAsideBusy; context, an
// aside the host's own bound or the session's end cut off, is
// context.DeadlineExceeded, as every context reason is).
func (s *Session) Aside(ctx context.Context, req agent.AsideRequest, onDelta func(string)) (agent.AsideResult, error) {
	ident := s.c.Identity()
	if err := backend.CheckEpoch(ctx, ident); err != nil {
		return agent.AsideResult{}, err
	}
	if !s.Info().Aside {
		return agent.AsideResult{}, agent.ErrAsideUnsupported
	}
	if err := ctx.Err(); err != nil {
		return agent.AsideResult{}, err
	}
	sid, err := s.sessionID(ctx, ident)
	if err != nil {
		return agent.AsideResult{}, err
	}
	if onDelta == nil {
		onDelta = func(string) {}
	}
	p := protocol.AsideParams{SessionID: sid, AsideID: s.c.mintAsideID(), Question: req.Question}
	for _, t := range req.Prior {
		p.Prior = append(p.Prior, protocol.AsideTurnParam{Question: t.Question, Answer: t.Answer})
	}
	var r protocol.AsideResult
	err = s.c.aside(ctx, ident, p, onDelta, &r)
	var e *Error
	switch {
	case errors.Is(err, ErrConnectionLost), ctx.Err() != nil && errors.Is(err, ctx.Err()):
		// Nothing came back from the host: the answer was lost on the way,
		// or the caller stopped waiting.
		return agent.AsideResult{}, err
	case s.c.Identity() != ident:
		// Whatever came back came back from a session this caller is no
		// longer bound to.
		return agent.AsideResult{}, backend.ErrStaleEpoch
	case errors.As(err, &e) && e.Code == protocol.CodeUnsupported && !errors.Is(err, agent.ErrAsideUnsupported):
		return agent.AsideResult{}, fmt.Errorf("%w: %w", agent.ErrAsideUnsupported, err)
	case err != nil:
		return agent.AsideResult{}, err
	}
	out := agent.AsideResult{Text: r.Text, Model: r.Model}
	if u := r.Usage; u != nil {
		out.Usage = &agent.AsideUsage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead}
	}
	return out, nil
}

// aside sends p as session.aside on a connection of identity ident, its
// notices to onDelta through a sink registered for p.AsideID on that
// connection, and decodes the reply into result. A connection gone before a
// byte was written is waited out and the next one of ident used (onWireAs) —
// nothing reached the host; one lost after is ErrConnectionLost, never
// resent.
func (c *Client) aside(ctx context.Context, ident uint64, p protocol.AsideParams, onDelta func(string), result *protocol.AsideResult) error {
	raw, err := paramsJSON(p)
	if err != nil {
		return err
	}
	cancel := protocol.AsideCancelParams{SessionID: p.SessionID, AsideID: p.AsideID}
	return c.onWireAs(ctx, ident, func(w *wire) error {
		k := &asideSink{w: w, onDelta: onDelta}
		if !c.addAsideSink(p.AsideID, k) {
			// terminate has set why before it closed the sinks.
			return c.Err()
		}
		defer c.dropAsideSink(p.AsideID, k)
		return c.asideOn(ctx, w, k, raw, cancel, result)
	})
}

// asideOn is one attempt of aside on w: errUnsent when w was gone before a
// byte of it was written; nothing at all when ctx has already ended.
func (c *Client) asideOn(ctx context.Context, w *wire, k *asideSink, raw json.RawMessage, cancel protocol.AsideCancelParams, result *protocol.AsideResult) error {
	if err := ctx.Err(); err != nil {
		// callRaw would write before it looks at ctx: an ended ctx sends
		// nothing.
		return err
	}
	// Its write, as every method's, waits on a stalled peer whatever ctx says (send).
	return c.callRaw(ctx, w, protocol.MethodSessionAside, raw, result, func() {
		// The host still owes the answer: stop listening, and ask it to
		// stop, on w alone.
		k.retire()
		c.cancelAside(w, cancel)
	})
}

// cancelAside is session.aside.cancel with params on w, never another
// connection: best effort, nothing sent when w is gone or the client has
// stopped. Its send and its reply's wait run on a goroutine the client's wait
// group counts — counted under c.mu, so Close, which sets err under it before
// it waits, never misses it (postFromCaller) — and cancelAside waits for them
// at most asideCancelWait: send waits for w's write lock and writes with no
// deadline, which must not hold Session.Aside past the bound. The goroutine
// ends once its write has returned (w's close ends a write that waits) and
// then its reply's wait, bounded by the same ctx.
func (c *Client) cancelAside(w *wire, params protocol.AsideCancelParams) {
	ctx, cancel := context.WithTimeout(c.ctx, asideCancelWait)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		cancel()
		return
	}
	c.wg.Add(1)
	c.mu.Unlock()
	sent := make(chan struct{})
	go func() {
		defer c.wg.Done()
		defer close(sent)
		defer cancel()
		_ = c.callOn(ctx, w, protocol.MethodSessionAsideCancel, params, nil)
	}()
	select {
	case <-sent:
	case <-ctx.Done():
	}
}
