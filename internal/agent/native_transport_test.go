package agent

import (
	"bytes"
	"testing"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

func TestNativeOpenAITransportUsesSealedEnv(t *testing.T) {
	const invalid = "native: CRAZE_OPENAI_TRANSPORT must be auto, websocket, or sse; ignoring it\n"
	for _, tc := range []struct {
		name, config, env, mode, warning string
	}{
		{"unset", "", "", "", ""},
		{"config only", "sse", "", "sse", ""},
		{"override", "sse", "  WebSocket  ", "websocket", ""},
		{"explicit auto", "websocket", "Auto", "auto", ""},
		{"invalid keeps config", "sse", "bad\nvalue", "sse", invalid},
		{"invalid keeps default", "", " ", "", invalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t)
			t.Setenv("CRAZE_OPENAI_TRANSPORT", "auto") // The harness's environment is authoritative.
			var options *harness.Options
			f.edit = func(o *harness.Options) { options = o }
			reads := 0
			f.getenv = func(key string) string {
				if key == "CRAZE_OPENAI_TRANSPORT" {
					reads++
					return tc.env
				}
				return f.env[key]
			}
			var diag bytes.Buffer
			s := f.started(Options{OpenAITransport: tc.config, Diag: &diag})
			if _, err := s.SetModel(t.Context(), "", "test/b"); err != nil {
				t.Fatal(err)
			}
			if options.OpenAITransport != tc.mode || reads != 1 {
				t.Fatalf("transport = %q after %d environment reads, want %q after one read", options.OpenAITransport, reads, tc.mode)
			}
			if diag.String() != tc.warning {
				t.Fatalf("diagnostic = %q, want %q", diag.String(), tc.warning)
			}
		})
	}
}

func TestNativeOpenAITransportWarnsOnNonOpenAIResponses(t *testing.T) {
	const warning = "native: websocket transport was requested, but this Responses model uses a non-OpenAI endpoint and stays on HTTP SSE\n"
	for _, tc := range []struct {
		name, provider, base string
	}{
		{"other Responses provider", "other", "https://api.meta.ai/v1"},
		{"OpenAI provider with a proxy", modeltable.OpenAIProvider, "https://proxy.example/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t)
			table := nativeTestTable("http://127.0.0.1:9/v1")
			table.Providers[tc.provider] = modeltable.Provider{
				Driver: modeltable.DriverOpenAIResponses, BaseURL: tc.base, APIKey: nativeCanaryOther,
			}
			m := table.Models["other/c"]
			m.Provider = tc.provider
			table.Models["other/c"] = m
			f.edit = func(o *harness.Options) { o.Table = table }
			var diag bytes.Buffer
			s := f.started(Options{Model: "other/c", OpenAITransport: "websocket", Diag: &diag})
			if got := diag.String(); got != warning {
				t.Fatalf("startup diagnostic = %q, want %q", got, warning)
			}
			diag.Reset()
			for _, alias := range []string{"test/a", "other/c"} {
				if _, err := s.SetModel(t.Context(), "", alias); err != nil {
					t.Fatal(err)
				}
			}
			if got := diag.String(); got != warning {
				t.Fatalf("switch diagnostic = %q, want %q", got, warning)
			}
		})
	}
}
