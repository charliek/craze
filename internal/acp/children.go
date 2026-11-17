package acp

import (
	"encoding/json"
	"time"
)

// The ACP child registry (plan 052 § G1): the one table of the sub-agent
// sessions routed on this connection, which grok's x.ai/session_notification
// and claude's subagent_update both feed — childStarted when a child is
// announced, childIdle and childGone as it goes on — with a per-dialect policy
// for what a finish means:
//
//   - grok: a finished child is deregistered (after its handler ran), a 65th
//     concurrent registration is refused and nothing is evicted, and an update
//     for an id not registered is dropped and counted.
//   - claude: an idle child is reusable and may report again, so it stays
//     registered (only unknown deregisters it, with its descendants); at the
//     cap the oldest idle child is evicted first — its live descendants with it
//     — and only with none idle is a registration refused; and an update for an
//     id not registered yet is held in the early buffer, up to earlyFrameCap
//     frames or earlyFrameAge per id, replayed in order when the id is
//     announced and otherwise dropped and counted. A child's update can come
//     before the announce naming it (R2-4: a grandchild is announced on its
//     parent child, whose own announce may still be on its way).
//
// It routes ids on this one connection only. A future child on another
// provider has its own transport and joins at the agent-level roster, not
// here. Everything in it is under c.mu.

// childRouteCap bounds concurrently routed children. On grok a 65th
// registration is refused (dropped and counted); no registered child is ever
// evicted. On claude the oldest idle child makes room first.
const childRouteCap = 64

// The early buffer's bounds, per unknown id, and how many unknown ids it holds.
const (
	earlyFrameCap = 256
	earlyFrameAge = 5 * time.Second
	earlyIDCap    = 64
)

type childPolicy uint8

const (
	childPolicyGrok childPolicy = iota
	childPolicyClaude
)

type childEntry struct {
	parent string
	idle   bool
}

type earlyFrames struct {
	first  time.Time
	frames []SessionNotification
}

type childRegistry struct {
	entries map[string]*childEntry
	order   []string
	early   map[string]*earlyFrames
	// earlyOrder is the unknown ids in the order they were first buffered.
	earlyOrder []string
}

func (r *childRegistry) reset() { *r = childRegistry{} }

func (r *childRegistry) has(id string) bool {
	_, ok := r.entries[id]
	return ok
}

// childStarted registers id as a child of parent. A known id keeps its slot
// (and, on claude, is running again). A new id past the cap is refused on
// grok; on claude the oldest idle child is evicted to make room, and evicted
// answers it and its descendants, every one of them deregistered.
func (r *childRegistry) childStarted(p childPolicy, parent, id string) (ok bool, evicted []string) {
	if e, known := r.entries[id]; known {
		e.idle = false
		return true, nil
	}
	if len(r.order) >= childRouteCap {
		if p != childPolicyClaude {
			return false, nil
		}
		victim := ""
		for _, cid := range r.order {
			if e := r.entries[cid]; e != nil && e.idle && cid != parent && !r.descends(parent, cid) {
				victim = cid
				break
			}
		}
		if victim == "" {
			return false, nil
		}
		evicted = r.childGone(victim)
	}
	if r.entries == nil {
		r.entries = make(map[string]*childEntry)
	}
	r.entries[id] = &childEntry{parent: parent}
	r.order = append(r.order, id)
	return true, evicted
}

// descends reports whether id is a descendant of anc. The walk is bounded by
// the registry's size, so a parent cycle cannot hold it.
func (r *childRegistry) descends(id, anc string) bool {
	for e, hops := r.entries[id], 0; e != nil && hops < len(r.entries); e, hops = r.entries[e.parent], hops+1 {
		if e.parent == anc {
			return true
		}
	}
	return false
}

// childIdle marks a claude child idle (true) or working (false).
func (r *childRegistry) childIdle(id string, idle bool) {
	if e := r.entries[id]; e != nil {
		e.idle = idle
	}
}

// childGone deregisters id and every registered descendant of it, and answers
// them all, id first, then the descendants in registration order.
func (r *childRegistry) childGone(id string) []string {
	if !r.has(id) {
		return nil
	}
	gone := []string{id}
	for _, cid := range r.order {
		if cid != id && r.descends(cid, id) {
			gone = append(gone, cid)
		}
	}
	for _, g := range gone {
		r.remove(g)
	}
	return gone
}

func (r *childRegistry) remove(id string) {
	if _, ok := r.entries[id]; !ok {
		return
	}
	delete(r.entries, id)
	for i, v := range r.order {
		if v == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// bufferEarly holds an update for an id not registered yet (claude). It
// answers how many frames it dropped to stay in bounds: the oldest unknown id
// whole when a new one would pass earlyIDCap, or this frame when its id's
// buffer is full.
func (r *childRegistry) bufferEarly(n SessionNotification, now time.Time) (dropped int64) {
	b := r.early[n.SessionID]
	if b == nil {
		if len(r.earlyOrder) >= earlyIDCap {
			old := r.earlyOrder[0]
			dropped += int64(len(r.early[old].frames))
			r.dropEarly(old)
		}
		if r.early == nil {
			r.early = make(map[string]*earlyFrames)
		}
		b = &earlyFrames{first: now}
		r.early[n.SessionID] = b
		r.earlyOrder = append(r.earlyOrder, n.SessionID)
	}
	if len(b.frames) >= earlyFrameCap {
		return dropped + 1
	}
	b.frames = append(b.frames, n)
	return dropped
}

// expireEarly drops, and answers the count of, every frame held for an id
// first buffered earlyFrameAge or more before now.
func (r *childRegistry) expireEarly(now time.Time) (dropped int64) {
	for len(r.earlyOrder) > 0 {
		id := r.earlyOrder[0]
		b := r.early[id]
		if now.Sub(b.first) < earlyFrameAge {
			break
		}
		dropped += int64(len(b.frames))
		r.dropEarly(id)
	}
	return dropped
}

// takeEarly answers, in arrival order, what is held for id, and forgets it.
func (r *childRegistry) takeEarly(id string) []SessionNotification {
	b := r.early[id]
	if b == nil {
		return nil
	}
	r.dropEarly(id)
	return b.frames
}

func (r *childRegistry) dropEarly(id string) {
	delete(r.early, id)
	for i, v := range r.earlyOrder {
		if v == id {
			r.earlyOrder = append(r.earlyOrder[:i], r.earlyOrder[i+1:]...)
			return
		}
	}
}

// claudeSubagent reads one claude subagent_update, made on the session outer
// (the child's immediate parent, native-subagents.js:333), into the shared
// notification. ok is false for one that names no child.
func parseClaudeSubagent(outer, root string, update json.RawMessage) (SubagentNotification, bool) {
	var w struct {
		SessionID    string           `json:"sessionId"`
		Title        string           `json:"title"`
		Description  string           `json:"description"`
		Capabilities *json.RawMessage `json:"capabilities"`
		State        *struct {
			State      string `json:"state"`
			StopReason string `json:"stopReason"`
			Error      *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"state"`
	}
	if json.Unmarshal(update, &w) != nil || w.SessionID == "" || w.SessionID == outer || w.SessionID == root {
		return SubagentNotification{}, false
	}
	parent := outer
	if outer == root {
		// The root's own child has no parent row.
		parent = ""
	}
	n := SubagentNotification{
		SessionID:       outer,
		Kind:            SubagentUpdate,
		SubagentID:      w.SessionID,
		ChildSessionID:  w.SessionID,
		ParentSessionID: parent,
		Title:           w.Title,
		// The RFD form's description is the child's role, its subagent_type
		// (native-subagents.js rfdLabels).
		SubagentType: w.Description,
	}
	if w.State != nil {
		n.State = w.State.State
		n.StopReason = w.State.StopReason
		if w.State.Error != nil {
			n.Error = w.State.Error.Message
		}
	}
	// Present at all, the capabilities replace the child's: null (an unknown
	// child's) and an object without cancel both grant nothing.
	if w.Capabilities != nil {
		var caps struct {
			Cancel *json.RawMessage `json:"cancel"`
		}
		can := false
		if json.Unmarshal(*w.Capabilities, &caps) == nil && caps.Cancel != nil && string(*caps.Cancel) != "null" {
			can = true
		}
		n.Cancel = &can
	}
	return n, true
}
