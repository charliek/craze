package host

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// t0 is the fake clock's start, in unix µs. Seqs are asserted against it.
const t0 = int64(1_800_000_000_000_000)

type fakeClock struct {
	mu sync.Mutex
	us int64
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.UnixMicro(c.us)
}

func (c *fakeClock) set(us int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.us = us
}

// fakeTimers records every debounce timer the Hub starts. Nothing fires until
// a test calls fire, so a held Idle is held for exactly as long as the test
// says.
type fakeTimers struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

type fakeTimer struct {
	d       time.Duration
	c       chan time.Time
	stopped atomic.Bool
}

func (f *fakeTimers) New(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{d: d, c: make(chan time.Time, 1)}
	f.timers = append(f.timers, t)
	return t
}

func (f *fakeTimers) all() []*fakeTimer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.timers)
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }
func (t *fakeTimer) Stop() bool          { return !t.stopped.Swap(true) }

// fire delivers a value whether or not the timer was stopped, so a test proves
// the Hub never leans on Stop to ignore a timer it has moved past.
func (t *fakeTimer) fire() {
	select {
	case t.c <- time.Time{}:
	default:
	}
}

type call struct {
	release bool
	status  Status
	seq     uint64
}

func (c call) String() string {
	if c.release {
		return "release"
	}
	return c.status.Kind.String() + " " + c.status.Message
}

// recorder is a Reporter that records every call on entry, then runs the
// test's hook, if any, for the result.
type recorder struct {
	name      string
	onReport  func(ctx context.Context, s Status, seq uint64) error
	onRelease func(ctx context.Context, seq uint64) error

	mu    sync.Mutex
	calls []call
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Report(ctx context.Context, s Status, seq uint64) error {
	r.record(call{status: s, seq: seq})
	if r.onReport != nil {
		return r.onReport(ctx, s, seq)
	}
	return nil
}

func (r *recorder) Release(ctx context.Context, seq uint64) error {
	r.record(call{release: true, seq: seq})
	if r.onRelease != nil {
		return r.onRelease(ctx, seq)
	}
	return nil
}

func (r *recorder) record(c call) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *recorder) snapshot() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func (r *recorder) trace() []string {
	var out []string
	for _, c := range r.snapshot() {
		out = append(out, c.String())
	}
	return out
}

// waitCalls polls until r has at least n calls and returns them. A timeout
// reports the calls as they stand at the deadline, not at the start.
func (r *recorder) waitCalls(t *testing.T, n int) []call {
	t.Helper()
	if !poll(func() bool { return len(r.snapshot()) >= n }) {
		t.Fatalf("timed out waiting for %s to record %d calls (has %v)", r.name, n, r.trace())
	}
	return r.snapshot()
}

func (r *recorder) wantTrace(t *testing.T, want ...string) {
	t.Helper()
	if got := r.trace(); !slices.Equal(got, want) {
		t.Fatalf("%s calls\n got  %q\n want %q", r.name, got, want)
	}
}

// parkFirst makes r's first Report announce itself on entered and wait for
// release to be closed; later Reports return at once.
func parkFirst(r *recorder) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	first := true // only the worker goroutine calls Report
	r.onReport = func(context.Context, Status, uint64) error {
		if first {
			first = false
			close(entered)
			<-release
		}
		return nil
	}
	return entered, release
}

type warnLog struct {
	mu    sync.Mutex
	lines []string
}

func (w *warnLog) warn(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, s)
}

func (w *warnLog) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.lines)
}

// waitFor polls until want is satisfied, so tests never sleep a fixed time.
func waitFor(t *testing.T, what string, want func() bool) {
	t.Helper()
	if !poll(want) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// poll reports whether want became true before a generous deadline. The
// deadline only bounds a failure; a passing run returns as soon as it can.
func poll(want func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if want() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return want()
}

type rig struct {
	h      *Hub
	clock  *fakeClock
	timers *fakeTimers
	warns  *warnLog
}

// newRig builds a Hub on a fixed fake clock and fake timers. SendTimeout and
// CloseTimeout are far beyond any passing run, so no test depends on how fast
// the machine is; a test that exercises a budget sets its own.
func newRig(t *testing.T, tweak func(*HubOptions), rs ...Reporter) *rig {
	t.Helper()
	g := &rig{clock: &fakeClock{us: t0}, timers: &fakeTimers{}, warns: &warnLog{}}
	o := HubOptions{
		Now:          g.clock.Now,
		NewTimer:     g.timers.New,
		SendTimeout:  time.Minute,
		CloseTimeout: 10 * time.Second,
		Warn:         g.warns.warn,
	}
	if tweak != nil {
		tweak(&o)
	}
	g.h = NewHub(rs, o)
	t.Cleanup(func() { g.h.Close(context.Background()) })
	return g
}

func st(k Kind, tag string) Status { return Status{Kind: k, Message: tag} }

// tagged is st with the Kind read off the tag's first letter: B, F, I or W.
func tagged(tag string) Status {
	return st(map[byte]Kind{'B': Blocked, 'F': Failed, 'I': Idle, 'W': Working}[tag[0]], tag)
}

// D6: a status identical to the last one published is not sent and does not
// take a seq.
func TestHubDedup(t *testing.T) {
	r := &recorder{name: "r"}
	entered, release := parkFirst(r)
	g := newRig(t, nil, r)

	g.h.Publish(st(Working, "w"))
	<-entered
	// Parked, so a duplicate that got queued could not hide by collapsing
	// into a send that already happened.
	g.h.Publish(st(Working, "w"))
	g.h.Publish(st(Blocked, "b"))
	g.h.Publish(st(Blocked, "b"))
	close(release)

	calls := r.waitCalls(t, 2)
	r.wantTrace(t, "working w", "blocked b")
	if calls[0].seq != uint64(t0) || calls[1].seq != uint64(t0)+1 {
		t.Fatalf("a duplicate took a seq: %d, %d", calls[0].seq, calls[1].seq)
	}

	// An identical Idle does not restart the debounce either.
	g.h.Publish(st(Idle, "i"))
	g.h.Publish(st(Idle, "i"))
	if n := len(g.timers.all()); n != 1 {
		t.Fatalf("%d idle timers, want 1", n)
	}
	g.timers.all()[0].fire()
	calls = r.waitCalls(t, 3)
	r.wantTrace(t, "working w", "blocked b", "idle i")
	if calls[2].seq != uint64(t0)+2 {
		t.Fatalf("idle seq %d, want %d", calls[2].seq, t0+2)
	}
}

func TestHubHeldIdleDroppedByWorking(t *testing.T) {
	r := &recorder{name: "r"}
	g := newRig(t, nil, r)

	g.h.Publish(st(Working, "w1"))
	r.waitCalls(t, 1)
	g.h.Publish(st(Idle, "i"))
	timers := g.timers.all()
	if len(timers) != 1 || timers[0].d != defaultIdleDelay {
		t.Fatalf("idle should start one %v timer: %d timers", defaultIdleDelay, len(timers))
	}
	g.h.Publish(st(Working, "w2"))
	if !timers[0].stopped.Load() {
		t.Fatal("a Working must stop the held Idle's timer")
	}
	// The stale timer delivering anyway must not resurrect the Idle.
	timers[0].fire()
	g.h.Publish(st(Blocked, "b"))
	r.waitCalls(t, 3)
	r.wantTrace(t, "working w1", "working w2", "blocked b")
}

func TestHubIdleSentWhenTimerFires(t *testing.T) {
	r := &recorder{name: "r"}
	g := newRig(t, func(o *HubOptions) { o.IdleDelay = 7 * time.Millisecond }, r)

	g.h.Publish(st(Working, "w"))
	r.waitCalls(t, 1)

	g.clock.set(t0 + 1000)
	g.h.Publish(st(Idle, "stop"))
	g.clock.set(t0 + 2000)
	g.h.Publish(st(Idle, "cancelled")) // restarts the delay with its own seq
	timers := g.timers.all()
	if len(timers) != 2 || timers[1].d != 7*time.Millisecond {
		t.Fatalf("each Idle starts an IdleDelay timer: %d timers", len(timers))
	}
	if !timers[0].stopped.Load() {
		t.Fatal("a new Idle must stop the previous one's timer")
	}
	r.wantTrace(t, "working w") // held, not sent

	g.clock.set(t0 + 9000)
	timers[0].fire() // replaced: ignored
	timers[1].fire()
	calls := r.waitCalls(t, 2)
	r.wantTrace(t, "working w", "idle cancelled")
	if calls[1].seq != uint64(t0)+2000 {
		t.Fatalf("idle seq %d: it keeps the seq it was published with, %d", calls[1].seq, t0+2000)
	}
}

func TestHubKindsNeverMerge(t *testing.T) {
	r := &recorder{name: "r"}
	entered, release := parkFirst(r)
	g := newRig(t, nil, r)

	g.h.Publish(st(Working, "w0"))
	<-entered
	g.h.Publish(st(Blocked, "b"))
	g.h.Publish(st(Failed, "f"))
	g.h.Publish(st(Working, "w"))
	close(release)

	r.waitCalls(t, 4)
	r.wantTrace(t, "working w0", "blocked b", "failed f", "working w")
}

func TestHubSameKindCollapsesToNewest(t *testing.T) {
	r := &recorder{name: "r"}
	entered, release := parkFirst(r)
	g := newRig(t, nil, r)

	g.h.Publish(st(Working, "w0"))
	<-entered
	for _, s := range []Status{
		st(Blocked, "b1"), st(Blocked, "b2"),
		st(Failed, "f1"), st(Failed, "f2"), st(Failed, "f3"),
		st(Working, "w1"), st(Working, "w2"),
	} {
		g.h.Publish(s)
	}
	close(release)

	calls := r.waitCalls(t, 4)
	r.wantTrace(t, "working w0", "blocked b2", "failed f3", "working w2")
	for i, want := range []uint64{0, 2, 5, 7} {
		if calls[i].seq != uint64(t0)+want {
			t.Fatalf("call %d seq %d, want t0+%d: the newest entry's seq survives", i, calls[i].seq, want)
		}
	}
}

func TestHubFullQueueDropOrder(t *testing.T) {
	r := &recorder{name: "r"}
	entered, release := parkFirst(r)
	g := newRig(t, nil, r)

	g.h.Publish(st(Working, "w0"))
	<-entered
	for _, tag := range []string{
		// Fills the queue: no two neighbours share a kind.
		"B1", "W1", "F1", "W2", "B2", "W3", "F2", "W4",
		// Each drops the oldest Working in turn...
		"B3", "F3", "W5", "B4", "F4",
		// ...and with none left, the oldest entry of any kind.
		"B5",
	} {
		g.h.Publish(tagged(tag))
	}
	close(release)

	r.waitCalls(t, 9)
	r.wantTrace(t, "working w0",
		"failed F1", "blocked B2", "failed F2", "blocked B3", "failed F3", "blocked B4", "failed F4", "blocked B5")
}

// TestQueuePushInvariant drives push directly, which is the only way to put
// an Idle in a full queue: the worker appends a fired Idle only while its own
// queue is empty.
func TestQueuePushInvariant(t *testing.T) {
	w := &worker{}
	seq := uint64(0)
	push := func(tags ...string) {
		for _, tag := range tags {
			seq++
			w.push(entry{status: tagged(tag), seq: seq})
		}
	}
	want := func(tags string) {
		t.Helper()
		var got []string
		last := uint64(0)
		for _, e := range w.queue {
			got = append(got, e.status.Message)
			if e.seq <= last {
				t.Fatalf("queue out of seq order: %v", w.queue)
			}
			last = e.seq
		}
		if strings.Join(got, " ") != tags {
			t.Fatalf("queue\n got  %s\n want %s", strings.Join(got, " "), tags)
		}
	}

	push("W1", "B1", "W2", "B2", "I1", "B3", "F1", "B4")
	want("W1 B1 W2 B2 I1 B3 F1 B4")
	push("F2") // drops the oldest Working
	want("B1 W2 B2 I1 B3 F1 B4 F2")
	push("F3") // collapses into the tail; a full queue drops nothing for it
	want("B1 W2 B2 I1 B3 F1 B4 F3")
	push("W3") // dropping W2 leaves B1 B2 adjacent: the newer survives
	want("B2 I1 B3 F1 B4 F3 W3")
	push("W4", "B5")
	want("B2 I1 B3 F1 B4 F3 W4 B5")
	push("W5") // an Idle is dropped like a Working, oldest first
	want("B3 F1 B4 F3 W4 B5 W5")
	push("F4", "B6") // B6 drops W4
	want("B3 F1 B4 F3 B5 W5 F4 B6")
	push("F5", "B7") // F5 drops W5; B7 finds none and drops the oldest, B3
	want("F1 B4 F3 B5 F4 B6 F5 B7")

	// Dropping the tail can make the new entry collapse into the one before.
	w.queue = nil
	push("B1", "F1", "B2", "F2", "B3", "F3", "B4", "W1", "B5")
	want("B1 F1 B2 F2 B3 F3 B5")
}

// B2: seq is strictly increasing, never below the µs clock at publish, and
// the release is above every report.
func TestHubSeq(t *testing.T) {
	r := &recorder{name: "r"}
	g := newRig(t, nil, r)

	type pub struct {
		clock int64
		s     Status
		want  uint64
	}
	pubs := []pub{
		{t0, st(Working, "a"), uint64(t0)},
		{t0, st(Blocked, "b"), uint64(t0) + 1},                // clock stood still: prev+1
		{t0 + 5000, st(Failed, "c"), uint64(t0) + 5000},       // clock moved on: the clock
		{t0 - 1_000_000, st(Working, "d"), uint64(t0) + 5001}, // clock went back: prev+1
		{t0 + 9000, st(Idle, "e"), uint64(t0) + 9000},         // stamped at publish, not at fire
	}
	for i, p := range pubs {
		g.clock.set(p.clock)
		g.h.Publish(p.s)
		if p.s.Kind == Idle {
			g.clock.set(t0 + 50_000)
			g.timers.all()[0].fire()
		}
		r.waitCalls(t, i+1)
	}
	g.clock.set(t0 + 20_000)
	g.h.Close(context.Background())

	calls := r.snapshot()
	if len(calls) != len(pubs)+1 {
		t.Fatalf("calls %v", r.trace())
	}
	for i, p := range pubs {
		c := calls[i]
		if c.seq != p.want {
			t.Errorf("publish %d: seq %d, want %d", i, c.seq, p.want)
		}
		if int64(c.seq) < p.clock {
			t.Errorf("publish %d: seq %d below the clock %d", i, c.seq, p.clock)
		}
		if i > 0 && c.seq <= calls[i-1].seq {
			t.Errorf("publish %d: seq %d not above %d", i, c.seq, calls[i-1].seq)
		}
	}
	rel := calls[len(calls)-1]
	if !rel.release || rel.seq != uint64(t0)+20_000 {
		t.Fatalf("release %+v, want seq %d", rel, t0+20_000)
	}
}

// B1: Publish returns while the worker is parked on a channel the test never
// releases. There is no wall-clock bound: were Publish to wait on the worker,
// this test would not reach its end.
func TestHubPublishNeverBlocks(t *testing.T) {
	r := &recorder{name: "r"}
	entered, release := parkFirst(r)
	g := newRig(t, nil, r)
	t.Cleanup(func() { close(release) }) // runs before the rig's Close

	g.h.Publish(st(Working, "w0"))
	<-entered
	kinds := []Kind{Blocked, Failed, Working, Idle}
	for i := range 1000 {
		g.h.Publish(st(kinds[i%len(kinds)], fmt.Sprint(i)))
	}

	g.h.mu.Lock()
	n := len(g.h.workers[0].queue)
	g.h.mu.Unlock()
	if n > queueCap {
		t.Fatalf("queue grew to %d past its cap %d", n, queueCap)
	}
}

// B3: Close cancels the in-flight send, waits for it, then releases once on a
// fresh context with a seq above everything; a second Close and a later
// Publish do nothing.
func TestHubCloseCancelsInFlightThenReleases(t *testing.T) {
	entered := make(chan struct{})
	var reportErr, releaseErr error
	var releaseDeadline bool
	r := &recorder{name: "r"}
	r.onReport = func(ctx context.Context, _ Status, _ uint64) error {
		close(entered)
		<-ctx.Done()
		reportErr = ctx.Err()
		return ctx.Err()
	}
	r.onRelease = func(ctx context.Context, _ uint64) error {
		releaseErr = ctx.Err()
		_, releaseDeadline = ctx.Deadline()
		return nil
	}
	g := newRig(t, nil, r)

	g.h.Publish(st(Working, "w"))
	<-entered
	g.h.Publish(st(Blocked, "queued")) // stamped, never sent
	g.h.Close(context.Background())

	// No polling: Close has joined the worker, so everything it will ever
	// send has been recorded.
	r.wantTrace(t, "working w", "release")
	if !errors.Is(reportErr, context.Canceled) {
		t.Fatalf("the in-flight Report ended with %v, want context.Canceled", reportErr)
	}
	if releaseErr != nil || !releaseDeadline {
		t.Fatalf("Release needs a fresh bounded context: err %v, deadline %v", releaseErr, releaseDeadline)
	}
	calls := r.snapshot()
	if calls[1].seq <= uint64(t0)+1 {
		t.Fatalf("release seq %d must exceed every seq stamped, t0+1 included", calls[1].seq)
	}
	if w := g.warns.snapshot(); len(w) != 0 {
		t.Fatalf("a send cancelled by Close is not a host failure: %q", w)
	}

	g.h.Close(context.Background())
	g.h.Publish(st(Failed, "late"))
	g.h.Publish(st(Idle, "late"))
	r.wantTrace(t, "working w", "release")
	if n := len(g.timers.all()); n != 0 {
		t.Fatalf("a Publish after Close started %d timers", n)
	}
}

func TestHubCloseSkipsReleaseForReporterThatNeverReported(t *testing.T) {
	r := &recorder{name: "r"}
	g := newRig(t, nil, r)
	g.h.Close(context.Background())
	r.wantTrace(t)
}

// B3: a reporter that ignores ctx cannot hold Close past its budget, whether
// the budget is CloseTimeout or the caller's ctx. Once abandoned, the worker
// still releases when its send finally returns, but never warns again.
func TestHubCloseAbandonsReporterIgnoringCtx(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tweak func(*HubOptions)
		ctx   func() context.Context
	}{
		{"close timeout",
			func(o *HubOptions) { o.CloseTimeout = 20 * time.Millisecond },
			context.Background},
		{"caller ctx",
			func(o *HubOptions) { o.CloseTimeout = time.Hour },
			func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stuck := make(chan struct{})
			entered := make(chan struct{})
			r := &recorder{name: "r"}
			r.onReport = func(context.Context, Status, uint64) error {
				close(entered)
				<-stuck
				return errors.New("late failure")
			}
			r.onRelease = func(context.Context, uint64) error { return errors.New("late release failure") }
			g := newRig(t, tc.tweak, r)

			g.h.Publish(st(Working, "w"))
			<-entered
			start := time.Now()
			g.h.Close(tc.ctx())
			// The budget is 20 ms or an already-ended ctx; 2 s is headroom for
			// a loaded -race run, and still fails a Close that overshoots its
			// budget a hundredfold.
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("Close took %v against a stuck reporter", elapsed)
			}
			r.wantTrace(t, "working w") // Close returned with the send still stuck
			// Idempotent: a second Close returns at once, not after another
			// budget spent on the same stuck worker.
			g.h.Close(context.Background())

			close(stuck)
			<-g.h.workers[0].done
			r.wantTrace(t, "working w", "release")
			if w := g.warns.snapshot(); len(w) != 0 {
				t.Fatalf("an abandoned worker warned after Close returned: %q", w)
			}
		})
	}
}

func TestHubCloseDropsHeldIdle(t *testing.T) {
	r := &recorder{name: "r"}
	g := newRig(t, nil, r)

	g.h.Publish(st(Working, "w"))
	r.waitCalls(t, 1)
	g.h.Publish(st(Idle, "held"))
	timer := g.timers.all()[0]
	g.h.Close(context.Background())
	if !timer.stopped.Load() {
		t.Fatal("Close must stop the held Idle's timer")
	}
	r.wantTrace(t, "working w", "release")

	// The worker has exited, so this is final, not a race.
	timer.fire()
	<-g.h.workers[0].done
	r.wantTrace(t, "working w", "release")

	// A reporter whose only status was a held Idle never reported, so it has
	// nothing to release.
	r2 := &recorder{name: "r2"}
	g2 := newRig(t, nil, r2)
	g2.h.Publish(st(Idle, "held"))
	g2.h.Close(context.Background())
	g2.timers.all()[0].fire()
	r2.wantTrace(t)
}

func TestHubTwoReporters(t *testing.T) {
	a, b := &recorder{name: "a"}, &recorder{name: "b"}
	// Each Release waits until both are inside Release, so a Close that
	// released one reporter after the other would spend its whole budget on
	// the first and return without the second.
	var mu sync.Mutex
	inside := 0
	bothIn := make(chan struct{})
	barrier := func(ctx context.Context, _ uint64) error {
		mu.Lock()
		if inside++; inside == 2 {
			close(bothIn)
		}
		mu.Unlock()
		select {
		case <-bothIn:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a.onRelease, b.onRelease = barrier, barrier
	g := newRig(t, nil, a, b)

	g.h.Publish(st(Working, "w"))
	a.waitCalls(t, 1)
	b.waitCalls(t, 1)
	g.h.Publish(st(Blocked, "b"))
	a.waitCalls(t, 2)
	b.waitCalls(t, 2)
	g.h.Publish(st(Idle, "i"))
	timers := g.timers.all()
	if len(timers) != 2 {
		t.Fatalf("one idle timer per reporter, got %d", len(timers))
	}
	for _, tm := range timers {
		tm.fire()
	}
	a.waitCalls(t, 3)
	b.waitCalls(t, 3)

	g.h.Close(context.Background())
	for _, r := range []*recorder{a, b} {
		r.wantTrace(t, "working w", "blocked b", "idle i", "release")
	}
	if a.snapshot()[3].seq != b.snapshot()[3].seq {
		t.Fatal("both releases come from the one stamp Close takes")
	}
	if w := g.warns.snapshot(); len(w) != 0 {
		t.Fatalf("releases ran one after the other and timed out: %q", w)
	}
}

func TestHubWarnsOncePerReporter(t *testing.T) {
	fail := func(msg string) *recorder {
		r := &recorder{name: msg}
		r.onReport = func(context.Context, Status, uint64) error { return errors.New(msg + " report") }
		r.onRelease = func(context.Context, uint64) error { return errors.New(msg + " release") }
		return r
	}
	a, b := fail("a"), fail("b")
	g := newRig(t, nil, a, b)

	for i, s := range []Status{st(Working, "1"), st(Blocked, "2"), st(Working, "3")} {
		g.h.Publish(s)
		a.waitCalls(t, i+1)
		b.waitCalls(t, i+1)
	}
	g.h.Close(context.Background())

	got := g.warns.snapshot()
	sort.Strings(got)
	want := []string{"host status: a: a report", "host status: b: b report"}
	if !slices.Equal(got, want) {
		t.Fatalf("warns\n got  %q\n want %q", got, want)
	}
}

// Warn is handed to the CLI's plain io.Writer, so the Hub serialises it. The
// Warn here has no lock of its own and yields mid-line, so two unserialised
// calls interleave and, under -race, are a reported data race besides.
//
// Both failures are released at once, but two workers reaching Warn in the
// same instant is luck, so the first call also holds itself open until a
// second one enters. A serialised Hub never lets one in, so the first call
// runs out its short window and the test passes regardless of timing; the
// window decides only how surely a Hub that does not serialise is caught.
func TestHubWarnsDoNotInterleave(t *testing.T) {
	gate := make(chan struct{})
	var entered sync.WaitGroup
	entered.Add(2)
	fail := func(name string) *recorder {
		first := true // only this reporter's worker calls it
		return &recorder{name: name, onReport: func(context.Context, Status, uint64) error {
			if first {
				first = false
				entered.Done()
				<-gate
			}
			return errors.New(strings.Repeat(name, 64))
		}}
	}
	a, b := fail("a"), fail("b")
	var out []byte
	var inside atomic.Int32
	var overlapped atomic.Bool
	secondIn := make(chan struct{}, 1)
	g := newRig(t, func(o *HubOptions) {
		o.Warn = func(s string) {
			if inside.Add(1) > 1 {
				overlapped.Store(true)
				select {
				case secondIn <- struct{}{}:
				default:
				}
			} else {
				select {
				case <-secondIn:
				case <-time.After(20 * time.Millisecond):
				}
			}
			for i := 0; i < len(s); i++ {
				out = append(out, s[i])
				if i%8 == 0 {
					runtime.Gosched()
				}
			}
			out = append(out, '\n')
			inside.Add(-1)
		}
	}, a, b)

	g.h.Publish(st(Working, "w"))
	entered.Wait()
	close(gate) // both fail, and warn, at once
	// A worker records its next call only after the last one, warning
	// included, is done — so this is the sync point, not Close: a send still
	// in flight when Close cancels it is not warned about.
	g.h.Publish(st(Blocked, "b"))
	a.waitCalls(t, 2)
	b.waitCalls(t, 2)
	g.h.Close(context.Background())

	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	sort.Strings(lines)
	want := []string{
		"host status: a: " + strings.Repeat("a", 64),
		"host status: b: " + strings.Repeat("b", 64),
	}
	if overlapped.Load() || !slices.Equal(lines, want) {
		t.Fatalf("Warn calls overlapped (%v):\n%s", overlapped.Load(), out)
	}
}

// A reporter that panics stops being fed and its worker stops reporting, but
// nothing reaches the TUI, the other reporter carries on, and the panicked one
// is still released at Close: its Report may have written state already.
func TestHubPanicStopsOnlyThatReporter(t *testing.T) {
	bad := &recorder{name: "bad", onReport: func(context.Context, Status, uint64) error { panic("kaboom") }}
	good := &recorder{name: "good"}
	g := newRig(t, nil, bad, good)

	g.h.Publish(st(Working, "w"))
	good.waitCalls(t, 1)
	// The reporter is marked dead before the warning, so this is the sync
	// point for everything below.
	waitFor(t, "the panic warning", func() bool { return len(g.warns.snapshot()) == 1 })

	g.h.Publish(st(Blocked, "b"))
	good.waitCalls(t, 2)
	g.h.Publish(st(Idle, "i"))
	if n := len(g.timers.all()); n != 1 {
		t.Fatalf("a dead reporter still got an idle timer: %d timers", n)
	}
	g.timers.all()[0].fire()
	good.waitCalls(t, 3)
	g.h.Close(context.Background())

	good.wantTrace(t, "working w", "blocked b", "idle i", "release")
	bad.wantTrace(t, "working w", "release")
	if w := g.warns.snapshot(); !slices.Equal(w, []string{"host status: bad: panic: kaboom"}) {
		t.Fatalf("warns %q", w)
	}
}

// A panic is a craze bug, so it is warned even though an earlier failure used
// up the reporter's one failure warning; and the release still goes out, once,
// with the seq Close stamped.
func TestHubPanicAfterFailureWarnsAndReleases(t *testing.T) {
	r := &recorder{name: "r"}
	first := true // only the worker goroutine calls Report
	r.onReport = func(context.Context, Status, uint64) error {
		if first {
			first = false
			return errors.New("refused")
		}
		panic("kaboom")
	}
	g := newRig(t, nil, r)

	g.h.Publish(st(Working, "w")) // seq t0: fails
	waitFor(t, "the failure warning", func() bool { return len(g.warns.snapshot()) == 1 })
	g.h.Publish(st(Blocked, "b")) // seq t0+1: panics
	waitFor(t, "the panic warning", func() bool { return len(g.warns.snapshot()) == 2 })
	g.h.Publish(st(Failed, "f")) // seq t0+2: stamped, not sent to a dead reporter
	g.h.Close(context.Background())

	r.wantTrace(t, "working w", "blocked b", "release")
	if rel := r.snapshot()[2]; rel.seq != uint64(t0)+3 {
		t.Fatalf("release seq %d, want the Close stamp t0+3", rel.seq)
	}
	want := []string{"host status: r: refused", "host status: r: panic: kaboom"}
	if w := g.warns.snapshot(); !slices.Equal(w, want) {
		t.Fatalf("warns\n got  %q\n want %q", w, want)
	}
}

func TestHubReleasePanicIsRecovered(t *testing.T) {
	bad := &recorder{name: "bad", onRelease: func(context.Context, uint64) error { panic("release kaboom") }}
	good := &recorder{name: "good"}
	g := newRig(t, nil, bad, good)

	g.h.Publish(st(Working, "w"))
	bad.waitCalls(t, 1)
	good.waitCalls(t, 1)
	g.h.Close(context.Background())

	// Close joined both workers rather than abandoning them.
	for i, w := range g.h.workers {
		select {
		case <-w.done:
		default:
			t.Fatalf("worker %d still running after Close", i)
		}
	}
	bad.wantTrace(t, "working w", "release")
	good.wantTrace(t, "working w", "release")
	if w := g.warns.snapshot(); !slices.Equal(w, []string{"host status: bad: panic: release kaboom"}) {
		t.Fatalf("warns %q", w)
	}
}

// A Close that arrives while another is still releasing waits for it, so no
// Close returns while a release failure could still warn. Only the caller's
// own ctx cuts that wait short.
func TestHubConcurrentCloseWaitsForTheFirst(t *testing.T) {
	releaseIn, proceed := make(chan struct{}), make(chan struct{})
	r := &recorder{name: "r", onRelease: func(context.Context, uint64) error {
		close(releaseIn)
		<-proceed
		return errors.New("release failed")
	}}
	var returned atomic.Int32 // Close calls A and B that have returned
	var late atomic.Bool
	var warns warnLog
	g := newRig(t, func(o *HubOptions) {
		o.Warn = func(s string) {
			if returned.Load() > 0 {
				late.Store(true)
			}
			warns.warn(s)
		}
	}, r)

	g.h.Publish(st(Working, "w"))
	r.waitCalls(t, 1)

	closeInBackground := func() chan struct{} {
		done := make(chan struct{})
		go func() {
			g.h.Close(context.Background())
			returned.Add(1)
			close(done)
		}()
		return done
	}
	doneA := closeInBackground()
	<-releaseIn // A is inside Close, its worker parked in Release
	doneB := closeInBackground()

	// A Close whose own ctx has ended does not wait on A. It is not counted
	// in returned: returning early is exactly what its ctx asked for.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	g.h.Close(ended)

	// Give B the chance to return early before the release fails. A correct
	// Hub holds B until A is done, so it runs out this window and passes
	// whatever the timing; the window decides only how surely a Hub that lets
	// B return early is caught.
	select {
	case <-doneB:
	case <-time.After(50 * time.Millisecond):
	}
	close(proceed)
	<-doneA
	<-doneB

	if late.Load() {
		t.Fatal("a release warned after a Close had returned")
	}
	if w := warns.snapshot(); !slices.Equal(w, []string{"host status: r: release failed"}) {
		t.Fatalf("warns %q", w)
	}
}
