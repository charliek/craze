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
	"github.com/charliek/craze/internal/journal"
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
		entered: make(chan struct{}),
		release: make(chan struct{}),
		wrote:   make(chan struct{}, 64),
	}
}

func (f *fakeIndex) Upsert(row sessions.Row) error {
	f.mu.Lock()
	f.attempts++
	n, from := f.attempts, f.parkFrom
	f.mu.Unlock()
	if from > 0 && n >= from {
		if n == from {
			close(f.entered)
		}
		<-f.release
	}
	f.mu.Lock()
	err := f.err
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
	return newRig(t, Options{
		CrazeSessionID: craze,
		Index: IndexOptions{
			Store:     idx,
			CWD:       "/w",
			Provider:  "cursor",
			Hidden:    func(p string) bool { return p == "native" },
			TitleLine: func(s string) string { return strings.TrimSpace(s) },
		},
	})
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
	awaitTurn(t, returned, "turn-1")
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
func TestAFailedSeedIsReportedOnceWithItsCause(t *testing.T) {
	idx := newFakeIndex()
	boom := errors.New("craze: no home directory to save the session index in")
	idx.setErr(boom)
	r := indexed(t, idx, "")
	c := Command{Client: r.e.NewClientID(), ID: "1"}
	if _, err := r.e.Submit(c, "a prompt", SubmitQueue, ""); err != nil {
		t.Fatalf("a failing index refused the prompt: %v", err)
	}
	got := r.until(func(ev agent.Event) bool {
		return ev.Type == agent.EventMeta && ev.State != nil && ev.State.IndexErr != ""
	})
	last := got[len(got)-1]
	if last.State.IndexErr != boom.Error() {
		t.Fatalf("the delta carries %q, want the store's own message", last.State.IndexErr)
	}
	if last.Cause != c.Cause() {
		t.Fatalf("the delta names %q, want the command that caused the write (%q)", last.Cause, c.Cause())
	}
	// One report per failure, not one per event: nothing else on the record up
	// to the turn's ending carries an IndexErr.
	rest := r.until(lastEnding)
	for _, ev := range rest {
		if ev.Type == agent.EventMeta && ev.State != nil && ev.State.IndexErr != "" {
			t.Fatalf("a second report for one failed write: %s", describe(rest))
		}
	}
}

// TestTheAgentTitleAndTheTurnEndAreWorkerWrites: neither has a caller — an
// observer may not do I/O — so both go to the worker, and the row they write
// carries the same durable id every other write does.
func TestTheAgentTitleAndTheTurnEndAreWorkerWrites(t *testing.T) {
	idx := newFakeIndex()
	r := indexed(t, idx, "")
	r.submit("a prompt")
	r.until(lastEnding)
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
	r.submit("a prompt")
	r.until(lastEnding)
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
	r.submit("a prompt")
	r.until(lastEnding)
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
	r.submit("a prompt")
	r.until(lastEnding)
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
