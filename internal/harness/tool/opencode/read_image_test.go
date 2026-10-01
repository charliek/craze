package opencode

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/tool/attach"
)

// pngOf is a w×h PNG of one grey: a real image, quick to make.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// gifOf is a one-frame w×h GIF.
func gifOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// readImage reads name on f, which must succeed, and checks the result is
// the image attach.Process makes of src — its bytes and type as the result's
// Media — with the one line text naming it as Text and Content.
func readImage(t *testing.T, f *fixture, name string, src []byte, text string) {
	t.Helper()
	_, res := f.call(t, "read", map[string]any{"filePath": name})
	ok(t, res)
	want, err := attach.Process(src)
	if err != nil {
		t.Fatalf("Process refused the test's own image: %v", err)
	}
	if res.Text != text || res.Content != text {
		t.Fatalf("read %s = text %q, content %q; want %q", name, res.Text, res.Content, text)
	}
	if res.Media == nil || !bytes.Equal(res.Media.Data, want.Data) || res.Media.MIME != want.MIME {
		t.Fatalf("read %s returned media %+v; want Process's %d bytes of %s", name, res.Media, len(want.Data), want.MIME)
	}
}

// TestReadImage (plan 033 §3.5, owner decision 8; A9's tool half): on a
// model that accepts images, read returns the image itself — attach.Process's
// copy of it, the one a paste of the same file would send — beside one line
// naming the file, its size and type, and its original size when it was
// scaled down; a GIF comes back as its first frame, a PNG. What Process
// refuses is refused with its reason, and so is a file over Process's source
// cap. The negative control is the same image on a model that does not
// accept images: refused, naming the model, no image.
func TestReadImage(t *testing.T) {
	f := newFixture(t)
	f.d.SetVision(true, "Eye")

	shot := pngOf(t, 40, 30)
	put(t, f.path("shot.png"), string(shot))
	readImage(t, f, "shot.png", shot, "Read image file: "+f.path("shot.png")+" (40×30 image/png)")
	// A pass-through: the file's own bytes.
	if _, res := f.call(t, "read", map[string]any{"filePath": "shot.png"}); !bytes.Equal(res.Media.Data, shot) {
		t.Fatal("a PNG that fits was not passed through untouched")
	}

	wide := pngOf(t, 3000, 40)
	put(t, f.path("wide.png"), string(wide))
	scaled, err := attach.Process(wide)
	if err != nil || scaled.Width != attach.MaxEdge || !scaled.Downscaled() {
		t.Fatalf("Process(3000×40) = %d×%d, %v; want it scaled to the long-edge cap", scaled.Width, scaled.Height, err)
	}
	readImage(t, f, "wide.png", wide, fmt.Sprintf("Read image file: %s (%d×%d image/png, downscaled from 3000×40)",
		f.path("wide.png"), scaled.Width, scaled.Height))

	anim := gifOf(t, 24, 16)
	put(t, f.path("anim.gif"), string(anim))
	readImage(t, f, "anim.gif", anim, "Read image file: "+f.path("anim.gif")+" (24×16 image/png)")

	refused := []struct{ name, content, reason string }{
		{"tiny.png", string(pngOf(t, 1, 1)), "smaller than 8×8 pixels"},
		{"named.jpeg", "just text", "not a supported image (png, jpeg, gif, webp or bmp)"},
		{"broken.png", string(shot[:len(shot)/2]), "not a supported image (png, jpeg, gif, webp or bmp)"},
	}
	for _, tc := range refused {
		put(t, f.path(tc.name), tc.content)
		_, res := f.call(t, "read", map[string]any{"filePath": tc.name})
		failed(t, res, tool.ClassToolError, "Cannot read image file: "+f.path(tc.name)+": "+tc.reason)
		if res.Media != nil {
			t.Fatalf("%s: a refusal carried an image", tc.name)
		}
	}
	// Over the source cap: a sparse file whose first bytes are a PNG's.
	huge := f.path("huge.png")
	put(t, huge, string(shot))
	if err := os.Truncate(huge, attach.MaxSourceBytes+1); err != nil {
		t.Fatal(err)
	}
	_, res := f.call(t, "read", map[string]any{"filePath": "huge.png"})
	failed(t, res, tool.ClassToolError, "Cannot read image file: "+huge+": larger than 20 MiB")

	// The control: the same image, on a model that does not accept images.
	f.d.SetVision(false, "GLM 5.3 (Z.AI)")
	_, res = f.call(t, "read", map[string]any{"filePath": "shot.png"})
	failed(t, res, tool.ClassToolError, "Cannot read image file: GLM 5.3 (Z.AI) does not accept images")
	if res.Media != nil {
		t.Fatal("the refusal carried an image")
	}
}

// TestReadImageChecksComeFirst: read's own checks still come before an image
// is looked at, on a model that accepts images — a hard link to the key file
// named like a PNG, holding a PNG's bytes, is the key file; a directory named
// like one is listed; a FIFO is refused unopened; a missing file is not
// found — so an image name or an image's magic opens nothing read would not
// open. The control is a real PNG beside them, which reads as an image.
func TestReadImageChecksComeFirst(t *testing.T) {
	f := newFixture(t)
	f.d.SetVision(true, "Eye")
	shot := pngOf(t, 16, 16)

	key := filepath.Join(f.env.Home, CredentialsFile)
	put(t, key, string(shot))
	must(t, os.Link(key, f.path("key.png")))
	_, res := f.call(t, "read", map[string]any{"filePath": "key.png"})
	failed(t, res, tool.ClassToolError, credentialsText)

	must(t, os.MkdirAll(f.path("dir.png"), 0o755))
	put(t, f.path("dir.png/inside.txt"), "x")
	_, res = f.call(t, "read", map[string]any{"filePath": "dir.png"})
	if ok(t, res); res.Media != nil || !strings.Contains(res.Text, "<type>directory</type>") {
		t.Fatalf("a directory named like a PNG = %q, media %v; want its listing", res.Text, res.Media != nil)
	}

	must(t, syscall.Mkfifo(f.path("pipe.png"), 0o644))
	_, res = f.call(t, "read", map[string]any{"filePath": "pipe.png"})
	failed(t, res, tool.ClassToolError, "Path is not a regular file or a directory: "+f.path("pipe.png"))

	_, res = f.call(t, "read", map[string]any{"filePath": "gone.png"})
	failed(t, res, tool.ClassNotFound, "File not found: "+f.path("gone.png"))

	put(t, f.path("real.png"), string(shot))
	readImage(t, f, "real.png", shot, "Read image file: "+f.path("real.png")+" (16×16 image/png)")
}

// TestReadImageWaitsForTheLane: one image at a time goes through Process
// (imageLane): a read that finds the lane taken waits, and a cancel — here,
// its deadline — ends the wait as an aborted call with no image. The control
// is the same read once the lane is free: it returns the image.
func TestReadImageWaitsForTheLane(t *testing.T) {
	f := newFixture(t)
	f.d.SetVision(true, "Eye")
	shot := pngOf(t, 16, 16)
	put(t, f.path("shot.png"), string(shot))

	imageLane <- struct{}{} // another read is processing
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, res := f.callCtx(t, ctx, "read", map[string]any{"filePath": "shot.png"})
	<-imageLane
	failed(t, res, tool.ClassAborted, tool.AbortedText)
	if res.Media != nil {
		t.Fatal("an aborted read carried an image")
	}

	readImage(t, f, "shot.png", shot, "Read image file: "+f.path("shot.png")+" (16×16 image/png)")
}

// TestACancelledImageReadReturnsNoImage is r2b #13 (plan 033 C6r): Process
// takes no context, so a read cancelled while it decodes — held there by the
// processImage seam — must not answer with the image Process then makes: it
// is an aborted call with no media, as a cancel during the lane's wait is. And
// a read already cancelled when it reaches a free lane, the two ready at once,
// never gets as far as Process.
func TestACancelledImageReadReturnsNoImage(t *testing.T) {
	f := newFixture(t)
	f.d.SetVision(true, "Eye")
	shot := pngOf(t, 16, 16)
	put(t, f.path("shot.png"), string(shot))
	entered, release := make(chan struct{}), make(chan struct{})
	processImage = func(src []byte) (attach.Image, error) {
		close(entered)
		<-release
		return attach.Process(src)
	}
	t.Cleanup(func() { processImage = attach.Process })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan tool.Result, 1)
	go func() {
		_, res := f.callCtx(t, ctx, "read", map[string]any{"filePath": "shot.png"})
		done <- res
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the read never reached Process")
	}
	cancel()
	close(release)
	var res tool.Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the read never answered")
	}
	if res.Media != nil {
		t.Fatal("a read cancelled during Process carried the image")
	}
	failed(t, res, tool.ClassAborted, tool.AbortedText)

	// Cancelled when it reaches the lane, the lane free: whichever case its
	// select takes, nothing is processed. The call is run as the tool's own
	// (the dispatcher answers a call cancelled before it starts itself, and
	// would never reach the lane).
	processImage = func([]byte) (attach.Image, error) {
		t.Error("a cancelled read processed its image")
		return attach.Image{}, fmt.Errorf("cancelled")
	}
	gone, stop := context.WithCancel(context.Background())
	stop()
	call := &readCall{abs: f.path("shot.png"), title: "shot.png"}
	env := tool.Env{Home: t.TempDir(), Vision: true, ModelName: "Eye"}
	for range 20 {
		res := call.Run(gone, env)
		if res.Media != nil {
			t.Fatal("a cancelled read carried an image")
		}
		failed(t, res, tool.ClassAborted, tool.AbortedText)
	}
}
