package agent

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/paths"
)

// TestMissingAgentBinarySaysWhatToDo is plan 037 LC-3's half here: a start
// whose agent binary is nowhere is acp's words — what it looked for, under the
// "agent binary not found: " prefix configuration.md quotes — with the fix
// that only this layer can give, since only it knows the provider and the
// config file: install it, or set [agents].<id>. CRAZE_AGENT_BIN, when it is
// the session's, is the explicit path the error names.
func TestMissingAgentBinarySaysWhatToDo(t *testing.T) {
	crazeHome := t.TempDir()
	t.Setenv("CRAZE_HOME", crazeHome)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CRAZE_AGENT_BIN", "")
	config := filepath.Join(crazeHome, "config.toml")
	if got := paths.ConfigPath(); got != config {
		t.Fatalf("the config file is %q, want %q", got, config)
	}
	missing := filepath.Join(t.TempDir(), "no-such-agent")
	cursor, grok, gx := CursorProvider(), GrokProvider(), GxProvider()
	for _, tc := range []struct {
		name string
		p    *Provider
		bin  string
		env  string
		want string
	}{
		{"cursor's two candidates", &cursor, "", "",
			"agent binary not found: cursor-agent is not on PATH (also tried agent); install it, or set [agents].cursor in " + config},
		{"grok", &grok, "", "", "agent binary not found: grok is not on PATH; install it, or set [agents].grok in " + config},
		{"gx", &gx, "", "", "agent binary not found: gx is not on PATH; install it, or set [agents].gx in " + config},
		{"an explicit path", &grok, missing, "", "agent binary not found: " + missing + "; install it, or set [agents].grok in " + config},
		{"CRAZE_AGENT_BIN", &cursor, "", missing, "agent binary not found: " + missing + "; install it, or set [agents].cursor in " + config},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CRAZE_AGENT_BIN", tc.env)
			s := newTestSession(t, Options{Provider: tc.p, Binary: tc.bin, Workspace: t.TempDir(), Stderr: io.Discard, Diag: io.Discard})
			if err := s.Start(t.Context()); err == nil || err.Error() != tc.want {
				t.Fatalf("Start: %v\nwant   %s", err, tc.want)
			}
		})
	}
}

// TestRejectedKeyText is plan 037 LC-8: a provider's refusal of its key — a
// 401, or an authentication failure it flagged — names the provider by its
// display name, keeps the status as the provider sent it, says to replace the
// key with craze auth login <id>, and names the variable the key may come
// from instead: one, several, or none. A variable's name, never a value; the
// provider's own message never. (A 403 says the provider's message first:
// TestRefusedText.)
func TestRejectedKeyText(t *testing.T) {
	table := &modeltable.Table{Providers: map[string]modeltable.Provider{
		"fireworks": {Name: "Fireworks", EnvKeys: []string{"FIREWORKS_API_KEY"}},
		"zhipu":     {Name: "Zhipu AI", EnvKeys: []string{"ZHIPU_API_KEY", "ZAI_API_KEY"}},
		"three":     {Name: "Three", EnvKeys: []string{"A_KEY", "B_KEY", "C_KEY"}},
		"bare":      {},
	}}
	for _, tc := range []struct {
		name     string
		table    *modeltable.Table
		provider string
		status   int
		want     string
	}{
		{"401, one variable", table, "fireworks", 401,
			`native: Fireworks rejected the API key (HTTP 401); replace it with "craze auth login fireworks", or check FIREWORKS_API_KEY`},
		{"401, two variables", table, "zhipu", 401,
			`native: Zhipu AI rejected the API key (HTTP 401); replace it with "craze auth login zhipu", or check ZHIPU_API_KEY or ZAI_API_KEY`},
		{"a flagged failure, three variables", table, "three", 400,
			`native: Three rejected the API key (HTTP 400); replace it with "craze auth login three", or check A_KEY, B_KEY or C_KEY`},
		{"no status", table, "fireworks", 0,
			`native: Fireworks rejected the API key; replace it with "craze auth login fireworks", or check FIREWORKS_API_KEY`},
		{"no name, no variable", table, "bare", 401,
			`native: bare rejected the API key (HTTP 401); replace it with "craze auth login bare"`},
		{"no table", nil, "fireworks", 401,
			`native: fireworks rejected the API key (HTTP 401); replace it with "craze auth login fireworks"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// What the harness's classify makes of a refused key: a
			// ProviderError of kind ErrAuth (its kind is the harness's own;
			// the join stands in for it).
			pe := &harness.ProviderError{Provider: tc.provider, Model: tc.provider + "/m", Driver: modeltable.DriverOpenAICompat,
				StatusCode: tc.status, Message: "invalid key " + nativeCanary}
			err := errors.Join(pe, harness.ErrAuth)
			got := phraseTurnErrorIn(fmt.Errorf("turn: %w", err), tc.table)
			if got.Error() != tc.want || !errors.Is(got, harness.ErrAuth) {
				t.Fatalf("phrased %q\nwant    %q", got, tc.want)
			}
		})
	}
}

// TestRefusedText is plan 050 C3: a 403 on a key-funded driver other than
// Anthropic's is as often the account's as the key's — OpenAI's for an
// organization not verified for a model, or a region it does not serve — so
// it says the provider refused the request, the provider's own message first
// (sanitized, then redacted again, bounded), and the key's advice after it,
// its kind still ErrAuth. With no message the advice follows the status. A
// key the provider echoed split by a zero-width space or a soft hyphen —
// which the harness's whole-key scrub does not match, and sanitizing would
// join again — never appears; with no redactor to run after sanitizing, a
// message sanitizing changes is not shown at all. The control: a 401 on the
// same driver is LC-8's words, with no message. Negative control: sanitizing
// after the last redaction shows the canary in the split cases.
func TestRefusedText(t *testing.T) {
	table := &modeltable.Table{Providers: map[string]modeltable.Provider{
		"openai": {Name: "OpenAI", Driver: modeltable.DriverOpenAIResponses, EnvKeys: []string{"OPENAI_API_KEY"}},
	}}
	red := func(s string) string { return strings.ReplaceAll(s, nativeCanary, "[redacted]") }
	half := len(nativeCanary) / 2
	split := func(sep string) string { return nativeCanary[:half] + sep + nativeCanary[half:] }
	const advice = `if the API key is the problem, replace it with "craze auth login openai", or check OPENAI_API_KEY`
	for _, tc := range []struct {
		name   string
		status int
		msg    string
		red    func(string) string
		want   string
	}{
		{"a 403 with a message", 403, "Your organization must be verified to use the model `gpt-6-astra`.\x1b[2J", red,
			"native: OpenAI refused the request (HTTP 403): Your organization must be verified to use the model `gpt-6-astra`.; " + advice},
		{"a key split by a zero-width space", 403, "forbidden: " + split("\u200b"), red,
			"native: OpenAI refused the request (HTTP 403): forbidden: [redacted]; " + advice},
		{"a key split by a soft hyphen", 403, "forbidden: " + split("\u00ad"), red,
			"native: OpenAI refused the request (HTTP 403): forbidden: [redacted]; " + advice},
		{"a long message, bounded", 403, strings.Repeat("x", 400), red,
			"native: OpenAI refused the request (HTTP 403): " + strings.Repeat("x", maxRefusalMessage-len("…")) + "…; " + advice},
		{"no redactor, a plain message", 403, "Country, region, or territory not supported", nil,
			"native: OpenAI refused the request (HTTP 403): Country, region, or territory not supported; " + advice},
		{"no redactor, a message sanitizing changes", 403, "forbidden: " + split("\u200b"), nil,
			"native: OpenAI refused the request (HTTP 403); " + advice},
		{"a 403 with none", 403, "", red,
			"native: OpenAI refused the request (HTTP 403); " + advice},
		{"a 401 (control)", 401, "Incorrect API key provided: sk-proj-****0000.", red,
			`native: OpenAI rejected the API key (HTTP 401); replace it with "craze auth login openai", or check OPENAI_API_KEY`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pe := &harness.ProviderError{Provider: "openai", Model: "openai/gpt-6-astra", WireModel: "gpt-6-astra",
				Driver: modeltable.DriverOpenAIResponses, StatusCode: tc.status, Message: tc.msg}
			got := phraseTurnErrorRed(fmt.Errorf("turn: %w", errors.Join(pe, harness.ErrAuth)), table, tc.red)
			if got.Error() != tc.want || !errors.Is(got, harness.ErrAuth) {
				t.Fatalf("phrased %q\nwant    %q", got, tc.want)
			}
			if strings.Contains(got.Error(), nativeCanary) {
				t.Fatalf("the key is in %q", got)
			}
		})
	}
}

// TestNothingFundedTextNamesTheChatGPTPlan is SF-143: the start error of a
// native session with nothing funded says what craze auth login does — an
// API key, or the ChatGPT plan's sign-in — as craze providers' fix does, with
// the variables when there are any and without them when there are none.
func TestNothingFundedTextNamesTheChatGPTPlan(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, modeltable.ProvidersFile)
	withEnv := &modeltable.Table{
		Providers: map[string]modeltable.Provider{"a": {EnvKeys: []string{"A_KEY"}}, "b": {EnvKeys: []string{"B_KEY", "B2_KEY"}}},
		Models:    map[string]modeltable.Model{"a/1": {Provider: "a"}, "b/1": {Provider: "b"}, "b/2": {Provider: "b"}},
	}
	without := &modeltable.Table{
		Providers: map[string]modeltable.Provider{"a": {}},
		Models:    map[string]modeltable.Model{"a/1": {Provider: "a"}},
	}
	for _, tc := range []struct {
		name  string
		table *modeltable.Table
		want  string
	}{
		{"variables", withEnv, `native: no model provider has an API key — run "craze auth login" (an API key, or "craze auth login chatgpt" for a ChatGPT plan), or set one of A_KEY, B_KEY, or add api_key to ` + file},
		{"no variables", without, `native: no model provider has an API key — run "craze auth login" (an API key, or "craze auth login chatgpt" for a ChatGPT plan), or add api_key to ` + file},
	} {
		if got := nothingFundedText(tc.table, dir); got != tc.want {
			t.Errorf("%s: %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// TestAnthropicAccessTexts is plan 044 P27: on Anthropic's driver a 403 is a
// model the key may not use — "Anthropic refused access to <model>" with
// Anthropic's reason — and a 404 a wire id the account cannot use — "the
// Anthropic account cannot use <wire model>" with its reason — rather than
// the key's and models.toml's words, while a 401 is still the key's; each
// keeps its sentinel. The controls: the same 404 on another driver keeps the
// models.toml words byte for byte, and its 403 says the provider's own
// message before the key's advice (plan 050 C3, TestRefusedText).
func TestAnthropicAccessTexts(t *testing.T) {
	table := &modeltable.Table{Providers: map[string]modeltable.Provider{
		"anthropic": {Name: "Anthropic", Driver: modeltable.DriverAnthropic, EnvKeys: []string{"ANTHROPIC_API_KEY"}},
		"fireworks": {Name: "Fireworks", EnvKeys: []string{"FIREWORKS_API_KEY"}},
	}}
	anthropic := func(status int, msg string) *harness.ProviderError {
		return &harness.ProviderError{Provider: "anthropic", Model: "anthropic/claude-fable-5-1", WireModel: "claude-fable-5-1",
			Driver: modeltable.DriverAnthropic, StatusCode: status, Message: msg}
	}
	other := func(status int, msg string) *harness.ProviderError {
		return &harness.ProviderError{Provider: "fireworks", Model: "fireworks/m", WireModel: "m",
			Driver: modeltable.DriverOpenAICompat, StatusCode: status, Message: msg}
	}
	for _, tc := range []struct {
		name string
		pe   *harness.ProviderError
		is   error
		want string
	}{
		{"an Anthropic 403", anthropic(403, "This model requires data retention. (request req_1)"), harness.ErrAuth,
			`native: Anthropic refused access to model "anthropic/claude-fable-5-1" (HTTP 403): This model requires data retention. (request req_1)`},
		{"an Anthropic 403 with no reason", anthropic(403, ""), harness.ErrAuth,
			`native: Anthropic refused access to model "anthropic/claude-fable-5-1" (HTTP 403)`},
		{"an Anthropic 404", anthropic(404, "model: claude-fable-5-1"), harness.ErrModelNotFound,
			`native: the Anthropic account cannot use claude-fable-5-1 (HTTP 404: model: claude-fable-5-1)`},
		{"an Anthropic 404 with no reason", anthropic(404, ""), harness.ErrModelNotFound,
			`native: the Anthropic account cannot use claude-fable-5-1 (HTTP 404)`},
		{"an Anthropic 401", anthropic(401, "invalid x-api-key"), harness.ErrAuth,
			`native: Anthropic rejected the API key (HTTP 401); replace it with "craze auth login anthropic", or check ANTHROPIC_API_KEY`},
		{"another driver's 403 (control)", other(403, "forbidden"), harness.ErrAuth,
			`native: Fireworks refused the request (HTTP 403): forbidden; if the API key is the problem, replace it with "craze auth login fireworks", or check FIREWORKS_API_KEY`},
		{"another driver's 404 (control)", other(404, "no such model"), harness.ErrModelNotFound,
			`native: provider "fireworks" does not serve model "fireworks/m" (HTTP 404); check its wire_model in models.toml`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The kind is the harness's own (classify); the join stands in.
			got := phraseTurnErrorIn(fmt.Errorf("turn: %w", errors.Join(tc.pe, tc.is)), table)
			if got.Error() != tc.want || !errors.Is(got, tc.is) {
				t.Fatalf("phrased %q\nwant    %q", got, tc.want)
			}
		})
	}
}

// TestEveryProviderMessageIsRedactedAfterSanitizing (plan 050 review): every
// branch that shows a provider's own message — Anthropic's 403, Anthropic's
// model-not-found, and the default for a 429 or a 5xx, beside the 403 of
// TestRefusedText — redacts it after sanitizing as well as before, so a key
// the scrub did not know, split by a format character that sanitizing drops,
// never reads whole. The control: the message's other words still show.
func TestEveryProviderMessageIsRedactedAfterSanitizing(t *testing.T) {
	table := &modeltable.Table{Providers: map[string]modeltable.Provider{
		"anthropic": {Name: "Anthropic", Driver: modeltable.DriverAnthropic, EnvKeys: []string{"ANTHROPIC_API_KEY"}},
		"fireworks": {Name: "Fireworks", EnvKeys: []string{"FIREWORKS_API_KEY"}},
	}}
	red := func(s string) string { return strings.ReplaceAll(s, nativeCanary, "[redacted]") }
	half := len(nativeCanary) / 2
	for _, sep := range []string{"\u200b", "\u2060", "\ufeff", "\u00ad"} {
		msg := "denied: " + nativeCanary[:half] + sep + nativeCanary[half:]
		for _, tc := range []struct {
			name string
			pe   *harness.ProviderError
			is   error
		}{
			{"an Anthropic 403", &harness.ProviderError{Provider: "anthropic", Model: "anthropic/c", WireModel: "c",
				Driver: modeltable.DriverAnthropic, StatusCode: 403, Message: msg}, harness.ErrAuth},
			{"an Anthropic 404", &harness.ProviderError{Provider: "anthropic", Model: "anthropic/c", WireModel: "c",
				Driver: modeltable.DriverAnthropic, StatusCode: 404, Message: msg}, harness.ErrModelNotFound},
			{"a 429", &harness.ProviderError{Provider: "fireworks", Model: "fireworks/m", WireModel: "m",
				Driver: modeltable.DriverOpenAICompat, StatusCode: 429, Message: msg}, nil},
			{"a 500", &harness.ProviderError{Provider: "anthropic", Model: "anthropic/c", WireModel: "c",
				Driver: modeltable.DriverAnthropic, StatusCode: 500, Message: msg}, nil},
		} {
			t.Run(fmt.Sprintf("%s/U+%04X", tc.name, []rune(sep)[0]), func(t *testing.T) {
				var err error = tc.pe
				if tc.is != nil {
					err = errors.Join(tc.pe, tc.is)
				}
				got := phraseTurnErrorRed(fmt.Errorf("turn: %w", err), table, red).Error()
				if strings.Contains(got, nativeCanary) || !strings.Contains(got, "denied: [redacted]") {
					t.Fatalf("phrased %q; want the message shown with the key redacted", got)
				}
			})
		}
	}
}
