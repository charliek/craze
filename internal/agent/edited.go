package agent

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/charliek/craze/internal/harness/tool"
)

// What a call changed, as a waiting command's result reports it (ToolEdited):
// the host decides, when a call of the main session completes, because the
// host is what knows the provider, the workspace and the agent's own session
// id. A failed, refused or cancelled call, a call still running, and a
// sub-agent's call are stamped with nothing.

// editedPathsCap is the most paths one call's stamp lists; beyond it the stamp
// says Truncated. A row's own Locations and Diffs keep their smaller caps: what
// the transcript draws is the first of them, and they travel on every update of
// a call, where a stamp travels once it has completed.
const editedPathsCap = 64

// editCall says how a provider's edit calls are told from its others.
type editCall struct {
	kinds, tools []string
	// grokPlan says the provider keeps its plan file in grok's session
	// directory (isGrokPlanFile). Native's plan file is the harness's own.
	grokPlan bool
}

// editCalls is every provider's rule (docs/reference/cli.md's table says the
// same): native and cursor by kind, grok and gx by tool name.
//
// Grok is by name because its kind is not a statement about the call: its
// session/update carries none until the second update of a call, and its
// _meta.updateParams says Other for every tool. search_replace and write are
// what its own session records show changing files (tool_definitions.json and
// updates.jsonl under ~/.grok/sessions; a real turn's are in
// internal/agent/testdata/grok-edits.jsonl), and gx reports the same names
// (eval/tests/samples/gx-smoke-fix.jsonl). A tool not named here does not count,
// whatever kind it reports. A provider with no rule has no edits.
var editCalls = map[string]editCall{
	nativeName: {kinds: []string{string(tool.KindEdit)}},
	cursorName: {kinds: []string{"edit", "delete", "move"}},
	grokName:   {tools: []string{"search_replace", "write"}, grokPlan: true},
	gxName:     {tools: []string{"search_replace", "write"}, grokPlan: true},
}

// is reports whether t is an edit under the rule.
func (r editCall) is(t ToolEvent) bool {
	return slices.Contains(r.kinds, t.Kind) || slices.Contains(r.tools, t.ToolName)
}

// callPaths is every distinct path each call of a main session has named, in
// the order they were named: up to one past editedPathsCap, which is what tells
// a stamp it was cut. It sits beside the rows rather than on them because a row
// goes out with every update.
type callPaths map[string][]string

// note adds paths to call id's.
func (c *callPaths) note(id string, paths ...string) {
	if *c == nil {
		*c = callPaths{}
	}
	named := (*c)[id]
	for _, p := range paths {
		if p == "" || len(named) > editedPathsCap || slices.Contains(named, p) {
			continue
		}
		// Whole: a reader holds the slice it was handed with no lock.
		named = append(slices.Clip(named), p)
	}
	(*c)[id] = named
}

// editScope is what a path is judged against: the session's workspace, and which
// resolved path is its plan file (nil for a session with none).
type editScope struct {
	workspace string
	isPlan    func(resolved string) bool
}

// edited is the stamp for a call that completed having named paths: nil when
// that says nothing, as for a call that named no path or only the workspace.
func (sc editScope) edited(paths []string) *ToolEdited {
	if len(paths) == 0 {
		return nil
	}
	ws := filepath.Clean(sc.workspace)
	if real, err := tool.RealPath(ws); err == nil {
		ws = real
	}
	var e ToolEdited
	for i, p := range paths {
		if i == editedPathsCap {
			e.Truncated = true
			break
		}
		local, ok := localPath(ws, p)
		if !ok {
			continue
		}
		resolved := resolveEditPath(ws, local)
		rel, inside := relativeTo(ws, resolved)
		switch {
		case sc.isPlan != nil && sc.isPlan(resolved):
			e.PlanFile = resolved
		case !inside:
			e.OutsideFiles = appendDistinct(e.OutsideFiles, resolved)
		case rel != ".":
			e.Files = appendDistinct(e.Files, rel)
		}
	}
	if e.same(&ToolEdited{}) {
		return nil
	}
	return &e
}

func appendDistinct(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

// uriScheme is what a location starting like a URI starts with.
var uriScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// localPath is the file location p names: p itself, or a local file: URI's path.
// Anything else with a scheme names no file here, and ok is false, unless the
// workspace holds a file spelled that way (a:b.txt is a legal name; once that file
// is gone it reads as a URI). ws is resolved.
func localPath(ws, p string) (path string, ok bool) {
	if !uriScheme.MatchString(p) {
		return p, true
	}
	if u, err := url.Parse(p); err == nil && strings.EqualFold(u.Scheme, "file") &&
		(u.Host == "" || u.Host == "localhost") && filepath.IsAbs(u.Path) {
		return u.Path, true
	}
	if _, err := os.Lstat(filepath.Join(ws, p)); err == nil {
		return p, true
	}
	return "", false
}

// maxEditLinks bounds the symlinks one path may pass through, the most a kernel
// would follow being far fewer.
const maxEditLinks = 255

// resolveEditPath is where path lands, walked a component at a time as the OS walks
// it, from the resolved workspace when it is relative: every symlink before the final
// element followed, and a .. taken from where that left it, so that out/../x with out
// a link is the link target's sibling. The final element stays as it is, so a file the
// call deleted resolves and an edit of a link names the link; a component that is not
// there is kept, for a directory the call removed. A path that loops stays as it was
// spelled, cleaned. A link re-pointed after the call is the race the mode gate accepts
// (tool.ModeGate).
func resolveEditPath(ws, path string) string {
	sep := string(filepath.Separator)
	abs := path
	if !filepath.IsAbs(abs) {
		abs = ws + sep + abs
	}
	cur, rest, links := sep, pathElements(abs), 0
	for len(rest) > 0 {
		elem := rest[0]
		rest = rest[1:]
		if elem == ".." {
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, elem)
		if len(rest) == 0 {
			return next
		}
		target, err := os.Readlink(next)
		if err != nil {
			cur = next
			continue
		}
		if links++; links > maxEditLinks {
			return filepath.Clean(abs)
		}
		if filepath.IsAbs(target) {
			cur = sep
		}
		rest = append(pathElements(target), rest...)
	}
	return cur
}

// pathElements is p's components, bytes as they are, without the empty and "." ones
// a walk does nothing with.
func pathElements(p string) []string {
	return slices.DeleteFunc(strings.Split(p, string(filepath.Separator)), func(e string) bool { return e == "" || e == "." })
}

// relativeTo is resolved spelled from ws, and whether it is inside ws: a
// path-boundary test, so that /a/bc is not inside /a/b and ..c is a name.
func relativeTo(ws, resolved string) (string, bool) {
	rel, err := filepath.Rel(ws, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// grokHome is the directory grok keeps its sessions under, for an agent that sees
// getenv and runs in cwd: GROK_HOME, else ~/.grok. A relative GROK_HOME is taken from
// the agent's working directory, as grok and gx take it (grok 1.0 and gx 1.0.45 make
// <cwd>/<GROK_HOME>/sessions at session/new, whatever cwd the session names).
func grokHome(getenv func(string) string, cwd string) string {
	if h := getenv("GROK_HOME"); h != "" {
		return resolveEditPath(cwd, h)
	}
	home := getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".grok")
}

// isGrokPlanFile reports whether resolved is the plan file of grok's session id:
// <grok home>/sessions/<encoded cwd>/<id>/plan.md, the directory grok tells the
// model to write it in. id is the agent's own session id (Snapshot.SessionID),
// which names that directory; craze's session id does not. It is told by where
// the file is, never by its name: a plan.md anywhere else is a file like another.
// The encoded cwd is not compared, a -C through a symlink being spelled by the
// agent as it pleases.
func isGrokPlanFile(resolved, home, id string) bool {
	if id == "" || home == "" || filepath.Base(resolved) != "plan.md" {
		return false
	}
	dir := filepath.Dir(resolved)
	if filepath.Base(dir) != id {
		return false
	}
	root := filepath.Clean(home)
	if real, err := tool.RealPath(root); err == nil {
		root = real
	}
	return filepath.Dir(filepath.Dir(dir)) == filepath.Join(root, "sessions")
}

// same reports whether e and o stamp the same thing.
func (e *ToolEdited) same(o *ToolEdited) bool {
	if e == nil || o == nil {
		return e == o
	}
	return e.PlanFile == o.PlanFile && e.Truncated == o.Truncated &&
		slices.Equal(e.Files, o.Files) && slices.Equal(e.OutsideFiles, o.OutsideFiles)
}

// clone is a stamp the caller owns.
func (e *ToolEdited) clone() *ToolEdited {
	if e == nil {
		return nil
	}
	out := *e
	out.Files = slices.Clone(e.Files)
	out.OutsideFiles = slices.Clone(e.OutsideFiles)
	return &out
}

// testBeforeEditStamp, when a test sets it, runs off every lock, before a stamp's
// paths are resolved.
var testBeforeEditStamp func()

// publishToolDelta merges d into its main-session row and publishes the row. An
// update can arrive on two goroutines (Client.NewSession flushes what came before its
// reply on its caller, while the read loop goes on with what comes after), so toolPub
// keeps each update's merge, stamp and publish together: the last event for a call
// says what its row says.
func (s *session) publishToolDelta(d toolDelta) {
	s.toolPub.Lock()
	defer s.toolPub.Unlock()
	tool, changed, extras := s.applyToolDelta("", d)
	if changed {
		s.emit(Event{Type: EventTool, Tool: &tool})
	}
	s.emitAll(extras)
}

// stampEditedLive is applyToolDelta's second half: the stamp of main-session call
// t, when the merge just made of d has it completed and something new to say.
// Resolving a path takes a few lstats, so it is done off s.mu, between two sections
// of its own. toolPub is held across all of it and the publish that follows
// (publishToolDelta), so no later update of the call merges in between.
func (s *session) stampEditedLive(d toolDelta, t *ToolEvent, changed *bool) {
	rule := editCalls[s.provider().Name()]
	if d.onlyIfInFlight || t.Status != toolCompleted || !rule.is(*t) {
		return
	}
	if t.Edited != nil && !d.hasLocations && !d.hasContent {
		return
	}
	s.mu.Lock()
	paths := s.namedPaths[d.id]
	sid := s.snap.SessionID
	s.mu.Unlock()
	if testBeforeEditStamp != nil {
		testBeforeEditStamp()
	}
	scope := editScope{workspace: s.editWorkspace()}
	if rule.grokPlan {
		home := grokHome(s.childEnv(), scope.workspace)
		scope.isPlan = func(resolved string) bool { return isGrokPlanFile(resolved, home, sid) }
	}
	edited := scope.edited(paths)
	if edited.same(t.Edited) {
		return
	}
	s.mu.Lock()
	if row, ok := s.tools[d.id]; ok && row.Status == toolCompleted {
		row.Edited = edited
		s.tools[d.id] = row
	}
	s.mu.Unlock()
	t.Edited, *changed = edited.clone(), true
}

// editWorkspace is the directory the agent runs in, as start works it out.
func (s *session) editWorkspace() string {
	abs, err := filepath.Abs(s.opts.Workspace)
	if err != nil {
		return s.opts.Workspace
	}
	return abs
}

// editedNative is the stamp for native call id, finished with no error: its plan
// file is the one the harness named for the session. A row whose kind is not an
// edit has none. toolMu is not held.
func (s *nativeSession) editedNative(id string, edits []tool.FileEdit) *ToolEdited {
	s.toolMu.Lock()
	for _, e := range edits {
		s.namedPaths.note(id, e.Path)
	}
	row, paths := s.tools.rows[id], s.namedPaths[id]
	s.toolMu.Unlock()
	if !editCalls[nativeName].is(row) {
		return nil
	}
	s.mu.Lock()
	hs := s.hs
	s.mu.Unlock()
	if hs == nil {
		return nil
	}
	plan := hs.PlanPath()
	if plan != "" {
		plan = resolveEditPath(filepath.Dir(plan), plan)
	}
	scope := editScope{workspace: hs.Workspace(), isPlan: func(resolved string) bool {
		return plan != "" && tool.IsThePlan(resolved, plan)
	}}
	return scope.edited(paths)
}
