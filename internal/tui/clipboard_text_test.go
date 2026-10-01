package tui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The clipboard's text read (plan 033 C3r, V1 finding 1, clipboard_text.go)
// over fake tools, and binary text refused wherever a paste lands.

// pngClip is what a clipboard holding only a screenshot hands an untyped read:
// the image's bytes.
var pngClip = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x10"

// fakeClipboard is a clipboard behind wl-paste and xclip: the types each
// lists (a tool missing from tools fails to run at all), and a typed read of
// a type it lists answers that type's data. ran records every command.
type fakeClipboard struct {
	tools map[string][]string // tool → the types it lists
	data  map[string]string   // type → its data
	ran   []string
}

func (f *fakeClipboard) run(_ context.Context, max int, name string, args ...string) ([]byte, error) {
	f.ran = append(f.ran, name+" "+strings.Join(args, " "))
	types, ok := f.tools[name]
	if !ok {
		return nil, errors.New("not installed")
	}
	joined := " " + strings.Join(args, " ") + " "
	if strings.Contains(joined, " --list-types ") || strings.Contains(joined, " TARGETS ") {
		return []byte(strings.Join(types, "\n") + "\n"), nil
	}
	for i, a := range args {
		if (a == "--type" || a == "-t") && i+1 < len(args) {
			if d, ok := f.data[args[i+1]]; ok && len(d) <= max {
				return []byte(d), nil
			}
			return nil, errors.New("no such type")
		}
	}
	// An untyped read: the bytes of whatever comes first (atotto's).
	return []byte(f.data[types[0]]), nil
}

// TestTheClipboardTextReadAsksForAType is V1 finding 1's fix on every target:
// text is read only as a type the clipboard offers as text; a clipboard of an
// image alone has no text — the image's bytes are never asked for as text —
// on Wayland and on X; a tool that cannot list gives way to the next, and to
// the untyped read (atotto) when none can, as on macOS, whose pbpaste is text
// only.
func TestTheClipboardTextReadAsksForAType(t *testing.T) {
	env := func(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }
	wayland := map[string]string{"WAYLAND_DISPLAY": "wayland-0"}
	x11 := map[string]string{"DISPLAY": ":0"}
	both := map[string]string{"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0"}
	for _, tc := range []struct {
		name      string
		goos      string
		vars      map[string]string
		clip      fakeClipboard
		want      string
		wantRan   []string
		wantUntyp bool
	}{
		{"wayland, an image alone", "linux", wayland,
			fakeClipboard{tools: map[string][]string{"wl-paste": {"image/png"}}, data: map[string]string{"image/png": pngClip}},
			"", []string{"wl-paste --list-types"}, false},
		{"wayland, text and an image", "linux", wayland,
			fakeClipboard{tools: map[string][]string{"wl-paste": {"image/png", "text/plain;charset=utf-8", "UTF8_STRING"}},
				data: map[string]string{"image/png": pngClip, "text/plain;charset=utf-8": "hello"}},
			"hello", []string{"wl-paste --list-types", "wl-paste --no-newline --type text/plain;charset=utf-8"}, false},
		{"x11, an image alone", "linux", x11,
			fakeClipboard{tools: map[string][]string{"xclip": {"TARGETS", "TIMESTAMP", "image/png"}}, data: map[string]string{"TARGETS": "", "image/png": pngClip}},
			"", []string{"xclip -selection clipboard -t TARGETS -o"}, false},
		{"x11, text", "linux", x11,
			fakeClipboard{tools: map[string][]string{"xclip": {"TARGETS", "UTF8_STRING", "STRING", "TEXT"}}, data: map[string]string{"UTF8_STRING": "héllo"}},
			"héllo", []string{"xclip -selection clipboard -t TARGETS -o", "xclip -selection clipboard -t UTF8_STRING -o"}, false},
		{"xwayland, wl-paste missing", "linux", both,
			fakeClipboard{tools: map[string][]string{"xclip": {"image/png"}}, data: map[string]string{"image/png": pngClip}},
			"", []string{"wl-paste --list-types", "xclip -selection clipboard -t TARGETS -o"}, false},
		{"no tool lists", "linux", both,
			fakeClipboard{tools: map[string][]string{}},
			"untyped", []string{"wl-paste --list-types", "xclip -selection clipboard -t TARGETS -o"}, true},
		{"headless", "linux", nil, fakeClipboard{}, "untyped", nil, true},
		{"macOS", "darwin", both, fakeClipboard{}, "untyped", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			untyped := false
			got, err := readTextVia(tc.goos, env(tc.vars), tc.clip.run, func() (string, error) {
				untyped = true
				return "untyped", nil
			})
			if err != nil || got != tc.want || untyped != tc.wantUntyp {
				t.Fatalf("read %q, %v (untyped %v); want %q (untyped %v)", got, err, untyped, tc.want, tc.wantUntyp)
			}
			if !reflect.DeepEqual(tc.clip.ran, tc.wantRan) {
				t.Fatalf("ran %q, want %q", tc.clip.ran, tc.wantRan)
			}
		})
	}
}

// TestLatin1ClipboardTextIsDecoded is r2 #6 (plan 033 C6r): text an app
// offers only as X11's STRING or TEXT is Latin-1 — "café" is 63 61 66 e9 — and
// is read as UTF-8, so it pastes instead of being refused as not text; through
// xclip and through wl-paste (XWayland's names). UTF-8 types are untouched,
// and an image's bytes offered as STRING are still not text: they carry a NUL.
func TestLatin1ClipboardTextIsDecoded(t *testing.T) {
	env := func(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }
	x11 := map[string]string{"DISPLAY": ":0"}
	wayland := map[string]string{"WAYLAND_DISPLAY": "wayland-0"}
	for _, tc := range []struct {
		name string
		vars map[string]string
		clip fakeClipboard
		want string
		text bool
	}{
		{"x11 STRING", x11,
			fakeClipboard{tools: map[string][]string{"xclip": {"TARGETS", "STRING"}}, data: map[string]string{"STRING": "caf\xe9"}},
			"café", true},
		{"x11 TEXT", x11,
			fakeClipboard{tools: map[string][]string{"xclip": {"TARGETS", "TEXT"}}, data: map[string]string{"TEXT": "na\xefve \xbd\xb0"}},
			"naïve ½°", true},
		{"xwayland STRING through wl-paste", wayland,
			fakeClipboard{tools: map[string][]string{"wl-paste": {"STRING", "TEXT"}}, data: map[string]string{"STRING": "\xc5ngstr\xf6m"}},
			"Ångström", true},
		{"UTF8_STRING is not decoded again", x11,
			fakeClipboard{tools: map[string][]string{"xclip": {"TARGETS", "UTF8_STRING", "STRING"}}, data: map[string]string{"UTF8_STRING": "café"}},
			"café", true},
		{"an image's bytes as STRING", x11,
			fakeClipboard{tools: map[string][]string{"xclip": {"TARGETS", "STRING"}}, data: map[string]string{"STRING": pngClip}},
			latin1ToUTF8([]byte(pngClip)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readTextVia("linux", env(tc.vars), tc.clip.run, func() (string, error) {
				t.Fatal("the untyped read ran")
				return "", nil
			})
			if err != nil || got != tc.want {
				t.Fatalf("read %q, %v; want %q", got, err, tc.want)
			}
			msg := readPaste(func() (string, error) { return got, nil }, pasteMsg{}).(pasteMsg)
			if pasted := msg.text == tc.want && !msg.textRefused; pasted != tc.text {
				t.Fatalf("readPaste = %#v; pasted %v, want %v", msg, pasted, tc.text)
			}
		})
	}
}

// TestTheClipboardTextTypeOrder: UTF-8 first in either spelling, then the
// older plain forms, then any text/*; nothing else is text.
func TestTheClipboardTextTypeOrder(t *testing.T) {
	for _, tc := range []struct {
		types []string
		want  string
	}{
		{[]string{"image/png", "TEXT", "STRING", "UTF8_STRING"}, "UTF8_STRING"},
		{[]string{"text/plain", "text/plain;charset=utf-8"}, "text/plain;charset=utf-8"},
		{[]string{"TARGETS", "STRING"}, "STRING"},
		{[]string{"text/html", "image/png"}, "text/html"},
		{[]string{" UTF8_STRING\r"}, "UTF8_STRING"},
		{[]string{"image/png", "TARGETS", "TIMESTAMP"}, ""},
		{nil, ""},
	} {
		if got := chooseTextType(tc.types); got != tc.want {
			t.Errorf("chooseTextType(%q) = %q, want %q", tc.types, got, tc.want)
		}
	}
}

// TestBinaryClipboardTextNeverLands: "text" that is not text — an image's
// bytes, a NUL, invalid UTF-8 — that slips past the typed read anyway is
// dropped by every read (readPaste) and lands nowhere: not the composer, not a
// `!` command line, not after a refused clipboard image, not the session
// list's input, not /connect's key field — with a note each time. Text is
// still text.
func TestBinaryClipboardTextNeverLands(t *testing.T) {
	for _, bad := range []string{pngClip, "ok\x00", "\xff\xfe"} {
		msg := readPaste(func() (string, error) { return bad, nil }, pasteMsg{}).(pasteMsg)
		if msg.text != "" || !msg.textRefused {
			t.Fatalf("readPaste(%q) = %#v", bad, msg)
		}
	}
	if msg := readPaste(func() (string, error) { return "plain ✓\ttext\n", nil }, pasteMsg{}).(pasteMsg); msg.text != "plain ✓\ttext\n" || msg.textRefused {
		t.Fatalf("readPaste of text = %#v", msg)
	}

	prevText := clipboardRead
	clipboardRead = func() (string, error) { return pngClip, nil }
	t.Cleanup(func() { clipboardRead = prevText })
	stubClipboardImage(t, func() ([]byte, error) { return nil, nil })
	refused := func(t *testing.T, what string, m Model) {
		t.Helper()
		if !strings.Contains(m.copyNote, clipboardNotText) {
			t.Fatalf("%s: the note is %q", what, m.copyNote)
		}
	}

	// The composer, its `!` command line, and a clipboard image it refuses.
	m, _ := imageModel(t)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = deliver(t, tm.(Model), runCmd(cmd))
	if m.input.Value() != "" {
		t.Fatalf("the composer took %q", m.input.Value())
	}
	refused(t, "the composer", m)

	s, _ := imageModel(t)
	s = typeComposer(t, s, "!file ")
	tm, cmd = s.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	s = deliver(t, tm.(Model), runCmd(cmd))
	if s.input.Value() != "!file " {
		t.Fatalf("the command line took %q", s.input.Value())
	}
	refused(t, "a command line", s)

	stubClipboardImage(t, func() ([]byte, error) { return pngBytes(t, 4, 4), nil })
	r, _ := imageModel(t)
	tm, cmd = r.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	r = deliver(t, tm.(Model), runCmd(cmd))
	if r.input.Value() != "" || len(r.images.list) != 0 {
		t.Fatalf("a refused clipboard image left %q, %d images", r.input.Value(), len(r.images.list))
	}
	if !strings.Contains(r.copyNote, "the clipboard image was not pasted: smaller than 8×8 pixels") {
		t.Fatalf("a refused clipboard image's note: %q", r.copyNote)
	}
	refused(t, "a refused clipboard image", r)

	// The session list's input.
	l, fs, _ := newSessModel(t, 100, 30)
	l = newList(t, l, fs)
	tm, cmd = l.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	l = deliver(t, tm.(Model), runCmd(cmd))
	if v, _ := inputOf(l); v != "" {
		t.Fatalf("the list's input took %q", v)
	}
	refused(t, "the session list", l)

	// /connect's key field.
	k := connectKeyStep(t, nativeStub(), t.TempDir(), func(string) string { return "" })
	tm, cmd = k.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	k = deliver(t, tm.(Model), runCmd(cmd))
	if got := k.cdlg.key.Value(); got != "" {
		t.Fatalf("the key field took %q", got)
	}
	refused(t, "the key field", k)
}
