package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// remote.Session, the TUI's backend over the wire (plan 027 §3.14, PR 4),
// against the real server in front of an engine over the Stub.

// dialSession dials a Session to path through tp, and closes it when the test
// ends (before the host's own cleanup, which was registered first).
func dialSession(t *testing.T, path string, tp *tap, o remote.SessionOptions) *remote.Session {
	t.Helper()
	s, err := tryDialSession(t, path, tp, o)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return s
}

func tryDialSession(t *testing.T, path string, tp *tap, o remote.SessionOptions) (*remote.Session, error) {
	t.Helper()
	return tryDialSessionHooked(t, path, tp, o, remote.TestHooks{})
}

// dialSessionHooked is dialSession with the client's schedule hooks h.
func dialSessionHooked(t *testing.T, path string, tp *tap, o remote.SessionOptions, h remote.TestHooks) *remote.Session {
	t.Helper()
	s, err := tryDialSessionHooked(t, path, tp, o, h)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return s
}

func tryDialSessionHooked(t *testing.T, path string, tp *tap, o remote.SessionOptions, h remote.TestHooks) (*remote.Session, error) {
	t.Helper()
	o.Client.Dial = tp.dial
	if o.Client.Client.Kind == "" {
		o.Client.Client = protocol.ClientInfo{Kind: "test", Name: "remote_test"}
	}
	if o.Client.RedialBackoff == 0 {
		o.Client.RedialBackoff = 5 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	s, err := remote.DialSessionForTest(ctx, path, o, h)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, nil
}

// started is a Session to h, started (attached when: "ready"), its first item
// — the first attach's Restore — read.
func started(t *testing.T, h *host, tp *tap, o remote.SessionOptions) (*remote.Session, backend.Item) {
	t.Helper()
	s := dialSession(t, h.path, tp, o)
	if err := s.Start(tctx(t)); err != nil {
		t.Fatalf("start: %v", err)
	}
	return s, readKind(t, s, backend.ItemRestore)
}

// readItem is the Session's next item.
func readItem(t *testing.T, s *remote.Session) backend.Item {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	it, err := s.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return it
}

// readKind is the Session's next item, which must be of kind k.
func readKind(t *testing.T, s *remote.Session, k backend.ItemKind) backend.Item {
	t.Helper()
	it := readItem(t, s)
	if it.Kind != k {
		t.Fatalf("want item kind %d; got %s", k, describeItem(it))
	}
	return it
}

// readUntil reads items up to and including the first for which stop holds.
func readUntil(t *testing.T, s *remote.Session, stop func(backend.Item) bool) []backend.Item {
	t.Helper()
	var items []backend.Item
	for {
		it := readItem(t, s)
		items = append(items, it)
		if stop(it) {
			return items
		}
		if it.Kind == backend.ItemEnd {
			t.Fatalf("the stream ended before the item waited for: %s", describeItem(it))
		}
	}
}

func itemKind(k backend.ItemKind) func(backend.Item) bool {
	return func(it backend.Item) bool { return it.Kind == k }
}

// textItem says it is a main-session text event carrying s.
func textItem(s string) func(backend.Item) bool {
	return func(it backend.Item) bool {
		return it.Kind == backend.ItemEvent && it.Event.Type == agent.EventText && it.Event.Text == s
	}
}

func describeItem(it backend.Item) string {
	switch it.Kind {
	case backend.ItemEvent:
		return "event " + string(it.Event.Type) + " " + it.Event.Text
	case backend.ItemReady:
		return "ready"
	case backend.ItemRestore:
		return "restore"
	case backend.ItemEnd:
		if it.Err != nil {
			return "end: " + it.Err.Error()
		}
		return "end"
	}
	return "an item of no kind"
}

// rewriteAttachReplies rewrites every attach reply's session document the
// host writes with edit — what the client reads in its place.
func rewriteAttachReplies(t *testing.T, tp *tap, edit func(*protocol.SessionInfo)) {
	tp.setRewriteIn(func(l wireLine) [][]byte {
		if l.method != protocol.MethodSessionAttach || l.resp == nil || l.resp.Error != nil {
			return nil
		}
		var res protocol.AttachResult
		if err := json.Unmarshal(l.resp.Result, &res); err != nil {
			t.Errorf("the attach reply: %v", err)
			return nil
		}
		edit(&res.Session)
		raw, err := json.Marshal(res)
		if err != nil {
			t.Errorf("the attach reply: %v", err)
			return nil
		}
		b, err := json.Marshal(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(l.id), Result: raw})
		if err != nil {
			t.Errorf("the attach reply: %v", err)
			return nil
		}
		return [][]byte{b}
	})
}

// TestInfoBeforeAttachIsTheFallback (GLM 11; §3.15's last bullet): before
// any attach reply, Info is the options' fallback — the registry entry's
// provider name and workspace, and the label and capabilities the options
// carry or, where they carry none, this binary's provider table's for the
// name (the in-process path's own read before Start) — with nothing asked of
// the host; once the attach reply is read, it is the host's, whatever the
// fallback said.
func TestInfoBeforeAttachIsTheFallback(t *testing.T) {
	h := newHost(t)
	h.stub.SetProvider(agent.CursorProvider())
	tp := newTap(t)
	s := dialSession(t, h.path, tp, remote.SessionOptions{SessionID: h.sid(), Provider: "grok", Workspace: "/elsewhere"})
	grok := agent.ProviderInfo{Name: "grok"}
	hello := s.Client().Hello()
	info := s.Info()
	switch {
	case info.Provider != "grok" || info.Label != grok.Label() || info.Capabilities != grok.Capabilities():
		t.Fatalf("the fallback's provider: %+v", info)
	case info.Workspace != "/elsewhere" || info.CrazeSessionID != h.sid() || info.ProviderSessionID != "" || info.Incarnation != "":
		t.Fatalf("the fallback's identities: %+v", info)
	case len(info.Models) != 0 || len(info.Modes) != 0:
		t.Fatalf("the fallback has catalogs: %+v", info)
	case info.RetryHorizon.Commands != hello.RetryHorizon.Commands || info.RetryHorizon.Age.Milliseconds() != hello.RetryHorizon.AgeMs:
		t.Fatalf("the fallback's retry horizon %+v, hello's %+v", info.RetryHorizon, hello.RetryHorizon)
	}
	if n := len(tp.sent(protocol.MethodSessionAttach)) + len(tp.sent(protocol.MethodSessionsList)); n != 0 {
		t.Fatalf("Info asked the host %d times", n)
	}

	// The options' own label and capabilities are the fallback's.
	own := agent.Capabilities{Todos: true, SubagentCancel: true}
	o := dialSession(t, h.path, newTap(t), remote.SessionOptions{Provider: "grok", Label: "Grok (the entry's)", Capabilities: &own})
	if info := o.Info(); info.Label != "Grok (the entry's)" || info.Capabilities != own {
		t.Fatalf("the options' own fallback: %+v", info)
	}

	// Once attached: the host's.
	if err := s.Start(tctx(t)); err != nil {
		t.Fatal(err)
	}
	cursor := agent.CursorProvider()
	info = s.Info()
	st := h.eng.State()
	switch {
	case info.Provider != "cursor" || info.Label != cursor.DisplayName() || info.Capabilities != cursor.Capabilities():
		t.Fatalf("the host's provider after the attach: %+v", info)
	case info.Workspace != "/work" || info.Incarnation != st.Incarnation || info.ProviderSessionID != st.SessionID:
		t.Fatalf("the host's identities after the attach: %+v", info)
	case len(info.Models) != len(st.Models) || len(info.Modes) != len(st.Modes):
		t.Fatalf("the host's catalogs after the attach: %+v", info)
	}
}

// TestInfoIsTheHostsAsSent (GLM 7, astra 25; §3.13): Info's capabilities
// are the host's as it sent them — here a grok host whose document says
// interject: false, todos: false and subagentCancel: true, none of which this
// binary's grok row says — on the Restore item and in Info alike, never
// rebuilt from this binary's provider table.
func TestInfoIsTheHostsAsSent(t *testing.T) {
	h := newHost(t)
	h.stub.SetProvider(agent.GrokProvider())
	tp := newTap(t)
	rewriteAttachReplies(t, tp, func(si *protocol.SessionInfo) {
		si.Capabilities.Interject = false
		si.Capabilities.Todos = false
		si.Capabilities.SubagentCancel = true
		si.Provider.Label = "Grok, as the host says"
	})
	table := agent.GrokProvider().Capabilities()
	if !table.Interject || !table.Todos || table.SubagentCancel {
		t.Fatalf("the premise: this binary's grok row is %+v", table)
	}
	want := table
	want.Interject, want.Todos, want.SubagentCancel = false, false, true

	s, restore := started(t, h, tp, remote.SessionOptions{Provider: "grok"})
	if restore.Info.Capabilities != want || restore.Info.Label != "Grok, as the host says" {
		t.Fatalf("the Restore's info: %+v, want the host's capabilities %+v", restore.Info, want)
	}
	if info := s.Info(); info.Capabilities != want || info.Provider != "grok" || info.Label != "Grok, as the host says" {
		t.Fatalf("Info: %+v, want the host's capabilities %+v", info, want)
	}
}

// TestReadyUpdatesInfoAndStartReturns (§3.4, §3.16 steps 3–4): an attach
// made when: "now" before the host starts its engine gets the document as it
// stands — no catalogs yet — then the start's events live, then Ready, whose
// document has the final catalogs and provider session id; Start returns once
// the ready is received — with no reader, and with one reading concurrently —
// and Info is the Ready's document by then.
func TestReadyUpdatesInfoAndStartReturns(t *testing.T) {
	for _, reader := range []bool{false, true} {
		name := "with no reader"
		if reader {
			name = "with a reader"
		}
		t.Run(name, func(t *testing.T) {
			h := newHost(t, withoutStart())
			h.stub.Replay = []agent.Event{{Type: agent.EventText, Text: "replayed one"}, {Type: agent.EventText, Text: "replayed two"}}
			tp := newTap(t)
			s := dialSession(t, h.path, tp, remote.SessionOptions{When: protocol.WhenNow})
			if err := s.Attach(tctx(t)); err != nil {
				t.Fatal(err)
			}
			if p := attachParams(t, tp.sent(protocol.MethodSessionAttach)[0]); p.When != protocol.WhenNow || p.Cursor != nil {
				t.Fatalf("the attach: %+v", p)
			}
			first := readKind(t, s, backend.ItemRestore)
			if first.Gen != 1 || len(first.Info.Models) != 0 || len(first.Info.Modes) != 0 {
				t.Fatalf("the first attach, before the start: gen %d, %+v", first.Gen, first.Info)
			}
			if info := s.Info(); len(info.Models) != 0 {
				t.Fatalf("Info before the start: %+v", info)
			}
			var items []backend.Item
			var readerDone chan struct{}
			if reader {
				readerDone = make(chan struct{})
				go func() {
					defer close(readerDone)
					ctx, cancel := context.WithTimeout(context.Background(), watchdog)
					defer cancel()
					for {
						it, err := s.Read(ctx)
						if err != nil {
							t.Errorf("the reader: %v", err)
							return
						}
						items = append(items, it)
						if it.Kind == backend.ItemReady {
							return
						}
					}
				}()
			}
			startErr := make(chan error, 1)
			go func() { startErr <- s.Start(tctx(t)) }()
			select {
			case err := <-startErr:
				t.Fatalf("Start returned before the host's start: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			if err := h.eng.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := recv(t, startErr); err != nil {
				t.Fatalf("Start: %v", err)
			}
			st := h.eng.State()
			info := s.Info()
			if len(info.Models) != len(st.Models) || len(info.Modes) != len(st.Modes) || info.ProviderSessionID != st.SessionID ||
				len(info.Models) == 0 {
				t.Fatalf("Info once Start returned: %+v, want the ready's catalogs and provider session id (%d models, %q)",
					info, len(st.Models), st.SessionID)
			}
			if reader {
				await(t, readerDone, "the reader")
			} else {
				items = readUntil(t, s, itemKind(backend.ItemReady))
			}
			var texts []string
			for _, it := range items[:len(items)-1] {
				if it.Kind != backend.ItemEvent || it.Gen != 1 {
					t.Fatalf("before the ready: %s (gen %d)", describeItem(it), it.Gen)
				}
				if it.Event.Type == agent.EventText {
					texts = append(texts, it.Event.Text)
				}
			}
			if !slices.Equal(texts, []string{"replayed one", "replayed two"}) {
				t.Fatalf("the start's events, live: %q", texts)
			}
			ready := items[len(items)-1]
			if ready.Err != nil || len(ready.Info.Models) != len(st.Models) || ready.Info.ProviderSessionID != st.SessionID {
				t.Fatalf("the Ready: %+v, err %v", ready.Info, ready.Err)
			}
		})
	}
}

// TestAStartFailureIsStartsError (§3.4; X16 1): the host's start failing is
// Start's error, whose text is the host's start error verbatim — told by the
// ready notification to an attach made before the start (and handed up as the
// Ready item's Err), or by the attach's own refusal (not_accepting,
// start_failed, its cause) to one made after.
func TestAStartFailureIsStartsError(t *testing.T) {
	const why = "craze: cursor-agent exited: exit status 1"
	t.Run("told by the ready", func(t *testing.T) {
		h := newHost(t, withoutStart())
		s := dialSession(t, h.path, newTap(t), remote.SessionOptions{When: protocol.WhenNow})
		if err := s.Attach(tctx(t)); err != nil {
			t.Fatal(err)
		}
		readKind(t, s, backend.ItemRestore)
		startErr := make(chan error, 1)
		go func() { startErr <- s.Start(tctx(t)) }()
		h.eng.Started(errors.New(why))
		err := recv(t, startErr)
		var se *remote.StartError
		if !errors.As(err, &se) || err.Error() != why {
			t.Fatalf("Start: %v, want the host's start error %q", err, why)
		}
		ready := readUntil(t, s, itemKind(backend.ItemReady))
		if r := ready[len(ready)-1]; r.Err == nil || r.Err.Error() != why {
			t.Fatalf("the Ready's Err: %v", r.Err)
		}
	})
	t.Run("told by the attach", func(t *testing.T) {
		h := newHost(t, withoutStart())
		h.eng.Started(errors.New(why))
		s := dialSession(t, h.path, newTap(t), remote.SessionOptions{})
		err := s.Start(tctx(t))
		var se *remote.StartError
		if !errors.As(err, &se) || err.Error() != why {
			t.Fatalf("Start: %v, want the host's start error %q", err, why)
		}
	})
}

// TestTheFirstAttachIsARestore (§3.14; X21): the first attach — no cursor, so
// it carries a snapshot — is Read's first item, a Restore of generation 1
// with the reply's document and the decoded snapshot at the host's head; the
// events after it are decoded, seq and all, and carry generation 1.
func TestTheFirstAttachIsARestore(t *testing.T) {
	h := newHost(t)
	h.text("before")
	head := h.head()
	tp := newTap(t)
	s, first := started(t, h, tp, remote.SessionOptions{})
	switch {
	case first.Gen != 1 || first.Snapshot == nil:
		t.Fatalf("the first attach: gen %d, snapshot %v", first.Gen, first.Snapshot != nil)
	case first.Snapshot.Seq != head || first.Snapshot.Incarnation != h.eng.State().Incarnation:
		t.Fatalf("the snapshot is at %s/%d, the host at %d", first.Snapshot.Incarnation, first.Snapshot.Seq, head)
	case first.Info.CrazeSessionID != h.sid() || first.Info.Incarnation != first.Snapshot.Incarnation ||
		s.Info().Incarnation != first.Info.Incarnation:
		t.Fatalf("the Restore's info: %+v", first.Info)
	}
	h.text("after")
	it := readItem(t, s)
	if !textItem("after")(it) || it.Gen != 1 || it.Event.Seq != head+1 {
		t.Fatalf("the event after the attach: %s, gen %d, seq %d (head was %d)", describeItem(it), it.Gen, it.Event.Seq, head)
	}
}

// TestARestoreMovesTheStreamGeneration (§3.14, CodeRabbit 6): a Restore — here
// an omitted record's re-attach, a snapshot past it (one or two, X22) — moves
// the stream generation by one each, and every event carries the generation
// of the Restore before it; a same-identity Restore moves no epoch.
func TestARestoreMovesTheStreamGeneration(t *testing.T) {
	h := newHost(t, withLog(agent.EventLogOptions{MaxRecordBytes: 4 << 10}))
	tp := newTap(t)
	s, first := started(t, h, tp, remote.SessionOptions{})
	epoch := s.Epoch()
	h.text("small")
	h.text(big)
	items := append([]backend.Item{first}, readUntil(t, s, itemKind(backend.ItemRestore))...)
	// An event after the omission: published once the Restore is read, and
	// again if a second Restore (X22) took it into its snapshot.
	for i := 0; ; i++ {
		if i == 3 {
			t.Fatal("no event came after the omission's Restores")
		}
		text := fmt.Sprintf("after %d", i)
		h.text(text)
		more := readUntil(t, s, func(it backend.Item) bool { return textItem(text)(it) || it.Kind == backend.ItemRestore })
		items = append(items, more...)
		if more[len(more)-1].Kind == backend.ItemEvent {
			break
		}
	}
	gen, restores := uint64(0), 0
	for _, it := range items {
		switch it.Kind {
		case backend.ItemRestore:
			if it.Gen != gen+1 {
				t.Fatalf("a Restore of generation %d after %d", it.Gen, gen)
			}
			gen = it.Gen
			restores++
		case backend.ItemEvent:
			if it.Gen != gen {
				t.Fatalf("%s carries generation %d under the Restore of %d", describeItem(it), it.Gen, gen)
			}
		}
	}
	if restores < 2 || restores > 3 {
		t.Fatalf("%d Restores: the first attach's and one or two for the omission", restores)
	}
	if last := items[len(items)-1]; last.Gen != gen || gen < 2 {
		t.Fatalf("the event after the omission: gen %d, the last Restore's %d", last.Gen, gen)
	}
	if s.Epoch() != epoch {
		t.Fatalf("a same-identity Restore moved the epoch %d → %d", epoch, s.Epoch())
	}
}

// retire drops the Session's connection with its redial held, and lets the
// host release and retire its client: the reconnect's hello is then answered
// resumed: false with a fresh id — a resume loss in the same engine
// incarnation. It returns the release of the redial.
func retire(t *testing.T, h *host, tp *tap, s *remote.Session) func() {
	t.Helper()
	id := s.ClientID()
	held := tp.holdDials()
	tp.kill()
	await(t, held, "the redial")
	hostReleases(t, h, id)
	h.advanceClock(pastRetirement)
	return tp.releaseDials
}

// hostReleases waits until the host has closed client id's connection and
// released it. A peer's full close is noticed only by a write that fails
// (X12 13), so an attached, idle session is given writes — text events on
// its stream — until the host has noticed.
func hostReleases(t *testing.T, h *host, id string) {
	t.Helper()
	deadline := time.Now().Add(watchdog)
	for !h.logs.has("close client " + id) {
		if time.Now().After(deadline) {
			t.Fatalf("the host did not release %s in %s", id, watchdog)
		}
		h.text("while away")
		time.Sleep(5 * time.Millisecond)
	}
}

// has says a line containing sub has been logged.
func (l *logSink) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.ContainsFunc(l.lines, func(line string) bool { return strings.Contains(line, sub) })
}

// TestAResumeLossMovesTheEpochBeforeItsRestore (§3.12 "Chains are fenced in the
// backend too", astra r3 13; X43 6): a reconnect the host answers resumed:
// false — here the client retired, the SAME engine and incarnation — moves the
// epoch (the client's identity) before the re-attach is even written, so
// before its Restore can be handed up; ClientID is the fresh id, read per call.
func TestAResumeLossMovesTheEpochBeforeItsRestore(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	s, first := started(t, h, tp, remote.SessionOptions{})
	epoch, client := s.Epoch(), s.ClientID()
	var mu sync.Mutex
	var atReattach []uint64
	tp.setHoldOut(func(l wireLine) <-chan struct{} {
		if isAttach(l) && l.conn > 0 {
			mu.Lock()
			atReattach = append(atReattach, s.Epoch())
			mu.Unlock()
		}
		return nil
	})
	retire(t, h, tp, s)()
	restore := readKind(t, s, backend.ItemRestore)
	mu.Lock()
	seen := slices.Clone(atReattach)
	mu.Unlock()
	switch {
	case len(seen) == 0 || seen[0] == epoch:
		t.Fatalf("the epoch as the re-attach was written: %v, was %d: it did not move before the Restore", seen, epoch)
	case s.Epoch() != seen[0]:
		t.Fatalf("the epoch moved again: %d, then %d", seen[0], s.Epoch())
	case restore.Gen != first.Gen+1 || restore.Info.Incarnation != first.Info.Incarnation:
		t.Fatalf("the Restore: gen %d, incarnation %q (was %q): the premise is a same-incarnation loss",
			restore.Gen, restore.Info.Incarnation, first.Info.Incarnation)
	case s.ClientID() == client || s.ClientID() != s.Client().Hello().ClientID:
		t.Fatalf("ClientID after the loss: %q (was %q)", s.ClientID(), client)
	}
}

// TestACommandIsNeverSentUnderAnotherIdentity (astra 13, astra 6; §3.14): a
// command is sent only under the client identity its engine.Command names.
// One waiting for a connection across a resume loss — bound by its ctx's epoch,
// or by its Command's client id alone — resolves outcome unknown, reason
// resume_lost, the stale epoch: never a success, and not a byte of it on the
// new connection (the tap); one whose reply the loss took is never resent: one
// execution, and outcome unknown.
func TestACommandIsNeverSentUnderAnotherIdentity(t *testing.T) {
	t.Run("waiting for a connection", func(t *testing.T) {
		h := newHost(t)
		tp := newTap(t)
		t.Cleanup(tp.releaseDials)
		s, _ := started(t, h, tp, remote.SessionOptions{})
		c1 := engine.Command{Client: s.ClientID(), ID: "7"}
		c2 := engine.Command{Client: s.ClientID(), ID: "8"}
		release := retire(t, h, tp, s)
		type answer struct {
			res engine.SubmitResult
			err error
		}
		done := make(chan answer, 2)
		go func() {
			var a answer
			a.res, a.err = s.Submit(backend.WithEpoch(tctx(t), s.Epoch()), c1, "by its epoch", engine.SubmitQueue, "")
			done <- a
		}()
		go func() {
			var a answer
			a.res, a.err = s.Submit(tctx(t), c2, "by its client id", engine.SubmitQueue, "")
			done <- a
		}()
		waitFor(t, "the commands to wait for a connection", func() bool { return s.Client().Waiting() == 2 })
		release()
		for range 2 {
			a := recv(t, done)
			outcomeUnknown(t, a.err, protocol.ReasonResumeLost)
			if !errors.Is(a.err, backend.ErrStaleEpoch) || !errors.Is(a.err, backend.ErrOutcomeUnknown) || a.res != (engine.SubmitResult{}) {
				t.Fatalf("a command across the loss: %+v, %v", a.res, a.err)
			}
		}
		if sent := tp.sent(protocol.MethodSessionPrompt); len(sent) != 0 {
			t.Fatalf("%d prompts went out, the first on connection %d: under another identity", len(sent), sent[0].conn)
		}
		if got := h.stub.Prompts(); len(got) != 0 {
			t.Fatalf("the host ran %q", got)
		}
		// The Session goes on under its new identity.
		if _, err := s.Submit(backend.WithEpoch(tctx(t), s.Epoch()), engine.Command{Client: s.ClientID(), ID: "9"}, "fresh", engine.SubmitQueue, ""); err != nil {
			t.Fatalf("a command under the new identity: %v", err)
		}
	})
	t.Run("its reply lost", func(t *testing.T) {
		h := newHost(t)
		tp := newTap(t)
		t.Cleanup(tp.releaseDials)
		s, _ := started(t, h, tp, remote.SessionOptions{})
		c := engine.Command{Client: s.ClientID(), ID: "3"}
		held := losesReply(tp, protocol.MethodSessionPrompt)
		done := make(chan error, 1)
		go func() {
			_, err := s.Submit(backend.WithEpoch(tctx(t), s.Epoch()), c, "once", engine.SubmitQueue, "")
			done <- err
		}()
		await(t, recv(t, held), "the client to redial")
		hostReleases(t, h, c.Client)
		h.advanceClock(pastRetirement)
		tp.releaseDials()
		err := recv(t, done)
		outcomeUnknown(t, err, protocol.ReasonResumeLost)
		if !errors.Is(err, backend.ErrOutcomeUnknown) {
			t.Fatalf("not backend.ErrOutcomeUnknown: %v", err)
		}
		if sent := tp.sent(protocol.MethodSessionPrompt); len(sent) != 1 || sent[0].conn != 0 {
			t.Fatalf("the prompt was sent %d times: a resend after resumed: false", len(sent))
		}
		if got := h.stub.Prompts(); !slices.Equal(got, []string{"once"}) {
			t.Fatalf("the host ran %q", got)
		}
	})
}

// TestAStaleEpochIsNeverSent (§3.12, astra r3 13; X43 6): every command whose
// ctx carries an epoch other than the Session's is refused before anything
// is written — ErrOutcomeUnknown, reason resume_lost, matching
// backend.ErrStaleEpoch (and backend.ErrOutcomeUnknown) — and every read is
// backend.ErrStaleEpoch, as in process.
func TestAStaleEpochIsNeverSent(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	s, _ := started(t, h, tp, remote.SessionOptions{})
	stale := backend.WithEpoch(tctx(t), s.Epoch()+1)
	c := engine.Command{Client: s.ClientID(), ID: "5"}
	calls := map[string]func() error{
		"Submit":    func() error { _, err := s.Submit(stale, c, "x", engine.SubmitQueue, ""); return err },
		"Interject": func() error { return s.Interject(stale, c, "x") },
		"Answer":    func() error { return s.Answer(stale, c, "perm-1", agent.AskAnswer{OptionID: "allow"}) },
		"Unqueue":   func() error { _, err := s.Unqueue(stale, c, "q-1"); return err },
		"EditQueued": func() error {
			v := 0
			return s.EditQueued(stale, c, "q-1", "x", &v)
		},
		"ClearQueue": func() error { _, err := s.ClearQueue(stale, c); return err },
		"Disarm":     func() error { return s.Disarm(stale, c) },
		"SetTitle":   func() error { return s.SetTitle(stale, c, "x") },
		"Set": func() error {
			_, err := s.Set(stale, c, engine.Setting{Kind: engine.SettingModel, Value: "fast"})
			return err
		},
		"Cancel":         func() error { _, err := s.Cancel(stale, c, ""); return err },
		"CancelSubagent": func() error { return s.CancelSubagent(stale, c, "a-1") },
		"Stop":           func() error { return s.Stop(stale, c) },
	}
	before := len(tp.linesFrom(0))
	for name, call := range calls {
		err := call()
		outcomeUnknown(t, err, protocol.ReasonResumeLost)
		if !errors.Is(err, backend.ErrStaleEpoch) || !errors.Is(err, backend.ErrOutcomeUnknown) {
			t.Errorf("%s with a stale epoch: %v", name, err)
		}
	}
	if _, _, err := s.Ask(stale, "perm-1"); !errors.Is(err, backend.ErrStaleEpoch) || errors.Is(err, backend.ErrOutcomeUnknown) {
		t.Errorf("Ask with a stale epoch: %v", err)
	}
	if _, err := s.Settings(stale); !errors.Is(err, backend.ErrStaleEpoch) || errors.Is(err, backend.ErrOutcomeUnknown) {
		t.Errorf("Settings with a stale epoch: %v", err)
	}
	for _, l := range tp.linesFrom(before) {
		if l.out {
			t.Fatalf("a stale call wrote %s", l.raw)
		}
	}
}

// TestAReplayLargerThanTheBudgetEndsInARestore (astra 22; §3.4, §3.14, A25):
// through the Session, a load's replay outrunning an attachment made when:
// "now" before the start — a record over the subscription's budget, so the
// host drops it slow_consumer before readiness — ends in a Restore after
// readiness, then the Ready the first reply owed: every re-attach is when:
// "ready" with no cursor, never a cursor inside the same burst; Start returns.
func TestAReplayLargerThanTheBudgetEndsInARestore(t *testing.T) {
	h := newHost(t, withoutStart())
	h.stub.Replay = []agent.Event{{Type: agent.EventText, Text: "small"}, {Type: agent.EventText, Text: big},
		{Type: agent.EventText, Text: "the replay's last"}}
	tp := newTap(t)
	s := dialSession(t, h.path, tp, remote.SessionOptions{When: protocol.WhenNow, Budget: smallBudget})
	if err := s.Attach(tctx(t)); err != nil {
		t.Fatal(err)
	}
	first := readKind(t, s, backend.ItemRestore)
	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(tctx(t)) }()
	if err := h.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	head := h.head()
	items := readUntil(t, s, itemKind(backend.ItemReady))
	if err := recv(t, startErr); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var restore *backend.Item
	for i, it := range items {
		switch it.Kind {
		case backend.ItemRestore:
			restore = &items[i]
		case backend.ItemEvent:
			if restore != nil {
				t.Fatalf("an event between the Restore and the Ready: %s", describeItem(it))
			}
			if it.Event.Text == big {
				t.Fatal("the record over the budget reached the reader live")
			}
		}
	}
	switch {
	case h.dropped() < 1:
		t.Fatalf("the premise: the host dropped %d subscriptions slow", h.dropped())
	case restore == nil || restore.Gen != first.Gen+1 || restore.Snapshot == nil || restore.Snapshot.Seq != head:
		t.Fatalf("the Restore after readiness: %+v (head %d)", restore, head)
	case len(items[len(items)-1].Info.Models) == 0:
		t.Fatalf("the Ready after the Restore: %+v", items[len(items)-1].Info)
	}
	attaches := tp.sent(protocol.MethodSessionAttach)
	if len(attaches) < 2 {
		t.Fatalf("%d attaches: no re-attach", len(attaches))
	}
	for _, l := range attaches[1:] {
		if p := attachParams(t, l); p.Cursor != nil || p.When != protocol.WhenReady {
			t.Fatalf("a re-attach before readiness: cursor %+v, when %q: want none, when: ready", p.Cursor, p.When)
		}
	}
}

// TestTheSessionEndsWithItsHost (§3.9, §3.14): the host's quit — its engine
// closing — is an End item with no error (reset{session_closed}), and every
// Read after it is backend.ErrClosed.
func TestTheSessionEndsWithItsHost(t *testing.T) {
	h := newHost(t)
	s, _ := started(t, h, newTap(t), remote.SessionOptions{})
	if err := h.eng.Close(); err != nil {
		t.Fatal(err)
	}
	items := readUntil(t, s, itemKind(backend.ItemEnd))
	if end := items[len(items)-1]; end.Err != nil {
		t.Fatalf("the host's quit: an End with %v", end.Err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	for range 2 {
		if _, err := s.Read(ctx); !errors.Is(err, backend.ErrClosed) {
			t.Fatalf("a Read after the End: %v", err)
		}
	}
}

// TestAViewCloseLeavesTheSession (§3.9): Close is a view close — the stream
// detached, the client closed, nil answered and never agent.ErrAgentExited —
// and the session goes on on its host, which another client attaches to; a
// command still waiting resolves outcome unknown (disconnected), which is
// backend.ErrOutcomeUnknown; Read and later commands fail; Close is
// idempotent.
func TestAViewCloseLeavesTheSession(t *testing.T) {
	h := newHost(t)
	held, release := h.stub.HoldNextSet()
	t.Cleanup(release)
	tp := newTap(t)
	s, _ := started(t, h, tp, remote.SessionOptions{})
	done := make(chan error, 1)
	go func() {
		_, err := s.Set(tctx(t), engine.Command{Client: s.ClientID(), ID: "1"}, engine.Setting{Kind: engine.SettingModel, Value: "fast"})
		done <- err
	}()
	await(t, held, "the Set to park")
	err := s.Close()
	if err != nil || errors.Is(err, agent.ErrAgentExited) {
		t.Fatalf("a view close: %v", err)
	}
	if detaches := tp.sent(protocol.MethodSessionDetach); len(detaches) != 1 {
		t.Fatalf("%d detaches", len(detaches))
	}
	err = recv(t, done)
	outcomeUnknown(t, err, protocol.ReasonDisconnected)
	if !errors.Is(err, backend.ErrOutcomeUnknown) {
		t.Fatalf("the Set a view close cut off: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
	if _, err := s.Read(tctx(t)); !errors.Is(err, backend.ErrClosed) {
		t.Fatalf("a Read after Close: %v", err)
	}
	if err := s.SetTitle(tctx(t), engine.Command{Client: s.ClientID(), ID: "2"}, "late"); !errors.Is(err, remote.ErrClosed) {
		t.Fatalf("a command after Close: %v", err)
	}
	release()
	if a := h.eng.State().Activity; a == engine.ActivityClosing {
		t.Fatal("a view close stopped the session")
	}
	o, _ := started(t, h, newTap(t), remote.SessionOptions{})
	if o.Info().CrazeSessionID != h.sid() {
		t.Fatalf("another client after the view close: %+v", o.Info())
	}
}

// TestTheCommandIDIsTheCallers (§3.12, X50): a command goes out under the
// caller's own id — so the host stamps its events with exactly the caller's
// engine.Command.Cause, which the TUI keys its echoes and overlays by — and the
// client's own counter stays above every id sent; an id already in flight, and
// one that is not canonical, are refused with nothing sent.
func TestTheCommandIDIsTheCallers(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	s, _ := started(t, h, tp, remote.SessionOptions{})
	hang := h.stub.HangNext()
	c := engine.Command{Client: s.ClientID(), ID: "41"}
	res, err := s.Submit(tctx(t), c, "the caller's", engine.SubmitQueue, "")
	if err != nil || res.Turn == "" || res.Text != "the caller's" {
		t.Fatalf("the submit: %+v, %v", res, err)
	}
	await(t, hang, "the turn to open")
	if got := commandID(t, tp.sent(protocol.MethodSessionPrompt)[0]); got != "41" {
		t.Fatalf("the commandId sent: %q", got)
	}
	var caused bool
	for _, it := range readUntil(t, s, func(it backend.Item) bool {
		return it.Kind == backend.ItemEvent && it.Event.Type == agent.EventTurn
	}) {
		caused = caused || it.Event.Cause == c.Cause()
	}
	if !caused {
		t.Fatalf("the turn's start is not stamped %q", c.Cause())
	}
	if n := s.Client().ResumeState().NextCommand; n <= 41 {
		t.Fatalf("the client's next command id %d is not above the caller's 41", n)
	}
	id, err := s.Client().Command(tctx(t), protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: h.sid(), Text: "minted"}, nil, remote.CommandOptions{})
	if err != nil || id != "42" {
		t.Fatalf("a minted command after the caller's: %q, %v", id, err)
	}

	// An id in flight, and one that is not canonical: refused, nothing sent.
	parked, releaseSet := h.stub.HoldNextSet()
	t.Cleanup(releaseSet)
	setDone := make(chan error, 1)
	go func() {
		_, err := s.Set(tctx(t), engine.Command{Client: s.ClientID(), ID: "50"}, engine.Setting{Kind: engine.SettingModel, Value: "fast"})
		setDone <- err
	}()
	await(t, parked, "the Set to park")
	before := len(tp.sent(protocol.MethodSessionDisarm))
	if err := s.Disarm(tctx(t), engine.Command{Client: s.ClientID(), ID: "50"}); !errors.Is(err, remote.ErrCommandInFlight) {
		t.Fatalf("a second command under an id in flight: %v", err)
	}
	for _, bad := range []string{"050", "0", "-1", "x", "18446744073709551615"} {
		if err := s.Disarm(tctx(t), engine.Command{Client: s.ClientID(), ID: bad}); !errors.Is(err, remote.ErrCommandID) {
			t.Fatalf("the command id %q: %v", bad, err)
		}
	}
	if err := s.Disarm(tctx(t), engine.Command{}); !errors.Is(err, engine.ErrBadRequest) {
		t.Fatalf("a command naming no client and no id: %v", err)
	}
	if n := len(tp.sent(protocol.MethodSessionDisarm)); n != before {
		t.Fatalf("%d refused disarms went out", n-before)
	}
	releaseSet()
	if err := recv(t, setDone); err != nil {
		t.Fatalf("the parked Set: %v", err)
	}
}

// TestTheCodecsMustBeThisBuilds (§3.3): a host whose event or snapshot codec
// is a version this build does not read is a dial error — its stream must not
// be folded — and the client is closed.
func TestTheCodecsMustBeThisBuilds(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	tp.setRewriteIn(func(l wireLine) [][]byte {
		if l.method != protocol.MethodHello || l.resp == nil || l.resp.Error != nil {
			return nil
		}
		var hr protocol.HelloResult
		if err := json.Unmarshal(l.resp.Result, &hr); err != nil {
			t.Errorf("the hello: %v", err)
			return nil
		}
		hr.Codecs.Event = agent.EventCodecVersion + 1
		raw, _ := json.Marshal(hr)
		b, _ := json.Marshal(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(l.id), Result: raw})
		return [][]byte{b}
	})
	_, err := tryDialSession(t, h.path, tp, remote.SessionOptions{})
	var ce *remote.CodecError
	if !errors.As(err, &ce) || ce.Host.Event != agent.EventCodecVersion+1 || !strings.Contains(err.Error(), "cannot be folded") {
		t.Fatalf("a host with another event codec: %v", err)
	}
	h.logs.wait(t, "close client")
}

// TestStopIsSessionStop (plan 030 §3.6a): Stop sends session.stop under the
// caller's own command id, bound as every command is, and returns the host's
// receipt once its coordinator has the stop — the session's end follows on
// the stream. On a host whose capability stop is false the refusal,
// stop_unsupported, is backend.ErrStopUnsupported — a host's answer, never an
// outcome unknown — and nothing was stopped.
func TestStopIsSessionStop(t *testing.T) {
	t.Run("a host that serves it", func(t *testing.T) {
		heard := make(chan control.StopRequest, 1)
		// The coordinator's sequence, off the handler: the engine's close.
		var eng atomic.Pointer[engine.Engine]
		h := newHost(t, withStop(func(r control.StopRequest) {
			heard <- r
			go func() { _ = eng.Load().Close() }()
		}))
		eng.Store(h.eng)
		tp := newTap(t)
		s, _ := started(t, h, tp, remote.SessionOptions{})
		c := engine.Command{Client: s.ClientID(), ID: "7"}
		if err := s.Stop(backend.WithEpoch(tctx(t), s.Epoch()), c); err != nil {
			t.Fatalf("stop: %v", err)
		}
		select {
		case r := <-heard:
			if r.Client != c.Client || r.CommandID != c.ID {
				t.Fatalf("the coordinator was handed %+v, want %s/%s", r, c.Client, c.ID)
			}
		case <-time.After(watchdog):
			t.Fatalf("the coordinator was handed nothing in %s", watchdog)
		}
		if sent := tp.sent(protocol.MethodSessionStop); len(sent) != 1 || !strings.Contains(string(sent[0].params), `"commandId":"7"`) {
			t.Fatalf("session.stop went out %d times: %+v", len(sent), sent)
		}
		// The session's end follows the receipt on the stream.
		end := readUntil(t, s, itemKind(backend.ItemEnd))
		if it := end[len(end)-1]; it.Err != nil {
			t.Fatalf("the stream ended %v, want the session's own end", it.Err)
		}
	})
	t.Run("an older host", func(t *testing.T) {
		h := newHost(t)
		tp := newTap(t)
		s, _ := started(t, h, tp, remote.SessionOptions{})
		err := s.Stop(tctx(t), engine.Command{Client: s.ClientID(), ID: "3"})
		var e *remote.Error
		if !errors.Is(err, backend.ErrStopUnsupported) || !errors.As(err, &e) || e.Reason != protocol.ReasonStopUnsupported {
			t.Fatalf("stop on a host without it: %v, want backend.ErrStopUnsupported", err)
		}
		if errors.Is(err, backend.ErrOutcomeUnknown) {
			t.Fatalf("a host's refusal is an outcome unknown: %v", err)
		}
		if st := h.eng.State(); st.Activity == engine.ActivityClosing {
			t.Fatal("a refused stop closed the session")
		}
	})
}

// TestAStopIsAnsweredByTheSessionsEnd (plan 030 C5, X3): a stop that meets
// the session's end before its receipt — a joined stop, or one arriving once a
// signal or the idle exit set the end going, whose receipt the host's ending
// connection never writes — is answered by that end: nil, never an outcome
// unknown, forced here by the tap dropping the receipt while the stream's
// reset{session_closed} goes through. A stop made once the session has ended
// is answered at once, sending nothing. Either way the Session's close after
// it detaches nothing.
func TestAStopIsAnsweredByTheSessionsEnd(t *testing.T) {
	var eng atomic.Pointer[engine.Engine]
	h := newHost(t, withStop(func(control.StopRequest) {
		go func() { _ = eng.Load().Close() }()
	}))
	eng.Store(h.eng)
	tp := newTap(t)
	tp.setRewriteIn(func(l wireLine) [][]byte {
		if l.method == protocol.MethodSessionStop && l.resp != nil {
			return [][]byte{} // the receipt never reaches the client
		}
		return nil
	})
	s, _ := started(t, h, tp, remote.SessionOptions{})
	if err := s.Stop(tctx(t), engine.Command{Client: s.ClientID(), ID: "5"}); err != nil {
		t.Fatalf("a stop whose receipt the session's end overtook: %v, want nil", err)
	}
	select {
	case <-s.Ended():
	default:
		t.Fatal("Stop answered before the session's end")
	}
	sent := len(tp.sent(protocol.MethodSessionStop))
	if err := s.Stop(tctx(t), engine.Command{Client: s.ClientID(), ID: "6"}); err != nil {
		t.Fatalf("a stop after the session's end: %v, want nil", err)
	}
	if got := len(tp.sent(protocol.MethodSessionStop)); got != sent {
		t.Fatalf("a stop after the session's end was sent: %d stops on the wire, want %d", got, sent)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close after a stop: %v", err)
	}
	if n := len(tp.sent(protocol.MethodSessionDetach)); n != 0 {
		t.Fatalf("close after a stop sent %d detaches", n)
	}
}

// TestCloseWithinBoundsItsDetach (plan 030 C5r, astra r8-c5 4): the explicit
// quit's close shares the quit's deadline. With time left the detach is sent,
// and a host that never answers it (the tap drops the reply) holds the close
// only until the context ends — forced here by ending it once the detach is on
// the wire — never for the client's own detach bound; with the context already
// done (the deadline passed, or a second quit) nothing is detached at all and
// the transport is closed at once.
func TestCloseWithinBoundsItsDetach(t *testing.T) {
	// bound is generous for a close whose context has ended, and short of the
	// 3 s detach wait a close with no deadline of its own spends (closeBound).
	const bound = 2 * time.Second
	closing := func(s *remote.Session, ctx context.Context) <-chan error {
		done := make(chan error, 1)
		go func() { done <- s.CloseWithin(ctx) }()
		return done
	}
	closed := func(t *testing.T, done <-chan error, what string) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("close: %v", err)
			}
		case <-time.After(bound):
			t.Fatalf("%s: the close did not return within %v", what, bound)
		}
	}
	withheld := func(tp *tap) {
		tp.setRewriteIn(func(l wireLine) [][]byte {
			if l.method == protocol.MethodSessionDetach && l.resp != nil {
				return [][]byte{} // the host never answers the detach
			}
			return nil
		})
	}

	t.Run("time left", func(t *testing.T) {
		h := newHost(t)
		tp := newTap(t)
		withheld(tp)
		s, _ := started(t, h, tp, remote.SessionOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := closing(s, ctx)
		tp.await(t, "the detach on the wire", func() bool { return len(tp.sent(protocol.MethodSessionDetach)) == 1 })
		select {
		case <-done:
			t.Fatal("the close returned with its detach unanswered and its context live")
		default:
		}
		cancel() // the quit's deadline
		closed(t, done, "the context ended")
	})
	t.Run("none left", func(t *testing.T) {
		h := newHost(t)
		tp := newTap(t)
		withheld(tp)
		s, _ := started(t, h, tp, remote.SessionOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		closed(t, closing(s, ctx), "no time left")
		if n := len(tp.sent(protocol.MethodSessionDetach)); n != 0 {
			t.Fatalf("%d detaches sent with no time left, want none", n)
		}
	})
}
