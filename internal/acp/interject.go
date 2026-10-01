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
	c.noteQueueNamedLocked(n)
	start, end := c.trackForeignLocked(n.RunningPromptID, n.RunningText)
	h := c.foreignHandler
	c.mu.Unlock()
	fireForeign(h, start, end)
}

// learnPromptIDLocked stamps the in-flight prompt's id the first time a
// broadcast names it, by the text that was sent: block 1 alone (grok 1.0.30)
// or every text block joined (grok 1.0.44, which names a prompt carrying the
// downscale note or a plugin expansion that way; plan 033 V1).
//
// A queued entry is preferred over the running one, and that order matters:
// a prompt craze has just sent is queued, not running. Reading the running id
// first would misidentify a retry of a prompt whose text the agent is still
// running — the broadcast names the old turn as running and the new one as
// queued, and the old turn's completion would then end the new one. An id
// already retired is one of craze's own turns that is over — the refused
// prompt an image resend repeats, by the same text — and never this one's.
//
// An id learned is the prompt heard: grok has taken it into its queue, so a
// refusal behind it is not one the prompt can be resent after (plan 033 C3r).
//
// The image resend's block 1 is the refused attempt's, and that attempt's id
// was never learned (or it would have been heard, and not resent), so neither
// retirement nor the text tells the two apart. The resend learns its id only
// from a broadcast read after its own bytes were written (resendWritten) —
// grok cannot name a prompt it has not read — and never an id grok named
// before that write (queueNamed): a broadcast still listing the refused
// attempt, however late, cannot then hand the resend that attempt's id, whose
// completion would end the resend while it runs (plan 033 C6r, r2 #1a).
func (c *Client) learnPromptIDLocked(n QueueChanged) {
	if !c.inPrompt || c.promptID != "" || c.promptText == "" {
		return
	}
	if c.resending && (!c.resendWritten || c.queueNamedFull) {
		return
	}
	names := func(id, text string) bool {
		return id != "" && !IsInterjectFallback(id) && !c.retiredLocked(id) &&
			(!c.resending || !c.queueNamedLocked(id)) &&
			(text == c.promptText || text == c.promptJoined)
	}
	for _, e := range n.Entries {
		if names(e.ID, e.Text) {
			c.promptID = e.ID
			c.heard = true
			return
		}
	}
	if names(n.RunningPromptID, n.RunningText) {
		c.promptID = n.RunningPromptID
		c.heard = true
	}
}

// queueNamedCap bounds queueNamed. grok's queue holds the prompt in flight and
// the odd interject fallback; a set this size between two prompts is not one
// any agent sends, and past it a resend learns nothing (its reply ends it).
const queueNamedCap = 64

// noteQueueNamedLocked records every promptId a broadcast names, queued or
// running, in queueNamed — but nothing once a resend's bytes are out: a
// broadcast read after the write may name the resend itself, the one id it
// must stay free to learn. Ids named before the write stay in, however the
// broadcasts after it change.
func (c *Client) noteQueueNamedLocked(n QueueChanged) {
	if c.resending && c.resendWritten {
		return
	}
	add := func(id string) {
		if id == "" || c.queueNamedLocked(id) {
			return
		}
		if len(c.queueNamed) == queueNamedCap {
			c.queueNamedFull = true
			return
		}
		c.queueNamed = append(c.queueNamed, id)
	}
	for _, e := range n.Entries {
		add(e.ID)
	}
	add(n.RunningPromptID)
}

// queueNamedLocked reports whether grok's queue named id since the last
// prompt that was not a resend opened (queueNamed).
func (c *Client) queueNamedLocked(id string) bool {
	for _, q := range c.queueNamed {
		if q == id {
			return true
		}
	}
	return false
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
