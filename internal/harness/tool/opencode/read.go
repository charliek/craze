package opencode

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/tool"
)

// read's limits, opencode's (read.ts:13-18).
const (
	defaultReadLimit = 2000
	maxLineLength    = 2000 // runes; see NOTICE
	maxLineSuffix    = "... (line truncated to 2000 chars)"
	maxReadBytes     = 50 * 1024
	maxReadLabel     = "50 KB"
	sampleBytes      = 4096
)

// keepBytes is as much of one line as read holds. A line longer than this
// has more than maxLineLength runes, since a rune is at most four bytes, and
// the first maxLineLength runes of any line lie inside it; so a line of any
// length costs read no more than this.
const keepBytes = 4 * maxLineLength

// A directory listing's bounds. opencode reads a whole directory before it
// slices it, so a huge directory, or an endless one on a FUSE mount, costs
// unbounded memory and never sees a cancel. read takes the entries dirChunk
// at a time, checking for a cancel between chunks, and stops at
// maxDirEntries, saying so (NOTICE). Variables so tests can shrink them.
var (
	dirChunk      = 1000
	maxDirEntries = 100_000
)

// dirCapNote ends a listing that stopped at maxDirEntries.
const dirCapNote = "(Listing stopped at %d entries: the directory has more, and only the first %d it returned are listed. Use the bash tool to see the rest.)"

type readTool struct{ spec tool.Spec }

func newRead() (tool.Tool, error) {
	desc, err := description("read", nil)
	if err != nil {
		return nil, err
	}
	// opencode's Parameters (read.ts:28-36) as its JSON Schema renders them
	// (test/tool/__snapshots__/parameters.test.ts.snap): offset and limit are
	// NonNegativeInt, so zero is allowed — offset 0 reads from line 1.
	return &readTool{spec: tool.Spec{
		ID:          "read",
		Description: desc,
		Parameters: map[string]any{
			"filePath": map[string]any{"type": "string", "description": "The absolute path to the file or directory to read"},
			"offset": map[string]any{"type": "integer", "minimum": 0, "maximum": maxSafeInteger,
				"description": "The line number to start reading from (1-indexed)"},
			"limit": map[string]any{"type": "integer", "minimum": 0, "maximum": maxSafeInteger,
				"description": "The maximum number of lines to read (defaults to 2000)"},
		},
		Required: []string{"filePath"},
		Kind:     tool.KindRead,
		ReadOnly: true,
		Parallel: true,
		// read caps itself, as opencode's does, and says so in its own words.
		Truncate: tool.None,
	}}, nil
}

func (r *readTool) Spec() tool.Spec { return r.spec }

func (r *readTool) Prepare(env tool.Env, c tool.Call) (tool.Prepared, error) {
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	path, _, err := a.str("filePath", true)
	if err != nil {
		return nil, err
	}
	offset, _, err := a.integer("offset", 0)
	if err != nil {
		return nil, err
	}
	limit, ok, err := a.integer("limit", 0)
	if err != nil {
		return nil, err
	}
	if !ok {
		limit = defaultReadLimit
	}
	if offset == 0 {
		offset = 1 // `params.offset || 1`
	}
	abs := env.Resolve(path)
	return &readCall{abs: abs, title: title(env, abs), offset: int(offset), limit: int(limit)}, nil
}

type readCall struct {
	abs, title    string
	offset, limit int
}

func (c *readCall) Request() tool.Request {
	return tool.Request{Title: c.title, Paths: []string{c.abs}}
}

func (c *readCall) Run(ctx context.Context, env tool.Env) tool.Result {
	res, err := c.run(ctx, env)
	if err != nil {
		return errorResult(err)
	}
	return res
}

func (c *readCall) run(ctx context.Context, env tool.Env) (tool.Result, error) {
	real, err := realPath(c.abs)
	if err != nil {
		return tool.Result{}, err
	}
	f, info, err := openFile(real)
	switch {
	case missing(err):
		return tool.Result{}, c.miss()
	case err != nil:
		return tool.Result{}, err
	}
	defer func() { _ = f.Close() }()
	switch {
	case isCredentials(env, real, info):
		return tool.Result{}, fail(tool.ClassToolError, credentialsText)
	case info.IsDir():
		return c.list(ctx, f, real)
	case !info.Mode().IsRegular():
		// A FIFO, a device or a socket: opencode would block on it or read
		// it forever (plan 019 §3.9).
		return tool.Result{}, fail(tool.ClassToolError, "Path is not a regular file or a directory: "+c.abs)
	}
	return c.file(ctx, f, info.Size())
}

// miss is opencode's answer for a path that does not exist: up to three
// names in its directory that contain, or are contained in, the missing
// name, ignoring case (read.ts:76-99).
func (c *readCall) miss() error {
	dir, base := filepath.Dir(c.abs), strings.ToLower(filepath.Base(c.abs))
	var items []string
	entries, _ := os.ReadDir(dir) // no directory, no suggestions
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if strings.Contains(name, base) || strings.Contains(base, name) {
			items = append(items, filepath.Join(dir, e.Name()))
			if len(items) == 3 {
				break
			}
		}
	}
	if len(items) > 0 {
		return fail(tool.ClassNotFound, "File not found: "+c.abs+"\n\nDid you mean one of these?\n"+strings.Join(items, "\n"))
	}
	return fail(tool.ClassNotFound, "File not found: "+c.abs)
}

// list is read's directory mode (read.ts:264-298): the entries sorted, a
// directory (or a symlink to one) with a trailing slash, sliced by offset
// and limit.
//
// The entries are taken a chunk at a time, with a cancel checked between
// chunks, and no more than maxDirEntries are kept: a directory of millions
// of names, or one on a mount that never ends, must cost neither the memory
// of all of them nor a turn that cannot be stopped. A listing that stopped
// says so, and what it shows is then the first maxDirEntries the directory
// returned, sorted among themselves.
func (c *readCall) list(ctx context.Context, dir *os.File, real string) (tool.Result, error) {
	var items []string
	capped := false
	for !capped {
		if err := ctx.Err(); err != nil {
			return tool.Result{}, err
		}
		entries, err := dir.ReadDir(dirChunk)
		for _, e := range entries {
			if len(items) == maxDirEntries {
				capped = true // one more than the cap exists
				break
			}
			items = append(items, entryName(real, e))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return tool.Result{}, err
		}
	}
	// Byte order, not opencode's localeCompare: see NOTICE.
	slices.Sort(items)

	start := min(c.offset-1, len(items))
	sliced := items[start : start+min(c.limit, len(items)-start)]
	truncated := start+len(sliced) < len(items)
	footer := fmt.Sprintf("\n(%d entries)", len(items))
	if truncated {
		footer = fmt.Sprintf("\n(Showing %d of %d entries. Use 'offset' parameter to read beyond entry %d)",
			len(sliced), len(items), c.offset+len(sliced))
	}
	if capped {
		note := fmt.Sprintf(dirCapNote, len(items), len(items))
		if truncated {
			footer += "\n" + note
		} else {
			footer = "\n" + note
		}
	}
	content := strings.Join(sliced, "\n")
	res := tool.Result{
		Text: strings.Join([]string{
			"<path>" + c.abs + "</path>", "<type>directory</type>", "<entries>", content, footer, "</entries>",
		}, "\n"),
		Content: content,
	}
	if truncated || capped {
		total := len(items)
		if capped {
			total++ // the one that was seen and not kept: a lower bound
		}
		res.Trunc = tool.Truncation{
			KeptBytes: len(content), TotalBytes: len(strings.Join(items, "\n")),
			KeptLines: len(sliced), TotalLines: total,
		}
	}
	return res, nil
}

// entryName is one directory entry as a listing shows it: its name, with a
// trailing slash for a directory or a symlink to one.
func entryName(dir string, e fs.DirEntry) string {
	name := e.Name()
	switch {
	case e.IsDir():
		return name + "/"
	case e.Type()&fs.ModeSymlink != 0:
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && st.IsDir() {
			return name + "/"
		}
	}
	return name
}

// file is read's file mode (read.ts:300-377) for a regular file of
// fileSize bytes: images, PDFs and binary files are refused, and the rest is
// shown as numbered lines from offset, at most limit of them and
// maxReadBytes of text, each cut to maxLineLength.
func (c *readCall) file(ctx context.Context, f *os.File, fileSize int64) (tool.Result, error) {
	sample := make([]byte, min(fileSize, sampleBytes))
	n, err := f.ReadAt(sample, 0)
	if err != nil && err != io.EOF {
		return tool.Result{}, err
	}
	sample = sample[:n]
	switch media(c.abs, sample) {
	case mediaImage:
		// opencode attaches images; craze cannot yet (plan 019 §3.2).
		return tool.Result{}, fail(tool.ClassToolError, "Cannot read image file yet: "+c.abs)
	case mediaPDF:
		// Nor PDFs, which opencode attaches too and has no refusal for.
		return tool.Result{}, fail(tool.ClassToolError, "Cannot read binary file: "+c.abs)
	}
	if binary(c.abs, sample) {
		return tool.Result{}, fail(tool.ClassToolError, "Cannot read binary file: "+c.abs)
	}

	lr := newLineReader(f)
	start := c.offset - 1
	var raw []string
	kept, count := 0, 0
	cut, more := false, false
	for {
		if count%4096 == 0 && ctx.Err() != nil {
			return tool.Result{}, ctx.Err()
		}
		line, long, ok, err := lr.next(ctx)
		if err != nil {
			return tool.Result{}, err
		}
		if !ok {
			break
		}
		count++
		if count <= start {
			continue
		}
		if len(raw) >= c.limit {
			more = true // and keep counting, for the total
			continue
		}
		text := clip(line, long)
		add := len(text)
		if len(raw) > 0 {
			add++ // the newline joining it to the line before
		}
		if kept+add <= maxReadBytes {
			raw = append(raw, text)
			kept += add
			continue
		}
		// Stop reading at the cap: count is then the lines read so far.
		cut, more = true, true
		break
	}
	if count < c.offset && (count != 0 || c.offset != 1) {
		return tool.Result{}, fail(tool.ClassToolError,
			fmt.Sprintf("Offset %d is out of range for this file (%d lines)", c.offset, count))
	}

	var b strings.Builder
	b.WriteString("<path>" + c.abs + "</path>\n<type>file</type>\n<content>\n")
	for i, line := range raw {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strconv.Itoa(i + c.offset))
		b.WriteString(": ")
		b.WriteString(line)
	}
	last := c.offset + len(raw) - 1
	switch {
	case cut:
		fmt.Fprintf(&b, "\n\n(Output capped at %s. Showing lines %d-%d. Use offset=%d to continue.)", maxReadLabel, c.offset, last, last+1)
	case more:
		fmt.Fprintf(&b, "\n\n(Showing lines %d-%d of %d. Use offset=%d to continue.)", c.offset, last, count, last+1)
	default:
		fmt.Fprintf(&b, "\n\n(End of file - total %d lines)", count)
	}
	b.WriteString("\n</content>")

	res := tool.Result{Text: b.String(), Content: strings.Join(raw, "\n")}
	if more {
		res.Trunc = tool.Truncation{KeptBytes: kept, TotalBytes: int(fileSize), KeptLines: len(raw), TotalLines: count}
	}
	return res, nil
}

type mediaKind int

const (
	mediaNone mediaKind = iota
	mediaImage
	mediaPDF
)

// media is what opencode would have attached rather than read: its
// sniffAttachmentMime (util/media.ts) by magic bytes, falling back to the
// extension's type, and then read.ts's SUPPORTED_IMAGE_MIMES (jpeg, png,
// gif, webp) and isPdfAttachment. Magic bytes win outright, so a BMP is not
// an image here even when named .png: it goes on to the binary check.
func media(path string, sample []byte) mediaKind {
	switch {
	case bytes.HasPrefix(sample, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}),
		bytes.HasPrefix(sample, []byte{0xff, 0xd8, 0xff}),
		bytes.HasPrefix(sample, []byte("GIF8")),
		bytes.HasPrefix(sample, []byte("RIFF")) && len(sample) >= 12 && bytes.HasPrefix(sample[8:], []byte("WEBP")):
		return mediaImage
	case bytes.HasPrefix(sample, []byte("BM")):
		return mediaNone // image/bmp: not a type opencode attaches
	case bytes.HasPrefix(sample, []byte("%PDF-")):
		return mediaPDF
	}
	// mime-types' lookup, lower-cased, for the types that matter here
	// (mime-db 1.54.0, opencode's pin).
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "png", "jpg", "jpeg", "jpe", "gif", "webp":
		return mediaImage
	case "pdf":
		return mediaPDF
	}
	return mediaNone
}

// binaryExts are the extensions opencode refuses without looking
// (read.ts:184-213).
var binaryExts = []string{
	".zip", ".tar", ".gz", ".exe", ".dll", ".so", ".class", ".jar", ".war", ".7z",
	".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".odt", ".ods", ".odp",
	".bin", ".dat", ".obj", ".o", ".a", ".lib", ".wasm", ".pyc", ".pyo",
}

// binary is opencode's isBinaryFile (read.ts:182-227): a known binary
// extension, a NUL byte in the sample, or more than 30% of the sample
// non-printable (control bytes other than tab through carriage return).
func binary(path string, sample []byte) bool {
	if slices.Contains(binaryExts, strings.ToLower(filepath.Ext(path))) {
		return true
	}
	if len(sample) == 0 {
		return false
	}
	nonPrintable := 0
	for _, b := range sample {
		if b == 0 {
			return true
		}
		if b < 9 || (b > 13 && b < 32) {
			nonPrintable++
		}
	}
	return float64(nonPrintable)/float64(len(sample)) > 0.3
}

// lineReader splits a file into lines as opencode's read does — a UTF-8
// decode, then Effect's Stream.splitLines: a line ends at "\n" or "\r\n"; a
// lone "\r" stays in its line, except at the very end of the file, where it
// is dropped; a last line with nothing in it is no line; and a byte order
// mark at the start of the file is dropped, as TextDecoder drops it. It
// holds at most keepBytes of any one line.
type lineReader struct {
	r    *bufio.Reader
	line []byte
}

func newLineReader(f io.Reader) *lineReader {
	r := bufio.NewReaderSize(f, 64*1024)
	if head, _ := r.Peek(len(bom)); string(head) == bom {
		_, _ = r.Discard(len(bom))
	}
	return &lineReader{r: r}
}

// next returns the next line — its first keepBytes bytes, or all of it when
// it is no longer — and whether it was longer. ok is false at the end.
//
// A line longer than the reader's buffer arrives in chunks: what is past
// keepBytes is counted and dropped, never held, and a cancel is checked
// between chunks. So one line of any length — a minified bundle, a file
// with no newline at all — costs neither memory nor a turn that cannot be
// stopped.
func (lr *lineReader) next(ctx context.Context) (line []byte, long, ok bool, err error) {
	lr.line = lr.line[:0]
	n := 0        // the line's length so far, its ending not counted
	var last byte // the line's last byte so far, when n > 0
	for {
		chunk, err := lr.r.ReadSlice('\n')
		ended := err == nil
		if ended {
			chunk = chunk[:len(chunk)-1]
		}
		if len(chunk) > 0 {
			// One byte more than keepBytes, so that dropping a "\r"
			// ending below still leaves keepBytes.
			if room := keepBytes + 1 - len(lr.line); room > 0 {
				lr.line = append(lr.line, chunk[:min(room, len(chunk))]...)
			}
			n += len(chunk)
			last = chunk[len(chunk)-1]
		}
		switch {
		case ended, err == io.EOF:
			if n > 0 && last == '\r' {
				n--
				lr.line = lr.line[:min(len(lr.line), n)]
			}
			if !ended && n == 0 {
				return nil, false, false, nil
			}
			long = n > keepBytes
			return lr.line[:min(len(lr.line), keepBytes)], long, true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			if err := ctx.Err(); err != nil {
				return nil, false, false, err
			}
			continue
		default:
			return nil, false, false, err
		}
	}
}

// clip is a line as read shows it: decoded as UTF-8, with each invalid
// byte as U+FFFD, and cut to maxLineLength runes with opencode's suffix.
// long says the line had more bytes than b holds, and so more runes than
// the cut.
func clip(b []byte, long bool) string {
	s := validUTF8(b)
	if !long && utf8.RuneCountInString(s) <= maxLineLength {
		return s
	}
	i := 0
	for range maxLineLength {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i] + maxLineSuffix
}

// validUTF8 returns b as a string with each byte that is not part of valid
// UTF-8 replaced by U+FFFD, much as TextDecoder replaces them.
func validUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			sb.WriteString("\uFFFD")
		} else {
			sb.Write(b[:size])
		}
		b = b[size:]
	}
	return sb.String()
}
