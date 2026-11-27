// Package theme defines Craze's semantic palette and its terminal colour depths.
package theme

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
)

// Tone provides one colour at each terminal colour depth.
type Tone struct {
	TrueColor, ANSI256, ANSI string
}

// Default is the preset craze falls back to when neither --theme nor the
// config file names one.
const Default = "craze-dark"

// diffBGMix is how much of the add/delete colour is mixed into the background
// for a diff row: enough to tint it, not enough to fight the foreground.
const diffBGMix = 10

// selectionMix is how much accent is mixed into the background for a selected
// row: enough to read as "this one", not enough to hide the text on it.
const selectionMix = 25

// shellMix distinguishes the shell-mode composer border.
const shellMix = 55

// paletteSpec defines the preset's RGB colors and its sixteen-color roles.
// The default foreground uses the terminal's own foreground (empty ANSI slot).
// Rule uses Dim's ANSI slot. At 256 colors we choose the nearest fixed index.
type paletteSpec struct {
	name                                                                   string
	bg, fg, dim, bright, border, accent, teal, ok, warn, err, purple, rule string
	ansi                                                                   [11]string
}

// palettes is the pinned preset table, in the order the picker lists them.
// The first entry is the default.
var palettes = []paletteSpec{
	{"craze-dark", "#141419", "#dedee8", "#9b9bab", "#e2e2ea", "#24242f", "#efb766", "#3fc8c8", "#8ccc94", "#f2cc79", "#ff8792", "#bb9af7", "#525261", [11]string{"0", "", "8", "15", "8", "11", "14", "10", "11", "9", "13"}},
	{"craze-light", "#f7f5ef", "#292834", "#656273", "#101018", "#d8d8d0", "#8d550b", "#137c7c", "#356b40", "#9e5d13", "#a63345", "#7a4fbf", "#92909a", [11]string{"15", "", "8", "0", "7", "3", "6", "2", "3", "1", "5"}},
	{"tokyo-night", "#1a1b26", "#b9c1e1", "#909bc2", "#c0caf5", "#292e42", "#8eafff", "#7dcfff", "#a9d97c", "#efd178", "#ff8aa1", "#bb9af7", "#4b526e", [11]string{"0", "", "8", "15", "8", "12", "14", "10", "11", "9", "13"}},
	{"dark", "#1c1c1c", "#d9d9d9", "#a1a1a1", "#e4e4e4", "#3a3a3a", "#86aaf2", "#5fafd7", "#a6cc86", "#ebcc66", "#f08080", "#af87d7", "#555555", [11]string{"0", "", "8", "15", "8", "12", "14", "10", "11", "9", "13"}},
	{"light", "#fafafa", "#30333c", "#626776", "#101010", "#d4d4d4", "#2757b8", "#017aad", "#376e3d", "#805400", "#ae3836", "#a626a4", "#90949c", [11]string{"15", "", "8", "0", "7", "4", "6", "2", "3", "1", "5"}},
	{"catppuccin", "#1e1e2e", "#cdd6f4", "#a0a7c2", "#f5f5ff", "#313244", "#89b4fa", "#89dceb", "#a6e3a1", "#f9e2af", "#f38ba8", "#cba6f7", "#585b70", [11]string{"0", "", "8", "15", "8", "12", "14", "10", "11", "9", "13"}},
	{"gruvbox", "#282828", "#ebdbb2", "#b6a28d", "#fbf1c7", "#3c3836", "#fabd2f", "#83a598", "#c3c94c", "#efc675", "#ff766b", "#d3869b", "#665b50", [11]string{"0", "", "8", "15", "8", "11", "14", "10", "11", "9", "13"}},
	{"high-contrast", "#101014", "#f4f4fb", "#c0c0d2", "#ffffff", "#696979", "#ffdc92", "#9de9e9", "#a4eab2", "#ffec94", "#ffabb6", "#d8b4ff", "#9595ab", [11]string{"0", "", "8", "15", "8", "11", "14", "10", "11", "9", "13"}},
}

// themeAliases are the spellings that are not a preset name but mean one.
var themeAliases = map[string]string{
	"tokyonight": "tokyo-night",
	"tokyo":      "tokyo-night",
	"craze":      "craze-dark",
	"crazedark":  "craze-dark",
	"crazelight": "craze-light",
}

// Theme names the colors used for content, status, focus and terminal chrome.
// C is the color representation of the consuming renderer.
type Theme[C any] struct {
	Name string

	BG, FG, Dim, Bright, Border         C
	Accent, Teal, OK, Warn, Err, Purple C

	// UserMark colours the ❯ / ↳ glyph that opens a user row; User is the bold
	// prompt text beside it. Heading colours a markdown heading (h1-h6 draw
	// identically), kept apart from Accent so a heading and inline code in
	// the same reply are not the same colour.
	User, UserMark, Assistant, Thought, ToolKind, Heading C
	DiffAdd, DiffDel                                      C
	DiffAddBG, DiffDelBG                                  C
	LineNo, TaskRail, Selection, Rule                     C
	// Shell is the composer's two rules while the draft is a shell command, and
	// the `!` that opens the transcript row it produces — one colour for "this
	// is your shell, not the agent", the way UserMark means "you, here". It is
	// derived rather than hand-picked so no preset row has to change for it.
	Shell C
	// SelectionBG is the background the dialog cursor row (and V4's mouse
	// selection) paints with; Selection stays a foreground slot. At 16
	// colours the band is reverse video instead (selectionBand).
	SelectionBG                      C
	ChipBypass, ChipPrompt, Provider C
	// One colour per mode kind, not per mode id: the chip has to stay
	// readable for an agent that spells plan mode "architect".
	ModeImplement, ModePlan, ModeReadOnly C
}

// resolve expands the palette into semantic roles in the caller’s color representation.
func resolve[C any](p paletteSpec, color func(Tone) C) Theme[C] {
	paint := func(hex, ansi string) C { return color(tone(hex, ansi)) }
	a := p.ansi
	th := Theme[C]{
		Name:   p.name,
		BG:     paint(p.bg, a[0]),
		FG:     paint(p.fg, a[1]),
		Dim:    paint(p.dim, a[2]),
		Bright: paint(p.bright, a[3]),
		Border: paint(p.border, a[4]),
		Accent: paint(p.accent, a[5]),
		Teal:   paint(p.teal, a[6]),
		OK:     paint(p.ok, a[7]),
		Warn:   paint(p.warn, a[8]),
		Err:    paint(p.err, a[9]),
		Purple: paint(p.purple, a[10]),
	}
	// User was Bright, which vanished into body text. Teal makes it read as
	// "you" without competing for the loudest colour on screen. UserMark on
	// the ❯ derives from Accent, which is what the composer's own prompt
	// glyph and the row gutter mark paint with, so today one colour means
	// "you, here" across all three.
	th.User = th.Teal
	th.UserMark = th.Accent
	th.Assistant = th.FG
	th.Thought = th.Dim
	th.ToolKind = th.FG
	// Heading was Accent, which is also inline code, the spinner and chips,
	// so a heading and a code word in the same reply were the same amber. OK
	// is not otherwise used in running prose, so it reads as its own thing.
	th.Heading = th.OK
	th.DiffAdd = th.OK
	th.DiffDel = th.Err
	// A blended slot has no hand-picked 16-colour index of its own, so it
	// takes its parent's: a tint is no colour at 16, the band is reverse
	// video there and takes Border's, the rule reads as Dim does (Border's 7
	// would vanish on a light background), and the shell rule as Purple.
	th.DiffAddBG = paint(blend(p.bg, p.ok, diffBGMix), "")
	th.DiffDelBG = paint(blend(p.bg, p.err, diffBGMix), "")
	th.LineNo = th.Dim
	th.TaskRail = th.Accent
	th.Selection = th.Accent
	th.SelectionBG = paint(blend(p.bg, p.accent, selectionMix), a[4])
	th.Rule = paint(p.rule, a[2])
	// Shell is the same rule leaning on Purple instead of FG. Purple is spoken
	// for only by the plan chip, which is a two-word chip on the status row and
	// never on screen beside the composer's rules, so the shell band is the one
	// purple thing the eye finds.
	th.Shell = paint(blend(p.border, p.purple, shellMix), a[10])
	th.ChipBypass = th.Err
	th.ChipPrompt = th.Warn
	th.Provider = th.Teal
	th.ModeImplement = th.Accent
	th.ModePlan = th.Purple
	th.ModeReadOnly = th.Teal
	return th
}

// Preset is the theme by name, falling back to the default for anything craze
// does not know.
func Preset[C any](name string, color func(Tone) C) Theme[C] {
	return resolve(palette(name), color)
}

// TerminalColors returns the preset’s canonical RGB defaults for OSC 10 and 11.
func TerminalColors(name string) (foreground, background string) {
	p := palette(name)
	return p.fg, p.bg
}

func palette(name string) paletteSpec {
	want := normalizeThemeName(name)
	for _, p := range palettes {
		if p.name == want {
			return p
		}
	}
	return palettes[0]
}

func KnownPreset(name string) bool {
	want := normalizeThemeName(name)
	for _, p := range palettes {
		if p.name == want {
			return true
		}
	}
	return false
}

// ThemeNames lists the presets in table order.
func ThemeNames() []string {
	out := make([]string, 0, len(palettes))
	for _, p := range palettes {
		out = append(out, p.name)
	}
	return out
}

func normalizeThemeName(name string) string {
	n := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
	if alias, ok := themeAliases[strings.ReplaceAll(n, "-", "")]; ok {
		return alias
	}
	return n
}

// rgb is the "#rrggbb" hex of one colour.
func rgb(r, g, b uint8) string {
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

// blend mixes pct percent of fg into bg. It is how the tinted diff backgrounds
// stay on the right side of a light or a dark palette without a second table.
func blend(bg, fg string, pct int) string {
	br, bgr, bb := hexRGB(bg)
	fr, fg2, fb := hexRGB(fg)
	mix := func(a, b uint8) uint8 {
		return uint8((int(a)*(100-pct) + int(b)*pct + 50) / 100)
	}
	return rgb(mix(br, fr), mix(bgr, fg2), mix(bb, fb))
}

// hexRGB parses "#rrggbb"; anything else is black, which only a malformed
// table could produce.
func hexRGB(s string) (r, g, b uint8) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return 0, 0, 0
	}
	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, 0, 0
	}
	return uint8(n >> 16), uint8(n >> 8), uint8(n)
}

// tone is one theme colour at every depth: the hex itself at true colour,
// craze's own nearest256 at 256 colours, and the preset's hand-picked index
// at 16 ("" is the terminal's default foreground).
//
// craze converts rather than leaving 256 colours to termenv, whose
// conversion is SF-142: its grey candidate averages the cube indices rather
// than the channels, so it is always 232, and it picks between cube and grey
// by an HSLuv distance that does not wrap hue, so craze-dark's Err (#e06c75)
// painted 232, near-black on a near-black background.
func tone(hex, ansi16 string) Tone {
	return Tone{TrueColor: hex, ANSI256: strconv.Itoa(nearest256(hex)), ANSI: ansi16}
}

// nearest256 is the xterm-256 index nearest hex by plain Euclidean distance in
// OKLab, over 16..255 only: 0..15 are the user's own palette, not fixed
// colours, so no hex can be matched against them.
func nearest256(hex string) int {
	l, a, b := oklab(hexRGB(hex))
	best, bd := 16, math.MaxFloat64
	for i, c := range xtermLab() {
		if d := (l-c[0])*(l-c[0]) + (a-c[1])*(a-c[1]) + (b-c[2])*(b-c[2]); d < bd {
			best, bd = 16+i, d
		}
	}
	return best
}

// xtermLab is the OKLab coordinates of xterm-256's fixed entries 16..255,
// computed once: every theme build converts every slot, and tests build
// themes thousands of times.
var xtermLab = sync.OnceValue(func() [][3]float64 {
	out := make([][3]float64, 0, 240)
	for i := 16; i < 256; i++ {
		l, a, b := oklab(xterm256(i))
		out = append(out, [3]float64{l, a, b})
	}
	return out
})

// xterm256 is the RGB of a fixed xterm-256 entry: 16..231 the 6×6×6 cube,
// 232..255 the grey ramp.
func xterm256(i int) (r, g, b uint8) {
	if i >= 232 {
		v := uint8(8 + 10*(i-232))
		return v, v, v
	}
	lv := [6]uint8{0, 0x5f, 0x87, 0xaf, 0xd7, 0xff}
	i -= 16
	return lv[i/36], lv[(i/6)%6], lv[i%6]
}

// oklab converts an sRGB colour to OKLab (Björn Ottosson's matrices).
func oklab(r, g, b uint8) (l, a, bb float64) {
	lin := func(v uint8) float64 {
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	lr, lg, lb := lin(r), lin(g), lin(b)
	lc := math.Cbrt(0.4122214708*lr + 0.5363325363*lg + 0.0514459929*lb)
	mc := math.Cbrt(0.2119034982*lr + 0.6806995451*lg + 0.1073969566*lb)
	sc := math.Cbrt(0.0883024619*lr + 0.2817188376*lg + 0.6299787005*lb)
	return 0.2104542553*lc + 0.7936177850*mc - 0.0040720468*sc,
		1.9779984951*lc - 2.4285922050*mc + 0.4505937099*sc,
		0.0259040371*lc + 0.7827717662*mc - 0.8086757660*sc
}
