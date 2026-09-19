package tool

import (
	"slices"
	"testing"
)

func TestChildEnviron(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"FIREWORKS_API_KEY=sk-canary-alpha-0001",
		"FIREWORKS_API_KEY_OLD=kept: not a configured name",
		"OPENROUTER_API_KEY=sk-canary-bravo-0002",
		"OPENAI_API_KEY=sk-openai",
		"OPENAI_ORG_ID=org",
		"OPENAI_CUSTOM_HEADERS=Authorization: x",
		"GITHUB_TOKEN=ghp",
		"AWS_SECRET_ACCESS_KEY=aws",
		"SSH_AUTH_SOCK=/tmp/agent",
		"NOEQUALS",
		"FIREWORKS_API_KEY=a duplicate entry",
	}
	orig := slices.Clone(base)
	got := ChildEnviron(base, []string{"FIREWORKS_API_KEY", "OPENROUTER_API_KEY"})
	want := []string{
		"PATH=/usr/bin",
		"FIREWORKS_API_KEY_OLD=kept: not a configured name",
		"GITHUB_TOKEN=ghp",
		"AWS_SECRET_ACCESS_KEY=aws",
		"SSH_AUTH_SOCK=/tmp/agent",
		"NOEQUALS",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ChildEnviron =\n%q\nwant\n%q", got, want)
	}
	if !slices.Equal(base, orig) {
		t.Fatal("ChildEnviron changed its input")
	}
	// The negative control: with no names, only OPENAI_* goes, so the
	// provider keys above were removed by name and not by accident.
	if bare := ChildEnviron(base, nil); !slices.Contains(bare, "FIREWORKS_API_KEY=sk-canary-alpha-0001") || slices.Contains(bare, "OPENAI_API_KEY=sk-openai") {
		t.Fatalf("ChildEnviron with no names = %q", bare)
	}
}

func TestEnvResolve(t *testing.T) {
	env := Env{Workspace: "/ws/project"}
	cases := map[string]string{
		"src/main.go":       "/ws/project/src/main.go",
		"./a/../b.txt":      "/ws/project/b.txt",
		"../sibling/x":      "/ws/sibling/x",
		"/etc/hosts":        "/etc/hosts",
		"/tmp//a/./b":       "/tmp/a/b",
		"~/notes.txt":       "/ws/project/~/notes.txt", // not expanded, as in opencode
		"":                  "/ws/project",
		"dir with space/f":  "/ws/project/dir with space/f",
		"/ws/project/../up": "/ws/up",
	}
	for in, want := range cases {
		if got := env.Resolve(in); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncationTruncated(t *testing.T) {
	cases := map[Truncation]bool{
		{}: false,
		{KeptBytes: 5, TotalBytes: 5, KeptLines: 1, TotalLines: 1}: false,
		{KeptBytes: 4, TotalBytes: 5, KeptLines: 1, TotalLines: 1}: true,
		{KeptBytes: 5, TotalBytes: 5, KeptLines: 1, TotalLines: 2}: true,
	}
	for tr, want := range cases {
		if got := tr.Truncated(); got != want {
			t.Errorf("%+v.Truncated() = %v, want %v", tr, got, want)
		}
	}
}
