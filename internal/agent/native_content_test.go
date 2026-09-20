package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skillDoc is the shape of a skill file: a frontmatter name and description,
// and a body underneath. extra is dropped into the frontmatter verbatim, so a
// case can write a block scalar over several lines.
func skillDoc(name, desc string, extra ...string) string {
	fm := "---\nname: " + name + "\ndescription: " + desc + "\n"
	for _, line := range extra {
		fm += line
		if !strings.HasSuffix(line, "\n") {
			fm += "\n"
		}
	}
	return fm + "---\nbody of " + name + "\n"
}

// contentFixture is a native session's world: a workspace that is its own
// repository, a home beside it, and the sources a session would resolve for
// them. The workspace holds a .git so the chain stops there — every fixture
// below means "this project", not whatever directory the test binary happens
// to be run from.
type contentFixture struct {
	src contentSources
	// workspace is the physical spelling, which is what the chain holds and
	// therefore what an entry's Root is compared against.
	workspace string
	home      string
}

func contentTree(t *testing.T, ws, home map[string]string) contentFixture {
	t.Helper()
	base := t.TempDir()
	workspace := writeTree(t, filepath.Join(base, "ws"), ws)
	gitDir(t, workspace)
	homeDir := writeTree(t, filepath.Join(base, "home"), home)
	return contentFixture{
		src:       resolveNativeSources(workspace, homeDir),
		workspace: physical(t, workspace),
		home:      homeDir,
	}
}

// discover runs the scan and hands back both halves of its contract: the
// entries, and every line it wrote about what it refused.
func (f contentFixture) discover() ([]PluginEntry, []string) {
	var lines []string
	got := discoverNative(f.src, func(msg string) { lines = append(lines, msg) })
	return got, lines
}

// wantNoLines is the assertion that a scan had nothing to say, which most of
// them should: a line is for a file craze could not offer, not for one it
// chose not to.
func wantNoLines(t *testing.T, lines []string) {
	t.Helper()
	if len(lines) != 0 {
		t.Fatalf("unexpected diagnostics %q", lines)
	}
}

// TestNativeSourceOrder is §3.2's order: workspace, then user root, then
// plugins, first winning the plugin:name key, and commands before skills
// inside each of them.
func TestNativeSourceOrder(t *testing.T) {
	t.Run("the workspace wins over the user root and a plugin", func(t *testing.T) {
		f := contentTree(t,
			map[string]string{".claude/commands/dup.md": commandFile("from the project", "body")},
			map[string]string{".claude/commands/dup.md": commandFile("from the user", "body")},
		)
		claudeFixture{
			installs: map[string][]claudeInstall{"pack@mkt": {{
				Scope: "user",
				InstallPath: writeTree(t, filepath.Join(f.home, "install"), map[string]string{
					"commands/dup.md": commandFile("from the plugin", "body"),
				}),
			}}},
			user: map[string]bool{"pack@mkt": true},
		}.write(t, f.home, f.workspace)

		got, lines := f.discover()
		wantNoLines(t, lines)
		wantNames(t, got, "project:dup", "user:dup", "pack:dup")
		if got[0].Description != "from the project" {
			t.Fatalf("description %q", got[0].Description)
		}
		// Root is what ${CLAUDE_PLUGIN_ROOT} will have to mean for each of
		// them: the chain directory, the user root, the plugin's install.
		if got[0].Root != f.workspace {
			t.Fatalf("project root %q, want %q", got[0].Root, f.workspace)
		}
		if got[1].Root != f.src.UserRoot {
			t.Fatalf("user root %q, want %q", got[1].Root, f.src.UserRoot)
		}
	})

	t.Run("a command beats a skill of one name in every source", func(t *testing.T) {
		f := contentTree(t,
			map[string]string{
				".claude/skills/both/SKILL.md": skillDoc("both", "the project skill"),
				".claude/commands/both.md":     commandFile("the project command", "body"),
			},
			map[string]string{
				".claude/skills/mine/SKILL.md": skillDoc("mine", "the user skill"),
				".claude/commands/mine.md":     commandFile("the user command", "body"),
			},
		)
		got, lines := f.discover()
		wantNoLines(t, lines)
		wantNames(t, got, "project:both", "user:mine")
		for _, e := range got {
			if e.Kind != PluginKindCommand {
				t.Fatalf("%s:%s is a %s", e.Plugin, e.Name, e.Kind)
			}
		}
	})

	t.Run("a deeper chain directory wins over the repository root", func(t *testing.T) {
		base := t.TempDir()
		repo := writeTree(t, filepath.Join(base, "repo"), map[string]string{
			".claude/commands/dup.md":  commandFile("from the root", "body"),
			".claude/commands/root.md": commandFile("only at the root", "body"),
		})
		gitDir(t, repo)
		sub := writeTree(t, filepath.Join(repo, "service"), map[string]string{
			".claude/commands/dup.md": commandFile("from the subdirectory", "body"),
		})
		got := discoverNative(resolveNativeSources(sub, ""), nil)
		wantNames(t, got, "project:dup", "project:root")
		if got[0].Description != "from the subdirectory" {
			t.Fatalf("description %q", got[0].Description)
		}
		if got[0].Root != physical(t, sub) || got[1].Root != physical(t, repo) {
			t.Fatalf("roots %q %q", got[0].Root, got[1].Root)
		}
	})

	t.Run(".claude/skills is read before .agents/skills", func(t *testing.T) {
		f := contentTree(t, map[string]string{
			".claude/skills/dup/SKILL.md": skillDoc("dup", "from .claude"),
			".agents/skills/dup/SKILL.md": skillDoc("dup", "from .agents"),
			".agents/skills/own/SKILL.md": skillDoc("own", "only in .agents"),
		}, nil)
		got, lines := f.discover()
		wantNoLines(t, lines)
		wantNames(t, got, "project:dup", "project:own")
		if got[0].Description != "from .claude" {
			t.Fatalf("description %q", got[0].Description)
		}
	})
}

// TestNativeReservedPluginIDs: "project" and "user" are craze's own words for
// its two pseudo sources, so a real plugin claiming either is skipped whole
// rather than allowed to take entries under a spelling that already means
// something else. Both directions, because each id is reserved by a different
// source.
func TestNativeReservedPluginIDs(t *testing.T) {
	for _, id := range []string{nativeProjectID, nativeUserID} {
		t.Run(id, func(t *testing.T) {
			f := contentTree(t,
				map[string]string{".claude/commands/mine.md": commandFile("the project's own", "body")},
				map[string]string{".claude/commands/yours.md": commandFile("the user's own", "body")},
			)
			key := id + "@mkt"
			claudeFixture{
				installs: map[string][]claudeInstall{key: {{
					Scope: "user",
					InstallPath: writeTree(t, filepath.Join(f.home, "install"), map[string]string{
						"commands/impostor.md": commandFile("from the plugin", "body"),
					}),
				}}},
				user: map[string]bool{key: true},
			}.write(t, f.home, f.workspace)

			got, lines := f.discover()
			// The pseudo sources are untouched and the plugin contributed
			// nothing at all, not even under another name.
			wantNames(t, got, "project:mine", "user:yours")
			if len(lines) != 1 || !strings.Contains(lines[0], `"`+id+`"`) {
				t.Fatalf("diagnostics %q, want one line naming %q", lines, id)
			}
		})
	}
}

// TestNativeSameFileDeduplication: os.SameFile, not the plugin:name key and not
// a string compare. The two cases are the ones that actually happen — a home
// directory that is also a chain directory, and one file under two names.
func TestNativeSameFileDeduplication(t *testing.T) {
	t.Run("a home that is also the workspace lists each entry once", func(t *testing.T) {
		// A dotfiles repository, or craze started in ~: the chain's
		// .claude/skills and the user root's skills are the same directory,
		// and project:name and user:name are two different dedupe keys.
		dir := writeTree(t, filepath.Join(t.TempDir(), "dotfiles"), map[string]string{
			".claude/commands/deploy.md":     commandFile("a command", "body"),
			".claude/skills/review/SKILL.md": skillDoc("review", "a skill"),
		})
		gitDir(t, dir)
		got := discoverNative(resolveNativeSources(dir, dir), nil)
		wantNames(t, got, "project:deploy", "project:review")
	})

	t.Run("one file under two names is read once", func(t *testing.T) {
		f := contentTree(t, map[string]string{
			".claude/commands/first.md": commandFile("the only body", "body"),
		}, nil)
		commands := filepath.Join(f.workspace, ".claude", "commands")
		if err := os.Link(filepath.Join(commands, "first.md"), filepath.Join(commands, "second.md")); err != nil {
			t.Skipf("hard link: %v", err)
		}
		got, lines := f.discover()
		wantNoLines(t, lines)
		// Both names are valid and the bodies are identical; what stops the
		// second is that it is the same file, which a path compare would miss
		// here and a case-insensitive volume would miss elsewhere.
		wantNames(t, got, "project:first")
	})
}

// TestNativeSkipsSyncedUserSkills: Claude's app syncs its own skills into
// skills/synced/<snapshot>/, keeping more than one snapshot, so the same skill
// is on disk under two versions of itself. Neither is offered.
func TestNativeSkipsSyncedUserSkills(t *testing.T) {
	f := contentTree(t, nil, map[string]string{
		".claude/skills/synced/2026-09-01/alpha/SKILL.md": skillDoc("alpha", "the older snapshot"),
		".claude/skills/synced/2026-09-15/alpha/SKILL.md": skillDoc("alpha", "the newer snapshot"),
		".claude/skills/own/SKILL.md":                     skillDoc("own", "the user's own"),
	})
	got, lines := f.discover()
	wantNoLines(t, lines)
	wantNames(t, got, "user:own")
}

// TestNativeVendorDefaultSkills is grok-build's denylist and the condition that
// makes it safe: the path has to say the skill came from Claude.
func TestNativeVendorDefaultSkills(t *testing.T) {
	t.Run("a .claude path drops it and another path keeps it", func(t *testing.T) {
		f := contentTree(t, map[string]string{
			".claude/skills/pdf/SKILL.md":  skillDoc("pdf", "Claude's own"),
			".agents/skills/pdf/SKILL.md":  skillDoc("pdf", "the project's own"),
			".agents/skills/docx/SKILL.md": skillDoc("docx", "also the project's own"),
		}, nil)
		got, lines := f.discover()
		wantNoLines(t, lines)
		// Lexical order inside .agents/skills, the .claude copy of pdf having
		// been dropped before it could take the name.
		wantNames(t, got, "project:docx", "project:pdf")
		if got[1].Description != "the project's own" {
			t.Fatalf("description %q", got[0].Description)
		}
	})

	t.Run("the user root is under .claude, so its defaults go", func(t *testing.T) {
		f := contentTree(t, nil, map[string]string{
			".claude/skills/xlsx/SKILL.md": skillDoc("xlsx", "Claude's own"),
			".claude/skills/kept/SKILL.md": skillDoc("kept", "the user's own"),
		})
		got, _ := f.discover()
		wantNames(t, got, "user:kept")
	})

	t.Run("a command of that name is not a vendor skill", func(t *testing.T) {
		// The denylist is about the skills Claude installs; a project that
		// writes a command called pdf wrote a command.
		f := contentTree(t, map[string]string{
			".claude/commands/pptx.md": commandFile("the project's own", "body"),
		}, nil)
		got, _ := f.discover()
		wantNames(t, got, "project:pptx")
	})

	t.Run("an enabled plugin keeps its own", func(t *testing.T) {
		// A plugin installs under <home>/.claude/plugins, so the path test
		// matches every plugin skill there is; the name test must therefore
		// not run on them, or a plugin shipping a "pdf" skill would lose it
		// for no reason but its name. No plugin on this machine is called any
		// of the five — Claude's own defaults arrive under skills/synced,
		// which is skipped whole — so this case is built rather than found.
		f := contentTree(t, nil, nil)
		claudeFixture{
			installs: map[string][]claudeInstall{"pack@mkt": {{
				Scope: "user",
				InstallPath: writeTree(t, filepath.Join(f.home, ".claude", "plugins", "pack"), map[string]string{
					"skills/pdf/SKILL.md": skillDoc("pdf", "the plugin's own"),
				}),
			}}},
			user: map[string]bool{"pack@mkt": true},
		}.write(t, f.home, f.workspace)
		got, _ := f.discover()
		wantNames(t, got, "pack:pdf")
	})
}

// TestNativeBadNameIsOneLine: the shared parsers drop a name they cannot offer
// in silence, because that is what cursor does. Native says so once per file —
// a name with a dot or a colon in it is a typo, not a decision, and a menu that
// is quietly short is no way to find that out.
func TestNativeBadNameIsOneLine(t *testing.T) {
	f := contentTree(t, map[string]string{
		".claude/commands/bad.md":     "---\nname: has:colon\ndescription: d\n---\nbody\n",
		".claude/skills/bad/SKILL.md": skillDoc("has.dot", "d"),
		".claude/commands/ok.md":      commandFile("kept", "body"),
	}, nil)
	got, lines := f.discover()
	wantNames(t, got, "project:ok")
	if len(lines) != 2 {
		t.Fatalf("diagnostics %q, want one per refused file", lines)
	}
	for _, want := range []string{
		filepath.Join(f.workspace, ".claude", "commands", "bad.md"),
		filepath.Join(f.workspace, ".claude", "skills", "bad", "SKILL.md"),
	} {
		if !strings.Contains(strings.Join(lines, "\n"), want) {
			t.Fatalf("diagnostics %q, want one naming %s", lines, want)
		}
	}
}

// TestNativeFrontmatterFields is the native parse option: the three fields
// §3.2 added, for a command and for a skill, and the keys that are read by
// nothing.
func TestNativeFrontmatterFields(t *testing.T) {
	cases := []struct {
		name      string
		fm        string
		whenToUse string
		hidden    bool
		noModel   bool
	}{
		{name: "nothing said", fm: ""},
		{name: "a plain when-to-use", fm: "when-to-use: after a release\n", whenToUse: "after a release"},
		{name: "the underscore spelling", fm: "when_to_use: after a release\n", whenToUse: "after a release"},
		{
			name:      "a folded when-to-use",
			fm:        "when-to-use: >-\n  the first half\n  the second half\n",
			whenToUse: "the first half the second half",
		},
		{
			name:      "a folded when-to-use without the chomp",
			fm:        "when-to-use: >\n  the first half\n  the second half\n",
			whenToUse: "the first half the second half",
		},
		{
			name:      "a literal when-to-use",
			fm:        "when-to-use: |\n  the first line\n  the second line\n",
			whenToUse: "the first line\nthe second line",
		},
		{
			name:      "a literal when-to-use with the chomp",
			fm:        "when-to-use: |-\n  the only line\n",
			whenToUse: "the only line",
		},
		{
			// The block ends at the next top-level key, so the two fields do
			// not run into each other.
			name:      "a following key ends the block",
			fm:        "when-to-use: >-\n  folded text\ndisable-model-invocation: true\n",
			whenToUse: "folded text",
			noModel:   true,
		},
		{name: "user-invocable false is hidden", fm: "user-invocable: false\n", hidden: true},
		{name: "the grok spelling hides it too", fm: "user_invocable: no\n", hidden: true},
		{name: "user-invocable true is not", fm: "user-invocable: true\n"},
		{name: "disable-model-invocation true", fm: "disable-model-invocation: true\n", noModel: true},
		{name: "disable-model-invocation yes", fm: "disable-model-invocation: yes\n", noModel: true},
		{name: "the case does not matter", fm: "disable-model-invocation: TRUE\n", noModel: true},
		{name: "the underscore spelling of it", fm: "disable_model_invocation: Yes\n", noModel: true},
		{
			// isFalsyScalar's rule, the other way up: YAML ends a plain scalar
			// at an unquoted " #", so a comment cannot turn the bit on.
			name:    "a trailing comment does not flip it",
			fm:      "disable-model-invocation: true # kept from the model\n",
			noModel: true,
		},
		{name: "disable-model-invocation false", fm: "disable-model-invocation: false\n"},
		{name: "a value that is neither is not truthy", fm: "disable-model-invocation: maybe\n"},
		{
			// A nested key belongs to its block, not to the document, which is
			// the rule the description and user-invocable readers already hold.
			name: "a nested key is not the document's own",
			fm:   "metadata:\n  disable-model-invocation: true\n  when-to-use: never\n",
		},
		{
			name: "the keys nothing reads are ignored",
			fm: "argument-hint: <pr number>\nallowed-tools: Bash, Read\nmodel: opus\n" +
				"effort: high\ncontext: fresh\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := contentTree(t, map[string]string{
				".claude/commands/probe-command.md": "---\nname: probe-command\ndescription: the description\n" +
					tc.fm + "---\nthe body\n",
				".claude/skills/probe-skill/SKILL.md": "---\nname: probe-skill\ndescription: the description\n" +
					tc.fm + "---\nthe body\n",
			}, nil)
			got, lines := f.discover()
			wantNoLines(t, lines)
			wantNames(t, got, "project:probe-command", "project:probe-skill")
			for _, e := range got {
				if e.Description != "the description" {
					t.Fatalf("%s description %q", e.Name, e.Description)
				}
				if e.WhenToUse != tc.whenToUse {
					t.Fatalf("%s when-to-use %q, want %q", e.Name, e.WhenToUse, tc.whenToUse)
				}
				if e.Hidden != tc.hidden || e.NoModel != tc.noModel {
					t.Fatalf("%s hidden=%v noModel=%v, want %v %v", e.Name, e.Hidden, e.NoModel, tc.hidden, tc.noModel)
				}
			}
		})
	}
}

// TestNativeKeepsAHiddenSkillCursorDrops is the fork between the two readings,
// from both sides: cursor's loader would not offer the file at all, and
// native's list keeps it so that naming is stable and the model still sees it.
// Everything else about the entry is the same.
func TestNativeKeepsAHiddenSkillCursorDrops(t *testing.T) {
	files := map[string]string{
		"skills/hidden/SKILL.md": skillDoc("hidden", "a hidden skill", "user-invocable: false"),
		"skills/shown/SKILL.md":  skillDoc("shown", "a visible skill"),
	}
	workspace := t.TempDir()
	writeTree(t, filepath.Join(workspace, "p"), files)
	wantNames(t, DiscoverPlugins(PluginScan{Dirs: true}, workspace, "", []string{"p"}), "p:shown")

	f := contentTree(t, map[string]string{
		".claude/skills/hidden/SKILL.md": files["skills/hidden/SKILL.md"],
		".claude/skills/shown/SKILL.md":  files["skills/shown/SKILL.md"],
	}, nil)
	got, _ := f.discover()
	wantNames(t, got, "project:hidden", "project:shown")
	if !got[0].Hidden || got[1].Hidden {
		t.Fatalf("hidden bits %v %v", got[0].Hidden, got[1].Hidden)
	}

	// The three fields are the native option's and nothing else's: a provider
	// that loads its own content sees a file saying all of them and still gets
	// the zero value, so no row of cursor's can move on their account.
	loud := map[string]string{
		"skills/loud/SKILL.md": skillDoc("loud", "d",
			"when-to-use: whenever", "user-invocable: false", "disable-model-invocation: true"),
		"commands/loudcmd.md": "---\nname: loudcmd\ndescription: d\nwhen-to-use: whenever\n" +
			"user-invocable: false\ndisable-model-invocation: true\n---\nbody\n",
	}
	cursorWorkspace := t.TempDir()
	writeTree(t, filepath.Join(cursorWorkspace, "p"), loud)
	cursor := DiscoverPlugins(PluginScan{Dirs: true}, cursorWorkspace, "", []string{"p"})
	wantNames(t, cursor, "p:loudcmd")
	if e := cursor[0]; e.WhenToUse != "" || e.Hidden || e.NoModel {
		t.Fatalf("cursor filled a native field: %+v", e)
	}
}

// TestNativeLayoutDepth: a project's commands and skills are read at the depth
// the convention puts them and no deeper, and the user's skills are the one
// place a nested tree is walked.
func TestNativeLayoutDepth(t *testing.T) {
	f := contentTree(t,
		map[string]string{
			".claude/commands/top.md":          commandFile("read", "body"),
			".claude/commands/nested/deep.md":  commandFile("not read", "body"),
			".claude/commands/other.txt":       commandFile("not markdown", "body"),
			".claude/skills/one/SKILL.md":      skillDoc("one", "read"),
			".claude/skills/one/two/SKILL.md":  skillDoc("two", "not read"),
			".claude/skills/README.md":         "not a skill\n",
			".agents/skills/three/SKILL.md":    skillDoc("three", "read"),
			".claude/skills/four/OTHER.md":     skillDoc("four", "not a SKILL.md"),
			".claude/skills/five/SKILL.md.bak": skillDoc("five", "not a SKILL.md"),
		},
		map[string]string{
			// The user files skills by hand, so the walk under the user root
			// is the recursive one.
			".claude/skills/flat/SKILL.md":               skillDoc("flat", "read"),
			".claude/skills/topic/nested/SKILL.md":       skillDoc("nested", "read"),
			".claude/skills/topic/deeper/one/SKILL.md":   skillDoc("deeper", "read"),
			".claude/commands/user-top.md":               commandFile("read", "body"),
			".claude/commands/nested/user-deep.md":       commandFile("not read", "body"),
			".claude/rules/not-content.md":               "rules are the instruction loader's\n",
			".claude/plugins/installed_plugins.json":     "{}",
			".claude/skills/topic/notes.md":              "not a skill\n",
			".claude/skills/topic/nested/reference/x.md": "not a skill\n",
		},
	)
	got, lines := f.discover()
	wantNoLines(t, lines)
	wantNames(t, got,
		"project:top", "project:one", "project:three",
		"user:user-top", "user:flat", "user:deeper", "user:nested",
	)
}

// TestNativeUserSkillDepthCap: the recursive walk stops where the provider
// skill walk stops, so a tree nobody pruned cannot be followed forever.
func TestNativeUserSkillDepthCap(t *testing.T) {
	deep := filepath.Join(".claude", "skills")
	files := map[string]string{}
	for i := 1; i <= maxSkillDepth+2; i++ {
		deep = filepath.Join(deep, fmt.Sprintf("d%02d", i))
		files[filepath.ToSlash(filepath.Join(deep, "SKILL.md"))] = skillDoc(fmt.Sprintf("depth%02d", i), "d")
	}
	f := contentTree(t, nil, files)
	got, _ := f.discover()
	// A directory at relative depth maxSkillDepth is the last one entered, so
	// the file inside it is the deepest read — the same boundary the provider
	// skill walk draws with relDepth.
	want := make([]string, 0, maxSkillDepth)
	for i := 1; i <= maxSkillDepth; i++ {
		want = append(want, fmt.Sprintf("user:depth%02d", i))
	}
	wantNames(t, got, want...)
}

// TestNativeBudgetIsOneAcrossTheSources: the file budget is the scan's, not
// each source's, so a workspace that exhausts it leaves nothing for the user
// root or the plugins behind it.
func TestNativeBudgetIsOneAcrossTheSources(t *testing.T) {
	ws := make(map[string]string, maxPluginFiles)
	for i := 0; i < maxPluginFiles; i++ {
		ws[fmt.Sprintf(".claude/commands/cmd-%04d.md", i)] = commandFile("d", "body")
	}
	f := contentTree(t, ws, map[string]string{
		".claude/commands/user-cmd.md":    commandFile("d", "body"),
		".claude/skills/user-sk/SKILL.md": skillDoc("user-sk", "d"),
	})
	got, _ := f.discover()
	if len(got) != maxPluginFiles {
		t.Fatalf("read %d entries, want the budget %d", len(got), maxPluginFiles)
	}
	for _, e := range got {
		if e.Plugin != nativeProjectID {
			t.Fatalf("%s:%s was read past the budget", e.Plugin, e.Name)
		}
	}
}

// TestNativeSymlinksAndGitignore: a link where content should be is not
// followed, whichever end of the layout it is at, and .gitignore is not
// consulted at all — grok-build honours it, craze deliberately does not (§4).
func TestNativeSymlinksAndGitignore(t *testing.T) {
	t.Run("a symlinked commands or skills root is skipped", func(t *testing.T) {
		f := contentTree(t, map[string]string{
			"outside/secret.md":             commandFile("secret", "body"),
			"outside/secret-skill/SKILL.md": skillDoc("secret-skill", "secret"),
			".claude/keep":                  "",
		}, nil)
		for _, rel := range []string{"commands", "skills"} {
			if err := os.Symlink(filepath.Join(f.workspace, "outside"), filepath.Join(f.workspace, ".claude", rel)); err != nil {
				t.Skipf("symlink: %v", err)
			}
		}
		got, lines := f.discover()
		wantNoLines(t, lines)
		wantNames(t, got)
	})

	t.Run("a symlinked entry file is skipped", func(t *testing.T) {
		f := contentTree(t, map[string]string{
			"outside/linked.md":             commandFile("linked", "body"),
			"outside/linked-skill/SKILL.md": skillDoc("linked-skill", "linked"),
			".claude/commands/kept.md":      commandFile("kept", "body"),
			".claude/skills/kept/SKILL.md":  skillDoc("kept-skill", "kept"),
			".claude/skills/linked/keep":    "",
		}, nil)
		links := map[string]string{
			filepath.Join(f.workspace, ".claude", "commands", "linked.md"):        filepath.Join(f.workspace, "outside", "linked.md"),
			filepath.Join(f.workspace, ".claude", "skills", "linked", "SKILL.md"): filepath.Join(f.workspace, "outside", "linked-skill", "SKILL.md"),
		}
		for link, target := range links {
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlink: %v", err)
			}
		}
		got, lines := f.discover()
		wantNoLines(t, lines)
		wantNames(t, got, "project:kept", "project:kept-skill")
	})

	t.Run("a symlinked user skill directory is skipped", func(t *testing.T) {
		f := contentTree(t, nil, map[string]string{
			"outside/linked/SKILL.md":      skillDoc("linked", "linked"),
			".claude/skills/kept/SKILL.md": skillDoc("kept", "kept"),
		})
		if err := os.Symlink(filepath.Join(f.home, "outside", "linked"),
			filepath.Join(f.home, ".claude", "skills", "linked")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		got, _ := f.discover()
		wantNames(t, got, "user:kept")
	})

	t.Run("a .gitignore changes nothing", func(t *testing.T) {
		f := contentTree(t, map[string]string{
			".gitignore":                    ".claude/\n.agents/\n",
			".claude/commands/ignored.md":   commandFile("still read", "body"),
			".claude/skills/one/SKILL.md":   skillDoc("one", "still read"),
			".claude/skills/one/.gitignore": "*\n",
		}, nil)
		got, _ := f.discover()
		wantNames(t, got, "project:ignored", "project:one")
	})
}

// TestNativeRelocatedUserRoot is A2, the seam's whole point: the loaders are
// told where content is and go nowhere else. The user root here has another
// name in another place, there is no home at all, and no plugin source — so a
// reader that reached for the developer's own ~/.claude would show up as an
// entry this fixture never wrote.
func TestNativeRelocatedUserRoot(t *testing.T) {
	base := t.TempDir()
	workspace := writeTree(t, filepath.Join(base, "ws"), map[string]string{
		".claude/commands/project-cmd.md": commandFile("the project's", "body"),
	})
	gitDir(t, workspace)
	imported := writeTree(t, filepath.Join(base, "elsewhere", "imported-claude-content"), map[string]string{
		"commands/imported-cmd.md":         commandFile("the user's", "body"),
		"skills/imported-skill/SKILL.md":   skillDoc("imported-skill", "the user's"),
		"skills/synced/v1/vendor/SKILL.md": skillDoc("vendor", "still skipped"),
	})

	src := contentSources{
		Chain:    []string{physical(t, workspace)},
		UserRoot: imported,
		Layout:   claudeLayout(),
		Plugins:  PluginScan{},
		Home:     "",
	}
	var lines []string
	got := discoverNative(src, func(msg string) { lines = append(lines, msg) })
	wantNoLines(t, lines)
	wantNames(t, got, "project:project-cmd", "user:imported-cmd", "user:imported-skill")
	for _, e := range got[1:] {
		if e.Root != imported {
			t.Fatalf("user root %q, want %q", e.Root, imported)
		}
		if !strings.HasPrefix(e.Path, imported+string(os.PathSeparator)) {
			t.Fatalf("path %q is not under the relocated root", e.Path)
		}
	}
}

// TestNativeDiscoveryNeverFails: the shapes a half-written or hostile tree
// comes in each contribute nothing, and none of them stops the scan.
func TestNativeDiscoveryNeverFails(t *testing.T) {
	f := contentTree(t, map[string]string{
		".claude/commands/unclosed.md": "---\nname: unclosed\n\nno closing delimiter\n",
		".claude/commands/empty.md":    "---\ndescription: nothing follows\n---\n\n   \n",
		".claude/commands/huge.md":     commandFile("huge", strings.Repeat("x", maxPluginBytes+1)),
		".claude/commands/kept.md":     commandFile("kept", "body"),
		".claude/skills/nofile/keep":   "",
	}, map[string]string{
		".claude/settings.json":                  "{not json",
		".claude/plugins/installed_plugins.json": "[[[",
		".claude/commands/user-kept.md":          commandFile("kept", "body"),
	})
	got, lines := f.discover()
	wantNoLines(t, lines)
	wantNames(t, got, "project:kept", "user:user-kept")

	// Nothing at all: no chain, no user root, no plugins.
	if got := discoverNative(contentSources{Layout: claudeLayout()}, nil); len(got) != 0 {
		t.Fatalf("empty sources found %v", qualifiedNames(got))
	}

	// A sources value with no layout names nowhere to look, which must not
	// resolve to the chain directory itself and read every markdown file
	// beside the workspace.
	bare := discoverNative(contentSources{Chain: []string{f.workspace}, UserRoot: f.src.UserRoot}, nil)
	if len(bare) != 0 {
		t.Fatalf("a layout-less scan found %v", qualifiedNames(bare))
	}
}
