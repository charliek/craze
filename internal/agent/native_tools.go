package agent

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/tool"
)

// The native adapter's tool rows (plan 019 §3.10). The harness reports one
// call as four events — started, called, progress, finished — and everything
// that draws a call reads one merged agent.ToolEvent: the transcript's rows
// (internal/tui/transcript.go) and `craze prompt --json`'s tool objects
// (internal/cli/events.go's toolJSON). So the adapter keeps its own merge
// here, with the ACP path's caps and its clone-before-publish rule
// (tools.go's mergeToolLocked): nothing in internal/tui changes, and no
// field is added to agent.Event or ToolEvent.
//
// Every row is published through the session's one emit choke point, and the
// merge lock is never held across it. The row handed to a consumer is a deep
// copy taken under that lock, because a consumer encodes it on a goroutine
// of its own while the next event is already merging into the stored row
// (coordination point 3, 019-native-harness-h2-tools/coordination-s1a.md).

// The statuses a row carries. They are the ACP words for the same states,
// which is what the transcript's glyphs switch on (transcript.go's
// statusGlyph) and what toolStatusInFlight counts as running.
const (
	toolPending    = "pending"
	toolInProgress = "in_progress"
	toolCompleted  = "completed"
	toolFailed     = "failed"
	// toolCancelled is what settling leaves on a row whose call never
	// reported how it ended (settleTools).
	toolCancelled = "cancelled"
)

// toolStarted opens the call's row: the model has begun it and its arguments
// are still streaming, so all that is known is which tool it is. Name is the
// tool's own, not a title — the row has to say something before the
// arguments arrive, and ToolCalled brings the title.
func (s *nativeSession) toolStarted(e harness.ToolStarted) {
	s.publishTool(e.ID, func(t *ToolEvent) {
		t.Status = toolPending
		t.Kind = string(e.Kind)
		t.ToolName = sanitizeText(e.Tool)
		t.Name = t.ToolName
	})
}

// toolCalled fills the row from the prepared call: the title a row shows,
// the paths it touches — which is how a read or edit row finds its file —
// and the raw input a command or a search query is drawn from.
func (s *nativeSession) toolCalled(e harness.ToolCalled) {
	s.publishTool(e.ID, func(t *ToolEvent) {
		t.Status = toolInProgress
		t.Title = sanitizeText(e.Request.Title)
		t.Locations = nativeLocations(e.Request.Paths)
		t.RawInput = nativeRawInput(e.Request)
		// The harness opens every call with a ToolStarted, so these are
		// already set; a row that somehow missed one would otherwise draw
		// as the kindless "tool" for the rest of its life.
		if t.Kind == "" {
			t.Kind = string(e.Request.Kind)
		}
		if t.ToolName == "" {
			t.ToolName = sanitizeText(e.Request.Tool)
			t.Name = t.ToolName
		}
	})
}

// toolProgress keeps a running call's output: the whole of what it has
// produced so far, not a delta, as the tail an expanded row draws and the
// head a collapsed one previews. It is published without blocking —
// progress is lossy by contract and the harness drops a snapshot rather
// than wait on this sink (plan 019 §3.5).
func (s *nativeSession) toolProgress(e harness.ToolProgress) {
	s.publishToolLossy(e.ID, func(t *ToolEvent) {
		out := ToolOutput{}
		if t.Output != nil {
			out = *t.Output
		}
		setStdout(&out, e.Output)
		t.Output = &out
	})
}

// toolFinished closes the row with the call's result. Each kind fills the
// one field its row draws (plan 019 §3.10): read the content an expanded
// row shows, execute the merged stream and the exit code a failed row
// previews as stderr, edit and write the diff — computed here, from the
// texts the tool held, and nowhere else.
func (s *nativeSession) toolFinished(e harness.ToolFinished) {
	s.publishTool(e.ID, func(t *ToolEvent) {
		res := e.Result
		t.Status = toolCompleted
		if res.IsError {
			t.Status = toolFailed
		}
		if len(res.Edits) > 0 {
			t.Diffs = nativeDiffs(res.Edits)
		}
		out := ToolOutput{}
		if t.Output != nil {
			out = *t.Output // what progress kept, for a call that reported some
		}
		if o := res.Output; o != nil {
			setStdout(&out, o.Output)
			code := o.ExitCode
			out.ExitCode = &code
		}
		if res.Content != "" {
			setContent(&out, res.Content)
		}
		// A failed call with nothing of its own to show — a refusal, a call
		// the turn aborted, a path that was not there — shows the text the
		// model was given, which is the only thing that says why. An
		// expanded row draws Content; a command's row draws its stream.
		if res.IsError {
			if out.Content == "" {
				setContent(&out, res.Text)
			}
			if out.Stdout == "" && t.Kind == string(tool.KindExecute) {
				setStdout(&out, res.Text)
			}
		}
		if out.ExitCode != nil && *out.ExitCode != 0 {
			// A collapsed failed command row previews StderrHead
			// (transcript.go:931-954), and what the harness keeps is the
			// command's stdout and stderr merged, in order.
			out.StderrHead = out.StdoutHead
		}
		if out != (ToolOutput{}) {
			t.Output = &out
		}
	})
}

// settleTools closes every row the turn left running, whichever way it
// ended — done with any stop reason, error, cancel, Close. It is the native
// equivalent of the live session's closeInFlightTools (live.go:909-943),
// and it exists for the same reason: a row nobody ever finishes spins for
// ever and the status row keeps counting it. The harness settles its own
// calls (toolbridge.go's settleCalls), so this usually finds nothing; what
// it stands against is a call whose ending never reached the sink at all —
// the common live case being a ToolStarted whose arguments the response cut
// off, which the ACP path never meets.
func (s *nativeSession) settleTools() {
	s.toolMu.Lock()
	ids := append([]string(nil), s.toolOrder...)
	s.toolMu.Unlock()
	for _, id := range ids {
		// The in-flight test happens under the merge lock: a terminal
		// update that lands between the scan and the merge wins.
		if t, changed := s.mergeTool(id, true, func(t *ToolEvent) { t.Status = toolCancelled }); changed {
			s.emit(Event{Type: EventTool, Tool: &t})
		}
	}
}

// toolRows is every row the session has, in call order, for Snapshot.
func (s *nativeSession) toolRows() []ToolEvent {
	s.toolMu.Lock()
	defer s.toolMu.Unlock()
	return snapshotTools(s.toolOrder, s.tools)
}

// publishTool merges one event into its row and publishes the row when
// something a consumer can see changed.
func (s *nativeSession) publishTool(id string, apply func(*ToolEvent)) {
	if t, changed := s.mergeTool(id, false, apply); changed {
		s.emit(Event{Type: EventTool, Tool: &t})
	}
}

// publishToolLossy is publishTool for progress: the send gives up rather
// than wait for a consumer that is not draining (plan 019 §3.5). The next
// snapshot carries the whole output again, so a drop loses nothing but a
// frame. (S1a replaces the select-default with EventLog.TryPublish;
// coordination point 4.)
func (s *nativeSession) publishToolLossy(id string, apply func(*ToolEvent)) {
	if t, changed := s.mergeTool(id, false, apply); changed {
		s.emitLossy(Event{Type: EventTool, Tool: &t})
	}
}

// mergeTool applies apply to row id — opening it the first time — and
// returns the row and whether it changed. onlyIfInFlight drops the change
// when the row has already settled, so a terminal update that raced in is
// not overwritten (mergeToolLocked's own rule).
//
// The returned row is a deep copy taken under the merge lock: the caller
// hands it to a consumer that reads it on another goroutine, while the
// stored row goes on merging.
func (s *nativeSession) mergeTool(id string, onlyIfInFlight bool, apply func(*ToolEvent)) (ToolEvent, bool) {
	s.toolMu.Lock()
	defer s.toolMu.Unlock()
	prev, exists := s.tools[id]
	if onlyIfInFlight && (!exists || !toolStatusInFlight(prev.Status)) {
		return ToolEvent{}, false
	}
	out := prev
	if !exists {
		if s.tools == nil {
			s.tools = map[string]ToolEvent{}
		}
		out.ID = id
		s.toolOrder = append(s.toolOrder, id)
	}
	apply(&out)
	out.At = time.Now()
	// The fields a consumer draws; a repeated progress snapshot changes
	// none of them and is not published twice.
	changed := !exists ||
		out.Status != prev.Status ||
		out.Title != prev.Title ||
		out.Kind != prev.Kind ||
		out.RawInput != prev.RawInput ||
		!sameOutput(prev.Output, out.Output) ||
		!sameDiffs(prev.Diffs, out.Diffs)
	s.tools[id] = out
	return cloneTool(out), changed
}

// nativeLocations are the paths a call touches, as the row's Locations: the
// first is the file a read or edit row names (transcript.go's toolPath).
func nativeLocations(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, 0, min(len(paths), locationsCap))
	for _, p := range paths {
		if p == "" {
			continue
		}
		out = append(out, sanitizeText(p))
		if len(out) >= locationsCap {
			break
		}
	}
	return out
}

// nativeRawInput is what a row draws the call's arguments from: the command
// for an execute call, which its row prints as the command
// (transcript.go:1074-1079), and the arguments as compact JSON for every
// other kind, which is where a search row looks for a query it has not
// already shown. Capped as mergeToolLocked caps a raw input.
func nativeRawInput(req harness.ToolRequest) string {
	if req.Kind == tool.KindExecute && req.Command != "" {
		return truncateUTF8(sanitizeText(req.Command), rawInputCap)
	}
	return truncateUTF8(sanitizeText(compactJSON(req.Input)), rawInputCap)
}

// compactJSON is s without the whitespace between its tokens, and s itself
// when it is not valid JSON — a model's arguments need not be.
func compactJSON(s string) string {
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}

// nativeDiffs are the row's diffs for what an edit or a write changed. The
// lines are counted on the whole texts and only then capped (diffOf), and
// no more diffs are kept than the ACP path keeps.
func nativeDiffs(edits []tool.FileEdit) []ToolDiff {
	out := make([]ToolDiff, 0, min(len(edits), diffsCap))
	for _, e := range edits {
		out = append(out, diffOf(e.Path, e.Old, e.New))
		if len(out) >= diffsCap {
			break
		}
	}
	return out
}

// setStdout keeps text as a row's stream: the 8 KiB tail an expanded row
// draws and the 512 B head a collapsed one previews.
func setStdout(out *ToolOutput, text string) {
	s := sanitizeText(text)
	out.Stdout = tailUTF8(s, outputTailCap)
	out.StdoutHead = truncateUTF8(s, outputHeadCap)
	out.Truncated = out.Truncated || len(s) > outputTailCap
}

// setContent keeps text as a row's content: what an expanded read draws
// (transcript.go:850-856), never ContentText.
func setContent(out *ToolOutput, text string) {
	s := sanitizeText(text)
	out.Content = tailUTF8(s, outputTailCap)
	out.Truncated = out.Truncated || len(s) > outputTailCap
}
