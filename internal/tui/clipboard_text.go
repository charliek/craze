package tui

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/atotto/clipboard"

	"github.com/charliek/craze/internal/harness/tool/attach"
)

// The clipboard's text (plan 033 C3r, V1 finding 1): what Ctrl+V pastes in
// the composer, the session list's input and /connect's key field.
//
// atotto/clipboard reads it on Linux with `wl-paste --no-newline` and
// `xclip -o`, neither naming a type: with only an image on the clipboard —
// a screenshot — each answers with the image's bytes, and a PNG went into the
// draft as text wherever no chip was made (a refused clipboard image, `!`
// shell mode, a session P27 refuses, the session list, the key field). So on
// Linux craze asks the clipboard which types it offers, as the image read
// does (clipboard_image.go), and reads text only when one of them is text:
//
//   - wl-paste with WAYLAND_DISPLAY set: `--list-types`, then
//     `--no-newline --type <type>`;
//   - xclip with DISPLAY set: the TARGETS listing, then `-t <type> -o`;
//   - neither able to list (not installed, no display): atotto, as before.
//
// macOS's pbpaste (atotto's) is text only already. And text that is not
// text all the same — not UTF-8, or holding a NUL — never reaches a draft
// (pastableText, readPaste): it is refused, with a note.

// clipboardTextMax bounds a text read: no one pastes a draft this long, and a
// tool must not be able to hand craze unbounded bytes.
const clipboardTextMax = attach.MaxSourceBytes

// textTypeOrder is the order a text type is chosen in: UTF-8 first, in the
// spellings Wayland (MIME) and X11 (atoms) give it, then the older plain
// forms. Any other text/* comes after these.
var textTypeOrder = []string{"text/plain;charset=utf-8", "UTF8_STRING", "text/plain", "STRING", "TEXT"}

// chooseTextType is the type to ask for among types, as a backend listed
// them, or "" when none is text.
func chooseTextType(types []string) string {
	have := map[string]string{}
	var other string
	for _, t := range types {
		t = strings.TrimSpace(t)
		lower := strings.ToLower(t)
		if _, ok := have[lower]; !ok {
			have[lower] = t
		}
		if other == "" && strings.HasPrefix(lower, "text/") {
			other = t
		}
	}
	for _, t := range textTypeOrder {
		if got, ok := have[strings.ToLower(t)]; ok {
			return got
		}
	}
	return other
}

// textBackends is the typed text readers for goos under getenv's environment:
// the image read's Linux tools (linuxBackends), read through clipboardTextMax;
// none on macOS, whose pbpaste is text only.
func textBackends(goos string, getenv func(string) string, run runner) []clipBackend {
	if goos == "darwin" {
		return nil
	}
	return linuxBackends(getenv, run, clipboardTextMax)
}

// errNoTextBackend is readTextFrom's word that no backend could list the
// clipboard's types: the caller falls back to an untyped read.
var errNoTextBackend = errors.New("tui: no clipboard tool lists types")

// readTextFrom reads the clipboard's text through backends, in order: a
// backend that fails gives way to the next; one that lists no text type
// stops the read with no text — the clipboard holds none, an image say, and
// asking another tool would not change that. errNoTextBackend when none
// could list. Text read as X11's STRING or TEXT is Latin-1 and comes back as
// UTF-8 (latin1Type).
func readTextFrom(ctx context.Context, backends []clipBackend) (string, error) {
	for _, b := range backends {
		types, err := b.types(ctx)
		if err != nil {
			continue
		}
		typ := chooseTextType(types)
		if typ == "" {
			return "", nil
		}
		data, err := b.read(ctx, typ)
		if err != nil {
			continue
		}
		if latin1Type(typ) {
			return latin1ToUTF8(data), nil
		}
		return string(data), nil
	}
	return "", errNoTextBackend
}

// latin1Type reports whether typ is one of X11's Latin-1 text types: STRING,
// which the ICCCM defines as ISO-8859-1, and TEXT, which an owner answers with
// STRING when it can. An app that offers only these — no UTF8_STRING, no
// text/plain — hands over "café" as 63 61 66 e9, not valid UTF-8, and
// pastableText would refuse it as not text (plan 033 C6r, r2 #6). XWayland
// lists them under the same names to wl-paste.
func latin1Type(typ string) bool {
	return typ == "STRING" || typ == "TEXT"
}

// latin1ToUTF8 is ISO-8859-1 text as UTF-8: each byte the code point of the
// same number. Every byte decodes, so whether it is text at all is still
// pastableText's question — an image's bytes offered as STRING carry a NUL.
func latin1ToUTF8(b []byte) string {
	var s strings.Builder
	s.Grow(len(b) + len(b)/2)
	for _, c := range b {
		s.WriteRune(rune(c))
	}
	return s.String()
}

// readClipboardText is nativePaste's production reader: the typed backends,
// bounded by clipboardTimeout, and atotto's untyped read where none
// could list the clipboard's types (macOS's pbpaste among them).
func readClipboardText() (string, error) {
	return readTextVia(runtime.GOOS, os.Getenv, runCapped, clipboard.ReadAll)
}

// readTextVia is readClipboardText with its seams: the platform, the
// environment, the command runner and the untyped fallback.
func readTextVia(goos string, getenv func(string) string, run runner, untyped func() (string, error)) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
	defer cancel()
	text, err := readTextFrom(ctx, textBackends(goos, getenv, run))
	if errors.Is(err, errNoTextBackend) {
		return untyped()
	}
	return text, err
}

// clipboardNotText is the note for clipboard "text" that is not text.
const clipboardNotText = "the clipboard's text was not pasted: it is not text"

// pastableText reports whether text read off the clipboard may go into a
// draft or a field: valid UTF-8 with no NUL in it. Anything else is binary
// that slipped past the typed read — an image's bytes, a tool's garbage —
// and a draft is never the place for it.
func pastableText(text string) bool {
	return utf8.ValidString(text) && !strings.ContainsRune(text, 0)
}
