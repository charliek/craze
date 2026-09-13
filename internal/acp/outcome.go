package acp

import (
	"bytes"
	"encoding/json"
)

func PickKind(opts []PermissionOption, kind string) (string, bool) {
	for _, o := range opts {
		if o.Kind == kind && o.OptionID != "" {
			return o.OptionID, true
		}
	}
	return "", false
}

// PickYoloAllow selects allow_always, then allow_once, using the option's own id.
func PickYoloAllow(opts []PermissionOption) (string, bool) {
	if id, ok := PickKind(opts, KindAllowAlways); ok {
		return id, true
	}
	return PickKind(opts, KindAllowOnce)
}

func optionIDInRequest(opts []PermissionOption, id string) bool {
	if id == "" {
		return false
	}
	for _, o := range opts {
		if o.OptionID == id {
			return true
		}
	}
	return false
}

type outcomeField struct {
	key   string
	value any
}

// outcome builds every reply body craze sends to a blocking agent request:
// {"outcome":{"outcome":"<name>", …fields}} with the fields in the order
// given. This is the single place the nesting lives, so switching to the
// fallback shape recorded in plan §9 is a change to this function alone.
func outcome(name string, fields ...outcomeField) json.RawMessage {
	var b bytes.Buffer
	b.WriteString(`{"outcome":{"outcome":`)
	writeJSON(&b, name)
	for _, f := range fields {
		b.WriteByte(',')
		writeJSON(&b, f.key)
		b.WriteByte(':')
		writeJSON(&b, f.value)
	}
	b.WriteString(`}}`)
	return b.Bytes()
}

func writeJSON(b *bytes.Buffer, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		b.WriteString("null")
		return
	}
	b.Write(raw)
}

func selectedOutcome(optionID string) json.RawMessage {
	return outcome("selected", outcomeField{"optionId", optionID})
}

func cancelledOutcome() json.RawMessage {
	return outcome("cancelled")
}

func grokFlatOutcome(name string, fields ...outcomeField) json.RawMessage {
	var b bytes.Buffer
	b.WriteString(`{"outcome":`)
	writeJSON(&b, name)
	for _, f := range fields {
		b.WriteByte(',')
		writeJSON(&b, f.key)
		b.WriteByte(':')
		writeJSON(&b, f.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func cancelledResult(d DialectID, method string) json.RawMessage {
	if d == DialectGrok && (isGrokAskMethod(method) || isGrokPlanMethod(method)) {
		return grokFlatOutcome("cancelled")
	}
	return cancelledOutcome()
}

type askAnswer struct {
	QuestionID        string   `json:"questionId"`
	SelectedOptionIDs []string `json:"selectedOptionIds"`
}

// askOutcome answers every question of the request in request order, keeping
// only option ids the request itself offered.
func askOutcome(d DialectID, req AskQuestionRequest, dec AskDecision) json.RawMessage {
	if d == DialectGrok {
		return grokAskOutcome(req, dec)
	}
	switch {
	case dec.Cancelled:
		return cancelledOutcome()
	case dec.Skip:
		return outcome("skipped")
	}
	answers := make([]askAnswer, 0, len(req.Questions))
	for _, q := range req.Questions {
		answers = append(answers, askAnswer{
			QuestionID:        q.ID,
			SelectedOptionIDs: knownOptionIDs(q, dec.Answers[q.ID]),
		})
	}
	return outcome("answered", outcomeField{"answers", answers})
}

func grokAskOutcome(req AskQuestionRequest, dec AskDecision) json.RawMessage {
	switch {
	case dec.Cancelled:
		return grokFlatOutcome("cancelled")
	case dec.Skip:
		return grokFlatOutcome("skip_interview")
	}
	var b bytes.Buffer
	b.WriteString(`{"outcome":"accepted","answers":{`)
	seen := make(map[string]struct{})
	first := true
	for _, q := range req.Questions {
		key := q.Prompt
		if key == "" {
			key = q.ID
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if !first {
			b.WriteByte(',')
		}
		first = false
		picked := dec.Answers[q.ID]
		if len(picked) == 0 {
			picked = dec.Answers[key]
		}
		writeJSON(&b, key)
		b.WriteByte(':')
		writeJSON(&b, knownOptionIDs(q, picked))
	}
	b.WriteString(`}}`)
	return b.Bytes()
}

func knownOptionIDs(q AskQuestion, picked []string) []string {
	out := []string{}
	for _, id := range picked {
		for _, o := range q.Options {
			if o.ID != "" && o.ID == id {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// AskAutoAnswers picks the first option of each question, the reply craze
// sends when nothing interactive is listening.
func AskAutoAnswers(req AskQuestionRequest) map[string][]string {
	answers := make(map[string][]string, len(req.Questions))
	for _, q := range req.Questions {
		if len(q.Options) > 0 && q.Options[0].ID != "" {
			answers[q.ID] = []string{q.Options[0].ID}
		} else {
			answers[q.ID] = nil
		}
	}
	return answers
}

func planOutcome(d DialectID, dec PlanDecision) json.RawMessage {
	if d == DialectGrok {
		switch {
		case dec.Cancelled:
			return grokFlatOutcome("cancelled")
		case dec.Accept:
			return grokFlatOutcome("approved")
		default:
			return grokFlatOutcome("abandoned")
		}
	}
	switch {
	case dec.Cancelled:
		return cancelledOutcome()
	case dec.Accept:
		return outcome("accepted")
	default:
		return outcome("rejected")
	}
}

// todosAcceptedOutcome echoes the merged list back to cursor.
func todosAcceptedOutcome(todos []TodoItem) json.RawMessage {
	if todos == nil {
		todos = []TodoItem{}
	}
	return outcome("accepted", outcomeField{"todos", todos})
}

// taskCompletedOutcome echoes the receipt's agentId and durationMs, which are
// empty/zero when the request omitted them.
func taskCompletedOutcome(agentID string, durationMs int) json.RawMessage {
	return outcome("completed",
		outcomeField{"agentId", agentID},
		outcomeField{"durationMs", durationMs},
	)
}
