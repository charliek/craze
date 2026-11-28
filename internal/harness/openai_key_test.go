package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The OpenAI key's provider in the harness (plan 050 C3): a message's phase
// from the stream to the transcript, through a reopen, to the replayed
// request; and the failures that are OpenAI's — a quota's 429 ended at once
// as quota, a model it does not take named by its code — through the real
// driver.

// onOpenAI points the fixture at an openai provider on the openai-responses
// driver at base, funded by TEST_API_KEY, with one model, openai/sol: the
// shape of the shipped provider, at a loopback server.
func onOpenAI(f *fixture, base string) {
	f.t.Helper()
	models := "version = 1\ncatalog = false\ndefault_model = \"openai/sol\"\n\n[models.\"openai/sol\"]\nprovider = \"openai\"\nwire_model = \"gpt-6.1-sol\"\nmax_output_tokens = 4096\n"
	if err := os.WriteFile(filepath.Join(f.home, modeltable.ModelsFile), []byte(models), 0o644); err != nil {
		f.t.Fatal(err)
	}
	writeProviders(f, fmt.Sprintf("version = 1\n\n[providers.openai]\ndriver = \"openai-responses\"\nbase_url = %q\nenv_keys = [\"TEST_API_KEY\"]\n", base))
}

// onShippedOpenAI points the fixture at the catalog's own GPT-6.1 Sol, its
// openai provider moved to base and funded by TEST_API_KEY: the catalog's
// model, on which the phase is kept (Resolved.CatalogModel).
func onShippedOpenAI(f *fixture, base string) {
	f.t.Helper()
	models := "version = 1\ndefault_model = \"openai/gpt-6.1-sol\"\n"
	if err := os.WriteFile(filepath.Join(f.home, modeltable.ModelsFile), []byte(models), 0o644); err != nil {
		f.t.Fatal(err)
	}
	writeProviders(f, fmt.Sprintf("version = 1\n\n[providers.openai]\nbase_url = %q\nenv_keys = [\"TEST_API_KEY\"]\n", base))
}

// onOwnOpenAI is onOpenAI over the catalog: a model of the user's own,
// mine/sol, on the user's override of the shipped openai provider — the shape
// a providers.toml and models.toml written before craze shipped openai have,
// which an older craze could have a session on.
func onOwnOpenAI(f *fixture, base string) {
	f.t.Helper()
	models := "version = 1\ndefault_model = \"mine/sol\"\n\n[models.\"mine/sol\"]\nprovider = \"openai\"\nwire_model = \"gpt-6.1-sol\"\nmax_output_tokens = 4096\n"
	if err := os.WriteFile(filepath.Join(f.home, modeltable.ModelsFile), []byte(models), 0o644); err != nil {
		f.t.Fatal(err)
	}
	writeProviders(f, fmt.Sprintf("version = 1\n\n[providers.openai]\ndriver = \"openai-responses\"\nbase_url = %q\nenv_keys = [\"TEST_API_KEY\"]\n", base))
}

// rsAnswer is a message written in phase.
func rsAnswer(id, text, phase string) map[string]any {
	m := rsMessage(id, text)
	m["phase"] = phase
	return m
}

// assistantPhases is each assistant message's phase in a request's input,
// nil where it sends none.
func assistantPhases(t *testing.T, body []byte) []any {
	t.Helper()
	var out []any
	for _, it := range rsInput(t, body) {
		if it["type"] == "message" && it["role"] == "assistant" {
			out = append(out, it["phase"])
		}
	}
	return out
}

// rsTextItem is the expected wire shape of a one-part message.
func rsTextItem(role, kind, text string) map[string]any {
	return map[string]any{"type": "message", "role": role,
		"content": []any{map[string]any{"type": kind, "text": text}}}
}

// openAIDeveloperInput verifies the prompt prefix and returns the remaining input.
func openAIDeveloperInput(t *testing.T, body []byte, system, digest string) []map[string]any {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["instructions"]; exists {
		t.Fatal("OpenAI request also sent top-level instructions")
	}
	var input []map[string]any
	if err := json.Unmarshal(fields["input"], &input); err != nil {
		t.Fatal(err)
	}
	if len(input) == 0 {
		t.Fatal("OpenAI request sent no input")
	}
	equal(t, "developer prefix", input[0], rsTextItem("developer", "input_text", system))
	sent := input[0]["content"].([]any)[0].(map[string]any)["text"].(string)
	equal(t, "digest of system prompt sent to OpenAI", promptDigest(sent), digest)
	for _, item := range input[1:] {
		if item["role"] == "developer" || item["role"] == "system" {
			t.Fatalf("unexpected prompt item after the developer prefix: %v", item)
		}
	}
	return input[1:]
}

func TestOpenAISummarizersKeepDeveloperPrefix(t *testing.T) {
	api := newRsAPI(t,
		rsReply(rsReasoning("rs_1", "sealed-1"), rsAnswer("msg_1", "hello back", "final_answer")),
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":"context_length_exceeded","message":"context length exceeded"}}`)
		},
		rsReply(rsAnswer("msg_2", longSummary("1. Request and intent\nSaid hello."), "final_answer")),
	)
	f := newFixture(t, api.base())
	onShippedOpenAI(f, api.base())
	s := f.open(realOptions(f))
	run(t, s, "hello")
	digest := transcript(t, s).Header.SystemPromptSHA256
	if _, err := s.Compact(t.Context(), "", "/compact", nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	reqs := api.requests()
	if len(reqs) != 3 {
		t.Fatalf("got %d requests, want turn, aligned summary, and text summary", len(reqs))
	}
	turn := openAIDeveloperInput(t, reqs[0], s.system, digest)
	equal(t, "turn input", turn, []map[string]any{rsTextItem("user", "input_text", "hello")})
	aligned := openAIDeveloperInput(t, reqs[1], s.system, digest)
	answer := rsTextItem("assistant", "output_text", "hello back")
	answer["phase"] = "final_answer"
	equal(t, "aligned summarizer input", aligned, []map[string]any{
		turn[0], rsReasoning("rs_1", "sealed-1"), answer,
		rsTextItem("user", "input_text", s.compactionPromptText("", s.redactor())),
	})
	text := openAIDeveloperInput(t, reqs[2], s.system, digest)
	equal(t, "text summarizer input shape", rsShape(text), []string{"message/user"})
	content := fmt.Sprint(text[0]["content"])
	for _, want := range []string{"[User]: hello", "[Assistant]: hello back", compactionPrompt} {
		if !strings.Contains(content, want) {
			t.Errorf("text summarizer input is missing %q", want)
		}
	}
	equal(t, "prompt digest after compaction", s.PromptSHA256(), digest)
}

// TestOpenAIMessagePhaseSurvivesAReopen: a turn on the catalog's own model on
// the OpenAI key's provider — a commentary message beside a tool call, then
// the final answer — replays the commentary's phase on its next step; the
// transcript keeps both phases in the text parts' metadata; and a reopened
// session's first request replays both, as the live session would. On the
// catalog model the developer prefix, digest and replayed history are also
// checked across the reopen. The controls run the same turns on another
// provider on the driver (Meta's shape,
// its stream carrying the phases), or on a model of the user's own on a
// provider named openai — alone, or over the catalog's overridden one (plan
// 050 review) — store no phase metadata at all — the transcript is what it
// was before phases were read, so an older craze still opens it — and send
// none. Negative controls: a transcript that loses the metadata replays no
// phase after the reopen; one that keeps it on every route fails the
// controls.
func TestOpenAIMessagePhaseSurvivesAReopen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fixture, string)
		sends bool
	}{
		{"the catalog's model on the OpenAI key's provider", onShippedOpenAI, true},
		{"another provider on the driver (control)", onKeyRoute, false},
		{"the user's own model on a provider named openai (control)", onOpenAI, false},
		{"the user's own model on the overridden openai provider (control)", onOwnOpenAI, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newRsAPI(t,
				rsReply(rsReasoning("rs_1", "sealed-1"), rsAnswer("msg_1", "Reading a.txt.", "commentary"), rsCall("call_01", "read", `{"filePath":"a.txt"}`)),
				rsReply(rsAnswer("msg_2", "It says alpha.", "final_answer")),
				rsReply(rsAnswer("msg_3", "Still alpha.", "final_answer")),
			)
			f := newFixture(t, api.base())
			tc.setup(f, api.base())
			f.put("a.txt", "alpha\n")
			s, err := Open(realOptions(f))
			if err != nil {
				t.Fatal(err)
			}
			run(t, s, "what is in a.txt?")
			digest := transcript(t, s).Header.SystemPromptSHA256
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(s.store.Path())
			if err != nil {
				t.Fatal(err)
			}
			for _, phase := range []string{"commentary", "final_answer"} {
				if want := `"craze.responses.message":{"type":"craze.responses.message","data":{"phase":"` + phase + `"}}`; strings.Contains(string(raw), want) != tc.sends {
					t.Fatalf("the transcript keeps %s: %v, want %v:\n%s", want, !tc.sends, tc.sends, raw)
				}
			}
			if !tc.sends && strings.Contains(string(raw), "craze.responses.message") {
				t.Fatalf("a transcript off the openai provider holds the phase's metadata type:\n%s", raw)
			}
			r := resumed(t, resumeOptions(realOptions(f), s.ID()))
			run(t, r, "and now?")
			reqs := api.requests()
			if len(reqs) != 3 {
				t.Fatalf("%d requests, want 3", len(reqs))
			}
			want := [][]any{{"commentary"}, {"commentary", "final_answer"}}
			if !tc.sends {
				want = [][]any{{nil}, {nil, nil}}
			}
			if got := assistantPhases(t, reqs[1]); !slices.Equal(got, want[0]) {
				t.Fatalf("the turn's second step replays phases %v, want %v", got, want[0])
			}
			if got := assistantPhases(t, reqs[2]); !slices.Equal(got, want[1]) {
				t.Fatalf("the reopened session's request replays phases %v, want %v", got, want[1])
			}
			if tc.sends {
				equal(t, "resumed prompt digest", r.PromptSHA256(), digest)
				step := openAIDeveloperInput(t, reqs[1], s.system, digest)
				replay := openAIDeveloperInput(t, reqs[2], s.system, digest)
				equal(t, "step history shape", rsShape(step), []string{
					"message/user", "reasoning/rs_1", "message/assistant", "function_call/call_01", "function_call_output/call_01",
				})
				equal(t, "encrypted reasoning", step[1], rsReasoning("rs_1", "sealed-1"))
				final := rsTextItem("assistant", "output_text", "It says alpha.")
				final["phase"] = "final_answer"
				equal(t, "history replayed after reopening", replay, append(step, final, rsTextItem("user", "input_text", "and now?")))
			}
		})
	}
}

// TestOpenAIFailuresThroughTheDriver: through the real driver on the OpenAI
// key's provider, a 429 naming OpenAI's spent credit ends the turn after one
// request — no retry in the turn — as kind quota, not retryable; a 400 and a
// 403 whose code is model_not_found are kind model_not_found; a 403 of
// another code stays auth. Negative control: without the 429 made final the
// turn sends the request again.
func TestOpenAIFailuresThroughTheDriver(t *testing.T) {
	reply := func(status int, code, typ string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"message":"refused","type":%q,"param":null,"code":%q}}`, typ, code)
		}
	}
	for _, tc := range []struct {
		name  string
		reply http.HandlerFunc
		kind  errkind.Kind
		is    error
	}{
		{"a 429 of spent credit", reply(429, "credit_balance_exhausted", "insufficient_quota"), errkind.Quota, nil},
		{"a 429 of a project's spend limit, by its code alone", reply(429, "project_spend_limit_exceeded", "requests"), errkind.Quota, nil},
		{"a 400 of a model the API does not take", reply(400, "model_not_found", "invalid_request_error"), errkind.ModelNotFound, ErrModelNotFound},
		{"a 403 of a model the project may not use", reply(403, "model_not_found", "invalid_request_error"), errkind.ModelNotFound, ErrModelNotFound},
		{"a 403 of another code", reply(403, "unsupported_country_region_territory", "request_forbidden"), errkind.Auth, ErrAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newRsAPI(t, tc.reply, tc.reply, tc.reply, tc.reply)
			f := newFixture(t, api.base())
			onOpenAI(f, api.base())
			s, err := Open(realOptions(f))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			_, err = s.Run(context.Background(), "go", nil)
			info, ok := errkind.Of(err)
			if !ok || info.Kind != tc.kind || info.Retryable {
				t.Fatalf("Run = %v: %+v (%v), want %s, not retryable", err, info, ok, tc.kind)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("Run = %v, want %v", err, tc.is)
			}
			if n := len(api.requests()); n != 1 {
				t.Fatalf("%d requests, want 1", n)
			}
		})
	}
}
