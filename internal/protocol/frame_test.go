package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/charliek/craze/internal/protocol"
)

// readAll reads every line of in with a limit, recording each line, or
// "!too long" for a line refused, until io.EOF.
func readAll(t *testing.T, r io.Reader, max int) []string {
	t.Helper()
	lr := protocol.NewLineReader(r, max)
	var got []string
	for range 1000 {
		line, err := lr.ReadLine()
		switch {
		case errors.Is(err, io.EOF):
			return got
		case errors.Is(err, protocol.ErrLineTooLong):
			got = append(got, "!too long")
		case err != nil:
			t.Fatalf("ReadLine: %v", err)
		default:
			if line == nil {
				t.Fatal("a line came back nil")
			}
			got = append(got, string(line))
		}
	}
	t.Fatal("the reader never reached EOF")
	return nil
}

// TestTheLineReaderFramesLines (§3.2): lines end at "\n", a "\r" before it is
// dropped, an empty line is a line, a last line with no "\n" is a line, and a
// line over the limit is discarded to its newline and reported — the reader
// goes on with the next line, so a connection survives it.
func TestTheLineReaderFramesLines(t *testing.T) {
	long := strings.Repeat("x", 200_000) // several of the reader's chunks
	for _, tc := range []struct {
		name string
		in   string
		max  int
		want []string
	}{
		{"lines", "{\"a\":1}\n{\"b\":2}\n", 0, []string{`{"a":1}`, `{"b":2}`}},
		{"a trailing CR", "{\"a\":1}\r\n{\"b\":2}\r\n", 0, []string{`{"a":1}`, `{"b":2}`}},
		{"an empty line and a lone CR", "\n\r\n{}\n", 0, []string{"", "", "{}"}},
		{"a last line with no newline", "{\"a\":1}\n{\"b\":2}", 0, []string{`{"a":1}`, `{"b":2}`}},
		{"a last line of CR alone", "{}\n\r", 0, []string{"{}", ""}},
		{"nothing", "", 0, nil},
		{"exactly the limit", "12345\n", 5, []string{"12345"}},
		{"exactly the limit with a CR", "12345\r\n", 5, []string{"12345"}},
		{"one over", "123456\n1\n", 5, []string{"!too long", "1"}},
		{"one over, a CR not counted", "123456\r\n1\n", 5, []string{"!too long", "1"}},
		{"a long line between two good ones", "a\n" + long + "\nb\n", 1000, []string{"a", "!too long", "b"}},
		{"a long last line with no newline", "a\n" + long, 1000, []string{"a", "!too long"}},
		{"a long line within a long limit", long + "\n", len(long), []string{long}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, r := range []io.Reader{strings.NewReader(tc.in), iotest.OneByteReader(strings.NewReader(tc.in))} {
				got := readAll(t, r, tc.max)
				if strings.Join(got, "|") != strings.Join(tc.want, "|") || len(got) != len(tc.want) {
					t.Fatalf("lines %q, want %q", abbreviate(got), abbreviate(tc.want))
				}
			}
		})
	}
}

func abbreviate(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if len(l) > 20 {
			l = l[:10] + "…" + l[len(l)-5:]
		}
		out[i] = l
	}
	return out
}

// TestTheLineReaderDefaultsToTheInboundLimit: no limit is InboundLineMax, and
// a line just over it is refused while the next is read.
func TestTheLineReaderDefaultsToTheInboundLimit(t *testing.T) {
	in := strings.Repeat("x", protocol.InboundLineMax) + "\n" + strings.Repeat("y", protocol.InboundLineMax+1) + "\nz\n"
	got := readAll(t, strings.NewReader(in), 0)
	if len(got) != 3 || len(got[0]) != protocol.InboundLineMax || got[1] != "!too long" || got[2] != "z" {
		t.Fatalf("got %d lines: %q", len(got), abbreviate(got))
	}
}

// TestTheLineReaderReturnsAReadError: an error other than EOF is returned as
// it is.
func TestTheLineReaderReturnsAReadError(t *testing.T) {
	boom := errors.New("boom")
	lr := protocol.NewLineReader(iotest.ErrReader(boom), 0)
	if _, err := lr.ReadLine(); !errors.Is(err, boom) {
		t.Fatalf("ReadLine: %v, want %v", err, boom)
	}
}

// TestMarshalLineCarriesARawBodyVerbatim (§3.3): an event notification
// carries Record.Body byte for byte — HTML characters unescaped, an escape
// the codec wrote kept as it wrote it, UTF-8 as it is — and the line ends in
// exactly one "\n". (encoding/json writes U+2028 and U+2029 escaped in every
// string, so a codec's body never holds them raw; internal/agent's twin test
// sends a real EncodeEvent body through.)
func TestMarshalLineCarriesARawBodyVerbatim(t *testing.T) {
	body := "{\"type\":\"text\",\"text\":\"a && b <c> \\u2028 \\u00e9 é\",\"at\":\"2026-09-25T10:30:45.123456789Z\"}"
	n := protocol.Notification{JSONRPC: protocol.JSONRPCVersion, Method: protocol.NotifyEvent,
		Params: json.RawMessage(`{"subscription":"s-1","seq":3,"event":` + body + `}`)}
	b, err := protocol.MarshalLine(n)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"jsonrpc":"2.0","method":"event","params":{"subscription":"s-1","seq":3,"event":` + body + "}}\n"
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	var w bytes.Buffer
	if err := protocol.WriteLine(&w, protocol.EventParams{Subscription: "s-1", Seq: 3, Event: json.RawMessage(body)}); err != nil {
		t.Fatal(err)
	}
	if want := `{"subscription":"s-1","seq":3,"event":` + body + "}\n"; w.String() != want {
		t.Fatalf("WriteLine wrote %s", w.String())
	}
	if bytes.Count(b, []byte("\n")) != 1 {
		t.Fatalf("a line with %d newlines", bytes.Count(b, []byte("\n")))
	}
}

// TestAResponseWithNoIDSaysNull: a response to a line whose id could not be
// read carries id null (§3.2).
func TestAResponseWithNoIDSaysNull(t *testing.T) {
	b, err := protocol.MarshalLine(protocol.Response{JSONRPC: protocol.JSONRPCVersion, Error: &protocol.Error{
		Code: protocol.RPCInvalidRequest, Message: "line too long",
		Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonLineTooLong}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"line too long","data":{"code":"bad_request","reason":"line_too_long"}}}` + "\n"
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

// TestARequestIDEchoesByteForByte: a request's id, number or string, is kept
// as its bytes, so the response carries exactly what the client sent.
func TestARequestIDEchoesByteForByte(t *testing.T) {
	for _, id := range []string{`7`, `"a-1"`, `1.50`, `"é"`, `12345678901234567890`} {
		var r protocol.Request
		if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":`+id+`,"method":"hello"}`), &r); err != nil {
			t.Fatal(err)
		}
		b, err := protocol.MarshalLine(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: r.ID, Result: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"jsonrpc":"2.0","id":` + id + `,"result":{}}` + "\n"; string(b) != want {
			t.Fatalf("id %s came back as %s", id, b)
		}
	}
}

// TestIsBatch: an array, whatever white space leads it, is a batch; an object
// or anything else is not.
func TestIsBatch(t *testing.T) {
	for line, want := range map[string]bool{
		`[{"jsonrpc":"2.0"}]`: true,
		" \t[]":               true,
		`{"jsonrpc":"2.0"}`:   false,
		"":                    false,
		"  ":                  false,
		`"["`:                 false,
	} {
		if got := protocol.IsBatch([]byte(line)); got != want {
			t.Errorf("IsBatch(%q) = %v, want %v", line, got, want)
		}
	}
}
