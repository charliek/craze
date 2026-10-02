package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm/responsesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The ChatGPT plan's driver live in the factory (plan 033 §3.12, C14): New
// builds it on a sign-in, its scrubber hides the sign-in's token values —
// current and retired — and keeps a FinalError and the sign-in's sentinels
// through every rebuild, and its usage limit latches the sign-in. The tokens
// here are dummies of at least eight bytes, none overlapping the redaction
// marker; the server is a loopback fake, never OpenAI.

const (
	planToken   = "test-plan-access-0001"
	planRetired = "test-plan-retired-0002"
	planOther   = "test-plan-unrelated-0003"
)

var (
	errTestSignedOut    = errors.New("test: not signed in to ChatGPT")
	errTestUsageLimited = errors.New("test: the ChatGPT plan's usage limit was reached")
)

// signInAuth is a sign-in's token source as the driver sees one: a token, the
// values it holds (current and retired), its sentinels, and a usage latch
// that, once set, fails Token at once, as chatgptauth's does.
type signInAuth struct {
	mu      sync.Mutex
	token   string
	values  []string
	latched bool
	latches int
}

func newSignInAuth(values ...string) *signInAuth {
	return &signInAuth{token: planToken, values: append([]string{planToken}, values...)}
}

func (a *signInAuth) Token(context.Context) (string, uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.latched {
		return "", 0, fmt.Errorf("%w; no new requests until the next turn you start", errTestUsageLimited)
	}
	return a.token, 1, nil
}

func (a *signInAuth) Invalidate(context.Context, uint64) error { return nil }

func (a *signInAuth) Values() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.values)
}

func (a *signInAuth) setValues(v ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.values = v
}

func (a *signInAuth) Sentinels() []error { return []error{errTestSignedOut, errTestUsageLimited} }

func (a *signInAuth) LatchUsageLimit() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.latched, a.latches = true, a.latches+1
}

func (a *signInAuth) latchCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latches
}

// at is an apiBase answering the fake server's base.
func at(s *rxServer) func() (string, error) {
	return func() (string, error) { return s.srv.URL + "/v1", nil }
}

// streamErr runs one step on lm and returns the error it ended with, and the
// text it streamed before it.
func streamErr(t *testing.T, lm fantasy.LanguageModel) (string, error) {
	t.Helper()
	stream, err := lm.Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}})
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeError:
			return text.String(), part.Error
		}
	}
	return text.String(), nil
}

// TestNewBuildsTheChatGPTDriver (C14, A16): New builds the plan's driver on a
// sign-in — the Responses model, wrapped — and its request goes to the API
// base the sign-in names, with the sign-in's bearer. The controls: with no
// sign-in it is refused, a base that cannot be had fails the build with its
// own error, and a base that is neither OpenAI's nor a loopback one is
// refused, so the bearer has nowhere else to go (P37).
func TestNewBuildsTheChatGPTDriver(t *testing.T) {
	srv := newRxServer(t, rxEvents(rxAdded(0, rxMessage("m1", "")), rxText(0, "m1", "hi"), rxDone(0, rxMessage("m1", "hi")), rxCompleted(5, 0, 1, 0)))
	lm, err := New(chatgpt(), WithSignIn(newSignInAuth(), at(srv)))
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := lm.(*model); !ok {
		t.Fatalf("New = %T, want the wrapper", lm)
	} else if _, ok := w.inner.(*responsesModel); !ok {
		t.Fatalf("the wrapped model is %T, want the Responses model", w.inner)
	}
	resp, err := lm.Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}})
	if err != nil || resp.Content.Text() != "hi" {
		t.Fatalf("Generate = %+v, %v", resp, err)
	}
	reqs := srv.requests()
	if len(reqs) != 1 || reqs[0].path != "/v1/responses" || reqs[0].header.Get("Authorization") != "Bearer "+planToken {
		t.Fatalf("requests = %+v", reqs)
	}

	if _, err := New(chatgpt()); err == nil || !strings.Contains(err.Error(), "signing in") {
		t.Fatalf("New with no sign-in = %v, want a refusal", err)
	}
	broken := errors.New("test: the override is not a loopback URL")
	if _, err := New(chatgpt(), WithSignIn(newSignInAuth(), func() (string, error) { return "", broken })); !errors.Is(err, broken) {
		t.Fatalf("New with a base that cannot be had = %v", err)
	}
	if _, err := New(chatgpt(), WithSignIn(newSignInAuth(), func() (string, error) { return "https://api.example.com/v1", nil })); !errors.Is(err, responsesapi.ErrBaseURL) {
		t.Fatalf("New with another host's base = %v, want ErrBaseURL", err)
	}
	// No apiBase is OpenAI's own: built, never sent here.
	if _, err := New(chatgpt(), WithSignIn(newSignInAuth(), nil)); err != nil {
		t.Fatalf("New on OpenAI's base = %v", err)
	}
}

// TestChatGPTEffortsMatchTheTable: the efforts EffortOptions sends on the
// plan's driver are the ones the table keeps of the account's list.
func TestChatGPTEffortsMatchTheTable(t *testing.T) {
	var ours []string
	for _, e := range openAIEfforts {
		ours = append(ours, string(e))
	}
	if !slices.Equal(ours, modeltable.ChatGPTEfforts()) {
		t.Fatalf("llm %q, modeltable %q", ours, modeltable.ChatGPTEfforts())
	}
}

// TestScrubberHidesTheSignInsValues (P19, P35, A21b): the scrubber hides every
// value its Auth holds — the current token and one it retired — in every
// spelling, and a value the Auth does not hold is text like any other. Its set
// follows the Auth's: once the source retires a value for good (an hour after
// its expiry or rotation), the scrubber's cache lets its pattern go too, so a
// long-lived host's set stays as small as the source's.
func TestScrubberHidesTheSignInsValues(t *testing.T) {
	auth := newSignInAuth(planRetired)
	s := newScrubber("", auth)
	got := s.text("now " + planToken + ", then " + escapedEveryOther(planRetired) + ", never " + planOther)
	if strings.Contains(got, planToken) || len(decodedLeaks(got, planRetired)) > 0 || !strings.Contains(got, redacted) {
		t.Fatalf("scrubbed = %q", got)
	}
	if !strings.Contains(got, planOther) {
		t.Fatalf("control: a value the Auth does not hold was scrubbed: %q", got)
	}
	if n := len(s.patterns); n != 2 {
		t.Fatalf("the cache holds %d patterns, want 2", n)
	}
	if len(leaks(s, planToken)) > 0 || len(leaks(s, planRetired)) > 0 {
		t.Fatal("the scrubber holds a token value whole")
	}

	auth.setValues(planToken) // the retired value has left the source's set
	if got := s.text("then " + planRetired); got != "then "+planRetired {
		t.Fatalf("a value the source dropped is still scrubbed: %q", got)
	}
	if n := len(s.patterns); n != 1 {
		t.Fatalf("the cache holds %d patterns after the retirement, want 1", n)
	}
}

// TestScrubberKeepsTheFinalError (C12's "C14 must"): a FinalError through the
// scrubber is a FinalError — its status, code, type, param and context flag
// kept — with its message scrubbed of the sign-in's values. The control: a
// param that is itself one of those values is dropped, not kept.
func TestScrubberKeepsTheFinalError(t *testing.T) {
	fe := &FinalError{StatusCode: 400, Code: UnsupportedCapabilityCode, Type: "invalid_request_error",
		Param: "input[0].content[1].image_url", Message: "refused bearer " + planRetired, contextTooLarge: true}
	out := newScrubber("", newSignInAuth(planRetired)).err(fmt.Errorf("wrapped: %w", fe))
	var got *FinalError
	if !errors.As(out, &got) {
		t.Fatalf("scrubbed = %T %v, want a *FinalError", out, out)
	}
	if got.StatusCode != 400 || got.Code != UnsupportedCapabilityCode || got.Type != "invalid_request_error" ||
		got.Param != fe.Param || !got.IsContextTooLarge() || !strings.Contains(got.Message, redacted) {
		t.Fatalf("scrubbed = %+v", got)
	}
	if l := leaks(out, planRetired); len(l) > 0 {
		t.Fatalf("the retired value survives at %v", l)
	}

	const paramToken = "input_token_param_0004"
	fe.Param = paramToken
	out = newScrubber("", newSignInAuth(paramToken)).err(fe)
	if !errors.As(out, &got) || got.Param != "" || len(leaks(out, paramToken)) > 0 {
		t.Fatalf("a param that is a token value = %+v", got)
	}
}

// TestScrubberKeepsTheSignInsSentinels (§3.12): an error carrying one of the
// sign-in's sentinels is rebuilt still carrying it — errors.Is answers, and
// AuthSentinel names it — with its text scrubbed. The controls: an Auth that
// lists no sentinels keeps none of them, and a context error is kept as the
// context's, never as the sign-in's.
func TestScrubberKeepsTheSignInsSentinels(t *testing.T) {
	err := fmt.Errorf("responsesapi: no token for the request: %w", fmt.Errorf("%w: the token %s was refused", errTestSignedOut, planToken))
	out := newScrubber("", newSignInAuth()).err(err)
	if !errors.Is(out, errTestSignedOut) || AuthSentinel(out) != errTestSignedOut || strings.Contains(out.Error(), planToken) {
		t.Fatalf("scrubbed = %v (sentinel %v)", out, AuthSentinel(out))
	}
	out = newScrubber("", newStaticAuth(planToken)).err(err)
	if errors.Is(out, errTestSignedOut) || AuthSentinel(out) != nil {
		t.Fatalf("control: an Auth with no sentinels kept one: %v", out)
	}
	out = newScrubber("", newSignInAuth()).err(fmt.Errorf("stopped: %w", context.Canceled))
	if !errors.Is(out, context.Canceled) || AuthSentinel(out) != nil {
		t.Fatalf("a cancel = %v (sentinel %v)", out, AuthSentinel(out))
	}
}

// TestFinalErrorStaysFinalAfterOutput (C12's "C14 must"): a FinalError that
// arrives after text reached the screen is still a FinalError — never a
// retryable error, and its code still there for the latch and the harness.
// The control: any other provider error after output is a MidStreamError.
func TestFinalErrorStaysFinalAfterOutput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		final bool
	}{
		{"a FinalError", &FinalError{Code: UsageLimitCode, Message: "limit"}, true},
		{"a provider error (control)", &fantasy.ProviderError{StatusCode: 500, Message: "boom"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := []fantasy.StreamPart{
				{Type: fantasy.StreamPartTypeTextStart, ID: "0"},
				{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: "partial"},
				{Type: fantasy.StreamPartTypeError, Error: tc.err},
			}
			text, err := streamErr(t, wrap(stubModel{parts: parts}, newScrubber("", newSignInAuth())))
			var fe *FinalError
			var mse *MidStreamError
			if text != "partial" || errors.As(err, &fe) != tc.final || errors.As(err, &mse) == tc.final {
				t.Fatalf("after %q the error is %T %v", text, err, err)
			}
			if tc.final && fe.Code != UsageLimitCode {
				t.Fatalf("the code was lost: %+v", fe)
			}
		})
	}
}

// TestUsageLimitLatchesTheSignIn (P33): the plan's usage limit — an HTTP 429
// before the stream, or response.failed inside it after text — is a final
// error that latches the sign-in before it is returned, so the next request
// fails at once, its sentinel kept, and never reaches the server. The control:
// any other 429 is final too, and latches nothing.
func TestUsageLimitLatchesTheSignIn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply func() string // "" is the status reply
		code  string
	}{
		{name: "an HTTP 429", code: UsageLimitCode},
		{name: "inside the stream", code: UsageLimitCode, reply: func() string { return "stream" }},
		{name: "another 429 (control)", code: "rate_limit_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := rxJSON(429, `{"error":{"code":"`+tc.code+`","message":"slow down"}}`)
			if tc.reply != nil {
				first = rxEvents(rxAdded(0, rxMessage("m1", "")), rxText(0, "m1", "partial"),
					rxFailed(tc.code, "the plan's limit", rxUsage(5, 0, 1, 0)))
			}
			srv := newRxServer(t, first, rxEvents(rxAdded(0, rxMessage("m1", "")), rxText(0, "m1", "hi"), rxDone(0, rxMessage("m1", "hi")), rxCompleted(5, 0, 1, 0)))
			auth := newSignInAuth()
			lm, err := New(chatgpt(), WithSignIn(auth, at(srv)))
			if err != nil {
				t.Fatal(err)
			}
			_, err = streamErr(t, lm)
			var fe *FinalError
			if !errors.As(err, &fe) || fe.Code != tc.code {
				t.Fatalf("the first request's error = %T %v", err, err)
			}
			limit := tc.code == UsageLimitCode
			if got := auth.latchCount(); got != map[bool]int{true: 1, false: 0}[limit] {
				t.Fatalf("latched %d times", got)
			}
			_, err = streamErr(t, lm)
			if limit != errors.Is(err, errTestUsageLimited) {
				t.Fatalf("the second request's error = %v", err)
			}
			if want := map[bool]int{true: 1, false: 2}[limit]; len(srv.requests()) != want {
				t.Fatalf("the server saw %d requests, want %d", len(srv.requests()), want)
			}
		})
	}
}

// TestAProviderErrorBodyIsScrubbedOfARotatedValue (A21b): a provider error
// whose body echoes a value the sign-in rotated away from, through the driver,
// the wrapper and Fantasy's agent, reaches the caller as a FinalError with its
// code and param — sent once, never retried — and no spelling of the value is
// reachable from it. The control is the unrelated text beside it, kept.
func TestAProviderErrorBodyIsScrubbedOfARotatedValue(t *testing.T) {
	body := `{"error":{"code":"` + UnsupportedCapabilityCode + `","type":"invalid_request_error","param":"tools[0]",` +
		`"message":"refused ` + escapedEveryOther(planRetired) + ` and ` + planOther + `"}}`
	srv := newRxServer(t, rxJSON(400, body))
	lm, err := New(chatgpt(), WithSignIn(newSignInAuth(planRetired), at(srv)))
	if err != nil {
		t.Fatal(err)
	}
	agent := fantasy.NewAgent(lm, fantasy.WithMaxRetries(3))
	_, err = agent.Generate(context.Background(), fantasy.AgentCall{Prompt: "go"})
	var fe *FinalError
	if !errors.As(err, &fe) || fe.Code != UnsupportedCapabilityCode || fe.Param != "tools[0]" || fe.StatusCode != 400 {
		t.Fatalf("err = %T %v", err, err)
	}
	if l := decodedLeaks(err, planRetired); len(l) > 0 {
		t.Fatalf("the rotated value survives at %v", l)
	}
	if !strings.Contains(fe.Message, planOther) {
		t.Fatalf("control: the unrelated text was lost: %q", fe.Message)
	}
	if n := len(srv.requests()); n != 1 {
		t.Fatalf("the request was sent %d times", n)
	}
}
