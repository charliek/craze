package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The claude foreign-turn state machine (plan 052 § E1) and the child
// registry (§ E3, § G1), driven over a raw pipe with a fake clock: every timer
// fires when the test says, and each test asserts the exact sequence of
// announcements and updates the client handed on.

// fakeClock is a Clock whose timers fire only in Advance, on the caller's
// goroutine, in deadline order.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at             time.Time
	d              time.Duration
	f              func()
	stopped, fired bool
	clk            *fakeClock
}

func (t *fakeTimer) Stop() bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	live := !t.stopped && !t.fired
	t.stopped = true
	return live
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), d: d, f: f, clk: c}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves the clock on by d and runs every live timer now due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	for _, t := range c.timers {
		if !t.stopped && !t.fired && !t.at.After(c.now) {
			t.fired = true
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	sort.SliceStable(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
	for _, t := range due {
		t.f()
	}
}

// lastFunc is the newest timer's callback, armed for d, whatever became of it:
// a test calls it by hand to play a callback that escaped its Stop.
func (c *fakeClock) lastFunc(d time.Duration) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.timers) - 1; i >= 0; i-- {
		if c.timers[i].d == d {
			return c.timers[i].f
		}
	}
	return nil
}

type rigLog struct {
	mu  sync.Mutex
	evs []string
}

func (l *rigLog) add(s string) {
	l.mu.Lock()
	l.evs = append(l.evs, s)
	l.mu.Unlock()
}

func (l *rigLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.evs...)
}

func foreignLine(f ForeignTurn) string {
	if f.Running {
		return "start " + f.ID + " " + f.Reason
	}
	s := "end " + f.ID + " " + f.Reason
	if f.Cancelled {
		s += " cancelled"
	}
	if f.Silent {
		s += " silent"
	}
	if f.Err != nil {
		s += " err"
	}
	return s
}

type claudeRig struct {
	*rawPipe
	// root is the session id session/new answers: rigRoot unless a fixture
	// names its own.
	root string
	clk  *fakeClock
	log  *rigLog
	// subs are the sub-agent notifications, in order.
	mu   sync.Mutex
	subs []SubagentNotification
}

const rigRoot = "root"

func newClaudeRig(t *testing.T) *claudeRig {
	t.Helper()
	p := newRawPipeDialect(t, DialectClaude)
	r := &claudeRig{rawPipe: p, root: rigRoot, clk: newFakeClock(), log: &rigLog{}}
	p.client.SetClock(r.clk)
	p.client.SetUpdateHandler(func(n SessionNotification) {
		k := sessionUpdateKind(n.Update)
		if n.Child != "" {
			k += "@" + n.Child
		}
		r.log.add(k)
	})
	p.client.SetForeignTurnHandler(func(f ForeignTurn) { r.log.add(foreignLine(f)) })
	p.client.SetSubagentHandler(func(n SubagentNotification) {
		r.mu.Lock()
		r.subs = append(r.subs, n)
		r.mu.Unlock()
		s := "sub " + n.ChildSessionID + " " + n.State
		if n.Evicted {
			s += " evicted"
		}
		r.log.add(s)
	})
	return r
}

// ready runs session/new to its reply, pushing pre before the reply (they are
// buffered: no session yet), so the machine is ready when it returns.
func (r *claudeRig) ready(t *testing.T, pre ...string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := r.client.NewSession(context.Background(), t.TempDir(), SessionParams{})
		done <- err
	}()
	req := r.readWithin(t, 3*time.Second, "session/new")
	for _, u := range pre {
		r.pushUpdate(t, r.root, u)
	}
	r.answerWith(t, req, `{"sessionId":"`+r.root+`"}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// push writes root updates and waits until the client has dispatched them.
func (r *claudeRig) push(t *testing.T, updates ...string) {
	t.Helper()
	r.pushOn(t, rigRoot, updates...)
}

// pushOn writes updates on sid and waits until they are dispatched.
func (r *claudeRig) pushOn(t *testing.T, sid string, updates ...string) {
	t.Helper()
	for _, u := range updates {
		r.pushUpdate(t, sid, u)
	}
	r.roundTrip(t)
}

func (r *claudeRig) want(t *testing.T, want ...string) {
	t.Helper()
	got := r.log.get()
	if !slices.Equal(got, want) {
		t.Fatalf("the client handed on\n %q\nwant\n %q", got, want)
	}
}

// prompt sends a prompt and answers the request it reads, returning the
// request and the call's result channel.
func (r *claudeRig) prompt(t *testing.T) (*Message, chan error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := r.client.Prompt(context.Background(), "go")
		done <- err
	}()
	return r.readWithin(t, 3*time.Second, "session/prompt"), done
}

func chunk(text string) string {
	return `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":` + fmt.Sprintf("%q", text) + `}}`
}

const (
	usageNoCost = `{"sessionUpdate":"usage_update","used":1,"size":2}`
	usageCost   = `{"sessionUpdate":"usage_update","used":1,"size":2,"cost":{"amount":0.1,"currency":"USD"},"_meta":{"_claude/origin":{"kind":"task-notification"}}}`
)

func toolCall(id, status string) string {
	return `{"sessionUpdate":"tool_call","toolCallId":"` + id + `","status":"` + status + `","title":"t","kind":"execute","_meta":{"claudeCode":{"toolName":"Bash"}}}`
}

func toolUpd(id, status string) string {
	return `{"sessionUpdate":"tool_call_update","toolCallId":"` + id + `","status":"` + status + `"}`
}

var grace = DefaultForeignTiming.Grace

// TestClaudeForeignWakeEndsAfterTheGrace: a wake's content opens one foreign
// turn ahead of the content, its cost usage makes a candidate, and the turn
// ends only once the quiet grace has passed.
func TestClaudeForeignWakeEndsAfterTheGrace(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.push(t, usageNoCost, chunk("banana"), usageCost)
	r.clk.Advance(grace - time.Millisecond)
	r.want(t, "usage_update", "start claude-1 background", "agent_message_chunk", "usage_update")
	r.clk.Advance(time.Millisecond)
	r.want(t, "usage_update", "start claude-1 background", "agent_message_chunk", "usage_update", "end claude-1 background")
	if got := r.client.ForeignPhase(); got != "idle" {
		t.Fatalf("phase %s after the ending", got)
	}
}

// TestClaudeForeignPlaceholderWithinTheGrace: a num_turns:0 placeholder's
// usage followed by content within the grace is one turn; only the follow-up's
// own cost usage ends it.
func TestClaudeForeignPlaceholderWithinTheGrace(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.push(t, chunk("a"), usageCost)
	r.clk.Advance(grace / 2)
	r.push(t, chunk("follow-up"))
	r.clk.Advance(10 * grace)
	r.want(t, "start claude-1 background", "agent_message_chunk", "usage_update", "agent_message_chunk")
	r.push(t, usageCost)
	r.clk.Advance(grace)
	r.want(t, "start claude-1 background", "agent_message_chunk", "usage_update", "agent_message_chunk", "usage_update", "end claude-1 background")
}

// TestClaudeForeignPlaceholderAfterTheGrace: a follow-up whose first content
// comes after the grace opens a second foreign turn (the v1 contract, R2-3).
func TestClaudeForeignPlaceholderAfterTheGrace(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.push(t, chunk("a"), usageCost)
	r.clk.Advance(grace)
	r.push(t, chunk("follow-up"))
	r.want(t, "start claude-1 background", "agent_message_chunk", "usage_update", "end claude-1 background",
		"start claude-2 background", "agent_message_chunk")
}

// TestClaudeForeignFollowUpAbsorbedByAQueuedPrompt: when a queued prompt goes
// out between the placeholder's ending and the follow-up, the follow-up is the
// prompted turn's and opens nothing; after the prompt's reply, content opens.
func TestClaudeForeignFollowUpAbsorbedByAQueuedPrompt(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.push(t, chunk("a"), usageCost)
	r.clk.Advance(grace)
	req, done := r.prompt(t)
	r.push(t, chunk("follow-up"))
	r.answerWith(t, req, `{"stopReason":"end_turn"}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.push(t, chunk("later"))
	r.want(t, "start claude-1 background", "agent_message_chunk", "usage_update", "end claude-1 background",
		"agent_message_chunk", "start claude-2 background", "agent_message_chunk")
}

// TestClaudeForeignNeverCompletesWithAToolOpen: a candidate waits for the open
// tool call's close, then the grace.
func TestClaudeForeignNeverCompletesWithAToolOpen(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.push(t, chunk("a"), toolCall("t1", "in_progress"), usageCost)
	r.clk.Advance(30 * grace)
	if slices.Contains(r.log.get(), "end claude-1 background") {
		t.Fatal("the turn ended with its tool call open")
	}
	r.push(t, toolUpd("t1", "completed"))
	r.clk.Advance(grace - time.Millisecond)
	if slices.Contains(r.log.get(), "end claude-1 background") {
		t.Fatal("the turn ended before the grace after the close")
	}
	r.clk.Advance(time.Millisecond)
	r.want(t, "start claude-1 background", "agent_message_chunk", "tool_call", "usage_update", "tool_call_update", "end claude-1 background")
}

// TestClaudeForeignToolUpdateInsideTheGrace: a tool_call_update inside the
// grace is applied and does not cancel the candidate.
func TestClaudeForeignToolUpdateInsideTheGrace(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.push(t, chunk("a"), toolCall("t1", "completed"), usageCost)
	r.clk.Advance(grace / 2)
	r.push(t, toolUpd("t1", "completed"))
	r.clk.Advance(grace / 2)
	r.want(t, "start claude-1 background", "agent_message_chunk", "tool_call", "usage_update", "tool_call_update", "end claude-1 background")
}

// TestClaudeForeignSilence: a wake that sends no usage ends by silence after
// the limit, and with a tool call open only after the longer one.
func TestClaudeForeignSilence(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.push(t, chunk("a"))
	r.clk.Advance(DefaultForeignTiming.Silence - time.Millisecond)
	r.push(t, chunk("b"))
	r.clk.Advance(DefaultForeignTiming.Silence - time.Millisecond)
	r.want(t, "start claude-1 background", "agent_message_chunk", "agent_message_chunk")
	r.clk.Advance(time.Millisecond)
	r.want(t, "start claude-1 background", "agent_message_chunk", "agent_message_chunk", "end claude-1 background silent")

	r.push(t, toolCall("t2", "in_progress"))
	r.clk.Advance(DefaultForeignTiming.Silence)
	if got := r.client.ForeignPhase(); got != "foreign" {
		t.Fatalf("a turn with a tool open ended at the short limit: %s", got)
	}
	r.clk.Advance(DefaultForeignTiming.SilenceToolOpen - DefaultForeignTiming.Silence)
	if got := r.log.get(); got[len(got)-1] != "end claude-2 background silent" {
		t.Fatalf("the long limit did not end it: %q", got)
	}
}

// TestClaudeForeignTrailingAndIdleTrafficOpenNothing: a trailing
// tool_call_update, a tool_call re-sent for a call already seen, idle notices,
// a plan, usage with cost, sub-agent traffic and kinds this build does not
// know open nothing; the session-level kinds neither.
func TestClaudeForeignTrailingAndIdleTrafficOpenNothing(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	req, done := r.prompt(t)
	r.push(t, toolCall("t1", "in_progress"))
	r.answerWith(t, req, `{"stopReason":"end_turn"}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	idle := []string{
		toolUpd("t1", "completed"),
		toolCall("t1", "completed"),
		usageCost,
		`{"sessionUpdate":"notice","notice":{"text":"x"}}`,
		`{"sessionUpdate":"compaction_summary_chunk","content":{"type":"text","text":"s"}}`,
		`{"sessionUpdate":"compaction_update","status":"completed"}`,
		`{"sessionUpdate":"plan","entries":[]}`,
		`{"sessionUpdate":"session_message","messageId":"m"}`,
		`{"sessionUpdate":"available_commands_update","availableCommands":[]}`,
		`{"sessionUpdate":"config_option_update","configOptions":[]}`,
		`{"sessionUpdate":"current_mode_update","currentModeId":"default"}`,
		`{"sessionUpdate":"session_info_update","title":"t"}`,
		`{"sessionUpdate":"something_new"}`,
	}
	r.push(t, idle...)
	for _, e := range r.log.get() {
		if strings.HasPrefix(e, "start") {
			t.Fatalf("idle traffic opened a foreign turn: %q", r.log.get())
		}
	}
	r.clk.Advance(DefaultForeignTiming.SilenceToolOpen)
	if got := r.client.ForeignPhase(); got != "idle" {
		t.Fatalf("phase %s", got)
	}
}

// TestClaudeForeignIdleCompaction: compaction_update in_progress while idle is
// a model call, and opens a compaction turn.
func TestClaudeForeignIdleCompaction(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.push(t, `{"sessionUpdate":"compaction_update","status":"in_progress"}`, usageCost)
	r.clk.Advance(grace)
	r.want(t, "start claude-1 compaction", "compaction_update", "usage_update", "end claude-1 compaction")
}

// TestClaudeForeignUserEchoOpens: a user_message_chunk outside a replay is a
// prompt the agent took that craze did not send: it opens.
func TestClaudeForeignUserEchoOpens(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.push(t, `{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"hi"}}`)
	r.want(t, "start claude-1 background", "user_message_chunk")
}

// TestClaudeForeignNothingBeforeSessionNewReturns: what the adapter sends
// before session/new's reply opens nothing, and is handed on in order; root
// content after the reply opens a turn.
func TestClaudeForeignNothingBeforeSessionNewReturns(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t, chunk("early"), toolCall("t0", "in_progress"))
	r.push(t, chunk("after"))
	r.want(t, "agent_message_chunk", "tool_call", "start claude-1 background", "agent_message_chunk")
}

// TestClaudeForeignNothingDuringAReplay: a load's replay opens nothing; the
// reply ends the replay on the read loop, so the very next update is judged by
// the rules.
func TestClaudeForeignNothingDuringAReplay(t *testing.T) {
	r := newClaudeRig(t)
	done := make(chan error, 1)
	go func() {
		_, err := r.client.LoadSession(context.Background(), rigRoot, t.TempDir(), SessionParams{})
		done <- err
	}()
	req := r.readWithin(t, 3*time.Second, "session/load")
	r.pushUpdate(t, rigRoot, `{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"old"}}`)
	r.pushUpdate(t, rigRoot, chunk("old reply"))
	r.answerWith(t, req, `{}`)
	r.pushUpdate(t, rigRoot, chunk("new"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.roundTrip(t)
	r.want(t, "user_message_chunk", "agent_message_chunk", "start claude-1 background", "agent_message_chunk")
}

// TestClaudeForeignPromptRefusedAndPromptedAbsorbs: a prompt cannot start
// while foreign; once the turn ends a prompt goes, and everything during it is
// its own; the reply is processed in order, so an update right behind it on
// the wire sees idle.
func TestClaudeForeignPromptRefusedAndPromptedAbsorbs(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.push(t, chunk("wake"))
	if _, err := r.client.Prompt(context.Background(), "x"); !errors.Is(err, ErrForeignTurn) {
		t.Fatalf("a prompt during a foreign turn = %v; want ErrForeignTurn", err)
	}
	if !r.client.ForeignTurnRunning() {
		t.Fatal("ForeignTurnRunning false during a claude foreign turn")
	}
	r.push(t, usageCost)
	r.clk.Advance(grace)
	req, done := r.prompt(t)
	r.pushUpdate(t, rigRoot, chunk("answer"))
	r.answerWith(t, req, `{"stopReason":"end_turn"}`)
	r.pushUpdate(t, rigRoot, chunk("wake again"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.roundTrip(t)
	r.want(t, "start claude-1 background", "agent_message_chunk", "usage_update", "end claude-1 background",
		"agent_message_chunk", "start claude-2 background", "agent_message_chunk")
}

// TestClaudeForeignCancel: a cancel during a foreign turn writes
// session/cancel and ends the turn cancelled after the grace; the cost usage
// the stopped cycle sends restarts it, and content no longer holds it.
func TestClaudeForeignCancel(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.push(t, chunk("wake"), toolCall("t1", "in_progress"))
	go func() { _ = r.client.Cancel(context.Background()) }()
	msg := r.readWithin(t, 3*time.Second, "session/cancel")
	if msg.Method != MethodSessionCancel || !strings.Contains(string(msg.Params), `"sessionId":"root"`) {
		t.Fatalf("the cancel wrote %s %s", msg.Method, msg.Params)
	}
	r.clk.Advance(grace / 2)
	r.push(t, chunk("trailing"), usageCost)
	r.clk.Advance(grace - time.Millisecond)
	if got := r.client.ForeignPhase(); got != "foreign" {
		t.Fatalf("the cost usage did not restart the grace: %s", got)
	}
	r.clk.Advance(time.Millisecond)
	r.want(t, "start claude-1 background", "agent_message_chunk", "tool_call", "agent_message_chunk", "usage_update", "end claude-1 background cancelled")
}

// TestClaudeForeignStartedNewTurn: a stray startedNewTurn reply to a steer is
// a foreign turn, opened on the read loop when the reply is read.
func TestClaudeForeignStartedNewTurn(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.client.mu.Lock()
	r.client.steering = true
	r.client.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- r.client.Interject(context.Background(), "do it", "craze-1") }()
	req := r.readWithin(t, 3*time.Second, "_session/steering")
	r.answerWith(t, req, `{"outcome":"startedNewTurn"}`)
	if err := <-done; !errors.Is(err, ErrSteerStartedTurn) {
		t.Fatalf("steer = %v", err)
	}
	// Foreign from the reply itself, before anything of the turn is read.
	r.want(t, "start claude-1 background")
	if _, err := r.client.Prompt(context.Background(), "x"); !errors.Is(err, ErrForeignTurn) {
		t.Fatalf("a prompt during the steered turn = %v; want ErrForeignTurn", err)
	}
	r.push(t, chunk("steered"))
	r.want(t, "start claude-1 background", "agent_message_chunk")
}

// TestClaudeForeignAnEndingAnnouncedBeforeTheNextStart parks a grace timer
// between its decision and its announcement, with the read loop holding the
// next turn's content at the dispatch lock: the ending is still announced
// before the start that content makes.
func TestClaudeForeignAnEndingAnnouncedBeforeTheNextStart(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.push(t, chunk("a"), usageCost)
	parked, release := make(chan struct{}), make(chan struct{})
	arrived := make(chan struct{}, 1)
	testForeignAnnounce = func(*ForeignTurn) { close(parked); <-release }
	testClaudeDispatch = func(method string) {
		if method == MethodSessionUpdate {
			select {
			case arrived <- struct{}{}:
			default:
			}
		}
	}
	t.Cleanup(func() { testForeignAnnounce, testClaudeDispatch = nil, nil })
	fired := make(chan struct{})
	go func() { r.clk.Advance(grace); close(fired) }()
	<-parked
	r.pushUpdate(t, rigRoot, chunk("next"))
	<-arrived
	if r.client.dispatchMu.TryLock() {
		t.Fatal("the parked timer does not hold the dispatch lock")
	}
	close(release)
	<-fired
	r.roundTrip(t)
	r.want(t, "start claude-1 background", "agent_message_chunk", "usage_update", "end claude-1 background",
		"start claude-2 background", "agent_message_chunk")
}

// TestClaudeForeignAStaleTimerDoesNothing: a grace callback that escaped its
// Stop — content cancelled the candidate after the callback was due — finds a
// newer generation and ends nothing.
func TestClaudeForeignAStaleTimerDoesNothing(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	r.push(t, chunk("a"), usageCost)
	stale := r.clk.lastFunc(grace)
	r.push(t, chunk("more"))
	stale()
	if got := r.client.ForeignPhase(); got != "foreign" {
		t.Fatalf("a stale grace callback ended the turn: %s", got)
	}
	r.want(t, "start claude-1 background", "agent_message_chunk", "usage_update", "agent_message_chunk")
}

// TestClaudeForeignFailedByTheSessionsStop: a turn still open when the client
// closes ends failed with what FailForeign recorded.
func TestClaudeForeignFailedByTheSessionsStop(t *testing.T) {
	r := newClaudeRig(t)
	r.ready(t)
	var got ForeignTurn
	r.client.SetForeignTurnHandler(func(f ForeignTurn) {
		if !f.Running {
			got = f
		}
	})
	r.push(t, chunk("a"))
	stop := errors.New("the login changed")
	r.client.FailForeign(stop)
	_ = r.client.Close()
	if !errors.Is(got.Err, stop) || got.ID != "claude-1" || got.Cancelled {
		t.Fatalf("the ending at close = %+v; want claude-1 failed with the stop", got)
	}
	if r.client.ForeignPhase() != "closed" {
		t.Fatal("the machine is not closed")
	}
}

// TestClaudeForeignWakeFrames replays the real wake capture (probe2's bgwait
// and bgwait-idle, redacted): the prompted turn absorbs its own frames, and
// each of the two wakes after it is one background turn that ends after the
// grace that follows its cost usage.
func TestClaudeForeignWakeFrames(t *testing.T) {
	r := newClaudeRig(t)
	r.root = "SESSION"
	r.ready(t)
	frames := fixtureFrames(t, "claude-wake")
	var req *Message
	var done chan error
	wakes := 0
	for _, f := range frames {
		switch {
		case f.Dir == "out":
			req, done = r.prompt(t)
		case f.Msg.Method == MethodSessionUpdate:
			if err := r.enc.WriteMessage(&f.Msg); err != nil {
				t.Fatal(err)
			}
			if f.Step == "bgwait-idle" && strings.Contains(string(f.Msg.Params), `"cost"`) {
				r.roundTrip(t)
				r.clk.Advance(grace)
				wakes++
			}
		case f.Msg.ID != nil:
			r.answerWith(t, req, string(f.Msg.Result))
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
	}
	var brackets []string
	for _, e := range r.log.get() {
		if strings.HasPrefix(e, "start") || strings.HasPrefix(e, "end") {
			brackets = append(brackets, e)
		}
	}
	want := []string{"start claude-1 background", "end claude-1 background", "start claude-2 background", "end claude-2 background"}
	if wakes != 2 || !slices.Equal(brackets, want) {
		t.Fatalf("wakes %d, brackets %q; want 2 and %q", wakes, brackets, want)
	}
}

func fixtureFrames(t *testing.T, dir string) []claudeFrame {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", dir, "frames.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []claudeFrame
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var f claudeFrame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}
