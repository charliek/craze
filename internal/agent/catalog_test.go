package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/acp"
)

func TestGrokEmptyModesInjected(t *testing.T) {
	snap := snapshotFromNewProvider(&acp.NewSessionResult{
		Models: json.RawMessage(`{"currentModelId":"grok-4.6","availableModels":[{"modelId":"grok-4.6","name":"Grok 4.6"}]}`),
	}, GrokProvider(), nil)
	if len(snap.Modes) != 3 || snap.Modes[0].ID != "default" || snap.Modes[1].ID != "plan" || snap.Modes[2].ID != "ask" {
		t.Fatalf("modes %+v", snap.Modes)
	}
	if snap.CurrentMode != "default" {
		t.Fatalf("current %q", snap.CurrentMode)
	}
	if id, ok := ResolveMode("agent", modeIDs(snap.Modes)); !ok || id != "default" {
		t.Fatalf("agent alias %q %v", id, ok)
	}
	cursor := snapshotFromNew(&acp.NewSessionResult{})
	if len(cursor.Modes) != 0 {
		t.Fatalf("cursor must not inject modes: %+v", cursor.Modes)
	}
}

func TestInitializeModelStateFallback(t *testing.T) {
	init := &acp.InitializeResult{Meta: json.RawMessage(`{"modelState":{"currentModelId":"grok-4.6","availableModels":[{"modelId":"grok-4.6","name":"Grok 4.6"}]}}`)}
	snap := snapshotFromNewProvider(&acp.NewSessionResult{}, GrokProvider(), init)
	if snap.CurrentModel != "grok-4.6" || len(snap.Models) != 1 || snap.Models[0].ID != "grok-4.6" {
		t.Fatalf("fallback %+v / %q", snap.Models, snap.CurrentModel)
	}
}

// TestTheInitialModelCanComeFromTheModelConfigOption is r25 finding 3 at the
// first snapshot a session ever has. An agent that keeps its model in a config
// option and names no current model in its models block would otherwise start
// on no model at all while advertising exactly which one it is on.
//
// The precedence is the one craze has always had: the models block wins
// whenever it names a model, even when the option disagrees with it. Two
// sources that disagree are a fact about the agent, not something to be
// resolved by whichever is read last.
func TestTheInitialModelCanComeFromTheModelConfigOption(t *testing.T) {
	modelOption := json.RawMessage(`[{"id":"model","name":"Model","category":"model","type":"select","currentValue":"composer","options":[{"value":"default","name":"Default"},{"value":"composer","name":"Composer"}]}]`)
	t.Run("no models block", func(t *testing.T) {
		snap := snapshotFromNew(&acp.NewSessionResult{ConfigOptions: modelOption})
		if snap.CurrentModel != "composer" {
			t.Fatalf("the session started on %q, want the model option's value", snap.CurrentModel)
		}
	})
	t.Run("the models block wins when it names one", func(t *testing.T) {
		snap := snapshotFromNew(&acp.NewSessionResult{
			Models:        json.RawMessage(`{"currentModelId":"default","availableModels":[{"modelId":"default","name":"Default"}]}`),
			ConfigOptions: modelOption,
		})
		if snap.CurrentModel != "default" {
			t.Fatalf("the session started on %q, want the models block's value", snap.CurrentModel)
		}
	})
	t.Run("an empty option says nothing", func(t *testing.T) {
		snap := snapshotFromNew(&acp.NewSessionResult{
			ConfigOptions: json.RawMessage(`[{"id":"model","name":"Model","category":"model","type":"select","currentValue":""}]`),
		})
		if snap.CurrentModel != "" {
			t.Fatalf("the session started on %q", snap.CurrentModel)
		}
	})
	t.Run("an ordinary option is not a model", func(t *testing.T) {
		snap := snapshotFromNew(&acp.NewSessionResult{
			ConfigOptions: json.RawMessage(`[{"id":"effort","name":"Effort","category":"thought_level","type":"select","currentValue":"high"}]`),
		})
		if snap.CurrentModel != "" {
			t.Fatalf("an effort option was read as the model: %q", snap.CurrentModel)
		}
	})
}

func TestParseModelsAndModes(t *testing.T) {
	modelsRaw := json.RawMessage(`{"currentModelId":"default","availableModels":[{"modelId":"default","name":"Default"},{"modelId":"composer","name":"Composer"}]}`)
	cur, models := parseModels(modelsRaw)
	if cur != "default" || len(models) != 2 || models[1].ID != "composer" {
		t.Fatalf("%q %+v", cur, models)
	}
	modesRaw := json.RawMessage(`{"currentModeId":"agent","availableModes":[{"id":"agent","name":"Agent"},{"id":"plan","name":"Plan"}]}`)
	curMode, modes := parseModes(modesRaw)
	if curMode != "agent" || len(modes) != 2 {
		t.Fatalf("%q %+v", curMode, modes)
	}
	id, err := MatchModel(Snapshot{Models: models}, "Composer")
	if err != nil || id != "composer" {
		t.Fatalf("match %q %v", id, err)
	}
	next := NextModeID(Snapshot{CurrentMode: "agent", Modes: modes})
	if next != "plan" {
		t.Fatalf("next %q", next)
	}
	next = NextModeID(Snapshot{CurrentMode: "plan", Modes: modes})
	if next != "agent" {
		t.Fatalf("wrap %q", next)
	}
	if got := NextModeID(Snapshot{CurrentMode: "missing", Modes: modes}); got != "agent" {
		t.Fatalf("unknown current should yield first mode, got %q", got)
	}
	if _, err := MatchModel(Snapshot{Models: models}, "nope"); err == nil {
		t.Fatal("expected unknown model")
	}
	if _, err := MatchModel(Snapshot{Models: []ModelInfo{
		{ID: "a", Name: "Twin"},
		{ID: "b", Name: "Twin"},
	}}, "Twin"); err == nil {
		t.Fatal("expected ambiguous model")
	}
}

func TestParseConfigOptions(t *testing.T) {
	raw := json.RawMessage(`[
		{
			"id":"effort",
			"name":"Effort",
			"category":"thought_level",
			"type":"select",
			"currentValue":"medium",
			"options":[{"value":"low","name":"Low"},{"value":"medium","name":"Medium"}]
		},
		{
			"id":"grouped",
			"name":"Grouped",
			"type":"select",
			"currentValue":"a",
			"options":[{"group":"g1","name":"Group 1","options":[{"value":"a","name":"A"},{"value":"b","name":"B"}]}]
		},
		{
			"id":"thinking",
			"name":"Thinking",
			"type":"boolean",
			"currentValue":true
		}
	]`)
	opts := parseConfigOptions(raw)
	if len(opts) != 3 {
		t.Fatalf("len %d", len(opts))
	}
	if opts[0].ID != "effort" || opts[0].Current != "medium" || opts[0].Category != "thought_level" {
		t.Fatalf("%+v", opts[0])
	}
	if len(opts[0].SelectValues) != 2 || opts[0].SelectValues[1].Value != "medium" {
		t.Fatalf("select %+v", opts[0].SelectValues)
	}
	if len(opts[1].SelectValues) != 2 || opts[1].SelectValues[0].Value != "a" || opts[1].SelectValues[0].Name != "A" {
		t.Fatalf("grouped should skip headers: %+v", opts[1].SelectValues)
	}
	// A boolean arrives as a two-value select, so the dialog's toggle rows do
	// not need a second shape.
	if opts[2].Type != "select" || opts[2].Current != "true" {
		t.Fatalf("boolean %+v", opts[2])
	}
	off, on, ok := FastOnOff(&opts[2])
	if !ok || off != "false" || on != "true" {
		t.Fatalf("boolean off/on = %q/%q (%v)", off, on, ok)
	}
}

// TestParseSelectValuesStringifiesScalars is hardening, not a fix: every live
// capture advertises fast as strings. A shape change to bare JSON scalars must
// not silently drop the option's values.
func TestParseSelectValuesStringifiesScalars(t *testing.T) {
	got := parseSelectValues(json.RawMessage(
		`[{"value":false,"name":"Off"},{"value":true,"name":"Fast"},{"value":3,"name":"Three"}]`))
	if len(got) != 3 {
		t.Fatalf("len %d: %+v", len(got), got)
	}
	for i, want := range []string{"false", "true", "3"} {
		if got[i].Value != want {
			t.Fatalf("value %d = %q, want %q", i, got[i].Value, want)
		}
	}
}

// TestFastOptionLiveShape is the fast toggle exactly as cursor advertises it,
// U+200B padding and all.
func TestFastOptionLiveShape(t *testing.T) {
	raw := json.RawMessage(`[{"id":"fast","name":"Fast","description":"Significantly faster",
		"category":"model_config","type":"select","currentValue":"true",
		"options":[{"value":"false","name":"Off"},{"value":"true","name":"Fast\u200b\u200b"}]}]`)
	snap := Snapshot{Config: parseConfigOptions(raw)}
	opt := FastOption(snap)
	if opt == nil {
		t.Fatal("the live fast option should be found")
	}
	if opt.SelectValues[1].Name != "Fast" {
		t.Fatalf("the zero-width padding survived: %q", opt.SelectValues[1].Name)
	}
	off, on, ok := FastOnOff(opt)
	if !ok || off != "false" || on != "true" {
		t.Fatalf("off/on = %q/%q (%v)", off, on, ok)
	}
	if !FastOn(snap) {
		t.Fatal("currentValue true means fast is on")
	}
}

// TestFastOnOffReadsNamesNotOrder: the off value is the one the agent names
// "off" or spells "false", and whatever else it advertises is on. Reading the
// list positionally inverted a renamed pair — "on" then sent the off value —
// and refused a list with only the on value in it.
func TestFastOnOffReadsNamesNotOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		current  string
		values   []SelectValue
		off, on  string
		ok, fast bool
	}{
		{
			// The live shape, which must keep mapping exactly as it did.
			"cursor", "true",
			[]SelectValue{{Value: "false", Name: "Off"}, {Value: "true", Name: "Fast"}},
			"false", "true", true, true,
		},
		{
			"renamed, on listed first", "true",
			[]SelectValue{{Value: "true", Name: "Turbo"}, {Value: "false", Name: "Disabled"}},
			"false", "true", true, true,
		},
		{
			"only the on value advertised", "true",
			[]SelectValue{{Value: "true", Name: "Fast"}},
			"", "true", true, true,
		},
		{
			"only the off value advertised", "false",
			[]SelectValue{{Value: "false", Name: "Off"}},
			"false", "", false, false,
		},
		{
			"named off, spelled otherwise", "yes",
			[]SelectValue{{Value: "no", Name: "Off"}, {Value: "yes", Name: "On"}},
			"no", "yes", true, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := ConfigOption{
				ID: "fast", Name: "Fast", Category: "model_config", Type: "select",
				Current: tc.current, SelectValues: tc.values,
			}
			off, on, ok := FastOnOff(&opt)
			if off != tc.off || on != tc.on || ok != tc.ok {
				t.Fatalf("off/on = %q/%q (%v), want %q/%q (%v)", off, on, ok, tc.off, tc.on, tc.ok)
			}
			if got := FastOn(Snapshot{Config: []ConfigOption{opt}}); got != tc.fast {
				t.Fatalf("FastOn = %v, want %v", got, tc.fast)
			}
		})
	}
}

func modelIDs(ms []ModelInfo) string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	return strings.Join(ids, ",")
}

func TestOrderModels(t *testing.T) {
	snap := Snapshot{
		CurrentModel: "fast",
		Models: []ModelInfo{
			{ID: "other", Name: "Zed"},
			{ID: "grok", Name: "Grok"},
			{ID: "fast", Name: "Fast"},
		},
	}
	got := OrderModels(snap)
	if modelIDs(got) != "fast,grok,other" {
		t.Fatalf("current then grok then rest: %s", modelIDs(got))
	}
	if snap.Models[0].ID != "other" {
		t.Fatalf("OrderModels mutated input: %+v", snap.Models)
	}

	alphaFirst := Snapshot{
		CurrentModel: "fast",
		Models: []ModelInfo{
			{ID: "aaa", Name: "Aaa"},
			{ID: "grok", Name: "Grok"},
			{ID: "fast", Name: "Fast"},
		},
	}
	if modelIDs(OrderModels(alphaFirst)) != "fast,grok,aaa" {
		t.Fatalf("grok before unrelated: %s", modelIDs(OrderModels(alphaFirst)))
	}

	tied := Snapshot{
		Models: []ModelInfo{
			{ID: "b", Name: "Same"},
			{ID: "a", Name: "Same"},
		},
	}
	if modelIDs(OrderModels(tied)) != "a,b" {
		t.Fatalf("id tie-break: %s", modelIDs(OrderModels(tied)))
	}

	fromCfg := Snapshot{
		CurrentModel: "fast",
		Config: []ConfigOption{{
			ID:       "model",
			Name:     "Model",
			Category: "model",
			Type:     "select",
			SelectValues: []SelectValue{
				{Value: "other", Name: "Zed"},
				{Value: "grok", Name: "Grok"},
				{Value: "fast", Name: "Fast"},
			},
		}},
	}
	if modelIDs(OrderModels(fromCfg)) != "fast,grok,other" {
		t.Fatalf("config fallback: %s", modelIDs(OrderModels(fromCfg)))
	}
}

func TestEffortOptionPreference(t *testing.T) {
	vals := []SelectValue{
		{Value: "low", Name: "Low"},
		{Value: "medium", Name: "Medium"},
		{Value: "high", Name: "High"},
	}
	sel := func(id, name, cat string) ConfigOption {
		return ConfigOption{ID: id, Name: name, Category: cat, Type: "select", SelectValues: vals}
	}
	first := sel("reasoning_misc", "Reasoning", "")
	thought := sel("tl", "Reasoning", "thought_level")
	idEffort := sel("effort", "Effort", "")
	modelOpt := sel("x", "Effort", "model_option")

	got := EffortOption(Snapshot{Config: []ConfigOption{first, thought, idEffort, modelOpt}})
	if got == nil || got.ID != "x" {
		t.Fatalf("prefer model_option, got %+v", got)
	}
	got = EffortOption(Snapshot{Config: []ConfigOption{first, thought, idEffort}})
	if got == nil || got.ID != "effort" {
		t.Fatalf("prefer id effort, got %+v", got)
	}
	got = EffortOption(Snapshot{Config: []ConfigOption{first, thought}})
	if got == nil || got.ID != "tl" {
		t.Fatalf("prefer thought_level, got %+v", got)
	}
	got = EffortOption(Snapshot{Config: []ConfigOption{first}})
	if got == nil || got.ID != "reasoning_misc" {
		t.Fatalf("first match, got %+v", got)
	}
	if EffortOption(Snapshot{}) != nil {
		t.Fatal("empty config should have no effort option")
	}
	if EffortOption(Snapshot{Config: []ConfigOption{{
		ID: "effort", Name: "Effort", Type: "boolean", Current: "true",
	}}}) != nil {
		t.Fatal("boolean thinking is not an effort select")
	}

	stubLike := EffortOption(Snapshot{Config: []ConfigOption{{
		ID: "effort", Name: "Effort", Category: "thought_level", Type: "select",
		Current: "medium", SelectValues: vals,
	}}})
	if stubLike == nil || stubLike.Current != "medium" || len(stubLike.SelectValues) != 3 {
		t.Fatalf("stub/fake shape %+v", stubLike)
	}
}

// TestEffortIsNotThinkingOrContext runs the effort and fast heuristics over
// cursor's own catalogs, captured live 2026-09-21 (Plan 025 X1) as
// set_config_option(model, X) answered them, the model option trimmed to four
// models: testdata/cursor-<model>.json. claude-opus-5 carries two selects
// craze does not offer — `thinking`, which shares effort's category
// (thought_level), and `context`, which shares fast's (model_config) — and
// neither may ever be taken for the control it sits beside: only the id and
// the name tell them apart. glm-5.2's effort is spelled `reasoning`, and it has
// no fast toggle. The captured currentValues are the account's persisted
// choices, which is why nothing here reads them.
func TestEffortIsNotThinkingOrContext(t *testing.T) {
	for _, tc := range []struct {
		model  string
		effort string // the id EffortOption must find, or "" for none
		fast   string // the id FastOption must find, or "" for none
	}{
		{"claude-opus-5", "effort", "fast"},
		{"glm-5.2", "reasoning", ""},
	} {
		t.Run(tc.model, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "cursor-"+tc.model+".json"))
			if err != nil {
				t.Fatal(err)
			}
			cfg := parseConfigOptions(raw)
			if len(cfg) == 0 {
				t.Fatalf("the capture parsed to nothing: %s", raw)
			}
			snap := Snapshot{Provider: CursorProvider().Info(), Config: cfg}
			if got := optionID(EffortOption(snap)); got != tc.effort {
				t.Fatalf("EffortOption found %q, want %q", got, tc.effort)
			}
			if got := optionID(FastOption(snap)); got != tc.fast {
				t.Fatalf("FastOption found %q, want %q", got, tc.fast)
			}
			// With the real controls taken away, what is left must not stand in
			// for them: a catalog of thinking and context has no effort select
			// and no fast toggle, whatever their categories say.
			var rest []ConfigOption
			for _, opt := range cfg {
				if opt.ID != tc.effort && opt.ID != tc.fast {
					rest = append(rest, opt)
				}
			}
			bare := Snapshot{Provider: CursorProvider().Info(), Config: rest}
			if got := EffortOption(bare); got != nil {
				t.Fatalf("without %q, EffortOption took %q", tc.effort, got.ID)
			}
			if got := FastOption(bare); got != nil {
				t.Fatalf("without %q, FastOption took %q", tc.fast, got.ID)
			}
		})
	}
}

func optionID(opt *ConfigOption) string {
	if opt == nil {
		return ""
	}
	return opt.ID
}

func TestSplitModelEffort(t *testing.T) {
	snap := Snapshot{
		Models: []ModelInfo{{ID: "grok", Name: "Grok"}, {ID: "fast", Name: "Fast"}},
		Config: []ConfigOption{{
			ID: "effort", Name: "Effort", Category: "thought_level", Type: "select",
			SelectValues: []SelectValue{
				{Value: "low", Name: "Low"},
				{Value: "medium", Name: "Medium"},
				{Value: "high", Name: "High"},
			},
		}},
	}
	model, effort := SplitModelEffort("grok high", snap)
	if model != "grok" || effort != "high" {
		t.Fatalf("grok high → %q %q", model, effort)
	}
	model, effort = SplitModelEffort("grok HIGH", snap)
	if model != "grok" || effort != "high" {
		t.Fatalf("canonical effort → %q %q", model, effort)
	}
	model, effort = SplitModelEffort("Composer 2", snap)
	if model != "Composer 2" || effort != "" {
		t.Fatalf("spaced name → %q %q", model, effort)
	}
	model, effort = SplitModelEffort("grok", snap)
	if model != "grok" || effort != "" {
		t.Fatalf("model only → %q %q", model, effort)
	}
	model, effort = SplitModelEffort("high", snap)
	if model != "high" || effort != "" {
		t.Fatalf("single token stays model → %q %q", model, effort)
	}
	if _, err := MatchModel(snap, "Composer 2"); err == nil {
		t.Fatal("unknown spaced name should fail MatchModel")
	}
	cfgOnly := Snapshot{
		Config: []ConfigOption{{
			ID: "model", Category: "model", Type: "select",
			SelectValues: []SelectValue{{Value: "fast", Name: "Fast"}},
		}},
	}
	id, err := MatchModel(cfgOnly, "Fast")
	if err != nil || id != "fast" {
		t.Fatalf("config models MatchModel %q %v", id, err)
	}
}
