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
//	                                 last seq it holds}: the host answers from
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
// been queued since. A reset reason protocol 1 does not name is re-attached
// with no cursor. Re-attaches are bounded at protocol.ReattachesPerEpisode (8)
// per episode — reset, reconnect, fallen-behind and retried re-attaches alike
// — the count starting again whenever the stream reaches synchronized; past
// the bound it hands up an Error item and stops. It checks that each event's
// seq follows the last one it holds, and that synchronized's is the last one
// it holds: a hole or a duplicate is an Error item, never swallowed.
//
// THE CURSOR (X18 4). The stream's position — {inc, last} — is the last event
// it holds, received and queued for Next (which hands them up in order), or
// the last attach's after: a re-attach with a cursor carries it, so nothing
// is replayed twice and nothing skipped. Stream.Cursor, which ResumeState
// persists, is the last one Next has handed OUT: what a caller has folded.
//
// A LOCAL SLOW CONSUMER (X18 8, X21). The stream's work runs on the
// connection's reader, in wire order — the attach replies included, which are
// the stream's own and never a caller's — and the reader never waits for Next
// and never writes: a re-attach or a detach it decides on is posted to the
// connection's writer (Client.post), in order, and its reply read in turn.
// Items go to a byte-bounded queue (Options.StreamBytes); an item that finds
// no room is not queued, and the stream FALLS BEHIND: it queues nothing more
// of that subscription's, detaches it on its own connection, and once the host
// has answered the detach and the caller has drained the queue (Next), it
// re-attaches as after the host's reset{slow_consumer} — with its cursor after
// readiness, when: "ready" with none before (and with none for a Restore that
// found no room, whose snapshot is the only way on). That re-attach counts in
// the episode. So a caller that stops reading while it waits for a command
// still gets the command's reply — even while a caller's write holds the
// connection — and a reset or the connection's end is still read; a caller
// that reads again gets every event, in order, from the re-attach. A Restore
// and the Ready it brings are queued as one (X21): an owed Ready never
// competes with its Restore for room.

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

	// cost is what the item counts in its queue (size, taken as it is
	// queued).
	cost int
}

// What an item counts besides its payloads: itemOverhead for its fields and
// its place in the queue; replyOverhead and readyOverhead for what an attach
// reply's and a ready's encoding adds to their strings and documents (member
// names, ready, after's seq).
const (
	itemOverhead  = 64
	replyOverhead = 128
	readyOverhead = 96
)

// size is what an item counts against the stream's byte bound: an honest
// estimate of every payload it retains, as encoded — an event's body, a
// reply's snapshot and info document (catalogs and all), a ready's, an
// error's text, result and cause (X18 8).
func (it Item) size() int {
	n := itemOverhead + len(it.Body)
	if r := it.Reply; r != nil {
		n += replyOverhead + len(r.Subscription) + len(r.After.Incarnation) + len(r.Reset) + len(r.Snapshot) + infoSize(&r.Session)
	}
	if r := it.Ready; r != nil {
		n += readyOverhead + len(r.Subscription) + len(r.Err) + infoSize(&r.Session)
	}
	if it.Err != nil {
		n += len(it.Err.Error())
		var e *Error
		if errors.As(it.Err, &e) {
			n += len(e.Result) + len(e.Cause)
		}
	}
	return n
}

// infoSize is an info document's size, encoded.
func infoSize(s *protocol.SessionInfo) int {
	b, err := json.Marshal(s)
	if err != nil {
		return 0
	}
	return len(b)
}

// The stream's own errors, as Error items carry them.
var (
	// ErrStreamGap is an event whose seq does not follow the last one the
	// stream holds, or a synchronized that is not at it: a hole, or a
	// duplicate.
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
	// w is the connection the stream's attach requests go on: the latest's,
	// or the one a reconnect is adopting.
	w *wire
	// sub is the live subscription, "" while there is none: before a reply,
	// after a reset, a lost connection or a fall behind; subW is the
	// connection it lives on, the one its detach goes to and no other (X18 6).
	sub  string
	subW *wire
	// tag numbers the attach requests: a reply to another than the latest
	// is stale. cursor is the cursor the latest carried (nil: none), and
	// params the latest's params, which a retried re-attach sends again.
	tag    int
	cursor *protocol.Cursor
	params protocol.AttachParams
	// inc and last are the stream's position: the incarnation and the seq of
	// the last event it holds (or the attach's after).
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
	// (or the stream has stopped or fallen behind): the reconnect sends its
	// commands then.
	reconnecting chan struct{}
	// done says the stream has handed up its last item, or was closed.
	done bool
	// delivered is the cursor of the last item Next handed out (ResumeState).
	delivered *protocol.Cursor
	// behind is the re-attach a fallen-behind stream owes, nil when none.
	behind *behind
}

// behind is a stream that fell behind its caller (a local slow consumer): the
// re-attach it owes once its caller has drained.
type behind struct {
	// cursor says the re-attach may carry the stream's cursor (once ready); a
	// Restore that found no room, or a resume loss meanwhile, says not.
	cursor bool
	// need is the size of the item that found no room: the re-attach waits
	// until one like it would fit.
	need int
	// detaching says the abandoned subscription's detach is unanswered: the
	// host's one place on the connection is not free yet.
	detaching bool
}

// Attach attaches the client to its session (§3.4) and returns the stream,
// whose first item is the attach reply (KindAttached). A client holds one
// stream at a time (SQ14): ErrAlreadyAttached until the last one has ended or
// been closed. A refused attach is its *Error, and a malformed reply an error
// too (the connection, whose host broke the protocol, is dropped). If the
// connection goes before the reply, the reconnect sends the attach again.
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
		tag := s.prepareLocked(w, p)
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

// prepareLocked makes p, to go on w, the latest attach request and numbers
// it; s.mu is held.
func (s *Stream) prepareLocked(w *wire, p protocol.AttachParams) int {
	s.tag++
	s.params = p
	s.cursor = p.Cursor
	s.sub, s.subW = "", nil
	s.w = w
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
	}, nil); errors.Is(err, ErrRequestTooLarge) {
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
// not "", is the session the host now serves). A stream that fell behind is
// re-attached here only if its caller has drained; otherwise its re-attach
// waits for that (kick), on w. The channel it returns is closed once the
// re-attach has been answered, or the stream has stopped; nil when there is
// nothing to wait for.
func (s *Stream) reconnected(w *wire, resumed bool, sid string) <-chan struct{} {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil
	}
	if sid != "" {
		s.sessionID = sid
	}
	s.w = w
	s.sub, s.subW = "", nil
	withCursor := resumed && s.ready
	if b := s.behind; b != nil {
		// The subscription it abandoned went with its connection, and
		// after a resume loss its cursor is no longer the way on.
		b.detaching = false
		b.cursor = b.cursor && resumed
		if !s.q.roomFor(b.need) {
			s.mu.Unlock()
			return nil
		}
		withCursor = withCursor && b.cursor
		s.behind = nil
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
	case withCursor:
		p = s.reattachParams(s.cursorLocked())
	default:
		p = s.reattachParams(nil)
	}
	tag := s.prepareLocked(w, p)
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
			tag = s.prepareLocked(w, p)
		}
		s.mu.Unlock()
		if ended {
			s.c.sessionEnded()
		}
		if retry {
			s.c.post(w, func() { s.send(w, tag, p) })
		}
		if len(items) > 0 {
			s.finish(items...)
		}
		return
	}
	res, err := decodeAttach(resp.Result)
	if err != nil {
		// The host broke the protocol (X18 7): the connection is dropped —
		// the host's attachment, which the client cannot name, goes with it
		// — and Attach, when this was its reply, is answered; a re-attach is
		// made again by the reconnect.
		first := !s.started
		if first {
			s.stopLocked()
		}
		s.mu.Unlock()
		_ = w.nc.Close()
		if first {
			s.answerFirst(fmt.Errorf("remote: the attach reply: %w", err))
		}
		return
	}
	if res.Snapshot == nil && (s.cursor == nil || res.After != *s.cursor) {
		// No snapshot, and not the cursor it was given: the stream cannot say
		// where it stands. An attach with no cursor — the first too — must
		// carry a snapshot (X21).
		err := fmt.Errorf("%w: an attach with cursor %v went on from %v with no snapshot", ErrStreamGap, s.cursor, res.After)
		first := !s.started
		s.stopLocked()
		sid := s.sessionID
		s.mu.Unlock()
		if first {
			// Attach is answered with it, and the connection dropped: its
			// host holds an attachment the client will not use (X20).
			_ = w.nc.Close()
			s.answerFirst(err)
			return
		}
		s.detachThen(w, sid, res.Subscription, Item{Kind: KindError, Err: err})
		return
	}
	switch {
	case !s.started:
		// The first item: the queue is empty, and one item always fits.
		s.q.offer(Item{Kind: KindAttached, Reply: res})
		s.started = true
		s.inc, s.last = res.After.Incarnation, res.After.Seq
		s.ready, s.readyOwed = res.Ready, !res.Ready
		s.sub, s.subW = res.Subscription, w
		s.settleReconnectLocked()
		s.mu.Unlock()
		s.answerFirst(nil)
		return
	case res.Snapshot != nil:
		s.sub, s.subW = res.Subscription, w
		items := []Item{{Kind: KindRestore, Reply: res}}
		owed := res.Ready && !s.ready && s.readyOwed
		if owed {
			// The ready notification went with the attachment it was owed
			// to: this reply is the news that the session is up, and it is
			// queued with the Restore, as one — never left to find room of
			// its own after it (X21).
			items = append(items, Item{Kind: KindReady, Ready: &protocol.ReadyParams{Subscription: res.Subscription, Session: res.Session}})
		}
		if !s.q.offer(items...) {
			// A snapshot is the only way on from here: the re-attach, once
			// the caller has drained, carries no cursor (and its reply the
			// Ready still owed).
			need := 0
			for _, it := range items {
				need += it.size()
			}
			s.fellBehindLocked(false, need)
			return
		}
		s.inc, s.last = res.After.Incarnation, res.After.Seq
		if owed {
			s.readyOwed = false
		}
	default:
		s.sub, s.subW = res.Subscription, w
	}
	if res.Ready && !s.ready {
		if s.readyOwed {
			// A cursor honoured, and the ready notification gone with the
			// attachment it was owed to: this reply is the news that the
			// session is up.
			it := Item{Kind: KindReady, Ready: &protocol.ReadyParams{Subscription: res.Subscription, Session: res.Session}}
			if !s.q.offer(it) {
				// Still owed, and still before readiness as far as the
				// caller knows: the re-attach's reply will carry it.
				s.fellBehindLocked(false, it.size())
				return
			}
			s.readyOwed = false
		}
		s.ready = true
	}
	s.settleReconnectLocked()
	s.mu.Unlock()
}

// decodeAttach is an attach reply's result, or why it is not one: it must
// name its subscription and where the stream continues.
func decodeAttach(raw json.RawMessage) (*protocol.AttachResult, error) {
	var res protocol.AttachResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	if res.Subscription == "" || res.After.Incarnation == "" {
		return nil, errors.New("no subscription, or no after")
	}
	return &res, nil
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
// (counted), and anything else stops it. The items it returns are the
// stream's last. s.mu is held.
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

// notice is one notification of the stream's, decoded.
type notice struct {
	method string
	sub    string
	seq    uint64
	body   json.RawMessage
	ready  *protocol.ReadyParams
	reason protocol.ResetReason
}

// decodeNotice decodes a notification protocol 1 names; params that do not
// decode as its own break the protocol (errMalformed).
func decodeNotice(method string, params json.RawMessage) (notice, error) {
	n := notice{method: method}
	var err error
	switch method {
	case protocol.NotifyEvent:
		var p protocol.EventParams
		err = json.Unmarshal(params, &p)
		n.sub, n.seq, n.body = p.Subscription, p.Seq, p.Event
	case protocol.NotifySynchronized:
		var p protocol.SynchronizedParams
		err = json.Unmarshal(params, &p)
		n.sub, n.seq = p.Subscription, p.Seq
	case protocol.NotifyReady:
		var p protocol.ReadyParams
		err = json.Unmarshal(params, &p)
		n.sub, n.ready = p.Subscription, &p
	case protocol.NotifyReset:
		var p protocol.ResetParams
		err = json.Unmarshal(params, &p)
		n.sub, n.reason = p.Subscription, p.Reason
	}
	if err != nil {
		return notice{}, fmt.Errorf("%w: %s's params: %w", errMalformed, method, err)
	}
	return n, nil
}

// note is one notification for the stream, on w's reader. One for another
// subscription than the live one is ignored.
func (s *Stream) note(w *wire, n notice) {
	s.mu.Lock()
	if s.done || s.sub == "" || n.sub != s.sub {
		s.mu.Unlock()
		return
	}
	switch n.method {
	case protocol.NotifyEvent:
		if n.seq != s.last+1 {
			s.brokenLocked(w, fmt.Errorf("%w: event %d after %d", ErrStreamGap, n.seq, s.last))
			return
		}
		it := Item{Kind: KindEvent, Seq: n.seq, Body: n.body}
		if !s.q.offer(it) {
			s.fellBehindLocked(s.ready, it.size())
			return
		}
		s.last = n.seq
	case protocol.NotifySynchronized:
		if n.seq != s.last {
			// The cutoff is the last event of the attachment's replay: one
			// the stream does not hold is a hole (X18 4).
			s.brokenLocked(w, fmt.Errorf("%w: synchronized at %d, the last event held %d", ErrStreamGap, n.seq, s.last))
			return
		}
		it := Item{Kind: KindSynchronized, Seq: n.seq}
		if !s.q.offer(it) {
			s.fellBehindLocked(s.ready, it.size())
			return
		}
		s.episode = 0
	case protocol.NotifyReady:
		if s.readyOwed {
			it := Item{Kind: KindReady, Ready: n.ready}
			if !s.q.offer(it) {
				s.fellBehindLocked(false, it.size())
				return
			}
			s.readyOwed = false
		}
		s.ready = true
	case protocol.NotifyReset:
		s.resetLocked(w, n.reason)
		return
	}
	s.mu.Unlock()
}

// brokenLocked stops the stream for err — never swallowed — its live
// attachment detached on w before the Error item is handed up; s.mu is held,
// and released here.
func (s *Stream) brokenLocked(w *wire, err error) {
	sub, sid := s.sub, s.sessionID
	s.stopLocked()
	s.sub, s.subW = "", nil
	s.mu.Unlock()
	s.detachThen(w, sid, sub, Item{Kind: KindError, Err: err})
}

// resetLocked is the live subscription's reset (§3.4's table, above); s.mu is
// held, and released here.
func (s *Stream) resetLocked(w *wire, reason protocol.ResetReason) {
	s.sub, s.subW = "", nil
	var cursor *protocol.Cursor
	switch reason {
	case protocol.ResetSessionClosed:
		s.stopLocked()
		s.mu.Unlock()
		s.c.sessionEnded()
		s.finish(Item{Kind: KindEnd})
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
		s.finish(it)
		return
	}
	p := s.reattachParams(cursor)
	tag := s.prepareLocked(w, p)
	s.mu.Unlock()
	s.c.post(w, func() { s.send(w, tag, p) })
}

// fellBehindLocked is an item (or a Restore and its Ready), need bytes, having
// found no room: the caller has stopped reading (X18 8). The stream queues
// nothing more of its live subscription's, detaches it on the connection it
// lives on — posted, since this is that connection's reader (X21) — and owes a
// re-attach (kick), with its cursor if cursor says it may; s.mu is held, and
// released here.
func (s *Stream) fellBehindLocked(cursor bool, need int) {
	sub, sw, sid := s.sub, s.subW, s.sessionID
	s.sub, s.subW = "", nil
	b := &behind{cursor: cursor, need: need, detaching: sub != "" && sw != nil}
	s.behind = b
	// A reconnect waiting on this re-attach goes on: the stream re-attaches
	// once its caller has drained, not before.
	s.settleReconnectLocked()
	s.mu.Unlock()
	if !b.detaching {
		return
	}
	// answered runs on sw's reader (the detach's reply) or on its writer (the
	// send failed); the re-attach it may make is written by the writer.
	answered := func(ok bool) {
		s.mu.Lock()
		if s.behind == b {
			b.detaching = false
		}
		s.mu.Unlock()
		if ok {
			s.c.post(sw, s.kick)
		}
	}
	// Any answer frees the host's place: a detach of an attachment that has
	// already ended on its own is answered after its reset. A connection that
	// goes first takes the attachment with it, and the reconnect re-attaches.
	s.c.post(sw, func() {
		raw, err := paramsJSON(protocol.DetachParams{SessionID: sid, Subscription: sub})
		if err == nil {
			_, err = s.c.send(sw, protocol.MethodSessionDetach, raw, func(_ *protocol.Response, err error) { answered(err == nil) }, nil)
		}
		if err != nil {
			answered(false)
		}
	})
}

// kick makes the re-attach a fallen-behind stream owes, once the host has
// answered its detach and its caller has drained the queue (itemQueue
// .roomFor), on the connection its attach requests go on — counted in the
// episode, as a reset's is. It is called whenever one of those may have
// become true: Next took an item, the detach was answered; a reconnect checks
// for itself.
func (s *Stream) kick() {
	s.mu.Lock()
	b := s.behind
	if s.done || b == nil || b.detaching || s.w == nil || !s.q.roomFor(b.need) {
		s.mu.Unlock()
		return
	}
	s.behind = nil
	if it, ok := s.countLocked("a caller that fell behind"); !ok {
		s.mu.Unlock()
		s.finish(it)
		return
	}
	var cursor *protocol.Cursor
	if b.cursor && s.ready {
		cursor = s.cursorLocked()
	}
	p := s.reattachParams(cursor)
	w := s.w
	tag := s.prepareLocked(w, p)
	s.mu.Unlock()
	s.send(w, tag, p)
}

// detachThen ends the host's attachment sub of a stream that stopped while it
// was live, and only then hands up the stream's last item it: once the host
// has answered the detach — its terminal acknowledgement (§3.7) — or the
// connection has gone, so a caller that attaches again on learning of the stop
// never races the detach for the connection's one place. It runs on w's
// reader: the detach is posted (X21).
func (s *Stream) detachThen(w *wire, sid, sub string, it Item) {
	s.c.post(w, func() {
		raw, err := paramsJSON(protocol.DetachParams{SessionID: sid, Subscription: sub})
		if err == nil {
			_, err = s.c.send(w, protocol.MethodSessionDetach, raw, func(*protocol.Response, error) { s.finish(it) }, nil)
		}
		if err != nil {
			s.finish(it)
		}
	})
}

// detachStray detaches the attachment an unwanted reply made, best effort. It
// runs on w's reader: the detach is posted (X21).
func (s *Stream) detachStray(w *wire, result json.RawMessage) {
	var res protocol.AttachResult
	if json.Unmarshal(result, &res) != nil || res.Subscription == "" {
		return
	}
	s.mu.Lock()
	sid := s.sessionID
	s.mu.Unlock()
	s.c.post(w, func() {
		raw, err := paramsJSON(protocol.DetachParams{SessionID: sid, Subscription: res.Subscription})
		if err == nil {
			_, _ = s.c.send(w, protocol.MethodSessionDetach, raw, nil, nil)
		}
	})
}

// finish hands up the stream's last items, whatever the bound, closes its
// queue to anything more, and lets the client attach again. A queue closed
// already takes none of them.
func (s *Stream) finish(items ...Item) {
	s.q.finish(items...)
	s.c.dropStream(s)
}

// fail stops the stream with err: the client stopped, or a request could not
// be made. It closes the queue whatever the stream's state (X18 8): a stream
// that had stopped but not yet handed up its last item hands up this one.
func (s *Stream) fail(err error) {
	s.mu.Lock()
	first := !s.done && !s.started
	s.stopLocked()
	s.mu.Unlock()
	if first {
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
	behind := s.behind != nil
	s.mu.Unlock()
	if behind {
		s.kick()
	}
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
// terminal acknowledgement) and drops what Next had not handed out. The
// detach goes to the connection the subscription lives on and nowhere else
// (X18 6): a subscription whose connection has gone went with it, and nothing
// is sent — never on a later connection, whose subscription ids start again
// and may name another stream's. A stream already over closes with nothing
// sent.
func (s *Stream) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		s.q.drop()
		s.c.dropStream(s)
		return nil
	}
	sub, sw, sid := s.sub, s.subW, s.sessionID
	s.sub, s.subW = "", nil
	s.stopLocked()
	s.mu.Unlock()
	s.q.drop()
	s.c.dropStream(s)
	if h := s.c.hooks.closing; h != nil {
		h()
	}
	if sub == "" || sw == nil {
		return nil
	}
	err := s.c.callOn(ctx, sw, protocol.MethodSessionDetach, protocol.DetachParams{SessionID: sid, Subscription: sub}, nil)
	if errors.Is(err, ErrConnectionLost) {
		// Gone with its connection, or going: nothing is left to detach.
		return nil
	}
	return err
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

// itemQueue holds a stream's items for Next, bounded in bytes (Item.size),
// and never makes its writer wait: offer takes an item (or a step of several)
// only if there is room — one always fits in an empty queue — and says so.
// finish appends the last items whatever the bound and closes the queue to
// more; drop empties it and closes it to both.
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

// offer queues items — one step: all of them or none — if there is room for
// them all, and never waits: false says there was none. An empty queue always
// has room for one step. A closed queue takes nothing and says true — its
// stream hands up nothing more.
func (q *itemQueue) offer(items ...Item) bool {
	cost := 0
	for i := range items {
		items[i].cost = items[i].size()
		cost += items[i].cost
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return true
	}
	if len(q.items) > 0 && q.bytes+cost > q.max {
		return false
	}
	q.items = append(q.items, items...)
	q.bytes += cost
	q.changedLocked()
	return true
}

// roomFor says a fallen-behind stream's caller has drained enough for it to
// re-attach: the queue is empty, or at most half full with room for an item of
// need bytes — so the re-attach is not at once behind again.
func (q *itemQueue) roomFor(need int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed || len(q.items) == 0 || (q.bytes <= q.max/2 && q.bytes+need <= q.max)
}

// finish queues the last items, whatever the bound, and closes the queue to
// more; a closed queue takes none.
func (q *itemQueue) finish(items ...Item) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	for _, it := range items {
		it.cost = it.size()
		q.items = append(q.items, it)
		q.bytes += it.cost
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

// queued is the bytes the queue counts now.
func (q *itemQueue) queued() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bytes
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
			q.bytes -= it.cost
			q.changedLocked()
			q.mu.Unlock()
			it.cost = 0
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
