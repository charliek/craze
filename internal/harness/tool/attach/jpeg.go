package attach

import (
	"bytes"
	"encoding/binary"
)

// JPEG markers jpegMeta reads (ITU T.81 Annex B).
const (
	markerSOI   = 0xd8
	markerEOI   = 0xd9
	markerSOS   = 0xda
	markerAPP0  = 0xe0
	markerAPP1  = 0xe1
	markerAPP2  = 0xe2
	markerAPP13 = 0xed
	markerAPP14 = 0xee
	markerAPP15 = 0xef
	markerCOM   = 0xfe
	markerTEM   = 0x01
	markerRST0  = 0xd0
	markerRST7  = 0xd7
)

// jpegMetadata reports whether a segment with this marker and payload (the
// bytes after its length) is metadata the walk cuts: a comment (COM), and
// every application segment but the three that say how to read the pixels —
// APP0 (JFIF), APP2 holding an ICC profile (iccProfileHeader) and APP14
// (Adobe's colour transform). The rest carry the user's, never the image's:
// APP1 EXIF and XMP (the camera, the time, GPS, a thumbnail of the uncropped
// original), every other APP2 — a phone's MPF (Multi-Picture Format: preview
// images, each a JPEG with EXIF and GPS of its own; plan 033 C6r2, r4 #3) and
// the vendors' payloads that share the marker — APP13 Photoshop/IPTC (a
// caption, a byline, a place), APP11's JUMBF (content credentials, a signer's
// name), the vendors' APP3 to APP12 and APP15 (C6r, r2 #4a, which named APP13
// and COM). No decoder draws a pixel from any of them.
func jpegMetadata(marker byte, payload []byte) bool {
	switch marker {
	case markerCOM:
		return true
	case markerAPP2:
		return !bytes.HasPrefix(payload, []byte(iccProfileHeader))
	case markerAPP0, markerAPP14:
		return false
	}
	return marker >= markerAPP0 && marker <= markerAPP15
}

// iccProfileHeader leads an APP2 segment that holds (a chunk of) an ICC
// profile (ICC.1, Annex B.4): the only APP2 the walk keeps.
const iccProfileHeader = "ICC_PROFILE\x00"

// exifHeader leads an APP1 segment that holds EXIF (XMP's APP1 leads with a
// namespace URI instead, and is cut all the same).
const exifHeader = "Exif\x00\x00"

// tagOrientation is EXIF's orientation tag in IFD0, and typeShort its type.
const (
	tagOrientation = 0x0112
	typeShort      = 3
)

// jpegMeta walks every marker of a JPEG — the header, each scan's
// entropy-coded data (stuffed zeros, restart markers and fill bytes stepped
// over), the tables and segments between a progressive file's scans, to EOI —
// and returns the EXIF orientation (1 when there is none, or it is unreadable
// or out of range) and the file with every metadata segment (jpegMetadata:
// APP1, APP13, COM, an APP2 that is not an ICC profile, and the other
// application segments but APP0 and APP14) cut out, wherever it sits, and nothing after its EOI. A file with no
// such segment and nothing past its EOI comes back as the same slice. ok is
// false when the file cannot be walked to an EOI after at least one scan: the
// caller then re-encodes rather than guess what a lossless cut of it would be
// (plan 033 C3r, r1 #9: an APP1 after the first scan used to be copied through
// with the scan data).
//
// None of the metadata is the image, all of it is the user's, and none of it
// is the model's business — and the decoder ignores those segments wherever
// they are, so a file can carry one between scans or before EOI as well as up
// front. What follows EOI is not the image either (a phone's motion-photo
// video, a vendor trailer): no decoder reads it. APP0 (JFIF), an APP2 holding
// the ICC profile and APP14 (Adobe's colour transform) stay: they say how to
// read the pixels. The orientation is read only from an EXIF APP1 ahead of the first
// scan, where EXIF belongs.
//
// It is total over arbitrary bytes: every read is bounds-checked, and the walk
// moves forward by at least one byte a step.
func jpegMeta(b []byte) (orientation int, stripped []byte, ok bool) {
	if len(b) < 2 || b[0] != 0xff || b[1] != markerSOI {
		return 1, nil, false
	}
	orientation = 1
	cut := cutter{b: b}
	foundOrientation, scanned := false, false
	i := 2
	for {
		start := i
		if i >= len(b) || b[i] != 0xff {
			return 1, nil, false
		}
		// Any number of 0xff fill bytes may precede a marker.
		for i < len(b) && b[i] == 0xff {
			i++
		}
		if i >= len(b) {
			return 1, nil, false
		}
		marker := b[i]
		i++
		switch {
		case marker == markerEOI:
			if !scanned {
				// An EOI before any scan is not a JPEG this walk understands.
				return 1, nil, false
			}
			return orientation, cut.end(i), true
		case marker == 0x00, marker == markerSOI:
			// A stuffed zero outside a scan is not a marker, and a second SOI
			// is not a JPEG this walk understands.
			return 1, nil, false
		case marker == markerTEM, marker >= markerRST0 && marker <= markerRST7:
			continue // standalone: no length follows
		}
		if i+2 > len(b) {
			return 1, nil, false
		}
		n := int(b[i])<<8 | int(b[i+1])
		if n < 2 || i+n > len(b) {
			return 1, nil, false
		}
		payload := b[i+2 : i+n]
		if jpegMetadata(marker, payload) {
			cut.cut(start, i+n)
		}
		if marker == markerAPP1 && !scanned && !foundOrientation && bytes.HasPrefix(payload, []byte(exifHeader)) {
			orientation, foundOrientation = exifOrientation(payload[len(exifHeader):]), true
		}
		i += n
		if marker == markerSOS {
			scanned = true
			i = skipEntropy(b, i)
		}
	}
}

// skipEntropy returns the index of the marker that ends the entropy-coded
// data starting at i — the first of its fill bytes, if it has any — or len(b)
// when the data runs off the end. Inside the data a 0xff is either a stuffed
// zero (0xff 0x00) or a restart marker (RST0–RST7), both part of the scan,
// possibly after fill bytes; any other marker ends it.
func skipEntropy(b []byte, i int) int {
	for i < len(b) {
		if b[i] != 0xff {
			i++
			continue
		}
		j := i + 1
		for j < len(b) && b[j] == 0xff {
			j++
		}
		if j >= len(b) {
			return len(b)
		}
		if m := b[j]; m == 0x00 || (m >= markerRST0 && m <= markerRST7) {
			i = j + 1
			continue
		}
		return i
	}
	return i
}

// exifOrientation reads the orientation tag from a TIFF structure (EXIF's
// body): the byte order, IFD0's offset, and IFD0's entries. Anything missing,
// malformed or out of range is 1, the identity.
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(tiff[:4]) {
	case "II*\x00":
		bo = binary.LittleEndian
	case "MM\x00*":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := uint64(bo.Uint32(tiff[4:8]))
	if off+2 > uint64(len(tiff)) {
		return 1
	}
	count := uint64(bo.Uint16(tiff[off:]))
	for k := range count {
		e := off + 2 + 12*k
		if e+12 > uint64(len(tiff)) {
			return 1
		}
		if bo.Uint16(tiff[e:]) != tagOrientation {
			continue
		}
		if bo.Uint16(tiff[e+2:]) != typeShort || bo.Uint32(tiff[e+4:]) < 1 {
			return 1
		}
		// A SHORT with a count of one sits left-justified in the 4-byte
		// value field, in the file's byte order.
		if v := int(bo.Uint16(tiff[e+8:])); v >= 1 && v <= 8 {
			return v
		}
		return 1
	}
	return 1
}
