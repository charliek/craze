package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/tool"
)

// The file tools refuse the harness's sign-in directory whole (plan 033
// §3.12, A21): <Home>/auth holds the ChatGPT plan's tokens, and a token a read
// put in front of the model would be one the redactor only learns at the next
// turn. The token here is a dummy of at least eight bytes.
const signInToken = "test-signin-token-0010"

// TestFileToolsRefuseTheSignInDirectory: read, write and edit refuse every
// file in <Home>/auth — by place, the directory itself, a symlink to it, its
// files, a file not there yet, another spelling of the directory before it
// exists — and by identity, a hard link to the token file made elsewhere;
// the token never reaches a result. The controls: the model list beside it
// (<Home>/chatgpt-models.json, not secret) and a directory named auth in the
// workspace are read as any file is.
func TestFileToolsRefuseTheSignInDirectory(t *testing.T) {
	t.Run("an existing sign-in", func(t *testing.T) {
		f := newFixture(t)
		auth := filepath.Join(f.env.Home, AuthDir)
		token := filepath.Join(auth, "chatgpt.json")
		put(t, token, `{"access_token":"`+signInToken+`"}`)
		put(t, filepath.Join(auth, "chatgpt-client.json"), `{"plan_usage":true}`)
		if err := os.Link(token, f.path("hard.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(auth, f.path("auth-link")); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{token, auth, filepath.Join(auth, "chatgpt-client.json"), f.path("hard.json"),
			f.path("auth-link/chatgpt.json"), f.path("auth-link")} {
			_, res := f.call(t, "read", map[string]any{"filePath": p})
			failed(t, res, tool.ClassToolError, credentialsText)
		}
		for _, call := range []struct {
			name string
			in   map[string]any
		}{
			{"write", map[string]any{"filePath": filepath.Join(auth, "new.json"), "content": "x"}},
			{"write", map[string]any{"filePath": f.path("hard.json"), "content": "x"}},
			{"edit", map[string]any{"filePath": token, "oldString": signInToken, "newString": "x"}},
		} {
			_, res := f.call(t, call.name, call.in)
			failed(t, res, tool.ClassToolError, credentialsText)
		}
		if b, err := os.ReadFile(token); err != nil || !strings.Contains(string(b), signInToken) {
			t.Fatalf("the token file was changed: %q, %v", b, err)
		}
		if _, err := os.Stat(filepath.Join(auth, "new.json")); !os.IsNotExist(err) {
			t.Fatalf("a file was created in the sign-in directory: %v", err)
		}

		put(t, filepath.Join(f.env.Home, "chatgpt-models.json"), `{"version":1}`)
		put(t, f.path("auth/notes.txt"), "not the sign-in\n")
		for _, p := range []string{filepath.Join(f.env.Home, "chatgpt-models.json"), f.path("auth/notes.txt")} {
			_, res := f.call(t, "read", map[string]any{"filePath": p})
			ok(t, res)
		}
	})
	t.Run("before the sign-in makes it", func(t *testing.T) {
		f := newFixture(t)
		for _, p := range []string{filepath.Join(f.env.Home, AuthDir, "chatgpt.json"), filepath.Join(f.env.Home, "AUTH", "chatgpt.json")} {
			_, res := f.call(t, "write", map[string]any{"filePath": p, "content": "x"})
			failed(t, res, tool.ClassToolError, credentialsText)
		}
		_, res := f.call(t, "write", map[string]any{"filePath": filepath.Join(f.env.Home, "notes.txt"), "content": "x"})
		ok(t, res)
	})
}

// TestSearchToolsRefuseTheSignInDirectory (plan 033 C14r, r12 #7): grep and
// glob keep the file tools' refusal. grep refuses a path that is the key file,
// the sign-in directory or anything in it — directly, through a symlink, or a
// hard link to the token file — with read's words; a search of a directory
// above them, the craze directory itself, finds nothing in them: their lines
// are dropped before they are previewed or counted, so the token file's many
// matching lines neither show nor cut the result short of the control's one;
// and the hard link in the workspace is passed over the same way. glob lists
// none of those files, and a listing of the sign-in directory is empty. The
// controls: the model list beside them (<Home>/chatgpt-models.json, not
// secret) and a note in the workspace, which match the same pattern, are
// found and listed.
func TestSearchToolsRefuseTheSignInDirectory(t *testing.T) {
	requireRG(t)
	f := newFixture(t)
	auth := filepath.Join(f.env.Home, AuthDir)
	token := filepath.Join(auth, "chatgpt.json")
	put(t, token, strings.Repeat(`"access_token":"`+signInToken+"\"\n", searchLimit+20))
	put(t, filepath.Join(auth, "chatgpt-client.json"), `{"note":"test-signin-client"}`)
	keys := filepath.Join(f.env.Home, CredentialsFile)
	put(t, keys, "[providers.p]\napi_key = \"test-signin-stored-key\"\n")
	models := filepath.Join(f.env.Home, "chatgpt-models.json")
	put(t, models, `{"note":"test-signin-control"}`)
	note := f.path("note.txt")
	put(t, note, "test-signin-control\n")
	if err := os.Link(token, f.path("hard.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(auth, f.path("auth-link")); err != nil {
		t.Fatal(err)
	}
	leaks := func(text string) bool {
		return strings.Contains(text, signInToken) || strings.Contains(text, "test-signin-client") ||
			strings.Contains(text, "test-signin-stored-key") || strings.Contains(text, auth) ||
			strings.Contains(text, "hard.json") || strings.Contains(text, CredentialsFile)
	}

	t.Run("grep, a protected path named", func(t *testing.T) {
		for _, p := range []string{token, auth, filepath.Join(auth, "chatgpt-client.json"), keys,
			f.path("hard.json"), f.path("auth-link"), f.path("auth-link/chatgpt.json")} {
			_, res := f.call(t, "grep", map[string]any{"pattern": "test-signin", "path": p})
			failed(t, res, tool.ClassToolError, credentialsText)
		}
	})
	t.Run("grep, from the craze directory", func(t *testing.T) {
		text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": "test-signin", "path": f.env.Home}))
		if want := "Found 1 matches\n" + models + ":\n  Line 1: " + `{"note":"test-signin-control"}`; text != want || leaks(text) {
			t.Fatalf("grep = %q\nwant the model list's line alone", text)
		}
	})
	t.Run("grep, from the workspace", func(t *testing.T) {
		text := ok(t, callOK(t, f, "grep", map[string]any{"pattern": "test-signin"}))
		if want := "Found 1 matches\n" + note + ":\n  Line 1: test-signin-control"; text != want || leaks(text) {
			t.Fatalf("grep = %q\nwant the note's line alone", text)
		}
	})
	t.Run("glob", func(t *testing.T) {
		text := ok(t, callOK(t, f, "glob", map[string]any{"pattern": "*", "path": f.env.Home}))
		if text != models || leaks(text) {
			t.Fatalf("glob of the craze directory = %q, want the model list alone", text)
		}
		text = ok(t, callOK(t, f, "glob", map[string]any{"pattern": "*.json"}))
		if text != "No files found" || leaks(text) {
			t.Fatalf("glob of the workspace = %q, want no hard link listed", text)
		}
		if text := ok(t, callOK(t, f, "glob", map[string]any{"pattern": "*", "path": auth})); text != "No files found" {
			t.Fatalf("glob of the sign-in directory = %q", text)
		}
		if text := ok(t, callOK(t, f, "glob", map[string]any{"pattern": "*.txt"})); text != note {
			t.Fatalf("control: glob of the workspace's notes = %q", text)
		}
	})
}
