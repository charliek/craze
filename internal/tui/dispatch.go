package tui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/roster"
)

// Starting a session from the list (plan 030 §3.13, R2-4): enter on the
// list's input starts a new session, in the directory the input's rule names
// (sessions_input.go), with the provider, model and permission mode of the
// session the list was opened from — all four captured at that enter
// (sessNewSpec). Two ways, split by what the input holds (R2-4):
//
//   - A prompt — background dispatch (runDispatch): the session is started
//     and given its prompt without the TUI leaving the list. The input says
//     starting… and takes no second enter; a host is spawned for it
//     (Sessions.Spawn) and opened as a connection of the dispatch's own
//     (Sessions.Open — a remote.Session in production, attached now), whose
//     stream a goroutine drains, since a start that publishes more than the
//     stream's queue holds before it is ready waits for a reader; its Start
//     returns at readiness; the prompt goes with that connection's own first
//     command id; the host is left running (Sessions.LeaveRunning); and the
//     connection is closed. The row appears on the roster's next tick.
//     The outcome is the hint line's: started in ~/projects/lumen (the input
//     cleared); the host's refusal of the prompt (the input kept, the host
//     stopped); may have started — check the list, when the connection went
//     after the prompt was sent, or no answer came in the prompt's time (the
//     input kept, the host kept); or why no session came up (the input
//     kept).
//   - A leading `@dir` alone — the unstarted session: opened in place, in the
//     foreground, with nothing spawned (openUnstarted). Its band says `new
//     session · <provider> · ~/projects/lumen`; its first enter spawns a host
//     and adopts it as any session opened in place is adopted — the TUI's own
//     attach, start and reader — and once the session is up sends the prompt
//     through the TUI's ordinary submit path, with the TUI's own command
//     numbering: no connection and no counter is handed over from anything
//     else (adoptUnstarted, the startedMsg arm). A spawn or start that fails
//     puts it back to unstarted, the error drawn and the prompt still in the
//     composer (backToUnstarted); `←` before a prompt discards it, leaving
//     nothing — no host, no draft, no row (discardUnstarted).

// dispatchStartWait bounds a background dispatch's wait for its session's
// start: an agent that takes longer than this to come up is given up on, and
// its host stopped. A variable only so a test can shorten it.
var dispatchStartWait = 60 * time.Second

// dispatchCommandID is the command id a background dispatch's prompt goes
// with: the first of the connection it opened for itself, whose client id is
// that connection's own (R2-4) — no id of the TUI's is spent on it.
const dispatchCommandID = "1"

// dispatchPromptContext is the time a background dispatch's prompt has: the
// command gate's deadline (gateDeadline). A variable only so a test can say
// when that time is up — once the prompt's write has begun — rather than race
// a clock against the write (whose start a starved CPU can put past a short
// deadline).
var dispatchPromptContext = func() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), gateDeadline)
}

// dispatchJoinWait bounds each wait of a background dispatch for a goroutine
// of its own once its connection is closed: the prompt's submission its
// deadline cut short, and the drain of the connection's stream. Over a socket
// both end as soon as that close does — the close ends the write a host that
// stopped reading left blocked, and every read — so this only bites on a
// backend whose calls do not end with it, and the dispatch goes on without
// them (as the quit's quitJoinWait does).
const dispatchJoinWait = time.Second

// dispatchOutcome is what a background dispatch came to.
type dispatchOutcome int

const (
	// dispatchAccepted: the session took the prompt; its host is left
	// running.
	dispatchAccepted dispatchOutcome = iota + 1
	// dispatchRefused: the session refused the prompt; its host is stopped.
	dispatchRefused
	// dispatchUnknown: the prompt was sent and no answer came — the
	// connection went after it was sent, or the call's time ran out — so the
	// session may have taken it; its host is left running.
	dispatchUnknown
	// dispatchFailed: no session came up to be given the prompt — the spawn,
	// the dial or the start failed — or its host could not be kept.
	dispatchFailed
)

// sessDispatchedMsg is a background dispatch's outcome, for the list's
// opening gen and the dispatch seq it was started as; dir is where the
// session was to run.
type sessDispatchedMsg struct {
	gen, seq uint64
	dir      string
	out      dispatchOutcome
	err      error
}

// The dispatch's words.
const (
	dispatchStartingText = "starting…"
	dispatchUnknownNote  = "may have started — check the list"
	unstartedHint        = "type the first prompt to start this session"
	sessNothingNote      = "nothing to go back to"
	sessNoTargetNote     = "no directory to start in: pick one with @"
)

// ---------------------------------------------------------------- the spec

// sessNewSpec is the new session enter on the list's input starts, in dir:
// the provider and model /provider and /model chose (sessPick,
// sessions_models.go, C16), else those of the session the list was opened
// from — as the status row names them, the model its current one, or none
// ("" — the provider's default) when it has not said one — and that session's
// permission mode (§3.13, Claude Code's rule; PermissionUnsaid, from an older
// host, is the launch's own flags). A provider this craze does not know
// starts nothing.
func (m Model) sessNewSpec(dir string) (SpawnSpec, error) {
	p, ok := m.sessNewProviderOf()
	if !ok {
		name := m.snap.Provider.Name
		if name == "" {
			name = m.sessProvider
		}
		return SpawnSpec{}, errNoProvider(name)
	}
	return SpawnSpec{Workspace: dir, Provider: p, Model: m.sessNewModelID(), PermissionMode: m.hostPerm}, nil
}

// ------------------------------------------------------- background dispatch

// sessDispatch is enter with a prompt on the list's input: the input says
// starting… and takes no second enter until the dispatch has answered
// (sessDispatched), and the dispatch runs off the Update (runDispatch). Its
// backend is recorded in the set of backends the program closes on its way
// out (Model.retired), so a quit mid-dispatch leaves no connection behind.
func (m Model) sessDispatch(spec SpawnSpec, prompt string) (Model, tea.Cmd) {
	l := &m.sessList
	l.dispatchSeq++
	l.in.dispatching = l.dispatchSeq
	// No popup while it starts: the input is not edited meanwhile.
	l.in.closePopups()
	s, set, gen, seq := m.sessions, m.retired, l.gen, l.dispatchSeq
	return m, func() tea.Msg {
		out, err := runDispatch(s, spec, prompt, set)
		return sessDispatchedMsg{gen: gen, seq: seq, dir: spec.Workspace, out: out, err: err}
	}
}

// sessDispatched is a dispatch's outcome on the hint line (§3.13), taken only
// by the list opening it was asked in, while it waits for that dispatch: one
// that answers after the list was left — or left and opened again — is
// dropped here, its host having been decided by the dispatch itself.
func (m Model) sessDispatched(msg sessDispatchedMsg) (Model, tea.Cmd) {
	l := &m.sessList
	if !l.open || msg.gen != l.gen || msg.seq == 0 || msg.seq != l.in.dispatching {
		return m, nil
	}
	l.in.dispatching = 0
	dir := sanitizeLine(sessTilde(l.home, msg.dir))
	switch msg.out {
	case dispatchAccepted:
		l.in.set("", 0)
		m.sessNote("started in "+dir, sessNoteOK)
	case dispatchUnknown:
		m.sessNote(dispatchUnknownNote, sessNoteWarn)
	default:
		why := "no session"
		if msg.err != nil {
			why = strings.TrimPrefix(sanitizeLine(failureText(msg.err)), "craze: ")
		}
		m.sessNote("could not start a session in "+dir+": "+why, sessNoteErr)
	}
	return m, m.syncSessInput()
}

// runDispatch starts spec's session and gives it prompt, in the background
// (§3.13, R2-4), and answers what that came to. Each step is bounded: the
// spawn and the dial by the launcher's own bounds, the start by
// dispatchStartWait, the prompt by the command gate's deadline.
//
//  1. Sessions.Spawn starts its host; a failure spawned nothing that runs.
//  2. Sessions.Open dials it — a connection of the dispatch's own, attached
//     now — and a host Spawn started that Open cannot reach is stopped there.
//  3. A goroutine drains the connection's stream from before the start: a
//     remote.Session's Start returns once the ready notification is queued,
//     and a start that publishes more than the stream's queue holds before
//     it would wait for a reader for ever.
//  4. Start waits for readiness. A failed start — the agent's, the bound's,
//     or the connection closed under it by the program's exit — stops the
//     host: nothing was sent to it.
//  5. The prompt goes with the connection's own first command id, and is
//     waited for no longer than the command gate's deadline, whether or not
//     the call can see it (awaitPrompt).
//  6. The session took the prompt: its host is left running
//     (Sessions.LeaveRunning) — unless craze's exit has decided otherwise
//     meanwhile (X99), which says so. No answer came: the prompt may have been
//     taken, and the host is left running for the list to show. Either is
//     decided while the connection still holds the host open, so the host is
//     at no moment neither held nor left — the moment an Open of it by the
//     list that could not reach it would stop it (X98, X110).
//  7. The connection is closed — a view close, which the program's exit cuts
//     short (dispatchConn), or at once when the prompt's deadline cut it
//     short — and the drain joined, bounded. A refused prompt's host is
//     stopped after.
func runDispatch(s Sessions, spec SpawnSpec, prompt string, set *backendSet) (dispatchOutcome, error) {
	ref, err := s.Spawn(spec)
	if err != nil {
		return dispatchFailed, err
	}
	b, err := s.Open(ref)
	if err == nil && b == nil {
		err = errors.New("no session")
	}
	if err != nil {
		return dispatchFailed, err
	}
	// Recorded for the program's exit as a connection it closes at once
	// (dispatchConn): nothing it could detach is worth the wait.
	conn := newDispatchConn(b)
	set.add(conn)
	ctx, cancel := context.WithCancel(context.Background())
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			if _, err := b.Read(ctx); err != nil {
				return
			}
		}
	}()
	// closeIt closes the connection — a view close, the stream detached,
	// which the program's exit cuts short (dispatchConn.closeView) — takes it
	// out of the program's set (whose close of it is then a no-op: Close is
	// idempotent), and joins the drain, bounded.
	closeIt := func() {
		_ = conn.closeView()
		set.close(conn)
		cancel()
		joinWithin(drained, dispatchJoinWait)
	}

	sctx, scancel := context.WithTimeout(context.Background(), dispatchStartWait)
	err = b.Start(sctx)
	if err != nil && errors.Is(sctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("it did not start within %s", dispatchStartWait)
	}
	scancel()
	if dispatchHook != nil {
		dispatchHook(dispatchStarted)
	}
	if err != nil {
		closeIt()
		_ = s.Stop(ref)
		return dispatchFailed, err
	}

	pctx, pcancel := dispatchPromptContext()
	defer pcancel()
	submitted := make(chan error, 1)
	go func() {
		_, err := b.Submit(pctx, engine.Command{Client: b.ClientID(), ID: dispatchCommandID}, prompt, engine.SubmitQueue, "")
		submitted <- err
	}()
	answered, err := awaitPrompt(pctx, submitted)
	if !answered {
		// The deadline passed with the prompt still out — its write, it may
		// be, blocked on a host that has stopped reading, where no context
		// reaches it (X54). The outcome is unknown: the host is left running
		// first, as for any unknown outcome, and then the connection is
		// closed at once — no detach, which could only wait behind that
		// write — which ends the write; the submission and the drain are
		// joined, each bounded.
		_ = s.LeaveRunning(ref)
		set.close(conn)
		joinWithin(submitted, dispatchJoinWait)
		cancel()
		joinWithin(drained, dispatchJoinWait)
		return dispatchUnknown, err
	}
	if dispatchHook != nil {
		dispatchHook(dispatchPrompted)
	}
	switch {
	case err == nil:
		if dispatchHook != nil {
			dispatchHook(dispatchLeaving)
		}
		lerr := s.LeaveRunning(ref)
		closeIt()
		if lerr != nil {
			return dispatchFailed, fmt.Errorf("it was not kept running: %w", lerr)
		}
		return dispatchAccepted, nil
	case promptUnknown(err):
		_ = s.LeaveRunning(ref)
		closeIt()
		return dispatchUnknown, err
	}
	closeIt()
	_ = s.Stop(ref)
	return dispatchRefused, err
}

// promptUnknown says a background dispatch's prompt, answered err, got no
// answer (runDispatch): the call's time ran out — remote.Client.Command
// answers its context's own error when that ends (context.DeadlineExceeded,
// or context.Canceled, which no host's reply carries), and a host that ran
// the prompt and gave up on its own bound answers the deadline too — or the
// client could not learn the outcome — its connection went after the prompt
// was sent, or it was closed under the call (the program's exit) — which it
// says as backend.ErrOutcomeUnknown. Anything else is the session's answer,
// and stands whatever the clock says by the time it is read: an answer
// awaitPrompt takes as the deadline passes is still the session's — a
// refusal among them, whose host is stopped (plan 030 C15r2, astra r32-c15r
// 1). So the call's error is read, never the deadline's context.
func promptUnknown(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, backend.ErrOutcomeUnknown)
}

// awaitPrompt is the prompt's answer (runDispatch), waited for no longer than
// its deadline, ctx's: answered says one came — one there as the deadline
// passes included — and err is then the call's; otherwise err is ctx.Err(),
// and the call is still out (plan 030 C15r, astra r30-c15 1). The call cannot
// be relied on to see its context: remote.Client.Command's write does not
// honour it (X54), and a host that has stopped reading leaves a prompt longer
// than the socket holds blocked in that write for ever — the dispatch, its
// connection and its drain with it, and the list's input saying starting…
func awaitPrompt(ctx context.Context, submitted <-chan error) (answered bool, err error) {
	select {
	case err := <-submitted:
		return true, err
	case <-ctx.Done():
	}
	select {
	case err := <-submitted:
		return true, err
	default:
		return false, ctx.Err()
	}
}

// joinWithin waits for done to close, or d to pass.
func joinWithin[T any](done <-chan T, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

// dispatchConn is a background dispatch's connection as the program's set of
// backends to close holds it (Model.retired): closed there at once, detaching
// nothing (closeNow). The program is exiting — a detach says nothing the
// connection's end does not (the host counts an attachment out at its EOF,
// X29) — and a host that has stopped reading would keep the detach waiting
// behind the prompt's blocked write for the client's whole close bound.
//
// The dispatch's own ordinary close is a view close of the backend
// (closeView, closeIt), and that exit close cuts it short: a view close
// already under way when the program exits would otherwise hold the exit's
// close for its detach's whole bound (3 s) — remote.Session's close runs
// once, and a second close waits for the first to finish (plan 030 C15r2,
// astra r32-c15r 2). So the view close is bounded by ctx, made with the
// connection, and the exit's close cancels ctx before it closes.
type dispatchConn struct {
	backend.Backend
	ctx    context.Context
	cancel context.CancelFunc
}

func newDispatchConn(b backend.Backend) *dispatchConn {
	ctx, cancel := context.WithCancel(context.Background())
	return &dispatchConn{Backend: b, ctx: ctx, cancel: cancel}
}

// Close is the program's exit's close (and closeIt's second, a no-op): a view
// close under way is cut short, and the connection closed at once.
func (c *dispatchConn) Close() error {
	c.cancel()
	return closeNow(c.Backend)
}

// closeView is the dispatch's ordinary close: a view close, the stream
// detached — through CloseWithin(ctx) for a backend that has it, whose detach
// waits only while ctx allows, so that Close's cancel ends the wait.
// remote.Session's close never waits in a write of its own (X54 does not
// reach it): its detach is posted to the connection's writer, every wait for
// it ends with ctx, and the transport closed after it ends any write still
// blocked. Any other backend closes as it does: it has no detach to cut short.
func (c *dispatchConn) closeView() error {
	if q, ok := c.Backend.(quitCloser); ok {
		return q.CloseWithin(c.ctx)
	}
	return c.Backend.Close()
}

// closeNow closes b at once: over a socket, the transport closed with no
// detach (a quitCloser handed a context already done), which ends every write
// blocked on it; any other backend closes as it does.
func closeNow(b backend.Backend) error {
	c, ok := b.(quitCloser)
	if !ok {
		return b.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return c.CloseWithin(ctx)
}

// dispatchStep names a place in runDispatch a test holds it at.
type dispatchStep int

const (
	// dispatchStarted: Start has answered.
	dispatchStarted dispatchStep = iota + 1
	// dispatchPrompted: the prompt's Submit has answered in its time, and
	// the answer is about to be read.
	dispatchPrompted
	// dispatchLeaving: the prompt taken, its host about to be left running.
	dispatchLeaving
)

// dispatchHook, when a test sets it, is called at each dispatchStep on the
// dispatch's goroutine: a test holds a dispatch there to force a schedule (a
// quit landing between the prompt and the host being left running). nil in
// production, like gateHook.
var dispatchHook func(dispatchStep)

// ------------------------------------------------------- the unstarted session

// unstartedSession is a new session the list's input opened in place with a
// leading `@dir` alone (§3.13): shown — its band, an empty transcript, its
// composer — with nothing spawned until its first prompt. It is the model's
// session while it is shown (Model.unstarted), with no backend.
type unstartedSession struct {
	// draft is the key its composer's text is stashed under (draftKey): a
	// temporary id of the TUI's own, "\x00new:N", until its host answers with
	// the session's craze id (§3.11: the stash moves to the real id then).
	draft string
	// spec is where and as what it runs, captured at the enter that opened
	// it; models the catalog that names spec's model on the status row: the
	// session the list was opened from's, or /model's choice (sessNewModels).
	spec   SpawnSpec
	models []agent.ModelInfo
	// home is $HOME as the list read it: the band's `~`.
	home string
	// from is the row the list was on — the session it was opened from —
	// where a list opened over this session's discard puts its cursor.
	from sessKey
	// pending is the spawn its first prompt asked for, awaited (0: none), and
	// prompt that prompt.
	pending uint64
	prompt  string
}

// unstartedSpawnedMsg is the spawn of an unstarted session's first prompt:
// the host's ref and the backend Open answered for it, or why there is none.
// seq is the spawn it answers (unstartedSession.pending).
type unstartedSpawnedMsg struct {
	seq uint64
	ref roster.Ref
	b   backend.Backend
	err error
}

// firstPrompt is an unstarted session's first prompt while the session its
// spawn adopted is coming up (Model.first): sent through the ordinary submit
// path once it is up (the startedMsg arm), or — the start failing — the
// session is put back to unstarted as it was (from), its host (ref) stopped.
type firstPrompt struct {
	text string
	from unstartedSession
	ref  roster.Ref
}

// openUnstarted is enter on a leading `@dir` alone (§3.13): a new session in
// spec's directory opened in place, nothing spawned. It is a switch to a
// session with no backend: the composer's draft of the session the list was
// opened from is stashed under that session, the list closes with its
// roster, the session's state is made again by the shared constructor
// (withSession) for another session shown (shownGen), and the backend the
// list was opened from is let go of — a view close, the session going on on
// its host — at a backend generation of its own, so nothing its stream or
// start still hands up is this session's (bgen).
func (m Model) openUnstarted(spec SpawnSpec) (Model, tea.Cmd) {
	old := m.eng
	m.stashDraft()
	r := m.sessList.roster
	from := m.sessList.here
	if from.zero() {
		from = m.sessList.sel
	}
	u := &unstartedSession{spec: spec, models: m.sessNewModels(), home: m.sessList.home, from: from}
	m.sessList.in.closePopups()
	m.sessList = sessListState{gen: m.sessList.gen, byDir: m.sessList.byDir}
	next := m.withSession(sessionSeed{workspace: spec.Workspace, provider: spec.Provider.Name()})
	next.shownGen++
	next.setBackend(nil)
	next.bgen++
	next.unstartedSeq++
	u.draft = fmt.Sprintf("\x00new:%d", next.unstartedSeq)
	next.unstarted = u
	next.showUnstarted()
	next.dropStaleHeld()
	next.takeDraft()
	_ = next.input.Focus()
	next.setViewportContent(true)
	return next, tea.Batch(next.retire(old), next.sessRosters.closeCmd(r))
}

// showUnstarted draws the unstarted session as what it will run: its
// provider and model on the status row, named from the catalog it was opened
// with, and its permission mode on the chip. Nothing reads a backend: there
// is none (recompute does nothing without one).
func (m *Model) showUnstarted() {
	u := m.unstarted
	m.snap = agent.Snapshot{Provider: providerNamed(u.spec.Provider.Name()), CurrentModel: u.spec.Model, Models: u.models}
	m.model = u.spec.Model
	if m.model == "" {
		m.model = "default"
	}
	m.hostPerm = u.spec.PermissionMode
}

// enterUnstarted is enter in an unstarted session's composer: the prompt
// typed spawns the session's host and opens it (Sessions.Spawn, Open) off the
// Update, and waits for that spawn alone (unstartedSpawned). The prompt stays
// in the composer meanwhile. Nothing typed, a spawn already running, a
// builtin or a shell command — each needs a session to run in — do nothing.
func (m Model) enterUnstarted(builtin bool) (tea.Model, tea.Cmd) {
	u := m.unstarted
	text := strings.TrimSpace(m.input.Value())
	if u.pending != 0 || text == "" || builtin || m.shellMode() {
		return m, nil
	}
	m.unstartedSeq++
	nu := *u
	nu.pending, nu.prompt = m.unstartedSeq, text
	m.unstarted = &nu
	s, spec, seq := m.sessions, nu.spec, nu.pending
	return m, func() tea.Msg {
		ref, err := s.Spawn(spec)
		if err != nil {
			return unstartedSpawnedMsg{seq: seq, err: err}
		}
		b, err := s.Open(ref)
		return unstartedSpawnedMsg{seq: seq, ref: ref, b: b, err: err}
	}
}

// unstartedSpawned is the first prompt's spawn answering. Only the spawn the
// unstarted session shown waits for, while craze is not quitting, is acted
// on: one it no longer waits for — the session discarded, another opened —
// has its backend closed and its host, which nobody will use, stopped
// (abandonSpawn). A failure puts the session back as it was, not waiting,
// the error drawn and the prompt in the composer. A backend is adopted
// (adoptUnstarted).
func (m Model) unstartedSpawned(msg unstartedSpawnedMsg) (tea.Model, tea.Cmd) {
	u := m.unstarted
	if u == nil || u.pending != msg.seq || m.quitting {
		return m, m.abandonSpawn(msg.b, msg.ref)
	}
	if msg.err != nil || msg.b == nil {
		err := msg.err
		if err == nil {
			err = errors.New("no session")
		}
		nu := *u
		nu.pending, nu.prompt = 0, ""
		m.unstarted = &nu
		m.addError(unstartedFailText(err))
		return m, m.abandonSpawn(msg.b, msg.ref)
	}
	next, cmd := m.adoptUnstarted(msg.b, *u, msg.ref)
	return next, cmd
}

// abandonSpawn closes b, the backend an unstarted session's spawn answered
// that nothing will adopt, and stops its host, which Spawn started for a
// session nobody will use (off the Update: a stop dials). Nothing without a
// backend: a spawn that failed left no host, and an Open that could not reach
// its host stopped it (X98).
func (m Model) abandonSpawn(b backend.Backend, ref roster.Ref) tea.Cmd {
	if b == nil {
		return nil
	}
	return tea.Batch(m.retire(b), stopSpawned(m.sessions, ref))
}

// stopSpawned stops ref's host off the Update, its answer dropped: a host a
// Spawn started for a session that will not run.
func stopSpawned(s Sessions, ref roster.Ref) tea.Cmd {
	if s == nil || ref.Host.ID == "" {
		return nil
	}
	return func() tea.Msg {
		_ = s.Stop(ref)
		return nil
	}
}

// adoptUnstarted adopts b, the host an unstarted session's first prompt
// spawned, as a session opened in place is adopted (switchBackend) — the
// state made again by the shared constructor, b adopted (a new session
// generation and backend generation), started and read as Init starts and
// reads one — but it is not another session shown: the one the user typed its
// first prompt into is the one coming up, so its composer (the prompt), its
// shown generation and whatever it asked for stay (X121's first adoption).
// Its draft's temporary id gives way to the session's craze id (draftKey).
// The prompt goes once the session is up (Model.first).
func (m Model) adoptUnstarted(b backend.Backend, u unstartedSession, ref roster.Ref) (Model, tea.Cmd) {
	info := b.Info()
	if id := info.CrazeSessionID; id != "" {
		m.moveDraft(u.draft, id)
	}
	ws := info.Workspace
	if ws == "" {
		ws = u.spec.Workspace
	}
	next := m.withSession(sessionSeed{workspace: ws, provider: info.Provider})
	next.setBackend(b)
	next.dropStaleHeld()
	_ = next.input.Focus()
	next.recompute()
	if next.model == "" && next.snap.CurrentModel != "" {
		next.model = next.snap.CurrentModel
	}
	if next.model == "" {
		next.model = "default"
	}
	next.setViewportContent(true)
	from := u
	from.pending, from.prompt = 0, ""
	next.first = &firstPrompt{text: u.prompt, from: from, ref: ref}
	return next, tea.Batch(next.startCmd(), next.readOn())
}

// sendFirst is the session an unstarted session's first prompt spawned coming
// up (the startedMsg arm): that prompt goes through the TUI's ordinary submit
// path — the composer's (submitOwn), with the TUI's own command numbering —
// and the composer is cleared as a send clears it, when it still holds that
// text.
func (m Model) sendFirst() (Model, tea.Cmd) {
	f := m.first
	m.first = nil
	if f == nil || !m.sessionReady() {
		return m, nil
	}
	return m.submitOwn(f.text, engine.SubmitQueue, submitted)
}

// backToUnstarted is the session an unstarted session's first prompt spawned
// failing to come up — its start failed, or its stream ended first: it is
// the unstarted session again, as it was before that prompt, with the error
// drawn and the prompt still in the composer (the composer is the TUI's, and
// nothing cleared it). The backend is let go of at a backend generation of
// its own, as a switch leaves one, and its host — Spawn's, which will not
// run a session — is stopped. The session shown is the same one throughout:
// its shown generation stays.
func (m Model) backToUnstarted(f firstPrompt, err error) (Model, tea.Cmd) {
	old := m.eng
	u := f.from
	next := m.withSession(sessionSeed{workspace: u.spec.Workspace, provider: u.spec.Provider.Name()})
	next.setBackend(nil)
	next.bgen++
	next.unstarted = &u
	next.showUnstarted()
	next.dropStaleHeld()
	_ = next.input.Focus()
	next.addError(unstartedFailText(err))
	next.setViewportContent(true)
	return next, tea.Batch(next.retire(old), stopSpawned(m.sessions, f.ref))
}

// unstartedFailText is an unstarted session's failure to start, as its error
// row says it: the launcher's leading "craze: " dropped, as the list's notes
// drop it.
func unstartedFailText(err error) string {
	return "could not start the session: " + strings.TrimPrefix(sanitizeLine(failureText(err)), "craze: ")
}

// discardUnstarted is `←` (or /sessions) before an unstarted session's first
// prompt has brought it up (§3.13): it is dropped, leaving nothing — no host
// (a spawn still running is abandoned where it answers, and its host
// stopped: unstartedSpawned), no draft under its temporary id, and no row.
// The model is left with no session: the list opened over it has nothing
// behind it (sessListState.none).
func (m Model) discardUnstarted() Model {
	if u := m.unstarted; u != nil {
		if _, ok := m.drafts[u.draft]; ok {
			d := maps.Clone(m.drafts)
			delete(d, u.draft)
			m.drafts = d
		}
	}
	m.unstarted = nil
	return m
}
