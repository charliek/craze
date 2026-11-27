package review

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/charliek/craze/internal/errkind"
)

func runGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s %v", args, b, err)
	}
	return string(b)
}
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	return dir
}
func put(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func commit(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "commit")
}
func TestBundleInlineAndUnborn(t *testing.T) {
	dir := newRepo(t)
	put(t, dir, "odd\nname", "added\n")
	put(t, dir, "binary", "\x00text")
	if e := os.Symlink("missing", filepath.Join(dir, "link")); e != nil {
		t.Fatal(e)
	}
	b, err := Build(t.Context(), dir, "HEAD", "prefix", 512<<10)
	if err != nil || !b.Inlined || len(b.Files) != 3 || !strings.Contains(b.Prompt, "symlink") || !strings.Contains(b.Prompt, "not inlined") || !strings.Contains(b.Prompt, "added") {
		t.Fatalf("%+v %v", b, err)
	}
	commit(t, dir)
	b, err = Build(t.Context(), dir, "HEAD", "prefix", 512<<10)
	if err != nil || !strings.Contains(b.Prompt, "No changes since HEAD.") {
		t.Fatal(b, err)
	}
	for _, base := range []string{"-p", "--help", "not-present"} {
		_, err := Build(t.Context(), dir, base, "", 512<<10)
		if info, ok := errkind.Of(err); !ok || info.Kind != errkind.Usage {
			t.Errorf("%s %v", base, err)
		}
	}
}
func TestBundleSpillAndBudget(t *testing.T) {
	dir := newRepo(t)
	put(t, dir, "a", "old\n")
	commit(t, dir)
	path := filepath.Join(dir, ".git", "index")
	put(t, dir, "a", strings.Repeat("needle\n", 910))
	put(t, dir, "z", "diff --git bogus\n")
	st, _ := os.Stat(path)
	b, err := Build(t.Context(), dir, "HEAD", "brief", 512<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Remove()
	raw, err := os.ReadFile(b.BundleFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	if b.Inlined || len(b.Files) != 2 || !strings.HasPrefix(lines[b.Files[0].First-1], "diff --git") || !strings.Contains(lines[b.Files[1].First-1], "z") || b.Bytes != len(raw) {
		t.Fatal(b)
	}
	for _, check := range []struct {
		p    string
		perm os.FileMode
	}{{b.BundleFile, 0600}, {filepath.Dir(b.BundleFile), 0700}} {
		s, e := os.Stat(check.p)
		if e != nil || s.Mode().Perm() != check.perm {
			t.Fatal(check, e)
		}
	}
	after, _ := os.Stat(path)
	if !st.ModTime().Equal(after.ModTime()) {
		t.Fatal("index mtime changed")
	}
	put(t, dir, "a", strings.Repeat("z", 8000))
	b, err = Build(t.Context(), dir, "HEAD", strings.Repeat("p", (512<<10)-7000), 512<<10)
	if err != nil || b.Inlined || len(b.Prompt) > 512<<10 {
		t.Fatal(err, b.Inlined)
	}
	defer b.Remove()
	if _, err = Build(t.Context(), dir, "HEAD", strings.Repeat("p", 512<<10), 512<<10); err == nil {
		t.Fatal("oversize prompt accepted")
	}
}
func TestBundleCapAndIndexMismatch(t *testing.T) {
	p := patchText{text: boundedBuffer{limit: 100000}}
	p.tracked([]byte("diff --git a/one b/one\n@@\n+x\ndiff --git a/two b/two\n+y\n"), []byte("wrong\x00"))
	if len(p.ranges) != 2 || p.ranges[0].Name != "a/one b/one" || p.ranges[1].First != 4 {
		t.Fatal(p.ranges)
	}
	dir := newRepo(t)
	put(t, dir, "large", "old\n")
	commit(t, dir)
	put(t, dir, "large", strings.Repeat("long line\n", 600000))
	b, err := Build(t.Context(), dir, "HEAD", "", 512<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Remove()
	data, err := os.ReadFile(b.BundleFile)
	if err != nil || len(data) > patchLimit || !bytes.HasSuffix(data, []byte(truncation)) {
		t.Fatalf("length %d, %v", len(data), err)
	}
}
func TestBundleDisablesGitConverters(t *testing.T) {
	dir := newRepo(t)
	put(t, dir, "a", "old\n")
	put(t, dir, ".gitattributes", "a diff=poison filter=example\n")
	commit(t, dir)
	runGit(t, dir, "config", "diff.poison.textconv", "false")
	runGit(t, dir, "config", "diff.external", "false")
	runGit(t, dir, "config", "filter.example.required", "true")
	runGit(t, dir, "config", "filter.example.clean", "false")
	runGit(t, dir, "config", "filter.example.process", "false")
	put(t, dir, "a", "new\n")
	_ = snap(t, dir)
	b, err := Build(t.Context(), dir, "HEAD", "", 512<<10)
	if err != nil || !strings.Contains(b.Prompt, "+new") {
		t.Fatal(b, err)
	}
}
func snap(t *testing.T, dir string) Snapshot {
	t.Helper()
	s, e := Inspect(t.Context(), dir)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestIntegrity(t *testing.T) {
	dir := newRepo(t)
	put(t, dir, "tracked", "a\n")
	commit(t, dir)
	before := snap(t, dir)
	if e := os.Chmod(filepath.Join(dir, "tracked"), 0700); e != nil {
		t.Fatal(e)
	}
	if len(Changes(before, snap(t, dir))) == 0 {
		t.Fatal("missed chmod")
	}
	if err := os.Symlink("one", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	before = snap(t, dir)
	if e := os.Remove(filepath.Join(dir, "link")); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("two", filepath.Join(dir, "link")); e != nil {
		t.Fatal(e)
	}
	if len(Changes(before, snap(t, dir))) == 0 {
		t.Fatal("missed dangling symlink swap")
	}
	before = snap(t, dir)
	runGit(t, dir, "add", "tracked")
	if !slices.Contains(Changes(before, snap(t, dir)), "index") {
		t.Fatal("missed staged change")
	}
	runGit(t, dir, "commit", "-qm", "mode")
	before = snap(t, dir)
	runGit(t, dir, "branch", "other")
	if !slices.Contains(Changes(before, snap(t, dir)), "refs") {
		t.Fatal("missed ref")
	}
	before = snap(t, dir)
	runGit(t, dir, "switch", "-q", "other")
	if !slices.Contains(Changes(before, snap(t, dir)), "HEAD") {
		t.Fatal("missed checkout")
	}
	before = snap(t, dir)
	put(t, dir, "tracked", "changed")
	runGit(t, dir, "stash", "push", "-qm", "scratch")
	if !slices.Contains(Changes(before, snap(t, dir)), "stash") {
		t.Fatal("missed stash")
	}
	if s := snap(t, t.TempDir()); s.Root != "" {
		t.Fatal(s)
	}
	b := snap(t, newRepo(t))
	if b.Root == "" {
		t.Fatal("unborn tree was skipped")
	}
	if _, err := Inspect(t.Context(), filepath.Join(dir, "no-such-directory")); err == nil || errors.Is(err, syscall.EPIPE) {
		t.Fatal(err)
	}
}

func TestBundleIncludesGitlinkAndRawPath(t *testing.T) {
	child := newRepo(t)
	put(t, child, "a", "first")
	commit(t, child)
	dir := newRepo(t)
	runGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", child, "module")
	commit(t, dir)
	put(t, filepath.Join(dir, "module"), "a", "second")
	commit(t, filepath.Join(dir, "module"))
	put(t, dir, "odd-\xff", "raw")
	b, err := Build(t.Context(), dir, "HEAD", "", 512<<10)
	if err != nil || !strings.Contains(b.Prompt, "Subproject commit") || !strings.Contains(b.Prompt, `odd-\xff`) {
		t.Fatalf("bundle %q %v", b.Prompt, err)
	}
}
func TestIntegrityChecksNestedAndOverBudgetFiles(t *testing.T) {
	dir := newRepo(t)
	child := filepath.Join(dir, "embedded")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	runGit(t, child, "init", "-q")
	put(t, child, "track", "one")
	commit(t, child)
	before := snap(t, dir)
	put(t, child, "track", "two")
	after := snap(t, dir)
	if !after.Incomplete || len(Changes(before, after)) == 0 {
		t.Fatal("missed embedded worktree")
	}
	path := filepath.Join(dir, "huge")
	put(t, dir, "huge", "")
	if err := os.Truncate(path, hashFileLimit+1); err != nil {
		t.Fatal(err)
	}
	before = snap(t, dir)
	if err := os.Truncate(path, hashFileLimit+2); err != nil {
		t.Fatal(err)
	}
	if !before.Incomplete || len(Changes(before, snap(t, dir))) == 0 {
		t.Fatal("missing bounded scan metadata")
	}
}

func TestReviewBareRepositoryAndUnbornBranch(t *testing.T) {
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare", "-q")
	if s := snap(t, bare); s.Root != "" {
		t.Fatal(s.Root)
	}
	dir := newRepo(t)
	before := snap(t, dir)
	runGit(t, dir, "symbolic-ref", "HEAD", "refs/heads/empty-other-branch")
	if !slices.Contains(Changes(before, snap(t, dir)), "HEAD") {
		t.Fatal("missed an unborn HEAD change")
	}
	commit(t, dir)
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "unchanged contents")
	b, err := Build(t.Context(), dir, "HEAD^", "", 512<<10)
	if err != nil || !strings.Contains(b.Prompt, "No changes since HEAD^.") || !strings.Contains(b.Prompt, "unchanged contents") {
		t.Fatal(b, err)
	}
}

func TestIntegrityOfCleanTrackedPaths(t *testing.T) {
	dir := newRepo(t)
	put(t, dir, "file", "old")
	commit(t, dir)
	before := snap(t, dir)
	if err := os.Chmod(filepath.Join(dir, "file"), 0644); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(Changes(before, snap(t, dir)), "file") {
		t.Fatal("missed non-executable mode change")
	}
	before = snap(t, dir)
	runGit(t, dir, "update-index", "--assume-unchanged", "file")
	if !slices.Contains(Changes(before, snap(t, dir)), "index") {
		t.Fatal("missed index flag")
	}
	before = snap(t, dir)
	put(t, dir, "file", "new")
	if !slices.Contains(Changes(before, snap(t, dir)), "file") {
		t.Fatal("missed assume-unchanged content")
	}
}

func TestReviewIgnoresInheritedRepoSelection(t *testing.T) {
	dir, other := newRepo(t), newRepo(t)
	commit(t, dir)
	commit(t, other)
	put(t, dir, "selected", "intended workspace")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	if got := snap(t, dir).Root; got != dir {
		t.Fatal(got)
	}
	b, err := Build(t.Context(), dir, "HEAD", "", 512<<10)
	if err != nil || !strings.Contains(b.Prompt, "intended workspace") {
		t.Fatal(b, err)
	}
}
