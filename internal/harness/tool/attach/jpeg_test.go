package attach

import (
	"bytes"
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

// FuzzJPEGMeta: the walk is total over anything a paste can hold. It never
// panics, the orientation is always one of the eight, and when it answers ok
// the result is no longer than the input, still starts with SOI, and walks
// again with nothing left to cut.
func FuzzJPEGMeta(f *testing.F) {
	good := encodeJPEG(f, gradient(16, 16), 90)
	f.Add(good)
	f.Add(afterSOI(good, exifAPP1(6, false), xmpAPP1(8)))
	f.Add(afterSOI(good, exifAPP1(8, true)))
	f.Add([]byte{0xff, markerSOI, 0xff, markerAPP1, 0x00, 0x08, 'E', 'x', 'i', 'f', 0, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		o, s, ok := jpegMeta(b)
		if o < 1 || o > 8 {
			t.Fatalf("orientation %d", o)
		}
		if !ok {
			return
		}
		if len(s) > len(b) || !bytes.HasPrefix(s, []byte{0xff, markerSOI}) {
			t.Fatalf("the cut is %d bytes from %d", len(s), len(b))
		}
		if _, again, ok := jpegMeta(s); !ok || !bytes.Equal(again, s) {
			t.Fatal("the cut left an APP1, or a header that no longer walks")
		}
	})
}
