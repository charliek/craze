package cli

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/errkind"
)

func (f reviewFlags) checkInputs(words []string) error {
	if len(words) > 0 && len(f.files) > 0 || f.remove && len(f.files) == 0 {
		return usagef("craze review: --prompt-file is exclusive with words and required for --remove-prompt-file")
	}
	stdin := false
	for _, p := range f.files {
		if p == "" || p == "-" && stdin {
			return usagef("craze review: each prompt file needs a path; stdin can appear once")
		}
		if p == "-" {
			stdin = true
		}
	}
	return nil
}
func (f reviewFlags) readInputs(ctx context.Context, cmd *cobra.Command, words []string) (string, error) {
	if len(words) > 0 {
		return checkPrompt(reviewWhat, strings.Join(words, " "), nil)
	}
	files := f.files
	if len(files) == 0 && !stdinIsTTY(cmd.InOrStdin()) {
		files = []string{"-"}
	}
	removed := map[string]os.FileInfo{}
	var out strings.Builder
	for i, path := range files {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		type input struct {
			text string
			err  error
			info os.FileInfo
		}
		ch := make(chan input, 1)
		go func() {
			var v input
			if path == "-" {
				v.text, v.err = readPrompt(cmd.InOrStdin())
			} else {
				file, e := os.Open(path)
				if e != nil {
					v.err = e
				} else {
					defer file.Close()
					v.info, v.err = file.Stat()
					if v.err == nil {
						v.text, v.err = readPrompt(file)
					}
				}
			}
			ch <- v
		}()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case v := <-ch:
			if v.err != nil {
				return "", usageKindf(errkind.Prompt, "craze review: reading prompt: %s", sanitizeLine(v.err.Error()))
			}
			if f.remove && path != "-" && v.info.Mode().IsRegular() {
				removed[path] = v.info
			}
			if i > 0 {
				out.WriteString("\n\n")
			}
			out.WriteString(strings.TrimSpace(v.text))
			if out.Len() > promptTextMax {
				return "", usageKindf(errkind.Prompt, "craze review: prompt files exceed 512 KiB")
			}
		}
	}
	// Wait until every source has been read: the same file may appear twice.
	var unlinked []os.FileInfo
	for path, info := range removed {
		now, e := os.Lstat(path)
		if errors.Is(e, fs.ErrNotExist) && slices.ContainsFunc(unlinked, func(u os.FileInfo) bool { return os.SameFile(u, info) }) {
			continue
		}
		if e != nil {
			return "", usageKindf(errkind.Prompt, "craze review: checking prompt file: %s", e)
		}
		if now.Mode().IsRegular() && os.SameFile(now, info) {
			if e := os.Remove(path); e != nil {
				return "", usageKindf(errkind.Prompt, "craze review: removing prompt file: %s", e)
			}
			unlinked = append(unlinked, info)
		}
	}
	input := strings.TrimSpace(out.String())
	if input == "" && f.criteria == "" && f.base == "" {
		return "", usageKindf(errkind.Prompt, "craze review: provide a prompt, criteria, or changes-since ref")
	}
	return input, nil
}
