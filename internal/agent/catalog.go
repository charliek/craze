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
	return snapshotFromNewProvider(res, CursorProvider(), nil)
}

func snapshotFromNewProvider(res *acp.NewSessionResult, p Provider, init *acp.InitializeResult) Snapshot {
	if res == nil {
		return Snapshot{}
	}
	modelsRaw := res.Models
	if _, models := parseModels(modelsRaw); len(models) == 0 && init != nil {
		if fallback := init.ModelState(); len(fallback) > 0 {
			modelsRaw = fallback
		}
	}
	curModel, models := parseModels(modelsRaw)
	curMode, modes := parseModes(res.Modes)
	if len(modes) == 0 {
		if modes = p.FallbackModes(); len(modes) > 0 && curMode == "" {
			curMode = modes[0].ID
		}
	}
	config := parseConfigOptions(res.ConfigOptions)
	if curModel == "" {
		// A provider that keeps its model in a config option and names no
		// current model in its models block — the shape a session/set_model-less
		// agent has — would otherwise start on no model at all while advertising
		// exactly which one it is on (r25 finding 3). The models block keeps
		// precedence whenever it names one: two disagreeing sources are a fact
		// about the agent, and the one craze has always believed is the models
		// block. onUpdate's config arm will not flip it afterwards either — an
		// update that re-lists the option unchanged is not a change.
		if opt := ModelConfigOptionIn(config); opt != nil && opt.Current != "" {
			curModel = opt.Current
		}
	}
	return Snapshot{
		Models:       models,
		Modes:        modes,
		Config:       config,
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

// parseConfigOptions is the tolerant parse, and it is the one the agent's own
// updates and the session/new and session/load results go through: a member it
// cannot read as an option is skipped and the rest are kept, and anything that
// is not an array at all is no catalog. That is unchanged by plan 025. An update
// has no caller to be answered — refusing it whole would leave the session on
// the catalog before it for no one's benefit — whereas a settings reply does,
// and is held to parseReplyConfigOptions instead.
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

// parseReplyConfigOptions is a settings reply's catalog, which is all or
// nothing (plan 025 design 1, "malformed is an error"; astra r2 item 7): every
// member of the array has to be an option, or the reply is malformed — the
// error wraps acp.ErrBadCatalog — and nothing of it is installed. raw is the
// array acp handed over (acp.ConfigCatalog.Options), already known to be one.
//
// A member is malformed exactly when parseConfigOption cannot read it, which is
// when it is not a JSON object (a number, a string, a list — or null, which
// reads as an object with no id), when one of the fields it reads as text
// (id, name, category, type) is of another JSON type, or when it has no id.
// There is nothing else: parseConfigOption skips no option it can read. One of
// a type craze draws no control for is kept, with that type and no values, and
// an unknown field is ignored, as is a currentValue that is no scalar (read as
// ""). What is inside an option's own value list is the option's: a value that
// cannot be read is left out of it by parseSelectValues, on this path as on the
// push path, and the option stands.
func parseReplyConfigOptions(raw json.RawMessage) ([]ConfigOption, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%w: %v", acp.ErrBadCatalog, err)
	}
	out := make([]ConfigOption, 0, len(items))
	for i, item := range items {
		opt, ok := parseConfigOption(item)
		if !ok {
			// The member itself stays out of the error, which a client may draw:
			// the journal has the wire.
			return nil, fmt.Errorf("%w: member %d is not an option", acp.ErrBadCatalog, i)
		}
		out = append(out, opt)
	}
	return out, nil
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

// effortRank orders the options isEffortSelect admits. The exact id `effort`
// comes first, ahead of every category: cursor files `thinking` under
// thought_level beside its effort select (plan 025 X1.2), so a category is
// no evidence that an option is the effort, and the one name that is
// unambiguous wins whatever the agent's categories say. isEffortSelect already
// keeps `thinking` out by its id and name; this is the second lock on the same
// door, so a future spelling that slipped past the first cannot outrank the
// real control.
func effortRank(opt ConfigOption) int {
	if strings.EqualFold(opt.ID, "effort") {
		return 0
	}
	if opt.Category == "model_option" {
		return 1
	}
	if opt.Category == "thought_level" {
		return 2
	}
	return 3
}

// EffortOption returns the advertised effort/reasoning select, preferring id
// effort, then category model_option, then thought_level, else the first match.
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

// fastOffName is the name cursor gives the fast toggle's off value, and
// fastOffValue the value it gives it. The values are cursor's spelling and
// craze sends them back verbatim, so what a value means has to be read off its
// name or its own spelling — never off its position in the list.
const (
	fastOffName  = "off"
	fastOffValue = "false"
)

// FastOnOff splits the fast toggle's advertised values into the one that means
// off and the one that means on. Off is the value the agent names "off" or
// spells "false"; whatever else it advertises is on. Position is not evidence:
// an agent that lists its on value first, or renames both, would otherwise
// invert the row and send "on" as off.
func FastOnOff(opt *ConfigOption) (off, on string, ok bool) {
	if opt == nil {
		return "", "", false
	}
	for _, v := range opt.SelectValues {
		switch {
		case isFastOff(v):
			if off == "" {
				off = v.Value
			}
		case on == "":
			on = v.Value
		}
	}
	// Without an on value there is nothing to switch to, so the row falls back
	// to the raw values rather than guessing one of them means on.
	return off, on, on != ""
}

// isFastOff recognises the toggle's off value either way the agent can spell
// it, so a singleton or a renamed pair still reads the right way round.
func isFastOff(v SelectValue) bool {
	return strings.EqualFold(strings.TrimSpace(v.Name), fastOffName) ||
		strings.EqualFold(strings.TrimSpace(v.Value), fastOffValue)
}

// FastOn reports whether the session's fast toggle exists and is on, which is
// the only case the status row names it.
func FastOn(snap Snapshot) bool {
	opt := FastOption(snap)
	_, on, ok := FastOnOff(opt)
	return ok && opt.Current == on
}

// modelCategory is the config category a provider keeps its MODEL in, rather
// than behind session/set_model. Changing that option is changing the model,
// which is why a session that has the concept moves Snapshot.CurrentModel with
// it and says both sections in one delta (plan 021 §3.8, r23 finding 3).
const modelCategory = "model"

// ModelConfigOption returns the first config option with category "model".
func ModelConfigOption(snap Snapshot) *ConfigOption {
	return ModelConfigOptionIn(snap.Config)
}

// ModelConfigOptionIn is ModelConfigOption over a bare option list: the config
// a session holds under its own lock, or the one an update just brought, where
// there is no Snapshot to ask. One definition of "the model option", so the
// session that writes CurrentModel from it and the UI that reads it can never
// pick different options.
func ModelConfigOptionIn(cfg []ConfigOption) *ConfigOption {
	for _, c := range cfg {
		if c.Category == modelCategory {
			opt := c
			return &opt
		}
	}
	return nil
}

// IsModelConfigOption reports whether opt is the option a provider keeps its
// model in: what makes SetConfig on it a MODEL change as well as a config one.
func IsModelConfigOption(opt ConfigOption) bool { return opt.Category == modelCategory }

// modeCategory is the category cursor files its mode option under.
const modeCategory = "mode"

// withoutModelOptions is cfg after a model change that brought no catalog of
// its own: the mode option and the model option kept, the model option moved
// to model, and every other option dropped (plan 025 design 2, panel astra 3).
//
// Those others — effort, fast, context, thinking — are per model on cursor,
// and what cfg holds are the PREVIOUS model's: offering them would offer a
// control the new model may not have, or hide one it does, and setting one
// would be refused ("Unknown model config option"). They are gone until the
// next catalog the agent sends, a push or a later reply, brings the new
// model's back.
//
// The model option reads the model the agent has just accepted, rather than
// the value the previous catalog left in it, so that the Model section and the
// Config section of the delta announcing the change say the same thing — and
// so that a later list carrying the old value is read, in arrival order, as
// the change it is. A fresh slice: cfg is left as it was.
func withoutModelOptions(cfg []ConfigOption, model string) []ConfigOption {
	out := make([]ConfigOption, 0, 2)
	for _, opt := range cfg {
		switch {
		case IsModelConfigOption(opt):
			opt.Current = model
			out = append(out, opt)
		case opt.Category == modeCategory:
			out = append(out, opt)
		}
	}
	return out
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
