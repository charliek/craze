package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/protocol"
)

// TestEveryEngineReasonIsOnTheWire (plan 027 §3.2, C4): every code and every
// reason classify answers with is one internal/protocol publishes, the reason
// under the same code and marked the engine's; the protocol's retry policy
// is exactly classify's "never stored"; every reason the protocol marks the
// engine's is one classify produces; and this package's own transcription of
// §3.2's table (wireReasons) is the protocol's.
func TestEveryEngineReasonIsOnTheWire(t *testing.T) {
	produced := map[string]bool{}
	for _, tc := range classifyTable() {
		code := protocol.Code(tc.code)
		if !code.Known() {
			t.Errorf("%s: code %q is not in the protocol's closed set", tc.name, tc.code)
		}
		info, ok := protocol.Reason(tc.reason).Lookup()
		switch {
		case !ok:
			t.Errorf("%s: reason %q is not in the protocol's table", tc.name, tc.reason)
		case string(info.Code) != tc.code:
			t.Errorf("%s: the protocol sends reason %q under %q, the engine under %q", tc.name, tc.reason, info.Code, tc.code)
		case !info.Engine || info.ClientSide:
			t.Errorf("%s: the protocol does not mark reason %q the engine's", tc.name, tc.reason)
		}
		if protocol.Retry(code) != tc.forgot {
			t.Errorf("%s: protocol.Retry(%s) = %v, and the engine stores the answer: %v", tc.name, code, protocol.Retry(code), !tc.forgot)
		}
		produced[tc.reason] = true
	}
	for _, r := range protocol.Reasons() {
		if r.Engine && !produced[string(r.Reason)] {
			t.Errorf("the protocol marks %q the engine's, and no row of classify's table produces it", r.Reason)
		}
	}

	byCode := map[string][]string{}
	for _, r := range protocol.Reasons() {
		if !r.ClientSide {
			byCode[string(r.Code)] = append(byCode[string(r.Code)], string(r.Reason))
		}
	}
	for code, reasons := range byCode {
		want, shared := wireReasons[code]
		if !shared {
			want = []string{code}
		}
		if !slices.Equal(reasons, want) {
			t.Errorf("code %s: the protocol's reasons %v, this package's transcription of §3.2 %v", code, reasons, want)
		}
	}
	for code := range wireReasons {
		if _, ok := byCode[code]; !ok {
			t.Errorf("this package lists reasons for %q, which the protocol has no code for", code)
		}
	}
}

// constValues is every constant of the named type this package's non-test
// files declare, by value, sorted.
func constValues(t *testing.T, typeName string) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != typeName {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("a %s constant that is not a string literal", typeName)
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out = append(out, s)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func sortedStrings[T ~string](vs []T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	sort.Strings(out)
	return out
}

// TestTheEngineVocabularyIsTheWires: the engine's own words that travel on
// the wire — its activities (session.state, the sessions.list row), its
// submit modes (session.prompt's queue and send_now), its cancel outcomes and
// its setting kinds — are the protocol's, value for value, so the server
// (C6) maps them by conversion and a value added to one fails here.
func TestTheEngineVocabularyIsTheWires(t *testing.T) {
	if got, want := sortedStrings(protocol.Activities()), constValues(t, "Activity"); !slices.Equal(got, want) {
		t.Errorf("activities: protocol %v, engine %v", got, want)
	}
	modes := sortedStrings([]protocol.PromptMode{protocol.PromptQueue, protocol.PromptSendNow})
	if want := constValues(t, "SubmitMode"); !slices.Equal(modes, want) {
		t.Errorf("submit modes: protocol %v, engine %v (interject is its own call)", modes, want)
	}
	outcomes := sortedStrings([]protocol.CancelOutcome{protocol.CancelRequested, protocol.CancelSettled, protocol.CancelUnknown})
	if want := constValues(t, "CancelOutcome"); !slices.Equal(outcomes, want) {
		t.Errorf("cancel outcomes: protocol %v, engine %v", outcomes, want)
	}
	kinds := sortedStrings([]protocol.SettingKind{protocol.SettingModel, protocol.SettingMode, protocol.SettingConfig})
	if want := constValues(t, "SettingKind"); !slices.Equal(kinds, want) {
		t.Errorf("setting kinds: protocol %v, engine %v", kinds, want)
	}
}
