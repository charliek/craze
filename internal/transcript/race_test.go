package transcript

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// raceBounds are small enough that a short script trims by count and by
// bytes, compacts the deque, saturates the stream cap and evicts from the
// roster — every path that could write something a cut still holds.
var raceBounds = Bounds{MainEntries: 48, MainBytes: 12 << 10, SubEntries: 12, SubBytes: 3 << 10, StreamText: 256, Agents: 2}

// raceScript is a deterministic session of n events, sequenced 1..n: chunks
// into open runs (main and children, some past the stream cap), tool reports
// and their updates, notes, deltas, roster spawns and finishes, queue verbs,
// asks and their endings.
func raceScript(n int) []agent.Event {
	evs := make([]agent.Event, 0, n)
	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	for i := 0; len(evs) < n; i++ {
		stamp := base.Add(time.Duration(i) * time.Millisecond)
		child := fmt.Sprintf("sub-%d", i/40)
		var ev agent.Event
		switch i % 17 {
		case 0, 1, 2:
			ev = agent.Event{Type: agent.EventThought, Text: strings.Repeat("t", 1+i%97)}
		case 3, 4:
			ev = agent.Event{Type: agent.EventText, Text: strings.Repeat("a", 1+(i*7)%300)}
		case 5:
			ev = agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: fmt.Sprintf("t%d", i%23), Status: "pending", RawInput: strings.Repeat("r", i%200), At: stamp}}
		case 6:
			ev = agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: fmt.Sprintf("t%d", (i+5)%23), Status: "completed", ContentText: strings.Repeat("c", i%500)}}
		case 7:
			ev = agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{
				Title: strp(fmt.Sprintf("title %d", i)), Mode: strp("agent"),
				Config: &agent.ConfigState{Options: []agent.ConfigOption{{ID: "effort", Current: fmt.Sprint(i)}}},
			}}
		case 8:
			ev = agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: child, Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned}
		case 9, 10:
			ev = agent.Event{Type: agent.EventText, Agent: child, Text: strings.Repeat("c", 1+(i*13)%400)}
		case 11:
			ev = agent.Event{Type: agent.EventTool, Agent: child, Tool: &agent.ToolEvent{ID: fmt.Sprintf("c%d", i%5), Status: "pending"}}
		case 12:
			ev = agent.Event{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: fmt.Sprintf("q%d", i%4), Text: "row"}, QueueChange: []agent.QueueChange{agent.QueueQueued, agent.QueueEdited, agent.QueueSent}[i%3], QueuePos: i % 3}
		case 13:
			if i%2 == 0 {
				ev = agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: fmt.Sprintf("ask-%d", i)}}
			} else {
				ev = agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{ID: fmt.Sprintf("ask-%d", i-26), Outcome: agent.AskAnswered}}
			}
		case 14:
			ev = agent.Event{Type: agent.EventDone, StopReason: []string{"end_turn", "cancelled"}[i%2]}
		case 15:
			ev = agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: child, Status: agent.SubagentCompleted, EndedAt: stamp}, SubagentChange: agent.SubagentChangeFinished}
		case 16:
			if i%3 == 0 {
				ev = agent.Event{Type: agent.EventError, Err: errors.New("boom")}
			} else {
				ev = agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: fmt.Sprintf("turn-%d", i), Phase: agent.TurnStarted, Text: "go"}}
			}
		}
		if ev.At.IsZero() {
			ev.At = stamp
		}
		evs = append(evs, ev)
	}
	return sequenced(evs)
}

// TestASnapshotRacingTheFoldIsExactAtItsSeq (plan 024 §3.4, A6; run under
// -race by make test-race): readers take cuts while another goroutine folds —
// chunks, tool updates, deltas, trims, roster evictions — and every cut, read
// only after the whole run is over, equals a fresh model folded to exactly the
// cut's Seq, on the history and the state projections. A cut that shared
// anything the fold later wrote would differ from its prefix model, or race.
//
// Barriers, not sleeps: at every checkpoint the folder hands a reader a reply
// channel and waits for the cut it takes, so cuts land at known Seqs whatever
// the scheduler does; between checkpoints the readers cut freely, racing the
// fold.
func TestASnapshotRacingTheFoldIsExactAtItsSeq(t *testing.T) {
	evs := raceScript(1200)
	opts := Options{Bounds: raceBounds}
	m := New(opts)

	const readers = 3
	checkpoint := make(chan chan cut)
	done := make(chan struct{})
	got := make([][]cut, readers)
	var wg sync.WaitGroup
	for r := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var last uint64
			keep := func(c cut) {
				if c.seq != last || len(got[r]) == 0 {
					got[r] = append(got[r], c)
					last = c.seq
				}
			}
			for {
				select {
				case <-done:
					return
				case reply := <-checkpoint:
					c := m.cut()
					keep(c)
					reply <- c
				default:
					keep(m.cut())
				}
			}
		}()
	}
	var barrier []cut
	for i, ev := range evs {
		m.Fold(ev)
		if i%97 == 0 {
			reply := make(chan cut)
			checkpoint <- reply
			barrier = append(barrier, <-reply)
		}
	}
	close(done)
	wg.Wait()

	all := append([]cut(nil), barrier...)
	for _, cs := range got {
		all = append(all, cs...)
	}
	bySeq := map[uint64][]cut{}
	for _, c := range all {
		bySeq[c.seq] = append(bySeq[c.seq], c)
	}
	if len(bySeq) < len(barrier) {
		t.Fatalf("only %d distinct cuts", len(bySeq))
	}

	// The reference folds the same events, in order, and is compared with every
	// cut at the Seq it reaches.
	ref := New(opts)
	openTails := 0
	for s := uint64(0); s <= uint64(len(evs)); s++ {
		if s > 0 {
			ref.Fold(evs[s-1])
		}
		cs := bySeq[s]
		if len(cs) == 0 {
			continue
		}
		wantH, wantS := ref.History(), ref.State()
		for _, c := range cs {
			if c.main.streamOpen {
				openTails++
			}
			if h := c.history(); !reflect.DeepEqual(h, wantH) {
				t.Fatalf("the cut at Seq %d differs from a model folded to %d:\ncut %+v\nref %+v", s, s, h.Main, wantH.Main)
			}
			if st := c.state(); !reflect.DeepEqual(st, wantS) {
				t.Fatalf("the cut's state at Seq %d differs from a model folded to %d:\ncut %+v\nref %+v", s, s, st, wantS)
			}
		}
	}
	if openTails == 0 {
		t.Fatal("no cut caught an open run, so none copied a tail")
	}
	if !ref.Main.trimmed || len(ref.subOrder) >= len(evs)/40 || ref.Main.head+ref.Main.base == 0 {
		t.Fatal("the script no longer trims and evicts, so the cuts no longer race those paths")
	}
	t.Logf("%d cuts at %d distinct Seqs, %d holding an open run's tail", len(all), len(bySeq), openTails)
}
