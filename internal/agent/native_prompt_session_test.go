package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
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

// TestNativeCanaryNeverReachesThePrompt is A8 extended to everything C6 added.
// The key is planted in the four places the prompt now reads from — an
// instruction file, a command body, a catalog description, and the path a row
// is listed at — and must reach none of: the wire, the transcript, the
// journal (the prompt_sources note included), or the session's own events,
// which are what `craze prompt --json` prints.
func TestNativeCanaryNeverReachesThePrompt(t *testing.T) {
	f := newNativeFixture(t)
	dir := filepath.Join(t.TempDir(), "journal")
	s := startContent(t, f, Options{JournalDir: dir}, map[string]string{
		"CLAUDE.md":                   "The key is " + nativeCanary + ".\n",
		".claude/commands/leak.md":    commandFile("a command", "export KEY="+nativeCanary),
		".claude/skills/say/SKILL.md": skillDoc("say", "described as "+nativeCanary),
		// A skill whose directory — and therefore whose name and path — is
		// the key itself. Its row is dropped rather than redacted (§3.4).
		".claude/skills/" + nativeCanary + "/SKILL.md": skillDoc(nativeCanary, "named after the key"),
	}, nil)
	w := journalOf(t, s.log)
	inc := s.Incarnation()

	call := s.oneRequest(t, "/leak")
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
	// The row whose path was the key is gone from the catalog and from the
	// menu's spelling of it, with one line that does not hold the key either.
	if strings.Contains(sent, "named after the key") {
		t.Fatalf("a row whose path holds a key was listed:\n%s", sent)
	}
	if strings.Contains(s.diag.String(), nativeCanary) {
		t.Fatalf("the canary reached craze's diagnostics: %q", s.diag.String())
	}
	// The event stream, which is what `craze prompt --json` prints: the
	// expansion's own announcement, and the rows a menu would draw.
	for _, e := range ofType(drained(s), EventCommand) {
		if strings.Contains(e.Command.Text+e.Command.Path+e.Command.Description, nativeCanary) {
			t.Fatalf("the canary reached a command event: %+v", e.Command)
		}
	}
	// A menu row's description too. Its *name* is deliberately left alone
	// (X13): it is what the user types and what the lookup is keyed by, so a
	// redacted one would be a row that answers to nothing — which is why the
	// catalog drops such a row rather than listing it redacted.
	for _, r := range s.Snapshot().Plugins {
		if strings.Contains(r.Description, nativeCanary) {
			t.Fatalf("the canary reached a menu row's description: %+v", r)
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
