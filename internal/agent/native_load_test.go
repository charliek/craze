package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// A native load (plan 028 §3.4): Start with Options.LoadSessionID reopens the
// stored session and replays it inside the EventReplay bracket, as the live
// session's session/load does (live.go's loadSession). The stored sessions are
// written by real native sessions on the scripted models, so what is replayed
// is what the harness really stores.

// storeNativeSession runs one turn per step list, prompted "prompt <n>", on a
// new session started with opts, and closes it: the stored session a load
// reopens. It returns the session's id. Each turn's events are drained as it
// ends, so any number of turns fits the primary.
func storeNativeSession(t *testing.T, f *nativeFixture, opts Options, alias string, turns ...[]step) string {
	t.Helper()
	s := f.started(opts)
	for i, steps := range turns {
		f.models[alias].push(steps...)
		if _, err := s.Prompt(context.Background(), fmt.Sprintf("prompt %d", i+1)); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		deltaSettled(t, s)
	}
	id := s.Snapshot().SessionID
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return id
}

// storedPath is session id's transcript among the fixture's files.
func storedPath(t *testing.T, f *nativeFixture, id string) string {
	t.Helper()
	for _, p := range f.transcripts() {
		if strings.HasSuffix(p, "_"+id+".jsonl") {
			return p
		}
	}
	t.Fatalf("no transcript for session %s among %v", id, f.transcripts())
	return ""
}

// startLoad starts s, a load, with nobody reading the primary — every stored
// session here replays far fewer events than the primary holds — and returns
// Start's error and everything the session published, in order. Every event
// is in the primary once Start has returned: nothing a load publishes waits
// for Start's return, so what is here is exactly what came before it, and
// the engine's Started, which Engine.Start calls after sess.Start returns, can
// only follow the last of them.
func startLoad(t *testing.T, s *nativeSession) ([]Event, error) {
	t.Helper()
	var err error
	within(t, "the load's Start", func() { err = s.Start(context.Background()) })
	_ = s.log.Flush(context.Background(), nil)
	return drained(s), err
}

// lockWatch fails a publish made while one of the adapter's three locks is
// held (S2's third condition, plan 028 §3.4): the log's admitting hook runs
// on the publishing goroutine just before a Publish enters the boundary, and
// no other goroutine touches the three while a load runs with no reader, so a
// lock that cannot be taken there is the publisher's own.
type lockWatch struct {
	mu   sync.Mutex
	held []string
}

func watchLocks(s *nativeSession) *lockWatch {
	w := &lockWatch{}
	s.log.hooks = &logHooks{admitting: func(k admitKind) {
		if k != admitPublish {
			return
		}
		for _, l := range []struct {
			name string
			mu   *sync.Mutex
		}{{"s.mu", &s.mu}, {"toolMu", &s.toolMu}, {"rosterMu", &s.rosterMu}} {
			if !l.mu.TryLock() {
				w.mu.Lock()
				w.held = append(w.held, l.name)
				w.mu.Unlock()
				continue
			}
			l.mu.Unlock()
		}
	}}
	return w
}

func (w *lockWatch) check(t *testing.T) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.held) != 0 {
		t.Fatalf("published while holding %v", w.held)
	}
}

// loadLine is one event of a load, on one line, for comparing whole
// sequences: the kind, what it carries, and whether it was stamped replayed.
func loadLine(ev Event) string {
	mark := ""
	if ev.Replayed {
		mark = " [r]"
	}
	switch ev.Type {
	case EventReplay:
		return "replay:" + string(ev.Replay.Phase) + mark
	case EventUser:
		if ev.Interjection {
			return "steer: " + ev.Text + mark
		}
		return "user: " + ev.Text + mark
	case EventText:
		return "text: " + ev.Text + mark
	case EventThought:
		return "thought: " + ev.Text + mark
	case EventTool:
		return "tool " + ev.Tool.ID + " " + ev.Tool.Status + mark
	case EventTodos:
		return fmt.Sprintf("todos %d%s", len(ev.Todos), mark)
	case EventMeta:
		var parts []string
		st := ev.State
		if st.Title != nil {
			parts = append(parts, "title="+*st.Title)
		}
		if st.Model != nil {
			parts = append(parts, "model="+*st.Model)
		}
		if st.Mode != nil {
			parts = append(parts, "mode="+*st.Mode)
		}
		if st.Config != nil {
			parts = append(parts, "config")
		}
		if st.Commands != nil {
			parts = append(parts, fmt.Sprintf("commands=%d", len(st.Commands.Commands)))
		}
		if st.Plugins != nil {
			parts = append(parts, "plugins")
		}
		return "meta " + strings.Join(parts, " ") + mark
	}
	return string(ev.Type) + mark
}

func loadLines(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, loadLine(ev))
	}
	return out
}

// lastTool is the last row published for id.
func lastTool(t *testing.T, evs []Event, id string) ToolEvent {
	t.Helper()
	var row *ToolEvent
	for _, ev := range evs {
		if ev.Type == EventTool && ev.Tool.ID == id {
			row = ev.Tool
		}
	}
	if row == nil {
		t.Fatalf("no row for %s", id)
	}
	return *row
}

// TestNativeLoadReplaysTheTranscript (A6, the adapter's half; plan 028 §3.4):
// a stored session with a background agent call, the wake that delivered its
// result, and a turn whose prompt carries a shell-context block and whose tool
// step takes a steer, reopened by a load. What the load publishes, in order:
// the seeded title delta, the opening bracket, the replay — each prompt as a
// user row with the block stripped, the steer as an interjection row, the
// results entry as nothing, each call as a row under its replayed id
// <entry id>.<k>, closed from the text the model read, the answer's text — the
// restored todo list once, the install delta with the title and the commands,
// and the closing bracket, the last event before Start returns. Replayed is
// stamped on exactly the events between the brackets. The read and bash rows
// show their output and say they were replayed, with no exit code; the
// background agent row is a closed task showing its receipt; no sub-agent row
// is rebuilt. Nothing is published while s.mu, toolMu or rosterMu is held.
func TestNativeLoadReplaysTheTranscript(t *testing.T) {
	ws := nativeWorkspaceWith(t, map[string]string{"notes.txt": "a note\n"})
	rig := newWakeRig(t, Options{Workspace: ws}, nil)

	// Turn 1 starts a background child and answers with its receipt; the
	// child's result comes back by a wake, a turn of the session's own whose
	// user entry is a results entry.
	child := newHeld(t)
	childID := rig.spawnOne(child, answer("noted the result"))
	rig.finish(child, childID)
	rig.awaitDecided(true, "the result pending")
	rig.bracket(false, 1, "wake-1")

	// Turn 3: a shell context in front of the prompt, one step of three calls
	// held while a steer is taken, which the next step answers.
	h := newHeld(t)
	rig.a.route("go",
		h.step(cat(
			nativeCallParts("r1", "read", `{"filePath":"notes.txt"}`),
			nativeCallParts("b1", "bash", `{"command":"echo hi"}`),
			nativeCallParts("w1", "todo_write", nativeArgs(t, map[string]any{
				"todos": []any{map[string]any{"id": "1", "content": "alpha", "status": "in_progress"}},
			})),
		), finishParts(fantasy.FinishReasonToolCalls)),
		answer("all done"))
	block := ShellContextBlock([]ShellResult{{Command: "ls", Output: "notes.txt\n"}})
	out := startPrompt(rig.s, block+"next")
	await(t, h.reached, "the held tool step")
	if err := rig.s.Interject(context.Background(), "also check the tests"); err != nil {
		t.Fatalf("Interject: %v", err)
	}
	close(h.release)
	if got := await(t, out, "the third turn"); got.err != nil {
		t.Fatalf("the third turn: %v", got.err)
	}
	id := rig.s.Snapshot().SessionID
	if err := rig.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The transcript's two assistant entries that made calls name the
	// replayed ids.
	tr, err := store.Load(storedPath(t, rig.f, id))
	if err != nil {
		t.Fatal(err)
	}
	var made []string
	for _, e := range tr.Entries {
		if e.Type != store.TypeMessage || e.Message.Role != fantasy.MessageRoleAssistant {
			continue
		}
		if slices.ContainsFunc(e.Message.Content, func(p fantasy.MessagePart) bool {
			_, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p)
			return ok
		}) {
			made = append(made, e.ID)
		}
	}
	if len(made) != 2 {
		t.Fatalf("%d assistant entries made calls; want the agent call's and the tool step's", len(made))
	}
	a1, a2 := made[0]+".0", made[1]

	// The load: a plain session on the same home, nothing routed, nobody
	// reading the primary while it starts.
	rig.f.edit = nil
	dir := filepath.Join(t.TempDir(), "journal")
	s := rig.f.session(Options{Workspace: ws, LoadSessionID: id, Title: "resumed title", JournalDir: dir})
	locks := watchLocks(s)
	evs, err := startLoad(t, s)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	locks.check(t)

	equal := func(what string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Fatalf("%s:\n got %s\nwant %s", what, strings.Join(got, "\n     "), strings.Join(want, "\n     "))
		}
	}
	equal("the load", loadLines(evs), []string{
		"meta title=resumed title",
		"replay:start",
		"user: go [r]",
		"tool " + a1 + " pending [r]",
		"tool " + a1 + " in_progress [r]",
		"tool " + a1 + " completed [r]",
		"text: started [r]",
		"text: noted the result [r]",
		"user: next [r]",
		"tool " + a2 + ".0 pending [r]",
		"tool " + a2 + ".0 in_progress [r]",
		"tool " + a2 + ".1 pending [r]",
		"tool " + a2 + ".1 in_progress [r]",
		"tool " + a2 + ".2 pending [r]",
		"tool " + a2 + ".2 in_progress [r]",
		"tool " + a2 + ".0 completed [r]",
		"tool " + a2 + ".1 completed [r]",
		"tool " + a2 + ".2 completed [r]",
		"steer: also check the tests [r]",
		"text: all done [r]",
		"todos 1 [r]",
		"meta title=resumed title model=test/a mode=agent config commands=0 plugins [r]",
		"replay:end",
	})

	// The rows, as a consumer that drew them holds them.
	read := lastTool(t, evs, a2+".0")
	if o := read.Output; o == nil || !strings.HasSuffix(o.Content, "\n"+replayedLabel) || !strings.Contains(o.Content, "a note") || o.ExitCode != nil {
		t.Fatalf("the replayed read row is %+v (output %+v); want its content, then the label", read, read.Output)
	}
	if read.Kind != "read" || len(read.Locations) == 0 || !strings.HasSuffix(read.Locations[0], "notes.txt") {
		t.Fatalf("the replayed read row is %+v; want the file it read", read)
	}
	bash := lastTool(t, evs, a2+".1")
	if o := bash.Output; o == nil || o.Stdout != "hi\n\n"+replayedLabel || !strings.HasPrefix(o.StdoutHead, "hi\n") || o.ExitCode != nil {
		t.Fatalf("the replayed bash row's output is %+v; want the stream, then the label, previewing the stream, and no exit code", bash.Output)
	}
	if bash.Kind != "execute" || bash.RawInput != "echo hi" {
		t.Fatalf("the replayed bash row is %+v; want the command", bash)
	}
	agentRow := lastTool(t, evs, a1)
	if task := agentRow.Task; task == nil || !task.Receipt || task.Status != SubagentCompleted || task.Model != "" ||
		task.Description != "job" || task.Prompt != "child work" {
		t.Fatalf("the replayed agent row's task is %+v", agentRow.Task)
	}
	if o := agentRow.Output; o == nil || !strings.Contains(o.Content, "Started sub-agent "+childID) {
		t.Fatalf("the replayed agent row's output is %+v; want the launch receipt", agentRow.Output)
	}
	if n := len(ofType(evs, EventSubagent)); n != 0 {
		t.Fatalf("the load rebuilt %d sub-agent events; a load rebuilds none", n)
	}
	for _, ev := range ofType(evs, EventUser) {
		if strings.Contains(ev.Text, "shell_context") || strings.Contains(ev.Text, "subagent_result") {
			t.Fatalf("a replayed user row carries wire content: %q", ev.Text)
		}
	}

	// The session is the stored one, restored and ready.
	snap := s.Snapshot()
	if snap.SessionID != id || snap.Title != "resumed title" || snap.CurrentModel != "test/a" {
		t.Fatalf("snapshot session %q title %q model %q; want %s, the seeded title, test/a", snap.SessionID, snap.Title, snap.CurrentModel, id)
	}
	if len(snap.Todos) != 1 || snap.Todos[0].Content != "alpha" || snap.Todos[0].Status != "in_progress" {
		t.Fatalf("the restored todo list is %+v", snap.Todos)
	}
	if len(snap.Tools) != 4 {
		t.Fatalf("the snapshot holds %d rows; want the four replayed", len(snap.Tools))
	}
	for _, row := range snap.Tools {
		if toolStatusInFlight(row.Status) {
			t.Fatalf("row %s is still %s after the load", row.ID, row.Status)
		}
	}
	if s.replaying.Load() {
		t.Fatal("replaying is still up after the end bracket")
	}

	// The journal says which session this one was loaded from.
	w := journalOf(t, s.log)
	closeJournaled(t, s, w)
	var loaded []string
	for _, l := range fileLines(t, w) {
		if l["type"] == "session" {
			loaded = append(loaded, fmt.Sprint(l["providerSessionId"], " from ", l["loadedFrom"]))
		}
	}
	equal("the session notes", loaded, []string{id + " from " + id})
}

// TestNativeLoadReplaysPastThePrimarysBuffer: a transcript that replays more
// events than the primary holds loads with a reader draining it while Start
// runs — the requirement every load has (Session.Start) — and every event
// arrives, in order, inside the bracket.
func TestNativeLoadReplaysPastThePrimarysBuffer(t *testing.T) {
	f := newNativeFixture(t)
	ws := t.TempDir()
	turns := make([][]step, primaryCap/2+10)
	for i := range turns {
		turns[i] = []step{answer(fmt.Sprintf("answer %d", i+1))}
	}
	id := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", turns...)

	s := f.session(Options{Workspace: ws, LoadSessionID: id})
	w := newNativeWatcher(t, s)
	var err error
	within(t, "the load's Start", func() { err = s.Start(context.Background()) })
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	w.wait("the end bracket", func(ev Event) bool { return ev.Type == EventReplay && ev.Replay.Phase == ReplayEnd })
	w.pause()
	evs := w.events()
	var want []string
	want = append(want, "replay:start")
	for i := range turns {
		want = append(want, fmt.Sprintf("user: prompt %d [r]", i+1), fmt.Sprintf("text: answer %d [r]", i+1))
	}
	want = append(want, "meta title= model=test/a mode=agent config commands=0 plugins [r]", "replay:end")
	if got := loadLines(evs); !slices.Equal(got, want) {
		t.Fatalf("the long load published %d events, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
}

// TestNativeLoadOfACorruptTranscriptFails (A7; plan 028 §3.4, P23): a stored
// session whose transcript is damaged — a malformed line before its last —
// refuses the load with the store's error, having published the opening
// bracket and nothing after it (the seeded title's delta before it for a
// titled row); replaying is down again, so a later delta is not stamped; the
// session has no id; and no file is created, trimmed or rewritten.
func TestNativeLoadOfACorruptTranscriptFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
		want  []string
	}{
		{"untitled", "", []string{"replay:start"}},
		{"titled", "kept", []string{"meta title=kept", "replay:start"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t)
			ws := t.TempDir()
			id := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("one")}, []step{answer("two")})
			path := storedPath(t, f, id)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.SplitAfter(string(raw), "\n")
			lines[1] = "{not json\n"
			damaged := []byte(strings.Join(lines, ""))
			if err := os.WriteFile(path, damaged, 0o600); err != nil {
				t.Fatal(err)
			}
			before := dirListing(t, filepath.Dir(path))

			s := f.session(Options{Workspace: ws, LoadSessionID: id, Title: tc.title})
			evs, err := startLoad(t, s)
			if err == nil {
				t.Fatal("the load of a corrupt transcript succeeded")
			}
			if !errors.Is(err, store.ErrCorrupt) || !strings.Contains(err.Error(), `native: session "`+id+`" cannot be resumed`) {
				t.Fatalf("Start: %v; want the store's ErrCorrupt, naming the session", err)
			}
			if got := loadLines(evs); !slices.Equal(got, tc.want) {
				t.Fatalf("the failed load published %q, want %q", got, tc.want)
			}
			if s.replaying.Load() {
				t.Fatal("replaying is still up after the failed load")
			}
			if got := s.Snapshot().SessionID; got != "" {
				t.Fatalf("a failed load published session %q", got)
			}
			// Nothing after the failure is stamped replayed.
			if err := s.SetTitle("", "after"); err != nil {
				t.Fatal(err)
			}
			for _, ev := range deltaSettled(t, s) {
				if ev.Replayed {
					t.Fatalf("an event after the failed load is stamped replayed: %s", loadLine(ev))
				}
			}
			if after := dirListing(t, filepath.Dir(path)); !slices.Equal(after, before) {
				t.Fatalf("the failed load changed the session directory:\nbefore %q\nafter  %q", before, after)
			}
			if now, err := os.ReadFile(path); err != nil || !bytes.Equal(now, damaged) {
				t.Fatalf("the failed load changed the transcript (%v)", err)
			}
		})
	}
}

// TestNativeLoadOfAnEmptyTranscriptBracketsNothing (plan 028 §3.4): a stored
// session with no complete step — its only step torn, which the reopen trims —
// still loads with both brackets, the install delta between them and nothing
// else, on the table's default model.
func TestNativeLoadOfAnEmptyTranscriptBracketsNothing(t *testing.T) {
	f := newNativeFixture(t)
	ws := t.TempDir()
	id := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("one")})
	path := storedPath(t, f, id)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	header, rest, _ := bytes.Cut(raw, []byte("\n"))
	torn := append(append(header, '\n'), rest[:len(rest)/2]...)
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}

	s := f.session(Options{Workspace: ws, LoadSessionID: id})
	evs, err := startLoad(t, s)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := []string{"replay:start", "meta title= model=test/a mode=agent config commands=0 plugins [r]", "replay:end"}
	if got := loadLines(evs); !slices.Equal(got, want) {
		t.Fatalf("the empty load published %q, want %q", got, want)
	}
	if got := s.Snapshot().SessionID; got != id {
		t.Fatalf("the empty load is session %q, want %s", got, id)
	}
}

// TestAMissingTranscriptOpensEmptyUnderTheSameID (A11; plan 028 §3.5, P35,
// PD8): a load of a session with no transcript at all — the row its first
// prompt seeded, and no output ever — opens a new, empty session under the
// row's own id rather than refusing: the title seeded as a load's is, both
// brackets with the install delta between them and nothing replayed, a
// resume_empty note in the journal, and no file until the first output, which
// files the transcript under that id — so a second load resumes it, the same
// session going on. With no transcript to resume on, the model and the mode
// are a new session's: an explicit --model or --plan, else the funded default
// and agent. Only a missing file qualifies: a candidate the store cannot read,
// or a transcript another load holds, is still the load's error and creates
// nothing.
func TestAMissingTranscriptOpensEmptyUnderTheSameID(t *testing.T) {
	const id = "0b8f3c1e-5d2a-4c7e-9f10-2a3b4c5d6e7f"
	t.Run("opens empty, then files under the id", func(t *testing.T) {
		f := newNativeFixture(t)
		ws := t.TempDir()
		dir := filepath.Join(t.TempDir(), "journal")
		s := f.session(Options{Workspace: ws, LoadSessionID: id, Title: "the first prompt", JournalDir: dir})
		evs, err := startLoad(t, s)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		want := []string{
			"meta title=the first prompt",
			"replay:start",
			"meta title=the first prompt model=test/a mode=agent config commands=0 plugins [r]",
			"replay:end",
		}
		if got := loadLines(evs); !slices.Equal(got, want) {
			t.Fatalf("the empty open published %q, want %q", got, want)
		}
		if got := s.Snapshot().SessionID; got != id {
			t.Fatalf("the empty open is session %q, want the row's %s", got, id)
		}
		if got := f.transcripts(); len(got) != 0 {
			t.Fatalf("the empty open wrote %v before any output", got)
		}

		f.models["test/a"].push(answer("carried on"))
		if _, err := s.Prompt(context.Background(), "carry on"); err != nil {
			t.Fatalf("the first turn: %v", err)
		}
		if got := titleDeltas(deltaSettled(t, s)); len(got) != 0 {
			t.Fatalf("the first prompt renamed the seeded session: %q", got)
		}
		path := storedPath(t, f, id)
		if h, err := store.ReadHeader(path); err != nil || h.ID != id {
			t.Fatalf("the transcript's header is %+v (%v); want session %s", h, err, id)
		}
		w := journalOf(t, s.log)
		closeJournaled(t, s, w)
		if notes := diags(fileLines(t, w), diagResumeEmpty); len(notes) != 1 || notes[0]["session"] != id {
			t.Fatalf("the journal's resume_empty notes are %v; want one naming %s", notes, id)
		}

		again := f.session(Options{Workspace: ws, LoadSessionID: id})
		evs, err = startLoad(t, again)
		if err != nil {
			t.Fatalf("the second load: %v", err)
		}
		if got := loadLines(evs); !slices.Contains(got, "user: carry on [r]") || !slices.Contains(got, "text: carried on [r]") {
			t.Fatalf("the second load replayed %q; want the turn the empty open went on to", got)
		}
		if got := f.transcripts(); len(got) != 1 {
			t.Fatalf("the session has %d transcripts, want its one: %v", len(got), got)
		}
	})
	t.Run("a new session's model and mode", func(t *testing.T) {
		f := newNativeFixture(t)
		s := f.session(Options{LoadSessionID: id, Model: "other/c", Mode: "plan"})
		if _, err := startLoad(t, s); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if snap := s.Snapshot(); snap.SessionID != id || snap.CurrentModel != "other/c" || snap.CurrentMode != "plan" {
			t.Fatalf("opened %s on %s in %s; want %s on the explicit other/c in plan mode", snap.SessionID, snap.CurrentModel, snap.CurrentMode, id)
		}
	})
	t.Run("only a missing file", func(t *testing.T) {
		f := newNativeFixture(t)
		ws := t.TempDir()
		// A candidate named for the id whose first line is no header.
		sessions := filepath.Join(f.dir, "sessions", store.Slug(ws))
		if err := os.MkdirAll(sessions, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sessions, "20260918T120000_"+id+".jsonl"), []byte("not a header\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// And a stored session another load holds.
		held := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("one")})
		holder := f.session(Options{Workspace: ws, LoadSessionID: held})
		if _, err := startLoad(t, holder); err != nil {
			t.Fatalf("the holder's load: %v", err)
		}
		before := dirListing(t, sessions)
		for _, tc := range []struct {
			id   string
			want error
		}{{id, store.ErrNoHeader}, {held, store.ErrBusy}} {
			s := f.session(Options{Workspace: ws, LoadSessionID: tc.id})
			evs, err := startLoad(t, s)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), `native: session "`+tc.id+`" cannot be resumed`) {
				t.Fatalf("the load of %s: %v; want %v, naming the session", tc.id, err, tc.want)
			}
			if got := loadLines(evs); !slices.Equal(got, []string{"replay:start"}) {
				t.Fatalf("the refused load of %s published %q", tc.id, got)
			}
		}
		if after := dirListing(t, sessions); !slices.Equal(after, before) {
			t.Fatalf("a refused load changed the session directory:\nbefore %q\nafter  %q", before, after)
		}
	})
}

// TestNativeReplayedResultsDrawTheirText (plan 028 §3.4, P9): a replayed
// result is the text the model read, drawn as the row's body with the label
// after it — a command's stream, whose collapsed preview is the output's own
// first line as a live row's is, and a read's content — with no exit code,
// even for a command whose text records a non-zero one; an error stays an
// error; an empty result is the label alone; and an output longer than the
// row's cap keeps the text's tail and the label, and says it was cut — while
// a long command's collapsed preview is still its output's first line, the
// head taken from the whole stored text as a live row's is (astra r1-c3 F3).
func TestNativeReplayedResultsDrawTheirText(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	row := func(id string, kind tool.Kind, req harness.ToolRequest, res tool.Result) ToolEvent {
		t.Helper()
		req.Kind = kind
		s.sink(harness.ToolStarted{ID: id, Step: 1, Tool: req.Tool, Kind: kind})
		s.sink(harness.ToolCalled{ID: id, Request: req})
		s.sink(harness.ToolFinished{ID: id, Result: res, Replayed: true})
		return lastTool(t, drained(s), id)
	}

	failed := "boom\n\n<shell_metadata>\nexit code: 3\n</shell_metadata>"
	bash := row("e1.0", tool.KindExecute, harness.ToolRequest{Tool: "bash", Command: "exit 3"}, tool.Result{Text: failed, Content: failed})
	if bash.Status != toolCompleted || bash.Output == nil || bash.Output.ExitCode != nil ||
		bash.Output.Stdout != failed+"\n"+replayedLabel || bash.Output.Content != bash.Output.Stdout ||
		!strings.HasPrefix(bash.Output.StdoutHead, "boom\n") || bash.Output.Truncated {
		t.Fatalf("the replayed command row is %+v (output %+v)", bash, bash.Output)
	}

	missing := "File not found: /ws/nope.txt"
	read := row("e2.0", tool.KindRead, harness.ToolRequest{Tool: "read", Paths: []string{"/ws/nope.txt"}},
		tool.Result{Text: missing, Content: missing, IsError: true, Class: tool.ClassToolError})
	if read.Status != toolFailed || read.Output == nil || read.Output.Content != missing+"\n"+replayedLabel || read.Output.Stdout != "" {
		t.Fatalf("the replayed failed read row is %+v (output %+v)", read, read.Output)
	}

	quiet := row("e3.0", tool.KindExecute, harness.ToolRequest{Tool: "bash", Command: "true"}, tool.Result{})
	if o := quiet.Output; o == nil || o.Stdout != replayedLabel || o.Content != replayedLabel {
		t.Fatalf("the replayed empty command row's output is %+v; want the label alone", quiet.Output)
	}

	long := strings.Repeat("x", outputTailCap) + "the end"
	big := row("e4.0", tool.KindRead, harness.ToolRequest{Tool: "read", Paths: []string{"/ws/big.txt"}}, tool.Result{Text: long, Content: long})
	if o := big.Output; o == nil || !strings.HasSuffix(o.Content, "the end\n"+replayedLabel) || !strings.HasPrefix(o.Content, ellipsis+"xxx") ||
		len(o.Content) != outputTailCap || !o.Truncated {
		t.Fatalf("the replayed long read's content is %d bytes (truncated %v); want the tail, the label, the cap", len(big.Output.Content), big.Output.Truncated)
	}

	ran := "first line\n" + strings.Repeat("y", outputTailCap) + "\nlast line"
	tall := row("e5.0", tool.KindExecute, harness.ToolRequest{Tool: "bash", Command: "yes | head"}, tool.Result{Text: ran, Content: ran})
	o := tall.Output
	if o == nil {
		t.Fatal("the replayed long command's row has no output")
	}
	if !strings.HasPrefix(o.StdoutHead, "first line\n") || len(o.StdoutHead) > outputHeadCap ||
		!strings.HasSuffix(o.Stdout, "last line\n"+replayedLabel) || !strings.HasPrefix(o.Stdout, ellipsis+"yyy") ||
		o.Content != o.Stdout || !o.Truncated || o.ExitCode != nil {
		t.Fatalf("the replayed long command's row previews %.40q and ends %q (truncated %v); want its first line, the tail, the label",
			o.StdoutHead, o.Stdout[max(len(o.Stdout)-40, 0):], o.Truncated)
	}
}

// dirListing is every name in dir with its size.
func dirListing(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %d", de.Name(), info.Size()))
	}
	return out
}

// TestNativeLoadSeedsTitleAndPin (A8, the adapter's half; plan 028 §3.4): the
// index row's title is the session's from before the bracket opens, and the
// first prompt after the load does not rename it; a pinned row with no title
// is not named by that prompt either; an unpinned, untitled one is, as a new
// session is. (That /rename reaches the row is the index's, C4.)
func TestNativeLoadSeedsTitleAndPin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opts   Options
		first  []string // the load's events up to and including the opening bracket
		titles []string // the title deltas the first prompt publishes
		want   string   // the title after it
	}{
		{"titled", Options{Title: "kept title"}, []string{"meta title=kept title", "replay:start"}, nil, "kept title"},
		{"pinned, untitled", Options{TitlePinned: true}, []string{"replay:start"}, nil, ""},
		{"neither", Options{}, []string{"replay:start"}, []string{"rename me please"}, "rename me please"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t)
			ws := t.TempDir()
			id := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("one")})
			opts := tc.opts
			opts.Workspace, opts.LoadSessionID = ws, id
			s := f.session(opts)
			evs, err := startLoad(t, s)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			lines := loadLines(evs)
			if len(lines) < len(tc.first) || !slices.Equal(lines[:len(tc.first)], tc.first) {
				t.Fatalf("the load opened with %q, want %q", lines, tc.first)
			}
			if got := lines[len(lines)-2]; got != "meta title="+tc.opts.Title+" model=test/a mode=agent config commands=0 plugins [r]" {
				t.Fatalf("the install delta is %q; want the seeded title in it", got)
			}
			if got := s.Snapshot().Title; got != tc.opts.Title {
				t.Fatalf("the title after the load is %q, want %q", got, tc.opts.Title)
			}

			f.models["test/a"].push(answer("ok"))
			if _, err := s.Prompt(context.Background(), "rename me please\nand more"); err != nil {
				t.Fatal(err)
			}
			if got := titleDeltas(deltaSettled(t, s)); !slices.Equal(got, tc.titles) {
				t.Fatalf("the first prompt after the load published titles %q, want %q", got, tc.titles)
			}
			if got := s.Snapshot().Title; got != tc.want {
				t.Fatalf("the title after the first prompt is %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNativeLoadModelPrecedence (A5, the adapter's half; plan 028 §3.3, P8):
// through NewNative, a load leaves an unspecified model unspecified — the
// session resumes on the transcript's own model and effort, not on the
// default fundedModel would pick for a new session — while an explicit
// --model wins and one with no key refuses; a model the table no longer names
// falls back to the default with a resume warning on the diagnostics and in
// the journal; and with neither the transcript's model nor the default funded
// the load refuses rather than moving to another funded model. The mode is the
// transcript's unless --plan/--ask names one.
func TestNativeLoadModelPrecedence(t *testing.T) {
	// A session on test/b at effort high, in plan mode.
	stored := func(t *testing.T) (*nativeFixture, string, string) {
		f := newNativeFixture(t)
		ws := t.TempDir()
		s := f.started(Options{Workspace: ws, Model: "test/b"})
		if _, err := s.SetConfig(context.Background(), "", nativeEffortID, "high", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := setMode(s, "plan"); err != nil {
			t.Fatal(err)
		}
		f.models["test/b"].push(answer("planned"))
		if _, err := s.Prompt(context.Background(), "plan it"); err != nil {
			t.Fatal(err)
		}
		id := s.Snapshot().SessionID
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return f, ws, id
	}
	effortOf := func(snap Snapshot) string {
		if o := EffortOption(snap); o != nil {
			return o.Current
		}
		return ""
	}

	t.Run("unspecified is the transcript's", func(t *testing.T) {
		f, ws, id := stored(t)
		s := f.session(Options{Workspace: ws, LoadSessionID: id})
		if _, err := startLoad(t, s); err != nil {
			t.Fatalf("Start: %v", err)
		}
		snap := s.Snapshot()
		if snap.CurrentModel != "test/b" || effortOf(snap) != "high" || snap.CurrentMode != "plan" {
			t.Fatalf("resumed on %s at %q in %s; want the transcript's test/b, high, plan", snap.CurrentModel, effortOf(snap), snap.CurrentMode)
		}
	})
	t.Run("explicit wins", func(t *testing.T) {
		f, ws, id := stored(t)
		s := f.session(Options{Workspace: ws, LoadSessionID: id, Model: "Other/C", Mode: "agent"})
		if _, err := startLoad(t, s); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if snap := s.Snapshot(); snap.CurrentModel != "other/c" || snap.CurrentMode != "agent" {
			t.Fatalf("resumed on %s in %s; want the explicit other/c in agent mode", snap.CurrentModel, snap.CurrentMode)
		}
	})
	t.Run("an explicit model with no key refuses", func(t *testing.T) {
		f, ws, id := stored(t)
		s := f.session(Options{Workspace: ws, LoadSessionID: id, Model: "nokey/d"})
		evs, err := startLoad(t, s)
		if !errors.Is(err, harness.ErrNoAPIKey) || !strings.Contains(err.Error(), `model "nokey/d" has no API key`) {
			t.Fatalf("Start: %v; want the model's missing key", err)
		}
		if got := loadLines(evs); !slices.Equal(got, []string{"replay:start"}) {
			t.Fatalf("the refused load published %q", got)
		}
	})
	t.Run("a model the table lost falls back, warned", func(t *testing.T) {
		f, ws, id := stored(t)
		f.edit = func(o *harness.Options) {
			table := nativeTestTable("http://127.0.0.1:9/v1")
			delete(table.Models, "test/b")
			o.Table = table
		}
		var diag bytes.Buffer
		dir := filepath.Join(t.TempDir(), "journal")
		s := f.session(Options{Workspace: ws, LoadSessionID: id, Diag: &diag, JournalDir: dir})
		if _, err := startLoad(t, s); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if got := s.Snapshot().CurrentModel; got != "test/a" {
			t.Fatalf("resumed on %s; want the default, test/a", got)
		}
		if !strings.Contains(diag.String(), "native: resume: session "+id+"'s model test/b") || !strings.Contains(diag.String(), "continuing on test/a") {
			t.Fatalf("the diagnostics say %q; want the fall-back", diag.String())
		}
		w := journalOf(t, s.log)
		closeJournaled(t, s, w)
		notes := diags(fileLines(t, w), diagResumeWarning)
		if len(notes) != 1 || !strings.Contains(fmt.Sprint(notes[0]["text"]), "continuing on test/a") {
			t.Fatalf("the journal's resume warnings are %v; want the one fall-back", notes)
		}
	})
	t.Run("no funded fall-back to another model", func(t *testing.T) {
		f, ws, id := stored(t)
		delete(f.env, "NATIVE_TEST_KEY")
		// A new session would start on other/c, the first funded alias
		// (fundedModel); a load does not.
		s := f.session(Options{Workspace: ws, LoadSessionID: id})
		_, err := startLoad(t, s)
		if !errors.Is(err, harness.ErrResumeModel) || !strings.Contains(err.Error(), `native: session "`+id+`" cannot be resumed`) {
			t.Fatalf("Start: %v; want ErrResumeModel, naming the session", err)
		}
		for _, key := range []string{nativeCanary, nativeCanaryOther} {
			if leaks := nativeLeaks(err, key); len(leaks) > 0 {
				t.Fatalf("a key leaked into the refusal at %v", leaks)
			}
		}
	})
}

// TestNativeLoadRefusesAPromptUntilTheBracketCloses: a prompt that reaches a
// load's session before its Start has returned — the engine never sends one,
// a raw caller can — is refused as not started while the replay runs, so
// nothing of a turn's can land inside the bracket or race the replay for the
// harness; once Start has returned the session takes turns.
func TestNativeLoadRefusesAPromptUntilTheBracketCloses(t *testing.T) {
	f := newNativeFixture(t)
	ws := t.TempDir()
	id := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("one")})
	s := f.session(Options{Workspace: ws, LoadSessionID: id})
	var refused error
	var once sync.Once
	s.sinkSeam = func(ev harness.Event) {
		if _, ok := ev.(harness.Prompted); ok {
			once.Do(func() { _, refused = s.Prompt(context.Background(), "too early") })
		}
	}
	if _, err := startLoad(t, s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if refused == nil || refused.Error() != "agent: session not started" {
		t.Fatalf("a prompt during the replay returned %v; want the not-started refusal", refused)
	}
	f.models["test/a"].push(answer("two"))
	if _, err := s.Prompt(context.Background(), "in time"); err != nil {
		t.Fatalf("a prompt after the load: %v", err)
	}
}

// TestNativeLoadRefusesSettingsUntilTheBracketCloses (astra r1-c3 F1): nothing
// but the replay reaches a load's session between its brackets. A direct
// caller's SetModel, SetMode, SetConfig or SetTitle — the engine sends none
// before Started, a raw caller can — is refused as not started at each of the
// load's three windows: while the harness opens, while the replay walks, and
// while the install delta's flush waits (the drainer held on that batch until
// the setters have been tried, so a delta they enqueued could only have landed
// behind the end bracket, stamped replayed). The load publishes exactly what an
// undisturbed one does, and once Start has returned the setters take.
func TestNativeLoadRefusesSettingsUntilTheBracketCloses(t *testing.T) {
	f := newNativeFixture(t)
	ws := t.TempDir()
	id := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("one")})
	s := f.session(Options{Workspace: ws, LoadSessionID: id})

	var admitted []string
	try := func(where string) {
		for _, set := range []struct {
			name string
			call func() error
		}{
			{"SetModel", func() error { _, err := s.SetModel(context.Background(), "", "test/b"); return err }},
			{"SetMode", func() error { _, err := setMode(s, "plan"); return err }},
			{"SetConfig", func() error {
				_, err := s.SetConfig(context.Background(), "", nativeEffortID, "low", "")
				return err
			}},
			{"SetTitle", func() error { return s.SetTitle("", "renamed") }},
		} {
			if err := set.call(); err == nil || err.Error() != "agent: session not started" {
				admitted = append(admitted, fmt.Sprintf("%s during %s: %v", set.name, where, err))
			}
		}
	}
	var inOpen, inReplay, inFlush sync.Once
	f.edit = func(*harness.Options) { inOpen.Do(func() { try("the open") }) }
	s.sinkSeam = func(ev harness.Event) {
		if _, ok := ev.(harness.Prompted); ok {
			inReplay.Do(func() { try("the replay") })
		}
	}
	held := make(chan struct{})
	var first atomic.Bool
	s.log.hooks = &logHooks{
		// An untitled load enqueues one batch, the install delta.
		outboxAdmitting: func(int) {
			if first.CompareAndSwap(false, true) {
				<-held
			}
		},
		flushParked: func(uint64) {
			inFlush.Do(func() {
				try("the final flush")
				close(held)
			})
		},
	}

	evs, err := startLoad(t, s)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(admitted) != 0 {
		t.Fatalf("setters reached the load's session:\n%s", strings.Join(admitted, "\n"))
	}
	want := []string{
		"replay:start",
		"user: prompt 1 [r]",
		"text: one [r]",
		"meta title= model=test/a mode=agent config commands=0 plugins [r]",
		"replay:end",
	}
	if got := loadLines(evs); !slices.Equal(got, want) {
		t.Fatalf("the load published\n%q\nwant\n%q", got, want)
	}
	if snap := s.Snapshot(); snap.CurrentModel != "test/a" || snap.CurrentMode != "agent" || snap.Title != "" {
		t.Fatalf("after the load the session is on %s in %s titled %q; want it as the transcript left it", snap.CurrentModel, snap.CurrentMode, snap.Title)
	}
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatalf("SetModel after the load: %v", err)
	}
	if err := s.SetTitle("", "renamed"); err != nil {
		t.Fatalf("SetTitle after the load: %v", err)
	}
	for _, ev := range deltaSettled(t, s) {
		if ev.Replayed {
			t.Fatalf("a delta after the load is stamped replayed: %s", loadLine(ev))
		}
	}
}

// TestNativeLoadClosedDuringTheFinalFlush (astra r1-c3 F2; X19): a Close that
// reaches a load while the install delta's flush waits — the drainer held on
// that batch, as a stalled primary holds it, until Close cuts the outbox —
// makes Start return "session closed", with no end bracket, and Start returns
// only once nothing the load enqueued can still be delivered: the log commits
// nothing after it.
func TestNativeLoadClosedDuringTheFinalFlush(t *testing.T) {
	f := newNativeFixture(t)
	ws := t.TempDir()
	id := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("one")})
	s := f.session(Options{Workspace: ws, LoadSessionID: id})

	closed := make(chan struct{})
	var first atomic.Bool
	var once sync.Once
	s.log.hooks = &logHooks{
		outboxAdmitting: func(int) {
			if first.CompareAndSwap(false, true) {
				<-s.log.outboxCut
			}
		},
		flushParked: func(uint64) {
			once.Do(func() {
				go func() {
					_ = s.Close()
					close(closed)
				}()
				<-s.done
			})
		},
	}

	var err error
	within(t, "the load's Start", func() { err = s.Start(context.Background()) })
	head := s.log.committed.Load()
	within(t, "the Close", func() { <-closed })
	if err == nil || err.Error() != "agent: session closed" {
		t.Fatalf("Start: %v; want the session closed", err)
	}
	if after := s.log.committed.Load(); after != head {
		t.Fatalf("the log committed %d events after Start returned", after-head)
	}
	if got := loadLines(drained(s)); slices.Contains(got, "replay:end") {
		t.Fatalf("the closed load published %q; want no end bracket", got)
	}
	s.mu.Lock()
	loading := s.loading
	s.mu.Unlock()
	if loading || s.replaying.Load() {
		t.Fatalf("after the closed load loading=%v replaying=%v; want both down", loading, s.replaying.Load())
	}
}

// TestNativeLoadPanicLowersTheFlags (astra r1-c3 F2, the review's note): a
// panic out of the replay's sink propagates out of Start, and leaves neither
// the load's refusal nor its replayed stamp up behind it.
func TestNativeLoadPanicLowersTheFlags(t *testing.T) {
	f := newNativeFixture(t)
	ws := t.TempDir()
	id := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("one")})
	s := f.session(Options{Workspace: ws, LoadSessionID: id})
	s.sinkSeam = func(ev harness.Event) {
		if _, ok := ev.(harness.Prompted); ok {
			panic("the sink")
		}
	}
	var recovered any
	within(t, "the load's Start", func() {
		defer func() { recovered = recover() }()
		_ = s.Start(context.Background())
	})
	if recovered != "the sink" {
		t.Fatalf("Start recovered %v; want the sink's panic", recovered)
	}
	s.mu.Lock()
	loading := s.loading
	s.mu.Unlock()
	if loading || s.replaying.Load() {
		t.Fatalf("after the panic loading=%v replaying=%v; want both down", loading, s.replaying.Load())
	}
}

// TestNativeResumeWarningRedactedAfterSanitizing (astra r1-c3 F4; X20): a
// resume warning quotes the transcript's model, which comes off disk, so it
// takes the redact, sanitize, redact discipline before it is journaled or
// shown: a stored wire model that is the provider key split by a zero-width
// space passes the first redaction whole, and sanitizing it would put the key
// back together on the diagnostics.
func TestNativeResumeWarningRedactedAfterSanitizing(t *testing.T) {
	f := newNativeFixture(t)
	ws := t.TempDir()
	id := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("one")})
	path := storedPath(t, f, id)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The key with a zero-width space inside it: no alias in the table has
	// this wire model, so the load falls back to the default, warned.
	zwsp := string(rune(0x200b))
	split := nativeCanary[:len("sk-canary-")] + zwsp + nativeCanary[len("sk-canary-"):]
	edited := strings.ReplaceAll(string(raw), `"wire_model":"wire-a"`, `"wire_model":"`+split+`"`)
	if edited == string(raw) {
		t.Fatalf("the transcript names no wire model to replace:\n%s", raw)
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	var diag bytes.Buffer
	dir := filepath.Join(t.TempDir(), "journal")
	s := f.session(Options{Workspace: ws, LoadSessionID: id, Diag: &diag, JournalDir: dir})
	if _, err := startLoad(t, s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.Contains(diag.String(), "continuing on test/a") || !strings.Contains(diag.String(), redact.Marker) ||
		strings.Contains(diag.String(), nativeCanary) {
		t.Fatalf("the diagnostics say %q; want the fall-back with the key redacted", diag.String())
	}
	w := journalOf(t, s.log)
	closeJournaled(t, s, w)
	lines := fileLines(t, w)
	notes := diags(lines, diagResumeWarning)
	if len(notes) != 1 {
		t.Fatalf("%d resume_warning notes, want one", len(notes))
	}
	if text := fmt.Sprint(notes[0]["text"]); !strings.Contains(text, redact.Marker) || strings.Contains(text, zwsp) {
		t.Fatalf("the journaled warning is %q; want it sanitized and redacted", text)
	}
	for _, l := range lines {
		if strings.Contains(fmt.Sprint(l), nativeCanary) {
			t.Fatalf("the journal carries the key: %v", l)
		}
	}
}
