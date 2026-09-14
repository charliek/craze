package acp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestCursorFakeSecondPromptCancelsTheFirst drives the fake through a raw
// Conn, bypassing Client.Prompt's one-in-flight guard, because that is the
// only way to reach the wire behaviour craze's queue exists to avoid: on
// cursor a second session/prompt makes the first return cancelled
// (cursor-second-prompt.jsonl 14.01). Nothing in craze may ever produce it.
func TestCursorFakeSecondPromptCancelsTheFirst(t *testing.T) {
	c := spawnScript(t, "long-turn")
	handshake(t, c)
	sid := c.SessionID()
	conn := c.Conn()

	type res struct {
		out PromptResult
		err error
	}
	first := make(chan res, 1)
	go func() {
		var out PromptResult
		err := conn.Call(context.Background(), MethodSessionPrompt, PromptParams{
			SessionID: sid,
			Prompt:    []ContentBlock{{Type: "text", Text: "one"}},
		}, &out)
		first <- res{out, err}
	}()
	// Let the first turn get going, then overtake it.
	time.Sleep(200 * time.Millisecond)
	second := make(chan res, 1)
	go func() {
		var out PromptResult
		err := conn.Call(context.Background(), MethodSessionPrompt, PromptParams{
			SessionID: sid,
			Prompt:    []ContentBlock{{Type: "text", Text: "two"}},
		}, &out)
		second <- res{out, err}
	}()

	select {
	case r := <-first:
		if r.err != nil {
			t.Fatalf("first prompt: %v", r.err)
		}
		if r.out.StopReason != StopCancelled {
			t.Fatalf("first stop %q, want cancelled", r.out.StopReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first prompt was never answered")
	}
	select {
	case r := <-second:
		if r.err != nil {
			t.Fatalf("second prompt: %v", r.err)
		}
		if r.out.StopReason != StopEndTurn {
			t.Fatalf("second stop %q, want end_turn", r.out.StopReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second prompt never finished")
	}
}

// TestCursorFakeRefusesInterject: only grok has the method.
func TestCursorFakeRefusesInterject(t *testing.T) {
	c := spawnScript(t, "long-turn")
	handshake(t, c)
	var ack interjectAck
	err := c.Conn().Call(t.Context(), MethodGrokInterject, InterjectParams{
		SessionID: c.SessionID(),
		Text:      "hi",
	}, &ack)
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != CodeMethodNotFound {
		t.Fatalf("err %v", err)
	}
}

// TestGrokLongTurnInterjectMerges is the whole mid-turn path end to end: the
// broadcast lands, the turn is not cancelled, and the reply carries the
// interjected word.
func TestGrokLongTurnInterjectMerges(t *testing.T) {
	c := spawnScript(t, "grok-long-turn")
	handshake(t, c)
	var log updateLog
	c.SetUpdateHandler(log.add)
	got := make(chan InterjectionNotification, 4)
	c.SetInterjectionHandler(func(n InterjectionNotification) { got <- n })
	c.SetForeignTurnHandler(func(f ForeignTurn) {
		if f.Running {
			t.Errorf("a merged interjection must not start a turn of its own: %+v", f)
		}
	})

	done := make(chan *PromptResult, 1)
	go func() {
		res, err := c.Prompt(context.Background(), "do the steps")
		if err != nil {
			t.Errorf("prompt: %v", err)
			done <- nil
			return
		}
		done <- res
	}()
	waitFor(t, func() bool { return c.PromptID() != "" }, "promptID learned from queue/changed")
	if err := c.Interject(t.Context(), "BANANA", "craze-1"); err != nil {
		t.Fatalf("interject: %v", err)
	}
	select {
	case n := <-got:
		if n.Text != "BANANA" || n.InterjectionID != "craze-1" {
			t.Fatalf("broadcast %+v", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no interjection broadcast")
	}
	select {
	case res := <-done:
		if res == nil {
			return
		}
		if res.StopReason != StopEndTurn {
			t.Fatalf("stop %q: an interjection must not cancel the turn", res.StopReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("turn never ended")
	}
	if text := log.text(); !strings.Contains(text, "DONE step1 BANANA step2") {
		t.Fatalf("reply %q must carry the interjection", text)
	}
}

// TestGrokLongTurnFallbackKeepsTheTurnCount is the adversarial case: the
// interjection is stranded, grok runs a turn of its own, and craze's prompt
// must settle exactly once — on its own id — with the foreign turn opened and
// closed around it.
func TestGrokLongTurnFallbackKeepsTheTurnCount(t *testing.T) {
	c := spawnScript(t, "grok-long-turn-fallback")
	handshake(t, c)
	foreign := make(chan ForeignTurn, 8)
	c.SetForeignTurnHandler(func(f ForeignTurn) { foreign <- f })

	done := make(chan *PromptResult, 2)
	go func() {
		res, err := c.Prompt(context.Background(), "do the steps")
		if err != nil {
			t.Errorf("prompt: %v", err)
			done <- nil
			return
		}
		done <- res
	}()
	waitFor(t, func() bool { return c.PromptID() != "" }, "promptID")
	if err := c.Interject(t.Context(), "BANANA", "craze-1"); err != nil {
		t.Fatalf("interject: %v", err)
	}
	select {
	case res := <-done:
		if res == nil {
			return
		}
		if res.StopReason != StopEndTurn {
			t.Fatalf("stop %q", res.StopReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the craze turn never settled")
	}
	start := waitForeign(t, foreign)
	if !start.Running || !IsInterjectFallback(start.ID) {
		t.Fatalf("start %+v", start)
	}
	end := waitForeign(t, foreign)
	if end.Running || end.ID != start.ID {
		t.Fatalf("end %+v", end)
	}
	waitFor(t, func() bool { return !c.ForeignTurnRunning() }, "foreign turn ended")
	// The queued prompt craze was holding runs next, on a session the
	// fallback has let go of.
	res, err := c.Prompt(t.Context(), "PINEAPPLE")
	if err != nil {
		t.Fatalf("queued prompt: %v", err)
	}
	if res.StopReason != StopEndTurn {
		t.Fatalf("queued stop %q", res.StopReason)
	}
}

func waitForeign(t *testing.T, ch <-chan ForeignTurn) ForeignTurn {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("no foreign-turn event")
		return ForeignTurn{}
	}
}

// TestGrokLongTurnCancelAnswersOnce proves one reply per request: a cancel
// mid-turn ends that prompt cancelled and nothing answers it twice.
func TestGrokLongTurnCancelAnswersOnce(t *testing.T) {
	c := spawnScript(t, "grok-long-turn")
	handshake(t, c)
	done := make(chan *PromptResult, 1)
	go func() {
		res, err := c.Prompt(context.Background(), "do the steps")
		if err != nil {
			t.Errorf("prompt: %v", err)
			done <- nil
			return
		}
		done <- res
	}()
	waitFor(t, func() bool { return c.PromptID() != "" }, "promptID")
	if err := c.Cancel(t.Context()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case res := <-done:
		if res == nil {
			return
		}
		if res.StopReason != StopCancelled {
			t.Fatalf("stop %q", res.StopReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled turn never ended")
	}
	// The next turn still runs: the cancel belonged to the one it interrupted.
	res, err := c.Prompt(t.Context(), "again")
	if err != nil {
		t.Fatalf("second prompt: %v", err)
	}
	if res.StopReason != StopEndTurn {
		t.Fatalf("second stop %q", res.StopReason)
	}
}

func (u *updateLog) text() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var b strings.Builder
	for _, upd := range u.list {
		if upd.SessionUpdate == UpdateAgentMessage && upd.Content != nil {
			b.WriteString(upd.Content.Text)
		}
	}
	return b.String()
}

// toolDone reports whether a tool call has reached a terminal status.
func (u *updateLog) toolDone(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, upd := range u.list {
		if upd.ToolCallID == id && (upd.Status == "completed" || upd.Status == "failed") {
			return true
		}
	}
	return false
}
