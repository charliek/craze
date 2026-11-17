package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
)

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

// claudeToolCall reads a claude tool update's id and tool name.
func claudeToolCall(update json.RawMessage) (id, name string) {
	var w struct {
		ToolCallID string `json:"toolCallId"`
		Meta       struct {
			ClaudeCode struct {
				ToolName string `json:"toolName"`
			} `json:"claudeCode"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(update, &w); err != nil {
		return "", ""
	}
	return w.ToolCallID, w.Meta.ClaudeCode.ToolName
}

// noteClaudeTool remembers the tool name the adapter stamped on a tool call,
// so the permission request that follows can be checked against it (§ B5).
// The first name a call is given is kept: a later update cannot rename it.
func (c *Client) noteClaudeTool(update json.RawMessage) {
	id, name := claudeToolCall(update)
	if id == "" || name == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.claudeTools[id]; ok {
		return
	}
	if c.claudeTools == nil {
		c.claudeTools = make(map[string]string)
	}
	if len(c.claudeToolOrder) >= claudeToolsCap {
		delete(c.claudeTools, c.claudeToolOrder[0])
		c.claudeToolOrder = c.claudeToolOrder[1:]
	}
	c.claudeTools[id] = name
	c.claudeToolOrder = append(c.claudeToolOrder, id)
}

func (c *Client) claudeToolName(id string) string {
	if id == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claudeTools[id]
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
// One request is not a suspect though its kind is switch_mode: EnterPlanMode's,
// which the adapter marks switch_mode too (tool-calls/reporters/interaction.js),
// when every tool name it carries says EnterPlanMode and nothing else about it
// says exit. Entering plan mode allows nothing; it is an ordinary permission.
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
	var names []string
	for _, n := range []string{
		w.Meta.ClaudeCode.ToolName, w.ToolCall.Meta.ClaudeCode.ToolName, w.ToolCall.Name,
		c.claudeToolName(w.ToolCall.ToolCallID),
	} {
		if n != "" {
			names = append(names, n)
		}
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
	enterOnly := len(names) > 0
	for _, n := range names {
		if n == claudeExitPlanTool {
			exitNamed = true
		}
		if n != claudeEnterPlanTool {
			enterOnly = false
		}
	}
	if !switchMode && !titled && !exitOption && !exitNamed {
		return planExit{}
	}
	if enterOnly && !titled && !exitOption {
		return planExit{}
	}
	pe := planExit{suspected: true, rejectID: rejectOption(req.Options)}
	pe.plan = planExitText(w.ToolCall.RawInput, w.ToolCall.Content)
	pe.why = planExitUnsafe(req.Options, w.ToolCall.Kind, names, pe.plan)
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

// rejectOption is the option an unsafe plan exit is answered with: the one
// named reject when it is of a reject kind, else the first option of kind
// reject_once (then reject_always) whose id says nothing of allowing or of
// exiting plan mode. "" when there is none: the request is answered
// cancelled.
func rejectOption(opts []PermissionOption) string {
	rejecting := func(o PermissionOption) bool {
		return (o.Kind == KindRejectOnce || o.Kind == KindRejectAlways) &&
			o.OptionID != "" &&
			!strings.HasPrefix(o.OptionID, claudeExitPlanPrefix) &&
			!strings.HasPrefix(strings.ToLower(o.OptionID), "allow")
	}
	for _, o := range opts {
		if o.OptionID == ClaudeExitPlanReject && rejecting(o) {
			return o.OptionID
		}
	}
	for _, kind := range []string{KindRejectOnce, KindRejectAlways} {
		for _, o := range opts {
			if o.Kind == kind && rejecting(o) {
				return o.OptionID
			}
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

// parseElicitation reads a claude elicitation/create (§ B4). mode is the
// request's mode; for a form, req is the questions it asks, and why says what
// made a form unanswerable ("" when it is not). A form is answerable only
// when every property is a question_<i> — a string with a oneOf of
// {const,title} options, or an array whose items are an anyOf of them — or a
// question_<i>_custom string beside its question_<i>.
func parseElicitation(params json.RawMessage) (mode string, req AskQuestionRequest, why string) {
	var w struct {
		Mode            string          `json:"mode"`
		ToolCallID      string          `json:"toolCallId"`
		Message         string          `json:"message"`
		RequestedSchema json.RawMessage `json:"requestedSchema"`
	}
	if err := json.Unmarshal(params, &w); err != nil {
		return "", AskQuestionRequest{}, "the request did not decode"
	}
	mode = w.Mode
	if mode == "" {
		// ACP's form mode is the default when a request names none.
		mode = "form"
	}
	if mode != "form" {
		return mode, AskQuestionRequest{}, ""
	}
	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(w.RequestedSchema, &schema); err != nil || schema.Type != "object" {
		return mode, AskQuestionRequest{}, "the form's schema is not an object"
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
			if !isStringProperty(raw) {
				return mode, AskQuestionRequest{}, fmt.Sprintf("property %q is not a text field", key)
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
		Type        string       `json:"type"`
		Title       string       `json:"title"`
		Description string       `json:"description"`
		OneOf       []enumOption `json:"oneOf"`
		Items       *struct {
			AnyOf []enumOption `json:"anyOf"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return formQuestion{}, "did not decode"
	}
	var opts []enumOption
	q := formQuestion{header: p.Title}
	q.Prompt = p.Description
	switch p.Type {
	case "string":
		if p.Items != nil {
			return formQuestion{}, "is a string with items"
		}
		opts = p.OneOf
	case "array":
		if p.Items == nil || len(p.OneOf) > 0 {
			return formQuestion{}, "is an array with no anyOf items"
		}
		opts = p.Items.AnyOf
		q.AllowMultiple = true
	default:
		return formQuestion{}, fmt.Sprintf("is of type %q", p.Type)
	}
	if len(opts) == 0 {
		return formQuestion{}, "offers no options"
	}
	seen := map[string]bool{}
	for _, o := range opts {
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
	switch {
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
