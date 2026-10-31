package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

func realDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestWhichCallsAreEdits is the table docs/reference/cli.md prints, a provider at
// a time: native and cursor by kind, grok and gx by tool name, whatever kind they
// report, and a tool neither knows not at all.
func TestWhichCallsAreEdits(t *testing.T) {
	for _, tc := range []struct {
		provider string
		call     ToolEvent
		want     bool
	}{
		{nativeName, ToolEvent{Kind: "edit", ToolName: "write"}, true},
		{nativeName, ToolEvent{Kind: "execute", ToolName: "bash"}, false},
		{nativeName, ToolEvent{Kind: "other", ToolName: "search_replace"}, false},
		{nativeName, ToolEvent{Kind: "delete"}, false},
		{nativeName, ToolEvent{Kind: "move"}, false},
		{cursorName, ToolEvent{Kind: "edit"}, true},
		{cursorName, ToolEvent{Kind: "delete"}, true},
		{cursorName, ToolEvent{Kind: "move"}, true},
		{cursorName, ToolEvent{Kind: "read"}, false},
		{cursorName, ToolEvent{Kind: "execute"}, false},
		{cursorName, ToolEvent{Kind: "other", ToolName: "write"}, false},
		{grokName, ToolEvent{Kind: "delete", ToolName: "something_new"}, false},
		{gxName, ToolEvent{Kind: "move"}, false},
		{grokName, ToolEvent{ToolName: "search_replace"}, true},
		{grokName, ToolEvent{Kind: "Other", ToolName: "write"}, true},
		{grokName, ToolEvent{Kind: "edit", ToolName: "image_edit"}, false},
		{grokName, ToolEvent{Kind: "edit"}, false},
		{gxName, ToolEvent{Kind: "other", ToolName: "search_replace"}, true},
		{gxName, ToolEvent{ToolName: "write"}, true},
		{gxName, ToolEvent{Kind: "edit", ToolName: "something_new"}, false},
		{"a provider with no rule", ToolEvent{Kind: "edit", ToolName: "write"}, false},
	} {
		if got := editCalls[tc.provider].is(tc.call); got != tc.want {
			t.Errorf("%s %+v: %v, want %v", tc.provider, tc.call, got, tc.want)
		}
	}
}

// TestAPathIsJudgedResolved: the path as it is once resolved, against the workspace
// as it is once resolved; inside means below it at a path boundary; a path inside
// is spelled from it, and a path outside is absolute.
func TestAPathIsJudgedResolved(t *testing.T) {
	root := realDir(t)
	ws := filepath.Join(root, "b")
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "kept.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A workspace reached through a symlink, as -C or macOS's /tmp can be.
	link := filepath.Join(root, "link")
	if err := os.Symlink(ws, link); err != nil {
		t.Fatal(err)
	}
	// A directory in the workspace that leads out of it, and one that leads back.
	if err := os.Symlink(filepath.Join(root, "bc"), filepath.Join(ws, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "sub"), filepath.Join(root, "in")); err != nil {
		t.Fatal(err)
	}
	// A link to a directory elsewhere, whose .. is that directory's parent.
	if err := os.MkdirAll(filepath.Join(root, "elsewhere", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "elsewhere", "sub"), filepath.Join(ws, "deep")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop", filepath.Join(ws, "loop")); err != nil {
		t.Fatal(err)
	}
	// The final element is the call's own: an edit of a link reports the link.
	if err := os.Symlink(filepath.Join(root, "bc", "elsewhere.txt"), filepath.Join(ws, "lastlink")); err != nil {
		t.Fatal(err)
	}
	for _, workspace := range []struct{ name, dir string }{{"plain", ws}, {"through a symlink", link}} {
		for _, tc := range []struct {
			name, path string
			files      []string
			outside    []string
		}{
			{name: "absolute", path: filepath.Join(ws, "kept.txt"), files: []string{"kept.txt"}},
			{name: "absolute through -C", path: filepath.Join(workspace.dir, "kept.txt"), files: []string{"kept.txt"}},
			{name: "relative", path: "sub/new.txt", files: []string{"sub/new.txt"}},
			{name: "deleted", path: filepath.Join(ws, "kept-no-more.txt"), files: []string{"kept-no-more.txt"}},
			{name: "deleted with its directory", path: filepath.Join(workspace.dir, "gone", "deep", "x.txt"), files: []string{"gone/deep/x.txt"}},
			{name: "dots that stay", path: "sub/../kept.txt", files: []string{"kept.txt"}},
			{name: "a name starting with dots", path: "..c", files: []string{"..c"}},
			{name: "dots that leave", path: "../escaped.txt", outside: []string{filepath.Join(root, "escaped.txt")}},
			{name: "dots through absolute", path: ws + "/sub/../../escaped.txt", outside: []string{filepath.Join(root, "escaped.txt")}},
			{name: "a sibling sharing the prefix", path: filepath.Join(root, "bc", "x.txt"), outside: []string{filepath.Join(root, "bc", "x.txt")}},
			{name: "the sibling itself", path: filepath.Join(root, "bc"), outside: []string{filepath.Join(root, "bc")}},
			{name: "the parent", path: root, outside: []string{root}},
			{name: "a link out", path: "out/x.txt", outside: []string{filepath.Join(root, "bc", "x.txt")}},
			{name: "a link back in", path: filepath.Join(root, "in", "x.txt"), files: []string{"sub/x.txt"}},
			{name: "a link as the final element", path: "lastlink", files: []string{"lastlink"}},
			{name: "dots past a link out land by the link's target", path: "deep/../changed.txt", outside: []string{filepath.Join(root, "elsewhere", "changed.txt")}},
			{name: "the same, absolute", path: ws + "/deep/../changed.txt", outside: []string{filepath.Join(root, "elsewhere", "changed.txt")}},
			{name: "dots past a link back in land in the workspace", path: filepath.Join(root, "in") + "/../x.txt", files: []string{"x.txt"}},
			{name: "dots past a directory that is gone", path: "gone/../x.txt", files: []string{"x.txt"}},
			{name: "dots as the final element", path: "sub/new/..", files: []string{"sub"}},
			{name: "a slash at the end", path: "sub/end/", files: []string{"sub/end"}},
			{name: "a link loop stays as spelled", path: "loop/x.txt", files: []string{"loop/x.txt"}},
			{name: "a file URI inside", path: "file://" + filepath.Join(ws, "uri.txt"), files: []string{"uri.txt"}},
			{name: "a file URI through -C", path: "file://" + filepath.Join(workspace.dir, "uri.txt"), files: []string{"uri.txt"}},
			{name: "a file URI percent-encoded", path: "file://" + filepath.Join(ws, "a%20b.txt"), files: []string{"a b.txt"}},
			{name: "a file URI on localhost", path: "file://localhost" + filepath.Join(ws, "uri.txt"), files: []string{"uri.txt"}},
			{name: "a file URI outside", path: "file://" + filepath.Join(root, "bc", "x.txt"), outside: []string{filepath.Join(root, "bc", "x.txt")}},
			{name: "the workspace itself"},
		} {
			t.Run(workspace.name+"/"+tc.name, func(t *testing.T) {
				path := tc.path
				if path == "" {
					path = ws
				}
				got := editScope{workspace: workspace.dir}.edited([]string{path})
				if tc.files == nil && tc.outside == nil {
					if got != nil {
						t.Fatalf("%+v, want no stamp", got)
					}
					return
				}
				if got == nil || !slices.Equal(got.Files, tc.files) || !slices.Equal(got.OutsideFiles, tc.outside) || got.Truncated {
					t.Fatalf("%+v, want files %q outside %q", got, tc.files, tc.outside)
				}
			})
		}
	}
}

// TestALocationThatNamesNoFile: a URI that is not a local file is left out, rather than
// read as a name in the workspace; a file with a colon in its name is still a file.
func TestALocationThatNamesNoFile(t *testing.T) {
	ws := realDir(t)
	for _, p := range []string{
		"https://example.com/a.go", "untitled:Untitled-1", "vscode-remote://ssh/a.go",
		"file://otherhost/a.go", "file:relative.go", "FILE://otherhost/a.go",
	} {
		if got := (editScope{workspace: ws}).edited([]string{p}); got != nil {
			t.Errorf("%s: %+v", p, got)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "a:b.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (editScope{workspace: ws}).edited([]string{"a:b.txt", "./c:d.txt", filepath.Join(ws, "e:f.txt")}); got == nil || !slices.Equal(got.Files, []string{"a:b.txt", "c:d.txt", "e:f.txt"}) {
		t.Fatalf("names with a colon: %+v", got)
	}
}

// TestAStampHoldsTheAgentsPath: a stamp is an identity. A row's display fields are
// sanitized and a stamp's are not: a zero-width character stays, so ab.go is not
// reported for a\u200bb.go, and a control character is the JSON encoder's to escape.
func TestAStampHoldsTheAgentsPath(t *testing.T) {
	ws := realDir(t)
	p := CursorProvider()
	s := newSession(Options{Provider: &p, Workspace: ws})
	name, ctl := "a\u200bb.go", "c\x1b[31m.go"
	d := liveDelta("c", "edit", "completed", filepath.Join(ws, name), ctl)
	d.content, _ = json.Marshal([]map[string]string{{"type": "diff", "path": filepath.Join(ws, "d\u200be.go"), "newText": "x"}})
	d.hasContent = true
	tool, _ := s.mergeTool(d)
	if tool.Edited == nil || !slices.Equal(tool.Edited.Files, []string{"d\u200be.go", name, ctl}) {
		t.Fatalf("the stamp: %+v", tool.Edited)
	}
	if tool.Locations[0] != filepath.Join(ws, "ab.go") || tool.Diffs[0].Path != filepath.Join(ws, "de.go") {
		t.Fatalf("the row's display fields: %q %q", tool.Locations, tool.Diffs[0].Path)
	}
	line, err := EncodeEvent(Event{Type: EventTool, Tool: &tool})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, `c\u001b[31m.go`) {
		t.Fatalf("the control character is not escaped: %s", line)
	}
	back, err := DecodeEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Tool.Edited.same(tool.Edited) {
		t.Fatalf("back from the wire: %q, want %q", back.Tool.Edited.Files, tool.Edited.Files)
	}
}

// TestAPathThatIsNotUTF8: judged by its bytes, so an edit inside a workspace whose
// name is not UTF-8 is inside. JSON cannot carry those bytes: on the wire each reads
// as U+FFFD, in files all the same, never a made-up outsideFiles entry.
func TestAPathThatIsNotUTF8(t *testing.T) {
	ws := filepath.Join(realDir(t), "caf\xe9")
	got := editScope{workspace: ws}.edited([]string{filepath.Join(ws, "n\xe9w.txt"), "sub/ok.txt"})
	if got == nil || !slices.Equal(got.Files, []string{"n\xe9w.txt", "sub/ok.txt"}) || got.OutsideFiles != nil {
		t.Fatalf("%+v", got)
	}
	line, err := EncodeEvent(Event{Type: EventTool, Tool: &ToolEvent{ID: "c", Status: "completed", Edited: got}})
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"n\ufffdw.txt", "sub/ok.txt"}; !slices.Equal(back.Tool.Edited.Files, want) || back.Tool.Edited.OutsideFiles != nil {
		t.Fatalf("on the wire: %q", back.Tool.Edited.Files)
	}
}

func TestAStampIsCapped(t *testing.T) {
	ws := realDir(t)
	names := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("f%03d", i)
		}
		return out
	}
	got := editScope{workspace: ws}.edited(names(editedPathsCap))
	if len(got.Files) != 64 || got.Truncated {
		t.Fatalf("64 paths: %d files, truncated %v", len(got.Files), got.Truncated)
	}
	var c callPaths
	c.note("call", names(100)...)
	c.note("call", "one more")
	if len(c["call"]) != 65 {
		t.Fatalf("%d paths noted, want one past the cap", len(c["call"]))
	}
	got = editScope{workspace: ws}.edited(c["call"])
	if len(got.Files) != 64 || !got.Truncated || got.Files[63] != "f063" {
		t.Fatalf("over the cap: %d files, truncated %v", len(got.Files), got.Truncated)
	}
}

// TestGroksPlanFileIsToldByItsDirectory: the session id that names the directory is
// the agent's own. In a live run that id (01a19050-6468…) was not craze's session's
// (01a19050-641b…), and only the agent's matches.
func TestGroksPlanFileIsToldByItsDirectory(t *testing.T) {
	home := filepath.Join(realDir(t), ".grok")
	ws := realDir(t)
	const agentID, crazeID = "01a19050-6468-79c1-ac66-263c44a65a74", "01a19050-641b-77c9-98bc-380c00df0ac9"
	cwd := url.PathEscape(ws)
	plan := filepath.Join(home, "sessions", cwd, agentID, "plan.md")
	for _, tc := range []struct {
		name, path, id string
		want           bool
	}{
		{"its own", plan, agentID, true},
		{"another cwd's spelling of the directory", filepath.Join(home, "sessions", "%2Fprivate%2Ftmp", agentID, "plan.md"), agentID, true},
		{"craze's id names no directory", plan, crazeID, false},
		{"no id yet", plan, "", false},
		{"another session's", filepath.Join(home, "sessions", cwd, crazeID, "plan.md"), agentID, false},
		{"another name", filepath.Join(home, "sessions", cwd, agentID, "notes.md"), agentID, false},
		{"deeper", filepath.Join(home, "sessions", cwd, agentID, "sub", "plan.md"), agentID, false},
		{"shallower", filepath.Join(home, "sessions", agentID, "plan.md"), agentID, false},
		{"a worktree's", filepath.Join(ws, "plan.md"), agentID, false},
		{"the same shape in the workspace", filepath.Join(ws, "sessions", cwd, agentID, "plan.md"), agentID, false},
		{"the same shape under another .grok", filepath.Join(ws, ".grok", "sessions", cwd, agentID, "plan.md"), agentID, false},
	} {
		if got := isGrokPlanFile(tc.path, home, tc.id); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	t.Run("a home through a symlink", func(t *testing.T) {
		if err := os.MkdirAll(home, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(realDir(t), "grok-link")
		if err := os.Symlink(home, link); err != nil {
			t.Fatal(err)
		}
		if !isGrokPlanFile(plan, link, agentID) {
			t.Fatal("a resolved plan path under a GROK_HOME that is a symlink is not the plan file")
		}
	})
	t.Run("where the agent sees its home", func(t *testing.T) {
		env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
		if got := grokHome(env(map[string]string{"GROK_HOME": "/g", "HOME": "/h"}), "/w"); got != "/g" {
			t.Fatalf("GROK_HOME: %q", got)
		}
		if got := grokHome(env(map[string]string{"HOME": "/h"}), "/w"); got != "/h/.grok" {
			t.Fatalf("HOME: %q", got)
		}
		if got := grokHome(env(map[string]string{"GROK_HOME": "rel/home", "HOME": "/h"}), "/w"); got != "/w/rel/home" {
			t.Fatalf("a relative GROK_HOME: %q", got)
		}
	})
	t.Run("a relative GROK_HOME, as the session spawns grok", func(t *testing.T) {
		ws, elsewhere := realDir(t), realDir(t)
		t.Chdir(elsewhere) // the host's own directory is not the agent's
		t.Setenv("GROK_HOME", "../"+filepath.Base(ws)+"/relhome")
		p := GrokProvider()
		s := newSession(Options{Provider: &p, Workspace: ws})
		s.snap.SessionID = agentID
		plan := filepath.Join(ws, "relhome", "sessions", "enc", agentID, "plan.md")
		name := "search_replace"
		s.mergeTool(toolDelta{id: "c", wireName: name})
		tool, _ := s.mergeTool(liveDelta("c", "edit", "completed", plan))
		if tool.Edited == nil || tool.Edited.PlanFile != plan || tool.Edited.Files != nil {
			t.Fatalf("%+v", tool.Edited)
		}
	})
}

// grokEditsFixture is testdata/grok-edits.jsonl, real grok session records with
// their ids, paths and text replaced, for a session sid under home in ws, as the
// client hands them over: ToolName read as acp reads it.
func grokEditsFixture(t *testing.T, ws, home, sid string) []acp.SessionNotification {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "grok-edits.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.NewReplacer("WORKSPACE", ws, "GROKHOME", home, "ENCODEDCWD", url.PathEscape(ws), "SESSION", sid).Replace(string(raw))
	var out []acp.SessionNotification
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		var m struct {
			Params struct {
				SessionID string          `json:"sessionId"`
				Update    json.RawMessage `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		var u struct {
			Meta struct {
				Tool struct {
					Name string `json:"name"`
				} `json:"x.ai/tool"`
			} `json:"_meta"`
		}
		if err := json.Unmarshal(m.Params.Update, &u); err != nil {
			t.Fatal(err)
		}
		out = append(out, acp.SessionNotification{SessionID: m.Params.SessionID, Update: m.Params.Update, ToolName: u.Meta.Tool.Name})
	}
	return out
}

// TestGrokFixture: real grok turns, whose first update reports no kind and whose
// _meta says Other: search_replace and write count by name, their status in an
// update later than their locations; a failed search_replace and a shell call do
// not; and the plan.md under the agent's session is the plan file, in neither list.
func TestGrokFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "grok-edits.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"updateParams":{"toolCallId":"call-write","title":"write","kind":"Other"`) {
		t.Fatal("the fixture no longer shows grok saying Other for an edit")
	}
	for _, provider := range []Provider{GrokProvider(), GxProvider()} {
		t.Run(provider.Name(), func(t *testing.T) {
			ws := realDir(t)
			home := filepath.Join(realDir(t), ".grok")
			t.Setenv("GROK_HOME", home)
			const sid = "the-agents-session"
			s := newSession(Options{Provider: &provider, Workspace: ws})
			s.snap.SessionID = sid
			notes := grokEditsFixture(t, ws, home, sid)
			stamped := map[string]*ToolEdited{}
			for _, n := range notes {
				s.onUpdate(n)
				for _, tool := range s.Snapshot().Tools {
					if tool.Edited != nil {
						stamped[tool.ID] = tool.Edited
					}
					if tool.Status != toolCompleted && tool.Edited != nil {
						t.Fatalf("%s is %s and stamped", tool.ID, tool.Status)
					}
				}
			}
			if len(stamped) != 2 {
				t.Fatalf("stamped %v, want the plan and the write", stamped)
			}
			wantPlan := filepath.Join(home, "sessions", url.PathEscape(ws), sid, "plan.md")
			if e := stamped["call-plan"]; e == nil || e.PlanFile != wantPlan || e.Files != nil || e.OutsideFiles != nil {
				t.Fatalf("the plan's stamp: %+v", e)
			}
			if e := stamped["call-write"]; e == nil || !slices.Equal(e.Files, []string{"notes/made.md"}) || e.OutsideFiles != nil || e.PlanFile != "" {
				t.Fatalf("the write's stamp: %+v", e)
			}
			tools := map[string]ToolEvent{}
			for _, tool := range s.Snapshot().Tools {
				tools[tool.ID] = tool
			}
			if tools["call-failed"].Status != toolFailed || tools["call-shell"].Status != toolCompleted {
				t.Fatalf("the fixture's other calls: %+v", tools)
			}
		})
	}
	t.Run("the same turn under another session's id", func(t *testing.T) {
		ws := realDir(t)
		home := filepath.Join(realDir(t), ".grok")
		t.Setenv("GROK_HOME", home)
		p := GrokProvider()
		s := newSession(Options{Provider: &p, Workspace: ws})
		s.snap.SessionID = "crazes-own-id"
		for _, n := range grokEditsFixture(t, ws, home, "the-agents-session") {
			s.onUpdate(n)
		}
		want := filepath.Join(home, "sessions", url.PathEscape(ws), "the-agents-session", "plan.md")
		for _, tool := range s.Snapshot().Tools {
			if tool.ID == "call-plan" && (tool.Edited == nil || tool.Edited.PlanFile != "" || !slices.Equal(tool.Edited.OutsideFiles, []string{want})) {
				t.Fatalf("%+v", tool.Edited)
			}
		}
	})
	t.Run("an unknown tool", func(t *testing.T) {
		ws := realDir(t)
		p := GrokProvider()
		s := newSession(Options{Provider: &p, Workspace: ws})
		for _, n := range grokEditsFixture(t, ws, realDir(t), "sid") {
			if n.ToolName == "write" {
				n.ToolName = "something_new"
			}
			s.onUpdate(n)
		}
		for _, tool := range s.Snapshot().Tools {
			if tool.ID == "call-write" && tool.Edited != nil {
				t.Fatalf("an unknown tool reporting kind edit is stamped: %+v", tool.Edited)
			}
		}
	})
}

// liveDelta is one ACP update as the merge is given it.
func liveDelta(id, kind, status string, paths ...string) toolDelta {
	d := toolDelta{id: id}
	if kind != "" {
		d.kind = &kind
	}
	if status != "" {
		d.status = &status
	}
	if paths != nil {
		items := make([]map[string]string, len(paths))
		for i, p := range paths {
			items[i] = map[string]string{"path": p}
		}
		d.locations, _ = json.Marshal(items)
		d.hasLocations = true
	}
	return d
}

// TestACursorCallIsStampedByHowItEnded: a call's kind, its paths and how it ended
// arrive in any order. Nothing but completed stamps it, a stamp goes with a status
// taken back, and a sub-agent's calls are its own.
func TestACursorCallIsStampedByHowItEnded(t *testing.T) {
	newCursor := func(t *testing.T) (*session, string) {
		ws := realDir(t)
		p := CursorProvider()
		return newSession(Options{Provider: &p, Workspace: ws}), ws
	}
	t.Run("completed, its status in a later update", func(t *testing.T) {
		s, _ := newCursor(t)
		if tool, _ := s.mergeTool(liveDelta("c", "edit", "in_progress", "a.go")); tool.Edited != nil {
			t.Fatalf("in progress: %+v", tool.Edited)
		}
		tool, emit := s.mergeTool(liveDelta("c", "", "completed"))
		if !emit || tool.Edited == nil || !slices.Equal(tool.Edited.Files, []string{"a.go"}) {
			t.Fatalf("completed: emit %v, %+v", emit, tool.Edited)
		}
		if got := s.Snapshot().Tools[0].Edited; got == nil || !slices.Equal(got.Files, []string{"a.go"}) {
			t.Fatalf("the row keeps its stamp: %+v", got)
		}
		tool, _ = s.mergeTool(toolDelta{id: "c", title: new("a later title")})
		if tool.Edited == nil {
			t.Fatal("a later update lost the stamp")
		}
	})
	t.Run("its status first, its paths after", func(t *testing.T) {
		s, _ := newCursor(t)
		if tool, _ := s.mergeTool(liveDelta("c", "edit", "completed")); tool.Edited != nil {
			t.Fatalf("no paths yet: %+v", tool.Edited)
		}
		tool, emit := s.mergeTool(liveDelta("c", "", "", "a.go"))
		if !emit || tool.Edited == nil || !slices.Equal(tool.Edited.Files, []string{"a.go"}) {
			t.Fatalf("emit %v, %+v", emit, tool.Edited)
		}
	})
	t.Run("its kind last", func(t *testing.T) {
		s, _ := newCursor(t)
		s.mergeTool(liveDelta("c", "", "completed", "a.go"))
		tool, _ := s.mergeTool(liveDelta("c", "edit", ""))
		if tool.Edited == nil || !slices.Equal(tool.Edited.Files, []string{"a.go"}) {
			t.Fatalf("%+v", tool.Edited)
		}
	})
	for _, status := range []string{"failed", "cancelled", "rejected", "pending", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			s, _ := newCursor(t)
			if tool, _ := s.mergeTool(liveDelta("c", "edit", status, "a.go")); tool.Edited != nil {
				t.Fatalf("%+v", tool.Edited)
			}
		})
	}
	t.Run("a delete, opened with no paths as cursor opens it", func(t *testing.T) {
		s, ws := newCursor(t)
		s.mergeTool(liveDelta("c", "delete", "pending"))
		s.mergeTool(liveDelta("c", "", "pending", filepath.Join(ws, "gone.txt")))
		s.mergeTool(liveDelta("c", "", "in_progress"))
		tool, emit := s.mergeTool(liveDelta("c", "", "completed"))
		if !emit || tool.Edited == nil || !slices.Equal(tool.Edited.Files, []string{"gone.txt"}) {
			t.Fatalf("emit %v, %+v", emit, tool.Edited)
		}
	})
	t.Run("a delete that failed", func(t *testing.T) {
		s, ws := newCursor(t)
		s.mergeTool(liveDelta("c", "delete", "pending", filepath.Join(ws, "kept.txt")))
		if tool, _ := s.mergeTool(liveDelta("c", "", "failed")); tool.Edited != nil {
			t.Fatalf("%+v", tool.Edited)
		}
	})
	t.Run("a move names both", func(t *testing.T) {
		s, ws := newCursor(t)
		tool, _ := s.mergeTool(liveDelta("c", "move", "completed", filepath.Join(ws, "from.txt"), filepath.Join(ws, "to.txt")))
		if tool.Edited == nil || !slices.Equal(tool.Edited.Files, []string{"from.txt", "to.txt"}) {
			t.Fatalf("%+v", tool.Edited)
		}
	})
	t.Run("a turn's end cancels what was never finished", func(t *testing.T) {
		s, _ := newCursor(t)
		s.mergeTool(liveDelta("c", "edit", "in_progress", "a.go"))
		s.closeInFlightTools("cancelled")
		if tool := s.Snapshot().Tools[0]; tool.Status != "cancelled" || tool.Edited != nil {
			t.Fatalf("%+v", tool)
		}
	})
	t.Run("a stamp goes with a status taken back", func(t *testing.T) {
		s, _ := newCursor(t)
		s.mergeTool(liveDelta("c", "edit", "completed", "a.go"))
		tool, emit := s.mergeTool(liveDelta("c", "", "failed"))
		if !emit || tool.Edited != nil {
			t.Fatalf("emit %v, %+v", emit, tool.Edited)
		}
	})
	t.Run("not an edit", func(t *testing.T) {
		s, _ := newCursor(t)
		if tool, _ := s.mergeTool(liveDelta("c", "execute", "completed", "a.go")); tool.Edited != nil {
			t.Fatalf("%+v", tool.Edited)
		}
	})
	t.Run("a turn's end leaves a stamp alone", func(t *testing.T) {
		s, _ := newCursor(t)
		s.mergeTool(liveDelta("c", "edit", "completed", "a.go"))
		s.closeInFlightTools("cancelled")
		if tool := s.Snapshot().Tools[0]; tool.Status != toolCompleted || tool.Edited == nil {
			t.Fatalf("%+v", tool)
		}
	})
	t.Run("a sub-agent's call", func(t *testing.T) {
		s, _ := newCursor(t)
		s.subagents = map[string]*subagentRec{"child": {}}
		// Grok numbers a child's calls as it numbers its parent's: one id, two calls.
		s.mergeTool(liveDelta("c", "edit", "in_progress", "parents.go"))
		tool, _, _ := s.applyToolDelta("child", liveDelta("c", "edit", "completed", "a.go"))
		if tool.Status != toolCompleted || tool.Edited != nil {
			t.Fatalf("%+v", tool)
		}
		if tool, _ := s.mergeTool(liveDelta("c", "", "completed")); tool.Edited == nil || !slices.Equal(tool.Edited.Files, []string{"parents.go"}) {
			t.Fatalf("the parent's own call: %+v", tool.Edited)
		}
	})
	t.Run("paths past the row's cap", func(t *testing.T) {
		s, _ := newCursor(t)
		paths := make([]string, 70)
		for i := range paths {
			paths[i] = fmt.Sprintf("f%02d", i)
		}
		tool, _ := s.mergeTool(liveDelta("c", "edit", "completed", paths...))
		if len(tool.Locations) != locationsCap {
			t.Fatalf("the row's locations: %d, want its own cap", len(tool.Locations))
		}
		if tool.Edited == nil || len(tool.Edited.Files) != editedPathsCap || !tool.Edited.Truncated {
			t.Fatalf("%+v", tool.Edited)
		}
	})
	t.Run("diffs past the row's cap", func(t *testing.T) {
		s, _ := newCursor(t)
		items := make([]map[string]string, 20)
		for i := range items {
			items[i] = map[string]string{"type": "diff", "path": fmt.Sprintf("f%02d", i), "newText": "x"}
		}
		content, _ := json.Marshal(items)
		d := liveDelta("c", "edit", "completed")
		d.content, d.hasContent = content, true
		tool, _ := s.mergeTool(d)
		if len(tool.Diffs) != diffsCap {
			t.Fatalf("the row's diffs: %d, want their own cap", len(tool.Diffs))
		}
		if tool.Edited == nil || len(tool.Edited.Files) != 20 || tool.Edited.Truncated {
			t.Fatalf("%+v", tool.Edited)
		}
	})
	t.Run("a call's diffs and locations are one list", func(t *testing.T) {
		s, _ := newCursor(t)
		d := liveDelta("c", "edit", "completed", "a.go")
		d.content, d.hasContent = json.RawMessage(`[{"type":"diff","path":"a.go","newText":"x"},{"type":"diff","path":"b.go","newText":"x"}]`), true
		tool, _ := s.mergeTool(d)
		if tool.Edited == nil || !slices.Equal(tool.Edited.Files, []string{"a.go", "b.go"}) {
			t.Fatalf("%+v", tool.Edited)
		}
	})
}

// TestAStampIsNotPublishedOverALaterFailure drives the one schedule that has an update
// on two goroutines, through the real client. The fake agent sends a completed edit
// before session/new's reply, which Client.NewSession flushes on Start's goroutine, and
// a failed update of the same call that the read loop takes meanwhile. The flush is held
// as it is about to stamp, long enough for the read loop to overtake it if it can: the
// last event for the call must still say what its row says.
func TestAStampIsNotPublishedOverALaterFailure(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_GATE", gate)
	s := newTestSession(t, Options{Binary: fakeAgentPath(t), ExtraArgs: []string{"-script=edit-race"}, Workspace: realDir(t), Stderr: io.Discard})
	rowStatus := func() string {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.tools["raced"].Status
	}
	testBeforeEditStamp = func() {
		f, err := os.OpenFile(gate, os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = f.Write([]byte{1})
		_ = f.Close()
		for deadline := time.Now().Add(100 * time.Millisecond); time.Now().Before(deadline) && rowStatus() != toolFailed; {
			time.Sleep(time.Millisecond)
		}
		// The read loop's publish, if nothing holds it back.
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { testBeforeEditStamp = nil })
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Every event of the call, up to a quiet moment with the row failed: a stale publish
	// comes last, however late.
	var last *ToolEvent
	deadline := time.After(5 * time.Second)
	for quiet := false; !quiet; {
		select {
		case ev := <-s.Events():
			if ev.Type == EventTool && ev.Tool != nil && ev.Tool.ID == "raced" {
				last = ev.Tool
			}
		case <-time.After(100 * time.Millisecond):
			quiet = last != nil && rowStatus() == toolFailed
		case <-deadline:
			t.Fatalf("no failure: last event %+v, row %s", last, rowStatus())
		}
	}
	if last.Status != toolFailed || last.Edited != nil {
		t.Fatalf("the row says failed, and the last event says %s, stamped %+v", last.Status, last.Edited)
	}
	if got := s.Snapshot().Tools[0]; got.Status != toolFailed || got.Edited != nil {
		t.Fatalf("the row: %+v", got)
	}
}

// TestTheStampOnTheWire: the codec carries it, and a call with none writes nothing.
func TestTheStampOnTheWire(t *testing.T) {
	ev := Event{Type: EventTool, Tool: &ToolEvent{ID: "c", Status: "completed", Edited: &ToolEdited{
		Files: []string{"a.go"}, OutsideFiles: []string{"/x/b.go"}, PlanFile: "/h/plan.md", Truncated: true}}}
	line, err := EncodeEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"edited":{"files":["a.go"],"outsideFiles":["/x/b.go"],"planFile":"/h/plan.md","truncated":true}`; !strings.Contains(line, want) {
		t.Fatalf("%s, want %s", line, want)
	}
	back, err := DecodeEvent(line)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Tool.Edited.same(ev.Tool.Edited) {
		t.Fatalf("%+v", back.Tool.Edited)
	}
	line, err = EncodeEvent(Event{Type: EventTool, Tool: &ToolEvent{ID: "c", Status: "completed"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(line, "edited") {
		t.Fatalf("a call with no stamp: %s", line)
	}
}
