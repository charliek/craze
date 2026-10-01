package attach

import (
	"bytes"
	"encoding/binary"
)

// JPEG markers jpegMeta reads (ITU T.81 Annex B).
const (
	markerSOI  = 0xd8
	markerEOI  = 0xd9
	markerSOS  = 0xda
	markerAPP1 = 0xe1
	markerTEM  = 0x01
	markerRST0 = 0xd0
	markerRST7 = 0xd7
)

// exifHeader leads an APP1 segment that holds EXIF (XMP's APP1 leads with a
// namespace URI instead, and is cut all the same).
const exifHeader = "Exif\x00\x00"

// tagOrientation is EXIF's orientation tag in IFD0, and typeShort its type.
const (
	tagOrientation = 0x0112
	typeShort      = 3
)

// jpegMeta walks a JPEG's header — every segment before the first SOS, after
// which the file is entropy-coded data copied as it is — and returns the EXIF
// orientation (1 when there is none, or it is unreadable or out of range) and
// the file with every APP1 segment cut out. A file with no APP1 comes back as
// the same slice. ok is false when the header cannot be walked to an SOS: the
// caller then re-encodes rather than guess what a lossless cut of it would be.
//
// APP1 carries EXIF and XMP: the camera, the time, GPS, a thumbnail of the
// uncropped original. None of it is the image, all of it is the user's, and
// none of it is the model's business. APP0 (JFIF), APP2 (the ICC profile) and
// APP14 (Adobe's colour transform) stay: they say how to read the pixels.
//
// It is total over arbitrary bytes: every read is bounds-checked, and the walk
// moves forward by at least one byte a step.
func jpegMeta(b []byte) (orientation int, stripped []byte, ok bool) {
	if len(b) < 2 || b[0] != 0xff || b[1] != markerSOI {
		return 1, nil, false
	}
	orientation = 1
	var cuts [][2]int
	foundOrientation := false
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
		case marker == 0x00, marker == markerSOI, marker == markerEOI:
			// A stuffed zero is not a marker, and a second SOI or an EOI
			// before any scan is not a JPEG this walk understands.
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
		if marker == markerSOS {
			break
		}
		if marker == markerAPP1 {
			cuts = append(cuts, [2]int{start, i + n})
			if payload := b[i+2 : i+n]; !foundOrientation && bytes.HasPrefix(payload, []byte(exifHeader)) {
				orientation, foundOrientation = exifOrientation(payload[len(exifHeader):]), true
			}
		}
		i += n
	}
	if len(cuts) == 0 {
		return orientation, b, true
	}
	out := make([]byte, 0, len(b))
	at := 0
	for _, c := range cuts {
		out = append(out, b[at:c[0]]...)
		at = c[1]
	}
	return orientation, append(out, b[at:]...), true
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
