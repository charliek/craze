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
// bytes — is cut like one up front, and what follows EOI is dropped; IPTC and
// comments too (C6r, r2 #4a). Each
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
			"IPTC before EOI":           insertAt(b, eoi, iptcAPP13("a byline")),
			"a comment before EOI":      insertAt(b, eoi, segment(markerCOM, []byte("a comment"))),
		}
		if sos := sosOffsets(b); len(sos) > 1 {
			cases["between scans"] = insertAt(b, sos[1], xmp)
			cases["before the last scan"] = insertAt(b, sos[len(sos)-1], xmp)
			cases["a comment and IPTC between scans"] = insertAt(insertAt(b, sos[1], segment(markerCOM, []byte("c"))), sos[1], iptcAPP13("x"))
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

// TestJPEGMetaKeepsWhatDrawsThePixels (C6r, r2 #4a): the segments that say
// how to read the pixels — APP0 (JFIF), APP2 (the ICC profile), APP14 (Adobe's
// colour transform) — stay, byte for byte and in place, beside the metadata
// cut around them.
func TestJPEGMetaKeepsWhatDrawsThePixels(t *testing.T) {
	plain := encodeJPEG(t, gradient(16, 16), 90)
	jfif := segment(markerAPP0, []byte("JFIF\x00\x01\x02\x00\x00\x01\x00\x01\x00\x00"))
	icc := iccAPP2()
	adobe := segment(markerAPP14, []byte("Adobe\x00\x64\x00\x00\x00\x00\x01"))
	kept := afterSOI(plain, jfif, icc, adobe)
	planted := afterSOI(plain, jfif, segment(markerCOM, []byte("a note")), icc, iptcAPP13("a place"), adobe, exifAPP1(1, false))
	o, s, ok := jpegMeta(planted)
	if !ok || o != 1 || !bytes.Equal(s, kept) {
		t.Fatalf("jpegMeta = (%d, %d bytes, %v), want the %d bytes with APP0, APP2 and APP14 kept", o, len(s), ok, len(kept))
	}
	if _, again, ok := jpegMeta(kept); !ok || !bytes.Equal(again, kept) {
		t.Fatal("a file of the kept segments alone was cut")
	}
	profile := []byte("ICC_PROFILE\x00\x01\x01")
	for m := byte(markerAPP0); m <= markerAPP15; m++ {
		for _, payload := range [][]byte{nil, profile, []byte("MPF\x00")} {
			want := m != markerAPP0 && m != markerAPP14 && (m != markerAPP2 || !bytes.Equal(payload, profile))
			if jpegMetadata(m, payload) != want {
				t.Errorf("jpegMetadata(APP%d, %q) = %v, want %v", m-markerAPP0, payload, !want, want)
			}
		}
	}
	if !jpegMetadata(markerCOM, nil) || jpegMetadata(markerSOS, nil) || jpegMetadata(0xdb, nil) || jpegMetadata(0xc0, nil) {
		t.Error("COM, or a table or frame marker, judged wrongly")
	}
}

// TestJPEGMetaCutsAnAPP2ThatIsNotAProfile is r4 #3 (plan 033 C6r2): APP2 is
// the ICC profile's marker, but not the profile's alone. A phone's MPF
// (Multi-Picture Format) segment — whose preview images are JPEGs with EXIF
// and GPS of their own — and a vendor's FlashPix data share it. Only an APP2
// whose payload starts ICC_PROFILE\0 stays, in place; any other is cut,
// wherever it sits, an empty one and a header cut short included. Each
// planted file decodes to the same pixels, and Process passes the file
// through without the segment.
func TestJPEGMetaCutsAnAPP2ThatIsNotAProfile(t *testing.T) {
	plain := encodeJPEG(t, gradient(16, 16), 90)
	icc := iccAPP2()
	kept := afterSOI(plain, icc)
	gps := []byte("GPS 37.7749 N 122.4194 W")
	preview := afterSOI(encodeJPEG(t, gradient(8, 8), 50), segment(markerAPP1, append([]byte(exifHeader), gps...)))
	mpf := mpfAPP2(preview)
	for what, planted := range map[string][]byte{
		"MPF after the profile":      afterSOI(plain, icc, mpf),
		"MPF before the profile":     afterSOI(plain, mpf, icc),
		"MPF before EOI":             insertAt(kept, len(kept)-2, mpf),
		"a vendor's FlashPix APP2":   afterSOI(plain, icc, segment(markerAPP2, []byte("FPXR\x00\x00\x01private"))),
		"an empty APP2":              afterSOI(plain, icc, segment(markerAPP2, nil)),
		"a profile header cut short": afterSOI(plain, icc, segment(markerAPP2, []byte("ICC_PROFILE"))),
	} {
		if !samePixels(planted, plain) {
			t.Fatalf("%s: the planted file does not decode to the fixture's pixels", what)
		}
		o, s, ok := jpegMeta(planted)
		if !ok || o != 1 || !bytes.Equal(s, kept) {
			t.Errorf("%s: jpegMeta = (%d, %d bytes, %v), want the %d bytes with the profile alone kept", what, o, len(s), ok, len(kept))
		}
	}
	img, err := Process(afterSOI(plain, icc, mpf))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(img.Data, kept) || bytes.Contains(img.Data, []byte("MPF\x00")) || bytes.Contains(img.Data, gps) {
		t.Fatalf("Process kept %d bytes, want the %d with the profile alone", len(img.Data), len(kept))
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

// metadataMarkers is every segment marker jpegMeta cuts (jpegMetadata), as the
// fuzz plants them: APP1, APP3 to APP13, APP15 and COM, and APP2, which is cut
// unless its payload is an ICC profile's (iccPayload; C6r2, r4 #3).
var metadataMarkers = []byte{
	markerAPP1, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xeb, 0xec,
	markerAPP13, markerAPP15, markerCOM, markerAPP2,
}

// iccPayload reports whether an APP2 payload is an ICC profile's, the one
// APP2 the walk keeps: the fuzz property's own words for it, not the walk's.
func iccPayload(payload []byte) bool {
	return bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00"))
}

// FuzzJPEGMeta: the walk is total over anything a paste can hold. It never
// panics, the orientation is always one of the eight, and when it answers ok
// the result is no longer than the input, still starts with SOI, and walks
// again with nothing left to cut.
//
// And it cuts every metadata segment, wherever one sits (C3r, r1 #9; C6r, r2
// #4a). That half does not trust the walk to say where the markers are — a
// walk that stopped at the first scan would agree with itself: the second walk
// of its output stopped at the same place. A segment of a fuzz-chosen metadata
// kind (APP1, APP13, COM, a vendor's APPn, an APP2: metadataMarkers) with the
// fuzzed payload is planted in front of a fuzz-chosen marker of a fixture
// (markerOffsets: before a table, an SOS between a progressive file's scans,
// EOI), Go's decoder confirms the planted file decodes to the fixture's
// pixels, and the walk must give the fixture back, byte for byte — or, for an
// APP2 whose payload is an ICC profile's, the planted file as it is (C6r2).
func FuzzJPEGMeta(f *testing.F) {
	fixtures := multiScanFixtures(f)
	names := []string{"go baseline", "prog16", "base24rst", "prog24"}
	good := fixtures["go baseline"]
	f.Add(good, uint16(0), []byte("x"), uint8(0))
	f.Add(afterSOI(good, exifAPP1(6, false), xmpAPP1(8)), uint16(5), []byte{}, uint8(1))
	f.Add(afterSOI(good, exifAPP1(8, true)), uint16(6), []byte("http://ns.adobe.com/xap/1.0/"), uint8(2))
	f.Add([]byte{0xff, markerSOI, 0xff, markerAPP1, 0x00, 0x08, 'E', 'x', 'i', 'f', 0, 0}, uint16(1), []byte{0xff, markerAPP1, 0, 8}, uint8(3))
	f.Add(afterSOI(good, iptcAPP13("a byline"), segment(markerCOM, []byte("a comment"))), uint16(2), []byte("Photoshop 3.0"), uint8(1))
	f.Add([]byte{}, uint16(0), []byte{}, uint8(0))
	// An MPF APP2 and an ICC one, planted (kind 14 is APP2).
	f.Add([]byte{}, uint16(1), []byte("MPF\x00MM\x00*\x00\x00\x00\x08"), uint8(14))
	f.Add([]byte{}, uint16(6), []byte("ICC_PROFILE\x00\x01\x01"), uint8(14))
	f.Add(afterSOI(good, mpfAPP2([]byte("preview")), iccAPP2()), uint16(2), []byte("FPXR\x00"), uint8(14))
	for k, name := range names {
		for j := range markerOffsets(fixtures[name]) {
			// at picks the fixture (at % 4) and its marker ((at / 4) % count);
			// kind picks what is planted there (kind % len(metadataMarkers)).
			f.Add([]byte{}, uint16(j*len(names)+k), []byte("private"), uint8(j+k))
		}
	}
	f.Fuzz(func(t *testing.T, b []byte, at uint16, payload []byte, kind uint8) {
		o, s, ok := jpegMeta(b)
		if o < 1 || o > 8 {
			t.Fatalf("orientation %d", o)
		}
		if ok {
			if len(s) > len(b) || !bytes.HasPrefix(s, []byte{0xff, markerSOI}) {
				t.Fatalf("the cut is %d bytes from %d", len(s), len(b))
			}
			if _, again, ok := jpegMeta(s); !ok || !bytes.Equal(again, s) {
				t.Fatal("the cut left a metadata segment, or a file that no longer walks")
			}
		}

		base := fixtures[names[int(at)%len(names)]]
		markers := markerOffsets(base)
		off := markers[(int(at)/len(names))%len(markers)]
		if len(payload) > 0xfff0 {
			return
		}
		marker := metadataMarkers[int(kind)%len(metadataMarkers)]
		planted := insertAt(base, off, segment(marker, payload))
		if !samePixels(planted, base) {
			t.Fatalf("a %#x segment before the marker at %d does not decode to the fixture's pixels", marker, off)
		}
		// A planted APP1 the fuzzer made an EXIF of may say an orientation.
		exif := marker == markerAPP1 && bytes.HasPrefix(payload, []byte(exifHeader))
		// A planted APP2 the fuzzer made an ICC profile of stays where it is.
		want := base
		if marker == markerAPP2 && iccPayload(payload) {
			want = planted
		}
		if o, s, ok := jpegMeta(planted); !ok || (o != 1 && !exif) || !bytes.Equal(s, want) {
			t.Fatalf("a %#x segment before the marker at %d of a %d-byte fixture: jpegMeta = (%d, %d bytes, %v), want the %d bytes back", marker, off, len(base), o, len(s), ok, len(want))
		}
	})
}
