package tui

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
)

// The held queue's accounting and retention (astra C17 7 and 13): every held
// message is charged its own data by one stated rule, and a drained message's
// payload is not kept alive by the queue.

// TestHeldResultsAreChargedTheirPayload (astra C17 7): result messages carry
// payload too — a refused model change's prev, a model apply's notes — and
// they count toward the held queue's byte bound as events do. Eight results of
// 8 MiB release the gate at the byte bound, far below the count bound.
func TestHeldResultsAreChargedTheirPayload(t *testing.T) {
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })
	m, _ := gatedModel(t)
	var got []error
	send := func(msg tea.Msg) {
		tm, _ := m.gated(msg, withGateOps)
		m = tm.(Model)
	}
	send(gateOpMsg{call: blockedCall(never, nil), cont: func(m Model, rep gateReply) (Model, tea.Cmd) {
		got = append(got, rep.err)
		return m, nil
	}})
	big := strings.Repeat("r", 8<<20)
	for i := range 7 {
		send(revertModelMsg{prev: big, err: errors.New("refused")})
		if m.gate == nil {
			t.Fatalf("released after %d results (%d bytes held), before the bound", i+1, m.heldBytes)
		}
	}
	send(modelApplyMsg{done: []applyStep{{note: big, label: "model"}}})
	if m.gate != nil || len(got) != 1 || !errors.Is(got[0], ErrNoAnswer) {
		t.Fatalf("at %d bytes held in %d messages: gate %v, continuation saw %v; want released at the byte bound",
			m.heldBytes, len(m.held), m.gate, got)
	}
}

// probeMsg carries one of everything the payload rule has to decide about.
type probeMsg struct {
	note  string
	runes []rune
	opts  map[string]string
	tool  *agent.ToolEvent
	err   error
	at    time.Time
	eng   *engine.Engine
	b     backend.Backend
	model *Model
	fn    func()
	ch    chan int
	mu    *sync.Mutex
	any   any
}

// TestTheHeldChargeFollowsItsRule: a held message is charged heldCharge plus
// its own data — strings, number slices, maps and plain data behind a pointer
// (an event's tool payload), an error at the fixed charge — and nothing a
// handle reaches: an engine, a backend, the model, a func, a chan, a lock, any
// other interface.
func TestTheHeldChargeFollowsItsRule(t *testing.T) {
	m := sized(t)
	eng := engineOf(t, m)
	tool := &agent.ToolEvent{ID: "call-1", Title: "Read main.go", RawInput: strings.Repeat("i", 1000)}
	msg := probeMsg{
		note:  "twelve bytes",
		runes: []rune("four"),
		opts:  map[string]string{"k": "vv"},
		tool:  tool,
		err:   errors.New("a very long error text that is never read"),
		at:    time.Now(),
		eng:   eng,
		b:     m.eng,
		model: &m,
		fn:    func() {},
		ch:    make(chan int),
		mu:    &sync.Mutex{},
		any:   strings.Repeat("h", 1000),
	}
	want := heldCharge + len(msg.note) + 4*len(msg.runes) + len("k") + len("vv") +
		len(tool.ID) + len(tool.Title) + len(tool.RawInput) + errValueBytes
	if got := payloadBytes(msg); got != want {
		t.Fatalf("the probe is charged %d, want %d: only its own data counts", got, want)
	}
	if got := payloadBytes(drainMsg{}); got != heldCharge {
		t.Fatalf("an empty message is charged %d, want the fixed %d", got, heldCharge)
	}
	// An event's payload behind its pointers is plain data, and counted.
	big := strings.Repeat("o", 1<<16)
	ev := agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "c", Output: &agent.ToolOutput{Stdout: big}}}
	if got := payloadBytes(eventMsg{ev}); got < len(big) {
		t.Fatalf("an event with a %d-byte tool output is charged %d", len(big), got)
	}
}

// racingBackend is a backend whose own fields another goroutine is writing
// while the test runs: a charge that walked into it would race, and -race
// says so.
type racingBackend struct {
	backend.Backend
	state string
	rows  []string
}

// TestAHeldStartAnswerIsChargedWithoutWalkingItsBackend: startedMsg and
// errMsg carry the backend they are about. They are charged the fixed charge
// (and errMsg its error's) without the charge ever reading the backend, which
// is live — its goroutines write it — so the walk is race-clean under -race
// while another goroutine writes it.
func TestAHeldStartAnswerIsChargedWithoutWalkingItsBackend(t *testing.T) {
	b := &racingBackend{}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			b.state = strings.Repeat("s", i%64)
			b.rows = append(b.rows[:0], b.state)
		}
	}()
	for range 200 {
		if got := payloadBytes(startedMsg{eng: b}); got != heldCharge {
			t.Fatalf("a held startedMsg is charged %d, want the fixed %d", got, heldCharge)
		}
		if got := payloadBytes(errMsg{err: errors.New("start failed"), eng: b}); got != heldCharge+errValueBytes {
			t.Fatalf("a held errMsg is charged %d, want %d", got, heldCharge+errValueBytes)
		}
	}
	close(stop)
	<-done
}

// finalizedErr is an error whose collection a test can watch.
type finalizedErr struct{ payload []byte }

func (e *finalizedErr) Error() string { return "refused" }

// TestADrainedRecordIsNotKeptAlive (astra C17 13): a drained message is gone
// from the queue's array too — the slot is emptied before the queue moves
// past it — so its payload is collectable while later messages stay held.
func TestADrainedRecordIsNotKeptAlive(t *testing.T) {
	m, _ := gatedModel(t)
	r := newGateRig(t, m)
	release := make(chan struct{})
	r.send(gateOpMsg{call: blockedCall(release, nil), cont: noteCont("released")})
	collected := make(chan struct{})
	func() {
		e := &finalizedErr{payload: make([]byte, 1<<20)}
		runtime.SetFinalizer(e, func(*finalizedErr) { close(collected) })
		// Its handler draws e.Error() and keeps nothing of it.
		r.send(revertModelMsg{prev: "x", err: e})
	}()
	r.send(runeKey('a'))
	r.send(runeKey('b'))
	close(release)
	r.answer()
	r.drain()
	if len(r.m.held) != 2 {
		t.Fatalf("%d messages held after one drain, want the two keys", len(r.m.held))
	}
	if !awaitCollected(collected) {
		t.Fatal("the drained record is still reachable while later messages are held")
	}
	// The model, and the queue with it, is live to here: the record was let
	// go by the queue, not with it.
	r.drainAll()
	if r.m.input.Value() != "ab" {
		t.Fatalf("the held keys drained as %q", r.m.input.Value())
	}
}

// TestALongDrainMovesTheQueueToAFreshArray (astra C17 13): once more than half
// the queue's array is drained slots, what is still held moves to an array of
// its own, and the old one — every slot a drained message ever sat in — is
// let go.
func TestALongDrainMovesTheQueueToAFreshArray(t *testing.T) {
	m, _ := gatedModel(t)
	r := newGateRig(t, m)
	release := make(chan struct{})
	r.send(gateOpMsg{call: blockedCall(release, nil), cont: noteCont("released")})
	for _, k := range "abcdefgh" {
		r.send(runeKey(k))
	}
	if len(r.m.held) != 8 || cap(r.m.held) != 8 {
		t.Fatalf("held %d in an array of %d, want 8 in 8", len(r.m.held), cap(r.m.held))
	}
	collected := make(chan struct{})
	runtime.SetFinalizer(&r.m.held[0], func(*heldMsg) { close(collected) })
	close(release)
	r.answer()
	for range 5 {
		r.drain()
	}
	if len(r.m.held) != 3 || r.m.heldDrained != 0 {
		t.Fatalf("after 5 of 8 drained: %d held, %d drained slots counted", len(r.m.held), r.m.heldDrained)
	}
	if !awaitCollected(collected) {
		t.Fatal("the queue still lives in its old array, five of its eight slots drained")
	}
	r.drainAll()
	if r.m.input.Value() != "abcdefgh" {
		t.Fatalf("the keys drained as %q", r.m.input.Value())
	}
}

// awaitCollected collects until the finalizer behind collected has run, for a
// bounded while.
func awaitCollected(collected <-chan struct{}) bool {
	for range 100 {
		runtime.GC()
		select {
		case <-collected:
			return true
		case <-time.After(10 * time.Millisecond):
		}
	}
	return false
}
