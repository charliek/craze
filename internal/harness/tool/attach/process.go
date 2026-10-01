package attach

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"

	xdraw "golang.org/x/image/draw"
)

// jpegLadder is the JPEG quality step-down for an image that is still too
// large as PNG (plan 033 §3.2).
var jpegLadder = [...]int{85, 75, 60, 45}

// shrinkFactor is how much smaller each round of the ladder makes the image
// once even the last quality was too large, and maxShrinks how many rounds it
// may take. A 2000 px image at quality 45 that is still over 3.75 MiB is
// already pathological; the bound only makes the loop finite.
const (
	shrinkFactor = 0.75
	maxShrinks   = 32
)

// limits are the two limits Process's output answers to. They are a value
// rather than the constants inline so the tests can drive the downscale and
// the ladder with small images and a small cap, which keeps them fast under
// -race; Process always uses MaxBytes and MaxEdge.
type limits struct {
	maxBytes int
	maxEdge  int
}

// Process turns a source image into the one copy craze sends (plan 033 §3.2,
// P3): the composer's paste and the native Read tool both call it.
//
// It refuses, before decoding anything: a source over MaxSourceBytes; one
// that is not a png, jpeg, gif, webp or bmp image; one whose header claims
// more than MaxSourcePixels; one under MinEdge on either edge (Probe). It then
// decodes the image — every one, so a file that only looks like an image is
// refused here and not by a provider mid-turn — and either:
//
//   - passes it through, bytes untouched, when it is a PNG, WebP or JPEG no
//     larger than MaxEdge on its long edge and MaxBytes in size. A JPEG's APP1
//     segments (EXIF and XMP: camera, GPS, thumbnails) are cut out losslessly
//     first, and the size is checked after the cut. A JPEG whose EXIF
//     orientation is not 1 is not passed through: the model sees pixels, not
//     tags, so the rotation is applied by re-encoding;
//   - or re-encodes it: a GIF's first frame and every BMP always, anything
//     else when it is too large or must be rotated. The image is scaled with
//     CatmullRom to a long edge of at most MaxEdge, oriented, and encoded as
//     PNG; when that is over MaxBytes it is composited onto white (JPEG has no
//     alpha) and stepped down through JPEG qualities 85, 75, 60 and 45, then
//     made 0.75 times smaller and stepped down again, until it fits.
//
// The scale is the x/image/draw kernel scaler, whose working buffer is 32
// bytes per pixel of (output width × source height): a few hundred MB,
// briefly, for a 50 MP photo. It runs once, at attach time, off the UI
// goroutine (plan 033 §3.3).
func Process(src []byte) (Image, error) {
	return limits{maxBytes: MaxBytes, maxEdge: MaxEdge}.process(src)
}

func (l limits) process(src []byte) (Image, error) {
	if len(src) > MaxSourceBytes {
		return Image{}, fmt.Errorf("%w (%d bytes)", ErrSourceTooLarge, len(src))
	}
	cfg, err := Probe(bytes.NewReader(src))
	if err != nil {
		return Image{}, err
	}
	// pass is the file a pass-through would keep: the source, or a JPEG's
	// source with its APP1 segments cut. nil means re-encode whatever the
	// size: a GIF or BMP, or a JPEG whose header could not be walked, which
	// cannot then be stripped losslessly either.
	var pass []byte
	orientation := 1
	switch cfg.MIME {
	case MIMEPNG, MIMEWebP:
		pass = src
	case MIMEJPEG:
		if o, stripped, ok := jpegMeta(src); ok {
			orientation, pass = o, stripped
		}
	}
	img, err := decode(src, cfg)
	if err != nil {
		return Image{}, err
	}
	ow, oh := cfg.Width, cfg.Height
	if orientation >= 5 {
		// 5 to 8 turn the image a quarter: what is displayed is the stored
		// image's height wide.
		ow, oh = oh, ow
	}
	if pass != nil && orientation == 1 && max(cfg.Width, cfg.Height) <= l.maxEdge && len(pass) <= l.maxBytes {
		return Image{Data: pass, MIME: cfg.MIME, Width: cfg.Width, Height: cfg.Height, OrigWidth: ow, OrigHeight: oh}, nil
	}
	out, err := l.encode(orient(l.fit(img), orientation))
	if err != nil {
		return Image{}, err
	}
	out.OrigWidth, out.OrigHeight = ow, oh
	return out, nil
}

// decode decodes src, whose header Probe has already read as cfg. A GIF
// decodes to its first frame only (gif.Decode keeps no others), drawn onto
// the GIF's logical screen at its own offset, so a first frame smaller than
// the screen keeps its place. Any other decoded image must be the size its
// header said.
func decode(src []byte, cfg Config) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotImage, err)
	}
	if cfg.MIME == mimeGIF {
		screen := image.NewRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
		xdraw.Draw(screen, img.Bounds(), img, img.Bounds().Min, xdraw.Src)
		return screen, nil
	}
	if b := img.Bounds(); b.Dx() != cfg.Width || b.Dy() != cfg.Height {
		return nil, fmt.Errorf("%w: it decodes to %d×%d, not the %d×%d its header says", ErrNotImage, b.Dx(), b.Dy(), cfg.Width, cfg.Height)
	}
	return img, nil
}

// fit returns img as an RGBA image at the origin, scaled with CatmullRom to a
// long edge of at most l.maxEdge. RGBA, because it is the destination the
// x/image/draw scaler and the JPEG encoder both have fast paths for.
func (l limits) fit(img image.Image) *image.RGBA {
	b := img.Bounds()
	w, h := scaledTo(b.Dx(), b.Dy(), l.maxEdge)
	return scale(img, w, h)
}

// scale draws img into a new w×h RGBA image at the origin: copied when that
// is its own size, scaled with CatmullRom when it is not.
func scale(img image.Image, w, h int) *image.RGBA {
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if w == b.Dx() && h == b.Dy() {
		xdraw.Draw(dst, dst.Rect, img, b.Min, xdraw.Src)
	} else {
		xdraw.CatmullRom.Scale(dst, dst.Rect, img, b, xdraw.Src, nil)
	}
	return dst
}

// scaledTo is w×h scaled to a long edge of at most edge, keeping the aspect
// ratio. The short edge never goes under MinEdge — a 2400×8 strip becomes
// 2000×8, not 2000×7, which the host would refuse — and so never grows past
// the source's either, since every source has passed Probe.
func scaledTo(w, h, edge int) (int, int) {
	long := max(w, h)
	if long <= edge {
		return w, h
	}
	f := float64(edge) / float64(long)
	sw := max(MinEdge, int(math.Round(float64(w)*f)))
	sh := max(MinEdge, int(math.Round(float64(h)*f)))
	if w >= h {
		sw = edge
	} else {
		sh = edge
	}
	return sw, sh
}

// encode is the end of Process's re-encode path: PNG when it fits, else the
// JPEG ladder over the image composited onto white, scaling down by
// shrinkFactor between rounds. Each round scales from the full-size image, so
// the steps do not compound their blur, and is sized by scaledTo, so the short
// edge is held to MinEdge as fit holds it.
func (l limits) encode(img *image.RGBA) (Image, error) {
	var buf bytes.Buffer
	// The PNG is written through the cap, so one that will not fit — a
	// photo's, nearly always — stops at the write that passes it instead of
	// being finished and thrown away.
	switch err := png.Encode(capWriter{&buf, l.maxBytes}, img); {
	case err == nil:
		return Image{Data: bytes.Clone(buf.Bytes()), MIME: MIMEPNG, Width: img.Rect.Dx(), Height: img.Rect.Dy()}, nil
	case !errors.Is(err, errOverCap):
		return Image{}, fmt.Errorf("attach: encoding png: %w", err)
	}
	base := img
	if !img.Opaque() {
		base = flatten(img)
	}
	cur := base
	w, h := base.Rect.Dx(), base.Rect.Dy()
	for round := 1; ; round++ {
		for _, q := range jpegLadder {
			buf.Reset()
			if err := jpeg.Encode(&buf, cur, &jpeg.Options{Quality: q}); err != nil {
				return Image{}, fmt.Errorf("attach: encoding jpeg: %w", err)
			}
			if buf.Len() <= l.maxBytes {
				return Image{Data: bytes.Clone(buf.Bytes()), MIME: MIMEJPEG, Width: cur.Rect.Dx(), Height: cur.Rect.Dy()}, nil
			}
		}
		edge := max(MinEdge, int(math.Round(float64(max(w, h))*math.Pow(shrinkFactor, float64(round)))))
		nw, nh := scaledTo(w, h, edge)
		if round > maxShrinks || (nw == cur.Rect.Dx() && nh == cur.Rect.Dy()) {
			return Image{}, fmt.Errorf("attach: no encoding of the image fits in %d bytes", l.maxBytes)
		}
		cur = scale(base, nw, nh)
	}
}

// errOverCap is capWriter's refusal.
var errOverCap = errors.New("attach: over the size cap")

// capWriter is a buffer that refuses any write that would take it past max.
// An encoder writing through it either finishes within max, or fails at the
// first write past it with errOverCap.
type capWriter struct {
	buf *bytes.Buffer
	max int
}

func (w capWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		return 0, errOverCap
	}
	return w.buf.Write(p)
}

// flatten composites img onto white: JPEG has no alpha, and a transparent
// pixel encoded as-is comes out black, which on a screenshot of a dark-text
// window is the text vanishing. encode skips it for an opaque image, which it
// would only copy.
func flatten(img *image.RGBA) *image.RGBA {
	dst := image.NewRGBA(img.Rect)
	xdraw.Draw(dst, dst.Rect, image.White, image.Point{}, xdraw.Src)
	xdraw.Draw(dst, dst.Rect, img, img.Rect.Min, xdraw.Over)
	return dst
}

// orient applies an EXIF orientation (1 to 8) to img, returning img itself for
// 1 and anything out of range. The eight are the four rotations, each with or
// without a mirror; 5 to 8 swap the width and the height. A stored pixel at
// (x, y) is drawn at:
//
//	2 mirrored          (w-1-x, y)
//	3 turned 180°       (w-1-x, h-1-y)
//	4 flipped           (x, h-1-y)
//	5 transposed        (y, x)
//	6 turned 90° right  (h-1-y, x)
//	7 transversed       (h-1-y, w-1-x)
//	8 turned 90° left   (y, w-1-x)
func orient(img *image.RGBA, o int) *image.RGBA {
	if o < 2 || o > 8 {
		return img
	}
	w, h := img.Rect.Dx(), img.Rect.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range h {
		row := img.Pix[y*img.Stride:]
		for x := range w {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			copy(dst.Pix[dy*dst.Stride+dx*4:dy*dst.Stride+dx*4+4], row[x*4:x*4+4])
		}
	}
	return dst
}
