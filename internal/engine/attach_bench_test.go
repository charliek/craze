package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/journal"
)

// benchSession is newFake for a benchmark: the same double, over a log with the
// given options, closed when the benchmark ends.
func benchSession(b *testing.B, o agent.EventLogOptions) *fakeSession {
	b.Helper()
	log := agent.NewEventLog(o)
	s := &fakeSession{
		log:       log,
		asks:      agent.NewAskRegistry(log, nil),
		done:      make(chan struct{}),
		cancelSig: make(chan struct{}, 1),
		clock:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

// BenchmarkEnginePublishObserved is V7 with the real fold (plan 024 §3.4, A6):
// internal/agent's BenchmarkEventLogPublishObserved, whose observer stores one
// integer, run instead through a log whose observer is the engine's own —
// e.model.Fold first, then the engine's flags — over a live primary kept
// drained, as the TUI keeps it. The budget is the same: a text delta with the
// journal attached under 5 µs. The attaching variant runs an attach loop on
// another goroutine the whole time — each iteration a snapshot cut under the
// model's lock and a Subscribe inside the boundary — so it measures what a
// client attaching costs the publisher (the lock-hold bound's other side).
//
// observer=int is the control: the same harness, session and journal with no
// engine, the log's observer storing one integer as internal/agent's does, so
// the difference between it and the engine's run in one invocation of this
// binary is the fold's, and the rest is the harness and the machine.
func BenchmarkEnginePublishObserved(b *testing.B) {
	ev := agent.Event{Type: agent.EventText, Text: "Here is the next chunk of the answer, ", At: time.Now()}
	for _, tc := range []struct {
		journaled, attaching, control bool
	}{{false, false, false}, {true, false, true}, {true, false, false}, {true, true, false}} {
		name := fmt.Sprintf("TextDelta/journal=%v/attaching=%v", tc.journaled, tc.attaching)
		if tc.control {
			name = fmt.Sprintf("TextDelta/journal=%v/observer=int", tc.journaled)
		}
		b.Run(name, func(b *testing.B) {
			var lo agent.EventLogOptions
			var w *journal.Writer
			if tc.journaled {
				inc := agent.NewIncarnation()
				var err error
				w, err = journal.New(journal.Options{
					Dir: filepath.Join(b.TempDir(), "journal"), Incarnation: inc, Cwd: b.TempDir(),
					Provider: "bench", EventCodec: agent.EventCodecVersion,
				})
				if err != nil {
					b.Fatal(err)
				}
				lo = agent.EventLogOptions{Journal: w, Incarnation: inc}
			}
			s := benchSession(b, lo)
			var e *Engine
			var seen uint64
			if tc.control {
				if err := s.log.Observe(func(ev agent.Event) { seen = ev.Seq }); err != nil {
					b.Fatal(err)
				}
			} else {
				var err error
				if e, err = newEngine(s, Options{}, nil); err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { _ = e.Close() })
				if err := e.Start(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
			stop := make(chan struct{})
			var wg sync.WaitGroup
			wg.Go(func() {
				for {
					select {
					case <-s.log.Primary():
					case <-stop:
						return
					}
				}
			})
			attaches := 0
			if tc.attaching {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				wg.Go(func() {
					for {
						select {
						case <-stop:
							return
						default:
						}
						a, err := e.Attach(ctx, AttachOptions{})
						if err != nil {
							return
						}
						a.Sub.Close()
						attaches++
					}
				})
			}
			b.ReportAllocs()
			for b.Loop() {
				if !s.log.Publish(context.Background(), nil, ev) {
					b.Fatal("Publish returned false")
				}
			}
			b.StopTimer()
			close(stop)
			wg.Wait()
			if tc.control && seen == 0 || !tc.control && e.model.Seq() == 0 {
				b.Fatal("the observer never ran")
			}
			if tc.attaching {
				b.ReportMetric(float64(attaches)/float64(b.N), "attaches/op")
			}
			if w != nil {
				b.ReportMetric(float64(w.Health().DroppedEvents)/float64(b.N), "journal-drops/op")
			}
		})
	}
}
