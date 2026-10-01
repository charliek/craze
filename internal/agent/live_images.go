package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/paths"
)

// A prompt's images on an ACP session (plan 033 §3.4).
//
// The images ride the prompt's text as the attachment envelope (attachments.go,
// P1) all the way to the session. Here, and nowhere earlier, the envelope comes
// off: the host reads the files it names, confined to its own attachments
// directory and validated from their bytes (ReadAttachments, P2, P28, P29), and
// the prompt goes out as
//
//	block 1      the draft as the user sees it, chips and all, plus path text
//	             for any image that could not be attached
//	expansions   one block per plugin reference, as before
//	images       one {type:"image", data, mimeType, uri} block per image, in
//	             chip order
//	note         one text block, only when an image was downscaled:
//	             [Image #1 was downscaled from 3024×1964 to 2000×1299]
//
// so block 1 — what grok's queue correlation compares — is the same with images
// as without (P6). An agent whose initialize says it takes no image blocks gets
// each image as [Image #N: <path>] text in its block's place, unless its
// provider sends them regardless (Provider.imagesDespiteCapability: grok and gx,
// P5). And an agent that refuses the image blocks with -32602 before doing
// anything with the prompt gets the same prompt once more with path text
// (imageResendWire), so a file the user attached is never dropped silently.
//
// Interjections carry no images at all (P7): interjectText turns an envelope
// into path text at both sessions' Interject.

// Journal diag kinds for images (plan 033 §3.4). attachments is what the host
// refused of an envelope — dropped labels, files it would not read, limits —
// with "via" saying which path read it (prompt, interject, native); the user is
// never shown a new event for these (§4), the model gets the path text, and the
// journal gets the reason. image_resend is the -32602 fallback's second
// attempt.
const (
	diagAttachments = "attachments"
	diagImageResend = "image_resend"
)

// noteAttachments journals what the host refused of a prompt's envelope, if
// anything. Like every note, it is written with the session lock released.
func noteAttachments(log *EventLog, via string, problems []string) {
	if len(problems) == 0 {
		return
	}
	log.Note(journal.DiagNote{Kind: diagAttachments, Fields: map[string]any{"via": via, "problems": problems}})
}

// interjectText is text as an interjection carries it (plan 033 P7): a leading
// attachment envelope becomes [Image #N: <path>] lines (AttachmentsAsPathText),
// and anything SplitAttachments had to say about it is journaled. Both sessions'
// Interject apply it, so a socket client sending session.prompt with mode
// "interject" and an envelope is held to the same rule as the TUI, which never
// sends one there. Text with no envelope comes back as it was.
func interjectText(log *EventLog, text string) string {
	out, problems := AttachmentsAsPathText(text)
	noteAttachments(log, "interject", problems)
	return out
}

// promptImages is a prompt's images as the host read them, which promptBlocks
// builds everything after block 1 from.
type promptImages struct {
	// atts are the images read and validated, in chip order (ReadAttachments).
	atts []Attachment
	// fallbacks are the path text of the images that could not be attached,
	// with their reasons, for block 1 (PlacePathText).
	fallbacks []string
	// asText sends each of atts as [Image #N: <path>] text in the place its
	// image block would have taken: the agent takes no image blocks, or it
	// refused them and this is the resend.
	asText bool
}

// sendsImages says the prompt carries at least one image block.
func (ims promptImages) sendsImages() bool { return len(ims.atts) > 0 && !ims.asText }

// asPathText is the same images with every one as path text: the resend.
func (ims promptImages) asPathText() promptImages {
	ims.asText = true
	return ims
}

// promptImagesOf takes a prompt's attachment envelope off text and reads the
// images it asks for from the host's own attachments directory (plan 033 §3.1,
// §3.4). rest is the text without the envelope — block 1 before any fallback
// is placed in it, and the visible text the label rule (P28) is judged against.
// images is whether the agent is to get image blocks; when it is not, the
// images are read all the same, so a refusal keeps its reason and an image
// the user never saw a chip for is still dropped, and each that passes goes as
// path text. problems is everything for the journal.
//
// Text with no envelope is rest unchanged and no images; an envelope over its
// limits is already path text in rest (SplitAttachments).
func promptImagesOf(text string, images bool) (rest string, ims promptImages, problems []string) {
	refs, rest, problems := SplitAttachments(text)
	if len(refs) == 0 {
		return rest, promptImages{}, problems
	}
	atts, fallbacks, read := ReadAttachments(paths.AttachmentsDir(), refs, rest)
	return rest, promptImages{atts: atts, fallbacks: fallbacks, asText: !images}, append(problems, read...)
}

// readImages is promptImagesOf for one of this session's prompts, with what
// the host refused journaled (via "prompt"). It reads files, so it runs with
// s.mu released.
func (s *session) readImages(text string, images bool) (string, promptImages) {
	rest, ims, problems := promptImagesOf(text, images)
	noteAttachments(s.log, "prompt", problems)
	return rest, ims
}

// imageBlocks is everything a prompt carries after its expansions (plan 033
// §3.4): one block per image in chip order — the image itself, or its path
// text when ims.asText — then, when any of them was downscaled by the TUI, one
// text block saying from what. The original size comes from the envelope and
// is used only if Attachment.Original allows it (X13): it is a claim, and it
// only ever reaches the note's words. The note stays with path text too: the
// file at that path is the downscaled copy.
func imageBlocks(ims promptImages) []acp.ContentBlock {
	if len(ims.atts) == 0 {
		return nil
	}
	out := make([]acp.ContentBlock, 0, len(ims.atts)+1)
	var notes []string
	for _, a := range ims.atts {
		if ims.asText {
			out = append(out, acp.ContentBlock{Type: "text", Text: attachmentPathText(a.N, a.Path, "")})
		} else {
			out = append(out, imageBlock(a))
		}
		if w, h, ok := a.Original(); ok {
			notes = append(notes, fmt.Sprintf("[Image #%d was downscaled from %d×%d to %d×%d]", a.N, w, h, a.Width, a.Height))
		}
	}
	if len(notes) > 0 {
		out = append(out, acp.ContentBlock{Type: "text", Text: strings.Join(notes, "\n")})
	}
	return out
}

// imageBlock is one attachment as ACP's image content: its bytes in standard
// base64, its type, and the file:// URL of the copy they were read from.
func imageBlock(a Attachment) acp.ContentBlock {
	return acp.ContentBlock{
		Type:     "image",
		Data:     base64.StdEncoding.EncodeToString(a.Data),
		MimeType: a.MIME,
		URI:      fileURI(a.Path),
	}
}

// fileURI is an absolute path as a file:// URL, escaped as one (RFC 8089): a
// CRAZE_HOME with a space in it still names one file.
func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// testBeforeResend runs, when set, between a prompt the agent refused with
// -32602 and the locked section that decides whether it goes again: the one
// point where a test can land a Cancel between the two attempts. It is a var
// only so the tests can set it; nothing in craze writes it.
var testBeforeResend func(s *session)

// imageResendWire decides plan 033 §3.4's fallback for a prompt whose
// session/prompt came back with err, and returns the wire its resend is to
// report into — nil when there is no resend and err stands as it is.
//
// A resend needs all of:
//
//   - err is the agent's own -32602 (an *acp.RPCError with
//     acp.CodeInvalidParams);
//   - the prompt carried image blocks, which is what such a refusal is taken
//     to be about;
//   - the agent sent nothing of its own while the prompt was in flight
//     (acp.Client.Heard: no update, tool call, permission, plan, nothing but
//     grok's turn bookkeeping) — a prompt the agent began on is not one it
//     refused, and sending it again could do its work twice. The signal is
//     the client's, taken on its read loop, so it is ordered against the
//     reply: anything the agent sent before refusing has been counted;
//   - no Cancel has marked the prompt since its claim (s.cancelling, read in
//     the locked section below).
//
// That section also installs the resend's own wire as s.wire. The first
// attempt's wire was settled sent when its bytes went out, so a Cancel that
// found it would write its session/cancel at once — possibly ahead of the
// resend, to an agent that drops a cancel for a turn it has not seen, and the
// resend would run on as though Esc had never been pressed. A Cancel from here
// on finds the new wire instead, still pending, and waits for the resend's
// bytes as it waits for any prompt's (plan 017's rule). The caller takes the
// new wire as the claim's, so the claim's release settles and clears it.
func (s *session) imageResendWire(err error, ims promptImages, client *acp.Client) *turnWire {
	var rpc *acp.RPCError
	if !ims.sendsImages() || !errors.As(err, &rpc) || rpc.Code != acp.CodeInvalidParams || client.Heard() {
		return nil
	}
	if testBeforeResend != nil {
		testBeforeResend(s)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelling {
		return nil
	}
	w := &turnWire{done: make(chan struct{})}
	s.wire = w
	return w
}

// resendAsPathText is the fallback's second and last attempt (plan 033 §3.4):
// blocks are the prompt again with every image as path text, sent with no
// accepted hook — the expansions were announced by the first attempt and are
// not announced twice — and reporting into wire, the resend's own
// (imageResendWire). first is the refusal, journaled with the resend; images is
// how many images went as text. Whatever this attempt returns is the prompt's
// answer: a second refusal is not resent.
func (s *session) resendAsPathText(ctx context.Context, client *acp.Client, wire *turnWire, blocks []acp.ContentBlock, first error, images int) (*acp.PromptResult, error) {
	s.log.Note(journal.DiagNote{Kind: diagImageResend, Fields: map[string]any{"images": images, "refusal": first.Error()}})
	res, err := client.PromptBlocks(ctx, blocks, nil, func() { s.publishWire(wire, wireSent) })
	// Settled as the first attempt's is (prompt): the first outcome wins, so a
	// resend whose bytes went out and whose reply then failed stays sent.
	switch {
	case refusedBeforeWire(err):
		s.publishWire(wire, wireRefused)
	case err != nil:
		s.publishWire(wire, wireFailed)
	default:
		s.publishWire(wire, wireSent)
	}
	return res, err
}
