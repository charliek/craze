package transcript

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// allocsOf is the mean number of heap allocations op makes, over n runs, with
// setup run before each and not counted: testing.AllocsPerRun for an op whose
// state has to be put back between runs. It runs on one P, as AllocsPerRun
// does.
//
// One P does not make the count the op's alone: MemStats.Mallocs is the
// process's, and the runtime's own goroutines — and one an earlier test left
// behind — still run and allocate between the two reads. So the mean is taken
// as AllocsPerRun takes it, by integer division: a stray allocation in 512 runs
// is a remainder and drops out, where a float mean read 3.002 against a bound
// of 3 and failed (macOS CI, PR #51: logged "3.00", "the bound is 3"); an op
// that really allocates once more every run still counts in full.
func allocsOf(n int, setup, op func()) float64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var before, after runtime.MemStats
	var total uint64
	for range n {
		if setup != nil {
			setup()
		}
		runtime.ReadMemStats(&before)
		op()
		runtime.ReadMemStats(&after)
		total += after.Mallocs - before.Mallocs
	}
	return float64(total / uint64(n))
}

// TestFoldAllocationsAreBounded (plan 024 §3.4, A6): what one Fold may
// allocate on the hot path, with the builder and the maps warm — a chunk into
// an open run 0 (execution amendment X24: no replacing Entry, the run's end
// lives on the transcript; the builder's growth amortised to 0),
// a new entry ≤ 3, a tool upsert ≤ 3 (the updated row, and the run it closes
// with its tail), a state delta ≤ 1 per section. The measured numbers are
// logged.
func TestFoldAllocationsAreBounded(t *testing.T) {
	if testing.CoverMode() != "" || raceEnabled {
		t.Skip("allocation counts are not meaningful under -cover or -race")
	}
	var seq uint64
	next := func(ev agent.Event) agent.Event {
		seq++
		ev.Seq = seq
		return ev
	}
	chunk := strings.Repeat("x", 64)

	t.Run("a chunk into an open run", func(t *testing.T) {
		m := New(Options{})
		// Warm the builder past its 2 × StreamText cap, so the in-place
		// compaction is part of the steady state being measured.
		for range 4096 {
			m.Fold(next(agent.Event{Type: agent.EventText, Text: chunk}))
		}
		got := testing.AllocsPerRun(4096, func() { m.Fold(next(agent.Event{Type: agent.EventText, Text: chunk})) })
		t.Logf("a chunk into an open run: %.2f allocs", got)
		if got != 0 {
			t.Fatalf("a chunk into an open run allocates %.2f, the bound is 0 (X24)", got)
		}
		child := testing.AllocsPerRun(4096, func() { m.Fold(next(agent.Event{Type: agent.EventText, Agent: "sub", Text: chunk})) })
		t.Logf("a chunk into a child's open run: %.2f allocs", child)
		if child != 0 {
			t.Fatalf("a chunk into a child's run allocates %.2f, the bound is 0 (X24)", child)
		}
	})

	t.Run("a new entry", func(t *testing.T) {
		m := New(Options{})
		done := agent.Event{Type: agent.EventDone, StopReason: "cancelled"}
		// Fill past the entry cap, so the maps and the deque are at steady state
		// and every note also trims one.
		for range 6000 {
			m.Fold(next(done))
		}
		note := testing.AllocsPerRun(4096, func() { m.Fold(next(done)) })
		started := &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "go"}
		user := testing.AllocsPerRun(4096, func() { m.Fold(next(agent.Event{Type: agent.EventTurn, Turn: started})) })
		// Alternating kinds: every chunk closes the other run (its Entry and its
		// tail) and opens one.
		kinds := [2]agent.EventType{agent.EventThought, agent.EventText}
		i := 0
		run := testing.AllocsPerRun(4096, func() {
			i++
			m.Fold(next(agent.Event{Type: kinds[i%2], Text: chunk}))
		})
		// Alternating 32 KiB chunks (r2 finding 7): every chunk closes a run
		// whose builder is past StreamText/4, which C1 let go and grew again
		// for the next run — a fourth allocation. The builder is kept across
		// runs now: the closed Entry, its tail's copy, and the new Entry.
		long := strings.Repeat("y", 32<<10)
		big := New(Options{})
		for j := range 64 {
			big.Fold(next(agent.Event{Type: kinds[j%2], Text: long}))
		}
		j := 0
		longRun := testing.AllocsPerRun(1024, func() {
			j++
			big.Fold(next(agent.Event{Type: kinds[j%2], Text: long}))
		})
		t.Logf("a new note: %.2f allocs; a started's user row: %.2f; a run closing into a new one: %.2f; the same with 32 KiB chunks: %.2f", note, user, run, longRun)
		for name, got := range map[string]float64{"a note": note, "a user row": user, "a new run": run, "a new run after a 32 KiB one": longRun} {
			if got > 3 {
				t.Fatalf("%s allocates %.2f, the bound is 3", name, got)
			}
		}
	})

	t.Run("a tool upsert", func(t *testing.T) {
		m := New(Options{})
		a, b := fullTool("t"), fullTool("t")
		m.Fold(next(agent.Event{Type: agent.EventTool, Tool: a}))
		flip := false
		tool := func() agent.Event {
			flip = !flip
			if flip {
				return next(agent.Event{Type: agent.EventTool, Tool: b})
			}
			return next(agent.Event{Type: agent.EventTool, Tool: a})
		}
		inPlace := testing.AllocsPerRun(4096, func() { m.Fold(tool()) })
		// The update that also closes a thought run above it: the thought is
		// opened in setup, uncounted.
		closing := allocsOf(512,
			func() { m.Fold(next(agent.Event{Type: agent.EventThought, Text: chunk})) },
			func() { m.Fold(tool()) })
		// A new tool row: a fresh id each time, at the entry cap.
		n := New(Options{Bounds: Bounds{MainEntries: 256}})
		ids := make([]*agent.ToolEvent, 8192)
		for i := range ids {
			ids[i] = &agent.ToolEvent{ID: fmt.Sprintf("tool-%d", i)}
		}
		k := 0
		for range 1024 {
			n.Fold(next(agent.Event{Type: agent.EventTool, Tool: ids[k%len(ids)]}))
			k++
		}
		fresh := testing.AllocsPerRun(4096, func() {
			n.Fold(next(agent.Event{Type: agent.EventTool, Tool: ids[k%len(ids)]}))
			k++
		})
		t.Logf("a tool update in place: %.2f allocs; one that closes a run: %.2f; a new tool row: %.2f", inPlace, closing, fresh)
		for name, got := range map[string]float64{"an update in place": inPlace, "an update closing a run": closing, "a new tool row": fresh} {
			if got > 3 {
				t.Fatalf("%s allocates %.2f, the bound is 3", name, got)
			}
		}
	})

	t.Run("a state delta", func(t *testing.T) {
		m := New(Options{})
		six := &agent.StateDelta{
			Title: strp("t"), Mode: strp("agent"), Model: strp("m"),
			Config:   &agent.ConfigState{Options: []agent.ConfigOption{{ID: "effort"}}},
			Commands: &agent.CommandsState{Commands: []agent.CommandInfo{{Name: "c"}}},
			Plugins:  &agent.PluginsState{Plugins: []agent.PluginCommand{{Qualified: "p:q"}}},
		}
		got := testing.AllocsPerRun(4096, func() { m.Fold(next(agent.Event{Type: agent.EventMeta, State: six})) })
		t.Logf("a delta of six sections: %.2f allocs", got)
		if got > 6 {
			t.Fatalf("a six-section delta allocates %.2f, the bound is 1 per section", got)
		}
	})
}
