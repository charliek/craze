package transcript

import (
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
				// An error as the engine's observer holds one (X14), so a
				// restored model's history compares equal by value.
				ev = agent.Event{Type: agent.EventError, Err: &agent.RemoteError{Message: "boom", Class: agent.EventErrOther}}
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

// raced is one reader's take at one Seq: the bare cut, and a whole Snapshot
// with its encoding, made on the reader's goroutine while the fold runs on.
type raced struct {
	seq uint64
	c   *cut
	s   *Snapshot
	b   []byte
}

// TestASnapshotRacingTheFoldIsExactAtItsSeq (plan 024 §3.4, A6; run under
// -race by make test-race): readers take cuts, and whole snapshots which they
// encode on the spot, while another goroutine folds — chunks, tool updates,
// deltas, trims, roster evictions. Read only after the whole run is over,
// every cut equals a fresh model folded to exactly its Seq on the history and
// the state projections, and so does every snapshot restored — in process and
// through the codec — on those and the last-ended list; the snapshots taken
// at the checkpoints, restored, fold the rest of the session to exactly the
// model the reference reaches. A cut or a snapshot that shared anything the
// fold later wrote would differ from its prefix model, or race.
//
// Barriers, not sleeps: at every checkpoint the folder hands a reader a reply
// channel and waits for the cut and the snapshot it takes, so they land at
// known Seqs whatever the scheduler does; between checkpoints the readers take
// them freely, racing the fold.
func TestASnapshotRacingTheFoldIsExactAtItsSeq(t *testing.T) {
	evs := raceScript(1200)
	opts := Options{Bounds: raceBounds}
	m := New(opts)

	take := func(snap bool) raced {
		if !snap {
			c := m.cut()
			return raced{seq: c.seq, c: &c}
		}
		s, err := m.Snapshot(0)
		if err != nil {
			panic(err)
		}
		b, err := EncodeSnapshot(s)
		if err != nil {
			panic(err)
		}
		return raced{seq: s.Seq, s: s, b: b}
	}
	const readers = 3
	checkpoint := make(chan chan [2]raced)
	done := make(chan struct{})
	got := make([][]raced, readers)
	var wg sync.WaitGroup
	for r := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var last [2]uint64
			keep := func(x raced) {
				i := 0
				if x.s != nil {
					i = 1
				}
				if x.seq != last[i] || len(got[r]) < 2 {
					got[r] = append(got[r], x)
					last[i] = x.seq
				}
			}
			for n := 0; ; n++ {
				select {
				case <-done:
					return
				case reply := <-checkpoint:
					pair := [2]raced{take(false), take(true)}
					keep(pair[0])
					keep(pair[1])
					reply <- pair
				default:
					keep(take(n%2 == 1))
				}
			}
		}()
	}
	var barrier []raced
	for i, ev := range evs {
		m.Fold(ev)
		if i%97 == 0 {
			reply := make(chan [2]raced)
			checkpoint <- reply
			pair := <-reply
			barrier = append(barrier, pair[0], pair[1])
		}
	}
	close(done)
	wg.Wait()

	all := append([]raced(nil), barrier...)
	for _, xs := range got {
		all = append(all, xs...)
	}
	bySeq := map[uint64][]raced{}
	snapshots := 0
	for _, x := range all {
		bySeq[x.seq] = append(bySeq[x.seq], x)
		if x.s != nil {
			snapshots++
		}
	}
	if len(bySeq) < len(barrier)/2 || snapshots < len(barrier)/2 {
		t.Fatalf("only %d distinct Seqs, %d snapshots", len(bySeq), snapshots)
	}

	// The reference folds the same events, in order, and is compared with every
	// cut and every restored snapshot at the Seq it reaches.
	ref := New(opts)
	openTails, restored := 0, 0
	var onward []*Model
	for s := uint64(0); s <= uint64(len(evs)); s++ {
		if s > 0 {
			ref.Fold(evs[s-1])
			for _, r := range onward {
				r.Fold(evs[s-1])
			}
		}
		xs := bySeq[s]
		if len(xs) == 0 {
			continue
		}
		wantH, wantS := ref.History(), ref.State()
		for _, x := range xs {
			if x.c == nil {
				continue
			}
			if x.c.main.streamOpen {
				openTails++
			}
			if h := x.c.history(); !reflect.DeepEqual(h, wantH) {
				t.Fatalf("the cut at Seq %d differs from a model folded to %d:\ncut %+v\nref %+v", s, s, h.Main, wantH.Main)
			}
			if st := x.c.state(); !reflect.DeepEqual(st, wantS) {
				t.Fatalf("the cut's state at Seq %d differs from a model folded to %d:\ncut %+v\nref %+v", s, s, st, wantS)
			}
		}
		first := true
		for _, x := range xs {
			if x.s == nil {
				continue
			}
			r1, r2 := restoredBoth(t, x.s, x.b, opts)
			assertSameModel(t, fmt.Sprintf("the snapshot at Seq %d, restored in process", s), ref, r1)
			assertSameModel(t, fmt.Sprintf("the snapshot at Seq %d, restored through the codec", s), ref, r2)
			restored++
			// One snapshot per Seq folds on to the end, both ways.
			if first {
				onward = append(onward, r1, r2)
				first = false
			}
		}
	}
	for i, r := range onward {
		assertSameModel(t, fmt.Sprintf("restored model %d folded to the end", i), ref, r)
	}
	if openTails == 0 {
		t.Fatal("no cut caught an open run, so none copied a tail")
	}
	if !ref.Main.trimmed || len(ref.subOrder) >= len(evs)/40 || ref.Main.head+ref.Main.base == 0 {
		t.Fatal("the script no longer trims and evicts, so the cuts no longer race those paths")
	}
	t.Logf("%d takes at %d distinct Seqs: %d cuts (%d holding an open run's tail), %d snapshots restored twice, %d restored models folded to the end",
		len(all), len(bySeq), len(all)-snapshots, openTails, restored, len(onward))
}

// TestACutBetweenChunksCarriesItsChunksEnd (execution amendment X24): a chunk
// into the open run stores no new Entry — the run's end lives on the
// transcript — so every reader must be handed the run as it stood when it
// read. A cut taken between two chunks carries the first chunk's end and
// tail, a later cut the second's, and the earlier cut's entry never changes;
// Entries, Entry and a snapshot agree; main and a child's run alike.
func TestACutBetweenChunksCarriesItsChunksEnd(t *testing.T) {
	for _, who := range []string{"", "sub"} {
		t.Run("agent="+who, func(t *testing.T) {
			m := New(Options{})
			tr := func() *Transcript {
				if who == "" {
					return m.Main
				}
				return m.Sub(who)
			}
			tc := func(c *cut) *transcriptCut {
				if who == "" {
					return &c.main
				}
				for i := range c.subs {
					if c.subs[i].id == who {
						return &c.subs[i].t
					}
				}
				t.Fatalf("the cut has no transcript %q", who)
				return nil
			}
			snapOpen := func(s *Snapshot) Entry {
				ts := &s.Main
				if who != "" {
					for i := range s.Subs {
						if s.Subs[i].ID == who {
							ts = &s.Subs[i].TranscriptSnap
						}
					}
				}
				return ts.Entries[len(ts.Entries)-1]
			}
			m.Fold(agent.Event{Seq: 1, Type: agent.EventText, Agent: who, Text: "one ", At: at(1)})
			m.Fold(agent.Event{Seq: 2, Type: agent.EventText, Agent: who, Text: "two ", At: at(2)})
			c1 := m.cut()
			s1, err := m.Snapshot(0)
			if err != nil {
				t.Fatal(err)
			}
			es1 := tr().Entries()
			e1 := es1[len(es1)-1]
			held := *tc(&c1).entries[0]
			m.Fold(agent.Event{Seq: 3, Type: agent.EventText, Agent: who, Text: "three", At: at(3)})
			c2 := m.cut()
			es2 := tr().Entries()
			byID, ok := tr().Entry(e1.ID)

			first, second := tc(&c1).entries[0], tc(&c2).entries[0]
			if !first.Streaming || !first.End.Equal(at(2)) || first.Text != "one two " || first.Bytes != len("one two ") {
				t.Fatalf("the cut between the chunks: end %v text %q bytes %d, want the second chunk's end %v, %q, 8", first.End, first.Text, first.Bytes, at(2), "one two ")
			}
			if !second.End.Equal(at(3)) || second.Text != "one two three" || second.Bytes != len("one two three") || second.ID != first.ID {
				t.Fatalf("the later cut: end %v text %q bytes %d id %v, want %v, %q, 13, %v", second.End, second.Text, second.Bytes, second.ID, at(3), "one two three", first.ID)
			}
			if *first != held {
				t.Fatalf("the earlier cut's entry changed under a later chunk: %+v, was %+v", *first, held)
			}
			if !e1.Streaming || e1.Text != "" || !e1.End.Equal(at(2)) || e1.Bytes != 8 {
				t.Fatalf("Entries between the chunks: %+v", *e1)
			}
			if e2 := es2[len(es2)-1]; e2 == e1 || !e2.End.Equal(at(3)) || e2.Bytes != 13 || !e1.End.Equal(at(2)) {
				t.Fatalf("Entries after the next chunk: %+v (the earlier read now %+v)", *e2, *e1)
			}
			if !ok || !byID.End.Equal(at(3)) || byID.Bytes != 13 {
				t.Fatalf("Entry(%v): %v %+v", e1.ID, ok, byID)
			}
			if o := snapOpen(s1); !o.End.Equal(at(2)) || o.Text != "one two " {
				t.Fatalf("the snapshot between the chunks carries end %v text %q", o.End, o.Text)
			}
			// Restored between the chunks, the next chunk moves the restored
			// run's end as it does the first model's.
			r := Restore(s1, Options{})
			r.Fold(agent.Event{Seq: 3, Type: agent.EventText, Agent: who, Text: "three", At: at(3)})
			if h, want := r.History(), m.History(); !reflect.DeepEqual(h, want) {
				t.Fatalf("restored between the chunks and folded on:\n got %+v\nwant %+v", h, want)
			}
			// The run closes with the last chunk's end and the whole tail.
			m.Fold(agent.Event{Seq: 4, Type: agent.EventDone, Agent: who, StopReason: "end_turn"})
			if f := facts(tr())[0]; f.Kind != "assistant" || f.Text != "one two three" || !f.End.Equal(at(3)) {
				t.Fatalf("the closed run: %v end %v", f, f.End)
			}
		})
	}
}
