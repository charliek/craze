package agent

import (
	"encoding/json"
	"testing"
)

// liveConfigOptions is what cursor-agent advertised in the 004 capture
// (live/u6.out), trimmed to the options craze looks for. It is the fixture the
// effort and fast heuristics are held against, because it is the only shape
// either of them has ever had to match in the wild.
const liveConfigOptions = `[
	{"id":"mode","name":"Mode","description":"Controls how the agent executes tasks","category":"mode","type":"select","currentValue":"agent",
	 "options":[{"value":"agent","name":"Agent"},{"value":"plan","name":"Plan"},{"value":"ask","name":"Ask"}]},
	{"id":"model","name":"Model","description":"Controls which model is used for responses","category":"model","type":"select","currentValue":"grok-4.6",
	 "options":[{"value":"default","name":"Auto"},{"value":"grok-4.6","name":"Cursor Grok 4.6"}]},
	{"id":"effort","name":"Effort","description":"Effort the model uses to generate its response.","category":"thought_level","type":"select","currentValue":"high",
	 "options":[{"value":"low","name":"Low"},{"value":"medium","name":"Medium"},{"value":"high","name":"High"},{"value":"xhigh","name":"Extra High"}]},
	{"id":"fast","name":"Fast","description":"Significantly faster but consumes more usage","category":"model_config","type":"select","currentValue":"true",
	 "options":[{"value":"false","name":"Off"},{"value":"true","name":"Fast"}]}
]`

func TestCursorProviderModeKinds(t *testing.T) {
	p := CursorProvider()
	if p.Name() != "cursor" {
		t.Fatalf("name %q", p.Name())
	}
	if p.ImplementPrompt() != "Implement the plan above." {
		t.Fatalf("implement prompt %q", p.ImplementPrompt())
	}
	for id, want := range map[string]ModeKind{
		"agent":     ModeImplement,
		"code":      ModeImplement,
		"default":   ModeImplement,
		"plan":      ModePlan,
		"architect": ModePlan,
		"ask":       ModeReadOnly,
		"":          ModeUnknown,
		"chat":      ModeUnknown,
	} {
		if got := p.ModeKind(id); got != want {
			t.Fatalf("ModeKind(%q) = %v, want %v", id, got, want)
		}
	}
	// The vocabulary is one table: every alias ResolveMode accepts has a kind.
	for _, v := range modeVocabulary {
		for _, alias := range v.aliases {
			if got := p.ModeKind(alias); got != v.kind {
				t.Fatalf("alias %q of %q is kind %v, want %v", alias, v.canonical, got, v.kind)
			}
			if id, ok := ResolveMode(v.canonical, []string{alias}); !ok || id != alias {
				t.Fatalf("ResolveMode(%q, [%q]) = %q %v", v.canonical, alias, id, ok)
			}
		}
	}
}

// TestNilProviderBehavesLikeCursor is the seam's own contract: a snapshot that
// carries no provider — a stub, anything taken before session/new — reads
// exactly as cursor's.
func TestNilProviderBehavesLikeCursor(t *testing.T) {
	zero := ProviderInfo{}
	if zero.Label() != "cursor" {
		t.Fatalf("zero label %q", zero.Label())
	}
	if zero.Kind("plan") != ModePlan || zero.Kind("nope") != ModeUnknown {
		t.Fatal("a zero ProviderInfo reads cursor's vocabulary")
	}
	if zero.ImplementPrompt() != "Implement the plan above." {
		t.Fatalf("zero implement prompt %q", zero.ImplementPrompt())
	}
	if got := newSession(Options{}).Snapshot().Provider; got.Name != "cursor" ||
		got.ImplementPrompt() != "Implement the plan above." || got.modeKinds["architect"] != ModePlan {
		t.Fatalf("a nil Options.Provider is cursor's, got %+v", got)
	}
	custom := CursorProvider()
	if got := newSession(Options{Provider: &custom}).Snapshot().Provider; got.Name != "cursor" {
		t.Fatalf("explicit provider %+v", got)
	}
}

func TestEffortAndFastOptionsAgainstTheLiveFixture(t *testing.T) {
	snap := Snapshot{Config: parseConfigOptions(json.RawMessage(liveConfigOptions))}
	if len(snap.Config) != 4 {
		t.Fatalf("fixture parsed to %d options", len(snap.Config))
	}
	effort := EffortOption(snap)
	if effort == nil || effort.ID != "effort" || effort.Current != "high" || len(effort.SelectValues) != 4 {
		t.Fatalf("effort %+v", effort)
	}
	fast := FastOption(snap)
	if fast == nil || fast.ID != "fast" || fast.Current != "true" {
		t.Fatalf("fast %+v", fast)
	}
	if len(fast.SelectValues) != 2 || fast.SelectValues[0].Name != "Off" {
		t.Fatalf("fast values %+v", fast.SelectValues)
	}
	// Neither heuristic may claim the other's option, or the mode and model
	// selects that sit beside them.
	p := CursorProvider()
	for _, opt := range snap.Config {
		if p.isEffortOption(opt) != (opt.ID == "effort") {
			t.Fatalf("isEffortOption(%q) = %v", opt.ID, p.isEffortOption(opt))
		}
		if p.isFastOption(opt) != (opt.ID == "fast") {
			t.Fatalf("isFastOption(%q) = %v", opt.ID, p.isFastOption(opt))
		}
	}
	if FastOption(Snapshot{}) != nil {
		t.Fatal("a session with no config advertises no fast option")
	}
	// A fast-sounding option in another category is not the toggle.
	if FastOption(Snapshot{Config: []ConfigOption{
		{ID: "fast", Name: "Fast", Category: "model", Type: "select"},
	}}) != nil {
		t.Fatal("only model_config carries the fast toggle")
	}
	// …and a model_config option named for speed counts even with another id.
	if got := FastOption(Snapshot{Config: []ConfigOption{
		{ID: "turbo", Name: "Fast mode", Category: "model_config", Type: "select"},
	}}); got == nil || got.ID != "turbo" {
		t.Fatalf("name match %+v", got)
	}
}

func TestParseModesKeepsSanitisedDescriptions(t *testing.T) {
	// The description is agent-supplied text, so it goes through the same
	// scrubber as everything else: it is built here with a real escape
	// sequence and a real BEL in it.
	raw, err := json.Marshal(map[string]any{
		"currentModeId": "plan",
		"availableModes": []map[string]string{
			{"id": "agent", "name": "Agent", "description": "Full agent capabilities with tool access"},
			{"id": "plan", "name": "Plan", "description": "Read-only\x1b[31m mode\a for planning"},
			{"id": "ask", "name": "Ask"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cur, modes := parseModes(raw)
	if cur != "plan" || len(modes) != 3 {
		t.Fatalf("%q %+v", cur, modes)
	}
	if modes[0].Description != "Full agent capabilities with tool access" {
		t.Fatalf("description %q", modes[0].Description)
	}
	if got := modes[1].Description; got != "Read-only mode for planning" {
		t.Fatalf("the description goes through the sanitiser: %q", got)
	}
	if modes[2].Description != "" {
		t.Fatalf("no description advertised, got %q", modes[2].Description)
	}
}
