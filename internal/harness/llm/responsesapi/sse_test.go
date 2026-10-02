package responsesapi

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, raw string) ([]sseEvent, error) {
	t.Helper()
	r := newSSEReader(strings.NewReader(raw))
	var out []sseEvent
	for {
		ev, err := r.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
		out = append(out, ev)
	}
}

// TestSSEReaderFraming: the event-stream format as a server may write it —
// CRLF or LF, comments, an event name, data over several lines, fields the
// reader does not use, an event with no data (not dispatched, its name with
// it) — and a last event the stream ended without a blank line after.
func TestSSEReaderFraming(t *testing.T) {
	raw := ": a comment\r\n" +
		"event: first\r\n" +
		"data: one\r\n" +
		"\r\n" +
		"id: 7\n" +
		"retry: 100\n" +
		"data:two\n" +
		"data:  three\n" +
		"\n" +
		"event: dropped\n" +
		"\n" +
		"data\n" +
		"\n" +
		"event: last\n" +
		"data: {\"type\":\"response.completed\"}"
	got, err := readAll(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []sseEvent{
		{name: "first", data: []byte("one")},
		{data: []byte("two\n three")}, // one space after the colon is the separator; the second is data
		{data: []byte("")},            // a bare "data" field is an empty data line, and dispatches
		{name: "last", data: []byte(`{"type":"response.completed"}`)},
	}
	if len(got) != len(want) {
		t.Fatalf("read %d events %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i].name != want[i].name || string(got[i].data) != string(want[i].data) {
			t.Errorf("event %d = {%q %q}, want {%q %q}", i, got[i].name, got[i].data, want[i].name, want[i].data)
		}
	}
}

// TestSSEReaderReadsALineLongerThanItsBuffer: a final item event is a line
// of any length — far more than the reader's 64 KiB buffer.
func TestSSEReaderReadsALineLongerThanItsBuffer(t *testing.T) {
	long := strings.Repeat("x", 200<<10)
	got, err := readAll(t, "data: "+long+"\n\ndata: next\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got[0].data) != long || string(got[1].data) != "next" {
		t.Fatalf("read %d events, the first %d bytes, want 2 with the long line whole", len(got), len(got[0].data))
	}
}

// TestSSEReaderRefusesAnOversizedEvent: an event past maxEventBytes, on one
// line or over several, fails the read rather than being buffered without
// end; one under it is read (the control).
func TestSSEReaderRefusesAnOversizedEvent(t *testing.T) {
	old := maxEventBytes
	maxEventBytes = 1 << 10
	defer func() { maxEventBytes = old }()

	for name, raw := range map[string]string{
		"one line":    "data: " + strings.Repeat("x", 2<<10) + "\n\n",
		"many lines":  strings.Repeat("data: "+strings.Repeat("x", 100)+"\n", 20) + "\n",
		"no newline":  "data: " + strings.Repeat("x", 2<<10),
		"one too big": "data: " + strings.Repeat("x", 1<<10+1) + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readAll(t, raw); !errors.Is(err, errEventTooLarge) {
				t.Fatalf("err = %v, want errEventTooLarge", err)
			}
		})
	}
	t.Run("at the cap", func(t *testing.T) {
		got, err := readAll(t, "data: "+strings.Repeat("x", 1<<10)+"\n\n")
		if err != nil || len(got) != 1 {
			t.Fatalf("read %d events, err %v; want the event", len(got), err)
		}
	})
}
