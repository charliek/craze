package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// The native adapter's tests. The model is scripted (harness.Options.NewModel
// through NewNative's tweak) except in TestNativeWireErrorsArePhrasedWithoutTheKey,
// which runs the real llm factory against a loopback server so the key
// scrubbing it relies on is the production one.

// nativeCanary is the only key these tests hold: obviously not a secret, and
// every test that could leak one looks for it.
const (
	nativeCanary      = "sk-canary-not-a-secret"
	nativeCanaryOther = "sk-canary-not-a-secret-other"
)

// zeroWidthSpace is U+200B, spelled as a rune so no editor can turn the
// escape into the invisible character itself.
var zeroWidthSpace = string(rune(0x200b))

// nativeWait bounds every wait on another goroutine. It is a bound, not a
// synchronisation: a correct run never comes near it.
const nativeWait = 10 * time.Second

// nativeTestTable is the model table the scripted tests run on: two models on
// one provider with different effort lists (and a name that needs
// sanitizing), a model with no effort control on a second provider, and a
// model whose provider has no key. baseURL is only validated; scripted models
// never dial it.
func nativeTestTable(baseURL string) *modeltable.Table {
	return &modeltable.Table{
		DefaultModel: "test/a",
		Providers: map[string]modeltable.Provider{
			"test":  {Driver: modeltable.DriverOpenAICompat, BaseURL: baseURL, EnvKeys: []string{"NATIVE_TEST_KEY"}},
			"other": {Driver: modeltable.DriverOpenAICompat, BaseURL: baseURL, EnvKeys: []string{"NATIVE_OTHER_KEY"}},
			"nokey": {Driver: modeltable.DriverOpenAICompat, BaseURL: baseURL, EnvKeys: []string{"NATIVE_NOKEY_KEY"}},
		},
		Models: map[string]modeltable.Model{
			"test/a":  {Provider: "test", WireModel: "wire-a", Name: "Model A", Efforts: []string{"low", "high"}, DefaultEffort: "high"},
			"test/b":  {Provider: "test", WireModel: "wire-b", Name: "Model \x1b[31mB\x1b[0m", Efforts: []string{"medium", "high"}, DefaultEffort: "medium"},
			"other/c": {Provider: "other", WireModel: "wire-c"},
			"nokey/d": {Provider: "nokey", WireModel: "wire-d"},
		},
	}
}

// nativeFixture is a craze directory (CRAZE_HOME, for the test) holding the
// test table, the environment the harness's Getenv sees, and one scripted
// model per alias.
type nativeFixture struct {
	t      *testing.T
	dir    string // $CRAZE_HOME/native
	env    map[string]string
	models map[string]*scriptedModel
	// getenv replaces the reading of env, for the one case that needs an
	// environment which answers differently the second time it is asked.
	getenv func(string) string
	// edit is the case's own last word on the harness's options, applied
	// after the fixture's: NewNative's seam is what a test edits the options
	// through, and a case that wants to set one the fixture also sets — or
	// one the adapter fills in, such as Prompt — has to run after it.
	edit func(*harness.Options)
}

func newNativeFixture(t *testing.T) *nativeFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	dir := filepath.Join(home, "native")
	table := nativeTestTable("http://127.0.0.1:9/v1")
	if err := modeltable.Save(dir, table); err != nil {
		t.Fatalf("saving the test model table: %v", err)
	}
	f := &nativeFixture{
		t:      t,
		dir:    dir,
		env:    map[string]string{"NATIVE_TEST_KEY": nativeCanary, "NATIVE_OTHER_KEY": nativeCanaryOther},
		models: map[string]*scriptedModel{},
	}
	for alias, m := range table.Models {
		f.models[alias] = &scriptedModel{provider: m.Provider, wire: m.WireModel}
	}
	return f
}

// tweak is NewNative's seam: scripted models, the fixture's environment, a
// fixed clock. Home and Table stay the adapter's own (paths.NativeDir and a
// Load from it), so Start's real path is what runs.
func (f *nativeFixture) tweak(o *harness.Options) {
	o.Getenv = func(k string) string {
		if f.getenv != nil {
			return f.getenv(k)
		}
		return f.env[k]
	}
	o.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) { return f.models[r.Alias], nil }
	o.Now = func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }
	if f.edit != nil {
		f.edit(o)
	}
}

// session builds an unstarted adapter, closed when the test ends.
//
// ContentHome is filled here and not at each call site because from plan 022
// C3 a native Start reads the user's own Claude content under it: left empty
// it is HomeDir(), so every case in this package would discover whatever the
// developer running the tests happens to have installed, and the same suite
// would give different answers on two machines. A case that wants content
// gives itself a home with something in it; every other case gets an empty
// one, which is the same answer everywhere.
func (f *nativeFixture) session(opts Options) *nativeSession {
	f.t.Helper()
	if opts.Workspace == "" {
		opts.Workspace = f.t.TempDir()
	}
	if opts.ContentHome == "" {
		opts.ContentHome = f.t.TempDir()
	}
	s := newNative(opts, f.tweak)
	closeAtCleanup(f.t, s)
	return s
}

// closeAtCleanup closes s when the test ends, bounded: a Close that deadlocks
// is reported as a failure rather than hanging the test binary until go
// test's own timeout.
func closeAtCleanup(t *testing.T, s Session) {
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() {
			_ = s.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(nativeWait):
			t.Errorf("Close at cleanup did not return within %v", nativeWait)
		}
	})
}

// started is session plus a Start that must succeed.
func (f *nativeFixture) started(opts Options) *nativeSession {
	f.t.Helper()
	s := f.session(opts)
	if err := s.Start(context.Background()); err != nil {
		f.t.Fatalf("Start: %v", err)
	}
	return s
}

// transcripts is every transcript file the harness has written.
func (f *nativeFixture) transcripts() []string {
	f.t.Helper()
	paths, err := filepath.Glob(filepath.Join(f.dir, "sessions", "*", "*.jsonl"))
	if err != nil {
		f.t.Fatal(err)
	}
	return paths
}

// step is one scripted model response.
type step func(ctx context.Context, yield func(fantasy.StreamPart) bool)

// scriptedModel answers each Stream call with the next queued step and
// records the call. With no step queued, Stream fails.
type scriptedModel struct {
	provider, wire string

	mu    sync.Mutex
	steps []step
	calls []fantasy.Call
}

func (m *scriptedModel) push(steps ...step) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, steps...)
}

func (m *scriptedModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// requests is every call the model has been sent, cloned: a test that reads
// them while a turn may still be running must not touch the slice itself.
func (m *scriptedModel) requests() []fantasy.Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]fantasy.Call(nil), m.calls...)
}

func (m *scriptedModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, call)
	if len(m.steps) == 0 {
		return nil, errors.New("scripted: no step queued")
	}
	next := m.steps[0]
	m.steps = m.steps[1:]
	return func(yield func(fantasy.StreamPart) bool) { next(ctx, yield) }, nil
}

func (m *scriptedModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("scripted: Generate is not used")
}

func (m *scriptedModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("scripted: GenerateObject is not used")
}

func (m *scriptedModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("scripted: StreamObject is not used")
}

func (m *scriptedModel) Provider() string { return m.provider }
func (m *scriptedModel) Model() string    { return m.wire }

func textParts(chunks ...string) []fantasy.StreamPart {
	parts := []fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextStart, ID: "0"}}
	for _, c := range chunks {
		parts = append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: c})
	}
	return append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "0"})
}

func thoughtParts(chunks ...string) []fantasy.StreamPart {
	parts := []fantasy.StreamPart{{Type: fantasy.StreamPartTypeReasoningStart, ID: "r"}}
	for _, c := range chunks {
		parts = append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: "r", Delta: c})
	}
	return append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: "r"})
}

func finishParts(reason fantasy.FinishReason) []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: reason,
		Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}}}
}

func errorParts(err error) []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: err}}
}

// reply is a step that yields its parts and ends.
func reply(parts ...[]fantasy.StreamPart) step {
	return func(_ context.Context, yield func(fantasy.StreamPart) bool) {
		for _, group := range parts {
			for _, p := range group {
				if !yield(p) {
					return
				}
			}
		}
	}
}

// answer is the common one-step reply: text, then a clean finish.
func answer(chunks ...string) step {
	return reply(textParts(chunks...), finishParts(fantasy.FinishReasonStop))
}

// held is a sync point inside a scripted step, so a test can act at a known
// moment of a turn.
type held struct {
	reached   chan struct{} // closed once the step has yielded its first parts
	release   chan struct{} // closed by the test: the step yields the rest and ends
	cancelled chan struct{} // closed once the step has seen its context end
	// linger, when non-nil, keeps a cancelled step from returning until the
	// test closes it: the turn is still running after its context ended.
	linger chan struct{}
}

func newHeld(t *testing.T) *held {
	h := &held{reached: make(chan struct{}), release: make(chan struct{}), cancelled: make(chan struct{})}
	t.Cleanup(func() { closeOnce(h.release); closeOnce(h.linger) })
	return h
}

// closeOnce closes ch unless it is nil or already closed; test cleanup only.
func closeOnce(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// step yields before, signals reached, and waits. A cancel ends it with the
// context's error part, as a real provider's stream ends when its request is
// cancelled; a release yields after.
func (h *held) step(before, after []fantasy.StreamPart) step {
	return func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		for _, p := range before {
			if !yield(p) {
				return
			}
		}
		close(h.reached)
		select {
		case <-ctx.Done():
			close(h.cancelled)
			if h.linger != nil {
				<-h.linger
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
			return
		case <-h.release:
		}
		for _, p := range after {
			if !yield(p) {
				return
			}
		}
	}
}

// outcome is what a continuation returned.
type outcome struct {
	res Result
	err error
}

// startPrompt runs a prompt on its own goroutine.
func startPrompt(s Session, text string) <-chan outcome {
	out := make(chan outcome, 1)
	go func() {
		res, err := s.Prompt(context.Background(), text)
		out <- outcome{res, err}
	}()
	return out
}

func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(nativeWait):
		t.Fatalf("timed out after %v waiting for %s", nativeWait, what)
		panic("unreachable")
	}
}

// drained is every event already buffered, without waiting for more. Every
// ending the adapter emits is emitted before its continuation returns, so
// after a Prompt has returned this is the whole turn.
func drained(s Session) []Event {
	var out []Event
	for {
		select {
		case ev := <-s.Events():
			out = append(out, ev)
		default:
			return out
		}
	}
}

func ofType(evs []Event, typ EventType) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func joined(evs []Event, typ EventType) string {
	var b strings.Builder
	for _, ev := range ofType(evs, typ) {
		b.WriteString(ev.Text)
	}
	return b.String()
}

// endings checks the turn-ending contract (live.go's prompt): a turn that
// did not fail ends in exactly one EventDone with stop and no EventError; a
// failed one (stop "") in exactly one EventError and no EventDone. It
// returns the error event's error, or nil.
func endings(t *testing.T, evs []Event, stop string) error {
	t.Helper()
	dones, errs := ofType(evs, EventDone), ofType(evs, EventError)
	if stop == "" {
		if len(errs) != 1 || len(dones) != 0 {
			t.Fatalf("a failed turn emitted %d EventError and %d EventDone, want 1 and 0: %+v", len(errs), len(dones), evs)
		}
		return errs[0].Err
	}
	if len(dones) != 1 || len(errs) != 0 {
		t.Fatalf("a turn that did not fail emitted %d EventDone and %d EventError, want 1 and 0: %+v", len(dones), len(errs), evs)
	}
	if dones[0].StopReason != stop {
		t.Fatalf("EventDone.StopReason = %q, want %q", dones[0].StopReason, stop)
	}
	return nil
}

// TestNativeProviderIsRegisteredHidden: the native provider resolves by id
// and is listed nowhere (plan 018 §3.4), is in-process, shows its label, and
// has exactly the one capability H1 gives it.
func TestNativeProviderIsRegisteredHidden(t *testing.T) {
	p, err := ProviderByName("native")
	if err != nil {
		t.Fatalf("ProviderByName(native): %v", err)
	}
	if !p.Hidden() || !p.InProcess() || p.DisplayName() != "native" {
		t.Fatalf("native is hidden=%v inProcess=%v label %q", p.Hidden(), p.InProcess(), p.DisplayName())
	}
	if got := p.Capabilities(); got != (Capabilities{Effort: true, Interject: true}) {
		t.Fatalf("Capabilities = %+v, want effort and interject alone", got)
	}
	if slices.Contains(ProviderNames(), "native") {
		t.Fatalf("ProviderNames lists native: %q", ProviderNames())
	}
	if !p.BinaryResolves("") {
		t.Fatal("an in-process provider must resolve without a binary")
	}
	if info := p.Info(); info.Label() != "native" || info.Capabilities() != p.Capabilities() {
		t.Fatalf("ProviderInfo label %q caps %+v", info.Label(), info.Capabilities())
	}
}

// TestNewBranchesOnInProcess: agent.New builds the native adapter for an
// in-process provider and the ACP session for everything else, and the
// adapter names the native provider from its very first snapshot.
func TestNewBranchesOnInProcess(t *testing.T) {
	native := NativeProvider()
	s := New(Options{Provider: &native})
	closeAtCleanup(t, s)
	if _, ok := s.(*nativeSession); !ok {
		t.Fatalf("New(native) = %T, want the native adapter", s)
	}
	if got := s.Snapshot().Provider.Name; got != "native" {
		t.Fatalf("an unstarted native snapshot names provider %q", got)
	}
	for _, p := range []*Provider{nil, ptr(CursorProvider()), ptr(GrokProvider()), ptr(GxProvider())} {
		acpSess := New(Options{Provider: p})
		if _, ok := acpSess.(*session); !ok {
			t.Fatalf("New(%v) = %T, want the ACP session", p, acpSess)
		}
		_ = acpSess.Close()
	}
}

func ptr[T any](v T) *T { return &v }

// TestNativeStart: the first snapshot carries the table's models by alias
// (names sanitized), the current model, the harness's session id, the
// native provider and an effort option EffortOption finds, and no title
// until a prompt gives it one.
func TestNativeStart(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	snap := s.Snapshot()
	want := []ModelInfo{{ID: "nokey/d", Name: "nokey/d"}, {ID: "other/c", Name: "other/c"},
		{ID: "test/a", Name: "Model A"}, {ID: "test/b", Name: "Model B"}}
	if !reflect.DeepEqual(snap.Models, want) {
		t.Fatalf("Models = %+v, want %+v", snap.Models, want)
	}
	if snap.CurrentModel != "test/a" || snap.SessionID == "" || snap.SessionID != s.hs.ID() || snap.Title != "" {
		t.Fatalf("snapshot current %q session %q title %q", snap.CurrentModel, snap.SessionID, snap.Title)
	}
	if snap.Provider.Name != "native" || !snap.Provider.Capabilities().Effort {
		t.Fatalf("snapshot provider %+v", snap.Provider)
	}
	opt := EffortOption(snap)
	if opt == nil || opt.ID != nativeEffortID || opt.Current != "high" ||
		!reflect.DeepEqual(opt.SelectValues, []SelectValue{{Value: "low", Name: "low"}, {Value: "high", Name: "high"}}) {
		t.Fatalf("EffortOption = %+v, want low/high at high", opt)
	}
	if len(snap.Modes) != 0 || len(snap.Commands) != 0 || FastOption(snap) != nil {
		t.Fatalf("a native snapshot advertises modes %v, commands %v, fast %v", snap.Modes, snap.Commands, FastOption(snap))
	}
	// Nothing is written until a turn has output.
	if got := f.transcripts(); len(got) != 0 {
		t.Fatalf("Start wrote transcripts: %v", got)
	}
}

// TestNativeStartModel: --model resolves with MatchModel's normalisation —
// alias, alias spelled loosely, or display name — and a model with no
// effort control publishes no effort option, so the effort UI hides.
func TestNativeStartModel(t *testing.T) {
	for _, tc := range []struct {
		model, want string
		effort      bool
	}{
		{"test/b", "test/b", true},
		{"TEST/B", "test/b", true},
		{"model a", "test/a", true},
		{"other/c", "other/c", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			f := newNativeFixture(t)
			snap := f.started(Options{Model: tc.model}).Snapshot()
			if snap.CurrentModel != tc.want {
				t.Fatalf("CurrentModel = %q, want %q", snap.CurrentModel, tc.want)
			}
			if got := EffortOption(snap) != nil; got != tc.effort {
				t.Fatalf("effort option present = %v, want %v (config %+v)", got, tc.effort, snap.Config)
			}
		})
	}
}

// TestNativeStartRefusals: every way Start can refuse, each phrased so the
// user knows what to do, none carrying a key, and each leaving the session
// able to be closed.
func TestNativeStartRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *nativeFixture)
		opts  Options
		want  []string // substrings of the error
		is    error
	}{
		{name: "session/load", opts: Options{LoadSessionID: "abc"},
			want: []string{"agent: native does not support session/load yet"}},
		{name: "a mode", opts: Options{Mode: "plan"}, want: []string{`mode "plan"`, "no modes"}},
		{name: "no model table", setup: func(t *testing.T, f *nativeFixture) {
			t.Setenv("CRAZE_HOME", t.TempDir())
		}, want: []string{`native: no models configured — run "craze import gx"`}, is: errNoModels},
		{name: "no craze directory", setup: func(t *testing.T, f *nativeFixture) {
			t.Setenv("CRAZE_HOME", "")
			t.Setenv("HOME", "")
		}, want: []string{"no craze directory"}},
		{name: "unknown model", opts: Options{Model: "nope"},
			want: []string{`unknown model "nope"`, "nokey/d, other/c, test/a, test/b"}},
		{name: "explicit model with no key", opts: Options{Model: "nokey/d"},
			want: []string{`model "nokey/d" has no API key`, "NATIVE_NOKEY_KEY"}, is: harness.ErrNoAPIKey},
		{name: "no model has a key", setup: func(t *testing.T, f *nativeFixture) {
			f.env = map[string]string{}
		}, want: []string{"no configured model has an API key", `"test/a"`, "NATIVE_TEST_KEY"}, is: harness.ErrNoAPIKey},
		{name: "explicit default with no key does not fall back", setup: func(t *testing.T, f *nativeFixture) {
			delete(f.env, "NATIVE_TEST_KEY")
		}, opts: Options{Model: "test/a"}, want: []string{`model "test/a" has no API key`}, is: harness.ErrNoAPIKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			s := f.session(tc.opts)
			err := s.Start(context.Background())
			if err == nil {
				t.Fatal("Start succeeded")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("Start error %q does not say %q", err, w)
				}
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("Start error %q does not match %v", err, tc.is)
			}
			if leaks := nativeLeaks(err, nativeCanary); len(leaks) > 0 {
				t.Errorf("the key leaked into the Start error at %v", leaks)
			}
			if got := s.Snapshot().SessionID; got != "" {
				t.Errorf("a refused Start published session %q", got)
			}
			if err := s.Close(); err != nil {
				t.Errorf("Close after a refused Start: %v", err)
			}
		})
	}
}

// TestNativeStartFallsBackFromAnUnfundedDefault: with no --model, a default
// whose provider has no key gives way to the lexicographically first alias
// whose key resolves, and the fallback is noted on Diag (plan 018 §3.8).
func TestNativeStartFallsBackFromAnUnfundedDefault(t *testing.T) {
	f := newNativeFixture(t)
	delete(f.env, "NATIVE_TEST_KEY") // test/a and test/b are unfunded; nokey/d always is
	var diag bytes.Buffer
	s := f.started(Options{Diag: &diag})
	if got := s.Snapshot().CurrentModel; got != "other/c" {
		t.Fatalf("CurrentModel = %q, want the first funded alias other/c", got)
	}
	if !strings.Contains(diag.String(), `the default model "test/a" has no API key; starting on "other/c"`) {
		t.Fatalf("Diag = %q, want the fallback noted", diag.String())
	}
	f.models["other/c"].push(answer("ok"))
	if res, err := s.Prompt(context.Background(), "hi"); err != nil || res.StopReason != "end_turn" {
		t.Fatalf("Prompt on the fallback = %+v, %v", res, err)
	}
}

// TestNativeStartLifecycle: Close before Start is safe and idempotent, a
// second Start is refused, and a Start after Close is refused.
func TestNativeStartLifecycle(t *testing.T) {
	f := newNativeFixture(t)
	s := f.session(Options{})
	if err := s.Close(); err != nil {
		t.Fatalf("Close before Start: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
	if err := s.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Start after Close = %v", err)
	}
	if err := s.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel on a closed, never-started session: %v", err)
	}

	s2 := f.started(Options{})
	if err := s2.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Fatalf("a second Start = %v", err)
	}
}

// TestNativeTurnEndings is the turn-ending contract (plan 018 §3.8, live.go's
// prompt): a turn that did not fail ends in exactly one EventDone carrying
// the harness's stop reason and no EventError; a failed turn ends in exactly
// one EventError and no EventDone, and returns that same error. Text and
// thinking arrive as EventText and EventThought, sanitized.
func TestNativeTurnEndings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		step     step
		stop     string // "" = the turn fails
		text     string
		thought  string
		errWords string
	}{
		{name: "end_turn", step: answer("hel", "lo"), stop: "end_turn", text: "hello"},
		{name: "thinking then text", step: reply(thoughtParts("hm", "m"), textParts("ok"), finishParts(fantasy.FinishReasonStop)),
			stop: "end_turn", text: "ok", thought: "hmm"},
		{name: "max_tokens", step: reply(textParts("cut"), finishParts(fantasy.FinishReasonLength)), stop: "max_tokens", text: "cut"},
		{name: "refusal", step: reply(textParts("no"), finishParts(fantasy.FinishReasonContentFilter)), stop: "refusal", text: "no"},
		{name: "escapes are stripped", step: answer("\x1b]0;pwned\x07safe", "\x1b[31m text\x1b[0m"),
			stop: "end_turn", text: "safe text"},
		// The harness has already cut the message to one line with no control
		// characters; a zero-width rune survives that, and the adapter's own
		// sanitizing is what removes it.
		{name: "a provider error", step: reply(errorParts(&fantasy.ProviderError{StatusCode: 400, Message: "bad" + zeroWidthSpace + " request"})),
			errWords: `native: provider "test" failed (HTTP 400): bad request`},
		{name: "a failure after text", step: reply(textParts("part")[:2], errorParts(&fantasy.ProviderError{StatusCode: 422, Message: "nope"})),
			text: "part", errWords: `native: provider "test" failed (HTTP 422): nope`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t)
			s := f.started(Options{})
			f.models["test/a"].push(tc.step)
			res, err := s.Prompt(context.Background(), "hi")
			evs := drained(s)
			evErr := endings(t, evs, tc.stop)
			if tc.stop != "" {
				if err != nil || res.StopReason != tc.stop {
					t.Fatalf("Prompt = %+v, %v; want %s", res, err, tc.stop)
				}
			} else {
				if err == nil || res != (Result{}) {
					t.Fatalf("Prompt = %+v, %v; want a zero Result and an error", res, err)
				}
				if evErr != err {
					t.Fatalf("EventError carries %v, Prompt returned %v: want the same error", evErr, err)
				}
				if err.Error() != tc.errWords {
					t.Fatalf("error %q, want %q", err, tc.errWords)
				}
			}
			if got := joined(evs, EventText); got != tc.text {
				t.Fatalf("text %q, want %q", got, tc.text)
			}
			if got := joined(evs, EventThought); got != tc.thought {
				t.Fatalf("thought %q, want %q", got, tc.thought)
			}
			if last := evs[len(evs)-1].Type; last != EventDone && last != EventError {
				t.Fatalf("the last event is %s, want the turn's ending", last)
			}
		})
	}
}

// TestNativeTypedErrorsArePhrased: each of the harness's typed failures is
// said in the adapter's own words, still matches its sentinel, and never
// says ErrInTurn's name or anything raw. (The empty step is the llm
// wrapper's and runs over the wire, in TestNativeWireErrorsArePhrasedWithoutTheKey.)
func TestNativeTypedErrorsArePhrased(t *testing.T) {
	for _, tc := range []struct {
		name string
		perr *fantasy.ProviderError
		is   error
		want string
	}{
		{"401", &fantasy.ProviderError{StatusCode: 401, Message: "who are you"}, harness.ErrAuth,
			`native: provider "test" rejected the API key (HTTP 401); check its env_keys or api_key in providers.toml`},
		{"auth flag", &fantasy.ProviderError{StatusCode: 400, AuthError: true}, harness.ErrAuth,
			`native: provider "test" rejected the API key (HTTP 400); check its env_keys or api_key in providers.toml`},
		{"404", &fantasy.ProviderError{StatusCode: 404, Message: "no such model"}, harness.ErrModelNotFound,
			`native: provider "test" does not serve model "test/a" (HTTP 404); check its wire_model in models.toml`},
		{"context too large", &fantasy.ProviderError{StatusCode: 400, ContextTooLargeErr: true}, harness.ErrContextTooLarge,
			`native: the conversation no longer fits model "test/a"'s context window (HTTP 400); start a new session (compaction arrives with H7)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t)
			s := f.started(Options{})
			f.models["test/a"].push(reply(errorParts(tc.perr)))
			_, err := s.Prompt(context.Background(), "hi")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error %q, want %q", err, tc.want)
			}
			if !errors.Is(err, tc.is) {
				t.Fatalf("error %q does not match %v", err, tc.is)
			}
			if endings(t, drained(s), "") != err {
				t.Fatal("the EventError does not carry the returned error")
			}
		})
	}
}

// TestNativeFailureClearsTheQueue: after a failed turn's EventError, every
// queued row is removed with its own EventQueue, in order — nothing drains
// from an error state (live.go's clearQueueOnError).
func TestNativeFailureClearsTheQueue(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	for _, text := range []string{"one", "two"} {
		if _, err := s.Queue(text); err != nil {
			t.Fatal(err)
		}
	}
	if got := ofType(drained(s), EventQueue); len(got) != 2 {
		t.Fatalf("queueing emitted %d EventQueue, want 2", len(got))
	}
	f.models["test/a"].push(reply(errorParts(&fantasy.ProviderError{StatusCode: 400, Message: "bad"})))
	if _, err := s.Prompt(context.Background(), "hi"); err == nil {
		t.Fatal("the turn did not fail")
	}
	evs := drained(s)
	endings(t, evs, "")
	var got []string
	for _, ev := range evs {
		switch ev.Type {
		case EventError:
			got = append(got, "error")
		case EventQueue:
			got = append(got, fmt.Sprintf("%s %s @%d", ev.QueueChange, ev.Queue.Text, ev.QueuePos))
		}
	}
	if want := []string{"error", "removed one @0", "removed two @0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("failure emitted %q, want %q", got, want)
	}
	if q := s.Snapshot().Queue; len(q) != 0 {
		t.Fatalf("the queue survived the error: %+v", q)
	}
}

// TestNativeCancel: a cancelled turn ends in exactly one EventDone{cancelled}
// and no EventError, returns Result{cancelled}, and its partial answer is
// persisted interrupted. A Cancel with nothing claimed is a no-op that does
// not withdraw the next prompt.
func TestNativeCancel(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	if err := s.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel while idle: %v", err)
	}
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("partial")[:2], nil))
	out := startPrompt(s, "hi")
	await(t, h.reached, "the turn to stream")
	if err := s.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got := await(t, out, "the cancelled prompt to return")
	if got.err != nil || got.res.StopReason != "cancelled" {
		t.Fatalf("Prompt = %+v, %v; want cancelled", got.res, got.err)
	}
	evs := drained(s)
	endings(t, evs, "cancelled")
	if text := joined(evs, EventText); text != "partial" {
		t.Fatalf("text %q, want the partial answer", text)
	}
	assertInterrupted(t, f, "partial")
}

// assertInterrupted checks the session's one transcript ends in an
// interrupted assistant entry holding text.
func assertInterrupted(t *testing.T, f *nativeFixture, text string) {
	t.Helper()
	paths := f.transcripts()
	if len(paths) != 1 {
		t.Fatalf("transcripts %v, want one", paths)
	}
	tr, err := store.Load(paths[0])
	if err != nil {
		t.Fatalf("loading the transcript: %v", err)
	}
	last := tr.Entries[len(tr.Entries)-1]
	if last.Type != store.TypeMessage || last.Message.Role != fantasy.MessageRoleAssistant || !last.Interrupted {
		t.Fatalf("the last entry is %+v, want an interrupted assistant message", last)
	}
	var got string
	for _, p := range last.Message.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
			got += tp.Text
		}
	}
	if got != text {
		t.Fatalf("the interrupted answer is %q, want %q", got, text)
	}
}

// TestNativeCancelWaits is the live session's behaviour and not tui.Stub's
// (plan 018 §2.1): Cancel returns only once the turn has finished. A turn that
// keeps running after its context ended holds Cancel, so Cancel's own context
// ending first is what it returns; once the turn is let go, a Cancel with no
// deadline returns with the turn's EventDone already emitted and the slot
// free.
func TestNativeCancelWaits(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})

	h := newHeld(t)
	h.linger = make(chan struct{})
	f.models["test/a"].push(h.step(nil, nil))
	out := startPrompt(s, "first")
	await(t, h.reached, "the first turn to open")
	cctx, stop := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() { cancelled <- s.Cancel(cctx) }()
	await(t, h.cancelled, "the turn to see its context end")
	// The turn is still running (lingering), so a Cancel that waits is
	// still waiting and can only answer with its own context's end.
	stop()
	if err := await(t, cancelled, "Cancel to give up"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Cancel = %v while the turn was still running, want context.Canceled: it did not wait", err)
	}
	close(h.linger)
	if got := await(t, out, "the first prompt"); got.err != nil || got.res.StopReason != "cancelled" {
		t.Fatalf("first prompt = %+v, %v", got.res, got.err)
	}
	drained(s)

	h2 := newHeld(t)
	h2.linger = make(chan struct{})
	f.models["test/a"].push(h2.step(nil, nil))
	out2 := startPrompt(s, "second")
	await(t, h2.reached, "the second turn to open")
	returned := make(chan error, 1)
	go func() { returned <- s.Cancel(context.Background()) }()
	await(t, h2.cancelled, "the second turn to see its context end")
	close(h2.linger)
	if err := await(t, returned, "Cancel to return"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// Cancel has returned, so the turn's ending is already buffered and the
	// slot is free — no waiting on the prompt's goroutine.
	endings(t, drained(s), "cancelled")
	if _, ok := s.PopQueue(); ok {
		t.Fatal("PopQueue on an empty queue")
	}
	s.mu.Lock()
	busy := s.claimed || s.inPrompt
	s.mu.Unlock()
	if busy {
		t.Fatal("Cancel returned with the prompt slot still held")
	}
	await(t, out2, "the second prompt")
}

// TestNativeBeginWithdraws is Plan 017's claim-then-withdraw: a Cancel after
// Begin and before the continuation runs makes the continuation return
// ErrPromptCancelled having emitted nothing, called no model and written
// nothing; the next Begin claims afresh, because a cancel asked before a
// claim is not that prompt's.
func TestNativeBeginWithdraws(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	run := s.Begin("hi")
	// Cancel waits for the claimed prompt to withdraw, which needs the
	// continuation to run — on this goroutine, below — so it is given a
	// context that has already ended: it marks the claim and returns.
	gone, stop := context.WithCancel(context.Background())
	stop()
	if err := s.Cancel(gone); !errors.Is(err, context.Canceled) {
		t.Fatalf("Cancel on a claimed prompt = %v", err)
	}
	res, err := run(context.Background())
	if !errors.Is(err, ErrPromptCancelled) || res != (Result{}) {
		t.Fatalf("the continuation = %+v, %v; want ErrPromptCancelled", res, err)
	}
	if evs := drained(s); len(evs) != 0 {
		t.Fatalf("a withdrawn prompt emitted %+v", evs)
	}
	if n := f.models["test/a"].callCount(); n != 0 {
		t.Fatalf("a withdrawn prompt reached the model %d times", n)
	}
	if got := f.transcripts(); len(got) != 0 {
		t.Fatalf("a withdrawn prompt wrote %v", got)
	}
	if title := s.Snapshot().Title; title != "" {
		t.Fatalf("a withdrawn prompt titled the session %q", title)
	}

	f.models["test/a"].push(answer("ok"))
	if res, err := s.Prompt(context.Background(), "again"); err != nil || res.StopReason != "end_turn" {
		t.Fatalf("the prompt after a withdraw = %+v, %v", res, err)
	}
}

// TestNativeBeginWhileClaimed: a Begin while the slot is claimed claims
// nothing, and its continuation returns ErrPromptInFlight with no event; the
// claim it could not take goes on to run normally.
func TestNativeBeginWhileClaimed(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	first := s.Begin("first")
	if _, err := s.Begin("second")(context.Background()); !errors.Is(err, ErrPromptInFlight) {
		t.Fatalf("a Begin over a claim = %v, want ErrPromptInFlight", err)
	}
	if _, err := s.Prompt(context.Background(), "third"); !errors.Is(err, ErrPromptInFlight) {
		t.Fatalf("a Prompt over a claim = %v, want ErrPromptInFlight", err)
	}
	if evs := drained(s); len(evs) != 0 {
		t.Fatalf("a refused Begin emitted %+v", evs)
	}
	f.models["test/a"].push(answer("ok"))
	if res, err := first(context.Background()); err != nil || res.StopReason != "end_turn" {
		t.Fatalf("the claimed prompt = %+v, %v", res, err)
	}
	if n := f.models["test/a"].callCount(); n != 1 {
		t.Fatalf("the model was called %d times, want once", n)
	}
}

// TestNativeContinuationRunsOnce: Begin's continuation is safe to call twice.
// A second call — after the first returned, or alongside it — returns
// ErrPromptInFlight, reaches no model, emits nothing, and leaves the session
// (a later claim included) exactly as it was.
func TestNativeContinuationRunsOnce(t *testing.T) {
	t.Run("sequential", func(t *testing.T) {
		f := newNativeFixture(t)
		s := f.started(Options{})
		f.models["test/a"].push(answer("ok"))
		run := s.Begin("hi")
		if res, err := run(context.Background()); err != nil || res.StopReason != "end_turn" {
			t.Fatalf("the first call = %+v, %v", res, err)
		}
		drained(s)
		if res, err := run(context.Background()); !errors.Is(err, ErrPromptInFlight) || res != (Result{}) {
			t.Fatalf("the second call = %+v, %v; want ErrPromptInFlight", res, err)
		}
		// A later claim is not the stale continuation's to run or release.
		next := s.Begin("next")
		if _, err := run(context.Background()); !errors.Is(err, ErrPromptInFlight) {
			t.Fatalf("the stale continuation over a new claim = %v", err)
		}
		if _, err := s.Begin("over the claim")(context.Background()); !errors.Is(err, ErrPromptInFlight) {
			t.Fatalf("the new claim was released by the stale continuation: Begin = %v", err)
		}
		if evs := drained(s); len(evs) != 0 {
			t.Fatalf("repeat calls emitted %+v", evs)
		}
		f.models["test/a"].push(answer("ok"))
		if res, err := next(context.Background()); err != nil || res.StopReason != "end_turn" {
			t.Fatalf("the new claim = %+v, %v", res, err)
		}
		if n := f.models["test/a"].callCount(); n != 2 {
			t.Fatalf("the model was called %d times, want twice (once per claim)", n)
		}
	})
	t.Run("concurrent", func(t *testing.T) {
		f := newNativeFixture(t)
		s := f.started(Options{})
		h := newHeld(t)
		f.models["test/a"].push(h.step(textParts("ok"), finishParts(fantasy.FinishReasonStop)))
		run := s.Begin("hi")
		outs := make(chan outcome, 2)
		for range 2 {
			go func() {
				res, err := run(context.Background())
				outs <- outcome{res, err}
			}()
		}
		await(t, h.reached, "one call's turn to open")
		// The call that opened the turn is held, so the first to return is
		// the other one.
		if got := await(t, outs, "the call that does not run"); !errors.Is(got.err, ErrPromptInFlight) {
			t.Fatalf("the losing call = %+v, %v; want ErrPromptInFlight", got.res, got.err)
		}
		close(h.release)
		if got := await(t, outs, "the call that runs"); got.err != nil || got.res.StopReason != "end_turn" {
			t.Fatalf("the winning call = %+v, %v", got.res, got.err)
		}
		endings(t, drained(s), "end_turn")
		if n := f.models["test/a"].callCount(); n != 1 {
			t.Fatalf("the model was called %d times, want once", n)
		}
	})
}

// TestNativeCallerContextEndingIsAFailure: a turn ended by the caller's own
// context — its deadline or its cancel — and not by Cancel or Close is a
// failed prompt, as on the live session: one EventError wrapping the
// context's error, no EventDone, and the queue cleared with removals. The
// session's own Cancel stays a clean stop that keeps the queue.
func TestNativeCallerContextEndingIsAFailure(t *testing.T) {
	// setup is a started session with one queued row and a turn that holds
	// until its context ends.
	setup := func(t *testing.T) (*nativeSession, *held) {
		f := newNativeFixture(t)
		s := f.started(Options{})
		if _, err := s.Queue("queued"); err != nil {
			t.Fatal(err)
		}
		drained(s)
		h := newHeld(t)
		f.models["test/a"].push(h.step(nil, nil))
		return s, h
	}
	failed := func(t *testing.T, s *nativeSession, got outcome, cause error, msg string) {
		t.Helper()
		if !errors.Is(got.err, cause) || got.err.Error() != msg || got.res != (Result{}) {
			t.Fatalf("Prompt = %+v, %v; want a zero Result and %q wrapping %v", got.res, got.err, msg, cause)
		}
		evs := drained(s)
		if endings(t, evs, "") != got.err {
			t.Fatal("the EventError does not carry the returned error")
		}
		var seq []string
		for _, ev := range evs {
			switch ev.Type {
			case EventError:
				seq = append(seq, "error")
			case EventQueue:
				seq = append(seq, fmt.Sprintf("%s %s", ev.QueueChange, ev.Queue.Text))
			}
		}
		if want := []string{"error", "removed queued"}; !reflect.DeepEqual(seq, want) {
			t.Fatalf("events %q, want %q", seq, want)
		}
		if q := s.Snapshot().Queue; len(q) != 0 {
			t.Fatalf("the queue survived: %+v", q)
		}
	}

	t.Run("deadline", func(t *testing.T) {
		s, _ := setup(t)
		// The held turn waits for its context, so the deadline is what ends
		// it whenever it fires; the value only sets how long the test takes.
		ctx, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer stop()
		out := make(chan outcome, 1)
		go func() {
			res, err := s.Prompt(ctx, "hi")
			out <- outcome{res, err}
		}()
		failed(t, s, await(t, out, "the prompt past its deadline"),
			context.DeadlineExceeded, "native: prompt cancelled: context deadline exceeded")
	})
	t.Run("the caller cancels", func(t *testing.T) {
		s, h := setup(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := make(chan outcome, 1)
		go func() {
			res, err := s.Prompt(ctx, "hi")
			out <- outcome{res, err}
		}()
		await(t, h.reached, "the turn to open")
		cancel()
		failed(t, s, await(t, out, "the prompt its caller cancelled"),
			context.Canceled, "native: prompt cancelled: context canceled")
	})
	t.Run("the session's Cancel is still a clean stop", func(t *testing.T) {
		s, h := setup(t)
		out := startPrompt(s, "hi")
		await(t, h.reached, "the turn to open")
		if err := s.Cancel(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := await(t, out, "the cancelled prompt"); got.err != nil || got.res.StopReason != "cancelled" {
			t.Fatalf("Prompt = %+v, %v; want cancelled", got.res, got.err)
		}
		evs := drained(s)
		endings(t, evs, "cancelled")
		if q := ofType(evs, EventQueue); len(q) != 0 {
			t.Fatalf("a Cancel touched the queue: %+v", q)
		}
		if q := s.Snapshot().Queue; len(q) != 1 || q[0].Text != "queued" {
			t.Fatalf("the queue after a Cancel = %+v, want the row kept", q)
		}
	})
}

// TestNativeQueueGuardsTheClaim: TakeQueued and PopQueue refuse while a
// prompt is claimed and not yet open, as the live session's do (Plan 017),
// as well as while it runs; the row stays and no event is emitted. Once the
// prompt has returned they hand the row over with its EventQueue.
func TestNativeQueueGuardsTheClaim(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	q, err := s.Queue("queued")
	if err != nil {
		t.Fatal(err)
	}
	drained(s)

	run := s.Begin("claimed")
	if _, ok := s.TakeQueued(q.ID); ok {
		t.Fatal("TakeQueued took a row while a prompt was claimed")
	}
	if _, ok := s.PopQueue(); ok {
		t.Fatal("PopQueue took a row while a prompt was claimed")
	}
	h := newHeld(t)
	f.models["test/a"].push(h.step(nil, finishParts(fantasy.FinishReasonStop)))
	out := make(chan outcome, 1)
	go func() {
		res, err := run(context.Background())
		out <- outcome{res, err}
	}()
	await(t, h.reached, "the claimed turn to open")
	if _, ok := s.PopQueue(); ok {
		t.Fatal("PopQueue took a row while a turn was running")
	}
	if evs := ofType(drained(s), EventQueue); len(evs) != 0 {
		t.Fatalf("a refused take emitted %+v", evs)
	}
	close(h.release)
	await(t, out, "the claimed prompt")
	got, ok := s.PopQueue()
	if !ok || got.ID != q.ID {
		t.Fatalf("PopQueue after the turn = %+v, %v", got, ok)
	}
	sent := ofType(drained(s), EventQueue)
	if len(sent) != 1 || sent[0].QueueChange != QueueSent || sent[0].Queue.ID != q.ID {
		t.Fatalf("the take emitted %+v, want one sent", sent)
	}
}

// TestNativeCloseDuringATurn: Close cancels a live turn and waits for it —
// the partial answer is persisted interrupted — and returns nil, as does a
// second Close. The turn's continuation returns cancelled; nothing it emits
// after Close reaches the channel, and a prompt after Close sends nothing.
func TestNativeCloseDuringATurn(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("half an answer")[:2], nil))
	out := startPrompt(s, "hi")
	await(t, h.reached, "the turn to stream")
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	if err := await(t, closed, "Close during a turn"); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	// Close waited for the turn: its persist is on disk now.
	assertInterrupted(t, f, "half an answer")
	if got := await(t, out, "the prompt Close cancelled"); got.err != nil || got.res.StopReason != "cancelled" {
		t.Fatalf("Prompt = %+v, %v; want cancelled", got.res, got.err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("a second Close = %v", err)
	}
	before := len(drained(s))
	if _, err := s.Prompt(context.Background(), "after"); err == nil {
		t.Fatal("a Prompt after Close succeeded")
	}
	if after := len(drained(s)); after != 0 {
		t.Fatalf("a Prompt after Close emitted %d events (%d were buffered before)", after, before)
	}
}

// TestNativeCloseBeforeTheContinuation: Close does not wait for a prompt that
// is claimed and not yet open — its continuation may be due on the caller's
// own goroutine — and that continuation then sends nothing.
func TestNativeCloseBeforeTheContinuation(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	run := s.Begin("hi")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := run(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("the continuation after Close = %v", err)
	}
	if n := f.models["test/a"].callCount(); n != 0 {
		t.Fatalf("the model was called %d times after Close", n)
	}
}

// TestNativeBurstWithNoReaderDoesNotDeadlockClose: a turn streaming far more
// events than the channel holds, with nobody reading, parks the harness's
// callback on a full channel; Close must still return, because it closes the
// signal every emit also selects on before it waits for the turn.
func TestNativeBurstWithNoReaderDoesNotDeadlockClose(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	started := make(chan struct{})
	f.models["test/a"].push(func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		close(started)
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "0"}) {
			return
		}
		for i := 0; i < 300; i++ {
			if ctx.Err() != nil {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: "x"}) {
				return
			}
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "0"})
		for _, p := range finishParts(fantasy.FinishReasonStop) {
			yield(p)
		}
	})
	out := startPrompt(s, "burst")
	await(t, started, "the burst to start")
	// The bounded wait is the assertion: with an emit that ignored the close
	// signal, this Close would wait for ever on a turn parked on the full
	// channel.
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	if err := await(t, closed, "Close with the event channel full and no reader"); err != nil {
		t.Fatal(err)
	}
	await(t, out, "the burst's prompt to return")
}

// TestNativeSetModelDuringATurn: a model switch is allowed while a turn runs,
// as on the live session: the snapshot shows it at once (with the effort
// option rebuilt for the new model), the running turn finishes on the model
// it started with, and the next turn runs on the new one. A switch to a model
// with no key fails, naming what to set, and leaves the model in place.
func TestNativeSetModelDuringATurn(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("from a")[:2], cat(textParts("")[2:], finishParts(fantasy.FinishReasonStop))))
	out := startPrompt(s, "one")
	await(t, h.reached, "the turn on test/a")
	if err := s.SetModel(context.Background(), "test/b"); err != nil {
		t.Fatalf("SetModel during a turn: %v", err)
	}
	snap := s.Snapshot()
	opt := EffortOption(snap)
	if snap.CurrentModel != "test/b" || opt == nil || opt.Current != "high" ||
		!reflect.DeepEqual(opt.SelectValues, []SelectValue{{Value: "medium", Name: "medium"}, {Value: "high", Name: "high"}}) {
		// "high" is kept: test/b lists it too (plan 018 §3.7).
		t.Fatalf("after SetModel: current %q, effort %+v", snap.CurrentModel, opt)
	}
	if meta := ofType(drained(s), EventMeta); len(meta) != 1 || meta[0].Text != "" {
		t.Fatalf("SetModel during a turn emitted %+v, want one bare EventMeta", meta)
	}
	close(h.release)
	if got := await(t, out, "the turn on test/a"); got.err != nil || got.res.StopReason != "end_turn" {
		t.Fatalf("the running turn = %+v, %v", got.res, got.err)
	}
	f.models["test/b"].push(answer("from b"))
	if _, err := s.Prompt(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if a, b := f.models["test/a"].callCount(), f.models["test/b"].callCount(); a != 1 || b != 1 {
		t.Fatalf("test/a got %d calls and test/b %d, want one each", a, b)
	}

	if err := s.SetModel(context.Background(), "other/c"); err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); snap.CurrentModel != "other/c" || EffortOption(snap) != nil || snap.Config != nil {
		t.Fatalf("a model with no efforts: current %q, config %+v", snap.CurrentModel, snap.Config)
	}
	err := s.SetModel(context.Background(), "nokey/d")
	if err == nil || !errors.Is(err, harness.ErrNoAPIKey) || !strings.Contains(err.Error(), "NATIVE_NOKEY_KEY") {
		t.Fatalf("SetModel to an unfunded model = %v", err)
	}
	if got := s.Snapshot().CurrentModel; got != "other/c" {
		t.Fatalf("a failed switch moved the model to %q", got)
	}
	if err := s.SetModel(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), `unknown model "nope"`) {
		t.Fatalf("SetModel to an unknown model = %v", err)
	}
}

func cat(parts ...[]fantasy.StreamPart) []fantasy.StreamPart {
	var all []fantasy.StreamPart
	for _, p := range parts {
		all = append(all, p...)
	}
	return all
}

// TestNativeSwitchesAnnounceTheirOptions: a model or effort change that took
// emits exactly one bare EventMeta — the native session's
// config_option_update, which is what makes the TUI re-read the snapshot —
// and the snapshot it points at already carries the new model's effort
// option, or none. A switch that failed, or was never supported, emits
// nothing.
func TestNativeSwitchesAnnounceTheirOptions(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Model: "other/c"})
	if opt := EffortOption(s.Snapshot()); opt != nil {
		t.Fatalf("other/c has no efforts, yet the snapshot offers %+v", opt)
	}
	oneMeta := func(t *testing.T, what string) {
		t.Helper()
		evs := drained(s)
		if len(evs) != 1 || evs[0].Type != EventMeta || evs[0].Text != "" || evs[0].Mode != "" {
			t.Fatalf("%s emitted %+v, want exactly one bare EventMeta", what, evs)
		}
	}
	none := func(t *testing.T, what string) {
		t.Helper()
		if evs := drained(s); len(evs) != 0 {
			t.Fatalf("%s emitted %+v, want nothing", what, evs)
		}
	}

	if err := s.SetModel(context.Background(), "test/a"); err != nil {
		t.Fatal(err)
	}
	oneMeta(t, "SetModel to a model with efforts")
	if opt := EffortOption(s.Snapshot()); opt == nil || opt.Current != "high" {
		t.Fatalf("after the switch to test/a the effort option is %+v, want low/high at high", opt)
	}

	if err := s.SetConfig(context.Background(), nativeEffortID, "low"); err != nil {
		t.Fatal(err)
	}
	oneMeta(t, "SetConfig(effort)")
	if opt := EffortOption(s.Snapshot()); opt == nil || opt.Current != "low" {
		t.Fatalf("after SetConfig the effort option is %+v, want low", opt)
	}

	if err := s.SetModel(context.Background(), "nokey/d"); err == nil {
		t.Fatal("a switch to an unfunded model succeeded")
	}
	none(t, "a failed SetModel")
	if err := s.SetConfig(context.Background(), nativeEffortID, "max"); err == nil {
		t.Fatal("an effort the model does not offer was taken")
	}
	none(t, "a refused SetConfig")
	if err := s.SetConfig(context.Background(), "fast", "true"); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	none(t, "an unsupported SetConfig")

	if err := s.SetModel(context.Background(), "other/c"); err != nil {
		t.Fatal(err)
	}
	oneMeta(t, "SetModel to a model with no efforts")
	if snap := s.Snapshot(); EffortOption(snap) != nil || snap.Config != nil {
		t.Fatalf("after the switch back to other/c the config is %+v, want none", snap.Config)
	}
}

// TestNativeSetConfig: the effort option's id sets the harness's effort, and
// the snapshot follows; an effort the model does not offer is refused naming
// the ones it does; any other id is unsupported.
func TestNativeSetConfig(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	if err := s.SetConfig(context.Background(), nativeEffortID, "low"); err != nil {
		t.Fatalf("SetConfig(effort, low): %v", err)
	}
	if _, effort := s.hs.Current(); effort != "low" {
		t.Fatalf("the harness's effort is %q, want low", effort)
	}
	if opt := EffortOption(s.Snapshot()); opt == nil || opt.Current != "low" {
		t.Fatalf("the effort option after SetConfig: %+v", opt)
	}
	err := s.SetConfig(context.Background(), nativeEffortID, "max")
	if err == nil || !strings.Contains(err.Error(), "low, high") {
		t.Fatalf("SetConfig(effort, max) = %v, want a refusal naming low, high", err)
	}
	if opt := EffortOption(s.Snapshot()); opt.Current != "low" {
		t.Fatalf("a refused effort changed the option to %q", opt.Current)
	}
	if err := s.SetConfig(context.Background(), nativeEffortID, ""); err != nil {
		t.Fatalf("SetConfig(effort, \"\") = %v; the model's default is always allowed", err)
	}
	if opt := EffortOption(s.Snapshot()); opt.Current != "high" {
		t.Fatalf("the default effort is %q, want high", opt.Current)
	}
	if err := s.SetConfig(context.Background(), "fast", "true"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("SetConfig(fast) = %v, want ErrUnsupported", err)
	}
	if err := s.SetModel(context.Background(), "other/c"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConfig(context.Background(), nativeEffortID, "low"); err == nil {
		t.Fatal("SetConfig(effort) on a model with no effort control succeeded")
	}
}

// TestNativeUnsupported: what the harness does not have says so before
// anything runs. Interject is not here since C12: it is supported, and is
// refused with ErrNotInTurn while the session is idle (TestNativeInterject).
func TestNativeUnsupported(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	for name, err := range map[string]error{
		"SetMode":          s.SetMode(context.Background(), "plan"),
		"AnswerPermission": s.AnswerPermission("p", "allow"),
		"AnswerQuestion":   s.AnswerQuestion("q", nil, true),
		"AnswerPlan":       s.AnswerPlan("p", true),
	} {
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s = %v, want ErrUnsupported", name, err)
		}
	}
	if evs := drained(s); len(evs) != 0 {
		t.Fatalf("unsupported calls emitted %+v", evs)
	}
}

// TestNativeTitle: the title is the first line of the first prompt, on one
// line, sanitized and capped, and later prompts leave it; /rename pins its own.
// No title ever produces an event (craze prompt --json prints none).
func TestNativeTitle(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	long := strings.Repeat("é", nativeTitleRuneCap+10)
	f.models["test/a"].push(answer("1"), answer("2"))
	if _, err := s.Prompt(context.Background(), "\x1b[31mfix\x1b[0m the  tests "+long+"\nsecond line"); err != nil {
		t.Fatal(err)
	}
	want := []rune("fix the tests " + long)[:nativeTitleRuneCap]
	if got := s.Snapshot().Title; got != string(want) {
		t.Fatalf("Title = %q, want %q", got, string(want))
	}
	if _, err := s.Prompt(context.Background(), "another"); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot().Title; got != string(want) {
		t.Fatalf("a later prompt changed the title to %q", got)
	}
	s.SetTitle("mine\x1b[2J")
	if got := s.Snapshot().Title; got != "mine" {
		t.Fatalf("SetTitle: %q", got)
	}
	if meta := ofType(drained(s), EventMeta); len(meta) != 0 {
		t.Fatalf("titles emitted %+v", meta)
	}
}

// TestNativeTitlePinnedBeforeTheFirstPrompt: a /rename before any prompt is
// not replaced by the first prompt's line.
func TestNativeTitlePinnedBeforeTheFirstPrompt(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	s.SetTitle("named")
	f.models["test/a"].push(answer("ok"))
	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot().Title; got != "named" {
		t.Fatalf("Title = %q, want the pinned one", got)
	}
}

// TestNativeWireErrorsArePhrasedWithoutTheKey runs the production model stack
// — the llm factory and its wrapper, Fantasy's OpenAI-compatible provider —
// against a loopback server whose errors echo the Authorization header, the
// realistic way a key reaches an error. The key (inline in providers.toml)
// must appear in no event, no EventError and no returned error, however deep
// in it one looks (plan 018 §7 criterion 8a), and each failure is phrased as
// its kind.
func TestNativeWireErrorsArePhrasedWithoutTheKey(t *testing.T) {
	echo := func(status int, prefix string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("retry-after-ms", "1") // a retried status must not wait Fantasy's 5 s
			jsonError(w, status, prefix+r.Header.Get("Authorization"))
		}
	}
	for _, tc := range []struct {
		name    string
		replies []http.HandlerFunc
		is      error
		want    string // a substring of the phrased error
		text    string // text streamed before the failure
	}{
		{name: "401", replies: []http.HandlerFunc{echo(401, "invalid credentials: ")}, is: harness.ErrAuth,
			want: `native: provider "wire" rejected the API key (HTTP 401)`},
		{name: "403", replies: []http.HandlerFunc{echo(403, "forbidden: ")}, is: harness.ErrAuth,
			want: `rejected the API key (HTTP 403)`},
		{name: "404", replies: []http.HandlerFunc{echo(404, "no model for ")}, is: harness.ErrModelNotFound,
			want: `does not serve model "wire/m" (HTTP 404)`},
		{name: "500 twice, retried once", replies: []http.HandlerFunc{echo(500, "overloaded: "), echo(500, "still overloaded: ")},
			want: `native: provider "wire" failed (HTTP 500): still overloaded: Bearer [redacted]`},
		{name: "context too large", replies: []http.HandlerFunc{func(w http.ResponseWriter, r *http.Request) {
			jsonError(w, 400, "This model's maximum context length is 1000 tokens. However, your messages resulted in 2000 tokens. "+r.Header.Get("Authorization"))
		}}, is: harness.ErrContextTooLarge, want: "context window"},
		{name: "stream error after text", replies: []http.HandlerFunc{func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", textChunk("partial"))
			fmt.Fprintf(w, "data: {\"error\":{\"message\":%q,\"type\":\"server_error\"}}\n\n", "upstream: "+r.Header.Get("Authorization"))
		}}, want: `native: provider "wire" failed: stream error: upstream: Bearer [redacted]`, text: "partial"},
		{name: "a raw HTML body", replies: []http.HandlerFunc{func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(400)
			fmt.Fprintf(w, "<html>\n<pre>Authorization: %s</pre>\n%s\n</html>\n", r.Header.Get("Authorization"), strings.Repeat("x", 2000))
		}}, want: `native: provider "wire" failed (HTTP 400)`},
		{name: "an empty step", replies: []http.HandlerFunc{sseReply(finishChunk("stop"))}, is: harness.ErrEmptyStep,
			want: "native: the model ended its answer without sending anything"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := wireSession(t, tc.replies...)
			res, err := s.Prompt(context.Background(), "hi")
			evs := drained(s)
			evErr := endings(t, evs, "")
			if err == nil || evErr != err || res != (Result{}) {
				t.Fatalf("Prompt = %+v, %v; EventError %v", res, err, evErr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not say %q", err, tc.want)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("error %q does not match %v", err, tc.is)
			}
			if strings.ContainsAny(err.Error(), "\n\r\x1b") || len(err.Error()) > 600 {
				t.Fatalf("error is not one bounded line: %q", err)
			}
			if got := joined(evs, EventText); got != tc.text {
				t.Fatalf("text %q, want %q", got, tc.text)
			}
			for _, v := range []any{err, evs} {
				if leaks := nativeLeaks(v, nativeCanary); len(leaks) > 0 {
					t.Fatalf("the key leaked at %v", leaks)
				}
			}
		})
	}
}

// wireSession is a started adapter on the production model stack, one model
// ("wire/m") whose provider's key is the canary, inline in providers.toml,
// and whose endpoint is a loopback server answering replies in order.
func wireSession(t *testing.T, replies ...http.HandlerFunc) Session {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		var next http.HandlerFunc
		if len(replies) > 0 {
			next, replies = replies[0], replies[1:]
		}
		mu.Unlock()
		if next == nil {
			jsonError(w, http.StatusTeapot, "the test queued no reply for this request")
			return
		}
		next(w, r)
	}))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	table := &modeltable.Table{
		DefaultModel: "wire/m",
		Providers: map[string]modeltable.Provider{
			"wire": {Driver: modeltable.DriverOpenAICompat, BaseURL: srv.URL + "/v1",
				EnvKeys: []string{"NATIVE_WIRE_KEY"}, APIKey: modeltable.Secret(nativeCanary)},
		},
		Models: map[string]modeltable.Model{"wire/m": {Provider: "wire", WireModel: "wire-model"}},
	}
	if err := modeltable.Save(filepath.Join(home, "native"), table); err != nil {
		t.Fatal(err)
	}
	// An empty ContentHome, as nativeFixture.session gives every other case:
	// left unset, Start reads the developer's own ~/.claude.
	s := NewNative(Options{Workspace: t.TempDir(), ContentHome: t.TempDir()}, func(o *harness.Options) {
		o.Getenv = func(string) string { return "" } // the inline key, whatever the real environment holds
	})
	closeAtCleanup(t, s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s
}

func sseReply(chunks ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func textChunk(text string) string {
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, text)
}

func finishChunk(reason string) string {
	return fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, reason)
}

func jsonError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":"invalid_request_error"}}`, message)
}

// nativeLeaks lists every place needle can be found from v: its formatted
// forms, and every string and byte slice reachable from it by reflection —
// unexported fields, wrapped causes, maps and slices included — since an
// error that prints clean but holds a key in a field leaks the moment a later
// layer reads the field. (The harness's tests have the same walker; test
// helpers do not cross packages.)
func nativeLeaks(v any, needle string) []string {
	var found []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if strings.Contains(fmt.Sprintf(verb, v), needle) {
			found = append(found, "formatted "+verb)
		}
	}
	w := &leakWalker{needle: []byte(needle), seen: map[leakVisit]bool{}}
	w.walk(reflect.ValueOf(v), "v")
	return append(found, w.found...)
}

type leakVisit struct {
	ptr uintptr
	typ reflect.Type
}

type leakWalker struct {
	needle []byte
	seen   map[leakVisit]bool
	found  []string
}

func (w *leakWalker) walk(v reflect.Value, path string) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		if strings.Contains(v.String(), string(w.needle)) {
			w.found = append(w.found, path)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if bytes.Contains(v.Bytes(), w.needle) {
				w.found = append(w.found, path)
			}
			return
		}
		for i := range v.Len() {
			w.walk(v.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.Array:
		for i := range v.Len() {
			w.walk(v.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		k := leakVisit{v.Pointer(), v.Type()}
		if w.seen[k] {
			return
		}
		w.seen[k] = true
		w.walk(v.Elem(), path)
	case reflect.Interface:
		w.walk(v.Elem(), path)
	case reflect.Struct:
		for i := range v.NumField() {
			w.walk(v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			w.walk(it.Key(), path+"{key}")
			w.walk(it.Value(), fmt.Sprintf("%s[%v]", path, it.Key()))
		}
	}
}

// TestNativeLeaksFindsAHiddenKey keeps nativeLeaks honest: a key held only in
// an unexported field of a wrapped cause, which no formatted form shows, must
// still be found.
func TestNativeLeaksFindsAHiddenKey(t *testing.T) {
	err := &nativeError{msg: "clean", cause: fmt.Errorf("clean: %w", &quietError{secret: []byte(nativeCanary)})}
	found := nativeLeaks(err, nativeCanary)
	if len(found) != 1 || !strings.HasSuffix(found[0], ".secret") {
		t.Fatalf("nativeLeaks = %v, want exactly the hidden field", found)
	}
}

type quietError struct{ secret []byte }

func (*quietError) Error() string { return "quiet" }

// TestNativeWorkspaceIsAbsolute: an empty workspace is the working directory
// and a relative one is made absolute, as the harness requires.
func TestNativeWorkspaceIsAbsolute(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{"": wd, "sub": filepath.Join(wd, "sub")} {
		got, err := nativeWorkspace(in)
		if err != nil || got != want {
			t.Fatalf("nativeWorkspace(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
