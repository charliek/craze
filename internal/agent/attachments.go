package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charliek/craze/internal/harness/tool/attach"
)

// The attachment envelope (plan 033 §3.1, P1).
//
// Images ride the prompt's text, as one leading line:
//
//	<craze_attachments>{"v":1,"images":[{"n":1,"path":"/abs/…/ab12cd34ef56ab78.png","mime":"image/png"}]}</craze_attachments>
//	look at [Image #1]
//
// The prompt is a string on every layer between the composer and the agent —
// the socket, the engine, the queue, the journal — and base64 could not ride
// it (a socket line is 4 MiB, a queue row 32 KiB), so the text names files
// instead: processed copies the TUI wrote into the attachments directory
// (internal/harness/tool/attach). The visible text keeps each image's chip,
// [Image #N], which is what the transcript, replay and compaction show.
//
// It follows the shell context's design (shellcontext.go) and sits in front
// of it when both are there. Unlike that block, the envelope is never wire
// content: the host takes it off (SplitAttachments), reads the files it names
// (ReadAttachments) and sends their bytes as images, and the display strips it
// with the shell context (SplitShellContext), so it is never on any screen.
//
// The envelope is forgeable — any socket client, or a draft the user typed,
// can write one (P2) — so nothing in it is trusted beyond being a list of
// requests: each limit is checked here, each file is opened confined to the
// host's own attachments directory and validated from its bytes, and an image
// is only sent when its chip is in the text the user sees (P28).

const (
	attachmentsTag   = "craze_attachments"
	attachmentsOpen  = "<" + attachmentsTag + ">"
	attachmentsClose = "</" + attachmentsTag + ">"

	// attachmentsVersion is the envelope's "v". Another version is not
	// guessed at: its images become path text.
	attachmentsVersion = 1
	// envelopeMax is the most bytes an envelope may take, tags and newline
	// included; MaxAttachmentN the highest chip number — the composer's
	// numbering stops there too (plan 033 §3.3); maxAttachmentPath the
	// longest path. Past any of them — or with more than
	// attach.MaxPerMessage images, a repeated or out-of-range number, a
	// relative path or a type the store never writes — the whole envelope
	// becomes path text.
	envelopeMax       = 4 << 10
	MaxAttachmentN    = 99
	maxAttachmentPath = 1024
	// pathTextMax caps a path as path text shows it, in runes.
	pathTextMax = 256
)

// AttachmentRef is one image an envelope names: its chip number, the absolute
// path of its processed copy, and that copy's media type.
//
// OW and OH are the source's dimensions when the TUI downscaled it (plan 033
// X13), so the host can tell an ACP agent "[Image #1 was downscaled from
// 3024×1964 to 2000×1299]" (§3.4) — the host cannot re-derive them, since it
// only ever sees the processed copy. Both are omitted (zero) when the image
// was not downscaled. They are a claim like the rest of the envelope, never
// trusted: Attachment.Original says when they may be used, and they are used
// for that note's words and nothing else.
type AttachmentRef struct {
	N    int    `json:"n"`
	Path string `json:"path"`
	MIME string `json:"mime"`
	OW   int    `json:"ow,omitempty"`
	OH   int    `json:"oh,omitempty"`
}

// envelope is the JSON between the tags.
type envelope struct {
	V      int             `json:"v"`
	Images []AttachmentRef `json:"images"`
}

// Attachment is an image the host read and validated, ready for the wire:
// its ref, the file's bytes, and the dimensions its own header gives.
type Attachment struct {
	AttachmentRef
	Data          []byte
	Width, Height int
}

// Original is the image's size before the TUI downscaled it, as its envelope
// claims (OW, OH; plan 033 X13), and whether the claim may be used: only when
// both are positive, within attach.MaxSourcePixels, and larger than the
// dimensions the host read from the file's own bytes (Width, Height) — on
// neither edge smaller, on at least one larger, which is what a downscale
// leaves. Anything else is no claim at all, and the caller writes no note.
// The numbers are untrusted: they only ever reach a note's words.
func (a Attachment) Original() (w, h int, ok bool) {
	w, h = a.OW, a.OH
	switch {
	case w <= 0 || h <= 0:
		return 0, 0, false
	// Each edge alone first, so the product below cannot overflow: the
	// numbers are whatever JSON the envelope carried.
	case w > attach.MaxSourcePixels || h > attach.MaxSourcePixels || int64(w)*int64(h) > attach.MaxSourcePixels:
		return 0, 0, false
	case w < a.Width || h < a.Height || (w == a.Width && h == a.Height):
		return 0, 0, false
	}
	return w, h, true
}

// ImageLabel is chip n's text, [Image #n]: what the composer writes, what the
// transcript shows, and what the label rule looks for (P28).
func ImageLabel(n int) string {
	return "[Image #" + strconv.Itoa(n) + "]"
}

// AttachmentBlock renders refs as the envelope that leads a message, ending
// in its newline; no refs is "", so a caller can always write block+text. The
// TUI puts it in front of the shell context block when both are there (plan
// 033 §3.1).
//
// The JSON is encoding/json's, which escapes '<' and '>' inside strings, so no
// path can hold a literal </craze_attachments> that would end the block early.
func AttachmentBlock(refs []AttachmentRef) string {
	if len(refs) == 0 {
		return ""
	}
	// Ints and strings: Marshal has nothing to fail on.
	b, _ := json.Marshal(envelope{V: attachmentsVersion, Images: refs})
	return attachmentsOpen + string(b) + attachmentsClose + "\n"
}

// leadingEnvelope finds a well-formed envelope leading text: the open tag
// first; the first close tag after it, followed by a newline or by the end of
// the text; and between them a single JSON object of the envelope's shape. It
// returns the block — tags, JSON and newline — and what it decodes to.
//
// Well-formed, not within limits: an envelope over its limits is still an
// envelope, which the display hides and the host turns into path text. What
// is not well-formed is not an envelope at all, and stays the user's text on
// both sides — the display and the host agree on that by both asking this.
//
// It is total and linear: one search for the close tag, one decode.
func leadingEnvelope(text string) (block string, env envelope, ok bool) {
	if !strings.HasPrefix(text, attachmentsOpen) {
		return "", envelope{}, false
	}
	body := text[len(attachmentsOpen):]
	i := strings.Index(body, attachmentsClose)
	if i < 0 {
		return "", envelope{}, false
	}
	end := len(attachmentsOpen) + i + len(attachmentsClose)
	if end < len(text) {
		if text[end] != '\n' {
			return "", envelope{}, false
		}
		end++
	}
	// An object: JSON's null would decode into the zero envelope without a
	// word, and a bare null is not a list of anything.
	if !strings.HasPrefix(strings.TrimLeft(body[:i], " \t\r\n"), "{") {
		return "", envelope{}, false
	}
	// One object and nothing after it but whitespace: Unmarshal refuses
	// anything else after the value.
	if err := json.Unmarshal([]byte(body[:i]), &env); err != nil {
		return "", envelope{}, false
	}
	return text[:end], env, true
}

// envelopeProblem is why a well-formed envelope is over its limits, or "".
// block is the whole envelope, for its size.
func envelopeProblem(block string, env envelope) string {
	switch {
	case len(block) > envelopeMax:
		return fmt.Sprintf("the attachment list is %d bytes, over its %d-byte limit", len(block), envelopeMax)
	case env.V != attachmentsVersion:
		return fmt.Sprintf("the attachment list is version %d, not %d", env.V, attachmentsVersion)
	case len(env.Images) > attach.MaxPerMessage:
		return fmt.Sprintf("the message names %d images, over the limit of %d", len(env.Images), attach.MaxPerMessage)
	}
	seen := make(map[int]bool, len(env.Images))
	for _, r := range env.Images {
		switch {
		case r.N < 1 || r.N > MaxAttachmentN:
			return fmt.Sprintf("image number %d is outside 1 to %d", r.N, MaxAttachmentN)
		case seen[r.N]:
			return fmt.Sprintf(namedTwice, r.N)
		case !filepath.IsAbs(r.Path):
			return fmt.Sprintf("image #%d's path is not absolute", r.N)
		case len(r.Path) > maxAttachmentPath:
			return fmt.Sprintf("image #%d's path is %d bytes, over the %d-byte limit", r.N, len(r.Path), maxAttachmentPath)
		case !attach.Stored(r.MIME):
			return fmt.Sprintf("image #%d's type %q is not one craze stores", r.N, sanitizeLine(r.MIME))
		}
		seen[r.N] = true
	}
	return ""
}

// namedTwice is the refusal of a repeated image number, which both the
// envelope's limits and ReadAttachments' own check make.
const namedTwice = "image number %d is named twice"

// SplitAttachments is the host's first step with a prompt's text (plan 033
// §3.1): the images a leading envelope asks for, the text without the
// envelope, and problems for the journal. It is total.
//
//   - No envelope leads the text, or one that is not well-formed: no refs,
//     and the text unchanged — a block that is not well-formed is the user's
//     text, sent as it was typed. A text that only starts like one is noted
//     in problems.
//   - A well-formed envelope within its limits: its refs, in its order (the
//     chip order the TUI wrote), and rest is everything after it. The refs
//     are non-nil even when the envelope names no image, so refs != nil says
//     exactly that an envelope was taken off.
//   - A well-formed envelope over its limits: no refs, and every image it
//     names becomes path text — [Image #N: <path> (not attached: <reason>)]
//     — in front of the user's text, after any shell context block (which
//     must keep leading, or the command output in it would be scanned for
//     /commands as though the user had typed it). problems says why.
//
// The refs are only requests: ReadAttachments decides what is sent.
func SplitAttachments(text string) (refs []AttachmentRef, rest string, problems []string) {
	block, env, ok := leadingEnvelope(text)
	if !ok {
		if strings.HasPrefix(text, attachmentsOpen) {
			problems = append(problems, "an attachment list that is not well-formed was sent as text")
		}
		return nil, text, problems
	}
	rest = text[len(block):]
	if p := envelopeProblem(block, env); p != "" {
		return nil, refsAsPathText(rest, env.Images, "the attachment list is over its limits"), []string{"attachments not sent: " + p}
	}
	if env.Images == nil {
		env.Images = []AttachmentRef{}
	}
	return env.Images, rest, nil
}

// ReadAttachments is the host's read of the images an envelope asked for (plan
// 033 §3.1, P2, P28, P29). dir is the host's own attachments directory
// (paths.AttachmentsDir()); visible is the text the user sees — the rest
// SplitAttachments returned; a leading shell context block in it is skipped,
// since a label in a command's output is not one the user put there.
//
// For each ref, in order:
//
//   - its label, [Image #N], must be in visible, or it is dropped: no image,
//     no path text, a problem for the journal. A forged envelope cannot send
//     an image the user never saw a chip for;
//   - the file is opened confined: dir as an os.Root (not a symlink, 0700,
//     the user's own); the path clean and directly inside dir, its base name
//     a stored name and a regular file (not a symlink, not a FIFO), read
//     through a limit (attach.OpenDir, attach.ReadPath);
//   - the bytes are validated, never the envelope's word: magic and header
//     agree with the type, at least 8×8, at most 2000 on the long edge
//     (attach.Validate);
//   - the images read so far, this one included, add up to at most
//     attach.MaxMessageBytes.
//
// It holds refs to the envelope's count and numbering limits itself too —
// at most attach.MaxPerMessage images, each number once — rather than trust
// that they came through SplitAttachments: refs past the tenth, and a number
// seen before, are refused like any other failure.
//
// An image that fails any step after the label is not sent; its path text,
// [Image #N: <path> (not attached: <reason>)], is in fallbacks (in ref order)
// for the caller to put in the text (PlacePathText), so the model still
// learns the user meant to show it something, and where it was. A file that
// is gone reads "no longer available". Every failure is also in problems,
// which the caller journals.
func ReadAttachments(dir string, refs []AttachmentRef, visible string) (atts []Attachment, fallbacks, problems []string) {
	if len(refs) == 0 {
		return nil, nil, nil
	}
	_, visible = splitShellBlock(visible)
	// The directory is opened once for the whole message; dirReason is why no
	// image can be read from it at all, or "".
	root, dirReason := openAttachments(dir)
	if root != nil {
		defer root.Close()
	}
	total := 0
	seen := make(map[int]bool, len(refs))
	for i, r := range refs {
		if !strings.Contains(visible, ImageLabel(r.N)) {
			problems = append(problems, fmt.Sprintf("image #%d (%s) dropped: %s is not in the message", r.N, pathTextPath(r.Path), ImageLabel(r.N)))
			continue
		}
		var att Attachment
		var reason string
		switch {
		case i >= attach.MaxPerMessage:
			reason = fmt.Sprintf("more than %d images in one message", attach.MaxPerMessage)
		case seen[r.N]:
			reason = fmt.Sprintf(namedTwice, r.N)
		case dirReason != "":
			reason = dirReason
		default:
			att, reason = readAttachment(root, r)
		}
		seen[r.N] = true
		if reason == "" && total+len(att.Data) > attach.MaxMessageBytes {
			reason = fmt.Sprintf("the message's images would be over %d MiB", attach.MaxMessageBytes>>20)
		}
		if reason != "" {
			fallbacks = append(fallbacks, attachmentPathText(r.N, r.Path, reason))
			problems = append(problems, fmt.Sprintf("image #%d (%s) not attached: %s", r.N, pathTextPath(r.Path), reason))
			continue
		}
		total += len(att.Data)
		atts = append(atts, att)
	}
	return atts, fallbacks, problems
}

// openAttachments opens the attachments directory dir for the host's read
// (attach.OpenDir): its Root, or nil and why no image can be read from it.
func openAttachments(dir string) (*os.Root, string) {
	if dir == "" {
		return nil, "craze has no attachments directory"
	}
	root, err := attach.OpenDir(dir)
	if err != nil {
		return nil, attachReason(err)
	}
	return root, ""
}

// readAttachment reads r's file through the confined read (attach.ReadPath)
// and validates its bytes against r's type (attach.Validate): the image, or
// the reason it is not one.
func readAttachment(root *os.Root, r AttachmentRef) (Attachment, string) {
	data, err := attach.ReadPath(root, r.Path)
	if err != nil {
		return Attachment{}, attachReason(err)
	}
	cfg, err := attach.Validate(data, r.MIME)
	if err != nil {
		return Attachment{}, attachReason(err)
	}
	return Attachment{AttachmentRef: r, Data: data, Width: cfg.Width, Height: cfg.Height}, ""
}

// attachReason is the "not attached: …" reason for an error from the confined
// read or the validation: "no longer available" for a file (or directory)
// that is gone, the refusal's own words (attach.Reason), and a plain fallback
// for anything else — never the error's text, which can carry a path the
// model needn't see.
func attachReason(err error) string {
	if errors.Is(err, fs.ErrNotExist) {
		return "no longer available"
	}
	if reason := attach.Reason(err); reason != "" {
		return reason
	}
	return "it could not be read"
}

// AttachmentsAsPathText is text with a leading envelope turned into path
// text, [Image #N: <path>], one line per image after any shell context block
// and before the user's words. It is how an interjection carries a chip (plan
// 033 P7): interject is text-only on both agents, and both apply this at
// their Interject, so a socket client's mode:"interject" is held to it too.
// Text with no envelope comes back unchanged; problems is SplitAttachments'.
func AttachmentsAsPathText(text string) (string, []string) {
	refs, rest, problems := SplitAttachments(text)
	return refsAsPathText(rest, refs, ""), problems
}

// PlacePathText puts path text lines into rest, the text after an envelope:
// after any leading shell context block, which has to keep leading, and
// before the user's words, one line each. No lines is rest unchanged.
func PlacePathText(rest string, lines []string) string {
	if len(lines) == 0 {
		return rest
	}
	shell, user := splitShellBlock(rest)
	return shell + strings.Join(lines, "\n") + "\n" + user
}

// refsAsPathText is rest with every ref put in as path text, each with reason
// ("" for none), where PlacePathText puts it. No refs is rest unchanged.
func refsAsPathText(rest string, refs []AttachmentRef, reason string) string {
	lines := make([]string, len(refs))
	for i, r := range refs {
		lines[i] = attachmentPathText(r.N, r.Path, reason)
	}
	return PlacePathText(rest, lines)
}

// attachmentPathText is one image as text: [Image #N: <path>], with
// " (not attached: <reason>)" inside the bracket when there is a reason.
func attachmentPathText(n int, path, reason string) string {
	if reason == "" {
		return fmt.Sprintf("[Image #%d: %s]", n, pathTextPath(path))
	}
	return fmt.Sprintf("[Image #%d: %s (not attached: %s)]", n, pathTextPath(path), reason)
}

// pathTextPath is a path as path text shows it: folded onto one clean line —
// the envelope is forgeable, and a path is otherwise free to hold newlines,
// escape sequences and bidi overrides — and capped at pathTextMax runes, the
// last of them an ellipsis when it was cut.
func pathTextPath(path string) string {
	p := sanitizeLine(path)
	n, cut := 0, 0
	for i := range p {
		if n == pathTextMax-1 {
			cut = i
		}
		if n == pathTextMax {
			return p[:cut] + ellipsis
		}
		n++
	}
	return p
}
