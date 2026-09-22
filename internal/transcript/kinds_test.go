package transcript

import (
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
	// fails the first direction, and a scratch row in a copy of the table fails
	// the second.
	t.Run("a scratch constant fails both directions", func(t *testing.T) {
		dir := t.TempDir()
		src, err := os.ReadFile(filepath.Join(agentDir, "session.go"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "session.go"), src, 0o600); err != nil {
			t.Fatal(err)
		}
		scratch := "package agent\n\nconst EventScratch EventType = \"scratch\"\n"
		if err := os.WriteFile(filepath.Join(dir, "scratch.go"), []byte(scratch), 0o600); err != nil {
			t.Fatal(err)
		}
		missing, _ := diffNames(eventTypeConsts(t, dir), tableKinds())
		if !slices.Equal(missing, []string{"scratch"}) {
			t.Fatalf("a constant with no row went unnoticed: missing %v", missing)
		}
		_, extra := diffNames(eventTypeConsts(t, agentDir), append(tableKinds(), "scratch_row"))
		if !slices.Equal(extra, []string{"scratch_row"}) {
			t.Fatalf("a row with no constant went unnoticed: extra %v", extra)
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
// its table against (the mechanism of internal/engine's classify_test.go). It
// reads every file in the directory, build tags or not, so a kind declared
// outside session.go is found too; a constant whose value is not a string
// literal fails the scan rather than being skipped.
func eventTypeConsts(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "EventType" {
					continue
				}
				for i, n := range vs.Names {
					if i >= len(vs.Values) {
						t.Fatalf("%s: EventType %s has no value of its own", name, n.Name)
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: EventType %s is not a string literal; the guard cannot read it", name, n.Name)
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: EventType %s: %v", name, n.Name, err)
					}
					out = append(out, v)
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no EventType constants found under %s", dir)
	}
	slices.Sort(out)
	return out
}
