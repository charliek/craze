package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/charliek/craze/internal/harness/tool"
)

// received is one request the fake MCP server took.
type received struct {
	Method, Path, RawQuery string
	Header                 http.Header
	Body                   []byte
	// Call is the body decoded as the JSON-RPC request the caller sends.
	Call struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
}

// fakeMCP is an MCP endpoint on loopback that answers every call with
// reply, and keeps what it was sent.
type fakeMCP struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []received
}

// replyFunc answers one call: rec is the request as received, its id the
// one the answer must carry.
type replyFunc func(w http.ResponseWriter, r *http.Request, rec received)

func newFake(t *testing.T, reply replyFunc) *fakeMCP {
	t.Helper()
	f := &fakeMCP{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rec received
		rec.Method, rec.Path, rec.RawQuery, rec.Header = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone()
		rec.Body, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(rec.Body, &rec.Call)
		f.mu.Lock()
		f.got = append(f.got, rec)
		f.mu.Unlock()
		reply(w, r, rec)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMCP) calls() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]received(nil), f.got...)
}

// useEndpoint points the exa provider's own endpoint at url for the test,
// so a service built with no override sends its key there, as it would to
// Exa's.
func useEndpoint(t *testing.T, url string) {
	t.Helper()
	e := providers["exa"]
	old := e.endpoint
	e.endpoint = url
	providers["exa"] = e
	t.Cleanup(func() {
		e.endpoint = old
		providers["exa"] = e
	})
}

// keyed is a service of the exa provider at f's endpoint as its own (so key
// is sent), with timeout as the call's deadline (0 is CallTimeout).
func keyed(t *testing.T, f *fakeMCP, key string, timeout time.Duration) *Service {
	t.Helper()
	useEndpoint(t, f.srv.URL+"/mcp")
	s, err := New(Config{Key: key, timeout: timeout})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// overridden is a service at f's endpoint through the test override.
func overridden(t *testing.T, f *fakeMCP, key string) *Service {
	t.Helper()
	s, err := New(Config{Key: key, Endpoint: f.srv.URL + "/mcp"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// jsonString is s as a JSON string literal that keeps every byte at or
// above 0x80 as it is — an invalid one included, as a careless server would
// send it — and escapes only what JSON requires.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20:
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// resultJSON is a JSON-RPC answer to id whose result has one text block per
// text, and isError when isError.
func resultJSON(id int64, isError bool, texts ...string) string {
	var blocks []string
	for _, t := range texts {
		blocks = append(blocks, `{"type":"text","text":`+jsonString(t)+`}`)
	}
	return fmt.Sprintf(`{"result":{"content":[%s],"isError":%t},"jsonrpc":"2.0","id":%d}`, strings.Join(blocks, ","), isError, id)
}

// errorJSON is a JSON-RPC error answer to id with message.
func errorJSON(id int64, message string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":%s}}`, id, jsonString(message))
}

// sendJSON writes body as an application/json reply.
func sendJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

// sendSSE writes stream as a text/event-stream reply.
func sendSSE(w http.ResponseWriter, stream string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, stream)
}

// event is one SSE message event carrying data.
func event(data string) string { return "event: message\ndata: " + data + "\n\n" }

// idRE finds the id a captured reply answered.
var idRE = regexp.MustCompile(`"jsonrpc":"2\.0","id":\d+\}`)

// replay answers with a captured reply from testdata, its id made the
// call's, and its bytes otherwise as Exa sent them.
func replay(t *testing.T, name string) replyFunc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return func(w http.ResponseWriter, _ *http.Request, rec received) {
		sendSSE(w, idRE.ReplaceAllString(string(b), fmt.Sprintf(`"jsonrpc":"2.0","id":%d}`, rec.Call.ID)))
	}
}

// replyText answers every call with a result whose one text block is text,
// as a stream.
func replyText(text string) replyFunc {
	return func(w http.ResponseWriter, _ *http.Request, rec received) {
		sendSSE(w, event(resultJSON(rec.Call.ID, false, text)))
	}
}

// capturedText is the text of the one text block of a captured reply.
func capturedText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var env struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(data), &env); err != nil || len(env.Result.Content) != 1 {
			t.Fatalf("%s: not a one-block reply (%v)", name, err)
		}
		return env.Result.Content[0].Text
	}
	t.Fatalf("%s: no data line", name)
	return ""
}

// webErr is err as the *tool.WebError every service error must be.
func webErr(t *testing.T, err error) *tool.WebError {
	t.Helper()
	if err == nil {
		t.Fatal("no error")
	}
	we, ok := err.(*tool.WebError)
	if !ok {
		t.Fatalf("error %T %q is not a *tool.WebError", err, err)
	}
	return we
}

// wantErr says err is a *tool.WebError of class with text.
func wantErr(t *testing.T, err error, class tool.ErrorClass, text string) {
	t.Helper()
	we := webErr(t, err)
	if we.Class != class || we.Text != text {
		t.Fatalf("error = {%s %q}, want {%s %q}", we.Class, we.Text, class, text)
	}
}

// search runs a search on s with a background context.
func search(s *Service, q string) ([]tool.WebResult, error) {
	return s.Search(context.Background(), q)
}

// shown is v as a display shows it: format characters dropped, and invalid
// bytes and U+FFFD with them — what joins a split key back together.
func shown(v string) string {
	v = strings.ToValidUTF8(v, "")
	return strings.Map(func(r rune) rune {
		if r == unicode.ReplacementChar || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, v)
}

// isWebError says err is a *tool.WebError, through a wrap.
func isWebError(err error) bool {
	_, ok := errors.AsType[*tool.WebError](err)
	return ok
}
