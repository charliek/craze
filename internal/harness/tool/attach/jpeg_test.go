package attach

import (
	"bytes"
	"encoding/base64"
	"image"
	"testing"
)

// TestJPEGMetaRefusesWhatItCannotWalk: a header jpegMeta cannot walk to its
// scan is not ok, so Process re-encodes it instead of guessing at a cut.
func TestJPEGMetaRefusesWhatItCannotWalk(t *testing.T) {
	good := encodeJPEG(t, gradient(16, 16), 90)
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"no SOI", good[2:]},
		{"SOI only", good[:2]},
		{"a segment running off the end", append(append([]byte{}, good[:2]...), 0xff, markerAPP1, 0xff, 0xff, 'E')},
		{"a length under 2", append(append([]byte{}, good[:2]...), 0xff, markerAPP1, 0, 1)},
		{"EOI before any scan", append(append([]byte{}, good[:2]...), 0xff, markerEOI)},
		{"junk where a marker belongs", append(append([]byte{}, good[:2]...), 0x00, 0x01)},
		{"fill bytes to the end", append(append([]byte{}, good[:2]...), 0xff, 0xff, 0xff)},
	} {
		if o, s, ok := jpegMeta(tc.b); ok || s != nil || o != 1 {
			t.Errorf("%s: jpegMeta = (%d, %d bytes, %v), want not ok", tc.name, o, len(s), ok)
		}
	}
	// Fill bytes before a marker, and a standalone marker, are walked over.
	withFill := afterSOI(good, []byte{0xff, 0xff, markerTEM}, append([]byte{0xff}, exifAPP1(6, true)...))
	o, s, ok := jpegMeta(withFill)
	if !ok || o != 6 || !bytes.Equal(s, afterSOI(good, []byte{0xff, 0xff, markerTEM})) {
		t.Fatalf("jpegMeta = (%d, %d bytes, %v), want orientation 6 and the APP1 (with its fill byte) cut", o, len(s), ok)
	}
}

// TestJPEGMetaFirstEXIFWins: of two EXIF segments, the first one's
// orientation is read, and both are cut.
func TestJPEGMetaFirstEXIFWins(t *testing.T) {
	good := encodeJPEG(t, gradient(16, 16), 90)
	o, s, ok := jpegMeta(afterSOI(good, exifAPP1(3, false), exifAPP1(8, false)))
	if !ok || o != 3 || !bytes.Equal(s, good) {
		t.Fatalf("jpegMeta = (%d, %d bytes, %v)", o, len(s), ok)
	}
}

// Multi-scan fixtures, made once with Pillow 12 (libjpeg), because Go's
// encoder writes only baseline single-scan files (plan 033 C3r, r1 #9):
// prog16 is a 16×16 progressive file of ten scans; base24rst is 24×24 noise,
// baseline, with a restart marker after every MCU and stuffed zeros in its
// data; prog24 is the same noise progressive (ten scans, stuffed zeros). A
// progressive file with restart markers is not here: Go's decoder refuses
// those ("missing 0xff00 sequence"), so Process never passes one through.
const (
	prog16B64 = "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAMCAgMCAgMDAwMEAwMEBQgFBQQEBQoHBwYIDAoMDAsKCwsNDhIQDQ4RDgsLEBYQERMU" +
		"FRUVDA8XGBYUGBIUFRT/2wBDAQMEBAUEBQkFBQkUDQsNFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQU" +
		"FBQUFBQUFBT/wgARCAAQABADASIAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAABgf/xAAVAQEBAAAAAAAAAAAAAAAAAAABA//a" +
		"AAwDAQACEAMQAAABlTNizqf/xAAWEAADAAAAAAAAAAAAAAAAAAAABAX/2gAIAQEAAQUCVlCkoUlCso//xAAYEQACAwAAAAAAAAAA" +
		"AAAAAAAABQYhMf/aAAgBAwEBPwGNvss//8QAFxEAAwEAAAAAAAAAAAAAAAAAAAQFYf/aAAgBAgEBPwFyxp//xAAVEAEBAAAAAAAA" +
		"AAAAAAAAAAAAMf/aAAgBAQAGPwKIiP/EABUQAQEAAAAAAAAAAAAAAAAAAAAx/9oACAEBAAE/IZsWLN//2gAMAwEAAgADAAAAEAv/" +
		"xAAVEQEBAAAAAAAAAAAAAAAAAAAAof/aAAgBAwEBPxCe/8QAFREBAQAAAAAAAAAAAAAAAAAAADH/2gAIAQIBAT8Qsf/EABUQAQEA" +
		"AAAAAAAAAAAAAAAAAADx/9oACAEBAAE/EJSEhJT/2Q=="
	base24rstB64 = "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAIBAQEBAQIBAQECAgICAgQDAgICAgUEBAMEBgUGBgYFBgYGBwkIBgcJBwYGCAsICQoK" +
		"CgoKBggLDAsKDAkKCgr/2wBDAQICAgICAgUDAwUKBwYHCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoK" +
		"CgoKCgoKCgr/wAARCAAYABgDASIAAhEBAxEB/8QAHwAAAQUBAQEBAQEAAAAAAAAAAAECAwQFBgcICQoL/8QAtRAAAgEDAwIEAwUF" +
		"BAQAAAF9AQIDAAQRBRIhMUEGE1FhByJxFDKBkaEII0KxwRVS0fAkM2JyggkKFhcYGRolJicoKSo0NTY3ODk6Q0RFRkdISUpTVFVW" +
		"V1hZWmNkZWZnaGlqc3R1dnd4eXqDhIWGh4iJipKTlJWWl5iZmqKjpKWmp6ipqrKztLW2t7i5usLDxMXGx8jJytLT1NXW19jZ2uHi" +
		"4+Tl5ufo6erx8vP09fb3+Pn6/8QAHwEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoL/8QAtREAAgECBAQDBAcFBAQAAQJ3AAEC" +
		"AxEEBSExBhJBUQdhcRMiMoEIFEKRobHBCSMzUvAVYnLRChYkNOEl8RcYGRomJygpKjU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVm" +
		"Z2hpanN0dXZ3eHl6goOEhYaHiImKkpOUlZaXmJmaoqOkpaanqKmqsrO0tba3uLm6wsPExcbHyMnK0tPU1dbX2Nna4uPk5ebn6Onq" +
		"8vP09fb3+Pn6/90ABAAB/9oADAMBAAIRAxEAPwDkprnRtY1HT3+CHw1F3LPa3f8AbOoar4Kt7x4NSkurbWBdTppaQ2d/p8do6ApM" +
		"m0C6nZpEY27DqF1G1SPUbD4WXt3p3iO707WdK8FaXp+hS26a5YXmpQQRRS/2Rd/ubKeC0utzSSQsstmjR+Xbx2yzdVq2lXHxe1y9" +
		"8PaDq/h281LUItAu9S1/xxoca6fqc8UwjEADXZjcwiCKG5tklkjjN1PC/wBtUQyHzr4oyeJfEset+OPEPxi16+uPEHgvUrvw/pmk" +
		"eI1fctxpUkGo3GqWts4uM20C6W3ksiTROWZ4fIN4Lfpx8sPjKuFqTlHkSpxV1Vn7VqM37iVGE2puXKuePt6EeZuEqcnOn1ZJ+8oS" +
		"qYOynQmnaNpSpuyjFSST9o5OXtZKUYSh7KbSkuSJ/9DI8IaTrvxc8B+G7r4hiDw+viDT5bqKwle40J7qyltJY7W4gvIZ3S3d49Vt" +
		"LCFntplWB4beOGPD2jlUtK+Id18ZPE3hL4rfCz4haPLp15ft9v8AEPjJJo5p9R0rULa5jaa4l8stEJbZFjhMsUXlX9qjCaWO4CFf" +
		"K8d5jxRUxNCtRxuEk5KbdPFwlTlTbqS0glTpuzd09GlypR5UvZw+zyfD5nWxNepR9reTjtiI01yxvThpOrTk3+7acpK8koy0Uk3/" +
		"AP/R5/R/Cn7LPxQ+KWt6Z4+0b7Dqj6xdJ8PtLurjWrrTLl7XUm1K/wBNgitZpHuZIrO4srye0URQm9klWQH5Z67ddJ+F/gbwna+O" +
		"J/CXi/VdT1u51DTdXubzwzfPNoutyW1vtjS0uHjtr68RI4bSNLSVY2u7oOsyRliSiuyGWUsv4oq4WnOTpVfq0pRcrq1W9KcLv3uR" +
		"8sanK206qUndLlOnMMLSznHqjX05/q0W1o7VKeIlJJu9vgs0tJc0nPnbTX//0ovjB4EuPCerS/DTXLI+HJfHWnX2mXF34f1K7t1j" +
		"kitbSzOn6hHbWs6tA8DRoJbq5V7hMNJPseNJiiivk8R4m5/wdw5l1fBQpynio1J1HP2kryhWnTTVqi+zFX3u9TzeGeO8zp5Uqnsa" +
		"Tbk07qdvdsrqPPaLf2uVJN7JH//Z"
	prog24B64 = "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAIBAQEBAQIBAQECAgICAgQDAgICAgUEBAMEBgUGBgYFBgYGBwkIBgcJBwYGCAsICQoK" +
		"CgoKBggLDAsKDAkKCgr/2wBDAQICAgICAgUDAwUKBwYHCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoKCgoK" +
		"CgoKCgoKCgr/wgARCAAYABgDASIAAhEBAxEB/8QAGQABAQEAAwAAAAAAAAAAAAAAAAUDAgQG/8QAFgEBAQEAAAAAAAAAAAAAAAAA" +
		"BQME/9oADAMBAAIQAxAAAAGRUq+dpXRiGU5WxsrogPF//8QAGxAAAwEBAQEBAAAAAAAAAAAAAwQFAgETBhH/2gAIAQEAAQUC7oJi" +
		"emfxQR66BRarnqdZZ4KhqyyFX5apU8paKlhHShf/xAAnEQABAgUDAgcAAAAAAAAAAAABAhEDBBIhMQBBYQUiE0JDUoGSsf/aAAgB" +
		"AwEBPwGS7kFUHKDtcp2D+53qLgEUnNhqThzK4i1Iqu3qBNh2jKkny5ObHfUxCTOR6F7+GPsmIT+fLl310zrsymVqoTnnbh7ctr//" +
		"xAAmEQABAwIFAwUAAAAAAAAAAAABAgMRISIABAUSUgYxURMyQmHw/9oACAECAQE/AXy28ppRIi0fI7qHtYDWYqN6BNCkynXcxqin" +
		"ELQ80Zm10FJTce1qfwpHtAyycvqimkk7VemSJ5WkeYoFRyr9Yc6mz+j6dl1shJLoUVTuNQsp5eBj/8QAJRAAAgEDAwMFAQAAAAAA" +
		"AAAAAQIDBBEhBRITACIxFBUyUVJB/9oACAEBAAY/Ao/ZNN3Eq3NJLRK9pCyzbjxWR4wv3+jn49SJpbtHUNHNFRRRwFedHkAAPE2E" +
		"IVvJGUxZQt6ZtQtT+ojLBDug3IVIVg4PbiVUHacWUAfHp6eCWneSQQNJPXQDjkINrfK2LAMtyBuI78HqauqNYndqiikanjiqP1ER" +
		"I0qr3do4seR9W37aTVdL1CExu/fUVlwTJFIrC7G2LrgXAs6+SG6mjr4dkvM3t8TNM0bbZOR4wFJ3EIyOVwN5N/vpa40lXLJM0kcr" +
		"PTPeGYquNrWV3wFG023N5t0dNnT05ro3jZqeRlsQqpxyBVOLWyzd39Pi/wD/xAAZEAEBAQEBAQAAAAAAAAAAAAABESEAQTH/2gAI" +
		"AQEAAT8hYN7gTTDSYZuhrgQ1gD1cd6kBgOzDkUjJWJToj2TJHTx61tmlDx0AnG7REXwqwar2LXDK2BsQGIbkVvQpc77ADxHjoxVm" +
		"c4DBAxK4a6JStaDBrYiRsUd//9oADAMBAAIAAwAAABDrF3z/xAAaEQEBAQEBAQEAAAAAAAAAAAABESExAEFR/9oACAEDAQE/EOUB" +
		"2QiAAGlFS4D1pZXANMXHQhQYAUPjuTGABbPlDKLog2lUNJ4KYp/AF4Hv/8QAGREBAQEBAQEAAAAAAAAAAAAAAREhADFB/9oACAEC" +
		"AQE/EJEkCl4uRY4e2LHTmYNWDgRItHEIIHLyV4ol3vroKJFQ4v8AItgpIPhfa73/xAAXEAEBAQEAAAAAAAAAAAAAAAABEQAh/9oA" +
		"CAEBAAE/EBrTtmIaTsoUQZX/AOan94BQQoZC9vE7cxg3EaHQcCKod4iyS1AXRY6uDdKM7imtHYQvE+hNiNJg4WNLMUcwR/MSjUy4" +
		"mSVGuqQoFOZVx1//2Q=="
)

// multiScanFixtures is every JPEG fixture the walk is tested on: Go's own
// baseline file and the three above.
func multiScanFixtures(t testing.TB) map[string][]byte {
	t.Helper()
	out := map[string][]byte{"go baseline": encodeJPEG(t, gradient(16, 16), 90)}
	for name, b64 := range map[string]string{"prog16": prog16B64, "base24rst": base24rstB64, "prog24": prog24B64} {
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = b
	}
	return out
}

// sosOffsets is where each SOS marker of a fixture starts: every 0xff 0xda,
// a pair that occurs nowhere else in these fixtures (stuffing keeps it out of
// entropy data, and their tables hold none). The tests that plant a segment
// there check that the planted file still decodes to the same pixels.
func sosOffsets(b []byte) []int {
	var out []int
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 0xff && b[i+1] == markerSOS {
			out = append(out, i)
		}
	}
	return out
}

// insertAt is b with seg inserted at offset at.
func insertAt(b []byte, at int, seg []byte) []byte {
	out := append([]byte{}, b[:at]...)
	out = append(out, seg...)
	return append(out, b[at:]...)
}

// samePixels reports whether a and b decode, and to the same pixels.
func samePixels(a, b []byte) bool {
	ia, _, err := image.Decode(bytes.NewReader(a))
	if err != nil {
		return false
	}
	ib, _, err := image.Decode(bytes.NewReader(b))
	if err != nil || ia.Bounds() != ib.Bounds() {
		return false
	}
	r := ia.Bounds()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if ia.At(x, y) != ib.At(x, y) {
				return false
			}
		}
	}
	return true
}

// TestJPEGMetaWalksEveryScan: every fixture — progressive scans, restart
// markers, stuffed zeros — walks to its EOI with nothing to cut, and comes
// back as itself.
func TestJPEGMetaWalksEveryScan(t *testing.T) {
	for name, b := range multiScanFixtures(t) {
		if _, _, err := image.Decode(bytes.NewReader(b)); err != nil {
			t.Fatalf("%s does not decode: %v", name, err)
		}
		o, s, ok := jpegMeta(b)
		if !ok || o != 1 || !bytes.Equal(s, b) {
			t.Errorf("%s: jpegMeta = (%d, %d of %d bytes, %v), want it back whole", name, o, len(s), len(b), ok)
		}
	}
}

// TestJPEGMetaCutsAPP1AfterTheFirstScan is r1 #9: an APP1 the decoder takes
// in its stride — before EOI, between a progressive file's scans, after fill
// bytes — is cut like one up front, and what follows EOI is dropped. Each
// planted file still decodes to the same pixels, which is what makes it a
// file a camera or an editor could hand over.
func TestJPEGMetaCutsAPP1AfterTheFirstScan(t *testing.T) {
	xmp := xmpAPP1(32)
	for name, b := range multiScanFixtures(t) {
		eoi := len(b) - 2
		cases := map[string][]byte{
			"before EOI":                insertAt(b, eoi, xmp),
			"before EOI, after fill":    insertAt(b, eoi, append([]byte{0xff, 0xff}, xmp...)),
			"before EOI and up front":   afterSOI(insertAt(b, eoi, xmp), exifAPP1(1, false)),
			"a trailer after EOI":       append(append([]byte{}, b...), []byte("motion-photo video")...),
			"an APP1 and a trailer too": append(insertAt(b, eoi, xmp), 0xff, 0xd8, 0xff, 0xe1, 0, 4, 'x', 'y'),
		}
		if sos := sosOffsets(b); len(sos) > 1 {
			cases["between scans"] = insertAt(b, sos[1], xmp)
			cases["before the last scan"] = insertAt(b, sos[len(sos)-1], xmp)
		}
		for what, planted := range cases {
			if !samePixels(planted, b) {
				t.Fatalf("%s, %s: the planted file does not decode to the fixture's pixels", name, what)
			}
			o, s, ok := jpegMeta(planted)
			if !ok || o != 1 || !bytes.Equal(s, b) {
				t.Errorf("%s, %s: jpegMeta = (%d, %d bytes, %v), want the fixture's %d bytes", name, what, o, len(s), ok, len(b))
			}
		}
	}
}

// TestProcessDropsAPP1AfterTheScan: the pass-through Process keeps for a
// small JPEG carries no APP1 from anywhere in the file.
func TestProcessDropsAPP1AfterTheScan(t *testing.T) {
	b := multiScanFixtures(t)["prog24"]
	sos := sosOffsets(b)
	planted := insertAt(insertAt(b, len(b)-2, xmpAPP1(8)), sos[3], xmpAPP1(8))
	img, err := Process(planted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(img.Data, b) || bytes.Contains(img.Data, []byte("ns.adobe.com/xap")) {
		t.Fatalf("Process kept %d bytes of %d (fixture %d)", len(img.Data), len(planted), len(b))
	}
}

// markerOffsets is where each marker of a known-good fixture starts, after
// SOI: every 0xff followed by a marker byte (0xc0 and up, but not a restart
// marker and not another 0xff), which in these fixtures occurs nowhere else
// (stuffing keeps such pairs out of entropy data, and their tables hold none).
// It is the fuzz property's own list, not the walk's.
func markerOffsets(b []byte) []int {
	var out []int
	for i := 2; i+1 < len(b); i++ {
		if m := b[i+1]; b[i] == 0xff && m >= 0xc0 && m != 0xff && (m < markerRST0 || m > markerRST7) {
			out = append(out, i)
		}
	}
	return out
}

// FuzzJPEGMeta: the walk is total over anything a paste can hold. It never
// panics, the orientation is always one of the eight, and when it answers ok
// the result is no longer than the input, still starts with SOI, and walks
// again with nothing left to cut.
//
// And it cuts every APP1, wherever one sits (C3r, r1 #9). That half does not
// trust the walk to say where the markers are — a walk that stopped at the
// first scan would agree with itself: the second walk of its output stopped
// at the same place. An APP1 with the fuzzed payload is planted in front of a
// fuzz-chosen marker of a fixture (markerOffsets: before a table, an SOS
// between a progressive file's scans, EOI), Go's decoder confirms the planted
// file decodes to the fixture's pixels, and the walk must give the fixture
// back, byte for byte.
func FuzzJPEGMeta(f *testing.F) {
	fixtures := multiScanFixtures(f)
	names := []string{"go baseline", "prog16", "base24rst", "prog24"}
	good := fixtures["go baseline"]
	f.Add(good, uint16(0), []byte("x"))
	f.Add(afterSOI(good, exifAPP1(6, false), xmpAPP1(8)), uint16(5), []byte{})
	f.Add(afterSOI(good, exifAPP1(8, true)), uint16(6), []byte("http://ns.adobe.com/xap/1.0/"))
	f.Add([]byte{0xff, markerSOI, 0xff, markerAPP1, 0x00, 0x08, 'E', 'x', 'i', 'f', 0, 0}, uint16(1), []byte{0xff, markerAPP1, 0, 8})
	f.Add([]byte{}, uint16(0), []byte{})
	for k, name := range names {
		for j := range markerOffsets(fixtures[name]) {
			// at picks the fixture (at % 4) and its marker ((at / 4) % count).
			f.Add([]byte{}, uint16(j*len(names)+k), []byte("private"))
		}
	}
	f.Fuzz(func(t *testing.T, b []byte, at uint16, payload []byte) {
		o, s, ok := jpegMeta(b)
		if o < 1 || o > 8 {
			t.Fatalf("orientation %d", o)
		}
		if ok {
			if len(s) > len(b) || !bytes.HasPrefix(s, []byte{0xff, markerSOI}) {
				t.Fatalf("the cut is %d bytes from %d", len(s), len(b))
			}
			if _, again, ok := jpegMeta(s); !ok || !bytes.Equal(again, s) {
				t.Fatal("the cut left an APP1, or a file that no longer walks")
			}
		}

		base := fixtures[names[int(at)%len(names)]]
		markers := markerOffsets(base)
		off := markers[(int(at)/len(names))%len(markers)]
		if len(payload) > 0xfff0 {
			return
		}
		planted := insertAt(base, off, segment(markerAPP1, payload))
		if !samePixels(planted, base) {
			t.Fatalf("an APP1 before the marker at %d does not decode to the fixture's pixels", off)
		}
		if o, s, ok := jpegMeta(planted); !ok || o != 1 || !bytes.Equal(s, base) {
			t.Fatalf("an APP1 before the marker at %d of a %d-byte fixture: jpegMeta = (%d, %d bytes, %v), want the fixture back", off, len(base), o, len(s), ok)
		}
	})
}
