package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// TestPromptFollowUpsAreTheQueue: every follow-up is queued before the first
// turn and sent one per settled turn, in order, with a line for each change.
func TestPromptFollowUpsAreTheQueue(t *testing.T) {
	evs := runPromptJSONArgs(t, "echo",
		[]string{"--follow-up", "Reply PINEAPPLE", "--follow-up", "Reply MANGO"}, "one")

	var dones int
	for _, ev := range evs {
		if ev.m["type"] == "done" {
			dones++
		}
	}
	if dones != 3 {
		t.Fatalf("want three done, got %d", dones)
	}

	var queued, sent []map[string]any
	for _, ev := range evs {
		if ev.m["type"] != "queue" {
			continue
		}
		switch ev.m["event"] {
		case "queued":
			queued = append(queued, ev.m)
		case "sent":
			sent = append(sent, ev.m)
		default:
			t.Fatalf("unexpected queue event %s", ev.raw)
		}
	}
	if len(queued) != 2 || len(sent) != 2 {
		t.Fatalf("queued %d sent %d", len(queued), len(sent))
	}
	if queued[0]["text"] != "Reply PINEAPPLE" || queued[0]["position"] != float64(0) {
		t.Fatalf("first queued %v", queued[0])
	}
	if queued[1]["text"] != "Reply MANGO" || queued[1]["position"] != float64(1) {
		t.Fatalf("second queued %v", queued[1])
	}
	if sent[0]["id"] != queued[0]["id"] || sent[1]["id"] != queued[1]["id"] {
		t.Fatalf("sent out of order: %v %v", sent[0], sent[1])
	}
	// Each row is the head when it goes.
	for i, s := range sent {
		if s["position"] != float64(0) {
			t.Fatalf("sent %d position %v", i, s["position"])
		}
	}
	// The turns ran in the queued order.
	var replies []string
	for _, ev := range evs {
		if ev.m["type"] == "text" {
			replies = append(replies, fmtString(ev.m["text"]))
		}
	}
	joined := strings.Join(replies, "")
	if strings.Index(joined, "PINEAPPLE") > strings.Index(joined, "MANGO") {
		t.Fatalf("replies out of order: %q", joined)
	}
}

// TestPromptPlainModeWritesNoQueueLines: the queue is JSON-only chrome.
func TestPromptPlainModeWritesNoQueueLines(t *testing.T) {
	isolateRunEnv(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"prompt", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir(),
		"--follow-up", "second", "one",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "queue") || strings.Contains(out, "\"event\"") {
		t.Fatalf("plain stdout carries queue chrome: %q", out)
	}
	if !strings.Contains(out, "echo: one") || !strings.Contains(out, "second") {
		t.Fatalf("both turns must still run: %q", out)
	}
}

// TestPromptCancelledFirstTurnStopsTheChain: the stop reason still ends the
// run with exit 1 and no queued turn behind it.
func TestPromptCancelledFirstTurnStopsTheChain(t *testing.T) {
	isolateRunEnv(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "hang")
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"prompt", "--json", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir(),
		"--follow-up", "never runs", "go",
	})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	// The hang script waits for session/cancel; the signal path is what the
	// context cancel stands in for here.
	time.Sleep(300 * time.Millisecond)
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("prompt hung")
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err %v", err)
	}
	evs := parseJSONLines(t, stdout.String())
	var removed, sent, dones int
	for _, ev := range evs {
		switch {
		case ev.m["type"] == "queue" && ev.m["event"] == "removed":
			removed++
		case ev.m["type"] == "queue" && ev.m["event"] == "sent":
			sent++
		case ev.m["type"] == "done":
			dones++
		}
	}
	if removed != 1 {
		t.Fatalf("the signal must clear the queue: %d removed\n%s", removed, stdout.String())
	}
	if sent != 0 {
		t.Fatalf("a cleared row must never be sent: %d", sent)
	}
	if dones != 1 {
		t.Fatalf("%d done: the chain must stop", dones)
	}
}

// TestPromptForeignTurnIsWaitedOut is the grok fallback through the CLI. The
// fake strands an interjection of its own as the first turn ends — craze never
// interjects headlessly, but grok will mint a fallback for an interjection
// craze did not send — and the queued follow-up must run after that turn, not
// into it.
func TestPromptForeignTurnIsWaitedOut(t *testing.T) {
	evs := runPromptJSONArgs(t, "grok-long-turn-fallback",
		[]string{"--provider", "grok", "--follow-up", "Reply PINEAPPLE"},
		"do the steps STRAND-INTERJECTION")
	var started, ended, dones int
	var order []string
	for _, ev := range evs {
		switch {
		case ev.m["type"] == "foreign_turn" && ev.m["event"] == "started":
			started++
			order = append(order, "foreign-start")
		case ev.m["type"] == "foreign_turn" && ev.m["event"] == "ended":
			ended++
			order = append(order, "foreign-end")
		case ev.m["type"] == "done":
			dones++
			order = append(order, "done")
		case ev.m["type"] == "queue" && ev.m["event"] == "sent":
			order = append(order, "sent")
		}
	}
	if dones != 2 {
		t.Fatalf("want one done per craze prompt, got %d\n%v", dones, order)
	}
	if started != 1 || ended != 1 {
		t.Fatalf("foreign turn: %d started, %d ended\n%v", started, ended, order)
	}
	joined := strings.Join(order, ",")
	if !strings.Contains(joined, "foreign-end,sent") {
		t.Fatalf("the queued message must be sent after the foreign turn: %v", order)
	}
	if strings.Contains(joined, "foreign-start,sent") {
		t.Fatalf("a message left the queue while the agent held the session: %v", order)
	}
	// The interjection grok could not merge is still shown as a user block.
	pick(t, evs, "user", func(m map[string]any) bool { return m["interjection"] == true })
}

// TestQueueFullRefusesTheFollowUp: the bound is the session's, and the CLI
// reports it rather than silently dropping a message.
func TestQueueFullRefusesTheFollowUp(t *testing.T) {
	isolateRunEnv(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	args := []string{"prompt", "--json", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir()}
	for i := 0; i < 33; i++ {
		args = append(args, "--follow-up", "x")
	}
	args = append(args, "one")
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "queue is full") {
		t.Fatalf("err %v", err)
	}
}

// TestPromptAnswersPermissionOutsideATurn: a permission request that arrives
// while nothing is reading a turn's own stream — between turns, or during a
// turn the agent started on its own — is still the agent waiting for an
// answer, so every reader of the stream answers it.
func TestPromptAnswersPermissionDuringAForeignTurn(t *testing.T) {
	isolateRunEnv(t)
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	t.Setenv("CRAZE_FAKE_SCRIPT", "grok-long-turn-fallback")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"prompt", "--json", "--provider", "grok", "--agent-bin", fakeAgentPath(t),
		"--workspace", t.TempDir(), "--follow-up", "Reply PINEAPPLE",
		"do the steps STRAND-INTERJECTION",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt: %v\nstderr: %s", err, stderr.String())
	}
	evs := parseJSONLines(t, stdout.String())
	// The queued follow-up ran after the foreign turn, which is only true if
	// nextQueued told "blocked" apart from "empty".
	var sentAfterEnd bool
	var ended bool
	for _, ev := range evs {
		if ev.m["type"] == "foreign_turn" && ev.m["event"] == "ended" {
			ended = true
		}
		if ended && ev.m["type"] == "queue" && ev.m["event"] == "sent" {
			sentAfterEnd = true
		}
	}
	if !sentAfterEnd {
		t.Fatalf("the follow-up must run after the foreign turn:\n%s", stdout.String())
	}
}

// TestPermissionRejectedBetweenTurnsStillFailsTheRun: the queue gave the
// event stream three readers, and a permission refused by any of them is
// still a refusal. A run that said no and then exited 0 would tell a script
// the opposite of what happened.
func TestPermissionRejectedBetweenTurnsStillFailsTheRun(t *testing.T) {
	isolateRunEnv(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "permission")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"prompt", "--json", "--no-force", "--agent-bin", fakeAgentPath(t),
		"--workspace", t.TempDir(),
		"--permission-decision", "reject-once",
		"--follow-up", "second", "go",
	})
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("a rejected permission must fail the run: %v", err)
	}
}

// stubSession drives the run loop's edges directly. A signal landing between a
// row leaving the queue and the prompt going out, a refusal the foreign-turn
// snapshot has not caught up with, a turn that keeps emitting after its
// ending — none of them can be asked of a real agent on demand, and all of
// them are where the queue loses messages if the loop is wrong.
type stubSession struct {
	mu      sync.Mutex
	events  chan agent.Event
	queue   []agent.QueuedPrompt
	prompts []string
	turns   []stubTurn
	answers []string
	foreign bool
	seq     int
	// onPop runs inside PopQueue before the row leaves the queue: it is where
	// the tests land a signal in the one window the loop cannot see.
	onPop func()
	// subagents is what Snapshot reports, so the end-of-run drain runs at all.
	subagents []agent.SubagentInfo
	// title is whatever SetTitle was handed, so the interface method has
	// somewhere to put it.
	title string
}

// stubTurn is one Prompt: what it emits before it returns, and what it
// returns. Turns are used in order, the last repeating.
type stubTurn struct {
	emit []agent.Event
	res  agent.Result
	err  error
}

func newStubSession(buffer int, turns ...stubTurn) *stubSession {
	return &stubSession{events: make(chan agent.Event, buffer), turns: turns}
}

func (s *stubSession) Start(context.Context) error { return nil }

func (s *stubSession) Prompt(_ context.Context, text string) (agent.Result, error) {
	s.mu.Lock()
	s.prompts = append(s.prompts, text)
	var turn stubTurn
	if len(s.turns) > 0 {
		turn = s.turns[0]
		if len(s.turns) > 1 {
			s.turns = s.turns[1:]
		}
	}
	s.mu.Unlock()
	// Emitting before the return is what the session does, and it is what
	// fills the buffer while the only reader is waiting for this call.
	for _, ev := range turn.emit {
		s.events <- ev
	}
	return turn.res, turn.err
}

// Begin is here for the interface: the run loop prompts through Prompt, and a
// continuation that is Prompt itself records the same.
func (s *stubSession) Begin(text string) func(context.Context) (agent.Result, error) {
	return func(ctx context.Context) (agent.Result, error) { return s.Prompt(ctx, text) }
}

func (s *stubSession) Events() <-chan agent.Event { return s.events }
func (s *stubSession) Cancel(context.Context) error {
	return nil
}

func (s *stubSession) Queue(text string) (agent.QueuedPrompt, error) {
	s.mu.Lock()
	s.seq++
	p := agent.QueuedPrompt{ID: fmt.Sprintf("q-%d", s.seq), Text: text}
	s.queue = append(s.queue, p)
	pos := len(s.queue) - 1
	s.mu.Unlock()
	s.events <- agent.Event{Type: agent.EventQueue, Queue: &p, QueueChange: agent.QueueQueued, QueuePos: pos}
	return p, nil
}

func (s *stubSession) EditQueued(id, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.queue {
		if s.queue[i].ID != id {
			continue
		}
		s.queue[i].Text = text
		s.queue[i].Version++
		p := s.queue[i]
		s.events <- agent.Event{Type: agent.EventQueue, Queue: &p, QueueChange: agent.QueueEdited, QueuePos: i}
		return nil
	}
	return fmt.Errorf("no queued message %q", id)
}

func (s *stubSession) Unqueue(id string) (agent.QueuedPrompt, bool) {
	return s.take(id, agent.QueueRemoved)
}

func (s *stubSession) TakeQueued(id string) (agent.QueuedPrompt, bool) {
	return s.take(id, agent.QueueSent)
}

func (s *stubSession) take(id string, change agent.QueueChange) (agent.QueuedPrompt, bool) {
	s.mu.Lock()
	for i := range s.queue {
		if s.queue[i].ID != id {
			continue
		}
		p := s.queue[i]
		s.queue = append(s.queue[:i], s.queue[i+1:]...)
		s.mu.Unlock()
		s.events <- agent.Event{Type: agent.EventQueue, Queue: &p, QueueChange: change, QueuePos: i}
		return p, true
	}
	s.mu.Unlock()
	return agent.QueuedPrompt{}, false
}

func (s *stubSession) PopQueue() (agent.QueuedPrompt, bool) {
	s.mu.Lock()
	pop := s.onPop
	s.mu.Unlock()
	if pop != nil {
		pop()
	}
	s.mu.Lock()
	if len(s.queue) == 0 {
		s.mu.Unlock()
		return agent.QueuedPrompt{}, false
	}
	p := s.queue[0]
	s.queue = s.queue[1:]
	s.mu.Unlock()
	s.events <- agent.Event{Type: agent.EventQueue, Queue: &p, QueueChange: agent.QueueSent}
	return p, true
}

func (s *stubSession) ClearQueue() int {
	s.mu.Lock()
	rows := s.queue
	s.queue = nil
	s.mu.Unlock()
	for _, p := range rows {
		row := p
		s.events <- agent.Event{Type: agent.EventQueue, Queue: &row, QueueChange: agent.QueueRemoved}
	}
	return len(rows)
}

func (s *stubSession) Interject(context.Context, string) error { return agent.ErrUnsupported }

func (s *stubSession) AnswerPermission(_, optionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers = append(s.answers, optionID)
	return nil
}

func (s *stubSession) AnswerQuestion(string, map[string][]string, bool) error { return nil }
func (s *stubSession) AnswerPlan(string, bool) error                          { return nil }
func (s *stubSession) SetModel(context.Context, string) error                 { return nil }
func (s *stubSession) SetMode(context.Context, string) error                  { return nil }
func (s *stubSession) SetConfig(context.Context, string, string) error        { return nil }
func (s *stubSession) Close() error                                           { return nil }

// SetTitle is the interface's, and nothing more: the run loop never renames a
// session — /rename is the TUI's, and `craze prompt` has no title of its own.
func (s *stubSession) SetTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.title = title
}

func (s *stubSession) Snapshot() agent.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return agent.Snapshot{
		Queue:       append([]agent.QueuedPrompt(nil), s.queue...),
		ForeignTurn: s.foreign,
		Subagents:   append([]agent.SubagentInfo(nil), s.subagents...),
	}
}

func (s *stubSession) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

// stubOpts is a headless promptOpts writing JSON to stdout, with the
// foreign-turn bound short enough to test.
func stubOpts(stdout, stderr *bytes.Buffer) *promptOpts {
	return &promptOpts{json: true, stdout: stdout, stderr: stderr, foreignMax: 100 * time.Millisecond}
}

func endTurn() stubTurn {
	return stubTurn{
		emit: []agent.Event{{Type: agent.EventDone, StopReason: "end_turn"}},
		res:  agent.Result{StopReason: "end_turn"},
	}
}

// TestSignalBetweenTheTakeAndTheSendRemovesTheRow: the row is taken, then the
// signal lands. Sending it now would start a whole turn on a context no second
// Ctrl+C could cancel, and the row is already out of the queue, so the clear
// the signal ran cannot report it. It is reported removed here instead.
//
// That line is craze's own, and it is where the no-`seq` rule (plan 020 §3.6,
// A21) is visible from the outside: the turn's event is numbered here — the
// stub numbers that one, standing in for the log a real session publishes
// through — and its line carries the number, while the removed line, which no
// session ever saw, carries no `seq` key at all.
func TestSignalBetweenTheTakeAndTheSendRemovesTheRow(t *testing.T) {
	const doneSeq = 77
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	s := newStubSession(64, stubTurn{
		emit: []agent.Event{{Type: agent.EventDone, StopReason: "end_turn", Seq: doneSeq}},
		res:  agent.Result{StopReason: "end_turn"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.onPop = func() { cancel() }
	if _, err := s.Queue("follow-up"); err != nil {
		t.Fatal(err)
	}
	err := o.runLoop(ctx, s, "first", &[]string{})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("a signal must end the run with exit 1: %v", err)
	}
	if got := s.sent(); len(got) != 1 || got[0] != "first" {
		t.Fatalf("the taken row must not be sent: %v", got)
	}
	if _, err := o.flushEvents(s, &[]string{}); err != nil {
		t.Fatal(err)
	}
	evs := parseJSONLines(t, stdout.String())
	var removed, sent int
	for _, ev := range evs {
		if ev.m["type"] == "done" && ev.m["seq"] != float64(doneSeq) {
			t.Fatalf("the session's own line lost its seq: %s", ev.raw)
		}
		if ev.m["type"] != "queue" {
			continue
		}
		switch ev.m["event"] {
		case "removed":
			removed++
			if ev.m["text"] != "follow-up" {
				t.Fatalf("removed the wrong row: %s", ev.raw)
			}
			if _, ok := ev.m["seq"]; ok {
				t.Fatalf("a line craze wrote itself must carry no seq: %s", ev.raw)
			}
		case "sent":
			sent++
		}
	}
	if sent != 1 {
		t.Fatalf("the row did leave the queue: %d sent lines\n%s", sent, stdout.String())
	}
	if removed != 1 {
		t.Fatalf("a row taken and not sent must be reported removed: %d lines\n%s", removed, stdout.String())
	}
}

// TestNextQueuedDoesNotWaitOutAForeignTurnForAnEmptyQueue: with nothing
// queued there is nothing to wait for. Waiting first would block a plain
// `craze prompt` on a fallback turn it has no stake in, and then exit 1.
func TestNextQueuedDoesNotWaitOutAForeignTurnForAnEmptyQueue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	s := newStubSession(8)
	s.foreign = true
	start := time.Now()
	_, ok, rejected, err := o.nextQueued(context.Background(), s, &[]string{})
	if err != nil {
		t.Fatalf("an empty queue is not a failure: %v", err)
	}
	if ok || rejected {
		t.Fatalf("ok=%v rejected=%v", ok, rejected)
	}
	if time.Since(start) > o.foreignBudget() {
		t.Fatalf("waited %s on a turn nothing was queued behind", time.Since(start))
	}
}

// TestForeignTurnRefusalIsRetriedUntilItClears: the snapshot the wait reads
// can lag the client, so the retry can be refused again. A single retry would
// hand the caller a raw ErrForeignTurn on a session that was about to be free.
func TestForeignTurnRefusalIsRetriedUntilItClears(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	refused := stubTurn{err: agent.ErrForeignTurn}
	s := newStubSession(8, refused, refused, endTurn())
	if err := o.runLoop(context.Background(), s, "go", &[]string{}); err != nil {
		t.Fatalf("the retry must outlast a lagging snapshot: %v", err)
	}
	if got := s.sent(); len(got) != 3 {
		t.Fatalf("%d attempts, want two refusals and a turn: %v", len(got), got)
	}
}

// TestForeignTurnRefusalGivesUpWithAStatus: past the bound craze stops rather
// than looping on a session the agent will not give back, and it says so on
// stderr instead of handing a script a raw wire error.
func TestForeignTurnRefusalGivesUpWithAStatus(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	s := newStubSession(8, stubTurn{err: agent.ErrForeignTurn})
	err := o.runLoop(context.Background(), s, "go", &[]string{})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(stderr.String(), "kept the session") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

// TestNonEndTurnStopClearsTheQueue: a provider-side cancel ends the chain with
// rows still queued. Exiting without a word about them would leave the JSON
// stream saying a follow-up was queued and never anything else.
func TestNonEndTurnStopClearsTheQueue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	s := newStubSession(64, stubTurn{
		emit: []agent.Event{{Type: agent.EventDone, StopReason: "cancelled"}},
		res:  agent.Result{StopReason: "cancelled"},
	})
	for _, text := range []string{"one", "two"} {
		if _, err := s.Queue(text); err != nil {
			t.Fatal(err)
		}
	}
	err := o.runLoop(context.Background(), s, "go", &[]string{})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err %v", err)
	}
	if got := s.Snapshot().Queue; len(got) != 0 {
		t.Fatalf("the queue outlived the run: %+v", got)
	}
	var removed []string
	for _, ev := range parseJSONLines(t, stdout.String()) {
		if ev.m["type"] == "queue" && ev.m["event"] == "removed" {
			removed = append(removed, fmtString(ev.m["text"]))
		}
	}
	if strings.Join(removed, ",") != "one,two" {
		t.Fatalf("removed %v, want both rows head first\n%s", removed, stdout.String())
	}
	if len(s.sent()) != 1 {
		t.Fatalf("a stopped chain must not run the queue: %v", s.sent())
	}
}

// TestTurnKeepsReadingWhileThePromptReturns: the error path emits the queue's
// removals after the error, one per row. A reader that stopped at the error
// and waited for Prompt to return would fill the session's buffer and wedge
// the goroutine it was waiting on — with Close on the far side of it.
func TestTurnKeepsReadingWhileThePromptReturns(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	boom := errors.New("wire broke")
	emit := []agent.Event{{Type: agent.EventError, Err: boom}}
	for i := 0; i < 32; i++ {
		row := agent.QueuedPrompt{ID: fmt.Sprintf("q-%d", i), Text: "row"}
		emit = append(emit, agent.Event{Type: agent.EventQueue, Queue: &row, QueueChange: agent.QueueRemoved})
	}
	// A buffer smaller than what the turn emits is the whole point: the turn
	// cannot return until someone reads.
	s := newStubSession(4, stubTurn{emit: emit, err: boom})
	done := make(chan error, 1)
	go func() { done <- o.runLoop(context.Background(), s, "go", &[]string{}) }()
	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the turn wedged: nothing read the events the error dragged behind it")
	}
	var removed int
	for _, ev := range parseJSONLines(t, stdout.String()) {
		if ev.m["type"] == "queue" && ev.m["event"] == "removed" {
			removed++
		}
	}
	if removed != 32 {
		t.Fatalf("%d of 32 removals reached stdout", removed)
	}
}

// TestSubagentDrainAnswersPermissions: a request that arrives while the
// children's last events drain is the agent waiting on an answer like any
// other. Printing it and moving on would leave the agent hanging and let a run
// that refused a permission exit 0.
func TestSubagentDrainAnswersPermissions(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	s := newStubSession(8)
	s.subagents = []agent.SubagentInfo{{ID: "sub-1", Status: agent.SubagentCompleted}}
	// The request arrives once the drain is the only reader left: an event
	// already buffered would be answered by the flush ahead of it and prove
	// nothing about this one.
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.events <- agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{
			ID:   "perm-1",
			Tool: "Shell",
			Options: []agent.PermissionOption{
				{OptionID: "opt-allow", Kind: "allow_once"},
				{OptionID: "opt-reject", Kind: "reject_once"},
			},
		}}
	}()
	decisions := []string{"reject-once"}
	err := o.finishRun(s, &decisions, nil)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("a refusal during the drain must fail the run: %v", err)
	}
	s.mu.Lock()
	answers := append([]string(nil), s.answers...)
	s.mu.Unlock()
	if len(answers) != 1 || answers[0] != "opt-reject" {
		t.Fatalf("the drain must answer the request: %v", answers)
	}
	if len(decisions) != 0 {
		t.Fatalf("the decision must be spent: %v", decisions)
	}
}

// TestQueueJSONCarriesTheEditedVersion: an edit keeps the row's id and
// position and bumps its version, which is how a consumer tells an edited row
// from a new one. This is a unit test of the encoder because the headless CLI
// has no edit path — --follow-up only queues, and nothing else takes an id —
// so the session's EditQueued has no caller to drive here.
func TestQueueJSONCarriesTheEditedVersion(t *testing.T) {
	row := agent.QueuedPrompt{ID: "q-1", Text: "ONE", Version: 1}
	j := queueJSON(agent.Event{
		Type:        agent.EventQueue,
		Queue:       &row,
		QueueChange: agent.QueueEdited,
		QueuePos:    2,
	})
	if j.Type != "queue" || j.Event != "edited" {
		t.Fatalf("%+v", j)
	}
	if j.ID != "q-1" || j.Position != 2 || j.Version != 1 || j.Text != "ONE" {
		t.Fatalf("%+v", j)
	}
	var buf bytes.Buffer
	if err := encodeEvent(&buf, agent.Event{
		Type:        agent.EventQueue,
		Queue:       &row,
		QueueChange: agent.QueueEdited,
		QueuePos:    2,
	}); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(buf.String())
	for _, want := range []string{`"type":"queue"`, `"event":"edited"`, `"id":"q-1"`, `"version":1`, `"text":"ONE"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %s missing %s", line, want)
		}
	}
}
