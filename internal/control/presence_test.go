package control_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/control/wiretest"
	"github.com/charliek/craze/internal/protocol"
)

// Presence (plan 032 §3.14, SF-64; conn.go's "Presence"): a Presence server
// tells each attachment how many clients are attached — after its
// synchronized, then on every change, the latest at most twice a second, never
// sequenced or journalled — and a slow reader holds back its own count and no
// one else's (R2-6); a LocalClient server counts its own process's TUI too
// (R3-1).

// presenceWithin is how soon a healthy client must see a change (A15).
const presenceWithin = time.Second

// withPresence serves capabilities.presence, with the host TUI's seat counted
// too when local (control.Options.LocalClient).
func withPresence(local bool) hostOpt {
	return func(c *hostConfig) { c.opts.Presence, c.opts.LocalClient = true, local }
}

// presenceSeen is one presence notification a watched client read, and when.
type presenceSeen struct {
	sub string
	n   uint
	at  time.Time
}

// lineWatch reads one client's every line on a goroutine of its own, from the
// moment it is started — so nothing else may read that client — and keeps
// what a presence test asks of them: every presence (schema-checked), the last
// event's seq, how many events, every reply's id, and the last line read.
type lineWatch struct {
	mu       sync.Mutex
	seen     []presenceSeen
	events   int
	lastSeq  uint64
	replies  map[string]*protocol.Response
	last     []byte
	err      error
	grew     chan struct{}
	finished chan struct{}
}

func watchLines(c *client) *lineWatch {
	w := &lineWatch{replies: map[string]*protocol.Response{}, grew: make(chan struct{}), finished: make(chan struct{})}
	go func() {
		defer close(w.finished)
		for {
			_ = c.nc.SetReadDeadline(time.Time{})
			line, err := c.lr.ReadLine()
			if err != nil {
				w.update(func() { w.err = err })
				return
			}
			w.take(line)
		}
	}()
	return w
}

func (w *lineWatch) update(f func()) {
	w.mu.Lock()
	f()
	close(w.grew)
	w.grew = make(chan struct{})
	w.mu.Unlock()
}

func (w *lineWatch) take(line []byte) {
	var m struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
		Params struct {
			Seq uint64 `json:"seq"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		w.update(func() { w.err = err })
		return
	}
	at := time.Now()
	w.update(func() {
		w.last = append([]byte(nil), line...)
		switch m.Method {
		case protocol.NotifyPresence:
			if err := wiretest.Default().Notification(line); err != nil {
				w.err = err
				return
			}
			var n protocol.Notification
			var p protocol.PresenceParams
			if err := json.Unmarshal(line, &n); err != nil {
				w.err = err
				return
			}
			if err := json.Unmarshal(n.Params, &p); err != nil {
				w.err = err
				return
			}
			w.seen = append(w.seen, presenceSeen{sub: p.Subscription, n: p.Attached, at: at})
		case protocol.NotifyEvent:
			w.events++
			w.lastSeq = m.Params.Seq
		case "":
			var r protocol.Response
			if err := json.Unmarshal(line, &r); err != nil {
				w.err = err
				return
			}
			w.replies[string(r.ID)] = &r
		}
	})
}

// until waits, within the watchdog, for cond over the watch; it fails the test
// with what the watch holds otherwise.
func (w *lineWatch) until(t *testing.T, what string, cond func(*lineWatch) bool) {
	t.Helper()
	deadline := time.After(watchdog)
	for {
		w.mu.Lock()
		if w.err != nil && !errors.Is(w.err, io.EOF) {
			err := w.err
			w.mu.Unlock()
			t.Fatalf("%s: the watched client's read failed: %v", what, err)
		}
		ok := cond(w)
		grew := w.grew
		seen := append([]presenceSeen(nil), w.seen...)
		w.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-grew:
		case <-deadline:
			t.Fatalf("%s: not within %s; presence seen %v", what, watchdog, seen)
		}
	}
}

// waitCount waits for n to be the latest presence the client read, and
// answers when it read it.
func (w *lineWatch) waitCount(t *testing.T, what string, n uint) time.Time {
	t.Helper()
	var at time.Time
	w.until(t, what, func(w *lineWatch) bool {
		if k := len(w.seen); k > 0 && w.seen[k-1].n == n {
			at = w.seen[k-1].at
			return true
		}
		return false
	})
	return at
}

// counts is every presence count read, in order.
func (w *lineWatch) counts() []uint {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]uint, len(w.seen))
	for i, s := range w.seen {
		out[i] = s.n
	}
	return out
}

// within fails unless at is no later than presenceWithin after since.
func within(t *testing.T, what string, since, at time.Time) {
	t.Helper()
	if d := at.Sub(since); d > presenceWithin {
		t.Fatalf("%s %s after the change, want within %s", what, d, presenceWithin)
	}
}

// countChanges records when the server's count changed, and to what: an
// attachments listener's, so each instant is the change's own (it runs in the
// section that makes it) — what "within a second of the change" is measured
// from, whatever a client's hello and attach cost before it.
type countChanges struct {
	mu   sync.Mutex
	seen []presenceSeen
	grew chan struct{}
}

func recordChanges(h *host) *countChanges {
	c := &countChanges{grew: make(chan struct{})}
	h.srv.AddAttachmentsListener(func(n int) {
		at := time.Now()
		c.mu.Lock()
		c.seen = append(c.seen, presenceSeen{n: uint(n), at: at})
		close(c.grew)
		c.grew = make(chan struct{})
		c.mu.Unlock()
	})
	return c
}

// to waits, within the watchdog, for the count's latest change to be to n,
// and answers when it was made.
func (c *countChanges) to(t *testing.T, n uint) time.Time {
	t.Helper()
	deadline := time.After(watchdog)
	for {
		c.mu.Lock()
		if k := len(c.seen); k > 0 && c.seen[k-1].n == n {
			at := c.seen[k-1].at
			c.mu.Unlock()
			return at
		}
		grew := c.grew
		c.mu.Unlock()
		select {
		case <-grew:
		case <-deadline:
			t.Fatalf("the count never changed to %d in %s", n, watchdog)
		}
	}
}

// attachAndCount says hello on a new connection, attaches, and reads the
// attach reply, synchronized and the presence that follows it, which must say
// want; it answers the client and its subscription id.
func attachAndCount(t *testing.T, h *host, want uint) (*client, string) {
	t.Helper()
	c := h.dial()
	c.sayHello(nil)
	res := c.attach(protocol.AttachParams{SessionID: sid(h)})
	if !res.Session.Capabilities.Presence {
		t.Fatalf("the attach reply's capabilities say no presence: %+v", res.Session.Capabilities)
	}
	c.note(protocol.NotifySynchronized)
	p := paramsOf[protocol.PresenceParams](t, c.note(protocol.NotifyPresence))
	if p.Subscription != res.Subscription || p.Attached != want {
		t.Fatalf("the presence after synchronized is %+v, want %d on %s", p, want, res.Subscription)
	}
	return c, res.Subscription
}

// listRow is the connection's sessions.list row, and its raw JSON.
func listRow(t *testing.T, c *client) (protocol.SessionRow, string) {
	t.Helper()
	resp := c.call(protocol.MethodSessionsList, protocol.SessionsListParams{})
	list := ok[protocol.SessionsListResult](t, resp)
	if len(list.Sessions) != 1 {
		t.Fatalf("the list holds %d rows", len(list.Sessions))
	}
	var raw struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(resp.Result, &raw); err != nil {
		t.Fatal(err)
	}
	return list.Sessions[0], string(raw.Sessions[0])
}

// TestPresenceFollowsSynchronized: on a Presence host an attachment is told
// the count once its synchronized is written — its own attachment counted —
// and the row a connection lists says it too, a listing connection not
// counted. The negative control: a host without presence says none of it — no
// capability, no notification before a later reply, no attached on the row —
// which is an older host's wire.
func TestPresenceFollowsSynchronized(t *testing.T) {
	h := newHost(t, withPresence(false))
	a, _ := attachAndCount(t, h, 1)
	row, _ := listRow(t, a)
	if !row.Capabilities.Presence || row.Attached != 1 {
		t.Fatalf("the attached client's row says presence %v, attached %d; want true and 1", row.Capabilities.Presence, row.Attached)
	}
	lister := h.dial()
	lister.sayHello(nil)
	if row, _ := listRow(t, lister); row.Attached != 1 {
		t.Fatalf("a listing connection's row says attached %d, want 1: it is not attached itself", row.Attached)
	}

	old := newHost(t)
	c := old.dial()
	c.sayHello(nil)
	res := c.attach(protocol.AttachParams{SessionID: sid(old)})
	if res.Session.Capabilities.Presence {
		t.Fatal("a host without presence says it has it")
	}
	c.note(protocol.NotifySynchronized)
	if notes, _ := c.sync(old); len(notes) != 0 {
		t.Fatalf("a host without presence sent %s before a reply", notes[0].Method)
	}
	if row, raw := listRow(t, c); row.Attached != 0 || strings.Contains(raw, `"attached"`) || strings.Contains(raw, `"presence"`) {
		t.Fatalf("a host without presence lists %s", raw)
	}
}

// TestAStalledAttachmentHoldsBackOnlyItself (A15, R2-6): a second attach
// shows 2 to both clients within a second; a third attach shows 3 to both
// within a second, and the third then stops reading while a backlog fills its
// socket — its writer stalled, its attachment still counted. With it stalled,
// the two healthy clients see each change within a second all the same: a
// fourth joining (4, which it sees too) and leaving (3); and when one healthy
// client leaves, the other sees 2 — the stalled one still counted. Once the
// stalled client reads again it gets its backlog and then the latest count,
// coalesced. The negative control: the stalled client's outbox holds lines its
// writer has not taken throughout, so it really was stalled — a count
// delivered by writing to every connection in turn would have waited on it.
func TestAStalledAttachmentHoldsBackOnlyItself(t *testing.T) {
	h := newHost(t, withPresence(false), withStall(10*time.Minute))
	changes := recordChanges(h)
	a, _ := attachAndCount(t, h, 1)
	wa := watchLines(a)

	// attachAndCount returns once the second client has read its count, 2.
	b, bsub := attachAndCount(t, h, 2)
	within(t, "the second client saw 2", changes.to(t, 2), time.Now())
	within(t, "the first client saw 2", changes.to(t, 2), wa.waitCount(t, "the first client sees 2", 2))
	wb := watchLines(b)

	stalled, _ := attachAndCount(t, h, 3)
	within(t, "the first client saw 3", changes.to(t, 3), wa.waitCount(t, "the first client sees 3", 3))
	within(t, "the second client saw 3", changes.to(t, 3), wb.waitCount(t, "the second client sees 3", 3))

	// The third stops reading; a backlog well past any socket buffer — and
	// well inside the subscription's 8 MiB and the writer's 32 MiB — fills
	// its socket, and its writer stalls.
	const backlog = 16
	chunk := strings.Repeat("o", 64<<10)
	for range backlog {
		h.publish(agent.Event{Type: agent.EventText, Text: chunk})
	}
	head := h.head()
	for _, w := range []*lineWatch{wa, wb} {
		w.until(t, "a healthy client reads the backlog", func(w *lineWatch) bool { return w.lastSeq == head })
	}
	stalledNow := func(what string) {
		t.Helper()
		if h.srv.Queued() == 0 {
			t.Fatalf("%s: no outbox holds a line its writer has not taken: the third client's writer is not stalled", what)
		}
	}
	stalledNow("after the backlog")

	// The newcomer is told 4 after its own synchronized (attachAndCount);
	// how long that takes is its attach's — a snapshot holding the backlog —
	// and no count's.
	d, _ := attachAndCount(t, h, 4)
	within(t, "the first client saw 4", changes.to(t, 4), wa.waitCount(t, "the first client sees 4", 4))
	within(t, "the second client saw 4", changes.to(t, 4), wb.waitCount(t, "the second client sees 4", 4))

	_ = d.nc.Close()
	within(t, "the first client saw 3", changes.to(t, 3), wa.waitCount(t, "the first client sees 3 again", 3))
	within(t, "the second client saw 3", changes.to(t, 3), wb.waitCount(t, "the second client sees 3 again", 3))

	ok[protocol.Empty](t, waitReply(t, wb, b.send(protocol.MethodSessionDetach, protocol.DetachParams{SessionID: sid(h), Subscription: bsub})))
	within(t, "the first client saw 2 with the stalled one counted", changes.to(t, 2), wa.waitCount(t, "the first client sees 2 again", 2))
	stalledNow("after the changes")

	// The stalled client reads again: its backlog, then the latest count.
	ws := watchLines(stalled)
	ws.until(t, "the stalled client reads its backlog", func(w *lineWatch) bool { return w.lastSeq == head })
	ws.waitCount(t, "the stalled client sees the latest count", 2)
	if got := ws.counts(); len(got) != 1 {
		t.Fatalf("the stalled client read the counts %v after its backlog, want the latest alone (2)", got)
	}
}

// waitReply waits for the watched client's reply to id.
func waitReply(t *testing.T, w *lineWatch, id string) *protocol.Response {
	t.Helper()
	var r *protocol.Response
	w.until(t, "the reply to "+id, func(w *lineWatch) bool {
		r = w.replies[id]
		return r != nil
	})
	return r
}

// idleAt is one park of a writer with nothing to write (TestHooks.WriterIdle).
type idleAt struct{ conn, taken uint64 }

// awaitIdle waits, within the watchdog, for connection conn's writer to park
// having taken taken lines.
func awaitIdle(t *testing.T, idles <-chan idleAt, conn, taken uint64) {
	t.Helper()
	deadline := time.After(watchdog)
	for {
		select {
		case i := <-idles:
			if i.conn == conn && i.taken == taken {
				return
			}
		case <-deadline:
			t.Fatalf("connection %d's writer never parked having taken %d lines", conn, taken)
		}
	}
}

// TestAnArmedSlotWakesItsWriter (review r29 item 7): the forwarder queues an
// attachment's synchronized and arms its presence slot in two steps, and the
// writer may take and write the synchronized, and park, between them — its
// one wake spent, nothing more queued on an idle session. Forced: the arm is
// held (BeforeArm) until the writer has written the synchronized and parked
// with no wake pending (WriterIdle, three lines taken: hello's reply, the
// attach's, the synchronized; the barrier is told only once any wake still
// buffered — the synchronized's own, say — has been taken and looked past).
// Released, the arm itself wakes the writer, and the count follows. The
// negative control is the premise, checked: the writer did park, its wakes
// all spent, with the synchronized written and the slot not yet armed — so
// without the arm's own wake nothing would ever send the count, and the read
// of it times out (verified by removing that wake: the test then fails at
// that read, every run).
func TestAnArmedSlotWakesItsWriter(t *testing.T) {
	ackHeld, releaseAck := make(chan struct{}), make(chan struct{})
	armHeld, releaseArm := make(chan struct{}), make(chan struct{})
	idles := make(chan idleAt, 1024)
	h := newHost(t, withPresence(false), withHooks(control.TestHooks{
		AckQueued: func(_, method string) {
			if method == protocol.MethodSessionAttach {
				close(ackHeld)
				<-releaseAck
			}
		},
		BeforeArm: func(string) {
			close(armHeld)
			<-releaseArm
		},
		WriterIdle: func(conn, taken uint64) {
			select {
			case idles <- idleAt{conn, taken}:
			default:
			}
		},
	}))
	// A failure while either is held lets it go before the server closes.
	t.Cleanup(func() { closeOnce(releaseAck); closeOnce(releaseArm) })
	c := h.dial()
	c.sayHello(nil)
	id := c.send(protocol.MethodSessionAttach, protocol.AttachParams{SessionID: sid(h)})
	await(t, ackHeld, "the attach reply to be queued")
	res := ok[protocol.AttachResult](t, c.reply(id))
	// The reply written and its slot given back (under conn.mu, which the
	// held arm will hold): the writer has parked after it.
	awaitIdle(t, idles, 1, 2)
	closeOnce(releaseAck)
	await(t, armHeld, "the arm to be held")
	awaitIdle(t, idles, 1, 3)
	c.note(protocol.NotifySynchronized)
	closeOnce(releaseArm)
	if p := paramsOf[protocol.PresenceParams](t, c.note(protocol.NotifyPresence)); p.Subscription != res.Subscription || p.Attached != 1 {
		t.Fatalf("the count after the late arm: %+v, want 1 on %s", p, res.Subscription)
	}
}

// TestPresenceCoalesces: a burst of attaches and detaches by other clients —
// twenty-two changes, as fast as they can be made — reaches an attached client
// as at most two counts a second, each different from the last, ending on the
// latest. The burst moves the count between 1 and 2 and ends at 3, a count it
// never had before, held there: once the client has read 3 it has read every
// line the writer took for it, and the writer takes no more, so the two
// totals compare (an earlier 2 can never pass for the end).
//
// The client's writer is held through the burst, whatever the CPU: the first
// presence line it takes once the burst has begun stops it (PresenceTaken
// blocks) until the count has reached 3 — every change made, the listener
// says. However slowly the burst runs, it can then have taken at most that
// one line and the latest, so the coalescing is forced and its bound holds
// deterministically: at most two counts read, of twenty-two changes (a burst
// slow enough to space its changes past the 500 ms interval would otherwise
// deliver every one, as it should). The interval is the server's own too:
// every presence line its writer took for that connection is 500 ms or more
// after the one before (the hook's instants). The negative control: the
// server's listener heard every one of the twenty-two changes, so the counts
// the client read are fewer because they were coalesced, not because the
// changes were few.
func TestPresenceCoalesces(t *testing.T) {
	var mu sync.Mutex
	taken := map[uint64][]time.Time{}
	// hold, while set, stops the first client's writer (connection 1) at the
	// presence line it takes, until it is closed.
	var hold chan struct{}
	h := newHost(t, withPresence(false), withHooks(control.TestHooks{PresenceTaken: func(conn uint64, _ []byte, at time.Time) {
		mu.Lock()
		taken[conn] = append(taken[conn], at)
		g := hold
		mu.Unlock()
		if conn == 1 && g != nil {
			<-g
		}
	}}))
	release := make(chan struct{})
	// Registered after the server's close, so it runs first: a failure with
	// the writer held lets it go.
	t.Cleanup(func() { closeOnce(release) })
	l := watchAttachments(h)
	a, _ := attachAndCount(t, h, 1)
	wa := watchLines(a)
	mu.Lock()
	hold = release
	mu.Unlock()

	start := time.Now()
	const rounds = 10
	for range rounds {
		c := h.dial()
		c.sayHello(nil)
		res := c.attach(protocol.AttachParams{SessionID: sid(h)})
		_, resp := c.until(c.send(protocol.MethodSessionDetach, protocol.DetachParams{SessionID: sid(h), Subscription: res.Subscription}))
		ok[protocol.Empty](t, resp)
	}
	// Two stay: the count ends at 3, which it has not been before, and is
	// held there.
	for range 2 {
		stay := h.dial()
		stay.sayHello(nil)
		stay.attach(protocol.AttachParams{SessionID: sid(h)})
	}
	const burst = 2*rounds + 2
	changes := l.waitLen(t, 1+burst)
	// Every change made, the count at 3 and held: the writer goes on.
	closeOnce(release)
	wa.waitCount(t, "the first client sees the latest count", 3)
	elapsed := time.Since(start)

	if len(changes) != 1+burst {
		t.Fatalf("the listener heard %v, want %d changes", changes, 1+burst)
	}
	got := wa.counts()
	if n := len(got); n == 0 || got[n-1] != 3 || slices.Index(got, 3) != n-1 {
		t.Fatalf("the client read %v, want the burst's end, 3, once and last", got)
	}
	// The writer held from its first line of the burst: that line, if it
	// took one, and the latest — and, whatever the burst's pace, at most one
	// count a half second of it.
	bound := 2 + int(elapsed/(500*time.Millisecond)) + 1
	if len(got) > 2 || len(got) > bound || len(got) >= burst {
		t.Fatalf("the client read %d counts %v over %s of %d changes, want at most 2", len(got), got, elapsed, len(changes))
	}
	for i := 1; i < len(got); i++ {
		if got[i] == got[i-1] {
			t.Fatalf("the client read the same count twice running: %v", got)
		}
	}
	mu.Lock()
	ats := append([]time.Time(nil), taken[1]...)
	mu.Unlock()
	// The first is the count attachAndCount read, before the watch.
	if len(ats) != 1+len(got) {
		t.Fatalf("the writer took %d presence lines for the first client, which read %d after its first", len(ats), len(got))
	}
	for i := 1; i < len(ats); i++ {
		if d := ats[i].Sub(ats[i-1]); d < 500*time.Millisecond {
			t.Fatalf("two presence lines %s apart on one connection, want 500ms or more: %v", d, ats)
		}
	}
}

// TestNoPresenceOutsideAnAttachment: a detached attachment is told nothing
// more — a change after its detach's reply reaches its connection as no line —
// a re-attach on the same connection is told the count after its own
// synchronized, and a session's end leaves the reset the connection's last
// line, whatever the count does as it closes. The negative control: the
// change does reach another attached client, so there was a count to send.
func TestNoPresenceOutsideAnAttachment(t *testing.T) {
	var mu sync.Mutex
	var after []uint64
	var detached bool
	h := newHost(t, withPresence(false), withHooks(control.TestHooks{PresenceTaken: func(conn uint64, _ []byte, _ time.Time) {
		mu.Lock()
		if detached && conn == 1 {
			after = append(after, conn)
		}
		mu.Unlock()
	}}))
	a, sub := attachAndCount(t, h, 1)
	mu.Lock()
	detached = true
	mu.Unlock()
	ok[protocol.Empty](t, a.detach(h, sub))
	b, _ := attachAndCount(t, h, 1)
	// The count moved 0 → 1 with the first client detached; a second
	// attached client makes it move again, and is told so.
	wb := watchLines(b)
	c, _ := attachAndCount(t, h, 2)
	wb.waitCount(t, "another attached client sees the change", 2)
	// A whole interval, so a count the detached connection were owed could
	// not be waiting on its writer's bound.
	time.Sleep(600 * time.Millisecond)
	if notes, _ := a.sync(h); len(notes) != 0 {
		t.Fatalf("a detached connection was sent %s %s", notes[0].Method, notes[0].Params)
	}
	mu.Lock()
	if len(after) != 0 {
		mu.Unlock()
		t.Fatal("the writer took a presence line for a detached connection")
	}
	mu.Unlock()

	// Attached again on the same connection: its own synchronized, then the
	// count.
	res := a.attach(protocol.AttachParams{SessionID: sid(h)})
	a.note(protocol.NotifySynchronized)
	if p := paramsOf[protocol.PresenceParams](t, a.note(protocol.NotifyPresence)); p.Subscription != res.Subscription || p.Attached != 3 {
		t.Fatalf("the re-attach was told %+v, want 3 on %s", p, res.Subscription)
	}

	// The session ends: every attachment closes, the count with them, and
	// each connection's last line is its reset.
	if err := h.eng.Close(); err != nil {
		t.Fatal(err)
	}
	for _, cl := range []*client{a, c} {
		var lastLine []byte
		for {
			line, err := cl.readLine()
			if err != nil {
				break
			}
			lastLine = line
		}
		var n protocol.Notification
		if err := json.Unmarshal(lastLine, &n); err != nil || n.Method != protocol.NotifyReset {
			t.Fatalf("a connection's last line at the session's end is %s, want its reset", lastLine)
		}
	}
	<-wb.finished
	wb.mu.Lock()
	defer wb.mu.Unlock()
	var n protocol.Notification
	if err := json.Unmarshal(wb.last, &n); err != nil || n.Method != protocol.NotifyReset {
		t.Fatalf("the watched connection's last line is %s, want its reset", wb.last)
	}
}

// TestALocalClientIsCountedEverywhere (R3-1): a LocalClient server counts the
// host TUI's seat in every count a client or a listener sees — the presence
// after an attach, the row's attached, a listener's — but not in the count its
// close fence reads (Attached). The negative control is
// TestPresenceFollowsSynchronized's host, where the same attach is told 1.
func TestALocalClientIsCountedEverywhere(t *testing.T) {
	h := newHost(t, withPresence(true))
	l := watchAttachments(h)
	a, _ := attachAndCount(t, h, 2)
	l.wantCounts(t, "attached", 2)
	if row, _ := listRow(t, a); row.Attached != 2 {
		t.Fatalf("the row says attached %d, want 2 (the host TUI and this client)", row.Attached)
	}
	if n := h.srv.Attached(); n != 1 {
		t.Fatalf("the fence's count is %d, want 1: the host TUI's seat is no connection's", n)
	}
	lister := h.dial()
	lister.sayHello(nil)
	if row, _ := listRow(t, lister); row.Attached != 2 {
		t.Fatalf("a listing connection's row says attached %d, want 2", row.Attached)
	}
	_ = a.nc.Close()
	l.wantCounts(t, "the client gone", 2, 1)
}

// TestListenersAreIndependent: two attachments listeners each hear every
// change; one removed hears no more — removing it twice is harmless — and the
// other goes on hearing.
func TestListenersAreIndependent(t *testing.T) {
	h := newHost(t)
	first := watchAttachments(h)
	var mu sync.Mutex
	var second []int
	remove := h.srv.AddAttachmentsListener(func(n int) {
		mu.Lock()
		second = append(second, n)
		mu.Unlock()
	})
	c := h.dial()
	c.sayHello(nil)
	res := c.attach(protocol.AttachParams{SessionID: sid(h)})
	first.wantCounts(t, "attached", 1)
	remove()
	remove()
	_, resp := c.until(c.send(protocol.MethodSessionDetach, protocol.DetachParams{SessionID: sid(h), Subscription: res.Subscription}))
	ok[protocol.Empty](t, resp)
	first.wantCounts(t, "detached", 1, 0)
	mu.Lock()
	defer mu.Unlock()
	if len(second) != 1 || second[0] != 1 {
		t.Fatalf("the removed listener heard %v, want [1]", second)
	}
	if h.srv.AddAttachmentsListener(nil) == nil {
		t.Fatal("a nil listener's remove is nil")
	}
}

// TestPresenceIsNeitherSequencedNorJournalled: counts going back and forth
// move nothing in the session's log — its head stays where it was, a cursor
// attach from it is synchronized at once with nothing replayed — and its
// journal holds no trace of them. The negative control: the counts were sent
// (each client read them), and the journal is being written (it holds the
// connections' notes).
func TestPresenceIsNeitherSequencedNorJournalled(t *testing.T) {
	jw, lo := testJournal(t)
	h := newHost(t, withLog(lo), withPresence(false))
	h.publish(agent.Event{Type: agent.EventText, Text: "before anyone attaches"})
	head := h.head()
	a, _ := attachAndCount(t, h, 1)
	wa := watchLines(a)
	b, bsub := attachAndCount(t, h, 2)
	wa.waitCount(t, "the first client sees 2", 2)
	ok[protocol.Empty](t, b.detach(h, bsub))
	wa.waitCount(t, "the first client sees 1", 1)
	if got := h.head(); got != head {
		t.Fatalf("the log's head moved from %d to %d with the counts", head, got)
	}

	c := h.dial()
	c.sayHello(nil)
	res := c.attach(protocol.AttachParams{SessionID: sid(h), Cursor: &protocol.Cursor{Incarnation: lo.Incarnation, Seq: head}})
	if res.Snapshot != nil {
		t.Fatal("a cursor at the head was answered with a snapshot")
	}
	if s := paramsOf[protocol.SynchronizedParams](t, c.note(protocol.NotifySynchronized)); s.Seq != head {
		t.Fatalf("synchronized at %d, want the head %d: something was replayed", s.Seq, head)
	}
	if p := paramsOf[protocol.PresenceParams](t, c.note(protocol.NotifyPresence)); p.Attached != 2 {
		t.Fatalf("the cursor attach was told %d, want 2", p.Attached)
	}

	if notes := connNotes(t, jw); len(notes) == 0 {
		t.Fatal("the journal holds no connection notes: it is not the session's")
	}
	raw, err := os.ReadFile(jw.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("presence")) || bytes.Contains(raw, []byte(`"attached"`)) {
		t.Fatal("the journal holds a count")
	}
}
