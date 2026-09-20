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
	"github.com/charliek/craze/internal/engine"
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
	// the drain told a blocked queue apart from an empty one.
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

// stubSession drives the run's edges directly. A signal landing while a row is
// queued, a refusal the foreign-turn snapshot has not caught up with, a turn
// that keeps emitting after its ending — none of them can be asked of a real
// agent on demand, and all of them are where the queue loses messages if the
// driver is wrong.
//
// It publishes through a real agent.EventLog, the same type every session
// publishes through, so Events() is that log's primary, the log numbers every
// event rather than the test choosing a seq, and the stub already carries the
// accessor a session needs to be driven at all (plan 021 §3.3: the engine
// refuses a session with no event log). Nothing here numbers, buffers or
// orders events on its own.
type stubSession struct {
	mu sync.Mutex
	// log is the session's event log and closed is its done signal, in the
	// live session's order: Close closes the signal first, so an emit blocked
	// on a primary nobody is reading gives its event up instead of waiting for
	// a reader that has gone.
	log     *agent.EventLog
	closed  chan struct{}
	queue   []agent.QueuedPrompt
	prompts []string
	turns   []stubTurn
	answers []string
	foreign bool
	// claimed is the session's prompt slot: Begin takes it and the
	// continuation releases it, so a second Begin while one is claimed is
	// refused exactly as a real session refuses it.
	claimed bool
	// nextID numbers the queue's rows. It is the queue's own counter and has
	// nothing to do with the log's sequence numbers.
	nextID int
	// eng is the engine driving this session, kept so that a test can read the
	// queue and the activity that are now the engine's (stillQueued). It is set
	// once, before the run it belongs to starts.
	eng *engine.Engine
	// onCancel runs inside Cancel, which is the last thing Stop does: by the
	// time it runs, a signal's queue removals have been enqueued and delivered.
	// It is the barrier a test lands a signal on.
	onCancel func()
	// onSnapshot runs once, before the first Snapshot answers, and is how a
	// test puts an event on the stream at a moment it can name rather than one
	// it guessed at with a sleep.
	onSnapshot func()
	// subagents is what Snapshot reports, so the end-of-run drain runs at all.
	subagents []agent.SubagentInfo
	// title is whatever SetTitle was handed, so the interface method has
	// somewhere to put it.
	title string
}

// stubTurn is one turn: what it emits before its continuation returns, and
// what that returns. Turns are used in order, the last repeating.
type stubTurn struct {
	// before runs on the turn's own goroutine before it emits anything, which is
	// where a test changes the session under the driver — the agent taking the
	// session for a turn of its own, a signal landing — at a moment it can name
	// rather than one it guessed at with a sleep.
	before func()
	emit   []agent.Event
	res    agent.Result
	err    error
}

// agent.LogOwner is the accessor the engine that takes this driver over refuses
// a session without (plan 021 §3.3). Asserting it here is what keeps the stub
// honest about publishing through a log instead of a channel of its own.
var (
	_ agent.Session     = (*stubSession)(nil)
	_ agent.EventSource = (*stubSession)(nil)
	_ agent.LogOwner    = (*stubSession)(nil)
)

// newStubSession builds a stub with its own event log. The log's goroutines
// and any emit still blocked on a full primary end with the test, whatever the
// test did or failed to do.
func newStubSession(t *testing.T, turns ...stubTurn) *stubSession {
	t.Helper()
	s := &stubSession{
		log:    agent.NewEventLog(agent.EventLogOptions{}),
		closed: make(chan struct{}),
		turns:  turns,
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func (s *stubSession) Start(context.Context) error { return nil }

// Begin is the session's claim, taken on the caller's goroutine, and the
// prompt is recorded here whether or not the claim succeeds — a real session
// records an attempt the same way. A Begin while the slot is claimed claims
// nothing and its continuation refuses, which is the contract the engine's
// admission is built on.
func (s *stubSession) Begin(text string) func(context.Context) (agent.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, text)
	if s.claimed {
		return func(context.Context) (agent.Result, error) { return agent.Result{}, agent.ErrPromptInFlight }
	}
	s.claimed = true
	var turn stubTurn
	if len(s.turns) > 0 {
		turn = s.turns[0]
		if len(s.turns) > 1 {
			s.turns = s.turns[1:]
		}
	}
	return func(context.Context) (agent.Result, error) { return s.runTurn(turn) }
}

// Prompt is Begin and its continuation back to back, as on every real session.
func (s *stubSession) Prompt(ctx context.Context, text string) (agent.Result, error) {
	return s.Begin(text)(ctx)
}

// runTurn is Begin's continuation. Emitting before it returns is what a
// session does, and it is what fills the log's primary while the only reader
// is waiting for this call.
func (s *stubSession) runTurn(turn stubTurn) (agent.Result, error) {
	defer func() {
		s.mu.Lock()
		s.claimed = false
		s.mu.Unlock()
	}()
	if turn.before != nil {
		turn.before()
	}
	for _, ev := range turn.emit {
		s.emit(ev)
	}
	return turn.res, turn.err
}

func (s *stubSession) Events() <-chan agent.Event { return s.log.Primary() }

// EventLog, Subscribe and Incarnation are the log's, as on every session: one
// log per session, and every reader of it goes through the log's own seams.
func (s *stubSession) EventLog() *agent.EventLog { return s.log }
func (s *stubSession) Subscribe(o agent.SubscribeOptions) (*agent.Subscription, error) {
	return s.log.Subscribe(o)
}
func (s *stubSession) Incarnation() string { return s.log.Incarnation() }

// Cancel is a no-op that reports nothing running: this stub's turns never
// wait on a cancel of their own, so every prompt it runs settles on its
// turns' own scripted result instead. It runs onCancel, which is where a test
// learns that a signal's whole path — refuse admission, clear the queue, deliver
// the removals — is behind it.
func (s *stubSession) Cancel(context.Context) (agent.CancelOutcome, error) {
	s.mu.Lock()
	hook := s.onCancel
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return agent.CancelOutcome{Settled: true}, nil
}

// ForeignTurn is the session contract's leaf accessor, over the same field
// Snapshot reports.
func (s *stubSession) ForeignTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.foreign
}

// setForeign is the agent taking the session for a turn of its own, or giving it
// back, exactly as a session reports it: the flag the driver reads, and then the
// event it wakes on.
func (s *stubSession) setForeign(running bool) {
	s.mu.Lock()
	s.foreign = running
	s.mu.Unlock()
	s.emit(agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{
		ID: "interject-fallback-1", Running: running,
	}})
}

// hold and engine keep the run's engine where a test can find it. The queue and
// the activity are the engine's now, and a test that asks what is left in the
// queue asks it (stillQueued).
func (s *stubSession) hold(eng *engine.Engine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eng = eng
}

func (s *stubSession) engine() *engine.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eng
}

func (s *stubSession) Queue(text string) (agent.QueuedPrompt, error) {
	s.mu.Lock()
	s.nextID++
	p := agent.QueuedPrompt{ID: fmt.Sprintf("q-%d", s.nextID), Text: text}
	s.queue = append(s.queue, p)
	pos := len(s.queue) - 1
	s.mu.Unlock()
	s.emit(agent.Event{Type: agent.EventQueue, Queue: &p, QueueChange: agent.QueueQueued, QueuePos: pos})
	return p, nil
}

func (s *stubSession) EditQueued(id, text string) error {
	s.mu.Lock()
	for i := range s.queue {
		if s.queue[i].ID != id {
			continue
		}
		s.queue[i].Text = text
		s.queue[i].Version++
		p := s.queue[i]
		s.mu.Unlock()
		s.emit(agent.Event{Type: agent.EventQueue, Queue: &p, QueueChange: agent.QueueEdited, QueuePos: i})
		return nil
	}
	s.mu.Unlock()
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
		s.emit(agent.Event{Type: agent.EventQueue, Queue: &p, QueueChange: change, QueuePos: i})
		return p, true
	}
	s.mu.Unlock()
	return agent.QueuedPrompt{}, false
}

func (s *stubSession) PopQueue() (agent.QueuedPrompt, bool) {
	s.mu.Lock()
	if len(s.queue) == 0 {
		s.mu.Unlock()
		return agent.QueuedPrompt{}, false
	}
	p := s.queue[0]
	s.queue = s.queue[1:]
	s.mu.Unlock()
	s.emit(agent.Event{Type: agent.EventQueue, Queue: &p, QueueChange: agent.QueueSent})
	return p, true
}

func (s *stubSession) ClearQueue() int {
	s.mu.Lock()
	rows := s.queue
	s.queue = nil
	s.mu.Unlock()
	for _, p := range rows {
		row := p
		s.emit(agent.Event{Type: agent.EventQueue, Queue: &row, QueueChange: agent.QueueRemoved})
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

// Close is the live session's order: the done signal first, so an emit blocked
// on a full primary gives its event up, then the log, which joins its own
// goroutines. It is idempotent, as the log's Close is.
func (s *stubSession) Close() error {
	s.mu.Lock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	s.mu.Unlock()
	s.log.Close(context.Background())
	return nil
}

// SetTitle is the interface's, and nothing more: the run never renames a
// session — /rename is the TUI's, and `craze prompt` has no title of its own.
func (s *stubSession) SetTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.title = title
}

func (s *stubSession) Snapshot() agent.Snapshot {
	s.mu.Lock()
	hook := s.onSnapshot
	s.onSnapshot = nil
	s.mu.Unlock()
	if hook != nil {
		// Outside the lock, because the hook publishes, and nothing publishes
		// with a session's own mutex held.
		hook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return agent.Snapshot{
		Queue:       append([]agent.QueuedPrompt(nil), s.queue...),
		ForeignTurn: s.foreign,
		Subagents:   append([]agent.SubagentInfo(nil), s.subagents...),
	}
}

// emit publishes through the log, as a live session's emit does: on this
// goroutine, so an emit that returned has its event in the primary's buffer —
// and blocking there once that buffer is full, which is the wedge
// TestTurnKeepsReadingWhileThePromptReturns is about.
func (s *stubSession) emit(ev agent.Event) {
	s.log.Publish(context.Background(), s.closed, ev)
}

// sent is every prompt handed to the session, in order, refused attempts
// included: it is how many times the driver tried, which is the observable
// half of a retry.
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

// stubWatchdog is how long a run below may make no progress before the test
// fails. It is not a timing assertion — nothing in a passing run waits this
// long — it is the difference between a readable failure and `go test` hanging
// until its own timeout kills the whole package.
const stubWatchdog = 10 * time.Second

// primaryBuffer is agent.EventLog's primary buffer, a constant 256 (see
// EventLog.Primary). A test that needs a publisher to be genuinely blocked has
// to emit more than this, and spelling the number here says why.
const primaryBuffer = 256

// stubEngine is the engine a headless run builds over a session: `craze prompt`'s
// own chain policy — a stop that is not end_turn ends the chain, and a prompt
// refused because the agent is running a turn of its own is waited out (plan 021
// §3.4) — started, and held where stillQueued can find it.
func stubEngine(s *stubSession) (*engine.Engine, error) {
	eng, err := engine.New(s, engine.Options{Chain: engine.ChainPolicy{
		StopOnNonEndTurn: true,
		RetryForeignTurn: true,
	}})
	if err != nil {
		return nil, err
	}
	s.hold(eng)
	if err := eng.Start(context.Background()); err != nil {
		_ = eng.Close()
		return nil, err
	}
	return eng, nil
}

// runChain is the one seam between these tests and whatever drives
// `craze prompt`'s turns. That is the engine now: drive() queues the follow-ups
// through it, submits the first prompt, prints its events until the chain is
// over, and makes the same final sweep run() makes, so what a test reads on
// stdout is what a script would get. Every test below asserts stdout, stderr,
// the exit status and what reached the session, and none of them names a driver
// function.
//
// It takes no *testing.T on purpose: a test may run it on a goroutine of its
// own — the only honest way to say "this run must not block" is to watch for it
// returning — and t.Fatal off the test's goroutine is not allowed. A follow-up
// the queue refuses comes back as the error run() would have returned.
func runChain(o *promptOpts, ctx context.Context, s *stubSession, text string, followUps ...string) error {
	eng, err := stubEngine(s)
	if err != nil {
		return err
	}
	// The engine's close is the session's, as run()'s deferred close is.
	defer func() { _ = eng.Close() }()
	o.followUps = followUps
	return o.drive(ctx, eng, text)
}

// stillQueued is what is left in the queue when a run is over, by text. It is
// the other half of runChain's seam: the queue is the engine's now, and this is
// the only function that followed it there.
func stillQueued(s *stubSession) []string {
	eng := s.engine()
	if eng == nil {
		return nil
	}
	var out []string
	for _, row := range eng.State().Queue {
		out = append(out, row.Text)
	}
	return out
}

// waitFor blocks until pred holds. It is a condition wait and not a timing
// assertion — nothing in a passing run waits anywhere near the watchdog — and
// the watchdog is the difference between a readable failure and `go test`
// hanging until it kills the package.
func waitFor(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(stubWatchdog)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatalf("still waiting for %s after %s", what, stubWatchdog)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSignalWithARowQueuedRemovesIt: a signal lands while the first turn runs
// with a follow-up behind it. Taking a row and starting its turn are one
// transaction in the engine, so the window the run used to have — a row out of
// the queue and its prompt not yet sent, which only craze itself could report —
// does not exist: the row is still queued when the signal arrives, the clear
// reports it removed, and that line is numbered like every other event of the
// session's stream (plan 021 A-X8).
func TestSignalWithARowQueuedRemovesIt(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The turn holds until the signal's own cancel reaches the session. Cancel is
	// the last thing a stop does — refuse admission, clear the queue, deliver the
	// removals, then cancel — so by then the row is gone from the queue and the
	// settlement this turn is about to run cannot drain it. The handshake is that
	// cancel, not a sleep.
	cancelled := make(chan struct{})
	first := stubTurn{
		before: func() { cancel(); <-cancelled },
		emit:   []agent.Event{{Type: agent.EventDone, StopReason: "cancelled"}},
		res:    agent.Result{StopReason: "cancelled"},
	}
	s := newStubSession(t, first)
	s.onCancel = func() { close(cancelled) }

	err := runChain(o, ctx, s, "first", "follow-up")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("a signal must end the run with exit 1: %v", err)
	}
	if got := s.sent(); len(got) != 1 || got[0] != "first" {
		t.Fatalf("a queued row must not be sent after a signal: %v", got)
	}
	var removed, sent int
	for _, ev := range parseJSONLines(t, stdout.String()) {
		if ev.m["type"] == "done" {
			if _, ok := ev.m["seq"].(float64); !ok {
				t.Fatalf("a line from the session's stream must carry a seq: %s", ev.raw)
			}
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
			if _, ok := ev.m["seq"].(float64); !ok {
				t.Fatalf("the removal is the engine's event now, and must carry a seq: %s", ev.raw)
			}
		case "sent":
			sent++
		}
	}
	if sent != 0 {
		t.Fatalf("a row the signal cleared was sent: %d sent lines\n%s", sent, stdout.String())
	}
	if removed != 1 {
		t.Fatalf("the cleared row must be reported removed once: %d lines\n%s", removed, stdout.String())
	}
}

// TestTheFinalSweepWaitsForTheOutbox: the run's last reads are non-blocking, and
// the engine publishes through the log's outbox rather than on the goroutine that
// called it, so an event it enqueued on the way out — a signal's queue removals
// above all — is not in the primary's buffer just because the call that caused it
// returned. The sweep waits for the outbox first (Sync, on a helper goroutine,
// while it keeps reading), so every line reaches stdout (plan 021 A19).
//
// The outbox here is genuinely stuck and not merely unlucky: the log's primary is
// full and nothing has read a single event, so its drainer cannot publish
// anything at all until the sweep starts reading.
func TestTheFinalSweepWaitsForTheOutbox(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	s := newStubSession(t)
	eng, err := stubEngine(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	o.eng = eng
	for i := 0; i < primaryBuffer; i++ {
		s.emit(agent.Event{Type: agent.EventText, Text: "x"})
	}
	const rows = 8
	for i := 0; i < rows; i++ {
		if _, err := eng.Queue(engine.Command{}, fmt.Sprintf("row-%d", i)); err != nil {
			t.Fatalf("queue: %v", err)
		}
	}
	decisions := []string{}
	if err := o.finishRun(s, &decisions, nil); err != nil {
		t.Fatalf("finishRun: %v", err)
	}
	var queued int
	for _, ev := range parseJSONLines(t, stdout.String()) {
		if ev.m["type"] == "queue" && ev.m["event"] == "queued" {
			queued++
		}
	}
	if queued != rows {
		t.Fatalf("%d of %d queue lines reached stdout\n%s", queued, rows, stderr.String())
	}
}

// TestAQueuedRowWaitsOutAForeignTurnAndThenRuns: the ending of a turn with a row
// behind it and a turn of the agent's own running says Next is empty and Pending
// is one, and that is not the end of the run. Nothing leaves the queue while the
// agent holds the session, and the row goes when that turn is over.
func TestAQueuedRowWaitsOutAForeignTurnAndThenRuns(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	// The wait ends because the agent's turn ends, not because time passed.
	o.foreignMax = time.Hour
	var s *stubSession
	first := endTurn()
	first.before = func() { s.setForeign(true) }
	s = newStubSession(t, first, endTurn())

	done := make(chan error, 1)
	go func() { done <- runChain(o, context.Background(), s, "first", "follow-up") }()
	// A turn has run, the engine is idle, and the row is still queued: the drain
	// is held. That state is the barrier for ending the agent's own turn.
	waitFor(t, "the drain to be held with the row still queued", func() bool {
		eng := s.engine()
		if eng == nil {
			return false
		}
		st := eng.State()
		return st.Prompted && st.Activity == engine.ActivityIdle && len(st.Queue) == 1
	})
	if got := s.sent(); len(got) != 1 {
		t.Fatalf("a row left the queue while the agent held the session: %v", got)
	}
	s.setForeign(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the row behind a finished foreign turn must run: %v", err)
		}
	case <-time.After(stubWatchdog):
		t.Fatal("the drain never ran the row the agent's own turn had held")
	}
	if got := s.sent(); strings.Join(got, ",") != "first,follow-up" {
		t.Fatalf("prompts %v", got)
	}
	var order []string
	for _, ev := range parseJSONLines(t, stdout.String()) {
		switch {
		case ev.m["type"] == "foreign_turn":
			order = append(order, "foreign-"+fmtString(ev.m["event"]))
		case ev.m["type"] == "queue" && ev.m["event"] == "sent":
			order = append(order, "sent")
		}
	}
	if strings.Join(order, ",") != "foreign-started,foreign-ended,sent" {
		t.Fatalf("the row must leave the queue after the foreign turn: %v\n%s", order, stdout.String())
	}
}

// TestAQueuedRowBlockedByAForeignTurnGivesUpWithAStatus: past the bound craze
// stops rather than waiting on a session the agent will not give back, and says
// so on stderr instead of exiting 0 with a follow-up that never ran. The row is
// left where it was: nothing pretends it was sent.
func TestAQueuedRowBlockedByAForeignTurnGivesUpWithAStatus(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	var s *stubSession
	first := endTurn()
	first.before = func() { s.setForeign(true) }
	s = newStubSession(t, first)

	err := runChain(o, context.Background(), s, "first", "follow-up")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(stderr.String(), "the queue is still blocked") {
		t.Fatalf("stderr %q", stderr.String())
	}
	if got := s.sent(); len(got) != 1 || got[0] != "first" {
		t.Fatalf("a row must not be sent into a turn of the agent's own: %v", got)
	}
	if got := stillQueued(s); strings.Join(got, ",") != "follow-up" {
		t.Fatalf("the row is still queued and unreported, as it always was: %v", got)
	}
}

// TestNextQueuedDoesNotWaitOutAForeignTurnForAnEmptyQueue: with nothing queued
// there is nothing to wait for. Waiting first would block a plain
// `craze prompt` on a fallback turn it has no stake in, and then exit 1.
//
// "Promptly" is structural here, not a stopwatch. The agent's own turn is
// never ended: nothing in this test ends it, and the budget is longer than any
// run of the suite, so a driver that waits for it to clear does not finish
// late — it does not finish, and the watchdog says so. A passing run waits on
// nothing at all.
func TestNextQueuedDoesNotWaitOutAForeignTurnForAnEmptyQueue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	o.foreignMax = time.Hour
	s := newStubSession(t, endTurn())
	// Set before the run starts, which is the happens-before edge to it.
	s.foreign = true
	done := make(chan error, 1)
	go func() { done <- runChain(o, context.Background(), s, "go") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("an empty queue is not a failure: %v", err)
		}
	case <-time.After(stubWatchdog):
		t.Fatal("the run waited out a turn nothing was queued behind")
	}
	if got := s.sent(); len(got) != 1 || got[0] != "go" {
		t.Fatalf("one turn, the caller's own: %v", got)
	}
	if stderr.String() != "" {
		t.Fatalf("a foreign turn with nothing queued behind it is not worth a word: %q", stderr.String())
	}
}

// TestForeignTurnRefusalIsRetriedUntilItClears: the snapshot the wait reads
// can lag the client, so the retry can be refused again. A single retry would
// hand the caller a raw ErrForeignTurn on a session that was about to be free.
// The refusal reached nothing, so a script sees no line for it either — only
// the turn that did run.
func TestForeignTurnRefusalIsRetriedUntilItClears(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	refused := stubTurn{err: agent.ErrForeignTurn}
	s := newStubSession(t, refused, refused, endTurn())
	if err := runChain(o, context.Background(), s, "go"); err != nil {
		t.Fatalf("the retry must outlast a lagging snapshot: %v", err)
	}
	if got := s.sent(); len(got) != 3 {
		t.Fatalf("%d attempts, want two refusals and a turn: %v", len(got), got)
	}
	evs := parseJSONLines(t, stdout.String())
	if len(evs) != 1 || evs[0].m["type"] != "done" {
		t.Fatalf("a refusal must print nothing:\n%s", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("a refusal craze recovered from is not worth a word: %q", stderr.String())
	}
}

// TestForeignTurnRefusalGivesUpWithAStatus: past the bound craze stops rather
// than looping on a session the agent will not give back, and it says so on
// stderr instead of handing a script a raw wire error.
func TestForeignTurnRefusalGivesUpWithAStatus(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	s := newStubSession(t, stubTurn{err: agent.ErrForeignTurn})
	err := runChain(o, context.Background(), s, "go")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(stderr.String(), "kept the session") {
		t.Fatalf("stderr %q", stderr.String())
	}
	if out := strings.TrimSpace(stdout.String()); out != "" {
		t.Fatalf("a run that never got a turn has nothing to print: %q", out)
	}
}

// TestNonEndTurnStopClearsTheQueue: a provider-side cancel ends the chain with
// rows still queued. Exiting without a word about them would leave the JSON
// stream saying a follow-up was queued and never anything else.
func TestNonEndTurnStopClearsTheQueue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	s := newStubSession(t, stubTurn{
		emit: []agent.Event{{Type: agent.EventDone, StopReason: "cancelled"}},
		res:  agent.Result{StopReason: "cancelled"},
	})
	err := runChain(o, context.Background(), s, "go", "one", "two")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err %v", err)
	}
	if got := stillQueued(s); len(got) != 0 {
		t.Fatalf("the queue outlived the run: %v", got)
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
// and waited for the turn to return would fill the session's event buffer and
// wedge the goroutine it was waiting on — with Close on the far side of it.
//
// The count is not the queue's 32. The stub publishes through a real event log
// whose primary buffer is a fixed 256, not a number a test can choose, so only
// a turn that emits more than that leaves the publisher genuinely blocked. At
// 33 events a reader that gave up at the error would still see the turn return
// and this test would pass on a wedge it had not reproduced.
func TestTurnKeepsReadingWhileThePromptReturns(t *testing.T) {
	const removals = primaryBuffer + 64
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	boom := errors.New("wire broke")
	emit := []agent.Event{{Type: agent.EventError, Err: boom}}
	for i := 0; i < removals; i++ {
		row := agent.QueuedPrompt{ID: fmt.Sprintf("q-%d", i), Text: "row"}
		emit = append(emit, agent.Event{Type: agent.EventQueue, Queue: &row, QueueChange: agent.QueueRemoved})
	}
	s := newStubSession(t, stubTurn{emit: emit, err: boom})
	done := make(chan error, 1)
	go func() { done <- runChain(o, context.Background(), s, "go") }()
	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(stubWatchdog):
		t.Fatal("the turn wedged: nothing read the events the error dragged behind it")
	}
	var removed int
	for _, ev := range parseJSONLines(t, stdout.String()) {
		if ev.m["type"] == "queue" && ev.m["event"] == "removed" {
			removed++
		}
	}
	if removed != removals {
		t.Fatalf("%d of %d removals reached stdout", removed, removals)
	}
}

// TestSubagentDrainAnswersPermissions: a request that arrives while the
// children's last events drain is the agent waiting on an answer like any
// other. Printing it and moving on would leave the agent hanging and let a run
// that refused a permission exit 0.
func TestSubagentDrainAnswersPermissions(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := stubOpts(&stdout, &stderr)
	s := newStubSession(t)
	s.subagents = []agent.SubagentInfo{{ID: "sub-1", Status: agent.SubagentCompleted}}
	// The request has to arrive once the drain is the only reader left: an
	// event already buffered would be answered by the flush ahead of it and
	// prove nothing about this one. The drain's own first Snapshot is exactly
	// that moment — the final flush takes none — so the hook is a barrier, not
	// a guess about how long a sleep has to be.
	s.onSnapshot = func() {
		s.emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{
			ID:   "perm-1",
			Tool: "Shell",
			Options: []agent.PermissionOption{
				{OptionID: "opt-allow", Kind: "allow_once"},
				{OptionID: "opt-reject", Kind: "reject_once"},
			},
		}})
	}
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
