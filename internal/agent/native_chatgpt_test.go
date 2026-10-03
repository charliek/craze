package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool/opencode"
	"github.com/charliek/craze/internal/paths"
)

// The ChatGPT plan through the native adapter (plan 033 §3.12, C14). Every
// case runs in its own CRAZE_HOME, signed in by hand the way
// internal/chatgptauth leaves a directory — a token record of dummy tokens,
// at least eight bytes each and sharing nothing with the marker — and points
// both of chatgptauth's endpoints at loopback fakes (noOpenAI), so nothing
// can reach OpenAI whatever a case does.

const (
	planAccess    = "test-plan-access-0011"
	planRefresh   = "test-plan-refresh-0012"
	planIDToken   = "test-plan-idtoken-0013"
	planAccess2   = "test-plan-access-0014"
	planRefresh2  = "test-plan-refresh-0015"
	planUnlearned = "test-plan-unlearned-0016"
	planSubject   = "user-subject-0001"
	planClient    = "app_client-0001"
)

// fakeEndpoint is a loopback stand-in for one of chatgptauth's origins: each
// request is answered by the next queued reply, else 503, and recorded with
// its headers and body.
type fakeEndpoint struct {
	srv     *httptest.Server
	mu      sync.Mutex
	replies []http.HandlerFunc
	seen    []fakeRequest
}

type fakeRequest struct {
	path   string
	query  url.Values
	header http.Header
	body   string
}

func newFakeEndpoint(t *testing.T) *fakeEndpoint {
	t.Helper()
	e := &fakeEndpoint{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.seen = append(e.seen, fakeRequest{path: r.URL.Path, query: r.URL.Query(), header: r.Header.Clone(), body: string(b)})
		var next http.HandlerFunc
		if len(e.replies) > 0 {
			next, e.replies = e.replies[0], e.replies[1:]
		}
		e.mu.Unlock()
		if next == nil {
			http.Error(w, "nothing queued", http.StatusServiceUnavailable)
			return
		}
		next(w, r)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *fakeEndpoint) queue(replies ...http.HandlerFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.replies = append(e.replies, replies...)
}

func (e *fakeEndpoint) requests() []fakeRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]fakeRequest(nil), e.seen...)
}

// noOpenAI points chatgptauth's two endpoints at loopback fakes — the API at
// <api>/v1, the sign-in's issuer at its origin — and returns both.
func noOpenAI(t *testing.T) (api, issuer *fakeEndpoint) {
	t.Helper()
	api, issuer = newFakeEndpoint(t), newFakeEndpoint(t)
	t.Setenv(chatgptauth.APIEnv, api.srv.URL+"/v1")
	t.Setenv(chatgptauth.IssuerEnv, issuer.srv.URL)
	return api, issuer
}

// writePlanTokens writes dir's token record as chatgptauth leaves one — an
// hour of life left, so nothing refreshes it — by a rename, as chatgptauth
// writes it.
func writePlanTokens(t *testing.T, dir, access, refresh, incarnation string, generation int) {
	t.Helper()
	if err := writePlanTokensOf(dir, planSubject, planClient, access, refresh, incarnation, generation); err != nil {
		t.Fatal(err)
	}
}

// writePlanTokensOf is writePlanTokens for the account subject and client
// names, returning its failure rather than failing a test, so a fake
// server's handler can call it.
func writePlanTokensOf(dir, subject, client, access, refresh, incarnation string, generation int) error {
	if err := os.MkdirAll(chatgptauth.AuthDir(dir), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(map[string]any{
		"version": 1, "client_id": client, "issuer": "https://auth.openai.com", "subject": subject,
		"email": "someone@example.com", "scopes": []string{"openid", "offline_access", "chatgpt.tokens.use.direct"},
		"access_token": access, "access_expires_at": time.Now().Add(time.Hour).UTC(), "refresh_token": refresh,
		"id_token": planIDToken, "incarnation": incarnation, "generation": generation,
	})
	if err != nil {
		return err
	}
	tmp := chatgptauth.TokenFile(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, chatgptauth.TokenFile(dir))
}

// The other account of the account-binding tests (plan 033 C14r2), and its
// dummy tokens.
const (
	otherSubject = "user-subject-0002"
	otherClient  = "app_client-0002"
	otherAccess  = "test-plan-other-access-0017"
	otherRefresh = "test-plan-other-refresh-0018"
)

// signInOf signs dir in to the account subject and client names, as a sign-in
// in another process leaves it: that account's registration, with plan
// usage, and a token record of access and refresh, each written by a rename.
// The model list is left as it is, still the first account's: the process's
// running sessions never read it again.
func signInOf(dir, subject, client, access, refresh, incarnation string) error {
	b, err := json.Marshal(chatgptauth.Client{ClientID: client, Subject: subject, Email: "someone@example.com", PlanUsage: true, NoticeShown: true})
	if err != nil {
		return err
	}
	tmp := chatgptauth.ClientFile(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, chatgptauth.ClientFile(dir)); err != nil {
		return err
	}
	return writePlanTokensOf(dir, subject, client, access, refresh, incarnation, 1)
}

// writePlanAccount writes dir's registration, with plan usage, and its model
// list bound to it, fetched now: one model, gpt-5.6-sol. The list is
// chatgptauth's own type, so the shape the table reads is the one the sign-in
// writes (plan 033 X120).
func writePlanAccount(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(chatgptauth.AuthDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	client, err := json.Marshal(chatgptauth.Client{ClientID: planClient, Subject: planSubject, Email: "someone@example.com", PlanUsage: true, NoticeShown: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chatgptauth.ClientFile(dir), client, 0o600); err != nil {
		t.Fatal(err)
	}
	parallel := true
	models, err := json.Marshal(chatgptauth.Models{Version: 1, Subject: planSubject, ClientID: planClient, ClientVersion: modeltable.ChatGPTModelsClientVersion(), FetchedAt: time.Now().UTC(),
		Models: []chatgptauth.Model{{Slug: "gpt-5.6-sol", MinClientVersion: "0.144.0", DisplayName: "GPT-5.6 Sol", ContextWindow: 272000,
			Efforts: []string{"low", "medium", "high", "ultra"}, DefaultEffort: "medium", InputModalities: []string{"text"},
			Priority: 0, ParallelToolCalls: &parallel}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chatgptauth.ModelsFile(dir), models, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestChatGPTFilesAreOneContract (plan 033 X120): the sign-in's files have
// one name everywhere — chatgptauth writes them, modeltable reads them for
// the plan's models and funding, and the file tools refuse the directory —
// and the model list chatgptauth writes is one the table reads — its
// client_version and each model's minimal_client_version included (plan 034
// §3.1): the table drops a model whose minimum is above its own pin, and keeps
// one at it, and chatgptauth's tests' pin is the shipped catalog's.
func TestChatGPTFilesAreOneContract(t *testing.T) {
	dir := t.TempDir()
	for _, pair := range [][2]string{
		{chatgptauth.AuthDir(dir), filepath.Join(dir, modeltable.ChatGPTAuthDir)},
		{chatgptauth.AuthDir(dir), filepath.Join(dir, opencode.AuthDir)},
		{chatgptauth.TokenFile(dir), filepath.Join(dir, modeltable.ChatGPTAuthDir, modeltable.ChatGPTTokenFile)},
		{chatgptauth.ClientFile(dir), filepath.Join(dir, modeltable.ChatGPTAuthDir, modeltable.ChatGPTClientFile)},
		{chatgptauth.ModelsFile(dir), filepath.Join(dir, modeltable.ChatGPTModelsFile)},
	} {
		if pair[0] != pair[1] {
			t.Errorf("chatgptauth names %q, the other side %q", pair[0], pair[1])
		}
	}
	writePlanAccount(t, dir)
	writePlanTokens(t, dir, planAccess, planRefresh, "inc-1", 1)
	tbl, err := modeltable.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := tbl.Resolve("chatgpt/gpt-5.6-sol", func(string) string { return "" })
	if err != nil || r.Auth != modeltable.AuthSignIn || r.ParallelToolCalls == nil || !*r.ParallelToolCalls ||
		strings.Join(r.Efforts, ",") != "low,medium,high" {
		t.Fatalf("the model chatgptauth's list names = %+v, %v", r, err)
	}

	// The new fields, written by chatgptauth's own types, are the ones the
	// table reads: a model above the pin is not offered, one at the pin is.
	list, err := json.Marshal(chatgptauth.Models{Version: 1, Subject: planSubject, ClientID: planClient,
		ClientVersion: modeltable.ChatGPTModelsClientVersion(), FetchedAt: time.Now().UTC(),
		Models: []chatgptauth.Model{
			{Slug: "gpt-at-the-pin", ContextWindow: 1000, Efforts: []string{"low"}, MinClientVersion: modeltable.ChatGPTModelsClientVersion()},
			{Slug: "gpt-above-the-pin", ContextWindow: 1000, Efforts: []string{"low"}, MinClientVersion: "999.0.0", Priority: 1},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chatgptauth.ModelsFile(dir), list, 0o600); err != nil {
		t.Fatal(err)
	}
	if tbl, err = modeltable.Load(dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := tbl.Models["chatgpt/gpt-at-the-pin"]; !ok {
		t.Fatal("a model at the pin is not offered")
	}
	if _, ok := tbl.Models["chatgpt/gpt-above-the-pin"]; ok {
		t.Fatal("a model above the pin is offered: the table does not read chatgptauth's minimal_client_version")
	}
}

// planEvent is one server-sent event of the Responses stream.
func planEvent(w io.Writer, ev map[string]any) {
	b, _ := json.Marshal(ev)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
}

func planCompleted(w io.Writer) {
	planEvent(w, map[string]any{"type": "response.completed", "response": map[string]any{"id": "r1", "status": "completed", "output": []any{},
		"usage": map[string]any{"input_tokens": 20, "output_tokens": 5, "total_tokens": 25}}})
}

// planReadCall answers with one call of the read tool on path.
func planReadCall(path string) http.HandlerFunc {
	args := `{"filePath":"` + path + `"}`
	call := func(a string) map[string]any {
		return map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "read", "namespace": "craze", "arguments": a}
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		planEvent(w, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": call("")})
		planEvent(w, map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_1", "delta": args})
		planEvent(w, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": call(args)})
		planCompleted(w)
	}
}

// planAnswer answers with one message of text.
func planAnswer(text string) http.HandlerFunc {
	msg := func(content []any) map[string]any {
		return map[string]any{"type": "message", "id": "m1", "role": "assistant", "content": content}
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		planEvent(w, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": msg([]any{})})
		planEvent(w, map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "m1", "delta": text})
		planEvent(w, map[string]any{"type": "response.output_item.done", "output_index": 0,
			"item": msg([]any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}})})
		planCompleted(w)
	}
}

// planSession starts a native session in a fresh CRAZE_HOME signed in to the
// plan, with no API key in its environment: it starts on the plan's start
// model, chatgpt/gpt-5.6-sol, through the real llm factory.
func planSession(t *testing.T, ws string) (*nativeSession, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	dir := filepath.Join(home, "native")
	writePlanAccount(t, dir)
	writePlanTokens(t, dir, planAccess, planRefresh, "inc-1", 1)
	s := newNative(Options{Workspace: ws, ContentHome: t.TempDir()}, func(o *harness.Options) {
		o.Getenv = func(string) string { return "" }
	})
	closeAtCleanup(t, s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	takeStartDelta(t, s.log)
	return s, dir
}

// TestNativeChatGPTSessionCompletesAToolTurn (§3.12, A16, A19, A20's shape):
// a session signed in to the plan, with nothing else funded, starts on the
// plan's start model and completes a tool turn through the real driver
// against a fake Responses API: each request goes to <api>/v1/responses with
// the sign-in's bearer and the session's id as session-id (P18), the read's
// result goes back as the call's output, and the answer ends the turn. No
// request goes to the sign-in's issuer: the token was fresh.
func TestNativeChatGPTSessionCompletesAToolTurn(t *testing.T) {
	api, issuer := noOpenAI(t)
	api.queue(planReadCall("hello.txt"), planAnswer("the file says hello"))
	s, _ := planSession(t, nativeWorkspaceWith(t, map[string]string{"hello.txt": "hello world\n"}))
	snap := s.Snapshot()
	if snap.CurrentModel != "chatgpt/gpt-5.6-sol" {
		t.Fatalf("the session started on %q", snap.CurrentModel)
	}
	if !strings.Contains(fmt.Sprint(snap.Models), "GPT-5.6 Sol (ChatGPT plan)") {
		t.Fatalf("the picker offers %+v", snap.Models)
	}

	res, err := s.Prompt(context.Background(), "read hello.txt")
	if err != nil || res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v", res, err)
	}
	evs := drained(s)
	if got := joined(evs, EventText); got != "the file says hello" {
		t.Fatalf("the answer = %q", got)
	}
	reqs := api.requests()
	if len(reqs) != 2 {
		t.Fatalf("the API saw %d requests, want 2", len(reqs))
	}
	for i, r := range reqs {
		if r.path != "/v1/responses" || r.header.Get("Authorization") != "Bearer "+planAccess ||
			r.header.Get("session-id") != snap.SessionID || snap.SessionID == "" || !strings.Contains(r.body, `"model":"gpt-5.6-sol"`) {
			t.Fatalf("request %d: %s bearer-matches %t session-id %q (want %q) model-in-body %t", i, r.path,
				r.header.Get("Authorization") == "Bearer "+planAccess, r.header.Get("session-id"), snap.SessionID,
				strings.Contains(r.body, `"model":"gpt-5.6-sol"`))
		}
	}
	if !strings.Contains(reqs[1].body, "function_call_output") || !strings.Contains(reqs[1].body, "hello world") {
		t.Fatalf("the read's result did not go back: %s", reqs[1].body)
	}
	if n := len(issuer.requests()); n != 0 {
		t.Fatalf("the issuer saw %d requests", n)
	}
}

// TestNativeChatGPTSignInRefusedAfterItsRenewal (§3.12): the API refusing the
// bearer is answered by one renewal — a refresh at the sign-in's issuer — and
// a second refusal of the renewed token ends the turn in the plan's words:
// the sign-in is no longer valid. The control is the renewal itself, which
// the issuer saw once, and the renewed bearer, which the API saw.
func TestNativeChatGPTSignInRefusedAfterItsRenewal(t *testing.T) {
	api, issuer := noOpenAI(t)
	refused := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"subscription_sharing_invalid_user","message":"refused"}}`)
	}
	api.queue(refused, refused)
	issuer.queue(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"`+planAccess2+`","refresh_token":"`+planRefresh2+
			`","token_type":"Bearer","expires_in":3600,"scope":"openid offline_access chatgpt.tokens.use.direct"}`)
	})
	s, _ := planSession(t, t.TempDir())
	_, err := s.Prompt(context.Background(), "hi")
	if err == nil || err.Error() != chatgptSignInText {
		t.Fatalf("Prompt err = %v, want %q", err, chatgptSignInText)
	}
	if n := len(issuer.requests()); n != 1 {
		t.Fatalf("the issuer saw %d requests, want the one renewal", n)
	}
	reqs := api.requests()
	if len(reqs) != 2 || reqs[1].header.Get("Authorization") != "Bearer "+planAccess2 {
		t.Fatalf("the API saw %d requests (the second with the renewed bearer?)", len(reqs))
	}
	for _, v := range []string{planAccess, planAccess2, planRefresh, planRefresh2} {
		if l := nativeLeaks(err, v); len(l) > 0 {
			t.Fatalf("a token reached the error at %v", l)
		}
	}
}

// TestNativeRedactsATokenRotatedMidTurn (A21, P19): a token the process's
// token source adopts while a turn runs — here another process's refresh,
// adopted at this process's next look — reaches the session through its
// subscription (AddSecrets) before the token is used, and the same turn's
// next tool output is redacted of it. The control is the same turn with no
// adoption, whose output shows the value: the turn-start look ran before the
// file changed, and nothing else learns it in time (R9's window).
func TestNativeRedactsATokenRotatedMidTurn(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		t.Run(fmt.Sprintf("adopted %v", adopted), func(t *testing.T) {
			noOpenAI(t)
			f := newNativeFixture(t)
			writePlanAccount(t, f.dir)
			writePlanTokens(t, f.dir, planAccess, planRefresh, "inc-1", 1)
			ws := nativeWorkspaceWith(t, map[string]string{"token.txt": "now " + planAccess2 + " never " + planUnlearned + "\n"})
			a := f.models["test/a"]
			rotate := func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
				writePlanTokens(t, f.dir, planAccess2, planRefresh2, "inc-1", 2) // a peer's refresh
				if adopted {
					if _, _, err := chatgptauth.Source(f.dir).Token(ctx); err != nil {
						t.Errorf("Token: %v", err)
					}
				}
				readStep("c1", "token.txt")(ctx, yield)
			}
			a.push(rotate, answer("read"))
			s := f.started(Options{Workspace: ws})
			if _, err := s.Prompt(context.Background(), "read token.txt"); err != nil {
				t.Fatal(err)
			}
			content := readOf(t, drained(s), "t1.1.1")
			if strings.Contains(content, planAccess2) == adopted || !strings.Contains(content, planUnlearned) {
				t.Fatalf("adopted %v: the read = %q", adopted, content)
			}
		})
	}
}

// TestNativeLearnsTheTokenFileAtTurnStart (A21, P19): a session on another
// provider — scripted here, as a GLM session's — learns the plan's token file
// at a turn's start, so a cat of the file at the next turn shows the marker,
// not the tokens. The control is the turn during which the file was written,
// after its look: its cat shows them (R9's window, which the file tools'
// refusal and the turn-start look bound).
func TestNativeLearnsTheTokenFileAtTurnStart(t *testing.T) {
	noOpenAI(t)
	f := newNativeFixture(t)
	cat := nativeArgs(t, map[string]any{"command": "cat " + chatgptauth.TokenFile(f.dir)})
	a := f.models["test/a"]
	signIn := func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		writePlanAccount(t, f.dir)
		writePlanTokens(t, f.dir, planAccess, planRefresh, "inc-1", 1)
		nativeCallStep("c1", "bash", cat)(ctx, yield)
	}
	a.push(signIn, answer("saw it"), nativeCallStep("c2", "bash", cat), answer("saw it again"))
	s := f.started(Options{Workspace: t.TempDir()})

	if _, err := s.Prompt(context.Background(), "show the token file"); err != nil {
		t.Fatal(err)
	}
	first := rowByID(t, rowsOf(drained(s)), "t1.1.1")
	if first.Output == nil || !strings.Contains(first.Output.Stdout, planAccess) {
		t.Fatal("control: the cat during the turn the file appeared did not show the token, so its redaction later proves nothing")
	}
	if _, err := s.Prompt(context.Background(), "show it again"); err != nil {
		t.Fatal(err)
	}
	second := drained(s)
	row := rowByID(t, rowsOf(second), "t2.1.1")
	if row.Output == nil || !strings.Contains(row.Output.Stdout, redact.Marker) {
		t.Fatal("the next turn's cat of the token file shows no marker")
	}
	for _, v := range []string{planAccess, planRefresh, planIDToken} {
		if l := nativeLeaks(second, v); len(l) > 0 {
			t.Fatalf("a token reached the next turn's events at %v", l)
		}
	}
}

// TestNativeModelsRefreshJournalsNoEchoedToken (plan 033 C14r, r12 #1): the
// background refresh of the plan's model list, refused by an API that echoes a
// token as its error code — one the session learned (the bearer it sent), and
// one it never saw — journals its chatgpt_models_refresh diag with neither:
// the code is unrecognised, and so unnamed. The control is the diag itself,
// which is there and names the refusal, so the refresh ran and failed.
func TestNativeModelsRefreshJournalsNoEchoedToken(t *testing.T) {
	for _, tc := range []struct{ name, echoed string }{{"learned", planAccess}, {"never seen", planUnlearned}} {
		echoed := tc.echoed
		t.Run(tc.name, func(t *testing.T) {
			api, _ := noOpenAI(t)
			api.queue(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":"`+echoed+`","message":"refused"}}`)
			})
			home := t.TempDir()
			t.Setenv("CRAZE_HOME", home)
			dir := filepath.Join(home, "native")
			writePlanAccount(t, dir)
			writePlanTokens(t, dir, planAccess, planRefresh, "inc-1", 1)
			staleModels(t, dir)
			jdir := filepath.Join(t.TempDir(), "journal")
			s := newNative(Options{Workspace: t.TempDir(), ContentHome: t.TempDir(), JournalDir: jdir}, func(o *harness.Options) {
				o.Getenv = func(string) string { return "" }
			})
			closeAtCleanup(t, s)
			if err := s.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			jw, inc := journalOf(t, s.log), s.Incarnation()
			s.mu.Lock()
			refreshed := s.refreshed
			s.mu.Unlock()
			if refreshed == nil {
				t.Fatal("the stale model list started no refresh")
			}
			await(t, refreshed, "the model list's refresh")
			closeJournaled(t, s, jw)
			lines := assertOneJournal(t, jdir, jw, inc)
			notes := diags(lines, diagModelsRefresh)
			if len(notes) != 1 || !strings.Contains(fmt.Sprint(notes[0]["error"]), "models refused (HTTP 400, an unrecognised error code)") {
				t.Fatalf("the refresh's diags = %v, want the one refusal", notes)
			}
			for _, v := range []string{planAccess, planRefresh, planUnlearned} {
				if l := nativeLeaks(lines, v); len(l) > 0 {
					t.Fatalf("a token reached the journal at %v", l)
				}
			}
		})
	}
}

// TestNativeModelsRefreshNoteIsRedacted (plan 033 C14r, r12 #1): the diag a
// failed refresh is journaled as passes the error through the session's
// redactor, so a token a wrapped error might carry — one the session knows —
// is the marker there. The session learns it as the refresh would before it
// could fail: the source's first look adopts the file and tells its
// subscribers. The control is the same error through no redactor, whose note
// shows the value: the sanitiser alone hides nothing.
func TestNativeModelsRefreshNoteIsRedacted(t *testing.T) {
	noOpenAI(t)
	s, _ := planSession(t, t.TempDir())
	if _, _, err := s.signIn.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := fmt.Errorf("chatgptauth: a wrapped failure: %s", planAccess)
	if got := fmt.Sprint(modelsRefreshNote(err, func(v string) string { return v }).Fields["error"]); !strings.Contains(got, planAccess) {
		t.Fatalf("control: the unredacted note = %q", got)
	}
	got := fmt.Sprint(modelsRefreshNote(err, s.hs.Redact).Fields["error"])
	if strings.Contains(got, planAccess) || !strings.Contains(got, redact.Marker) {
		t.Fatalf("the note = %q, want the token redacted", got)
	}
}

// TestNativeModelsRefreshRefetchesAnotherClientVersion (plan 034 A1, Q3;
// review r6 #6): a session's open fetches the plan's model list again when
// the cache is fresh by age but was fetched with another client_version, or
// with none (a legacy file): the request carries the shipped catalog's pin,
// exactly once, and the list is written with it. The host's refresh has no
// observer, so it never writes the sign-in log: nothing is made under
// <native>/logs. The control is a cache fetched with the pin, whose open
// asks for nothing.
func TestNativeModelsRefreshRefetchesAnotherClientVersion(t *testing.T) {
	pin := modeltable.ChatGPTModelsClientVersion()
	for _, tc := range []struct {
		name, version string
		fetched       bool
	}{
		{"the pin (control)", pin, false},
		{"another version", "0.1.0", true},
		{"none, a legacy cache", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, _ := noOpenAI(t)
			api.queue(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-6.1-sol","display_name":"GPT-6.1 Sol","visibility":"list","priority":0,`+
					`"context_window":272000,"input_modalities":["text"],"supported_reasoning_levels":[{"effort":"low"}],"default_reasoning_level":"low"}]}`)
			})
			home := t.TempDir()
			t.Setenv("CRAZE_HOME", home)
			dir := filepath.Join(home, "native")
			writePlanAccount(t, dir)
			writePlanTokens(t, dir, planAccess, planRefresh, "inc-1", 1)
			fetchedWith(t, dir, tc.version)
			s := newNative(Options{Workspace: t.TempDir(), ContentHome: t.TempDir()}, func(o *harness.Options) {
				o.Getenv = func(string) string { return "" }
			})
			closeAtCleanup(t, s)
			if err := s.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			s.mu.Lock()
			refreshed := s.refreshed
			s.mu.Unlock()
			if refreshed == nil {
				t.Fatal("an account signed in with plan usage started no refresh")
			}
			await(t, refreshed, "the model list's refresh")
			reqs := api.requests()
			m, err := chatgptauth.ReadModels(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.fetched {
				if len(reqs) != 0 || len(m.Models) != 1 || m.Models[0].Slug != "gpt-5.6-sol" {
					t.Fatalf("the control: a list fetched with the pin was fetched again (%d requests)", len(reqs))
				}
			} else {
				if len(reqs) != 1 || reqs[0].path != "/v1/models" || !slices.Equal(reqs[0].query["client_version"], []string{pin}) {
					// The path and the version alone: a request's headers carry
					// the bearer.
					var seen []string
					for _, r := range reqs {
						seen = append(seen, fmt.Sprintf("%s client_version=%q", r.path, r.query["client_version"]))
					}
					t.Fatalf("the refresh's requests: %v; want one model list request carrying client_version %s", seen, pin)
				}
				if m.ClientVersion != pin || len(m.Models) != 1 || m.Models[0].Slug != "gpt-6.1-sol" {
					t.Fatalf("the list written: client_version %q, %d models; want the fetched list, with the pin", m.ClientVersion, len(m.Models))
				}
			}
			if _, err := os.Lstat(filepath.Join(dir, paths.LogsName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the host's refresh made the sign-in log's directory (%v)", err)
			}
		})
	}
}

// fetchedWith rewrites dir's model list as fetched now with client_version
// v ("" is none: a legacy file), fresh by age whatever its version.
func fetchedWith(t *testing.T, dir, v string) {
	t.Helper()
	m, err := chatgptauth.ReadModels(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.ClientVersion, m.FetchedAt = v, time.Now().UTC()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chatgptauth.ModelsFile(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// staleModels makes dir's model list two days old, so a session's open
// fetches it again (chatgptauth.ModelsMaxAge).
func staleModels(t *testing.T, dir string) {
	t.Helper()
	b, err := os.ReadFile(chatgptauth.ModelsFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	var m chatgptauth.Models
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m.FetchedAt = time.Now().Add(-48 * time.Hour).UTC()
	if b, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chatgptauth.ModelsFile(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestNativeUsageLatchLiftsOnAPersonsTurnOnly (P33): the plan's usage latch,
// set for the process, stays set through a wake — a turn nobody started —
// and the next turn a person starts lifts it. The control is the wake itself,
// which ran (it is a turn, and the latch did not stop it here: its model is
// scripted, not the plan's; the harness's own test proves a latched wake on
// the plan sends nothing).
func TestNativeUsageLatchLiftsOnAPersonsTurnOnly(t *testing.T) {
	noOpenAI(t)
	rig := newWakeRig(t, Options{}, nil)
	src := chatgptauth.Source(rig.f.dir)
	child, wake := newHeld(t), newHeld(t)
	id := rig.spawnOne(child, wake.step(openTextParts("noted"), closeTextParts()), answer("next turn"))
	src.LatchUsageLimit()
	rig.finish(child, id)
	rig.awaitDecided(true, "the result pending")
	await(t, wake.reached, "the wake's step")
	close(wake.release)
	rig.bracket(false, 1, "wake-1")
	if !src.UsageLimited() {
		t.Fatal("a wake lifted the usage latch")
	}
	if _, err := rig.s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if src.UsageLimited() {
		t.Fatal("a person's turn left the usage latch set")
	}
}

// TestNativePhrasesTheChatGPTPlansFailures (§3.12's table): each of the
// plan's failures in the adapter's words, and the empty-step copy without
// its max_output_tokens advice on the plan. The controls: a key provider's
// empty step keeps the advice, and a key provider's error carrying the
// plan's code is not the plan's.
func TestNativePhrasesTheChatGPTPlansFailures(t *testing.T) {
	plan := func(code, param string) error {
		return &harness.ProviderError{Provider: "chatgpt", Model: "chatgpt/gpt-5.6-sol", Driver: modeltable.DriverChatGPT, Code: code, Param: param, Final: true}
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"the usage limit", plan(harness.CodeUsageLimit, ""), chatgptUsageLimitText},
		{"the latch", fmt.Errorf("x: %w", chatgptauth.ErrUsageLimited), chatgptUsageLimitText},
		{"not eligible", plan(harness.CodeNotEligible, ""), chatgptNotEligibleText},
		{"an unsupported capability", plan(harness.CodeUnsupportedCapability, "tools[0]"), "native: ChatGPT plan request refused: tools[0] is not supported"},
		{"an unsupported capability with no param", plan(harness.CodeUnsupportedCapability, ""), "native: ChatGPT plan request refused: a part of the request is not supported"},
		{"signed out", fmt.Errorf("x: %w", chatgptauth.ErrSignedOut), chatgptSignInText},
		{"sign in again", fmt.Errorf("x: %w: refresh refused", chatgptauth.ErrSignInAgain), chatgptSignInText},
		{"plan usage off, at the refresh", fmt.Errorf("x: %w", chatgptauth.ErrPlanUsageDisabled), chatgptPlanOffText},
		{"plan usage off, in the table", fmt.Errorf("x: %w", modeltable.ErrPlanUsageDisabled), chatgptPlanOffText},
		{"an empty step on the plan", &harness.EmptyStepError{Model: "chatgpt/gpt-5.6-sol", Driver: modeltable.DriverChatGPT},
			"native: the model ended its answer without sending anything"},
		{"an empty step elsewhere (control)", &harness.EmptyStepError{Model: "test/a", Driver: modeltable.DriverOpenAICompat},
			"native: the model ended its answer without sending anything (a reasoning model can spend its whole output ceiling thinking; raise max_output_tokens in models.toml)"},
		{"the code from a key provider (control)", &harness.ProviderError{Provider: "test", Model: "test/a", Driver: modeltable.DriverOpenAICompat,
			Code: harness.CodeUsageLimit, StatusCode: 429, Message: "slow down"}, `native: provider "test" failed (HTTP 429): slow down`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := phraseTurnError(tc.err)
			if got.Error() != tc.want || !errors.Is(got, tc.err) {
				t.Fatalf("phraseTurnError = %q, want %q", got, tc.want)
			}
		})
	}
	if got := phraseSetupError(fmt.Errorf("harness: %w", modeltable.ErrNotSignedIn), nil, "chatgpt/gpt-5.6-sol"); !strings.Contains(got.Error(), "nobody is signed in; run /connect or craze auth login chatgpt") {
		t.Fatalf("phraseSetupError(not signed in) = %q", got)
	}
	// Another account's list (C14r, r12 #4) is not a missing key: the
	// table is at hand, and noKeyText's api_key advice is not what is said.
	other := fmt.Errorf("harness: %w", modeltable.ErrOtherAccount)
	tbl := &modeltable.Table{Models: map[string]modeltable.Model{"chatgpt/gpt-5.6-sol": {Provider: modeltable.ChatGPTProvider}},
		Providers: map[string]modeltable.Provider{modeltable.ChatGPTProvider: {Driver: modeltable.DriverChatGPT}}}
	if got := phraseSetupError(other, tbl, "chatgpt/gpt-5.6-sol"); got.Error() != `native: model "chatgpt/gpt-5.6-sol" is from another ChatGPT account's model list; a new session offers the signed-in account's models` || !errors.Is(got, modeltable.ErrOtherAccount) {
		t.Fatalf("phraseSetupError(another account's model) = %q", got)
	}
}

// planAgentCall answers with one call of the agent tool: a sub-agent on the
// session's own model, given prompt.
func planAgentCall(prompt string) http.HandlerFunc {
	b, _ := json.Marshal(map[string]any{"description": "look for me", "prompt": prompt, "model": "inherit"})
	args := string(b)
	call := func(a string) map[string]any {
		return map[string]any{"type": "function_call", "id": "fc_a", "call_id": "call_a", "name": "agent", "namespace": "craze", "arguments": a}
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		planEvent(w, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": call("")})
		planEvent(w, map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_a", "delta": args})
		planEvent(w, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": call(args)})
		planCompleted(w)
	}
}

// noBearerOf fails the test if any request the API saw carried one of tokens.
func noBearerOf(t *testing.T, api *fakeEndpoint, tokens ...string) {
	t.Helper()
	for i, r := range api.requests() {
		for _, tok := range tokens {
			if strings.Contains(r.header.Get("Authorization"), tok) {
				t.Fatalf("request %d reached the API with another account's token", i)
			}
		}
	}
}

// TestNativeChatGPTHoldsASessionToItsAccount (plan 033 C14r2, review r13 d):
// a session open on a model of account A's list sends nothing with account
// B's token once B has signed in — B's registration and token record written
// as a sign-in in another process writes them, while the session's table,
// and so Resolve, still holds A's list — and its next turn ends in the plan's
// words for a model of another account's list: the request is refused as its
// credential is fetched, and the API sees nothing after the switch. With A
// signed in again, the turn goes through on A's new token. That last turn is
// the control: the switch of the record reaches the requests, so without the
// binding B's token would have reached them too.
func TestNativeChatGPTHoldsASessionToItsAccount(t *testing.T) {
	api, _ := noOpenAI(t)
	api.queue(planAnswer("first"), planAnswer("again"))
	s, dir := planSession(t, t.TempDir())
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("the first turn, on A: %v", err)
	}
	drained(s)

	if err := signInOf(dir, otherSubject, otherClient, otherAccess, otherRefresh, "inc-b"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Prompt(context.Background(), "and again")
	if want := otherAccountText("chatgpt/gpt-5.6-sol"); err == nil || err.Error() != want || !errors.Is(err, modeltable.ErrOtherAccount) {
		t.Fatalf("a turn after B signed in = %v, want %q", err, want)
	}
	if n := len(api.requests()); n != 1 {
		t.Fatalf("the API saw %d requests, want the first turn's alone", n)
	}
	for _, v := range []string{otherAccess, otherRefresh, planAccess} {
		if l := nativeLeaks(err, v); len(l) > 0 {
			t.Fatalf("a token reached the error at %v", l)
		}
	}
	drained(s)

	if err := signInOf(dir, planSubject, planClient, planAccess2, planRefresh2, "inc-a2"); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Prompt(context.Background(), "once more"); err != nil || res.StopReason != harness.StopEndTurn {
		t.Fatalf("control: with A signed in again the turn = %+v, %v", res, err)
	}
	reqs := api.requests()
	if len(reqs) != 2 || reqs[1].header.Get("Authorization") != "Bearer "+planAccess2 {
		t.Fatalf("control: the API saw %d requests (the second with A's new token?)", len(reqs))
	}
	noBearerOf(t, api, otherAccess)
}

// TestNativeChatGPTHoldsASubagentToItsAccount (r13 d): the same for a
// sub-agent already running on A's model. B signs in while the child works —
// the API's answer to the child's first request switches the files, then asks
// for a read — and the child's next request, after the read, is refused, so
// the child fails; the parent's next request is refused too, and the turn
// ends in the plan's words. The API saw the parent's first request and the
// child's, both with A's token, and nothing with B's.
func TestNativeChatGPTHoldsASubagentToItsAccount(t *testing.T) {
	api, _ := noOpenAI(t)
	var dir string
	switched := func(w http.ResponseWriter, r *http.Request) {
		if err := signInOf(dir, otherSubject, otherClient, otherAccess, otherRefresh, "inc-b"); err != nil {
			t.Error(err)
		}
		planReadCall("hello.txt")(w, r)
	}
	api.queue(planAgentCall("read hello.txt and say what it holds"), switched)
	s, d := planSession(t, nativeWorkspaceWith(t, map[string]string{"hello.txt": "hello world\n"}))
	dir = d
	_, err := s.Prompt(context.Background(), "delegate the read")
	if want := otherAccountText("chatgpt/gpt-5.6-sol"); err == nil || err.Error() != want {
		t.Fatalf("the parent's turn = %v, want %q", err, want)
	}
	reqs := api.requests()
	if len(reqs) != 2 {
		t.Fatalf("the API saw %d requests, want the parent's first and the child's first", len(reqs))
	}
	for i, r := range reqs {
		if r.header.Get("Authorization") != "Bearer "+planAccess {
			t.Fatalf("request %d did not carry A's token", i)
		}
	}
	if !strings.Contains(reqs[1].body, "read hello.txt and say what it holds") {
		t.Fatalf("the second request was not the child's: %.200s", reqs[1].body)
	}
	rows := s.Snapshot().Subagents
	if len(rows) != 1 || rows[0].Status != SubagentFailed || !strings.Contains(rows[0].Error, "another account") {
		t.Fatalf("the child's row = %+v, want it failed on the other account's token", rows)
	}
	noBearerOf(t, api, otherAccess)
}

// TestNativeModelDialogListsThePlanInTheAccountsOrder (plan 033 C14r2, V5):
// a session signed in to the plan, with an account list of five models whose
// priority order is not their names' — astra, sol, terra, luna, gpt-5.5 —
// offers them, through its snapshot and the dialog's order (OrderModels), as
// the running model first and then the rest of the block in the account's
// order. The control is the names' order, which the V5 dialog showed: GPT-5.5
// first.
func TestNativeModelDialogListsThePlanInTheAccountsOrder(t *testing.T) {
	_, _ = noOpenAI(t)
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	dir := filepath.Join(home, "native")
	writePlanAccount(t, dir)
	var models []chatgptauth.Model
	for i, m := range [][2]string{
		{"gpt-6-astra", "GPT-6-Astra"}, {"gpt-5.6-sol", "GPT-5.6-Sol"}, {"gpt-5.6-terra", "GPT-5.6-Terra"},
		{"gpt-5.6-luna", "GPT-5.6-Luna"}, {"gpt-5.5", "GPT-5.5"},
	} {
		models = append(models, chatgptauth.Model{Slug: m[0], DisplayName: m[1], ContextWindow: 272000,
			Efforts: []string{"low", "medium", "high"}, InputModalities: []string{"text"}, Priority: i})
	}
	list, err := json.Marshal(chatgptauth.Models{Version: 1, Subject: planSubject, ClientID: planClient, ClientVersion: modeltable.ChatGPTModelsClientVersion(), FetchedAt: time.Now().UTC(), Models: models})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chatgptauth.ModelsFile(dir), list, 0o600); err != nil {
		t.Fatal(err)
	}
	writePlanTokens(t, dir, planAccess, planRefresh, "inc-1", 1)
	s := newNative(Options{Workspace: t.TempDir(), ContentHome: t.TempDir()}, func(o *harness.Options) {
		o.Getenv = func(string) string { return "" }
	})
	closeAtCleanup(t, s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	snap := s.Snapshot()
	got := modelIDs(OrderModels(snap))
	if want := "chatgpt/gpt-5.6-sol,chatgpt/gpt-6-astra,chatgpt/gpt-5.6-terra,chatgpt/gpt-5.6-luna,chatgpt/gpt-5.5"; got != want {
		t.Fatalf("the dialog lists %s, want %s", got, want)
	}
	byName := slices.Clone(snap.Models)
	slices.SortFunc(byName, func(a, b ModelInfo) int { return strings.Compare(a.Name, b.Name) })
	if byName[0].ID != "chatgpt/gpt-5.5" {
		t.Fatalf("control: by name the list would start %s", byName[0].ID)
	}
}
