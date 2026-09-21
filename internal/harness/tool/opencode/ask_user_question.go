package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/charliek/craze/internal/harness/tool"
)

// UnansweredText is what a question nobody answered returns: grok-build's
// words (ask_user_question/format.rs:22-34). It is a result, not a failure —
// the model is told to get on with the task — unless the call itself was
// stopped, when nothing will read it and the call is reported aborted.
const UnansweredText = "User declined to answer the questions. Continue with the task using your best judgment, or ask different questions."

// What one ask_user_question call may hold. The card is one screen, and the
// arguments are decoded whole before anything is asked, so both are bounded
// before that (todo_write's maxTodoInputBytes says why).
const (
	maxAskInputBytes = 64 << 10
	maxQuestions     = 8
	maxOptions       = 12
)

// newAskUserQuestion builds ask_user_question (plan 023 §3.4): grok-build's
// tool and schema, with Claude Code's header field, which a card may use as its
// title. It blocks on the person through Env.Asker.
//
// grok-build's description promises an automatic "Other" choice; craze's card
// is options-only in H5, so that sentence is not ported (NOTICE).
func newAskUserQuestion() (tool.Tool, error) {
	desc, err := description("ask_user_question", nil)
	if err != nil {
		return nil, err
	}
	return &askTool{spec: tool.Spec{
		ID:          "ask_user_question",
		Description: desc,
		Parameters: map[string]any{
			"questions": map[string]any{
				"type":        "array",
				"description": "The questions to ask, each with its own options.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{"type": "string", "description": "The question to ask, phrased as a full question."},
						"header":   map[string]any{"type": "string", "description": "Optional. A very short label for the question."},
						"options": map[string]any{
							"type":        "array",
							"description": "The choices for this question.",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"label":       map[string]any{"type": "string", "description": "Option text shown to the user. A few words at most."},
									"description": map[string]any{"type": "string", "description": "What picking this option means or implies."},
								},
								"required": []any{"label"},
							},
						},
						"multi_select": map[string]any{"type": "boolean", "description": "Let the user pick more than one option (default false)."},
					},
					"required": []any{"question", "options"},
				},
			},
		},
		Required: []string{"questions"},
		Kind:     tool.KindAsk,
		// It changes nothing, so ask mode allows it; and it is not Parallel: a
		// person answers one card at a time, and Fantasy runs a step's
		// sequential calls in order.
		ReadOnly: true,
		Truncate: tool.Head,
	}}, nil
}

type askTool struct{ spec tool.Spec }

func (t *askTool) Spec() tool.Spec { return t.spec }

func (t *askTool) Prepare(_ tool.Env, c tool.Call) (tool.Prepared, error) {
	if len(c.Input) > maxAskInputBytes {
		return nil, fmt.Errorf("the arguments are %d bytes, larger than the %d this tool reads; ask fewer, shorter questions", len(c.Input), maxAskInputBytes)
	}
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	raw, _, err := a.array("questions", true)
	if err != nil {
		return nil, err
	}
	switch {
	case len(raw) == 0:
		return nil, fmt.Errorf("questions must hold at least one question")
	case len(raw) > maxQuestions:
		return nil, fmt.Errorf("questions has %d items, more than the %d one call may ask", len(raw), maxQuestions)
	}
	qs := make([]tool.Question, len(raw))
	seen := map[string]bool{}
	for i, r := range raw {
		q, err := parseQuestion(r)
		if err != nil {
			return nil, fmt.Errorf("questions[%d]: %w", i, err)
		}
		// grok-build refuses a repeated question too (mod.rs:371-382): the
		// answer text names each question by its words.
		if seen[q.Question] {
			return nil, fmt.Errorf("questions[%d]: duplicate question text %q", i, q.Question)
		}
		seen[q.Question] = true
		qs[i] = q
	}
	return &askCall{questions: qs}, nil
}

func parseQuestion(raw json.RawMessage) (tool.Question, error) {
	if t := jsonType(raw); t != "object" {
		return tool.Question{}, fmt.Errorf("must be an object, got a JSON %s", t)
	}
	a, err := parseArgs(raw)
	if err != nil {
		return tool.Question{}, err
	}
	text, _, err := a.str("question", true)
	if err != nil {
		return tool.Question{}, err
	}
	if strings.TrimSpace(text) == "" {
		return tool.Question{}, fmt.Errorf("question must not be empty")
	}
	header, _, err := a.strOrNull("header")
	if err != nil {
		return tool.Question{}, err
	}
	multi, _, err := a.boolean("multi_select")
	if err != nil {
		return tool.Question{}, err
	}
	rawOpts, _, err := a.array("options", true)
	if err != nil {
		return tool.Question{}, err
	}
	switch {
	case len(rawOpts) == 0:
		// The card offers the options and nothing else (no free-text answer in
		// H5), so a question without any could never be answered.
		return tool.Question{}, fmt.Errorf("options must hold at least one option")
	case len(rawOpts) > maxOptions:
		return tool.Question{}, fmt.Errorf("options has %d items, more than the %d a question may offer", len(rawOpts), maxOptions)
	}
	opts := make([]tool.QuestionOption, len(rawOpts))
	for i, r := range rawOpts {
		if t := jsonType(r); t != "object" {
			return tool.Question{}, fmt.Errorf("options[%d] must be an object, got a JSON %s", i, t)
		}
		oa, err := parseArgs(r)
		if err != nil {
			return tool.Question{}, fmt.Errorf("options[%d]: %w", i, err)
		}
		label, _, err := oa.str("label", true)
		if err != nil {
			return tool.Question{}, fmt.Errorf("options[%d]: %w", i, err)
		}
		if strings.TrimSpace(label) == "" {
			return tool.Question{}, fmt.Errorf("options[%d]: label must not be empty", i)
		}
		desc, _, err := oa.strOrNull("description")
		if err != nil {
			return tool.Question{}, fmt.Errorf("options[%d]: %w", i, err)
		}
		opts[i] = tool.QuestionOption{Label: label, Description: desc}
	}
	return tool.Question{Question: text, Header: header, Options: opts, MultiSelect: multi}, nil
}

type askCall struct{ questions []tool.Question }

func (c *askCall) Request() tool.Request {
	title := c.questions[0].Question
	if n := len(c.questions); n > 1 {
		title += " (+" + strconv.Itoa(n-1) + " more)"
	}
	return tool.Request{Title: title}
}

// Run asks and waits, for as long as the person takes: there is no timeout
// (D-52). What is shown is redacted first, with the call's redactor — the
// questions are the model's words about files it has read, and the asker's
// side of the seam has no redactor of its own; the answer goes back to the
// model in the model's own words, by index (tool.Answers).
func (c *askCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if ctx.Err() != nil {
		return aborted()
	}
	if env.Asker == nil {
		return tool.Result{Text: UnansweredText}
	}
	ans, err := env.Asker.AskQuestion(ctx, redactQuestions(env, c.questions))
	switch {
	case err != nil && askStopped(ctx, env):
		return unanswered(ctx, env, UnansweredText)
	case err != nil:
		return tool.Result{Text: "The question could not be asked: " + err.Error(), IsError: true, Class: tool.ClassToolError}
	case !ans.Answered:
		return unanswered(ctx, env, UnansweredText)
	}
	return tool.Result{Text: answerText(c.questions, ans.Picked)}
}

// redactQuestions is a copy of qs with every string redacted.
func redactQuestions(env tool.Env, qs []tool.Question) []tool.Question {
	out := make([]tool.Question, len(qs))
	for i, q := range qs {
		o := tool.Question{
			Question:    env.Redactor.String(q.Question),
			Header:      env.Redactor.String(q.Header),
			MultiSelect: q.MultiSelect,
			Options:     make([]tool.QuestionOption, len(q.Options)),
		}
		for j, opt := range q.Options {
			o.Options[j] = tool.QuestionOption{Label: env.Redactor.String(opt.Label), Description: env.Redactor.String(opt.Description)}
		}
		out[i] = o
	}
	return out
}

// answerText is opencode's answer (question.ts:33-46): each question with
// the labels picked for it, "Unanswered" for one with none. An index the
// question does not offer is dropped rather than trusted.
func answerText(qs []tool.Question, picked [][]int) string {
	parts := make([]string, len(qs))
	for i, q := range qs {
		var labels []string
		if i < len(picked) {
			for _, n := range picked[i] {
				if n >= 0 && n < len(q.Options) {
					labels = append(labels, q.Options[n].Label)
				}
			}
		}
		answer := "Unanswered"
		if len(labels) > 0 {
			answer = strings.Join(labels, ", ")
		}
		parts[i] = fmt.Sprintf("%q=%q", q.Question, answer)
	}
	return "User has answered your questions: " + strings.Join(parts, ", ") +
		". You can now continue with the user's answers in mind."
}

// askStopped reports whether the call is over for a reason of the session's own:
// its context is done, or the session is closing. An ask that ends while either
// holds was not declined by anyone, whatever outcome the asker read — a close
// can resolve the ask before it cancels the context (plan 023 §3.5).
func askStopped(ctx context.Context, env tool.Env) bool {
	if ctx.Err() != nil {
		return true
	}
	select {
	case <-env.Closing: // a nil channel never closes
		return true
	default:
		return false
	}
}

// unanswered is the result of an ask nobody answered: text for the model to
// carry on from, or, when the call was stopped, the same text reported as
// aborted — no request follows a stopped call, so nothing reads it, and the
// card shows the call as cut short rather than as answered.
func unanswered(ctx context.Context, env tool.Env, text string) tool.Result {
	if askStopped(ctx, env) {
		return tool.Result{Text: text, IsError: true, Class: tool.ClassAborted}
	}
	return tool.Result{Text: text}
}
