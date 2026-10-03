package modeltable

import (
	"fmt"
	"strings"
	"testing"
)

// TestMinimumMet (plan 034 §3.1, Q3): the downgrade-safe compare. Equal
// passes, missing parts are 0, leading zeros are numbers, and a missing or
// malformed minimum (or pin) fails open — kept, because the server already
// filtered the list.
func TestMinimumMet(t *testing.T) {
	for _, tc := range []struct {
		name, min, pin string
		want           bool
	}{
		{"equal", "0.160.0", "0.160.0", true},
		{"below", "0.155.0", "0.160.0", true},
		{"above by minor", "0.161.0", "0.160.0", false},
		{"above by patch", "0.160.1", "0.160.0", false},
		{"above by major", "1.0.0", "0.160.0", false},
		{"missing parts are zero, equal", "0.160", "0.160.0", true},
		{"missing parts in the pin", "0.160.0", "0.160", true},
		{"missing parts, above", "0.160.1", "0.160", false},
		{"one part", "1", "0.160.0", false},
		{"leading zeros", "0.0160.00", "0.160.0", true},
		{"leading zeros, above", "0.0161.0", "0.160.0", false},
		{"numeric, not lexical", "0.9.0", "0.10.0", true},
		{"four parts, above", "0.160.0.1", "0.160.0", false},
		{"four parts, equal", "0.160.0.0", "0.160.0", true},
		{"six digits is fine", "999999.0.0", "999999.0.0", true},
		{"seven digits is malformed: kept", "1000000.0.0", "0.160.0", true},
		{"a non-digit is malformed: kept", "0.161.0-beta", "0.160.0", true},
		{"a sign is malformed: kept", "+9.0.0", "0.160.0", true},
		{"an empty part is malformed: kept", "9..0", "0.160.0", true},
		{"five parts is malformed: kept", "9.0.0.0.0", "0.160.0", true},
		{"no minimum: kept", "", "0.160.0", true},
		{"a malformed pin keeps everything", "9.0.0", "x", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := minimumMet(tc.min, tc.pin); got != tc.want {
				t.Fatalf("minimumMet(%q, %q) = %v, want %v", tc.min, tc.pin, got, tc.want)
			}
		})
	}
}

// TestChatGPTModelsClientVersionIsTheShippedPin (plan 034 Q1): the accessor
// is the shipped catalog's pin, and that pin is one the catalog's own rule
// accepts.
func TestChatGPTModelsClientVersionIsTheShippedPin(t *testing.T) {
	v := ChatGPTModelsClientVersion()
	if v != "0.160.0" {
		t.Fatalf("ChatGPTModelsClientVersion() = %q, want the shipped 0.160.0", v)
	}
	if !validPinVersion(v) {
		t.Fatalf("the shipped pin %q fails the catalog's own rule", v)
	}
}

// pinnedModels is a list of three models, each with a minimum, bound to the
// test account: one at the pin, one below, one above.
func pinnedModels() string {
	return fmt.Sprintf(`{"version":1,"subject":%q,"client_id":%q,"client_version":"9.9.9","fetched_at":"2026-10-01T00:00:00Z","models":[
{"slug":"gpt-new","display_name":"New","context_window":1000,"efforts":["low"],"input_modalities":["text"],"priority":0,"minimal_client_version":"0.161.0"},
{"slug":"gpt-at","display_name":"At","context_window":1000,"efforts":["low"],"input_modalities":["text"],"priority":1,"minimal_client_version":"0.160"},
{"slug":"gpt-old","display_name":"Old","context_window":1000,"efforts":["low"],"input_modalities":["text"],"priority":2,"minimal_client_version":"0.144.0"},
{"slug":"gpt-junk","display_name":"Junk","context_window":1000,"efforts":["low"],"input_modalities":["text"],"priority":3,"minimal_client_version":"not a version"},
{"slug":"gpt-none","display_name":"None","context_window":1000,"efforts":["low"],"input_modalities":["text"],"priority":4}
]}`, planSubject, planClient)
}

// TestDiscoveredModelsAreDowngradeSafe (plan 034 §3.1, A2): a cache whose
// model needs a client version above this binary's pin is read without it,
// silently; a minimum equal to or below the pin, a malformed one and a
// missing one keep the model; and a legacy cache (no client_version, no
// minimums) reads as before. The control is a catalog whose pin is higher:
// the same file then offers the model.
func TestDiscoveredModelsAreDowngradeSafe(t *testing.T) {
	dir := t.TempDir()
	signInAs(t, dir, registration(planSubject, planClient, true), true)
	withPlanModels(t, dir, pinnedModels())
	tbl, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"chatgpt/gpt-at", "chatgpt/gpt-junk", "chatgpt/gpt-none", "chatgpt/gpt-old"}
	if got := planAliases(tbl); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("plan aliases = %v, want %v (gpt-new needs 0.161.0 > the pin)", got, want)
	}
	if len(tbl.Warnings) != 0 {
		t.Fatalf("a model above the pin is dropped with warnings %q; it is silent", tbl.Warnings)
	}

	// Control: a catalog pinned higher offers it.
	cat, err := ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	cat.ChatGPT.ModelsClientVersion = "0.161.0"
	tbl, err = LoadWith(dir, cat)
	if err != nil {
		t.Fatal(err)
	}
	if !hasModel(tbl.Models, "chatgpt/gpt-new") {
		t.Fatalf("control: with the pin raised, aliases = %v", planAliases(tbl))
	}

	// A legacy cache (the shape plan 033 wrote) is read as it always was.
	withPlanModels(t, dir, planModels(planSubject, planClient))
	if tbl, err = Load(dir); err != nil {
		t.Fatal(err)
	}
	if got := planAliases(tbl); len(got) != 3 {
		t.Fatalf("legacy cache aliases = %v, want its three valid models", got)
	}
}

// startList is a plan list of the given slugs, bound to the test account,
// every model at the same priority order as given.
func startList(slugs ...string) string {
	var models []string
	for i, s := range slugs {
		models = append(models, fmt.Sprintf(`{"slug":%q,"display_name":%q,"context_window":1000,"efforts":["low","medium"],"default_effort":"medium","input_modalities":["text"],"priority":%d}`, s, s, i))
	}
	return fmt.Sprintf(`{"version":1,"subject":%q,"client_id":%q,"fetched_at":"2026-10-01T00:00:00Z","models":[%s]}`,
		planSubject, planClient, strings.Join(models, ","))
}

// TestStartModelOrderedChatGPTStart (plan 034 Q4, A3): the shipped start is
// [gpt-6.1-sol, gpt-5.6-sol], and StartModel takes the first of them the
// account lists, ahead of the alias that sorts first; with neither listed it
// takes the first alias that resolves; with no start configured, the same.
// The remembered model and a funded default still beat the whole list.
func TestStartModelOrderedChatGPTStart(t *testing.T) {
	signed := func(slugs ...string) *Table {
		t.Helper()
		dir := t.TempDir()
		signInAs(t, dir, registration(planSubject, planClient, true), true)
		withPlanModels(t, dir, startList(slugs...))
		tbl, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		return tbl
	}
	start := func(tbl *Table, recent []RecentEntry, env map[string]string) string {
		t.Helper()
		alias, _, err := tbl.StartModel(recent, fakeEnv(env))
		if err != nil {
			t.Fatal(err)
		}
		return alias
	}

	// Both listed, the second sorting first: the first preference wins.
	both := signed("gpt-5.6-sol", "gpt-6.1-sol", "gpt-5.6-luna")
	if got := start(both, nil, nil); got != "chatgpt/gpt-6.1-sol" {
		t.Fatalf("both listed: StartModel = %q, want the first preference chatgpt/gpt-6.1-sol", got)
	}
	// The first preference not listed: the second.
	second := signed("gpt-5.6-luna", "gpt-5.6-sol")
	if got := start(second, nil, nil); got != "chatgpt/gpt-5.6-sol" {
		t.Fatalf("first unlisted: StartModel = %q, want chatgpt/gpt-5.6-sol", got)
	}
	// Neither listed: the first alias that resolves, in sorted order.
	neither := signed("gpt-6-sol", "gpt-5.5")
	if got := start(neither, nil, nil); got != "chatgpt/gpt-5.5" {
		t.Fatalf("neither listed: StartModel = %q, want the first resolving alias chatgpt/gpt-5.5", got)
	}
	// The configured default, funded, beats the list.
	if got := start(both, nil, map[string]string{"FIREWORKS_API_KEY": canary}); got != both.DefaultModel {
		t.Fatalf("funded default: StartModel = %q, want %q", got, both.DefaultModel)
	}
	// A remembered model beats everything.
	prov, wire := both.Models["chatgpt/gpt-5.6-luna"].Provider, both.Models["chatgpt/gpt-5.6-luna"].WireModel
	rem := []RecentEntry{{Alias: "chatgpt/gpt-5.6-luna", Provider: prov, WireModel: wire}}
	if got := start(both, rem, nil); got != "chatgpt/gpt-5.6-luna" {
		t.Fatalf("remembered: StartModel = %q, want chatgpt/gpt-5.6-luna", got)
	}

	// No start configured: the first resolving alias, as with neither listed.
	cat, err := ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	cat.ChatGPT.Start, cat.ChatGPT.startSet = nil, false
	dir := t.TempDir()
	signInAs(t, dir, registration(planSubject, planClient, true), true)
	withPlanModels(t, dir, startList("gpt-6.1-sol", "gpt-5.5"))
	tbl, err := LoadWith(dir, cat)
	if err != nil {
		t.Fatal(err)
	}
	if got := start(tbl, nil, nil); got != "chatgpt/gpt-5.5" {
		t.Fatalf("no start: StartModel = %q, want the first resolving alias chatgpt/gpt-5.5", got)
	}
}

// TestDiscoveredModelsKeepAModelWithANonStringMinimum (plan 034 r1 #4): a
// minimal_client_version that is not a JSON string is read as no minimum, so
// the model stays; it never fails the entry.
func TestDiscoveredModelsKeepAModelWithANonStringMinimum(t *testing.T) {
	for _, v := range []string{`999`, `1.5`, `{"a":"9.9.9"}`, `null`, `[]`, `["9.9.9"]`, `true`} {
		dir := t.TempDir()
		signInAs(t, dir, registration(planSubject, planClient, true), true)
		withPlanModels(t, dir, fmt.Sprintf(`{"version":1,"subject":%q,"client_id":%q,"fetched_at":"2026-10-01T00:00:00Z","models":[
{"slug":"gpt-odd","display_name":"Odd","context_window":1000,"efforts":["low"],"input_modalities":["text"],"priority":0,"minimal_client_version":%s},
{"slug":"gpt-ok","display_name":"Ok","context_window":1000,"efforts":["low"],"input_modalities":["text"],"priority":1}
]}`, planSubject, planClient, v))
		tbl, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(planAliases(tbl), ","); got != "chatgpt/gpt-odd,chatgpt/gpt-ok" {
			t.Errorf("minimum %s: plan aliases = %s, want both models", v, got)
		}
		if len(tbl.Warnings) != 0 {
			t.Errorf("minimum %s: warnings %q", v, tbl.Warnings)
		}
	}
}
