package agent

import (
	"encoding/json"
	"testing"
)

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
	if opts[2].Type != "boolean" || opts[2].Current != "true" {
		t.Fatalf("boolean %+v", opts[2])
	}
}
