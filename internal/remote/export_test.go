package remote

// Waiting is how many commands have a caller waiting on them: sent and
// unanswered, or held for a connection.
func (c *Client) Waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.cmds)
}
