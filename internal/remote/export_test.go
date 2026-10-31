package remote

import (
	"context"
	"sync"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
)

// SetBrokenPipeDrain shortens brokenPipeDrain, until the test ends.
func SetBrokenPipeDrain(d time.Duration) (restore func()) {
	old := brokenPipeDrain
	brokenPipeDrain = d
	return func() { brokenPipeDrain = old }
}

// Waiting is how many commands have a caller waiting on them: sent and
// unanswered, or held for a connection.
func (c *Client) Waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.cmds)
}

// TestHooks are the client's schedule points for tests outside the package
// (remote_test): the hooks of the same names (client.go).
type TestHooks struct {
	// Registered runs on Command once the command is registered, before its
	// caller's first attempt; Attempted once that attempt has returned.
	Registered func(commandID string)
	Attempted  func(commandID string)
	// Claimed runs on the goroutine making an attempt once it has claimed
	// it, before a byte of it is written.
	Claimed func(commandID string)
	// Replied runs on the reader once a command's reply has been handled;
	// Retrying on Command once it has taken an answer it retries by code,
	// before its backoff.
	Replied  func(commandID string)
	Retrying func(commandID string)
	// Resent runs on a reconnect once its publication has made an attempt of
	// a command it held, before it looks for more; Published once it has
	// published the connection it adopted, after its resends.
	Resent    func(commandID string)
	Published func()
	// Closing runs on Stream.Close once it has taken the subscription it
	// detaches, before it sends anything.
	Closing func()
	// Starting runs on a reconnect once its re-attach, if the stream owes
	// one, is written, before the connection's reader starts.
	Starting func()
	// Pausing runs on a reconnect once its re-attach is written and the
	// connection's reader started, before the episode's clock stops for the
	// re-attach's reply; spent is closed once the episode is spent.
	Pausing func(spent <-chan struct{})
	// Posted runs on a connection's reader once it has handed a write to the
	// connection's writer; PostRan on the writer once it has run one.
	Posted  func()
	PostRan func()
	// Waiting runs on a call once it waits for a connection; Answered once
	// its reply has reached it, before it is decoded.
	Waiting  func()
	Answered func(method string)
	// Entered runs on a Session command once it has taken its binding,
	// before the command is registered; Attached on a Session's Attach once
	// the client's attach has returned, before the Session holds the stream.
	Entered  func(commandID string)
	Attached func()
	// Broken runs on a Session's detach of a stream a decode failure ended,
	// before the detach is made.
	Broken func()
	// Delivering runs on a connection's reader once an aside notification
	// has found its aside's sink, before the sink is asked to hand it over.
	Delivering func(asideID string, seq uint64)
}

// DialForTest is Dial with h in place.
func DialForTest(ctx context.Context, path string, o Options, h TestHooks) (*Client, error) {
	return dial(ctx, path, o, h.hooks())
}

// hooks is h as the client's own.
func (h TestHooks) hooks() hooks {
	return hooks{
		registered: h.Registered,
		attempted:  h.Attempted,
		claimed:    h.Claimed,
		replied:    h.Replied,
		retrying:   h.Retrying,
		resent:     h.Resent,
		published:  h.Published,
		closing:    h.Closing,
		starting:   h.Starting,
		pausing:    h.Pausing,
		posted:     h.Posted,
		postRan:    h.PostRan,
		waiting:    h.Waiting,
		answered:   h.Answered,
		entered:    h.Entered,
		attached:   h.Attached,
		broken:     h.Broken,
		delivering: h.Delivering,
	}
}

// AsideCancelWait is how long Session.Aside, its ctx ended, waits for the
// session.aside.cancel it sends (asideCancelWait).
const AsideCancelWait = asideCancelWait

// HoldWrites takes the write lock of the connection calls go to now, as a
// request mid-write to a peer that has stopped reading holds it: nothing is
// written on that connection until release is called. release may be called
// more than once.
func (c *Client) HoldWrites() (release func()) {
	c.mu.Lock()
	w := c.cur
	c.mu.Unlock()
	w.wmu.Lock()
	var once sync.Once
	return func() { once.Do(w.wmu.Unlock) }
}

// AsideSinks is how many aside sinks the client holds: one per Session.Aside
// in flight, none once each has returned or the client has stopped.
func (c *Client) AsideSinks() int {
	c.asideMu.Lock()
	defer c.asideMu.Unlock()
	return len(c.asides)
}

// QueuedBytes is what the stream's queue counts now; QueuedItems how many
// items it holds.
func (s *Stream) QueuedBytes() int { return s.q.queued() }

func (s *Stream) QueuedItems() int {
	s.q.mu.Lock()
	defer s.q.mu.Unlock()
	return len(s.q.items)
}

// AwaitQueued waits until cond holds over the items the stream's queue
// holds now — a copy, read under its lock — waking on the queue's own change
// signal (every item queued, folded in or handed out moves it), or until ctx
// ends: a barrier on what the stream has applied, which no line the tap
// recorded can be (the tap records a line before the client reads it).
func (s *Stream) AwaitQueued(ctx context.Context, cond func([]Item) bool) error {
	for {
		s.q.mu.Lock()
		ok := cond(append([]Item(nil), s.q.items...))
		changed := s.q.changed
		s.q.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// QueueClosed says the stream's queue takes nothing more: its last item is
// in it, or it was dropped.
func (s *Stream) QueueClosed() bool {
	s.q.mu.Lock()
	defer s.q.mu.Unlock()
	return s.q.closed
}

// ErrMalformed is a line from the host that breaks the protocol.
var ErrMalformed = errMalformed

// ItemSize is what it counts against the stream's byte bound.
func ItemSize(it Item) int { return it.size() }

// DialSessionForTest is DialSession with h in place.
func DialSessionForTest(ctx context.Context, path string, o SessionOptions, h TestHooks) (*Session, error) {
	return dialSession(ctx, path, o, h.hooks())
}

// ErrorFromWire is a host's error object as the client reads it off a reply
// (the one construction every refusal goes through).
func ErrorFromWire(pe *protocol.Error) *Error { return newError(pe) }

// Sentinels is the reconstruction table's row for (code, reason).
func Sentinels(code protocol.Code, reason protocol.Reason) []error { return sentinels(code, reason) }

// CapabilitiesFromWire is the session capability set on the wire as agent's.
func CapabilitiesFromWire(c protocol.SessionCapabilities) agent.Capabilities { return capabilities(c) }
