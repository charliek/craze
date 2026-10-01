// Package attach is craze's one processing path for images (plan 033 §3.2,
// P30): a screenshot pasted into the composer and an image the native Read
// tool returns go through the same Process, are held to the same limits, and
// — for a paste — are stored in the same attachments directory, which the
// host later reads back through the confined open here (OpenDir, ReadPath)
// before it sends a byte to an agent.
//
// Images are processed once, at attach time, provider-agnostic (P3,
// prior-art R7): one processed copy serves ACP and native alike. The limits
// below are the whole set; the TUI's pre-check, the store, the host's read and
// the envelope all answer to them, so a limit changed here changes everywhere.
//
// The package lives under the tool framework so Read can use it, which puts it
// inside the harness-tool depguard boundary (.golangci.yml): the standard
// library, golang.org/x/image and nothing of craze's but the framework's own
// packages, the redactor and atomicfile. In particular it cannot import
// internal/paths, so every function that touches the attachments directory is
// handed that directory as an argument (the TUI and the host both pass
// paths.AttachmentsDir()).
package attach

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"time"

	// The decoders Process and Probe accept, registered with image.Decode
	// and image.DecodeConfig: png, jpeg and gif from the standard library,
	// webp and bmp from x/image (Go has neither, and no WebP encoder at all,
	// so a WebP that has to change becomes PNG or JPEG).
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// The limits (plan 033 §3.2, P29). One set, shared by everything that makes,
// stores, reads or sends an attachment.
const (
	// MaxBytes is the processed cap: no image leaves Process, or is read back
	// by the host, larger than 3.75 MiB. That is 5 MiB once base64-encoded,
	// the most the providers craze speaks to take per image (prior-art Q4).
	MaxBytes = 3_932_160
	// MaxEdge is the processed long edge, in pixels. Larger images are scaled
	// down to it, and the host refuses a stored file that exceeds it.
	MaxEdge = 2000
	// MinEdge is the floor on both edges: smaller images are refused at
	// attach time and by the host (gx drops anything under 8 px, and roost's
	// live matrix saw a 1×1 stay text; prior-art R12).
	MinEdge = 8
	// MaxSourceBytes refuses a source file before anything is decoded.
	MaxSourceBytes = 20 << 20
	// MaxSourcePixels refuses a source by its header alone (DecodeConfig),
	// before decoding: 50 megapixels, so a crafted header cannot make
	// Process allocate gigabytes.
	MaxSourcePixels = 50_000_000
	// MaxPerMessage is the most images one message may carry, and
	// MaxMessageBytes the most raw bytes they may add up to: ten max-size
	// images would otherwise be about 50 MiB of base64 in one store line and
	// one request (P29).
	MaxPerMessage   = 10
	MaxMessageBytes = 15 << 20
	// DirBudget is the attachments directory's size cap, and MaxAge the age
	// past which a stored file goes: Sweep enforces both, age first, then the
	// oldest files while the directory is still over budget.
	DirBudget = 500 << 20
	MaxAge    = 7 * 24 * time.Hour
)

// The media types. Process emits only the first three (Stored): PNG and JPEG
// for anything it re-encodes, and WebP for a WebP it passes through untouched.
// GIF and BMP are accepted as sources and never emitted.
const (
	MIMEPNG  = "image/png"
	MIMEJPEG = "image/jpeg"
	MIMEWebP = "image/webp"
	mimeGIF  = "image/gif"
	mimeBMP  = "image/bmp"
)

// The refusals. Each message is phrased to finish "not attached: …", which is
// how the host reports a refused image to the model (internal/agent); Reason
// gives a caller those words for any error that wraps one. The ones that name
// a limit are written from it, so the reason changes with the limit.
var (
	// ErrNotImage is anything that is not a png, jpeg, gif, webp or bmp
	// image, or one that does not decode.
	ErrNotImage = errors.New("not a supported image (png, jpeg, gif, webp or bmp)")
	// ErrSourceTooLarge is a source over MaxSourceBytes.
	ErrSourceTooLarge = fmt.Errorf("larger than %d MiB", MaxSourceBytes>>20)
	// ErrTooManyPixels is a source over MaxSourcePixels.
	ErrTooManyPixels = fmt.Errorf("more than %d megapixels", MaxSourcePixels/1_000_000)
	// ErrTooSmall is an image under MinEdge on either edge.
	ErrTooSmall = fmt.Errorf("smaller than %d×%d pixels", MinEdge, MinEdge)
	// ErrTooLarge is a stored file over MaxBytes.
	ErrTooLarge = fmt.Errorf("larger than %g MiB", float64(MaxBytes)/(1<<20))
	// ErrOverEdge is a stored file over MaxEdge on its long edge.
	ErrOverEdge = fmt.Errorf("larger than %d pixels on its long edge", MaxEdge)
	// ErrWrongType is a stored file whose bytes are not the media type it
	// was named with.
	ErrWrongType = errors.New("not the image type it claims to be")
	// ErrNotRegular is a stored name that is a symlink, a FIFO, a directory
	// or anything else but a regular file.
	ErrNotRegular = errors.New("not a regular file")
	// ErrUnsafeDir is an attachments directory the host will not read from:
	// a symlink, not a directory, not 0700, not the user's own, or swapped
	// while it was being opened.
	ErrUnsafeDir = errors.New("craze's attachments directory is not private to this user")
)

// refusals is every sentinel the package refuses with, ErrNotStored (the
// store's) included: what Reason recognises.
var refusals = [...]error{
	ErrNotImage, ErrSourceTooLarge, ErrTooManyPixels, ErrTooSmall, ErrTooLarge,
	ErrOverEdge, ErrWrongType, ErrNotRegular, ErrUnsafeDir, ErrNotStored,
}

// Reason is the refusal err carries, in its sentinel's own words — the reason
// a caller puts after "not attached: " — without whatever detail was wrapped
// around it (a path, a decoder's message, the dimensions), which the model
// need not see. It is "" when err wraps none of this package's refusals; a
// missing file (fs.ErrNotExist) is the caller's to word.
func Reason(err error) string {
	for _, s := range refusals {
		if errors.Is(err, s) {
			return s.Error()
		}
	}
	return ""
}

// Image is a processed image: what Process returns and the store keeps.
type Image struct {
	// Data is the image file. For a pass-through it is the source itself
	// (sharing its backing array), with a JPEG's APP1 segments cut out.
	Data []byte
	// MIME is Data's media type: one of MIMEPNG, MIMEJPEG and MIMEWebP.
	MIME string
	// Width and Height are Data's dimensions.
	Width, Height int
	// OrigWidth and OrigHeight are the source's dimensions as displayed —
	// after its EXIF orientation, so a downscale note compares like with
	// like ("downscaled 3024×1964 → 2000×1299").
	OrigWidth, OrigHeight int
}

// Downscaled reports whether Process changed the image's dimensions.
func (i Image) Downscaled() bool {
	return i.Width != i.OrigWidth || i.Height != i.OrigHeight
}

// Config is what an image's header says: its media type and dimensions.
type Config struct {
	MIME          string
	Width, Height int
}

// extMIME is the one list of the types the store keeps — the three Process
// emits — by the file extension it gives each. Stored, Ext and StoredName all
// read it, so a type is added in one place.
var extMIME = map[string]string{"png": MIMEPNG, "jpg": MIMEJPEG, "webp": MIMEWebP}

// Stored reports whether mime is a type Process emits, and so a type a stored
// attachment may have.
func Stored(mime string) bool {
	return Ext(mime) != ""
}

// Ext is the file extension the store uses for mime, without the dot, or ""
// for a type it does not store.
func Ext(mime string) string {
	for ext, m := range extMIME {
		if m == mime {
			return ext
		}
	}
	return ""
}

// formatMIME maps image.DecodeConfig's format names to media types.
var formatMIME = map[string]string{
	"png":  MIMEPNG,
	"jpeg": MIMEJPEG,
	"gif":  mimeGIF,
	"webp": MIMEWebP,
	"bmp":  mimeBMP,
}

// Sniff is the media type b's magic bytes name, or "" when they name none of
// the five formats. It reads the bytes alone, never a file name or a claim:
// Anthropic validates the media type against the base64's magic and refuses a
// mismatch (prior-art Q4), so the host checks it the same way (Validate).
func Sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return MIMEPNG
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return MIMEJPEG
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return mimeGIF
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return MIMEWebP
	case bytes.HasPrefix(b, []byte("BM")):
		return mimeBMP
	}
	return ""
}

// Probe reads an image's header from r — DecodeConfig, nothing decoded — and
// applies the source refusals that need no more than that: not an image, over
// MaxSourcePixels, under MinEdge. It is Process's first step, and the TUI's
// synchronous pre-check of a pasted path (plan 033 §3.3), which adds the
// regular-file and MaxSourceBytes checks it can make from a stat.
func Probe(r io.Reader) (Config, error) {
	cfg, err := header(r)
	if err != nil {
		return Config{}, err
	}
	// Both are positive ints from a header, so the product fits in an int64
	// (a 32-bit platform is not one craze builds for).
	if int64(cfg.Width)*int64(cfg.Height) > MaxSourcePixels {
		return Config{}, fmt.Errorf("%w (%d×%d)", ErrTooManyPixels, cfg.Width, cfg.Height)
	}
	return cfg, nil
}

// header reads an image's header from r — DecodeConfig, nothing decoded —
// and applies the two refusals Probe and Validate share: not one of the five
// formats, and under MinEdge on either edge.
func header(r io.Reader) (Config, error) {
	cfg, format, err := image.DecodeConfig(r)
	if err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrNotImage, err)
	}
	mime := formatMIME[format]
	if mime == "" {
		return Config{}, ErrNotImage
	}
	if cfg.Width < MinEdge || cfg.Height < MinEdge {
		return Config{}, fmt.Errorf("%w (%d×%d)", ErrTooSmall, cfg.Width, cfg.Height)
	}
	return Config{MIME: mime, Width: cfg.Width, Height: cfg.Height}, nil
}

// Validate is the host's check of a stored attachment's bytes against the
// media type the envelope named for it (plan 033 §3.1, P2). Nothing the
// envelope says about the file is trusted, and the store's own work is not
// assumed either — the envelope is forgeable, and the directory is the
// user's. So, from the bytes alone: at most MaxBytes; a type the store emits;
// the magic and DecodeConfig's format both that type; at least MinEdge on
// both edges and at most MaxEdge on the long one. It returns what the header
// says.
func Validate(b []byte, mime string) (Config, error) {
	if len(b) > MaxBytes {
		return Config{}, ErrTooLarge
	}
	if !Stored(mime) || Sniff(b) != mime {
		return Config{}, ErrWrongType
	}
	cfg, err := header(bytes.NewReader(b))
	if err != nil {
		return Config{}, err
	}
	if cfg.MIME != mime {
		return Config{}, ErrWrongType
	}
	if max(cfg.Width, cfg.Height) > MaxEdge {
		return Config{}, fmt.Errorf("%w (%d×%d)", ErrOverEdge, cfg.Width, cfg.Height)
	}
	return cfg, nil
}
