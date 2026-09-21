package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSlug(t *testing.T) {
	for _, tc := range []struct{ cwd, want string }{
		{"/home/me/src/app", "--home-me-src-app--"},
		{"/", "----"},
		{"/Users/me/My Project", "--Users-me-My Project--"},
		// A Windows-style path slugs the same way on any host: both
		// separators and the drive colon become '-'.
		{`C:\Users\me\app`, "--C--Users-me-app--"},
		{`\\server\share\app`, "---server-share-app--"},
		{"/srv/a:b", "--srv-a-b--"},
	} {
		if got := Slug(tc.cwd); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.cwd, got, tc.want)
		}
	}
}

// The plan file is the transcript's sibling, and created once: empty when
// nothing is there, and never touched again — a second call leaves what the
// model wrote exactly as it is (plan 023 §3.2).
func TestPlanFile(t *testing.T) {
	s := newStore(t, testOptions(t))
	plan := PlanPath(s.Path())
	if want := strings.TrimSuffix(s.Path(), ".jsonl") + ".plan.md"; plan != want {
		t.Fatalf("PlanPath = %q, want %q", plan, want)
	}
	if err := CreatePlanFile(plan); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(plan)
	if err != nil || info.Size() != 0 {
		t.Fatalf("stat = %v, %v; want an empty file", info, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the plan file is %04o, want 0600 like the transcript", info.Mode().Perm())
	}
	if dir, err := os.Stat(filepath.Dir(plan)); err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatalf("the session directory is %v, %v; want 0700", dir, err)
	}
	if err := os.WriteFile(plan, []byte("## The plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CreatePlanFile(plan); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(plan)
	if err != nil || string(got) != "## The plan\n" {
		t.Fatalf("the plan file reads %q (%v); it must never be truncated", got, err)
	}
}

// TestSlugsCollide documents that the rule is lossy: two workspaces can share
// a directory, so H7's resume must also match the header's cwd.
func TestSlugsCollide(t *testing.T) {
	if a, b := Slug("/a/b-c"), Slug("/a/b/c"); a != b {
		t.Fatalf("Slug(/a/b-c) = %q and Slug(/a/b/c) = %q; the rule is documented to collide here", a, b)
	}
}

func TestLongSlugIsTruncatedAndHashed(t *testing.T) {
	long := "/" + strings.Repeat("deep/", 60) + "project"
	got := Slug(long)
	if len(got) > maxSlugBytes {
		t.Fatalf("Slug is %d bytes, want at most %d", len(got), maxSlugBytes)
	}
	if !strings.HasPrefix(got, "--deep-deep-") || !strings.HasSuffix(got, "--") {
		t.Fatalf("Slug(long) = %q: want the \"--…--\" wrapping kept", got)
	}
	suffix := got[len(got)-len("-12345678--"):]
	if hex := suffix[1:9]; suffix[0] != '-' || strings.Trim(hex, "0123456789abcdef") != "" {
		t.Fatalf("Slug(long) = %q: want '-', 8 hex chars and \"--\" at the end", got)
	}
	if Slug(long) != got {
		t.Fatal("Slug is not deterministic")
	}
	// Two long paths that differ only past the cut still differ.
	if other := Slug(long + "2"); other == got {
		t.Fatalf("two long workspaces share the slug %q", got)
	}
	// A path of exactly the limit is left alone.
	exact := "/" + strings.Repeat("x", maxSlugBytes-4)
	if s := Slug(exact); s != "--"+exact[1:]+"--" {
		t.Fatalf("a %d-byte slug was changed to %q", maxSlugBytes, s)
	}
}

// TestLongSlugCutsAtARuneBoundary: macOS refuses a file name that is not
// valid UTF-8, so the cut never splits a character.
func TestLongSlugCutsAtARuneBoundary(t *testing.T) {
	for pad := range 4 {
		long := "/" + strings.Repeat("a", pad) + strings.Repeat("проект/", 40)
		got := Slug(long)
		if !utf8.ValidString(got) {
			t.Fatalf("pad %d: Slug = %q is not valid UTF-8", pad, got)
		}
		if len(got) > maxSlugBytes {
			t.Fatalf("pad %d: Slug is %d bytes", pad, len(got))
		}
	}
}
