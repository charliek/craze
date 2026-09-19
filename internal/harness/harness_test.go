package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charliek/craze/internal/harness/modeltable"
)

func TestOpen(t *testing.T) {
	cases := []struct {
		name          string
		model, effort string
		wantModel     string
		wantEffort    string
		wantErr       error // nil = opens
	}{
		{"table default at its default effort", "", "", "test/a", "high", nil},
		{"another model at its default effort", "test/b", "", "test/b", "medium", nil},
		{"an effort the model lists", "test/a", "low", "test/a", "low", nil},
		{"a model with no effort control", "other/c", "", "other/c", "", nil},
		{"an unknown alias", "nope", "", "", "", ErrUnknownModel},
		{"a provider with no key", "nokey/d", "", "", "", ErrNoAPIKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			opts := f.options()
			opts.Model, opts.Effort = tc.model, tc.effort
			s, err := Open(opts)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Open = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.Close()
			if m, e := s.Current(); m != tc.wantModel || e != tc.wantEffort {
				t.Fatalf("Current() = %q, %q; want %q, %q", m, e, tc.wantModel, tc.wantEffort)
			}
		})
	}
}

func TestOpenRefuses(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	cases := map[string]func(*Options){
		"no table":                  func(o *Options) { o.Table = nil },
		"an effort the model lacks": func(o *Options) { o.Effort = "medium" },
		"effort with no control":    func(o *Options) { o.Model, o.Effort = "other/c", "low" },
		"a relative home":           func(o *Options) { o.Home = "native" },
		"a relative workspace":      func(o *Options) { o.Workspace = "project" },
		"a model that cannot build": func(o *Options) {
			o.NewModel = func(modeltable.Resolved) (fantasy.LanguageModel, error) { return nil, errors.New("no client") }
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			opts := f.options()
			change(&opts)
			if s, err := Open(opts); err == nil {
				s.Close()
				t.Fatal("Open succeeded")
			}
		})
	}
}

// Open and Close with no turn in between leave nothing on disk.
func TestSessionWithoutATurnWritesNothing(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	if s.ID() == "" {
		t.Fatal("no session id")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.home, "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the sessions directory exists (stat: %v)", err)
	}
}

func TestModels(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	equal(t, "Models()", s.Models(), []ModelInfo{
		{Alias: "nokey/d", Name: "nokey/d", Provider: "nokey"},
		{Alias: "other/c", Name: "other/c", Provider: "other"},
		{Alias: "test/a", Name: "Model A", Provider: "test", Efforts: []string{"low", "high"}, DefaultEffort: "high"},
		{Alias: "test/b", Name: "test/b", Provider: "test", Efforts: []string{"medium", "high"}, DefaultEffort: "medium"},
	})
}

// effortOf is the reasoning effort a scripted call carried, "" for none.
func effortOf(t *testing.T, call fantasy.Call, provider string) string {
	t.Helper()
	v, ok := call.ProviderOptions[provider]
	if !ok {
		return ""
	}
	opts, ok := v.(*openaicompat.ProviderOptions)
	if !ok || opts.ReasoningEffort == nil {
		t.Fatalf("provider options for %q = %#v, want an openaicompat reasoning effort", provider, v)
	}
	return string(*opts.ReasoningEffort)
}

// Effort across a model switch: kept when the new model lists it, else the
// new model's default, else none (plan 018 §3.7).
func TestSetModelCarriesEffort(t *testing.T) {
	cases := []struct {
		from, fromEffort string
		to, wantEffort   string
	}{
		{"test/a", "high", "test/b", "high"},   // listed by both: kept
		{"test/a", "low", "test/b", "medium"},  // not listed: the new default
		{"test/a", "high", "other/c", ""},      // no effort control: none
		{"other/c", "", "test/a", "high"},      // none before: the new default
		{"test/b", "medium", "test/a", "high"}, // not listed: the new default
		{"test/a", "low", "test/a", "low"},     // the same model: kept
	}
	for _, tc := range cases {
		t.Run(tc.from+"@"+tc.fromEffort+"→"+tc.to, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			opts := f.options()
			opts.Model, opts.Effort = tc.from, tc.fromEffort
			s := f.open(opts)
			if err := s.SetModel(tc.to); err != nil {
				t.Fatal(err)
			}
			if m, e := s.Current(); m != tc.to || e != tc.wantEffort {
				t.Fatalf("Current() = %q, %q; want %q, %q", m, e, tc.to, tc.wantEffort)
			}
		})
	}
}

// The model dialog applies model, then effort, as two calls, the effort
// picked from the old model's list (internal/tui/model_dialog.go:280-292).
// The model switch stands whether or not the effort step does.
func TestModelDialogSequence(t *testing.T) {
	cases := []struct {
		name             string
		from, fromEffort string
		to, pickedEffort string
		wantEffort       string
		effortFails      bool
	}{
		{"picked effort listed by both", "test/a", "low", "test/b", "high", "high", false},
		{"picked effort only the old model lists", "test/a", "high", "test/b", "low", "high", true},
		{"old effort unlisted, picked unlisted", "test/a", "low", "test/b", "low", "medium", true},
		{"new model has no effort control", "test/a", "high", "other/c", "low", "", true},
		{"back to a model that lists it", "test/b", "medium", "test/a", "high", "high", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			opts := f.options()
			opts.Model, opts.Effort = tc.from, tc.fromEffort
			s := f.open(opts)
			if err := s.SetModel(tc.to); err != nil {
				t.Fatalf("SetModel: %v", err)
			}
			if err := s.SetEffort(tc.pickedEffort); (err != nil) != tc.effortFails {
				t.Fatalf("SetEffort(%q) = %v, want failure %v", tc.pickedEffort, err, tc.effortFails)
			}
			if m, e := s.Current(); m != tc.to || e != tc.wantEffort {
				t.Fatalf("Current() = %q, %q; want %q, %q", m, e, tc.to, tc.wantEffort)
			}
			// The next turn goes to the new model at that effort.
			f.models[tc.to].push(answerWith("ok"))
			run(t, s, "hi")
			calls := f.models[tc.to].requests()
			if len(calls) != 1 {
				t.Fatalf("%s saw %d requests, want 1", tc.to, len(calls))
			}
			if got := effortOf(t, calls[0], f.models[tc.to].provider); got != tc.wantEffort {
				t.Errorf("the request carried effort %q, want %q", got, tc.wantEffort)
			}
		})
	}
}

func TestSetEffort(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	if err := s.SetEffort("low"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEffort("medium"); err == nil {
		t.Fatal("SetEffort accepted an effort test/a does not list")
	}
	if _, e := s.Current(); e != "low" {
		t.Fatalf("a failed SetEffort changed the effort to %q", e)
	}
	if err := s.SetEffort(""); err != nil {
		t.Fatal(err)
	}
	if _, e := s.Current(); e != "high" {
		t.Fatalf(`SetEffort("") = %q, want the default "high"`, e)
	}
	if err := s.SetModel("other/c"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEffort("low"); err == nil {
		t.Fatal("SetEffort accepted an effort for a model with no effort control")
	}
}

// A switch that cannot be built fails and leaves the session where it was:
// the next turn still goes to the old model.
func TestFailedSetModelKeepsTheModel(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	opts := f.options()
	build := opts.NewModel
	opts.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
		if r.Alias == "test/b" {
			return nil, errors.New("no client for test/b")
		}
		return build(r)
	}
	s := f.open(opts)
	if err := s.SetEffort("low"); err != nil {
		t.Fatal(err)
	}
	for alias, want := range map[string]error{"nope": ErrUnknownModel, "nokey/d": ErrNoAPIKey, "test/b": nil} {
		err := s.SetModel(alias)
		if err == nil || (want != nil && !errors.Is(err, want)) {
			t.Errorf("SetModel(%q) = %v, want %v", alias, err, want)
		}
		if found := leaks(err, canary); len(found) > 0 {
			t.Errorf("SetModel(%q)'s error leaks the key at %v", alias, found)
		}
	}
	if m, e := s.Current(); m != "test/a" || e != "low" {
		t.Fatalf("Current() = %q, %q after failed switches; want test/a, low", m, e)
	}
	f.models["test/a"].push(answerWith("still a"))
	run(t, s, "hi")
	if n := len(f.models["test/a"].requests()); n != 1 {
		t.Fatalf("test/a saw %d requests, want 1", n)
	}
}

// Switching to the current alias rebuilds its client (picking up a key
// exported since), and records nothing.
func TestSetModelToTheCurrentAliasRebuilds(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	if err := s.SetModel("test/a"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	built := append([]string(nil), f.built...)
	f.mu.Unlock()
	equal(t, "models built", built, []string{"test/a", "test/a"})
	f.models["test/a"].push(answerWith("ok"))
	run(t, s, "hi")
	equal(t, "transcript", entries(transcript(t, s)), []string{
		"user test/a high: hi",
		"assistant test/a high end_turn: ok",
	})
}

// Close is idempotent, and everything that would change the session after it
// fails with ErrClosed; reading it still works.
func TestAfterClose(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	for i := range 2 {
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	if _, err := s.Run(context.Background(), "hi", nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Run = %v, want ErrClosed", err)
	}
	if err := s.SetModel("test/b"); !errors.Is(err, ErrClosed) {
		t.Errorf("SetModel = %v, want ErrClosed", err)
	}
	if err := s.SetEffort("low"); !errors.Is(err, ErrClosed) {
		t.Errorf("SetEffort = %v, want ErrClosed", err)
	}
	if m, _ := s.Current(); m != "test/a" || len(s.Models()) != 4 || s.ID() == "" {
		t.Errorf("reads after Close: Current %q, %d models, id %q", m, len(s.Models()), s.ID())
	}
	if n := len(f.models["test/a"].requests()); n != 0 {
		t.Errorf("a closed session sent %d requests", n)
	}
}

func TestEmptyPrompt(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	for _, text := range []string{"", " \n\t"} {
		if _, err := s.Run(context.Background(), text, nil); !errors.Is(err, ErrEmptyPrompt) {
			t.Errorf("Run(%q) = %v, want ErrEmptyPrompt", text, err)
		}
	}
}
