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
	// Replied runs on the reader once a command's reply has been handled;
	// Retrying on Command once it has taken an answer it retries by code,
	// before its backoff.
	Replied  func(commandID string)
	Retrying func(commandID string)
	// Published runs on a reconnect once it has published the connection it
	// adopted, after its resends.
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
		replied:    h.Replied,
		retrying:   h.Retrying,
		published:  h.Published,
		closing:    h.Closing,
	})
}

// QueuedBytes is what the stream's queue counts now.
func (s *Stream) QueuedBytes() int { return s.q.queued() }

// ItemSize is what it counts against the stream's byte bound.
func ItemSize(it Item) int { return it.size() }
