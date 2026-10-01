package agent

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/tool/attach"
)

// testImage is a w×h opaque gradient, small and valid.
func testImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{uint8(x * 7), uint8(y * 5), 0x40, 0xff})
		}
	}
	return img
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, testImage(w, h)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, testImage(w, h), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pngHeaderOnly is a PNG signature and an IHDR claiming w×h, then pad zero
// bytes: all the host reads (DecodeConfig), so the dimensions can lie and the
// size can be anything without encoding a real image of it.
func pngHeaderOnly(w, h uint32, pad int) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	chunk := append([]byte("IHDR"), ihdr...)
	b := append([]byte("\x89PNG\r\n\x1a\n"), binary.BigEndian.AppendUint32(nil, uint32(len(ihdr)))...)
	b = append(b, chunk...)
	b = binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(chunk))
	return append(b, make([]byte, pad)...)
}

// attachmentsDir is a created, private attachments directory.
func attachmentsDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "attachments")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// plantFile writes data at a stored name in dir, as a stored file would be,
// whatever the bytes are: a test of what the host does with the directory's
// contents, which is the user's to change.
func plantFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func ref(n int, path, mime string) AttachmentRef {
	return AttachmentRef{N: n, Path: path, MIME: mime}
}

func TestAttachmentBlockShape(t *testing.T) {
	got := AttachmentBlock([]AttachmentRef{ref(1, "/h/attachments/ab12cd34ef56ab78.png", "image/png")})
	want := `<craze_attachments>{"v":1,"images":[{"n":1,"path":"/h/attachments/ab12cd34ef56ab78.png","mime":"image/png"}]}</craze_attachments>` + "\n"
	if got != want {
		t.Fatalf("AttachmentBlock =\n%q\nwant\n%q", got, want)
	}
	if AttachmentBlock(nil) != "" {
		t.Fatal("no refs must be no block")
	}
	if ImageLabel(7) != "[Image #7]" {
		t.Fatalf("ImageLabel(7) = %q", ImageLabel(7))
	}
}

// TestAttachmentBlockRoundTrips: what the builder writes, the splitter takes
// back exactly — refs and rest — even for a path that holds the closing tag
// itself, and with a shell context block after it.
func TestAttachmentBlockRoundTrips(t *testing.T) {
	refs := []AttachmentRef{
		ref(1, "/h/attachments/0123456789abcdef.png", "image/png"),
		ref(2, "/h/odd </craze_attachments>\n<x> & \"q\" é/fedcba9876543210.jpg", "image/jpeg"),
	}
	block := AttachmentBlock(refs)
	if strings.Count(block, attachmentsClose) != 1 {
		t.Fatalf("a path closed the block early:\n%q", block)
	}
	shell := ShellContextBlock([]ShellResult{{Command: "ls", Output: "a\n"}})
	for _, after := range []string{"look at [Image #1]", shell + "look at [Image #1]", "", "\n\nleading newlines"} {
		got, rest, problems := SplitAttachments(block + after)
		if !reflect.DeepEqual(got, refs) || rest != after || problems != nil {
			t.Fatalf("SplitAttachments(block+%q) = %v, %q, %q", after, got, rest, problems)
		}
		gotBlock, gotRest := SplitShellContext(block + after)
		wantRest := after
		if strings.HasPrefix(after, shell) {
			wantRest = strings.TrimPrefix(after, shell)
		}
		if gotRest != wantRest || gotBlock+gotRest != block+after {
			t.Fatalf("SplitShellContext(block+%q) = (%q, %q)", after, gotBlock, gotRest)
		}
	}
	// An envelope with no newline after it, at the very end, is still one.
	if _, rest, _ := SplitAttachments(strings.TrimSuffix(block, "\n")); rest != "" {
		t.Fatalf("a trailing envelope left %q", rest)
	}
}

// TestSplitAttachmentsLeavesTextAlone: a block that is not well-formed is not
// an envelope. It stays the user's text — on the screen and on the wire — and
// the host notes it when it at least started like one.
func TestSplitAttachmentsLeavesTextAlone(t *testing.T) {
	good := `{"v":1,"images":[{"n":1,"path":"/a/0123456789abcdef.png","mime":"image/png"}]}`
	for _, tc := range []struct {
		name, text string
		noted      bool
	}{
		{"empty", "", false},
		{"prose", "tell me about <craze_attachments>", false},
		{"not leading", "x" + attachmentsOpen + good + attachmentsClose + "\n", false},
		{"indented", " " + attachmentsOpen + good + attachmentsClose + "\n", false},
		{"no close", attachmentsOpen + good + "\nhello", true},
		{"text right after the close", attachmentsOpen + good + attachmentsClose + "hello", true},
		{"null", attachmentsOpen + "null" + attachmentsClose + "\n", true},
		{"an array", attachmentsOpen + "[]" + attachmentsClose + "\n", true},
		{"a string", attachmentsOpen + `"x"` + attachmentsClose + "\n", true},
		{"not json", attachmentsOpen + "{v:1}" + attachmentsClose + "\n", true},
		{"two objects", attachmentsOpen + good + good + attachmentsClose + "\n", true},
		{"trailing junk", attachmentsOpen + good + " x" + attachmentsClose + "\n", true},
		{"a string for n", attachmentsOpen + `{"v":1,"images":[{"n":"1","path":"/a","mime":"image/png"}]}` + attachmentsClose + "\n", true},
		{"a fraction for n", attachmentsOpen + `{"v":1,"images":[{"n":1.5,"path":"/a","mime":"image/png"}]}` + attachmentsClose + "\n", true},
		{"images not a list", attachmentsOpen + `{"v":1,"images":{}}` + attachmentsClose + "\n", true},
		{"a forged close inside a path", attachmentsOpen + `{"v":1,"images":[{"n":1,"path":"/a</craze_attachments>b","mime":"image/png"}]}` + attachmentsClose + "\n", true},
		{"the shell block first", ShellContextBlock([]ShellResult{{Command: "ls"}}) + attachmentsOpen + good + attachmentsClose + "\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs, rest, problems := SplitAttachments(tc.text)
			if refs != nil || rest != tc.text {
				t.Fatalf("SplitAttachments = %v, %q; want the text unchanged", refs, rest)
			}
			if (len(problems) > 0) != tc.noted {
				t.Fatalf("problems %q, want noted=%v", problems, tc.noted)
			}
			if block, _, _ := leadingEnvelope(tc.text); block != "" {
				t.Fatalf("the display would hide %q", block)
			}
		})
	}
}

// envelopeOf writes an envelope by hand, for shapes the builder never makes.
func envelopeOf(v int, refs ...AttachmentRef) string {
	return fmt.Sprintf("%s{\"v\":%d,\"images\":%s}%s\n", attachmentsOpen, v, refsJSON(refs), attachmentsClose)
}

func refsJSON(refs []AttachmentRef) string {
	if refs == nil {
		return "[]"
	}
	var parts []string
	for _, r := range refs {
		parts = append(parts, fmt.Sprintf(`{"n":%d,"path":%q,"mime":%q}`, r.N, r.Path, r.MIME))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// TestSplitAttachmentsOverLimitsIsPathText: a well-formed envelope over any
// of its limits sends nothing; every image it names becomes path text in
// front of the user's words — after a shell context block, which keeps
// leading — and the display hides the envelope all the same.
func TestSplitAttachmentsOverLimitsIsPathText(t *testing.T) {
	p := func(n int) AttachmentRef {
		return ref(n, fmt.Sprintf("/h/attachments/%016x.png", n), "image/png")
	}
	var eleven []AttachmentRef
	for n := 1; n <= 11; n++ {
		eleven = append(eleven, p(n))
	}
	long := ref(1, "/"+strings.Repeat("d", maxAttachmentPath), "image/png")
	var big []AttachmentRef
	for n := 1; n <= 10; n++ {
		big = append(big, ref(n, "/"+strings.Repeat("e", 400), "image/png"))
	}
	for _, tc := range []struct {
		name string
		env  string
		why  string
	}{
		{"eleven images", envelopeOf(1, eleven...), "11 images"},
		{"a repeated number", envelopeOf(1, p(1), p(2), ref(1, "/h/attachments/0000000000000009.png", "image/png")), "named twice"},
		{"number 0", envelopeOf(1, p(0)), "outside 1 to 99"},
		{"number 100", envelopeOf(1, p(100)), "outside 1 to 99"},
		{"a relative path", envelopeOf(1, ref(1, "attachments/0123456789abcdef.png", "image/png")), "not absolute"},
		{"a path over 1024 bytes", envelopeOf(1, long), "over the 1024-byte limit"},
		{"a gif", envelopeOf(1, ref(1, "/h/attachments/0123456789abcdef.gif", "image/gif")), "not one craze stores"},
		{"another version", envelopeOf(2, p(1)), "version 2"},
		{"over 4 KiB", envelopeOf(1, big...), "over its 4096-byte limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shell := ShellContextBlock([]ShellResult{{Command: "ls", Output: "/flows:gauntlet\n"}})
			for _, after := range []string{"see [Image #1]", shell + "see [Image #1]"} {
				refs, rest, problems := SplitAttachments(tc.env + after)
				if refs != nil {
					t.Fatalf("refs %v sent from an over-limit envelope", refs)
				}
				if len(problems) != 1 || !strings.Contains(problems[0], tc.why) {
					t.Fatalf("problems %q, want one saying %q", problems, tc.why)
				}
				if !strings.HasSuffix(rest, "\nsee [Image #1]") {
					t.Fatalf("the user's words are not last: %q", rest)
				}
				if strings.Contains(rest, attachmentsTag) {
					t.Fatalf("the envelope reached the wire: %q", rest)
				}
				_, env, _ := leadingEnvelope(tc.env)
				for _, r := range env.Images {
					if want := attachmentPathText(r.N, r.Path, "the attachment list is over its limits"); !strings.Contains(rest, want) {
						t.Fatalf("no path text %q in %q", want, rest)
					}
				}
				if strings.HasPrefix(after, shell) {
					if b, _ := splitShellBlock(rest); b != shell {
						t.Fatalf("the shell block no longer leads: %q", rest)
					}
				}
				if block, _ := SplitShellContext(tc.env + after); !strings.HasPrefix(block, tc.env) {
					t.Fatalf("the display would show the envelope: block %q", block)
				}
			}
		})
	}
}

// TestReadAttachments is the host's confined read (plan 033 A4): each way a
// named file can be refused becomes path text with its reason, and each way
// it can be dropped leaves a problem and nothing else.
func TestReadAttachments(t *testing.T) {
	visible := "compare [Image #1] with [Image #2]"
	good := testPNG(t, 16, 12)
	other := testPNG(t, 20, 10)

	t.Run("two images", func(t *testing.T) {
		dir := attachmentsDir(t)
		p1, err := attach.Save(dir, good, attach.MIMEPNG)
		if err != nil {
			t.Fatal(err)
		}
		jpg := testJPEG(t, 24, 16)
		p2, err := attach.Save(dir, jpg, attach.MIMEJPEG)
		if err != nil {
			t.Fatal(err)
		}
		atts, fallbacks, problems := ReadAttachments(dir, []AttachmentRef{ref(1, p1, attach.MIMEPNG), ref(2, p2, attach.MIMEJPEG)}, visible)
		if fallbacks != nil || problems != nil || len(atts) != 2 {
			t.Fatalf("got %d, fallbacks %q, problems %q", len(atts), fallbacks, problems)
		}
		if atts[0].N != 1 || !bytes.Equal(atts[0].Data, good) || atts[0].Width != 16 || atts[0].Height != 12 {
			t.Fatalf("image 1: %+v", atts[0].AttachmentRef)
		}
		if atts[1].N != 2 || !bytes.Equal(atts[1].Data, jpg) || atts[1].MIME != attach.MIMEJPEG || atts[1].Width != 24 {
			t.Fatalf("image 2: %+v", atts[1].AttachmentRef)
		}
	})

	// One image refused, the other sent: each case plants image 1 its own
	// way, and image 2 is always a good stored file.
	for _, tc := range []struct {
		name   string
		plant  func(t *testing.T, dir string) AttachmentRef
		reason string
	}{
		{"a missing file", func(t *testing.T, dir string) AttachmentRef {
			return ref(1, filepath.Join(dir, "0123456789abcdef.png"), attach.MIMEPNG)
		}, "no longer available"},
		{"a symlink to a stored file", func(t *testing.T, dir string) AttachmentRef {
			plantFile(t, dir, "fedcba9876543210.png", good)
			if err := os.Symlink("fedcba9876543210.png", filepath.Join(dir, "0123456789abcdef.png")); err != nil {
				t.Fatal(err)
			}
			return ref(1, filepath.Join(dir, "0123456789abcdef.png"), attach.MIMEPNG)
		}, "not a regular file"},
		{"a symlink out of the directory", func(t *testing.T, dir string) AttachmentRef {
			secret := filepath.Join(t.TempDir(), "secret.png")
			if err := os.WriteFile(secret, good, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(secret, filepath.Join(dir, "0123456789abcdef.png")); err != nil {
				t.Fatal(err)
			}
			return ref(1, filepath.Join(dir, "0123456789abcdef.png"), attach.MIMEPNG)
		}, "not a regular file"},
		{"a FIFO", func(t *testing.T, dir string) AttachmentRef {
			if err := syscall.Mkfifo(filepath.Join(dir, "0123456789abcdef.png"), 0o600); err != nil {
				t.Fatal(err)
			}
			return ref(1, filepath.Join(dir, "0123456789abcdef.png"), attach.MIMEPNG)
		}, "not a regular file"},
		{"over the cap", func(t *testing.T, dir string) AttachmentRef {
			p := plantFile(t, dir, "0123456789abcdef.png", nil)
			if err := os.Truncate(p, attach.MaxBytes+1); err != nil {
				t.Fatal(err)
			}
			return ref(1, p, attach.MIMEPNG)
		}, "larger than 3.75 MiB"},
		{"a header claiming 4000 wide", func(t *testing.T, dir string) AttachmentRef {
			return ref(1, plantFile(t, dir, "0123456789abcdef.png", pngHeaderOnly(4000, 10, 64)), attach.MIMEPNG)
		}, "larger than 2000 pixels on its long edge"},
		{"a header claiming 4×4", func(t *testing.T, dir string) AttachmentRef {
			return ref(1, plantFile(t, dir, "0123456789abcdef.png", pngHeaderOnly(4, 4, 64)), attach.MIMEPNG)
		}, "smaller than 8×8 pixels"},
		{"a jpeg named as a png", func(t *testing.T, dir string) AttachmentRef {
			return ref(1, plantFile(t, dir, "0123456789abcdef.png", testJPEG(t, 16, 16)), attach.MIMEPNG)
		}, "not the image type it claims to be"},
		{"a png claimed as a jpeg", func(t *testing.T, dir string) AttachmentRef {
			return ref(1, plantFile(t, dir, "0123456789abcdef.jpg", good), attach.MIMEJPEG)
		}, "not the image type it claims to be"},
		{"not an image", func(t *testing.T, dir string) AttachmentRef {
			return ref(1, plantFile(t, dir, "0123456789abcdef.png", []byte("#!/bin/sh\necho hi\n")), attach.MIMEPNG)
		}, "not the image type it claims to be"},
		{"a path outside the directory", func(t *testing.T, dir string) AttachmentRef {
			elsewhere := filepath.Join(t.TempDir(), "0123456789abcdef.png")
			if err := os.WriteFile(elsewhere, good, 0o600); err != nil {
				t.Fatal(err)
			}
			return ref(1, elsewhere, attach.MIMEPNG)
		}, "not a file in craze's attachments directory"},
		{"a path that only resolves into the directory", func(t *testing.T, dir string) AttachmentRef {
			plantFile(t, dir, "0123456789abcdef.png", good)
			return ref(1, filepath.Join(dir, "..", "attachments")+"/./0123456789abcdef.png", attach.MIMEPNG)
		}, "not a file in craze's attachments directory"},
		{"a name the store never writes", func(t *testing.T, dir string) AttachmentRef {
			return ref(1, plantFile(t, dir, "notes.png", good), attach.MIMEPNG)
		}, "not a file in craze's attachments directory"},
		{"a path into a subdirectory", func(t *testing.T, dir string) AttachmentRef {
			if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
				t.Fatal(err)
			}
			return ref(1, plantFile(t, filepath.Join(dir, "sub"), "0123456789abcdef.png", good), attach.MIMEPNG)
		}, "not a file in craze's attachments directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := attachmentsDir(t)
			r1 := tc.plant(t, dir)
			p2, err := attach.Save(dir, other, attach.MIMEPNG)
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				atts                []Attachment
				fallbacks, problems []string
			}
			done := make(chan result, 1)
			go func() {
				// A FIFO opened without O_NONBLOCK would block here forever.
				a, f, p := ReadAttachments(dir, []AttachmentRef{r1, ref(2, p2, attach.MIMEPNG)}, visible)
				done <- result{a, f, p}
			}()
			var got result
			select {
			case got = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("ReadAttachments blocked")
			}
			want := attachmentPathText(1, r1.Path, tc.reason)
			if len(got.fallbacks) != 1 || got.fallbacks[0] != want {
				t.Fatalf("fallbacks %q, want [%q]", got.fallbacks, want)
			}
			if len(got.problems) != 1 || !strings.Contains(got.problems[0], tc.reason) {
				t.Fatalf("problems %q", got.problems)
			}
			if len(got.atts) != 1 || got.atts[0].N != 2 || !bytes.Equal(got.atts[0].Data, other) {
				t.Fatalf("the good image did not go: %d attachments", len(got.atts))
			}
		})
	}

	// The directory itself refused: every image falls back.
	for _, tc := range []struct {
		name   string
		dir    func(t *testing.T) (dir, refDir string)
		reason string
	}{
		{"a symlinked directory", func(t *testing.T) (string, string) {
			real := attachmentsDir(t)
			plantFile(t, real, "0123456789abcdef.png", good)
			link := filepath.Join(t.TempDir(), "attachments")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			return link, link
		}, "craze's attachments directory is not private to this user"},
		{"a directory others can read", func(t *testing.T) (string, string) {
			dir := attachmentsDir(t)
			plantFile(t, dir, "0123456789abcdef.png", good)
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return dir, dir
		}, "craze's attachments directory is not private to this user"},
		{"a missing directory", func(t *testing.T) (string, string) {
			dir := filepath.Join(t.TempDir(), "attachments")
			return dir, dir
		}, "no longer available"},
		{"no craze directory", func(t *testing.T) (string, string) {
			return "", "/h/attachments"
		}, "craze has no attachments directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, refDir := tc.dir(t)
			refs := []AttachmentRef{
				ref(1, filepath.Join(refDir, "0123456789abcdef.png"), attach.MIMEPNG),
				ref(2, filepath.Join(refDir, "fedcba9876543210.png"), attach.MIMEPNG),
			}
			atts, fallbacks, problems := ReadAttachments(dir, refs, visible)
			if atts != nil || len(fallbacks) != 2 || len(problems) != 2 {
				t.Fatalf("got %d attachments, fallbacks %q, problems %q", len(atts), fallbacks, problems)
			}
			for i, r := range refs {
				if want := attachmentPathText(r.N, r.Path, tc.reason); fallbacks[i] != want {
					t.Fatalf("fallback %d %q, want %q", i, fallbacks[i], want)
				}
			}
		})
	}

	t.Run("a label absent from the text", func(t *testing.T) {
		dir := attachmentsDir(t)
		p1, _ := attach.Save(dir, good, attach.MIMEPNG)
		p3, _ := attach.Save(dir, other, attach.MIMEPNG)
		atts, fallbacks, problems := ReadAttachments(dir, []AttachmentRef{ref(1, p1, attach.MIMEPNG), ref(3, p3, attach.MIMEPNG)}, visible)
		if len(atts) != 1 || atts[0].N != 1 {
			t.Fatalf("attachments %d", len(atts))
		}
		if fallbacks != nil {
			t.Fatalf("a dropped image left path text: %q", fallbacks)
		}
		if len(problems) != 1 || !strings.Contains(problems[0], "[Image #3] is not in the message") {
			t.Fatalf("problems %q", problems)
		}
	})

	t.Run("a label only in a command's output", func(t *testing.T) {
		dir := attachmentsDir(t)
		p1, _ := attach.Save(dir, good, attach.MIMEPNG)
		shown := ShellContextBlock([]ShellResult{{Command: "cat notes", Output: "see [Image #1]\n"}}) + "what about this?"
		atts, fallbacks, problems := ReadAttachments(dir, []AttachmentRef{ref(1, p1, attach.MIMEPNG)}, shown)
		if atts != nil || fallbacks != nil || len(problems) != 1 {
			t.Fatalf("got %d attachments, fallbacks %q, problems %q", len(atts), fallbacks, problems)
		}
	})

	t.Run("a label that is a prefix of another", func(t *testing.T) {
		dir := attachmentsDir(t)
		p1, _ := attach.Save(dir, good, attach.MIMEPNG)
		atts, _, problems := ReadAttachments(dir, []AttachmentRef{ref(1, p1, attach.MIMEPNG)}, "only [Image #10] here")
		if atts != nil || len(problems) != 1 {
			t.Fatalf("[Image #10] counted as [Image #1]: %d attachments", len(atts))
		}
	})

	t.Run("over the per-message total", func(t *testing.T) {
		dir := attachmentsDir(t)
		const each = 3_500_000 // four fit in 15 MiB, five do not
		var refs []AttachmentRef
		var text []string
		for n := 1; n <= 5; n++ {
			// Distinct bytes, so the store keeps five files.
			data := pngHeaderOnly(uint32(16+n), 16, each)
			p, err := attach.Save(dir, data, attach.MIMEPNG)
			if err != nil {
				t.Fatal(err)
			}
			refs = append(refs, ref(n, p, attach.MIMEPNG))
			text = append(text, ImageLabel(n))
		}
		atts, fallbacks, problems := ReadAttachments(dir, refs, strings.Join(text, " "))
		if len(atts) != 4 || len(fallbacks) != 1 || len(problems) != 1 {
			t.Fatalf("got %d attachments, fallbacks %q", len(atts), fallbacks)
		}
		if !strings.HasPrefix(fallbacks[0], "[Image #5: ") || !strings.Contains(fallbacks[0], "over 15 MiB") {
			t.Fatalf("fallback %q", fallbacks[0])
		}
		total := 0
		for _, a := range atts {
			total += len(a.Data)
		}
		if total > attach.MaxMessageBytes {
			t.Fatalf("%d bytes went", total)
		}
	})
}

// TestALabelMustBeDrawnToCount is P28 against what a terminal draws (plan 033
// C3r, r1 #2): a label inside a string sequence's payload — an OSC title, a
// DCS, APC, PM or SOS, in either spelling, closed or not — is not visible, and
// a text with anything that can move, erase, recolour or conceal what is drawn
// (a CSI eating the label's bracket, SGR conceal, a lone CR, backspace, an
// 8-bit CSI) shows no label at all. A plain label still counts, beside a title
// or across a CRLF too.
func TestALabelMustBeDrawnToCount(t *testing.T) {
	good := testPNG(t, 16, 16)
	cases := []struct {
		name, text string
		sent       bool
	}{
		{"a plain label", "look at [Image #1] please", true},
		{"a label beside an OSC title", "\x1b]0;a title\x07look at [Image #1]", true},
		{"a label across a CRLF", "look\r\nat [Image #1]", true},
		{"a label after a tab", "look\tat [Image #1]", true},
		{"an OSC title, BEL", "\x1b]0;[Image #1]\x07hello", false},
		{"an OSC title, ST", "\x1b]2;[Image #1]\x1b\\hello", false},
		{"an OSC title, 8-bit", "\u009d0;[Image #1]\u009chello", false},
		{"an OSC never closed", "hello \x1b]0;[Image #1]", false},
		{"a DCS", "\x1bP[Image #1]\x1b\\hello", false},
		{"a DCS, 8-bit", "\u0090[Image #1]\u009chello", false},
		{"an APC", "\x1b_[Image #1]\x1b\\hello", false},
		{"an APC, 8-bit", "\u009f[Image #1]\u009chello", false},
		{"a PM", "\x1b^[Image #1]\x07hello", false},
		{"an SOS", "\x1bX[Image #1]\x07hello", false},
		{"a CSI eating the bracket", "\x1b[Image #1] hello", false},
		{"a CSI eating the bracket, 8-bit", "\u009b[Image #1] hello", false},
		{"SGR conceal around the label", "\x1b[8m[Image #1]\x1b[28m hello", false},
		{"an erase after the label", "[Image #1]\x1b[2K hello", false},
		{"a lone CR after the label", "[Image #1]\rhello world", false},
		{"backspaces over the label", "[Image #1]\b\b\b\b\b\b\b\b\b\b", false},
		{"another ESC sequence", "[Image #1]\x1b8 hello", false},
		{"DEL", "[Image #1]\x7f", false},
		{"a C1 control", "[Image #1]\u0085", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := attachmentsDir(t)
			p1, err := attach.Save(dir, good, attach.MIMEPNG)
			if err != nil {
				t.Fatal(err)
			}
			atts, fallbacks, problems := ReadAttachments(dir, []AttachmentRef{ref(1, p1, attach.MIMEPNG)}, tc.text)
			if fallbacks != nil {
				t.Fatalf("a label rule drop left path text: %q", fallbacks)
			}
			if sent := len(atts) == 1; sent != tc.sent {
				t.Fatalf("%q: image sent = %v, want %v (problems %q)", tc.text, sent, tc.sent, problems)
			}
			if !tc.sent && (len(problems) != 1 || !strings.Contains(problems[0], "[Image #1] is not in the message")) {
				t.Fatalf("problems %q", problems)
			}
		})
	}
}

// TestReadAttachmentsHoldsTheEnvelopeLimits: refs that did not come through
// SplitAttachments are held to its count and numbering limits all the same.
func TestReadAttachmentsHoldsTheEnvelopeLimits(t *testing.T) {
	dir := attachmentsDir(t)
	p, err := attach.Save(dir, testPNG(t, 16, 16), attach.MIMEPNG)
	if err != nil {
		t.Fatal(err)
	}
	var refs []AttachmentRef
	var labels []string
	for n := 1; n <= 11; n++ {
		refs = append(refs, ref(n, p, attach.MIMEPNG))
		labels = append(labels, ImageLabel(n))
	}
	atts, fallbacks, _ := ReadAttachments(dir, refs, strings.Join(labels, " "))
	if len(atts) != attach.MaxPerMessage || len(fallbacks) != 1 || !strings.Contains(fallbacks[0], "more than 10 images") {
		t.Fatalf("got %d attachments, fallbacks %q", len(atts), fallbacks)
	}
	atts, fallbacks, _ = ReadAttachments(dir, []AttachmentRef{ref(1, p, attach.MIMEPNG), ref(1, p, attach.MIMEPNG)}, "[Image #1]")
	if len(atts) != 1 || len(fallbacks) != 1 || !strings.Contains(fallbacks[0], "named twice") {
		t.Fatalf("got %d attachments, fallbacks %q", len(atts), fallbacks)
	}
}

// TestHostRefusesWhatTheEnvelopeLimits is A4's envelope half, end to end from
// the text: a repeated number, eleven images and a relative path each send
// nothing and leave path text, before any file is touched.
func TestHostRefusesWhatTheEnvelopeLimits(t *testing.T) {
	dir := attachmentsDir(t)
	good := testPNG(t, 16, 16)
	p, err := attach.Save(dir, good, attach.MIMEPNG)
	if err != nil {
		t.Fatal(err)
	}
	var eleven []AttachmentRef
	var labels []string
	for n := 1; n <= 11; n++ {
		eleven = append(eleven, ref(n, p, attach.MIMEPNG))
		labels = append(labels, ImageLabel(n))
	}
	for _, tc := range []struct {
		name string
		refs []AttachmentRef
	}{
		{"a repeated number", []AttachmentRef{ref(1, p, attach.MIMEPNG), ref(1, p, attach.MIMEPNG)}},
		{"eleven images", eleven},
		{"a relative path", []AttachmentRef{ref(1, "attachments/"+filepath.Base(p), attach.MIMEPNG)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := AttachmentBlock(tc.refs) + strings.Join(labels, " ")
			refs, rest, problems := SplitAttachments(text)
			atts, _, _ := ReadAttachments(dir, refs, rest)
			if atts != nil || len(problems) != 1 || !strings.Contains(rest, "(not attached: the attachment list is over its limits)") {
				t.Fatalf("got %d attachments, problems %q, rest %q", len(atts), problems, rest)
			}
		})
	}
}

// TestPathTextIsSanitisedAndCapped: a forged path can hold anything, and
// path text is model-facing and journaled: one clean line, at most 256 runes.
func TestPathTextIsSanitisedAndCapped(t *testing.T) {
	got := attachmentPathText(3, "/tmp/a\nb\x1b[31mred\u202eevil\x07.png", "no longer available")
	want := "[Image #3: /tmp/a bredevil.png (not attached: no longer available)]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	long := "/" + strings.Repeat("é", 400)
	p := pathTextPath(long)
	if utf8.RuneCountInString(p) != pathTextMax || !strings.HasSuffix(p, "…") || !utf8.ValidString(p) {
		t.Fatalf("capped to %d runes: %q…", utf8.RuneCountInString(p), p[:20])
	}
	if short := "/a/0123456789abcdef.png"; pathTextPath(short) != short {
		t.Fatal("a short clean path changed")
	}
}

// TestAttachmentsAsPathText is P7's helper: an interjection's envelope becomes
// [Image #N: <path>] lines after any shell block, and text without one is
// untouched.
func TestAttachmentsAsPathText(t *testing.T) {
	refs := []AttachmentRef{ref(1, "/h/attachments/0123456789abcdef.png", "image/png"), ref(2, "/h/attachments/fedcba9876543210.jpg", "image/jpeg")}
	shell := ShellContextBlock([]ShellResult{{Command: "ls", Output: "a\n"}})
	lines := "[Image #1: /h/attachments/0123456789abcdef.png]\n[Image #2: /h/attachments/fedcba9876543210.jpg]\n"
	for _, tc := range []struct{ in, want string }{
		{AttachmentBlock(refs) + "look at [Image #1] and [Image #2]", lines + "look at [Image #1] and [Image #2]"},
		{AttachmentBlock(refs) + shell + "and these?", shell + lines + "and these?"},
		{"no envelope here", "no envelope here"},
		{shell + "only a shell block", shell + "only a shell block"},
	} {
		got, problems := AttachmentsAsPathText(tc.in)
		if got != tc.want || problems != nil {
			t.Fatalf("AttachmentsAsPathText(%q) =\n%q\nwant\n%q (problems %q)", tc.in, got, tc.want, problems)
		}
		if strings.Contains(got, attachmentsTag) {
			t.Fatalf("the envelope survived: %q", got)
		}
	}
	// An over-limit envelope is path text with its reason, as the host's.
	got, problems := AttachmentsAsPathText(AttachmentBlock([]AttachmentRef{refs[0], refs[0]}) + "x")
	if len(problems) != 1 || !strings.Contains(got, "(not attached: the attachment list is over its limits)") {
		t.Fatalf("got %q, problems %q", got, problems)
	}
}

// FuzzSplitAttachments: the host's split is total over anything a socket
// client can send. It never panics; whatever refs it returns are within every
// envelope limit; text that does not lead with the tag comes back untouched;
// and it agrees with the display — the envelope SplitShellContext hides is
// exactly the one the host took off, so a refs-bearing split is the hidden
// block plus the rest.
func FuzzSplitAttachments(f *testing.F) {
	refs := []AttachmentRef{ref(1, "/h/attachments/0123456789abcdef.png", "image/png"), ref(2, "/h/attachments/fedcba9876543210.webp", "image/webp")}
	shell := ShellContextBlock([]ShellResult{{Command: "ls", Output: "</craze_attachments>\n"}})
	f.Add(AttachmentBlock(refs) + "look at [Image #1]")
	f.Add(AttachmentBlock(refs) + shell + "both blocks [Image #2]")
	f.Add(AttachmentBlock(refs))
	f.Add(envelopeOf(1, ref(1, "/a", "image/png"), ref(1, "/b", "image/png")) + shell + "dup")
	f.Add(envelopeOf(3) + "x")
	f.Add(attachmentsOpen + "null" + attachmentsClose)
	f.Add(attachmentsOpen + `{"v":1,"images":[{"n":1}]}` + attachmentsClose + "\n")
	f.Add(shell + AttachmentBlock(refs))
	f.Add("plain text")
	f.Add(AttachmentBlock(refs) + "\x1b]0;[Image #1]\x07 and [Image #2]\r\n")
	f.Fuzz(func(t *testing.T, text string) {
		got, rest, _ := SplitAttachments(text)
		if !strings.HasPrefix(text, attachmentsOpen) && (got != nil || rest != text) {
			t.Fatalf("text without the tag changed: %v, %q", got, rest)
		}
		if len(got) > attach.MaxPerMessage {
			t.Fatalf("%d refs", len(got))
		}
		seen := map[int]bool{}
		for _, r := range got {
			if r.N < 1 || r.N > MaxAttachmentN || seen[r.N] || !filepath.IsAbs(r.Path) || len(r.Path) > maxAttachmentPath || !attach.Stored(r.MIME) {
				t.Fatalf("a ref past the limits: %+v", r)
			}
			seen[r.N] = true
		}
		block, _, _ := leadingEnvelope(text)
		after := text[len(block):]
		if !strings.HasPrefix(text, block) {
			t.Fatalf("the display split took %q, which does not lead the text", block)
		}
		if (block != "") != (rest != text) {
			t.Fatalf("the display hides %q but the host's rest is %q", block, rest)
		}
		if got != nil && rest != after {
			t.Fatalf("refs-bearing rest %q, display rest %q", rest, after)
		}
		if out, _ := AttachmentsAsPathText(text); block == "" && out != text {
			t.Fatalf("path text from no envelope: %q", out)
		}
		// P28's projection (C3r) is total, and what it keeps is drawable: no
		// escape, no control but a newline, a tab or a CRLF's CR.
		for _, r := range visibleText(rest) {
			if r == 0x1b || r == 0x7f || isC1(r) || (r < 0x20 && r != '\n' && r != '\t' && r != '\r') {
				t.Fatalf("visibleText(%q) kept %U", rest, r)
			}
		}
	})
}

// TestAttachmentOriginalIsUntrusted is plan 033 X13: the envelope's ow/oh
// round-trip through the block (and are absent from it when zero), and the
// host uses them — for the downscale note's words alone — only when both are
// positive, within the pixel limit, and larger than what the file's own bytes
// say; anything else is no claim.
func TestAttachmentOriginalIsUntrusted(t *testing.T) {
	r := AttachmentRef{N: 1, Path: "/h/attachments/0123456789abcdef.png", MIME: "image/png", OW: 3024, OH: 1964}
	block := AttachmentBlock([]AttachmentRef{r})
	if !strings.Contains(block, `"ow":3024,"oh":1964`) {
		t.Fatalf("the original size is not in the block: %q", block)
	}
	got, _, _ := SplitAttachments(block + "[Image #1]")
	if len(got) != 1 || got[0] != r {
		t.Fatalf("round trip: %+v", got)
	}
	for _, tc := range []struct {
		name   string
		ow, oh int
		w, h   int
		ok     bool
	}{
		{"a downscale", 3024, 1964, 2000, 1299, true},
		{"one edge larger", 2001, 1299, 2000, 1299, true},
		{"absent", 0, 0, 2000, 1299, false},
		{"one absent", 3024, 0, 2000, 1299, false},
		{"negative", -3024, -1964, 2000, 1299, false},
		{"the same size", 2000, 1299, 2000, 1299, false},
		{"smaller than the bytes", 1000, 600, 2000, 1299, false},
		{"one edge smaller", 3024, 1000, 2000, 1299, false},
		{"over the pixel limit", 10000, 10000, 2000, 2000, false},
		{"an edge past the limit alone", 1 << 62, 1 << 62, 2000, 1299, false},
	} {
		a := Attachment{AttachmentRef: AttachmentRef{OW: tc.ow, OH: tc.oh}, Width: tc.w, Height: tc.h}
		w, h, ok := a.Original()
		if ok != tc.ok || (ok && (w != tc.ow || h != tc.oh)) || (!ok && (w != 0 || h != 0)) {
			t.Errorf("%s: Original() = %d, %d, %v; want ok=%v", tc.name, w, h, ok, tc.ok)
		}
	}
}
