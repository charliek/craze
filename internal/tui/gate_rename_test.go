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
	"github.com/charliek/craze/internal/transcript"
)

// titleBackend is the in-process backend with SetTitle in a test's hands:
// every other call goes to the engine.
type titleBackend struct {
	backend.Backend
	setTitle func(ctx context.Context, c engine.Command, title string) error
}

func (b *titleBackend) SetTitle(ctx context.Context, c engine.Command, title string) error {
	return b.setTitle(ctx, c, title)
}

// TestTheGateIsInvisible (§3.12 (d)): the watch TestMain installs holds every
// gate still from its issuing Update to its reply. Here a /rename is issued
// asynchronously, a message of each kind is held while it waits, and the
// watch compares the state at every one of them and at the release — and
// finds nothing moved. The issuing Update's own work (the draft consumed) is
// allowed; the continuation's (the note, the title) is not there until the
// reply.
func TestTheGateIsInvisible(t *testing.T) {
	if gateHook == nil {
		t.Fatal("TestMain installs the invisibility watch; it is not installed")
	}
	m, stub := gatedModel(t)
	r := newGateRig(t, m)
	checks := gateWatch.checked()
	for _, k := range "/rename unseen" {
		r.send(runeKey(k))
	}
	r.send(enter())
	if r.m.gate == nil {
		t.Fatal("/rename opened no gate")
	}
	if r.m.input.Value() != "" {
		t.Fatalf("the issuing Update did not consume the draft: %q", r.m.input.Value())
	}
	if slices.Contains(texts(r.m, entryNote), "renamed to unseen") {
		t.Fatal("the rename's note was drawn before its reply")
	}
	issued := digestModel(&r.m)
	held := []tea.Msg{runeKey('q'), tea.WindowSizeMsg{Width: 70, Height: 20}, tickMsg{gen: r.m.tickGen}, frameSyncMsg{n: 1}}
	for _, msg := range held {
		r.send(msg)
	}
	stub.Emit(agent.Event{Type: agent.EventText, Text: "while renaming"})
	r.read()
	if changed := issued.diff(digestModel(&r.m)); len(changed) != 0 {
		t.Fatalf("the held messages changed %v", changed)
	}
	r.answer()
	if n := gateWatch.checked() - checks; n != len(held)+2 {
		t.Fatalf("the watch compared %d times, want once per held message and once at the release (%d)", n, len(held)+2)
	}
	if err := gateWatch.err(); err != nil {
		t.Fatalf("the watch saw a break: %v", err)
	}
	if !slices.Contains(texts(r.m, entryNote), "renamed to unseen") {
		t.Fatalf("the continuation did not run at the release: %q", texts(r.m, entryNote))
	}
}

// TestTheInvisibilityWatchSeesAHeldMutation proves the watch fires: a held
// message's path that writes through state every copy of the model shares —
// here the transcript pane, the pointer a real bug would write through — is
// seen at the next comparison, named by the field that moved.
func TestTheInvisibilityWatchSeesAHeldMutation(t *testing.T) {
	m, _ := gatedModel(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	tm, _ := m.gated(gateOpMsg{call: blockedCall(release, nil), cont: noteCont("released")}, withGateOps)
	m = tm.(Model)
	g := m.gate
	broke := gateWatch.expectBreak(g)
	t.Cleanup(func() { gateWatch.forget(g) })

	// A copy of the model draws a row: the pane is shared, so the gated model
	// now shows it too, without having applied anything.
	stray := m
	stray.addNote("a row nothing applied")
	tm, _ = m.gated(runeKey('k'), withGateOps)
	m = tm.(Model)
	if broke.phase != gateHeld || !slices.Contains(broke.fields, "main") {
		t.Fatalf("the watch saw %+v, want the held message's comparison to name the pane (main)", broke)
	}
}

// TestTheInvisibilityWatchSeesASharedEntryRewritten (astra C17 5): the digest
// holds the shared transcript's contents, not only its sizes. An earlier entry
// rewritten under an open gate to different text of the same length — the
// sequence, the state, every length, byte count and tail unchanged — is seen
// at the next held message, named as the shared model.
func TestTheInvisibilityWatchSeesASharedEntryRewritten(t *testing.T) {
	m, stub := gatedModel(t)
	r := newGateRig(t, m)
	stub.Emit(agent.Event{Type: agent.EventUser, Text: "the original line", Replayed: true})
	stub.Emit(agent.Event{Type: agent.EventText, Text: "an answer still streaming"})
	r.read()
	r.read()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	r.send(gateOpMsg{call: blockedCall(release, nil), cont: noteCont("released")})
	g := r.m.gate
	broke := gateWatch.expectBreak(g)
	t.Cleanup(func() { gateWatch.forget(g) })

	var user *transcript.Entry
	for _, e := range r.m.shared.Main.Entries() {
		if e.Kind == transcript.KindUser {
			user = e
		}
	}
	if user == nil {
		t.Fatal("no user entry was folded")
	}
	bytes, tail := r.m.shared.Main.Bytes(), r.m.shared.Main.Tail()
	user.Text = "the rewrote! line"
	if len(user.Text) != len("the original line") || r.m.shared.Main.Bytes() != bytes || r.m.shared.Main.Tail() != tail {
		t.Fatal("the rewrite moved a size or the tail: it proves nothing")
	}
	r.send(runeKey('k'))
	if broke.phase != gateHeld || !slices.Contains(broke.fields, "shared") {
		t.Fatalf("the watch saw %+v, want the held message's comparison to name the shared model", broke)
	}
}

// TestTheGateDigestSkipsOnlyNamedFields: every Model field the invisibility
// digest leaves out is named, with its reason, and names a field that exists;
// every other field is digested. A field added to Model is therefore held
// still under a gate unless someone decides otherwise here.
func TestTheGateDigestSkipsOnlyNamedFields(t *testing.T) {
	ty := reflect.TypeFor[Model]()
	fields := map[string]bool{}
	for i := range ty.NumField() {
		fields[ty.Field(i).Name] = true
	}
	for name, why := range gateDigestSkips {
		if !fields[name] {
			t.Errorf("gateDigestSkips names %q, which is not a Model field", name)
		}
		if why == "" {
			t.Errorf("gateDigestSkips gives no reason for %q", name)
		}
	}
	m := sized(t)
	d := digestModel(&m)
	for name := range fields {
		_, skipped := gateDigestSkips[name]
		if _, digested := d[name]; digested == skipped {
			t.Errorf("field %q: digested %v, skipped %v", name, digested, skipped)
		}
	}
}

// TestAGatedFrameMatchesNoWait: a frame published while a gated call waits for
// its reply satisfies no wait at all — not <start>, not idle, not a text it
// plainly shows — while the per-token barrier still reads its sync.
func TestAGatedFrameMatchesNoWait(t *testing.T) {
	s := frameState{plain: "craze renamed", status: statusIdle, started: true, card: true, copied: true, sync: 4, gated: true}
	for _, w := range []waitSpec{{kind: "idle"}, {kind: "working"}, {kind: "card"}, {kind: "copied"}, {kind: "text", needle: "renamed"}, {kind: "gone", needle: "absent"}} {
		if w.match(s) {
			t.Errorf("%+v matched a gated frame", w)
		}
		s.gated = false
		if w.kind != "idle" && w.kind != "working" && !w.match(s) {
			t.Errorf("%+v does not match the same frame ungated", w)
		}
		s.gated = true
	}
	if startedSpec(s) {
		t.Error("<start> matched a gated frame")
	}
}

// TestARenameThatNeverAnswersSaysTheTitleMayHaveChanged (§3.12 "On expiry"):
// a SetTitle that never answers releases at its deadline with ErrNoAnswer, and
// the continuation says the outcome is unknown and shows the title the session
// has. Both cases: the command never ran (the old title), and it ran but its
// answer was lost (the new one).
func TestARenameThatNeverAnswersSaysTheTitleMayHaveChanged(t *testing.T) {
	prev := gateDeadline
	gateDeadline = 20 * time.Millisecond
	t.Cleanup(func() { gateDeadline = prev })
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })

	for _, tc := range []struct {
		name   string
		ran    bool
		titled string
	}{
		{"the command never ran", false, ""},
		{"its answer was lost", true, "lost answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stub := gatedModel(t)
			inner := m.eng
			m.eng = &titleBackend{Backend: inner, setTitle: func(ctx context.Context, c engine.Command, title string) error {
				if tc.ran {
					if err := inner.SetTitle(ctx, c, title); err != nil {
						return err
					}
				}
				<-never
				return nil
			}}
			r := newGateRig(t, m)
			r.m.input.SetValue("/rename lost answer")
			r.send(enter())
			r.answer()
			notes := texts(r.m, entryNote)
			if len(notes) == 0 || notes[len(notes)-1] != noAnswerRenameNote {
				t.Fatalf("notes %q, want %q last", notes, noAnswerRenameNote)
			}
			if slices.Contains(notes, "renamed to lost answer") || len(texts(r.m, entryError)) != 0 {
				t.Fatalf("an unknown outcome drew the rename's note or an error: notes %q, errors %q", notes, texts(r.m, entryError))
			}
			// At the release the title is the fold's, never a read of the
			// session (plan 027 §3.12: from C21 the title shows the fold's
			// facts); once what the session published is folded, it is the
			// session's.
			if got, folded := r.m.snap.Title, r.m.shared.State().Settings.Title; got != folded {
				t.Fatalf("at the release the title shows %q, the fold %q", got, folded)
			}
			m = feed(t, r.m, stubDeltas(t, stub)...)
			if got := m.snap.Title; got != tc.titled || got != stub.Snapshot().Title {
				t.Fatalf("the title shows %q, the session has %q; want %q", got, stub.Snapshot().Title, tc.titled)
			}
		})
	}
}

// TestARenameEndsTheSameInBothModes: /rename's continuation is today's
// post-call code, so each of SetTitle's outcomes — renamed, refused, and
// renamed with the index write failed — leaves the same rows and title in the
// gateSync baseline and asynchronously.
func TestARenameEndsTheSameInBothModes(t *testing.T) {
	outcomes := map[string]error{
		"renamed":       nil,
		"refused":       errors.New("the log is backed up"),
		"index failure": fmt.Errorf("%w: %w", engine.ErrIndexWrite, errors.New("disk full")),
	}
	for name, fail := range outcomes {
		t.Run(name, func(t *testing.T) {
			run := func(sync bool) (notes, errs []string, title string) {
				m, _ := gatedModel(t)
				m.gateSync = sync
				inner := m.eng
				m.eng = &titleBackend{Backend: inner, setTitle: func(ctx context.Context, c engine.Command, title string) error {
					if fail != nil && !errors.Is(fail, engine.ErrIndexWrite) {
						return fail
					}
					if err := inner.SetTitle(ctx, c, title); err != nil {
						return err
					}
					return fail
				}}
				r := newGateRig(t, m)
				r.m.input.SetValue("/rename both ways")
				r.send(enter())
				if !sync {
					r.answer()
					r.drainAll()
				}
				return texts(r.m, entryNote), texts(r.m, entryError), r.m.snap.Title
			}
			bn, be, bt := run(true)
			an, ae, at := run(false)
			if !slices.Equal(bn, an) || !slices.Equal(be, ae) || bt != at {
				t.Fatalf("gateSync: notes %q errors %q title %q\nasync:    notes %q errors %q title %q", bn, be, bt, an, ae, at)
			}
		})
	}
}

// ---------------------------------------------------------- the frame order

// frameSeqState is a frame as a wait sees it: everything but the raw view.
type frameSeqState struct {
	plain                                     string
	status                                    status
	started, picking, replaying, card, copied bool
	sync                                      int
}

func frameSeq(s frameState) frameSeqState {
	return frameSeqState{s.plain, s.status, s.started, s.picking, s.replaying, s.card, s.copied, s.sync}
}

// renameSchedule is one /rename through a frameModel driven by direct
// Updates, the runner's way — every key followed by its sync token — until
// Enter; then the arrivals in order ("R" the reply, "S" Enter's token, "E1"
// the rename's own title delta, "E2" an event published after it), with every
// drain the model owes applied as soon as it is owed. It answers every frame
// published, and Enter's token.
func renameSchedule(t *testing.T, sync bool, order []string) ([]frameState, int) {
	t.Helper()
	m, stub := gatedModel(t)
	m.gateSync = sync
	fixed := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	m.clock = func() time.Time { return fixed }
	m.frozen = true
	eng := engineOf(t, m)
	f := frameModel{inner: m, bus: newFrameBus(nil)}
	var frames []frameState
	var calls []tea.Cmd
	drains := 0
	var sortCmd func(tea.Cmd)
	sortCmd = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch name := cmdFuncName(cmd); {
		case strings.HasPrefix(name, teaPkg+"compactCmds"), strings.HasPrefix(name, teaPkg+"Batch"):
			if b, ok := cmd().(tea.BatchMsg); ok {
				for _, c := range b {
					sortCmd(c)
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
		frames = append(frames, f.bus.last())
		sortCmd(cmd)
		for drains > 0 {
			drains--
			step(drainMsg{})
		}
	}
	// The stream as the primary holds it: flushed, then everything on it.
	pending := func() []agent.Event {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := eng.Sync(ctx); err != nil {
			t.Fatalf("sync: %v", err)
		}
		var out []agent.Event
		for {
			select {
			case ev := <-eng.Events():
				out = append(out, ev)
			default:
				return out
			}
		}
	}
	for _, ev := range pending() {
		step(eventMsg{ev})
	}
	n := 0
	for _, k := range "/rename fix it" {
		step(runeKey(k))
		n++
		step(frameSyncMsg{n: n})
	}
	step(enter())
	n++
	arrivals := map[string]tea.Msg{"S": frameSyncMsg{n: n}}
	if !sync {
		if len(calls) != 1 {
			t.Fatalf("Enter issued %d gated calls, want the rename's", len(calls))
		}
		arrivals["R"] = runWatched(t, calls[0])
	}
	title := pending()
	if len(title) != 1 || title[0].State == nil || title[0].State.Title == nil {
		t.Fatalf("the rename published %+v, want its one title delta", title)
	}
	arrivals["E1"] = eventMsg{title[0]}
	stub.Emit(agent.Event{Type: agent.EventText, Text: "after the rename"})
	after := pending()
	if len(after) != 1 {
		t.Fatalf("the emit published %+v", after)
	}
	arrivals["E2"] = eventMsg{after[0]}
	for _, a := range order {
		if msg, ok := arrivals[a]; ok {
			step(msg)
		}
	}
	return frames, n
}

// baselineOrder is what an async arrival order is for the gateSync baseline:
// the same order with no reply — the call returned inside Enter's Update. The
// token keeps its place (C17c): acknowledged after every message that arrived
// before it, in both modes — at the release when it arrived with nothing held
// (right behind its key), behind the held messages otherwise.
func baselineOrder(order []string) []string {
	return slices.DeleteFunc(slices.Clone(order), func(a string) bool { return a == "R" })
}

// matchable is the frames a wait after Enter's barrier can see: none published
// while a gate was open, and none before the first that acknowledged Enter's
// token, which the barrier consumes.
func matchable(frames []frameState, n int) []frameSeqState {
	var out []frameSeqState
	seen := false
	for _, f := range frames {
		if f.gated {
			continue
		}
		if f.sync >= n {
			seen = true
		}
		if seen {
			out = append(out, frameSeq(f))
		}
	}
	return out
}

// chained runs the runner's barrier for token n and then a chained wait over
// frames, on a real frameBus: fast publishes the frames up to the barrier's
// match before it runs and the rest after (a runner that keeps up); otherwise
// every frame is published first (one that does not). It answers what each
// wait matched, "" for a wait that timed out.
func chained(frames []frameState, n int, fast bool, waits ...waitSpec) []string {
	bus := newFrameBus(nil)
	closed := make(chan struct{})
	close(closed)
	i := 0
	if fast {
		for i < len(frames) {
			bus.publish(frames[i])
			i++
			if !frames[i-1].gated && frames[i-1].sync >= n {
				break
			}
		}
	} else {
		for ; i < len(frames); i++ {
			bus.publish(frames[i])
		}
	}
	bus.await(func(s frameState) bool { return s.sync >= n }, 0, closed)
	for ; i < len(frames); i++ {
		bus.publish(frames[i])
	}
	var out []string
	for _, w := range waits {
		s, ok := bus.await(w.match, 0, closed)
		if !ok {
			out = append(out, "")
			continue
		}
		out = append(out, fmt.Sprintf("%+v", frameSeq(s)))
	}
	return out
}

// TestTheGatedFrameSequenceIsTodays (§3.12, astra r3 23; X34): one
// deterministic message schedule per gated site, in every arrival order,
// through the gateSync baseline and asynchronously — /rename (C17), the
// prompt's call sites (C18a, gate_submit_test.go), the queue verbs and the
// Ctrl+C and Esc chains (C18b, gate_queue_test.go), and a card's answer, an
// interjection and the masked opening (C18c, gate_answers_test.go).
func TestTheGatedFrameSequenceIsTodays(t *testing.T) {
	t.Run("rename", renameFrameSequence)
	t.Run("submit", submitFrameSequences)
	t.Run("queue", chainFrameSequences)
	t.Run("answers", answerFrameSequences)
	t.Run("mask", maskFrameSequences)
}

// renameFrameSequence is /rename's: for every order in which the rename's
// reply, its own title delta, a later event and Enter's sync token can arrive,
// the frames a wait can see after Enter's barrier — the same frames, with the
// same sync values — are the gateSync baseline's for the order today's code
// would have seen, and a chained wait over them matches the same frames in
// both modes.
func renameFrameSequence(t *testing.T) {
	var orders [][]string
	var permute func(prefix, rest []string)
	permute = func(prefix, rest []string) {
		if len(rest) == 0 {
			// One reader delivers the stream in Seq order: E1 before E2.
			if slices.Index(prefix, "E1") < slices.Index(prefix, "E2") {
				orders = append(orders, slices.Clone(prefix))
			}
			return
		}
		for i := range rest {
			next := slices.Concat(rest[:i:i], rest[i+1:])
			permute(append(prefix, rest[i]), next)
		}
	}
	permute(nil, []string{"R", "S", "E1", "E2"})
	if len(orders) != 12 {
		t.Fatalf("%d orders, want 12", len(orders))
	}
	waits := []waitSpec{{kind: "text", needle: "renamed to fix it"}, {kind: "text", needle: "after the rename"}}
	for _, order := range orders {
		t.Run(strings.Join(order, ","), func(t *testing.T) {
			base := baselineOrder(order)
			bf, bn := renameSchedule(t, true, base)
			af, an := renameSchedule(t, false, order)
			if bn != an {
				t.Fatalf("the tokens differ: %d, %d", bn, an)
			}
			want, got := matchable(bf, bn), matchable(af, an)
			if len(want) == 0 {
				t.Fatal("the baseline published no frame after Enter's barrier")
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("the matchable frames differ (baseline order %v)\n%s", base, frameSeqDiff(want, got))
			}
			for _, fast := range []bool{true, false} {
				bw, aw := chained(bf, bn, fast, waits...), chained(af, an, fast, waits...)
				if !slices.Equal(bw, aw) {
					t.Fatalf("a chained wait (fast runner %v) matched differently:\ngateSync %q\nasync    %q", fast, bw, aw)
				}
			}
			for _, f := range af {
				if f.gated && (f.sync >= an || strings.Contains(f.plain, "renamed to fix it")) {
					t.Fatalf("a gated frame shows the rename or carries Enter's token (sync %d)", f.sync)
				}
			}
		})
	}
}

func frameSeqDiff(want, got []frameSeqState) string {
	var b strings.Builder
	for i := range max(len(want), len(got)) {
		var w, g frameSeqState
		if i < len(want) {
			w = want[i]
		}
		if i < len(got) {
			g = got[i]
		}
		if w != g {
			fmt.Fprintf(&b, "frame %d: baseline sync %d %s / async sync %d %s\n--- baseline ---\n%s\n--- async ---\n%s\n",
				i, w.sync, w.status, g.sync, g.status, w.plain, g.plain)
			break
		}
	}
	fmt.Fprintf(&b, "(%d baseline frames, %d async)", len(want), len(got))
	return b.String()
}

// TestEngineBackendReturnsAnItemItTook (sol r40 5): an event Read has taken is
// always returned, even when its context was cancelled after the read began —
// it is off the primary, and a reader must never discard it. The context here
// is live when Read checks it and done by the time the receive has won, which
// is the order the select's race allows; the Read answers the event.
func TestEngineBackendReturnsAnItemItTook(t *testing.T) {
	stub := NewStub()
	eng, err := engine.New(stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	b := newEngineBackend(eng, "")
	stub.Emit(agent.Event{Type: agent.EventText, Text: "taken"})
	if err := stub.EventLog().Flush(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	ctx := &cancelledAfterCheck{}
	it, err := b.Read(ctx)
	if err != nil || it.Kind != backend.ItemEvent || it.Event.Text != "taken" {
		t.Fatalf("Read answered %+v, %v; want the event it took", it, err)
	}
	if ctx.Err() == nil {
		t.Fatal("the context was never cancelled: the test proves nothing")
	}
}

// cancelledAfterCheck is a context that is live the first time it is asked
// and cancelled from then on; its Done never closes, so a select can only take
// the event — a Read that re-checked the context after the receive, and
// dropped what it took, would answer the context's error.
type cancelledAfterCheck struct {
	context.Context
	asked bool
}

func (c *cancelledAfterCheck) Err() error {
	if !c.asked {
		c.asked = true
		return nil
	}
	return context.Canceled
}

func (c *cancelledAfterCheck) Done() <-chan struct{} { return nil }

func (c *cancelledAfterCheck) Deadline() (time.Time, bool) { return time.Time{}, false }

func (c *cancelledAfterCheck) Value(any) any { return nil }
