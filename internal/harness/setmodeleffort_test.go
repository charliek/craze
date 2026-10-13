package harness

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// SetModelEffort (plan 047 D1, §7 A1): a person's switch lands on the effort
// it is handed — the native adapter's remembered effort, else the model's
// default — when the new model offers it, and otherwise on the default; it
// never carries the effort the session is on over to another model. The
// same alias is the re-key, which keeps the effort. Model and effort are
// installed in one locked section, and a switch that fails leaves both.

// TestSetModelEffortLands: the landing effort is the one asked for when the
// new model lists it, otherwise its default, otherwise none — never the
// effort the session was on (test/b offers test/a's high, and a switch asked
// for nothing lands on test/b's medium); the same alias keeps the effort the
// session is at whatever is asked; and the next turn's request carries the
// landing effort. Negative control: a landing that carries the current
// effort (SetModel's rule) puts the first case at high.
func TestSetModelEffortLands(t *testing.T) {
	cases := []struct {
		name             string
		from, fromEffort string
		to, effort       string
		want             string
	}{
		{"nothing asked: the default, not the carried effort", "test/a", "high", "test/b", "", "medium"},
		{"asked and offered", "test/a", "low", "test/b", "high", "high"},
		{"asked and not offered: the default, no error", "test/a", "high", "test/b", "low", "medium"},
		{"no effort control", "test/a", "high", "other/c", "high", ""},
		{"from no effort control", "other/c", "", "test/a", "low", "low"},
		{"the same alias keeps its effort", "test/a", "low", "test/a", "high", "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			opts := f.options()
			opts.Model, opts.Effort = tc.from, tc.fromEffort
			s := f.open(opts)
			if err := s.SetModelEffort(tc.to, tc.effort); err != nil {
				t.Fatal(err)
			}
			if m, e := s.Current(); m != tc.to || e != tc.want {
				t.Fatalf("Current() = %q, %q; want %q, %q", m, e, tc.to, tc.want)
			}
			f.models[tc.to].push(answerWith("ok"))
			run(t, s, "hi")
			calls := f.models[tc.to].requests()
			if len(calls) != 1 {
				t.Fatalf("%s saw %d requests, want 1", tc.to, len(calls))
			}
			if got := effortOf(t, calls[0], f.models[tc.to].provider); got != tc.want {
				t.Errorf("the request carried effort %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSetModelEffortInstallsBothAtOnce (A1, P28: through the setModelBuilt
// seam): once the new model's client is built and before it is installed, an
// observer is started and the switch goes on only after the observer's first
// reading — which must be the old model at the old effort — so the observer
// is reading Current as fast as it can across the whole installation, and
// keeps reading until the switch has returned. It sees only the old pair or
// the new one, never test/b at test/a's high, which a switch installing the
// model first (carried) and its effort after would show between the two
// sections; and the switch ends on test/b at medium. The guarantee is
// structural, the one locked section that installs both; this test is a
// strong regression check of it, not a proof — a split installation with no
// pause between its sections can slip between two of the observer's
// readings. Negative control: a setModel that
// installs the model at the carried effort, releases the lock and then sets
// the landing effort is seen at test/b@high.
func TestSetModelEffortInstallsBothAtOnce(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	opts := f.options()
	opts.Model, opts.Effort = "test/a", "high"
	s := f.open(opts)
	var (
		mu    sync.Mutex
		seen  = map[string]int{}
		first string
	)
	observing, done, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.setModelBuilt = func() {
		go func() {
			defer close(stopped)
			for i := 0; ; i++ {
				m, e := s.Current()
				mu.Lock()
				seen[m+"@"+e]++
				if i == 0 {
					first = m + "@" + e
				}
				mu.Unlock()
				if i == 0 {
					close(observing)
				}
				select {
				case <-done:
					return
				default:
				}
			}
		}()
		<-observing // the switch goes on only once the observer is reading
	}
	if err := s.SetModelEffort("test/b", "medium"); err != nil {
		t.Fatal(err)
	}
	close(done)
	<-stopped
	if m, e := s.Current(); m != "test/b" || e != "medium" {
		t.Fatalf("after the switch Current() = %q, %q; want test/b, medium", m, e)
	}
	mu.Lock()
	defer mu.Unlock()
	if first != "test/a@high" {
		t.Fatalf("the observer's first reading, before the installation, is %s; want test/a@high", first)
	}
	for pair := range seen {
		if pair != "test/a@high" && pair != "test/b@medium" {
			t.Fatalf("an observer saw %s during the switch (all seen: %v); want only test/a@high or test/b@medium", pair, seen)
		}
	}
}

// TestSetModelEffortReKeysTheSameModel (A1: the same-alias re-key, through
// the setModelBuilt seam): a switch to the model the session is on rebuilds
// its client and keeps its effort, whatever effort is asked — and when a
// SetTable lands mid-switch, the client is built again from the new table
// (which carries the running model's entry, so its levels are what they
// were), still at the session's own effort. Negative control: a re-key that
// takes the asked effort lands on high in both cases.
func TestSetModelEffortReKeysTheSameModel(t *testing.T) {
	providers, models := fixtureTOML("http://127.0.0.1:1/v1")
	for _, tc := range []struct {
		name  string
		swap  bool
		built string
	}{
		{"the client rebuilt", false, "test/a,test/a"},
		{"a table swapped mid-switch", true, "test/a,test/a,test/a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			opts := f.options()
			opts.Effort = "low"
			s := f.open(opts)
			if tc.swap {
				swapMidSwitch(t, s, tableFrom(t, providers, withModelE(models)))
			}
			if err := s.SetModelEffort("test/a", "high"); err != nil {
				t.Fatal(err)
			}
			if m, e := s.Current(); m != "test/a" || e != "low" {
				t.Fatalf("Current() = %q, %q; want test/a at its own low", m, e)
			}
			if got := strings.Join(f.built, ","); got != tc.built {
				t.Fatalf("built %s; want %s", got, tc.built)
			}
		})
	}
}

// TestASetModelEffortThatFailsChangesNothing (A1): a switch that fails
// leaves the model and the effort exactly as they were — a model with no key,
// an unknown one, one whose effort its driver cannot send (refused inside
// the section that would install it, after its client was built), and one
// whose table changed under both of its builds (errTableChanged). Negative
// control: a setModel that installs the model before it applies the effort
// leaves the session on test/t after the third case.
func TestASetModelEffortThatFailsChangesNothing(t *testing.T) {
	providers, models := fixtureTOML("http://127.0.0.1:1/v1")
	turbo := models + "\n[models.\"test/t\"]\nprovider = \"test\"\nwire_model = \"wire-t\"\nefforts = [\"turbo\"]\ndefault_effort = \"turbo\"\n"
	f := newFixture(t, "http://127.0.0.1:1/v1")
	opts := f.options()
	opts.Effort = "low"
	s := f.open(opts)
	if err := s.SetTable(tableFrom(t, providers, turbo), nil); err != nil {
		t.Fatal(err)
	}
	unchanged := func(what string) {
		t.Helper()
		if m, e := s.Current(); m != "test/a" || e != "low" {
			t.Fatalf("after %s Current() = %q, %q; want test/a, low as before", what, m, e)
		}
	}
	if err := s.SetModelEffort("nokey/d", "high"); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("a model with no key: %v; want ErrNoAPIKey", err)
	}
	unchanged("a model with no key")
	if err := s.SetModelEffort("nope", "high"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("an unknown model: %v; want ErrUnknownModel", err)
	}
	unchanged("an unknown model")
	if err := s.SetModelEffort("test/t", "turbo"); err == nil || !strings.Contains(err.Error(), "turbo") {
		t.Fatalf("an effort the driver cannot send: %v; want it refused", err)
	}
	unchanged("an effort the driver cannot send")
	s.setModelBuilt = func() {
		if err := s.SetTable(tableFrom(t, providers, turbo), nil); err != nil {
			t.Errorf("SetTable: %v", err)
		}
	}
	if err := s.SetModelEffort("test/b", "high"); !errors.Is(err, errTableChanged) {
		t.Fatalf("a table changed under both builds: %v; want errTableChanged", err)
	}
	unchanged("a table changed under both builds")
}

// TestTheCompactionModelKeepsTheConversationsEffort (A1, P29): a person's
// switch lands the session on the new model's effort, but the compaction it
// leads to runs on the model holding the context at the conversation's own
// effort, carried (carriedEffort) — the compaction model is the
// conversation's, not a switch. Here test/a at low, then a switch to test/b
// (a window the context is over) at medium: test/a's summarizer goes out at
// low, and test/b's turn at medium. Negative control: a previous model built
// at its default rather than the carried effort sends the summarizer at
// high.
func TestTheCompactionModelKeepsTheConversationsEffort(t *testing.T) {
	f := newFixture(t, "http://unused")
	windowed(f, "test/a", 1_000_000, 4096)
	windowed(f, "test/b", 100_000, 0)
	opts := f.options()
	opts.Effort = "low"
	s := f.open(opts)
	a, b := f.models["test/a"], f.models["test/b"]
	a.push(answerSpending("one", 90_000)) // well under test/a's threshold, over test/b's
	run(t, s, "turn one")
	if err := s.SetModelEffort("test/b", "medium"); err != nil {
		t.Fatal(err)
	}
	a.push(summaryReply("turn one, on a."))
	b.push(answerWith("two"))
	run(t, s, "turn two")
	ar, br := a.requests(), b.requests()
	if len(ar) != 2 || !isSummarizer(ar[1]) || len(br) != 1 {
		t.Fatalf("requests: test/a %d, test/b %d; want test/a's turn and summarizer, and test/b's turn", len(ar), len(br))
	}
	if got := effortOf(t, ar[1], a.provider); got != "low" {
		t.Fatalf("the summarizer went out at %q; want the conversation's low", got)
	}
	if got := effortOf(t, br[0], b.provider); got != "medium" {
		t.Fatalf("test/b's turn went out at %q; want the switch's medium", got)
	}
}
