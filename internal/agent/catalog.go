package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

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
		Config:       parseConfigOptions(res.ConfigOptions),
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

func parseConfigOptions(raw json.RawMessage) []ConfigOption {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]ConfigOption, 0, len(items))
	for _, item := range items {
		opt, ok := parseConfigOption(item)
		if !ok {
			continue
		}
		out = append(out, opt)
	}
	return out
}

func parseConfigOption(raw json.RawMessage) (ConfigOption, bool) {
	var parsed struct {
		ID           string          `json:"id"`
		Name         string          `json:"name"`
		Category     string          `json:"category"`
		Type         string          `json:"type"`
		CurrentValue json.RawMessage `json:"currentValue"`
		Options      json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.ID == "" {
		return ConfigOption{}, false
	}
	name := parsed.Name
	if name == "" {
		name = parsed.ID
	}
	typ := parsed.Type
	opt := ConfigOption{
		ID:       parsed.ID,
		Name:     name,
		Category: parsed.Category,
		Type:     typ,
		Current:  parseConfigCurrent(typ, parsed.CurrentValue),
	}
	if typ == "select" || typ == "" {
		opt.SelectValues = parseSelectValues(parsed.Options)
		if typ == "" {
			opt.Type = "select"
		}
	}
	return opt, true
}

func parseConfigCurrent(typ string, raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if typ == "boolean" {
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			return strconv.FormatBool(b)
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return strconv.FormatBool(b)
	}
	return ""
}

func parseSelectValues(raw json.RawMessage) []SelectValue {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]SelectValue, 0, len(items))
	for _, item := range items {
		var probe struct {
			Value   string          `json:"value"`
			Name    string          `json:"name"`
			Group   string          `json:"group"`
			Options json.RawMessage `json:"options"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			continue
		}
		nested := bytes.TrimSpace(probe.Options)
		if probe.Group != "" || (len(nested) > 0 && nested[0] == '[') {
			out = append(out, parseSelectValues(probe.Options)...)
			continue
		}
		if probe.Value == "" {
			continue
		}
		name := probe.Name
		if name == "" {
			name = probe.Value
		}
		out = append(out, SelectValue{Value: probe.Value, Name: name})
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
