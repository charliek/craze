package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// TestErrorInfoAtTheSeam: what every turn's failure says it is as it crosses
// into the engine, whichever provider it came from.
func TestErrorInfoAtTheSeam(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		kind      errkind.Kind
		retryable bool
	}{
		{"an agent's RPC error", &acp.RPCError{Code: -32000, Message: "boom"}, errkind.Agent, false},
		{"an agent flagging it retryable", &acp.RPCError{Code: -32000, Message: "boom", Data: []byte(`{"retryable":true}`)}, errkind.Agent, true},
		{"an RPC error's data saying false", &acp.RPCError{Code: -32000, Data: []byte(`{"retryable":false}`)}, errkind.Agent, false},
		{"an RPC error's data saying something else", &acp.RPCError{Code: -32000, Data: []byte(`"retryable"`)}, errkind.Agent, false},
		{"an RPC -32602 in a turn is no model refusal", &acp.RPCError{Code: acp.CodeInvalidParams}, errkind.Agent, false},
		{"an agent connection closed", acp.ErrClosed, errkind.Agent, false},
		{"an agent exiting non-zero", fmt.Errorf("acp: %w", &acp.ExitError{}), errkind.Agent, false},
		{"the agent's own turn", ErrForeignTurn, errkind.Agent, true},
		{"a prompt in flight", ErrPromptInFlight, errkind.Agent, true},
		{"a native provider's", &harness.ProviderError{StatusCode: 503}, errkind.Provider, true},
		{"a native provider's, phrased", phraseTurnError(&harness.ProviderError{StatusCode: 429}), errkind.Provider, true},
		{"an empty step", phraseTurnError(&harness.EmptyStepError{Model: "m"}), errkind.Provider, false},
		{"the harness closed", phraseTurnError(harness.ErrClosed), errkind.Stopped, false},
		{"an empty prompt", phraseTurnError(harness.ErrEmptyPrompt), errkind.Prompt, false},
		{"a frozen key", phraseTurnError(harness.ErrStoredKeyFrozen), errkind.Config, false},
		{"no key", harness.ErrNoAPIKey, errkind.Auth, false},
		{"an unknown model", harness.ErrUnknownModel, errkind.Config, false},
		{"the plan's limit", chatgptauth.ErrUsageLimited, errkind.Quota, false},
		{"the plan's sign-in", chatgptauth.ErrSignInAgain, errkind.Auth, false},
		{"the plan off", modeltable.ErrPlanUsageDisabled, errkind.Config, false},
		{"the caller's context", context.Canceled, errkind.Interrupted, false},
		{"anything else", errors.New("a journal that could not be saved"), errkind.Internal, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ErrorInfo(tc.err)
			if got.Kind != tc.kind || got.Retryable != tc.retryable {
				t.Fatalf("%+v, want %s, retryable %v", got, tc.kind, tc.retryable)
			}
		})
	}
	if got := ErrorInfo(nil); got != (errkind.Info{}) {
		t.Fatalf("nil: %+v", got)
	}
}

// TestARemoteErrorNeverMakesUpAKind: an error that came through the codec says
// what it said, and nothing when an older host's said nothing, whatever its
// class's sentinel would otherwise classify as.
func TestARemoteErrorNeverMakesUpAKind(t *testing.T) {
	old := &RemoteError{Message: "x", Class: EventErrClosed}
	if got := ErrorInfo(old); got != (errkind.Info{}) {
		t.Fatalf("an older host's: %+v", got)
	}
	if got := ErrorInfo(fmt.Errorf("wrapped: %w", old)); got != (errkind.Info{}) {
		t.Fatalf("wrapped: %+v", got)
	}
	want := errkind.Info{Kind: errkind.Quota, Status: 429, ProviderCode: "insufficient_quota"}
	if got := ErrorInfo(&RemoteError{Class: EventErrClosed, Info: want}); got != want {
		t.Fatalf("a newer host's: %+v", got)
	}
}

// TestEventCodecCarriesWhatAnErrorSaid: an error's and a turn's four new fields
// through a round trip, a re-encode included; and absent at their zero values, so
// an event with none reads as an older host's wrote it.
func TestEventCodecCarriesWhatAnErrorSaid(t *testing.T) {
	ev := Event{Type: EventError, Err: &harness.ProviderError{StatusCode: 429, Code: "rate_limit_exceeded"}}
	body, err := EncodeEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"error","err":{"message":"harness: provider error (provider \"\", model \"\", HTTP 429)","class":"provider","code":429,"kind":"provider","retryable":true,"status":429,"provider_code":"rate_limit_exceeded"}}`
	if body != want {
		t.Fatalf("encoded\n got %s\nwant %s", body, want)
	}
	decoded, err := DecodeEvent(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := ErrorInfo(decoded.Err); got != (errkind.Info{Kind: errkind.Provider, Retryable: true, Status: 429, ProviderCode: "rate_limit_exceeded"}) {
		t.Fatalf("decoded: %+v", got)
	}
	if again, err := EncodeEvent(decoded); err != nil || again != body {
		t.Fatalf("encoded again: %s (%v)", again, err)
	}

	tu := &TurnInfo{ID: "turn-1", Phase: TurnEnded, Err: "boom", ErrClass: EventErrProvider,
		Info: errkind.Info{Kind: errkind.Quota, Retryable: true, Status: 402, ProviderCode: "insufficient_quota"}}
	body, err = EncodeEvent(Event{Type: EventTurn, Turn: tu})
	if err != nil {
		t.Fatal(err)
	}
	want = `{"type":"turn","turn":{"id":"turn-1","phase":"ended","errClass":"provider","err":"boom","kind":"quota","retryable":true,"status":402,"provider_code":"insufficient_quota"}}`
	if body != want {
		t.Fatalf("a turn encoded\n got %s\nwant %s", body, want)
	}
	if decoded, err = DecodeEvent(body); err != nil || decoded.Turn.Info != tu.Info {
		t.Fatalf("a turn decoded: %+v (%v)", decoded.Turn, err)
	}
	if body, err = EncodeEvent(Event{Type: EventTurn, Turn: &TurnInfo{ID: "turn-1", Phase: TurnEnded, StopReason: "end_turn"}}); err != nil ||
		body != `{"type":"turn","turn":{"id":"turn-1","phase":"ended","stopReason":"end_turn"}}` {
		t.Fatalf("a turn with no failure: %s (%v)", body, err)
	}
}

// TestStartErrorKind: a start's failure keeps a kind another attempt would meet
// again, whose default never claims a retry would help; and any other is
// start_failed, whose default does.
func TestStartErrorKind(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		kind      errkind.Kind
		retryable bool
	}{
		{"no key", phraseSetupError(harness.ErrNoAPIKey, nil, "m"), errkind.Auth, false},
		{"not signed in", phraseSetupError(modeltable.ErrNotSignedIn, nil, "m"), errkind.Auth, false},
		{"an unknown model", phraseSetupError(harness.ErrUnknownModel, nil, "m"), errkind.Config, false},
		{"an agent's model refusal", startModelRefusal("nope", &acp.RPCError{Code: acp.CodeInvalidParams, Data: []byte(`"unknown model id"`)}), errkind.ModelNotFound, false},
		{"nothing funded, with no key's error to say so", &nativeError{msg: "x", cause: modeltable.ErrNothingFunded}, errkind.Auth, false},
		{"an agent's RPC error", &acp.RPCError{Code: -32000, Message: "authentication failed"}, errkind.StartFailed, true},
		{"an agent that died", fmt.Errorf("x: %w", &acp.ExitError{}), errkind.StartFailed, true},
		{"anything else", errors.New("boom"), errkind.StartFailed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StartErrorKind(tc.err); got != tc.kind || got.Retryable() != tc.retryable {
				t.Fatalf("%s, want %s, retryable %v", got, tc.kind, tc.retryable)
			}
		})
	}
}

// TestStartModelRefusal: cursor's and grok's -32602 for a --model at the start
// names the model and the reply's generic Message, and still classes as the RPC
// error it is. Any other failure is left as it was.
func TestStartModelRefusal(t *testing.T) {
	want := `the agent refused model "nope": Invalid params`
	for _, data := range []string{`{"message":"Invalid model value: nope"}`, `"unknown model id"`, ``} {
		err := startModelRefusal("nope", &acp.RPCError{Code: acp.CodeInvalidParams, Message: "Invalid params", Data: []byte(data)})
		if err.Error() != want {
			t.Errorf("%s: %q, want %q", data, err, want)
		}
		if class, code := classifyEventErr(err); class != EventErrRPC || code != acp.CodeInvalidParams {
			t.Errorf("%s: class %s, code %d", data, class, code)
		}
	}
	for _, err := range []error{errors.New("boom"), &acp.RPCError{Code: -32000, Message: "boom"}} {
		if got := startModelRefusal("nope", err); got != err {
			t.Errorf("%v became %v", err, got)
		}
	}
}

// TestStartModelRefusalKeepsTheAgentsDataOut: an agent's data is its own
// diagnostic, and may quote a request or a credential. None of it reaches the
// failure's message, on the event that carries it to a waiter included, whichever
// of the two shapes it came in.
func TestStartModelRefusalKeepsTheAgentsDataOut(t *testing.T) {
	const secret = "sk-secret-in-the-data"
	for _, data := range []string{
		`{"message":"request https://example.test/?api_key=` + secret + `; Authorization: Bearer ` + secret + `"}`,
		`"Authorization: Bearer ` + secret + `"`,
	} {
		err := startModelRefusal("nope", &acp.RPCError{Code: acp.CodeInvalidParams, Message: "Invalid params", Data: []byte(data)})
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the message holds the data: %q", err)
		}
		body, encErr := EncodeEvent(Event{Type: EventError, Err: err})
		if encErr != nil || strings.Contains(body, secret) {
			t.Errorf("the event holds the data: %s (%v)", body, encErr)
		}
	}
}

// TestANativeStartRefusedForItsConfiguration is the real path a --model and a
// model table take through a native Start: each refusal is what another attempt
// would meet again, so a config failure no retry helps, in the words it had. Any
// other failure to start stays one a retry may help.
func TestANativeStartRefusedForItsConfiguration(t *testing.T) {
	start := func(t *testing.T, f *nativeFixture, opts Options) error {
		t.Helper()
		err := f.session(opts).Start(context.Background())
		if err == nil {
			t.Fatal("the start succeeded")
		}
		return err
	}
	wantConfig := func(t *testing.T, err error, words string) {
		t.Helper()
		if got := StartErrorKind(err); got != errkind.Config || got.Retryable() {
			t.Fatalf("%v: %s", err, got)
		}
		if !strings.Contains(err.Error(), words) {
			t.Fatalf("%q does not say %q", err, words)
		}
	}
	t.Run("a --model no model answers to", func(t *testing.T) {
		f := newNativeFixture(t)
		wantConfig(t, start(t, f, Options{Model: "definitely-not-a-model"}), `unknown model "definitely-not-a-model" (the model table has `)
	})
	t.Run("a --model two models answer to", func(t *testing.T) {
		f := newNativeFixture(t)
		table := nativeTestTable("http://127.0.0.1:9/v1")
		a, b := table.Models["test/a"], table.Models["test/b"]
		a.Name, b.Name = "same", "same"
		table.Models["test/a"], table.Models["test/b"] = a, b
		if err := modeltable.Save(f.dir, table); err != nil {
			t.Fatal(err)
		}
		wantConfig(t, start(t, f, Options{Model: "same"}), `ambiguous model "same"`)
	})
	t.Run("a model table that is not TOML", func(t *testing.T) {
		f := newNativeFixture(t)
		if err := os.WriteFile(filepath.Join(f.dir, modeltable.ModelsFile), []byte("version = [\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := start(t, f, Options{})
		wantConfig(t, err, "not valid TOML")
		var fe *modeltable.FileError
		if !errors.As(err, &fe) {
			t.Fatalf("the file's error is gone from the chain: %v", err)
		}
	})
	t.Run("a model table with a key it does not know", func(t *testing.T) {
		f := newNativeFixture(t)
		if err := os.WriteFile(filepath.Join(f.dir, modeltable.ModelsFile), []byte("version = 1\nnope = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		wantConfig(t, start(t, f, Options{}), "unknown key")
	})
	t.Run("no directory to read a table from", func(t *testing.T) {
		f := newNativeFixture(t)
		f.edit = func(o *harness.Options) { o.Home = "" }
		wantConfig(t, start(t, f, Options{}), "no craze directory")
	})
	t.Run("a default effort no request can carry", func(t *testing.T) {
		f := newNativeFixture(t)
		table := nativeTestTable("http://127.0.0.1:9/v1")
		a := table.Models["test/a"]
		a.Efforts, a.DefaultEffort = []string{"turbo"}, "turbo"
		table.Models["test/a"] = a
		if err := modeltable.Save(f.dir, table); err != nil {
			t.Fatal(err)
		}
		wantConfig(t, start(t, f, Options{Model: "test/a"}), `effort "turbo" is not one`)
	})
	t.Run("a model whose build fails is no refusal", func(t *testing.T) {
		f := newNativeFixture(t)
		f.edit = func(o *harness.Options) {
			o.NewModel = func(modeltable.Resolved) (fantasy.LanguageModel, error) { return nil, errors.New("boom") }
		}
		if err := start(t, f, Options{}); StartErrorKind(err) != errkind.StartFailed {
			t.Fatalf("%v: %s", err, StartErrorKind(err))
		}
	})
}

// TestALoadOfAStoredSessionThatCannotBeLoaded: a retry reads the same file, so a
// damaged transcript, or a file that is none, is internal, and no retry helps. One
// another load holds is the exception: its holder may let go.
func TestALoadOfAStoredSessionThatCannotBeLoaded(t *testing.T) {
	const noHeaderID = "0199c0de-7777-7000-8000-000000007007"
	f := newNativeFixture(t)
	ws := t.TempDir()
	damaged := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("one")}, []step{answer("two")})
	path := storedPath(t, f, damaged)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(raw), "\n")
	lines[1] = "{not json\n"
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "20260918T120000_"+noHeaderID+".jsonl"), []byte("not a header\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	held := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("one")})
	if _, err := startLoad(t, f.session(Options{Workspace: ws, LoadSessionID: held})); err != nil {
		t.Fatalf("the holder's load: %v", err)
	}
	for _, tc := range []struct {
		name, id string
		is       error
		kind     errkind.Kind
		retry    bool
	}{
		{"damaged", damaged, store.ErrCorrupt, errkind.Internal, false},
		{"no header", noHeaderID, store.ErrNoHeader, errkind.Internal, false},
		{"held by another load", held, store.ErrBusy, errkind.StartFailed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := startLoad(t, f.session(Options{Workspace: ws, LoadSessionID: tc.id}))
			if !errors.Is(err, tc.is) {
				t.Fatalf("the load: %v, want %v", err, tc.is)
			}
			if got := StartErrorKind(err); got != tc.kind || got.Retryable() != tc.retry {
				t.Fatalf("%v: %s, want %s", err, got, tc.kind)
			}
		})
	}
	for _, sentinel := range []error{store.ErrNotResumable, store.ErrNewerTranscript, store.ErrBadSessionID} {
		if got := StartErrorKind(phraseLoadError(sentinel, nil, "", "id")); got != errkind.Internal {
			t.Errorf("%v: %s", sentinel, got)
		}
	}
}
