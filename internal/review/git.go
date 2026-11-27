// Package review prepares review prompts and checks a worktree's integrity.
package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charliek/craze/internal/errkind"
)

type problem struct {
	kind    errkind.Kind
	message string
}

func (e problem) Error() string           { return e.message }
func (e problem) ErrorInfo() errkind.Info { return errkind.New(e.kind) }

type boundedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Len() int      { return b.buf.Len() }
func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }
func (b *boundedBuffer) Write(data []byte) (int, error) {
	count := len(data)
	if room := b.limit - b.Len(); count > room {
		data = data[:room]
		b.truncated = true
	}
	_, err := b.buf.Write(data)
	return count, err
}

func git(ctx context.Context, dir string, maxSize int, args ...string) ([]byte, bool, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.fsmonitor=false", "-c", "core.filemode=true", "-c", "color.ui=false", "-c", "core.quotePath=true", "-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.WaitDelay = time.Second
	out := &boundedBuffer{limit: maxSize}
	diag := &boundedBuffer{limit: 1024}
	cmd.Stderr = diag
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			ee.Stderr = diag.Bytes()
		}
		return nil, false, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out.Bytes(), out.truncated, nil
}

const metadataLimit = 16 << 20

func gitData(ctx context.Context, dir string, args ...string) ([]byte, error) {
	b, cut, err := git(ctx, dir, metadataLimit, args...)
	if err == nil && cut {
		return nil, errors.New("git metadata exceeded 16 MiB")
	}
	return b, err
}

func repoRoot(ctx context.Context, dir string) (string, error) {
	b, err := gitData(ctx, dir, "rev-parse", "--show-toplevel")
	return strings.TrimSuffix(string(b), "\n"), err
}

func resolveOID(ctx context.Context, root, ref string) (string, error) {
	b, err := gitData(ctx, root, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	return strings.TrimSpace(string(b)), err
}
func unborn(ctx context.Context, root string) bool {
	ref, err := gitData(ctx, root, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return false
	}
	_, err = gitData(ctx, root, "show-ref", "--verify", "--quiet", "--", strings.TrimSpace(string(ref)))
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 1
}
