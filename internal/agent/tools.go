package agent

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/textdiff"
)

const (
	rawInputCap    = 512
	contentTextCap = 2048
	locationsCap   = 8
	outputTailCap  = 8 * 1024
	outputHeadCap  = 512
	diffTextCap    = 64 * 1024
	taskPromptCap  = 4 * 1024
	diffsCap       = 8
	ellipsis       = "…"
)

// subagentTitleRe is the 003 fallback classifier, kept so titles cursor sends
// without a _toolName still register as sub-agents.
var subagentTitleRe = regexp.MustCompile(`(?i)\bsubagent\b|\btask\b`)

// IsTask reports whether the tool call is a sub-agent. Classification stamps
// Task at merge; this is the compatibility query the TUI still reads.
func (t ToolEvent) IsTask() bool {
	return t.Task != nil
}

// IsTodoTool reports whether the tool call is cursor's todo writer, which the
// transcript hides in favour of the cursor/update_todos stream.
func (t ToolEvent) IsTodoTool() bool {
	return t.ToolName == "updateTodos" || strings.HasPrefix(t.Title, "Update TODOs")
}

type toolDelta struct {
	id string
	// onlyIfInFlight drops the delta when the tool has already settled, so a
	// terminal update that raced in is not overwritten (closeInFlightTools).
	onlyIfInFlight bool
	title          *string
	kind           *string
	status         *string
	rawInput       json.RawMessage
	hasRawInput    bool
	content        json.RawMessage
	hasContent     bool
	rawOutput      json.RawMessage
	hasRawOutput   bool
	locations      json.RawMessage
	hasLocations   bool
	// wireName is SessionNotification.ToolName (grok _meta); rawInput._toolName wins.
	wireName string
}

func (s *session) mergeTool(d toolDelta) (ToolEvent, bool) {
	tool, changed, extras := s.applyToolDelta("", d)
	s.emitAll(extras)
	return tool, changed
}

func (s *session) applyToolDelta(owner string, d toolDelta) (ToolEvent, bool, []Event) {
	s.mu.Lock()
	tool, changed, extras := s.mergeToolLocked(owner, d)
	s.mu.Unlock()
	return tool, changed, extras
}

func (s *session) mergeToolLocked(owner string, d toolDelta) (ToolEvent, bool, []Event) {
	tools, order, rec, capN := s.toolStoreLocked(owner)
	if tools == nil {
		return ToolEvent{}, false, nil
	}
	if rec.isEvicted(d.id) {
		return ToolEvent{}, false, nil
	}
	prev, exists := tools[d.id]
	if d.onlyIfInFlight && (!exists || !toolStatusInFlight(prev.Status)) {
		return cloneTool(prev), false, nil
	}
	out := prev
	if !exists {
		if capN > 0 && len(*order) >= capN {
			s.evictChildToolLocked(tools, order, rec)
		}
		out.ID = d.id
		*order = append(*order, d.id)
	}
	if d.title != nil {
		title := sanitizeText(*d.title)
		out.Title = title
		out.Name = title
	}
	if d.kind != nil {
		out.Kind = sanitizeText(*d.kind)
	}
	if d.status != nil {
		out.Status = sanitizeText(*d.status)
	}
	if d.hasRawInput {
		out.RawInput = stringifyRawInput(d.rawInput)
		if name := toolNameOf(d.rawInput); name != "" {
			out.ToolName = name
		}
		if task := taskFromRawInput(d.rawInput, out.ToolName); task != nil {
			out.Task = mergeTaskInfo(out.Task, *task)
		}
	}
	if out.ToolName == "" && d.wireName != "" {
		out.ToolName = sanitizeText(d.wireName)
		if d.hasRawInput {
			if task := taskFromRawInput(d.rawInput, out.ToolName); task != nil {
				out.Task = mergeTaskInfo(out.Task, *task)
			}
		}
	}
	if s.classifyTask(out) {
		if out.Task == nil {
			out.Task = &TaskInfo{}
		}
		if out.Task.Description == "" {
			out.Task.Description = strings.TrimPrefix(out.Title, "Task: ")
		}
	}
	if d.hasContent {
		out.ContentText = contentText(d.content)
		if diffs := contentDiffs(d.content); len(diffs) > 0 {
			out.Diffs = diffs
		}
	}
	if d.hasRawOutput {
		out.Output = parseToolOutput(d.rawOutput)
		if ms := durationFromRawOutput(d.rawOutput); ms > 0 && out.IsTask() {
			out.Task = mergeTaskInfo(out.Task, TaskInfo{DurationMs: ms})
		}
	}
	if d.hasLocations {
		out.Locations = locationPaths(d.locations)
	}
	out.At = time.Now()
	tools[d.id] = out
	// rec is the owning child (nil for the main session): §3.1's Activity is
	// the title of its most recent tool call.
	if rec != nil && out.Title != "" {
		rec.info.Activity = truncateUTF8(out.Title, subagentActivityCap)
	}

	var extras []Event
	extras = append(extras, s.syncCursorLocked(out)...)
	if t, ok := tools[d.id]; ok {
		out = t
	}
	if r, ok := s.taskReceipts[d.id]; ok {
		out.Task = mergeTaskInfo(out.Task, r)
		s.dropTaskReceiptLocked(d.id)
		tools[d.id] = out
		extras = append(extras, s.syncCursorLocked(out)...)
		if t, ok := tools[d.id]; ok {
			out = t
		}
	}
	if d.hasRawOutput || d.hasContent {
		join := s.joinFromOutputLocked(owner, out, d.rawOutput, d.content)
		extras = append(extras, join...)
		if t, ok := tools[d.id]; ok {
			out = t
		}
	}

	changed := !exists ||
		out.Status != prev.Status ||
		out.Title != prev.Title ||
		out.Kind != prev.Kind ||
		out.RawInput != prev.RawInput ||
		out.ContentText != prev.ContentText ||
		!sameOutput(prev.Output, out.Output) ||
		!sameDiffs(prev.Diffs, out.Diffs) ||
		!sameTask(prev.Task, out.Task)
	tools[d.id] = out
	return cloneTool(out), changed, extras
}

// toolStatusInFlight is the status set the TUI counts as running.
func toolStatusInFlight(status string) bool {
	return status == "pending" || status == "in_progress"
}

// sameOutput compares by value; ToolOutput holds a *int so == on the struct
// would compare pointers and re-emit an identical update.
func sameOutput(a, b *ToolOutput) bool {
	if a == nil || b == nil {
		return a == b
	}
	if (a.ExitCode == nil) != (b.ExitCode == nil) {
		return false
	}
	if a.ExitCode != nil && *a.ExitCode != *b.ExitCode {
		return false
	}
	x, y := *a, *b
	x.ExitCode, y.ExitCode = nil, nil
	return x == y
}

// sameDiffs compares the summary fields only; the capped texts are large and
// always arrive with them.
func sameDiffs(a, b []ToolDiff) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Path != b[i].Path || a[i].Added != b[i].Added ||
			a[i].Removed != b[i].Removed || a[i].Truncated != b[i].Truncated {
			return false
		}
	}
	return true
}

func sameTask(a, b *TaskInfo) bool {
	if a == nil && b == nil {
		return true
	}
	empty := TaskInfo{}
	if a == nil {
		return *b == empty
	}
	if b == nil {
		return *a == empty
	}
	return *a == *b
}

// mergeTaskInfo fills in the fields the incoming half knows about; a later
// receipt for the same tool overwrites what it carries.
func mergeTaskInfo(prev *TaskInfo, in TaskInfo) *TaskInfo {
	out := TaskInfo{}
	if prev != nil {
		out = *prev
	}
	if in.Description != "" {
		out.Description = in.Description
	}
	if in.Prompt != "" {
		out.Prompt = in.Prompt
	}
	if in.Model != "" {
		out.Model = in.Model
	}
	if in.AgentID != "" {
		out.AgentID = in.AgentID
	}
	if in.SubagentType != "" {
		out.SubagentType = in.SubagentType
	}
	if in.DurationMs > 0 {
		out.DurationMs = in.DurationMs
	}
	if in.Receipt {
		out.Receipt = true
	}
	if in.Status != "" {
		out.Status = in.Status
	}
	if in.Background {
		out.Background = true
	}
	return &out
}

func cloneTool(t ToolEvent) ToolEvent {
	if t.Locations != nil {
		t.Locations = append([]string(nil), t.Locations...)
	}
	if t.Diffs != nil {
		t.Diffs = append([]ToolDiff(nil), t.Diffs...)
	}
	if t.Output != nil {
		out := *t.Output
		if out.ExitCode != nil {
			code := *out.ExitCode
			out.ExitCode = &code
		}
		t.Output = &out
	}
	if t.Task != nil {
		task := *t.Task
		t.Task = &task
	}
	return t
}

func snapshotTools(order []string, byID map[string]ToolEvent) []ToolEvent {
	if len(order) == 0 {
		return nil
	}
	out := make([]ToolEvent, 0, len(order))
	for _, id := range order {
		if t, ok := byID[id]; ok {
			out = append(out, cloneTool(t))
		}
	}
	return out
}

func cloneConfig(in []ConfigOption) []ConfigOption {
	if in == nil {
		return nil
	}
	out := make([]ConfigOption, len(in))
	for i, c := range in {
		out[i] = c
		if c.SelectValues != nil {
			out[i].SelectValues = append([]SelectValue(nil), c.SelectValues...)
		}
	}
	return out
}

func presentJSON(raw json.RawMessage) (json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	return raw, true
}

func stringifyRawInput(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, key := range []string{"command", "args", "prompt"} {
			v, ok := obj[key]
			if !ok {
				continue
			}
			var str string
			if err := json.Unmarshal(v, &str); err == nil {
				return truncateUTF8(sanitizeText(str), rawInputCap)
			}
		}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return truncateUTF8(sanitizeText(string(raw)), rawInputCap)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return truncateUTF8(sanitizeText(string(raw)), rawInputCap)
	}
	return truncateUTF8(sanitizeText(string(b)), rawInputCap)
}

func contentText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var items []acp.ToolContent
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	var b []byte
	for _, it := range items {
		if it.Type != "content" || it.Content == nil || it.Content.Type != "text" {
			continue
		}
		b = append(b, it.Content.Text...)
	}
	return tailUTF8(sanitizeText(string(b)), contentTextCap)
}

func locationPaths(raw json.RawMessage) []string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []acp.ToolCallLocation
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it.Path == "" {
			continue
		}
		out = append(out, sanitizeText(it.Path))
		if len(out) >= locationsCap {
			break
		}
	}
	return out
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	keep := max - len(ellipsis)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + ellipsis
}

func tailUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	keep := max - len(ellipsis)
	if keep < 0 {
		keep = 0
	}
	start := len(s) - keep
	if start < 0 {
		start = 0
	}
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return ellipsis + s[start:]
}

// toolNameOf reads rawInput._toolName, which is how cursor identifies its own
// built-in tools (task, updateTodos, …).
func toolNameOf(raw json.RawMessage) string {
	var obj struct {
		ToolName string `json:"_toolName"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	return sanitizeText(obj.ToolName)
}

type taskRawInput struct {
	ToolName     string          `json:"_toolName"`
	Prompt       string          `json:"prompt"`
	Description  string          `json:"description"`
	SubagentType json.RawMessage `json:"subagentType"`
}

type grokSpawnRawInput struct {
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	SubagentType string `json:"subagent_type"`
	Background   bool   `json:"background"`
}

// taskFromRawInput reads the sub-agent fields off a spawn tool's rawInput.
// toolName is already resolved (_toolName else wire _meta).
func taskFromRawInput(raw json.RawMessage, toolName string) *TaskInfo {
	if toolName == "spawn_subagent" {
		var in grokSpawnRawInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil
		}
		return &TaskInfo{
			Description:  sanitizeText(in.Description),
			Prompt:       truncateUTF8(sanitizeText(in.Prompt), taskPromptCap),
			SubagentType: sanitizeText(in.SubagentType),
			Background:   in.Background,
		}
	}
	var in taskRawInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil
	}
	if toolName != "task" && in.ToolName != "task" {
		return nil
	}
	return &TaskInfo{
		Description:  sanitizeText(in.Description),
		Prompt:       truncateUTF8(sanitizeText(in.Prompt), taskPromptCap),
		SubagentType: subagentTypeName(in.SubagentType),
	}
}

// subagentTypeName flattens cursor's nested subagentType ({"custom":
// {"unspecified":{}}}) to its innermost key.
func subagentTypeName(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return ""
	}
	name := ""
	for i := 0; i < 4; i++ {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || len(obj) != 1 {
			return name
		}
		for k, v := range obj {
			name = sanitizeText(k)
			raw = bytes.TrimSpace(v)
		}
		if len(raw) == 0 || raw[0] != '{' {
			return name
		}
	}
	return name
}

type rawOutputWire struct {
	ExitCode   *int   `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Content    string `json:"content"`
	DurationMs int    `json:"durationMs"`
}

// parseToolOutput stores the tail of each stream plus a head for previews, so
// a long output neither hides its start nor its end.
func parseToolOutput(raw json.RawMessage) *ToolOutput {
	raw, ok := presentJSON(raw)
	if !ok || raw[0] != '{' {
		return nil
	}
	var w rawOutputWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil
	}
	stdout := sanitizeText(w.Stdout)
	stderr := sanitizeText(w.Stderr)
	content := sanitizeText(w.Content)
	if w.ExitCode == nil && stdout == "" && stderr == "" && content == "" {
		return nil
	}
	out := &ToolOutput{
		Stdout:     tailUTF8(stdout, outputTailCap),
		Stderr:     tailUTF8(stderr, outputTailCap),
		Content:    tailUTF8(content, outputTailCap),
		StdoutHead: truncateUTF8(stdout, outputHeadCap),
		StderrHead: truncateUTF8(stderr, outputHeadCap),
		Truncated: len(stdout) > outputTailCap ||
			len(stderr) > outputTailCap ||
			len(content) > outputTailCap,
	}
	if w.ExitCode != nil {
		code := *w.ExitCode
		out.ExitCode = &code
	}
	return out
}

func durationFromRawOutput(raw json.RawMessage) int {
	raw, ok := presentJSON(raw)
	if !ok || raw[0] != '{' {
		return 0
	}
	var w rawOutputWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return 0
	}
	return w.DurationMs
}

// contentDiffs parses the diff items of a tool_call content[]. Added/Removed
// are counted on the uncapped text; only then is the text capped.
func contentDiffs(raw json.RawMessage) []ToolDiff {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var items []acp.ToolContent
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	var out []ToolDiff
	for _, it := range items {
		if it.Type != "diff" {
			continue
		}
		out = append(out, toolDiff(it))
		if len(out) >= diffsCap {
			break
		}
	}
	return out
}

func toolDiff(it acp.ToolContent) ToolDiff {
	oldText := sanitizeText(it.OldText)
	newText := sanitizeText(it.NewText)
	d := ToolDiff{Path: sanitizeText(it.Path)}
	added, removed, _, err := textdiff.Lines(oldText, newText)
	if err != nil {
		d.Truncated = true
	} else {
		d.Added, d.Removed = added, removed
	}
	if len(oldText) > diffTextCap || len(newText) > diffTextCap {
		d.Truncated = true
	}
	d.OldText = truncateUTF8(oldText, diffTextCap)
	d.NewText = truncateUTF8(newText, diffTextCap)
	return d
}
