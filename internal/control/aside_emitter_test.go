package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/protocol"
)

const emitterWatchdog = 10 * time.Second

// emitterOver is an emitter over a bare outbox, the aside's abort recorded.
func emitterOver(t *testing.T, o *outbox, interval time.Duration) (*asideEmitter, context.Context, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	aborted := make(chan struct{})
	m := newAsideEmitter(&conn{out: o}, "a1", interval, func() {
		select {
		case <-aborted:
		default:
			close(aborted)
		}
		cancel()
	})
	return m, ctx, aborted
}

// takeNotice is the next line the outbox hands its writer, as a notice.
func takeNotice(t *testing.T, o *outbox) protocol.AsideNoticeParams {
	t.Helper()
	got := make(chan outLine, 1)
	go func() {
		ln, _ := o.next()
		got <- ln
	}()
	var ln outLine
	select {
	case ln = <-got:
	case <-time.After(emitterWatchdog):
		t.Fatal("no line queued")
	}
	o.written(ln)
	var n struct {
		Method string                     `json:"method"`
		Params protocol.AsideNoticeParams `json:"params"`
	}
	if err := json.Unmarshal(ln.b, &n); err != nil || n.Method != protocol.NotifyAside {
		t.Fatalf("not an aside notice: %s (%v)", ln.b, err)
	}
	return n.Params
}

// queued is how many lines the outbox holds.
func queued(o *outbox) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.q)
}

// TestSplitNoticesCutsBetweenCharacters (plan 040 §3.2): a notice is at most
// AsideNoticeMax bytes and never ends inside a character; whole takes all of
// it, and otherwise only full notices, the rest kept.
func TestSplitNoticesCutsBetweenCharacters(t *testing.T) {
	s := "a" + strings.Repeat("é", protocol.AsideNoticeMax) // the 4096th byte is the first half of an é
	out, rest := splitNotices(nil, s, true)
	if rest != "" || strings.Join(out, "") != s {
		t.Fatal("whole lost text")
	}
	for i, n := range out {
		if len(n) > protocol.AsideNoticeMax || !utf8.ValidString(n) {
			t.Fatalf("notice %d: %d bytes, valid %v", i, len(n), utf8.ValidString(n))
		}
	}
	if len(out[0]) != protocol.AsideNoticeMax-1 {
		t.Fatalf("the first notice is %d bytes; the cut belongs before the split é", len(out[0]))
	}
	out, rest = splitNotices(nil, strings.Repeat("x", protocol.AsideNoticeMax+5), false)
	if len(out) != 1 || len(out[0]) != protocol.AsideNoticeMax || rest != "xxxxx" {
		t.Fatalf("full notices only: %d notices, rest %q", len(out), rest)
	}
	if out, rest = splitNotices(nil, "abc", false); len(out) != 0 || rest != "abc" {
		t.Fatalf("less than a notice: %d notices, rest %q", len(out), rest)
	}
}

// TestTheEmitterFlushesAtAFullNoticeTheIntervalAndTheEnd (plan 040 §3.2): with
// an interval, pieces under a notice's worth wait — a full notice's worth is
// sent at once, the rest at the interval or, at the latest, before the reply
// (finish) — and every notice is numbered from 1 in order.
func TestTheEmitterFlushesAtAFullNoticeTheIntervalAndTheEnd(t *testing.T) {
	o := newOutbox(nil)
	m, ctx, _ := emitterOver(t, o, time.Hour) // the interval never comes
	go m.run(ctx)
	m.delta("abc")
	m.delta(strings.Repeat("x", protocol.AsideNoticeMax))
	n := takeNotice(t, o)
	if n.Seq != 1 || len(n.Text) != protocol.AsideNoticeMax || !strings.HasPrefix(n.Text, "abcx") {
		t.Fatalf("the full notice: seq %d, %d bytes", n.Seq, len(n.Text))
	}
	m.finish()
	if n := takeNotice(t, o); n.Seq != 2 || n.Text != "xxx" {
		t.Fatalf("the rest, at the end: %+v", n)
	}

	o = newOutbox(nil)
	m, ctx, _ = emitterOver(t, o, time.Millisecond)
	go m.run(ctx)
	m.delta("abc")
	if n := takeNotice(t, o); n.Seq != 1 || n.Text != "abc" {
		t.Fatalf("the interval's notice: %+v", n)
	}
	m.finish()
	if q := queued(o); q != 0 {
		t.Fatalf("%d lines queued after the interval took everything", q)
	}
}

// TestTheEmitterNeverBlocksTheAgentAndBoundsWhatWaits (plan 040 §3.2, P2):
// with the outbox full the emitter waits for room and the agent's onDelta
// never does; once more than AsidePendingMax waits unsent, the aside's
// notices stop and their text is dropped — after the room is made only the
// notice already on its way is queued, and the end sends nothing more.
func TestTheEmitterNeverBlocksTheAgentAndBoundsWhatWaits(t *testing.T) {
	full := make(chan struct{}, 1)
	o := newOutbox(func() {
		select {
		case full <- struct{}{}:
		default:
		}
	})
	if err := o.push(context.Background(), make([]byte, ordinaryLimit), nil); err != nil {
		t.Fatal(err)
	}
	m, ctx, aborted := emitterOver(t, o, AsideEachDelta)
	go m.run(ctx)
	m.delta("first")
	select {
	case <-full:
	case <-time.After(emitterWatchdog):
		t.Fatal("the emitter's notice did not wait for room")
	}
	piece := strings.Repeat("y", 64<<10)
	for range protocol.AsidePendingMax/len(piece) + 1 { // just over the bound, every call returning
		m.delta(piece)
	}
	m.mu.Lock()
	over, kept := m.over, m.size
	m.mu.Unlock()
	if !over || kept != 0 {
		t.Fatalf("over the bound: over %v, %d bytes kept", over, kept)
	}
	ln, _ := o.next() // the writer takes the full budget's line: room
	o.written(ln)
	if n := takeNotice(t, o); n.Seq != 1 || n.Text != "first" {
		t.Fatalf("the notice on its way: %+v", n)
	}
	m.delta("late")
	m.finish()
	if q := queued(o); q != 0 {
		t.Fatalf("%d notices queued after the bound", q)
	}
	select {
	case <-aborted:
		t.Fatal("the bound cancelled the aside")
	default:
	}
}

// TestTheBoundCountsTheNoticesOnTheirWay (plan 040 §3.2, astra P2): text a
// flush has taken and not yet queued is still unsent, and counts against
// AsidePendingMax. With the outbox full, one flush takes exactly the bound's
// worth and waits for room on its first notice; the next piece trips the
// bound — not another bound's worth later — and once room is made only the
// notice already on its way is queued. Negative control: a flush that stops
// counting what it took lets a second 1 MiB wait unsent with over still
// false.
func TestTheBoundCountsTheNoticesOnTheirWay(t *testing.T) {
	full := make(chan struct{}, 1)
	o := newOutbox(func() {
		select {
		case full <- struct{}{}:
		default:
		}
	})
	if err := o.push(context.Background(), make([]byte, ordinaryLimit), nil); err != nil {
		t.Fatal(err)
	}
	m, ctx, aborted := emitterOver(t, o, time.Hour) // the interval never comes
	piece := strings.Repeat("y", 64<<10)
	for range protocol.AsidePendingMax / len(piece) { // exactly the bound: not over it
		m.delta(piece)
	}
	go m.run(ctx) // its one poke waiting: a flush takes the whole bound's worth
	select {
	case <-full:
	case <-time.After(emitterWatchdog):
		t.Fatal("the flush's first notice did not wait for room")
	}
	for range protocol.AsidePendingMax / len(piece) {
		m.delta(piece)
	}
	m.mu.Lock()
	over := m.over
	m.mu.Unlock()
	if !over {
		t.Fatalf("%d bytes taken and not queued, %d more delivered, and the bound not tripped",
			protocol.AsidePendingMax, protocol.AsidePendingMax)
	}
	ln, _ := o.next() // the writer takes the full budget's line: room
	o.written(ln)
	if n := takeNotice(t, o); n.Seq != 1 || len(n.Text) != protocol.AsideNoticeMax {
		t.Fatalf("the notice on its way: seq %d, %d bytes", n.Seq, len(n.Text))
	}
	m.finish()
	if q := queued(o); q != 0 {
		t.Fatalf("%d notices queued after the bound", q)
	}
	select {
	case <-aborted:
		t.Fatal("the bound cancelled the aside")
	default:
	}
}

// TestANoticeOnItsWayIsNotCountedTwice (plan 040 §3.2, C1 review r2): the
// notice being pushed leaves the count as it is handed to the outbox, so text
// already on its way is never counted twice — queued and taken — and a delta
// that keeps the unsent text within the bound does not trip it. With the
// outbox full, a flush takes exactly the bound's worth and waits for room on
// its first notice; one more notice's worth then arrives, which leaves the
// unsent text at the bound, not over it; once room is made the whole answer
// streams, nothing dropped. Negative control: retiring the notice only after
// its push counts the waiting notice twice and trips the bound.
func TestANoticeOnItsWayIsNotCountedTwice(t *testing.T) {
	full := make(chan struct{}, 1)
	o := newOutbox(func() {
		select {
		case full <- struct{}{}:
		default:
		}
	})
	if err := o.push(context.Background(), make([]byte, ordinaryLimit), nil); err != nil {
		t.Fatal(err)
	}
	m, ctx, aborted := emitterOver(t, o, time.Hour) // the interval never comes
	piece := strings.Repeat("y", 64<<10)
	for range protocol.AsidePendingMax / len(piece) {
		m.delta(piece)
	}
	go m.run(ctx)
	select {
	case <-full:
	case <-time.After(emitterWatchdog):
		t.Fatal("the flush's first notice did not wait for room")
	}
	m.delta(strings.Repeat("z", protocol.AsideNoticeMax))
	m.mu.Lock()
	over := m.over
	m.mu.Unlock()
	if over {
		t.Fatal("the notice waiting for room was counted twice: the bound tripped at exactly its worth")
	}
	ln, _ := o.next() // the writer takes the full budget's line: room
	o.written(ln)
	finished := make(chan struct{})
	go func() {
		m.finish()
		close(finished)
	}()
	total := 0
	for total < protocol.AsidePendingMax+protocol.AsideNoticeMax {
		n := takeNotice(t, o)
		total += len(n.Text)
	}
	select {
	case <-finished:
	case <-time.After(emitterWatchdog):
		t.Fatal("finish did not return once the answer was queued")
	}
	if total != protocol.AsidePendingMax+protocol.AsideNoticeMax {
		t.Fatalf("%d bytes streamed, want %d", total, protocol.AsidePendingMax+protocol.AsideNoticeMax)
	}
	select {
	case <-aborted:
		t.Fatal("the aside was cancelled")
	default:
	}
}

// TestAnEmitterWhoseOutboxClosesCancelsTheAside (plan 040 §3.2): a notice
// that cannot be queued — the connection closed during the final flush — ends
// the emitter, cancelling the aside, and finish returns.
func TestAnEmitterWhoseOutboxClosesCancelsTheAside(t *testing.T) {
	full := make(chan struct{}, 1)
	o := newOutbox(func() {
		select {
		case full <- struct{}{}:
		default:
		}
	})
	if err := o.push(context.Background(), make([]byte, ordinaryLimit), nil); err != nil {
		t.Fatal(err)
	}
	m, ctx, aborted := emitterOver(t, o, time.Hour)
	go m.run(ctx)
	m.delta("the last words")
	finished := make(chan struct{})
	go func() {
		m.finish()
		close(finished)
	}()
	select {
	case <-full:
	case <-time.After(emitterWatchdog):
		t.Fatal("the final flush did not wait for room")
	}
	o.close()
	for _, ch := range []<-chan struct{}{aborted, finished} {
		select {
		case <-ch:
		case <-time.After(emitterWatchdog):
			t.Fatal("a closed outbox left the emitter waiting, or the aside running")
		}
	}
}
