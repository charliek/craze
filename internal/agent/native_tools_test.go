package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/tool"
)

// The native adapter's tool rows (plan 019 §3.10). The turns here run the
// real tools in a temporary workspace, driven by the scripted model, so what
// is asserted is the whole path a row travels: the harness's four events,
// the adapter's merge, and the ToolEvent a transcript row or a `--json` tool
// object is drawn from.

// nativeCallParts streams one tool call the way the OpenAI-compatible
// provider does: the input's start, its arguments, its end, and the
// complete call (internal/harness's helper of the same shape).
func nativeCallParts(id, name, input string) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: name},
		{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: input},
		{Type: fantasy.StreamPartTypeToolInputEnd, ID: id},
		{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: name, ToolCallInput: input},
	}
}

// nativeCallStep is a step that calls one tool and finishes "tool-calls", so
// the turn runs it and goes on to the next step.
func nativeCallStep(id, name, input string) step {
	return reply(nativeCallParts(id, name, input), finishParts(fantasy.FinishReasonToolCalls))
}

// nativeArgs marshals a call's arguments.
func nativeArgs(t *testing.T, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// nativeWorkspaceWith is a workspace holding the named files.
func nativeWorkspaceWith(t *testing.T, files map[string]string) string {
	t.Helper()
	ws := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// rowsOf is the last row published for each call, in the order the calls
// were first seen: the state a consumer that drew every event would hold.
func rowsOf(evs []Event) []ToolEvent {
	var order []string
	byID := map[string]ToolEvent{}
	for _, ev := range evs {
		if ev.Type != EventTool || ev.Tool == nil {
			continue
		}
		if _, seen := byID[ev.Tool.ID]; !seen {
			order = append(order, ev.Tool.ID)
		}
		byID[ev.Tool.ID] = *ev.Tool
	}
	out := make([]ToolEvent, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// rowByID is the row for one call, or a failure naming what there was.
func rowByID(t *testing.T, rows []ToolEvent, id string) ToolEvent {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no row %q among %+v", id, rows)
	return ToolEvent{}
}

// requireNativeRG is internal/harness/tool/opencode's rule for a test that
// needs ripgrep: a laptop without it skips, and CI — which installs rg and
// sets CRAZE_REQUIRE_RG — fails instead of quietly passing (plan 019 §3.9).
func requireNativeRG(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		if os.Getenv("CRAZE_REQUIRE_RG") == "1" {
			t.Fatalf("CRAZE_REQUIRE_RG=1, and ripgrep (rg) is not on PATH: %v", err)
		}
		t.Skipf("ripgrep (rg) is not on PATH (set CRAZE_REQUIRE_RG=1 to fail instead): %v", err)
	}
}

// TestNativeToolRowsCarryEachKind runs one turn of four real calls and holds
// §3.10's field mapping: what a row says before it ran, and the one field
// each kind fills for the row that draws it — read the content, execute the
// stream and its exit code (and, failing, the head a collapsed row previews
// as stderr), edit the diff. The negative controls are in the same rows: a
// read has no diff and no exit code, an edit no output at all, a command
// that succeeded no stderr head.
func TestNativeToolRowsCarryEachKind(t *testing.T) {
	f := newNativeFixture(t)
	ws := nativeWorkspaceWith(t, map[string]string{
		"main.go":   "package main\n\nfunc main() {}\n",
		"notes.txt": "alpha\n",
	})
	f.models["test/a"].push(
		nativeCallStep("c1", "read", nativeArgs(t, map[string]any{"filePath": "main.go"})),
		nativeCallStep("c2", "edit", nativeArgs(t, map[string]any{
			"filePath": "notes.txt", "oldString": "alpha", "newString": "beta"})),
		nativeCallStep("c3", "bash", nativeArgs(t, map[string]any{"command": "echo hi"})),
		nativeCallStep("c4", "bash", nativeArgs(t, map[string]any{"command": "echo boom >&2; exit 3"})),
		answer("done"),
	)
	s := f.started(Options{Workspace: ws})

	res, err := s.Prompt(context.Background(), "go")
	if err != nil || res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want an end_turn", res, err)
	}
	evs := drained(s)
	_ = endings(t, evs, harness.StopEndTurn)
	rows := rowsOf(evs)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want one per call: %+v", len(rows), rows)
	}
	// The ids are the harness's own, "t<turn>.<step>.<n>", in call order.
	for i, want := range []string{"t1.1.1", "t1.2.1", "t1.3.1", "t1.4.1"} {
		if rows[i].ID != want {
			t.Fatalf("row %d is %q, want %q", i, rows[i].ID, want)
		}
	}
	// The snapshot holds the same rows, in the same order.
	snap := s.Snapshot().Tools
	if len(snap) != len(rows) {
		t.Fatalf("Snapshot.Tools has %d rows, the stream published %d", len(snap), len(rows))
	}
	for i := range snap {
		if snap[i].ID != rows[i].ID || snap[i].Status != rows[i].Status {
			t.Fatalf("Snapshot.Tools[%d] = %q/%q, the stream ended at %q/%q",
				i, snap[i].ID, snap[i].Status, rows[i].ID, rows[i].Status)
		}
	}

	read := rowByID(t, rows, "t1.1.1")
	switch {
	case read.Status != toolCompleted || read.Kind != string(tool.KindRead):
		t.Fatalf("read row: status %q kind %q", read.Status, read.Kind)
	case read.Name != "read" || read.ToolName != "read":
		t.Fatalf("read row: name %q tool name %q", read.Name, read.ToolName)
	case read.Title != "main.go":
		t.Fatalf("read row: title %q, want the workspace-relative path", read.Title)
	case len(read.Locations) != 1 || read.Locations[0] != filepath.Join(ws, "main.go"):
		t.Fatalf("read row: locations %q", read.Locations)
	case read.RawInput != `{"filePath":"main.go"}`:
		t.Fatalf("read row: raw input %q, want the arguments as compact JSON", read.RawInput)
	case read.Output == nil || !strings.Contains(read.Output.Content, "func main() {}"):
		t.Fatalf("read row: an expanded row draws Output.Content, got %+v", read.Output)
	case read.ContentText != "":
		t.Fatalf("read row: ContentText is the ACP path's field, got %q", read.ContentText)
	case len(read.Diffs) != 0 || read.Output.ExitCode != nil:
		t.Fatalf("read row: a read has no diff and no exit code: %+v", read)
	}

	edit := rowByID(t, rows, "t1.2.1")
	switch {
	case edit.Status != toolCompleted || edit.Kind != string(tool.KindEdit):
		t.Fatalf("edit row: status %q kind %q", edit.Status, edit.Kind)
	case len(edit.Diffs) != 1:
		t.Fatalf("edit row: %d diffs, want one per edited file: %+v", len(edit.Diffs), edit.Diffs)
	case edit.Diffs[0].Path != filepath.Join(ws, "notes.txt"):
		t.Fatalf("edit row: diff path %q", edit.Diffs[0].Path)
	case edit.Diffs[0].Added != 1 || edit.Diffs[0].Removed != 1 || edit.Diffs[0].Truncated:
		t.Fatalf("edit row: diff counts %+v, want +1 −1 untruncated", edit.Diffs[0])
	case edit.Diffs[0].OldText != "alpha\n" || edit.Diffs[0].NewText != "beta\n":
		t.Fatalf("edit row: diff texts %q → %q", edit.Diffs[0].OldText, edit.Diffs[0].NewText)
	case edit.Output != nil:
		t.Fatalf("edit row: an edit has no output of its own: %+v", edit.Output)
	}

	ok := rowByID(t, rows, "t1.3.1")
	switch {
	case ok.Status != toolCompleted || ok.Kind != string(tool.KindExecute):
		t.Fatalf("bash row: status %q kind %q", ok.Status, ok.Kind)
	case ok.RawInput != "echo hi":
		t.Fatalf("bash row: raw input %q, want the command itself", ok.RawInput)
	case ok.Title != "echo hi":
		t.Fatalf("bash row: title %q", ok.Title)
	case ok.Output == nil || ok.Output.ExitCode == nil || *ok.Output.ExitCode != 0:
		t.Fatalf("bash row: output %+v, want exit 0", ok.Output)
	case !strings.Contains(ok.Output.Stdout, "hi") || !strings.Contains(ok.Output.StdoutHead, "hi"):
		t.Fatalf("bash row: stream %q / head %q", ok.Output.Stdout, ok.Output.StdoutHead)
	case ok.Output.StderrHead != "":
		t.Fatalf("bash row: a command that succeeded has no stderr head: %q", ok.Output.StderrHead)
	}

	failed := rowByID(t, rows, "t1.4.1")
	switch {
	// A command that ran and exited non-zero is not a failed call: the
	// tool did its work, and the row says so with the exit code, exactly
	// as the ACP path's does (the bash-80x24 golden's "exit 127").
	case failed.Status != toolCompleted:
		t.Fatalf("failing bash row: status %q, want completed with an exit code", failed.Status)
	case failed.Output == nil || failed.Output.ExitCode == nil || *failed.Output.ExitCode != 3:
		t.Fatalf("failing bash row: output %+v, want exit 3", failed.Output)
	case !strings.Contains(failed.Output.StderrHead, "boom"):
		t.Fatalf("failing bash row: a collapsed row previews StderrHead, got %q", failed.Output.StderrHead)
	case failed.Output.StderrHead != failed.Output.StdoutHead:
		t.Fatalf("failing bash row: the stream is merged, so the two heads are one: %q vs %q",
			failed.Output.StderrHead, failed.Output.StdoutHead)
	}
}

// TestNativeSearchRowIsADrawableRow: grep is neither a read nor an edit nor
// a command, so its row is the transcript's "other" row — the kind is its
// label and the title its target — and its raw input is the JSON a search
// row looks in for a query the title does not already say.
func TestNativeSearchRowIsADrawableRow(t *testing.T) {
	requireNativeRG(t)
	f := newNativeFixture(t)
	ws := nativeWorkspaceWith(t, map[string]string{"main.go": "// TODO: later\n"})
	f.models["test/a"].push(
		nativeCallStep("c1", "grep", nativeArgs(t, map[string]any{"pattern": "TODO", "path": "."})),
		answer("done"),
	)
	s := f.started(Options{Workspace: ws})

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	rows := rowsOf(drained(s))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want one: %+v", len(rows), rows)
	}
	row := rows[0]
	switch {
	case row.Status != toolCompleted || row.Kind != string(tool.KindSearch):
		t.Fatalf("grep row: status %q kind %q", row.Status, row.Kind)
	case row.ToolName != "grep" || row.Title != "TODO in .":
		t.Fatalf("grep row: tool %q title %q", row.ToolName, row.Title)
	case row.RawInput != `{"path":".","pattern":"TODO"}`:
		t.Fatalf("grep row: raw input %q", row.RawInput)
	case len(row.Diffs) != 0:
		t.Fatalf("grep row: a search changes nothing, so it has no diff: %+v", row.Diffs)
	}
}

// TestNativeToolRowsAreTerminalAtEveryTurnEnding is §3.10's settling, from
// the consumer's side: however a turn ends, no row is left running — in the
// stream or in the snapshot. The max_tokens case is the live one the ACP
// path never meets: the model began a call, the response hit its ceiling,
// and the call's arguments never arrived.
func TestNativeToolRowsAreTerminalAtEveryTurnEnding(t *testing.T) {
	t.Run("max_tokens mid-call", func(t *testing.T) {
		f := newNativeFixture(t)
		// A call begun and cut off: the input's start, half its arguments,
		// and a "length" finish.
		f.models["test/a"].push(reply(
			[]fantasy.StreamPart{
				{Type: fantasy.StreamPartTypeToolInputStart, ID: "c1", ToolCallName: "read"},
				{Type: fantasy.StreamPartTypeToolInputDelta, ID: "c1", Delta: `{"filePa`},
			},
			finishParts(fantasy.FinishReasonLength),
		))
		s := f.started(Options{})
		res, err := s.Prompt(context.Background(), "go")
		if err != nil || res.StopReason != harness.StopMaxTokens {
			t.Fatalf("Prompt = %+v, %v; want a max_tokens turn", res, err)
		}
		evs := drained(s)
		_ = endings(t, evs, harness.StopMaxTokens)
		rows := rowsOf(evs)
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want the one begun call: %+v", len(rows), rows)
		}
		if rows[0].Status != toolFailed {
			t.Fatalf("a call the ceiling cut off ended %q, want failed", rows[0].Status)
		}
		// The reason reaches the row, so the card says why nothing ran.
		if rows[0].Output == nil || !strings.Contains(rows[0].Output.Content, "was not executed") {
			t.Fatalf("the row does not say why: %+v", rows[0].Output)
		}
		assertNoRunningRows(t, s, evs)
	})

	t.Run("a failed turn", func(t *testing.T) {
		f := newNativeFixture(t)
		f.models["test/a"].push(reply(
			[]fantasy.StreamPart{{Type: fantasy.StreamPartTypeToolInputStart, ID: "c1", ToolCallName: "read"}},
			errorParts(errors.New("the stream broke")),
		))
		s := f.started(Options{})
		if _, err := s.Prompt(context.Background(), "go"); err == nil {
			t.Fatal("Prompt returned no error for a broken stream")
		}
		evs := drained(s)
		if err := endings(t, evs, ""); err == nil {
			t.Fatal("a failed turn emitted no error")
		}
		assertNoRunningRows(t, s, evs)
	})

	t.Run("a cancel while a command runs", func(t *testing.T) {
		f := newNativeFixture(t)
		f.models["test/a"].push(
			nativeCallStep("c1", "bash", nativeArgs(t, map[string]any{"command": runningCommand})),
			answer("unreachable"),
		)
		s := f.started(Options{})
		out := startPrompt(s, "go")
		evs := awaitToolOutput(t, s, "ready")
		if err := s.Cancel(context.Background()); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		got := await(t, out, "the cancelled prompt")
		if got.err != nil || got.res.StopReason != harness.StopCancelled {
			t.Fatalf("Prompt = %+v, %v; want a cancelled turn", got.res, got.err)
		}
		evs = append(evs, drained(s)...)
		_ = endings(t, evs, harness.StopCancelled)
		assertNoRunningRows(t, s, evs)
	})

	t.Run("Close while a command runs", func(t *testing.T) {
		f := newNativeFixture(t)
		f.models["test/a"].push(
			nativeCallStep("c1", "bash", nativeArgs(t, map[string]any{"command": runningCommand})),
			answer("unreachable"),
		)
		s := f.started(Options{})
		out := startPrompt(s, "go")
		awaitToolOutput(t, s, "ready")
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// Close emits nothing — done is closed first — so the snapshot is
		// the only place the row can be read, and it must be settled.
		await(t, out, "the prompt Close cut short")
		for _, row := range s.Snapshot().Tools {
			if toolStatusInFlight(row.Status) {
				t.Fatalf("Close left row %q %q", row.ID, row.Status)
			}
		}
	})
}

// awaitToolOutput waits for a row to report output holding needle — which
// only a command that is really running can produce, since it arrives as a
// progress snapshot — and returns everything read on the way.
func awaitToolOutput(t *testing.T, s Session, needle string) []Event {
	t.Helper()
	var evs []Event
	for {
		ev := await(t, s.Events(), "a tool row reporting "+needle)
		evs = append(evs, ev)
		if ev.Type == EventTool && ev.Tool != nil && ev.Tool.Output != nil &&
			strings.Contains(ev.Tool.Output.Stdout, needle) {
			if ev.Tool.Status != toolInProgress {
				t.Fatalf("the row is %q while its command is still running", ev.Tool.Status)
			}
			return evs
		}
	}
}

// runningCommand keeps writing until it is stopped, so a test waiting for
// its output gets another chance at every snapshot: one dropped snapshot
// (progress is lossy) must not leave the test waiting for ever.
const runningCommand = "while :; do echo ready; sleep 1; done"

// assertNoRunningRows holds the settling contract from both sides: the last
// event published for each call is terminal, and so is every row in the
// snapshot.
func assertNoRunningRows(t *testing.T, s Session, evs []Event) {
	t.Helper()
	for _, row := range rowsOf(evs) {
		if toolStatusInFlight(row.Status) {
			t.Fatalf("the stream left row %q %q", row.ID, row.Status)
		}
	}
	for _, row := range s.Snapshot().Tools {
		if toolStatusInFlight(row.Status) {
			t.Fatalf("the snapshot left row %q %q", row.ID, row.Status)
		}
	}
}

// TestNativeSettleClosesOnlyRunningRows is settling itself: a row that never
// finished is cancelled and published, and one that did is left exactly as
// it was — the control, because a settle that overwrote a result would
// rewrite what the user already read.
func TestNativeSettleClosesOnlyRunningRows(t *testing.T) {
	s := newNative(Options{}, nil)
	closeAtCleanup(t, s)
	s.sink(harness.ToolStarted{ID: "t1.1.1", Step: 1, Tool: "read", Kind: tool.KindRead})
	s.sink(harness.ToolStarted{ID: "t1.1.2", Step: 1, Tool: "read", Kind: tool.KindRead})
	s.sink(harness.ToolFinished{ID: "t1.1.2", Result: tool.Result{Content: "hello"}})
	drained(s)

	s.settleTools()
	evs := drained(s)
	if len(evs) != 1 || evs[0].Tool == nil || evs[0].Tool.ID != "t1.1.1" {
		t.Fatalf("settling published %+v, want the running row alone", evs)
	}
	if evs[0].Tool.Status != toolCancelled {
		t.Fatalf("the settled row is %q, want %q", evs[0].Tool.Status, toolCancelled)
	}
	rows := s.Snapshot().Tools
	if len(rows) != 2 || rows[0].Status != toolCancelled || rows[1].Status != toolCompleted {
		t.Fatalf("after settling: %+v", rows)
	}
	// A second settle finds nothing: the rows are terminal now.
	s.settleTools()
	if evs := drained(s); len(evs) != 0 {
		t.Fatalf("settling twice published %+v", evs)
	}
}

// TestNativeToolProgressIsDroppedNotBlocked is the harness's contract for a
// sink that may block (plan 019 §3.5): a progress snapshot the consumer has
// no room for is dropped, not queued and not waited on. The control is the
// same snapshot with one slot free, which is delivered.
func TestNativeToolProgressIsDroppedNotBlocked(t *testing.T) {
	s := newNative(Options{}, nil)
	closeAtCleanup(t, s)
	s.sink(harness.ToolStarted{ID: "t1.1.1", Step: 1, Tool: "bash", Kind: tool.KindExecute})
	drained(s)
	// Filled through the session's own emit: the primary is the event log's
	// now, and the field is receive-only precisely so nothing can put an
	// event into it without a sequence number (plan 020 §3.1). Each of these
	// has room by the loop's own condition, so none of them blocks.
	for len(s.events) < cap(s.events) {
		s.emit(Event{Type: EventText, Text: "filler"})
	}

	// It must return: a test that hangs here has found the bug.
	s.sink(harness.ToolProgress{ID: "t1.1.1", Output: "one"})
	if len(s.events) != cap(s.events) {
		t.Fatalf("a dropped snapshot still took a slot: %d of %d", len(s.events), cap(s.events))
	}
	// The row took it even though nobody was told.
	if rows := s.Snapshot().Tools; len(rows) != 1 || rows[0].Output == nil || rows[0].Output.Stdout != "one" {
		t.Fatalf("the merge did not happen: %+v", rows)
	}

	<-s.events
	s.sink(harness.ToolProgress{ID: "t1.1.1", Output: "two"})
	if len(s.events) != cap(s.events) {
		t.Fatalf("the control snapshot was dropped too: %d of %d", len(s.events), cap(s.events))
	}
	var last *ToolEvent
	for _, ev := range drained(s) {
		if ev.Type == EventTool {
			last = ev.Tool
		}
	}
	if last == nil || last.Output == nil || last.Output.Stdout != "two" {
		t.Fatalf("the control snapshot never arrived: %+v", last)
	}
}

// TestNativeToolRowsArePublishedAsDeepCopies: the row a consumer is handed
// is its own, because it is read on another goroutine while the next event
// merges into the stored one (coordination point 3). Writing to what was
// published must not reach the session, and two events for one call must
// not share a slice or a pointer.
func TestNativeToolRowsArePublishedAsDeepCopies(t *testing.T) {
	s := newNative(Options{}, nil)
	closeAtCleanup(t, s)
	s.sink(harness.ToolStarted{ID: "t1.1.1", Step: 1, Tool: "edit", Kind: tool.KindEdit})
	s.sink(harness.ToolCalled{ID: "t1.1.1", CallID: "c1", Request: harness.ToolRequest{
		Tool: "edit", Kind: tool.KindEdit, Title: "notes.txt", Paths: []string{"/ws/notes.txt"},
		Input: `{"filePath":"notes.txt"}`}})
	s.sink(harness.ToolFinished{ID: "t1.1.1", Result: tool.Result{
		Edits:  []tool.FileEdit{{Path: "/ws/notes.txt", Old: "alpha\n", New: "beta\n"}},
		Output: &tool.ExecOutput{ExitCode: 0, Output: "out"},
	}})
	evs := ofType(drained(s), EventTool)
	if len(evs) < 2 {
		t.Fatalf("got %d tool events, want one per harness event", len(evs))
	}
	first, last := evs[0].Tool, evs[len(evs)-1].Tool
	if first == last {
		t.Fatal("two events share one ToolEvent")
	}
	last.Locations[0] = "/tampered"
	last.Diffs[0].Path = "/tampered"
	*last.Output.ExitCode = 99
	last.Title = "tampered"

	rows := s.Snapshot().Tools
	switch {
	case len(rows) != 1:
		t.Fatalf("got %d rows, want one: %+v", len(rows), rows)
	case rows[0].Locations[0] != "/ws/notes.txt" || rows[0].Diffs[0].Path != "/ws/notes.txt":
		t.Fatalf("a consumer's write reached the session: %+v", rows[0])
	case rows[0].Output.ExitCode == nil || *rows[0].Output.ExitCode != 0:
		t.Fatalf("a consumer's write reached the session's exit code: %+v", rows[0].Output)
	case rows[0].Title != "notes.txt":
		t.Fatalf("a consumer's write reached the session's title: %q", rows[0].Title)
	}
}

// TestNativeToolRowsAreCappedAndSanitized: the caps are mergeToolLocked's,
// and everything a tool reports is text from a file, a command or a model,
// so nothing reaches a terminal unsanitized. The controls are the same
// fields within their caps, which are kept whole.
func TestNativeToolRowsAreCappedAndSanitized(t *testing.T) {
	t.Run("caps", func(t *testing.T) {
		s := newNative(Options{}, nil)
		closeAtCleanup(t, s)
		long := strings.Repeat("x", 700)
		big := strings.Repeat("y", 9*1024)
		huge := strings.Repeat("z", 70*1024)
		s.sink(harness.ToolStarted{ID: "t1.1.1", Step: 1, Tool: "bash", Kind: tool.KindExecute})
		s.sink(harness.ToolCalled{ID: "t1.1.1", CallID: "c1", Request: harness.ToolRequest{
			Tool: "bash", Kind: tool.KindExecute, Command: long, Input: `{"command":"` + long + `"}`}})
		s.sink(harness.ToolProgress{ID: "t1.1.1", Output: big})
		s.sink(harness.ToolStarted{ID: "t1.1.2", Step: 1, Tool: "edit", Kind: tool.KindEdit})
		s.sink(harness.ToolFinished{ID: "t1.1.2", Result: tool.Result{
			Edits: []tool.FileEdit{{Path: "/ws/big.txt", Old: huge, New: huge + "!"}}}})
		drained(s)

		rows := s.Snapshot().Tools
		cmd := rowByID(t, rows, "t1.1.1")
		switch {
		case len(cmd.RawInput) > rawInputCap || !strings.HasSuffix(cmd.RawInput, ellipsis):
			t.Fatalf("raw input is %d bytes: %q", len(cmd.RawInput), cmd.RawInput)
		case len(cmd.Output.Stdout) > outputTailCap || !strings.HasPrefix(cmd.Output.Stdout, ellipsis):
			t.Fatalf("the stream keeps the tail: %d bytes", len(cmd.Output.Stdout))
		case len(cmd.Output.StdoutHead) > outputHeadCap || !strings.HasSuffix(cmd.Output.StdoutHead, ellipsis):
			t.Fatalf("the head is %d bytes", len(cmd.Output.StdoutHead))
		case !cmd.Output.Truncated:
			t.Fatalf("a capped stream must say so: %+v", cmd.Output)
		}
		diff := rowByID(t, rows, "t1.1.2").Diffs[0]
		switch {
		case len(diff.OldText) > diffTextCap || len(diff.NewText) > diffTextCap:
			t.Fatalf("diff texts are %d and %d bytes", len(diff.OldText), len(diff.NewText))
		case !diff.Truncated:
			t.Fatalf("a capped diff must say so: %+v", diff)
		case diff.Added != 1 || diff.Removed != 1:
			// The counts are taken on the whole texts, before the cap: two
			// texts capped to the same 64 KiB would otherwise count as
			// identical.
			t.Fatalf("diff counts %+v, want +1 −1 from the uncapped texts", diff)
		}
	})

	t.Run("control: within the caps, nothing is cut", func(t *testing.T) {
		s := newNative(Options{}, nil)
		closeAtCleanup(t, s)
		s.sink(harness.ToolStarted{ID: "t1.1.1", Step: 1, Tool: "bash", Kind: tool.KindExecute})
		s.sink(harness.ToolCalled{ID: "t1.1.1", CallID: "c1", Request: harness.ToolRequest{
			Tool: "bash", Kind: tool.KindExecute, Command: "echo hi", Input: `{"command":"echo hi"}`}})
		s.sink(harness.ToolProgress{ID: "t1.1.1", Output: "hi\n"})
		drained(s)
		row := s.Snapshot().Tools[0]
		if row.RawInput != "echo hi" || row.Output.Stdout != "hi\n" || row.Output.Truncated {
			t.Fatalf("a small call was cut: %+v", row)
		}
	})

	t.Run("sanitized", func(t *testing.T) {
		s := newNative(Options{}, nil)
		closeAtCleanup(t, s)
		esc := "\x1b[31mred\x1b[0m" + zeroWidthSpace
		s.sink(harness.ToolStarted{ID: "t1.1.1", Step: 1, Tool: "read" + esc, Kind: tool.KindRead})
		s.sink(harness.ToolCalled{ID: "t1.1.1", CallID: "c1", Request: harness.ToolRequest{
			Tool: "read", Kind: tool.KindRead, Title: "note" + esc, Paths: []string{"/ws/note" + esc},
			Input: `{"filePath":"note` + esc + `"}`}})
		s.sink(harness.ToolFinished{ID: "t1.1.1", Result: tool.Result{Content: "body" + esc}})
		drained(s)
		row := s.Snapshot().Tools[0]
		for what, got := range map[string]string{
			"name":     row.Name,
			"title":    row.Title,
			"location": row.Locations[0],
			"input":    row.RawInput,
			"content":  row.Output.Content,
		} {
			if strings.ContainsAny(got, "\x1b") || strings.Contains(got, zeroWidthSpace) {
				t.Fatalf("the %s still holds an escape: %q", what, got)
			}
		}
		if row.Title != "notered" || row.Output.Content != "bodyred" {
			t.Fatalf("sanitizing took more than the escapes: %q, %q", row.Title, row.Output.Content)
		}
	})
}

// TestNativeSnapshotHasNoToolsBeforeACall is the control for Snapshot.Tools:
// a session that has run nothing reports no rows at all, so a row in the
// transcript can only have come from a call.
func TestNativeSnapshotHasNoToolsBeforeACall(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	if rows := s.Snapshot().Tools; len(rows) != 0 {
		t.Fatalf("a session with no turn reports %+v", rows)
	}
	f.models["test/a"].push(answer("hi"))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if rows := s.Snapshot().Tools; len(rows) != 0 {
		t.Fatalf("a turn that called nothing reports %+v", rows)
	}
	if evs := ofType(drained(s), EventTool); len(evs) != 0 {
		t.Fatalf("a turn that called nothing published %+v", evs)
	}
}
