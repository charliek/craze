package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// The prompt of craze new --wait and craze send (plan 041 §3.2, P14, P16):
// one source of three — --prompt-file <path> (`-` for stdin), the words after
// the command's own arguments, or stdin when it is not a terminal — and
// exactly one: a file and words together are a usage error, and stdin is read
// only when neither was given. None, or a prompt that is empty once its
// surrounding whitespace is gone, is a usage error (exit 2); so is one over
// promptTextMax, refused before anything is spawned or dialled.
// --remove-prompt-file deletes --prompt-file's file once it has been read —
// whatever the prompt then turns out to be (empty, over the bound), and never
// a file that could not be read — so a caller that wrote the task to a file
// of its own (the Claude Code plugin's agents) leaves nothing behind. The read
// is the command's like everything after it: under --timeout and its signals,
// so a source that never ends (a pipe held open, a FIFO nobody writes) ends
// the command, exit 1, before anything is spawned. Plain craze new keeps its
// own rule: words or nothing, and stdin is never read.

// promptTextMax is the most a prompt may be, in bytes: the message queue's
// own bound (internal/agent's queueTextCap), which a prompt queued behind a
// running turn meets at the host — refused here first, before anything is
// spawned (TestPromptTextMaxIsTheQueuesBound holds the two together).
const promptTextMax = 32 << 10

// promptInput is --prompt-file and --remove-prompt-file.
type promptInput struct {
	file   string
	remove bool
}

// registerPromptInputFlags declares --prompt-file and --remove-prompt-file on
// cmd, into p.
func registerPromptInputFlags(cmd *cobra.Command, p *promptInput) {
	cmd.Flags().StringVar(&p.file, "prompt-file", "", "read the prompt from this file (- for stdin)")
	cmd.Flags().BoolVar(&p.remove, "remove-prompt-file", false, "delete --prompt-file's file once it has been read")
}

// check refuses what the flags say wrong on their own, before anything is
// read: words beside --prompt-file, a --prompt-file with no path, and
// --remove-prompt-file with no file to remove. what is the command, "craze
// send", as its usage errors start.
func (p *promptInput) check(cmd *cobra.Command, what string, words []string) error {
	fromFile := cmd.Flags().Changed("prompt-file")
	switch {
	case fromFile && p.file == "":
		return usagef("%s: --prompt-file needs a path (- for stdin)", what)
	case fromFile && len(words) > 0:
		return usagef("%s: give the prompt as words or as --prompt-file, not both", what)
	case p.remove && !fromFile:
		return usagef("%s: --remove-prompt-file needs --prompt-file", what)
	case p.remove && p.file == "-":
		return usagef("%s: --remove-prompt-file removes a file, not stdin", what)
	}
	return nil
}

// read is the prompt (the file's comment), its surrounding whitespace gone;
// check has passed. A file that cannot be read is a usage error too: the
// prompt the command was given is not there. A file --remove-prompt-file
// cannot delete is said on stderr, and the command goes on.
//
// A source is read on a goroutine of its own, under ctx — the command's work
// (promptJob.ctx: --timeout's end, and its first signal, which signalled
// says came): ended first, the read is given up — the goroutine left to the
// process's exit — and the command is exit 1, `timed out reading the prompt
// from stdin` or `interrupted while reading the prompt from <path>` (C3 review
// r1, 6).
func (p *promptInput) read(ctx context.Context, cmd *cobra.Command, what string, words []string, signalled func() bool) (string, error) {
	stdin := cmd.InOrStdin()
	var (
		source string
		readIt func() (string, error)
	)
	switch {
	case cmd.Flags().Changed("prompt-file") && p.file == "-":
		source, readIt = "stdin", func() (string, error) { return readPrompt(stdin) }
	case cmd.Flags().Changed("prompt-file"):
		source, readIt = sanitizeLine(p.file), func() (string, error) { return readPromptFile(p.file) }
	case len(words) > 0:
		return checkPrompt(what, strings.Join(words, " "), nil)
	case !stdinIsTTY(stdin):
		source, readIt = "stdin", func() (string, error) { return readPrompt(stdin) }
	default:
		return "", usagef("%s: needs a prompt: words, --prompt-file <path>, or stdin; see %s --help", what, cmd.CommandPath())
	}
	type read struct {
		text string
		err  error
	}
	got := make(chan read, 1)
	go func() {
		text, err := readIt()
		got <- read{text, err}
	}()
	promptReading()
	select {
	case r := <-got:
		if p.remove && p.file != "-" && (r.err == nil || errors.Is(r.err, errPromptTooLong)) {
			// Read: whatever it turns out to be, it is gone (P14). A file
			// that could not be read stays.
			if rerr := os.Remove(p.file); rerr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s: --remove-prompt-file: %s\n", what, sanitizeLine(rerr.Error()))
			}
		}
		return checkPrompt(what, r.text, r.err)
	case <-ctx.Done():
		if signalled() {
			return "", exitf(1, "%s: interrupted while reading the prompt from %s", what, source)
		}
		return "", exitf(1, "%s: timed out reading the prompt from %s", what, source)
	}
}

// promptReading sees each read of a prompt's source begin: a no-op, and the
// barrier a test sends its signal after when it means one during the read.
var promptReading = func() {}

// checkPrompt is a source's text and read error as the prompt (read's rules):
// the error a usage error, the text trimmed, empty or over the bound refused.
func checkPrompt(what, text string, err error) (string, error) {
	over := errors.Is(err, errPromptTooLong)
	if err != nil && !over {
		return "", usagef("%s: the prompt cannot be read: %s", what, sanitizeLine(err.Error()))
	}
	text = strings.TrimSpace(text)
	switch {
	case over, len(text) > promptTextMax:
		return "", usagef("%s: the prompt is over %d KiB; put the bulk of it in a file the task names", what, promptTextMax>>10)
	case text == "":
		return "", usagef("%s: the prompt is empty", what)
	}
	return text, nil
}

// readPromptFile is the prompt in the file at path, as readPrompt reads it.
func readPromptFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return readPrompt(f)
}

// promptReadMax bounds what is read of a prompt's source: a prompt is
// promptTextMax once trimmed, and a source past this is over it whatever
// whitespace surrounds it — read no further than that.
const promptReadMax = 1 << 20

// errPromptTooLong is a source longer than promptReadMax.
var errPromptTooLong = errors.New("the prompt is too long")

// readPrompt reads r to its end, or to one byte past promptReadMax: a source
// that long is errPromptTooLong, unread beyond it.
func readPrompt(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, promptReadMax+1))
	if err == nil && len(b) > promptReadMax {
		err = errPromptTooLong
	}
	return string(b), err
}
