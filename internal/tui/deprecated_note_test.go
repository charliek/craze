package tui

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// deprecatedCatalog is a catalog whose model "grok" — the stub's current
// model — is deprecated, replaced by acme/fast.
func deprecatedCatalog() *modeltable.Catalog {
	return &modeltable.Catalog{
		ProviderOrder: []string{"acme"},
		Providers:     map[string]modeltable.Provider{"acme": {Name: "Acme", Driver: modeltable.DriverOpenAICompat, BaseURL: "https://api.acme.example/v1", EnvKeys: []string{"ACME_NOTE_API_KEY"}}},
		Models: map[string]modeltable.Model{
			"grok":      {Provider: "acme", WireModel: "acme-old", Deprecated: true, ReplacedBy: "acme/fast"},
			"acme/fast": {Provider: "acme", WireModel: "acme-fast"},
		},
	}
}

// TestADeprecatedModelIsNotedAtOpen (plan 050 C2): a native session that
// opens on a deprecated model says so once, in the waiting commands' words,
// through the TUI's note — the table read by a command off the Update, never
// by the handler: the session's coming up reads nothing, and the note lands
// only when the command's answer is applied. An ACP session on a model of the
// same name schedules nothing; a table that will not load answers nothing.
func TestADeprecatedModelIsNotedAtOpen(t *testing.T) {
	prev := noteTable
	t.Cleanup(func() { noteTable = prev })
	var reads atomic.Int32
	load := func(dir string) (*modeltable.Table, error) {
		reads.Add(1)
		return modeltable.LoadWith(dir, deprecatedCatalog())
	}
	noteTable = load
	const want = "grok is deprecated; use acme/fast"

	open := func(stub *Stub) Model {
		t.Cleanup(func() { _ = stub.Close() })
		return connectModelOf(t, Config{Session: stub, NativeDir: t.TempDir(), Getenv: func(string) string { return "" }})
	}
	m := open(nativeStub())
	if n := reads.Load(); n != 0 || strings.Contains(allNotes(m), "deprecated") {
		t.Fatalf("the session's coming up read the table %d times on the Update, notes %q", n, allNotes(m))
	}
	m.noteDeprecatedModel()
	if reads.Load() != 0 || m.backCmd == nil {
		t.Fatalf("no command scheduled, or the table read on the Update (%d reads)", reads.Load())
	}
	msg, ok := m.backCmd().(deprecatedNoteMsg)
	if !ok || reads.Load() != 1 || msg.text != want {
		t.Fatalf("the command answered %#v after %d reads", msg, reads.Load())
	}
	m.backCmd = nil
	tm, cmd := m.Update(msg)
	if got := allNotes(tm.(Model)); strings.Count(got, want) != 1 || cmd != nil && reads.Load() != 1 {
		t.Fatalf("notes = %q, want %q once", got, want)
	}

	acp := NewStub()
	acp.SetProvider(agent.GrokProvider())
	m = open(acp)
	m.noteDeprecatedModel()
	if m.backCmd != nil {
		t.Fatal("an ACP session scheduled a deprecation note")
	}
	noteTable = func(string) (*modeltable.Table, error) { return nil, errors.New("will not load") }
	m = open(nativeStub())
	m.noteDeprecatedModel()
	if m.backCmd == nil || m.backCmd() != nil {
		t.Fatal("a table that will not load answered a note")
	}
}
