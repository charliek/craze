package tui

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/tool/attach"
	"github.com/charliek/craze/internal/roster"
)

// The composer's images (plan 033 §3.3, A1, A1b, A2, A2b, A3b, A6): the
// paste tokenizer and the path rules, then through the model — paste → chip →
// processing → envelope — and every place the draft goes.

// imageModel is a started model over a stub (startStub's: its HOME isolated,
// so its attachments directory is a temporary one).
func imageModel(t *testing.T) (Model, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	m := startStub(t, stub, t.TempDir(), 80, 24)
	if m.attachDir == "" || !strings.HasPrefix(m.attachDir, os.Getenv("HOME")) {
		t.Fatalf("fixture: the attachments directory %q is not under the test's HOME", m.attachDir)
	}
	return m, stub
}

// shotPNG is the PNG most tests paste: shot.png, 16×16, in a directory of
// its own.
func shotPNG(t *testing.T) string {
	t.Helper()
	return writePNG(t, t.TempDir(), "shot.png", 16, 16)
}

// stubClipboardImage installs read as the clipboard's image for the test.
func stubClipboardImage(t *testing.T, read func() ([]byte, error)) {
	t.Helper()
	prev := clipboardReadImage
	clipboardReadImage = read
	t.Cleanup(func() { clipboardReadImage = prev })
}

// pasteText is a terminal's bracketed paste of text into the model.
func pasteText(t *testing.T, m Model, text string) Model {
	t.Helper()
	tm, _ := m.Update(pasteKey(text))
	return tm.(Model)
}

// settleImages runs the processing of every chip still pending, as the
// program would (processCmd), and delivers each answer.
func settleImages(t *testing.T, m Model) Model {
	t.Helper()
	for _, d := range []draftImages{m.images, m.editImages} {
		for _, a := range d.list {
			if a.pending {
				m = deliver(t, m, m.processCmd(a)())
			}
		}
	}
	return m
}

// sentImages is the envelope in front of a sent prompt, decoded.
func sentImages(t *testing.T, sent string) []agent.AttachmentRef {
	t.Helper()
	refs, _, problems := agent.SplitAttachments(sent)
	if problems != nil {
		t.Fatalf("the sent envelope has problems: %q", problems)
	}
	return refs
}

// pngBytes is a w×h PNG, a gradient so it is not all one byte.
func pngBytes(t testing.TB, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: uint8(x + y), A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// writePNG writes a w×h PNG to dir/name and answers its path.
func writePNG(t testing.TB, dir, name string, w, h int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pngBytes(t, w, h), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func words(toks []pasteToken) []string {
	var out []string
	for _, tk := range toks {
		out = append(out, tk.word)
	}
	return out
}

func raws(toks []pasteToken) []string {
	var out []string
	for _, tk := range toks {
		out = append(out, tk.raw)
	}
	return out
}

// TestSplitPaste is the shell-like split (§3.3): whitespace — CR and LF
// among it — separates; quotes keep; a backslash escapes; an unterminated
// quote is not a list of paths at all.
func TestSplitPaste(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		words []string
		raws  []string
		ok    bool
	}{
		{"one path", "/a/b.png", []string{"/a/b.png"}, []string{"/a/b.png"}, true},
		{"spaces, tabs, CR and LF separate", " /a.png\t/b.png\r\n/c.png\n", []string{"/a.png", "/b.png", "/c.png"}, []string{"/a.png", "/b.png", "/c.png"}, true},
		{"single quotes keep a space", `'/tmp/my shot.png'`, []string{"/tmp/my shot.png"}, []string{`'/tmp/my shot.png'`}, true},
		{"double quotes, with escapes inside", `"/tmp/a \"b\" \\c.png"`, []string{`/tmp/a "b" \c.png`}, []string{`"/tmp/a \"b\" \\c.png"`}, true},
		{"a backslash escapes a space", `/tmp/my\ shot.png /b.png`, []string{"/tmp/my shot.png", "/b.png"}, []string{`/tmp/my\ shot.png`, "/b.png"}, true},
		{"a backslash before a line break joins", "/tmp/a\\\nb.png", []string{"/tmp/ab.png"}, []string{"/tmp/a\\\nb.png"}, true},
		{"quoted and bare parts run together", `/tmp/'a b'"c".png`, []string{"/tmp/a bc.png"}, []string{`/tmp/'a b'"c".png`}, true},
		{"a trailing backslash is itself", `/tmp/a\`, []string{`/tmp/a\`}, []string{`/tmp/a\`}, true},
		{"an empty quoted word is a word", `''`, []string{""}, []string{`''`}, true},
		{"only whitespace", " \n\t", nil, nil, true},
		{"an unterminated single quote", `'/tmp/a.png`, nil, nil, false},
		{"an unterminated double quote", `"/tmp/a.png`, nil, nil, false},
		{"multibyte runes and an NBSP separator", "/tmp/é.png /ü.png", []string{"/tmp/é.png", "/ü.png"}, []string{"/tmp/é.png", "/ü.png"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			toks, ok := splitPaste(tc.in)
			if ok != tc.ok || !reflect.DeepEqual(words(toks), tc.words) || !reflect.DeepEqual(raws(toks), tc.raws) {
				t.Fatalf("splitPaste(%q) = words %q, raws %q, ok %v; want %q, %q, %v", tc.in, words(toks), raws(toks), ok, tc.words, tc.raws, tc.ok)
			}
		})
	}
}

// FuzzSplitPaste: the tokenizer is total over any bytes — no panic, no loop —
// every raw word is the paste's own text, in order, and the words it reads
// are read again from its raw words put back together with a space.
func FuzzSplitPaste(f *testing.F) {
	for _, s := range []string{
		"/a/b.png", `'/tmp/my shot.png' "/x \"y\".png"`, "file:///tmp/a%20b.png\r\n~/c.png",
		`a\`, `"`, `'`, "\\\n", "\xff\xfe /\x00.png", "  x", `""''\ `,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		toks, ok := splitPaste(s)
		if !ok {
			if toks != nil {
				t.Fatalf("a refused paste answered words: %q", raws(toks))
			}
			return
		}
		at := 0
		for _, tk := range toks {
			i := strings.Index(s[at:], tk.raw)
			if tk.raw == "" || i < 0 {
				t.Fatalf("raw word %q is not the paste's text after offset %d of %q", tk.raw, at, s)
			}
			at += i + len(tk.raw)
		}
		again, ok := splitPaste(strings.Join(raws(toks), " "))
		if !ok || !reflect.DeepEqual(words(again), words(toks)) {
			t.Fatalf("re-reading %q gave %q (ok %v), first read %q", raws(toks), words(again), ok, words(toks))
		}
	})
}

// TestPastePath: a word is an image path when, after a file:// URI is decoded
// or a leading ~/ expanded, it is absolute with an image file's extension.
func TestPastePath(t *testing.T) {
	for _, tc := range []struct {
		word, want string
		ok         bool
	}{
		{"/tmp/shot.png", "/tmp/shot.png", true},
		{"/tmp/SHOT.JPEG", "/tmp/SHOT.JPEG", true},
		{"/tmp/a.jpg", "/tmp/a.jpg", true},
		{"/tmp/a.gif", "/tmp/a.gif", true},
		{"/tmp/a.webp", "/tmp/a.webp", true},
		{"/tmp/a.bmp", "/tmp/a.bmp", true},
		{"file:///tmp/my%20shot.png", "/tmp/my shot.png", true},
		{"file://localhost/tmp/a.png", "/tmp/a.png", true},
		{"~/Desktop/a.png", "/home/u/Desktop/a.png", true},
		{"file://otherhost/tmp/a.png", "", false},
		{"file:///tmp/a.png?x=1", "", false},
		{"relative/a.png", "", false},
		{"~other/a.png", "", false},
		{"/tmp/a.txt", "", false},
		{"/tmp/png", "", false},
		{"", "", false},
	} {
		got, ok := pastePath(tc.word, "/home/u")
		if ok != tc.ok || got != tc.want {
			t.Errorf("pastePath(%q) = %q, %v; want %q, %v", tc.word, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := pastePath("~/a.png", ""); ok {
		t.Error("~/ with no home expanded")
	}
}

// TestPrecheckImage is the synchronous pre-check: a regular file, at most 20
// MiB, whose header decodes as an image of at least 8×8.
func TestPrecheckImage(t *testing.T) {
	dir := t.TempDir()
	good := writePNG(t, dir, "good.png", 16, 16)
	if size, err := precheckImage(good); err != nil || size <= 0 {
		t.Fatalf("a good PNG: %d, %v", size, err)
	}
	tiny := writePNG(t, dir, "tiny.png", 4, 4)
	link := filepath.Join(dir, "link.png")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	notImage := filepath.Join(dir, "not.png")
	if err := os.WriteFile(notImage, []byte("not an image at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.png")
	if err := os.WriteFile(big, pngBytes(t, 16, 16), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, attach.MaxSourceBytes+1); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"under 8×8": tiny, "a symlink": link, "not an image": notImage, "over 20 MiB": big,
		"a directory": dir, "missing": filepath.Join(dir, "gone.png"),
	} {
		if _, err := precheckImage(path); err == nil {
			t.Errorf("%s passed the pre-check", name)
		}
	}
}

// ------------------------------------------------------- through the model

// TestAPastedImagePathBecomesAChip is A1's core: a bracketed paste of a path
// to a real PNG is the chip [Image #1] — the path is nowhere in the draft —
// processed into the attachments directory (0600, in a 0700 directory), and a
// send carries its envelope in front of the text, the chip in the text.
// Accepted, the draft and its sidecar go, and the next paste is #1 again.
func TestAPastedImagePathBecomesAChip(t *testing.T) {
	m, stub := imageModel(t)
	shot := writePNG(t, t.TempDir(), "shot.png", 40, 20)
	m = pasteText(t, m, shot)
	if got := m.input.Value(); got != "[Image #1]" {
		t.Fatalf("the draft after the paste: %q", got)
	}
	if len(m.images.list) != 1 || !m.images.list[0].pending {
		t.Fatalf("the sidecar after the paste: %+v", m.images)
	}
	m = settleImages(t, m)
	a := m.images.list[0]
	if a.pending || filepath.Dir(a.path) != m.attachDir || a.mime != attach.MIMEPNG || a.w != 40 || a.h != 20 || a.ow != 0 {
		t.Fatalf("the processed entry: %+v", a)
	}
	if fi, err := os.Stat(a.path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the stored file: %v, %v", fi, err)
	}
	if fi, err := os.Stat(m.attachDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the attachments directory: %v, %v", fi, err)
	}
	m = typeComposer(t, m, " what is this?")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	sent := stub.Prompts()
	if len(sent) != 1 {
		t.Fatalf("prompts %q", sent)
	}
	want := agent.AttachmentBlock([]agent.AttachmentRef{{N: 1, Path: a.path, MIME: attach.MIMEPNG}}) + "[Image #1] what is this?"
	if sent[0] != want {
		t.Fatalf("sent %q, want %q", sent[0], want)
	}
	if got := texts(m, entryUser); len(got) != 1 || got[0] != "[Image #1] what is this?" {
		t.Fatalf("the transcript shows %q", got)
	}
	if m.input.Value() != "" || len(m.images.list) != 0 || m.images.last != 0 {
		t.Fatalf("after the accepted send: draft %q, sidecar %+v", m.input.Value(), m.images)
	}
	if strings.Contains(plainView(m), shot) || strings.Contains(plainView(m), "craze_attachments") {
		t.Fatalf("the path or the envelope is on screen:\n%s", plainView(m))
	}
}

// TestThePasteStartsItsProcessing: the paste's own command is the chip's
// processing, which answers for the chip's entry under the shown generation.
func TestThePasteStartsItsProcessing(t *testing.T) {
	m, _ := imageModel(t)
	tm, cmd := m.Update(pasteKey(shotPNG(t)))
	m = tm.(Model)
	msg, ok := runWatched(t, mustCmd(t, cmd, "processCmd")).(attachDoneMsg)
	if !ok || msg.id != m.images.list[0].id || msg.shownGen != m.shownGen || msg.err != nil || msg.path == "" {
		t.Fatalf("the processing answered %+v", msg)
	}
}

// TestChipsNumberOnWithinADraft (A1): a second paste is #2; a paste of two
// paths is two chips; a number a chip once had is not reused in the draft.
func TestChipsNumberOnWithinADraft(t *testing.T) {
	m, _ := imageModel(t)
	dir := t.TempDir()
	a := writePNG(t, dir, "a.png", 16, 16)
	b := writePNG(t, dir, "b b.png", 16, 16)
	c := writePNG(t, dir, "c.png", 16, 16)
	m = pasteText(t, m, a)
	m = typeComposer(t, m, " ")
	m = pasteText(t, m, "'"+b+"'\n"+c)
	if got := m.input.Value(); got != "[Image #1] [Image #2] [Image #3]" {
		t.Fatalf("the draft: %q", got)
	}
	m = pressKey(t, m, tea.KeyBackspace) // #3, whole
	m = pasteText(t, m, c)
	if got := m.input.Value(); got != "[Image #1] [Image #2] [Image #4]" {
		t.Fatalf("after deleting #3 and pasting again: %q", got)
	}
	var ns []int
	for _, e := range m.images.list {
		ns = append(ns, e.n)
	}
	if !reflect.DeepEqual(ns, []int{1, 2, 4}) {
		t.Fatalf("the sidecar holds %v", ns)
	}
}

// TestTypedAndMixedPathsStayText (A1): typed text is never promoted; a paste
// is promoted only when every word of it is an image path passing the
// pre-check. Each case starts from a model of its own: bubbles' textarea
// shares its lines between copies of a model, so no two may be edited from
// one.
func TestTypedAndMixedPathsStayText(t *testing.T) {
	dir := t.TempDir()
	shot := writePNG(t, dir, "shot.png", 16, 16)
	tiny := writePNG(t, dir, "tiny.png", 4, 4)
	m, _ := imageModel(t)
	m = typeComposer(t, m, shot)
	if m.input.Value() != shot || len(m.images.list) != 0 {
		t.Fatalf("a typed path became %q (%d images)", m.input.Value(), len(m.images.list))
	}
	for _, paste := range []string{
		"look at " + shot,
		shot + " " + filepath.Join(dir, "missing.png"),
		tiny,
		filepath.Join(dir, "notes.txt"),
		"'" + shot,
	} {
		m, _ := imageModel(t)
		m = pasteText(t, m, paste)
		if m.input.Value() != paste || len(m.images.list) != 0 || m.copyNote != "" {
			t.Errorf("the paste %q became %q (%d images, note %q)", paste, m.input.Value(), len(m.images.list), m.copyNote)
		}
	}
}

// seeChip is a model whose draft is "see [Image #1]", the chip processed,
// the cursor at the end.
func seeChip(t *testing.T) (Model, *Stub) {
	t.Helper()
	m, stub := imageModel(t)
	m = typeComposer(t, m, "see ")
	m = pasteText(t, m, shotPNG(t))
	return settleImages(t, m), stub
}

// TestBackspaceAndDeleteTakeAWholeChip (A2): Backspace at a chip's end, or
// Delete at its start, removes the whole chip and its image; a send after it
// carries no image. A label typed by hand is text, a character at a time.
func TestBackspaceAndDeleteTakeAWholeChip(t *testing.T) {
	m, stub := seeChip(t)
	m = pressKey(t, m, tea.KeyBackspace)
	if m.input.Value() != "see " || len(m.images.list) != 0 {
		t.Fatalf("Backspace at the chip's end: %q, %d images", m.input.Value(), len(m.images.list))
	}
	m = typeComposer(t, m, "[Image #7]")
	m = pressKey(t, m, tea.KeyBackspace)
	if m.input.Value() != "see [Image #7" {
		t.Fatalf("Backspace after a label typed by hand: %q", m.input.Value())
	}
	m = pressKey(t, m, tea.KeyEnter)
	if sent := stub.Prompts(); len(sent) != 1 || sent[0] != "see [Image #7" {
		t.Fatalf("the send after the Backspace: %q", sent)
	}

	// Delete with the cursor at the chip's start.
	m, _ = seeChip(t)
	for range len("[Image #1]") {
		m = pressKey(t, m, tea.KeyLeft)
	}
	m = pressKey(t, m, tea.KeyDelete)
	if m.input.Value() != "see " || len(m.images.list) != 0 {
		t.Fatalf("Delete at the chip's start: %q, %d images", m.input.Value(), len(m.images.list))
	}

	// Backspace inside a chip deletes a character, and the chip it broke
	// takes its image with it (reconcile).
	m, _ = seeChip(t)
	m = pressKey(t, m, tea.KeyLeft)
	m = pressKey(t, m, tea.KeyBackspace)
	if m.input.Value() != "see [Image #]" || len(m.images.list) != 0 {
		t.Fatalf("Backspace inside a chip: %q, %d images", m.input.Value(), len(m.images.list))
	}
}

// TestAnEditorRewriteDropsAChipsImage: a draft rewritten outside the
// composer's keys — an external editor — keeps only the images whose chips
// it still holds (reconcile, after every message).
func TestAnEditorRewriteDropsAChipsImage(t *testing.T) {
	m, _ := imageModel(t)
	dir := t.TempDir()
	m = pasteText(t, m, writePNG(t, dir, "a.png", 16, 16)+" "+writePNG(t, dir, "b.png", 16, 16))
	m = settleImages(t, m)
	m.input.SetValue("only [Image #2] now")
	m = deliver(t, m, tickMsg{})
	if len(m.images.list) != 1 || m.images.list[0].n != 2 {
		t.Fatalf("the sidecar after the rewrite: %+v", m.images.list)
	}
}

// TestTheEnvelopeLeadsTheShellContext (§3.1): with a command's output pending,
// the envelope comes first, then the shell context block, then the text.
func TestTheEnvelopeLeadsTheShellContext(t *testing.T) {
	m, stub := imageModel(t)
	m = plantShellResult(m, "cat plan.md", "the plan\n")
	m = pasteText(t, m, shotPNG(t))
	m = settleImages(t, m)
	path := m.images.list[0].path
	m = typeComposer(t, m, " and this?")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	want := agent.AttachmentBlock([]agent.AttachmentRef{{N: 1, Path: path, MIME: attach.MIMEPNG}}) +
		shellBlock("cat plan.md", "the plan\n") + "[Image #1] and this?"
	if sent := stub.Prompts(); len(sent) != 1 || sent[0] != want {
		t.Fatalf("sent %q, want %q", sent, want)
	}
	if m.input.Value() != "" || len(m.images.list) != 0 {
		t.Fatalf("the accepted send left %q, %+v", m.input.Value(), m.images.list)
	}
}

// TestSendWaitsForAChipBeingProcessed (§3.3): Enter while a chip is still
// processing sends nothing — the draft stays, the status row names the chip —
// and goes once it is done.
func TestSendWaitsForAChipBeingProcessed(t *testing.T) {
	m, stub := imageModel(t)
	m = pasteText(t, m, shotPNG(t))
	tm, _ := m.Update(enter())
	held := tm.(Model)
	if len(stub.Prompts()) != 0 || held.input.Value() != "[Image #1]" {
		t.Fatalf("a send while the chip processed: prompts %q, draft %q", stub.Prompts(), held.input.Value())
	}
	if !strings.Contains(plainView(held), "image #1 is still being attached") {
		t.Fatalf("the status row does not say why:\n%s", plainView(held))
	}
	m = settleImages(t, held)
	_ = pressKey(t, m, tea.KeyEnter)
	if sent := stub.Prompts(); len(sent) != 1 || len(sentImages(t, sent[0])) != 1 {
		t.Fatalf("the send once processed: %q", sent)
	}
}

// TestARefusedSendKeepsItsImages: a send the engine refuses — a queued row
// too long, the envelope counted in — keeps the draft and its sidecar (only an
// accepted send clears them); the shortened draft's send carries the image.
func TestARefusedSendKeepsItsImages(t *testing.T) {
	m, _ := queueWorking(t)
	m = pasteText(t, m, shotPNG(t))
	m = settleImages(t, m)
	long := "[Image #1] " + strings.Repeat("x", queueRowTextCap)
	m.input.SetValue(long)
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if got := queueTexts(m); len(got) != 0 {
		t.Fatalf("the row should have been refused for length: %d rows", len(got))
	}
	if m.input.Value() != long || len(m.images.list) != 1 || m.images.last != 1 {
		t.Fatalf("the refused send left %d bytes of draft, sidecar %+v", len(m.input.Value()), m.images)
	}
	m = typeEnter(t, m, "[Image #1] shorter")
	rows := queuedRows(m)
	if len(rows) != 1 || len(sentImages(t, rows[0].Text)) != 1 {
		t.Fatalf("the shortened send queued %q", queueTexts(m))
	}
	if m.input.Value() != "" || len(m.images.list) != 0 {
		t.Fatalf("the accepted send left %q, %+v", m.input.Value(), m.images.list)
	}
}

// pendingChip is a model whose draft is one chip still being processed, and
// the processing's answer, run but not delivered.
func pendingChip(t *testing.T) (Model, attachDoneMsg, string) {
	t.Helper()
	m, _ := imageModel(t)
	shot := shotPNG(t)
	m = pasteText(t, m, shot)
	return m, m.processCmd(m.images.list[0])().(attachDoneMsg), shot
}

// TestAStaleProcessResultIsDropped (§3.3): a result under a shown generation
// the model has left is dropped at the gate, the chip still pending; one for
// a chip deleted since is dropped, its file left for the sweep; one that
// failed puts the pasted text back, with the reason.
func TestAStaleProcessResultIsDropped(t *testing.T) {
	m, msg, _ := pendingChip(t)
	stale := msg
	stale.shownGen = m.shownGen + 1
	if m = deliver(t, m, stale); !m.images.list[0].pending {
		t.Fatal("a result of another shown generation was applied")
	}
	if m = deliver(t, m, msg); m.images.list[0].pending {
		t.Fatal("the chip's own result was not applied")
	}

	m, msg, _ = pendingChip(t)
	m = pressKey(t, m, tea.KeyBackspace)
	m = deliver(t, m, msg)
	if m.input.Value() != "" || len(m.images.list) != 0 || m.copyNote != "" {
		t.Fatalf("a result for a deleted chip: draft %q, sidecar %+v, note %q", m.input.Value(), m.images.list, m.copyNote)
	}
	if _, err := os.Stat(msg.path); err != nil {
		t.Fatalf("the dropped result's file is the sweep's to remove: %v", err)
	}

	m, msg, shot := pendingChip(t)
	msg.err, msg.path = attach.ErrTooSmall, ""
	m = typeComposer(t, m, " next")
	m = deliver(t, m, msg)
	if m.input.Value() != shot+" next" || len(m.images.list) != 0 {
		t.Fatalf("a failed chip: draft %q, sidecar %+v", m.input.Value(), m.images.list)
	}
	if !strings.Contains(m.copyNote, "image #1 not attached: smaller than 8×8 pixels") {
		t.Fatalf("the failure's note: %q", m.copyNote)
	}
}

// TestAPasteThatFailsToProcessIsItsPathAgain: the file gone by the time it is
// processed — the chip is the pasted text again, quotes and all.
func TestAPasteThatFailsToProcessIsItsPathAgain(t *testing.T) {
	m, _ := imageModel(t)
	shot := writePNG(t, t.TempDir(), "my shot.png", 16, 16)
	quoted := "'" + shot + "'"
	m = pasteText(t, m, quoted)
	if m.input.Value() != "[Image #1]" {
		t.Fatalf("the paste: %q", m.input.Value())
	}
	if err := os.Remove(shot); err != nil {
		t.Fatal(err)
	}
	m = settleImages(t, m)
	if m.input.Value() != quoted || len(m.images.list) != 0 || !strings.Contains(m.copyNote, "image #1 not attached: the file is gone") {
		t.Fatalf("after the failure: %q, %+v, note %q", m.input.Value(), m.images.list, m.copyNote)
	}
}

// TestOverTheCapAPasteStaysText (A2b, P29): the eleventh image, and images
// counted past 15 MiB, stay text, with a note.
func TestOverTheCapAPasteStaysText(t *testing.T) {
	m, _ := imageModel(t)
	dir := t.TempDir()
	var paths []string
	for i := range attach.MaxPerMessage {
		paths = append(paths, writePNG(t, dir, fmt.Sprintf("s%d.png", i), 16, 16))
	}
	m = pasteText(t, m, strings.Join(paths, " "))
	if len(m.images.list) != attach.MaxPerMessage {
		t.Fatalf("ten images: %d chips", len(m.images.list))
	}
	before := m.input.Value()
	eleventh := writePNG(t, dir, "s10.png", 16, 16)
	m = pasteText(t, m, eleventh)
	if m.input.Value() != before+eleventh || len(m.images.list) != attach.MaxPerMessage {
		t.Fatalf("the eleventh: %q, %d chips", m.input.Value(), len(m.images.list))
	}
	if !strings.Contains(m.copyNote, "at most 10 images") {
		t.Fatalf("the note: %q", m.copyNote)
	}

	// The bytes: four images that may each be 3.75 MiB processed are the
	// whole 15 MiB; a fifth is over.
	big, _ := imageModel(t)
	var full, one attachment
	full.size, one.size = attach.MaxBytes, 1
	if _, _, note := big.addImages([]attachment{full, full, full, full}); note != "" {
		t.Fatalf("15 MiB exactly was refused: %q", note)
	}
	if _, _, note := big.addImages([]attachment{one}); !strings.Contains(note, "15 MiB") {
		t.Fatalf("a fifth image past 15 MiB: %q", note)
	}
}

// TestADownscaledImageSaysSo (A2b, X13): an image over 2000 px on its long
// edge is downscaled, the status row says from what to what, and the
// envelope carries the original size for the host's note.
func TestADownscaledImageSaysSo(t *testing.T) {
	m, stub := imageModel(t)
	m = pasteText(t, m, writePNG(t, t.TempDir(), "wide.png", 2400, 16))
	m = settleImages(t, m)
	if !strings.Contains(m.copyNote, "image #1 downscaled 2400×16 → 2000×13") {
		t.Fatalf("the note: %q", m.copyNote)
	}
	_ = pressKey(t, m, tea.KeyEnter)
	refs := sentImages(t, stub.Prompts()[0])
	if len(refs) != 1 || refs[0].OW != 2400 || refs[0].OH != 16 {
		t.Fatalf("the envelope: %+v", refs)
	}
}

// TestANonVisionModelSaysItGetsAPlaceholder (A2b): a native session whose
// model the catalog does not mark as taking images says so once the chip is
// processed — the check is the processing's (it reads the catalog off the
// Update) — and a model the catalog does not know, or a session that is not
// native, is not checked at all.
func TestANonVisionModelSaysItGetsAPlaceholder(t *testing.T) {
	native := t.TempDir()
	alias, name := shippedModel(t, native)
	vc := visionCheck{dir: native, alias: alias, name: name}
	if got := vc.noVision(); got != name {
		t.Fatalf("a model with no vision: %q, want %q", got, name)
	}
	if got := (visionCheck{dir: native, alias: "no/such-model", name: "x"}).noVision(); got != "" {
		t.Fatalf("an unknown model: %q", got)
	}
	if got := (visionCheck{}).noVision(); got != "" {
		t.Fatalf("no check: %q", got)
	}

	// The session's check: native, its model by alias and by the name the
	// status row gives it.
	m, _ := imageModel(t)
	m.nativeDir = native
	m.snap.CurrentModel = alias
	m.snap.Models = []agent.ModelInfo{{ID: alias, Name: name}}
	if vc := m.visionCheck(); vc != (visionCheck{}) {
		t.Fatalf("a cursor session checked its model's sight: %+v", vc)
	}
	m.snap.Provider = agent.NativeProvider().Info()
	if got := m.visionCheck(); got != vc {
		t.Fatalf("a native session's check %+v, want %+v", got, vc)
	}

	// Through the processing — under the session's check — to the note.
	m = pasteText(t, m, shotPNG(t))
	msg := m.processCmd(m.images.list[0])().(attachDoneMsg)
	if msg.err != nil || msg.noVision != name {
		t.Fatalf("the processing answered %+v", msg)
	}
	m = m.attachDone(msg)
	if want := name + " can't see images; it will get a placeholder"; m.copyNote != want {
		t.Fatalf("the note %q, want %q", m.copyNote, want)
	}
}

// shippedModel is a model of the shipped catalog (read through an empty
// native directory) not marked as taking images: its alias and its name.
func shippedModel(t *testing.T, dir string) (alias, name string) {
	t.Helper()
	tbl, err := modeltable.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range tbl.Aliases() {
		if md := tbl.Models[a]; !md.Vision {
			if md.Name == "" {
				return a, a
			}
			return a, md.Name
		}
	}
	t.Fatal("the shipped catalog has no model without vision")
	return "", ""
}

// TestP27AHostThatCannotReadPastesAPath (A2b, P27): a session whose host
// cannot read this TUI's attachments directory makes no chip — the path stays
// the path, the status row says why — and a backend says so through the
// optional ReadsAttachments, read when it is adopted.
func TestP27AHostThatCannotReadPastesAPath(t *testing.T) {
	m, _ := imageModel(t)
	if !m.attachReads || !m.attachmentsReadable() {
		t.Fatal("the in-process engine's host cannot read its own attachments")
	}
	shot := shotPNG(t)
	m.attachReads = false
	m = pasteText(t, m, shot)
	if m.input.Value() != shot || len(m.images.list) != 0 || m.copyNote != p27Note {
		t.Fatalf("a refusing host: draft %q, %d chips, note %q", m.input.Value(), len(m.images.list), m.copyNote)
	}
	if p27Note != "this session's host can't read craze attachments; pasted as a path" {
		t.Fatalf("the note's words: %q", p27Note)
	}

	// The backend's word, as adopt reads it.
	if readsAttachments(&laneBackend{}) {
		t.Fatal("a backend that says nothing reads the attachments")
	}
	if !readsAttachments(&readingLane{laneBackend: &laneBackend{}}) {
		t.Fatal("a backend that says it reads them is not believed")
	}
	if !readsAttachments(&engineBackend{}) {
		t.Fatal("the in-process engine is refused")
	}
	if (Model{}).attachmentsReadable() {
		t.Fatal("no backend is a host that reads")
	}
	if !(Model{unstarted: &unstartedSession{}}).attachmentsReadable() {
		t.Fatal("an unstarted session's host — this TUI's own spawn — is refused")
	}
}

// readingLane is a lane whose host says it reads the TUI's attachments.
type readingLane struct{ *laneBackend }

func (readingLane) ReadsAttachments() bool { return true }

// ------------------------------------------------- wherever the draft goes
//
// Drafts stashed and put back, the unstarted first prompt, the queue editor,
// an interjection, and the pastes that are not the composer's (A1b, A3b, A6).

// TestDraftsCarryTheirImages (A1b): stashDraft, takeDraft and moveDraft carry
// the sidecar with the text — its numbering included — and an empty composer
// stashes none.
func TestDraftsCarryTheirImages(t *testing.T) {
	m, _ := imageModel(t)
	m = pasteText(t, m, shotPNG(t))
	m = settleImages(t, m)
	m = typeComposer(t, m, " look")
	want := m.images
	m.unstarted = &unstartedSession{draft: "tmp"}
	m.stashDraft()
	m.input.SetValue("")
	m.images = draftImages{}
	m.moveDraft("tmp", "real")
	m.unstarted = &unstartedSession{draft: "real"}
	m.takeDraft()
	if m.input.Value() != "[Image #1] look" || len(m.images.list) != 1 || !reflect.DeepEqual(m.images.list[0], want.list[0]) || m.images.last != 1 {
		t.Fatalf("the draft put back: %q, %+v", m.input.Value(), m.images)
	}
	if _, kept := m.drafts["real"]; kept {
		t.Fatal("the draft put back is still stashed")
	}
	// A draft put back with a chip still processing is started again, under
	// the shown generation it is shown in now.
	m.images.list[0].pending, m.images.list[0].src = true, attachSource{path: want.list[0].path}
	msg := runWatched(t, mustCmd(t, m.relaunchImages(), "processCmd")).(attachDoneMsg)
	if msg.id != want.list[0].id || msg.shownGen != m.shownGen || msg.err != nil {
		t.Fatalf("the relaunch answered %+v", msg)
	}
	if (Model{}).relaunchImages() != nil {
		t.Fatal("a draft with nothing pending relaunched something")
	}
}

// TestASwitchDropsAProcessResultAndStartsItAgainOnTheWayBack (§3.3, A1b): a
// chip still processing when its session is left keeps its place in the
// stashed draft; the answer that lands meanwhile is dropped at the gate (its
// shown generation is the last session's); back on the session, the draft's
// chip is processed again, and is done.
func TestASwitchDropsAProcessResultAndStartsItAgainOnTheWayBack(t *testing.T) {
	a, b, a2 := newLane(t, "a", "alpha"), newLane(t, "b", "bravo"), newLane(t, "a", "alpha")
	r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}, "a": {a2}})
	// The lanes' hosts read this TUI's attachments (P27).
	r.m.attachReads = true
	rows := []roster.Row{laneRow(a, "session a", time.Minute), laneRow(b, "session b", 2*time.Minute)}
	shot := shotPNG(t)
	r.send(pasteKey(shot))
	if r.m.input.Value() != "[Image #1]" || len(r.images) != 1 {
		t.Fatalf("the paste: %q, %d processings", r.m.input.Value(), len(r.images))
	}
	answer := r.pop(&r.images, "processing")
	r.stream(a, backend.Item{Kind: backend.ItemEnd})
	r.send(sessSnapMsg{gen: r.m.sessList.gen, snap: roster.Snapshot{Running: rows[1:]}})
	r.enterOn(b)
	r.send(r.pop(&r.opens, "dial"))
	if r.m.input.Value() != "" || len(r.m.images.list) != 0 {
		t.Fatalf("the draft followed the switch to B: %q, %+v", r.m.input.Value(), r.m.images.list)
	}
	r.dropped("a chip's processing answering after the switch", answer)
	r.up(b)
	r.stream(b, backend.Item{Kind: backend.ItemEnd})
	r.send(sessSnapMsg{gen: r.m.sessList.gen, snap: roster.Snapshot{Running: rows}})
	r.enterOn(a2)
	r.send(r.pop(&r.opens, "dial"))
	if r.m.input.Value() != "[Image #1]" || len(r.m.images.list) != 1 || !r.m.images.list[0].pending {
		t.Fatalf("back on A: %q, %+v", r.m.input.Value(), r.m.images.list)
	}
	r.send(r.pop(&r.images, "the processing started again"))
	if a := r.m.images.list[0]; a.pending || a.path == "" {
		t.Fatalf("the chip after its second processing: %+v", a)
	}
}

// TestTheUnstartedFirstPromptCarriesItsImages (A1b): an unstarted session's
// first prompt goes, once its host is up, with the envelope of the chips it was
// typed with — P27 passes for it, its host being this TUI's own spawn.
func TestTheUnstartedFirstPromptCarriesItsImages(t *testing.T) {
	m, _, hb, _ := openedUnstarted(t, 100, 30)
	m = pasteText(t, m, shotPNG(t))
	if m.input.Value() != "[Image #1]" {
		t.Fatalf("the paste into an unstarted session: %q", m.input.Value())
	}
	m = settleImages(t, m)
	path := m.images.list[0].path
	m = typeComposer(t, m, " fix this")
	m, cmd := press(m, enter())
	if len(m.unstarted.images) != 1 {
		t.Fatalf("the first prompt's images: %+v", m.unstarted.images)
	}
	tm, cmd := m.Update(runWatched(t, mustCmd(t, cmd, "enterUnstarted")))
	m = tm.(Model)
	if m.first == nil || len(m.first.images) != 1 || m.input.Value() != "[Image #1] fix this" || len(m.images.list) != 1 {
		t.Fatalf("after the adoption: first %+v, draft %q, sidecar %+v", m.first, m.input.Value(), m.images.list)
	}
	tm, _ = m.Update(runWatched(t, mustCmd(t, cmd, "startCmd")))
	m = tm.(Model)
	_, texts := hb.sent()
	want := agent.AttachmentBlock([]agent.AttachmentRef{{N: 1, Path: path, MIME: attach.MIMEPNG}}) + "[Image #1] fix this"
	if len(texts) != 1 || texts[0] != want {
		t.Fatalf("the first prompt went as %q, want %q", texts, want)
	}
	if m.input.Value() != "" || len(m.images.list) != 0 {
		t.Fatalf("the accepted first prompt left %q, %+v", m.input.Value(), m.images.list)
	}
}

// TestAnUnstartedFirstPromptWaitsForItsChip: enter on an unstarted session
// with a chip still processing spawns nothing.
func TestAnUnstartedFirstPromptWaitsForItsChip(t *testing.T) {
	m, fs, _, _ := openedUnstarted(t, 100, 30)
	m = pasteText(t, m, shotPNG(t))
	m, cmd := press(m, enter())
	if findCmd(cmd, "enterUnstarted") != nil || m.unstarted.pending != 0 || len(fs.spawns) != 0 {
		t.Fatal("enter spawned the session with a chip still processing")
	}
}

// TestAnInterjectionSendsPathText (A3b, P7): Ctrl+L on grok mid-turn sends the
// draft's chips as [Image #N: <path>] text — never an envelope — and the
// accepted interjection clears the draft and its sidecar.
func TestAnInterjectionSendsPathText(t *testing.T) {
	m, stub := queueWorkingLive(t)
	stub.SetProvider(agent.GrokProvider())
	tm, _ := m.Update(refreshSnapMsg{})
	m = tm.(Model)
	m = pasteText(t, m, shotPNG(t))
	m = settleImages(t, m)
	path := m.images.list[0].path
	m = typeComposer(t, m, " and this")
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = tm.(Model)
	got := stub.Interjections()
	if want := "[Image #1: " + path + "]\n[Image #1] and this"; len(got) != 1 || got[0] != want {
		t.Fatalf("interjected %q, want %q", got, want)
	}
	if m.input.Value() != "" || len(m.images.list) != 0 {
		t.Fatalf("the interjection left %q, %+v", m.input.Value(), m.images.list)
	}
}

// queuedChipRow queues "[Image #1] [Image #2] two shots" behind a hung turn and
// answers the model and the row.
func queuedChipRow(t *testing.T) (Model, agent.QueuedPrompt) {
	t.Helper()
	m, _ := queueWorking(t)
	dir := t.TempDir()
	m = pasteText(t, m, writePNG(t, dir, "a.png", 16, 16)+" "+writePNG(t, dir, "b.png", 24, 16))
	m = settleImages(t, m)
	m = typeComposer(t, m, " two shots")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	rows := queuedRows(m)
	if len(rows) != 1 || len(sentImages(t, rows[0].Text)) != 2 {
		t.Fatalf("the queued row: %q", queueTexts(m))
	}
	return m, rows[0]
}

// TestAQueueEditRebuildsTheRowsImages (A1b): a row's envelope comes back as
// chips in the editor — the envelope never on screen — and the save writes it
// afresh from what the edit kept: a chip deleted takes its image off the row,
// a chip kept keeps its own. The draft the edit displaced comes back with its
// own sidecar.
func TestAQueueEditRebuildsTheRowsImages(t *testing.T) {
	m, row := queuedChipRow(t)
	before := sentImages(t, row.Text)
	m = pasteText(t, m, writePNG(t, t.TempDir(), "draft.png", 16, 16))
	m = settleImages(t, m)
	draftImg := m.images

	m.startQueueEdit(row)
	if m.input.Value() != "[Image #1] [Image #2] two shots" || len(m.images.list) != 2 {
		t.Fatalf("the editor holds %q, %+v", m.input.Value(), m.images.list)
	}
	if strings.Contains(plainView(m), "craze_attachments") {
		t.Fatalf("the envelope is on screen:\n%s", plainView(m))
	}
	// Delete #2 whole: the cursor after it.
	m.input.SetValue("[Image #1] [Image #2]")
	m = pressKey(t, m, tea.KeyBackspace)
	m = typeComposer(t, m, "only the first")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	saved := queuedRows(m)
	if len(saved) != 1 {
		t.Fatalf("the band after the save: %q", queueTexts(m))
	}
	after := sentImages(t, saved[0].Text)
	if len(after) != 1 || after[0] != before[0] {
		t.Fatalf("the saved row's images %+v, want only %+v", after, before[0])
	}
	if _, rest := agent.SplitShellContext(saved[0].Text); rest != "[Image #1] only the first" {
		t.Fatalf("the saved row's text %q", rest)
	}
	if m.input.Value() != "[Image #1]" || len(m.images.list) != 1 || !reflect.DeepEqual(m.images.list[0], draftImg.list[0]) {
		t.Fatalf("the draft after the edit: %q, %+v", m.input.Value(), m.images.list)
	}
}

// TestAQueueEditKeepsAChipItKept: an edit of the text alone keeps every image.
func TestAQueueEditKeepsAChipItKept(t *testing.T) {
	m, row := queuedChipRow(t)
	before := sentImages(t, row.Text)
	m.startQueueEdit(row)
	m = typeEnter(t, m, "[Image #1] [Image #2] two shots, edited")
	saved := queuedRows(m)
	if len(saved) != 1 {
		t.Fatalf("the band after the save: %q", queueTexts(m))
	}
	if after := sentImages(t, saved[0].Text); len(after) != 2 || after[0] != before[0] || after[1] != before[1] {
		t.Fatalf("the saved row's images %+v, want %+v", after, before)
	}
}

// TestThePastesThatAreNotTheComposersStayText (A6): the session list's input,
// /connect's key field and a `!` command line take a pasted image path as
// text; none makes a chip, and Ctrl+V in the list never reads an image.
func TestThePastesThatAreNotTheComposersStayText(t *testing.T) {
	shot := shotPNG(t)
	reads := 0
	stubClipboardImage(t, func() ([]byte, error) { reads++; return nil, nil })

	// The session list's input.
	m, fs, _ := newSessModel(t, 100, 30)
	m = newList(t, m, fs)
	tm, _ := m.Update(pasteKey(shot))
	m = tm.(Model)
	if v, _ := inputOf(m); v != shot || len(m.images.list) != 0 || m.input.Value() != "" {
		t.Fatalf("the list's input %q, composer %q, %d images", v, m.input.Value(), len(m.images.list))
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	runWork(t, cmd)
	if reads != 0 {
		t.Fatalf("Ctrl+V in the list's input read the clipboard's image %d times", reads)
	}

	// /connect's key field.
	k := connectKeyStep(t, nativeStub(), t.TempDir(), func(string) string { return "" })
	tm, _ = k.Update(pasteKey(shot))
	k = tm.(Model)
	if got := k.cdlg.key.Value(); got != shot || len(k.images.list) != 0 || k.input.Value() != "" {
		t.Fatalf("the key field %q, composer %q, %d images", got, k.input.Value(), len(k.images.list))
	}

	// A `!` command line, and an empty paste into one.
	s, _ := imageModel(t)
	s = typeComposer(t, s, "!file ")
	s = pasteText(t, s, shot)
	if s.input.Value() != "!file "+shot || len(s.images.list) != 0 {
		t.Fatalf("shell mode: %q, %d images", s.input.Value(), len(s.images.list))
	}
	if cmd := s.updateComposer(pasteKey("")); cmd != nil {
		t.Fatal("an empty paste into a command line asked for the clipboard's image")
	}
}

// TestTheEmptyPasteProbe (§3.3): an empty bracketed paste asks the
// clipboard for its image — a chip when it holds one — except over SSH, where
// the clipboard craze could read is not the user's.
func TestTheEmptyPasteProbe(t *testing.T) {
	img := pngBytes(t, 16, 16)
	stubClipboardImage(t, func() ([]byte, error) { return img, nil })

	for _, env := range []map[string]string{{"SSH_CONNECTION": "10.0.0.1 1 10.0.0.2 22"}, {"SSH_TTY": "/dev/pts/3"}} {
		m, _ := imageModel(t)
		m.nativeEnv = func(k string) string { return env[k] }
		if cmd := m.updateComposer(pasteKey("")); cmd != nil {
			t.Fatalf("over SSH (%v) the empty paste asked for the clipboard's image", env)
		}
	}

	m, _ := imageModel(t)
	m.nativeEnv = func(string) string { return "" }
	msg, ok := runWatched(t, m.updateComposer(pasteKey(""))).(pasteMsg)
	if !ok || string(msg.image) != string(img) || msg.text != "" {
		t.Fatalf("the probe answered %#v", msg)
	}
	m = deliver(t, m, msg)
	m = settleImages(t, m)
	if m.input.Value() != "[Image #1]" || len(m.images.list) != 1 || m.images.list[0].pending {
		t.Fatalf("the probe's image: %q, %+v", m.input.Value(), m.images.list)
	}

	clipboardReadImage = noClipboardImage
	if msg := runWatched(t, probeClipboardImage(1)); msg != nil {
		t.Fatalf("a probe of a clipboard with no image answered %#v", msg)
	}
}

// TestCtrlVAndAltVPasteTheClipboardsImage (A6): with an image on the
// clipboard Ctrl+V makes a chip, and Alt+V is its alias; on a host that cannot
// read the attachments (P27) the clipboard's text goes instead, with a note; an
// image too large says why and pastes the text.
func TestCtrlVAndAltVPasteTheClipboardsImage(t *testing.T) {
	img := pngBytes(t, 16, 16)
	stubClipboardImage(t, func() ([]byte, error) { return img, nil })
	prevText := clipboardRead
	clipboardRead = func() (string, error) { return "the clipboard's text", nil }
	t.Cleanup(func() { clipboardRead = prevText })

	for _, key := range []tea.KeyMsg{{Type: tea.KeyCtrlV}, {Type: tea.KeyRunes, Runes: []rune{'v'}, Alt: true}} {
		m, _ := imageModel(t)
		tm, cmd := m.Update(key)
		m = tm.(Model)
		msg := runCmd(cmd).(pasteMsg)
		if string(msg.image) != string(img) || msg.text != "the clipboard's text" {
			t.Fatalf("%s read %#v", key, msg)
		}
		m = settleImages(t, deliver(t, m, msg))
		if m.input.Value() != "[Image #1]" || len(m.images.list) != 1 || m.images.list[0].pending {
			t.Fatalf("%s: %q, %+v", key, m.input.Value(), m.images.list)
		}
	}

	m, _ := imageModel(t)
	m.attachReads = false
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = deliver(t, tm.(Model), runCmd(cmd))
	if m.input.Value() != "the clipboard's text" || len(m.images.list) != 0 || m.copyNote != p27ClipNote {
		t.Fatalf("a refusing host: %q, %d images, note %q", m.input.Value(), len(m.images.list), m.copyNote)
	}

	clipboardReadImage = func() ([]byte, error) { return nil, attach.ErrSourceTooLarge }
	m, _ = imageModel(t)
	tm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = deliver(t, tm.(Model), runCmd(cmd))
	if m.input.Value() != "the clipboard's text" || !strings.Contains(m.copyNote, "the clipboard image was not pasted: larger than 20 MiB") {
		t.Fatalf("an image over the cap: %q, note %q", m.input.Value(), m.copyNote)
	}

	// A clipboard image the header check refuses never becomes a chip.
	msg := readPasteImage(func() ([]byte, error) { return pngBytes(t, 4, 4), nil }, pasteMsg{})
	if msg.image != nil || msg.imageErr == nil {
		t.Fatalf("a 4×4 clipboard image: %#v", msg)
	}
}

// TestTheFrameRunnerWaitsForAChip: a run does not end while a chip's
// processing runs or its answer is unapplied (frameState.settled), so the
// capture is the chip's final state and no store write lands after the run's
// HOME is gone — a chip deleted while it was processing included, whose entry
// is gone but whose processing is not.
func TestTheFrameRunnerWaitsForAChip(t *testing.T) {
	m, _ := imageModel(t)
	if m.imagesInFlight() {
		t.Fatal("a fresh model has a chip in flight")
	}
	shot := shotPNG(t)
	tm, cmd := m.Update(pasteKey(shot))
	m = tm.(Model)
	run := mustCmd(t, cmd, "processCmd")
	m = pressKey(t, m, tea.KeyBackspace)
	if len(m.images.list) != 0 || !m.imagesInFlight() {
		t.Fatalf("a deleted chip whose processing runs: %d entries, in flight %v", len(m.images.list), m.imagesInFlight())
	}
	m = deliver(t, m, runWatched(t, run))
	if m.imagesInFlight() || m.input.Value() != "" {
		t.Fatalf("after the deleted chip's processing answered: in flight %v, draft %q", m.imagesInFlight(), m.input.Value())
	}

	// A displaced draft's chip, answered while the queue edit holds the
	// composer: in flight until its answer is applied, wherever the draft is.
	tm, cmd = m.Update(pasteKey(shot))
	m = tm.(Model)
	run = mustCmd(t, cmd, "processCmd")
	m.startQueueEdit(agent.QueuedPrompt{ID: "r", Text: "row"})
	msg := runWatched(t, run)
	if !m.imagesInFlight() {
		t.Fatal("a displaced draft's answered but unapplied chip is not in flight")
	}
	m = deliver(t, m, msg)
	if m.imagesInFlight() {
		t.Fatal("a processed chip is still in flight")
	}
	// The row is no band's, so the edit may have ended and put the draft
	// back: the chip is wherever the draft is.
	d := m.editImages
	if m.queueEdit == "" {
		d = m.images
	}
	if len(d.list) != 1 || d.list[0].pending {
		t.Fatalf("the draft's sidecar: %+v", d.list)
	}
	if _, err := os.Stat(d.list[0].path); err != nil {
		t.Fatalf("the displaced draft's chip was not stored: %v", err)
	}
}
