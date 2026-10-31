package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/transcript"
)

// The result accumulator (plan 041 §3.1, P5–P6) folds the target's own events
// — from its `started` (or the foreign bracket's start) to its ending, the
// main session's only (an event with an agent is a sub-agent's) — into what
// a waiting command reports. It never scans a transcript: transcript entries
// carry no turn id, a foreign turn opens with a note, and the main transcript
// is trimmed from the front (P5), so the turn's own event range is the one
// place its result can be read exactly.

// resultMessageMax is the most of a message the accumulator keeps: its tail,
// with messageCut set when more was dropped (§3.1).
const resultMessageMax = 1 << 20

// The tool statuses a call ends in (agent's ToolEvent.Status).
const (
	toolCompleted = "completed"
	toolFailed    = "failed"
	// toolKindEdit is an edit-kind call's Kind: its paths are files.
	toolKindEdit = "edit"
)

// turnAccumulator is one turn's result, folded from its events.
type turnAccumulator struct {
	// startAt and endAt are the started's time and the ending's.
	startAt, endAt time.Time
	// msg is the last assistant text entry: the run open now, or the last one
	// closed. A run is split exactly where the transcript splits assistant
	// entries (internal/transcript's appendStream and endRun): any main
	// session event that writes an entry of its own or closes the run — a
	// tool, a thought, a plan, a user interjection, an ask's opening, a
	// command line, a compaction, an error row, the wire's own done. runOpen
	// says the next text grows msg; msgCut that msg lost its head
	// (resultMessageMax), or that the entry it was seeded from had. msg may
	// hold up to twice resultMessageMax (compact): message takes the tail.
	msg     []byte
	runOpen bool
	msgCut  bool
	// tools are the main session's tool calls by id; touches every distinct
	// (call, path) an update named, in first-touch order (files) — an ACP
	// call's updates name its paths again and again, touched once each.
	tools   map[string]*accTool
	touches []accTouch
	touched map[accTouch]bool
	// usage is the last usage section folded (P6): before the ending, since
	// the accumulator stops at it.
	usage *agent.UsageState
	// plan is the text of the last plan the turn presented, automatic or not
	// (P9: an automatic plan is not in the transcript at all).
	plan string
	// auto is every automatic question or plan answer, in order (P9).
	auto []autoAnswer
	// returned says the turn presented a plan an unattended host returned
	// rather than accepted (X28, agent.ApprovalReturn): an automatic plan
	// answer that did not accept, which no other policy ever writes. The
	// host then cancelled the turn itself, and its cancelled ending is the
	// plan's delivery, not a person's cancel (turnWaiter.ended).
	returned bool
	// planApproved says the last thing the turn did was have its plan approved
	// (automatically or by a person): nothing that opens a new step of work has
	// come since. A native turn ends right there, so its end_turn is the plan's
	// (turnAccumulator.planEnded); an ACP agent told its plan was accepted goes
	// on, and its next text or tool clears this.
	planApproved bool
	// noPlanEnded says the snapshot this was seeded from names no plan's ask as
	// ended, so that there is nothing to look back for (turnWaiter.lookApproved).
	noPlanEnded bool
}

// accTool is one tool call's merged state: its kind (an update may carry it
// only later) and whether it reached completed or failed.
type accTool struct {
	kind     string
	finished bool
	failed   bool
}

// accTouch is one path a tool call named.
type accTouch struct{ tool, path string }

// autoAnswer is one automatic answer (`autoAnswered`): what was asked and
// what craze's policy chose.
type autoAnswer struct {
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	Choice  string `json:"choice"`
}

func newTurnAccumulator() *turnAccumulator {
	return &turnAccumulator{tools: map[string]*accTool{}, touched: map[accTouch]bool{}}
}

// start is the turn's opening: its time.
func (a *turnAccumulator) start(at time.Time) { a.startAt = at }

// end is the turn's ending: its time.
func (a *turnAccumulator) end(at time.Time) { a.endAt = at }

// runBoundary reports whether ev ends the assistant text run before it: a
// top-level (Agent == "") non-todo tool event, a non-empty thought (an empty
// chunk opens nothing in the transcript, appendStream), or the wire's own
// done. A child's events never are one. turnAccumulator splits on it, and so
// does plain `craze prompt`'s paragraph break (plan 047 D7): one predicate.
func runBoundary(ev agent.Event) bool {
	if ev.Agent != "" {
		return false
	}
	switch ev.Type {
	case agent.EventThought:
		return ev.Text != ""
	case agent.EventTool:
		return ev.Tool != nil && !ev.Tool.IsTodoTool()
	case agent.EventDone:
		return true
	}
	return false
}

// fold applies one event of the turn's range.
func (a *turnAccumulator) fold(ev agent.Event) {
	if ev.Agent != "" || ev.Type == agent.EventSubagent {
		// A sub-agent's text, tools and thoughts are its own (§3.1: main
		// session only).
		return
	}
	if runBoundary(ev) {
		a.split()
	}
	switch ev.Type {
	case agent.EventText:
		a.planApproved = a.planApproved && ev.Text == ""
		a.text(ev.Text)
	case agent.EventThought:
		a.planApproved = a.planApproved && ev.Text == ""
	case agent.EventTool:
		if ev.Tool == nil {
			return
		}
		a.tool(*ev.Tool)
	case agent.EventPlan:
		if p := ev.Plan; p != nil {
			a.plan = p.Plan
			a.planApproved = p.Auto && p.Accepted
			if p.Auto {
				a.auto = append(a.auto, autoPlan(p))
				a.returned = a.returned || !p.Accepted
			} else {
				a.split()
			}
		}
	case agent.EventQuestion:
		if q := ev.Question; q != nil {
			if q.Auto {
				a.auto = append(a.auto, autoQuestion(q))
			} else {
				a.split()
			}
		}
	case agent.EventPermission:
		if ev.Permission != nil {
			a.split()
		}
	case agent.EventUser:
		// An interjection draws a user row — and so ends the run — only
		// when its text is not empty once its shell context is gone: the
		// transcript's own rule (transcript.InterjectionRow).
		if _, ok := transcript.InterjectionRow(ev); ok {
			a.split()
		}
	case agent.EventCommand:
		if ev.Command != nil {
			a.split()
		}
	case agent.EventCompaction:
		if ev.Compaction != nil {
			a.split()
		}
	case agent.EventError:
		if ev.Err != nil && ev.Err.Error() != "" {
			a.split()
		}
	case agent.EventAsk:
		// An answered question's or plan's ending draws its outcome note.
		if u := ev.Ask; u != nil && u.Outcome == agent.AskAnswered && u.Kind != agent.AskPermission {
			a.split()
			if u.Kind == agent.AskPlan {
				a.planApproved = u.Accepted
			}
		}
	case agent.EventMeta:
		if st := ev.State; st != nil {
			if st.Usage != nil {
				u := *st.Usage
				a.usage = &u
			}
			if st.Detail != "" || st.IndexErr != "" {
				// Each draws an error row.
				a.split()
			}
		}
	}
}

// text grows the open run, or opens a new one: the last entry is this one.
func (a *turnAccumulator) text(s string) {
	if s == "" {
		return
	}
	if !a.runOpen {
		a.msg, a.msgCut, a.runOpen = a.msg[:0], false, true
	}
	a.msg = append(a.msg, s...)
	a.compact()
}

// compact cuts msg to its tail (messageTail) once it holds twice
// resultMessageMax: so a long message is moved once a MiB, not once a delta.
func (a *turnAccumulator) compact() {
	if len(a.msg) <= 2*resultMessageMax {
		return
	}
	tail, _ := messageTail(a.msg)
	a.msg, a.msgCut = append(a.msg[:0], tail...), true
}

// messageTail is msg's last resultMessageMax bytes, from a rune's start, and
// whether that cut anything.
func messageTail(msg []byte) ([]byte, bool) {
	if len(msg) <= resultMessageMax {
		return msg, false
	}
	start := len(msg) - resultMessageMax
	for start < len(msg) && !utf8.RuneStart(msg[start]) {
		start++
	}
	return msg[start:], true
}

// split ends the open run: the next text is a new entry.
func (a *turnAccumulator) split() { a.runOpen = false }

// seedMessage is a message already under way when the accumulator starts —
// read from a snapshot: an entry's text, whether its run is still open, and
// whether the entry lost its head.
func (a *turnAccumulator) seedMessage(text string, open, cut bool) {
	a.msg, a.runOpen, a.msgCut = append(a.msg[:0], text...), open, cut
	a.compact()
}

// tool merges one tool update by its id.
func (a *turnAccumulator) tool(t agent.ToolEvent) {
	if t.ID == "" {
		return
	}
	st, ok := a.tools[t.ID]
	if !ok {
		st = &accTool{}
		a.tools[t.ID] = st
		// A call that opens after the approval is more work — but for one the
		// harness refused because the plan was approved, which fails unrun.
		a.planApproved = a.planApproved && t.Status == toolFailed
	}
	if t.Kind != "" {
		st.kind = t.Kind
	}
	if t.Status == toolCompleted || t.Status == toolFailed {
		st.finished = true
		st.failed = t.Status == toolFailed
	}
	for _, p := range t.Locations {
		a.touch(t.ID, p)
	}
	for _, d := range t.Diffs {
		a.touch(t.ID, d.Path)
	}
}

// touch notes that call id named path, the first time it does.
func (a *turnAccumulator) touch(id, path string) {
	if t := (accTouch{tool: id, path: path}); path != "" && !a.touched[t] {
		a.touched[t] = true
		a.touches = append(a.touches, t)
	}
}

// message is the last assistant text entry, and whether it lost its head.
func (a *turnAccumulator) message() (string, bool) {
	tail, cut := messageTail(a.msg)
	return string(tail), a.msgCut || cut
}

// toolCalls is how many distinct tool calls reached completed or failed.
func (a *turnAccumulator) toolCalls() int {
	n := 0
	for _, st := range a.tools {
		if st.finished {
			n++
		}
	}
	return n
}

// files is every distinct path an edit-kind call named, in first-touch order.
func (a *turnAccumulator) files() []string {
	out, seen := []string{}, map[string]bool{}
	for _, t := range a.touches {
		if st := a.tools[t.tool]; st == nil || st.kind != toolKindEdit || seen[t.path] {
			continue
		}
		seen[t.path] = true
		out = append(out, t.path)
	}
	return out
}

// planEnded says the turn, ending with stopReason, ended at a plan it presented:
// returned to the caller (the ACP agents' completed, stop reason cancelled, X28),
// or approved with nothing after it (native's, whose turn the approval ends with
// its own end_turn, so that craze prompt's exit stays what it was).
func (a *turnAccumulator) planEnded(stopReason string) bool {
	return a.returned || a.planApproved && stopReason == stopEndTurn
}

// workSince says a, seeded from a snapshot, shows work since prev, the accumulator
// it replaces: a tool call prev did not have, but for one that failed unrun, or
// another last message. fold reads that as the plan's approval having been followed.
// A gap of thoughts alone goes unseen, a snapshot not showing them as work: the
// approval is then kept, where fold would have cleared it.
func (a *turnAccumulator) workSince(prev *turnAccumulator) bool {
	for id, st := range a.tools {
		if prev.tools[id] == nil && !st.failed {
			return true
		}
	}
	was, _ := prev.message()
	now, _ := a.message()
	return was != now
}

// durationMs is the turn's length, from its started to its ending; 0 when
// either is unknown.
func (a *turnAccumulator) durationMs() int64 {
	if a.startAt.IsZero() || a.endAt.IsZero() || a.endAt.Before(a.startAt) {
		return 0
	}
	return a.endAt.Sub(a.startAt).Milliseconds()
}

// autoQuestion is an automatic question answer: its first question (or its
// title), and the labels picked, question by question.
func autoQuestion(q *agent.QuestionEvent) autoAnswer {
	var picks []string
	for _, qq := range q.Questions {
		var labels []string
		for _, id := range q.Answers[qq.ID] {
			label := id
			for _, o := range qq.Options {
				if o.ID == id && o.Label != "" {
					label = o.Label
				}
			}
			labels = append(labels, label)
		}
		if len(labels) == 0 {
			labels = []string{"nothing"}
		}
		picks = append(picks, strings.Join(labels, ", "))
	}
	return autoAnswer{Kind: string(agent.AskQuestion), Summary: askSummary(agent.AskQuestion, agent.AskBody{Question: q}),
		Choice: psLine(strings.Join(picks, "; "))}
}

// autoPlan is an automatic plan answer: its name, and accepted — or
// returned, the one automatic answer that does not accept (X28,
// agent.ApprovalReturn): the plan handed back to the caller, its turn ended.
func autoPlan(p *agent.PlanEvent) autoAnswer {
	choice := "returned"
	if p.Accepted {
		choice = "accepted"
	}
	name := p.Name
	if strings.TrimSpace(name) == "" {
		name = "plan"
	}
	return autoAnswer{Kind: string(agent.AskPlan), Summary: psLine(name), Choice: choice}
}

// ------------------------------------------------------------ the result

// The statuses a waiting command reports (§3.1), and their exit codes.
const (
	statusCompleted  = "completed"
	statusIdle       = "idle"
	statusFailed     = "failed"
	statusCancelled  = "cancelled"
	statusDropped    = "dropped"
	statusNeedsInput = "needs_input"
	statusRunning    = "running"
)

// statusExit is a status's exit code (§3.1's table): 0 completed or idle, 3
// failed, cancelled or dropped, 4 needs_input, 5 running.
func statusExit(status string) int {
	switch status {
	case statusCompleted, statusIdle:
		return 0
	case statusNeedsInput:
		return 4
	case statusRunning:
		return 5
	}
	return 3
}

// terminalStatus reports whether status is a turn's (or row's) end: the
// statuses that carry message, toolCalls, durationMs and files.
func terminalStatus(status string) bool {
	switch status {
	case statusCompleted, statusFailed, statusCancelled, statusDropped:
		return true
	}
	return false
}

// turnResult is one waiting command's result, as `--json` prints it (§3.1's
// keys): status, session, short, provider and attach always; model once it was
// resolved; turn and/or row with a target; message, toolCalls, durationMs, files
// (`[]` when none) and stopReason on a terminal status; the rest when present. A
// key that is absent is never written as null or false.
type turnResult struct {
	Status       string          `json:"status"`
	Session      string          `json:"session"`
	Short        string          `json:"short"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model,omitempty"`
	Turn         string          `json:"turn,omitempty"`
	Row          string          `json:"row,omitempty"`
	Message      *string         `json:"message,omitempty"`
	MessageCut   bool            `json:"messageCut,omitempty"`
	MessageFile  string          `json:"messageFile,omitempty"`
	StopReason   string          `json:"stopReason,omitempty"`
	Error        *resultError    `json:"error,omitempty"`
	PlanEnded    bool            `json:"planEnded,omitempty"`
	ToolCalls    *int            `json:"toolCalls,omitempty"`
	DurationMs   *int64          `json:"durationMs,omitempty"`
	Files        *[]string       `json:"files,omitempty"`
	Plan         string          `json:"plan,omitempty"`
	Usage        json.RawMessage `json:"usage,omitempty"`
	AutoAnswered []autoAnswer    `json:"autoAnswered,omitempty"`
	Ask          *resultAsk      `json:"ask,omitempty"`
	BlockedBy    string          `json:"blockedBy,omitempty"`
	Partial      bool            `json:"partial,omitempty"`
	Handle       string          `json:"handle,omitempty"`
	Attach       string          `json:"attach"`

	// usage is Usage decoded, for the plain footer.
	usage *agent.UsageState
	// cutBody, when set, is the plain body in place of plainBody's: the
	// head --max-message kept and the pointer to the whole (writeResult).
	cutBody string
	// inc is the incarnation whose stream the result's target is in, ""
	// when the command never had one: part of the name --max-message's
	// whole result is written under (writeFull), since turn ids restart at
	// turn-1 with every incarnation — a reload's first turn is another
	// turn-1 of the same session (plan 041 C8, 1).
	inc string
}

// resultError is `error` wherever the waiting commands print one — a result's
// and an exit-1 envelope's alike: its words on one line, what it was, whether a
// fresh turn may succeed, and what else the provider said of it, when it did.
type resultError struct {
	Message   string       `json:"message"`
	Kind      errkind.Kind `json:"kind"`
	Retryable bool         `json:"retryable"`
	Detail    *errorDetail `json:"detail,omitempty"`
}

// errorDetail is resultError's detail: names and numbers only, each cleaned
// again here whatever host sent it, so no text from outside craze — a key, a
// path, a piece of a request — can ride in it.
type errorDetail struct {
	Status       int    `json:"status,omitempty"`
	ProviderCode string `json:"provider_code,omitempty"`
}

// newResultError is msg as a resultError saying info. A kind a reader does not
// know — none at all, from a host that predates kinds — is unknown.
func newResultError(msg string, info errkind.Info) *resultError {
	info = info.Known()
	e := &resultError{Message: sanitizeLine(msg), Kind: info.Kind, Retryable: info.Retryable}
	if d := (errorDetail{Status: errkind.CleanStatus(info.Status), ProviderCode: errkind.CleanCode(info.ProviderCode)}); d != (errorDetail{}) {
		e.Detail = &d
	}
	return e
}

// kindError is msg as a resultError of kind, with its default retryable.
func kindError(msg string, kind errkind.Kind) *resultError {
	return newResultError(msg, errkind.New(kind))
}

// resultAsk is a needs_input result's open ask.
type resultAsk struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

// resultSession is who a result is about: the session's full id, its
// provider and its model.
type resultSession struct {
	id, provider, model string
}

// newResult is a result about s with status, its always-present keys set —
// "" for a session the command never learnt (craze new --wait cancelled while
// its host was still starting), which has no attach command either.
func newResult(s resultSession, status string) *turnResult {
	short := psShort(s.id)
	res := &turnResult{Status: status, Session: s.id, Short: short, Provider: s.provider, Model: s.model}
	if short != "" {
		res.Attach = "craze attach --session " + short
	}
	return res
}

// fill sets a result's turn facts from the accumulator: plan, usage and the
// automatic answers, and — a terminal status's always, `""`, 0 and `[]` when
// the turn folded none (§3.1) — message, tools, files and duration.
func (r *turnResult) fill(a *turnAccumulator) {
	msg, cut := a.message()
	n, d, files := a.toolCalls(), a.durationMs(), a.files()
	if terminalStatus(r.Status) {
		r.Message, r.ToolCalls, r.DurationMs, r.Files = &msg, &n, &d, &files
		r.MessageCut = cut
		if r.StopReason == "" {
			r.StopReason = defaultStopReason(r.Status)
		}
		r.PlanEnded = r.Status == statusCompleted && a.planEnded(r.StopReason)
	}
	r.Plan = a.plan
	r.AutoAnswered = a.auto
	r.setUsage(a.usage)
}

// defaultStopReason is a terminal status's stopReason when the engine sent none:
// a failure's `error`; a cancel's, and a queued row's that never ran, `cancelled`;
// and `end_turn` for a turn that ran to its end with no reason told — a foreign
// turn's bracket carries none, nor does an older host's lastTurn.
func defaultStopReason(status string) string {
	switch status {
	case statusFailed:
		return stopError
	case statusCancelled, statusDropped:
		return stopCancelled
	}
	return stopEndTurn
}

// setUsage sets the usage section, as the wire's event codec writes it.
func (r *turnResult) setUsage(u *agent.UsageState) {
	r.usage, r.Usage = nil, nil
	if u == nil {
		return
	}
	raw, err := agent.EncodeUsageState(u)
	if err != nil {
		return
	}
	r.usage, r.Usage = u, raw
}

// message is the result's message, "" when it has none.
func (r *turnResult) message() string {
	if r.Message == nil {
		return ""
	}
	return *r.Message
}

// plainBody is what plain output prints above the footer: the message, then
// a plan the turn presented, or `(no final message)` when there is neither.
func (r *turnResult) plainBody() string { return plainBodyOf(r.message(), r.Plan) }

// plainBodyOf is plainBody of a message and a plan.
func plainBodyOf(msg, plan string) string {
	switch {
	case plan == "" && msg == "":
		return "(no final message)"
	case plan == "":
		return msg
	case msg == "":
		return "plan:\n" + plan
	}
	return msg + "\n\nplan:\n" + plan
}

// footer is plain output's footer (§3.1): the status line —
// `<status> · <s>s · <n> tool calls[ · $<turn cost>| · unpriced][ · context
// <a>/<b>]` — then, each when there is one, the error, `files:`, one
// `auto-answered:` line per answer, the ask and what blocks the target, the
// session line, and the exact next command for a running or needs_input
// result.
func (r *turnResult) footer() string {
	var lines []string
	status := r.Status
	if r.Partial {
		status += " (partial)"
	}
	parts := []string{status}
	if r.Status != statusIdle {
		parts = append(parts, fmt.Sprintf("%ds", r.durationSeconds()), plural(r.toolCallCount(), "tool call"))
		if u := r.usage; u != nil {
			parts = append(parts, turnCost(u.Turn))
			if u.ContextWindow > 0 {
				parts = append(parts, "context "+transcript.TokenCount(u.ContextTokens)+"/"+transcript.TokenCount(u.ContextWindow))
			} else if u.ContextTokens > 0 {
				parts = append(parts, "context "+transcript.TokenCount(u.ContextTokens))
			}
		}
	}
	lines = append(lines, strings.Join(parts, " · "))
	if r.Error != nil {
		lines = append(lines, "error: "+psLine(r.Error.Message))
	}
	if r.Files != nil && len(*r.Files) > 0 {
		lines = append(lines, "files: "+psLine(strings.Join(*r.Files, ", ")))
	}
	for _, a := range r.AutoAnswered {
		lines = append(lines, fmt.Sprintf("auto-answered: %s %q → %s", a.Kind, a.Summary, a.Choice))
	}
	if a := r.Ask; a != nil {
		lines = append(lines, fmt.Sprintf("ask: %s %s (%s)", a.Kind, psLine(a.Summary), a.ID))
	}
	if r.BlockedBy != "" {
		lines = append(lines, "blocked by: "+psLine(r.BlockedBy))
	}
	if r.Short != "" {
		lines = append(lines, "session "+r.Short+" · "+r.Attach)
	}
	switch r.Status {
	case statusRunning:
		lines = append(lines, "next: "+waitCommand(r.Short, r.Handle))
	case statusNeedsInput:
		lines = append(lines, fmt.Sprintf("next: answer it in %s (or craze cancel %s, craze stop %s), then %s",
			r.Attach, r.Short, r.Short, waitCommand(r.Short, r.Handle)))
	}
	return strings.Join(lines, "\n")
}

// plain is the whole plain output: the body, `---`, the footer.
func (r *turnResult) plain() string {
	body := r.plainBody()
	if r.cutBody != "" {
		body = r.cutBody
	}
	return body + "\n---\n" + r.footer() + "\n"
}

// durationSeconds is durationMs in whole seconds, rounded.
func (r *turnResult) durationSeconds() int64 {
	if r.DurationMs == nil {
		return 0
	}
	return (*r.DurationMs + 500) / 1000
}

// toolCallCount is toolCalls, 0 when absent.
func (r *turnResult) toolCallCount() int {
	if r.ToolCalls == nil {
		return 0
	}
	return *r.ToolCalls
}

// waitCommand is the command that follows a handle.
func waitCommand(short, handle string) string {
	if handle == "" {
		return "craze wait " + short
	}
	return "craze wait " + short + " --from " + handle
}

// plural is n and noun, plural unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// halfCent is half a cent in picodollars (10⁻¹² $): a cent is 10¹⁰.
const halfCent = 5_000_000_000

// turnCost is a turn's spend as the footer says it: `$0.04`, `<$0.01` for an
// amount above nothing that rounds to nothing, a `+` when some of its usage
// had no price, and `unpriced` when none of it had one (the TUI's status row,
// internal/tui's spendMoney, in the same words).
func turnCost(s agent.Spend) string {
	if s.Unpriced && s.CostPicoUSD == 0 {
		return "unpriced"
	}
	pico := max(s.CostPicoUSD, 0)
	var out string
	if pico > 0 && pico < halfCent {
		out = "<$0.01"
	} else {
		cents := pico/(2*halfCent) + (pico%(2*halfCent)+halfCent)/(2*halfCent)
		out = fmt.Sprintf("$%d.%02d", cents/100, cents%100)
	}
	if s.Unpriced {
		out += "+"
	}
	return out
}

// ------------------------------------------------------------- the output

// resultOutput is how a waiting command prints its result (§3.1): plain or
// --json, with --max-message's cut.
type resultOutput struct {
	// what names the command in its lines: "craze wait".
	what string
	json bool
	// maxMessage is --max-message, 0 for none.
	maxMessage int
	// env is where the cut's whole result is written (rundir.ResultsDir).
	env rundir.Env
}

// writeResult prints res and answers its status's exit (statusExit): nil for
// 0, else an exit with no line of its own — the result on stdout says it. A
// result that cannot be written is the command's failure, exit 1.
func writeResult(stdout, stderr io.Writer, o resultOutput, res *turnResult) error {
	if o.maxMessage > 0 {
		o.cut(stderr, res)
	}
	b, err := o.render(res)
	if err != nil {
		return exitf(1, "%s: the result: %v", o.what, err)
	}
	if _, err := stdout.Write(b); err != nil {
		return exitf(1, "%s: %s", o.what, sanitizeLine(err.Error()))
	}
	if code := statusExit(res.Status); code != 0 {
		return &exitError{code: code}
	}
	return nil
}

// render is res as the command prints it: one JSON object on a line, or the
// plain body and footer.
func (o resultOutput) render(res *turnResult) ([]byte, error) {
	if !o.json {
		return []byte(res.plain()), nil
	}
	b, err := json.Marshal(res)
	return append(b, '\n'), err
}

// cut is --max-message (P13): N characters bound the message and the plan
// together, in that order (cutResult) — a --plan sub-agent's main output is
// its plan (X28), and P13's reason, a caller that truncates long output,
// applies to it as much as to a message. When anything was cut or left out,
// the message and plan printed are what was kept — never more than N
// characters of the two (C8, 2) — plain output's body then ends with
// `[… cut: full result in <path>]`, and --json's messageFile names the file;
// the whole result — the plain body and footer, or the JSON — is written to
// that file under the cache tree (rundir.ResultsDir), <short>-<inc>-<turn>.md
// (writeFull). A file that cannot be written does not lift the bound (C8 r1,
// 2): the message and plan are cut all the same, --json names no
// messageFile, plain output's marker reads `[… cut: the full result could
// not be saved: <reason>]`, stderr says so too, and the exit is the
// result's own.
func (o resultOutput) cut(stderr io.Writer, res *turnResult) {
	msg, plan, cut := cutResult(res.message(), res.Plan, o.maxMessage)
	if !cut {
		return
	}
	full, err := o.render(res)
	var path string
	if err == nil {
		path, err = o.writeFull(res, full)
	}
	marker := "[… cut: full result in " + path + "]"
	if err != nil {
		why := unsavedWhy(err)
		fmt.Fprintf(stderr, "%s: --max-message: the full result could not be saved: %s; the message and plan are cut\n", o.what, why)
		marker = "[… cut: the full result could not be saved: " + why + "]"
	} else {
		res.MessageFile = path
	}
	if o.json {
		if res.Message != nil {
			res.Message = &msg
		}
		res.Plan = plan
	} else {
		res.cutBody = plainBodyOf(msg, plan) + "\n" + marker
	}
}

// unsavedWhyMax bounds the reason an unsaved result's marker gives, in
// characters, its ellipsis included: one short line.
const unsavedWhyMax = 200

// unsavedWhy is the reason an unsaved result's marker gives: err on one line,
// at most unsavedWhyMax characters, a cut one ending in an ellipsis.
func unsavedWhy(err error) string {
	why := sanitizeLine(err.Error())
	if short, cut := cutMessage(why, unsavedWhyMax-1); cut {
		why = short + "…"
	}
	return why
}

// cutResult is --max-message's budget of n characters over a result's message
// and plan, in that order (plan 041 C8, 2, amending X29): the message's first
// n; the plan, what is left — cut at a character's boundary, and back to the
// end of its last whole line when one ends within what is left — and none
// when nothing is left; and whether either was cut or left out. The two
// together are never more than n characters.
func cutResult(msg, plan string, n int) (msgHead, planHead string, cut bool) {
	msgHead, cut = cutMessage(msg, n)
	if plan == "" {
		return msgHead, "", cut
	}
	left := n - utf8.RuneCountInString(msgHead)
	if left <= 0 {
		return msgHead, "", true
	}
	planHead, planCut := cutMessage(plan, left)
	if !planCut {
		return msgHead, plan, cut
	}
	if i := strings.LastIndexByte(planHead, '\n'); i > 0 {
		planHead = planHead[:i]
	}
	return msgHead, planHead, true
}

// writeFull writes a whole result to <results>/<short>-<inc>-<turn>.md (the
// row's id for a row that never ran), 0600 and atomically — a later wait on
// the same turn rewrites it under a reader — and answers its path. <inc> is
// the incarnation's last 8 characters (plan 041 C8, 1): turn ids restart at
// turn-1 with every incarnation, and a reloaded session keeps its short id,
// so without it a reload's turn-1 would overwrite the first incarnation's.
// The last 8, as the session's short id is its craze id's: an incarnation is
// a UUIDv7, whose first characters are its creation time and so are shared by
// two incarnations made within a minute or so of each other, and whose last
// are random. A result whose incarnation is unknown has no <inc> part; one
// that is not a handle's characters is written encoded, as a turn id is.
func (o resultOutput) writeFull(res *turnResult, data []byte) (string, error) {
	dir, err := rundir.ResultsDir(o.env)
	if err != nil {
		return "", err
	}
	name := res.Turn
	if name == "" {
		name = res.Row
	}
	if name == "" || !handleChars(name, true) {
		name = base64.RawURLEncoding.EncodeToString([]byte(name))
	}
	if inc := psShort(res.inc); inc != "" {
		if !handleChars(inc, false) {
			inc = base64.RawURLEncoding.EncodeToString([]byte(inc))
		}
		name = inc + "-" + name
	}
	path := filepath.Join(dir, res.Short+"-"+name+".md")
	if err := atomicfile.Write(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// failureBody is the one object a failure with no result prints under --json.
type failureBody struct {
	Error  *resultError `json:"error"`
	Handle string       `json:"handle,omitempty"`
}

// writeFailure prints a wait's exit-1 failure (§3.1): with --json, one
// {"error": {"message": …, "kind": …, "retryable": …}} object on stdout first,
// `handle` beside it when the failure has one; and the command's one stderr
// line, which names the handle — and, for a turn that goes on, the command that
// follows it.
func writeFailure(stdout io.Writer, o resultOutput, short string, f *waitFailure) error {
	msg := sanitizeLine(f.msg)
	kind := f.kind
	if kind == "" {
		kind = errkind.Internal
	}
	if o.json {
		if b, err := json.Marshal(failureBody{Error: kindError(msg, kind), Handle: f.handle}); err == nil {
			fmt.Fprintf(stdout, "%s\n", b)
		}
	}
	switch {
	case f.handle != "" && f.follow:
		msg += "; " + waitCommand(short, f.handle) + " follows it"
	case f.handle != "":
		msg += " (handle " + f.handle + ")"
	}
	return kindf(kind, "%s: %s", o.what, msg)
}

// failJSON is an exit-1 error before the wait began, with --json's object
// first (writeFailure); any other error — a usage error stays plain — as it
// is.
func failJSON(stdout io.Writer, o resultOutput, err error) error {
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 || !o.json {
		return err
	}
	_ = writeFailure(stdout, o, "", &waitFailure{msg: o.exitMessage(err), kind: failureKind(err)})
	return err
}

// exitMessage is an exit error's message without the command's own prefix
// (o.what, as resolveLive's and dialWait's errors start), which writeFailure
// puts back.
func (o resultOutput) exitMessage(err error) string {
	msg := err.Error()
	var ee *exitError
	if errors.As(err, &ee) {
		msg = ee.msg
	}
	if rest, ok := strings.CutPrefix(msg, o.what+": "); ok {
		return rest
	}
	return msg
}

// cutMessage is --max-message (P13): a message longer than n characters is
// cut to its first n, and true; otherwise it is left whole.
func cutMessage(msg string, n int) (string, bool) {
	if n <= 0 {
		return msg, false
	}
	count := 0
	for i := range msg {
		if count == n {
			return msg[:i], true
		}
		count++
	}
	return msg, false
}
