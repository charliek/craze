package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// TestProviderRegistryRoundTrip pins Providers() as the one registry:
// ProviderByName and ProviderNames cannot drift from it in either direction.
func TestProviderRegistryRoundTrip(t *testing.T) {
	names := ProviderNames()
	for _, name := range names {
		p, err := ProviderByName(name)
		if err != nil || p.Name() != name {
			t.Fatalf("ProviderByName(%q) = %+v, %v", name, p, err)
		}
	}
	seen := make(map[string]bool)
	for _, p := range Providers() {
		if !slices.Contains(names, p.Name()) {
			t.Fatalf("%q is in Providers but not ProviderNames", p.Name())
		}
		if seen[p.Name()] {
			t.Fatalf("duplicate provider name %q", p.Name())
		}
		seen[p.Name()] = true
	}
	if got := names; !reflect.DeepEqual(got, []string{"cursor", "grok", "native"}) {
		t.Fatalf("Providers order %q, want [cursor grok native]", got)
	}
}

// TestRetiredProviderIsUnknown pins the upgrade path of a provider craze no
// longer lists: its id is an unknown provider, so a config or a flag naming it
// is refused or falls back the way any unknown id does.
func TestRetiredProviderIsUnknown(t *testing.T) {
	if _, err := ProviderByName("gx"); err == nil {
		t.Fatal("gx is retired: ProviderByName must refuse it")
	}
	if slices.Contains(ProviderNames(), "gx") {
		t.Fatalf("ProviderNames lists a retired provider: %q", ProviderNames())
	}
}

// TestProvidersReturnsCopies pins that the registry is unreachable from its
// own result: Providers builds a fresh slice per call, so a caller writing
// through it cannot corrupt ProviderByName for the whole process. Same rule
// as Bins and SkillScan.
func TestProvidersReturnsCopies(t *testing.T) {
	got := Providers()
	got[0] = GrokProvider()
	if Providers()[0].Name() != cursorName {
		t.Fatal("Providers handed back the registry itself")
	}
	if p, err := ProviderByName(cursorName); err != nil || p.Name() != cursorName {
		t.Fatalf("ProviderByName(cursor) = %+v, %v after mutating a Providers result", p, err)
	}
}

// writeFakeBin writes an executable stub at dir/name, mode 0o755 — a 0o644
// fake would make exec.LookPath fail for the wrong reason and pass a
// negative BinaryResolves case by accident.
func writeFakeBin(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestGrokBinaryResolves pins the four cases from plan 012 §5 C1: present on
// PATH, absent, present via $CRAZE_AGENT_BIN, and the exclusive-override
// case where an invalid $CRAZE_AGENT_BIN hides grok even though grok is on
// PATH.
func TestGrokBinaryResolves(t *testing.T) {
	t.Setenv("CRAZE_AGENT_BIN", "")
	grok := GrokProvider()

	dir := t.TempDir()
	writeFakeBin(t, dir, "grok")
	t.Setenv("PATH", dir)
	if !grok.BinaryResolves("") {
		t.Fatal("grok on PATH must resolve")
	}

	t.Setenv("PATH", t.TempDir())
	if grok.BinaryResolves("") {
		t.Fatal("no grok anywhere must not resolve")
	}

	envDir := t.TempDir()
	envBin := writeFakeBin(t, envDir, "grok-fork")
	t.Setenv("CRAZE_AGENT_BIN", envBin)
	if !grok.BinaryResolves("") {
		t.Fatal("$CRAZE_AGENT_BIN must resolve even off PATH")
	}

	// The override is exclusive (§2.7): pointing it at a nonexistent path
	// hides grok even with a real grok sitting on PATH.
	t.Setenv("PATH", dir)
	t.Setenv("CRAZE_AGENT_BIN", filepath.Join(t.TempDir(), "does-not-exist"))
	if grok.BinaryResolves("") {
		t.Fatal("an invalid $CRAZE_AGENT_BIN must hide grok even with grok on PATH")
	}
}

// TestGrokBinaryResolvesExplicit pins the explicit argument itself — the
// --agent-bin value internal/cli hands the picker. Without these cases a
// BinaryResolves that dropped the parameter would still pass, since
// ResolveBinaryCandidates reads $CRAZE_AGENT_BIN out of the environment on
// its own.
func TestGrokBinaryResolvesExplicit(t *testing.T) {
	t.Setenv("CRAZE_AGENT_BIN", "")
	grok := GrokProvider()

	// No grok anywhere, but --agent-bin points at a real binary: grok resolves.
	binDir := t.TempDir()
	bin := writeFakeBin(t, binDir, "some-agent")
	t.Setenv("PATH", t.TempDir())
	if !grok.BinaryResolves(bin) {
		t.Fatal("an explicit --agent-bin must resolve with grok off PATH")
	}

	// Explicit beats $CRAZE_AGENT_BIN, so a bad env value cannot hide grok.
	t.Setenv("CRAZE_AGENT_BIN", filepath.Join(t.TempDir(), "does-not-exist"))
	if !grok.BinaryResolves(bin) {
		t.Fatal("an explicit --agent-bin must win over $CRAZE_AGENT_BIN")
	}

	// And it is exclusive in the other direction too: an unresolvable
	// --agent-bin hides grok even with a real grok on PATH.
	pathDir := t.TempDir()
	writeFakeBin(t, pathDir, "grok")
	t.Setenv("CRAZE_AGENT_BIN", "")
	t.Setenv("PATH", pathDir)
	if grok.BinaryResolves(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Fatal("an unresolvable --agent-bin must hide grok even with grok on PATH")
	}
}
