package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

const contentLengthPrefix = "content-length:"

var ErrContentLengthFraming = errors.New("acp: Content-Length (LSP) framing is not supported; expected newline-delimited JSON-RPC")

type Decoder struct {
	r *bufio.Reader
}

func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReader(r)}
}

func (d *Decoder) ReadMessage() (*Message, error) {
	for {
		line, err := d.readLine()
		if err != nil {
			return nil, err
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if looksLikeContentLength(trimmed) {
			return nil, ErrContentLengthFraming
		}
		if trimmed[0] != '{' {
			return nil, fmt.Errorf("acp ndjson: expected a JSON object, got %q", truncateForError(trimmed))
		}
		var msg Message
		if err := json.Unmarshal(trimmed, &msg); err != nil {
			return nil, fmt.Errorf("acp ndjson: %w", err)
		}
		return &msg, nil
	}
}

func (d *Decoder) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := d.r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(bytes.TrimSpace(buf)) == 0 {
				return nil, io.EOF
			}
			if looksLikeContentLength(bytes.TrimSpace(buf)) {
				return nil, ErrContentLengthFraming
			}
			return nil, fmt.Errorf("acp ndjson: unexpected EOF")
		}
		return nil, err
	}
	buf = bytes.TrimSuffix(buf, []byte("\n"))
	buf = bytes.TrimSuffix(buf, []byte("\r"))
	return buf, nil
}

func looksLikeContentLength(line []byte) bool {
	s := strings.ToLower(string(bytes.TrimSpace(line)))
	return strings.HasPrefix(s, contentLengthPrefix)
}

func truncateForError(b []byte) string {
	const max = 80
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

type Encoder struct {
	mu sync.Mutex
	w  io.Writer
}

func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

func (e *Encoder) WriteMessage(msg *Message) error {
	if msg.JSONRPC == "" {
		msg.JSONRPC = "2.0"
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.w.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}
