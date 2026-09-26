package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/charliek/craze/internal/paths"
)

// TestMain is the guard plan 020 R6 asks for. The TUI and `craze prompt`
// journal every session by default, so a test in this package that builds a
// real session without pointing CRAZE_HOME at a temp directory would journal
// into the developer's own craze directory — and lazy creation only delays
// the file to the session's first line. Every such test sets CRAZE_HOME; this
// proves it, by failing the run if a journal written by this test process
// appeared under the real journal directory.
//
// "Real" is computed from the environment the test binary started with,
// before any test rewrites it: the journal directory as craze would resolve
// it (an exported CRAZE_HOME included), and ~/.craze/journal besides, which a
// test that cleared CRAZE_HOME but kept HOME would reach. A new file there
// counts only when its header's pid is this process's, so a craze the
// developer runs at the same time cannot trip the guard, and a file whose
// header cannot be read is left alone for the same reason.
func TestMain(m *testing.M) {
	dirs := realJournalDirs()
	before := journalsIn(dirs)
	// Every TUI run serves a control socket (plan 027 C13), in the runtime
	// base's first usable candidate: $XDG_RUNTIME_DIR/craze on a desktop.
	// A test that reached it would leave its namespace directory in the
	// developer's own runtime directory, so the whole package runs under a
	// short one of its own (never $TMPDIR: sun_path, plan 027 §3.8). A test
	// that wants another sets CRAZE_RUNTIME_DIR itself.
	runtimeDir, err := os.MkdirTemp("/tmp", "czc")
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: a runtime directory for the package:", err)
		os.Exit(1)
	}
	_ = os.Setenv("CRAZE_RUNTIME_DIR", runtimeDir)
	_ = os.Unsetenv("XDG_RUNTIME_DIR")
	_ = os.Unsetenv(controlSocketEnv)
	code := m.Run()
	_ = os.RemoveAll(runtimeDir)
	if leaked := journaledBy(os.Getpid(), dirs, before); len(leaked) > 0 {
		fmt.Fprintln(os.Stderr, "FAIL: a test journaled into the real craze directory; set CRAZE_HOME to a temp dir in it:")
		for _, path := range leaked {
			fmt.Fprintln(os.Stderr, "\t"+path)
		}
		code = 1
	}
	os.Exit(code)
}

// realJournalDirs is where a test that forgot CRAZE_HOME would journal:
// paths.JournalDir under the starting environment, and the default under
// HOME when that differs.
func realJournalDirs() []string {
	var dirs []string
	if dir := paths.JournalDir(); dir != "" {
		dirs = append(dirs, dir)
	}
	if home := paths.HomeDir(); home != "" {
		if dir := filepath.Join(home, ".craze", "journal"); !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// journalsIn is every journal file under dirs: <dir>/<workspace slug>/*.jsonl.
func journalsIn(dirs []string) map[string]bool {
	files := map[string]bool{}
	for _, dir := range dirs {
		matches, _ := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
		for _, m := range matches {
			files[m] = true
		}
	}
	return files
}

// journaledBy is every journal file under dirs that was not in before and
// whose header says pid wrote it.
func journaledBy(pid int, dirs []string, before map[string]bool) []string {
	var out []string
	for path := range journalsIn(dirs) {
		if !before[path] && headerPID(path) == pid {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}

// headerPID is the pid a journal's header line records, or 0 when the first
// line is not a readable header.
func headerPID(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	if !sc.Scan() {
		return 0
	}
	var h struct {
		Type string `json:"type"`
		PID  int    `json:"pid"`
	}
	if json.Unmarshal(sc.Bytes(), &h) != nil || h.Type != "header" {
		return 0
	}
	return h.PID
}
