package journal

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// TestEveryLineShapeIsWrittenAsPinned pins each line type's exact bytes: ts
// first and type second, the table's keys in order, times in UTC, an event
// body embedded byte for byte (its own escapes kept, none added), and a
// diag's fields always an object.
func TestEveryLineShapeIsWrittenAsPinned(t *testing.T) {
	opts := testOptions(t)
	w := newWriter(t, opts)
	cest := time.FixedZone("CEST", 2*60*60)
	at := time.Date(2026, 9, 19, 12, 0, 1, 500_000_000, cest)
	body := `{"type":"text","text":"x\u003cy <raw> é"}`

	w.Note(SessionNote{ProviderSessionID: "sess-1", LoadedFrom: "sess-0", AgentBinary: "/usr/local/bin/cursor-agent"})
	w.Note(PromptNote{Attempt: "a1", Kind: PromptKindPrompt, Text: `fix <this> & "that"`})
	w.Append(Record{Seq: 1, At: at, EventType: "text", Body: body})
	w.Append(Record{Seq: 2, At: at, EventType: "plan", Omitted: &Omitted{Reason: OmittedOversized, Bytes: 9_000_000}})
	w.Note(PromptEndNote{Attempt: "a1", StopReason: "end_turn", Duration: 1500 * time.Millisecond})
	w.Note(DiagNote{Kind: DiagAgentStderr, Fields: map[string]any{"line": "boom"}})
	w.Note(PromptEndNote{Attempt: "a2", ErrClass: "prompt_cancelled", ErrMessage: "context canceled", Duration: 2 * time.Millisecond})
	w.Note(DiagNote{Kind: DiagClosing})
	closeWriter(t, w)

	want := []string{
		fmt.Sprintf(`{"ts":"2026-09-19T10:00:00Z","type":"header","format":1,"eventCodec":1,"incarnation":"%s","crazeVersion":"v0.0.0-test","os":"%s","arch":"%s","provider":"cursor","agentBinary":"cursor-agent","cwd":"/work/app","force":true,"interactive":true,"mode":"agent","pid":%d}`,
			testIncarnation, runtime.GOOS, runtime.GOARCH, os.Getpid()),
		`{"ts":"2026-09-19T10:00:00.001Z","type":"session","providerSessionId":"sess-1","loadedFrom":"sess-0","agentBinary":"/usr/local/bin/cursor-agent"}`,
		`{"ts":"2026-09-19T10:00:00.002Z","type":"prompt","attempt":"a1","text":"fix <this> & \"that\"","kind":"prompt"}`,
		`{"ts":"2026-09-19T10:00:00.003Z","type":"event","seq":1,"at":"2026-09-19T10:00:01.5Z","eventType":"text","event":` + body + `}`,
		`{"ts":"2026-09-19T10:00:00.004Z","type":"event","seq":2,"at":"2026-09-19T10:00:01.5Z","eventType":"plan","omitted":{"reason":"oversized","bytes":9000000}}`,
		`{"ts":"2026-09-19T10:00:00.005Z","type":"prompt_end","attempt":"a1","stopReason":"end_turn","durationMs":1500}`,
		`{"ts":"2026-09-19T10:00:00.006Z","type":"diag","kind":"agent_stderr","fields":{"line":"boom"}}`,
		`{"ts":"2026-09-19T10:00:00.007Z","type":"prompt_end","attempt":"a2","errClass":"prompt_cancelled","errMessage":"context canceled","durationMs":2}`,
		`{"ts":"2026-09-19T10:00:00.008Z","type":"diag","kind":"closing","fields":{}}`,
	}
	got := rawLines(t, w.Path())
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %s\nwant %s", i+1, got[i], want[i])
		}
	}

	// The records read back as they went in: the body byte for byte, the
	// time equal, the omitted marker intact.
	recs, err := collect(func(fn func(Record) error) error { return ReadFile(w.Path(), 1, MaxSeq, fn) })
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("read %d records, want 2", len(recs))
	}
	if recs[0].Body != body || !recs[0].At.Equal(at) || recs[0].EventType != "text" || recs[0].Omitted != nil {
		t.Errorf("record 1 = %+v, want the body %s at %v", recs[0], body, at)
	}
	if o := recs[1].Omitted; recs[1].Body != "" || o == nil || *o != (Omitted{Reason: OmittedOversized, Bytes: 9_000_000}) {
		t.Errorf("record 2 = %+v, want an oversized omitted record", recs[1])
	}
}

// TestTheFileLivesUnderTheWorkspaceSlugWithAnAbsoluteCwd: the path is
// <dir>/<slug>/<UTC stamp>_<incarnation>.jsonl, the stamp taken at New, and
// a relative workspace is made absolute before it is slugged or recorded.
func TestTheFileLivesUnderTheWorkspaceSlugWithAnAbsoluteCwd(t *testing.T) {
	t.Chdir(t.TempDir())
	opts := testOptions(t)
	opts.Cwd = filepath.Join("rel", "ws")
	abs, err := filepath.Abs(opts.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	w := newWriter(t, opts)
	want := filepath.Join(opts.Dir, Slug(abs), "20260919T100000Z_"+testIncarnation+".jsonl")
	if w.Path() != want {
		t.Fatalf("Path() = %q, want %q", w.Path(), want)
	}
	w.Append(event(1))
	closeWriter(t, w)
	lines := parsedLines(t, w.Path())
	if lines[0]["type"] != typeHeader || lines[0]["cwd"] != abs {
		t.Fatalf("line 1 = %v, want the header with cwd %q", lines[0], abs)
	}
}

// TestNewRefusesWhatCannotNameAFile: a relative directory and an
// incarnation that is not a plain name are refused before anything is
// written, as are limits the writer cannot honor.
func TestNewRefusesWhatCannotNameAFile(t *testing.T) {
	for name, mod := range map[string]func(*Options){
		"relative dir":            func(o *Options) { o.Dir = "journal" },
		"empty dir":               func(o *Options) { o.Dir = "" },
		"empty incarnation":       func(o *Options) { o.Incarnation = "" },
		"separator":               func(o *Options) { o.Incarnation = "a/b" },
		"dot dot":                 func(o *Options) { o.Incarnation = ".." },
		"backslash":               func(o *Options) { o.Incarnation = `a\b` },
		"record cap too small":    func(o *Options) { o.MaxRecordBytes = 100 },
		"record cap over reader":  func(o *Options) { o.MaxRecordBytes = MaxLineBytes },
		"queue with no free slot": func(o *Options) { o.QueueEntries = 1 },
	} {
		opts := testOptions(t)
		mod(&opts)
		if _, err := New(opts); err == nil {
			t.Errorf("%s: New succeeded, want an error", name)
		}
	}
}

// TestAppendNeverWaitsForAStalledWrite (A13): with the writer stuck inside
// Write, Append, Note and every request still return, far past the queue's
// capacity, and nothing accepted goes unaccounted once the stall ends.
func TestAppendNeverWaitsForAStalledWrite(t *testing.T) {
	hooks := newFileHooks()
	stall := newGate(t)
	hooks.onWrite = func(call int, p []byte, f *os.File) (int, error) {
		if call == 1 {
			stall.block()
		}
		return f.Write(p)
	}
	opts := testOptions(t)
	opts.openFile = hooks.open
	opts.QueueEntries = 64
	w := newWriter(t, opts)

	w.Append(event(1))
	w.RequestFlush()
	stall.waitEntered(t)

	const events, notes = 5000, 2000
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		wg.Go(func() { appendEvents(w, 2, events) })
		wg.Go(func() {
			for i := range notes {
				w.Note(DiagNote{Kind: DiagAgentStderr, Fields: map[string]any{"line": i}})
				w.RequestSync()
				w.RequestFlush()
				_ = w.Health()
				_, _ = w.Flushed()
			}
		})
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(watchdog):
		t.Fatal("Append or Note waited behind a stalled Write")
	}
	if writes, _, _ := hooks.counts(); writes != 1 {
		t.Fatalf("%d writes began during the stall, want only the stalled one", writes)
	}
	if h := w.Health(); h.State != StateGap || h.DroppedEvents == 0 {
		t.Fatalf("Health during the stall = %+v, want a gap with dropped events", h)
	}

	stall.open()
	closeWriter(t, w)
	lines := parsedLines(t, w.Path())
	var wroteEvents, wroteNotes, gapEvents, gapNotes int
	for _, l := range lines {
		switch l["type"] {
		case typeEvent:
			wroteEvents++
		case typeDiag:
			wroteNotes++
		case typeGap:
			gapEvents += atoi(t, l["droppedEvents"])
			gapNotes += atoi(t, l["droppedNotes"])
		}
	}
	h := w.Health()
	if wroteEvents+h.DroppedEvents != events || wroteNotes+h.DroppedNotes != notes {
		t.Fatalf("wrote %d events and %d notes, dropped %d and %d; want %d and %d accounted",
			wroteEvents, wroteNotes, h.DroppedEvents, h.DroppedNotes, events, notes)
	}
	if gapEvents != h.DroppedEvents || gapNotes != h.DroppedNotes {
		t.Fatalf("gap lines count %d events and %d notes, Health %d and %d", gapEvents, gapNotes, h.DroppedEvents, h.DroppedNotes)
	}
	if h.State != StateOK {
		t.Fatalf("State = %s after every gap line was written, want ok", h.State)
	}
}

func atoi(t *testing.T, v any) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscan(fmt.Sprint(v), &n); err != nil {
		t.Fatalf("%v is not a number", v)
	}
	return n
}

// TestOverflowIsAnOrderedGapBetweenItsNeighbors (A13) is plan 020's own
// example: events 1–10 queued, 11 dropped, 12 queued after the writer
// drained, written as 1–10, gap{11..11}, 12. Health records the range at
// once, returns to ok once the gap line is written, and keeps the range.
func TestOverflowIsAnOrderedGapBetweenItsNeighbors(t *testing.T) {
	hooks := newFileHooks()
	stall := newGate(t)
	hooks.onWrite = func(call int, p []byte, f *os.File) (int, error) {
		if call == 1 {
			stall.block()
		}
		return f.Write(p)
	}
	diag := &diagSink{}
	opts := testOptions(t)
	opts.openFile = hooks.open
	opts.Diag = diag
	opts.QueueEntries = 11 // ten records, and the gap marker's slot
	w := newWriter(t, opts)

	w.Append(event(1))
	w.RequestFlush()
	stall.waitEntered(t) // 1 is in flight, and still counts against the queue
	appendEvents(w, 2, 10)
	w.Append(event(11))

	h := w.Health()
	if h.State != StateGap || !slices.Equal(h.Gaps, []SeqRange{{11, 11}}) || h.DroppedEvents != 1 {
		t.Fatalf("Health after the drop = %+v, want gap state with {11 11} and one dropped event", h)
	}
	stall.open()
	waitFlushed(t, w, 11)
	w.Append(event(12))
	waitFlushed(t, w, 12)

	lines := parsedLines(t, w.Path())
	if got, want := shape(lines), "header 1 2 3 4 5 6 7 8 9 10 gap{11..11 e1 n0} 12"; got != want {
		t.Fatalf("file layout:\n got %s\nwant %s", got, want)
	}
	gap := rawLines(t, w.Path())[11]
	if want := `{"ts":"2026-09-19T10:00:00.011Z","type":"gap","fromSeq":11,"toSeq":11,"droppedEvents":1,"droppedNotes":0,"error":"journal queue full"}`; gap != want {
		t.Fatalf("gap line:\n got %s\nwant %s", gap, want)
	}
	h = w.Health()
	if h.State != StateOK || !slices.Equal(h.Gaps, []SeqRange{{11, 11}}) {
		t.Fatalf("Health after the gap line = %+v, want ok with {11 11} kept", h)
	}
	if !h.Overlaps(5, 11) || h.Overlaps(12, 12) || h.Overlaps(1, 10) {
		t.Fatalf("Overlaps disagrees with Gaps %v", h.Gaps)
	}

	// Readers agree with the file: the range up to the gap and after it are
	// served, one across it is ErrGap.
	if recs, err := collect(func(fn func(Record) error) error { return w.ReadRange(1, 10, fn) }); err != nil || !slices.Equal(seqs(recs), seqRange(1, 10)) {
		t.Fatalf("ReadRange(1, 10) = %v, %v", seqs(recs), err)
	}
	if _, err := collect(func(fn func(Record) error) error { return w.ReadRange(1, 12, fn) }); !errors.Is(err, ErrGap) {
		t.Fatalf("ReadRange(1, 12) = %v, want ErrGap", err)
	}
	if recs, err := collect(func(fn func(Record) error) error { return w.ReadRange(12, 12, fn) }); err != nil || !slices.Equal(seqs(recs), []uint64{12}) {
		t.Fatalf("ReadRange(12, 12) = %v, %v", seqs(recs), err)
	}
	closeWriter(t, w)
	if n := len(diag.lines()); n != 1 {
		t.Fatalf("%d notices, want exactly one: %q", n, diag.lines())
	}
	if msg := diag.lines()[0]; !strings.HasPrefix(msg, "journal: ") || !strings.Contains(msg, "gap") {
		t.Fatalf("notice %q, want it to say the journal has a gap", msg)
	}
}

// TestANoteOnlyOverflowIsAGapWithCountsAndNoRange (A13): notes have no seq,
// so a gap of only notes counts them and names no range, and Health records
// no missing seq.
func TestANoteOnlyOverflowIsAGapWithCountsAndNoRange(t *testing.T) {
	hooks := newFileHooks()
	stall := newGate(t)
	hooks.onWrite = func(call int, p []byte, f *os.File) (int, error) {
		if call == 1 {
			stall.block()
		}
		return f.Write(p)
	}
	opts := testOptions(t)
	opts.openFile = hooks.open
	opts.QueueEntries = 3
	w := newWriter(t, opts)

	w.Append(event(1))
	w.RequestFlush()
	stall.waitEntered(t)
	w.Note(DiagNote{Kind: DiagAgentStderr, Fields: map[string]any{"line": "kept"}})
	w.Note(DiagNote{Kind: DiagAgentStderr, Fields: map[string]any{"line": "dropped 1"}})
	w.Note(PromptNote{Attempt: "a1", Kind: PromptKindPrompt, Text: "dropped 2"})
	if h := w.Health(); h.State != StateGap || len(h.Gaps) != 0 || h.DroppedNotes != 2 {
		t.Fatalf("Health = %+v, want gap state, no missing seqs, two dropped notes", h)
	}
	stall.open()
	closeWriter(t, w)

	lines := parsedLines(t, w.Path())
	if got, want := shape(lines), "header 1 diag gap{<nil>..<nil> e0 n2}"; got != want {
		t.Fatalf("file layout:\n got %s\nwant %s", got, want)
	}
	if _, ok := lines[3]["fromSeq"]; ok {
		t.Fatalf("a note-only gap line has fromSeq: %v", lines[3])
	}
	if h := w.Health(); h.State != StateOK || len(h.Gaps) != 0 || h.DroppedNotes != 2 {
		t.Fatalf("Health after close = %+v", h)
	}
}

// TestAPartialWriteFailsTheWriterWithOneNoticeOffTheAppendPath (A13): a
// Write that lands one whole line and part of the next, then errors, leaves
// the boundary after the whole line, the rest counted from the first
// incomplete line as an open-ended gap, and exactly one notice, written by
// the writer goroutine: a Diag that blocks cannot hold up Append or Note.
func TestAPartialWriteFailsTheWriterWithOneNoticeOffTheAppendPath(t *testing.T) {
	hooks := newFileHooks()
	hooks.onWrite = func(call int, p []byte, f *os.File) (int, error) {
		if call == 1 {
			return f.Write(p)
		}
		// Event 2's line whole, then five bytes of event 3's.
		cut := strings.IndexByte(string(p), '\n') + 1 + 5
		n, _ := f.Write(p[:cut])
		return n, errors.New("injected: no space left on device")
	}
	diagStall := newGate(t)
	diag := &diagSink{gate: diagStall}
	opts := testOptions(t)
	opts.openFile = hooks.open
	opts.Diag = diag
	w := newWriter(t, opts)

	w.Append(event(1))
	waitFlushed(t, w, 1)
	_, before := w.Flushed()
	appendEvents(w, 2, 4)
	w.RequestFlush()
	diagStall.waitEntered(t) // the writer failed and is printing its notice

	appended := make(chan struct{})
	go func() {
		w.Append(event(5))
		w.Note(PromptNote{Attempt: "a1", Kind: PromptKindPrompt, Text: "after the failure"})
		close(appended)
	}()
	select {
	case <-appended:
	case <-time.After(watchdog):
		t.Fatal("Append waited for the notice")
	}

	h := w.Health()
	if h.State != StateFailed || !strings.Contains(h.Err, "no space left") {
		t.Fatalf("Health = %+v, want failed with the write's error", h)
	}
	if !slices.Equal(h.Gaps, []SeqRange{{3, MaxSeq}}) {
		t.Fatalf("Gaps = %v, want everything from 3 on", h.Gaps)
	}
	if h.DroppedEvents != 3 || h.DroppedNotes != 1 {
		t.Fatalf("dropped %d events and %d notes, want 3 (3, 4 unwritten; 5 refused) and 1", h.DroppedEvents, h.DroppedNotes)
	}
	seq, bytesFlushed := w.Flushed()
	line2 := rawLines(t, w.Path())[2]
	if seq != 2 || bytesFlushed != before+int64(len(line2))+1 {
		t.Fatalf("Flushed = %d, %d; want seq 2 at %d bytes", seq, bytesFlushed, before+int64(len(line2))+1)
	}
	if st, err := os.Stat(w.Path()); err != nil || st.Size() != bytesFlushed+5 {
		t.Fatalf("file size %v (%v), want the boundary plus the 5 torn bytes", st.Size(), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	if err := w.WaitFlushed(ctx, 3); !errors.Is(err, ErrFailed) {
		t.Fatalf("WaitFlushed(3) = %v, want ErrFailed", err)
	}

	diagStall.open()
	waitExited(t, w)
	if n := len(diag.lines()); n != 1 || !strings.Contains(diag.lines()[0], "no space left") {
		t.Fatalf("notices %q, want exactly one naming the error", diag.lines())
	}
	if _, _, closes := hooks.counts(); closes != 1 {
		t.Fatalf("the file was closed %d times, want once", closes)
	}
	if err := w.Close(context.Background()); !errors.Is(err, ErrFailed) {
		t.Fatalf("Close = %v, want ErrFailed", err)
	}

	// The dead file reads up to the tear, and no further.
	recs, err := collect(func(fn func(Record) error) error { return ReadFile(w.Path(), 1, MaxSeq, fn) })
	if err != nil || !slices.Equal(seqs(recs), []uint64{1, 2}) {
		t.Fatalf("ReadFile = %v, %v; want 1 and 2 with the torn tail ignored", seqs(recs), err)
	}
	if _, err := collect(func(fn func(Record) error) error { return ReadFile(w.Path(), 1, 3, fn) }); !errors.Is(err, ErrGap) {
		t.Fatalf("ReadFile(1, 3) = %v, want ErrGap", err)
	}
}

// TestASyncErrorFailsTheWriter: an fsync error is as final as a write error;
// what was written stays readable and the gap starts after it.
func TestASyncErrorFailsTheWriter(t *testing.T) {
	hooks := newFileHooks()
	hooks.onSync = func(int, *os.File) error { return errors.New("injected: EIO") }
	diag := &diagSink{}
	opts := testOptions(t)
	opts.openFile = hooks.open
	opts.Diag = diag
	w := newWriter(t, opts)

	w.Append(event(1))
	w.RequestSync()
	waitExited(t, w)
	h := w.Health()
	if h.State != StateFailed || !slices.Equal(h.Gaps, []SeqRange{{2, MaxSeq}}) || !strings.Contains(h.Err, "EIO") {
		t.Fatalf("Health = %+v, want failed with {2 max}", h)
	}
	if n := len(diag.lines()); n != 1 {
		t.Fatalf("%d notices, want one", n)
	}
	if recs, err := collect(func(fn func(Record) error) error { return w.ReadRange(1, 1, fn) }); err != nil || len(recs) != 1 {
		t.Fatalf("ReadRange(1, 1) after a sync failure = %v, %v; the written line stays readable", seqs(recs), err)
	}
}

// TestADroppedPromptEndStillAsksForASync (A13): the turn's end is when
// durability matters, so the sync is requested even when the queue has no
// room for the note. Nothing else in this test asks for one.
func TestADroppedPromptEndStillAsksForASync(t *testing.T) {
	hooks := newFileHooks()
	stall := newGate(t)
	hooks.onWrite = func(call int, p []byte, f *os.File) (int, error) {
		if call == 1 {
			stall.block()
		}
		return f.Write(p)
	}
	opts := testOptions(t)
	opts.openFile = hooks.open
	opts.QueueEntries = 2 // event 1 in flight fills it
	w := newWriter(t, opts)

	w.Append(event(1))
	w.RequestFlush()
	stall.waitEntered(t)
	w.Note(PromptEndNote{Attempt: "a1", StopReason: "end_turn"})
	if h := w.Health(); h.DroppedNotes != 1 {
		t.Fatalf("Health = %+v, want the prompt_end dropped", h)
	}
	if _, syncs, _ := hooks.counts(); syncs != 0 {
		t.Fatalf("%d syncs before the stall ended", syncs)
	}
	stall.open()
	hooks.waitFor(t, "the sync the dropped prompt_end asked for", func(_, syncs, _ int) bool { return syncs == 1 })
	if lines := parsedLines(t, w.Path()); shape(lines) != "header 1 gap{<nil>..<nil> e0 n1}" {
		t.Fatalf("file layout %s, want the prompt_end counted in a gap", shape(lines))
	}
}

// TestAWrittenPromptEndIsSynced: a prompt_end that is queued is fsynced
// along with everything before it, with no request from the caller. The
// Note's own sync request can be served by a batch taken just before the
// note, so what is asserted is that some fsync ran with the prompt_end
// already in the file: the writer syncs after every batch that wrote one.
func TestAWrittenPromptEndIsSynced(t *testing.T) {
	hooks := newFileHooks()
	synced := make(chan struct{})
	var once sync.Once
	hooks.onSync = func(_ int, f *os.File) error {
		if raw, _ := os.ReadFile(f.Name()); strings.Contains(string(raw), `"type":"prompt_end"`) {
			once.Do(func() { close(synced) })
		}
		return f.Sync()
	}
	opts := testOptions(t)
	opts.openFile = hooks.open
	w := newWriter(t, opts)
	w.Note(PromptNote{Attempt: "a1", Kind: PromptKindPrompt, Text: "hi"})
	w.Append(event(1))
	w.Note(PromptEndNote{Attempt: "a1", StopReason: "end_turn"})
	select {
	case <-synced:
	case <-time.After(watchdog):
		t.Fatal("no fsync ran after the prompt_end was written")
	}
	if got := shape(parsedLines(t, w.Path())); got != "header prompt 1 prompt_end" {
		t.Fatalf("file layout at the sync: %s", got)
	}
}

// TestTheFlushTimerWritesABufferedLine: a line waits in the buffer until the
// timer the writer armed for it fires. The test fires it by hand.
func TestTheFlushTimerWritesABufferedLine(t *testing.T) {
	hooks := newFileHooks()
	armed := make(chan *idleTimer, 4)
	opts := testOptions(t)
	opts.openFile = hooks.open
	opts.newTimer = func(d time.Duration) flushTimer {
		if d != defaultFlushInterval {
			t.Errorf("timer armed for %v, want %v", d, defaultFlushInterval)
		}
		tm := &idleTimer{c: make(chan time.Time, 1)}
		armed <- tm
		return tm
	}
	w := newWriter(t, opts)
	w.Append(event(1))
	var tm *idleTimer
	select {
	case tm = <-armed:
	case <-time.After(watchdog):
		t.Fatal("no timer was armed for a buffered line")
	}
	if writes, _, _ := hooks.counts(); writes != 0 {
		t.Fatalf("%d writes before the timer fired", writes)
	}
	tm.c <- time.Now()
	waitBoundary(t, w, 1)
	if writes, _, _ := hooks.counts(); writes != 1 {
		t.Fatalf("%d writes for the timer's flush, want 1", writes)
	}
}

// TestTheLineThresholdWritesWithoutARequest: 512 buffered lines (here, a
// test's four) go to the OS with no request and no timer.
func TestTheLineThresholdWritesWithoutARequest(t *testing.T) {
	hooks := newFileHooks()
	opts := testOptions(t)
	opts.openFile = hooks.open
	opts.flushLines = 4
	w := newWriter(t, opts)
	appendEvents(w, 1, 3) // with the header, four lines
	waitBoundary(t, w, 3)
	if writes, _, _ := hooks.counts(); writes != 1 {
		t.Fatalf("%d writes, want the threshold's one", writes)
	}
}

// TestTheByteThresholdWritesWithoutARequest: likewise for 1 MiB (here, one
// byte: every line).
func TestTheByteThresholdWritesWithoutARequest(t *testing.T) {
	hooks := newFileHooks()
	opts := testOptions(t)
	opts.openFile = hooks.open
	opts.flushBytes = 1
	w := newWriter(t, opts)
	w.Append(event(1))
	waitBoundary(t, w, 1)
}

// TestCloseReturnsWhileAWriteIsStalled (A14): Close's bound is the caller's;
// the writer keeps the file through the stall, and once the Write returns it
// finishes, syncs, closes the descriptor exactly once, and exits.
func TestCloseReturnsWhileAWriteIsStalled(t *testing.T) {
	hooks := newFileHooks()
	stall := newGate(t)
	hooks.onWrite = func(call int, p []byte, f *os.File) (int, error) {
		if call == 1 {
			stall.block()
		}
		return f.Write(p)
	}
	opts := testOptions(t)
	opts.openFile = hooks.open
	opts.closeWait = time.Millisecond
	w := newWriter(t, opts)

	w.Append(event(1))
	w.RequestFlush()
	stall.waitEntered(t)
	w.Append(event(2))
	if err := w.Close(context.Background()); !errors.Is(err, ErrStalled) {
		t.Fatalf("Close during the stall = %v, want ErrStalled", err)
	}
	if err := w.Close(context.Background()); !errors.Is(err, ErrStalled) {
		t.Fatalf("a second Close during the stall = %v, want ErrStalled", err)
	}
	assertStillOwned(t, w, hooks)

	stall.open()
	waitExited(t, w)
	if writes, syncs, closes := hooks.counts(); closes != 1 || syncs != 1 {
		t.Fatalf("after the stall: %d writes, %d syncs, %d closes; want the close's sync and one close", writes, syncs, closes)
	}
	if got := shape(parsedLines(t, w.Path())); got != "header 1 2" {
		t.Fatalf("file layout %s, want everything queued before Close written", got)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close after the writer finished = %v", err)
	}
	if _, _, closes := hooks.counts(); closes != 1 {
		t.Fatalf("closed %d times after a third Close", closes)
	}
}

// TestCloseReturnsWhileASyncIsStalled (A14): the same for a stalled fsync.
func TestCloseReturnsWhileASyncIsStalled(t *testing.T) {
	hooks := newFileHooks()
	stall := newGate(t)
	hooks.onSync = func(call int, f *os.File) error {
		if call == 1 {
			stall.block()
		}
		return f.Sync()
	}
	opts := testOptions(t)
	opts.openFile = hooks.open
	opts.closeWait = time.Millisecond
	w := newWriter(t, opts)

	w.Append(event(1))
	w.RequestSync()
	stall.waitEntered(t)
	if err := w.Close(context.Background()); !errors.Is(err, ErrStalled) {
		t.Fatalf("Close during the stall = %v, want ErrStalled", err)
	}
	assertStillOwned(t, w, hooks)

	stall.open()
	waitExited(t, w)
	if _, syncs, closes := hooks.counts(); closes != 1 || syncs != 1 {
		t.Fatalf("after the stall: %d syncs, %d closes; want one of each (nothing new to sync at close)", syncs, closes)
	}
	if h := w.Health(); h.State != StateOK {
		t.Fatalf("Health = %+v", h)
	}
}

// TestCloseHonorsAnEndedContextDuringAStall: ctx bounds Close as well as the
// 500 ms; the stall is still the writer's.
func TestCloseHonorsAnEndedContextDuringAStall(t *testing.T) {
	hooks := newFileHooks()
	stall := newGate(t)
	hooks.onWrite = func(call int, p []byte, f *os.File) (int, error) {
		stall.block()
		return f.Write(p)
	}
	opts := testOptions(t)
	opts.openFile = hooks.open
	w := newWriter(t, opts) // the bound itself is the watchdog: only ctx can end this Close
	w.Append(event(1))
	w.RequestFlush()
	stall.waitEntered(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Close(ctx); !errors.Is(err, ErrStalled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Close with an ended ctx = %v, want ErrStalled wrapping context.Canceled", err)
	}
	assertStillOwned(t, w, hooks)
	stall.open()
	waitExited(t, w)
	if _, _, closes := hooks.counts(); closes != 1 {
		t.Fatalf("closed %d times, want once", closes)
	}
}

// assertStillOwned: after Close gave up, the writer goroutine is still
// running and nothing has closed the descriptor under it.
func assertStillOwned(t *testing.T, w *Writer, hooks *fileHooks) {
	t.Helper()
	select {
	case <-w.exited:
		t.Fatal("the writer exited during the stall")
	default:
	}
	if _, _, closes := hooks.counts(); closes != 0 {
		t.Fatalf("the descriptor was closed %d times during the stall", closes)
	}
}

// TestCloseDrainsSyncsAndClosesAHealthyWriter: without a stall, Close waits
// for everything queued, one sync and one close.
func TestCloseDrainsSyncsAndClosesAHealthyWriter(t *testing.T) {
	hooks := newFileHooks()
	opts := testOptions(t)
	opts.openFile = hooks.open
	w := newWriter(t, opts)
	appendEvents(w, 1, 100)
	closeWriter(t, w)
	if _, syncs, closes := hooks.counts(); syncs != 1 || closes != 1 {
		t.Fatalf("%d syncs and %d closes, want one of each", syncs, closes)
	}
	recs, err := collect(func(fn func(Record) error) error { return ReadFile(w.Path(), 1, 100, fn) })
	if err != nil || len(recs) != 100 {
		t.Fatalf("ReadFile = %d records, %v", len(recs), err)
	}
	// Anything after Close is counted, never written.
	w.Append(event(101))
	w.Note(DiagNote{Kind: DiagClosing})
	if h := w.Health(); h.DroppedEvents != 1 || h.DroppedNotes != 1 || h.State != StateOK {
		t.Fatalf("Health after late entries = %+v", h)
	}
	if got := len(rawLines(t, w.Path())); got != 101 {
		t.Fatalf("%d lines after late entries, want the header and 100 events", got)
	}
}

// TestAClosingNoteAloneCreatesNothing (A26): a session built and closed
// without doing anything leaves no file and no directory, even though its
// log wrote the closing diag it always writes.
func TestAClosingNoteAloneCreatesNothing(t *testing.T) {
	opts := testOptions(t)
	w := newWriter(t, opts)
	w.Note(DiagNote{Kind: DiagClosing, Fields: map[string]any{"droppedAtClose": 0}})
	closeWriter(t, w)
	if _, err := os.Stat(opts.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(%s) = %v, want nothing created", opts.Dir, err)
	}
	if h := w.Health(); h.State != StateOK || h.DroppedNotes != 0 {
		t.Fatalf("Health = %+v; a discarded closing note is not a loss", h)
	}
}

// TestAnUnusedWriterOwnsNoGoroutineAndCreatesNothing: requests alone start
// nothing, Close returns at once, and waiting for a flush after it says
// closed.
func TestAnUnusedWriterOwnsNoGoroutineAndCreatesNothing(t *testing.T) {
	opts := testOptions(t)
	w := newWriter(t, opts)
	w.RequestSync()
	w.RequestFlush()
	w.mu.Lock()
	started := w.started
	w.mu.Unlock()
	if started {
		t.Fatal("a writer with nothing to write started its goroutine")
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.exited:
		t.Fatal("exited is closed: a goroutine was started and finished")
	default:
	}
	if err := w.WaitFlushed(context.Background(), 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("WaitFlushed after Close = %v, want ErrClosed", err)
	}
	if _, err := os.Stat(opts.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(%s) = %v, want nothing created", opts.Dir, err)
	}
}

// TestCreationIsExclusive (A20): a file already at the path is never
// overwritten or appended to; the journal fails instead, and a live reader
// does not read the stranger's file.
func TestCreationIsExclusive(t *testing.T) {
	diag := &diagSink{}
	opts := testOptions(t)
	opts.Diag = diag
	w := newWriter(t, opts)
	if err := os.MkdirAll(filepath.Dir(w.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	const theirs = "someone else's journal\n"
	if err := os.WriteFile(w.Path(), []byte(theirs), 0o600); err != nil {
		t.Fatal(err)
	}
	w.Append(event(1))
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	if err := w.WaitFlushed(ctx, 1); !errors.Is(err, ErrFailed) {
		t.Fatalf("WaitFlushed = %v, want ErrFailed", err)
	}
	waitExited(t, w)
	if raw, _ := os.ReadFile(w.Path()); string(raw) != theirs {
		t.Fatalf("the existing file now holds %q", raw)
	}
	h := w.Health()
	if h.State != StateFailed || !slices.Equal(h.Gaps, []SeqRange{{1, MaxSeq}}) || h.DroppedEvents != 1 {
		t.Fatalf("Health = %+v, want failed with everything missing", h)
	}
	if n := len(diag.lines()); n != 1 || !strings.Contains(diag.lines()[0], "exists") {
		t.Fatalf("notices %q, want one saying the file exists", diag.lines())
	}
	if recs, err := collect(func(fn func(Record) error) error { return w.ReadRange(1, MaxSeq, fn) }); err != nil || len(recs) != 0 {
		t.Fatalf("ReadRange = %v, %v; want nothing read from a file the journal never wrote", seqs(recs), err)
	}
}

// TestAnExistingDirectoryIsUsedAsItIs (A20): the journal creates what is
// missing and never tightens (or loosens) a directory that is there.
func TestAnExistingDirectoryIsUsedAsItIs(t *testing.T) {
	opts := testOptions(t)
	w := newWriter(t, opts)
	dir := filepath.Dir(w.Path())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // the umask may have narrowed MkdirAll
		t.Fatal(err)
	}
	w.Append(event(1))
	closeWriter(t, w)
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("the existing directory's mode is %v (%v), want 0755 untouched", st.Mode().Perm(), err)
	}
	if st, err := os.Stat(w.Path()); err != nil || st.Mode().Perm()&^0o600 != 0 {
		t.Fatalf("the file's mode is %v (%v), want no broader than 0600", st.Mode().Perm(), err)
	}
}

// TestNewDirectoriesAndTheFileAreOwnerOnly: every directory the journal
// makes is no broader than 0700 and the file no broader than 0600.
func TestNewDirectoriesAndTheFileAreOwnerOnly(t *testing.T) {
	opts := testOptions(t)
	w := newWriter(t, opts)
	w.Append(event(1))
	closeWriter(t, w)
	for _, dir := range []string{opts.Dir, filepath.Dir(w.Path())} {
		if st, err := os.Stat(dir); err != nil || st.Mode().Perm()&^0o700 != 0 {
			t.Fatalf("%s mode %v (%v), want no broader than 0700", dir, st.Mode().Perm(), err)
		}
	}
	if st, err := os.Stat(w.Path()); err != nil || st.Mode().Perm()&^0o600 != 0 {
		t.Fatalf("file mode %v (%v), want no broader than 0600", st.Mode().Perm(), err)
	}
}

// TestALongPromptIsCutToTheCapAndMarked: a note's line never exceeds
// MaxRecordBytes, even when escaping inflates its text; the cut keeps valid
// UTF-8 and a prefix of what was typed, and says it was cut.
func TestALongPromptIsCutToTheCapAndMarked(t *testing.T) {
	const cap = 64 << 10
	opts := testOptions(t)
	opts.MaxRecordBytes = cap
	w := newWriter(t, opts)
	runes := strings.Repeat("é", 100<<10)      // 200 KiB, cut at acceptance and again at write
	controls := strings.Repeat("\x01", 60<<10) // under the cap raw, six times over escaped
	w.Note(PromptNote{Attempt: "a1", Kind: PromptKindPrompt, Text: runes})
	w.Note(PromptNote{Attempt: "a2", Kind: PromptKindInterject, Text: controls})
	w.Note(PromptEndNote{Attempt: "a2", ErrClass: "x", ErrMessage: strings.Repeat("\x02", 60<<10)})
	w.Note(PromptNote{Attempt: "a3", Kind: PromptKindPrompt, Text: "short"})
	closeWriter(t, w)

	raw := rawLines(t, w.Path())
	lines := parsedLines(t, w.Path())
	for i, want := range []struct {
		field string
		orig  string
	}{{"text", runes}, {"text", controls}, {"errMessage", strings.Repeat("\x02", 60<<10)}} {
		line, l := raw[i+1], lines[i+1]
		if len(line) > cap {
			t.Errorf("line %d is %d bytes, over the %d cap", i+2, len(line), cap)
		}
		got, _ := l[want.field].(string)
		if l["truncated"] != true || !utf8.ValidString(got) || !strings.HasPrefix(want.orig, got) || len(got) == 0 {
			t.Errorf("line %d: truncated=%v, %s is %d bytes (valid %v, a prefix %v)", i+2, l["truncated"], want.field, len(got), utf8.ValidString(got), strings.HasPrefix(want.orig, got))
		}
	}
	if _, ok := lines[4]["truncated"]; ok || lines[4]["text"] != "short" {
		t.Errorf("a short prompt was marked: %v", lines[4])
	}
}

// TestNotesWithNoFieldToCutAreReplacedNotWrittenLarge: an oversized diag
// field is cut to the cap and marked, a note with nothing left to give
// becomes a small note_too_large diag, and a field DiagNote does not allow
// is the unsupported marker; no line ever exceeds the cap.
func TestNotesWithNoFieldToCutAreReplacedNotWrittenLarge(t *testing.T) {
	const cap = 8 << 10
	opts := testOptions(t)
	opts.MaxRecordBytes = cap
	w := newWriter(t, opts)
	big := strings.Repeat("x", 3*cap)
	w.Note(DiagNote{Kind: DiagAgentStderr, Fields: map[string]any{"line": big}})
	w.Note(SessionNote{ProviderSessionID: big})
	w.Note(DiagNote{Kind: DiagStartFailed, Fields: map[string]any{"bad": make(chan int)}})
	closeWriter(t, w)
	for i, line := range rawLines(t, w.Path()) {
		if len(line) > cap {
			t.Errorf("line %d is %d bytes, over the cap", i+1, len(line))
		}
	}
	got := rawLines(t, w.Path())[1:]
	for i, want := range []string{
		`"kind":"agent_stderr","fields":{"line":"xxxx`,
		`"kind":"note_too_large","fields":{"bytes":` + fmt.Sprint(3*cap) + `,"type":"session"},"truncated":true`,
		`"kind":"start_failed","fields":{"bad":"<unsupported>"}}`,
	} {
		if !strings.Contains(got[i], want) {
			t.Errorf("line %d = %.200s, want it to contain %s", i+2, got[i], want)
		}
	}
	if !strings.HasSuffix(got[0], `"},"truncated":true}`) {
		t.Errorf("line 2 ends %q, want the cut marked", got[0][len(got[0])-40:])
	}
}

// callTrap is a diag field value whose every method a journal could be
// tempted to call counts the call and then blocks until the test ends.
type callTrap struct {
	calls *atomic.Int32
	stall *gate
}

func (c callTrap) trap()                        { c.calls.Add(1); c.stall.block() }
func (c callTrap) MarshalJSON() ([]byte, error) { c.trap(); return []byte(`"ran"`), nil }
func (c callTrap) String() string               { c.trap(); return "ran" }
func (c callTrap) Error() string                { c.trap(); return "ran" }

// textTrap is a callTrap reached through encoding.TextMarshaler, which
// encoding/json calls when a type has no MarshalJSON.
type textTrap struct{ c callTrap }

func (t textTrap) MarshalText() ([]byte, error) { t.c.trap(); return []byte("ran"), nil }

// namedString is a named type over string with no methods at all: still not
// a string, so still not allowed.
type namedString string

// TestDiagFieldsNeverCallIntoTheCallersValues (finding 1): Note runs inside
// the session's ordering boundary, so no method of a field's value is ever
// called, whatever it implements. Note returns at once holding values whose
// MarshalJSON or MarshalText would block forever; every value DiagNote does
// not allow (a named type, a Duration, a map, an []int, a pointer) is the
// unsupported marker; plain scalars, strings and []string are kept as they
// are. A non-finite float, which JSON has no number for, is a string rather
// than a failure that costs the note its other fields.
func TestDiagFieldsNeverCallIntoTheCallersValues(t *testing.T) {
	w := newWriter(t, testOptions(t))
	var calls atomic.Int32
	trap := callTrap{calls: &calls, stall: newGate(t)}
	noted := make(chan struct{})
	go func() {
		defer close(noted)
		w.Note(DiagNote{Kind: DiagStartFailed, Fields: map[string]any{
			"marshaler": trap,
			"pointer":   &trap,
			"text":      textTrap{trap},
			"named":     namedString("x"),
			"duration":  time.Second,
			"map":       map[string]any{"a": 1},
			"ints":      []int{1},
			"s":         "plain <kept>",
			"b":         true,
			"i":         -3,
			"u":         uint8(7),
			"f":         1.5,
			"f32":       float32(0.25),
			"nil":       nil,
			"list":      []string{"a", "b"},
			"nilList":   []string(nil),
		}})
	}()
	select {
	case <-noted:
	case <-time.After(watchdog):
		t.Fatal("Note waited on a method of a field's value")
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("Note called %d methods of the fields' values", n)
	}
	w.Note(DiagNote{Kind: DiagStartFailed, Fields: map[string]any{
		"nan": math.NaN(), "inf": math.Inf(-1), "f32inf": float32(math.Inf(1)), "kept": "yes",
	}})
	closeWriter(t, w)
	raw := rawLines(t, w.Path())
	for i, want := range []string{
		`"kind":"start_failed","fields":{"b":true,"duration":"<unsupported>","f":1.5,"f32":0.25,"i":-3,` +
			`"ints":"<unsupported>","list":["a","b"],"map":"<unsupported>","marshaler":"<unsupported>",` +
			`"named":"<unsupported>","nil":null,"nilList":null,"pointer":"<unsupported>","s":"plain <kept>",` +
			`"text":"<unsupported>","u":7}}`,
		`"kind":"start_failed","fields":{"f32inf":"+Inf","inf":"-Inf","kept":"yes","nan":"NaN"}}`,
	} {
		if !strings.HasSuffix(raw[i+1], want) {
			t.Errorf("diag line %d:\n got %s\nwant it to end %s", i+2, raw[i+1], want)
		}
	}
}

// TestAHugeDiagFieldIsCutBeforeItIsEncoded (finding 1): a field far over
// the note cap costs Note no more than the cap. Strings and []string
// elements are cut between runes before anything is encoded, so a 16 MiB
// value that would escape to 96 MiB is never escaped; the texts share the
// room left after the scalars, which keep their place; the line is marked
// truncated and fits the cap. A diag with more fields than it keeps is
// reduced to their count.
func TestAHugeDiagFieldIsCutBeforeItIsEncoded(t *testing.T) {
	const cap = 8 << 10
	opts := testOptions(t)
	opts.MaxRecordBytes = cap
	w := newWriter(t, opts)
	huge := strings.Repeat("\x01", 16<<20)
	list := slices.Repeat([]string{"é"}, 1<<16)
	many := make(map[string]any, 65)
	for i := range 65 {
		many[fmt.Sprint("k", i)] = i
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	w.Note(DiagNote{Kind: DiagAgentStderr, Fields: map[string]any{"line": huge, "list": list, "truncated": true, "n": 3}})
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 4<<20 {
		t.Fatalf("Note allocated %d bytes for a diag capped at %d", grew, cap)
	}
	w.Note(DiagNote{Kind: DiagAgentStderr, Fields: many})
	closeWriter(t, w)

	raw := rawLines(t, w.Path())
	if len(raw[1]) > cap {
		t.Fatalf("the diag line is %d bytes, over the %d cap", len(raw[1]), cap)
	}
	l := parsedLines(t, w.Path())[1]
	fields, _ := l["fields"].(map[string]any)
	line, _ := fields["line"].(string)
	elems, _ := fields["list"].([]any)
	if l["truncated"] != true || fields["truncated"] != true || fields["n"] != json.Number("3") {
		t.Fatalf("diag line %v: want it marked truncated with the scalars kept", l)
	}
	if line == "" || !strings.HasPrefix(huge, line) || len(elems) == 0 || len(elems) >= len(list) {
		t.Fatalf("line is %d bytes (a prefix %v), list has %d of %d: want both cut, neither emptied",
			len(line), strings.HasPrefix(huge, line), len(elems), len(list))
	}
	for _, e := range elems {
		if e != "é" {
			t.Fatalf("list element %q, want only whole elements", e)
		}
	}
	if want := `"fields":{"omittedFields":65},"truncated":true}`; !strings.HasSuffix(raw[2], want) {
		t.Fatalf("a diag of 65 fields:\n got %s\nwant it to end %s", raw[2], want)
	}
}

// TestDiagFieldsAreFrozenWhenNoted: the caller may reuse its map as soon as
// Note returns.
func TestDiagFieldsAreFrozenWhenNoted(t *testing.T) {
	opts := testOptions(t)
	w := newWriter(t, opts)
	fields := map[string]any{"line": "first"}
	w.Note(DiagNote{Kind: DiagAgentStderr, Fields: fields})
	fields["line"] = "changed"
	w.Note(&DiagNote{Kind: DiagAgentStderr, Fields: fields})
	var nilNote *PromptNote
	w.Note(nilNote)
	w.Note(nil)
	closeWriter(t, w)
	lines := parsedLines(t, w.Path())
	if got := shape(lines); got != "header diag diag" {
		t.Fatalf("file layout %s", got)
	}
	first := lines[1]["fields"].(map[string]any)["line"]
	second := lines[2]["fields"].(map[string]any)["line"]
	if first != "first" || second != "changed" {
		t.Fatalf("fields %v then %v, want each as it was when noted", first, second)
	}
}

// TestBodiesTheJournalCannotEmbedBecomeOmittedRecords: a body that is not
// one JSON object, or is over the cap, keeps its seq as an omitted record,
// and Health counts every omitted line.
func TestBodiesTheJournalCannotEmbedBecomeOmittedRecords(t *testing.T) {
	const cap = 8 << 10
	opts := testOptions(t)
	opts.MaxRecordBytes = cap
	w := newWriter(t, opts)
	w.Append(Record{Seq: 1, At: testBase, EventType: "text", Body: `{"broken":`})
	w.Append(Record{Seq: 2, At: testBase, EventType: "text", Body: `["not","an","object"]`})
	w.Append(Record{Seq: 3, At: testBase, EventType: "plan", Body: `{"text":"` + strings.Repeat("p", cap) + `"}`})
	w.Append(Record{Seq: 4, At: testBase, EventType: "tool", Omitted: &Omitted{Reason: OmittedEncodeError, Error: strings.Repeat("e", 10<<10)}})
	w.Append(Record{Seq: 5, At: testBase, EventType: "text", Body: "\n {\n\"multi\": \"line\"\n}\n"})
	closeWriter(t, w)

	recs, err := collect(func(fn func(Record) error) error { return ReadFile(w.Path(), 1, 5, fn) })
	if err != nil || len(recs) != 5 {
		t.Fatalf("ReadFile = %d records, %v", len(recs), err)
	}
	for i, want := range []string{OmittedEncodeError, OmittedEncodeError, OmittedOversized, OmittedEncodeError} {
		if o := recs[i].Omitted; o == nil || o.Reason != want || recs[i].Body != "" {
			t.Errorf("record %d = %+v, want omitted %s", i+1, recs[i], want)
		}
	}
	if o := recs[2].Omitted; o.Bytes != cap+len(`{"text":""}`) {
		t.Errorf("oversized record says %d bytes", o.Bytes)
	}
	if o := recs[3].Omitted; len(o.Error) > maxOmittedError {
		t.Errorf("an omitted error of %d bytes was kept whole", len(o.Error))
	}
	if recs[4].Body != `{"multi":"line"}` {
		t.Errorf("a body with insignificant newlines reads back as %q, want it compacted onto the one line", recs[4].Body)
	}
	if h := w.Health(); h.Omitted != 4 {
		t.Fatalf("Health.Omitted = %d, want 4", h.Omitted)
	}
}

// TestANilWriterIsOff: a session without a journal calls the same methods
// with no branch.
func TestANilWriterIsOff(t *testing.T) {
	var w *Writer
	w.Append(event(1))
	w.Note(PromptNote{Attempt: "a1", Text: "x"})
	w.RequestSync()
	w.RequestFlush()
	if h := w.Health(); h.State != StateOff {
		t.Fatalf("nil Health = %+v, want off", h)
	}
	if seq, n := w.Flushed(); seq != 0 || n != 0 {
		t.Fatalf("nil Flushed = %d, %d", seq, n)
	}
	if w.Path() != "" {
		t.Fatal("nil Path is not empty")
	}
	if err := w.ReadRange(1, 1, func(Record) error { return nil }); !errors.Is(err, ErrNoJournal) {
		t.Fatalf("nil ReadRange = %v", err)
	}
	if err := w.WaitFlushed(context.Background(), 1); !errors.Is(err, ErrNoJournal) {
		t.Fatalf("nil WaitFlushed = %v", err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("nil Close = %v", err)
	}
}

// TestWaitFlushedEndsWithItsContext: a stalled writer does not hold a
// waiter past its ctx.
func TestWaitFlushedEndsWithItsContext(t *testing.T) {
	hooks := newFileHooks()
	stall := newGate(t)
	hooks.onWrite = func(call int, p []byte, f *os.File) (int, error) {
		stall.block()
		return f.Write(p)
	}
	opts := testOptions(t)
	opts.openFile = hooks.open
	w := newWriter(t, opts)
	w.Append(event(1))
	w.RequestFlush()
	stall.waitEntered(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.WaitFlushed(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitFlushed = %v, want context.Canceled", err)
	}
	if err := w.ReadRange(1, 1, func(Record) error { return nil }); !errors.Is(err, ErrBehind) {
		t.Fatalf("ReadRange during the stall = %v, want ErrBehind", err)
	}
	stall.open()
	waitFlushed(t, w, 1)
}

// TestHealthGapsAreBoundedAndOnlyEverWiden: past 64 ranges the closest
// neighbors merge, every seq ever missing is still reported missing, and an
// open-ended failure range absorbs what it touches.
func TestHealthGapsAreBoundedAndOnlyEverWiden(t *testing.T) {
	var h Health
	var all []SeqRange
	for i := range uint64(100) {
		r := SeqRange{From: i*10 + 1, To: i*10 + 1 + i%3}
		all = append(all, r)
		h.addGap(r)
	}
	if len(h.Gaps) != maxHealthGaps || !h.Truncated {
		t.Fatalf("%d gaps, truncated %v; want %d and true", len(h.Gaps), h.Truncated, maxHealthGaps)
	}
	for _, r := range all {
		if !h.Overlaps(r.From, r.From) || !h.Overlaps(r.To, r.To) {
			t.Fatalf("%v is no longer reported missing", r)
		}
	}
	if !slices.IsSortedFunc(h.Gaps, func(a, b SeqRange) int { return cmp.Compare(a.From, b.From) }) {
		t.Fatalf("gaps out of order: %v", h.Gaps)
	}
	prev := h.Gaps[len(h.Gaps)-1]
	h.addOpenGap(prev.To + 1) // adjacent to the last range: absorbed
	if n, last := len(h.Gaps), h.Gaps[len(h.Gaps)-1]; n != maxHealthGaps || last != (SeqRange{prev.From, MaxSeq}) {
		t.Fatalf("after an adjacent open gap: %d gaps ending %v, want %d ending {%d max}", n, last, maxHealthGaps, prev.From)
	}
	var fresh Health
	fresh.addGap(SeqRange{5, 6})
	fresh.addOpenGap(20)
	if !slices.Equal(fresh.Gaps, []SeqRange{{5, 6}, {20, MaxSeq}}) {
		t.Fatalf("open gap after a distant range: %v", fresh.Gaps)
	}
}

// TestConcurrentUseIsRaceFree drives every entry point at once under -race:
// ordered appends from several goroutines, notes, requests, health reads,
// live reads and flush waits, then Close. With a queue large enough for no
// drops, the file holds every event, contiguous.
func TestConcurrentUseIsRaceFree(t *testing.T) {
	opts := testOptions(t)
	opts.Now = time.Now
	opts.flushLines = 16
	w := newWriter(t, opts)
	const perWriter, writers = 200, 4
	var seqMu sync.Mutex
	var next uint64
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for range perWriter {
				seqMu.Lock() // the event log's boundary: seqs reach Append in order
				next++
				w.Append(event(next))
				seqMu.Unlock()
			}
		})
	}
	wg.Go(func() {
		for i := range 200 {
			w.Note(DiagNote{Kind: DiagAgentStderr, Fields: map[string]any{"i": i}})
			if i%20 == 0 {
				w.Note(PromptEndNote{Attempt: fmt.Sprint(i)})
			}
		}
	})
	stopCtx, stop := context.WithCancel(context.Background())
	var readers sync.WaitGroup
	readers.Go(func() {
		for stopCtx.Err() == nil {
			_ = w.Health()
			seq, _ := w.Flushed()
			if seq > 0 {
				recs, err := collect(func(fn func(Record) error) error { return w.ReadRange(1, seq, fn) })
				if err != nil || !slices.Equal(seqs(recs), seqRange(1, seq)) {
					t.Errorf("live ReadRange(1, %d) = %d records, %v", seq, len(recs), err)
					return
				}
			}
			_ = w.WaitFlushed(stopCtx, seq+1)
		}
	})
	wg.Wait()
	waitFlushed(t, w, perWriter*writers)
	stop()
	readers.Wait()
	closeWriter(t, w)
	recs, err := collect(func(fn func(Record) error) error { return ReadFile(w.Path(), 1, MaxSeq, fn) })
	if err != nil || len(recs) != perWriter*writers {
		t.Fatalf("ReadFile = %d records, %v; want %d", len(recs), err, perWriter*writers)
	}
	if h := w.Health(); h.State != StateOK || h.DroppedEvents != 0 || h.DroppedNotes != 0 {
		t.Fatalf("Health = %+v, want nothing dropped", h)
	}
}

// stallFirstWrite makes the hooks' first Write block until the returned
// gate opens, so what Append admits while it is stuck is decided by the
// bounds alone. Called after newWriter, so a failing test's cleanup opens
// the gate before it closes the writer.
func stallFirstWrite(t *testing.T, hooks *fileHooks) *gate {
	stall := newGate(t)
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.onWrite = func(call int, p []byte, f *os.File) (int, error) {
		if call == 1 {
			stall.block()
		}
		return f.Write(p)
	}
	return stall
}

// TestAnOmittedRecordsTextIsWeighedByTheQueue (finding 2): an omitted
// record's error and reason count against QueueBytes as a body does, so a
// record carrying a 4 KiB error is not admitted into a 1 KiB queue that a
// stalled write already holds an event in: it is dropped into a gap, and
// the queue never holds more than its budget.
func TestAnOmittedRecordsTextIsWeighedByTheQueue(t *testing.T) {
	hooks := newFileHooks()
	opts := testOptions(t)
	opts.openFile = hooks.open
	opts.QueueBytes = 1024
	w := newWriter(t, opts)
	stall := stallFirstWrite(t, hooks)

	w.Append(event(1))
	w.RequestFlush()
	stall.waitEntered(t) // event 1 is in flight and still weighs on the queue
	w.Append(Record{Seq: 2, At: testBase, EventType: "tool",
		Omitted: &Omitted{Reason: OmittedEncodeError, Error: strings.Repeat("e", 4<<10)}})
	w.mu.Lock()
	queued := w.queuedBytes
	w.mu.Unlock()
	if queued > opts.QueueBytes {
		t.Fatalf("the queue holds %d bytes, over its %d budget", queued, opts.QueueBytes)
	}
	if h := w.Health(); h.DroppedEvents != 1 || !slices.Equal(h.Gaps, []SeqRange{{2, 2}}) {
		t.Fatalf("Health = %+v, want record 2 dropped into a gap", h)
	}
	stall.open()
	closeWriter(t, w)
	if got := shape(parsedLines(t, w.Path())); got != "header 1 gap{2..2 e1 n0}" {
		t.Fatalf("file layout %s, want record 2 recorded as a gap", got)
	}
}

// TestAClosingNoteDroppedIntoAGapStillCreatesNothing (finding 3): with a
// queue too small to hold it, the closing note is dropped into a gap marker
// rather than queued; a gap that stands only for closing notes is discarded
// as the note itself would have been, so still nothing is created, nothing
// is counted lost, and no notice is printed.
func TestAClosingNoteDroppedIntoAGapStillCreatesNothing(t *testing.T) {
	diag := &diagSink{}
	opts := testOptions(t)
	opts.QueueBytes = 1 // nothing fits: every entry is dropped
	opts.Diag = diag
	w := newWriter(t, opts)
	w.Note(DiagNote{Kind: DiagClosing, Fields: map[string]any{"droppedAtClose": 0}})
	closeWriter(t, w)
	if _, err := os.Stat(opts.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(%s) = %v, want nothing created", opts.Dir, err)
	}
	if h := w.Health(); h.State != StateOK || h.DroppedNotes != 0 || len(h.Gaps) != 0 {
		t.Fatalf("Health = %+v; a discarded closing note is not a loss", h)
	}
	if n := diag.lines(); len(n) != 0 {
		t.Fatalf("notices %q for a journal that wrote nothing", n)
	}
}

// TestAClosingOnlyGapIsWrittenOnceTheFileExists (finding 3): the discard is
// only for a file that does not exist. Once one does, a closing note
// dropped into a gap is a loss like any other, and its gap line is written.
func TestAClosingOnlyGapIsWrittenOnceTheFileExists(t *testing.T) {
	hooks := newFileHooks()
	opts := testOptions(t)
	opts.openFile = hooks.open
	opts.QueueEntries = 2 // event 1 in flight fills it
	w := newWriter(t, opts)
	stall := stallFirstWrite(t, hooks)
	w.Append(event(1))
	w.RequestFlush()
	stall.waitEntered(t)
	w.Note(DiagNote{Kind: DiagClosing})
	stall.open()
	closeWriter(t, w)
	if got := shape(parsedLines(t, w.Path())); got != "header 1 gap{<nil>..<nil> e0 n1}" {
		t.Fatalf("file layout %s, want the closing note counted in a gap line", got)
	}
	if h := w.Health(); h.State != StateOK || h.DroppedNotes != 1 {
		t.Fatalf("Health = %+v, want the closing note counted lost", h)
	}
}

// TestNoLineOutgrowsTheReaderWhateverItIsHanded (finding 5): an event type
// or a header string that escaping would take past MaxLineBytes never makes
// a line the reader refuses. An event type over 256 bytes makes its record
// an encode_error omitted record under a placeholder type, keeping its
// seq; a header string is cut at 4 KiB. Every line fits, the journal stays
// healthy, and the file reads back whole.
func TestNoLineOutgrowsTheReaderWhateverItIsHanded(t *testing.T) {
	opts := testOptions(t)
	opts.Mode = strings.Repeat("m", MaxLineBytes)
	opts.Provider = strings.Repeat("é", 3<<10) // cut between runes
	w := newWriter(t, opts)
	w.Append(event(1))
	w.Append(Record{Seq: 2, At: testBase, EventType: strings.Repeat("\x01", 3<<20), Body: `{"type":"text"}`})
	w.Append(Record{Seq: 3, At: testBase, EventType: strings.Repeat("t", 256), Body: `{"type":"text"}`})
	closeWriter(t, w)

	for i, line := range rawLines(t, w.Path()) {
		if len(line) > MaxLineBytes {
			t.Fatalf("line %d is %d bytes, over MaxLineBytes", i+1, len(line))
		}
	}
	header := parsedLines(t, w.Path())[0]
	mode, _ := header["mode"].(string)
	provider, _ := header["provider"].(string)
	if len(mode) != 4<<10 || !utf8.ValidString(provider) || len(provider) > 4<<10 || len(provider) < 4<<10-1 {
		t.Fatalf("header mode is %d bytes and provider %d (valid %v), want each cut to 4 KiB between runes",
			len(mode), len(provider), utf8.ValidString(provider))
	}
	recs, err := collect(func(fn func(Record) error) error { return ReadFile(w.Path(), 1, MaxSeq, fn) })
	if err != nil || !slices.Equal(seqs(recs), []uint64{1, 2, 3}) {
		t.Fatalf("ReadFile = %v, %v; want 1..3", seqs(recs), err)
	}
	if r := recs[1]; r.EventType != "<event type too long>" || r.Body != "" || r.Omitted == nil ||
		r.Omitted.Reason != OmittedEncodeError || !strings.Contains(r.Omitted.Error, "event type") {
		t.Fatalf("record 2 = %+v, want an encode_error omitted record under the placeholder type", r)
	}
	if r := recs[2]; r.EventType != strings.Repeat("t", 256) || r.Omitted != nil {
		t.Fatalf("record 3 = %+v, want a 256-byte event type kept with its body", r)
	}
	if h := w.Health(); h.State != StateOK || h.Omitted != 1 {
		t.Fatalf("Health = %+v, want ok with one omitted record", h)
	}
}

// TestAnEventLineOverTheLineCapIsWrittenOmitted (finding 5): the check
// behind Append's caps. With the writer's line cap lowered below a body
// the record cap allows, the record is journaled as an oversized omitted
// record, keeping its seq, rather than as a line the reader would refuse.
func TestAnEventLineOverTheLineCapIsWrittenOmitted(t *testing.T) {
	const lineCap = 8 << 10
	opts := testOptions(t)
	opts.maxLineBytes = lineCap
	w := newWriter(t, opts)
	body := `{"text":"` + strings.Repeat("p", 2*lineCap) + `"}`
	w.Append(event(1))
	w.Append(Record{Seq: 2, At: testBase, EventType: "plan", Body: body})
	w.Append(event(3))
	closeWriter(t, w)
	for i, line := range rawLines(t, w.Path()) {
		if len(line) > lineCap {
			t.Fatalf("line %d is %d bytes, over the %d line cap", i+1, len(line), lineCap)
		}
	}
	recs, err := collect(func(fn func(Record) error) error { return ReadFile(w.Path(), 1, MaxSeq, fn) })
	if err != nil || !slices.Equal(seqs(recs), []uint64{1, 2, 3}) {
		t.Fatalf("ReadFile = %v, %v; want 1..3", seqs(recs), err)
	}
	if o := recs[1].Omitted; o == nil || *o != (Omitted{Reason: OmittedOversized, Bytes: len(body)}) || recs[1].EventType != "plan" {
		t.Fatalf("record 2 = %+v, want an oversized omitted record of %d bytes", recs[1], len(body))
	}
}

// TestATimeNoReaderCanParseIsLeftOut (finding 6): an event whose own time is
// outside years 0000–9999 has no RFC 3339 form time.Time parses back, so its
// line leaves "at" out and the record reads back between its neighbors with
// the zero time, instead of the reader losing the rest of the file from that
// line on. A line's own ts is left out the same way; the years at the edges
// are written as they are.
func TestATimeNoReaderCanParseIsLeftOut(t *testing.T) {
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	edge := time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.UTC)
	clock := &testClock{}
	var farNow atomic.Bool
	opts := testOptions(t)
	opts.Now = func() time.Time {
		if farNow.Load() {
			return far
		}
		return clock.now()
	}
	w := newWriter(t, opts)
	w.Append(event(1))
	w.Append(Record{Seq: 2, At: far, EventType: "text", Body: `{"n":2}`})
	w.Append(Record{Seq: 3, At: time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC), EventType: "text", Body: `{"n":3}`})
	w.Append(Record{Seq: 4, At: edge, EventType: "text", Body: `{"n":4}`})
	farNow.Store(true)
	w.Append(Record{Seq: 5, At: time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), EventType: "text", Body: `{"n":5}`})
	farNow.Store(false)
	w.Append(event(6))
	closeWriter(t, w)

	recs, err := collect(func(fn func(Record) error) error { return ReadFile(w.Path(), 1, MaxSeq, fn) })
	if err != nil || !slices.Equal(seqs(recs), seqRange(1, 6)) {
		t.Fatalf("ReadFile = %v, %v; want 1..6", seqs(recs), err)
	}
	for i, want := range []time.Time{testBase, {}, {}, edge, time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), testBase} {
		if !recs[i].At.Equal(want) {
			t.Errorf("record %d at %v, want %v", i+1, recs[i].At, want)
		}
	}
	raw := rawLines(t, w.Path())
	for i, l := range parsedLines(t, w.Path()) {
		_, hasAt := l["at"]
		_, hasTS := l["ts"]
		if wantAt := i != 2 && i != 3; l["type"] == typeEvent && hasAt != wantAt {
			t.Errorf("line %d has at: %v, want %v: %s", i+1, hasAt, wantAt, raw[i])
		}
		if wantTS := i != 5; hasTS != wantTS {
			t.Errorf("line %d has ts: %v, want %v: %s", i+1, hasTS, wantTS, raw[i])
		}
	}
}
