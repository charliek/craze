package agent

import (
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// hiddenForTest plants a hidden provider for the length of one test.
func hiddenForTest(t *testing.T, name, label string) Provider {
	t.Helper()
	p, restore := RegisterHiddenProviderForTest(name, label)
	t.Cleanup(restore)
	return p
}

// TestHiddenProviderResolvesButIsNeverListed is plan 018 §3.4's
// safe-by-default rule: a hidden provider resolves by id, which is how
// --provider, $CRAZE_PROVIDER and config.toml reach it, and no listing ever
// shows it — so the picker, --help and the unknown-provider text need no
// filter of their own.
func TestHiddenProviderResolvesButIsNeverListed(t *testing.T) {
	hiddenForTest(t, "hush", "Hush")

	p, err := ProviderByName("hush")
	if err != nil {
		t.Fatalf("a hidden provider must resolve by id: %v", err)
	}
	if p.Name() != "hush" || !p.Hidden() || !p.InProcess() {
		t.Fatalf("resolved %q hidden=%v inProcess=%v", p.Name(), p.Hidden(), p.InProcess())
	}
	// Lowercase-exact, as the listed ids are.
	if _, err := ProviderByName("Hush"); err == nil {
		t.Fatal("hidden ids are lowercase-exact too")
	}

	if slices.Contains(ProviderNames(), "hush") {
		t.Fatalf("ProviderNames lists a hidden provider: %q", ProviderNames())
	}
	for _, list := range []struct {
		name string
		got  []Provider
	}{
		{"Providers", Providers()},
		{"DefaultProviders", DefaultProviders()},
	} {
		for _, q := range list.got {
			if q.Name() == "hush" || q.Hidden() {
				t.Fatalf("%s lists the hidden provider %q", list.name, q.Name())
			}
		}
	}
	for _, q := range Providers() {
		if q.Hidden() || q.InProcess() {
			t.Fatalf("listed provider %q is hidden=%v inProcess=%v", q.Name(), q.Hidden(), q.InProcess())
		}
	}
}

// TestHiddenProviderRestoreRemovesIt: the cleanup a test registers leaves the
// registry as it found it, and running it twice is harmless.
func TestHiddenProviderRestoreRemovesIt(t *testing.T) {
	_, restore := RegisterHiddenProviderForTest("hush-gone", "")
	t.Cleanup(restore) // a failure below must not leave it registered
	if _, err := ProviderByName("hush-gone"); err != nil {
		t.Fatalf("not registered: %v", err)
	}
	restore()
	restore()
	if _, err := ProviderByName("hush-gone"); err == nil {
		t.Fatal("restore left the hidden provider resolvable")
	}
}

// TestRegisterHiddenProviderForTestRefusesATakenId: ProviderByName reads the
// listed providers first, so a hidden "grok" could never be reached, and a
// second entry under one id would be removed along with the first.
func TestRegisterHiddenProviderForTestRefusesATakenId(t *testing.T) {
	hiddenForTest(t, "hush-once", "")
	for _, id := range []string{"", cursorName, gxName, "hush-once"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("registering %q did not panic", id)
				}
			}()
			_, restore := RegisterHiddenProviderForTest(id, "")
			restore()
		}()
	}
}

// TestDisplayNameFallsBackToTheId is the label/id split: every ACP provider
// shows its id, so the status bar and the picker goldens are unchanged, and a
// provider with a label shows the label wherever the UI reads DisplayName —
// including a snapshot's ProviderInfo.Label, which rebuilds the provider from
// its id and so has to find hidden ones too.
func TestDisplayNameFallsBackToTheId(t *testing.T) {
	for _, p := range Providers() {
		if p.DisplayName() != p.Name() {
			t.Fatalf("%s shows %q", p.Name(), p.DisplayName())
		}
		if got := p.Info().Label(); got != p.Name() {
			t.Fatalf("%s snapshot label %q", p.Name(), got)
		}
	}
	if got := (ProviderInfo{}).Label(); got != cursorName {
		t.Fatalf("a zero snapshot is labelled %q, want cursor", got)
	}
	if got := (ProviderInfo{Name: "codex"}).Label(); got != "codex" {
		t.Fatalf("an unknown id is labelled %q, want the id", got)
	}

	labelled := hiddenForTest(t, "hush-label", "Hush")
	if labelled.DisplayName() != "Hush" || labelled.Name() != "hush-label" {
		t.Fatalf("labelled provider id %q label %q", labelled.Name(), labelled.DisplayName())
	}
	if got := labelled.Info().Label(); got != "Hush" {
		t.Fatalf("hidden snapshot label %q, want Hush", got)
	}
	if got := labelled.Info().Name; got != "hush-label" {
		t.Fatalf("the snapshot must carry the id, got %q", got)
	}
	bare := hiddenForTest(t, "hush-bare", "")
	if got := bare.Info().Label(); got != "hush-bare" {
		t.Fatalf("an unlabelled provider is labelled %q, want its id", got)
	}
}

// TestInProcessProviderAlwaysResolves: an in-process provider has nothing to
// spawn, so neither PATH nor an override decides whether it is available. The
// environment is one in which every ACP lookup fails, and the ACP provider
// checked first is the control that says so.
func TestInProcessProviderAlwaysResolves(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CRAZE_AGENT_BIN", missing)

	if (Provider{name: "spawned", defaultBins: []string{"spawned"}}).BinaryResolves("") {
		t.Fatal("control: an ACP provider resolved in an environment with no binaries")
	}
	p := hiddenForTest(t, "hush-bin", "")
	if !p.BinaryResolves("") {
		t.Fatal("an in-process provider must resolve with nothing on PATH")
	}
	if !p.BinaryResolves(missing) {
		t.Fatal("an in-process provider must resolve whatever the override says")
	}
}

// TestHiddenListIsSafeUnderConcurrentUse is the reason for hiddenMu: a TUI
// test resolves provider ids from its own goroutines while the test plants and
// removes a hidden provider. -race is what fails this without the lock.
func TestHiddenListIsSafeUnderConcurrentUse(t *testing.T) {
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				_, _ = ProviderByName("hush-race")
			}
		}()
	}
	for range 50 {
		_, restore := RegisterHiddenProviderForTest("hush-race", "")
		restore()
	}
	wg.Wait()
}
