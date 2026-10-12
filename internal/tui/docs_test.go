package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

func docsModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

func readDoc(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{docsModuleRoot(t)}, rel...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// mentionsSlash reports whether doc names the slash command: `/name` alone,
// or a code span that starts with the name and goes on to its arguments
// (`/rename <title>`, `/btw <question>`).
func mentionsSlash(doc, name string) bool {
	return strings.Contains(doc, "`/"+name+"`") || strings.Contains(doc, "`/"+name+" ")
}

// TestTUIReferenceNamesEveryBuiltinSlashCommand keeps docs/reference/tui.md
// current: every builtin slash command, the conditional /sessions, /connect
// and /btw included, appears there as `/name` or `/name <args>`. The builtins
// are agent's descriptor table — every row, whatever its gate, which the menu
// and the catalog a host sends both draw from (plan 043 D1) — and the TUI's
// own client row, /sessions (P13).
func TestTUIReferenceNamesEveryBuiltinSlashCommand(t *testing.T) {
	doc := readDoc(t, "docs", "reference", "tui.md")
	names := []string{sessionsBuiltin.Name}
	for _, b := range agent.SlashBuiltins() {
		names = append(names, b.Item.Name)
	}
	for _, name := range names {
		if !mentionsSlash(doc, name) {
			t.Errorf("docs/reference/tui.md does not name the builtin slash command `/%s`", name)
		}
	}
}

// configKeys reads internal/tui/config.go for the keys the config decoder
// looks up. The decoder reads a plain map (readConfig), so there are no toml
// struct tags to reflect over. The rule, which does not depend on any
// variable's name:
//
//   - the config map is the first result of a readConfig() / readConfigAt()
//     call in a function; a string-literal index of it is a top-level key;
//   - a string-literal key passed to configSwitch, or as saveSetting's key
//     argument, is a top-level key;
//   - ConfigCompatClaude's list of {"key", &c.NoX} pairs is the
//     [compat.claude] table's keys, and AgentBinKeys is [agents]'s.
//
// Any other string-keyed index in the file is not a config key and is
// ignored. A key the scrape cannot read as a literal (a non-literal index of
// the config map, a non-literal configSwitch or saveSetting key, a compat pair
// whose key is not a literal) fails the test, since it would otherwise go
// undocumented unseen. top holds the top-level keys, tables the keys of each
// table.
func configKeys(t *testing.T) (top []string, tables map[string][]string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "config.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	str := func(e ast.Expr) (string, bool) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(lit.Value)
		return s, err == nil
	}
	calls := func(e ast.Expr, names ...string) bool {
		c, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := c.Fun.(*ast.Ident)
		return ok && slices.Contains(names, id.Name)
	}
	tables = map[string][]string{}
	add := func(dst *[]string, k string) {
		if !slices.Contains(*dst, k) {
			*dst = append(*dst, k)
		}
	}
	unseen := func(pos token.Pos, what string) {
		t.Errorf("config.go:%d: %s: the docs test cannot see which config.toml key this reads; read it through a string literal, or teach configKeys",
			fset.Position(pos).Line, what)
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			// var AgentBinKeys = []string{...}: the [agents] table's keys.
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "AgentBinKeys" || len(vs.Values) != 1 {
					continue
				}
				cl, ok := vs.Values[0].(*ast.CompositeLit)
				if !ok {
					unseen(vs.Pos(), "AgentBinKeys is not a literal list")
					continue
				}
				for _, e := range cl.Elts {
					if s, ok := str(e); ok {
						tables["agents"] = append(tables["agents"], s)
					} else {
						unseen(e.Pos(), "AgentBinKeys has a non-literal element")
					}
				}
			}
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			inCompat := d.Name.Name == "ConfigCompatClaude"
			// The config map variables of this function.
			cfgVars := map[string]bool{}
			ast.Inspect(d.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Rhs) != 1 || !calls(as.Rhs[0], "readConfig", "readConfigAt") {
					return true
				}
				if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
					cfgVars[id.Name] = true
				}
				return true
			})
			ast.Inspect(d.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.IndexExpr:
					id, ok := x.X.(*ast.Ident)
					if !ok || !cfgVars[id.Name] {
						return true // not the config map: not a config key
					}
					if s, ok := str(x.Index); ok {
						add(&top, s)
					} else if d.Name.Name != "configSwitch" { // its callers pass the key
						unseen(x.Pos(), "non-literal index of the config map in "+d.Name.Name)
					}
				case *ast.CallExpr:
					id, ok := x.Fun.(*ast.Ident)
					if !ok {
						return true
					}
					switch {
					case id.Name == "configSwitch" && len(x.Args) == 1:
						if s, ok := str(x.Args[0]); ok {
							add(&top, s)
						} else {
							unseen(x.Pos(), "non-literal configSwitch key")
						}
					case id.Name == "saveSetting" && len(x.Args) == 4:
						if s, ok := str(x.Args[2]); ok {
							add(&top, s)
						} else if d.Name.Name != "saveSetting" {
							unseen(x.Pos(), "non-literal saveSetting key")
						}
					}
				case *ast.CompositeLit:
					// ConfigCompatClaude's {"key", &c.NoX} switches.
					if inCompat && len(x.Elts) == 2 {
						if s, ok := str(x.Elts[0]); ok {
							tables["compat.claude"] = append(tables["compat.claude"], s)
						} else {
							unseen(x.Pos(), "non-literal [compat.claude] key")
						}
					}
				}
				return true
			})
		}
	}
	sort.Strings(top)
	// A scrape that quietly finds nothing would pass for ever.
	for _, want := range []string{"theme", "provider", "journal", "host_idle_exit", "control_socket", "detach"} {
		if !slices.Contains(top, want) {
			t.Fatalf("config.go scrape lost the key %q (found %v): update configKeys", want, top)
		}
	}
	if len(tables["compat.claude"]) < 6 || len(tables["agents"]) < 3 {
		t.Fatalf("config.go scrape lost a table's keys: %v", tables)
	}
	return top, tables
}

var (
	mdHeadingRE = regexp.MustCompile("^(#{1,6}) ")
	mdFenceRE   = regexp.MustCompile("^\\s*(```|~~~)")
)

// sectionWithTable is the shortest section (a heading of level 2 or deeper to
// the next heading of the same or a higher level) that shows the TOML table
// header [name] on a line of its own inside a fenced code block, allowing
// trailing whitespace and a # comment after it.
func sectionWithTable(doc, name string) (string, bool) {
	lines := strings.Split(doc, "\n")
	type head struct{ line, level int }
	var heads []head
	inFence := false
	for i, l := range lines {
		if mdFenceRE.MatchString(l) {
			inFence = !inFence
			continue
		}
		if m := mdHeadingRE.FindString(l); !inFence && m != "" && len(m)-1 >= 2 {
			heads = append(heads, head{i, len(m) - 1})
		}
	}
	header := regexp.MustCompile(`^\s*\[` + regexp.QuoteMeta(name) + `\]\s*(#.*)?$`)
	best, found := "", false
	for i, h := range heads {
		end := len(lines)
		for _, n := range heads[i+1:] {
			if n.level <= h.level {
				end = n.line
				break
			}
		}
		fenced, hit := false, false
		for _, l := range lines[h.line:end] {
			if mdFenceRE.MatchString(l) {
				fenced = !fenced
				continue
			}
			if fenced && header.MatchString(l) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		text := strings.Join(lines[h.line:end], "\n")
		if !found || len(text) < len(best) {
			best, found = text, true
		}
	}
	return best, found
}

func mentionsKey(text, key string) bool {
	return strings.Contains(text, "`"+key+"`") || regexp.MustCompile(`(?m)^\s*`+regexp.QuoteMeta(key)+` *=`).MatchString(text)
}

// TestConfigurationReferenceNamesEveryConfigKey keeps
// docs/reference/configuration.md current: every config.toml key the decoder
// reads is documented, and every key of the [agents] and [compat.claude]
// tables is documented in the section that shows the table.
func TestConfigurationReferenceNamesEveryConfigKey(t *testing.T) {
	doc := readDoc(t, "docs", "reference", "configuration.md")
	top, tables := configKeys(t)
	for _, k := range top {
		if _, isTable := tables[k]; isTable || k == "compat" {
			continue // a table: checked below, by its own section
		}
		if !mentionsKey(doc, k) {
			t.Errorf("docs/reference/configuration.md does not document the config.toml key `%s`", k)
		}
	}
	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		section, ok := sectionWithTable(doc, name)
		if !ok {
			t.Errorf("docs/reference/configuration.md has no section showing the [%s] table in a code block", name)
			continue
		}
		for _, k := range tables[name] {
			if !mentionsKey(section, k) {
				t.Errorf("docs/reference/configuration.md: the [%s] section does not document its key `%s`", name, k)
			}
		}
	}
}
