package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
)

// The ChatGPT plan through the harness (plan 033 §3.12, C14): token values
// the sign-in learns mid-turn are redacted from the running turn's tools and
// its children's (AddSecrets), the driver's final errors keep their codes
// through the wrapper, Fantasy and classify — a turn's and a summary's — with
// every token value scrubbed, and the usage latch stops the plan's requests
// from wakes and sub-agents. Every token is a dummy of at least eight bytes,
// sharing nothing with the marker; the API is a loopback fake.

const (
	planBearer  = "test-plan-bearer-0007"
	planRotated = "test-plan-rotated-0008"
	planOld     = "test-plan-old-refresh-0009"
)

var (
	errPlanSignedOut = errors.New("test: not signed in to ChatGPT")
	errPlanLimited   = errors.New("test: the ChatGPT plan's usage limit was reached")
)

// planAuth is a sign-in as the harness sees one (llm.Auth, with its
// sentinels and usage latch).
type planAuth struct {
	mu      sync.Mutex
	values  []string
	latched bool
}

func newPlanAuth(values ...string) *planAuth {
	return &planAuth{values: append([]string{planBearer}, values...)}
}

func (a *planAuth) Token(context.Context) (string, uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.latched {
		return "", 0, errPlanLimited
	}
	return planBearer, 1, nil
}

func (a *planAuth) Invalidate(context.Context, uint64) error { return nil }

func (a *planAuth) Values() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.values)
}

func (a *planAuth) Sentinels() []error { return []error{errPlanSignedOut, errPlanLimited} }

func (a *planAuth) LatchUsageLimit() { a.setLatch(true) }

func (a *planAuth) setLatch(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.latched = on
}

func (a *planAuth) isLatched() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latched
}

// planAPI is a fake Responses API: each request is answered by the next
// queued reply, and recorded.
type planAPI struct {
	srv     *httptest.Server
	mu      sync.Mutex
	replies []http.HandlerFunc
	seen    []*http.Request
}

func newPlanAPI(t *testing.T, replies ...http.HandlerFunc) *planAPI {
	t.Helper()
	a := &planAPI{replies: replies}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		a.mu.Lock()
		a.seen = append(a.seen, r.Clone(context.Background()))
		var next http.HandlerFunc
		if len(a.replies) > 0 {
			next, a.replies = a.replies[0], a.replies[1:]
		}
		a.mu.Unlock()
		if next == nil {
			http.Error(w, "no reply queued", http.StatusTeapot)
			return
		}
		next(w, r)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *planAPI) base() (string, error) { return a.srv.URL + "/v1", nil }

func (a *planAPI) requests() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.seen)
}

func (a *planAPI) queue(replies ...http.HandlerFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replies = append(a.replies, replies...)
}

// planText answers with one message of text, completed.
func planText(text string) http.HandlerFunc {
	msg := func(content []any) map[string]any {
		return map[string]any{"type": "message", "id": "m1", "role": "assistant", "content": content}
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		for _, ev := range []map[string]any{
			{"type": "response.output_item.added", "output_index": 0, "item": msg([]any{})},
			{"type": "response.output_text.delta", "output_index": 0, "item_id": "m1", "delta": text},
			{"type": "response.output_item.done", "output_index": 0, "item": msg([]any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}})},
			{"type": "response.completed", "response": map[string]any{"id": "r1", "status": "completed", "output": []any{},
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12}}},
		} {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
		}
	}
}

// planRefusal answers status with the standard error object.
func planRefusal(status int, code, param, message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "param": param, "type": "invalid_request_error", "message": message}})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
	}
}

// onThePlan is the fixture's options with test/a on the ChatGPT plan
// (onChatGPT), built by the real llm factory on auth against api; every other
// alias stays scripted.
func onThePlan(f *fixture, auth *planAuth, api *planAPI) Options {
	onChatGPT(f)
	o := f.options()
	o.Auth, o.AuthAPIBase = auth, api.base
	scripted := o.NewModel
	o.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
		if r.Driver == modeltable.DriverChatGPT {
			return llm.New(r, llm.WithSignIn(auth, api.base))
		}
		return scripted(r)
	}
	return o
}

// TestAddSecretsRedactsTheRunningTurn (A21, P19): a token value the sign-in
// learns while a turn runs is redacted from that turn's next tool output — the
// installed redactor widened at once, the documented exception to "one
// redactor per turn" — and from its transcript line, while the output the turn
// read before it learned the value keeps it (the control: nothing else
// redacts it). A value that cannot be a key is not learned.
func TestAddSecretsRedactsTheRunningTurn(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.put("token.txt", "the token is "+planRotated+"\n")
	readIt := input(t, map[string]any{"filePath": "token.txt"})
	rotateThenRead := func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		s.AddSecrets("short", "  "+planRotated+"\n") // as a token source hands it over mid-turn
		callStep(callParts("c2", "read", readIt))(ctx, yield)
	}
	f.models["test/a"].push(callStep(callParts("c1", "read", readIt)), rotateThenRead, answerWith("read twice"))
	var ev events
	runWith(t, s, "read it twice", ev.sink)

	results := of[ToolFinished](ev.list())
	if len(results) != 2 {
		t.Fatalf("%d tool results, want 2", len(results))
	}
	if !strings.Contains(results[0].Result.Content, planRotated) {
		t.Fatalf("control: the read before the rotation was redacted: %q", results[0].Result.Content)
	}
	if got := results[1].Result.Content; strings.Contains(got, planRotated) || !strings.Contains(got, redact.Marker) {
		t.Fatalf("the read after the rotation, in the same turn = %q", got)
	}
	reqs := f.models["test/a"].requests()
	if last := requestText(reqs[len(reqs)-1], false); strings.Count(last, planRotated) != 1 {
		t.Fatalf("the model was sent the value %d times after it was learned, want once (the earlier read's)", strings.Count(last, planRotated))
	}
	if lines := entries(transcript(t, s)); !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, redact.Marker) }) {
		t.Fatalf("no transcript line holds the marker:\n%s", strings.Join(lines, "\n"))
	}
	if got := s.Redact("x short y"); got != "x short y" {
		t.Fatalf("a value that cannot be a key was learned: %q", got)
	}
}

// TestAddSecretsReachesARunningSubagent (§3.12): a value the parent is taught
// while its sub-agent runs is pushed to the child, whose own tools redact it
// from their next output. The control is the same child with nothing taught,
// whose read shows the value.
func TestAddSecretsReachesARunningSubagent(t *testing.T) {
	for _, taught := range []bool{false, true} {
		t.Run(fmt.Sprintf("taught %v", taught), func(t *testing.T) {
			f := newRouted(t)
			s := f.open(f.options())
			f.put("token.txt", "the token is "+planRotated+"\n")
			a := f.routers["test/a"]
			w := newWorker()
			read := callParts("r1", "read", input(t, map[string]any{"filePath": "token.txt"}))
			a.route("parent", callStep(agentPart(t, "a1", task("look", "child reads"))), answerWith("done"))
			a.route("child reads", w.step(nil, cat(read, finish(fantasy.FinishReasonToolCalls))), answerWith("child done"))
			var ev events
			out := start(context.Background(), s, "parent", ev.sink)
			await(t, w.reached, "the child mid-step")
			if taught {
				s.AddSecrets(planRotated)
			}
			close(w.release)
			if got := await(t, out, "the parent's turn"); got.err != nil {
				t.Fatal(got.err)
			}
			var content string
			for _, e := range of[SubagentEvent](ev.list()) {
				if tf, ok := e.Event.(ToolFinished); ok {
					content = tf.Result.Content
				}
			}
			if content == "" {
				t.Fatal("the child read nothing")
			}
			if strings.Contains(content, planRotated) == taught {
				t.Fatalf("taught %v: the child's read = %q", taught, content)
			}
			settled(t, s)
		})
	}
}

// TestFinalErrorThroughTheHarness (C12's "C14 must", A21b): the plan's
// refusal of a turn's request, and of a summary's, each reach the caller
// through the wrapper, Fantasy and classify as a ProviderError that is Final,
// on the chatgpt driver, with the provider's code and param — the request
// sent once, never retried, the summarizer's attempts ended at once — and no
// token value, current or one the sign-in rotated from, anywhere in it. A
// usage limit among them latches the sign-in. The control is the turn between
// them, which completes.
func TestFinalErrorThroughTheHarness(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	auth := newPlanAuth(planOld)
	api := newPlanAPI(t, planRefusal(400, llm.UnsupportedCapabilityCode, "tools[0]", "refused bearer "+planBearer+" and "+planOld))
	s := f.open(onThePlan(f, auth, api))

	_, err := s.Run(context.Background(), "first", nil)
	var pe *ProviderError
	if !errors.As(err, &pe) || !pe.Final || pe.Driver != modeltable.DriverChatGPT || pe.Code != CodeUnsupportedCapability ||
		pe.Param != "tools[0]" || pe.StatusCode != 400 {
		t.Fatalf("Run = %T %+v", err, err)
	}
	for _, v := range []string{planBearer, planOld} {
		if l := leaks(err, v); len(l) > 0 {
			t.Fatalf("a token value survives at %v", l)
		}
	}
	if n := api.requests(); n != 1 {
		t.Fatalf("the refused request was sent %d times", n)
	}

	api.queue(planText("hello back"))
	if res, err := s.Run(context.Background(), "second", nil); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("control: the turn on the plan = %+v, %v", res, err)
	}

	api.queue(planRefusal(429, llm.UsageLimitCode, "", "limit reached for "+planOld), planText("never sent"))
	s.sleep = func(context.Context, time.Duration) {}
	_, err = s.Compact(context.Background(), "", "/compact", nil)
	if !errors.As(err, &pe) || !pe.Final || pe.Code != CodeUsageLimit || leaks(err, planOld) != nil {
		t.Fatalf("Compact = %T %+v", err, err)
	}
	if n := api.requests(); n != 3 {
		t.Fatalf("the summarizer sent %d requests after the usage limit, want 1 (3 in all)", n-2)
	}
	if !auth.isLatched() {
		t.Fatal("the usage limit did not latch the sign-in")
	}
	if summarizerFailureKind(err) != "fatal" {
		t.Fatalf("a final refusal is %q to the summarizer", summarizerFailureKind(err))
	}
}

// TestUsageLatchStopsWakesAndSubagents (P33): while the sign-in is latched,
// neither a wake nor a sub-agent on the plan sends a request — each fails at
// once with the sign-in's own sentinel, which the turn's error carries — and
// once the latch is lifted (the adapter's next person-started turn) the same
// wake sends. The control is that last wake, which reaches the server.
func TestUsageLatchStopsWakesAndSubagents(t *testing.T) {
	auth := newPlanAuth()
	api := newPlanAPI(t)
	f := newRouted(t)
	// plan/x is a second model, on the ChatGPT plan; test/a, the parent's,
	// stays scripted. The fixture's home is signed in.
	signInFixture(t, f.home)
	appendTo := func(name, text string) {
		path := filepath.Join(f.home, name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, text...), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	appendTo(modeltable.ProvidersFile, "\n[providers.chatgpt]\ndriver = \"chatgpt\"\n")
	appendTo(modeltable.ModelsFile, "\n[models.\"plan/x\"]\nprovider = \"chatgpt\"\nwire_model = \"gpt-x\"\n")
	f.table = f.load()

	pending := make(chan bool, 8)
	opts := f.options()
	routed := opts.NewModel
	opts.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
		if r.Driver == modeltable.DriverChatGPT {
			return llm.New(r, llm.WithSignIn(auth, api.base))
		}
		return routed(r)
	}
	opts.Auth, opts.AuthAPIBase, opts.Background = auth, api.base, true
	var s *Session
	opts.OnPending = func() { pending <- s.HasPending() }
	s = f.open(opts)
	a := f.routers["test/a"]

	// Two background children on test/a, each held until released.
	w1, w2 := newWorker(), newWorker()
	a.route("one", w1.step(openText("did one"), finishText()))
	a.route("two", w2.step(openText("did two"), finishText()))
	bgArgs := func(id, prompt string) []fantasy.StreamPart {
		args := task("job "+prompt, prompt)
		args["run_in_background"] = true
		return agentPart(t, id, args)
	}
	// A foreground sub-agent on the plan, which the latch stops.
	sub := task("on the plan", "plan child")
	sub["model"] = "plan/x"
	a.route("go", callStep(bgArgs("b1", "one"), bgArgs("b2", "two")), callStep(agentPart(t, "f1", sub)), answerWith("started"))
	auth.LatchUsageLimit()
	var ev events
	if res, err := s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("the parent's turn = %+v, %v", res, err)
	}
	if r := callResult(t, ev.list(), "t1.2.1"); !r.IsError || !strings.Contains(r.Text, "usage limit") {
		t.Fatalf("the sub-agent on the plan = %+v; want its failure, the latch's", r)
	}
	if n := api.requests(); n != 0 {
		t.Fatalf("the latched sub-agent sent %d requests", n)
	}

	// The session's own model is now the plan's: a wake goes there.
	if err := s.SetModel("plan/x"); err != nil {
		t.Fatal(err)
	}
	close(w1.release)
	if !await(t, pending, "the first child's result pending") {
		t.Fatal("nothing pending")
	}
	if _, err := s.Wake(context.Background(), nil); !errors.Is(err, errPlanLimited) {
		t.Fatalf("a latched wake = %v; want the sign-in's sentinel", err)
	}
	if n := api.requests(); n != 0 {
		t.Fatalf("the latched wake sent %d requests", n)
	}

	auth.setLatch(false) // the adapter's next person-started turn
	api.queue(planText("woke"))
	close(w2.release)
	if !await(t, pending, "the second child's result pending") {
		t.Fatal("nothing pending")
	}
	if res, err := s.Wake(context.Background(), nil); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("control: the wake after the latch lifted = %+v, %v", res, err)
	}
	if n := api.requests(); n != 1 {
		t.Fatalf("control: the wake sent %d requests, want 1", n)
	}
	settled(t, s)
}
