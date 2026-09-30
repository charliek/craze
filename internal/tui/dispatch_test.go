package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/roster"
)

// Starting a session from the list (plan 030 §3.13, §3.18 PR 3, C15): every
// schedule the brief names, forced one step at a time — never by repetition
// or sleeping — against a fake Sessions and a fake host backend, each step
// bounded on its own (runWatched, awaitStep): a dispatch accepted, refused,
// its outcome unknown; enter twice; a spawn, dial or start that fails; a
// stream that fills before Start returns; a quit landing mid-dispatch; the
// unstarted session opened, adopted on its first prompt with the TUI's own
// command numbering, put back by a failure, and abandoned.

// ---------------------------------------------------------------- fixtures

// callLog is the order things happened in, across the fake Sessions and the
// fake host backend a dispatch meets.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(what string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, what)
}

func (l *callLog) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// hostBackend is a new session's host as a dispatch — or an unstarted
// session's adoption — meets it over the connection Open answered: a stream
// held in a bounded queue (the host's, which a start that publishes more than
// it holds waits on), a Start that publishes fill items before it answers —
// and then, with startGate set, waits for its answer — and a Submit that
// records the command it was sent under and answers what submit says (an
// accepted turn when nil). Close ends the stream. A method nothing here
// expects panics (the nil embedded interface).
type hostBackend struct {
	backend.Backend
	info      backend.SessionInfo
	log       *callLog
	stream    chan backend.Item
	closed    chan struct{}
	once      sync.Once
	fill      int
	startErr  error
	startGate chan error
	// startIn is closed as Start begins: a test holds the schedule there.
	startIn chan struct{}
	submit  func(ctx context.Context, text string) (engine.SubmitResult, error)
	reads   atomic.Int32

	mu    sync.Mutex
	cmds  []engine.Command
	texts []string
}

var _ backend.Backend = (*hostBackend)(nil)

func newHostBackend(id, ws string, log *callLog) *hostBackend {
	return &hostBackend{
		info: backend.SessionInfo{CrazeSessionID: id, Incarnation: "inc-" + id, Workspace: ws, Provider: "cursor",
			Label: "cursor", PermissionMode: backend.PermissionPrompt},
		log: log, stream: make(chan backend.Item, 2), closed: make(chan struct{}), startIn: make(chan struct{}),
	}
}

func (b *hostBackend) Info() backend.SessionInfo { return b.info }
func (b *hostBackend) ClientID() string          { return "client-" + b.info.CrazeSessionID }
func (b *hostBackend) Epoch() uint64             { return 1 }
func (b *hostBackend) Started(error)             {}

func (b *hostBackend) Start(ctx context.Context) error {
	b.log.add("start")
	close(b.startIn)
	for i := range b.fill {
		select {
		case b.stream <- backend.Item{Kind: backend.ItemEvent, Event: agent.Event{Seq: uint64(i + 1)}}:
		case <-b.closed:
			return backend.ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if b.startGate != nil {
		select {
		case err := <-b.startGate:
			return err
		case <-b.closed:
			return backend.ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return b.startErr
}

func (b *hostBackend) Read(ctx context.Context) (backend.Item, error) {
	select {
	case it := <-b.stream:
		b.reads.Add(1)
		return it, nil
	default:
	}
	select {
	case it := <-b.stream:
		b.reads.Add(1)
		return it, nil
	case <-b.closed:
		return backend.Item{}, backend.ErrClosed
	case <-ctx.Done():
		return backend.Item{}, ctx.Err()
	}
}

func (b *hostBackend) Submit(ctx context.Context, c engine.Command, text string, mode engine.SubmitMode, fromRow string) (engine.SubmitResult, error) {
	b.log.add("submit")
	b.mu.Lock()
	b.cmds, b.texts = append(b.cmds, c), append(b.texts, text)
	b.mu.Unlock()
	if b.submit != nil {
		return b.submit(ctx, text)
	}
	return engine.SubmitResult{Turn: "turn-1", Text: text}, nil
}

func (b *hostBackend) Close() error {
	b.once.Do(func() {
		b.log.add("close")
		close(b.closed)
	})
	return nil
}

func (b *hostBackend) LastTurn(context.Context) (*engine.LastTurn, error) { return nil, nil }

func (b *hostBackend) sent() ([]engine.Command, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.cmds), slices.Clone(b.texts)
}

func (b *hostBackend) isClosed() bool {
	select {
	case <-b.closed:
		return true
	default:
		return false
	}
}

// hostRef is the ref Spawn answers for host id.
func hostRef(id string) roster.Ref {
	return roster.Ref{Host: roster.Host{ID: "host-" + id, Socket: "/run/craze/" + id + ".sock", CrazeSessionID: id}}
}

// dispatchModel is newSessModel's list, open on richSnapshot with its
// recent directories, whose Spawn answers the host "new" and whose Open
// answers hb, a host backend in ~/projects/lumen; both log to log.
func dispatchModel(t *testing.T, cols, rows int) (Model, *startSessions, *hostBackend, *callLog) {
	t.Helper()
	m, fs, _ := newSessModel(t, cols, rows)
	m = newList(t, m, fs)
	log := &callLog{}
	hb := newHostBackend("new", homePath("projects/lumen"), log)
	fs.note = log.add
	fs.spawn = func(SpawnSpec) (roster.Ref, error) { return hostRef("new"), nil }
	fs.open = func(roster.Ref) (backend.Backend, error) { return hb, nil }
	return m, fs, hb, log
}

// findCmd is the command named part among cmd and the batches it holds —
// expanded only when a batch, never run otherwise — or nil.
func findCmd(cmd tea.Cmd, part string) tea.Cmd {
	if cmd == nil {
		return nil
	}
	name := cmdFuncName(cmd)
	if strings.HasPrefix(name, teaPkg+"compactCmds") || strings.HasPrefix(name, teaPkg+"Batch") {
		if b, ok := cmd().(tea.BatchMsg); ok {
			for _, c := range b {
				if found := findCmd(c, part); found != nil {
					return found
				}
			}
		}
		return nil
	}
	if strings.Contains(name, part) {
		return cmd
	}
	return nil
}

// mustCmd is findCmd, or the test fails.
func mustCmd(t *testing.T, cmd tea.Cmd, part string) tea.Cmd {
	t.Helper()
	c := findCmd(cmd, part)
	if c == nil {
		t.Fatalf("no %s among the commands", part)
	}
	return c
}

// awaitStep waits for ch within a step.
func awaitStep(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(pumpWatchdog):
		t.Fatalf("%s: not within %s", what, pumpWatchdog)
	}
}

// inputRow is the list's input line, as drawn: the third row from the
// bottom, between the rule naming the target and the rule over the hint line.
func inputRow(t *testing.T, m Model) string {
	t.Helper()
	rows := strings.Split(plainView(m), "\n")
	if len(rows) < 3 || !strings.HasPrefix(rows[len(rows)-3], sessInputPrompt) {
		t.Fatalf("no input line:\n%s", plainView(m))
	}
	return strings.TrimRight(rows[len(rows)-3], " ")
}

// dispatched runs a dispatch's command within a step and answers its
// outcome, undelivered.
func dispatched(t *testing.T, cmd tea.Cmd) sessDispatchedMsg {
	t.Helper()
	msg, ok := runWatched(t, cmd).(sessDispatchedMsg)
	if !ok {
		t.Fatal("the dispatch answered no outcome")
	}
	return msg
}

// ------------------------------------------------------ background dispatch

// TestADispatchStartsTheSessionInTheBackground (§3.13, AC10): enter with a
// prompt after a bound `@` token starts the session in the token's
// directory, with the provider, model and permission mode of the session the
// list came from — captured at enter — without leaving the list: the input
// says starting…, a second enter and typing do nothing, and the dispatch
// spawns, opens a connection of its own, starts, prompts with that
// connection's first command id — the prompt without its token — leaves the
// host running and closes the connection, in that order. Accepted, the input is cleared and
// the hint line says where it started.
func TestADispatchStartsTheSessionInTheBackground(t *testing.T) {
	m, fs, hb, log := dispatchModel(t, 100, 30)
	m, _ = typeList(t, m, "@lu")
	m, _ = press(m, enter())
	m, _ = typeList(t, m, "tidy the changelog")
	// The session the list came from, as its host says it (SF-60).
	m.hostPerm = backend.PermissionPrompt
	// What the rule names: the session the list came from's provider and
	// model.
	provider, model := "cursor", m.snap.CurrentModel
	if !strings.Contains(ruleOf(t, m), " · "+provider+" · ") || model == "" {
		t.Fatalf("fixture: the rule %q, the model %q", ruleOf(t, m), model)
	}

	m, cmd := press(m, enter())
	disp := mustCmd(t, cmd, "sessDispatch")
	if m.sessList.in.dispatching == 0 || inputRow(t, m) != sessInputPrompt+dispatchStartingText || !m.sessList.open {
		t.Fatalf("after enter: dispatching %d, input %q, open %v", m.sessList.in.dispatching, inputRow(t, m), m.sessList.open)
	}
	if !strings.Contains(sessHint(m), "starting it in the background") {
		t.Fatalf("the hint line while it starts: %q", sessHint(m))
	}
	// A second enter, and typing, do nothing while it starts.
	again, cmd2 := press(m, enter())
	again, cmd3 := typeList(t, again, "x")
	if findCmd(cmd2, "sessDispatch") != nil || findCmd(cmd3, "sessDispatch") != nil ||
		again.sessList.in.dispatching != m.sessList.in.dispatching || again.sessList.in.ti.Value() != m.sessList.in.ti.Value() {
		t.Fatal("a second enter, or typing, while the first dispatch runs did something")
	}
	// The selection is captured at enter: moving the list changes nothing.
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyDown})

	msg := dispatched(t, disp)
	if msg.out != dispatchAccepted || msg.err != nil {
		t.Fatalf("the outcome %v: %v", msg.out, msg.err)
	}
	if got, want := log.seen(), []string{"spawn", "open", "start", "submit", "leave", "close"}; !slices.Equal(got, want) {
		t.Fatalf("the dispatch ran %v, want %v", got, want)
	}
	if len(fs.spawns) != 1 {
		t.Fatalf("%d spawns", len(fs.spawns))
	}
	spec := fs.spawns[0]
	if spec.Workspace != homePath("projects/lumen") || spec.Provider.Name() != provider || spec.Model != model ||
		spec.PermissionMode != backend.PermissionPrompt {
		t.Fatalf("spawned %+v (provider %s), want ~/projects/lumen, %s, %q, prompt", spec, spec.Provider.Name(), provider, model)
	}
	cmds, texts := hb.sent()
	if !slices.Equal(cmds, []engine.Command{{Client: "client-new", ID: dispatchCommandID}}) || !slices.Equal(texts, []string{"tidy the changelog"}) {
		t.Fatalf("prompted %v with %q, want the connection's own first command and the prompt without its token", cmds, texts)
	}
	if !slices.Equal(fs.leaves, []roster.Ref{hostRef("new")}) || len(fs.stops) != 0 || !hb.isClosed() {
		t.Fatalf("left %v, stopped %v, closed %v", fs.leaves, fs.stops, hb.isClosed())
	}

	tm, _ := m.Update(msg)
	m = tm.(Model)
	if v, _ := inputOf(m); v != "" || m.sessList.in.dispatching != 0 || m.sessList.in.boundTok != "" {
		t.Fatalf("accepted: input %q, dispatching %d, bound %q", v, m.sessList.in.dispatching, m.sessList.in.boundTok)
	}
	if hint := sessHint(m); hint != "started in ~/projects/lumen" {
		t.Fatalf("the hint line %q", hint)
	}
	// And the input takes a new prompt.
	if m, _ = typeList(t, m, "next"); inputRow(t, m) != sessInputPrompt+"next" {
		t.Fatalf("after the outcome the input is %q", inputRow(t, m))
	}
}

// TestADispatchTheSessionRefusesStopsItsHost (§3.13): a prompt the new
// session refuses keeps the input, says why, and stops the host — which was
// never left running. The target here is the selected row's directory.
func TestADispatchTheSessionRefusesStopsItsHost(t *testing.T) {
	m, fs, hb, log := dispatchModel(t, 100, 30)
	hb.submit = func(context.Context, string) (engine.SubmitResult, error) {
		return engine.SubmitResult{}, fmt.Errorf("the host: %w", engine.ErrClosing)
	}
	m = selectKey(t, m, runKey("deps"))
	m, _ = typeList(t, m, "bump them again")
	m, cmd := press(m, enter())
	msg := dispatched(t, mustCmd(t, cmd, "sessDispatch"))
	if msg.out != dispatchRefused {
		t.Fatalf("the outcome %v: %v", msg.out, msg.err)
	}
	if got, want := log.seen(), []string{"spawn", "open", "start", "submit", "close", "stop"}; !slices.Equal(got, want) {
		t.Fatalf("the dispatch ran %v, want %v", got, want)
	}
	if fs.spawns[0].Workspace != homePath("projects/spendwise") || len(fs.leaves) != 0 {
		t.Fatalf("spawned in %q; left %v", fs.spawns[0].Workspace, fs.leaves)
	}
	tm, _ := m.Update(msg)
	m = tm.(Model)
	if v, _ := inputOf(m); v != "bump them again" || m.sessList.in.dispatching != 0 {
		t.Fatalf("refused: input %q, dispatching %d", v, m.sessList.in.dispatching)
	}
	if hint := sessHint(m); hint != "could not start a session in ~/projects/spendwise: "+closingNote {
		t.Fatalf("the hint line %q", hint)
	}
}

// TestADispatchWhoseOutcomeIsUnknownKeepsItsHost (§3.13): the prompt sent and
// no answer — the connection lost after sending (the outcome unknown), or the
// call's deadline passing — keeps the input, says the session may have
// started, and leaves the host running for the list to show.
func TestADispatchWhoseOutcomeIsUnknownKeepsItsHost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		submit func(ctx context.Context, _ string) (engine.SubmitResult, error)
	}{
		{"the connection lost", func(context.Context, string) (engine.SubmitResult, error) {
			return engine.SubmitResult{}, fmt.Errorf("remote: session.prompt: %w", backend.ErrOutcomeUnknown)
		}},
		{"no answer in time", func(ctx context.Context, _ string) (engine.SubmitResult, error) {
			<-ctx.Done()
			return engine.SubmitResult{}, ctx.Err()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := gateDeadline
			gateDeadline = 50 * time.Millisecond
			t.Cleanup(func() { gateDeadline = prev })
			m, fs, hb, log := dispatchModel(t, 100, 30)
			hb.submit = tc.submit
			m = selectKey(t, m, runKey("lumen"))
			m, _ = typeList(t, m, "try this")
			m, cmd := press(m, enter())
			msg := dispatched(t, mustCmd(t, cmd, "sessDispatch"))
			if msg.out != dispatchUnknown {
				t.Fatalf("the outcome %v: %v", msg.out, msg.err)
			}
			if got, want := log.seen(), []string{"spawn", "open", "start", "submit", "leave", "close"}; !slices.Equal(got, want) {
				t.Fatalf("the dispatch ran %v, want %v", got, want)
			}
			if len(fs.stops) != 0 {
				t.Fatalf("stopped %v", fs.stops)
			}
			tm, _ := m.Update(msg)
			m = tm.(Model)
			if v, _ := inputOf(m); v != "try this" || sessHint(m) != dispatchUnknownNote {
				t.Fatalf("unknown: input %q, hint %q", v, sessHint(m))
			}
		})
	}
}

// TestADispatchThatStartsNoSessionSaysWhy (§3.13): the spawn fails — no host
// runs; the dial fails — Open stops a host it cannot reach, the dispatch
// nothing; the start fails, or does not finish within its bound — the
// connection is closed and the host stopped, nothing having been sent. Each
// keeps the input and says why.
func TestADispatchThatStartsNoSessionSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(fs *startSessions, hb *hostBackend)
		calls []string
		why   string
	}{
		{"the spawn fails", func(fs *startSessions, _ *hostBackend) {
			fs.spawn = func(SpawnSpec) (roster.Ref, error) {
				return roster.Ref{}, errors.New("craze: the session host could not start: bad flag")
			}
		}, []string{"spawn"}, "the session host could not start: bad flag"},
		{"the dial fails", func(fs *startSessions, _ *hostBackend) {
			fs.open = func(roster.Ref) (backend.Backend, error) {
				return nil, errors.New("craze: that session cannot be reached")
			}
		}, []string{"spawn", "open"}, "that session cannot be reached"},
		{"the start fails", func(_ *startSessions, hb *hostBackend) {
			hb.startErr = errors.New("authentication required")
		}, []string{"spawn", "open", "start", "close", "stop"}, "authentication required"},
		{"the start takes too long", func(_ *startSessions, hb *hostBackend) {
			hb.startGate = make(chan error)
			prev := dispatchStartWait
			dispatchStartWait = 20 * time.Millisecond
			t.Cleanup(func() { dispatchStartWait = prev })
		}, []string{"spawn", "open", "start", "close", "stop"}, "it did not start within 20ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, fs, hb, log := dispatchModel(t, 100, 30)
			tc.set(fs, hb)
			m = selectKey(t, m, runKey("lumen"))
			m, _ = typeList(t, m, "go")
			m, cmd := press(m, enter())
			msg := dispatched(t, mustCmd(t, cmd, "sessDispatch"))
			if msg.out != dispatchFailed {
				t.Fatalf("the outcome %v: %v", msg.out, msg.err)
			}
			if got := log.seen(); !slices.Equal(got, tc.calls) {
				t.Fatalf("the dispatch ran %v, want %v", got, tc.calls)
			}
			if _, texts := hb.sent(); len(texts) != 0 || len(fs.leaves) != 0 {
				t.Fatalf("sent %q, left %v", texts, fs.leaves)
			}
			tm, _ := m.Update(msg)
			m = tm.(Model)
			if v, _ := inputOf(m); v != "go" || sessHint(m) != "could not start a session in ~/projects/lumen: "+tc.why {
				t.Fatalf("input %q, hint %q", v, sessHint(m))
			}
		})
	}
}

// TestADispatchDrainsAStreamThatFillsBeforeItsStart (R2-4): a host whose
// start publishes more than the connection's stream queue holds before it is
// ready waits for a reader — the fixture proves it stalls without one — and
// the dispatch's drain reads it past, so Start returns and the prompt goes.
func TestADispatchDrainsAStreamThatFillsBeforeItsStart(t *testing.T) {
	// The fixture: no reader, and the start never returns (a short bound
	// ends the wait).
	stalled := newHostBackend("stalled", t.TempDir(), &callLog{})
	stalled.fill = 64
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := stalled.Start(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fixture: with nobody reading, the start answered %v", err)
	}

	m, _, hb, log := dispatchModel(t, 100, 30)
	hb.fill = 64
	m = selectKey(t, m, runKey("lumen"))
	m, _ = typeList(t, m, "go")
	_, cmd := press(m, enter())
	msg := dispatched(t, mustCmd(t, cmd, "sessDispatch"))
	if msg.out != dispatchAccepted {
		t.Fatalf("the outcome %v: %v", msg.out, msg.err)
	}
	if n := hb.reads.Load(); n != 64 {
		t.Fatalf("the drain read %d items, want the 64 the start published", n)
	}
	if got := log.seen(); !slices.Equal(got, []string{"spawn", "open", "start", "submit", "leave", "close"}) {
		t.Fatalf("the dispatch ran %v", got)
	}
}

// TestAQuitRacingADispatch (§3.18 PR 3, X99): craze quitting mid-dispatch
// leaves no host the quit did not decide. Forced at each place:
//
//   - the start still out: the exit closes the dispatch's connection
//     (finishRun, the backends it closes), the start answers closed, and the
//     dispatch stops the host — nothing was sent to it, and nothing leaves it
//     running;
//   - the prompt taken, the host about to be left running: LeaveRunning
//     before the launch's finish keeps it; finish first — which stops every
//     host Spawn started that was not left, X99 — and LeaveRunning says so,
//     which the dispatch reports.
//
// Either way the outcome lands on a program that has quit, and changes
// nothing.
func TestAQuitRacingADispatch(t *testing.T) {
	t.Run("the start out", func(t *testing.T) {
		m, fs, hb, log := dispatchModel(t, 100, 30)
		hb.startGate = make(chan error)
		m = selectKey(t, m, runKey("lumen"))
		m, _ = typeList(t, m, "go")
		m, cmd := press(m, enter())
		wait := later(t, mustCmd(t, cmd, "sessDispatch"))
		awaitStep(t, hb.startIn, "the start")
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlD})
		if !m.quitting {
			t.Fatal("ctrl+d on the list did not quit")
		}
		finishRun(io.Discard, m, m, nil)
		msg := wait().(sessDispatchedMsg)
		if msg.out != dispatchFailed || !errors.Is(msg.err, backend.ErrClosed) {
			t.Fatalf("the outcome %v: %v", msg.out, msg.err)
		}
		if got := log.seen(); !slices.Equal(got, []string{"spawn", "open", "start", "close", "stop"}) {
			t.Fatalf("the dispatch ran %v", got)
		}
		if len(fs.leaves) != 0 {
			t.Fatalf("left %v running", fs.leaves)
		}
	})
	for _, finishFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("the prompt taken, finish first %v", finishFirst), func(t *testing.T) {
			m, fs, _, _ := dispatchModel(t, 100, 30)
			// The launch as X99 has it: finish decides under the lock
			// LeaveRunning decides under, and stops each host Spawn started
			// that was not left running.
			var mu sync.Mutex
			over, left := false, false
			fs.leave = func(roster.Ref) error {
				mu.Lock()
				defer mu.Unlock()
				if over {
					return errors.New("craze is exiting")
				}
				left = true
				return nil
			}
			finish := func() {
				mu.Lock()
				over = true
				stop := !left
				mu.Unlock()
				if stop {
					_ = fs.Stop(hostRef("new"))
				}
			}
			held, release := make(chan struct{}), make(chan struct{})
			dispatchHook = func(step dispatchStep) {
				if step == dispatchLeaving {
					close(held)
					<-release
				}
			}
			t.Cleanup(func() { dispatchHook = nil })

			m = selectKey(t, m, runKey("lumen"))
			m, _ = typeList(t, m, "go")
			m, cmd := press(m, enter())
			wait := later(t, mustCmd(t, cmd, "sessDispatch"))
			awaitStep(t, held, "the dispatch before LeaveRunning")
			m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlD})
			finishRun(io.Discard, m, m, nil)
			if finishFirst {
				finish()
				close(release)
			} else {
				close(release)
			}
			msg := wait().(sessDispatchedMsg)
			if !finishFirst {
				finish()
			}
			switch {
			case finishFirst && (msg.out != dispatchFailed || !strings.Contains(fmt.Sprint(msg.err), "it was not kept running: craze is exiting")):
				t.Fatalf("finish first: the outcome %v: %v", msg.out, msg.err)
			case finishFirst && len(fs.stops) != 1:
				t.Fatalf("finish first: stopped %v, want the host finish stopped", fs.stops)
			case !finishFirst && (msg.out != dispatchAccepted || len(fs.stops) != 0 || len(fs.leaves) != 1):
				t.Fatalf("LeaveRunning first: the outcome %v (%v), stopped %v, left %v", msg.out, msg.err, fs.stops, fs.leaves)
			}
			// The outcome lands on the program that quit: it stays quitting,
			// and nothing is started.
			tm, cmd2 := m.Update(msg)
			if !tm.(Model).quitting || findCmd(cmd2, "sessDispatch") != nil || findCmd(cmd2, "enterUnstarted") != nil {
				t.Fatal("the late outcome started something")
			}
		})
	}
}

// joinedSession is a dispatch's connection over a real socket that says which
// of its calls still run: the prompt's Submit (returned, closed as it
// returns) and the drain's Reads (reading, the count in flight).
type joinedSession struct {
	*remote.Session
	returned chan struct{}
	reading  atomic.Int32
}

func (b *joinedSession) Submit(ctx context.Context, c engine.Command, text string, mode engine.SubmitMode, fromRow string) (engine.SubmitResult, error) {
	defer close(b.returned)
	return b.Session.Submit(ctx, c, text, mode, fromRow)
}

func (b *joinedSession) Read(ctx context.Context) (backend.Item, error) {
	b.reading.Add(1)
	defer b.reading.Add(-1)
	return b.Session.Read(ctx)
}

// heldDeadline is a dispatch's prompt deadline (dispatchPromptContext) that
// passes when the test says (pass): until then it never does, and from then
// on it is a deadline exceeded — so a test forces the order of the deadline
// and the prompt's write instead of racing a clock against it.
type heldDeadline struct {
	done chan struct{}
	once sync.Once
}

func (d *heldDeadline) Deadline() (time.Time, bool) { return time.Time{}, false }
func (d *heldDeadline) Done() <-chan struct{}       { return d.done }
func (d *heldDeadline) Value(any) any               { return nil }

func (d *heldDeadline) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (d *heldDeadline) pass() { d.once.Do(func() { close(d.done) }) }

// holdPromptDeadline makes every dispatch's prompt deadline, for one test, a
// heldDeadline the test passes.
func holdPromptDeadline(t *testing.T) *heldDeadline {
	t.Helper()
	d := &heldDeadline{done: make(chan struct{})}
	prev := dispatchPromptContext
	dispatchPromptContext = func() (context.Context, context.CancelFunc) { return d, func() {} }
	t.Cleanup(func() {
		dispatchPromptContext = prev
		d.pass()
	})
	return d
}

// setHeld is how many backends s holds to close.
func setHeld(s *backendSet) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open)
}

// stalledDispatch is a dispatch asked for on dispatchModel's list — its
// command not yet run — whose session is a real host behind a stall proxy
// (newStallProxy): the host starts, and at the dispatch's dispatchStarted
// step the proxy stops reading, so the prompt, longer than the socket holds
// (longLine), blocks in its write, holding the connection's write lock,
// where no context reaches it (X54). w says when that write has begun.
func stalledDispatch(t *testing.T) (Model, tea.Cmd, *startSessions, *callLog, *joinedSession, *writeWatch) {
	t.Helper()
	m, fs, _, log := dispatchModel(t, 100, 30)
	h := newAttachHost(t, true)
	p := newStallProxy(t, h.path)
	w := &writeWatch{began: make(chan struct{})}
	conn := &joinedSession{Session: dialWatched(t, p, w), returned: make(chan struct{})}
	fs.open = func(roster.Ref) (backend.Backend, error) { return conn, nil }
	dispatchHook = func(step dispatchStep) {
		if step == dispatchStarted {
			p.stall()
		}
	}
	t.Cleanup(func() { dispatchHook = nil })
	spec, err := m.sessNewSpec(homePath("projects/lumen"))
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.sessDispatch(spec, strings.Repeat("x", longLine))
	return m, cmd, fs, log, conn, w
}

// assertDispatchJoined: once the dispatch has answered, nothing of it runs or
// is held — its prompt's Submit has returned, its drain reads nothing, its
// connection is closed and out of the program's set — and its host was left
// running, never stopped.
func assertDispatchJoined(t *testing.T, m Model, fs *startSessions, log *callLog, conn *joinedSession) {
	t.Helper()
	select {
	case <-conn.returned:
	default:
		t.Fatal("the dispatch answered with its prompt's Submit still running")
	}
	if n := conn.reading.Load(); n != 0 {
		t.Fatalf("the dispatch answered with %d reads of its drain still running", n)
	}
	if _, err := conn.Session.Read(context.Background()); !errors.Is(err, backend.ErrClosed) {
		t.Fatalf("the dispatch's connection after it answered: a read answered %v, want it closed", err)
	}
	if n := setHeld(m.retired); n != 0 {
		t.Fatalf("the program still holds %d of the dispatch's backends to close", n)
	}
	if got, want := log.seen(), []string{"spawn", "open", "leave"}; !slices.Equal(got, want) {
		t.Fatalf("the dispatch ran %v, want %v", got, want)
	}
	if !slices.Equal(fs.leaves, []roster.Ref{hostRef("new")}) || len(fs.stops) != 0 {
		t.Fatalf("left %v, stopped %v: want the host left running", fs.leaves, fs.stops)
	}
}

// TestADispatchWhoseHostStopsReadingSettlesAtItsDeadline (C15r, astra
// r30-c15 1), a forced schedule over a real socket: the new session's host
// starts and then reads nothing more, so the prompt blocks in its write,
// where the call's context never reaches it (stalledDispatch). The dispatch
// still settles at the prompt's deadline — it used to wait on that write for
// ever, the input saying starting… and the connection and its drain held —
// with the outcome unknown and the host left running; before it answers, the
// connection is closed at once (which ends the write: no detach waits behind
// it) and the submission and the drain are joined. The input then says the
// session may have started.
func TestADispatchWhoseHostStopsReadingSettlesAtItsDeadline(t *testing.T) {
	deadline := holdPromptDeadline(t)
	m, cmd, fs, log, conn, w := stalledDispatch(t)
	answered := make(chan tea.Msg, 1)
	go func() { answered <- cmd() }()
	awaitStep(t, w.began, "the prompt's write")
	// The write blocked, the prompt's time runs out.
	deadline.pass()
	var msg sessDispatchedMsg
	select {
	case got := <-answered:
		msg = got.(sessDispatchedMsg)
	case <-time.After(quitStepBound):
		t.Fatalf("the dispatch did not settle within %v of its prompt's deadline", quitStepBound)
	}
	if msg.out != dispatchUnknown || !errors.Is(msg.err, context.DeadlineExceeded) {
		t.Fatalf("the outcome %v: %v; want unknown at the prompt's deadline", msg.out, msg.err)
	}
	assertDispatchJoined(t, m, fs, log, conn)
	tm, _ := m.Update(msg)
	m = tm.(Model)
	if m.sessList.in.dispatching != 0 || sessHint(m) != dispatchUnknownNote {
		t.Fatalf("after the outcome: dispatching %d, hint %q", m.sessList.in.dispatching, sessHint(m))
	}
}

// TestAQuitDuringADispatchsSubmission (C15r, astra r30-c15 3), a forced
// schedule over a real socket: craze quits — ctrl+d on the list, or a signal
// (finishRun without it, as Run reaches it on SIGTERM) — while the dispatch's
// prompt is blocked in its write to a host that has stopped reading
// (stalledDispatch), the prompt's own deadline held off (holdPromptDeadline). The run's exit tail
// closes the dispatch's connection at once — no detach, which would wait
// behind that write for the client's whole close bound — so the exit is not
// held up, and the close ends the write: the dispatch settles, the outcome
// unknown (its connection closed under the call), its host left running,
// every goroutine of it joined. The outcome lands on a program that has quit
// and changes nothing.
func TestAQuitDuringADispatchsSubmission(t *testing.T) {
	for _, quit := range []string{"ctrl+d", "a signal"} {
		t.Run(quit, func(t *testing.T) {
			holdPromptDeadline(t)
			m, cmd, fs, log, conn, w := stalledDispatch(t)
			answered := make(chan tea.Msg, 1)
			go func() { answered <- cmd() }()
			awaitStep(t, w.began, "the prompt's write")
			if quit == "ctrl+d" {
				m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlD})
				if !m.quitting {
					t.Fatal("ctrl+d on the list did not quit")
				}
			}
			within(t, "the run's exit tail (finishRun)", func() bool {
				_, _ = finishRun(io.Discard, m, m, nil)
				return true
			})
			var msg sessDispatchedMsg
			select {
			case got := <-answered:
				msg = got.(sessDispatchedMsg)
			case <-time.After(quitStepBound):
				t.Fatalf("the dispatch did not settle within %v of the exit's close", quitStepBound)
			}
			if msg.out != dispatchUnknown || !errors.Is(msg.err, backend.ErrOutcomeUnknown) {
				t.Fatalf("the outcome %v: %v; want unknown, its connection closed under the call", msg.out, msg.err)
			}
			assertDispatchJoined(t, m, fs, log, conn)
			if quit == "ctrl+d" {
				tm, cmd2 := m.Update(msg)
				if !tm.(Model).quitting || findCmd(cmd2, "sessDispatch") != nil || findCmd(cmd2, "enterUnstarted") != nil {
					t.Fatal("the late outcome started something")
				}
			}
		})
	}
}

// TestADispatchLeftBehindIsDropped: the list left while a dispatch runs, its
// outcome — the host decided by the dispatch itself — changes nothing, and a
// list opened again does not take it either.
func TestADispatchLeftBehindIsDropped(t *testing.T) {
	m, fs, _, _ := dispatchModel(t, 100, 30)
	m = selectKey(t, m, runKey("lumen"))
	m, _ = typeList(t, m, "go")
	m, cmd := press(m, enter())
	disp := mustCmd(t, cmd, "sessDispatch")
	// esc leaves the list while it starts (it does not clear the input).
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.sessList.open {
		t.Fatal("esc while starting did not leave the list")
	}
	msg := dispatched(t, disp)
	if msg.out != dispatchAccepted || len(fs.leaves) != 1 {
		t.Fatalf("the dispatch after the list was left: %v (%v), left %v", msg.out, msg.err, fs.leaves)
	}
	if tm, _ := m.Update(msg); tm.(Model).sessList.open || tm.(Model).sessList.note != "" {
		t.Fatal("an outcome for a list since left was taken")
	}
	reopened := openList(t, m)
	if tm, _ := reopened.Update(msg); tm.(Model).sessList.note != "" {
		t.Fatal("an outcome for an earlier opening of the list was taken")
	}
}

// TestExitInTheListsInputQuitsCraze (decision 11, §3.13): `/exit` alone in
// the list's input quits craze with every session left running — the list's
// own quit, never the explicit quit that stops the session behind it.
func TestExitInTheListsInputQuitsCraze(t *testing.T) {
	m, fs, _, log := dispatchModel(t, 100, 30)
	m, _ = typeList(t, m, "/exit")
	m, cmd := press(m, enter())
	if !m.quitting || findCmd(cmd, "sessQuit") == nil {
		t.Fatalf("/exit: quitting %v, commands %v", m.quitting, cmdFuncName(cmd))
	}
	if len(fs.spawns) != 0 || len(log.seen()) != 0 || len(fs.stops) != 0 {
		t.Fatalf("/exit started or stopped something: %v", log.seen())
	}
	// With words after it, it is a prompt.
	n, _, _, _ := dispatchModel(t, 100, 30)
	n = selectKey(t, n, runKey("lumen"))
	n, _ = typeList(t, n, "/exit now")
	if n, cmd = press(n, enter()); n.quitting || findCmd(cmd, "sessDispatch") == nil {
		t.Fatal("`/exit now` was not dispatched as a prompt")
	}
}

// TestNothingBehindTheListStartsFromTheSelectedRowAlone (X142's note, C15):
// once the connection to the session behind the list is lost, nothing is
// behind it: the `@` picker offers no here row, the target no token names is
// the selected row's — and with no row to select there is nowhere to start,
// which the rule and enter say.
func TestNothingBehindTheListStartsFromTheSelectedRowAlone(t *testing.T) {
	m, fs, _, _ := dispatchModel(t, 100, 30)
	tm, _ := m.Update(endMsg{err: errors.New("re-attaches spent"), bgen: m.bgen})
	m = tm.(Model)
	if !m.sessList.hereLost {
		t.Fatal("fixture: the lost connection did not leave nothing behind the list")
	}
	pop, _ := typeList(t, m, "@")
	for _, it := range pop.sessList.in.at.ans.Items {
		if strings.HasPrefix(it.Note, "here") {
			t.Fatalf("the picker offers a here row with nothing behind the list: %+v", it)
		}
	}
	m = selectKey(t, m, runKey("lumen"))
	if got := ruleOf(t, m); !strings.HasPrefix(got, "new session → ~/projects/lumen") {
		t.Fatalf("the rule %q", got)
	}
	// No rows at all.
	m = listSnap(t, m, roster.Snapshot{})
	if got := ruleOf(t, m); got != sessNoTargetNote {
		t.Fatalf("with no row and nothing behind, the rule %q", got)
	}
	m, _ = typeList(t, m, "go")
	m, cmd := press(m, enter())
	if findCmd(cmd, "sessDispatch") != nil || len(fs.spawns) != 0 || sessHint(m) != sessNoTargetNote {
		t.Fatalf("enter with nowhere to start: hint %q, spawns %d", sessHint(m), len(fs.spawns))
	}
}

// ------------------------------------------------------ the unstarted session

// openedUnstarted is dispatchModel's list with `@lumen` picked and entered
// alone: the unstarted session in ~/projects/lumen.
func openedUnstarted(t *testing.T, cols, rows int) (Model, *startSessions, *hostBackend, *callLog) {
	t.Helper()
	m, fs, hb, log := dispatchModel(t, cols, rows)
	m, _ = typeList(t, m, "@lu")
	m, _ = press(m, enter())
	m, _ = press(m, enter())
	if m.sessList.open || m.unstarted == nil {
		t.Fatalf("enter on @lumen alone opened no unstarted session:\n%s", plainView(m))
	}
	return m, fs, hb, log
}

// typeComposer types text into the composer a key at a time.
func typeComposer(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, r := range text {
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

// TestAnUnstartedSessionOpensInPlaceWithNothingSpawned (§3.13, AC10): enter
// on a leading `@lumen` alone opens a new session in place — the list closed,
// the session it came from let go of (a view close), the band naming the new
// one and its directory, the status row what it will run, the composer
// empty — and spawns nothing. Its draft is keyed by a temporary id.
func TestAnUnstartedSessionOpensInPlaceWithNothingSpawned(t *testing.T) {
	m, fs, _ := newSessModel(t, 100, 30)
	m = newList(t, m, fs)
	provider := m.snap.Provider.Label()
	shown, bgen := m.shownGen, m.bgen
	m, _ = typeList(t, m, "@lu")
	m, _ = press(m, enter())
	m, cmd := press(m, enter())
	switch {
	case m.sessList.open || m.unstarted == nil || m.eng != nil:
		t.Fatalf("list open %v, unstarted %v, backend %v", m.sessList.open, m.unstarted != nil, m.eng)
	case len(fs.spawns) != 0 || len(fs.opened()) != 0:
		t.Fatal("the unstarted session spawned or opened something")
	case m.shownGen != shown+1 || m.bgen == bgen:
		t.Fatalf("shown %d→%d, backend generation %d→%d: another session shown, at a generation of its own", shown, m.shownGen, bgen, m.bgen)
	case !strings.HasPrefix(m.draftKey(), "\x00new:"):
		t.Fatalf("the draft key %q", m.draftKey())
	case m.unstarted.spec.Workspace != homePath("projects/lumen") || m.cwd != homePath("projects/lumen"):
		t.Fatalf("the unstarted session runs in %q (cwd %q)", m.unstarted.spec.Workspace, m.cwd)
	}
	if findCmd(cmd, "retire") == nil {
		t.Fatal("the session the list came from was not let go of")
	}
	view := plainView(m)
	for _, want := range []string{"new session · " + provider + " · ~/projects/lumen", "← sessions", unstartedHint, "lumen │"} {
		if !strings.Contains(view, want) {
			t.Fatalf("the unstarted session's frame is missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "starting…") {
		t.Fatalf("an unstarted session says it is starting:\n%s", view)
	}
}

// TestAnUnstartedSessionsFirstPromptIsAdoptedWithTheTUIsOwnNumbering (§3.13,
// R2-4): its first enter spawns the host and opens it (a second enter while
// that runs does nothing), and the backend Open answered is adopted as a
// session opened in place is — started and read by the TUI — while the
// session shown stays the one typed into: its composer keeps the prompt, its
// shown generation stands, and its draft key becomes the session's craze id.
// Once the session is up the prompt goes through the ordinary submit path,
// under the adopted backend's client with the TUI's own first command id: no
// connection or counter from anything else. Accepted, the composer is cleared
// and the prompt drawn.
func TestAnUnstartedSessionsFirstPromptIsAdoptedWithTheTUIsOwnNumbering(t *testing.T) {
	m, fs, hb, log := openedUnstarted(t, 100, 30)
	shown := m.shownGen
	m = typeComposer(t, m, "add a search box")
	m, cmd := press(m, enter())
	spawn := mustCmd(t, cmd, "enterUnstarted")
	if m.unstarted.pending == 0 || !strings.Contains(plainView(m), "starting…") {
		t.Fatalf("pending %d:\n%s", m.unstarted.pending, plainView(m))
	}
	if _, cmd2 := press(m, enter()); findCmd(cmd2, "enterUnstarted") != nil {
		t.Fatal("a second enter while the spawn runs spawned again")
	}
	msg := runWatched(t, spawn)
	tm, cmd := m.Update(msg)
	m = tm.(Model)
	switch {
	case m.eng != hb || m.unstarted != nil || m.first == nil:
		t.Fatalf("adopted %v, unstarted %v, first %v", m.eng, m.unstarted, m.first)
	case m.shownGen != shown:
		t.Fatalf("the adoption moved the shown generation %d→%d", shown, m.shownGen)
	case m.input.Value() != "add a search box":
		t.Fatalf("the composer after the adoption: %q", m.input.Value())
	case m.draftKey() != "new":
		t.Fatalf("the draft key after the host answered: %q", m.draftKey())
	case !m.reading:
		t.Fatal("the adopted backend's stream is not read")
	}
	start := mustCmd(t, cmd, "startCmd")
	tm, _ = m.Update(runWatched(t, start))
	m = tm.(Model)
	cmds, texts := hb.sent()
	if !slices.Equal(cmds, []engine.Command{{Client: "client-new", ID: "1"}}) || !slices.Equal(texts, []string{"add a search box"}) {
		t.Fatalf("the first prompt went as %v with %q", cmds, texts)
	}
	if got := log.seen(); !slices.Equal(got, []string{"spawn", "open", "start", "submit"}) {
		t.Fatalf("the adoption ran %v", got)
	}
	if len(fs.opened()) != 1 || len(fs.leaves) != 0 || len(fs.stops) != 0 || m.first != nil {
		t.Fatalf("opens %d, left %v, stopped %v, first %v", len(fs.opened()), fs.leaves, fs.stops, m.first)
	}
	if m.input.Value() != "" || !strings.Contains(plainView(m), "add a search box") || m.status != statusWorking {
		t.Fatalf("after the prompt: composer %q, status %v:\n%s", m.input.Value(), m.status, plainView(m))
	}
	// The TUI's next command is its own second.
	if c := m.nextCmd(); c != (engine.Command{Client: "client-new", ID: "2"}) {
		t.Fatalf("the TUI's next command %+v", c)
	}
}

// TestAnUnstartedSessionThatFailsToStartIsUnstartedAgain (§3.13): a spawn
// that fails, a start that fails, or a stream that ends before the session
// is up puts it back to unstarted — the error drawn, the prompt still in the
// composer, its draft key temporary again — and a started host is let go of
// and stopped. Enter tries again; craze's exit status is not a failure.
func TestAnUnstartedSessionThatFailsToStartIsUnstartedAgain(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fail makes the attempt fail: before the adoption (spawned), or
		// after it on the adopted model (adopted).
		spawnErr error
		adopted  func(t *testing.T, m Model, cmd tea.Cmd) (tea.Model, tea.Cmd)
		why      string
	}{
		{name: "the spawn fails", spawnErr: errors.New("craze: the session host could not start: no agent"),
			why: "could not start the session: the session host could not start: no agent"},
		{name: "the start fails", adopted: func(t *testing.T, m Model, cmd tea.Cmd) (tea.Model, tea.Cmd) {
			m.eng.(*hostBackend).startErr = errors.New("authentication required")
			return m.Update(runWatched(t, mustCmd(t, cmd, "startCmd")))
		}, why: "could not start the session: authentication required"},
		{name: "the stream ends first", adopted: func(t *testing.T, m Model, _ tea.Cmd) (tea.Model, tea.Cmd) {
			return m.Update(endMsg{bgen: m.bgen})
		}, why: "could not start the session: the session ended before it started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, fs, hb, _ := openedUnstarted(t, 100, 30)
			tmp := m.draftKey()
			if tc.spawnErr != nil {
				fs.spawn = func(SpawnSpec) (roster.Ref, error) { return roster.Ref{}, tc.spawnErr }
			}
			m = typeComposer(t, m, "add a search box")
			m, cmd := press(m, enter())
			tm, cmd := m.Update(runWatched(t, mustCmd(t, cmd, "enterUnstarted")))
			m = tm.(Model)
			var failed tea.Cmd
			if tc.adopted != nil {
				bgen := m.bgen
				tm, failed = tc.adopted(t, m, cmd)
				m = tm.(Model)
				if m.bgen == bgen {
					t.Fatal("the failed backend's generation is still the model's")
				}
			}
			switch {
			case m.unstarted == nil || m.unstarted.pending != 0 || m.eng != nil || m.first != nil:
				t.Fatalf("unstarted %+v, backend %v, first %v", m.unstarted, m.eng, m.first)
			case m.input.Value() != "add a search box":
				t.Fatalf("the composer %q", m.input.Value())
			case m.draftKey() != tmp:
				t.Fatalf("the draft key %q, want the temporary %q again", m.draftKey(), tmp)
			case m.startErr != nil:
				t.Fatalf("craze's exit status carries %v", m.startErr)
			case !strings.Contains(plainView(m), tc.why):
				t.Fatalf("the error is not drawn (%q):\n%s", tc.why, plainView(m))
			}
			if tc.adopted != nil {
				runWatched(t, mustCmd(t, failed, "retire"))
				runWatched(t, mustCmd(t, failed, "stopSpawned"))
				if !hb.isClosed() || !slices.Equal(fs.stops, []roster.Ref{hostRef("new")}) {
					t.Fatalf("closed %v, stopped %v, want the backend closed and the host the spawn started stopped", hb.isClosed(), fs.stops)
				}
			}
			// Enter tries again.
			if _, cmd := press(m, enter()); findCmd(cmd, "enterUnstarted") == nil {
				t.Fatal("enter after the failure did not spawn again")
			}
		})
	}
}

// TestAnUnstartedSessionAbandonedLeavesNothing (§3.13, AC10): `←` before a
// prompt discards it — nothing spawned, no draft under its temporary id, and
// the list opened over it has nothing behind it: esc and ← stay and say so,
// and the cursor starts on the row it was opened from. A spawn it abandoned
// while running — its composer emptied and ← pressed — is closed where it
// answers, and its host stopped.
func TestAnUnstartedSessionAbandonedLeavesNothing(t *testing.T) {
	t.Run("before a prompt", func(t *testing.T) {
		m, fs, _ := newSessModel(t, 100, 30)
		m = newList(t, m, fs)
		from := m.sessList.sel
		here := m.hereKey()
		m, _ = typeList(t, m, "@lu")
		m, _ = press(m, enter())
		m, _ = press(m, enter())
		tmp := m.draftKey()
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyLeft})
		switch {
		case !m.sessList.open || !m.sessList.none || m.unstarted != nil || m.eng != nil:
			t.Fatalf("← on the unstarted session: list %v, none %v, unstarted %v", m.sessList.open, m.sessList.none, m.unstarted)
		case len(fs.spawns) != 0:
			t.Fatal("something was spawned")
		}
		if _, ok := m.drafts[tmp]; ok {
			t.Fatal("a draft is left under the temporary id")
		}
		// The session the list came from still runs on its host: its row is
		// listed, an ordinary one now.
		m = listSnap(t, m, richSnapshot(homePath(""), here))
		m = listRecents(t, m, fs)
		if m.sessList.sel != from {
			t.Fatalf("the cursor is on %+v, want %+v", m.sessList.sel, from)
		}
		for _, k := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyLeft}} {
			if n, _ := press(m, k); !n.sessList.open || sessHint(n) != sessNothingNote {
				t.Fatalf("%v with nothing behind the list: open %v, hint %q", k, n.sessList.open, sessHint(n))
			}
		}
	})
	t.Run("its spawn running", func(t *testing.T) {
		m, fs, hb, _ := openedUnstarted(t, 100, 30)
		m = typeComposer(t, m, "go")
		m, cmd := press(m, enter())
		spawn := mustCmd(t, cmd, "enterUnstarted")
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyBackspace})
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyBackspace})
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyLeft})
		if !m.sessList.open || !m.sessList.none || m.unstarted != nil {
			t.Fatalf("← with the spawn running: list %v, none %v", m.sessList.open, m.sessList.none)
		}
		tm, cmd := m.Update(runWatched(t, spawn))
		m = tm.(Model)
		if m.eng != nil || !m.sessList.open {
			t.Fatal("an abandoned spawn was adopted")
		}
		runWatched(t, mustCmd(t, cmd, "retire"))
		runWatched(t, mustCmd(t, cmd, "stopSpawned"))
		if !hb.isClosed() || !slices.Equal(fs.stops, []roster.Ref{hostRef("new")}) {
			t.Fatalf("the abandoned spawn: closed %v, stopped %v", hb.isClosed(), fs.stops)
		}
	})
}

// TestFrameGoldenSessionsUnstarted (§3.17): the unstarted session — the band
// naming it and its directory, an empty transcript, the composer saying its
// first prompt starts it, the status row what it will run — at 100×30 and
// 80×24.
func TestFrameGoldenSessionsUnstarted(t *testing.T) {
	for _, size := range []struct{ cols, rows int }{{100, 30}, {80, 24}} {
		m, _, _, _ := openedUnstarted(t, size.cols, size.rows)
		assertFrameGolden(t, fmt.Sprintf("sessions-unstarted-%dx%d", size.cols, size.rows), size.cols, size.rows, plainView(m),
			[]string{"new session · ", "~/projects/lumen", "← sessions", unstartedHint, "▸▸ bypass permissions on"},
			[]string{"starting…"})
	}
}
