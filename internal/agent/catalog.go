package agent

import (
	"encoding/json"
	"fmt"

	"github.com/charliek/craze/internal/acp"
)

func parseModels(raw json.RawMessage) (current string, models []ModelInfo) {
	if len(raw) == 0 {
		return "", nil
	}
	var parsed struct {
		CurrentModelID  string `json:"currentModelId"`
		AvailableModels []struct {
			ModelID string `json:"modelId"`
			Name    string `json:"name"`
		} `json:"availableModels"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", nil
	}
	current = parsed.CurrentModelID
	for _, m := range parsed.AvailableModels {
		if m.ModelID == "" {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ModelID
		}
		models = append(models, ModelInfo{ID: m.ModelID, Name: name})
	}
	return current, models
}

func parseModes(raw json.RawMessage) (current string, modes []ModeInfo) {
	if len(raw) == 0 {
		return "", nil
	}
	var parsed struct {
		CurrentModeID  string `json:"currentModeId"`
		AvailableModes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"availableModes"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", nil
	}
	current = parsed.CurrentModeID
	for _, m := range parsed.AvailableModes {
		if m.ID == "" {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		modes = append(modes, ModeInfo{ID: m.ID, Name: name})
	}
	return current, modes
}

func snapshotFromNew(res *acp.NewSessionResult) Snapshot {
	if res == nil {
		return Snapshot{}
	}
	curModel, models := parseModels(res.Models)
	curMode, modes := parseModes(res.Modes)
	return Snapshot{
		Models:       models,
		Modes:        modes,
		CurrentModel: curModel,
		CurrentMode:  curMode,
	}
}

func commandsFromUpdate(cmds []acp.AvailableCommand) []CommandInfo {
	out := make([]CommandInfo, 0, len(cmds))
	for _, c := range cmds {
		if c.Name == "" {
			continue
		}
		out = append(out, CommandInfo{Name: c.Name, Description: c.Description})
	}
	return out
}

func NextModeID(snap Snapshot) string {
	if len(snap.Modes) == 0 {
		return ""
	}
	idx := -1
	for i, m := range snap.Modes {
		if m.ID == snap.CurrentMode {
			idx = i
			break
		}
	}
	if idx < 0 {
		return snap.Modes[0].ID
	}
	return snap.Modes[(idx+1)%len(snap.Modes)].ID
}

func MatchModel(snap Snapshot, raw string) (string, error) {
	want := normalizeIdent(raw)
	if want == "" {
		return "", fmt.Errorf("empty model")
	}
	for _, m := range snap.Models {
		if normalizeIdent(m.ID) == want {
			return m.ID, nil
		}
	}
	var hits []string
	for _, m := range snap.Models {
		if normalizeIdent(m.Name) == want {
			hits = append(hits, m.ID)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("unknown model %q", raw)
	default:
		return "", fmt.Errorf("ambiguous model %q", raw)
	}
}

func normalizeIdent(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c == ' ' || c == '_' {
			c = '-'
		}
		out = append(out, c)
	}
	return string(out)
}
