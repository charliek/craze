package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/tool"
)

// grep's and glob's tests come in two kinds. Those that search real files run
// the real rg, through a session's dispatcher, and need it on PATH:
// requireRG skips them without it, or fails them when CRAZE_REQUIRE_RG=1, as
// CI sets it. Those that pin how rg is run — its argv, its exit codes, its
// lifetime — run a fake rg, a shell script TestMain writes, so they need no
// ripgrep and nothing about them depends on its version. Their sleeps use
// distinct durations (621, 622, ...) as markers, as bash's tests do.

// fakeRGDir holds the fake rg scripts: <fakeRGDir>/<name>/rg.
var fakeRGDir string

// fakeRGScripts are the fake rgs, by name. Each takes its parameters from
// its environment, which a test sets through Env.Environ.
var fakeRGScripts = map[string]string{
	// argv records how it was run — its working directory, then each
	// argument on a line of its own — in FAKE_RG_LOG, and finds nothing. It
	// uses shell builtins only, so it runs with any PATH.
	"argv": `{ pwd; for a in "$@"; do printf '%s\n' "$a"; done; } > "$FAKE_RG_LOG"
exit 1
`,
	// exit writes FAKE_RG_OUT to stdout — with each "\n" a NUL when
	// FAKE_RG_NUL is set, as `rg --files --null` ends each path, since an
	// environment variable cannot hold a NUL — and FAKE_RG_ERR to stderr,
	// and exits with FAKE_RG_CODE.
	"exit": `if [ -n "$FAKE_RG_NUL" ]; then
	printf '%s' "$FAKE_RG_OUT" | tr '\n' '\000'
else
	printf '%s' "$FAKE_RG_OUT"
fi
printf '%s' "$FAKE_RG_ERR" >&2
exit "$FAKE_RG_CODE"
`,
	// slow writes FAKE_RG_OUT, then waits for a background sleep of
	// FAKE_RG_SLEEP seconds, whose pid it writes to FAKE_RG_PID.
	"slow": `printf '%s' "$FAKE_RG_OUT"
sleep "$FAKE_RG_SLEEP" &
echo $! > "$FAKE_RG_PID"
wait
`,
	// stubborn is slow, ignoring SIGTERM, as its sleep does too.
	"stubborn": `trap '' TERM
sleep "$FAKE_RG_SLEEP" &
echo $! > "$FAKE_RG_PID"
wait
`,
	// flood starts a sleep as slow does, prints FAKE_RG_COUNT matches in
	// ./f.txt, and waits for the sleep.
	"flood": `sleep "$FAKE_RG_SLEEP" &
echo $! > "$FAKE_RG_PID"
i=1
while [ "$i" -le "$FAKE_RG_COUNT" ]; do
	printf '{"type":"match","data":{"path":{"text":"./f.txt"},"lines":{"text":"needle %d\\n"},"line_number":%d}}\n' "$i" "$i"
	i=$((i + 1))
done
wait
`,
}

// TestMain writes the fake rgs before any test runs. A script written while
// another test's goroutine forks can fail to run with ETXTBSY: the forked
// child holds the file open for writing until it execs (go.dev/issue/22315).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "craze-fake-rg-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for name, body := range fakeRGScripts {
		if err = os.Mkdir(filepath.Join(dir, name), 0o755); err == nil {
			err = os.WriteFile(filepath.Join(dir, name, "rg"), []byte("#!/bin/sh\n"+body), 0o755)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			_ = os.RemoveAll(dir)
			os.Exit(1)
		}
	}
	fakeRGDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// tb is what checkRG needs of a test.
type tb interface {
	Helper()
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// requireRG skips the test when rg is not on PATH — unless CRAZE_REQUIRE_RG
// is 1, as CI sets it, when it fails the test instead: a runner that lost
// ripgrep must fail, not pass by skipping (plan 019 §3.9).
func requireRG(t *testing.T) {
	t.Helper()
	checkRG(t, exec.LookPath, os.Getenv)
}

func checkRG(t tb, look func(string) (string, error), getenv func(string) string) {
	t.Helper()
	_, err := look("rg")
	switch {
	case err == nil:
	case getenv("CRAZE_REQUIRE_RG") == "1":
		t.Fatalf("CRAZE_REQUIRE_RG=1, and ripgrep (rg) is not on PATH: %v", err)
	default:
		t.Skipf("ripgrep (rg) is not on PATH (set CRAZE_REQUIRE_RG=1 to fail instead): %v", err)
	}
}

// verdict records what checkRG did.
type verdict struct{ skipped, failed bool }

func (v *verdict) Helper()               {}
func (v *verdict) Skipf(string, ...any)  { v.skipped = true }
func (v *verdict) Fatalf(string, ...any) { v.failed = true }
func lookFor(err error) func(string) (string, error) {
	return func(string) (string, error) { return "/usr/bin/rg", err }
}

// TestRequireRG: with CRAZE_REQUIRE_RG=1 a missing rg fails the test rather
// than skipping it; without it, it skips; and rg on PATH does neither
// either way — the controls.
func TestRequireRG(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		missing bool
		require string
		want    verdict
	}{
		{"missing, required", true, "1", verdict{failed: true}},
		{"missing, not required", true, "", verdict{skipped: true}},
		{"missing, required by another value", true, "yes", verdict{skipped: true}},
		{"present, required", false, "1", verdict{}},
		{"present, not required", false, "", verdict{}},
	} {
		var err error
		if tc.missing {
			err = exec.ErrNotFound
		}
		var v verdict
		checkRG(&v, lookFor(err), func(name string) string {
			if name == "CRAZE_REQUIRE_RG" {
				return tc.require
			}
			return ""
		})
		if v != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, v, tc.want)
		}
	}
}

// fakeRG is a ripgrep that runs the fake rg called name, with timeout.
func fakeRG(name string, timeout time.Duration) *ripgrep {
	bin := filepath.Join(fakeRGDir, name, "rg")
	return &ripgrep{look: func(string) (string, error) { return bin, nil }, timeout: timeout}
}

// fakeEnv is a session's Env for calling grep or glob directly, whose
// commands get vars on top of this process's environment.
func fakeEnv(t *testing.T, vars ...string) tool.Env {
	t.Helper()
	return tool.Env{Workspace: t.TempDir(), Home: t.TempDir(), Environ: append(os.Environ(), vars...)}
}

var searchIDs atomic.Int64

// prepareSearch prepares a call of the grep or glob tool built on rg, with
// in as its arguments: a value to marshal, or a string of raw JSON.
func prepareSearch(t *testing.T, rg *ripgrep, env tool.Env, name string, in any) tool.Prepared {
	t.Helper()
	build := map[string]func(*ripgrep) (tool.Tool, error){"grep": newGrep, "glob": newGlob}[name]
	tl, err := build(rg)
	if err != nil {
		t.Fatal(err)
	}
	raw, isRaw := in.(string)
	if !isRaw {
		j, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(j)
	}
	p, err := tl.Prepare(env, tool.Call{ID: fmt.Sprintf("t1.1.%d", searchIDs.Add(1)), Tool: name, Input: json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// startSearch runs p on a goroutine of its own. When the test ends it
// closes the call, as a session would, and waits for it.
func startSearch(t *testing.T, p tool.Prepared, env tool.Env) *bashRun {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	r := &bashRun{cancel: cancel, res: make(chan tool.Result, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.res <- p.Run(ctx, env)
	}()
	t.Cleanup(func() {
		cancel(tool.ErrClosing)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after the call was closed")
		}
	})
	return r
}

// runSearch makes one call of the grep or glob tool on rg and returns its
// result.
func runSearch(t *testing.T, rg *ripgrep, env tool.Env, name string, in any) tool.Result {
	t.Helper()
	return startSearch(t, prepareSearch(t, rg, env, name, in), env).await(t, 30*time.Second)
}

// TestSearchArgv pins how each tool runs rg — the argv, flag for flag, and
// the directory — with a fake rg that records it (ripgrep.ts:154-231, plan
// 019 §3.2): --no-config for both; --json, --hidden and --no-messages for
// grep only; --null for glob only, so each path ends with a NUL, not a
// newline a name can hold; the include glob before .git's exclusion; "--"
// before grep's pattern, so a pattern like "-v" is a pattern; a file
// searched from its directory, by name. Each case is the others' control.
func TestSearchArgv(t *testing.T) {
	t.Parallel()
	log := filepath.Join(t.TempDir(), "argv")
	env := fakeEnv(t, "FAKE_RG_LOG="+log)
	sub := filepath.Join(env.Workspace, "sub")
	put(t, filepath.Join(sub, "a.txt"), "x\n")
	grep := []string{"--no-config", "--json", "--hidden", "--no-messages"}
	for _, tc := range []struct {
		name, tool string
		in         map[string]any
		dir        string
		argv       []string
	}{
		{"grep", "grep", map[string]any{"pattern": "TODO"}, env.Workspace,
			append(slices.Clone(grep), "--glob=!**/.git/**", "--", "TODO", ".")},
		{"grep with include and a path", "grep", map[string]any{"pattern": "TODO", "path": "sub", "include": "*.{go,ts}"}, sub,
			append(slices.Clone(grep), "--glob=*.{go,ts}", "--glob=!**/.git/**", "--", "TODO", ".")},
		{"grep with an empty include", "grep", map[string]any{"pattern": "TODO", "include": ""}, env.Workspace,
			append(slices.Clone(grep), "--glob=!**/.git/**", "--", "TODO", ".")},
		{"grep of a file", "grep", map[string]any{"pattern": "TODO", "path": "sub/a.txt"}, sub,
			append(slices.Clone(grep), "--glob=!**/.git/**", "--", "TODO", "a.txt")},
		{"grep for a flag", "grep", map[string]any{"pattern": "-v"}, env.Workspace,
			append(slices.Clone(grep), "--glob=!**/.git/**", "--", "-v", ".")},
		{"glob", "glob", map[string]any{"pattern": "**/*.go"}, env.Workspace,
			[]string{"--no-config", "--files", "--null", "--glob=**/*.go", "--glob=!**/.git/**", "."}},
		{"glob with a path", "glob", map[string]any{"pattern": "*.txt", "path": sub}, sub,
			[]string{"--no-config", "--files", "--null", "--glob=*.txt", "--glob=!**/.git/**", "."}},
	} {
		// One at a time: they share the log.
		res := runSearch(t, fakeRG("argv", time.Minute), env, tc.tool, tc.in)
		if res.IsError || res.Text != "No files found" {
			t.Fatalf("%s: result = %+v, want no match (exit 1 is not an error)", tc.name, res)
		}
		got := strings.Split(strings.TrimSuffix(load(t, log), "\n"), "\n")
		dir, _ := filepath.EvalSymlinks(got[0])
		want, _ := filepath.EvalSymlinks(tc.dir)
		if dir != want || !slices.Equal(got[1:], tc.argv) {
			t.Errorf("%s: rg ran in %s with %q\nwant it in %s with %q", tc.name, got[0], got[1:], tc.dir, tc.argv)
		}
	}
}

// TestSearchMissingRG: with no rg, both tools return the pinned error — an
// injected lookup here, the real one on an empty PATH in
// TestSearchFindsRGOnPath — and look once, however many calls. The control
// is the same calls with an rg to find.
func TestSearchMissingRG(t *testing.T) {
	t.Parallel()
	var looks atomic.Int64
	missing := &ripgrep{timeout: time.Minute, look: func(string) (string, error) {
		looks.Add(1)
		return "", exec.ErrNotFound
	}}
	env := fakeEnv(t, "FAKE_RG_LOG="+filepath.Join(t.TempDir(), "argv"))
	for _, name := range []string{"grep", "glob", "grep"} {
		res := runSearch(t, missing, env, name, map[string]any{"pattern": "x"})
		failed(t, res, tool.ClassToolError, "ripgrep (rg) is not installed or not on PATH. Use the bash tool with grep or find instead.")
	}
	if n := looks.Load(); n != 1 {
		t.Fatalf("rg was looked for %d times in one session, want once", n)
	}
	for _, name := range []string{"grep", "glob"} {
		if res := runSearch(t, fakeRG("argv", time.Minute), env, name, map[string]any{"pattern": "x"}); res.IsError {
			t.Fatalf("control: %s with rg to find: %+v", name, res)
		}
	}
}

// TestSearchFindsRGOnPath: the profile finds rg on PATH, once per session —
// a session that found it keeps it when PATH loses it — and a session
// started with no rg on PATH gets the pinned error from both tools.
func TestSearchFindsRGOnPath(t *testing.T) {
	// Not parallel: it sets PATH.
	log := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_RG_LOG", log)
	t.Setenv("PATH", filepath.Join(fakeRGDir, "argv"))
	found := newFixture(t)
	if res := ok(t, callOK(t, found, "grep", map[string]any{"pattern": "on-path"})); res != "No files found" {
		t.Fatalf("grep = %q", res)
	}
	if !strings.Contains(load(t, log), "\non-path\n") {
		t.Fatalf("the rg on PATH did not run: %q", load(t, log))
	}

	t.Setenv("PATH", t.TempDir())
	if _, res := found.call(t, "glob", map[string]any{"pattern": "*"}); res.IsError {
		t.Fatalf("a session that found rg lost it with PATH: %+v", res)
	}
	lost := newFixture(t)
	for _, name := range []string{"grep", "glob"} {
		_, res := lost.call(t, name, map[string]any{"pattern": "x"})
		failed(t, res, tool.ClassToolError, rgMissingText)
	}
}

// callOK makes a call through f and returns its result.
func callOK(t *testing.T, f *fixture, name string, in any) tool.Result {
	t.Helper()
	_, res := f.call(t, name, in)
	return res
}

// jsonMatch is one line of `rg --json` for a match of text in ./f.txt.
func jsonMatch(line int, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "match", "data": map[string]any{
		"path": map[string]any{"text": "./f.txt"}, "lines": map[string]any{"text": text + "\n"}, "line_number": line,
	}})
	return string(b) + "\n"
}

// TestSearchExitCodes pins how rg's exit and output become the result, as
// opencode's ripgrep.ts:130-141 has it: 0 and 1 are answers, 1 with no
// rows; 2 keeps its rows unless rg says the pattern is invalid, when rg's
// message is the error; anything else is an error with rg's stderr, or
// opencode's words when it wrote none. A record that is not JSON, or a
// match without a line number, fails the search; a record that is not a
// match is skipped. A path that would lie outside the directory searched —
// which rg never prints — fails the search as invalid output (rgPath).
// glob's fake output ends each path with a NUL, as `--files --null` does.
func TestSearchExitCodes(t *testing.T) {
	t.Parallel()
	begin := `{"type":"begin","data":{"path":{"text":"./f.txt"}}}` + "\n"
	one := begin + jsonMatch(3, "a needle")
	outside := func(p string) string {
		b, _ := json.Marshal(map[string]any{"type": "match", "data": map[string]any{
			"path": map[string]any{"text": p}, "lines": map[string]any{"text": "a needle\n"}, "line_number": 1,
		}})
		return string(b) + "\n"
	}
	for _, tc := range []struct {
		name, tool, out, stderr string
		code                    int
		text                    string // "" when the result is text's error
		errText                 string
	}{
		{name: "grep, found", tool: "grep", out: one, code: 0, text: "Found 1 matches\n{ws}/f.txt:\n  Line 3: a needle"},
		{name: "grep, none", tool: "grep", code: 1, text: "No files found"},
		{name: "grep, partial", tool: "grep", out: one, stderr: "rg: ./locked: Permission denied (os error 13)\n", code: 2,
			text: "Found 1 matches\n{ws}/f.txt:\n  Line 3: a needle"},
		{name: "grep, invalid regex", tool: "grep", stderr: "rg: regex parse error:\n    (?:a()\n    ^\nerror: unclosed group\n", code: 2,
			errText: "rg: regex parse error:\n    (?:a()\n    ^\nerror: unclosed group"},
		{name: "grep, invalid regex (older words)", tool: "grep", stderr: "error parsing regex\n", code: 2, errText: "error parsing regex"},
		{name: "grep, invalid include", tool: "grep", stderr: "rg: error parsing glob '*.{ts': unclosed alternate group\n", code: 2,
			errText: "rg: error parsing glob '*.{ts': unclosed alternate group"},
		{name: "grep, another failure", tool: "grep", out: one, stderr: "rg: something broke\n", code: 3, errText: "rg: something broke"},
		{name: "grep, a silent failure", tool: "grep", code: 101, errText: "ripgrep failed with code 101"},
		{name: "grep, not JSON", tool: "grep", out: one + "{\"type\":\n", code: 0, errText: "Invalid ripgrep JSON output"},
		{name: "grep, no line number", tool: "grep", out: `{"type":"match","data":{"path":{"text":"f"},"lines":{"text":"x"}}}` + "\n",
			errText: "Invalid ripgrep match output"},
		{name: "grep, a zero line number", tool: "grep", out: `{"type":"match","data":{"path":{"text":"f"},"lines":{"text":"x"},"line_number":0}}` + "\n",
			errText: "Invalid ripgrep match output"},
		{name: "grep, no lines", tool: "grep", out: `{"type":"match","data":{"path":{"text":"f"},"line_number":1}}` + "\n",
			errText: "Invalid ripgrep match output"},
		{name: "grep, other records and blank lines skipped", tool: "grep", out: "\n[1]\n" + `{"type":5}` + "\n" + one + "\n",
			text: "Found 1 matches\n{ws}/f.txt:\n  Line 3: a needle"},
		{name: "grep, a path above the root", tool: "grep", out: outside("../secret"),
			errText: `Invalid ripgrep output: "../secret" is not a path under {ws}`},
		{name: "grep, a path that climbs out", tool: "grep", out: outside("./a/../../secret"),
			errText: `Invalid ripgrep output: "./a/../../secret" is not a path under {ws}`},
		{name: "grep, the root itself", tool: "grep", out: outside("."), errText: `Invalid ripgrep output: "." is not a path under {ws}`},
		{name: "grep, an absolute path stays under the root", tool: "grep", out: outside("/etc/f.txt"),
			text: "Found 1 matches\n{ws}/etc/f.txt:\n  Line 1: a needle"},
		{name: "glob, found", tool: "glob", out: "./a.go\nsub/b.go\n", code: 0, text: "{ws}/a.go\n{ws}/sub/b.go"},
		{name: "glob, a last path with no NUL", tool: "glob", out: "./a.go\n./b.go", code: 0, text: "{ws}/a.go\n{ws}/b.go"},
		{name: "glob, a path above the root", tool: "glob", out: "./a.go\n../secret\n", code: 0,
			errText: `Invalid ripgrep output: "../secret" is not a path under {ws}`},
		{name: "glob, none", tool: "glob", code: 1, text: "No files found"},
		{name: "glob, partial", tool: "glob", out: "./a.go\n", stderr: "rg: ./locked: Permission denied\n", code: 2, text: "{ws}/a.go"},
		{name: "glob, invalid glob", tool: "glob", stderr: "rg: error parsing glob '*.{ts': unclosed alternate group\n", code: 2,
			errText: "rg: error parsing glob '*.{ts': unclosed alternate group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := fakeEnv(t, "FAKE_RG_OUT="+tc.out, "FAKE_RG_ERR="+tc.stderr, fmt.Sprintf("FAKE_RG_CODE=%d", tc.code))
			if tc.tool == "glob" {
				env.Environ = append(env.Environ, "FAKE_RG_NUL=1")
			}
			res := runSearch(t, fakeRG("exit", time.Minute), env, tc.tool, map[string]any{"pattern": "x"})
			if tc.errText != "" {
				failed(t, res, tool.ClassToolError, strings.ReplaceAll(tc.errText, "{ws}", env.Workspace))
				return
			}
			if want := strings.ReplaceAll(tc.text, "{ws}", env.Workspace); res.IsError || res.Text != want {
				t.Fatalf("result = {IsError:%v Class:%q Text:%q}, want {Text:%q}", res.IsError, res.Class, res.Text, want)
			}
		})
	}
}

// TestRGPath: a path rg printed joins onto the root it searched when it
// stays strictly under it, a name like "x\n.." being one element; one that
// is the root, or climbs above it, is invalid output. The accepted cases are
// the refusals' controls.
func TestRGPath(t *testing.T) {
	t.Parallel()
	const root = "/w/s"
	for _, tc := range []struct{ printed, want string }{
		{"./a.go", "/w/s/a.go"},
		{"a.go", "/w/s/a.go"},
		{"././sub/./b", "/w/s/sub/b"},
		{"/etc/passwd", "/w/s/etc/passwd"}, // leading "/" dropped, as opencode drops it
		{"x\n../secret", "/w/s/x\n../secret"},
		{"..x/y", "/w/s/..x/y"},
		{"a/../b", "/w/s/b"},
		{"../secret", ""},
		{"a/../../secret", ""},
		{"..", ""},
		{".", ""},
		{"", ""},
	} {
		got, err := rgPath(root, tc.printed)
		switch {
		case tc.want == "" && err == nil:
			t.Errorf("rgPath(%q) = %q, want it refused", tc.printed, got)
		case tc.want == "" && err.Error() != fmt.Sprintf("Invalid ripgrep output: %q is not a path under %s", tc.printed, root):
			t.Errorf("rgPath(%q): %v", tc.printed, err)
		case tc.want != "" && (err != nil || got != tc.want):
			t.Errorf("rgPath(%q) = %q, %v; want %q", tc.printed, got, err, tc.want)
		}
	}
}

// TestShownPath: a path with a control character in it is Go-quoted, so it
// is one line; any other, however unusual, is shown as it is.
func TestShownPath(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"/w/a.go":            "/w/a.go",
		"/w/with space/ä.go": "/w/with space/ä.go",
		`/w/back\slash"q`:    `/w/back\slash"q`,
		"/w/x\n../secret":    `"/w/x\n../secret"`,
		"/w/tab\there":       `"/w/tab\there"`,
		"/w/cr\r":            `"/w/cr\r"`,
		"/w/del\x7f":         `"/w/del\x7f"`,
	} {
		if got := shownPath(in); got != want {
			t.Errorf("shownPath(%q) = %s, want %s", in, got, want)
		}
	}
}

// awaitPID waits for a fake rg to write its sleep's pid, and checks that the
// sleep runs: the control for every kill that follows.
func awaitPID(t *testing.T, env tool.Env, marker string) int {
	t.Helper()
	pid := bashPID(t, filepath.Join(env.Home, "pid"), marker)
	if !alive(pid, marker) {
		t.Fatalf("control: %s is not running while the search runs", marker)
	}
	return pid
}

// sleepEnv is fakeEnv for a fake rg that sleeps secs seconds and writes the
// sleep's pid to <Home>/pid.
func sleepEnv(t *testing.T, secs int, vars ...string) tool.Env {
	t.Helper()
	env := fakeEnv(t)
	env.Environ = append(env.Environ, append([]string{fmt.Sprintf("FAKE_RG_SLEEP=%d", secs), "FAKE_RG_PID=" + filepath.Join(env.Home, "pid")}, vars...)...)
	return env
}

// TestSearchTimeout: rg is stopped after the search's timeout — its whole
// group, the fake's background sleep included — and the result says so,
// class timeout, even though the kill cut rg's last line short: a timeout is
// not a JSON error. Run returns within the timeout and the SIGTERM grace.
func TestSearchTimeout(t *testing.T) {
	t.Parallel()
	for i, name := range []string{"grep", "glob"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			marker := fmt.Sprintf("sleep %d", 621+i)
			env := sleepEnv(t, 621+i, `FAKE_RG_OUT={"type":"ma`)
			const timeout = 2 * time.Second
			began := time.Now()
			r := startSearch(t, prepareSearch(t, fakeRG("slow", timeout), env, name, map[string]any{"pattern": "x"}), env)
			pid := awaitPID(t, env, marker)
			res := r.await(t, 30*time.Second)
			took := time.Since(began)
			failed(t, res, tool.ClassTimeout, name+" tool terminated ripgrep after exceeding timeout 2000 ms. Consider using a more specific path or pattern.")
			gone(t, pid, marker)
			if took < timeout || took > timeout+planGrace {
				t.Fatalf("a %v timeout returned after %v", timeout, took)
			}
		})
	}
}

// TestSearchCancel: a cancel ends rg's whole group at once — rg dies on
// SIGTERM — and the result is opencode's aborted text.
func TestSearchCancel(t *testing.T) {
	t.Parallel()
	for i, name := range []string{"grep", "glob"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			marker := fmt.Sprintf("sleep %d", 623+i)
			env := sleepEnv(t, 623+i)
			r := startSearch(t, prepareSearch(t, fakeRG("slow", time.Minute), env, name, map[string]any{"pattern": "x"}), env)
			pid := awaitPID(t, env, marker)
			cancelled := time.Now()
			r.cancel(nil)
			res := r.await(t, 30*time.Second)
			if took := time.Since(cancelled); took > planGrace {
				t.Fatalf("Run took %v after a cancel", took)
			}
			failed(t, res, tool.ClassAborted, tool.AbortedText)
			gone(t, pid, marker)
		})
	}
}

// TestSearchCloseKillsAtOnce: when the session closes, an rg that ignores
// SIGTERM is killed at once, with its group. The control is an ordinary
// cancel of the same rg, which waits out the 3 s grace before SIGKILL.
func TestSearchCloseKillsAtOnce(t *testing.T) {
	t.Parallel()
	for i, closing := range []bool{true, false} {
		t.Run(fmt.Sprintf("closing=%v", closing), func(t *testing.T) {
			t.Parallel()
			marker := fmt.Sprintf("sleep %d", 625+i)
			env := sleepEnv(t, 625+i)
			r := startSearch(t, prepareSearch(t, fakeRG("stubborn", time.Minute), env, "grep", map[string]any{"pattern": "x"}), env)
			pid := awaitPID(t, env, marker)
			cancelled := time.Now()
			var cause error
			if closing {
				cause = fmt.Errorf("session 1: %w", tool.ErrClosing)
			}
			r.cancel(cause)
			res := r.await(t, 30*time.Second)
			took := time.Since(cancelled)
			failed(t, res, tool.ClassAborted, tool.AbortedText)
			gone(t, pid, marker)
			switch {
			case closing && took >= planGrace-500*time.Millisecond:
				t.Fatalf("Run took %v after a close; want it well inside the %v grace", took, planGrace)
			case !closing && took < planGrace:
				t.Fatalf("control: an ordinary cancel of an rg that ignores SIGTERM took %v, less than the %v grace", took, planGrace)
			}
		})
	}
}

// TestSearchStopsAtTheCap: once rg has printed one match more than grep
// shows, grep stops reading and ends rg's group, as opencode ends rg once it
// has taken limit+1 rows, and returns the capped answer without waiting for
// rg. The control is an rg that prints fewer: grep reads on until rg ends,
// here at the timeout.
func TestSearchStopsAtTheCap(t *testing.T) {
	t.Parallel()
	var want strings.Builder
	want.WriteString("Found 100 matches (more matches available)\n{ws}/f.txt:")
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&want, "\n  Line %d: needle %d", i, i)
	}
	want.WriteString("\n\n(Results truncated. Consider using a more specific path or pattern.)")

	t.Run("more than the cap", func(t *testing.T) {
		t.Parallel()
		env := sleepEnv(t, 627, "FAKE_RG_COUNT=150")
		began := time.Now()
		r := startSearch(t, prepareSearch(t, fakeRG("flood", time.Minute), env, "grep", map[string]any{"pattern": "needle"}), env)
		res := r.await(t, 30*time.Second)
		if took := time.Since(began); took > planGrace {
			t.Fatalf("grep took %v; it should stop rg once it has enough", took)
		}
		if w := strings.ReplaceAll(want.String(), "{ws}", env.Workspace); res.IsError || res.Text != w {
			t.Fatalf("result = {IsError:%v Class:%q Text:%q}\nwant %q", res.IsError, res.Class, res.Text, w)
		}
		gone(t, bashPID(t, filepath.Join(env.Home, "pid"), "sleep 627"), "sleep 627")
	})
	t.Run("control: fewer", func(t *testing.T) {
		t.Parallel()
		env := sleepEnv(t, 628, "FAKE_RG_COUNT=50")
		r := startSearch(t, prepareSearch(t, fakeRG("flood", 2*time.Second), env, "grep", map[string]any{"pattern": "needle"}), env)
		pid := awaitPID(t, env, "sleep 628")
		if res := r.await(t, 30*time.Second); res.Class != tool.ClassTimeout {
			t.Fatalf("result = %+v, want the timeout: grep should have read on", res)
		}
		gone(t, pid, "sleep 628")
	})
}

// TestSearchParameters ports opencode's parameters.test.ts cases for grep
// and glob, and adds what their schemas refuse and Fantasy would not: a
// value of the wrong JSON type, null. grep refuses an empty pattern before
// it runs (NOTICE); glob passes one on. Only Prepare runs, so no rg is
// needed. Each acceptance is the refusals' control.
func TestSearchParameters(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, tc := range []struct {
		name, tool, input string
		field             string // "" when the input is accepted
	}{
		{"glob accepts pattern-only", "glob", `{"pattern":"**/*.ts"}`, ""},
		{"glob accepts optional path", "glob", `{"pattern":"**/*.ts","path":"/tmp"}`, ""},
		{"glob accepts an empty pattern", "glob", `{"pattern":""}`, ""},
		{"glob ignores an unknown field", "glob", `{"pattern":"*","extra":1}`, ""},
		{"glob rejects missing pattern", "glob", `{}`, "pattern"},
		{"glob rejects a numeric pattern", "glob", `{"pattern":5}`, "pattern"},
		{"glob rejects a boolean path", "glob", `{"pattern":"*","path":true}`, "path"},
		{"glob rejects a null path", "glob", `{"pattern":"*","path":null}`, "path"},
		{"grep accepts pattern-only", "grep", `{"pattern":"TODO"}`, ""},
		{"grep accepts optional path + include", "grep", `{"pattern":"TODO","path":"/tmp","include":"*.ts"}`, ""},
		{"grep rejects missing pattern", "grep", `{}`, "pattern"},
		{"grep rejects an empty pattern", "grep", `{"pattern":""}`, "pattern is required"},
		{"grep rejects a numeric pattern", "grep", `{"pattern":10}`, "pattern"},
		{"grep rejects an array pattern", "grep", `{"pattern":["a"]}`, "pattern"},
		{"grep rejects a numeric path", "grep", `{"pattern":"x","path":1}`, "path"},
		{"grep rejects a null include", "grep", `{"pattern":"x","include":null}`, "include"},
		{"grep rejects an object include", "grep", `{"pattern":"x","include":{}}`, "include"},
		{"grep rejects a non-object", "grep", `"TODO"`, "JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.n++
			id := fmt.Sprintf("t1.1.%d", f.n)
			_, res, prepared := f.d.Prepare(tool.Call{ID: id, CallID: "call_1", Tool: tc.tool, Input: json.RawMessage(tc.input)})
			f.d.Discard(id)
			if tc.field == "" {
				if !prepared {
					t.Fatalf("refused: %+v", res)
				}
				return
			}
			if prepared || res.Class != tool.ClassInvalidInput || !strings.Contains(res.Text, tc.field) {
				t.Fatalf("prepared %v, result = %+v; want invalid_input naming %q", prepared, res, tc.field)
			}
		})
	}
}

// TestSearchRefusesWithoutAnEnvironment: with no Env.Environ, grep and glob
// run nothing — as bash refuses (TestBashRefusesWithoutAnEnvironment), and
// rather than run rg with an environment of PWD alone, which is not what the
// session configured. The control is a configured environment: the same
// search finds the file.
func TestSearchRefusesWithoutAnEnvironment(t *testing.T) {
	t.Parallel()
	requireRG(t)
	p, err := Profile()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		in   map[string]any
	}{
		{"grep", map[string]any{"pattern": "needle"}},
		{"glob", map[string]any{"pattern": "*.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			i := slices.IndexFunc(p.Tools, func(tl tool.Tool) bool { return tl.Spec().ID == tc.name })
			if i < 0 {
				t.Fatalf("no %s tool in the profile", tc.name)
			}
			env := tool.Env{Workspace: t.TempDir(), Home: t.TempDir()}
			put(t, filepath.Join(env.Workspace, "hay.txt"), "needle\n")
			raw, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			run := func(env tool.Env) tool.Result {
				t.Helper()
				c, err := p.Tools[i].Prepare(env, tool.Call{ID: "t1.1.1", Tool: tc.name, Input: raw})
				if err != nil {
					t.Fatal(err)
				}
				return c.Run(context.Background(), env)
			}
			failed(t, run(env), tool.ClassToolError, noEnviron(tc.name))

			env.Environ = tool.ChildEnviron(os.Environ(), nil)
			if text := ok(t, run(env)); !strings.Contains(text, "hay.txt") {
				t.Fatalf("control: with an environment, %s did not find the file: %q", tc.name, text)
			}
		})
	}
}
