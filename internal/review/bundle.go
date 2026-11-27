package review

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/charliek/craze/internal/errkind"
)

const (
	patchLimit   = 5 << 20
	maxUntracked = 200 << 10
	maxIndex     = 300
	truncation   = "\n[changes truncated at 5 MiB]\n"
)

type FileRange struct {
	Name  string `json:"name"`
	First int    `json:"first"`
	Last  int    `json:"last"`
}
type Bundle struct {
	Inlined    bool        `json:"inlined"`
	BundleFile string      `json:"bundleFile"`
	Lines      int         `json:"lines"`
	Bytes      int         `json:"bytes"`
	Files      []FileRange `json:"files"`
	Prompt     string      `json:"-"`
}

func (b Bundle) Remove() error {
	if b.BundleFile == "" {
		return nil
	}
	return os.RemoveAll(filepath.Dir(b.BundleFile))
}

type patchText struct {
	text   boundedBuffer
	ranges []FileRange
	lines  int
}

func (p *patchText) add(b []byte) {
	start := p.text.Len()
	_, _ = p.text.Write(b)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		_, _ = p.text.Write([]byte{'\n'})
	}
	p.lines += bytes.Count(p.text.Bytes()[start:], []byte{'\n'})
}
func quotePath(raw []byte) string {
	quoted := strconv.QuoteToASCII(string(raw))
	if strings.ContainsRune(quoted, '\\') {
		return quoted
	}
	return quoted[1 : len(quoted)-1]
}

func (p *patchText) tracked(patch, names []byte) {
	starts := []FileRange{}
	for len(patch) > 0 && !p.text.truncated {
		line, rest, _ := bytes.Cut(patch, []byte{'\n'})
		if bytes.HasPrefix(line, []byte("diff --git ")) {
			starts = append(starts, FileRange{Name: string(bytes.TrimPrefix(line, []byte("diff --git "))), First: p.lines + 1})
		}
		p.add(line)
		if len(line) == 0 {
			p.add([]byte{'\n'})
		}
		patch = rest
	}
	paths := bytes.Split(bytes.TrimSuffix(names, []byte{0}), []byte{0})
	for i, r := range starts {
		// A status/diff race, dirty submodule or truncation can change
		// the number of sections. The emitted header then identifies it.
		if len(paths) == len(starts) {
			r.Name = quotePath(paths[i])
		}
		r.Last = p.lines
		if i+1 < len(starts) {
			r.Last = starts[i+1].First - 1
		}
		p.ranges = append(p.ranges, r)
	}
}

func untrackedBlock(root *os.Root, name string) ([]byte, error) {
	rel := filepath.Clean(name)
	st, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	title := "===== " + quotePath([]byte(name))
	if st.Mode()&os.ModeSymlink != 0 {
		target, err := root.Readlink(rel)
		return []byte(title + " (symlink to " + quotePath([]byte(target)) + ") ====="), err
	}
	summary := []byte(fmt.Sprintf("%s (%d bytes, not inlined; read from worktree) =====", title, st.Size()))
	if !st.Mode().IsRegular() || st.Size() > maxUntracked {
		return summary, nil
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return summary, nil
	}
	b, err := io.ReadAll(io.LimitReader(f, maxUntracked+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxUntracked || bytes.IndexByte(b, 0) >= 0 {
		return summary, nil
	}
	return append([]byte(title+" =====\n"), b...), nil
}

// Build includes the net tracked patch and non-ignored untracked files. maxPrompt
// bounds the final assembled message, including the spill instructions and index.
func Build(ctx context.Context, dir, base, prefix string, maxPrompt int) (Bundle, error) {
	if base == "" || strings.HasPrefix(base, "-") {
		return Bundle{}, problem{errkind.Usage, "invalid changes-since ref"}
	}
	root, err := repoRoot(ctx, dir)
	if err != nil {
		return Bundle{}, err
	}
	head, headErr := resolveOID(ctx, root, "HEAD")
	oid, err := resolveOID(ctx, root, base)
	if err != nil {
		if headErr != nil && base == "HEAD" && unborn(ctx, root) {
			b, e := gitData(ctx, root, "hash-object", "-t", "tree", "--stdin")
			if e != nil {
				return Bundle{}, e
			}
			oid = strings.TrimSpace(string(b))
		} else if ctx.Err() != nil {
			return Bundle{}, ctx.Err()
		} else {
			return Bundle{}, problem{errkind.Usage, "changes-since does not name an available commit"}
		}
	} else if headErr != nil && !unborn(ctx, root) {
		return Bundle{}, headErr
	}
	status, err := gitData(ctx, root, "status", "--short", "--untracked-files=all")
	if err != nil {
		return Bundle{}, err
	}
	p := patchText{text: boundedBuffer{limit: patchLimit - len(truncation)}}
	if head != "" && head != oid {
		log, cut, e := git(ctx, root, patchLimit, "log", "--no-show-signature", "--oneline", oid+".."+head, "--")
		if e != nil {
			return Bundle{}, e
		}
		p.add([]byte("--- commits since " + base + " ---"))
		p.add(log)
		p.text.truncated = p.text.truncated || cut
	}
	patch, cut, err := git(ctx, root, patchLimit, "diff-index", "--no-textconv", "--no-ext-diff", "--submodule=short", "-M", "-p", oid, "--")
	if err != nil {
		return Bundle{}, err
	}
	names, err := gitData(ctx, root, "diff-index", "--no-textconv", "--no-ext-diff", "--name-only", "-z", "-M", oid, "--")
	if err != nil {
		return Bundle{}, err
	}
	untracked, err := gitData(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return Bundle{}, err
	}
	if len(patch) == 0 && len(untracked) == 0 && len(status) == 0 {
		p.add([]byte("No changes since " + base + "."))
	} else {
		p.add([]byte("--- changed files ---"))
		p.add(status)
		p.add([]byte("--- diff against " + base + " ---"))
		p.tracked(patch, names)
		p.text.truncated = p.text.truncated || cut
		rootFD, e := os.OpenRoot(root)
		if e != nil {
			return Bundle{}, e
		}
		defer rootFD.Close()
		for _, name := range bytes.Split(untracked, []byte{0}) {
			if p.text.truncated {
				break
			}
			if len(name) == 0 {
				continue
			}
			if err := ctx.Err(); err != nil {
				return Bundle{}, err
			}
			block, e := untrackedBlock(rootFD, string(name))
			if e != nil {
				return Bundle{}, e
			}
			first := p.lines + 1
			p.add(block)
			p.ranges = append(p.ranges, FileRange{Name: quotePath(name), First: first, Last: max(first, p.lines)})
		}
	}
	if p.text.truncated {
		// The bounded writer can end inside a line. Finish it and mark
		// the truncation in the bytes the reviewer actually sees.
		p.text.limit = patchLimit
		p.add([]byte(truncation))
	}
	changes := p.text.Bytes()
	result := Bundle{Lines: p.lines, Bytes: len(changes), Files: p.ranges}
	if result.Files == nil {
		result.Files = []FileRange{}
	}
	if len(result.Files) > maxIndex {
		result.Files = result.Files[:maxIndex]
	}
	limit := min(100<<10, maxPrompt-len(prefix)-4096)
	if p.lines <= 900 && len(changes) <= limit {
		result.Inlined = true
		result.Prompt = prefix + "\n\n=== BEGIN CHANGES ===\n" + strings.ToValidUTF8(string(changes), "�") + "=== END CHANGES ===\n"
		if len(result.Prompt) <= maxPrompt {
			return result, nil
		}
		result.Inlined = false
	}
	tmp, err := os.MkdirTemp("", "craze-review-")
	if err != nil {
		return Bundle{}, err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(tmp)
		}
	}()
	canonical, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		return Bundle{}, err
	}
	result.BundleFile = filepath.Join(canonical, "changes.diff")
	var out strings.Builder
	fmt.Fprintf(&out, "%s\n\nRead the changes from %s in line ranges with offset and limit (50 KiB per read, 2,000 runes per line).\n", prefix, result.BundleFile)
	for _, r := range result.Files {
		fmt.Fprintf(&out, "  %s: lines %d-%d\n", r.Name, r.First, r.Last)
	}
	if more := len(p.ranges) - len(result.Files); more > 0 {
		fmt.Fprintf(&out, "  %d more files; consult the remaining diff headers.\n", more)
	}
	result.Prompt = out.String()
	if len(result.Prompt) > maxPrompt {
		return Bundle{}, problem{errkind.Prompt, "assembled review prompt exceeds 512 KiB"}
	}
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	if err = os.WriteFile(result.BundleFile, changes, 0600); err != nil {
		return Bundle{}, err
	}
	success = true
	return result, nil
}
