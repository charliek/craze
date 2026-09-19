package llm

import (
	"context"
	"errors"
	"strings"

	"charm.land/fantasy"
)

// ErrEmptyStep is the error a streamed step ends with when the provider
// finished it with "stop" having sent nothing: no text, reasoning or tool
// call, and zero total usage. Meta reports reasoning that exhausted the
// output ceiling this way (D-25); passed through, Fantasy would record a
// successful, empty answer.
var ErrEmptyStep = errors.New("llm: the provider ended the step with no content and no usage")

// MidStreamError replaces a provider error that arrives after the stream has
// begun producing output. Fantasy retries a failed step by replaying it from
// scratch, callbacks included, and it decides by type and text: a
// *fantasy.ProviderError with a retryable status or flag, any net.Error, or
// an error whose text looks like an HTTP/2 transport failure (retry.go:
// 183-196, errors.go:137-175). A replay after text is on screen would show
// that text twice. MidStreamError is none of those — it does not unwrap to
// the original, and its Error text never contains the transport phrases — so
// the step fails once, instead (D-32).
//
// It keeps what the harness classifies by: the provider's message, scrubbed
// of the key; the HTTP status; the auth flag; and whether the provider said
// the context was too large. Its fields and IsContextTooLarge mirror
// *fantasy.ProviderError's, so the runner can classify either one the same
// way.
type MidStreamError struct {
	// Message is the original error's text, scrubbed.
	Message string
	// StatusCode is the original's HTTP status: 0 when it had none, which is
	// the usual case, since an in-band stream error rides in a 200 response.
	StatusCode int
	// AuthError is the original's fantasy.ProviderError.AuthError.
	AuthError bool

	contextTooLarge bool
}

// transportPhrases defuses the two phrases fantasy.IsTransportError looks
// for in an error's text; either one would make Fantasy retry the step.
// An in-band stream error's text starts with one ("stream error: …").
var transportPhrases = strings.NewReplacer("stream error:", "stream error -", "connection error:", "connection error -")

func (e *MidStreamError) Error() string {
	return "llm: the stream failed after output began: " + transportPhrases.Replace(e.Message)
}

// IsContextTooLarge reports whether the original error said the request
// exceeded the model's context window.
func (e *MidStreamError) IsContextTooLarge() bool { return e.contextTooLarge }

// model decorates a Fantasy LanguageModel with the four things the harness
// needs from every streamed step (plan 018 §3.5):
//
//  1. A "stop" finish becomes "tool-calls" when the step carried at least
//     one complete tool call. Some providers finish a tool turn with "stop",
//     and Fantasy dispatches tools and continues only on "tool-calls"
//     (agent.go:1780, 1824), so the call would be silently dropped (D-21).
//     "length", "content-filter", "error" and "unknown" are left alone, so
//     Fantasy keeps refusing to run a call that may have been cut short.
//  2. A "stop" finish with no output part and zero total usage becomes an
//     error part carrying ErrEmptyStep (D-25).
//  3. A provider error after output began becomes a *MidStreamError, which
//     Fantasy does not retry (D-32). Before that, an error keeps its
//     classification, so a failed connection or a 503 is retried as usual.
//     Cancellation and deadline errors always pass through as themselves.
//  4. Every error leaving the model — from Stream itself, from an error
//     part, and from Generate, GenerateObject and StreamObject — is
//     scrubbed of the key and of URL query strings (see scrubber).
//
// Generate, GenerateObject and StreamObject otherwise delegate untouched:
// nothing in the harness uses them, and the finish rules only matter to a
// streamed agent loop.
type model struct {
	inner fantasy.LanguageModel
	scrub *scrubber
}

// wrap decorates inner; scrub hides the key inner authenticates with.
func wrap(inner fantasy.LanguageModel, scrub *scrubber) fantasy.LanguageModel {
	return &model{inner: inner, scrub: scrub}
}

func (m *model) Provider() string { return m.inner.Provider() }
func (m *model) Model() string    { return m.inner.Model() }

func (m *model) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	resp, err := m.inner.Generate(ctx, call)
	return resp, m.scrub.err(err)
}

func (m *model) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	resp, err := m.inner.GenerateObject(ctx, call)
	return resp, m.scrub.err(err)
}

func (m *model) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	inner, err := m.inner.StreamObject(ctx, call)
	if err != nil {
		return nil, m.scrub.err(err)
	}
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		for part := range inner {
			part.Error = m.scrub.err(part.Error)
			if !yield(part) {
				return
			}
		}
	}, nil
}

// Stream applies the four rules above to one step. An error returned by the
// inner Stream itself — the OpenAI-compatible client returns one only for a
// request it could not build; HTTP failures arrive as error parts — precedes
// any output by definition, so it is scrubbed and otherwise left to Fantasy.
//
// The state lives inside the iterator, so a step Fantasy retries starts
// clean: a retry calls Stream again, and the fresh response has yielded
// nothing.
func (m *model) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	inner, err := m.inner.Stream(ctx, call)
	if err != nil {
		return nil, m.scrub.err(err)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		// output is set by any part a consumer can observe as the answer —
		// text, reasoning, tool input, tool calls and results, sources — and
		// never by warnings, the finish or an error. Rules 2 and 3 share it:
		// a step that produced nothing is both empty and safe to replay.
		output, toolCalls := false, 0
		for part := range inner {
			switch part.Type {
			case fantasy.StreamPartTypeWarnings:
			case fantasy.StreamPartTypeError:
				part.Error = m.stepError(part.Error, output)
			case fantasy.StreamPartTypeFinish:
				part = normalizeFinish(part, output, toolCalls)
			default:
				output = true
				if part.Type == fantasy.StreamPartTypeToolCall {
					toolCalls++
				}
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

// stepError is rule 3 plus the scrub.
func (m *model) stepError(err error, output bool) error {
	if !output || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return m.scrub.err(err)
	}
	return m.scrub.midStream(err)
}

// normalizeFinish is rules 1 and 2. The provider suppresses the ToolCall
// parts of a truncated call before a "length" finish, so every ToolCall part
// counted here is complete.
func normalizeFinish(part fantasy.StreamPart, output bool, toolCalls int) fantasy.StreamPart {
	if part.FinishReason != fantasy.FinishReasonStop {
		return part
	}
	switch {
	case toolCalls > 0:
		part.FinishReason = fantasy.FinishReasonToolCalls
	case !output && part.Usage.TotalTokens == 0:
		part = fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ErrEmptyStep}
	}
	return part
}
