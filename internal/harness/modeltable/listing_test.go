package modeltable

import (
	"cmp"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Plan 050 C2: what `craze models` reads of a table — the catalog as a table
// of its own, how each provider's models reach it, which models an account's
// list brought, the retired rows and the deprecated model's sentence.

// TestCatalogTable: the catalog alone, as a table, reads nothing — not the
// environment, whose Anthropic key would otherwise bind a list, nor any
// directory — every model shipped, the [anthropic_defaults] applied as a load
// with no list applies them, and the input never changed.
func TestCatalogTable(t *testing.T) {
	withAnthropicEnv(t, map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA})
	cat, err := ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	before := cat.Clone()
	tbl, err := CatalogTable(cat)
	if err != nil {
		t.Fatal(err)
	}
	loaded := loadShipped(t, t.TempDir())
	if got, want := tbl.Aliases(), loaded.Aliases(); !slices.Equal(got, want) {
		t.Fatalf("aliases = %q, want a load of an empty directory's %q", got, want)
	}
	for _, alias := range tbl.Aliases() {
		if tbl.ModelOrigin(alias) != OriginShipped || tbl.FromAccount(alias) {
			t.Errorf("%s: origin %q, from an account %v", alias, tbl.ModelOrigin(alias), tbl.FromAccount(alias))
		}
		if got, want := tbl.Models[alias].DefaultEffort, loaded.Models[alias].DefaultEffort; got != want {
			t.Errorf("%s default effort = %q, want the load's %q", alias, got, want)
		}
	}
	if tbl.AccountListRead(ChatGPTProvider) || tbl.AccountListRead(AnthropicProvider) {
		t.Fatal("the catalog alone read an account list")
	}
	if !slices.Equal(slices.Sorted(maps.Keys(cat.Models)), slices.Sorted(maps.Keys(before.Models))) || cat.Models["anthropic/claude-opus-5"].DefaultEffort != before.Models["anthropic/claude-opus-5"].DefaultEffort {
		t.Fatal("CatalogTable changed its catalog")
	}
	if _, err := CatalogTable(&Catalog{}); err == nil {
		t.Fatal("an empty catalog made a table")
	}
}

// TestAccountModels: how each provider's models reach the table, and whether
// the load read the account's list — for the ChatGPT plan, no list, a list of
// another account and one holding a model this craze is too old for are all
// no list; for Anthropic, no key, another key's list and a list the load used.
// A model the Anthropic list adds is the account's; a seed it names is not.
func TestAccountModels(t *testing.T) {
	withAnthropicEnv(t, map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA})
	empty := loadShipped(t, t.TempDir())
	for id, want := range map[string]string{ChatGPTProvider: AccountModelsOnly, AnthropicProvider: AccountModelsAlso,
		"fireworks": AccountModelsNone, "openai": AccountModelsNone, "nope": AccountModelsNone} {
		if got := empty.AccountModels(id); got != want {
			t.Errorf("AccountModels(%s) = %q, want %q", id, got, want)
		}
	}
	if empty.AccountListRead(ChatGPTProvider) || empty.AccountListRead(AnthropicProvider) || empty.AccountListRead("fireworks") {
		t.Fatal("an empty directory read an account list")
	}

	plan := signedInDir(t)
	if !loadShipped(t, plan).AccountListRead(ChatGPTProvider) {
		t.Fatal("the account's own list was not read")
	}
	other := t.TempDir()
	signInAs(t, other, registration("someone-else", planClient, true), true)
	withPlanModels(t, other, planModels(planSubject, planClient))
	if loadShipped(t, other).AccountListRead(ChatGPTProvider) {
		t.Fatal("another account's list counts as read")
	}
	newer := t.TempDir()
	signInAs(t, newer, registration(planSubject, planClient, true), true)
	withPlanModels(t, newer, `{"version":1,"subject":"`+planSubject+`","client_id":"`+planClient+`","models":[
{"slug":"gpt-6-astra","display_name":"GPT-6 Astra","context_window":272000,"priority":0},
{"slug":"gpt-9","display_name":"GPT-9","context_window":272000,"priority":1,"minimal_client_version":"999.0.0"}]}`)
	tbl := loadShipped(t, newer)
	if tbl.AccountListRead(ChatGPTProvider) {
		t.Fatal("a list holding a model this craze is too old for counts as read")
	}
	if !tbl.FromAccount("chatgpt/gpt-6-astra") {
		t.Fatal("a plan model is not the account's")
	}

	// A list bound to the account that gave no model — empty, or every
	// entry rejected — is no list that was read.
	for name, models := range map[string]string{"empty": `[]`, "every entry rejected": `[null, {"slug":"bad slug!"}]`} {
		d := t.TempDir()
		signInAs(t, d, registration(planSubject, planClient, true), true)
		withPlanModels(t, d, `{"version":1,"subject":"`+planSubject+`","client_id":"`+planClient+`","models":`+models+`}`)
		if tbl := loadShipped(t, d); tbl.AccountListRead(ChatGPTProvider) || len(planAliases(tbl)) != 0 {
			t.Errorf("%s: read %v, plan models %q", name, tbl.AccountListRead(ChatGPTProvider), planAliases(tbl))
		}
	}

	dir := t.TempDir()
	writeAnthropicList(t, dir, anthropicKeyB, append(seedEntries(t), opus6()))
	if loadShipped(t, dir).AccountListRead(AnthropicProvider) {
		t.Fatal("another key's Anthropic list counts as read")
	}
	writeAnthropicList(t, dir, anthropicKeyA, append(seedEntries(t), opus6()))
	tbl = loadShipped(t, dir)
	if !tbl.AccountListRead(AnthropicProvider) || !tbl.FromAccount("anthropic/claude-opus-6") || tbl.FromAccount("anthropic/claude-opus-5") {
		t.Fatalf("the key's own list: read %v, opus-6 the account's %v, opus-5 the account's %v",
			tbl.AccountListRead(AnthropicProvider), tbl.FromAccount("anthropic/claude-opus-6"), tbl.FromAccount("anthropic/claude-opus-5"))
	}

	withAnthropicEnv(t, nil)
	if loadShipped(t, dir).AccountListRead(AnthropicProvider) {
		t.Fatal("with no key the Anthropic list counts as read")
	}
}

// TestRetiredRowsAndDeprecatedNotice: the retired rows sorted, each the
// caller's own copy; a deprecated model's one sentence, and none for an
// active model or one the table does not have.
func TestRetiredRowsAndDeprecatedNotice(t *testing.T) {
	tbl, err := LoadWith(t.TempDir(), lifecycle(t))
	if err != nil {
		t.Fatal(err)
	}
	rows := tbl.RetiredRows()
	if !slices.IsSortedFunc(rows, func(a, b Retired) int { return cmp.Compare(a.Alias, b.Alias) }) || len(rows) == 0 {
		t.Fatalf("retired rows = %+v", rows)
	}
	if r, _ := tbl.Retired("acme/old"); r.ReplacedBy != "acme/fast" {
		t.Fatalf("acme/old = %+v", r)
	}
	if len(rows[0].WireModels) > 0 {
		rows[0].WireModels[0] = "changed"
		if again := tbl.RetiredRows(); again[0].WireModels[0] == "changed" {
			t.Fatal("RetiredRows shares the table's wire models")
		}
	}
	by, sentence, ok := tbl.DeprecatedNotice("acme/dep")
	if !ok || by != "acme/fast" || sentence != "acme/dep is deprecated; use acme/fast" {
		t.Fatalf("DeprecatedNotice(acme/dep) = %q, %q, %v", by, sentence, ok)
	}
	for _, alias := range []string{"acme/fast", "acme/nope"} {
		if _, _, ok := tbl.DeprecatedNotice(alias); ok {
			t.Errorf("DeprecatedNotice(%s) says deprecated", alias)
		}
	}
	if (&Table{}).RetiredRows() == nil {
		t.Fatal("a table with no catalog's rows is nil, not empty")
	}
}

// TestARepointedAliasIsTheUsersOwn (plan 050 review): an alias the user's
// models.toml points at another provider or wire model is the user's own
// model, whatever brought the alias — the ChatGPT plan's list, the Anthropic
// key's list, or the catalog: neither FromAccount nor CatalogModel. The
// controls: the same aliases untouched are the account's and the catalog's,
// and a shipped alias overridden on another key only stays the catalog's.
func TestARepointedAliasIsTheUsersOwn(t *testing.T) {
	withAnthropicEnv(t, map[string]string{"ANTHROPIC_API_KEY": anthropicKeyA})
	const repoint = "provider = \"openrouter\"\nwire_model = \"someone/else\"\n"
	dir := signedInDir(t)
	writeAnthropicList(t, dir, anthropicKeyA, append(seedEntries(t), opus6()))
	untouched := loadShipped(t, dir)
	for _, alias := range []string{"chatgpt/gpt-6-astra", "anthropic/claude-opus-6"} {
		if !untouched.FromAccount(alias) || !untouched.CatalogModel(alias) {
			t.Fatalf("control: %s from an account %v, the catalog's %v", alias, untouched.FromAccount(alias), untouched.CatalogModel(alias))
		}
	}
	if !untouched.CatalogModel("fireworks/kimi-k3p1-pro") {
		t.Fatal("control: a shipped model is not the catalog's")
	}

	models := "version = 1\n\n" +
		"[models.\"chatgpt/gpt-6-astra\"]\n" + repoint + "\n" +
		"[models.\"anthropic/claude-opus-6\"]\n" + repoint + "\n" +
		"[models.\"fireworks/kimi-k3p1-pro\"]\nwire_model = \"accounts/me/models/my-kimi\"\n\n" +
		"[models.\"fireworks/glm-5p3\"]\ndefault_effort = \"low\"\n"
	if err := os.WriteFile(filepath.Join(dir, ModelsFile), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	tbl := loadShipped(t, dir)
	for _, alias := range []string{"chatgpt/gpt-6-astra", "anthropic/claude-opus-6", "fireworks/kimi-k3p1-pro"} {
		if _, ok := tbl.Models[alias]; !ok {
			t.Fatalf("%s is not in the table: %q", alias, tbl.Warnings)
		}
		if tbl.FromAccount(alias) || tbl.CatalogModel(alias) {
			t.Errorf("%s, re-pointed: from an account %v, the catalog's %v; want the user's own", alias, tbl.FromAccount(alias), tbl.CatalogModel(alias))
		}
	}
	if !tbl.CatalogModel("fireworks/glm-5p3") {
		t.Error("a shipped model overridden on another key is no longer the catalog's")
	}
}
