package agent

import "github.com/charliek/craze/internal/harness"

// A prompt's images on a native session (plan 033 §3.5).
//
// The envelope comes off here, as it does on ACP (live_images.go): the host
// reads the files it names from its own attachments directory, confined and
// validated from their bytes (ReadAttachments, P2, P28, P29), and the turn
// gets them as harness.Images — RunWith's file parts, after the prompt's text.
// What the model is sent of them is the harness's decision, not this one's:
// the transcript always keeps them (P4), and a model that does not accept
// images is sent a placeholder in their place (the vision strip), on this turn
// and every later one, until a /model to one that does sends them after all.
// So every image read is handed over, whatever the session's model.
//
// An image the host would not read goes as its path text with the reason,
// [Image #N: <path> (not attached: <reason>)], after any shell context block
// (PlacePathText), as on ACP; a label the visible text does not carry drops
// its image (P28). Native adds no downscale note (X13): its file part is the
// processed image, and the model is told nothing about a source it never
// sees. Interjections stay text only (P7, interjectText).

// nativeImages takes a prompt's attachment envelope off text and reads the
// images it names (promptImagesOf): the text the turn is to send — the visible
// text with the path text of every image that could not be attached — the
// images that could, in chip order, and everything for the journal. Text with
// no envelope comes back as it was, with no images. It reads files, so it runs
// with s.mu released.
func nativeImages(text string) (string, []harness.Image, []string) {
	rest, ims, problems := promptImagesOf(text, true)
	var images []harness.Image
	for _, a := range ims.atts {
		images = append(images, harness.Image{Path: a.Path, MediaType: a.MIME, Data: a.Data})
	}
	return PlacePathText(rest, ims.fallbacks), images, problems
}
