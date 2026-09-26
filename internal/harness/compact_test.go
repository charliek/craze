package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// --- shared helpers ---------------------------------------------------

// recordSleep is a fixture's s.sleep seam: it records every delay instead of
// waiting, so a retry test takes no real time.
func recordSleep(rec *[]time.Duration) func(time.Duration) {
	return func(d time.Duration) { *rec = append(*rec, d) }
}

// longSummary pads s with filler text until it is at least minSummaryLen
// bytes: a summary compact accepts without retrying.
func longSummary(s string) string {
	for len(s) < minSummaryLen+50 {
		s += " Padding text so this summary is not degenerate for the test."
	}
	return s
}

// shortSummary is a summary compact must reject as degenerate: well under
// minSummaryLen.
const shortSummary = "<summary>too short</summary>"

// compactionEntries is the compaction entries on s's transcript, in file
// order.
func compactionEntries(t *testing.T, s *Session) []store.Entry {
	t.Helper()
	var out []store.Entry
	for _, e := range transcript(t, s).Entries {
		if e.Type == store.TypeCompaction {
			out = append(out, e)
		}
	}
	return out
}

// --- A1 (C9 part): TestAManualCompactionLeavesNoNumberGap --------------

func TestAManualCompactionLeavesNoNumberGap(t *testing.T) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	s := f.open(opts)

	f.models["test/a"].push(answerWith("hi"))
	run(t, s, "turn one")

	f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nturn one.")))
	if _, err := s.Compact(context.Background(), "", "/compact", nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	f.models["test/a"].push(answerWith("hi again"))
	run(t, s, "turn two")

	var turns []int
	for _, e := range transcript(t, s).Entries {
		switch e.Type {
		case store.TypeMessage:
			if e.Turn > 0 {
				turns = append(turns, e.Turn)
			}
		case store.TypeCompaction:
			turns = append(turns, e.Turn)
		}
	}
	want := []int{1, 2, 3}
	if !slices.Equal(turns, want) {
		t.Fatalf("recorded turns = %v, want %v (no number gap)", turns, want)
	}
}

// --- A15: TestSummarizerRequestIsTheLastRequestPlusThePrompt -----------

// redirect sends every request to target, whatever host the client aimed
// at: how a test reaches a local server from the openrouter driver, whose
// endpoint is fixed (mirrors llm's own factory_test.go, unexported there).
type redirectRT struct{ target *url.URL }

func (rt redirectRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme, r.URL.Host, r.Host = rt.target.Scheme, rt.target.Host, rt.target.Host
	return http.DefaultTransport.RoundTrip(r)
}

func redirectClient(t *testing.T, to string) *http.Client {
	t.Helper()
	u, err := url.Parse(to)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: redirectRT{target: u}}
}

const driverProvidersTOML = `version = 1

[providers.test]
driver = "openai-compat"
base_url = "%[1]s"
env_keys = ["TEST_API_KEY"]

[providers.or]
driver = "openrouter"
env_keys = ["OR_API_KEY"]
`

const driverModelsTOML = `version = 1
default_model = "test/a"

[models."test/a"]
provider = "test"
wire_model = "wire-a"
max_output_tokens = 4096
efforts = ["low", "high"]
default_effort = "high"

[models."or/a"]
provider = "or"
wire_model = "or-wire"
max_output_tokens = 4096
efforts = ["low", "high"]
default_effort = "high"
`

// driverOptions is Open's options for A15: both of craze's drivers built by
// the real llm factory, aimed at w's server (openai-compat directly, by its
// base_url; openrouter through a client that redirects its fixed endpoint).
func driverOptions(t *testing.T, w *wire, driver, mode string) Options {
	t.Helper()
	home := filepath.Join(t.TempDir(), "native")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, modeltable.ProvidersFile),
		[]byte(fmt.Sprintf(driverProvidersTOML, w.srv.URL+"/v1")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, modeltable.ModelsFile), []byte(driverModelsTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	table, err := modeltable.Load(home)
	if err != nil {
		t.Fatalf("loading the driver model table: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := "test/a"
	if driver == modeltable.DriverOpenRouter {
		alias = "or/a"
	}
	return Options{
		Home: home, Workspace: ws, Table: table, Model: alias, Mode: mode,
		Getenv: func(string) string { return canary },
		Now:    testNow, Version: "v0.0.0-test",
		NewModel: func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
			if r.Driver == modeltable.DriverOpenRouter {
				return llm.New(r, llm.WithHTTPClient(redirectClient(t, w.srv.URL)))
			}
			return llm.New(r)
		},
	}
}

func TestSummarizerRequestIsTheLastRequestPlusThePrompt(t *testing.T) {
	for _, driver := range []string{modeltable.DriverOpenAICompat, modeltable.DriverOpenRouter} {
		for _, mode := range []string{modeAgent, modePlan} {
			t.Run(driver+"_"+mode, func(t *testing.T) {
				testSummarizerRequestMatches(t, driver, mode)
			})
		}
	}
}

func testSummarizerRequestMatches(t *testing.T, driver, mode string) {
	w := newWire(t,
		sseReply(textChunk("hi"), finishChunk("stop", true)),
		sseReply(textChunk("there"), finishChunk("stop", true)),
		sseReply(textChunk(longSummary("1. Request and intent\nSaid hello.")), finishChunk("stop", true)),
	)
	opts := driverOptions(t, w, driver, mode)
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	run(t, s, "hello")
	run(t, s, "world")

	reqs := w.requests()
	if len(reqs) != 2 {
		t.Fatalf("saw %d requests before compacting, want 2", len(reqs))
	}
	turn2Req := reqs[1]

	res, err := s.compact(context.Background(), s.cur, 3, store.CompactionManual, "", "", nil)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.EntryID == "" {
		t.Fatal("compact reported no entry id")
	}

	reqs = w.requests()
	if len(reqs) != 3 {
		t.Fatalf("saw %d requests after compacting, want 3", len(reqs))
	}
	summReq := reqs[2]

	wantFields, gotFields := fields(t, turn2Req), fields(t, summReq)
	for _, k := range []string{"model", "tools", "tool_choice", "max_tokens", "reasoning_effort", "reasoning", "stream"} {
		w, wok := wantFields[k]
		g, gok := gotFields[k]
		if wok != gok || string(w) != string(g) {
			t.Errorf("field %q: turn 2 sent %s, the summarizer sent %s", k, w, g)
		}
	}

	wantMsgs, gotMsgs := messages(t, turn2Req), messages(t, summReq)
	if len(gotMsgs) != len(wantMsgs)+2 {
		t.Fatalf("the summarizer sent %d messages, want %d (turn 2's, plus its answer, plus the prompt)",
			len(gotMsgs), len(wantMsgs)+2)
	}
	for i, m := range wantMsgs {
		if string(gotMsgs[i]) != string(m) {
			t.Errorf("message %d differs from turn 2's own request:\nwant %s\ngot  %s", i, m, gotMsgs[i])
		}
	}
	var reply struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(gotMsgs[len(wantMsgs)], &reply); err != nil {
		t.Fatalf("turn 2's answer message: %v", err)
	}
	if reply.Role != "assistant" || !strings.Contains(reply.Content, "there") {
		t.Errorf("the message after turn 2's own request = %+v, want turn 2's own answer", reply)
	}
	var prompt struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(gotMsgs[len(gotMsgs)-1], &prompt); err != nil {
		t.Fatalf("the compaction prompt message: %v", err)
	}
	if prompt.Role != "user" || !strings.Contains(prompt.Content, "1. Request and intent") {
		t.Errorf("the last message = %+v, want the compaction prompt", prompt)
	}
}

// --- A16: retries, all attempts, overflow/text form, failure, append failure, cancel ---

func TestAShortSummaryIsRetried(t *testing.T) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	s := f.open(opts)
	var delays []time.Duration
	s.sleep = recordSleep(&delays)

	f.models["test/a"].push(answerWith("hi"))
	run(t, s, "hello")

	f.models["test/a"].push(answerWith(shortSummary), answerWith(longSummary("1. Request and intent\nOK.")))
	res, err := s.compact(context.Background(), s.cur, 2, store.CompactionAuto, "", "", nil)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.EntryID == "" {
		t.Fatal("compact reported no entry id after the retry succeeded")
	}
	if want := []time.Duration{summarizerBackoff[0]}; !slices.Equal(delays, want) {
		t.Errorf("backoff delays = %v, want %v", delays, want)
	}
	entries := compactionEntries(t, s)
	if len(entries) != 1 || !entries[0].Compaction.Succeeded() {
		t.Fatalf("compaction entries = %+v, want one success", entries)
	}
}

func TestAllAttemptsAreOnTheEntry(t *testing.T) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	s := f.open(opts)
	var delays []time.Duration
	s.sleep = recordSleep(&delays)

	f.models["test/a"].push(answerWith("hi"))
	run(t, s, "hello")

	f.models["test/a"].push(answerWith(shortSummary), answerWith(shortSummary), answerWith(shortSummary))
	_, err := s.compact(context.Background(), s.cur, 2, store.CompactionAuto, "", "", nil)
	if err == nil {
		t.Fatal("compact succeeded with three degenerate replies")
	}
	if len(delays) != 2 {
		t.Fatalf("backoff delays = %v, want 2 (before attempts 2 and 3)", delays)
	}
	entries := compactionEntries(t, s)
	if len(entries) != 1 || entries[0].Compaction.Succeeded() {
		t.Fatalf("compaction entries = %+v, want one failure", entries)
	}
	want := store.Usage{
		Input: stepUsage.InputTokens * 3, Output: stepUsage.OutputTokens * 3,
		CacheRead: stepUsage.CacheReadTokens * 3,
	}
	if got := *entries[0].Usage; got != want {
		t.Errorf("the failure entry's usage = %+v, want the sum of all three attempts %+v", got, want)
	}
}

func TestAFailedSummarizerWritesAFailureEntry(t *testing.T) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	s := f.open(opts)
	s.sleep = func(time.Duration) {}

	f.models["test/a"].push(answerWith("hi"))
	run(t, s, "hello")

	// A model-not-found failure ends the summarizer at once (fatal), one
	// entry, no summary.
	f.models["test/a"].push(errorStep(&fantasy.ProviderError{StatusCode: 404, Message: "no such model"}))
	_, err := s.compact(context.Background(), s.cur, 2, store.CompactionAuto, "", "", nil)
	if err == nil {
		t.Fatal("compact succeeded on a 404")
	}
	entries := compactionEntries(t, s)
	if len(entries) != 1 || entries[0].Compaction.Succeeded() || entries[0].Compaction.Error == "" {
		t.Fatalf("compaction entries = %+v, want one failure with an error", entries)
	}
}

// errorStep is a scripted step that fails at once, as a provider error
// would.
func errorStep(err error) step {
	return reply(errorPart(err))
}

func TestOverflowingSummaryUsesTheTextForm(t *testing.T) {
	t.Run("known_window", func(t *testing.T) {
		testOverflowUsesTextForm(t, 4000)
	})
	t.Run("unknown_window", func(t *testing.T) {
		testOverflowUsesTextForm(t, 0)
	})
}

func testOverflowUsesTextForm(t *testing.T, contextWindow int) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	s := f.open(opts)
	s.sleep = func(time.Duration) {}

	f.models["test/a"].push(answerWith("hi"))
	run(t, s, "hello")

	m := s.cur
	m.r.ContextWindow = contextWindow
	m.r.MaxOutputTokens = 100

	f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nFrom the text form.")))
	res, err := s.compact(context.Background(), m, 2, store.CompactionOverflow, "", "", nil)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.EntryID == "" {
		t.Fatal("compact reported no entry id")
	}
	calls := f.models["test/a"].requests()
	last := calls[len(calls)-1]
	if len(last.Tools) != 0 {
		t.Errorf("the text form offered %d tools, want none", len(last.Tools))
	}
	if len(last.Prompt) != 2 {
		t.Fatalf("the text form sent %d messages, want two (the system prompt, then one user message: no history)", len(last.Prompt))
	}
	text := textOf(last.Prompt[len(last.Prompt)-1])
	if !strings.Contains(text, "[User]: hello") || !strings.Contains(text, "[Assistant]: hi") {
		t.Errorf("the text form's prompt = %q, want opencode-style lines for the turn", text)
	}
	if !strings.Contains(text, "1. Request and intent") {
		t.Errorf("the text form's prompt lacks the compaction prompt")
	}
}

func TestOverflowingSummaryKeepsThePriorSummary(t *testing.T) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	s := f.open(opts)
	s.sleep = func(time.Duration) {}

	f.models["test/a"].push(answerWith("hi"))
	run(t, s, "hello")
	f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nFirst summary.")))
	if _, err := s.compact(context.Background(), s.cur, 2, store.CompactionManual, "", "", nil); err != nil {
		t.Fatalf("first compact: %v", err)
	}

	f.models["test/a"].push(answerWith("more"))
	run(t, s, "another turn")

	f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nSecond summary.")))
	if _, err := s.compact(context.Background(), s.cur, 4, store.CompactionOverflow, "", "", nil); err != nil {
		t.Fatalf("overflow compact: %v", err)
	}
	calls := f.models["test/a"].requests()
	last := calls[len(calls)-1]
	text := textOf(last.Prompt[len(last.Prompt)-1])
	if !strings.Contains(text, "First summary.") {
		t.Errorf("the text form's prompt dropped the prior summary:\n%s", text)
	}
}

func TestAFailedCompactionAppendStopsTheTurn(t *testing.T) {
	for _, name := range []string{"zero_byte", "partial"} {
		t.Run(name, func(t *testing.T) {
			testFailedAppendStopsTheTurn(t, name == "partial")
		})
	}
}

// failWriter is store.Options.OpenFile's descriptor for
// TestAFailedCompactionAppendStopsTheTurn: every write through calls up to
// failAt succeeds in full; the one at failAt writes n bytes (0, or part of
// the line) and fails.
type failWriter struct {
	mu      sync.Mutex
	f       *os.File
	calls   int
	failAt  int
	partial bool
}

func (w *failWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls == w.failAt {
		n := 0
		if w.partial && len(p) > 1 {
			n = len(p) / 2
			_, _ = w.f.Write(p[:n])
		}
		return n, errors.New("injected write failure")
	}
	return w.f.Write(p)
}

func (w *failWriter) Close() error { return w.f.Close() }

func testFailedAppendStopsTheTurn(t *testing.T, partial bool) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	opts.storeOpenFile = func(name string, flag int, perm os.FileMode) (io.WriteCloser, error) {
		file, err := os.OpenFile(name, flag, perm)
		if err != nil {
			return nil, err
		}
		// 1st write: turn one's step; 2nd: the compaction entry.
		return &failWriter{f: file, failAt: 2, partial: partial}, nil
	}
	s := f.open(opts)

	f.models["test/a"].push(answerWith("hi"))
	run(t, s, "hello")

	f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nOK.")))
	_, err := s.compact(context.Background(), s.cur, 2, store.CompactionAuto, "", "", nil)
	if err == nil {
		t.Fatal("compact succeeded despite the injected write failure")
	}
	var saveErr *errCompactionSaveFailed
	if !errors.As(err, &saveErr) {
		t.Fatalf("compact's error = %v (%T), want *errCompactionSaveFailed", err, err)
	}
	// A write that lost part of a line leaves the store permanently failed
	// (ErrFailed: appending after a torn line would corrupt the file); one
	// that wrote nothing failed only this append, and the store is still
	// usable — either way compact treats it as a save failure and stops (P5).
	_, err = s.store.AppendCompaction(3, s.cur.id(), store.Usage{}, store.Compaction{Reason: store.CompactionManual, Summary: longSummary("x")})
	if partial && err == nil {
		t.Fatal("the store accepted a write after a torn line")
	}
	if !partial && err != nil {
		t.Fatalf("a zero-byte write failure should leave the store usable: %v", err)
	}
}

func TestACancelDuringCompactionEndsTheTurnCancelled(t *testing.T) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	s := f.open(opts)

	f.models["test/a"].push(answerWith("hi"))
	run(t, s, "hello")

	g := newGate()
	f.models["test/a"].push(g.hold(nil, textParts("unused")))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		res CompactResult
		err error
	}, 1)
	go func() {
		res, err := s.compact(ctx, s.cur, 2, store.CompactionManual, "", "", nil)
		done <- struct {
			res CompactResult
			err error
		}{res, err}
	}()
	<-g.reached
	cancel()
	out := await(t, done, "compact to return after a cancel")
	if !errors.Is(out.err, context.Canceled) {
		t.Fatalf("compact's error = %v, want context.Canceled", out.err)
	}
	// No usage was billed (the stream errored before any finish part), so no
	// failure entry.
	if entries := compactionEntries(t, s); len(entries) != 0 {
		t.Fatalf("compaction entries = %+v, want none (nothing was billed)", entries)
	}
}

// --- A17: TestSecondCompactionCarriesTheFirst --------------------------

func TestSecondCompactionCarriesTheFirst(t *testing.T) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	s := f.open(opts)

	f.models["test/a"].push(answerWith("reply one"))
	run(t, s, "prompt one")
	f.models["test/a"].push(answerWith("reply two"))
	run(t, s, "prompt two")

	f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nFirst.")))
	if _, err := s.compact(context.Background(), s.cur, 3, store.CompactionManual, "", "", nil); err != nil {
		t.Fatalf("first compact: %v", err)
	}

	f.models["test/a"].push(answerWith("reply three"))
	run(t, s, "prompt three")
	f.models["test/a"].push(answerWith("reply four"))
	run(t, s, "prompt four")

	f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nSecond.")))
	if _, err := s.compact(context.Background(), s.cur, 6, store.CompactionManual, "", "", nil); err != nil {
		t.Fatalf("second compact: %v", err)
	}

	dir := store.SegmentDir(s.store.Path())
	seg1, err := os.ReadFile(filepath.Join(dir, store.SegmentName(1)))
	if err != nil {
		t.Fatalf("reading segment 1: %v", err)
	}
	seg2, err := os.ReadFile(filepath.Join(dir, store.SegmentName(2)))
	if err != nil {
		t.Fatalf("reading segment 2: %v", err)
	}
	if !strings.Contains(string(seg1), "prompt one") || !strings.Contains(string(seg1), "prompt two") {
		t.Errorf("segment 1 lacks the first compaction's own turns:\n%s", seg1)
	}
	if strings.Contains(string(seg1), "prompt three") || strings.Contains(string(seg1), "prompt four") {
		t.Errorf("segment 1 leaked the second compaction's turns:\n%s", seg1)
	}
	if !strings.Contains(string(seg2), "prompt three") || !strings.Contains(string(seg2), "prompt four") {
		t.Errorf("segment 2 lacks the second compaction's own turns:\n%s", seg2)
	}
	if strings.Contains(string(seg2), "prompt one") || strings.Contains(string(seg2), "prompt two") {
		t.Errorf("segment 2 overlaps the first compaction's turns:\n%s", seg2)
	}
}

func TestSecondCompactionCarriesTheFirstAfterTwoNoTailCompactions(t *testing.T) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	s := f.open(opts)

	f.models["test/a"].push(answerWith("reply one"))
	run(t, s, "prompt one")

	f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nFirst.")))
	if _, err := s.compact(context.Background(), s.cur, 2, store.CompactionManual, "", "", nil); err != nil {
		t.Fatalf("first compact: %v", err)
	}

	f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nSecond, of the first summary alone.")))
	res, err := s.compact(context.Background(), s.cur, 3, store.CompactionManual, "", "", nil)
	if err != nil {
		t.Fatalf("second compact: %v", err)
	}
	if res.EntryID == "" {
		t.Fatal("the second (no-tail) compaction reported no entry id")
	}

	dir := store.SegmentDir(s.store.Path())
	if _, err := os.Stat(filepath.Join(dir, store.SegmentName(2))); err != nil {
		t.Fatalf("segment 2: %v", err)
	}
	entries := compactionEntries(t, s)
	if len(entries) != 2 {
		t.Fatalf("compaction entries = %d, want 2", len(entries))
	}
	for _, e := range entries {
		if e.Compaction.FirstKeptID != "" {
			t.Errorf("compaction %s has a tail %q, want none (X32)", e.ID, e.Compaction.FirstKeptID)
		}
	}
}

// --- the compaction prompt golden (plan 028 §3.8 item 2) ---------------

const compactionPromptGolden = "testdata/compaction_prompt.golden"

func TestCompactionPromptGolden(t *testing.T) {
	if *updateGolden {
		if err := os.WriteFile(compactionPromptGolden, []byte(compactionPrompt), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(compactionPromptGolden)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./internal/harness -run TestCompactionPromptGolden -update)", err)
	}
	if compactionPrompt != string(want) {
		t.Fatalf("the compaction prompt differs from %s\n--- want ---\n%s\n--- got ---\n%s", compactionPromptGolden, want, compactionPrompt)
	}
}

// --- A18: golden segment.golden; capped, oldest first; orphan replaced ---

const segmentGolden = "testdata/segment.golden"

// goldenEntryID masks an 8-hex-char entry id with a fixed placeholder, so
// the segment golden does not depend on the store's random ids.
var goldenEntryID = regexp.MustCompile(`\b[0-9a-f]{8}\b`)

func maskEntryIDs(s string) string { return goldenEntryID.ReplaceAllString(s, "00000000") }

func TestSegmentGolden(t *testing.T) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	opts.Now = testNow
	opts.SessionID = "00000000-0000-4000-8000-000000000001"
	s := f.open(opts)

	f.models["test/a"].push(answerWith("Adding the flag now."))
	run(t, s, "add a --json flag to export")

	f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nAdd a --json flag to the export command.")))
	if _, err := s.compact(context.Background(), s.cur, 2, store.CompactionManual, "", "", nil); err != nil {
		t.Fatalf("compact: %v", err)
	}

	dir := store.SegmentDir(s.store.Path())
	raw, err := os.ReadFile(filepath.Join(dir, store.SegmentName(1)))
	if err != nil {
		t.Fatalf("reading the segment: %v", err)
	}
	got := maskEntryIDs(string(raw))
	if *updateGolden {
		if err := os.WriteFile(segmentGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(segmentGolden)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./internal/harness -run TestSegmentGolden -update)", err)
	}
	if got != string(want) {
		t.Fatalf("the segment differs from %s\n--- want ---\n%s\n--- got ---\n%s", segmentGolden, want, got)
	}
}

func TestSegmentIsCappedOldestFirst(t *testing.T) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	s := f.open(opts)

	big := strings.Repeat("x", 200*1024)
	const turns = 4
	for i := range turns {
		f.models["test/a"].push(answerWith(fmt.Sprintf("answer-%d %s", i, big)))
		run(t, s, fmt.Sprintf("prompt-%d", i))
	}

	f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nMany big turns.")))
	if _, err := s.compact(context.Background(), s.cur, turns+1, store.CompactionManual, "", "", nil); err != nil {
		t.Fatalf("compact: %v", err)
	}

	dir := store.SegmentDir(s.store.Path())
	got, err := os.ReadFile(filepath.Join(dir, store.SegmentName(1)))
	if err != nil {
		t.Fatalf("reading the segment: %v", err)
	}
	if len(got) > maxSegmentBytes {
		t.Fatalf("the segment is %d bytes, want at most %d", len(got), maxSegmentBytes)
	}
	if strings.Contains(string(got), "prompt-0") {
		t.Errorf("the segment kept the oldest turn; it should have been dropped first")
	}
	if !strings.Contains(string(got), fmt.Sprintf("prompt-%d", turns-1)) {
		t.Errorf("the segment dropped the newest turn")
	}
	if !strings.Contains(string(got), "omitted") {
		t.Errorf("the segment names no dropped turns, want a line naming the transcript")
	}
}

func TestAnOrphanSegmentIsReplaced(t *testing.T) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	s := f.open(opts)

	f.models["test/a"].push(answerWith("hi"))
	run(t, s, "hello")

	dir := store.SegmentDir(s.store.Path())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, store.SegmentName(1))
	if err := os.WriteFile(orphan, []byte("garbage left by a crash"), 0o600); err != nil {
		t.Fatal(err)
	}

	f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nReal summary.")))
	if _, err := s.compact(context.Background(), s.cur, 2, store.CompactionManual, "", "", nil); err != nil {
		t.Fatalf("compact: %v", err)
	}
	got, err := os.ReadFile(orphan)
	if err != nil {
		t.Fatalf("reading the segment: %v", err)
	}
	if strings.Contains(string(got), "garbage left by a crash") {
		t.Fatalf("the orphan was not replaced:\n%s", got)
	}
	if !strings.Contains(string(got), "Real summary.") {
		t.Errorf("the segment does not carry the real summary:\n%s", got)
	}
}

// --- A19: TestManualCompactStoresItsCommand ----------------------------

func TestManualCompactStoresItsCommand(t *testing.T) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	s := f.open(opts)

	f.models["test/a"].push(answerWith("hi"))
	run(t, s, "hello")

	f.models["test/a"].push(answerWith(longSummary("1. Request and intent\nOK.")))
	if _, err := s.Compact(context.Background(), "keep the plan", "/compact keep the plan", nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	opts2 := f.options()
	opts2.Resume = s.ID()
	s2, err := Open(opts2)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	var evs events
	if err := s2.Replay(evs.sink); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	list := evs.list()
	var sawCommand, sawEnded bool
	for i, ev := range list {
		if p, ok := ev.(Prompted); ok && p.Text == "/compact keep the plan" {
			sawCommand = true
			if i+1 < len(list) {
				if c, ok := list[i+1].(Compacted); ok && c.Phase == CompactionEnded {
					sawEnded = true
				}
			}
		}
	}
	if !sawCommand {
		t.Fatalf("replay never emitted Prompted{%q}: %+v", "/compact keep the plan", list)
	}
	if !sawEnded {
		t.Fatalf("Prompted{command} was not immediately followed by Compacted{ended}: %+v", list)
	}
}
