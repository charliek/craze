package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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
			var hookErr error
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
							hookErr = errors.New("the short turn never ended: the schedule was not forced")
							return
						}
						time.Sleep(time.Millisecond)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := eng.Sync(ctx); err != nil {
						hookErr = fmt.Errorf("the turn's events were not all published: %w", err)
						return
					}
					time.Sleep(150 * time.Millisecond)
				},
			})
			if !fellBehind {
				t.Fatal("the runner never fell behind after its Enter")
			}
			if hookErr != nil {
				t.Fatal(hookErr)
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
// runner captures only a model that has folded the stream as far as it had
// gone: the last line is in the frame, in both modes. Quitting at once raced
// the quit against the drain's own commands and the reader.
//
// It pins the capture's fold-at-head half. The settled half — the newest
// frame, never a queued one — is TestTheSettleJudgesTheNewestFrameOnly's, and
// the quit's place in the model's FIFO is TestTheQuitWaitsItsTurn's.
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

// TestAHeldKeysTokenIsAcknowledgedAfterIt (C17c, astra C17b 1): a gate an
// asynchronous message opened — the plan offer's implement prompt, sent when
// its mode change lands — is open when the runner's key and its token arrive,
// as one message. The key is held; its token is held behind it, not parked
// for the release: the release applies the reply alone, and the token is
// acknowledged only once the key is applied. So no barrier ever sees the
// frame before the key — in both modes.
func TestAHeldKeysTokenIsAcknowledgedAfterIt(t *testing.T) {
	for _, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			m, _ := gatedModel(t)
			m.gateSync = mode.sync
			if m.snap.Provider.ImplementPrompt() == "" {
				t.Fatal("the provider has no implement prompt: the plan offer sends nothing")
			}
			f := frameModel{inner: m, bus: newFrameBus(nil)}
			const n = 1
			var calls []tea.Cmd
			drains := 0
			acked := false
			ackedWith := ""
			var collect func(tea.Cmd)
			collect = func(cmd tea.Cmd) {
				if cmd == nil {
					return
				}
				switch name := cmdFuncName(cmd); {
				case strings.HasPrefix(name, teaPkg+"compactCmds"), strings.HasPrefix(name, teaPkg+"Batch"):
					if b, ok := cmd().(tea.BatchMsg); ok {
						for _, c := range b {
							collect(c)
						}
					}
				case name == tuiPkg+"drainNext":
					drains++
				case strings.HasPrefix(name, tuiPkg+"Model.run"):
					calls = append(calls, cmd)
				}
			}
			var step func(tea.Msg)
			step = func(msg tea.Msg) {
				tm, cmd := f.Update(msg)
				f = tm.(frameModel)
				if !acked && f.inner.syncAck >= n {
					acked, ackedWith = true, f.inner.input.Value()
				}
				collect(cmd)
				for drains > 0 {
					drains--
					step(drainMsg{})
				}
			}
			step(planImplementMsg{seq: f.inner.turnSeq, gen: f.inner.modeGen, mode: "agent"})
			if !mode.sync && f.inner.gate == nil {
				t.Fatal("the implement prompt's Submit opened no gate")
			}
			step(frameTokenMsg{msg: runeKey('X'), n: n})
			if !mode.sync {
				if acked || f.inner.syncPending != 0 {
					t.Fatalf("the token was acked (%v) or parked (%d) under a gate its key did not open", acked, f.inner.syncPending)
				}
				if got := heldKinds(f.inner); !slices.Equal(got, []string{"tea.KeyMsg", "tui.frameSyncMsg"}) {
					t.Fatalf("held %v, want the key and its token behind it", got)
				}
			}
			for len(calls) > 0 {
				c := calls[0]
				calls = calls[1:]
				step(runWatched(t, c))
			}
			if !acked {
				t.Fatal("the token was never acknowledged")
			}
			if ackedWith != "X" {
				t.Fatalf("the token was acknowledged with the composer %q: the barrier saw the frame before its key", ackedWith)
			}
		})
	}
}

// TestTheSettleJudgesTheNewestFrameOnly (C17c, astra C17b 3): the capture's
// settle is judged on the newest frame alone. With a settled frame queued and
// a newer unsettled one — a gate an asynchronous message opened after it — it
// waits, and returns once a newer frame is settled again; it never takes the
// queued one.
func TestTheSettleJudgesTheNewestFrameOnly(t *testing.T) {
	b := newFrameBus(nil)
	r := &frameRunner{bus: b, timeout: 200 * time.Millisecond, finished: make(chan struct{})}
	b.publish(frameState{settled: true, folded: 5})
	b.publish(frameState{settled: false, gated: true, folded: 5})
	var te *WaitTimeoutError
	if err := r.settle(5); !errors.As(err, &te) {
		t.Fatalf("settle returned %v with the newest frame unsettled, want its timeout", err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		b.publish(frameState{settled: true, folded: 6})
	}()
	if err := r.settle(5); err != nil {
		t.Fatalf("settle returned %v once the newest frame settled", err)
	}
	// Folded short of the head is not settled either.
	b.publish(frameState{settled: true, folded: 4})
	if err := r.settle(5); !errors.As(err, &te) {
		t.Fatalf("settle returned %v with the newest frame short of the head", err)
	}
}

// slowTitleStub is a Stub whose rename takes a while: a gate that stays open
// long enough for a quit to arrive under it.
type slowTitleStub struct {
	*Stub
	d time.Duration
}

func (s slowTitleStub) SetTitle(cause, title string) error {
	time.Sleep(s.d)
	return s.Stub.SetTitle(cause, title)
}

// TestTheQuitWaitsItsTurn (C17c, astra C17b 3): the runner's quit goes in
// through the model's FIFO. A rename that arrives after the settle and opens a
// gate is still open when the quit arrives; the quit is held behind it, and
// the capture — the frame of the quit's own Update — shows the rename done. A
// quit that bypassed the model would capture the gated frame instead.
func TestTheQuitWaitsItsTurn(t *testing.T) {
	for _, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			isolateSkillsHome(t)
			var hookErr error
			got, _, err := RunFrameScript(Config{
				Session:   slowTitleStub{Stub: NewStub(), d: 300 * time.Millisecond},
				Theme:     "tokyo-night",
				Workspace: frameWorkspace(t),
				Model:     "grok",
				Yolo:      true,
			}, 100, 30, "<wait:idle>/rename late", FrameOpts{
				Timeout:  5 * time.Second,
				gateSync: mode.sync,
				beforeQuit: func(send func(tea.Msg), last func() frameState) {
					send(enter())
					if mode.sync {
						return
					}
					deadline := time.Now().Add(pumpWatchdog)
					for !last().gated {
						if time.Now().After(deadline) {
							hookErr = errors.New("the rename opened no gate")
							return
						}
						time.Sleep(time.Millisecond)
					}
				},
			})
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			if err != nil {
				t.Fatalf("run frame script: %v", err)
			}
			if !strings.Contains(got, "renamed to late") {
				t.Fatalf("the capture came before the rename that arrived ahead of the quit:\n%s", got)
			}
		})
	}
}

// stuckTitleStub is a Stub whose rename blocks until the session is closed:
// in the gateSync baseline, a call blocked inside an Update.
type stuckTitleStub struct {
	*Stub
	closed chan struct{}
	once   *sync.Once
}

func (s stuckTitleStub) SetTitle(cause, title string) error {
	<-s.closed
	return errors.New("stuck: the session closed")
}

func (s stuckTitleStub) Close() error {
	s.once.Do(func() { close(s.closed) })
	return s.Stub.Close()
}

// TestAShutdownIsBoundedWhenACallBlocks (C17c, astra C17b 2): a rename blocked
// until the session closes — inside Enter's Update in the gateSync baseline,
// holding its gate open asynchronously — times the barrier out; the shutdown
// then neither blocks on handing the program its quit nor waits for it without
// end: past its timeout it closes the engine, which unblocks the call, and
// kills the program. RunFrameScript returns the barrier's timeout, promptly.
func TestAShutdownIsBoundedWhenACallBlocks(t *testing.T) {
	for _, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			isolateSkillsHome(t)
			out := make(chan error, 1)
			go func() {
				_, _, err := RunFrameScript(Config{
					Session:   stuckTitleStub{Stub: NewStub(), closed: make(chan struct{}), once: &sync.Once{}},
					Theme:     "tokyo-night",
					Workspace: frameWorkspace(t),
					Model:     "grok",
					Yolo:      true,
				}, 80, 24, "<wait:idle>/rename stuck<enter>", FrameOpts{Timeout: 500 * time.Millisecond, gateSync: mode.sync})
				out <- err
			}()
			select {
			case err := <-out:
				var te *WaitTimeoutError
				if !errors.As(err, &te) {
					t.Fatalf("RunFrameScript returned %v, want the barrier's timeout", err)
				}
			case <-time.After(pumpWatchdog):
				t.Fatal("RunFrameScript hung: the shutdown waited on a program blocked inside an Update")
			}
		})
	}
}

// TestTheCaptureBoundaryIsAnErrorWhenItCannotBeRead (C17c, astra C17b further):
// the stream's head is the capture's boundary. An engine that is closed will
// publish nothing more — head 0, nothing to wait for — but one whose head
// cannot be read in time is an error, never a silent 0 that would let the
// capture be taken short of events already published.
func TestTheCaptureBoundaryIsAnErrorWhenItCannotBeRead(t *testing.T) {
	stub := NewStub()
	eng, err := engine.New(stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	owner := &sessionOwner{}
	owner.set(newEngineBackend(eng))
	// The primary full and nobody reading: the flush cannot finish.
	saturateStub(t, stub)
	if head, err := streamHead(owner, 50*time.Millisecond); err == nil {
		t.Fatalf("an unreadable head answered %d with no error", head)
	}
	if err := eng.Close(); err != nil && !errors.Is(err, agent.ErrAgentExited) {
		t.Fatal(err)
	}
	if head, err := streamHead(owner, 50*time.Millisecond); err != nil || head != 0 {
		t.Fatalf("a closed engine's head is %d, %v; want 0 and no error", head, err)
	}
	if head, err := streamHead(&sessionOwner{}, 50*time.Millisecond); err != nil || head != 0 {
		t.Fatalf("no engine's head is %d, %v; want 0 and no error", head, err)
	}
}
