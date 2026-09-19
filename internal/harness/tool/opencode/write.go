package opencode

import (
	"context"

	"github.com/charliek/craze/internal/harness/tool"
)

// wroteText is opencode's result for a write (write.ts:74), less the LSP
// diagnostics it appends: craze has no language servers.
const wroteText = "Wrote file successfully."

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
	return &writeCall{abs: abs, title: title(env, abs), content: content}, nil
}

type writeCall struct {
	abs, title, content string
}

func (c *writeCall) Request() tool.Request {
	return tool.Request{Title: c.title, Paths: []string{c.abs}}
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
	source, err := t.read()
	if err != nil {
		return errorResult(err)
	}
	old, oldBOM := splitBOM(string(source))
	next, nextBOM := splitBOM(c.content)
	if err := t.replace(ctx, []byte(joinBOM(next, oldBOM || nextBOM))); err != nil {
		return errorResult(err)
	}
	return tool.Result{Text: wroteText, Edits: []tool.FileEdit{{Path: c.abs, Old: old, New: next}}}
}
