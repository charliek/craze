package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// Anthropic's driver through the native adapter (plan 044 §3.4): the API
// base the adapter hands the harness — the test override in craze's own
// environment, CRAZE_TEST_ANTHROPIC_API (P21) — and the note a refused turn
// ends with (P6). Every session here is built by the real llm factory; none
// starts until the adapter is seen to honour the override, so no request can
// reach Anthropic. The key is a canary.

const (
	nativeAnthropicKey   = "sk-ant-canary-not-a-secret-0002"
	nativeAnthropicModel = "anthropic/claude-test-5"
)

// anthropicNative starts a native session on a Claude model, built by the
// real llm factory, whose requests go to api. It first shows the adapter
// reads the override: one that is not a loopback URL fails the start model's
// build (messagesapi.ErrOverride) — which an adapter that left the harness on
// Anthropic's own base would not — and only then aims it at api.
func anthropicNative(t *testing.T, api *fakeEndpoint) *nativeSession {
	t.Helper()
	f := newNativeFixture(t)
	table := nativeTestTable("http://127.0.0.1:9/v1")
	table.Providers[modeltable.AnthropicProvider] = modeltable.Provider{Driver: modeltable.DriverAnthropic, EnvKeys: []string{"NATIVE_ANTHROPIC_KEY"}}
	table.Models[nativeAnthropicModel] = modeltable.Model{Provider: modeltable.AnthropicProvider, WireModel: "claude-test-5", Name: "Claude Test 5",
		ContextWindow: 200000, MaxOutputTokens: 8000, Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium"}
	table.DefaultModel = nativeAnthropicModel
	if err := modeltable.Save(f.dir, table); err != nil {
		t.Fatal(err)
	}
	f.env["NATIVE_ANTHROPIC_KEY"] = nativeAnthropicKey
	f.edit = func(o *harness.Options) { o.NewModel = nil }

	t.Setenv(messagesapi.TestAPIEnv, "http://example.com")
	refused := f.session(Options{})
	if err := refused.Start(context.Background()); !errors.Is(err, messagesapi.ErrOverride) {
		t.Fatalf("a session started with %s naming no loopback URL: Start = %v; want %v — the adapter does not hand the harness the override, so a request would reach Anthropic",
			messagesapi.TestAPIEnv, err, messagesapi.ErrOverride)
	}
	t.Setenv(messagesapi.TestAPIEnv, api.srv.URL)
	return f.started(Options{})
}

// anthropicEvent writes one event of the Messages stream: its type, and
// fields, the rest of its JSON object ("" for none).
func anthropicEvent(w io.Writer, typ, fields string) {
	if fields != "" {
		fields = "," + fields
	}
	fmt.Fprintf(w, "event: %s\ndata: {\"type\":%q%s}\n\n", typ, typ, fields)
}

// anthropicText answers with one text block, stopping for stop with details
// (a JSON object, or "null").
func anthropicText(text, stop, details string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		anthropicEvent(w, "message_start", `"message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test-5","content":[],"usage":{"input_tokens":10,"output_tokens":1}}`)
		anthropicEvent(w, "content_block_start", `"index":0,"content_block":{"type":"text","text":""}`)
		anthropicEvent(w, "content_block_delta", fmt.Sprintf(`"index":0,"delta":{"type":"text_delta","text":%q}`, text))
		anthropicEvent(w, "content_block_stop", `"index":0`)
		anthropicEvent(w, "message_delta", fmt.Sprintf(`"delta":{"stop_reason":%q,"stop_details":%s},"usage":{"output_tokens":5}`, stop, details))
		anthropicEvent(w, "message_stop", "")
	}
}

// TestNativeRefusalNoteNamesTheCategory (§3.4, P6, O5): a turn the model
// refuses ends with the session's note, after the model's own text and
// before the ending, naming the category Anthropic gave — "(refusal)" when
// it gave none, or something that is no short identifier — and the stop
// reason stays refusal. The note is the session's, not the model's: the next
// turn's request replays the model's text and not the note (the control:
// that request does carry the refused turn's text, so the note's absence is
// not the turn's). Every request reached the fake, through the override.
func TestNativeRefusalNoteNamesTheCategory(t *testing.T) {
	const said = "I can't help with that."
	for _, tc := range []struct {
		name, details, want string
	}{
		{"a category", `{"type":"refusal","category":"cyber"}`, "refusal category: cyber"},
		{"none", "null", "refusal"},
		{"not an identifier", `{"type":"refusal","category":"Cyber \u001b[31m"}`, "refusal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeEndpoint(t)
			s := anthropicNative(t, api)
			api.queue(anthropicText(said, "refusal", tc.details))
			res, err := s.Prompt(context.Background(), "do the thing")
			if err != nil || res.StopReason != harness.StopRefusal {
				t.Fatalf("Prompt = %+v, %v; want refusal", res, err)
			}
			evs := drained(s)
			endings(t, evs, harness.StopRefusal)
			note := "craze: the model declined this request (" + tc.want + "). Switch models with /model, or rephrase."
			if got := joined(evs, EventText); got != said+"\n\n"+note {
				t.Fatalf("text %q\nwant %q", got, said+"\n\n"+note)
			}
			texts := ofType(evs, EventText)
			if last := texts[len(texts)-1]; last.Text != "\n\n"+note {
				t.Fatalf("the last text is %q, want the note alone", last.Text)
			}
			if evs[len(evs)-1].Type != EventDone {
				t.Fatalf("the last event is %s, want the ending after the note", evs[len(evs)-1].Type)
			}

			api.queue(anthropicText("Fine.", "end_turn", "null"))
			if _, err := s.Prompt(context.Background(), "thanks"); err != nil {
				t.Fatal(err)
			}
			reqs := api.requests()
			if len(reqs) != 2 {
				t.Fatalf("the fake saw %d requests, want 2", len(reqs))
			}
			for _, r := range reqs {
				if r.path != "/v1/messages" || r.header.Get("X-Api-Key") != nativeAnthropicKey {
					t.Fatalf("a request to %s with the wrong key: not the session's", r.path)
				}
			}
			next := reqs[1].body
			if !strings.Contains(next, said) {
				t.Fatalf("control: the next request does not replay the refused turn's text:\n%s", next)
			}
			if strings.Contains(next, "declined this request") || strings.Contains(next, "Switch models") {
				t.Fatalf("the next request carries the session's note:\n%s", next)
			}
		})
	}
}

// TestNativeWakeRefusalIsNoted (§3.4): a wake — the session's own turn that
// delivers a background result — that the model refuses ends with the same
// note as a prompt's turn, inside the wake's brackets and before its ending.
// The control: a wake that answers has no note.
func TestNativeWakeRefusalIsNoted(t *testing.T) {
	for _, refused := range []bool{true, false} {
		t.Run(fmt.Sprintf("refused=%v", refused), func(t *testing.T) {
			rig := newWakeRig(t, Options{}, nil)
			child := newHeld(t)
			finish, want := fantasy.FinishReasonStop, "noted"
			if refused {
				finish = fantasy.FinishReasonContentFilter
				want = "noted\n\ncraze: the model declined this request (refusal). Switch models with /model, or rephrase."
			}
			id := rig.spawnOne(child, reply(textParts("noted"), finishParts(finish)))
			rig.finish(child, id)
			rig.awaitDecided(true, "the result pending")
			rig.bracket(false, 1, "wake-1")
			rig.noTerminalAfterTheFirst(1)
			evs := rig.w.events()
			opening, ending := indexWhere(evs, 0, isBracket(true)), indexWhere(evs, 0, isBracket(false))
			if got := joined(evs[opening:ending], EventText); got != want {
				t.Fatalf("the wake's text is %q, want %q", got, want)
			}
		})
	}
}
