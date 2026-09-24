package harness

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/modeltable"
)

// resolverSession is a *Session built only far enough for
// resolveChildModel/resolveChildEffort: the test model table (helpers_test.go's
// providersTOML/modelsTOML — test/a and test/b on a keyed provider, other/c
// with no effort control, nokey/d on a provider with no usable key), with
// tiers and a configured sub-agent default layered on. It never goes through
// Open: nothing else these methods read needs a store or tools.
func resolverSession(t *testing.T, tiers map[string]string, defaultModel, defaultEffort string, matchModel func(string) (string, bool)) *Session {
	t.Helper()
	f := newFixture(t, "http://127.0.0.1:0")
	f.table.Subagents = modeltable.Subagents{Model: defaultModel, Effort: defaultEffort, Tiers: tiers}
	return &Session{table: f.table, getenv: f.getenv, matchModel: matchModel}
}

// collectWarn is a warn func that records every line it is given, in order.
func collectWarn(lines *[]string) func(string) {
	return func(s string) { *lines = append(*lines, s) }
}

// TestResolveChildModelMatrix covers §3.6's precedence (call, persona, the
// configured default, the parent) crossed with how a candidate's raw value
// reads: a plain alias, a mapped tier, an unmapped (but recognised) tier,
// "inherit", and a value that is none of those, at every position it is
// legal (an unknown or key-less value at the call position is a hard error;
// at persona or default it falls through with one warn line).
func TestResolveChildModelMatrix(t *testing.T) {
	tiers := map[string]string{"speedy": "test/b"} // opus/sonnet/haiku/fable stay unmapped
	const parentAlias = "test/a"                   // has a key; test/a's own alias

	type row struct {
		name             string
		call, persona    string
		defaultModel     string
		matchModel       func(string) (string, bool)
		wantAlias        string
		wantErr          string // substring; "" = no error
		wantWarnCount    int
		wantWarnContains string // substring of the first warn line, when wantWarnCount > 0
	}
	rows := []row{
		// call position
		{name: "call: a plain alias", call: "test/b", wantAlias: "test/b"},
		{name: "call: a mapped tier", call: "speedy", wantAlias: "test/b"},
		{name: "call: an unmapped built-in tier is the parent's", call: "opus", wantAlias: parentAlias},
		{name: "call: inherit is the parent's", call: "inherit", wantAlias: parentAlias},
		{name: "call: case-insensitive inherit", call: "Inherit", wantAlias: parentAlias},
		{name: "call: unknown value errors, not falls through", call: "bogus", defaultModel: "test/b", wantErr: "Unknown model `bogus`"},
		{name: "call: a key-less alias errors, not falls through", call: "nokey/d", defaultModel: "test/b", wantErr: "nokey/d"},
		{
			name: "call: matched through the adapter's normalisation",
			call: "TEST/A", matchModel: func(raw string) (string, bool) {
				if strings.EqualFold(raw, "test/a") {
					return "test/a", true
				}
				return "", false
			},
			wantAlias: "test/a",
		},
		{name: "call: no matcher is exact alias match only", call: "TEST/A", wantErr: "Unknown model"},

		// persona position (call empty)
		{name: "persona: a plain alias", persona: "test/b", wantAlias: "test/b"},
		{name: "persona: a mapped tier", persona: "speedy", wantAlias: "test/b"},
		{name: "persona: an unmapped tier is the parent's, no warn", persona: "sonnet", wantAlias: parentAlias, wantWarnCount: 0},
		{name: "persona: inherit", persona: "inherit", wantAlias: parentAlias},
		{
			name:    "persona: unknown falls through to the default, with a warn",
			persona: "bogus", defaultModel: "test/b",
			wantAlias: "test/b", wantWarnCount: 1, wantWarnContains: `the persona's model "bogus"`,
		},
		{
			name:    "persona: key-less falls through to the default, with a warn",
			persona: "nokey/d", defaultModel: "test/b",
			wantAlias: "test/b", wantWarnCount: 1, wantWarnContains: `the persona's model "nokey/d"`,
		},
		{
			name:      "persona: unknown with no default falls through to the parent",
			persona:   "bogus",
			wantAlias: parentAlias, wantWarnCount: 1, wantWarnContains: "the persona's model",
		},

		// default position (call and persona empty)
		{name: "default: a plain alias", defaultModel: "test/b", wantAlias: "test/b"},
		{
			name:         "default: unknown falls through to the parent, with a warn",
			defaultModel: "bogus",
			wantAlias:    parentAlias, wantWarnCount: 1, wantWarnContains: "the configured sub-agent default model",
		},
		{
			name:         "default: key-less falls through to the parent, with a warn",
			defaultModel: "nokey/d",
			wantAlias:    parentAlias, wantWarnCount: 1, wantWarnContains: "nokey/d",
		},

		// parent (nothing set at all)
		{name: "parent: every candidate empty", wantAlias: parentAlias},

		// the whole chain: persona and default both fall through
		{
			name:    "chain: persona and default both fall through to the parent",
			persona: "bogus", defaultModel: "nokey/d",
			wantAlias: parentAlias, wantWarnCount: 2,
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			s := resolverSession(t, tiers, r.defaultModel, "", r.matchModel)
			var warnings []string
			alias, err := s.resolveChildModel(childModelInput{
				Call: r.call, Persona: r.persona,
				ParentAlias: parentAlias, ParentEffort: "high",
			}, collectWarn(&warnings))

			if r.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), r.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, r.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if alias != r.wantAlias {
				t.Fatalf("alias = %q, want %q", alias, r.wantAlias)
			}
			if len(warnings) != r.wantWarnCount {
				t.Fatalf("warnings = %q, want %d of them", warnings, r.wantWarnCount)
			}
			if r.wantWarnContains != "" && (len(warnings) == 0 || !strings.Contains(warnings[0], r.wantWarnContains)) {
				t.Fatalf("warnings = %q, want the first to contain %q", warnings, r.wantWarnContains)
			}
		})
	}
}

// TestResolveChildModelUnknownListsEveryAliasAndTier pins the exact shape of
// the call-position error text (plan 026 §3.6): every alias, the configured
// tier mappings, and the instruction to omit `model`.
func TestResolveChildModelUnknownListsEveryAliasAndTier(t *testing.T) {
	s := resolverSession(t, map[string]string{"speedy": "test/b", "careful": "test/a"}, "", "", nil)
	_, err := s.resolveChildModel(childModelInput{Call: "nope", ParentAlias: "test/a"}, nil)
	if err == nil {
		t.Fatal("err = nil, want the unknown-model error")
	}
	msg := err.Error()
	for _, want := range []string{
		"Unknown model `nope`.",
		"test/a", "test/b", "other/c", "nokey/d", // every alias
		"careful → test/a", "speedy → test/b", // sorted by tier name
		"Omit `model` to use the parent's.",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("err = %q, want it to contain %q", msg, want)
		}
	}
}

// TestResolveChildModelUnknownCapsTheList: a table with far more aliases
// than 1 KiB of text holds still gets back a message no longer than the cap,
// counting what was left out (like §3.4's unknown-type text).
func TestResolveChildModelUnknownCapsTheList(t *testing.T) {
	models := map[string]modeltable.Model{}
	for i := range 300 {
		alias := fmt.Sprintf("provider/a-fairly-long-model-name-%03d", i)
		models[alias] = modeltable.Model{Provider: "p", WireModel: alias}
	}
	tbl := &modeltable.Table{
		DefaultModel: "provider/a-fairly-long-model-name-000",
		Providers:    map[string]modeltable.Provider{"p": {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://x", EnvKeys: []string{"X"}}},
		Models:       models,
	}
	err := unknownModelError("nope", tbl)
	if err == nil {
		t.Fatal("err = nil")
	}
	msg := err.Error()
	if len(msg) > unknownModelCap {
		t.Fatalf("message is %d bytes, want at most %d", len(msg), unknownModelCap)
	}
	if !strings.Contains(msg, "… and ") || !strings.Contains(msg, "more") {
		t.Fatalf("message = %q, want a count of what was left out", msg)
	}
	if !strings.Contains(msg, "Omit `model` to use the parent's.") {
		t.Fatalf("message = %q, the fixed suffix must survive the cap", msg)
	}
}

// TestResolveChildModelUnknownCapsTheTierList (review r5, finding 8): a
// table with about 150 configured tiers, all mapping to one alias, makes the
// tier clause alone exceed unknownModelCap even though the alias list is
// tiny — capModelList used to leave that clause whole and return an
// oversized message. The cap now holds, with a count of what the tier
// clause left out, and the alias list and the fixed suffix survive because
// they were never what forced the cut.
func TestResolveChildModelUnknownCapsTheTierList(t *testing.T) {
	tiers := make(map[string]string, 150)
	for i := range 150 {
		tiers[fmt.Sprintf("tier-%03d", i)] = "test/a"
	}
	tbl := &modeltable.Table{
		DefaultModel: "test/a",
		Providers:    map[string]modeltable.Provider{"p": {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://x", EnvKeys: []string{"X"}}},
		Models: map[string]modeltable.Model{
			"test/a": {Provider: "p", WireModel: "test-a"},
		},
		Subagents: modeltable.Subagents{Tiers: tiers},
	}
	err := unknownModelError("nope", tbl)
	if err == nil {
		t.Fatal("err = nil")
	}
	msg := err.Error()
	if len(msg) > unknownModelCap {
		t.Fatalf("message is %d bytes, want at most %d", len(msg), unknownModelCap)
	}
	if !strings.Contains(msg, "Models: test/a;") {
		t.Fatalf("message = %q, want the tiny alias list kept whole", msg)
	}
	if !strings.Contains(msg, "… and ") || !strings.Contains(msg, "more") {
		t.Fatalf("message = %q, want a count of the tiers left out", msg)
	}
	if !strings.Contains(msg, "Omit `model` to use the parent's.") {
		t.Fatalf("message = %q, the fixed suffix must survive the cap", msg)
	}
}

// TestResolveChildModelUnknownCapsTheRawValue (review r5, finding 8): a call
// `model` value on its own can be longer than unknownModelCap. The message
// still fits, the offending value is folded to one line and cut rather than
// dropped, and the alias/tier clauses — small here — survive whole because
// they were never what forced the cut.
func TestResolveChildModelUnknownCapsTheRawValue(t *testing.T) {
	tbl := &modeltable.Table{
		DefaultModel: "test/a",
		Providers:    map[string]modeltable.Provider{"p": {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://x", EnvKeys: []string{"X"}}},
		Models: map[string]modeltable.Model{
			"test/a": {Provider: "p", WireModel: "test-a"},
		},
		Subagents: modeltable.Subagents{Tiers: map[string]string{"opus": "test/a"}},
	}
	raw := strings.Repeat("x\n", 1<<10) // 2 KiB, all newlines between the x's
	err := unknownModelError(raw, tbl)
	if err == nil {
		t.Fatal("err = nil")
	}
	msg := err.Error()
	if len(msg) > unknownModelCap {
		t.Fatalf("message is %d bytes, want at most %d", len(msg), unknownModelCap)
	}
	if strings.Contains(msg, raw) {
		t.Fatalf("message = %q, want the 2 KiB raw value cut, not quoted whole", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Fatalf("message = %q, want the raw value folded to one line", msg)
	}
	if !strings.Contains(msg, "…") {
		t.Fatalf("message = %q, want the cut raw value marked", msg)
	}
	if !strings.Contains(msg, "Models: test/a; tiers: opus → test/a. Omit `model` to use the parent's.") {
		t.Fatalf("message = %q, want the small alias and tier clauses kept whole", msg)
	}
}

// TestResolveChildEffortMatrix covers §3.6's effort precedence: call,
// persona, the configured default, then the parent's own effort — but only
// when the child runs the parent's model — then the model's default.
func TestResolveChildEffortMatrix(t *testing.T) {
	type row struct {
		name                             string
		alias, callEffort, personaEffort string
		defaultEffort                    string
		parentAlias, parentEffort        string
		want                             string
		wantErr                          string
		wantWarnCount                    int
		wantWarnContains                 string
	}
	rows := []row{
		{name: "call effort offered", alias: "test/a", callEffort: "low", want: "low"},
		{
			name:  "call effort not offered errors, naming the model and its list",
			alias: "test/a", callEffort: "extreme",
			wantErr: "Effort `extreme` is not offered by `test/a`. Its efforts: low, high.",
		},
		{
			name:  "call effort on a model with no effort control",
			alias: "other/c", callEffort: "low",
			wantErr: "Effort `low` is not offered by `other/c`. Its efforts: none.",
		},
		{name: "persona effort offered", alias: "test/b", personaEffort: "high", want: "high"},
		{
			name:  "persona effort not offered falls through to the default, with a warn",
			alias: "test/b", personaEffort: "bogus", defaultEffort: "medium",
			want: "medium", wantWarnCount: 1, wantWarnContains: `the persona's effort "bogus"`,
		},
		{name: "default effort offered", alias: "test/b", defaultEffort: "high", want: "high"},
		{
			name:  "default effort not offered, child on the parent's model, falls through to the parent's effort",
			alias: "test/a", defaultEffort: "bogus", parentAlias: "test/a", parentEffort: "low",
			want: "low", wantWarnCount: 1, wantWarnContains: "the configured sub-agent default effort",
		},
		{
			name:  "default effort not offered, child on a different model, falls through to that model's default",
			alias: "test/b", defaultEffort: "bogus", parentAlias: "test/a", parentEffort: "low",
			want: "medium", wantWarnCount: 1,
		},
		{
			name:  "the parent's effort only when the child runs the parent's model",
			alias: "test/a", parentAlias: "test/a", parentEffort: "low",
			want: "low",
		},
		{
			name:  "a different child model ignores the parent's effort, uses its own default",
			alias: "test/b", parentAlias: "test/a", parentEffort: "low",
			want: "medium",
		},
		{
			name:  "same model but no parent effort given falls to the model's default",
			alias: "test/a", parentAlias: "test/a", parentEffort: "",
			want: "high",
		},
		{
			name:  "no effort control and nothing set resolves to empty",
			alias: "other/c",
			want:  "",
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			s := resolverSession(t, nil, "", r.defaultEffort, nil)
			var warnings []string
			got, err := s.resolveChildEffort(r.alias, r.callEffort, r.personaEffort, r.parentAlias, r.parentEffort, collectWarn(&warnings))
			if r.wantErr != "" {
				if err == nil || err.Error() != r.wantErr {
					t.Fatalf("err = %v, want %q", err, r.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if got != r.want {
				t.Fatalf("effort = %q, want %q", got, r.want)
			}
			if len(warnings) != r.wantWarnCount {
				t.Fatalf("warnings = %q, want %d of them", warnings, r.wantWarnCount)
			}
			if r.wantWarnContains != "" && (len(warnings) == 0 || !strings.Contains(warnings[0], r.wantWarnContains)) {
				t.Fatalf("warnings = %q, want the first to contain %q", warnings, r.wantWarnContains)
			}
		})
	}
}

// TestResolveChildModelWarnNilDiscards: a nil warn func is never called,
// even through a chain of fall-throughs (Options.Warn's "nil discards").
func TestResolveChildModelWarnNilDiscards(t *testing.T) {
	s := resolverSession(t, nil, "nokey/d", "", nil)
	alias, err := s.resolveChildModel(childModelInput{Persona: "bogus", ParentAlias: "test/a"}, nil)
	if err != nil || alias != "test/a" {
		t.Fatalf("resolveChildModel = %q, %v, want test/a, nil", alias, err)
	}
}

// TestUnknownModelErrorIsAPlainError sanity-checks that the call error is a
// plain error carrying the message, not something wrapping a modeltable
// sentinel a caller might match on by mistake.
func TestUnknownModelErrorIsAPlainError(t *testing.T) {
	s := resolverSession(t, nil, "", "", nil)
	_, err := s.resolveChildModel(childModelInput{Call: "nope", ParentAlias: "test/a"}, nil)
	if errors.Is(err, modeltable.ErrUnknownModel) {
		t.Fatal("the call error wraps modeltable.ErrUnknownModel; it must be its own message")
	}
}
