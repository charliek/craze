package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// canary is the only key any test here holds: obviously not a secret, and
// every test that could leak a key looks for it. canaryOther keys the second
// provider.
const (
	canary      = "sk-canary-not-a-secret"
	canaryOther = "sk-canary-not-a-secret-other"
)

// The test model table: two models on one provider with different effort
// lists, a model with no effort control on a second provider, and a model
// whose provider has no key.
const (
	providersTOML = `version = 1

[providers.test]
driver = "openai-compat"
base_url = "%[1]s"
env_keys = ["TEST_API_KEY"]

[providers.other]
driver = "openai-compat"
base_url = "%[1]s"
env_keys = ["OTHER_API_KEY"]

[providers.nokey]
driver = "openai-compat"
base_url = "%[1]s"
env_keys = ["NOKEY_API_KEY"]
`
	modelsTOML = `version = 1
default_model = "test/a"

[models."test/a"]
provider = "test"
wire_model = "wire-a"
name = "Model A"
max_output_tokens = 4096
efforts = ["low", "high"]
default_effort = "high"

[models."test/b"]
provider = "test"
wire_model = "wire-b"
efforts = ["medium", "high"]
default_effort = "medium"

[models."other/c"]
provider = "other"
wire_model = "wire-c"

[models."nokey/d"]
provider = "nokey"
wire_model = "wire-d"
`
)

// testEnv is the environment the fixture's Getenv sees: two keys, and
// nothing for NOKEY_API_KEY.
var testEnv = map[string]string{"TEST_API_KEY": canary, "OTHER_API_KEY": canaryOther}

// testNow is the transcript clock: fixed, so nothing in a test depends on
// the time.
func testNow() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }

// fixture is a harness directory with the test model table in it, and a
// scripted model per alias for Options.NewModel.
type fixture struct {
	t         *testing.T
	home      string // <craze dir>/native: the table's files, and sessions/
	workspace string
	table     *modeltable.Table
	models    map[string]*scripted // by alias

	mu    sync.Mutex
	built []string // the aliases NewModel built, in order
}

// newFixture writes the table with every provider aimed at baseURL (unused
// by scripted models) and loads it the way the adapter does.
func newFixture(t *testing.T, baseURL string) *fixture {
	t.Helper()
	home := filepath.Join(t.TempDir(), "native")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string, perm os.FileMode) {
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), perm); err != nil {
			t.Fatal(err)
		}
	}
	write(modeltable.ProvidersFile, fmt.Sprintf(providersTOML, baseURL), 0o600)
	write(modeltable.ModelsFile, modelsTOML, 0o644)
	f := &fixture{t: t, home: home, workspace: filepath.Join(t.TempDir(), "project"), models: map[string]*scripted{}}
	if err := os.MkdirAll(f.workspace, 0o755); err != nil { // the tools work in it
		t.Fatal(err)
	}
	f.table = f.load()
	for alias, m := range f.table.Models {
		f.models[alias] = &scripted{provider: m.Provider, wire: m.WireModel}
	}
	return f
}

// load reads the table from the fixture's files: a fresh Table each call.
func (f *fixture) load() *modeltable.Table {
	f.t.Helper()
	table, err := modeltable.Load(f.home)
	if err != nil {
		f.t.Fatalf("loading the test model table: %v", err)
	}
	return table
}

func (f *fixture) getenv(name string) string { return testEnv[name] }

// options are Open's options with scripted models.
func (f *fixture) options() Options {
	return Options{
		Home:      f.home,
		Workspace: f.workspace,
		Table:     f.table,
		Getenv:    f.getenv,
		Now:       testNow,
		Version:   "v0.0.0-test",
		NewModel: func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
			f.mu.Lock()
			f.built = append(f.built, r.Alias)
			f.mu.Unlock()
			return f.models[r.Alias], nil
		},
	}
}

// open opens a session, closed when the test ends.
func (f *fixture) open(opts Options) *Session {
	f.t.Helper()
	s, err := Open(opts)
	if err != nil {
		f.t.Fatalf("Open: %v", err)
	}
	f.t.Cleanup(func() { _ = s.Close() })
	return s
}

// step is one scripted model response: it yields stream parts as the
// provider would, and sees the turn's context.
type step func(ctx context.Context, yield func(fantasy.StreamPart) bool)

// scripted is a fantasy.LanguageModel that answers each Stream call with the
// next queued step and records the call. With no step queued, Stream fails.
type scripted struct {
	provider, wire string

	mu    sync.Mutex
	steps []step
	calls []fantasy.Call
}

// push queues steps, answered in order.
func (m *scripted) push(steps ...step) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, steps...)
}

// requests is every call the model has been sent.
func (m *scripted) requests() []fantasy.Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]fantasy.Call(nil), m.calls...)
}

func (m *scripted) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
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

func (m *scripted) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("scripted: Generate is not used")
}

func (m *scripted) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("scripted: GenerateObject is not used")
}

func (m *scripted) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("scripted: StreamObject is not used")
}

func (m *scripted) Provider() string { return m.provider }
func (m *scripted) Model() string    { return m.wire }

// reply is a step that yields parts and ends.
func reply(parts ...[]fantasy.StreamPart) step {
	all := cat(parts...)
	return func(_ context.Context, yield func(fantasy.StreamPart) bool) {
		for _, p := range all {
			if !yield(p) {
				return
			}
		}
	}
}

// answerWith is the common one-step reply: text, then a clean finish.
func answerWith(text string) step {
	return reply(textParts(text), finish(fantasy.FinishReasonStop))
}

func cat(parts ...[]fantasy.StreamPart) []fantasy.StreamPart {
	var all []fantasy.StreamPart
	for _, p := range parts {
		all = append(all, p...)
	}
	return all
}

// textParts streams one text block, a delta per chunk, as the
// OpenAI-compatible provider does.
func textParts(chunks ...string) []fantasy.StreamPart {
	parts := []fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextStart, ID: "0"}}
	for _, c := range chunks {
		parts = append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: c})
	}
	return append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "0"})
}

// openText is textParts without the end: text still streaming.
func openText(chunks ...string) []fantasy.StreamPart {
	parts := textParts(chunks...)
	return parts[:len(parts)-1]
}

// finishText ends an openText block and the step: what a held step yields
// once released.
func finishText() []fantasy.StreamPart {
	return cat([]fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextEnd, ID: "0"}}, finish(fantasy.FinishReasonStop))
}

// reasoningParts streams one reasoning block.
func reasoningParts(chunks ...string) []fantasy.StreamPart {
	parts := []fantasy.StreamPart{{Type: fantasy.StreamPartTypeReasoningStart, ID: "r"}}
	for _, c := range chunks {
		parts = append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: "r", Delta: c})
	}
	return append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: "r"})
}

// stepUsage is every scripted finish's usage.
var stepUsage = fantasy.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CacheReadTokens: 4}

func finish(reason fantasy.FinishReason) []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: reason, Usage: stepUsage}}
}

func errorPart(err error) []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: err}}
}

// gate is a sync point inside a scripted step, so a test can act — cancel,
// switch model, close — at a known moment of a turn.
type gate struct {
	reached chan struct{} // closed once the step has yielded its first parts
	release chan struct{} // closed by the test to let the step go on
}

func newGate() *gate { return &gate{reached: make(chan struct{}), release: make(chan struct{})} }

// hold is a step that yields before, signals reached, and waits. A cancel
// ends it with the context's error part, as a real provider's stream ends
// when its request is cancelled; a release yields after.
func (g *gate) hold(before, after []fantasy.StreamPart) step {
	return func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		for _, p := range before {
			if !yield(p) {
				return
			}
		}
		close(g.reached)
		select {
		case <-ctx.Done():
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
			return
		case <-g.release:
		}
		for _, p := range after {
			if !yield(p) {
				return
			}
		}
	}
}

// events records what a sink was sent.
type events struct {
	mu  sync.Mutex
	all []Event
}

func (e *events) sink(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.all = append(e.all, ev)
}

func (e *events) list() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Event(nil), e.all...)
}

// outcome is what a Run returned.
type outcome struct {
	res Result
	err error
}

// sendSteer is Steer with the session's own token, which is how the adapter calls
// it: the token is read and used with nothing happening in between. A test
// that needs the two halves apart takes the token itself.
func sendSteer(s *Session, text string) error { return s.Steer(s.SteerToken(), text) }

// empty reports whether a Result carries nothing at all, which is what a
// failed turn returns — no stop reason, no usage and no unanswered steer.
// Result holds a slice since C12, so it cannot be compared with ==.
func empty(res Result) bool { return only(res, "") }

// only reports whether res says stop and nothing else: no usage, and no
// unanswered steer. It is what a cancelled turn returns.
func only(res Result, stop string) bool {
	return res.StopReason == stop && res.Usage == (Usage{}) && len(res.Unanswered) == 0
}

// start runs a turn on its own goroutine.
func start(ctx context.Context, s *Session, text string, sink func(Event)) <-chan outcome {
	out := make(chan outcome, 1)
	go func() {
		res, err := s.Run(ctx, text, sink)
		out <- outcome{res, err}
	}()
	return out
}

// waitTimeout bounds every wait on another goroutine. It is a bound, not a
// synchronisation: a correct run never comes near it.
const waitTimeout = 10 * time.Second

// await receives from ch, failing the test, named by what, if nothing
// arrives in time.
func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitTimeout):
		t.Fatalf("timed out after %v waiting for %s", waitTimeout, what)
		panic("unreachable")
	}
}

// run runs one turn on the test's goroutine, failing the test on an error.
func run(t *testing.T, s *Session, text string) Result {
	t.Helper()
	res, err := s.Run(context.Background(), text, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", text, err)
	}
	return res
}

// transcript reads the session's file back.
func transcript(t *testing.T, s *Session) *store.Transcript {
	t.Helper()
	tr, err := store.Load(s.store.Path())
	if err != nil {
		t.Fatalf("loading the transcript: %v", err)
	}
	return tr
}

// noTranscript fails the test if the session's file exists.
func noTranscript(t *testing.T, s *Session) {
	t.Helper()
	if _, err := os.Stat(s.store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the transcript exists (stat: %v); nothing should have been written", err)
	}
}

// entries summarizes a transcript's entries, one line each:
//
//	user test/a high: prompt
//	assistant test/a high end_turn: (thinking: …) answer
//	model_change test/b
//	effort_change medium
//	mode_change plan
func entries(tr *store.Transcript) []string {
	var out []string
	for _, e := range tr.Entries {
		switch e.Type {
		case store.TypeModelChange:
			out = append(out, "model_change "+e.Model.Alias)
		case store.TypeEffortChange:
			out = append(out, "effort_change "+e.Effort)
		case store.TypeModeChange:
			out = append(out, "mode_change "+e.Mode)
		case store.TypeMessage:
			head := []string{string(e.Message.Role), e.Model.Alias}
			for _, extra := range []string{e.Effort, e.StopReason} {
				if extra != "" {
					head = append(head, extra)
				}
			}
			if e.Interrupted {
				head = append(head, "interrupted")
			}
			out = append(out, strings.Join(head, " ")+": "+messageText(e.Message))
		default:
			out = append(out, e.Type)
		}
	}
	return out
}

// messageText is a message's parts in order, reasoning as "(thinking: …)",
// a tool call as "[call <id> <tool> <input>]", and a result as
// "[result <id>: <text>]" or, for an error result, "[error <id>: <text>]".
func messageText(m fantasy.Message) string {
	var parts []string
	for _, p := range m.Content {
		if r, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](p); ok {
			parts = append(parts, "(thinking: "+r.Text+")")
		} else if t, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
			parts = append(parts, t.Text)
		} else if c, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok {
			parts = append(parts, fmt.Sprintf("[call %s %s %s]", c.ToolCallID, c.ToolName, c.Input))
		} else if r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p); ok {
			text, isErr := outputText(r.Output)
			kind := "result"
			if isErr {
				kind = "error"
			}
			parts = append(parts, fmt.Sprintf("[%s %s: %s]", kind, r.ToolCallID, text))
		} else {
			parts = append(parts, "<"+string(p.GetType())+">")
		}
	}
	return strings.Join(parts, " ")
}

// plain is evs with what differs from run to run made fixed: a StepDone's
// time to first token is zeroed and its entry ids, which are random, each
// become "id"; a ToolCalled's and a ToolFinished's times are zeroed.
func plain(evs []Event) []Event {
	out := make([]Event, len(evs))
	for i, ev := range evs {
		switch e := ev.(type) {
		case StepDone:
			e.TimeToFirstToken = 0
			if e.Entries != nil {
				e.Entries = slices.Repeat([]string{"id"}, len(e.Entries))
			}
			ev = e
		case ToolCalled:
			e.At = time.Time{}
			ev = e
		case ToolFinished:
			e.At, e.Duration = time.Time{}, 0
			ev = e
		}
		out[i] = ev
	}
	return out
}

// done is the StepDone plain makes of a test/a step: finish is what the
// step finished with, stop its stop reason, and entries how many entries
// it wrote (0: none, Saved false).
func done(step int, finish fantasy.FinishReason, stop string, entries int) StepDone {
	d := StepDone{
		Step: step, Provider: "test", Model: "test/a", WireModel: "wire-a",
		Finish: string(finish), FinishRaw: string(finish), StopReason: stop,
		Usage: Usage{Input: 10, Output: 5, CacheRead: 4},
	}
	if entries > 0 {
		d.Saved, d.Entries = true, slices.Repeat([]string{"id"}, entries)
	}
	return d
}

// callParts streams one tool call the way the OpenAI-compatible provider
// does: the input's start, its arguments in one delta, its end, and the
// complete call.
func callParts(id, name, input string) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: name},
		{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: input},
		{Type: fantasy.StreamPartTypeToolInputEnd, ID: id},
		{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: name, ToolCallInput: input},
	}
}

// bareCall is a tool call with no input parts before it: the call alone, as
// a provider that does not stream arguments sends it.
func bareCall(id, name, input string) []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: name, ToolCallInput: input}}
}

// callStep is a step that calls tools and finishes "tool-calls".
func callStep(calls ...[]fantasy.StreamPart) step {
	return reply(cat(calls...), finish(fantasy.FinishReasonToolCalls))
}

// input marshals a tool call's arguments.
func input(t *testing.T, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// of returns the events of type T, in order.
func of[T Event](evs []Event) []T {
	var out []T
	for _, ev := range evs {
		if e, ok := ev.(T); ok {
			out = append(out, e)
		}
	}
	return out
}

// requestText is everything in a request a model reads, as one string: the
// system prompt, the tools it is offered, and every message — text,
// thinking, tool-call arguments and tool results alike.
//
// withCallArgs excludes an assistant message's tool-call arguments when it
// is false: those are the model's own output, which Fantasy replays inside
// the turn exactly as the model sent it (see redactCalls), so a test that
// plants a key in a call's arguments leaves them out. Everything craze put
// in the request is in it either way.
func requestText(c fantasy.Call, withCallArgs bool) string {
	var b strings.Builder
	for _, tl := range c.Tools {
		fmt.Fprintf(&b, "%+v\n", tl)
	}
	for _, m := range c.Prompt {
		for _, p := range m.Content {
			if cp, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok {
				b.WriteString(cp.ToolName + " " + cp.ToolCallID + "\n")
				if withCallArgs {
					b.WriteString(cp.Input + "\n")
				}
				continue
			}
			b.WriteString(messageText(fantasy.Message{Role: m.Role, Content: []fantasy.MessagePart{p}}) + "\n")
		}
	}
	return b.String()
}

// promptOf summarizes a request's messages, role and text, one line each.
func promptOf(call fantasy.Call) []string {
	var out []string
	for _, m := range call.Prompt {
		out = append(out, string(m.Role)+": "+messageText(m))
	}
	return out
}

func equal[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got %#v\nwant %#v", what, got, want)
	}
}

// leaks lists every place needle can be found from v: its formatted forms,
// and every string and byte slice reachable from it by reflection,
// unexported fields, causes, maps and slices included — an error that
// prints clean but still holds a key in a field leaks the moment a later
// layer reads the field. (package llm's test of the same name, which this
// package cannot import.)
func leaks(v any, needle string) []string {
	var found []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if strings.Contains(fmt.Sprintf(verb, v), needle) {
			found = append(found, "formatted "+verb)
		}
	}
	w := &walker{needle: []byte(needle), seen: map[visit]bool{}}
	w.walk(reflect.ValueOf(v), "v")
	return append(found, w.found...)
}

type visit struct {
	ptr uintptr
	typ reflect.Type
}

type walker struct {
	needle []byte
	seen   map[visit]bool
	found  []string
}

func (w *walker) walk(v reflect.Value, path string) {
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
		k := visit{v.Pointer(), v.Type()}
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

// quietError holds a key in an unexported field that its text never shows.
type quietError struct{ secret []byte }

func (quietError) Error() string { return "quiet" }

// TestLeaksFindsAHiddenKey keeps leaks honest: a key in an unexported field
// of a wrapped cause, which no formatted form shows, must still be found.
func TestLeaksFindsAHiddenKey(t *testing.T) {
	err := fmt.Errorf("clean: %w", &ProviderError{Message: "clean", kind: quietError{secret: []byte(canary)}})
	found := leaks(err, canary)
	if len(found) != 1 || !strings.HasSuffix(found[0], ".kind.secret") {
		t.Fatalf("leaks = %v, want exactly the hidden field …kind.secret", found)
	}
}
