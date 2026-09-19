package opencode

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/charliek/craze/internal/harness/tool"
)

type globTool struct {
	spec tool.Spec
	rg   *ripgrep
}

func newGlob(rg *ripgrep) (tool.Tool, error) {
	desc, err := description("glob", nil)
	if err != nil {
		return nil, err
	}
	// opencode's Parameters (glob.ts:10-15) as its JSON Schema renders them
	// (test/tool/__snapshots__/parameters.test.ts.snap).
	return &globTool{rg: rg, spec: tool.Spec{
		ID:          "glob",
		Description: desc,
		Parameters: map[string]any{
			"pattern": map[string]any{"type": "string", "description": "The glob pattern to match files against"},
			"path": map[string]any{"type": "string",
				"description": `The directory to search in. If not specified, the current working directory will be used. IMPORTANT: Omit this field to use the default directory. DO NOT enter "undefined" or "null" - simply omit it for the default behavior. Must be a valid directory path if provided.`},
		},
		Required: []string{"pattern"},
		Kind:     tool.KindSearch,
		ReadOnly: true,
		Parallel: true,
		// glob caps itself at 100 files and says so; opencode's shared
		// truncation skips a result that reports its own (tool.ts:131).
		Truncate: tool.None,
	}}, nil
}

func (g *globTool) Spec() tool.Spec { return g.spec }

func (g *globTool) Prepare(env tool.Env, c tool.Call) (tool.Prepared, error) {
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	pattern, _, err := a.str("pattern", true)
	if err != nil {
		return nil, err
	}
	path, _, err := a.str("path", false)
	if err != nil {
		return nil, err
	}
	// `params.path ?? ins.directory`, resolved against the workspace
	// (glob.ts:37-38): an empty path is the workspace. An empty pattern is
	// passed on, as opencode passes it, and rg matches every file with it.
	abs := env.Resolve(path)
	return &globCall{rg: g.rg, pattern: pattern, abs: abs, title: searchTitle(env, pattern, path, abs)}, nil
}

type globCall struct {
	rg      *ripgrep
	pattern string
	abs     string // the directory to search, resolved; not yet checked
	title   string
}

func (c *globCall) Request() tool.Request {
	return tool.Request{Title: c.title, Paths: []string{c.abs}}
}

// globArgs is rg's argv, less rg itself, for pattern (ripgrep.ts:155-168):
// no --hidden, so rg enters a hidden directory only when pattern matches it.
// --null is craze's: each path ends with NUL rather than "\n", the one byte
// a file name cannot hold, so a name with a newline in it is still one
// path (NOTICE).
func globArgs(pattern string) []string {
	return []string{rgNoConfig, "--files", "--null", "--glob=" + pattern, rgNoGit, "."}
}

func (c *globCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if ctx.Err() != nil {
		return tool.Result{Text: tool.AbortedText, IsError: true, Class: tool.ClassAborted}
	}
	bin, err := c.rg.find()
	if err != nil {
		return rgMissing()
	}
	switch info, err := os.Stat(c.abs); {
	case missing(err):
		return errorResult(fail(tool.ClassNotFound, "glob path does not exist: "+c.abs))
	case err != nil:
		return errorResult(err)
	case !info.IsDir():
		// opencode's words (glob.ts:42), for anything that is not a
		// directory, where opencode checks for a file only.
		return errorResult(fail(tool.ClassToolError, "glob path must be a directory: "+c.abs))
	}

	var files []string
	end, readErr, runErr := c.rg.run(ctx, env, bin, c.abs, globArgs(c.pattern), 0, func(rec []byte) (bool, error) {
		// Joined onto the path as the call named it (glob.ts:53).
		file, err := rgPath(c.abs, string(rec))
		if err != nil {
			return false, err
		}
		files = append(files, file)
		return len(files) <= searchLimit, nil
	})
	if res, ok := searchOutcome(ctx, "glob", end, readErr, runErr, c.rg.timeout); !ok {
		return res
	}
	return tool.Result{Text: globText(files)}
}

// globText is glob's answer (glob.ts:51-62): the files, one absolute path a
// line (shownPath), in rg's order. When rg listed more than searchLimit, the
// first searchLimit are shown and the text says so; opencode says so at
// exactly searchLimit too (NOTICE).
func globText(files []string) string {
	if len(files) == 0 {
		return "No files found"
	}
	more := len(files) > searchLimit
	shown := make([]string, 0, min(len(files), searchLimit))
	for _, f := range files[:min(len(files), searchLimit)] {
		shown = append(shown, shownPath(f))
	}
	text := strings.Join(shown, "\n")
	if more {
		text += "\n\n(Results are truncated: showing first " + strconv.Itoa(searchLimit) +
			" results. Consider using a more specific path or pattern.)"
	}
	return text
}
