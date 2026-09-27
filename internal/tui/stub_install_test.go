package tui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// The Stub publishes what the live session would (plan 027 §3.13, C19): the
// install at Start, which TestMain turns on for every Stub this package builds,
// and the delta of each catalog setter. The three list setters stay
// fixture-only snapshot writers.

// installShaped reports that ev is an install delta: one EventMeta carrying
// every section the live session's installDeltaLocked does.
func installShaped(ev agent.Event) bool {
	st := ev.State
	return ev.Type == agent.EventMeta && st != nil && st.Title != nil && st.Mode != nil && st.Model != nil &&
		st.Config != nil && st.Commands != nil && st.Plugins != nil
}

// nextOnPrimary is the event already on s's primary, failing the test when
// there is none: every caller has flushed, or knows its event was published
// before it asks.
func nextOnPrimary(t *testing.T, s *Stub, what string) agent.Event {
	t.Helper()
	select {
	case ev := <-s.Events():
		return ev
	default:
		t.Fatalf("nothing on the primary, want %s", what)
		return agent.Event{}
	}
}

// primaryEmpty fails the test when anything more is on s's primary.
func primaryEmpty(t *testing.T, s *Stub, after string) {
	t.Helper()
	select {
	case ev := <-s.Events():
		t.Fatalf("%s, and then %+v", after, ev)
	default:
	}
}

// TestStubInstallsAtStartAsTheLiveSessionDoes pins the install the Stub
// publishes at Start, on for every Stub this package builds: one EventMeta
// carrying every section of the snapshot as it stands, with no cause and no
// text, placed where the live session places it — the one event a new
// session's Start publishes (live.go:723), and a load's last before
// EventReplay{end}, after every replayed event (live.go:927).
func TestStubInstallsAtStartAsTheLiveSessionDoes(t *testing.T) {
	if !NewStub().InstallOnStart {
		t.Fatal("TestMain turns the Stub's install on for every Stub this package builds")
	}
	isInstall := func(t *testing.T, s *Stub, ev agent.Event) {
		t.Helper()
		snap := s.Snapshot()
		if !installShaped(ev) || ev.Cause != "" || ev.Text != "" || ev.Replayed {
			t.Fatalf("the install is %+v", ev)
		}
		st := ev.State
		if *st.Title != snap.Title || *st.Mode != snap.CurrentMode || *st.Model != snap.CurrentModel ||
			!reflect.DeepEqual(st.Config.Options, snap.Config) ||
			!reflect.DeepEqual(noneIsNil(st.Commands.Commands), noneIsNil(snap.Commands)) ||
			!reflect.DeepEqual(noneIsNil(st.Plugins.Plugins), noneIsNil(snap.Plugins)) {
			t.Fatalf("the install carries %+v, want every section of the snapshot %+v", *st, snap)
		}
	}

	t.Run("a new session", func(t *testing.T) {
		s := NewStub()
		t.Cleanup(func() { _ = s.Close() })
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Enqueued and not flushed, as live.go:723 has it: the barrier is the
		// log's own.
		if err := s.EventLog().Flush(context.Background(), nil); err != nil {
			t.Fatalf("flush: %v", err)
		}
		ev := nextOnPrimary(t, s, "the install")
		isInstall(t, s, ev)
		if ev.Seq != 1 {
			t.Fatalf("the install is seq %d, want the session's first event", ev.Seq)
		}
		primaryEmpty(t, s, "a new session's Start publishes its install")
	})

	t.Run("a load", func(t *testing.T) {
		s := NewStub()
		t.Cleanup(func() { _ = s.Close() })
		s.Replay = []agent.Event{
			{Type: agent.EventUser, Text: "yesterday's prompt"},
			{Type: agent.EventText, Text: "restored"},
		}
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if ev := nextOnPrimary(t, s, "the start bracket"); ev.Type != agent.EventReplay || ev.Replay.Phase != agent.ReplayStart {
			t.Fatalf("first event %+v, want the start bracket", ev)
		}
		for range s.Replay {
			if ev := nextOnPrimary(t, s, "a replayed event"); !ev.Replayed || installShaped(ev) {
				t.Fatalf("event %+v, want a replayed event", ev)
			}
		}
		isInstall(t, s, nextOnPrimary(t, s, "the install"))
		if ev := nextOnPrimary(t, s, "the end bracket"); ev.Type != agent.EventReplay || ev.Replay.Phase != agent.ReplayEnd {
			t.Fatalf("event %+v after the install, want the end bracket", ev)
		}
		primaryEmpty(t, s, "a load's Start ends on its end bracket")
	})
}

// TestStubCatalogSettersPublishTheirDelta: SetCommands and SetPlugins publish
// the delta a live session's available_commands_update does, one section each,
// committed before the setter returns; SetTodos, SetTools and SetSubagents stay
// fixture-only snapshot writers and publish nothing (plan 027 §3.13).
func TestStubCatalogSettersPublishTheirDelta(t *testing.T) {
	s := NewStub()
	t.Cleanup(func() { _ = s.Close() })

	cmds := []agent.CommandInfo{{Name: "review", Description: "a new one"}}
	s.SetCommands(cmds)
	if ev := nextOnPrimary(t, s, "the commands delta"); ev.Type != agent.EventMeta || ev.Cause != "" ||
		!reflect.DeepEqual(ev.State, &agent.StateDelta{Commands: &agent.CommandsState{Commands: cmds}}) {
		t.Fatalf("SetCommands published %+v, want its Commands section alone", ev)
	}
	plugins := []agent.PluginCommand{{Plugin: "p", Bare: "c", Display: "c", Qualified: "p:c", Kind: agent.PluginKindCommand}}
	s.SetPlugins(plugins)
	if ev := nextOnPrimary(t, s, "the plugins delta"); ev.Type != agent.EventMeta || ev.Cause != "" ||
		!reflect.DeepEqual(ev.State, &agent.StateDelta{Plugins: &agent.PluginsState{Plugins: plugins}}) {
		t.Fatalf("SetPlugins published %+v, want its Plugins section alone", ev)
	}
	if snap := s.Snapshot(); !reflect.DeepEqual(snap.Commands, cmds) || !reflect.DeepEqual(snap.Plugins, plugins) {
		t.Fatalf("the snapshot holds %+v and %+v", snap.Commands, snap.Plugins)
	}

	s.SetTodos([]agent.Todo{{ID: "1", Content: "Read", Status: "pending"}})
	s.SetTools([]agent.ToolEvent{{ID: "t1", Kind: "read", Status: "in_progress"}})
	s.SetSubagents([]agent.SubagentInfo{{ID: "task-1", Status: agent.SubagentRunning}})
	if err := s.EventLog().Flush(context.Background(), nil); err != nil {
		t.Fatalf("flush: %v", err)
	}
	primaryEmpty(t, s, "the list setters are snapshot writers")
}

// TestTheStubsStartAndCatalogsReachTheFold is the TUI's side of the two above:
// a model whose session starts as Init starts it (sizedLikeInit) folds the
// install's settings before any pump; and a catalog a test sets reaches the
// fold by the setter's own delta, with no event the test publishes on the
// session's behalf.
func TestTheStubsStartAndCatalogsReachTheFold(t *testing.T) {
	m := sizedLikeInit(t)
	stub := stubOf(t, m)
	snap, st := stub.Snapshot(), m.shared.State().Settings
	if st.Model != snap.CurrentModel || st.Mode != snap.CurrentMode ||
		!reflect.DeepEqual(st.Config, snap.Config) || !reflect.DeepEqual(st.Commands, snap.Commands) {
		t.Fatalf("after the start the fold holds %+v, want the install's sections of %+v", st, snap)
	}

	cmds := []agent.CommandInfo{{Name: "research"}, {Name: "review", Description: "a new one"}}
	stub.SetCommands(cmds)
	plugins := []agent.PluginCommand{{Plugin: "p", Bare: "c", Display: "c", Qualified: "p:c", Kind: agent.PluginKindCommand}}
	stub.SetPlugins(plugins)
	m = pumpSettled(t, m)
	st = m.shared.State().Settings
	if !reflect.DeepEqual(st.Commands, cmds) || !reflect.DeepEqual(st.Plugins, plugins) {
		t.Fatalf("the fold holds the commands %+v and the plugins %+v, want the ones set: %+v, %+v", st.Commands, st.Plugins, cmds, plugins)
	}
	if !reflect.DeepEqual(m.snap.Commands, cmds) || !reflect.DeepEqual(m.snap.Plugins, plugins) {
		t.Fatalf("the TUI's mirror holds %+v and %+v", m.snap.Commands, m.snap.Plugins)
	}
}

// installFrameRun is one run of a Stub golden script for
// TestStubGoldensHoldWithTheInstall.
type installFrameRun struct {
	plain, raw string
	err        error
	// frames is every frame the program published, header and plain text,
	// with a run of equal frames kept once.
	frames []string
	// events is every event the Stub's log committed, in order.
	events []agent.Event
}

// frameHeader is the frame bus's PrintFrames header (frameBus.publish), less
// the frame's number.
var frameHeader = regexp.MustCompile(`(?m)^--- frame \d+ (.*) ---\n`)

// distinctFrames splits the frame bus's printed frames and keeps a run of
// equal ones once: two runs that differ only by a frame equal to the one
// before it — an Update that changed nothing drawn — published the same
// frames for every wait to match.
func distinctFrames(printed string) []string {
	idx := frameHeader.FindAllStringSubmatchIndex(printed, -1)
	var out []string
	for i, at := range idx {
		end := len(printed)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		f := printed[at[2]:at[3]] + "\n" + strings.TrimSuffix(printed[at[1]:end], "\n")
		if len(out) == 0 || out[len(out)-1] != f {
			out = append(out, f)
		}
	}
	return out
}

// lockedBuffer is a bytes.Buffer the frame bus may write while the test reads
// it afterwards.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runInstallFrame runs script over a fresh Stub with the install on or off, in
// one gate mode, frozen so a tick's frame draws nothing new, and keeps every
// frame the program published and every event the Stub's log committed.
func runInstallFrame(t *testing.T, cols, rows int, script string, replay []agent.Event, install, sync bool) installFrameRun {
	t.Helper()
	stub := NewStub()
	stub.InstallOnStart = install
	stub.Replay = replay
	sub, err := stub.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	var recs []agent.Record
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for rec := range sub.Records() {
			recs = append(recs, rec)
		}
	}()
	var printed lockedBuffer
	var r installFrameRun
	r.plain, r.raw, r.err = RunFrameScript(Config{
		Session:   stub,
		Theme:     "tokyo-night",
		Workspace: frameWorkspace(t),
		Model:     "grok",
		Yolo:      true,
		Loading:   replay != nil,
	}, cols, rows, script, FrameOpts{Timeout: 10 * time.Second, Freeze: true, PrintFrames: true, Out: &printed, gateSync: sync})
	// The run closed its session, and with it the log, which ends the
	// subscription: what it had accepted and not delivered is its Rest.
	_ = stub.Close()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the subscription did not end with the session")
	}
	rest, _ := sub.Rest()
	for _, rec := range append(recs, rest...) {
		ev, err := rec.Event()
		if err != nil {
			t.Fatalf("record %d: %v", rec.Seq, err)
		}
		r.events = append(r.events, ev)
	}
	r.frames = distinctFrames(printed.String())
	return r
}

// checkInstallPlace holds a run's stream to the install's place: exactly one,
// where the live session puts it, when on — a new session's first event, a
// load's last before EventReplay{end}, after every replayed event — and none
// when off.
func checkInstallPlace(t *testing.T, evs []agent.Event, load, on bool) {
	t.Helper()
	var at []int
	for i, ev := range evs {
		if installShaped(ev) {
			at = append(at, i)
		}
	}
	if !on {
		if len(at) != 0 {
			t.Fatalf("the install is off and the stream carries one at %v", at)
		}
		return
	}
	if len(at) != 1 {
		t.Fatalf("the install is on and the stream carries %d of them", len(at))
	}
	i := at[0]
	if !load {
		if i != 0 || evs[0].Seq != 1 {
			t.Fatalf("a new session's install is event %d, seq %d, want its first", i, evs[i].Seq)
		}
		return
	}
	if i == 0 || !evs[i-1].Replayed || i+1 >= len(evs) ||
		evs[i+1].Type != agent.EventReplay || evs[i+1].Replay == nil || evs[i+1].Replay.Phase != agent.ReplayEnd {
		t.Fatalf("a load's install is event %d, want it between the last replayed event and the end bracket", i)
	}
}

// eventKinds is a stream's shape: each event's kind, with the install left out.
func eventKinds(evs []agent.Event) []string {
	var out []string
	for _, ev := range evs {
		if !installShaped(ev) {
			out = append(out, fmt.Sprintf("%s replayed=%v", ev.Type, ev.Replayed))
		}
	}
	return out
}

// TestStubGoldensHoldWithTheInstall is plan 027 §3.13's named check for the
// Stub's install, as the mirror onto the fold (C21) leaves it. C19 ran it with
// the install on and off to show the install moved no frame while the mirror
// still read the session live; since C21 the install is where every frame's
// settings come from — the fold holds what the stream said — so with it off a
// frame has no model, mode or catalog to draw, and the comparison with the
// install off is gone. What it holds now, for representative Stub goldens — a
// turn, a settings change through the dialog, a title delta, a load — in both
// gate modes:
//
//   - the captured frame is the golden's, byte for byte, with the install on
//     (the golden suite itself runs with it on);
//   - the install is on the stream exactly where the live session puts it — a
//     new session's first event, a load's last before EventReplay{end} —
//     exactly once;
//   - with the install off there is none on the stream, and, for a script whose
//     steps do not need the install's settings to be taken, the rest of the
//     stream is the same either way. (The dialog's script cannot run without
//     them: its tab is the install's catalog.)
func TestStubGoldensHoldWithTheInstall(t *testing.T) {
	// TestFrameGoldenReplay's transcript.
	replay := []agent.Event{
		{Type: agent.EventUser, Text: "List the files in the current working directory in one line."},
		{Type: agent.EventThought, Text: "Recalling the earlier turn."},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "replay-0-1", Title: "List Directory", Kind: "read", Status: "completed"}},
		{Type: agent.EventText, Text: "the workspace holds main.py and README.md"},
	}
	for _, c := range []struct {
		golden     string
		cols, rows int
		script     string
		replay     []agent.Event
		// offRuns says the script's steps can be taken with the install off.
		offRuns bool
	}{
		{"echo-80x24", 80, 24, "<wait:idle>" + echoPrompt() + "<enter><wait:text:echo:><wait:idle>", nil, true},
		{"model-dialog-fast-100x30", 100, 30, "<wait:idle>/model<enter><tab><tab><right><enter><wait:text:fast → on>", nil, false},
		{"rename-100x30", 100, 30, "<wait:idle>/rename fix the flaky pty test<enter><wait:text:renamed>", nil, true},
		{"replay-100x30", 100, 30, "<wait:text:restored><wait:idle>", replay, true},
	} {
		t.Run(c.golden, func(t *testing.T) {
			isolateSkillsHome(t)
			want, err := os.ReadFile(filepath.Join("testdata", c.golden+".golden"))
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range frameGateModes {
				on := runInstallFrame(t, c.cols, c.rows, c.script, c.replay, true, mode.sync)
				if on.err != nil {
					t.Fatalf("%s: %v", mode.name, on.err)
				}
				if on.plain != string(want) {
					t.Fatalf("%s: the captured frame is not the golden's (%s)\n--- golden ---\n%s\n--- got ---\n%s",
						mode.name, frameLineDiff(string(want), on.plain), want, on.plain)
				}
				checkInstallPlace(t, on.events, c.replay != nil, true)
				if !c.offRuns {
					t.Logf("%s: %d distinct frames; %d events", mode.name, len(on.frames), len(on.events))
					continue
				}
				off := runInstallFrame(t, c.cols, c.rows, c.script, c.replay, false, mode.sync)
				if off.err != nil {
					t.Fatalf("%s, the install off: %v", mode.name, off.err)
				}
				checkInstallPlace(t, off.events, c.replay != nil, false)
				if a, b := eventKinds(on.events), eventKinds(off.events); !slices.Equal(a, b) {
					t.Fatalf("%s: the rest of the stream differs with the install on and off:\n on: %v\noff: %v", mode.name, a, b)
				}
				t.Logf("%s: %d distinct frames; %d events with the install, %d without", mode.name, len(on.frames), len(on.events), len(off.events))
			}
		})
	}
}
