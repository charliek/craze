package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"slices"
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
// of the key; the HTTP status; the auth flag; whether the provider said the
// context was too large; and the provider's own code and type for the
// failure. Its fields and IsContextTooLarge mirror *fantasy.ProviderError's,
// so the runner can classify either one the same way.
type MidStreamError struct {
	// Message is the original error's text, scrubbed.
	Message string
	// StatusCode is the original's HTTP status: 0 when it had none, which is
	// the usual case, since an in-band stream error rides in a 200 response.
	StatusCode int
	// AuthError is the original's fantasy.ProviderError.AuthError.
	AuthError bool
	// Code and Type are the provider's names for the failure, read from the
	// original's response as it arrived and kept only as ErrorNames keeps
	// them: "" unless a short lowercase identifier — "insufficient_quota",
	// say — that the scrub leaves as it is.
	Code, Type string

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
//     Cancellation and deadline errors always pass through as themselves,
//     and so does a *FinalError, scrubbed, before output or after: it is
//     never retried either way, and its code is what the ChatGPT plan's
//     usage latch and the harness's classification read (plan 033 §3.12).
//  4. Every error leaving the model — from Stream itself, from an error
//     part, and from Generate, GenerateObject and StreamObject — is
//     scrubbed of the key and of URL query strings (see scrubber).
//  5. The images tools returned in a run of tool results go to the provider
//     after the whole run, in one user message, rather than one after each
//     result (regroupToolImages, plan 033 §3.5, P10). The request alone is
//     rewritten, never the messages the caller holds.
//
// Generate, GenerateObject and StreamObject otherwise delegate untouched:
// nothing in the harness uses them, and the finish rules only matter to a
// streamed agent loop. Generate sends the request rule 5 rewrites, as Stream
// does, since the request is the same either way.
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
	call.Prompt = regroupToolImages(call.Prompt)
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

// Stream applies the five rules above to one step. An error returned by the
// inner Stream itself — the OpenAI-compatible client returns one only for a
// request it could not build; HTTP failures arrive as error parts — precedes
// any output by definition, so it is scrubbed and otherwise left to Fantasy.
//
// The state lives inside the iterator, so a step Fantasy retries starts
// clean: a retry calls Stream again, and the fresh response has yielded
// nothing.
func (m *model) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	call.Prompt = regroupToolImages(call.Prompt)
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
	var fe *FinalError
	if !output || errors.As(err, &fe) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return m.scrub.err(err)
	}
	return m.scrub.midStream(err)
}

// normalizeFinish is rules 1 and 2. The provider suppresses the ToolCall
// parts of a truncated call before a "length" finish, so every ToolCall part
// counted here is complete. A finish that stays a finish records the reason
// the provider sent, before either rule, in its provider metadata
// (RawFinish).
func normalizeFinish(part fantasy.StreamPart, output bool, toolCalls int) fantasy.StreamPart {
	raw := part.FinishReason
	if raw == fantasy.FinishReasonStop {
		switch {
		case toolCalls > 0:
			part.FinishReason = fantasy.FinishReasonToolCalls
		case !output && part.Usage.TotalTokens == 0:
			return fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ErrEmptyStep}
		}
	}
	// A copy: the map is the provider's, and may be shared.
	md := maps.Clone(part.ProviderMetadata)
	if md == nil {
		md = fantasy.ProviderMetadata{}
	}
	md[rawFinishKey] = &rawFinish{Reason: raw}
	part.ProviderMetadata = md
	return part
}

// rawFinishKey is the provider-metadata key the wrapper records a step's raw
// finish reason under. It names no provider, so no provider's own metadata
// can be under it.
const rawFinishKey = "craze.raw_finish"

// rawFinish is the finish reason a provider sent for a step, before the
// wrapper's rules. It rides in the step's provider metadata, which Fantasy
// keeps on the step's response and never puts in a message, so it reaches
// neither the transcript nor the wire.
type rawFinish struct{ Reason fantasy.FinishReason }

func (*rawFinish) Options() {}

func (r *rawFinish) MarshalJSON() ([]byte, error) { return json.Marshal(string(r.Reason)) }

func (r *rawFinish) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	r.Reason = fantasy.FinishReason(s)
	return nil
}

// RawFinish returns the finish reason the provider sent for a step, before
// the wrapper turned a "stop" into "tool-calls" (rule 1), from the step's
// provider metadata (fantasy.StepResult.ProviderMetadata). ok is false for a
// step from a model New did not build, whose finish nothing normalized.
func RawFinish(md fantasy.ProviderMetadata) (reason fantasy.FinishReason, ok bool) {
	r, ok := md[rawFinishKey].(*rawFinish)
	if !ok {
		return "", false
	}
	return r.Reason, true
}

// regroupToolImages is rule 5 (plan 033 §3.5, P10): prompt as the request
// sends it, with the images tools returned moved out of their tool results
// and into one user message after the run of tool results they came in.
//
// Chat Completions has no image in a tool message, so the OpenAI-compatible
// client sends a tool's image result as a text tool message followed at once
// by a user message holding the image (openai.ToolResultMediaMessages,
// openaicompat language_model_hooks.go:539-556). Two images read in parallel
// then go out as tool, user, tool, user: the step's run of tool messages, the
// answers to the assistant's tool calls, is split by a user message, which
// providers that want every call answered before anything else refuse or
// misread. So here, before the client encodes the request, each run of
// consecutive tool messages is rewritten:
//
//   - a tool result whose output is an image (a fantasy.ToolResultOutputContentMedia
//     of an image/* type) becomes a text result of its text — or, with none,
//     a line saying an image follows the results — still answering the same
//     call, so the run stays tool messages alone;
//   - after the run comes one user message: "Images returned by tool call(s)
//     <ids>:", the calls' ids in order, then each image as a file part, which
//     the client sends as an image_url data URI, as it sends a pasted image.
//
// A run with no image, every message outside a run, and an image whose
// base64 does not decode (left to the client's own encoding) are kept as
// they are; a prompt with nothing to move comes back as it is. Nothing the
// caller holds is written to — the messages, their parts, the slice — so the
// history the harness keeps, and the transcript, still hold each image in
// its own result: the regroup is the request's alone, made again for every
// request, and it is a pure function of the messages, so a history replayed
// on a later step or turn is regrouped into the same bytes, and a provider's
// prefix cache sees the same request it saw before.
//
// The ChatGPT plan's Responses driver (responses_adapter.go) keeps it too
// (plan 033 C12): the Responses API's function_call_output can carry an
// image, but the route was only ever seen to take one as user input_image
// (the spike), which is also where Fantasy's own Responses model puts a
// tool's image.
//
// It composes with the harness's vision strip (stripImages, plan 033 §3.5),
// which runs first, before Fantasy assembles the request: to a model that
// does not accept images the history's image results are text already, and
// the turn's own tools return none (tool.Env.Vision), so there is nothing
// left here to move.
func regroupToolImages(prompt fantasy.Prompt) fantasy.Prompt {
	var out fantasy.Prompt // nil until a run changes
	for i := 0; i < len(prompt); {
		if prompt[i].Role != fantasy.MessageRoleTool {
			if out != nil {
				out = append(out, prompt[i])
			}
			i++
			continue
		}
		j := i + 1
		for j < len(prompt) && prompt[j].Role == fantasy.MessageRoleTool {
			j++
		}
		run, images, moved := liftToolImages(prompt[i:j])
		switch {
		case moved:
			if out == nil {
				out = append(make(fantasy.Prompt, 0, len(prompt)+1), prompt[:i]...)
			}
			out = append(out, run...)
			out = append(out, images)
		case out != nil:
			out = append(out, prompt[i:j]...)
		}
		i = j
	}
	if out == nil {
		return prompt
	}
	return out
}

// liftToolImages is regroupToolImages for one run of tool messages: the run
// with each image result made text, and the user message holding the images,
// in the order the results came; moved is false, and the rest zero, when the
// run holds no image to move. run itself is never written to.
func liftToolImages(run []fantasy.Message) (out []fantasy.Message, images fantasy.Message, moved bool) {
	var ids []string
	var files []fantasy.MessagePart
	for k, m := range run {
		var content []fantasy.MessagePart // m's, copied once it changes
		for p, part := range m.Content {
			r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				continue
			}
			o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](r.Output)
			if !ok || !strings.HasPrefix(o.MediaType, "image/") {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(o.Data)
			if err != nil {
				continue
			}
			if content == nil {
				content = slices.Clone(m.Content)
			}
			text := o.Text
			if text == "" {
				text = "The tool returned an image (" + o.MediaType + "); it follows the tool results."
			}
			r.Output = fantasy.ToolResultOutputContentText{Text: text}
			content[p] = r
			ids = append(ids, r.ToolCallID)
			files = append(files, fantasy.FilePart{Data: data, MediaType: o.MediaType})
		}
		if content == nil {
			continue
		}
		if out == nil {
			out = slices.Clone(run)
		}
		m.Content = content
		out[k] = m
	}
	if out == nil {
		return nil, fantasy.Message{}, false
	}
	intro := fantasy.TextPart{Text: "Images returned by tool call(s) " + strings.Join(ids, ", ") + ":"}
	images = fantasy.Message{Role: fantasy.MessageRoleUser, Content: append([]fantasy.MessagePart{intro}, files...)}
	return out, images, true
}
