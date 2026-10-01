package tui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/tool/attach"
	"github.com/charliek/craze/internal/paths"
)

// Images in the composer (plan 033 §3.3, owner decision 6).
//
// A screenshot pasted into the composer — a bracketed paste of its path, a
// terminal's drag-and-drop (which pastes the dropped path), Ctrl+V or Alt+V
// with an image on the clipboard — becomes a chip, [Image #N], in the draft:
// the text the user sees, and the text the transcript, the queue band and
// every replay show. Behind each chip is an entry of the draft's sidecar
// (draftImages): the processed copy craze wrote into its attachments
// directory (attach.Process, attach.Save; paths.AttachmentsDir). A send puts
// the sidecar in front of the text as the attachment envelope
// (agent.AttachmentBlock, §3.1), ahead of any shell context, and the host
// reads the files back from there.
//
//   - Only a paste is promoted, never typed text, and only a paste every word
//     of which is an absolute path to an image file that passes the
//     synchronous pre-check (precheckImage); anything else is the paste, as
//     text. Not in `!` shell mode, and only for a session whose host can read
//     this TUI's attachments directory (P27, attachmentsReadable).
//   - The processing runs off the Update (processAttachment), one command per
//     chip, carrying the chip's id and the shown generation: a switch drops
//     its result (attachDoneMsg is shownStamped), and the draft put back
//     later starts it again (relaunchImages). A result whose chip is gone is
//     dropped too, its file left for the sweep. Enter waits for every chip
//     (imagePending): the draft stays, and the status row says which.
//   - Backspace at a chip's end, or Delete at its start, removes the whole
//     chip and its entry (deleteChip); every other edit is reconciled after
//     the fact (reconcileImages), so a chip broken by hand, or one an
//     external editor dropped, takes its entry with it.
//   - The sidecar is the draft's: stashed and put back with it by a switch
//     (drafts), carried by an unstarted session's first prompt, displaced by
//     a queue edit and put back after it, and cleared — its numbering
//     restarting at 1 — only when a send is accepted (clearMatchingDraft) or
//     by /clear.

// attachment is one entry of the draft's sidecar: the image behind one chip.
type attachment struct {
	// id names the entry for the TUI's life (Model.attachSeq), whichever
	// draft it is in: what a processing result finds it by.
	id uint64
	// n is its chip's number: the chip is agent.ImageLabel(n).
	n int
	// storedImage is the processed copy, all "" and 0 while the entry is
	// pending — all but its size, which is then the most the copy could come
	// to (sourceEstimate): what the per-message cap is counted in (P29).
	storedImage
	// pending says the processing has not answered yet; src is what it
	// processes — the pasted file or the clipboard's bytes — kept while it is
	// pending, so that a draft a switch put away and brought back can start
	// it again.
	pending bool
	src     attachSource
	// orig is the text the chip stands in for, as it was pasted — quotes and
	// all — which a failed processing puts back in its place; "" for an
	// image off the clipboard, which had no text.
	orig string
}

// storedImage is an image's processed copy, as its processing stored it
// (processAttachment): path, absolute, in the attachments directory; mime its
// type; size its bytes; w and h its dimensions; ow and oh the source's when it
// was downscaled (0 otherwise), which the envelope carries for §3.4's note
// (X13).
type storedImage struct {
	path, mime   string
	size         int
	w, h, ow, oh int
}

// attachSource is what one chip's processing reads: a pasted file (path) or
// the clipboard's bytes (data).
type attachSource struct {
	path string
	data []byte
}

// draftImages is a draft's sidecar: its chips' entries in the order they were
// made, and the last chip number this draft has issued.
//
// It is copied with the Model on every Update, so it is never written in
// place: every change makes a new slice (the shell context's discipline,
// keepShellResult), and an older copy's entries stay what they were.
type draftImages struct {
	list []attachment
	last int
}

// stashedDraft is a session's stashed composer (Model.drafts): its text and
// its sidecar.
type stashedDraft struct {
	text   string
	images draftImages
}

// imageExts are the file extensions a pasted path must have to be looked at
// as an image (§3.3): the formats attach.Process decodes.
var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true}

// The status notes of §3.3.
const (
	// p27Note is a paste of image paths into a session whose host could not
	// read the attachments directory (P27): it stays the paths it was.
	p27Note = "this session's host can't read craze attachments; pasted as a path"
	// p27ClipNote is Ctrl+V's twin of it: an image on the clipboard has no
	// path to stay, so the clipboard's text is pasted instead, if it has any.
	p27ClipNote = "this session's host can't read craze attachments; the clipboard image was not pasted"
	// visionNote is a native session whose model is not marked as taking
	// images (modeltable.Model.Vision): the host will send a placeholder.
	visionNote = "%s can't see images; it will get a placeholder"
)

// ------------------------------------------------------------ who can read

// attachmentsReader is a backend that says whether its session's host can
// read this TUI's attachments directory (plan 033 P27): the in-process engine
// always can (engineBackend), and a socket's host can when it runs in the
// TUI's own CRAZE_HOME namespace — which internal/cli decides from the
// socket's path as it dials it (remote.SessionOptions.ReadsAttachments: the
// launch's dials, craze attach's), since nothing the TUI holds names it. A
// backend without the method is a host that
// cannot: the envelope would name files its host refuses to read, every image
// would arrive as path text, and the transcript would show chips the agent
// never saw — so the TUI makes none for it, and a pasted path stays a path.
type attachmentsReader interface{ ReadsAttachments() bool }

// readsAttachments is b's answer, false for a backend that gives none. It is
// read from the raw backend, as adopt is handed it, before a test's wrapper
// (sessionBackendHook) can hide the method.
func readsAttachments(b backend.Backend) bool {
	r, ok := b.(attachmentsReader)
	return ok && r.ReadsAttachments()
}

// ReadsAttachments is the in-process engine's answer: its host is this
// process.
func (b *engineBackend) ReadsAttachments() bool { return true }

// attachmentsReadable is P27 for the session shown: chips are made only for a
// session whose host reads this TUI's attachments directory — the backend's
// answer (attachReads, from adopt), or, for an unstarted session (plan 030
// §3.13), yes: its first prompt spawns its host from this TUI, under this
// TUI's own environment. No backend yet (a launch still spawning) is no.
func (m Model) attachmentsReadable() bool {
	if m.unstarted != nil {
		return true
	}
	return m.eng != nil && m.attachReads
}

// configAttachmentsDir is Config.AttachmentsDir as New resolves it: the craze
// directory's attachments/ when the Config names none (configNativeDir's
// rule).
func configAttachmentsDir(dir string) string {
	if dir == "" {
		return paths.AttachmentsDir()
	}
	return dir
}

// getenv is the TUI's environment (Config.Getenv, nativeEnv), "" for every
// name in a model New never built.
func (m Model) getenv(name string) string {
	if m.nativeEnv == nil {
		return ""
	}
	return m.nativeEnv(name)
}

// --------------------------------------------------------------- the paste

// pasteToken is one word of a paste split as a shell splits one: raw is the
// text as it was pasted — quotes and escapes included — and word the text it
// stands for.
type pasteToken struct {
	raw, word string
}

// splitPaste splits a paste into words the way a shell would read it, since
// that is how terminals and file managers write the paths they paste:
// whitespace (CR and LF among it) separates words; a backslash escapes the
// character after it (a backslash before a line break joins the lines); single
// quotes keep everything up to the next one; double quotes keep everything up
// to the next unescaped one, a backslash escaping only `"` and `\` inside
// them; quoted and bare parts run together into one word. ok is false for an
// unterminated quote — not a list of paths, so the paste is text.
//
// It is total and linear over any input, invalid UTF-8 included (whose bytes
// are copied as they are).
func splitPaste(s string) (toks []pasteToken, ok bool) {
	var word strings.Builder
	start, in := 0, false
	flush := func(end int) {
		if in {
			toks = append(toks, pasteToken{raw: s[start:end], word: word.String()})
			word.Reset()
			in = false
		}
	}
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r != utf8.RuneError && unicode.IsSpace(r) {
			flush(i)
			i += size
			continue
		}
		if !in {
			start, in = i, true
		}
		switch r {
		case '\\':
			i += size
			if i == len(s) {
				// A trailing backslash escapes nothing: it is itself.
				word.WriteByte('\\')
				continue
			}
			_, n := utf8.DecodeRuneInString(s[i:])
			if s[i] != '\n' {
				word.WriteString(s[i : i+n])
			}
			i += n
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			word.WriteString(s[i+1 : i+1+j])
			i += j + 2
		case '"':
			// Byte by byte: `"` and `\` are ASCII, which UTF-8 never uses
			// inside a multibyte rune, so the rest is copied as it is.
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' && j+1 < len(s) && (s[j+1] == '"' || s[j+1] == '\\') {
					j++
				}
				word.WriteByte(s[j])
			}
			if j == len(s) {
				return nil, false
			}
			i = j + 1
		default:
			word.WriteString(s[i : i+size])
			i += size
		}
	}
	flush(len(s))
	return toks, true
}

// pastePath is word as a path to an image file, if it is one (§3.3): a
// file:// URI decoded (a local one: no host but localhost, no query or
// fragment), a leading ~/ expanded against home, and then absolute, with an
// image file's extension. Nothing is read from the disk here.
func pastePath(word, home string) (string, bool) {
	switch {
	case strings.HasPrefix(word, "file://"):
		u, err := url.Parse(word)
		if err != nil || (u.Host != "" && u.Host != "localhost") || u.RawQuery != "" || u.Fragment != "" {
			return "", false
		}
		word = u.Path
	case strings.HasPrefix(word, "~/"):
		if home == "" || !filepath.IsAbs(home) {
			return "", false
		}
		word = filepath.Join(home, word[2:])
	}
	if !filepath.IsAbs(word) || !imageExts[strings.ToLower(filepath.Ext(word))] {
		return "", false
	}
	return word, true
}

// precheckImage is a pasted path's synchronous pre-check (§3.3), run in the
// Update before a chip is made, so a path that will not process stays text
// rather than flashing a chip: a regular file (Lstat: not a symlink, a FIFO or
// a directory), at most attach.MaxSourceBytes, whose header decodes as an
// image of at least attach.MinEdge on each edge and at most
// attach.MaxSourcePixels (attach.Probe — the header alone, nothing decoded).
// It answers the file's size.
func precheckImage(path string) (int64, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, attach.ErrNotRegular
	}
	if fi.Size() > attach.MaxSourceBytes {
		return 0, attach.ErrSourceTooLarge
	}
	f, err := openSource(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := attach.Probe(bufio.NewReader(io.LimitReader(f, attach.MaxSourceBytes))); err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// openSource opens a pasted file for reading without blocking: O_NONBLOCK, so
// a FIFO put where the checked file was cannot hang the caller (the Update,
// for the pre-check), and then a regular file, by its own descriptor.
func openSource(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, attach.ErrNotRegular
	}
	return f, nil
}

// sourceEstimate is what an image of srcSize bytes counts against the
// message's cap before it has been processed: the processed copy is never
// larger than attach.MaxBytes, and a pass-through keeps the source's size.
// A re-encode can come out larger than its source; the result is counted
// again, exactly, when it lands (attachDone).
func sourceEstimate(srcSize int64) int {
	return int(min(srcSize, attach.MaxBytes))
}

// pasteImages is a bracketed paste of text into the composer (updateComposer):
// when every word of it is a path to an image that passes the pre-check, and
// chips may be made here, it answers the paste as the chips it becomes — the
// textarea inserts that text in the paste's place, as a paste — and the
// commands that process them; otherwise the paste as it was, and nil.
//
// A paste of image paths that P27 or the per-message cap refuses stays text,
// and the status row says why. One that is not image paths says nothing: it
// is what the user pasted.
func (m *Model) pasteImages(text string) (string, tea.Cmd) {
	if m.shellMode() {
		// A command line: a path pasted into it is an argument.
		return text, nil
	}
	toks, ok := splitPaste(text)
	if !ok || len(toks) == 0 {
		return text, nil
	}
	// Every word a path first, which reads nothing, and only then each one's
	// pre-check: a paste with a word of prose in it is text without a file of
	// it opened on the Update.
	home := m.getenv("HOME")
	add := make([]attachment, len(toks))
	for i, tok := range toks {
		path, ok := pastePath(tok.word, home)
		if !ok {
			return text, nil
		}
		add[i] = attachment{src: attachSource{path: path}, orig: tok.raw}
	}
	for i := range add {
		size, err := precheckImage(add[i].src.path)
		if err != nil {
			return text, nil
		}
		add[i].size = sourceEstimate(size)
	}
	if !m.attachmentsReadable() {
		m.note(p27Note)
		return text, nil
	}
	chips, cmd, note := m.addImages(add)
	if note != "" {
		m.note(note)
		return text, nil
	}
	return chips, cmd
}

// pasteClipboardImage is an image off the clipboard landing (pasteMsg's
// image, plan 033 §3.3): a chip in the composer, processed like a pasted
// file's. Where no chip may be made — a `!` command line, a session whose host
// cannot read the attachments directory (P27), a draft at its cap — or where
// the image could not be read, the clipboard's text is pasted instead, as
// Ctrl+V always has, with a status note for every reason but the first. A
// composer a layer covers takes nothing, as for a text paste.
func (m Model) pasteClipboardImage(msg pasteMsg) (tea.Model, tea.Cmd) {
	if m.composerCovered() {
		return m, nil
	}
	switch {
	case msg.imageErr != nil:
		m.note("the clipboard image was not pasted: " + attachErrReason(msg.imageErr))
	case m.shellMode():
	case !m.attachmentsReadable():
		m.note(p27ClipNote)
	default:
		add := attachment{src: attachSource{data: msg.image}}
		add.size = sourceEstimate(int64(len(msg.image)))
		chips, cmd, note := m.addImages([]attachment{add})
		if note == "" {
			// Inserted as one bracketed paste of the chip's text, the way a
			// file's chips are: updateComposer leaves text that is not a list
			// of image paths as it is.
			return m, tea.Batch(m.updateComposer(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(chips), Paste: true}), cmd)
		}
		m.note(note)
	}
	if msg.text == "" {
		return m, nil
	}
	return m, m.updateComposer(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(msg.text), Paste: true})
}

// overSSH reports that the TUI runs in an SSH session (SSH_CONNECTION or
// SSH_TTY set): the clipboard tools there read the remote machine's
// clipboard, not the one the user pasted from (§3.3).
func (m Model) overSSH() bool {
	return m.getenv("SSH_CONNECTION") != "" || m.getenv("SSH_TTY") != ""
}

// addImages makes chips of add — each entry's source, the text it stands in
// for, and its size as the cap counts it (sourceEstimate) — in the draft's
// sidecar, and answers the chips' text, one after another with a space
// between, and the commands that process them. A draft the cap refuses (P29:
// more than attach.MaxPerMessage images, more than attach.MaxMessageBytes
// counted, or no chip number left), or a TUI with no attachments directory to
// store them in, gets nothing, and note says why. Nothing is made — no
// number, no id, no command — unless every chip is.
func (m *Model) addImages(add []attachment) (string, tea.Cmd, string) {
	cur := m.images
	if m.attachDir == "" {
		return "", nil, "craze has no attachments directory; pasted as text"
	}
	if len(cur.list)+len(add) > attach.MaxPerMessage {
		return "", nil, fmt.Sprintf("a message holds at most %d images; pasted as text", attach.MaxPerMessage)
	}
	if imageBytes(cur.list)+imageBytes(add) > attach.MaxMessageBytes {
		return "", nil, fmt.Sprintf("a message's images may add up to %d MiB at most; pasted as text", attach.MaxMessageBytes>>20)
	}
	// The numbers first, every one of them: a paste is all chips or none.
	value := m.input.Value()
	list, last := slices.Clone(cur.list), cur.last
	for _, a := range add {
		n := nextChip(last, list, value)
		if n == 0 {
			return "", nil, "no image number is free in this message; pasted as text"
		}
		a.n, a.pending, last = n, true, n
		list = append(list, a)
	}
	labels := make([]string, len(add))
	cmds := make([]tea.Cmd, len(add))
	for i := range add {
		a := &list[len(cur.list)+i]
		m.attachSeq++
		a.id = m.attachSeq
		labels[i] = agent.ImageLabel(a.n)
		cmds[i] = m.processCmd(*a)
	}
	m.images = draftImages{list: list, last: last}
	return strings.Join(labels, " "), tea.Batch(cmds...), ""
}

// imageBytes is what list counts against the per-message cap (P29): every
// entry's size, a pending one's at the most it could come to.
func imageBytes(list []attachment) int {
	total := 0
	for _, a := range list {
		total += a.size
	}
	return total
}

// nextChip is the number the draft's next chip gets: one past the last it
// issued (numbering never goes back within a draft, owner decision 6), skipping
// any number the sidecar still holds or whose label is already in the draft's
// text, so no label is ever ambiguous; past the envelope's highest number (99,
// §3.1) the lowest free one. 0 is none free.
func nextChip(last int, list []attachment, value string) int {
	free := func(n int) bool {
		for _, a := range list {
			if a.n == n {
				return false
			}
		}
		return !strings.Contains(value, agent.ImageLabel(n))
	}
	for n := last + 1; n <= agent.MaxAttachmentN; n++ {
		if free(n) {
			return n
		}
	}
	for n := 1; n <= last && n <= agent.MaxAttachmentN; n++ {
		if free(n) {
			return n
		}
	}
	return 0
}

// ------------------------------------------------------------- processing

// attachDoneMsg is one chip's processing answering (processAttachment): the
// processed copy stored, or why not. id is the chip's entry, which the draft
// it is in may have dropped since; shownGen is the shown generation it was
// started under — a switch drops it at the command gate (shownStamped), and
// the draft it was for starts it again when it comes back (relaunchImages).
type attachDoneMsg struct {
	id       uint64
	shownGen uint64
	storedImage
	err error
	// noVision is the session's model, by name, when it is a native model the
	// catalog does not mark as taking images (§3.3's note); "" otherwise.
	noVision string
}

func (m attachDoneMsg) shownUnder() uint64 { return m.shownGen }

// processCmd is the command that processes a's source for the session shown
// now: into the attachments directory, under the shown generation, counted
// in attachRuns from now until it has answered.
func (m Model) processCmd(a attachment) tea.Cmd {
	run := processAttachment(m.attachDir, a.src, a.id, m.shownGen, m.visionCheck())
	runs := m.attachRuns
	if runs == nil {
		return run
	}
	runs.Add(1)
	return func() tea.Msg {
		defer runs.Add(-1)
		return run()
	}
}

// visionCheck is what the processing needs to say §3.3's non-vision note: the
// native directory and the session's model — its alias and the name the
// status row gives it — on a native session; the zero value (no check) on any
// other provider, whose model's sight craze does not know.
type visionCheck struct {
	dir, alias, name string
}

func (m Model) visionCheck() visionCheck {
	if !m.connectOffered() || m.nativeDir == "" {
		return visionCheck{}
	}
	alias, name := m.currentModel()
	return visionCheck{dir: m.nativeDir, alias: alias, name: name}
}

// noVision answers the note's model name when the native catalog in c.dir has
// c.alias and does not mark it as taking images; "" when it does, when the
// model is not in the catalog, and when there is nothing to check. It reads
// the disk, so it runs inside the processing command.
func (c visionCheck) noVision() string {
	if c.dir == "" || c.alias == "" {
		return ""
	}
	t, err := modeltable.Load(c.dir)
	if err != nil {
		return ""
	}
	md, ok := t.Models[c.alias]
	if !ok || md.Vision {
		return ""
	}
	return c.name
}

// processAttachment is one chip's processing, off the Update (§3.3): the
// pasted file read (at most attach.MaxSourceBytes, through openSource) or the
// clipboard's bytes, attach.Process — the one processing path, P30 — and the
// copy stored in dir (attach.Save; never "": addImages makes no chip without
// one). Its answer is attachDoneMsg.
func processAttachment(dir string, src attachSource, id, shown uint64, vc visionCheck) tea.Cmd {
	return func() tea.Msg {
		msg := attachDoneMsg{id: id, shownGen: shown}
		data := src.data
		if data == nil {
			b, err := readSource(src.path)
			if err != nil {
				msg.err = err
				return msg
			}
			data = b
		}
		img, err := attach.Process(data)
		if err != nil {
			msg.err = err
			return msg
		}
		path, err := attach.Save(dir, img.Data, img.MIME)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.storedImage = storedImage{path: path, mime: img.MIME, size: len(img.Data), w: img.Width, h: img.Height}
		if img.Downscaled() {
			msg.ow, msg.oh = img.OrigWidth, img.OrigHeight
		}
		msg.noVision = vc.noVision()
		return msg
	}
}

// readSource reads an image file — a pasted one for processing, or the
// temporary file osascript wrote the clipboard's image to — opened without
// following a FIFO into a hang (openSource), and read through the cap
// (capRead, attach.MaxSourceBytes), so a file that grew past it since the
// pre-check is refused rather than read whole.
func readSource(path string) ([]byte, error) {
	f, err := openSource(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return capRead(f, attach.MaxSourceBytes)
}

// attachDone applies one chip's processing: the entry it was for — in the
// composer's sidecar, or in the draft a queue edit displaced (editImages) —
// is done, and the status row says what §3.3 asks of it (a downscale, a model
// that can't see it); or it failed, and the chip is replaced by the text it
// stood in for, with the reason. A result whose entry is gone — the chip
// deleted, the draft sent, a queue edit left — or already done is dropped,
// its file left for the sweep (§3.2).
func (m Model) attachDone(msg attachDoneMsg) Model {
	d, inEdit := &m.images, false
	i := d.index(msg.id)
	if i < 0 {
		d, inEdit = &m.editImages, true
		i = d.index(msg.id)
	}
	if i < 0 || !d.list[i].pending {
		return m
	}
	a := d.list[i]
	err := msg.err
	if err == nil && imageBytes(d.list)-a.size+msg.size > attach.MaxMessageBytes {
		err = fmt.Errorf("the message's images would be over %d MiB", attach.MaxMessageBytes>>20)
	}
	if err != nil {
		*d = d.without(i)
		if inEdit {
			m.editDraft = strings.Replace(m.editDraft, agent.ImageLabel(a.n), a.orig, 1)
		} else {
			m.replaceComposerChip(a.n, a.orig)
		}
		m.note(fmt.Sprintf("image #%d not attached: %s", a.n, attachErrReason(err)))
		return m
	}
	a.pending, a.src, a.storedImage = false, attachSource{}, msg.storedImage
	list := slices.Clone(d.list)
	list[i] = a
	*d = draftImages{list: list, last: d.last}
	var notes []string
	if a.ow != 0 {
		notes = append(notes, fmt.Sprintf("image #%d downscaled %d×%d → %d×%d", a.n, a.ow, a.oh, a.w, a.h))
	}
	if msg.noVision != "" {
		notes = append(notes, fmt.Sprintf(visionNote, msg.noVision))
	}
	if len(notes) > 0 {
		m.note(strings.Join(notes, " · "))
	}
	return m
}

// attachErrReason is a failed processing's reason, as the status row says it:
// the refusal's own words (attach.Reason), "the file is gone" for one that is,
// and otherwise the error's text on one clean line.
func attachErrReason(err error) string {
	if r := attach.Reason(err); r != "" {
		return r
	}
	if errors.Is(err, fs.ErrNotExist) {
		return "the file is gone"
	}
	return sanitizeLine(err.Error())
}

// relaunchImages starts again the processing of every chip of the draft just
// put back in the composer that has not answered (takeDraft's callers): the
// one it started answered under another shown generation, and the command
// gate dropped it.
func (m Model) relaunchImages() tea.Cmd {
	var cmds []tea.Cmd
	for _, a := range m.images.list {
		if a.pending {
			cmds = append(cmds, m.processCmd(a))
		}
	}
	return tea.Batch(cmds...)
}

// ------------------------------------------------------------- the sidecar

// index is the position of the entry id names, or -1.
func (d draftImages) index(id uint64) int {
	return slices.IndexFunc(d.list, func(a attachment) bool { return a.id == id })
}

// without is d less its i-th entry, in a new slice.
func (d draftImages) without(i int) draftImages {
	return draftImages{list: slices.Delete(slices.Clone(d.list), i, i+1), last: d.last}
}

// pending is the first entry still being processed, if any.
func (d draftImages) pending() (attachment, bool) {
	for _, a := range d.list {
		if a.pending {
			return a, true
		}
	}
	return attachment{}, false
}

// reconciled is d less every entry whose chip is not in value: a chip deleted
// a character at a time, or by an external editor, or a draft emptied, takes
// its image with it (§3.3). It is d itself when nothing goes.
func (d draftImages) reconciled(value string) draftImages {
	keep := func(a attachment) bool { return strings.Contains(value, agent.ImageLabel(a.n)) }
	if !slices.ContainsFunc(d.list, func(a attachment) bool { return !keep(a) }) {
		return d
	}
	var list []attachment
	for _, a := range d.list {
		if keep(a) {
			list = append(list, a)
		}
	}
	return draftImages{list: list, last: d.last}
}

// reconcileImages holds the composer's sidecar to its text, after every
// message (finish, beside syncComposerAt). A draft with no chips has nothing
// to hold, and its text is not read.
func (m *Model) reconcileImages() {
	if len(m.images.list) == 0 {
		return
	}
	m.images = m.images.reconciled(m.input.Value())
}

// imageRefs is the envelope's list for text, a message about to go: every
// done entry of list whose chip is in text, in chip order (P28: an image is
// only ever sent with its chip in the text). Pending entries are never
// among them — a send waits for them (imagePending).
func imageRefs(list []attachment, text string) []agent.AttachmentRef {
	var refs []agent.AttachmentRef
	for _, a := range list {
		if a.pending || a.path == "" || !strings.Contains(text, agent.ImageLabel(a.n)) {
			continue
		}
		refs = append(refs, agent.AttachmentRef{N: a.n, Path: a.path, MIME: a.mime, OW: a.ow, OH: a.oh})
	}
	return refs
}

// withImages is text on its way to the agent with the envelope for its chips
// in front of it (§3.1): ahead of the shell context, which the caller has
// already put in front of the message (withShellContext) — the envelope is
// the outermost block. No chips is text itself.
func withImages(list []attachment, text string) string {
	_, msg := agent.SplitShellContext(text)
	return agent.AttachmentBlock(imageRefs(list, msg)) + text
}

// imagesInFlight reports a chip's processing still running, or a chip of the
// composer's draft — or of the draft a queue edit displaced — whose answer
// has not been applied: what the frame runner waits out before its quit
// (frameState.settled). The count covers a chip deleted while its processing
// ran, whose store write would otherwise race the run's end.
func (m Model) imagesInFlight() bool {
	if m.attachRuns != nil && m.attachRuns.Load() > 0 {
		return true
	}
	_, a := m.images.pending()
	_, b := m.editImages.pending()
	return a || b
}

// imagePending is the gate on every send of the draft (§3.3): while a chip of
// it is still being processed nothing goes — the draft stays, and the status
// row names the chip — so a message is never sent without an image its text
// shows. It reports whether the send was held.
func (m *Model) imagePending() bool {
	a, ok := m.images.pending()
	if !ok {
		return false
	}
	m.note(fmt.Sprintf("image #%d is still being attached; press enter again in a moment", a.n))
	return true
}

// imagesFromRefs is a queued row's envelope as a sidecar (startQueueEdit): an
// entry per ref, each with an id of its own, its size from the stored file
// (attach.MaxBytes if it cannot be read: the most it could be), numbering on
// from the highest.
func (m *Model) imagesFromRefs(refs []agent.AttachmentRef) draftImages {
	var d draftImages
	for _, r := range refs {
		m.attachSeq++
		size := attach.MaxBytes
		if fi, err := os.Stat(r.Path); err == nil && fi.Mode().IsRegular() {
			size = int(min(fi.Size(), attach.MaxBytes))
		}
		d.list = append(d.list, attachment{id: m.attachSeq, n: r.N, storedImage: storedImage{path: r.Path, mime: r.MIME, size: size, ow: r.OW, oh: r.OH}})
		d.last = max(d.last, r.N)
	}
	return d
}

// --------------------------------------------------------- editing a chip

// deleteChip is Backspace with the cursor just after a chip, or Delete with it
// just before one (§3.3): the whole chip goes, and its entry with it. It
// reports whether msg was that; any other key is the textarea's. Only a chip
// with an entry is atomic — a label typed by hand is text like any other.
func (m *Model) deleteChip(msg tea.KeyMsg) bool {
	if msg.Paste || len(m.images.list) == 0 {
		return false
	}
	back := key.Matches(msg, m.input.KeyMap.DeleteCharacterBackward)
	if !back && !key.Matches(msg, m.input.KeyMap.DeleteCharacterForward) {
		return false
	}
	v, off := m.input.Value(), m.composerCursorOffset()
	for i, a := range m.images.list {
		label := agent.ImageLabel(a.n)
		at := off
		if back {
			at = off - len(label)
		}
		if at < 0 || at+len(label) > len(v) || v[at:at+len(label)] != label {
			continue
		}
		// The textarea's own key, once per character of the label (all
		// ASCII, all on one line): its cursor and its view move exactly as
		// they do for a character deleted by hand.
		for range len(label) {
			m.input, _ = m.input.Update(msg)
		}
		m.images = m.images.without(i)
		return true
	}
	return false
}

// replaceComposerChip puts orig where chip n is in the composer (a failed
// processing), the cursor kept where it was relative to the text around it.
func (m *Model) replaceComposerChip(n int, orig string) {
	v := m.input.Value()
	label := agent.ImageLabel(n)
	at := strings.Index(v, label)
	if at < 0 {
		return
	}
	off := m.composerCursorOffset()
	switch {
	case off >= at+len(label):
		off += len(orig) - len(label)
	case off > at:
		off = at + len(orig)
	}
	next := v[:at] + orig + v[at+len(label):]
	m.input.SetHeight(composerHeadroom)
	m.input.SetValue(next)
	m.setComposerCursor(next, off)
}

// currentModel is the session's current model: its id — the snapshot's, or
// the one the TUI was started with before the agent has said — and the name
// the agent advertises for it, on one clean line (the id when it advertises
// none), the way the status row's modelLabel names it. The non-vision note
// (visionCheck) uses it.
func (m Model) currentModel() (id, name string) {
	id = m.snap.CurrentModel
	if id == "" {
		id = m.model
	}
	name = id
	for _, md := range m.snap.Models {
		if md.ID == id && md.Name != "" {
			name = md.Name
			break
		}
	}
	return id, sanitizeLine(name)
}
