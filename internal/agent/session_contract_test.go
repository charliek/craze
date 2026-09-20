package agent_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/tui"
)

// The session contract: the behaviour tui.Stub and the native adapter share,
// run against both, so the stub the TUI's chrome tests and goldens stand on
// cannot drift from the in-process session that ships (plan 018 §3.8, §9
// "adapter fidelity"). It is the first external test package in
// internal/agent because internal/tui imports agent, so a test in package
// agent could not import the stub.
//
// It deliberately leaves out the two places the stub is not production:
// Cancel waiting for the turn (the stub's returns at once) and the error path
// (the stub has none). The native adapter's own tests hold those, against the
// live session's behaviour. So does the claimed-slot queue guard, which the
// live session and the adapter have and the stub does not.

// contractWait bounds every wait on another goroutine; a correct run never
// comes near it.
const contractWait = 10 * time.Second

// contractSession is one implementation under test, started.
type contractSession struct {
	agent.Session
	// hold makes the next prompt stay open until it is cancelled. It returns
	// the context to run that prompt on and a channel closed once its turn
	// is open — in flight, not merely claimed.
	hold func(ctx context.Context) (context.Context, <-chan struct{})
}

var contractImpls = []struct {
	name string
	open func(t *testing.T) contractSession
}{
	{"stub", openStub},
	{"native", openNative},
}

// openStub is tui.Stub, whose echo reply is "echo: <text>". Its hung turn
// has no hook that says it has opened; the first thing it does once open is
// wait on its context, so a context that reports its first Done call is that
// signal.
func openStub(t *testing.T) contractSession {
	s := tui.NewStub()
	// The stub carries no provider of its own, so it advertises no
	// capability; grok's is the ACP provider that can interject, which is the
	// contract the native adapter now shares (plan 019 §3.10).
	s.SetProvider(agent.GrokProvider())
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return contractSession{Session: s, hold: func(ctx context.Context) (context.Context, <-chan struct{}) {
		s.HangNext()
		p := &openedProbe{Context: ctx, opened: make(chan struct{})}
		return p, p.opened
	}}
}

// openedProbe is a context that closes opened the first time anything asks
// for its Done channel.
type openedProbe struct {
	context.Context
	once   sync.Once
	opened chan struct{}
}

func (p *openedProbe) Done() <-chan struct{} {
	p.once.Do(func() { close(p.opened) })
	return p.Context.Done()
}

// openNative is the native adapter, through the same NewNative seam package
// tui's golden uses, on a one-model table under a temporary CRAZE_HOME, with a
// scripted model that echoes the prompt the way the stub does.
func openNative(t *testing.T) contractSession {
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	table := &modeltable.Table{
		DefaultModel: "echo",
		Providers: map[string]modeltable.Provider{
			"echo": {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1",
				APIKey: modeltable.Secret("sk-canary-not-a-secret")},
		},
		Models: map[string]modeltable.Model{"echo": {Provider: "echo", WireModel: "echo-1"}},
	}
	if err := modeltable.Save(filepath.Join(home, "native"), table); err != nil {
		t.Fatal(err)
	}
	m := &echoModel{}
	// ContentHome is an empty directory for the reason nativeFixture.session
	// fills it: left unset a native Start reads the developer's own ~/.claude,
	// and this suite would then answer differently on two machines.
	s := agent.NewNative(agent.Options{Workspace: t.TempDir(), ContentHome: t.TempDir()}, func(o *harness.Options) {
		o.Getenv = func(string) string { return "" }
		o.NewModel = func(modeltable.Resolved) (fantasy.LanguageModel, error) { return m, nil }
	})
	t.Cleanup(func() {
		// Bounded, so a Close that deadlocks fails the test instead of
		// hanging the binary until go test's own timeout.
		closed := make(chan struct{})
		go func() {
			_ = s.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(contractWait):
			t.Errorf("Close at cleanup did not return within %v", contractWait)
		}
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return contractSession{Session: s, hold: func(ctx context.Context) (context.Context, <-chan struct{}) {
		opened := make(chan struct{})
		m.holdNext(opened)
		return ctx, opened
	}}
}

// echoModel answers "echo: <the prompt>" in one text delta, or, once
// holdNext has armed it, holds the next request open until its context ends.
type echoModel struct {
	mu   sync.Mutex
	held chan struct{}
}

func (m *echoModel) holdNext(opened chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.held = opened
}

func (m *echoModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	held := m.held
	m.held = nil
	m.mu.Unlock()
	if held != nil {
		return func(yield func(fantasy.StreamPart) bool) {
			close(held)
			<-ctx.Done()
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
		}, nil
	}
	reply := "echo: " + lastUserText(call.Prompt)
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range []fantasy.StreamPart{
			{Type: fantasy.StreamPartTypeTextStart, ID: "0"},
			{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: reply},
			{Type: fantasy.StreamPartTypeTextEnd, ID: "0"},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop,
				Usage: fantasy.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}},
		} {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func lastUserText(msgs []fantasy.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != fantasy.MessageRoleUser {
			continue
		}
		for _, p := range msgs[i].Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
				return tp.Text
			}
		}
	}
	return ""
}

func (m *echoModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("echo: Generate is not used")
}

func (m *echoModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("echo: GenerateObject is not used")
}

func (m *echoModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("echo: StreamObject is not used")
}

func (m *echoModel) Provider() string { return "echo" }
func (m *echoModel) Model() string    { return "echo-1" }

// eachImpl runs body once per implementation, as a subtest.
func eachImpl(t *testing.T, body func(t *testing.T, s contractSession)) {
	for _, impl := range contractImpls {
		t.Run(impl.name, func(t *testing.T) { body(t, impl.open(t)) })
	}
}

// buffered is every event already emitted. Both implementations emit a
// turn's events before its prompt returns, so after a prompt has returned
// this is all of them.
func buffered(s agent.Session) []agent.Event {
	var out []agent.Event
	for {
		select {
		case ev := <-s.Events():
			out = append(out, ev)
		default:
			return out
		}
	}
}

func count(evs []agent.Event, typ agent.EventType) int {
	n := 0
	for _, ev := range evs {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

type promptOutcome struct {
	res agent.Result
	err error
}

// runHeld begins text on a held turn and returns once that turn is open,
// with the channel its outcome arrives on.
func runHeld(t *testing.T, s contractSession, text string) <-chan promptOutcome {
	t.Helper()
	ctx, opened := s.hold(context.Background())
	run := s.Begin(text)
	out := make(chan promptOutcome, 1)
	go func() {
		res, err := run(ctx)
		out <- promptOutcome{res, err}
	}()
	select {
	case <-opened:
	case <-time.After(contractWait):
		t.Fatalf("timed out after %v waiting for the held turn to open", contractWait)
	}
	return out
}

// cancelHeld cancels the held turn and waits for its prompt to return; it
// does not rely on Cancel waiting, which the stub's does not.
func cancelHeld(t *testing.T, s contractSession, out <-chan promptOutcome) promptOutcome {
	t.Helper()
	if _, err := s.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case o := <-out:
		return o
	case <-time.After(contractWait):
		t.Fatalf("timed out after %v waiting for the cancelled prompt to return", contractWait)
		panic("unreachable")
	}
}

// TestSessionContractPromptStreamsTextThenDone: a prompt's reply arrives as
// EventText and the turn ends in exactly one EventDone{end_turn}, last, with
// no EventError, and Prompt returns the same stop reason.
func TestSessionContractPromptStreamsTextThenDone(t *testing.T) {
	eachImpl(t, func(t *testing.T, s contractSession) {
		res, err := s.Prompt(context.Background(), "hello there")
		if err != nil || res.StopReason != "end_turn" {
			t.Fatalf("Prompt = %+v, %v; want end_turn", res, err)
		}
		evs := buffered(s)
		var text strings.Builder
		for _, ev := range evs {
			if ev.Type == agent.EventText {
				text.WriteString(ev.Text)
			}
		}
		if !strings.Contains(text.String(), "hello there") {
			t.Fatalf("text %q does not answer the prompt", text.String())
		}
		if count(evs, agent.EventDone) != 1 || count(evs, agent.EventError) != 0 {
			t.Fatalf("events %+v: want exactly one EventDone and no EventError", evs)
		}
		if last := evs[len(evs)-1]; last.Type != agent.EventDone || last.StopReason != "end_turn" {
			t.Fatalf("the last event is %+v, want EventDone{end_turn}", last)
		}
	})
}

// TestSessionContractRefusesASecondPrompt: while a prompt is claimed, or its
// turn is in flight, another Begin claims nothing and its continuation — like
// Prompt — returns ErrPromptInFlight with no event; the running prompt is
// unaffected, and once it has returned the slot is free again.
func TestSessionContractRefusesASecondPrompt(t *testing.T) {
	eachImpl(t, func(t *testing.T, s contractSession) {
		first := s.Begin("claimed")
		if _, err := s.Begin("over a claim")(context.Background()); !errors.Is(err, agent.ErrPromptInFlight) {
			t.Fatalf("Begin over a claim = %v, want ErrPromptInFlight", err)
		}
		if res, err := first(context.Background()); err != nil || res.StopReason != "end_turn" {
			t.Fatalf("the claimed prompt = %+v, %v", res, err)
		}
		buffered(s)

		out := runHeld(t, s, "in flight")
		if _, err := s.Prompt(context.Background(), "over a turn"); !errors.Is(err, agent.ErrPromptInFlight) {
			t.Fatalf("Prompt over a turn = %v, want ErrPromptInFlight", err)
		}
		if _, err := s.Begin("over a turn")(context.Background()); !errors.Is(err, agent.ErrPromptInFlight) {
			t.Fatalf("Begin over a turn = %v, want ErrPromptInFlight", err)
		}
		if evs := buffered(s); len(evs) != 0 {
			t.Fatalf("refused prompts emitted %+v", evs)
		}
		o := cancelHeld(t, s, out)
		if o.err != nil || o.res.StopReason != "cancelled" {
			t.Fatalf("the held prompt = %+v, %v; want cancelled", o.res, o.err)
		}
		evs := buffered(s)
		if count(evs, agent.EventDone) != 1 || count(evs, agent.EventError) != 0 || count(evs, agent.EventText) != 0 {
			t.Fatalf("the cancelled turn emitted %+v, want one EventDone and nothing else", evs)
		}
		if res, err := s.Prompt(context.Background(), "after"); err != nil || res.StopReason != "end_turn" {
			t.Fatalf("a prompt after the turn = %+v, %v", res, err)
		}
	})
}

// TestSessionContractQueue: every queue mutation emits its EventQueue with
// the row, what happened to it and the position it held; a take is refused
// while a turn is in flight, with no event and the row kept; and once the
// turn has returned the drain takes it.
func TestSessionContractQueue(t *testing.T) {
	eachImpl(t, func(t *testing.T, s contractSession) {
		one, err := s.Queue("one")
		if err != nil {
			t.Fatal(err)
		}
		two, err := s.Queue("two")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.EditQueued(one.ID, "one, edited"); err != nil {
			t.Fatal(err)
		}
		if _, ok := s.Unqueue(two.ID); !ok {
			t.Fatal("Unqueue lost the row")
		}
		if _, err := s.Queue(strings.Repeat("x", 64<<10)); !errors.Is(err, agent.ErrQueueTextTooLong) {
			t.Fatalf("an oversized row = %v, want ErrQueueTextTooLong", err)
		}

		out := runHeld(t, s, "in flight")
		if _, ok := s.TakeQueued(one.ID); ok {
			t.Fatal("TakeQueued took a row while a turn was in flight")
		}
		if _, ok := s.PopQueue(); ok {
			t.Fatal("PopQueue took a row while a turn was in flight")
		}
		cancelHeld(t, s, out)

		got, ok := s.PopQueue()
		if !ok || got.ID != one.ID || got.Text != "one, edited" {
			t.Fatalf("PopQueue after the turn = %+v, %v", got, ok)
		}
		if _, err := s.Queue("three"); err != nil {
			t.Fatal(err)
		}
		if n := s.ClearQueue(); n != 1 {
			t.Fatalf("ClearQueue removed %d rows, want 1", n)
		}

		var changes []string
		for _, ev := range buffered(s) {
			if ev.Type == agent.EventQueue {
				changes = append(changes, fmt.Sprintf("%s %q @%d", ev.QueueChange, ev.Queue.Text, ev.QueuePos))
			}
		}
		want := []string{
			`queued "one" @0`,
			`queued "two" @1`,
			`edited "one, edited" @0`,
			`removed "two" @1`,
			`sent "one, edited" @0`,
			`queued "three" @0`,
			`removed "three" @0`,
		}
		if !reflect.DeepEqual(changes, want) {
			t.Fatalf("queue events:\n got %q\nwant %q", changes, want)
		}
		if q := s.Snapshot().Queue; len(q) != 0 {
			t.Fatalf("the queue is not empty: %+v", q)
		}
	})
}

// TestSessionContractInterject (plan 019 §3.10, §7.12): both sessions
// advertise interject, both refuse it with ErrNotInTurn — emitting nothing —
// when there is no running turn to merge into, and both take one while a turn
// is in flight and show it exactly once as EventUser{Interjection}, before the
// turn's ending event and never after it.
//
// What the two do with text the turn could not answer is not shared: only the
// native adapter has a turn that can fail to take it up, and its own tests
// hold that (the head of the queue).
func TestSessionContractInterject(t *testing.T) {
	eachImpl(t, func(t *testing.T, s contractSession) {
		if !s.Snapshot().Provider.Capabilities().Interject {
			t.Fatal("the session does not advertise Interject, so the cases below say nothing")
		}
		if err := s.Interject(context.Background(), "while idle"); !errors.Is(err, agent.ErrNotInTurn) {
			t.Fatalf("Interject while idle = %v, want ErrNotInTurn", err)
		}
		if evs := buffered(s); len(evs) != 0 {
			t.Fatalf("a refused interjection emitted %+v", evs)
		}

		out := runHeld(t, s, "in flight")
		if err := s.Interject(context.Background(), "mid-turn"); err != nil {
			t.Fatalf("Interject during a turn = %v, want it accepted", err)
		}
		o := cancelHeld(t, s, out)
		if o.err != nil || o.res.StopReason != "cancelled" {
			t.Fatalf("the held prompt = %+v, %v; want cancelled", o.res, o.err)
		}
		if err := s.Interject(context.Background(), "after the turn"); !errors.Is(err, agent.ErrNotInTurn) {
			t.Fatalf("Interject after the turn = %v, want ErrNotInTurn", err)
		}

		evs := buffered(s)
		shown, last, end := -1, "", -1
		for i, ev := range evs {
			switch {
			case ev.Type == agent.EventUser && ev.Interjection:
				if shown >= 0 {
					t.Fatalf("the interjection was shown twice: %+v", evs)
				}
				shown, last = i, ev.Text
			case ev.Type == agent.EventDone, ev.Type == agent.EventError:
				end = i
			}
		}
		if shown < 0 || last != "mid-turn" {
			t.Fatalf("the accepted interjection was shown as %q at %d: %+v", last, shown, evs)
		}
		if end < 0 || shown > end {
			t.Fatalf("the interjection at %d followed the turn's ending at %d: %+v", shown, end, evs)
		}
	})
}
