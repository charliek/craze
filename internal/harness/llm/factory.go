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
	httpClient *http.Client
}

// WithHTTPClient sends the model's requests through c instead of the SDK's
// default client: the seam tests use to reach a local server.
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) { o.httpClient = c }
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
	default:
		return nil, unknownDriver(r)
	}
	key := strings.TrimSpace(r.APIKey.Reveal())
	switch {
	case key == "":
		return nil, fmt.Errorf("%w for provider %q (model %q)", modeltable.ErrNoAPIKey, r.ProviderID, r.Alias)
	case len(key) < minKeyLen:
		return nil, fmt.Errorf("%w: provider %q (model %q) has one under %d bytes", ErrAPIKeyTooShort, r.ProviderID, r.Alias, minKeyLen)
	}
	scrub := newScrubber(key)

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

// openAIEfforts are the reasoning efforts Fantasy's OpenAI-compatible client
// can send (openaicompat PrepareCallFunc); it fails a request with any other.
var openAIEfforts = []openai.ReasoningEffort{
	openai.ReasoningEffortNone,
	openai.ReasoningEffortMinimal,
	openai.ReasoningEffortLow,
	openai.ReasoningEffortMedium,
	openai.ReasoningEffortHigh,
	openai.ReasoningEffortXHigh,
	openai.ReasoningEffortMax,
}

// EffortOptions returns the provider options that ask r's model for effort,
// for a call's ProviderOptions (fantasy.AgentStreamCall.ProviderOptions):
// reasoning_effort for the openai-compat driver, OpenRouter's
// reasoning.effort for openrouter, keyed by the provider id New named the
// model's provider with. It returns nil when effort is "", so nothing is
// sent: the harness passes "" for a model with no effort control.
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
