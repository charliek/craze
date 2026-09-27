package tui

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
)

// The prompt's call sites through the command gate (plan 027 §3.12, C18a):
// Submit from send, sendText, confirmStrongSend and sendQueuedNow, each with
// its whole post-call chain as the continuation. These are the site schedules
// TestTheGatedFrameSequenceIsTodays extends to (X34), the reshaped working
// frame (astra 1), and the Submit that never answers.

// schedRun is one Submit site driven the frame runner's way by direct Updates:
// a frameModel publishing after every message, every key followed by its sync
// token, every command sorted — the stream's reads dropped (the schedule
// reads the primary itself), the gated calls kept for the schedule to answer,
// the drains applied as soon as they are owed, and the handler commands a
// prepare runs by hand (a mode change's Set) kept aside.
type schedRun struct {
	t      *testing.T
	sync   bool
	f      frameModel
	sess   *scriptedSession
	stub   *Stub
	eng    *engine.Engine
	frames []frameState
	calls  []tea.Cmd
	others []tea.Cmd
	// cancels are the cancel commands the model handed back (cancelTurn's),
	// kept for a schedule to run when the runtime would (C18b's Ctrl+C).
	cancels []tea.Cmd
	drains  int
	// noDrain leaves owed drains for the schedule to apply (drainOwed), so a
	// release's own state can be read before anything held is applied.
	noDrain bool
	// blind runs messages through the model with no frame drawn or published:
	// a prepare whose frames no schedule compares, which costs no View.
	blind bool
	n     int
}

// newSchedRun is a started model at a fixed clock with its frames frozen, so
// two runs' frames compare, in the gate mode asked for. Its session is the
// scripted decorator over the Stub — a prompt with no script, and a cancel
// with no hold, are the Stub's own — so a site can pin what the engine does
// after the Submit (submitSite.pin).
func newSchedRun(t *testing.T, sync bool) *schedRun {
	t.Helper()
	isolateSkillsHome(t)
	sess := newScriptedSession()
	t.Cleanup(func() { _ = sess.Close() })
	m := startSession(t, sess, frameWorkspace(t), 80, 24)
	m.gateSync = sync
	fixed := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	m.clock = func() time.Time { return fixed }
	m.frozen = true
	r := &schedRun{t: t, sync: sync, f: frameModel{inner: m, bus: newFrameBus(nil)}, sess: sess, stub: sess.Stub, eng: engineOf(t, m)}
	r.feed()
	return r
}

func (r *schedRun) m() Model { return r.f.inner }

func (r *schedRun) sort(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch name := cmdFuncName(cmd); {
	case strings.HasPrefix(name, teaPkg+"compactCmds"), strings.HasPrefix(name, teaPkg+"Batch"):
		if b, ok := cmd().(tea.BatchMsg); ok {
			for _, c := range b {
				r.sort(c)
			}
		}
	case name == tuiPkg+"drainNext":
		r.drains++
	case strings.HasPrefix(name, tuiPkg+"Model.run.func"):
		r.calls = append(r.calls, cmd)
	case strings.HasPrefix(name, tuiPkg+"Model.applyMode"), strings.HasPrefix(name, tuiPkg+"Model.implementPlan"):
		r.others = append(r.others, cmd)
	case strings.HasPrefix(name, tuiPkg+"Model.cancelTurn"):
		r.cancels = append(r.cancels, cmd)
	}
}

// step is one message through the frameModel, and every drain it owes.
func (r *schedRun) step(msg tea.Msg) {
	r.t.Helper()
	var cmd tea.Cmd
	if r.blind {
		var tm tea.Model
		tm, cmd = r.f.inner.Update(msg)
		r.f.inner = tm.(Model)
	} else {
		var tm tea.Model
		tm, cmd = r.f.Update(msg)
		r.f = tm.(frameModel)
		r.frames = append(r.frames, r.f.bus.last())
	}
	r.sort(cmd)
	if !r.noDrain {
		r.drainOwed()
	}
}

// drainOwed applies every drain the model owes, as they come due.
func (r *schedRun) drainOwed() {
	r.t.Helper()
	for r.drains > 0 {
		r.drains--
		r.step(drainMsg{})
	}
}

// answer runs every gated call waiting and applies its reply at once: a
// prepare's gates, which no schedule is about.
func (r *schedRun) answer() {
	r.t.Helper()
	for len(r.calls) > 0 {
		c := r.calls[0]
		r.calls = r.calls[1:]
		r.step(runWatched(r.t, c))
	}
}

// key is a prepare's key: sent, its gate (if any) answered, then its token.
func (r *schedRun) key(k tea.Msg) {
	r.t.Helper()
	r.step(k)
	r.answer()
	r.token()
}

func (r *schedRun) token() {
	r.n++
	r.step(frameSyncMsg{n: r.n})
}

func (r *schedRun) typeText(s string) {
	r.t.Helper()
	for _, k := range s {
		r.key(runeKey(k))
	}
}

// pending is the stream as the primary holds it: flushed, then everything on
// it.
func (r *schedRun) pending() []agent.Event {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	if err := r.eng.Sync(ctx); err != nil {
		r.t.Fatalf("sync: %v", err)
	}
	var out []agent.Event
	for {
		select {
		case ev := <-r.eng.Events():
			out = append(out, ev)
		default:
			return out
		}
	}
}

// until reads the stream until done says the events read are all the site's,
// then everything else already flushed.
func (r *schedRun) until(done func([]agent.Event) bool) []agent.Event {
	r.t.Helper()
	var out []agent.Event
	timeout := time.After(pumpWatchdog)
	for done != nil && !done(out) {
		select {
		case ev := <-r.eng.Events():
			out = append(out, ev)
		case <-timeout:
			r.t.Fatalf("the site's events did not all arrive in %s: %d read", pumpWatchdog, len(out))
		}
	}
	return append(out, r.pending()...)
}

// feed applies every event the stream holds now.
func (r *schedRun) feed() {
	r.t.Helper()
	r.stepEvents(r.pending())
}

// feedUntil applies the stream's events until done.
func (r *schedRun) feedUntil(done func([]agent.Event) bool) {
	r.t.Helper()
	r.stepEvents(r.until(done))
}

// stepEvents is each of evs through step, in order.
func (r *schedRun) stepEvents(evs []agent.Event) {
	r.t.Helper()
	for _, ev := range evs {
		r.step(eventMsg{ev})
	}
}

// runOthers runs the handler commands a prepare kept aside and applies their
// answers.
func (r *schedRun) runOthers() tea.Msg {
	r.t.Helper()
	var last tea.Msg
	for len(r.others) > 0 {
		c := r.others[0]
		r.others = r.others[1:]
		last = runWatched(r.t, c)
	}
	return last
}

// endings counts the turns the events end.
func endings(n int) func([]agent.Event) bool {
	return func(evs []agent.Event) bool {
		got := 0
		for _, ev := range evs {
			if ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.Phase == agent.TurnEnded {
				got++
			}
		}
		return got >= n
	}
}

// aWorkingTurn is a prepare: a prompt whose turn stays open until cancelled,
// applied until the model shows it working, and open at the session. A cancel
// that reaches a prompt the session is still claiming withdraws it instead
// (Stub.HangNext) — no ending of its own, the engine's synthetic one — and at
// one CPU the claim can still be under way when a schedule's cancel (an arm's,
// Ctrl+C's) lands, which made the cancel's events a matter of how the
// goroutines were scheduled.
func aWorkingTurn(r *schedRun) {
	r.t.Helper()
	hung := r.stub.HangNext()
	r.typeText("go")
	r.key(enter())
	r.feed()
	if r.m().status != statusWorking {
		r.t.Fatalf("prepare: the held turn left the model %s", r.m().status)
	}
	select {
	case <-hung:
	case <-time.After(pumpWatchdog):
		r.t.Fatal("prepare: the hung turn never opened")
	}
}

// holdTheCancel is a pin: the cancel an arm asks for waits at the session.
func holdTheCancel(r *schedRun) func() { return r.sess.HoldNextCancel() }

// submitSite is one of the prompt's call sites as a schedule.
type submitSite struct {
	name string
	// prepare brings the model to the moment before the keys that send, the
	// same in both modes; its frames are not compared.
	prepare func(r *schedRun)
	// typed is the draft typed first, each rune with its token; keys are the
	// keys after it, each with its token, the last of which issues the Submit
	// unless issue says otherwise. Enter's token (S) is the last key's.
	typed string
	keys  []tea.Msg
	// issue, when set, is the message that issues the Submit, produced after
	// the keys (the plan offer's SetMode answer, P); nil is the last key.
	issue func(r *schedRun) tea.Msg
	// pin, when set, holds what the engine goes on to do after the Submit on
	// its own goroutines — the cancel an arm asks for, the turn a send
	// starts — until the Submit has returned and the band has been read with
	// it, and answers the release, which runs then. Without it the schedule
	// would race the engine: the synchronous read right after Submit could
	// find a row armed as a send-now already fired and gone, in either mode.
	pin func(r *schedRun) (release func())
	// done says the stream has carried all of the Submit's events.
	done func([]agent.Event) bool
	// split is how many of the Submit's events arrive as the first held
	// event (E1), the rest arriving together as E2; zero is one arrival (E1).
	split int
	// held is a key typed after the sending one — held while the gate is
	// open (K).
	held tea.Msg
	// waits is a chained wait run over the frames after Enter's barrier.
	waits []waitSpec
}

// submitSchedule runs one site in one gate mode and one arrival order ("S"
// the sending key's token, "R" the Submit's reply, "E1"/"E2" its events, "K"
// the held key, "P" the issuing message where the site has one). It answers
// every frame published and the token's number.
func submitSchedule(t *testing.T, site submitSite, sync bool, order []string) ([]frameState, int) {
	t.Helper()
	r := newSchedRun(t, sync)
	if site.prepare != nil {
		site.prepare(r)
	}
	r.typeText(site.typed)
	for _, k := range site.keys[:len(site.keys)-1] {
		r.key(k)
	}
	last := site.keys[len(site.keys)-1]
	arrivals := map[string]tea.Msg{"K": site.held}
	release := func() {}
	if site.pin != nil {
		release = site.pin(r)
	}
	var issue tea.Msg
	if site.issue == nil {
		r.step(last)
		r.n++
		arrivals["S"] = frameSyncMsg{n: r.n}
	} else {
		r.step(last)
		r.n++
		arrivals["S"] = frameSyncMsg{n: r.n}
		issue = site.issue(r)
	}
	// The Submit runs where it runs: inside the issuing Update in the
	// baseline, from its gated call here. Its events exist only once it has.
	issued := func() {
		defer release()
		if sync {
			return
		}
		if len(r.calls) != 1 {
			t.Fatalf("the site issued %d gated calls, want its Submit", len(r.calls))
		}
		arrivals["R"] = runWatched(t, r.calls[0])
		r.calls = nil
	}
	var evs []agent.Event
	collect := func() {
		evs = r.until(site.done)
		if len(evs) == 0 {
			t.Fatal("the Submit published nothing")
		}
		split := site.split
		if split == 0 || split > len(evs) {
			split = len(evs)
		}
		arrivals["E1"], arrivals["E2"] = evs[:split], evs[split:]
	}
	if issue == nil {
		issued()
		collect()
	}
	for _, a := range order {
		switch a {
		case "P":
			r.step(issue)
			issued()
			collect()
			continue
		}
		msg, ok := arrivals[a]
		if !ok {
			continue
		}
		switch msg := msg.(type) {
		case []agent.Event:
			r.stepEvents(msg)
		default:
			r.step(msg)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("%d gated calls were left unanswered", len(r.calls))
	}
	return r.frames, r.n
}

// siteBaseline is what an async arrival order is for the gateSync baseline:
// the same order with no reply — the call returned inside the issuing Update.
// The token keeps its place (C17c): acknowledged after every message that
// arrived before it, in both modes — at the release when it arrived with
// nothing held (under a gate its own key opened, or with no gate at all),
// behind the held messages otherwise.
func siteBaseline(order []string) []string {
	return slices.DeleteFunc(slices.Clone(order), func(a string) bool { return a == "R" })
}

// siteOrders is every arrival order of the labels in which the events keep
// Seq order (one reader delivers them in it), and a reply or an event never
// precedes the message that issues the Submit.
func siteOrders(labels []string) [][]string {
	var out [][]string
	var permute func(prefix, rest []string)
	permute = func(prefix, rest []string) {
		if len(rest) == 0 {
			at := func(a string) int { return slices.Index(prefix, a) }
			if e2 := at("E2"); e2 >= 0 && at("E1") > e2 {
				return
			}
			if p := at("P"); p >= 0 && (at("R") < p || at("E1") < p) {
				return
			}
			out = append(out, slices.Clone(prefix))
			return
		}
		for i := range rest {
			permute(append(prefix, rest[i]), slices.Concat(rest[:i:i], rest[i+1:]))
		}
	}
	permute(nil, labels)
	return out
}

// submitSites is the prompt's call sites (§5's C18a row), each in the schedule
// that reaches it: a plain send that starts a turn; one that queues behind a
// working turn, with the next rune held (the queue-rows-* hazard, §2.7: the
// queued Enter's draft clear lands before it); a send-now that arms; a queued
// row sent now while idle, and one confirmed as a send-now while working; and
// the plan offer's implement prompt.
func submitSites() []submitSite {
	ctrlL := tea.KeyMsg{Type: tea.KeyCtrlL}
	up := tea.KeyMsg{Type: tea.KeyUp}
	return []submitSite{
		{
			name:  "a send that starts a turn",
			typed: "hello",
			keys:  []tea.Msg{enter()},
			done:  endings(1),
			split: 1,
			held:  runeKey('k'),
			waits: []waitSpec{{kind: "working"}, {kind: "text", needle: "echo: hello"}, {kind: "idle"}},
		},
		{
			name:    "a send queued behind a working turn",
			prepare: aWorkingTurn,
			typed:   "Reply with MANGO",
			keys:    []tea.Msg{enter()},
			held:    runeKey('R'),
			waits:   []waitSpec{{kind: "text", needle: "#1 Reply with MANGO"}, {kind: "gone", needle: "❯ Reply with MANGO"}},
		},
		{
			name:    "a send-now that arms",
			prepare: aWorkingTurn,
			typed:   "NOW",
			keys:    []tea.Msg{ctrlL, enter()},
			pin:     holdTheCancel,
			done:    endings(2),
			split:   1,
			held:    runeKey('k'),
			waits:   []waitSpec{{kind: "working"}, {kind: "text", needle: "echo: NOW"}, {kind: "idle"}},
		},
		{
			// Two rows stranded in the band of an idle session (queued-only),
			// the last one sent now: the band keeps a row, so where the
			// keyboard goes after the send is the continuation's to say, and
			// the row left behind drains when the sent one's turn ends.
			name: "a queued row sent now",
			prepare: func(r *schedRun) {
				for _, text := range []string{"ROW", "NEXT"} {
					if _, err := r.eng.Queue(engine.Command{}, text); err != nil {
						r.t.Fatalf("prepare: queueing: %v", err)
					}
				}
				r.feed()
				r.key(tea.KeyMsg{Type: tea.KeyUp})
				if sel, _ := r.m().queueSelected(); !r.m().queueFocus || sel.Text != "NEXT" {
					r.t.Fatalf("prepare: ↑ did not reach the band's last row (focus %v, %+v)", r.m().queueFocus, sel)
				}
			},
			keys: []tea.Msg{ctrlL},
			pin: func(r *schedRun) func() {
				return r.sess.Script(scriptHeld()).Release
			},
			done:  endings(2),
			split: 1,
			held:  runeKey('k'),
			waits: []waitSpec{{kind: "working"}, {kind: "text", needle: "echo: ROW"}, {kind: "idle"}},
		},
		{
			name: "a queued row confirmed as a send-now",
			prepare: func(r *schedRun) {
				aWorkingTurn(r)
				r.typeText("ROW")
				r.key(enter())
				r.feed()
				if len(r.m().queue) != 1 {
					r.t.Fatalf("prepare: the row is not queued: %+v", r.m().queue)
				}
			},
			keys:  []tea.Msg{up, ctrlL, enter()},
			pin:   holdTheCancel,
			done:  endings(2),
			split: 1,
			held:  runeKey('k'),
			waits: []waitSpec{{kind: "working"}, {kind: "text", needle: "echo: ROW"}, {kind: "idle"}},
		},
		{
			name: "the plan offer's implement prompt",
			prepare: func(r *schedRun) {
				r.key(tea.KeyMsg{Type: tea.KeyShiftTab})
				r.step(r.runOthers())
				r.feed()
				if r.m().snap.CurrentMode != "plan" {
					r.t.Fatalf("prepare: mode %q, want plan", r.m().snap.CurrentMode)
				}
				r.typeText("plan it")
				r.key(enter())
				r.feedUntil(endings(1))
				if !r.m().planOffering() {
					r.t.Fatal("prepare: the plan-mode turn left no offer")
				}
			},
			keys: []tea.Msg{enter()},
			issue: func(r *schedRun) tea.Msg {
				msg := r.runOthers()
				if _, ok := msg.(planImplementMsg); !ok {
					r.t.Fatalf("the offer's SetMode answered %T, want planImplementMsg", msg)
				}
				// The mode change's own delta is on the stream ahead of it.
				r.feed()
				return msg
			},
			done:  endings(1),
			split: 1,
			held:  runeKey('k'),
			waits: []waitSpec{{kind: "working"}, {kind: "text", needle: "follow-up:"}, {kind: "idle"}},
		},
	}
}

// siteLabels is a site's arrivals.
func siteLabels(site submitSite) []string {
	labels := []string{"R", "S", "K", "E1"}
	if site.split > 0 {
		labels = append(labels, "E2")
	}
	if site.issue != nil {
		labels = append(labels, "P")
	}
	return labels
}

// submitFrameSequences is TestTheGatedFrameSequenceIsTodays at the prompt's
// call sites (X34): for every order in which the Submit's reply, its own
// events, the sending key's sync token and a key typed after it can arrive,
// the frames a wait can see after the token's barrier — the same frames,
// carrying the same sync values — are the gateSync baseline's for the order
// today's code would have seen, a chained wait over them matches the same
// frames in both modes, and no frame published while the gate was open
// carries a token that arrived after the Submit was issued.
func submitFrameSequences(t *testing.T) {
	for _, site := range submitSites() {
		t.Run(site.name, func(t *testing.T) {
			orders := siteOrders(siteLabels(site))
			if len(orders) == 0 {
				t.Fatal("no arrival orders")
			}
			for _, order := range orders {
				t.Run(strings.Join(order, ","), func(t *testing.T) {
					base := siteBaseline(order)
					bf, bn := submitSchedule(t, site, true, base)
					af, an := submitSchedule(t, site, false, order)
					if bn != an {
						t.Fatalf("the tokens differ: %d, %d", bn, an)
					}
					want, got := matchable(bf, bn), matchable(af, an)
					if len(want) == 0 {
						t.Fatal("the baseline published no frame after the token's barrier")
					}
					if !reflect.DeepEqual(want, got) {
						t.Fatalf("the matchable frames differ (baseline order %v)\n%s", base, frameSeqDiff(want, got))
					}
					for _, fast := range []bool{true, false} {
						bw, aw := chained(bf, bn, fast, site.waits...), chained(af, an, fast, site.waits...)
						if !slices.Equal(bw, aw) {
							t.Fatalf("a chained wait (fast runner %v) matched differently:\ngateSync %q\nasync    %q", fast, bw, aw)
						}
					}
					// A token that arrived before the issuing message was
					// acknowledged before any gate opened; one that arrived after
					// it is the release's to acknowledge, never a gated frame's.
					late := slices.Index(order, "P") < slices.Index(order, "S")
					gated := 0
					for _, f := range af {
						if !f.gated {
							continue
						}
						gated++
						if late && f.sync >= an {
							t.Fatalf("a gated frame carries the token (sync %d)", f.sync)
						}
					}
					if gated == 0 {
						t.Fatal("the async run published no gated frame: the Submit did not go through the gate")
					}
				})
			}
		})
	}
}

// TestTheWorkingFrameSurvivesAShortTurn (astra 1; §3.12 "reshaped"): the whole
// `<enter><wait:working>` script over a turn so short that its started and its
// ended are both on the stream — and held — before the reply. The runner's
// sync token comes with its key (one message, C17c), so it is parked under the
// key's gate with nothing held, and acknowledged by the release, so the
// barrier's match is the release frame, which shows the turn working; the
// drained events' frames follow it, the ending last; and a <wait:working> run
// right after the barrier finds the release frame, one run after the drained
// frames were published finds the first of them — in the gateSync baseline
// and asynchronously alike. (Whether a runner that falls behind still sees a
// working frame is the frame harness's rendezvous, not this schedule's.)
func TestTheWorkingFrameSurvivesAShortTurn(t *testing.T) {
	site := submitSites()[0]
	order := []string{"S", "E1", "E2", "R"}
	var views [2][]frameSeqState
	for i, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			o := order
			if mode.sync {
				o = siteBaseline(order)
			}
			frames, n := submitSchedule(t, site, mode.sync, o)
			barrier := -1
			for i, f := range frames {
				if !f.gated && f.sync >= n {
					barrier = i
					break
				}
			}
			if barrier < 0 {
				t.Fatal("no frame acknowledged the token")
			}
			b := frames[barrier]
			if b.status != statusWorking {
				t.Fatalf("the barrier's match is %s, want the working frame:\n%s", b.status, b.plain)
			}
			if !mode.sync {
				if barrier == 0 || !frames[barrier-1].gated {
					t.Fatal("the barrier's match is not the release: the frame before it was not gated")
				}
				for _, f := range frames[:barrier] {
					if f.sync >= n {
						t.Fatal("a frame before the release acknowledged the token")
					}
				}
			}
			after := frames[barrier+1:]
			if len(after) == 0 || after[len(after)-1].status != statusIdle {
				t.Fatalf("the drained ending does not follow the barrier: %d frames after it", len(after))
			}
			for _, f := range after {
				if f.gated || f.sync < n {
					t.Fatalf("a frame after the barrier is gated (%v) or lost the token (sync %d)", f.gated, f.sync)
				}
			}
			if after[0].status != statusWorking {
				t.Fatalf("the first drained frame is %s, want working (the started)", after[0].status)
			}

			working := waitSpec{kind: "working"}
			// A runner that waits the moment its barrier returns: nothing has
			// been published after the release yet.
			bus := newFrameBus(nil)
			closed := make(chan struct{})
			close(closed)
			for _, f := range frames[:barrier+1] {
				bus.publish(f)
			}
			if _, ok := bus.await(func(s frameState) bool { return s.sync >= n }, 0, closed); !ok {
				t.Fatal("the barrier did not match")
			}
			s, ok := bus.await(working.match, 0, closed)
			if !ok || frameSeq(s) != frameSeq(b) {
				t.Fatalf("<wait:working> right after the barrier found %+v (ok %v), want the release frame", frameSeq(s), ok)
			}
			// A runner the drained frames were published ahead of: the first
			// drained frame, the started's, is the one it finds.
			got := chained(frames, n, true, working)
			if got[0] == "" || got[0] != fmt.Sprintf("%+v", frameSeq(after[0])) {
				t.Fatalf("<wait:working> after the drained frames found %q, want the first of them", got[0])
			}
			views[i] = matchable(frames, n)
		})
	}
	if !reflect.DeepEqual(views[0], views[1]) {
		t.Fatalf("the modes' matchable frames differ\n%s", frameSeqDiff(views[0], views[1]))
	}
}

// submitBackend is the in-process backend with Submit in a test's hands.
type submitBackend struct {
	backend.Backend
	submit func(ctx context.Context, c engine.Command, text string, mode engine.SubmitMode, fromRow string) (engine.SubmitResult, error)
}

func (b *submitBackend) Submit(ctx context.Context, c engine.Command, text string, mode engine.SubmitMode, fromRow string) (engine.SubmitResult, error) {
	return b.submit(ctx, c, text, mode, fromRow)
}

// neverAnswers is a Submit that does not answer before the test ends: it runs
// the engine's own first when ran says so — the answer lost — and not at all
// otherwise.
func neverAnswers(t *testing.T, inner backend.Backend, ran bool) *submitBackend {
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })
	return &submitBackend{Backend: inner, submit: func(ctx context.Context, c engine.Command, text string, mode engine.SubmitMode, fromRow string) (engine.SubmitResult, error) {
		if ran {
			if _, err := inner.Submit(ctx, c, text, mode, fromRow); err != nil {
				return engine.SubmitResult{}, err
			}
		}
		<-never
		return engine.SubmitResult{}, nil
	}}
}

// TestASubmitThatNeverAnswersKeepsTheDraft (§3.12 "On expiry"): a Submit that
// does not answer releases at its deadline with ErrNoAnswer, and its
// continuation says the outcome is unknown and claims nothing — the draft is
// kept, nothing is drawn, no echo marker is set, the shell context stays with
// the draft, the band shows the engine's queue — and a reply that turns up
// later is dropped. Both cases: the command never ran, and it ran with its
// answer lost; each from an idle model and behind a working turn.
func TestASubmitThatNeverAnswersKeepsTheDraft(t *testing.T) {
	prev := gateDeadline
	gateDeadline = 20 * time.Millisecond
	t.Cleanup(func() { gateDeadline = prev })

	for _, working := range []bool{false, true} {
		for _, ran := range []bool{false, true} {
			name := fmt.Sprintf("working %v, ran %v", working, ran)
			t.Run(name, func(t *testing.T) {
				m, stub := gatedModel(t)
				eng := engineOf(t, m)
				r := newGateRig(t, m)
				if working {
					stub.HangNext()
					r.m.input.SetValue("go")
					r.send(enter())
					r.answer()
					if r.m.status != statusWorking {
						t.Fatalf("setup: %s", r.m.status)
					}
				}
				r.m.shellCtx = []agent.ShellResult{{Command: "ls", Output: "a b"}}
				before := texts(r.m, entryUser)
				own, next, armed := r.m.ownTurn, r.m.nextTurn, r.m.armedDraft
				r.m.eng = neverAnswers(t, r.m.eng, ran)
				r.m.input.SetValue("lost prompt")
				r.send(enter())
				if r.m.gate == nil {
					t.Fatal("Enter opened no gate")
				}
				id := r.m.gate.id
				rep := r.answer()
				if rep.err != ErrNoAnswer {
					t.Fatalf("the reply is %v, want ErrNoAnswer", rep.err)
				}
				m = r.m
				if got := m.input.Value(); got != "lost prompt" {
					t.Fatalf("the draft is %q, want it kept", got)
				}
				if m.copyNote != noAnswerSubmitNote {
					t.Fatalf("the note is %q, want %q", m.copyNote, noAnswerSubmitNote)
				}
				if got := texts(m, entryUser); !slices.Equal(got, before) {
					t.Fatalf("user rows %q, want nothing drawn (%q)", got, before)
				}
				if m.ownTurn != own || m.nextTurn != next || m.armedDraft != armed {
					t.Fatalf("an echo marker was set: own %q, next %q, armed draft %q (before: %q, %q, %q)",
						m.ownTurn, m.nextTurn, m.armedDraft, own, next, armed)
				}
				if want := statusIdle; !working && m.status != want {
					t.Fatalf("status %s, want nothing begun", m.status)
				}
				if len(m.shellCtx) != 1 {
					t.Fatalf("the shell context went: %+v", m.shellCtx)
				}
				// The band is the engine's queue: the row a lost answer queued is
				// there, and nothing when the command never ran.
				eq := eng.State().Queue
				if !reflect.DeepEqual(m.queue, eq) {
					t.Fatalf("the band holds %+v, the engine %+v", m.queue, eq)
				}
				if queued := len(eq) == 1; queued != (working && ran) {
					t.Fatalf("the engine's queue %+v: working %v, ran %v", eq, working, ran)
				}

				// A reply for that gate turning up now: dropped, with nothing
				// applied.
				late := digestModel(&r.m)
				r.send(gateReply{id: id, result: engine.SubmitResult{Turn: "late"}})
				if changed := late.diff(digestModel(&r.m)); len(changed) != 0 {
					t.Fatalf("a late reply changed %v", changed)
				}
			})
		}
	}
}

// TestAQueuedRowSentNowThatNeverAnswersKeepsTheBand: sendQueuedNow's own
// post-call work is in the continuation too — a row sent now that starts a
// turn hands the keyboard to the composer — and an unanswered Submit starts
// nothing this client knows of, so the band keeps the keyboard, as for a row
// that could not start. Two rows are queued and the last is sent, so the band
// is still drawn afterwards: an emptied band gives the keyboard back on its
// own (syncQueue), which would hide where the send put it.
func TestAQueuedRowSentNowThatNeverAnswersKeepsTheBand(t *testing.T) {
	prev := gateDeadline
	gateDeadline = 20 * time.Millisecond
	t.Cleanup(func() { gateDeadline = prev })

	for _, answered := range []bool{true, false} {
		t.Run(fmt.Sprintf("answered %v", answered), func(t *testing.T) {
			m, _ := gatedModel(t)
			enqueueRow(t, m, "ROW")
			enqueueRow(t, m, "NEXT")
			if !answered {
				m.eng = neverAnswers(t, m.eng, false)
			}
			r := newGateRig(t, m)
			// The rows' own events, read as the model's reader reads them.
			for len(r.m.visibleQueue()) < 2 {
				r.read()
			}
			if answered {
				r.send(tea.KeyMsg{Type: tea.KeyUp})
				r.send(tea.KeyMsg{Type: tea.KeyCtrlL})
				if !r.m.queueFocus {
					t.Fatal("the band gave up the keyboard before the reply")
				}
				r.answer()
				if r.m.queueFocus || r.m.status != statusWorking || len(r.m.visibleQueue()) != 1 {
					t.Fatalf("a row that started a turn: queue focus %v, status %s, band %+v", r.m.queueFocus, r.m.status, r.m.queue)
				}
				return
			}
			r.send(tea.KeyMsg{Type: tea.KeyUp})
			r.send(tea.KeyMsg{Type: tea.KeyCtrlL})
			r.answer()
			if !r.m.queueFocus || r.m.status == statusWorking {
				t.Fatalf("an unanswered row: queue focus %v, status %s; want the band's keyboard kept", r.m.queueFocus, r.m.status)
			}
			if r.m.copyNote != noAnswerSubmitNote {
				t.Fatalf("the note is %q", r.m.copyNote)
			}
			if len(r.m.queue) != 2 {
				t.Fatalf("the band shows %+v", r.m.queue)
			}
		})
	}
}

// TestTheConfirmComesDownInTheUpdateThatAnswersIt: the send-now confirm's
// pre-call work stays in the issuing Update (§3.12 "Everything before the call
// stays where it is"): Enter on it takes the confirm down while the Submit is
// still waiting for its reply, and the arm lands with the reply.
func TestTheConfirmComesDownInTheUpdateThatAnswersIt(t *testing.T) {
	m, stub := gatedModel(t)
	stub.HangNext()
	r := newGateRig(t, m)
	r.m.input.SetValue("go")
	r.send(enter())
	r.answer()
	r.m.input.SetValue("NOW")
	r.send(tea.KeyMsg{Type: tea.KeyCtrlL})
	if r.m.confirm == nil {
		t.Fatal("Ctrl+L over a working turn raised no confirm")
	}
	r.send(enter())
	if r.m.gate == nil {
		t.Fatal("the confirm's Enter opened no gate")
	}
	if r.m.confirm != nil {
		t.Fatal("the confirm is still up while its Submit waits: taking it down is the issuing Update's work")
	}
	if r.m.cardMasking || r.m.armedDraft != "" {
		t.Fatal("the arm was applied before its reply")
	}
	r.answer()
	if !r.m.cardMasking || r.m.armedDraft == "" {
		t.Fatalf("the reply's arm was not applied: masking %v, armed draft %q", r.m.cardMasking, r.m.armedDraft)
	}
}

// TestAGatedCallsPanicReachesTheCommandsGoroutine: a gated call runs on a
// goroutine of the gate's own, where a panic would take the process down with
// the terminal still raw — no recover of bubbletea's reaches it. The gate
// carries it to the command's goroutine and re-raises it there, where
// bubbletea's does (TestFramePanicClosesThePickedSession, async), with the
// value it panicked with and the stack it panicked on.
func TestAGatedCallsPanicReachesTheCommandsGoroutine(t *testing.T) {
	m, _ := gatedModel(t)
	m, cmd := m.run(gateDeadline, func(context.Context, backend.Backend) (any, error) {
		panic("the call's own panic")
	}, noteCont("never"))
	if m.gate == nil || cmd == nil {
		t.Fatal("the call opened no gate")
	}
	caught := make(chan any, 1)
	go func() {
		defer func() { caught <- recover() }()
		cmd()
	}()
	var v any
	select {
	case v = <-caught:
	case <-time.After(pumpWatchdog):
		t.Fatal("the command never returned or panicked")
	}
	p, ok := v.(callPanic)
	if !ok {
		t.Fatalf("the command's goroutine recovered %#v, want the call's panic carried as a callPanic", v)
	}
	if p.value != "the call's own panic" || !strings.Contains(string(p.stack), "TestAGatedCallsPanicReachesTheCommandsGoroutine") {
		t.Fatalf("the carried panic is %v with stack\n%s", p.value, p.stack)
	}
	if s := fmt.Sprint(p); !strings.Contains(s, "the call's own panic") || !strings.Contains(s, "goroutine") {
		t.Fatalf("the printed panic says %q, want the value and the call's stack", s)
	}
}
