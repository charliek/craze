package attach

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"testing"

	"golang.org/x/image/bmp"
)

// Fixtures are generated, never checked in (plan 033 §5 C1): small images
// built per test, and a few hand-made headers for what no encoder will write.

// Four well-separated colours, one per quadrant (quadrants).
var (
	red   = color.RGBA{0xff, 0, 0, 0xff}
	green = color.RGBA{0, 0xff, 0, 0xff}
	blue  = color.RGBA{0, 0, 0xff, 0xff}
	white = color.RGBA{0xff, 0xff, 0xff, 0xff}
)

// quadrants is a w×h image whose top-left, top-right, bottom-left and
// bottom-right quarters are red, green, blue and white.
func quadrants(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			switch {
			case x < w/2 && y < h/2:
				img.Set(x, y, red)
			case y < h/2:
				img.Set(x, y, green)
			case x < w/2:
				img.Set(x, y, blue)
			default:
				img.Set(x, y, white)
			}
		}
	}
	return img
}

// gradient is a w×h opaque image that compresses well.
func gradient(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), uint8(x + y), 0xff})
		}
	}
	return img
}

// noise is a w×h opaque image of seeded random pixels: as close to
// incompressible as an image gets, so it drives the size ladder.
func noise(w, h int, seed uint64) *image.NRGBA {
	r := rand.New(rand.NewPCG(seed, seed))
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = uint8(r.Uint32()), uint8(r.Uint32()), uint8(r.Uint32()), 0xff
	}
	return img
}

func encodePNG(t testing.TB, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeJPEG(t testing.TB, img image.Image, q int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeBMP(t testing.TB, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := bmp.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// encodeGIF writes the frames as one GIF on a screen of the given size, each
// frame at its own bounds.
func encodeGIF(t testing.TB, screen image.Rectangle, frames ...*image.Paletted) []byte {
	t.Helper()
	g := &gif.GIF{Image: frames, Delay: make([]int, len(frames)), Config: image.Config{Width: screen.Dx(), Height: screen.Dy(), ColorModel: frames[0].Palette}}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// solidFrame is a paletted frame over r filled with c, whose palette also
// holds a transparent entry.
func solidFrame(r image.Rectangle, c color.Color) *image.Paletted {
	f := image.NewPaletted(r, color.Palette{color.Transparent, c})
	for i := range f.Pix {
		f.Pix[i] = 1
	}
	return f
}

// segment is a JPEG marker segment: 0xff, the marker, the big-endian length
// (which counts itself) and the payload.
func segment(marker byte, payload []byte) []byte {
	s := []byte{0xff, marker, 0, 0}
	binary.BigEndian.PutUint16(s[2:], uint16(len(payload)+2))
	return append(s, payload...)
}

// afterSOI inserts segments into a JPEG right after its SOI.
func afterSOI(jpg []byte, segs ...[]byte) []byte {
	out := append([]byte{}, jpg[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, jpg[2:]...)
}

// exifAPP1 is an APP1 EXIF segment whose IFD0 holds an orientation tag (when
// orientation is not 0) after an unrelated tag, in either byte order.
func exifAPP1(orientation uint16, bigEndian bool) []byte {
	var bo interface {
		binary.ByteOrder
		binary.AppendByteOrder
	} = binary.LittleEndian
	tiff := []byte("II*\x00")
	if bigEndian {
		bo, tiff = binary.BigEndian, []byte("MM\x00*")
	}
	tiff = bo.AppendUint32(tiff, 8) // IFD0 right after the header
	entries := 1
	if orientation != 0 {
		entries = 2
	}
	tiff = bo.AppendUint16(tiff, uint16(entries))
	// ImageDescription (0x010e), ASCII, count 4, value inline: "abc\0".
	tiff = bo.AppendUint16(tiff, 0x010e)
	tiff = bo.AppendUint16(tiff, 2)
	tiff = bo.AppendUint32(tiff, 4)
	tiff = append(tiff, 'a', 'b', 'c', 0)
	if orientation != 0 {
		tiff = bo.AppendUint16(tiff, tagOrientation)
		tiff = bo.AppendUint16(tiff, typeShort)
		tiff = bo.AppendUint32(tiff, 1)
		tiff = bo.AppendUint16(tiff, orientation)
		tiff = append(tiff, 0, 0)
	}
	tiff = bo.AppendUint32(tiff, 0) // no next IFD
	return segment(markerAPP1, append([]byte(exifHeader), tiff...))
}

// iptcAPP13 is an APP13 Photoshop segment holding one IPTC dataset (2:80,
// the byline) with text in it.
func iptcAPP13(text string) []byte {
	iptc := []byte{0x1c, 2, 80, 0, 0}
	binary.BigEndian.PutUint16(iptc[3:], uint16(len(text)))
	iptc = append(iptc, text...)
	payload := []byte("Photoshop 3.0\x008BIM\x04\x04\x00\x00")
	payload = binary.BigEndian.AppendUint32(payload, uint32(len(iptc)))
	return segment(markerAPP13, append(payload, iptc...))
}

// iccAPP2 is an APP2 segment holding (the first and only chunk of) an ICC
// profile: the ICC_PROFILE\0 header, the chunk's number and count, a body.
func iccAPP2() []byte {
	return segment(markerAPP2, append([]byte("ICC_PROFILE\x00\x01\x01"), bytes.Repeat([]byte{7}, 40)...))
}

// mpfAPP2 is an APP2 segment of a phone's MPF (CIPA DC-007, Multi-Picture
// Format): the MPF\0 header, a big-endian TIFF header, and then preview — as
// the private part a vendor fills, the preview image's own bytes (EXIF and
// all) carried inside the segment, which is what r4 #3 is about.
func mpfAPP2(preview []byte) []byte {
	return segment(markerAPP2, append([]byte("MPF\x00MM\x00*\x00\x00\x00\x08"), preview...))
}

// xmpAPP1 is an APP1 XMP segment padded to about n bytes.
func xmpAPP1(n int) []byte {
	payload := append([]byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta/>"), bytes.Repeat([]byte(" "), n)...)
	return segment(markerAPP1, payload)
}

// pngHeader is a PNG signature and an IHDR chunk claiming w×h (8-bit RGBA),
// with a correct CRC, followed by pad zero bytes: enough for DecodeConfig and
// Validate, which read no further, at whatever size a test needs and with
// whatever dimensions it wants to lie about.
func pngHeader(w, h uint32, pad int) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 6, 0, 0, 0) // depth 8, RGBA, deflate, filter 0, no interlace
	b := []byte("\x89PNG\r\n\x1a\n")
	b = binary.BigEndian.AppendUint32(b, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b = append(b, chunk...)
	b = binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(chunk))
	return append(b, make([]byte, pad)...)
}

// webpSolid is a lossless WebP (VP8L) of one colour. Go has no WebP encoder,
// and a solid image is the one VP8L can say in a few bytes: no transforms, no
// colour cache, no meta codes, and every prefix code a "simple" code of one
// symbol, which costs zero bits a pixel (WebP lossless bitstream spec §3.7.2).
func webpSolid(w, h int, c color.NRGBA) []byte {
	var bw bitWriter
	bw.write(0x2f, 8) // VP8L signature
	bw.write(uint32(w-1), 14)
	bw.write(uint32(h-1), 14)
	bw.write(1, 1) // alpha hint
	bw.write(0, 3) // version
	bw.write(0, 1) // no transform
	bw.write(0, 1) // no colour cache
	bw.write(0, 1) // no meta prefix codes
	// Green, red, blue, alpha: simple, one symbol, eight bits wide.
	for _, v := range []uint8{c.G, c.R, c.B, c.A} {
		bw.write(1, 1)
		bw.write(0, 1)
		bw.write(1, 1)
		bw.write(uint32(v), 8)
	}
	// Distance: simple, one symbol, one bit wide, symbol 0.
	bw.write(1, 1)
	bw.write(0, 1)
	bw.write(0, 1)
	bw.write(0, 1)
	data := bw.bytes()
	if len(data)%2 == 1 {
		data = append(data, 0) // RIFF chunks are padded to even sizes
	}
	b := []byte("RIFF")
	b = binary.LittleEndian.AppendUint32(b, uint32(4+8+len(data)))
	b = append(b, "WEBPVP8L"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	return append(b, data...)
}

// bitWriter packs bits least significant first, as VP8L reads them.
type bitWriter struct {
	buf   []byte
	acc   uint64
	nbits uint
}

func (w *bitWriter) write(v uint32, n uint) {
	w.acc |= uint64(v&(1<<n-1)) << w.nbits
	w.nbits += n
	for w.nbits >= 8 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc >>= 8
		w.nbits -= 8
	}
}

func (w *bitWriter) bytes() []byte {
	if w.nbits > 0 {
		return append(w.buf, byte(w.acc))
	}
	return w.buf
}

// near reports whether two colours are within tol on every channel: JPEG is
// lossy, and a sample at a region's centre is close, not exact.
func near(a, b color.Color, tol int) bool {
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	d := func(x, y uint32) bool {
		diff := int(x>>8) - int(y>>8)
		return diff <= tol && diff >= -tol
	}
	return d(ar, br) && d(ag, bg) && d(ab, bb) && d(aa, ba)
}

// decoded decodes an output of Process, failing the test if it does not.
func decoded(t testing.TB, b []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("Process's output does not decode: %v", err)
	}
	return img
}
