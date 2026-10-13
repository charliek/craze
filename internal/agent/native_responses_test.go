package agent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// TestNativePhrasesTheResponsesDriversFailures (plan 047 D13): a model on the
// openai-responses driver is worded as any key provider's. Its empty step
// keeps the max_output_tokens advice — its requests carry the ceiling as
// max_output_tokens (MR-8) — and a refused key names the provider, its
// command and its variable, never the ChatGPT plan; a 404 points at
// models.toml. The control: the ChatGPT plan's empty step drops the advice.
func TestNativePhrasesTheResponsesDriversFailures(t *testing.T) {
	advice := "native: the model ended its answer without sending anything " +
		"(a reasoning model can spend its whole output ceiling thinking; raise max_output_tokens in models.toml)"
	if got := phraseTurnError(&harness.EmptyStepError{Model: "muse-wonder-1.0", Driver: modeltable.DriverOpenAIResponses}); got.Error() != advice {
		t.Fatalf("an empty step on openai-responses = %q, want %q", got, advice)
	}
	if got := phraseTurnError(&harness.EmptyStepError{Model: "chatgpt/gpt-5.6-sol", Driver: modeltable.DriverChatGPT}); got.Error() == advice {
		t.Fatal("the control: the ChatGPT plan's empty step kept the advice")
	}

	table := &modeltable.Table{Providers: map[string]modeltable.Provider{
		"meta": {Name: "Meta", Driver: modeltable.DriverOpenAIResponses, BaseURL: "https://api.meta.ai/v1", EnvKeys: []string{"META_API_KEY"}},
	}}
	pe := func(status int, kind error) error {
		p := &harness.ProviderError{Provider: "meta", Model: "muse-wonder-1.0", Driver: modeltable.DriverOpenAIResponses,
			StatusCode: status, Message: "Unauthorized " + nativeCanary}
		// The harness's classify sets the kind itself; the join stands in.
		return fmt.Errorf("turn: %w", errors.Join(p, kind))
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"401", pe(401, harness.ErrAuth),
			`native: Meta rejected the API key (HTTP 401); replace it with "craze auth login meta", or check META_API_KEY`},
		{"404", pe(404, harness.ErrModelNotFound),
			`native: provider "meta" does not serve model "muse-wonder-1.0" (HTTP 404); check its wire_model in models.toml`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := phraseTurnErrorIn(tc.err, table); got.Error() != tc.want {
				t.Fatalf("phrased %q\nwant    %q", got, tc.want)
			}
		})
	}
}
