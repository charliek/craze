package websearch

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/charliek/craze/internal/harness/tool/opencode"
)

// The remembered choice (plan 051 §C.2): websearch.json read leniently and
// written whole.

// TestReadChoice: a missing file is nothing remembered and silent; the two
// shapes craze writes are an allow through exa and a disable; anything else —
// a file that is not JSON, a version this craze does not read, a provider it
// does not have, a file that is no regular file or is too large — is nothing
// remembered with a line that names the file and none of its content.
func TestReadChoice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, StateFile)
	if c, why := ReadChoice(path); c != (Choice{}) || why != "" {
		t.Fatalf("no file: %+v, %q", c, why)
	}
	for body, want := range map[string]Choice{
		`{"version":1,"provider":"exa"}`:                   {Provider: "exa"},
		`{"version":1,"provider":false}`:                   {Disabled: true},
		"{\"provider\": false, \"version\": 1}\n":          {Disabled: true},
		`{"version":1,"provider":"exa","later":"ignored"}`: {Provider: "exa"},
	} {
		writeFile(t, path, body)
		if c, why := ReadChoice(path); c != want || why != "" {
			t.Errorf("%s: %+v, %q; want %+v", body, c, why, want)
		}
	}
	const secret = "sk-secret-value-0123456789"
	for body, want := range map[string]string{
		`not json ` + secret:               "is not valid JSON",
		``:                                 "is not valid JSON",
		`{"version":2,"provider":"exa"}`:   "has a version this craze does not read",
		`{"provider":"exa"}`:               "has a version this craze does not read",
		`{"version":"1","provider":"exa"}`: "is not valid JSON",
		`{"version":1,"provider":"` + secret + `"}`: "names no web search provider",
		`{"version":1,"provider":true}`:             "names no web search provider",
		`{"version":1,"provider":null}`:             "names no web search provider",
		`{"version":1,"provider":"EXA"}`:            "names no web search provider",
		`{"version":1}`:                             "names no web search provider",
		`{"version":1,"provider":0}`:                "names no web search provider",
		`[1]`:                                       "is not valid JSON",
		`{"version":1,"provider":"exa"}` + strings.Repeat(" ", stateMaxBytes): "could not be read",
	} {
		writeFile(t, path, body)
		c, why := ReadChoice(path)
		if c != (Choice{}) || !strings.Contains(why, want) || !strings.HasPrefix(why, StateFile+" ") || !strings.HasSuffix(why, "so no web search choice is remembered") {
			t.Errorf("%.40q: %+v, %q; want nothing remembered and a line saying it %s", body, c, why, want)
		}
		if strings.Contains(why, secret) || strings.Contains(why, dir) {
			t.Errorf("%.40q: the line %q carries the file's content or path", body, why)
		}
	}
	// Not a regular file: a directory, and a FIFO, which must not block.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if c, why := ReadChoice(path); c != (Choice{}) || !strings.Contains(why, "could not be read") {
		t.Errorf("a FIFO: %+v, %q", c, why)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if c, why := ReadChoice(path); c != (Choice{}) || !strings.Contains(why, "could not be read") {
		t.Errorf("a directory: %+v, %q", c, why)
	}
}

// TestWriteChoice: an allow and a disable are written as the two pinned
// shapes, at 0600 in a directory made 0700, and read back; a choice that
// remembers nothing, or a provider the table does not have, writes nothing.
func TestWriteChoice(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "native")
	path := filepath.Join(dir, StateFile)
	for _, bad := range []Choice{{}, {Provider: "bing"}, {Provider: "exa", Disabled: true}} {
		if err := WriteChoice(path, bad); err == nil {
			t.Errorf("WriteChoice(%+v) wrote", bad)
		}
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("a refused choice made the directory: %v", err)
	}
	for c, want := range map[Choice]string{
		{Provider: "exa"}: `{"version":1,"provider":"exa"}` + "\n",
		{Disabled: true}:  `{"version":1,"provider":false}` + "\n",
	} {
		if err := WriteChoice(path, c); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil || string(b) != want {
			t.Fatalf("%+v wrote %q, %v; want %q", c, b, err, want)
		}
		if got, why := ReadChoice(path); got != c || why != "" {
			t.Fatalf("%+v read back as %+v, %q", c, got, why)
		}
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v, %v", fi.Mode(), err)
		}
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %v, %v", fi.Mode(), err)
	}
}

// TestTheFileToolsRefuseTheStateFile: the file tools keep their own copy of
// the remembered choice's name (the tool package may not import this one);
// this pins the two together.
func TestTheFileToolsRefuseTheStateFile(t *testing.T) {
	if opencode.WebSearchStateFile != StateFile {
		t.Fatalf("the file tools refuse %q, but the remembered choice lives in %q", opencode.WebSearchStateFile, StateFile)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
