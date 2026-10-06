package modeltable

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The ChatGPT plan in the table (plan 033 §3.11, P22, P34): its models come
// from the account's own list, bound to the account, and it is funded by the
// sign-in. Every case writes a temp directory laid out as internal/chatgptauth
// leaves one — the registration, a token file and the model list — with no
// token in it: this package only ever stats the token file, so a placeholder
// stands for it.

// The registration of the account every case signs in as.
const (
	planSubject = "user-subject-0001"
	planClient  = "app_client-0001"
)

// signInAs lays down dir's registration (client) and, when tokens, a
// non-empty token file: the sign-in's state as chatgptauth leaves it.
func signInAs(t *testing.T, dir, client string, tokens bool) {
	t.Helper()
	auth := filepath.Join(dir, ChatGPTAuthDir)
	if err := os.MkdirAll(auth, 0o700); err != nil {
		t.Fatal(err)
	}
	if client != "" {
		if err := os.WriteFile(filepath.Join(auth, ChatGPTClientFile), []byte(client), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if tokens {
		if err := os.WriteFile(filepath.Join(auth, ChatGPTTokenFile), []byte(`{"placeholder":"no token here"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// registration is a chatgpt-client.json for subject and client id.
func registration(subject, clientID string, planUsage bool) string {
	return fmt.Sprintf(`{"client_id":%q,"subject":%q,"email":"someone@example.com","plan_usage":%v,"notice_shown":true}`,
		clientID, subject, planUsage)
}

// planModels is the model list the cases use, bound to subject and client
// id, in the order chatgptauth would never write it (priority 1 before 0), so
// the order the table keeps is the priority's, not the file's.
func planModels(subject, clientID string) string {
	return fmt.Sprintf(`{"version":1,"subject":%q,"client_id":%q,"models_etag":"e1","fetched_at":"2026-10-01T00:00:00Z","models":[
{"slug":"gpt-6-astra","display_name":"GPT-6 Astra","context_window":272000,"efforts":["low","medium","high","ultra"],"default_effort":"medium","input_modalities":["text","image"],"priority":1,"parallel_tool_calls":true},
{"slug":"gpt-5.6-sol","display_name":"GPT-5.6 Sol","context_window":272000,"efforts":["low","medium","high","xhigh"],"default_effort":"medium","input_modalities":["text"],"priority":0,"parallel_tool_calls":false},
{"slug":"bad slug!","display_name":"Nope","context_window":1000,"priority":2},
{"slug":"gpt-5.6-luna","display_name":"GPT-5.6 Luna","context_window":128000,"efforts":["ultra"],"priority":3}
]}`, subject, clientID)
}

// withPlanModels writes dir's model list.
func withPlanModels(t *testing.T, dir, list string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ChatGPTModelsFile), []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
}

// signedInDir is a directory signed in with plan usage, whose list is the
// account's own.
func signedInDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	signInAs(t, dir, registration(planSubject, planClient, true), true)
	withPlanModels(t, dir, planModels(planSubject, planClient))
	return dir
}

// planAliases are the table's aliases on the ChatGPT plan, sorted.
func planAliases(tbl *Table) []string {
	var out []string
	for _, a := range tbl.Aliases() {
		if strings.HasPrefix(a, ChatGPTAliasPrefix) {
			out = append(out, a)
		}
	}
	return out
}

// TestDiscoveredModels (plan 033 §3.11, A19): each model of the account's
// list becomes chatgpt/<slug> — its display name and " (ChatGPT plan)", its
// window, its efforts less "ultra", craze's default effort before the
// list's, vision from its modalities, parallel tool calls as listed, no cost
// — and an entry that cannot be one is skipped with one warning that never
// quotes its slug. The user's models.toml overrides one as it would a shipped
// model.
func TestDiscoveredModels(t *testing.T) {
	dir := signedInDir(t)
	tbl, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	want := map[string]Model{
		"chatgpt/gpt-6-astra": {Provider: "chatgpt", WireModel: "gpt-6-astra", Name: "GPT-6 Astra (ChatGPT plan)",
			ContextWindow: 272000, Efforts: []string{"low", "medium", "high"}, DefaultEffort: "low", // craze's, over the list's medium
			Vision: true, ParallelToolCalls: &yes},
		"chatgpt/gpt-5.6-sol": {Provider: "chatgpt", WireModel: "gpt-5.6-sol", Name: "GPT-5.6 Sol (ChatGPT plan)",
			ContextWindow: 272000, Efforts: []string{"low", "medium", "high", "xhigh"}, DefaultEffort: "medium",
			ParallelToolCalls: &no},
		// ultra alone: no effort a request can carry, so no effort control.
		"chatgpt/gpt-5.6-luna": {Provider: "chatgpt", WireModel: "gpt-5.6-luna", Name: "GPT-5.6 Luna (ChatGPT plan)", ContextWindow: 128000},
	}
	if got := planAliases(tbl); !slices.Equal(got, []string{"chatgpt/gpt-5.6-luna", "chatgpt/gpt-5.6-sol", "chatgpt/gpt-6-astra"}) {
		t.Fatalf("plan models = %q", got)
	}
	for alias, w := range want {
		if got := tbl.Models[alias]; !reflect.DeepEqual(got, w) {
			t.Errorf("%s =\n%+v\nwant\n%+v", alias, got, w)
		}
		if o := tbl.ModelOrigin(alias); o != OriginDiscovered {
			t.Errorf("%s origin = %q", alias, o)
		}
	}
	wantWarnings(t, tbl, []string{ChatGPTModelsFile, "models[2]", "is skipped", "slug"})
	if strings.Contains(tbl.Warnings[0], "bad slug") {
		t.Fatalf("the warning quotes a slug it could not name: %q", tbl.Warnings[0])
	}
	r, err := tbl.Resolve("chatgpt/gpt-5.6-sol", fakeEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if r.Driver != DriverChatGPT || r.Auth != AuthSignIn || r.CredentialDir != dir || r.APIKey != "" ||
		r.WireModel != "gpt-5.6-sol" || r.ParallelToolCalls == nil || *r.ParallelToolCalls ||
		r.MaxOutputTokens != DefaultMaxOutputTokens || r.Name != "GPT-5.6 Sol (ChatGPT plan)" {
		t.Fatalf("Resolve = %+v", r)
	}

	// An override in models.toml, as of a shipped model.
	if err := os.WriteFile(filepath.Join(dir, ModelsFile), []byte("version = 1\n\n[models.\"chatgpt/gpt-5.6-sol\"]\ncontext_window = 1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	over, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m := over.Models["chatgpt/gpt-5.6-sol"]; m.ContextWindow != 1000 || m.Name != "GPT-5.6 Sol (ChatGPT plan)" || over.ModelOrigin("chatgpt/gpt-5.6-sol") != OriginOverridden {
		t.Fatalf("overridden = %+v, origin %q", m, over.ModelOrigin("chatgpt/gpt-5.6-sol"))
	}
}

// TestDiscoveredModelsAreAccountBound (P34): a list another account fetched —
// another subject, or another issued client — is ignored, without a word,
// and so is a list with no registration beside it; a list that is not one is
// one warning; a table with the catalog off learns none. The control is the
// account's own list, which TestDiscoveredModels loads.
func TestDiscoveredModelsAreAccountBound(t *testing.T) {
	for _, tc := range []struct {
		name, client, list string
		catalogOff         bool
		warned             bool
	}{
		{name: "the account's own (control)", client: registration(planSubject, planClient, true), list: planModels(planSubject, planClient)},
		{name: "another subject", client: registration(planSubject, planClient, true), list: planModels("user-subject-0002", planClient)},
		{name: "another client", client: registration(planSubject, planClient, true), list: planModels(planSubject, "app_client-0002")},
		{name: "no registration", list: planModels(planSubject, planClient)},
		{name: "a registration refused", client: registration(planSubject, "", true), list: planModels(planSubject, "")},
		{name: "not a list", client: registration(planSubject, planClient, true), list: "not json", warned: true},
		{name: "a newer list", client: registration(planSubject, planClient, true), list: strings.Replace(planModels(planSubject, planClient), `"version":1`, `"version":2`, 1), warned: true},
		{name: "the catalog off", client: registration(planSubject, planClient, true), list: planModels(planSubject, planClient), catalogOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.catalogOff {
				dir = writeFiles(t, validProviders, validModels)
			}
			signInAs(t, dir, tc.client, true)
			withPlanModels(t, dir, tc.list)
			tbl, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			got := planAliases(tbl)
			control := strings.HasPrefix(tc.name, "the account's own")
			if control != (len(got) == 3) || (!control && len(got) != 0) {
				t.Fatalf("plan models = %q", got)
			}
			if warned := slices.ContainsFunc(tbl.Warnings, func(w string) bool { return strings.Contains(w, "not a ChatGPT plan model list") }); warned != tc.warned {
				t.Fatalf("Warnings = %q, want the unreadable list said: %v", tbl.Warnings, tc.warned)
			}
		})
	}
}

// TestDiscoveredModelsFollowTheAccountSignedIn (plan 033 C14r, r12 #4): a
// table loaded with account A's list keeps the account that list was bound
// to, and once the registration names another account — B signed in while
// the process runs, by its subject, its client id or both — A's models no
// longer resolve: ErrOtherAccount, an unfunded model (ErrNoAPIKey), so a
// picker offers none of them, though a session running on one still lists it
// (P7). The controls: before the switch A's models resolve, and after A signs
// in again — a new sign-in to the same registration — they resolve again.
func TestDiscoveredModelsFollowTheAccountSignedIn(t *testing.T) {
	for _, tc := range []struct{ name, subject, client string }{
		{"another account", "user-subject-0002", "app_client-0002"},
		{"another subject", "user-subject-0002", planClient},
		{"another client", planSubject, "app_client-0002"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := signedInDir(t)
			tbl, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			aliases := planAliases(tbl)
			if len(aliases) != 3 {
				t.Fatalf("premise: plan models = %q", aliases)
			}
			resolves := func() (funded []string) {
				for _, a := range aliases {
					_, err := tbl.Resolve(a, fakeEnv(nil))
					switch {
					case err == nil:
						funded = append(funded, a)
					case !errors.Is(err, ErrOtherAccount) || !errors.Is(err, ErrNoAPIKey):
						t.Fatalf("Resolve(%s) = %v, want ErrOtherAccount", a, err)
					}
				}
				return funded
			}
			if got := resolves(); len(got) != 3 {
				t.Fatalf("control: before the switch %q resolve", got)
			}

			signInAs(t, dir, registration(tc.subject, tc.client, true), true)
			if got := resolves(); len(got) != 0 {
				t.Fatalf("after %s signed in, %q resolve on its sign-in", tc.name, got)
			}
			if got := tbl.Choices(nil, fakeEnv(nil), ""); len(got) != 0 {
				t.Fatalf("Choices offers %+v of the other account's list", got)
			}
			got := tbl.Choices(nil, fakeEnv(nil), "chatgpt/gpt-5.6-sol")
			if len(got) != 1 || got[0].Alias != "chatgpt/gpt-5.6-sol" {
				t.Fatalf("Choices = %+v, want the running model alone", got)
			}

			signInAs(t, dir, registration(planSubject, planClient, true), true)
			if got := resolves(); len(got) != 3 {
				t.Fatalf("control: after the account signed in again %q resolve", got)
			}
		})
	}
}

// TestResolvedCarriesTheListsAccount (plan 033 C14r2, review r13 d): a model
// the table learned from the account's list resolves bound to that account —
// the registration's subject and issued client id, which the driver holds
// every request on the model to — while a plan model the user's models.toml
// adds, which no list named, and a key-funded model resolve bound to none.
func TestResolvedCarriesTheListsAccount(t *testing.T) {
	dir := signedInDir(t)
	if err := os.WriteFile(filepath.Join(dir, ModelsFile), []byte("version = 1\n\n[models.\"mine\"]\nprovider = \"chatgpt\"\nwire_model = \"gpt-5.6-sol\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tbl, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for alias, want := range map[string]Account{
		"chatgpt/gpt-5.6-sol": {Subject: planSubject, ClientID: planClient},
		"chatgpt/gpt-6-astra": {Subject: planSubject, ClientID: planClient},
		"mine":                {},
	} {
		r, err := tbl.Resolve(alias, fakeEnv(nil))
		if err != nil {
			t.Fatalf("Resolve(%s): %v", alias, err)
		}
		if r.Account != want {
			t.Fatalf("Resolve(%s).Account = %+v, want %+v", alias, r.Account, want)
		}
	}
	keyed := 0
	for alias, m := range tbl.Models {
		if p := tbl.Providers[m.Provider]; p.Driver == DriverChatGPT {
			continue
		}
		r, err := tbl.Resolve(alias, func(string) string { return "sk-canary-funded-0001" })
		if err != nil {
			continue
		}
		if keyed++; r.Account != (Account{}) {
			t.Fatalf("the key-funded %s resolved bound to %+v", alias, r.Account)
		}
	}
	if keyed == 0 {
		t.Fatal("premise: no key-funded model resolved")
	}
}

// TestChatGPTFunding (P34): a plan model resolves only while the sign-in
// funds it — the token file a non-empty regular file and plan usage granted —
// and is ErrPlanUsageDisabled when the account declined plan usage and
// ErrNotSignedIn otherwise, both ErrNoAPIKey. Providers says the same of the
// provider, which only a funded sign-in connects.
func TestChatGPTFunding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client string
		tokens string // "file", "empty", "dir", "link" or ""
		via    KeySource
		err    error
	}{
		{"signed in", registration(planSubject, planClient, true), "file", KeySignedIn, nil},
		{"plan usage disabled", registration(planSubject, planClient, false), "", KeyPlanDisabled, ErrPlanUsageDisabled},
		{"disabled, with a token file left", registration(planSubject, planClient, false), "file", KeyPlanDisabled, ErrPlanUsageDisabled},
		{"signed out", registration(planSubject, planClient, true), "", KeyNone, ErrNotSignedIn},
		{"an empty token file", registration(planSubject, planClient, true), "empty", KeyNone, ErrNotSignedIn},
		{"a directory for a token file", registration(planSubject, planClient, true), "dir", KeyNone, ErrNotSignedIn},
		{"a symlink for a token file", registration(planSubject, planClient, true), "link", KeyNone, ErrNotSignedIn},
		{"no registration", "", "file", KeyNone, ErrNotSignedIn},
		{"a registration that is not one", "{not json", "file", KeyNone, ErrNotSignedIn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			signInAs(t, dir, tc.client, false)
			// The list is the registered account's in every case, so the
			// model is in the table, and only the funding differs.
			withPlanModels(t, dir, planModels(planSubject, planClient))
			if tc.client == "" || strings.HasPrefix(tc.client, "{not") {
				// Without a readable registration there is no list either:
				// resolve a model of the user's own on the provider instead.
				if err := os.WriteFile(filepath.Join(dir, ModelsFile), []byte("version = 1\n\n[models.\"mine\"]\nprovider = \"chatgpt\"\nwire_model = \"gpt-5.6-sol\"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			tokens := filepath.Join(dir, ChatGPTAuthDir, ChatGPTTokenFile)
			switch tc.tokens {
			case "file":
				signInAs(t, dir, "", true)
			case "empty":
				if err := os.WriteFile(tokens, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "dir":
				if err := os.Mkdir(tokens, 0o700); err != nil {
					t.Fatal(err)
				}
			case "link":
				target := filepath.Join(t.TempDir(), "elsewhere.json")
				if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, tokens); err != nil {
					t.Fatal(err)
				}
			}
			tbl, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			alias := "chatgpt/gpt-5.6-sol"
			if _, ok := tbl.Models[alias]; !ok {
				alias = "mine"
			}
			r, err := tbl.Resolve(alias, fakeEnv(nil))
			switch {
			case tc.err == nil && (err != nil || r.Auth != AuthSignIn || r.CredentialDir != dir):
				t.Fatalf("Resolve = %+v, %v; want it funded by the sign-in", r, err)
			case tc.err != nil && (!errors.Is(err, tc.err) || !errors.Is(err, ErrNoAPIKey)):
				t.Fatalf("Resolve err = %v, want %v (and ErrNoAPIKey)", err, tc.err)
			}
			infos, err := Providers(dir, fakeEnv(nil))
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(infos, func(p ProviderInfo) bool { return p.ID == ChatGPTProvider })
			if i < 0 {
				t.Fatalf("Providers has no chatgpt: %+v", infos)
			}
			p := infos[i]
			if p.Via != tc.via || !p.SignIn || p.Connected() != (tc.via == KeySignedIn) || p.Name != "ChatGPT plan" || p.EnvVar != "" {
				t.Fatalf("chatgpt = %+v (connected %v), want via %q", p, p.Connected(), tc.via)
			}
		})
	}
}

// TestChoicesKeepPlanModelsInPriorityOrder (A19): the funded plan models are
// offered as one block in the account's own order — sol (priority 0), astra
// (1), luna (3) — not by name, which would put luna first; and a remembered
// one still leads. The control is the name order of every other model.
func TestChoicesKeepPlanModelsInPriorityOrder(t *testing.T) {
	dir := signedInDir(t)
	tbl, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	env := fakeEnv(map[string]string{"OPENROUTER_API_KEY": canary})
	choices := tbl.Choices(nil, env, "")
	var plan, others []string
	for _, c := range choices {
		if c.Provider == ChatGPTProvider {
			plan = append(plan, c.Alias)
		} else {
			others = append(others, c.Name)
		}
	}
	if want := []string{"chatgpt/gpt-5.6-sol", "chatgpt/gpt-6-astra", "chatgpt/gpt-5.6-luna"}; !slices.Equal(plan, want) {
		t.Fatalf("plan models offered %q, want %q", plan, want)
	}
	if len(others) == 0 || !slices.IsSorted(others) {
		t.Fatalf("the other models %q are not in name order", others)
	}
	first := slices.IndexFunc(choices, func(c Choice) bool { return c.Provider == ChatGPTProvider })
	for i, c := range choices[first : first+3] {
		if c.Provider != ChatGPTProvider {
			t.Fatalf("the plan's block is broken at %d: %+v", first+i, choices)
		}
	}
	recent := []RecentEntry{{Alias: "chatgpt/gpt-5.6-luna", Provider: "chatgpt", WireModel: "gpt-5.6-luna"}}
	if got := tbl.Choices(recent, env, ""); got[0].Alias != "chatgpt/gpt-5.6-luna" || got[0].Recent != 1 {
		t.Fatalf("a remembered plan model does not lead: %+v", got[0])
	}
}

// TestChoicesKeepTheRunningPlanModel (choices.go's P7, A19): signed out, the
// plan's models are not offered — but the one a session runs on still is, so
// its picker has its row. The control is the same table with no current
// model, which offers none.
func TestChoicesKeepTheRunningPlanModel(t *testing.T) {
	dir := signedInDir(t)
	if err := os.Remove(filepath.Join(dir, ChatGPTAuthDir, ChatGPTTokenFile)); err != nil { // signed out
		t.Fatal(err)
	}
	tbl, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := tbl.Choices(nil, fakeEnv(nil), ""); len(got) != 0 {
		t.Fatalf("control: signed out with nothing funded offers %+v", got)
	}
	got := tbl.Choices(nil, fakeEnv(nil), "chatgpt/gpt-5.6-sol")
	if len(got) != 1 || got[0].Alias != "chatgpt/gpt-5.6-sol" || got[0].Name != "GPT-5.6 Sol (ChatGPT plan)" {
		t.Fatalf("Choices = %+v, want the running model alone", got)
	}
}

// TestStartModelPrefersTheChatGPTStart (§3.11): with only the plan funded, a
// new session starts on [chatgpt_defaults] start — gpt-5.6-sol, though luna
// sorts first — at its default effort. The controls: a Fireworks key as well
// changes nothing, the plan being ranked above Fireworks (plan 038 §2.1), and
// with sol not on the account's list the first funded alias is taken, as
// before.
func TestStartModelPrefersTheChatGPTStart(t *testing.T) {
	dir := signedInDir(t)
	tbl, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	alias, effort, err := tbl.StartModel(nil, fakeEnv(nil))
	if err != nil || alias != "chatgpt/gpt-5.6-sol" || effort != "medium" {
		t.Fatalf("StartModel = %q, %q, %v; want chatgpt/gpt-5.6-sol at medium", alias, effort, err)
	}
	alias, _, err = tbl.StartModel(nil, fakeEnv(map[string]string{"FIREWORKS_API_KEY": canary}))
	if err != nil || alias != "chatgpt/gpt-5.6-sol" {
		t.Fatalf("with Fireworks funded too StartModel = %q, %v; want the plan's start, ranked first", alias, err)
	}
	withPlanModels(t, dir, strings.Replace(planModels(planSubject, planClient), `"gpt-5.6-sol"`, `"gpt-5.6-terra"`, 1))
	if tbl, err = Load(dir); err != nil {
		t.Fatal(err)
	}
	alias, _, err = tbl.StartModel(nil, fakeEnv(nil))
	if err != nil || alias != "chatgpt/gpt-5.6-luna" {
		t.Fatalf("with no start model listed StartModel = %q, %v; want the first funded alias", alias, err)
	}
}

// TestChatGPTProviderRules (§3.11): the plan's provider is renamed and nothing
// else — a driver, an endpoint, variables or a key written for it is dropped
// with a warning naming the key and never its value — and a provider of the
// user's on the chatgpt driver under another id is dropped, as is the driver
// written for a shipped provider of another id. With the catalog off, the
// files are the whole table, and the same entries fail the load instead.
func TestChatGPTProviderRules(t *testing.T) {
	const providers = "version = 1\n\n" +
		"[providers.chatgpt]\nname = \"My plan\"\ndriver = \"openai-compat\"\nbase_url = \"https://x.example/v1\"\nenv_keys = [\"PLAN_KEY\"]\napi_key = \"" + canary + "\"\n\n" +
		"[providers.mine]\ndriver = \"chatgpt\"\n\n" +
		"[providers.fireworks]\ndriver = \"chatgpt\"\n"
	dir := writeFiles(t, providers, "")
	tbl, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := tbl.Providers[ChatGPTProvider]
	if p.Name != "My plan" || p.Driver != DriverChatGPT || p.BaseURL != "" || len(p.EnvKeys) != 0 || p.APIKey != "" {
		t.Fatalf("chatgpt = %+v; want only its name overridden", p)
	}
	if _, ok := tbl.Providers["mine"]; ok {
		t.Fatal("a provider of the user's on the chatgpt driver was kept")
	}
	if tbl.Providers["fireworks"].Driver != DriverOpenAICompat {
		t.Fatalf("fireworks = %+v; want its shipped driver", tbl.Providers["fireworks"])
	}
	wantWarnings(t, tbl,
		[]string{"providers.chatgpt", "driver", "always driver"},
		[]string{"providers.chatgpt", "base_url", "endpoint is fixed"},
		[]string{"providers.chatgpt", "env_keys", "signing in"},
		[]string{"providers.chatgpt", "api_key", "signing in"},
		[]string{"providers.fireworks", "driver", "alone", "shipped driver"},
		[]string{"providers.mine", "driver", "alone", "ignored"},
	)
	for _, w := range tbl.Warnings {
		if strings.Contains(w, canary) {
			t.Fatalf("a warning quotes the key: %q", w)
		}
	}
	// The variable funds nothing, and is still kept from the commands a
	// session runs, as every name a providers.toml declares is (plan 031
	// C2r2).
	if !slices.Contains(tbl.CredentialEnvNames(), "PLAN_KEY") {
		t.Fatal("the dropped variable is no longer known to hold a credential")
	}

	// Catalog off: the strict rules of a whole table.
	for _, tc := range []struct{ name, providers, table, key string }{
		{"a key", "version = 1\n\n[providers.chatgpt]\ndriver = \"chatgpt\"\napi_key = \"" + canary + "\"\n", "providers.chatgpt", "api_key"},
		{"variables", "version = 1\n\n[providers.chatgpt]\ndriver = \"chatgpt\"\nenv_keys = [\"PLAN_KEY\"]\n", "providers.chatgpt", "env_keys"},
		{"another id", "version = 1\n\n[providers.mine]\ndriver = \"chatgpt\"\n", "providers.mine", "driver"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, tc.providers, "version = 1\ncatalog = false\ndefault_model = \"m\"\n\n[models.m]\nprovider = \"chatgpt\"\nwire_model = \"gpt-5.6-sol\"\n")
			_, err := Load(dir)
			wantFileError(t, err, filepath.Join(dir, ProvidersFile), tc.table, tc.key)
			if strings.Contains(err.Error(), canary) {
				t.Fatal("the error quotes the key")
			}
		})
	}
	// The control: the provider alone, with the catalog off, loads, and its
	// directory is the one its sign-in is read from.
	off := writeFiles(t, "version = 1\n\n[providers.chatgpt]\ndriver = \"chatgpt\"\n", "version = 1\ncatalog = false\ndefault_model = \"m\"\n\n[models.m]\nprovider = \"chatgpt\"\nwire_model = \"gpt-5.6-sol\"\n")
	signInAs(t, off, registration(planSubject, planClient, true), true)
	alone, err := Load(off)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := alone.Resolve("m", fakeEnv(nil)); err != nil || r.CredentialDir != off {
		t.Fatalf("Resolve = %+v, %v", r, err)
	}
}

// TestSetKeyRefusesTheSignInProvider (§3.11): the store takes no key for the
// ChatGPT plan, which signs in, and writes nothing — no file, no lock is
// needed to say so after the file is read — while a stray key a hand put
// there is listed and can be removed. The control is a key provider beside
// it, stored as before.
func TestSetKeyRefusesTheSignInProvider(t *testing.T) {
	dir := t.TempDir()
	if err := SetKey(dir, ChatGPTProvider, canary); !errors.Is(err, ErrSignInProvider) || strings.Contains(err.Error(), canary) {
		t.Fatalf("SetKey(chatgpt) = %v, want ErrSignInProvider without the key", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ProvidersFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused key wrote providers.toml: %v", err)
	}
	if err := SetKey(dir, "fireworks", canary); err != nil {
		t.Fatalf("control: %v", err)
	}
	// A key written for it by hand: listed, funding nothing, and removable.
	b, err := os.ReadFile(filepath.Join(dir, ProvidersFile))
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, []byte("\n[providers.chatgpt]\napi_key = \""+canary+"\"\n")...)
	if err := os.WriteFile(filepath.Join(dir, ProvidersFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	infos, err := Providers(dir, fakeEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range infos {
		if p.ID == ChatGPTProvider && (!p.Stored || p.Via != KeyNone || p.Connected()) {
			t.Fatalf("chatgpt with a stray key = %+v", p)
		}
	}
	if removed, err := RemoveKey(dir, ChatGPTProvider); err != nil || !removed {
		t.Fatalf("RemoveKey(chatgpt) = %v, %v", removed, err)
	}
}

// TestChatGPTDefaultsAreStrict (§3.11, plan 034 §3.1): [chatgpt_defaults]
// decodes as strictly as the rest of the catalog — a key a slug's table does
// not have is refused, and so is start as a string — and validate refuses each
// setting that cannot stand: a missing or malformed models_client_version, a
// start that is empty, repeats a slug or names one that is not, a slug that is
// not one, an effort no request can carry ("ultra"), a default outside its
// efforts, an unknown tool profile, and the section with no chatgpt provider to
// apply to. The control is the shipped catalog, valid.
func TestChatGPTDefaultsAreStrict(t *testing.T) {
	const head = "version = 1\nprovider_order = [\"p\", \"chatgpt\"]\n\n[providers.p]\nname = \"P\"\ndriver = \"openrouter\"\nenv_keys = [\"P_KEY\"]\n\n" +
		"[providers.chatgpt]\nname = \"ChatGPT plan\"\ndriver = \"chatgpt\"\n\n[models.m]\nprovider = \"p\"\nwire_model = \"w\"\n"
	const pin = "\n[chatgpt_defaults]\nmodels_client_version = \"0.160.0\"\n"
	if _, err := parseCatalog(CatalogFile, []byte(head+pin+"\n[chatgpt_defaults.models.\"gpt-x\"]\ncolour = \"red\"\n")); err == nil {
		t.Fatal("an unknown key in a slug's table decoded")
	} else {
		wantFileError(t, err, CatalogFile, `chatgpt_defaults.models.gpt-x`, "colour")
	}
	// start is an array outright: a string is a decode error, never read as
	// a one-slug list.
	if _, err := parseCatalog(CatalogFile, []byte(head+pin+"start = \"gpt-6.1-sol\"\n")); err == nil {
		t.Fatal("start as a string decoded")
	}
	for _, tc := range []struct{ name, section, table, key string }{
		{"no pin", "\n[chatgpt_defaults]\nstart = [\"gpt-x\"]\n", "chatgpt_defaults", "models_client_version"},
		{"no section at all", "", "chatgpt_defaults", "models_client_version"},
		{"an empty pin", "\n[chatgpt_defaults]\nmodels_client_version = \"\"\n", "chatgpt_defaults", "models_client_version"},
		{"a pin with a letter", "\n[chatgpt_defaults]\nmodels_client_version = \"0.160.x\"\n", "chatgpt_defaults", "models_client_version"},
		{"a pin of four parts", "\n[chatgpt_defaults]\nmodels_client_version = \"0.160.0.1\"\n", "chatgpt_defaults", "models_client_version"},
		{"a pin part of seven digits", "\n[chatgpt_defaults]\nmodels_client_version = \"0.1600000.0\"\n", "chatgpt_defaults", "models_client_version"},
		{"a pin with an empty part", "\n[chatgpt_defaults]\nmodels_client_version = \"0..1\"\n", "chatgpt_defaults", "models_client_version"},
		{"an empty start", pin + "start = []\n", "chatgpt_defaults", "start"},
		{"a repeated start", pin + "start = [\"gpt-x\", \"gpt-y\", \"gpt-x\"]\n", "chatgpt_defaults", "start"},
		{"a start that is not a slug", pin + "start = [\"gpt-x\", \"no such/slug\"]\n", "chatgpt_defaults", "start"},
		{"ultra", pin + "[chatgpt_defaults.models.\"gpt-x\"]\nefforts = [\"low\", \"ultra\"]\n", `chatgpt_defaults.models.gpt-x`, "efforts"},
		{"a default effort no request carries", pin + "[chatgpt_defaults.models.\"gpt-x\"]\ndefault_effort = \"ultra\"\n", `chatgpt_defaults.models.gpt-x`, "default_effort"},
		{"a default outside its efforts", pin + "[chatgpt_defaults.models.\"gpt-x\"]\nefforts = [\"low\"]\ndefault_effort = \"high\"\n", `chatgpt_defaults.models.gpt-x`, "default_effort"},
		{"an unknown tool profile", pin + "[chatgpt_defaults.models.\"gpt-x\"]\ntool_profile = \"gpt\"\n", `chatgpt_defaults.models.gpt-x`, "tool_profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseCatalog(CatalogFile, []byte(head+tc.section))
			if err != nil {
				t.Fatal(err)
			}
			wantFileError(t, c.validate(CatalogFile), CatalogFile, tc.table, tc.key)
		})
	}
	// The controls: a pin of one to three parts, leading zeros, and a start
	// of several slugs validate; so does no start at all.
	for _, ok := range []string{pin, "\n[chatgpt_defaults]\nmodels_client_version = \"1\"\nstart = [\"gpt-x\"]\n",
		"\n[chatgpt_defaults]\nmodels_client_version = \"000.0160\"\nstart = [\"gpt-x\", \"gpt-y\"]\n"} {
		c, err := parseCatalog(CatalogFile, []byte(head+ok))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.validate(CatalogFile); err != nil {
			t.Fatalf("control %q: %v", ok, err)
		}
	}
	c, err := ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	delete(c.Providers, ChatGPTProvider)
	c.ProviderOrder = slices.DeleteFunc(c.ProviderOrder, func(id string) bool { return id == ChatGPTProvider })
	wantFileError(t, c.validate(CatalogFile), CatalogFile, "chatgpt_defaults", "")
}
