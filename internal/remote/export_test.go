package remote

import "context"

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
}

// DialForTest is Dial with h in place.
func DialForTest(ctx context.Context, path string, o Options, h TestHooks) (*Client, error) {
	return dial(ctx, path, o, hooks{
		registered: h.Registered,
		attempted:  h.Attempted,
		claimed:    h.Claimed,
		replied:    h.Replied,
		retrying:   h.Retrying,
		resent:     h.Resent,
		published:  h.Published,
		closing:    h.Closing,
	})
}

// QueuedBytes is what the stream's queue counts now; QueuedItems how many
// items it holds.
func (s *Stream) QueuedBytes() int { return s.q.queued() }

func (s *Stream) QueuedItems() int {
	s.q.mu.Lock()
	defer s.q.mu.Unlock()
	return len(s.q.items)
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
