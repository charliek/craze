package tui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
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

// TestLegacyClipboardTextIsDecoded is r2 #6 (plan 033 C6r) and r4 #9 (C6r2):
// text an app offers only as X11's STRING or TEXT is Windows-1252 — "café" is
// 63 61 66 e9, curly quotes are 93 and 94 — and is read as UTF-8, so it pastes
// instead of being refused as not text, its quotes, dashes and euro signs as
// themselves rather than C1 controls the composer would drop; the five bytes
// Windows-1252 leaves undefined are dropped; through xclip and through
// wl-paste (XWayland's names). UTF-8 types are untouched, and an image's bytes
// offered as STRING are still not text: they carry a NUL.
func TestLegacyClipboardTextIsDecoded(t *testing.T) {
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
			windows1252ToUTF8([]byte(pngClip)), false},
		// C6r2, r4 #9: 0x80–0x9F is Windows-1252's, not C1 controls.
		{"x11 STRING curly quotes", x11,
			fakeClipboard{tools: map[string][]string{"xclip": {"TARGETS", "STRING"}}, data: map[string]string{"STRING": "\x93quoted\x94 \x91it\x92s\x85"}},
			"“quoted” ‘it’s…", true},
		{"xwayland TEXT dashes and the euro", wayland,
			fakeClipboard{tools: map[string][]string{"wl-paste": {"TEXT"}}, data: map[string]string{"TEXT": "5\x80 \x96 6\x80 \x97 \x99"}},
			"5€ – 6€ — ™", true},
		{"the five undefined bytes are dropped", x11,
			fakeClipboard{tools: map[string][]string{"xclip": {"TARGETS", "STRING"}}, data: map[string]string{"STRING": "a\x81b\x8dc\x8fd\x90e\x9df"}},
			"abcdef", true},
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

// TestWindows1252ToUTF8 pins the decode byte by byte: ASCII and 0xa0–0xff
// are the code point of the same number, 0x80–0x9f Windows-1252's characters,
// and the five undefined bytes nothing.
func TestWindows1252ToUTF8(t *testing.T) {
	var all []byte
	for c := range 256 {
		all = append(all, byte(c))
	}
	got := []rune(windows1252ToUTF8(all))
	if len(got) != 256-5 {
		t.Fatalf("%d runes from 256 bytes, want 251", len(got))
	}
	want := []rune("€‚ƒ„…†‡ˆ‰Š‹ŒŽ‘’“”•–—˜™š›œžŸ")
	for i, r := range got {
		switch {
		case i < 0x80 && r != rune(i):
			t.Fatalf("byte %#x decoded as %U", i, r)
		case i >= 0x80 && i < 0x80+len(want) && r != want[i-0x80]:
			t.Fatalf("Windows-1252 rune %d is %U, want %U", i-0x80, r, want[i-0x80])
		case i >= 0x80+len(want) && r != rune(i+5):
			t.Fatalf("byte %#x decoded as %U", i+5, r)
		}
	}
}

// TestPastedControlsAreStripped is r4 #9's other half (plan 033 C6r2): text
// off the clipboard that is text loses every C0 and C1 control and DEL before
// it reaches a draft or a field — an ESC and what follows it as plain text, a
// BEL, NEL, CSI — while newline, tab and carriage return go on as before
// (the composer makes newlines of \r\n as it does for a terminal's paste).
// Binary is still refused, not cleaned.
func TestPastedControlsAreStripped(t *testing.T) {
	for in, want := range map[string]string{
		"a\x1b[31mb\x07c\u0085d\x7fe\u009bf\r\ng\th": "a[31mbcdef\r\ng\th",
		"\x1b\x07\u0090\u009c":                       "",
		"plain “text” ✓":                             "plain “text” ✓",
	} {
		msg := readPaste(func() (string, error) { return in, nil }, pasteMsg{}).(pasteMsg)
		if msg.text != want || msg.textRefused {
			t.Errorf("readPaste(%q) = %q (refused %v), want %q", in, msg.text, msg.textRefused, want)
		}
	}
	if msg := readPaste(func() (string, error) { return "a\x1bb\x00", nil }, pasteMsg{}).(pasteMsg); msg.text != "" || !msg.textRefused {
		t.Fatalf("text with a NUL: %#v", msg)
	}
}

// TestAPastedControlLeavesTheChipSending is r4 #9 end to end (plan 033 C6r2):
// X11 STRING text with curly quotes and an escape sequence is pasted with
// Ctrl+V, then the clipboard's image as a chip, in one draft. The draft holds
// the quotes as themselves and no control, so the envelope's label check
// (agent.ReadAttachments) sees [Image #1] and the image is sent.
func TestAPastedControlLeavesTheChipSending(t *testing.T) {
	clip := fakeClipboard{tools: map[string][]string{"xclip": {"TARGETS", "STRING"}},
		data: map[string]string{"STRING": "\x93look\x94 at\x1b[2J this "}}
	env := func(k string) string { return map[string]string{"DISPLAY": ":0"}[k] }
	prevText := clipboardRead
	clipboardRead = func() (string, error) {
		return readTextVia("linux", env, clip.run, func() (string, error) { return "", errors.New("the untyped read ran") })
	}
	t.Cleanup(func() { clipboardRead = prevText })
	var img []byte
	stubClipboardImage(t, func() ([]byte, error) { return img, nil })

	m, stub := imageModel(t)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = deliver(t, tm.(Model), runCmd(cmd))
	const typed = "“look” at[2J this "
	if got := m.input.Value(); got != typed {
		t.Fatalf("the pasted text is %q, want %q", got, typed)
	}
	img = pngBytes(t, 16, 16)
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = settleImages(t, deliver(t, tm.(Model), runCmd(cmd)))
	if got := m.input.Value(); got != typed+"[Image #1]" || len(m.images.list) != 1 {
		t.Fatalf("the draft is %q with %d images", got, len(m.images.list))
	}
	tm, _ = m.Update(enter())
	m = tm.(Model)
	sent := stub.Prompts()
	if len(sent) != 1 {
		t.Fatalf("prompts %q", sent)
	}
	refs, rest, problems := agent.SplitAttachments(sent[0])
	if len(refs) != 1 || problems != nil || rest != typed+"[Image #1]" {
		t.Fatalf("sent %d refs (%q) before %q", len(refs), problems, rest)
	}
	atts, fallbacks, problems := agent.ReadAttachments(m.attachDir, refs, rest)
	if len(atts) != 1 || fallbacks != nil || problems != nil {
		t.Fatalf("the host reads %d images, fallbacks %q, problems %q", len(atts), fallbacks, problems)
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
