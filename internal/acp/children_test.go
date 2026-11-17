package acp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// The claude side of the child registry (plan 052 § E3, § G1): registration
// before routing at any depth, the early buffer, idle children kept, unknown
// and eviction, child requests, and the child's own cancel. grok's side is
// subagent_test.go, unchanged.

func subUpd(child, extra string) string {
	return `{"sessionUpdate":"subagent_update","sessionId":"` + child + `"` + extra + `}`
}

const (
	running       = `,"title":"Read a file","description":"general-purpose","capabilities":{"cancel":{}},"state":{"state":"running"}`
	idleEndTurn   = `,"state":{"state":"idle","stopReason":"end_turn"}`
	requiresInput = `,"state":{"state":"requires_action"}`
	unknownState  = `,"capabilities":null,"state":{"state":"unknown"}`
)

// TestClaudeChildRegisteredBeforeItsUpdates: a subagent_update on the root
// registers the child before anything of the child is routed, and the child's
// updates go on as its own, never opening a foreign turn.
func TestClaudeChildRegisteredBeforeItsUpdates(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.pushUpdate(t, rigRoot, subUpd("c1", running))
	r.pushOn(t, "c1", chunk("child says"), toolCall("ct", "in_progress"))
	r.want(t, "sub c1 running", "agent_message_chunk@c1", "tool_call@c1")
	r.mu.Lock()
	n := r.subs[0]
	r.mu.Unlock()
	if n.Title != "Read a file" || n.SubagentType != "general-purpose" || n.Cancel == nil || !*n.Cancel || n.ParentSessionID != "" {
		t.Fatalf("the announce read as %+v", n)
	}
}

// TestClaudeChildEarlyUpdatesReplayed: a child's update that arrives before
// its announce is held and replayed, in order, right after the announce.
func TestClaudeChildEarlyUpdatesReplayed(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.pushOn(t, "c1", chunk("one"), chunk("two"))
	r.want(t)
	r.pushUpdate(t, rigRoot, subUpd("c1", running))
	r.pushOn(t, "c1", chunk("three"))
	r.want(t, "sub c1 running", "agent_message_chunk@c1", "agent_message_chunk@c1", "agent_message_chunk@c1")
	if got := r.client.DroppedUpdates(); got != 0 {
		t.Fatalf("dropped %d", got)
	}
}

// TestClaudeChildEarlyBufferBounds: past 5 s an unknown id's frames are
// dropped and counted, and past 256 frames the rest are.
func TestClaudeChildEarlyBufferBounds(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.pushOn(t, "late", chunk("x"), chunk("y"))
	r.clk.Advance(earlyFrameAge)
	r.pushUpdate(t, rigRoot, subUpd("late", running))
	r.roundTrip(t)
	r.want(t, "sub late running")
	if got := r.client.DroppedUpdates(); got != 2 {
		t.Fatalf("dropped %d after the age bound; want 2", got)
	}
	for i := 0; i < earlyFrameCap+3; i++ {
		r.pushUpdate(t, "full", chunk("f"))
	}
	r.roundTrip(t)
	if got := r.client.DroppedUpdates(); got != 5 {
		t.Fatalf("dropped %d past the frame bound; want 5", got)
	}
}

// TestClaudeChildNested: a grandchild is announced on its parent child (the
// immediate parent's id), its frames route by its own id, and its parent's
// unknown ends it too; a sibling is untouched.
func TestClaudeChildNested(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.pushUpdate(t, rigRoot, subUpd("c1", running))
	r.pushUpdate(t, rigRoot, subUpd("c2", running))
	r.pushUpdate(t, "c1", subUpd("g1", running))
	r.pushOn(t, "g1", chunk("deep"))
	r.mu.Lock()
	g := r.subs[2]
	r.mu.Unlock()
	if g.ChildSessionID != "g1" || g.ParentSessionID != "c1" {
		t.Fatalf("the grandchild's announce %+v; want parent c1", g)
	}
	r.pushOn(t, rigRoot, subUpd("c1", unknownState))
	r.pushOn(t, "g1", chunk("gone"))
	r.pushOn(t, "c2", chunk("still here"))
	r.clk.Advance(earlyFrameAge)
	r.pushOn(t, rigRoot, usageNoCost)
	r.want(t, "sub c1 running", "sub c2 running", "sub g1 running", "agent_message_chunk@g1",
		"sub c1 unknown", "sub g1 unknown evicted", "agent_message_chunk@c2", "usage_update")
	if got := r.client.DroppedUpdates(); got != 1 {
		t.Fatalf("dropped %d; want the forgotten grandchild's one frame", got)
	}
}

// TestClaudeChildGrandchildBeforeItsParent: a grandchild announced on a child
// whose own announce has not come yet is held with that child's frames, and
// both register, in order, when it does.
func TestClaudeChildGrandchildBeforeItsParent(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.pushUpdate(t, "c1", subUpd("g1", running))
	r.pushOn(t, "g1", chunk("deep"))
	r.want(t)
	r.pushOn(t, rigRoot, subUpd("c1", running))
	r.want(t, "sub c1 running", "sub g1 running", "agent_message_chunk@g1")
}

// TestClaudeChildIdleStaysRegistered: an idle child is reusable — its late
// updates still route, and running again reuses it.
func TestClaudeChildIdleStaysRegistered(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.pushUpdate(t, rigRoot, subUpd("c1", running))
	r.pushUpdate(t, rigRoot, subUpd("c1", idleEndTurn))
	r.pushOn(t, "c1", chunk("late"))
	r.pushOn(t, rigRoot, subUpd("c1", `,"state":{"state":"running"}`))
	r.want(t, "sub c1 running", "sub c1 idle", "agent_message_chunk@c1", "sub c1 running")
	if got := r.client.DroppedUpdates(); got != 0 {
		t.Fatalf("dropped %d", got)
	}
}

// TestClaudeChildCapEvictsTheOldestIdle: with 64 children registered the
// oldest idle one makes room for the 65th; with none idle the 65th is refused.
func TestClaudeChildCapEvictsTheOldestIdle(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	for i := 0; i < childRouteCap; i++ {
		r.pushUpdate(t, rigRoot, subUpd(fmt.Sprintf("c%d", i), running))
	}
	r.pushUpdate(t, rigRoot, subUpd("c5", idleEndTurn))
	r.pushUpdate(t, rigRoot, subUpd("c9", idleEndTurn))
	r.pushOn(t, rigRoot, subUpd("new", running))
	got := r.log.get()
	if tail := got[len(got)-2:]; !slices.Equal(tail, []string{"sub c5 unknown evicted", "sub new running"}) {
		t.Fatalf("the 65th announce handed on %q; want c5 evicted first", tail)
	}
	r.client.mu.Lock()
	c5, c9 := r.client.kids.has("c5"), r.client.kids.has("c9")
	r.client.mu.Unlock()
	if c5 || !c9 {
		t.Fatalf("c5 registered %v, c9 %v; want only the oldest idle evicted", c5, c9)
	}
	r.pushUpdate(t, rigRoot, subUpd("c9", running))
	r.pushOn(t, rigRoot, subUpd("refused", running))
	if r.client.DroppedUpdates() != 1 {
		t.Fatalf("dropped %d; want the refused registration", r.client.DroppedUpdates())
	}
}

// TestClaudeChildRequestIsNotTheRootTurns: a child's permission request is
// registered with no root turn, so the root turn ending never makes it stale.
func TestClaudeChildRequestIsNotTheRootTurns(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.pushOn(t, rigRoot, subUpd("c1", running))
	arrivals := make(chan Arrival, 2)
	decide := make(chan PermissionDecision)
	r.client.SetPermissionHandler(func(a Arrival, _ PermissionRequest) PermissionDecision {
		arrivals <- a
		return <-decide
	})
	req, done := r.prompt(t)
	r.send(t, 7, MethodRequestPermission, `{"sessionId":"c1","toolCall":{"toolCallId":"ct","title":"Read"},"options":[{"optionId":"allow-once","name":"Yes","kind":"allow_once"},{"optionId":"reject","name":"No","kind":"reject_once"}]}`)
	a := <-arrivals
	if a.InTurn || a.Child != "c1" {
		t.Fatalf("the child's request arrived %+v; want no turn, child c1", a)
	}
	r.answerWith(t, req, `{"stopReason":"end_turn"}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	decide <- PermissionDecision{OptionID: "allow-once"}
	msg := r.readWithin(t, 3*time.Second, "the child's permission reply")
	if !strings.Contains(string(msg.Result), "allow-once") {
		t.Fatalf("the child's answer went out as %s", msg.Result)
	}
}

// TestClaudeChildCancelSession: a child's own cancel is session/cancel on its
// id; an id that is not a registered child is refused before the wire.
func TestClaudeChildCancelSession(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.pushOn(t, rigRoot, subUpd("c1", running))
	if err := r.client.CancelSession(context.Background(), "nobody"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("cancel of an unknown id = %v", err)
	}
	if err := r.client.CancelSession(context.Background(), rigRoot); !errors.Is(err, ErrNoSession) {
		t.Fatalf("cancel of the root by id = %v", err)
	}
	go func() { _ = r.client.CancelSession(context.Background(), "c1") }()
	msg := r.readWithin(t, 3*time.Second, "the child's cancel")
	if msg.Method != MethodSessionCancel || string(msg.Params) != `{"sessionId":"c1"}` {
		t.Fatalf("wrote %s %s", msg.Method, msg.Params)
	}
}

// TestClaudeChildFrames replays the real sub-agent capture (probe2's agent
// step, redacted): the announce registers the child before its stream, the
// child's stream routes by its id, the held prompt absorbs the follow-up, and
// nothing opens a foreign turn.
func TestClaudeChildFrames(t *testing.T) {
	r := newClaudeRig(t)
	r.root = "SESSION"
	r.ready(t)
	var req *Message
	var done chan error
	for _, f := range fixtureFrames(t, "claude-subagent") {
		switch {
		case f.Dir == "out":
			req, done = r.prompt(t)
		case f.Msg.Method == MethodSessionUpdate:
			if err := r.enc.WriteMessage(&f.Msg); err != nil {
				t.Fatal(err)
			}
		case f.Msg.ID != nil:
			r.answerWith(t, req, string(f.Msg.Result))
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
	}
	r.roundTrip(t)
	got := r.log.get()
	first := slices.Index(got, "sub CHILD running")
	childFirst := slices.IndexFunc(got, func(s string) bool { return strings.HasSuffix(s, "@CHILD") })
	if first < 0 || childFirst < first || !slices.Contains(got, "sub CHILD idle") || !slices.Contains(got, "tool_call@CHILD") {
		t.Fatalf("the capture handed on %q", got)
	}
	for _, e := range got {
		if strings.HasPrefix(e, "start") {
			t.Fatalf("the held turn opened a foreign turn: %q", got)
		}
	}
	if r.client.DroppedUpdates() != 0 {
		t.Fatalf("dropped %d", r.client.DroppedUpdates())
	}
}

// TestClaudeChildNestedAsksAndCancels (AC19): at each depth a child's request
// arrives as that child's, outside any root turn, and each child — the
// grandchild included — is cancelled by its own id.
func TestClaudeChildNestedAsksAndCancels(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.pushUpdate(t, rigRoot, subUpd("c1", running))
	r.pushOn(t, "c1", subUpd("g1", running))
	arrivals := make(chan Arrival, 2)
	r.client.SetPermissionHandler(func(a Arrival, _ PermissionRequest) PermissionDecision {
		arrivals <- a
		return PermissionDecision{OptionID: "reject"}
	})
	for i, sid := range []string{"c1", "g1"} {
		r.send(t, 20+i, MethodRequestPermission, `{"sessionId":"`+sid+`","toolCall":{"toolCallId":"t-`+sid+`","title":"Read"},"options":[{"optionId":"reject","name":"No","kind":"reject_once"}]}`)
		if a := <-arrivals; a.Child != sid || a.InTurn {
			t.Fatalf("%s's request arrived %+v", sid, a)
		}
		r.readWithin(t, 3*time.Second, sid+"'s reply")
	}
	for _, sid := range []string{"g1", "c1"} {
		go func() { _ = r.client.CancelSession(context.Background(), sid) }()
		if msg := r.readWithin(t, 3*time.Second, sid+"'s cancel"); string(msg.Params) != `{"sessionId":"`+sid+`"}` {
			t.Fatalf("cancel of %s wrote %s", sid, msg.Params)
		}
	}
}

// TestClaudeChildALabelKeepsTheIdleBit (review C4 item 6): a subagent_update
// with no state — a cwd, a label — does not mark an idle child working, so it
// stays the one the cap evicts; one first named that way is idle.
func TestClaudeChildALabelKeepsTheIdleBit(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.pushUpdate(t, rigRoot, subUpd("c0", running))
	r.pushUpdate(t, rigRoot, subUpd("c0", idleEndTurn))
	r.pushUpdate(t, rigRoot, subUpd("c0", `,"cwd":"/elsewhere"`))
	r.pushUpdate(t, rigRoot, subUpd("restored", `,"title":"An ancestor"`))
	for i := 1; i < childRouteCap-1; i++ {
		r.pushUpdate(t, rigRoot, subUpd(fmt.Sprintf("c%d", i), running))
	}
	r.pushOn(t, rigRoot, subUpd("new1", running))
	r.pushOn(t, rigRoot, subUpd("new2", running))
	if got := r.client.DroppedUpdates(); got != 0 {
		t.Fatalf("dropped %d: an idle child relabelled was not evictable", got)
	}
	r.client.mu.Lock()
	c0, restored := r.client.kids.has("c0"), r.client.kids.has("restored")
	r.client.mu.Unlock()
	if c0 || restored {
		t.Fatalf("c0 %v, restored %v still registered; want both evicted as idle", c0, restored)
	}
}
