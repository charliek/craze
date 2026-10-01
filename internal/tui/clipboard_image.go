package tui

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/charliek/craze/internal/harness/tool/attach"
)

// The clipboard's image (plan 033 §3.3): what Ctrl+V, Alt+V and an empty
// bracketed paste read before they read text (pasteFromClipboard,
// probeClipboardImage). atotto/clipboard, which reads the text, is text only
// on every platform — pbpaste cannot emit image data at all — so the image is
// asked of the platform's own tools:
//
//   - Linux: wl-paste with WAYLAND_DISPLAY set; xclip with DISPLAY set. Each
//     lists the types the clipboard offers (wl-paste --list-types; xclip's
//     TARGETS), and the image type is chosen png, then jpeg, webp, gif, then
//     any image/* (imageTypeOrder).
//   - macOS: osascript, which says whether the clipboard holds «class PNGf»
//     (clipboard info) and writes it to a temporary file, removed after.
//
// A backend that fails — not installed, no display, an error — gives way to
// the next; one that answers with no image type stops the read: the
// clipboard holds no image, and asking another tool would not change that.
// Every read is bounded by clipboardImageTimeout and by attach.MaxSourceBytes
// (a larger image is refused with attach.ErrSourceTooLarge, never read
// whole), and it runs inside a tea.Cmd, never on the Update.

// clipboardImageTimeout bounds the whole image read, every backend tried.
const clipboardImageTimeout = 3 * time.Second

// imageBackend is one platform tool that can read the clipboard's image: the
// types it says the clipboard offers, and the bytes of one of them, read
// through the cap.
type imageBackend struct {
	types func(ctx context.Context) ([]string, error)
	read  func(ctx context.Context, mime string) ([]byte, error)
}

// imageTypeOrder is the order an image type is chosen in: the formats every
// provider takes first, gif last of the named ones (only its first frame is
// kept), then anything else that calls itself an image.
var imageTypeOrder = []string{attach.MIMEPNG, attach.MIMEJPEG, attach.MIMEWebP, "image/gif"}

// chooseImageType is the type to ask for among types, as a backend listed
// them (one per entry, whitespace trimmed), or "" when none is an image.
func chooseImageType(types []string) string {
	have := map[string]bool{}
	var other string
	for _, t := range types {
		t = strings.ToLower(strings.TrimSpace(t))
		have[t] = true
		if other == "" && strings.HasPrefix(t, "image/") {
			other = t
		}
	}
	for _, t := range imageTypeOrder {
		if have[t] {
			return t
		}
	}
	return other
}

// readImageFrom reads the clipboard's image through backends, in order (the
// fallthrough rules above): the bytes, nil for no image, or
// attach.ErrSourceTooLarge for an image over the cap.
func readImageFrom(ctx context.Context, backends []imageBackend) ([]byte, error) {
	for _, b := range backends {
		types, err := b.types(ctx)
		if err != nil {
			continue
		}
		mime := chooseImageType(types)
		if mime == "" {
			return nil, nil
		}
		data, err := b.read(ctx, mime)
		switch {
		case errors.Is(err, attach.ErrSourceTooLarge):
			return nil, err
		case err != nil:
			continue
		case len(data) == 0:
			return nil, nil
		}
		return data, nil
	}
	return nil, nil
}

// readClipboardImage is nativePasteImage's production reader: this platform's
// backends, bounded by clipboardImageTimeout.
func readClipboardImage() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardImageTimeout)
	defer cancel()
	return readImageFrom(ctx, imageBackends(runtime.GOOS, os.Getenv, runCapped))
}

// runner runs a command and answers its standard output, refusing one longer
// than max bytes (attach.ErrSourceTooLarge). runCapped is the real one; a
// test hands imageBackends another.
type runner func(ctx context.Context, max int, name string, args ...string) ([]byte, error)

// imageBackends is the backend list for goos under getenv's environment.
func imageBackends(goos string, getenv func(string) string, run runner) []imageBackend {
	var out []imageBackend
	switch goos {
	case "darwin":
		out = append(out, osascriptBackend(run))
	default:
		if getenv("WAYLAND_DISPLAY") != "" {
			out = append(out, imageBackend{
				types: func(ctx context.Context) ([]string, error) {
					return listTypes(ctx, run, "wl-paste", "--list-types")
				},
				read: func(ctx context.Context, mime string) ([]byte, error) {
					return run(ctx, attach.MaxSourceBytes, "wl-paste", "--no-newline", "--type", mime)
				},
			})
		}
		if getenv("DISPLAY") != "" {
			out = append(out, imageBackend{
				types: func(ctx context.Context) ([]string, error) {
					return listTypes(ctx, run, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
				},
				read: func(ctx context.Context, mime string) ([]byte, error) {
					return run(ctx, attach.MaxSourceBytes, "xclip", "-selection", "clipboard", "-t", mime, "-o")
				},
			})
		}
	}
	return out
}

// typesMax bounds a type listing: a few hundred bytes in practice.
const typesMax = 64 << 10

// listTypes runs a type listing and splits it into lines.
func listTypes(ctx context.Context, run runner, name string, args ...string) ([]string, error) {
	out, err := run(ctx, typesMax, name, args...)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(out), "\n"), nil
}

// osascriptBackend is macOS's: `clipboard info` lists what the clipboard
// holds, and a PNG («class PNGf») is the one type asked for — the form macOS
// puts a screenshot on the clipboard in. It is written to a temporary file by
// osascript itself (its standard output cannot carry binary), read through
// the cap and removed.
func osascriptBackend(run runner) imageBackend {
	return imageBackend{
		types: func(ctx context.Context) ([]string, error) {
			out, err := run(ctx, typesMax, "osascript", "-e", "clipboard info")
			if err != nil {
				return nil, err
			}
			if strings.Contains(string(out), "«class PNGf»") {
				return []string{attach.MIMEPNG}, nil
			}
			return nil, nil
		},
		read: func(ctx context.Context, _ string) ([]byte, error) {
			f, err := os.CreateTemp("", "craze-clipboard-*.png")
			if err != nil {
				return nil, err
			}
			path := f.Name()
			_ = f.Close()
			defer os.Remove(path)
			script := []string{
				"-e", "set png to the clipboard as «class PNGf»",
				"-e", "set f to open for access POSIX file " + appleScriptString(path) + " with write permission",
				"-e", "set eof f to 0",
				"-e", "write png to f",
				"-e", "close access f",
			}
			if _, err := run(ctx, typesMax, "osascript", script...); err != nil {
				return nil, err
			}
			return readSource(path)
		},
	}
}

// appleScriptString quotes s as an AppleScript string literal.
func appleScriptString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// capRead reads r through a limit one past max: more than max is
// attach.ErrSourceTooLarge.
func capRead(r io.Reader, max int) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > max {
		return nil, attach.ErrSourceTooLarge
	}
	return b, nil
}

// runCapped is the real runner: the command's output read through the cap,
// the command killed — not waited out — once it is past it, and ctx's
// deadline killing it too.
func runCapped(ctx context.Context, max int, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b, rerr := capRead(out, max)
	if rerr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, rerr
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return b, nil
}
