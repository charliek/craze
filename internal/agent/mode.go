package agent

import "encoding/json"

var modeAliases = map[string][]string{
	"plan":  {"plan", "architect"},
	"ask":   {"ask"},
	"agent": {"agent", "code", "default"},
}

func ResolveMode(want string, available []string) (string, bool) {
	if want == "" {
		return "", false
	}
	aliases := modeAliases[want]
	if aliases == nil {
		aliases = []string{want}
	}
	have := make(map[string]struct{}, len(available))
	for _, id := range available {
		have[id] = struct{}{}
	}
	for _, a := range aliases {
		if _, ok := have[a]; ok {
			return a, true
		}
	}
	return "", false
}

func availableModeIDs(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var parsed struct {
		AvailableModes []struct {
			ID string `json:"id"`
		} `json:"availableModes"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	ids := make([]string, 0, len(parsed.AvailableModes))
	for _, m := range parsed.AvailableModes {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}
