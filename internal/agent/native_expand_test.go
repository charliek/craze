package agent

import (
	"strings"
	"testing"
)

// nativeLookup is every spelling native resolves for entries: the two
// projections a session builds at Start, with nothing taken but craze's own
// builtins and nothing provisional.
func nativeLookup(entries []PluginEntry) map[string]pluginTarget {
	rows := ResolvePluginNames(entries, nil, false)
	return buildPluginLookup(entries, visibleNativeRows(entries, rows))
}

// nativeRefNames is one scan's result as "typed=args" pairs, which is
// everything the token rules have to say.
func nativeRefNames(refs []pluginRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.typed+"="+r.args)
	}
	return out
}

// TestNativeRefsStartOfLineOnly is §3.3's one deliberate departure from
// cursor's scan. Native's content is full of shell, and a command called
// "build" would otherwise turn every `cd /build` in a draft into an expansion
// of somebody's build runbook. Cursor's whitespace rule is unchanged and its
// own tests pin it; this is the other rule, beside it.
func TestNativeRefsStartOfLineOnly(t *testing.T) {
	entries := []PluginEntry{
		entryOf("project", "build", PluginKindCommand, "build body"),
		entryOf("project", "ship", PluginKindCommand, "ship body"),
	}
	lookup := nativeLookup(entries)
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"the whole draft", "/build", []string{"build="}},
		{"with arguments", "/build now please", []string{"build=now please"}},
		{"leading spaces and a tab", "   /build a\n\t/ship b", []string{"build=a", "ship=b"}},
		{"mid-line is not a reference", "cd /build && make", nil},
		{"after a word is not a reference", "see /build", nil},
		{"a second reference on the same line is arguments", "/build /ship", []string{"build=/ship"}},
		{"one per line, composed", "/build\n/ship", []string{"build=", "ship="}},
		{"a blank line between them", "/build\n\n/ship x", []string{"build=", "ship=x"}},
		{"CRLF leaves no carriage return in the arguments", "/build one\r\n/ship two\r\n", []string{"build=one", "ship=two"}},
		{"a name typed twice expands once", "/build a\n/build b", []string{"build=a"}},
		{"an unknown name is left alone", "/nope\n/build", []string{"build="}},
		{"prose is not a reference", "the / key", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nativeRefNames(nativeRefs(tc.text, lookup))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("refs %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNativeRefsStopAtTheBlockCap: expand.go's eight blocks bound a native
// draft too, and the ninth reference is simply not read.
func TestNativeRefsStopAtTheBlockCap(t *testing.T) {
	var entries []PluginEntry
	var draft strings.Builder
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		entries = append(entries, entryOf("project", n, PluginKindCommand, "body "+n))
		draft.WriteString("/" + n + "\n")
	}
	refs := nativeRefs(draft.String(), nativeLookup(entries))
	if len(refs) != maxPluginBlocks {
		t.Fatalf("%d refs, want %d", len(refs), maxPluginBlocks)
	}
	if refs[len(refs)-1].typed != "h" {
		t.Fatalf("the last reference is %q, want the eighth", refs[len(refs)-1].typed)
	}
}

// TestNativeHiddenEntryIsUnreachable is A5's expansion half: an entry marked
// user-invocable: false takes part in naming — the visible "one" is still
// qualified, because two entries claim that name — and answers to neither
// spelling when it is typed.
func TestNativeHiddenEntryIsUnreachable(t *testing.T) {
	hidden := entryOf("user", "one", PluginKindCommand, "hidden body")
	hidden.Hidden = true
	entries := []PluginEntry{entryOf("project", "one", PluginKindCommand, "visible body"), hidden}

	rows := visibleNativeRows(entries, ResolvePluginNames(entries, nil, false))
	if len(rows) != 1 || rows[0].Qualified != "project:one" {
		t.Fatalf("rows %+v, want project:one alone", rows)
	}
	// Naming ran over both, so the survivor is qualified: its spelling does
	// not depend on what was hidden.
	if rows[0].Display != "project:one" {
		t.Fatalf("display %q, want the qualified spelling", rows[0].Display)
	}
	lookup := buildPluginLookup(entries, rows)
	for _, typed := range []string{"/user:one", "/one"} {
		if refs := nativeRefs(typed, lookup); len(refs) != 0 {
			t.Fatalf("%q expanded %v", typed, nativeRefNames(refs))
		}
	}
	refs := nativeRefs("/project:one", lookup)
	if len(refs) != 1 || refs[0].target.entry.Body != "visible body" {
		t.Fatalf("the visible entry did not expand: %v", nativeRefNames(refs))
	}
}

// TestExpandNativeBody is every substitution of §3.3, for a command and for a
// skill alike — native substitutes into both, where cursor substitutes into
// neither's skill — and the one-based rule craze keeps against grok's.
func TestExpandNativeBody(t *testing.T) {
	vars := pluginVars{Root: "/root", Skill: "/root/skills/s", Session: "sess-1", Extras: true}
	for _, tc := range []struct {
		name, body, args, want string
	}{
		{"all of the arguments", "run $ARGUMENTS", "a b", "run a b"},
		{"one-based positionals", "first=$1 second=$2", "x y", "first=x second=y"},
		{"a positional with nothing behind it", "only=$1 then=$2", "x", "only=x then="},
		{"the plugin root", "at ${CLAUDE_PLUGIN_ROOT}/x", "", "at /root/x"},
		{"the skill directory", "in ${CLAUDE_SKILL_DIR}", "", "in /root/skills/s"},
		{"the session id", "id ${CLAUDE_SESSION_ID}", "", "id sess-1"},
		{"no token and no arguments", "just prose", "", "just prose"},
		{"no token with arguments", "just prose", "a b", "just prose\n\n**ARGUMENTS:** a b"},
		{"a spent $1 suppresses the fallback", "only $1", "a b", "only a"},
		// The one-pass rule: what a replacement produced is never rescanned,
		// so an argument that looks like a placeholder stays an argument.
		{"one pass over the body", "run $ARGUMENTS", "$2 final", "run $2 final"},
		{"a $ that is not a placeholder", "cost $100 and a$1", "x", "cost $100 and a$1\n\n**ARGUMENTS:** x"},
		{"${CLAUDE_PLUGIN_DATA} is not built", "at ${CLAUDE_PLUGIN_DATA}", "", "at ${CLAUDE_PLUGIN_DATA}"},
		{"$ARGUMENTS[0] is not grok's", "run $ARGUMENTS[0]", "a b", "run a b[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandNativeBody(tc.body, vars, tc.args); got != tc.want {
				t.Fatalf("body\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestNativeDollarZeroStaysLiteral (round-2 review): the positional arguments
// are one-based, so $0 names none of them. Cursor's regex matches it anyway
// and consumes it — which deletes the two characters the author wrote and
// counts as a placeholder the body spent, suppressing the fallback that would
// have carried the arguments instead — and cursor's bytes are frozen, so the
// literal reading is native's alone. $01 is a leading zero on a real index and
// keeps meaning $1 for both.
func TestNativeDollarZeroStaysLiteral(t *testing.T) {
	vars := pluginVars{Root: "/root", Extras: true}
	for _, tc := range []struct {
		name, body, args, native, cursor string
	}{
		{"$0", "run $0", "alpha", "run $0\n\n**ARGUMENTS:** alpha", "run "},
		{"$00", "run $00", "alpha", "run $00\n\n**ARGUMENTS:** alpha", "run "},
		{"$0 with no arguments at all", "run $0", "", "run $0", "run "},
		{"$01 is $1 for both", "run $01", "alpha", "run alpha", "run alpha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandNativeBody(tc.body, vars, tc.args); got != tc.native {
				t.Fatalf("native\n got %q\nwant %q", got, tc.native)
			}
			if got := expandCommandBody(tc.body, "/root", tc.args); got != tc.cursor {
				t.Fatalf("cursor\n got %q\nwant %q", got, tc.cursor)
			}
		})
	}
}

// TestCursorBodyIgnoresNativesVariables is the other side of that seam: a
// plugin body craze expands for cursor reaches cursor's agent as its author
// wrote it, so the two variables only native fills stay literal there.
func TestCursorBodyIgnoresNativesVariables(t *testing.T) {
	body := "in ${CLAUDE_SKILL_DIR} as ${CLAUDE_SESSION_ID}"
	if got := expandCommandBody(body, "/root", ""); got != body {
		t.Fatalf("cursor's command body %q, want it unchanged", got)
	}
	if got := expandSkillBody(body, "/root"); got != body {
		t.Fatalf("cursor's skill body %q, want it unchanged", got)
	}
}

// nativeBlockOf is one entry expanded as native's wire would carry it, with no
// redactor: the cases that are about the bytes of a block, rather than about
// what is kept out of them, have no session to borrow one from.
func nativeBlockOf(e PluginEntry, typed, args, sessionID string) string {
	return nativeBlock(pluginRef{target: pluginTarget{entry: e}, typed: typed, args: args}, sessionID, nil)
}

// nativeFraming is what nativeBlock puts around a body: the sentence, the
// element and its attributes for this entry and this invocation, redacted the
// way the finished block will be. A ceiling assertion needs it exactly — a
// slack of a kilobyte would hide a body that grew by hundreds of bytes under
// redaction, which is the whole of C's bug.
func nativeFraming(e PluginEntry, typed, sessionID string, redact func(string) string) int {
	empty := e
	empty.Body = ""
	return len(nativeBlock(pluginRef{target: pluginTarget{entry: empty}, typed: typed}, sessionID, redact))
}

// TestNativeBlockNamesItsSource is the new golden of §3.3: the sentence says
// where the content came from, and native's two pseudo sources are not
// plugins, so they do not claim to be. A real plugin reads exactly as it does
// for cursor — those bytes are pinned by expand_test.go and must not move.
func TestNativeBlockNamesItsSource(t *testing.T) {
	for _, tc := range []struct {
		id, want string
	}{
		{"project", `from this project`},
		{"user", `from the user's own commands`},
		{"git-commands", `from the "git-commands" plugin`},
	} {
		t.Run(tc.id, func(t *testing.T) {
			e := entryOf(tc.id, "watch-pr", PluginKindCommand, "Watch PR $ARGUMENTS.")
			want := `The user invoked /watch-pr 12 (the "watch-pr" command ` + tc.want + ", " +
				e.Path + "). Follow its instructions:\n" +
				`<command name="watch-pr" plugin="` + tc.id + `" args="12">` + "\n" +
				"Watch PR 12 until it is green.\n" +
				"</command>"
			e.Body = "Watch PR $ARGUMENTS until it is green."
			if got := nativeBlockOf(e, "watch-pr", "12", ""); got != want {
				t.Fatalf("block\n got %q\nwant %q", got, want)
			}
		})
	}
}

// TestNativeBlockSkillSubstitutes: a skill's body is substituted too, and its
// ${CLAUDE_SKILL_DIR} is the directory the SKILL.md itself sits in.
func TestNativeBlockSkillSubstitutes(t *testing.T) {
	e := entryOf("project", "linux-test", PluginKindSkill, "run ${CLAUDE_SKILL_DIR}/go.sh $1 in ${CLAUDE_SESSION_ID}")
	got := nativeBlockOf(e, "linux-test", "fast", "sess-7")
	want := "run /plugins/project/skills/linux-test/go.sh fast in sess-7"
	if !strings.Contains(got, want) {
		t.Fatalf("block %q\nis missing %q", got, want)
	}
	if !strings.Contains(got, `<skill name="linux-test" plugin="project" args="fast">`) {
		t.Fatalf("block %q has the wrong tag", got)
	}
}

// TestNativePromptJoin is §3.3's joining rule: the harness takes one string,
// the typed draft leads it, and each block follows blank-line separated, in
// the order the references were written. The event carries exactly the block
// that was sent.
func TestNativePromptJoin(t *testing.T) {
	entries := []PluginEntry{
		entryOf("project", "one", PluginKindCommand, "first body"),
		entryOf("user", "two", PluginKindSkill, "second body"),
	}
	draft := "/one a\n/two b"
	refs := nativeRefs(draft, nativeLookup(entries))
	sent, cmds := nativePrompt(draft, refs, "sess", nil)
	if len(cmds) != 2 {
		t.Fatalf("%d expansions, want two", len(cmds))
	}
	// The exact bytes, not a prefix and two containments: those would pass a
	// join that separated the blocks twice, or one that put something of its
	// own between them, and the join is the whole of what this pins.
	want := draft + nativeBlockSep + cmds[0].Text + nativeBlockSep + cmds[1].Text
	if sent != want {
		t.Fatalf("the join\n got %q\nwant %q", sent, want)
	}
	if cmds[0].Qualified != "project:one" || cmds[1].Qualified != "user:two" {
		t.Fatalf("names %q %q", cmds[0].Qualified, cmds[1].Qualified)
	}
	if cmds[1].Path != entries[1].Path || cmds[1].Kind != PluginKindSkill {
		t.Fatalf("the skill's row is wrong: %+v", cmds[1])
	}
	// A draft with nothing to expand is the draft, and no events.
	plain, none := nativePrompt("hello", nil, "sess", nil)
	if plain != "hello" || none != nil {
		t.Fatalf("plain draft %q %+v", plain, none)
	}
}

// TestNativePromptCaps is the three ceilings together. The per-body 64 KiB cap
// is expand.go's and cuts inside a block; the 128 KiB total is native's and
// drops whole blocks, because the harness replays the whole user message on
// every later request of the turn. A dropped block produces no event either:
// the ⤷ rows are what the user reads as "this was sent".
func TestNativePromptCaps(t *testing.T) {
	t.Run("one body over 64 KiB is cut inside its block", func(t *testing.T) {
		e := entryOf("project", "one", PluginKindCommand, strings.Repeat("x\n", 40<<10))
		refs := nativeRefs("/one", nativeLookup([]PluginEntry{e}))
		sent, cmds := nativePrompt("/one", refs, "", nil)
		if len(cmds) != 1 {
			t.Fatalf("%d expansions, want one", len(cmds))
		}
		// The real bound, not the ceiling plus a kilobyte of slack: the block
		// is its framing and a body that has been capped, and nothing else.
		if max := nativeFraming(e, "one", "", nil) + maxPluginBodyBytes; len(cmds[0].Text) > max {
			t.Fatalf("the block is %d bytes, over the %d its framing and the ceiling allow", len(cmds[0].Text), max)
		}
		if !strings.Contains(sent, pluginTruncated) {
			t.Fatal("a body over the ceiling was not marked truncated")
		}
	})

	t.Run("a body seeded with keys is capped after it is redacted", func(t *testing.T) {
		// Redaction grows what it rewrites — a key of 22 bytes becomes a
		// marker of 27 — so a body capped before it ran would swell back
		// through the ceiling by a fifth of its length. One line per key, just
		// over the ceiling, so the cap has something to do either way.
		line := nativeCanary + "\n"
		e := entryOf("project", "one", PluginKindCommand, strings.Repeat(line, maxPluginBodyBytes/len(line)+1))
		refs := nativeRefs("/one", nativeLookup([]PluginEntry{e}))
		redact := func(s string) string { return strings.ReplaceAll(s, nativeCanary, "[redacted-credential-marker]") }
		_, cmds := nativePrompt("/one", refs, "", redact)
		if len(cmds) != 1 {
			t.Fatalf("%d expansions, want one", len(cmds))
		}
		if max := nativeFraming(e, "one", "", redact) + maxPluginBodyBytes; len(cmds[0].Text) > max {
			t.Fatalf("the redacted block is %d bytes, over the %d its framing and the ceiling allow",
				len(cmds[0].Text), max)
		}
		// Capping before the frame is what keeps the element whole; capping
		// the framed block would have taken its closing tag off.
		if !strings.HasSuffix(cmds[0].Text, "</"+PluginKindCommand+">") {
			t.Fatalf("the block lost its closing tag: %q", cmds[0].Text[max(0, len(cmds[0].Text)-80):])
		}
		if strings.Contains(cmds[0].Text, nativeCanary) {
			t.Fatal("the cap put a key back in the block")
		}
	})

	t.Run("the block that crosses 128 KiB, and every one after it, is dropped", func(t *testing.T) {
		big := strings.Repeat("x\n", 25<<10) // 50 KiB, under the per-body ceiling
		entries := []PluginEntry{
			entryOf("project", "one", PluginKindCommand, big),
			entryOf("project", "two", PluginKindCommand, big),
			entryOf("project", "three", PluginKindCommand, big),
			entryOf("project", "four", PluginKindCommand, "small"),
		}
		draft := "/one\n/two\n/three\n/four"
		refs := nativeRefs(draft, nativeLookup(entries))
		if len(refs) != 4 {
			t.Fatalf("%d refs, want four", len(refs))
		}
		sent, cmds := nativePrompt(draft, refs, "", nil)
		if len(cmds) != 2 {
			t.Fatalf("%d expansions, want the two that fit under 128 KiB", len(cmds))
		}
		if len(sent)-len(draft) > maxNativeBlockBytes {
			t.Fatalf("the blocks add %d bytes, over the %d ceiling", len(sent)-len(draft), maxNativeBlockBytes)
		}
		if strings.Contains(sent, `name="three"`) {
			t.Fatal("the block that crossed the ceiling was still sent")
		}
		// The fourth is small and would have fitted on its own; taking it
		// after a block that did not would make what the model is shown
		// depend on the sizes of files the user never mentioned.
		if strings.Contains(sent, `name="four"`) {
			t.Fatal("a block after the one that crossed the ceiling was still sent")
		}
	})

	t.Run("blocks landing exactly on 128 KiB all fit, and one byte more does not", func(t *testing.T) {
		// The boundary itself, which the case above is nowhere near. Two
		// entries with names of the same length have framing of the same
		// length, so each block can be made to cost exactly half the ceiling —
		// its separator included, which is the byte accounting this pins.
		half := maxNativeBlockBytes / 2
		shape := entryOf("project", "one", PluginKindCommand, "")
		body := strings.Repeat("x", half-len(nativeBlockSep)-nativeFraming(shape, "one", "", nil))
		for _, tc := range []struct {
			name  string
			extra string
			want  int
		}{
			{"exactly on it", "", 2},
			{"one byte over", "x", 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				entries := []PluginEntry{
					entryOf("project", "one", PluginKindCommand, body),
					entryOf("project", "two", PluginKindCommand, body+tc.extra),
				}
				draft := "/one\n/two"
				refs := nativeRefs(draft, nativeLookup(entries))
				sent, cmds := nativePrompt(draft, refs, "", nil)
				if len(cmds) != tc.want {
					t.Fatalf("%d expansions, want %d", len(cmds), tc.want)
				}
				if added := len(sent) - len(draft); added > maxNativeBlockBytes {
					t.Fatalf("the blocks add %d bytes, over the %d ceiling", added, maxNativeBlockBytes)
				}
				if tc.want == 2 && len(sent)-len(draft) != maxNativeBlockBytes {
					t.Fatalf("the blocks add %d bytes, want the ceiling exactly", len(sent)-len(draft))
				}
			})
		}
	})
}

// TestNativePromptRedacts: the block and the event carry the same redacted
// text, because Run persists and sends the user's message unchanged and a key
// in a command file would otherwise reach the wire, the transcript, the
// journal and --json at once.
func TestNativePromptRedacts(t *testing.T) {
	e := entryOf("user", "leaky", PluginKindCommand, "export KEY="+nativeCanary)
	refs := nativeRefs("/leaky", nativeLookup([]PluginEntry{e}))
	redact := func(s string) string { return strings.ReplaceAll(s, nativeCanary, "[redacted]") }
	sent, cmds := nativePrompt("/leaky", refs, "", redact)
	if strings.Contains(sent, nativeCanary) {
		t.Fatalf("the canary reached the prompt: %q", sent)
	}
	if len(cmds) != 1 || strings.Contains(cmds[0].Text, nativeCanary) {
		t.Fatalf("the canary reached the event: %+v", cmds)
	}
	if !strings.Contains(sent, "[redacted]") {
		t.Fatalf("nothing was redacted at all: %q", sent)
	}
}

// TestNativePromptRedactsTheRecordedPath (round-2 review): the path an
// expansion records is data — it is what the event, the journal and --json
// carry, and what the block's own sentence names — so a key in it leaks
// exactly as a key in the body does. The entry keeps the real spelling, which
// is what ${CLAUDE_SKILL_DIR} is derived from and what any later reader of the
// file needs.
func TestNativePromptRedactsTheRecordedPath(t *testing.T) {
	e := entryOf("user", "leaky", PluginKindSkill, "in ${CLAUDE_SKILL_DIR}")
	e.Path = "/home/" + nativeCanary + "/.claude/skills/leaky/SKILL.md"
	refs := nativeRefs("/leaky", nativeLookup([]PluginEntry{e}))
	redact := func(s string) string { return strings.ReplaceAll(s, nativeCanary, "[redacted]") }
	sent, cmds := nativePrompt("/leaky", refs, "", redact)
	if len(cmds) != 1 {
		t.Fatalf("%d expansions, want one", len(cmds))
	}
	if strings.Contains(cmds[0].Path, nativeCanary) {
		t.Fatalf("the canary reached the recorded path: %q", cmds[0].Path)
	}
	if !strings.Contains(cmds[0].Path, "[redacted]") {
		t.Fatalf("the path was not redacted at all: %q", cmds[0].Path)
	}
	// The sentence names it too, and so does the substituted skill directory.
	if strings.Contains(sent, nativeCanary) {
		t.Fatalf("the canary reached the prompt: %q", sent)
	}
	// The entry itself is untouched: it is a filesystem handle, not a record.
	if !strings.Contains(refs[0].target.entry.Path, nativeCanary) {
		t.Fatalf("the entry's own path was rewritten: %q", refs[0].target.entry.Path)
	}
}

// TestNativeBlockDoesNotRunACommandSplice is A6's last clause. A Claude
// command file may hold !`cmd`, which Claude Code runs before the model sees
// the body; craze never does (D-46), so the three bytes reach the wire exactly
// as written and the model decides whether to run anything.
func TestNativeBlockDoesNotRunACommandSplice(t *testing.T) {
	const body = "Current branch: !`git rev-parse --abbrev-ref HEAD`\nThen act."
	e := entryOf("project", "branch", PluginKindCommand, body)
	refs := nativeRefs("/branch", nativeLookup([]PluginEntry{e}))
	sent, cmds := nativePrompt("/branch", refs, "", nil)
	if !strings.Contains(sent, body) {
		t.Fatalf("the splice did not reach the wire byte for byte:\n%q", sent)
	}
	if len(cmds) != 1 || !strings.Contains(cmds[0].Text, body) {
		t.Fatalf("the event's text lost the splice: %+v", cmds)
	}
}
