package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/charliek/craze/internal/journal"
)

// logWatchdog is these tests' wall-clock bound, like the package's await and
// waitDone: a generous limit on a wait a correct log satisfies at once, so a
// deadlock fails the test instead of hanging the run. Nothing here sleeps to
// let something happen; the tests know where a goroutine is from a hook or a
// channel it closed.
const logWatchdog = 10 * time.Second

// The functions whose goroutines the leak checks count, as runtime.Stack
// prints their frames.
const (
	ownerFrame  = "github.com/charliek/craze/internal/agent.(*Subscription).run("
	writerFrame = "github.com/charliek/craze/internal/journal.(*Writer).run("
)

var logTestTime = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

func textEvent(text string) Event { return Event{Type: EventText, Text: text, At: logTestTime} }

// within runs fn on a goroutine of its own and fails the test if it has not
// returned within the watchdog: the way a test says "this returns at once"
// without a clock deciding what "at once" is.
func within(t testing.TB, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(logWatchdog):
		t.Fatalf("%s: still blocked after %v", what, logWatchdog)
	}
}

// mustSubscribe is l.Subscribe(o), failing the test on an error.
func mustSubscribe(t *testing.T, l *EventLog, o SubscribeOptions) *Subscription {
	t.Helper()
	s, err := l.Subscribe(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// readN reads exactly n records, failing if the subscription ends first.
func readN(t *testing.T, s *Subscription, n int) []Record {
	t.Helper()
	recs := make([]Record, 0, n)
	deadline := time.After(logWatchdog)
	for len(recs) < n {
		select {
		case r, ok := <-s.Records():
			if !ok {
				t.Fatalf("the subscription ended (%v) after %d of %d records", s.Err(), len(recs), n)
			}
			recs = append(recs, r)
		case <-deadline:
			t.Fatalf("timed out after %d of %d records", len(recs), n)
		}
	}
	return recs
}

// readAll reads until Records closes.
func readAll(t *testing.T, s *Subscription) []Record {
	t.Helper()
	var recs []Record
	deadline := time.After(logWatchdog)
	for {
		select {
		case r, ok := <-s.Records():
			if !ok {
				return recs
			}
			recs = append(recs, r)
		case <-deadline:
			t.Fatalf("the subscription was still open after %v and %d records", logWatchdog, len(recs))
		}
	}
}

// drainPrimary empties the primary without waiting: what is there now.
func drainPrimary(l *EventLog) []Event {
	var evs []Event
	for {
		select {
		case ev := <-l.Primary():
			evs = append(evs, ev)
		default:
			return evs
		}
	}
}

// runSeqs is the first problem with seqs being exactly from, from+1, …, or "".
func runSeqs(seqs []uint64, from uint64) string {
	for i, s := range seqs {
		if want := from + uint64(i); s != want {
			return fmt.Sprintf("position %d is seq %d, want %d (a hole, a duplicate, or out of order)", i, s, want)
		}
	}
	return ""
}

// assertRun fails unless recs are seqs from, from+1, … and n of them (n < 0:
// any number).
func assertRun(t *testing.T, what string, recs []Record, from uint64, n int) {
	t.Helper()
	seqs := make([]uint64, len(recs))
	for i, r := range recs {
		seqs[i] = r.Seq
	}
	if p := runSeqs(seqs, from); p != "" {
		t.Fatalf("%s: %s", what, p)
	}
	if n >= 0 && len(recs) != n {
		t.Fatalf("%s: %d records, want %d", what, len(recs), n)
	}
}

// recordTexts decodes each record's Text.
func recordTexts(t *testing.T, recs []Record) []string {
	t.Helper()
	out := make([]string, len(recs))
	for i, r := range recs {
		ev, err := r.Event()
		if err != nil {
			t.Fatalf("seq %d: %v", r.Seq, err)
		}
		out[i] = ev.Text
	}
	return out
}

func assertNoText(t *testing.T, what string, got []string, bad ...string) {
	t.Helper()
	for _, g := range got {
		for _, b := range bad {
			if g == b {
				t.Fatalf("%s: %q reached it", what, b)
			}
		}
	}
}

// fillPrimary publishes until the primary's buffer is full: primaryCap
// events, seqs 1 to 256, with nobody reading.
func fillPrimary(t *testing.T, l *EventLog) {
	t.Helper()
	for i := range primaryCap {
		if !l.Publish(context.Background(), nil, textEvent(fmt.Sprintf("fill %d", i+1))) {
			t.Fatalf("filling the primary: publish %d returned false", i+1)
		}
	}
}

// takeStartDelta takes the one settings delta a session's Start enqueues — the
// install: everything session/new (or the native harness) brought, said in the
// stream instead of only in Snapshot() (live.go's installDeltaLocked, r23
// finding 2) — off the primary, so a test that goes on to fill the primary
// starts from an empty buffer.
//
// The flush is the test's own, and has to be: Start does NOT flush the install,
// because it may not wait on a reader that a caller is allowed not to have
// started yet (Session.Start, r25 finding 1). Here the primary is empty and
// this goroutine is its reader, so the drainer has room and the barrier returns
// at once.
func takeStartDelta(t *testing.T, l *EventLog) Event {
	t.Helper()
	_ = l.Flush(context.Background(), nil)
	select {
	case ev := <-l.Primary():
		if ev.Type != EventMeta || ev.State == nil {
			t.Fatalf("the first event a started session published is %s, want the install delta", ev.Type)
		}
		return ev
	default:
		t.Fatal("Start published no install delta")
		panic("unreachable")
	}
}

// publishWithin publishes one event that must go through, failing the test
// rather than hanging it if the primary has no room.
func publishWithin(t *testing.T, l *EventLog, ev Event) {
	t.Helper()
	ok := false
	within(t, fmt.Sprintf("publishing %q", ev.Text), func() { ok = l.Publish(context.Background(), nil, ev) })
	if !ok {
		t.Fatalf("publishing %q returned false", ev.Text)
	}
}

// collectPrimary reads the next n events off the primary on a goroutine and
// hands them over, in order, on the returned channel.
func collectPrimary(l *EventLog, n int) <-chan []Event {
	out := make(chan []Event, 1)
	go func() {
		evs := make([]Event, 0, n)
		for range n {
			evs = append(evs, <-l.Primary())
		}
		out <- evs
	}()
	return out
}

// keepDrained reads the primary on a goroutine until the test ends, as a
// session's consumer would, for tests about something else.
func keepDrained(t testing.TB, l *EventLog) {
	t.Helper()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-l.Primary():
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
}

// insideAt is a beforePrimarySend hook that closes the returned channel when
// a publisher is inside the boundary about to block with seq.
func insideAt(seq uint64) (func(uint64), <-chan struct{}) {
	inside := make(chan struct{})
	var once sync.Once
	return func(s uint64) {
		if s == seq {
			once.Do(func() { close(inside) })
		}
	}, inside
}

// newTestJournal is a journal in a fresh directory, built with a fresh
// incarnation, as C5a's wiring will build one.
func newTestJournal(t testing.TB) (*journal.Writer, string) {
	t.Helper()
	return newTestJournalWith(t, nil)
}

// newTestJournalWith is newTestJournal with its options edited first, when
// edit is not nil.
func newTestJournalWith(t testing.TB, edit func(*journal.Options)) (*journal.Writer, string) {
	t.Helper()
	inc := NewIncarnation()
	o := journal.Options{
		Dir:         filepath.Join(t.TempDir(), "journal"),
		Incarnation: inc,
		Cwd:         t.TempDir(),
		Provider:    "test",
		EventCodec:  EventCodecVersion,
	}
	if edit != nil {
		edit(&o)
	}
	w, err := journal.New(o)
	if err != nil {
		t.Fatal(err)
	}
	// Close bounds its wait at 500 ms; the cleanup waits for the writer to
	// finish as well, so no test leaves a writer running into the next one
	// (the leak checks count them).
	t.Cleanup(func() {
		_ = w.Close(context.Background())
		_ = writerExited(w)
	})
	return w, inc
}

// writerExited waits, up to the watchdog, for w's writer goroutine to finish
// after its Close. WaitFlushed for a seq never reached returns ErrClosed
// exactly when the writer has finished.
func writerExited(w *journal.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), logWatchdog)
	defer cancel()
	return w.WaitFlushed(ctx, journal.MaxSeq)
}

// newJournaledLog is an event log feeding a fresh journal, closed when the
// test ends.
func newJournaledLog(t testing.TB, o EventLogOptions) (*EventLog, *journal.Writer) {
	t.Helper()
	w, inc := newTestJournal(t)
	o.Journal, o.Incarnation = w, inc
	return newTestLog(t, o), w
}

// newTestLog is an event log with o's journal, if any, closed when the test
// ends.
func newTestLog(t testing.TB, o EventLogOptions) *EventLog {
	t.Helper()
	l := NewEventLog(o)
	t.Cleanup(func() { l.Close(context.Background()) })
	return l
}

// closeLog closes l and, when it has a journal, waits for the journal's
// writer goroutine to finish: l.Close bounds its own wait at 500 ms, and a
// slow -race run must not make a test read a file the writer is still
// writing.
func closeLog(t testing.TB, l *EventLog, w *journal.Writer) {
	t.Helper()
	within(t, "EventLog.Close", func() { l.Close(context.Background()) })
	if w == nil {
		return
	}
	if err := writerExited(w); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("waiting for the journal writer to finish: %v", err)
	}
}

// flushThrough waits for the journal to have written every line through seq,
// so a resume's head is served from the file with no wait at all. It is the
// journal's own handshake — WaitFlushed asks for the flush itself — and not a
// sleep waiting for the writer's 250 ms timer.
func flushThrough(t *testing.T, w *journal.Writer, seq uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), logWatchdog)
	defer cancel()
	if err := w.WaitFlushed(ctx, seq); err != nil {
		t.Fatalf("waiting for the journal to reach seq %d: %v", seq, err)
	}
}

// heldPublisher publishes a run of events on a goroutine of its own and holds
// at one of them until it is released: the way these tests put a cutoff inside
// a run of publishes rather than after one. Event n carries the text n, so
// every record says which seq it should have.
type heldPublisher struct {
	release func() // let it run to the end; idempotent
	wg      sync.WaitGroup
}

// publishHeldAt starts a publisher of total events and returns once it has
// published hold of them and stopped there. It is released by the returned
// value's release, and by the test's end whatever happens, so a failure never
// leaves a publisher holding the log.
func publishHeldAt(t *testing.T, l *EventLog, total, hold int) *heldPublisher {
	t.Helper()
	reached, resume := make(chan struct{}), make(chan struct{})
	p := &heldPublisher{release: sync.OnceFunc(func() { close(resume) })}
	t.Cleanup(func() {
		p.release()
		waitDone(t, &p.wg)
	})
	p.wg.Go(func() {
		for i := range total {
			if !l.Publish(context.Background(), nil, textEvent(fmt.Sprint(i+1))) {
				return // the log closed under it: the test is ending.
			}
			if i+1 == hold {
				close(reached)
				<-resume
			}
		}
	})
	await(t, reached, "the publisher to reach the barrier")
	return p
}

// heldJournal stands in front of a real journal writer, so a test can count
// what the file leg of a resume asks of it and, when it is held, keep the
// file behind: the boundary reads as empty, ReadRange says ErrBehind and
// WaitFlushed waits, until the test lets it go and every call is the real
// writer's again. What a released resume finally reads is a real journal file.
//
// It exists because the journal's own stall seams are unexported, its tests'
// alone, and because letting the writer's 250 ms flush timer decide when a
// writer catches up would make these tests a race with a clock.
type heldJournal struct {
	journalFile // the real writer
	release     chan struct{}
	releaseOnce sync.Once
	waiting     chan struct{} // closed when a wait begins
	waitingOnce sync.Once
	reads       atomic.Int64
	waits       atomic.Int64
}

// holdJournal puts a heldJournal in front of l's journal, held back when hold
// is set and counting only otherwise. The caller must call it before anything
// subscribes, which is the only time the field is read.
func holdJournal(t *testing.T, l *EventLog, hold bool) *heldJournal {
	t.Helper()
	j := &heldJournal{journalFile: l.file, release: make(chan struct{}), waiting: make(chan struct{})}
	if !hold {
		j.letGo()
	}
	l.file = j
	// Before the log's own Close, which waits for every owner: a failing test
	// must not leave one waiting out the bound.
	t.Cleanup(j.letGo)
	return j
}

func (j *heldJournal) letGo() { j.releaseOnce.Do(func() { close(j.release) }) }

func (j *heldJournal) held() bool {
	select {
	case <-j.release:
		return false
	default:
		return true
	}
}

func (j *heldJournal) Flushed() (uint64, int64) {
	if j.held() {
		return 0, 0
	}
	return j.journalFile.Flushed()
}

func (j *heldJournal) ReadRange(from, to uint64, fn func(journal.Record) error) error {
	j.reads.Add(1)
	if j.held() {
		return fmt.Errorf("%w: the file is held at seq 0, the range ends at %d", journal.ErrBehind, to)
	}
	return j.journalFile.ReadRange(from, to, fn)
}

func (j *heldJournal) WaitFlushed(ctx context.Context, seq uint64) error {
	j.waits.Add(1)
	j.waitingOnce.Do(func() { close(j.waiting) })
	select {
	case <-j.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return j.journalFile.WaitFlushed(ctx, seq)
}

// shortJournalWait shortens the file leg's wait for a stalled writer for one
// test, so a bound that must run out runs out at once instead of being waited
// through.
func shortJournalWait(t *testing.T, d time.Duration) {
	t.Helper()
	was := journalWaitBound
	journalWaitBound = d
	t.Cleanup(func() { journalWaitBound = was })
}

// assertUnresolvable fails unless err is an ErrCursorUnresolvable with reason,
// and returns it.
func assertUnresolvable(t *testing.T, what string, err error, reason CursorReason) ErrCursorUnresolvable {
	t.Helper()
	var cu ErrCursorUnresolvable
	if !errors.As(err, &cu) || cu.Reason != reason {
		t.Fatalf("%s: %v; want ErrCursorUnresolvable{%s}", what, err, reason)
	}
	return cu
}

// fileRecords is every event record in the journal's file.
func fileRecords(t *testing.T, w *journal.Writer) []Record {
	t.Helper()
	var recs []Record
	err := journal.ReadFile(w.Path(), 1, journal.MaxSeq, func(r journal.Record) error {
		recs = append(recs, recordFromJournal(r))
		return nil
	})
	if err != nil {
		t.Fatalf("reading the journal: %v", err)
	}
	return recs
}

// fileLines is every line of the journal's file, notes included, as JSON.
func fileLines(t *testing.T, w *journal.Writer) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatalf("reading the journal: %v", err)
	}
	var lines []map[string]any
	for _, line := range bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("a journal line is not JSON: %v: %s", err, line)
		}
		lines = append(lines, m)
	}
	return lines
}

// diags is the file's diag lines of one kind, their fields.
func diags(lines []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["type"] == "diag" && l["kind"] == kind {
			out = append(out, l["fields"].(map[string]any))
		}
	}
	return out
}

// assertClosingIsLast fails unless the file's last line is the closing diag
// and no other closing precedes it: no note is ever written after closing.
func assertClosingIsLast(t *testing.T, lines []map[string]any) map[string]any {
	t.Helper()
	if len(lines) == 0 {
		t.Fatal("the journal is empty")
	}
	last := lines[len(lines)-1]
	if last["type"] != "diag" || last["kind"] != journal.DiagClosing {
		t.Fatalf("the journal's last line is %v, want the closing diag", last)
	}
	if n := len(diags(lines, journal.DiagClosing)); n != 1 {
		t.Fatalf("%d closing diags, want 1", n)
	}
	return last["fields"].(map[string]any)
}

// subscriberCount is how many subscriptions are registered and have not
// ended, read inside the boundary.
func subscriberCount(t *testing.T, l *EventLog) int {
	t.Helper()
	n := -1
	within(t, "counting subscribers", func() {
		l.sem <- struct{}{}
		defer l.release()
		n = 0
		for _, s := range l.subs {
			if s.terminal() == nil {
				n++
			}
		}
	})
	return n
}

// lastSeq is the last seq committed, read inside the boundary.
func lastSeq(t *testing.T, l *EventLog) uint64 {
	t.Helper()
	var n uint64
	within(t, "reading the last seq", func() {
		l.sem <- struct{}{}
		defer l.release()
		n = l.next
	})
	return n
}

// assertNoneOmitted fails if any of recs is an omitted record: in a test
// whose only event the codec refuses is the one that must reach nobody.
func assertNoneOmitted(t *testing.T, what string, recs []Record) {
	t.Helper()
	for _, r := range recs {
		if r.Omitted != nil {
			t.Fatalf("%s: seq %d is an omitted record (%s): the abandoned event reached it", what, r.Seq, r.Omitted.Reason)
		}
	}
}

// goroutinesRunning counts the goroutines with frame on their stack.
func goroutinesRunning(frame string) int {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return bytes.Count(buf[:n], []byte(frame))
		}
		buf = make([]byte, 2*len(buf))
	}
}

// settleGoroutines waits until at most want goroutines run frame. A
// goroutine that has signalled its end (a closed channel, a Done) still has
// a few instructions to run before the runtime stops listing it; the wait is
// bounded by the watchdog, so a real leak fails.
func settleGoroutines(t *testing.T, frame string, want int) {
	t.Helper()
	deadline := time.Now().Add(logWatchdog)
	for {
		got := goroutinesRunning(frame)
		if got <= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines still run %s, want at most %d", got, frame, want)
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

// TestEventLogIncarnationIsAStableUUIDv7: the id NewEventLog mints is a
// UUIDv7, the same on every call, different per log, and a name the journal
// accepts in a file name. A given one is kept.
func TestEventLogIncarnationIsAStableUUIDv7(t *testing.T) {
	l := newTestLog(t, EventLogOptions{})
	id := l.Incarnation()
	u, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("incarnation %q is not a UUID: %v", id, err)
	}
	if v := u[6] >> 4; v != 7 {
		t.Fatalf("incarnation %q is a version %d UUID, want 7", id, v)
	}
	if l.Incarnation() != id {
		t.Fatal("the incarnation changed between calls")
	}
	if other := newTestLog(t, EventLogOptions{}).Incarnation(); other == id {
		t.Fatalf("two logs share the incarnation %q", id)
	}
	if got := newTestLog(t, EventLogOptions{Incarnation: "given"}).Incarnation(); got != "given" {
		t.Fatalf("a given incarnation came back as %q", got)
	}
	if _, err := journal.New(journal.Options{Dir: t.TempDir(), Incarnation: id, Cwd: t.TempDir()}); err != nil {
		t.Fatalf("the journal refuses the incarnation: %v", err)
	}
}

// TestRecordEventDecodesTheBodyAndSetsSeq: a record replays as the event that
// was published, under the codec's equivalence, with Seq restored from the
// envelope — the same Seq the primary saw.
func TestRecordEventDecodesTheBodyAndSetsSeq(t *testing.T) {
	l := newTestLog(t, EventLogOptions{})
	l.Publish(context.Background(), nil, textEvent("first"))
	ev := Event{Type: EventTool, At: codecTestTime, Tool: &ToolEvent{
		ID: "call-1", Status: "completed", Output: &ToolOutput{ExitCode: ptr(0), Stdout: "ok"}, Task: &TaskInfo{},
	}}
	if !l.Publish(context.Background(), nil, ev) {
		t.Fatal("Publish returned false")
	}
	prim := drainPrimary(l)
	if len(prim) != 2 || prim[1].Seq != 2 {
		t.Fatalf("the primary has %+v", prim)
	}
	s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation(), Seq: 1}})
	rec := readN(t, s, 1)[0]
	got, err := rec.Event()
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 2 || rec.Seq != 2 {
		t.Fatalf("the replayed event has Seq %d (record %d), want 2", got.Seq, rec.Seq)
	}
	// Seq is the envelope's, checked above; the codec's comparison holds the
	// body to carrying none.
	got.Seq = 0
	if d := codecDiff(ev, got); len(d) > 0 {
		t.Fatalf("the replayed event differs:\n  %s", strings.Join(d, "\n  "))
	}
	if rec.Type != EventTool || !rec.At.Equal(codecTestTime) {
		t.Fatalf("the envelope is %v at %v", rec.Type, rec.At)
	}
	if _, err := (Record{Seq: 3, Type: EventPlan, Omitted: &Omitted{Reason: journal.OmittedOversized}}).Event(); err == nil {
		t.Fatal("an omitted record decoded without an error")
	}
}

// TestEventLogConcurrentPublishersGetOneGaplessSequenceEverywhere is A4: with
// G goroutines publishing at once, the primary receives strictly increasing
// Seq from 1 with no holes, every subscriber that was not dropped receives
// the identical sequence (the same bodies at the same numbers), a subscriber
// that was dropped received a gapless prefix of it, and the journal holds it
// too.
func TestEventLogConcurrentPublishersGetOneGaplessSequenceEverywhere(t *testing.T) {
	const publishers, each = 8, 250
	const total = publishers * each
	l, w := newJournaledLog(t, EventLogOptions{})

	full := make([]*Subscription, 3)
	for i := range full {
		s := mustSubscribe(t, l, SubscribeOptions{MaxItems: total})
		full[i] = s
	}
	tiny := mustSubscribe(t, l, SubscribeOptions{MaxItems: 1})

	got := make([][]Record, len(full))
	reached := make([]chan struct{}, len(full))
	var readers sync.WaitGroup
	for i, s := range full {
		reached[i] = make(chan struct{})
		readers.Go(func() {
			for r := range s.Records() {
				got[i] = append(got[i], r)
				if r.Seq == total {
					close(reached[i])
				}
			}
		})
	}
	var tinyGot []Record
	readers.Go(func() {
		for r := range tiny.Records() {
			tinyGot = append(tinyGot, r)
		}
	})
	primary := collectPrimary(l, total)

	var failed atomic.Int64
	var pubs sync.WaitGroup
	for p := range publishers {
		pubs.Go(func() {
			for i := range each {
				if !l.Publish(context.Background(), nil, textEvent(fmt.Sprintf("p%d-%d", p, i))) {
					failed.Add(1)
				}
			}
		})
	}
	waitDone(t, &pubs)
	if n := failed.Load(); n != 0 {
		t.Fatalf("%d publishes returned false on an open log", n)
	}
	evs := await(t, primary, "the primary's events")
	for i := range full {
		await(t, reached[i], fmt.Sprintf("subscriber %d to reach seq %d", i, total))
	}
	closeLog(t, l, w)
	waitDone(t, &readers)

	primSeqs := make([]uint64, len(evs))
	byseq := make(map[uint64]string, total)
	lastIndex := map[int]int{}
	for i, ev := range evs {
		primSeqs[i] = ev.Seq
		byseq[ev.Seq] = ev.Text
		// Each publisher's own events keep its order.
		var p, n int
		if _, err := fmt.Sscanf(ev.Text, "p%d-%d", &p, &n); err != nil {
			t.Fatalf("event %q: %v", ev.Text, err)
		}
		if prev, ok := lastIndex[p]; ok && n <= prev {
			t.Fatalf("publisher %d's event %d reached the primary after its event %d", p, n, prev)
		}
		lastIndex[p] = n
	}
	if p := runSeqs(primSeqs, 1); p != "" {
		t.Fatalf("the primary: %s", p)
	}
	check := func(what string, recs []Record, n int) {
		t.Helper()
		assertRun(t, what, recs, 1, n)
		for i, text := range recordTexts(t, recs) {
			if text != byseq[recs[i].Seq] {
				t.Fatalf("%s: seq %d is %q, the primary had %q", what, recs[i].Seq, text, byseq[recs[i].Seq])
			}
		}
	}
	for i, s := range full {
		if !errors.Is(s.Err(), ErrClosed) {
			t.Fatalf("subscriber %d ended with %v, want ErrClosed: it was not to be dropped", i, s.Err())
		}
		check(fmt.Sprintf("subscriber %d", i), got[i], total)
	}
	if err := tiny.Err(); !errors.Is(err, ErrSlowConsumer) && !errors.Is(err, ErrClosed) {
		t.Fatalf("the tiny subscriber ended with %v", err)
	}
	check("the tiny subscriber's prefix", tinyGot, -1)
	check("the journal", fileRecords(t, w), total)
}

// TestEventLogWhatAReturnedPublisherEmittedIsAlreadyInThePrimary is A5,
// "emitted means buffered": up to 256 events published from a goroutine that
// has returned are all found by a non-blocking drain of the primary — with a
// journal attached and a subscriber that never reads, neither of which may
// put anything between Publish and the primary.
func TestEventLogWhatAReturnedPublisherEmittedIsAlreadyInThePrimary(t *testing.T) {
	l, _ := newJournaledLog(t, EventLogOptions{})
	mustSubscribe(t, l, SubscribeOptions{MaxItems: 8})
	var failed atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range primaryCap {
			if !l.Publish(context.Background(), nil, textEvent(fmt.Sprint(i+1))) {
				failed.Store(true)
			}
		}
	})
	waitDone(t, &wg)
	if failed.Load() {
		t.Fatal("a publish returned false")
	}
	evs := drainPrimary(l)
	if len(evs) != primaryCap {
		t.Fatalf("a non-blocking drain found %d events, want %d", len(evs), primaryCap)
	}
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) || ev.Text != fmt.Sprint(i+1) {
			t.Fatalf("position %d is seq %d %q", i, ev.Seq, ev.Text)
		}
	}
}

// TestEventLogAWaitingPublisherHonorsItsCtxWhileAnotherIsBlockedInside is A6,
// cancellable admission: with the primary full and publisher A blocked on it
// inside the boundary, publisher B, waiting to be admitted, returns false
// when its ctx is cancelled — while A is still blocked. A mutex boundary
// would hold B until A got out. B consumes no number.
func TestEventLogAWaitingPublisherHonorsItsCtxWhileAnotherIsBlockedInside(t *testing.T) {
	l := newTestLog(t, EventLogOptions{})
	fillPrimary(t, l)

	hook, aInside := insideAt(primaryCap + 1)
	var arrivals atomic.Int64
	bWaiting := make(chan struct{})
	l.hooks = &logHooks{
		beforePrimarySend: hook,
		admitting: func(admitKind) {
			if arrivals.Add(1) == 2 {
				close(bWaiting)
			}
		},
	}
	aResult := make(chan bool, 1)
	go func() { aResult <- l.Publish(context.Background(), nil, textEvent("A")) }()
	await(t, aInside, "A to block on the full primary inside the boundary")

	ctx, cancel := context.WithCancel(context.Background())
	bResult := make(chan bool, 1)
	go func() { bResult <- l.Publish(ctx, nil, textEvent("B")) }()
	await(t, bWaiting, "B to wait for admission")
	cancel()
	if await(t, bResult, "B to give up on its cancelled ctx") {
		t.Fatal("B returned true")
	}
	select {
	case <-aResult:
		t.Fatal("A returned while the primary was still full")
	default:
	}

	first := await(t, l.Primary(), "the primary's first event")
	if first.Seq != 1 {
		t.Fatalf("the primary's first event is seq %d", first.Seq)
	}
	if !await(t, aResult, "A, once the primary had room") {
		t.Fatal("A returned false")
	}
	// A filled the slot it waited for; make room for C.
	evs := drainPrimary(l)
	publishWithin(t, l, textEvent("C"))
	evs = append(evs, drainPrimary(l)...)
	last := evs[len(evs)-2:]
	if last[0].Text != "A" || last[0].Seq != 257 || last[1].Text != "C" || last[1].Seq != 258 {
		t.Fatalf("the primary ends %+v, want A at 257 and C at 258: B took no number", last)
	}
	if n := l.Health().DroppedAtClose; n != 0 {
		t.Fatalf("DroppedAtClose is %d: a ctx escape is not a drop at close", n)
	}
}

// TestEventLogASubscriberThatNeverReadsIsDroppedAndHoldsNobodyBack is A7: a
// subscriber that never reads ends with ErrSlowConsumer; the publisher is
// never held by it, and the primary and the other subscriber lose nothing.
// The drop is recorded once, in the health and in the journal.
func TestEventLogASubscriberThatNeverReadsIsDroppedAndHoldsNobodyBack(t *testing.T) {
	const total = 200
	l, w := newJournaledLog(t, EventLogOptions{})
	stuck := mustSubscribe(t, l, SubscribeOptions{MaxItems: 8})
	reader := mustSubscribe(t, l, SubscribeOptions{MaxItems: total})
	readerGot := make(chan []Record, 1)
	go func() {
		var recs []Record
		for r := range reader.Records() {
			if recs = append(recs, r); r.Seq == total {
				break
			}
		}
		readerGot <- recs
	}()
	primary := collectPrimary(l, total)

	within(t, "publishing past a subscriber that never reads", func() {
		for i := range total {
			if !l.Publish(context.Background(), nil, textEvent(fmt.Sprint(i+1))) {
				t.Errorf("publish %d returned false", i+1)
			}
		}
	})
	evs := await(t, primary, "the primary's events")
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("the primary's position %d is seq %d", i, ev.Seq)
		}
	}
	assertRun(t, "the reading subscriber", await(t, readerGot, "the reading subscriber"), 1, total)

	// The owner closes the stuck subscription on its own: reading it now
	// finds the channel closed, after at most the one record the owner held
	// when the overflow came.
	stuckGot := readAll(t, stuck)
	if !errors.Is(stuck.Err(), ErrSlowConsumer) {
		t.Fatalf("the stuck subscriber ended with %v, want ErrSlowConsumer", stuck.Err())
	}
	if len(stuckGot) > 1 {
		t.Fatalf("the stuck subscriber was delivered %d records after its drop", len(stuckGot))
	}
	assertRun(t, "the stuck subscriber", stuckGot, 1, -1)
	if n := l.Health().SubscribersDropped; n != 1 {
		t.Fatalf("SubscribersDropped is %d, want 1", n)
	}
	closeLog(t, l, w)
	// Its 8-item buffer overflows at seq 9, or at 10 if its owner had begun
	// sending seq 1 by then: a record being sent no longer counts.
	dropped := diags(fileLines(t, w), journal.DiagSubscriberDropped)
	if len(dropped) != 1 || dropped[0]["seq"] != float64(9) && dropped[0]["seq"] != float64(10) || dropped[0]["subscribers"] != float64(1) {
		t.Fatalf("the journal's subscriber_dropped notes are %v, want one at seq 9 or 10 (its 8-item buffer full)", dropped)
	}
}

// TestEventLogAnAbandonedPublishConsumesNoSeqAndReachesNothing is A8: a
// Publish abandoned — on done or ctx while blocked on the primary, on done
// while waiting for admission, or by the log closing — takes no number and
// reaches no subscriber, the ring, or the journal, and leaves no orphan note.
// Escapes on done and the cutoff are counted for the closing diag; a ctx
// escape is not.
//
// The abandoned event is one the codec refuses, so committing it would owe a
// record_omitted note, and a tiny subscriber is open that it would overflow,
// which would owe a subscriber_dropped note: a note queued for it anywhere
// but after the commit is caught, not merely absent because nothing asked
// for one.
func TestEventLogAnAbandonedPublishConsumesNoSeqAndReachesNothing(t *testing.T) {
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	abandonedEvent := Event{Type: EventTool, At: logTestTime, Tool: &ToolEvent{ID: "abandoned", At: far}}
	if r, _ := (&EventLog{maxRecord: defaultMaxRecordBytes}).record(abandonedEvent); r.Omitted == nil || r.size() <= 1 {
		t.Fatalf("the abandoned event's record is %+v: it must be omitted and weigh more than the tiny subscriber's budget", r)
	}
	type abandon struct {
		name      string
		atAdmit   bool // abandoned while waiting for admission, behind a blocked publisher
		byClose   bool // abandoned by the log closing
		byCtx     bool // abandoned by ctx rather than done
		wantDrops int
	}
	for _, c := range []abandon{
		{name: "done while blocked on the primary", wantDrops: 1},
		{name: "ctx while blocked on the primary", byCtx: true},
		{name: "done while waiting for admission", atAdmit: true, wantDrops: 1},
		{name: "ctx while waiting for admission", atAdmit: true, byCtx: true},
		{name: "the log closing", byClose: true, wantDrops: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, w := newJournaledLog(t, EventLogOptions{})
			// The watcher is read by the test itself, up to the last seq
			// committed, before the log closes: Close ends a subscription at
			// once and discards what it had not delivered yet, so a watcher
			// on its own goroutine could lose its tail to a slow scheduler
			// and prove nothing.
			watch := mustSubscribe(t, l, SubscribeOptions{MaxItems: 4096})
			fillPrimary(t, l)
			watchedRecs := readN(t, watch, primaryCap)
			// One byte of budget: the abandoned event, committed, would
			// overflow it. It is closed before anything else is committed.
			tiny := mustSubscribe(t, l, SubscribeOptions{MaxItems: 1, MaxBytes: 1})

			hook, blocked := insideAt(primaryCap + 1)
			// Every later Publish and Subscribe arrives here too; the buffer
			// is far larger than the handful this test makes, so the hook
			// never blocks one of them.
			arrived := make(chan struct{}, 64)
			l.hooks = &logHooks{beforePrimarySend: hook, admitting: func(admitKind) { arrived <- struct{}{} }}
			done := make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			abandon := func() {
				if c.byCtx {
					cancel()
				} else {
					close(done)
				}
			}

			var blocker chan bool
			if c.atAdmit {
				// A publisher that will not be abandoned holds the boundary
				// on the full primary; the abandoned one waits behind it.
				blocker = make(chan bool, 1)
				go func() { blocker <- l.Publish(context.Background(), nil, textEvent("blocker")) }()
				await(t, arrived, "the blocker's admission")
				await(t, blocked, "the blocker to block inside the boundary")
			}
			result := make(chan bool, 1)
			go func() { result <- l.Publish(ctx, done, abandonedEvent) }()
			await(t, arrived, "the abandoned publisher's admission")
			if !c.atAdmit {
				await(t, blocked, "the publisher to block inside the boundary")
			}
			closed := make(chan struct{})
			if c.byClose {
				go func() {
					defer close(closed)
					l.Close(context.Background())
				}()
			} else {
				abandon()
			}
			if await(t, result, "the abandoned publish to return") {
				t.Fatal("the abandoned publish returned true")
			}
			if h := l.Health(); h.Omitted != 0 || h.SubscribersDropped != 0 {
				t.Fatalf("health after the abandoned publish is %+v: it counted an omission or a drop", h)
			}
			if err := tiny.terminal(); errors.Is(err, ErrSlowConsumer) {
				t.Fatal("the abandoned event overflowed the tiny subscriber")
			}
			tiny.Close()

			want := uint64(primaryCap)
			if c.byClose {
				await(t, closed, "Close")
			} else {
				await(t, l.Primary(), "one event off the full primary")
				if c.atAdmit {
					if !await(t, blocker, "the blocker") {
						t.Fatal("the blocker returned false")
					}
					want++
				}
				// The blocker, if any, filled the slot it waited for; make
				// room for the publish after.
				evs := drainPrimary(l)
				publishWithin(t, l, textEvent("after"))
				want++
				evs = append(evs, drainPrimary(l)...)
				if last := evs[len(evs)-1]; last.Seq != want || last.Text != "after" {
					t.Fatalf("the primary ends with seq %d %q, want %d \"after\": the abandoned event took a number", last.Seq, last.Text, want)
				}
				// The ring: a replay of everything has every number once and
				// not the abandoned event.
				replay := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}, MaxItems: 4096})
				recs := readN(t, replay, int(want))
				assertRun(t, "the ring's replay", recs, 1, int(want))
				assertNoneOmitted(t, "the ring", recs)
				watchedRecs = append(watchedRecs, readN(t, watch, int(want)-primaryCap)...)
			}
			if n := l.Health().DroppedAtClose; n != c.wantDrops {
				t.Fatalf("DroppedAtClose is %d, want %d", n, c.wantDrops)
			}
			closeLog(t, l, w)

			// Everything committed was read; the abandoned event never
			// follows it.
			watchedRecs = append(watchedRecs, readAll(t, watch)...)
			if !errors.Is(watch.Err(), ErrClosed) {
				t.Fatalf("the watcher ended with %v", watch.Err())
			}
			assertRun(t, "the watching subscriber", watchedRecs, 1, int(want))
			assertNoneOmitted(t, "the watching subscriber", watchedRecs)
			if recs := readAll(t, tiny); len(recs) != 0 || !errors.Is(tiny.Err(), ErrClosed) {
				t.Fatalf("the tiny subscriber was handed %d records and ended with %v", len(recs), tiny.Err())
			}
			fileRecs := fileRecords(t, w)
			assertRun(t, "the journal", fileRecs, 1, int(want))
			assertNoneOmitted(t, "the journal", fileRecs)
			if h := l.Health(); h.Omitted != 0 || h.SubscribersDropped != 0 || h.Journal.Omitted != 0 {
				t.Fatalf("health at the end is %+v: the abandoned event was counted", h)
			}
			lines := fileLines(t, w)
			closing := assertClosingIsLast(t, lines)
			if closing["droppedAtClose"] != float64(c.wantDrops) {
				t.Fatalf("closing says droppedAtClose %v, want %d", closing["droppedAtClose"], c.wantDrops)
			}
			for _, kind := range []string{journal.DiagRecordOmitted, journal.DiagSubscriberDropped} {
				if n := len(diags(lines, kind)); n != 0 {
					t.Fatalf("the journal has %d %s notes: an abandoned publish left an orphan", n, kind)
				}
			}
		})
	}
}

// TestEventLogResumeInsideTheRingWhileAPublisherRuns is A9(a): a cursor
// inside the ring, taken while a publisher is mid-run, delivers exactly
// (After.Seq, …] in order, with no duplicate and no hole across the seam
// between the pinned replay and the live buffer. The publisher is held at a
// barrier past the cursor until Subscribe has returned, and let go at once,
// so the cutoff falls inside its run: part of the delivery is the pinned
// replay and the rest is live, published while the owner replays.
func TestEventLogResumeInsideTheRingWhileAPublisherRuns(t *testing.T) {
	const total, after, cutoff = 2000, 300, 500
	l := newTestLog(t, EventLogOptions{})
	keepDrained(t, l)
	reached, subscribed := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(subscribed) })
	t.Cleanup(release) // a failed Subscribe must not leave the publisher held
	var pub sync.WaitGroup
	pub.Go(func() {
		for i := range total {
			l.Publish(context.Background(), nil, textEvent(fmt.Sprint(i+1)))
			if i+1 == cutoff {
				close(reached)
				<-subscribed
			}
		}
	})
	await(t, reached, "the publisher to reach the barrier")
	s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation(), Seq: after}, MaxItems: total})
	// The publisher is held, so this is the cutoff Subscribe took: (after,
	// cutoff] is the pinned replay and everything later is live.
	if n := lastSeq(t, l); n != cutoff {
		t.Fatalf("the cutoff is seq %d, want %d", n, cutoff)
	}
	release()
	recs := readN(t, s, total-after)
	waitDone(t, &pub)
	if pinned := cutoff - after; pinned <= 0 || pinned >= len(recs) || recs[len(recs)-1].Seq <= cutoff {
		t.Fatalf("%d records pinned of %d delivered, the last seq %d: the delivery was not replay and then live", pinned, len(recs), recs[len(recs)-1].Seq)
	}
	assertRun(t, "the resumed subscription", recs, after+1, total-after)
	for i, text := range recordTexts(t, recs) {
		if text != fmt.Sprint(recs[i].Seq) {
			t.Fatalf("seq %d carries %q", recs[i].Seq, text)
		}
	}
}

// TestEventLogSubscribePinsItsBacklogAgainstLaterEviction is A9(d): the ring
// evicts every record a subscription pinned, after the cutoff and before the
// subscriber reads one, and the subscription still delivers them all,
// followed by the live records, gapless. A second cursor at the same place,
// taken after the eviction, proves the ring no longer holds them.
func TestEventLogSubscribePinsItsBacklogAgainstLaterEviction(t *testing.T) {
	const ring, before, after, more = 64, 100, 50, 200
	l := newTestLog(t, EventLogOptions{RingEvents: ring})
	keepDrained(t, l)
	for i := range before {
		l.Publish(context.Background(), nil, textEvent(fmt.Sprint(i+1)))
	}
	cursor := &Cursor{Incarnation: l.Incarnation(), Seq: after}
	s, err := l.Subscribe(SubscribeOptions{After: cursor, MaxItems: more})
	if err != nil {
		t.Fatalf("the cursor at %d is inside a ring holding %d..%d: %v", after, before-ring+1, before, err)
	}
	within(t, "publishing while the subscriber reads nothing", func() {
		for i := range more {
			l.Publish(context.Background(), nil, textEvent(fmt.Sprint(before+i+1)))
		}
	})
	var cu ErrCursorUnresolvable
	if _, err := l.Subscribe(SubscribeOptions{After: cursor}); !errors.As(err, &cu) || cu.Reason != CursorNoJournal {
		t.Fatalf("a second cursor at %d after the eviction: %v, want no_journal", after, err)
	}
	recs := readN(t, s, before+more-after)
	assertRun(t, "the pinned subscription", recs, after+1, before+more-after)
	for i, text := range recordTexts(t, recs) {
		if text != fmt.Sprint(recs[i].Seq) {
			t.Fatalf("seq %d carries %q", recs[i].Seq, text)
		}
	}
}

// TestEventLogSubscribeRefusesACursorItCannotServeAndRegistersNothing is A9's
// synchronous failures: a foreign incarnation, a future seq, a head that has
// left the ring with no journal to serve it, and a backlog over the budget
// each fail with their reason, return no subscription, and register nothing
// — no subscriber, no owner goroutine. The cursors either side of each
// boundary succeed.
func TestEventLogSubscribeRefusesACursorItCannotServeAndRegistersNothing(t *testing.T) {
	const ring, published = 16, 40 // the ring holds 25..40
	l := newTestLog(t, EventLogOptions{RingEvents: ring})
	keepDrained(t, l)
	for i := range published {
		l.Publish(context.Background(), nil, textEvent(fmt.Sprintf("event %d", i+1)))
	}
	inc := l.Incarnation()
	owners := l.liveOwners.Load()
	base := subscriberCount(t, l)

	for _, c := range []struct {
		name string
		o    SubscribeOptions
		want CursorReason
	}{
		{"another incarnation", SubscribeOptions{After: &Cursor{Incarnation: NewIncarnation(), Seq: 30}}, CursorForeignIncarnation},
		{"no incarnation", SubscribeOptions{After: &Cursor{Seq: 30}}, CursorForeignIncarnation},
		{"a seq not yet published", SubscribeOptions{After: &Cursor{Incarnation: inc, Seq: published + 1}}, CursorFutureSeq},
		{"a head long evicted", SubscribeOptions{After: &Cursor{Incarnation: inc, Seq: 10}}, CursorNoJournal},
		{"a head one seq evicted", SubscribeOptions{After: &Cursor{Incarnation: inc, Seq: published - ring - 1}}, CursorNoJournal},
		{"from the start", SubscribeOptions{After: &Cursor{Incarnation: inc}}, CursorNoJournal},
		{"a backlog over the budget", SubscribeOptions{After: &Cursor{Incarnation: inc, Seq: 30}, MaxBytes: 64}, CursorBacklogTooLarge},
	} {
		s, err := l.Subscribe(c.o)
		var cu ErrCursorUnresolvable
		if s != nil || !errors.As(err, &cu) || cu.Reason != c.want {
			t.Fatalf("%s: got %v, %v; want ErrCursorUnresolvable{%s}", c.name, s, err, c.want)
		}
		if n := subscriberCount(t, l); n != base {
			t.Fatalf("%s: %d subscribers registered after the refusal, want %d", c.name, n, base)
		}
		if n := l.liveOwners.Load(); n != owners {
			t.Fatalf("%s: %d owner goroutines after the refusal, want %d", c.name, n, owners)
		}
	}
	// Nothing lingers to be offered the next record either.
	l.Publish(context.Background(), nil, textEvent("after the refusals"))
	if n := subscriberCount(t, l); n != base {
		t.Fatalf("%d subscribers after a later publish, want %d", n, base)
	}

	// The edges that are servable: the oldest record the ring holds, and
	// the last seq published (nothing to replay).
	oldest := uint64(published - ring + 1 + 1) // the later publish evicted one more
	s, err := l.Subscribe(SubscribeOptions{After: &Cursor{Incarnation: inc, Seq: oldest - 1}})
	if err != nil {
		t.Fatalf("a cursor just before the ring's oldest record: %v", err)
	}
	assertRun(t, "the ring's whole content", readN(t, s, ring), oldest, ring)
	s.Close()
	tip, err := l.Subscribe(SubscribeOptions{After: &Cursor{Incarnation: inc, Seq: published + 1}})
	if err != nil {
		t.Fatalf("a cursor at the last seq: %v", err)
	}
	l.Publish(context.Background(), nil, textEvent("next"))
	assertRun(t, "a cursor at the last seq", readN(t, tip, 1), published+2, 1)
}

// TestEventLogResumeWithItsHeadInTheJournal is A9(b): a cursor whose head has
// left the ring is served from the journal file. The ring holds one record, so
// all but the tip of the replay comes from the file, and a publisher runs
// throughout: the cutoff is taken with the publisher held mid-run, and it
// finishes while the owner is still delivering the file's part. The
// subscription delivers exactly (After.Seq, …] in order, with no duplicate and
// no hole across either seam — file to pinned record, pinned record to live.
func TestEventLogResumeWithItsHeadInTheJournal(t *testing.T) {
	const total, after, cutoff = 600, 10, 400
	l, w := newJournaledLog(t, EventLogOptions{RingEvents: 1})
	keepDrained(t, l)
	file := holdJournal(t, l, false)
	p := publishHeldAt(t, l, total, cutoff)
	// The publisher is held, so the file can be brought level with the cutoff
	// first: this case is about a head the file already holds.
	flushThrough(t, w, cutoff)
	s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation(), Seq: after}, MaxItems: total})
	if n := lastSeq(t, l); n != cutoff {
		t.Fatalf("the cutoff is seq %d, want %d", n, cutoff)
	}
	p.release()

	recs := readN(t, s, total-after)
	assertRun(t, "the resumed subscription", recs, after+1, total-after)
	for i, text := range recordTexts(t, recs) {
		if text != fmt.Sprint(recs[i].Seq) {
			t.Fatalf("seq %d carries %q", recs[i].Seq, text)
		}
	}
	if n := file.reads.Load(); n != 1 {
		t.Fatalf("the journal file was read %d times, want once: the head must come from it", n)
	}
	if n := file.waits.Load(); n != 0 {
		t.Fatalf("the owner waited %d times for a file that already reached the cutoff", n)
	}
}

// TestEventLogResumeSpanningTheJournalAndTheRing is A9(c): one cursor whose
// range is served in three parts — the head from the journal file, the middle
// from the ring records the cutoff pinned, and the rest live, published while
// the owner is delivering. Each part is substantial, and the delivered run is
// one contiguous sequence across both seams.
func TestEventLogResumeSpanningTheJournalAndTheRing(t *testing.T) {
	const total, after, cutoff, ring = 600, 10, 400, 64
	const pinnedFrom = cutoff - ring + 1 // the ring's oldest record at the cutoff
	l, w := newJournaledLog(t, EventLogOptions{RingEvents: ring})
	keepDrained(t, l)
	file := holdJournal(t, l, false)
	p := publishHeldAt(t, l, total, cutoff)
	flushThrough(t, w, cutoff)
	s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation(), Seq: after}, MaxItems: total})
	if n := lastSeq(t, l); n != cutoff {
		t.Fatalf("the cutoff is seq %d, want %d", n, cutoff)
	}
	p.release()

	recs := readN(t, s, total-after)
	assertRun(t, "the resumed subscription", recs, after+1, total-after)
	for i, text := range recordTexts(t, recs) {
		if text != fmt.Sprint(recs[i].Seq) {
			t.Fatalf("seq %d carries %q", recs[i].Seq, text)
		}
	}
	// The three parts are each real: the file served (after, pinnedFrom), the
	// pin served [pinnedFrom, cutoff], and the rest was published live.
	if n := file.reads.Load(); n != 1 {
		t.Fatalf("the journal file was read %d times, want once", n)
	}
	if pinnedFrom-after-1 < ring || total-cutoff < ring {
		t.Fatalf("the case is degenerate: %d records from the file, %d pinned, %d live", pinnedFrom-after-1, ring, total-cutoff)
	}
}

// TestEventLogTheJournalsHeadCostsTheSubscriptionNoBudget: the file range is
// streamed, never held, so it does not count against MaxBytes — only the
// pinned ring records do. A subscription with room for exactly its one pinned
// record replays hundreds from the file and is not dropped.
func TestEventLogTheJournalsHeadCostsTheSubscriptionNoBudget(t *testing.T) {
	const total = 200
	l, w := newJournaledLog(t, EventLogOptions{RingEvents: 1})
	keepDrained(t, l)
	for i := range total {
		publishWithin(t, l, textEvent(fmt.Sprint(i+1)))
	}
	flushThrough(t, w, total)
	// Room for the pinned record and not a byte more; the head is the other
	// 199 records.
	pinned, err := EncodeEvent(textEvent(fmt.Sprint(total)))
	if err != nil {
		t.Fatal(err)
	}
	s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}, MaxBytes: len(pinned)})
	assertRun(t, "the resumed subscription", readN(t, s, total), 1, total)
	if h := l.Health(); h.SubscribersDropped != 0 {
		t.Fatalf("the subscription was dropped (%v): the streamed head was charged to its budget", s.terminal())
	}
}

// TestEventLogAResumeWaitsForAStalledJournalAndThenServesIt is A9(e): at the
// cutoff the writer has not written the head of the range, so the owner asks
// for a flush and waits — delivering nothing while it does — and once the
// writer catches up it serves the whole range from the file. A publisher runs
// throughout.
func TestEventLogAResumeWaitsForAStalledJournalAndThenServesIt(t *testing.T) {
	const total, after, cutoff, ring = 300, 5, 200, 8
	l, _ := newJournaledLog(t, EventLogOptions{RingEvents: ring})
	keepDrained(t, l)
	stalled := holdJournal(t, l, true)
	p := publishHeldAt(t, l, total, cutoff)
	s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation(), Seq: after}, MaxItems: total})
	p.release()

	await(t, stalled.waiting, "the owner to wait for the stalled journal")
	select {
	case r, ok := <-s.Records():
		t.Fatalf("record %d was delivered (open: %v) while the file was behind the range", r.Seq, ok)
	default:
	}
	stalled.letGo()
	recs := readN(t, s, total-after)
	assertRun(t, "the resumed subscription", recs, after+1, total-after)
	for i, text := range recordTexts(t, recs) {
		if text != fmt.Sprint(recs[i].Seq) {
			t.Fatalf("seq %d carries %q", recs[i].Seq, text)
		}
	}
	if n := stalled.waits.Load(); n != 1 {
		t.Fatalf("the owner waited %d times, want once", n)
	}
}

// TestEventLogAResumeGivesUpOnAJournalThatNeverCatchesUp is A9(f): a file that
// never reaches the head of the range fails the subscription with
// journal_behind and delivers nothing of it — neither when the wait's bound
// runs out on a writer stuck inside a write, nor when the writer has finished
// and will never write those seqs at all. The journal's own error is kept, so
// a caller can tell the two apart.
func TestEventLogAResumeGivesUpOnAJournalThatNeverCatchesUp(t *testing.T) {
	t.Run("the wait's bound runs out", func(t *testing.T) {
		const total, after, cutoff, ring = 300, 5, 200, 8
		shortJournalWait(t, 20*time.Millisecond)
		l, _ := newJournaledLog(t, EventLogOptions{RingEvents: ring})
		keepDrained(t, l)
		stalled := holdJournal(t, l, true)
		p := publishHeldAt(t, l, total, cutoff)
		s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation(), Seq: after}, MaxItems: total})
		p.release()

		if recs := readAll(t, s); len(recs) != 0 {
			t.Fatalf("%d records were delivered of a range the file never reached", len(recs))
		}
		cu := assertUnresolvable(t, "a stalled journal", s.Err(), CursorJournalBehind)
		if !errors.Is(cu, context.DeadlineExceeded) {
			t.Fatalf("the subscription ended with %v, want the wait's own deadline", cu)
		}
		if n := stalled.reads.Load(); n != 0 {
			t.Fatalf("the file was read %d times after the wait gave up", n)
		}
	})

	t.Run("the writer has finished", func(t *testing.T) {
		const written, more, ring = 50, 200, 8
		l, w := newJournaledLog(t, EventLogOptions{RingEvents: ring})
		keepDrained(t, l)
		for i := range written {
			publishWithin(t, l, textEvent(fmt.Sprint(i+1)))
		}
		// The journal is closed under the session: what it has is on disk, and
		// nothing published from here on will ever be.
		if err := w.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := writerExited(w); !errors.Is(err, journal.ErrClosed) {
			t.Fatalf("waiting for the journal writer to finish: %v", err)
		}
		for i := range more {
			publishWithin(t, l, textEvent(fmt.Sprint(written+i+1)))
		}
		if gaps := w.Health().Gaps; len(gaps) != 0 {
			t.Fatalf("the journal recorded gaps %v: this case is about a file that is merely short", gaps)
		}
		s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}})
		if recs := readAll(t, s); len(recs) != 0 {
			t.Fatalf("%d records were delivered of a range the file will never hold", len(recs))
		}
		cu := assertUnresolvable(t, "a finished journal writer", s.Err(), CursorJournalBehind)
		if !errors.Is(cu, journal.ErrClosed) {
			t.Fatalf("the subscription ended with %v, want the journal's own ErrClosed", cu)
		}
	})
}

// answeringJournal is a journal that answers the file leg with what a test
// names, so the one mapping from the journal's errors to cursor reasons can be
// exercised for every error — several of which a real writer produces only
// from a corrupt file or a failing disk. Health reports lateGaps only after
// the cutoff has read it, which is where a writer that failed under the wait
// would record one.
//
// A read error is injected after deliver records of the range have been read
// from the real file and handed over, because that is where a real one is
// found: ReadRange parses and calls back per record, so a malformed line, an
// overlong one or a missing seq late in the range is discovered only once the
// records before it have gone out (headRange.serve).
type answeringJournal struct {
	journalFile
	waitErr  error
	readErr  error
	deliver  int
	lateGaps []journal.SeqRange
	healths  atomic.Int64
}

// errStopRead ends the real read once the injected prefix has been delivered.
// The reader returns a callback's error as it is, so it never leaves here.
var errStopRead = errors.New("test: the injected prefix has been delivered")

func (j *answeringJournal) Health() journal.Health {
	h := j.journalFile.Health()
	if j.healths.Add(1) > 1 {
		h.Gaps = j.lateGaps
	}
	return h
}

// Flushed reports the file as empty when the wait is the leg under test, so
// the owner waits instead of reading.
func (j *answeringJournal) Flushed() (uint64, int64) {
	if j.waitErr != nil {
		return 0, 0
	}
	return j.journalFile.Flushed()
}

func (j *answeringJournal) WaitFlushed(ctx context.Context, seq uint64) error {
	if j.waitErr != nil {
		return j.waitErr
	}
	return j.journalFile.WaitFlushed(ctx, seq)
}

func (j *answeringJournal) ReadRange(from, to uint64, fn func(journal.Record) error) error {
	if j.readErr == nil {
		return j.journalFile.ReadRange(from, to, fn)
	}
	if j.deliver > 0 {
		left := j.deliver
		err := j.journalFile.ReadRange(from, to, func(r journal.Record) error {
			if left == 0 {
				return errStopRead
			}
			left--
			return fn(r)
		})
		// The owner's own error (a subscription that ended under the send)
		// goes back as it is; only the stop is this fake's.
		if err != nil && !errors.Is(err, errStopRead) {
			return err
		}
	}
	return j.readErr
}

// TestEventLogTheFileLegsErrorsEachMapToOneReason pins the mapping in
// headRange.unresolvable: what the journal said, and why the cursor could not
// be resumed. Whichever it is, the subscription ends with that reason and
// keeps the journal's own error for the caller.
//
// Each read error is injected after a prefix of the range has been delivered,
// which is the shape a real one has: the range is streamed, so a file that
// turns out not to hold it whole is discovered with records already gone out
// (headRange.serve). What the subscription owes then is exactly this — it
// ends, with the reason, rather than stopping where the file stopped and
// passing the prefix off as the range. The consumer's half is to discard what
// it received, and the prefix is asserted here so that a leg which silently
// finished early instead of failing could not pass.
func TestEventLogTheFileLegsErrorsEachMapToOneReason(t *testing.T) {
	// The head of the range is 56 records long (the ring keeps the last four
	// of the 60 published), so a prefix of three is well inside it.
	const published, ring, prefix = 60, 4, 3
	lost := []journal.SeqRange{{From: 1, To: journal.MaxSeq}}
	for _, c := range []struct {
		name string
		wait error
		read error
		gaps []journal.SeqRange
		want CursorReason
	}{
		{name: "a gap line inside the range", read: fmt.Errorf("%w: seqs 4..9 were dropped", journal.ErrGap), want: CursorJournalGap},
		{name: "the file is still behind the range", read: fmt.Errorf("%w: it reaches seq 3", journal.ErrBehind), want: CursorJournalBehind},
		{name: "a line over the reader's limit", read: journal.ErrLineTooLong, want: CursorEvicted},
		{name: "a file that is not a journal", read: fmt.Errorf("%w: no header", journal.ErrMalformed), want: CursorEvicted},
		{name: "the file could not be opened", read: fs.ErrPermission, want: CursorEvicted},
		{name: "no journal after all", read: journal.ErrNoJournal, want: CursorNoJournal},
		{name: "the writer failed under the wait", wait: journal.ErrFailed, want: CursorJournalBehind},
		{name: "the writer failed and recorded the loss", wait: journal.ErrFailed, gaps: lost, want: CursorJournalGap},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, w := newJournaledLog(t, EventLogOptions{RingEvents: ring})
			keepDrained(t, l)
			for i := range published {
				publishWithin(t, l, textEvent(fmt.Sprint(i+1)))
			}
			flushThrough(t, w, published)
			l.file = &answeringJournal{journalFile: l.file, waitErr: c.wait, readErr: c.read, deliver: prefix, lateGaps: c.gaps}

			s := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}})
			// The wait leg fails before a byte is read, so it delivers
			// nothing; a read that fails has already handed over what it
			// parsed before the failure, and no more than that — never the
			// ring records behind it, and never anything live.
			delivered := prefix
			if c.wait != nil {
				delivered = 0
			}
			assertRun(t, "what the file leg delivered before it failed", readAll(t, s), 1, delivered)
			cu := assertUnresolvable(t, "the file leg", s.Err(), c.want)
			said := c.read
			if c.wait != nil {
				said = c.wait
			}
			if !errors.Is(cu, said) {
				t.Fatalf("the subscription ended with %v, which does not carry %v", cu, said)
			}
		})
	}
}

// TestEventLogAJournaledSubscribeStillRefusesWhatItCannotServe is A9's
// synchronous failures with a journal attached: a range crossing a gap the
// journal recorded is refused as journal_gap without the file being read at
// all, and a foreign incarnation, a future seq and a backlog over the budget
// fail exactly as they do without a journal — before the head is even
// considered. Every refusal registers nothing: no subscription, no owner
// goroutine, no read.
func TestEventLogAJournaledSubscribeStillRefusesWhatItCannotServe(t *testing.T) {
	const ring, publishes = 8, 400
	// A queue one entry deep: the writer gives a slot back only when a line
	// reaches the OS, so a run of publishes overflows it within a few events
	// and the drop is recorded as a gap.
	w, inc := newTestJournalWith(t, func(o *journal.Options) { o.QueueEntries = 2 })
	l := newTestLog(t, EventLogOptions{Journal: w, Incarnation: inc, RingEvents: ring})
	keepDrained(t, l)
	file := holdJournal(t, l, false)
	for i := range publishes {
		publishWithin(t, l, textEvent(fmt.Sprint(i+1)))
	}
	gaps := w.Health().Gaps
	oldest := uint64(publishes - ring + 1) // the ring's oldest record
	if len(gaps) == 0 || gaps[0].From >= oldest {
		t.Fatalf("the journal's gaps are %v: none of them is below the ring's oldest record, seq %d", gaps, oldest)
	}
	owners := l.liveOwners.Load()
	base := subscriberCount(t, l)

	for _, c := range []struct {
		name string
		o    SubscribeOptions
		want CursorReason
	}{
		{"another incarnation", SubscribeOptions{After: &Cursor{Incarnation: NewIncarnation(), Seq: 30}}, CursorForeignIncarnation},
		{"a seq not yet published", SubscribeOptions{After: &Cursor{Incarnation: inc, Seq: publishes + 1}}, CursorFutureSeq},
		// The budget is judged before the head: this cursor's head needs the
		// file too, and it is the pinned backlog that refuses it.
		{"a backlog over the budget", SubscribeOptions{After: &Cursor{Incarnation: inc}, MaxBytes: 1}, CursorBacklogTooLarge},
		{"a range crossing a recorded gap", SubscribeOptions{After: &Cursor{Incarnation: inc}}, CursorJournalGap},
	} {
		s, err := l.Subscribe(c.o)
		if s != nil {
			t.Fatalf("%s: a subscription was registered: %v", c.name, err)
		}
		assertUnresolvable(t, c.name, err, c.want)
		if n := subscriberCount(t, l); n != base {
			t.Fatalf("%s: %d subscribers registered after the refusal, want %d", c.name, n, base)
		}
		if n := l.liveOwners.Load(); n != owners {
			t.Fatalf("%s: %d owner goroutines after the refusal, want %d", c.name, n, owners)
		}
		if n := file.reads.Load(); n != 0 {
			t.Fatalf("%s: the journal file was read %d times; a refusal reads nothing", c.name, n)
		}
	}
}

// TestEventLogALiveOnlySubscriptionStartsWithTheNextEvent: After nil is live
// only, from the first event published after Subscribe returns.
func TestEventLogALiveOnlySubscriptionStartsWithTheNextEvent(t *testing.T) {
	l := newTestLog(t, EventLogOptions{})
	for i := range 5 {
		l.Publish(context.Background(), nil, textEvent(fmt.Sprint(i+1)))
	}
	s := mustSubscribe(t, l, SubscribeOptions{})
	for i := range 3 {
		l.Publish(context.Background(), nil, textEvent(fmt.Sprint(6+i)))
	}
	assertRun(t, "a live-only subscription", readN(t, s, 3), 6, 3)
}

// TestEventLogAHandedOverRecordNoLongerCountsAgainstTheBudget: the budget
// counts records not yet handed to Records, so a subscription that kept
// within it is never dropped for a record its reader already has. With room
// for one record — by items, by bytes, and by bytes for a record replayed
// from the ring — the owner hands the first to the reader and is held right
// there, before it runs again; the next record published must be buffered
// and delivered, not overflow the subscription.
func TestEventLogAHandedOverRecordNoLongerCountsAgainstTheBudget(t *testing.T) {
	body, err := EncodeEvent(textEvent("1"))
	if err != nil {
		t.Fatal(err)
	}
	size := len(body) // "2"'s record is the same size
	for _, c := range []struct {
		name   string
		o      SubscribeOptions
		replay bool // the first record is published before Subscribe, and pinned
	}{
		{"by items", SubscribeOptions{MaxItems: 1}, false},
		{"by bytes", SubscribeOptions{MaxBytes: size}, false},
		{"by bytes, a pinned record", SubscribeOptions{MaxBytes: size}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newTestLog(t, EventLogOptions{})
			handed, resume := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(resume) })
			t.Cleanup(release) // before the log's Close, which waits for the owner
			l.hooks = &logHooks{delivered: func(seq uint64) {
				if seq == 1 {
					close(handed)
					<-resume
				}
			}}
			if c.replay {
				publishWithin(t, l, textEvent("1"))
				c.o.After = &Cursor{Incarnation: l.Incarnation()}
			}
			s := mustSubscribe(t, l, c.o)
			if !c.replay {
				publishWithin(t, l, textEvent("1"))
			}
			assertRun(t, "the first record", readN(t, s, 1), 1, 1)
			await(t, handed, "the owner to hand the first record over")
			publishWithin(t, l, textEvent("2"))
			if n := l.Health().SubscribersDropped; n != 0 {
				t.Fatalf("the subscription was dropped (%v) for a record its reader already had", s.terminal())
			}
			release()
			assertRun(t, "the next record", readN(t, s, 1), 2, 1)
		})
	}
}

// TestEventLogSubscriptionsSurviveConcurrentPublishDropCloseAndLogClose is
// A10's churn: publishers, a subscriber that is dropped, one that closes
// itself, one closed from elsewhere, one that walks away from its backlog,
// subscribes and closes in a loop, and the log's own Close, all at once.
// Every subscription ends with ErrClosed or ErrSlowConsumer and was handed a
// gapless run; the number of publishes that returned true is the last seq
// the primary and the journal hold, gaplessly; every one that returned false
// is counted as dropped at close; and no owner goroutine or journal writer
// is left behind.
//
// The overlap is forced, not hoped for. The publishers pause part-way until
// the resumer has completed several rounds and left one resume abandoned.
// Then the primary's reader stops at closeAt, so the log closes wedged: one
// publisher is blocked on the full primary inside the boundary (the hook
// says so), every other has publishes left, and the resumer is still
// looping. The test asserts that each of those paths ran.
func TestEventLogSubscriptionsSurviveConcurrentPublishDropCloseAndLogClose(t *testing.T) {
	const publishers, each = 6, 400
	const total = publishers * each
	// Each publisher pauses after pauseAt publishes until the resumer has
	// done churnRounds rounds; publishers*pauseAt is under closeAt, so the
	// reader cannot stop before every publisher is past its pause. The
	// reader stops once it has seen closeAt, and the publish that takes
	// wedgeAt, with the primary's buffer full behind it, blocks for good.
	const pauseAt, closeAt, churnRounds = 100, 1000, 3
	const wedgeAt = closeAt + primaryCap + 1
	settleGoroutines(t, ownerFrame, 0)
	settleGoroutines(t, writerFrame, 0)
	l, w := newJournaledLog(t, EventLogOptions{RingEvents: 256})
	inc := l.Incarnation()
	wedged := make(chan struct{})
	l.hooks = &logHooks{beforePrimarySend: func(seq uint64) {
		if seq == wedgeAt {
			close(wedged) // taken once: nothing commits after it
		}
	}}

	var latest atomic.Uint64
	drained := make(chan []uint64, 1)
	go func() {
		var seqs []uint64
		for {
			ev := <-l.Primary()
			seqs = append(seqs, ev.Seq)
			latest.Store(ev.Seq)
			if ev.Seq == closeAt {
				drained <- seqs
				return
			}
		}
	}()

	type sub struct {
		name  string
		s     *Subscription
		from  uint64
		recs  []Record
		drop  bool // may end as a slow consumer
		reads chan struct{}
	}
	subscribe := func(name string, o SubscribeOptions, from uint64, drop bool) *sub {
		s := mustSubscribe(t, l, o)
		return &sub{name: name, s: s, from: from, drop: drop, reads: make(chan struct{})}
	}
	fast := subscribe("fast", SubscribeOptions{MaxItems: total}, 1, false)
	slow := subscribe("slow", SubscribeOptions{MaxItems: 4}, 1, true)
	self := subscribe("closes itself", SubscribeOptions{MaxItems: total}, 1, false)
	other := subscribe("closed from elsewhere", SubscribeOptions{MaxItems: total}, 1, false)
	subs := []*sub{fast, slow, self, other}

	var readers sync.WaitGroup
	read := func(x *sub, stopAfter int) {
		readers.Go(func() {
			defer close(x.reads)
			for r := range x.s.Records() {
				x.recs = append(x.recs, r)
				if len(x.recs) == stopAfter {
					x.s.Close()
				}
			}
		})
	}
	read(fast, -1)
	read(self, 50)
	read(other, -1)

	churned := make(chan struct{}) // the resumer's rounds are done
	resumeChurned := sync.OnceFunc(func() { close(churned) })
	t.Cleanup(resumeChurned) // a failed resumer must not leave the publishers paused
	var pubs sync.WaitGroup
	var trues, falses atomic.Int64
	for p := range publishers {
		pubs.Go(func() {
			for i := range each {
				if i == pauseAt {
					<-churned
				}
				if l.Publish(context.Background(), nil, textEvent(fmt.Sprintf("p%d-%d", p, i))) {
					trues.Add(1)
				} else {
					falses.Add(1)
				}
			}
		})
	}

	// A resumer subscribes behind the tip, reads a little, and closes, over
	// and over, until the log refuses it; one resume walks away from its
	// backlog without reading or closing, left for the log's Close.
	type resume struct {
		s     *Subscription
		after uint64
	}
	var churn sync.WaitGroup
	var abandoned atomic.Pointer[resume]
	var rounds atomic.Int64
	var churnSawClose atomic.Bool
	churn.Go(func() {
		for i := 0; ; i++ {
			after := latest.Load()
			after -= min(after, 20)
			s, err := l.Subscribe(SubscribeOptions{After: &Cursor{Incarnation: inc, Seq: after}})
			if errors.Is(err, ErrClosed) {
				churnSawClose.Store(true)
				return
			}
			var cu ErrCursorUnresolvable
			if errors.As(err, &cu) {
				continue // the tip moved past the ring; try again
			}
			if err != nil {
				t.Errorf("resume %d: %v", i, err)
				resumeChurned()
				return
			}
			if abandoned.Load() == nil {
				abandoned.Store(&resume{s, after})
				continue
			}
			var got []uint64
			for r := range s.Records() {
				if got = append(got, r.Seq); len(got) == 5 {
					s.Close()
				}
			}
			if p := runSeqs(got, after+1); p != "" {
				t.Errorf("resume %d after %d: %s", i, after, p)
			}
			if err := s.Err(); !errors.Is(err, ErrClosed) && !errors.Is(err, ErrSlowConsumer) {
				t.Errorf("resume %d ended with %v", i, err)
			}
			if rounds.Add(1) == churnRounds {
				resumeChurned()
			}
		}
	})

	seqs := await(t, drained, "the primary's reader to reach the close point")
	await(t, wedged, "a publisher to block on the full primary inside the boundary")
	other.s.Close()
	closeLog(t, l, w)
	waitDone(t, &pubs)
	waitDone(t, &churn)
	for _, ev := range drainPrimary(l) {
		seqs = append(seqs, ev.Seq)
	}
	read(slow, -1)
	waitDone(t, &readers)

	if n := int(trues.Load() + falses.Load()); n != total {
		t.Fatalf("%d publishes returned, want %d", n, total)
	}
	committed := int(trues.Load())
	t.Logf("%d publishes committed and %d abandoned by the close; %d resume rounds", committed, falses.Load(), rounds.Load())
	if committed != wedgeAt-1 || falses.Load() == 0 {
		t.Fatalf("%d publishes committed and %d abandoned: the log did not close wedged at seq %d with publishes in flight", committed, falses.Load(), wedgeAt)
	}
	if p := runSeqs(seqs, 1); p != "" || len(seqs) != committed {
		t.Fatalf("the primary holds %d events (%s); %d publishes returned true", len(seqs), p, committed)
	}
	if n := l.Health().DroppedAtClose; n != int(falses.Load()) {
		t.Fatalf("DroppedAtClose is %d; %d publishes returned false", n, falses.Load())
	}
	if n := rounds.Load(); n < churnRounds || abandoned.Load() == nil || !churnSawClose.Load() {
		t.Fatalf("the resumer did %d rounds (want at least %d), abandoned a resume: %v, and saw the close: %v",
			n, churnRounds, abandoned.Load() != nil, churnSawClose.Load())
	}
	for _, x := range subs {
		err := x.s.Err()
		dropped := x.drop && errors.Is(err, ErrSlowConsumer)
		if !errors.Is(err, ErrClosed) && !dropped {
			t.Fatalf("%s ended with %v", x.name, err)
		}
		assertRun(t, x.name, x.recs, x.from, -1)
	}
	// The resume that walked away: its owner was holding the first record
	// of its replay when the end came, and may hand over just that one.
	if r := abandoned.Load(); r != nil {
		recs := readAll(t, r.s)
		if err := r.s.Err(); !errors.Is(err, ErrClosed) && !errors.Is(err, ErrSlowConsumer) {
			t.Fatalf("the abandoned resume ended with %v", err)
		}
		if len(recs) > 1 {
			t.Fatalf("the abandoned resume was handed %d records nobody read for", len(recs))
		}
		assertRun(t, "the abandoned resume", recs, r.after+1, -1)
	}
	assertRun(t, "the journal", fileRecords(t, w), 1, committed)
	// The wedged publisher was in flight at the cutoff, so the closing diag
	// counts it at least; publishes that arrived later are in Health only.
	if closing := assertClosingIsLast(t, fileLines(t, w)); closing["droppedAtClose"].(float64) < 1 {
		t.Fatalf("closing says droppedAtClose %v with a publisher blocked across the cutoff", closing["droppedAtClose"])
	}

	if n := l.liveOwners.Load(); n != 0 {
		t.Fatalf("%d owner goroutines counted after Close", n)
	}
	settleGoroutines(t, ownerFrame, 0)
	settleGoroutines(t, writerFrame, 0)
}

// TestEventLogAnAbandonedBacklogReaderNeedsNoReaderToEnd is A10's abandoned
// backlog: two subscriptions resume from the start of a large backlog, read
// three records, and their readers walk away. One is closed explicitly; the
// other is left to the log's Close. Neither owner needs anyone to read to
// exit: Close, which waits for every owner, returns while nobody reads, and
// both channels are closed afterwards with ErrClosed.
func TestEventLogAnAbandonedBacklogReaderNeedsNoReaderToEnd(t *testing.T) {
	const backlog = 1000
	l := newTestLog(t, EventLogOptions{})
	keepDrained(t, l)
	for i := range backlog {
		l.Publish(context.Background(), nil, textEvent(fmt.Sprint(i+1)))
	}
	from := &Cursor{Incarnation: l.Incarnation()}
	closed := mustSubscribe(t, l, SubscribeOptions{After: from})
	left := mustSubscribe(t, l, SubscribeOptions{After: from})
	assertRun(t, "the closed reader's first records", readN(t, closed, 3), 1, 3)
	assertRun(t, "the left reader's first records", readN(t, left, 3), 1, 3)

	closed.Close()
	within(t, "Close with two abandoned backlog readers", func() { l.Close(context.Background()) })
	if n := l.liveOwners.Load(); n != 0 {
		t.Fatalf("%d owner goroutines counted after Close", n)
	}
	for name, s := range map[string]*Subscription{"closed": closed, "left": left} {
		// An owner blocked in its send when the end came may still hand over
		// that one record; never more, and never a hole.
		recs := readAll(t, s)
		if len(recs) > 1 || len(recs) == 1 && recs[0].Seq != 4 {
			t.Fatalf("the %s reader got %d more records after the end", name, len(recs))
		}
		if !errors.Is(s.Err(), ErrClosed) {
			t.Fatalf("the %s reader ended with %v", name, s.Err())
		}
	}
}

// TestEventLogCloseTwiceAndErrStaysPut is A10's idempotence: Close on a
// subscription twice, on the log from several goroutines at once and then
// again; Err is nil while the subscription runs and the same value on every
// read once Records has closed; a closed log refuses everything quietly.
func TestEventLogCloseTwiceAndErrStaysPut(t *testing.T) {
	l := NewEventLog(EventLogOptions{})
	s := mustSubscribe(t, l, SubscribeOptions{})
	if err := s.Err(); err != nil {
		t.Fatalf("Err before the end is %v", err)
	}
	s.Close()
	s.Close()
	readAll(t, s)
	for range 3 {
		if err := s.Err(); !errors.Is(err, ErrClosed) {
			t.Fatalf("Err after the end is %v", err)
		}
	}
	byLog := mustSubscribe(t, l, SubscribeOptions{})
	var closers sync.WaitGroup
	for range 3 {
		closers.Go(func() { l.Close(context.Background()) })
	}
	waitDone(t, &closers)
	within(t, "a later Close", func() { l.Close(context.Background()) })
	readAll(t, byLog)
	if err := byLog.Err(); !errors.Is(err, ErrClosed) {
		t.Fatalf("a subscription the log closed ended with %v", err)
	}
	byLog.Close()
	if err := byLog.Err(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Close after the end changed Err to %v", err)
	}
	if l.Publish(context.Background(), nil, textEvent("late")) || l.TryPublish(textEvent("late")) {
		t.Fatal("a publish on a closed log returned true")
	}
	if _, err := l.Subscribe(SubscribeOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe on a closed log: %v", err)
	}
	l.Note(journal.DiagNote{Kind: journal.DiagAgentStderr})
	h := l.Health()
	if h.DroppedAtClose != 2 || h.NotesDropped != 1 || h.Journal.State != journal.StateOff {
		t.Fatalf("health after close is %+v", h)
	}
	if n := len(drainPrimary(l)); n != 0 {
		t.Fatalf("%d events reached the primary after Close", n)
	}
}

// TestEventLogAWedgedPrimaryHoldsNeitherNotesNorCloseAndCloseReleasesSubscribers
// is A10's wedge: the primary full and a publisher blocked on it inside the
// boundary. Note, Health and Subscription.Close all return at once;
// Subscribe calls wait; Close releases the publisher (false) and every
// waiting Subscribe (ErrClosed). Every note written before Close is in the
// journal ahead of closing, which counts the abandoned publish.
func TestEventLogAWedgedPrimaryHoldsNeitherNotesNorCloseAndCloseReleasesSubscribers(t *testing.T) {
	const waiters, notes = 4, 20
	l, w := newJournaledLog(t, EventLogOptions{})
	pre := mustSubscribe(t, l, SubscribeOptions{})
	fillPrimary(t, l)

	hook, inside := insideAt(primaryCap + 1)
	arrivals := make(chan admitKind, 2*waiters)
	l.hooks = &logHooks{beforePrimarySend: hook, admitting: func(k admitKind) { arrivals <- k }}
	aResult := make(chan bool, 1)
	go func() { aResult <- l.Publish(context.Background(), nil, textEvent("wedged")) }()
	await(t, inside, "the publisher to block on the full primary")
	if k := await(t, arrivals, "the publisher's arrival"); k != admitPublish {
		t.Fatalf("the first arrival was %v", k)
	}

	type subResult struct {
		s   *Subscription
		err error
	}
	subResults := make(chan subResult, waiters)
	for range waiters {
		go func() {
			s, err := l.Subscribe(SubscribeOptions{})
			subResults <- subResult{s, err}
		}()
	}
	for range waiters {
		if k := await(t, arrivals, "a Subscribe's arrival"); k != admitSubscribe {
			t.Fatalf("an arrival was %v, want a Subscribe", k)
		}
	}
	within(t, "Note with the primary wedged", func() {
		for i := range notes {
			l.Note(journal.DiagNote{Kind: journal.DiagAgentStderr, Fields: map[string]any{"line": i}})
		}
	})
	within(t, "Health with the primary wedged", func() { l.Health() })
	within(t, "Subscription.Close with the primary wedged", pre.Close)
	select {
	case r := <-subResults:
		t.Fatalf("a Subscribe returned (%v) while the boundary was held", r.err)
	default:
	}

	closeLog(t, l, w)
	if await(t, aResult, "the wedged publisher") {
		t.Fatal("the wedged publisher returned true")
	}
	for range waiters {
		if r := await(t, subResults, "a waiting Subscribe"); r.s != nil || !errors.Is(r.err, ErrClosed) {
			t.Fatalf("a waiting Subscribe returned %v, %v; want ErrClosed", r.s, r.err)
		}
	}
	lines := fileLines(t, w)
	closing := assertClosingIsLast(t, lines)
	if closing["droppedAtClose"] != float64(1) {
		t.Fatalf("closing says droppedAtClose %v, want 1", closing["droppedAtClose"])
	}
	if n := len(diags(lines, journal.DiagAgentStderr)); n != notes {
		t.Fatalf("the journal has %d of the %d notes", n, notes)
	}
	assertRun(t, "the journal", fileRecords(t, w), 1, primaryCap)
}

// TestEventLogSubscribeHonoursItsCtxWhileWaitingForTheBoundary is plan 024
// §3.6's SubscribeOptions.Ctx over the same wedge: a Subscribe waiting behind
// a publisher blocked on the full primary returns its ctx's error once the ctx
// ends — with nothing registered and no owner started — and the publisher is
// untouched by it. A ctx that had already ended is refused even when the
// boundary is free, every time (the check after the boundary is won; without
// it the select would register one call in two). And a ctx is consulted for
// that wait alone: one that ends after Subscribe returned ends nothing.
func TestEventLogSubscribeHonoursItsCtxWhileWaitingForTheBoundary(t *testing.T) {
	l := newTestLog(t, EventLogOptions{})
	fillPrimary(t, l)
	hook, inside := insideAt(primaryCap + 1)
	arrivals := make(chan admitKind, 2)
	l.hooks = &logHooks{beforePrimarySend: hook, admitting: func(k admitKind) { arrivals <- k }}
	published := make(chan bool, 1)
	go func() { published <- l.Publish(context.Background(), nil, textEvent("wedged")) }()
	await(t, inside, "the publisher to block on the full primary")
	if k := await(t, arrivals, "the publisher's arrival"); k != admitPublish {
		t.Fatalf("the first arrival was %v", k)
	}

	type subResult struct {
		s   *Subscription
		err error
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan subResult, 1)
	go func() {
		s, err := l.Subscribe(SubscribeOptions{Ctx: ctx})
		got <- subResult{s, err}
	}()
	if k := await(t, arrivals, "the Subscribe's arrival"); k != admitSubscribe {
		t.Fatalf("the second arrival was %v, want the Subscribe", k)
	}
	select {
	case r := <-got:
		t.Fatalf("the Subscribe returned (%v) while the boundary was held", r.err)
	default:
	}
	cancel()
	if r := await(t, got, "the cancelled Subscribe"); r.s != nil || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("the cancelled Subscribe returned %v, %v; want nil, context.Canceled", r.s, r.err)
	}
	if n := l.liveOwners.Load(); n != 0 {
		t.Fatalf("%d subscription owners run after a cancelled Subscribe", n)
	}

	// The publisher never knew: one read makes room and it commits.
	<-l.Primary()
	if !await(t, published, "the wedged publisher") {
		t.Fatal("the wedged publisher returned false")
	}
	l.hooks = nil
	registered := func() int {
		l.sem <- struct{}{}
		defer l.release()
		return len(l.subs)
	}
	if n := registered(); n != 0 {
		t.Fatalf("%d subscriptions are registered after a cancelled Subscribe", n)
	}

	// Already ended, with the boundary free: refused every time.
	ended, end := context.WithCancel(context.Background())
	end()
	for i := range 64 {
		if s, err := l.Subscribe(SubscribeOptions{Ctx: ended}); s != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("attempt %d with an ended ctx and a free boundary: %v, %v", i, s, err)
		}
	}
	if n, owners := registered(), l.liveOwners.Load(); n != 0 || owners != 0 {
		t.Fatalf("%d subscriptions registered and %d owners running after the ended ctx's attempts", n, owners)
	}

	// Consulted for the wait alone.
	drainPrimary(l)
	later, endLater := context.WithCancel(context.Background())
	s := mustSubscribe(t, l, SubscribeOptions{Ctx: later})
	endLater()
	publishWithin(t, l, textEvent("after"))
	if texts := recordTexts(t, readN(t, s, 1)); texts[0] != "after" || s.Err() != nil {
		t.Fatalf("a subscription whose ctx ended after it was opened got %q, err %v", texts, s.Err())
	}
	s.Close()
}

// TestEventLogClosingCountsEveryPublishInFlightAtTheCutoff: the closing
// diag's droppedAtClose counts every publish that was in flight when Close
// cut off, not only the one holding the boundary. With the primary full, A
// is blocked on it inside the boundary and B waits for admission. Close cuts
// off; B gives up on the cutoff while A still holds the boundary (so the
// cutoff is B's only way out) and is held there, not yet counted; A then
// gives up, is counted and releases the boundary. Close must wait for B too:
// a Close that waited only for the boundary would write closing with 1.
//
// B is released only once Close's closeWaits hook has fired, and that hook
// runs inside Close's wait, with the publishes it is waiting for — so the
// test cannot pass without the wait: without it the hook never fires and the
// test times out on it rather than reaching the count.
func TestEventLogClosingCountsEveryPublishInFlightAtTheCutoff(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	fillPrimary(t, l)

	aHook, aInside := insideAt(primaryCap + 1)
	var arrivals atomic.Int64
	bWaiting, bGaveUp := make(chan struct{}), make(chan struct{})
	held := make(chan struct{})
	releaseHeld := sync.OnceFunc(func() { close(held) })
	t.Cleanup(releaseHeld) // before the log's Close: a failure must not strand either publisher
	closeWaiting := make(chan int, 1)
	l.hooks = &logHooks{
		beforePrimarySend: aHook,
		admitting: func(admitKind) {
			if arrivals.Add(1) == 2 {
				close(bWaiting)
			}
		},
		abandoning: func(inside bool) {
			if inside {
				// A keeps the boundary until B has given up.
				select {
				case <-bGaveUp:
				case <-held:
				}
				return
			}
			close(bGaveUp)
			<-held
		},
		closeWaits: func(inflight int) { closeWaiting <- inflight },
	}
	aResult, bResult := make(chan bool, 1), make(chan bool, 1)
	go func() { aResult <- l.Publish(context.Background(), nil, textEvent("A")) }()
	await(t, aInside, "A to block on the full primary inside the boundary")
	go func() { bResult <- l.Publish(context.Background(), nil, textEvent("B")) }()
	await(t, bWaiting, "B to wait for admission")

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		l.Close(context.Background())
	}()
	await(t, bGaveUp, "B to give up on the cutoff")
	// Hold B until Close is waiting for the publishes in flight, which B is
	// one of: B is inside the region and cannot leave until it is released.
	if n := await(t, closeWaiting, "Close to wait for the publishes in flight"); n < 1 {
		t.Fatalf("Close waited for %d publishes in flight, want at least B's", n)
	}
	releaseHeld()
	if await(t, aResult, "A") || await(t, bResult, "B") {
		t.Fatal("a publish abandoned at the cutoff returned true")
	}
	await(t, closed, "Close")
	closeLog(t, l, w)

	closing := assertClosingIsLast(t, fileLines(t, w))
	if got, want := closing["droppedAtClose"], l.Health().DroppedAtClose; got != float64(2) || want != 2 {
		t.Fatalf("closing says droppedAtClose %v and Health %d, want 2 for both: a publish in flight at the cutoff was left out", got, want)
	}
}

// TestEventLogAPublishArrivingWhileCloseWaitsIsRefusedRatherThanHeld: the
// in-flight region's door never makes a publisher wait. With A in flight and
// Close waiting for it, C arrives with a live ctx of its own and must come
// straight back false: a door that waited for Close would hold C's event off
// the wire with neither its ctx nor its session's done able to release it,
// which is exactly what emitCtx's pre-wire EventCommand must never meet.
func TestEventLogAPublishArrivingWhileCloseWaitsIsRefusedRatherThanHeld(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	fillPrimary(t, l)

	aHook, aInside := insideAt(primaryCap + 1)
	held := make(chan struct{})
	releaseHeld := sync.OnceFunc(func() { close(held) })
	t.Cleanup(releaseHeld) // before the log's Close: a failure must not strand A
	closeWaiting := make(chan int, 1)
	l.hooks = &logHooks{
		beforePrimarySend: aHook,
		abandoning: func(inside bool) {
			if inside {
				<-held // A stays in flight while Close waits for it
			}
		},
		closeWaits: func(inflight int) { closeWaiting <- inflight },
	}
	aResult := make(chan bool, 1)
	go func() { aResult <- l.Publish(context.Background(), nil, textEvent("A")) }()
	await(t, aInside, "A to block on the full primary inside the boundary")

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		l.Close(context.Background())
	}()
	await(t, closeWaiting, "Close to wait for A")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cResult := make(chan bool, 1)
	within(t, "a publish arriving while Close waits", func() {
		cResult <- l.Publish(ctx, nil, textEvent("C"))
	})
	if <-cResult {
		t.Fatal("a publish after the cutoff returned true")
	}
	releaseHeld()
	if await(t, aResult, "A") {
		t.Fatal("a publish abandoned at the cutoff returned true")
	}
	await(t, closed, "Close")
	closeLog(t, l, w)

	if got := l.Health().DroppedAtClose; got != 2 {
		t.Fatalf("Health counts %d dropped at close, want A's and C's", got)
	}
}

// publishingError is an error whose Error method does the two things a
// caller's error must never be able to wedge: it publishes to the log, and it
// closes it. Encoding calls Error and classifies it (errors.Is, errors.As),
// all on the publisher's goroutine, so encoding must happen before the
// publisher is in flight — with the encoding inside, this error's own Publish
// and Close would each wait for a region the publisher itself holds.
type publishingError struct {
	l     *EventLog
	once  sync.Once
	inner chan bool // capacity 1: what the re-entrant Publish returned
}

func (e *publishingError) Error() string {
	e.once.Do(func() {
		e.inner <- e.l.Publish(context.Background(), nil, textEvent("inner"))
		e.l.Close(context.Background())
	})
	return "agent test: an error whose message publishes and closes"
}

// TestEventLogAnErrorThatPublishesAndClosesWhileItIsEncodedDeadlocksNothing:
// the event carrying that error is encoded with nothing held, so its Error
// method publishes (that event lands, numbered 1) and closes the log, and the
// outer publish then finds the cutoff and returns false.
func TestEventLogAnErrorThatPublishesAndClosesWhileItIsEncodedDeadlocksNothing(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	e := &publishingError{l: l, inner: make(chan bool, 1)}
	ok := true
	within(t, "publishing an event whose Error publishes and closes", func() {
		ok = l.Publish(context.Background(), nil, Event{Type: EventError, At: logTestTime, Err: e})
	})
	if ok {
		t.Fatal("the publish returned true although its own error closed the log first")
	}
	if !await(t, e.inner, "the re-entrant publish inside Error") {
		t.Fatal("the publish made from inside Error returned false")
	}
	closeLog(t, l, w)

	if evs := drainPrimary(l); len(evs) != 1 || evs[0].Seq != 1 || evs[0].Text != "inner" {
		t.Fatalf("the primary holds %+v, want only the event published from inside Error", evs)
	}
	recs := fileRecords(t, w)
	assertRun(t, "the journal file", recs, 1, 1)
	assertNoneOmitted(t, "the journal file", recs)
	if got := l.Health().DroppedAtClose; got != 1 {
		t.Fatalf("Health counts %d dropped at close, want the one the cutoff refused", got)
	}
}

// TestEventLogCloseLeavesNoOwnerOrJournalGoroutineBehind is A10's leak check,
// by counting: with subscriptions in every state and a journal writing, the
// counts first show the goroutines running (so the check can see them), and
// after Close none is left.
func TestEventLogCloseLeavesNoOwnerOrJournalGoroutineBehind(t *testing.T) {
	settleGoroutines(t, ownerFrame, 0)
	settleGoroutines(t, writerFrame, 0)
	l, w := newJournaledLog(t, EventLogOptions{})
	keepDrained(t, l)
	live := mustSubscribe(t, l, SubscribeOptions{})
	stuck := mustSubscribe(t, l, SubscribeOptions{MaxItems: 2})
	for i := range 300 {
		l.Publish(context.Background(), nil, textEvent(fmt.Sprint(i+1)))
	}
	backlog := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}})
	readN(t, backlog, 1)
	closedOne := mustSubscribe(t, l, SubscribeOptions{})
	closedOne.Close()
	readN(t, live, 1)

	// A goroutine the scheduler has not run yet shows only its go
	// statement's wrapper on its stack, not the function the count looks
	// for. The owners of live and backlog have run (each delivered a record);
	// the writer has once it has flushed.
	ctx, cancel := context.WithTimeout(context.Background(), logWatchdog)
	defer cancel()
	if err := w.WaitFlushed(ctx, 300); err != nil {
		t.Fatal(err)
	}
	if got := goroutinesRunning(ownerFrame); got < 2 {
		t.Fatalf("%d owner goroutines running with live and backlog open, want at least 2: the count cannot see them", got)
	}
	if got := goroutinesRunning(writerFrame); got != 1 {
		t.Fatalf("%d journal writers running, want 1: the count cannot see it", got)
	}
	closeLog(t, l, w)
	if n := l.liveOwners.Load(); n != 0 {
		t.Fatalf("%d owner goroutines counted after Close", n)
	}
	settleGoroutines(t, ownerFrame, 0)
	settleGoroutines(t, writerFrame, 0)
	for _, s := range []*Subscription{live, stuck, backlog, closedOne} {
		readAll(t, s)
		if err := s.Err(); !errors.Is(err, ErrClosed) && !errors.Is(err, ErrSlowConsumer) {
			t.Fatalf("a subscription ended with %v", err)
		}
	}
}

// TestEventLogNoNoteEverFollowsClosing: notes racing Close either land
// before the closing diag or are counted as dropped, never after it, and a
// note after Close is counted and dropped. Each noter notes some before
// Close begins (they must all be written), some racing it, and some after
// it returns (they must all be dropped), so both outcomes happen every run;
// and each noter's written notes are a prefix of what it noted, since the
// cutoff never lets a later note through after refusing an earlier one.
func TestEventLogNoNoteEverFollowsClosing(t *testing.T) {
	const noters, before, racing, after = 4, 50, 150, 20
	const each = before + racing + after
	l, w := newJournaledLog(t, EventLogOptions{})
	l.Note(journal.PromptNote{Attempt: "a1", Kind: journal.PromptKindPrompt, Text: "hello"})
	l.Publish(context.Background(), nil, textEvent("x"))
	var noted sync.WaitGroup // every noter is past its first phase
	race, closed := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	for n := range noters {
		noted.Add(1)
		wg.Go(func() {
			for i := range each {
				switch i {
				case before:
					noted.Done()
					<-race
				case before + racing:
					<-closed
				}
				l.Note(journal.DiagNote{Kind: journal.DiagAgentStderr, Fields: map[string]any{"noter": n, "i": i}})
			}
		})
	}
	waitDone(t, &noted)
	close(race)
	closeLog(t, l, w)
	close(closed)
	waitDone(t, &wg)
	l.Note(journal.PromptEndNote{Attempt: "a1"})

	lines := fileLines(t, w)
	assertClosingIsLast(t, lines)
	stderr := diags(lines, journal.DiagAgentStderr)
	written, dropped := len(stderr), l.Health().NotesDropped
	if written < noters*before || dropped < noters*after+1 {
		t.Fatalf("%d notes written and %d dropped: want at least the %d noted before Close written and the %d after it dropped",
			written, dropped, noters*before, noters*after+1)
	}
	if written+dropped != noters*each+1 {
		t.Fatalf("%d notes written and %d dropped, want %d in all", written, dropped, noters*each+1)
	}
	next := make([]int, noters)
	for _, f := range stderr {
		n, i := int(f["noter"].(float64)), int(f["i"].(float64))
		if i != next[n] {
			t.Fatalf("noter %d's note %d was written after its note %d: the cutoff let a later note through", n, i, next[n])
		}
		next[n]++
	}
	for _, line := range lines {
		if line["type"] == "prompt_end" {
			t.Fatal("a note after Close was written")
		}
	}
}

// TestEventLogCloseOnANeverUsedLogIsSafeAndLeavesNothing: a log built and
// closed without an event leaves no journal file and no directory, and one
// with no journal closes with a nil ctx too.
func TestEventLogCloseOnANeverUsedLogIsSafeAndLeavesNothing(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	closeLog(t, l, w)
	dir := filepath.Dir(filepath.Dir(w.Path())) // the journal directory, above the workspace's slug
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("closing an unused log left %s behind (%v)", dir, err)
	}
	if n := l.liveOwners.Load(); n != 0 {
		t.Fatalf("%d owner goroutines after closing an unused log", n)
	}
	bare := NewEventLog(EventLogOptions{})
	within(t, "Close on a log with no journal", func() { bare.Close(context.Background()) })
}

// TestEventLogARetainedRecordNeverChanges is A11: a record a subscriber holds
// — a live one, a replayed one, one with a body and one omitted — is
// unchanged after later publishes and after the ring has evicted it, and
// what a subscriber does to its own record's Omitted reaches no other
// holder.
func TestEventLogARetainedRecordNeverChanges(t *testing.T) {
	l := newTestLog(t, EventLogOptions{RingEvents: 8, MaxRecordBytes: 4096})
	keepDrained(t, l)
	a := mustSubscribe(t, l, SubscribeOptions{MaxItems: 1000})
	b := mustSubscribe(t, l, SubscribeOptions{MaxItems: 1000})
	l.Publish(context.Background(), nil, textEvent("kept"))
	l.Publish(context.Background(), nil, Event{Type: EventPlan, At: logTestTime, Plan: &PlanEvent{Plan: strings.Repeat("p", 8000)}})
	replay := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}, MaxItems: 1000})
	type snapshot struct {
		rec     Record
		body    string
		omitted *Omitted
	}
	snap := func(r Record) snapshot {
		s := snapshot{rec: r, body: strings.Clone(r.Body)}
		if r.Omitted != nil {
			o := *r.Omitted
			s.omitted = &o
		}
		return s
	}
	var held []snapshot
	for _, s := range []*Subscription{a, b, replay} {
		for _, r := range readN(t, s, 2) {
			held = append(held, snap(r))
		}
	}
	// Subscriber a tampers with its own copy of the omitted record.
	held[1].rec.Omitted.Reason, held[1].rec.Omitted.Error = "tampered", "tampered"
	held[1].omitted = &Omitted{Reason: "tampered", Error: "tampered", Bytes: held[1].omitted.Bytes}

	for i := range 100 {
		l.Publish(context.Background(), nil, textEvent(fmt.Sprintf("later %d with other bytes", i)))
	}
	for _, s := range []*Subscription{a, b, replay} {
		readN(t, s, 100)
	}
	for i, h := range held {
		r := h.rec
		if r.Body != h.body || r.Seq != uint64(i%2+1) || !r.At.Equal(logTestTime) {
			t.Fatalf("held record %d changed: seq %d body %q", i, r.Seq, r.Body)
		}
		if (r.Omitted == nil) != (h.omitted == nil) || r.Omitted != nil && *r.Omitted != *h.omitted {
			t.Fatalf("held record %d's Omitted changed: %+v, want %+v", i, r.Omitted, h.omitted)
		}
	}
	for _, i := range []int{3, 5} {
		if held[i].rec.Omitted.Reason != journal.OmittedOversized {
			t.Fatalf("subscriber a's tampering reached holder %d: %+v", i, held[i].rec.Omitted)
		}
	}
	if recordTexts(t, []Record{held[0].rec})[0] != "kept" {
		t.Fatal("a held body no longer decodes to what was published")
	}
}

// TestEventLogOmittedRecordsAreTheSameInLiveRingAndFile is A12's live, ring
// and file parts: an oversized event and two events the codec refuses each
// reach the primary whole, and appear as the same omitted record — seq,
// type, time and marker — in a live subscription, a ring replay and the
// journal file, with the sequence contiguous around them. Each is counted
// and noted once.
func TestEventLogOmittedRecordsAreTheSameInLiveRingAndFile(t *testing.T) {
	const maxRecord = 4096
	l, w := newJournaledLog(t, EventLogOptions{MaxRecordBytes: maxRecord})
	live := mustSubscribe(t, l, SubscribeOptions{})
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := strings.Repeat("a plan line\n", 1000)
	events := []Event{
		textEvent("before"),
		{Type: EventPlan, At: logTestTime, Plan: &PlanEvent{ID: "plan-1", Plan: plan}},
		{Type: EventTool, At: logTestTime, Tool: &ToolEvent{ID: "call-1", At: far}},
		{At: logTestTime, Text: "no type"},
		textEvent("after"),
	}
	for _, ev := range events {
		if !l.Publish(context.Background(), nil, ev) {
			t.Fatal("Publish returned false")
		}
	}
	prim := drainPrimary(l)
	if len(prim) != len(events) || prim[1].Plan.Plan != plan || !prim[2].Tool.At.Equal(far) || prim[3].Text != "no type" {
		t.Fatal("the primary did not receive every event whole")
	}
	liveRecs := readN(t, live, len(events))
	ring := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}})
	ringRecs := readN(t, ring, len(events))
	closeLog(t, l, w)
	fileRecs := fileRecords(t, w)

	planBody, err := EncodeEvent(events[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(planBody) <= maxRecord {
		t.Fatalf("the plan's body is %d bytes, not over the %d-byte cap", len(planBody), maxRecord)
	}
	for _, where := range []struct {
		name string
		recs []Record
	}{{"live", liveRecs}, {"ring replay", ringRecs}, {"journal file", fileRecs}} {
		assertRun(t, where.name, where.recs, 1, len(events))
		for i, r := range where.recs {
			if r.Type != events[i].Type || !r.At.Equal(logTestTime) {
				t.Fatalf("%s seq %d: envelope %q at %v", where.name, r.Seq, r.Type, r.At)
			}
			lr := liveRecs[i]
			if r.Body != lr.Body || (r.Omitted == nil) != (lr.Omitted == nil) || r.Omitted != nil && *r.Omitted != *lr.Omitted {
				t.Fatalf("%s seq %d differs from the live record:\n %+v %+v\nwant\n %+v %+v", where.name, r.Seq, r, r.Omitted, lr, lr.Omitted)
			}
		}
	}
	if o := liveRecs[1].Omitted; o == nil || o.Reason != journal.OmittedOversized || o.Bytes != len(planBody) || o.Error != "" {
		t.Fatalf("the oversized plan's record is %+v, want oversized with %d bytes", o, len(planBody))
	}
	for _, i := range []int{2, 3} {
		if o := liveRecs[i].Omitted; o == nil || o.Reason != journal.OmittedEncodeError || o.Error == "" || o.Bytes != 0 {
			t.Fatalf("seq %d's record is %+v, want an encode_error with its message", i+1, o)
		}
		if liveRecs[i].Body != "" {
			t.Fatalf("seq %d's omitted record has a body", i+1)
		}
	}
	for _, r := range liveRecs {
		if _, err := r.Event(); (err != nil) != (r.Omitted != nil) {
			t.Fatalf("seq %d: Event() error %v with Omitted %+v", r.Seq, err, r.Omitted)
		}
	}
	if h := l.Health(); h.Omitted != 3 || h.Journal.Omitted != 3 {
		t.Fatalf("health counts %d omitted in the log and %d in the journal, want 3", h.Omitted, h.Journal.Omitted)
	}
	noted := diags(fileLines(t, w), journal.DiagRecordOmitted)
	if len(noted) != 3 {
		t.Fatalf("%d record_omitted notes, want 3", len(noted))
	}
	for i, n := range noted {
		if n["seq"] != float64(i+2) || n["reason"] != liveRecs[i+1].Omitted.Reason {
			t.Fatalf("record_omitted note %d is %v", i, n)
		}
	}
}

// TestEventLogOmittedRecordsAreTheSameInAFileReplay is A12's file-replay leg:
// an oversized event, one the codec refuses and one whose type is too long to
// be a type each reach the primary whole, and a resume whose head has left the
// ring and is served from the journal file carries exactly the records a live
// subscription was handed — same seq, type, time and marker — with the
// sequence contiguous around them.
func TestEventLogOmittedRecordsAreTheSameInAFileReplay(t *testing.T) {
	const maxRecord, ring, filler = 4096, 4, 20
	l, w := newJournaledLog(t, EventLogOptions{MaxRecordBytes: maxRecord, RingEvents: ring})
	keepDrained(t, l)
	file := holdJournal(t, l, false)
	live := mustSubscribe(t, l, SubscribeOptions{})
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) // no form the journal can write back
	long := EventType(strings.Repeat("t", journal.MaxEventTypeBytes+1))
	events := []Event{
		textEvent("before"),
		{Type: EventPlan, At: logTestTime, Plan: &PlanEvent{ID: "plan-1", Plan: strings.Repeat("a plan line\n", 1000)}},
		{Type: EventTool, At: logTestTime, Tool: &ToolEvent{ID: "call-1", At: far}},
		{Type: long, At: logTestTime, Text: "wordy"},
		textEvent("after"),
	}
	for _, ev := range events {
		publishWithin(t, l, ev)
	}
	// Push them all out of the ring, so the resume's head must come from the
	// file rather than from a pinned record.
	for i := range filler {
		publishWithin(t, l, textEvent(fmt.Sprintf("filler %d", i+1)))
	}
	total := len(events) + filler
	liveRecs := readN(t, live, total)
	if n := len(liveRecs) - ring; n < len(events) {
		t.Fatalf("only %d records left the ring, want at least the %d that are omitted", n, len(events))
	}
	flushThrough(t, w, uint64(total))

	replay := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}})
	fileRecs := readN(t, replay, total)
	if n := file.reads.Load(); n != 1 {
		t.Fatalf("the journal file was read %d times, want once", n)
	}
	assertRun(t, "the file replay", fileRecs, 1, total)
	for i, r := range fileRecs {
		lr := liveRecs[i]
		if r.Seq != lr.Seq || r.Type != lr.Type || !r.At.Equal(lr.At) || r.Body != lr.Body {
			t.Fatalf("seq %d from the file is %s at %v with %d body bytes; live it was %s at %v with %d",
				r.Seq, r.Type, r.At, len(r.Body), lr.Type, lr.At, len(lr.Body))
		}
		if (r.Omitted == nil) != (lr.Omitted == nil) || r.Omitted != nil && *r.Omitted != *lr.Omitted {
			t.Fatalf("seq %d's marker from the file is %+v, live it was %+v", r.Seq, r.Omitted, lr.Omitted)
		}
	}
	// The three omissions are the ones A12 names, and they are omitted
	// records in the file replay too, not full bodies.
	for i, want := range map[int]string{1: journal.OmittedOversized, 2: journal.OmittedEncodeError, 3: journal.OmittedEncodeError} {
		if o := fileRecs[i].Omitted; o == nil || o.Reason != want || fileRecs[i].Body != "" {
			t.Fatalf("seq %d in the file replay is %+v with %d body bytes, want an omitted %s record", i+1, o, len(fileRecs[i].Body), want)
		}
	}
	if h := l.Health(); h.Omitted != 3 {
		t.Fatalf("the log counts %d omitted records, want 3", h.Omitted)
	}
}

// TestEventLogTakesTheJournalsSmallerRecordLimit: the log and the journal
// each have a MaxRecordBytes, and a journal built with the smaller one lowers
// the log's. An event between the two limits is then the same oversized
// record in a ring replay, a live subscription and the file, and the log
// counts and notes the omission — rather than only the file omitting it,
// and a file replay disagreeing with a ring replay.
func TestEventLogTakesTheJournalsSmallerRecordLimit(t *testing.T) {
	const journalMax = 4096
	w, inc := newTestJournalWith(t, func(o *journal.Options) { o.MaxRecordBytes = journalMax })
	l := newTestLog(t, EventLogOptions{Journal: w, Incarnation: inc})
	live := mustSubscribe(t, l, SubscribeOptions{})
	big := textEvent(strings.Repeat("x", 5<<10))
	events := []Event{textEvent("before"), big, textEvent("after")}
	for _, ev := range events {
		publishWithin(t, l, ev)
	}
	if prim := drainPrimary(l); len(prim) != len(events) || prim[1].Text != big.Text {
		t.Fatal("the primary did not receive the large event whole")
	}
	liveRecs := readN(t, live, len(events))
	ringRecs := readN(t, mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: inc}}), len(events))
	closeLog(t, l, w)

	body, err := EncodeEvent(big)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= journalMax || len(body) > defaultMaxRecordBytes {
		t.Fatalf("the large event's body is %d bytes, not between the journal's %d and the log's default %d", len(body), journalMax, defaultMaxRecordBytes)
	}
	want := Omitted{Reason: journal.OmittedOversized, Bytes: len(body)}
	for _, where := range []struct {
		name string
		recs []Record
	}{{"the ring replay", ringRecs}, {"the live subscription", liveRecs}, {"the journal file", fileRecords(t, w)}} {
		assertRun(t, where.name, where.recs, 1, len(events))
		if r := where.recs[1]; r.Omitted == nil || *r.Omitted != want || r.Body != "" || r.Type != EventText {
			t.Fatalf("%s: seq 2 is %s with %d body bytes and Omitted %+v, want the oversized record %+v", where.name, r.Type, len(r.Body), r.Omitted, want)
		}
		for _, i := range []int{0, 2} {
			if r := where.recs[i]; r.Omitted != nil || r.Body != liveRecs[i].Body {
				t.Fatalf("%s: seq %d differs from the live record", where.name, r.Seq)
			}
		}
	}
	if h := l.Health(); h.Omitted != 1 {
		t.Fatalf("the log counts %d omitted records, want 1", h.Omitted)
	}
	if noted := diags(fileLines(t, w), journal.DiagRecordOmitted); len(noted) != 1 || noted[0]["seq"] != float64(2) {
		t.Fatalf("the record_omitted notes are %v, want one for seq 2", noted)
	}
}

// TestEventLogAnOverlongEventTypeIsTheSameOmittedRecordEverywhere: the
// journal cannot write an event type over its cap — it would make a line no
// reader accepts — so the log applies that rule where the record is built.
// The event still reaches the primary whole; its record is one omitted
// record, the same in a live subscription, a ring replay and the file,
// counted and noted once, rather than a full body in the ring and a
// placeholder in the file.
func TestEventLogAnOverlongEventTypeIsTheSameOmittedRecordEverywhere(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	live := mustSubscribe(t, l, SubscribeOptions{})
	long := EventType(strings.Repeat("t", journal.MaxEventTypeBytes+1))
	events := []Event{textEvent("before"), {Type: long, At: logTestTime, Text: "wordy"}, textEvent("after")}
	for _, ev := range events {
		publishWithin(t, l, ev)
	}
	if prim := drainPrimary(l); len(prim) != len(events) || prim[1].Type != long || prim[1].Text != "wordy" {
		t.Fatal("the primary did not receive the event with the overlong type whole")
	}
	liveRecs := readN(t, live, len(events))
	ringRecs := readN(t, mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}}), len(events))
	closeLog(t, l, w)

	for _, where := range []struct {
		name string
		recs []Record
	}{{"the live subscription", liveRecs}, {"the ring replay", ringRecs}, {"the journal file", fileRecords(t, w)}} {
		assertRun(t, where.name, where.recs, 1, len(events))
		r := where.recs[1]
		if r.Type != journal.OverlongEventType || r.Body != "" || r.Omitted == nil ||
			r.Omitted.Reason != journal.OmittedEncodeError || !strings.Contains(r.Omitted.Error, "event type") {
			t.Fatalf("%s: seq 2 is %q with %d body bytes and Omitted %+v, want an encode_error omitted record under %q",
				where.name, r.Type, len(r.Body), r.Omitted, journal.OverlongEventType)
		}
		if lr := liveRecs[1]; *r.Omitted != *lr.Omitted {
			t.Fatalf("%s: seq 2's marker %+v differs from the live record's %+v", where.name, r.Omitted, lr.Omitted)
		}
		for _, i := range []int{0, 2} {
			if r := where.recs[i]; r.Omitted != nil || r.Body != liveRecs[i].Body || r.Type != EventText {
				t.Fatalf("%s: seq %d differs from the live record", where.name, r.Seq)
			}
		}
	}
	if h := l.Health(); h.Omitted != 1 || h.Journal.Omitted != 1 {
		t.Fatalf("health counts %d omitted in the log and %d in the journal, want 1", h.Omitted, h.Journal.Omitted)
	}
	noted := diags(fileLines(t, w), journal.DiagRecordOmitted)
	if len(noted) != 1 || noted[0]["seq"] != float64(2) || noted[0]["eventType"] != journal.OverlongEventType {
		t.Fatalf("the record_omitted notes are %v, want one for seq 2 under %q", noted, journal.OverlongEventType)
	}
}

// TestEventLogTryPublishNeverWaitsAndDropsForEveryone is A23: TryPublish
// publishes when there is room, and with a wedged primary — full, or held by
// a publisher blocked inside the boundary — returns false at once, takes no
// number, and reaches no subscriber, the ring, or the journal.
func TestEventLogTryPublishNeverWaitsAndDropsForEveryone(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	// Read by the test itself before Close, which discards what a
	// subscription has not delivered.
	watch := mustSubscribe(t, l, SubscribeOptions{MaxItems: 4096})
	if !l.TryPublish(textEvent("tried")) {
		t.Fatal("TryPublish with room returned false")
	}
	for i := 1; i < primaryCap; i++ {
		l.Publish(context.Background(), nil, textEvent(fmt.Sprint(i+1)))
	}
	within(t, "TryPublish on a full primary", func() {
		if l.TryPublish(textEvent("dropped: full")) {
			t.Error("TryPublish on a full primary returned true")
		}
	})
	hook, inside := insideAt(primaryCap + 1)
	l.hooks = &logHooks{beforePrimarySend: hook}
	blocked := make(chan bool, 1)
	go func() { blocked <- l.Publish(context.Background(), nil, textEvent("blocked")) }()
	await(t, inside, "a publisher to block inside the boundary")
	within(t, "TryPublish while a publisher holds the boundary", func() {
		if l.TryPublish(textEvent("dropped: busy")) {
			t.Error("TryPublish past a held boundary returned true")
		}
	})
	evs := []Event{await(t, l.Primary(), "the primary's first event")}
	if !await(t, blocked, "the blocked publisher") {
		t.Fatal("the blocked publisher returned false")
	}
	evs = append(evs, drainPrimary(l)...)
	if !l.TryPublish(textEvent("tried again")) {
		t.Fatal("TryPublish with room returned false")
	}
	evs = append(evs, drainPrimary(l)...)
	const want = primaryCap + 2
	primSeqs := make([]uint64, len(evs))
	for i, ev := range evs {
		primSeqs[i] = ev.Seq
		if strings.HasPrefix(ev.Text, "dropped") {
			t.Fatalf("%q reached the primary", ev.Text)
		}
	}
	if p := runSeqs(primSeqs, 1); p != "" || len(evs) != want {
		t.Fatalf("the primary has %d events: %s", len(evs), p)
	}
	if evs[0].Text != "tried" || evs[want-2].Text != "blocked" || evs[want-1].Text != "tried again" {
		t.Fatalf("the primary's events are out of place: %q %q %q", evs[0].Text, evs[want-2].Text, evs[want-1].Text)
	}
	replay := mustSubscribe(t, l, SubscribeOptions{After: &Cursor{Incarnation: l.Incarnation()}})
	ringRecs := readN(t, replay, want)
	watched := readN(t, watch, want)
	closeLog(t, l, w)
	watched = append(watched, readAll(t, watch)...)
	for _, where := range []struct {
		name string
		recs []Record
	}{{"the ring", ringRecs}, {"the watching subscriber", watched}, {"the journal", fileRecords(t, w)}} {
		assertRun(t, where.name, where.recs, 1, want)
		assertNoText(t, where.name, recordTexts(t, where.recs), "dropped: full", "dropped: busy")
	}
	if n := l.Health().DroppedAtClose; n != 0 {
		t.Fatalf("DroppedAtClose is %d: a TryPublish that found no room is not a drop at close", n)
	}
}

// TestSubscriptionEndsRatherThanDeliverAHole: the owner's own guard. Handed
// a replay with a hole in it — which nothing in the log produces — it
// delivers up to the hole and ends with the error, never the record after
// it.
func TestSubscriptionEndsRatherThanDeliverAHole(t *testing.T) {
	l := newTestLog(t, EventLogOptions{})
	body := func(n int) string { return fmt.Sprintf(`{"type":"text","text":"%d"}`, n) }
	pinned := []Record{
		{Seq: 11, Type: EventText, Body: body(11)},
		{Seq: 12, Type: EventText, Body: body(12)},
		{Seq: 14, Type: EventText, Body: body(14)},
	}
	s := l.newSubscription(defaultSubscribeItems, defaultSubscribeBytes, 0)
	l.startOwner(s, nil, pinned, 10)
	recs := readAll(t, s)
	assertRun(t, "the delivered records", recs, 11, 2)
	if !errors.Is(s.Err(), errNotContiguous) {
		t.Fatalf("the subscription ended with %v, want the contiguity error", s.Err())
	}
}

// TestRecordRingEvictsByCountAndBytesAndKeepsItsOrder: the ring stays
// contiguous and ends at the newest record through growth and wrap-around;
// it evicts by count and by bytes, keeps the newest record even when that
// one alone is over the byte bound, and locates the records after a seq.
func TestRecordRingEvictsByCountAndBytesAndKeepsItsOrder(t *testing.T) {
	r := recordRing{maxItems: 40, maxBytes: 1000}
	check := func(what string, last uint64, n int) {
		t.Helper()
		if r.n != n {
			t.Fatalf("%s: %d records, want %d", what, r.n, n)
		}
		total := 0
		for i := range r.n {
			rec := r.at(i)
			total += rec.size()
			if want := last - uint64(r.n-1-i); rec.Seq != want {
				t.Fatalf("%s: position %d is seq %d, want %d", what, i, rec.Seq, want)
			}
		}
		if total != r.bytes {
			t.Fatalf("%s: bytes %d, counted %d", what, r.bytes, total)
		}
	}
	rec := func(seq uint64, size int) Record {
		return Record{Seq: seq, Type: EventText, Body: strings.Repeat("x", size)}
	}
	var seq uint64
	for range 100 {
		seq++
		r.push(rec(seq, 10))
	}
	check("by count", seq, 40)
	if len(r.buf) != 40 {
		t.Fatalf("the buffer grew to %d, past its bound of 40", len(r.buf))
	}
	seq++
	r.push(rec(seq, 700)) // 39 records of 10 bytes plus 700 is over 1000
	check("by bytes", seq, 31)
	seq++
	r.push(rec(seq, 5000))
	check("one record over the bound on its own", seq, 1)
	for range 5 {
		seq++
		r.push(rec(seq, 10))
	}
	check("after the large one is evicted", seq, 5)

	skip, count, bytes := r.after(seq - 3)
	if skip != 2 || count != 3 || bytes != 30 {
		t.Fatalf("after(%d) = %d, %d, %d; want 2, 3, 30", seq-3, skip, count, bytes)
	}
	if _, count, _ := r.after(seq); count != 0 {
		t.Fatalf("after the newest seq: %d records", count)
	}
	skip, count, _ = r.after(1)
	if skip != 0 || count != 5 {
		t.Fatalf("after a seq older than the ring: %d, %d", skip, count)
	}
	got := r.copyOut(skip, count)
	r.push(rec(seq+1, 10000)) // evicts everything the copy holds
	check("after evicting what was copied", seq+1, 1)
	for i, g := range got {
		if g.Seq != seq-4+uint64(i) || len(g.Body) != 10 {
			t.Fatalf("a pinned copy changed under eviction: %d is seq %d", i, g.Seq)
		}
	}
}

// BenchmarkEventLogPublish is V4: Publish's cost on the emit path, encoding
// included, for a streamed text delta and for a large tool event (8 KiB of
// output and a diff), without and with a journal attached. The primary is
// drained by a goroutine, as a session's reader would. Budget: a text delta
// under 5 µs with the journal attached.
func BenchmarkEventLogPublish(b *testing.B) {
	tail := strings.Repeat("output line <with> & some text\n", outputTailCap/32)
	cases := []struct {
		name string
		ev   Event
	}{
		{"TextDelta", Event{Type: EventText, Text: "Here is the next chunk of the answer, ", At: time.Now()}},
		{"LargeTool", Event{Type: EventTool, At: time.Now(), Tool: &ToolEvent{
			ID: "call-1", Title: "Run make", Status: "completed", Kind: "execute", RawInput: "make",
			Output: &ToolOutput{ExitCode: ptr(1), Stdout: tail, StdoutHead: tail[:outputHeadCap], Truncated: true},
			Diffs:  []ToolDiff{{Path: "/a.go", OldText: tail, NewText: tail + "x", Added: 1}},
			At:     time.Now(),
		}}},
	}
	for _, c := range cases {
		for _, journaled := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/journal=%v", c.name, journaled), func(b *testing.B) {
				var w *journal.Writer
				inc := ""
				if journaled {
					w, inc = newTestJournal(b)
				}
				l := newTestLog(b, EventLogOptions{Journal: w, Incarnation: inc})
				keepDrained(b, l)
				b.ReportAllocs()
				for b.Loop() {
					if !l.Publish(context.Background(), nil, c.ev) {
						b.Fatal("Publish returned false")
					}
				}
				b.StopTimer()
				closeLog(b, l, w)
				if w != nil {
					// A writer that cannot keep up sheds records into gap
					// lines rather than slow Publish; say how much it shed,
					// since that changes what the journal's cost includes.
					b.ReportMetric(float64(w.Health().DroppedEvents)/float64(b.N), "journal-drops/op")
				}
			})
		}
	}
}
