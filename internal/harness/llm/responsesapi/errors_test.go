package responsesapi

import (
	"net/http"
	"testing"
)

// TestErrorClassification: which failures are final (never retried, plan 033
// §3.9, P33) and which may be tried again before any output (D-32), by HTTP
// status and by code, inside the stream or out. Every 429 and every 400 is
// final; so are the codes OpenAI's docs say not to repeat. A 503 and the
// usage and user checks that could not run are transient. The rest — a 403
// for a region, a 401 after its renewal, a 404, a 500 — are neither: the
// caller's own rule decides.
func TestErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status    int
		code      string
		final     bool
		transient bool
	}{
		{http.StatusTooManyRequests, "", true, false},
		{http.StatusTooManyRequests, "subscription_sharing_usage_limit_exceeded", true, false},
		{http.StatusBadRequest, "", true, false},
		{http.StatusBadRequest, "subscription_sharing_unsupported_capability", true, false},
		{http.StatusForbidden, "subscription_sharing_user_not_eligible", true, false},
		{http.StatusForbidden, "subscription_sharing_route_not_supported", true, false},
		{http.StatusForbidden, "chatpass_v2_scope_not_authorized", true, false},
		{http.StatusForbidden, "chatpass_v2_invalid_authorization_context", true, false},
		{0, "subscription_sharing_usage_limit_exceeded", true, false},
		{0, "subscription_sharing_unsupported_capability", true, false},
		{0, "rate_limit_exceeded", true, false},
		{http.StatusServiceUnavailable, "", false, true},
		{http.StatusServiceUnavailable, "subscription_sharing_usage_unavailable", false, true},
		{0, "subscription_sharing_usage_unavailable", false, true},
		{0, "subscription_sharing_user_unavailable", false, true},
		{0, "server_error", false, true},
		{http.StatusForbidden, "", false, false},
		{http.StatusUnauthorized, "subscription_sharing_invalid_user", false, false},
		{http.StatusNotFound, "", false, false},
		{http.StatusInternalServerError, "", false, false},
		{0, "", false, false},
		{0, "content_policy_violation", false, false},
	} {
		e := &Error{StatusCode: tc.status, Code: tc.code}
		if e.Final() != tc.final || e.Transient() != tc.transient {
			t.Errorf("HTTP %d %q: final %v transient %v, want %v %v", tc.status, tc.code, e.Final(), e.Transient(), tc.final, tc.transient)
		}
	}
	if e := (&Error{Type: "server_error"}); !e.Transient() {
		t.Error("a server_error type is not transient")
	}
}

// TestErrorContextTooLarge: an overflow is told apart by its code or its
// words, so the harness can compact and send a smaller request.
func TestErrorContextTooLarge(t *testing.T) {
	for _, tc := range []struct {
		e    Error
		want bool
	}{
		{Error{StatusCode: 400, Code: "context_length_exceeded"}, true},
		{Error{StatusCode: 400, Message: "Your input exceeds the context window of this model. Please adjust your input and try again."}, true},
		{Error{Message: "This model's maximum context length is 272000 tokens."}, true},
		{Error{StatusCode: 400, Code: "invalid_value", Message: "Invalid value: 'bogus'."}, false},
		{Error{StatusCode: 429, Message: "rate limited"}, false},
	} {
		if got := tc.e.ContextTooLarge(); got != tc.want {
			t.Errorf("%+v: ContextTooLarge = %v, want %v", tc.e, got, tc.want)
		}
	}
}

// TestErrorKeepsOnlyIdentifiers: a code, type or param that is not the
// short identifier it should be is dropped, so free text — or a token a
// server echoed — cannot ride in what the harness decides by. The message
// is kept as text.
func TestErrorKeepsOnlyIdentifiers(t *testing.T) {
	e := &Error{}
	e.setError(&wireError{
		Code:    []byte(`"Bearer test-token-one-0001"`),
		Type:    []byte(`429`),
		Param:   []byte(`"input[3].content[0].image_url"`),
		Message: "kept as text",
	})
	if e.Code != "" || e.Type != "" || e.Param != "input[3].content[0].image_url" || e.Message != "kept as text" {
		t.Fatalf("error = %+v", e)
	}
	e = &Error{}
	e.setError(&wireError{Code: []byte(`"subscription_sharing_usage_limit_exceeded"`), Param: []byte(`"a b"`)})
	if e.Code != "subscription_sharing_usage_limit_exceeded" || e.Param != "" {
		t.Fatalf("error = %+v", e)
	}
}
