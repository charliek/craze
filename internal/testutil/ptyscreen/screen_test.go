package ptyscreen

import "testing"

func TestUTF8TitlesOccupyNoCells(t *testing.T) {
	// Include a title on either side of visible UTF-8 text, both string
	// terminators, and a CSI cursor move in the ordinary output.
	data := []byte("echo: \x1b]2;✦ craze\a猫!\x1b]2;✦ next\x1b\\\x1b[1D?")
	for split := 1; split < len(data); split++ {
		s := &Screen{cols: 20, rows: 1}
		s.Feed(t, data[:split])
		s.Feed(t, data[split:])
		if got := s.String(); got != "echo: 猫?" {
			t.Fatalf("split at byte %d: screen %q", split, got)
		}
	}
}

func TestSplitGraphemeKeepsItsCells(t *testing.T) {
	for _, cluster := range []string{"👩‍💻", "1️⃣", "é"} {
		data := []byte("x" + cluster + "z\x1b[1D?")
		want := "x" + cluster + "?"
		for split := 1; split < len(data); split++ {
			s := &Screen{cols: 20, rows: 1}
			s.Feed(t, data[:split])
			s.Feed(t, data[split:])
			if got := s.String(); got != want {
				t.Fatalf("%s split at byte %d: got %q, want %q", cluster, split, got, want)
			}
		}
	}
}

func TestPartialTitleNeverReachesTheScreen(t *testing.T) {
	s := &Screen{cols: 20, rows: 1}
	s.Feed(t, []byte("echo: \x1b]2;✦ craze"))
	if got := s.String(); got != "echo:" {
		t.Fatalf("incomplete title: %q", got)
	}
}
