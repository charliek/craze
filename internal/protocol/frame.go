package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// The framing (plan 027 §3.2, SD-03): one UTF-8 JSON object per line, ended by
// "\n", with a "\r" before the "\n" tolerated inbound. There are no batch
// arrays: a line that is one is answered -32600. Both ends of every transport
// — the Unix socket, craze bridge's stdio, a later WebSocket — frame the same
// way, and both internal/control and internal/remote read and write through
// these helpers.

// ErrLineTooLong is LineReader.ReadLine's answer for a line longer than the
// reader's limit. The line has already been discarded up to and including
// its newline, so the reader stands at the start of the next line and the
// connection goes on: the server answers -32600, reason line_too_long, with
// id null — the id was never read — and keeps reading (§3.2).
var ErrLineTooLong = errors.New("protocol: line too long")

// readChunk is the reader's buffer: how much of a line it holds between
// reads. A longer line is gathered from several reads; it bounds nothing.
const readChunk = 64 << 10

// LineReader reads a stream one line at a time, holding at most one line
// within its limit in memory. It is not safe for concurrent use: a
// connection has one reader (§3.7).
type LineReader struct {
	r   *bufio.Reader
	max int
}

// NewLineReader reads r a line at a time, refusing a line whose content is
// longer than max bytes (its "\n", and a "\r" before it, not counted); max <= 0
// is InboundLineMax, the host's inbound limit.
func NewLineReader(r io.Reader, max int) *LineReader {
	if max <= 0 {
		max = InboundLineMax
	}
	return &LineReader{r: bufio.NewReaderSize(r, readChunk), max: max}
}

// ReadLine returns the next line without its "\n" and without a "\r" just
// before it. The slice is the caller's own; nothing the reader does later
// changes it. An empty line is returned as one (empty, non-nil): what to do
// with it is the caller's to decide.
//
// A line longer than the limit is read to its end and dropped, and ReadLine
// returns ErrLineTooLong; the next call reads the line after it. A last line
// the stream ends without a "\n" is returned as a line all the same — a peer
// that half-closes after its final request still gets its reply (§3.7) — and
// the call after it returns io.EOF. A read error other than io.EOF is
// returned as it is, and what was read of the line is dropped.
func (l *LineReader) ReadLine() ([]byte, error) {
	var line []byte
	over := false
	for {
		chunk, err := l.r.ReadSlice('\n')
		if !over {
			// Content, "\r" and "\n": anything longer can only be too long, so
			// it is not kept while its end is read and dropped.
			if len(line)+len(chunk) > l.max+2 {
				over, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case err == nil:
			if over {
				return nil, ErrLineTooLong
			}
			line = trimLineEnd(line)
			if len(line) > l.max {
				return nil, ErrLineTooLong
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if over {
				return nil, ErrLineTooLong
			}
			if len(line) == 0 {
				return nil, io.EOF
			}
			line = trimLineEnd(line)
			if len(line) > l.max {
				return nil, ErrLineTooLong
			}
			return line, nil
		default:
			return nil, err
		}
	}
}

// trimLineEnd is line without a final "\n", and then without a final "\r",
// and never nil.
func trimLineEnd(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if line == nil {
		return []byte{}
	}
	return line
}

// MarshalLine is v as one line: compact JSON, HTML not escaped, ended by
// "\n". A json.RawMessage inside v that one of the codecs wrote — an event
// body, a snapshot, a leaf payload — is written as it is: it is already
// compact, with HTML escaping off nothing in it is rewritten, and the one
// thing encoding/json would still escape in a raw value, a bare U+2028 or
// U+2029, never occurs in one, because encoding/json escapes those in every
// string it writes. So an event notification carries Record.Body byte for
// byte (§3.3), and a server only ever embeds codec output, never hand-built
// JSON (plan 027 X6). The limit on an outbound line (OutboundLineMax) is the
// caller's to hold: a reply over it becomes failed, reason response_too_large
// (§3.2).
func MarshalLine(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil // Encode ends the value with "\n"
}

// WriteLine writes v to w as one line (MarshalLine), in one Write.
func WriteLine(w io.Writer, v any) error {
	b, err := MarshalLine(v)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// IsBatch reports whether line is a JSON array — a JSON-RPC batch, which
// protocol 1 does not accept (§3.2: -32600) — by its first byte that is not
// white space.
func IsBatch(line []byte) bool {
	line = bytes.TrimLeft(line, " \t\r\n")
	return len(line) > 0 && line[0] == '['
}
