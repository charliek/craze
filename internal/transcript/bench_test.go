package transcript

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// BenchmarkFold is the fold's cost on the hot path (plan 024 §3.4, §3.7): a
// text chunk into an open run, a saturated stream (every chunk past the
// 2 × StreamText cap pays its share of the in-place copy), a saturated run of
// bytes that are not UTF-8 grown a byte at a time, trimming at the
// entry cap and at the byte budget, a tool update at the payload caps, a delta
// of six sections, a chunk while another goroutine cuts snapshots, and the cut
// itself on a transcript at its entry cap.
func BenchmarkFold(b *testing.B) {
	var seq uint64
	next := func(ev agent.Event) agent.Event {
		seq++
		ev.Seq = seq
		return ev
	}
	chunk := strings.Repeat("x", 64)

	b.Run("chunk", func(b *testing.B) {
		m := New(Options{})
		ev := agent.Event{Type: agent.EventText, Text: chunk}
		done := agent.Event{Type: agent.EventDone}
		b.ReportAllocs()
		for i := 0; b.Loop(); i++ {
			if i%512 == 0 {
				// A reply of 32 KiB, then the next one: under the cap.
				m.Fold(next(done))
			}
			m.Fold(next(ev))
		}
	})

	b.Run("saturated-stream", func(b *testing.B) {
		m := New(Options{})
		ev := agent.Event{Type: agent.EventText, Text: strings.Repeat("y", 4<<10)}
		b.SetBytes(4 << 10)
		b.ReportAllocs()
		for b.Loop() {
			m.Fold(next(ev))
		}
	})

	// r2 finding 4's schedule: a run of 2 × StreamText continuation bytes, grown
	// a continuation byte at a time, so the tail's cut never finds a rune
	// start. Each chunk must cost what the ASCII chunk does, not a rescan of
	// the ~64 KiB behind the cut.
	b.Run("saturated-malformed", func(b *testing.B) {
		m := New(Options{})
		m.Fold(next(agent.Event{Type: agent.EventText, Text: strings.Repeat("\x80", 2*DefaultBounds().StreamText)}))
		ev := agent.Event{Type: agent.EventText, Text: "\x80"}
		b.ReportAllocs()
		for b.Loop() {
			m.Fold(next(ev))
		}
	})

	b.Run("trim-at-entry-cap", func(b *testing.B) {
		m := New(Options{})
		ev := agent.Event{Type: agent.EventDone, StopReason: "cancelled"}
		for range DefaultBounds().MainEntries {
			m.Fold(next(ev))
		}
		b.ReportAllocs()
		for b.Loop() {
			m.Fold(next(ev))
		}
	})

	b.Run("trim-at-byte-cap", func(b *testing.B) {
		m := New(Options{})
		tools := make([]*agent.ToolEvent, 64)
		for i := range tools {
			tools[i] = fullTool(fmt.Sprintf("t%d", i)) // ~1 MiB each: 8 fit
		}
		for i := range 16 {
			m.Fold(next(agent.Event{Type: agent.EventTool, Tool: tools[i]}))
		}
		b.ReportAllocs()
		for i := 0; b.Loop(); i++ {
			m.Fold(next(agent.Event{Type: agent.EventTool, Tool: tools[i%len(tools)]}))
		}
	})

	b.Run("tool-update-at-cap", func(b *testing.B) {
		m := New(Options{})
		a, c := fullTool("t"), fullTool("t")
		m.Fold(next(agent.Event{Type: agent.EventTool, Tool: a}))
		b.ReportAllocs()
		for i := 0; b.Loop(); i++ {
			tl := a
			if i%2 == 0 {
				tl = c
			}
			m.Fold(next(agent.Event{Type: agent.EventTool, Tool: tl}))
		}
	})

	b.Run("delta-six-sections", func(b *testing.B) {
		m := New(Options{})
		st := &agent.StateDelta{
			Title: strp("t"), Mode: strp("agent"), Model: strp("m"),
			Config:   &agent.ConfigState{Options: []agent.ConfigOption{{ID: "effort"}}},
			Commands: &agent.CommandsState{Commands: []agent.CommandInfo{{Name: "c"}}},
			Plugins:  &agent.PluginsState{Plugins: []agent.PluginCommand{{Qualified: "p:q"}}},
		}
		ev := agent.Event{Type: agent.EventMeta, State: st}
		b.ReportAllocs()
		for b.Loop() {
			m.Fold(next(ev))
		}
	})

	b.Run("chunk-racing-cuts", func(b *testing.B) {
		m := New(Options{})
		for range 1000 {
			m.Fold(next(agent.Event{Type: agent.EventDone, StopReason: "cancelled"}))
		}
		ev := agent.Event{Type: agent.EventText, Text: chunk}
		var stop atomic.Bool
		var cuts atomic.Int64
		var wg sync.WaitGroup
		wg.Go(func() {
			for !stop.Load() {
				_ = m.cut()
				cuts.Add(1)
			}
		})
		b.ReportAllocs()
		for i := 0; b.Loop(); i++ {
			if i%512 == 0 {
				m.Fold(next(agent.Event{Type: agent.EventDone}))
			}
			m.Fold(next(ev))
		}
		stop.Store(true)
		wg.Wait()
		b.ReportMetric(float64(cuts.Load()), "cuts")
	})

	b.Run("cut-at-entry-cap", func(b *testing.B) {
		m := New(Options{})
		for i := range DefaultBounds().MainEntries {
			m.Fold(next(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: fmt.Sprint(i)}}))
		}
		m.Fold(next(agent.Event{Type: agent.EventText, Text: strings.Repeat("z", 100<<10)}))
		b.ReportAllocs()
		for b.Loop() {
			_ = m.cut()
		}
	})
}
