package transcript

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// TestTailGenNamesAnAppendOnlyTail is TailGen's contract (plan 032 §3.3 C6):
// under one generation the tail only grows by appends — every read is a prefix
// of every later read under it — and the generation changes on everything
// else: a run opening (after a tool row, a done, a run of another kind), every
// chunk once the run is past the cap, a run closing (0 with no run open). A
// generation is never handed out again once another has replaced it. Random
// streams, small caps (the cap's moving head) and the default one.
func TestTailGenNamesAnAppendOnlyTail(t *testing.T) {
	runes := []string{"a", "é", "⤷", "😀", "\n", " ", "|", "`"}
	for _, limit := range []int{4, 9, 64, 64 << 10} {
		for seed := range uint64(8) {
			r := rand.New(rand.NewPCG(seed, uint64(limit)))
			m := New(Options{Bounds: Bounds{StreamText: limit, MainBytes: 1 << 30}})
			tr := m.Main
			var (
				prevTail string
				prevGen  uint64
				whole    strings.Builder // the open run's whole text
				retired  = map[uint64]bool{}
			)
			// stream folds a chunk of kind k, keeping whole the open run's text.
			stream := func(typ agent.EventType, k Kind, text string) {
				if last := tr.lastEntry(); last == nil || !tr.streamOpen || last.Kind != k {
					whole.Reset()
				}
				m.Fold(agent.Event{Type: typ, Text: text})
				whole.WriteString(text)
			}
			for step := range 400 {
				switch r.IntN(20) {
				case 0:
					m.Fold(agent.Event{Type: agent.EventDone})
					whole.Reset()
				case 1:
					m.Fold(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t", Title: "x", Status: "completed"}})
					whole.Reset()
				case 2:
					// A run of another kind ends this one and opens its own.
					stream(agent.EventThought, KindThought, "hm")
				default:
					var chunk strings.Builder
					// Mostly token-sized, now and then up to twice the cap.
					n := 1 + r.IntN(9)
					if r.IntN(8) == 0 {
						n = 1 + r.IntN(2*limit+2)
					}
					for chunk.Len() < n {
						chunk.WriteString(runes[r.IntN(len(runes))])
					}
					stream(agent.EventText, KindAssistant, chunk.String())
				}
				tail, gen := tr.TailGen()
				if tail != tr.Tail() {
					t.Fatalf("limit %d seed %d step %d: TailGen's tail %q, Tail %q", limit, seed, step, tail, tr.Tail())
				}
				switch {
				case !tr.StreamOpen():
					if gen != 0 || tail != "" {
						t.Fatalf("limit %d seed %d step %d: no run open, TailGen = %q, %d", limit, seed, step, tail, gen)
					}
				case gen == 0:
					t.Fatalf("limit %d seed %d step %d: an open run's generation is 0", limit, seed, step)
				case retired[gen]:
					t.Fatalf("limit %d seed %d step %d: generation %d was handed out again", limit, seed, step, gen)
				}
				if gen != 0 && gen == prevGen && !strings.HasPrefix(tail, prevTail) {
					t.Fatalf("limit %d seed %d step %d: generation %d held while the tail went %q → %q", limit, seed, step, gen, prevTail, tail)
				}
				if gen != 0 && gen == prevGen && whole.Len() > limit {
					t.Fatalf("limit %d seed %d step %d: generation %d held over a chunk past the cap", limit, seed, step, gen)
				}
				if prevGen != 0 && gen != prevGen {
					retired[prevGen] = true
				}
				prevTail, prevGen = tail, gen
			}
		}
	}
}

// TestTailGenOfARestoredRun: a run restored from a snapshot — cut or not — has
// a generation of its own, never its first model's, and keeps it while the
// restored tail grows by appends; the next chunk past the cap changes it.
func TestTailGenOfARestoredRun(t *testing.T) {
	for _, cut := range []bool{false, true} {
		m := New(Options{Bounds: Bounds{StreamText: 16, MainBytes: 1 << 30}})
		text := "0123456789"
		if cut {
			text = strings.Repeat("x", 40)
		}
		m.Fold(agent.Event{Type: agent.EventText, Text: text, Seq: 1})
		_, first := m.Main.TailGen()
		s, err := m.Snapshot(0)
		if err != nil {
			t.Fatal(err)
		}
		r := Restore(s, Options{Bounds: Bounds{StreamText: 16, MainBytes: 1 << 30}})
		tail, gen := r.Main.TailGen()
		if gen == 0 || gen == first || tail != m.Main.Tail() {
			t.Fatalf("cut %v: the restored run's TailGen = %q, %d (its first model's %q, %d)", cut, tail, gen, m.Main.Tail(), first)
		}
		r.Fold(agent.Event{Type: agent.EventText, Text: "ab", Seq: 2})
		next, again := r.Main.TailGen()
		if cut {
			if again == gen {
				t.Fatalf("a chunk into a restored run past the cap kept generation %d: %q → %q", gen, tail, next)
			}
			continue
		}
		if again != gen || next != tail+"ab" {
			t.Fatalf("a chunk into the restored run: %q, %d → %q, %d", tail, gen, next, again)
		}
	}
}
