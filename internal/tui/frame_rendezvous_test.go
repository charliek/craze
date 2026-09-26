package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

// The frame harness's rendezvous (plan 027 C17a, astra C17 1): an Update that
// acknowledges a sync token waits, the program loop blocked, until the runner
// has taken that token's barrier — so the barrier's newest frame is always the
// acknowledgement and every later frame stays queued for the next wait — and
// it is let go on every path the runner can end by.

// TestTheWorkingFrameSurvivesTheTokenBarrier is astra C17 1's schedule through
// the real runner: Enter sends a turn so short that its started and ended are
// on the stream — held behind the gate asynchronously — before the runner
// takes Enter's barrier. The runner is held back (beforeBarrier) until the
// engine has finished the turn and published every event of it, and a while
// longer: a program that reduced on meanwhile would have drained the ending,
// left an idle newest frame for the barrier to match, and the barrier would
// have cleared the working frame with the rest. With the rendezvous the
// program waits at the acknowledgement, so <wait:working> finds a working
// frame — in both gate modes.
func TestTheWorkingFrameSurvivesTheTokenBarrier(t *testing.T) {
	for _, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			isolateSkillsHome(t)
			stub := NewStub()
			var eng *engine.Engine
			held := false
			_, _, err := RunFrameScript(Config{
				Session:   stub,
				Theme:     "tokyo-night",
				Workspace: frameWorkspace(t),
				Model:     "grok",
				Yolo:      true,
				OnEngine:  func(e *engine.Engine) { eng = e },
			}, 80, 24, "<wait:idle>hi<enter><wait:working>", FrameOpts{
				Timeout:  3 * time.Second,
				gateSync: mode.sync,
				beforeBarrier: func(tok string, _ int, _ func() frameState) error {
					if tok != "<enter>" {
						return nil
					}
					held = true
					// The whole turn, over at the engine and on the stream.
					deadline := time.Now().Add(5 * time.Second)
					for len(stub.Prompts()) == 0 || stub.InTurn() || eng.State().Turn != "" {
						if time.Now().After(deadline) {
							return errors.New("the short turn never ended")
						}
						time.Sleep(time.Millisecond)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := eng.Sync(ctx); err != nil {
						return err
					}
					// And the time a program running on would take to reduce it.
					time.Sleep(150 * time.Millisecond)
					return nil
				},
			})
			if !held {
				t.Fatal("the runner was never held back at Enter's barrier")
			}
			if err != nil {
				t.Fatalf("the script failed: %v — the barrier cleared the working frame", err)
			}
		})
	}
}

// TestTheRendezvousLetsGoWhenTheRunnerStops: a program waiting at a
// rendezvous is let go however the runner ends. Here the runner gives up at
// Enter's barrier while the program is waiting there for it (the hook returns
// only once the acknowledgement is published); RunFrameScript still quits the
// program and returns the runner's error, promptly. A wait that times out and
// a script that quits end promptly too.
func TestTheRendezvousLetsGoWhenTheRunnerStops(t *testing.T) {
	run := func(t *testing.T, script string, opts FrameOpts) error {
		t.Helper()
		isolateSkillsHome(t)
		out := make(chan error, 1)
		go func() {
			_, _, err := RunFrameScript(Config{
				Session:   NewStub(),
				Theme:     "tokyo-night",
				Workspace: frameWorkspace(t),
				Model:     "grok",
				Yolo:      true,
			}, 80, 24, script, opts)
			out <- err
		}()
		select {
		case err := <-out:
			return err
		case <-time.After(pumpWatchdog):
			t.Fatalf("RunFrameScript hung: a program waiting at a rendezvous was never let go")
			return nil
		}
	}
	for _, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Run("the runner gives up at a barrier", func(t *testing.T) {
				gaveUp := errors.New("the runner gave up")
				err := run(t, "<wait:idle>hi<enter><wait:idle>", FrameOpts{
					Timeout:  3 * time.Second,
					gateSync: mode.sync,
					beforeBarrier: func(tok string, n int, last func() frameState) error {
						if tok != "<enter>" {
							return nil
						}
						// Until the program has acknowledged the token, and so is
						// waiting at the rendezvous for this barrier.
						for last().sync < n {
							time.Sleep(time.Millisecond)
						}
						return gaveUp
					},
				})
				if !errors.Is(err, gaveUp) {
					t.Fatalf("RunFrameScript returned %v, want the runner's own error", err)
				}
			})
			t.Run("a wait that times out", func(t *testing.T) {
				var te *WaitTimeoutError
				if err := run(t, "<wait:idle>hi<enter><wait:text:never drawn>", FrameOpts{Timeout: 500 * time.Millisecond, gateSync: mode.sync}); !errors.As(err, &te) {
					t.Fatalf("RunFrameScript returned %v, want the wait's timeout", err)
				}
			})
			t.Run("a script that quits", func(t *testing.T) {
				if err := run(t, "<wait:idle>hi<enter><wait:idle><ctrl-d><sleep:50ms><esc>", FrameOpts{Timeout: 3 * time.Second, gateSync: mode.sync}); err != nil {
					t.Fatalf("a quitting script returned %v", err)
				}
			})
		})
	}
}

// TestTheRendezvousWaitsForItsBarrier: at the bus, a program waiting for token
// n's barrier waits past an older token's and is let go by its own, or by the
// runner finishing.
func TestTheRendezvousWaitsForItsBarrier(t *testing.T) {
	b := newFrameBus(nil)
	met := make(chan struct{})
	go func() {
		b.meet(3)
		close(met)
	}()
	b.take(2)
	select {
	case <-met:
		t.Fatal("the rendezvous for token 3 let go at token 2's barrier")
	case <-time.After(50 * time.Millisecond):
	}
	b.take(3)
	select {
	case <-met:
	case <-time.After(pumpWatchdog):
		t.Fatal("the rendezvous for token 3 was not let go by its barrier")
	}

	b2 := newFrameBus(nil)
	met2 := make(chan struct{})
	go func() {
		b2.meet(1)
		close(met2)
	}()
	b2.finish()
	select {
	case <-met2:
	case <-time.After(pumpWatchdog):
		t.Fatal("the rendezvous was not let go when the runner finished")
	}
	b2.meet(7) // a finished runner never holds a program again
}

// TestAKeyAndItsTokenAreOneMessage (C17b, astra r43 1): a key token and its
// sync token reach the program as one message, so a runner that falls behind
// right after handing the program its key cannot let the program reduce the
// key's whole short turn before the token: the token is acknowledged on the
// frame right after the key's own (or at the release of the gate the key
// opened), and the program waits there. Here the runner falls behind right
// after its Enter send (afterSend) until the engine has finished the turn and
// published all of it; <wait:working> still finds the working frame, in both
// gate modes. Every key the runner sent went as one message with its token.
func TestAKeyAndItsTokenAreOneMessage(t *testing.T) {
	isEnter := func(msg tea.Msg) bool {
		if tok, ok := msg.(frameTokenMsg); ok {
			msg = tok.msg
		}
		k, ok := msg.(tea.KeyMsg)
		return ok && k.Type == tea.KeyEnter
	}
	for _, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			isolateSkillsHome(t)
			stub := NewStub()
			var eng *engine.Engine
			var bare []string
			fellBehind := false
			_, _, err := RunFrameScript(Config{
				Session:   stub,
				Theme:     "tokyo-night",
				Workspace: frameWorkspace(t),
				Model:     "grok",
				Yolo:      true,
				OnEngine:  func(e *engine.Engine) { eng = e },
			}, 80, 24, "<wait:idle>hi<enter><wait:working>", FrameOpts{
				Timeout:  3 * time.Second,
				gateSync: mode.sync,
				afterSend: func(msg tea.Msg) {
					switch msg.(type) {
					case frameTokenMsg, frameSyncMsg:
					default:
						bare = append(bare, fmt.Sprintf("%T", msg))
					}
					if !isEnter(msg) {
						return
					}
					fellBehind = true
					deadline := time.Now().Add(5 * time.Second)
					for len(stub.Prompts()) == 0 || stub.InTurn() || eng.State().Turn != "" {
						if time.Now().After(deadline) {
							return
						}
						time.Sleep(time.Millisecond)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = eng.Sync(ctx)
					time.Sleep(150 * time.Millisecond)
				},
			})
			if !fellBehind {
				t.Fatal("the runner never fell behind after its Enter")
			}
			if len(bare) != 0 {
				t.Fatalf("the runner sent %v without its sync token", bare)
			}
			if err != nil {
				t.Fatalf("the script failed: %v — the program reduced the short turn before Enter's token", err)
			}
		})
	}
}

// TestAPanickingHookStillEndsTheRun (C17b, astra r43 further): a runner that
// panics — a test's hook, while the program waits at the rendezvous for it —
// still lets the program go, quits it and closes the engine, before the panic
// reaches the caller.
func TestAPanickingHookStillEndsTheRun(t *testing.T) {
	for _, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			isolateSkillsHome(t)
			sess := newCloseCounter()
			out := make(chan any, 1)
			go func() {
				defer func() { out <- recover() }()
				_, _, _ = RunFrameScript(Config{
					Session:   sess,
					Theme:     "tokyo-night",
					Workspace: frameWorkspace(t),
					Model:     "grok",
					Yolo:      true,
				}, 80, 24, "<wait:idle>hi<enter><wait:idle>", FrameOpts{
					Timeout:  3 * time.Second,
					gateSync: mode.sync,
					beforeBarrier: func(tok string, n int, last func() frameState) error {
						if tok != "<enter>" {
							return nil
						}
						for last().sync < n {
							time.Sleep(time.Millisecond)
						}
						panic("the hook panicked")
					},
				})
			}()
			select {
			case v := <-out:
				if v != "the hook panicked" {
					t.Fatalf("RunFrameScript ended with %#v, want the hook's panic", v)
				}
			case <-time.After(pumpWatchdog):
				t.Fatal("RunFrameScript hung after its hook panicked: the program was never let go")
			}
			if n := sess.closes.Load(); n != 1 {
				t.Fatalf("the session was closed %d times after the hook's panic, want once", n)
			}
		})
	}
}

// chattyStub is a Stub whose rename publishes lines lines of reply first: a
// gated /rename whose call leaves the reader a long stream to hold or read.
type chattyStub struct {
	*Stub
	lines int
}

func (s chattyStub) SetTitle(cause, title string) error {
	for i := range s.lines {
		s.Emit(agent.Event{Type: agent.EventText, Text: fmt.Sprintf("line %02d\n", i+1)})
	}
	return s.Stub.SetTitle(cause, title)
}

// TestTheCaptureWaitsForEverythingThatArrived (C17b): the script ends the
// moment the rename's note is drawn, with the sixty events its call published
// still to reduce — held behind the gate and drained one per Update
// asynchronously, still on the stream for the reader in the baseline. The
// runner captures only a settled model that has folded the stream as far as
// it had gone: the last line is in the frame, in both modes. Quitting at once
// raced the quit against the drain's own commands and the reader.
func TestTheCaptureWaitsForEverythingThatArrived(t *testing.T) {
	isolateSkillsHome(t)
	got, _, err := runFrameModes(t, func() Config {
		return Config{
			Session:   chattyStub{Stub: NewStub(), lines: 60},
			Theme:     "tokyo-night",
			Workspace: frameWorkspace(t),
			Model:     "grok",
			Yolo:      true,
		}
	}, 100, 30, "<wait:idle>/rename chatty<enter><wait:text:renamed to chatty>", FrameOpts{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	if !strings.Contains(got, "line 60") {
		t.Fatalf("the capture came before everything that had arrived was reduced:\n%s", got)
	}
}
