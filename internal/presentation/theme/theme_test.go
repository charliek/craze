package theme

import "testing"

// TestNearest256 pins craze's own 256-colour conversion: an exact cube or grey
// entry maps to itself, nothing maps into 0..15 (the user's palette), and
// craze-dark's Err, which termenv painted 232 (SF-142), is 167.
func TestNearest256(t *testing.T) {
	for _, tc := range []struct {
		hex  string
		want int
	}{
		{"#e06c75", 167},
		{"#d75f87", 168},
		{"#5f5f5f", 59},
		{"#080808", 232},
		{"#eeeeee", 255},
		{"#000000", 16},
		{"#ffffff", 231},
		{"#ff0000", 196},
	} {
		if got := nearest256(tc.hex); got != tc.want {
			t.Errorf("nearest256(%s) = %d, want %d", tc.hex, got, tc.want)
		}
	}
}

func TestDerivedColors(t *testing.T) {
	if got := blend("#000000", "#ffffff", 10); got != "#1a1a1a" {
		t.Fatalf("10%% blend = %q", got)
	}
	for _, p := range palettes {
		th := Preset(p.name, func(t Tone) Tone { return t })
		if got, want := th.Shell.TrueColor, blend(p.border, p.purple, shellMix); got != want {
			t.Errorf("%s: shell color = %q, want %q", p.name, got, want)
		}
	}
}
