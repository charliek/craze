package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/transcript"
)

func seqd(evs ...agent.Event) []agent.Event {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range evs {
		evs[i].Seq = uint64(i + 1)
		evs[i].At = base.Add(time.Duration(i) * time.Second)
	}
	return evs
}

// retriedTurn is a turn with a finished step, a retried step and the attempt that
// succeeds. From is the failed attempt's first event.
func retriedTurn(answer string) []agent.Event {
	evs := seqd(
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "go"}},
		agent.Event{Type: agent.EventText, Text: "first step"},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.1.1", Status: "completed", Edited: &agent.ToolEdited{Files: []string{"kept.go"}}}},
		agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &agent.UsageState{Turn: agent.Spend{Input: 10}}}},
		agent.Event{Type: agent.EventThought, Text: "abandoned thought"},
		agent.Event{Type: agent.EventText, Text: "abandoned text"},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.2.1", Status: "pending"}},
		agent.Event{Type: agent.EventText, Text: "more abandoned text"},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.2.1", Status: "failed", Edited: &agent.ToolEdited{Files: []string{"abandoned.go"}}}},
		agent.Event{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Turn: 1, Step: 2, Attempt: 1, Total: 1, Reason: "INTERNAL_ERROR", From: 5, Discarded: true}},
	)
	if answer == "" {
		return evs
	}
	return append(evs, seqdFrom(uint64(len(evs)+1),
		agent.Event{Type: agent.EventText, Text: answer},
		agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &agent.UsageState{Turn: agent.Spend{Input: 20}}}},
		agent.Event{Type: agent.EventDone, StopReason: "end_turn"},
	)...)
}

func seqdFrom(from uint64, evs ...agent.Event) []agent.Event {
	for i := range evs {
		evs[i].Seq = from + uint64(i)
	}
	return evs
}

func foldOf(evs []agent.Event) *turnAccumulator {
	a := newTurnAccumulator()
	for _, ev := range evs {
		a.fold(ev)
	}
	return a
}

// TestARetrySaysNothingOfTheAttempt is the accumulator's whole contract with a
// retry: message is the final attempt's, toolCalls and files count no abandoned
// call, retries says how many, and usage is what was reported, the attempt having
// reported none.
func TestARetrySaysNothingOfTheAttempt(t *testing.T) {
	a := foldOf(retriedTurn("whole answer"))
	msg, cut := a.message()
	if msg != "whole answer" || cut {
		t.Fatalf("message = %q, cut %v", msg, cut)
	}
	if n := a.toolCalls(); n != 1 {
		t.Fatalf("toolCalls = %d; want the finished call alone", n)
	}
	if got := a.edited().files; !reflect.DeepEqual(got, []string{"kept.go"}) {
		t.Fatalf("files = %v; want the finished call's", got)
	}
	if a.retries != 1 {
		t.Fatalf("retries = %d", a.retries)
	}
	if a.usage == nil || a.usage.Turn.Input != 20 {
		t.Fatalf("usage = %+v", a.usage)
	}
	if a.stepOpen {
		t.Fatal("a step is open after the last report: the abandoned call's stepNext outlived its attempt")
	}
	res := &turnResult{Status: statusCompleted}
	res.fill(a)
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"retries":1`) || strings.Contains(string(raw), "abandoned") {
		t.Fatalf("the result: %s", raw)
	}
	res = &turnResult{Status: statusCompleted}
	res.fill(newTurnAccumulator())
	if raw, _ := json.Marshal(res); strings.Contains(string(raw), "retries") {
		t.Fatalf("a turn with none says retries: %s", raw)
	}
}

// TestAMessageIsUncoveredByARetry: an attempt whose replacement says nothing
// leaves the message as the step before left it, however many runs the attempt
// had; and one that fails again says the last attempt's, its run closed.
func TestAMessageIsUncoveredByARetry(t *testing.T) {
	a := foldOf(retriedTurn(""))
	if msg, _ := a.message(); msg != "first step" {
		t.Fatalf("message = %q; want the step before the attempt's", msg)
	}
	evs := append(retriedTurn(""), seqdFrom(11,
		agent.Event{Type: agent.EventText, Text: "second try"},
		agent.Event{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Turn: 1, Step: 2, Attempt: 2, Total: 2, From: 11, Discarded: true}},
		agent.Event{Type: agent.EventText, Text: "third try"},
		agent.Event{Type: agent.EventError},
	)...)
	a = foldOf(evs)
	if msg, _ := a.message(); msg != "third try" || a.retries != 2 {
		t.Fatalf("message = %q, retries %d", msg, a.retries)
	}
	// A retry of an attempt that streamed nothing moves only the count.
	evs = seqd(
		agent.Event{Type: agent.EventText, Text: "kept"},
		agent.Event{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Attempt: 1, Total: 1, From: 2}},
		agent.Event{Type: agent.EventText, Text: " on"},
	)
	if a = foldOf(evs); a.retries != 1 {
		t.Fatalf("retries = %d", a.retries)
	}
	if msg, _ := a.message(); msg != "kept on" {
		t.Fatalf("message = %q", msg)
	}
}

// TestAStopDuringARetryCountsNoStepTwice: the attempt's call is gone, so a stop
// straight after the retry reports a step in flight, and no finished call of it.
func TestAStopDuringARetryCountsNoStepTwice(t *testing.T) {
	a := foldOf(retriedTurn(""))
	if !a.stepOpen || a.stepNext {
		t.Fatalf("stepOpen %v, stepNext %v; want a step under way, and none promised after it", a.stepOpen, a.stepNext)
	}
	if n := a.toolCalls(); n != 1 {
		t.Fatalf("toolCalls = %d", n)
	}
}

// TestAWaiterSeededFromASnapshotAgrees is a waiter met by a turn at any point: it
// seeds from a snapshot cut there, through the codec, and folds the rest. Whether
// the cut is before the attempt, in the middle of it with its run open, or after
// the retry, the result is what a waiter that saw it all reports. A reconnect
// after the retry, whose event it never folds, still counts it and nothing of
// the attempt.
func TestAWaiterSeededFromASnapshotAgrees(t *testing.T) {
	all := retriedTurn("whole answer")
	whole := foldOf(all)
	wholeMsg, _ := whole.message()
	for cut := 1; cut < len(all); cut++ {
		m := transcript.New(transcript.Options{})
		for _, ev := range all[:cut] {
			m.Fold(ev)
		}
		snap, err := m.Snapshot(1 << 20)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := transcript.EncodeSnapshot(snap)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := transcript.DecodeSnapshot(raw)
		if err != nil {
			t.Fatal(err)
		}
		a := newTurnAccumulator()
		seedFromSnapshot(a, dec)
		for _, ev := range all[cut:] {
			a.fold(ev)
		}
		msg, _ := a.message()
		if msg != wholeMsg {
			t.Errorf("cut at %d: message %q; want %q", cut, msg, wholeMsg)
		}
		if a.toolCalls() != whole.toolCalls() || a.retries != whole.retries {
			t.Errorf("cut at %d: toolCalls %d, retries %d; want %d and %d", cut, a.toolCalls(), a.retries, whole.toolCalls(), whole.retries)
		}
		if got := a.edited().files; !reflect.DeepEqual(got, whole.edited().files) {
			t.Errorf("cut at %d: files %v; want %v", cut, got, whole.edited().files)
		}
	}
}

// TestAWaiterSeededFromASnapshotMidAttemptWithNoReplacementText: the message a
// retry uncovers is the snapshot's own older entry.
func TestAWaiterSeededFromASnapshotMidAttemptUncovers(t *testing.T) {
	all := retriedTurn("")
	m := transcript.New(transcript.Options{})
	for _, ev := range all[:6] { // through the attempt's first text
		m.Fold(ev)
	}
	snap, err := m.Snapshot(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	a := newTurnAccumulator()
	seedFromSnapshot(a, snap)
	if msg, _ := a.message(); msg != "abandoned text" {
		t.Fatalf("the seed's message = %q", msg)
	}
	for _, ev := range all[6:] {
		a.fold(ev)
	}
	if msg, _ := a.message(); msg != "first step" {
		t.Fatalf("message = %q; want the entry before the attempt's", msg)
	}
}

// TestPromptJSONLineForAStepRetry is `craze prompt --json`'s additive line: the
// one thing a line reader needs to drop what it read, which seq says exactly.
func TestPromptJSONLineForAStepRetry(t *testing.T) {
	var out bytes.Buffer
	err := encodeEvent(&out, agent.Event{Type: agent.EventStepRetry, Seq: 9, Agent: "sub-1",
		StepRetry: &agent.StepRetryInfo{Turn: 1, Step: 2, Attempt: 1, Total: 3, Reason: "INTERNAL_ERROR", DelayMs: 5000, From: 5, Discarded: true}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"step_retry","seq":9,"agent":"sub-1","turn":1,"step":2,"attempt":1,"reason":"INTERNAL_ERROR","from":5,"discarded":true}` + "\n"
	if out.String() != want {
		t.Fatalf("the line:\n %s\nwant\n %s", out.String(), want)
	}
	out.Reset()
	if err := encodeEvent(&out, agent.Event{Type: agent.EventStepRetry}); err != nil || out.Len() != 0 {
		t.Fatalf("an event with no payload wrote %q, %v", out.String(), err)
	}
}

// TestPlainPromptSaysWhatItCannotTakeBack: plain output cannot retract what it
// wrote, so the next attempt's text starts a paragraph and stderr says so; a
// retry of nothing, or a child's, says nothing.
func TestPlainPromptSaysWhatItCannotTakeBack(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := &promptOpts{stdout: &stdout, stderr: &stderr}
	for _, ev := range []agent.Event{
		{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Reason: "before anything", From: 1}},
		{Type: agent.EventStepRetry, Agent: "sub-1", StepRetry: &agent.StepRetryInfo{Reason: "a child's", From: 1, Discarded: true}},
		{Type: agent.EventText, Text: "half"},
		{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Reason: "INTERNAL_ERROR", From: 1, Discarded: true}},
		{Type: agent.EventText, Text: "whole"},
	} {
		if err := o.writeEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	if stdout.String() != "half\n\nwhole" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "INTERNAL_ERROR") || !strings.Contains(got, "not part of the answer") {
		t.Fatalf("stderr = %q", got)
	}
}

// TestAWaiterMeetsARetryOverTheWire is the real waiter against a host's stream
// and snapshots: attached in the middle of the failed attempt, or after its
// retry, the result is the final attempt's, with the count.
func TestAWaiterMeetsARetryOverTheWire(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after bool // the waiter attaches once the retry has gone by
	}{
		{"attached mid-attempt", false},
		{"attached after the retry", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWaitRig(t, 0)
			attempt, retried := make(chan struct{}), make(chan struct{})
			goOn, finish := make(chan struct{}), make(chan struct{})
			turn := endTurn()
			turn.before = func() {
				r.s.emit(agent.Event{Type: agent.EventText, Text: "first step"})
				r.s.emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.1.1", Status: "completed"}})
				from := r.s.log.LastSeq() + 1
				r.s.emit(agent.Event{Type: agent.EventText, Text: "abandoned"})
				r.s.emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.2.1", Status: "pending"}})
				close(attempt)
				<-goOn
				r.s.emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.2.1", Status: "failed"}})
				r.s.emit(agent.Event{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Turn: 1, Step: 2, Attempt: 1, Total: 1, Reason: "INTERNAL_ERROR", From: from, Discarded: true}})
				close(retried)
				<-finish
				r.s.emit(agent.Event{Type: agent.EventText, Text: "whole answer"})
			}
			r.script(turn)
			r.submit("mine")
			<-attempt
			if tc.after {
				close(goOn)
				<-retried
			}
			conn, tp := r.dialTapped()
			w := goWait(conn, waitStart{}, waitOptions{})
			waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
			if !tc.after {
				close(goOn)
				<-retried
			}
			close(finish)
			res, f := w.result(t)
			if f != nil {
				t.Fatalf("the wait failed: %+v", f)
			}
			if res.message() != "whole answer" || res.Retries != 1 || res.ToolCalls == nil || *res.ToolCalls != 1 {
				t.Fatalf("the result: message %q, retries %d, toolCalls %v", res.message(), res.Retries, res.ToolCalls)
			}
		})
	}
}

// nineBursts is a finished step's answer, then a step whose failed attempt says
// nine runs of text, thoughts between them, and is retried.
func nineBursts() []agent.Event {
	evs := []agent.Event{
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "go"}},
		{Type: agent.EventText, Text: "older answer"},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.1.1", Status: "completed"}},
		{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &agent.UsageState{Turn: agent.Spend{Input: 10}}}},
		{Type: agent.EventText, Text: "previous answer"},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.2.1", Status: "completed"}},
		{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &agent.UsageState{Turn: agent.Spend{Input: 20}}}},
	}
	from := uint64(len(evs) + 1)
	for i := range 9 {
		evs = append(evs, agent.Event{Type: agent.EventThought, Text: "burst"}, agent.Event{Type: agent.EventText, Text: "abandoned " + string(rune('a'+i))})
	}
	evs = append(evs, agent.Event{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Turn: 1, Step: 3, Attempt: 1, Total: 1, Reason: "x", From: from, Discarded: true}})
	return seqd(evs...)
}

// TestNineBurstsRollBackToThePreviousAnswer: however many runs a failed attempt
// says, the message after it is what it found: for a follower and for a waiter
// seeded from a snapshot cut at any point.
func TestNineBurstsRollBackToThePreviousAnswer(t *testing.T) {
	all := nineBursts()
	if msg, _ := foldOf(all).message(); msg != "previous answer" {
		t.Fatalf("a follower's message = %q", msg)
	}
	for cut := 1; cut < len(all); cut++ {
		m := transcript.New(transcript.Options{})
		for _, ev := range all[:cut] {
			m.Fold(ev)
		}
		snap, err := m.Snapshot(1 << 20)
		if err != nil {
			t.Fatal(err)
		}
		a := newTurnAccumulator()
		seedFromSnapshot(a, snap)
		for _, ev := range all[cut:] {
			a.fold(ev)
		}
		if msg, _ := a.message(); msg != "previous answer" {
			t.Errorf("seeded at %d: message = %q", cut, msg)
		}
	}
	// And a second failed attempt comes back to the same place.
	a := foldOf(append(all, seqdFrom(uint64(len(all)+1),
		agent.Event{Type: agent.EventText, Text: "second try"},
		agent.Event{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Attempt: 2, Total: 2, From: uint64(len(all) + 1), Discarded: true}},
	)...))
	if msg, _ := a.message(); msg != "previous answer" {
		t.Fatalf("after the second attempt: %q", msg)
	}
}

// TestAnAttemptHoldsNoMoreBehindItsMessage is the bound: a follower holds at most
// two runs behind the message, whatever the turn says, and each is cut to the
// message's cap.
func TestAnAttemptHoldsNoMoreBehindItsMessage(t *testing.T) {
	big := strings.Repeat("x", resultMessageMax+1000)
	a := newTurnAccumulator()
	seq := uint64(0)
	next := func(ev agent.Event) {
		seq++
		ev.Seq = seq
		a.fold(ev)
	}
	for range 50 { // steps
		for range 40 { // runs of a step
			next(agent.Event{Type: agent.EventText, Text: big})
			next(agent.Event{Type: agent.EventThought, Text: "t"})
			if len(a.behind) > 2 {
				t.Fatalf("%d runs behind the message, inside a step", len(a.behind))
			}
		}
		next(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &agent.UsageState{}}})
		if len(a.behind) > 2 {
			t.Fatalf("%d runs behind the message", len(a.behind))
		}
		for _, r := range a.behind {
			if len(r.text) > resultMessageMax {
				t.Fatalf("a run of %d bytes held", len(r.text))
			}
		}
	}
}

// TestAStrayReportDuringAnAttemptLosesNothing: a report is no promise that no
// attempt is under way, so one landing inside it leaves the previous answer there.
func TestAStrayReportDuringAnAttemptLosesNothing(t *testing.T) {
	evs := seqd(
		agent.Event{Type: agent.EventText, Text: "previous answer"},
		agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t", Status: "completed"}},
		agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &agent.UsageState{}}},
		agent.Event{Type: agent.EventText, Text: "abandoned"},
		agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &agent.UsageState{}}},
		agent.Event{Type: agent.EventThought, Text: "and"},
		agent.Event{Type: agent.EventText, Text: "abandoned too"},
		agent.Event{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Attempt: 1, Total: 1, From: 4, Discarded: true}},
	)
	if msg, _ := foldOf(evs).message(); msg != "previous answer" {
		t.Fatalf("message = %q", msg)
	}
}

// TestAStepsReportOrderLeavesTheNextInFlight: step 1's report may land after step
// 2 has begun, the one ordering a report is allowed once the host flushes at each
// step's start: it leaves step 2 in flight, with or without a retry after it.
func TestAStepsReportOrderLeavesTheNextInFlight(t *testing.T) {
	report := agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Usage: &agent.UsageState{}}}
	tool := agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.1.1", Status: "completed"}}
	for _, tc := range []struct {
		name string
		evs  []agent.Event
	}{
		{"no retry", seqd(tool, agent.Event{Type: agent.EventText, Text: "step 2"}, report)},
		{"a retry after it", seqd(tool, agent.Event{Type: agent.EventText, Text: "step 2"}, report,
			agent.Event{Type: agent.EventStepRetry, StepRetry: &agent.StepRetryInfo{Attempt: 1, Total: 1, From: 2, Discarded: true}})},
		{"the report before step 2", seqd(tool, report, agent.Event{Type: agent.EventText, Text: "step 2"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := foldOf(tc.evs)
			if got := usageIncomplete(a.stepOpen); got == nil || *got {
				t.Fatalf("usageComplete = %v; want false, step 2 in flight", got)
			}
		})
	}
}

// TestACancelDuringTheBackoffIsAStepInFlight is the real session: a step reported,
// the next failing after text, and a cancel while its retry waits. What the waiter
// folds says usageComplete: false, with the first step's usage, and no abandoned
// text.
func TestACancelDuringTheBackoffIsAStepInFlight(t *testing.T) {
	failure := &fantasy.ProviderError{Title: "stream transport error", Message: "INTERNAL_ERROR", TransientError: true,
		ResponseHeaders: map[string]string{"retry-after-ms": "50000"}}
	model := &nativeJSONModel{steps: [][]fantasy.StreamPart{
		nativeJSONCall("c1", "read", `{"filePath":"nope"}`),
		{{Type: fantasy.StreamPartTypeTextStart, ID: "0"}, {Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: "abandoned"},
			{Type: fantasy.StreamPartTypeError, Error: failure}},
	}}
	sess := nativeJSONSession(t, model, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := sess.Prompt(ctx, "go")
		done <- err
	}()
	a := newTurnAccumulator()
	var sawRetry bool
	for !sawRetry {
		select {
		case ev := <-sess.Events():
			a.fold(ev)
			if ev.Type == agent.EventStepRetry {
				sawRetry = true
				cancel()
			}
		case err := <-done:
			t.Fatalf("the turn ended before its retry: %v", err)
		case <-time.After(30 * time.Second):
			t.Fatal("no step_retry")
		}
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancel did not end the backoff")
	}
	for _, ev := range drainNativeEvents(sess) {
		a.fold(ev)
	}
	if got := usageIncomplete(a.stepOpen); got == nil || *got {
		t.Fatalf("usageComplete = %v; want false: the retried step never reported", got)
	}
	if a.usage == nil {
		t.Fatal("the first step's report never arrived")
	}
	if msg, _ := a.message(); strings.Contains(msg, "abandoned") {
		t.Fatalf("message = %q", msg)
	}
}
