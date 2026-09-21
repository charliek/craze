package opencode

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/tool"
)

const (
	// editedText is opencode's result for an edit (edit.ts:196), less the
	// LSP diagnostics it appends: craze has no language servers.
	editedText = "Edit applied successfully."

	// maxEditBytes is the largest file edit searches (plan 019 §3.9), and
	// the largest oldString or newString it takes. The fuzzy replacers are
	// quadratic in a file's lines and Levenshtein in a line's length, and
	// both sides of every comparison come from the file or from oldString;
	// anything larger is refused before any matching.
	maxEditBytes = 5 << 20
	tooLargeText = "File too large to edit; use bash"
	argsTooLarge = "oldString or newString is larger than 5 MiB, too large to edit; use write or bash"

	// maxEditResultBytes bounds what an edit may write: a file and a
	// newString at their caps, replaced once. Only replaceAll can go past
	// it, by multiplying newString.
	maxEditResultBytes = 2 * maxEditBytes

	// invalidOldText refuses an oldString holding U+FFFD in a file that is
	// not valid UTF-8, where it cannot be told from the U+FFFD read shows
	// for an invalid byte.
	invalidOldText = "The file contains bytes that are not valid UTF-8, which read shows as U+FFFD; an oldString containing U+FFFD cannot match them exactly. Use bash to edit this file."
)

type editTool struct{ spec tool.Spec }

func newEdit() (tool.Tool, error) {
	desc, err := description("edit", nil)
	if err != nil {
		return nil, err
	}
	// opencode's Parameters (edit.ts:47-56), as its JSON Schema renders them
	// (test/tool/__snapshots__/parameters.test.ts.snap).
	return &editTool{spec: tool.Spec{
		ID:          "edit",
		Description: desc,
		Parameters: map[string]any{
			"filePath":   map[string]any{"type": "string", "description": "The absolute path to the file to modify"},
			"oldString":  map[string]any{"type": "string", "description": "The text to replace"},
			"newString":  map[string]any{"type": "string", "description": "The text to replace it with (must be different from oldString)"},
			"replaceAll": map[string]any{"type": "boolean", "description": "Replace all occurrences of oldString (default false)"},
		},
		Required: []string{"filePath", "oldString", "newString"},
		Kind:     tool.KindEdit,
		// Not parallel, as for write; the path lock orders it against any
		// call on the same file.
		Truncate: tool.Head,
	}}, nil
}

func (e *editTool) Spec() tool.Spec { return e.spec }

func (e *editTool) Prepare(env tool.Env, c tool.Call) (tool.Prepared, error) {
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	path, _, err := a.str("filePath", true)
	if err != nil {
		return nil, err
	}
	if path == "" {
		// opencode's check (edit.ts:71-73); Resolve would make "" the
		// workspace itself.
		return nil, errors.New("filePath is required")
	}
	oldString, _, err := a.str("oldString", true)
	if err != nil {
		return nil, err
	}
	newString, _, err := a.str("newString", true)
	if err != nil {
		return nil, err
	}
	replaceAll, _, err := a.boolean("replaceAll")
	if err != nil {
		return nil, err
	}
	abs := env.Resolve(path)
	return &editCall{abs: abs, real: targetPath(abs), title: title(env, abs),
		oldString: oldString, newString: newString, replaceAll: replaceAll}, nil
}

type editCall struct {
	abs, real, title     string
	oldString, newString string
	replaceAll           bool
}

// Request names the file the edit will really land on in Targets; see write's.
func (c *editCall) Request() tool.Request {
	return tool.Request{Title: c.title, Paths: []string{c.abs}, Targets: []string{c.real}}
}

func (c *editCall) Run(ctx context.Context, env tool.Env) tool.Result {
	res, err := c.run(ctx, env)
	if err != nil {
		return errorResult(err)
	}
	return res
}

// run is opencode's execute (edit.ts:69-212) without the permission ask,
// the formatter, the LSP and the file-watcher events, which craze does not
// have. The file is read and replaced under craze's path lock, through
// openTarget, as write's is.
func (c *editCall) run(ctx context.Context, env tool.Env) (tool.Result, error) {
	if len(c.oldString) > maxEditBytes || len(c.newString) > maxEditBytes {
		return tool.Result{}, fail(tool.ClassOutputLimit, argsTooLarge)
	}
	// Refused before the file is touched (edit.ts:75-77).
	if c.oldString == c.newString {
		return tool.Result{}, fail(tool.ClassInvalidInput, identicalText)
	}
	if err := refuseMarker(c.oldString, c.newString); err != nil {
		return tool.Result{}, err
	}
	t, err := openTarget(ctx, env, c.abs)
	if err != nil {
		return tool.Result{}, err
	}
	defer t.release()

	if c.oldString == "" {
		return c.create(ctx, t)
	}
	if !t.Exists {
		return tool.Result{}, fail(tool.ClassNotFound, "File "+c.abs+" not found") // edit.ts:124
	}
	if t.Size > maxEditBytes {
		return tool.Result{}, fail(tool.ClassOutputLimit, tooLargeText)
	}
	source, err := t.read()
	if err != nil {
		return tool.Result{}, err
	}
	if len(source) > maxEditBytes {
		// It grew since it was opened; replace would refuse it anyway.
		return tool.Result{}, fail(tool.ClassOutputLimit, tooLargeText)
	}

	// opencode matches and writes in the file's own line ending: oldString
	// and newString are converted to it, and the content is left as it is
	// (edit.ts:129-131). The byte order mark is set aside first, as
	// Bom.readFile does, so it is neither matched nor lost.
	content, hadBOM := splitBOM(string(source))
	ending := detectLineEnding(content)
	oldString := convertToLineEnding(normalizeLineEndings(c.oldString), ending)
	newString := convertToLineEnding(normalizeLineEndings(c.newString), ending)

	// The match is made against the file as read showed it to the model,
	// each invalid byte as U+FFFD, so that which span is found, and whether
	// it is found once, is judged on the text the model copied from; splice
	// then replaces the same span in the bytes, and nothing else. In such a
	// file a U+FFFD in oldString may stand for an invalid byte, which no
	// JSON string can carry back, so it is refused before matching; a match
	// that takes in an invalid byte some other way is refused by splice.
	view := content
	if !utf8.ValidString(content) {
		if strings.ContainsRune(oldString, utf8.RuneError) {
			return tool.Result{}, fail(tool.ClassToolError, invalidOldText)
		}
		view = validUTF8([]byte(content))
	}
	search, at, err := match(ctx, view, oldString, newString, c.replaceAll)
	if err != nil {
		return tool.Result{}, err
	}
	replaced, err := splice(content, view, search, at, newString, c.replaceAll)
	if err != nil {
		return tool.Result{}, err
	}

	// The file keeps its BOM, and the rest is the edited text byte for
	// byte, with one exception, opencode's (edit.ts:133-135): a BOM that
	// newString brings to the very start of the text is taken off and kept
	// as the file's, so it is neither doubled nor lost. opencode takes a
	// BOM off the front whatever put it there, which drops a byte the edit
	// never touched from a file that begins with two BOMs, or when an edit
	// deletes what stood before a second one; here, only newString's.
	next, nextBOM := replaced, false
	if at == 0 && strings.HasPrefix(newString, bom) {
		next, nextBOM = splitBOM(replaced)
	}
	data := next
	if hadBOM || nextBOM {
		data = bom + next
	}
	if err := t.replace(ctx, []byte(data)); err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Text: editedText, Edits: []tool.FileEdit{{Path: c.abs, Old: content, New: next}}}, nil
}

// create is an edit with an empty oldString (edit.ts:90-121), which only
// ever makes a new file: newString is its content, with a BOM it brings
// kept, and missing parent directories are made.
func (c *editCall) create(ctx context.Context, t *target) (tool.Result, error) {
	if t.Exists {
		return tool.Result{}, fail(tool.ClassInvalidInput, emptyOldText)
	}
	next, withBOM := splitBOM(c.newString)
	if err := t.replace(ctx, []byte(joinBOM(next, withBOM))); err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Text: editedText, Edits: []tool.FileEdit{{Path: c.abs, New: next}}}, nil
}
