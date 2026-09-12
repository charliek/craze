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
