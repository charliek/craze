package remote_test

import (
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// TestAKilledConnectionResumesSilently (A4, the first leg; §3.14): a
// connection killed mid-turn — as a command's reply arrives, so the host ran
// the command and its reply is lost — is redialled once the session has moved
// on without the client; the hello resumes the same client with its token
// (resumed: true); the stream re-attaches with its cursor, which the host
// honours, so no Restore item is handed up and the events go on — the one
// published while the client was away included — with no gap and no
// duplicate; and the command is resent under its SAME id, only once the
// re-attach is answered, and gets its stored answer: the host ran it once.
func TestAKilledConnectionResumesSilently(t *testing.T) {
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	h := newHost(t)
	c := dialClient(t, h.path, tp, remote.Options{})
	first := c.Hello()
	s := attach(t, c, remote.AttachOptions{})
	after := nextKind(t, s, remote.KindAttached).Reply.After
	nextKind(t, s, remote.KindSynchronized)

	hung := h.stub.HangNext()
	var pr protocol.PromptResult
	command(t, c, protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: h.sid(), Text: "running", Mode: protocol.PromptQueue}, &pr)
	await(t, hung, "the turn to open")
	if pr.Turn == "" {
		t.Fatalf("the prompt: %+v", pr)
	}

	held := losesReply(tp, protocol.MethodQueueAdd)
	type answer struct {
		id  string
		row protocol.QueueAddResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		var a answer
		a.id, a.err = c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "mid-turn"}, &a.row, remote.CommandOptions{})
		done <- a
	}()
	await(t, recv(t, held), "the client to redial")
	h.text("while away")
	tp.releaseDials()
	a := recv(t, done)
	if a.err != nil {
		t.Fatalf("the command: %v", a.err)
	}
	if q, err := agent.DecodeQueuedPrompt(a.row.Row); err != nil || q.Text != "mid-turn" {
		t.Fatalf("the resend's answer: %+v, %v", q, err)
	}
	h.text("after the resume")
	items := until(t, s, isText(t, "after the resume"))

	if hc := c.Hello(); !hc.Resumed || hc.ClientID != first.ClientID || hc.Token != first.Token {
		t.Fatalf("the reconnect's hello: %+v, want %s resumed with its token", hc, first.ClientID)
	}
	if n := kinds(items)[remote.KindRestore]; n != 0 {
		t.Fatalf("%d restore items: the resume was not silent", n)
	}
	contiguous(t, after.Seq+1, items)
	if !slices.ContainsFunc(items, isText(t, "while away")) {
		t.Fatal("the event published while the client was away never came")
	}
	if tp.connCount() != 2 {
		t.Fatalf("%d connections, want 2", tp.connCount())
	}
	// The re-attach carried the cursor: the last event handed up before the
	// kill, in the stream's incarnation.
	att := tp.sent(protocol.MethodSessionAttach)
	if len(att) != 2 || att[1].conn != 1 {
		t.Fatalf("%d attach requests", len(att))
	}
	re := attachParams(t, att[1])
	if re.Cursor == nil || re.Cursor.Incarnation != after.Incarnation || re.Cursor.Seq <= after.Seq {
		t.Fatalf("the re-attach's cursor: %+v (the first attach was after %+v)", re.Cursor, after)
	}
	// Sent twice under one id — the second after the re-attach is answered —
	// and run once.
	adds := tp.sent(protocol.MethodQueueAdd)
	if len(adds) != 2 || commandID(t, adds[0]) != a.id || commandID(t, adds[1]) != a.id || adds[1].conn != 1 {
		t.Fatalf("the queue.add requests: %d, want the same id %s twice", len(adds), a.id)
	}
	if n := queued(h, "mid-turn"); n != 1 {
		t.Fatalf("%d rows of the command's text: the host ran it %d times", n, n)
	}
	reattached := tp.received(func(l wireLine) bool { return l.conn == 1 && l.method == protocol.MethodSessionAttach })
	if len(reattached) != 1 || lineIndex(tp, adds[1]) < lineIndex(tp, reattached[0]) {
		t.Fatal("the command was resent before the re-attach was answered")
	}
}

// losesReply arms tp to lose the next reply to method: as it arrives, every
// later dial is held and the connection is killed, so the client never reads
// it. The channel carries the dial hold's "a redial has begun".
func losesReply(tp *tap, method string) <-chan (<-chan struct{}) {
	held := make(chan (<-chan struct{}), 1)
	var armed atomic.Bool
	armed.Store(true)
	tp.setRewriteIn(func(l wireLine) [][]byte {
		if l.method == method && l.resp != nil && armed.CompareAndSwap(true, false) {
			held <- tp.holdDials()
			tp.killConn(l.conn)
			return [][]byte{}
		}
		return nil
	})
	return held
}

// recv is the next value on ch.
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(watchdog):
		t.Fatalf("nothing in %s", watchdog)
		var zero T
		return zero
	}
}

// queued is how many queued rows of the engine's carry text.
func queued(h *host, text string) int {
	n := 0
	for _, q := range h.eng.State().Queue {
		if q.Text == text {
			n++
		}
	}
	return n
}

// lineIndex is l's place among the tap's lines.
func lineIndex(tp *tap, l wireLine) int {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	for i, x := range tp.lines {
		if x.conn == l.conn && x.out == l.out && string(x.raw) == string(l.raw) {
			return i
		}
	}
	return -1
}

// TestAResentCommandStillRunningIsWaitedOut (§3.14): a command still running
// on the host when its connection is killed — a SetTitle parked in the session
// index — is resent under its same id once the client has resumed; the host
// answers in_progress while the first execution runs, which the client waits
// out and resends, and the stored answer then comes back: one execution.
func TestAResentCommandStillRunningIsWaitedOut(t *testing.T) {
	store := newGateStore()
	tp := newTap(t)
	h := newHost(t, withIndex(engine.IndexOptions{Store: store, CWD: "/w", Provider: "cursor"}))
	t.Cleanup(store.open)
	c := dialClient(t, h.path, tp, remote.Options{})
	first := c.Hello()
	done := make(chan error, 1)
	var id atomic.Value
	go func() {
		cid, err := c.Command(tctx(t), protocol.MethodSessionSetTitle, protocol.SetTitleParams{SessionID: h.sid(), Title: "renamed"}, nil, remote.CommandOptions{})
		id.Store(cid)
		done <- err
	}()
	waitFor(t, "the SetTitle in the index", func() bool { return store.entered.Load() == 1 })
	tp.kill()
	tp.await(t, "the resend answered in_progress", func() bool {
		return len(tp.received(func(l wireLine) bool {
			return l.method == protocol.MethodSessionSetTitle && errorCode(l) == protocol.CodeInProgress
		})) > 0
	})
	store.open()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the command: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("the command never came back")
	}
	if hc := c.Hello(); !hc.Resumed || hc.ClientID != first.ClientID {
		t.Fatalf("the reconnect: %+v", hc)
	}
	sends := tp.sent(protocol.MethodSessionSetTitle)
	if len(sends) < 3 {
		t.Fatalf("%d sends, want the first, an in_progress resend and a resend after it", len(sends))
	}
	for _, l := range sends {
		if commandID(t, l) != id.Load().(string) {
			t.Fatalf("a resend under another id: %s", l.raw)
		}
	}
	if n := store.entered.Load(); n != 1 {
		t.Fatalf("the index was written %d times: the command ran again", n)
	}
	if got := h.eng.State().Title; got != "renamed" {
		t.Fatalf("the title: %q", got)
	}
}

// TestAClientProcessRestartResumesFromItsCursor (A4, the second leg): a NEW
// client — another Dial, as a restarted process would — built from the
// ResumeState a first client persisted is resumed: true as the same client,
// its attach with the persisted cursor is answered with no snapshot, after
// the cursor, and the events from afterSeq on come in order, the ones
// published while no client was attached included; and it goes on counting
// command ids where the first left off.
func TestAClientProcessRestartResumesFromItsCursor(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	c1 := dialClient(t, h.path, tp, remote.Options{})
	s1 := attach(t, c1, remote.AttachOptions{})
	nextKind(t, s1, remote.KindAttached)
	nextKind(t, s1, remote.KindSynchronized)
	command(t, c1, protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "before"}, nil)
	h.text("one")
	h.text("two")
	until(t, s1, isText(t, "two"))
	st := c1.ResumeState()
	if st.Cursor == nil || st.Cursor.Seq != h.head() || st.NextCommand != 2 || st.ClientID == "" || st.Token == "" {
		t.Fatalf("the persisted state: %+v (head %d)", st, h.head())
	}
	persisted, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	_ = c1.Close()
	h.text("three")
	h.text("four")

	var restored remote.ResumeState
	if err := json.Unmarshal(persisted, &restored); err != nil {
		t.Fatal(err)
	}
	c2 := dialClient(t, h.path, tp, remote.Options{Resume: &restored})
	if hc := c2.Hello(); !hc.Resumed || hc.ClientID != st.ClientID {
		t.Fatalf("the new process's hello: %+v, want %s resumed", hc, st.ClientID)
	}
	s2 := attach(t, c2, remote.AttachOptions{Cursor: restored.Cursor})
	r := nextKind(t, s2, remote.KindAttached).Reply
	if r.Snapshot != nil || r.Reset != "" || r.After != *st.Cursor {
		t.Fatalf("the attach from the cursor: %s", describe(remote.Item{Kind: remote.KindAttached, Reply: r}))
	}
	items := until(t, s2, ofKind(remote.KindSynchronized))
	if last := contiguous(t, st.Cursor.Seq+1, items); last != h.head() {
		t.Fatalf("replayed to %d, want %d", last, h.head())
	}
	var texts []string
	for _, it := range items {
		if it.Kind == remote.KindEvent {
			texts = append(texts, decode(t, it).Text)
		}
	}
	if !slices.Equal(texts, []string{"three", "four"}) {
		t.Fatalf("the events from afterSeq: %q", texts)
	}
	id := command(t, c2, protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "after"}, nil)
	if id != "2" {
		t.Fatalf("the restarted client's first command id is %s, want 2", id)
	}
	if n := queued(h, "after"); n != 1 {
		t.Fatalf("the restarted client's command: %d rows", n)
	}
}

// TestAResumeLostCommandIsNotResent (astra 13, the blocker; §3.14): command
// (c-1, 1) runs on the host and its reply is lost with the connection; the
// host releases the client and — the receipts clock moved past the table's
// age bound while the client's redial waits — retires it, so the reconnect's
// hello answers resumed: false with a fresh id. The command resolves
// ErrOutcomeUnknown, reason resume_lost, and is NOT resent: a resend under the
// fresh id would be a new command to the host, and the host ran it once.
func TestAResumeLostCommandIsNotResent(t *testing.T) {
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	h := newHost(t)
	c := dialClient(t, h.path, tp, remote.Options{})
	first := c.Hello()
	held := losesReply(tp, protocol.MethodSessionPrompt)
	done := make(chan error, 1)
	go func() {
		_, err := c.Command(tctx(t), protocol.MethodSessionPrompt,
			protocol.PromptParams{SessionID: h.sid(), Text: "once", Mode: protocol.PromptQueue}, nil, remote.CommandOptions{})
		done <- err
	}()
	await(t, recv(t, held), "the client to redial")
	// The host's reply went out before the kill, so its half-closed
	// connection is idle: the host closes it and releases the client, and
	// past the age bound retires it.
	h.logs.wait(t, "close client "+first.ClientID)
	h.advanceClock(pastRetirement)
	tp.releaseDials()

	outcomeUnknown(t, recv(t, done), protocol.ReasonResumeLost)
	if hc := c.Hello(); hc.Resumed || hc.ClientID == first.ClientID || hc.Token == first.Token {
		t.Fatalf("the reconnect's hello: %+v, want a fresh client", hc)
	}
	var hp protocol.HelloParams
	if hellos := tp.sent(protocol.MethodHello); len(hellos) != 2 || json.Unmarshal(hellos[1].params, &hp) != nil ||
		hp.Resume == nil || hp.Resume.ClientID != first.ClientID {
		t.Fatalf("the reconnect did not ask to resume %s", first.ClientID)
	}
	if n := len(tp.sent(protocol.MethodSessionPrompt)); n != 1 {
		t.Fatalf("the prompt was sent %d times: a resend after resumed: false", n)
	}
	if got := h.stub.Prompts(); !slices.Equal(got, []string{"once"}) {
		t.Fatalf("the host ran %q: a second execution", got)
	}
	// The client goes on under its fresh id.
	command(t, c, protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "fresh"}, nil)
}

// TestAClientWorksThroughASplice (A25; §3.3's hub splice): through a
// test-only proxy implementing exactly a hub's opening (hello, answered as a
// hub) and session.connect, then the splice, a client says hello to the host
// itself (client ids and tokens are the host's), attaches, prompts, and —
// its spliced connection killed — redials through the hub and resumes with
// the host that minted its token: resumed: true, a silent re-attach.
func TestAClientWorksThroughASplice(t *testing.T) {
	h := newHost(t)
	hb := newHub(t, h.path)
	tp := newTap(t)
	c := dialClient(t, hb.path, tp, remote.Options{Connect: h.sid()})
	first := c.Hello()
	if first.Endpoint.Kind != protocol.EndpointHost || first.Endpoint.HostID != h.srv.HostID() || first.ClientID == "" {
		t.Fatalf("the host's hello through the splice: %+v", first)
	}
	s := attach(t, c, remote.AttachOptions{})
	after := nextKind(t, s, remote.KindAttached).Reply.After
	nextKind(t, s, remote.KindSynchronized)
	var pr protocol.PromptResult
	command(t, c, protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: h.sid(), Text: "through the splice", Mode: protocol.PromptQueue}, &pr)
	if pr.Turn == "" || pr.Text == nil || *pr.Text != "through the splice" {
		t.Fatalf("the prompt: %+v", pr)
	}
	h.text("before the kill")
	items := until(t, s, isText(t, "before the kill"))

	hb.kill()
	h.text("after the kill")
	items = append(items, until(t, s, isText(t, "after the kill"))...)
	if hc := c.Hello(); !hc.Resumed || hc.ClientID != first.ClientID {
		t.Fatalf("the resume through the hub: %+v", hc)
	}
	if kinds(items)[remote.KindRestore] != 0 {
		t.Fatal("a restore: the resume through the hub was not silent")
	}
	contiguous(t, after.Seq+1, items)

	// Both connections opened with the hub hop, and the host's hello on the
	// second carried the token.
	hellos := tp.sent(protocol.MethodHello)
	connects := tp.sent(protocol.MethodSessionConnect)
	if len(hellos) != 4 || len(connects) != 2 {
		t.Fatalf("%d hellos and %d session.connects, want 4 and 2", len(hellos), len(connects))
	}
	var hp protocol.HelloParams
	for i, l := range hellos {
		if err := json.Unmarshal(l.params, &hp); err != nil {
			t.Fatal(err)
		}
		switch {
		case i%2 == 0 && hp.Resume != nil:
			t.Fatal("the hub's hello carried a resume: tokens are the host's")
		case i == 3 && (hp.Resume == nil || hp.Resume.ClientID != first.ClientID || hp.Resume.Token != first.Token):
			t.Fatalf("the host's hello on the redial: %+v", hp.Resume)
		}
	}
	hb.mu.Lock()
	defer hb.mu.Unlock()
	if !slices.Equal(hb.connect, []string{h.sid(), h.sid()}) {
		t.Fatalf("session.connect named %q", hb.connect)
	}
}

// TestRedialsAreBoundedThenDisconnected (§3.14): a lost connection whose
// redials all fail is redialled Options.Redials times within the window and
// no more; then a command still waiting — here a Set parked in the session
// when the connection went, so it may have run — resolves ErrOutcomeUnknown,
// reason disconnected; the stream hands up an Error item carrying
// ErrDisconnected; and the client has stopped.
func TestRedialsAreBoundedThenDisconnected(t *testing.T) {
	h := newHost(t)
	held, release := h.stub.HoldNextSet()
	t.Cleanup(release)
	tp := newTap(t)
	c := dialClient(t, h.path, tp, remote.Options{})
	s := attach(t, c, remote.AttachOptions{})
	nextKind(t, s, remote.KindAttached)
	nextKind(t, s, remote.KindSynchronized)
	done := make(chan error, 1)
	go func() {
		_, err := c.Command(tctx(t), protocol.MethodSessionSet, protocol.SetParams{SessionID: h.sid(),
			Setting: protocol.Setting{Kind: protocol.SettingModel, Value: "fast"}}, nil, remote.CommandOptions{})
		done <- err
	}()
	await(t, held, "the Set to park")
	tp.failDials(errors.New("the host is gone"))
	tp.kill()
	select {
	case err := <-done:
		outcomeUnknown(t, err, protocol.ReasonDisconnected)
	case <-time.After(watchdog):
		t.Fatal("the command never resolved")
	}
	it := until(t, s, ofKind(remote.KindError))
	if last := it[len(it)-1]; !errors.Is(last.Err, remote.ErrDisconnected) {
		t.Fatalf("the stream's last item: %s", describe(last))
	}
	await(t, c.Done(), "the client to stop")
	if !errors.Is(c.Err(), remote.ErrDisconnected) {
		t.Fatalf("the client stopped with %v", c.Err())
	}
	if n := tp.dialCount(); n != 1+3 {
		t.Fatalf("%d dials, want the first and 3 redials", n)
	}
	if _, err := c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "late"}, nil, remote.CommandOptions{}); !errors.Is(err, remote.ErrDisconnected) {
		t.Fatalf("a command after the client stopped: %v", err)
	}
}
