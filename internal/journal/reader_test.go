package journal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// headerFixture is the least of a header the reader needs.
const headerFixture = `{"ts":"2026-09-19T10:00:00Z","type":"header","format":1}`

// eventFixture is a well-formed event line for seq.
func eventFixture(seq uint64) string {
	return fmt.Sprintf(`{"ts":"2026-09-19T10:00:00Z","type":"event","seq":%d,"at":"2026-09-19T10:00:00Z","eventType":"text","event":{"n":%d}}`, seq, seq)
}

// journalFixture writes lines, each newline-terminated, to a fresh file.
func journalFixture(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(path string, from, to uint64) ([]Record, error) {
	return collect(func(fn func(Record) error) error { return ReadFile(path, from, to, fn) })
}

// TestReadDeliversExactlyTheRangeInOrder: [from, to] inclusive, MaxSeq for
// "through the end", nothing outside it, and ranges that are not ranges
// refused.
func TestReadDeliversExactlyTheRangeInOrder(t *testing.T) {
	w := newWriter(t, testOptions(t))
	appendEvents(w, 1, 10)
	waitFlushed(t, w, 10)
	live := func(from, to uint64) ([]Record, error) {
		return collect(func(fn func(Record) error) error { return w.ReadRange(from, to, fn) })
	}
	for _, read := range []struct {
		name string
		fn   func(from, to uint64) ([]Record, error)
	}{{"live", live}, {"dead", func(from, to uint64) ([]Record, error) { return readFile(w.Path(), from, to) }}} {
		if read.name == "dead" {
			closeWriter(t, w)
		}
		for _, tc := range []struct {
			from, to uint64
			want     []uint64
		}{
			{3, 7, seqRange(3, 7)},
			{1, MaxSeq, seqRange(1, 10)},
			{10, 10, []uint64{10}},
			{1, 1, []uint64{1}},
			{11, MaxSeq, nil},
		} {
			recs, err := read.fn(tc.from, tc.to)
			if err != nil || !slices.Equal(seqs(recs), tc.want) {
				t.Errorf("%s (%d, %d) = %v, %v; want %v", read.name, tc.from, tc.to, seqs(recs), err, tc.want)
			}
		}
		for _, bad := range [][2]uint64{{0, 5}, {5, 3}} {
			if _, err := read.fn(bad[0], bad[1]); err == nil {
				t.Errorf("%s (%d, %d) succeeded, want an error", read.name, bad[0], bad[1])
			}
		}
	}
	if _, err := readFile(w.Path(), 5, 11); !errors.Is(err, ErrGap) {
		t.Fatalf("a dead file read past its end = %v, want ErrGap", err)
	}
	if recs, _ := readFile(w.Path(), 3, 3); recs[0].Body != `{"type":"text","text":"e3"}` || recs[0].EventType != "text" || !recs[0].At.Equal(testBase) {
		t.Fatalf("record 3 = %+v", recs[0])
	}
}

// TestAMissingSeqIsAnErrorNotASkip: contiguity is checked, not assumed.
// Records before the hole are delivered; the hole stops the read.
func TestAMissingSeqIsAnErrorNotASkip(t *testing.T) {
	path := journalFixture(t, headerFixture, eventFixture(1), eventFixture(2), eventFixture(4))
	recs, err := readFile(path, 1, 4)
	if !errors.Is(err, ErrGap) || !slices.Equal(seqs(recs), []uint64{1, 2}) {
		t.Fatalf("ReadFile(1, 4) = %v, %v; want 1, 2 then ErrGap", seqs(recs), err)
	}
	if recs, err := readFile(path, 1, 2); err != nil || len(recs) != 2 {
		t.Fatalf("ReadFile(1, 2) = %v, %v", seqs(recs), err)
	}
	if recs, err := readFile(path, 4, 4); err != nil || !slices.Equal(seqs(recs), []uint64{4}) {
		t.Fatalf("ReadFile(4, 4) = %v, %v; the hole is outside this range", seqs(recs), err)
	}
	if _, err := readFile(path, 3, MaxSeq); !errors.Is(err, ErrGap) {
		t.Fatalf("ReadFile(3, max) = %v, want ErrGap: 3 is the range's first seq", err)
	}
}

// TestNotesGapsAndLaterLineTypesAreNotRecords: only event lines are
// delivered; a note-only gap costs no seq; a type a later format adds is
// skipped rather than refused.
func TestNotesGapsAndLaterLineTypesAreNotRecords(t *testing.T) {
	path := journalFixture(t, headerFixture,
		`{"ts":"2026-09-19T10:00:00Z","type":"session","providerSessionId":"s"}`,
		eventFixture(1),
		`{"ts":"2026-09-19T10:00:00Z","type":"prompt","attempt":"a","text":"hi","kind":"prompt"}`,
		`{"ts":"2026-09-19T10:00:00Z","type":"gap","droppedEvents":0,"droppedNotes":3,"error":"journal queue full"}`,
		eventFixture(2),
		`{"ts":"2026-09-19T10:00:00Z","type":"turn","id":"from a later format"}`,
		eventFixture(3),
	)
	if recs, err := readFile(path, 1, MaxSeq); err != nil || !slices.Equal(seqs(recs), []uint64{1, 2, 3}) {
		t.Fatalf("ReadFile = %v, %v", seqs(recs), err)
	}
}

// TestAGapLineBeforeTheRangeDoesNotStopIt: a range after a recorded gap is
// served; one that reaches into it is not.
func TestAGapLineBeforeTheRangeDoesNotStopIt(t *testing.T) {
	path := journalFixture(t, headerFixture, eventFixture(1),
		`{"ts":"2026-09-19T10:00:00Z","type":"gap","fromSeq":2,"toSeq":4,"droppedEvents":3,"droppedNotes":0,"error":"journal queue full"}`,
		eventFixture(5), eventFixture(6))
	if recs, err := readFile(path, 5, 6); err != nil || !slices.Equal(seqs(recs), []uint64{5, 6}) {
		t.Fatalf("ReadFile(5, 6) = %v, %v", seqs(recs), err)
	}
	if _, err := readFile(path, 4, 6); !errors.Is(err, ErrGap) {
		t.Fatalf("ReadFile(4, 6) = %v, want ErrGap", err)
	}
	if recs, err := readFile(path, 1, 1); err != nil || len(recs) != 1 {
		t.Fatalf("ReadFile(1, 1) = %v, %v; it ends before the gap", seqs(recs), err)
	}
}

// TestWhatIsNotAJournalIsMalformed: no header, another format, an empty
// file, seqs out of order, an event with no body, and a complete line that
// is not JSON (interior corruption, unlike a torn tail).
func TestWhatIsNotAJournalIsMalformed(t *testing.T) {
	for name, lines := range map[string][]string{
		"no header":      {eventFixture(1)},
		"a later format": {`{"ts":"x","type":"header","format":2}`, eventFixture(1)},
		// Read from 1, a descent would be a missing seq first; from 5, both
		// lines are skipped and only the ordering check can object.
		"out of order":      {headerFixture, eventFixture(2), eventFixture(1)},
		"a repeated seq":    {headerFixture, eventFixture(1), eventFixture(1)},
		"an empty event":    {headerFixture, `{"ts":"x","type":"event","seq":1,"at":"2026-09-19T10:00:00Z","eventType":"text"}`},
		"a garbage line":    {headerFixture, eventFixture(1), "not json", eventFixture(2)},
		"an event not JSON": {headerFixture, `{"ts":"x","type":"event","seq":1,`, eventFixture(2)},
	} {
		from := uint64(1)
		if name == "out of order" {
			from = 5
		}
		if _, err := readFile(journalFixture(t, lines...), from, MaxSeq); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: ReadFile = %v, want ErrMalformed", name, err)
		}
	}
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(empty, 1, MaxSeq); !errors.Is(err, ErrMalformed) {
		t.Errorf("an empty file: ReadFile = %v, want ErrMalformed", err)
	}
}

// TestATornTailIsTolerated (A13): a dead file's unterminated last line is a
// crash mid-write, ignored; it is never delivered, even when it happens to
// parse.
func TestATornTailIsTolerated(t *testing.T) {
	w := newWriter(t, testOptions(t))
	appendEvents(w, 1, 3)
	closeWriter(t, w)
	for _, tail := range []string{`{"ts":"2026-09-19T10:00:00Z","type":"event","se`, eventFixture(4)} {
		path := filepath.Join(t.TempDir(), "torn.jsonl")
		raw, err := os.ReadFile(w.Path())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, tail...), 0o600); err != nil {
			t.Fatal(err)
		}
		if recs, err := readFile(path, 1, MaxSeq); err != nil || !slices.Equal(seqs(recs), []uint64{1, 2, 3}) {
			t.Errorf("tail %.20q: ReadFile = %v, %v; want 1..3", tail, seqs(recs), err)
		}
		if _, err := readFile(path, 1, 4); !errors.Is(err, ErrGap) {
			t.Errorf("tail %.20q: ReadFile(1, 4) = %v, want ErrGap", tail, err)
		}
	}
}

// TestTheLiveReaderNeverSeesAPartialLine (A13): while a Write has put half
// its bytes in the file and not returned, the live reader stops at the last
// whole line the writer published, and a range needing the rest is
// ErrBehind, never a partial record. A reader treating the same file as dead
// would see only a torn tail.
func TestTheLiveReaderNeverSeesAPartialLine(t *testing.T) {
	hooks := newFileHooks()
	stall := newGate(t)
	hooks.onWrite = func(call int, p []byte, f *os.File) (int, error) {
		if call != 2 {
			return f.Write(p)
		}
		half := strings.IndexByte(string(p), '\n') / 2 // inside event 2's line
		n, err := f.Write(p[:half])
		if err != nil {
			return n, err
		}
		stall.block()
		m, err := f.Write(p[half:])
		return n + m, err
	}
	opts := testOptions(t)
	opts.openFile = hooks.open
	w := newWriter(t, opts)
	w.Append(event(1))
	waitFlushed(t, w, 1)
	w.Append(event(2))
	w.Append(event(3))
	w.RequestFlush()
	stall.waitEntered(t)

	_, boundary := w.Flushed()
	if st, err := os.Stat(w.Path()); err != nil || st.Size() <= boundary {
		t.Fatalf("file size %v (%v), want half a line past the boundary %d", st.Size(), err, boundary)
	}
	live := func(from, to uint64) ([]Record, error) {
		return collect(func(fn func(Record) error) error { return w.ReadRange(from, to, fn) })
	}
	if recs, err := live(1, MaxSeq); err != nil || !slices.Equal(seqs(recs), []uint64{1}) {
		t.Fatalf("live ReadRange(1, max) during the write = %v, %v; want 1 only", seqs(recs), err)
	}
	if _, err := live(1, 2); !errors.Is(err, ErrBehind) {
		t.Fatalf("live ReadRange(1, 2) during the write = %v, want ErrBehind", err)
	}
	if recs, err := readFile(w.Path(), 1, MaxSeq); err != nil || !slices.Equal(seqs(recs), []uint64{1}) {
		t.Fatalf("the same file read as dead = %v, %v; want 1 and the half line ignored", seqs(recs), err)
	}

	stall.open()
	waitFlushed(t, w, 3)
	if recs, err := live(1, 3); err != nil || !slices.Equal(seqs(recs), []uint64{1, 2, 3}) {
		t.Fatalf("live ReadRange(1, 3) after the write = %v, %v", seqs(recs), err)
	}
}

// TestAnErrorFromTheCallbackStopsTheRead: fn's error is returned as it is,
// and nothing after it is delivered.
func TestAnErrorFromTheCallbackStopsTheRead(t *testing.T) {
	path := journalFixture(t, headerFixture, eventFixture(1), eventFixture(2), eventFixture(3))
	stop := errors.New("stop")
	var got []uint64
	err := ReadFile(path, 1, MaxSeq, func(r Record) error {
		got = append(got, r.Seq)
		if r.Seq == 2 {
			return stop
		}
		return nil
	})
	if err != stop || !slices.Equal(got, []uint64{1, 2}) {
		t.Fatalf("ReadFile = %v after %v, want stop after 1, 2", err, got)
	}
}

// TestTheLineLimitIsExactWhereverTheLineIs (finding 4): a line of up
// to MaxLineBytes is read and one byte more is ErrLineTooLong, whether the
// line is interior, last with its newline, or last without one. The newline
// is the one byte a line may have past the limit, and only when it is the
// newline: an unterminated tail of MaxLineBytes+1 is too long, not a torn
// line to skip. A torn tail within the limit is still tolerated.
func TestTheLineLimitIsExactWhereverTheLineIs(t *testing.T) {
	// sized is event 2's line, valid JSON padded to exactly n bytes.
	const prefix = `{"ts":"2026-09-19T10:00:00Z","type":"event","seq":2,"at":"2026-09-19T10:00:00Z","eventType":"text","event":{"pad":"`
	const suffix = `"}}`
	pad := strings.Repeat("x", MaxLineBytes+1)
	sized := func(n int) io.Reader {
		return io.MultiReader(strings.NewReader(prefix), strings.NewReader(pad[:n-len(prefix)-len(suffix)]), strings.NewReader(suffix))
	}
	tails := map[string]string{
		"interior":     "\n" + eventFixture(3) + "\n",
		"last":         "\n",
		"unterminated": "",
	}
	for _, n := range []int{MaxLineBytes - 1, MaxLineBytes, MaxLineBytes + 1} {
		for where, tail := range tails {
			var want []uint64
			var wantErr error
			switch {
			case n > MaxLineBytes:
				want, wantErr = []uint64{1}, ErrLineTooLong
			case where == "interior":
				want = []uint64{1, 2, 3}
			case where == "last":
				want = []uint64{1, 2}
			default:
				want = []uint64{1} // a torn tail: ignored
			}
			r := io.MultiReader(strings.NewReader(headerFixture+"\n"+eventFixture(1)+"\n"), sized(n), strings.NewReader(tail))
			recs, err := collect(func(fn func(Record) error) error { return readRange(r, false, 1, MaxSeq, fn) })
			if !errors.Is(err, wantErr) || !slices.Equal(seqs(recs), want) {
				t.Errorf("a %s line of MaxLineBytes%+d: %v, %v; want %v, %v", where, n-MaxLineBytes, seqs(recs), err, want, wantErr)
			}
		}
	}

	// The reported case end to end: a dead file whose unterminated last line
	// is one byte over.
	path := filepath.Join(t.TempDir(), "long.jsonl")
	if err := os.WriteFile(path, []byte(headerFixture+"\n"+eventFixture(1)+"\n"+pad), 0o600); err != nil {
		t.Fatal(err)
	}
	if recs, err := readFile(path, 1, MaxSeq); !errors.Is(err, ErrLineTooLong) || len(recs) != 1 {
		t.Fatalf("ReadFile over an unterminated long tail = %v, %v; want record 1 then ErrLineTooLong", seqs(recs), err)
	}
	// And readLine alone finds it without the newline it never reaches.
	if _, _, err := readLine(bufio.NewReaderSize(strings.NewReader(pad), readBufferBytes), nil); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("readLine over %d unterminated bytes = %v, want ErrLineTooLong", len(pad), err)
	}
}

// TestAnUnreadableEventTimeCostsOnlyThatTime (finding 6): an event line
// whose "at" does not parse (a year past 9999, as a writer once formatted
// it) or is missing reads as the zero time; the record and every line after
// it are still delivered.
func TestAnUnreadableEventTimeCostsOnlyThatTime(t *testing.T) {
	path := journalFixture(t, headerFixture, eventFixture(1),
		`{"ts":"2026-09-19T10:00:00Z","type":"event","seq":2,"at":"10000-01-01T00:00:00Z","eventType":"text","event":{"n":2}}`,
		`{"type":"event","seq":3,"eventType":"text","event":{"n":3}}`,
		`{"ts":"2026-09-19T10:00:00Z","type":"event","seq":4,"at":17,"eventType":"text","event":{"n":4}}`,
		eventFixture(5))
	recs, err := readFile(path, 1, MaxSeq)
	if err != nil || !slices.Equal(seqs(recs), seqRange(1, 5)) {
		t.Fatalf("ReadFile = %v, %v; want 1..5", seqs(recs), err)
	}
	for _, r := range recs[1:4] {
		if !r.At.IsZero() || r.Body != fmt.Sprintf(`{"n":%d}`, r.Seq) {
			t.Errorf("record %d = %+v, want its body at the zero time", r.Seq, r)
		}
	}
	if recs[4].At.IsZero() {
		t.Errorf("record 5 lost its time")
	}
}
