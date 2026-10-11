package engine

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/sessions"
)

// The user-title slot (plan 043 P10; index.go's "The user's title, and why it
// has a slot of its own"): the title a session was created with and every
// /rename, owed to the index until the session has its id, the newest the one
// that lands, never an older one after it.

// writerLearningID is writerOn over a session that has no id until setID
// gives it one: the index's half of a session whose id comes with
// session/new's reply. reported is every failure the writer published, as
// "cause: message".
func writerLearningID(t *testing.T, idx Index) (w *indexWriter, reported func() []string, setID func()) {
	t.Helper()
	var mu sync.Mutex
	var out []string
	id := ""
	w = newIndexWriter(
		IndexOptions{
			Store: idx, CWD: "/w", Provider: "cursor",
			TitleLine: func(s string) string { return strings.TrimSpace(s) },
		},
		"018f-the-thread",
		func() agent.Snapshot {
			mu.Lock()
			defer mu.Unlock()
			return agent.Snapshot{SessionID: id}
		},
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
		}, func() {
			mu.Lock()
			defer mu.Unlock()
			id = "fake-1"
		}
}

// userRows is every row written as the user's title, in order.
func (f *fakeIndex) userRows() []string {
	var out []string
	for _, row := range f.all() {
		if row.TitleKind == sessions.TitleKindUser {
			out = append(out, row.Title)
		}
	}
	return out
}

// TestPreIDUserTitlesWaitForTheIDAndTheNewestWins (P10): a create's title and
// two renames made on the writer before the session has its id write nothing —
// no Upsert at all, and no error — and once the id is known (sessionKnown) ONE
// row is written, the newest's. The negative control is the state before
// sessionKnown: a post lets the slot go and a pass takes the newest title and
// tries it — a pass made here, on this goroutine, before the worker serves, so
// it has been made and is over before the id arrives (review C9) — and it
// still writes nothing while there is no id, and leaves the title held.
func TestPreIDUserTitlesWaitForTheIDAndTheNewestWins(t *testing.T) {
	idx := newFakeIndex()
	w, reported, setID := writerLearningID(t, idx)
	w.offerUser("", "the created title")
	if err := w.rename("c-1/1", "first rename"); err != nil {
		t.Fatalf("a rename before the id: %v", err)
	}
	if err := w.rename("c-1/2", "second rename"); err != nil {
		t.Fatalf("a second rename before the id: %v", err)
	}
	// The pass with the slot let go of, still with no id: it takes the newest
	// title and tries it, and nothing reaches the store, and nothing is
	// reported.
	w.post(indexWork{touch: true})
	w.post(indexWork{title: "the agent's own name"})
	work := w.take()
	if work.user.title != "second rename" || work.title == "" {
		t.Fatalf("the pre-id pass took %+v, want the newest user title and the agent's", work)
	}
	w.runPending(work)
	if n := idx.tries(); n != 0 {
		t.Fatalf("%d writes before the session had an id: %+v", n, idx.all())
	}
	if again := w.take(); again.user.seq != 0 {
		t.Fatalf("a title that found no id was taken again at once: %+v", again.user)
	}
	setID()
	go w.serve()
	w.sessionKnown()
	idx.waitFor(t, 1, func() int { return len(idx.userRows()) }, "user rows")
	w.close()
	if got := idx.userRows(); len(got) != 1 || got[0] != "second rename" {
		t.Fatalf("the user rows written: %q, want the newest alone", got)
	}
	if got := reported(); len(got) != 0 {
		t.Fatalf("a title owed before the id reported %q", got)
	}
}

// TestTwoRenamesEndTheIndexOnTheSessionsTitle is review C3, through the
// engine: rename A sets the session's title and is held there, inside its
// turn, before the index has numbered it; rename B, made meanwhile, waits for
// that turn (titleTurnTaken) rather than set the session's title, number it
// and write it ahead of A. So B's title is the session's last and the index's
// last: they agree. The negative control is the same schedule with the turn
// broken — A's number taken outside it — where B finishes while A is held,
// the session ends on B and the index on A, which the select below tells
// apart: B returning while A is held is the broken schedule.
func TestTwoRenamesEndTheIndexOnTheSessionsTitle(t *testing.T) {
	idx := newFakeIndex()
	aHeld, releaseA := make(chan struct{}), make(chan struct{})
	bWaits := make(chan struct{}, 1)
	r := indexedHooked(t, idx, "018f-the-thread", &hooks{
		titleSet: func(title string) {
			if title == "A" {
				close(aHeld)
				<-releaseA
			}
		},
		titleTurnTaken: func() {
			select {
			case bWaits <- struct{}{}:
			default:
			}
		},
	})
	client := r.e.NewClientID()
	aDone := make(chan error, 1)
	go func() { aDone <- r.e.SetTitle(Command{Client: client, ID: "1"}, "A") }()
	await(t, aHeld, "rename A to be held after setting the session's title")
	bDone := make(chan error, 1)
	go func() { bDone <- r.e.SetTitle(Command{Client: client, ID: "2"}, "B") }()
	select {
	case <-bWaits:
	case err := <-bDone:
		close(releaseA)
		_ = awaitErr(t, aDone, "rename A")
		t.Fatalf("rename B (%v) finished while A was held between the session's title and the index's number: "+
			"the session ends on %q, the index on %q", err, r.s.Snapshot().Title, idx.last().Title)
	case <-time.After(watchdog):
		t.Fatalf("rename B neither waited for A's turn nor finished in %s", watchdog)
	}
	close(releaseA)
	if err := awaitErr(t, aDone, "rename A"); err != nil {
		t.Fatalf("rename A: %v", err)
	}
	if err := awaitErr(t, bDone, "rename B"); err != nil {
		t.Fatalf("rename B: %v", err)
	}
	session, rows := r.s.Snapshot().Title, idx.userRows()
	if session != "B" || len(rows) == 0 || rows[len(rows)-1] != "B" {
		t.Fatalf("the session ends on %q and the index on %q, want both on B", session, rows)
	}
}

// TestARenameWaitsForAUserTitleWriteInFlight (P10): a /rename made while the
// worker's write of the create's title is parked in the store waits for it —
// two user-title Upserts never run at once — and then lands after it, so the
// row ends on the rename. The rename has taken its number (the slot's seq)
// before the park is released, so a rename that did not wait would be inside
// the store beside it.
func TestARenameWaitsForAUserTitleWriteInFlight(t *testing.T) {
	idx := newFakeIndex()
	entered, release := idx.parkAt(1)
	w, _, setID := writerLearningID(t, idx)
	setID()
	go w.serve()
	w.offerUser("", "the created title")
	w.sessionKnown()
	await(t, entered, "the created title's write to park in Upsert")
	renamed := make(chan error, 1)
	go func() { renamed <- w.rename("c-1/1", "the rename") }()
	// The rename's number is taken in the section before its write: once it
	// is, the rename is at the write, and the write is all that is left.
	for deadline := time.Now().Add(watchdog); ; runtime.Gosched() {
		w.mu.Lock()
		seq := w.userSeq
		w.mu.Unlock()
		if seq == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rename did not take its number within %s", watchdog)
		}
	}
	if n := idx.tries(); n != 1 {
		t.Fatalf("%d Upserts with the first parked, want it alone", n)
	}
	release()
	if err := awaitErr(t, renamed, "the rename"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := idx.userRows(); len(got) != 2 || got[0] != "the created title" || got[1] != "the rename" {
		t.Fatalf("the user rows, in order: %q", got)
	}
	if peak := idx.peakLive(); peak != 1 {
		t.Fatalf("%d user-title Upserts ran at once", peak)
	}
}

// TestAnOlderUserTitleNeverLandsAfterANewerOne (P10): a write of an older
// user title that reaches the store's turn after a newer one has landed — the
// worker's pass, overtaken by a rename on its caller's goroutine — writes
// nothing at all. The negative control: the same title, not overtaken, is
// written.
func TestAnOlderUserTitleNeverLandsAfterANewerOne(t *testing.T) {
	idx := newFakeIndex()
	w, _, setID := writerLearningID(t, idx)
	setID()
	created := w.offerUser("", "the created title")
	if err := w.rename("c-1/1", "the rename"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if wrote, err := w.writeUser(created, true); wrote || err != nil {
		t.Fatalf("the overtaken title: wrote %v, %v", wrote, err)
	}
	if got := idx.userRows(); len(got) != 1 || got[0] != "the rename" {
		t.Fatalf("the user rows: %q", got)
	}
	if got := w.take(); got.user.seq != 0 {
		t.Fatalf("a landed rename left %+v owed", got.user)
	}

	// The negative control, on a writer of its own.
	idx2 := newFakeIndex()
	w2, _, setID2 := writerLearningID(t, idx2)
	setID2()
	alone := w2.offerUser("", "the created title")
	if wrote, err := w2.writeUser(alone, true); !wrote || err != nil {
		t.Fatalf("a title nothing overtook: wrote %v, %v", wrote, err)
	}
	if got := idx2.userRows(); len(got) != 1 || got[0] != "the created title" {
		t.Fatalf("the user rows: %q", got)
	}
}

// TestAFailedUserTitleWriteIsReportedAndRetriedAtTheNextPost (P10): the
// worker's write of a user title that fails is published as an IndexErr
// (cause: the command that gave it, none for a create's), held — the very
// next pass does not take it, so a failing store is not spun on — and
// retried once a post lets it go, when it lands.
func TestAFailedUserTitleWriteIsReportedAndRetriedAtTheNextPost(t *testing.T) {
	boom := errors.New("craze: not saving the session: permission denied")
	idx := newFakeIndex()
	idx.failAt(1, boom)
	w, reported, setID := writerLearningID(t, idx)
	setID()
	w.offerUser("", "the created title")
	if got := w.take(); got.user.seq != 0 {
		t.Fatalf("a title held for the id was taken: %+v", got.user)
	}
	w.sessionKnown()
	w.runPending(w.take())
	if got := reported(); len(got) != 1 || got[0] != ": "+boom.Error() {
		t.Fatalf("the failure reported %q", got)
	}
	if got := w.take(); got.user.seq != 0 {
		t.Fatalf("the failed title was taken again at once: %+v", got.user)
	}
	w.post(indexWork{touch: true})
	work := w.take()
	if work.user.title != "the created title" {
		t.Fatalf("the next post's pass took %+v", work.user)
	}
	w.runPending(work)
	if got := idx.userRows(); len(got) != 1 || got[0] != "the created title" {
		t.Fatalf("the retry wrote %q", got)
	}
	if got := w.take(); got.any() {
		t.Fatalf("something is owed after the retry landed: %+v", got)
	}
}

// TestAFailedRenameIsItsCallersAndOwesNothing: a rename whose own write fails
// returns the failure, as it always has, publishes nothing, and owes the
// index nothing more — a newer rename retries it, not the worker.
func TestAFailedRenameIsItsCallersAndOwesNothing(t *testing.T) {
	boom := errors.New("craze: not saving the session: permission denied")
	idx := newFakeIndex()
	idx.failAt(1, boom)
	w, reported, setID := writerLearningID(t, idx)
	setID()
	if err := w.rename("c-1/1", "a name"); !errors.Is(err, boom) {
		t.Fatalf("a failed rename returned %v", err)
	}
	if got := reported(); len(got) != 0 {
		t.Fatalf("a rename's own failure was published too: %q", got)
	}
	if w.owed() {
		t.Fatal("a failed rename is still owed to the worker")
	}
}

// TestCloseMakesOneLastAttemptOnAnOwedUserTitle (P10): close attempts a user
// title still owed — held after a failure, which no post let go of — once,
// within its bound, and it lands; one owed by a session that never learned
// its id is attempted, writes nothing, and close returns; and one whose write
// parks in the store is abandoned at the bound.
func TestCloseMakesOneLastAttemptOnAnOwedUserTitle(t *testing.T) {
	boom := errors.New("craze: not saving the session: permission denied")
	t.Run("held after a failure", func(t *testing.T) {
		idx := newFakeIndex()
		idx.failAt(1, boom)
		w, _, setID := writerLearningID(t, idx)
		setID()
		w.offerUser("", "the created title")
		w.sessionKnown()
		w.runPending(w.take())
		if !w.owed() {
			t.Fatal("the failed title is not owed")
		}
		go w.serve()
		w.close()
		if got := idx.userRows(); len(got) != 1 || got[0] != "the created title" {
			t.Fatalf("close's last attempt wrote %q", got)
		}
	})
	t.Run("a session with no id", func(t *testing.T) {
		idx := newFakeIndex()
		w, reported, _ := writerLearningID(t, idx)
		w.offerUser("", "the created title")
		go w.serve()
		w.close()
		if n := idx.tries(); n != 0 {
			t.Fatalf("%d writes for a session with no id", n)
		}
		if got := reported(); len(got) != 0 {
			t.Fatalf("reported %q", got)
		}
	})
	t.Run("a write parked in the store", func(t *testing.T) {
		idx := newFakeIndex()
		entered, release := idx.parkAt(1)
		defer release()
		w, _, setID := writerLearningID(t, idx)
		w.closeWait = indexCloseWait
		setID()
		w.offerUser("", "the created title")
		w.stop()
		go w.serve()
		await(t, entered, "the last attempt to park in Upsert")
		closed := make(chan struct{})
		go func() { defer close(closed); w.close() }()
		await(t, closed, "close to return with its last write parked")
	})
}

// TestAUserTitleWithNoStoreOrAnUnindexedProviderIsDone: no store, and a
// provider whose sessions stay out of the index, count as done for a user
// title as for every write — nothing is tried, nothing owed, nothing
// reported.
func TestAUserTitleWithNoStoreOrAnUnindexedProviderIsDone(t *testing.T) {
	for name, opts := range map[string]IndexOptions{
		"no store":   {CWD: "/w", Provider: "cursor"},
		"unindexed":  {Store: newFakeIndex(), CWD: "/w", Provider: "native", Unindexed: func(p string) bool { return p == "native" }},
		"no title":   {Store: newFakeIndex(), CWD: "/w", Provider: "cursor"},
		"both, none": {},
	} {
		t.Run(name, func(t *testing.T) {
			w := newIndexWriter(opts, "018f", func() agent.Snapshot { return agent.Snapshot{SessionID: "fake-1"} },
				func(cause, msg string) { t.Errorf("reported %s: %s", cause, msg) })
			t.Cleanup(w.stop)
			if name != "no title" {
				w.offerUser("", "the created title")
			}
			w.sessionKnown()
			w.runPending(w.take())
			if w.owed() {
				t.Fatal("a title is still owed")
			}
			if f, ok := opts.Store.(*fakeIndex); ok && f.tries() != 0 {
				t.Fatalf("%d writes", f.tries())
			}
		})
	}
}

// TestACreatedTitleIsWrittenOnceTheSessionIsUp is the engine's half (P10):
// an engine built with the title its session was created with
// (IndexOptions.Title) writes it as the user's — pinned — once Start has
// given the session its id, with no prompt and no event needed: Started is
// the trigger. Nothing is written before. The negative control is the same
// engine over a start that fails: no id, no row, and Close returns.
func TestACreatedTitleIsWrittenOnceTheSessionIsUp(t *testing.T) {
	build := func(t *testing.T, idx *fakeIndex, startErr error) *Engine {
		t.Helper()
		s := newFake(t, agent.EventLogOptions{NoPrimary: true})
		s.noIDYet, s.startErr = true, startErr
		e, err := newEngine(s, Options{Index: IndexOptions{
			Store: idx, CWD: "/w", Provider: "cursor", Title: "the created title",
			TitleLine: func(s string) string { return strings.TrimSpace(s) },
		}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		return e
	}
	idx := newFakeIndex()
	e := build(t, idx, nil)
	if n := idx.tries(); n != 0 {
		t.Fatalf("%d writes before the start", n)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	idx.waitRows(t, 1)
	row := idx.last()
	if row.TitleKind != sessions.TitleKindUser || row.Title != "the created title" || row.SessionID != "fake-1" || row.CrazeID == "" {
		t.Fatalf("the created title's row: %+v", row)
	}

	failed := newFakeIndex()
	e2 := build(t, failed, errors.New("the agent could not start"))
	if err := e2.Start(context.Background()); err == nil {
		t.Fatal("the failing start started")
	}
	if err := e2.Close(); err != nil {
		t.Fatal(err)
	}
	if n := failed.tries(); n != 0 {
		t.Fatalf("a session that never started was indexed %d times: %+v", n, failed.all())
	}
}

// TestACreatedTitleFailureIsAnIndexErrDelta: the engine publishes the
// worker's failure to write the created title as a StateDelta{IndexErr} with
// no cause, as every event-driven write's failure is.
func TestACreatedTitleFailureIsAnIndexErrDelta(t *testing.T) {
	boom := errors.New("craze: not saving the session: permission denied")
	idx := newFakeIndex()
	idx.setErr(boom)
	s := newFake(t, agent.EventLogOptions{NoPrimary: true})
	e, err := newEngine(s, Options{Index: IndexOptions{Store: idx, CWD: "/w", Provider: "cursor", Title: "the created title"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	sub, err := e.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	r := &rig{t: t, s: s, e: e, sub: sub}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := r.until(func(ev agent.Event) bool {
		return ev.Type == agent.EventMeta && ev.State != nil && ev.State.IndexErr != ""
	})
	if last := got[len(got)-1]; last.State.IndexErr != boom.Error() || last.Cause != "" {
		t.Fatalf("the delta carries %q (cause %q)", last.State.IndexErr, last.Cause)
	}
}
