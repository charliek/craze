package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// answerInterject replies to the x.ai/interject request the client just wrote,
// in the live nested shape.
func answerInterject(t *testing.T, p *rawPipe, id json.RawMessage, status string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"result": map[string]any{"status": status}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: id, Result: raw}); err != nil {
		t.Fatal(err)
	}
}

func TestInterjectAckRoundTrip(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	errCh := make(chan error, 1)
	go func() { errCh <- p.client.Interject(t.Context(), "also say BANANA", "craze-1") }()
	req := p.readWithin(t, 3*time.Second, "interject")
	if req.Method != MethodGrokInterjectWrapped {
		t.Fatalf("method %q", req.Method)
	}
	var got InterjectParams
	if err := json.Unmarshal(req.Params, &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "s1" || got.Text != "also say BANANA" || got.InterjectionID != "craze-1" {
		t.Fatalf("params %+v", got)
	}
	answerInterject(t, p, req.ID, InterjectStatusQueued)
	if err := <-errCh; err != nil {
		t.Fatalf("interject: %v", err)
	}
}

func TestInterjectNonQueuedStatusIsAnError(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	errCh := make(chan error, 1)
	go func() { errCh <- p.client.Interject(t.Context(), "hi", "craze-1") }()
	req := p.readWithin(t, 3*time.Second, "interject")
	answerInterject(t, p, req.ID, "rejected")
	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("err %v", err)
	}
}

func TestInterjectUnsupportedOnCursorWritesNothing(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	if err := p.client.Interject(t.Context(), "hi", "craze-1"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err %v", err)
	}
	p.noReply(t)
}

func TestInterjectNeedsASession(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	if err := p.client.Interject(t.Context(), "hi", "craze-1"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("err %v", err)
	}
	p.noReply(t)
}

func TestInterjectionBroadcastRoutedAndDeduped(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	got := make(chan InterjectionNotification, 4)
	p.client.SetInterjectionHandler(func(n InterjectionNotification) { got <- n })
	p.send(t, nil, MethodGrokInterjectionWrapped, `{"sessionId":"s1","text":"BANANA","interjectionId":"craze-1"}`)
	select {
	case n := <-got:
		if n.Text != "BANANA" || n.InterjectionID != "craze-1" {
			t.Fatalf("n %+v", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no interjection")
	}
	// Same id again: dropped and counted.
	before := p.client.DroppedUpdates()
	p.send(t, nil, MethodGrokInterjectionWrapped, `{"sessionId":"s1","text":"BANANA","interjectionId":"craze-1"}`)
	// A different id still lands, which is also how the drop above is
	// observed without a sleep.
	p.send(t, nil, MethodGrokInterjectionWrapped, `{"sessionId":"s1","text":"MANGO","interjectionId":"craze-2"}`)
	select {
	case n := <-got:
		if n.Text != "MANGO" {
			t.Fatalf("dedup let the repeat through: %+v", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second interjection lost")
	}
	if p.client.DroppedUpdates() <= before {
		t.Fatal("the deduped repeat must be counted")
	}
}

func TestInterjectionWithoutIDIsNeverDeduped(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	got := make(chan InterjectionNotification, 4)
	p.client.SetInterjectionHandler(func(n InterjectionNotification) { got <- n })
	for i := 0; i < 2; i++ {
		p.send(t, nil, MethodGrokInterjectionWrapped, `{"sessionId":"s1","text":"BANANA"}`)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-got:
		case <-time.After(2 * time.Second):
			t.Fatalf("interjection %d lost", i)
		}
	}
}

func TestInterjectionDedupSetIsBounded(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done := make(chan struct{}, interjectSeenCap*2)
	p.client.SetInterjectionHandler(func(InterjectionNotification) { done <- struct{}{} })
	for i := 0; i < interjectSeenCap*2; i++ {
		p.send(t, nil, MethodGrokInterjectionWrapped,
			`{"sessionId":"s1","text":"x","interjectionId":"`+itoa(i)+`"}`)
	}
	for i := 0; i < interjectSeenCap*2; i++ {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("interjection %d lost", i)
		}
	}
	p.client.mu.Lock()
	n := len(p.client.interjectSeen)
	order := len(p.client.interjectOrder)
	p.client.mu.Unlock()
	if n > interjectSeenCap || order > interjectSeenCap {
		t.Fatalf("dedup set unbounded: %d/%d", n, order)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestQueueChangedStampsPromptIDFromRunningText(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, _ := startGrokPrompt(t, p, "hello")
	p.send(t, nil, MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[],"runningPromptId":"p-1","runningText":"hello","runningKind":"prompt"}`)
	waitFor(t, func() bool { return p.client.PromptID() == "p-1" }, "promptID from runningText")
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"p-1","stopReason":"end_turn"}`)
	if res := waitPrompt(t, done); res.StopReason != StopEndTurn {
		t.Fatalf("stop %q", res.StopReason)
	}
}

func TestQueueChangedStampsPromptIDFromEntryText(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, _ := startGrokPrompt(t, p, "hello")
	p.send(t, nil, MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[{"id":"p-9","version":0,"kind":"prompt","text":"hello","position":0}]}`)
	waitFor(t, func() bool { return p.client.PromptID() == "p-9" }, "promptID from entry text")
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"p-9","stopReason":"end_turn"}`)
	waitPrompt(t, done)
}

func TestForeignPromptCompleteDoesNotSettleTheTurn(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, req := startGrokPrompt(t, p, "hello")
	p.send(t, nil, MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[],"runningPromptId":"p-1","runningText":"hello","runningKind":"prompt"}`)
	waitFor(t, func() bool { return p.client.PromptID() == "p-1" }, "promptID")
	before := p.client.DroppedUpdates()
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"p-other","stopReason":"end_turn"}`)
	waitFor(t, func() bool { return p.client.DroppedUpdates() > before }, "foreign completion counted")
	select {
	case out := <-done:
		t.Fatalf("a foreign completion settled the turn: %+v %v", out.res, out.err)
	case <-time.After(150 * time.Millisecond):
	}
	replyPrompt(t, p, req.ID, StopEndTurn)
	waitPrompt(t, done)
}

func TestFallbackCompletionNeverSettlesTheTurn(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, req := startGrokPrompt(t, p, "hello")
	// No queue/changed: the id is unknown, which is the weakest case.
	before := p.client.DroppedUpdates()
	p.send(t, nil, MethodGrokPromptComplete,
		`{"sessionId":"s1","promptId":"interject-fallback-abc","stopReason":"end_turn"}`)
	waitFor(t, func() bool { return p.client.DroppedUpdates() > before }, "fallback completion counted")
	select {
	case out := <-done:
		t.Fatalf("a fallback settled the turn: %+v %v", out.res, out.err)
	case <-time.After(150 * time.Millisecond):
	}
	replyPrompt(t, p, req.ID, StopEndTurn)
	waitPrompt(t, done)
}

func TestUnknownCompletionAfterAForeignSightingIsRefused(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, req := startGrokPrompt(t, p, "hello")
	// A fallback starts while craze's own id is still unknown.
	p.send(t, nil, MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[],"runningPromptId":"interject-fallback-1","runningText":"note","runningKind":"prompt"}`)
	waitFor(t, func() bool { return p.client.ForeignTurnRunning() }, "foreign turn")
	before := p.client.DroppedUpdates()
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","stopReason":"end_turn"}`)
	waitFor(t, func() bool { return p.client.DroppedUpdates() > before }, "unmatched completion counted")
	select {
	case out := <-done:
		t.Fatalf("an id-less completion settled the turn after a foreign sighting: %+v %v", out.res, out.err)
	case <-time.After(150 * time.Millisecond):
	}
	replyPrompt(t, p, req.ID, StopEndTurn)
	waitPrompt(t, done)
}

// TestPreQueueRaceStillCorrelates is the pre-007 race: the RPC reply and
// prompt_complete land together and the notify wins, with no queue/changed to
// learn an id from.
func TestPromptCompleteWithoutQueueChangedStillSettles(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, _ := startGrokPrompt(t, p, "hello")
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","stopReason":"end_turn"}`)
	if res := waitPrompt(t, done); res.StopReason != StopEndTurn {
		t.Fatalf("stop %q", res.StopReason)
	}
}

func TestForeignTurnStartEndAndPromptRefusal(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	events := make(chan ForeignTurn, 4)
	p.client.SetForeignTurnHandler(func(f ForeignTurn) { events <- f })
	p.send(t, nil, MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[],"runningPromptId":"interject-fallback-1","runningText":"note","runningKind":"prompt"}`)
	select {
	case f := <-events:
		if !f.Running || f.ID != "interject-fallback-1" || f.Text != "note" {
			t.Fatalf("start %+v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no foreign start")
	}
	if _, err := p.client.Prompt(t.Context(), "hi"); !errors.Is(err, ErrForeignTurn) {
		t.Fatalf("prompt during a foreign turn: %v", err)
	}
	// turn_completed is the only ending grok gives a fallback.
	p.send(t, nil, MethodGrokSessionNotificationWrapped,
		`{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"interject-fallback-1","stop_reason":"end_turn"}}`)
	select {
	case f := <-events:
		if f.Running || f.ID != "interject-fallback-1" {
			t.Fatalf("end %+v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no foreign end")
	}
	if p.client.ForeignTurnRunning() {
		t.Fatal("foreign turn still running")
	}
	done, req := startGrokPrompt(t, p, "hi")
	replyPrompt(t, p, req.ID, StopEndTurn)
	waitPrompt(t, done)
}

func TestForeignTurnEndsOnQueueChangedWithoutIt(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	events := make(chan ForeignTurn, 4)
	p.client.SetForeignTurnHandler(func(f ForeignTurn) { events <- f })
	p.send(t, nil, MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[],"runningPromptId":"interject-fallback-1","runningText":"note"}`)
	<-events
	p.send(t, nil, MethodGrokQueueChangedWrapped, `{"sessionId":"s1","entries":[]}`)
	select {
	case f := <-events:
		if f.Running {
			t.Fatalf("expected an end: %+v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queue/changed without the id must end the foreign turn")
	}
}

func TestTurnCompletedForCrazesOwnTurnIsNotForeign(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	events := make(chan ForeignTurn, 4)
	p.client.SetForeignTurnHandler(func(f ForeignTurn) { events <- f })
	done, req := startGrokPrompt(t, p, "hello")
	p.send(t, nil, MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[],"runningPromptId":"p-1","runningText":"hello","runningKind":"prompt"}`)
	waitFor(t, func() bool { return p.client.PromptID() == "p-1" }, "promptID")
	p.send(t, nil, MethodGrokSessionNotificationWrapped,
		`{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p-1","stop_reason":"end_turn"}}`)
	replyPrompt(t, p, req.ID, StopEndTurn)
	waitPrompt(t, done)
	select {
	case f := <-events:
		t.Fatalf("craze's own turn reported as foreign: %+v", f)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestQueueNotificationsIgnoredOnCursor(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	p.client.SetInterjectionHandler(func(InterjectionNotification) {
		t.Error("cursor must not route interjections")
	})
	p.client.SetForeignTurnHandler(func(ForeignTurn) {
		t.Error("cursor must not route foreign turns")
	})
	p.send(t, nil, MethodGrokInterjectionWrapped, `{"sessionId":"s1","text":"BANANA"}`)
	p.send(t, nil, MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[],"runningPromptId":"interject-fallback-1"}`)
	time.Sleep(120 * time.Millisecond)
	if p.client.ForeignTurnRunning() {
		t.Fatal("cursor tracked a foreign turn")
	}
}

func TestMalformedQueueNotificationsAreCounted(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	before := p.client.DroppedUpdates()
	p.send(t, nil, MethodGrokQueueChangedWrapped, `[]`)
	p.send(t, nil, MethodGrokInterjectionWrapped, `{"sessionId":"s1"}`)
	p.send(t, nil, MethodGrokInterjectionWrapped, `{"sessionId":"other","text":"x"}`)
	waitFor(t, func() bool { return p.client.DroppedUpdates() >= before+3 }, "three dropped")
}

// queueFixtures are the sanitized live captures of the queue extension.
var queueFixtures = []string{"fallback.jsonl", "interject.jsonl", "queue-changed.jsonl"}

func queueFixtureLines(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "grok-queue", name))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: empty", name)
	}
	return out
}

func TestQueueFixturesParse(t *testing.T) {
	for _, name := range queueFixtures {
		for _, l := range queueFixtureLines(t, name) {
			var m struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			switch m.Method {
			case MethodGrokQueueChangedWrapped, MethodGrokQueueChanged:
				if _, ok := parseQueueChanged(m.Params); !ok {
					t.Fatalf("%s: queue/changed must parse: %s", name, l)
				}
			case MethodGrokInterjectionWrapped, MethodGrokInterjection:
				if _, ok := parseInterjection(m.Params); !ok {
					t.Fatalf("%s: interjection must parse: %s", name, l)
				}
			case MethodGrokPromptCompleteWrapped, MethodGrokPromptComplete:
				if _, ok := parseGrokPromptComplete(m.Params); !ok {
					t.Fatalf("%s: prompt_complete must parse: %s", name, l)
				}
			}
		}
	}
}

// TestFallbackFixtureNeverSendsAPromptComplete is the live fact the whole
// foreign-turn design rests on: grok ends its interject fallback with
// turn_completed and nothing else, so a client that waits for prompt_complete
// waits for ever.
func TestFallbackFixtureNeverSendsAPromptComplete(t *testing.T) {
	var fallbackID string
	var sawTurnCompleted bool
	for _, l := range queueFixtureLines(t, "fallback.jsonl") {
		var m struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		switch m.Method {
		case MethodGrokQueueChangedWrapped:
			n, _ := parseQueueChanged(m.Params)
			if IsInterjectFallback(n.RunningPromptID) {
				fallbackID = n.RunningPromptID
			}
		case MethodGrokPromptCompleteWrapped:
			n, _ := parseGrokPromptComplete(m.Params)
			if IsInterjectFallback(n.PromptID) {
				t.Fatalf("the capture has a prompt_complete for %s", n.PromptID)
			}
		case MethodGrokSessionNotificationWrapped:
			if n, ok := parseGrokTurnCompleted(m.Params); ok && IsInterjectFallback(n.PromptID) {
				sawTurnCompleted = true
			}
		}
	}
	if fallbackID == "" {
		t.Fatal("the capture has no interject fallback")
	}
	if !sawTurnCompleted {
		t.Fatal("the fallback must end with turn_completed")
	}
}

// TestFallbackFixtureReplay drives the whole live capture through a real grok
// client: the craze turn settles on its own id, the fallback becomes a foreign
// turn that starts and ends, and the prompt after it settles too.
func TestFallbackFixtureReplay(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("SESSION")
	foreign := make(chan ForeignTurn, 8)
	p.client.SetForeignTurnHandler(func(f ForeignTurn) { foreign <- f })
	interjections := make(chan InterjectionNotification, 4)
	p.client.SetInterjectionHandler(func(n InterjectionNotification) { interjections <- n })

	lines := queueFixtureLines(t, "fallback.jsonl")
	// The capture's first turn: everything up to its prompt_complete.
	done, req := startGrokPrompt(t, p,
		"Run the shell command \"sleep 6 && echo step1\" in the foreground and wait for it, then reply with the word FINISHED followed by anything else you were asked to include.")
	cut := 0
	for i, l := range lines {
		p.writeLine(t, l)
		if strings.Contains(l, MethodGrokPromptCompleteWrapped) {
			cut = i + 1
			break
		}
	}
	if res := waitPrompt(t, done); res.StopReason != StopEndTurn {
		t.Fatalf("stop %q", res.StopReason)
	}
	_ = req
	if got := p.client.PromptID(); got != "" {
		t.Fatalf("promptID must clear with the turn: %q", got)
	}

	for _, l := range lines[cut:] {
		p.writeLine(t, l)
	}
	select {
	case n := <-interjections:
		if !strings.Contains(n.Text, "BANANA") {
			t.Fatalf("interjection %+v", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no interjection from the fallback")
	}
	start := <-foreign
	if !start.Running || !IsInterjectFallback(start.ID) {
		t.Fatalf("start %+v", start)
	}
	end := <-foreign
	if end.Running || end.ID != start.ID {
		t.Fatalf("end %+v", end)
	}
	if p.client.ForeignTurnRunning() {
		t.Fatal("foreign turn outlived its turn_completed")
	}
}

// waitFor polls a condition the read loop settles asynchronously.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestPromptRefusedUnderAForeignTurnWritesNothing is the other half of the
// refusal: the guard is decided before the wire, so the agent never sees a
// prompt it would have queued behind its own turn.
func TestPromptRefusedUnderAForeignTurnWritesNothing(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	ready := make(chan struct{})
	p.client.SetForeignTurnHandler(func(ForeignTurn) { close(ready) })
	p.send(t, nil, MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[],"runningPromptId":"interject-fallback-1","runningText":"note"}`)
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("no foreign start")
	}
	if _, err := p.client.Prompt(t.Context(), "hi"); !errors.Is(err, ErrForeignTurn) {
		t.Fatalf("err %v", err)
	}
	p.noReply(t)
}

// TestQueuedEntryOutranksTheRunningOne is the retry case: the agent is still
// running an earlier turn with the same text and craze's new prompt is queued
// behind it. The queued entry is the new prompt; reading the running id first
// would give this turn the old turn's identity, and the old turn's completion
// would end it.
func TestQueuedEntryOutranksTheRunningOne(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, req := startGrokPrompt(t, p, "same text")
	p.send(t, nil, MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[{"id":"p-new","version":0,"kind":"prompt","text":"same text","position":0}],`+
			`"runningPromptId":"p-old","runningText":"same text","runningKind":"prompt"}`)
	waitFor(t, func() bool { return p.client.PromptID() == "p-new" }, "the queued entry's id")
	before := p.client.DroppedUpdates()
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","promptId":"p-old","stopReason":"end_turn"}`)
	waitFor(t, func() bool { return p.client.DroppedUpdates() > before }, "the old turn's completion counted")
	select {
	case out := <-done:
		t.Fatalf("the old turn's completion ended the new one: %+v %v", out.res, out.err)
	case <-time.After(150 * time.Millisecond):
	}
	replyPrompt(t, p, req.ID, StopEndTurn)
	waitPrompt(t, done)
}

// TestCancelledTurnDoesNotHandItsInterjectionToTheNext holds the fake's own
// contract: a merge buffer belongs to the turn that filled it. The
// interjection is merged at step 1's safe point and the turn is then
// cancelled, so nothing is stranded and nothing may carry over.
func TestCancelledTurnDoesNotHandItsInterjectionToTheNext(t *testing.T) {
	c := spawnScript(t, "grok-long-turn")
	handshake(t, c)
	var log updateLog
	c.SetUpdateHandler(log.add)
	got := make(chan InterjectionNotification, 4)
	c.SetInterjectionHandler(func(n InterjectionNotification) { got <- n })
	c.SetForeignTurnHandler(func(f ForeignTurn) {
		if f.Running {
			t.Errorf("a merged interjection must not strand: %+v", f)
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := c.Prompt(context.Background(), "first"); err != nil {
			t.Errorf("first prompt: %v", err)
		}
	}()
	waitFor(t, func() bool { return c.PromptID() != "" }, "promptID")
	if err := c.Interject(t.Context(), "BANANA", "craze-1"); err != nil {
		t.Fatalf("interject: %v", err)
	}
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no interjection broadcast")
	}
	// The merge happens at the first tool result; cancelling before it would
	// strand the interjection into a fallback turn instead, which is a
	// different case (TestGrokLongTurnFallbackKeepsTheTurnCount).
	waitFor(t, func() bool { return log.toolDone("call-step-1") }, "step 1's tool result")
	if err := c.Cancel(t.Context()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled turn never ended")
	}
	before := log.text()
	if _, err := c.Prompt(t.Context(), "second"); err != nil {
		t.Fatalf("second prompt: %v", err)
	}
	added := strings.TrimPrefix(log.text(), before)
	if strings.Contains(added, "BANANA") {
		t.Fatalf("the cancelled turn's interjection leaked into the next reply: %q", added)
	}
}
