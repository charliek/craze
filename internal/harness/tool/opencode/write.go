package opencode

import (
	"context"

	"github.com/charliek/craze/internal/harness/tool"
)

// wroteText is opencode's result for a write (write.ts:74), less the LSP
// diagnostics it appends: craze has no language servers.
const wroteText = "Wrote file successfully."

// maxWriteBytes is the largest existing file a write loads. write needs the
// old content only for the card's diff and for replace's content check, so a
// file over the cap is replaced without being read: no diff, and the change
// check on the inode, size and modification time alone (NOTICE). Overwriting
// a multi-gigabyte log must not cost its size in memory, twice. It is edit's
// cap, for the same reason, and a variable so tests can shrink it.
var maxWriteBytes int64 = maxEditBytes

type writeTool struct{ spec tool.Spec }

func newWrite() (tool.Tool, error) {
	desc, err := description("write", nil)
	if err != nil {
		return nil, err
	}
	// opencode's Parameters (write.ts:20-25), as its JSON Schema renders them.
	return &writeTool{spec: tool.Spec{
		ID:          "write",
		Description: desc,
		Parameters: map[string]any{
			"content":  map[string]any{"type": "string", "description": "The content to write to the file"},
			"filePath": map[string]any{"type": "string", "description": "The absolute path to the file to write (must be absolute, not relative)"},
		},
		Required: []string{"content", "filePath"},
		Kind:     tool.KindEdit,
		// Not parallel: Fantasy runs it one at a time among the tools that
		// are not, and the path lock orders it against any call on the
		// same file.
		Truncate: tool.Head,
	}}, nil
}

func (w *writeTool) Spec() tool.Spec { return w.spec }

func (w *writeTool) Prepare(env tool.Env, c tool.Call) (tool.Prepared, error) {
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	content, _, err := a.str("content", true)
	if err != nil {
		return nil, err
	}
	path, _, err := a.str("filePath", true)
	if err != nil {
		return nil, err
	}
	abs := env.Resolve(path)
	return &writeCall{abs: abs, real: targetPath(abs), title: title(env, abs), content: content}, nil
}

type writeCall struct {
	abs, real, title, content string
}

// Request names the file the write will really land on in Targets — resolved
// here, where Run resolves it again under the path lock — so the gate judges
// the file rather than the spelling (plan 023 §3.1).
func (c *writeCall) Request() tool.Request {
	return tool.Request{Title: c.title, Paths: []string{c.abs}, Targets: []string{c.real}}
}

// Run replaces the file with the content, keeping a byte order mark the
// file had or the content brings, as opencode does (write.ts:46-64).
func (c *writeCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if err := refuseMarker(c.content); err != nil {
		return errorResult(err)
	}
	t, err := openTarget(ctx, env, c.abs)
	if err != nil {
		return errorResult(err)
	}
	defer t.release()

	// A file over the cap is replaced unread: its byte order mark is still
	// kept, but there is no old content to diff against or to check.
	var old string
	var oldBOM, loaded bool
	if t.Size <= maxWriteBytes {
		source, err := t.read()
		if err != nil {
			return errorResult(err)
		}
		old, oldBOM = splitBOM(string(source))
		loaded = true
	} else if oldBOM, err = t.startsWithBOM(); err != nil {
		return errorResult(err)
	}

	next, nextBOM := splitBOM(c.content)
	if err := t.replace(ctx, []byte(joinBOM(next, oldBOM || nextBOM))); err != nil {
		return errorResult(err)
	}
	res := tool.Result{Text: wroteText}
	if loaded {
		res.Edits = []tool.FileEdit{{Path: c.abs, Old: old, New: next}}
	}
	return res
}
