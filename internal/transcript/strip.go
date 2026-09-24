package transcript

// stripANSI and its transition table are ansi.Strip from
// github.com/charmbracelet/x/ansi v0.10.1 (width.go, parser.go's utf8ByteLen,
// and parser/transition_table.go), transcribed so that sanitizeLine gives the
// TUI's exact answer without this package importing a terminal library (plan
// 024 §3.1's depguard rule). Only the names changed. Its license:
//
//	MIT License
//
//	Copyright (c) 2023 Charmbracelet, Inc.
//
//	Permission is hereby granted, free of charge, to any person obtaining a
//	copy of this software and associated documentation files (the
//	"Software"), to deal in the Software without restriction, including
//	without limitation the rights to use, copy, modify, merge, publish,
//	distribute, sublicense, and/or sell copies of the Software, and to permit
//	persons to whom the Software is furnished to do so, subject to the
//	following conditions:
//
//	The above copyright notice and this permission notice shall be included
//	in all copies or substantial portions of the Software.
//
//	THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS
//	OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
//	MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN
//	NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM,
//	DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR
//	OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE
//	USE OR OTHER DEALINGS IN THE SOFTWARE.

import "strings"

// The DEC ANSI parser's states and actions (parser/const.go).
const (
	psGround byte = iota
	psCsiEntry
	psCsiIntermediate
	psCsiParam
	psDcsEntry
	psDcsIntermediate
	psDcsParam
	psDcsString
	psEscape
	psEscapeIntermediate
	psOscString
	psSosString
	psPmString
	psApcString
	psUtf8
)

const (
	paNone byte = iota
	paClear
	paCollect
	paPrefix
	paDispatch
	paExecute
	paStart
	paPut
	paParam
	paPrint

	paIgnore = paNone
)

const (
	ptActionShift = 4
	ptStateMask   = 15
	ptIndexShift  = 8
	ptTableSize   = 4096
)

// stripTable is parser.Table: index state<<8|byte, value action<<4|next.
var stripTable = genStripTable()

type ptTable []byte

func (t ptTable) setDefault(action, state byte) {
	for i := range t {
		t[i] = action<<ptActionShift | state
	}
}

func (t ptTable) addOne(code, state, action, next byte) {
	t[int(state)<<ptIndexShift|int(code)] = action<<ptActionShift | next
}

func (t ptTable) addMany(codes []byte, state, action, next byte) {
	for _, code := range codes {
		t.addOne(code, state, action, next)
	}
}

func (t ptTable) addRange(start, end, state, action, next byte) {
	for i := int(start); i <= int(end); i++ {
		t.addOne(byte(i), state, action, next)
	}
}

func (t ptTable) transition(state, code byte) (byte, byte) {
	v := t[int(state)<<ptIndexShift|int(code)]
	return v & ptStateMask, v >> ptActionShift
}

// genStripTable is GenerateTransitionTable, statement for statement.
func genStripTable() ptTable {
	t := make(ptTable, ptTableSize)
	t.setDefault(paNone, psGround)

	// Anywhere
	for s := int(psGround); s <= int(psUtf8); s++ {
		state := byte(s)
		// Anywhere -> Ground
		t.addMany([]byte{0x18, 0x1a, 0x99, 0x9a}, state, paExecute, psGround)
		t.addRange(0x80, 0x8F, state, paExecute, psGround)
		t.addRange(0x90, 0x97, state, paExecute, psGround)
		t.addOne(0x9C, state, paExecute, psGround)
		// Anywhere -> Escape
		t.addOne(0x1B, state, paClear, psEscape)
		// Anywhere -> SosStringState
		t.addOne(0x98, state, paStart, psSosString)
		// Anywhere -> PmStringState
		t.addOne(0x9E, state, paStart, psPmString)
		// Anywhere -> ApcStringState
		t.addOne(0x9F, state, paStart, psApcString)
		// Anywhere -> CsiEntry
		t.addOne(0x9B, state, paClear, psCsiEntry)
		// Anywhere -> DcsEntry
		t.addOne(0x90, state, paClear, psDcsEntry)
		// Anywhere -> OscString
		t.addOne(0x9D, state, paStart, psOscString)
		// Anywhere -> Utf8
		t.addRange(0xC2, 0xDF, state, paCollect, psUtf8) // UTF8 2 byte sequence
		t.addRange(0xE0, 0xEF, state, paCollect, psUtf8) // UTF8 3 byte sequence
		t.addRange(0xF0, 0xF4, state, paCollect, psUtf8) // UTF8 4 byte sequence
	}

	// Ground
	t.addRange(0x00, 0x17, psGround, paExecute, psGround)
	t.addOne(0x19, psGround, paExecute, psGround)
	t.addRange(0x1C, 0x1F, psGround, paExecute, psGround)
	t.addRange(0x20, 0x7E, psGround, paPrint, psGround)
	t.addOne(0x7F, psGround, paExecute, psGround)

	// EscapeIntermediate
	t.addRange(0x00, 0x17, psEscapeIntermediate, paExecute, psEscapeIntermediate)
	t.addOne(0x19, psEscapeIntermediate, paExecute, psEscapeIntermediate)
	t.addRange(0x1C, 0x1F, psEscapeIntermediate, paExecute, psEscapeIntermediate)
	t.addRange(0x20, 0x2F, psEscapeIntermediate, paCollect, psEscapeIntermediate)
	t.addOne(0x7F, psEscapeIntermediate, paIgnore, psEscapeIntermediate)
	// EscapeIntermediate -> Ground
	t.addRange(0x30, 0x7E, psEscapeIntermediate, paDispatch, psGround)

	// Escape
	t.addRange(0x00, 0x17, psEscape, paExecute, psEscape)
	t.addOne(0x19, psEscape, paExecute, psEscape)
	t.addRange(0x1C, 0x1F, psEscape, paExecute, psEscape)
	t.addOne(0x7F, psEscape, paIgnore, psEscape)
	// Escape -> Ground
	t.addRange(0x30, 0x4F, psEscape, paDispatch, psGround)
	t.addRange(0x51, 0x57, psEscape, paDispatch, psGround)
	t.addOne(0x59, psEscape, paDispatch, psGround)
	t.addOne(0x5A, psEscape, paDispatch, psGround)
	t.addOne(0x5C, psEscape, paDispatch, psGround)
	t.addRange(0x60, 0x7E, psEscape, paDispatch, psGround)
	// Escape -> Escape_intermediate
	t.addRange(0x20, 0x2F, psEscape, paCollect, psEscapeIntermediate)
	// Escape -> Sos_pm_apc_string
	t.addOne('X', psEscape, paStart, psSosString) // SOS
	t.addOne('^', psEscape, paStart, psPmString)  // PM
	t.addOne('_', psEscape, paStart, psApcString) // APC
	// Escape -> Dcs_entry
	t.addOne('P', psEscape, paClear, psDcsEntry)
	// Escape -> Csi_entry
	t.addOne('[', psEscape, paClear, psCsiEntry)
	// Escape -> Osc_string
	t.addOne(']', psEscape, paStart, psOscString)

	// Sos_pm_apc_string
	for s := int(psSosString); s <= int(psApcString); s++ {
		state := byte(s)
		t.addRange(0x00, 0x17, state, paPut, state)
		t.addOne(0x19, state, paPut, state)
		t.addRange(0x1C, 0x1F, state, paPut, state)
		t.addRange(0x20, 0x7F, state, paPut, state)
		// ESC, ST, CAN, and SUB terminate the sequence
		t.addOne(0x1B, state, paDispatch, psEscape)
		t.addOne(0x9C, state, paDispatch, psGround)
		t.addMany([]byte{0x18, 0x1A}, state, paIgnore, psGround)
	}

	// Dcs_entry
	t.addRange(0x00, 0x07, psDcsEntry, paIgnore, psDcsEntry)
	t.addRange(0x0E, 0x17, psDcsEntry, paIgnore, psDcsEntry)
	t.addOne(0x19, psDcsEntry, paIgnore, psDcsEntry)
	t.addRange(0x1C, 0x1F, psDcsEntry, paIgnore, psDcsEntry)
	t.addOne(0x7F, psDcsEntry, paIgnore, psDcsEntry)
	// Dcs_entry -> Dcs_intermediate
	t.addRange(0x20, 0x2F, psDcsEntry, paCollect, psDcsIntermediate)
	// Dcs_entry -> Dcs_param
	t.addRange(0x30, 0x3B, psDcsEntry, paParam, psDcsParam)
	t.addRange(0x3C, 0x3F, psDcsEntry, paPrefix, psDcsParam)
	// Dcs_entry -> Dcs_passthrough
	t.addRange(0x08, 0x0D, psDcsEntry, paPut, psDcsString) // Follows ECMA-48 § 8.3.27
	// XXX: allows passing ESC (not a ECMA-48 standard) this to allow for
	// passthrough of ANSI sequences like in Screen or Tmux passthrough mode.
	t.addOne(0x1B, psDcsEntry, paPut, psDcsString)
	t.addRange(0x40, 0x7E, psDcsEntry, paStart, psDcsString)

	// Dcs_intermediate
	t.addRange(0x00, 0x17, psDcsIntermediate, paIgnore, psDcsIntermediate)
	t.addOne(0x19, psDcsIntermediate, paIgnore, psDcsIntermediate)
	t.addRange(0x1C, 0x1F, psDcsIntermediate, paIgnore, psDcsIntermediate)
	t.addRange(0x20, 0x2F, psDcsIntermediate, paCollect, psDcsIntermediate)
	t.addOne(0x7F, psDcsIntermediate, paIgnore, psDcsIntermediate)
	// Dcs_intermediate -> Dcs_passthrough
	t.addRange(0x30, 0x3F, psDcsIntermediate, paStart, psDcsString)
	t.addRange(0x40, 0x7E, psDcsIntermediate, paStart, psDcsString)

	// Dcs_param
	t.addRange(0x00, 0x17, psDcsParam, paIgnore, psDcsParam)
	t.addOne(0x19, psDcsParam, paIgnore, psDcsParam)
	t.addRange(0x1C, 0x1F, psDcsParam, paIgnore, psDcsParam)
	t.addRange(0x30, 0x3B, psDcsParam, paParam, psDcsParam)
	t.addOne(0x7F, psDcsParam, paIgnore, psDcsParam)
	t.addRange(0x3C, 0x3F, psDcsParam, paIgnore, psDcsParam)
	// Dcs_param -> Dcs_intermediate
	t.addRange(0x20, 0x2F, psDcsParam, paCollect, psDcsIntermediate)
	// Dcs_param -> Dcs_passthrough
	t.addRange(0x40, 0x7E, psDcsParam, paStart, psDcsString)

	// Dcs_passthrough
	t.addRange(0x00, 0x17, psDcsString, paPut, psDcsString)
	t.addOne(0x19, psDcsString, paPut, psDcsString)
	t.addRange(0x1C, 0x1F, psDcsString, paPut, psDcsString)
	t.addRange(0x20, 0x7E, psDcsString, paPut, psDcsString)
	t.addOne(0x7F, psDcsString, paPut, psDcsString)
	t.addRange(0x80, 0xFF, psDcsString, paPut, psDcsString) // Allow Utf8 characters by extending the printable range from 0x7F to 0xFF
	// ST, CAN, SUB, and ESC terminate the sequence
	t.addOne(0x1B, psDcsString, paDispatch, psEscape)
	t.addOne(0x9C, psDcsString, paDispatch, psGround)
	t.addMany([]byte{0x18, 0x1A}, psDcsString, paIgnore, psGround)

	// Csi_param
	t.addRange(0x00, 0x17, psCsiParam, paExecute, psCsiParam)
	t.addOne(0x19, psCsiParam, paExecute, psCsiParam)
	t.addRange(0x1C, 0x1F, psCsiParam, paExecute, psCsiParam)
	t.addRange(0x30, 0x3B, psCsiParam, paParam, psCsiParam)
	t.addOne(0x7F, psCsiParam, paIgnore, psCsiParam)
	t.addRange(0x3C, 0x3F, psCsiParam, paIgnore, psCsiParam)
	// Csi_param -> Ground
	t.addRange(0x40, 0x7E, psCsiParam, paDispatch, psGround)
	// Csi_param -> Csi_intermediate
	t.addRange(0x20, 0x2F, psCsiParam, paCollect, psCsiIntermediate)

	// Csi_intermediate
	t.addRange(0x00, 0x17, psCsiIntermediate, paExecute, psCsiIntermediate)
	t.addOne(0x19, psCsiIntermediate, paExecute, psCsiIntermediate)
	t.addRange(0x1C, 0x1F, psCsiIntermediate, paExecute, psCsiIntermediate)
	t.addRange(0x20, 0x2F, psCsiIntermediate, paCollect, psCsiIntermediate)
	t.addOne(0x7F, psCsiIntermediate, paIgnore, psCsiIntermediate)
	// Csi_intermediate -> Ground
	t.addRange(0x40, 0x7E, psCsiIntermediate, paDispatch, psGround)
	// Csi_intermediate -> Csi_ignore
	t.addRange(0x30, 0x3F, psCsiIntermediate, paIgnore, psGround)

	// Csi_entry
	t.addRange(0x00, 0x17, psCsiEntry, paExecute, psCsiEntry)
	t.addOne(0x19, psCsiEntry, paExecute, psCsiEntry)
	t.addRange(0x1C, 0x1F, psCsiEntry, paExecute, psCsiEntry)
	t.addOne(0x7F, psCsiEntry, paIgnore, psCsiEntry)
	// Csi_entry -> Ground
	t.addRange(0x40, 0x7E, psCsiEntry, paDispatch, psGround)
	// Csi_entry -> Csi_intermediate
	t.addRange(0x20, 0x2F, psCsiEntry, paCollect, psCsiIntermediate)
	// Csi_entry -> Csi_param
	t.addRange(0x30, 0x3B, psCsiEntry, paParam, psCsiParam)
	t.addRange(0x3C, 0x3F, psCsiEntry, paPrefix, psCsiParam)

	// Osc_string
	t.addRange(0x00, 0x06, psOscString, paIgnore, psOscString)
	t.addRange(0x08, 0x17, psOscString, paIgnore, psOscString)
	t.addOne(0x19, psOscString, paIgnore, psOscString)
	t.addRange(0x1C, 0x1F, psOscString, paIgnore, psOscString)
	t.addRange(0x20, 0xFF, psOscString, paPut, psOscString) // Allow Utf8 characters by extending the printable range from 0x7F to 0xFF

	// ST, CAN, SUB, ESC, and BEL terminate the sequence
	t.addOne(0x1B, psOscString, paDispatch, psEscape)
	t.addOne(0x07, psOscString, paDispatch, psGround)
	t.addOne(0x9C, psOscString, paDispatch, psGround)
	t.addMany([]byte{0x18, 0x1A}, psOscString, paIgnore, psGround)

	return t
}

// utf8ByteLen is parser.go's: the length a UTF-8 sequence's first byte
// announces, -1 for a byte that starts none.
func utf8ByteLen(b byte) int {
	switch {
	case b <= 0b0111_1111: // 0x00-0x7F
		return 1
	case b >= 0b1100_0000 && b <= 0b1101_1111: // 0xC0-0xDF
		return 2
	case b >= 0b1110_0000 && b <= 0b1110_1111: // 0xE0-0xEF
		return 3
	case b >= 0b1111_0000 && b <= 0b1111_0111: // 0xF0-0xF7
		return 4
	}
	return -1
}

// stripANSI is ansi.Strip: it removes ANSI escape codes, keeping printable
// characters and the executed controls (which dropControls deals with next).
func stripANSI(s string) string {
	var (
		buf    strings.Builder // collects printable characters
		ri     int             // rune index
		rw     int             // rune width
		pstate = psGround      // initial state
	)
	// This implements a subset of the Parser to only collect runes and
	// printable characters.
	for i := range len(s) {
		if pstate == psUtf8 {
			// During this state, collect rw bytes to form a valid rune in the
			// buffer. After getting all the rune bytes into the buffer,
			// transition to GroundState and reset the counters.
			buf.WriteByte(s[i])
			ri++
			if ri < rw {
				continue
			}
			pstate = psGround
			ri = 0
			rw = 0
			continue
		}

		state, action := stripTable.transition(pstate, s[i])
		switch action {
		case paCollect:
			if state == psUtf8 {
				// This action happens when we transition to the Utf8State.
				rw = utf8ByteLen(s[i])
				buf.WriteByte(s[i])
				ri++
			}
		case paPrint, paExecute:
			// collects printable ASCII and non-printable characters
			buf.WriteByte(s[i])
		}

		// Transition to the next state.
		// The Utf8State is managed separately above.
		if pstate != psUtf8 {
			pstate = state
		}
	}

	return buf.String()
}
