package responsesapi

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// maxEventBytes bounds one event's data. An output item's final event holds
// the item whole — every word of a long answer, a reasoning item's encrypted
// content (about 2 KB in the spike) — so it is generous; past it the stream
// is refused rather than buffered without end. A variable only so a test can
// lower it.
var maxEventBytes = 32 << 20

// errEventTooLarge is the stream's failure when one event's data passes
// maxEventBytes.
var errEventTooLarge = errors.New("responsesapi: a stream event is larger than the reader accepts (32 MiB)")

// sseReader reads Server-Sent Events (the WHATWG event-stream format) from a
// response body, whatever the response's Content-Type says: the route sends
// its stream with no Content-Type at all (spike), and a client that insists
// on text/event-stream fails it. Lines end in LF or CRLF; a line starting
// with ":" is a comment; "data" lines accumulate, joined by newlines, until a
// blank line dispatches them with the latest "event" name; other fields are
// ignored.
type sseReader struct {
	r *bufio.Reader
}

// sseEvent is one dispatched event: its name ("" when it gave none) and its
// data.
type sseEvent struct {
	name string
	data []byte
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// next is the next event with data, or an error: io.EOF when the stream
// ended cleanly between events. A last event the stream did not end with a
// blank line is dispatched all the same — a server that closes right after
// response.completed's data line has still sent it whole, and the event's
// own JSON decoding catches one cut short.
func (s *sseReader) next() (sseEvent, error) {
	var ev sseEvent
	var data []byte
	hasData := false
	for {
		line, err := s.line()
		if err != nil {
			if errors.Is(err, io.EOF) && hasData {
				ev.data = data
				return ev, nil
			}
			return sseEvent{}, err
		}
		if len(line) == 0 {
			if hasData {
				ev.data = data
				return ev, nil
			}
			ev.name = "" // an event with no data is not dispatched, name and all
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, found := bytes.Cut(line, []byte(":"))
		if found && len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch string(field) {
		case "data":
			join := 0
			if hasData {
				join = 1
			}
			if len(data)+join+len(value) > maxEventBytes {
				return sseEvent{}, errEventTooLarge
			}
			if hasData {
				data = append(data, '\n')
			}
			data = append(data, value...)
			hasData = true
		case "event":
			ev.name = string(value)
		}
	}
}

// maxFieldOverhead is what a line may hold beyond an event's data: its
// field name, the colon and space, and the line's end.
const maxFieldOverhead = 256

// line is the next line, without its LF or CRLF: io.EOF once nothing is
// left, and a last line with no newline is returned as a line. A line longer
// than the reader's buffer is put together from its pieces, up to
// maxEventBytes and a field's overhead.
func (s *sseReader) line() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := s.r.ReadSlice('\n')
		if len(buf)+len(chunk) > maxEventBytes+maxFieldOverhead {
			return nil, errEventTooLarge
		}
		buf = append(buf, chunk...) // a copy: ReadSlice's bytes are the reader's own
		switch {
		case err == nil:
			return trimEOL(buf[:len(buf)-1]), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(buf) == 0 {
				return nil, io.EOF
			}
			return trimEOL(buf), nil
		default:
			return nil, err
		}
	}
}

func trimEOL(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	if b == nil {
		return []byte{}
	}
	return b
}
