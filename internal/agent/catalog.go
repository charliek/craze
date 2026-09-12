package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

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
	current = sanitizeText(parsed.CurrentModelID)
	for _, m := range parsed.AvailableModels {
		id := sanitizeText(m.ModelID)
		if id == "" {
			continue
		}
		name := sanitizeText(m.Name)
		if name == "" {
			name = id
		}
		models = append(models, ModelInfo{ID: id, Name: name})
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
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"availableModes"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", nil
	}
	current = sanitizeText(parsed.CurrentModeID)
	for _, m := range parsed.AvailableModes {
		id := sanitizeText(m.ID)
		if id == "" {
			continue
		}
		name := sanitizeText(m.Name)
		if name == "" {
			name = id
		}
		modes = append(modes, ModeInfo{ID: id, Name: name, Description: sanitizeText(m.Description)})
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
		name := sanitizeText(c.Name)
		if name == "" {
			continue
		}
		out = append(out, CommandInfo{Name: name, Description: sanitizeText(c.Description)})
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
	id := sanitizeText(parsed.ID)
	name := sanitizeText(parsed.Name)
	if name == "" {
		name = id
	}
	typ := sanitizeText(parsed.Type)
	opt := ConfigOption{
		ID:       id,
		Name:     name,
		Category: sanitizeText(parsed.Category),
		Type:     typ,
		Current:  parseConfigCurrent(typ, parsed.CurrentValue),
	}
	switch typ {
	case "select", "":
		opt.SelectValues = parseSelectValues(parsed.Options)
		opt.Type = "select"
	case "boolean":
		// A boolean is a two-value select wearing another hat, so it is
		// flattened into one here and nothing downstream — the effort
		// heuristics, the model dialog's toggle rows — has to know which shape
		// the agent happened to advertise.
		opt.SelectValues = parseSelectValues(parsed.Options)
		if len(opt.SelectValues) == 0 {
			opt.SelectValues = []SelectValue{{Value: "false", Name: "Off"}, {Value: "true", Name: "On"}}
		}
		opt.Type = "select"
	}
	return opt, true
}

func parseConfigCurrent(typ string, raw json.RawMessage) string {
	_ = typ // every scalar shape is handled the same way
	return sanitizeText(scalarString(raw))
}

// scalarString renders a JSON scalar as the string craze sends back on the
// wire. Cursor advertises its fast toggle as strings ("false"/"true") in every
// capture craze has; accepting a bare JSON true or a number is hardening
// against that shape changing, not a fix for anything observed.
func scalarString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return strconv.FormatBool(b)
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
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
			Value   json.RawMessage `json:"value"`
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
		value := sanitizeText(scalarString(probe.Value))
		if value == "" {
			continue
		}
		name := sanitizeText(probe.Name)
		if name == "" {
			name = value
		}
		out = append(out, SelectValue{Value: value, Name: name})
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
	models := snapshotModels(snap)
	for _, m := range models {
		if normalizeIdent(m.ID) == want {
			return m.ID, nil
		}
	}
	var hits []string
	for _, m := range models {
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

func snapshotModels(snap Snapshot) []ModelInfo {
	if len(snap.Models) > 0 {
		return append([]ModelInfo(nil), snap.Models...)
	}
	for _, c := range snap.Config {
		if c.Category != "model" {
			continue
		}
		out := make([]ModelInfo, 0, len(c.SelectValues))
		for _, v := range c.SelectValues {
			if v.Value == "" {
				continue
			}
			name := v.Name
			if name == "" {
				name = v.Value
			}
			out = append(out, ModelInfo{ID: v.Value, Name: name})
		}
		return out
	}
	return nil
}

func modelDisplayName(m ModelInfo) string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

func isGrokModel(m ModelInfo) bool {
	return strings.Contains(strings.ToLower(m.ID), "grok") ||
		strings.Contains(strings.ToLower(m.Name), "grok")
}

func sortModelBucket(ms []ModelInfo) {
	sort.Slice(ms, func(i, j int) bool {
		ni, nj := modelDisplayName(ms[i]), modelDisplayName(ms[j])
		if ni != nj {
			return ni < nj
		}
		return ms[i].ID < ms[j].ID
	})
}

// OrderModels returns advertised models with the current model first, then
// any id/name matching grok, then the rest by display name and id.
func OrderModels(snap Snapshot) []ModelInfo {
	models := snapshotModels(snap)
	if len(models) == 0 {
		return nil
	}
	var cur, grok, rest []ModelInfo
	seenCurrent := false
	for _, m := range models {
		if !seenCurrent && snap.CurrentModel != "" && m.ID == snap.CurrentModel {
			cur = append(cur, m)
			seenCurrent = true
			continue
		}
		if isGrokModel(m) {
			grok = append(grok, m)
			continue
		}
		rest = append(rest, m)
	}
	sortModelBucket(grok)
	sortModelBucket(rest)
	out := make([]ModelInfo, 0, len(models))
	out = append(out, cur...)
	out = append(out, grok...)
	out = append(out, rest...)
	return out
}

func isEffortSelect(opt ConfigOption) bool {
	if opt.Type != "" && opt.Type != "select" {
		return false
	}
	if len(opt.SelectValues) == 0 {
		return false
	}
	id := strings.ToLower(opt.ID)
	name := strings.ToLower(opt.Name)
	return strings.Contains(id, "effort") || strings.Contains(id, "reasoning") ||
		strings.Contains(name, "effort") || strings.Contains(name, "reasoning")
}

func effortRank(opt ConfigOption) int {
	if opt.Category == "model_option" {
		return 0
	}
	if strings.EqualFold(opt.ID, "effort") {
		return 1
	}
	if opt.Category == "thought_level" {
		return 2
	}
	return 3
}

// EffortOption returns the advertised effort/reasoning select, preferring
// category model_option, then id effort, then thought_level, else the first match.
// Which options qualify is the session provider's call.
func EffortOption(snap Snapshot) *ConfigOption {
	p := snap.Provider.provider()
	bestI := -1
	bestRank := 4
	for i, opt := range snap.Config {
		if !p.isEffortOption(opt) {
			continue
		}
		r := effortRank(opt)
		if bestI < 0 || r < bestRank {
			bestI = i
			bestRank = r
		}
	}
	if bestI < 0 {
		return nil
	}
	opt := snap.Config[bestI]
	return &opt
}

// FastOption returns the advertised fast toggle — cursor's model_config
// `fast` — or nil when the session offers none.
func FastOption(snap Snapshot) *ConfigOption {
	p := snap.Provider.provider()
	for _, c := range snap.Config {
		if p.isFastOption(c) {
			opt := c
			return &opt
		}
	}
	return nil
}

// fastOffName is the name cursor gives the fast toggle's off value. The values
// themselves are cursor's spelling ("false"/"true") and craze sends them back
// verbatim, so the name — not the value — is what says which way is on.
const fastOffName = "off"

// FastOnOff splits the fast toggle's advertised values into the one that means
// off and the one that means on. An option that names neither falls back to
// its own order, which is how every capture lists them.
func FastOnOff(opt *ConfigOption) (off, on string, ok bool) {
	if opt == nil || len(opt.SelectValues) < 2 {
		return "", "", false
	}
	for i, v := range opt.SelectValues {
		if !strings.EqualFold(strings.TrimSpace(v.Name), fastOffName) {
			continue
		}
		other := 0
		if i == 0 {
			other = 1
		}
		return v.Value, opt.SelectValues[other].Value, true
	}
	return opt.SelectValues[0].Value, opt.SelectValues[1].Value, true
}

// FastOn reports whether the session's fast toggle exists and is on, which is
// the only case the status row names it.
func FastOn(snap Snapshot) bool {
	opt := FastOption(snap)
	_, on, ok := FastOnOff(opt)
	return ok && opt.Current == on
}

// ModelConfigOption returns the first config option with category "model".
func ModelConfigOption(snap Snapshot) *ConfigOption {
	for _, c := range snap.Config {
		if c.Category == "model" {
			opt := c
			return &opt
		}
	}
	return nil
}

func matchEffortValue(opt *ConfigOption, raw string) (string, bool) {
	if opt == nil {
		return "", false
	}
	want := strings.TrimSpace(raw)
	if want == "" {
		return "", false
	}
	for _, v := range opt.SelectValues {
		if strings.EqualFold(v.Value, want) {
			return v.Value, true
		}
	}
	return "", false
}

func splitLastToken(s string) (rest, last string) {
	s = strings.TrimSpace(s)
	i := strings.LastIndexAny(s, " \t")
	if i < 0 {
		return "", s
	}
	return strings.TrimSpace(s[:i]), s[i+1:]
}

// SplitModelEffort splits `/model` args: if the last token is an advertised
// effort value, it is effort and the remainder is the model. Otherwise the
// whole string is the model.
func SplitModelEffort(args string, snap Snapshot) (model, effort string) {
	args = strings.TrimSpace(args)
	if args == "" {
		return "", ""
	}
	rest, last := splitLastToken(args)
	if rest == "" {
		return args, ""
	}
	if val, ok := matchEffortValue(EffortOption(snap), last); ok {
		return rest, val
	}
	return args, ""
}
