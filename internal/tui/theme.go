package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type Theme struct {
	Name string

	BG, FG, Dim lipgloss.Color
	Border      lipgloss.Color
	Title       lipgloss.Color

	User, Assistant, Tool lipgloss.Color

	FooterBG, FooterFG, FooterKey lipgloss.Color
	OK, Warn, Err                 lipgloss.Color
}

func Preset(name string) Theme {
	switch normalizeThemeName(name) {
	case "dark":
		return darkTheme()
	case "light":
		return lightTheme()
	default:
		return tokyoNightTheme()
	}
}

func KnownPreset(name string) bool {
	switch normalizeThemeName(name) {
	case "tokyo-night", "tokyonight", "tokyo", "dark", "light":
		return true
	default:
		return false
	}
}

func normalizeThemeName(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
}

func rgb(r, g, b uint8) lipgloss.Color {
	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", r, g, b))
}

func tokyoNightTheme() Theme {
	return Theme{
		Name:      "tokyo-night",
		BG:        rgb(26, 27, 38),
		FG:        rgb(169, 177, 214),
		Dim:       rgb(86, 95, 137),
		Border:    rgb(41, 46, 66),
		Title:     rgb(122, 162, 247),
		User:      rgb(125, 207, 255),
		Assistant: rgb(169, 177, 214),
		Tool:      rgb(187, 154, 247),
		FooterBG:  rgb(22, 22, 30),
		FooterFG:  rgb(192, 202, 245),
		FooterKey: rgb(122, 162, 247),
		OK:        rgb(158, 206, 106),
		Warn:      rgb(230, 194, 72),
		Err:       rgb(247, 118, 142),
	}
}

func darkTheme() Theme {
	return Theme{
		Name:      "dark",
		BG:        rgb(28, 28, 28),
		FG:        rgb(208, 208, 208),
		Dim:       rgb(128, 128, 128),
		Border:    rgb(58, 58, 58),
		Title:     rgb(95, 135, 215),
		User:      rgb(95, 175, 215),
		Assistant: rgb(208, 208, 208),
		Tool:      rgb(175, 135, 215),
		FooterBG:  rgb(18, 18, 18),
		FooterFG:  rgb(228, 228, 228),
		FooterKey: rgb(95, 135, 215),
		OK:        rgb(135, 175, 95),
		Warn:      rgb(230, 200, 80),
		Err:       rgb(215, 95, 95),
	}
}

func lightTheme() Theme {
	return Theme{
		Name:      "light",
		BG:        rgb(250, 250, 250),
		FG:        rgb(56, 58, 66),
		Dim:       rgb(160, 161, 167),
		Border:    rgb(212, 212, 212),
		Title:     rgb(64, 120, 242),
		User:      rgb(1, 132, 188),
		Assistant: rgb(56, 58, 66),
		Tool:      rgb(166, 38, 164),
		FooterBG:  rgb(234, 234, 235),
		FooterFG:  rgb(56, 58, 66),
		FooterKey: rgb(64, 120, 242),
		OK:        rgb(80, 161, 79),
		Warn:      rgb(200, 150, 20),
		Err:       rgb(228, 86, 73),
	}
}
