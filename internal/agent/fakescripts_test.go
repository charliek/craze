package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// The package's fake executables are written once, by TestMain before any
// test runs, and never written again: a script written while another
// goroutine forks can fail to exec with ETXTBSY — the forked child holds the
// file open for writing until it execs (go.dev/issue/22315) — and this
// package's fake-agent tests fork constantly. A test puts a symlink to one of
// them where it wants it (a symlink is never open for writing) and says what
// the fake does in data files beside the link, which the script reads by its
// own $0.
var fakeScriptDir string

// fakeScripts are the generic fakes, by name.
var fakeScripts = map[string]string{
	// claude: `auth status` records its argv and directory in auth-args,
	// sleeps for the seconds in sleep, starts a background sleep whose pid
	// goes to the file bgpid names, when those are there, then prints status;
	// `--version` prints version. All of them beside the link.
	"claude": `#!/bin/sh
d=$(dirname "$0")
case "$*" in
  *'auth status'*)
    { for a in "$@"; do printf '[%s]' "$a"; done; echo; pwd; } > "$d/auth-args"
    if [ -f "$d/sleep" ]; then sleep "$(cat "$d/sleep")"; fi
    if [ -f "$d/bgpid" ]; then sleep 30 & echo $! > "$(cat "$d/bgpid")"; fi
    cat "$d/status" ;;
  *--version*) cat "$d/version" ;;
esac
`,
	// mark: touches the file <link>.mark names, then fails — a decoy agent,
	// a repository hook.
	"mark": `#!/bin/sh
: > "$(cat "$0.mark")"
exit 1
`,
	// sleeper: starts a background sleep, appends its pid to the file
	// <link>.pids names, and waits — a git that never answers.
	"sleeper": `#!/bin/sh
sleep 30 &
echo $! >> "$(cat "$0.pids")"
wait
`,
}

// installFakeScripts writes fakeScripts into a directory of their own.
func installFakeScripts() error {
	dir, err := os.MkdirTemp("", "craze-agent-fakes-")
	if err != nil {
		return err
	}
	for name, body := range fakeScripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			return err
		}
	}
	fakeScriptDir = dir
	return nil
}

// linkFakeScript puts the fake name at path, a symlink, with data: each
// value written to the file its key names, relative to path's directory
// (".mark" and the like are appended to path itself: a key starting with a
// dot is a sidecar of the link).
func linkFakeScript(t *testing.T, name, path string, data map[string]string) {
	t.Helper()
	if fakeScriptDir == "" {
		t.Fatal("the fake scripts are TestMain's: none installed")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fakeScriptDir, name), path); err != nil {
		t.Fatal(err)
	}
	for k, v := range data {
		file := filepath.Join(filepath.Dir(path), k)
		if k != "" && k[0] == '.' {
			file = path + k
		}
		if err := os.WriteFile(file, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
