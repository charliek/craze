package remote_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// astra r61's schedules (C26a): a Session's commands and reads are bound to
// the client identity's number, never its client id's spelling; the adoption
// that moves the identity settles every command bound to the old one at once;
// a stream a decode failure ended ends the Session; Close beats a first
// attach still coming back; and the command counter never wraps.

// TestABindingIsTheIdentityNotItsSpelling (astra r61 2): a command made with
// no epoch in its ctx takes its binding at entry — the identity's number and
// its client id — and is paused before registration; the host restarts, and
// the replacement mints the SAME client id, "c-1", for a new identity. The
// command resolves outcome unknown (resume_lost, the stale epoch): not a byte
// of it on the new connection, and its id — one the old host already ran — is
// not run a second time on the new host.
func TestABindingIsTheIdentityNotItsSpelling(t *testing.T) {
	old, restarted := newHost(t), newHost(t)
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	var arm atomic.Bool
	paused, resume := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { closeOnce(resume) })
	s := dialSessionHooked(t, old.path, tp, remote.SessionOptions{}, remote.TestHooks{Entered: func(string) {
		if arm.CompareAndSwap(true, false) {
			close(paused)
			<-resume
		}
	}})
	if err := s.Start(tctx(t)); err != nil {
		t.Fatal(err)
	}
	readKind(t, s, backend.ItemRestore)
	c := engine.Command{Client: s.ClientID(), ID: "5"}
	if _, err := s.Submit(tctx(t), c, "once", engine.SubmitQueue, ""); err != nil {
		t.Fatalf("the first run of command 5: %v", err)
	}

	arm.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := s.Submit(tctx(t), c, "once", engine.SubmitQueue, "")
		done <- err
	}()
	await(t, paused, "the command to take its binding")
	// The host restarts: every later dial reaches the replacement, which does
	// not know the token and mints c-1 afresh.
	tp.redirect(restarted.path)
	tp.kill()
	items := readUntil(t, s, itemKind(backend.ItemRestore))
	if r := items[len(items)-1]; r.Info.Incarnation != restarted.eng.State().Incarnation {
		t.Fatalf("the restore after the restart: %+v", r.Info)
	}
	if s.ClientID() != c.Client || s.Epoch() == 1 {
		t.Fatalf("the premise: the replacement's client id %q (identity %d), want the same spelling %q under a new identity",
			s.ClientID(), s.Epoch(), c.Client)
	}
	close(resume)
	err := recv(t, done)
	outcomeUnknown(t, err, protocol.ReasonResumeLost)
	if !errors.Is(err, backend.ErrStaleEpoch) {
		t.Fatalf("not the stale epoch: %v", err)
	}
	if sent := tp.sentOn(1, protocol.MethodSessionPrompt); len(sent) != 0 {
		t.Fatalf("command 5 went out to the restarted host under its new identity: %s", sent[0].raw)
	}
	if got := restarted.stub.Prompts(); len(got) != 0 {
		t.Fatalf("the restarted host ran %q: command 5 a second time", got)
	}
	if got := old.stub.Prompts(); !slices.Equal(got, []string{"once"}) {
		t.Fatalf("the old host ran %q", got)
	}
}

// TestAdoptionSettlesEveryCommandBoundToTheOldIdentity (astra r61 3): a bound
// command registered while the client is disconnected — never sent — resolves
// resume_lost the moment the reconnect adopts a new identity, not once the
// re-attach is answered: here the replacement's engine is slow to start, so
// the re-attach waits (the episode's clock stopped), and the caller's
// deadline is long.
func TestAdoptionSettlesEveryCommandBoundToTheOldIdentity(t *testing.T) {
	old, slow := newHost(t), newHost(t, withoutStart())
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	s, _ := started(t, old, tp, remote.SessionOptions{})
	held := tp.holdDials()
	tp.kill()
	await(t, held, "the redial")
	done := make(chan error, 1)
	go func() {
		_, err := s.Submit(backend.WithEpoch(tctx(t), s.Epoch()), engine.Command{Client: s.ClientID(), ID: "3"}, "later",
			engine.SubmitQueue, "")
		done <- err
	}()
	waitFor(t, "the command to wait for a connection", func() bool { return s.Client().Waiting() == 1 })
	tp.redirect(slow.path)
	tp.releaseDials()
	err := recv(t, done)
	outcomeUnknown(t, err, protocol.ReasonResumeLost)
	if !errors.Is(err, backend.ErrStaleEpoch) {
		t.Fatalf("not the stale epoch: %v", err)
	}
	// Answered before the re-attach was: the replacement has not started.
	answered := tp.received(func(l wireLine) bool {
		return l.conn == 1 && l.method == protocol.MethodSessionAttach && l.resp != nil
	})
	if len(answered) != 0 {
		t.Fatal("the premise: the re-attach was answered before the command resolved")
	}
	if sent := tp.sent(protocol.MethodSessionPrompt); len(sent) != 0 {
		t.Fatalf("the bound command went out: %s", sent[0].raw)
	}
	if err := slow.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	readUntil(t, s, func(it backend.Item) bool { return it.Kind == backend.ItemRestore && it.Gen == 2 })
}

// TestADecodeEndedStreamEndsTheSession (astra r61 8): a stream that ends
// because an event does not decode (valid JSON, a numeric text) ends the
// Session as the stream's own End does: a Start waiting for readiness — an
// attach when: "now", the host not started — returns with the decode error;
// Read hands up the End, then backend.ErrClosed.
func TestADecodeEndedStreamEndsTheSession(t *testing.T) {
	h := newHost(t, withoutStart())
	tp := newTap(t)
	poisonEvents(t, tp)
	s := dialSession(t, h.path, tp, remote.SessionOptions{When: protocol.WhenNow})
	if err := s.Attach(tctx(t)); err != nil {
		t.Fatal(err)
	}
	readKind(t, s, backend.ItemRestore)
	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(context.Background()) }()
	h.text("poison")
	end := readKind(t, s, backend.ItemEnd)
	if end.Err == nil {
		t.Fatal("the End of an undecodable event carries no error")
	}
	if err := recv(t, startErr); !errors.Is(err, end.Err) {
		t.Fatalf("Start: %v, want the decode error %v", err, end.Err)
	}
	if _, err := s.Read(tctx(t)); !errors.Is(err, backend.ErrClosed) {
		t.Fatalf("a Read after the End: %v", err)
	}
}

// poisonEvents makes every event the host writes whose text is "poison" one
// the client cannot decode: valid JSON, a numeric text.
func poisonEvents(t *testing.T, tp *tap) {
	tp.setRewriteIn(func(l wireLine) [][]byte {
		if l.method != protocol.NotifyEvent || !bytes.Contains(l.raw, []byte("poison")) {
			return nil
		}
		var p protocol.EventParams
		if err := json.Unmarshal(l.params, &p); err != nil {
			t.Errorf("the event: %v", err)
			return nil
		}
		p.Event = json.RawMessage(`{"type":"text","text":5}`)
		b, err := json.Marshal(protocol.Notification{JSONRPC: protocol.JSONRPCVersion, Method: protocol.NotifyEvent, Params: mustJSON(t, p)})
		if err != nil {
			t.Errorf("the event: %v", err)
			return nil
		}
		return [][]byte{b}
	})
}

// TestAReadyAfterADecodeEndDoesNotStartTheSession (astra r62 3): a stream
// ended by an event the client cannot decode, then — its detach not yet made
// — the host's ready for it, received before Start looks: Start answers the
// end (the decode error), never the ready. readyLocked settles nothing once
// the Session has ended, and Start decides by the Session's state, whichever
// of its channels it sees.
func TestAReadyAfterADecodeEndDoesNotStartTheSession(t *testing.T) {
	h := newHost(t, withoutStart())
	tp := newTap(t)
	poisonEvents(t, tp)
	hold := make(chan struct{})
	s := dialSessionHooked(t, h.path, tp, remote.SessionOptions{When: protocol.WhenNow}, remote.TestHooks{Broken: func() { <-hold }})
	// Registered after the Session, so it runs before the Session's Close,
	// which joins the detach this holds.
	t.Cleanup(func() { closeOnce(hold) })
	if err := s.Attach(tctx(t)); err != nil {
		t.Fatal(err)
	}
	readKind(t, s, backend.ItemRestore)
	h.text("poison")
	end := readKind(t, s, backend.ItemEnd)
	if err := h.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tp.await(t, "the host's ready", func() bool {
		return len(tp.received(func(l wireLine) bool { return l.method == protocol.NotifyReady })) == 1
	})
	// A round trip on the same connection: its reply follows the ready on the
	// wire, so the client's reader has taken the ready by the time it answers.
	if err := s.Client().Call(tctx(t), protocol.MethodSessionSync, protocol.SyncParams{SessionID: h.sid()}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(tctx(t)); !errors.Is(err, end.Err) {
		t.Fatalf("Start after a decode end and a later ready: %v, want the decode error %v", err, end.Err)
	}
	if info := s.Info(); len(info.Models) != 0 {
		t.Fatalf("a document after the end moved Info: %+v", info)
	}
	closeOnce(hold)
}

// TestAdoptionWakesAReadWaitingForAConnection (astra r62 6): reads bound to
// identity E wait for a connection — one bounded by its deadline, one not at
// all — while the client reconnects; the reconnect adopts E+1, and the
// replacement's re-attach waits for its start, unanswered, the episode's clock
// stopped. Both reads answer backend.ErrStaleEpoch at the adoption, before the
// re-attach is answered, and neither goes out.
func TestAdoptionWakesAReadWaitingForAConnection(t *testing.T) {
	old, slow := newHost(t), newHost(t, withoutStart())
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	waiting := make(chan struct{}, 2)
	s := dialSessionHooked(t, old.path, tp, remote.SessionOptions{}, remote.TestHooks{Waiting: func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}})
	if err := s.Start(tctx(t)); err != nil {
		t.Fatal(err)
	}
	readKind(t, s, backend.ItemRestore)
	epoch := s.Epoch()
	held := tp.holdDials()
	tp.kill()
	await(t, held, "the redial")
	bounded, unbounded := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := s.Settings(backend.WithEpoch(tctx(t), epoch))
		bounded <- err
	}()
	go func() {
		_, _, err := s.Ask(backend.WithEpoch(context.Background(), epoch), "perm-1")
		unbounded <- err
	}()
	await(t, waiting, "a read to wait for a connection")
	await(t, waiting, "the other read to wait for a connection")
	tp.redirect(slow.path)
	tp.releaseDials()
	for name, ch := range map[string]chan error{"the bounded read": bounded, "the unbounded read": unbounded} {
		if err := recv(t, ch); !errors.Is(err, backend.ErrStaleEpoch) {
			t.Fatalf("%s across the adoption: %v, want backend.ErrStaleEpoch", name, err)
		}
	}
	if answered := tp.received(func(l wireLine) bool {
		return l.conn == 1 && l.method == protocol.MethodSessionAttach && l.resp != nil
	}); len(answered) != 0 {
		t.Fatal("the premise: the re-attach was answered before the reads were")
	}
	for _, m := range []string{protocol.MethodSessionState, protocol.MethodAsksGet} {
		if sent := tp.sentOn(1, m); len(sent) != 0 {
			t.Fatalf("%s went out under the new identity", m)
		}
	}
	if err := slow.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestAReadsAnswerIsJudgedAgainstItsBinding (astra r62, new): a read made on
// identity E whose answer has reached its caller — held there, not yet
// decoded — while the connection goes and the reconnect adopts E+1, answers
// backend.ErrStaleEpoch: never E's settings, or E's "no such ask", handed to a
// caller bound elsewhere now.
func TestAReadsAnswerIsJudgedAgainstItsBinding(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	var arm atomic.Bool
	var reached sync.WaitGroup
	reached.Add(2)
	resume := make(chan struct{})
	t.Cleanup(func() { closeOnce(resume) })
	s := dialSessionHooked(t, h.path, tp, remote.SessionOptions{}, remote.TestHooks{Answered: func(m string) {
		if arm.Load() && (m == protocol.MethodSessionState || m == protocol.MethodAsksGet) {
			reached.Done()
			<-resume
		}
	}})
	if err := s.Start(tctx(t)); err != nil {
		t.Fatal(err)
	}
	readKind(t, s, backend.ItemRestore)
	epoch := s.Epoch()
	arm.Store(true)
	settings, ask := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := s.Settings(backend.WithEpoch(tctx(t), epoch))
		settings <- err
	}()
	go func() {
		_, _, err := s.Ask(backend.WithEpoch(tctx(t), epoch), "no-such-ask")
		ask <- err
	}()
	answers := make(chan struct{})
	go func() { reached.Wait(); close(answers) }()
	await(t, answers, "both answers to reach their callers")
	arm.Store(false)
	retire(t, h, tp, s)()
	waitFor(t, "the adoption of a new identity", func() bool { return s.Epoch() != epoch })
	close(resume)
	for name, ch := range map[string]chan error{"Settings": settings, "Ask": ask} {
		if err := recv(t, ch); !errors.Is(err, backend.ErrStaleEpoch) {
			t.Fatalf("%s answered on E, returned after E+1: %v, want backend.ErrStaleEpoch", name, err)
		}
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCloseBeatsTheFirstAttach (astra r61 9): the client's first attach has
// come back — its snapshot queued — but the Session does not hold the stream
// yet when Close runs and returns. The Attach then detaches the stream it
// made and answers backend.ErrClosed, and Read hands up nothing of it:
// backend.ErrClosed.
func TestCloseBeatsTheFirstAttach(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	returned, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { closeOnce(release) })
	s := dialSessionHooked(t, h.path, tp, remote.SessionOptions{}, remote.TestHooks{Attached: func() {
		close(returned)
		<-release
	}})
	attached := make(chan error, 1)
	go func() { attached <- s.Attach(tctx(t)) }()
	await(t, returned, "the client's attach to come back")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(release)
	if err := recv(t, attached); !errors.Is(err, backend.ErrClosed) {
		t.Fatalf("an Attach Close beat: %v", err)
	}
	for range 2 {
		if it, err := s.Read(tctx(t)); !errors.Is(err, backend.ErrClosed) {
			t.Fatalf("a Read after Close: %s, %v", describeItem(it), err)
		}
	}
}

// TestAReadNeverGoesOutUnderAnotherIdentity (astra r61 11): a read that passed
// its epoch check waits for a connection while the client reconnects; the
// host answers resumed: false (a new identity). The read is
// backend.ErrStaleEpoch — plain, as in process, not an outcome unknown — and
// not a byte of it is written on the new identity's connection.
func TestAReadNeverGoesOutUnderAnotherIdentity(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	waiting := make(chan struct{}, 2)
	s := dialSessionHooked(t, h.path, tp, remote.SessionOptions{}, remote.TestHooks{Waiting: func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}})
	if err := s.Start(tctx(t)); err != nil {
		t.Fatal(err)
	}
	readKind(t, s, backend.ItemRestore)
	epoch := s.Epoch()
	release := retire(t, h, tp, s)
	settings, ask := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := s.Settings(backend.WithEpoch(tctx(t), epoch))
		settings <- err
	}()
	go func() {
		_, _, err := s.Ask(backend.WithEpoch(tctx(t), epoch), "perm-1")
		ask <- err
	}()
	await(t, waiting, "a read to wait for a connection")
	await(t, waiting, "the other read to wait for a connection")
	release()
	for name, ch := range map[string]chan error{"Settings": settings, "Ask": ask} {
		if err := recv(t, ch); !errors.Is(err, backend.ErrStaleEpoch) || errors.Is(err, backend.ErrOutcomeUnknown) {
			t.Fatalf("%s across the loss: %v, want plain backend.ErrStaleEpoch", name, err)
		}
	}
	for _, m := range []string{protocol.MethodSessionState, protocol.MethodAsksGet} {
		if sent := tp.sentOn(1, m); len(sent) != 0 {
			t.Fatalf("%s went out under the new identity: %s", m, sent[0].raw)
		}
	}
}

// TestTheCommandCounterNeverWraps (astra r61, further): a caller's id is
// refused when the counter would reach math.MaxUint64 after it — the largest
// taken leaves the next minted id nowhere to go, which is refused too, nothing
// sent — and a counter resumed near its end mints up to the last id and then
// refuses; ResumeState's NextCommand is never below an id sent.
func TestTheCommandCounterNeverWraps(t *testing.T) {
	last := uint64(math.MaxUint64 - 2)
	t.Run("a caller's id", func(t *testing.T) {
		h := newHost(t)
		tp := newTap(t)
		s, _ := started(t, h, tp, remote.SessionOptions{})
		for _, n := range []uint64{last + 1, last + 2} {
			id := strconv.FormatUint(n, 10)
			if err := s.Disarm(tctx(t), engine.Command{Client: s.ClientID(), ID: id}); !errors.Is(err, remote.ErrCommandID) {
				t.Fatalf("the caller's id %s: %v", id, err)
			}
		}
		if n := len(tp.sent(protocol.MethodSessionDisarm)); n != 0 {
			t.Fatalf("%d refused ids went out", n)
		}
		id := strconv.FormatUint(last, 10)
		if err := s.SetTitle(tctx(t), engine.Command{Client: s.ClientID(), ID: id}, "the last"); err != nil {
			t.Fatalf("the caller's id %s: %v", id, err)
		}
		if n := s.Client().ResumeState().NextCommand; n != last+1 {
			t.Fatalf("NextCommand after %s: %d", id, n)
		}
		if _, err := s.Client().Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "x"},
			nil, remote.CommandOptions{}); !errors.Is(err, remote.ErrCommandIDsSpent) {
			t.Fatalf("a minted command past the caller's last: %v", err)
		}
		if n := len(tp.sent(protocol.MethodQueueAdd)); n != 0 || s.Client().ResumeState().NextCommand != last+1 {
			t.Fatalf("%d minted commands went out; NextCommand %d", n, s.Client().ResumeState().NextCommand)
		}
	})
	t.Run("a minted id", func(t *testing.T) {
		h := newHost(t)
		tp := newTap(t)
		c := dialClient(t, h.path, tp, remote.Options{Resume: &remote.ResumeState{NextCommand: last}})
		add := func() (string, error) {
			return c.Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "x"}, nil, remote.CommandOptions{})
		}
		if id, err := add(); err != nil || id != strconv.FormatUint(last, 10) {
			t.Fatalf("the last minted id: %q, %v", id, err)
		}
		if _, err := add(); !errors.Is(err, remote.ErrCommandIDsSpent) {
			t.Fatalf("a mint past the last: %v", err)
		}
		if n := len(tp.sent(protocol.MethodQueueAdd)); n != 1 || c.ResumeState().NextCommand != last+1 {
			t.Fatalf("%d sends; NextCommand %d", n, c.ResumeState().NextCommand)
		}
	})
}
