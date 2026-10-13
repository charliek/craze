package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/anthropicmodels"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The providers' model lists a native session fetches in the background
// (native_lists.go; plan 044 §3.6, P9, P23, A5): the Anthropic account's,
// fetched as a session opens and on RefreshModels with the key its table
// funds craze's own Anthropic provider with, through the real fetcher
// (internal/anthropicmodels) and the Messages API client against a fake
// Anthropic API reached by the test override — and its refresher beside the
// ChatGPT plan's. No request leaves the machine: the override is the fake's,
// and TestMain fences it otherwise. The keys are canaries.

const (
	listKeyA = "sk-ant-canary-not-a-secret-00A1"
	listKeyB = "sk-ant-canary-not-a-secret-00B2"
	// opus6 is the model the fake's account lists that no seed names: offered
	// only once the session has taken its list up.
	opus6 = "anthropic/claude-opus-6"
	// sonnet55 is a seed the fake's account does not list: offered until the
	// session takes the list up, hidden after.
	sonnet55 = "anthropic/claude-sonnet-5-5"
)

// anthropicListFixture is a native fixture over the shipped catalog — so
// craze's own Anthropic provider is in its table — whose sessions start on
// the scripted test/a (models.toml's default_model, test/b beside it), and
// a fake Anthropic API every fetch of the account's list goes to (the test
// override). key, when it is not "", is stored for the Anthropic provider.
func anthropicListFixture(t *testing.T, key string) (*nativeFixture, *fakeEndpoint) {
	t.Helper()
	f := newNativeFixture(t)
	for name, doc := range map[string]string{
		modeltable.ProvidersFile: "version = 1\n\n[providers.test]\ndriver = \"openai-compat\"\nbase_url = \"http://127.0.0.1:9/v1\"\nenv_keys = [\"NATIVE_TEST_KEY\"]\n",
		modeltable.ModelsFile: "version = 1\ndefault_model = \"test/a\"\n\n" +
			"[models.\"test/a\"]\nprovider = \"test\"\nwire_model = \"wire-a\"\nname = \"Model A\"\nefforts = [\"low\", \"high\"]\ndefault_effort = \"high\"\n\n" +
			"[models.\"test/b\"]\nprovider = \"test\"\nwire_model = \"wire-b\"\nname = \"Model B\"\n",
	} {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if key != "" {
		storeAnthropicKey(t, f.dir, key)
	}
	api := newFakeEndpoint(t)
	t.Setenv(messagesapi.TestAPIEnv, api.srv.URL)
	return f, api
}

// storeAnthropicKey stores key for the Anthropic provider in dir, as craze
// auth login and /connect do.
func storeAnthropicKey(t *testing.T, dir, key string) {
	t.Helper()
	if err := modeltable.SetKey(dir, modeltable.AnthropicProvider, key); err != nil {
		t.Fatal(err)
	}
}

// listed is one model of an account's list: its id, its family and when it
// was made.
type listed struct{ id, line, created string }

// theAccount is the list the fake answers with unless a case says otherwise:
// a new Opus no seed names, and Haiku 5.5, a seed. Every other seed — Sonnet
// 5.5 among them — is one the account does not list.
var theAccount = []listed{{"claude-opus-6", "opus", "2026-10-10T09:00:00Z"}, {"claude-haiku-5-5", "haiku", "2026-10-07T18:00:00Z"}}

// accountList answers a model list request with models, complete, as the
// Messages API's GET /v1/models does.
func accountList(models ...listed) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		on := map[string]any{"supported": true}
		data := make([]map[string]any, 0, len(models))
		for _, m := range models {
			data = append(data, map[string]any{
				"type": "model", "id": m.id, "display_name": "Claude " + m.id, "created_at": m.created, "line": m.line,
				"max_input_tokens": 1000000, "max_tokens": 128000, "lifecycle": "active",
				"capabilities": map[string]any{
					"effort":      map[string]any{"supported": true, "low": on, "medium": on, "high": on},
					"image_input": on,
					"thinking":    map[string]any{"supported": true, "types": map[string]any{"adaptive": on}},
				},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": false, "last_id": models[len(models)-1].id})
	}
}

// keyEcho answers a model list request with a 401 whose message repeats the
// key the request carried — the leak an error body is (P13).
func keyEcho(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key %s"}}`, r.Header.Get("X-Api-Key"))
}

// heldReply is reply held back until release is closed — or the request is
// cancelled, which answers nothing — with arrived closed as the request
// comes in and answered once reply has written its answer. release is
// closed at the test's end at the latest, before the fake's server closes
// (which waits for its handlers). One request each.
func heldReply(t *testing.T, reply http.HandlerFunc) (h http.HandlerFunc, arrived, release, answered chan struct{}) {
	t.Helper()
	arrived, release, answered = make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { closeOnce(release) })
	return func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		select {
		case <-release:
			reply(w, r)
			close(answered)
		case <-r.Context().Done():
		}
	}, arrived, release, answered
}

// listRequests is how many model list requests the fake saw, and whether
// each carried key, in order.
func listRequests(api *fakeEndpoint, key string) (n int, keyed []bool) {
	for _, r := range api.requests() {
		if r.path == "/v1/models" {
			n++
			keyed = append(keyed, r.header.Get("X-Api-Key") == key)
		}
	}
	return n, keyed
}

// anthropicFetch is s's Anthropic refresher's done channel: nil when it has
// started no fetch.
func anthropicFetch(s *nativeSession) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.anthropicList.done
}

// anthropicPending is the fetch s's Anthropic refresher has pending, nil
// for none.
func anthropicPending(s *nativeSession) listSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.anthropicList.pending
}

// awaitAnthropicFetch waits for the fetch of the Anthropic account's list a
// refresh started, if it started one.
func awaitAnthropicFetch(t *testing.T, s *nativeSession) {
	t.Helper()
	if ch := anthropicFetch(s); ch != nil {
		await(t, ch, "the Anthropic list's fetch")
	}
}

// listFile is dir's account list, as a load reads it.
func listFile(t *testing.T, dir string) *modeltable.AnthropicModels {
	t.Helper()
	doc, err := modeltable.ReadAnthropicModels(dir)
	if err != nil {
		t.Fatalf("the account's list: %v", err)
	}
	return doc
}

// listIDs are doc's model ids, in order.
func listIDs(doc *modeltable.AnthropicModels) []string {
	var out []string
	for _, m := range doc.Models {
		out = append(out, m.ID)
	}
	return out
}

// TestNativeOpenFetchesTheAnthropicList (plan 044 §3.6 b, A5): a session whose
// table funds craze's own Anthropic provider — with a stored key, or with an
// exported one alone — fetches the account's list as it opens, once, with
// that key: the list is written bound to the key, and the fetch's completion
// reloads the table, so the session offers the model only the list names and
// no longer the seed the list leaves out. A RefreshModels after it finds the
// list current and fetches nothing. The control: the open offered the seeds
// alone, read while the fake held the answer back. Negative control: a
// session nothing funds Anthropic for starts no fetch.
func TestNativeOpenFetchesTheAnthropicList(t *testing.T) {
	for _, tc := range []struct {
		name             string
		stored, exported bool
	}{
		{"a stored key", true, false},
		{"an exported key alone", false, true},
		{"no key (negative control)", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := ""
			if tc.stored {
				stored = listKeyA
			}
			f, api := anthropicListFixture(t, stored)
			if tc.exported {
				t.Setenv("ANTHROPIC_API_KEY", listKeyA) // what a load binds the list to
				f.env["ANTHROPIC_API_KEY"] = listKeyA   // what the session is funded by
			}
			reply, arrived, release, _ := heldReply(t, accountList(theAccount...))
			api.queue(reply)
			s := f.started(Options{})
			fetch := anthropicFetch(s)
			if !tc.stored && !tc.exported {
				if n, _ := listRequests(api, listKeyA); fetch != nil || n != 0 {
					t.Fatalf("a session nothing funds Anthropic for started a fetch (%d requests)", n)
				}
				return
			}
			if fetch == nil {
				t.Fatal("the open started no fetch")
			}
			await(t, arrived, "the open's request")
			if got := offeredIDs(s.Snapshot().Models); slices.Contains(got, opus6) || !slices.Contains(got, sonnet55) {
				t.Fatalf("control: the open offered %v; want the seeds alone", got)
			}
			close(release)
			await(t, fetch, "the open's fetch")
			if got := offeredIDs(s.Snapshot().Models); !slices.Contains(got, opus6) || slices.Contains(got, sonnet55) {
				t.Fatalf("after the fetch the session offers %v; want the account's list", got)
			}
			if doc := listFile(t, f.dir); doc.KeyID != modeltable.AnthropicKeyID(listKeyA) || !slices.Equal(listIDs(doc), []string{"claude-opus-6", "claude-haiku-5-5"}) {
				t.Fatalf("the list written: %v, bound to the key: %t", listIDs(doc), doc.KeyID == modeltable.AnthropicKeyID(listKeyA))
			}
			refresh(t, s, "")
			awaitAnthropicFetch(t, s)
			if n, keyed := listRequests(api, listKeyA); n != 1 || !keyed[0] {
				t.Fatalf("%d model list requests (each with the key: %v); want the open's one", n, keyed)
			}
		})
	}
}

// TestNativeRefreshFetchesWithAKeySavedWhileItRuns (plan 044 P23, A5): a key
// saved while the session runs — what /connect does before it asks for a
// refresh — is the key that very RefreshModels fetches the account's list
// with: the refresh reloads first and asks for the fetch on the table it
// read, whether it published it (idle) or a turn holds it back (mid-turn,
// where the fetch's completion lands during the turn as well, and is taken
// up at its end). The list is offered once the fetch ends; the one request
// is the session's, with the saved key (the TUI makes none of its own). The
// control: a refresh before the save starts no fetch. Negative control: a
// refresh that asked on the table it held before its reload — which funds
// nothing — would start none after the save either.
func TestNativeRefreshFetchesWithAKeySavedWhileItRuns(t *testing.T) {
	for _, midTurn := range []bool{false, true} {
		t.Run(fmt.Sprintf("mid-turn=%v", midTurn), func(t *testing.T) {
			f, api := anthropicListFixture(t, "")
			api.queue(accountList(theAccount...))
			s := f.started(Options{})
			refresh(t, s, "")
			if fetch := anthropicFetch(s); fetch != nil {
				t.Fatal("control: a refresh with nothing funding Anthropic started a fetch")
			}
			var h *held
			var out <-chan outcome
			if midTurn {
				h = newHeld(t)
				f.models["test/a"].push(h.step(textParts("working"), finishParts(fantasy.FinishReasonStop)))
				out = startPrompt(s, "go")
				await(t, h.reached, "the held step")
			}
			storeAnthropicKey(t, f.dir, listKeyA)
			want := ModelsApplied
			if midTurn {
				want = ModelsPending
			}
			if r := refresh(t, s, ""); r.Status != want {
				t.Fatalf("RefreshModels after the save = %+v; want %s", r, want)
			}
			fetch := anthropicFetch(s)
			if fetch == nil {
				t.Fatal("a refresh after the save started no fetch: it asked on the table from before its reload")
			}
			await(t, fetch, "the fetch with the saved key")
			if midTurn {
				if offers(s.Snapshot().Models, opus6) {
					t.Fatal("a list fetched mid-turn was offered before the turn's end")
				}
				close(h.release)
				if o := await(t, out, "the turn"); o.err != nil {
					t.Fatalf("the turn: %v", o.err)
				}
			}
			if got := offeredIDs(s.Snapshot().Models); !slices.Contains(got, opus6) {
				t.Fatalf("after the fetch the session offers %v; want the account's list", got)
			}
			if n, keyed := listRequests(api, listKeyA); n != 1 || !keyed[0] {
				t.Fatalf("%d model list requests (each with the saved key: %v); want one", n, keyed)
			}
		})
	}
}

// TestNativeListRefreshersAreIndependent (plan 044 P9, A5): the ChatGPT
// plan's refresher and the Anthropic account's are each their own. With the
// plan's fetch held in flight, the Anthropic fetch runs, ends and is taken
// up, and a RefreshModels joins the plan's fetch (no second plan request)
// while it asks the Anthropic one again (current: no request). Close, with
// the plan's fetch still in flight, cancels it and joins both refreshers.
// The control: the plan's fetch was in flight — its request arrived and was
// never answered — all the while. Negative control: one refresher for both
// would have made the Anthropic fetch wait for the plan's, and opus6 unoffered
// until Close.
func TestNativeListRefreshersAreIndependent(t *testing.T) {
	planAPI, _ := noOpenAI(t)
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	dir := filepath.Join(home, "native")
	writePlanAccount(t, dir)
	writePlanTokens(t, dir, planAccess, planRefresh, "inc-1", 1)
	staleModels(t, dir)
	storeAnthropicKey(t, dir, listKeyA)
	claudeAPI := newFakeEndpoint(t)
	t.Setenv(messagesapi.TestAPIEnv, claudeAPI.srv.URL)
	planReply, planArrived, _, _ := heldReply(t, func(http.ResponseWriter, *http.Request) {})
	planAPI.queue(planReply)
	claudeAPI.queue(accountList(theAccount...))

	s := newNative(Options{Workspace: t.TempDir(), ContentHome: t.TempDir()}, func(o *harness.Options) {
		o.Getenv = func(string) string { return "" }
	})
	closeAtCleanup(t, s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.mu.Lock()
	planDone := s.planList.done
	s.mu.Unlock()
	if planDone == nil {
		t.Fatal("control: the stale plan list started no fetch")
	}
	await(t, planArrived, "the plan list's request")
	await(t, anthropicFetch(s), "the Anthropic list's fetch")
	if !offers(s.Snapshot().Models, opus6) {
		t.Fatalf("the Anthropic list was not taken up while the plan's fetch ran: %v", offeredIDs(s.Snapshot().Models))
	}
	refresh(t, s, "")
	awaitAnthropicFetch(t, s)
	s.mu.Lock()
	inFlight := s.planList.running != nil && s.planList.done == planDone
	s.mu.Unlock()
	planRequests := 0
	for _, r := range planAPI.requests() {
		if strings.HasSuffix(r.path, "/models") {
			planRequests++
		}
	}
	if n, _ := listRequests(claudeAPI, listKeyA); !inFlight || planRequests != 1 || n != 1 {
		t.Fatalf("plan fetch in flight %t, plan list requests %d, Anthropic list requests %d; want the one plan fetch still in flight, joined, and the one Anthropic request", inFlight, planRequests, n)
	}
	within(t, "Close with the plan's fetch in flight", func() { _ = s.Close() })
	for name, ch := range map[string]chan struct{}{"the plan's": planDone, "the Anthropic one's": anthropicFetch(s)} {
		select {
		case <-ch:
		default:
			t.Fatalf("Close returned before %s refresher ended", name)
		}
	}
}

// TestNativeAKeySavedMidFetchIsFetchedWithNext (plan 044 P9, A5): a refresh
// asked while the old key's fetch is in flight is remembered and run once it
// returns: key A's fetch is held; B is saved and a refresh asked; when A's
// answer comes, B's fetch follows in the same run, and the session offers
// B's list — A's is bound to A, so never offered with B. The control: a
// refresh asked for A itself while its own fetch is in flight joins it,
// leaving nothing pending and making no request.
func TestNativeAKeySavedMidFetchIsFetchedWithNext(t *testing.T) {
	const fable6 = "anthropic/claude-fable-6"
	f, api := anthropicListFixture(t, listKeyA)
	replyA, arrivedA, releaseA, _ := heldReply(t, accountList(theAccount...))
	api.queue(replyA, accountList(listed{"claude-fable-6", "fable", "2026-10-11T00:00:00Z"}))
	s := f.started(Options{})
	await(t, arrivedA, "key A's request")
	refresh(t, s, "")
	if p := anthropicPending(s); p != nil {
		t.Fatal("control: a refresh for the key in flight was left pending, not joined")
	}
	storeAnthropicKey(t, f.dir, listKeyB)
	refreshApplied(t, s, "")
	if p := anthropicPending(s); p == nil || p.id() != modeltable.AnthropicKeyID(listKeyB) {
		t.Fatal("a refresh for key B while A's fetch ran is not pending")
	}
	if n, _ := listRequests(api, listKeyA); n != 1 {
		t.Fatalf("%d model list requests before A's answer; want A's alone", n)
	}
	close(releaseA)
	await(t, anthropicFetch(s), "A's fetch and B's after it")
	n, keyedA := listRequests(api, listKeyA)
	_, keyedB := listRequests(api, listKeyB)
	if n != 2 || !keyedA[0] || !keyedB[1] {
		t.Fatalf("%d model list requests, with A %v, with B %v; want A's, then B's", n, keyedA, keyedB)
	}
	if doc := listFile(t, f.dir); doc.KeyID != modeltable.AnthropicKeyID(listKeyB) {
		t.Fatal("the list left is not B's")
	}
	if got := offeredIDs(s.Snapshot().Models); !slices.Contains(got, fable6) || slices.Contains(got, opus6) {
		t.Fatalf("the session offers %v; want B's list and never A's", got)
	}
}

// TestNativeAFailedAnthropicFetchBacksOffForItsKey (plan 044 P9, P13, A5): a
// fetch the account refuses — with a 401 that repeats the key — is one
// anthropic_models_refresh diag naming the status and never the key, and
// starts a backoff for that key: a refresh within it fetches nothing, one
// after it fetches again. A key saved meanwhile is no retry of the failed
// one: its refresh fetches at once, within the old key's backoff. The
// control: each fetch that was made reached the fake, with its key.
func TestNativeAFailedAnthropicFetchBacksOffForItsKey(t *testing.T) {
	f, api := anthropicListFixture(t, listKeyA)
	api.queue(keyEcho, keyEcho, accountList(theAccount...))
	var (
		clockMu sync.Mutex
		now     = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	)
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}
	jdir := filepath.Join(t.TempDir(), "journal")
	s := f.session(Options{JournalDir: jdir})
	s.fetchClock = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	jw, inc := journalOf(t, s.log), s.Incarnation()
	awaitAnthropicFetch(t, s)

	advance(listRetryBackoff - time.Second)
	refresh(t, s, "")
	awaitAnthropicFetch(t, s)
	if n, _ := listRequests(api, listKeyA); n != 1 {
		t.Fatalf("%d model list requests within the backoff; want the open's alone", n)
	}
	advance(2 * time.Second)
	refresh(t, s, "")
	awaitAnthropicFetch(t, s)
	if n, keyed := listRequests(api, listKeyA); n != 2 || !keyed[1] {
		t.Fatalf("%d model list requests after the backoff (with the key: %v); want a second", n, keyed)
	}

	storeAnthropicKey(t, f.dir, listKeyB)
	refreshApplied(t, s, "")
	awaitAnthropicFetch(t, s)
	if n, keyed := listRequests(api, listKeyB); n != 3 || !keyed[2] {
		t.Fatalf("%d model list requests after B was saved (with B: %v); want B's at once", n, keyed)
	}
	if !offers(s.Snapshot().Models, opus6) {
		t.Fatalf("B's list is not offered: %v", offeredIDs(s.Snapshot().Models))
	}

	closeJournaled(t, s, jw)
	lines := assertOneJournal(t, jdir, jw, inc)
	notes := diags(lines, diagAnthropicModelsRefresh)
	if len(notes) != 2 || !strings.Contains(fmt.Sprint(notes[0]["error"]), "401") {
		t.Fatalf("the refresh's diags = %v; want the two refusals, naming the status", notes)
	}
	for _, key := range []string{listKeyA, listKeyB} {
		if l := nativeLeaks(lines, key); len(l) > 0 {
			t.Fatalf("a key reached the journal at %v", l)
		}
	}
}

// TestNativeAnthropicFetchLandsDuringALoad (plan 044 A5; plan 034 A19): the
// list fetched as a load opens — its answer in before the replay's walk has
// ended — is not published inside the load's bracket: the reload it asks for
// is owed, and the load's end takes it up after the end bracket. The
// control: the fetch had ended inside the bracket, and the session did not
// offer its list there. Negative control: a reload that did not hold back
// while loading would publish a catalog before replay:end.
func TestNativeAnthropicFetchLandsDuringALoad(t *testing.T) {
	f, api := anthropicListFixture(t, "")
	ws := t.TempDir()
	id := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("one")})
	storeAnthropicKey(t, f.dir, listKeyA)
	api.queue(accountList(theAccount...))
	s := f.session(Options{Workspace: ws, LoadSessionID: id})
	var fetched, offered bool
	s.loadReplayedSeam = func() {
		fetch := anthropicFetch(s)
		if fetch == nil {
			return
		}
		select {
		case <-fetch:
			fetched = true
		case <-time.After(nativeWait):
		}
		offered = offers(s.Snapshot().Models, opus6)
	}
	evs, err := startLoad(t, s)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !fetched || offered {
		t.Fatalf("control: inside the bracket the fetch had ended %t, and its list was offered %t; want ended, not offered", fetched, offered)
	}
	lines := loadLines(evs)
	end := slices.Index(lines, "replay:end")
	if end < 0 {
		t.Fatalf("no end bracket in %q", lines)
	}
	for i, l := range lines {
		if strings.Contains(l, "catalog=") && i < end {
			t.Fatalf("a catalog inside the bracket: %q", lines)
		}
	}
	if got := offeredIDs(s.Snapshot().Models); !slices.Contains(got, opus6) {
		t.Fatalf("after the load the session offers %v; want the fetched list", got)
	}
}

// TestNativeAnthropicFetchLandsDuringASwitch (plan 044 A5; plan 034 A24; C5
// review 4): a fetch whose list is written while a model switch is in its
// window — modelsMu held between the switch's reading of the list and the
// harness's switch — reloads only once the switch is out of it. The fetch is
// held after it wrote the list (listSeam "fetched") until the switch is in
// its window; let go there, its reload is seen waiting for modelsMu
// (reloadQueuedSeam) and never reading the files (reloadSeam "stamped")
// inside it; and the switch's delta is published before the fetched list's
// catalog, which then offers the list with the switched-to model current.
// The control: modelsMu was held in the window (a probe could not take it).
// Negative control: a reload that did not take modelsMu would read the files
// inside the window — "stamped" with no wait — which the window's own check
// fails.
func TestNativeAnthropicFetchLandsDuringASwitch(t *testing.T) {
	f, api := anthropicListFixture(t, listKeyA)
	api.queue(accountList(theAccount...))
	fetched, release := make(chan struct{}), make(chan struct{})
	queued, stamped := make(chan struct{}), make(chan struct{})
	var queuedOnce, stampedOnce sync.Once
	var held atomic.Bool
	t.Cleanup(func() { closeOnce(release) })
	s := f.session(Options{})
	s.listSeam = func(stage string) {
		if stage == "fetched" && held.CompareAndSwap(false, true) {
			close(fetched)
			<-release
		}
	}
	s.reloadQueuedSeam = func() { queuedOnce.Do(func() { close(queued) }) }
	s.reloadSeam = func(stage string) {
		if stage == "stamped" {
			stampedOnce.Do(func() { close(stamped) })
		}
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	takeStartDelta(t, s.log)
	await(t, fetched, "the fetch, its list written")
	var once sync.Once
	var probed, waited, early bool
	s.mu.Lock()
	s.setModelSeam = func() {
		once.Do(func() {
			if s.modelsMu.TryLock() {
				probed = true
				s.modelsMu.Unlock()
			}
			close(release)
			select {
			case <-queued:
				waited = true
			case <-stamped:
				early = true
			case <-time.After(nativeWait):
			}
		})
	}
	s.mu.Unlock()
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatalf("the switch: %v", err)
	}
	if probed || early || !waited {
		t.Fatalf("inside the switch's window: modelsMu free %t, the fetch's reload read the files %t, seen waiting for modelsMu %t; want held, not read, waiting",
			probed, early, waited)
	}
	await(t, anthropicFetch(s), "the fetch's reload")
	evs := deltaSettled(t, s)
	switched := eventIndex(evs, func(ev Event) bool {
		return ev.Type == EventMeta && ev.State != nil && ev.State.Model != nil && *ev.State.Model == "test/b"
	})
	listed := eventIndex(evs, func(ev Event) bool { return hasCatalog(ev) && offers(ev.State.Catalog.Models, opus6) })
	if switched < 0 || listed < switched {
		t.Fatalf("the switch's delta is event %d and the fetched list's catalog %d; want the switch's first", switched, listed)
	}
	if snap := s.Snapshot(); snap.CurrentModel != "test/b" || !offers(snap.Models, opus6) || !offers(snap.Models, "test/b") {
		t.Fatalf("after the switch and the fetch the session runs on %s and offers %v; want test/b and the fetched list", snap.CurrentModel, offeredIDs(snap.Models))
	}
}

// TestNativeCloseJoinsTheAnthropicFetch (plan 044 P9, A5; C5 reviews 3, 4):
// Close cancels the fetch in flight and waits for every goroutine its
// refresher started. The run here is held after its fetch was cancelled and
// its done closed (listSeam "exit") until Close is joining the refreshers
// (listsJoinSeam), so Close can return only once the run has ended; and a
// fetch Close cancelled is no failure — no diag — and wrote no list.
// Negative control: a Close that waited only for each refresher's done would
// return while the run is held, which the run sees (its Close-returned case).
func TestNativeCloseJoinsTheAnthropicFetch(t *testing.T) {
	f, api := anthropicListFixture(t, listKeyA)
	reply, arrived, _, _ := heldReply(t, accountList(theAccount...))
	api.queue(reply)
	jdir := filepath.Join(t.TempDir(), "journal")
	s := f.session(Options{JournalDir: jdir})
	joining, closed, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var outlived atomic.Bool
	s.listSeam = func(stage string) {
		if stage != "exit" {
			return
		}
		select {
		case <-joining:
		case <-closed:
			outlived.Store(true)
		}
		close(exited)
	}
	s.listsJoinSeam = func() { close(joining) }
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	jw, inc := journalOf(t, s.log), s.Incarnation()
	await(t, arrived, "the open's request")
	go func() {
		_ = s.Close()
		close(closed)
	}()
	await(t, closed, "Close")
	select {
	case <-exited:
	default:
		t.Fatal("Close returned before the fetch's run had ended")
	}
	if outlived.Load() {
		t.Fatal("the fetch's run outlived Close")
	}
	closeJournaled(t, s, jw)
	if notes := diags(assertOneJournal(t, jdir, jw, inc), diagAnthropicModelsRefresh); len(notes) != 0 {
		t.Fatalf("a fetch Close cancelled was journaled as a failure: %v", notes)
	}
	if _, err := os.Stat(filepath.Join(f.dir, modeltable.AnthropicModelsFile)); !os.IsNotExist(err) {
		t.Fatalf("a fetch Close cancelled wrote the list (%v)", err)
	}
}

// TestNativeHostsOpeningAtOnceFetchTheListOnce (plan 044 P8, A5; C5 review
// 4): two sessions on one native directory — two hosts — opening at once
// make one request between them: the first's fetch holds the list's lock
// across its check, its request and its write, and the second, past its own
// check, is seen waiting on that lock (anthropicmodels' lock-busy hook)
// while the first's request is still held; once the first writes, the
// second finds the list current. Both then offer it — the one that fetched
// by its own reload, the other at its next refresh, the list's file being
// one of the table's inputs — with no request more. Negative control: a
// fetch that did not hold the lock across its request would make a second
// request instead of waiting, which fails the wait.
func TestNativeHostsOpeningAtOnceFetchTheListOnce(t *testing.T) {
	f, api := anthropicListFixture(t, listKeyA)
	reply, arrived, release, _ := heldReply(t, accountList(theAccount...))
	second := make(chan struct{})
	api.queue(reply, func(w http.ResponseWriter, _ *http.Request) {
		close(second)
		http.Error(w, "a second request", http.StatusServiceUnavailable)
	})
	busy := make(chan struct{})
	var busyOnce sync.Once
	t.Cleanup(anthropicmodels.SetLockBusyHookForTest(func() { busyOnce.Do(func() { close(busy) }) }))
	sessions := []*nativeSession{f.session(Options{}), f.session(Options{})}
	var wg sync.WaitGroup
	errs := make([]error, len(sessions))
	for i, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Start(context.Background())
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("session %d's Start: %v", i, err)
		}
	}
	await(t, arrived, "the first request")
	select {
	case <-busy:
	case <-second:
		t.Fatal("the second host made a request of its own while the first's was held")
	case <-time.After(nativeWait):
		t.Fatalf("the second host was not seen waiting on the list's lock within %v", nativeWait)
	}
	close(release)
	for _, s := range sessions {
		await(t, anthropicFetch(s), "a session's fetch")
		refresh(t, s, "")
		awaitAnthropicFetch(t, s)
	}
	for i, s := range sessions {
		if !offers(s.Snapshot().Models, opus6) {
			t.Fatalf("session %d offers %v; want the fetched list", i, offeredIDs(s.Snapshot().Models))
		}
	}
	if n, _ := listRequests(api, listKeyA); n != 1 {
		t.Fatalf("%d model list requests from two hosts opening at once; want one", n)
	}
}

// TestNativeAnOlderRefreshNeverUndoesANewerOne (plan 044 P9; C5 review 1):
// two refreshes reach the Anthropic refresher in the other order from the one
// they read the table in, while key A's fetch is in flight. Refresh 1 reads
// the table funded by A and is held there (listSeam "decided"); B is saved,
// and refresh 2 reads it and leaves B's fetch pending; refresh 1, let go,
// submits A's — decided on the older reading, it changes nothing — and once
// A's answer comes, B's fetch follows, so the list left is B's. The control:
// B's fetch was pending before refresh 1 went on. Negative control: a
// submission not ordered by its reading would clear B's pending fetch (A's
// joins the fetch in flight), and B's list would never be fetched.
func TestNativeAnOlderRefreshNeverUndoesANewerOne(t *testing.T) {
	f, api := anthropicListFixture(t, listKeyA)
	replyA, arrivedA, releaseA, _ := heldReply(t, accountList(theAccount...))
	api.queue(replyA, accountList(listed{"claude-fable-6", "fable", "2026-10-11T00:00:00Z"}))
	decided, resume := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { closeOnce(resume) })
	var held atomic.Bool // the first refresh to decide is held; every later one goes on
	s := f.session(Options{})
	s.listSeam = func(stage string) {
		if stage == "decided" && held.CompareAndSwap(false, true) {
			close(decided)
			<-resume
		}
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	takeStartDelta(t, s.log)
	await(t, arrivedA, "key A's request")
	first := make(chan ModelsRefresh, 1)
	go func() {
		r, _ := s.RefreshModels(context.Background(), "")
		first <- r
	}()
	await(t, decided, "refresh 1, decided on A's table")
	storeAnthropicKey(t, f.dir, listKeyB)
	refreshApplied(t, s, "")
	if p := anthropicPending(s); p == nil || p.id() != modeltable.AnthropicKeyID(listKeyB) {
		t.Fatal("control: refresh 2 left no fetch for B pending")
	}
	close(resume)
	await(t, first, "refresh 1")
	if p := anthropicPending(s); p == nil || p.id() != modeltable.AnthropicKeyID(listKeyB) {
		t.Fatal("refresh 1, decided on the table before B was saved, undid B's pending fetch")
	}
	close(releaseA)
	await(t, anthropicFetch(s), "A's fetch and B's after it")
	n, keyedA := listRequests(api, listKeyA)
	_, keyedB := listRequests(api, listKeyB)
	if n != 2 || !keyedA[0] || !keyedB[1] {
		t.Fatalf("%d model list requests, with A %v, with B %v; want A's, then B's", n, keyedA, keyedB)
	}
	if doc := listFile(t, f.dir); doc.KeyID != modeltable.AnthropicKeyID(listKeyB) {
		t.Fatal("the list left is not B's")
	}
}

// TestNativeABackoffOutlivesAnotherKeysFetch (plan 044 P9; C5 review 2): a
// key's backoff after a failed fetch is its own, kept while another key's
// fetch runs and ends. Key A's fetch is refused; B is saved and fetched with
// at once; A is saved again within A's backoff, and nothing is fetched for
// it — whether its refresh comes with nothing in flight, or while B's fetch
// is (pending behind it). Once the backoff has passed, a refresh fetches
// with A again. The controls: B's fetch ran inside A's backoff, and in the
// pending case A's refresh was pending behind it. Negative controls: a
// backoff that B's fetch cleared or replaced, or that a pending fetch
// skipped, makes a request for A inside its backoff.
func TestNativeABackoffOutlivesAnotherKeysFetch(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%v", pending), func(t *testing.T) {
			f, api := anthropicListFixture(t, listKeyA)
			replyB, arrivedB, releaseB := accountList(theAccount...), chan struct{}(nil), chan struct{}(nil)
			if pending {
				replyB, arrivedB, releaseB, _ = heldReply(t, accountList(theAccount...))
			}
			api.queue(keyEcho, replyB, accountList(theAccount...))
			var (
				clockMu sync.Mutex
				now     = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
			)
			s := f.session(Options{})
			s.fetchClock = func() time.Time {
				clockMu.Lock()
				defer clockMu.Unlock()
				return now
			}
			if err := s.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			takeStartDelta(t, s.log)
			awaitAnthropicFetch(t, s) // A's, refused

			storeAnthropicKey(t, f.dir, listKeyB)
			refreshApplied(t, s, "")
			if pending {
				await(t, arrivedB, "B's request")
				storeAnthropicKey(t, f.dir, listKeyA)
				refreshApplied(t, s, "")
				if p := anthropicPending(s); p == nil || p.id() != modeltable.AnthropicKeyID(listKeyA) {
					t.Fatal("control: A's refresh is not pending behind B's fetch")
				}
				close(releaseB)
				awaitAnthropicFetch(t, s)
			} else {
				awaitAnthropicFetch(t, s) // B's
				storeAnthropicKey(t, f.dir, listKeyA)
				refreshApplied(t, s, "")
				awaitAnthropicFetch(t, s)
			}
			if n, keyed := listRequests(api, listKeyB); n != 2 || !keyed[1] {
				t.Fatalf("%d model list requests (the second with B: %v); want A's refused one and B's, and none for A inside its backoff", n, keyed)
			}

			clockMu.Lock()
			now = now.Add(listRetryBackoff)
			clockMu.Unlock()
			refresh(t, s, "")
			awaitAnthropicFetch(t, s)
			if n, keyed := listRequests(api, listKeyA); n != 3 || !keyed[2] {
				t.Fatalf("%d model list requests after A's backoff (the third with A: %v); want A fetched with again", n, keyed)
			}
		})
	}
}

// TestNativeAForcedFetchNeverOverwritesANewerList (plan 044 P8, A5): `craze
// auth login`'s forced fetch, whose answer comes after a session's
// background fetch — begun after it — has written the list for the same key,
// leaves that newer list standing, and the session keeps offering it. The
// control: a forced fetch with nothing newer landed writes its own list,
// which the session's next refresh takes up.
func TestNativeAForcedFetchNeverOverwritesANewerList(t *testing.T) {
	const haiku = "anthropic/claude-haiku-5-5"
	f, api := anthropicListFixture(t, listKeyA)
	haikuOnly := accountList(listed{"claude-haiku-5-5", "haiku", "2026-10-07T18:00:00Z"})
	forced, arrived, release, _ := heldReply(t, haikuOnly)
	api.queue(forced, accountList(theAccount...), haikuOnly)
	type result struct {
		n   int
		err error
	}
	login := make(chan result, 1)
	go func() {
		n, err := anthropicmodels.Fetch(context.Background(), f.dir, listKeyA)
		login <- result{n, err}
	}()
	await(t, arrived, "the forced fetch's request")
	s := f.started(Options{})
	await(t, anthropicFetch(s), "the session's fetch")
	close(release)
	if r := await(t, login, "the forced fetch"); r.err != nil || r.n != 2 {
		t.Fatalf("the forced fetch = %d, %v; want the newer list's 2 models", r.n, r.err)
	}
	if got := listIDs(listFile(t, f.dir)); !slices.Equal(got, []string{"claude-opus-6", "claude-haiku-5-5"}) {
		t.Fatalf("the list is %v; want the session's newer one", got)
	}
	refresh(t, s, "")
	awaitAnthropicFetch(t, s)
	if !offers(s.Snapshot().Models, opus6) {
		t.Fatalf("the session offers %v; want the newer list", offeredIDs(s.Snapshot().Models))
	}

	if n, err := anthropicmodels.Fetch(context.Background(), f.dir, listKeyA); err != nil || n != 1 {
		t.Fatalf("control: a forced fetch with nothing newer = %d, %v", n, err)
	}
	refreshApplied(t, s, "")
	awaitAnthropicFetch(t, s)
	if got := offeredIDs(s.Snapshot().Models); slices.Contains(got, opus6) || !slices.Contains(got, haiku) {
		t.Fatalf("control: after the forced fetch's own list the session offers %v; want Haiku alone of the account's", got)
	}
	if n, _ := listRequests(api, listKeyA); n != 3 {
		t.Fatalf("%d model list requests; want the two forced fetches' and the session's", n)
	}
}

// TestNativeAFailedPlanFetchBacksOffForItsAccount (plan 047 D8): the ChatGPT
// plan's list id is the signed-in account's, so a fetch the API refuses
// starts a backoff for that account alone. Account A's open fetch fails, and a
// refresh within the backoff fetches nothing; account B, signed in meanwhile,
// is fetched at once, within A's backoff; B's own failure then backs B off.
// Negative control: with the id "" for every account, B's refresh is held back
// by A's failure and the request count stays at one.
func TestNativeAFailedPlanFetchBacksOffForItsAccount(t *testing.T) {
	api, _ := noOpenAI(t)
	refuse := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"code":"refused","message":"refused"}}`)
	}
	api.queue(refuse, refuse, refuse, refuse)
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	dir := filepath.Join(home, "native")
	writePlanAccount(t, dir)
	writePlanTokens(t, dir, planAccess, planRefresh, "inc-1", 1)
	staleModels(t, dir)
	s := newNative(Options{Workspace: t.TempDir(), ContentHome: t.TempDir()}, func(o *harness.Options) {
		o.Getenv = func(string) string { return "" }
	})
	closeAtCleanup(t, s)
	var (
		clockMu sync.Mutex
		now     = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	)
	s.fetchClock = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitPlanFetch(t, s)
	modelRequests := func() int {
		n := 0
		for _, r := range api.requests() {
			if strings.HasSuffix(r.path, "/models") {
				n++
			}
		}
		return n
	}
	if n := modelRequests(); n != 1 {
		t.Fatalf("%d model list requests after the open; want A's one", n)
	}
	refresh(t, s, "")
	awaitPlanFetch(t, s)
	if n := modelRequests(); n != 1 {
		t.Fatalf("%d model list requests for A within its backoff; want none more", n)
	}

	if err := signInOf(dir, otherSubject, otherClient, otherAccess, otherRefresh, "inc-2"); err != nil {
		t.Fatal(err)
	}
	refresh(t, s, "")
	awaitPlanFetch(t, s)
	if n := modelRequests(); n != 2 {
		t.Fatalf("%d model list requests after B signed in; want B's at once, within A's backoff", n)
	}
	refresh(t, s, "")
	awaitPlanFetch(t, s)
	if n := modelRequests(); n != 2 {
		t.Fatalf("%d model list requests for B within its own backoff; want none more", n)
	}
}

// TestPlanSourceIDIsTheStoredAccounts (plan 047 D8): planSource names the list
// for the account in the stored sign-in — two accounts, two ids, each 12 hex
// digits and never the subject — and a directory with no stored sign-in has
// the id "". Negative control: the constant "" id fails the distinct-ids
// check.
func TestPlanSourceIDIsTheStoredAccounts(t *testing.T) {
	noOpenAI(t)
	idOf := func(subject string) string {
		dir := t.TempDir()
		writePlanAccount(t, dir)
		if err := writePlanTokensOf(dir, subject, planClient, planAccess, planRefresh, "inc-1", 1); err != nil {
			t.Fatal(err)
		}
		src := planSource(chatgptauth.Source(dir))
		if src == nil {
			t.Fatalf("no plan source for subject %q", subject)
		}
		return src.id()
	}
	a, b := idOf(planSubject), idOf(otherSubject)
	if len(a) != 12 || len(b) != 12 || a == b || strings.Contains(a, planSubject) {
		t.Fatalf("ids %q and %q; want two distinct 12-hex-digit ids", a, b)
	}
	if again := idOf(planSubject); again != a {
		t.Fatalf("one account's id changed: %q then %q", a, again)
	}
	noRecord := t.TempDir()
	writePlanAccount(t, noRecord) // a registration, no token file
	if none := planAccountID(noRecord); none != "" {
		t.Fatalf("a directory with no stored sign-in has id %q; want \"\"", none)
	}
}
