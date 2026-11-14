package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/websearch"
)

// The native session's web tally (plan 051 F.4.1–F.4.2, native_webtally.go):
// windows, the idle carry, webLast, the restatement and the close's freeze.
// The engine is not imported here (its tests may not import the harness):
// where a case needs the engine's turn events, it enqueues them through the
// session's own log exactly where the engine does — a turn's started before
// its continuation runs, its ended after the continuation returns, and a
// close's freeze then synthetic closing ending in that order (engine.Close)
// — and the integration over the real engine is internal/tui's
// native_webtally_test.go. Reports come from the real web service against a
// fake MCP endpoint (webEndpoint) where the case is about the path, and from
// direct webReport calls where it is about a schedule.

// webEndpoint is a fake MCP endpoint on loopback: a search for q answers one
// result, https://r.example/<q>, unless results names its text; a search for
// "block" waits for its request to be cancelled; a fetch answers a page.
type webEndpoint struct {
	srv     *httptest.Server
	started chan struct{}

	mu      sync.Mutex
	results map[string]string
}

func newWebEndpoint(t *testing.T) *webEndpoint {
	t.Helper()
	e := &webEndpoint{started: make(chan struct{}, 8), results: map[string]string{}}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			ID     int64 `json:"id"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &call)
		text := "# Page\n\nthe page"
		if call.Params.Name == "web_search_exa" {
			q, _ := call.Params.Arguments["query"].(string)
			if q == "block" {
				e.started <- struct{}{}
				<-r.Context().Done()
				return
			}
			e.mu.Lock()
			res, ok := e.results[q]
			e.mu.Unlock()
			if !ok {
				res = "Title: R\nURL: https://r.example/" + q + "\nText: x"
			}
			text = res
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", out)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *webEndpoint) url() string { return e.srv.URL + "/mcp" }

// answerWith makes a search for q answer text.
func (e *webEndpoint) answerWith(q, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.results[q] = text
}

// webOn is a fixture whose sessions reach e, and the options of a session
// that opens with web search on (--web-search), so no consent is asked.
func webOn(t *testing.T) (*nativeFixture, *webEndpoint, Options) {
	t.Helper()
	f := newNativeFixture(t)
	e := newWebEndpoint(t)
	f.env[websearch.TestEndpointEnv] = e.url()
	return f, e, Options{WebFlag: WebFlagOn}
}

// searchStep is a step that calls websearch with q and goes on.
func searchStep(t *testing.T, id, q string) step {
	t.Helper()
	return nativeCallStep(id, tool.WebSearchTool, searchArgs(t, q))
}

// fetchStep is a step that calls webfetch on u and goes on.
func fetchStep(t *testing.T, id, u string) step {
	t.Helper()
	return nativeCallStep(id, tool.WebFetchTool, nativeArgs(t, map[string]any{"url": u}))
}

// webOf is ev's web section, nil for none.
func webOf(ev Event) *WebSearchState {
	if ev.Type == EventMeta && ev.State != nil {
		return ev.State.WebSearch
	}
	return nil
}

// webAt is the index of every web delta in evs.
func webAt(evs []Event) []int {
	var out []int
	for i, ev := range evs {
		if webOf(ev) != nil {
			out = append(out, i)
		}
	}
	return out
}

// turnEv is an engine turn event, as the engine enqueues it.
func turnEv(id, phase string) Event {
	return Event{Type: EventTurn, At: time.Now(), Turn: &TurnInfo{ID: id, Phase: phase}}
}

// closingEv is the synthetic closing ending engine.Close gives the turn it cuts.
func closingEv(id string) Event {
	return Event{Type: EventTurn, At: time.Now(), Turn: &TurnInfo{ID: id, Phase: TurnEnded, Synthetic: true, StopReason: "closing"}}
}

// isTurn accepts the engine turn event of id and phase.
func isTurn(id, phase string) func(Event) bool {
	return func(ev Event) bool {
		return ev.Type == EventTurn && ev.Turn != nil && ev.Turn.ID == id && ev.Turn.Phase == phase
	}
}

// markN numbers the marks a test enqueues.
var markN atomic.Int64

// mark enqueues a marker delta behind everything enqueued so far and waits for
// the watcher to see it: everything before it is then in w's events.
func mark(t *testing.T, s *nativeSession, w *nativeWatcher) []Event {
	t.Helper()
	reason := "mark-" + strconv.FormatInt(markN.Add(1), 10)
	s.log.Enqueue(Event{Type: EventMeta, At: time.Now(), State: &StateDelta{Reason: reason}})
	w.wait(reason, func(ev Event) bool { return ev.Type == EventMeta && ev.State != nil && ev.State.Reason == reason })
	return w.events()
}

// webTallies is a copy of the session's tallies and its window's flags.
type webTallies struct {
	web, idle, last WebSearchState
	open, frozen    bool
}

// webSnap is a copy of the session's tallies, their URLs as held.
func webSnap(s *nativeSession) webTallies {
	s.mu.Lock()
	defer s.mu.Unlock()
	var held *redact.Replacer // no key: the URLs as held
	return webTallies{web: *s.web.state(held), idle: *s.webIdle.state(held), last: *s.webLast.state(held), open: s.webOpen, frozen: s.webFrozen}
}

// webHeldText is the text of a held URL.
func webHeldText(u webHeld) string { return u.text }

// noKeys is a redactor that knows no key.
var noKeys *redact.Replacer

// tallyOf is a tally of calls, as the session holds it, reported with no key
// to redact.
func tallyOf(calls ...tool.WebCall) *webTally {
	t := &webTally{}
	for _, c := range calls {
		t.add(c, noKeys)
	}
	return t
}

// published is t's published value with no key to redact.
func published(t *webTally) WebSearchState {
	return *t.state(noKeys)
}

// heldPrompt is a started session (its webReportSeam seam, nil for none) with
// a watcher, whose prompt "go" — behind turn's started when turn is named —
// is held at its one step, its window open: the session, the watcher, the
// held step and the prompt's outcome.
func heldPrompt(t *testing.T, turn string, seam func()) (*nativeSession, *nativeWatcher, *held, <-chan outcome) {
	t.Helper()
	f := newNativeFixture(t)
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("x"), finishParts(fantasy.FinishReasonStop)))
	s := f.session(Options{})
	s.webReportSeam = seam
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	takeStartDelta(t, s.log)
	w := newNativeWatcher(t, s)
	if turn != "" {
		s.log.Enqueue(turnEv(turn, TurnStarted))
	}
	out := startPrompt(s, "go")
	await(t, h.reached, "the turn's step")
	return s, w, h, out
}

// reportHold is a webReportSeam that holds the first report after it is
// armed at the attribution point: reached closes as it is held, release lets
// it go.
type reportHold struct {
	armed            atomic.Bool
	reached, release chan struct{}
}

func newReportHold() *reportHold {
	return &reportHold{reached: make(chan struct{}), release: make(chan struct{})}
}

func (r *reportHold) seam() {
	if r.armed.CompareAndSwap(true, false) {
		close(r.reached)
		<-r.release
	}
}

func search(urls ...string) tool.WebCall { return tool.WebCall{URLs: urls} }

// endedCtx is a context that has already ended.
func endedCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// ---------------------------------------------------------------- URLs

// TestWebTallyURLRules (F.4.1's pipeline, steps 3–5): duplicates listed once,
// first seen first; a URL over 500 bytes cut back to a UTF-8 boundary, two
// equal after the cut listed once with urlsTruncated; the 101st distinct URL
// dropped with urlsTruncated, and exactly 100 kept without it.
func TestWebTallyURLRules(t *testing.T) {
	tl := published(tallyOf(search("https://b.example/", "https://a.example/", "https://b.example/"), search("https://a.example/", "https://c.example/")))
	if want := []string{"https://b.example/", "https://a.example/", "https://c.example/"}; !slices.Equal(tl.URLs, want) || tl.URLsTruncated || tl.Searches != 2 {
		t.Fatalf("tally %+v; want %v, first seen first, nothing truncated", tl, want)
	}

	for _, prefix := range []int{0, 1} {
		long := "https://x.example/" + strings.Repeat("a", prefix) + strings.Repeat("é", 300)
		cut, did := cutWebURL(long)
		if !did || len(cut) > webURLMax || !utf8.ValidString(cut) || !strings.HasPrefix(long, cut) || len(cut) < webURLMax-utf8.UTFMax {
			t.Fatalf("prefix %d: cut to %d bytes (cut=%v, valid=%v)", prefix, len(cut), did, utf8.ValidString(cut))
		}
	}
	if u, did := cutWebURL(strings.Repeat("a", webURLMax)); did || len(u) != webURLMax {
		t.Fatalf("a URL of exactly %d bytes was cut", webURLMax)
	}

	base := "https://x.example/" + strings.Repeat("a", 600)
	if cutT := published(tallyOf(search(base+"1"), search(base+"2"))); len(cutT.URLs) != 1 || !cutT.URLsTruncated {
		t.Fatalf("two URLs equal after the cut: %d listed, truncated=%v; want 1, true", len(cutT.URLs), cutT.URLsTruncated)
	}

	hundred, more := &webTally{}, &webTally{}
	for i := range 101 {
		u := fmt.Sprintf("https://n.example/%d", i)
		if i < 100 {
			hundred.add(search(u), noKeys)
		}
		more.add(search(u), noKeys)
	}
	if h := published(hundred); len(h.URLs) != 100 || h.URLsTruncated {
		t.Fatalf("100 distinct URLs: %d kept, truncated=%v", len(h.URLs), h.URLsTruncated)
	}
	if m := published(more); len(m.URLs) != 100 || !m.URLsTruncated || m.URLs[99] != "https://n.example/99" || m.Searches != 101 {
		t.Fatalf("101 distinct URLs: %d kept, truncated=%v, last %q", len(m.URLs), m.URLsTruncated, m.URLs[len(m.URLs)-1])
	}
	// A duplicate past the cap is not a dropped distinct value.
	hundred.add(search("https://n.example/3"), noKeys)
	if published(hundred).URLsTruncated {
		t.Fatal("a duplicate at the cap marked the list truncated")
	}
	if len(more.urls) != webURLsMax {
		t.Fatalf("101 distinct URLs: %d held; want the %d published", len(more.urls), webURLsMax)
	}
}

// TestWebTallyRedactsAtEachPublication (review fix1, fix3): the tally holds a
// URL as the service returned it, and publishes it redacted with the
// publication's redactor — in the session, the tally's own, which holds every
// key of the report's and those learned since (TestWebChildKey… cover that
// union) — at most webURLHeld bytes held, one per published form at the
// report, so URLs that collapse into one form leave room for a later distinct
// one; and a published list is its own.
func TestWebTallyRedactsAtEachPublication(t *testing.T) {
	const early, late = "sk-known-at-the-report-0321", "sk-learned-after-it-0322"
	knowsEarly, knowsLate := redact.New(early), redact.New(late)
	tl := &webTally{}
	tl.add(search("https://e.example/?k="+early, "https://l.example/?k="+late), knowsEarly)
	if tl.urls[0].text != "https://e.example/?k="+early || tl.urls[1].text != "https://l.example/?k="+late {
		t.Fatalf("the tally holds %q, %q; want the URLs as returned", tl.urls[0].text, tl.urls[1].text)
	}
	want := []string{"https://e.example/?k=" + redact.Marker, "https://l.example/?k=" + redact.Marker}
	if got := tl.state(knowsEarly.Union(knowsLate)); !slices.Equal(got.URLs, want) {
		t.Fatalf("published with the report's keys and the later one: %q; want both keys redacted", got.URLs)
	}

	// The held bound: a long URL is held to webURLHeld bytes at a UTF-8
	// boundary, and published cut to webURLMax.
	long := &webTally{}
	long.add(search("https://long.example/"+strings.Repeat("é", 4*webURLHeld)), noKeys)
	if h := long.urls[0]; len(long.urls) != 1 || !h.cut || len(h.text) > webURLHeld || len(h.text) < webURLHeld-utf8.UTFMax || !utf8.ValidString(h.text) {
		t.Fatalf("a long URL is held as %d bytes (cut=%v)", len(h.text), h.cut)
	}
	if got := published(long); len(got.URLs[0]) > webURLMax || !got.URLsTruncated {
		t.Fatalf("a long URL is published as %d bytes, truncated=%v", len(got.URLs[0]), got.URLsTruncated)
	}

	// 400 URLs with one published form hold one slot; a distinct one after
	// them is held and published.
	base := "https://same.example/" + strings.Repeat("s", webURLMax)
	collapse := &webTally{}
	for i := range 400 {
		collapse.add(search(fmt.Sprintf("%s/%d", base, i)), noKeys)
	}
	collapse.add(search("https://other.example/"), noKeys)
	if got := published(collapse); len(collapse.urls) != 2 || !slices.Equal(got.URLs, []string{base[:webURLMax], "https://other.example/"}) || !got.URLsTruncated {
		t.Fatalf("collapsing URLs: %d held, published %q, truncated=%v", len(collapse.urls), got.URLs, got.URLsTruncated)
	}

	// Two held URLs that differ only in keys learned later are published once.
	fold := tallyOf(search("https://f.example/?k=k001-secret", "https://f.example/?k=k002-secret"))
	got := fold.state(redact.New("k001-secret", "k002-secret"))
	if !slices.Equal(got.URLs, []string{"https://f.example/?k=" + redact.Marker}) {
		t.Fatalf("folded by later keys: %q", got.URLs)
	}
	got = fold.state(noKeys)
	got.URLs[0] = "mutated"
	if fold.urls[0].text != "https://f.example/?k=k001-secret" || published(fold).URLs[0] == "mutated" {
		t.Fatal("a published list shares its backing array with the tally")
	}
}

// TestWebTallyHeldCutHidesAKeysStart (review fix2 #2): a URL held cut at
// webURLHeld bytes ends in the first bytes of a key B that its report did
// not know; ten occurrences of a 400-byte key A come before it. Both learned
// before the publication, A's redaction pulls what follows it forward by
// thousands of bytes — B's start included, into the first 500 — so the
// publication must treat a held URL that was cut as the start of a longer
// text: B's halved start is not shown.
func TestWebTallyHeldCutHidesAKeysStart(t *testing.T) {
	keyA := "qa-" + strings.Repeat("0123456789", 40)[:397]
	const keyB = "zb7-the-second-key-0331"
	head := "https://x.example/?a=" + strings.Repeat(keyA, 10) + "&pad="
	raw := head + strings.Repeat("p", 4088-len(head)) + keyB + "&tail"
	if len(keyA) != 400 || strings.Index(raw, keyB) != 4088 {
		t.Fatalf("fixture: A is %d bytes, B at %d", len(keyA), strings.Index(raw, keyB))
	}
	tl := tallyOf(search(raw))
	got := tl.state(redact.New(keyA, keyB))
	if len(got.URLs) != 1 || strings.Contains(got.URLs[0], keyA[:8]) || strings.Contains(got.URLs[0], keyB[:8]) {
		t.Fatalf("published %q; want both keys redacted and no piece of either", got.URLs)
	}
}

// TestWebTallyOverlappingKeyLearnedLater (review fix3 #1): a URL holds key B,
// which begins with key A. Its report knows A alone; B is learned before the
// publication. Published, the URL is redacted as if B had been known at the
// report — B whole, its private tail with it — since the tally publishes
// from the URL as the service returned it, not from the report's redaction
// (which would have left B's tail behind A's marker, where B no longer
// matches).
func TestWebTallyOverlappingKeyLearnedLater(t *testing.T) {
	const keyA = "qz9k-base-secret-051"
	const keyB = keyA + "-new-private-tail"
	tl := &webTally{}
	tl.add(search("https://example.com/?k="+keyB), redact.New(keyA))
	got := tl.state(redact.New(keyA, keyB))
	if want := []string{"https://example.com/?k=" + redact.Marker}; !slices.Equal(got.URLs, want) {
		t.Fatalf("published %q; want %q", got.URLs, want)
	}
}

// TestWebTallyStoresByPublishedForm (review fix3 #2): 100 URLs, each cut at
// webURLHeld bytes inside a different long key the redactor knows, publish as
// one form — Head hides each halved key — so they take one slot of the
// tally's 100, and a distinct URL reported after them is still published.
func TestWebTallyStoresByPublishedForm(t *testing.T) {
	const base = "https://same.example/"
	keys := make([]string, webURLsMax)
	for i := range keys {
		keys[i] = fmt.Sprintf("qz9k-%03d-", i) + strings.Repeat("p", 4100)
	}
	red := redact.New(keys...)
	tl := &webTally{}
	for _, k := range keys {
		tl.add(search(base+k[:webURLHeld-len(base)]+"X"), red)
	}
	tl.add(search("https://other.example/"), red)
	got := tl.state(red)
	if want := []string{base, "https://other.example/"}; !slices.Equal(got.URLs, want) || !got.URLsTruncated {
		t.Fatalf("published %d URLs (first %.40q), truncated=%v; want %q, truncated", len(got.URLs), got.URLs[0], got.URLsTruncated, want)
	}
}

// TestWebTallyFoldedDuplicatesAreTruncated (review fix4): a URL admission
// folds into a held one with the same published form is left out, and the
// tally says so — urlsTruncated — whenever what was left out was not that
// very URL: two URLs that differ only behind keys the redactor knows now,
// which a key learned later tells apart again; and a URL of 500 bytes then
// the same URL one byte longer, which the cut folds into it. The same URL
// reported twice leaves nothing out.
func TestWebTallyFoldedDuplicatesAreTruncated(t *testing.T) {
	const keyA, keyB = "qz9k-alpha-secret-051", "qz9k-bravo-secret-051"
	known := redact.New(keyA, keyB)
	folded := &webTally{}
	folded.add(search("https://example.com/?k="+keyA+"-tail"), known)
	folded.add(search("https://example.com/?k="+keyB+"-tail"), known)
	if len(folded.urls) != 1 {
		t.Fatalf("fixture: %d held; want the two folded into one", len(folded.urls))
	}
	got := folded.state(known.Union(redact.New(keyA + "-tail")))
	if !slices.Equal(got.URLs, []string{"https://example.com/?k=" + redact.Marker}) || !got.URLsTruncated {
		t.Errorf("published %q, truncated=%v; want the held URL alone, truncated: the other is told apart now", got.URLs, got.URLsTruncated)
	}

	u := "https://cut.example/" + strings.Repeat("u", webURLMax-len("https://cut.example/"))
	cut := tallyOf(search(u), search(u+"z"))
	if got := published(cut); !slices.Equal(got.URLs, []string{u}) || !got.URLsTruncated {
		t.Errorf("a %d-byte URL then one byte longer: published %d URLs, truncated=%v; want one, truncated", len(u), len(got.URLs), got.URLsTruncated)
	}

	twice := tallyOf(search("https://same.example/a"), search("https://same.example/a"))
	if got := published(twice); !slices.Equal(got.URLs, []string{"https://same.example/a"}) || got.URLsTruncated {
		t.Errorf("the same URL twice: published %q, truncated=%v; want it once, nothing truncated", got.URLs, got.URLsTruncated)
	}
}

// TestWebTallyHeldURLsOwnTheirBytes (review fix2 #3): a held URL cut from a
// long one is a copy, not a slice of it, so the tally keeps webURLHeld bytes
// per URL, never the reply the URL came in: 100 distinct URLs of about 200 KiB
// each are held in strings none of which points into its input.
func TestWebTallyHeldURLsOwnTheirBytes(t *testing.T) {
	tl := &webTally{}
	inputs := make([]string, webURLsMax)
	for i := range inputs {
		inputs[i] = fmt.Sprintf("https://big.example/%03d/", i) + strings.Repeat("b", 200<<10)
		tl.add(search(inputs[i]), noKeys)
	}
	if len(tl.urls) != webURLsMax {
		t.Fatalf("%d held; want %d", len(tl.urls), webURLsMax)
	}
	for i, u := range tl.urls {
		in := inputs[i]
		start, end := uintptr(unsafe.Pointer(unsafe.StringData(in))), uintptr(unsafe.Pointer(unsafe.StringData(in)))+uintptr(len(in))
		if p := uintptr(unsafe.Pointer(unsafe.StringData(webHeldText(u)))); p >= start && p < end {
			t.Fatalf("held URL %d (%d bytes) points into its %d-byte input", i, len(webHeldText(u)), len(in))
		}
	}
}

// TestWebChildKeyCoveredAtAPublicationStaysCovered (review fix2 #1): the
// parent reports a URL holding K before any child knows K, so its report
// leaves K raw; a child that opens knowing K then reports a call, and that
// publication — the harness's redactor covering K while the child runs —
// redacts K in the earlier URL. Coverage once had is kept: after the child
// retires and the parent's turn returns, the restatement still redacts K.
func TestWebChildKeyCoveredAtAPublicationStaysCovered(t *testing.T) {
	const childKey = "sk-the-child-learns-it-later-0341"
	f, r := routedNative(t)
	env := newLearnableEnv(f)
	s := f.started(Options{})
	w := newNativeWatcher(t, s)
	env.set("NATIVE_NOKEY_KEY", childKey)
	a := r["test/a"]
	a.route("go", func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		s.webReport(search("https://k.example/?key=" + childKey))
		callsStep(agentCall(t, "a1", "looks", "look it up"))(ctx, yield)
	}, answer("noted"))
	a.route("look it up", func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		s.webReport(search("https://other.example/"))
		answer("found it")(ctx, yield)
	})
	s.log.Enqueue(turnEv("turn-1", TurnStarted))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	s.log.Enqueue(turnEv("turn-1", TurnEnded))
	evs := mark(t, s, w)
	if s.hs.Redact(childKey) != childKey {
		t.Fatal("control: the parent learned the key")
	}
	at := webAt(evs)
	if len(at) < 3 {
		t.Fatalf("%d web deltas; want the parent's report's, the child's and the restatement", len(at))
	}
	if got := webOf(evs[at[0]]); !slices.Equal(got.URLs, []string{"https://k.example/?key=" + childKey}) {
		t.Fatalf("control: the parent's report published %q; want the key raw, which nothing knew yet", got.URLs)
	}
	want := []string{"https://k.example/?key=" + redact.Marker, "https://other.example/"}
	for _, i := range at[1:] {
		if got := webOf(evs[i]); !slices.Equal(got.URLs, want) {
			t.Fatalf("a web delta after the child's report is %q; want %q", got.URLs, want)
		}
	}
}

// TestWebTallyRedactsBeforeTheCut (F.4.1 step 2): the session's redactor runs
// on every URL before the cut and before distinctness — two URLs that differ
// only in a key the session knows are listed once, and a key that straddles
// the 500-byte cut leaves no piece of itself behind.
func TestWebTallyRedactsBeforeTheCut(t *testing.T) {
	s, w, h, out := heldPrompt(t, "", nil)

	s.webReport(search("https://k.example/?key="+nativeCanary, "https://k.example/?key="+nativeCanaryOther))
	pad := "https://k.example/?q=" + strings.Repeat("p", webURLMax-15-len("https://k.example/?q="))
	straddle := pad + nativeCanary + "&tail"
	if i := strings.Index(straddle, nativeCanary); i >= webURLMax || i+len(nativeCanary) <= webURLMax {
		t.Fatalf("fixture: the key is at %d..%d, not across the cut", i, i+len(nativeCanary))
	}
	s.webReport(search(straddle))
	close(h.release)
	await(t, out, "the turn")
	evs := mark(t, s, w)
	at := webAt(evs)
	if len(at) == 0 {
		t.Fatal("no web delta")
	}
	last := webOf(evs[at[len(at)-1]])
	// The pair is listed once; the straddling URL, redacted, is longer than
	// the cut (the marker is longer than the key) and is cut after it.
	if len(last.URLs) != 2 || !last.URLsTruncated || !strings.Contains(last.URLs[1], "[craze:redacte") || len(last.URLs[1]) > webURLMax {
		t.Fatalf("the tally is %+v; want the redacted pair listed once and the straddling URL redacted, then cut", last)
	}
	for _, i := range at {
		for _, u := range webOf(evs[i]).URLs {
			if strings.Contains(u, "sk-can") || strings.Contains(u, nativeCanary[len(nativeCanary)-6:]) {
				t.Fatalf("a published URL carries a piece of a key: %q", u)
			}
		}
	}
}

// TestWebKeyInAURLIsNeverPublished: the web service's key in a result's URL —
// as itself, percent-encoded, with its spaces as "+", and straddling the
// 500-byte cut — is in no delta the session publishes and no journal record:
// the service's scrub and the session's redactor have both run before the
// tally holds it. (The snapshot's turn.webSearch, folded from the same
// deltas, is internal/tui's TestWebTallyOverTheEngine.)
func TestWebKeyInAURLIsNeverPublished(t *testing.T) {
	const key = "exa key 0123456789abcdef"
	f, e, opts := webOn(t)
	f.env["EXA_API_KEY"] = key
	long := "https://leak.example/?q=" + strings.Repeat("p", webURLMax-8-len("https://leak.example/?q="))
	e.answerWith("leak", strings.Join([]string{
		"Title: Raw\nURL: https://leak.example/?k=" + key + "\nText: a",
		"Title: Pct\nURL: https://leak.example/?k=" + url.PathEscape(key) + "\nText: b",
		"Title: Plus\nURL: https://leak.example/?k=" + url.QueryEscape(key) + "\nText: c",
		"Title: Long\nURL: " + long + url.QueryEscape(key) + "\nText: d",
	}, "\n---\n"))
	f.models["test/a"].push(searchStep(t, "w1", "leak"), answer("done"))
	opts.JournalDir = t.TempDir()
	s := f.session(opts)
	jw := journalOf(t, s.log)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	takeStartDelta(t, s.log)
	w := newNativeWatcher(t, s)
	if _, err := s.Prompt(context.Background(), "look"); err != nil {
		t.Fatal(err)
	}
	evs := mark(t, s, w)
	at := webAt(evs)
	if len(at) < 2 {
		t.Fatalf("%d web deltas, want the call's and the restatement", len(at))
	}
	final := webOf(evs[at[len(at)-1]])
	if final.Searches != 1 || len(final.URLs) == 0 {
		t.Fatalf("the tally is %+v", final)
	}
	spellings := []string{key, url.PathEscape(key), url.QueryEscape(key), "0123456789abcdef", "0123456789"}
	for _, i := range at {
		for _, u := range webOf(evs[i]).URLs {
			for _, sp := range spellings {
				if strings.Contains(u, sp) {
					t.Fatalf("a published URL %q carries the key (%q)", u, sp)
				}
			}
		}
	}
	closeJournaled(t, s, jw)
	files := journalFiles(t, opts.JournalDir)
	if len(files) == 0 {
		t.Fatal("no journal")
	}
	sawWeb := false
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		sawWeb = sawWeb || strings.Contains(string(b), `"webSearch"`)
		for _, sp := range spellings {
			if strings.Contains(string(b), sp) {
				t.Fatalf("the journal %s carries the key (%q)", p, sp)
			}
		}
	}
	if !sawWeb {
		t.Fatal("the journal holds no web section: the check above checked nothing")
	}
}

// webLearnedKey is a web search key the session learns only after a URL
// holding it was counted; its first bytes are a spelling nothing else holds.
const webLearnedKey = "qz9k-learned-after-the-count-0301"

// idleKeyURLs are two URLs holding key: one with it in its query, and one
// over the 500-byte cut with it straddling byte 500.
func idleKeyURLs(t *testing.T, key string) (short, straddle string) {
	t.Helper()
	short = "https://idle.example/?k=" + key
	pad := "https://idle.example/?q=" + strings.Repeat("p", webURLMax-12-len("https://idle.example/?q="))
	straddle = pad + key + "&tail"
	if i := strings.Index(straddle, key); i >= webURLMax || i+len(key) <= webURLMax {
		t.Fatalf("fixture: the key is at %d..%d, not across the cut", i, i+len(key))
	}
	return short, straddle
}

// checkLearnedKeyRedacted checks that every web delta in evs holds both idle
// URLs with key redacted — the short one whole, the straddling one redacted,
// then cut, so the cut falls inside the marker — and no piece of key: the
// last of them is the restatement a waiting command's result reads.
func checkLearnedKeyRedacted(t *testing.T, evs []Event, key, short, straddle string) {
	t.Helper()
	at := webAt(evs)
	if len(at) == 0 {
		t.Fatal("no web delta")
	}
	wantLong, _ := cutWebURL(strings.ReplaceAll(straddle, key, redact.Marker))
	if i := strings.LastIndex(wantLong, "["); i < 0 || len(wantLong)-i >= len(redact.Marker) || !strings.HasPrefix(redact.Marker, wantLong[i:]) {
		t.Fatalf("fixture: the cut does not fall inside the marker: %q", wantLong[len(wantLong)-20:])
	}
	want := []string{strings.ReplaceAll(short, key, redact.Marker), wantLong}
	for _, i := range at {
		got := webOf(evs[i])
		for _, u := range got.URLs {
			if strings.Contains(u, key[:4]) {
				t.Fatalf("a web delta %+v carries a piece of the key learned before it was published: %q", got, u)
			}
		}
		if !slices.Equal(got.URLs, want) || !got.URLsTruncated || got.Searches != 1 {
			t.Fatalf("a web delta is %+v; want %q, the learned key redacted and the long URL then cut", got, want)
		}
	}
}

// TestWebIdleURLRedactedWithAKeyLearnedSince (review branch F1): a URL counted
// while no window was open, holding a key stored after it was counted, is
// published with that key redacted by the next prompt: the prompt's start
// learns the key before its window opens, so the opening delta — which
// carries the idle URL — and the restatement both redact it; a key straddling
// the 500-byte cut leaves no piece of itself, since the redactor runs before
// the cut at each publication.
func TestWebIdleURLRedactedWithAKeyLearnedSince(t *testing.T) {
	f := newNativeFixture(t)
	f.models["test/a"].push(answer("one"), answer("two"))
	s := f.started(Options{})
	w := newNativeWatcher(t, s)
	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	short, straddle := idleKeyURLs(t, webLearnedKey)
	s.webReport(search(short, straddle))
	storeWebKey(t, f.dir, webLearnedKey)
	s.mu.Lock()
	hs := s.hs
	s.mu.Unlock()
	if got := hs.Redact(webLearnedKey); got != webLearnedKey {
		t.Fatal("control: the key was learned before the prompt started")
	}

	s.log.Enqueue(turnEv("turn-2", TurnStarted))
	if _, err := s.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	s.log.Enqueue(turnEv("turn-2", TurnEnded))
	evs := mark(t, s, w)
	if at := webAt(evs); len(at) != 2 {
		t.Fatalf("%d web deltas; want the opening's and the restatement", len(at))
	}
	checkLearnedKeyRedacted(t, evs, webLearnedKey, short, straddle)
}

// TestWebIdleURLRedactedWithAKeyAWakeLearned (review branch F1): an idle
// background child's URL holding a key stored after it was counted — the
// wake learns the key before it opens its window — is published with the key
// redacted in the wake's opening delta and its restatement.
func TestWebIdleURLRedactedWithAKeyAWakeLearned(t *testing.T) {
	rig, _ := webWakeRig(t)
	s := rig.s
	child := newHeld(t)
	id := rig.spawnOne(child, answer("noted"), answer("later"))
	short, straddle := idleKeyURLs(t, webLearnedKey)
	s.webReport(search(short, straddle))
	storeWebKey(t, rig.f.dir, webLearnedKey)
	rig.finish(child, id)
	rig.awaitDecided(true, "the result pending")
	rig.bracket(false, 1, "wake-1")
	evs := mark(t, s, rig.w)
	op, en := bracketAt(evs, "wake-1", true), bracketAt(evs, "wake-1", false)
	at := webAt(evs)
	if len(at) != 2 || at[0] <= op || at[1] >= en {
		t.Fatalf("web deltas at %v (wake %d..%d); want the carried call's and the restatement inside the wake", at, op, en)
	}
	checkLearnedKeyRedacted(t, evs, webLearnedKey, short, straddle)
}

// TestWebChildKeyStaysRedactedAfterTheChildRetires (review fix1 F1): a URL a
// running child's call reported holds a key only that child knows. The
// harness's redactor covers it while the child runs and for the rest of the
// parent's harness turn, and no longer once that turn has returned — before
// the window's restatement. Redacted when it was reported, the URL stays
// redacted in every delta, the restatement (what a result reads) included.
func TestWebChildKeyStaysRedactedAfterTheChildRetires(t *testing.T) {
	const childKey = "sk-only-the-child-knows-it-0311"
	f, r := routedNative(t)
	env := newLearnableEnv(f)
	s := f.started(Options{})
	w := newNativeWatcher(t, s)
	env.set("NATIVE_NOKEY_KEY", childKey)
	a := r["test/a"]
	a.route("go", callsStep(agentCall(t, "a1", "looks", "look it up")), answer("noted"))
	var during string
	a.route("look it up", func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		during = s.hs.Redact(childKey)
		s.webReport(search("https://k.example/?key=" + childKey))
		answer("found it")(ctx, yield)
	})
	s.log.Enqueue(turnEv("turn-1", TurnStarted))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	s.log.Enqueue(turnEv("turn-1", TurnEnded))
	evs := mark(t, s, w)
	if during != redact.Marker || s.hs.Redact(childKey) != childKey {
		t.Fatalf("control: the parent's redactor held the child's key %q while it ran and %q after; want it while it ran only",
			during, s.hs.Redact(childKey))
	}
	at := webAt(evs)
	if len(at) < 2 {
		t.Fatalf("%d web deltas; want the report's and the restatement", len(at))
	}
	for _, i := range at {
		if got := webOf(evs[i]); !slices.Equal(got.URLs, []string{"https://k.example/?key=" + redact.Marker}) {
			t.Fatalf("a web delta is %+v; want the child's key redacted", got)
		}
	}
}

// TestWebCollapsingURLsKeepALaterDistinctOne (review fix1 F3): 400 distinct
// URLs that share their first 500 bytes publish as one value, cut; a distinct
// URL reported after them is still published beside it.
func TestWebCollapsingURLsKeepALaterDistinctOne(t *testing.T) {
	s, w, h, out := heldPrompt(t, "", nil)
	base := "https://same.example/" + strings.Repeat("s", webURLMax)
	for i := range 400 {
		s.webReport(search(fmt.Sprintf("%s/%d", base, i)))
	}
	s.webReport(search("https://other.example/"))
	close(h.release)
	await(t, out, "the turn")
	evs := mark(t, s, w)
	at := webAt(evs)
	if len(at) == 0 {
		t.Fatal("no web delta")
	}
	want := []string{base[:webURLMax], "https://other.example/"}
	if got := webOf(evs[at[len(at)-1]]); !slices.Equal(got.URLs, want) || !got.URLsTruncated || got.Searches != 401 {
		t.Fatalf("the restatement is %d searches, URLs %q, truncated %v; want %d searches and %q, truncated", got.Searches, got.URLs, got.URLsTruncated, 401, want)
	}
}

// ---------------------------------------------------------------- windows

// TestWebPromptWindow: a prompt's web deltas — one per call, the whole tally
// each time, then the restatement as its last — all lie after the turn's
// started and before its ended, the engine's two enqueued where the engine
// enqueues them; a search's URLs and a fetch's requested URL are counted. A
// turn with no call has no web delta at all.
func TestWebPromptWindow(t *testing.T) {
	f, e, opts := webOn(t)
	e.answerWith("two", "Title: A\nURL: https://a.example/\nText: x\n---\nTitle: B\nURL: https://b.example/\nText: y")
	f.models["test/a"].push(searchStep(t, "w1", "two"), fetchStep(t, "w2", "https://c.example/page"), searchStep(t, "w3", "two"), answer("done"))
	f.models["test/a"].push(answer("quiet"))
	s := f.started(opts)
	w := newNativeWatcher(t, s)

	s.log.Enqueue(turnEv("turn-1", TurnStarted))
	if _, err := s.Prompt(context.Background(), "look"); err != nil {
		t.Fatal(err)
	}
	s.log.Enqueue(turnEv("turn-1", TurnEnded))
	s.log.Enqueue(turnEv("turn-2", TurnStarted))
	if _, err := s.Prompt(context.Background(), "nothing"); err != nil {
		t.Fatal(err)
	}
	s.log.Enqueue(turnEv("turn-2", TurnEnded))
	evs := mark(t, s, w)

	st, en := slices.IndexFunc(evs, isTurn("turn-1", TurnStarted)), slices.IndexFunc(evs, isTurn("turn-1", TurnEnded))
	at := webAt(evs)
	if len(at) != 4 {
		t.Fatalf("%d web deltas, want 3 calls and the restatement", len(at))
	}
	for _, i := range at {
		if i < st || i > en {
			t.Fatalf("a web delta at %d is outside turn-1 (%d..%d)", i, st, en)
		}
	}
	for n, want := range []WebSearchState{
		{Searches: 1, URLs: []string{"https://a.example/", "https://b.example/"}},
		{Searches: 1, Fetches: 1, URLs: []string{"https://a.example/", "https://b.example/", "https://c.example/page"}},
		{Searches: 2, Fetches: 1, URLs: []string{"https://a.example/", "https://b.example/", "https://c.example/page"}},
		{Searches: 2, Fetches: 1, URLs: []string{"https://a.example/", "https://b.example/", "https://c.example/page"}},
	} {
		if got := webOf(evs[at[n]]); got.Searches != want.Searches || got.Fetches != want.Fetches || !slices.Equal(got.URLs, want.URLs) {
			t.Fatalf("web delta %d is %+v; want %+v", n, got, want)
		}
	}
	// The restatement is the window's last delta: after the turn's ending
	// event of the wire (EventDone), before the engine's ended.
	if done := slices.IndexFunc(evs[st:], func(ev Event) bool { return ev.Type == EventDone }) + st; at[3] < done {
		t.Fatalf("the restatement (%d) came before the turn's EventDone (%d)", at[3], done)
	}
	st2, en2 := slices.IndexFunc(evs, isTurn("turn-2", TurnStarted)), slices.IndexFunc(evs, isTurn("turn-2", TurnEnded))
	for _, i := range at {
		if i > st2 && i < en2 {
			t.Fatal("a turn with no call published a web delta")
		}
	}
}

// TestWebQueuedSuccessor: an ended(N) and started(N+1) enqueued as one batch,
// as the engine's settlement does for a queued successor: neither turn's
// deltas carry the other's calls.
func TestWebQueuedSuccessor(t *testing.T) {
	f, _, opts := webOn(t)
	f.models["test/a"].push(searchStep(t, "w1", "one"), answer("one"))
	f.models["test/a"].push(fetchStep(t, "w2", "https://two.example/"), fetchStep(t, "w3", "https://three.example/"), answer("two"))
	s := f.started(opts)
	w := newNativeWatcher(t, s)
	s.log.Enqueue(turnEv("turn-1", TurnStarted))
	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	s.log.Enqueue(turnEv("turn-1", TurnEnded), turnEv("turn-2", TurnStarted))
	if _, err := s.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	s.log.Enqueue(turnEv("turn-2", TurnEnded))
	evs := mark(t, s, w)
	en1, st2 := slices.IndexFunc(evs, isTurn("turn-1", TurnEnded)), slices.IndexFunc(evs, isTurn("turn-2", TurnStarted))
	if st2 != en1+1 {
		t.Fatalf("the batch was split: ended at %d, started at %d", en1, st2)
	}
	for _, i := range webAt(evs) {
		got := webOf(evs[i])
		switch {
		case i < en1 && (got.Fetches != 0 || got.Searches != 1):
			t.Fatalf("turn-1's delta %+v carries turn-2's calls", got)
		case i > st2 && got.Searches != 0:
			t.Fatalf("turn-2's delta %+v carries turn-1's calls", got)
		}
	}
	if last := webOf(evs[webAt(evs)[len(webAt(evs))-1]]); last.Fetches != 2 || last.Searches != 0 {
		t.Fatalf("turn-2's restatement is %+v", last)
	}
}

// TestWebIdleCallsGoToTheNextTurn: a call reported while no window is open
// publishes nothing; the next turn a person starts takes it as its starting
// tally, published as the window opens, and the turn after carries none of it.
func TestWebIdleCallsGoToTheNextTurn(t *testing.T) {
	f := newNativeFixture(t)
	f.models["test/a"].push(answer("one"), answer("two"), answer("three"))
	s := f.started(Options{})
	w := newNativeWatcher(t, s)
	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	s.webReport(search("https://idle.example/"))
	if evs := mark(t, s, w); len(webAt(evs)) != 0 {
		t.Fatal("an idle call published a delta")
	}
	if ws := webSnap(s); ws.idle.Searches != 1 {
		t.Fatalf("webIdle = %+v", ws.idle)
	}
	s.log.Enqueue(turnEv("turn-2", TurnStarted))
	if _, err := s.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	s.log.Enqueue(turnEv("turn-2", TurnEnded))
	if _, err := s.Prompt(context.Background(), "third"); err != nil {
		t.Fatal(err)
	}
	evs := mark(t, s, w)
	at := webAt(evs)
	st, en := slices.IndexFunc(evs, isTurn("turn-2", TurnStarted)), slices.IndexFunc(evs, isTurn("turn-2", TurnEnded))
	if len(at) != 2 || at[0] < st || at[1] > en {
		t.Fatalf("web deltas at %v; want the opening's and the restatement inside turn-2 (%d..%d)", at, st, en)
	}
	if got := webOf(evs[at[0]]); got.Searches != 1 || !slices.Equal(got.URLs, []string{"https://idle.example/"}) {
		t.Fatalf("the opening delta is %+v", got)
	}
}

// TestWebClaimsThatOpenNoWindow: a claim withdrawn before its turn opened, a
// claim refused because another holds the slot, and a synthetic turn the
// engine never hands to the session each touch no tally: the idle call stays
// in webIdle, nothing is published, and the next window takes it.
func TestWebClaimsThatOpenNoWindow(t *testing.T) {
	f := newNativeFixture(t)
	f.models["test/a"].push(answer("ran"))
	s := f.started(Options{})
	w := newNativeWatcher(t, s)
	s.webReport(search("https://idle.example/"))

	withdrawn := s.Begin("withdrawn")
	// Cancel waits for the withdrawal, which needs the continuation to run
	// below, so its context has already ended: it marks the claim and returns
	// (TestNativeBeginWithdraws).
	if out, _ := s.Cancel(endedCtx()); !out.Withdrew {
		t.Fatalf("Cancel = %+v; want a withdrawal", out)
	}
	if _, err := withdrawn(context.Background()); err != ErrPromptCancelled {
		t.Fatalf("the withdrawn claim = %v", err)
	}
	// A synthetic turn: the engine's started and ended, no Begin.
	s.log.Enqueue(turnEv("turn-9", TurnStarted), Event{Type: EventTurn, At: time.Now(), Turn: &TurnInfo{ID: "turn-9", Phase: TurnEnded, Synthetic: true, StopReason: "cancelled"}})
	run := s.Begin("ran")
	refused := s.Begin("refused")
	if _, err := refused(context.Background()); err != ErrPromptInFlight {
		t.Fatalf("the second claim = %v; want ErrPromptInFlight", err)
	}
	if evs := mark(t, s, w); len(webAt(evs)) != 0 {
		t.Fatal("a claim that opened no window published a delta")
	}
	if ws := webSnap(s); ws.idle.Searches != 1 || ws.open || !ws.web.Empty() {
		t.Fatalf("after the claims: idle %+v, open %v, web %+v; want the idle call kept", ws.idle, ws.open, ws.web)
	}
	if _, err := run(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := mark(t, s, w)
	if at := webAt(evs); len(at) != 2 || webOf(evs[at[0]]).Searches != 1 {
		t.Fatalf("the next window's deltas at %v; want the carried call and its restatement", at)
	}
	if ws := webSnap(s); !ws.idle.Empty() {
		t.Fatalf("webIdle after the window took it: %+v", ws.idle)
	}
}

// TestWebConcurrentReports (under -race): the parent and two children
// reporting at once into one open window — every report counted once, the
// deltas' counts never going down in seq order, the restatement the totals.
func TestWebConcurrentReports(t *testing.T) {
	s, w, h, out := heldPrompt(t, "", nil)
	const each = 40
	var wg sync.WaitGroup
	for g := range 3 {
		wg.Go(func() {
			for i := range each {
				call := search(fmt.Sprintf("https://g%d.example/%d", g, i))
				call.Fetch = i%2 == 1
				call.Failed = i%5 == 0
				s.webReport(call)
			}
		})
	}
	wg.Wait()
	close(h.release)
	await(t, out, "the turn")
	evs := mark(t, s, w)
	at := webAt(evs)
	if len(at) != 3*each+1 {
		t.Fatalf("%d web deltas, want one per report and the restatement", len(at))
	}
	prev := 0
	var prevSeq uint64
	for _, i := range at {
		got := webOf(evs[i])
		n := got.Searches + got.Fetches
		if n < prev || evs[i].Seq <= prevSeq {
			t.Fatalf("the deltas went back: %d calls after %d (seq %d after %d)", n, prev, evs[i].Seq, prevSeq)
		}
		prev, prevSeq = n, evs[i].Seq
	}
	final := webOf(evs[at[len(at)-1]])
	if final.Searches != 3*each/2 || final.Fetches != 3*each/2 || final.Failed != 3*(each/5) || len(final.URLs) != webURLsMax || !final.URLsTruncated {
		t.Fatalf("the final tally is %+v", final)
	}
}

// TestWebPublishedDeltaIsItsOwn: a delta's URL list is never written after it
// is published — later reports into the same window leave it as it was.
func TestWebPublishedDeltaIsItsOwn(t *testing.T) {
	s, w, h, out := heldPrompt(t, "", nil)
	s.webReport(search("https://one.example/"))
	evs := mark(t, s, w)
	first := webOf(evs[webAt(evs)[0]])
	urls := first.URLs
	before := slices.Clone(urls)
	for i := range 20 {
		s.webReport(search(fmt.Sprintf("https://more.example/%d", i)))
	}
	close(h.release)
	await(t, out, "the turn")
	_ = mark(t, s, w)
	if first.Searches != 1 || !slices.Equal(first.URLs, before) || !slices.Equal(urls[:cap(urls)][:len(before)], before) {
		t.Fatalf("the published delta changed under later reports: %+v", first)
	}
}

// TestWebAttributionPoint: a report belongs to the window open when it takes
// s.mu, not when its call returned: held before it takes s.mu and released
// once the window has closed, it lands in webIdle; released after
// the freeze, it is dropped.
func TestWebAttributionPoint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		freeze bool
	}{{"after the release", false}, {"after the freeze", true}} {
		t.Run(tc.name, func(t *testing.T) {
			rh := newReportHold()
			s, w, h, out := heldPrompt(t, "", rh.seam)
			rh.armed.Store(true)
			reported := make(chan struct{})
			go func() { s.webReport(search("https://late.example/")); close(reported) }()
			await(t, rh.reached, "the report at the barrier")
			if tc.freeze {
				s.FreezeWebTally(true)
			}
			close(h.release)
			await(t, out, "the turn")
			close(rh.release)
			await(t, reported, "the report")
			evs := mark(t, s, w)
			if at := webAt(evs); len(at) != 0 {
				t.Fatalf("the late report reached the closed window: %d deltas", len(at))
			}
			ws := webSnap(s)
			if tc.freeze {
				if !ws.idle.Empty() || !ws.last.Empty() {
					t.Fatalf("after the freeze: idle %+v, last %+v; want the report dropped", ws.idle, ws.last)
				}
				return
			}
			if ws.idle.Searches != 1 || !slices.Equal(ws.idle.URLs, []string{"https://late.example/"}) {
				t.Fatalf("webIdle = %+v; want the late report", ws.idle)
			}
		})
	}
}

// TestWebIdleThenClose: calls reported idle, then Close — nothing is
// published, the idle calls are gone, and a report after the close is
// dropped without a panic.
func TestWebIdleThenClose(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	s.webReport(search("https://idle.example/"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.webReport(search("https://after.example/"))
	for _, ev := range drained(s) {
		if webOf(ev) != nil {
			t.Fatalf("a web delta after an idle call and a close: %+v", ev)
		}
	}
	if ws := webSnap(s); !ws.web.Empty() || !ws.idle.Empty() || !ws.last.Empty() || !ws.frozen {
		t.Fatalf("after Close: web %+v, idle %+v, last %+v, frozen %v", ws.web, ws.idle, ws.last, ws.frozen)
	}
}

// TestWebReloadStartsAtZero: a session's idle calls die with its incarnation:
// a reload of it starts with every tally empty, its replay of a transcript
// holding web calls publishes no web section, and its first turn counts its
// own calls alone.
func TestWebReloadStartsAtZero(t *testing.T) {
	f, _, opts := webOn(t)
	opts.Workspace = t.TempDir()
	id := func() string {
		f.models["test/a"].push(searchStep(t, "w1", "stored"), answer("done"))
		s := f.started(opts)
		if _, err := s.Prompt(context.Background(), "prompt 1"); err != nil {
			t.Fatal(err)
		}
		deltaSettled(t, s)
		s.webReport(search("https://idle.example/"))
		sid := s.Snapshot().SessionID
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return sid
	}()
	load := opts
	load.LoadSessionID = id
	s := f.session(load)
	evs, err := startLoad(t, s)
	if err != nil {
		t.Fatalf("the load: %v", err)
	}
	if slices.IndexFunc(evs, func(ev Event) bool {
		return ev.Type == EventTool && ev.Tool != nil && ev.Tool.Name == tool.WebSearchTool
	}) < 0 &&
		!strings.Contains(strings.Join(loadLines(evs), "\n"), "websearch") {
		t.Fatalf("fixture: the replay holds no web call:\n%s", strings.Join(loadLines(evs), "\n"))
	}
	if at := webAt(evs); len(at) != 0 {
		t.Fatalf("the replay published %d web sections", len(at))
	}
	if ws := webSnap(s); !ws.web.Empty() || !ws.idle.Empty() || !ws.last.Empty() {
		t.Fatalf("a reload's tallies: %+v %+v %+v", ws.web, ws.idle, ws.last)
	}
	w := newNativeWatcher(t, s)
	f.models["test/a"].push(fetchStep(t, "w2", "https://new.example/"), answer("again"))
	if _, err := s.Prompt(context.Background(), "prompt 2"); err != nil {
		t.Fatal(err)
	}
	evs = mark(t, s, w)
	at := webAt(evs)
	if len(at) == 0 {
		t.Fatal("the reload's turn published no web delta")
	}
	if got := webOf(evs[at[len(at)-1]]); got.Searches != 0 || got.Fetches != 1 || !slices.Equal(got.URLs, []string{"https://new.example/"}) {
		t.Fatalf("the reload's first turn counted %+v; want its own fetch alone", got)
	}
}

// ---------------------------------------------------------------- wakes

// webWakeRig is a wake rig whose session opens with web search on, at e.
func webWakeRig(t *testing.T) (*wakeRig, *webEndpoint) {
	t.Helper()
	e := newWebEndpoint(t)
	return newWakeRigWith(t, Options{WebFlag: WebFlagOn}, wakeSeams{env: map[string]string{websearch.TestEndpointEnv: e.url()}}), e
}

// bracketAt is the index of wake id's opening (running) or ending bracket.
func bracketAt(evs []Event, id string, running bool) int {
	return slices.IndexFunc(evs, func(ev Event) bool { return isBracket(running)(ev) && ev.ForeignTurn.ID == id })
}

// TestWebWakeWindow: a wake's own call and a background call returning while
// it runs are both the wake's: every web delta lies after its opening bracket
// and before its ending, the restatement last; a wake keeps no webLast, and
// its opening cleared the one the turn before it left.
func TestWebWakeWindow(t *testing.T) {
	rig, _ := webWakeRig(t)
	s := rig.s
	child, wake := newHeld(t), newHeld(t)
	// The user's turn makes a call of its own, so it leaves a webLast.
	rig.a.route("go", searchStep(t, "w0", "turn"))
	id := rig.spawnOne(child, searchStep(t, "w1", "wake"), wake.step(textParts("noted "), answerParts("done")))
	if ws := webSnap(s); ws.last.Searches != 1 {
		t.Fatalf("the user's turn left webLast %+v", ws.last)
	}
	rig.finish(child, id)
	rig.awaitDecided(true, "the result pending")
	await(t, wake.reached, "the wake's last step")
	if ws := webSnap(s); !ws.last.Empty() || !ws.open {
		t.Fatalf("inside the wake: webLast %+v, window open %v; want the opening to have cleared it", ws.last, ws.open)
	}
	// A background child's call returning now is the wake's.
	s.webReport(search("https://background.example/"))
	close(wake.release)
	rig.bracket(false, 1, "wake-1")
	evs := mark(t, s, rig.w)
	op, en := bracketAt(evs, "wake-1", true), bracketAt(evs, "wake-1", false)
	var inside []int
	for _, i := range webAt(evs) {
		if i > op {
			inside = append(inside, i)
		}
	}
	if len(inside) != 3 || inside[2] > en {
		t.Fatalf("the wake's web deltas at %v (brackets %d..%d); want its call's, the background call's and the restatement inside", inside, op, en)
	}
	if got := webOf(evs[inside[2]]); got.Searches != 2 || !slices.Equal(got.URLs, []string{"https://r.example/wake", "https://background.example/"}) {
		t.Fatalf("the wake's restatement is %+v", got)
	}
	if ws := webSnap(s); !ws.last.Empty() {
		t.Fatalf("a wake left webLast %+v", ws.last)
	}
}

// TestWebIdleCallsGoToTheNextWake: a call reported while nothing ran is the
// next window's — here a wake's, published right after its opening bracket —
// and no later turn's.
func TestWebIdleCallsGoToTheNextWake(t *testing.T) {
	rig, _ := webWakeRig(t)
	s := rig.s
	child := newHeld(t)
	id := rig.spawnOne(child, answer("noted"), answer("later"))
	s.webReport(search("https://idle.example/"))
	rig.finish(child, id)
	rig.awaitDecided(true, "the result pending")
	rig.bracket(false, 1, "wake-1")
	rig.awaitDecided(false, "the wake's ending")
	if _, err := s.Prompt(context.Background(), "later"); err != nil {
		t.Fatal(err)
	}
	rig.w.waitTerminals(2)
	evs := mark(t, s, rig.w)
	op, en := bracketAt(evs, "wake-1", true), bracketAt(evs, "wake-1", false)
	at := webAt(evs)
	if len(at) != 2 || at[0] <= op || at[1] >= en {
		t.Fatalf("web deltas at %v (wake %d..%d); want the carried call's and the restatement, inside the wake alone", at, op, en)
	}
	if got := webOf(evs[at[0]]); got.Searches != 1 || !slices.Equal(got.URLs, []string{"https://idle.example/"}) {
		t.Fatalf("the wake's opening delta is %+v", got)
	}
}

// TestWebChildSpanningWindows (F.4.6): a background child whose search
// completes during the user's turn B and whose result a wake C delivers is
// counted in B, where it completed, and C reports none.
func TestWebChildSpanningWindows(t *testing.T) {
	rig, _ := webWakeRig(t)
	s := rig.s
	gate, childEnd, b := newHeld(t), newHeld(t), newHeld(t)
	rig.a.route("go", callsStep(bgCall(t, "a1", "job", "child work")), answer("started"),
		b.step(textParts("b "), answerParts("done")), answer("noted"))
	rig.a.route("child work",
		gate.step(nil, cat(nativeCallParts("c1", tool.WebSearchTool, nativeArgs(t, map[string]any{"query": "child"})), finishParts(fantasy.FinishReasonToolCalls))),
		childEnd.step(openTextParts("did "), closeTextParts("work")))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	rig.w.waitType(EventDone)
	rig.awaitDecided(false, "turn A's release, the child running")
	await(t, gate.reached, "the child's first step")

	out := startPrompt(s, "b")
	await(t, b.reached, "turn B's step")
	close(gate.release)
	rig.w.wait("the child's search in B", func(ev Event) bool { w := webOf(ev); return w != nil && w.Searches == 1 })
	await(t, childEnd.reached, "the child's last step")
	close(b.release)
	await(t, out, "turn B")
	rig.awaitDecided(false, "turn B's release, the child still running")
	close(childEnd.release)
	rig.awaitPending("the child")
	rig.awaitDecided(true, "the result pending")
	rig.bracket(false, 1, "wake-1")
	evs := mark(t, s, rig.w)
	op, en := bracketAt(evs, "wake-1", true), bracketAt(evs, "wake-1", false)
	at := webAt(evs)
	if len(at) != 2 {
		t.Fatalf("%d web deltas; want the child's call in B and B's restatement", len(at))
	}
	for _, i := range at {
		if i > op && i < en {
			t.Fatal("wake C, which delivered the result, reported a web call")
		}
		if got := webOf(evs[i]); got.Searches != 1 || !slices.Equal(got.URLs, []string{"https://r.example/child"}) {
			t.Fatalf("B's delta is %+v", got)
		}
	}
	if len(rig.resultRequests()) == 0 {
		t.Fatal("the wake delivered nothing")
	}
}

// TestWebSuspendedWake: a call that returned while idle is carried into the
// wake that opens next; that wake fails and writes nothing, so its result is
// suspended and the next turn a person starts delivers it — and the count
// stays in the wake, where the call was carried: the delivering turn has none.
func TestWebSuspendedWake(t *testing.T) {
	rig, _ := webWakeRig(t)
	s := rig.s
	child := newHeld(t)
	id := rig.spawnOne(child, reply(errorParts(fmt.Errorf("the provider is down"))), answer("delivered later"))
	s.webReport(search("https://child.example/"))
	rig.finish(child, id)
	rig.awaitDecided(true, "the result pending")
	rig.bracket(false, 1, "wake-1")
	rig.awaitDecided(false, "the failed wake's ending")
	if s.hs.HasPending() {
		t.Fatal("fixture: the failed wake's result is pending, not suspended")
	}
	if _, err := s.Prompt(context.Background(), "later"); err != nil {
		t.Fatal(err)
	}
	rig.w.waitTerminals(2)
	evs := mark(t, s, rig.w)
	op, en := bracketAt(evs, "wake-1", true), bracketAt(evs, "wake-1", false)
	at := webAt(evs)
	if len(at) != 2 || at[0] <= op || at[1] >= en {
		t.Fatalf("web deltas at %v (wake %d..%d); want both inside the suspended wake", at, op, en)
	}
	if reqs := rig.resultRequests(); len(reqs) < 2 {
		t.Fatalf("%d requests carried the result; want the wake's and the delivering turn's", len(reqs))
	}
}

// TestWebBackgroundCallCancelledByClose: a background child's search in flight
// when the session closes is cancelled by the close; its report — a failed
// call — is dropped, nothing is published, and nothing panics.
func TestWebBackgroundCallCancelledByClose(t *testing.T) {
	rig, e := webWakeRig(t)
	s := rig.s
	var reports atomic.Int32
	s.webReportSeam = func() { reports.Add(1) }
	rig.a.route("go", callsStep(bgCall(t, "a1", "job", "child work")), answer("started"))
	rig.a.route("child work", searchStep(t, "c1", "block"), answer("never"))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	await(t, e.started, "the child's search in flight")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rig.w.pause()
	for _, ev := range rig.w.events() {
		if webOf(ev) != nil {
			t.Fatalf("a web delta was published: %+v", ev.State.WebSearch)
		}
	}
	if reports.Load() != 1 {
		t.Fatalf("%d reports reached the session; want the cancelled call's one", reports.Load())
	}
	if ws := webSnap(s); !ws.web.Empty() || !ws.idle.Empty() {
		t.Fatalf("after the close: web %+v, idle %+v", ws.web, ws.idle)
	}
}

// ---------------------------------------------------------------- the freeze

// TestWebFreezeCutsAClosingTurn: the engine's close, emulated as engine.Close
// does it — the freeze, then the synthetic closing ending — with a turn's
// window open: the freeze publishes the window's tally ahead of the ending
// (lower seq), every report after it is dropped, and the turn's own release
// restates nothing; a turn with no calls gets no delta at all.
func TestWebFreezeCutsAClosingTurn(t *testing.T) {
	for _, calls := range []int{2, 0} {
		t.Run(strconv.Itoa(calls)+" calls", func(t *testing.T) {
			s, w, h, out := heldPrompt(t, "turn-1", nil)
			for i := range calls {
				s.webReport(search(fmt.Sprintf("https://c.example/%d", i)))
			}
			before := len(webAt(mark(t, s, w)))
			s.FreezeWebTally(true)
			s.log.Enqueue(closingEv("turn-1"))
			s.webReport(search("https://late.example/"))
			end := mark(t, s, w)
			ending := slices.IndexFunc(end, isTurn("turn-1", TurnEnded))
			at := webAt(end)
			if calls == 0 {
				if len(at) != 0 {
					t.Fatalf("a turn with no calls published %d web deltas", len(at))
				}
			} else {
				if len(at) != before+1 {
					t.Fatalf("%d web deltas after the freeze, want %d (the freeze's)", len(at), before+1)
				}
				freeze := end[at[len(at)-1]]
				if got := webOf(freeze); got.Searches != calls || freeze.Seq >= end[ending].Seq {
					t.Fatalf("the freeze's delta %+v at seq %d; the closing ending at seq %d", got, freeze.Seq, end[ending].Seq)
				}
			}
			close(h.release)
			await(t, out, "the turn")
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			w.pause()
			all := w.events()
			ending = slices.IndexFunc(all, isTurn("turn-1", TurnEnded))
			for _, i := range webAt(all) {
				if i > ending {
					t.Fatalf("a web delta (%+v) followed the closing ending", webOf(all[i]))
				}
			}
		})
	}
}

// TestWebFreezeInTheReleaseGap: a prompt whose window has released and whose
// engine turn has not settled — the continuation has returned to nobody yet —
// is closed: the freeze restates the window's final tally (webLast), the same
// value as its restatement, ahead of the closing ending. With the turn settled
// before the close (no turn current), webLast is not restated.
func TestWebFreezeInTheReleaseGap(t *testing.T) {
	for _, settled := range []bool{false, true} {
		t.Run(fmt.Sprintf("settled=%v", settled), func(t *testing.T) {
			f, _, opts := webOn(t)
			f.models["test/a"].push(searchStep(t, "w1", "gap"), answer("done"))
			s := f.started(opts)
			w := newNativeWatcher(t, s)
			s.log.Enqueue(turnEv("turn-1", TurnStarted))
			if _, err := s.Prompt(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			if ws := webSnap(s); ws.last.Searches != 1 || ws.open {
				t.Fatalf("after the release: webLast %+v, open %v", ws.last, ws.open)
			}
			if settled {
				s.log.Enqueue(turnEv("turn-1", TurnEnded))
			}
			before := webAt(mark(t, s, w))
			s.FreezeWebTally(!settled)
			if !settled {
				s.log.Enqueue(closingEv("turn-1"))
			}
			evs := mark(t, s, w)
			at := webAt(evs)
			if settled {
				if len(at) != len(before) {
					t.Fatal("a settled turn's webLast was restated by the freeze")
				}
				return
			}
			if len(at) != len(before)+1 {
				t.Fatalf("%d web deltas after the freeze; want the restated webLast", len(at)-len(before))
			}
			restate, dup := webOf(evs[at[len(at)-2]]), webOf(evs[at[len(at)-1]])
			ending := slices.IndexFunc(evs, isTurn("turn-1", TurnEnded))
			if dup.Searches != restate.Searches || !slices.Equal(dup.URLs, restate.URLs) || at[len(at)-1] > ending || evs[at[len(at)-1]].Seq >= evs[ending].Seq {
				t.Fatalf("the freeze restated %+v (the release's was %+v) at %d, the ending at %d", dup, restate, at[len(at)-1], ending)
			}
		})
	}
}

// TestWebSettleRestatesInTheReleaseGap: a prompt whose window has released is
// settled by the engine (RestateWebTally, then its ended): the released
// window's final tally (webLast) is restated, the same value, ahead of the
// ended, and spent — a second restatement, and a close's freeze after the
// settlement, publish nothing. A frozen tally, and an empty one, restate
// nothing.
func TestWebSettleRestatesInTheReleaseGap(t *testing.T) {
	f, _, opts := webOn(t)
	f.models["test/a"].push(searchStep(t, "w1", "gap"), answer("done"))
	s := f.started(opts)
	w := newNativeWatcher(t, s)
	s.log.Enqueue(turnEv("turn-1", TurnStarted))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	before := webAt(mark(t, s, w))
	s.RestateWebTally()
	s.log.Enqueue(turnEv("turn-1", TurnEnded))
	evs := mark(t, s, w)
	at := webAt(evs)
	if len(at) != len(before)+1 {
		t.Fatalf("%d web deltas after the settlement; want the restated webLast", len(at)-len(before))
	}
	restate, dup := webOf(evs[at[len(at)-2]]), webOf(evs[at[len(at)-1]])
	ending := slices.IndexFunc(evs, isTurn("turn-1", TurnEnded))
	if dup.Searches != 1 || !reflect.DeepEqual(dup, restate) || evs[at[len(at)-1]].Seq >= evs[ending].Seq {
		t.Fatalf("the settlement restated %+v (the release's was %+v) at seq %d, the ended at %d", dup, restate, evs[at[len(at)-1]].Seq, evs[ending].Seq)
	}
	if ws := webSnap(s); !ws.last.Empty() || ws.frozen {
		t.Fatalf("after the settlement: webLast %+v, frozen %v", ws.last, ws.frozen)
	}
	s.RestateWebTally()
	s.FreezeWebTally(false)
	if evs := mark(t, s, w); len(webAt(evs)) != len(at) {
		t.Fatal("a spent webLast was restated again")
	}
	// Frozen: nothing, whatever webLast holds.
	s.mu.Lock()
	s.webLast = webTally{searches: 2}
	s.mu.Unlock()
	s.RestateWebTally()
	if evs := mark(t, s, w); len(webAt(evs)) != len(at) {
		t.Fatal("a frozen tally was restated")
	}
}

// TestWebLastIsClearedByEveryBegin: the released prompt's webLast is cleared
// by the next Begin, whatever it answers (a refused one included), so a
// freeze never restates it for another turn.
func TestWebLastIsClearedByEveryBegin(t *testing.T) {
	s, _, h, out := heldPrompt(t, "", nil)
	s.webReport(search("https://a.example/"))
	close(h.release)
	await(t, out, "the turn")
	if ws := webSnap(s); ws.last.Searches != 1 {
		t.Fatalf("webLast after the release = %+v", ws.last)
	}
	// A Begin before Start's harness... any Begin: claimed then withdrawn.
	run := s.Begin("next")
	if ws := webSnap(s); !ws.last.Empty() {
		t.Fatalf("webLast after a Begin = %+v", ws.last)
	}
	_, _ = s.Cancel(endedCtx())
	_, _ = run(context.Background())
	// And a refused Begin clears it too.
	s.mu.Lock()
	s.webLast = webTally{searches: 3}
	s.wake = true
	s.mu.Unlock()
	refused := s.Begin("refused")
	s.mu.Lock()
	s.wake = false
	last := s.webLast
	s.mu.Unlock()
	if _, err := refused(context.Background()); err != ErrForeignTurn {
		t.Fatalf("the refused Begin = %v", err)
	}
	if !last.empty() {
		t.Fatalf("webLast after a refused Begin = %+v", last)
	}
}

// TestWebFreezeDuringAWake: a wake running at the close is frozen too — its
// window's tally published by the freeze, with no engine turn current — and
// every report after it is dropped; its own ending restates nothing.
func TestWebFreezeDuringAWake(t *testing.T) {
	rig := newWakeRig(t, Options{}, nil)
	s := rig.s
	child, wake := newHeld(t), newHeld(t)
	id := rig.spawnOne(child, wake.step(textParts("noted "), answerParts("done")))
	rig.finish(child, id)
	rig.awaitDecided(true, "the result pending")
	await(t, wake.reached, "the wake's step")
	s.webReport(search("https://w.example/"))
	s.FreezeWebTally(false)
	s.webReport(search("https://dropped.example/"))
	close(wake.release)
	rig.bracket(false, 1, "wake-1")
	evs := mark(t, s, rig.w)
	at := webAt(evs)
	if len(at) != 2 {
		t.Fatalf("%d web deltas; want the call's and the freeze's, no restatement", len(at))
	}
	for _, i := range at {
		if got := webOf(evs[i]); got.Searches != 1 {
			t.Fatalf("a delta counted %+v; the report after the freeze was not dropped", got)
		}
	}
}

// TestWebReportRacingThePreCloseInterval: a report held at the barrier, let go
// after the freeze and the closing ending — before the session's own Close —
// is dropped: no web delta follows the ending.
func TestWebReportRacingThePreCloseInterval(t *testing.T) {
	rh := newReportHold()
	s, w, h, out := heldPrompt(t, "turn-1", rh.seam)
	s.webReport(search("https://counted.example/"))
	rh.armed.Store(true)
	done := make(chan struct{})
	go func() { s.webReport(search("https://racing.example/")); close(done) }()
	await(t, rh.reached, "the racing report")
	s.FreezeWebTally(true)
	s.log.Enqueue(closingEv("turn-1"))
	close(rh.release)
	await(t, done, "the racing report")
	close(h.release)
	await(t, out, "the turn")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	w.pause()
	evs := w.events()
	ending := slices.IndexFunc(evs, isTurn("turn-1", TurnEnded))
	at := webAt(evs)
	if ending < 0 || len(at) == 0 {
		t.Fatalf("fixture: ending %d, %d web deltas", ending, len(at))
	}
	for _, i := range at {
		if i > ending {
			t.Fatalf("a web delta followed the closing ending: %+v", webOf(evs[i]))
		}
		if got := webOf(evs[i]); got.Searches != 1 || slices.Contains(got.URLs, "https://racing.example/") {
			t.Fatalf("the racing report was counted: %+v", got)
		}
	}
}

// TestWebFreezeDiscardsIdleCalls: calls still idle at the freeze are counted
// nowhere, and nothing is published for them.
func TestWebFreezeDiscardsIdleCalls(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	w := newNativeWatcher(t, s)
	s.webReport(search("https://idle.example/"))
	s.FreezeWebTally(false)
	if evs := mark(t, s, w); len(webAt(evs)) != 0 {
		t.Fatal("the freeze published idle calls")
	}
	if ws := webSnap(s); !ws.idle.Empty() || !ws.frozen {
		t.Fatalf("after the freeze: idle %+v, frozen %v", ws.idle, ws.frozen)
	}
	// Idempotent: a second freeze publishes nothing either.
	s.FreezeWebTally(true)
	if evs := mark(t, s, w); len(webAt(evs)) != 0 {
		t.Fatal("a second freeze published")
	}
}

// TestWebSearchAccessor: WebSearch answers offered and known once Open has
// decided — on with --web-search, off with --no-web-search — and unknown
// before Start.
func TestWebSearchAccessor(t *testing.T) {
	f, _, opts := webOn(t)
	s := f.session(opts)
	if offered, known := s.WebSearch(); offered || known {
		t.Fatalf("before Start: offered %v, known %v", offered, known)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if offered, known := s.WebSearch(); !offered || !known {
		t.Fatalf("on: offered %v, known %v", offered, known)
	}
	off := f.started(Options{WebFlag: WebFlagOff})
	if offered, known := off.WebSearch(); offered || !known {
		t.Fatalf("off: offered %v, known %v", offered, known)
	}
}
