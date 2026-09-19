package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// MaxLineBytes is the longest line the reader accepts, newline excluded. A
// longer line is an error, never an unbounded allocation. The writer keeps
// every line well under it: bodies and notes are capped at MaxRecordBytes,
// which New holds at least 64 KiB below this.
const MaxLineBytes = 16 << 20

// readBufferBytes is the reader's buffered chunk; a longer line is
// assembled from chunks, up to MaxLineBytes.
const readBufferBytes = 64 << 10

var (
	// ErrBehind is ReadRange's error, before it delivers anything, when the
	// live file's complete-record boundary does not yet reach the range's
	// end. The caller may WaitFlushed and try again.
	ErrBehind = errors.New("journal: the file does not reach the requested seq yet")
	// ErrGap: a seq inside the requested range is not in the file, because
	// a gap line records it dropped, or the file skips it, or (a dead file)
	// ends before it. The range can never be served from this file.
	ErrGap = errors.New("journal: a seq in the requested range is not in the file")
	// ErrLineTooLong: a line is longer than MaxLineBytes.
	ErrLineTooLong = errors.New("journal: a line is longer than the reader's limit")
	// ErrMalformed: the file is not a journal this reader understands: no
	// header, another format, a complete line that is not JSON, or seqs out
	// of order.
	ErrMalformed = errors.New("journal: malformed journal")
)

// ReadFile reads the event records with from <= seq <= to from a finished
// journal, in order, calling fn for each; to may be MaxSeq for "through the
// last record". Every seq in the range must be there: one that is missing is
// ErrGap, not a skip, and nothing after it is delivered. An omitted record
// is delivered as a record. A torn last line (a crash mid-write) is
// ignored; a torn line anywhere else is ErrMalformed. The file is streamed,
// never loaded whole. An error from fn stops the read and is returned.
func ReadFile(path string, from, to uint64, fn func(Record) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return readRange(f, false, from, to, fn)
}

// ReadRange is ReadFile for the journal being written: it reads the
// writer's own file through a descriptor of its own, never past the
// complete-record boundary as it stood when the call began, so it never
// sees a partial line. When that boundary does not reach to, it returns
// ErrBehind before delivering anything.
func (w *Writer) ReadRange(from, to uint64, fn func(Record) error) error {
	if w == nil {
		return ErrNoJournal
	}
	if err := checkRange(from, to); err != nil {
		return err
	}
	seq, n := w.Flushed()
	if to != MaxSeq && seq < to {
		return fmt.Errorf("%w: the file reaches seq %d, the range ends at %d", ErrBehind, seq, to)
	}
	if n == 0 {
		// Nothing written; to is MaxSeq, so there is nothing to deliver.
		// The path is not opened: a file already there (a failed exclusive
		// create) is not this journal's.
		return nil
	}
	f, err := os.Open(w.path)
	if err != nil {
		return err
	}
	defer f.Close()
	return readRange(io.LimitReader(f, n), true, from, to, fn)
}

func checkRange(from, to uint64) error {
	if from == 0 || from > to {
		return fmt.Errorf("journal: %d..%d is not a range of seqs (seqs start at 1)", from, to)
	}
	return nil
}

// fileLine is every line type's fields a reader needs, in one struct so
// each line is decoded once. Notes' own fields are not decoded. Each key
// here means the same thing, with the same JSON shape, on every line type
// that carries it; a later line type that reused one with another shape
// would make this reader refuse the file, so it needs a new FormatVersion.
type fileLine struct {
	Type      string          `json:"type"`
	Format    int             `json:"format"`
	Seq       uint64          `json:"seq"`
	At        time.Time       `json:"at"`
	EventType string          `json:"eventType"`
	Event     json.RawMessage `json:"event"`
	Omitted   *omittedJSON    `json:"omitted"`
	FromSeq   uint64          `json:"fromSeq"`
	ToSeq     uint64          `json:"toSeq"`
}

// readRange is ReadFile and ReadRange's one implementation. live says the
// input ends at a complete-record boundary, so an unterminated last line is
// corruption there rather than a tolerated crash tear.
func readRange(r io.Reader, live bool, from, to uint64, fn func(Record) error) error {
	if err := checkRange(from, to); err != nil {
		return err
	}
	br := bufio.NewReaderSize(r, readBufferBytes)
	var buf []byte
	header := false
	next := from    // the seq to deliver next
	var last uint64 // the highest seq any line so far accounts for
	for n := 1; ; n++ {
		line, complete, err := readLine(br, buf)
		if err != nil && err != io.EOF {
			return err
		}
		if !complete {
			if live && len(line) > 0 {
				return fmt.Errorf("%w: the live file ends in a partial line", ErrMalformed)
			}
			break
		}
		buf = line[:0]
		var l fileLine
		if err := json.Unmarshal(line, &l); err != nil {
			return fmt.Errorf("%w: line %d: %v", ErrMalformed, n, err)
		}
		if !header {
			if l.Type != typeHeader {
				return fmt.Errorf("%w: line 1 is %q, not a header", ErrMalformed, l.Type)
			}
			if l.Format != FormatVersion {
				return fmt.Errorf("%w: format %d, this reader knows %d", ErrMalformed, l.Format, FormatVersion)
			}
			header = true
			continue
		}
		switch l.Type {
		case typeEvent:
			if l.Seq <= last {
				return fmt.Errorf("%w: line %d: seq %d after %d", ErrMalformed, n, l.Seq, last)
			}
			last = l.Seq
			if l.Seq < next {
				continue
			}
			if l.Seq > next {
				return fmt.Errorf("%w: seqs %d..%d are missing before line %d", ErrGap, next, min(l.Seq-1, to), n)
			}
			rec, err := l.record()
			if err != nil {
				return fmt.Errorf("%w: line %d: %v", ErrMalformed, n, err)
			}
			if err := fn(rec); err != nil {
				return err
			}
			if l.Seq == to {
				return nil
			}
			next++
		case typeGap:
			if l.ToSeq == 0 {
				continue // a note-only gap: no seq is missing
			}
			if l.FromSeq <= to && l.ToSeq >= next {
				return fmt.Errorf("%w: seqs %d..%d were dropped (line %d)", ErrGap, l.FromSeq, l.ToSeq, n)
			}
			last = max(last, l.ToSeq)
		}
		// Notes, and line types a later format adds, are not records.
	}
	if !header {
		return fmt.Errorf("%w: no header", ErrMalformed)
	}
	if to == MaxSeq {
		return nil
	}
	return fmt.Errorf("%w: seqs %d..%d are missing; the file ends at seq %d", ErrGap, next, to, last)
}

// record is an event line as a Record.
func (l fileLine) record() (Record, error) {
	rec := Record{Seq: l.Seq, At: l.At, EventType: l.EventType}
	switch {
	case l.Omitted != nil:
		rec.Omitted = &Omitted{Reason: l.Omitted.Reason, Bytes: l.Omitted.Bytes, Error: l.Omitted.Error}
	case len(l.Event) > 0:
		rec.Body = string(l.Event)
	default:
		return Record{}, errors.New("an event line with neither event nor omitted")
	}
	return rec, nil
}

// readLine reads the next line into buf's storage and returns it without
// its newline, and whether it had one. At the end of the input it returns
// io.EOF with whatever unterminated bytes were left. A line longer than
// MaxLineBytes is ErrLineTooLong, found before it is fully buffered.
func readLine(br *bufio.Reader, buf []byte) ([]byte, bool, error) {
	buf = buf[:0]
	for {
		frag, err := br.ReadSlice('\n')
		if len(buf)+len(frag) > MaxLineBytes+1 {
			return nil, false, ErrLineTooLong
		}
		buf = append(buf, frag...)
		switch err {
		case nil:
			return buf[:len(buf)-1], true, nil
		case bufio.ErrBufferFull:
			continue
		default:
			return buf, false, err
		}
	}
}
