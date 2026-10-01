package attach

import (
	"bytes"
	"encoding/binary"
)

// The PNG and WebP halves of what a pass-through keeps (plan 033 C6r, item 8;
// jpeg.go is the JPEG half). A pass-through sends the user's own file, so it
// must carry the image and nothing else of theirs: a macOS screenshot holds an
// iTXt XMP packet, a phone or an editor writes eXIf (the camera, the time, GPS)
// and tEXt or zTXt comments, and a WebP carries EXIF and XMP chunks.

// pngSignature leads every PNG file.
const pngSignature = "\x89PNG\r\n\x1a\n"

// pngKeeps reports whether a pass-through keeps a PNG chunk of this type: the
// four critical chunks (IHDR, PLTE, IDAT, IEND), and the ancillary chunks that
// say how to draw the pixels — transparency (tRNS), colour space and gamma
// (gAMA, cHRM, sRGB, iCCP, sBIT, and the HDR trio cICP, mDCV, cLLI), the
// physical pixel size (pHYs) and the background colour (bKGD).
//
// Every other ancillary chunk is cut: eXIf, tEXt, zTXt, iTXt and tIME, which
// are the user's, and the ones craze has no reason to send — hIST, sPLT, an
// APNG's acTL/fcTL/fdAT (the default image, IDAT, is what is sent, as Go
// decodes it), a private chunk.
func pngKeeps(typ string) bool {
	switch typ {
	case "IHDR", "PLTE", "IDAT", "IEND",
		"tRNS", "gAMA", "cHRM", "sRGB", "iCCP", "sBIT", "pHYs", "bKGD",
		"cICP", "mDCV", "cLLI":
		return true
	}
	return false
}

// pngMeta walks a PNG's chunks from its signature to IEND and returns the file
// with every chunk pngKeeps does not keep cut out, and nothing after IEND (a
// cropping tool's leftover of the uncropped original, a vendor trailer): every
// kept chunk byte for byte, IDAT included, so the pixels are the same bytes. A
// file with nothing to cut and nothing past IEND comes back as the same slice.
//
// ok is false — the caller re-encodes — when the chunks cannot be walked to an
// IEND (a length running off the end, a type that is not four ASCII letters)
// or when there is a critical chunk (an upper-case first letter) the list does
// not know: a chunk a decoder may not skip is not one to guess about. It
// checks no CRC: the decoder that runs next checks every one, a cut chunk's
// included.
//
// It is total over arbitrary bytes, and moves forward at least 12 bytes a
// chunk.
func pngMeta(b []byte) (stripped []byte, ok bool) {
	if !bytes.HasPrefix(b, []byte(pngSignature)) {
		return nil, false
	}
	c := cutter{b: b}
	i := len(pngSignature)
	for {
		// A chunk is its length, its type, its data and its CRC.
		if len(b)-i < 12 {
			return nil, false
		}
		n := binary.BigEndian.Uint32(b[i:])
		if uint64(n) > uint64(len(b)-i-12) {
			return nil, false
		}
		typ := b[i+4 : i+8]
		if !pngChunkType(typ) {
			return nil, false
		}
		end := i + 12 + int(n)
		switch t := string(typ); {
		case t == "IEND":
			return c.end(end), true
		case pngKeeps(t):
		case typ[0]&0x20 == 0:
			return nil, false
		default:
			c.cut(i, end)
		}
		i = end
	}
}

// pngChunkType reports whether typ is a PNG chunk type: four ASCII letters.
func pngChunkType(typ []byte) bool {
	for _, ch := range typ {
		if (ch < 'A' || ch > 'Z') && (ch < 'a' || ch > 'z') {
			return false
		}
	}
	return true
}

// The VP8X flags that say a WebP carries metadata (WebP container
// specification, "Extended file format").
const (
	webpFlagXMP  = 1 << 2
	webpFlagEXIF = 1 << 3
)

// webpPlain reports whether a WebP can pass through as it is: a RIFF that is
// exactly the file (nothing after it), whose chunks are only the image's own —
// VP8 or VP8L, ALPH, VP8X and ICCP (the colour profile) — with neither of
// VP8X's metadata flags set. Anything else is re-encoded (as PNG: Go has no
// WebP encoder), which keeps the pixels and none of the rest: an EXIF or XMP
// chunk, a flag promising one, a chunk this list does not know, a trailer, a
// walk that runs off the end. There is no lossless cut, as for PNG and JPEG,
// because VP8X's flags and the RIFF size would have to be rewritten with it.
//
// It is total over arbitrary bytes, and moves forward at least 8 bytes a
// chunk.
func webpPlain(b []byte) bool {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return false
	}
	if uint64(binary.LittleEndian.Uint32(b[4:8]))+8 != uint64(len(b)) {
		return false
	}
	for i := 12; i < len(b); {
		if len(b)-i < 8 {
			return false
		}
		n := uint64(binary.LittleEndian.Uint32(b[i+4:]))
		n += n & 1 // a chunk is padded to an even size
		if n > uint64(len(b)-i-8) {
			return false
		}
		switch string(b[i : i+4]) {
		case "VP8X":
			if n == 0 || b[i+8]&(webpFlagEXIF|webpFlagXMP) != 0 {
				return false
			}
		case "VP8 ", "VP8L", "ALPH", "ICCP":
		default:
			return false
		}
		i += 8 + int(n)
	}
	return true
}

// cutter is b with ranges cut out of it, in order, built as they are found:
// nothing is allocated until the first cut, and then never more than b's size,
// however many ranges there are (a file of nothing but four-byte JPEG comments
// is millions of them).
type cutter struct {
	b   []byte
	out []byte
	at  int // the start of what is still to be copied
}

// cut removes b[from:to]; from is at or past the end of the last range cut.
func (c *cutter) cut(from, to int) {
	if c.out == nil {
		c.out = make([]byte, 0, len(c.b))
	}
	c.out = append(c.out, c.b[c.at:from]...)
	c.at = to
}

// end is b up to end with every range cut out: b itself, capped at end, when
// nothing was.
func (c *cutter) end(end int) []byte {
	if c.out == nil {
		return c.b[:end:end]
	}
	return append(c.out, c.b[c.at:end]...)
}
