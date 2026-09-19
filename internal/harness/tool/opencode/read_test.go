package opencode

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// fileText is read's envelope around numbered lines and a footer.
func fileText(path, numbered, footer string) string {
	return "<path>" + path + "</path>\n<type>file</type>\n<content>\n" + numbered + "\n\n" + footer + "\n</content>"
}

// numbered is lines numbered from first, as read shows them.
func numbered(first int, lines ...string) string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = fmt.Sprintf("%d: %s", first+i, l)
	}
	return strings.Join(out, "\n")
}

// lines returns "<prefix><i>" for i in [from, to).
func lines(prefix string, from, to int) []string {
	var out []string
	for i := from; i < to; i++ {
		out = append(out, fmt.Sprintf("%s%d", prefix, i))
	}
	return out
}

// TestReadFile ports "allows reading absolute path inside project
// directory", "... file in subdirectory", "does not truncate small file" and
// "uses worktree-relative path for read permission" (as the call's title):
// the whole envelope, the card's content, a workspace-relative title. A
// relative path resolves against the workspace; one that leaves it and
// names nothing is not found (opencode's "reading relative path outside
// project", less the permission ask).
func TestReadFile(t *testing.T) {
	f := newFixture(t)
	put(t, f.path("test.txt"), "hello world")
	put(t, f.path("src/secret.ts"), "shh\n")

	req, res := f.call(t, "read", map[string]any{"filePath": f.path("test.txt")})
	if got, want := ok(t, res), fileText(f.path("test.txt"), "1: hello world", "(End of file - total 1 lines)"); got != want {
		t.Fatalf("text =\n%s\nwant\n%s", got, want)
	}
	if res.Content != "hello world" || res.Trunc != (tool.Truncation{}) {
		t.Fatalf("content %q, trunc %+v; want the text and no truncation", res.Content, res.Trunc)
	}
	if req.Title != "test.txt" || !reflect.DeepEqual(req.Paths, []string{f.path("test.txt")}) || !req.ReadOnly || req.Kind != tool.KindRead {
		t.Fatalf("request = %+v", req)
	}

	req, res = f.call(t, "read", map[string]any{"filePath": "src/secret.ts"})
	if !strings.Contains(ok(t, res), "<path>"+f.path("src/secret.ts")+"</path>") || !strings.Contains(res.Text, "1: shh") {
		t.Fatalf("relative read = %q", res.Text)
	}
	if req.Title != filepath.Join("src", "secret.ts") {
		t.Fatalf("title = %q, want the workspace-relative path", req.Title)
	}

	_, res = f.call(t, "read", map[string]any{"filePath": "../outside.txt"})
	failed(t, res, tool.ClassNotFound, "File not found: "+filepath.Join(filepath.Dir(f.env.Workspace), "outside.txt"))
}

// TestReadOutsideTheWorkspace: opencode asks for external_directory
// permission here; craze has no permission prompts (plan 019 decision 2),
// so the file is read. The title says where it is.
func TestReadOutsideTheWorkspace(t *testing.T) {
	f := newFixture(t)
	outer := filepath.Join(t.TempDir(), "secret.txt")
	put(t, outer, "secret data")
	req, res := f.call(t, "read", map[string]any{"filePath": outer})
	if !strings.Contains(ok(t, res), "1: secret data") {
		t.Fatalf("text = %q", res.Text)
	}
	if !strings.HasPrefix(req.Title, "..") {
		t.Fatalf("title = %q, want it relative to the workspace", req.Title)
	}
	// The negative control: the same name in the workspace does not exist.
	_, res = f.call(t, "read", map[string]any{"filePath": "secret.txt"})
	if res.Class != tool.ClassNotFound {
		t.Fatalf("result = %+v", res)
	}
}

// TestReadTruncation ports opencode's truncation cases: the byte cap stops
// reading early; limit shows a window and says where the next starts;
// offset starts it later.
func TestReadTruncation(t *testing.T) {
	f := newFixture(t)

	t.Run("truncates large file by bytes and stops streaming", func(t *testing.T) {
		line := strings.Repeat("x", 80)
		put(t, f.path("huge.txt"), strings.Repeat(line+"\n", 50_000))
		_, res := f.call(t, "read", map[string]any{"filePath": f.path("huge.txt")})
		// Each kept line costs 81 bytes (80 and a joining newline), so 632
		// fit under 50 KiB; the 633rd is where reading stops.
		want := "\n\n(Output capped at 50 KB. Showing lines 1-632. Use offset=633 to continue.)\n</content>"
		if !strings.HasSuffix(ok(t, res), want) {
			t.Fatalf("text ends %q, want %q", res.Text[max(0, len(res.Text)-120):], want)
		}
		if tr := res.Trunc; !tr.Truncated() || tr.KeptLines != 632 || tr.TotalLines != 633 || tr.KeptBytes != 632*81-1 || tr.TotalBytes != 50_000*81 {
			t.Fatalf("trunc = %+v: the read went on past the cap, or says it kept what it did not", tr)
		}
	})

	t.Run("truncates by line count when limit is specified", func(t *testing.T) {
		put(t, f.path("many-lines.txt"), strings.Join(lines("line", 0, 100), "\n"))
		_, res := f.call(t, "read", map[string]any{"filePath": f.path("many-lines.txt"), "limit": 10})
		want := fileText(f.path("many-lines.txt"), numbered(1, lines("line", 0, 10)...), "(Showing lines 1-10 of 100. Use offset=11 to continue.)")
		if ok(t, res) != want {
			t.Fatalf("text =\n%s\nwant\n%s", res.Text, want)
		}
		if res.Trunc.KeptLines != 10 || res.Trunc.TotalLines != 100 || strings.Contains(res.Content, "line10") {
			t.Fatalf("trunc %+v, content %q", res.Trunc, res.Content)
		}
		// The negative control: a limit past the end reads it all.
		_, res = f.call(t, "read", map[string]any{"filePath": f.path("many-lines.txt"), "limit": 1000})
		if !strings.HasSuffix(ok(t, res), "(End of file - total 100 lines)\n</content>") || res.Trunc.Truncated() {
			t.Fatalf("text ends %q", res.Text[len(res.Text)-60:])
		}
	})

	t.Run("respects offset parameter", func(t *testing.T) {
		put(t, f.path("offset.txt"), strings.Join(lines("line", 1, 21), "\n"))
		_, res := f.call(t, "read", map[string]any{"filePath": f.path("offset.txt"), "offset": 10, "limit": 5})
		want := fileText(f.path("offset.txt"), numbered(10, lines("line", 10, 15)...), "(Showing lines 10-14 of 20. Use offset=15 to continue.)")
		if ok(t, res) != want {
			t.Fatalf("text =\n%s\nwant\n%s", res.Text, want)
		}
		// Offset 0 is opencode's `offset || 1`: line 1.
		_, res = f.call(t, "read", map[string]any{"filePath": f.path("offset.txt"), "offset": 0, "limit": 1})
		if !strings.Contains(ok(t, res), "\n1: line1\n") {
			t.Fatalf("offset 0 = %q", res.Text)
		}
	})

	t.Run("throws when offset is beyond end of file", func(t *testing.T) {
		put(t, f.path("short.txt"), "line1\nline2\nline3")
		_, res := f.call(t, "read", map[string]any{"filePath": f.path("short.txt"), "offset": 4, "limit": 5})
		failed(t, res, tool.ClassToolError, "Offset 4 is out of range for this file (3 lines)")
		_, res = f.call(t, "read", map[string]any{"filePath": f.path("short.txt"), "offset": 3})
		if !strings.Contains(ok(t, res), "3: line3\n\n(End of file - total 3 lines)") {
			t.Fatalf("the last line = %q", res.Text)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		put(t, f.path("empty.txt"), "")
		_, res := f.call(t, "read", map[string]any{"filePath": f.path("empty.txt")})
		if ok(t, res) != fileText(f.path("empty.txt"), "", "(End of file - total 0 lines)") || res.Trunc.Truncated() {
			t.Fatalf("text = %q", res.Text)
		}
		_, res = f.call(t, "read", map[string]any{"filePath": f.path("empty.txt"), "offset": 2})
		failed(t, res, tool.ClassToolError, "Offset 2 is out of range for this file (0 lines)")
	})

	t.Run("truncates long lines", func(t *testing.T) {
		put(t, f.path("long-line.txt"), strings.Repeat("x", 3000))
		_, res := f.call(t, "read", map[string]any{"filePath": f.path("long-line.txt")})
		if !strings.Contains(ok(t, res), "\n1: "+strings.Repeat("x", 2000)+"... (line truncated to 2000 chars)\n") || len(res.Text) >= 3000 {
			t.Fatalf("text = %q", res.Text)
		}
		// The negative control: 2000 characters is not too long.
		put(t, f.path("exact.txt"), strings.Repeat("x", 2000))
		_, res = f.call(t, "read", map[string]any{"filePath": f.path("exact.txt")})
		if strings.Contains(ok(t, res), "truncated") {
			t.Fatalf("a 2000-character line was cut: %q", res.Text)
		}
	})

	t.Run("line length counts characters, not bytes", func(t *testing.T) {
		// 1500 four-byte characters are 6000 bytes and 1500 runes: kept
		// whole (opencode counts UTF-16 units and would cut them: NOTICE).
		wide := strings.Repeat("\xf0\x9f\x98\x80", 1500)
		put(t, f.path("wide.txt"), wide)
		_, res := f.call(t, "read", map[string]any{"filePath": f.path("wide.txt")})
		if !strings.Contains(ok(t, res), "1: "+wide+"\n") {
			t.Fatal("a 1500-character line was cut")
		}
		// 10000 two-byte characters, longer than read holds of a line: cut
		// at 2000 characters, never mid-character.
		put(t, f.path("accents.txt"), strings.Repeat("\xc3\xa9", 10_000)+"\nnext")
		_, res = f.call(t, "read", map[string]any{"filePath": f.path("accents.txt")})
		want := numbered(1, strings.Repeat("\xc3\xa9", 2000)+"... (line truncated to 2000 chars)", "next")
		if !strings.Contains(ok(t, res), want) {
			t.Fatalf("text = %q", res.Text[:100])
		}
	})
}

// TestReadLineEndings: lines end at "\n" or "\r\n"; a lone "\r" stays in
// its line except at the end of the file; a leading byte order mark is
// dropped; invalid UTF-8 reads as U+FFFD. These are opencode's decode and
// Stream.splitLines, which read.test.ts does not test directly.
func TestReadLineEndings(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name, content string
		want          []string
	}{
		{"LF", "a\nb\n", []string{"a", "b"}},
		{"CRLF", "a\r\nb\r\n", []string{"a", "b"}},
		{"no final newline", "a\nb", []string{"a", "b"}},
		{"blank lines kept", "a\n\n\nb\n", []string{"a", "", "", "b"}},
		{"a trailing blank line", "a\n\n", []string{"a", ""}},
		{"a lone CR stays mid-file", "a\rb\nc\r\r\n", []string{"a\rb", "c\r"}},
		{"a lone CR at the end goes", "a\nb\r", []string{"a", "b"}},
		{"a BOM goes from line one only", "\xef\xbb\xbfa\n\xef\xbb\xbfb\n", []string{"a", "\xef\xbb\xbfb"}},
		{"invalid UTF-8", "caf\xe9\n", []string{"caf\xef\xbf\xbd"}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := f.path(fmt.Sprintf("endings-%d.txt", i))
			put(t, p, tc.content)
			_, res := f.call(t, "read", map[string]any{"filePath": p})
			want := fileText(p, numbered(1, tc.want...), fmt.Sprintf("(End of file - total %d lines)", len(tc.want)))
			if ok(t, res) != want {
				t.Fatalf("text =\n%q\nwant\n%q", res.Text, want)
			}
		})
	}
	// A file that is only a BOM, or only a lone CR, has no lines.
	for _, content := range []string{"\xef\xbb\xbf", "\r"} {
		put(t, f.path("nothing.txt"), content)
		_, res := f.call(t, "read", map[string]any{"filePath": f.path("nothing.txt")})
		if !strings.Contains(ok(t, res), "(End of file - total 0 lines)") {
			t.Fatalf("%q reads as %q", content, res.Text)
		}
	}
}

// TestReadDirectory ports "does not mark final directory page as
// truncated", with a first page as its negative control, and pins the
// listing's shape: sorted, directories and symlinks to them with a slash.
func TestReadDirectory(t *testing.T) {
	f := newFixture(t)
	dir := f.path("dir")
	for i := 1; i <= 10; i++ {
		put(t, filepath.Join(dir, fmt.Sprintf("file-%d.txt", i)), fmt.Sprintf("line%d", i))
	}

	_, res := f.call(t, "read", map[string]any{"filePath": dir, "offset": 6, "limit": 5})
	want := "<path>" + dir + "</path>\n<type>directory</type>\n<entries>\nfile-5.txt\nfile-6.txt\nfile-7.txt\nfile-8.txt\nfile-9.txt\n\n(10 entries)\n</entries>"
	if ok(t, res) != want {
		t.Fatalf("text =\n%s\nwant\n%s", res.Text, want)
	}
	if res.Trunc.Truncated() || res.Content != "file-5.txt\nfile-6.txt\nfile-7.txt\nfile-8.txt\nfile-9.txt" {
		t.Fatalf("trunc %+v, content %q", res.Trunc, res.Content)
	}

	_, res = f.call(t, "read", map[string]any{"filePath": dir, "limit": 5})
	// Byte order puts file-10 second.
	if !strings.HasSuffix(ok(t, res), "<entries>\nfile-1.txt\nfile-10.txt\nfile-2.txt\nfile-3.txt\nfile-4.txt\n\n(Showing 5 of 10 entries. Use 'offset' parameter to read beyond entry 6)\n</entries>") ||
		res.Trunc.KeptLines != 5 || res.Trunc.TotalLines != 10 {
		t.Fatalf("first page = %q, trunc %+v", res.Text, res.Trunc)
	}

	mixed := f.path("mixed")
	put(t, filepath.Join(mixed, "b.txt"), "")
	put(t, filepath.Join(mixed, "sub", "x"), "")
	if err := os.Symlink(filepath.Join(mixed, "sub"), filepath.Join(mixed, "a-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(mixed, "b.txt"), filepath.Join(mixed, "c-link")); err != nil {
		t.Fatal(err)
	}
	_, res = f.call(t, "read", map[string]any{"filePath": "mixed"})
	if ok(t, res) != "<path>"+mixed+"</path>\n<type>directory</type>\n<entries>\na-link/\nb.txt\nc-link\nsub/\n\n(4 entries)\n</entries>" {
		t.Fatalf("text = %q", res.Text)
	}
}

// TestReadMediaAndBinary ports the image, attachment-sniffing, .fbs, and
// binary-detection cases. Images are refused where opencode attaches them
// (plan 019 §3.2), PDFs with the binary message; unsupported image types
// fall through to the text path, as in opencode; each of the three binary
// rules refuses, and the text just under the non-printable threshold reads.
func TestReadMediaAndBinary(t *testing.T) {
	f := newFixture(t)
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	nonPrintable := func(pct int) string { // 1000 bytes, pct% of them \x01
		return strings.Repeat("\x01", pct*10) + strings.Repeat("a", 1000-pct*10)
	}
	refused := []struct{ name, content, text string }{
		{"image.png", string(png), "Cannot read image file yet: "},
		{"image.bin", "\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01", "Cannot read image file yet: "}, // sniffed, before .bin refuses
		{"anim.gif", "GIF89a...", "Cannot read image file yet: "},
		{"pic.webp", "RIFF\x00\x00\x00\x00WEBPVP8 ", "Cannot read image file yet: "},
		{"named.jpeg", "just text", "Cannot read image file yet: "}, // by extension
		{"doc.txt", "%PDF-1.7\nsome text", "Cannot read binary file: "},
		{"doc.pdf", "just text", "Cannot read binary file: "},
		{"null-byte.txt", "hello\x00world", "Cannot read binary file: "},
		{"module.wasm", "not really wasm", "Cannot read binary file: "},
		{"noise.txt", nonPrintable(31), "Cannot read binary file: "},
	}
	for _, tc := range refused {
		put(t, f.path(tc.name), tc.content)
		_, res := f.call(t, "read", map[string]any{"filePath": f.path(tc.name)})
		failed(t, res, tool.ClassToolError, tc.text+f.path(tc.name))
	}

	read := []struct{ name, content string }{
		{"schema.fbs", "namespace MyGame;\n\ntable Monster {\n  pos:Vec3;\n}\n"},
		{"image.bmp", "BM text content"},
		{"photo.tiff", "II text content"},
		{"photo.avif", "avif text content"},
		{"named.png.txt", "text named like a png"},
		{"quiet.txt", nonPrintable(30)},
		{"controls.txt", strings.Repeat("\t\n\v\f\r", 100)},
	}
	for _, tc := range read {
		put(t, f.path(tc.name), tc.content)
		_, res := f.call(t, "read", map[string]any{"filePath": f.path(tc.name)})
		if ok(t, res); !strings.Contains(res.Text, "<type>file</type>") {
			t.Fatalf("%s: %q", tc.name, res.Text)
		}
	}
}

// TestReadMissingSuggests ports opencode's "Did you mean" (read.ts:76-99),
// which read.test.ts does not test: up to three names that contain, or are
// contained in, the missing one, ignoring case.
func TestReadMissingSuggests(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"Config.json", "config.yaml", "config.toml", "configure.sh", "readme.md"} {
		put(t, f.path(n), "")
	}
	_, res := f.call(t, "read", map[string]any{"filePath": "config"})
	failed(t, res, tool.ClassNotFound, "File not found: "+f.path("config")+"\n\nDid you mean one of these?\n"+
		strings.Join([]string{f.path("Config.json"), f.path("config.toml"), f.path("config.yaml")}, "\n"))

	_, res = f.call(t, "read", map[string]any{"filePath": "README.md.bak"})
	failed(t, res, tool.ClassNotFound, "File not found: "+f.path("README.md.bak")+"\n\nDid you mean one of these?\n"+f.path("readme.md"))

	// The negative control: nothing alike, no suggestions.
	_, res = f.call(t, "read", map[string]any{"filePath": "zzz.txt"})
	failed(t, res, tool.ClassNotFound, "File not found: "+f.path("zzz.txt"))
	// Nor when the directory itself is missing.
	_, res = f.call(t, "read", map[string]any{"filePath": "nodir/config"})
	failed(t, res, tool.ClassNotFound, "File not found: "+f.path("nodir/config"))
}

// TestReadRefusesSpecialFiles: a FIFO is refused at once, where a plain open
// would wait for a writer forever, ignoring cancel (plan 019 §3.9) — also
// through a symlink; so is a device. A regular file is the negative control.
func TestReadRefusesSpecialFiles(t *testing.T) {
	f := newFixture(t)
	fifo := f.path("fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fifo, f.path("fifo-link")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fifo, f.path("fifo-link"), "/dev/null"} {
		done := make(chan tool.Result, 1)
		go func() {
			_, res := f.call(t, "read", map[string]any{"filePath": p})
			done <- res
		}()
		select {
		case res := <-done:
			failed(t, res, tool.ClassToolError, "Path is not a regular file or a directory: "+p)
		case <-time.After(10 * time.Second):
			t.Fatalf("reading %s hung", p)
		}
	}
	put(t, f.path("plain"), "plain\n")
	_, res := f.call(t, "read", map[string]any{"filePath": f.path("plain")})
	ok(t, res)
}

// TestReadThroughASymlink: the link is followed; the envelope names the
// path as the model gave it.
func TestReadThroughASymlink(t *testing.T) {
	f := newFixture(t)
	put(t, f.path("real/target.txt"), "through the link\n")
	if err := os.Symlink(filepath.Join("real", "target.txt"), f.path("link.txt")); err != nil {
		t.Fatal(err)
	}
	_, res := f.call(t, "read", map[string]any{"filePath": "link.txt"})
	if ok(t, res) != fileText(f.path("link.txt"), "1: through the link", "(End of file - total 1 lines)") {
		t.Fatalf("text = %q", res.Text)
	}
	// The negative control: a dangling link is not found.
	if err := os.Symlink("gone.txt", f.path("dangling.txt")); err != nil {
		t.Fatal(err)
	}
	_, res = f.call(t, "read", map[string]any{"filePath": "dangling.txt"})
	if res.Class != tool.ClassNotFound {
		t.Fatalf("dangling link = %+v", res)
	}
	// A loop of links is an error, not a hang.
	if err := os.Symlink("loop-b", f.path("loop-a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop-a", f.path("loop-b")); err != nil {
		t.Fatal(err)
	}
	_, res = f.call(t, "read", map[string]any{"filePath": "loop-a"})
	if !res.IsError || res.Class != tool.ClassToolError {
		t.Fatalf("a symlink loop = %+v", res)
	}
}

// TestReadRefusesTheCredentialsFile: <Home>/providers.toml is refused by
// resolved path — directly, through a symlink, and through a hard link —
// and its content never reaches the result. Its neighbour models.toml is
// the negative control. A key in any other file is redacted, not refused.
func TestReadRefusesTheCredentialsFile(t *testing.T) {
	f := newFixture(t)
	cred := filepath.Join(f.env.Home, CredentialsFile)
	put(t, cred, "[providers.x]\napi_key = \""+keyA+"\"\n")
	put(t, filepath.Join(f.env.Home, "models.toml"), "version = 1\n")
	if err := os.Symlink(cred, f.path("sneaky.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(cred, f.path("hard.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.env.Home, f.path("home-link")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{cred, f.path("sneaky.toml"), f.path("hard.toml"), f.path("home-link/" + CredentialsFile)} {
		_, res := f.call(t, "read", map[string]any{"filePath": p})
		failed(t, res, tool.ClassToolError, credentialsText)
	}
	_, res := f.call(t, "read", map[string]any{"filePath": filepath.Join(f.env.Home, "models.toml")})
	ok(t, res)

	put(t, f.path(".env"), "KEY="+keyA+"\n")
	_, res = f.call(t, "read", map[string]any{"filePath": ".env"})
	if strings.Contains(ok(t, res), keyA) || strings.Contains(res.Content, keyA) || !strings.Contains(res.Text, "1: KEY="+redact.Marker) {
		t.Fatalf("a key in a file reached the result: %q / %q", res.Text, res.Content)
	}
}

// TestReadHonoursCancel: a call cancelled before it starts reads nothing.
// The dispatcher refuses a call whose context is already done, so this runs
// the prepared call itself; the live context is the negative control.
func TestReadHonoursCancel(t *testing.T) {
	f := newFixture(t)
	put(t, f.path("a.txt"), "a\n")
	rt, _ := newRead()
	p, err := rt.Prepare(f.env, tool.Call{ID: "t1.1.1", Tool: "read", Input: []byte(`{"filePath":"a.txt"}`)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failed(t, p.Run(ctx, f.env), tool.ClassAborted, tool.AbortedText)
	ok(t, p.Run(context.Background(), f.env))
}
