package errkind

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestTableIsEveryKindDeclared: the table Retryable and Known read names every Kind
// the package declares: a kind added beside the others but not to the table would
// have no default, and no row in the reference.
func TestTableIsEveryKindDeclared(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "errkind.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "Kind" {
				declared = append(declared, strings.Trim(vs.Values[0].(*ast.BasicLit).Value, `"`))
			}
		}
	}
	var all []string
	for k := range kinds {
		all = append(all, string(k))
	}
	slices.Sort(declared)
	slices.Sort(all)
	if !slices.Equal(declared, all) {
		t.Fatalf("declared %v, the table %v", declared, all)
	}
}

// TestKindsTableIsTheSet keeps docs/reference/cli.md's kinds table the closed set,
// in both directions, each row saying the retryable the code defaults to: `true`,
// `false`, or the words of the kinds whose source can say otherwise.
func TestKindsTableIsTheSet(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := kindsTable(t, string(raw))
	for k := range kinds {
		got, ok := rows[string(k)]
		if !ok {
			t.Errorf("docs/reference/cli.md's kinds table has no row for `%s`", k)
			continue
		}
		want := fmt.Sprint(k.Retryable())
		if k == Provider {
			want = "per class"
		}
		if k == Agent {
			want = "false unless flagged"
		}
		if k == Unknown {
			want = "false unless sent"
		}
		if got != want {
			t.Errorf("the kinds table says `%s`'s retryable is %q, the code %q", k, got, want)
		}
		delete(rows, string(k))
	}
	for k := range rows {
		t.Errorf("the kinds table has a row for `%s`, which is no kind", k)
	}
}

// kindsTable is the table under cli.md's kinds heading: kind to retryable.
func kindsTable(t *testing.T, doc string) map[string]string {
	t.Helper()
	_, rest, ok := strings.Cut(doc, "\n### Failure kinds\n")
	if !ok {
		t.Fatal("docs/reference/cli.md has no ### Failure kinds section")
	}
	rest, _, _ = strings.Cut(rest, "\n#")
	row := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|.*\\| ([a-z ]+) \\|$")
	out := map[string]string{}
	for _, m := range row.FindAllStringSubmatch(rest, -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatal("the kinds table has no rows")
	}
	return out
}

// TestKnown: what a reader reports. An older host says nothing, which is unknown
// and claims nothing; a later craze's kind is unknown too, its flag standing.
func TestKnown(t *testing.T) {
	for _, tc := range []struct{ in, want Info }{
		{Info{}, Info{Kind: Unknown}},
		{Info{Retryable: true}, Info{Kind: Unknown}},
		{Info{Kind: "quantum", Retryable: true, Status: 503}, Info{Kind: Unknown, Retryable: true, Status: 503}},
		{Info{Kind: Provider, Retryable: true, Status: 429, ProviderCode: "rate_limit_error"}, Info{Kind: Provider, Retryable: true, Status: 429, ProviderCode: "rate_limit_error"}},
	} {
		if got := tc.in.Known(); got != tc.want {
			t.Errorf("%+v.Known() = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	if Kind("quantum").Retryable() || Kind("").Retryable() {
		t.Error("a kind outside the set claims to be retryable")
	}
}

type said struct{ Info }

func (s said) Error() string   { return "said" }
func (s said) ErrorInfo() Info { return s.Info }

// TestOfFindsTheSourceThroughWrapping: classification made at the source survives
// any wrapping on the way up, and nothing is made up for an error that says nothing.
func TestOfFindsTheSourceThroughWrapping(t *testing.T) {
	want := Info{Kind: Quota, Status: 429}
	if got, ok := Of(fmt.Errorf("a: %w", fmt.Errorf("b: %w", said{want}))); !ok || got != want {
		t.Fatalf("through two wraps: %+v, %v", got, ok)
	}
	if got, ok := Of(errors.Join(errors.New("x"), said{want})); !ok || got != want {
		t.Fatalf("through a join: %+v, %v", got, ok)
	}
	if got, ok := Of(errors.New("x")); ok || got != (Info{}) {
		t.Fatalf("an error that says nothing: %+v, %v", got, ok)
	}
}

// TestCleanKeepsNamesAndNumbersOnly: what may ride in a result's detail. Nothing
// that could hold a key, a path, or a piece of a request survives.
func TestCleanKeepsNamesAndNumbersOnly(t *testing.T) {
	for s, want := range map[string]string{
		"insufficient_quota":    "insufficient_quota",
		"model.not-found2":      "model.not-found2",
		"":                      "",
		"sk-abc DEF":            "",
		"Bearer":                "",
		"has space":             "",
		"/etc/passwd":           "",
		"2fast":                 "",
		strings.Repeat("a", 65): "",
		strings.Repeat("a", 64): strings.Repeat("a", 64),
		`{"prompt":"secret"}`:   "",
	} {
		if got := CleanCode(s); got != want {
			t.Errorf("CleanCode(%q) = %q, want %q", s, got, want)
		}
	}
	for s, want := range map[int]int{0: 0, -32602: 0, 99: 0, 100: 100, 429: 429, 599: 599, 600: 0} {
		if got := CleanStatus(s); got != want {
			t.Errorf("CleanStatus(%d) = %d, want %d", s, got, want)
		}
	}
}
