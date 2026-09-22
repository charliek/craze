package transcript

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// agentDir is internal/agent's source directory, which the kind guard scans.
var agentDir = filepath.Join("..", "agent")

// ignoredForAChild is §3.3's child column written down: the kinds whose event,
// when it names a child, the fold deliberately drops (after creating the
// child's transcript, as the TUI's applyChildEvent does). A kind listed here
// is a decision, not a hole; one missing from both this list and the table's
// child handlers fails the guard.
var ignoredForAChild = []agent.EventType{
	agent.EventTodos,
	agent.EventPermission,
	agent.EventQuestion,
	agent.EventPlan,
	agent.EventAsk,
	agent.EventDone,
	agent.EventError,
	agent.EventMeta,
	agent.EventQueue,
	agent.EventForeignTurn,
	agent.EventReplay,
	agent.EventTurn,
}

// routedWhateverAgentSays is the one kind the fold routes to the main model
// whatever Agent says: a roster change, which is the parent's news about the
// child.
var routedWhateverAgentSays = []agent.EventType{agent.EventSubagent}

// TestFoldClassifiesEveryEventKind (plan 024 A4) holds the fold's kind table
// against internal/agent's EventType constants, found by a go/ast scan of the
// package's sources, in both directions: a kind added to agent fails here until
// it has a row, and a row for a kind agent no longer declares fails too. Each
// row must then say what a child's event of that kind does — a handler, or an
// explicit entry in ignoredForAChild — and the "deliberately ignored" list is
// itself held against the table both ways.
func TestFoldClassifiesEveryEventKind(t *testing.T) {
	missing, extra := diffNames(eventTypeConsts(t, agentDir), tableKinds())
	if len(missing) > 0 {
		t.Errorf("internal/agent declares EventType %v, which the fold's kind table (fold.go) has no row for: give it a row, or write it down as deliberately ignored", missing)
	}
	if len(extra) > 0 {
		t.Errorf("the fold's kind table has rows for %v, which internal/agent no longer declares", extra)
	}

	var gotIgnored, gotRouted []string
	for k, row := range kinds {
		if row.main == nil {
			t.Errorf("kind %q has no main handler", k)
		}
		ways := 0
		if row.child != nil {
			ways++
		}
		if row.childIgnored {
			ways++
			gotIgnored = append(gotIgnored, string(k))
		}
		if row.anyAgent {
			ways++
			gotRouted = append(gotRouted, string(k))
		}
		if ways != 1 {
			t.Errorf("kind %q must say exactly one thing about a child's event (a handler, deliberately ignored, or routed whatever Agent says); it says %d", k, ways)
		}
		if row.class == 0 || row.class&^(classStream|classState|classMarker) != 0 {
			t.Errorf("kind %q has no class, or an unknown one: %b", k, row.class)
		}
	}
	if missing, extra := diffNames(gotIgnored, typeNames(ignoredForAChild)); len(missing)+len(extra) > 0 {
		t.Errorf("the table ignores a child's %v without the list saying so, and the list says %v the table does not ignore", missing, extra)
	}
	if missing, extra := diffNames(gotRouted, typeNames(routedWhateverAgentSays)); len(missing)+len(extra) > 0 {
		t.Errorf("the table routes %v whatever Agent says without the list saying so, and the list says %v", missing, extra)
	}

	// The guard bites: a scratch constant declared in a copy of agent's sources
	// fails the first direction — in either spelling the scan reads, the type
	// written or inferred (r2 finding 8) — and a scratch row in a copy of the
	// table fails the second. A spelling the scan cannot read fails the scan
	// itself, never slips past it.
	withScratch := func(t *testing.T, scratch string) ([]string, error) {
		t.Helper()
		dir := t.TempDir()
		src, err := os.ReadFile(filepath.Join(agentDir, "session.go"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "session.go"), src, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "scratch.go"), []byte("package agent\n\n"+scratch+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return scanEventTypeConsts(dir)
	}
	t.Run("a scratch constant fails both directions", func(t *testing.T) {
		for _, scratch := range []string{
			`const EventScratch EventType = "scratch"`,
			`const EventScratch = EventType("scratch")`,
			"const (\n\tunrelated = 1\n\tEventScratch = EventType(\"scratch\")\n)",
			`const EventScratch EventType = EventType("scratch")`,
		} {
			found, err := withScratch(t, scratch)
			if err != nil {
				t.Fatalf("%s: %v", scratch, err)
			}
			if missing, _ := diffNames(found, tableKinds()); !slices.Equal(missing, []string{"scratch"}) {
				t.Fatalf("%s: a constant with no row went unnoticed: missing %v", scratch, missing)
			}
		}
		_, extra := diffNames(eventTypeConsts(t, agentDir), append(tableKinds(), "scratch_row"))
		if !slices.Equal(extra, []string{"scratch_row"}) {
			t.Fatalf("a row with no constant went unnoticed: extra %v", extra)
		}
	})
	t.Run("a spelling the scan cannot read fails it", func(t *testing.T) {
		for _, scratch := range []string{
			`const EventScratch EventType = EventText`,
			`const EventAlias = EventText`,
			`const EventScratch = (EventType)("scratch")`,
			`const EventScratch = EventType("scr" + "atch")`,
			"const (\n\tEventScratch EventType = \"scratch\"\n\tEventRepeated\n)",
			"const (\n\tEventScratch = EventType(\"scratch\")\n\tEventRepeated\n)",
		} {
			if found, err := withScratch(t, scratch); err == nil {
				t.Fatalf("%s: the scan read it as %v instead of failing", scratch, found)
			}
		}
		// What is not an EventType is not refused: an iota block of another
		// type, and untyped strings.
		for _, scratch := range []string{
			"type scratchKind int\n\nconst (\n\tscratchA scratchKind = iota\n\tscratchB\n)",
			`const scratchText = "scratch"`,
		} {
			if _, err := withScratch(t, scratch); err != nil {
				t.Fatalf("%s: %v", scratch, err)
			}
		}
	})
}

// TestFoldClassifiesEveryStateDeltaSection (plan 024 A4) holds the delta table
// (fold.go's deltaFields) against agent.StateDelta's fields by reflection, in
// both directions, and checks each is what the table calls it: a section is a
// pointer (nil = untouched), a report a plain string.
func TestFoldClassifiesEveryStateDeltaSection(t *testing.T) {
	var fields []string
	typ := reflect.TypeFor[agent.StateDelta]()
	for f := range typ.Fields() {
		fields = append(fields, f.Name)
	}
	var listed []string
	for _, f := range deltaFields {
		listed = append(listed, f.name)
		sf, ok := typ.FieldByName(f.name)
		if !ok {
			continue
		}
		switch {
		case f.report && sf.Type.Kind() != reflect.String:
			t.Errorf("StateDelta.%s is listed as a report, but it is a %v, not text", f.name, sf.Type)
		case !f.report && sf.Type.Kind() != reflect.Pointer:
			t.Errorf("StateDelta.%s is listed as a section, but it is a %v, not a pointer", f.name, sf.Type)
		}
		if f.apply == nil {
			t.Errorf("StateDelta.%s has no apply", f.name)
		}
	}
	missing, extra := diffNames(fields, listed)
	if len(missing) > 0 {
		t.Errorf("agent.StateDelta has fields %v, which the fold's delta table (fold.go deltaFields) does not classify", missing)
	}
	if len(extra) > 0 {
		t.Errorf("the fold's delta table lists %v, which agent.StateDelta no longer has", extra)
	}

	t.Run("a scratch field fails both directions", func(t *testing.T) {
		if missing, _ := diffNames(append(slices.Clone(fields), "Scratch"), listed); !slices.Equal(missing, []string{"Scratch"}) {
			t.Fatalf("a field with no row went unnoticed: %v", missing)
		}
		if _, extra := diffNames(fields, append(slices.Clone(listed), "Scratch")); !slices.Equal(extra, []string{"Scratch"}) {
			t.Fatalf("a row with no field went unnoticed: %v", extra)
		}
	})
}

func tableKinds() []string {
	out := make([]string, 0, len(kinds))
	for k := range kinds {
		out = append(out, string(k))
	}
	return out
}

func typeNames(ts []agent.EventType) []string {
	out := make([]string, len(ts))
	for i, k := range ts {
		out[i] = string(k)
	}
	return out
}

// diffNames is the difference in both directions, sorted: the names found and
// not listed, and the names listed and not found (classify_test.go's sameNames,
// returning its answer so a test can hold the guard itself to account).
func diffNames(found, listed []string) (missing, extra []string) {
	have, want := map[string]bool{}, map[string]bool{}
	for _, n := range found {
		have[n] = true
	}
	for _, n := range listed {
		want[n] = true
	}
	for n := range have {
		if !want[n] {
			missing = append(missing, n)
		}
	}
	for n := range want {
		if !have[n] {
			extra = append(extra, n)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	return missing, extra
}

// eventTypeConsts is the value of every constant of type EventType declared at
// package level in dir's non-test sources: the go/ast scan the kind guard holds
// its table against (the mechanism of internal/engine's classify_test.go),
// failing the test when the scan cannot read one (scanEventTypeConsts).
func eventTypeConsts(t *testing.T, dir string) []string {
	t.Helper()
	out, err := scanEventTypeConsts(dir)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// scanEventTypeConsts reads every file in dir, build tags or not, so a kind
// declared outside session.go is found too. It reads the two spellings an
// EventType constant can have without type information:
//
//   - a spec typed EventType, each value a string literal (or a conversion of
//     one): `EventText EventType = "text"`;
//   - an untyped spec whose value converts a string literal, the type Go
//     infers: `EventScratch = EventType("scratch")` (r2 finding 8).
//
// Any other declaration that makes an EventType constant, or might, is an
// error rather than a skip (the guard would otherwise miss a kind): a typed
// spec whose value is not a literal; an untyped value that mentions EventType
// or an EventType constant in any other way — an alias of another kind, a
// parenthesised or computed conversion; and a spec with neither type nor value
// that repeats an EventType spec implicitly inside a const block (the
// iota-style repetition). internal/agent uses none of these today.
func scanEventTypeConsts(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	names := map[string]bool{} // every EventType constant read
	var out []string
	// Checked once every file has been read, when names is complete.
	type pending struct {
		where string
		exprs []ast.Expr // an untyped value, or the spec an implicit repetition repeats
		typed bool       // the repeated spec was typed EventType
	}
	var unread []pending
	for _, e := range entries {
		file := e.Name()
		if e.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, file), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			var last *ast.ValueSpec // the spec an implicit repetition repeats
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				where := file + ": " + vs.Names[0].Name
				switch {
				case vs.Type == nil && len(vs.Values) == 0:
					if last != nil {
						unread = append(unread, pending{where: where + " (an implicit repetition)", exprs: last.Values, typed: isEventTypeName(last.Type)})
					}
					continue
				case isEventTypeName(vs.Type):
					for i, n := range vs.Names {
						if i >= len(vs.Values) {
							return nil, fmt.Errorf("%s: EventType %s has no value of its own", file, n.Name)
						}
						v, ok := literalKind(vs.Values[i], true)
						if !ok {
							return nil, fmt.Errorf("%s: EventType %s is not a string literal; the guard cannot read it", file, n.Name)
						}
						names[n.Name] = true
						out = append(out, v)
					}
				case vs.Type == nil:
					for i, n := range vs.Names {
						if i >= len(vs.Values) {
							break
						}
						if v, ok := literalKind(vs.Values[i], false); ok {
							names[n.Name] = true
							out = append(out, v)
							continue
						}
						unread = append(unread, pending{where: file + ": " + n.Name, exprs: vs.Values[i : i+1]})
					}
				}
				last = vs
			}
		}
	}
	for _, p := range unread {
		if p.typed || slices.ContainsFunc(p.exprs, func(x ast.Expr) bool { return mentionsEventType(x, names) }) {
			return nil, fmt.Errorf("%s declares an EventType constant in a form the guard cannot read: spell it `Name EventType = \"kind\"` or `Name = EventType(\"kind\")`", p.where)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no EventType constants found under %s", dir)
	}
	slices.Sort(out)
	return out, nil
}

func isEventTypeName(x ast.Expr) bool {
	id, ok := x.(*ast.Ident)
	return ok && id.Name == "EventType"
}

// literalKind reads a kind's string from x: a string literal where the spec is
// typed EventType, or the conversion EventType("kind") either way.
func literalKind(x ast.Expr, typed bool) (string, bool) {
	if call, ok := x.(*ast.CallExpr); ok && isEventTypeName(call.Fun) && len(call.Args) == 1 && !call.Ellipsis.IsValid() {
		x, typed = call.Args[0], true
	}
	lit, ok := x.(*ast.BasicLit)
	if !typed || !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// mentionsEventType reports whether x names the EventType type or one of its
// constants anywhere in it.
func mentionsEventType(x ast.Expr, names map[string]bool) bool {
	found := false
	ast.Inspect(x, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (id.Name == "EventType" || names[id.Name]) {
			found = true
		}
		return !found
	})
	return found
}
