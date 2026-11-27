package routing

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestRoutingReference(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile("../../docs/reference/" + name + ".md")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	keys := read("configuration")
	for _, v := range []any{Config{}, Settings{}, Role{}, Target{}, Route{}} {
		for f := range reflect.TypeOf(v).Fields() {
			key := f.Tag.Get("toml")
			if key == "" || !strings.Contains(keys, "`"+key+"`") {
				t.Errorf("missing configuration key %q", key)
			}
		}
	}
	doc := read("cli")
	file, err := parser.ParseFile(token.NewFileSet(), "resolve.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range file.Decls {
		decl, ok := d.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			continue
		}
		for _, s := range decl.Specs {
			v := s.(*ast.ValueSpec)
			typ, ok := v.Type.(*ast.Ident)
			if !ok || typ.Name != "Reason" {
				continue
			}
			raw := v.Values[0].(*ast.BasicLit).Value
			key, err := strconv.Unquote(raw)
			if err != nil || !strings.Contains(doc, "`"+key+"`") {
				t.Errorf("missing route reason %s", raw)
			}
		}
	}
}
