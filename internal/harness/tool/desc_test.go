package tool

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	// The shape of opencode's shell.txt head, placeholders and all.
	tmpl := "${intro}\n\nBe aware: OS: ${os}, Shell: ${shell}\n\nUse `${tmp}` for temporary work."
	vars := map[string]string{
		"intro": "Executes a given bash command.",
		"os":    "linux",
		"shell": "bash",
		"tmp":   "/tmp/${not-a-placeholder}", // values are not scanned again
	}
	got, err := Render(tmpl, vars)
	if err != nil {
		t.Fatal(err)
	}
	want := "Executes a given bash command.\n\nBe aware: OS: linux, Shell: bash\n\nUse `/tmp/${not-a-placeholder}` for temporary work."
	if got != want {
		t.Fatalf("Render =\n%q\nwant\n%q", got, want)
	}
	// Text with no placeholder is its own rendering, even with no vars.
	if got, err := Render("plain $HOME and {braces}", nil); err != nil || got != "plain $HOME and {braces}" {
		t.Fatalf("Render(plain) = %q, %v", got, err)
	}
}

func TestRenderRefusesWhatItCannotFill(t *testing.T) {
	cases := map[string]struct {
		text string
		vars map[string]string
		want string
	}{
		"a placeholder with no value": {"OS: ${os}, shell: ${shell}", map[string]string{"os": "linux"}, "${shell} has no value"},
		"an unterminated placeholder": {"OS: ${os", map[string]string{"os": "linux"}, "no closing brace"},
		"a shell-style expansion":     {"echo ${HOME:-/}", map[string]string{"HOME": "/h"}, "${HOME:-/} has no value"},
		"a variable that is no name":  {"x", map[string]string{"a-b": "v"}, "not a name"},
		"an empty variable name":      {"${}", map[string]string{"": "v"}, "not a name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Render(tc.text, tc.vars)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Render(%q) = %q, %v; want an error containing %q", tc.text, got, err, tc.want)
			}
		})
	}
}
