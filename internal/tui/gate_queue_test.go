package tui

import (
	"context"
	"errors"
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

// The queue verbs and the Ctrl+C and Esc chains through the command gate (plan
// 027 §3.12, C18b): Disarm (withdrawSendNow, from the Esc ladder and from
// clearPending), ClearQueue (clearPending, from Ctrl+C and /clear), Unqueue
// (Backspace on a row, a row's [cancel], an emptied edit) and EditQueued (a
// saved edit, and Ctrl+L's save-then-send), each with its whole post-call chain
// as its continuation. These are the site schedules
// TestTheGatedFrameSequenceIsTodays extends to (X34), the Ctrl+C chain run
// whole (astra 3), and the verbs that never answer.

// chainSite is one of C18b's sites as a schedule: the last of its keys issues
// a chain of gated calls — links, in order — and, for Ctrl+C, the chain's end
// hands back the cancel, which the schedule runs where the runtime would.
type chainSite struct {
	name string
	// prepare brings the model to the moment before the keys, the same in both
	// modes; its frames are not compared. It answers the release of what it
	// holds of the engine's own next step (a pin: the cancel an arm asked
	// for, the turn a send will start), or nil; the release runs once every
	// link's call has run, so no read a link's call takes races the engine.
	prepare func(r *schedRun) (release func())
	// keys are the keys, each followed by its token; the last issues the chain.
	keys func(r *schedRun) []tea.Msg
	// links is how many gated calls the chain makes; quiet names the links
	// that publish nothing (a Disarm refused because nothing is armed).
	links int
	quiet []int
	// after says the stream has carried what follows the pin's release (X).
	after func([]agent.Event) bool
	// cancel: the chain's end hands back Ctrl+C's cancel. cancelEnds says it
	// ends a turn (its events, C); without it the turn is over by then and the
	// cancel publishes nothing.
	cancel, cancelEnds bool
	// held is a key typed after the issuing one (K).
	held tea.Msg
	// still: the issuing Update's pre-call work draws nothing, so its (gated)
	// frame is the frame before the key — what the continuation does is not
	// there until the reply (§3.12 "Everything before the call stays").
	still bool
	// waits is a chained wait run over the frames after the key's barrier.
	waits []waitSpec
}

func (s chainSite) link(i int) (reply, events string) {
	return fmt.Sprintf("R%d", i), fmt.Sprintf("E%d", i)
}

// labels is the site's arrivals: the key's token S, the held key K, each
// link's reply and — unless it is quiet — its events, what follows the pin's
// release X, and the cancel's events C.
func (s chainSite) labels() []string {
	l := []string{"S"}
	if s.held != nil {
		l = append(l, "K")
	}
	for i := 1; i <= s.links; i++ {
		reply, events := s.link(i)
		l = append(l, reply)
		if !slices.Contains(s.quiet, i) {
			l = append(l, events)
		}
	}
	if s.after != nil {
		l = append(l, "X")
	}
	if s.cancel && s.cancelEnds {
		l = append(l, "C")
	}
	return l
}

// chainOrders is every arrival order of the site's labels that an
// asynchronous run can see: the stream's arrivals in Seq order (one reader
// delivers them in it — each link's events, then X, then C); a link's reply
// and events after the reply of the link before it, whose release issues it;
// X after the last link's call can have run; C after the chain's end.
func chainOrders(s chainSite) [][]string {
	valid := func(o []string) bool {
		at := func(a string) int { return slices.Index(o, a) }
		var seq []string
		for i := 1; i <= s.links; i++ {
			_, events := s.link(i)
			seq = append(seq, events)
		}
		seq = append(seq, "X", "C")
		prev := -1
		for _, a := range seq {
			if i := at(a); i >= 0 {
				if i < prev {
					return false
				}
				prev = i
			}
		}
		for i := 1; i < s.links; i++ {
			r := at(fmt.Sprintf("R%d", i))
			next, events := s.link(i + 1)
			if at(next) < r {
				return false
			}
			if e := at(events); e >= 0 && e < r {
				return false
			}
		}
		if x := at("X"); x >= 0 && s.links > 1 && x < at(fmt.Sprintf("R%d", s.links-1)) {
			return false
		}
		if c := at("C"); c >= 0 && c < at(fmt.Sprintf("R%d", s.links)) {
			return false
		}
		return true
	}
	var out [][]string
	var permute func(prefix, rest []string)
	permute = func(prefix, rest []string) {
		if len(rest) == 0 {
			if valid(prefix) {
				out = append(out, slices.Clone(prefix))
			}
			return
		}
		for i := range rest {
			permute(append(prefix, rest[i]), slices.Concat(rest[:i:i], rest[i+1:]))
		}
	}
	permute(nil, s.labels())
	return out
}

// chainBaseline is what an async arrival order is for the gateSync baseline:
// the same order with no replies — every link ran inside the key's Update. The
// token keeps its place (C17c): one that arrived with nothing held (right
// behind its key, or after replies alone) is the chain's end's to acknowledge
// (TestASyncTokenWaitsForTheChainsEnd), and one behind held messages is
// acknowledged when drained, after them — in both modes.
func chainBaseline(order []string, _ int) []string {
	return slices.DeleteFunc(slices.Clone(order), func(a string) bool { return strings.HasPrefix(a, "R") })
}

// parkedToken reports that the key's token was parked under the chain: it
// arrived before the chain's end with only replies before it, so nothing was
// held when it came.
func parkedToken(order []string, links int) bool {
	s := slices.Index(order, "S")
	if s > slices.Index(order, fmt.Sprintf("R%d", links)) {
		return false
	}
	for _, a := range order[:s] {
		if !strings.HasPrefix(a, "R") {
			return false
		}
	}
	return true
}

// chainSchedule runs one site in one gate mode and one arrival order. It
// answers every frame published, the last key's token, and how many events
// each link published: the asynchronous run reads each link's on their own,
// and the baseline, whose key's Update ran every link, splits what it caused
// by those counts.
func chainSchedule(t *testing.T, site chainSite, sync bool, order []string, counts []int) ([]frameState, int, []int) {
	t.Helper()
	r := newSchedRun(t, sync)
	// Nothing before the last key is compared, so nothing before it is drawn
	// but the frame just before it, which the issuing Update's is held to.
	r.blind = true
	release := func() {}
	if site.prepare != nil {
		if rel := site.prepare(r); rel != nil {
			release = rel
			t.Cleanup(rel)
		}
	}
	keys := site.keys(r)
	for _, k := range keys[:len(keys)-1] {
		r.key(k)
	}
	r.blind = false
	r.f.publish()
	before := r.f.bus.last()
	r.frames = append(r.frames, before)
	r.step(keys[len(keys)-1])
	r.n++
	arrivals := map[string]any{"S": frameSyncMsg{n: r.n}}
	if site.held != nil {
		arrivals["K"] = site.held
	}
	pinned := func() {
		release()
		if site.after != nil {
			arrivals["X"] = r.until(site.after)
		}
	}
	cancelled := func() {
		if len(r.cancels) != 1 {
			t.Fatalf("the chain's end handed back %d cancels, want Ctrl+C's one", len(r.cancels))
		}
		c := r.cancels[0]
		r.cancels = nil
		if msg := runWatched(t, c); msg != nil {
			t.Fatalf("the cancel answered %#v", msg)
		}
		var evs []agent.Event
		if site.cancelEnds {
			evs = r.until(endings(1))
		} else {
			evs = r.pending()
		}
		if site.cancelEnds != (len(evs) > 0) {
			t.Fatalf("the cancel published %d events (ends a turn: %v)", len(evs), site.cancelEnds)
		}
		if len(evs) > 0 {
			arrivals["C"] = evs
		}
	}
	got := make([]int, site.links)
	placeEvents := func(i int, evs []agent.Event) {
		_, events := site.link(i)
		if quiet := slices.Contains(site.quiet, i); quiet != (len(evs) == 0) {
			t.Fatalf("link %d published %d events (quiet: %v)", i, len(evs), quiet)
		}
		if len(evs) > 0 {
			arrivals[events] = evs
		}
	}
	link := 0
	// issued runs the call the open link is waiting on, asynchronously: its
	// reply and its events arrive where the order puts them.
	issued := func() {
		if len(r.calls) != 1 {
			t.Fatalf("link %d: %d gated calls waiting, want one", link+1, len(r.calls))
		}
		link++
		reply, _ := site.link(link)
		arrivals[reply] = runWatched(t, r.calls[0])
		r.calls = nil
		evs := r.pending()
		got[link-1] = len(evs)
		placeEvents(link, evs)
		if link == site.links {
			pinned()
		}
	}
	if sync {
		if len(r.calls) != 0 {
			t.Fatalf("the baseline left %d gated calls", len(r.calls))
		}
		evs := r.pending()
		for i, n := range counts {
			if n > len(evs) {
				t.Fatalf("the baseline published %d events for the links, fewer than the async run's %v", len(evs), counts)
			}
			placeEvents(i+1, evs[:n])
			evs = evs[n:]
		}
		if len(evs) != 0 {
			t.Fatalf("the baseline published %d events more than the async run's links %v", len(evs), counts)
		}
		pinned()
		if site.cancel {
			cancelled()
		}
	} else {
		issuing := r.frames[len(r.frames)-1]
		if !issuing.gated {
			t.Fatal("the key opened no gate: the chain did not go through it")
		}
		if site.still && issuing.plain != before.plain {
			t.Fatalf("the issuing Update drew more than its pre-call work:\n--- before ---\n%s\n--- issuing ---\n%s", before.plain, issuing.plain)
		}
		issued()
	}
	ended := sync
	for _, a := range order {
		msg, ok := arrivals[a]
		if !ok {
			t.Fatalf("arrival %s never came", a)
		}
		switch msg := msg.(type) {
		case []agent.Event:
			r.stepEvents(msg)
		default:
			r.step(msg)
		}
		if !sync && strings.HasPrefix(a, "R") && link == site.links && !ended {
			// The last link's reply: the chain's end, and the one place its
			// cancel may be handed back.
			ended = true
			if site.cancel {
				cancelled()
			}
		}
		if !ended && len(r.cancels) != 0 {
			t.Fatalf("a cancel was handed back after %s, before the chain's end", a)
		}
		if !sync && strings.HasPrefix(a, "R") && !ended {
			issued()
		}
	}
	if !sync && link != site.links {
		t.Fatalf("the chain made %d gated calls, want %d", link, site.links)
	}
	if len(r.calls) != 0 || len(r.cancels) != 0 {
		t.Fatalf("%d gated calls and %d cancels were left", len(r.calls), len(r.cancels))
	}
	return r.frames, r.n, got
}

// queuedBehind is a prepare's step: a working turn with rows queued behind it.
func queuedBehind(r *schedRun, rows ...string) {
	r.t.Helper()
	aWorkingTurn(r)
	for _, text := range rows {
		r.typeText(text)
		r.key(enter())
		r.feed()
	}
	if got := len(r.m().queue); got != len(rows) {
		r.t.Fatalf("prepare: %d rows queued, want %d", got, len(rows))
	}
}

// armedOver is a prepare's step: a send-now armed over the working turn, with
// the cancel the arm asked for held at the session. It answers that cancel's
// release.
func armedOver(r *schedRun) func() {
	r.t.Helper()
	return armedHeld(r, holdTheCancel(r))
}

// armedOverPastItsDeadline is armedOver with the arm's cancel held past the
// engine's own deadline for it (HoldNextCancelPastItsDeadline): for a test
// whose property needs the turn alive until it lets the cancel go, however
// slowly the run goes (sol r46 4).
func armedOverPastItsDeadline(r *schedRun) func() {
	r.t.Helper()
	return armedHeld(r, r.sess.HoldNextCancelPastItsDeadline())
}

// armedHeld arms the send-now with the cancel's hold, release, already in
// place.
func armedHeld(r *schedRun, release func()) func() {
	r.t.Helper()
	r.typeText("NOW")
	r.key(tea.KeyMsg{Type: tea.KeyCtrlL})
	r.key(enter())
	r.feed()
	if !r.m().sendNowPending() || r.m().armedDraft == "" {
		r.t.Fatalf("prepare: no send-now armed (pending %v, armed draft %q)", r.m().sendNowPending(), r.m().armedDraft)
	}
	return release
}

// strandedRows is a prepare's step: rows in the band of an idle session
// (queued-only), which nothing drains until a turn is sent.
func strandedRows(r *schedRun, rows ...string) {
	r.t.Helper()
	for _, text := range rows {
		if _, err := r.eng.Queue(engine.Command{}, text); err != nil {
			r.t.Fatalf("prepare: queueing: %v", err)
		}
	}
	r.feed()
	if got := len(r.m().queue); got != len(rows) {
		r.t.Fatalf("prepare: %d rows stranded, want %d", got, len(rows))
	}
}

func theKeys(k ...tea.Msg) func(*schedRun) []tea.Msg {
	return func(*schedRun) []tea.Msg { return k }
}

func runesThen(s string, k ...tea.Msg) func(*schedRun) []tea.Msg {
	return func(*schedRun) []tea.Msg {
		var out []tea.Msg
		for _, c := range s {
			out = append(out, runeKey(c))
		}
		return append(out, k...)
	}
}

// chainSites is C18b's sites (§5's row; the brief's list): Esc withdrawing an
// armed send-now; Ctrl+C's chain — clearPending's one call, then the cancel —
// over a queued row and over an armed send-now too; /clear; an edit saved; an
// emptied edit; Backspace on a row; a click on a row's [cancel]; and Ctrl+L's
// save-then-send, two links.
func chainSites() []chainSite {
	ctrlC := tea.KeyMsg{Type: tea.KeyCtrlC}
	ctrlL := tea.KeyMsg{Type: tea.KeyCtrlL}
	esc := tea.KeyMsg{Type: tea.KeyEsc}
	up := tea.KeyMsg{Type: tea.KeyUp}
	bs := tea.KeyMsg{Type: tea.KeyBackspace}
	return []chainSite{
		{
			// The withdrawal's note is the model's own, and its delta the echo
			// the disarmed marker skips; the cancelled turn then settles into
			// nothing.
			name: "Esc withdrawing an armed send-now",
			prepare: func(r *schedRun) func() {
				aWorkingTurn(r)
				return armedOver(r)
			},
			keys:  theKeys(esc),
			links: 1,
			after: endings(1),
			held:  runeKey('k'),
			still: true,
			waits: []waitSpec{{kind: "text", needle: "send now dropped"}, {kind: "idle"}},
		},
		{
			// clearPending's one call: nothing is armed, so its Disarm is
			// refused and publishes nothing, and its ClearQueue takes the row;
			// the cancel, handed back when the call has answered, ends the turn.
			name:       "Ctrl+C's chain",
			prepare:    func(r *schedRun) func() { queuedBehind(r, "ROW"); return nil },
			keys:       theKeys(ctrlC),
			links:      1,
			cancel:     true,
			cancelEnds: true,
			held:       runeKey('k'),
			still:      true,
			waits:      []waitSpec{{kind: "gone", needle: "#1 ROW"}, {kind: "idle"}},
		},
		{
			// Both halves of the call publish: the Disarm its withdrawal
			// (skipped as this model's echo), the ClearQueue the row. The arm's
			// own cancel ends the turn once the call has run, so Ctrl+C's finds
			// it over.
			name: "Ctrl+C's chain over an armed send-now",
			prepare: func(r *schedRun) func() {
				queuedBehind(r, "ROW")
				return armedOver(r)
			},
			keys:   theKeys(ctrlC),
			links:  1,
			after:  endings(1),
			cancel: true,
			held:   runeKey('k'),
			still:  true,
			waits:  []waitSpec{{kind: "gone", needle: "#1 ROW"}, {kind: "idle"}},
		},
		{
			name:    "/clear",
			prepare: func(r *schedRun) func() { queuedBehind(r, "ROW"); return nil },
			keys:    runesThen("/clear", enter()),
			links:   1,
			held:    runeKey('k'),
			waits:   []waitSpec{{kind: "gone", needle: "#1 ROW"}, {kind: "gone", needle: "❯ go"}},
		},
		{
			name:    "an edit saved",
			prepare: func(r *schedRun) func() { queuedBehind(r, "ROW"); return nil },
			keys:    theKeys(up, enter(), runeKey('!'), enter()),
			links:   1,
			held:    runeKey('k'),
			still:   true,
			waits:   []waitSpec{{kind: "text", needle: "#1 ROW!"}, {kind: "gone", needle: "editing #1"}},
		},
		{
			name:    "an emptied edit",
			prepare: func(r *schedRun) func() { queuedBehind(r, "ROW"); return nil },
			keys:    theKeys(up, enter(), bs, bs, bs, enter()),
			links:   1,
			held:    runeKey('k'),
			still:   true,
			waits:   []waitSpec{{kind: "gone", needle: "#1 ROW"}},
		},
		{
			name:    "Backspace on a row",
			prepare: func(r *schedRun) func() { queuedBehind(r, "ROW", "NEXT"); return nil },
			keys:    theKeys(up, bs),
			links:   1,
			held:    runeKey('k'),
			still:   true,
			waits:   []waitSpec{{kind: "gone", needle: "#2 NEXT"}},
		},
		{
			name:    "a click on a row's cancel",
			prepare: func(r *schedRun) func() { queuedBehind(r, "ROW"); return nil },
			keys: func(r *schedRun) []tea.Msg {
				// The strip is drawn on the keyboard's row, right-aligned, and
				// [cancel] is its last button.
				band := r.m().lay.Region(regionQueue)
				x := r.m().width - 2
				if band.Empty() || queueActionAt(x, r.m().width) != actionCancel {
					r.t.Fatalf("prepare: no [cancel] under the pointer (band %+v, x %d)", band, x)
				}
				return []tea.Msg{up, tea.MouseMsg{X: x, Y: band.Top, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}}
			},
			links: 1,
			held:  runeKey('k'),
			still: true,
			waits: []waitSpec{{kind: "gone", needle: "#1 ROW"}},
		},
		{
			// Two rows stranded in an idle session's band, the last edited and
			// then sent now: the save is the first link, and its continuation
			// issues the Submit, the second. The sent row's turn is held until
			// both calls have run; then it ends and the row left behind drains.
			name: "Ctrl+L's save then send",
			prepare: func(r *schedRun) func() {
				strandedRows(r, "ROW", "NEXT")
				return r.sess.Script(scriptHeld()).Release
			},
			keys:  theKeys(up, enter(), runeKey('!'), ctrlL),
			links: 2,
			after: endings(2),
			held:  runeKey('k'),
			still: true,
			waits: []waitSpec{{kind: "working"}, {kind: "text", needle: "echo: ROW"}, {kind: "idle"}},
		},
	}
}

// chainFrameSequences is TestTheGatedFrameSequenceIsTodays at C18b's sites
// (X34): for every order in which each link's reply and events, what follows,
// the issuing key's sync token and a key typed after it can arrive, the frames
// a wait can see after the token's barrier — the same frames, carrying the
// same sync values — are the gateSync baseline's for the order today's code
// would have seen; a chained wait over them matches the same frames in both
// modes; no frame published while a gate was open carries the token; the
// issuing Update's frame shows only its pre-call work; and no cancel is handed
// back before the chain's end.
func chainFrameSequences(t *testing.T) { chainFrameSequencesOf(t, chainSites()) }

// chainFrameSequencesOf is chainFrameSequences over sites: C18b's, and C18c's
// key-issued ones (answerFrameSequences).
func chainFrameSequencesOf(t *testing.T, sites []chainSite) {
	type baseline struct {
		frames []frameState
		n      int
	}
	for _, site := range sites {
		t.Run(site.name, func(t *testing.T) {
			orders := chainOrders(site)
			if len(orders) == 0 {
				t.Fatal("no arrival orders")
			}
			// Many async orders are one baseline order — the replies gone, the
			// token placed after the key — and the baseline is deterministic, so
			// each is run once.
			baselines := map[string]baseline{}
			for _, order := range orders {
				t.Run(strings.Join(order, ","), func(t *testing.T) {
					af, an, counts := chainSchedule(t, site, false, order, nil)
					base := chainBaseline(order, site.links)
					key := fmt.Sprint(base, counts)
					b, ok := baselines[key]
					if !ok {
						b.frames, b.n, _ = chainSchedule(t, site, true, base, counts)
						baselines[key] = b
					}
					bf, bn := b.frames, b.n
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
						// A runner that keeps up (the frame harness's rendezvous, X36),
						// whose token the chain's end acknowledged — parked, with nothing
						// held before it, the one order the runner's one-message delivery
						// makes (C17c) — finds every wait. One that fell behind, or whose
						// token arrived behind held messages or after everything, can find
						// the latest frame alone — in both modes alike, above.
						early := parkedToken(order, site.links)
						if fast && early && slices.Contains(aw, "") {
							t.Fatalf("a chained wait matched nothing: %q", aw)
						}
					}
					for _, f := range af {
						if f.gated && f.sync >= an {
							t.Fatalf("a gated frame carries the token (sync %d)", f.sync)
						}
					}
				})
			}
		})
	}
}

// ------------------------------------------------------- the chain run whole

// TestTheCtrlCChainRunsWhole (astra 3; X34): Ctrl+C while working is a chain —
// the Disarm and the ClearQueue, back to back in clearPending's one gated call,
// then the cancel — and it runs whole, in order, in the gateSync baseline and
// asynchronously alike. Asynchronously the call is open while a key, the key's
// token and the call's own events arrive, all held: the confirm comes down in
// the key's own Update, and nothing after the call has run; the call's one
// continuation takes the Disarm's marker, ends the edit, settles the band,
// hands back the cancel — never before — and arms the double-press window from
// the moment of the key, though the clock has moved since. The frames a wait
// can see after the token, and the final state, are the baseline's.
func TestTheCtrlCChainRunsWhole(t *testing.T) {
	ctrlC := tea.KeyMsg{Type: tea.KeyCtrlC}
	for _, tc := range []struct {
		name  string
		armed bool
	}{
		// Nothing armed — the Disarm is refused — and a row being edited, with
		// the confirm's question never raised.
		{"a row being edited behind a working turn", false},
		// A send-now armed, its cancel held at the session, and a row queued.
		{"an armed send-now and a queued row", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var finals [2]Model
			var views [2][]frameSeqState
			var prompts [2][]string
			for i, mode := range frameGateModes {
				t.Run(mode.name, func(t *testing.T) {
					r := newSchedRun(t, mode.sync)
					r.noDrain = true
					var release func()
					queuedBehind(r, "ROW", "NEXT")
					if tc.armed {
						release = armedOver(r)
						t.Cleanup(release)
					} else {
						r.key(tea.KeyMsg{Type: tea.KeyUp})
						r.key(enter())
						if r.m().queueEdit == "" || r.m().input.Value() != "NEXT" {
							t.Fatalf("prepare: not editing NEXT (edit %q, composer %q)", r.m().queueEdit, r.m().input.Value())
						}
					}
					t0 := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
					now := t0
					r.f.inner.clock = func() time.Time { return now }
					prev := r.m()
					r.step(ctrlC)
					r.n++
					tok := r.n
					if mode.sync {
						m := r.m()
						if m.gate != nil || len(r.calls) != 0 || len(r.cancels) != 1 {
							t.Fatalf("the baseline's key: gate %v, %d calls, %d cancels; want the whole chain run and its cancel", m.gate, len(r.calls), len(r.cancels))
						}
						if !m.ctrlCDeadline.Equal(t0.Add(ctrlCWindow)) {
							t.Fatalf("the window closes at %s, want %s", m.ctrlCDeadline, t0.Add(ctrlCWindow))
						}
						now = t0.Add(5 * time.Second)
						r.step(frameSyncMsg{n: tok})
						r.step(runeKey('k'))
						r.feed()
					} else {
						// The key's Update: clearPending's one call is open, and
						// nothing after it has run.
						m := r.m()
						switch {
						case m.gate == nil || len(r.calls) != 1:
							t.Fatalf("the key: gate %v, %d calls; want the one call open", m.gate, len(r.calls))
						case len(r.cancels) != 0:
							t.Fatal("the cancel was handed back with the call: it is the chain's last link")
						case m.confirm != nil:
							t.Fatal("the confirm is up: it comes down in the key's own Update")
						case m.queueEdit != prev.queueEdit:
							t.Fatal("the edit ended before the call answered: it is the continuation's")
						case m.disarmed != prev.disarmed || m.armedDraft != prev.armedDraft:
							t.Fatal("the disarm's markers moved before its reply")
						case len(m.queue) != 2 || !m.ctrlCDeadline.IsZero():
							t.Fatalf("the band (%d rows) or the window (%s) moved before the call answered", len(m.queue), m.ctrlCDeadline)
						}
						// The key's token comes with it — one message, nothing between
						// them (C17c) — and is parked under the key's own gate. The
						// clock moves; another key arrives, and is held.
						r.step(frameSyncMsg{n: tok})
						now = t0.Add(5 * time.Second)
						r.step(runeKey('k'))

						rep := runWatched(t, r.calls[0])
						r.calls = nil
						evs := r.pending()
						if want := map[bool]int{false: 2, true: 3}[tc.armed]; len(evs) != want {
							t.Fatalf("the call published %d events, want %d (a withdrawal when armed, a removal per row)", len(evs), want)
						}
						// The call's own events arrive before its reply, and are held.
						r.stepEvents(evs)
						if held := len(r.m().held); held != 1+len(evs) {
							t.Fatalf("%d messages held, want the key and the call's events", held)
						}
						r.step(rep)
						m = r.m()
						switch {
						case m.gate != nil || len(r.calls) != 0:
							t.Fatalf("after the call's reply: gate %v, %d calls; want the chain's end", m.gate, len(r.calls))
						case len(r.cancels) != 1:
							t.Fatalf("the chain's end handed back %d cancels, want one", len(r.cancels))
						case m.queueEdit != "":
							t.Fatal("the continuation did not end the edit")
						case tc.armed && (m.disarmed == prev.disarmed || m.armedDraft != ""):
							t.Fatalf("the withdrawal's markers were not taken: disarmed %q, armed draft %q", m.disarmed, m.armedDraft)
						case !tc.armed && m.disarmed != prev.disarmed:
							t.Fatal("a refused Disarm set the marker")
						case len(m.queue) != 0 || m.queueFocus:
							t.Fatalf("the band after the clear: %+v, focus %v", m.queue, m.queueFocus)
						case !m.ctrlCDeadline.Equal(t0.Add(ctrlCWindow)):
							t.Fatalf("the window closes at %s, want %s: the moment of the key, not of the reply", m.ctrlCDeadline, t0.Add(ctrlCWindow))
						case m.syncAck != tok:
							t.Fatalf("the chain's end acknowledged %d, want the key's token %d", m.syncAck, tok)
						case len(m.held) != 1+len(evs):
							t.Fatalf("the release applied held messages: %d held", len(m.held))
						}
						r.drainOwed()
					}
					// The cancel, as the runtime runs it. With an arm, the arm's own
					// cancel ends the turn first, and Ctrl+C's finds it over.
					if tc.armed {
						release()
						r.feedUntil(endings(1))
					}
					c := r.cancels[0]
					r.cancels = nil
					if msg := runWatched(t, c); msg != nil {
						t.Fatalf("the cancel answered %#v", msg)
					}
					if !tc.armed {
						r.feedUntil(endings(1))
					}
					r.feed()
					m := r.m()
					if m.status != statusIdle || len(m.queue) != 0 || m.sendNowPending() {
						t.Fatalf("the end: status %s, band %+v, armed %v", m.status, m.queue, m.sendNowPending())
					}
					if slices.Contains(r.stub.Prompts(), "ROW") || slices.Contains(r.stub.Prompts(), "NOW") {
						t.Fatalf("a cleared row or a withdrawn send went: prompts %q", r.stub.Prompts())
					}
					finals[i], views[i], prompts[i] = m, matchable(r.frames, tok), r.stub.Prompts()
				})
			}
			if t.Failed() {
				return
			}
			if !reflect.DeepEqual(views[0], views[1]) {
				t.Fatalf("the modes' matchable frames differ\n%s", frameSeqDiff(views[0], views[1]))
			}
			if !slices.Equal(prompts[0], prompts[1]) {
				t.Fatalf("the session was prompted %q in the baseline, %q asynchronously", prompts[0], prompts[1])
			}
			// Every field the invisibility watch digests, but those two runs
			// cannot share — their workspaces, their sessions' start, and the
			// transcripts' entry times the engines' clocks stamp — whose text is
			// compared instead.
			perRun := []string{"cwd", "sessStart", "layouts", "main", "shared"}
			var changed []string
			for _, f := range digestModel(&finals[0]).diff(digestModel(&finals[1])) {
				if !slices.Contains(perRun, f) {
					changed = append(changed, f)
				}
			}
			if len(changed) != 0 {
				t.Fatalf("the final states differ in %v", changed)
			}
			for _, kind := range []entryKind{entryUser, entryAssistant, entryNote, entryError} {
				if b, a := texts(finals[0], kind), texts(finals[1], kind); !slices.Equal(b, a) {
					t.Fatalf("the transcripts differ: %q in the baseline, %q asynchronously", b, a)
				}
			}
		})
	}
}

// ------------------------------------------------------ the verbs unanswered

// stallBackend is the in-process backend with one verb that does not answer
// before the test ends: it runs the engine's own first when ran says so — the
// answer lost — and not at all otherwise. Every other call is the engine's.
// The queue verbs are C18b's; Answer, Interject and Ask are C18c's
// (gate_answers_test.go).
type stallBackend struct {
	backend.Backend
	verb  string
	ran   bool
	never <-chan struct{}
	// settled closes once the stalled verb has done all it will — run the
	// engine's call, or not — so a test applies the unanswered reply to a
	// session that is no longer moving, however long the call took.
	settled chan struct{}
}

// stall puts the model's backend behind a stallBackend for verb, until the
// test ends, and hands it back: its settled for answerStalled, its Backend to
// put back.
func (r *schedRun) stall(verb string, ran bool) *stallBackend {
	never := make(chan struct{})
	r.t.Cleanup(func() { close(never) })
	sb := &stallBackend{Backend: r.m().eng, verb: verb, ran: ran, never: never, settled: make(chan struct{})}
	r.f.inner.eng = sb
	return sb
}

func (b *stallBackend) stall(verb string, call func()) bool {
	if b.verb != verb {
		return false
	}
	if b.ran {
		call()
	}
	close(b.settled)
	<-b.never
	return true
}

func (b *stallBackend) Disarm(ctx context.Context, c engine.Command) error {
	if b.stall("Disarm", func() { _ = b.Backend.Disarm(ctx, c) }) {
		return nil
	}
	return b.Backend.Disarm(ctx, c)
}

func (b *stallBackend) ClearQueue(ctx context.Context, c engine.Command) ([]agent.QueuedPrompt, error) {
	if b.stall("ClearQueue", func() { _, _ = b.Backend.ClearQueue(ctx, c) }) {
		return nil, nil
	}
	return b.Backend.ClearQueue(ctx, c)
}

func (b *stallBackend) Unqueue(ctx context.Context, c engine.Command, id string) (agent.QueuedPrompt, error) {
	if b.stall("Unqueue", func() { _, _ = b.Backend.Unqueue(ctx, c, id) }) {
		return agent.QueuedPrompt{}, nil
	}
	return b.Backend.Unqueue(ctx, c, id)
}

func (b *stallBackend) EditQueued(ctx context.Context, c engine.Command, id, text string, v *int) error {
	if b.stall("EditQueued", func() { _ = b.Backend.EditQueued(ctx, c, id, text, v) }) {
		return nil
	}
	return b.Backend.EditQueued(ctx, c, id, text, v)
}

func (b *stallBackend) Answer(ctx context.Context, c engine.Command, id string, a agent.AskAnswer) error {
	if b.stall("Answer", func() { _ = b.Backend.Answer(ctx, c, id, a) }) {
		return nil
	}
	return b.Backend.Answer(ctx, c, id, a)
}

func (b *stallBackend) Interject(ctx context.Context, c engine.Command, text string) error {
	if b.stall("Interject", func() { _ = b.Backend.Interject(ctx, c, text) }) {
		return nil
	}
	return b.Backend.Interject(ctx, c, text)
}

func (b *stallBackend) Ask(ctx context.Context, id string) (agent.AskRecord, bool, error) {
	if b.stall("Ask", func() { _, _, _ = b.Backend.Ask(ctx, id) }) {
		return agent.AskRecord{}, false, nil
	}
	return b.Backend.Ask(ctx, id)
}

// answerStalled runs every gated call waiting, one at a time, and applies its
// reply — a chain's links in turn. An unanswered reply is applied once the
// stalled verb has settled.
func (r *schedRun) answerStalled(b *stallBackend) {
	r.t.Helper()
	for len(r.calls) > 0 {
		c := r.calls[0]
		r.calls = r.calls[1:]
		rep := runWatched(r.t, c)
		if g, ok := rep.(gateReply); ok && g.err == ErrNoAnswer && b != nil {
			select {
			case <-b.settled:
			case <-time.After(pumpWatchdog):
				r.t.Fatal("the stalled verb never settled")
			}
		}
		r.step(rep)
	}
}

// TestAQueueVerbThatNeverAnswersSaysSo (§3.12 "On expiry"; the brief's pinned
// wordings): each queue verb that does not answer releases at its deadline
// with ErrNoAnswer, and its continuation says the outcome is unknown, in the
// verb's own words, and claims nothing — the band is the session's, read now
// — and the chain goes on where the user asked for everything to stop. Both
// cases each time: the command never ran, and it ran with its answer lost.
func TestAQueueVerbThatNeverAnswersSaysSo(t *testing.T) {
	prev := gateDeadline
	gateDeadline = 20 * time.Millisecond
	t.Cleanup(func() { gateDeadline = prev })
	ctrlC := tea.KeyMsg{Type: tea.KeyCtrlC}
	ctrlL := tea.KeyMsg{Type: tea.KeyCtrlL}
	up := tea.KeyMsg{Type: tea.KeyUp}
	bs := tea.KeyMsg{Type: tea.KeyBackspace}

	// bandIsTheSessions: at the release the band is the fold's — what this
	// client has folded, never a read of the engine's live queue (plan 027
	// §3.12: from C21 the band shows the fold's facts) — and once the
	// command's events, held behind the gate, are folded, it is the session's.
	bandIsTheSessions := func(t *testing.T, r *schedRun) {
		t.Helper()
		if fq := r.m().shared.State().Queue; !reflect.DeepEqual(noneIsNil(r.m().queue), fq) {
			t.Fatalf("at the release the band holds %+v, the fold %+v", r.m().queue, fq)
		}
		r.feed()
		if eq := r.eng.State().Queue; !reflect.DeepEqual(r.m().queue, eq) {
			t.Fatalf("the band holds %+v, the session %+v", r.m().queue, eq)
		}
	}
	noted := func(t *testing.T, r *schedRun, want string) {
		t.Helper()
		if got := r.m().copyNote; got != want {
			t.Fatalf("the note is %q, want %q", got, want)
		}
	}
	for _, ran := range []bool{false, true} {
		t.Run(fmt.Sprintf("ran %v", ran), func(t *testing.T) {
			t.Run("Esc's Disarm", func(t *testing.T) {
				r := newSchedRun(t, false)
				aWorkingTurn(r)
				t.Cleanup(armedOver(r))
				before := r.m()
				sb := r.stall("Disarm", ran)
				r.step(tea.KeyMsg{Type: tea.KeyEsc})
				r.answerStalled(sb)
				m := r.m()
				noted(t, r, noAnswerDisarmNote)
				if m.disarmed != before.disarmed || m.armedDraft != before.armedDraft {
					t.Fatalf("an unanswered Disarm moved a marker: disarmed %q→%q, armed draft %q→%q",
						before.disarmed, m.disarmed, before.armedDraft, m.armedDraft)
				}
				if m.input.Value() != "NOW" {
					t.Fatalf("the draft is %q", m.input.Value())
				}
				if armed := r.eng.State().SendNow != nil; armed == ran {
					t.Fatalf("the session's arm: %v (ran %v)", armed, ran)
				}
			})
			// clearPending's one call, stuck in its Disarm: it never reaches
			// its ClearQueue, and the call as a whole does not answer, so
			// neither outcome is known — both notes are written, in the chain's
			// order, and the status line keeps the last — and the chain goes on.
			t.Run("Ctrl+C's call, stalled in its Disarm", func(t *testing.T) {
				r := newSchedRun(t, false)
				queuedBehind(r, "ROW")
				t.Cleanup(armedOver(r))
				before := r.m()
				sb := r.stall("Disarm", ran)
				r.step(ctrlC)
				r.answerStalled(sb)
				m := r.m()
				noted(t, r, noAnswerClearNote)
				if m.disarmed != before.disarmed || m.armedDraft != before.armedDraft {
					t.Fatal("an unanswered Disarm moved a marker")
				}
				bandIsTheSessions(t, r)
				m = r.m()
				if len(m.queue) != 1 || m.queueFocus {
					t.Fatalf("the band %+v (focus %v): the ClearQueue never ran", m.queue, m.queueFocus)
				}
				if armed := r.eng.State().SendNow != nil; armed == ran {
					t.Fatalf("the session's arm: %v (ran %v)", armed, ran)
				}
				// The chain went on: the cancel issued, the window armed.
				if len(r.cancels) != 1 || m.ctrlCDeadline.IsZero() {
					t.Fatalf("the chain's end: %d cancels, window %s", len(r.cancels), m.ctrlCDeadline)
				}
				r.cancels = nil
			})
			for _, from := range []string{"Ctrl+C", "/clear"} {
				t.Run(from+"'s call, stalled in its ClearQueue", func(t *testing.T) {
					r := newSchedRun(t, false)
					queuedBehind(r, "ROW")
					sb := r.stall("ClearQueue", ran)
					if from == "Ctrl+C" {
						r.step(ctrlC)
					} else {
						for _, k := range "/clear" {
							r.key(runeKey(k))
						}
						r.step(enter())
					}
					r.answerStalled(sb)
					noted(t, r, noAnswerClearNote)
					bandIsTheSessions(t, r)
					m := r.m()
					if got := len(m.queue); got != map[bool]int{false: 1, true: 0}[ran] {
						t.Fatalf("the band holds %d rows (ran %v)", got, ran)
					}
					if m.queueFocus {
						t.Fatal("the band kept the keyboard")
					}
					if from == "Ctrl+C" {
						if len(r.cancels) != 1 || m.ctrlCDeadline.IsZero() {
							t.Fatalf("the chain stopped at the call: %d cancels, window %s", len(r.cancels), m.ctrlCDeadline)
						}
						r.cancels = nil
					} else if m.input.Value() != "" || len(texts(m, entryUser)) != 0 {
						t.Fatalf("/clear's own work did not run: composer %q, user rows %q", m.input.Value(), texts(m, entryUser))
					}
				})
			}
			for _, how := range []string{"Backspace", "a click on [cancel]"} {
				t.Run(how+"'s Unqueue", func(t *testing.T) {
					r := newSchedRun(t, false)
					queuedBehind(r, "ROW", "NEXT")
					r.key(up)
					sb := r.stall("Unqueue", ran)
					if how == "Backspace" {
						r.step(bs)
					} else {
						band := r.m().lay.Region(regionQueue)
						r.step(tea.MouseMsg{X: r.m().width - 2, Y: band.Top + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
					}
					if len(r.calls) != 1 {
						t.Fatalf("%s issued %d gated calls", how, len(r.calls))
					}
					r.answerStalled(sb)
					noted(t, r, noAnswerRowNote)
					bandIsTheSessions(t, r)
					if got := len(r.m().queue); got != map[bool]int{false: 2, true: 1}[ran] {
						t.Fatalf("the band holds %d rows (ran %v)", got, ran)
					}
				})
			}
			t.Run("an edit's EditQueued", func(t *testing.T) {
				r := newSchedRun(t, false)
				queuedBehind(r, "ROW")
				r.key(up)
				r.key(enter())
				id := r.m().queueEdit
				r.key(runeKey('!'))
				sb := r.stall("EditQueued", ran)
				r.step(enter())
				r.answerStalled(sb)
				m := r.m()
				noted(t, r, noAnswerRowNote)
				if m.queueEdit != id || m.input.Value() != "ROW!" {
					t.Fatalf("an unanswered save left edit mode: editing %q (want %q), composer %q", m.queueEdit, id, m.input.Value())
				}
				bandIsTheSessions(t, r)
				m = r.m()
				if got := m.queue[0].Text; got != map[bool]string{false: "ROW", true: "ROW!"}[ran] {
					t.Fatalf("the band's row reads %q (ran %v)", got, ran)
				}
			})
			t.Run("Ctrl+L's EditQueued", func(t *testing.T) {
				r := newSchedRun(t, false)
				strandedRows(r, "ROW")
				r.key(up)
				r.key(enter())
				id := r.m().queueEdit
				r.key(runeKey('!'))
				sb := r.stall("EditQueued", ran)
				r.step(ctrlL)
				r.answerStalled(sb)
				m := r.m()
				noted(t, r, noAnswerRowNote)
				if m.queueEdit != id || m.input.Value() != "ROW!" {
					t.Fatalf("an unanswered save left edit mode: editing %q (want %q), composer %q", m.queueEdit, id, m.input.Value())
				}
				r.feed()
				if len(r.stub.Prompts()) != 0 || r.m().status != statusIdle || len(r.m().queue) != 1 {
					t.Fatalf("the row was sent after an unanswered save: prompts %q, status %s, band %+v", r.stub.Prompts(), r.m().status, r.m().queue)
				}
			})
			t.Run("an emptied edit's Unqueue", func(t *testing.T) {
				r := newSchedRun(t, false)
				queuedBehind(r, "ROW")
				r.key(up)
				r.key(enter())
				id := r.m().queueEdit
				for range "ROW" {
					r.key(bs)
				}
				sb := r.stall("Unqueue", ran)
				r.step(enter())
				r.answerStalled(sb)
				bandIsTheSessions(t, r)
				m := r.m()
				if !ran {
					// The row is still there, and so is the edit.
					noted(t, r, noAnswerRowNote)
					if m.queueEdit != id || len(m.queue) != 1 {
						t.Fatalf("an unanswered save left edit mode: editing %q (want %q), band %+v", m.queueEdit, id, m.queue)
					}
					return
				}
				// The row went: the edit stayed open, and the band that no
				// longer holds its row ends it (syncQueue), saying so.
				noted(t, r, "the message you were editing is gone")
				if m.queueEdit != "" || len(m.queue) != 0 {
					t.Fatalf("editing %q over band %+v", m.queueEdit, m.queue)
				}
			})
		})
	}
}

// halfBackend is the in-process backend with one of clearPending's two verbs
// coming back unanswered at once — the outcome its own deadline would have
// given it inside an answered call (unanswered) — having done nothing.
type halfBackend struct {
	backend.Backend
	verb string
}

func (b *halfBackend) Disarm(ctx context.Context, c engine.Command) error {
	if b.verb == "Disarm" {
		return ErrNoAnswer
	}
	return b.Backend.Disarm(ctx, c)
}

func (b *halfBackend) ClearQueue(ctx context.Context, c engine.Command) ([]agent.QueuedPrompt, error) {
	if b.verb == "ClearQueue" {
		return nil, ErrNoAnswer
	}
	return b.Backend.ClearQueue(ctx, c)
}

// TestClearPendingSaysWhichHalfDidNotAnswer: clearPending's one call answers
// each verb's outcome, and its continuation applies each as its own link did —
// a Disarm that did not answer says the send may still be armed and moves no
// marker while the ClearQueue's clear still shows; a ClearQueue that did not
// answer says the queue may not have been cleared, over the session's band.
func TestClearPendingSaysWhichHalfDidNotAnswer(t *testing.T) {
	ctrlC := tea.KeyMsg{Type: tea.KeyCtrlC}
	t.Run("the Disarm", func(t *testing.T) {
		r := newSchedRun(t, false)
		queuedBehind(r, "ROW")
		t.Cleanup(armedOver(r))
		before := r.m()
		r.f.inner.eng = &halfBackend{Backend: r.m().eng, verb: "Disarm"}
		r.step(ctrlC)
		r.answerStalled(nil)
		m := r.m()
		if m.copyNote != noAnswerDisarmNote {
			t.Fatalf("the note is %q, want %q", m.copyNote, noAnswerDisarmNote)
		}
		if m.disarmed != before.disarmed || m.armedDraft != before.armedDraft {
			t.Fatal("an unanswered Disarm moved a marker")
		}
		if len(m.queue) != 0 || len(r.eng.State().Queue) != 0 || len(r.cancels) != 1 {
			t.Fatalf("the ClearQueue's half did not show: band %+v, %d cancels", m.queue, len(r.cancels))
		}
		r.cancels = nil
	})
	t.Run("the ClearQueue", func(t *testing.T) {
		r := newSchedRun(t, false)
		queuedBehind(r, "ROW")
		r.f.inner.eng = &halfBackend{Backend: r.m().eng, verb: "ClearQueue"}
		r.step(ctrlC)
		r.answerStalled(nil)
		m := r.m()
		if m.copyNote != noAnswerClearNote {
			t.Fatalf("the note is %q, want %q", m.copyNote, noAnswerClearNote)
		}
		if eq := r.eng.State().Queue; len(eq) != 1 || !reflect.DeepEqual(m.queue, eq) {
			t.Fatalf("the band holds %+v, the session %+v", m.queue, eq)
		}
		if len(r.cancels) != 1 || m.ctrlCDeadline.IsZero() {
			t.Fatalf("the chain stopped: %d cancels, window %s", len(r.cancels), m.ctrlCDeadline)
		}
		r.cancels = nil
	})
}

// ctxErrBackend answers clearPending's two verbs with a deadline's error.
type ctxErrBackend struct{ backend.Backend }

func (ctxErrBackend) Disarm(context.Context, engine.Command) error {
	return fmt.Errorf("gave up: %w", context.DeadlineExceeded)
}

func (ctxErrBackend) ClearQueue(context.Context, engine.Command) ([]agent.QueuedPrompt, error) {
	return nil, fmt.Errorf("gave up: %w", context.DeadlineExceeded)
}

// TestClearPendingsCallJudgesEachVerbByItsDeadline: inside clearPending's one
// call, a verb that gave up because the call's deadline passed did not answer
// (ErrNoAnswer, callGated's rule, verb by verb), and one that answered a
// deadline error of its own while the call's was still running is that error.
func TestClearPendingsCallJudgesEachVerbByItsDeadline(t *testing.T) {
	m, _ := gatedModel(t)
	b := ctxErrBackend{Backend: m.eng}
	expired, cancel := context.WithTimeout(context.Background(), -time.Second)
	defer cancel()
	res, err := clearPendingCall(engine.Command{}, engine.Command{})(expired, b)
	ans, ok := res.(clearAnswer)
	if err != nil || !ok || ans.disarm != ErrNoAnswer || ans.clear != ErrNoAnswer {
		t.Fatalf("past its deadline the call answered %+v, %v; want both verbs unanswered", res, err)
	}
	res, _ = clearPendingCall(engine.Command{}, engine.Command{})(context.Background(), b)
	ans = res.(clearAnswer)
	if errors.Is(ans.disarm, ErrNoAnswer) || !errors.Is(ans.disarm, context.DeadlineExceeded) || errors.Is(ans.clear, ErrNoAnswer) {
		t.Fatalf("within its deadline the call answered %+v; want each verb's own error", ans)
	}
}

// TestACtrlCOverASettlingTurnRunsNoQueuedRow (the finding-2 decision, X40):
// Ctrl+C over an armed send-now with a row queued, where the turn the arm's own
// cancel is ending settles the instant clearPending's gated call returns — on
// the command's goroutine, before its reply reaches the model, which is the
// window two gates would have put between the Disarm and the ClearQueue. The
// settlement claims no successor: the engine's own record says so (the old
// turn's ending names no Next, no started follows it, and the engine is idle
// with nothing queued and nothing armed), and neither the row nor the
// withdrawn send reaches the session.
//
// What it does not prove: a settlement BETWEEN the two engine calls inside
// the one gated call. The engine lets go of its lock after the Disarm and
// takes it again for the ClearQueue, and a settlement there can still drain
// the head — the original microsecond window, X40 2's accepted residual. The
// settlement here is pinned until the whole call has returned.
//
// A successor the engine did claim is held before its prompt reaches the
// session (HoldNextBegin), so the check cannot pass on the Stub's prompts
// alone while a claimed successor has yet to record its own. The arm's cancel
// is held past the engine's own two-second deadline for it
// (armedOverPastItsDeadline): a slow run must not let the old turn settle
// before the test releases it (sol r46 4).
//
// Only the asynchronous subtest stands in the window between the call's
// return and its reply: the gateSync baseline runs the call AND applies its
// reply inside the key's Update, and the turn settles only after that (sol
// r46 3). The baseline is the same property over today's control flow, not a
// second run of the window.
func TestACtrlCOverASettlingTurnRunsNoQueuedRow(t *testing.T) {
	ctrlC := tea.KeyMsg{Type: tea.KeyCtrlC}
	for _, mode := range frameGateModes {
		t.Run(mode.name, func(t *testing.T) {
			r := newSchedRun(t, mode.sync)
			queuedBehind(r, "ROW")
			release := armedOverPastItsDeadline(r)
			t.Cleanup(release)
			_, releaseBegin := r.sess.HoldNextBegin()
			t.Cleanup(releaseBegin)
			var after []agent.Event
			settled := false
			// settle lets the arm's cancel end the turn, and waits for the
			// engine to settle it — into whatever the engine holds then. The
			// settlement enqueues the ending, and any successor's removal and
			// started, as one batch: what the read after the ending brings back
			// is all of it.
			settle := func() {
				release()
				after = r.until(endings(1))
				r.stepEvents(after)
				settled = true
			}
			r.step(ctrlC)
			// The gated calls, each answered, and the turn settling the instant
			// the first returns — before its reply reaches the model.
			for len(r.calls) > 0 {
				c := r.calls[0]
				r.calls = r.calls[1:]
				rep := runWatched(t, c)
				if !settled {
					settle()
				}
				r.step(rep)
			}
			if !settled {
				// The baseline ran every call inside the key's Update.
				settle()
			}
			for _, c := range r.cancels {
				if msg := runWatched(t, c); msg != nil {
					t.Fatalf("the cancel answered %#v", msg)
				}
			}
			r.cancels = nil
			rest := r.pending()
			r.stepEvents(rest)

			// The engine's own record: the old turn's ending, and nothing
			// started after it.
			var ending *agent.TurnInfo
			var successors []string
			for _, ev := range append(after, rest...) {
				if ev.Type != agent.EventTurn || ev.Turn == nil {
					continue
				}
				switch ev.Turn.Phase {
				case agent.TurnEnded:
					if ending == nil {
						ending = ev.Turn
					}
				case agent.TurnStarted:
					if ending != nil {
						successors = append(successors, fmt.Sprintf("%s %s %q", ev.Turn.ID, ev.Turn.Origin, ev.Turn.Text))
					}
				}
			}
			if ending == nil {
				t.Fatal("the settlement published no ending")
			}
			st := r.eng.State()
			if ending.Next != "" || len(successors) != 0 || st.Turn != "" {
				t.Fatalf("the settlement claimed a successor: the ending names Next %q, started after it %q, the engine's turn %q (prompts %q)",
					ending.Next, successors, st.Turn, r.stub.Prompts())
			}
			if st.Activity != engine.ActivityIdle || len(st.Queue) != 0 || st.SendNow != nil || len(r.m().queue) != 0 {
				t.Fatalf("after Ctrl+C: activity %s, session queue %+v, armed %v, band %+v", st.Activity, st.Queue, st.SendNow != nil, r.m().queue)
			}
			if got := r.stub.Prompts(); slices.Contains(got, "ROW") || slices.Contains(got, "NOW") {
				t.Fatalf("a row the user cleared, or the send taken back, reached the session: prompts %q", got)
			}
		})
	}
}
