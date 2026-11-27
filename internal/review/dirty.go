package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const (
	hashFileLimit  = 16 << 20
	hashTotalLimit = 128 << 20
	hashFileCount  = 2000
)

type Snapshot struct {
	Root       string
	Incomplete bool
	state      map[string]string
}
type fileScan struct {
	ctx           context.Context
	root          *os.Root
	hashes, bytes int
	snapshot      *Snapshot
}

func (s *fileScan) record(path string) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	path = filepath.Clean(path)
	if !filepath.IsLocal(path) || path == "." {
		return errors.New("invalid git status path")
	}
	name := "file " + quotePath([]byte(path))
	if _, done := s.snapshot.state[name]; done {
		return nil
	}
	fi, err := s.root.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		s.snapshot.state[name] = "deleted"
		return nil
	}
	if err != nil {
		return err
	}
	prefix := fi.Mode().String() + " "
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		dest, e := s.root.Readlink(path)
		if e != nil {
			return e
		}
		s.snapshot.state[name] = prefix + dest
	case fi.IsDir():
		s.snapshot.state[name] = prefix
		// A nested repo can be reported as one untracked directory. Walk
		// only its worktree and bound traversal. git already enumerates
		// ordinary untracked directories via --untracked-files=all.
		s.snapshot.Incomplete = true
		f, e := s.root.Open(path)
		if e != nil {
			return e
		}
		defer f.Close()
		for s.hashes < hashFileCount {
			entries, e := f.ReadDir(64)
			for _, entry := range entries {
				if entry.Name() == ".git" {
					continue
				}
				if s.hashes >= hashFileCount {
					break
				}
				s.hashes++
				if err := s.record(filepath.Join(path, entry.Name())); err != nil {
					return err
				}
			}
			if errors.Is(e, io.EOF) {
				break
			}
			if e != nil {
				return e
			}
		}
	case !fi.Mode().IsRegular():
		s.snapshot.state[name] = prefix
	case fi.Size() > hashFileLimit || s.hashes >= hashFileCount || int64(s.bytes)+fi.Size() > hashTotalLimit:
		s.snapshot.Incomplete = true
		s.snapshot.state[name] = fmt.Sprintf("%s%d %d %d", prefix, fi.Size(), fi.ModTime().UnixNano(), fi.Sys().(*syscall.Stat_t).Ino)
	default:
		f, e := s.root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if e != nil {
			return e
		}
		defer f.Close()
		opened, e := f.Stat()
		if e != nil {
			return e
		}
		if !os.SameFile(fi, opened) || !opened.Mode().IsRegular() {
			return errors.New("file changed during integrity check")
		}
		h := sha256.New()
		n, e := io.Copy(h, io.LimitReader(f, fi.Size()+1))
		if e != nil {
			return e
		}
		if n != fi.Size() {
			return errors.New("file changed during integrity check")
		}
		s.bytes += int(n)
		s.hashes++
		s.snapshot.state[name] = fmt.Sprintf("%s%x", prefix, h.Sum(nil))
	}
	return nil
}

// Inspect captures refs, index and dirty worktree content without refreshing
// the index. Root is empty for a directory outside a git worktree.
func Inspect(ctx context.Context, dir string) (Snapshot, error) {
	root, err := repoRoot(ctx, dir)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 128 {
			// Distinguish "outside a repository" from access failures by
			// checking the directory and git's own diagnostic.
			if st, e := os.Stat(dir); e == nil && st.IsDir() {
				if strings.Contains(string(ee.Stderr), "not a git repository") {
					return Snapshot{}, nil
				}
				inside, e := gitData(ctx, dir, "rev-parse", "--is-inside-work-tree")
				if e == nil && strings.TrimSpace(string(inside)) == "false" {
					return Snapshot{}, nil
				}
			}
		}
		return Snapshot{}, err
	}
	s := Snapshot{Root: root, state: map[string]string{}}
	head, err := resolveOID(ctx, root, "HEAD")
	if err != nil && !unborn(ctx, root) {
		return Snapshot{}, err
	}
	s.state["HEAD"] = head
	for _, entry := range []struct {
		key  string
		args []string
	}{
		{"refs", []string{"for-each-ref", "--format=%(refname) %(objectname)"}},
		{"stash", []string{"stash", "list", "--format=%H%x00%gs"}},
		{"index", []string{"ls-files", "--stage", "-z"}},
	} {
		b, e := gitData(ctx, root, entry.args...)
		if e != nil {
			return Snapshot{}, e
		}
		s.state[entry.key] = fmt.Sprintf("%x", sha256.Sum256(b))
	}
	branch, e := gitData(ctx, root, "symbolic-ref", "--quiet", "HEAD")
	if e == nil {
		s.state["HEAD"] += " " + string(branch)
	} else {
		var exit *exec.ExitError
		if !errors.As(e, &exit) || exit.ExitCode() != 1 {
			return Snapshot{}, e
		}
	}
	status, err := gitData(ctx, root, "status", "--porcelain=v1", "--no-renames", "-z", "--untracked-files=all")
	if err != nil {
		return Snapshot{}, err
	}
	fd, err := os.OpenRoot(root)
	if err != nil {
		return Snapshot{}, err
	}
	defer fd.Close()
	scan := fileScan{ctx: ctx, root: fd, snapshot: &s}
	for _, rec := range bytes.Split(status, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		if len(rec) < 4 || rec[2] != ' ' {
			return Snapshot{}, errors.New("invalid git status entry")
		}
		path := string(rec[3:])
		s.state["status "+quotePath(rec[3:])] = string(rec[:2])
		if e := scan.record(path); e != nil {
			return Snapshot{}, e
		}
	}
	return s, nil
}

func Changes(before, after Snapshot) []string {
	changed := map[string]bool{}
	compare := func(left, right Snapshot) {
		for k, val := range left.state {
			if other, ok := right.state[k]; !ok || other != val {
				label, isFile := strings.CutPrefix(k, "file ")
				if !isFile {
					label = strings.TrimPrefix(k, "status ")
				}
				changed[label] = true
			}
		}
	}
	if before.Root != after.Root {
		changed["worktree"] = true
	}
	compare(before, after)
	compare(after, before)
	out := make([]string, 0, len(changed))
	for k := range changed {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
