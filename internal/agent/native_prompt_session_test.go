package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/journal"
)

// C6's rules through a real Start, where the reading has to happen before
// harness.Open freezes the prompt (§3.4) and the redaction after it.

// systemText is the frozen system prompt as one request carried it. A request
// leads with it, so this is what every turn of the session sends ahead of a
// word the user wrote.
func systemText(t *testing.T, call fantasy.Call) string {
	t.Helper()
	for _, m := range call.Prompt {
		if m.Role == fantasy.MessageRoleSystem {
			return userTextOf(m)
		}
	}
	t.Fatalf("the request carried no system message: %+v", call.Prompt)
	return ""
}

// oneRequest runs one turn and hands back the request it produced.
func (c *nativeContent) oneRequest(t *testing.T, draft string) fantasy.Call {
	t.Helper()
	c.f.models["test/a"].push(answer("done"))
	if _, err := c.Prompt(context.Background(), draft); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	calls := c.f.models["test/a"].requests()
	if len(calls) != 1 {
		t.Fatalf("%d requests, want one", len(calls))
	}
	return calls[0]
}

// jsonStrings is a journal field that should be an array of strings.
func jsonStrings(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("field is %#v, want an array", v)
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("array element is %#v, want a string", e)
		}
		out = append(out, s)
	}
	return out
}

// transcriptHeader is the fixture's one transcript's header line.
func transcriptHeader(t *testing.T, f *nativeFixture) map[string]any {
	t.Helper()
	var head map[string]any
	if err := json.Unmarshal([]byte(transcriptOf(t, f)[0]), &head); err != nil {
		t.Fatalf("the transcript's header is not JSON: %v", err)
	}
	return head
}

// TestNativeStartFreezesInstructionsAndCatalog is the whole of C6 end to end:
// the files the session read are in the prompt of its first request, under
// craze's own framing, with the menu's names and the paths that load them.
func TestNativeStartFreezesInstructionsAndCatalog(t *testing.T) {
	f := newNativeFixture(t)
	s := startContent(t, f, Options{},
		map[string]string{
			"CLAUDE.md":                     "Run make lint before every commit.\n",
			".claude/commands/ship.md":      commandFile("ship it", "Ship $ARGUMENTS."),
			".claude/skills/build/SKILL.md": skillDoc("build", "build the thing"),
		}, nil)
	sent := systemText(t, s.oneRequest(t, "hi"))

	for _, want := range []string{
		"# Project and user instructions",
		"## From: " + filepath.Join(s.ws, "CLAUDE.md"),
		"Run make lint before every commit.",
		"# Skills and commands",
		"- ship (command): ship it",
		"  Path: " + filepath.Join(s.ws, ".claude", "commands", "ship.md"),
		"- build (skill): build the thing",
	} {
		if !strings.Contains(sent, want) {
			t.Errorf("the frozen prompt lacks %q:\n%s", want, sent)
		}
	}
	// A project's entry is not a plugin's, so it is offered no root (§3.4).
	if strings.Contains(sent, "  Root: ") {
		t.Errorf("a pseudo-plugin row was given a Root:\n%s", sent)
	}
}

// TestNativeStartWithNoContentSendsTheProfileAlone: the same session over an
// empty tree sends the tool profile's text and nothing after it, which is what
// keeps a craze session with no content sending exactly what it sent before
// H4 (D-30).
func TestNativeStartWithNoContentSendsTheProfileAlone(t *testing.T) {
	f := newNativeFixture(t)
	s := startContent(t, f, Options{}, nil, nil)
	sent := systemText(t, s.oneRequest(t, "hi"))
	for _, unwanted := range []string{"# Project and user instructions", "# Skills and commands"} {
		if strings.Contains(sent, unwanted) {
			t.Errorf("an empty tree still framed %q:\n%s", unwanted, sent)
		}
	}
}

// TestNativeStartPassesTheTogglesThrough is A13 at the session: a toggle on
// the Options reaches the scan and the loader, and removes its class from the
// prompt as well as from the menu.
func TestNativeStartPassesTheTogglesThrough(t *testing.T) {
	f := newNativeFixture(t)
	s := startContent(t, f, Options{Compat: ClaudeCompat{NoInstructions: true, NoSkills: true}},
		map[string]string{
			"CLAUDE.md":                     "Run make lint before every commit.\n",
			".claude/commands/ship.md":      commandFile("ship it", "Ship $ARGUMENTS."),
			".claude/skills/build/SKILL.md": skillDoc("build", "build the thing"),
		}, nil)
	sent := systemText(t, s.oneRequest(t, "hi"))
	wantDisplays(t, s.Snapshot().Plugins, "ship")
	for _, unwanted := range []string{"# Project and user instructions", "Run make lint", "- build (skill)"} {
		if strings.Contains(sent, unwanted) {
			t.Errorf("the prompt still holds %q:\n%s", unwanted, sent)
		}
	}
	if !strings.Contains(sent, "- ship (command): ship it") {
		t.Errorf("the toggles took a class nobody turned off:\n%s", sent)
	}
}

// TestNativeStartFailureLeavesNoRows: the content is read before Open now, so
// a session whose harness refuses to open has to throw the reading away rather
// than keep a menu built for a session that never existed.
func TestNativeStartFailureLeavesNoRows(t *testing.T) {
	f := newNativeFixture(t)
	base := t.TempDir()
	ws := writeTree(t, filepath.Join(base, "ws"), map[string]string{
		".claude/commands/ship.md": commandFile("ship it", "ship body"),
	})
	gitDir(t, ws)
	s := f.session(Options{
		Workspace:   ws,
		ContentHome: writeTree(t, filepath.Join(base, "home"), nil),
		Model:       "nokey/d", // the one alias whose provider has no key
	})
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded on a model whose provider has no key")
	}
	if snap := s.Snapshot(); len(snap.Plugins) != 0 {
		t.Fatalf("a session that never opened has rows: %+v", snap.Plugins)
	}
}

// TestNativePromptSourcesNote is §3.4's provenance through a real session: the
// prompt is never stored, so this note is the only record of which files went
// into it. Its digest is the transcript header's own, so the two can be read
// together.
func TestNativePromptSourcesNote(t *testing.T) {
	f := newNativeFixture(t)
	dir := filepath.Join(t.TempDir(), "journal")
	s := startContent(t, f, Options{JournalDir: dir},
		map[string]string{
			"CLAUDE.md":                "Run make lint before every commit.\n",
			".claude/commands/ship.md": commandFile("ship it", "Ship $ARGUMENTS."),
		}, nil)
	w := journalOf(t, s.log)
	inc := s.Incarnation()
	s.oneRequest(t, "hi")
	head := transcriptHeader(t, f)
	closeJournaled(t, s, w)

	notes := diags(assertOneJournal(t, dir, w, inc), journal.DiagPromptSources)
	if len(notes) != 1 {
		t.Fatalf("%d prompt_sources notes, want exactly one", len(notes))
	}
	fields := notes[0]
	if got, want := jsonString(fields, "prompt_sha256"), jsonString(head, "system_prompt_sha256"); got == "" || got != want {
		t.Fatalf("the note's prompt_sha256 is %q, the transcript header's %q", got, want)
	}
	if n, _ := fields["prompt_bytes"].(float64); n <= 0 {
		t.Fatalf("prompt_bytes is %v", fields["prompt_bytes"])
	}
	docs, cat := jsonStrings(t, fields["instructions"]), jsonStrings(t, fields["catalog"])
	if len(docs) != 1 || !strings.HasSuffix(docs[0], filepath.Join(s.ws, "CLAUDE.md")) {
		t.Fatalf("instructions %v, want one element naming the project's file", docs)
	}
	if len(cat) != 1 || !strings.HasSuffix(cat[0], filepath.Join(s.ws, ".claude", "commands", "ship.md")) {
		t.Fatalf("catalog %v, want one element naming the command's file", cat)
	}
	// "<sha256> <bytes> <path>", in that order.
	for _, line := range append(docs, cat...) {
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 || len(parts[0]) != 64 || !filepath.IsAbs(parts[2]) {
			t.Fatalf("element %q is not \"<sha256> <bytes> <path>\"", line)
		}
	}
}

// TestNativePromptSourcesNoteWithNoContent: a session that read nothing still
// writes the note, with two empty classes — "craze looked and found nothing"
// is a different fact from "craze never looked", and afterwards only the note
// can tell them apart.
func TestNativePromptSourcesNoteWithNoContent(t *testing.T) {
	f := newNativeFixture(t)
	dir := filepath.Join(t.TempDir(), "journal")
	s := startContent(t, f, Options{JournalDir: dir}, nil, nil)
	w := journalOf(t, s.log)
	inc := s.Incarnation()
	closeJournaled(t, s, w)

	notes := diags(assertOneJournal(t, dir, w, inc), journal.DiagPromptSources)
	if len(notes) != 1 {
		t.Fatalf("%d prompt_sources notes, want exactly one", len(notes))
	}
	for _, key := range []string{"instructions", "catalog"} {
		if got := jsonStrings(t, notes[0][key]); len(got) != 0 {
			t.Fatalf("%s is %v, want an empty array", key, got)
		}
	}
}

// TestNativeSaysWhenThePromptIsLarge is R1's signal. The renderer's budgets
// allow more than this, deliberately, so the size is a diagnostic and never a
// refusal: the files are the user's own and craze is not the one to decide
// there are too many of them. What the owner should know is that this prompt
// now rides on every request of the session, with no compaction until H7.
func TestNativeSaysWhenThePromptIsLarge(t *testing.T) {
	big := strings.Repeat("a line of instructions that is long enough to count.\n", 800)
	f := newNativeFixture(t)
	s := startContent(t, f, Options{},
		map[string]string{"CLAUDE.md": big},
		map[string]string{".claude/CLAUDE.md": big})
	sent := systemText(t, s.oneRequest(t, "hi"))
	if len(sent) <= maxNativePromptBytes {
		t.Fatalf("the prompt is %d bytes, which is not over the mark this test is about", len(sent))
	}
	if !strings.Contains(s.diag.String(), "the system prompt is") {
		t.Fatalf("a %d-byte prompt said nothing: %q", len(sent), s.diag.String())
	}

	// And an ordinary session says nothing at all.
	f2 := newNativeFixture(t)
	small := startContent(t, f2, Options{}, map[string]string{"CLAUDE.md": "short.\n"}, nil)
	if strings.Contains(small.diag.String(), "the system prompt is") {
		t.Fatalf("an ordinary session reported its prompt size: %q", small.diag.String())
	}
}

// TestNativeCanaryNeverReachesThePrompt is A8 extended to everything C6 added,
// over every field rather than over the ones a previous round happened to
// name. The key is planted everywhere the prompt and the menu now read from —
// text that can be redacted (an instruction file, a command body, a catalog
// description) and identities that cannot (a row's path, a frontmatter name,
// a plugin id) — and must reach none of: the wire, the turn's own text, the
// transcript, craze's diagnostics, the journal (the prompt_sources note
// included), the snapshot the slash menu draws, or the session's events.
//
// The assertions walk whole values through nativeLeaks rather than picking
// fields out of them, which is the point of this round: the version that
// checked Text, Path and Description by hand missed Plugin, Bare, Display and
// Qualified, and those are exactly the four an identity leaks through. `craze
// prompt --json` prints a subset of the event's fields (internal/cli's
// commandJSON: bare, qualified, plugin, kind, path, text), so an event with
// the key in no field at all cannot produce one; tests/cli/test_native.py
// runs that half against the real binary.
func TestNativeCanaryNeverReachesThePrompt(t *testing.T) {
	f := newNativeFixture(t)
	dir := filepath.Join(t.TempDir(), "journal")
	base := t.TempDir()
	wsDir := writeTree(t, filepath.Join(base, "ws"), map[string]string{
		"CLAUDE.md":                   "The key is " + nativeCanary + ".\n",
		".claude/commands/leak.md":    commandFile("a command", "export KEY="+nativeCanary),
		".claude/skills/say/SKILL.md": skillDoc("say", "described as "+nativeCanary),
		// A skill whose directory — and therefore whose name and path — is
		// the key itself. Its entry is dropped rather than redacted (§3.4).
		".claude/skills/" + nativeCanary + "/SKILL.md": skillDoc(nativeCanary, "named after the key"),
		// And a command whose frontmatter name is the key while its file is
		// called something else: the name is the identity, not the filename.
		".claude/commands/named.md": commandDoc("names itself after the key", "named body",
			"name: "+nativeCanary+"-cmd"),
	})
	gitDir(t, wsDir)
	homeDir := writeTree(t, filepath.Join(base, "home"), nil)
	// An enabled plugin whose *id* is the key. Nothing the user types names
	// it — they type /deploy — so without the gate the key would ride out in
	// the event's plugin and qualified fields for a command they asked for by
	// another name entirely.
	install := writeTree(t, filepath.Join(base, "plugin"), map[string]string{
		"commands/deploy.md": commandFile("a plugin's own command", "deploy body"),
	})
	claudeFixture{
		installs: map[string][]claudeInstall{nativeCanary + "@mkt": {{Scope: "user", InstallPath: install}}},
		user:     map[string]bool{nativeCanary + "@mkt": true},
	}.write(t, homeDir, wsDir)

	s := startTrees(t, f, Options{JournalDir: dir}, wsDir, homeDir)
	w := journalOf(t, s.log)
	inc := s.Incarnation()

	// /deploy is typed alongside the one that does expand: the user never sees
	// the plugin id, so a suppressed entry has to be unreachable by the name
	// they do type. The spellings that hold the key are resolved below rather
	// than typed, because a draft carrying the key would be the user's own
	// text and would make every assertion here pass or fail for that reason.
	call := s.oneRequest(t, "/leak\n/deploy")
	sent := systemText(t, call)
	// Nothing here may pass vacuously: the instruction file and the row whose
	// description holds the key are both in the prompt, redacted.
	if !strings.Contains(sent, "The key is ") {
		t.Fatalf("the instruction file did not reach the prompt at all:\n%s", sent)
	}
	if !strings.Contains(sent, "- say (skill): described as ") {
		t.Fatalf("the row whose description holds the key was not listed at all:\n%s", sent)
	}
	if strings.Contains(sent, nativeCanary) {
		t.Fatalf("the canary reached the frozen prompt:\n%s", sent)
	}
	if strings.Contains(firstUserText(t, call), nativeCanary) {
		t.Fatal("the canary reached the turn's own text")
	}
	// Each suppressed entry is gone from the catalog whole, not listed with a
	// marker where its name was.
	for _, gone := range []string{"named after the key", "names itself after the key", "a plugin's own command"} {
		if strings.Contains(sent, gone) {
			t.Fatalf("an entry whose identity holds a key was listed (%q):\n%s", gone, sent)
		}
	}
	if strings.Contains(s.diag.String(), nativeCanary) {
		t.Fatalf("the canary reached craze's diagnostics: %q", s.diag.String())
	}
	// One expansion, the clean one.
	evs := drained(s)
	cmds := ofType(evs, EventCommand)
	if len(cmds) != 1 || cmds[0].Command.Bare != "leak" {
		t.Fatalf("%d command events (%v), want the clean one alone", len(cmds), cmds)
	}
	// And a suppressed entry answers to neither of its spellings: the bare
	// name its author gave it and the qualified one the menu would have drawn
	// are both resolved against the session's own rows, and neither finds it.
	s.mu.Lock()
	refs := s.refsLocked("/" + nativeCanary + "-cmd\n/" + nativeCanary + ":deploy\n/user:" + nativeCanary)
	s.mu.Unlock()
	if len(refs) != 0 {
		t.Fatalf("%d suppressed entries are still expandable: %+v", len(refs), refs)
	}
	// The whole of what a consumer can see, field by field: the events, the
	// snapshot the slash menu is drawn from, and the requests that went out.
	// A menu row's *name* is deliberately never redacted (X13) — it has to
	// keep matching what the user types — which is why a row whose name would
	// hold the key is not here at all.
	for what, v := range map[string]any{
		"the events":   evs,
		"the snapshot": s.Snapshot(),
		"the wire":     f.models["test/a"].requests(),
	} {
		if leaks := nativeLeaks(v, nativeCanary); len(leaks) > 0 {
			t.Fatalf("the canary leaked into %s at %v", what, leaks)
		}
	}
	for _, line := range transcriptOf(t, f) {
		if strings.Contains(line, nativeCanary) {
			t.Fatalf("the canary reached the transcript: %s", line)
		}
	}
	closeJournaled(t, s, w)
	lines := assertOneJournal(t, dir, w, inc)
	if len(diags(lines, journal.DiagPromptSources)) != 1 {
		t.Fatalf("%d prompt_sources notes, want one", len(diags(lines, journal.DiagPromptSources)))
	}
	if raw, err := os.ReadFile(w.Path()); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(raw), nativeCanary) {
		t.Fatalf("the canary reached the journal:\n%s", raw)
	}
}

// TestNativeLoaderDiagnosticsAreRedacted is the other half of A8's "craze's
// diagnostics" clause, and the half that was open: the content is read before
// harness.Open, so the loaders' own lines reach Diag before the session has a
// redactor at all. Every one of them interpolates something off disk — an
// import as its author wrote it, the path it resolves to, a filename craze
// cannot offer as a name — so the lane itself is redacted (contentWarn).
//
// The controls matter as much as the assertion: each line has to be produced,
// or a loader that stopped writing it would leave this passing for the wrong
// reason.
func TestNativeLoaderDiagnosticsAreRedacted(t *testing.T) {
	f := newNativeFixture(t)
	s := startContent(t, f, Options{}, map[string]string{
		// An import that resolves outside the repository, whose reference and
		// whose root are both written into the line.
		"CLAUDE.md": "@/nowhere/" + nativeCanary + "/missing.md\n",
		// A file whose *name* craze could never offer as a slash command, in
		// a directory named after the key.
		".claude/commands/" + nativeCanary + " bad.md": commandFile("unusable", "body"),
	}, nil)

	diag := s.diag.String()
	for _, want := range []string{"not read", "not a usable name"} {
		if !strings.Contains(diag, want) {
			t.Fatalf("control: the loaders wrote no %q line, so redacting one proves nothing: %q", want, diag)
		}
	}
	if strings.Contains(diag, nativeCanary) {
		t.Fatalf("a loader diagnostic carried the key: %q", diag)
	}
	// Each line is one line: a path holding a newline would otherwise write a
	// second, in craze's own voice, saying whatever the checkout chose.
	for _, line := range strings.Split(strings.TrimSpace(diag), "\n") {
		if strings.TrimSpace(line) == "" {
			t.Fatalf("a diagnostic wrote a blank line: %q", diag)
		}
	}
}

// TestNativeDiagnosticsQuoteAName is the %q half of that: a filename may hold
// a newline, and with %s the rest of it is a second diagnostic line in
// craze's own voice. Every path and every name in a content diagnostic is
// written with %q, which folds it back onto one line and escapes the control
// bytes with it.
func TestNativeDiagnosticsQuoteAName(t *testing.T) {
	f := newNativeFixture(t)
	forged := "bad\ncraze: read etc-shadow"
	s := startContent(t, f, Options{}, map[string]string{
		".claude/commands/" + forged + ".md": commandFile("unusable", "body"),
	}, nil)

	diag := s.diag.String()
	if !strings.Contains(diag, "not a usable name") {
		t.Fatalf("control: no name was refused, so quoting one proves nothing: %q", diag)
	}
	if strings.Contains(diag, "\ncraze: read etc-shadow") {
		t.Fatalf("a filename's newline wrote a second diagnostic line: %q", diag)
	}
}

// TestNativeResolvesTheKeysOnce is the third-round hazard behind the gate: a
// session resolves the table's keys twice — here, to judge what it read off
// disk before the prompt is frozen, and again inside harness.Open, to build
// the redactor over what it froze — and an environment that answered
// differently between the two would leave the gate judging one set and the
// redactor covering another. Reading the environment once (onceGetenv) makes
// them one set by construction.
//
// The environment here answers with the key the first time it is asked and
// with another value afterwards, which is the sharpest form of the hazard:
// the entry named after the first reading must be gone, and the session's own
// redactor must cover that same first reading rather than the second.
func TestNativeResolvesTheKeysOnce(t *testing.T) {
	const second = "sk-second-reading-not-a-secret"
	f := newNativeFixture(t)
	reads := 0
	f.getenv = func(name string) string {
		if name != "NATIVE_TEST_KEY" {
			return f.env[name]
		}
		reads++
		if reads == 1 {
			return nativeCanary
		}
		return second
	}
	s := startContent(t, f, Options{}, map[string]string{
		".claude/skills/" + nativeCanary + "/SKILL.md": skillDoc(nativeCanary, "named after the first reading"),
		".claude/commands/ship.md":                     commandFile("ship it", "ship body"),
	}, nil)

	// The gate judged the entry against the first reading.
	wantDisplays(t, s.Snapshot().Plugins, "ship")
	// And so did the harness: its redactor covers that same value, so nothing
	// the gate let through can carry a key the redactor does not know.
	if got := s.hs.Redact("before " + nativeCanary + " after"); strings.Contains(got, nativeCanary) {
		t.Fatalf("the session's redactor does not cover the key the gate used: %q", got)
	}
	// The later readings never became keys of this session, which is the
	// other half of "one set": the memo answered from the first one.
	if got := s.hs.Redact("before " + second + " after"); !strings.Contains(got, second) {
		t.Fatalf("a value the environment only offered later became a key: %q", got)
	}
}

// TestNativeLearnsAKeyExportedAfterStart is the other half of that seal, and
// the capability it must not cost: the memo covers the startup window and is
// released when open() returns, so the harness reads the environment again
// from then on. A switch to a provider whose key the environment gained since
// Open resolves it and the session's redactor grows to cover it
// (toolset.resolve) — which a memo held for the session's life would have
// turned into a permanent failure.
//
// The control is the same switch before the key exists: it fails, so the
// success below is the export being seen and not the model having been
// funded all along.
func TestNativeLearnsAKeyExportedAfterStart(t *testing.T) {
	const exported = "sk-exported-after-start-not-a-secret"
	f := newNativeFixture(t)
	// Atomic rather than a write to f.env: the closure is read from the
	// harness's goroutines as well as this one.
	var live atomic.Bool
	f.getenv = func(name string) string {
		if name == "NATIVE_NOKEY_KEY" {
			if live.Load() {
				return exported
			}
			return ""
		}
		return f.env[name]
	}
	s := startContent(t, f, Options{}, nil, nil)

	if err := s.SetModel(context.Background(), "nokey/d"); err == nil {
		t.Fatal("control: a switch to the unfunded provider succeeded before its key existed")
	}
	live.Store(true)
	if err := s.SetModel(context.Background(), "nokey/d"); err != nil {
		t.Fatalf("a switch to a provider whose key was exported after Open: %v", err)
	}
	if got := s.Snapshot().CurrentModel; got != "nokey/d" {
		t.Fatalf("the session is on %q, want the model it switched to", got)
	}
	// And the session's redactor grew to cover the key it learned, which is
	// what reading the environment again is for.
	if got := s.hs.Redact("before " + exported + " after"); strings.Contains(got, exported) {
		t.Fatalf("the session did not learn the key it switched to: %q", got)
	}
}

// TestNativeCloseDuringStartOpensNoHarness: the content is read before
// harness.Open, which lengthened the window in which a Close finds no harness
// under s.mu and returns while Start reads on. The reading itself is not
// cancellable — that is left for the start/stop lifecycle — but nothing is
// opened for a session that is already closed.
//
// The Close is made from inside the seam, which runs after the workspace is
// resolved and before a byte is read, so the race is the ordering under test
// rather than a matter of timing.
func TestNativeCloseDuringStartOpensNoHarness(t *testing.T) {
	f := newNativeFixture(t)
	built := 0
	var s *nativeSession
	f.edit = func(o *harness.Options) {
		o.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
			built++
			return f.models[r.Alias], nil
		}
		if s != nil {
			if err := s.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}
	}
	base := t.TempDir()
	ws := writeTree(t, filepath.Join(base, "ws"), map[string]string{
		".claude/commands/ship.md": commandFile("ship it", "ship body"),
	})
	gitDir(t, ws)
	home := writeTree(t, filepath.Join(base, "home"), nil)

	// The control first, with the same seam and nothing closed: the session
	// opens, so the assertion below is about the Close and not about the
	// tree.
	control := f.session(Options{Workspace: ws, ContentHome: home})
	if err := control.Start(context.Background()); err != nil {
		t.Fatalf("control: Start: %v", err)
	}
	if built != 1 {
		t.Fatalf("control: the harness built %d models, want one", built)
	}

	built = 0
	s = f.session(Options{Workspace: ws, ContentHome: home})
	err := s.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "session closed") {
		t.Fatalf("Start = %v, want the closed refusal", err)
	}
	if built != 0 {
		t.Fatal("a session closed during its own start still opened a harness")
	}
	if snap := s.Snapshot(); len(snap.Plugins) != 0 || snap.SessionID != "" {
		t.Fatalf("a session closed during its own start kept state: %+v", snap)
	}
}

// TestNativeTweakSuppliesPromptExtras is the seam's contract on the one field
// the adapter also fills: tweak is a test's last word on harness.Options
// (NewNative), and Options.Prompt used to be overwritten after it ran, so a
// case could not hand in extras of its own. What the adapter read is still
// used for the menu — the seam replaces what the model is told exists, not
// what the user can type.
func TestNativeTweakSuppliesPromptExtras(t *testing.T) {
	f := newNativeFixture(t)
	f.edit = func(o *harness.Options) {
		o.Prompt = harness.PromptExtras{
			Instructions: []harness.PromptDoc{{Path: "/seam/CLAUDE.md", Text: "the seam's own document\n"}},
		}
	}
	s := startContent(t, f, Options{}, map[string]string{
		"CLAUDE.md":                "the workspace's own document\n",
		".claude/commands/ship.md": commandFile("ship it", "ship body"),
	}, nil)
	sent := systemText(t, s.oneRequest(t, "hi"))

	if !strings.Contains(sent, "the seam's own document") {
		t.Fatalf("the seam's extras did not reach the prompt:\n%s", sent)
	}
	for _, unwanted := range []string{"the workspace's own document", "- ship (command): ship it"} {
		if strings.Contains(sent, unwanted) {
			t.Fatalf("the adapter's own extras overwrote the seam's (%q):\n%s", unwanted, sent)
		}
	}
	// The menu is still the session's own reading.
	wantDisplays(t, s.Snapshot().Plugins, "ship")
}

// TestNativeReadsOnlyTheSeamsRoots is A14's second half. The seam is
// Options.ContentHome (§3.1): with it pointed at one tree and the process's
// own HOME holding another, everything the prompt and the menu carry comes
// from the seam's tree — and nothing under the home's .claude is written,
// which is the other half of the owner's decision that craze reads Claude's
// content live and never edits it.
func TestNativeReadsOnlyTheSeamsRoots(t *testing.T) {
	f := newNativeFixture(t)
	decoy := writeTree(t, filepath.Join(t.TempDir(), "decoy"), map[string]string{
		".claude/CLAUDE.md":             "decoy instructions\n",
		".claude/commands/decoy.md":     commandFile("a decoy command", "decoy body"),
		".claude/skills/decoy/SKILL.md": skillDoc("decoy", "a decoy skill"),
	})
	t.Setenv("HOME", decoy)
	before := treeState(t, filepath.Join(decoy, ".claude"))

	s := startContent(t, f, Options{},
		map[string]string{".claude/commands/own.md": commandFile("the project's own", "own body")},
		map[string]string{".claude/CLAUDE.md": "the seam's instructions\n"})
	sent := systemText(t, s.oneRequest(t, "hi"))

	wantDisplays(t, s.Snapshot().Plugins, "own")
	if !strings.Contains(sent, "the seam's instructions") {
		t.Errorf("the seam's own tree was not read:\n%s", sent)
	}
	for _, unwanted := range []string{"decoy instructions", "decoy command", "a decoy skill"} {
		if strings.Contains(sent, unwanted) {
			t.Errorf("the process's home was read past the seam (%q):\n%s", unwanted, sent)
		}
	}
	if after := treeState(t, filepath.Join(decoy, ".claude")); after != before {
		t.Fatalf("a session wrote under the home's .claude:\n before %s\n after  %s", before, after)
	}
}

// TestNativeIgnoresPluginDirsInTheCatalog is A14's first half carried into the
// prompt: --plugin-dir is cursor-agent's flag by another route, and a native
// session neither lists what it points at nor tells the model about it.
func TestNativeIgnoresPluginDirsInTheCatalog(t *testing.T) {
	f := newNativeFixture(t)
	dir := writeTree(t, filepath.Join(t.TempDir(), "extra"), map[string]string{
		"commands/extra.md": commandFile("from a --plugin-dir", "extra body"),
	})
	before := treeState(t, dir)
	s := startContent(t, f, Options{PluginDirs: []string{dir}},
		map[string]string{".claude/commands/own.md": commandFile("the project's own", "own body")}, nil)
	sent := systemText(t, s.oneRequest(t, "hi"))

	if !strings.Contains(s.diag.String(), "plugin dirs ignored for native") {
		t.Fatalf("diagnostics %q do not say the dirs were ignored", s.diag.String())
	}
	if strings.Contains(sent, "from a --plugin-dir") || strings.Contains(sent, dir) {
		t.Fatalf("the prompt carries the ignored directory:\n%s", sent)
	}
	if !strings.Contains(sent, "- own (command): the project's own") {
		t.Fatalf("the project's own row is missing:\n%s", sent)
	}
	if after := treeState(t, dir); after != before {
		t.Fatalf("the ignored directory was written to:\n before %s\n after  %s", before, after)
	}
}

// treeState is every file under root with its size and modification time, as
// one string: what a test compares before and after to say that nothing was
// created, removed or rewritten. A missing root is "".
func treeState(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %d %s\n", path, info.Size(), info.ModTime().UTC())
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walking %s: %v", root, err)
	}
	return b.String()
}
