package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
)

// The in-process backend (plan 027 §3.12) and the model's use of a Backend:
// what the model reads per command, what its reader makes of a stream that
// has ended, and how a test reaches the engine behind it.

// The compile-time proof, held by the tests as well as the file: the model's
// in-process backend is a backend.Backend.
var _ backend.Backend = (*engineBackend)(nil)

// engineOf is the engine behind a model's in-process backend: the one way a
// test reaches the engine for what the Backend does not carry (Session,
// NewClientID, Queue) and for every State read, so that C21's deletion of the
// Backend's transitional State touches no test. It fails the test when the
// model holds no backend, or one that is not the in-process engine.
func engineOf(t testing.TB, m Model) *engine.Engine {
	t.Helper()
	eng, ok := engineIn(m)
	if !ok {
		t.Fatalf("the model's backend is %T, want the in-process *engineBackend", m.eng)
	}
	return eng
}

// engineIn is engineOf for a helper with no test to fail: ok is false when the
// model holds no backend, or one that is not the in-process engine — nor a
// test's wrapper around it that names its engine (engineBehind) — nor a socket
// run's session, whose host engine the registry names (hostEngineOf, plan 027
// §3.16).
func engineIn(m Model) (*engine.Engine, bool) {
	eng := engineBehind(m.eng)
	if eng == nil {
		eng = hostEngineOf(m.eng)
	}
	return eng, eng != nil
}

// fakeBackend is a Backend whose every method panics except the ones a test
// gives it: the embedded interface is nil, so a call the test did not expect
// is a loud failure, never a silent zero.
type fakeBackend struct {
	backend.Backend
	clientID func() string
	read     func(ctx context.Context) (backend.Item, error)
	set      func(ctx context.Context, c engine.Command, s engine.Setting) (engine.SetResult, error)
	settings func(ctx context.Context) (backend.Settings, error)
}

func (f *fakeBackend) ClientID() string { return f.clientID() }

func (f *fakeBackend) Read(ctx context.Context) (backend.Item, error) { return f.read(ctx) }

func (f *fakeBackend) Set(ctx context.Context, c engine.Command, s engine.Setting) (engine.SetResult, error) {
	return f.set(ctx, c, s)
}

func (f *fakeBackend) Settings(ctx context.Context) (backend.Settings, error) { return f.settings(ctx) }

// TestNextCmdReadsTheClientPerCommand is §3.12's "ClientID() is read per
// command, never cached": a socket client that reconnects without resuming has
// a new id, and the next command must name it. The backend here answers a new
// id every time it is asked, so a model that kept the first one would send the
// second command as a client that no longer exists.
func TestNextCmdReadsTheClientPerCommand(t *testing.T) {
	var asked int
	m := Model{eng: &fakeBackend{clientID: func() string {
		asked++
		return fmt.Sprintf("client-%d", asked)
	}}}
	first := m.nextCmd()
	second := m.nextCmd()
	if first != (engine.Command{Client: "client-1", ID: "1"}) {
		t.Fatalf("the first command is %+v, want client-1's command 1", first)
	}
	if second != (engine.Command{Client: "client-2", ID: "2"}) {
		t.Fatalf("the second command is %+v, want client-2's command 2: the client was not read again", second)
	}
	ids := m.nextCmds(2)
	if ids[0].Client != "client-3" || ids[1].Client != "client-4" {
		t.Fatalf("nextCmds named %+v, want the client read for each", ids)
	}

	// A backend with no client, and no backend at all, is the zero command, as
	// it always was, and spends no number.
	none := Model{eng: &fakeBackend{clientID: func() string { return "" }}}
	if c := none.nextCmd(); !c.IsZero() || none.cmdSeq != 0 {
		t.Fatalf("a backend with no client made %+v (seq %d), want the zero command", c, none.cmdSeq)
	}
	var bare Model
	if c := bare.nextCmd(); !c.IsZero() || bare.cmdSeq != 0 {
		t.Fatalf("a model with no backend made %+v (seq %d), want the zero command", c, bare.cmdSeq)
	}
}

// TestWaitEventStopsWhenTheStreamEnds: the reader turns an event into an
// eventMsg, whose handler re-arms it, and an ended stream into nothing — a
// closed stream is not an event, and a zero event handed to applyEvent would be
// a message about nothing, followed by another read of a stream that has
// ended. The stream's own End is an endMsg (plan 027 PR 4, C27), after which
// the reader is not armed again; a Ready is a readyMsg, and the event behind it
// the next read's (PR 3's read-past rule was transitional, X33 4).
func TestWaitEventStopsWhenTheStreamEnds(t *testing.T) {
	stream := func(items ...any) *fakeBackend {
		return &fakeBackend{read: func(context.Context) (backend.Item, error) {
			if len(items) == 0 {
				t.Fatal("the reader read past the item that ended the stream")
			}
			it := items[0]
			items = items[1:]
			switch v := it.(type) {
			case error:
				return backend.Item{}, v
			case backend.Item:
				return v, nil
			}
			panic("stream: not an item or an error")
		}}
	}
	ev := agent.Event{Type: agent.EventText, Text: "hello"}
	stopped := errors.New("session stopped")
	for _, tc := range []struct {
		name string
		b    *fakeBackend
		// want is what each read answers, in order: nil for nothing.
		want []tea.Msg
	}{
		{"an event", stream(backend.Item{Kind: backend.ItemEvent, Event: ev}), []tea.Msg{eventMsg{ev: ev}}},
		{"a closed stream", stream(backend.ErrClosed), []tea.Msg{nil}},
		{"a failed read", stream(errors.New("the socket went away")), []tea.Msg{nil}},
		{"an end item", stream(backend.Item{Kind: backend.ItemEnd, Err: stopped}), []tea.Msg{endMsg{err: stopped}}},
		{"a ready item, then an event", stream(backend.Item{Kind: backend.ItemReady}, backend.Item{Kind: backend.ItemEvent, Event: ev}),
			[]tea.Msg{readyMsg{}, eventMsg{ev: ev}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, want := range tc.want {
				cmd := waitEvent(tc.b, 0)
				if cmd == nil {
					t.Fatal("waitEvent made no reader for a backend")
				}
				if got := cmd(); !reflect.DeepEqual(got, want) {
					t.Fatalf("read %d answered %#v, want %#v", i+1, got, want)
				}
			}
			end, ok := tc.want[len(tc.want)-1].(endMsg)
			if !ok {
				return
			}
			// The End applied, as the read that delivered it: the program
			// quits, and the reader is not armed again.
			m := sized(t)
			m.reading = true
			tm, cmd := m.Update(end)
			if m = tm.(Model); !m.ended || m.reading || len(namedCmds(cmd, "waitEvent")) != 0 {
				t.Fatalf("after the End: ended %v, reading %v, reads armed %d — want ended and no read",
					m.ended, m.reading, len(namedCmds(cmd, "waitEvent")))
			}
		})
	}
	if waitEvent(nil, 0) != nil {
		t.Fatal("waitEvent made a reader for no backend")
	}
}

// TestTheChainsJudgeSettingsAsTheEnginesSnapshot: the snapshot a model-change
// chain judges its next step on — the backend's Settings with the provider the
// Update captured from the mirror (settingsSnapshot) — answers every read the
// chains make exactly as the engine's own State().Snapshot did before the
// Backend: the current model and mode, the config, the effort and fast options
// (agent.EffortOption, agent.FastOption) and the dialog's tabs (catalogTabs),
// whatever the provider.
func TestTheChainsJudgeSettingsAsTheEnginesSnapshot(t *testing.T) {
	for _, prov := range []agent.Provider{agent.CursorProvider(), agent.GrokProvider(), agent.NativeProvider()} {
		t.Run(prov.Name(), func(t *testing.T) {
			m, stub := cursorStub(t, "claude-opus-5")
			stub.SetProvider(prov)
			stub.mu.Lock()
			stub.snap.CurrentMode = "plan"
			stub.mu.Unlock()
			m = republish(t, m)

			set, err := m.eng.Settings(context.Background())
			if err != nil {
				t.Fatalf("an in-process settings read failed: %v", err)
			}
			want := engineOf(t, m).State().Snapshot
			got := settingsSnapshot(set, m.snap.Provider)
			if got.CurrentModel != want.CurrentModel || got.CurrentMode != want.CurrentMode || !reflect.DeepEqual(got.Provider, want.Provider) {
				t.Fatalf("settings name model %q mode %q provider %+v, the engine %q %q %+v",
					got.CurrentModel, got.CurrentMode, got.Provider, want.CurrentModel, want.CurrentMode, want.Provider)
			}
			if !reflect.DeepEqual(got.Config, want.Config) {
				t.Fatalf("settings carry config %+v, the engine %+v", got.Config, want.Config)
			}
			if g, w := agent.EffortOption(got), agent.EffortOption(want); !reflect.DeepEqual(g, w) || g == nil {
				t.Fatalf("the effort option is %+v from settings, %+v from the engine", g, w)
			}
			if g, w := agent.FastOption(got), agent.FastOption(want); !reflect.DeepEqual(g, w) || g == nil {
				t.Fatalf("the fast option is %+v from settings, %+v from the engine", g, w)
			}
			if g, w := catalogTabs(got), catalogTabs(want); !reflect.DeepEqual(g, w) || len(g) == 0 {
				t.Fatalf("the tabs are %q from settings, %q from the engine", tabLabels(g), tabLabels(w))
			}
		})
	}
}

// TestAChainWhoseSettingsReadFailsEndsAtThatStep: over the socket the read a
// model-change chain judges its next step on is a round trip, and one that
// fails says nothing about the session. The dialog's chain ends there as an
// error row naming the step it was about to judge, with nothing more sent, as
// any other failed step ends it; `/model`'s effort step is the error row its
// other failures are. In process the read cannot fail, so only a backend that
// is not the engine reaches this.
func TestAChainWhoseSettingsReadFailsEndsAtThatStep(t *testing.T) {
	lost := errors.New("the socket went away")
	var sent []engine.Setting
	b := &fakeBackend{
		set: func(_ context.Context, _ engine.Command, s engine.Setting) (engine.SetResult, error) {
			sent = append(sent, s)
			return engine.SetResult{Value: s.Value, Rev: 1}, nil
		},
		settings: func(context.Context) (backend.Settings, error) { return backend.Settings{}, lost },
	}
	steps := []applyStep{
		{value: "grok-4.6", label: "model"},
		{cfgID: "effort", value: "high", label: "effort", role: roleEffort},
	}
	out := runModelApply(context.Background(), issued{}, b, agent.ProviderInfo{}, make([]engine.Command, len(steps)), steps, "grok-4.6", 7)
	if out.step != "effort" || !errors.Is(out.err, lost) || out.unread {
		t.Fatalf("the chain ended at %q with %v (unread %v), want the effort step's failed read", out.step, out.err, out.unread)
	}
	if len(out.done) != 1 || out.done[0].cfgID != "" || len(sent) != 1 || sent[0].Kind != engine.SettingModel {
		t.Fatalf("the chain did %+v and sent %+v, want the model step alone", out.done, sent)
	}

	msg := runModelEffort(context.Background(), issued{}, b, agent.ProviderInfo{}, engine.Command{}, "grok-4.6", "high", modelLanded{}, 0)
	if am, ok := msg.(actionErrMsg); !ok || !errors.Is(am.err, lost) {
		t.Fatalf("/model's effort step answered %#v, want the failed read's error row", msg)
	}
	if len(sent) != 1 {
		t.Fatalf("/model's effort step sent %+v after a failed read", sent[1:])
	}
}

// TestEngineBackendReadHonoursItsContext: a Read with a context already done
// answers the context's error and takes nothing off the primary — the event is
// still there for the next Read — and a live one reads the engine's primary.
func TestEngineBackendReadHonoursItsContext(t *testing.T) {
	stub := NewStub()
	eng, err := engine.New(stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	b := newEngineBackend(eng, "")
	// The in-process client is minted once, when the engine is wrapped, and
	// is the same on every read: only a socket client's can change.
	first, again := b.ClientID(), b.ClientID()
	if first == "" || again != first {
		t.Fatalf("the backend's client read %q then %q, want one minted id", first, again)
	}
	stub.Emit(agent.Event{Type: agent.EventText, Text: "kept"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Read(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled Read answered %v, want context.Canceled", err)
	}
	it, err := b.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if it.Kind != backend.ItemEvent || it.Event.Type != agent.EventText || it.Event.Text != "kept" || it.Gen != 0 {
		t.Fatalf("the next Read took %+v, want the event the cancelled one left", it)
	}
}

// TestInfoReflectsTheEnginesStaticFacts (§3.13, X33 4): Info() maps every
// field from the engine's own State — the snapshot's static ones
// (Provider.Name/Label/Capabilities, SessionID, Models, Modes) and State's own
// (Incarnation, CrazeSessionID, RetryHorizon) — plus the workspace the backend
// was built with, which is the caller's own resolved cwd and never a read of
// the engine (engineBackend.workspace's doc: the engine holds no workspace of
// its own).
func TestInfoReflectsTheEnginesStaticFacts(t *testing.T) {
	stub := NewStub()
	stub.SetProvider(agent.GrokProvider())
	eng, err := engine.New(stub, engine.Options{CrazeSessionID: "cz-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	b := newEngineBackend(eng, "/work/dir")

	info := b.Info()
	st := eng.State()
	if info.CrazeSessionID != "cz-1" || info.CrazeSessionID != st.CrazeSessionID {
		t.Fatalf("CrazeSessionID is %q, want %q", info.CrazeSessionID, st.CrazeSessionID)
	}
	if info.ProviderSessionID == "" || info.ProviderSessionID != st.SessionID {
		t.Fatalf("ProviderSessionID is %q, want the snapshot's %q", info.ProviderSessionID, st.SessionID)
	}
	if info.Incarnation == "" || info.Incarnation != st.Incarnation {
		t.Fatalf("Incarnation is %q, want %q", info.Incarnation, st.Incarnation)
	}
	if info.Workspace != "/work/dir" {
		t.Fatalf("Workspace is %q, want the backend's own construction value, not a read of the engine", info.Workspace)
	}
	if info.Provider != "grok" || info.Label != agent.GrokProvider().Info().Label() {
		t.Fatalf("Provider/Label are %q/%q, want grok's", info.Provider, info.Label)
	}
	if info.Capabilities != agent.GrokProvider().Capabilities() {
		t.Fatalf("Capabilities are %+v, want grok's own table (Snapshot.Provider.Capabilities())", info.Capabilities)
	}
	if !reflect.DeepEqual(info.Models, st.Models) || !reflect.DeepEqual(info.Modes, st.Modes) {
		t.Fatalf("Models/Modes are %+v/%+v, want the snapshot's %+v/%+v", info.Models, info.Modes, st.Models, st.Modes)
	}
	if info.RetryHorizon != st.RetryHorizon {
		t.Fatalf("RetryHorizon is %+v, want %+v", info.RetryHorizon, st.RetryHorizon)
	}
}

// TestInfoBeforeStartReflectsTheConfiguredProvider (§3.13, GLM 11): Info
// answers before Start is ever called, from the session's own initial
// snapshot — the configured provider — exactly as the TUI reads it today.
func TestInfoBeforeStartReflectsTheConfiguredProvider(t *testing.T) {
	stub := NewStub()
	stub.SetProvider(agent.NativeProvider())
	eng, err := engine.New(stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	b := newEngineBackend(eng, "")

	info := b.Info()
	if info.Provider != "native" || info.Capabilities != agent.NativeProvider().Capabilities() {
		t.Fatalf("Info before Start is %+v, want native's configured facts", info)
	}
}

// TestInfoWaitsOnNothing (§3.12's interface paragraph, "Info() ... waits on
// nothing"): Info reads State().Snapshot's static fields exactly as Settings
// already does on every model-change chain step (settings_test.go,
// TestTheChainsJudgeSettingsAsTheEnginesSnapshot above) — State reads the
// snapshot outside the engine's own mutex and takes it only briefly to merge
// in its own fields (engine/state.go's State doc) — so Info is no new way to
// block. This races Info against the engine's snapshot changing underneath it
// (caught by -race) and bounds every answer well past what an in-memory read
// needs, so a real deadlock — Info waiting on something the writer holds —
// fails loudly rather than hanging the suite.
func TestInfoWaitsOnNothing(t *testing.T) {
	stub := NewStub()
	eng, err := engine.New(stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	b := newEngineBackend(eng, "")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			stub.SetProvider(agent.GrokProvider())
			_ = eng.State()
			stub.SetProvider(agent.CursorProvider())
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()

	for i := 0; i < 200; i++ {
		done := make(chan backend.SessionInfo, 1)
		go func() { done <- b.Info() }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Info blocked for a second while the engine's snapshot was busy — it must wait on nothing")
		}
	}
}
