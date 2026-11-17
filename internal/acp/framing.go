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
	"sync/atomic"
	"time"
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
	// fenced, once set (fence), refuses every frame but a session/cancel
	// notification: the overage guard's stop (plan 052 § D3), which must
	// still cancel what runs while nothing else reaches the agent. It is read
	// under mu, after the frame's turn to write has come, so a frame queued
	// behind a write in progress when the fence went up is refused too; and
	// fence takes mu once it is set, so a frame that read it unset finishes
	// its write before fence returns (review C5 item 5).
	fenced atomic.Bool
}

// ErrFenced is a frame refused because the connection is fenced: the session
// stopped for the overage guard (plan 052 § D3), and nothing but a
// session/cancel goes to the agent any more.
var ErrFenced = errors.New("acp: the session is stopping; nothing more is sent")

// fence makes the encoder refuse every frame but a session/cancel
// notification from here on, and waits, up to wait, for a write that read
// the flag unset to end: once it answers true, no frame but a cancel is ever
// written again. false is a write still in progress at the bound — held by
// an agent that is not reading — which the caller fails by closing the
// writer, so that nothing reaches the agent after it either.
func (e *Encoder) fence(wait time.Duration) bool {
	e.fenced.Store(true)
	if e.mu.TryLock() {
		e.mu.Unlock()
		return true
	}
	got := make(chan struct{})
	gaveUp := make(chan struct{})
	go func() {
		e.mu.Lock()
		select {
		case got <- struct{}{}:
			e.mu.Unlock()
		case <-gaveUp:
			e.mu.Unlock()
		}
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-got:
		return true
	case <-timer.C:
		close(gaveUp)
		return false
	}
}

// testFenceChecked is a test seam, nil in production: it runs in
// WriteMessage under the encoder's lock, once a frame has passed the fence
// check and before its bytes are written.
var testFenceChecked func(msg *Message)

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
	if e.fenced.Load() && (msg.Method != MethodSessionCancel || len(msg.ID) != 0) {
		return ErrFenced
	}
	if testFenceChecked != nil {
		testFenceChecked(msg)
	}
	if _, err := e.w.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}
