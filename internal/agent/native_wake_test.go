package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/tool"
)

// The native session's wake (plan 026 §3.11, §7 A14). Every case runs a real
// native session with background children on (Options.Interactive), its
// parent and children on the routing scripted model (native_subagents_test.go):
// a background child's answer is held by the test, so its result becomes
// pending exactly when the test says, and the wake's own step is held the same
// way, so the test acts inside the wake at a known point. The worker's
// rechecks are watched through the wakeDecided seam — each one reports whether
// it claimed or stood down — and the one schedule a Begin can win is forced
// through the wakeSeam barrier. Nothing sleeps but waitFor's poll of a state
// the session has already reached.
//
// Every later turn of a session sends its first prompt, "go", as the first
// user message of its request, so the wake's steps — and a later prompt's —
// are routed under "go" too, in the order the requests are made.

// wakeRig is a started interactive native session over routers, watched, with
// its recheck outcomes on a channel, every OnPending call on another (pending:
// a result's publication, which the roster's finished event precedes — astra
// r19 #2), and two hooks around the sink — sinkBefore runs at the head of
// the adapter's own sink, before it reads any event of a turn's or a child's
// (the sinkSeam); sinkAfter once the adapter has handled an event of the
// session-level sink, a background child's — for the schedules only the sink
// can force: a call's acknowledgement held while its child finishes (F5), a
// child paused between its finished event and its publication, a panic
// inside a wake. Both are nil until a test sets them.
type wakeRig struct {
	t          *testing.T
	f          *nativeFixture
	a          *nativeRouter
	s          *nativeSession
	w          *nativeWatcher
	decided    chan bool
	pending    chan struct{}
	sinkBefore atomic.Pointer[func(harness.Event)]
	sinkAfter  atomic.Pointer[func(harness.Event)]
}

// wakeSeams is what a test installs before the session starts: the worker's
// two seams, the log's hooks and its observer (the engine's own hook,
// log.Observe, which a session without an engine leaves to the test).
type wakeSeams struct {
	seam    func()
	ended   func()
	log     *logHooks
	observe func(Event)
}

// newWakeRig starts the session; seam, when set, is the worker's wakeSeam.
func newWakeRig(t *testing.T, opts Options, seam func()) *wakeRig {
	t.Helper()
	return newWakeRigWith(t, opts, wakeSeams{seam: seam})
}

// newWakeRigWith is newWakeRig with every seam.
func newWakeRigWith(t *testing.T, opts Options, seams wakeSeams) *wakeRig {
	t.Helper()
	f, r := routedNative(t)
	opts.Interactive = true
	rig := &wakeRig{t: t, f: f, a: r["test/a"], decided: make(chan bool, 64), pending: make(chan struct{}, 64)}
	models := f.edit
	f.edit = func(o *harness.Options) {
		models(o)
		// The adapter's own wiring, wrapped: the kick first, as OnPending is
		// the kick, then the test's signal; the sink with the hooks around it.
		o.OnPending = func() {
			rig.s.kickWake()
			rig.pending <- struct{}{}
		}
		// The session-level sink, for the after hook: a background child's
		// own events and its finish come this way, and nothing of a turn's.
		o.Sink = func(ev harness.Event) {
			rig.s.sink(ev)
			if h := rig.sinkAfter.Load(); h != nil {
				(*h)(ev)
			}
		}
	}
	s := f.session(opts)
	rig.s = s
	s.mu.Lock()
	s.wakeSeam = seams.seam
	s.wakeEnded = seams.ended
	s.wakeDecided = func(claimed bool) { rig.decided <- claimed }
	// The before hook sits at the head of the adapter's own sink, which every
	// event reaches — a turn's through Run's sink, a child's through the one
	// above — on the goroutine that handed it over.
	s.sinkSeam = func(ev harness.Event) {
		if h := rig.sinkBefore.Load(); h != nil {
			(*h)(ev)
		}
	}
	s.mu.Unlock()
	// Before Start, as the engine installs its observer and the log's tests
	// their hooks: nothing has been enqueued or published yet.
	if seams.log != nil {
		s.log.hooks = seams.log
	}
	if seams.observe != nil {
		if err := s.log.Observe(seams.observe); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	takeStartDelta(t, s.log)
	rig.w = newNativeWatcher(t, s)
	return rig
}

// hookAfter sets the sink's after hook; nil clears it.
func (rig *wakeRig) hookAfter(h func(harness.Event)) {
	if h == nil {
		rig.sinkAfter.Store(nil)
		return
	}
	rig.sinkAfter.Store(&h)
}

// hookBefore sets the sink's before hook; nil clears it.
func (rig *wakeRig) hookBefore(h func(harness.Event)) {
	if h == nil {
		rig.sinkBefore.Store(nil)
		return
	}
	rig.sinkBefore.Store(&h)
}

// awaitPending waits for the next OnPending: a result published.
func (rig *wakeRig) awaitPending(why string) {
	rig.t.Helper()
	await(rig.t, rig.pending, "a result's publication ("+why+")")
}

// bgCall is one agent call asking for the background.
func bgCall(t *testing.T, id, description, prompt string) []fantasy.StreamPart {
	t.Helper()
	return nativeCallParts(id, tool.AgentTool, nativeArgs(t, map[string]any{
		"description": description, "prompt": prompt, "run_in_background": true,
	}))
}

// awaitDecided waits for the worker's next recheck and fails unless it came
// to want: true claimed a wake, false stood down.
func (rig *wakeRig) awaitDecided(want bool, why string) {
	rig.t.Helper()
	if got := await(rig.t, rig.decided, "the wake worker's recheck ("+why+")"); got != want {
		rig.t.Fatalf("the recheck (%s) claimed=%v, want %v", why, got, want)
	}
}

// noRecheck fails if a recheck has run and not been received.
func (rig *wakeRig) noRecheck(why string) {
	rig.t.Helper()
	select {
	case got := <-rig.decided:
		rig.t.Fatalf("a recheck ran (claimed=%v) where none was due: %s", got, why)
	default:
	}
}

// isBracket accepts a wake bracket: the opening when running, else the ending.
func isBracket(running bool) func(Event) bool {
	return func(ev Event) bool {
		return ev.Type == EventForeignTurn && ev.ForeignTurn != nil && ev.ForeignTurn.Running == running
	}
}

// bracket waits for the n-th bracket of the kind and checks its shape: the
// wake's id, its reason on both ends, and the text on the opening alone.
func (rig *wakeRig) bracket(running bool, n int, id string) Event {
	rig.t.Helper()
	kind := "ending"
	if running {
		kind = "opening"
	}
	rig.w.waitCount("the "+kind+" bracket", n, isBracket(running))
	var got Event
	seen := 0
	for _, ev := range rig.w.events() {
		if isBracket(running)(ev) {
			seen++
			if seen == n {
				got = ev
				break
			}
		}
	}
	f := got.ForeignTurn
	text := ""
	if running {
		text = wakeText
	}
	if f.ID != id || f.Reason != ReasonSubagentWake || f.Text != text {
		rig.t.Fatalf("the %s bracket is %+v; want id %s, reason %s, text %q", kind, f, id, ReasonSubagentWake, text)
	}
	return got
}

// noTerminalAfterTheFirst fails if the session emitted anything but the one
// EventDone the user's turn owes: a wake ends in neither an EventDone nor an
// EventError.
func (rig *wakeRig) noTerminalAfterTheFirst(dones int) {
	rig.t.Helper()
	if d, e := rig.w.count(EventDone), rig.w.count(EventError); d != dones || e != 0 {
		rig.t.Fatalf("%d EventDone and %d EventError, want %d and 0: %s", d, e, dones, strings.Join(rig.w.kinds(), ", "))
	}
}

// messageTexts is every text part of a message, joined: a step-0 delivery is
// a second text part beside the prompt's.
func messageTexts(m fantasy.Message) string {
	var b strings.Builder
	for _, p := range m.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}

// lastUserTexts is the text of a request's last message, which must be a
// user one.
func lastUserTexts(t *testing.T, c fantasy.Call) string {
	t.Helper()
	m := c.Prompt[len(c.Prompt)-1]
	if m.Role != fantasy.MessageRoleUser {
		t.Fatalf("the request ends with a %s message", m.Role)
	}
	return messageTexts(m)
}

// resultRequests is every request of the parent's whose last user message
// carries a delivered sub-agent result.
func (rig *wakeRig) resultRequests() []fantasy.Call {
	var out []fantasy.Call
	for _, c := range rig.a.requests() {
		m := c.Prompt[len(c.Prompt)-1]
		if m.Role == fantasy.MessageRoleUser && strings.Contains(messageTexts(m), "<subagent_result") {
			out = append(out, c)
		}
	}
	return out
}

// spawnOne runs the first turn, "go", which starts one background child whose
// answer child holds, and answers "started"; it returns the child's id once
// the turn has ended and the release's recheck has stood down (nothing was
// pending: the child is held). more are the parent's later steps under "go".
func (rig *wakeRig) spawnOne(child *held, more ...step) string {
	rig.t.Helper()
	rig.a.route("go", append([]step{callsStep(bgCall(rig.t, "a1", "job", "child work")), answer("started")}, more...)...)
	rig.a.route("child work", child.step(openTextParts("did "), closeTextParts("work")))
	if _, err := rig.s.Prompt(context.Background(), "go"); err != nil {
		rig.t.Fatalf("Prompt: %v", err)
	}
	rig.w.waitType(EventDone)
	rig.awaitDecided(false, "the turn's release, the child still running")
	return spawnedWith(rig.t, rig.w.events(), "child work")
}

// finish lets the held child finish and waits for its result to be published
// — OnPending, which follows the roster's finished event: a test that waited
// on the event alone could act before the result was pending (astra r19 #2).
func (rig *wakeRig) finish(child *held, id string) {
	rig.t.Helper()
	await(rig.t, child.reached, "the child's step")
	close(child.release)
	rig.w.wait("the child's finished row", func(ev Event) bool { return isRoster(ev, id, SubagentChangeFinished) })
	rig.awaitPending("child " + id)
}

// TestNativeWakeBrackets (A14): a background child's result finishing while
// nothing runs starts one wake — bracketed by EventForeignTurn{Running: true,
// Text: "sub-agent result", Reason: "subagent_wake"} before any of its output
// and {Running: false} after, no EventDone and no EventError — whose request
// carries the result as its user message; ForeignTurn() and Snapshot()
// agree through it, Interject is refused as no turn, the settings verbs work
// as during a turn, and the ending's own recheck stands down. The background
// call's row was final at its acknowledgement: stamped with the child's id,
// model and Background inside the call, closed by its ToolFinished, and never
// republished when the child finished (F5); the roster row carries
// Background.
func TestNativeWakeBrackets(t *testing.T) {
	rig := newWakeRig(t, Options{}, nil)
	s, w := rig.s, rig.w
	child, wake := newHeld(t), newHeld(t)
	id := rig.spawnOne(child, wake.step(openTextParts("noted: "), closeTextParts("child done")))
	evs := w.events()

	spawned := indexWhere(evs, 0, func(ev Event) bool { return isRoster(ev, id, SubagentChangeSpawned) })
	if !evs[spawned].Subagent.Background {
		t.Fatalf("the spawned row is %+v; want Background", evs[spawned].Subagent)
	}
	done := indexWhere(evs, 0, func(ev Event) bool { return isParentRowDone(ev, "t1.1.1") })
	if done < 0 {
		t.Fatalf("the call's row never closed: %s", strings.Join(w.kinds(), ", "))
	}
	if task := evs[done].Tool.Task; task == nil || !task.Background || task.AgentID != id || task.Model != "test/a" ||
		!task.Receipt || task.Status != SubagentCompleted {
		t.Fatalf("the call's row at its acknowledgement is %+v (task %+v)", evs[done].Tool, evs[done].Tool.Task)
	}
	if s.ForeignTurn() || s.Snapshot().ForeignTurn {
		t.Fatal("a foreign turn before any wake")
	}

	rig.finish(child, id)
	rig.awaitDecided(true, "the result pending")
	rig.bracket(true, 1, "wake-1")
	await(t, wake.reached, "the wake's step")
	// Mid-wake: the flag and the snapshot agree, an interjection is refused as
	// no turn, and a setting changes as it would during a turn.
	if !s.ForeignTurn() || !s.Snapshot().ForeignTurn {
		t.Fatal("ForeignTurn() and Snapshot().ForeignTurn must both be true during a wake")
	}
	if err := s.Interject(context.Background(), "psst"); !errors.Is(err, ErrNotInTurn) {
		t.Fatalf("Interject during a wake: %v, want ErrNotInTurn", err)
	}
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatalf("SetModel during a wake: %v", err)
	}
	// The opening bracket is in the record before any of the wake's output:
	// the first main-session text after the user's turn ended is the wake's,
	// and it follows the bracket. The held step has yielded its text, which
	// the harness publishes on its own goroutine, so the watcher is the
	// barrier.
	w.wait("the wake's first text", func(ev Event) bool { return ev.Type == EventText && ev.Agent == "" && ev.Text == "noted: " })
	evs = w.events()
	opening := indexWhere(evs, 0, isBracket(true))
	ended := indexWhere(evs, 0, func(ev Event) bool { return ev.Type == EventDone })
	first := indexWhere(evs, ended+1, func(ev Event) bool { return ev.Type == EventText && ev.Agent == "" })
	if !ascending(ended, opening, first) {
		t.Fatalf("the turn ended at %d, the opening bracket is at %d and the wake's first text at %d", ended, opening, first)
	}
	close(wake.release)
	rig.bracket(false, 1, "wake-1")
	rig.awaitDecided(false, "the wake's ending, nothing pending")

	evs = w.events()
	rig.noTerminalAfterTheFirst(1)
	if got := joined(evs[opening:], EventText); got != "noted: child done" {
		t.Fatalf("the wake's text is %q", got)
	}
	if later := indexWhere(evs, done+1, func(ev Event) bool {
		return ev.Type == EventTool && ev.Agent == "" && ev.Tool.ID == "t1.1.1"
	}); later >= 0 {
		t.Fatalf("the background call's row was republished after its acknowledgement: %+v", evs[later].Tool)
	}
	fin := evs[indexWhere(evs, 0, func(ev Event) bool { return isRoster(ev, id, SubagentChangeFinished) })].Subagent
	if !fin.Background || fin.Status != SubagentCompleted || fin.Output != "did work" {
		t.Fatalf("the finished roster row is %+v", fin)
	}
	if s.ForeignTurn() || s.Snapshot().ForeignTurn || s.hs.HasPending() {
		t.Fatal("after the wake: the flags must be down and nothing pending")
	}
	reqs := rig.resultRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests carried a result, want the wake's one", len(reqs))
	}
	if text := lastUserTexts(t, reqs[0]); !strings.Contains(text, `<subagent_result id="`+id+`"`) || !strings.Contains(text, "did work") {
		t.Fatalf("the wake's user message is %q", text)
	}
	if got := s.Snapshot().CurrentModel; got != "test/b" {
		t.Fatalf("the model switched during the wake is %q", got)
	}
}

// TestNativeWakeRacesBegin (§3.11, X30): the wake's claim and Begin's are one
// s.mu section each, so whichever takes s.mu first wins. Wake first: Begin's
// continuation returns ErrForeignTurn with nothing sent and nothing emitted,
// the wake's claim untouched, and a Begin after the wake's ending runs. Begin
// first — the worker parked between its reading and its claim, the window the
// race is in — the wake stands down, and that turn takes the pending result at
// its first step, so no bracket is ever published.
func TestNativeWakeRacesBegin(t *testing.T) {
	t.Run("the wake claims first", func(t *testing.T) {
		rig := newWakeRig(t, Options{}, nil)
		s, w := rig.s, rig.w
		child, wake := newHeld(t), newHeld(t)
		id := rig.spawnOne(child, wake.step(openTextParts("noted"), closeTextParts()), answer("after the wake"))
		rig.finish(child, id)
		rig.awaitDecided(true, "the result pending")
		rig.bracket(true, 1, "wake-1")
		await(t, wake.reached, "the wake's step")
		// The held step's text is published on the harness's goroutine after
		// the yield: once it is in the record the wake is quiet until released.
		w.wait("the wake's text", func(ev Event) bool { return ev.Type == EventText && ev.Agent == "" && ev.Text == "noted" })

		before := len(w.events())
		run := s.Begin("later")
		res, err := run(context.Background())
		if !errors.Is(err, ErrForeignTurn) || !zeroResult(res) {
			t.Fatalf("Begin during a wake = %+v, %v; want ErrForeignTurn", res, err)
		}
		if got := len(w.events()); got != before {
			t.Fatalf("the refused Begin emitted %d events", got-before)
		}
		if !s.ForeignTurn() {
			t.Fatal("the refusal touched the wake's claim")
		}
		rig.noRecheck("a refused Begin releases nothing")
		close(wake.release)
		rig.bracket(false, 1, "wake-1")
		rig.awaitDecided(false, "the wake's ending")
		if _, err := s.Prompt(context.Background(), "later"); err != nil {
			t.Fatalf("a prompt after the wake: %v", err)
		}
		w.waitTerminals(2)
		rig.noTerminalAfterTheFirst(2)
	})

	t.Run("Begin claims first", func(t *testing.T) {
		parked, resume := make(chan struct{}), make(chan struct{})
		var once sync.Once
		rig := newWakeRig(t, Options{}, func() {
			once.Do(func() { close(parked) })
			<-resume
		})
		t.Cleanup(func() { closeOnce(resume) })
		s, w := rig.s, rig.w
		child := newHeld(t)
		id := rig.spawnOne(child, answer("saw it"))
		rig.finish(child, id)
		await(t, parked, "the worker parked before its claim")

		run := s.Begin("later")
		close(resume)
		rig.awaitDecided(false, "Begin claimed first")
		res, err := run(context.Background())
		if err != nil || res.StopReason != "end_turn" {
			t.Fatalf("the turn that won = %+v, %v", res, err)
		}
		w.waitTerminals(2)
		rig.awaitDecided(false, "the turn's release, the result taken")
		if n := w.count(EventForeignTurn); n != 0 {
			t.Fatalf("%d brackets published; the wake stood down", n)
		}
		reqs := rig.resultRequests()
		if len(reqs) != 1 {
			t.Fatalf("%d requests carried the result, want the turn's one", len(reqs))
		}
		wantDelivery(t, reqs[0], "later", id)
		if s.hs.HasPending() {
			t.Fatal("the result is still pending after the turn took it")
		}
	})
}

// wantDelivery checks a user turn's request carries prompt as a user message
// and the result of child id as the user message after it (the harness's
// step-0 delivery: its own entry, after the prompt's).
func wantDelivery(t *testing.T, c fantasy.Call, prompt, id string) {
	t.Helper()
	var users []string
	for _, m := range c.Prompt {
		if m.Role == fantasy.MessageRoleUser {
			users = append(users, messageTexts(m))
		}
	}
	n := len(users)
	if n < 2 || users[n-2] != prompt || !strings.Contains(users[n-1], `<subagent_result id="`+id+`"`) {
		t.Fatalf("the request's user messages are %q; want %q and then the result of %s", users, prompt, id)
	}
}

// TestPendingRecheckedOnEveryRelease (§3.11, P23): a result that becomes
// pending after a turn's last step boundary — its OnPending kick spent on a
// recheck that found the claim held — is delivered by the wake the claim's
// release rechecks for, on every path the claim is released: a turn's
// success, its cancel, its failure, a claimed prompt's withdrawal, and a
// wake's own ending, for a second child that finished during it.
func TestPendingRecheckedOnEveryRelease(t *testing.T) {
	// heldLast is a parent turn whose last step is held after the child was
	// started: the child finishes during it, past every step boundary.
	heldLast := func(t *testing.T, tail []fantasy.StreamPart) (*wakeRig, *held, *held) {
		t.Helper()
		rig := newWakeRig(t, Options{}, nil)
		child, last := newHeld(t), newHeld(t)
		rig.a.route("go", callsStep(bgCall(t, "a1", "job", "child work")),
			last.step(openTextParts("thinking"), tail), answer("delivered"))
		rig.a.route("child work", child.step(openTextParts("did "), closeTextParts("work")))
		return rig, child, last
	}
	// childFinishesDuring lets the child finish while the parent's last step
	// is held: the kick stands down, the claim being held.
	childFinishesDuring := func(t *testing.T, rig *wakeRig, child, last *held) string {
		t.Helper()
		await(t, last.reached, "the parent's last step")
		// The spawned row was published before the call returned, but the
		// watcher reads on a goroutine of its own: its wait is the barrier.
		id := rig.w.wait("the child's spawned row", func(ev Event) bool {
			return ev.Type == EventSubagent && ev.SubagentChange == SubagentChangeSpawned && ev.Subagent.Prompt == "child work"
		}).Subagent.ID
		rig.finish(child, id)
		rig.awaitDecided(false, "the child's result pending during the parent's last step")
		return id
	}
	// delivered checks the one wake that followed carried the child's result.
	delivered := func(t *testing.T, rig *wakeRig, id string) {
		t.Helper()
		rig.awaitDecided(true, "the release")
		rig.bracket(true, 1, "wake-1")
		rig.bracket(false, 1, "wake-1")
		rig.awaitDecided(false, "the wake's ending")
		reqs := rig.resultRequests()
		if len(reqs) != 1 || !strings.Contains(lastUserTexts(t, reqs[0]), `<subagent_result id="`+id+`"`) {
			t.Fatalf("%d requests carried a result; want the wake's one with %s", len(reqs), id)
		}
		if n := rig.w.count(EventForeignTurn); n != 2 {
			t.Fatalf("%d brackets, want one wake's two", n)
		}
	}

	t.Run("a turn's success", func(t *testing.T) {
		rig, child, last := heldLast(t, closeTextParts(" aloud"))
		go rig.s.Prompt(context.Background(), "go") //nolint:errcheck // the record is checked below
		id := childFinishesDuring(t, rig, child, last)
		close(last.release)
		rig.w.waitType(EventDone)
		delivered(t, rig, id)
		rig.noTerminalAfterTheFirst(1)
	})

	t.Run("a cancel", func(t *testing.T) {
		rig, child, last := heldLast(t, closeTextParts())
		go rig.s.Prompt(context.Background(), "go") //nolint:errcheck // the record is checked below
		id := childFinishesDuring(t, rig, child, last)
		if out, err := rig.s.Cancel(context.Background()); err != nil || !out.Wrote || !out.Settled {
			t.Fatalf("Cancel = %+v, %v", out, err)
		}
		if ev := rig.w.waitType(EventDone); ev.StopReason != "cancelled" {
			t.Fatalf("the cancelled turn ended %q", ev.StopReason)
		}
		delivered(t, rig, id)
	})

	t.Run("an error", func(t *testing.T) {
		rig, child, last := heldLast(t, errorParts(errors.New("provider down")))
		go rig.s.Prompt(context.Background(), "go") //nolint:errcheck // the record is checked below
		id := childFinishesDuring(t, rig, child, last)
		close(last.release)
		rig.w.waitType(EventError)
		delivered(t, rig, id)
		if d := rig.w.count(EventDone); d != 0 {
			t.Fatalf("%d EventDone after a failed turn and a wake", d)
		}
	})

	t.Run("a withdrawal", func(t *testing.T) {
		rig := newWakeRig(t, Options{}, nil)
		s := rig.s
		child := newHeld(t)
		id := rig.spawnOne(child, answer("delivered"))
		run := s.Begin("never sent")
		rig.finish(child, id)
		rig.awaitDecided(false, "the result pending behind a claim not yet run")
		type cancelled struct {
			out CancelOutcome
			err error
		}
		back := make(chan cancelled, 1)
		go func() {
			out, err := s.Cancel(context.Background())
			back <- cancelled{out, err}
		}()
		// Cancel marks the claim and then waits for its release, which the
		// withdrawal below is; the mark is the one thing to wait for first.
		waitFor(t, "the cancel to mark the claim", func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.cancelling
		})
		if res, err := run(context.Background()); !errors.Is(err, ErrPromptCancelled) || !zeroResult(res) {
			t.Fatalf("the withdrawn continuation = %+v, %v", res, err)
		}
		if got := await(t, back, "Cancel to return"); got.err != nil || got.out.Wrote || !got.out.Withdrew || !got.out.Settled {
			t.Fatalf("Cancel = %+v", got)
		}
		delivered(t, rig, id)
		rig.noTerminalAfterTheFirst(1)
	})

	t.Run("a wake's own ending", func(t *testing.T) {
		rig := newWakeRig(t, Options{}, nil)
		a, w := rig.a, rig.w
		childA, childB, wakeA := newHeld(t), newHeld(t), newHeld(t)
		a.route("go", callsStep(bgCall(t, "a1", "A", "work A"), bgCall(t, "a2", "B", "work B")), answer("started"),
			wakeA.step(openTextParts("got A"), closeTextParts()), answer("got B"))
		a.route("work A", childA.step(openTextParts("A "), closeTextParts("done")))
		a.route("work B", childB.step(openTextParts("B "), closeTextParts("done")))
		if _, err := rig.s.Prompt(context.Background(), "go"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		w.waitType(EventDone)
		rig.awaitDecided(false, "the turn's release")
		idA, idB := spawnedWith(t, w.events(), "work A"), spawnedWith(t, w.events(), "work B")

		rig.finish(childA, idA)
		rig.awaitDecided(true, "A pending")
		rig.bracket(true, 1, "wake-1")
		await(t, wakeA.reached, "the first wake's step")
		// B's kick lands while the worker itself is running the wake: it
		// waits in the slot, and the ending's recheck is what takes it up.
		// finish returns once B's result is published (OnPending), so the
		// ending's recheck finds it pending by construction.
		rig.finish(childB, idB)
		rig.noRecheck("the worker is running the wake")
		close(wakeA.release)
		rig.bracket(false, 1, "wake-1")
		rig.awaitDecided(true, "the first wake's ending, B pending")
		rig.bracket(true, 2, "wake-2")
		rig.bracket(false, 2, "wake-2")
		rig.awaitDecided(false, "the second wake's ending")
		wantTwoWakes(t, rig, idA, idB)
	})

	// astra r19 #2's schedule, forced: B's finished event is published but B
	// pauses before its result is (the sink's after hook holds it), the first
	// wake ends, and its recheck rightly finds nothing pending — a test that
	// took the roster event for the publication would have expected a claim
	// here. B's publication then kicks the worker, which delivers it.
	t.Run("a child published after the wake's ending", func(t *testing.T) {
		rig := newWakeRig(t, Options{}, nil)
		a, w := rig.a, rig.w
		childA, childB, wakeA := newHeld(t), newHeld(t), newHeld(t)
		a.route("go", callsStep(bgCall(t, "a1", "A", "work A"), bgCall(t, "a2", "B", "work B")), answer("started"),
			wakeA.step(openTextParts("got A"), closeTextParts()), answer("got B"))
		a.route("work A", childA.step(openTextParts("A "), closeTextParts("done")))
		a.route("work B", childB.step(openTextParts("B "), closeTextParts("done")))
		if _, err := rig.s.Prompt(context.Background(), "go"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		w.waitType(EventDone)
		rig.awaitDecided(false, "the turn's release")
		idA, idB := spawnedWith(t, w.events(), "work A"), spawnedWith(t, w.events(), "work B")

		rig.finish(childA, idA)
		rig.awaitDecided(true, "A pending")
		rig.bracket(true, 1, "wake-1")
		await(t, wakeA.reached, "the first wake's step")

		holdB := make(chan struct{})
		rig.hookAfter(func(ev harness.Event) {
			if fin, ok := ev.(harness.SubagentFinished); ok && fin.ID == idB {
				<-holdB
			}
		})
		await(t, childB.reached, "B's step")
		close(childB.release)
		w.wait("B's finished row", func(ev Event) bool { return isRoster(ev, idB, SubagentChangeFinished) })
		close(wakeA.release)
		rig.bracket(false, 1, "wake-1")
		rig.awaitDecided(false, "the first wake's ending: B not yet published")
		if rig.s.hs.HasPending() {
			t.Fatal("B is pending before its publication")
		}
		close(holdB)
		rig.awaitPending("B")
		rig.awaitDecided(true, "B's publication")
		rig.bracket(true, 2, "wake-2")
		rig.bracket(false, 2, "wake-2")
		rig.awaitDecided(false, "the second wake's ending")
		wantTwoWakes(t, rig, idA, idB)
	})
}

// wantTwoWakes checks two wakes ran, the first carrying A alone and the second
// B alone, and no terminal event followed the user's turn.
func wantTwoWakes(t *testing.T, rig *wakeRig, idA, idB string) {
	t.Helper()
	reqs := rig.resultRequests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests carried a result, want two wakes'", len(reqs))
	}
	first, second := lastUserTexts(t, reqs[0]), lastUserTexts(t, reqs[1])
	if !strings.Contains(first, idA) || strings.Contains(first, idB) || !strings.Contains(second, idB) || strings.Contains(second, idA) {
		t.Fatalf("the wakes carried\n %q\n %q", first, second)
	}
	rig.noTerminalAfterTheFirst(1)
}

// TestWakeFencedAtClaimAndValidation (§3.11), at native's level: while the
// engine's admission fence is up the worker refuses to claim — a result that
// becomes pending then waits, no bracket is published and ForeignTurn stays
// false — and the fence coming down is what claims. A fence raised while a
// wake runs neither cancels nor ends it; the ending's recheck then stands
// down with a second result pending, until the down.
func TestWakeFencedAtClaimAndValidation(t *testing.T) {
	rig := newWakeRig(t, Options{}, nil)
	a, s, w := rig.a, rig.s, rig.w
	childA, childB, wakeA := newHeld(t), newHeld(t), newHeld(t)
	a.route("go", callsStep(bgCall(t, "a1", "A", "work A"), bgCall(t, "a2", "B", "work B")), answer("started"),
		wakeA.step(openTextParts("got A"), closeTextParts()), answer("got B"))
	a.route("work A", childA.step(openTextParts("A "), closeTextParts("done")))
	a.route("work B", childB.step(openTextParts("B "), closeTextParts("done")))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	w.waitType(EventDone)
	rig.awaitDecided(false, "the turn's release")
	idA, idB := spawnedWith(t, w.events(), "work A"), spawnedWith(t, w.events(), "work B")

	s.FenceUp()
	rig.finish(childA, idA) // returns once A's result is published
	rig.awaitDecided(false, "A pending under the fence")
	if s.ForeignTurn() || w.count(EventForeignTurn) != 0 || !s.hs.HasPending() {
		t.Fatal("a wake claimed under the fence")
	}
	s.FenceDown()
	rig.awaitDecided(true, "the fence down")
	rig.bracket(true, 1, "wake-1")
	await(t, wakeA.reached, "the first wake's step")

	s.FenceUp()
	if !s.ForeignTurn() {
		t.Fatal("the fence ended the running wake")
	}
	rig.finish(childB, idB) // B's result published, not merely its row ended
	rig.noRecheck("the worker is running the wake")
	close(wakeA.release)
	rig.bracket(false, 1, "wake-1")
	rig.awaitDecided(false, "the wake's ending under the fence")
	if w.count(EventForeignTurn) != 2 || !s.hs.HasPending() {
		t.Fatal("a second wake claimed under the fence")
	}
	s.FenceDown()
	rig.awaitDecided(true, "the fence down again")
	rig.bracket(true, 2, "wake-2")
	rig.bracket(false, 2, "wake-2")
	rig.awaitDecided(false, "the second wake's ending")
	rig.noTerminalAfterTheFirst(1)
}

// TestNativeWakeCancel (§3.11): Cancel during a wake cancels it as a turn —
// Wrote, and settled once the wake's ending is out — with no EventDone: the
// harness suspends the result the wake had taken and not persisted, so no
// second wake follows, and the next turn a person starts delivers it at its
// first step. The wake is held before it has streamed anything: a wake
// cancelled after streaming text persists that text, interrupted, and the
// append commits the result it carried (§3.11: a successful append beats a
// cancellation) — delivered, not suspended.
func TestNativeWakeCancel(t *testing.T) {
	rig := newWakeRig(t, Options{}, nil)
	s, w := rig.s, rig.w
	child, wake := newHeld(t), newHeld(t)
	id := rig.spawnOne(child, wake.step(nil, answerParts("noted")), answer("took it"))
	rig.finish(child, id)
	rig.awaitDecided(true, "the result pending")
	rig.bracket(true, 1, "wake-1")
	await(t, wake.reached, "the wake's step")

	out, err := s.Cancel(context.Background())
	if err != nil || !out.Wrote || out.Withdrew || !out.Settled {
		t.Fatalf("Cancel during a wake = %+v, %v", out, err)
	}
	rig.bracket(false, 1, "wake-1")
	rig.awaitDecided(false, "the cancelled wake's ending: the result suspended")
	if s.hs.HasPending() || s.ForeignTurn() {
		t.Fatal("a cancelled wake's result is suspended, never pending")
	}
	rig.noTerminalAfterTheFirst(1)

	if _, err := s.Prompt(context.Background(), "later"); err != nil {
		t.Fatalf("Prompt after the cancelled wake: %v", err)
	}
	w.waitTerminals(2)
	rig.awaitDecided(false, "the later turn's release")
	if n := w.count(EventForeignTurn); n != 2 {
		t.Fatalf("%d brackets, want the one cancelled wake's", n)
	}
	reqs := rig.resultRequests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests carried the result, want the wake's and the later turn's", len(reqs))
	}
	wantDelivery(t, reqs[1], "later", id)
}

// answerParts is a held step's tail: text and a clean finish.
func answerParts(chunks ...string) []fantasy.StreamPart {
	return append(textParts(chunks...), finishParts(fantasy.FinishReasonStop)...)
}

// TestNativeWakeClose (§3.11): Close during a wake cancels it, joins the
// harness and the worker, and closes the log last — within the bound, nothing
// leaked. The journal has the wake's outcome and, from the harness's Close,
// the undelivered result's subagent_usage note: the child's id, undelivered,
// and its usage rows in the step note's scheme.
func TestNativeWakeClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	rig := newWakeRig(t, Options{JournalDir: dir}, nil)
	s := rig.s
	jw := journalOf(t, s.log)
	inc := s.Incarnation()
	child, wake := newHeld(t), newHeld(t)
	// Held before it streams, so Close persists nothing of the wake and the
	// result it took is suspended, never delivered (TestNativeWakeCancel).
	id := rig.spawnOne(child, wake.step(nil, answerParts("noted")))
	rig.finish(child, id)
	rig.awaitDecided(true, "the result pending")
	rig.bracket(true, 1, "wake-1")
	await(t, wake.reached, "the wake's step")

	closeJournaled(t, s, jw)
	select {
	case <-s.wakeDone:
	default:
		t.Fatal("Close returned without joining the wake worker")
	}
	if s.ForeignTurn() {
		t.Fatal("ForeignTurn after Close")
	}
	lines := assertOneJournal(t, dir, jw, inc)
	wakes := diags(lines, diagSubagentWake)
	if len(wakes) != 1 || wakes[0]["wake"] != "wake-1" || wakes[0]["outcome"] != "cancelled" {
		t.Fatalf("the wake's notes are %v; want wake-1 cancelled", wakes)
	}
	usage := diags(lines, diagSubagentUsage)
	if len(usage) != 1 {
		t.Fatalf("%d subagent_usage notes, want the undelivered result's one", len(usage))
	}
	n := usage[0]
	for k, v := range map[string]any{"subagent": id, "type": "general-purpose", "undelivered": true, "rows": float64(1),
		"provider_1": "test", "model_1": "test/a", "wire_model_1": "wire-a", "input_1": float64(10), "output_1": float64(5)} {
		if n[k] != v {
			t.Fatalf("the undelivered note's %s is %v, want %v (note %v)", k, n[k], v, n)
		}
	}
}

// TestNativeHeadlessBackgroundIsForeground (§3.11): a session that is not
// interactive — headless `craze prompt` — runs a run_in_background call in the
// foreground as before: the child finishes inside the call, its row and the
// roster row say nothing of the background, and no wake ever runs.
func TestNativeHeadlessBackgroundIsForeground(t *testing.T) {
	f, r := routedNative(t)
	s := f.started(Options{})
	w := newNativeWatcher(t, s)
	a := r["test/a"]
	a.route("go", callsStep(bgCall(t, "a1", "job", "child work")), answer("done"))
	a.route("child work", answer("did it"))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	w.waitType(EventDone)
	evs := w.events()
	id := spawnedWith(t, evs, "child work")
	finished := indexWhere(evs, 0, func(ev Event) bool { return isRoster(ev, id, SubagentChangeFinished) })
	done := indexWhere(evs, 0, func(ev Event) bool { return isParentRowDone(ev, "t1.1.1") })
	if !ascending(finished, done) {
		t.Fatalf("the child finished at %d and the call's row closed at %d; want the child inside the call", finished, done)
	}
	if evs[finished].Subagent.Background || evs[done].Tool.Task.Background {
		t.Fatal("a headless session marked the child background")
	}
	if evs[done].Tool.Task.Status != SubagentCompleted || evs[done].Tool.Task.DurationMs < 0 {
		t.Fatalf("the foreground call's row is %+v", evs[done].Tool.Task)
	}
	if n := w.count(EventForeignTurn); n != 0 {
		t.Fatalf("%d brackets in a headless session", n)
	}
}
