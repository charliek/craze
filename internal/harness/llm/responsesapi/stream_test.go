package responsesapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestStreamNeedsNoContentType: the route streams with no Content-Type
// header at all (spike). The fixture's reply really has none — read raw, as
// the control — and the client reads it to its completion.
func TestStreamNeedsNoContentType(t *testing.T) {
	reply := events(created(), added(0, message("msg_1", "")), textDelta(0, "msg_1", "Hello"),
		itemDone(0, message("msg_1", "Hello")), completed(10, 0, 2, 0))
	srv := newServer(t, reply, reply)

	raw, err := http.Post(srv.srv.URL+"/v1/responses", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, raw.Body)
	_ = raw.Body.Close()
	if ct, ok := raw.Header["Content-Type"]; ok {
		t.Fatalf("the fixture's reply has a Content-Type (%q); the test would prove nothing", ct)
	}

	evs, err := streamOf(t, srv)
	if err != nil {
		t.Fatalf("a stream with no Content-Type failed: %v", err)
	}
	if got := deltas(evs, TextDelta, 0); got != "Hello" {
		t.Fatalf("text = %q, want Hello", got)
	}
}

// TestStreamAssemblesAToolStep: a reasoning item and a function call, as
// the spike's gpt-6-astra tool step streamed them. Every item opens with
// ItemAdded, its deltas follow, and ItemDone carries the item as its end
// event did; Completed comes last, with the usage.
func TestStreamAssemblesAToolStep(t *testing.T) {
	srv := newServer(t, events(
		created(),
		added(0, reasoning("rs_1", "sealed-partial")),
		summaryDelta(0, "rs_1", 0, "**Verifying**"),
		itemDone(0, reasoning("rs_1", "sealed-whole-blob", "**Verifying**")),
		added(1, call("fc_1", "call_1", "read_file", "")),
		argsDelta(1, "fc_1", `{"path":`),
		argsDelta(1, "fc_1", `"README.md"}`),
		itemDone(1, call("fc_1", "call_1", "read_file", `{"path":"README.md"}`)),
		completed(130, 0, 253, 231),
	))
	evs, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"added0", "reasoning0", "done0", "added1", "args1", "args1", "done1", "completed"}
	if got := kinds(evs); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	r, c := evs[2].Item, evs[6].Item
	if r.Type != "reasoning" || r.ID != "rs_1" || r.EncryptedContent != "sealed-whole-blob" || !slices.Equal(r.Summary, []string{"**Verifying**"}) {
		t.Errorf("reasoning item = %+v", r)
	}
	if c.Type != "function_call" || c.CallID != "call_1" || c.Name != "read_file" || c.Namespace != Namespace || c.Arguments != `{"path":"README.md"}` {
		t.Errorf("call item = %+v", c)
	}
	if u := evs[len(evs)-1].Usage; u.InputTokens != 130 || u.OutputTokens != 253 || u.ReasoningTokens != 231 || u.TotalTokens != 383 {
		t.Errorf("usage = %+v", u)
	}
}

// TestStreamReadsSealedReasoningFromItsEnd: the encrypted content an item
// begins with is incomplete (spike: 1,380 bytes at the start, 1,996 at the
// end); the item reported done carries the end's. The control: the start
// really carried another blob.
func TestStreamReadsSealedReasoningFromItsEnd(t *testing.T) {
	srv := newServer(t, events(
		added(0, reasoning("rs_1", "partial")),
		itemDone(0, reasoning("rs_1", "partial-then-the-rest")),
		added(1, message("msg_1", "")),
		itemDone(1, message("msg_1", "ok")),
		completed(1, 0, 1, 0),
	))
	evs, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Kind != ItemAdded || evs[0].Item.EncryptedContent != "partial" {
		t.Fatalf("the start event = %+v; the fixture does not differ", evs[0])
	}
	var done []OutputItem
	for _, e := range evs {
		if e.Kind == ItemDone {
			done = append(done, e.Item)
		}
	}
	if len(done) != 2 || done[0].EncryptedContent != "partial-then-the-rest" {
		t.Fatalf("done items = %+v, want the reasoning's blob from its end", done)
	}
}

// TestStreamKeepsParallelCallsApartByIndex: two calls open, their argument
// deltas interleave, and the second finishes first. Each delta stays with
// its own index, each call keeps its own arguments, and ItemDone reports
// the items in output_index order all the same.
func TestStreamKeepsParallelCallsApartByIndex(t *testing.T) {
	srv := newServer(t, events(
		added(0, call("fc_a", "call_a", "read", "")),
		added(1, call("fc_b", "call_b", "read", "")),
		argsDelta(0, "fc_a", `{"path":`),
		argsDelta(1, "fc_b", `{"path":`),
		argsDelta(1, "fc_b", `"b.txt"}`),
		argsDelta(0, "fc_a", `"a.txt"}`),
		itemDone(1, call("fc_b", "call_b", "read", `{"path":"b.txt"}`)),
		itemDone(0, call("fc_a", "call_a", "read", `{"path":"a.txt"}`)),
		completed(5, 0, 5, 0),
	))
	evs, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := deltas(evs, ArgumentsDelta, 0), deltas(evs, ArgumentsDelta, 1); a != `{"path":"a.txt"}` || b != `{"path":"b.txt"}` {
		t.Fatalf("argument deltas: 0 = %q, 1 = %q", a, b)
	}
	var done []string
	for _, e := range evs {
		if e.Kind == ItemDone {
			done = append(done, fmt.Sprintf("%d:%s:%s", e.Index, e.Item.CallID, e.Item.Arguments))
		}
	}
	want := []string{`0:call_a:{"path":"a.txt"}`, `1:call_b:{"path":"b.txt"}`}
	if !slices.Equal(done, want) {
		t.Fatalf("done = %v, want %v (index order, each call its own arguments)", done, want)
	}
}

// TestStreamReleasesHeldItemsAtCompletion: an index no event ever names
// cannot hold the items after it past the response's end.
func TestStreamReleasesHeldItemsAtCompletion(t *testing.T) {
	srv := newServer(t, events(
		added(1, message("msg_1", "")),
		itemDone(1, message("msg_1", "after a gap")),
		completed(1, 0, 1, 0),
	))
	evs, err := streamOf(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kinds(evs), []string{"added1", "text1", "done1", "completed"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// TestStreamNeverCountsTextTwice: an item's text is its end event's; the
// deltas stream it, and what they did not send goes as one more delta, so
// the deltas add up to the item's text — never the text again on top of
// them.
func TestStreamNeverCountsTextTwice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		deltas []string
		final  string
		want   string // what the deltas add up to
		count  int    // how many text deltas there are
	}{
		{"the deltas are the text", []string{"Hel", "lo"}, "Hello", "Hello", 2},
		{"no deltas", nil, "Hello", "Hello", 1},
		{"the deltas fall short", []string{"Hel"}, "Hello", "Hello", 2},
		{"the deltas disagree", []string{"Bye"}, "Hello", "Bye", 1}, // what was shown stands
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs := []string{added(0, message("msg_1", ""))}
			for _, d := range tc.deltas {
				evs = append(evs, textDelta(0, "msg_1", d))
			}
			evs = append(evs, itemDone(0, message("msg_1", tc.final)), completed(1, 0, 1, 0))
			got, err := streamOf(t, newServer(t, events(evs...)))
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, e := range got {
				if e.Kind == TextDelta {
					n++
				}
			}
			if text := deltas(got, TextDelta, 0); text != tc.want || n != tc.count {
				t.Fatalf("deltas = %q in %d events, want %q in %d", text, n, tc.want, tc.count)
			}
		})
	}
	t.Run("a reasoning summary in two parts", func(t *testing.T) {
		got, err := streamOf(t, newServer(t, events(
			added(0, reasoning("rs_1", "")),
			summaryDelta(0, "rs_1", 0, "first"),
			summaryDelta(0, "rs_1", 1, "sec"),
			itemDone(0, reasoning("rs_1", "blob", "first", "second")),
			completed(1, 0, 1, 0),
		)))
		if err != nil {
			t.Fatal(err)
		}
		if text := deltas(got, ReasoningDelta, 0); text != "first"+SummarySeparator+"second" {
			t.Fatalf("summary deltas = %q", text)
		}
	})
}

// TestStreamMakesUpAnUnannouncedItem: a delta for an index no added event
// opened still opens it first, so a reader always sees an item begin.
func TestStreamMakesUpAnUnannouncedItem(t *testing.T) {
	got, err := streamOf(t, newServer(t, events(
		textDelta(0, "msg_1", "hi"),
		itemDone(0, message("msg_1", "hi")),
		completed(1, 0, 1, 0),
	)))
	if err != nil {
		t.Fatal(err)
	}
	if k := kinds(got); !slices.Equal(k, []string{"added0", "text0", "done0", "completed"}) || got[0].Item.Type != "message" {
		t.Fatalf("events = %v (%+v)", k, got[0])
	}
}

// TestStreamSucceedsOnlyOnCompleted: a stream that stops before
// response.completed — at a clean end of the body, or at [DONE] — is
// ErrIncomplete, a truncation (io.ErrUnexpectedEOF), however much it
// streamed; the same events with response.completed after them (the
// control) succeed.
func TestStreamSucceedsOnlyOnCompleted(t *testing.T) {
	body := []string{added(0, message("msg_1", "")), textDelta(0, "msg_1", "almost"), itemDone(0, message("msg_1", "almost"))}
	for _, tc := range []struct {
		name  string
		reply http.HandlerFunc
	}{
		{"the body ends", events(body...)},
		{"[DONE]", func(w http.ResponseWriter, r *http.Request) {
			events(body...)(w, r)
			fmt.Fprint(w, "data: [DONE]\n\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs, err := streamOf(t, newServer(t, tc.reply))
			if !errors.Is(err, ErrIncomplete) || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("err = %v, want ErrIncomplete", err)
			}
			if slices.ContainsFunc(evs, func(e Event) bool { return e.Kind == Completed }) {
				t.Fatal("an incomplete stream reported Completed")
			}
		})
	}
	t.Run("completed", func(t *testing.T) {
		if _, err := streamOf(t, newServer(t, events(append(body, completed(1, 0, 1, 0))...))); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestStreamFailuresKeepTheirCodeAndUsage: a failure inside the stream —
// the usage limit arriving after output began (OpenAI's docs say it can), a
// usage check that could not run, an incomplete response, an error event —
// ends the stream with an *Error keeping the code or reason, the message,
// and the usage the response reported.
func TestStreamFailuresKeepTheirCodeAndUsage(t *testing.T) {
	text := []string{added(0, message("msg_1", "")), textDelta(0, "msg_1", "partial answer")}
	for _, tc := range []struct {
		name      string
		last      string
		event     string
		code      string
		reason    string
		message   string
		usage     bool
		final     bool
		transient bool
	}{
		{"usage limit after output", failed("subscription_sharing_usage_limit_exceeded", "You've hit your usage limit.", usageObject(300, 0, 40, 0)),
			"response.failed", "subscription_sharing_usage_limit_exceeded", "", "You've hit your usage limit.", true, true, false},
		{"usage unavailable", failed("subscription_sharing_usage_unavailable", "try later", nil),
			"response.failed", "subscription_sharing_usage_unavailable", "", "try later", false, false, true},
		{"incomplete", incomplete("max_output_tokens", usageObject(300, 0, 4000, 3900)),
			"response.incomplete", "", "max_output_tokens", "", true, false, false},
		{"error event", ev(map[string]any{"type": "error", "code": "server_error", "message": "boom", "param": nil}),
			"error", "server_error", "", "boom", false, false, true},
		{"error event, nested", ev(map[string]any{"type": "error", "error": map[string]any{"code": "rate_limit_exceeded", "message": "slow down"}}),
			"error", "rate_limit_exceeded", "", "slow down", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs, err := streamOf(t, newServer(t, events(append(slices.Clone(text), tc.last)...)))
			e := asError(t, err)
			if e.Event != tc.event || e.Code != tc.code || e.Reason != tc.reason || e.Message != tc.message || e.StatusCode != 0 {
				t.Fatalf("error = %+v", e)
			}
			if (e.Usage != nil) != tc.usage {
				t.Fatalf("usage = %+v, want kept: %v", e.Usage, tc.usage)
			}
			if e.Final() != tc.final || e.Transient() != tc.transient {
				t.Fatalf("final %v transient %v, want %v %v", e.Final(), e.Transient(), tc.final, tc.transient)
			}
			if deltas(evs, TextDelta, 0) != "partial answer" {
				t.Fatal("the text before the failure was not streamed")
			}
		})
	}
}

// TestStreamRefusesAMalformedEvent: an event that is not JSON ends the
// stream with an error, never a success.
func TestStreamRefusesAMalformedEvent(t *testing.T) {
	_, err := streamOf(t, newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "data: {not json\n\n")
	}))
	if err == nil || errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want a decoding error", err)
	}
}

// TestStreamCancelledIsTheContextsError: a stream read while its context is
// cancelled ends with the context's error.
func TestStreamCancelledIsTheContextsError(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		events(added(0, message("msg_1", "")), textDelta(0, "msg_1", "hi"))(w, r)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		case <-time.After(10 * time.Second):
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := srv.client(t, newCreds(tokenOne)).Stream(ctx, Request{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for s.Next() {
		if s.Event().Kind == TextDelta {
			cancel()
		}
	}
	if !errors.Is(s.Err(), context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", s.Err())
	}
}
