package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness"
)

// The native session's side questions (native_aside.go, plan 040 §3.4,
// §7.5): Asides is always true and readiness is a refusal; pieces, text,
// label and errors sanitized; the ChatGPT usage latch lifted; nothing
// emitted or journaled but the idle usage delta; a stale Spent dropped by its
// Seq; and Close joins the publication after the harness's call.

// asked is what an Aside returned, with the pieces it streamed.
type asked struct {
	r      AsideResult
	deltas []string
	err    error
}

// askNative is s.Aside with a background context: the question q after the
// earlier pairs prior.
func askNative(s *nativeSession, q string, prior ...AsideTurn) asked {
	var mu sync.Mutex
	var deltas []string
	r, err := s.Aside(context.Background(), AsideRequest{Question: q, Prior: prior}, func(d string) {
		mu.Lock()
		defer mu.Unlock()
		deltas = append(deltas, d)
	})
	mu.Lock()
	defer mu.Unlock()
	return asked{r: r, deltas: deltas, err: err}
}

// startNativeAside is askNative on its own goroutine.
func startNativeAside(s *nativeSession, q string) <-chan asked {
	out := make(chan asked, 1)
	go func() { out <- askNative(s, q) }()
	return out
}

// TestNativeAsideReadiness (§3.3, §3.4): a native session always says it
// answers side questions, and refuses one with ErrAsideNotReady — sending
// nothing — before Start, while a load holds it, and after Close.
func TestNativeAsideReadiness(t *testing.T) {
	f := newNativeFixture(t)
	a := f.models["test/a"]
	a.push(answer("never sent"))
	s := f.session(Options{})
	if !s.Asides() {
		t.Fatal("an unstarted native session says it has no side questions")
	}
	if o := askNative(s, "q"); !errors.Is(o.err, ErrAsideNotReady) {
		t.Fatalf("before Start: %v, want ErrAsideNotReady", o.err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.loading = true
	s.mu.Unlock()
	o := askNative(s, "q")
	s.mu.Lock()
	s.loading = false
	s.mu.Unlock()
	if !errors.Is(o.err, ErrAsideNotReady) {
		t.Fatalf("while loading: %v, want ErrAsideNotReady", o.err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if o := askNative(s, "q"); !errors.Is(o.err, ErrAsideNotReady) {
		t.Fatalf("after Close: %v, want ErrAsideNotReady", o.err)
	}
	if !s.Asides() {
		t.Fatal("a closed native session says it has no side questions")
	}
	if n := a.callCount(); n != 0 {
		t.Fatalf("the refused side questions sent %d requests, want none", n)
	}
}

// TestNativeAsideSanitizes (§3.4, P8): each piece is sanitized before onDelta
// — an escape split across two pieces may show in the stream — and the
// final text, which corrects it, the model label and an error's text are
// sanitized too.
func TestNativeAsideSanitizes(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{Model: "test/b"})
	b := f.models["test/b"]
	b.push(answer("\x1b[31mred\x1b[0m and \x1b[3", "1mmore"))
	o := askNative(s, "q")
	if o.err != nil {
		t.Fatal(o.err)
	}
	for _, d := range o.deltas {
		if strings.ContainsAny(d, "\x1b\u009b") {
			t.Fatalf("a streamed piece holds an escape: %q", d)
		}
	}
	if got := strings.Join(o.deltas, ""); got != "red and 1mmore" {
		t.Fatalf("the pieces joined are %q; want each piece sanitized on its own", got)
	}
	if o.r.Text != "red and more" {
		t.Fatalf("the answer is %q, want the whole text sanitized at once", o.r.Text)
	}
	if o.r.Model != "Model B" {
		t.Fatalf("the model label is %q, want the table's name sanitized", o.r.Model)
	}
	if o.r.Usage == nil || o.r.Usage.Input != 10 || o.r.Usage.Output != 5 {
		t.Fatalf("the usage is %+v, want the step's", o.r.Usage)
	}

	b.push(reply(errorParts(&fantasy.ProviderError{StatusCode: 400, Message: "bad \x1b]0;pwned\x07 request"})))
	o = askNative(s, "q")
	if o.err == nil || strings.ContainsAny(o.err.Error(), "\x1b\x07") || !strings.Contains(o.err.Error(), "HTTP 400") {
		t.Fatalf("the failure is %q; want the provider's, sanitized", o.err)
	}

	b.push(answer("\x1b[31m\x1b[0m"))
	if o := askNative(s, "q"); !errors.Is(o.err, ErrAsideEmpty) {
		t.Fatalf("an answer that is only escapes: %v, want ErrAsideEmpty", o.err)
	}
}

// TestNativeAsideErrors: a tool call instead of an answer is ErrAsideEmpty,
// saying so; the earlier pairs reach the model, the last answer just before
// the question; and a side question the session's Close cut short is
// ErrAsideNotReady.
func TestNativeAsideErrors(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	a := f.models["test/a"]
	a.push(nativeCallStep("c1", "read", `{"path":"x"}`))
	o := askNative(s, "q")
	if !errors.Is(o.err, ErrAsideEmpty) || !strings.Contains(o.err.Error(), "tried to use a tool") {
		t.Fatalf("a tool call answered %v, want ErrAsideEmpty with the tool's wording", o.err)
	}

	a.push(answer("ok"))
	if o := askNative(s, "now?", AsideTurn{Question: "before?", Answer: "earlier"}); o.err != nil {
		t.Fatal(o.err)
	}
	calls := a.requests()
	last := calls[len(calls)-1].Prompt
	if n := len(last); n < 3 || last[n-2].Role != fantasy.MessageRoleAssistant {
		t.Fatalf("the follow-up's request ends %+v; want the earlier answer before the question", last)
	}

	h := newHeld(t)
	a.push(h.step(textParts("partial"), nil))
	side := startNativeAside(s, "q")
	await(t, h.reached, "the side question's request")
	go func() { _ = s.Close() }()
	if o := await(t, side, "the side question"); !errors.Is(o.err, ErrAsideNotReady) {
		t.Fatalf("a side question Close cut short answered %v, want ErrAsideNotReady", o.err)
	}
}

// TestNativeAsideLiftsTheUsageLatch (§3.4, P33): a side question is a
// person's request, and lifts the ChatGPT plan's usage latch before its
// request is sent, as a turn they start does.
func TestNativeAsideLiftsTheUsageLatch(t *testing.T) {
	noOpenAI(t)
	f := newNativeFixture(t)
	s := f.started(Options{})
	src := chatgptauth.Source(f.dir)
	if s.signIn != src {
		t.Fatal("control: the session's sign-in is not the fixture directory's")
	}
	src.LatchUsageLimit()
	var latched atomic.Bool
	f.models["test/a"].push(func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		latched.Store(src.UsageLimited())
		answer("ok")(ctx, yield)
	})
	if o := askNative(s, "q"); o.err != nil {
		t.Fatal(o.err)
	}
	if latched.Load() {
		t.Fatal("the side question's request went out with the usage latch still set")
	}
}

// TestNativeAsideIsNeverEmittedOrJournaled (§7.3): a side question's text
// reaches no event, no journal line and no transcript; the one thing it
// publishes, idle, is the usage delta, which counts it in the session's
// spend and not the last turn's.
func TestNativeAsideIsNeverEmittedOrJournaled(t *testing.T) {
	f := newNativeFixture(t)
	dir := filepath.Join(t.TempDir(), "journal")
	s := f.started(Options{JournalDir: dir})
	jw := journalOf(t, s.log)
	inc := s.Incarnation()
	a := f.models["test/a"]
	a.push(answer("hi"), answer("SIDE-", "ANSWER"))
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	drained(s)
	turn := s.Snapshot().Usage
	if o := askNative(s, "SIDE-QUESTION"); o.err != nil || o.r.Text != "SIDE-ANSWER" {
		t.Fatalf("the side question: %+v", o)
	}
	_ = s.log.Flush(context.Background(), nil)
	evs := drained(s)
	if idx := usageDeltas(evs); len(idx) != 1 || len(evs) != 1 {
		t.Fatalf("the side question published %d events, %d of them usage deltas; want the usage delta alone:\n%s",
			len(evs), len(idx), strings.Join(loadLines(evs), "\n"))
	}
	u := evs[0].State.Usage
	if u.Turn != turn.Turn || u.Session.Input != 2*turn.Session.Input || u.Session.Output != 2*turn.Session.Output {
		t.Fatalf("the usage delta %+v; want the last turn's spend as it was (%+v) and the session's doubled (%+v)", u, turn.Turn, turn.Session)
	}
	closeJournaled(t, s, jw)
	lines := assertOneJournal(t, dir, jw, inc)
	for _, l := range lines {
		if text := fmt.Sprint(l); strings.Contains(text, "SIDE-") {
			t.Fatalf("a journal line holds the side question's text: %s", text)
		}
	}
	for _, p := range f.transcripts() {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "SIDE-") {
			t.Fatal("the transcript holds the side question's text")
		}
	}
}

// TestNativeSpentDropsAStaleReport (§3.4 step 8, P3): a Spent whose Seq is
// not above the last applied one's is older data, and dropped — one sent out
// of order, an idle side question's computed before a turn's step reported,
// and one of two side questions finishing together.
func TestNativeSpentDropsAStaleReport(t *testing.T) {
	t.Run("out of order", func(t *testing.T) {
		f := newNativeFixture(t)
		s := f.started(Options{})
		s.spent(harness.Spent{ContextTokens: 200, Seq: 7})
		s.spent(harness.Spent{ContextTokens: 100, Seq: 6})
		s.spent(harness.Spent{ContextTokens: 150, Seq: 7})
		if got := s.Snapshot().Usage.ContextTokens; got != 200 {
			t.Fatalf("the snapshot's context is %d, want the newest report's 200", got)
		}
		_ = s.log.Flush(context.Background(), nil)
		if n := len(usageDeltas(drained(s))); n != 1 {
			t.Fatalf("%d usage deltas were published, want only the newest report's", n)
		}
	})

	// The side question's IdleSpent is read; a whole turn then runs and
	// reports before the side question publishes.
	t.Run("a turn between the idle read and the publication", func(t *testing.T) {
		f := newNativeFixture(t)
		reached, release := make(chan struct{}), make(chan struct{})
		var held atomic.Bool
		s := f.session(Options{})
		s.asideSeam = func(stage string) {
			if stage == "spent" && held.CompareAndSwap(false, true) {
				close(reached)
				<-release
			}
		}
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		a := f.models["test/a"]
		a.push(answer("one"), answer("side"), answer("two"))
		if _, err := s.Prompt(context.Background(), "one"); err != nil {
			t.Fatal(err)
		}
		side := startNativeAside(s, "q")
		await(t, reached, "the side question's publication")
		if _, err := s.Prompt(context.Background(), "two"); err != nil {
			t.Fatal(err)
		}
		after := *s.Snapshot().Usage
		close(release)
		if o := await(t, side, "the side question"); o.err != nil {
			t.Fatal(o.err)
		}
		if got := *s.Snapshot().Usage; got != after {
			t.Fatalf("the side question's stale report replaced the turn's:\n got %+v\nwant %+v", got, after)
		}
		if after.Session.Input != 30 {
			t.Fatalf("the turn's report counts %d input tokens, want three steps' 30 (the side question's among them)", after.Session.Input)
		}
	})

	// Two side questions: the first's report is read, the second runs and
	// publishes, then the first publishes.
	t.Run("two side questions finishing together", func(t *testing.T) {
		f := newNativeFixture(t)
		reached, release := make(chan struct{}), make(chan struct{})
		var held atomic.Bool
		s := f.session(Options{})
		s.asideSeam = func(stage string) {
			if stage == "spent" && held.CompareAndSwap(false, true) {
				close(reached)
				<-release
			}
		}
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		a := f.models["test/a"]
		a.push(answer("one"), answer("side one"), answer("side two"))
		if _, err := s.Prompt(context.Background(), "one"); err != nil {
			t.Fatal(err)
		}
		first := startNativeAside(s, "q1")
		await(t, reached, "the first side question's publication")
		if o := askNative(s, "q2"); o.err != nil {
			t.Fatal(o.err)
		}
		after := *s.Snapshot().Usage
		close(release)
		if o := await(t, first, "the first side question"); o.err != nil {
			t.Fatal(o.err)
		}
		if got := *s.Snapshot().Usage; got != after || after.Session.Input != 30 {
			t.Fatalf("the usage after both is %+v, want the second's report, three steps' (%+v)", got, after)
		}
	})
}

// TestNativeCloseJoinsAnAsidesPublication (§3.4, P9): Close returns only
// once a side question's publication after the harness's call has run, so
// nothing is published after Close.
func TestNativeCloseJoinsAnAsidesPublication(t *testing.T) {
	f := newNativeFixture(t)
	s := f.session(Options{})
	reached, joining, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var order, asideAt, closeAt atomic.Int32
	s.asideSeam = func(stage string) {
		switch stage {
		case "spent":
			close(reached)
			// On until Close joins the side questions — or, were it not
			// to, has returned without them.
			select {
			case <-joining:
			case <-closed:
			}
			asideAt.Store(order.Add(1))
		case "join":
			close(joining)
		}
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.models["test/a"].push(answer("side"))
	side := startNativeAside(s, "q")
	await(t, reached, "the side question's publication")
	go func() {
		_ = s.Close()
		closeAt.Store(order.Add(1))
		close(closed)
	}()
	await(t, closed, "Close")
	if o := await(t, side, "the side question"); o.err != nil {
		t.Fatal(o.err)
	}
	if asideAt.Load() > closeAt.Load() {
		t.Fatalf("Close returned (%d) before the side question's publication ran (%d)", closeAt.Load(), asideAt.Load())
	}
}

// owedAfter is the one usage delta in evs after the event at end, failing the
// test unless there is exactly one, and the last usage delta at or before it.
func owedAfter(t *testing.T, evs []Event, end int) (before, after *UsageState) {
	t.Helper()
	var later []int
	for _, i := range usageDeltas(evs) {
		switch {
		case i > end:
			later = append(later, i)
		default:
			before = evs[i].State.Usage
		}
	}
	if end < 0 || before == nil || len(later) != 1 {
		t.Fatalf("the ending is at %d, with %d usage deltas after it; want the turn's own before it and exactly one after:\n%s",
			end, len(later), strings.Join(loadLines(evs), "\n"))
	}
	return before, evs[later[0]].State.Usage
}

// TestNativeAsideReportOwedPastTheEnd (§3.4 step 8, Codex review of C2): a
// side question that finishes after the running turn's last report, and
// before the turn has ended — its IdleSpent refused — is published once the
// ending is out: exactly one further usage delta, after the EventDone, whose
// session spend has the side question's usage and whose turn spend is the
// turn's own; the snapshot ends on it. A prompt's turn and a /compact alike
// (both end in prompt).
func TestNativeAsideReportOwedPastTheEnd(t *testing.T) {
	for _, tc := range []struct {
		name, prompt string
		first        bool // a turn runs first: /compact needs something to compact
		step         step // the turn's one request: its step, or the summarizer's
	}{
		{"a turn", "go", false, answer("work")},
		{"a /compact", "/compact", true, nativeSummary("The user said hello.")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t)
			s := f.session(Options{})
			reached, release := make(chan struct{}), make(chan struct{})
			var armed atomic.Bool
			// The turn is held in the adapter's sink as its last report
			// arrives: from here until the release it has no report left to
			// make, and has not ended.
			s.mu.Lock()
			s.sinkSeam = func(ev harness.Event) {
				if _, ok := ev.(harness.Spent); ok && armed.CompareAndSwap(true, false) {
					close(reached)
					<-release
				}
			}
			s.mu.Unlock()
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			takeStartDelta(t, s.log)
			a := f.models["test/a"]
			if tc.first {
				a.push(answer("hi"))
				if _, err := s.Prompt(context.Background(), "hello"); err != nil {
					t.Fatal(err)
				}
				_ = s.log.Flush(context.Background(), nil)
				drained(s)
			}
			a.push(tc.step)
			armed.Store(true)
			out := startPrompt(s, tc.prompt)
			await(t, reached, "the turn's last report")
			a.push(answer("side"))
			if o := askNative(s, "q"); o.err != nil {
				t.Fatal(o.err)
			}
			close(release)
			if o := await(t, out, "the turn"); o.err != nil {
				t.Fatal(o.err)
			}
			_ = s.log.Flush(context.Background(), nil)
			evs := drained(s)
			turn, owed := owedAfter(t, evs, indexOf(evs, EventDone))
			if owed.Turn != turn.Turn || owed.Session.Input != turn.Session.Input+10 || owed.Session.Output != turn.Session.Output+5 {
				t.Fatalf("the usage after the end is %+v; want the turn's spend as it was (%+v) and the session's (%+v) plus the side question's",
					owed, turn.Turn, turn.Session)
			}
			if got := s.Snapshot().Usage; got == nil || *got != *owed {
				t.Fatalf("the snapshot's usage is %+v, want the last delta's %+v", got, owed)
			}
		})
	}
}

// TestNativeWakeAsideReportOwedPastTheEnd (§3.4 step 8, Codex review of C2):
// the same for a wake — a side question that finishes after the wake's last
// report and before it has ended is published once the wake's ending bracket
// is out, as exactly one further usage delta with its usage.
func TestNativeWakeAsideReportOwedPastTheEnd(t *testing.T) {
	rig := newWakeRig(t, Options{}, nil)
	child := newHeld(t)
	id := rig.spawnOne(child, answer("woke"), answer("side"))
	reached, release := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	armed.Store(true)
	// The wake is held in the adapter's sink as its last report arrives.
	rig.hookBefore(func(ev harness.Event) {
		if _, ok := ev.(harness.Spent); ok && armed.CompareAndSwap(true, false) {
			close(reached)
			<-release
		}
	})
	rig.finish(child, id)
	rig.awaitDecided(true, "the result pending")
	await(t, reached, "the wake's last report")
	if o := askNative(rig.s, "q"); o.err != nil {
		t.Fatal(o.err)
	}
	close(release)
	rig.bracket(false, 1, "wake-1")
	// The recheck the ending kicks runs after the publication.
	rig.awaitDecided(false, "the wake's ending, nothing pending")
	_ = rig.s.log.Flush(context.Background(), nil)
	rig.w.pause()
	evs := rig.w.events()
	wake, owed := owedAfter(t, evs, indexWhere(evs, 0, isBracket(false)))
	if owed.Turn != wake.Turn || owed.Session.Input != wake.Session.Input+10 || owed.Session.Output != wake.Session.Output+5 {
		t.Fatalf("the usage after the wake is %+v; want the wake's turn spend as it was (%+v) and the session's (%+v) plus the side question's",
			owed, wake.Turn, wake.Session)
	}
	if got := rig.s.Snapshot().Usage; got == nil || *got != *owed {
		t.Fatalf("the snapshot's usage is %+v, want the last delta's %+v", got, owed)
	}
}
