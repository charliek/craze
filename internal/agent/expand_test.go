package agent

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// entryOf is one discovered plugin entry, as short as a test needs it: the
// path and the root are derived from the plugin so an assertion can spell them
// without a fixture on disk.
func entryOf(plugin, name, kind, body string) PluginEntry {
	root := "/plugins/" + plugin
	path := root + "/commands/" + name + ".md"
	if kind == PluginKindSkill {
		path = root + "/skills/" + name + "/SKILL.md"
	}
	return PluginEntry{Plugin: plugin, Name: name, Kind: kind, Body: body, Root: root, Path: path}
}

// lookupOf resolves entries the way a session does once the agent's catalog
// has landed, then maps every spelling they can be typed by.
func lookupOf(entries []PluginEntry, taken ...string) map[string]pluginTarget {
	return buildPluginLookup(entries, ResolvePluginNames(entries, taken, false))
}

// blockOf is one entry expanded as the wire would carry it. pluginBlock reads
// the entry and the spelling only, so a test never has to invent the resolved
// row that goes with them.
func blockOf(e PluginEntry, typed, args string) string {
	return pluginBlock(pluginRef{target: pluginTarget{entry: e}, typed: typed, args: args})
}

// refNames is one scan's result as "typed=args" pairs, which is everything the
// token rules have to say.
func refNames(refs []pluginRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.typed+"="+r.args)
	}
	return out
}

// TestPluginRefTokenRules is cursor's own scan: where a name may start, where
// it ends, what counts as its arguments, and which spellings resolve.
func TestPluginRefTokenRules(t *testing.T) {
	entries := []PluginEntry{
		entryOf("git-commands", "watch-pr", PluginKindCommand, "body"),
		entryOf("forge", "gauntlet", PluginKindCommand, "body"),
		entryOf("flows", "gauntlet", PluginKindCommand, "body"),
	}
	// simplify is what the agent advertises, so the plugin's own is qualified.
	entries = append(entries, entryOf("forge", "simplify", PluginKindCommand, "body"))
	lookup := lookupOf(entries, "simplify")

	cases := []struct {
		name string
		text string
		want []string
	}{
		{"at offset zero", "/watch-pr 12", []string{"watch-pr=12"}},
		{"after a space", "please /watch-pr 12", []string{"watch-pr=12"}},
		{"after a newline", "please\n/watch-pr 12", []string{"watch-pr=12"}},
		{"after a non-breaking space", "please\u00a0/watch-pr 12", []string{"watch-pr=12"}},
		{"inside a word", "foo/watch-pr 12", nil},
		{"inside a url", "see https://x/watch-pr now", nil},
		{"punctuation ends the name", "/watch-pr, then stop", []string{"watch-pr="}},
		{"arguments run to the end of the line", "/watch-pr 12 and 13\nnext line", []string{"watch-pr=12 and 13"}},
		{"a tab starts the arguments", "/watch-pr\t12", []string{"watch-pr=12"}},
		{"a second line carries its own token", "hello\n/watch-pr 12", []string{"watch-pr=12"}},
		{"the qualified spelling always resolves", "/git-commands:watch-pr 12", []string{"git-commands:watch-pr=12"}},
		{"a colliding name resolves qualified", "/forge:gauntlet run", []string{"forge:gauntlet=run"}},
		{"a colliding name does not resolve bare", "/gauntlet run", nil},
		{"a name the agent advertises does not resolve bare", "/simplify this", nil},
		{"but its qualified spelling does", "/forge:simplify this", []string{"forge:simplify=this"}},
		{"an unknown name is left alone", "/nope banana", nil},
		{"case does not matter", "/Watch-PR 12", []string{"Watch-PR=12"}},
		{"the first occurrence wins", "/watch-pr 12\n/watch-pr 13", []string{"watch-pr=12"}},
		{"both spellings are still one entry", "/watch-pr 12\n/git-commands:watch-pr 13", []string{"watch-pr=12"}},
		{"two entries are two references", "/watch-pr 12\n/forge:gauntlet run", []string{"watch-pr=12", "forge:gauntlet=run"}},
		{"an unknown qualified name is left alone", "/git-commands:nope 12", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := refNames(pluginRefs(tc.text, lookup))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("refs %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPluginRefsStopAtTheBlockCap pins that the ninth reference of a draft is
// left alone: the cap is on what craze expands, not on what it sends.
func TestPluginRefsStopAtTheBlockCap(t *testing.T) {
	var entries []PluginEntry
	var draft []string
	for _, name := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9"} {
		entries = append(entries, entryOf("p", name, PluginKindCommand, "body"))
		draft = append(draft, "/"+name)
	}
	refs := pluginRefs(strings.Join(draft, " "), lookupOf(entries))
	if len(refs) != maxPluginBlocks {
		t.Fatalf("refs %d, want %d", len(refs), maxPluginBlocks)
	}
	if refs[len(refs)-1].typed != "a8" {
		t.Fatalf("last reference %q", refs[len(refs)-1].typed)
	}
}

// TestPluginRefsWithoutPlugins is the whole of what a grok session does with a
// draft: an empty lookup resolves nothing, whatever the draft says.
func TestPluginRefsWithoutPlugins(t *testing.T) {
	if refs := pluginRefs("/watch-pr 12", nil); refs != nil {
		t.Fatalf("refs %v with no plugins", refNames(refs))
	}
}

func TestExpandCommandBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		args string
		want string
	}{
		{"arguments", "watch $ARGUMENTS now", "12 13", "watch 12 13 now"},
		{"arguments collapse whitespace", "watch $ARGUMENTS now", "12   13", "watch 12 13 now"},
		{"arguments with none given", "watch $ARGUMENTS now", "", "watch  now"},
		{"positional", "pr $1 repo $2", "12 craze", "pr 12 repo craze"},
		{"positional beyond the arguments", "pr $1 repo $2", "12", "pr 12 repo "},
		{"two digits", "arg $10", "a", "arg "},
		{"three digits are text", "arg $123", "a", "arg $123\n\na"},
		{"a word after the digits is text", "arg $1x", "a", "arg $1x\n\na"},
		{"a word before the dollar is text", "arg a$1", "a", "arg a$1\n\na"},
		{"no placeholder appends the arguments", "do the thing", "12 13", "do the thing\n\n12 13"},
		{"no placeholder and no arguments", "do the thing", "", "do the thing"},
		{"a spent placeholder is not appended", "pr $1", "", "pr "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandCommandBody(tc.body, "/plugins/p", tc.args); got != tc.want {
				t.Fatalf("body %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExpandBodyPluginRoot pins both spellings of the plugin root, for both
// kinds. Cursor substitutes both over plugin content; craze is at parity.
func TestExpandBodyPluginRoot(t *testing.T) {
	body := "claude ${CLAUDE_PLUGIN_ROOT}/bin cursor ${CURSOR_PLUGIN_ROOT}/bin"
	want := "claude /plugins/p/bin cursor /plugins/p/bin"
	if got := expandCommandBody(body, "/plugins/p", ""); got != want {
		t.Fatalf("command %q, want %q", got, want)
	}
	if got := expandSkillBody(body, "/plugins/p"); got != want {
		t.Fatalf("skill %q, want %q", got, want)
	}
}

// TestExpandSkillBodyIsUnsubstituted is cursor's skill rule: the file goes as
// it stands and the arguments are in the invocation only.
func TestExpandSkillBodyIsUnsubstituted(t *testing.T) {
	body := "---\nname: probe-skill\n---\nrun $ARGUMENTS and $1"
	if got := expandSkillBody(body, "/plugins/p"); got != body {
		t.Fatalf("skill body %q, want it untouched", got)
	}
	entry := entryOf("probe-plugin", "probe-skill", PluginKindSkill, body)
	block := blockOf(entry, "probe-skill", "banana")
	if !strings.Contains(block, `args="banana"`) {
		t.Fatalf("skill block lost its arguments:\n%s", block)
	}
	if !strings.Contains(block, "run $ARGUMENTS and $1") {
		t.Fatalf("skill block substituted its body:\n%s", block)
	}
}

// TestCapPluginBodyWithoutALineBreak: one enormous line has no boundary to cut
// on, and the marker still has to fit under the ceiling rather than on top of
// it. This is the shape that used to come out over the cap.
func TestCapPluginBodyWithoutALineBreak(t *testing.T) {
	got := capPluginBody(strings.Repeat("x", maxPluginBodyBytes+1))
	if len(got) > maxPluginBodyBytes {
		t.Fatalf("capped body is %d bytes, over the %d ceiling", len(got), maxPluginBodyBytes)
	}
	if !strings.HasSuffix(got, "\n"+pluginTruncated) {
		t.Fatal("the marker is missing")
	}
}

// TestPluginBlockStaysUnderTheCeiling: escaping can only grow a body, so the
// cap has to be the last thing applied to it.
func TestPluginBlockStaysUnderTheCeiling(t *testing.T) {
	body := strings.Repeat("</command>", maxPluginBodyBytes/10+16)
	entry := entryOf("p", "big", PluginKindCommand, body)
	block := blockOf(entry, "big", "")
	if strings.Contains(block[:len(block)-len("</command>")], "</command>") {
		t.Fatal("an unescaped closing tag survived into the block")
	}
	if len(block) > maxPluginBodyBytes+2048 {
		t.Fatalf("block is %d bytes: the body was not capped after escaping", len(block))
	}
}

func TestCapPluginBody(t *testing.T) {
	line := strings.Repeat("x", 63) + "\n"
	body := strings.Repeat(line, (maxPluginBodyBytes/len(line))+40)
	got := capPluginBody(body)
	if len(got) > maxPluginBodyBytes {
		t.Fatalf("capped body is %d bytes, over the %d ceiling", len(got), maxPluginBodyBytes)
	}
	if !strings.HasSuffix(got, "\n"+pluginTruncated) {
		t.Fatalf("capped body ends %q", got[len(got)-80:])
	}
	if strings.Contains(strings.TrimSuffix(got, "\n"+pluginTruncated), "\n"+pluginTruncated) {
		t.Fatal("the marker is in the body")
	}
	// The cut lands on a line boundary, so the last kept line is whole.
	kept := strings.Split(strings.TrimSuffix(got, "\n"+pluginTruncated), "\n")
	if last := kept[len(kept)-1]; last != strings.Repeat("x", 63) {
		t.Fatalf("last kept line %q", last)
	}
	if short := "under the cap"; capPluginBody(short) != short {
		t.Fatal("a body under the cap was rewritten")
	}
}

// TestCapPluginBodyWithoutNewlines is the one-long-line case: there is no line
// boundary to cut on, so the cut has to fall on a rune boundary instead.
func TestCapPluginBodyWithoutNewlines(t *testing.T) {
	// Three bytes a rune, so the cap does not fall on a boundary of its own.
	got := capPluginBody(strings.Repeat("€", maxPluginBodyBytes))
	body := strings.TrimSuffix(got, "\n"+pluginTruncated)
	if body == got {
		t.Fatal("no truncation marker")
	}
	if strings.ContainsRune(body, '�') || len(body)%3 != 0 {
		t.Fatalf("cut split a rune: %d bytes", len(body))
	}
}

func TestAttributeEscaping(t *testing.T) {
	entry := entryOf("git-commands", "watch-pr", PluginKindCommand, "body")
	entry.Path = `/plugins/"odd" <dir>/watch-pr.md`
	block := blockOf(entry, "watch-pr", `12 "> ouch <command> & more`)
	tag := blockTag(t, block)
	want := `<command name="watch-pr" plugin="git-commands" args="12 &quot;&gt; ouch &lt;command&gt; &amp; more">`
	if tag != want {
		t.Fatalf("tag\n got %s\nwant %s", tag, want)
	}
	// A newline in an argument would put the rest of the tag on its own line.
	nl := blockOf(entry, "watch-pr", "12\nrogue")
	if got := blockTag(t, nl); got != `<command name="watch-pr" plugin="git-commands" args="12 rogue">` {
		t.Fatalf("tag %s", got)
	}
}

// blockTag is a block's opening tag, which is always its second line.
func blockTag(t *testing.T, block string) string {
	t.Helper()
	lines := strings.SplitN(block, "\n", 3)
	if len(lines) < 2 {
		t.Fatalf("block has no tag:\n%s", block)
	}
	return lines[1]
}

// TestClosingTagInsideABodyIsEscaped keeps a body that talks about the block
// format from ending the block it is inside.
func TestClosingTagInsideABodyIsEscaped(t *testing.T) {
	entry := entryOf("p", "meta", PluginKindCommand, "write </command> and </COMMAND> and </skill>")
	block := blockOf(entry, "meta", "")
	if !strings.Contains(block, `write <\/command> and <\/COMMAND> and </skill>`) {
		t.Fatalf("body not escaped:\n%s", block)
	}
	if strings.Count(block, "</command>") != 1 {
		t.Fatalf("the block has more than one closing tag:\n%s", block)
	}
	// A rune whose lowercase is a different width (U+0130 folds to two runes)
	// would slide every offset after it if the match were made against a
	// folded copy of the body rather than against the body itself.
	wide := entryOf("p", "meta", PluginKindCommand, "İ then </command> now")
	if got := blockOf(wide, "meta", ""); !strings.Contains(got, "İ then <\\/command> now") {
		t.Fatalf("a wide-folding rune skewed the escape:\n%s", got)
	}
	skill := entryOf("p", "meta", PluginKindSkill, "write </skill> now")
	sblock := blockOf(skill, "meta", "")
	if !strings.Contains(sblock, `write <\/skill> now`) || strings.Count(sblock, "</skill>") != 1 {
		t.Fatalf("skill body not escaped:\n%s", sblock)
	}
	// XML lets an end tag carry whitespace before its ">", so "</command >"
	// closes the element every bit as well as the tight spelling.
	for _, spelling := range []string{"</command >", "</command\t>", "</command\n>", "</Command  \t >"} {
		loose := entryOf("p", "meta", PluginKindCommand, "before "+spelling+" after")
		got := blockOf(loose, "meta", "")
		if strings.Count(got, "</command>") != 1 || !strings.Contains(got, `before <\/`) {
			t.Fatalf("%q was not escaped:\n%s", spelling, got)
		}
	}
	// A "<" that only looks like the start of one is left alone.
	plain := entryOf("p", "meta", PluginKindCommand, "a </commander> and </command")
	if got := blockOf(plain, "meta", ""); !strings.Contains(got, "a </commander> and </command\n") {
		t.Fatalf("a non-tag was rewritten:\n%s", got)
	}
}

// TestSubstitutionOutputIsOpaque: what a placeholder produced is text, not
// something the next placeholder may read. An argument spelled "$2" is the
// user's own text and must survive as it was typed.
func TestSubstitutionOutputIsOpaque(t *testing.T) {
	if got := expandCommandBody("run $ARGUMENTS", "/root", "$2 final"); got != "run $2 final" {
		t.Fatalf("expanded %q, want the arguments verbatim", got)
	}
	if got := expandCommandBody("first=$1 rest=$ARGUMENTS", "/root", "$ARGUMENTS b"); got != "first=$ARGUMENTS rest=$ARGUMENTS b" {
		t.Fatalf("expanded %q", got)
	}
	// A plugin whose install path happens to spell a placeholder is text too.
	if got := expandCommandBody("at ${CLAUDE_PLUGIN_ROOT}", "/p/$1", "arg"); got != "at /p/$1\n\narg" {
		t.Fatalf("expanded %q, want the root left alone", got)
	}
}

// TestBlockFormat is the golden: one sentence a model can read without a
// system prompt, then the tag, the expanded body and the closing tag.
func TestBlockFormat(t *testing.T) {
	entry := PluginEntry{
		Plugin: "git-commands",
		Name:   "watch-pr",
		Kind:   PluginKindCommand,
		Body:   "Watch PR $ARGUMENTS until it is green.",
		Root:   "/home/u/.cursor/plugins/cache/cc/git-commands/1",
		Path:   "/home/u/.cursor/plugins/cache/cc/git-commands/1/commands/watch-pr.md",
	}
	lookup := lookupOf([]PluginEntry{entry})
	refs := pluginRefs("/watch-pr 12", lookup)
	if len(refs) != 1 {
		t.Fatalf("refs %v", refNames(refs))
	}
	want := `The user invoked /watch-pr 12 (the "watch-pr" command from the "git-commands" plugin, ` +
		"/home/u/.cursor/plugins/cache/cc/git-commands/1/commands/watch-pr.md). Follow its instructions:\n" +
		`<command name="watch-pr" plugin="git-commands" args="12">` + "\n" +
		"Watch PR 12 until it is green.\n" +
		"</command>"
	if got := pluginBlock(refs[0]); got != want {
		t.Fatalf("block\n got %q\nwant %q", got, want)
	}
}

// TestBlockFormatSkill is the same for a skill: the word changes, the tag
// changes, the body does not.
func TestBlockFormatSkill(t *testing.T) {
	entry := PluginEntry{
		Plugin: "probe-plugin",
		Name:   "probe-skill",
		Kind:   PluginKindSkill,
		Body:   "---\nname: probe-skill\n---\nSay PROBE-SKILL-EXPANDED.\n",
		Root:   "/plugins/probe-plugin",
		Path:   "/plugins/probe-plugin/skills/probe-skill/SKILL.md",
	}
	refs := pluginRefs("/probe-skill", lookupOf([]PluginEntry{entry}))
	if len(refs) != 1 {
		t.Fatalf("refs %v", refNames(refs))
	}
	want := `The user invoked /probe-skill (the "probe-skill" skill from the "probe-plugin" plugin, ` +
		"/plugins/probe-plugin/skills/probe-skill/SKILL.md). Follow its instructions:\n" +
		`<skill name="probe-skill" plugin="probe-plugin" args="">` + "\n" +
		"---\nname: probe-skill\n---\nSay PROBE-SKILL-EXPANDED.\n" +
		"</skill>"
	if got := pluginBlock(refs[0]); got != want {
		t.Fatalf("block\n got %q\nwant %q", got, want)
	}
}

// TestPromptBlocksKeepTheDraftFirst pins block 1: the agent sees exactly what
// the user typed, and craze's own blocks follow it in reference order.
func TestPromptBlocksKeepTheDraftFirst(t *testing.T) {
	entries := []PluginEntry{
		entryOf("p", "one", PluginKindCommand, "first body"),
		entryOf("p", "two", PluginKindSkill, "second body"),
	}
	draft := "/one a\n/two b"
	blocks, cmds := promptBlocks(draft, pluginRefs(draft, lookupOf(entries)))
	if len(blocks) != 3 || len(cmds) != 2 {
		t.Fatalf("%d blocks, %d commands", len(blocks), len(cmds))
	}
	if blocks[0].Type != "text" || blocks[0].Text != draft {
		t.Fatalf("block 1 %+v", blocks[0])
	}
	if !strings.Contains(blocks[1].Text, "first body") || !strings.Contains(blocks[2].Text, "second body") {
		t.Fatalf("blocks out of order: %q %q", blocks[1].Text, blocks[2].Text)
	}
	if cmds[0].Text != blocks[1].Text || cmds[1].Text != blocks[2].Text {
		t.Fatal("the event text is not the block that was sent")
	}
	if cmds[0].Kind != PluginKindCommand || cmds[1].Kind != PluginKindSkill {
		t.Fatalf("kinds %q %q", cmds[0].Kind, cmds[1].Kind)
	}
	if cmds[0].Bare != "one" || cmds[0].Qualified != "p:one" || cmds[0].Display != "one" {
		t.Fatalf("names %+v", cmds[0])
	}
	if cmds[1].Path != entries[1].Path {
		t.Fatalf("path %q", cmds[1].Path)
	}
	// A draft with nothing to expand is still exactly one block.
	plain, none := promptBlocks("hello", nil)
	if len(plain) != 1 || plain[0].Text != "hello" || none != nil {
		t.Fatalf("plain draft %+v %+v", plain, none)
	}
}

// probeFixtureDir is the fixture the wire tests send: one command that spends
// its arguments and one skill that does not.
func probeFixtureDir(t *testing.T) string {
	t.Helper()
	return writeTree(t, filepath.Join(t.TempDir(), "probe-plugin"), map[string]string{
		"commands/probe-echo.md": commandFile("probe command",
			"Reply with exactly this line:\nPROBE-COMMAND-EXPANDED args=[$ARGUMENTS]"),
		"skills/probe-skill/SKILL.md": "---\nname: probe-skill\ndescription: probe skill\n---\n" +
			"Reply with exactly this line:\nPROBE-SKILL-EXPANDED\n",
	})
}

// commandEvents is every expansion a run reported, in the order it reported
// them.
func commandEvents(evs []Event) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.Type == EventCommand {
			out = append(out, ev)
		}
	}
	return out
}

// TestPromptExpandsAPluginCommand is the whole path with the fake in the
// middle: the draft and one block go out together, the echo comes back with
// the block boundary in it, and the event that says so arrives before any
// agent text.
func TestPromptExpandsAPluginCommand(t *testing.T) {
	dir := probeFixtureDir(t)
	s := startScriptOpts(t, "echo", Options{PluginDirs: []string{dir}})
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "/probe-plugin:probe-echo banana"); err != nil {
		t.Fatal(err)
	}
	want := "echo: /probe-plugin:probe-echo banana\n" +
		`The user invoked /probe-plugin:probe-echo banana (the "probe-echo" command from the "probe-plugin" plugin, ` +
		filepath.Join(dir, "commands", "probe-echo.md") + "). Follow its instructions:\n" +
		`<command name="probe-echo" plugin="probe-plugin" args="banana">` + "\n" +
		"Reply with exactly this line:\nPROBE-COMMAND-EXPANDED args=[banana]\n" +
		"</command>"
	log.waitTexts(t, want)

	evs := log.snapshot()
	cmds := commandEvents(evs)
	if len(cmds) != 1 {
		t.Fatalf("%d command events", len(cmds))
	}
	got := cmds[0].Command
	if got == nil || got.Bare != "probe-echo" || got.Qualified != "probe-plugin:probe-echo" {
		t.Fatalf("command %+v", got)
	}
	if got.Kind != PluginKindCommand || got.Plugin != "probe-plugin" {
		t.Fatalf("command %+v", got)
	}
	if got.Path != filepath.Join(dir, "commands", "probe-echo.md") {
		t.Fatalf("path %q", got.Path)
	}
	if !strings.Contains(got.Text, "PROBE-COMMAND-EXPANDED args=[banana]") {
		t.Fatalf("text %q", got.Text)
	}
	// The hook runs before the request is written, so nothing the turn
	// produced can come first.
	for _, ev := range evs {
		if ev.Type == EventCommand {
			break
		}
		if ev.Type == EventText || ev.Type == EventThought || ev.Type == EventTool {
			t.Fatalf("an agent event preceded the command event: %+v", ev)
		}
	}
}

// TestPromptExpandsAPluginSkill: the skill's file goes as it stands, and the
// arguments live in the invocation line only.
func TestPromptExpandsAPluginSkill(t *testing.T) {
	dir := probeFixtureDir(t)
	s := startScriptOpts(t, "echo", Options{PluginDirs: []string{dir}})
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "/probe-plugin:probe-skill kiwi"); err != nil {
		t.Fatal(err)
	}
	ev := log.waitType(t, EventCommand)
	if ev.Command == nil || ev.Command.Kind != PluginKindSkill {
		t.Fatalf("command %+v", ev.Command)
	}
	if !strings.Contains(ev.Command.Text, `args="kiwi"`) {
		t.Fatalf("the skill block lost its arguments:\n%s", ev.Command.Text)
	}
	if !strings.Contains(ev.Command.Text, "---\nname: probe-skill\ndescription: probe skill\n---\n") {
		t.Fatalf("the skill block is not the whole file:\n%s", ev.Command.Text)
	}
}

// TestAdvertisedBareNameIsSentVerbatim: once the agent advertises a name, the
// bare spelling is the agent's and goes out untouched — only the qualified one
// is still craze's to expand.
func TestAdvertisedBareNameIsSentVerbatim(t *testing.T) {
	dir := writeTree(t, filepath.Join(t.TempDir(), "probe-plugin"), map[string]string{
		"commands/gauntlet-like.md": commandFile("collides", "PLUGIN BODY"),
	})
	s := startScriptOpts(t, "commands", Options{PluginDirs: []string{dir}})
	waitUntil(t, "the agent's catalog", func() bool {
		c, ok := displayOf(s.Snapshot(), "gauntlet-like")
		return ok && c.Display == "probe-plugin:gauntlet-like" && s.Snapshot().Commands != nil
	})
	log := collect(t, s)

	if _, err := s.Prompt(t.Context(), "/gauntlet-like now"); err != nil {
		t.Fatal(err)
	}
	log.waitTexts(t, "echo: /gauntlet-like now")
	if cmds := commandEvents(log.snapshot()); len(cmds) != 0 {
		t.Fatalf("an advertised name was expanded: %+v", cmds[0].Command)
	}

	if _, err := s.Prompt(t.Context(), "/probe-plugin:gauntlet-like now"); err != nil {
		t.Fatal(err)
	}
	ev := log.waitType(t, EventCommand)
	if ev.Command == nil || !strings.Contains(ev.Command.Text, "PLUGIN BODY") {
		t.Fatalf("command %+v", ev.Command)
	}
}

// TestRefusedPromptEmitsNoCommand: a prompt the client refuses never reached
// the wire, so nothing was expanded and nothing may be reported. The hook the
// events come from runs one step after the refusal could happen, which is what
// makes this hold for the CLI's foreign-turn retry too.
//
// The trailing Cancel shares the exact hazard TestCancelWaitsUntilPromptReturns
// and TestSerializedPrompt (session_test.go) were fixed for: promptInFlight
// flips true before session/prompt is written, so waiting on it alone would
// let Cancel's session/cancel beat session/prompt onto the wire, get discarded
// by the fake on purpose, and leave "hang" (and, with it, this test) waiting
// forever for a second cancel that never comes. "hang-ack" and waiting for its
// chunk prove the fake read the prompt before Cancel is allowed to run; the
// bounded context is the same fail-fast backstop for if that regresses.
func TestRefusedPromptEmitsNoCommand(t *testing.T) {
	dir := probeFixtureDir(t)
	s := startScriptOpts(t, "hang-ack", Options{PluginDirs: []string{dir}})
	log := collect(t, s)
	go func() { _, _ = s.Prompt(context.Background(), "hold the turn") }()
	log.waitTexts(t, "ack: hold the turn")

	if _, err := s.Prompt(context.Background(), "/probe-plugin:probe-echo banana"); !errors.Is(err, ErrPromptInFlight) {
		t.Fatalf("second prompt: %v", err)
	}
	if cmds := commandEvents(log.snapshot()); len(cmds) != 0 {
		t.Fatalf("a refused prompt reported %+v", cmds[0].Command)
	}
	cancelCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := s.Cancel(cancelCtx); err != nil {
		t.Fatal(err)
	}
}

// TestForeignTurnRefusalEmitsNoCommand is the same rule for the other refusal.
// Only cursor sessions have plugins and only grok runs turns of its own, so the
// two are put in one session by hand: the lookup a cursor session would have,
// on the wire that can refuse this way.
func TestForeignTurnRefusalEmitsNoCommand(t *testing.T) {
	s := startGrokScript(t, "grok-long-turn-fallback", true)
	entries := DiscoverPlugins(PluginScan{Dirs: true}, t.TempDir(), "", []string{probeFixtureDir(t)})
	s.mu.Lock()
	s.plugins = entries
	s.snap.Plugins = s.resolvePluginsLocked()
	s.mu.Unlock()
	if len(s.Snapshot().Plugins) == 0 {
		t.Fatal("fixture found nothing")
	}
	log := collect(t, s)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Prompt(context.Background(), "do the steps"); err != nil {
			t.Errorf("prompt: %v", err)
		}
	}()
	waitUntil(t, "the turn to start", s.promptInFlight)
	if err := s.Interject(t.Context(), "BANANA"); err != nil {
		t.Fatalf("interject: %v", err)
	}
	<-done
	// The start event rather than the snapshot flag, for the reason spelled
	// out in TestPopQueueRefusesDuringAForeignTurn: the flag flips first, so
	// waiting on it would let the check below read a log the collector has not
	// caught up with.
	waitUntil(t, "the foreign turn's start event", func() bool {
		started, _ := foreignTurnCounts(log.snapshot())
		return started > 0
	})

	if _, err := s.Prompt(context.Background(), "/probe-plugin:probe-echo banana"); !errors.Is(err, ErrForeignTurn) {
		t.Fatalf("prompt during a foreign turn: %v", err)
	}
	if cmds := commandEvents(log.snapshot()); len(cmds) != 0 {
		t.Fatalf("a refused prompt reported %+v", cmds[0].Command)
	}
	waitUntil(t, "the foreign turn to end", func() bool { return !s.Snapshot().ForeignTurn })
}

// TestGrokPromptUnchanged: grok advertises every plugin skill itself and
// expands them server-side, so craze scans nothing for it and its wire is
// byte-for-byte what it was — one block, the draft, no event.
func TestGrokPromptUnchanged(t *testing.T) {
	dir := probeFixtureDir(t)
	grok := GrokProvider()
	s := newTestSession(t, Options{
		Binary:     fakeAgentPath(t),
		ExtraArgs:  []string{"-script=grok-echo"},
		Workspace:  t.TempDir(),
		Force:      true,
		Provider:   &grok,
		PluginDirs: []string{dir},
		Stderr:     io.Discard,
	})
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	log := collect(t, s)
	if got := s.Snapshot().Plugins; len(got) != 0 {
		t.Fatalf("grok discovered %+v", got)
	}
	if _, err := s.Prompt(t.Context(), "/probe-plugin:probe-echo banana"); err != nil {
		t.Fatal(err)
	}
	// The fake joins text blocks with a newline, so a second block could not
	// hide in this.
	log.waitTexts(t, "echo: /probe-plugin:probe-echo banana")
	if cmds := commandEvents(log.snapshot()); len(cmds) != 0 {
		t.Fatalf("grok reported %+v", cmds[0].Command)
	}
}

// TestHasUnresolvedSlash is the question the catalog wait asks, on its own.
// The lookup is the resolved one, so a name that is only reachable qualified
// counts as unresolved bare — which is exactly the state a provisional
// session is in for every row it has.
func TestHasUnresolvedSlash(t *testing.T) {
	entries := []PluginEntry{
		entryOf("git-commands", "watch-pr", PluginKindCommand, "body"),
		entryOf("forge", "simplify", PluginKindCommand, "body"),
	}
	lookup := lookupOf(entries, "simplify")

	cases := []struct {
		name string
		text string
		want bool
	}{
		{"a name that resolves", "/watch-pr 12", false},
		{"a qualified name that resolves", "/git-commands:watch-pr 12", false},
		{"a name the agent took over", "/simplify this", true},
		{"a name nobody claims", "/nope banana", true},
		{"no slash at all", "watch-pr now", false},
		{"a slash inside a word", "see https://x/nope now", false},
		{"an empty draft", "", false},
		{"one resolved name and one that is not", "/watch-pr 12\n/nope", true},
		{"the resolved one does not hide the other", "/nope\n/watch-pr 12", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasUnresolvedSlash(tc.text, lookup); got != tc.want {
				t.Fatalf("hasUnresolvedSlash(%q) = %v", tc.text, got)
			}
		})
	}
	// A session with no plugins resolves nothing, which is not the same
	// question: the wait asks this one only once it knows there are plugins.
	if !hasUnresolvedSlash("/watch-pr 12", nil) {
		t.Fatal("an empty lookup answers nothing")
	}
}

// shortCatalogWait cuts the §3.3 window down to something a test can afford to
// sit out, and puts it back afterwards. Nothing in this package runs in
// parallel, so the one variable is safe to swap.
func shortCatalogWait(t *testing.T, d time.Duration) time.Duration {
	t.Helper()
	prev := catalogWait
	catalogWait = d
	t.Cleanup(func() { catalogWait = prev })
	return d
}

// longCatalogWait is the window a test sets when the point is that something
// other than the timeout ended the wait, and catalogWaitMargin is what a wait
// that must not happen at all is allowed to take anyway. They are five times
// apart, so neither bound is a close call on a loaded box under -race.
const (
	longCatalogWait   = 10 * time.Second
	catalogWaitMargin = 2 * time.Second
)

// waitingSession is the one state §3.3 asks about, built by hand: plugins
// discovered, no catalog applied, no agent behind it. The wait is decided
// entirely from the session's own fields, so timing it needs no wire — and
// timing it through a Prompt would time the fake's round trip instead.
func waitingSession(t *testing.T) *session {
	t.Helper()
	s := newTestSession(t, Options{PluginDirs: []string{probeFixtureDir(t)}, Stderr: io.Discard})
	s.plugins = s.discoverPlugins(t.TempDir())
	if len(s.plugins) != 2 {
		t.Fatalf("the fixture discovered %+v", s.plugins)
	}
	s.mu.Lock()
	s.snap.Plugins = s.resolvePluginsLocked()
	s.mu.Unlock()
	return s
}

// awaitOnce runs the wait exactly as Prompt runs it — awaitCatalog, then the
// locked hand-back that says whether Cancel got there first — and reports how
// long the wait itself took, whether one was registered at all, whether it was
// aborted, and what the wait refused the caller with. Every caller here is the
// only prompt of its session, so the error is the timing tests' proof that the
// slot reservation did not fire on a session that has no other prompt in it.
func awaitOnce(ctx context.Context, s *session, text string) (elapsed time.Duration, waited, aborted bool, err error) {
	start := time.Now()
	abort, err := s.awaitCatalog(ctx, text)
	elapsed = time.Since(start)
	s.mu.Lock()
	aborted = s.clearCatalogWaitLocked(abort)
	s.mu.Unlock()
	return elapsed, abort != nil, aborted, err
}

// awaitOutcome is one run of awaitOnce, carried back from the goroutine that
// parked in the wait.
type awaitOutcome struct {
	elapsed time.Duration
	waited  bool
	aborted bool
	err     error
}

// awaitInBackground parks a wait on its own goroutine, so the test goroutine
// is free to be the thing that ends it — and stays the only one allowed to
// call t.
func awaitInBackground(s *session, text string) <-chan awaitOutcome {
	out := make(chan awaitOutcome, 1)
	go func() {
		elapsed, waited, aborted, err := awaitOnce(context.Background(), s, text)
		out <- awaitOutcome{elapsed, waited, aborted, err}
	}()
	return out
}

// waitForCatalogWait blocks until a prompt is provably parked in the wait, so
// that what the test does next — apply a catalog, cancel — lands inside the
// window rather than racing the goroutine into it.
func waitForCatalogWait(t *testing.T, s *session) {
	t.Helper()
	waitFor(t, "a prompt to park in the catalog wait", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.catalogAbort != nil
	})
}

// promptOn runs one Prompt on its own goroutine, so the test can drive the
// session while that prompt is parked in the wait. The context is Background
// because both real callers pass that: Esc and a headless signal both arrive
// through Session.Cancel, never through the prompt's context.
func promptOn(s *session, text string) <-chan error {
	out := make(chan error, 1)
	go func() {
		_, err := s.Prompt(context.Background(), text)
		out <- err
	}()
	return out
}

// promptReturn is what that prompt returned. The deadline is a deadlock guard,
// not a measurement: it is far shorter than the window these tests set, so a
// wait that ended on the timeout instead of on what the test did fails here
// rather than passing slowly.
func promptReturn(t *testing.T, out <-chan error, why string) error {
	t.Helper()
	select {
	case err := <-out:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("the prompt never returned: %s", why)
		return nil
	}
}

// twoBlockEcho is what the fake echoes back for a resolved bare /probe-echo:
// the draft, then the expansion, with the fake's newline between the blocks.
func twoBlockEcho(dir string) string {
	return "echo: /probe-echo banana\n" +
		`The user invoked /probe-echo banana (the "probe-echo" command from the "probe-plugin" plugin, ` +
		filepath.Join(dir, "commands", "probe-echo.md") + "). Follow its instructions:\n" +
		`<command name="probe-echo" plugin="probe-plugin" args="banana">` + "\n" +
		"Reply with exactly this line:\nPROBE-COMMAND-EXPANDED args=[banana]\n" +
		"</command>"
}

// TestCatalogWaitTiming is the clock, and the only place there is one: what
// waits, what does not, and what ends a wait early. It runs against the wait
// itself rather than a prompt, because a prompt measures a round trip to the
// fake as well and -race makes that no kind of clock at all.
func TestCatalogWaitTiming(t *testing.T) {
	t.Run("an unresolved name sits out the window", func(t *testing.T) {
		window := shortCatalogWait(t, 200*time.Millisecond)
		s := waitingSession(t)
		elapsed, waited, aborted, err := awaitOnce(t.Context(), s, "/probe-echo banana")
		if err != nil || !waited || aborted {
			t.Fatalf("waited=%v aborted=%v err=%v", waited, aborted, err)
		}
		// A lower bound: the window is the whole of what is being claimed, and
		// nothing can make it shorter.
		if elapsed < window {
			t.Fatalf("the wait ended after %s, short of the %s window", elapsed, window)
		}
	})

	// Every case below sets a window it must not reach, so an elapsed under
	// the margin means the gate refused to wait rather than merely waited
	// quickly.
	skips := []struct {
		name  string
		text  string
		setup func(*session)
	}{
		{"a qualified spelling already resolves", "/probe-plugin:probe-echo banana", nil},
		{"a draft with no slash token", "hello", nil},
		{"a session with no plugins", "/probe-echo banana", func(s *session) { s.plugins = nil }},
		{"the catalog has already been applied", "/probe-echo banana", func(s *session) {
			s.onUpdate(commandsUpdate("research"))
		}},
	}
	for _, tc := range skips {
		t.Run(tc.name, func(t *testing.T) {
			window := shortCatalogWait(t, longCatalogWait)
			s := waitingSession(t)
			if tc.setup != nil {
				tc.setup(s)
			}
			elapsed, waited, _, err := awaitOnce(t.Context(), s, tc.text)
			if err != nil {
				t.Fatal(err)
			}
			if waited {
				t.Fatal("a wait was registered for something waiting cannot change")
			}
			if elapsed >= catalogWaitMargin {
				t.Fatalf("took %s of the %s window", elapsed, window)
			}
		})
	}

	t.Run("the catalog ends the wait", func(t *testing.T) {
		shortCatalogWait(t, longCatalogWait)
		s := waitingSession(t)
		res := awaitInBackground(s, "/probe-echo banana")
		waitForCatalogWait(t, s)
		s.onUpdate(commandsUpdate("research"))
		got := <-res
		if got.err != nil || !got.waited || got.aborted {
			t.Fatalf("waited=%v aborted=%v err=%v", got.waited, got.aborted, got.err)
		}
		if got.elapsed >= catalogWaitMargin {
			t.Fatalf("the update took %s to end the wait", got.elapsed)
		}
	})

	t.Run("a cancel ends the wait", func(t *testing.T) {
		shortCatalogWait(t, longCatalogWait)
		s := waitingSession(t)
		res := awaitInBackground(s, "/probe-echo banana")
		waitForCatalogWait(t, s)
		if !s.abortCatalogWait() {
			t.Fatal("the abort found no wait to end")
		}
		// A second one has nothing left to close, which is what lets Cancel
		// answer "this Esc was the wait's" from the return value alone.
		if s.abortCatalogWait() {
			t.Fatal("a second abort claimed the same wait")
		}
		got := <-res
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !got.aborted {
			t.Fatal("the wait ended without reporting the cancel")
		}
		if got.elapsed >= catalogWaitMargin {
			t.Fatalf("the cancel took %s to end the wait", got.elapsed)
		}
	})

	t.Run("a cancelled context ends the wait", func(t *testing.T) {
		shortCatalogWait(t, longCatalogWait)
		s := waitingSession(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// Not a cancel: the caller went away, which is not the user saying the
		// prompt must not be sent, and the send below fails on its own.
		elapsed, waited, aborted, err := awaitOnce(ctx, s, "/probe-echo banana")
		if err != nil || !waited || aborted {
			t.Fatalf("waited=%v aborted=%v err=%v", waited, aborted, err)
		}
		if elapsed >= catalogWaitMargin {
			t.Fatalf("a dead context took %s to end the wait", elapsed)
		}
	})
}

// TestCancelDuringTheCatalogWait is Esc while the first prompt is still parked
// in the wait, through the very Session.Cancel the TUI's Esc calls. The turn
// the prompt would have opened does not exist yet, so the cancel has nothing
// but the wait to find — and what it must leave behind is a prompt that never
// opened a turn and never reached the agent.
func TestCancelDuringTheCatalogWait(t *testing.T) {
	shortCatalogWait(t, longCatalogWait)
	s := startScriptOpts(t, "nocommands", Options{PluginDirs: []string{probeFixtureDir(t)}})
	log := collect(t, s)
	s.mu.Lock()
	turnBefore := s.turn
	s.mu.Unlock()

	out := promptOn(s, "/probe-echo banana")
	waitForCatalogWait(t, s)
	if err := s.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := promptReturn(t, out, "the cancel never reached the wait"); !errors.Is(err, ErrPromptCancelled) {
		t.Fatalf("the cancelled prompt returned %v", err)
	}
	// No turn: not one still running, and not one whose number was spent.
	// Snapshot carries no in-flight flag, so the question is asked where the
	// answer is kept.
	s.mu.Lock()
	in, turn := s.inPrompt, s.turn
	s.mu.Unlock()
	if in || turn != turnBefore {
		t.Fatalf("inPrompt=%v turn=%d (was %d) after a cancelled wait", in, turn, turnBefore)
	}
	if cmds := commandEvents(log.snapshot()); len(cmds) != 0 {
		t.Fatalf("a cancelled prompt reported %+v", cmds[0].Command)
	}
	// The fake echoes every prompt it is handed, in order, so what it echoes
	// next is the whole of what ever reached it: the cancelled draft did not.
	if _, err := s.Prompt(t.Context(), "still here"); err != nil {
		t.Fatal(err)
	}
	log.waitTexts(t, "echo: still here")
}

// TestCancelDuringTheWaitLeavesTheNextPromptAlone is the race the test above
// cannot reach, because waiting for Cancel to return before sending anything
// else serializes it away. Live, nothing does: the aborted prompt returns
// ErrPromptCancelled, the TUI settles the turn and drains its queue, and the
// next prompt goes out while Cancel is still walking the rest of its path. That
// tail is written for a turn on the wire — mark it cancelled, answer its
// blocked requests, send session/cancel, wait on its promptDone — so every step
// of it would land on the prompt that just started. Esc for the one the user
// stopped would stop the one they did not.
//
// What makes the failure deterministic is not the timing but the wire: an Esc
// that ran the tail always sends a session/cancel, whenever it lands, and an
// Esc that returns at the abort never sends one at all. The `callorder` script
// reports what it read, so the last turn's receipt names every notification
// craze wrote before it — and a cancel anywhere in that record fails this.
func TestCancelDuringTheWaitLeavesTheNextPromptAlone(t *testing.T) {
	shortCatalogWait(t, longCatalogWait)
	s := startScriptOpts(t, "callorder", Options{PluginDirs: []string{probeFixtureDir(t)}})
	log := collect(t, s)

	// A parks in the wait.
	outA := promptOn(s, "/probe-echo banana")
	waitForCatalogWait(t, s)

	// Esc, on a goroutine of its own and deliberately not waited on: the whole
	// finding is in what Cancel does after the abort, and waiting here is what
	// hid it.
	cancelled := make(chan error, 1)
	go func() { cancelled <- s.Cancel(t.Context()) }()

	if err := promptReturn(t, outA, "the cancel never reached the wait"); !errors.Is(err, ErrPromptCancelled) {
		t.Fatalf("the cancelled prompt returned %v", err)
	}
	// B, the moment A is out of the way — the TUI's promptDoneMsg → finishTurn
	// → queue drain, with nothing in between.
	res, err := s.Prompt(t.Context(), "still here")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != acp.StopEndTurn {
		t.Fatalf("the prompt after the cancel ended %q", res.StopReason)
	}
	// The old Esc has to be finished before the record can be read: the cancel
	// it would have written is on the wire by then, ahead of anything sent
	// after this.
	select {
	case err := <-cancelled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel never returned")
	}
	if _, err := s.Prompt(t.Context(), "settled"); err != nil {
		t.Fatal(err)
	}
	// Two prompts and no cancel: A never reached the agent, and neither did the
	// Esc that stopped it.
	log.waitTexts(t, "echo: still here\ncalls: prompt\necho: settled\ncalls: prompt,prompt\n")
	if cmds := commandEvents(log.snapshot()); len(cmds) != 0 {
		t.Fatalf("a cancelled prompt reported %+v", cmds[0].Command)
	}
}

// TestASecondPromptDuringTheWaitIsRefused is the other half of that race: not
// what Cancel does after the abort, but what a prompt sent while one is still
// parked may do. The wait holds the prompt slot without holding s.inPrompt, so
// the refusal has to come from the registration itself — otherwise the second
// caller either overwrites the registration (a bare name, which would park too,
// leaving Cancel with only the later channel to close while the first waiter
// timed out and sent) or opens a turn straight over the parked one (plain text,
// which never waits). Both are one prompt slot claimed twice.
//
// B goes on a goroutine so a lost guard shows up as the deadline rather than as
// a test that sits out the whole window; the pointer comparison is what says
// the registration A parked on is still the one Cancel finds.
func TestASecondPromptDuringTheWaitIsRefused(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"one that would park behind it", "/probe-echo cherry"},
		{"one that would not have waited at all", "plain words"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortCatalogWait(t, longCatalogWait)
			s := startScriptOpts(t, "nocommands", Options{PluginDirs: []string{probeFixtureDir(t)}})
			log := collect(t, s)

			// A parks in the wait, and the channel it parked on is what B must
			// not disturb.
			outA := promptOn(s, "/probe-echo banana")
			waitForCatalogWait(t, s)
			s.mu.Lock()
			parked := s.catalogAbort
			turnBefore := s.turn
			s.mu.Unlock()

			outB := promptOn(s, tc.text)
			if err := promptReturn(t, outB, "the second prompt was not refused"); !errors.Is(err, ErrPromptInFlight) {
				t.Fatalf("the second prompt returned %v", err)
			}
			s.mu.Lock()
			still, in, turn := s.catalogAbort, s.inPrompt, s.turn
			s.mu.Unlock()
			if still != parked {
				t.Fatal("the refused prompt took the wait's registration")
			}
			if in {
				t.Fatal("the refused prompt opened a turn")
			}
			// A refusal before the bookkeeping spends nothing, so there is no
			// rollback to get wrong: the number is simply untouched.
			if turn != turnBefore {
				t.Fatalf("turn=%d (was %d) after a refusal", turn, turnBefore)
			}

			// A is still the cancellable one, and Esc still ends it.
			if err := s.Cancel(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := promptReturn(t, outA, "the cancel never reached the wait"); !errors.Is(err, ErrPromptCancelled) {
				t.Fatalf("the cancelled prompt returned %v", err)
			}
			// The fake echoes every prompt it is handed, in order, so what it
			// echoes next is the whole of what ever reached it: neither A nor
			// the refused B did.
			if _, err := s.Prompt(t.Context(), "still here"); err != nil {
				t.Fatal(err)
			}
			log.waitTexts(t, "echo: still here")
			if cmds := commandEvents(log.snapshot()); len(cmds) != 0 {
				t.Fatalf("a prompt that never ran reported %+v", cmds[0].Command)
			}
		})
	}
}

// TestCatalogWaitFallsThroughToVerbatim is the fallthrough: an agent that
// never advertises a catalog leaves every row provisional forever, so the bare
// name cannot resolve however long craze waits. The window is sat out once and
// the draft then goes as the user typed it — one block, no expansion — which
// is what craze did before the wait existed.
func TestCatalogWaitFallsThroughToVerbatim(t *testing.T) {
	shortCatalogWait(t, 200*time.Millisecond)
	s := startScriptOpts(t, "nocommands", Options{PluginDirs: []string{probeFixtureDir(t)}})
	log := collect(t, s)

	if _, err := s.Prompt(t.Context(), "/probe-echo banana"); err != nil {
		t.Fatal(err)
	}
	// The fake joins text blocks with a newline, so a second block could not
	// hide in this.
	log.waitTexts(t, "echo: /probe-echo banana")
	if cmds := commandEvents(log.snapshot()); len(cmds) != 0 {
		t.Fatalf("a provisional row answered a bare name: %+v", cmds[0].Command)
	}
}

// TestBareNameResolvesOnTheFirstPrompt is the live miss this wait was added
// for (§9): a one-shot run whose only prompt carries a bare plugin name. The
// catalog lands, the row renames, and the draft goes out with its expansion
// behind it. The production window stands here — what the wait costs is timed
// in TestCatalogWaitTiming; what it buys is this.
func TestBareNameResolvesOnTheFirstPrompt(t *testing.T) {
	dir := probeFixtureDir(t)
	s := startScriptOpts(t, "commands", Options{PluginDirs: []string{dir}})
	log := collect(t, s)

	if _, err := s.Prompt(t.Context(), "/probe-echo banana"); err != nil {
		t.Fatal(err)
	}
	// Two blocks, the draft first: the whole point of having waited.
	log.waitTexts(t, twoBlockEcho(dir))
	ev := log.waitType(t, EventCommand)
	if ev.Command == nil || ev.Command.Display != "probe-echo" {
		t.Fatalf("the row did not rename: %+v", ev.Command)
	}
}

// TestCatalogWaitWakesOnTheUpdate pins what the wait blocks on, deterministically:
// the agent advertises nothing of its own, so no catalog can land on its own,
// and the only one there is goes through the real update handler once the
// prompt is provably parked. The window is far longer than promptReturn's
// deadline, so the timeout cannot be what let this prompt through.
func TestCatalogWaitWakesOnTheUpdate(t *testing.T) {
	shortCatalogWait(t, longCatalogWait)
	dir := probeFixtureDir(t)
	s := startScriptOpts(t, "nocommands", Options{PluginDirs: []string{dir}})
	log := collect(t, s)

	out := promptOn(s, "/probe-echo banana")
	waitForCatalogWait(t, s)
	s.onUpdate(commandsUpdate("research"))
	if err := promptReturn(t, out, "the update did not wake the wait"); err != nil {
		t.Fatal(err)
	}
	log.waitTexts(t, twoBlockEcho(dir))
	ev := log.waitType(t, EventCommand)
	if ev.Command == nil || ev.Command.Display != "probe-echo" {
		t.Fatalf("the row did not rename: %+v", ev.Command)
	}
}

// TestGrokNeverWaitsForACatalog: grok reads its own plugin skills off the
// wire, so --plugin-dir discovers nothing for it and no rename could rescue a
// name. A session with nothing to expand registers no wait at all, which is
// the gate rather than the clock.
func TestGrokNeverWaitsForACatalog(t *testing.T) {
	shortCatalogWait(t, longCatalogWait)
	grok := GrokProvider()
	s := newTestSession(t, Options{
		Provider:   &grok,
		PluginDirs: []string{probeFixtureDir(t)},
		Stderr:     io.Discard,
	})
	s.plugins = s.discoverPlugins(t.TempDir())
	if len(s.plugins) != 0 {
		t.Fatalf("grok discovered %+v", s.plugins)
	}
	_, waited, _, err := awaitOnce(t.Context(), s, "/probe-echo banana")
	if err != nil {
		t.Fatal(err)
	}
	if waited {
		t.Fatal("a session with no plugins registered a wait")
	}
}

// TestCancelledContextEndsTheCatalogWait: the caller has gone, so the wait
// ends with it. What happens next is what an already-cancelled context has
// always meant here — the prompt fails on the wire and the session reports it,
// which is not the cancelled-before-the-wire ending Cancel produces — and no
// expansion is reported, because none was resolved.
func TestCancelledContextEndsTheCatalogWait(t *testing.T) {
	shortCatalogWait(t, longCatalogWait)
	s := startScriptOpts(t, "nocommands", Options{PluginDirs: []string{probeFixtureDir(t)}})
	log := collect(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := make(chan error, 1)
	go func() {
		_, err := s.Prompt(ctx, "/probe-echo banana")
		out <- err
	}()
	err := promptReturn(t, out, "a dead context did not end the wait")
	if err == nil {
		t.Fatal("the prompt must fail")
	}
	if errors.Is(err, ErrPromptCancelled) {
		t.Fatalf("a dead context is not a cancel: %v", err)
	}
	if cmds := commandEvents(log.snapshot()); len(cmds) != 0 {
		t.Fatalf("a cancelled prompt reported %+v", cmds[0].Command)
	}
}
