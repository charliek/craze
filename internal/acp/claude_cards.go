package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The claude dialect's cards and steering (plan 052 § B2, § B4, § B5, § B7):
// AskUserQuestion as an elicitation/create form, ExitPlanMode as a suspected
// plan exit routed to the plan card ahead of the permission and force policy,
// and _session/steering for the mid-turn interjection.

// MethodClaudeSteering is claude-agent-acp's steering request (v1/steering.js):
// a message injected into the running turn. craze always sends it with
// _meta.steering.idleBehavior "promptRequired", so an idle session hands the
// text back instead of starting a turn of its own.
const MethodClaudeSteering = "_session/steering"

// The steering outcomes the adapter answers with (v1/acp-agent.js steer).
const (
	steerInjected       = "injected"
	steerPromptRequired = "promptRequired"
	steerStartedNewTurn = "startedNewTurn"
)

var (
	// ErrSteerIdle is a steer the adapter found no running turn for: the text
	// was not taken, and is the caller's to send as a prompt.
	ErrSteerIdle = errors.New("acp: no running turn to steer")
	// ErrSteerStartedTurn is a steer the adapter took as a turn of its own,
	// which idleBehavior promptRequired should rule out. The text was taken.
	ErrSteerStartedTurn = errors.New("acp: the steer started a turn of the agent's own")
)

// The ids claude-agent-acp gives ExitPlanMode's options (permissions/options.js):
// craze answers a plan exit with one of these two and never any other.
const (
	ClaudeExitPlanDefault = "exit-plan-default"
	ClaudeExitPlanReject  = "reject"
	claudeExitPlanPrefix  = "exit-plan-"
	claudeExitPlanTool    = "ExitPlanMode"
	claudeEnterPlanTool   = "EnterPlanMode"
	claudeSwitchModeKind  = "switch_mode"
	claudeApprovePlanText = "Approve Plan"
	claudeReadyToCodeText = "Ready to code?"
)

// RefusedRequest is a claude request craze answered itself without raising
// anything, because it could not be shown safely (plan 052 § B4, § B5): an
// elicitation in a mode it never declared or a form it cannot render, or a
// plan exit whose shape is not the one it knows. Answer is what was sent.
type RefusedRequest struct {
	Method string
	Kind   string
	Answer string
	Detail string
}

// The kinds of RefusedRequest.
const (
	RefusedElicitationMode = "elicitation_mode"
	RefusedElicitationForm = "elicitation_form"
	RefusedPlanExitShape   = "plan_exit_shape"
	RefusedRequestSession  = "request_session"
)

// requestSessionUnknown says why a claude request's sessionId names no session
// craze routes — missing, null, not a string, or neither the active session
// nor a registered child — or "" when it names one (review C3c 1). Such a
// request is malformed: no lookup is ever made under "".
func (c *Client) requestSessionUnknown(params json.RawMessage) string {
	var w struct {
		SessionID *json.RawMessage `json:"sessionId"`
	}
	if json.Unmarshal(params, &w) != nil || w.SessionID == nil {
		return "the request names no session"
	}
	var sid string
	if json.Unmarshal(*w.SessionID, &sid) != nil || sid == "" {
		return "the request's session is not an id"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionID == "" || (sid != c.sessionID && !c.isChildLocked(sid)) {
		return "the request is about a session craze does not route"
	}
	return ""
}

// SetRefusedRequestHandler installs the handler told of every RefusedRequest.
// It runs on its own goroutine, after the reply was written.
func (c *Client) SetRefusedRequestHandler(h func(RefusedRequest)) {
	c.mu.Lock()
	c.refusedHandler = h
	c.mu.Unlock()
}

func (c *Client) reportRefused(r RefusedRequest) {
	c.mu.Lock()
	h := c.refusedHandler
	c.mu.Unlock()
	if h != nil {
		go h(r)
	}
}

// claudeToolsCap bounds the tool-name memory: a request names the tool call
// just announced, never one long gone.
const claudeToolsCap = 256

// claudeToolCall reads a claude tool update's id, tool name and status.
func claudeToolCall(update json.RawMessage) (id, name, status string) {
	var w struct {
		ToolCallID string `json:"toolCallId"`
		Status     string `json:"status"`
		Meta       struct {
			ClaudeCode struct {
				ToolName string `json:"toolName"`
			} `json:"claudeCode"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(update, &w); err != nil {
		return "", "", ""
	}
	return w.ToolCallID, w.Meta.ClaudeCode.ToolName, w.Status
}

// toolKey is one tool call of one session: a toolCallId is the adapter's,
// unique within the session that announced it and nothing more.
type toolKey struct{ session, call string }

// toolSeen is what the adapter has stamped on one tool call: the latest name,
// and whether it ever gave the call two different names — which makes any
// request about the call a suspect (§ B5, review C3 item 1).
type toolSeen struct {
	name      string
	conflicts bool
	// exit says the call was ever named ExitPlanMode, and settled that it
	// reached a terminal status: such a call is never forgotten while it is
	// open (noteClaudeTool), so a request about it is always suspected.
	exit, settled bool
}

// noteClaudeTool remembers the tool name the adapter stamped on a tool call of
// sid, so the permission request that follows can be checked against it
// (§ B5). It is called only for an update the session filter routed — the
// active session's or a registered child's — never for one it dropped. The
// latest name wins, and a second, different name marks the call.
func (c *Client) noteClaudeTool(sid string, update json.RawMessage) {
	id, name, status := claudeToolCall(update)
	if id == "" || sid == "" {
		return
	}
	k := toolKey{sid, id}
	settled := status == "completed" || status == "failed"
	c.mu.Lock()
	defer c.mu.Unlock()
	if name == "" {
		// A terminal update that names no tool still settles the call it
		// is about (review C3c 2); it teaches no name.
		if seen, ok := c.claudeTools[k]; ok && settled {
			seen.settled = true
			c.claudeTools[k] = seen
		}
		return
	}
	if seen, ok := c.claudeTools[k]; ok {
		seen.conflicts = seen.conflicts || seen.name != name
		seen.name = name
		seen.exit = seen.exit || name == claudeExitPlanTool
		seen.settled = seen.settled || settled
		c.claudeTools[k] = seen
		return
	}
	if c.claudeTools == nil {
		c.claudeTools = make(map[toolKey]toolSeen)
	}
	for len(c.claudeToolOrder) >= claudeToolsCap && c.evictClaudeToolLocked() {
		// Back to the cap: settled calls past it go now, not one per call
		// (review C3c 2).
	}
	c.claudeTools[k] = toolSeen{name: name, exit: name == claudeExitPlanTool, settled: settled}
	c.claudeToolOrder = append(c.claudeToolOrder, k)
}

// evictClaudeToolLocked forgets the oldest call it may, and reports whether
// it found one: never one named
// ExitPlanMode that is still open, whose forgetting would let a request about
// it pass for an ordinary one (review C3b 2). With every remembered call such
// a one, nothing is forgotten and the memory grows past its cap — bounded by
// the plan exits the adapter leaves open at once. c.mu is held.
func (c *Client) evictClaudeToolLocked() bool {
	for i, k := range c.claudeToolOrder {
		if seen := c.claudeTools[k]; seen.exit && !seen.settled {
			continue
		}
		delete(c.claudeTools, k)
		c.claudeToolOrder = append(c.claudeToolOrder[:i:i], c.claudeToolOrder[i+1:]...)
		return true
	}
	return false
}

// settleClaudeToolsLocked settles every remembered call of session sid: its
// prompted turn is over, or cancelled, so no request about one of them is
// still to come (review C3c 2) — the exits among them become forgettable.
// c.mu is held.
func (c *Client) settleClaudeToolsLocked(sid string) {
	for k, seen := range c.claudeTools {
		if k.session == sid && !seen.settled {
			seen.settled = true
			c.claudeTools[k] = seen
		}
	}
}

// claudeTool is what the session sid's tool call id was stamped with.
func (c *Client) claudeTool(sid, id string) toolSeen {
	if id == "" || sid == "" {
		return toolSeen{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claudeTools[toolKey{sid, id}]
}

// resetClaudeToolsLocked forgets every tool call: a new or loaded session's
// ids are its own. c.mu is held.
func (c *Client) resetClaudeToolsLocked() {
	c.claudeTools = nil
	c.claudeToolOrder = nil
}

// ------------------------------------------------------------ plan exits

// planExit is one claude permission request read for a plan exit (§ B5).
type planExit struct {
	// suspected: any of the four signals holds.
	suspected bool
	// safe: the exact shape R2-7 pins, which alone raises the plan card.
	safe bool
	// plan is the plan text, rawInput.plan or else the content's text.
	plan string
	// why is what made an unsafe exit unsafe, for the note.
	why string
	// rejectID is the option an unsafe exit is answered with: a reject
	// option, "" for none (the request is then answered cancelled).
	rejectID string
}

// classifyPlanExit reads a claude session/request_permission for a plan exit.
// A request is suspected when any of these holds (§ B5): its tool call's kind
// is switch_mode; its title is "Approve Plan" (or the adapter's v2 heading,
// "Ready to code?"); an option id starts with exit-plan-; a tool name — the
// request's _meta.claudeCode.toolName, its tool call's, the tool call's own
// name, or the one the preceding tool_call was stamped with — is ExitPlanMode.
//
// The preceding tool_call's name is looked up by the request's own session
// and tool call id (noteClaudeTool); a call the adapter named twice over is a
// suspect whatever its names are.
//
// One request is not a suspect though its kind is switch_mode: EnterPlanMode's,
// which the adapter marks switch_mode too (tool-calls/reporters/interaction.js)
// — but only when the request ITSELF says EnterPlanMode (its tool call's name,
// either _meta tool name, or its title), no option id starts with exit-plan-,
// it has no plan title, and no source at all, the remembered tool_call
// included, names anything else. A remembered name alone never makes a request
// ordinary, and a call ever named ExitPlanMode is remembered until it settles
// (noteClaudeTool), so a request about an open exit is always a suspect
// (review C3b 2).
func (c *Client) classifyPlanExit(params json.RawMessage, req PermissionRequest) planExit {
	var w struct {
		Title    string     `json:"title"`
		Meta     claudeMeta `json:"_meta"`
		ToolCall struct {
			ToolCallID string          `json:"toolCallId"`
			Name       string          `json:"name"`
			Title      string          `json:"title"`
			Kind       *string         `json:"kind"`
			RawInput   json.RawMessage `json:"rawInput"`
			Content    json.RawMessage `json:"content"`
			Meta       claudeMeta      `json:"_meta"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(params, &w); err != nil {
		// decodeObject already took it as a permission request; a shape this
		// read cannot follow is suspected and unsafe, never allowed.
		return planExit{suspected: true, why: "the request did not decode", rejectID: rejectOption(req.Options)}
	}
	var own []string
	for _, n := range []string{w.Meta.ClaudeCode.ToolName, w.ToolCall.Meta.ClaudeCode.ToolName, w.ToolCall.Name} {
		if n != "" {
			own = append(own, n)
		}
	}
	seen := c.claudeTool(req.SessionID, w.ToolCall.ToolCallID)
	names := own
	if seen.name != "" {
		names = append(append([]string(nil), own...), seen.name)
	}
	ownEnter := w.ToolCall.Title == claudeEnterPlanTool
	for _, n := range own {
		ownEnter = ownEnter || n == claudeEnterPlanTool
	}
	exitOption := false
	for _, o := range req.Options {
		if strings.HasPrefix(o.OptionID, claudeExitPlanPrefix) {
			exitOption = true
		}
	}
	titled := false
	for _, t := range []string{w.Title, w.ToolCall.Title} {
		if t == claudeApprovePlanText || t == claudeReadyToCodeText {
			titled = true
		}
	}
	switchMode := w.ToolCall.Kind != nil && *w.ToolCall.Kind == claudeSwitchModeKind
	exitNamed := false
	enterOnly := true
	for _, n := range names {
		if n == claudeExitPlanTool {
			exitNamed = true
		}
		if n != claudeEnterPlanTool {
			enterOnly = false
		}
	}
	if !switchMode && !titled && !exitOption && !exitNamed && !seen.conflicts {
		return planExit{}
	}
	if ownEnter && enterOnly && !titled && !exitOption && !seen.conflicts {
		return planExit{}
	}
	pe := planExit{suspected: true, rejectID: rejectOption(req.Options)}
	pe.plan = planExitText(w.ToolCall.RawInput, w.ToolCall.Content)
	pe.why = planExitUnsafe(req.Options, w.ToolCall.Kind, names, pe.plan)
	if pe.why == "" && seen.conflicts {
		pe.why = "its tool call was given two names"
	}
	pe.safe = pe.why == ""
	return pe
}

type claudeMeta struct {
	ClaudeCode struct {
		ToolName string `json:"toolName"`
	} `json:"claudeCode"`
}

// planExitUnsafe is why a suspected exit is not the exact safe shape (R2-7),
// "" when it is: unique option ids; exactly one exit-plan-default, of kind
// allow_once; exactly one reject, of kind reject_once; the tool call's kind,
// when present, switch_mode; every tool name present ExitPlanMode; and a
// plan.
func planExitUnsafe(opts []PermissionOption, kind *string, names []string, plan string) string {
	seen := map[string]bool{}
	approve, reject := 0, 0
	for _, o := range opts {
		if seen[o.OptionID] {
			return fmt.Sprintf("option %q is offered twice", o.OptionID)
		}
		seen[o.OptionID] = true
		switch o.OptionID {
		case ClaudeExitPlanDefault:
			if o.Kind != KindAllowOnce {
				return fmt.Sprintf("%s is of kind %q", ClaudeExitPlanDefault, o.Kind)
			}
			approve++
		case ClaudeExitPlanReject:
			if o.Kind != KindRejectOnce {
				return fmt.Sprintf("%s is of kind %q", ClaudeExitPlanReject, o.Kind)
			}
			reject++
		}
	}
	switch {
	case approve != 1:
		return "no " + ClaudeExitPlanDefault + " option"
	case reject != 1:
		return "no " + ClaudeExitPlanReject + " option"
	case kind != nil && *kind != claudeSwitchModeKind:
		return fmt.Sprintf("the tool call's kind is %q", *kind)
	}
	for _, n := range names {
		if n != claudeExitPlanTool {
			return fmt.Sprintf("the tool is named %q", n)
		}
	}
	if strings.TrimSpace(plan) == "" {
		return "it carries no plan"
	}
	return ""
}

// planExitText is the plan a request presents: rawInput.plan, or else the
// text of its content blocks.
func planExitText(rawInput, content json.RawMessage) string {
	var in struct {
		Plan json.RawMessage `json:"plan"`
	}
	if json.Unmarshal(rawInput, &in) == nil {
		var s string
		if json.Unmarshal(in.Plan, &s) == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	var blocks []struct {
		Type    string `json:"type"`
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "content" && b.Content.Type == "text" && strings.TrimSpace(b.Content.Text) != "" {
			parts = append(parts, b.Content.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// rejectOption is the option an unsafe plan exit is answered with: exactly the
// option named reject, whatever kind it is given — the id is what the adapter
// acts on, and reject is its "keep planning" — or "" when there is none: the
// request is then answered cancelled (review C3 item 2). No other id is ever
// chosen: the adapter throws on one it does not know.
func rejectOption(opts []PermissionOption) string {
	for _, o := range opts {
		if o.OptionID == ClaudeExitPlanReject {
			return o.OptionID
		}
	}
	return ""
}

// routeClaudePermission is a claude permission request's routing (§ B5): a
// suspected plan exit never reaches the permission handler — so neither the
// force policy nor a permission card ever sees it. The safe shape is raised
// as a plan; anything else is answered reject (or cancelled) here and now. It
// reports whether it took the request.
func (c *Client) routeClaudePermission(msg *Message, req PermissionRequest) bool {
	pe := c.classifyPlanExit(msg.Params, req)
	if why := c.requestSessionUnknown(msg.Params); why != "" {
		// A request about no session craze knows (review C3c 1): its tool
		// call cannot be checked against the one it names, so it never
		// reaches a handler — or force. A suspected exit gets the unsafe
		// exit's answer; anything else is cancelled.
		answer, sent := cancelledOutcome(), "cancelled"
		if pe.suspected && pe.rejectID != "" {
			answer, sent = selectedOutcome(pe.rejectID), pe.rejectID
		}
		_ = c.conn.Reply(msg.ID, answer)
		c.reportRefused(RefusedRequest{Method: msg.Method, Kind: RefusedRequestSession, Answer: sent, Detail: why})
		return true
	}
	if !pe.suspected {
		return false
	}
	if !pe.safe {
		answer := cancelledOutcome()
		sent := "cancelled"
		if pe.rejectID != "" {
			answer = selectedOutcome(pe.rejectID)
			sent = pe.rejectID
		}
		_ = c.conn.Reply(msg.ID, answer)
		c.reportRefused(RefusedRequest{Method: msg.Method, Kind: RefusedPlanExitShape, Answer: sent, Detail: pe.why})
		return true
	}
	plan, _ := json.Marshal(pe.plan)
	preq := CreatePlanRequest{Plan: plan}
	c.dispatch(msg, RequestParams{Plan: &preq}, func(in *pendingReq) { c.handleClaudePlanExit(in, preq) })
	return true
}

// handleClaudePlanExit asks the plan handler about a safe plan exit and
// answers with one of the two options craze ever sends one: approve is
// exit-plan-default, anything else reject. With no handler it is rejected:
// nothing approves a plan by default.
func (c *Client) handleClaudePlanExit(in *pendingReq, req CreatePlanRequest) {
	c.mu.Lock()
	h := c.planHandler
	c.mu.Unlock()
	if h == nil {
		c.replyIncoming(in, claudePlanOutcome(PlanDecision{}))
		return
	}
	dec := h(c.arrival(in), req)
	disp := c.replyIncoming(in, claudePlanOutcome(dec))
	if dec.Replied != nil {
		dec.Replied(disp)
	}
}

// claudePlanOutcome is a plan decision as a claude plan exit's reply:
// exit-plan-default, reject, or cancelled. exit-plan-auto, -bypass,
// -accept-edits and -clear-* are never sent.
func claudePlanOutcome(dec PlanDecision) json.RawMessage {
	switch {
	case dec.Cancelled:
		return cancelledOutcome()
	case dec.Accept:
		return selectedOutcome(ClaudeExitPlanDefault)
	default:
		return selectedOutcome(ClaudeExitPlanReject)
	}
}

// ------------------------------------------------------------ elicitation

// elicitationResult is an elicitation/create reply: {action, content?}.
func elicitationResult(action string, content map[string]any) json.RawMessage {
	var b bytes.Buffer
	b.WriteString(`{"action":`)
	writeJSON(&b, action)
	if content != nil {
		b.WriteString(`,"content":`)
		writeJSON(&b, content)
	}
	b.WriteByte('}')
	return b.Bytes()
}

var questionKey = regexp.MustCompile(`^question_(0|[1-9][0-9]{0,3})(_custom)?$`)

// The keywords a form may carry, each at its level: exactly what the
// adapter's own AskUserQuestion form uses (elicitation.js
// askUserQuestionsToCreateRequest), plus the schema's `required`, which craze
// enforces (AskQuestion.Required). Any other keyword is a constraint the card
// cannot enforce — an `enum` on the free text, a `minItems` on a
// multi-select — and the form is refused (review C3 item 4).
var (
	formSchemaKeys   = []string{"type", "properties", "required", "title", "description"}
	formSingleKeys   = []string{"type", "title", "description", "oneOf"}
	formMultiKeys    = []string{"type", "title", "description", "items"}
	formItemsKeys    = []string{"anyOf"}
	formOptionKeys   = []string{"const", "title", "description", "_meta"}
	formCustomKeys   = []string{"type", "title", "description", "_meta"}
	errFormMalformed = "the request is not a well-formed elicitation"
)

// strictObject decodes raw as a JSON object and names its first key not in
// allowed ("" when none is).
func strictObject(raw json.RawMessage, allowed []string) (map[string]json.RawMessage, string, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, "", false
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !slices.Contains(allowed, k) {
			return m, k, true
		}
	}
	return m, "", true
}

// parseElicitation reads a claude elicitation/create (§ B4). mode is the
// request's mode — set only when the envelope is well formed: a request whose
// mode, message, toolCallId or schema is of the wrong type is malformed, and
// cancelled, never declined (review C3 item 3). For a form, req is the
// questions it asks, and why says what made it unanswerable ("" when it is
// not). A form is answerable only when every property is a question_<i> — a
// string with a oneOf of {const,title} options, or an array whose items are an
// anyOf of them — or a question_<i>_custom string beside its question_<i>,
// each carrying no keyword but those (formSchemaKeys and the rest); its
// `required` may name questions, which must then be answered.
func parseElicitation(params json.RawMessage) (mode string, req AskQuestionRequest, why string) {
	var w struct {
		Mode            *string         `json:"mode"`
		ToolCallID      string          `json:"toolCallId"`
		Message         string          `json:"message"`
		RequestedSchema json.RawMessage `json:"requestedSchema"`
	}
	if err := json.Unmarshal(params, &w); err != nil {
		return "", AskQuestionRequest{}, errFormMalformed
	}
	// A field that is there and null is malformed, not absent: Go would read
	// mode:null as no mode (form) and message:null as "" (review C3b 1).
	var fields map[string]json.RawMessage
	if json.Unmarshal(params, &fields) != nil {
		return "", AskQuestionRequest{}, errFormMalformed
	}
	for _, k := range []string{"mode", "message", "toolCallId", "requestedSchema", "sessionId"} {
		if raw, ok := fields[k]; ok && string(bytes.TrimSpace(raw)) == "null" {
			return "", AskQuestionRequest{}, errFormMalformed
		}
	}
	mode = "form"
	if w.Mode != nil {
		if *w.Mode == "" {
			return "", AskQuestionRequest{}, errFormMalformed
		}
		mode = *w.Mode
	}
	if mode != "form" {
		return mode, AskQuestionRequest{}, ""
	}
	top, extra, ok := strictObject(w.RequestedSchema, formSchemaKeys)
	if !ok {
		return mode, AskQuestionRequest{}, "the form's schema is not an object"
	}
	if extra != "" {
		return mode, AskQuestionRequest{}, fmt.Sprintf("the form's schema carries %q, which the card cannot enforce", extra)
	}
	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(w.RequestedSchema, &schema); err != nil || schema.Type != "object" {
		return mode, AskQuestionRequest{}, "the form's schema is not an object"
	}
	if _, has := top["required"]; has && schema.Required == nil {
		return mode, AskQuestionRequest{}, "the form's required is not a list"
	}
	if len(schema.Properties) == 0 {
		return mode, AskQuestionRequest{}, "the form asks nothing"
	}
	questions := map[int]*formQuestion{}
	customs := map[int]bool{}
	for key, raw := range schema.Properties {
		m := questionKey.FindStringSubmatch(key)
		if m == nil {
			return mode, AskQuestionRequest{}, fmt.Sprintf("property %q is not a question", key)
		}
		i, _ := strconv.Atoi(m[1])
		if m[2] != "" {
			_, extra, ok := strictObject(raw, formCustomKeys)
			switch {
			case !ok || !isStringProperty(raw):
				return mode, AskQuestionRequest{}, fmt.Sprintf("property %q is not a text field", key)
			case extra != "":
				return mode, AskQuestionRequest{}, fmt.Sprintf("property %q carries %q, which the card cannot enforce", key, extra)
			}
			customs[i] = true
			continue
		}
		q, err := parseQuestionProperty(raw)
		if err != "" {
			return mode, AskQuestionRequest{}, fmt.Sprintf("property %q %s", key, err)
		}
		q.ID = key
		questions[i] = &q
	}
	for i := range customs {
		if questions[i] == nil {
			return mode, AskQuestionRequest{}, fmt.Sprintf("question_%d_custom has no question", i)
		}
		questions[i].AllowText = true
	}
	for _, key := range schema.Required {
		m := questionKey.FindStringSubmatch(key)
		if m == nil || m[2] != "" {
			// A required free text the card would have to insist on.
			return mode, AskQuestionRequest{}, fmt.Sprintf("the form requires %q, which the card cannot enforce", key)
		}
		i, _ := strconv.Atoi(m[1])
		if questions[i] == nil {
			return mode, AskQuestionRequest{}, fmt.Sprintf("the form requires %q, which it does not ask", key)
		}
		questions[i].Required = true
	}
	order := make([]int, 0, len(questions))
	for i := range questions {
		order = append(order, i)
	}
	sort.Ints(order)
	req = AskQuestionRequest{ToolCallID: w.ToolCallID}
	single := len(order) == 1
	for _, i := range order {
		q := *questions[i]
		// The prompt: a question's own description when the form has several
		// (each field carries its question there), the request's message when
		// it has one (the adapter carries a lone question's text there), and
		// the field's title — the question's short header — as the last word.
		switch {
		case q.Prompt != "" && !single:
		case single && strings.TrimSpace(w.Message) != "":
			q.Prompt = w.Message
		case q.Prompt == "":
			q.Prompt = q.header
		}
		q.header = ""
		req.Questions = append(req.Questions, q.AskQuestion)
	}
	if !single {
		req.Title = w.Message
	}
	return mode, req, ""
}

// formQuestion is one question_<i> as read, with the field's title kept apart
// until the prompt is chosen.
type formQuestion struct {
	AskQuestion
	header string
}

func isStringProperty(raw json.RawMessage) bool {
	var p struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &p) == nil && p.Type == "string"
}

type enumOption struct {
	Const       json.RawMessage `json:"const"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
}

// parseQuestionProperty reads one question_<i>, or says what is wrong with it.
func parseQuestionProperty(raw json.RawMessage) (formQuestion, string) {
	var p struct {
		Type        string          `json:"type"`
		Title       string          `json:"title"`
		Description string          `json:"description"`
		OneOf       json.RawMessage `json:"oneOf"`
		Items       json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return formQuestion{}, "did not decode"
	}
	q := formQuestion{header: p.Title}
	q.Prompt = p.Description
	var list json.RawMessage
	allowed := formSingleKeys
	switch p.Type {
	case "string":
		list = p.OneOf
	case "array":
		allowed = formMultiKeys
		items, extra, ok := strictObject(p.Items, formItemsKeys)
		if !ok {
			return formQuestion{}, "is an array with no anyOf items"
		}
		if extra != "" {
			return formQuestion{}, fmt.Sprintf("has items carrying %q, which the card cannot enforce", extra)
		}
		list = items["anyOf"]
		q.AllowMultiple = true
	default:
		return formQuestion{}, fmt.Sprintf("is of type %q", p.Type)
	}
	if _, extra, _ := strictObject(raw, allowed); extra != "" {
		return formQuestion{}, fmt.Sprintf("carries %q, which the card cannot enforce", extra)
	}
	var raws []json.RawMessage
	if json.Unmarshal(list, &raws) != nil || len(raws) == 0 {
		return formQuestion{}, "offers no options"
	}
	seen := map[string]bool{}
	for _, r := range raws {
		_, extra, ok := strictObject(r, formOptionKeys)
		if !ok {
			return formQuestion{}, "has an option that is not an object"
		}
		if extra != "" {
			return formQuestion{}, fmt.Sprintf("has an option carrying %q, which the card cannot enforce", extra)
		}
		var o enumOption
		if json.Unmarshal(r, &o) != nil {
			return formQuestion{}, "has an option that did not decode"
		}
		var id string
		if json.Unmarshal(o.Const, &id) != nil || id == "" {
			return formQuestion{}, "has an option with no string const"
		}
		if seen[id] {
			return formQuestion{}, fmt.Sprintf("offers %q twice", id)
		}
		seen[id] = true
		label := o.Title
		if label == "" {
			label = id
		}
		q.Options = append(q.Options, AskOption{ID: id, Label: label, Description: o.Description})
	}
	return q, ""
}

// routeElicitation is claude's elicitation/create (§ B4): a mode it never
// declared is declined, a form it cannot render cancelled — which aborts the
// tool rather than letting it run on empty answers (t3code 4aead9e6d9) — and
// an answerable form raised as a question.
func (c *Client) routeElicitation(msg *Message) {
	mode, req, why := parseElicitation(msg.Params)
	if why == "" {
		if bad := c.requestSessionUnknown(msg.Params); bad != "" {
			_ = c.conn.Reply(msg.ID, elicitationResult("cancel", nil))
			c.reportRefused(RefusedRequest{Method: msg.Method, Kind: RefusedRequestSession, Answer: "cancel", Detail: bad})
			return
		}
	}
	switch {
	case why == errFormMalformed:
		// Not a request craze can read at all: withdrawn, as an unrenderable
		// form is — a decline would let the tool run on empty answers.
		_ = c.conn.Reply(msg.ID, elicitationResult("cancel", nil))
		c.reportRefused(RefusedRequest{Method: msg.Method, Kind: RefusedElicitationForm, Answer: "cancel", Detail: why})
	case mode != "form":
		_ = c.conn.Reply(msg.ID, elicitationResult("decline", nil))
		c.reportRefused(RefusedRequest{Method: msg.Method, Kind: RefusedElicitationMode, Answer: "decline", Detail: fmt.Sprintf("mode %q", mode)})
	case why != "":
		_ = c.conn.Reply(msg.ID, elicitationResult("cancel", nil))
		c.reportRefused(RefusedRequest{Method: msg.Method, Kind: RefusedElicitationForm, Answer: "cancel", Detail: why})
	default:
		c.dispatch(msg, RequestParams{Ask: &req}, func(in *pendingReq) { c.handleAskQuestion(in, req) })
	}
}

// elicitationOutcome is a question decision as an elicitation reply: accept
// with each answered question's pick (a const, or a list of them for a
// multi-select) and its free text; Skip is decline; a cancel is cancel. Only
// options the form offered are sent, and text only for a question that takes
// it.
func elicitationOutcome(req AskQuestionRequest, dec AskDecision) json.RawMessage {
	switch {
	case dec.Cancelled:
		return elicitationResult("cancel", nil)
	case dec.Skip:
		return elicitationResult("decline", nil)
	}
	content := map[string]any{}
	for _, q := range req.Questions {
		picked := knownOptionIDs(q, dec.Answers[q.ID])
		switch {
		case q.AllowMultiple && len(picked) > 0:
			content[q.ID] = picked
		case len(picked) > 0:
			content[q.ID] = picked[0]
		}
		if !q.AllowText {
			continue
		}
		if text := strings.TrimSpace(dec.Text[q.ID]); text != "" {
			content[q.ID+"_custom"] = text
		}
	}
	return elicitationResult("accept", content)
}

// ------------------------------------------------------------ steering

// steer is claude's Interject (§ B7): _session/steering with the text as one
// block, always with idleBehavior promptRequired. injected is success, and
// tells the interjection handler — on the read loop, ahead of whatever the
// steered turn sends next — so the transcript shows the text where it went in.
func (c *Client) steer(ctx context.Context, sid, text, id string) error {
	// The same last word a prompt gets (SetPromptGate): a steer is model work.
	c.mu.Lock()
	gate := c.promptGate
	c.mu.Unlock()
	if gate != nil {
		if err := gate(ctx); err != nil {
			return err
		}
	}
	params := map[string]any{
		"sessionId": sid,
		"prompt":    []ContentBlock{{Type: "text", Text: text}},
		"_meta":     map[string]any{"steering": map[string]any{"idleBehavior": steerPromptRequired}},
	}
	var outcome string
	_, err := c.conn.callReply(ctx, MethodClaudeSteering, params, func(result json.RawMessage) error {
		var r struct {
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal(result, &r); err != nil {
			return fmt.Errorf("%s: %w", MethodClaudeSteering, err)
		}
		outcome = r.Outcome
		if outcome == steerInjected || outcome == steerStartedNewTurn {
			c.mu.Lock()
			h := c.interjectHandler
			c.mu.Unlock()
			if h != nil {
				h(InterjectionNotification{SessionID: sid, Text: text, InterjectionID: id})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	switch outcome {
	case steerInjected:
		return nil
	case steerPromptRequired:
		return ErrSteerIdle
	case steerStartedNewTurn:
		return ErrSteerStartedTurn
	default:
		return fmt.Errorf("acp: steer answered %q", outcome)
	}
}

// ------------------------------------------------------------ withdrawal

// MethodCancelRequest is ACP's $/cancel_request: the agent no longer waits on
// a request it made of the client. claude-agent-acp sends it when the callback
// behind an elicitation or a permission request is aborted (v1/acp-agent.js
// elicitFromClient).
const MethodCancelRequest = "$/cancel_request"

// ErrRequestWithdrawn is the cause a withdrawn request's Arrival.Call ends
// with (context.Cause): its ask closes as withdrawn by the agent, not as
// cancelled by anything of craze's.
var ErrRequestWithdrawn = errors.New("acp: the agent withdrew the request")

// handleCancelRequest is $/cancel_request (review C3 item 6): the matching
// blocking request is answered — by nobody: no reply is written, the agent
// having stopped waiting — and its context ends with ErrRequestWithdrawn, so
// a card parked on it closes as withdrawn. A decision that races it is
// dropped, its Replied told ReplyLostWithdrawn. A request whose handler has
// already claimed its answer, or that was answered, is left alone.
func (c *Client) handleCancelRequest(msg *Message) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if decodeObject(msg.Params, &p) != nil || len(bytes.TrimSpace(p.RequestID)) == 0 {
		c.countDropped()
		return
	}
	c.incomingMu.Lock()
	in, ok := c.incoming[idKey(bytes.TrimSpace(p.RequestID))]
	if !ok || in.replied || in.claimed {
		c.incomingMu.Unlock()
		return
	}
	in.replied = true
	in.repliedBy = ReplyLostWithdrawn
	if in.decide != nil {
		select {
		case in.decide <- cancelDecision(in.method):
		default:
		}
	}
	// The context ends in the same section that marks the request withdrawn
	// (review C3c 4): the registry refuses an answer for an ask whose call has
	// ended withdrawn, so no answer can be taken once delivery is impossible.
	// Ending a context is a leaf operation, as replyIncomingBy's is.
	//
	// What remains, and is accepted (orchestrator's disposition, C3d): the
	// user's answer and Claude's withdrawal crossing on the wire — the answer
	// taken by the registry before this runs, its reply then lost to the
	// withdrawal. The ask's ending stays the answer; the lost delivery is
	// recorded by the journal's diagnostic (AskRegistry.Report). No protocol
	// change carries it.
	if in.withdraw != nil {
		in.withdraw()
	}
	c.incomingMu.Unlock()
	if testAfterWithdraw != nil {
		testAfterWithdraw(in)
	}
}

// testAfterWithdraw runs, when a test sets it, once handleCancelRequest has
// left its locked section.
var testAfterWithdraw func(*pendingReq)
