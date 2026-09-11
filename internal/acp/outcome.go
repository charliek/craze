package acp

func PickKind(opts []PermissionOption, kind string) (string, bool) {
	for _, o := range opts {
		if o.Kind == kind && o.OptionID != "" {
			return o.OptionID, true
		}
	}
	for _, o := range opts {
		if o.OptionID == kind {
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

func selectedOutcome(optionID string) map[string]any {
	return map[string]any{
		"outcome": map[string]any{
			"outcome":  "selected",
			"optionId": optionID,
		},
	}
}

func cancelledOutcome() map[string]any {
	return map[string]any{
		"outcome": map[string]any{
			"outcome": "cancelled",
		},
	}
}

func askAnsweredOutcome(req AskQuestionRequest) map[string]any {
	answers := make([]map[string]any, 0, len(req.Questions))
	for _, q := range req.Questions {
		selected := []string{}
		if len(q.Options) > 0 && q.Options[0].ID != "" {
			selected = []string{q.Options[0].ID}
		}
		answers = append(answers, map[string]any{
			"questionId":        q.ID,
			"selectedOptionIds": selected,
		})
	}
	return map[string]any{
		"outcome": map[string]any{
			"outcome": "answered",
			"answers": answers,
		},
	}
}

func planAcceptedOutcome() map[string]any {
	return map[string]any{
		"outcome": map[string]any{
			"outcome": "accepted",
		},
	}
}
