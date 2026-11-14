package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// The tools a session is offered before plan 051's web tools existed, taken
// at 5c6665a (whose tools are 0cfc228's) before C2 changed a line: the
// serialized tools array (toolset.wire, which the header hashes) of a
// top-level session that runs background jobs, of a jobs-less one, and of a
// child with every child tool. A session opened with the zero web state must
// offer exactly these bytes, so the plan's tools cost a withheld session
// nothing — not its specs, not the agent tool's text, not its tools_sha256,
// not a byte of its cache prefix.
//
// bash's description names the machine — its OS, its shell and the
// temporary directory it offers — so testdata/base_tools holds each array
// with those three written ${os}, ${shell} and ${tmp}. These files are not
// goldens and have no -update: they were written once from the base, and the
// hashes below, which are the base's own tools_sha256 on a linux machine with
// bash and /tmp/craze, pin their bytes too.
var baseTools = []struct {
	name  string
	sha   string // tools_sha256 at the base, for the pinned host
	tools []string
}{
	{"top", "cd4d1ce95eaa117713b8260fd60000a88e43756b8fac87beec7c121c89506656", []string{
		"bash", "bash_output", "bash_stop", "read", "glob", "grep", "edit", "write",
		"agent", "agent_output", "todo_write", "ask_user_question", "exit_plan_mode"}},
	{"jobsless", "71ab103bd550f341430225701a21d6f28de08c221273dc3aa18754c7325be50a", []string{
		"bash", "read", "glob", "grep", "edit", "write",
		"agent", "agent_output", "todo_write", "ask_user_question", "exit_plan_mode"}},
	{"child", "6526d4b1b33f2a89e5a18c4e5b20ea1c092a678584ad85d0947afa28e80eb65a", []string{
		"bash", "read", "glob", "grep", "edit", "write"}},
}

// pinnedHost is the machine the base hashes were taken on.
var pinnedHost = [3]string{"linux", "bash", "/tmp/craze"}

var (
	hostOSShell = regexp.MustCompile(`OS: (\w+), Shell: (\w+)`)
	hostTmp     = regexp.MustCompile("Use `([^`]+)` for temporary work")
)

// hostNeutral is wire with this machine's three values in bash's description
// written as placeholders, and the values it found. The values are held to
// what this test finds of the machine itself (thisMachine), not only taken
// from the text under test: a change confined to them — another temporary
// directory, the shell's path where its name was — would otherwise be
// written back as a placeholder and pass.
func hostNeutral(t *testing.T, wire []byte) ([]byte, [3]string) {
	t.Helper()
	m, tm := hostOSShell.FindSubmatch(wire), hostTmp.FindSubmatch(wire)
	if m == nil || tm == nil {
		t.Fatalf("bash's description does not name the machine as the base's did:\n%s", wire)
	}
	host := [3]string{string(m[1]), string(m[2]), string(tm[1])}
	if want := thisMachine(t, host[2]); host != want {
		t.Fatalf("bash's description names the machine %q; this machine, found by the test, is %q", host, want)
	}
	out := bytes.Replace(wire, m[0], []byte("OS: ${os}, Shell: ${shell}"), 1)
	out = bytes.Replace(out, tm[0], []byte("Use `${tmp}` for temporary work"), 1)
	return out, host
}

// thisMachine is what bash's description should say of this machine, found
// from the machine and not from the description, by the rules the base's
// bash used: the OS is runtime.GOOS; the shell is bash when /bin/bash is a
// regular, executable file, else sh; the temporary directory is
// os.TempDir()/craze when that is a directory (not a symlink) of this user's
// with mode 0700 — the session's bash made it so, or found it so, when it
// opened. Otherwise bash left that path alone and made a directory with a
// random name beside it, so found, the description's, must be a craze-*
// directory in os.TempDir(), and is the answer.
func thisMachine(t *testing.T, found string) [3]string {
	t.Helper()
	shell := "sh"
	if info, err := os.Stat("/bin/bash"); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
		shell = "bash"
	}
	base := filepath.Clean(os.TempDir())
	tmp := filepath.Join(base, "craze")
	if info, err := os.Lstat(tmp); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByMe(info) {
		if filepath.Dir(found) != base || !strings.HasPrefix(filepath.Base(found), "craze-") {
			t.Fatalf("%s is not usable, and bash's description offers %s, not a craze-* directory in %s", tmp, found, base)
		}
		tmp = found
	}
	return [3]string{runtime.GOOS, shell, tmp}
}

func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// onHost is a host-neutral array with host's values put back.
func onHost(neutral []byte, host [3]string) []byte {
	out := bytes.Replace(neutral, []byte("${os}"), []byte(host[0]), 1)
	out = bytes.Replace(out, []byte("${shell}"), []byte(host[1]), 1)
	return bytes.Replace(out, []byte("${tmp}"), []byte(host[2]), 1)
}

func hexSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// openBaseSessions opens the three sessions baseTools describes, each with
// Options as every caller built them before plan 051: no web state set.
func openBaseSessions(t *testing.T, f *fixture, mutate func(*Options)) map[string]*Session {
	t.Helper()
	if mutate == nil {
		mutate = func(*Options) {}
	}
	top := f.options()
	top.Background = true
	mutate(&top)
	jobsless := f.options()
	mutate(&jobsless)
	parent := f.open(jobsless)
	child := childOf(f, parent, ChildOptions{
		ID: "child-0001", ParentCall: "t1.1.1", Type: "general-purpose", Role: "r", AllTools: true, Mode: "agent",
	})
	mutate(&child)
	return map[string]*Session{"top": f.open(top), "jobsless": parent, "child": f.open(child)}
}

// checkBaseTools holds each of sessions to its base bytes and hash.
func checkBaseTools(t *testing.T, sessions map[string]*Session) {
	t.Helper()
	for _, b := range baseTools {
		s := sessions[b.name]
		want, err := os.ReadFile(filepath.Join("testdata", "base_tools", b.name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		// The files are the base's: the pinned host's rendering of each is the
		// hash the base's header recorded.
		if got := hexSHA(onHost(want, pinnedHost)); got != b.sha {
			t.Fatalf("%s: testdata/base_tools/%s.json is not the base's (its pinned-host hash is %s, the base's %s)", b.name, b.name, got, b.sha)
		}
		var specs []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(want, &specs); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, sp := range specs {
			names = append(names, sp.Name)
		}
		if !slices.Equal(names, b.tools) {
			t.Fatalf("%s: the base offered %v; this test says %v", b.name, names, b.tools)
		}

		wire := s.tools.wire
		if h := s.store.Header().ToolsSHA256; h != hexSHA(wire) {
			t.Fatalf("%s: the header's tools_sha256 %s is not the hash of the tools offered (%s)", b.name, h, hexSHA(wire))
		}
		got, host := hostNeutral(t, wire)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: the tools offered differ from the base's\n--- base ---\n%s\n--- got ---\n%s", b.name, want, got)
		}
		if h := s.store.Header().ToolsSHA256; h != hexSHA(onHost(want, host)) {
			t.Fatalf("%s: tools_sha256 %s; the base's on this machine is %s", b.name, h, hexSHA(onHost(want, host)))
		}
		if host == pinnedHost && s.store.Header().ToolsSHA256 != b.sha {
			t.Fatalf("%s: tools_sha256 %s on the pinned host; the base's is %s", b.name, s.store.Header().ToolsSHA256, b.sha)
		}
	}
}

// TestWithheldWebIsTheBase (plan 051 C2's first step): sessions opened as
// every caller opened them before the plan — the web state's zero value,
// withheld — offer the base's tools, byte for byte, and record its
// tools_sha256.
func TestWithheldWebIsTheBase(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	checkBaseTools(t, openBaseSessions(t, f, nil))
}
