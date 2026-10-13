// Package llm turns a model-table entry into the Fantasy LanguageModel the
// native harness runs a turn on: the provider factory (New), the per-call
// effort options (EffortOptions), and the decorator every model is wrapped
// in, which normalizes a streamed step's finish, refuses to retry a step
// whose output is already on screen, and scrubs the key from every error
// (wrap.go; plan 018 §3.5).
//
// Both drivers are built on Fantasy's OpenAI-compatible provider, and no
// other Fantasy provider package is imported. Fantasy's openrouter package
// imports its anthropic and google packages (for their reasoning-metadata
// types), which pull in the Anthropic, AWS and Google SDKs — about 16 MB of
// binary — and keeping those SDKs out is the point of the link-set rule
// (owner decision 2). OpenRouter's API is OpenAI-compatible, so the
// "openrouter" driver is the same client at OpenRouter's fixed endpoint, with
// effort sent in OpenRouter's own request shape. deps_test.go holds the line.
//
// A third driver, the ChatGPT plan's (modeltable.DriverChatGPT, plan 033
// §3.9), is craze's own Responses client, package responsesapi — no Fantasy
// provider and no SDK at all — adapted to Fantasy in responses_adapter.go
// and authenticated by an Auth rather than a key: the sign-in's token source,
// which the caller hands New with WithSignIn (plan 033 §3.12).
//
// A fourth, Anthropic's (modeltable.DriverAnthropic, plan 044 §3.3), is
// craze's own Messages API client, package messagesapi — again no Fantasy
// provider and no SDK, so the Anthropic SDK stays out of the binary (owner
// decision 2) — adapted to Fantasy in messages_adapter.go and funded by a
// key, at the API base the caller answers with WithAnthropicAPIBase (P21).
//
// A fifth, the Responses API with an API key (modeltable.DriverOpenAIResponses,
// plan 047 D12, D13), is package responsesapi again, on its key route: the
// same adapter as the ChatGPT plan's, at the provider's base_url, funded by
// its key (newKeyResponses).
package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/openai/openai-go/v3/option"
)

// openRouterBaseURL is OpenRouter's Chat Completions endpoint, Fantasy's
// openrouter.DefaultURL. A model table gives the openrouter driver no base
// URL; tests reach a local server by rewriting the host in the injected
// client's transport.
const openRouterBaseURL = "https://openrouter.ai/api/v1"

// minKeyLen is the shortest API key New accepts, in bytes: the model
// table's floor, which Load and Keys enforce too.
const minKeyLen = modeltable.MinKeyLen

// ErrAPIKeyTooShort is New's error for a key under minKeyLen bytes. No real
// provider issues one that short, so it is a placeholder or a typo — and the
// scrubber replaces the key's text wherever it appears, so a key like "a" or
// "error" would shred every error message, including the "stream error:"
// phrase Fantasy's retry logic reads. The error names the provider, never
// the key.
var ErrAPIKeyTooShort = errors.New("llm: API key too short to be real")

// Option configures New.
type Option func(*options)

type options struct {
	httpClient    *http.Client
	auth          Auth
	apiBase       func() (string, error)
	anthropicBase func() (string, error)
}

// WithHTTPClient sends the model's requests through c instead of the SDK's
// default client: the seam tests use to reach a local server.
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) { o.httpClient = c }
}

// WithSignIn is the sign-in a model on the ChatGPT plan's driver is funded by
// (plan 033 §3.12, P31): auth, the process's token source — the harness
// knows it only as an Auth, handed in through harness.Options — and apiBase,
// which answers the API's base URL when the model is built: OpenAI's own, or
// a test's loopback server (chatgptauth.APIBase, whose error — a test
// override that is not a loopback URL — fails the build, so the bearer can
// never fall back to another host). A nil apiBase is OpenAI's. Every model
// New builds also keeps auth's token values out of its errors, whatever its
// driver: a provider error can echo a request, and a request can hold a tool
// result that printed a token (scrubber, A21b).
func WithSignIn(auth Auth, apiBase func() (string, error)) Option {
	return func(o *options) { o.auth, o.apiBase = auth, apiBase }
}

// WithAnthropicAPIBase answers the API base a model on Anthropic's driver
// sends to (plan 044 §3.3, P21), when the model is built: Anthropic's own,
// or a test's loopback server (messagesapi.BaseURL, whose error — a test
// override that is not a loopback URL — fails the build, so the key can
// never fall back to another host). A nil apiBase is Anthropic's own.
func WithAnthropicAPIBase(apiBase func() (string, error)) Option {
	return func(o *options) { o.anthropicBase = apiBase }
}

// New builds r's language model, wrapped (see wrap.go). This is the one
// place outside modeltable that reads the key: it goes to the provider's
// HTTP client, and to the scrubber that keeps it out of the model's errors.
//
// r comes from modeltable.Table.Resolve, which has already failed with
// modeltable.ErrNoAPIKey for a provider with no key and modeltable.Load,
// which has already refused an openai-compat provider with no base URL; New
// does not look at the environment again. It checks both all the same, for a
// Resolved built anywhere else, because each gap sends a key to the wrong
// place: with no key the OpenAI SDK falls back to OPENAI_API_KEY and sends
// the owner's OpenAI key to another provider, and with no base URL Fantasy
// falls back to https://api.openai.com/v1 and sends another provider's key
// to OpenAI.
//
// A key shorter than minKeyLen is refused too (see ErrAPIKeyTooShort).
//
// A model on Anthropic's driver is checked for its key the same way, and is
// craze's own Messages API model (newAnthropic); so is a model on the
// openai-responses driver, which is craze's own Responses model on its key
// route, at its base URL (newKeyResponses).
func New(r modeltable.Resolved, opts ...Option) (fantasy.LanguageModel, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	var baseURL string
	switch r.Driver {
	case modeltable.DriverOpenAICompat:
		baseURL = strings.TrimSpace(r.BaseURL)
		if baseURL == "" {
			return nil, fmt.Errorf("llm: provider %q (model %q): driver %q needs a base URL", r.ProviderID, r.Alias, r.Driver)
		}
	case modeltable.DriverOpenRouter:
		baseURL = openRouterBaseURL
	case modeltable.DriverChatGPT:
		return newSignedIn(r, o)
	case modeltable.DriverAnthropic:
		return newAnthropic(r, o)
	case modeltable.DriverOpenAIResponses:
		return newKeyResponses(r, o)
	default:
		return nil, unknownDriver(r)
	}
	key, err := checkedKey(r)
	if err != nil {
		return nil, err
	}
	scrub := newScrubber(key, o.auth)

	providerOpts := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(key),
		// The provider id names the model's provider, and it is the key
		// the OpenAI-compatible client looks its per-call options up by
		// (openaicompat PrepareCallFunc: call.ProviderOptions[model.Provider()]).
		openaicompat.WithName(r.ProviderID),
		// The OpenAI SDK adds these two headers from OPENAI_ORG_ID and
		// OPENAI_PROJECT_ID in the environment (openai-go client.go,
		// DefaultClientOptions): the owner's OpenAI identifiers, which have no
		// business at another provider. Its switch for ignoring the
		// environment is internal, so the headers are deleted instead; the
		// key and base URL it reads from there are always overridden above.
		openaicompat.WithSDKOptions(
			option.WithHeaderDel("OpenAI-Organization"),
			option.WithHeaderDel("OpenAI-Project"),
		),
	}
	if o.httpClient != nil {
		providerOpts = append(providerOpts, openaicompat.WithHTTPClient(o.httpClient))
	}
	provider, err := openaicompat.New(providerOpts...)
	if err != nil {
		return nil, fmt.Errorf("llm: model %q: %w", r.Alias, scrub.err(err))
	}
	lm, err := provider.LanguageModel(context.Background(), r.WireModel)
	if err != nil {
		return nil, fmt.Errorf("llm: model %q: %w", r.Alias, scrub.err(err))
	}
	return wrap(lm, scrub), nil
}

// checkedKey is r's key, trimmed, refused when there is none
// (modeltable.ErrNoAPIKey) or it is too short to be real (ErrAPIKeyTooShort).
func checkedKey(r modeltable.Resolved) (string, error) {
	key := strings.TrimSpace(r.APIKey.Reveal())
	switch {
	case key == "":
		return "", fmt.Errorf("%w for provider %q (model %q)", modeltable.ErrNoAPIKey, r.ProviderID, r.Alias)
	case len(key) < minKeyLen:
		return "", fmt.Errorf("%w: provider %q (model %q) has one under %d bytes", ErrAPIKeyTooShort, r.ProviderID, r.Alias, minKeyLen)
	}
	return key, nil
}

// resolveBase is the API base answer gives r's model when it is built (an
// option's: WithSignIn's, WithAnthropicAPIBase's), or fallback without one.
func resolveBase(r modeltable.Resolved, answer func() (string, error), fallback string) (string, error) {
	if answer == nil {
		return fallback, nil
	}
	base, err := answer()
	if err != nil {
		return "", fmt.Errorf("llm: model %q: %w", r.Alias, err)
	}
	return base, nil
}

// newSignedIn is New for the ChatGPT plan's driver (plan 033 §3.12): the
// Responses model on the sign-in's Auth, at the API base apiBase answers,
// wrapped as every model is, its scrubber hiding the Auth's token values. The
// model is funded by the sign-in or not at all: with no Auth it is refused,
// and a key r carries — none, from the table — is never sent.
func newSignedIn(r modeltable.Resolved, o options) (fantasy.LanguageModel, error) {
	if o.auth == nil {
		return nil, fmt.Errorf("llm: provider %q (model %q): driver %q is funded by signing in to ChatGPT, and no sign-in was given",
			r.ProviderID, r.Alias, r.Driver)
	}
	base, err := resolveBase(r, o.apiBase, "")
	if err != nil {
		return nil, err
	}
	lm, err := newResponsesModel(r, o.auth, base, o.httpClient)
	if err != nil {
		return nil, err
	}
	// The driver's refusal of another account's token (boundCredentials) is
	// kept through the scrub as the Auth's own sentinels are, so the turn's
	// failure answers errors.Is(err, modeltable.ErrOtherAccount) and the
	// adapter can say so, and a summary gives up on it at once (plan 033
	// C14r2): it is fixed text, so it can carry no token.
	scrub := newScrubber("", o.auth)
	scrub.sentinels = append(scrub.sentinels, modeltable.ErrOtherAccount)
	return wrap(lm, scrub), nil
}

// newAnthropic is New for Anthropic's driver (plan 044 §3.3): the Messages
// API model on r's key, checked as New checks any, at the API base the option
// answers (WithAnthropicAPIBase; Anthropic's own without one), wrapped as
// every model is, its scrubber hiding the key. Its build errors name the
// model and the client's refusal (messagesapi.ErrBaseURL among them), never
// the key.
func newAnthropic(r modeltable.Resolved, o options) (fantasy.LanguageModel, error) {
	key, err := checkedKey(r)
	if err != nil {
		return nil, err
	}
	base, err := resolveBase(r, o.anthropicBase, messagesapi.DefaultBaseURL)
	if err != nil {
		return nil, err
	}
	lm, err := newMessagesModel(r, key, base, o.httpClient)
	if err != nil {
		return nil, err
	}
	return wrap(lm, newScrubber(key, o.auth)), nil
}

// newKeyResponses is New for the openai-responses driver (plan 047 D13): the
// Responses model on the key route (newKeyResponsesModel), on r's key,
// checked as New checks any, at r's base URL — which the model table held to
// the route's rule at load, and the client holds to it again here
// (responsesapi.ErrKeyBaseURL), so the key never reaches another origin —
// wrapped as every model is, its scrubber hiding the key. Its build errors
// name the provider and the model, never the key.
func newKeyResponses(r modeltable.Resolved, o options) (fantasy.LanguageModel, error) {
	key, err := checkedKey(r)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSpace(r.BaseURL)
	if base == "" {
		return nil, fmt.Errorf("llm: provider %q (model %q): driver %q needs a base URL", r.ProviderID, r.Alias, r.Driver)
	}
	lm, err := newKeyResponsesModel(r, key, base, o.httpClient)
	if err != nil {
		return nil, err
	}
	return wrap(lm, newScrubber(key, o.auth)), nil
}

// openAIEfforts are the reasoning efforts Fantasy's OpenAI-compatible client
// can send (openaicompat PrepareCallFunc); it fails a request with any other.
// They are also the ChatGPT plan's Responses route's own list (plan 033
// §3.9).
var openAIEfforts = []openai.ReasoningEffort{
	openai.ReasoningEffortNone,
	openai.ReasoningEffortMinimal,
	openai.ReasoningEffortLow,
	openai.ReasoningEffortMedium,
	openai.ReasoningEffortHigh,
	openai.ReasoningEffortXHigh,
	openai.ReasoningEffortMax,
}

// anthropicEfforts are the efforts an Anthropic request can carry
// (output_config.effort, plan 044 §3.3): the API's levels, which
// modeltable keeps its own copy of (TestAnthropicEffortsMatchTheTable).
var anthropicEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// EffortOptions returns the provider options that ask r's model for effort,
// for a call's ProviderOptions (fantasy.AgentStreamCall.ProviderOptions):
// reasoning_effort for the openai-compat driver, OpenRouter's
// reasoning.effort for openrouter, the Responses reasoning.effort for
// chatgpt and openai-responses (responsesOptions, plan 033 §3.9, plan 047
// D13), and output_config.effort for anthropic (anthropicOptions, plan 044
// §3.3), keyed by the provider id New named the model's provider with. It
// returns nil when effort is "", so nothing is sent: the harness passes ""
// for a model with no effort control. A request on openai-responses carries
// more than its effort: WithCacheKey builds each request's options from
// these.
//
// An effort the model does not list is an error rather than a silent no-op,
// as is one the OpenAI-compatible client cannot send, so a bad switch fails
// when it is made instead of on the next request.
func EffortOptions(r modeltable.Resolved, effort string) (fantasy.ProviderOptions, error) {
	if effort == "" {
		return nil, nil
	}
	if !slices.Contains(r.Efforts, effort) {
		if len(r.Efforts) == 0 {
			return nil, fmt.Errorf("llm: model %q has no effort control, so effort %q cannot be sent", r.Alias, effort)
		}
		return nil, fmt.Errorf("llm: model %q has no effort %q (it offers %s)", r.Alias, effort, strings.Join(r.Efforts, ", "))
	}
	var opts *openaicompat.ProviderOptions
	switch r.Driver {
	case modeltable.DriverOpenAICompat:
		e := openai.ReasoningEffort(effort)
		if !slices.Contains(openAIEfforts, e) {
			return nil, fmt.Errorf("llm: model %q: effort %q is not one an OpenAI-compatible request can carry", r.Alias, effort)
		}
		opts = &openaicompat.ProviderOptions{ReasoningEffort: &e}
	case modeltable.DriverChatGPT:
		// The Responses route's own list, which its validation error names
		// (spike): the same seven as an OpenAI-compatible request. Never
		// "ultra", which the account's model list offers some models but which
		// is codex's multi-agent mode, not a request value (plan 033 §3.9).
		if !slices.Contains(openAIEfforts, openai.ReasoningEffort(effort)) {
			return nil, fmt.Errorf("llm: model %q: effort %q is not one a ChatGPT plan request can carry", r.Alias, effort)
		}
		return fantasy.ProviderOptions{r.ProviderID: &responsesOptions{Effort: effort}}, nil
	case modeltable.DriverOpenAIResponses:
		// The same seven as the ChatGPT plan's route: the Responses API's
		// own reasoning.effort values. A provider may accept fewer (Meta
		// refuses none, MR-7); its models' efforts lists say which.
		if !slices.Contains(openAIEfforts, openai.ReasoningEffort(effort)) {
			return nil, fmt.Errorf("llm: model %q: effort %q is not one a Responses request can carry", r.Alias, effort)
		}
		return fantasy.ProviderOptions{r.ProviderID: &responsesOptions{Effort: effort}}, nil
	case modeltable.DriverAnthropic:
		if !slices.Contains(anthropicEfforts, effort) {
			return nil, fmt.Errorf("llm: model %q: effort %q is not one an Anthropic request can carry", r.Alias, effort)
		}
		return fantasy.ProviderOptions{r.ProviderID: &anthropicOptions{Effort: effort}}, nil
	case modeltable.DriverOpenRouter:
		// The same body openrouter.ProviderOptions{Reasoning: {Effort}}
		// produces, spelled through the OpenAI-compatible client's extra
		// body; OpenRouter validates the value itself.
		opts = &openaicompat.ProviderOptions{ExtraBody: map[string]any{
			"reasoning": map[string]any{"effort": effort},
		}}
	default:
		return nil, unknownDriver(r)
	}
	return fantasy.ProviderOptions{r.ProviderID: opts}, nil
}

func unknownDriver(r modeltable.Resolved) error {
	return fmt.Errorf("llm: model %q: provider %q has unknown driver %q", r.Alias, r.ProviderID, r.Driver)
}
