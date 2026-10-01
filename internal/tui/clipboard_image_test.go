package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/tool/attach"
)

// The clipboard's image read (plan 033 §3.3, clipboard_image.go) over fake
// backends: the type chosen, the fallthrough rules, the cap, and which tools
// each platform asks.

// fakeImageBackend is a clipboard tool for the image read: the types it
// lists (or listErr), and what a read of a type answers. asked records the
// types read, lists counts its listings.
type fakeImageBackend struct {
	types   []string
	listErr error
	data    []byte
	readErr error
	asked   []string
	lists   int
}

func (f *fakeImageBackend) backend() clipBackend {
	return clipBackend{
		types: func(context.Context) ([]string, error) {
			f.lists++
			return f.types, f.listErr
		},
		read: func(_ context.Context, mime string) ([]byte, error) {
			f.asked = append(f.asked, mime)
			return f.data, f.readErr
		},
	}
}

// TestTheClipboardImageTypeOrder: png, then jpeg, webp, gif, then any
// image/*; nothing else is an image.
func TestTheClipboardImageTypeOrder(t *testing.T) {
	for _, tc := range []struct {
		types []string
		want  string
	}{
		{[]string{"text/plain", "image/gif", "image/png", "image/jpeg"}, "image/png"},
		{[]string{"image/gif", "image/webp", "image/jpeg"}, "image/jpeg"},
		{[]string{"image/gif", "image/webp"}, "image/webp"},
		{[]string{"TARGETS", "image/gif"}, "image/gif"},
		{[]string{"text/plain", "image/x-icon", "image/tiff"}, "image/x-icon"},
		{[]string{" image/PNG\r"}, "image/png"},
		{[]string{"text/plain", "UTF8_STRING", "TARGETS"}, ""},
		{nil, ""},
	} {
		if got := chooseImageType(tc.types); got != tc.want {
			t.Errorf("chooseImageType(%q) = %q, want %q", tc.types, got, tc.want)
		}
	}
}

// TestTheClipboardImageReadFallsThrough: a backend that fails gives way to the
// next; one with no image stops the read (the next is never asked); an image
// over the cap is refused and stops it too.
func TestTheClipboardImageReadFallsThrough(t *testing.T) {
	img := pngBytes(t, 8, 8)
	ctx := context.Background()

	failing := &fakeImageBackend{listErr: errors.New("wl-paste: not found")}
	next := &fakeImageBackend{types: []string{"image/jpeg", "image/png"}, data: img}
	got, err := readImageFrom(ctx, []clipBackend{failing.backend(), next.backend()})
	if err != nil || !bytes.Equal(got, img) || !reflect.DeepEqual(next.asked, []string{"image/png"}) {
		t.Fatalf("after a failing backend: %d bytes, %v, asked %q", len(got), err, next.asked)
	}

	readFails := &fakeImageBackend{types: []string{"image/png"}, readErr: errors.New("exit 1")}
	next = &fakeImageBackend{types: []string{"image/png"}, data: img}
	if got, err := readImageFrom(ctx, []clipBackend{readFails.backend(), next.backend()}); err != nil || !bytes.Equal(got, img) {
		t.Fatalf("after a backend whose read failed: %d bytes, %v", len(got), err)
	}

	empty := &fakeImageBackend{types: []string{"text/plain"}}
	never := &fakeImageBackend{types: []string{"image/png"}, data: img}
	if got, err := readImageFrom(ctx, []clipBackend{empty.backend(), never.backend()}); got != nil || err != nil || never.lists != 0 {
		t.Fatalf("after a backend with no image: %d bytes, %v, the next listed %d times", len(got), err, never.lists)
	}

	tooBig := &fakeImageBackend{types: []string{"image/png"}, readErr: attach.ErrSourceTooLarge}
	never = &fakeImageBackend{types: []string{"image/png"}, data: img}
	if got, err := readImageFrom(ctx, []clipBackend{tooBig.backend(), never.backend()}); got != nil || !errors.Is(err, attach.ErrSourceTooLarge) || never.lists != 0 {
		t.Fatalf("an image over the cap: %d bytes, %v, the next listed %d times", len(got), err, never.lists)
	}

	if got, err := readImageFrom(ctx, nil); got != nil || err != nil {
		t.Fatalf("no backend at all: %d bytes, %v", len(got), err)
	}
}

// TestTheClipboardImageCap: the read goes through a limit one past 20 MiB:
// exactly the cap is read, one byte more is refused without being read whole
// — and a tool past it is killed, not waited out.
func TestTheClipboardImageCap(t *testing.T) {
	at := bytes.Repeat([]byte{1}, attach.MaxSourceBytes)
	if got, err := capRead(bytes.NewReader(at), attach.MaxSourceBytes); err != nil || len(got) != attach.MaxSourceBytes {
		t.Fatalf("exactly the cap: %d bytes, %v", len(got), err)
	}
	over := &countingReader{r: bytes.NewReader(append(at, 1, 2, 3))}
	if got, err := capRead(over, attach.MaxSourceBytes); got != nil || !errors.Is(err, attach.ErrSourceTooLarge) {
		t.Fatalf("one past the cap: %d bytes, %v", len(got), err)
	}
	if over.n > attach.MaxSourceBytes+1 {
		t.Fatalf("read %d bytes of an image over the cap", over.n)
	}

	// The real runner, over a command that would write far past a small cap.
	ctx := context.Background()
	if got, err := runCapped(ctx, 64, "head", "-c", "1000000", "/dev/zero"); got != nil || !errors.Is(err, attach.ErrSourceTooLarge) {
		t.Fatalf("a tool past the cap: %d bytes, %v", len(got), err)
	}
	if got, err := runCapped(ctx, 64, "head", "-c", "64", "/dev/zero"); err != nil || len(got) != 64 {
		t.Fatalf("a tool at the cap: %d bytes, %v", len(got), err)
	}
	if _, err := runCapped(ctx, 64, "false"); err == nil {
		t.Fatal("a tool that failed answered no error")
	}
}

// countingReader counts what was read through it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestTheClipboardImageBackends: which tools are asked, in order — wl-paste
// under Wayland, xclip under X, both (Wayland first) under XWayland, none
// headless, osascript on macOS — and the arguments each runs with.
func TestTheClipboardImageBackends(t *testing.T) {
	var ran []string
	run := func(_ context.Context, _ int, name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil, errors.New("not here")
	}
	env := func(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }
	// The tool each backend runs, in order, as its type listing names it.
	tools := func(bs []clipBackend) []string {
		var out []string
		for _, b := range bs {
			ran = nil
			_, _ = b.types(context.Background())
			out = append(out, strings.Fields(ran[0])[0])
		}
		return out
	}
	for _, tc := range []struct {
		goos string
		vars map[string]string
		want []string
	}{
		{"linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, []string{"wl-paste"}},
		{"linux", map[string]string{"DISPLAY": ":0"}, []string{"xclip"}},
		{"linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0"}, []string{"wl-paste", "xclip"}},
		{"linux", nil, nil},
		{"darwin", nil, []string{"osascript"}},
	} {
		if got := tools(imageBackends(tc.goos, env(tc.vars), run)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s %v: backends %q, want %q", tc.goos, tc.vars, got, tc.want)
		}
	}
	ran = nil
	bs := imageBackends("linux", env(map[string]string{"WAYLAND_DISPLAY": "w", "DISPLAY": ":0"}), run)
	for _, b := range bs {
		_, _ = b.types(context.Background())
		_, _ = b.read(context.Background(), "image/png")
	}
	want := []string{
		"wl-paste --list-types", "wl-paste --no-newline --type image/png",
		"xclip -selection clipboard -t TARGETS -o", "xclip -selection clipboard -t image/png -o",
	}
	if !reflect.DeepEqual(ran, want) {
		t.Fatalf("ran %q, want %q", ran, want)
	}
}
