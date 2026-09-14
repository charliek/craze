package acp

import (
	"context"
	"fmt"
)

// interjectSeenCap bounds the interjection dedup set. Ids arrive in wire
// order, so the oldest is evicted first; the set is never allowed to grow.
const interjectSeenCap = 64

// SetInterjectionHandler installs the x.ai/session/interjection handler. The
// user block is created from this broadcast, not from the Interject ack: the
// ack only says the text was accepted, and grok also broadcasts one for the
// interjection it could not merge (the fallback, captured live).
func (c *Client) SetInterjectionHandler(h func(InterjectionNotification)) {
	c.mu.Lock()
	c.interjectHandler = h
	c.mu.Unlock()
}

// SetForeignTurnHandler installs the foreign-turn handler. It fires once with
// Running true when the agent starts a turn craze did not prompt for, and once
// with Running false when that turn ends.
func (c *Client) SetForeignTurnHandler(h func(ForeignTurn)) {
	c.mu.Lock()
	c.foreignHandler = h
	c.mu.Unlock()
}

// ForeignTurnRunning reports whether the agent is in a turn of its own.
func (c *Client) ForeignTurnRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.foreignID != ""
}

// PromptID is the promptId the agent gave the prompt in flight, or "" when it
// has not been learned (or there is nothing in flight).
func (c *Client) PromptID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.promptID
}

// Interject merges text into the running turn without cancelling it. Only the
// grok dialect has it: every other dialect returns ErrUnsupported before
// anything reaches the wire, because a -32601 round trip would look to the
// caller like a transient failure rather than a missing feature.
//
// The ack is nested one level under the JSON-RPC result
// ({"result":{"result":{"status":"queued"}}}), and any status but "queued" is
// an error — the text is the user's and must not be silently dropped.
func (c *Client) Interject(ctx context.Context, text, interjectionID string) error {
	c.mu.Lock()
	d := c.dialect
	sid := c.sessionID
	c.mu.Unlock()
	if d != DialectGrok {
		return ErrUnsupported
	}
	if sid == "" {
		return ErrNoSession
	}
	var ack interjectAck
	if err := c.conn.Call(ctx, MethodGrokInterjectWrapped, InterjectParams{
		SessionID:      sid,
		Text:           text,
		InterjectionID: interjectionID,
	}, &ack); err != nil {
		return err
	}
	if ack.Result.Status != InterjectStatusQueued {
		return fmt.Errorf("acp: interject not queued (status %q)", ack.Result.Status)
	}
	return nil
}

// handleInterjection routes one x.ai/session/interjection. A repeat of an id
// already seen is dropped (grok broadcasts the fallback's interjection too, so
// a reconnect or a relay could show the same text twice); a broadcast with no
// id is rendered as it arrives.
func (c *Client) handleInterjection(msg *Message) {
	n, ok := parseInterjection(msg.Params)
	if !ok {
		c.countDropped()
		return
	}
	c.mu.Lock()
	if c.dialect != DialectGrok || n.SessionID != c.sessionID {
		c.dropped++
		c.mu.Unlock()
		return
	}
	if n.InterjectionID != "" {
		if _, seen := c.interjectSeen[n.InterjectionID]; seen {
			c.dropped++
			c.mu.Unlock()
			return
		}
		if c.interjectSeen == nil {
			c.interjectSeen = make(map[string]struct{}, interjectSeenCap)
		}
		c.interjectSeen[n.InterjectionID] = struct{}{}
		c.interjectOrder = append(c.interjectOrder, n.InterjectionID)
		if len(c.interjectOrder) > interjectSeenCap {
			delete(c.interjectSeen, c.interjectOrder[0])
			c.interjectOrder = c.interjectOrder[1:]
		}
	}
	h := c.interjectHandler
	c.mu.Unlock()
	if h != nil {
		h(n)
	}
}

// handleQueueChanged reads x.ai/queue/changed for turn identity only. craze
// keeps its own queue and never puts anything in grok's, so the entries matter
// for one thing: the entry whose text is the prompt craze just sent carries
// the promptId that prompt's prompt_complete will name.
func (c *Client) handleQueueChanged(msg *Message) {
	n, ok := parseQueueChanged(msg.Params)
	if !ok {
		c.countDropped()
		return
	}
	c.mu.Lock()
	if c.dialect != DialectGrok || n.SessionID != c.sessionID {
		c.dropped++
		c.mu.Unlock()
		return
	}
	c.learnPromptIDLocked(n)
	start, end := c.trackForeignLocked(n.RunningPromptID, n.RunningText)
	h := c.foreignHandler
	c.mu.Unlock()
	fireForeign(h, start, end)
}

// learnPromptIDLocked stamps the in-flight prompt's id the first time a
// broadcast names it, by the text that was sent.
//
// A queued entry is preferred over the running one, and that order matters:
// a prompt craze has just sent is queued, not running. Reading the running id
// first would misidentify a retry of a prompt whose text the agent is still
// running — the broadcast names the old turn as running and the new one as
// queued, and the old turn's completion would then end the new one.
func (c *Client) learnPromptIDLocked(n QueueChanged) {
	if !c.inPrompt || c.promptID != "" || c.promptText == "" {
		return
	}
	for _, e := range n.Entries {
		if e.ID != "" && e.Text == c.promptText && !IsInterjectFallback(e.ID) {
			c.promptID = e.ID
			return
		}
	}
	if n.RunningPromptID != "" && n.RunningText == c.promptText && !IsInterjectFallback(n.RunningPromptID) {
		c.promptID = n.RunningPromptID
	}
}

// trackForeignLocked turns the broadcast's running id into foreign-turn
// transitions. A running id that is not craze's is foreign; while the prompt's
// own id is still unknown only a fallback id counts, so a broadcast that
// arrives before the id is learned cannot mistake craze's own turn for one of
// the agent's. It returns the start and end events to fire off the lock.
func (c *Client) trackForeignLocked(running, text string) (start, end *ForeignTurn) {
	if running != "" && running != c.promptID {
		if !IsInterjectFallback(running) && c.inPrompt && c.promptID == "" {
			// The prompt's own id is still unknown, so this running id may
			// well be it under a runningText that did not compare equal.
			// Calling it foreign here would refuse the very completion that
			// ends craze's turn.
			return nil, nil
		}
		if c.inPrompt {
			c.foreignSeen = true
		}
		if running != c.foreignID {
			if c.foreignID != "" {
				end = &ForeignTurn{ID: c.foreignID, Text: c.foreignText}
			}
			c.foreignID, c.foreignText = running, text
			start = &ForeignTurn{ID: running, Text: text, Running: true}
		}
		return start, end
	}
	// running is empty or craze's own: whatever the agent was running alone
	// is over.
	if c.foreignID != "" {
		end = &ForeignTurn{ID: c.foreignID, Text: c.foreignText}
		c.foreignID, c.foreignText = "", ""
	}
	return start, end
}

// endForeignLocked closes a foreign turn named by id. It returns the event to
// fire off the lock, or nil when the id is not the one running.
func (c *Client) endForeignLocked(id string) *ForeignTurn {
	if id == "" || id != c.foreignID {
		return nil
	}
	ev := &ForeignTurn{ID: c.foreignID, Text: c.foreignText}
	c.foreignID, c.foreignText = "", ""
	return ev
}

func fireForeign(h func(ForeignTurn), start, end *ForeignTurn) {
	if h == nil {
		return
	}
	// The end of the previous turn is announced before the start of the next,
	// so a consumer that counts them never sees two starts in a row.
	if end != nil {
		h(*end)
	}
	if start != nil {
		h(*start)
	}
}

// handleTurnCompleted is the only ending grok's interject fallback has: it
// sends no prompt_complete and no trailing queue/changed (captured live,
// 2026-09-14). turn_completed arrives for every turn, craze's own included, so
// it is acted on only for the foreign turn it names.
func (c *Client) handleTurnCompleted(n grokTurnCompleted) {
	c.mu.Lock()
	if c.dialect != DialectGrok || n.SessionID != c.sessionID {
		c.mu.Unlock()
		return
	}
	end := c.endForeignLocked(n.PromptID)
	h := c.foreignHandler
	c.mu.Unlock()
	fireForeign(h, nil, end)
}

func (c *Client) countDropped() {
	c.mu.Lock()
	c.dropped++
	c.mu.Unlock()
}
