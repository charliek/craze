package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/store"
)

// TestClassify pins the mapping from what Fantasy's agent returns to the
// harness's errors, for both shapes a provider failure arrives in: the
// scrubbed *fantasy.ProviderError (before output) and the wrapper's
// *llm.MidStreamError (after).
func TestClassify(t *testing.T) {
	pe := func(status int, msg string) *fantasy.ProviderError {
		return &fantasy.ProviderError{Title: "t", Message: msg, StatusCode: status}
	}
	cases := []struct {
		name       string
		err        error
		kind       error // nil = a ProviderError of no kind
		wantStatus int
		wantMsg    string
	}{
		{"401", pe(401, "bad key"), ErrAuth, 401, "bad key"},
		{"403", pe(403, "forbidden"), ErrAuth, 403, "forbidden"},
		{"auth flag without a 401", &fantasy.ProviderError{Message: "expired", StatusCode: 400, AuthError: true}, ErrAuth, 400, "expired"},
		{"404", pe(404, "no such model"), ErrModelNotFound, 404, "no such model"},
		{"context too large", &fantasy.ProviderError{Message: "too long", StatusCode: 400, ContextTooLargeErr: true}, ErrContextTooLarge, 400, "too long"},
		{"context tokens reported", &fantasy.ProviderError{Message: "too long", StatusCode: 400, ContextMaxTokens: 10}, ErrContextTooLarge, 400, "too long"},
		{"other status", pe(500, "overloaded"), nil, 500, "overloaded"},
		{"no message falls back to the title", &fantasy.ProviderError{Title: "bad request", StatusCode: 400}, nil, 400, "bad request"},
		{"wrapped", fmt.Errorf("layer: %w", pe(404, "gone")), ErrModelNotFound, 404, "gone"},
		{"retried, judged by the last try", &fantasy.RetryError{Errors: []error{pe(503, "busy"), pe(401, "bad key")}}, ErrAuth, 401, "bad key"},
		{"mid-stream, no status", &llm.MidStreamError{Message: "stream error - boom"}, nil, 0, "stream error - boom"},
		{"mid-stream 401", &llm.MidStreamError{Message: "m", StatusCode: 401}, ErrAuth, 401, "m"},
		{"mid-stream auth flag", &llm.MidStreamError{Message: "m", AuthError: true}, ErrAuth, 0, "m"},
		{"mid-stream 404", &llm.MidStreamError{Message: "m", StatusCode: 404}, ErrModelNotFound, 404, "m"},
		{"anything else", errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), nil, 0, "dial tcp 127.0.0.1:1: connect: connection refused"},
	}
	m := store.Model{Provider: "test", Alias: "test/a", WireModel: "wire-a"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classify(tc.err, m)
			var got *ProviderError
			if !errors.As(err, &got) {
				t.Fatalf("classify = %#v, want a *ProviderError", err)
			}
			if got.StatusCode != tc.wantStatus || got.Message != tc.wantMsg || got.Provider != "test" || got.Model != "test/a" {
				t.Errorf("got %+v; want status %d, message %q, provider test, model test/a", got, tc.wantStatus, tc.wantMsg)
			}
			for _, kind := range []error{ErrAuth, ErrModelNotFound, ErrContextTooLarge} {
				if is := errors.Is(err, kind); is != (kind == tc.kind) {
					t.Errorf("errors.Is(err, %v) = %v", kind, is)
				}
			}
		})
	}
}

// ErrEmptyStep is its own error, not a provider failure.
func TestClassifyEmptyStep(t *testing.T) {
	err := classify(fmt.Errorf("fantasy: %w", llm.ErrEmptyStep), store.Model{Provider: "p", Alias: "a", WireModel: "w"})
	var pe *ProviderError
	if !errors.Is(err, ErrEmptyStep) || errors.As(err, &pe) {
		t.Fatalf("classify = %#v; want ErrEmptyStep and no ProviderError", err)
	}
	if !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("%q does not name the model", err)
	}
}

// A provider's message is shown on one line: a raw body — the SDK's fallback
// when it cannot parse the error — cannot bring newlines, terminal escapes,
// or kilobytes with it.
func TestClassifyBoundsTheMessage(t *testing.T) {
	body := "<html>\n<body>\x1b[31mBad\tGateway</body>\n</html>\n" + strings.Repeat("é", 400)
	err := classify(&fantasy.ProviderError{Message: body, StatusCode: 400}, store.Model{})
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("classify = %#v", err)
	}
	if len(pe.Message) > maxMessageBytes || !utf8.ValidString(pe.Message) || !strings.HasSuffix(pe.Message, "…") {
		t.Errorf("message is %d bytes (valid UTF-8 %v), want at most %d, valid, ending in an ellipsis: %q",
			len(pe.Message), utf8.ValidString(pe.Message), maxMessageBytes, pe.Message)
	}
	if want := "<html> <body> [31mBad Gateway</body> </html> éé"; !strings.HasPrefix(pe.Message, want) {
		t.Errorf("message = %q, want it to start %q", pe.Message, want)
	}
}

func TestOneLine(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"plain", 10, "plain"},
		{"  a\r\n\tb  \x00c\u0085d ", 20, "a b c d"},
		{"bad \xff utf8", 20, "bad utf8"},
		{"abcdefghij", 10, "abcdefghij"},
		{"abcdefghijk", 10, "abcdefg…"},
		{"ééééé", 8, "éé…"}, // 10 bytes; a cut at byte 5 would split a rune
		{"", 10, ""},
	}
	for _, tc := range cases {
		if got := oneLine(tc.in, tc.max); got != tc.want {
			t.Errorf("oneLine(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

// ProviderError's text names what the adapter needs and reads as a sentence.
func TestProviderErrorText(t *testing.T) {
	cases := []struct {
		err  *ProviderError
		want string
	}{
		{&ProviderError{Provider: "p", Model: "m", StatusCode: 401, Message: "bad key", kind: ErrAuth},
			`harness: the provider rejected the API key (provider "p", model "m", HTTP 401): bad key`},
		{&ProviderError{Provider: "p", Model: "m", Message: "boom"},
			`harness: provider error (provider "p", model "m"): boom`},
		{&ProviderError{Provider: "p", Model: "m", StatusCode: 500},
			`harness: provider error (provider "p", model "m", HTTP 500)`},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
	if errors.Is(&ProviderError{}, context.Canceled) {
		t.Error("a ProviderError of no kind unwraps to something")
	}
}
