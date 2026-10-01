package attach

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"testing"
)

// The PNG and WebP pass-through's metadata (plan 033 C6r, item 8).

// pngChunk is a PNG chunk: the big-endian data length, the type, the data and
// the CRC of type and data.
func pngChunk(typ string, data []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	td := append([]byte(typ), data...)
	b = append(b, td...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(td))
}

// pngChunkStarts is where each chunk of a known-good PNG starts, IHDR first:
// the test's own walk, not pngMeta's.
func pngChunkStarts(b []byte) (starts []int, types []string) {
	for i := len(pngSignature); i+12 <= len(b); {
		starts = append(starts, i)
		types = append(types, string(b[i+4:i+8]))
		i += 12 + int(binary.BigEndian.Uint32(b[i:]))
	}
	return starts, types
}

// pngPlantable is where a chunk may be planted in a known-good PNG and leave
// it one Go decodes to the same pixels: before every chunk after IHDR, but
// never between two IDATs, whose data Go reads as one stream.
func pngPlantable(b []byte) []int {
	starts, types := pngChunkStarts(b)
	var out []int
	for k := 1; k < len(starts); k++ {
		if types[k-1] == "IDAT" && types[k] == "IDAT" {
			continue
		}
		out = append(out, starts[k])
	}
	return out
}

// afterIHDR is a PNG with chunks inserted right after its IHDR.
func afterIHDR(b []byte, chunks ...[]byte) []byte {
	return insertAt(b, len(pngSignature)+25, bytes.Join(chunks, nil))
}

// beforeIEND is a PNG with chunks inserted right before its IEND.
func beforeIEND(b []byte, chunks ...[]byte) []byte {
	return insertAt(b, len(b)-12, bytes.Join(chunks, nil))
}

// exifTIFF is the TIFF body of exifAPP1: what a PNG's eXIf and a WebP's EXIF
// chunk hold, with no "Exif\0\0" in front.
func exifTIFF(orientation uint16) []byte {
	return exifAPP1(orientation, false)[4+len(exifHeader):]
}

// pngMetaFixtures is the PNGs the walk is tested on: one IDAT, several IDATs
// (Go's encoder splits its stream every 32 KiB), and a paletted one (PLTE).
func pngMetaFixtures(t testing.TB) map[string][]byte {
	t.Helper()
	pal := image.NewPaletted(image.Rect(0, 0, 20, 12), color.Palette{color.Black, color.White, color.NRGBA{200, 10, 10, 255}})
	for i := range pal.Pix {
		pal.Pix[i] = uint8(i % 3)
	}
	return map[string][]byte{
		"one IDAT":   encodePNG(t, gradient(24, 16)),
		"many IDATs": encodePNG(t, noise(160, 160, 7)),
		"paletted":   encodePNG(t, pal),
	}
}

// metadataChunks is every PNG chunk the walk cuts that the tests plant: the
// user's (eXIf, tEXt, zTXt, iTXt, tIME) and ones craze has no use for (a
// private chunk, an APNG's acTL).
var metadataChunks = []string{"eXIf", "tEXt", "zTXt", "iTXt", "tIME", "prVt", "acTL"}

// TestProcessStripsPNGMetadata: a PNG's eXIf, tEXt, zTXt, iTXt and tIME — a
// macOS screenshot's XMP packet among them — and any other ancillary chunk
// that is not about drawing the pixels are cut out, before IDAT or after it,
// and so is anything after IEND (a cropping tool's leftover of the original).
// What is sent is byte for byte the PNG the chunks were inserted into: its
// IDAT bytes, and every other chunk, untouched.
func TestProcessStripsPNGMetadata(t *testing.T) {
	plain := encodePNG(t, gradient(24, 16))
	xmp := pngChunk("iTXt", []byte("XML:com.adobe.xmp\x00\x00\x00\x00\x00<x:xmpmeta><exif:UserComment>Jane's desk</exif:UserComment></x:xmpmeta>"))
	for _, tc := range []struct {
		name string
		src  []byte
	}{
		{"a macOS screenshot's iTXt XMP", afterIHDR(plain, xmp)},
		{"eXIf", afterIHDR(plain, pngChunk("eXIf", exifTIFF(1)))},
		{"tEXt, zTXt and tIME", afterIHDR(plain, pngChunk("tEXt", []byte("Author\x00Jane Doe")), pngChunk("zTXt", []byte("Comment\x00\x00x\x9c\x03\x00\x00\x00\x00\x01")), pngChunk("tIME", []byte{0x07, 0xea, 10, 1, 9, 30, 0}))},
		{"tEXt, iTXt and eXIf after IDAT", beforeIEND(plain, pngChunk("tEXt", []byte("Location\x00home")), xmp, pngChunk("eXIf", exifTIFF(1)))},
		{"a private chunk and an APNG's acTL", afterIHDR(plain, pngChunk("prVt", []byte("secret")), pngChunk("acTL", make([]byte, 8)))},
		{"a trailer after IEND", append(append([]byte{}, plain...), "IDAT of the uncropped original"...)},
		{"all of it", append(beforeIEND(afterIHDR(plain, xmp, pngChunk("eXIf", exifTIFF(1))), pngChunk("tEXt", []byte("a\x00b"))), "trailer"...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !samePixels(tc.src, plain) {
				t.Fatal("fixture: the planted PNG does not decode to the plain one's pixels")
			}
			got, err := Process(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Data, plain) {
				t.Fatalf("got %d bytes, want the plain PNG's %d", len(got.Data), len(plain))
			}
			if got.MIME != MIMEPNG || got.Width != 24 || got.Height != 16 || got.Downscaled() {
				t.Fatalf("got %s %d×%d from %d×%d", got.MIME, got.Width, got.Height, got.OrigWidth, got.OrigHeight)
			}
		})
	}
}

// TestPNGMetaKeepsWhatDrawsThePixels: the ancillary chunks that say how to
// draw the pixels — colour space, gamma, transparency, physical size,
// background — stay, byte for byte and in place, beside the metadata cut
// around them; a PNG of nothing else comes back as the same slice.
func TestPNGMetaKeepsWhatDrawsThePixels(t *testing.T) {
	plain := encodePNG(t, gradient(24, 16))
	var kept [][]byte
	for _, typ := range []string{"gAMA", "cHRM", "sRGB", "iCCP", "sBIT", "pHYs", "bKGD", "tRNS", "cICP", "mDCV", "cLLI"} {
		kept = append(kept, pngChunk(typ, []byte{1, 2, 3, 4}))
	}
	want := afterIHDR(plain, kept...)
	if s, ok := pngMeta(want); !ok || !bytes.Equal(s, want) || &s[0] != &want[0] {
		t.Fatalf("pngMeta = (%d bytes, %v) of a PNG with nothing to cut, want it back as it is", len(s), ok)
	}
	planted := afterIHDR(plain, append(append([][]byte{pngChunk("tEXt", []byte("a\x00b"))}, kept...), pngChunk("eXIf", exifTIFF(1)))...)
	if s, ok := pngMeta(planted); !ok || !bytes.Equal(s, want) {
		t.Fatalf("pngMeta = (%d bytes, %v), want the %d bytes with the rendering chunks kept", len(s), ok, len(want))
	}
}

// TestPNGMetaRefusesWhatItCannotWalk: a file that cannot be walked to IEND,
// or that has a critical chunk the walk does not know, is not cut — Process
// re-encodes it — and Process's re-encode of the one Go still decodes is a
// fresh PNG of the same pixels, without the chunk.
func TestPNGMetaRefusesWhatItCannotWalk(t *testing.T) {
	plain := encodePNG(t, gradient(24, 16))
	unknownCritical := afterIHDR(plain, pngChunk("XXXX", []byte("must understand")), pngChunk("tEXt", []byte("a\x00b")))
	for name, b := range map[string][]byte{
		"not a PNG":                 []byte("GIF89a"),
		"no IEND":                   plain[:len(plain)-12],
		"a length off the end":      append(append([]byte{}, plain[:33]...), 0x7f, 0xff, 0xff, 0xff, 't', 'E', 'X', 't'),
		"a type that is not ASCII":  afterIHDR(plain, pngChunk("t\x00Xt", nil)),
		"an unknown critical chunk": unknownCritical,
		"a signature alone":         []byte(pngSignature),
	} {
		if s, ok := pngMeta(b); ok || s != nil {
			t.Errorf("%s: pngMeta = (%d bytes, %v), want a refusal", name, len(s), ok)
		}
	}
	got, err := Process(unknownCritical)
	if err != nil {
		t.Fatal(err)
	}
	if got.MIME != MIMEPNG || bytes.Contains(got.Data, []byte("XXXX")) || bytes.Contains(got.Data, []byte("tEXt")) || !samePixels(got.Data, plain) {
		t.Fatalf("the re-encode: %s, %d bytes", got.MIME, len(got.Data))
	}
}

// riffChunk is a RIFF chunk: the FourCC, the little-endian data length, the
// data, and a pad byte when the length is odd.
func riffChunk(fcc string, data []byte) []byte {
	b := append([]byte(fcc), binary.LittleEndian.AppendUint32(nil, uint32(len(data)))...)
	b = append(b, data...)
	if len(data)%2 == 1 {
		b = append(b, 0)
	}
	return b
}

// webpExtended is webpSolid's image in the extended format: a VP8X with these
// flags and a w×h canvas, then before, the VP8L, and after — the container's
// own order for an ICC profile ahead of the image and EXIF and XMP behind it.
func webpExtended(w, h int, c color.NRGBA, flags byte, before, after [][]byte) []byte {
	vp8x := []byte{flags, 0, 0, 0}
	vp8x = append(vp8x, byte(w-1), byte((w-1)>>8), byte((w-1)>>16), byte(h-1), byte((h-1)>>8), byte((h-1)>>16))
	body := []byte("WEBP")
	body = append(body, riffChunk("VP8X", vp8x)...)
	for _, ch := range before {
		body = append(body, ch...)
	}
	body = append(body, webpSolid(w, h, c)[12:]...) // its VP8L chunk
	for _, ch := range after {
		body = append(body, ch...)
	}
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...), body...)
}

// TestProcessReencodesAWebPWithMetadata: a WebP's EXIF and XMP cannot be cut
// losslessly (VP8X's flags and the RIFF size go with them), so a WebP with
// either — or with the flag promising one, a chunk the walk does not know, or
// a trailer — is re-encoded as PNG: the same pixels, none of the metadata. A
// plain WebP, extended format and ICC profile included, passes through
// untouched.
func TestProcessReencodesAWebPWithMetadata(t *testing.T) {
	c := color.NRGBA{0x20, 0x80, 0xc0, 0xff}
	exif := riffChunk("EXIF", append([]byte("GPS:"), exifTIFF(1)...))
	xmp := riffChunk("XMP ", []byte("<x:xmpmeta>Jane's desk</x:xmpmeta>"))
	icc := riffChunk("ICCP", bytes.Repeat([]byte{7}, 41))
	for _, tc := range []struct {
		name string
		src  []byte
		pass bool
	}{
		{"simple format", webpSolid(16, 24, c), true},
		{"extended, no metadata", webpExtended(16, 24, c, 0, nil, nil), true},
		{"extended, an ICC profile", webpExtended(16, 24, c, 1<<5, [][]byte{icc}, nil), true},
		{"an EXIF chunk and its flag", webpExtended(16, 24, c, webpFlagEXIF, nil, [][]byte{exif}), false},
		{"an XMP chunk and its flag", webpExtended(16, 24, c, webpFlagXMP, nil, [][]byte{xmp}), false},
		{"both", webpExtended(16, 24, c, webpFlagEXIF|webpFlagXMP, [][]byte{icc}, [][]byte{exif, xmp}), false},
		{"the EXIF flag alone", webpExtended(16, 24, c, webpFlagEXIF, nil, nil), false},
		{"an EXIF chunk without its flag", webpExtended(16, 24, c, 0, nil, [][]byte{exif}), false},
		{"a chunk the walk does not know", webpExtended(16, 24, c, 0, nil, [][]byte{riffChunk("ZZZZ", []byte("x"))}), false},
		{"a trailer after the RIFF", append(webpSolid(16, 24, c), "trailer"...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if px := decoded(t, tc.src).At(8, 12); !near(px, c, 0) {
				t.Fatalf("fixture: pixel %v, want %v", px, c)
			}
			got, err := Process(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if got.Width != 16 || got.Height != 24 || got.Downscaled() {
				t.Fatalf("got %d×%d from %d×%d", got.Width, got.Height, got.OrigWidth, got.OrigHeight)
			}
			if tc.pass {
				if got.MIME != MIMEWebP || !bytes.Equal(got.Data, tc.src) {
					t.Fatalf("a plain WebP: got %s, %d bytes of %d", got.MIME, len(got.Data), len(tc.src))
				}
				return
			}
			if got.MIME != MIMEPNG {
				t.Fatalf("got %s, want the re-encode's PNG", got.MIME)
			}
			for _, leak := range []string{"GPS:", "Jane's desk", "trailer", "ZZZZ"} {
				if bytes.Contains(got.Data, []byte(leak)) {
					t.Fatalf("the re-encode carries %q", leak)
				}
			}
			if px := decoded(t, got.Data).At(8, 12); !near(px, c, 0) {
				t.Fatalf("pixel %v, want %v", px, c)
			}
		})
	}
}

// TestWebPPlainIsTotal: the WebP check refuses, without reading past the end,
// whatever is cut short or lies about a length.
func TestWebPPlainIsTotal(t *testing.T) {
	good := webpExtended(16, 24, color.NRGBA{1, 2, 3, 255}, 0, nil, nil)
	if !webpPlain(good) {
		t.Fatal("fixture: the plain extended WebP is not plain")
	}
	for i := range len(good) {
		if webpPlain(good[:i]) {
			t.Fatalf("a WebP cut to %d bytes of %d is plain", i, len(good))
		}
	}
	huge := append([]byte{}, good...)
	binary.LittleEndian.PutUint32(huge[16:], 0xffffffff) // VP8X's length
	if webpPlain(huge) {
		t.Fatal("a chunk length past the end is plain")
	}
	empty := append([]byte{}, good[:12]...)
	binary.LittleEndian.PutUint32(empty[4:], 4)
	empty = append(empty, riffChunk("VP8X", nil)...)
	binary.LittleEndian.PutUint32(empty[4:], uint32(len(empty)-8))
	if webpPlain(empty) {
		t.Fatal("an empty VP8X is plain")
	}
}

// FuzzPNGMeta: the walk is total over anything a paste can hold. It never
// panics; when it answers ok the result is no longer than the input, still
// starts with the signature, and walks again with nothing left to cut.
//
// And it cuts every metadata chunk, wherever one sits. A chunk of a
// fuzz-chosen kind (metadataChunks) with the fuzzed payload is planted at a
// fuzz-chosen place in a fixture (pngPlantable: after IHDR, before IDAT, after
// it, before IEND), Go's decoder confirms the planted file decodes to the
// fixture's pixels, and the walk must give the fixture back, byte for byte.
func FuzzPNGMeta(f *testing.F) {
	fixtures := pngMetaFixtures(f)
	names := []string{"one IDAT", "many IDATs", "paletted"}
	good := fixtures["one IDAT"]
	f.Add(good, uint16(0), []byte("x"), uint8(0))
	f.Add(afterIHDR(good, pngChunk("tEXt", []byte("a\x00b"))), uint16(1), []byte{}, uint8(1))
	f.Add(append(beforeIEND(good, pngChunk("eXIf", exifTIFF(6))), "trailer"...), uint16(2), []byte("XML:com.adobe.xmp"), uint8(3))
	f.Add([]byte(pngSignature+"\x00\x00\x00\x00IEND"), uint16(3), []byte{}, uint8(4))
	f.Add([]byte{}, uint16(0), []byte{}, uint8(0))
	for k, name := range names {
		for j := range pngPlantable(fixtures[name]) {
			// at picks the fixture (at % 3) and the place ((at / 3) % count);
			// kind picks what is planted there.
			f.Add([]byte{}, uint16(j*len(names)+k), []byte("private"), uint8(j+k))
		}
	}
	f.Fuzz(func(t *testing.T, b []byte, at uint16, payload []byte, kind uint8) {
		if s, ok := pngMeta(b); ok {
			if len(s) > len(b) || !bytes.HasPrefix(s, []byte(pngSignature)) {
				t.Fatalf("the cut is %d bytes from %d", len(s), len(b))
			}
			if again, ok := pngMeta(s); !ok || !bytes.Equal(again, s) {
				t.Fatal("the cut left a metadata chunk, or a file that no longer walks")
			}
		}

		base := fixtures[names[int(at)%len(names)]]
		places := pngPlantable(base)
		off := places[(int(at)/len(names))%len(places)]
		typ := metadataChunks[int(kind)%len(metadataChunks)]
		if len(payload) > 1<<16 {
			return
		}
		planted := insertAt(base, off, pngChunk(typ, payload))
		if !samePixels(planted, base) {
			t.Fatalf("a %s chunk at %d does not decode to the fixture's pixels", typ, off)
		}
		if s, ok := pngMeta(planted); !ok || !bytes.Equal(s, base) {
			t.Fatalf("a %s chunk at %d of a %d-byte fixture: pngMeta = (%d bytes, %v), want the fixture back", typ, off, len(base), len(s), ok)
		}
	})
}
