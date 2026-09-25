package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/sessions"
)

// ------------------------------------------------------------------ the double

// fakeIndex is engine.Index as a test holds it: every row it was handed, an
// error it can be made to fail with, and a park at Upsert's entry, which is
// what a contended file lock looks like from up here (sessions.Store.Upsert
// takes an flock, and flock has no timeout).
//
// Every wait on it is a barrier. Nothing here sleeps.
type fakeIndex struct {
	mu       sync.Mutex
	rows     []sessions.Row
	err      error
	attempts int
	// failCall fails one numbered call and no other: the TRANSIENT failure the
	// retry rules are about, where err fails every call.
	failCall map[int]error
	// panicCall PANICS in one numbered call: a store — or anything else the
	// write calls into — blowing up under a caller that recovers, which is a
	// different exit from the write than an error is (r31 finding 3).
	panicCall map[int]bool
	// live is how many Upserts are inside this double right now, and peak the
	// most there have ever been: "one engine never runs two of these at once"
	// is a claim about the worker that only counting can check.
	live int
	peak int
	// parkFrom is the call number from which Upsert waits for release; 0 never
	// parks. entered is closed by the first call that parks, so a test knows the
	// write really is in flight.
	parkFrom int
	entered  chan struct{}
	release  chan struct{}
	// wrote carries one token per completed call, buffered past anything these
	// tests do so Upsert never blocks on it.
	wrote chan struct{}
}

func newFakeIndex() *fakeIndex {
	return &fakeIndex{
		failCall:  map[int]error{},
		panicCall: map[int]bool{},
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		wrote:     make(chan struct{}, 64),
	}
}

func (f *fakeIndex) Upsert(row sessions.Row) error {
	f.mu.Lock()
	f.attempts++
	n, from := f.attempts, f.parkFrom
	f.live++
	if f.live > f.peak {
		f.peak = f.live
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.live--
		f.mu.Unlock()
	}()
	if from > 0 && n >= from {
		if n == from {
			close(f.entered)
		}
		<-f.release
	}
	f.mu.Lock()
	boom := f.panicCall[n]
	f.mu.Unlock()
	if boom {
		// After the park, so a test can hold the write open and let it blow up
		// at the moment its schedule wants.
		panic("fakeIndex: the store blew up in Upsert")
	}
	f.mu.Lock()
	err := f.err
	if e, ok := f.failCall[n]; ok {
		err = e
	}
	if err == nil {
		f.rows = append(f.rows, row)
	}
	f.mu.Unlock()
	select {
	case f.wrote <- struct{}{}:
	default:
	}
	return err
}

// failAt makes the n-th call, and only it, fail: an index that is busy for one
// write and free for the next.
func (f *fakeIndex) failAt(n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCall[n] = err
}

// panicAt makes the n-th call, and only it, PANIC inside Upsert.
func (f *fakeIndex) panicAt(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panicCall[n] = true
}

// peakLive is the most Upserts this double has ever held at one time.
func (f *fakeIndex) peakLive() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

// parkAt arms the park: from the n-th call on, an Upsert waits until the
// returned release is called, and entered closes when the n-th one has arrived.
func (f *fakeIndex) parkAt(n int) (entered <-chan struct{}, release func()) {
	f.mu.Lock()
	f.parkFrom = n
	f.mu.Unlock()
	var once sync.Once
	return f.entered, func() { once.Do(func() { close(f.release) }) }
}

func (f *fakeIndex) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeIndex) all() []sessions.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sessions.Row(nil), f.rows...)
}

func (f *fakeIndex) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func (f *fakeIndex) tries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// last is the newest row, and the zero Row when nothing has been written.
func (f *fakeIndex) last() sessions.Row {
	rows := f.all()
	if len(rows) == 0 {
		return sessions.Row{}
	}
	return rows[len(rows)-1]
}

// seedRow is the one row that carries a first-prompt title. A test about what a
// title SAYS asks for this rather than last(), because a turn's own end touches
// the row afterwards on the worker, whenever the worker gets to it.
func (f *fakeIndex) seedRow(t *testing.T) sessions.Row {
	t.Helper()
	for _, row := range f.all() {
		if row.TitleKind == sessions.TitleKindFallback {
			return row
		}
	}
	t.Fatalf("no first-prompt row was written: %+v", f.all())
	return sessions.Row{}
}

// seeds is how many first-prompt rows were written: the "later prompts do not
// rewrite the file for a title no rule would keep" guarantee, countable without
// racing a turn's end.
func (f *fakeIndex) seeds() int {
	n := 0
	for _, row := range f.all() {
		if row.TitleKind == sessions.TitleKindFallback {
			n++
		}
	}
	return n
}

// waitRows blocks, with the watchdog, until at least n rows have been written.
func (f *fakeIndex) waitRows(t *testing.T, n int) {
	t.Helper()
	f.waitFor(t, n, f.count, "rows")
}

// waitTries is waitRows for CALLS, which is what a test of a failing index has
// to wait on: a failed write records no row.
func (f *fakeIndex) waitTries(t *testing.T, n int) {
	t.Helper()
	f.waitFor(t, n, f.tries, "attempts")
}

func (f *fakeIndex) waitFor(t *testing.T, n int, have func() int, what string) {
	t.Helper()
	timeout := time.After(watchdog)
	for have() < n {
		select {
		case <-f.wrote:
		case <-timeout:
			t.Fatalf("the index reached %d %s in %s, want %d: %+v", have(), what, watchdog, n, f.all())
		}
	}
}

// indexed is a rig whose engine writes an index: the options every test here
// shares, with a provider the hidden predicate accepts.
func indexed(t *testing.T, idx Index, craze string) *rig {
	t.Helper()
	return indexedHooked(t, idx, craze, nil)
}

// indexedHooked is indexed with the engine's own seams in place, which is the
// only way the goroutines that read them may be given any (newRigHooked).
func indexedHooked(t *testing.T, idx Index, craze string, h *hooks) *rig {
	t.Helper()
	return newRigHooked(t, Options{
		CrazeSessionID: craze,
		Index: IndexOptions{
			Store:     idx,
			CWD:       "/w",
			Provider:  "cursor",
			Hidden:    func(p string) bool { return p == "native" },
			TitleLine: func(s string) string { return strings.TrimSpace(s) },
		},
	}, agent.EventLogOptions{NoPrimary: true}, h)
}

// writerOn is the index worker ALONE, with no engine above it: the unit whose
// properties are the shape of a pass and the shape of a shutdown, where an
// engine can only arrange those through events. The snapshot is a session that
// has learned its id; reported is every failure the writer published, as
// "cause: message".
//
// closeWait is set past the watchdog, because the drain and not the bound is
// what these tests are about: the one test about the bound sets it back to the
// production value.
func writerOn(t *testing.T, idx Index) (w *indexWriter, reported func() []string) {
	t.Helper()
	var mu sync.Mutex
	var out []string
	w = newIndexWriter(
		IndexOptions{
			Store: idx, CWD: "/w", Provider: "cursor",
			TitleLine: func(s string) string { return strings.TrimSpace(s) },
		},
		"018f-the-thread",
		func() agent.Snapshot { return agent.Snapshot{SessionID: "fake-1"} },
		func(cause, msg string) {
			mu.Lock()
			defer mu.Unlock()
			out = append(out, cause+": "+msg)
		},
	)
	w.closeWait = watchdog
	t.Cleanup(w.stop)
	return w, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), out...)
	}
}

// wrote is the row a pass wrote for kind, and the zero Row for a kind nothing
// wrote: the whole row, because what a test of an index write is about is what
// a picker will read.
func wrote(craze, title string, kind sessions.TitleKind) sessions.Row {
	return sessions.Row{
		SessionID: "fake-1", Provider: "cursor", CWD: "/w",
		CrazeID: craze, Title: title, TitleKind: kind,
	}
}

// ---------------------------------------------------------------- the identity

// TestCrazeSessionIDIsCarriedInOrMinted is SD-22's identity: a load's row hands
// its id back and the engine keeps it; a new session gets a fresh UUIDv7. State
// is where a client reads it.
func TestCrazeSessionIDIsCarriedInOrMinted(t *testing.T) {
	loaded := newRig(t, Options{CrazeSessionID: "018f-carried-in"})
	if got := loaded.e.State().CrazeSessionID; got != "018f-carried-in" {
		t.Fatalf("a loaded session's craze id is %q, want the one it was given", got)
	}

	a := newRig(t, Options{})
	b := newRig(t, Options{})
	first, second := a.e.State().CrazeSessionID, b.e.State().CrazeSessionID
	if first == "" || second == "" {
		t.Fatalf("a new session minted no craze id: %q %q", first, second)
	}
	if first == second {
		t.Fatalf("two engines minted the same craze id: %q", first)
	}
	// A UUIDv7: 36 characters, version 7 in the 13th nibble. It is not a format
	// the protocol pins, but a mistake here would be a collision, and the shape
	// is how a reader tells one of these from an incarnation or a provider id.
	if len(first) != 36 || first[14] != '7' {
		t.Fatalf("the minted craze id %q is not a UUIDv7", first)
	}
	// Fixed for the engine's life: a client that cached it is not reading a
	// different session a moment later.
	if again := a.e.State().CrazeSessionID; again != first {
		t.Fatalf("the craze id changed under a running engine: %q then %q", first, again)
	}
}

// TestTheJournalRecordsOneCrazeSessionNote: the durable id is in the record
// exactly once per incarnation, beside the header's incarnation and the
// session note's provider id — the third of SD-22's three identities.
func TestTheJournalRecordsOneCrazeSessionNote(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	w, err := journal.New(journal.Options{
		Dir:         dir,
		Incarnation: agent.NewIncarnation(),
		Cwd:         t.TempDir(),
		Provider:    "test",
		EventCodec:  agent.EventCodecVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := newRigOn(t, Options{CrazeSessionID: "018f-the-thread"},
		agent.EventLogOptions{NoPrimary: true, Journal: w})
	// Run a turn, so the note is not the only thing in the file and its
	// once-ness is a claim about a session that did something.
	r.submit("hello")
	r.until(lastEnding)
	if err := r.e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("closing the journal: %v", err)
	}

	raw, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	var notes []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("journal line %q: %v", line, err)
		}
		if obj["type"] == "diag" && obj["kind"] == journal.DiagCrazeSession {
			notes = append(notes, obj)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("%d craze_session notes, want exactly one:\n%s", len(notes), raw)
	}
	fields, _ := notes[0]["fields"].(map[string]any)
	if got := fields["crazeSessionId"]; got != "018f-the-thread" {
		t.Fatalf("the note records %v, want the session's durable id", got)
	}
	if got := fields["loaded"]; got != true {
		t.Fatalf("the note says loaded=%v for an id that was carried in", got)
	}
}

// ------------------------------------------------------------------ the writes

// TestTheFirstPromptSeedsTheRow: the first prompt is what creates the row, it
// is written on the CALLER's goroutine (so it has happened by the time Submit
// returns), and it carries everything a picker needs — including the durable
// craze id, which is how --continue finds its way back to this thread of work.
func TestTheFirstPromptSeedsTheRow(t *testing.T) {
	idx := newFakeIndex()
	r := indexed(t, idx, "018f-the-thread")
	r.submit("  the first line  \nand a second")
	// Submit has returned, so the seed has happened: that is the whole claim
	// about which goroutine it runs on. (The turn's own end may already have
	// touched the row behind it, on the worker, which is why this reads the
	// rows written so far rather than counting them.)
	rows := idx.all()
	if len(rows) == 0 {
		t.Fatal("Submit returned with nothing written: the seed is not on the caller's goroutine")
	}
	want := sessions.Row{
		SessionID: "fake-1", Provider: "cursor", CWD: "/w",
		CrazeID: "018f-the-thread", Title: "the first line",
		TitleKind: sessions.TitleKindFallback,
	}
	if rows[0] != want {
		t.Fatalf("the seed wrote %+v, want %+v", rows[0], want)
	}

	// A later prompt changes nothing a title rule would keep, so it writes no
	// second fallback title.
	r.until(lastEnding)
	r.submit("a second prompt")
	r.until(lastEnding)
	if n := idx.seeds(); n != 1 {
		t.Fatalf("%d first-prompt rows were written: %+v", n, idx.all())
	}
}

// TestADrainedFirstPromptIsSeededByTheWorker: a drain has no caller's
// goroutine, so its seed is the index worker's — and it is a real first prompt,
// because the one before it failed to write. Both halves of A15 are here: a
// failed seed is retried on the next turn, and every path by which a first
// prompt starts seeds exactly once.
func TestADrainedFirstPromptIsSeededByTheWorker(t *testing.T) {
	idx := newFakeIndex()
	boom := errors.New("the index is busy")
	idx.setErr(boom)
	r, returned := newRigReturning(t, Options{Index: IndexOptions{
		Store: idx, CWD: "/w", Provider: "cursor",
	}})
	turn := r.s.script(held())
	r.submit("the first prompt")
	await(t, turn.opened, "the first turn to open")
	idx.waitTries(t, 1)
	if n := idx.count(); n != 0 {
		t.Fatalf("a failing index recorded %d rows", n)
	}

	// A row behind it, and the index working again: the settlement drains it,
	// and the seed that turn owes is the worker's.
	r.queue("the drained row")
	idx.setErr(nil)
	turn.release()
	// Either turn's report will do: a turn's hook runs after its settlement has
	// launched the successor, so the drained turn-2 can report before turn-1 —
	// and its report already proves turn-1 settled. The write is the barrier.
	awaitTurn(t, returned, "")
	idx.waitRows(t, 1)
	row := idx.seedRow(t)
	if row.Title != "the drained row" {
		t.Fatalf("the drain's seed wrote %+v", row)
	}
	if row.CrazeID != r.e.State().CrazeSessionID {
		t.Fatalf("the drain's seed carried craze id %q, want %q", row.CrazeID, r.e.State().CrazeSessionID)
	}
}

// TestAFailedSeedIsReportedOnceWithItsCause: Submit has already answered with
// its turn by the time the seed runs, so the failure cannot be its return
// value. It goes out as a StateDelta{IndexErr} naming the command that caused
// it — one per failure, which is what a client draws its one error row from.
//
// The report and the turn's ending come in either order: runOwn launches the
// turn before it makes the inline seed (r31 finding 1), so a Submit whose own
// goroutine is descheduled between the two lets a turn as short as the fake's
// end first. The test once waited for the report and then for an ending after
// it, and failed on a loaded CI runner (plan 026 X47); the second schedule is
// now forced through beforeInlineSeed.
func TestAFailedSeedIsReportedOnceWithItsCause(t *testing.T) {
	for _, tc := range []struct {
		name      string
		endsFirst bool // hold the Submit's inline seed until the turn has ended
	}{{"the report as it comes", false}, {"the turn ending first", true}} {
		t.Run(tc.name, func(t *testing.T) {
			idx := newFakeIndex()
			boom := errors.New("craze: no home directory to save the session index in")
			idx.setErr(boom)
			ended := make(chan struct{})
			var h *hooks
			if tc.endsFirst {
				h = &hooks{beforeInlineSeed: func(string) { <-ended }}
			}
			r := indexedHooked(t, idx, "", h)
			c := Command{Client: r.e.NewClientID(), ID: "1"}
			submitted := make(chan struct{})
			go func() {
				defer close(submitted)
				if _, err := r.e.Submit(c, "a prompt", SubmitQueue, ""); err != nil {
					t.Errorf("a failing index refused the prompt: %v", err)
				}
			}()
			if !tc.endsFirst {
				close(ended)
			}
			// Everything up to the later of the two: the report and the turn's
			// ending, in whichever order they came.
			var reports []agent.Event
			sawEnding := false
			got := r.until(func(ev agent.Event) bool {
				if ev.Type == agent.EventMeta && ev.State != nil && ev.State.IndexErr != "" {
					reports = append(reports, ev)
				}
				if lastEnding(ev) {
					sawEnding = true
					if tc.endsFirst {
						close(ended)
					}
				}
				return sawEnding && len(reports) > 0
			})
			<-submitted
			// One report per failure, not one per event.
			if len(reports) != 1 {
				t.Fatalf("%d reports for one failed write: %s", len(reports), describe(got))
			}
			if got := reports[0].State.IndexErr; got != boom.Error() {
				t.Fatalf("the delta carries %q, want the store's own message", got)
			}
			if reports[0].Cause != c.Cause() {
				t.Fatalf("the delta names %q, want the command that caused the write (%q)", reports[0].Cause, c.Cause())
			}
			if tc.endsFirst && !lastEnding(got[len(got)-2]) {
				t.Fatalf("the forced schedule did not put the ending first: %s", describe(got))
			}
		})
	}
}

// overlappingSeeds runs the schedule r29 finding 2 is about, on the rig idx
// backs: turn A's Submit parks in its seed, A's turn ends while that caller is
// still inside the write, and turn B is then submitted and its seed finds A's
// in flight. It returns the two commands and the barrier that says A's Submit
// has come back, with A's write still parked — the caller releases it.
func overlappingSeeds(t *testing.T, r *rig, idx *fakeIndex, entered <-chan struct{}) (a, b Command, submitted <-chan struct{}) {
	t.Helper()
	a = Command{Client: r.e.NewClientID(), ID: "1"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := r.e.Submit(a, "the first prompt", SubmitQueue, ""); err != nil {
			t.Errorf("submit A: %v", err)
		}
	}()
	await(t, entered, "A's seed to park in Upsert")
	// A's turn runs and ends while its caller is still inside that write. The
	// touch it owes finds no row and writes nothing, so the session is still
	// unindexed.
	r.until(lastEnding)

	b = Command{Client: a.Client, ID: "2"}
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		if _, err := r.e.Submit(b, "the second prompt", SubmitQueue, ""); err != nil {
			t.Errorf("submit B: %v", err)
		}
	}()
	// B's caller is not made to wait for A's write: that is the half of the fix
	// that must not cost anything.
	await(t, returned, "B's Submit to return while A's seed is parked")
	r.until(lastEnding)
	if n := idx.tries(); n != 1 {
		t.Fatalf("%d writes with A's still parked, want only A's", n)
	}
	return a, b, done
}

// TestAnOverlappingTurnRetriesAFailedSeed is r29 finding 2. "A failed seed is
// retried by the next turn" was not true of turns that OVERLAP: B's seed found
// A's parked in Upsert, did nothing, and was gone by the time A failed — so a
// session that had run two turns had no row at all, and only a THIRD turn could
// give it one. B is now retained as the one deferred opportunity and posted to
// the worker when A fails.
func TestAnOverlappingTurnRetriesAFailedSeed(t *testing.T) {
	idx := newFakeIndex()
	entered, release := idx.parkAt(1)
	t.Cleanup(release)
	boom := errors.New("craze: not saving the session: permission denied")
	// Transient: A's write fails, and the retry that follows it lands.
	idx.failAt(1, boom)
	r := indexed(t, idx, "018f-the-thread")

	a, b, submitted := overlappingSeeds(t, r, idx, entered)
	release()
	await(t, submitted, "A's Submit to return once its write is let go")

	idx.waitRows(t, 1)
	row := idx.seedRow(t)
	if got, want := row, wrote("018f-the-thread", "the second prompt", sessions.TitleKindFallback); got != want {
		t.Fatalf("the retry wrote %+v, want %+v", got, want)
	}
	if n := idx.seeds(); n != 1 {
		t.Fatalf("%d first-prompt rows: %+v", n, idx.all())
	}

	// One failure, A's, and it names A's command: B's seed ran on the worker
	// and landed, so there is nothing else to report.
	got := r.until(func(ev agent.Event) bool {
		return ev.Type == agent.EventMeta && ev.State != nil && ev.State.IndexErr != ""
	})
	last := got[len(got)-1]
	if last.State.IndexErr != boom.Error() {
		t.Fatalf("the delta carries %q, want the store's own message", last.State.IndexErr)
	}
	if last.Cause != a.Cause() {
		t.Fatalf("the delta names %q, want A's command (%q)", last.Cause, a.Cause())
	}
	if last.Cause == b.Cause() {
		t.Fatalf("the delta names B's command, but it was A's write that failed")
	}
}

// TestClosingWaitsForAnInlineSeedThatOwesARetry is r30 finding 2, and the one
// piece of owed work the worker's slot cannot show. A's seed is in flight on a
// CLIENT's goroutine — Submit's own inline write, §3.2's documented exception —
// and B is retained behind it; B only reaches the slot if A FAILS. The worker's
// exit used to look at the slot, find a plain touch, and go: A then failed with
// nobody left to hand B to, and a session that had run two prompts had no index
// row at all, so --continue could not find it.
//
// The exit now waits for the attempt in flight whenever an opportunity is
// retained — inside the same 500 ms bound as everything else it waits for — and
// makes the retry its last write.
func TestClosingWaitsForAnInlineSeedThatOwesARetry(t *testing.T) {
	idx := newFakeIndex()
	entered, release := idx.parkAt(1)
	t.Cleanup(release)
	boom := errors.New("craze: not saving the session: permission denied")
	// Transient: A's write fails, and the retry the exit makes for it lands.
	idx.failAt(1, boom)
	r := indexed(t, idx, "018f-the-thread")
	// The bound is not what this is about — TestCloseAbandonsALastWriteNothingCanInterrupt
	// is — so it is set past the watchdog, as writerOn's own is. The barrier is
	// what makes this the SHUTDOWN's schedule and not a race with an ordinary
	// pass: released before the exit had begun, A's failure would simply kick a
	// worker that is still running, and the retry it starts would be the
	// ordinary in-flight write that close is entitled to ABANDON. Both fields
	// are written here and never again, and the worker reads them only after it
	// has seen the stop this test signals afterwards.
	r.e.idx.closeWait = watchdog
	finishing := make(chan struct{})
	r.e.idx.beforeFinish = func() { close(finishing) }

	_, _, submitted := overlappingSeeds(t, r, idx, entered)

	closed := make(chan error, 1)
	go func() { closed <- r.e.Close() }()
	await(t, finishing, "the worker's exit to begin with A's seed still in flight")
	release()
	await(t, submitted, "A's Submit to return once its write is let go")
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close never returned with an inline seed's retry owed")
	}

	// By the time Close has RETURNED the row is there: that is the whole claim.
	// Its title is B's prompt, because A's attempt consumed A's.
	row := idx.seedRow(t)
	if got, want := row, wrote("018f-the-thread", "the second prompt", sessions.TitleKindFallback); got != want {
		t.Fatalf("the exit wrote %+v, want %+v", got, want)
	}
	if n := idx.seeds(); n != 1 {
		t.Fatalf("%d first-prompt rows: %+v", n, idx.all())
	}
}

// submitting runs one Submit on a goroutine of its own and answers with the
// barrier that closes when it has returned: the shape every schedule here needs,
// because a Submit whose seed is parked in Upsert does not come back.
func submitting(t *testing.T, r *rig, text string) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := r.e.Submit(Command{}, text, SubmitQueue, ""); err != nil {
			t.Errorf("submit %q: %v", text, err)
		}
	}()
	return done
}

// TestASeedOpportunityIsRetainedByThePreWriteHook is r31 finding 1. A turn is
// counted by e.wg before it is launched and gives that count back when it ends,
// so an opportunity ADMITTED after the launch can arrive after Close has passed
// e.wg.Wait() and the worker's exit has looked at the slot — and then there is
// nobody left to write it: the retry a failing attempt hands back kicks a worker
// that has already gone, and a session that ran two prompts has no index row at
// all.
//
// What this test forces, with a seam of the engine's own (beforeInlineSeed,
// which runs AFTER the launch, so B's turn may already have ended when it fires):
// B's caller is held before its write with A's seed parked in Upsert, and at
// that hook B's opportunity is ALREADY retained; B's turn ends, Close begins,
// and only then does A fail. B's row exists by the time Close returns. That
// rejects the old launch→hook→admit order every time. That admission precedes
// the LAUNCH is runOwn's construction (admit, then `go e.runTurn`), which no
// hook here can observe.
func TestASeedOpportunityIsRetainedByThePreWriteHook(t *testing.T) {
	idx := newFakeIndex()
	entered, release := idx.parkAt(1)
	t.Cleanup(release)
	boom := errors.New("craze: not saving the session: permission denied")
	// Transient: A's write fails, and the retry the exit makes for it lands.
	idx.failAt(1, boom)

	atHook, letGo := make(chan struct{}), make(chan struct{})
	r := indexedHooked(t, idx, "018f-the-thread", &hooks{beforeInlineSeed: func(turn string) {
		if turn != "turn-2" {
			return
		}
		close(atHook)
		<-letGo
	}})
	// The bound is not what this is about (TestCloseAbandonsALastWriteNothingCanInterrupt
	// is), and beforeFinish is what makes this the SHUTDOWN's schedule rather
	// than a race with an ordinary pass. Both are written here and never again,
	// and the worker reads them only after the stop this test signals later.
	r.e.idx.closeWait = watchdog
	finishing := make(chan struct{})
	r.e.idx.beforeFinish = func() { close(finishing) }

	first := submitting(t, r, "the first prompt")
	await(t, entered, "A's seed to park in Upsert")
	// A's turn ends while its caller is still inside that write.
	r.until(lastEnding)

	second := submitting(t, r, "the second prompt")
	await(t, atHook, "B's caller to reach the window between its launch and its write")
	// While B's caller is parked at the hook, the opportunity is ALREADY
	// retained in the index writer's one slot. That is all this inspection
	// proves, and it deterministically rejects an implementation that admits
	// only after the hook. B's turn may or may not have ended by now.
	r.e.idx.mu.Lock()
	gotNext, gotText, gotCause := r.e.idx.seedNext, r.e.idx.seedText, r.e.idx.seedCause
	r.e.idx.mu.Unlock()
	if !gotNext || gotText != "the second prompt" || gotCause != "" {
		t.Fatalf("B's seed opportunity while its caller sits at the hook: retained=%v text=%q cause=%q, want retained %q",
			gotNext, gotText, gotCause, "the second prompt")
	}
	// B's turn ends — and gives its e.wg count back — with its caller parked.
	r.until(lastEnding)
	if n := idx.tries(); n != 1 {
		t.Fatalf("%d writes with A's still parked, want only A's", n)
	}

	closed := make(chan error, 1)
	go func() { closed <- r.e.Close() }()
	await(t, finishing, "the worker's exit to begin with A's seed still in flight")
	close(letGo)
	await(t, second, "B's Submit to return")
	release()
	await(t, first, "A's Submit to return once its write is let go")
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close never returned with B's seed owed")
	}

	row := idx.seedRow(t)
	if got, want := row, wrote("018f-the-thread", "the second prompt", sessions.TitleKindFallback); got != want {
		t.Fatalf("the exit wrote %+v, want %+v", got, want)
	}
	if n := idx.seeds(); n != 1 {
		t.Fatalf("%d first-prompt rows: %+v", n, idx.all())
	}
}

// TestASeedWhoseWriteBlowsUpStillHandsOnWhatItOwed is r31 finding 3. The seed's
// bookkeeping — seeding, the retained opportunity's kick, and the close of
// seedDone — is a defer, so an Upsert (or a snapshot, a hidden predicate, a
// title fold, a report callback) that PANICS under a caller which recovers
// leaves the retry state exactly as a failed write would.
//
// Without it seeding stayed true and seedDone stayed open for ever: every later
// turn was retained behind an attempt that had already unwound, and the worker's
// exit waited out its whole bound on a channel nothing would close — which is
// what this test would do to the watchdog if the defer went away.
func TestASeedWhoseWriteBlowsUpStillHandsOnWhatItOwed(t *testing.T) {
	idx := newFakeIndex()
	entered, release := idx.parkAt(1)
	t.Cleanup(release)
	idx.panicAt(1)
	r := indexed(t, idx, "018f-the-thread")
	r.e.idx.closeWait = watchdog
	finishing := make(chan struct{})
	r.e.idx.beforeFinish = func() { close(finishing) }

	panicked := make(chan struct{})
	go func() {
		defer close(panicked)
		defer func() {
			if rec := recover(); rec == nil {
				t.Error("A's Submit returned although its Upsert panicked")
			}
		}()
		if _, err := r.e.Submit(Command{}, "the first prompt", SubmitQueue, ""); err != nil {
			t.Errorf("submit A: %v", err)
		}
	}()
	await(t, entered, "A's seed to park in Upsert")
	r.until(lastEnding)

	second := submitting(t, r, "the second prompt")
	await(t, second, "B's Submit to return while A's seed is parked")
	r.until(lastEnding)

	closed := make(chan error, 1)
	go func() { closed <- r.e.Close() }()
	await(t, finishing, "the worker's exit to begin with A's seed still in flight")
	release()
	await(t, panicked, "A's Submit to unwind once its write is let go")
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close waited out its bound on a seedDone the panic never closed")
	}

	// The next turn's opportunity was handed on and written, and no attempt is
	// left in flight for ever.
	row := idx.seedRow(t)
	if got, want := row, wrote("018f-the-thread", "the second prompt", sessions.TitleKindFallback); got != want {
		t.Fatalf("the exit wrote %+v, want %+v", got, want)
	}
	r.e.idx.mu.Lock()
	seeding, done := r.e.idx.seeding, r.e.idx.seedDone
	r.e.idx.mu.Unlock()
	if seeding || done != nil {
		t.Fatalf("the panic left seeding=%v seedDone=%v: the retry state was not restored", seeding, done != nil)
	}
}

// TestASucceededSeedDiscardsTheRetainedOne: the retained opportunity is a RETRY
// and nothing more. A lands, so there is a row and its title is the first
// prompt's, and B's text is never written — a second fallback row would name the
// session by the wrong prompt.
func TestASucceededSeedDiscardsTheRetainedOne(t *testing.T) {
	idx := newFakeIndex()
	entered, release := idx.parkAt(1)
	t.Cleanup(release)
	r := indexed(t, idx, "018f-the-thread")

	_, _, submitted := overlappingSeeds(t, r, idx, entered)
	release()
	// A's write landed before its Submit returned, and the discard happens in
	// that same call: once this barrier is closed nothing can seed again.
	await(t, submitted, "A's Submit to return once its write is let go")

	if n := idx.seeds(); n != 1 {
		t.Fatalf("%d first-prompt rows, want only A's: %+v", n, idx.all())
	}
	if got := idx.seedRow(t).Title; got != "the first prompt" {
		t.Fatalf("the first-prompt row is named %q, want A's prompt", got)
	}
}

// indexErr accepts the state delta that reports a failed index write; cause,
// when set, is the command it must name.
func indexErr(cause string) func(agent.Event) bool {
	return func(ev agent.Event) bool {
		if ev.Type != agent.EventMeta || ev.State == nil || ev.State.IndexErr == "" {
			return false
		}
		return cause == "" || ev.Cause == cause
	}
}

// causesOf is the cause of every failed-index-write delta among evs, in order.
func causesOf(evs []agent.Event) []string {
	var out []string
	for _, ev := range evs {
		if indexErr("")(ev) {
			out = append(out, ev.Cause)
		}
	}
	return out
}

// TestAnArmedSendsFailedSeedNamesTheCommandThatArmedIt is r29 finding 4. A turn
// the ENGINE started can still have a command behind it — the send-now a client
// armed, which cancelled a running turn to make room for itself — and that
// client is the one that wants to hear that the session could not be written
// down. The worker's seed used to be posted with no cause at all, so the delta
// arrived naming nobody and a client drawing its error row from the cause had
// nothing to key it to.
func TestAnArmedSendsFailedSeedNamesTheCommandThatArmedIt(t *testing.T) {
	idx := newFakeIndex()
	boom := errors.New("craze: no home directory to save the session index in")
	idx.setErr(boom)
	r := indexed(t, idx, "")

	turn := r.s.script(held())
	submit := Command{Client: r.e.NewClientID(), ID: "1"}
	if _, err := r.e.Submit(submit, "the first prompt", SubmitQueue, ""); err != nil {
		t.Fatalf("the first prompt: %v", err)
	}
	await(t, turn.opened, "the first turn to open")

	// Armed behind it: the cancel this asks for ends that turn, and the
	// settlement fires the send. Its seed is the WORKER's — there is no caller
	// on that path — and it is a real first-prompt seed, because the one above
	// failed.
	arm := Command{Client: submit.Client, ID: "2"}
	res, err := r.e.Submit(arm, "the armed prompt", SubmitSendNow, "")
	if err != nil || !res.Armed {
		t.Fatalf("arming a send-now: %+v %v", res, err)
	}

	got := r.until(indexErr(arm.Cause()))
	if last := got[len(got)-1]; last.State.IndexErr != boom.Error() {
		t.Fatalf("the delta carries %q, want the store's own message", last.State.IndexErr)
	}
	// The two failures on the record are the two prompts', each naming its own
	// command: nothing reported with an empty cause on the way.
	if want := []string{submit.Cause(), arm.Cause()}; strings.Join(causesOf(got), "|") != strings.Join(want, "|") {
		t.Fatalf("the failures name %q, want %q%s", causesOf(got), want, describe(got))
	}
}

// TestARetriedClaimsFailedSeedNamesItsCommandToo is the other worker-driven
// seed with a command behind it: a turn the session refused because the agent
// was running one of its own, claimed again by the driver. The turn is the same
// turn, so it carries the same cause, and the seed the re-claim posts must name
// it just as the first attempt's did.
func TestARetriedClaimsFailedSeedNamesItsCommandToo(t *testing.T) {
	idx := newFakeIndex()
	idx.setErr(errors.New("craze: no home directory to save the session index in"))
	ticks := make(chan time.Time)
	returned := make(chan string, 16)
	r := newRigHooked(t, Options{
		Chain: ChainPolicy{RetryForeignTurn: true},
		Index: IndexOptions{Store: idx, CWD: "/w", Provider: "cursor"},
	}, agent.EventLogOptions{NoPrimary: true},
		&hooks{retryTick: ticks, turnReturned: func(id string) { returned <- id }})

	r.s.script(&script{refuse: agent.ErrForeignTurn})
	c := Command{Client: r.e.NewClientID(), ID: "1"}
	if _, err := r.e.Submit(c, "go", SubmitQueue, ""); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if id := <-returned; id != "turn-1" {
		t.Fatalf("%s came back, want turn-1's refusal", id)
	}
	tick(t, ticks, returned)

	// Two failures: the Submit's own seed, on its caller's goroutine, and the
	// re-claim's, on the worker. Both name the command that submitted the turn.
	first := r.until(indexErr(""))
	second := r.until(indexErr(""))
	for i, ev := range []agent.Event{first[len(first)-1], second[len(second)-1]} {
		if ev.Cause != c.Cause() {
			t.Fatalf("failure %d names %q, want the command that submitted the turn (%q)", i+1, ev.Cause, c.Cause())
		}
	}
}

// TestATurnEndingBeforeItsSeedWritesTheSeedFirst pins the window seededTurn
// closes for the tests that count writes, and what happens inside it. CI found
// it: a test that expected "seed, then touch" saw one row. The schedule is
// FORCED with beforeInlineSeed: the submitter is held between the launch and
// its write, the turn runs and ends, and only then is the seed written. No
// write can precede the seed — a touch never conjures a row — and the seed still
// creates the row. Whether a touch FOLLOWS is the worker's timing, which this
// test does not force (r.sync flushes the event log, not the index worker): a
// worker that took the touch before the seed landed found no row and dropped it
// (one row, what CI saw); one that woke after it writes it (two). Here the seed
// is written after the turn's end, so the dropped touch costs no recency. The
// narrower case — a touch taken while the seed is INSIDE Upsert, which keeps the
// seed's slightly earlier timestamp — is recorded in `12` for S2, not fixed.
func TestATurnEndingBeforeItsSeedWritesTheSeedFirst(t *testing.T) {
	idx := newFakeIndex()
	atHook, letGo := make(chan struct{}), make(chan struct{})
	r := indexedHooked(t, idx, "", &hooks{beforeInlineSeed: func(string) {
		close(atHook)
		<-letGo
	}})
	done := submitting(t, r, "a prompt")
	await(t, atHook, "the submitter to reach the window between its launch and its write")
	r.until(lastEnding)
	r.sync()
	if n := idx.tries(); n != 0 {
		t.Fatalf("%d index writes before the seed: a touch with no row writes nothing", n)
	}
	close(letGo)
	await(t, done, "the submit to return")
	idx.waitRows(t, 1)
	if err := r.e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rows := idx.all()
	if got := rows[0]; got.TitleKind != sessions.TitleKindFallback || got.Title != "a prompt" {
		t.Fatalf("the first write was %+v, want the first prompt's fallback row", got)
	}
	switch len(rows) {
	case 1:
	case 2:
		if got := rows[1]; got.TitleKind != sessions.TitleKindNone || got.Title != "" {
			t.Fatalf("the write after the seed was %+v, want a touch", got)
		}
	default:
		t.Fatalf("%d rows, want the seed and at most one touch: %+v", len(rows), rows)
	}
}

// seededTurn runs one turn whose seed is on the record before the turn can end.
// A submit admits its seed, launches the turn and only then writes the seed, so
// a turn that ends at once can reach its end-of-turn touch while there is still
// no row — and a touch with no row writes nothing, by design. That is harmless
// (the seed's own later write carries the newer UpdatedAt) but it makes "seed,
// then touch" a race for a test that counts writes. Holding the turn until the
// seed has landed makes it the order.
func seededTurn(t *testing.T, r *rig, idx *fakeIndex, text string) {
	t.Helper()
	turn := r.s.script(held())
	r.submit(text)
	idx.waitRows(t, 1)
	turn.release()
	r.until(lastEnding)
}

// TestTheAgentTitleAndTheTurnEndAreWorkerWrites: neither has a caller — an
// observer may not do I/O — so both go to the worker, and the row they write
// carries the same durable id every other write does.
func TestTheAgentTitleAndTheTurnEndAreWorkerWrites(t *testing.T) {
	idx := newFakeIndex()
	r := indexed(t, idx, "")
	seededTurn(t, r, idx, "a prompt")
	// The turn's own ending touched the row: the seed, then the touch.
	idx.waitRows(t, 2)
	if got := idx.last(); got.TitleKind != sessions.TitleKindNone || got.Title != "" {
		t.Fatalf("the turn's end wrote %+v, want a touch", got)
	}

	r.s.emit(agent.Event{Type: agent.EventMeta, Text: "the agent's own name"})
	idx.waitRows(t, 3)
	row := idx.last()
	if row.TitleKind != sessions.TitleKindAgent || row.Title != "the agent's own name" {
		t.Fatalf("the agent title wrote %+v", row)
	}
	if row.CrazeID != r.e.State().CrazeSessionID {
		t.Fatalf("a worker write carried craze id %q, want %q", row.CrazeID, r.e.State().CrazeSessionID)
	}
}

// TestATouchWithNoRowWritesNothing: a turn the agent ran on its own, before
// craze ever sent a prompt, must not conjure a titleless row. The proof is
// ordering, not a sleep — the worker is FIFO, so a title posted afterwards
// being the FIRST write is exactly "the touch wrote nothing".
func TestATouchWithNoRowWritesNothing(t *testing.T) {
	idx := newFakeIndex()
	r := indexed(t, idx, "")
	r.s.emit(agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	r.sync()
	r.s.emit(agent.Event{Type: agent.EventMeta, Text: "the agent's own name"})
	idx.waitRows(t, 1)
	if got := idx.last(); got.TitleKind != sessions.TitleKindAgent {
		t.Fatalf("the first write was %+v, want the title: the touch created a row", got)
	}
	if n := idx.tries(); n != 1 {
		t.Fatalf("%d writes, want only the title's: %+v", n, idx.all())
	}
}

// TestSubsumptionIsConditionalOnSuccess is r29 finding 3. One pass writes once
// wherever it can, because a write that lands bumps UpdatedAt and creates the
// row — but a write that FAILED did neither, so the work merged beside it was
// being consumed by a write that never happened. The baseline wrote each kind
// separately and so never lost one to another's failure: a title write that
// failed there still left the loaded row's touch to be made, and with it the
// durable id that touch would have written.
func TestSubsumptionIsConditionalOnSuccess(t *testing.T) {
	boom := errors.New("craze: not saving the session: permission denied")

	t.Run("a failed title still leaves the load", func(t *testing.T) {
		idx := newFakeIndex()
		idx.failAt(1, boom)
		w, reported := writerOn(t, idx)
		w.runPending(indexWork{title: "the agent's own name", load: true})

		if n := idx.tries(); n != 2 {
			t.Fatalf("%d writes, want the title's failure and the load behind it", n)
		}
		rows := idx.all()
		if len(rows) != 1 {
			t.Fatalf("%d rows landed: %+v", len(rows), rows)
		}
		// The load's own write, carrying the durable id a freshly minted one
		// would otherwise have had to wait for another write to record.
		if got, want := rows[0], wrote("018f-the-thread", "", sessions.TitleKindNone); got != want {
			t.Fatalf("the load wrote %+v, want %+v", got, want)
		}
		if got := reported(); len(got) != 1 || got[0] != ": "+boom.Error() {
			t.Fatalf("the pass reported %q, want the title's failure alone", got)
		}
	})

	t.Run("a failed title still leaves the touch", func(t *testing.T) {
		idx := newFakeIndex()
		w, _ := writerOn(t, idx)
		// A row to touch, from a pass of its own: write 1.
		w.runPending(indexWork{load: true})
		idx.failAt(2, boom)
		w.runPending(indexWork{title: "the agent's own name", touch: true})

		if n := idx.tries(); n != 3 {
			t.Fatalf("%d writes, want the load's, the title's failure and the touch behind it", n)
		}
		rows := idx.all()
		if len(rows) != 2 {
			t.Fatalf("%d rows landed: %+v", len(rows), rows)
		}
		if got, want := rows[1], wrote("018f-the-thread", "", sessions.TitleKindNone); got != want {
			t.Fatalf("the touch wrote %+v, want %+v", got, want)
		}
	})

	t.Run("a seed that landed still subsumes a touch", func(t *testing.T) {
		idx := newFakeIndex()
		w, _ := writerOn(t, idx)
		w.runPending(indexWork{seed: true, seedText: "a prompt", touch: true})
		if n := idx.tries(); n != 1 {
			t.Fatalf("%d writes, want the seed alone: its UpdatedAt IS the touch", n)
		}
		if got, want := idx.last(), wrote("018f-the-thread", "a prompt", sessions.TitleKindFallback); got != want {
			t.Fatalf("the pass wrote %+v, want %+v", got, want)
		}
	})

	t.Run("a title that landed subsumes both", func(t *testing.T) {
		idx := newFakeIndex()
		w, _ := writerOn(t, idx)
		w.runPending(indexWork{title: "the agent's own name", load: true, touch: true})
		if n := idx.tries(); n != 1 {
			t.Fatalf("%d writes, want the title alone", n)
		}
	})
}

// TestALoadedRowIsKnownToExistWhateverItsFirstWriteDid is r30 finding 3. A
// loaded session's row is the very row --continue or --resume read its id out
// of: it EXISTS, whatever the write that touches it comes to. Marking it known
// only on a write that LANDED left a transiently failed load with row false, so
// the touch merged beside it — and every ordinary touch after it — wrote
// nothing at all. The promised retry never happened, and the durable craze id
// and the recency a resumed session sorts by could stay unwritten for the whole
// of that session.
func TestALoadedRowIsKnownToExistWhateverItsFirstWriteDid(t *testing.T) {
	boom := errors.New("craze: not saving the session: permission denied")
	idx := newFakeIndex()
	idx.failAt(1, boom)
	w, reported := writerOn(t, idx)

	// The replay-end load, merged with a touch: the load fails, and the touch
	// behind it is the retry.
	w.runPending(indexWork{load: true, touch: true})
	if n := idx.tries(); n != 2 {
		t.Fatalf("%d writes, want the load's failure and the touch behind it", n)
	}
	rows := idx.all()
	if len(rows) != 1 {
		t.Fatalf("%d rows landed: %+v", len(rows), rows)
	}
	if got, want := rows[0], wrote("018f-the-thread", "", sessions.TitleKindNone); got != want {
		t.Fatalf("the touch wrote %+v, want %+v", got, want)
	}
	if got := reported(); len(got) != 1 || got[0] != ": "+boom.Error() {
		t.Fatalf("the pass reported %q, want the load's failure alone", got)
	}

	// And an ORDINARY touch, in a pass of its own, is no longer suppressed
	// either: the row is known, so the recency it records goes to the file.
	w.runPending(indexWork{touch: true})
	if n := idx.count(); n != 2 {
		t.Fatalf("%d rows written, want the touch after the failed load to have landed too: %+v", n, idx.all())
	}
}

// TestOneAdmissionSequenceForEverySeed is r30 finding 4. A seed opportunity
// arrives on either of two goroutines — a client's own, inside Submit, and the
// observer's post for a turn the ENGINE started — and with a pending slot and a
// deferred slot kept apart, the retry handed back by a failed attempt could
// find the pending one already taken by a LATER prompt and be dropped in its
// favour. The session was then named by the wrong turn.
//
// Here A is in flight, B arrives inline behind it, C is posted behind B, and A
// fails. B is the earliest opportunity still unwritten, so B is the fallback
// title; C, which arrived while a seed was in flight, went into the same one
// slot and lost it by arrival.
func TestOneAdmissionSequenceForEverySeed(t *testing.T) {
	idx := newFakeIndex()
	entered, release := idx.parkAt(1)
	t.Cleanup(release)
	idx.failAt(1, errors.New("craze: not saving the session: permission denied"))
	w, _ := writerOn(t, idx)
	go w.serve()

	// A: a posted seed the worker claims and parks in.
	w.post(indexWork{seed: true, seedText: "the first prompt"})
	await(t, entered, "A's seed to park in Upsert")
	// B: an INLINE seed, arriving while A is in flight. It writes nothing and
	// does not wait for A.
	if w.seed("c-1/2", "the second prompt") {
		t.Fatal("B's seed wrote a row with A's still in flight")
	}
	// C: a posted seed, arriving behind B and later than it.
	w.post(indexWork{seed: true, seedText: "the third prompt"})

	release()
	idx.waitRows(t, 1)
	row := idx.seedRow(t)
	if got, want := row, wrote("018f-the-thread", "the second prompt", sessions.TitleKindFallback); got != want {
		t.Fatalf("the retry wrote %+v, want %+v — the earliest opportunity still unwritten", got, want)
	}
	if n := idx.seeds(); n != 1 {
		t.Fatalf("%d first-prompt rows: %+v", n, idx.all())
	}
}

// TestASuccessfulTitleRetiresFallbackSeeding is r30 finding 5. A row that has
// been named by the AGENT — or by a /rename, which pins — wants no first-prompt
// fallback: applyTitle would overrule it anyway. With seeded left false by the
// seed's own failure, every later turn retried that useless write, and every
// one of those that failed sent the client another IndexErr for a row that is
// safely indexed and correctly named.
func TestASuccessfulTitleRetiresFallbackSeeding(t *testing.T) {
	boom := errors.New("craze: not saving the session: permission denied")

	t.Run("the agent's own name", func(t *testing.T) {
		idx := newFakeIndex()
		idx.failAt(1, boom)
		w, reported := writerOn(t, idx)
		// One merged pass: the seed fails, and the title behind it lands.
		w.runPending(indexWork{seed: true, seedText: "a prompt", title: "the agent's own name"})
		if n := idx.tries(); n != 2 {
			t.Fatalf("%d writes, want the seed's failure and the title behind it", n)
		}
		if got := idx.last(); got.TitleKind != sessions.TitleKindAgent {
			t.Fatalf("the title wrote %+v", got)
		}

		// A later turn on a client's own goroutine attempts nothing...
		if w.seed("c-1/1", "a second prompt") {
			t.Fatal("a later Submit seeded a row the agent had already named")
		}
		// ...and one the ENGINE started is not even admitted, so no pass of the
		// worker's carries it.
		w.post(indexWork{seed: true, seedText: "a third prompt"})
		if got := w.take(); got.seed {
			t.Fatalf("a later engine turn's seed was admitted after the title: %+v", got)
		}
		if n := idx.seeds(); n != 0 {
			t.Fatalf("%d first-prompt rows were written: %+v", n, idx.all())
		}
		if n := idx.tries(); n != 2 {
			t.Fatalf("%d writes in all, want the two of the first pass: %+v", n, idx.all())
		}
		// One report, the first seed's: no repeated IndexErr for a named row.
		if got := reported(); len(got) != 1 || got[0] != ": "+boom.Error() {
			t.Fatalf("the writer reported %q, want the first seed's failure alone", got)
		}
	})

	t.Run("a /rename", func(t *testing.T) {
		idx := newFakeIndex()
		idx.failAt(1, boom)
		w, _ := writerOn(t, idx)
		w.runPending(indexWork{seed: true, seedText: "a prompt"})
		if err := w.rename("c-1/1", "a better name"); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if got := idx.last(); got.TitleKind != sessions.TitleKindUser {
			t.Fatalf("the rename wrote %+v", got)
		}
		if w.seed("c-1/2", "a second prompt") {
			t.Fatal("a later Submit seeded a row the user had already named and pinned")
		}
		if n := idx.seeds(); n != 0 {
			t.Fatalf("%d first-prompt rows were written: %+v", n, idx.all())
		}
	})
}

// TestLatestWinsNeverLosesATitleBehindATouch is why the worker's slot is a
// merged STRUCT and not a channel of rows: with one slot holding one row, a
// turn ending behind a title would simply replace it and the name would be
// lost. Here both land while a write is parked, and the title is still written.
func TestLatestWinsNeverLosesATitleBehindATouch(t *testing.T) {
	idx := newFakeIndex()
	// Armed before anything is written: the seed is write 1 and goes through,
	// and write 2 — the turn's own touch — is the one that parks. Arming it
	// after the turn had ended would be racing the worker for that write.
	entered, release := idx.parkAt(2)
	r := indexed(t, idx, "")
	seededTurn(t, r, idx, "a prompt")
	await(t, entered, "the turn's touch to park")

	// Both of these are merged while the worker is held inside that write: a
	// title, and then a touch behind it.
	r.s.emit(agent.Event{Type: agent.EventMeta, Text: "the agent's own name"})
	r.s.emit(agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	r.sync()
	release()

	idx.waitRows(t, 3)
	if got := idx.last(); got.TitleKind != sessions.TitleKindAgent || got.Title != "the agent's own name" {
		t.Fatalf("the merged pass wrote %+v, want the title the touch came in behind", got)
	}
}

// TestAStalledIndexWriteBlocksNoControlMethod is A15's last clause. An Upsert
// parked the way a contended file lock parks one holds the index worker and
// nothing else: every Control method that takes e.mu answers at once, and Close
// returns rather than waiting for a write nothing can interrupt.
//
// The two writes that are NOT covered by that, by design, are the two §3.2
// documents as file I/O on the caller's own goroutine — Submit's first-prompt
// seed and SetTitle's rename — and neither holds e.mu while it runs. Submit
// appears below precisely because the row here is already seeded, so its seed
// is a no-op and the call is as instant as every other.
func TestAStalledIndexWriteBlocksNoControlMethod(t *testing.T) {
	idx := newFakeIndex()
	// The seed is write 1 and goes through; write 2 is the turn's own touch,
	// and that is the one held. Armed before the engine exists, so nothing is
	// racing the worker for which write parks.
	entered, release := idx.parkAt(2)
	t.Cleanup(release)
	r := indexed(t, idx, "")
	seededTurn(t, r, idx, "a prompt")
	await(t, entered, "the turn's touch to park in Upsert")

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.e.State()
		if _, err := r.e.Queue(Command{}, "a queued row"); err != nil {
			t.Errorf("Queue while an index write is parked: %v", err)
		}
		if _, err := r.e.Submit(Command{}, "another prompt", SubmitQueue, ""); err != nil {
			t.Errorf("Submit while an index write is parked: %v", err)
		}
		if _, err := r.e.ClearQueue(Command{}); err != nil {
			t.Errorf("ClearQueue while an index write is parked: %v", err)
		}
		if err := r.e.Disarm(Command{}); !errors.Is(err, ErrNotAccepting) {
			t.Errorf("Disarm while an index write is parked: %v", err)
		}
	}()
	await(t, done, "the Control methods to answer with an index write parked")

	closed := make(chan error, 1)
	go func() { closed <- r.e.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Close is waiting for an index write that nothing can interrupt")
	}
}

// TestCloseJoinsTheIndexWorker: the worker is one of the engine's goroutines
// and Close is what ends it, so nothing of a closed engine's is still running —
// which is what A18 asks of every path that closes one (the exit tail, the
// provider picker, the resume picker).
func TestCloseJoinsTheIndexWorker(t *testing.T) {
	idx := newFakeIndex()
	r := indexed(t, idx, "")
	seededTurn(t, r, idx, "a prompt")
	idx.waitRows(t, 2)
	if err := r.e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-r.e.idx.exited:
	default:
		t.Fatal("Close returned with the index worker still running")
	}
}

// TestTheWorkersExitDrainsWhatItStillOwes is r29 finding 1. A quit right after
// a first prompt the DRAIN started — or after the agent named the session, or
// after a load came up — used to be able to lose that write entirely: the
// worker's select had `done` and `wake` both ready and chose between them at
// random, and the branch that took `done` returned without ever looking at the
// slot. The row then never existed, so --continue could not find the session at
// all, where the TUI writing synchronously in Update never lost one.
//
// The schedule is FORCED here rather than raced for: the work is posted and the
// stop signalled before the worker's loop exists, so both are ready the first
// time it looks.
func TestTheWorkersExitDrainsWhatItStillOwes(t *testing.T) {
	for _, tc := range []struct {
		name string
		work indexWork
		want sessions.Row
	}{
		{"a drained first prompt's seed",
			indexWork{seed: true, seedText: "the drained row\nand a second line"},
			wrote("018f-the-thread", "the drained row", sessions.TitleKindFallback)},
		{"the agent's own name",
			indexWork{title: "the agent's own name"},
			wrote("018f-the-thread", "the agent's own name", sessions.TitleKindAgent)},
		{"a loaded row's touch",
			indexWork{load: true},
			wrote("018f-the-thread", "", sessions.TitleKindNone)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := newFakeIndex()
			w, _ := writerOn(t, idx)
			w.post(tc.work)
			w.stop()
			go w.serve()
			w.close()

			rows := idx.all()
			if len(rows) != 1 {
				t.Fatalf("closing wrote %d rows, want the one it still owed: %+v", len(rows), rows)
			}
			if rows[0] != tc.want {
				t.Fatalf("closing wrote %+v, want %+v", rows[0], tc.want)
			}
		})
	}
}

// TestTheWorkersExitDropsAPlainTouch is the other half of the rule: a touch on
// its own records nothing but an UpdatedAt that the next run's first write
// bumps anyway, and it is not worth holding a quit for. The row exists here, so
// this is a claim about the drain and not about "a touch with no row writes
// nothing".
func TestTheWorkersExitDropsAPlainTouch(t *testing.T) {
	idx := newFakeIndex()
	w, _ := writerOn(t, idx)
	// One pass, run inline: the row now exists and the touch has something to
	// touch.
	w.runPending(indexWork{load: true})
	if n := idx.count(); n != 1 {
		t.Fatalf("the load wrote %d rows: %+v", n, idx.all())
	}

	w.post(indexWork{touch: true})
	w.stop()
	go w.serve()
	w.close()
	if n := idx.tries(); n != 1 {
		t.Fatalf("%d writes, want only the load's: closing wrote the touch too", n)
	}
}

// TestTheLastWriteQueuesBehindOneInFlight: one engine never runs two Upserts on
// this worker at once, and closing does not break that. The write that was in
// flight when the stop arrived is waited for — inside the same bound — and only
// then does the final one go.
func TestTheLastWriteQueuesBehindOneInFlight(t *testing.T) {
	idx := newFakeIndex()
	entered, release := idx.parkAt(1)
	t.Cleanup(release)
	w, _ := writerOn(t, idx)
	// The barrier that makes this the shutdown's schedule and not a race with
	// an ordinary pass: the write is let go only once the exit has begun.
	finishing := make(chan struct{})
	w.beforeFinish = func() { close(finishing) }
	go w.serve()

	w.post(indexWork{load: true})
	await(t, entered, "the loaded row's touch to park in Upsert")
	// Merged while that write is held, so it is still in the slot when the stop
	// arrives and the worker is inside the select that waits for the write.
	w.post(indexWork{title: "the agent's own name"})

	w.stop()
	await(t, finishing, "the worker's exit to begin with a write still in flight")
	closed := make(chan struct{})
	go func() { defer close(closed); w.close() }()
	release()
	await(t, closed, "close to return once both writes are through")

	rows := idx.all()
	if len(rows) != 2 {
		t.Fatalf("%d rows, want the parked write's and the last one's: %+v", len(rows), rows)
	}
	if got, want := rows[1], wrote("018f-the-thread", "the agent's own name", sessions.TitleKindAgent); got != want {
		t.Fatalf("the last write wrote %+v, want %+v", got, want)
	}
	if n := idx.peakLive(); n != 1 {
		t.Fatalf("%d Upserts ran at once; the worker writes one at a time", n)
	}
}

// TestCloseAbandonsALastWriteNothingCanInterrupt: the drain keeps Close bounded
// where it always was. A flock has neither timeout nor context, so the last
// write is waited for only up to the worker's own bound — the journal's, 500 ms
// — and then left to its goroutine, exactly as a write already in flight is.
func TestCloseAbandonsALastWriteNothingCanInterrupt(t *testing.T) {
	idx := newFakeIndex()
	entered, release := idx.parkAt(1)
	t.Cleanup(release)
	w, _ := writerOn(t, idx)
	// The production bound: this is the one test about it.
	w.closeWait = indexCloseWait

	w.post(indexWork{seed: true, seedText: "the drained row"})
	w.stop()
	go w.serve()
	await(t, entered, "the last write to park in Upsert")

	closed := make(chan struct{})
	go func() { defer close(closed); w.close() }()
	await(t, closed, "close to return with its last write parked in Upsert")
	if n := idx.count(); n != 0 {
		t.Fatalf("a parked write recorded %d rows", n)
	}
	if n := idx.tries(); n != 1 {
		t.Fatalf("%d writes, want the one that was attempted and abandoned", n)
	}
}

// TestAHiddenProvidersSessionIsNeverIndexed is A15's native clause (plan 018
// §3.4): until the harness has a loader, a native session stays out of the
// shared index, so --continue and --resume never offer a row nothing can load.
// Skipping counts as DONE — no retry, and no error row.
func TestAHiddenProvidersSessionIsNeverIndexed(t *testing.T) {
	idx := newFakeIndex()
	r := newRig(t, Options{Index: IndexOptions{
		Store: idx, CWD: "/w", Provider: "native",
		Hidden: func(p string) bool { return p == "native" },
	}})
	r.submit("a prompt")
	r.until(lastEnding)
	r.s.emit(agent.Event{Type: agent.EventMeta, Text: "the agent's own name"})
	if err := r.e.SetTitle(Command{}, "renamed"); err != nil {
		t.Fatalf("a rename on a hidden provider: %v", err)
	}
	r.submit("a second prompt")
	r.until(lastEnding)
	r.sync()
	if n := idx.tries(); n != 0 {
		t.Fatalf("a hidden provider's session was indexed %d times: %+v", n, idx.all())
	}
	// And nothing was reported: a skipped write is not a failure.
	for _, ev := range r.seen {
		if ev.Type == agent.EventMeta && ev.State != nil && ev.State.IndexErr != "" {
			t.Fatalf("skipping a hidden provider reported %q", ev.State.IndexErr)
		}
	}
}

// TestANilIndexWritesNothingAndStillMintsAnID is `craze prompt`: it persists
// nothing, as it never did, and still has the identity every other record names
// it by.
func TestANilIndexWritesNothingAndStillMintsAnID(t *testing.T) {
	r := newRig(t, Options{})
	r.submit("a prompt")
	r.until(lastEnding)
	if err := r.e.SetTitle(Command{}, "renamed"); err != nil {
		t.Fatalf("a rename with no index: %v", err)
	}
	if got := r.e.State().CrazeSessionID; got == "" {
		t.Fatal("a session with no index minted no craze id")
	}
}

// ------------------------------------------------------------------- /rename

// TestRenameWritesThePinnedRow: a user title always wins and pins the row, so
// no agent title this session or a later --continue produces can take the name
// back — which is sessions' merge rule, and the kind is how the engine asks for
// it.
func TestRenameWritesThePinnedRow(t *testing.T) {
	idx := newFakeIndex()
	r := indexed(t, idx, "018f-the-thread")
	if err := r.e.SetTitle(Command{}, "  a better name  "); err != nil {
		t.Fatalf("rename: %v", err)
	}
	row := idx.last()
	if row.TitleKind != sessions.TitleKindUser || row.Title != "a better name" {
		t.Fatalf("the rename wrote %+v", row)
	}
	if row.CrazeID != "018f-the-thread" {
		t.Fatalf("the rename carried craze id %q", row.CrazeID)
	}
}

// TestRenameReturnsAFailedIndexWrite: SetTitle is the one index write with a
// caller still standing, so its failure comes back rather than going out as a
// delta — and it says which half failed. The session IS renamed and pinned;
// only the record of it was not written.
func TestRenameReturnsAFailedIndexWrite(t *testing.T) {
	idx := newFakeIndex()
	boom := errors.New("craze: not saving the session: permission denied")
	idx.setErr(boom)
	r := indexed(t, idx, "")

	c := Command{Client: r.e.NewClientID(), ID: "1"}
	err := r.e.SetTitle(c, "a better name")
	if !errors.Is(err, ErrIndexWrite) {
		t.Fatalf("a failed index write came back as %v, want ErrIndexWrite", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("the cause is not matchable through it: %v", err)
	}
	if Code(err) != "index_write" {
		t.Fatalf("its code is %q, want index_write", Code(err))
	}
	if got := errors.Unwrap(err); got == nil || got.Error() != boom.Error() {
		t.Fatalf("a client cannot reach the store's own message: %v", got)
	}
	// The rename itself happened, and nothing published an IndexErr for it: the
	// return value is the whole report.
	if got := r.e.State().Title; got != "a better name" {
		t.Fatalf("the session title is %q, want the rename to have stood", got)
	}
	r.sync()
	for _, ev := range r.until(func(ev agent.Event) bool {
		return ev.Type == agent.EventMeta && ev.State != nil && ev.State.Title != nil
	}) {
		if ev.State != nil && ev.State.IndexErr != "" {
			t.Fatalf("a rename's failure was reported twice: %q", ev.State.IndexErr)
		}
	}

	// It is a stable answer about this command, so it is STORED: a resend
	// replays it and does not rename a second time.
	idx.setErr(nil)
	if again := r.e.SetTitle(c, "a better name"); !errors.Is(again, ErrIndexWrite) {
		t.Fatalf("the resend ran again and answered %v", again)
	}
	if n := idx.count(); n != 0 {
		t.Fatalf("the resend wrote a row: %+v", idx.all())
	}
}

// --------------------------------------------------- the real store, contended

// countingStore is the REAL sessions.Store with a barrier: every Upsert that
// returns sends a token, so a test can wait for writes to land without
// sleeping. It is what the engine actually writes through in production, flock
// and file rewrite included.
type countingStore struct {
	store sessions.Store
	mu    sync.Mutex
	n     int
	wrote chan struct{}
}

func newCountingStore() *countingStore {
	return &countingStore{wrote: make(chan struct{}, 64)}
}

func (c *countingStore) Upsert(row sessions.Row) error {
	err := c.store.Upsert(row)
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	select {
	case c.wrote <- struct{}{}:
	default:
	}
	return err
}

func (c *countingStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *countingStore) waitWrites(t *testing.T, n int) {
	t.Helper()
	timeout := time.After(watchdog)
	for c.count() < n {
		select {
		case <-c.wrote:
		case <-timeout:
			t.Fatalf("%d writes landed in %s, want %d", c.count(), watchdog, n)
		}
	}
}

// TestBothKindsOfWriteContendOnARealFileLock is the schedule the fake store
// cannot show: sessions.Store.Upsert taking an flock that somebody else already
// holds — a second craze in the same workspace — with one COMMAND-driven write
// (/rename, on its caller's goroutine) and one WORKER write (the agent naming
// the session) both waiting on it.
//
// What it pins is A15's last clause against the real thing: no Control method
// that takes e.mu waits for either of them, nothing reaches the file while the
// lock is held, and both land on the one row once it is let go — named by the
// /rename, whichever of the two gets there first, because applyTitle's
// precedence and not the order is what decides it (a user title pins; an agent
// title cannot take a pinned name back).
//
// The two writes are the WHOLE of what this session ever writes, deliberately:
// a turn would leave an end-of-turn touch running behind the assertions, and a
// write still landing in CRAZE_HOME while the test's temporary directory is
// being removed is a flake, not a schedule. /rename is a command-driven write
// that starts no turn.
func TestBothKindsOfWriteContendOnARealFileLock(t *testing.T) {
	t.Setenv("CRAZE_HOME", t.TempDir())
	path := paths.SessionsPath()
	if path == "" {
		t.Fatal("no session index path under CRAZE_HOME")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// The sibling lock sessions.Upsert takes, held here on a descriptor of our
	// own: flock is per open file description, so this really does shut both
	// writers out.
	unlock, err := atomicfile.Lock(path + ".lock")
	if err != nil {
		t.Fatalf("taking the index lock: %v", err)
	}
	var once sync.Once
	release := func() { once.Do(unlock) }
	t.Cleanup(release)

	store := newCountingStore()
	r := newRig(t, Options{CrazeSessionID: "018f-the-thread", Index: IndexOptions{
		Store: store, CWD: "/w", Provider: "cursor",
		TitleLine: func(s string) string { return strings.TrimSpace(s) },
	}})

	renamed := make(chan struct{})
	go func() {
		defer close(renamed)
		if err := r.e.SetTitle(Command{}, "a better name"); err != nil {
			t.Errorf("rename with the index lock held: %v", err)
		}
	}()
	r.s.emit(agent.Event{Type: agent.EventMeta, Text: "the agent's own name"})
	r.sync()

	answered := make(chan struct{})
	go func() {
		defer close(answered)
		r.e.State()
		if _, err := r.e.Queue(Command{}, "a queued row"); err != nil {
			t.Errorf("Queue with the index lock held: %v", err)
		}
		if _, err := r.e.ClearQueue(Command{}); err != nil {
			t.Errorf("ClearQueue with the index lock held: %v", err)
		}
	}()
	await(t, answered, "the Control methods to answer with the index lock held")

	if n := store.count(); n != 0 {
		t.Fatalf("%d writes got through a held file lock", n)
	}
	select {
	case <-renamed:
		t.Fatal("SetTitle returned with the index lock held: its write never reached the file")
	default:
	}

	release()
	await(t, renamed, "SetTitle to return once the lock is free")
	store.waitWrites(t, 2)

	rows, err := (&sessions.Store{}).Recent("/w", "cursor", 0)
	if err != nil {
		t.Fatalf("reading the index back: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows in the index, want the one session's: %+v", len(rows), rows)
	}
	if rows[0].Title != "a better name" || !rows[0].Pinned {
		t.Fatalf("the row is %+v, want the rename's name, pinned", rows[0])
	}
	if rows[0].CrazeID != "018f-the-thread" {
		t.Fatalf("the row carries craze id %q", rows[0].CrazeID)
	}
	// Nothing of this session's is still writing into CRAZE_HOME: the two
	// writes are through, and closing the engine here — before the temporary
	// directory goes — joins the worker rather than leaving it to a cleanup.
	if err := r.e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if n := store.count(); n != 2 {
		t.Fatalf("%d writes in all, want the rename's and the agent title's", n)
	}
}

// --------------------------------------------------------------- the fallback

// TestFallbackTitleIsTheFirstLine is the engine's half of the first-prompt
// title: the first line of the prompt, with a shell-context block in front of
// it stripped — a picker row reading "<shell_context>" would name every session
// that opened with a command the same thing. Folding it onto one line and
// capping it is the client's half (tui.indexTitleLine).
func TestFallbackTitleIsTheFirstLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"one line", "one line"},
		{"the first line\nthe second", "the first line"},
		{"", ""},
		{agent.ShellContextBlock([]agent.ShellResult{{Command: "ls", Output: "a b c"}}) + "what does it say?", "what does it say?"},
	} {
		if got := fallbackTitle(tc.in); got != tc.want {
			t.Fatalf("fallbackTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
