package harness

import (
	"errors"
	"fmt"
	"testing"

	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// TestProviderErrorSaysWhatItIs is the table the waiting commands' kinds and
// retryable come from: a typed failure's kind, and for the rest whether a fresh
// turn may succeed, by its class and never by its text.
func TestProviderErrorSaysWhatItIs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       *ProviderError
		kind      errkind.Kind
		retryable bool
	}{
		{"a 401", &ProviderError{StatusCode: 401, kind: ErrAuth}, errkind.Auth, false},
		{"a 403", &ProviderError{StatusCode: 403, kind: ErrAuth}, errkind.Auth, false},
		{"a 404", &ProviderError{StatusCode: 404, kind: ErrModelNotFound}, errkind.ModelNotFound, false},
		{"an overflow", &ProviderError{StatusCode: 400, kind: ErrContextTooLarge}, errkind.ContextTooLarge, false},
		{"a 402", &ProviderError{StatusCode: 402}, errkind.Quota, false},
		{"a 429 of a quota gone, by its code", &ProviderError{StatusCode: 429, Code: "insufficient_quota"}, errkind.Quota, false},
		{"a 429 of a quota gone, by its words", &ProviderError{StatusCode: 429, Message: "You exceeded your current quota"}, errkind.Quota, false},
		{"the plan's usage limit", &ProviderError{StatusCode: 429, Code: CodeUsageLimit, Final: true}, errkind.Quota, false},
		{"a rate limit", &ProviderError{StatusCode: 429}, errkind.Provider, true},
		{"the plan's 429", &ProviderError{StatusCode: 429, Final: true}, errkind.Provider, false},
		{"the plan's 400", &ProviderError{StatusCode: 400, Final: true}, errkind.Provider, false},
		{"a 500", &ProviderError{StatusCode: 500}, errkind.Provider, true},
		{"a 503", &ProviderError{StatusCode: 503}, errkind.Provider, true},
		{"a 408", &ProviderError{StatusCode: 408}, errkind.Provider, true},
		{"a 400", &ProviderError{StatusCode: 400}, errkind.Provider, false},
		{"a 422", &ProviderError{StatusCode: 422}, errkind.Provider, false},
		{"a connection that failed", &ProviderError{Message: "connection reset by peer"}, errkind.Provider, true},
		{"an in-band server error", &ProviderError{Code: "server_error"}, errkind.Provider, true},
		{"an in-band overload, by its type", &ProviderError{Type: "overloaded_error"}, errkind.Provider, true},
		{"an in-band content filter", &ProviderError{Code: "content_filter"}, errkind.Provider, false},
		{"an in-band quota", &ProviderError{Code: "insufficient_quota"}, errkind.Quota, false},
		{"an in-band OpenAI overload, by its code", &ProviderError{Code: "server_is_overloaded"}, errkind.Provider, true},
		{"an in-band OpenAI overload, by its type", &ProviderError{Type: "service_unavailable_error"}, errkind.Provider, true},
		{"tool calls that cannot be paired", &ProviderError{kind: ErrBadToolCalls}, errkind.Provider, false},
		{"the sign-in's own failure", &ProviderError{signIn: errors.New("sign in again")}, errkind.Provider, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// It says so through any wrapping: nativeError's, the adapter's own.
			got, ok := errkind.Of(fmt.Errorf("wrapped: %w", tc.err))
			if !ok || got.Kind != tc.kind || got.Retryable != tc.retryable || got.Status != tc.err.StatusCode {
				t.Fatalf("%+v (%v), want %s, retryable %v", got, ok, tc.kind, tc.retryable)
			}
		})
	}
}

// TestProviderErrorsCodeIsANameOnly: its code, its type standing in, and neither
// when it is anything but a short identifier: what a result's detail may carry.
func TestProviderErrorsCodeIsANameOnly(t *testing.T) {
	for _, tc := range []struct {
		err  *ProviderError
		want string
	}{
		{&ProviderError{Code: "rate_limit_exceeded", Type: "requests"}, "rate_limit_exceeded"},
		{&ProviderError{Type: "overloaded_error"}, "overloaded_error"},
		{&ProviderError{Code: "sk-ant-THIS IS A KEY"}, ""},
		{&ProviderError{Message: "insufficient_quota"}, ""},
	} {
		if got := tc.err.ErrorInfo().ProviderCode; got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestAKeyInTheWayIsConfiguration: Open's refusals over where a configured key
// turns up each say what to change, and that is what they say they are. Wrapped
// as the adapter wraps Open's errors. Negative: a child's, which no start reports.
func TestAKeyInTheWayIsConfiguration(t *testing.T) {
	for _, err := range []error{errWorkspaceKey, errPlanPathKey, errProfileKey, errSnapshotKey, errDigestKey, errHeaderKey} {
		info, ok := errkind.Of(fmt.Errorf("native: %w", err))
		if !ok || info != errkind.New(errkind.Config) {
			t.Errorf("%v: %+v (%v)", err, info, ok)
		}
	}
	if _, ok := errkind.Of(errChildPromptKey); ok {
		t.Errorf("a sub-agent's refusal says what it is: %v", errChildPromptKey)
	}
}

// TestOpenAIQuotaCodesAreQuota (plan 050 C3): the key route's final 429 for
// each of OpenAI's billing codes — no prepaid credit left, an organization's
// or a project's spend limit, an organization's usage limit — and
// insufficient_quota, in its code or its type, as classify makes it from the
// driver's *llm.FinalError, is kind quota and not retryable; so is the same
// code inside the stream, with no status (through the real driver:
// TestOpenAIFailuresThroughTheDriver). Negative control: without the four
// codes in quotaExhaustedCodes a 429 naming one by its code alone is a
// provider failure.
func TestOpenAIQuotaCodesAreQuota(t *testing.T) {
	r := modeltable.Resolved{ProviderID: "openai", Alias: "openai/gpt-6.1-sol", WireModel: "gpt-6.1-sol", Driver: modeltable.DriverOpenAIResponses}
	for _, code := range []string{"insufficient_quota", "credit_balance_exhausted", "organization_spend_limit_exceeded",
		"project_spend_limit_exceeded", "organization_usage_limit_exceeded"} {
		for name, err := range map[string]error{
			"the code":  &llm.FinalError{StatusCode: 429, Code: code, Type: "requests", Message: "billing"},
			"the type":  &llm.FinalError{StatusCode: 429, Type: code, Message: "billing"},
			"in-stream": &llm.MidStreamError{Message: "billing", Code: code},
		} {
			t.Run(code+" as "+name, func(t *testing.T) {
				info, ok := errkind.Of(fmt.Errorf("native: %w", classify(err, r)))
				if !ok || info.Kind != errkind.Quota || info.Retryable {
					t.Fatalf("%+v (%v), want quota, not retryable", info, ok)
				}
			})
		}
	}
}

// TestModelNotFoundByItsCode (plan 050 C3): on a key-funded driver other than
// Anthropic's, a 400 or a 403 whose code is model_not_found — OpenAI's for a
// model its Responses API does not take, or one a project may not use — is
// ErrModelNotFound, kind model_not_found; a 401 is the key's whatever its
// code. The controls: the same 400 and 403 on the ChatGPT plan's and
// Anthropic's drivers keep their kinds, and another code on a 403 stays auth.
// (Before output, through the real driver: TestOpenAIFailuresThroughTheDriver.)
// Negative control: without the rule the 400 is a provider failure and the
// 403 an auth one.
func TestModelNotFoundByItsCode(t *testing.T) {
	failure := func(status int, code string) error {
		return &llm.MidStreamError{Message: "m", StatusCode: status, AuthError: status == 401 || status == 403, Code: code, Type: "invalid_request_error"}
	}
	for _, tc := range []struct {
		name   string
		driver string
		err    error
		kind   errkind.Kind
	}{
		{"a 400 on openai-responses", modeltable.DriverOpenAIResponses, failure(400, "model_not_found"), errkind.ModelNotFound},
		{"a 403 on openai-responses", modeltable.DriverOpenAIResponses, failure(403, "model_not_found"), errkind.ModelNotFound},
		{"a 400 on openai-compat", modeltable.DriverOpenAICompat, failure(400, "model_not_found"), errkind.ModelNotFound},
		{"a 401 keeps the key's", modeltable.DriverOpenAIResponses, failure(401, "model_not_found"), errkind.Auth},
		{"a 403 of another code", modeltable.DriverOpenAIResponses, failure(403, "unsupported_country_region_territory"), errkind.Auth},
		{"the ChatGPT plan's 400 (control)", modeltable.DriverChatGPT, &llm.FinalError{StatusCode: 400, Code: "model_not_found", Message: "m"}, errkind.Provider},
		{"the ChatGPT plan's 403 (control)", modeltable.DriverChatGPT, failure(403, "model_not_found"), errkind.Auth},
		{"Anthropic's 403 (control)", modeltable.DriverAnthropic, failure(403, "model_not_found"), errkind.Auth},
		{"Anthropic's 400 (control)", modeltable.DriverAnthropic, failure(400, "model_not_found"), errkind.Provider},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := modeltable.Resolved{ProviderID: "p", Alias: "p/m", WireModel: "m", Driver: tc.driver}
			err := classify(tc.err, r)
			info, ok := errkind.Of(err)
			if !ok || info.Kind != tc.kind || info.Retryable {
				t.Fatalf("%v: %+v (%v), want %s, not retryable", err, info, ok, tc.kind)
			}
			if errors.Is(err, ErrModelNotFound) != (tc.kind == errkind.ModelNotFound) {
				t.Fatalf("errors.Is(err, ErrModelNotFound) = %v", errors.Is(err, ErrModelNotFound))
			}
		})
	}
}
