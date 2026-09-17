package host

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// fakeRequest is one recorded request line: the connection it arrived on
// (0-based, in accept order) and the raw bytes, including the trailing '\n'.
type fakeRequest struct {
	conn int
	line []byte
}

// fakeReplyFunc handles one decoded request line on conn. req is nil if the
// line did not decode as a JSON object. The function may write whatever it
// wants directly to conn (or nothing), and may block — selecting on stop so
// it unblocks at test cleanup — to hold a reply open. This is the one hook
// fakeUDS exposes, and it is enough to build every mode a test needs: an
// ordinary canned reply, "accept and never reply" (block on stop and write
// nothing), "close without newline" (write partial bytes, return: the caller
// closes conn), and a held reply released by an external gate (block on a
// channel the test closes). Returning ends the function's involvement with
// this one line; fakeUDS then reads the next line on the same connection, if
// the peer sends one.
type fakeReplyFunc func(conn net.Conn, connIdx int, req map[string]any, stop <-chan struct{})

// fakeUDS is a host-agnostic fake UDS listener, shared by herdr's tests and
// roost's, which need the same one-line-per-request framing; roost's reply
// function echoes ids and may prepend "event" frames.
type fakeUDS struct {
	t      testing.TB
	ln     net.Listener
	socket string
	reply  fakeReplyFunc

	stop     chan struct{}
	stopOnce sync.Once
	// wg tracks the accept loop and every handler goroutine, so close can
	// join them: nothing outlives the test that started them.
	wg sync.WaitGroup

	mu     sync.Mutex
	closed bool // set by close, under mu, before any conn already accepted is registered
	conns  []net.Conn
	reqs   []fakeRequest
	n      int
}

// shortSocketPath is a unix socket path, not yet bound, in a fresh temp dir
// removed at test cleanup. The path is asserted short: macOS's sun_path is 104
// bytes, and t.TempDir() paths routinely exceed that, so os.MkdirTemp("", ...)
// is used instead.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	socket := filepath.Join(dir, "s")
	if len(socket) >= 100 {
		t.Fatalf("fake socket path %d bytes, want < 100 (macOS sun_path is 104): %s", len(socket), socket)
	}
	return socket
}

// newFakeUDS starts a fake UDS listener on a shortSocketPath and stops it,
// closing every connection it ever accepted, at test cleanup.
func newFakeUDS(t *testing.T, reply fakeReplyFunc) *fakeUDS {
	t.Helper()
	socket := shortSocketPath(t)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	s := &fakeUDS{t: t, ln: ln, socket: socket, reply: reply, stop: make(chan struct{})}
	t.Cleanup(s.close)
	s.wg.Add(1)
	go s.acceptLoop()
	return s
}

func (s *fakeUDS) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			// close ran (and snapshotted s.conns) between this Accept
			// returning and this goroutine taking mu: register nothing and
			// close the conn now, so it cannot outlive the test blocked
			// forever in ReadBytes.
			s.mu.Unlock()
			conn.Close()
			continue
		}
		idx := s.n
		s.n++
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		s.wg.Add(1)
		go s.handle(idx, conn)
	}
}

func (s *fakeUDS) handle(idx int, conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			s.record(idx, line)
			var req map[string]any
			dec := json.NewDecoder(bytes.NewReader(line))
			dec.UseNumber()
			if dec.Decode(&req) != nil {
				req = nil
			}
			s.reply(conn, idx, req, s.stop)
		}
		if err != nil {
			return
		}
	}
}

func (s *fakeUDS) record(idx int, line []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, fakeRequest{conn: idx, line: append([]byte(nil), line...)})
}

// requests returns every recorded request line so far, in arrival order.
func (s *fakeUDS) requests() []fakeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeRequest(nil), s.reqs...)
}

// connections is how many connections have been accepted, lines or not.
func (s *fakeUDS) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// close stops accepting, closes every connection it knows about (and any
// accepted-but-not-yet-registered one, via the closed flag acceptLoop
// checks), then joins the accept loop and every handler goroutine: nothing
// it started is still running once close returns.
func (s *fakeUDS) close() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.ln.Close()
	s.mu.Lock()
	s.closed = true
	conns := append([]net.Conn(nil), s.conns...)
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	s.wg.Wait()
}

// writeLine JSON-encodes v and writes it to conn as one NDJSON line. Test
// reply functions use it for the ordinary canned-reply case.
func writeLine(t testing.TB, conn net.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fake reply: %v", err)
	}
	b = append(b, '\n')
	if _, err := conn.Write(b); err != nil {
		t.Logf("fake reply write: %v", err) // the client may have already closed; not a test failure
	}
}

// okReply is herdr's success shape for id.
func okReply(id string) map[string]any {
	return map[string]any{"id": id, "result": map[string]any{"type": "ok"}}
}

// errReply is herdr's error shape for id.
func errReply(id string, code int, msg string) map[string]any {
	return map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}}
}

// decodeJSONObject decodes one NDJSON line as the "semantic JSON" tests
// require: json.Decoder with UseNumber, into a plain map[string]any, so a
// whole-object comparison never trips over int-vs-float64 noise.
func decodeJSONObject(t testing.TB, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode JSON line %q: %v", line, err)
	}
	return m
}

// assertJSONEqual compares a decoded object against want (an ordinary Go
// value such as a literal map[string]any) for exact, whole-object equality:
// an unexpected extra field fails, same as a missing or differing one. want
// is normalised through the same json.Marshal + UseNumber decode path as
// got, so a plain Go int in a literal test table compares equal to the
// json.Number a real reply decodes to.
func assertJSONEqual(t testing.TB, got map[string]any, want any) {
	t.Helper()
	wb, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	wantMap := decodeJSONObject(t, wb)
	if !reflect.DeepEqual(got, wantMap) {
		gb, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("JSON mismatch\n got:  %s\nwant: %s", gb, wb)
	}
}
