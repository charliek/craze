package llm

import (
	"fmt"
	"strings"
)

// FinalError is a provider failure that must never be retried: not by
// Fantasy's retry, not by the harness's, and not by a wake or a sub-agent
// that would send the same request again (plan 033 §3.9, P33). The ChatGPT
// plan's driver (newResponsesModel) returns it for every HTTP 429 — a usage
// limit says to pause new requests, and Fantasy retries every 429
// (fantasy errors.go:81-100) — for any 400, and for the codes OpenAI's Sign
// in with ChatGPT docs say not to repeat (subscription_sharing_user_not_eligible,
// …_unsupported_capability, …_route_not_supported, chatpass_v2_*), in an
// HTTP status or inside the stream alike.
//
// Fantasy retries a *fantasy.ProviderError it judges retryable, any
// net.Error, and an error whose text holds an HTTP/2 transport phrase
// (retry.go, errors.go). FinalError is none of those: it is its own type, it
// unwraps to nothing, and its Error text has the transport phrases defused,
// as MidStreamError's has — so it fails the step once, before output or
// after.
//
// Its fields mirror what the harness classifies by. Code, Type and Param are
// the provider's own names for the failure — error.code (or a response's
// incomplete_details.reason), error.type and error.param — each "" unless a
// short identifier the driver vetted (package responsesapi), so none of them
// can carry free text. Message is the provider's text — error.message, or a
// {"detail": …} body's detail — which is display text from outside craze,
// and which the scrubber rebuilds scrubbed (plan 033 §3.12, C14).
type FinalError struct {
	// StatusCode is the HTTP status: 0 for a failure inside the stream, which
	// rides in a 200 response.
	StatusCode int
	Code       string
	Type       string
	Param      string
	Message    string

	contextTooLarge bool
}

func (e *FinalError) Error() string {
	var b strings.Builder
	b.WriteString("llm: the provider refused the request, and it is not retried")
	var names []string
	if e.StatusCode != 0 {
		names = append(names, fmt.Sprintf("HTTP %d", e.StatusCode))
	}
	if e.Code != "" {
		names = append(names, e.Code)
	}
	if len(names) > 0 {
		b.WriteString(" (" + strings.Join(names, ", ") + ")")
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return transportPhrases.Replace(b.String())
}

// IsContextTooLarge reports whether the provider said the request exceeded
// the model's context window — a 400 like any other here, final for the
// request as it was sent, but one the harness answers by compacting and
// sending a smaller one (plan 028), as it does for a *fantasy.ProviderError
// that says so.
func (e *FinalError) IsContextTooLarge() bool { return e.contextTooLarge }
