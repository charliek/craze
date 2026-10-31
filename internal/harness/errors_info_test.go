package harness

import (
	"errors"
	"fmt"
	"testing"

	"github.com/charliek/craze/internal/errkind"
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
