package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// A step's retry after a mid-stream transport error (D-32, as amended, at
// llm.MidStreamError), through the real stack: Fantasy's agent, the
// OpenAI-compatible driver and the wrapper, against an HTTP/2 server whose
// handler aborts mid-body — a real RST_STREAM INTERNAL_ERROR, read by the real
// transport. Each driver's own reset is llm's TestAStreamResetIsRetriedThroughEveryDriver.

// h2Wire is newWire over HTTP/2 in the clear.
type h2Wire struct {
	url    string
	client *http.Client

	mu      sync.Mutex
	replies []http.HandlerFunc
	bodies  [][]byte
}

func newH2Wire(t *testing.T, replies ...http.HandlerFunc) *h2Wire {
	t.Helper()
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	w := &h2Wire{replies: replies}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("a request came over HTTP/%d", r.ProtoMajor)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading a request body: %v", err)
		}
		w.mu.Lock()
		w.bodies = append(w.bodies, body)
		var next http.HandlerFunc
		if len(w.replies) > 0 {
			next, w.replies = w.replies[0], w.replies[1:]
		}
		w.mu.Unlock()
		if next == nil {
			jsonError(rw, http.StatusTeapot, "the test queued no reply for this request")
			return
		}
		next(rw, r)
	}))
	srv.Config.Protocols = &p
	srv.Start()
	t.Cleanup(srv.Close)
	w.url = srv.URL + "/v1"
	w.client = &http.Client{Transport: &http.Transport{Protocols: &p}}
	t.Cleanup(w.client.CloseIdleConnections)
	return w
}

func (w *h2Wire) requests() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.bodies)
}

// resetAfter streams chunks and resets the stream.
func resetAfter(chunks ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
}

// fastRetries is a model that asks Fantasy for a millisecond's wait before a
// retry, as a provider's Retry-After would, so a test need not sit out the 5 s
// backoff it cannot otherwise shorten. Everything else is the wrapped model's.
type fastRetries struct{ fantasy.LanguageModel }

func (m fastRetries) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	inner, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for part := range inner {
			var pe *fantasy.ProviderError
			if part.Type == fantasy.StreamPartTypeError && errors.As(part.Error, &pe) {
				pe.ResponseHeaders = map[string]string{"retry-after-ms": "1"}
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

// retrySession is a session on w, its waits cut short (fastRetries).
func retrySession(t *testing.T, w *h2Wire, budget *int) *Session {
	t.Helper()
	f := newFixture(t, w.url)
	o := f.options()
	o.ProviderRetries = budget
	o.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
		lm, err := llm.New(r, llm.WithHTTPClient(w.client))
		return fastRetries{lm}, err
	}
	return f.open(o)
}

func budgetOf(n int) *int { return &n }

// countingCall is a bash call that appends its tag to a file, so that what ran
// is counted on disk, not in events.
func countingCall(t *testing.T, id, file, tag string) string {
	t.Helper()
	return toolCallChunk(id, "bash", input(t, map[string]any{"command": "echo " + tag + " >> " + file, "description": "count"}))
}

func ran(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// TestAResetAtEachFaultPointRetriesTheStep: step 2 of a turn is reset at each
// point of its stream. The failed attempt's call runs 0 times and its
// replacement once, step 1 is untouched, the retry's request is byte for byte
// the failed one's (a provider's prefix cache sees the same request), and the
// transcript holds no trace of the attempt.
func TestAResetAtEachFaultPointRetriesTheStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fault is the failed attempt's chunks.
		fault func(call string) []string
		// abandoned is the failed attempt's calls, by harness id.
		abandoned []string
	}{
		{name: "after partial text", fault: func(string) []string { return []string{textChunk("the abandoned answer")} }},
		{name: "after thinking", fault: func(string) []string { return []string{reasoningChunk("abandoned thought")} }},
		{name: "after a partial tool argument", fault: func(string) []string {
			return []string{textChunk("abandoned "), toolCallChunk("call_2", "bash", `{"command":"echo abandon`)}
		}, abandoned: []string{"t1.2.1"}},
		{name: "after a complete tool call", fault: func(call string) []string {
			return []string{textChunk("abandoned "), call}
		}, abandoned: []string{"t1.2.1"}},
		{name: "around the finish event", fault: func(call string) []string {
			return []string{call, finishChunk("tool_calls", true)}
		}, abandoned: []string{"t1.2.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := filepath.Join(t.TempDir(), "ran")
			w := newH2Wire(t,
				sseReply(textChunk("first step"), countingCall(t, "call_1", count, "one"), finishChunk("tool_calls", true)),
				resetAfter(tc.fault(countingCall(t, "call_2", count, "ABANDONED"))...),
				// The replacement uses the id the abandoned call had.
				sseReply(countingCall(t, "call_2", count, "two"), finishChunk("tool_calls", true)),
				sseReply(textChunk("done"), finishChunk("stop", true)),
			)
			s := retrySession(t, w, nil)
			var ev events
			res, err := s.Run(context.Background(), "go", ev.sink)
			if err != nil || res.StopReason != StopEndTurn {
				t.Fatalf("Run = %+v, %v; want end_turn", res, err)
			}
			if got := ran(t, count); !slices.Equal(got, []string{"one", "two"}) {
				t.Fatalf("what ran = %v; want the first step's and the replacement's, each once", got)
			}
			evs := ev.list()
			retrying := of[Retrying](evs)
			if len(retrying) != 1 {
				t.Fatalf("%d Retrying; want 1", len(retrying))
			}
			r := retrying[0]
			if r.Turn != 1 || r.Step != 2 || r.Attempt != 1 || r.Total != 1 || !r.Discarded || !slices.Equal(r.Abandoned, tc.abandoned) || r.Delay != time.Millisecond {
				t.Fatalf("Retrying = %+v; want turn 1, step 2, attempt 1, abandoning %v", r, tc.abandoned)
			}
			at := eventIndex(evs, func(e Event) bool { _, ok := e.(Retrying); return ok })
			for _, e := range evs[at+1:] {
				switch d := e.(type) {
				case TextDelta:
					if strings.Contains(d.Text, "abandon") {
						t.Fatalf("a delta of the failed attempt followed its Retrying: %+v", d)
					}
				case ThoughtDelta:
					t.Fatalf("a thought of the failed attempt followed its Retrying: %+v", d)
				case ToolFinished:
					if slices.Contains(tc.abandoned, d.ID) {
						t.Fatalf("an abandoned call finished after its Retrying: %+v", d)
					}
				}
			}
			// The abandoned call was settled, not run, and its replacement under
			// the same provider id is a new row.
			fin, _ := finishedByID(evs)
			for _, id := range tc.abandoned {
				if len(fin[id]) != 1 || fin[id][0].Result.Class != tool.ClassNotExecuted {
					t.Fatalf("the abandoned call %s finished %+v; want one, not executed", id, fin[id])
				}
			}
			if got := callIDs(evs); !slices.Equal(got[len(got)-1:], []string{fmt.Sprintf("t1.2.%d", len(tc.abandoned)+1)}) {
				t.Fatalf("tool ids = %v; the replacement's must not reuse the abandoned call's", got)
			}
			if got := stepsOf(evs); !slices.Equal(got, []int{1, 2, 3}) {
				t.Fatalf("StepDone steps = %v; want one per step, none for the failed attempt", got)
			}
			bodies := w.requests()
			if len(bodies) != 4 || !bytes.Equal(bodies[1], bodies[2]) {
				t.Fatalf("%d requests; the retry's must equal the failed attempt's:\n%s\n%s", len(bodies), bodies[1], bodies[2])
			}
			lines := strings.Join(entries(transcript(t, s)), "\n")
			if strings.Contains(lines, "bandon") {
				t.Fatalf("the transcript holds the failed attempt:\n%s", lines)
			}
			if !strings.Contains(lines, "first step") || !strings.Contains(lines, "call call_1") || !strings.Contains(lines, "call call_2") || !strings.Contains(lines, "end_turn: done") {
				t.Fatalf("the transcript lost a step:\n%s", lines)
			}
			// What a resumed session reads is the same transcript.
			msgs, _ := s.store.ContextWithResults(s.cur.id())
			var history strings.Builder
			for _, m := range msgs {
				history.WriteString(messageText(m))
			}
			if strings.Contains(history.String(), "bandon") {
				t.Fatalf("the context a resume reads holds the failed attempt: %s", history.String())
			}
		})
	}
}

// TestRetryThenFailure: a second reset is final, kind provider and retryable,
// and what the transcript keeps of the turn is the last attempt's alone.
func TestRetryThenFailure(t *testing.T) {
	w := newH2Wire(t, resetAfter(textChunk("first try")), resetAfter(textChunk("second try")))
	s := retrySession(t, w, nil)
	var ev events
	_, err := s.Run(context.Background(), "go", ev.sink)
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("Run = %v (%T); want a *ProviderError", err, err)
	}
	if info := pe.ErrorInfo(); info.Kind != errkind.Provider || !info.Retryable {
		t.Fatalf("ErrorInfo = %+v; want provider, retryable", info)
	}
	if !strings.Contains(pe.Message, "INTERNAL_ERROR") {
		t.Fatalf("Message = %q; want the reset named", pe.Message)
	}
	if n := len(w.requests()); n != 2 {
		t.Fatalf("%d requests; want 2", n)
	}
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: go",
		"assistant test/a high interrupted: second try",
	})
}

// TestACancelDuringTheBackoffEndsTheTurnAtOnce: the wait is Fantasy's own 5 s,
// which a cancel cuts short: the turn ends cancelled, the retry never goes out,
// and nothing of the attempt is saved. No fastRetries here.
func TestACancelDuringTheBackoffEndsTheTurnAtOnce(t *testing.T) {
	w := newH2Wire(t, resetAfter(textChunk("abandoned")))
	f := newFixture(t, w.url)
	o := f.options()
	o.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
		return llm.New(r, llm.WithHTTPClient(w.client))
	}
	s := f.open(o)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delay time.Duration
	began := time.Now()
	res, err := s.Run(ctx, "go", func(ev Event) {
		if r, ok := ev.(Retrying); ok {
			delay = r.Delay
			cancel()
		}
	})
	if err != nil || res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", res, err)
	}
	if delay != 5*time.Second {
		t.Fatalf("the wait was %v; want Fantasy's 5s", delay)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("the cancel took %v: it waited out the backoff", took)
	}
	if n := len(w.requests()); n != 1 {
		t.Fatalf("%d requests; want the failed one alone", n)
	}
	noTranscript(t, s)
}

// TestTheBudget: provider_retries bounds a step's retries, before output and
// after it together, each step on its own.
func TestTheBudget(t *testing.T) {
	busy := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("retry-after-ms", "1")
		jsonError(w, 503, "busy")
	}
	whole := sseReply(textChunk("whole"), finishChunk("stop", true))
	reset := resetAfter(textChunk("abandoned"))
	for _, tc := range []struct {
		name     string
		budget   *int
		replies  []http.HandlerFunc
		requests int
		retries  int
		fails    bool
	}{
		{"the default is one", nil, []http.HandlerFunc{reset, reset, whole}, 2, 1, true},
		{"0 means no retry", budgetOf(0), []http.HandlerFunc{reset, whole}, 1, 0, true},
		{"0, before output too", budgetOf(0), []http.HandlerFunc{busy, whole}, 1, 0, true},
		{"3 retries thrice", budgetOf(3), []http.HandlerFunc{reset, reset, reset, whole}, 4, 3, false},
		{"3 is all it has", budgetOf(3), []http.HandlerFunc{reset, reset, reset, reset, whole}, 4, 3, true},
		{"before output and after share it", budgetOf(1), []http.HandlerFunc{busy, reset, whole}, 2, 1, true},
		{"both within it", budgetOf(2), []http.HandlerFunc{busy, reset, whole}, 3, 2, false},
		{"past the range is held to it", budgetOf(9), []http.HandlerFunc{reset, reset, reset, reset, whole}, 4, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newH2Wire(t, tc.replies...)
			s := retrySession(t, w, tc.budget)
			var ev events
			res, err := s.Run(context.Background(), "go", ev.sink)
			if (err != nil) != tc.fails {
				t.Fatalf("Run = %+v, %v; fails %v", res, err, tc.fails)
			}
			if err != nil {
				var pe *ProviderError
				if !errors.As(err, &pe) || !pe.ErrorInfo().Retryable {
					t.Fatalf("err = %v (%T); want a retryable provider failure", err, err)
				}
			}
			if n := len(w.requests()); n != tc.requests {
				t.Fatalf("%d requests; want %d", n, tc.requests)
			}
			rs := of[Retrying](ev.list())
			if len(rs) != tc.retries {
				t.Fatalf("%d Retrying; want %d", len(rs), tc.retries)
			}
			for i, r := range rs {
				if r.Attempt != i+1 || r.Total != i+1 {
					t.Fatalf("retry %d = %+v", i, r)
				}
			}
		})
	}

	t.Run("each step has its own", func(t *testing.T) {
		count := filepath.Join(t.TempDir(), "ran")
		w := newH2Wire(t,
			reset,
			sseReply(countingCall(t, "c1", count, "one"), finishChunk("tool_calls", true)),
			reset,
			whole,
		)
		s := retrySession(t, w, nil)
		var ev events
		if _, err := s.Run(context.Background(), "go", ev.sink); err != nil {
			t.Fatal(err)
		}
		rs := of[Retrying](ev.list())
		if len(rs) != 2 || rs[0].Step != 1 || rs[0].Attempt != 1 || rs[0].Total != 1 || rs[1].Step != 2 || rs[1].Attempt != 1 || rs[1].Total != 2 {
			t.Fatalf("Retrying = %+v; want one per step, the second's attempt 1 of the turn's 2", rs)
		}
	})
}

// TestARequestRefusedAfterOutputStaysFinal: a non-transport failure after output
// costs the turn as it always has, with no retry, and the attempt saved as
// interrupted: the budget is for what a repeat can change.
func TestARequestRefusedAfterOutputStaysFinal(t *testing.T) {
	w := newH2Wire(t, func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(rw, "data: %s\n\n", textChunk("partial"))
		fmt.Fprint(rw, "data: {\"error\":{\"code\":\"content_filter\",\"message\":\"filtered\"}}\n\n")
	})
	s := retrySession(t, w, budgetOf(3))
	var ev events
	_, err := s.Run(context.Background(), "go", ev.sink)
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.ErrorInfo().Retryable {
		t.Fatalf("Run = %v (%T); want a provider failure a fresh turn would meet again", err, err)
	}
	if n, rs := len(w.requests()), of[Retrying](ev.list()); n != 1 || len(rs) != 0 {
		t.Fatalf("%d requests, %d Retrying; want 1 and none", n, len(rs))
	}
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: go",
		"assistant test/a high interrupted: partial",
	})
}

// transientPart is a scripted reset, its wait cut short.
func transientPart() []fantasy.StreamPart {
	return errorPart(&fantasy.ProviderError{Title: "stream transport error", Message: "INTERNAL_ERROR", TransientError: true,
		ResponseHeaders: map[string]string{"retry-after-ms": "1"}})
}

// TestARetryAdjacentToACompaction: PrepareStep is not run again for a retry, so
// the request a compaction's next segment prepared is sent again as it was: the
// same messages, one summary, the prompt once. And an overflow on the retry
// itself is answered by compacting, with nothing of the failed attempts kept.
func TestARetryAdjacentToACompaction(t *testing.T) {
	t.Run("the segment's first request", func(t *testing.T) {
		_, s, a := overflowFixture(t, nil)
		a.push(toolStep(1, over), summaryOf("looping."),
			reply(openText("abandoned"), transientPart()),
			answerWith("done"))
		var ev events
		res, err := s.Run(context.Background(), "loop", ev.sink)
		if err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v; want end_turn", res, err)
		}
		reqs := a.requests()
		if len(reqs) != 4 || summarizers(reqs) != 1 {
			t.Fatalf("%d requests, %d summarizers; want 4 and 1", len(reqs), summarizers(reqs))
		}
		equal(t, "the retry's prompt", reqs[3].Prompt, reqs[2].Prompt)
		if !startsFromSummary(reqs[3]) {
			t.Fatal("the retry does not start from the summary")
		}
		lines := entries(transcript(t, s))
		if got := compactions(t, s); len(got) != 1 {
			t.Fatalf("compactions = %v; want one: a retry compacts nothing", got)
		}
		if strings.Contains(strings.Join(lines, "\n"), "abandoned") || lines[len(lines)-1] != "assistant test/a high end_turn: done" {
			t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
		}
		if got := stepsOf(ev.list()); !slices.Equal(got, []int{1, 2}) {
			t.Fatalf("steps = %v", got)
		}
	})

	t.Run("a steer taken during the failed attempt waits for the next step", func(t *testing.T) {
		_, s, a := overflowFixture(t, nil)
		a.push(reply(openText("abandoned"), transientPart()), callStep(callParts("c1", "nope", `{}`)), answerWith("done"))
		steered := false
		res, err := s.Run(context.Background(), "loop", func(ev Event) {
			if _, ok := ev.(TextDelta); ok && !steered {
				steered = true
				if err := sendSteer(s, "and this"); err != nil {
					t.Errorf("the steer: %v", err)
				}
			}
		})
		if err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v", res, err)
		}
		reqs := a.requests()
		if len(reqs) != 3 {
			t.Fatalf("%d requests", len(reqs))
		}
		equal(t, "the retry's prompt", reqs[1].Prompt, reqs[0].Prompt)
		if last := messageText(reqs[2].Prompt[len(reqs[2].Prompt)-1]); !strings.Contains(last, "and this") {
			t.Fatalf("the next step's last message = %q; want the steer", last)
		}
	})

	t.Run("an overflow on the retry compacts", func(t *testing.T) {
		_, s, a := overflowFixture(t, nil)
		a.push(toolStep(1, under),
			reply(openText("abandoned"), transientPart()),
			reply(openText("abandoned too"), errorPart(midStreamOverflow())),
			summaryOf("one call."),
			answerWith("done"))
		res, err := s.Run(context.Background(), "loop", nil)
		if err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v; want end_turn", res, err)
		}
		lines := strings.Join(entries(transcript(t, s)), "\n")
		if strings.Contains(lines, "abandoned") || !strings.Contains(lines, "compaction overflow") {
			t.Fatalf("transcript:\n%s", lines)
		}
	})
}

// TestASideQuestionNeverRepeatsWhatItHandedOn: a side question hands its text on
// as it comes and cannot take it back, so a reset after its first text is final.
func TestASideQuestionNeverRepeatsWhatItHandedOn(t *testing.T) {
	w := newH2Wire(t, resetAfter(textChunk("half of it")), sseReply(textChunk("never"), finishChunk("stop", true)))
	s := retrySession(t, w, budgetOf(3))
	o := ask(s, nil, "q")
	if o.err == nil || !slices.Equal(o.deltas, []string{"half of it"}) {
		t.Fatalf("the side question: %+v; want a failure and the one delta", o)
	}
	if n := len(w.requests()); n != 1 {
		t.Fatalf("%d requests; want 1", n)
	}
}

// TestASummaryRetriesWithItsOwnAttempts: a summary takes no retry of Fantasy's,
// so its stream resetting after output is one of its own attempts, whatever the
// session's budget, and the second attempt's text alone is the summary.
func TestASummaryRetriesWithItsOwnAttempts(t *testing.T) {
	w := newH2Wire(t,
		sseReply(textChunk("an answer"), finishChunk("stop", true)),
		resetAfter(textChunk("half a summary")),
		sseReply(textChunk(segmentSummary("the summary")), finishChunk("stop", true)),
	)
	s := retrySession(t, w, budgetOf(3))
	s.sleep = func(context.Context, time.Duration) {}
	if _, err := s.Run(context.Background(), "go", nil); err != nil {
		t.Fatal(err)
	}
	var ev events
	if _, err := s.Compact(context.Background(), "", "/compact", ev.sink); err != nil {
		t.Fatal(err)
	}
	if n := len(w.requests()); n != 3 {
		t.Fatalf("%d requests; want the answer's and the summary's two", n)
	}
	if rs := of[Retrying](ev.list()); len(rs) != 0 {
		t.Fatalf("Retrying = %+v; a summary's attempts are no step's", rs)
	}
	if got := compactions(t, s); len(got) != 1 || !strings.HasSuffix(got[0], " ok") {
		t.Fatalf("compactions = %v", got)
	}
}

// TestAFailedAttemptsUsageIsReportedNowhere: a response that reports usage and
// then fails — Anthropic's and the Responses API's do — leaves no StepDone and no
// Spent, and the turn's Usage is what its finished steps spent: documented as
// spend no report holds.
func TestAFailedAttemptsUsageIsReportedNowhere(t *testing.T) {
	_, s, a := overflowFixture(t, nil)
	billed := []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonError,
		Usage: fantasy.Usage{InputTokens: 7777, OutputTokens: 7777, TotalTokens: 15554}}}
	a.push(reply(openText("abandoned"), billed, transientPart()), answerWith("done"))
	var ev events
	res, err := s.Run(context.Background(), "go", ev.sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.Input == 7777 || usageOf(ev.list()).Input == 7777 {
		t.Fatalf("the failed attempt's usage reached a report: %+v, %+v", res.Usage, usageOf(ev.list()))
	}
	if got := stepsOf(ev.list()); !slices.Equal(got, []int{1}) {
		t.Fatalf("steps = %v; want the finished attempt's alone", got)
	}
	if res.Usage != *store.UsageOf(stepUsage) {
		t.Fatalf("Usage = %+v; want the finished step's %+v", res.Usage, *store.UsageOf(stepUsage))
	}
}

// A failure a long Retry-After kept from a retry still says a later turn may do
// better: kind provider, retryable.
func TestALongWaitIsFinalAndRetryable(t *testing.T) {
	err := classify(&llm.MidStreamError{Message: "busy", StatusCode: 529, Type: "overloaded_error"}, modeltable.Resolved{})
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("classify = %v (%T)", err, err)
	}
	if info := pe.ErrorInfo(); info.Kind != errkind.Provider || !info.Retryable {
		t.Fatalf("ErrorInfo = %+v; want provider, retryable", info)
	}
}
