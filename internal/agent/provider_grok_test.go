package agent

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/charliek/craze/internal/acp"
)

// TestGrokProviderData pins the grok value the spawn, auth and TUI cuts read.
func TestGrokProviderData(t *testing.T) {
	p := GrokProvider()
	if p.Name() != "grok" {
		t.Fatalf("name %q", p.Name())
	}
	if got := p.Bins(); !reflect.DeepEqual(got, []string{"grok"}) {
		t.Fatalf("bins %q", got)
	}
	for _, bad := range []string{"agent", "cursor-agent"} {
		if slices.Contains(p.Bins(), bad) {
			t.Fatalf("grok bins must never contain %q", bad)
		}
	}
	if got := p.Dialect(); got != acp.DialectGrok {
		t.Fatalf("dialect %q", got)
	}
	if got := p.AuthMethodIDs(); !reflect.DeepEqual(got, []string{acp.AuthXAIAPIKey, acp.AuthCachedToken}) {
		t.Fatalf("auth ids %q", got)
	}
	if p.LoginHint() != "grok login" {
		t.Fatalf("login hint %q", p.LoginHint())
	}
	wantCaps := Capabilities{
		FastToggle: false, SubagentRows: false,
		Effort: true, Modes: true, Todos: true,
		AskCards: true, PlanCards: true, ParameterizedPicker: true,
	}
	if p.Capabilities() != wantCaps {
		t.Fatalf("capabilities %+v", p.Capabilities())
	}
	scan := p.SkillScan()
	if !reflect.DeepEqual(scan.RelRoots, []string{filepath.Join(".grok", "skills")}) {
		t.Fatalf("rel roots %q", scan.RelRoots)
	}
	if !reflect.DeepEqual(scan.InspectArgs, []string{"inspect", "--json"}) {
		t.Fatalf("inspect args %q", scan.InspectArgs)
	}
	if p.ImplementPrompt() != "Implement the plan above." {
		t.Fatalf("implement prompt %q", p.ImplementPrompt())
	}
}

// TestCursorProviderData pins the current behaviour the seam must preserve.
func TestCursorProviderData(t *testing.T) {
	p := CursorProvider()
	if got := p.Bins(); !reflect.DeepEqual(got, []string{"cursor-agent", "agent"}) {
		t.Fatalf("bins %q", got)
	}
	if got := p.Dialect(); got != acp.DialectCursor {
		t.Fatalf("dialect %q", got)
	}
	if got := p.AuthMethodIDs(); !reflect.DeepEqual(got, []string{acp.AuthCursorLogin}) {
		t.Fatalf("auth ids %q", got)
	}
	if p.LoginHint() != "agent login" {
		t.Fatalf("login hint %q", p.LoginHint())
	}
	caps := p.Capabilities()
	if !caps.FastToggle || !caps.SubagentRows || !caps.ParameterizedPicker {
		t.Fatalf("capabilities %+v", caps)
	}
	if len(p.SkillScan().InspectArgs) != 0 {
		t.Fatalf("cursor has no inspect command: %q", p.SkillScan().InspectArgs)
	}
	if !p.SkillScan().SkipCursorPlugins {
		t.Fatal("cursor still skips .cursor/plugins this cut")
	}
}

// TestProviderArgsOrder pins argv construction per provider, ExtraArgs first.
func TestProviderArgsOrder(t *testing.T) {
	cursor := CursorProvider()
	if got, want := cursor.Args([]string{"-script=echo"}, true),
		[]string{"-script=echo", "--force", "--trust", "acp"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cursor force argv %q", got)
	}
	if got, want := cursor.Args([]string{"-script=echo"}, false),
		[]string{"-script=echo", "--trust", "acp"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cursor argv %q", got)
	}
	grok := GrokProvider()
	if got, want := grok.Args([]string{"-script=echo"}, true),
		[]string{"-script=echo", "--no-auto-update", "agent", "--always-approve", "stdio"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("grok force argv %q", got)
	}
	if got, want := grok.Args([]string{"-script=echo"}, false),
		[]string{"-script=echo", "--no-auto-update", "agent", "stdio"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("grok argv %q", got)
	}
}

// TestProviderByNameLookup pins empty-means-cursor and the unknown-id error.
func TestProviderByNameLookup(t *testing.T) {
	if p, err := ProviderByName(""); err != nil || p.Name() != "cursor" {
		t.Fatalf("empty name = %+v, %v", p, err)
	}
	if p, err := ProviderByName("cursor"); err != nil || p.Name() != "cursor" {
		t.Fatalf("cursor = %+v, %v", p, err)
	}
	if p, err := ProviderByName("grok"); err != nil || p.Name() != "grok" {
		t.Fatalf("grok = %+v, %v", p, err)
	}
	if _, err := ProviderByName("codex"); err == nil {
		t.Fatal("unknown provider must error")
	}
	if _, err := ProviderByName("Grok"); err == nil {
		t.Fatal("provider ids are lowercase-exact")
	}
}

// TestProviderInfoRoundTrip pins the U1 contract: a snapshot rebuilds its
// provider from the name, so capabilities survive the copy.
func TestProviderInfoRoundTrip(t *testing.T) {
	grok := GrokProvider().Info()
	if got := grok.provider(); got.Name() != "grok" {
		t.Fatalf("rebuilt %+v", got)
	}
	if grok.Capabilities().SubagentRows {
		t.Fatal("a grok snapshot must report SubagentRows false")
	}
	if grok.Capabilities().FastToggle {
		t.Fatal("a grok snapshot must report FastToggle false")
	}
	if grok.Dialect() != acp.DialectGrok {
		t.Fatalf("dialect %q", grok.Dialect())
	}
	if !reflect.DeepEqual(grok.SkillScan().InspectArgs, []string{"inspect", "--json"}) {
		t.Fatalf("inspect args %q", grok.SkillScan().InspectArgs)
	}
	cursor := CursorProvider().Info()
	if !cursor.Capabilities().SubagentRows || cursor.Dialect() != acp.DialectCursor {
		t.Fatalf("cursor snapshot %+v %q", cursor.Capabilities(), cursor.Dialect())
	}
}

// TestGrokAuthMethodSelection pins the env-key preference over the daemon's
// default cached_token.
func TestGrokAuthMethodSelection(t *testing.T) {
	grok := GrokProvider()
	both := &acp.InitializeResult{AuthMethods: []acp.AuthMethod{{ID: acp.AuthCachedToken}, {ID: acp.AuthXAIAPIKey}}}
	t.Setenv("XAI_API_KEY", "test-key")
	if id, meta, ok := newSession(Options{Provider: &grok}).authMethod(both); !ok || id != acp.AuthXAIAPIKey {
		t.Fatalf("env key should win: %q %v %v", id, meta, ok)
	} else if meta["headless"] != true {
		t.Fatalf("grok auth carries headless meta: %v", meta)
	}
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	if id, _, ok := newSession(Options{Provider: &grok}).authMethod(both); !ok || id != acp.AuthCachedToken {
		t.Fatalf("without a key the cached token wins: %q %v", id, ok)
	}
	if _, _, ok := newSession(Options{Provider: &grok}).authMethod(&acp.InitializeResult{}); ok {
		t.Fatal("no advertised methods means no login")
	}
	if _, _, ok := newSession(Options{Provider: &grok}).authMethod(&acp.InitializeResult{
		AuthMethods: []acp.AuthMethod{{ID: "grok.com"}},
	}); ok {
		t.Fatal("interactive browser login is never picked")
	}
	if _, _, ok := newSession(Options{Provider: &grok}).authMethod(&acp.InitializeResult{
		AuthMethods: []acp.AuthMethod{{ID: acp.AuthXAIAPIKey}},
	}); ok {
		t.Fatal("advertised api_key without an env key is not usable")
	}
	cursor := CursorProvider()
	if id, meta, ok := newSession(Options{Provider: &cursor}).authMethod(&acp.InitializeResult{
		AuthMethods: []acp.AuthMethod{{ID: acp.AuthCursorLogin}},
	}); !ok || id != acp.AuthCursorLogin || meta != nil {
		t.Fatalf("cursor passes cursor_login with nil meta: %q %v %v", id, meta, ok)
	}
}

func TestSessionCopiesProviderAtNew(t *testing.T) {
	p := GrokProvider()
	s := newSession(Options{Provider: &p})
	p = CursorProvider()
	if s.provider().Name() != "grok" {
		t.Fatalf("mutating the caller's provider changed the session: %q", s.provider().Name())
	}
}

func TestSkillScanReturnsCopies(t *testing.T) {
	p := GrokProvider()
	scan := p.SkillScan()
	scan.RelRoots[0] = "mutated"
	scan.InspectArgs[0] = "mutated"
	got := p.SkillScan()
	if got.RelRoots[0] == "mutated" || got.InspectArgs[0] == "mutated" {
		t.Fatal("SkillScan shared backing storage")
	}
}
