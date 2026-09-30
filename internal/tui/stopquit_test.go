package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/host"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// The explicit quit's one deadline (plan 030 C5r, astra r8-c5 4): the stop's
// receipt, the wait for the session's end and the backend's close after them
// all end by quitStopWait from the quit, and a second quit closes at once. A
// host that has stopped answering is a proxy in front of a real one that
// withholds its replies to the methods it names; every step is bounded on its
// own (quitStepBound), and each bound is generous for the deadline the step
// runs to and short of the client's own 3 s detach wait, which the defect
// spent after the quit's.

// quitStepBound bounds one step of a quit here: well past any deadline these
// tests set, and short of remote's closeBound (3 s), the detach wait a close
// with no deadline of its own would spend.
const quitStepBound = 2 * time.Second

// replyProxy is a Unix socket in front of a host's that passes every line
// through but the host's replies to the requests whose method it withholds,
// which it drops — a host that has stopped answering them. It counts every
// request the client makes, by method, and reports each reply it drops.
type replyProxy struct {
	path     string
	withhold map[string]bool
	withheld chan string

	mu    sync.Mutex
	sent  map[string]int
	conns []net.Conn
}

// newReplyProxy serves a proxy to target, withholding the replies to
// withhold's methods, until the test ends.
func newReplyProxy(t *testing.T, target string, withhold ...string) *replyProxy {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "czp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := &replyProxy{path: filepath.Join(dir, "p"), withhold: map[string]bool{}, withheld: make(chan string, 16),
		sent: map[string]int{}}
	for _, m := range withhold {
		p.withhold[m] = true
	}
	l, err := net.Listen("unix", p.path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("unix", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			ids := &idMethods{m: map[string]string{}}
			wg.Add(2)
			go func() { defer wg.Done(); p.pump(c, up, true, ids) }()
			go func() { defer wg.Done(); p.pump(up, c, false, ids) }()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		p.mu.Lock()
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.mu.Unlock()
		wg.Wait()
	})
	return p
}

// idMethods is one connection's request ids, each with its method.
type idMethods struct {
	mu sync.Mutex
	m  map[string]string
}

// pump copies src's lines to dst — a client's (out) or the host's — noting
// each request's method, and dropping the host's replies to a withheld one.
// Either side ending ends both.
func (p *replyProxy) pump(src, dst net.Conn, out bool, ids *idMethods) {
	defer func() { _ = src.Close(); _ = dst.Close() }()
	r := bufio.NewReader(src)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			_ = json.Unmarshal(line, &msg)
			switch {
			case out && msg.Method != "" && len(msg.ID) > 0:
				ids.mu.Lock()
				ids.m[string(msg.ID)] = msg.Method
				ids.mu.Unlock()
				p.mu.Lock()
				p.sent[msg.Method]++
				p.mu.Unlock()
			case !out && msg.Method == "" && len(msg.ID) > 0:
				ids.mu.Lock()
				method := ids.m[string(msg.ID)]
				ids.mu.Unlock()
				if p.withhold[method] {
					select {
					case p.withheld <- method:
					default:
					}
					continue
				}
			}
			if _, werr := dst.Write(line); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// count is how many requests of method the client has made.
func (p *replyProxy) count(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent[method]
}

// dialVia is a remote.Session to a host through p, closed when the test ends.
func dialVia(t *testing.T, p *replyProxy) *remote.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	s, err := remote.DialSession(ctx, p.path, remote.SessionOptions{
		Client: remote.Options{Client: protocol.ClientInfo{Kind: "test", Name: "tui_test"}},
	})
	if err != nil {
		t.Fatalf("dial through the proxy: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// shortQuit sets quitStopWait for one test.
func shortQuit(t *testing.T, d time.Duration) {
	t.Helper()
	prev := quitStopWait
	quitStopWait = d
	t.Cleanup(func() { quitStopWait = prev })
}

// within runs f on a goroutine and fails the test unless it returns within
// quitStepBound.
func within[T any](t *testing.T, what string, f func() T) T {
	t.Helper()
	out := make(chan T, 1)
	go func() { out <- f() }()
	select {
	case v := <-out:
		return v
	case <-time.After(quitStepBound):
		t.Fatalf("%s did not return within %v", what, quitStepBound)
		var zero T
		return zero
	}
}

// ctrlD is the quit key, sent to r's model; it answers the command it handed
// back.
func (r *gateRig) ctrlD() tea.Cmd {
	r.t.Helper()
	tm, cmd := r.m.gated(tea.KeyMsg{Type: tea.KeyCtrlD}, Model.update)
	r.m = tm.(Model)
	return cmd
}

// finish is finishRun over r's model, as Run makes it once the program has
// ended, within quitStepBound.
func (r *gateRig) finish() {
	r.t.Helper()
	within(r.t, "the run's exit tail (finishRun)", func() bool {
		_, _ = finishRun(io.Discard, r.m, r.m, nil)
		return true
	})
}

// TestTheQuitIsBoundedByItsOneDeadline (plan 030 C5r, astra r8-c5 4): the
// explicit quit and its cleanup share one deadline, whatever the host does.
//
//   - A host that never answers the stop (its receipt withheld, and every
//     detach's reply with it): the quit waits out its deadline and quits,
//     saying the stop was answered neither way; the close after it detaches
//     nothing — the deadline has passed — and returns at once, where it used
//     to start a detach wait of its own.
//   - An older host that refuses the stop and then never answers the detach
//     that follows: the detach waits only for what is left of the quit's
//     deadline.
//   - A second quit while the first is actually blocked — the stop's request
//     is on the host, its reply withheld, and the quit's deadline far off:
//     the program quits at once, the close detaches nothing and returns at
//     once, and that close is what frees the first quit's wait.
//   - The quit's own cleanup does not spend its deadline: the stop goes out
//     beside the host-status release, not after it — a release that waits
//     for the stop to reach the host sees it arrive, where the stop used to
//     be sent only once the release was over (up to its own second).
//   - A host that stops reading (C5r2, astra r9-fix45 1): an outstanding
//     command's line, longer than the socket holds, has filled it, its write
//     never done, so the stop's request waits for the connection's write lock
//     where its context never reaches it. The quit still ends at its
//     deadline — it closes the transport, which ends both writes — and it
//     has joined its stop when it returns: nothing of the quit is left
//     running, where it used to wait on the stop for ever.
func TestTheQuitIsBoundedByItsOneDeadline(t *testing.T) {
	hang := func(control.StopRequest) {} // a stop that ends nothing

	t.Run("a withheld stop reply", func(t *testing.T) {
		isolateSkillsHome(t)
		shortQuit(t, 200*time.Millisecond)
		h := newAttachHostStop(t, true, hang)
		p := newReplyProxy(t, h.path, protocol.MethodSessionStop, protocol.MethodSessionDetach)
		r := startedOver(t, dialVia(t, p), true, frameWorkspace(t))
		quits := namedCmds(r.ctrlD(), "stopQuit")
		if len(quits) != 1 {
			t.Fatalf("Ctrl+D asked %d stops, want one", len(quits))
		}
		if _, ok := within(t, "the quit", quits[0]).(tea.QuitMsg); !ok {
			t.Fatal("the quit did not quit the program")
		}
		if stopped, unsupported, err := r.m.exit.outcome(); stopped || unsupported || err == nil {
			t.Fatalf("the quit's stop: stopped %v, unsupported %v, %v; want answered neither way", stopped, unsupported, err)
		}
		r.finish()
		if n := p.count(protocol.MethodSessionDetach); n != 0 {
			t.Fatalf("the close after the quit's deadline sent %d detaches, want none", n)
		}
	})

	t.Run("a withheld detach reply", func(t *testing.T) {
		isolateSkillsHome(t)
		shortQuit(t, 200*time.Millisecond)
		h := newAttachHostStop(t, true, nil) // refuses session.stop, stop_unsupported
		p := newReplyProxy(t, h.path, protocol.MethodSessionDetach)
		r := startedOver(t, dialVia(t, p), true, frameWorkspace(t))
		quits := namedCmds(r.ctrlD(), "stopQuit")
		if len(quits) != 1 {
			t.Fatalf("Ctrl+D asked %d stops, want one", len(quits))
		}
		if _, ok := within(t, "the quit", quits[0]).(tea.QuitMsg); !ok {
			t.Fatal("the quit did not quit the program")
		}
		if stopped, unsupported, err := r.m.exit.outcome(); stopped || !unsupported || err != nil {
			t.Fatalf("the quit's stop: stopped %v, unsupported %v, %v; want refused stop_unsupported", stopped, unsupported, err)
		}
		r.finish()
	})

	t.Run("the stop beside the host's release", func(t *testing.T) {
		isolateSkillsHome(t)
		shortQuit(t, 200*time.Millisecond)
		h := newAttachHostStop(t, true, hang)
		p := newReplyProxy(t, h.path, protocol.MethodSessionStop, protocol.MethodSessionDetach)
		r := startedOver(t, dialVia(t, p), true, frameWorkspace(t))
		rel := &releaseHost{stop: p.withheld, saw: make(chan bool, 1)}
		r.m.host = rel
		quits := namedCmds(r.ctrlD(), "stopQuit")
		if len(quits) != 1 {
			t.Fatalf("Ctrl+D asked %d stops, want one", len(quits))
		}
		if _, ok := within(t, "the quit", quits[0]).(tea.QuitMsg); !ok {
			t.Fatal("the quit did not quit the program")
		}
		select {
		case saw := <-rel.saw:
			if !saw {
				t.Fatal("the stop was not sent while the host-status release ran: the release spent the quit's deadline")
			}
		default:
			t.Fatal("the quit did not release the host status")
		}
		r.finish()
	})

	t.Run("a second quit while the first is blocked", func(t *testing.T) {
		isolateSkillsHome(t)
		shortQuit(t, pumpWatchdog) // the first quit's deadline is far off
		h := newAttachHostStop(t, true, hang)
		p := newReplyProxy(t, h.path, protocol.MethodSessionStop, protocol.MethodSessionDetach)
		r := startedOver(t, dialVia(t, p), true, frameWorkspace(t))
		quits := namedCmds(r.ctrlD(), "stopQuit")
		if len(quits) != 1 {
			t.Fatalf("Ctrl+D asked %d stops, want one", len(quits))
		}
		first := make(chan tea.Msg, 1)
		go func() { first <- quits[0]() }()
		select {
		case m := <-p.withheld:
			if m != protocol.MethodSessionStop {
				t.Fatalf("the proxy withheld a %s reply first", m)
			}
		case <-time.After(pumpWatchdog):
			t.Fatal("the first quit's stop never reached the host")
		}
		select {
		case <-first:
			t.Fatal("the first quit returned with its stop's reply withheld and its deadline far off")
		default:
		}

		second := r.ctrlD()
		if len(namedCmds(second, "stopQuit")) != 0 || len(namedCmds(second, "bubbletea.Quit")) != 1 {
			t.Fatal("a second quit did not quit at once")
		}
		r.finish()
		if n := p.count(protocol.MethodSessionDetach); n != 0 {
			t.Fatalf("the close after a second quit sent %d detaches, want none", n)
		}
		select {
		case msg := <-first:
			if _, ok := msg.(tea.QuitMsg); !ok {
				t.Fatalf("the first quit answered %#v", msg)
			}
		case <-time.After(quitStepBound):
			t.Fatalf("the first quit still waits %v after the close", quitStepBound)
		}
		if stopped, unsupported, err := r.m.exit.outcome(); !stopped || unsupported || err != nil {
			t.Fatalf("the quit's stop: stopped %v, unsupported %v, %v; want on its way when the program ended", stopped, unsupported, err)
		}
	})

	t.Run("a host that stops reading", func(t *testing.T) {
		isolateSkillsHome(t)
		shortQuit(t, 200*time.Millisecond)
		h := newAttachHostStop(t, true, hang)
		p := newStallProxy(t, h.path)
		w := &writeWatch{began: make(chan struct{})}
		s := dialWatched(t, p, w)
		b := &stopJoined{Session: s, returned: make(chan struct{})}
		r := startedOver(t, b, true, frameWorkspace(t))

		// The host reads nothing more, and an outstanding command's line —
		// a prompt longer than the socket holds — is being written: its
		// write holds the connection's write lock and never finishes.
		p.stall()
		submitted := make(chan error, 1)
		go func() {
			c := engine.Command{Client: s.ClientID(), ID: "9001"}
			_, err := s.Submit(context.Background(), c, strings.Repeat("x", longLine), engine.SubmitQueue, "")
			submitted <- err
		}()
		select {
		case <-w.began:
		case <-time.After(pumpWatchdog):
			t.Fatal("the outstanding command's write never began")
		}

		quits := namedCmds(r.ctrlD(), "stopQuit")
		if len(quits) != 1 {
			t.Fatalf("Ctrl+D asked %d stops, want one", len(quits))
		}
		if _, ok := within(t, "the quit", quits[0]).(tea.QuitMsg); !ok {
			t.Fatal("the quit did not quit the program")
		}
		select {
		case <-b.returned:
		default:
			t.Fatal("the quit returned with its stop still running")
		}
		if stopped, unsupported, err := r.m.exit.outcome(); stopped || unsupported || err == nil {
			t.Fatalf("the quit's stop: stopped %v, unsupported %v, %v; want answered neither way", stopped, unsupported, err)
		}
		// The transport the quit closed ended the outstanding command's
		// write too: neither write outlives the quit.
		select {
		case err := <-submitted:
			if err == nil {
				t.Fatal("the outstanding command was answered by a host that read none of it")
			}
		case <-time.After(quitStepBound):
			t.Fatalf("the outstanding command's write still runs %v after the quit", quitStepBound)
		}
		r.finish()
	})
}

// longLine is the outstanding command's prompt in "a host that stops
// reading": far longer than a Unix socket's buffers hold (a few hundred KiB at
// most, on Linux and macOS), well inside a host's inbound line limit.
const longLine = 1 << 20

// stallProxy is a Unix socket in front of a host's that passes every byte
// both ways until stall: from then on it reads nothing more of what the
// client writes — a host that has stopped reading, whose receive buffer the
// client's next long line fills, so that the line's write blocks. What the
// host writes still reaches the client.
type stallProxy struct {
	path    string
	stalled chan struct{}
	once    sync.Once
}

// newStallProxy serves a proxy to target until the test ends.
func newStallProxy(t *testing.T, target string) *stallProxy {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "czp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := &stallProxy{path: filepath.Join(dir, "p"), stalled: make(chan struct{})}
	l, err := net.Listen("unix", p.path)
	if err != nil {
		t.Fatal(err)
	}
	// over ends a stalled pump at the test's end: it holds what it would
	// have read until then.
	over := make(chan struct{})
	var mu sync.Mutex
	var conns []net.Conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("unix", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, c, up)
			mu.Unlock()
			wg.Add(2)
			go func() {
				defer wg.Done()
				p.inbound(c, up, over)
			}()
			go func() {
				defer wg.Done()
				defer func() { _ = up.Close(); _ = c.Close() }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		close(over)
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return p
}

// stall makes the proxy stop reading the client: at most the one read in
// flight is taken, and never forwarded.
func (p *stallProxy) stall() { p.once.Do(func() { close(p.stalled) }) }

// inbound copies the client's bytes to the host until the proxy stalls, then
// reads nothing more until over.
func (p *stallProxy) inbound(src, dst net.Conn, over <-chan struct{}) {
	defer func() { _ = src.Close(); _ = dst.Close() }()
	buf := make([]byte, 4096)
	for {
		select {
		case <-p.stalled:
			<-over
			return
		default:
		}
		n, err := src.Read(buf)
		if n > 0 {
			select {
			case <-p.stalled:
				<-over
				return
			default:
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// writeWatch says when the client's write of a long line has begun (began,
// closed once): the outstanding command holds its connection's write lock.
// With detach set, it also says when the client's first session.detach
// request's write begins (closed once): a view close has begun its detach,
// and holds the Session's one close while it waits for the answer.
type writeWatch struct {
	began  chan struct{}
	once   sync.Once
	detach chan struct{}
	donce  sync.Once
}

// watchedConn is a client connection that tells its writeWatch of each long
// line's write, and of a detach's, as it begins.
type watchedConn struct {
	net.Conn
	w *writeWatch
}

func (c *watchedConn) Write(b []byte) (int, error) {
	if len(b) >= longLine {
		c.w.once.Do(func() { close(c.w.began) })
	}
	if c.w.detach != nil && bytes.Contains(b, []byte(`"method":"`+protocol.MethodSessionDetach+`"`)) {
		c.w.donce.Do(func() { close(c.w.detach) })
	}
	return c.Conn.Write(b)
}

// dialWatched is a remote.Session to a host through p whose connections
// report to w, closed when the test ends.
func dialWatched(t *testing.T, p *stallProxy, w *writeWatch) *remote.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	s, err := remote.DialSession(ctx, p.path, remote.SessionOptions{
		Client: remote.Options{
			Client: protocol.ClientInfo{Kind: "test", Name: "tui_test"},
			Dial: func(ctx context.Context, path string) (net.Conn, error) {
				var d net.Dialer
				nc, err := d.DialContext(ctx, "unix", path)
				if err != nil {
					return nil, err
				}
				return &watchedConn{Conn: nc, w: w}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("dial through the proxy: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// stopJoined is a session whose Stop says when it has returned (returned): a
// quit that returns after it has joined its stop.
type stopJoined struct {
	*remote.Session
	returned chan struct{}
}

func (b *stopJoined) Stop(ctx context.Context, c engine.Command) error {
	defer close(b.returned)
	return b.Session.Stop(ctx, c)
}

// releaseHost is a host-status hub whose release (Close) waits, within its own
// budget, for the stop's request to reach the host — stop, the proxy's
// withheld replies — and reports on saw whether it did.
type releaseHost struct {
	stop <-chan string
	saw  chan bool
}

func (h *releaseHost) Publish(host.Status) {}

func (h *releaseHost) Close(ctx context.Context) {
	select {
	case <-h.stop:
		h.saw <- true
	case <-ctx.Done():
		h.saw <- false
	}
}
