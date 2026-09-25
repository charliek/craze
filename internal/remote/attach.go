package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/charliek/craze/internal/protocol"
)

// The attach stream (plan 027 §3.4, §3.14). It folds nothing: it hands up, in
// the order the host wrote them, the attach reply, every event (its seq and
// its body, raw), synchronized, ready, and — after a re-attach that got a
// snapshot — a Restore item; and it turns each reset into what §3.4's table
// says:
//
//	slow_consumer, after readiness   re-attach WITH its cursor {incarnation,
//	                                 last seq handed up}: the host answers from
//	                                 its ring or journal (no item), or refuses
//	                                 the cursor and sends a snapshot (Restore)
//	slow_consumer, before readiness  re-attach when: "ready" with NO cursor: a
//	                                 snapshot after the replay, never a cursor
//	                                 inside the same burst (astra 22)
//	omitted, replay_failed           re-attach with no cursor (Restore)
//	session_replaced                 nothing on this connection: the host
//	                                 closes it, and the reconnect says a fresh
//	                                 hello and attaches anew (Restore)
//	session_closed                   an End item; the stream stops
//
// "Before readiness" is: the first reply said ready: false and no ready has
// arrived since. A reset reason protocol 1 does not name is re-attached with no
// cursor. Re-attaches are bounded at protocol.ReattachesPerEpisode (8) per
// episode — reset, reconnect and retried re-attaches alike — the count starting
// again whenever the stream reaches synchronized; past the bound it hands up an
// Error item and stops. It checks that each event's seq follows the last one
// it handed up: a hole or a duplicate is an Error item, never swallowed.
//
// The stream's work runs on the connection's reader, in wire order — the
// attach replies included, which are the stream's own and never a caller's —
// so a re-attach is written there and its reply read in turn. Items wait in a
// byte-bounded queue for Next; a reader that finds it full waits, which backs
// the socket up, and the host drops a subscription that falls behind
// slow_consumer — the one mechanism, as for a socket nobody reads. A caller
// therefore reads the stream while it waits for a command: the host writes a
// command's reply after its events (§3.6).

// Kind is what an Item is.
type Kind int

const (
	// KindAttached is the first attach's reply (Item.Reply): the snapshot,
	// or none when a cursor was honoured, where the stream continues (after),
	// whether the session is ready, the info document, and why a cursor was
	// refused (reset).
	KindAttached Kind = iota + 1
	// KindEvent is one event: Item.Seq and Item.Body, the record's body
	// verbatim (the lossless event codec's JSON), undecoded.
	KindEvent
	// KindSynchronized says the stream has delivered through its attach's
	// cutoff, Item.Seq.
	KindSynchronized
	// KindReady is the session's start having completed or failed
	// (Item.Ready), once, when the first reply said ready: false: the ready
	// notification, or — when the stream re-attached before one came — made
	// from the re-attach's reply, or its start_failed refusal.
	KindReady
	// KindRestore is a re-attach's reply that carried a snapshot
	// (Item.Reply): the stream now continues from its after, and nothing
	// handed up before it is to be trusted over it.
	KindRestore
	// KindEnd is the session's end (reset{session_closed}, or a re-attach
	// the host answered that the session has closed): the last item.
	KindEnd
	// KindError is the stream stopping for Item.Err: a hole, the re-attach
	// bound, a refused re-attach, the client stopping. The last item.
	KindError
)

var kindNames = map[Kind]string{
	KindAttached: "attached", KindEvent: "event", KindSynchronized: "synchronized", KindReady: "ready",
	KindRestore: "restore", KindEnd: "end", KindError: "error",
}

func (k Kind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Item is one thing the stream hands up. Which fields are set depends on
// Kind.
type Item struct {
	Kind Kind
	// Seq is an event's seq, or the cutoff synchronized marks.
	Seq uint64
	// Body is an event's body, raw.
	Body json.RawMessage
	// Reply is the attach reply of an Attached or a Restore item: its
	// Snapshot is transcript.EncodeSnapshot's JSON, raw.
	Reply *protocol.AttachResult
	// Ready is a Ready item's: the notification's params, or those made from
	// a re-attach's reply.
	Ready *protocol.ReadyParams
	// Err is why an Error item stopped the stream.
	Err error
}

// size is what an item counts against the stream's byte bound.
func (it Item) size() int {
	n := 64 + len(it.Body)
	if it.Reply != nil {
		n += len(it.Reply.Snapshot)
	}
	return n
}

// The stream's own errors, as Error items carry them.
var (
	// ErrStreamGap is an event whose seq does not follow the last one handed
	// up: a hole, or a duplicate.
	ErrStreamGap = errors.New("remote: the stream's seqs do not follow on")
	// ErrReattachBound is a stream that re-attached protocol
	// .ReattachesPerEpisode times without reaching synchronized.
	ErrReattachBound = errors.New("remote: the stream re-attached too many times without synchronizing")
)

// AttachOptions shape an Attach.
type AttachOptions struct {
	// SessionID is the session to attach; "" is the host's one session, as
	// sessions.list names it.
	SessionID string
	// Cursor is where the caller already is, {incarnation, last folded seq}
	// (a persisted ResumeState's); nil asks for a snapshot.
	Cursor *protocol.Cursor
	// When is the first attach's: "" or ready waits for the session's start,
	// now attaches at once and a Ready item follows. Re-attaches always wait
	// ("ready").
	When protocol.When
	// Budget is the subscription's budget, every attach's.
	Budget *protocol.AttachBudget
}

// Stream is a client's attachment to its session (Attach): Next hands its
// items up.
type Stream struct {
	c    *Client
	q    *itemQueue
	opts AttachOptions
	// first carries the first attach's answer to Attach.
	first chan error

	mu sync.Mutex
	// sessionID is the session attached (it changes only when a reconnect
	// finds the host serving another).
	sessionID string
	// sub is the live subscription, "" while there is none: before a reply,
	// after a reset or a lost connection.
	sub string
	// tag numbers the attach requests: a reply to another than the latest
	// is stale. cursor is the cursor the latest carried (nil: none), and
	// params the latest's params, which a retried re-attach sends again.
	tag    int
	cursor *protocol.Cursor
	params protocol.AttachParams
	// inc and last are the stream's position: the incarnation and the seq of
	// the last event handed up (or the attach's after).
	inc  string
	last uint64
	// started says the first attach has been answered; ready says the
	// session is known to be ready; readyOwed says a Ready item is still owed
	// (the first reply said ready: false).
	started   bool
	ready     bool
	readyOwed bool
	// episode is how many re-attaches since the stream last reached
	// synchronized.
	episode int
	// reconnecting is closed once a reconnect's re-attach has been answered
	// (or the stream has stopped): the reconnect sends its commands then.
	reconnecting chan struct{}
	// done says the stream has handed up its last item, or was closed.
	done bool
	// delivered is the cursor of the last item Next handed out (ResumeState).
	delivered *protocol.Cursor
}

// Attach attaches the client to its session (§3.4) and returns the stream,
// whose first item is the attach reply (KindAttached). A client holds one
// stream at a time (SQ14): ErrAlreadyAttached until the last one has ended or
// been closed. A refused attach is its *Error. If the connection goes before
// the reply, the reconnect sends the attach again.
func (c *Client) Attach(ctx context.Context, o AttachOptions) (*Stream, error) {
	if o.SessionID == "" {
		var list protocol.SessionsListResult
		if err := c.Call(ctx, protocol.MethodSessionsList, protocol.SessionsListParams{}, &list); err != nil {
			return nil, err
		}
		if len(list.Sessions) != 1 {
			return nil, fmt.Errorf("remote: the host serves %d sessions: name one in AttachOptions.SessionID", len(list.Sessions))
		}
		o.SessionID = list.Sessions[0].SessionID
	}
	s := &Stream{c: c, q: newItemQueue(c.opts.StreamBytes), opts: o, first: make(chan error, 1), sessionID: o.SessionID}
	c.mu.Lock()
	switch {
	case c.err != nil:
		err := c.err
		c.mu.Unlock()
		return nil, err
	case c.stream != nil:
		c.mu.Unlock()
		return nil, ErrAlreadyAttached
	}
	c.stream = s
	c.mu.Unlock()
	w, err := c.wire(ctx)
	if err != nil {
		s.abandon()
		return nil, err
	}
	s.mu.Lock()
	if s.tag == 0 && !s.done {
		// Not already sent by a reconnect that ran while this waited for
		// the connection.
		p := s.firstParams()
		tag := s.prepareLocked(p)
		s.mu.Unlock()
		s.send(w, tag, p)
	} else {
		s.mu.Unlock()
	}
	select {
	case err := <-s.first:
		if err != nil {
			s.abandon()
			return nil, err
		}
		return s, nil
	case <-ctx.Done():
		s.abandon()
		return nil, ctx.Err()
	}
}

// firstParams is the first attach's params, as the caller gave them.
func (s *Stream) firstParams() protocol.AttachParams {
	return protocol.AttachParams{SessionID: s.sessionID, Cursor: s.opts.Cursor, When: s.opts.When, Budget: s.opts.Budget}
}

// reattachParams is a re-attach's params: with cursor or none, when: ready.
func (s *Stream) reattachParams(cursor *protocol.Cursor) protocol.AttachParams {
	return protocol.AttachParams{SessionID: s.sessionID, Cursor: cursor, When: protocol.WhenReady, Budget: s.opts.Budget}
}

// cursorLocked is the stream's position as a cursor.
func (s *Stream) cursorLocked() *protocol.Cursor {
	return &protocol.Cursor{Incarnation: s.inc, Seq: s.last}
}

// prepareLocked makes p the latest attach request and numbers it; s.mu is
// held.
func (s *Stream) prepareLocked(p protocol.AttachParams) int {
	s.tag++
	s.params = p
	s.cursor = p.Cursor
	s.sub = ""
	return s.tag
}

// send writes attach request tag, of params p, on w. Its reply comes to reply,
// on w's reader. A send that fails is a connection going: the reconnect
// attaches again.
func (s *Stream) send(w *wire, tag int, p protocol.AttachParams) {
	raw, err := paramsJSON(p)
	if err != nil {
		s.fail(err)
		return
	}
	if _, err := s.c.send(w, protocol.MethodSessionAttach, raw, func(resp *protocol.Response, err error) {
		s.reply(w, tag, resp, err)
	}); errors.Is(err, ErrRequestTooLarge) {
		s.fail(err)
	}
}

// countLocked counts one re-attach against the episode's bound, and reports
// false — having stopped the stream, whose Error item it returns — once the
// bound is spent; s.mu is held.
func (s *Stream) countLocked(why string) (Item, bool) {
	if s.episode >= protocol.ReattachesPerEpisode {
		s.stopLocked()
		return Item{Kind: KindError, Err: fmt.Errorf("%w: %d re-attaches in one episode, the last for %s",
			ErrReattachBound, s.episode, why)}, false
	}
	s.episode++
	return Item{}, true
}

// stopLocked marks the stream over: nothing more is handed up after the
// items in hand; s.mu is held.
func (s *Stream) stopLocked() {
	s.done = true
	s.settleReconnectLocked()
}

// settleReconnectLocked lets a waiting reconnect go on; s.mu is held.
func (s *Stream) settleReconnectLocked() {
	if s.reconnecting != nil {
		close(s.reconnecting)
		s.reconnecting = nil
	}
}

// reconnected is the reconnect's re-attach (reconnect.go, step 3), on the new
// connection w before its reader starts: with the stream's cursor after
// resumed: true once the session is ready, and with none otherwise (sid, when
// not "", is the session the host now serves). The channel it returns is
// closed once the re-attach has been answered, or the stream has stopped; nil
// when there is nothing to wait for.
func (s *Stream) reconnected(w *wire, resumed bool, sid string) <-chan struct{} {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil
	}
	if sid != "" {
		s.sessionID = sid
	}
	if it, ok := s.countLocked("a reconnect"); !ok {
		s.mu.Unlock()
		s.finish(it)
		return nil
	}
	var p protocol.AttachParams
	switch {
	case !s.started:
		p = s.firstParams()
	case resumed && s.ready:
		p = s.reattachParams(s.cursorLocked())
	default:
		p = s.reattachParams(nil)
	}
	tag := s.prepareLocked(p)
	ch := make(chan struct{})
	s.reconnecting = ch
	s.mu.Unlock()
	s.send(w, tag, p)
	return ch
}

// reply is an attach reply (tag), on w's reader.
func (s *Stream) reply(w *wire, tag int, resp *protocol.Response, err error) {
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.tag != tag || s.done {
		s.mu.Unlock()
		if resp.Error == nil {
			// An attachment nobody wants any more (the stream was closed or
			// abandoned while its attach was out): detached at once, so the
			// connection's one place is free.
			s.detachStray(w, resp.Result)
		}
		return
	}
	if resp.Error != nil {
		items, retry, ended := s.refusedLocked(newError(resp.Error))
		p := s.params
		var tag int
		if retry {
			tag = s.prepareLocked(p)
		}
		s.mu.Unlock()
		if ended {
			s.c.sessionEnded()
		}
		if retry {
			s.send(w, tag, p)
		}
		s.hand(items)
		return
	}
	var res protocol.AttachResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		s.stopLocked()
		s.mu.Unlock()
		s.detachStray(w, resp.Result)
		s.hand([]Item{{Kind: KindError, Err: fmt.Errorf("remote: decoding the attach reply: %w", err)}})
		return
	}
	var items []Item
	s.sub = res.Subscription
	switch {
	case !s.started:
		s.started = true
		s.inc, s.last = res.After.Incarnation, res.After.Seq
		s.ready, s.readyOwed = res.Ready, !res.Ready
		items = append(items, Item{Kind: KindAttached, Reply: &res})
	case res.Snapshot != nil:
		s.inc, s.last = res.After.Incarnation, res.After.Seq
		items = append(items, Item{Kind: KindRestore, Reply: &res})
	case s.cursor == nil || res.After != *s.cursor:
		// No snapshot, and not the cursor it was given: the stream cannot say
		// where it stands.
		err := fmt.Errorf("%w: a re-attach with cursor %v went on from %v with no snapshot", ErrStreamGap, s.cursor, res.After)
		s.stopLocked()
		s.sub = ""
		sid := s.sessionID
		s.mu.Unlock()
		s.detachThen(w, sid, res.Subscription, Item{Kind: KindError, Err: err})
		return
	}
	if res.Ready && !s.ready {
		s.ready = true
		if s.readyOwed {
			// The ready notification went with the attachment it was owed
			// to: this reply is the news that the session is up.
			s.readyOwed = false
			items = append(items, Item{Kind: KindReady, Ready: &protocol.ReadyParams{Subscription: res.Subscription, Session: res.Session}})
		}
	}
	first := len(items) > 0 && items[0].Kind == KindAttached
	s.settleReconnectLocked()
	s.mu.Unlock()
	s.hand(items)
	if first {
		s.answerFirst(nil)
	}
}

// answerFirst hands Attach its answer; there is only ever one.
func (s *Stream) answerFirst(err error) {
	select {
	case s.first <- err:
	default:
	}
}

// refusedLocked is a refused attach: for the first, the answer Attach
// returns; for a re-attach, the table's — start_failed stops the stream (after
// the Ready item it is owed), a closed session ends it as session_closed does
// (ended), a code protocol.Retry allows sends the same re-attach again
// (counted), and anything else stops it. s.mu is held.
func (s *Stream) refusedLocked(e *Error) (items []Item, retry, ended bool) {
	if !s.started {
		s.stopLocked()
		s.answerFirst(e)
		return nil, false, false
	}
	switch {
	case e.Code == protocol.CodeNotAccepting && e.Reason == protocol.ReasonStartFailed:
		if s.readyOwed {
			s.readyOwed = false
			items = append(items, Item{Kind: KindReady, Ready: &protocol.ReadyParams{StartFailed: true, Err: e.Cause}})
		}
		s.stopLocked()
		return append(items, Item{Kind: KindError, Err: e}), false, false
	case e.Code == protocol.CodeNotAccepting:
		// The session has closed (§3.4: an attach to a closed engine), and
		// the host closes the connection.
		s.stopLocked()
		return []Item{{Kind: KindEnd}}, false, true
	case protocol.Retry(e.Code):
		if it, ok := s.countLocked(fmt.Sprintf("a refusal (%s/%s)", e.Code, e.Reason)); !ok {
			return []Item{it}, false, false
		}
		return nil, true, false
	}
	s.stopLocked()
	return []Item{{Kind: KindError, Err: e}}, false, false
}

// note is one notification for the stream, on w's reader. One for another
// subscription than the live one is ignored.
func (s *Stream) note(w *wire, method string, params json.RawMessage) {
	var sub struct {
		Subscription string `json:"subscription"`
	}
	if json.Unmarshal(params, &sub) != nil {
		return
	}
	s.mu.Lock()
	if s.done || s.sub == "" || sub.Subscription != s.sub {
		s.mu.Unlock()
		return
	}
	var items []Item
	switch method {
	case protocol.NotifyEvent:
		var p protocol.EventParams
		var bad error
		switch err := json.Unmarshal(params, &p); {
		case err != nil:
			bad = fmt.Errorf("remote: decoding an event: %w", err)
		case p.Seq != s.last+1:
			bad = fmt.Errorf("%w: event %d after %d", ErrStreamGap, p.Seq, s.last)
		}
		if bad != nil {
			// Never swallowed: the stream stops, its attachment detached.
			s.stopLocked()
			s.sub = ""
			sid := s.sessionID
			s.mu.Unlock()
			s.detachThen(w, sid, sub.Subscription, Item{Kind: KindError, Err: bad})
			return
		}
		s.last = p.Seq
		items = append(items, Item{Kind: KindEvent, Seq: p.Seq, Body: p.Event})
	case protocol.NotifySynchronized:
		var p protocol.SynchronizedParams
		_ = json.Unmarshal(params, &p)
		s.episode = 0
		items = append(items, Item{Kind: KindSynchronized, Seq: p.Seq})
	case protocol.NotifyReady:
		var p protocol.ReadyParams
		_ = json.Unmarshal(params, &p)
		s.ready = true
		if s.readyOwed {
			s.readyOwed = false
			items = append(items, Item{Kind: KindReady, Ready: &p})
		}
	case protocol.NotifyReset:
		var p protocol.ResetParams
		_ = json.Unmarshal(params, &p)
		s.resetLocked(w, p.Reason)
		return
	}
	s.mu.Unlock()
	s.hand(items)
}

// resetLocked is the live subscription's reset (§3.4's table, above); s.mu is
// held, and released here.
func (s *Stream) resetLocked(w *wire, reason protocol.ResetReason) {
	s.sub = ""
	var cursor *protocol.Cursor
	switch reason {
	case protocol.ResetSessionClosed:
		s.stopLocked()
		s.mu.Unlock()
		s.c.sessionEnded()
		s.hand([]Item{{Kind: KindEnd}})
		return
	case protocol.ResetSessionReplaced:
		// The host closes this connection; the reconnect's hello is a fresh
		// one and its attach a snapshot.
		s.mu.Unlock()
		s.c.voidTokenForReplacement()
		return
	case protocol.ResetSlowConsumer:
		if s.ready {
			cursor = s.cursorLocked()
		}
	}
	if it, ok := s.countLocked(string(reason)); !ok {
		s.mu.Unlock()
		s.hand([]Item{it})
		return
	}
	p := s.reattachParams(cursor)
	tag := s.prepareLocked(p)
	s.mu.Unlock()
	s.send(w, tag, p)
}

// detachThen ends the host's attachment sub of a stream that stopped while it
// was live, and only then hands up the stream's last item it: once the host
// has answered the detach — its terminal acknowledgement (§3.7) — or the
// connection has gone, so a caller that attaches again on learning of the stop
// never races the detach for the connection's one place.
func (s *Stream) detachThen(w *wire, sid, sub string, it Item) {
	raw, err := paramsJSON(protocol.DetachParams{SessionID: sid, Subscription: sub})
	if err == nil {
		_, err = s.c.send(w, protocol.MethodSessionDetach, raw, func(*protocol.Response, error) { s.finish(it) })
	}
	if err != nil {
		s.finish(it)
	}
}

// detachStray detaches the attachment an unwanted reply made, best effort.
func (s *Stream) detachStray(w *wire, result json.RawMessage) {
	var res protocol.AttachResult
	if json.Unmarshal(result, &res) != nil || res.Subscription == "" {
		return
	}
	s.mu.Lock()
	sid := s.sessionID
	s.mu.Unlock()
	raw, err := paramsJSON(protocol.DetachParams{SessionID: sid, Subscription: res.Subscription})
	if err == nil {
		_, _ = s.c.send(w, protocol.MethodSessionDetach, raw, nil)
	}
}

// hand queues items for Next, in order: a terminal one (End, Error) last of
// all and never waiting for room, and the stream is dropped from its client.
func (s *Stream) hand(items []Item) {
	for _, it := range items {
		if it.Kind == KindEnd || it.Kind == KindError {
			s.finish(it)
			return
		}
		s.q.push(it)
	}
}

// finish hands up the stream's last item and lets the client attach again.
func (s *Stream) finish(it Item) {
	s.q.finish(&it)
	s.c.dropStream(s)
}

// fail stops the stream with err: the client stopped, or a request could
// not be made.
func (s *Stream) fail(err error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	started := s.started
	s.stopLocked()
	s.mu.Unlock()
	if !started {
		s.answerFirst(err)
	}
	s.finish(Item{Kind: KindError, Err: err})
}

// abandon drops a stream whose Attach returned without it.
func (s *Stream) abandon() {
	s.mu.Lock()
	s.stopLocked()
	s.mu.Unlock()
	s.q.drop()
	s.c.dropStream(s)
}

// Next is the stream's next item, waiting for one. After the last (End or
// Error) — or once the stream is closed — it is ErrStreamClosed.
func (s *Stream) Next(ctx context.Context) (Item, error) {
	it, err := s.q.pop(ctx)
	if err != nil {
		return Item{}, err
	}
	s.mu.Lock()
	switch it.Kind {
	case KindAttached, KindRestore:
		after := it.Reply.After
		s.delivered = &after
	case KindEvent:
		if s.delivered != nil {
			s.delivered = &protocol.Cursor{Incarnation: s.delivered.Incarnation, Seq: it.Seq}
		}
	}
	s.mu.Unlock()
	return it, nil
}

// Cursor is the stream's cursor as far as Next has handed it out — the
// {incarnation, seq} of the last event, or the last attach's after — for a
// caller to persist (ResumeState); nil before the attach reply.
func (s *Stream) Cursor() *protocol.Cursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delivered == nil {
		return nil
	}
	c := *s.delivered
	return &c
}

// SessionID is the session the stream is attached to.
func (s *Stream) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// Close detaches the stream (session.detach, whose reply is the attachment's
// terminal acknowledgement) and drops what Next had not handed out. A stream
// already over closes with nothing sent.
func (s *Stream) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		s.q.drop()
		s.c.dropStream(s)
		return nil
	}
	sub, sid := s.sub, s.sessionID
	s.sub = ""
	s.stopLocked()
	s.mu.Unlock()
	s.q.drop()
	s.c.dropStream(s)
	if sub == "" {
		return nil
	}
	return s.c.Call(ctx, protocol.MethodSessionDetach, protocol.DetachParams{SessionID: sid, Subscription: sub}, nil)
}

// dropStream forgets s as the client's stream.
func (c *Client) dropStream(s *Stream) {
	c.mu.Lock()
	if c.stream == s {
		c.stream = nil
	}
	c.mu.Unlock()
}

// sessionEnded is the stream having seen reset{session_closed}: the host
// closes the connection, which is not redialled.
func (c *Client) sessionEnded() {
	c.mu.Lock()
	c.ended = true
	c.mu.Unlock()
}

// voidTokenForReplacement is the stream having seen reset{session_replaced}:
// the next hello is a fresh one (the old token is void).
func (c *Client) voidTokenForReplacement() {
	c.mu.Lock()
	c.voidToken = true
	c.mu.Unlock()
}

// ------------------------------------------------------------------ queue

// itemQueue holds a stream's items for Next, bounded in bytes: a push that
// finds no room waits, and one item always fits in an empty queue. finish
// appends the last item whatever the bound and closes the queue to pushes;
// drop empties it and closes it to both.
type itemQueue struct {
	mu      sync.Mutex
	items   []Item
	bytes   int
	max     int
	closed  bool
	dropped bool
	changed chan struct{}
}

func newItemQueue(max int) *itemQueue {
	return &itemQueue{max: max, changed: make(chan struct{})}
}

func (q *itemQueue) changedLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

// push queues it, waiting for room; false once the queue is closed.
func (q *itemQueue) push(it Item) bool {
	n := it.size()
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return false
		}
		if len(q.items) == 0 || q.bytes+n <= q.max {
			q.items = append(q.items, it)
			q.bytes += n
			q.changedLocked()
			q.mu.Unlock()
			return true
		}
		ch := q.changed
		q.mu.Unlock()
		<-ch
	}
}

// finish queues the last item, if any, and closes the queue to pushes.
func (q *itemQueue) finish(it *Item) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if it != nil {
		q.items = append(q.items, *it)
		q.bytes += it.size()
	}
	q.closed = true
	q.changedLocked()
}

// drop empties the queue and closes it.
func (q *itemQueue) drop() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items, q.bytes = nil, 0
	q.closed, q.dropped = true, true
	q.changedLocked()
}

// pop is the next item, waiting for one; ErrStreamClosed once the queue is
// closed and empty, or dropped.
func (q *itemQueue) pop(ctx context.Context) (Item, error) {
	for {
		q.mu.Lock()
		if q.dropped {
			q.mu.Unlock()
			return Item{}, ErrStreamClosed
		}
		if len(q.items) > 0 {
			it := q.items[0]
			q.items[0] = Item{}
			q.items = q.items[1:]
			q.bytes -= it.size()
			q.changedLocked()
			q.mu.Unlock()
			return it, nil
		}
		if q.closed {
			q.mu.Unlock()
			return Item{}, ErrStreamClosed
		}
		ch := q.changed
		q.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return Item{}, ctx.Err()
		}
	}
}
