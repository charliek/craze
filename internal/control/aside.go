package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
)

// Side questions (plan 040 §3.2): session.aside answers one from the
// session's context, streaming the answer as aside notifications to the
// connection that asked and whole in the reply; session.aside.cancel cancels
// one of the same connection's.
//
// # Reservation
//
// Handlers run on goroutines of their own, so a cancel sent right behind its
// aside could otherwise run first, find nothing and leave the aside to start.
// So dispatch, on the reader, reserves the aside's asideId in the
// connection's map (reserveAside) before it spawns the handler: a cancel read
// after the aside's line always finds it. The entry carries the aside's
// context — the connection's, so a hard close cancels the aside (a half-close
// does not: the request is admitted and finishes, its reply written) — and is
// removed in the same conn.mu section that queues the aside's reply, or on any
// other way out of the handler (a deferred forget). A cancel's target is found
// on the reader too, as its line is read (liveAside), and its handler cancels
// that entry alone: a cancel whose handler runs late never cancels an aside
// that reused the id after the one it named had answered. A cancel that finds
// the entry answers once the aside's reply is queued (or after
// asideCancelWait), so its {} follows the aside's answer.
//
// # The emitter
//
// Each aside has one emitter goroutine, which alone numbers and queues its
// notices: the agent's onDelta only appends to the emitter's pending text,
// under its mutex, and pokes it without blocking when there is a notice to
// send. The emitter sends what is pending in notices of at most
// AsideNoticeMax bytes, cut on a character boundary — every piece as it
// arrives (AsideEachDelta), or otherwise a full
// notice's worth at once and the rest at the server's interval. When the
// agent call returns the handler tells the emitter, which sends what is left
// and exits, and only then is the reply queued: every notice precedes it on
// the connection's one FIFO. Once more than AsidePendingMax bytes wait unsent
// — pending, or taken by a flush and not yet queued — (a client not reading)
// the emitter sends no more of that aside's notices and
// drops their text; the reply still carries the whole answer. A notice that
// cannot be queued — the connection closed, or its outbox replaced — cancels
// the aside.
//
// An aside holds one of the connection's admission slots while it runs, as
// every request does; the engine bounds asides at two per session.

// asideEntry is one session.aside's reservation on its connection.
type asideEntry struct {
	id string
	// ctx is the aside's context, derived from the connection's; cancel ends
	// it (session.aside.cancel, a notice that could not be queued, the
	// handler's own end).
	ctx    context.Context
	cancel context.CancelFunc
	// queued is closed, under conn.mu, as the entry leaves the map: once the
	// aside's reply is queued, or once it never will be.
	queued chan struct{}
}

// reserveAside reserves req's asideId on the connection (on the reader, before
// the handler runs), and reports whether it did; when it did not, the request
// is answered: an asideId that is not session.create's requestId shape is
// -32602, bad_request, and one an aside of this connection in flight holds is
// bad_request too. Only asideId is read here; the handler decodes the params
// strictly.
func (c *conn) reserveAside(req *request) bool {
	var p struct {
		AsideID string `json:"asideId"`
	}
	_ = json.Unmarshal(req.params, &p)
	if perr := checkAsideID(p.AsideID); perr != nil {
		c.replyErr(req.id, perr)
		return false
	}
	ctx, cancel := context.WithCancel(c.ctx)
	e := &asideEntry{id: p.AsideID, ctx: ctx, cancel: cancel, queued: make(chan struct{})}
	c.mu.Lock()
	_, dup := c.asides[e.id]
	if !dup {
		if c.asides == nil {
			c.asides = map[string]*asideEntry{}
		}
		c.asides[e.id] = e
	}
	c.mu.Unlock()
	if dup {
		cancel()
		c.replyErr(req.id, refused(protocol.CodeBadRequest, protocol.ReasonBadRequest,
			"asideId %q is already in flight on this connection", e.id))
		return false
	}
	c.srv.asides.Add(1)
	req.aside = e
	return true
}

// checkAsideID refuses an asideId that is not session.create's requestId
// shape (protocol.ValidRequestID): -32602, bad_request.
func checkAsideID(id string) *protocol.Error {
	if !protocol.ValidRequestID(id) {
		return badParams("params.asideId %q is not 1 to %d of [A-Za-z0-9._-]", id, protocol.RequestIDMax)
	}
	return nil
}

// liveAside is the aside of this connection in flight under req's asideId as
// its session.aside.cancel's line is read (on the reader), nil when none. Only
// asideId is read here; the handler decodes the params strictly and cancels
// this entry alone, so an aside that reuses the id — read after the cancel,
// once the aside it named has answered — is never the cancel's.
func (c *conn) liveAside(req *request) *asideEntry {
	var p struct {
		AsideID string `json:"asideId"`
	}
	_ = json.Unmarshal(req.params, &p)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asides[p.AsideID]
}

// forgetAsideLocked takes e out of the connection's map and closes its
// queued; a no-op once e has left it (the map may by then hold a later aside
// of the same id). conn.mu is held.
func (c *conn) forgetAsideLocked(e *asideEntry) {
	if c.asides[e.id] != e {
		return
	}
	delete(c.asides, e.id)
	close(e.queued)
	c.srv.asides.Add(-1)
}

// sessionAside is session.aside: the engine's Aside, reached only for a
// session whose capability aside is true (dispatch refuses it
// aside_unsupported otherwise), on the reservation dispatch made. It queues
// its own reply and takes the reservation out of the map in the conn.mu
// section that queues it (send's commit): a cancel that finds the aside
// answers after this reply, and one that finds nothing finds it already
// queued; a reply that cannot be queued gives the slot back, and the deferred
// forget takes the reservation out. It is no command: no commandId, no
// receipt, no host-wide command slot, no reply barrier, and never commandCtx —
// the aside's context is the connection's, bounded by the server's
// asideCeiling.
func (c *conn) sessionAside(b *bound, info protocol.MethodInfo, req *request) outcome {
	e := req.aside
	defer func() {
		c.mu.Lock()
		c.forgetAsideLocked(e)
		c.mu.Unlock()
		e.cancel()
	}()
	c.replyOutcome(req, c.askAside(b, info, req), func() { c.forgetAsideLocked(e) })
	return replied()
}

// askAside decodes the params, asks the engine, and streams the answer's
// pieces through the aside's emitter, which has exited by the time it
// returns.
func (c *conn) askAside(b *bound, info protocol.MethodInfo, req *request) outcome {
	e := req.aside
	var p protocol.AsideParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	if h := c.srv.hooks.beforeAside; h != nil {
		h(e.ctx, e.id)
	}
	ctx, cancel := context.WithTimeout(e.ctx, c.srv.asideCeiling)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return refusal(engineError(fmt.Errorf("the side question was cancelled: %w", err)))
	}
	ar := agent.AsideRequest{Question: p.Question}
	for _, t := range p.Prior {
		ar.Prior = append(ar.Prior, agent.AsideTurn{Question: t.Question, Answer: t.Answer})
	}
	em := newAsideEmitter(c, e.id, c.srv.asideInterval, e.cancel)
	go em.run(ctx)
	r, err := b.eng.Aside(ctx, ar, em.delta)
	em.finish()
	if err != nil {
		return refusal(engineError(err))
	}
	res := protocol.AsideResult{Text: r.Text, Model: r.Model}
	if u := r.Usage; u != nil {
		res.Usage = &protocol.AsideUsageResult{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead}
	}
	return answer(res)
}

// sessionAsideCancel is session.aside.cancel: it cancels the aside asideId of
// this connection that was in flight when its line was read (req.cancels,
// found on the reader), and answers {} once that aside's reply is queued (or
// after asideCancelWait); an id unknown, finished, or another connection's is
// answered {} at once. An asideId that is not session.create's requestId
// shape is -32602, bad_request, as the schema says.
func (c *conn) sessionAsideCancel(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.AsideCancelParams
	if perr := c.params(b, info, req, &p, func() *protocol.Error { return checkAsideID(p.AsideID) }); perr != nil {
		return refusal(perr)
	}
	if h := c.srv.hooks.beforeAsideCancel; h != nil {
		h(p.AsideID)
	}
	if e := req.cancels; e != nil && e.id == p.AsideID {
		e.cancel()
		t := time.NewTimer(asideCancelWait)
		select {
		case <-e.queued:
		case <-t.C:
		}
		t.Stop()
	}
	return answer(protocol.Empty{})
}

// asideEmitter is one aside's notice sender (the package's "The emitter").
type asideEmitter struct {
	c  *conn
	id string
	// interval is the server's: AsideEachDelta (negative) sends each piece
	// as it arrives; otherwise a full notice's worth is sent at once and the
	// rest every interval.
	interval time.Duration
	// abort cancels the aside: a notice could not be queued.
	abort context.CancelFunc
	seq   uint64

	mu sync.Mutex
	// pending is the text not yet taken, piece by piece, and size its bytes;
	// taken is the bytes a flush has taken and not yet handed to the outbox
	// (or dropped). Unsent text is size and taken together, short of at most
	// the one notice being pushed: over says it was once more than
	// AsidePendingMax, and from then on no text is kept or sent.
	pending []string
	size    int
	taken   int
	over    bool

	poke   chan struct{}
	fin    chan struct{}
	exited chan struct{}
}

func newAsideEmitter(c *conn, id string, interval time.Duration, abort context.CancelFunc) *asideEmitter {
	return &asideEmitter{c: c, id: id, interval: interval, abort: abort,
		poke: make(chan struct{}, 1), fin: make(chan struct{}), exited: make(chan struct{})}
}

// delta is the agent's onDelta: it never blocks, and a piece that arrives
// after the emitter has exited is kept and never sent. It pokes the emitter
// only when a poke can send something: every piece under AsideEachDelta, and
// otherwise once a full notice's worth is pending (the interval sends the
// rest).
func (m *asideEmitter) delta(s string) {
	if s == "" {
		return
	}
	m.mu.Lock()
	switch {
	case m.over:
	case m.size+m.taken+len(s) > protocol.AsidePendingMax:
		m.over, m.pending, m.size, m.taken = true, nil, 0, 0
	default:
		m.pending = append(m.pending, s)
		m.size += len(s)
	}
	poke := !m.over && (m.interval <= 0 || m.size >= protocol.AsideNoticeMax)
	m.mu.Unlock()
	if !poke {
		return
	}
	select {
	case m.poke <- struct{}{}:
	default:
	}
}

// finish tells the emitter the answer is complete and waits for it to send
// what is left and exit.
func (m *asideEmitter) finish() {
	close(m.fin)
	<-m.exited
}

// run is the emitter's goroutine: it ends once finish has been called and
// what was pending is queued, once ctx ends, or once a notice could not be
// queued (which cancels the aside).
func (m *asideEmitter) run(ctx context.Context) {
	defer close(m.exited)
	var tick <-chan time.Time
	if m.interval > 0 {
		t := time.NewTicker(m.interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		var whole, final bool
		select {
		case <-ctx.Done():
			return
		case <-m.fin:
			whole, final = true, true
		case <-tick:
			whole = true
		case <-m.poke:
		}
		if !m.flush(ctx, whole) {
			m.abort()
			return
		}
		if final {
			return
		}
	}
}

// flush takes what is pending and queues it as notices: all of it when whole,
// and otherwise only full notices, the rest staying pending. What it takes
// counts as unsent (taken) until each notice is handed to the outbox, so the
// bound holds while a push waits for room. It reports false once a notice
// could not be queued.
func (m *asideEmitter) flush(ctx context.Context, whole bool) bool {
	m.mu.Lock()
	var texts []string
	switch {
	case m.over:
	case m.interval <= 0:
		for _, s := range m.pending {
			texts, _ = splitNotices(texts, s, true)
		}
		m.pending, m.size, m.taken = nil, 0, m.taken+m.size
	default:
		var rest string
		texts, rest = splitNotices(nil, strings.Join(m.pending, ""), whole)
		m.pending, m.size, m.taken = nil, len(rest), m.taken+m.size-len(rest)
		if rest != "" {
			m.pending = []string{rest}
		}
	}
	m.mu.Unlock()
	for _, text := range texts {
		m.mu.Lock()
		over := m.over
		if !over {
			// The notice leaves the count as it is handed to the outbox, in
			// the section that checks over, not after its push: retired after
			// a push that succeeded, it would be counted twice — queued and
			// taken — for as long as the emitter took to get back here, and a
			// delta arriving then could trip the bound on text already on its
			// way (plan 040, C1 review r2). The bound is exact to within the
			// one notice being pushed; a push that fails ends the emitter, so
			// nothing is owed back.
			m.taken -= len(text)
		}
		m.mu.Unlock()
		if over {
			return true
		}
		m.seq++
		line, err := notificationLine(protocol.NotifyAside, protocol.AsideNoticeParams{AsideID: m.id, Seq: m.seq, Text: text})
		if err != nil || m.c.out.push(ctx, line, nil) != nil {
			return false
		}
	}
	return true
}

// splitNotices appends s to out cut into notices of at most AsideNoticeMax
// bytes, each ending on a character boundary: all of s when whole, and
// otherwise only while a full notice's worth is left, which it returns as
// rest.
func splitNotices(out []string, s string, whole bool) ([]string, string) {
	for len(s) >= protocol.AsideNoticeMax || (whole && s != "") {
		n := min(len(s), protocol.AsideNoticeMax)
		if n < len(s) {
			for n > 0 && !utf8.RuneStart(s[n]) {
				n--
			}
			if n == 0 {
				n = protocol.AsideNoticeMax
			}
		}
		out = append(out, s[:n])
		s = s[n:]
	}
	return out, s
}
