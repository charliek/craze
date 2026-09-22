package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// nativeScriptedModel is a fantasy.LanguageModel that answers each Stream
// call with the next queued step: the test seam plan 018 §3.8 built for
// exactly this (harness.Options.NewModel, through agent.NewNative's tweak),
// so both the "never persisted/indexed" test and the native-echo golden run a
// real native session without a network call.
type nativeScriptedModel struct {
	provider, wire string
	steps          [][]fantasy.StreamPart
	calls          int
	// onCall, when set, runs on every request before the step answering it.
	// It is what lets a scripted turn act on something only the request knows
	// — the plan file's absolute path, which a plan-mode reminder names and
	// nothing outside the harness can guess (nativePlanPathIn, plan 023 §3.2).
	onCall func(fantasy.Call)
}

func (m *nativeScriptedModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if m.onCall != nil {
		m.onCall(call)
	}
	if m.calls >= len(m.steps) {
		return nil, errors.New("nativeScriptedModel: no step queued")
	}
	step := m.steps[m.calls]
	m.calls++
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range step {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (m *nativeScriptedModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("nativeScriptedModel: Generate is not used")
}

func (m *nativeScriptedModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("nativeScriptedModel: GenerateObject is not used")
}

func (m *nativeScriptedModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("nativeScriptedModel: StreamObject is not used")
}

func (m *nativeScriptedModel) Provider() string { return m.provider }
func (m *nativeScriptedModel) Model() string    { return m.wire }

func nativeTextParts(chunks ...string) []fantasy.StreamPart {
	parts := []fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextStart, ID: "0"}}
	for _, c := range chunks {
		parts = append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: c})
	}
	return append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "0"})
}

func nativeThoughtParts(chunks ...string) []fantasy.StreamPart {
	parts := []fantasy.StreamPart{{Type: fantasy.StreamPartTypeReasoningStart, ID: "r"}}
	for _, c := range chunks {
		parts = append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: "r", Delta: c})
	}
	return append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: "r"})
}

func nativeFinishParts() []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop,
		Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}}}
}

// nativeOneModelTable is the smallest table Start can open: one provider, one
// model, no efforts.
func nativeOneModelTable() *modeltable.Table {
	return &modeltable.Table{
		DefaultModel: "test/echo",
		Providers: map[string]modeltable.Provider{
			"test": {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"NATIVE_TUI_TEST_KEY"}},
		},
		Models: map[string]modeltable.Model{
			"test/echo": {Provider: "test", WireModel: "wire-echo", Name: "Echo"},
		},
	}
}

// nativeSessionTweak is agent.NewNative's test seam: it points the harness at
// home directly, rather than paths.NativeDir() — the frame runner's
// isolateFrameHome swaps HOME during a run, so a golden or a test that wants
// its own fixed table must hand it in here instead of relying on whatever the
// isolated HOME holds (plan 018 §3.8, C9's note to C10). Getenv never reads
// the real environment (plan 018 §3.5).
func nativeSessionTweak(home string, table *modeltable.Table, model *nativeScriptedModel) func(*harness.Options) {
	return func(o *harness.Options) {
		o.Home = home
		o.Table = table
		o.Getenv = func(k string) string {
			if k == "NATIVE_TUI_TEST_KEY" {
				return "test-key"
			}
			return ""
		}
		o.NewModel = func(modeltable.Resolved) (fantasy.LanguageModel, error) { return model, nil }
		o.Now = func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }
	}
}

// drainSessionEvents feeds every event sess has already buffered into m, the
// way waitEvent/eventMsg would one at a time in a real program. A native
// turn's callbacks run synchronously inside the harness (plan 018 §3.7), so
// by the time runCmd(cmd) has returned, the whole turn is already sitting in
// the channel with nothing left to arrive.
func drainSessionEvents(t *testing.T, m *Model, sess agent.Session) {
	t.Helper()
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				return
			}
			tm, _ := m.Update(eventMsg{ev})
			*m = tm.(Model)
		default:
			return
		}
	}
}

// TestNativeSessionDoesNotPersistOrIndex drives a real native session — not
// tui.Stub — through startedMsg and one full turn, and holds plan 018 §3.4's
// two "never persisted, never indexed" rules together against production
// code neither TestStartedMsgDoesNotPersistAHiddenProvider (a planted stub)
// nor internal/agent's own tests (no TUI in them) can reach: a hidden
// provider's session must not write a row to the shared index (the
// fakeIndex recorder catches an Upsert), and must not replace the persisted
// default (config.toml's seeded "grok" must survive startedMsg's own
// SaveProvider).
func TestNativeSessionDoesNotPersistOrIndex(t *testing.T) {
	isolateSkillsHome(t)
	path := writeConfigFile(t, "provider = \"grok\"\n")

	table := nativeOneModelTable()
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.steps = [][]fantasy.StreamPart{append(nativeTextParts("hi there"), nativeFinishParts()...)}

	ws := t.TempDir()
	harnessHome := t.TempDir()
	// ContentHome, not HOME: from plan 022 C3 a native Start reads the user's
	// own Claude commands and skills, and a golden or a row count that
	// depended on the developer's ~/.claude would not be one.
	sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()}, nativeSessionTweak(harnessHome, table, model))
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	idx := &fakeIndex{}
	m := New(Config{
		Session:         sess,
		Theme:           "tokyo-night",
		Workspace:       ws,
		Yolo:            true,
		PersistProvider: true,
		ProviderLocked:  true,
		SessionIndex:    idx,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	if !m.started || m.snap.Provider.Name != "native" {
		t.Fatalf("started=%v provider=%q, want a started native session", m.started, m.snap.Provider.Name)
	}
	// startedMsg's own persist already had its chance by the time it returns.
	if idx.count() != 0 {
		t.Fatalf("startedMsg indexed a session with no prompt yet: %+v", idx.all())
	}
	if got := ConfigProvider(); got != "grok" {
		t.Fatalf("startedMsg replaced the persisted default with %q", got)
	}

	m.input.SetValue("hello")
	tm, cmd := m.Update(enter())
	m = tm.(Model)
	if m.status != statusWorking || cmd == nil {
		t.Fatal("Enter did not start a turn")
	}
	msg := runCmd(cmd)
	drainSessionEvents(t, &m, sess)
	tm, _ = m.Update(msg)
	m = tm.(Model)
	if m.status != statusIdle {
		t.Fatalf("status %s after the turn, want idle", m.status)
	}
	if !strings.Contains(plainView(m), "hi there") {
		t.Fatalf("the answer never rendered:\n%s", plainView(m))
	}

	if idx.count() != 0 {
		t.Fatalf("a completed turn on a hidden provider was indexed: %+v", idx.all())
	}
	if got := ConfigProvider(); got != "grok" {
		body, _ := os.ReadFile(path)
		t.Fatalf("a native turn replaced the persisted default: %q\n%s", got, body)
	}
}

// TestNativeModelSwitchGainsTheEffortOption is the live smoke's bug, through
// the TUI's own /model path on a real native session: started on a model with
// no efforts, `/model <a model with efforts>` must leave the snapshot — and
// so the status row — with that model's effort option, because a following
// `/model <id> <effort>` is split by SplitModelEffort against exactly that
// option. /model with no effort returns no message of its own; what brings
// the new options in is the session's bare EventMeta, as an ACP agent's
// config_option_update does.
func TestNativeModelSwitchGainsTheEffortOption(t *testing.T) {
	isolateSkillsHome(t)
	table := &modeltable.Table{
		DefaultModel: "test/plain",
		Providers: map[string]modeltable.Provider{
			"test": {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"NATIVE_TUI_TEST_KEY"}},
		},
		Models: map[string]modeltable.Model{
			"test/plain":   {Provider: "test", WireModel: "wire-plain", Name: "Plain"},
			"test/thinker": {Provider: "test", WireModel: "wire-thinker", Name: "Thinker", Efforts: []string{"low", "high"}, DefaultEffort: "high"},
			"test/sage":    {Provider: "test", WireModel: "wire-sage", Name: "Sage", Efforts: []string{"low", "medium"}, DefaultEffort: "medium"},
		},
	}
	model := &nativeScriptedModel{provider: "test", wire: "wire"}
	ws := t.TempDir()
	sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()}, nativeSessionTweak(t.TempDir(), table, model))
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	m := New(Config{Session: sess, Theme: "tokyo-night", Workspace: ws, Yolo: true, ProviderLocked: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	if agent.EffortOption(m.snap) != nil || !strings.Contains(plainView(m), "native │ Plain │") {
		t.Fatalf("a model with no efforts shows one:\n%s", plainView(m))
	}

	// slash runs one /model command the way the composer does, then its
	// command, then the events that change made — waiting for them rather than
	// draining whatever happens to be there.
	//
	// The wait is the point (plan 021 §2.8's C10 row). A settings change is a
	// StateDelta the session *enqueues* under its own lock and the log's drainer
	// publishes, so the command coming back is not the event arriving: an empty
	// channel says only that nothing has been published yet. Sync is the
	// barrier — everything enqueued before it is delivered when it returns — and
	// it runs on a helper goroutine while this one keeps reading, because with a
	// full primary the reader is what lets the drainer move (A19's shape).
	slash := func(line string) {
		t.Helper()
		m.input.SetValue(line)
		tm, cmd := m.Update(enter())
		m = tm.(Model)
		if msg := runCmd(cmd); msg != nil {
			tm, _ = m.Update(msg)
			m = tm.(Model)
		}
		synced := make(chan struct{})
		eng := m.eng
		go func() { defer close(synced); _ = eng.Sync(context.Background()) }()
		for done := false; !done; {
			select {
			case ev, ok := <-sess.Events():
				if !ok {
					done = true
					break
				}
				tm, _ := m.Update(eventMsg{ev})
				m = tm.(Model)
			case <-synced:
				done = true
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: the change's events never arrived", line)
			}
		}
		drainSessionEvents(t, &m, sess)
		if errs := texts(m, entryError); len(errs) > 0 {
			t.Fatalf("%s: %q", line, errs)
		}
	}

	slash("/model test/thinker")
	opt := agent.EffortOption(m.snap)
	if m.snap.CurrentModel != "test/thinker" || opt == nil || opt.Current != "high" {
		t.Fatalf("after /model test/thinker: model %q, effort option %+v; want test/thinker at high",
			m.snap.CurrentModel, opt)
	}
	if !strings.Contains(plainView(m), "native │ Thinker (high) │") {
		t.Fatalf("the status row does not show the effort:\n%s", plainView(m))
	}

	slash("/model test/sage low")
	opt = agent.EffortOption(m.snap)
	if m.snap.CurrentModel != "test/sage" || opt == nil || opt.Current != "low" {
		t.Fatalf("after /model test/sage low: model %q, effort option %+v; want test/sage at low",
			m.snap.CurrentModel, opt)
	}
	if !strings.Contains(plainView(m), "native │ Sage (low) │") {
		t.Fatalf("the status row does not show the new effort:\n%s", plainView(m))
	}
}
