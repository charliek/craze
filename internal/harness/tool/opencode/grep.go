package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/tool"
)

// maxMatchLength is the most of a matching line grep shows, in runes, before
// "...": opencode's 2000 (ripgrep.ts:267-270), which counts UTF-16 units
// (NOTICE).
const maxMatchLength = 2000

type grepTool struct {
	spec tool.Spec
	rg   *ripgrep
}

func newGrep(rg *ripgrep) (tool.Tool, error) {
	desc, err := description("grep", nil)
	if err != nil {
		return nil, err
	}
	// opencode's Parameters (grep.ts:10-18) as its JSON Schema renders them
	// (test/tool/__snapshots__/parameters.test.ts.snap).
	return &grepTool{rg: rg, spec: tool.Spec{
		ID:          "grep",
		Description: desc,
		Parameters: map[string]any{
			"pattern": map[string]any{"type": "string", "description": "The regex pattern to search for in file contents"},
			"path": map[string]any{"type": "string",
				"description": "The directory to search in. Defaults to the current working directory."},
			"include": map[string]any{"type": "string",
				"description": `File pattern to include in the search (e.g. "*.js", "*.{ts,tsx}")`},
		},
		Required: []string{"pattern"},
		Kind:     tool.KindSearch,
		ReadOnly: true,
		Parallel: true,
		// grep caps itself at 100 matches and says so; opencode's shared
		// truncation skips a result that reports its own (tool.ts:131).
		Truncate: tool.None,
	}}, nil
}

func (g *grepTool) Spec() tool.Spec { return g.spec }

func (g *grepTool) Prepare(env tool.Env, c tool.Call) (tool.Prepared, error) {
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	pattern, _, err := a.str("pattern", true)
	if err != nil {
		return nil, err
	}
	// opencode fails an empty pattern when the call runs (grep.ts:36-38);
	// craze refuses it here, before the gate (NOTICE).
	if pattern == "" {
		return nil, errors.New("pattern is required")
	}
	path, _, err := a.str("path", false)
	if err != nil {
		return nil, err
	}
	include, _, err := a.str("include", false)
	if err != nil {
		return nil, err
	}
	// `params.path ?? ins.directory`, joined to the workspace when relative
	// (grep.ts:52-54): an empty path is the workspace.
	abs := env.Resolve(path)
	return &grepCall{rg: g.rg, pattern: pattern, include: include, abs: abs, title: searchTitle(env, pattern, path, abs)}, nil
}

// searchTitle is a search's card title: the pattern, and the path it names
// relative to the workspace, when it names one.
func searchTitle(env tool.Env, pattern, path, abs string) string {
	if path == "" {
		return pattern
	}
	return pattern + " in " + title(env, abs)
}

type grepCall struct {
	rg               *ripgrep
	pattern, include string
	abs              string // the path to search, resolved; not yet checked
	title            string
}

func (c *grepCall) Request() tool.Request {
	return tool.Request{Title: c.title, Paths: []string{c.abs}}
}

// grepArgs is rg's argv, less rg itself, for a search of target — "." or a
// file's name — for pattern (ripgrep.ts:218-231).
func grepArgs(pattern, include, target string) []string {
	args := []string{rgNoConfig, "--json", "--hidden", "--no-messages"}
	if include != "" {
		args = append(args, "--glob="+include)
	}
	return append(args, rgNoGit, "--", pattern, target)
}

// grepMatch is one matching line.
type grepMatch struct {
	path string // absolute, under the path the call named
	line int64
	text string
}

func (c *grepCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if ctx.Err() != nil {
		return tool.Result{Text: tool.AbortedText, IsError: true, Class: tool.ClassAborted}
	}
	bin, err := c.rg.find()
	if err != nil {
		return rgMissing()
	}
	// rg searches from the directory, or from a file's directory for that
	// file alone, and the rows it prints are joined back onto the path as
	// the call named it, so a symlink the call went through stays in them
	// (grep.ts:58-75).
	info, err := os.Stat(c.abs)
	switch {
	case missing(err):
		return errorResult(fail(tool.ClassNotFound, "grep path does not exist: "+c.abs))
	case err != nil:
		return errorResult(err)
	}
	dir, target := c.abs, "."
	switch {
	case info.IsDir():
	case info.Mode().IsRegular():
		dir, target = filepath.Dir(c.abs), filepath.Base(c.abs)
	default:
		// A FIFO named outright would be read until a writer came.
		return errorResult(fail(tool.ClassToolError, "Path is not a regular file or a directory: "+c.abs))
	}

	var rows []grepMatch
	end, readErr, runErr := c.rg.run(ctx, env, bin, dir, grepArgs(c.pattern, c.include, target), '\n', func(line []byte) (bool, error) {
		m, ok, err := parseMatch(env, dir, line)
		if err != nil {
			return false, err
		}
		if ok {
			rows = append(rows, m)
		}
		// One more than the limit, to know there are more (ripgrep.ts:126).
		return len(rows) <= searchLimit, nil
	})
	if res, ok := searchOutcome(ctx, "grep", end, readErr, runErr, c.rg.timeout); !ok {
		return res
	}
	return tool.Result{Text: grepText(rows)}
}

// grepText is grep's answer (grep.ts:64-101): a count, then the matches
// grouped by file (a header, shownPath), in rg's order, as
// "  Line n: text". When rg had more
// than searchLimit, the first searchLimit are shown and the text says there
// are more. opencode says so at exactly searchLimit too (NOTICE).
func grepText(rows []grepMatch) string {
	if len(rows) == 0 {
		return "No files found"
	}
	more := len(rows) > searchLimit
	rows = rows[:min(len(rows), searchLimit)]
	out := make([]string, 0, 2*len(rows)+3)
	head := fmt.Sprintf("Found %d matches", len(rows))
	if more {
		head += " (more matches available)"
	}
	out = append(out, head)
	current := ""
	for _, m := range rows {
		if m.path != current {
			if current != "" {
				out = append(out, "")
			}
			current = m.path
			out = append(out, shownPath(m.path)+":")
		}
		out = append(out, "  Line "+strconv.FormatInt(m.line, 10)+": "+m.text)
	}
	if more {
		out = append(out, "", "(Results truncated. Consider using a more specific path or pattern.)")
	}
	return strings.Join(out, "\n")
}

// rgData is a string in rg's JSON output: text when it is valid UTF-8,
// else its bytes, base64-encoded.
type rgData struct {
	Text  *string `json:"text"`
	Bytes *string `json:"bytes"`
}

// value returns the string; ok is false when it holds neither form.
func (d rgData) value() (s string, ok bool) {
	switch {
	case d.Text != nil:
		return *d.Text, true
	case d.Bytes != nil:
		b, err := base64.StdEncoding.DecodeString(*d.Bytes)
		return string(b), err == nil
	}
	return "", false
}

// The failures of a line of rg's JSON output, in opencode's words
// (ripgrep.ts:237, 249).
var (
	errInvalidJSON  = fail(tool.ClassToolError, "Invalid ripgrep JSON output")
	errInvalidMatch = fail(tool.ClassToolError, "Invalid ripgrep match output")
)

// parseMatch reads one line of `rg --json` as opencode does
// (ripgrep.ts:232-252, 254-277): a record that is not JSON fails the
// search, one that is not a match is skipped, and a match must have a
// path, a line and a positive line number. The path is joined onto dir,
// where rg ran (rgPath). It is a JSON string, so a newline in a name is
// escaped in it, not a record's end, and the path cannot leave dir; rgPath
// fails the search if it ever did. The line loses its line ending, and is
// redacted and then cut to maxMatchLength runes (preview).
//
// opencode fails the whole search on a line or path that is not valid
// UTF-8, which rg reports as base64 bytes; craze decodes it (NOTICE).
func parseMatch(env tool.Env, dir string, line []byte) (m grepMatch, ok bool, err error) {
	if !json.Valid(line) {
		return grepMatch{}, false, errInvalidJSON
	}
	var rec struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "match" {
		return grepMatch{}, false, nil
	}
	var data struct {
		Path       rgData `json:"path"`
		Lines      rgData `json:"lines"`
		LineNumber *int64 `json:"line_number"`
	}
	if json.Unmarshal(rec.Data, &data) != nil || data.LineNumber == nil || *data.LineNumber < 1 {
		return grepMatch{}, false, errInvalidMatch
	}
	path, okPath := data.Path.value()
	text, okText := data.Lines.value()
	if !okPath || !okText {
		return grepMatch{}, false, errInvalidMatch
	}
	if data.Lines.Text == nil {
		text = validUTF8([]byte(text))
	}
	abs, err := rgPath(dir, path)
	if err != nil {
		return grepMatch{}, false, err
	}
	return grepMatch{path: abs, line: *data.LineNumber, text: preview(env, text)}, true, nil
}

// preview is a matching line as grep shows it: without its line ending,
// which rg's JSON keeps and opencode shows (NOTICE); with craze's provider
// keys redacted, before the cut, so a key the cut would halve leaves no
// half behind; and cut to maxMatchLength runes, then "...", when longer.
func preview(env tool.Env, s string) string {
	s = strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
	s = env.Redactor.String(s)
	if utf8.RuneCountInString(s) <= maxMatchLength {
		return s
	}
	i := 0
	for range maxMatchLength {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i] + "..."
}
