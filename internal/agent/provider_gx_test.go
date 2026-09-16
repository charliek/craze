package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/charliek/craze/internal/acp"
)

// TestGxProviderIsGrokWithFourFieldsOverridden pins the §3.1 structural
// invariant: gx is grok with exactly name, defaultBins, loginHint and
// optional overwritten. Enumerating "the shared fields" by hand would
// silently miss modeKinds, forceAfter, forceArgs, extraGlobalArgs and
// titleTaskFallback, so the whole struct is compared instead — this covers
// every field that exists now and every field added later.
func TestGxProviderIsGrokWithFourFieldsOverridden(t *testing.T) {
	grok := GrokProvider()
	want := grok
	want.name, want.defaultBins, want.loginHint, want.optional = gxName, []string{gxName}, "gx login", true
	gx := GxProvider()
	if !reflect.DeepEqual(want, gx) {
		t.Fatalf("gx = %+v, want %+v", gx, want)
	}
	// The four overrides must actually differ from grok's, so this test
	// cannot pass by accident if GxProvider ever stops overriding one.
	if gx.name == grok.name || gx.loginHint == grok.loginHint || gx.optional == grok.optional ||
		reflect.DeepEqual(gx.defaultBins, grok.defaultBins) {
		t.Fatalf("gx must differ from grok on all four fields: gx=%+v grok=%+v", gx, grok)
	}
}

// TestGxProviderArgs pins argv construction: gx spawns exactly like grok,
// since it is grok on the wire.
func TestGxProviderArgs(t *testing.T) {
	gx := GxProvider()
	want := []string{"--no-auto-update", "agent", "--always-approve", "stdio"}
	if got := gx.Args(nil, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("gx force argv %q, want %q", got, want)
	}
}

// TestGxProviderBins pins the one field GxProvider must not inherit from
// grok: its own binary name, replaced wholesale rather than appended to, and
// returned as a copy so a caller mutating the result cannot affect the next
// call — in the style of TestSkillScanReturnsCopies.
func TestGxProviderBins(t *testing.T) {
	p := GxProvider()
	if got := p.Bins(); !reflect.DeepEqual(got, []string{"gx"}) {
		t.Fatalf("bins %q", got)
	}
	for _, bad := range []string{"grok", "cursor-agent", "agent"} {
		if slices.Contains(p.Bins(), bad) {
			t.Fatalf("gx bins must never contain %q", bad)
		}
	}
	bins := p.Bins()
	bins[0] = "mutated"
	if got := p.Bins(); got[0] == "mutated" {
		t.Fatal("Bins shared backing storage")
	}
}

// TestGxProviderInfoRoundTrip pins that a gx snapshot rebuilds gx's
// capabilities and dialect from the name alone, mirroring
// TestProviderInfoRoundTrip.
func TestGxProviderInfoRoundTrip(t *testing.T) {
	gx := ProviderInfo{Name: "gx"}
	if got := gx.provider(); got.Name() != "gx" {
		t.Fatalf("rebuilt %+v", got)
	}
	if gx.Dialect() != acp.DialectGrok {
		t.Fatalf("dialect %q", gx.Dialect())
	}
	if !gx.Capabilities().SubagentRows || !gx.Capabilities().SubagentTranscript || !gx.Capabilities().Interject {
		t.Fatalf("gx capabilities %+v", gx.Capabilities())
	}
	if gx.Capabilities().FastToggle {
		t.Fatal("a gx snapshot must report FastToggle false")
	}
}

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
	if got := names; !reflect.DeepEqual(got, []string{"cursor", "grok", "gx"}) {
		t.Fatalf("Providers order %q, want [cursor grok gx]", got)
	}
	var defaults []string
	for _, p := range DefaultProviders() {
		defaults = append(defaults, p.Name())
	}
	if !reflect.DeepEqual(defaults, []string{"cursor", "grok"}) {
		t.Fatalf("DefaultProviders %q, want [cursor grok]", defaults)
	}
}

// TestProvidersReturnsCopies pins that the registry is unreachable from its
// own result: Providers builds a fresh slice per call, so a caller writing
// through it cannot corrupt ProviderByName for the whole process. Same rule
// as Bins and SkillScan.
func TestProvidersReturnsCopies(t *testing.T) {
	got := Providers()
	got[0] = GxProvider()
	if Providers()[0].Name() != cursorName {
		t.Fatal("Providers handed back the registry itself")
	}
	if p, err := ProviderByName(cursorName); err != nil || p.Name() != cursorName {
		t.Fatalf("ProviderByName(cursor) = %+v, %v after mutating a Providers result", p, err)
	}
}

// TestProviderOptional pins which providers the picker can hide: cursor and
// grok never, gx only when its binary is missing.
func TestProviderOptional(t *testing.T) {
	if CursorProvider().Optional() {
		t.Fatal("cursor must never be optional")
	}
	if GrokProvider().Optional() {
		t.Fatal("grok must never be optional")
	}
	if !GxProvider().Optional() {
		t.Fatal("gx must be optional")
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

// TestGxBinaryResolves pins the four cases from plan 012 §5 C1: present on
// PATH, absent, present via $CRAZE_AGENT_BIN, and the exclusive-override
// case where an invalid $CRAZE_AGENT_BIN hides gx even though gx is on PATH.
func TestGxBinaryResolves(t *testing.T) {
	t.Setenv("CRAZE_AGENT_BIN", "")
	gx := GxProvider()

	dir := t.TempDir()
	writeFakeBin(t, dir, "gx")
	t.Setenv("PATH", dir)
	if !gx.BinaryResolves("") {
		t.Fatal("gx on PATH must resolve")
	}

	t.Setenv("PATH", t.TempDir())
	if gx.BinaryResolves("") {
		t.Fatal("no gx anywhere must not resolve")
	}

	envDir := t.TempDir()
	envBin := writeFakeBin(t, envDir, "gx-fork")
	t.Setenv("CRAZE_AGENT_BIN", envBin)
	if !gx.BinaryResolves("") {
		t.Fatal("$CRAZE_AGENT_BIN must resolve even off PATH")
	}

	// The override is exclusive (§2.7): pointing it at a nonexistent path
	// hides gx even with a real gx sitting on PATH.
	t.Setenv("PATH", dir)
	t.Setenv("CRAZE_AGENT_BIN", filepath.Join(t.TempDir(), "does-not-exist"))
	if gx.BinaryResolves("") {
		t.Fatal("an invalid $CRAZE_AGENT_BIN must hide gx even with gx on PATH")
	}
}

// TestGxBinaryResolvesExplicit pins the explicit argument itself — the
// --agent-bin value internal/cli hands the picker. Without these cases a
// BinaryResolves that dropped the parameter would still pass, since
// ResolveBinaryCandidates reads $CRAZE_AGENT_BIN out of the environment on
// its own.
func TestGxBinaryResolvesExplicit(t *testing.T) {
	t.Setenv("CRAZE_AGENT_BIN", "")
	gx := GxProvider()

	// No gx anywhere, but --agent-bin points at a real binary: gx resolves.
	binDir := t.TempDir()
	bin := writeFakeBin(t, binDir, "some-agent")
	t.Setenv("PATH", t.TempDir())
	if !gx.BinaryResolves(bin) {
		t.Fatal("an explicit --agent-bin must resolve with gx off PATH")
	}

	// Explicit beats $CRAZE_AGENT_BIN, so a bad env value cannot hide gx.
	t.Setenv("CRAZE_AGENT_BIN", filepath.Join(t.TempDir(), "does-not-exist"))
	if !gx.BinaryResolves(bin) {
		t.Fatal("an explicit --agent-bin must win over $CRAZE_AGENT_BIN")
	}

	// And it is exclusive in the other direction too: an unresolvable
	// --agent-bin hides gx even with a real gx on PATH.
	pathDir := t.TempDir()
	writeFakeBin(t, pathDir, "gx")
	t.Setenv("CRAZE_AGENT_BIN", "")
	t.Setenv("PATH", pathDir)
	if gx.BinaryResolves(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Fatal("an unresolvable --agent-bin must hide gx even with gx on PATH")
	}
}
