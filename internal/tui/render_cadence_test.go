package tui

import (
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/transcript"
)

// The replay's cadence held to painting every event (plan 032 §3.3 C6): the
// cadence may skip a paint, but every frame it does paint — on a boundary, on
// a forced refresh, at the replay's end — is the frame craze drew when it
// painted every event, and no gesture acts on rows the cadence held back.
// The oracle is the same model with the cadence off (paintEveryEvent). While
// a replay runs nothing here reads a frame the cadence left unpainted.

// cadenceTwins is a loaded model and its oracle: the same model, built the
// same way over the same workspace, provider and clock, with the replay's
// cadence off — painting every event, as craze did before C6.
func cadenceTwins(t *testing.T, cols, rows int, provider *agent.Provider) (lazy, eager Model) {
	t.Helper()
	isolateSkillsHome(t)
	ws := t.TempDir()
	fixed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	build := func(every bool) Model {
		stub := NewStub()
		t.Cleanup(func() { _ = stub.Close() })
		if provider != nil {
			stub.SetProvider(*provider)
		}
		m := New(Config{Session: stub, Theme: "tokyo-night", Workspace: ws, Model: "grok", Yolo: true, Loading: true})
		m.clock = func() time.Time { return fixed }
		m.paintEveryEvent = every
		tm, _ := m.Update(tea.WindowSizeMsg{Width: cols, Height: rows})
		return tm.(Model)
	}
	return build(false), build(true)
}

// cadenceRun drives the twins through the same messages and, after each, holds
// the cadence model to the oracle wherever its drawn pane is painted.
type cadenceRun struct {
	t           *testing.T
	lazy, eager Model
	// compared counts the painted frames compared while the replay ran;
	// caught the gestures that met a pane the cadence had left unpainted;
	// away the events folded with the viewport scrolled away from the
	// bottom, which the cadence paints one by one; boundaries the 256th
	// events.
	compared, caught, away, boundaries int
	// copies counts the gestures that copied something; clip is what the
	// last one put on the clipboard.
	copies int
	clip   []string
}

// newCadenceRun is the twins, with the clipboard recording what is copied
// (the seam is read where the copy is made, on the Update goroutine:
// sendCopy).
func newCadenceRun(t *testing.T, cols, rows int, provider *agent.Provider) *cadenceRun {
	lazy, eager := cadenceTwins(t, cols, rows, provider)
	r := &cadenceRun{t: t, lazy: lazy, eager: eager}
	prev := clipboardWrite
	clipboardWrite = func(s string) error { r.clip = append(r.clip, s); return nil }
	t.Cleanup(func() { clipboardWrite = prev })
	return r
}

// send applies msg to both and compares them.
func (r *cadenceRun) send(msg tea.Msg, what string) {
	r.t.Helper()
	ev, isEvent := msg.(eventMsg)
	if !isEvent && r.lazy.cur().dirty {
		r.caught++
	}
	if isEvent && r.lazy.replaying && r.lazy.vp.Height > 0 && !r.lazy.vp.AtBottom() {
		r.away++
	}
	r.lazy = applyMsg(r.t, r.lazy, msg)
	r.eager = applyMsg(r.t, r.eager, msg)
	if isEvent && r.lazy.replaying && r.lazy.replayFolded%replayPaintEvery == 0 && ev.ev.Type != agent.EventReplay {
		r.boundaries++
	}
	r.check(what)
}

// copyGesture applies a gesture that can copy — a release, a double-click, Ctrl+Y —
// to both, through the handler and then the Update wrapper (the gate is
// closed and nothing is held, so that is Update), and compares what each put
// on the clipboard, whether or not the cadence model's pane is painted after
// it: the handler's own command is the copy, run here against the recording
// clipboard (newCadenceRun).
func (r *cadenceRun) copyGesture(msg tea.Msg, what string) {
	t := r.t
	t.Helper()
	if r.lazy.cur().dirty {
		r.caught++
	}
	var copied [2]string
	for i, m := range []*Model{&r.lazy, &r.eager} {
		if m.gate != nil || len(m.held) > 0 {
			t.Fatalf("%s: a gate is open (%v) or messages are held (%d)", what, m.gate != nil, len(m.held))
		}
		tm, cmd := m.update(msg)
		*m = tm.(Model)
		r.clip = nil
		if cmd != nil {
			if _, ok := cmd().(clipboardDoneMsg); !ok {
				t.Fatalf("%s: the handler's command is not a copy", what)
			}
		}
		copied[i] = strings.Join(r.clip, "\x00")
		*m, _ = m.finish(nil)
	}
	if copied[0] != copied[1] {
		t.Fatalf("%s copied %q; painting every event, %q", what, copied[0], copied[1])
	}
	if copied[0] != "" {
		r.copies++
	}
	r.check(what)
}

// event is a replayed event.
func (r *cadenceRun) event(ev agent.Event) {
	r.t.Helper()
	r.send(eventMsg{ev: replayed(ev)}, fmt.Sprintf("event %s %q", ev.Type, ev.Text))
}

// view enters the sub-agent view of id, or leaves it for "", as a key would,
// with the Update wrapper after it.
func (r *cadenceRun) view(id string) {
	r.t.Helper()
	if r.lazy.cur().dirty {
		r.caught++
	}
	for _, m := range []*Model{&r.lazy, &r.eager} {
		if id == "" {
			m.leaveView()
		} else {
			m.enterView(id)
		}
		*m, _ = m.finish(nil)
	}
	r.check("view " + id)
}

// check compares the twins: always what the cadence does not touch, and
// everything — the frame, the viewport, the selection and what it copies —
// wherever the cadence model's drawn pane is painted. The oracle's always is.
func (r *cadenceRun) check(what string) {
	t := r.t
	t.Helper()
	l, e := r.lazy, r.eager
	if e.cur().dirty {
		t.Fatalf("%s: the oracle left its drawn pane unpainted", what)
	}
	if l.viewing != e.viewing || l.replaying != e.replaying || l.replayFolded != e.replayFolded {
		t.Fatalf("%s: the twins diverged (viewing %q/%q, replaying %v/%v, folded %d/%d)",
			what, l.viewing, e.viewing, l.replaying, e.replaying, l.replayFolded, e.replayFolded)
	}
	if l.cur().dirty {
		if !l.replaying {
			t.Fatalf("%s: a dirty drawn pane outside a replay", what)
		}
		return
	}
	if l.replaying {
		r.compared++
	}
	if l.vp.YOffset != e.vp.YOffset || l.vp.Height != e.vp.Height || l.vp.lines() != e.vp.lines() {
		t.Fatalf("%s: the viewport is at %d of %d lines (height %d); painting every event left it at %d of %d (height %d)",
			what, l.vp.YOffset, l.vp.lines(), l.vp.Height, e.vp.YOffset, e.vp.lines(), e.vp.Height)
	}
	if l.sel != e.sel || l.pressed != e.pressed {
		t.Fatalf("%s: the selection is %+v (pressed %v); painting every event left %+v (pressed %v)", what, l.sel, l.pressed, e.sel, e.pressed)
	}
	if lt, et := l.selectionText(), e.selectionText(); lt != et {
		t.Fatalf("%s: the selection holds %q; painting every event, %q", what, lt, et)
	}
	if lv, ev := l.View(), e.View(); lv != ev {
		t.Fatalf("%s: the frame differs from painting every event\ncadence:\n%s\nevery event:\n%s", what, plain(lv), plain(ev))
	}
}

// region is the transcript band as the twins lay it out (the layout is the
// same for both: it does not depend on what is painted).
func (r *cadenceRun) region() (top, bottom int) {
	tr := r.lazy.lay.Region(regionTranscript)
	return tr.Top, tr.Bottom
}

// cadenceFragments are a reply's chunks; %d is a running number, so the words
// on different rows differ (a double-click on the wrong row copies another).
var cadenceFragments = []string{
	"w%d ", "w%d that wraps ", "a longer run of words, w%d, for the paragraph ", "end %d.\n\n", "\n\n",
	"\n- item %d\n", "\n1. step %d\n", "```\ncode %d\n", "```\n", "| a%d | b |\n", "|---|---|\n",
	"# heading %d\n\n", "> quoted %d\n\n", "`inline%d` ", "**bold%d** ",
}

// cadenceDiff is a diff of n added lines.
func cadenceDiff(n int) []agent.ToolDiff {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return []agent.ToolDiff{{Path: "a.go", OldText: "", NewText: b.String(), Added: n}}
}

// replayMix draws a replay's events and the user's gestures from one seed.
type replayMix struct {
	rng      *rand.Rand
	seq      int
	tools    int
	children []string // spawned, in order
}

// fragment is a random chunk of a reply.
func (x *replayMix) fragment() string {
	x.seq++
	f := cadenceFragments[x.rng.Intn(len(cadenceFragments))]
	if strings.Contains(f, "%d") {
		return fmt.Sprintf(f, x.seq)
	}
	return f
}

// event is one of a replay's events: reply and thought chunks, tools that
// appear and then grow or shrink (an edit's diff growing, or going when it is
// truncated; a command's output), prompts, sub-agents spawned, moving and
// finished, and a child's own reply.
func (x *replayMix) event() agent.Event {
	rng := x.rng
	switch p := rng.Intn(100); {
	case p < 50:
		return agent.Event{Type: agent.EventText, Text: x.fragment()}
	case p < 55:
		return agent.Event{Type: agent.EventThought, Text: "thinking about it "}
	case p < 70:
		if x.tools == 0 || rng.Intn(3) == 0 {
			x.tools++
		}
		// Mostly the newest tool, near the bottom.
		n := x.tools - min(x.tools-1, rng.Intn(3))
		tool := &agent.ToolEvent{ID: "tool-" + strconv.Itoa(n), Kind: "edit", Title: "edit a.go", Status: "in_progress", Locations: []string{"a.go"}}
		switch rng.Intn(4) {
		case 0:
			tool.Diffs = cadenceDiff(1 + rng.Intn(12))
		case 1:
			tool.Diffs = []agent.ToolDiff{{Path: "a.go", Truncated: true}}
			tool.Status = "completed"
		case 2:
			tool.Kind, tool.Title = "execute", "run it"
			tool.RawInput = `{"command":"make test"}`
			if rng.Intn(2) == 0 {
				tool.Output = &agent.ToolOutput{Stdout: "ok\n", StdoutHead: "ok\n"}
			}
		default:
			tool.Status = "completed"
		}
		return agent.Event{Type: agent.EventTool, Tool: tool}
	case p < 74:
		return agent.Event{Type: agent.EventUser, Text: "and then this"}
	case p < 82:
		id := "c" + strconv.Itoa(1+rng.Intn(2))
		info := &agent.SubagentInfo{ID: id, Description: "child " + id, Prompt: "do " + id, Status: agent.SubagentRunning}
		change := agent.SubagentChangeSpawned
		if slices.Contains(x.children, id) {
			change = agent.SubagentChangeProgress
			if rng.Intn(3) == 0 {
				change = agent.SubagentChangeFinished
				info.Status = agent.SubagentCompleted
				info.Output = strings.Repeat("the child's answer ", 1+rng.Intn(30))
				info.Model, info.DurationMs = "grok-4", 1200
			}
		} else {
			x.children = append(x.children, id)
		}
		return agent.Event{Type: agent.EventSubagent, Subagent: info, SubagentChange: change}
	default:
		id := "c" + strconv.Itoa(1+rng.Intn(2))
		return agent.Event{Type: agent.EventText, Agent: id, Text: x.fragment()}
	}
}

// gesture applies one of the user's gestures to the run: the wheel, the page
// keys, a press, a drag (off either edge too, which scrolls), a release, a
// double-click, Ctrl+Y, a view switch, a sub-agent view's arrows, a resize,
// Ctrl+O.
func (x *replayMix) gesture(r *cadenceRun) {
	rng := x.rng
	top, bottom := r.region()
	y := top + rng.Intn(max(1, bottom-top))
	px := rng.Intn(max(1, r.lazy.width))
	switch rng.Intn(15) {
	case 0, 1:
		r.send(tea.MouseMsg{X: px, Y: y, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress}, "wheel up")
	case 2:
		r.send(tea.MouseMsg{X: px, Y: y, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress}, "wheel down")
	case 3:
		r.send(tea.KeyMsg{Type: tea.KeyPgUp}, "pgup")
	case 4:
		r.send(tea.KeyMsg{Type: tea.KeyPgDown}, "pgdown")
	case 5, 6:
		r.send(tea.MouseMsg{X: px, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}, "press")
	case 7, 8:
		y = top - 1 + rng.Intn(max(1, bottom-top+2))
		r.send(tea.MouseMsg{X: px, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion}, "drag")
	case 9:
		r.copyGesture(tea.MouseMsg{X: px, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease}, "release")
	case 10:
		r.copyGesture(dblClickMsg{X: px, Y: y}, "double-click")
	case 11:
		r.copyGesture(tea.KeyMsg{Type: tea.KeyCtrlY}, "ctrl+y")
	case 12:
		switch {
		case r.lazy.viewing != "" && rng.Intn(2) == 0:
			r.view("")
		case len(x.children) > 0:
			r.view(x.children[rng.Intn(len(x.children))])
		}
	case 13:
		if r.lazy.viewing != "" {
			k := []tea.KeyType{tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown}[rng.Intn(4)]
			r.send(tea.KeyMsg{Type: k}, "view key")
			return
		}
		r.send(tea.WindowSizeMsg{Width: 60 + rng.Intn(61), Height: 18 + rng.Intn(19)}, "resize")
	default:
		r.send(tea.KeyMsg{Type: tea.KeyCtrlO}, "ctrl+o")
	}
}

// motif is a short schedule the random mix rarely lands on by itself, each one
// a way a gesture can meet rows the cadence left unpainted, or the rows can
// change under a viewport the user scrolled away:
//   - the rows shrinking under a viewport a few rows above the bottom (an
//     edit's diff truncated just after a wheel up);
//   - a drag off an edge, and a release, over a chunk that landed after the
//     press;
//   - Ctrl+Y over a selection a chunk landed under;
//   - a double-click after the rows moved;
//   - a sub-agent view's keys after its child's rows grew.
func (x *replayMix) motif(r *cadenceRun) {
	rng := x.rng
	top, bottom := r.region()
	mouse := func(y int, b tea.MouseButton, a tea.MouseAction) tea.MouseMsg {
		return tea.MouseMsg{X: 1 + rng.Intn(20), Y: y, Button: b, Action: a}
	}
	switch rng.Intn(5) {
	case 0:
		x.tools++
		id := "tool-" + strconv.Itoa(x.tools)
		edit := func(diffs []agent.ToolDiff) agent.Event {
			return agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: id, Kind: "edit", Title: "edit a.go",
				Status: "completed", Locations: []string{"a.go"}, Diffs: diffs}}
		}
		if r.lazy.viewing != "" {
			r.view("")
		}
		r.event(edit(cadenceDiff(12)))
		r.event(agent.Event{Type: agent.EventText, Text: x.fragment()})
		// Back to the bottom, and three rows up from it.
		for i := 0; i < 40 && !r.lazy.vp.AtBottom(); i++ {
			r.send(tea.KeyMsg{Type: tea.KeyPgDown}, "pgdown")
		}
		r.send(mouse(top+1, tea.MouseButtonWheelUp, tea.MouseActionPress), "wheel up")
		r.event(edit([]agent.ToolDiff{{Path: "a.go", Truncated: true}}))
	case 1:
		r.send(mouse(top+rng.Intn(max(1, bottom-top)), tea.MouseButtonLeft, tea.MouseActionPress), "press")
		for range 1 + rng.Intn(3) {
			r.event(agent.Event{Type: agent.EventText, Text: x.fragment()})
		}
		edge := top
		if rng.Intn(2) == 0 {
			edge = bottom - 1
		}
		r.send(mouse(edge, tea.MouseButtonLeft, tea.MouseActionMotion), "a drag off an edge")
		r.copyGesture(mouse(edge, tea.MouseButtonLeft, tea.MouseActionRelease), "release")
	case 2:
		r.send(mouse(top+1, tea.MouseButtonLeft, tea.MouseActionPress), "press")
		r.send(mouse(bottom-2, tea.MouseButtonLeft, tea.MouseActionMotion), "drag")
		r.copyGesture(mouse(bottom-2, tea.MouseButtonLeft, tea.MouseActionRelease), "release")
		r.event(agent.Event{Type: agent.EventText, Text: x.fragment()})
		r.copyGesture(tea.KeyMsg{Type: tea.KeyCtrlY}, "ctrl+y")
	case 3:
		for range 1 + rng.Intn(3) {
			r.event(agent.Event{Type: agent.EventText, Text: x.fragment()})
		}
		r.copyGesture(dblClickMsg{X: 1 + rng.Intn(10), Y: top + rng.Intn(max(1, bottom-top))}, "double-click")
	default:
		if len(x.children) == 0 {
			return
		}
		id := x.children[rng.Intn(len(x.children))]
		r.view(id)
		for range 2 + rng.Intn(4) {
			r.event(agent.Event{Type: agent.EventText, Agent: id, Text: x.fragment()})
			r.event(agent.Event{Type: agent.EventSubagent, SubagentChange: agent.SubagentChangeProgress,
				Subagent: &agent.SubagentInfo{ID: id, Description: "child " + id, Prompt: "do " + id,
					Status: agent.SubagentRunning, Output: strings.Repeat("so far ", x.seq%40)}})
		}
		k := []tea.KeyType{tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown}[rng.Intn(4)]
		r.send(tea.KeyMsg{Type: k}, "view key")
		r.view("")
	}
}

// TestTheReplayCadenceDrawsWhatPaintingEveryEventDraws is the cadence's oracle
// test: a seeded mix of a replay's events and the user's gestures — the wheel,
// the page keys, presses, drags and releases, double-clicks, Ctrl+Y, view
// switches, a sub-agent view's arrows, resizes, Ctrl+O — runs through the
// model twice, the cadence on and off. Wherever the cadence model's drawn pane
// is painted — a boundary, a forced refresh, a gesture that caught it up, the
// viewport scrolled away — and after the replay's end, its frame, viewport,
// selection and the text that selection holds are the oracle's; and every copy
// is the oracle's, painted or not. Frames the cadence left unpainted are never
// read. Short schedules the mix seldom lands on by itself are mixed in
// (motif). Half the seeds stream the children's transcripts (grok), half rebuild
// them from the receipt.
func TestTheReplayCadenceDrawsWhatPaintingEveryEventDraws(t *testing.T) {
	seeds, ops := 10, 600
	if raceEnabled {
		seeds, ops = 3, 500
	}
	var compared, caught, away, boundaries, copies int
	for seed := range seeds {
		t.Run("seed "+strconv.Itoa(seed), func(t *testing.T) {
			var provider *agent.Provider
			if seed%2 == 0 {
				p := agent.GrokProvider()
				provider = &p
			}
			r := newCadenceRun(t, 100, 30, provider)
			x := &replayMix{rng: rand.New(rand.NewSource(int64(seed)))}
			r.send(eventMsg{ev: replayEvent(agent.ReplayStart)}, "the replay's start")
			for range ops {
				switch p := x.rng.Intn(100); {
				case p < 73:
					r.event(x.event())
				case p < 96:
					x.gesture(r)
				default:
					x.motif(r)
				}
			}
			r.send(eventMsg{ev: replayEvent(agent.ReplayEnd)}, "the replay's end")
			if r.lazy.cur().dirty {
				t.Fatal("the replay's end left the drawn pane unpainted")
			}
			// Live, both paint every event: still one frame.
			for range 60 {
				if x.rng.Intn(100) < 60 {
					ev := x.event()
					r.send(eventMsg{ev: ev}, fmt.Sprintf("live %s", ev.Type))
				} else {
					x.gesture(r)
				}
			}
			compared += r.compared
			caught += r.caught
			away += r.away
			boundaries += r.boundaries
			copies += r.copies
		})
	}
	t.Logf("%d painted mid-replay frames compared; %d gestures caught an unpainted pane up; %d events folded scrolled away; %d boundaries; %d copies",
		compared, caught, away, boundaries, copies)
	if compared == 0 || caught == 0 || away == 0 || boundaries < seeds || copies == 0 {
		t.Fatalf("the mix exercised too little: %d compared, %d caught up, %d scrolled away, %d boundaries, %d copies",
			compared, caught, away, boundaries, copies)
	}
}

// para is a replayed paragraph of its own: two rows.
func para(text string) agent.Event {
	return agent.Event{Type: agent.EventText, Text: text + "\n\n"}
}

// paints is how many times the cadence model has painted its drawn pane.
func (r *cadenceRun) paints() int { return r.lazy.cur().work.paints }

// grownPastAScreen starts a replay, paints a short transcript with a forced
// refresh (a resize to the same size), and then folds n paragraphs — fewer
// than a boundary's worth — that grow it well past a screen, none of them
// painted: the review's schedule (r7 finding 1).
func grownPastAScreen(t *testing.T, r *cadenceRun, n int) {
	t.Helper()
	r.send(eventMsg{ev: replayEvent(agent.ReplayStart)}, "the replay's start")
	for i := range 5 {
		r.event(para("early " + strconv.Itoa(i)))
	}
	before := r.paints()
	r.send(tea.WindowSizeMsg{Width: r.lazy.width, Height: r.lazy.height}, "a resize")
	if r.paints() != before+1 {
		t.Fatal("fixture: the resize did not paint the short transcript")
	}
	before = r.paints()
	for i := range n {
		r.event(para("line " + strconv.Itoa(i)))
	}
	if r.paints() != before || !r.lazy.cur().dirty {
		t.Fatal("fixture: an event between boundaries painted")
	}
	if r.lazy.replayFolded >= replayPaintEvery {
		t.Fatal("fixture: the replay reached a boundary")
	}
}

// TestAScrollMidReplayScrollsTheRowsAsTheyStand (r7 finding 1): the transcript
// grows past a screen while the replay's cadence leaves it unpainted, and the
// user scrolls up before the replay ends — PgUp, the wheel, a sub-agent view's
// ↑ and PgUp. Painting every event, the viewport had followed the rows down,
// so the scroll leaves the bottom and the replay's end keeps it where the user
// put it. The scroll paints the rows first, so it does the same: the final
// frame is the oracle's, and it is not at the bottom.
func TestAScrollMidReplayScrollsTheRowsAsTheyStand(t *testing.T) {
	top := func(r *cadenceRun) int { y, _ := r.region(); return y + 2 }
	for _, tc := range []struct {
		name   string
		viewed bool
		scroll func(r *cadenceRun) tea.Msg
	}{
		{"pgup", false, func(*cadenceRun) tea.Msg { return tea.KeyMsg{Type: tea.KeyPgUp} }},
		{"the wheel", false, func(r *cadenceRun) tea.Msg {
			return tea.MouseMsg{X: 2, Y: top(r), Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress}
		}},
		{"a sub-agent view's up arrow", true, func(*cadenceRun) tea.Msg { return tea.KeyMsg{Type: tea.KeyUp} }},
		{"a sub-agent view's pgup", true, func(*cadenceRun) tea.Msg { return tea.KeyMsg{Type: tea.KeyPgUp} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grok := agent.GrokProvider()
			r := newCadenceRun(t, 100, 30, &grok)
			if !tc.viewed {
				grownPastAScreen(t, r, 100)
			} else {
				r.send(eventMsg{ev: replayEvent(agent.ReplayStart)}, "the replay's start")
				info := &agent.SubagentInfo{ID: "c1", Description: "child", Prompt: "do it", Status: agent.SubagentRunning}
				r.event(agent.Event{Type: agent.EventSubagent, Subagent: info, SubagentChange: agent.SubagentChangeSpawned})
				r.event(agent.Event{Type: agent.EventText, Agent: "c1", Text: "early\n\n"})
				r.view("c1")
				before := r.paints()
				for i := range 100 {
					r.event(agent.Event{Type: agent.EventText, Agent: "c1", Text: "child line " + strconv.Itoa(i) + "\n\n"})
				}
				if r.paints() != before || !r.lazy.cur().dirty {
					t.Fatal("fixture: an event between boundaries painted the child's pane")
				}
			}
			before := r.paints()
			r.send(tc.scroll(r), tc.name)
			painted := r.paints() - before
			// The end's frame is compared with the oracle's first (send).
			r.send(eventMsg{ev: replayEvent(agent.ReplayEnd)}, "the replay's end")
			if r.lazy.vp.AtBottom() || r.eager.vp.AtBottom() {
				t.Fatalf("after %s the replay's end put the viewport at the bottom (%v; painting every event %v)",
					tc.name, r.lazy.vp.AtBottom(), r.eager.vp.AtBottom())
			}
			if painted != 1 {
				t.Fatalf("%s painted %d times: it paints the rows the cadence left unpainted, once, before it scrolls them", tc.name, painted)
			}
		})
	}
}

// TestASelectionOverAnUnpaintedChunkGoesWithIt (r7 finding 2): painting every
// event, a chunk that lands while a selection is up repaints the rows under
// it and clears it, so the gesture that follows works on no selection. With
// the cadence the chunk is not painted; the gesture paints it first — the
// same clear — and so copies, and draws, what painting every event did:
//   - a press, a chunk that turns a paragraph into a table, a drag off the
//     top edge (which scrolls) and a release;
//   - a press, a chunk, and a release on another cell with no motion between
//     (an X10 terminal's click-and-release);
//   - a selection made and copied, a chunk, then Ctrl+Y: the last reply;
//   - a chunk that moves the rows up, then a double-click.
//
// A stalled replay cannot leave the gesture on the old rows: the gesture
// itself paints.
func TestASelectionOverAnUnpaintedChunkGoesWithIt(t *testing.T) {
	press := func(x, y int) tea.Msg {
		return tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	}
	motion := func(x, y int) tea.Msg {
		return tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion}
	}
	release := func(x, y int) tea.Msg {
		return tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease}
	}
	// painted starts a replay whose transcript, past a screen, is painted
	// by a forced refresh, with the reply open on a paragraph that a chunk
	// can make a table of.
	painted := func(t *testing.T) *cadenceRun {
		t.Helper()
		r := newCadenceRun(t, 100, 30, nil)
		grownPastAScreen(t, r, 40)
		r.event(agent.Event{Type: agent.EventText, Text: "| a | b |\n"})
		r.send(tea.WindowSizeMsg{Width: 100, Height: 30}, "a resize")
		return r
	}
	// unpainted folds ev and checks it is left unpainted.
	unpainted := func(t *testing.T, r *cadenceRun, ev agent.Event) {
		t.Helper()
		before := r.paints()
		r.event(ev)
		if r.paints() != before || !r.lazy.cur().dirty {
			t.Fatal("fixture: the chunk was painted")
		}
	}
	table := agent.Event{Type: agent.EventText, Text: "|---|---|\n| c | d |\n| e | f |\n"}

	t.Run("a drag off the edge and a release", func(t *testing.T) {
		r := painted(t)
		top, bottom := r.region()
		r.send(press(3, bottom-2), "press")
		if !r.lazy.sel.on || !r.eager.sel.on {
			t.Fatal("fixture: the press anchored no selection")
		}
		unpainted(t, r, table)
		before := r.paints()
		r.send(motion(10, top), "a drag off the top edge")
		if r.paints() != before+1 {
			t.Fatal("the drag did not paint the chunk before hit-testing it")
		}
		r.copyGesture(release(20, top+3), "release")
	})

	t.Run("a release with no motion", func(t *testing.T) {
		r := painted(t)
		top, bottom := r.region()
		r.send(press(3, bottom-2), "press")
		unpainted(t, r, table)
		r.copyGesture(release(20, top+3), "release")
		if r.lazy.cur().dirty {
			t.Fatal("the release left the chunk unpainted")
		}
	})

	t.Run("ctrl+y over a selection the chunk took", func(t *testing.T) {
		r := painted(t)
		top, bottom := r.region()
		r.send(press(3, top+2), "press")
		r.send(motion(30, bottom-3), "drag")
		r.copyGesture(release(30, bottom-3), "release")
		if r.lazy.sel.empty() || r.copies != 1 {
			t.Fatal("fixture: the drag selected and copied nothing")
		}
		unpainted(t, r, agent.Event{Type: agent.EventText, Text: "more of the reply\n\n"})
		r.copyGesture(tea.KeyMsg{Type: tea.KeyCtrlY}, "ctrl+y")
		if r.copies != 2 {
			t.Fatal("ctrl+y copied nothing")
		}
	})

	t.Run("a double-click after the rows moved", func(t *testing.T) {
		r := painted(t)
		top, _ := r.region()
		for i := range 3 {
			unpainted(t, r, para("moved "+strconv.Itoa(i)))
		}
		// A row of text in the rows as they stand — "line N", its number
		// under the pointer — which the rows last painted have elsewhere.
		y := top
		for e := r.eager; e.cur().plainRow(e.vp.YOffset+y-top) == ""; y++ {
		}
		r.copyGesture(dblClickMsg{X: 6, Y: y}, "double-click")
		if r.copies != 1 {
			t.Fatal("the double-click copied no word")
		}
	})
}

// TestAReceiptPaneKeepsTheReplaysCadence (r7 finding 3): a receipt-only
// sub-agent's view — its rows rebuilt from the roster on every one of its
// roster events — is the drawn pane during a replay. Those events rebuild it
// and leave it unpainted between boundaries, its last paint still the drawn
// rows (the hit test reads them until the next paint); a boundary paints it,
// and so does the replay's end, whose frame is the oracle's. Outside a replay
// the event paints at once, as it always has.
func TestAReceiptPaneKeepsTheReplaysCadence(t *testing.T) {
	r := newCadenceRun(t, 100, 30, nil)
	if r.lazy.showSubagentTranscript() {
		t.Fatal("fixture: the stub's provider streams its children")
	}
	r.send(eventMsg{ev: replayEvent(agent.ReplayStart)}, "the replay's start")
	info := func(out string, change string) agent.Event {
		st := agent.SubagentRunning
		if change == agent.SubagentChangeFinished {
			st = agent.SubagentCompleted
		}
		return agent.Event{Type: agent.EventSubagent, SubagentChange: change, Subagent: &agent.SubagentInfo{
			ID: "c1", Description: "child", Prompt: "count the lines", Status: st, Output: out}}
	}
	r.event(info("", agent.SubagentChangeSpawned))
	r.view("c1")
	pane := r.lazy.cur()
	if pane != r.lazy.subs["c1"] || pane.drawn == nil {
		t.Fatal("fixture: the receipt pane is not the painted, drawn one")
	}
	// A selection is up over the receipt when its roster moves.
	top, _ := r.region()
	r.send(tea.MouseMsg{X: 2, Y: top, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}, "press")
	drawn, paints := pane.drawn, pane.work.paints
	for i := 0; r.lazy.replayFolded%replayPaintEvery != replayPaintEvery-1; i++ {
		if i%2 == 0 {
			r.event(info(strings.Repeat("an answer that grows ", i/2), agent.SubagentChangeProgress))
		} else {
			r.event(para("main " + strconv.Itoa(i)))
		}
		if pane.work.paints != paints {
			t.Fatalf("event %d of the replay painted the receipt between boundaries", r.lazy.replayFolded)
		}
		if pane.drawn != drawn {
			t.Fatalf("event %d of the replay dropped the receipt's drawn rows before a paint", r.lazy.replayFolded)
		}
	}
	if !r.lazy.sel.on {
		t.Fatal("a rebuild between boundaries cleared the selection without a paint")
	}
	r.event(info(strings.Repeat("the last of it ", 40), agent.SubagentChangeProgress))
	if pane.work.paints != paints+1 {
		t.Fatalf("the boundary painted the receipt %d times", pane.work.paints-paints)
	}
	r.event(info(strings.Repeat("the answer ", 50), agent.SubagentChangeFinished))
	if pane.work.paints != paints+1 {
		t.Fatal("the finish after the boundary painted the receipt")
	}
	r.send(eventMsg{ev: replayEvent(agent.ReplayEnd)}, "the replay's end")
	if pane.work.paints != paints+2 || pane.dirty {
		t.Fatalf("the replay's end painted the receipt %d times", pane.work.paints-paints-1)
	}
	// Live, a roster event paints the receipt at once.
	r.send(eventMsg{ev: info(strings.Repeat("and more ", 60), agent.SubagentChangeFinished)}, "a live finish")
	if pane.work.paints != paints+3 {
		t.Fatal("a live roster event left the receipt unpainted")
	}
}

// TestEachReplayCountsItsOwnEvents (r7 finding 4): the cadence counts the
// events of the replay it is in. A second replay in the same model paints on
// its own 256th event, whatever the first counted; a restore that adopts
// another incarnation mid-replay counts again from the restore; one of the
// same incarnation is the same replay and keeps its count.
func TestEachReplayCountsItsOwnEvents(t *testing.T) {
	// fold folds n replayed lines, each painting exactly when the replay's
	// count reaches a boundary, and reports the counts at which they did.
	fold := func(t *testing.T, m *Model, n int) []int {
		t.Helper()
		var at []int
		for i := range n {
			before := m.main.work.paints
			*m = applyMsg(t, *m, eventMsg{ev: replayLine(i)})
			if m.main.work.paints > before {
				at = append(at, i+1)
			}
		}
		return at
	}
	t.Run("a second replay", func(t *testing.T) {
		m, _ := loadedStub(t, nil)
		m = feed(t, m, replayEvent(agent.ReplayStart))
		if at := fold(t, &m, 100); len(at) != 0 {
			t.Fatalf("the first replay painted at %v", at)
		}
		m = feed(t, m, replayEvent(agent.ReplayEnd), replayEvent(agent.ReplayStart))
		// The start is the second replay's first event.
		if at := fold(t, &m, 2*replayPaintEvery); len(at) != 2 || at[0] != replayPaintEvery-1 || at[1] != 2*replayPaintEvery-1 {
			t.Fatalf("the second replay painted after %v of its lines, want %d and %d", at, replayPaintEvery-1, 2*replayPaintEvery-1)
		}
	})
	// restore applies a restore of a snapshot of incarnation inc that is
	// replaying, as a client attached mid-replay receives one.
	restore := func(t *testing.T, m Model, snap *transcript.Snapshot) Model {
		t.Helper()
		info := m.info()
		info.Incarnation = snap.Incarnation
		m = applyMsg(t, m, restoreMsg{snap: snap, info: info})
		if !m.replaying || m.shared.Incarnation() != snap.Incarnation {
			t.Fatalf("fixture: the restore left replaying %v under %q", m.replaying, m.shared.Incarnation())
		}
		return m
	}
	attached := func(t *testing.T) Model {
		t.Helper()
		m, _ := loadedStub(t, nil)
		m = restore(t, m, snapshotFolded(t, "inc-a", replayEvent(agent.ReplayStart)))
		if at := fold(t, &m, 100); len(at) != 0 {
			t.Fatalf("the replay painted at %v", at)
		}
		return m
	}
	t.Run("a restore of another incarnation", func(t *testing.T) {
		m := attached(t)
		m = restore(t, m, snapshotFolded(t, "inc-b", replayEvent(agent.ReplayStart)))
		if at := fold(t, &m, replayPaintEvery); len(at) != 1 || at[0] != replayPaintEvery {
			t.Fatalf("after another incarnation's restore the replay painted after %v of its lines, want %d", at, replayPaintEvery)
		}
	})
	t.Run("a restore of the same incarnation", func(t *testing.T) {
		m := attached(t)
		snap, err := m.shared.Snapshot(0)
		if err != nil {
			t.Fatal(err)
		}
		m = restore(t, m, snap)
		// 100 lines are counted already.
		if at := fold(t, &m, replayPaintEvery); len(at) != 1 || at[0] != replayPaintEvery-100 {
			t.Fatalf("after the same incarnation's restore the replay painted after %v of its lines, want %d", at, replayPaintEvery-100)
		}
	})
}

// TestAReplayScrolledAwayPaintsEveryEvent: the cadence holds only while the
// viewport follows the bottom. The user scrolls up a few rows mid-replay, and
// a tool row above loses its diff: painting every event, the rows shrank to
// the viewport, which went to the bottom and followed it from then on. With
// the cadence skipping paints the viewport would not have seen the rows
// shrink, and the replay's end would have left it scrolled up — so while it
// is scrolled away every event paints, and the end's frame is the oracle's.
func TestAReplayScrolledAwayPaintsEveryEvent(t *testing.T) {
	r := newCadenceRun(t, 100, 30, nil)
	r.send(eventMsg{ev: replayEvent(agent.ReplayStart)}, "the replay's start")
	edit := func(diffs []agent.ToolDiff) agent.Event {
		return agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1", Kind: "edit", Title: "edit a.go",
			Status: "completed", Locations: []string{"a.go"}, Diffs: diffs}}
	}
	r.event(edit(cadenceDiff(8)))
	for i := range 60 {
		r.event(para("line " + strconv.Itoa(i)))
	}
	top, _ := r.region()
	r.send(tea.MouseMsg{X: 2, Y: top + 2, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress}, "wheel up")
	if r.lazy.vp.AtBottom() {
		t.Fatal("fixture: the wheel left the viewport at the bottom")
	}
	before := r.paints()
	r.event(edit([]agent.ToolDiff{{Path: "a.go", Truncated: true}}))
	if r.paints() != before+1 {
		t.Fatal("an event with the viewport scrolled away was not painted")
	}
	if !r.lazy.vp.AtBottom() {
		t.Fatal("fixture: the shrunk rows did not bring the viewport to the bottom")
	}
	before = r.paints()
	for i := range 20 {
		r.event(para("more " + strconv.Itoa(i)))
	}
	if r.paints() != before {
		t.Fatal("back at the bottom, an event between boundaries painted")
	}
	r.send(eventMsg{ev: replayEvent(agent.ReplayEnd)}, "the replay's end")
	if !r.lazy.vp.AtBottom() {
		t.Fatal("the replay's end left the viewport above the bottom")
	}
}

// TestAQuitMidReplayPaintsTheLastFrame (r12 P2): Bubble Tea takes a quit
// without another Update and draws View once more, as the program's last
// frame. A quit mid-replay, with the events folded since the last boundary
// still unpainted and the viewport following the bottom, paints them: the
// Update that sets quitting paints the drawn pane, and so does every event
// applied while the quit waits. So the last frame — after Ctrl+D, after an
// event that lands while the quit waits, and after a served session's second
// Ctrl+D, whose tea.Quit comes at once (stopQuit) — is the frame painting
// every event draws.
func TestAQuitMidReplayPaintsTheLastFrame(t *testing.T) {
	for _, tc := range []struct {
		name string
		// remote quits as a session served elsewhere does (stopQuit): the
		// second quit answers tea.Quit in its own Update.
		remote bool
	}{
		{"ctrl+d", false},
		{"a served session's ctrl+d, twice", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCadenceRun(t, 100, 30, nil)
			r.lazy.remote, r.eager.remote = tc.remote, tc.remote
			r.send(eventMsg{ev: replayEvent(agent.ReplayStart)}, "the replay's start")
			for i := 0; r.lazy.replayFolded < replayPaintEvery+40; i++ {
				r.event(para("line " + strconv.Itoa(i)))
			}
			if r.boundaries == 0 || !r.lazy.cur().dirty || !r.lazy.vp.AtBottom() {
				t.Fatalf("fixture: %d boundaries; the pane dirty %v, the viewport at the bottom %v",
					r.boundaries, r.lazy.cur().dirty, r.lazy.vp.AtBottom())
			}
			// last holds the cadence model's last frame, were the program to
			// end here, to the oracle's: painted, and the same.
			last := func(what string) {
				t.Helper()
				if r.lazy.cur().dirty {
					t.Fatalf("%s left the drawn pane unpainted: the last frame misses the events folded since the boundary", what)
				}
				if lv, ev := r.lazy.View(), r.eager.View(); lv != ev {
					t.Fatalf("after %s the last frame differs from painting every event\ncadence:\n%s\nevery event:\n%s", what, plain(lv), plain(ev))
				}
			}
			r.send(tea.KeyMsg{Type: tea.KeyCtrlD}, "ctrl+d")
			if !r.lazy.quitting || !r.lazy.replaying || (tc.remote && !r.lazy.exit.stopping) {
				t.Fatalf("fixture: after ctrl+d quitting %v, replaying %v, stopping %v", r.lazy.quitting, r.lazy.replaying, r.lazy.exit.stopping)
			}
			last("the quit")
			r.event(para("while the quit waits"))
			last("an event while the quit waits")
			if !tc.remote {
				return
			}
			var quits [2]int
			for i, m := range []*Model{&r.lazy, &r.eager} {
				tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
				*m = tm.(Model)
				quits[i] = len(namedCmds(cmd, "bubbletea.Quit"))
			}
			if quits != [2]int{1, 1} {
				t.Fatalf("fixture: the second quit answered %v tea.Quit, want one each", quits)
			}
			r.check("the second quit")
			last("the second quit")
		})
	}
}
